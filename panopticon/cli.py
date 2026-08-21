"""Command-line interface for ``bin/panopticon``."""

from __future__ import annotations

import argparse
import json
import math
import os
import shutil
from collections.abc import Sequence
from contextlib import suppress
from pathlib import Path
from typing import Any

from panopticon.engine import FlowEngine, FlowError, StartOptions, dry_run_payload
from panopticon.herdr import HerdrClient, HerdrError, require_herdr_env
from panopticon.state import (
    DEFAULT_WAIT_INTERVAL_SECONDS,
    DEFAULT_WAIT_TIMEOUT_SECONDS,
    RunStore,
    StateError,
    compact_state,
    wait_for_terminal,
)
from panopticon.workflow import (
    WorkflowError,
    load_repo_config,
    load_workflow,
    resolve_verify_commands,
    resolve_workflow_path,
)

PACKAGE_ROOT = Path(__file__).resolve().parents[1]


def _json_print(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))


def _json_print_compact(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))


def _positive_seconds(value: str) -> float:
    try:
        seconds = float(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("秒数は数値で指定してください") from exc
    if not math.isfinite(seconds) or seconds <= 0:
        raise argparse.ArgumentTypeError("秒数は正数で指定してください")
    return seconds


def _interval_seconds(value: str) -> float:
    seconds = _positive_seconds(value)
    if not 0.2 <= seconds <= 30:
        raise argparse.ArgumentTypeError("interval は0.2〜30秒で指定してください")
    return seconds


def _runtime(args: argparse.Namespace) -> tuple[RunStore, HerdrClient]:
    root_value = getattr(args, "state_root", None)
    store = RunStore(Path(root_value).expanduser() if root_value else None)
    executable = getattr(args, "herdr_bin", None)
    return store, HerdrClient(executable=executable)


def _repo(args: argparse.Namespace) -> Path:
    value = getattr(args, "repo", None) or os.getcwd()
    return Path(value).expanduser().resolve()


def _task(args: argparse.Namespace) -> str:
    option = getattr(args, "task_option", None)
    positional = getattr(args, "task", None)
    value = option if option is not None else positional
    return (
        value.strip()
        if isinstance(value, str) and value.strip()
        else "指定された依頼を調査・実装・検証する"
    )


def _load_plan_inputs(
    args: argparse.Namespace,
) -> tuple[Path, Any, tuple[tuple[str, ...], ...]]:
    repo = _repo(args)
    config = load_repo_config(repo)
    workflow_path = resolve_workflow_path(
        repo,
        getattr(args, "workflow", None),
        PACKAGE_ROOT,
        config,
    )
    workflow = load_workflow(workflow_path)
    verify_commands = resolve_verify_commands(
        getattr(args, "verify", None), config, workflow
    )
    return repo, workflow, verify_commands


def _require_managed(client: HerdrClient) -> None:
    require_herdr_env(client.environ)


def _run_start(args: argparse.Namespace) -> int:
    store, client = _runtime(args)
    _require_managed(client)
    repo, workflow, verify_commands = _load_plan_inputs(args)
    options = StartOptions(
        repo=repo,
        workflow=workflow,
        task=_task(args),
        verify_commands=verify_commands,
        use_worktree=bool(getattr(args, "use_worktree", True)),
        worktree_path=(
            Path(args.worktree_path).expanduser().resolve()
            if args.worktree_path
            else None
        ),
        branch=args.branch,
        base=args.base,
        background=bool(args.background),
        script_path=Path(__file__).resolve().parents[1] / "bin" / "panopticon",
    )
    state = FlowEngine(store, client).create_run(options)
    _json_print_compact(compact_state(state))
    return _state_exit_code(state)


def _run_dry_run(args: argparse.Namespace) -> int:
    _store, client = _runtime(args)
    _require_managed(client)
    repo, workflow, verify_commands = _load_plan_inputs(args)
    _json_print(
        dry_run_payload(
            repo,
            workflow,
            _task(args),
            verify_commands,
            use_worktree=bool(getattr(args, "use_worktree", True)),
        )
    )
    return 0


def _run_resume(args: argparse.Namespace) -> int:
    store, client = _runtime(args)
    _require_managed(client)
    state = store.load(args.run_id)
    workflow = load_workflow(Path(state["workflow"]["path"]))
    result = FlowEngine(store, client).resume_run(args.run_id, workflow)
    _json_print(result)
    return _state_exit_code(result)


def _pick_run_id(store: RunStore, argument: str | None, option: str | None) -> str:
    run_id = option or argument
    if run_id:
        return run_id
    entries = store.list()
    if not entries:
        raise FlowError("run がありません")
    return str(entries[0]["run_id"])


def _run_status(args: argparse.Namespace, *, full: bool = False) -> int:
    store, client = _runtime(args)
    _require_managed(client)
    run_id = _pick_run_id(
        store, getattr(args, "run_id_arg", None), getattr(args, "run_id_option", None)
    )
    state = store.load(run_id)
    if full and getattr(args, "step", None):
        step = state.get("steps", {}).get(args.step)
        if step is None:
            raise FlowError(f"step が見つかりません: {args.step}")
        _json_print(step)
    elif full:
        _json_print(state)
    else:
        _json_print_compact(compact_state(state))
    return _state_exit_code(state)


def _run_list(args: argparse.Namespace) -> int:
    store, client = _runtime(args)
    _require_managed(client)
    _json_print(store.list())
    return 0


def _wait_exit_code(state: dict[str, Any]) -> int:
    status = state.get("status")
    if status == "completed":
        return 0
    if status == "blocked":
        return 2
    if status in {"failed", "cancelled"}:
        return 1
    return 124


def _run_wait(args: argparse.Namespace) -> int:
    store, client = _runtime(args)
    _require_managed(client)
    run_id = _pick_run_id(store, None, args.run_id)
    try:
        state, timed_out = wait_for_terminal(
            store,
            run_id,
            timeout_seconds=args.timeout_seconds,
            interval_seconds=args.interval_seconds,
        )
    except KeyboardInterrupt:
        # wait は観測だけを行い、Ctrl-C で run の cancel 要求は出さない。
        state = store.load(run_id)
        _json_print_compact(compact_state(state))
        return 130
    _json_print_compact(compact_state(state))
    return 124 if timed_out else _wait_exit_code(state)


def _run_cancel(args: argparse.Namespace) -> int:
    store, client = _runtime(args)
    _require_managed(client)
    result = FlowEngine(store, client).cancel_run(args.run_id)
    _json_print_compact(compact_state(result))
    return _state_exit_code(result)


def _run_cleanup(args: argparse.Namespace) -> int:
    store, client = _runtime(args)
    _require_managed(client)
    result = FlowEngine(store, client).cleanup(
        args.run_id,
        all_runs=args.all_runs,
        older_than_hours=args.older_than_hours,
        remove_worktree=args.remove_worktree,
    )
    _json_print(result)
    return 0


def _run_doctor(args: argparse.Namespace) -> int:
    store, client = _runtime(args)
    checks: list[dict[str, Any]] = []
    env_ok = client.environ.get("HERDR_ENV") == "1"
    checks.append(
        {
            "name": "HERDR_ENV",
            "ok": env_ok,
            "detail": "1" if env_ok else "HERDR_ENV=1 が必要です",
        }
    )
    executable = client.executable
    executable_path = shutil.which(executable)
    if executable_path is None and Path(executable).is_file():
        executable_path = str(Path(executable).resolve())
    checks.append(
        {
            "name": "herdr_executable",
            "ok": executable_path is not None,
            "detail": executable_path or executable,
        }
    )
    state_ok = True
    state_detail = str(store.root)
    try:
        store.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        probe = store.root / ".doctor-write-test"
        probe.write_text("ok\n", encoding="utf-8")
        probe.unlink()
    except OSError as exc:
        state_ok = False
        state_detail = str(exc)
    checks.append({"name": "state_directory", "ok": state_ok, "detail": state_detail})
    version = None
    if env_ok and executable_path:
        with suppress(HerdrError):
            version = client.run_text(["--version"], timeout_seconds=10)
    version_ok = isinstance(version, str) and version.startswith("herdr 0.8.2")
    checks.append(
        {
            "name": "herdr_version",
            "ok": version_ok,
            "detail": version or "取得できません（0.8.2 を推奨）",
        }
    )
    if getattr(args, "repo", None) or getattr(args, "workflow", None):
        try:
            repo, workflow, verify_commands = _load_plan_inputs(args)
            checks.append(
                {"name": "workflow", "ok": True, "detail": str(workflow.path)}
            )
            checks.append(
                {
                    "name": "verification_commands",
                    "ok": bool(verify_commands),
                    "detail": [list(command) for command in verify_commands],
                }
            )
            checks.append({"name": "repo", "ok": repo.is_dir(), "detail": str(repo)})
        except (FlowError, WorkflowError, OSError) as exc:
            checks.append({"name": "workflow", "ok": False, "detail": str(exc)})
    result = {"ok": all(check["ok"] for check in checks), "checks": checks}
    _json_print(result)
    return 0 if result["ok"] else 1


def _state_exit_code(value: Any) -> int:
    if not isinstance(value, dict):
        return 0
    status = value.get("status")
    if status == "failed":
        return 1
    if status in {"blocked", "cancel_requested"}:
        return 2
    if status == "cancelled":
        return 3
    return 0


def _add_state_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "--state-root", default=argparse.SUPPRESS, help="run state の保存先"
    )
    parser.add_argument(
        "--herdr-bin", default=argparse.SUPPRESS, help="herdr executable のパス"
    )


