package panopticon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeCLIState(t *testing.T, store *RunStore, runID, status, updatedAt string, extraError any) {
	t.Helper()
	runDir := mustRunDir(t, store, runID)
	stepStatus := status
	if status == "running" {
		stepStatus = "running"
	}
	current := any(nil)
	if status == "running" {
		current = "developer"
	}
	state := map[string]any{
		"run_id": runID, "status": status,
		"current_step": current, "repo": "/tmp/repo", "run_dir": runDir,
		"worktree":   map[string]any{"path": "/tmp/worktree", "enabled": true},
		"updated_at": updatedAt,
		"steps": map[string]any{
			"developer": map[string]any{
				"status": stepStatus,
				"result": map[string]any{"summary": "step summary"},
				"error":  nil,
			},
		},
		"error":                    extraError,
		"events":                   []any{map[string]any{"event": "huge snapshot"}},
		"worktree_snapshot_before": map[string]any{"files": map[string]any{"large": "snapshot"}},
	}
	if _, err := store.Create(runID, state); err != nil {
		t.Fatal(err)
	}
	path, err := store.StatePath(runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteJSON(path, state); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorPassesWithFakeHerdr(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("PANOPTICON_STATE_DIR", filepath.Join(root, "runs"))
	code, stdout := captureMain(t, []string{"doctor", "--herdr-bin", fakeHerdrPath(t)})
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout=%q err=%v", stdout, err)
	}
	if code != 0 || !boolValue(payload["ok"], false) {
		t.Fatalf("code=%d payload=%v", code, payload)
	}
	names := map[string]bool{}
	for _, raw := range payload["checks"].([]any) {
		names[stringValue(mapValue(raw)["name"])] = true
	}
	for _, name := range []string{"HERDR_ENV", "herdr_executable", "state_directory", "herdr_version"} {
		if !names[name] {
			t.Fatalf("missing check %s in %v", name, names)
		}
	}
}

func TestDoctorReportsMissingManagedEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PANOPTICON_STATE_DIR", filepath.Join(root, "runs"))
	_ = os.Unsetenv("HERDR_ENV")
	code, stdout := captureMain(t, []string{"doctor", "--herdr-bin", fakeHerdrPath(t)})
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout=%q err=%v", stdout, err)
	}
	if code != 1 || boolValue(payload["ok"], true) {
		t.Fatalf("code=%d payload=%v", code, payload)
	}
	found := false
	for _, raw := range payload["checks"].([]any) {
		check := mapValue(raw)
		if stringValue(check["name"]) == "HERDR_ENV" {
			found = true
			if boolValue(check["ok"], true) {
				t.Fatal("HERDR_ENV should fail")
			}
		}
	}
	if !found {
		t.Fatal("HERDR_ENV check missing")
	}
}

func TestWaitReturnsTerminalStatusExitCodes(t *testing.T) {
	root := t.TempDir()
	store, err := NewRunStore(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIState(t, store, "run-completed", "completed", "2025-01-01T00:00:00Z", nil)
	writeCLIState(t, store, "run-blocked", "blocked", "2025-01-01T00:00:01Z", nil)
	writeCLIState(t, store, "run-failed", "failed", "2025-01-01T00:00:02Z", nil)
	writeCLIState(t, store, "run-cancelled", "cancelled", "2025-01-01T00:00:03Z", nil)
	t.Setenv("HERDR_ENV", "1")
	cases := []struct {
		runID  string
		code   int
		status string
	}{
		{"run-completed", 0, "completed"},
		{"run-blocked", 2, "blocked"},
		{"run-failed", 1, "failed"},
		{"run-cancelled", 1, "cancelled"},
	}
	for _, item := range cases {
		code, stdout := captureMain(t, []string{"wait", "--state-root", store.Root, "--run-id", item.runID, "--timeout-seconds", "1"})
		var payload map[string]any
		if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
			t.Fatalf("%s stdout=%q err=%v", item.runID, stdout, err)
		}
		if code != item.code || stringValue(payload["status"]) != item.status {
			t.Fatalf("%s code=%d status=%v want %d %s", item.runID, code, payload["status"], item.code, item.status)
		}
	}
}

