"""Execution engine for validated Herdr workflows."""

from __future__ import annotations

import hashlib
import json
import os
import re
import shlex
import stat
import subprocess
import sys
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from panopticon.herdr import (
    CommandError,
    HerdrClient,
    HerdrError,
    extract_error_code,
    extract_id,
    extract_path,
    extract_status,
    parse_json_output,
    require_herdr_env,
)
from panopticon.state import (
    RunStore,
    StateError,
    atomic_write_json,
    make_run_id,
    read_json,
    utc_now,
)
from panopticon.workflow import StepSpec, Workflow, load_workflow


class FlowError(RuntimeError):
    """Raised for an orchestration error that should be shown to the user."""


RUN_TERMINAL = {"completed", "failed", "cancelled"}
STEP_SUCCESS = {"completed", "skipped"}
STEP_TERMINAL = STEP_SUCCESS | {"failed", "blocked", "timed_out", "cancelled"}
MAX_VERIFICATION_OUTPUT = 4000
WORKING_WAIT_MS = 5_000
_HERDR_AGENT_START_MIN_TIMEOUT_MS = 3_001
_HERDR_AGENT_START_MAX_TIMEOUT_MS = 300_000


@dataclass(frozen=True)
class StartOptions:
    repo: Path
    workflow: Workflow
    task: str
    verify_commands: tuple[tuple[str, ...], ...]
    use_worktree: bool = True
    worktree_path: Path | None = None
    branch: str | None = None
    base: str | None = None
    background: bool = True
    script_path: Path | None = None


def _display_command(command: tuple[str, ...]) -> str:
    return shlex.join(list(command))


def _clamp_agent_start_timeout_ms(timeout_ms: int) -> int:
    return min(
        max(timeout_ms, _HERDR_AGENT_START_MIN_TIMEOUT_MS),
        _HERDR_AGENT_START_MAX_TIMEOUT_MS,
    )


def _now_epoch(value: str | None) -> float:
    if not value:
        return 0.0
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    except ValueError:
        return 0.0


def _is_within(path: Path, roots: tuple[Path, ...]) -> bool:
    candidate = path.resolve()
    return any(candidate == root or root in candidate.parents for root in roots)


def _read_symlink_target(path: Path) -> str:
    try:
        return os.readlink(path)
    except OSError as exc:
        raise FlowError(f"worktree snapshot を取得できません: {path}: {exc}") from exc


def _read_file_digest(path: Path, info: os.stat_result) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as handle:
            for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                digest.update(chunk)
        after = path.stat(follow_symlinks=False)
    except OSError as exc:
        raise FlowError(f"worktree snapshot を取得できません: {path}: {exc}") from exc
    if (
        after.st_dev != info.st_dev
        or after.st_ino != info.st_ino
        or after.st_size != info.st_size
        or after.st_mtime_ns != info.st_mtime_ns
        or stat.S_IMODE(after.st_mode) != stat.S_IMODE(info.st_mode)
    ):
        raise FlowError(f"worktree snapshot 中にファイルが変更されました: {path}")
    return digest.hexdigest()


def _snapshot_filesystem(
    root: Path, excluded_paths: tuple[Path, ...]
) -> dict[str, str]:
    """Capture a non-Git worktree without walking excluded paths."""

    snapshot: dict[str, str] = {}

    def is_excluded(path: Path) -> bool:
        return any(
            path == excluded_path or excluded_path in path.parents
            for excluded_path in excluded_paths
        )

    def visit(directory: Path) -> None:
        try:
            entries = sorted(os.scandir(directory), key=lambda entry: entry.name)
        except OSError as exc:
            raise FlowError(
                f"worktree snapshot を取得できません: {directory}: {exc}"
            ) from exc
        for entry in entries:
            path = Path(entry.path)
            if entry.name == ".git" or is_excluded(path):
                continue
            relative = path.relative_to(root).as_posix()
            try:
                info = entry.stat(follow_symlinks=False)
            except OSError as exc:
                raise FlowError(
                    f"worktree snapshot を取得できません: {path}: {exc}"
                ) from exc
            mode = stat.S_IMODE(info.st_mode)
            if stat.S_ISLNK(info.st_mode):
                target = _read_symlink_target(path)
                snapshot[relative] = f"symlink:{mode:o}:{target}"
            elif stat.S_ISDIR(info.st_mode):
                visit(path)
            elif stat.S_ISREG(info.st_mode):
                digest = _read_file_digest(path, info)
                snapshot[relative] = f"file:{mode:o}:{digest}"
            else:
                snapshot[relative] = (
                    f"special:{info.st_mode:o}:{info.st_size}:{info.st_mtime_ns}"
                )

    visit(root)
    return snapshot


def _has_git_metadata(root: Path) -> bool:
    current = root
    while True:
        marker = current / ".git"
        if marker.is_dir() or marker.is_file():
            return True
        if current.parent == current:
            return False
        current = current.parent


def _is_non_git_status_failure(
    root: Path, completed: subprocess.CompletedProcess[bytes]
) -> bool:
    if _has_git_metadata(root):
        return False
    stderr = completed.stderr
    if isinstance(stderr, str):
        stderr_bytes = os.fsencode(stderr)
    elif isinstance(stderr, bytes):
        stderr_bytes = stderr
    else:
        return False
    return b"not a git repository" in stderr_bytes.lower()


def _status_path(root: Path, raw_path: bytes) -> tuple[str, Path]:
    decoded = os.fsdecode(raw_path)
    relative_path = Path(decoded)
    if (
        not decoded
        or relative_path.is_absolute()
        or not relative_path.parts
        or any(part == ".." for part in relative_path.parts)
    ):
        raise FlowError(
            f"git status の path が worktree 相対ではありません: {decoded!r}"
        )
    path = root.joinpath(*relative_path.parts)
    try:
        path.relative_to(root)
    except ValueError as exc:
        raise FlowError(
            f"git status の path が worktree の外側です: {decoded!r}"
        ) from exc
    return path.relative_to(root).as_posix(), path


def _parse_git_status(
    output: bytes,
) -> list[tuple[str, bytes, bytes | None]]:
    if not output:
        return []
    if not output.endswith(b"\0"):
        raise FlowError("git status の NUL 終端が不正です")
    fields = output[:-1].split(b"\0")
    entries: list[tuple[str, bytes, bytes | None]] = []
    index = 0
    valid_statuses = frozenset(b" MARD?CUT!")
    while index < len(fields):
        record = fields[index]
        if len(record) < 4 or record[2:3] != b" ":
            raise FlowError("git status の status/path record が不正です")
        status_bytes = record[:2]
        if any(value not in valid_statuses for value in status_bytes):
            raise FlowError("git status の status code が不正です")
        path = record[3:]
        if not path:
            raise FlowError("git status の path が空です")
        status = status_bytes.decode("ascii")
        original: bytes | None = None
        if b"R" in status_bytes or b"C" in status_bytes:
            index += 1
            if index >= len(fields) or not fields[index]:
                raise FlowError("git status の rename/copy 元 path がありません")
            original = fields[index]
        if status == "!!":
            index += 1
            continue
        entries.append((status, path, original))
        index += 1
    return entries


