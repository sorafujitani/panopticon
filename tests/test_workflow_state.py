import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from helpers import ROOT

from panopticon.engine import dry_run_payload
from panopticon.state import RunStore, atomic_write_json
from panopticon.workflow import (
    WorkflowError,
    load_repo_config,
    load_workflow,
    resolve_verify_commands,
    resolve_workflow_path,
)


class WorkflowTests(unittest.TestCase):
    def test_standard_workflow_is_parsed_and_graph_is_ordered(self) -> None:
        workflow = load_workflow(ROOT / "workflows" / "standard.toml")

        self.assertEqual(workflow.name, "standard")
        self.assertEqual(
            [step.id for step in workflow.steps],
            ["scout", "developer", "reviewer", "fixer", "verifier"],
        )
        self.assertEqual(workflow.step_map["developer"].depends_on, ("scout",))
        self.assertEqual(workflow.step_map["fixer"].condition, "reviewer.needs_fixer")
        self.assertTrue(all(step.kind == "pi" for step in workflow.steps))
        self.assertTrue(all(step.submit_key == "ctrl+enter" for step in workflow.steps))
        self.assertTrue(
            all(step.agent_args == ("--no-extensions",) for step in workflow.steps)
        )
        self.assertEqual(workflow.default_verify, (("git", "diff", "--check"),))

    def test_agent_args_are_parsed_serialized_and_change_digest(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "prompt.md").write_text("prompt", encoding="utf-8")
            workflow_path = root / "custom.toml"
            content = """version = 1
name = "custom"
default_verify = [["python3", "-c", "pass"]]

[[steps]]
id = "step"
role = "developer"
kind = "pi"
depends_on = []
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"
submit_key = "ctrl+enter"
agent_args = ["--profile", "profile with spaces"]

[steps.contract]
required_fields = ["schema_version"]
artifact_kinds = ["report"]
"""
            workflow_path.write_text(content, encoding="utf-8")
            workflow = load_workflow(workflow_path)
            workflow_path.write_text(
                content.replace("profile with spaces", "different profile"),
                encoding="utf-8",
            )
            changed_workflow = load_workflow(workflow_path)

        self.assertEqual(
            workflow.steps[0].agent_args,
            ("--profile", "profile with spaces"),
        )
        self.assertEqual(
            workflow.as_dict()["steps"][0]["agent_args"],
            ["--profile", "profile with spaces"],
        )
        self.assertNotEqual(workflow.digest, changed_workflow.digest)

    def test_empty_agent_arg_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "prompt.md").write_text("prompt", encoding="utf-8")
            (root / "custom.toml").write_text(
                """version = 1
name = "custom"
default_verify = [["python3", "-c", "pass"]]

[[steps]]
id = "step"
role = "developer"
kind = "pi"
depends_on = []
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"
agent_args = [" "]

[steps.contract]
required_fields = ["schema_version"]
artifact_kinds = ["report"]
""",
                encoding="utf-8",
            )

            with self.assertRaisesRegex(WorkflowError, "agent_args"):
                load_workflow(root / "custom.toml")

    def test_toml_filename_resolves_from_workflows_directory(self) -> None:
        path = resolve_workflow_path(ROOT, "standard.toml", ROOT)

        self.assertEqual(path, (ROOT / "workflows" / "standard.toml").resolve())

    def test_invalid_workflow_cycle_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "prompt.md").write_text("prompt", encoding="utf-8")
            (root / "cycle.toml").write_text(
                """version = 1
name = "cycle"
default_verify = [["python3", "-c", "pass"]]

[[steps]]
id = "first"
role = "scout"
kind = "codex"
depends_on = ["second"]
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"

[steps.contract]
required_fields = ["schema_version"]
artifact_kinds = ["report"]

[[steps]]
id = "second"
role = "scout"
kind = "codex"
depends_on = ["first"]
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"

[steps.contract]
required_fields = ["schema_version"]
artifact_kinds = ["report"]
""",
                encoding="utf-8",
            )

            with self.assertRaisesRegex(WorkflowError, "循環依存"):
                load_workflow(root / "cycle.toml")

    def test_verify_cli_value_remains_an_argv(self) -> None:
        workflow = load_workflow(ROOT / "workflows" / "standard.toml")

        commands = resolve_verify_commands(
            ["python3 -m unittest discover -s tests -v"], {}, workflow
        )

        self.assertEqual(
            commands,
            (("python3", "-m", "unittest", "discover", "-s", "tests", "-v"),),
        )


class StateTests(unittest.TestCase):
    def test_default_store_uses_panopticon_state_namespace(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            with (
                patch.dict(os.environ, {}, clear=True),
                patch("panopticon.state.Path.home", return_value=home),
            ):
                store = RunStore()

            self.assertEqual(
                store.root,
                (home / ".local" / "state" / "panopticon" / "runs").resolve(),
            )

    def test_atomic_write_and_run_store_are_durable_shapes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "state.json"
            atomic_write_json(path, {"status": "created", "value": 1})
            atomic_write_json(path, {"status": "running", "value": 2})

            self.assertEqual(json.loads(path.read_text(encoding="utf-8"))["value"], 2)
            self.assertEqual(list(root.glob(".state.json.*.tmp")), [])

            store = RunStore(root / "runs")
            store.create("run-test", {"run_id": "run-test", "status": "created"})
            state = store.load("run-test")
            state["status"] = "completed"
            store.save("run-test", state)
            self.assertEqual(store.list()[0]["status"], "completed")

            with store.lock("run-test"):
                self.assertTrue((store.run_dir("run-test") / "run.lock").exists())
            self.assertFalse((store.run_dir("run-test") / "run.lock").exists())


class RepoConfigTests(unittest.TestCase):
    def test_loads_panopticon_repository_config(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            (repo / ".panopticon.toml").write_text(
                'workflow = "standard"\n', encoding="utf-8"
            )

            self.assertEqual(load_repo_config(repo), {"workflow": "standard"})


class DryRunTests(unittest.TestCase):
    def test_dry_run_has_no_state_side_effect(self) -> None:
        workflow = load_workflow(ROOT / "workflows" / "standard.toml")
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory) / "repo"
            repo.mkdir()
            payload = dry_run_payload(
                repo,
                workflow,
                "調査する",
                workflow.default_verify,
            )

            self.assertTrue(payload["dry_run"])
            self.assertFalse((repo / ".panopticon").exists())
            self.assertFalse(payload["worktree"]["destructive_integration"])
            self.assertTrue(
                all(
                    step["agent_args"] == ["--no-extensions"]
                    for step in payload["execution"]
                )
            )


if __name__ == "__main__":
    unittest.main()
