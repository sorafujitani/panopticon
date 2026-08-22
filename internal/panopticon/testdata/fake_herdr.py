#!/usr/bin/env python3
"""A tiny Herdr CLI double used by the integration tests."""

from __future__ import annotations

import json
import os
import re
import sys
from pathlib import Path
from typing import Any


def _value(args: list[str], flag: str, default: str | None = None) -> str | None:
    try:
        return args[args.index(flag) + 1]
    except (ValueError, IndexError):
        return default


def _log(args: list[str]) -> None:
    path_value = os.environ.get("FAKE_HERDR_LOG")
    if not path_value:
        return
    path = Path(path_value)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        json.dump(args, handle, ensure_ascii=False)
        handle.write("\n")


def _emit(payload: Any) -> int:
    print(json.dumps(payload, ensure_ascii=False))
    return 0


def _prompt_value(prompt: str, prefix: str) -> str | None:
    for line in prompt.splitlines():
        if line.startswith(prefix):
            return line.removeprefix(prefix).strip()
    return None


def _prompt_json(prompt: str, marker: str) -> Any | None:
    start = prompt.find(marker)
    if start < 0:
        return None
    try:
        value, _end = json.JSONDecoder().raw_decode(
            prompt[start + len(marker) :].lstrip()
        )
    except json.JSONDecodeError:
        return None
    return value


def _contract_kind(prompt: str) -> str:
    marker = prompt.find("## JSON contract")
    start = prompt.find("{", marker)
    if start >= 0:
        try:
            contract, _ = json.JSONDecoder().raw_decode(prompt[start:])
        except json.JSONDecodeError:
            contract = {}
        if isinstance(contract, dict):
            artifacts = contract.get("artifacts")
            if isinstance(artifacts, list) and artifacts:
                kind = (
                    artifacts[0].get("kind") if isinstance(artifacts[0], dict) else None
                )
                if isinstance(kind, str):
                    return kind.removeprefix("one of: ").split(",", 1)[0].strip()
    return "report"