def _git_snapshot_path(
    path: Path,
    status: str,
    original: str | None,
    *,
    allow_missing: bool = False,
) -> str:
    origin = f":original:{original}" if original is not None else ""
    try:
        info = path.stat(follow_symlinks=False)
    except FileNotFoundError:
        if allow_missing:
            return f"git:{status}:deleted{origin}"
        if "D" in status:
            return f"git:{status}:deleted{origin}"
        raise FlowError(
            f"git status に列挙された path が snapshot 前に消えました: {path}"
        ) from None
    except OSError as exc:
        raise FlowError(f"worktree snapshot を取得できません: {path}: {exc}") from exc

    mode = stat.S_IMODE(info.st_mode)
    if stat.S_ISLNK(info.st_mode):
        target = _read_symlink_target(path)
        return f"git:{status}:symlink:{mode:o}:{target}{origin}"
    if stat.S_ISDIR(info.st_mode):
        raise FlowError(
            f"git status に列挙された path が directory のため snapshot できません: {path}"
        )
    if stat.S_ISREG(info.st_mode):
        digest = _read_file_digest(path, info)
        return f"git:{status}:file:{mode:o}:{digest}{origin}"
    return (
        f"git:{status}:special:{info.st_mode:o}:{info.st_size}:"
        f"{info.st_mtime_ns}{origin}"
    )


def _snapshot_worktree(
    worktree: Path,
    *,
    excluded: tuple[Path, ...] = (),
    allow_filesystem_fallback: bool = False,
) -> dict[str, str]:
    """Capture only Git-dirty paths, or a non-Git no-worktree fallback."""

    root = Path(worktree).resolve()
    if not root.is_dir():
        raise FlowError(f"worktree が directory ではありません: {root}")
    excluded_paths = tuple(path.resolve() for path in excluded)
    command = [
        "git",
        "status",
        "--porcelain=v1",
        "-z",
        "--untracked-files=all",
    ]
    environment = os.environ.copy()
    environment.update({"LC_ALL": "C", "LANG": "C", "LANGUAGE": "C"})
    try:
        completed = subprocess.run(
            command,
            cwd=root,
            env=environment,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=False,
            check=False,
            shell=False,
        )
    except OSError as exc:
        raise FlowError(f"git status を実行できません: {root}: {exc}") from exc
    if completed.returncode != 0:
        if allow_filesystem_fallback and _is_non_git_status_failure(root, completed):
            return _snapshot_filesystem(root, excluded_paths)
        stderr = completed.stderr
        detail = (
            os.fsdecode(stderr) if isinstance(stderr, bytes) else str(stderr or "")
        ).strip()
        suffix = f": {detail}" if detail else ""
        raise FlowError(
            f"git status が失敗しました (returncode={completed.returncode}){suffix}"
        )
    if not isinstance(completed.stdout, bytes):
        raise FlowError("git status の stdout が bytes ではありません")

    snapshot: dict[str, str] = {}
    for status, raw_path, raw_original in _parse_git_status(completed.stdout):
        relative, path = _status_path(root, raw_path)
        original: str | None = None
        original_path: Path | None = None
        if raw_original is not None:
            original, original_path = _status_path(root, raw_original)
            if any(
                original_path == excluded_path or excluded_path in original_path.parents
                for excluded_path in excluded_paths
            ):
                continue
        if any(
            path == excluded_path or excluded_path in path.parents
            for excluded_path in excluded_paths
        ):
            continue
        if relative in snapshot:
            raise FlowError(f"git status に同じ path が重複しています: {relative}")
        snapshot[relative] = _git_snapshot_path(path, status, original)
        if "R" in status:
            if original is None or original_path is None:
                raise FlowError("git status の rename 元 path がありません")
            if original in snapshot:
                raise FlowError(f"git status に同じ path が重複しています: {original}")
            snapshot[original] = _git_snapshot_path(
                original_path, status, None, allow_missing=True
            )
    return snapshot


def _snapshot_diff(before: dict[str, str], after: dict[str, str]) -> list[str]:
    return sorted(
        path for path in set(before) | set(after) if before.get(path) != after.get(path)
    )


def _bounded_text(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, bytes):
        value = value.decode("utf-8", errors="replace")
    return str(value)[-MAX_VERIFICATION_OUTPUT:]


def _current_worktree_workspace_id(
    payload: Any, repo: Path, saved_path: Path, saved_branch: str
) -> str:
    """Find one live workspace by the saved worktree path and branch."""

    path_keys = {
        "path",
        "worktree_path",
        "worktreepath",
        "checkout_path",
        "checkoutpath",
        "working_directory",
        "workingdirectory",
    }
    branch_keys = {"branch", "branch_name", "branchname"}
    workspace_keys = {"workspace", "workspace_id", "workspaceid"}
    matches: list[str | None] = []

    def direct_value(node: dict[str, Any], keys: set[str]) -> str | None:
        for key, value in node.items():
            normalised = str(key).replace("-", "_").lower()
            if normalised in keys and isinstance(value, str) and value.strip():
                return value.strip()
        return None

    def workspace_value(node: dict[str, Any]) -> str | None:
        for key, value in node.items():
            normalised = str(key).replace("-", "_").lower()
            if normalised not in workspace_keys:
                continue
            if isinstance(value, str) and value.strip():
                return value.strip()
            extracted = extract_id(value, "workspace")
            if extracted:
                return extracted
        return None

    def visit(node: Any, inherited_workspace: str | None = None) -> None:
        if isinstance(node, dict):
            workspace_id = workspace_value(node) or inherited_workspace
            path_value = direct_value(node, path_keys)
            branch_value = direct_value(node, branch_keys)
            if path_value is not None and branch_value is not None:
                candidate = Path(path_value).expanduser()
                if not candidate.is_absolute():
                    candidate = repo / candidate
                if (
                    candidate.resolve() == saved_path.resolve()
                    and branch_value == saved_branch
                ):
                    matches.append(workspace_id)
            for value in node.values():
                visit(value, workspace_id)
        elif isinstance(node, list):
            for value in node:
                visit(value, inherited_workspace)

    visit(payload)
    if len(matches) != 1 or not matches[0]:
        raise FlowError(
            "現在の worktree list から保存済み path+branch に一意な workspace id を "
            "取得できません"
        )
    return str(matches[0])


def _result_contract(spec: StepSpec, run_id: str) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "run_id": run_id,
        "step_id": spec.id,
        "role": spec.role,
        "status": "success | blocked | failed",
        "summary": "短い文字列",
        "artifacts": [
            {
                "path": "/absolute/path/to/an-existing-file",
                "kind": "one of: " + ", ".join(spec.contract.artifact_kinds),
                "description": "成果物の説明",
            }
        ],
        "changed_files": ["worktree-relative/path"],
        "tests": ["実行した検証の説明"],
        "contract": spec.contract.as_dict(),
    }