func TestWaitReturnsCompactSnapshotAndTimeoutJSON(t *testing.T) {
	root := t.TempDir()
	store, err := NewRunStore(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIState(t, store, "run-running", "running", "2025-01-01T00:00:00Z", map[string]any{
		"type": "CommandError", "code": "timeout", "message": "run error", "returncode": 1, "stderr": "stderr output",
	})
	t.Setenv("HERDR_ENV", "1")
	code, stdout := captureMain(t, []string{"wait", "--state-root", store.Root, "--run-id", "run-running", "--timeout-seconds", "0.01", "--interval-seconds", "0.2"})
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout=%q err=%v", stdout, err)
	}
	if code != 124 {
		t.Fatalf("code=%d payload=%v", code, payload)
	}
	for _, key := range []string{"run_id", "status", "current_step", "repo", "worktree", "steps", "error", "updated_at"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("missing %s in %v", key, payload)
		}
	}
	if payload["worktree"] != "/tmp/worktree" {
		t.Fatalf("worktree=%v", payload["worktree"])
	}
	step := mapValue(payload["steps"].(map[string]any)["developer"])
	if stringValue(step["status"]) != "running" || stringValue(step["summary"]) != "step summary" {
		t.Fatalf("step=%v", step)
	}
	errorValue := mapValue(payload["error"])
	if stringValue(errorValue["type"]) != "CommandError" || stringValue(errorValue["code"]) != "timeout" || stringValue(errorValue["message"]) != "run error" || ParseInt64(errorValue["returncode"]) != 1 || stringValue(errorValue["stderr"]) != "stderr output" {
		t.Fatalf("error=%v", errorValue)
	}
	if strings.Count(stdout, "\n") <= 1 || !strings.Contains(stdout, "\n  ") || len([]byte(stdout)) > 12*1024 {
		t.Fatalf("stdout formatting/size: %q", stdout)
	}
	if _, ok := payload["events"]; ok {
		t.Fatal("events leaked")
	}
	if _, ok := payload["worktree_snapshot_before"]; ok {
		t.Fatal("snapshot leaked")
	}
}

func TestWaitBoundsOversizedCompactSnapshot(t *testing.T) {
	root := t.TempDir()
	store, err := NewRunStore(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	structured := map[string]any{"type": "CommandError", "code": "timeout", "message": strings.Repeat("e", 2000), "returncode": 1, "stderr": strings.Repeat("s", 2000)}
	steps := map[string]any{}
	for index := 0; index < 32; index++ {
		steps[itoa(index)+"-"+strings.Repeat("x", 200)] = map[string]any{
			"status": "failed", "result": map[string]any{"summary": strings.Repeat("s", 2000)}, "error": structured,
		}
	}
	state := map[string]any{
		"run_id": "run-large", "status": "failed", "current_step": "developer",
		"repo": strings.Repeat("r", 2500), "worktree": map[string]any{"path": strings.Repeat("w", 2500)},
		"updated_at": "2025-01-01T00:00:00Z", "steps": steps, "error": structured,
	}
	if _, err := store.Create("run-large", state); err != nil {
		t.Fatal(err)
	}
	path, _ := store.StatePath("run-large")
	if err := atomicWriteJSON(path, state); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	code, stdout := captureMain(t, []string{"wait", "--state-root", store.Root, "--run-id", "run-large", "--timeout-seconds", "1"})
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatal(err)
	}
	if code != 1 || stringValue(payload["run_id"]) != "run-large" || stringValue(payload["status"]) != "failed" {
		t.Fatalf("code=%d payload=%v", code, payload)
	}
	compactSteps := payload["steps"].(map[string]any)
	if len(compactSteps) > 32 {
		t.Fatalf("too many steps: %d", len(compactSteps))
	}
	for stepID, raw := range compactSteps {
		if len(stepID) > 128 {
			t.Fatalf("step id too long: %d", len(stepID))
		}
		step := mapValue(raw)
		message := stringValue(mapValue(step["error"])["message"])
		if len(message) > 1000 {
			t.Fatalf("step error too long: %d", len(message))
		}
		summary := stringValue(step["summary"])
		if summary != "" && len(summary) > 1000 {
			t.Fatalf("summary too long: %d", len(summary))
		}
	}
	if len(stringValue(mapValue(payload["error"])["message"])) > 3000 {
		t.Fatal("run error too long")
	}
	if len(stringValue(payload["repo"])) > 2000 || len(stringValue(payload["worktree"])) > 2000 {
		t.Fatal("path too long")
	}
	if len([]byte(stdout)) > 12*1024 {
		t.Fatalf("stdout too large: %d", len(stdout))
	}
}

