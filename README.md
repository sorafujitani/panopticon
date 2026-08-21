# Panopticon

Panopticon is a Go CLI that uses Herdr 0.8.2 workspaces, tabs, panes, and agents to run declarative workflows as resumable runs. Workflows are TOML, prompt and step artifacts are Markdown/JSON, and run state is stored atomically in `state.json`.

## Prerequisites

- Go 1.23 or later (Python is not required at runtime)
- `HERDR_ENV=1` when running inside a Herdr-managed pane
- Herdr 0.8.2

## Installation

```sh
./scripts/install.sh
```

`go build ./cmd/panopticon` builds the Go binary, and `~/.local/bin/panopticon` is safely symlinked to the repository's `bin/panopticon` wrapper. The wrapper rebuilds the binary only when needed. It does not overwrite an existing file or symlink, and rerunning it with the same symlink is idempotent.

To run without installing:

```sh
HERDR_ENV=1 ./bin/panopticon doctor --repo .
```

To build and run the binary directly:

```sh
go build -o .panopticon-bin ./cmd/panopticon
HERDR_ENV=1 ./.panopticon-bin doctor --repo .
```

The standard workflow and prompts are embedded in the binary, so `standard` can also be resolved from the installation directory. If the target repository contains `workflows/<name>.toml` or `.panopticon.toml`, that configuration takes precedence.

## Quick start

```sh
HERDR_ENV=1 panopticon doctor --repo . --workflow standard
HERDR_ENV=1 panopticon dry-run --repo . "Investigate, implement, and verify the request"
HERDR_ENV=1 panopticon start --repo . "Investigate, implement, and verify the request"
```

By default, `start` creates an orchestrator pane and launches `resume` in the background. To run in the current process:

```sh
HERDR_ENV=1 panopticon start --foreground --repo . "Implement the request"
```

## CLI

All commands accept `--state-root DIR` and `--herdr-bin PATH`. The default state directory is `~/.local/state/panopticon/runs/`; configure it with `PANOPTICON_STATE_DIR`.

- `start [--repo DIR] [--workflow NAME_OR_PATH] [--verify 'argv'] [--foreground|--background] [--no-worktree] [--worktree-path DIR] [--branch BRANCH] [--base REF] [TASK]`
- `dry-run [--repo DIR] [--workflow NAME_OR_PATH] [--verify 'argv'] [--no-worktree] [TASK]`
- `status [RUN_ID] [--run-id RUN_ID]` - compact JSON
- `show [RUN_ID] [--run-id RUN_ID] [--step STEP_ID]` - complete state or step
- `wait [--run-id RUN_ID] [--timeout-seconds N] [--interval-seconds N]`
- `list`
- `resume --run-id RUN_ID`
- `cancel RUN_ID`
- `cleanup --run-id RUN_ID` or `cleanup --all [--older-than-hours N]`
- `doctor [--repo DIR] [--workflow NAME_OR_PATH] [--verify 'argv']`

Exit codes remain unchanged: `wait` returns `completed=0`, `blocked=2`, `failed/cancelled=1`, and timeout=`124`. `start/status/show/resume/cancel` return `failed=1`, `blocked/cancel_requested=2`, and `cancelled=3`. Pressing Ctrl-C during `wait` does not cancel the run and exits with `130`.

## Workflow and contract

Go validates the `version`, step DAG, `read_policy`, `write_policy`, timeout, `reuse_agent`, `submit_key`, `agent_args`, and result contract in `workflows/standard.toml`. The workflow digest is stored in state and changes are detected when resuming.

Verification commands are not run as shell strings. Quoted CLI values are split into argv and executed directly, equivalent to `shell=false`.

```sh
panopticon start \
  --verify 'go test ./...' \
  --verify 'go vet ./...' \
  "TASK"
```

The resolution order is CLI `--verify`, the repository's `.panopticon.toml`, then the workflow's `default_verify`. Example repository configuration:

```toml
workflow = "standard"

[verification]
commands = [["go", "test", "./..."], ["go", "vet", "./..."]]
```

Each step validates the specified `result.json` contract. Artifact paths are always restricted to the run directory or the current worktree. Steps with `read_policy = "repo-and-dependencies"` may also read the target repository and other Git worktrees of the same repository. All paths are validated as absolute, canonical paths to existing files. Compact JSON is limited to 32 steps, bounded path/text fields, 256-character error identifiers, and 12 KiB overall; large events and snapshots are omitted.

## Cleanup lifecycle

Only resources created by the run itself are recorded as owned in state. Existing workspaces, reused agents, and worktrees provided by the user are not treated as owned.

- When a run reaches `completed`, `failed`, or `cancelled`, owned agent tabs/panes, orchestrator resources, and dedicated worktrees are cleaned up best-effort and idempotently.
- Partial provisioning failures and cancellation also trigger compensating cleanup from the saved ownership data.
- State, each step's `result.json`, and verification artifacts remain after a terminal state for observation and auditing.
- If a run reaches a terminal state from inside its dedicated worktree, cleanup is delegated to a detached worker running from the parent repository directory.
- `blocked` retains resources so the run can be resumed.
- Explicit `cleanup` removes resources. With `--remove-worktree`, it also removes owned worktrees before deleting the state directory. State remains when cleanup is only partially successful.

```sh
panopticon cleanup --run-id RUN_ID --remove-worktree
panopticon cleanup --all --older-than-hours 24 --remove-worktree
```

Resuming a failed run reprovisions cleaned-up resources and worktrees with the same workflow, branch, and path. Changes made by an agent without being saved may be removed during cleanup, so save important changes in a contract artifact or a committed branch.

## Herdr boundary and recursion prevention

When creating an agent tab, `PANOPTICON_CHILD=1` is passed as a Herdr environment setting rather than an argv value. The Go CLI rejects `start` from a child process with this value, preventing child agents in the standard workflow from recursively starting Panopticon. A normal CLI without `HERDR_ENV=1` rejects every command except `doctor`.

## Development verification

```sh
go test ./...
go vet ./...
HERDR_ENV=1 go run ./cmd/panopticon doctor --repo .
HERDR_ENV=1 go run ./cmd/panopticon dry-run --repo . --workflow standard
```

Implementation is concentrated in `cmd/panopticon` and `internal/panopticon`. Go tests cover TOML parsing, the Herdr JSON adapter, atomic state and locks, the workflow engine, cleanup lifecycle, and the CLI.