def _validate_contract(
    result: Any, spec: StepSpec, state: dict[str, Any]
) -> tuple[bool, str]:
    if not isinstance(result, dict):
        return False, "result.json のトップレベルは object が必要です"
    required = set(spec.contract.required_fields)
    missing = sorted(field for field in required if field not in result)
    if missing:
        return False, f"必須フィールドがありません: {', '.join(missing)}"
    if result.get("schema_version") != 1:
        return False, "schema_version は 1 である必要があります"
    if result.get("run_id") != state.get("run_id"):
        return False, "result.run_id が現在の run と一致しません"
    if result.get("step_id") != spec.id:
        return False, "result.step_id が現在の step と一致しません"
    if result.get("role") != spec.role:
        return False, "result.role が workflow の role と一致しません"
    if result.get("status") not in {"success", "blocked", "failed"}:
        return False, "result.status は success / blocked / failed のいずれかが必要です"
    if not isinstance(result.get("summary"), str) or not result["summary"].strip():
        return False, "result.summary は空でない文字列が必要です"

    artifacts = result.get("artifacts")
    if not isinstance(artifacts, list) or not artifacts:
        return False, "result.artifacts は1つ以上の配列が必要です"
    run_dir = Path(state["run_dir"]).resolve()
    worktree = Path(state["worktree"]["path"]).resolve()
    allowed_roots = (run_dir, worktree)
    for index, artifact in enumerate(artifacts):
        if not isinstance(artifact, dict):
            return False, f"artifacts[{index}] は object が必要です"
        path_value = artifact.get("path")
        if not isinstance(path_value, str) or not Path(path_value).is_absolute():
            return False, f"artifacts[{index}].path は絶対パスが必要です"
        path = Path(path_value).resolve()
        if not _is_within(path, allowed_roots):
            return False, f"artifacts[{index}].path が run/worktree 外です: {path}"
        if not path.is_file():
            return False, f"artifacts[{index}].path が存在しません: {path}"
        kind = artifact.get("kind")
        if kind not in spec.contract.artifact_kinds:
            return False, f"artifacts[{index}].kind が不正です: {kind!r}"
        if (
            not isinstance(artifact.get("description"), str)
            or not artifact["description"].strip()
        ):
            return False, f"artifacts[{index}].description は必須です"

    changed_files = result.get("changed_files", [])
    if not isinstance(changed_files, list) or any(
        not isinstance(item, str) for item in changed_files
    ):
        return False, "changed_files は文字列配列が必要です"
    for index, item in enumerate(changed_files):
        relative = Path(item)
        candidate = (worktree / relative).resolve()
        if not item.strip() or relative.is_absolute() or candidate == worktree:
            return (
                False,
                f"changed_files[{index}] は worktree-relative path が必要です: {item!r}",
            )
        if not _is_within(candidate, (worktree,)):
            return False, f"changed_files[{index}] が worktree 外です: {item!r}"
    if spec.write_policy == "none" and changed_files:
        return False, f"read-only step が changed_files を報告しました: {changed_files}"
    tests = result.get("tests", [])
    if not isinstance(tests, list) or any(not isinstance(item, str) for item in tests):
        return False, "tests は文字列配列が必要です"
    for field in spec.contract.required_boolean_fields:
        if not isinstance(result.get(field), bool):
            return False, f"{field} は boolean が必要です"
    for field in spec.contract.required_list_fields:
        if not isinstance(result.get(field), list):
            return False, f"{field} は配列が必要です"
    if spec.role == "reviewer":
        decision = result.get("decision")
        if decision not in {"approved", "needs_fixer"}:
            return False, "reviewer の decision は approved / needs_fixer が必要です"
        if result.get("needs_fixer") != (decision == "needs_fixer"):
            return False, "reviewer の decision と needs_fixer が一致しません"
    if spec.role == "verifier" and not isinstance(result.get("verified"), bool):
        return False, "verifier の verified は boolean が必要です"
    return True, "ok"


def _error_payload(exc: Exception) -> dict[str, Any]:
    value: dict[str, Any] = {"message": str(exc), "type": type(exc).__name__}
    if isinstance(exc, CommandError):
        code = getattr(exc, "code", "command_failed")
        returncode = getattr(exc, "returncode", None)
        stderr = getattr(exc, "stderr", "")
        value.update({"code": code, "returncode": returncode})
        if stderr:
            value["stderr"] = stderr[-2000:]
    return value