def _result(prompt: str) -> dict[str, Any]:
    run_id_match = re.search(r"- run_id: `([^`]+)`", prompt)
    step_id_match = re.search(r"- step_id: `([^`]+)`", prompt)
    role_match = re.search(r"- role: `([^`]+)`", prompt)
    result_path_value = _prompt_value(prompt, "RESULT_PATH=")
    worktree_value = _prompt_value(prompt, "- Dedicated worktree only: ")
    if worktree_value:
        worktree_value = worktree_value.strip("`")
    if run_id_match is None or step_id_match is None:
        raise RuntimeError("fake Herdr cannot parse the prompt's fixed boundaries")
    if result_path_value is None:
        raise RuntimeError("fake Herdr cannot parse RESULT_PATH")
    role_value = role_match.group(1) if role_match is not None else step_id_match.group(1)

    if role_value == "controller":
        controller_changed_file = os.environ.get("FAKE_HERDR_CONTROLLER_CHANGED_FILE")
        controller_worktree = _prompt_value(prompt, "- Worktree: ")
        if controller_changed_file and controller_worktree:
            relative = Path(controller_changed_file)
            if not relative.is_absolute() and relative.parts and ".." not in relative.parts:
                target = Path(controller_worktree.strip("`")) / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text("fake controller change\n", encoding="utf-8")
        context = _prompt_json(prompt, "CONTROL_CONTEXT=")
        eligible = _prompt_json(prompt, "ELIGIBLE_NEXT_STEPS=")
        allowed = _prompt_json(prompt, "ALLOWED_ACTIONS=")
        completion = context.get("completion", {}) if isinstance(context, dict) else {}
        action = os.environ.get("FAKE_HERDR_CONTROLLER_ACTION")
        if completion.get("status") in {"failed", "blocked", "timed_out"}:
            action = os.environ.get("FAKE_HERDR_CONTROLLER_ACTION_FAILED", action)
        if not action:
            action = allowed[0] if isinstance(allowed, list) and allowed else "fail"
        next_step = os.environ.get("FAKE_HERDR_CONTROLLER_NEXT_STEP")
        if next_step is None and action == "retry":
            next_step = completion.get("step_id")
        elif next_step is None and action == "continue" and isinstance(eligible, list) and eligible:
            next_step = eligible[0]
        return {
            "schema_version": 1,
            "run_id": run_id_match.group(1),
            "step_id": "controller",
            "role": "controller",
            "observed_step": completion.get("step_id", "__start__"),
            "observed_attempt": completion.get("attempt", 0),
            "action": action,
            "next_step": next_step,
            "reason": os.environ.get("FAKE_HERDR_CONTROLLER_REASON", f"fake {action}"),
            "user_summary": f"fake controller {action}",
        }

    worktree_prompt_value = _prompt_value(prompt, "- worktree: ")
    if worktree_prompt_value:
        worktree_value = worktree_prompt_value.strip("`")
    artifact_root = Path(result_path_value).parent
    artifact_root.mkdir(parents=True, exist_ok=True)
    artifact = artifact_root / f"fake-{step_id_match.group(1)}.txt"
    artifact.write_text("fake artifact\n", encoding="utf-8")

    changed_files: list[str] = []
    role_key = role_value.upper()
    changed_value = os.environ.get(
        f"FAKE_HERDR_CHANGED_FILES_{role_key}",
        os.environ.get("FAKE_HERDR_CHANGED_FILES"),
    )
    if changed_value:
        try:
            parsed = json.loads(changed_value)
        except json.JSONDecodeError:
            parsed = [changed_value]
        changed_files = parsed if isinstance(parsed, list) else [changed_value]
    actual_value = os.environ.get(
        f"FAKE_HERDR_ACTUAL_CHANGED_FILES_{role_key}",
        os.environ.get("FAKE_HERDR_ACTUAL_CHANGED_FILES", changed_value or ""),
    )
    actual_files: list[str] = []
    if actual_value:
        try:
            actual_parsed = json.loads(actual_value)
        except json.JSONDecodeError:
            actual_parsed = [actual_value]
        actual_files = (
            actual_parsed if isinstance(actual_parsed, list) else [actual_value]
        )
    if worktree_value:
        worktree_path = Path(worktree_value)
        for changed_file in actual_files:
            if not isinstance(changed_file, str):
                continue
            relative = Path(changed_file)
            if relative.is_absolute() or not relative.parts or ".." in relative.parts:
                continue
            target = worktree_path / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text("fake change\n", encoding="utf-8")

    status = os.environ.get("FAKE_HERDR_MODE", "success")
    if status not in {"success", "blocked", "failed"}:
        status = "success"
    verified = True
    if role_value == "verifier":
        override = os.environ.get("FAKE_HERDR_VERIFIED")
        if override is not None:
            verified = override.lower() == "true"
        else:
            engine_verification = _prompt_json(prompt, "ENGINE_VERIFICATION=")
            verified = bool(
                isinstance(engine_verification, dict)
                and engine_verification.get("all_succeeded")
            )
    return {
        "schema_version": 1,
        "run_id": run_id_match.group(1),
        "step_id": step_id_match.group(1),
        "role": role_value,
        "status": status,
        "summary": f"fake {status}",
        "artifacts": [
            {
                "path": str(artifact.resolve()),
                "kind": os.environ.get(
                    "FAKE_HERDR_ARTIFACT_KIND", _contract_kind(prompt)
                ),
                "description": "fake artifact",
            }
        ],
        "changed_files": changed_files,
        "tests": ["fake test"],
        "findings": [],
        "decision": "approved",
        "needs_fixer": False,
        "verified": verified,
        "verification": ["fake verification"],
    }


