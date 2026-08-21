"""Workflow and repository configuration loading.

The module intentionally uses only :mod:`tomllib` and small dataclasses.  A
workflow is validated before a run directory or a Herdr resource is created.
"""

from __future__ import annotations

import hashlib
import re
from collections.abc import Iterable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import tomllib


class WorkflowError(ValueError):
    """Raised when a workflow or repository configuration is invalid."""


_ID_RE = re.compile(r"^[a-z][a-z0-9_-]{0,47}$")
_POLICY_VALUES = {"none", "worktree", "repo-and-dependencies"}


@dataclass(frozen=True)
class Contract:
    """The JSON contract expected from one step."""

    required_fields: tuple[str, ...]
    artifact_kinds: tuple[str, ...]
    required_boolean_fields: tuple[str, ...] = ()
    required_list_fields: tuple[str, ...] = ()

    def as_dict(self) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "required_fields": list(self.required_fields),
            "artifact_kinds": list(self.artifact_kinds),
            "required_boolean_fields": list(self.required_boolean_fields),
            "required_list_fields": list(self.required_list_fields),
        }


@dataclass(frozen=True)
class StepSpec:
    """A validated workflow step."""

    id: str
    role: str
    kind: str
    depends_on: tuple[str, ...]
    read_policy: str
    write_policy: str
    timeout_seconds: int
    template: Path
    contract: Contract
    condition: str | None = None
    reuse_agent: str | None = None
    submit_key: str | None = None
    agent_args: tuple[str, ...] = ()

    @property
    def timeout_ms(self) -> int:
        return self.timeout_seconds * 1000


@dataclass(frozen=True)
class Workflow:
    """A validated, executable workflow."""

    name: str
    version: int
    path: Path
    steps: tuple[StepSpec, ...]
    default_verify: tuple[tuple[str, ...], ...]
    digest: str = ""

    @property
    def step_map(self) -> dict[str, StepSpec]:
        return {step.id: step for step in self.steps}

    def as_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "version": self.version,
            "path": str(self.path),
            "digest": self.digest,
            "default_verify": [list(command) for command in self.default_verify],
            "steps": [
                {
                    "id": step.id,
                    "role": step.role,
                    "kind": step.kind,
                    "depends_on": list(step.depends_on),
                    "read_policy": step.read_policy,
                    "write_policy": step.write_policy,
                    "timeout_seconds": step.timeout_seconds,
                    "template": str(step.template),
                    "condition": step.condition,
                    "reuse_agent": step.reuse_agent,
                    "submit_key": step.submit_key,
                    "agent_args": list(step.agent_args),
                    "contract": step.contract.as_dict(),
                }
                for step in self.steps
            ],
        }


SUPPORTED_AGENT_KINDS = {
    "pi",
    "claude",
    "codex",
    "gemini",
    "cursor",
    "devin",
    "agy",
    "cline",
    "omp",
    "mastracode",
    "opencode",
    "copilot",
    "kimi",
    "kiro",
    "droid",
    "amp",
    "grok",
    "hermes",
    "kilo",
    "qodercli",
    "qwen",
    "maki",
}