def _add_plan_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--repo", help="対象 repository（既定: 現在のディレクトリ）")
    parser.add_argument("--workflow", help="workflow 名または TOML path")
    parser.add_argument(
        "--verify", action="append", help="検証 argv を引用付きで指定（複数可）"
    )
    parser.add_argument(
        "--no-worktree",
        dest="use_worktree",
        action="store_false",
        help="専用 worktree を作らない明示的な選択",
    )
    parser.set_defaults(use_worktree=True)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="panopticon",
        description="Herdr 0.8.2 の workspace/tab/pane/agent primitives で宣言的 workflow を実行します。",
    )
    _add_state_options(parser)
    subparsers = parser.add_subparsers(dest="command", required=True)

    start = subparsers.add_parser(
        "start", help="run を作成して開始（既定はバックグラウンド）"
    )
    _add_state_options(start)
    _add_plan_options(start)
    start.add_argument("task", nargs="?", help="依頼内容")
    start.add_argument(
        "--task", dest="task_option", help="依頼内容（位置引数より優先）"
    )
    start.add_argument("--worktree-path")
    start.add_argument("--branch")
    start.add_argument("--base")
    mode = start.add_mutually_exclusive_group()
    mode.add_argument(
        "--foreground",
        dest="background",
        action="store_false",
        help="このプロセスで完了まで実行",
    )
    mode.add_argument(
        "--background",
        dest="background",
        action="store_true",
        help="orchestrator pane で実行（既定）",
    )
    start.set_defaults(background=True)
    start.set_defaults(handler=_run_start)

    dry = subparsers.add_parser("dry-run", help="副作用なしで実行計画を表示")
    _add_state_options(dry)
    _add_plan_options(dry)
    dry.add_argument("task", nargs="?", help="依頼内容")
    dry.add_argument("--task", dest="task_option")
    dry.set_defaults(handler=_run_dry_run)

    status = subparsers.add_parser("status", help="run の概要を表示")
    _add_state_options(status)
    status.add_argument("run_id_arg", nargs="?")
    status.add_argument("--run-id", dest="run_id_option")
    status.set_defaults(handler=_run_status, full=False)

    show = subparsers.add_parser("show", help="run state 全体を表示")
    _add_state_options(show)
    show.add_argument("run_id_arg", nargs="?")
    show.add_argument("--run-id", dest="run_id_option")
    show.add_argument("--step")
    show.set_defaults(handler=lambda args: _run_status(args, full=True))

    listing = subparsers.add_parser("list", help="run 一覧を表示")
    _add_state_options(listing)
    listing.set_defaults(handler=_run_list)

    wait = subparsers.add_parser("wait", help="run の完了を待機")
    _add_state_options(wait)
    wait.add_argument("--run-id")
    wait.add_argument(
        "--timeout-seconds",
        type=_positive_seconds,
        default=DEFAULT_WAIT_TIMEOUT_SECONDS,
        help="待機タイムアウト（正数、既定: 3600秒）",
    )
    wait.add_argument(
        "--interval-seconds",
        type=_interval_seconds,
        default=DEFAULT_WAIT_INTERVAL_SECONDS,
        help="state.json の監視間隔（0.2〜30秒、既定: 1秒）",
    )
    wait.set_defaults(handler=_run_wait)

    resume = subparsers.add_parser(
        "resume", help="failed/blocked/中断 run を安全に再開"
    )
    _add_state_options(resume)
    resume.add_argument("--run-id", required=True)
    resume.add_argument("--foreground", action="store_true", help=argparse.SUPPRESS)
    resume.set_defaults(handler=_run_resume)

    cancel = subparsers.add_parser("cancel", help="run をキャンセル")
    _add_state_options(cancel)
    cancel.add_argument("run_id")
    cancel.set_defaults(handler=_run_cancel)

    cleanup = subparsers.add_parser("cleanup", help="terminal run の state を削除")
    _add_state_options(cleanup)
    cleanup.add_argument("--run-id")
    cleanup.add_argument("--all", dest="all_runs", action="store_true")
    cleanup.add_argument("--older-than-hours", type=float, default=24.0)
    cleanup.add_argument(
        "--remove-worktree",
        action="store_true",
        help="明示時のみ専用 worktree/workspace も削除",
    )
    cleanup.set_defaults(handler=_run_cleanup)

    doctor = subparsers.add_parser(
        "doctor", help="環境、Herdr version、state、workflow を診断"
    )
    _add_state_options(doctor)
    doctor.add_argument("--repo")
    doctor.add_argument("--workflow")
    doctor.add_argument("--verify", action="append")
    doctor.set_defaults(handler=_run_doctor)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    handler = getattr(args, "handler", None)
    if handler is None:
        parser.error("command が必要です")
    try:
        return int(handler(args))
    except (FlowError, WorkflowError, HerdrError, StateError, OSError) as exc:
        _json_print({"error": str(exc), "type": type(exc).__name__})
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
