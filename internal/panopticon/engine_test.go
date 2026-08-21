package panopticon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureAgentRejectsMissingReuseAgentSourceWithoutRecursion(t *testing.T) {
	workflow := Workflow{Steps: []StepSpec{
		{ID: "first", Role: "developer", Kind: "codex", ReuseAgent: "second"},
		{ID: "second", Role: "developer", Kind: "codex", ReuseAgent: "first"},
	}}
	engine := NewFlowEngine(nil, nil)
	state := engine.newState("run-cycle", StartOptions{Workflow: workflow, Repo: t.TempDir()}, t.TempDir())
	_, err := engine.ensureAgent(state, workflow.Steps[0])
	var flowErr *FlowError
	if !errors.As(err, &flowErr) || !strings.Contains(err.Error(), "reuse_agent target has no agent: second") {
		t.Fatalf("expected FlowError for missing reuse_agent source, got %v", err)
	}
}

func TestAgentNameNormalizesRunIDSuffixAndTruncates(t *testing.T) {
	cases := []struct {
		runID  string
		stepID string
		want   string
	}{{
		runID:  "RUN-20260822T004500Z-AB!D_EF2",
		stepID: "step",
		want:   "pan-ab-d_ef2-step",
	}, {
		runID:  "run-20260822T004500Z-abcd1234",
		stepID: "averylongstepidentifier",
		want:   "pan-abcd1234-averylongstepident",
	}}
	for _, item := range cases {
		engine := NewFlowEngine(nil, nil)
		got := engine.agentName(map[string]any{"run_id": item.runID}, StepSpec{ID: item.stepID})
		if got != item.want || len([]rune(got)) > 31 {
			t.Fatalf("agentName(%q, %q)=%q want %q", item.runID, item.stepID, got, item.want)
		}
	}
}

func TestDryRunIsSideEffectFreeAndKeepsArgvShape(t *testing.T) {
	workflow, err := LoadWorkflow(filepath.Join(repositoryRoot(t), "workflows", "standard.toml"))
	if err != nil {
		t.Fatal(err)
	}
	payload := DryRunPayload(".", workflow, "task; never shell", [][]string{{"go", "test", "./..."}}, true)
	if !boolValue(payload["dry_run"], false) {
		t.Fatal("dry-run marker is missing")
	}
	commands := payload["verify_commands"].([]any)
	if len(commands) != 1 || commands[0].([]string)[2] != "./..." {
		t.Fatalf("verification command was not retained as argv: %#v", commands)
	}
}

func TestShellQuoteProtectsPaneRunArguments(t *testing.T) {
	value := "path with spaces; echo injected"
	quoted := shellQuote(value)
	if !strings.HasPrefix(quoted, "'") || !strings.HasSuffix(quoted, "'") || !strings.Contains(quoted, ";") {
		t.Fatalf("unexpected shell quote: %q", quoted)
	}
	if commandText([]string{"panopticon", "start", value}) != "'panopticon' 'start' 'path with spaces; echo injected'" {
		t.Fatalf("unexpected command text: %q", commandText([]string{"panopticon", "start", value}))
	}
}