class FlowEngine:
    """Create, resume, cancel, and clean one workflow run."""

    def __init__(self, store: RunStore, client: HerdrClient) -> None:
        self.store = store
        self.client = client

    def _append_event(self, state: dict[str, Any], event: str, **details: Any) -> None:
        events = state.setdefault("events", [])
        events.append({"at": utc_now(), "event": event, **details})
        if len(events) > 200:
            del events[:-200]

    def _save(self, state: dict[str, Any]) -> None:
        self.store.save(state["run_id"], state)

    def _snapshot_step(
        self, state: dict[str, Any], spec: StepSpec, phase: str
    ) -> dict[str, str]:
        if phase not in {"before", "after"}:
            raise ValueError(f"unknown worktree snapshot phase: {phase}")
        worktree = Path(state["worktree"]["path"])
        run_dir = Path(state["run_dir"])
        worktree_state = state.get("worktree")
        if not isinstance(worktree_state, dict):
            raise FlowError("state の worktree が不正です")
        enabled = worktree_state.get("enabled", True)
        if not isinstance(enabled, bool):
            raise FlowError("state の worktree.enabled が不正です")
        snapshot = _snapshot_worktree(
            worktree,
            excluded=(run_dir,),
            allow_filesystem_fallback=not enabled,
        )
        step_state = state["steps"][spec.id]
        step_state[f"worktree_snapshot_{phase}"] = {
            "captured_at": utc_now(),
            "files": snapshot,
        }
        if phase == "after":
            before = step_state.get("worktree_snapshot_before")
            if not isinstance(before, dict) or not isinstance(
                before.get("files"), dict
            ):
                raise FlowError(
                    f"step {spec.id} の worktree before snapshot がありません"
                )
            step_state["actual_changed_files"] = _snapshot_diff(
                before["files"], snapshot
            )
        self._save(state)
        return snapshot

    def _run_verification(
        self, state: dict[str, Any], spec: StepSpec
    ) -> dict[str, Any]:
        worktree = Path(state["worktree"]["path"]).resolve()
        records: list[dict[str, Any]] = []
        for command in state.get("verify_commands", []):
            record: dict[str, Any] = {"argv": list(command)}
            try:
                completed = subprocess.run(
                    list(command),
                    cwd=worktree,
                    env=dict(self.client.environ),
                    stdin=subprocess.DEVNULL,
                    capture_output=True,
                    text=True,
                    check=False,
                    timeout=spec.timeout_seconds,
                    shell=False,
                )
            except subprocess.TimeoutExpired as exc:
                record.update(
                    {
                        "returncode": None,
                        "stdout": _bounded_text(exc.stdout),
                        "stderr": _bounded_text(exc.stderr),
                        "error": "timeout",
                        "success": False,
                    }
                )
            except OSError as exc:
                record.update(
                    {
                        "returncode": None,
                        "stdout": "",
                        "stderr": _bounded_text(exc),
                        "error": type(exc).__name__,
                        "success": False,
                    }
                )
            else:
                record.update(
                    {
                        "returncode": completed.returncode,
                        "stdout": _bounded_text(completed.stdout),
                        "stderr": _bounded_text(completed.stderr),
                        "success": completed.returncode == 0,
                    }
                )
            records.append(record)
        verification = {
            "commands": records,
            "all_succeeded": bool(records)
            and all(bool(record.get("success")) for record in records),
        }
        artifact_path = Path(state["run_dir"]) / "steps" / spec.id / "verification.json"
        atomic_write_json(artifact_path, verification)
        step_state = state["steps"][spec.id]
        step_state["verification"] = verification
        step_state["verification_artifact_path"] = str(artifact_path)
        self._append_event(
            state,
            "verification_completed",
            step_id=spec.id,
            all_succeeded=verification["all_succeeded"],
        )
        self._save(state)
        return verification

    def _new_state(
        self, run_id: str, options: StartOptions, run_dir: Path
    ) -> dict[str, Any]:
        repo = options.repo.resolve()
        steps: dict[str, dict[str, Any]] = {}
        for spec in options.workflow.steps:
            step_dir = run_dir / "steps" / spec.id
            steps[spec.id] = {
                "id": spec.id,
                "role": spec.role,
                "kind": spec.kind,
                "depends_on": list(spec.depends_on),
                "read_policy": spec.read_policy,
                "write_policy": spec.write_policy,
                "timeout_seconds": spec.timeout_seconds,
                "condition": spec.condition,
                "reuse_agent": spec.reuse_agent,
                "submit_key": spec.submit_key,
                "agent_args": list(spec.agent_args),
                "contract": spec.contract.as_dict(),
                "status": "pending",
                "attempts": 0,
                "result_path": str(step_dir / "result.json"),
                "agent": None,
                "started_at": None,
                "finished_at": None,
                "error": None,
                "result": None,
                "worktree_snapshot_before": None,
                "worktree_snapshot_after": None,
                "actual_changed_files": [],
                "verification": None,
                "verification_artifact_path": None,
            }
        return {
            "schema_version": 1,
            "run_id": run_id,
            "status": "created",
            "task": options.task,
            "repo": str(repo),
            "run_dir": str(run_dir),
            "created_at": utc_now(),
            "updated_at": utc_now(),
            "current_step": None,
            "workflow": options.workflow.as_dict(),
            "verify_commands": [list(command) for command in options.verify_commands],
            "worktree": {
                "enabled": options.use_worktree,
                "path": str(repo),
                "branch": options.branch,
                "base": options.base,
            },
            "resources": {
                "workspace_id": None,
                "orchestrator_tab_id": None,
                "orchestrator_pane_id": None,
                "tabs": {},
                "panes": [],
                "worktree_created": False,
            },
            "orchestrator": {
                "script_path": str(options.script_path.resolve())
                if options.script_path
                else None,
                "background": options.background,
            },
            "steps": steps,
            "events": [{"at": utc_now(), "event": "created"}],
        }

    @staticmethod
    def _required_id(payload: Any, kind: str) -> str:
        value = extract_id(payload, kind)
        if not value:
            raise FlowError(f"Herdr {kind} id をレスポンスから抽出できませんでした")
        return value

    @staticmethod
    def _normalise_resource_path(value: str | None, fallback: Path) -> Path:
        if not value:
            return fallback.resolve()
        path = Path(value).expanduser()
        if not path.is_absolute():
            path = fallback / path
        return path.resolve()

    def _create_workspace_tab(
        self,
        *,
        cwd: Path,
        label: str,
        workspace_id: str | None = None,
    ) -> tuple[str, str, str]:
        if workspace_id is None:
            payload = self.client.run_json(
                [
                    "workspace",
                    "create",
                    "--cwd",
                    str(cwd),
                    "--label",
                    label,
                    "--no-focus",
                ],
                timeout_seconds=60,
            )
            workspace_id = self._required_id(payload, "workspace")
            tab_id = extract_id(payload, "tab")
            pane_id = extract_id(payload, "pane")
        else:
            tab_id = None
            pane_id = None
        if not tab_id or not pane_id:
            payload = self.client.run_json(
                [
                    "tab",
                    "create",
                    "--workspace",
                    workspace_id,
                    "--cwd",
                    str(cwd),
                    "--label",
                    label,
                    "--no-focus",
                ],
                timeout_seconds=60,
            )
            tab_id = tab_id or self._required_id(payload, "tab")
            pane_id = pane_id or self._required_id(payload, "pane")
        return workspace_id, tab_id, pane_id

    def _provision(self, state: dict[str, Any], options: StartOptions) -> None:
        repo = Path(state["repo"]).resolve()
        resources = state["resources"]
        if options.use_worktree:
            branch = options.branch or f"panopticon/{state['run_id']}"
            arguments = [
                "worktree",
                "create",
                "--cwd",
                str(repo),
                "--branch",
                branch,
                "--label",
                f"flow-{state['run_id'][-8:]}",
                "--no-focus",
            ]
            if options.base:
                arguments.extend(["--base", options.base])
            if options.worktree_path:
                arguments.extend(["--path", str(options.worktree_path.resolve())])
            payload = self.client.run_json(arguments, timeout_seconds=120)
            raw_worktree_path = extract_path(payload)
            if not raw_worktree_path:
                raise FlowError(
                    "Herdr worktree create のレスポンスに path がありません"
                )
            worktree_path = self._normalise_resource_path(raw_worktree_path, repo)
            if not worktree_path.is_dir():
                raise FlowError(
                    f"Herdr worktree が directory ではありません: {worktree_path}"
                )
            if _is_within(worktree_path, (repo,)):
                raise FlowError(
                    f"専用 worktree は対象 repository の外側である必要があります: {worktree_path}"
                )
            workspace_id = extract_id(payload, "workspace")
            tab_id = extract_id(payload, "tab")
            pane_id = extract_id(payload, "pane")
            if workspace_id is None or pane_id is None:
                created_workspace, created_tab, created_pane = (
                    self._create_workspace_tab(
                        cwd=worktree_path,
                        label=f"flow-{state['run_id'][-8:]}",
                        workspace_id=workspace_id,
                    )
                )
                workspace_id = workspace_id or created_workspace
                tab_id = tab_id or created_tab
                pane_id = pane_id or created_pane
            resources["worktree_created"] = True
            state["worktree"].update({"path": str(worktree_path), "branch": branch})
        else:
            workspace_id, tab_id, pane_id = self._create_workspace_tab(
                cwd=repo,
                label=f"flow-{state['run_id'][-8:]}",
            )
        resources["workspace_id"] = workspace_id
        resources["orchestrator_tab_id"] = tab_id
        resources["orchestrator_pane_id"] = pane_id
        resources["panes"] = [pane_id]
        self._append_event(
            state,
            "resources_created",
            workspace_id=workspace_id,
            tab_id=tab_id,
            pane_id=pane_id,
            worktree=state["worktree"],
        )
        self._save(state)

    def create_run(self, options: StartOptions) -> dict[str, Any]:
        require_herdr_env(self.client.environ)
        repo = options.repo.resolve()
        if not repo.is_dir():
            raise FlowError(f"repo がディレクトリではありません: {repo}")
        run_id = make_run_id()
        run_dir = self.store.run_dir(run_id)
        state = self._new_state(run_id, options, run_dir)
        self.store.create(run_id, state)
        for step_state in state["steps"].values():
            Path(step_state["result_path"]).parent.mkdir(
                parents=True, exist_ok=True, mode=0o700
            )
        try:
            with self.store.lock(run_id):
                state = self.store.load(run_id)
                self._provision(state, options)
                state["status"] = "running"
                self._append_event(state, "provisioned")
                self._save(state)
        except (FlowError, HerdrError, StateError, OSError) as exc:
            state = self.store.load(run_id)
            state["status"] = "failed"
            state["error"] = _error_payload(exc)
            self._append_event(state, "provision_failed", error=state["error"])
            self._save(state)
            raise FlowError(f"run {run_id} の初期化に失敗しました: {exc}") from exc

        if not options.background:
            return self.resume_run(run_id, options.workflow)
        try:
            self._launch_background(run_id, state, options)
        except (FlowError, HerdrError, StateError, OSError) as exc:
            with self.store.lock(run_id):
                state = self.store.load(run_id)
                state["status"] = "failed"
                state["error"] = _error_payload(exc)
                self._append_event(
                    state, "orchestrator_launch_failed", error=state["error"]
                )
                self._save(state)
            raise FlowError(
                f"run {run_id} のバックグラウンド起動に失敗しました: {exc}"
            ) from exc
        return self.store.load(run_id)

    def _launch_background(
        self, run_id: str, state: dict[str, Any], options: StartOptions
    ) -> None:
        pane_id = state["resources"].get("orchestrator_pane_id")
        script = (
            options.script_path
            or Path(__file__).resolve().parents[1] / "bin" / "panopticon"
        )
        if not pane_id:
            raise FlowError("orchestrator pane がありません")
        command_tokens = [
            sys.executable,
            str(script.resolve()),
            "resume",
            "--run-id",
            run_id,
            "--foreground",
            "--state-root",
            str(self.store.root),
            "--herdr-bin",
            self.client.resolved_executable,
        ]
        command_text = shlex.join(command_tokens)
        pane_command = ["pane", "run", pane_id, command_text]
        raw_result = self.client.run_raw(pane_command, timeout_seconds=30)
        if raw_result.returncode != 0:
            raise CommandError(
                f"Herdr pane run が失敗しました: {' '.join(pane_command)}",
                argv=(self.client.executable, *pane_command),
                returncode=raw_result.returncode,
                stdout=raw_result.stdout,
                stderr=raw_result.stderr,
            )
        with self.store.lock(run_id):
            current = self.store.load(run_id)
            current["orchestrator"].update(
                {"command": command_tokens, "launched_at": utc_now()}
            )
            self._append_event(current, "orchestrator_launched", pane_id=pane_id)
            self._save(current)

    def _load_result(self, state: dict[str, Any], spec: StepSpec) -> Any | None:
        path = Path(state["steps"][spec.id]["result_path"])
        if not path.is_file():
            return None
        try:
            return read_json(path)
        except StateError:
            return None

    def _condition_matches(self, condition: str | None, state: dict[str, Any]) -> bool:
        if not condition:
            return True
        expression = condition.strip()
        expected: bool | None = None
        if "==" in expression:
            expression, raw_expected = (
                part.strip() for part in expression.split("==", 1)
            )
            if raw_expected.lower() not in {"true", "false"}:
                raise FlowError(
                    f"condition の比較値は true/false のみです: {condition}"
                )
            expected = raw_expected.lower() == "true"
        parts = expression.split(".")
        if len(parts) != 2 or not all(parts):
            raise FlowError(f"condition は step.field 形式が必要です: {condition}")
        dependency, field = parts
        dependency_state = state["steps"].get(dependency)
        if not dependency_state or not isinstance(dependency_state.get("result"), dict):
            return False
        value = dependency_state["result"].get(field)
        if expected is not None:
            return value is expected
        return bool(value)

    def _dependency_state(
        self, state: dict[str, Any], spec: StepSpec
    ) -> tuple[bool, str | None]:
        for dependency in spec.depends_on:
            dependency_status = state["steps"][dependency]["status"]
            if dependency_status in {"failed", "timed_out", "cancelled"}:
                return False, dependency
            if dependency_status == "blocked":
                return False, dependency
            if dependency_status not in STEP_SUCCESS:
                return False, None
        return True, None

    def _agent_name(self, state: dict[str, Any], spec: StepSpec) -> str:
        suffix = re.sub(r"[^a-z0-9_-]", "-", state["run_id"][-8:].lower())
        name = f"pan-{suffix}-{spec.id}"
        return name[:31]

    @staticmethod
    def _agent_start_args(name: str, spec: StepSpec, pane_id: str) -> list[str]:
        arguments = [
            "agent",
            "start",
            name,
            "--kind",
            spec.kind,
            "--pane",
            pane_id,
            "--timeout",
            str(_clamp_agent_start_timeout_ms(spec.timeout_ms)),
        ]
        if spec.agent_args:
            arguments.extend(["--", *spec.agent_args])
        return arguments

    def _ensure_agent(self, state: dict[str, Any], spec: StepSpec) -> str:
        step_state = state["steps"][spec.id]
        if spec.reuse_agent:
            source = state["steps"][spec.reuse_agent]
            source_agent = source.get("agent")
            if not isinstance(source_agent, dict) or not source_agent.get("target"):
                raise FlowError(
                    f"reuse_agent の対象に agent がありません: {spec.reuse_agent}"
                )
            step_state["agent"] = {
                **source_agent,
                "reused_from": spec.reuse_agent,
                "agent_args": list(source_agent.get("agent_args", spec.agent_args)),
            }
            return str(source_agent["target"])
        existing = step_state.get("agent")
        if isinstance(existing, dict) and existing.get("target"):
            existing["agent_args"] = list(spec.agent_args)
            if not existing.get("started", True):
                self.client.run_json(
                    self._agent_start_args(
                        str(existing["name"]), spec, str(existing["pane_id"])
                    ),
                    timeout_seconds=min(spec.timeout_seconds + 30, 300),
                )
                existing["started"] = True
                self._save(state)
            return str(existing["target"])

        workspace_id = state["resources"].get("workspace_id")
        worktree = Path(state["worktree"]["path"])
        if not workspace_id:
            raise FlowError("workspace id が state にありません")
        payload = self.client.run_json(
            [
                "tab",
                "create",
                "--workspace",
                str(workspace_id),
                "--cwd",
                str(worktree),
                "--label",
                f"flow-{spec.id}",
                "--env",
                "PANOPTICON_CHILD=1",
                "--no-focus",
            ],
            timeout_seconds=60,
        )
        tab_id = self._required_id(payload, "tab")
        pane_id = self._required_id(payload, "pane")
        name = self._agent_name(state, spec)
        step_state["agent"] = {
            "name": name,
            "target": name,
            "kind": spec.kind,
            "tab_id": tab_id,
            "pane_id": pane_id,
            "agent_args": list(spec.agent_args),
            "started": False,
        }
        state["resources"]["tabs"][spec.id] = tab_id
        state["resources"]["panes"].append(pane_id)
        self._save(state)
        self.client.run_json(
            self._agent_start_args(name, spec, pane_id),
            timeout_seconds=min(spec.timeout_seconds + 30, 300),
        )
        step_state["agent"]["started"] = True
        self._append_event(
            state, "agent_started", step_id=spec.id, target=name, pane_id=pane_id
        )
        self._save(state)
        return name

    def _prompt(self, state: dict[str, Any], spec: StepSpec) -> str:
        step_state = state["steps"][spec.id]
        try:
            template = spec.template.read_text(encoding="utf-8")
        except OSError as exc:
            raise FlowError(
                f"prompt template を読めません: {spec.template}: {exc}"
            ) from exc
        dependency_results = {
            dependency: state["steps"][dependency]["result_path"]
            for dependency in spec.depends_on
        }
        replacements = {
            "TASK": str(state["task"]),
            "RUN_ID": str(state["run_id"]),
            "STEP_ID": spec.id,
            "ROLE": spec.role,
            "WORKTREE_PATH": str(Path(state["worktree"]["path"]).resolve()),
            "RUN_DIR": str(Path(state["run_dir"]).resolve()),
            "RESULT_PATH": str(Path(step_state["result_path"]).resolve()),
            "DEPENDENCY_RESULTS": json.dumps(
                dependency_results, ensure_ascii=False, indent=2
            ),
            "VERIFY_COMMANDS": json.dumps(
                state["verify_commands"], ensure_ascii=False, indent=2
            ),
            "ENGINE_VERIFICATION": json.dumps(
                step_state.get("verification") or {},
                ensure_ascii=False,
                indent=2,
            ),
            "VERIFICATION_ARTIFACT": str(
                step_state.get("verification_artifact_path") or ""
            ),
            "READ_POLICY": spec.read_policy,
            "WRITE_POLICY": spec.write_policy,
            "JSON_CONTRACT": json.dumps(
                _result_contract(spec, state["run_id"]), ensure_ascii=False, indent=2
            ),
        }
        for key, value in replacements.items():
            template = template.replace("{{" + key + "}}", value)
        return template.strip() + "\n"

    def _mark_step(
        self, state: dict[str, Any], spec: StepSpec, status: str, **details: Any
    ) -> None:
        step_state = state["steps"][spec.id]
        step_state["status"] = status
        if status in STEP_TERMINAL:
            step_state["finished_at"] = utc_now()
        if "error" in details:
            step_state["error"] = details["error"]
        self._append_event(state, f"step_{status}", step_id=spec.id, **details)

    def _mark_cancelled(self, state: dict[str, Any]) -> None:
        current = state.get("current_step")
        for step_state in state["steps"].values():
            if step_state["status"] not in STEP_TERMINAL:
                step_state["status"] = "cancelled"
                step_state["finished_at"] = utc_now()
        state["status"] = "cancelled"
        state["current_step"] = current
        self._append_event(state, "cancelled")
        self.store.clear_cancel_request(state["run_id"])
        self._save(state)

    def _worktree_change_error(
        self, state: dict[str, Any], spec: StepSpec, result: dict[str, Any]
    ) -> str | None:
        step_state = state["steps"][spec.id]
        actual_value = step_state.get("actual_changed_files")
        if not isinstance(actual_value, list) or any(
            not isinstance(item, str) for item in actual_value
        ):
            return f"step {spec.id} の worktree after snapshot がありません"
        worktree = Path(state["worktree"]["path"]).resolve()
        declared: list[str] = []
        for item in result.get("changed_files", []):
            relative = Path(item)
            candidate = (worktree / relative).resolve()
            if not _is_within(candidate, (worktree,)):
                return f"changed_files が worktree 外です: {item!r}"
            try:
                normalised = candidate.relative_to(worktree).as_posix()
            except ValueError:
                return f"changed_files が worktree 外です: {item!r}"
            declared.append(normalised)
        if len(declared) != len(set(declared)):
            return f"changed_files に重複があります: {declared}"
        actual = sorted(set(actual_value))
        if spec.write_policy == "none":
            if actual:
                return f"read-only step が worktree を変更しました: {actual}"
            return None
        if set(declared) != set(actual):
            return (
                f"実際の worktree 差分と changed_files が一致しません: "
                f"declared={declared}, actual={actual}"
            )
        return None

    def _apply_result(
        self, state: dict[str, Any], spec: StepSpec, result: dict[str, Any]
    ) -> None:
        step_state = state["steps"][spec.id]
        step_state["result"] = result
        if result["status"] == "blocked":
            self._mark_step(
                state, spec, "blocked", result_summary=result.get("summary")
            )
            state["status"] = "blocked"
        elif result["status"] == "failed":
            self._mark_step(state, spec, "failed", result_summary=result.get("summary"))
            state["status"] = "failed"
            state["error"] = {
                "message": result.get("summary"),
                "type": "agent_result_failed",
            }
        else:
            if spec.role == "verifier":
                verification = step_state.get("verification")
                if not isinstance(verification, dict) or not verification.get(
                    "all_succeeded", False
                ):
                    error = {
                        "message": "engine が実行した検証コマンドがすべて成功していません",
                        "type": "verification_failed",
                    }
                    self._mark_step(state, spec, "failed", error=error)
                    state["status"] = "failed"
                    state["error"] = error
                elif not result.get("verified"):
                    error = {
                        "message": "verifier が verified=false を返しました",
                        "type": "verification_failed",
                    }
                    self._mark_step(state, spec, "failed", error=error)
                    state["status"] = "failed"
                    state["error"] = error
                else:
                    self._mark_step(
                        state, spec, "completed", result_summary=result.get("summary")
                    )
            else:
                self._mark_step(
                    state, spec, "completed", result_summary=result.get("summary")
                )
        self._save(state)

    def _apply_recovered_result(
        self, state: dict[str, Any], spec: StepSpec
    ) -> bool | None:
        result = self._load_result(state, spec)
        if result is None:
            return None
        valid, _reason = _validate_contract(result, spec, state)
        if not valid:
            return None
        step_state = state["steps"][spec.id]
        before = step_state.get("worktree_snapshot_before")
        if not isinstance(before, dict) or not isinstance(before.get("files"), dict):
            error = {
                "message": f"step {spec.id} の worktree before snapshot がありません",
                "type": "worktree_snapshot_failed",
            }
            self._mark_step(state, spec, "failed", error=error)
            state["status"] = "failed"
            state["error"] = error
            self._save(state)
            return True
        try:
            self._snapshot_step(state, spec, "after")
        except (FlowError, StateError, OSError) as exc:
            error = {"message": str(exc), "type": "worktree_snapshot_failed"}
            self._mark_step(state, spec, "failed", error=error)
            state["status"] = "failed"
            state["error"] = error
            self._save(state)
            return True
        change_error = self._worktree_change_error(state, spec, result)
        if change_error:
            error = {"message": change_error, "type": "worktree_change_mismatch"}
            self._mark_step(state, spec, "failed", error=error)
            state["status"] = "failed"
            state["error"] = error
            self._save(state)
            return True
        self._apply_result(state, spec, result)
        return True

    def _recover_running_step(self, state: dict[str, Any], spec: StepSpec) -> bool:
        step_state = state["steps"][spec.id]
        recovered = self._apply_recovered_result(state, spec)
        if recovered is not None:
            return recovered
        agent = step_state.get("agent")
        if not isinstance(agent, dict) or not agent.get("target"):
            step_state["status"] = "pending"
            self._save(state)
            return False
        try:
            payload = self.client.run_json(
                [
                    "agent",
                    "wait",
                    str(agent["target"]),
                    "--timeout",
                    str(spec.timeout_ms),
                ],
                timeout_seconds=spec.timeout_seconds + 30,
            )
        except CommandError as exc:
            if exc.code in {"agent_blocked", "blocked"}:
                self._mark_step(state, spec, "blocked", error=_error_payload(exc))
                state["status"] = "blocked"
                self._save(state)
                return True
            step_state["status"] = "pending"
            step_state["error"] = _error_payload(exc)
            self._save(state)
            return False
        recovered = self._apply_recovered_result(state, spec)
        if recovered is not None:
            return recovered
        if extract_status(payload) == "blocked":
            self._mark_step(
                state, spec, "blocked", error={"message": "agent が blocked 状態です"}
            )
            state["status"] = "blocked"
            self._save(state)
            return True
        step_state["status"] = "pending"
        self._save(state)
        return False

    @staticmethod
    def _is_timeout_error(exc: CommandError) -> bool:
        return exc.code in {"timeout", "timeout_or_start_failed"}

    def _run_raw_checked(
        self,
        args: list[str],
        *,
        timeout_seconds: float,
        description: str,
    ) -> None:
        result = self.client.run_raw(args, timeout_seconds=timeout_seconds)
        if result.returncode == 0:
            return
        payload = parse_json_output(result.stderr) or parse_json_output(result.stdout)
        code = extract_error_code(payload) if payload is not None else None
        details = (result.stderr or result.stdout).strip()
        message = f"{description} が失敗しました: {' '.join(args)}"
        if details:
            message += f"\n{details}"
        raise CommandError(
            message,
            argv=(self.client.executable, *args),
            returncode=result.returncode,
            code=code or "command_failed",
            stdout=result.stdout,
            stderr=result.stderr,
        )

    def _wait_agent(
        self,
        target: str,
        *,
        until: str | None,
        timeout_ms: int,
    ) -> Any:
        args = ["agent", "wait", target]
        if until is not None:
            args.extend(["--until", until])
        args.extend(["--timeout", str(timeout_ms)])
        payload = self.client.run_json(
            args,
            timeout_seconds=max(1.0, timeout_ms / 1000 + 5),
        )
        if extract_status(payload) == "blocked":
            raise CommandError(
                f"agent {target} が blocked 状態です",
                argv=(self.client.executable, *args),
                code="agent_blocked",
                stdout=json.dumps(payload, ensure_ascii=False),
            )
        return payload

    @staticmethod
    def _status_from_agent_error(exc: HerdrError) -> str | None:
        if isinstance(exc, CommandError) and exc.code in {
            "agent_blocked",
            "blocked",
        }:
            return "blocked"
        return None

    def _current_agent_status(self, target: str) -> str | None:
        args = ["agent", "get", target]
        try:
            payload = self.client.run_json(args, timeout_seconds=5.0)
        except HerdrError as exc:
            return self._status_from_agent_error(exc)
        return extract_status(payload)

    def _handle_custom_submission_timeout(
        self,
        state: dict[str, Any],
        spec: StepSpec,
        target: str,
        timeout_error: CommandError,
    ) -> None:
        result = self._load_result(state, spec)
        status = self._current_agent_status(target)
        if status == "blocked":
            raise CommandError(
                f"agent {target} が blocked 状態です",
                argv=(self.client.executable, "agent", "get", target),
                code="agent_blocked",
            )
        if result is not None and status in {"idle", "done"}:
            return
        raise timeout_error

    def _wait_custom_submission(
        self, state: dict[str, Any], spec: StepSpec, target: str
    ) -> None:
        deadline = time.monotonic() + spec.timeout_seconds
        working_timeout = min(spec.timeout_ms, WORKING_WAIT_MS)
        try:
            self._wait_agent(
                target,
                until="working",
                timeout_ms=working_timeout,
            )
        except CommandError as exc:
            if not self._is_timeout_error(exc):
                raise
            self._handle_custom_submission_timeout(state, spec, target, exc)
            return

        try:
            remaining_ms = max(0, int((deadline - time.monotonic()) * 1000))
        except (OverflowError, ValueError):
            remaining_ms = 0
        if remaining_ms == 0:
            timeout_error = CommandError(
                f"agent {target} の settled 待機がタイムアウトしました",
                argv=(self.client.executable, "agent", "wait", target),
                code="timeout",
            )
            self._handle_custom_submission_timeout(state, spec, target, timeout_error)
            return
        try:
            self._wait_agent(
                target,
                until=None,
                timeout_ms=remaining_ms,
            )
        except CommandError as exc:
            if not self._is_timeout_error(exc):
                raise
            self._handle_custom_submission_timeout(state, spec, target, exc)

    def _submit_prompt(
        self, state: dict[str, Any], spec: StepSpec, target: str, prompt: str
    ) -> None:
        prompt_args = ["agent", "prompt", target, prompt]
        if spec.submit_key is None:
            self.client.run_json(
                [*prompt_args, "--wait", "--timeout", str(spec.timeout_ms)],
                timeout_seconds=spec.timeout_seconds + 30,
            )
            return

        agent = state["steps"][spec.id].get("agent")
        pane_id = agent.get("pane_id") if isinstance(agent, dict) else None
        if not pane_id:
            raise FlowError(
                f"step {spec.id}: submit_key の送信先 agent pane がありません"
            )
        self._run_raw_checked(
            prompt_args,
            timeout_seconds=spec.timeout_seconds + 30,
            description="agent prompt",
        )
        self._run_raw_checked(
            ["pane", "send-keys", str(pane_id), spec.submit_key],
            timeout_seconds=30,
            description="pane send-keys",
        )
        self._wait_custom_submission(state, spec, target)

    def _execute_step(self, state: dict[str, Any], spec: StepSpec) -> None:
        step_state = state["steps"][spec.id]
        result_path = Path(step_state["result_path"])
        result_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        if result_path.exists():
            result_path.unlink()
        state["current_step"] = spec.id
        step_state["status"] = "running"
        step_state["attempts"] += 1
        step_state["started_at"] = utc_now()
        step_state["finished_at"] = None
        step_state["error"] = None
        step_state["worktree_snapshot_before"] = None
        step_state["worktree_snapshot_after"] = None
        step_state["actual_changed_files"] = []
        step_state["verification"] = None
        step_state["verification_artifact_path"] = None
        state["status"] = "running"
        self._append_event(
            state, "step_started", step_id=spec.id, attempt=step_state["attempts"]
        )
        self._save(state)

        try:
            self._snapshot_step(state, spec, "before")
        except (FlowError, StateError, OSError) as exc:
            error = {
                "message": str(exc),
                "type": "worktree_snapshot_failed",
            }
            self._mark_step(state, spec, "failed", error=error)
            state["status"] = "failed"
            state["error"] = error
            self._save(state)
            return

        failure_status: str | None = None
        failure_error: dict[str, Any] | None = None
        try:
            if spec.role == "verifier":
                self._run_verification(state, spec)
            target = self._ensure_agent(state, spec)
            prompt = self._prompt(state, spec)
            self._submit_prompt(state, spec, target, prompt)
        except CommandError as exc:
            failure_error = _error_payload(exc)
            if exc.code in {"agent_blocked", "blocked"}:
                failure_status = "blocked"
            elif self._is_timeout_error(exc):
                failure_status = "timed_out"
            else:
                failure_status = "failed"
        except (FlowError, HerdrError, StateError, OSError) as exc:
            failure_status = "failed"
            failure_error = _error_payload(exc)

        try:
            self._snapshot_step(state, spec, "after")
        except (FlowError, StateError, OSError) as exc:
            failure_status = "failed"
            failure_error = {
                "message": str(exc),
                "type": "worktree_snapshot_failed",
            }

        actual_changed_files = step_state.get("actual_changed_files", [])
        if spec.write_policy == "none" and actual_changed_files:
            failure_status = "failed"
            failure_error = {
                "message": f"read-only step が worktree を変更しました: {actual_changed_files}",
                "type": "worktree_changed_read_only",
            }
        if failure_status is not None:
            self._mark_step(
                state,
                spec,
                failure_status,
                error=failure_error or {"message": "step が失敗しました"},
            )
            state["status"] = "blocked" if failure_status == "blocked" else "failed"
            if state["status"] == "failed":
                state["error"] = failure_error or {
                    "message": "step が失敗しました",
                    "type": "step_failed",
                }
            self._save(state)
            return

        result = self._load_result(state, spec)
        if result is None:
            error = {
                "message": "agent prompt 後に result.json が生成されませんでした",
                "type": "missing_result",
            }
            self._mark_step(state, spec, "failed", error=error)
            state["status"] = "failed"
            state["error"] = error
            self._save(state)
            return
        valid, reason = _validate_contract(result, spec, state)
        if not valid:
            error = {"message": reason, "type": "invalid_result"}
            self._mark_step(state, spec, "failed", error=error)
            state["status"] = "failed"
            state["error"] = error
            self._save(state)
            return
        change_error = self._worktree_change_error(state, spec, result)
        if change_error:
            error = {"message": change_error, "type": "worktree_change_mismatch"}
            self._mark_step(state, spec, "failed", error=error)
            state["status"] = "failed"
            state["error"] = error
            self._save(state)
            return
        self._apply_result(state, spec, result)

    def _execute(self, state: dict[str, Any], workflow: Workflow) -> None:
        while True:
            if self.store.cancel_requested(state["run_id"]):
                self._mark_cancelled(state)
                return
            progress = False
            for spec in workflow.steps:
                step_state = state["steps"][spec.id]
                status = step_state["status"]
                if status in STEP_SUCCESS:
                    continue
                if status == "running":
                    progress = self._recover_running_step(state, spec) or progress
                    if state["status"] in {"blocked", "failed"}:
                        return
                    if state["steps"][spec.id]["status"] in STEP_SUCCESS:
                        continue
                ready, failed_dependency = self._dependency_state(state, spec)
                if failed_dependency:
                    error = {
                        "message": f"依存 step が成功していません: {failed_dependency}",
                        "type": "blocked_dependency",
                        "dependency": failed_dependency,
                    }
                    self._mark_step(state, spec, "blocked", error=error)
                    state["status"] = "blocked"
                    state["error"] = error
                    self._save(state)
                    return
                if not ready:
                    continue
                if not self._condition_matches(spec.condition, state):
                    self._mark_step(state, spec, "skipped", reason="condition_false")
                    progress = True
                    self._save(state)
                    continue
                if status in {"failed", "blocked", "timed_out"}:
                    step_state["status"] = "pending"
                    step_state["error"] = None
                self._execute_step(state, spec)
                progress = True
                if state["status"] in {"blocked", "failed"}:
                    return
            if all(item["status"] in STEP_SUCCESS for item in state["steps"].values()):
                state["status"] = "completed"
                state["current_step"] = None
                self._append_event(state, "completed")
                self._save(state)
                return
            if not progress:
                error = {
                    "message": "依存関係を解決できない step が残っています",
                    "type": "workflow_deadlock",
                }
                state["status"] = "blocked"
                state["error"] = error
                self._append_event(state, "blocked", error=error)
                self._save(state)
                return

    def resume_run(
        self, run_id: str, workflow: Workflow | None = None
    ) -> dict[str, Any]:
        require_herdr_env(self.client.environ)
        with self.store.lock(run_id):
            state = self.store.load(run_id)
            workflow_value = state.get("workflow")
            if not isinstance(workflow_value, dict):
                raise FlowError("state に workflow がありません")
            workflow_path_value = workflow_value.get("path")
            stored_digest = workflow_value.get("digest")
            if (
                not isinstance(workflow_path_value, str)
                or not isinstance(stored_digest, str)
                or not stored_digest
            ):
                raise FlowError("state に workflow TOML digest がありません")
            workflow_path = Path(workflow_path_value)
            current_workflow = load_workflow(workflow_path)
            if current_workflow.digest != stored_digest:
                raise FlowError(
                    "workflow TOML が run 作成後に変更されています "
                    f"(state={stored_digest}, current={current_workflow.digest})"
                )
            resolved_workflow = workflow or current_workflow
            if resolved_workflow.digest != current_workflow.digest:
                raise FlowError("指定 workflow の digest が state と一致しません")
            if resolved_workflow.name != workflow_value.get("name"):
                raise FlowError("state の workflow と指定 workflow が一致しません")
            if state.get("status") == "completed":
                return state
            if state.get("status") == "cancelled":
                return state
            state["status"] = "running"
            self._append_event(state, "resumed")
            self._save(state)
            try:
                self._execute(state, resolved_workflow)
            except Exception as exc:  # noqa: BLE001 - persist terminal state on unexpected engine errors.
                error = _error_payload(exc)
                state["status"] = "failed"
                state["error"] = error
                self._append_event(state, "engine_failed", error=error)
                self._save(state)
            return state

    def cancel_run(self, run_id: str) -> dict[str, Any]:
        require_herdr_env(self.client.environ)
        state = self.store.load(run_id)
        if state.get("status") in RUN_TERMINAL:
            return state
        try:
            with self.store.lock(run_id):
                state = self.store.load(run_id)
                if state.get("status") in RUN_TERMINAL:
                    return state
                self._mark_cancelled(state)
                return state
        except StateError:
            self.store.request_cancel(run_id)
            state["status"] = "cancel_requested"
            state["message"] = "実行中プロセスにキャンセル要求を書き込みました"
            return state

    def cleanup(
        self,
        run_id: str | None = None,
        *,
        all_runs: bool = False,
        older_than_hours: float = 24.0,
        remove_worktree: bool = False,
    ) -> list[dict[str, Any]]:
        require_herdr_env(self.client.environ)
        if not run_id and not all_runs:
            raise FlowError("cleanup は --run-id または --all が必要です")
        cutoff = datetime.now(timezone.utc).timestamp() - older_than_hours * 3600
        candidate_ids: list[str] = []
        if run_id:
            candidate_ids = [run_id]
        else:
            for entry in self.store.list():
                if (
                    entry.get("status") in RUN_TERMINAL
                    and _now_epoch(entry.get("updated_at")) <= cutoff
                ):
                    candidate_ids.append(str(entry["run_id"]))

        cleaned: list[dict[str, Any]] = []
        for current_run_id in candidate_ids:
            with self.store.lock(current_run_id):
                state = self.store.load(current_run_id)
                current_status = state.get("status")
                if current_status not in RUN_TERMINAL:
                    raise FlowError(
                        f"active な run は cleanup できません: {current_run_id}"
                    )
                if not run_id and _now_epoch(state.get("updated_at")) > cutoff:
                    continue
                worktree_removed = False
                resources = state.get("resources", {})
                worktree = state.get("worktree", {})
                if (
                    remove_worktree
                    and isinstance(resources, dict)
                    and resources.get("worktree_created")
                ):
                    if not isinstance(worktree, dict):
                        raise FlowError("cleanup 対象 state の worktree が不正です")
                    path_value = worktree.get("path")
                    branch_value = worktree.get("branch")
                    repo_value = state.get("repo")
                    if (
                        not isinstance(path_value, str)
                        or not path_value
                        or not isinstance(branch_value, str)
                        or not branch_value
                        or not isinstance(repo_value, str)
                        or not repo_value
                    ):
                        raise FlowError(
                            "cleanup 対象 state に worktree path/branch/repo がありません"
                        )
                    repo = Path(repo_value).resolve()
                    payload = self.client.run_json(
                        ["worktree", "list", "--cwd", str(repo)],
                        timeout_seconds=120,
                    )
                    workspace_id = _current_worktree_workspace_id(
                        payload,
                        repo,
                        Path(path_value),
                        branch_value,
                    )
                    self.client.run_json(
                        [
                            "worktree",
                            "remove",
                            "--workspace",
                            workspace_id,
                            "--force",
                        ],
                        timeout_seconds=120,
                    )
                    worktree_removed = True
                self.store.remove(current_run_id)
                cleaned.append(
                    {
                        "run_id": current_run_id,
                        "status": "removed",
                        "worktree_removed": worktree_removed,
                    }
                )
        return cleaned


def dry_run_payload(
    repo: Path,
    workflow: Workflow,
    task: str,
    verify_commands: tuple[tuple[str, ...], ...],
    *,
    use_worktree: bool = True,
) -> dict[str, Any]:
    """Return a side-effect-free execution plan."""

    return {
        "dry_run": True,
        "repo": str(repo.resolve()),
        "task": task,
        "workflow": workflow.as_dict(),
        "verify_commands": [list(command) for command in verify_commands],
        "worktree": {
            "mode": "herdr worktree create"
            if use_worktree
            else "existing repo (explicit --no-worktree)",
            "destructive_integration": False,
        },
        "execution": [
            {
                "step_id": spec.id,
                "role": spec.role,
                "depends_on": list(spec.depends_on),
                "kind": spec.kind,
                "read_policy": spec.read_policy,
                "write_policy": spec.write_policy,
                "timeout_seconds": spec.timeout_seconds,
                "reuse_agent": spec.reuse_agent,
                "agent_args": list(spec.agent_args),
                "condition": spec.condition,
                "result_contract": spec.contract.as_dict(),
            }
            for spec in workflow.steps
        ],
    }
