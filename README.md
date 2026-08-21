# Panopticon

Panopticon は、Herdr 0.8.2 の workspace / tab / pane / agent を使って、調査・実装・レビュー・修正・検証を再開可能な run として実行する小さな CLI です。workflow と prompt は TOML/Markdown で宣言し、実行状態と各 step の成果物を JSON で保存します。

実装は Python 標準ライブラリだけに依存します。Python 3.11 以上（`tomllib` が必要）と、Herdr 管理ペイン内での実行（`HERDR_ENV=1`）が必要です。

## インストール

リポジトリのルートで次を一度実行します。

```sh
./scripts/install.sh
```

`~/.local/bin/panopticon` に、このリポジトリの `bin/panopticon` への symlink を作ります。既存の別ファイルや別 symlink は上書きせず、同じ symlink に対しては安全に再実行できます。`~/.local/bin` が PATH にない場合は、シェルの PATH に追加してください。

インストールせずに実行する場合は `./bin/panopticon` を使えます。

## クイックスタート

まず環境を確認します。

```sh
HERDR_ENV=1 panopticon doctor --repo .
```

副作用のない計画確認を行い、問題がなければ開始します。

```sh
HERDR_ENV=1 panopticon dry-run --repo . "この依頼を調査して実装する"
HERDR_ENV=1 panopticon start --repo . "この依頼を調査して実装する"
```

`start` は既定で orchestrator 用の Herdr pane を作り、バックグラウンドで `resume` を起動します。現在のプロセスで完了まで実行する場合は `--foreground` を指定します。

```sh
HERDR_ENV=1 panopticon start --foreground --repo . "この依頼を調査して実装する"
```

## コマンド

全コマンドで利用できる主なオプションは `--state-root DIR`（run state の保存先）と `--herdr-bin PATH`（Herdr 実行ファイル）です。通常の実行コマンドは Herdr 管理ペイン内でのみ動作します。

### `start`

新しい run を作成して実行します。

```sh
panopticon start [--repo DIR] [--workflow NAME_OR_PATH] [--verify 'argv'] \
  [--foreground|--background] [--no-worktree] \
  [--worktree-path DIR] [--branch BRANCH] [--base REF] [TASK]
```

既定では依頼ごとに専用 worktree を作ります。`--no-worktree` は対象 repository を直接使う明示的な例外です。`--task TEXT` でも依頼を指定できます（位置引数より優先）。`start` の標準出力は `wait` と同じ compact JSON（pretty表示、最大12KiB）です。

### `dry-run`

Herdr resource、state、worktree、agent を作らず、解決された workflow、検証 argv、依存関係、成果物 contract を JSON で表示します。

```sh
panopticon dry-run --repo . --workflow standard "依頼内容"
```

### `status` / `show`

`status` は `wait` と同じ compact JSON（pretty表示）の概要、`show` は完全な state を表示します。run ID を省略すると、更新日時が最も新しい run を選びます。

```sh
panopticon status
panopticon status --run-id RUN_ID
panopticon show --run-id RUN_ID
panopticon show --run-id RUN_ID --step developer
```

### `wait`

run の `completed` / `failed` / `cancelled` / `blocked` を `state.json` の atomic snapshot で監視します。`--run-id` を省略すると最新の run を選びます。監視間隔は 0.2〜30 秒、タイムアウトは正数です。タイムアウト時も最後の compact JSON を出力します。終了コードは completed=0、blocked=2、failed/cancelled=1、timeout=124 です。Ctrl-C は待機だけを終了し、run の cancel は要求しません（130）。

```sh
panopticon wait [--run-id RUN_ID] [--timeout-seconds N] [--interval-seconds N]
```

出力には `run_id`、`status`、`current_step`、`repo`、`worktree`、step ごとの `status` / `summary` / `error`、run の `error`、`updated_at` だけを含みます。step は最大32件、step id は128文字、step の `summary` と `error` 内の各文字列は各1000文字、run の `error` 内の各文字列は各3000文字、`repo` / `worktree` は各2000文字に制限します。`error` が文字列なら文字列のまま、object なら `type` / `code` / `message` / `returncode` / `stderr` を直接読める bounded object として表示し、未知の object は `message` に縮退します。compact JSON は indent=2 の pretty表示と末尾改行を含む実出力で12KiB以下に抑えます。events や worktree snapshot などの巨大な state は含みません。

### `list`

保存されている run の一覧を表示します。

```sh
panopticon list
```

### `resume`

`failed`、`blocked`、中断状態の run を、保存済み state と成果物を使って再開します。完了済み run は再実行しません。

