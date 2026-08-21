import json
from pathlib import Path

from panopticon.workflow import Workflow, load_workflow

ROOT = Path(__file__).resolve().parents[1]
FAKE_HERDR = Path(__file__).resolve().with_name("fake_herdr.py")


def write_single_step_workflow(
    root: Path,
    *,
    role: str = "developer",
    write_policy: str = "worktree",
    submit_key: str | None = None,
    agent_args: tuple[str, ...] | None = None,
    timeout_seconds: int = 30,
) -> Workflow:
    root.mkdir(parents=True, exist_ok=True)
    prompt = (
        "RESULT_PATH={{RESULT_PATH}}\n"
        "- 作業対象は専用 worktree のみ: `{{WORKTREE_PATH}}`\n"
        "- worktree: `{{WORKTREE_PATH}}`\n"
        "- run_id: `{{RUN_ID}}`\n"
        "- step_id: `{{STEP_ID}}`\n"
        "- role: `{{ROLE}}`\n"
    )
    if role == "verifier":
        prompt += "ENGINE_VERIFICATION={{ENGINE_VERIFICATION}}\n"
    prompt += "## JSON contract\n{{JSON_CONTRACT}}\n"
    (root / "prompt.md").write_text(prompt, encoding="utf-8")
    if role == "verifier":
        required_fields = (
            '["schema_version", "run_id", "step_id", "role", "status", '
            '"summary", "artifacts", "verified", "verification"]'
        )
        artifact_kinds = '["verification"]'
        required_boolean_fields = '["verified"]'
        required_list_fields = '["verification"]'
    else:
        required_fields = (
            '["schema_version", "run_id", "step_id", "role", "status", '
            '"summary", "artifacts", "changed_files"]'
        )
        artifact_kinds = '["report"]'
        required_boolean_fields = "[]"
        required_list_fields = '["changed_files"]'
    workflow_path = root / "workflow.toml"
    submit_key_line = f'submit_key = "{submit_key}"\n' if submit_key is not None else ""
    agent_args_line = (
        f"agent_args = {json.dumps(list(agent_args), ensure_ascii=False)}\n"
        if agent_args is not None
        else ""
    )
    workflow_path.write_text(
        f'''version = 1
name = "test"
default_verify = [["python3", "-c", "pass"]]

[[steps]]
id = "step"
role = "{role}"
kind = "codex"
depends_on = []
read_policy = "repo-and-dependencies"
write_policy = "{write_policy}"
timeout_seconds = {timeout_seconds}
{submit_key_line}{agent_args_line}template = "prompt.md"

[steps.contract]
required_fields = {required_fields}
artifact_kinds = {artifact_kinds}
required_boolean_fields = {required_boolean_fields}
required_list_fields = {required_list_fields}
''',
        encoding="utf-8",
    )
    return load_workflow(workflow_path)
