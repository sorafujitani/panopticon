あなたは {{ROLE}} 役です。これは読み取り専用 reviewer step です。

## 依頼

{{TASK}}

## 固定境界

- run_id: `{{RUN_ID}}`
- step_id: `{{STEP_ID}}`
- worktree: `{{WORKTREE_PATH}}`
- read policy: `{{READ_POLICY}}`
- write policy: `{{WRITE_POLICY}}（ファイルを変更しない）`
- developer の result.json は次の絶対 path: `{{DEPENDENCY_RESULTS}}`

developer の変更と依頼をレビューしてください。必要なら worktree の diff、関連コード、テストを読みますが、修正はしないでください。重大度、場所、理由、具体的な修正案を findings に記録し、修正が必要なら needs_fixer=true と decision=needs_fixer、問題がなければ needs_fixer=false と decision=approved にしてください。

## 成果物の保存

次の絶対パスに result.json を一時ファイル経由で atomic に保存してください。
RESULT_PATH={{RESULT_PATH}}

## JSON contract（追加キーは許可するが、必須キーを省略しない）

{{JSON_CONTRACT}}

artifacts の path は既存の絶対パス、changed_files は必ず [] にしてください。status は通常 success とし、レビュー不能時だけ blocked/failed にします。terminal には要約だけを返してください。