def _as_string(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise WorkflowError(f"{label} は空でない文字列で指定してください")
    return value.strip()


def _as_string_list(value: Any, label: str) -> tuple[str, ...]:
    if value is None:
        return ()
    if not isinstance(value, list) or any(not isinstance(item, str) for item in value):
        raise WorkflowError(f"{label} は文字列配列で指定してください")
    return tuple(item for item in value if item.strip())


def _as_agent_args(value: Any, label: str) -> tuple[str, ...]:
    if value is None:
        return ()
    if not isinstance(value, list) or any(
        not isinstance(item, str) or not item.strip() for item in value
    ):
        raise WorkflowError(f"{label} は空でない文字列配列で指定してください")
    return tuple(value)


def _parse_command_list(value: Any, label: str) -> tuple[tuple[str, ...], ...]:
    """Parse commands represented as ``[["python", "-m", ...], ...]``."""

    if value is None:
        return ()
    if not isinstance(value, list):
        raise WorkflowError(f"{label} はコマンド配列の配列で指定してください")
    commands: list[tuple[str, ...]] = []
    for index, command in enumerate(value):
        if (
            not isinstance(command, list)
            or not command
            or any(not isinstance(token, str) or not token for token in command)
        ):
            raise WorkflowError(
                f"{label}[{index}] は空でない文字列配列で指定してください"
            )
        commands.append(tuple(command))
    return tuple(commands)


def _resolve_template(workflow_path: Path, template_value: Any) -> Path:
    template = Path(_as_string(template_value, "steps[].template"))
    if template.is_absolute():
        resolved = template.resolve()
    else:
        resolved = (workflow_path.parent / template).resolve()
    if not resolved.is_file():
        raise WorkflowError(f"prompt template が見つかりません: {resolved}")
    return resolved


def _parse_contract(raw: Any, step_id: str) -> Contract:
    if not isinstance(raw, dict):
        raise WorkflowError(f"step {step_id}: contract table が必要です")
    required = _as_string_list(
        raw.get("required_fields"), f"step {step_id}.contract.required_fields"
    )
    if not required:
        raise WorkflowError(f"step {step_id}: contract.required_fields は必須です")
    artifact_kinds = _as_string_list(
        raw.get("artifact_kinds"), f"step {step_id}.contract.artifact_kinds"
    )
    if not artifact_kinds:
        raise WorkflowError(f"step {step_id}: artifact_kinds は1つ以上必要です")
    return Contract(
        required_fields=required,
        artifact_kinds=artifact_kinds,
        required_boolean_fields=_as_string_list(
            raw.get("required_boolean_fields"),
            f"step {step_id}.contract.required_boolean_fields",
        ),
        required_list_fields=_as_string_list(
            raw.get("required_list_fields"),
            f"step {step_id}.contract.required_list_fields",
        ),
    )


def _parse_step(raw: Any, workflow_path: Path) -> StepSpec:
    if not isinstance(raw, dict):
        raise WorkflowError("steps は table の配列で指定してください")
    step_id = _as_string(raw.get("id"), "steps[].id")
    if not _ID_RE.fullmatch(step_id):
        raise WorkflowError(f"step id が不正です: {step_id!r}")
    role = _as_string(raw.get("role"), f"step {step_id}.role")
    kind = _as_string(raw.get("kind"), f"step {step_id}.kind").lower()
    if kind not in SUPPORTED_AGENT_KINDS:
        raise WorkflowError(f"step {step_id}: 未対応の agent kind: {kind}")

    depends_on = _as_string_list(
        raw.get("depends_on", []), f"step {step_id}.depends_on"
    )
    if step_id in depends_on:
        raise WorkflowError(f"step {step_id}: 自分自身には依存できません")
    read_policy = _as_string(raw.get("read_policy"), f"step {step_id}.read_policy")
    write_policy = _as_string(raw.get("write_policy"), f"step {step_id}.write_policy")
    if read_policy not in _POLICY_VALUES:
        raise WorkflowError(f"step {step_id}: read_policy が不正です: {read_policy}")
    if write_policy not in _POLICY_VALUES:
        raise WorkflowError(f"step {step_id}: write_policy が不正です: {write_policy}")
    timeout = raw.get("timeout_seconds")
    if (
        isinstance(timeout, bool)
        or not isinstance(timeout, int)
        or not 1 <= timeout <= 86_400
    ):
        raise WorkflowError(f"step {step_id}: timeout_seconds は1〜86400の整数です")
    condition_value = raw.get("condition")
    condition = (
        None
        if condition_value in (None, "")
        else _as_string(condition_value, f"step {step_id}.condition")
    )
    reuse_value = raw.get("reuse_agent")
    reuse_agent = (
        None
        if reuse_value in (None, "")
        else _as_string(reuse_value, f"step {step_id}.reuse_agent")
    )
    submit_key_value = raw.get("submit_key")
    submit_key = (
        None
        if submit_key_value in (None, "")
        else _as_string(submit_key_value, f"step {step_id}.submit_key")
    )
    agent_args = _as_agent_args(raw.get("agent_args"), f"step {step_id}.agent_args")
    return StepSpec(
        id=step_id,
        role=role,
        kind=kind,
        depends_on=depends_on,
        read_policy=read_policy,
        write_policy=write_policy,
        timeout_seconds=timeout,
        template=_resolve_template(workflow_path, raw.get("template")),
        contract=_parse_contract(raw.get("contract"), step_id),
        condition=condition,
        reuse_agent=reuse_agent,
        submit_key=submit_key,
        agent_args=agent_args,
    )


def _validate_graph(steps: tuple[StepSpec, ...]) -> None:
    by_id = {step.id: step for step in steps}
    if len(by_id) != len(steps):
        raise WorkflowError("step id が重複しています")
    for step in steps:
        for dependency in step.depends_on:
            if dependency not in by_id:
                raise WorkflowError(f"step {step.id}: 未定義の依存先: {dependency}")
        if step.reuse_agent and step.reuse_agent not in by_id:
            raise WorkflowError(
                f"step {step.id}: reuse_agent の参照先がありません: {step.reuse_agent}"
            )

    visiting: set[str] = set()
    visited: set[str] = set()

    def visit(step_id: str) -> None:
        if step_id in visiting:
            raise WorkflowError(f"workflow に循環依存があります: {step_id}")
        if step_id in visited:
            return
        visiting.add(step_id)
        for dependency in by_id[step_id].depends_on:
            visit(dependency)
        visiting.remove(step_id)
        visited.add(step_id)

    for step in steps:
        visit(step.id)


def _workflow_data(raw: dict[str, Any]) -> dict[str, Any]:
    nested = raw.get("workflow")
    if nested is None:
        return raw
    if not isinstance(nested, dict):
        raise WorkflowError("[workflow] は table で指定してください")
    merged = dict(raw)
    merged.update(nested)
    return merged


def load_workflow(path: Path) -> Workflow:
    """Load and validate one workflow TOML file."""

    path = path.resolve()
    try:
        content = path.read_bytes()
        raw = tomllib.loads(content.decode("utf-8"))
    except FileNotFoundError as exc:
        raise WorkflowError(f"workflow が見つかりません: {path}") from exc
    except UnicodeDecodeError as exc:
        raise WorkflowError(f"workflow TOML が UTF-8 ではありません ({path})") from exc
    except tomllib.TOMLDecodeError as exc:
        raise WorkflowError(f"workflow TOML が不正です ({path}): {exc}") from exc
    if not isinstance(raw, dict):
        raise WorkflowError(f"workflow のトップレベルが不正です: {path}")
    data = _workflow_data(raw)
    version = data.get("version", 1)
    if version != 1:
        raise WorkflowError(f"未対応の workflow version: {version}")
    name = _as_string(data.get("name", path.stem), "workflow.name")
    raw_steps = data.get("steps")
    if not isinstance(raw_steps, list) or not raw_steps:
        raise WorkflowError("workflow.steps は1つ以上の table 配列が必要です")
    steps = tuple(_parse_step(raw_step, path) for raw_step in raw_steps)
    _validate_graph(steps)
    default_verify = _parse_command_list(data.get("default_verify"), "default_verify")
    if not default_verify:
        raise WorkflowError("default_verify は1つ以上必要です")
    return Workflow(
        name=name,
        version=version,
        path=path,
        steps=steps,
        default_verify=default_verify,
        digest=hashlib.sha256(content).hexdigest(),
    )


def load_repo_config(repo: Path) -> dict[str, Any]:
    """Load optional ``.panopticon.toml`` from a repository."""

    path = repo / ".panopticon.toml"
    if not path.is_file():
        return {}
    try:
        with path.open("rb") as handle:
            raw = tomllib.load(handle)
    except tomllib.TOMLDecodeError as exc:
        raise WorkflowError(f"repo の .panopticon.toml が不正です: {exc}") from exc
    if not isinstance(raw, dict):
        raise WorkflowError("repo の .panopticon.toml のトップレベルが不正です")
    return raw


def _configured_workflow_name(config: dict[str, Any]) -> str | None:
    for container in (config, config.get("flow"), config.get("workflow")):
        if isinstance(container, dict):
            for key in ("workflow", "name"):
                value = container.get(key)
                if isinstance(value, str) and value.strip():
                    return value.strip()
    return None


def resolve_workflow_path(
    repo: Path,
    requested: str | None,
    package_root: Path,
    repo_config: dict[str, Any] | None = None,
) -> Path:
    """Resolve a workflow name/path without importing project code."""

    repo = repo.resolve()
    package_root = package_root.resolve()
    configured = _configured_workflow_name(repo_config or {})
    requested = requested or configured or "standard"
    requested_path = Path(requested).expanduser()
    candidates: list[Path] = []
    if requested_path.is_absolute():
        candidates.append(requested_path)
    elif requested_path.suffix == ".toml" or "/" in requested or "\\" in requested:
        candidates.extend(
            [
                repo / requested_path,
                repo / ".panopticon" / requested_path,
                package_root / requested_path,
            ]
        )
        if requested_path.parent == Path("."):
            candidates.extend(
                [
                    repo / "workflows" / requested_path,
                    repo / ".panopticon" / "workflows" / requested_path,
                    package_root / "workflows" / requested_path,
                ]
            )
    else:
        candidates.extend(
            [
                repo / "workflows" / f"{requested}.toml",
                repo / ".panopticon" / "workflows" / f"{requested}.toml",
                package_root / "workflows" / f"{requested}.toml",
            ]
        )
    for candidate in candidates:
        if candidate.is_file():
            return candidate.resolve()
    rendered = ", ".join(str(candidate) for candidate in candidates)
    raise WorkflowError(f"workflow '{requested}' が見つかりません。検索先: {rendered}")


def _commands_from_config(config: dict[str, Any]) -> tuple[tuple[str, ...], ...]:
    candidates: list[Any] = []
    for container in (config.get("verification"), config.get("verify")):
        if isinstance(container, dict):
            candidates.extend(container.get(key) for key in ("commands", "command"))
    candidates.extend(
        config.get(key) for key in ("verification_commands", "verify_commands")
    )
    for value in candidates:
        if value is not None:
            return _parse_command_list(value, "repo verification")
    return ()


def resolve_verify_commands(
    cli_values: Iterable[str] | None,
    repo_config: dict[str, Any],
    workflow: Workflow,
) -> tuple[tuple[str, ...], ...]:
    """Resolve verification commands in CLI > repo config > workflow order."""

    cli_values = tuple(cli_values or ())
    if cli_values:
        import shlex

        commands: list[tuple[str, ...]] = []
        for value in cli_values:
            try:
                tokens = tuple(shlex.split(value, posix=True))
            except ValueError as exc:
                raise WorkflowError(f"--verify の引用が不正です: {exc}") from exc
            if not tokens:
                raise WorkflowError("--verify に空のコマンドは指定できません")
            commands.append(tokens)
        return tuple(commands)
    configured = _commands_from_config(repo_config)
    return configured or workflow.default_verify
