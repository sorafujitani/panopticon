package panopticon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeHerdrPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "testdata", "fake_herdr.py")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func gitEnv() []string {
	environment := os.Environ()
	environment = append(environment,
		"GIT_AUTHOR_NAME=panopticon-test",
		"GIT_AUTHOR_EMAIL=panopticon-test@example.invalid",
		"GIT_COMMITTER_NAME=panopticon-test",
		"GIT_COMMITTER_EMAIL=panopticon-test@example.invalid",
	)
	return environment
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	command.Env = gitEnv()
	command.Stdin = nil
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
}

func initGitRepo(t *testing.T, root string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "--quiet")
	for relative, contents := range files {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=panopticon-test", "-c", "user.email=panopticon-test@example.invalid", "commit", "--quiet", "-m", "initial")
}

func writeSingleStepWorkflow(t *testing.T, root, role, writePolicy, submitKey string, agentArgs []string, timeoutSeconds int) Workflow {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	prompt := "RESULT_PATH={{RESULT_PATH}}\n" +
		"- Dedicated worktree only: `{{WORKTREE_PATH}}`\n" +
		"- worktree: `{{WORKTREE_PATH}}`\n" +
		"- run_id: `{{RUN_ID}}`\n" +
		"- step_id: `{{STEP_ID}}`\n" +
		"- role: `{{ROLE}}`\n"
	requiredFields := `["schema_version", "run_id", "step_id", "role", "status", "summary", "artifacts", "changed_files"]`
	artifactKinds := `["report"]`
	requiredBooleanFields := `[]`
	requiredListFields := `["changed_files"]`
	if role == "verifier" {
		prompt += "ENGINE_VERIFICATION={{ENGINE_VERIFICATION}}\n"
		requiredFields = `["schema_version", "run_id", "step_id", "role", "status", "summary", "artifacts", "verified", "verification"]`
		artifactKinds = `["verification"]`
		requiredBooleanFields = `["verified"]`
		requiredListFields = `["verification"]`
	}
	prompt += "## JSON contract\n{{JSON_CONTRACT}}\n"
	if err := os.WriteFile(filepath.Join(root, "prompt.md"), []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	submitLine := ""
	if submitKey != "" {
		submitLine = "submit_key = \"" + submitKey + "\"\n"
	}
	agentLine := ""
	if agentArgs != nil {
		encoded, err := json.Marshal(agentArgs)
		if err != nil {
			t.Fatal(err)
		}
		agentLine = "agent_args = " + string(encoded) + "\n"
	}
	workflow := "version = 1\nname = \"test\"\ndefault_verify = [[\"python3\", \"-c\", \"pass\"]]\n\n" +
		"[[steps]]\nid = \"step\"\nrole = \"" + role + "\"\nkind = \"codex\"\ndepends_on = []\n" +
		"read_policy = \"repo-and-dependencies\"\nwrite_policy = \"" + writePolicy + "\"\n" +
		"timeout_seconds = " + itoa(timeoutSeconds) + "\n" + submitLine + agentLine +
		"template = \"prompt.md\"\n\n[steps.contract]\nrequired_fields = " + requiredFields +
		"\nartifact_kinds = " + artifactKinds + "\nrequired_boolean_fields = " + requiredBooleanFields +
		"\nrequired_list_fields = " + requiredListFields + "\n"
	path := filepath.Join(root, "workflow.toml")
	if err := os.WriteFile(path, []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWorkflow(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func withController(t *testing.T, workflow Workflow, maxRetries int) Workflow {
	t.Helper()
	root := filepath.Dir(workflow.Path)
	prompt := `- run_id: ` + "`{{RUN_ID}}`" + `
- step_id: ` + "`{{STEP_ID}}`" + `
- role: ` + "`{{ROLE}}`" + `
- Worktree: ` + "`{{WORKTREE_PATH}}`" + `
RESULT_PATH={{RESULT_PATH}}
CONTROL_CONTEXT={{CONTROL_CONTEXT}}
ALLOWED_ACTIONS={{ALLOWED_ACTIONS}}
ELIGIBLE_NEXT_STEPS={{ELIGIBLE_NEXT_STEPS}}
## JSON contract
{{JSON_CONTRACT}}
`
	if err := os.WriteFile(filepath.Join(root, "controller.md"), []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(workflow.Path)
	if err != nil {
		t.Fatal(err)
	}
	controller := `
[controller]
role = "controller"
kind = "codex"
read_policy = "repo-and-dependencies"
write_policy = "none"
timeout_seconds = 30
max_retries = ` + itoa(maxRetries) + `
template = "controller.md"
`
	updated := strings.Replace(string(content), "\n[[steps]]", controller+"\n[[steps]]", 1)
	if err := os.WriteFile(workflow.Path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWorkflow(workflow.Path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return sign + string(digits)
}

type engineFixture struct {
	root    string
	logPath string
	engine  *FlowEngine
	options StartOptions
}

func newEngineFixture(t *testing.T, mode, role, writePolicy, submitKey string, agentArgs []string, verify [][]string, timeoutSeconds int) engineFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	workflow := writeSingleStepWorkflow(t, filepath.Join(root, "workflow"), role, writePolicy, submitKey, agentArgs, timeoutSeconds)
	store, err := NewRunStore(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	environment := currentEnvironment()
	environment["HERDR_ENV"] = "1"
	environment["FAKE_HERDR_MODE"] = mode
	environment["FAKE_HERDR_LOG"] = filepath.Join(root, "herdr.jsonl")
	client := NewHerdrClient(fakeHerdrPath(t), environment)
	if verify == nil {
		verify = workflow.DefaultVerify
	}
	return engineFixture{
		root:    root,
		logPath: filepath.Join(root, "herdr.jsonl"),
		engine:  NewFlowEngine(store, client),
		options: StartOptions{
			Repo:           repo,
			Workflow:       workflow,
			Task:           "fake task",
			VerifyCommands: verify,
			UseWorktree:    false,
			Background:     false,
			ScriptPath:     filepath.Join(repositoryRoot(t), "bin", "panopticon"),
		},
	}
}

func herdrCalls(t *testing.T, logPath string) [][]string {
	t.Helper()
	content, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	calls := [][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var call []string
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatalf("invalid herdr log line %q: %v", line, err)
		}
		calls = append(calls, call)
	}
	return calls
}

func filterCalls(calls [][]string, command ...string) [][]string {
	matched := [][]string{}
	for _, call := range calls {
		if len(call) < len(command) {
			continue
		}
		ok := true
		for index, part := range command {
			if call[index] != part {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, call)
		}
	}
	return matched
}

func containsArg(call []string, value string) bool {
	for _, item := range call {
		if item == value {
			return true
		}
	}
	return false
}

func argAfter(call []string, flag string) string {
	for index, item := range call {
		if item == flag && index+1 < len(call) {
			return call[index+1]
		}
	}
	return ""
}

func stepState(state map[string]any, id string) map[string]any {
	return mapValue(stateMap(state, "steps")[id])
}

func errorType(state map[string]any) string {
	return stringValue(mapValue(state["error"])["type"])
}

func errorCode(state map[string]any) string {
	return stringValue(mapValue(state["error"])["code"])
}

func stepErrorCode(state map[string]any, id string) string {
	return stringValue(mapValue(stepState(state, id)["error"])["code"])
}

func stringList(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func mustCreate(t *testing.T, fixture engineFixture) map[string]any {
	t.Helper()
	state, err := fixture.engine.CreateRun(fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func captureMain(t *testing.T, args []string) (int, string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = writer
	code := Main(args)
	_ = writer.Close()
	os.Stdout = stdout
	buf := make([]byte, 0, 16*1024)
	tmp := make([]byte, 4096)
	for {
		n, readErr := reader.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if readErr != nil {
			break
		}
	}
	_ = reader.Close()
	return code, string(buf)
}
