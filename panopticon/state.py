"""Atomic run-state persistence and a small per-run lock."""

from __future__ import annotations

import errno
import json
import os
import secrets
import tempfile
import time
from collections.abc import Iterator
from contextlib import contextmanager, suppress
from datetime import datetime, timezone
from pathlib import Path
from types import TracebackType
from typing import Any, Self


class StateError(RuntimeError):
    """Raised when run state cannot be loaded or safely updated."""


WAIT_TERMINAL = frozenset({"completed", "failed", "cancelled", "blocked"})
DEFAULT_WAIT_TIMEOUT_SECONDS = 3600.0
DEFAULT_WAIT_INTERVAL_SECONDS = 1.0

_COMPACT_MAX_STEPS = 32
_COMPACT_MAX_STEP_ID_LENGTH = 128
_COMPACT_MAX_STEP_TEXT_LENGTH = 1000
_COMPACT_MAX_RUN_ERROR_LENGTH = 3000
_COMPACT_MAX_PATH_LENGTH = 2000
_COMPACT_MAX_OUTPUT_BYTES = 12 * 1024
_COMPACT_ERROR_KEYS = ("type", "code", "message", "returncode", "stderr")
_COMPACT_ERROR_DETAIL_KEYS = ("stderr", "message")


def _safe_text(value: Any) -> str:
    try:
        text = str(value)
    except Exception:  # noqa: BLE001
        return "<unserializable>"
    return text.encode("utf-8", "replace").decode("utf-8")


def _json_text(value: Any) -> str:
    try:
        encoded = json.dumps(
            value,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
            default=_safe_text,
        )
    except (TypeError, ValueError, RecursionError, UnicodeError, OverflowError):
        try:
            encoded = json.dumps(
                value,
                ensure_ascii=False,
                separators=(",", ":"),
                sort_keys=False,
                default=_safe_text,
            )
        except (TypeError, ValueError, RecursionError, UnicodeError, OverflowError):
            return _safe_text(value)
    return _safe_text(encoded)


def _bounded_text(
    value: Any,
    maximum: int,
    *,
    stringify_non_strings_as_json: bool = False,
) -> str | None:
    if value is None:
        return None
    if isinstance(value, str):
        text = _safe_text(value)
    elif stringify_non_strings_as_json:
        text = _json_text(value)
    else:
        text = _safe_text(value)
    return text[:maximum]


def _compact_value(value: Any) -> str | None:
    return _bounded_text(
        value,
        _COMPACT_MAX_OUTPUT_BYTES,
        stringify_non_strings_as_json=True,
    )


def _bounded_error_returncode(value: Any, maximum: int) -> Any:
    if value is None:
        return None
    if isinstance(value, int) and not isinstance(value, bool):
        text = _safe_text(value)
        if text != "<unserializable>" and len(text) <= maximum:
            try:
                json.dumps(value, allow_nan=False)
            except (TypeError, ValueError, RecursionError, UnicodeError, OverflowError):
                return text[:maximum]
            return value
        return text[:maximum]
    return _bounded_text(value, maximum, stringify_non_strings_as_json=True)


def _bounded_error_text(value: Any, maximum: int) -> str | None:
    if value is None or isinstance(value, str):
        return _bounded_text(value, maximum)
    if isinstance(value, (dict, list, tuple)):
        return _bounded_text(value, maximum, stringify_non_strings_as_json=True)
    return _bounded_text(value, maximum)


def _compact_error(value: Any, maximum: int) -> str | dict[str, Any] | None:
    if value is None:
        return None
    if isinstance(value, str):
        return _safe_text(value)[:maximum]
    if not isinstance(value, dict):
        return _bounded_text(value, maximum, stringify_non_strings_as_json=True)

    compact: dict[str, Any] = {}
    for key in _COMPACT_ERROR_KEYS:
        if key not in value:
            continue
        if key == "returncode":
            compact[key] = _bounded_error_returncode(value[key], maximum)
        else:
            compact[key] = _bounded_error_text(value[key], maximum)
    if compact:
        return compact
    return {
        "message": _bounded_text(value, maximum, stringify_non_strings_as_json=True)
    }