```sh
panopticon resume --run-id RUN_ID
```

各 run は exclusive lock で保護され、同じ run の同時 resume を防ぎます。agent が生成済みの `result.json` は再利用されるため、途中終了後も step の境界から継続できます。

### `cancel`

実行中の run をキャンセルします。別プロセスが state lock を保持している場合は、キャンセル要求ファイルを書き、次の安全な境界で停止します。標準出力は `wait` と同じ compact JSON で、lock競合時も事前に読み込んだ state の `repo` / `worktree` / `current_step` / `steps` / `error` / `updated_at` を保持します。

```sh
panopticon cancel RUN_ID
```

### `cleanup`

terminal 状態（`completed` / `failed` / `cancelled`）の state を削除します。active な run は削除しません。

```sh
panopticon cleanup --run-id RUN_ID
panopticon cleanup --all --older-than-hours 24
```

Herdr が作った専用 worktree も削除する場合だけ `--remove-worktree` を明示します。

```sh
panopticon cleanup --run-id RUN_ID --remove-worktree
```

### `doctor`

Herdr 管理環境、Herdr executable、state directory、Herdr version を検査します。`--repo` または `--workflow` を追加すると workflow と検証コマンドも検査します。失敗時も JSON を返すので、管理ペインの外から環境確認に利用できます。

```sh
panopticon doctor
panopticon doctor --repo . --workflow standard
```

## 検証コマンドと `--verify`

検証コマンドは shell 文字列として連結せず、engine が `shell=False`、`cwd=worktree` の argv 配列として順番に実行します。stdout/stderr は bounded に state と `steps/verifier/verification.json` へ保存され、全コマンドが成功した場合だけ verifier は完了します。engine の実行結果は verifier prompt に渡され、agent の `verified=true` だけでは成功になりません。標準 workflow の汎用 `default_verify` は `git diff --check` だけです。実際の project test/lint は任意の repository に `tests/` があることを前提にせず、対象 repository の `.panopticon.toml` の `[verification].commands` または CLI の `--verify` で必ず明示指定してください。設定は workflow の `default_verify`、repository の設定、CLI の `--verify` の順に解決し、CLI 指定が最優先です。`--verify` は複数指定できます。

```sh
panopticon start --verify 'python3 -m unittest discover -s tests -v' \
  --verify 'python3 -m compileall panopticon' \
  "依頼内容"
```

引用符の解析には Python の `shlex.split` を使います。パイプ、リダイレクト、`&&` などの shell 構文は実行されません。必要な場合は検証用の Python script などを argv で指定してください。

## repository 設定: `.panopticon.toml`

対象 repository のルートに `.panopticon.toml` を置くと、workflow と検証コマンドを repository 単位で切り替えられます。

```toml
workflow = "standard"

[verification]
commands = [
  ["python3", "-m", "unittest", "discover", "-s", "tests", "-v"],
  ["python3", "-m", "compileall", "panopticon"],
]
```

`workflow` は workflow 名（`workflows/NAME.toml`）または path です。workflow の検索順は、repository の `workflows/`、repository の `.panopticon/`、この設定リポジトリの `workflows/` です。検証コマンドは `[verification].commands` のほか、互換的に `verify`、`verification_commands`、`verify_commands` も読み取れます。

### カスタム submit binding

workflow step の `submit_key` を指定すると、prompt の貼り付けと送信キーを分離できます。例えば Pi 側で `tui.input.submit = ctrl+enter` に変更している場合は、Pi の step に次を追加します。

```toml
[[steps]]
id = "developer"
role = "developer"
kind = "pi"
# ... depends_on / policy / timeout_seconds / template / contract ...
submit_key = "ctrl+enter"
```

`submit_key` を省略した step は従来どおり `herdr agent prompt --wait` を使います。指定した step は prompt 送信後に Herdr の screen detection fallback を含む監視で working/settled を待ちます。

`agent_args` は agent 起動時の追加引数です。shell 文字列には変換せず、指定した各要素をそのまま argv として `herdr agent start NAME --kind KIND --pane ID --timeout MS -- <args...>` の `--` より後ろへ渡します。空文字要素は拒否されます。

```toml
agent_args = ["--no-extensions"]
```

background の child agent tab には `tab create --env PANOPTICON_CHILD=1` を付けます。agent 側の extension が `panopticon` を再帰起動しないための環境境界です。

## 標準 workflow

この repository の `workflows/standard.toml` は一般的な Codex 用ではなく、このユーザーの個人 Herdr 設定向けです。全 step を Pi、`submit_key = "ctrl+enter"`、`agent_args = ["--no-extensions"]` で起動します。`--no-extensions` により Pi の拡張ツールとポストタスク選択 UI は無効になりますが、Pi の built-in tools と skills は利用できます。

