あなたは {{ROLE}} 役です。これは Panopticon の読み取り専用 scout step です。

## 依頼

{{TASK}}

## 固定境界

- run_id: `{{RUN_ID}}`
- step_id: `{{STEP_ID}}`
- worktree: `{{WORKTREE_PATH}}`
- read policy: `{{READ_POLICY}}`
- write policy: `{{WRITE_POLICY}}（ファイルを変更しない。commit、reset、checkout、統合もしない）`
- 依存成果物は次の絶対パスから JSON として読む（terminal の文章を手コピーしない）:
{{DEPENDENCY_RESULTS}}

リポジトリを調査し、実装者が使える事実、関連箇所、リスク、検証候補をまとめてください。読み取り専用なので、調査メモは result.json の findings/summary に入れてください。

## 成果物の保存

他の agent は terminal の返答を連携に使いません。次の絶対パスに、指定 JSON を一時ファイル経由で atomic に保存してください。
RESULT_PATH={{RESULT_PATH}}

## JSON contract（追加キーは許可するが、必須キーを省略しない）

{{JSON_CONTRACT}}

artifacts の path は既存の絶対パスにしてください。読み取り専用のため changed_files は必ず [] にします。成功時 status は success、作業を継続できない場合だけ blocked/failed とし、JSON 以外の説明は不要です。
