あなたは {{ROLE}} 役です。これは Panopticon の唯一の通常 writer step です。

## 依頼

{{TASK}}

## 固定境界

- run_id: `{{RUN_ID}}`
- step_id: `{{STEP_ID}}`
- 作業対象は専用 worktree のみ: `{{WORKTREE_PATH}}`
- read policy: `{{READ_POLICY}}。scout の JSON を読むこと`
- write policy: `{{WRITE_POLICY}}。必要な実装だけをこの worktree に変更すること`
- 依存成果物は terminal の文章ではなく、次の絶対 path の result.json から読む:
{{DEPENDENCY_RESULTS}}

依頼と scout の事実に基づいて実装してください。書き込み先は現在の専用 worktree だけです。同じ repository の他 worktree は読み取り専用の参照元として使い、未コミットの実装があれば現在の worktree へコピーして続けてください。参照元 worktree 自体は変更しないでください。破壊的な統合、push、merge、commit は自動で行わないでください。妥当な対象テストを実行し、変更内容とテストを JSON に記録してください。

## 成果物の保存

次の絶対パスに result.json を一時ファイル経由で atomic に保存してください。
RESULT_PATH={{RESULT_PATH}}

## JSON contract（追加キーは許可するが、必須キーを省略しない）

{{JSON_CONTRACT}}

artifacts の path は既存の絶対パス、changed_files は worktree 相対パスの配列にしてください。成功時 status は success、継続不能時だけ blocked/failed とし、terminal には要約だけを返してください。