Codex workflow を作る場合、新規 worktree で `Do you trust...` の確認が残っていると agent は blocked になり、unattended 開始できません。trust prompt を事前に解消してから実行してください。

`workflows/standard.toml` は次の順序で動きます。

1. `scout`: 調査（read-only）
2. `developer`: 専用 worktree で実装
3. `reviewer`: read-only レビュー
4. `fixer`: reviewer が `needs_fixer=true` の場合だけ修正
5. `verifier`: 指定された検証コマンドを実行

各 step は `result.json` の JSON contract を満たす必要があります。必須フィールド、artifact の kind、reviewer の判定、verifier の `verified` などは workflow の contract で検証されます。Git worktree では engine が `git status --porcelain=v1 -z --untracked-files=all` を argv 配列・`shell=False` で実行し、dirty path だけを status、mode、内容 digest 込みで前後 snapshot します。そのため、step 開始前から dirty なファイルの再変更、rename/copy、削除、空白や非 ASCII の path も検証できます。status の失敗は fail-closed です。Git ではない repository を `--no-worktree` で明示した場合だけ filesystem fallback を使い、Git repository の `--no-worktree` は通常どおり Git status を使います。ignored path は status の対象外で、通常の untracked path は検出されます。read-only step の変更を拒否し、writer の `changed_files` と実差分を一致検証します。`success`、`blocked`、`failed` は別々に state へ記録されます。workflow TOML の SHA-256 digest も state に保存され、resume 時に現在の TOML と一致しなければ停止します。

## 成果物と state

既定の保存先は `~/.local/state/panopticon/runs/` です。`--state-root` または `PANOPTICON_STATE_DIR` で変更できます。旧 CLI の run を参照する場合は、`--state-root ~/.local/state/herdr-flow/runs` のように旧保存先を明示してください。

```text
RUN_ROOT/
└── RUN_ID/
    ├── state.json
    └── steps/
        └── STEP_ID/
            ├── result.json
            └── verification.json（verifier step のみ）
```

- `state.json`: run の status、step 状態、resource ID、イベント履歴、worktree path、workflow digest、各 step の前後 snapshot と検証結果
- `steps/*/result.json`: agent が atomic に保存する step 成果物
- `steps/verifier/verification.json`: engine が保存する検証 argv、終了コード、bounded stdout/stderr
- `artifacts[].path`: run directory または専用 worktree 内の既存ファイルへの絶対 path
- `changed_files`: worktree 相対 path の配列

state は同一ディレクトリの一時ファイルを fsync してから replace する atomic write です。state directory は可能な範囲で `0700`、JSON ファイルは `0600` に設定されます。

## worktree と安全性

- `start` の既定は Herdr 管理の専用 worktree です。
- `--worktree-path` を使う場合も、agent の cwd と成果物の境界はその path に固定されます。
- main repository への merge、commit、push、破壊的な統合は自動実行しません。
- `--no-worktree` は main repository を直接変更し得るため、明示指定した場合だけ有効です。
- `cleanup` は state だけを既定で削除し、worktree は `--remove-worktree` がある場合だけ、現在の Herdr レスポンスを照合して削除を依頼します。保存済み workspace ID は再利用しません。
- Git worktree の snapshot は dirty path のみを対象にし、status 取得に失敗した場合は変更を安全側に判定します。
- artifact path と writer step の `changed_files` は run/worktree の境界を越えられません。
- cleanup は run lock 内で terminal state を再確認して state を削除します。worktree も削除する場合は、現在の `herdr worktree list --cwd <repo>` で保存 path+branch に一意一致する workspace ID が得られた場合だけ、その ID を使います。

作業中の worktree を残したい場合は `cleanup` に `--remove-worktree` を付けないでください。外部の git 操作や統合は、内容を確認してから利用者が行ってください。

## Panopticon の popup

Herdr の `config.toml` に任意の popup binding を追加すると、run 一覧を popup で表示できます。command には次を指定してください。

```toml
command = "panopticon list"
```

インストール済みの `panopticon` が PATH に必要です。既存の binding と衝突しないキーを割り当ててください。

## 検証

この repository での最低限の検証コマンドは次のとおりです。

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -B -m unittest discover -s tests -v
ruff check --no-cache .  # ruff が利用可能な場合
```

テストは外部サービスを使わず、subprocess 経由の fake Herdr CLI で workflow、atomic state、dry-run、ID 抽出、blocked/failure/resume、doctor を検証します。
