---
name: panopticon
description: Explicitly start and monitor a Panopticon workflow run via the panopticon CLI instead of typing commands by hand. INVOKE ONLY when the user explicitly says "panopticon start" / "$panopticon" / "panopticon で開始" or asks to start/resume/check a Panopticon run by name. Do NOT trigger from generic multi-step task requests — opt-in only. Covers start (background default), run_id capture, status/wait monitoring with exit-code semantics, resume for blocked runs, and cancel/cleanup guidance.
---

# Panopticon

`panopticon` CLI を使ってワークフロー実行 (run) を明示的に開始・監視するためのスキル。
CLI を手打ちする代わりに、エージェントがこの手順に従って安全に `start` 〜 `wait` までを実行する。

## 前提チェック（必ず最初に行う）

`start` を実行する前に以下を確認し、どちらか一方でも満たさなければ**開始せずに中断して**ユーザーに伝える。

1. **HERDR_ENV=1 が必要** — herdr 管理外のプロセスからは `doctor` 以外のコマンドが全て拒否される。
   ```sh
   [ "${HERDR_ENV:-}" = "1" ] || echo "NOT_IN_HERDR"
   ```
   `NOT_IN_HERDR` の場合は失敗理由を説明する: 「herdr のペイン (HERDR_ENV=1) から実行してください。環境変数を偽装しないこと」。
2. **PANOPTICON_CHILD=1 では開始できない** — Panopticon が起動した子エージェントからの再帰 start は設計上ブロックされる。
   ```sh
   [ "${PANOPTICON_CHILD:-}" = "1" ] && echo "IS_PANOPTICON_CHILD"
   ```
   子ペインから呼ばれた場合は「子エージェントからは start できません。親オーケストレータに任せるか、通常の CLI ターミナルから実行してください」と伝えて停止する。

両方クリアしたら `HERDR_ENV=1 panopticon doctor --repo <REPO>` で構成を先に検証できる（任意）。

## run の開始

ユーザーの指示から TASK（任意）、workflow 名（任意）を決める。workflow を指定しない場合はリポジトリの `.panopticon.toml` → ユーザー設定 → 埋め込み `standard` の順で解決されるため、通常は `--workflow` を省略して既存設定に任せる。

```sh
HERDR_ENV=1 panopticon start --repo <REPO_DIR> [--workflow NAME] [--verify 'cmd...'] "TASK TEXT"
```

- 既定はバックグラウンド実行: オーケストレータペインを作成して即座に返る。
- 標準出力は CompactState JSON。ここから `run_id` を抽出して以降のコマンドに使うこと（jq があれば `| jq -r .run_id`、無ければ出力全体をユーザーに見せて run_id を確認）。
- 成功しても run はまだ完了していない。**run_id を取得したら必ず監視へ進む。**

## 監視

```sh
HERDR_ENV=1 panopticon wait --run-id <RUN_ID> --timeout-seconds 1800
```

`wait` の終了コード:

| code | 意味 | エージェントの対応 |
|---|---|---|
| 0 | completed | 完了を報告。`status` / `show --run-id <RUN_ID>` で結果サマリを提示 |
| 2 | blocked | リソース保持は意図的。cleanup しない。`show` で該当 step を確認し、ユーザーに `resume --run-id <RUN_ID>` を提案 |
| 1 | failed / cancelled | `show` で error を確認して報告。自動 cleanup しない |
| 124 | timeout | 監視がタイムアウトしただけ。run は継続中。`status` で現状を報告し再 wait するかユーザーに判断を仰ぐ |

Ctrl-C (`130`) は run をキャンセルしない。長時間 run では適切な `--timeout-seconds` を設定し、タイムアウト時に状況を報告して継続可否をユーザーに確認する。

## 後片付け

- `cancel <RUN_ID>`: 実行中 run の中止（ユーザーが明示依頼した場合のみ）。
- `cleanup --run-id <RUN_ID> [--remove-worktree]`: 完了/失敗 run の状態と worktree を削除。worktree に未保存の変更がないかユーザーに確認してから行う。blocked run に対しては resume が優先で、勝手に cleanup しない。

## 禁止事項

- `HERDR_ENV` / `PANOPTICON_CHILD` を偽装して gate を迂回しない。
- git identity の捏造、GitHub リポジトリの自動作成を行わない。
- run の push / merge / commit を自動で行わない（明示依頼がある場合のみ）。
