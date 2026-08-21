import json
import os
import tempfile
import unittest
from contextlib import redirect_stdout
from io import StringIO
from pathlib import Path
from typing import Any
from unittest.mock import patch

from helpers import FAKE_HERDR

from panopticon.cli import main
from panopticon.state import RunStore, atomic_write_json, compact_state


class DoctorCliTests(unittest.TestCase):
    def test_doctor_passes_with_fake_herdr(self) -> None:
        FAKE_HERDR.chmod(0o755)
        with tempfile.TemporaryDirectory() as directory:
            output = StringIO()
            environment = os.environ.copy()
            environment.update(
                {
                    "HERDR_ENV": "1",
                    "PANOPTICON_STATE_DIR": str(Path(directory) / "runs"),
                }
            )
            with (
                patch.dict(os.environ, environment, clear=True),
                redirect_stdout(output),
            ):
                code = main(["doctor", "--herdr-bin", str(FAKE_HERDR)])

            payload = json.loads(output.getvalue())
            self.assertEqual(code, 0)
            self.assertTrue(payload["ok"])
            self.assertEqual(
                {check["name"] for check in payload["checks"]},
                {"HERDR_ENV", "herdr_executable", "state_directory", "herdr_version"},
            )

    def test_doctor_reports_missing_managed_environment(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output = StringIO()
            environment = os.environ.copy()
            environment.pop("HERDR_ENV", None)
            environment["PANOPTICON_STATE_DIR"] = str(Path(directory) / "runs")
            with (
                patch.dict(os.environ, environment, clear=True),
                redirect_stdout(output),
            ):
                code = main(["doctor", "--herdr-bin", str(FAKE_HERDR)])

            payload = json.loads(output.getvalue())
            self.assertEqual(code, 1)
            self.assertFalse(payload["ok"])
            self.assertFalse(
                next(
                    check for check in payload["checks"] if check["name"] == "HERDR_ENV"
                )["ok"]
            )


class WaitCliTests(unittest.TestCase):
    @staticmethod
    def _write_state(
        store: RunStore,
        run_id: str,
        status: str,
        updated_at: str,
        *,
        error: Any = None,
    ) -> None:
        state = {
            "run_id": run_id,
            "status": status,
            "current_step": "developer" if status == "running" else None,
            "repo": "/tmp/repo",
            "run_dir": str(store.run_dir(run_id)),
            "worktree": {"path": "/tmp/worktree", "enabled": True},
            "updated_at": updated_at,
            "steps": {
                "developer": {
                    "status": "running" if status == "running" else status,
                    "result": {"summary": "step summary"},
                    "error": None,
                }
            },
            "error": error,
            "events": [{"event": "huge snapshot"}],
            "worktree_snapshot_before": {"files": {"large": "snapshot"}},
        }
        store.create(run_id, state)
        atomic_write_json(store.state_path(run_id), state)

    @staticmethod
    def _run_wait(root: Path, *arguments: str) -> tuple[int, dict[str, Any], str]:
        output = StringIO()
        environment = os.environ.copy()
        environment.update(
            {
                "HERDR_ENV": "1",
                "PANOPTICON_STATE_DIR": str(root / "runs"),
            }
        )
        with patch.dict(os.environ, environment, clear=True), redirect_stdout(output):
            code = main(["wait", "--state-root", str(root / "runs"), *arguments])
        stdout = output.getvalue()
        return code, json.loads(stdout), stdout

    def test_wait_returns_terminal_status_exit_codes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            store = RunStore(root / "runs")
            self._write_state(
                store, "run-completed", "completed", "2025-01-01T00:00:00Z"
            )
            self._write_state(store, "run-blocked", "blocked", "2025-01-01T00:00:01Z")
            self._write_state(store, "run-failed", "failed", "2025-01-01T00:00:02Z")
            self._write_state(
                store, "run-cancelled", "cancelled", "2025-01-01T00:00:03Z"
            )

            completed_code, completed, _ = self._run_wait(
                root, "--run-id", "run-completed", "--timeout-seconds", "1"
            )
            blocked_code, blocked, _ = self._run_wait(
                root, "--run-id", "run-blocked", "--timeout-seconds", "1"
            )
            failed_code, failed, _ = self._run_wait(
                root, "--run-id", "run-failed", "--timeout-seconds", "1"
            )
            cancelled_code, cancelled, _ = self._run_wait(
                root, "--run-id", "run-cancelled", "--timeout-seconds", "1"
            )

            self.assertEqual(completed_code, 0)
            self.assertEqual(completed["status"], "completed")
            self.assertEqual(blocked_code, 2)
            self.assertEqual(blocked["status"], "blocked")
            self.assertEqual(failed_code, 1)
            self.assertEqual(failed["status"], "failed")
            self.assertEqual(cancelled_code, 1)
            self.assertEqual(cancelled["status"], "cancelled")

    def test_wait_returns_compact_snapshot_and_timeout_json(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            store = RunStore(root / "runs")
            self._write_state(
                store,
                "run-running",
                "running",
                "2025-01-01T00:00:00Z",
                error={
                    "type": "CommandError",
                    "code": "timeout",
                    "message": "run error",
                    "returncode": 1,
                    "stderr": "stderr output",
                },
            )

            code, payload, stdout = self._run_wait(
                root,
                "--run-id",
                "run-running",
                "--timeout-seconds",
                "0.01",
                "--interval-seconds",
                "0.2",
            )

            self.assertEqual(code, 124)
            self.assertEqual(
                set(payload),
                {
                    "run_id",
                    "status",
                    "current_step",
                    "repo",
                    "worktree",
                    "steps",
                    "error",
                    "updated_at",
                },
            )
            self.assertEqual(payload["worktree"], "/tmp/worktree")
            self.assertEqual(payload["steps"]["developer"]["status"], "running")
            self.assertEqual(payload["steps"]["developer"]["summary"], "step summary")
            self.assertEqual(payload["error"]["type"], "CommandError")
            self.assertEqual(payload["error"]["code"], "timeout")
            self.assertEqual(payload["error"]["message"], "run error")
            self.assertEqual(payload["error"]["returncode"], 1)
            self.assertEqual(payload["error"]["stderr"], "stderr output")
            self.assertGreater(stdout.count("\n"), 1)
            self.assertIn("\n  ", stdout)
            self.assertLessEqual(len(stdout.encode("utf-8")), 12 * 1024)
            self.assertNotIn("events", payload)
            self.assertNotIn("worktree_snapshot_before", payload)

    def test_wait_bounds_oversized_compact_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            store = RunStore(root / "runs")
            run_id = "run-large"
            structured_error = {
                "type": "CommandError",
                "code": "timeout",
                "message": "e" * 2000,
                "returncode": 1,
                "stderr": "s" * 2000,
            }
            state = {
                "run_id": run_id,
                "status": "failed",
                "current_step": "developer",
                "repo": "r" * 2500,
                "worktree": {"path": "w" * 2500},
                "updated_at": "2025-01-01T00:00:00Z",
                "steps": {
                    f"{index}-" + "x" * 200: {
                        "status": "failed",
                        "result": {"summary": "s" * 2000},
                        "error": structured_error,
                    }
                    for index in range(32)
                },
                "error": structured_error,
            }
            store.create(run_id, state)
            atomic_write_json(store.state_path(run_id), state)

            code, payload, stdout = self._run_wait(
                root, "--run-id", run_id, "--timeout-seconds", "1"
            )

            self.assertEqual(code, 1)
            self.assertEqual(payload["run_id"], run_id)
            self.assertEqual(payload["status"], "failed")
            self.assertLessEqual(len(payload["steps"]), 32)
            self.assertTrue(all(len(step_id) <= 128 for step_id in payload["steps"]))
            self.assertTrue(
                all(
                    isinstance(step["error"], dict)
                    and isinstance(step["error"].get("message"), str)
                    and len(step["error"]["message"]) <= 1000
                    for step in payload["steps"].values()
                )
            )
            self.assertTrue(
                all(
                    step["summary"] is None or len(step["summary"]) <= 1000
                    for step in payload["steps"].values()
                )
            )
            self.assertIsInstance(payload["error"], dict)
            for error in [
                payload["error"],
                *(step["error"] for step in payload["steps"].values()),
            ]:
                self.assertEqual(error["type"], "CommandError")
                self.assertEqual(error["code"], "timeout")
                self.assertEqual(error["returncode"], 1)
            self.assertLessEqual(len(payload["error"]["message"]), 3000)
            self.assertLessEqual(len(payload["repo"]), 2000)
            self.assertLessEqual(len(payload["worktree"]), 2000)
            self.assertLessEqual(len(stdout.encode("utf-8")), 12 * 1024)

    def test_compact_state_handles_unserializable_errors(self) -> None:
        class Unserializable:
            def __str__(self) -> str:
                raise RuntimeError("cannot stringify")

        payload = compact_state(
            {
                "run_id": "run-unserializable",
                "status": "failed",
                "steps": {
                    "step": {
                        "error": {
                            "message": Unserializable(),
                            "stderr": Unserializable(),
                        }
                    }
                },
                "error": {
                    "message": Unserializable(),
                    "stderr": Unserializable(),
                },
            }
        )

        self.assertEqual(payload["error"]["message"], "<unserializable>")
        self.assertEqual(payload["error"]["stderr"], "<unserializable>")
        self.assertEqual(
            payload["steps"]["step"]["error"]["message"], "<unserializable>"
        )
        string_payload = compact_state(
            {
                "steps": {"step": {"error": "plain step error"}},
                "error": "plain run error",
            }
        )
        self.assertEqual(string_payload["error"], "plain run error")
        self.assertEqual(string_payload["steps"]["step"]["error"], "plain step error")
        json.dumps(payload, ensure_ascii=False, separators=(",", ":"))

    def test_compact_state_collapses_unknown_error_dict_to_message(self) -> None:
        payload = compact_state(
            {
                "steps": {"step": {"error": {"details": "unknown"}}},
                "error": {"details": "unknown"},
            }
        )

        self.assertEqual(payload["error"], {"message": '{"details":"unknown"}'})
        self.assertEqual(
            payload["steps"]["step"]["error"], {"message": '{"details":"unknown"}'}
        )

    def test_wait_ctrl_c_does_not_request_run_cancellation(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            store = RunStore(root / "runs")
            self._write_state(store, "run-running", "running", "2025-01-01T00:00:00Z")

            with patch("panopticon.state.time.sleep", side_effect=KeyboardInterrupt):
                code, payload, _ = self._run_wait(
                    root,
                    "--run-id",
                    "run-running",
                    "--timeout-seconds",
                    "1",
                    "--interval-seconds",
                    "0.2",
                )

            self.assertEqual(code, 130)
            self.assertEqual(payload["status"], "running")
            self.assertFalse((store.run_dir("run-running") / "cancel.request").exists())

    def test_wait_without_run_id_uses_latest_run(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            store = RunStore(root / "runs")
            self._write_state(store, "run-old", "completed", "2025-01-01T00:00:00Z")
            self._write_state(store, "run-latest", "blocked", "2025-01-01T00:00:01Z")

            code, payload, _ = self._run_wait(root, "--timeout-seconds", "1")

            self.assertEqual(code, 2)
            self.assertEqual(payload["run_id"], "run-latest")
            self.assertEqual(payload["status"], "blocked")

    @staticmethod
    def _run_cli(root: Path, *arguments: str) -> tuple[int, dict[str, Any]]:
        output = StringIO()
        environment = os.environ.copy()
        environment.update(
            {
                "HERDR_ENV": "1",
                "PANOPTICON_STATE_DIR": str(root / "runs"),
            }
        )
        with patch.dict(os.environ, environment, clear=True), redirect_stdout(output):
            code = main([*arguments, "--state-root", str(root / "runs")])
        return code, json.loads(output.getvalue())

    def test_status_and_cancel_return_compact_snapshots(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            store = RunStore(root / "runs")
            self._write_state(
                store,
                "run-status",
                "running",
                "2025-01-01T00:00:00Z",
                error={"message": "status error"},
            )

            status_code, status_payload = self._run_cli(
                root, "status", "--run-id", "run-status"
            )
            cancel_code, cancel_payload = self._run_cli(root, "cancel", "run-status")

            expected_keys = {
                "run_id",
                "status",
                "current_step",
                "repo",
                "worktree",
                "steps",
                "error",
                "updated_at",
            }
            self.assertEqual(status_code, 0)
            self.assertEqual(set(status_payload), expected_keys)
            self.assertEqual(status_payload["worktree"], "/tmp/worktree")
            self.assertEqual(status_payload["error"]["message"], "status error")
            self.assertNotIn("events", status_payload)
            self.assertEqual(cancel_code, 3)
            self.assertEqual(set(cancel_payload), expected_keys)
            self.assertEqual(cancel_payload["status"], "cancelled")
            self.assertEqual(cancel_payload["worktree"], "/tmp/worktree")
            self.assertNotIn("events", cancel_payload)


if __name__ == "__main__":
    unittest.main()
