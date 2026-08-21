package panopticon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTOMLAcceptsMultilineArraysQuotedKeysAndInlineTables(t *testing.T) {
	content := `
version = 1
name = "custom"
"display.name" = "quoted dotted"
default_verify = [
  ["python3", "-c", "pass"],
]

[[steps]]
id = "step"
role = "developer"
kind = "pi"
depends_on = [
]
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"
contract = { required_fields = ["schema_version"], artifact_kinds = ["report"] }
`
	root, err := ParseTOML(content)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(root["display.name"]) != "quoted dotted" {
		t.Fatalf("quoted dotted key: %#v", root["display.name"])
	}
	verify := root["default_verify"].([]any)
	if len(verify) != 1 || strings.Join(stringList(verify[0]), " ") != "python3 -c pass" {
		t.Fatalf("default_verify=%#v", verify)
	}
	steps := root["steps"].([]any)
	contract := mapValue(mapValue(steps[0])["contract"])
	if strings.Join(stringList(contract["required_fields"]), ",") != "schema_version" {
		t.Fatalf("inline contract=%#v", contract)
	}
}

func TestLoadWorkflowAcceptsMultilinePythonCompatibleTOML(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.md"), []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workflow.toml")
	content := `version = 1
name = "custom"
default_verify = [
  ["python3", "-c", "pass"],
]

[[steps]]
id = "step"
role = "developer"
kind = "pi"
depends_on = [
]
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"

[steps.contract]
required_fields = [
  "schema_version",
]
artifact_kinds = [
  "report",
]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow, err := LoadWorkflow(path)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Name != "custom" || len(workflow.DefaultVerify) != 1 || strings.Join(workflow.DefaultVerify[0], " ") != "python3 -c pass" {
		t.Fatalf("workflow=%#v", workflow)
	}
	if strings.Join(workflow.Steps[0].Contract.RequiredFields, ",") != "schema_version" {
		t.Fatalf("required_fields=%v", workflow.Steps[0].Contract.RequiredFields)
	}
}

func TestLoadRepoConfigAcceptsInlineTableAndQuotedKeys(t *testing.T) {
	repo := t.TempDir()
	content := `
workflow = "standard"
verification = { commands = [["true"]] }
"display.name" = "repo"
`
	if err := os.WriteFile(filepath.Join(repo, ".panopticon.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadRepoConfig(repo)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(config["workflow"]) != "standard" || stringValue(config["display.name"]) != "repo" {
		t.Fatalf("config=%#v", config)
	}
	commands, err := commandsFromConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || strings.Join(commands[0], ",") != "true" {
		t.Fatalf("commands=%v", commands)
	}
}
