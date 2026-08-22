package panopticon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupResourcesIsOwnedAndKeepsState(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "dedicated-worktree")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(root, "herdr")
	script := `#!/bin/sh
if [ "$1" = "worktree" ] && [ "$2" = "list" ]; then
  printf '{"worktrees":[{"workspace_id":"w-owned","worktree_path":"%s","branch":"branch"}]}' "$FAKE_WORKTREE"
else
  printf '{"status":"ok"}'
fi
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	environment := currentEnvironment()
	environment["HERDR_ENV"] = "1"
	environment["FAKE_WORKTREE"] = worktree
	client := NewHerdrClient(fake, environment)
	store, err := NewRunStore(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"run_id": "run-cleanup", "status": "completed", "repo": repo,
		"worktree": map[string]any{"path": worktree, "branch": "branch", "enabled": true},
		"resources": map[string]any{
			"workspace_id": "w-owned", "workspace_owned": true, "owned_tabs": []any{"w-tab"}, "owned_panes": []any{"w-pane"},
			"worktree_created": true, "worktree_owned": true,
			"cleanup": map[string]any{"status": "pending", "attempts": 0, "errors": []any{}},
		},
		"events": []any{},
	}
	if _, err := store.Create("run-cleanup", state); err != nil {
		t.Fatal(err)
	}
	engine := NewFlowEngine(store, client)
	cleaned, err := engine.CleanupResources("run-cleanup", true)
	if err != nil {
		t.Fatal(err)
	}
	if !directoryGone(worktree) {
		t.Fatal("owned worktree was not removed")
	}
	cleanup := mapValue(stateMap(cleaned, "resources")["cleanup"])
	if stringValue(cleanup["status"]) != "completed" {
		t.Fatalf("cleanup was not completed: %#v", cleanup)
	}
	if _, err := store.Load("run-cleanup"); err != nil {
		t.Fatalf("terminal state must remain observable: %v", err)
	}
	if !strings.Contains(jsonText(cleaned), "cleanup_finished") {
		t.Fatal("cleanup event was not persisted")
	}
}

func directoryGone(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

func TestResumeRetriesPartialCleanupAndKeepsOwnership(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	workflow := writeSingleStepWorkflow(t, filepath.Join(root, "workflow"), "developer", "none", "", nil, 30)
	fake := filepath.Join(root, "herdr")
	script := `#!/bin/sh
if [ "$1" = "tab" ] && [ "$2" = "close" ] && [ "$FAKE_CLOSE_FAIL" = "1" ]; then
  printf '{"error":{"code":"busy"}}' >&2
  exit 1
fi
printf '{"status":"ok"}'
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	environment := currentEnvironment()
	environment["HERDR_ENV"] = "1"
	environment["FAKE_CLOSE_FAIL"] = "1"
	client := NewHerdrClient(fake, environment)
	store, err := NewRunStore(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-partial-resume"
	state := map[string]any{
		"run_id": runID, "status": "failed", "repo": repo, "task": "fake task",
		"workflow": workflow.AsMap(), "verify_commands": []any{[]any{"true"}},
		"worktree":     map[string]any{"path": repo, "enabled": false},
		"orchestrator": map[string]any{},
		"steps": map[string]any{
			"step": map[string]any{"status": "failed", "agent": map[string]any{"target": "stale"}},
		},
		"resources": map[string]any{
			"workspace_id": "w-owned", "workspace_owned": true,
			"owned_tabs": []any{"w-stale-tab"}, "owned_panes": []any{"w-stale-pane"},
			"tabs": map[string]any{"step": "w-stale-tab"}, "panes": []any{"w-stale-pane"},
			"cleanup": map[string]any{"status": "partial", "attempts": 1, "errors": []any{}},
		},
		"events": []any{},
	}
	if _, err := store.Create(runID, state); err != nil {
		t.Fatal(err)
	}
	engine := NewFlowEngine(store, client)
	_, err = engine.ResumeRun(runID, nil)
	if err == nil || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("expected cleanup resume error, got %v", err)
	}
	loaded, err := store.Load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(loaded["status"]) != "failed" {
		t.Fatalf("status=%v", loaded["status"])
	}
	resources := stateMap(loaded, "resources")
	if strings.Join(stringSlice(resources["owned_tabs"]), ",") != "w-stale-tab" {
		t.Fatalf("owned_tabs dropped: %#v", resources["owned_tabs"])
	}
	if strings.Join(stringSlice(resources["owned_panes"]), ",") != "w-stale-pane" {
		t.Fatalf("owned_panes dropped: %#v", resources["owned_panes"])
	}
	if stringValue(resources["workspace_id"]) != "w-owned" || !boolValue(resources["workspace_owned"], false) {
		t.Fatalf("workspace ownership dropped: %#v", resources)
	}
	if stringValue(mapValue(resources["cleanup"])["status"]) != "partial" {
		t.Fatalf("cleanup=%#v", resources["cleanup"])
	}

	client.Environ["FAKE_CLOSE_FAIL"] = "0"
	cleaned, err := engine.CleanupResources(runID, true)
	if err != nil {
		t.Fatalf("expected cleanup retry to succeed, got %v", err)
	}
	cleanup := mapValue(stateMap(cleaned, "resources")["cleanup"])
	if stringValue(cleanup["status"]) != "completed" {
		t.Fatalf("cleanup retry was not completed: %#v", cleanup)
	}
	if !boolValue(cleanup["resources_closed"], false) {
		t.Fatalf("resources_closed=%v", cleanup["resources_closed"])
	}
	errors, ok := cleanup["errors"].([]any)
	if !ok || len(errors) != 0 {
		t.Fatalf("cleanup retry retained errors: %#v", cleanup["errors"])
	}
}
