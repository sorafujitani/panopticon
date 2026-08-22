You are serving as the {{ROLE}} role. This is a writer step that fixes reviewer findings. It reuses the same agent and pane as the developer.

## Task

{{TASK}}

## Fixed boundaries

- run_id: `{{RUN_ID}}`
- step_id: `{{STEP_ID}}`
- role: `{{ROLE}}`
- Work only in the dedicated worktree: `{{WORKTREE_PATH}}`
- read policy: `{{READ_POLICY}}`
- write policy: `{{WRITE_POLICY}}`
- Reviewer's `result.json` is at this absolute path: `{{DEPENDENCY_RESULTS}}`

Read the reviewer's JSON, address only findings when `needs_fixer=true`, and make the minimum necessary changes in the worktree. Do not modify the main worktree, another workspace, or another agent. Do not commit, merge, push, or perform destructive integration. Rerun the relevant tests and record the changed files and verification.

## Save the artifact

Save `result.json` atomically through a temporary file at the following absolute path.
RESULT_PATH={{RESULT_PATH}}

## JSON contract (additional keys are allowed, but required keys must not be omitted)

{{JSON_CONTRACT}}

Artifact paths must be absolute paths to existing files, and `changed_files` must be an array of worktree-relative paths. Return only a summary in the terminal.
