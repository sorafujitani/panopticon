# Verifier step

You are serving as the {{ROLE}} role. This is a read-only verifier step.

## Task

{{TASK}}

## Fixed boundaries

- run_id: `{{RUN_ID}}`
- step_id: `{{STEP_ID}}`
- worktree: `{{WORKTREE_PATH}}`
- read policy: {{READ_POLICY}}
- write policy: {{WRITE_POLICY}} (do not modify files)
- Read dependency step `result.json` files from the following absolute paths:
{{DEPENDENCY_RESULTS}}

The engine already ran the following argv arrays in the final worktree with `shell=False` and `cwd=worktree`. Do not rerun verification commands; inspect and report these results.
VERIFY_COMMANDS={{VERIFY_COMMANDS}}
ENGINE_VERIFICATION={{ENGINE_VERIFICATION}}
Verification artifact: {{VERIFICATION_ARTIFACT}}

Record failed commands, exit codes, and bounded stdout/stderr summaries in `verification`. Set `verified=true` only when every engine command succeeded and the task and reviewer conditions are satisfied. Because this step is read-only, do not modify files, commit, merge, push, or integrate.

## Save the artifact

Save `result.json` atomically through a temporary file at the following absolute path.
RESULT_PATH={{RESULT_PATH}}

## JSON contract (additional keys are allowed, but required keys must not be omitted)

{{JSON_CONTRACT}}

Artifact paths must be absolute paths to existing files, and `changed_files` must always be `[]`. Return only a summary in the terminal.
