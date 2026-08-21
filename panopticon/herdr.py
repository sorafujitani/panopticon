"""Small, JSON-first adapter for the Herdr 0.8.2 CLI."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
from collections.abc import Iterable
from contextlib import suppress
from dataclasses import dataclass
from pathlib import Path
from typing import Any


class HerdrError(RuntimeError):
    """Base error for Herdr integration failures."""


class CommandError(HerdrError):
    """A Herdr command returned a non-zero status or timed out."""

    def __init__(
        self,
        message: str,
        *,
        argv: tuple[str, ...],
        returncode: int | None = None,
        code: str = "command_failed",
        stdout: str = "",
        stderr: str = "",
    ) -> None:
        super().__init__(message)
        self.argv = argv
        self.returncode = returncode
        self.code = code
        self.stdout = stdout
        self.stderr = stderr


@dataclass(frozen=True)
class RawResult:
    stdout: str
    stderr: str
    returncode: int


_ID_KEYS: dict[str, tuple[str, ...]] = {
    "workspace": ("workspace_id", "workspaceid", "workspace"),
    "tab": ("tab_id", "tabid", "tab"),
    "pane": ("pane_id", "paneid", "pane", "root_pane", "rootpane", "root_pane_id"),
    "agent": ("agent_id", "agentid", "agent", "agent_name", "agentname"),
    "worktree": ("worktree_id", "worktreeid", "worktree"),
}

_ID_PREFIXES = {
    "workspace": ("w",),
    "tab": ("w",),
    "pane": ("w",),
    "worktree": ("w", "worktree", "wt"),
}
_PATH_KEYS = (
    "worktree_path",
    "worktreepath",
    "checkout_path",
    "checkoutpath",
    "path",
    "cwd",
    "working_directory",
    "workingdirectory",
)
_AGENT_STATUS_KEYS = ("agent_status", "agentstatus")
_GENERIC_STATUS_KEYS = ("status", "state", "lifecycle")
_STATUS_KEYS = _AGENT_STATUS_KEYS + _GENERIC_STATUS_KEYS
_AGENT_CONTAINER_KEYS = ("agent", "agent_info", "agentinfo")
_ERROR_KEYS = ("code", "error_code", "errorcode", "error", "type")


def require_herdr_env(environ: dict[str, str] | None = None) -> None:
    """Enforce Herdr's explicit managed-pane boundary."""

    env = environ if environ is not None else os.environ
    if env.get("HERDR_ENV") != "1":
        raise HerdrError("HERDR_ENV=1 の Herdr 管理ペイン内で実行してください")


def parse_json_output(text: str) -> Any | None:
    """Extract the last JSON document from noisy CLI output."""

    stripped = text.strip()
    if not stripped:
        return None
    try:
        return json.loads(stripped)
    except json.JSONDecodeError:
        decoder = json.JSONDecoder()
    documents: list[Any] = []
    for index, character in enumerate(text):
        if character not in "[{":
            continue
        try:
            value, _end = decoder.raw_decode(text[index:])
        except json.JSONDecodeError:
            continue
        documents.append(value)
    return documents[-1] if documents else None


def _normalise_key(key: str) -> str:
    return key.replace("-", "_").lower()


def _looks_like_id(value: Any, kind: str) -> bool:
    if not isinstance(value, str) or not value or value.startswith("cli:"):
        return False
    if kind == "agent":
        return True
    return any(value.startswith(prefix) for prefix in _ID_PREFIXES.get(kind, ()))


def _scalar_from_node(node: Any, kind: str, key_hint: str = "") -> str | None:
    if isinstance(node, str) and _looks_like_id(node, kind):
        return node
    if isinstance(node, dict):
        exact = set(_ID_KEYS.get(kind, ()))
        for key, value in node.items():
            normalised = _normalise_key(str(key))
            if normalised in exact:
                result = _scalar_from_node(value, kind, normalised)
                if result:
                    return result
        for key, value in node.items():
            if _normalise_key(str(key)) == "id":
                result = _scalar_from_node(value, kind, key_hint)
                if result:
                    return result
        for value in node.values():
            result = _scalar_from_node(value, kind, key_hint)
            if result:
                return result
    elif isinstance(node, list):
        for value in node:
            result = _scalar_from_node(value, kind, key_hint)
            if result:
                return result
    return None


def extract_id(payload: Any, kind: str) -> str | None:
    """Extract an opaque workspace/tab/pane id from any known response nesting.

    Herdr wraps responses in ``id`` and ``result`` fields, while resource
    objects use both ``*_id`` and nested ``id`` shapes.  This function scores
    semantic keys first and deliberately ignores the wrapper's ``cli:...`` id.
    """

    kind = kind.lower()
    if kind not in _ID_KEYS:
        raise ValueError(f"unknown Herdr id kind: {kind}")
    if isinstance(payload, dict):
        for key, value in payload.items():
            normalised = _normalise_key(str(key))
            if normalised in set(_ID_KEYS[kind]):
                result = _scalar_from_node(value, kind, normalised)
                if result:
                    return result
        for key, value in payload.items():
            if _normalise_key(str(key)) == "result":
                result = extract_id(value, kind)
                if result:
                    return result
        for value in payload.values():
            result = _scalar_from_node(value, kind)
            if result:
                return result
    return _scalar_from_node(payload, kind)


