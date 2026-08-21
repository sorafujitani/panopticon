package panopticon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunStoreAtomicStateAndLatestOrdering(t *testing.T) {
	store, err := NewRunStore(filepath.Join(t.TempDir(), "runs"))
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"run_id": "run-test", "status": "created", "created_at": "2025-01-01T00:00:00Z"}
	if _, err := store.Create("run-test", state); err != nil {
		t.Fatal(err)
	}
	state["status"] = "completed"
	if err := store.Save("run-test", state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("run-test")
	if err != nil {
		t.Fatal(err)
	}
	if loaded["status"] != "completed" {
		t.Fatalf("state was not saved: %#v", loaded)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0]["run_id"] != "run-test" {
		t.Fatalf("unexpected list: %#v", entries)
	}
	statePath, err := store.StatePath("run-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatal(err)
	}
}

func TestCompactStateIsBoundedAndOmitsLargeState(t *testing.T) {
	steps := map[string]any{}
	for index := 0; index < 40; index++ {
		steps["step-"+string(rune('a'+index%26))+"-"+string(make([]byte, 180))] = map[string]any{
			"status": "failed",
			"result": map[string]any{"summary": string(make([]byte, 2500))},
			"error":  map[string]any{"type": "CommandError", "message": string(make([]byte, 2500)), "stderr": string(make([]byte, 2500))},
		}
	}
	state := map[string]any{
		"run_id": "run-large", "status": "failed", "current_step": "developer",
		"repo": string(make([]byte, 2500)), "worktree": map[string]any{"path": string(make([]byte, 2500))},
		"steps": steps, "error": map[string]any{"type": "CommandError", "message": string(make([]byte, 4000))},
	}
	payload := CompactState(state)
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded)+1 > compactMaxOutputBytes {
		t.Fatalf("compact state exceeds limit: %d", len(encoded)+1)
	}
	if len(payload["steps"].(map[string]any)) > compactMaxSteps {
		t.Fatalf("too many compact steps: %d", len(payload["steps"].(map[string]any)))
	}
}

func TestCompactStateBoundsOversizedErrorIdentifiers(t *testing.T) {
	identifiers := map[string]any{
		"type":       strings.Repeat("T", 20_000),
		"code":       strings.Repeat("C", 20_000),
		"returncode": strings.Repeat("R", 20_000),
	}
	steps := map[string]any{}
	for index := 0; index < 32; index++ {
		steps[fmt.Sprintf("step-%d", index)] = map[string]any{"status": "failed", "error": identifiers}
	}
	payload := CompactState(map[string]any{"run_id": "run-large-identifiers", "status": "failed", "steps": steps, "error": identifiers})
	compactedSteps := mapValue(payload["steps"])
	if len(compactedSteps) == 0 || len(compactedSteps) >= 32 {
		t.Fatalf("compacted step count=%d", len(compactedSteps))
	}
	errors := []map[string]any{mapValue(payload["error"])}
	for _, raw := range compactedSteps {
		errors = append(errors, mapValue(mapValue(raw)["error"]))
	}
	for _, compacted := range errors {
		for key := range identifiers {
			value := stringValue(compacted[key])
			if value == "" || len([]rune(value)) > compactMaxErrorIdentifier {
				t.Fatalf("%s length=%d value=%q", key, len([]rune(value)), value)
			}
		}
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded)+1 > compactMaxOutputBytes {
		t.Fatalf("compact state exceeds limit: %d", len(encoded)+1)
	}
}

func TestDefaultStoreUsesPanopticonStateNamespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.Unsetenv("PANOPTICON_STATE_DIR")
	store, err := NewRunStore("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "state", "panopticon", "runs")
	if store.Root != want {
		t.Fatalf("root=%s want=%s", store.Root, want)
	}
}

func TestAtomicWriteAndRunStoreAreDurableShapes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	if err := atomicWriteJSON(path, map[string]any{"status": "created", "value": 1}); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteJSON(path, map[string]any{"status": "running", "value": 2}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if ParseInt64(payload["value"]) != 2 {
		t.Fatalf("value=%v", payload["value"])
	}
	matches, _ := filepath.Glob(filepath.Join(root, ".state.json.*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("tmp leftovers=%v", matches)
	}
	store, err := NewRunStore(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("run-test", map[string]any{"run_id": "run-test", "status": "created"}); err != nil {
		t.Fatal(err)
	}
	state, err := store.Load("run-test")
	if err != nil {
		t.Fatal(err)
	}
	state["status"] = "completed"
	if err := store.Save("run-test", state); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(entries[0]["status"]) != "completed" {
		t.Fatalf("list=%v", entries)
	}
	lock, err := store.Lock("run-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.With(func() error {
		if _, err := os.Stat(filepath.Join(mustRunDir(t, store, "run-test"), "run.lock")); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mustRunDir(t, store, "run-test"), "run.lock")); !os.IsNotExist(err) {
		t.Fatal("lock should be released")
	}
}

func TestCompactStateHandlesUnserializableErrors(t *testing.T) {
	stringPayload := CompactState(map[string]any{
		"steps": map[string]any{"step": map[string]any{"error": "plain step error"}},
		"error": "plain run error",
	})
	if stringPayload["error"] != "plain run error" || mapValue(stringPayload["steps"].(map[string]any)["step"])["error"] != "plain step error" {
		t.Fatalf("string payload=%v", stringPayload)
	}
	if _, err := json.Marshal(stringPayload); err != nil {
		t.Fatal(err)
	}
}

func TestCompactStateCollapsesUnknownErrorDictToMessage(t *testing.T) {
	payload := CompactState(map[string]any{
		"steps": map[string]any{"step": map[string]any{"error": map[string]any{"details": "unknown"}}},
		"error": map[string]any{"details": "unknown"},
	})
	if stringValue(mapValue(payload["error"])["message"]) != `{"details":"unknown"}` {
		t.Fatalf("error=%v", payload["error"])
	}
	if stringValue(mapValue(mapValue(payload["steps"].(map[string]any)["step"])["error"])["message"]) != `{"details":"unknown"}` {
		t.Fatalf("step=%v", payload["steps"])
	}
}
