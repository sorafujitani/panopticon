package panopticon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join(workingDirectory, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestStandardWorkflowIsValidated(t *testing.T) {
	workflow, err := LoadWorkflow(filepath.Join(repositoryRoot(t), "workflows", "standard.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Name != "standard" || len(workflow.Steps) != 5 || workflow.Controller == nil {
		t.Fatalf("unexpected workflow: %#v", workflow)
	}
	if workflow.Controller.Role != "controller" || workflow.Controller.MaxRetries != 1 || workflow.Controller.StepSpec().WritePolicy != "none" {
		t.Fatalf("unexpected controller: %#v", workflow.Controller)
	}
	if workflow.Steps[1].ID != "developer" || workflow.Steps[1].DependsOn[0] != "scout" {
		t.Fatalf("dependency graph was not parsed: %#v", workflow.Steps)
	}
	if workflow.Steps[3].Condition != "reviewer.needs_fixer" {
		t.Fatalf("condition was not parsed: %q", workflow.Steps[3].Condition)
	}
	if got := workflow.Steps[0].Contract.AsMap()["required_boolean_fields"]; got == nil {
		t.Fatal("empty contract arrays must serialize as [] rather than null")
	}
}

func TestWorkflowResolutionPrefersRepositoryFiles(t *testing.T) {
	directory := t.TempDir()
	repo := filepath.Join(directory, "repo")
	workflowDirectory := filepath.Join(repo, "workflows")
	if err := os.MkdirAll(workflowDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(workflowDirectory, "prompt.md")
	if err := os.WriteFile(prompt, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow := `version = 1
name = "local"
default_verify = [["true"]]

[[steps]]
id = "step"
role = "developer"
kind = "codex"
depends_on = []
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"

[steps.contract]
required_fields = ["schema_version"]
artifact_kinds = ["report"]
`
	path := filepath.Join(workflowDirectory, "local.toml")
	if err := os.WriteFile(path, []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveWorkflowPath(repo, "local", repositoryRoot(t), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != path {
		t.Fatalf("repository workflow was not preferred: %s", resolved)
	}
	loaded, err := LoadWorkflow(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "local" || loaded.Steps[0].Template != prompt {
		t.Fatalf("local workflow was not loaded: %#v", loaded)
	}
}

func TestStandardWorkflowGraphAndAgentSettings(t *testing.T) {
	workflow, err := LoadWorkflow(filepath.Join(repositoryRoot(t), "workflows", "standard.toml"))
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, step := range workflow.Steps {
		ids = append(ids, step.ID)
		if step.Kind != "pi" || step.SubmitKey != "ctrl+enter" || len(step.AgentArgs) != 1 || step.AgentArgs[0] != "--no-extensions" {
			t.Fatalf("unexpected step settings: %#v", step)
		}
	}
	if strings.Join(ids, ",") != "scout,developer,reviewer,fixer,verifier" {
		t.Fatalf("ids=%v", ids)
	}
	if workflow.StepMap()["developer"].DependsOn[0] != "scout" || workflow.StepMap()["fixer"].Condition != "reviewer.needs_fixer" {
		t.Fatal("graph fields")
	}
	if len(workflow.DefaultVerify) != 1 || strings.Join(workflow.DefaultVerify[0], " ") != "git diff --check" {
		t.Fatalf("verify=%v", workflow.DefaultVerify)
	}
	if workflow.Controller == nil || workflow.Controller.Kind != "pi" || workflow.Controller.SubmitKey != "ctrl+enter" || strings.Join(workflow.Controller.AgentArgs, ",") != "--no-extensions" {
		t.Fatalf("controller=%#v", workflow.Controller)
	}
}

func TestAgentArgsAreParsedSerializedAndChangeDigest(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.md"), []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := `version = 1
name = "custom"
default_verify = [["python3", "-c", "pass"]]

[[steps]]
id = "step"
role = "developer"
kind = "pi"
depends_on = []
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"
submit_key = "ctrl+enter"
model = "anthropic/claude-sonnet-4"
effort = "high"
agent_args = ["--profile", "profile with spaces"]

[steps.contract]
required_fields = ["schema_version"]
artifact_kinds = ["report"]
`
	path := filepath.Join(root, "custom.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow, err := LoadWorkflow(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(content, "profile with spaces", "different profile")), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := LoadWorkflow(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(workflow.Steps[0].AgentArgs, "\x00") != "--profile\x00profile with spaces" {
		t.Fatalf("args=%v", workflow.Steps[0].AgentArgs)
	}
	if workflow.Steps[0].Model != "anthropic/claude-sonnet-4" || workflow.Steps[0].Effort != "high" {
		t.Fatalf("model/effort=%q/%q", workflow.Steps[0].Model, workflow.Steps[0].Effort)
	}
	serializedStep := mapValue(workflow.AsMap()["steps"].([]any)[0])
	serialized := stringList(serializedStep["agent_args"])
	if strings.Join(serialized, "\x00") != "--profile\x00profile with spaces" {
		t.Fatalf("serialized=%v", serialized)
	}
	if stringValue(serializedStep["model"]) != "anthropic/claude-sonnet-4" || stringValue(serializedStep["effort"]) != "high" {
		t.Fatalf("serialized model/effort=%v/%v", serializedStep["model"], serializedStep["effort"])
	}
	if workflow.Digest == changed.Digest {
		t.Fatal("digest should change")
	}
}

func TestEmptyAgentArgIsRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.md"), []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "custom.toml"), []byte(`version = 1
name = "custom"
default_verify = [["python3", "-c", "pass"]]

[[steps]]
id = "step"
role = "developer"
kind = "pi"
depends_on = []
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"
agent_args = [" "]

[steps.contract]
required_fields = ["schema_version"]
artifact_kinds = ["report"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWorkflow(filepath.Join(root, "custom.toml"))
	if err == nil || !strings.Contains(err.Error(), "agent_args") {
		t.Fatalf("expected agent_args error, got %v", err)
	}
}

func TestUserWorkflowOverridesBundledWorkflow(t *testing.T) {
	configDirectory := t.TempDir()
	t.Setenv("PANOPTICON_CONFIG_DIR", configDirectory)
	workflowDirectory := filepath.Join(configDirectory, "workflows")
	if err := os.MkdirAll(workflowDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDirectory, "prompt.md"), []byte("user prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := `version = 1
name = "user-standard"
default_verify = [["true"]]

[[steps]]
id = "step"
role = "developer"
kind = "pi"
depends_on = []
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"

[steps.contract]
required_fields = ["schema_version"]
artifact_kinds = ["report"]
`
	path := filepath.Join(workflowDirectory, "standard.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveWorkflowPath(t.TempDir(), "standard", repositoryRoot(t), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != path {
		t.Fatalf("user workflow was not preferred: got %s want %s", resolved, path)
	}
	workflow, err := LoadWorkflow(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Name != "user-standard" || workflow.Steps[0].Template != filepath.Join(workflowDirectory, "prompt.md") {
		t.Fatalf("workflow=%#v", workflow)
	}
}

func TestRepoConfigAliasesOverrideUserConfigAliases(t *testing.T) {
	workflowConfigs := func(name string) []map[string]any {
		return []map[string]any{
			{"workflow": name},
			{"workflow": map[string]any{"name": name}},
			{"flow": map[string]any{"name": name}},
		}
	}
	for userIndex, userConfig := range workflowConfigs("personal") {
		for repoIndex, repoConfig := range workflowConfigs("repository") {
			got := configuredWorkflowFromSources("", repoConfig, userConfig)
			if got != "repository" {
				t.Fatalf("workflow aliases repo=%d user=%d: got %q", repoIndex, userIndex, got)
			}
		}
	}

	verificationConfigs := func(value string) []map[string]any {
		command := []any{[]any{"echo", value}}
		return []map[string]any{
			{"verification": map[string]any{"commands": command}},
			{"verification": map[string]any{"command": command}},
			{"verify": map[string]any{"commands": command}},
			{"verify": map[string]any{"command": command}},
			{"verification_commands": command},
			{"verify_commands": command},
		}
	}
	workflow := Workflow{DefaultVerify: [][]string{{"echo", "default"}}}
	for userIndex, userConfig := range verificationConfigs("user") {
		for repoIndex, repoConfig := range verificationConfigs("repo") {
			commands, err := resolveVerifyCommandsFromSources(nil, workflow, repoConfig, userConfig)
			if err != nil {
				t.Fatal(err)
			}
			if len(commands) != 1 || strings.Join(commands[0], " ") != "echo repo" {
				t.Fatalf("verification aliases repo=%d user=%d: commands=%v", repoIndex, userIndex, commands)
			}
		}
	}
}

func TestWorkflowResolutionDoesNotRequireHomeWithoutUserConfig(t *testing.T) {
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "PANOPTICON_CONFIG_DIR"} {
		t.Setenv(key, "restore-with-test-cleanup")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	config, err := LoadUserConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(config) != 0 {
		t.Fatalf("user config=%v", config)
	}
	root := repositoryRoot(t)
	for _, requested := range []string{"standard", filepath.Join(root, "workflows", "standard.toml")} {
		repo, workflow, _, err := loadPlanInputs(root, requested, nil)
		if err != nil {
			t.Fatalf("workflow %q: %v", requested, err)
		}
		if repo != root || workflow.Name != "standard" {
			t.Fatalf("workflow %q resolved to repo=%q workflow=%#v", requested, repo, workflow)
		}
	}
}

func TestInvalidStepModelAndEffortSettingsAreRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.md"), []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := `version = 1
name = "invalid"
default_verify = [["true"]]

[[steps]]
id = "step"
role = "developer"
kind = "pi"
depends_on = []
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
%s
template = "prompt.md"

[steps.contract]
required_fields = ["schema_version"]
artifact_kinds = ["report"]
`
	cases := []struct {
		name, settings, errorText string
	}{
		{"invalid effort", `effort = "extreme"`, "invalid effort"},
		{"non-pi model", "kind = \"codex\"\nmodel = \"gpt-5\"", "only supported for kind pi"},
		{"model conflict", "model = \"openai/gpt-5\"\nagent_args = [\"--model=other\"]", "conflicts with --model"},
		{"effort conflict", "effort = \"high\"\nagent_args = [\"--thinking\", \"low\"]", "conflicts with --thinking"},
		{"reuse override", "model = \"openai/gpt-5\"\nreuse_agent = \"other\"", "cannot be set with reuse_agent"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			content := fmt.Sprintf(base, item.settings)
			if item.name == "non-pi model" {
				content = strings.Replace(content, "kind = \"pi\"\n", "", 1)
			}
			path := filepath.Join(root, strings.ReplaceAll(item.name, " ", "-")+".toml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadWorkflow(path)
			if err == nil || !strings.Contains(err.Error(), item.errorText) {
				t.Fatalf("expected %q, got %v", item.errorText, err)
			}
		})
	}
}

func TestTomlFilenameResolvesFromWorkflowsDirectory(t *testing.T) {
	root := repositoryRoot(t)
	path, err := ResolveWorkflowPath(root, "standard.toml", root, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(filepath.Join(root, "workflows", "standard.toml"))
	if path != want {
		t.Fatalf("got %s want %s", path, want)
	}
}

func TestInvalidWorkflowCycleIsRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.md"), []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cycle.toml"), []byte(`version = 1
name = "cycle"
default_verify = [["python3", "-c", "pass"]]

[[steps]]
id = "first"
role = "scout"
kind = "codex"
depends_on = ["second"]
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"

[steps.contract]
required_fields = ["schema_version"]
artifact_kinds = ["report"]

[[steps]]
id = "second"
role = "scout"
kind = "codex"
depends_on = ["first"]
read_policy = "none"
write_policy = "none"
timeout_seconds = 1
template = "prompt.md"

[steps.contract]
required_fields = ["schema_version"]
artifact_kinds = ["report"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWorkflow(filepath.Join(root, "cycle.toml"))
	if err == nil || !strings.Contains(err.Error(), "workflow contains a cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestVerifyCLIValueRemainsAnArgv(t *testing.T) {
	workflow, err := LoadWorkflow(filepath.Join(repositoryRoot(t), "workflows", "standard.toml"))
	if err != nil {
		t.Fatal(err)
	}
	commands, err := ResolveVerifyCommands([]string{"python3 -m unittest discover -s tests -v"}, map[string]any{}, workflow)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"python3", "-m", "unittest", "discover", "-s", "tests", "-v"}
	if len(commands) != 1 || strings.Join(commands[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("commands=%v", commands)
	}
}

func TestLoadsPanopticonRepositoryConfig(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".panopticon.toml"), []byte("workflow = \"standard\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadRepoConfig(repo)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(config["workflow"]) != "standard" {
		t.Fatalf("config=%v", config)
	}
}

func TestDryRunHasNoStateSideEffect(t *testing.T) {
	workflow, err := LoadWorkflow(filepath.Join(repositoryRoot(t), "workflows", "standard.toml"))
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := DryRunPayload(repo, workflow, "Investigate", workflow.DefaultVerify, true)
	if !boolValue(payload["dry_run"], false) {
		t.Fatal("dry_run")
	}
	if _, err := os.Stat(filepath.Join(repo, ".panopticon")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote state")
	}
	if boolValue(mapValue(payload["worktree"])["destructive_integration"], true) {
		t.Fatal("destructive_integration")
	}
	for _, raw := range payload["execution"].([]any) {
		step := mapValue(raw)
		if strings.Join(stringList(step["agent_args"]), ",") != "--no-extensions" {
			t.Fatalf("agent_args=%v", step["agent_args"])
		}
		if step["model"] != nil || step["effort"] != nil {
			t.Fatalf("unexpected default model/effort=%v/%v", step["model"], step["effort"])
		}
	}
}

func TestShellSplitKeepsShellSyntaxInOneArgument(t *testing.T) {
	command, err := shellSplit(`go test ./... -run 'Test name'`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"go", "test", "./...", "-run", "Test name"}
	if len(command) != len(want) {
		t.Fatalf("got %#v, want %#v", command, want)
	}
	for index := range want {
		if command[index] != want[index] {
			t.Fatalf("got %#v, want %#v", command, want)
		}
	}
}

func TestEmbeddedAssetsMatchRepositoryFiles(t *testing.T) {
	root := repositoryRoot(t)
	for _, pair := range []struct{ embeddedDirectory, repositoryDirectory string }{
		{"workflows", filepath.Join(root, "workflows")},
		{"prompts", filepath.Join(root, "prompts")},
	} {
		embeddedEntries, err := embeddedAssets.ReadDir(filepath.ToSlash(filepath.Join("assets", pair.embeddedDirectory)))
		if err != nil {
			t.Fatal(err)
		}
		repositoryEntries, err := os.ReadDir(pair.repositoryDirectory)
		if err != nil {
			t.Fatal(err)
		}
		embeddedFiles := map[string]bool{}
		for _, entry := range embeddedEntries {
			if entry.IsDir() {
				t.Fatalf("embedded %s contains a directory: %s", pair.embeddedDirectory, entry.Name())
			}
			embeddedFiles[entry.Name()] = true
			embedded, err := embeddedAssets.ReadFile(filepath.ToSlash(filepath.Join("assets", pair.embeddedDirectory, entry.Name())))
			if err != nil {
				t.Fatal(err)
			}
			repository, err := os.ReadFile(filepath.Join(pair.repositoryDirectory, entry.Name()))
			if err != nil {
				t.Fatalf("embedded %s/%s has no repository copy: %v", pair.embeddedDirectory, entry.Name(), err)
			}
			if string(embedded) != string(repository) {
				t.Fatalf("embedded %s/%s differs from the repository copy", pair.embeddedDirectory, entry.Name())
			}
		}
		for _, entry := range repositoryEntries {
			if !entry.IsDir() && !embeddedFiles[entry.Name()] {
				t.Fatalf("repository %s/%s is missing from embedded assets", pair.embeddedDirectory, entry.Name())
			}
		}
	}
}
