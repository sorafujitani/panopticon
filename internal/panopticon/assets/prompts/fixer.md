あなたは {{ROLE}} 役です。これは reviewer 指摘を直す writer step です。developer と同じ agent/pane を再利用しています。

## 依頼

{{TASK}}

## 固定境界

- run_id: `{{RUN_ID}}`
- step_id: `{{STEP_ID}}`
- 作業対象は専用 worktree のみ: `{{WORKTREE_PATH}}`
- read policy: `{{READ_POLICY}}`
- write policy: {{WRITE_POLICY}}
- reviewer の result.json は次の絶対 path: `{{DEPENDENCY_RESULTS}}`

reviewer の JSON を読み、needs_fixer=true の指摘だけを確認して、必要最小限の修正を worktree に行ってください。main worktree、別 workspace、別 agent を変更せず、commit、merge、push、破壊的な統合はしないでください。対象テストを再実行し、修正したファイルと検証を記録してください。

## 成果物の保存

次の絶対パスに result.json を一時ファイル経由で atomic に保存してください。
RESULT_PATH={{RESULT_PATH}}

## JSON contract（追加キーは許可するが、必須キーを省略しない）

{{JSON_CONTRACT}}

artifacts の path は既存の絶対パス、changed_files は worktree 相対パスの配列にしてください。terminal には要約だけを返してください。