def _compact_json_size(payload: dict[str, Any]) -> int:
    try:
        encoded = json.dumps(
            payload,
            ensure_ascii=False,
            indent=2,
            sort_keys=True,
            allow_nan=False,
        ).encode("utf-8")
    except (TypeError, ValueError, RecursionError, UnicodeError, OverflowError):
        return _COMPACT_MAX_OUTPUT_BYTES + 1
    # _json_print_compact adds a trailing newline to this document.
    return len(encoded) + 1


def _shrink_text_field(container: dict[str, Any], field: str) -> bool:
    value = container.get(field)
    if isinstance(value, str):
        if not value:
            return False
        container[field] = value[: len(value) // 2]
        return True
    if field == "error" and isinstance(value, dict):
        for nested_field in _COMPACT_ERROR_DETAIL_KEYS:
            if _shrink_text_field(value, nested_field):
                return True
    return False


def _shrink_step_fields(payload: dict[str, Any], fields: tuple[str, ...]) -> bool:
    changed = False
    for step in payload["steps"].values():
        for field in fields:
            changed |= _shrink_text_field(step, field)
    return changed


def _shrink_top_fields(payload: dict[str, Any], fields: tuple[str, ...]) -> bool:
    changed = False
    for field in fields:
        changed |= _shrink_text_field(payload, field)
    return changed


def _shrink_step_ids(payload: dict[str, Any]) -> bool:
    steps = payload["steps"]
    shortened: dict[str, dict[str, Any]] = {}
    changed = False
    for step_id, step in steps.items():
        shortened_id = step_id[: max(1, len(step_id) // 2)]
        if shortened_id != step_id or shortened_id in shortened:
            changed = True
        if shortened_id not in shortened:
            shortened[shortened_id] = step
    if changed:
        payload["steps"] = shortened
    return changed


def _fit_compact_payload(payload: dict[str, Any]) -> dict[str, Any]:
    while _compact_json_size(payload) > _COMPACT_MAX_OUTPUT_BYTES:
        if _shrink_step_fields(payload, ("summary", "error")):
            continue
        if _shrink_top_fields(
            payload, ("error", "repo", "worktree", "current_step", "updated_at")
        ):
            continue
        if _shrink_step_fields(payload, ("status",)):
            continue
        if _shrink_step_ids(payload):
            continue
        if payload["steps"]:
            payload["steps"].popitem()
            continue
        if _shrink_top_fields(payload, ("run_id", "status")):
            continue
        break
    return payload


def compact_state(state: dict[str, Any]) -> dict[str, Any]:
    """Return the bounded run snapshot intended for automation consumers."""

    worktree = state.get("worktree")
    worktree_path = worktree.get("path") if isinstance(worktree, dict) else worktree
    steps: dict[str, dict[str, Any]] = {}
    raw_steps = state.get("steps")
    if isinstance(raw_steps, dict):
        for step_id, raw_step in raw_steps.items():
            if len(steps) >= _COMPACT_MAX_STEPS:
                break
            if not isinstance(raw_step, dict):
                continue
            result = raw_step.get("result")
            summary = (
                result.get("summary")
                if isinstance(result, dict)
                else raw_step.get("summary")
            )
            steps[_safe_text(step_id)[:_COMPACT_MAX_STEP_ID_LENGTH]] = {
                "status": _compact_value(raw_step.get("status")),
                "summary": _bounded_text(
                    summary,
                    _COMPACT_MAX_STEP_TEXT_LENGTH,
                    stringify_non_strings_as_json=True,
                ),
                "error": _compact_error(
                    raw_step.get("error"), _COMPACT_MAX_STEP_TEXT_LENGTH
                ),
            }
    payload = {
        "run_id": _compact_value(state.get("run_id")),
        "status": _compact_value(state.get("status")),
        "current_step": _compact_value(state.get("current_step")),
        "repo": _bounded_text(
            state.get("repo"),
            _COMPACT_MAX_PATH_LENGTH,
        ),
        "worktree": _bounded_text(worktree_path, _COMPACT_MAX_PATH_LENGTH),
        "steps": steps,
        "error": _compact_error(state.get("error"), _COMPACT_MAX_RUN_ERROR_LENGTH),
        "updated_at": _compact_value(state.get("updated_at")),
    }
    return _fit_compact_payload(payload)


def wait_for_terminal(
    store: RunStore,
    run_id: str,
    *,
    timeout_seconds: float = DEFAULT_WAIT_TIMEOUT_SECONDS,
    interval_seconds: float = DEFAULT_WAIT_INTERVAL_SECONDS,
) -> tuple[dict[str, Any], bool]:
    """Poll complete atomic state.json snapshots until terminal or timeout."""

    deadline = time.monotonic() + timeout_seconds
    while True:
        state = store.load(run_id)
        if state.get("status") in WAIT_TERMINAL:
            return state, False
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return state, True
        time.sleep(min(interval_seconds, remaining))


def utc_now() -> str:
    return (
        datetime.now(timezone.utc)
        .replace(microsecond=0)
        .isoformat()
        .replace("+00:00", "Z")
    )


def make_run_id() -> str:
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    return f"run-{timestamp}-{secrets.token_hex(4)}"


def _fsync_directory(directory: Path) -> None:
    """Best-effort directory fsync for POSIX atomic rename durability."""

    try:
        fd = os.open(directory, os.O_RDONLY)
    except OSError:
        return
    with suppress(OSError):
        os.fsync(fd)
    os.close(fd)


def atomic_write_json(path: Path, payload: Any) -> None:
    """Write JSON by fsyncing a same-directory temporary file then replacing it."""

    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with suppress(OSError):
        os.chmod(path.parent, 0o700)
    temporary: str | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            dir=path.parent,
            prefix=f".{path.name}.",
            suffix=".tmp",
            delete=False,
        ) as handle:
            temporary = handle.name
            json.dump(payload, handle, ensure_ascii=False, indent=2, sort_keys=True)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        temporary = None
        with suppress(OSError):
            os.chmod(path, 0o600)
        _fsync_directory(path.parent)
    finally:
        if temporary:
            with suppress(FileNotFoundError):
                os.unlink(temporary)


def read_json(path: Path) -> Any:
    try:
        with Path(path).open(encoding="utf-8") as handle:
            return json.load(handle)
    except FileNotFoundError as exc:
        raise StateError(f"state が見つかりません: {path}") from exc
    except json.JSONDecodeError as exc:
        raise StateError(f"JSON state が壊れています: {path}: {exc}") from exc


class RunLock:
    """An O_EXCL lock that prevents two resume processes mutating one run."""

    def __init__(self, path: Path) -> None:
        self.path = Path(path)
        self._held = False

    @staticmethod
    def _pid_alive(pid: int) -> bool:
        if pid <= 0:
            return False
        try:
            os.kill(pid, 0)
        except OSError as exc:
            return exc.errno == errno.EPERM
        return True

    def acquire(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        payload = {"pid": os.getpid(), "created_at": utc_now()}
        for _ in range(2):
            fd: int | None = None
            with suppress(FileExistsError):
                fd = os.open(self.path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            if fd is None:
                try:
                    existing = read_json(self.path)
                    pid = (
                        int(existing.get("pid", 0)) if isinstance(existing, dict) else 0
                    )
                except (StateError, TypeError, ValueError):
                    pid = 0
                if pid and self._pid_alive(pid):
                    raise StateError(
                        f"run は別プロセスが実行中です (pid={pid})"
                    ) from None
                with suppress(FileNotFoundError):
                    self.path.unlink()
                continue
            try:
                os.write(fd, json.dumps(payload).encode("utf-8"))
            finally:
                os.close(fd)
            self._held = True
            return
        raise StateError(f"run lock を取得できません: {self.path}")

    def release(self) -> None:
        if not self._held:
            return
        with suppress(FileNotFoundError):
            self.path.unlink()
        self._held = False

    def __enter__(self) -> Self:
        self.acquire()
        return self

    def __exit__(
        self,
        _exc_type: type[BaseException] | None,
        _exc_value: BaseException | None,
        _traceback: TracebackType | None,
    ) -> None:
        self.release()


class RunStore:
    """Filesystem-backed run store rooted at ``~/.local/state/panopticon/runs``."""

    def __init__(self, root: Path | None = None) -> None:
        configured = os.environ.get("PANOPTICON_STATE_DIR")
        base = Path(
            root
            or configured
            or Path.home() / ".local" / "state" / "panopticon" / "runs"
        )
        self.root = base.expanduser().resolve()
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        with suppress(OSError):
            os.chmod(self.root, 0o700)

    def run_dir(self, run_id: str) -> Path:
        if not run_id or Path(run_id).name != run_id or run_id in {".", ".."}:
            raise StateError(f"run id が不正です: {run_id!r}")
        return self.root / run_id

    def state_path(self, run_id: str) -> Path:
        return self.run_dir(run_id) / "state.json"

    def lock(self, run_id: str) -> RunLock:
        return RunLock(self.run_dir(run_id) / "run.lock")

    def create(self, run_id: str, state: dict[str, Any]) -> Path:
        directory = self.run_dir(run_id)
        try:
            directory.mkdir(mode=0o700)
        except FileExistsError as exc:
            raise StateError(f"run id が既に存在します: {run_id}") from exc
        (directory / "steps").mkdir(mode=0o700)
        atomic_write_json(directory / "state.json", state)
        return directory

    def load(self, run_id: str) -> dict[str, Any]:
        value = read_json(self.state_path(run_id))
        if not isinstance(value, dict):
            raise StateError(f"state のトップレベルが object ではありません: {run_id}")
        return value

    def save(self, run_id: str, state: dict[str, Any]) -> None:
        state["updated_at"] = utc_now()
        atomic_write_json(self.state_path(run_id), state)

    def list(self) -> list[dict[str, Any]]:
        entries: list[dict[str, Any]] = []
        if not self.root.exists():
            return entries
        for directory in sorted(self.root.iterdir(), key=lambda item: item.name):
            if not directory.is_dir() or directory.name.startswith("."):
                continue
            path = directory / "state.json"
            if not path.exists():
                continue
            try:
                state = read_json(path)
            except StateError as exc:
                entries.append(
                    {
                        "run_id": directory.name,
                        "status": "corrupt",
                        "error": str(exc),
                    }
                )
                continue
            if isinstance(state, dict):
                entries.append(
                    {
                        "run_id": state.get("run_id", directory.name),
                        "status": state.get("status", "unknown"),
                        "workflow": state.get("workflow", {}).get("name"),
                        "task": state.get("task"),
                        "created_at": state.get("created_at"),
                        "updated_at": state.get("updated_at"),
                        "current_step": state.get("current_step"),
                    }
                )
        entries.sort(
            key=lambda item: item.get("updated_at") or item.get("created_at") or "",
            reverse=True,
        )
        return entries

    def request_cancel(self, run_id: str) -> Path:
        path = self.run_dir(run_id) / "cancel.request"
        atomic_write_json(path, {"requested_at": utc_now(), "pid": os.getpid()})
        return path

    def cancel_requested(self, run_id: str) -> bool:
        return (self.run_dir(run_id) / "cancel.request").exists()

    def clear_cancel_request(self, run_id: str) -> None:
        with suppress(FileNotFoundError):
            (self.run_dir(run_id) / "cancel.request").unlink()

    def remove(self, run_id: str) -> None:
        directory = self.run_dir(run_id)
        if not directory.is_dir():
            raise StateError(f"run が見つかりません: {run_id}")
        import shutil

        try:
            shutil.rmtree(directory)
        except OSError as exc:
            raise StateError(f"run state を削除できません: {directory}: {exc}") from exc


@contextmanager
def locked_store(store: RunStore, run_id: str) -> Iterator[None]:
    """Convenience context manager used by mutating commands."""

    with store.lock(run_id):
        yield