def _write_result(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.fake-tmp")
    with temporary.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
    temporary.replace(path)


def _state_path() -> Path:
    configured = os.environ.get("FAKE_HERDR_STATE")
    if configured:
        return Path(configured)
    log_path = os.environ.get("FAKE_HERDR_LOG")
    if log_path:
        return Path(log_path).with_suffix(".state.json")
    return Path(".fake-herdr-state.json").resolve()


def _read_state() -> dict[str, Any]:
    try:
        value = json.loads(_state_path().read_text(encoding="utf-8"))
    except (FileNotFoundError, json.JSONDecodeError):
        return {}
    return value if isinstance(value, dict) else {}


def _write_state(state: dict[str, Any]) -> None:
    path = _state_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.fake-tmp")
    temporary.write_text(json.dumps(state, ensure_ascii=False), encoding="utf-8")
    temporary.replace(path)


def _emit_error(code: str, message: str | None = None) -> int:
    print(
        json.dumps(
            {"error": {"code": code, "message": message or code}},
            ensure_ascii=False,
        ),
        file=sys.stderr,
    )
    return 1


def main() -> int:
    args = sys.argv[1:]
    _log(args)
    if args == ["--version"]:
        print("herdr 0.8.2")
        return 0
    if not args:
        return _emit({"status": "ok"})

    command = tuple(args[:2])
    if command == ("workspace", "create"):
        return _emit(
            {
                "workspace_id": "w-fake-workspace",
                "tab_id": "w-fake-tab",
                "pane_id": "w-fake-pane",
            }
        )
    if command == ("tab", "create"):
        return _emit(
            {
                "tab_id": "w-fake-agent-tab",
                "pane_id": "w-fake-agent-pane",
            }
        )
    if command == ("worktree", "create"):
        path_value = _value(args, "--path") or os.environ.get("FAKE_HERDR_WORKTREE")
        if not path_value:
            return _emit({"error": {"code": "missing_worktree_path"}})
        path = Path(path_value).expanduser().resolve()
        path.mkdir(parents=True, exist_ok=True)
        return _emit(
            {
                "worktree_id": "wt-fake-worktree",
                "worktree_path": str(path),
                "workspace_id": "w-fake-workspace",
                "tab_id": "w-fake-tab",
                "pane_id": "w-fake-pane",
            }
        )
    if command == ("worktree", "list"):
        configured = os.environ.get("FAKE_HERDR_WORKTREE_LIST")
        if configured:
            try:
                return _emit(json.loads(configured))
            except json.JSONDecodeError:
                return _emit({"error": {"code": "invalid_worktree_list"}})
        return _emit(
            {
                "worktrees": [
                    {
                        "workspace_id": os.environ.get(
                            "FAKE_HERDR_CURRENT_WORKSPACE", "w-current-workspace"
                        ),
                        "worktree_path": os.environ.get("FAKE_HERDR_WORKTREE", ""),
                        "branch": os.environ.get(
                            "FAKE_HERDR_BRANCH", "panopticon/current"
                        ),
                    }
                ]
            }
        )
    if command == ("worktree", "remove"):
        return _emit({"status": "removed"})
    if command == ("pane", "run"):
        return 0
    if command == ("pane", "send-keys"):
        pane_id = args[2] if len(args) > 2 else ""
        submit_key = args[3] if len(args) > 3 else ""
        state = _read_state()
        pending_prompts = state.get("pending_prompts", {})
        target = next(
            (
                candidate
                for candidate, pending in pending_prompts.items()
                if isinstance(pending, dict) and pending.get("pane_id") == pane_id
            ),
            None,
        )
        if target is None:
            return _emit_error("no_pending_prompt")
        expected_key = os.environ.get(
            "FAKE_HERDR_EXPECTED_SUBMIT_KEY",
            os.environ.get("FAKE_HERDR_SUBMIT_KEY", "ctrl+enter"),
        )
        if submit_key != expected_key:
            return _emit_error("invalid_key", f"expected {expected_key}")
        pending = pending_prompts.pop(target)
        prompt = pending.get("prompt") if isinstance(pending, dict) else None
        if not isinstance(prompt, str):
            return _emit_error("invalid_pending_prompt")
        mode = os.environ.get("FAKE_HERDR_MODE", "success")
        if mode == "timeout":
            state.setdefault("agents", {})[target] = {"state": "timeout"}
            _write_state(state)
            return 0
        result_path_value = _prompt_value(prompt, "RESULT_PATH=")
        if not result_path_value:
            return _emit_error("missing_result_path")
        if mode in {
            "delayed-working-before-result",
            "transient-idle-before-result",
        }:
            state.setdefault("agents", {})[target] = {
                "state": "delayed_working"
                if mode == "delayed-working-before-result"
                else "working",
                "prompt": prompt,
            }
            _write_state(state)
            return 0
        _write_result(Path(result_path_value), _result(prompt))
        lifecycle = (
            "settled" if mode in {"fast-success", "blocked-timeout"} else "working"
        )
        state.setdefault("agents", {})[target] = {"state": lifecycle}
        _write_state(state)
        return 0
    if command == ("agent", "start"):
        return _emit({"status": "started"})
    if command == ("agent", "get"):
        target = args[2] if len(args) > 2 else ""
        state = _read_state()
        agent_state = state.get("agents", {}).get(target)
        lifecycle = agent_state.get("state") if isinstance(agent_state, dict) else None
        mode = os.environ.get("FAKE_HERDR_MODE", "success")
        if mode in {"blocked", "blocked-timeout"}:
            status = "blocked"
        elif mode == "fast-success" or lifecycle == "settled":
            status = "idle"
        elif mode == "result-working-timeout" or lifecycle in {
            "working",
            "working_observed",
        }:
            status = "working"
        else:
            status = "unknown"
        return _emit(
            {
                "id": "cli:agent:get",
                "result": {
                    "agent": {"agent_status": status},
                    "type": "agent_info",
                },
            }
        )
    if command == ("agent", "wait"):
        target = args[2] if len(args) > 2 else ""
        until = _value(args, "--until")
        state = _read_state()
        agent_state = state.get("agents", {}).get(target)
        lifecycle = agent_state.get("state") if isinstance(agent_state, dict) else None
        mode = os.environ.get("FAKE_HERDR_MODE", "success")
        if (
            mode == "timeout"
            or lifecycle == "timeout"
            or (mode == "result-working-timeout" and until is None)
        ):
            return _emit_error("timeout")
        if until == "working":
            if (
                mode == "delayed-working-before-result"
                and lifecycle == "delayed_working"
            ):
                state.setdefault("agents", {})[target] = {
                    "state": "working",
                    "prompt": agent_state.get("prompt")
                    if isinstance(agent_state, dict)
                    else None,
                }
                _write_state(state)
                return _emit_error("timeout")
            if lifecycle == "working":
                state.setdefault("agents", {})[target] = {
                    "state": "working_observed",
                    "prompt": agent_state.get("prompt")
                    if isinstance(agent_state, dict)
                    else None,
                }
                _write_state(state)
                return _emit({"status": "working"})
            if mode == "transient-idle-before-result" and lifecycle == "transient_idle":
                state.setdefault("agents", {})[target] = {
                    "state": "working_observed_final",
                    "prompt": agent_state.get("prompt")
                    if isinstance(agent_state, dict)
                    else None,
                }
                _write_state(state)
                return _emit({"status": "working"})
            return _emit_error("timeout")
        if mode == "delayed-working-before-result" and lifecycle == "working_observed":
            prompt = (
                agent_state.get("prompt") if isinstance(agent_state, dict) else None
            )
            if not isinstance(prompt, str):
                return _emit_error("missing_result_path")
            result_path_value = _prompt_value(prompt, "RESULT_PATH=")
            if not result_path_value:
                return _emit_error("missing_result_path")
            _write_result(Path(result_path_value), _result(prompt))
            state.setdefault("agents", {})[target] = {"state": "settled"}
            _write_state(state)
            return _emit({"status": "idle"})
        if mode == "transient-idle-before-result" and lifecycle == "working_observed":
            state.setdefault("agents", {})[target] = {
                "state": "transient_idle",
                "prompt": agent_state.get("prompt")
                if isinstance(agent_state, dict)
                else None,
            }
            _write_state(state)
            return _emit({"status": "idle"})
        if (
            mode == "transient-idle-before-result"
            and lifecycle == "working_observed_final"
        ):
            prompt = (
                agent_state.get("prompt") if isinstance(agent_state, dict) else None
            )
            if not isinstance(prompt, str):
                return _emit_error("missing_result_path")
            result_path_value = _prompt_value(prompt, "RESULT_PATH=")
            if not result_path_value:
                return _emit_error("missing_result_path")
            _write_result(Path(result_path_value), _result(prompt))
            state.setdefault("agents", {})[target] = {"state": "settled"}
            _write_state(state)
            return _emit({"status": "idle"})
        if lifecycle in {"working", "working_observed", "settled"}:
            status = "blocked" if mode == "blocked" else "idle"
            state.setdefault("agents", {})[target] = {"state": "settled"}
            _write_state(state)
            return _emit({"status": status})
        return _emit({"status": "completed"})
    if command == ("agent", "prompt"):
        target = args[2] if len(args) > 2 else ""
        prompt = args[3] if len(args) > 3 else ""
        mode = os.environ.get("FAKE_HERDR_MODE", "success")
        if mode == "command-blocked":
            return _emit_error("agent_blocked")
        if mode == "command-failed":
            return _emit_error("agent_failed")
        result_path_value = _prompt_value(prompt, "RESULT_PATH=")
        if not result_path_value:
            return _emit_error("missing_result_path")
        if "--wait" not in args:
            state = _read_state()
            state.setdefault("pending_prompts", {})[target] = {
                "pane_id": "w-fake-agent-pane",
                "prompt": prompt,
            }
            state.setdefault("agents", {})[target] = {"state": "awaiting_submit"}
            _write_state(state)
            return 0
        _write_result(Path(result_path_value), _result(prompt))
        return _emit({"status": "completed"})
    return _emit({"status": "ok"})


if __name__ == "__main__":
    raise SystemExit(main())