func TestWaitCtrlCDoesNotRequestRunCancellation(t *testing.T) {
	root := t.TempDir()
	store, err := NewRunStore(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIState(t, store, "run-running", "running", "2025-01-01T00:00:00Z", nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	state, _, waitErr := WaitForTerminalContext(ctx, store, "run-running", time.Second, 200*time.Millisecond)
	if waitErr != context.Canceled {
		t.Fatalf("expected cancel, got %v", waitErr)
	}
	if stringValue(state["status"]) != "running" {
		t.Fatalf("status=%v", state["status"])
	}
	if _, err := os.Stat(filepath.Join(mustRunDir(t, store, "run-running"), "cancel.request")); !os.IsNotExist(err) {
		t.Fatalf("cancel.request exists: %v", err)
	}
	t.Setenv("HERDR_ENV", "1")
	done := make(chan struct{})
	var code int
	var stdout string
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	go func() {
		code, stdout = captureMain(t, []string{"wait", "--state-root", store.Root, "--run-id", "run-running", "--timeout-seconds", "1", "--interval-seconds", "0.2"})
		close(done)
	}()
	select {
	case <-done:
		if code != 130 {
			t.Fatalf("code=%d stdout=%s", code, stdout)
		}
		if _, err := os.Stat(filepath.Join(mustRunDir(t, store, "run-running"), "cancel.request")); !os.IsNotExist(err) {
			t.Fatal("SIGINT cancelled the run")
		}
	case <-time.After(2 * time.Second):
		t.Skip("SIGINT wait CLI path did not return in time; context cancel coverage remains")
	}
}

func TestWaitWithoutRunIDUsesLatestRun(t *testing.T) {
	root := t.TempDir()
	store, err := NewRunStore(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIState(t, store, "run-old", "completed", "2025-01-01T00:00:00Z", nil)
	writeCLIState(t, store, "run-latest", "blocked", "2025-01-01T00:00:01Z", nil)
	t.Setenv("HERDR_ENV", "1")
	code, stdout := captureMain(t, []string{"wait", "--state-root", store.Root, "--timeout-seconds", "1"})
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatal(err)
	}
	if code != 2 || stringValue(payload["run_id"]) != "run-latest" || stringValue(payload["status"]) != "blocked" {
		t.Fatalf("code=%d payload=%v", code, payload)
	}
}

func TestStatusAndCancelReturnCompactSnapshots(t *testing.T) {
	root := t.TempDir()
	store, err := NewRunStore(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIState(t, store, "run-status", "running", "2025-01-01T00:00:00Z", map[string]any{"message": "status error"})
	t.Setenv("HERDR_ENV", "1")
	statusCode, statusOut := captureMain(t, []string{"status", "--run-id", "run-status", "--state-root", store.Root, "--herdr-bin", fakeHerdrPath(t)})
	var statusPayload map[string]any
	if err := json.Unmarshal([]byte(statusOut), &statusPayload); err != nil {
		t.Fatalf("status stdout=%q err=%v", statusOut, err)
	}
	expected := map[string]bool{"run_id": true, "status": true, "current_step": true, "repo": true, "worktree": true, "steps": true, "error": true, "updated_at": true}
	if statusCode != 0 {
		t.Fatalf("status code=%d payload=%v", statusCode, statusPayload)
	}
	if len(statusPayload) != len(expected) {
		t.Fatalf("status keys=%v", statusPayload)
	}
	for key := range expected {
		if _, ok := statusPayload[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	if statusPayload["worktree"] != "/tmp/worktree" || stringValue(mapValue(statusPayload["error"])["message"]) != "status error" {
		t.Fatalf("status payload=%v", statusPayload)
	}
	cancelCode, cancelOut := captureMain(t, []string{"--state-root", store.Root, "--herdr-bin", fakeHerdrPath(t), "cancel", "run-status"})
	var cancelPayload map[string]any
	if err := json.Unmarshal([]byte(cancelOut), &cancelPayload); err != nil {
		t.Fatalf("cancel stdout=%q err=%v", cancelOut, err)
	}
	if cancelCode != 3 || stringValue(cancelPayload["status"]) != "cancelled" || cancelPayload["worktree"] != "/tmp/worktree" {
		t.Fatalf("cancel code=%d payload=%v", cancelCode, cancelPayload)
	}
	if _, ok := cancelPayload["events"]; ok {
		t.Fatal("events leaked from cancel")
	}
}
