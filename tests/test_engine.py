import json
import os
import stat
import subprocess
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

from helpers import FAKE_HERDR, write_single_step_workflow

from panopticon.engine import (
    FlowEngine,
    FlowError,
    StartOptions,
    _snapshot_diff,
    _snapshot_worktree,
)
from panopticon.herdr import HerdrClient
from panopticon.state import RunStore, StateError, atomic_write_json, compact_state
from panopticon.workflow import load_workflow


class EngineFakeHerdrTests(unittest.TestCase):
    def setUp(self) -> None:
        FAKE_HERDR.chmod(0o755)

    @staticmethod
    def _git(root: Path, *args: str) -> None:
        environment = os.environ.copy()
        environment.update(
            {
                "GIT_AUTHOR_NAME": "panopticon-test",
                "GIT_AUTHOR_EMAIL": "panopticon-test@example.invalid",
                "GIT_COMMITTER_NAME": "panopticon-test",
                "GIT_COMMITTER_EMAIL": "panopticon-test@example.invalid",
            }
        )
        completed = subprocess.run(
            ["git", *args],
            cwd=root,
            env=environment,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            check=False,
            shell=False,
        )
        if completed.returncode != 0:
            raise AssertionError(
                f"git fixture command failed: {args}: {completed.stderr}"
            )

    def _init_git_repo(self, root: Path, files: dict[str, str]) -> None:
        root.mkdir(parents=True, exist_ok=True)
        self._git(root, "init", "--quiet")
        for relative, contents in files.items():
            path = root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(contents, encoding="utf-8")
        self._git(root, "add", ".")
        self._git(
            root,
            "-c",
            "user.name=panopticon-test",
            "-c",
            "user.email=panopticon-test@example.invalid",
            "commit",
            "--quiet",
            "-m",
            "initial",
        )

    def _new_engine(
        self,
        mode: str = "success",
        *,
        role: str = "developer",
        write_policy: str = "worktree",
        submit_key: str | None = None,
        agent_args: tuple[str, ...] | None = None,
        verify_commands: tuple[tuple[str, ...], ...] | None = None,
        timeout_seconds: int = 30,
    ) -> tuple[tempfile.TemporaryDirectory[str], FlowEngine, StartOptions]:
        temporary = tempfile.TemporaryDirectory()
        root = Path(temporary.name)
        repo = root / "repo"
        repo.mkdir()
        workflow = write_single_step_workflow(
            root / "workflow",
            role=role,
            write_policy=write_policy,
            submit_key=submit_key,
            agent_args=agent_args,
            timeout_seconds=timeout_seconds,
        )
        store = RunStore(root / "runs")
        environment = os.environ.copy()
        environment.update(
            {
                "HERDR_ENV": "1",
                "FAKE_HERDR_MODE": mode,
                "FAKE_HERDR_LOG": str(root / "herdr.jsonl"),
            }
        )
        client = HerdrClient(executable=str(FAKE_HERDR), environ=environment)
        engine = FlowEngine(store, client)
        options = StartOptions(
            repo=repo,
            workflow=workflow,
            task="fake task",
            verify_commands=(
                verify_commands
                if verify_commands is not None
                else workflow.default_verify
            ),
            use_worktree=False,
            background=False,
            script_path=Path(__file__).resolve().parents[1] / "bin" / "panopticon",
        )
        self.addCleanup(temporary.cleanup)
        return temporary, engine, options

    def test_background_launch_accepts_empty_pane_run_stdout_and_passes_herdr_bin(
        self,
    ) -> None:
        temporary, engine, options = self._new_engine()

        state = engine.create_run(replace(options, background=True))

        self.assertEqual(state["status"], "running")
        command = state["orchestrator"]["command"]
        self.assertIn("--herdr-bin", command)
        self.assertEqual(
            command[command.index("--herdr-bin") + 1], str(FAKE_HERDR.resolve())
        )
        calls = [
            json.loads(line)
            for line in (Path(temporary.name) / "herdr.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        pane_runs = [call for call in calls if call[:2] == ["pane", "run"]]
        self.assertEqual(len(pane_runs), 1)
        self.assertIn("--herdr-bin", pane_runs[0][3])

    def test_submit_key_uses_raw_prompt_and_pane_key_then_settles(self) -> None:
        temporary, engine, options = self._new_engine(submit_key="ctrl+enter")

        state = engine.create_run(options)

        self.assertEqual(state["status"], "completed")
        self.assertEqual(state["workflow"]["steps"][0]["submit_key"], "ctrl+enter")
        calls = [
            json.loads(line)
            for line in (Path(temporary.name) / "herdr.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        prompt_calls = [call for call in calls if call[:2] == ["agent", "prompt"]]
        self.assertEqual(len(prompt_calls), 1)
        self.assertNotIn("--wait", prompt_calls[0])
        send_calls = [call for call in calls if call[:2] == ["pane", "send-keys"]]
        self.assertEqual(
            send_calls, [["pane", "send-keys", "w-fake-agent-pane", "ctrl+enter"]]
        )
        wait_calls = [call for call in calls if call[:2] == ["agent", "wait"]]
        self.assertEqual(len(wait_calls), 2)
        self.assertEqual(wait_calls[0][3:5], ["--until", "working"])
        self.assertNotIn("--until", wait_calls[1])

    def test_agent_args_are_passed_as_argv_to_fake_agent_start(self) -> None:
        temporary, engine, options = self._new_engine(
            agent_args=("--no-extensions", "--profile", "profile with spaces")
        )

        state = engine.create_run(options)

        self.assertEqual(
            state["workflow"]["steps"][0]["agent_args"],
            ["--no-extensions", "--profile", "profile with spaces"],
        )
        self.assertEqual(
            state["steps"]["step"]["agent_args"],
            ["--no-extensions", "--profile", "profile with spaces"],
        )
        expected_agent_name = f"pan-{state['run_id'][-8:]}-step"
        self.assertEqual(
            state["steps"]["step"]["agent"]["name"], expected_agent_name
        )
        self.assertEqual(
            state["steps"]["step"]["agent"]["target"], expected_agent_name
        )
        calls = [
            json.loads(line)
            for line in (Path(temporary.name) / "herdr.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        start_calls = [call for call in calls if call[:2] == ["agent", "start"]]
        self.assertEqual(
            start_calls,
            [
                [
                    "agent",
                    "start",
                    state["steps"]["step"]["agent"]["target"],
                    "--kind",
                    "codex",
                    "--pane",
                    "w-fake-agent-pane",
                    "--timeout",
                    "30000",
                    "--",
                    "--no-extensions",
                    "--profile",
                    "profile with spaces",
                ]
            ],
        )

    def test_agent_start_timeout_is_clamped_to_herdr_range(self) -> None:
        _, _engine, options = self._new_engine()
        spec = options.workflow.steps[0]

        for timeout_seconds, expected_timeout in (
            (1, "3001"),
            (30, "30000"),
            (900, "300000"),
            (86400, "300000"),
        ):
            arguments = FlowEngine._agent_start_args(
                "agent", replace(spec, timeout_seconds=timeout_seconds), "pane"
            )
            self.assertEqual(
                arguments[arguments.index("--timeout") + 1], expected_timeout
            )

    def test_agent_start_clamp_keeps_step_prompt_timeout(self) -> None:
        temporary, engine, options = self._new_engine(timeout_seconds=900)

        state = engine.create_run(options)
        calls = [
            json.loads(line)
            for line in (Path(temporary.name) / "herdr.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        start_calls = [call for call in calls if call[:2] == ["agent", "start"]]
        prompt_calls = [call for call in calls if call[:2] == ["agent", "prompt"]]

        self.assertEqual(
            start_calls[0][start_calls[0].index("--timeout") + 1], "300000"
        )
        self.assertEqual(
            prompt_calls[0][prompt_calls[0].index("--timeout") + 1], "900000"
        )
        self.assertEqual(state["workflow"]["steps"][0]["timeout_seconds"], 900)

    def test_child_agent_tab_receives_recursion_guard_environment(self) -> None:
        temporary, engine, options = self._new_engine()

        engine.create_run(options)

        calls = [
            json.loads(line)
            for line in (Path(temporary.name) / "herdr.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        child_tab_calls = [call for call in calls if call[:2] == ["tab", "create"]]
        self.assertEqual(len(child_tab_calls), 1)
        env_index = child_tab_calls[0].index("--env")
        self.assertEqual(child_tab_calls[0][env_index + 1], "PANOPTICON_CHILD=1")

    def test_resume_restarts_existing_agent_with_the_same_agent_args(self) -> None:
        temporary, engine, options = self._new_engine(
            agent_args=("--no-extensions", "--profile", "profile with spaces")
        )

        state = engine.create_run(options)
        state["status"] = "failed"
        state["steps"]["step"]["status"] = "failed"
        state["steps"]["step"]["agent"]["started"] = False
        engine.store.save(state["run_id"], state)

        resumed = engine.resume_run(state["run_id"])

        self.assertEqual(resumed["status"], "completed")
        calls = [
            json.loads(line)
            for line in (Path(temporary.name) / "herdr.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        start_calls = [call for call in calls if call[:2] == ["agent", "start"]]
        self.assertEqual(len(start_calls), 2)
        self.assertEqual(
            start_calls[-1][-4:],
            ["--", "--no-extensions", "--profile", "profile with spaces"],
        )

    def test_submit_key_wrong_key_fails_the_step(self) -> None:
        _, engine, options = self._new_engine(submit_key="enter")

        state = engine.create_run(options)

        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["steps"]["step"]["status"], "failed")
        self.assertEqual(state["steps"]["step"]["error"]["code"], "invalid_key")

    def test_submit_key_timeout_is_recorded_as_timed_out(self) -> None:
        _, engine, options = self._new_engine("timeout", submit_key="ctrl+enter")

        state = engine.create_run(options)

        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["steps"]["step"]["status"], "timed_out")
        self.assertEqual(state["steps"]["step"]["error"]["code"], "timeout")

    def test_submit_key_result_while_agent_is_still_working_times_out(self) -> None:
        temporary, engine, options = self._new_engine(
            "result-working-timeout", submit_key="ctrl+enter"
        )

        state = engine.create_run(options)

        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["steps"]["step"]["status"], "timed_out")
        self.assertEqual(state["steps"]["step"]["error"]["code"], "timeout")
        calls = [
            json.loads(line)
            for line in (Path(temporary.name) / "herdr.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        self.assertTrue(any(call[:2] == ["agent", "get"] for call in calls))

    def test_submit_key_blocked_lifecycle_is_recorded_as_blocked(self) -> None:
        _, engine, options = self._new_engine("blocked", submit_key="ctrl+enter")

        state = engine.create_run(options)

        self.assertEqual(state["status"], "blocked")
        self.assertEqual(state["steps"]["step"]["status"], "blocked")
        self.assertEqual(state["steps"]["step"]["error"]["code"], "agent_blocked")

    def test_submit_key_fast_completion_with_idle_status_is_accepted(self) -> None:
        temporary, engine, options = self._new_engine(
            "fast-success", submit_key="ctrl+enter"
        )

        state = engine.create_run(options)

        self.assertEqual(state["status"], "completed")
        calls = [
            json.loads(line)
            for line in (Path(temporary.name) / "herdr.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        get_calls = [call for call in calls if call[:2] == ["agent", "get"]]
        self.assertEqual(len(get_calls), 1)
        self.assertEqual(get_calls[0][2], state["steps"]["step"]["agent"]["target"])

    def test_submit_key_blocked_status_after_timeout_is_propagated(self) -> None:
        temporary, engine, options = self._new_engine(
            "blocked-timeout", submit_key="ctrl+enter"
        )

        state = engine.create_run(options)

        self.assertEqual(state["status"], "blocked")
        self.assertEqual(state["steps"]["step"]["status"], "blocked")
        self.assertEqual(state["steps"]["step"]["error"]["code"], "agent_blocked")
        calls = [
            json.loads(line)
            for line in (Path(temporary.name) / "herdr.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        self.assertTrue(any(call[:2] == ["agent", "get"] for call in calls))

    def test_submit_key_unspecified_keeps_legacy_wait_command(self) -> None:
        temporary, engine, options = self._new_engine()

        state = engine.create_run(options)

        self.assertEqual(state["status"], "completed")
        calls = [
            json.loads(line)
            for line in (Path(temporary.name) / "herdr.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        prompt_calls = [call for call in calls if call[:2] == ["agent", "prompt"]]
        self.assertEqual(len(prompt_calls), 1)
        self.assertIn("--wait", prompt_calls[0])
        self.assertFalse(any(call[:2] == ["pane", "send-keys"] for call in calls))
        self.assertFalse(any(call[:2] == ["agent", "wait"] for call in calls))

    def test_blocked_result_can_be_resumed(self) -> None:
        _, engine, options = self._new_engine("blocked")

        state = engine.create_run(options)
        self.assertEqual(state["status"], "blocked")
        self.assertEqual(state["steps"]["step"]["status"], "blocked")

        engine.client.environ["FAKE_HERDR_MODE"] = "success"
        resumed = engine.resume_run(state["run_id"])

        self.assertEqual(resumed["status"], "completed")
        self.assertEqual(resumed["steps"]["step"]["attempts"], 2)

    def test_writer_changed_files_match_the_real_worktree_snapshot(self) -> None:
        _, engine, options = self._new_engine()
        engine.client.environ["FAKE_HERDR_CHANGED_FILES"] = '["changed.py"]'

        state = engine.create_run(options)

        self.assertEqual(state["status"], "completed")
        self.assertEqual(state["steps"]["step"]["actual_changed_files"], ["changed.py"])
        self.assertTrue((Path(state["worktree"]["path"]) / "changed.py").is_file())

    def test_writer_changed_files_mismatch_fails_closed(self) -> None:
        _, engine, options = self._new_engine()
        engine.client.environ["FAKE_HERDR_CHANGED_FILES"] = '["declared.py"]'
        engine.client.environ["FAKE_HERDR_ACTUAL_CHANGED_FILES"] = '["actual.py"]'

        state = engine.create_run(options)

        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["error"]["type"], "worktree_change_mismatch")
        self.assertEqual(state["steps"]["step"]["actual_changed_files"], ["actual.py"])

    def test_read_only_worktree_change_fails_closed(self) -> None:
        _, engine, options = self._new_engine(write_policy="none")
        engine.client.environ["FAKE_HERDR_ACTUAL_CHANGED_FILES"] = '["sneaky.py"]'

        state = engine.create_run(options)

        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["error"]["type"], "worktree_changed_read_only")
        self.assertEqual(state["steps"]["step"]["actual_changed_files"], ["sneaky.py"])

    def test_snapshot_failure_fails_closed(self) -> None:
        _, engine, options = self._new_engine()

        with patch(
            "panopticon.engine._snapshot_worktree", side_effect=OSError("denied")
        ):
            state = engine.create_run(options)

        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["error"]["type"], "worktree_snapshot_failed")

    def test_git_snapshot_uses_status_and_ignores_cache(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self._init_git_repo(
                root,
                {
                    ".gitignore": ".cache/\n",
                    "tracked.txt": "initial\n",
                },
            )
            (root / "tracked.txt").write_text("dirty\n", encoding="utf-8")
            (root / ".cache").mkdir()
            (root / ".cache" / "ignored.bin").write_bytes(b"ignored")
            normal_untracked = root / "space 名.txt"
            normal_untracked.write_text("untracked\n", encoding="utf-8")

            with patch(
                "panopticon.engine.subprocess.run",
                wraps=subprocess.run,
            ) as run:
                snapshot = _snapshot_worktree(
                    root,
                    allow_filesystem_fallback=True,
                )

            self.assertEqual(
                run.call_args.args[0],
                [
                    "git",
                    "status",
                    "--porcelain=v1",
                    "-z",
                    "--untracked-files=all",
                ],
            )
            self.assertFalse(run.call_args.kwargs["shell"])
            self.assertFalse(run.call_args.kwargs["text"])
            environment = run.call_args.kwargs["env"]
            self.assertEqual(environment["LC_ALL"], "C")
            self.assertEqual(environment["LANG"], "C")
            self.assertEqual(environment["LANGUAGE"], "C")
            self.assertEqual(set(snapshot), {"tracked.txt", "space 名.txt"})
            self.assertIn("git: M:file:", snapshot["tracked.txt"])
            self.assertIn("git:??:file:", snapshot["space 名.txt"])

    def test_git_snapshot_detects_dirty_rechange_rename_and_delete(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self._init_git_repo(
                root,
                {
                    "existing.txt": "initial\n",
                    "old name.txt": "rename me\n",
                    "deleted.txt": "delete me\n",
                },
            )
            existing = root / "existing.txt"
            existing.write_text("dirty before\n", encoding="utf-8")
            before = _snapshot_worktree(root)
            existing.write_text("dirty after\n", encoding="utf-8")
            initial_mode = stat.S_IMODE(existing.stat().st_mode)
            os.chmod(existing, initial_mode ^ stat.S_IXUSR)
            after = _snapshot_worktree(root)

            self.assertEqual(_snapshot_diff(before, after), ["existing.txt"])

            self._git(root, "mv", "old name.txt", "新しい name.txt")
            self._git(root, "rm", "deleted.txt")
            snapshot = _snapshot_worktree(root)

            self.assertIn("新しい name.txt", snapshot)
            self.assertIn("old name.txt", snapshot)
            self.assertIn("original:old name.txt", snapshot["新しい name.txt"])
            self.assertIn("git:R :deleted", snapshot["old name.txt"])
            self.assertIn("deleted.txt", snapshot)
            self.assertIn("git:D :deleted", snapshot["deleted.txt"])
            self.assertEqual(
                _snapshot_diff(after, snapshot),
                ["deleted.txt", "old name.txt", "新しい name.txt"],
            )
            mode = stat.S_IMODE(existing.stat().st_mode)
            self.assertIn(f":{mode:o}:", snapshot["existing.txt"])

    def test_git_snapshot_parses_copy_record(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self._init_git_repo(root, {"source.txt": "source\n"})
            (root / "copy 名.txt").write_text("source\n", encoding="utf-8")
            result = subprocess.CompletedProcess(
                ["git", "status"],
                0,
                "C  copy 名.txt\0source.txt\0".encode(),
                b"",
            )

            with patch("panopticon.engine.subprocess.run", return_value=result):
                snapshot = _snapshot_worktree(root)

            self.assertEqual(list(snapshot), ["copy 名.txt"])
            self.assertIn("original:source.txt", snapshot["copy 名.txt"])

    def test_git_snapshot_rejects_directory_status_path(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "submodule").mkdir()
            result = subprocess.CompletedProcess(
                ["git", "status"],
                0,
                b" M submodule\0",
                b"",
            )

            with (
                patch("panopticon.engine.subprocess.run", return_value=result),
                self.assertRaisesRegex(FlowError, "directory.*snapshot"),
            ):
                _snapshot_worktree(root)

    def test_git_status_failure_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self._init_git_repo(root, {"tracked.txt": "tracked\n"})
            result = subprocess.CompletedProcess(
                ["git", "status"],
                1,
                b"",
                b"fatal: index unavailable",
            )

            with (
                patch("panopticon.engine.subprocess.run", return_value=result),
                self.assertRaisesRegex(FlowError, "git status が失敗"),
            ):
                _snapshot_worktree(root, allow_filesystem_fallback=True)

    def test_non_git_no_worktree_uses_filesystem_fallback(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "normal.txt"
            path.write_text("normal\n", encoding="utf-8")

            snapshot = _snapshot_worktree(
                root,
                allow_filesystem_fallback=True,
            )

            self.assertIn("normal.txt", snapshot)
            with self.assertRaises(FlowError):
                _snapshot_worktree(root)

    def test_verifier_success_uses_engine_verification_result(self) -> None:
        _, engine, options = self._new_engine(
            role="verifier",
            write_policy="none",
            verify_commands=(("python3", "-c", "print('ok')"),),
        )

        state = engine.create_run(options)
        verification = state["steps"]["step"]["verification"]

        self.assertEqual(state["status"], "completed")
        self.assertTrue(verification["all_succeeded"])
        self.assertEqual(verification["commands"][0]["returncode"], 0)
        self.assertEqual(verification["commands"][0]["stdout"].strip(), "ok")
        self.assertTrue(
            Path(state["steps"]["step"]["verification_artifact_path"]).is_file()
        )

    def test_verifier_failure_is_not_overridden_by_agent_verified_true(self) -> None:
        _, engine, options = self._new_engine(
            role="verifier",
            write_policy="none",
            verify_commands=(
                (
                    "python3",
                    "-c",
                    "import sys; print('o' * 6000); print('e' * 6000, file=sys.stderr); sys.exit(3)",
                ),
            ),
        )
        engine.client.environ["FAKE_HERDR_VERIFIED"] = "true"

        state = engine.create_run(options)
        record = state["steps"]["step"]["verification"]["commands"][0]

        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["error"]["type"], "verification_failed")
        self.assertFalse(state["steps"]["step"]["verification"]["all_succeeded"])
        self.assertEqual(record["returncode"], 3)
        self.assertLessEqual(len(record["stdout"]), 4000)
        self.assertLessEqual(len(record["stderr"]), 4000)

    def test_resume_rejects_modified_workflow_digest(self) -> None:
        _, engine, options = self._new_engine("blocked")
        state = engine.create_run(options)
        self.assertEqual(state["workflow"]["digest"], options.workflow.digest)
        options.workflow.path.write_text(
            options.workflow.path.read_text(encoding="utf-8") + "\n# modified\n",
            encoding="utf-8",
        )

        with self.assertRaisesRegex(FlowError, "workflow TOML"):
            engine.resume_run(state["run_id"])

    def test_reviewer_does_not_claim_existing_developer_changes(self) -> None:
        temporary, engine, options = self._new_engine()
        workflow_root = Path(temporary.name) / "workflow"
        (workflow_root / "workflow.toml").write_text(
            """version = 1
name = "developer-reviewer"
default_verify = [["python3", "-c", "pass"]]

[[steps]]
id = "developer"
role = "developer"
kind = "codex"
depends_on = []
read_policy = "repo-and-dependencies"
write_policy = "worktree"
timeout_seconds = 30
template = "prompt.md"

[steps.contract]
required_fields = ["schema_version", "run_id", "step_id", "role", "status", "summary", "artifacts", "changed_files"]
artifact_kinds = ["change"]
required_list_fields = ["changed_files"]

[[steps]]
id = "reviewer"
role = "reviewer"
kind = "codex"
depends_on = ["developer"]
read_policy = "worktree"
write_policy = "none"
timeout_seconds = 30
template = "prompt.md"

[steps.contract]
required_fields = ["schema_version", "run_id", "step_id", "role", "status", "summary", "artifacts", "changed_files", "findings", "decision", "needs_fixer"]
artifact_kinds = ["review"]
required_boolean_fields = ["needs_fixer"]
required_list_fields = ["changed_files", "findings"]
""",
            encoding="utf-8",
        )
        workflow = load_workflow(workflow_root / "workflow.toml")
        options = replace(
            options,
            workflow=workflow,
            verify_commands=workflow.default_verify,
        )
        engine.client.environ["FAKE_HERDR_CHANGED_FILES_DEVELOPER"] = '["developer.py"]'

        state = engine.create_run(options)

        self.assertEqual(state["status"], "completed")
        self.assertEqual(
            state["steps"]["developer"]["actual_changed_files"], ["developer.py"]
        )
        self.assertEqual(state["steps"]["reviewer"]["actual_changed_files"], [])
        self.assertEqual(state["steps"]["reviewer"]["result"]["changed_files"], [])

    def test_failed_result_can_be_resumed(self) -> None:
        _, engine, options = self._new_engine("failed")

        state = engine.create_run(options)
        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["steps"]["step"]["status"], "failed")

        engine.client.environ["FAKE_HERDR_MODE"] = "success"
        resumed = engine.resume_run(state["run_id"])

        self.assertEqual(resumed["status"], "completed")

    def test_command_blocked_is_persisted_and_resumable(self) -> None:
        _, engine, options = self._new_engine("command-blocked")

        state = engine.create_run(options)
        self.assertEqual(state["status"], "blocked")
        self.assertEqual(state["steps"]["step"]["error"]["code"], "agent_blocked")

        engine.client.environ["FAKE_HERDR_MODE"] = "success"
        self.assertEqual(engine.resume_run(state["run_id"])["status"], "completed")

    def test_worktree_mode_keeps_agents_outside_repository(self) -> None:
        temporary, engine, options = self._new_engine("success")
        dedicated = Path(temporary.name) / "dedicated-worktree"
        self._init_git_repo(dedicated, {"initial.txt": "initial\n"})
        engine.client.environ["FAKE_HERDR_WORKTREE"] = str(dedicated)

        state = engine.create_run(replace(options, use_worktree=True))

        self.assertEqual(state["status"], "completed")
        self.assertEqual(Path(state["worktree"]["path"]), dedicated.resolve())
        self.assertNotEqual(Path(state["worktree"]["path"]), options.repo.resolve())
        self.assertTrue(state["resources"]["worktree_created"])

    def test_worktree_mode_rejects_repository_path(self) -> None:
        _, engine, options = self._new_engine("success")
        engine.client.environ["FAKE_HERDR_WORKTREE"] = str(options.repo)

        with self.assertRaisesRegex(FlowError, "repository の外側"):
            engine.create_run(replace(options, use_worktree=True))

    def test_changed_files_must_stay_relative_to_worktree(self) -> None:
        _, engine, options = self._new_engine("success")
        engine.client.environ["FAKE_HERDR_CHANGED_FILES"] = "../escape.py"

        state = engine.create_run(options)

        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["error"]["type"], "invalid_result")
        self.assertIn("worktree", state["error"]["message"])

    def test_cancel_lock_conflict_returns_preloaded_compact_state(self) -> None:
        _, engine, options = self._new_engine()
        run_id = "run-cancel-locked"
        run_dir = engine.store.run_dir(run_id)
        state = engine._new_state(run_id, options, run_dir)
        state.update(
            {
                "status": "running",
                "current_step": "step",
                "error": {"message": "still running"},
            }
        )
        state["steps"]["step"]["status"] = "running"
        engine.store.create(run_id, state)

        with engine.store.lock(run_id):
            result = engine.cancel_run(run_id)

        self.assertEqual(result["status"], "cancel_requested")
        payload = compact_state(result)
        self.assertEqual(payload["repo"], state["repo"])
        self.assertEqual(payload["worktree"], state["worktree"]["path"])
        self.assertEqual(payload["current_step"], "step")
        self.assertEqual(payload["steps"]["step"]["status"], "running")
        self.assertEqual(payload["error"], {"message": "still running"})
        self.assertEqual(payload["updated_at"], state["updated_at"])
        self.assertTrue((run_dir / "cancel.request").exists())

    def test_cleanup_uses_current_unique_workspace_id_under_run_lock(self) -> None:
        temporary, engine, options = self._new_engine()
        run_id = "run-cleanup"
        state = engine._new_state(run_id, options, engine.store.run_dir(run_id))
        worktree = Path(temporary.name) / "dedicated-worktree"
        worktree.mkdir()
        state["status"] = "failed"
        state["resources"]["worktree_created"] = True
        state["resources"]["workspace_id"] = "w-stale-workspace"
        state["worktree"].update(
            {"path": str(worktree), "branch": "panopticon/current"}
        )
        engine.store.create(run_id, state)
        engine.client.environ["FAKE_HERDR_WORKTREE_LIST"] = json.dumps(
            {
                "worktrees": [
                    {
                        "workspace_id": "w-current-workspace",
                        "worktree_path": str(worktree),
                        "branch": "panopticon/current",
                    }
                ]
            }
        )

        result = engine.cleanup(run_id, remove_worktree=True)

        self.assertEqual(result[0]["worktree_removed"], True)
        self.assertFalse(engine.store.run_dir(run_id).exists())
        calls = [
            json.loads(line)
            for line in (Path(temporary.name) / "herdr.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        remove = next(call for call in calls if call[:2] == ["worktree", "remove"])
        self.assertEqual(remove[remove.index("--workspace") + 1], "w-current-workspace")

    def test_cleanup_rejects_lock_competition(self) -> None:
        _, engine, options = self._new_engine()
        run_id = "run-locked"
        state = engine._new_state(run_id, options, engine.store.run_dir(run_id))
        state["status"] = "failed"
        engine.store.create(run_id, state)

        with engine.store.lock(run_id), self.assertRaises(StateError):
            engine.cleanup(run_id)
        self.assertTrue(engine.store.run_dir(run_id).exists())

    def test_recovery_honours_a_blocked_result_instead_of_rerunning_agent(self) -> None:
        temporary, engine, options = self._new_engine("success")
        store = engine.store
        run_id = "run-recovery"
        run_dir = store.run_dir(run_id)
        state = engine._new_state(run_id, options, run_dir)
        store.create(run_id, state)
        step = state["steps"]["step"]
        step["status"] = "running"
        state["status"] = "running"
        engine._snapshot_step(state, options.workflow.steps[0], "before")
        artifact = Path(state["run_dir"]) / "blocked-report.txt"
        artifact.write_text("blocked\n", encoding="utf-8")
        result = {
            "schema_version": 1,
            "run_id": run_id,
            "step_id": "step",
            "role": "developer",
            "status": "blocked",
            "summary": "resume later",
            "artifacts": [
                {
                    "path": str(artifact),
                    "kind": "report",
                    "description": "blocked report",
                }
            ],
            "changed_files": [],
            "tests": [],
        }
        atomic_write_json(Path(step["result_path"]), result)
        store.save(run_id, state)

        resumed = engine.resume_run(run_id)

        self.assertEqual(resumed["status"], "blocked")
        self.assertEqual(resumed["steps"]["step"]["status"], "blocked")
        log_path = Path(temporary.name) / "herdr.jsonl"
        if log_path.exists():
            calls = [
                json.loads(line)
                for line in log_path.read_text(encoding="utf-8").splitlines()
            ]
            self.assertFalse(any(call[:2] == ["agent", "prompt"] for call in calls))


if __name__ == "__main__":
    unittest.main()
