package panopticon

import (
	"path/filepath"
	"strings"
	"testing"
)

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
