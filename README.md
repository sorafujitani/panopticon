# Panopticon

Panopticon は、Herdr 0.8.2 の workspace / tab / pane / agent を使って、宣言的な workflow を再開可能な run として実行する Go CLI です。workflow は TOML、prompt と step の成果物は Markdown/JSON、run の観測状態は atomic な `state.json` に保存します。

## 前提

- Go 1.23 以上（実行時の Python 依存はありません）
- Herdr 管理ペイン内で実行する場合は `HERDR_ENV=1`
- Herdr 0.8.2

## インストール

```sh
./scripts/install.sh
```

`go build ./cmd/panopticon` で Go binary を作り、`~/.local/bin/panopticon` にはリポジトリ内の `bin/panopticon` wrapper を安全に symlink します。wrapper は必要時だけ binary を再ビルドします。既存の別ファイル・別 symlink は上書きせず、同じ symlink には冪等に再実行できます。

インストールせずに実行する場合:

```sh
HERDR_ENV=1 ./bin/panopticon doctor --repo .
```

直接 binary を作る場合:

```sh
go build -o .panopticon-bin ./cmd/panopticon
HERDR_ENV=1 ./.panopticon-bin doctor --repo .
```

標準 workflow と prompt は binary にも埋め込まれているため、インストール先からも `standard` を解決できます。対象 repository に `workflows/<name>.toml` または `.panopticon.toml` があればそちらを優先します。

## クイックスタート

```sh
HERDR_ENV=1 panopticon doctor --repo . --workflow standard
HERDR_ENV=1 panopticon dry-run --repo . "依頼を調査・実装・検証する"
HERDR_ENV=1 panopticon start --repo . "依頼を調査・実装・検証する"
```

`start` は既定で orchestrator pane を作り、`resume` をバックグラウンド起動します。現在の process で実行する場合:

```sh
HERDR_ENV=1 panopticon start --foreground --repo . "依頼を実装する"
```

## CLI

全コマンドで `--state-root DIR` と `--herdr-bin PATH` を指定できます。既定の state 保存先は `~/.local/state/panopticon/runs/`、環境変数での指定は `PANOPTICON_STATE_DIR` です。

- `start [--repo DIR] [--workflow NAME_OR_PATH] [--verify 'argv'] [--foreground|--background] [--no-worktree] [--worktree-path DIR] [--branch BRANCH] [--base REF] [TASK]`
- `dry-run [--repo DIR] [--workflow NAME_OR_PATH] [--verify 'argv'] [--no-worktree] [TASK]`
- `status [RUN_ID] [--run-id RUN_ID]` — compact JSON
- `show [RUN_ID] [--run-id RUN_ID] [--step STEP_ID]` — 完全 state または step
- `wait [--run-id RUN_ID] [--timeout-seconds N] [--interval-seconds N]`
- `list`
- `resume --run-id RUN_ID`
- `cancel RUN_ID`
- `cleanup --run-id RUN_ID` または `cleanup --all [--older-than-hours N]`
- `doctor [--repo DIR] [--workflow NAME_OR_PATH] [--verify 'argv']`

終了コードは従来どおり、`wait` の `completed=0`、`blocked=2`、`failed/cancelled=1`、timeout=`124` です。`start/status/show/resume/cancel` は `failed=1`、`blocked/cancel_requested=2`、`cancelled=3` を返します。Ctrl-C の `wait` は run を cancel せず `130` で終了します。

## workflow / contract

`workflows/standard.toml` の `version`、step DAG、`read_policy`、`write_policy`、timeout、`reuse_agent`、`submit_key`、`agent_args`、result contract を Go が検証します。workflow の digest は state に保存され、resume 時に変更を検出します。

検証コマンドは shell 文字列として実行せず、引用付き CLI 値を argv に分解して `shell=false` 相当の直接実行を行います。

```sh
panopticon start \
  --verify 'go test ./...' \
  --verify 'go vet ./...' \
  "依頼"
```

解決順は CLI `--verify`、repository の `.panopticon.toml`、workflow の `default_verify` です。repository 設定例:

```toml
workflow = "standard"

[verification]
commands = [["go", "test", "./..."], ["go", "vet", "./..."]]
```

各 step は指定された `result.json` contract を検証し、artifact path は run directory または worktree 内に制限されます。compact JSON は step 32 件、path/text の上限、全体 12 KiB 上限を守り、巨大な events/snapshot は含めません。

## cleanup lifecycle

run 自身が作成した resource だけを state に所有情報として記録します。既存 workspace、再利用 agent、ユーザーが用意した worktree は所有扱いにしません。

- `completed` / `failed` / `cancelled` 到達時、所有する agent の tab/pane、orchestrator resource、専用 worktree を best-effort・冪等に cleanup します。
- 部分 provision 失敗と cancel でも、保存済み所有情報から補償 cleanup を試みます。
- terminal 到達後も state、step の `result.json`、verification artifact は残るため、観測と監査ができます。
- 実行中の専用 worktree 内から terminal になった場合は、親 repository cwd の detached cleanup worker に委譲します。
- `blocked` は resume のため resource を保持します。
- 明示 `cleanup` は resource cleanup を行い、`--remove-worktree` を付けた場合は所有 worktree も削除してから state directory を削除します。cleanup が部分失敗した state は残ります。

```sh
panopticon cleanup --run-id RUN_ID --remove-worktree
panopticon cleanup --all --older-than-hours 24 --remove-worktree
```

failed run の resume は cleanup 済み resource/worktree を同じ workflow・branch・path で再 provision します。agent が未保存のまま作った変更は cleanup の対象になり得るため、重要な変更は contract artifact または commit 済み branch に保存してください。

## Herdr 境界と再帰防止

agent tab 作成時には `PANOPTICON_CHILD=1` を argv ではなく Herdr の環境設定として渡します。Go CLI はこの値の child process からの `start` を拒否し、標準 workflow の child agent が Panopticon を再帰起動することを防ぎます。`HERDR_ENV=1` がない通常 CLI は doctor 以外を拒否します。

## Pi extension

リポジトリ内の `.pi/extensions/panopticon.ts` は次を提供します。

- agent 内 tool: `herdr_orchestrate`
- user slash command: `/panopticon <依頼>`

両方とも `node:child_process.spawnFile` の argv 配列で `bin/panopticon` を呼び、shell command string を組み立てません。child 環境では extension 自体が orchestration surface を登録しません。

Pi の project trust 後に extension が自動検出されます。必要なら `pi -e ./.pi/extensions/panopticon.ts` で明示ロードしてください。

## 開発者向け検証

```sh
go test ./...
go vet ./...
HERDR_ENV=1 go run ./cmd/panopticon doctor --repo .
HERDR_ENV=1 go run ./cmd/panopticon dry-run --repo . --workflow standard
```

実装は `cmd/panopticon` と `internal/panopticon` に集約されています。TOML、Herdr JSON adapter、atomic state/lock、workflow engine、cleanup lifecycle、CLI を Go tests で検証します。