def _extract_by_keys(
    payload: Any, keys: Iterable[str], *, string_only: bool = True
) -> Any | None:
    wanted = {_normalise_key(key) for key in keys}
    if isinstance(payload, dict):
        for key, value in payload.items():
            if _normalise_key(str(key)) in wanted:
                if not string_only or isinstance(value, str):
                    return value
                nested = _extract_by_keys(value, keys, string_only=string_only)
                if nested is not None:
                    return nested
        for value in payload.values():
            nested = _extract_by_keys(value, keys, string_only=string_only)
            if nested is not None:
                return nested
    elif isinstance(payload, list):
        for value in payload:
            nested = _extract_by_keys(value, keys, string_only=string_only)
            if nested is not None:
                return nested
    return None


def extract_path(payload: Any) -> str | None:
    value = _extract_by_keys(payload, _PATH_KEYS)
    return value if isinstance(value, str) and value.strip() else None


def _normalise_status(value: Any) -> str | None:
    if not isinstance(value, str):
        return None
    normalised = value.strip().lower()
    return normalised or None


def extract_status(payload: Any) -> str | None:
    """Extract an agent lifecycle status without preferring CLI wrapper status."""

    status = _normalise_status(_extract_by_keys(payload, _AGENT_STATUS_KEYS))
    if status is not None:
        return status
    if isinstance(payload, dict):
        for key, value in payload.items():
            if _normalise_key(str(key)) in _AGENT_CONTAINER_KEYS:
                status = _normalise_status(_extract_by_keys(value, _STATUS_KEYS))
                if status is not None:
                    return status
        for key, value in payload.items():
            if _normalise_key(str(key)) in {"result", "response", "data", "payload"}:
                status = extract_status(value)
                if status is not None:
                    return status
    return _normalise_status(_extract_by_keys(payload, _GENERIC_STATUS_KEYS))


def extract_error_code(payload: Any) -> str | None:
    value = _extract_by_keys(payload, _ERROR_KEYS)
    if isinstance(value, str) and value.strip():
        return value.strip().lower().replace(" ", "_")
    if isinstance(value, dict):
        nested = _extract_by_keys(value, _ERROR_KEYS)
        if isinstance(nested, str):
            return nested.strip().lower().replace(" ", "_")
    return None


def _render_payload(text: str) -> str:
    return text.strip()[-4000:]


class HerdrClient:
    """Invoke Herdr with argv arrays and parse its JSON response."""

    def __init__(
        self, executable: str | None = None, environ: dict[str, str] | None = None
    ) -> None:
        configured = executable or os.environ.get("HERDR_BIN") or "herdr"
        self.executable = configured
        self.environ = dict(environ or os.environ)

    @property
    def resolved_executable(self) -> str:
        """Return an absolute executable path when Herdr can be resolved."""

        path = Path(self.executable)
        if path.is_file():
            return str(path.resolve())
        located = shutil.which(self.executable)
        return str(Path(located).resolve()) if located else self.executable

    def _argv(self, args: Iterable[str]) -> tuple[str, ...]:
        return (self.executable, *tuple(str(arg) for arg in args))

    def run_raw(self, args: Iterable[str], timeout_seconds: float = 30.0) -> RawResult:
        argv = self._argv(args)
        require_herdr_env(self.environ)
        available = (
            bool(shutil.which(self.executable)) or Path(self.executable).is_file()
        )
        if not available:
            raise HerdrError(f"herdr executable が見つかりません: {self.executable}")
        completed = None
        with suppress(FileNotFoundError), suppress(subprocess.TimeoutExpired):
            completed = subprocess.run(
                list(argv),
                cwd=None,
                env=self.environ,
                stdin=subprocess.DEVNULL,
                capture_output=True,
                text=True,
                check=False,
                timeout=timeout_seconds,
                shell=False,
            )
        if completed is None:
            raise CommandError(
                f"Herdr command がタイムアウトまたは起動失敗しました: {' '.join(argv)}",
                argv=argv,
                code="timeout_or_start_failed",
            )
        return RawResult(completed.stdout, completed.stderr, completed.returncode)

    def run_json(self, args: Iterable[str], timeout_seconds: float = 30.0) -> Any:
        argv = self._argv(args)
        result = self.run_raw(args, timeout_seconds=timeout_seconds)
        payload = parse_json_output(result.stdout) or parse_json_output(result.stderr)
        if result.returncode != 0:
            code = extract_error_code(payload) if payload is not None else None
            code = code or "command_failed"
            details = _render_payload(result.stderr or result.stdout)
            raise CommandError(
                f"Herdr command failed ({code}): {' '.join(argv)}\n{details}",
                argv=argv,
                returncode=result.returncode,
                code=code,
                stdout=result.stdout,
                stderr=result.stderr,
            )
        if payload is None:
            raise HerdrError(f"Herdr が JSON を返しませんでした: {' '.join(argv)}")
        return payload

    def run_text(self, args: Iterable[str], timeout_seconds: float = 30.0) -> str:
        argv = self._argv(args)
        result = self.run_raw(args, timeout_seconds=timeout_seconds)
        if result.returncode != 0:
            payload = parse_json_output(result.stderr) or parse_json_output(
                result.stdout
            )
            code = extract_error_code(payload) if payload is not None else None
            raise CommandError(
                f"Herdr command failed ({code or 'command_failed'}): {' '.join(argv)}\n"
                f"{_render_payload(result.stderr or result.stdout)}",
                argv=argv,
                returncode=result.returncode,
                code=code or "command_failed",
                stdout=result.stdout,
                stderr=result.stderr,
            )
        return result.stdout.strip()
