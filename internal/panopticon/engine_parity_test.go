package panopticon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackgroundLaunchAcceptsEmptyPaneRunStdoutAndPassesHerdrBin(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	fixture.options.Background = true
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "running" {
		t.Fatalf("status=%v", state["status"])
	}
	command := stringList(stateMap(state, "orchestrator")["command"])
	if argAfter(command, "--herdr-bin") != fakeHerdrPath(t) {
		t.Fatalf("orchestrator command missing herdr bin: %#v", command)
	}
	paneRuns := filterCalls(herdrCalls(t, fixture.logPath), "pane", "run")
	if len(paneRuns) != 1 || !strings.Contains(paneRuns[0][3], "--herdr-bin") {
		t.Fatalf("pane run argv: %#v", paneRuns)
	}
}

func TestSubmitKeyUsesRawPromptAndPaneKeyThenSettles(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "ctrl+enter", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "completed" {
		t.Fatalf("status=%v error=%v", state["status"], state["error"])
	}
	if stringValue(stepState(state, "step")["submit_key"]) != "ctrl+enter" {
		t.Fatalf("submit_key not persisted: %#v", stepState(state, "step")["submit_key"])
	}
	calls := herdrCalls(t, fixture.logPath)
	promptCalls := filterCalls(calls, "agent", "prompt")
	if len(promptCalls) != 1 || containsArg(promptCalls[0], "--wait") {
		t.Fatalf("prompt calls: %#v", promptCalls)
	}
	sendCalls := filterCalls(calls, "pane", "send-keys")
	if len(sendCalls) != 1 || sendCalls[0][2] != "w-fake-agent-pane" || sendCalls[0][3] != "ctrl+enter" {
		t.Fatalf("send-keys: %#v", sendCalls)
	}
	waitCalls := filterCalls(calls, "agent", "wait")
	if len(waitCalls) != 2 || argAfter(waitCalls[0], "--until") != "working" || containsArg(waitCalls[1], "--until") {
		t.Fatalf("wait calls: %#v", waitCalls)
	}
}

func TestModelEffortAndAgentArgsArePassedAsArgvToFakeAgentStart(t *testing.T) {
	args := []string{"--no-extensions", "--profile", "profile with spaces"}
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", args, nil, 30)
	fixture.options.Workflow.Steps[0].Kind = "pi"
	fixture.options.Workflow.Steps[0].Model = "anthropic/claude-sonnet-4"
	fixture.options.Workflow.Steps[0].Effort = "high"
	state := mustCreate(t, fixture)
	step := stepState(state, "step")
	if got := stringList(step["agent_args"]); strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("step agent_args=%v", got)
	}
	if stringValue(step["model"]) != "anthropic/claude-sonnet-4" || stringValue(step["effort"]) != "high" {
		t.Fatalf("step model/effort=%v/%v", step["model"], step["effort"])
	}
	workflowSteps, _ := stateMap(state, "workflow")["steps"].([]any)
	if len(workflowSteps) == 0 {
		t.Fatal("workflow steps are empty")
	}
	workflowStep := mapValue(workflowSteps[0])
	if strings.Join(stringList(workflowStep["agent_args"]), "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("workflow agent_args=%#v", workflowSteps)
	}
	if stringValue(workflowStep["model"]) != "anthropic/claude-sonnet-4" || stringValue(workflowStep["effort"]) != "high" {
		t.Fatalf("workflow model/effort=%v/%v", workflowStep["model"], workflowStep["effort"])
	}
	agent := mapValue(step["agent"])
	expected := "pan-" + lastRunPart(stringValue(state["run_id"])) + "-step"
	if stringValue(agent["name"]) != expected || stringValue(agent["target"]) != expected {
		t.Fatalf("agent identity=%#v want %s", agent, expected)
	}
	if stringValue(agent["model"]) != "anthropic/claude-sonnet-4" || stringValue(agent["effort"]) != "high" {
		t.Fatalf("agent model/effort=%v/%v", agent["model"], agent["effort"])
	}
	startCalls := filterCalls(herdrCalls(t, fixture.logPath), "agent", "start")
	want := []string{"agent", "start", expected, "--kind", "pi", "--pane", "w-fake-agent-pane", "--timeout", "30000", "--", "--model", "anthropic/claude-sonnet-4", "--thinking", "high", "--no-extensions", "--profile", "profile with spaces"}
	if len(startCalls) != 1 || strings.Join(startCalls[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("start calls=%#v want=%#v", startCalls, want)
	}
}

func TestAgentStartTimeoutIsClampedToHerdrRange(t *testing.T) {
	cases := []struct {
		seconds  int
		expected string
	}{{1, "3001"}, {30, "30000"}, {900, "300000"}, {86400, "300000"}}
	for _, item := range cases {
		arguments := agentStartArgs("agent", StepSpec{Kind: "codex", TimeoutSec: item.seconds}, "pane")
		if argAfter(arguments, "--timeout") != item.expected {
			t.Fatalf("timeout %d -> %q", item.seconds, argAfter(arguments, "--timeout"))
		}
	}
}

func TestAgentStartClampKeepsStepPromptTimeout(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 900)
	state := mustCreate(t, fixture)
	calls := herdrCalls(t, fixture.logPath)
	startCalls := filterCalls(calls, "agent", "start")
	promptCalls := filterCalls(calls, "agent", "prompt")
	if argAfter(startCalls[0], "--timeout") != "300000" {
		t.Fatalf("start timeout=%q", argAfter(startCalls[0], "--timeout"))
	}
	if argAfter(promptCalls[0], "--timeout") != "900000" {
		t.Fatalf("prompt timeout=%q", argAfter(promptCalls[0], "--timeout"))
	}
	workflowSteps, _ := stateMap(state, "workflow")["steps"].([]any)
	if ParseInt64(mapValue(workflowSteps[0])["timeout_seconds"]) != 900 {
		t.Fatalf("workflow timeout=%v", mapValue(workflowSteps[0])["timeout_seconds"])
	}
}

func TestChildAgentTabReceivesRecursionGuardEnvironment(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	mustCreate(t, fixture)
	var guarded [][]string
	for _, call := range filterCalls(herdrCalls(t, fixture.logPath), "tab", "create") {
		if containsArg(call, "--env") && argAfter(call, "--env") == "PANOPTICON_CHILD=1" {
			guarded = append(guarded, call)
		}
	}
	if len(guarded) != 1 {
		t.Fatalf("child tab creates=%#v", filterCalls(herdrCalls(t, fixture.logPath), "tab", "create"))
	}
}

func TestResumeRestartsExistingAgentWithTheSameModelEffortAndAgentArgs(t *testing.T) {
	args := []string{"--no-extensions", "--profile", "profile with spaces"}
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", args, nil, 30)
	workflowPath := fixture.options.Workflow.Path
	content, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), "kind = \"codex\"", "kind = \"pi\"\nmodel = \"anthropic/claude-sonnet-4\"\neffort = \"high\"", 1))
	if err := os.WriteFile(workflowPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.options.Workflow, err = LoadWorkflow(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	state := mustCreate(t, fixture)
	state["status"] = "failed"
	stepState(state, "step")["status"] = "failed"
	mapValue(stepState(state, "step")["agent"])["started"] = false
	if err := fixture.engine.Store.Save(stringValue(state["run_id"]), state); err != nil {
		t.Fatal(err)
	}
	resumed, err := fixture.engine.ResumeRun(stringValue(state["run_id"]), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(resumed["status"]) != "completed" {
		t.Fatalf("resume status=%v error=%v", resumed["status"], resumed["error"])
	}
	startCalls := filterCalls(herdrCalls(t, fixture.logPath), "agent", "start")
	if len(startCalls) != 2 {
		t.Fatalf("start calls=%#v", startCalls)
	}
	tail := startCalls[len(startCalls)-1]
	got := tail[len(tail)-8:]
	want := []string{"--", "--model", "anthropic/claude-sonnet-4", "--thinking", "high", "--no-extensions", "--profile", "profile with spaces"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("resume argv tail=%v", got)
	}
}

func TestSubmitKeyWrongKeyFailsTheStep(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "enter", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "failed" || stringValue(stepState(state, "step")["status"]) != "failed" || stepErrorCode(state, "step") != "invalid_key" {
		t.Fatalf("state=%v step=%v", state["status"], stepState(state, "step"))
	}
}

func TestSubmitKeyTimeoutIsRecordedAsTimedOut(t *testing.T) {
	fixture := newEngineFixture(t, "timeout", "developer", "worktree", "ctrl+enter", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "failed" || stringValue(stepState(state, "step")["status"]) != "timed_out" || stepErrorCode(state, "step") != "timeout" {
		t.Fatalf("state=%v step=%v", state["status"], stepState(state, "step"))
	}
}

func TestSubmitKeyResultWhileAgentIsStillWorkingTimesOut(t *testing.T) {
	fixture := newEngineFixture(t, "result-working-timeout", "developer", "worktree", "ctrl+enter", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "failed" || stringValue(stepState(state, "step")["status"]) != "timed_out" || stepErrorCode(state, "step") != "timeout" {
		t.Fatalf("state=%v step=%v", state["status"], stepState(state, "step"))
	}
	if len(filterCalls(herdrCalls(t, fixture.logPath), "agent", "get")) == 0 {
		t.Fatal("expected agent get")
	}
}

func TestSubmitKeyBlockedLifecycleIsRecordedAsBlocked(t *testing.T) {
	fixture := newEngineFixture(t, "blocked", "developer", "worktree", "ctrl+enter", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "blocked" || stringValue(stepState(state, "step")["status"]) != "blocked" || stepErrorCode(state, "step") != "agent_blocked" {
		t.Fatalf("state=%v step=%v", state["status"], stepState(state, "step"))
	}
}

func TestSubmitKeyFastCompletionWithIdleStatusIsAccepted(t *testing.T) {
	fixture := newEngineFixture(t, "fast-success", "developer", "worktree", "ctrl+enter", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "completed" {
		t.Fatalf("status=%v error=%v", state["status"], state["error"])
	}
	gets := filterCalls(herdrCalls(t, fixture.logPath), "agent", "get")
	if len(gets) != 1 || gets[0][2] != stringValue(mapValue(stepState(state, "step")["agent"])["target"]) {
		t.Fatalf("get calls=%#v", gets)
	}
}

func TestSubmitKeyIgnoresTransientIdleBeforeResult(t *testing.T) {
	fixture := newEngineFixture(t, "transient-idle-before-result", "developer", "worktree", "ctrl+enter", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "completed" || stringValue(stepState(state, "step")["status"]) != "completed" {
		t.Fatalf("state=%v step=%v error=%v", state["status"], stepState(state, "step")["status"], state["error"])
	}
	if calls := filterCalls(herdrCalls(t, fixture.logPath), "agent", "wait"); len(calls) != 4 {
		t.Fatalf("wait calls=%#v", calls)
	}
}

func TestSubmitKeyWorkingTimeoutFallsBackToTimeoutHandling(t *testing.T) {
	fixture := newEngineFixture(t, "delayed-working-before-result", "developer", "worktree", "ctrl+enter", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "failed" || stringValue(stepState(state, "step")["status"]) != "timed_out" || stepErrorCode(state, "step") != "timeout" {
		t.Fatalf("state=%v step=%v error=%v", state["status"], stepState(state, "step")["status"], state["error"])
	}
	if calls := filterCalls(herdrCalls(t, fixture.logPath), "agent", "wait"); len(calls) != 1 || argAfter(calls[0], "--until") != "working" {
		t.Fatalf("wait calls=%#v", calls)
	}
}

func TestSubmitKeyBlockedStatusAfterTimeoutIsPropagated(t *testing.T) {
	fixture := newEngineFixture(t, "blocked-timeout", "developer", "worktree", "ctrl+enter", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "blocked" || stringValue(stepState(state, "step")["status"]) != "blocked" || stepErrorCode(state, "step") != "agent_blocked" {
		t.Fatalf("state=%v step=%v", state["status"], stepState(state, "step"))
	}
	if len(filterCalls(herdrCalls(t, fixture.logPath), "agent", "get")) == 0 {
		t.Fatal("expected agent get")
	}
}

func TestSubmitKeyUnspecifiedKeepsLegacyWaitCommand(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "completed" {
		t.Fatalf("status=%v error=%v", state["status"], state["error"])
	}
	calls := herdrCalls(t, fixture.logPath)
	promptCalls := filterCalls(calls, "agent", "prompt")
	if len(promptCalls) != 1 || !containsArg(promptCalls[0], "--wait") {
		t.Fatalf("prompt calls=%#v", promptCalls)
	}
	if len(filterCalls(calls, "pane", "send-keys")) != 0 || len(filterCalls(calls, "agent", "wait")) != 0 {
		t.Fatalf("unexpected submit-key commands: %#v", calls)
	}
}

func TestBlockedResultCanBeResumed(t *testing.T) {
	fixture := newEngineFixture(t, "blocked", "developer", "worktree", "", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "blocked" {
		t.Fatalf("status=%v", state["status"])
	}
	fixture.engine.Client.Environ["FAKE_HERDR_MODE"] = "success"
	resumed, err := fixture.engine.ResumeRun(stringValue(state["run_id"]), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(resumed["status"]) != "completed" || ParseInt64(stepState(resumed, "step")["attempts"]) != 2 {
		t.Fatalf("resume=%v attempts=%v", resumed["status"], stepState(resumed, "step")["attempts"])
	}
}

func TestWriterChangedFilesMatchTheRealWorktreeSnapshot(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	fixture.engine.Client.Environ["FAKE_HERDR_CHANGED_FILES"] = `["changed.py"]`
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "completed" {
		t.Fatalf("status=%v error=%v", state["status"], state["error"])
	}
	if got := stringList(stepState(state, "step")["actual_changed_files"]); strings.Join(got, ",") != "changed.py" {
		t.Fatalf("actual=%v", got)
	}
	if _, err := os.Stat(filepath.Join(stringValue(stateMap(state, "worktree")["path"]), "changed.py")); err != nil {
		t.Fatal(err)
	}
}

func TestWriterChangedFilesMismatchFailsClosed(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	fixture.engine.Client.Environ["FAKE_HERDR_CHANGED_FILES"] = `["declared.py"]`
	fixture.engine.Client.Environ["FAKE_HERDR_ACTUAL_CHANGED_FILES"] = `["actual.py"]`
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "failed" || errorType(state) != "worktree_change_mismatch" {
		t.Fatalf("status=%v error=%v", state["status"], state["error"])
	}
	if got := stringList(stepState(state, "step")["actual_changed_files"]); strings.Join(got, ",") != "actual.py" {
		t.Fatalf("actual=%v", got)
	}
}

func TestReadOnlyWorktreeChangeFailsClosed(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "none", "", nil, nil, 30)
	fixture.engine.Client.Environ["FAKE_HERDR_ACTUAL_CHANGED_FILES"] = `["sneaky.py"]`
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "failed" || errorType(state) != "worktree_changed_read_only" {
		t.Fatalf("status=%v error=%v", state["status"], state["error"])
	}
	if got := stringList(stepState(state, "step")["actual_changed_files"]); strings.Join(got, ",") != "sneaky.py" {
		t.Fatalf("actual=%v", got)
	}
}

func TestSnapshotFailureFailsClosed(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	initGitRepo(t, fixture.options.Repo, map[string]string{"tracked.txt": "tracked\n"})
	directory := t.TempDir()
	script := "#!/bin/sh\necho 'fatal: index unavailable' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(directory, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	state, err := fixture.engine.CreateRun(fixture.options)
	if err != nil {
		t.Fatalf("create returned error: %v", err)
	}
	if stringValue(state["status"]) != "failed" || errorType(state) != "worktree_snapshot_failed" {
		t.Fatalf("status=%v error=%v", state["status"], state["error"])
	}
}

func TestVerifierSuccessUsesEngineVerificationResult(t *testing.T) {
	fixture := newEngineFixture(t, "success", "verifier", "none", "", nil, [][]string{{"python3", "-c", "print('ok')"}}, 30)
	state := mustCreate(t, fixture)
	verification := mapValue(stepState(state, "step")["verification"])
	if stringValue(state["status"]) != "completed" || !boolValue(verification["all_succeeded"], false) {
		t.Fatalf("status=%v verification=%v error=%v", state["status"], verification, state["error"])
	}
	record := mapValue(mapValue(verification["commands"].([]any)[0]))
	if ParseInt64(record["returncode"]) != 0 || strings.TrimSpace(stringValue(record["stdout"])) != "ok" {
		t.Fatalf("record=%v", record)
	}
	if _, err := os.Stat(stringValue(stepState(state, "step")["verification_artifact_path"])); err != nil {
		t.Fatal(err)
	}
}

func TestVerifierFailureIsNotOverriddenByAgentVerifiedTrue(t *testing.T) {
	fixture := newEngineFixture(t, "success", "verifier", "none", "", nil, [][]string{{"python3", "-c", "import sys; print('o' * 6000); print('e' * 6000, file=sys.stderr); sys.exit(3)"}}, 30)
	fixture.engine.Client.Environ["FAKE_HERDR_VERIFIED"] = "true"
	state := mustCreate(t, fixture)
	record := mapValue(mapValue(mapValue(stepState(state, "step")["verification"])["commands"].([]any)[0]))
	if stringValue(state["status"]) != "failed" || errorType(state) != "verification_failed" {
		t.Fatalf("status=%v error=%v", state["status"], state["error"])
	}
	if boolValue(mapValue(stepState(state, "step")["verification"])["all_succeeded"], true) {
		t.Fatal("verification should not succeed")
	}
	if ParseInt64(record["returncode"]) != 3 || len(stringValue(record["stdout"])) > 4000 || len(stringValue(record["stderr"])) > 4000 {
		t.Fatalf("record=%v", record)
	}
}

func TestResumeRejectsModifiedWorkflowDigest(t *testing.T) {
	fixture := newEngineFixture(t, "blocked", "developer", "worktree", "", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(stateMap(state, "workflow")["digest"]) != fixture.options.Workflow.Digest {
		t.Fatal("digest mismatch at create")
	}
	path := fixture.options.Workflow.Path
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(content, []byte("\n# modified\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.engine.ResumeRun(stringValue(state["run_id"]), nil)
	if err == nil || !strings.Contains(err.Error(), "workflow TOML") {
		t.Fatalf("expected digest error, got %v", err)
	}
}

func TestReviewerDoesNotClaimExistingDeveloperChanges(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	workflowRoot := filepath.Join(fixture.root, "workflow")
	prompt := "RESULT_PATH={{RESULT_PATH}}\n- Dedicated worktree only: `{{WORKTREE_PATH}}`\n- worktree: `{{WORKTREE_PATH}}`\n- run_id: `{{RUN_ID}}`\n- step_id: `{{STEP_ID}}`\n- role: `{{ROLE}}`\n## JSON contract\n{{JSON_CONTRACT}}\n"
	if err := os.WriteFile(filepath.Join(workflowRoot, "prompt.md"), []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	content := `version = 1
name = "developer-reviewer"
default_verify = [["python3", "-c", "pass"]]

[[steps]]
id = "developer"
role = "developer"
kind = "codex"
depends_on = []
read_policy = "repo-and-dependencies"
write_policy = "worktree"
timeout_seconds = 30
template = "prompt.md"

[steps.contract]
required_fields = ["schema_version", "run_id", "step_id", "role", "status", "summary", "artifacts", "changed_files"]
artifact_kinds = ["change"]
required_list_fields = ["changed_files"]

[[steps]]
id = "reviewer"
role = "reviewer"
kind = "codex"
depends_on = ["developer"]
read_policy = "worktree"
write_policy = "none"
timeout_seconds = 30
template = "prompt.md"

[steps.contract]
required_fields = ["schema_version", "run_id", "step_id", "role", "status", "summary", "artifacts", "changed_files", "findings", "decision", "needs_fixer"]
artifact_kinds = ["review"]
required_boolean_fields = ["needs_fixer"]
required_list_fields = ["changed_files", "findings"]
`
	if err := os.WriteFile(filepath.Join(workflowRoot, "workflow.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow, err := LoadWorkflow(filepath.Join(workflowRoot, "workflow.toml"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.options.Workflow = workflow
	fixture.options.VerifyCommands = workflow.DefaultVerify
	fixture.engine.Client.Environ["FAKE_HERDR_CHANGED_FILES_DEVELOPER"] = `["developer.py"]`
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "completed" {
		t.Fatalf("status=%v error=%v developer=%v reviewer=%v", state["status"], state["error"], stepState(state, "developer"), stepState(state, "reviewer"))
	}
	if strings.Join(stringList(stepState(state, "developer")["actual_changed_files"]), ",") != "developer.py" {
		t.Fatalf("developer actual=%v", stepState(state, "developer")["actual_changed_files"])
	}
	if len(stringList(stepState(state, "reviewer")["actual_changed_files"])) != 0 {
		t.Fatalf("reviewer actual=%v", stepState(state, "reviewer")["actual_changed_files"])
	}
	if len(stringList(mapValue(stepState(state, "reviewer")["result"])["changed_files"])) != 0 {
		t.Fatalf("reviewer result changed=%v", mapValue(stepState(state, "reviewer")["result"])["changed_files"])
	}
}

func TestFailedResultCanBeResumed(t *testing.T) {
	fixture := newEngineFixture(t, "failed", "developer", "worktree", "", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "failed" {
		t.Fatalf("status=%v", state["status"])
	}
	fixture.engine.Client.Environ["FAKE_HERDR_MODE"] = "success"
	resumed, err := fixture.engine.ResumeRun(stringValue(state["run_id"]), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(resumed["status"]) != "completed" {
		t.Fatalf("resume=%v error=%v", resumed["status"], resumed["error"])
	}
}

func TestTimedOutStepRecoversLateResultWithoutRerun(t *testing.T) {
	fixture := newEngineFixture(t, "timeout", "developer", "worktree", "ctrl+enter", nil, nil, 1)
	state := mustCreate(t, fixture)
	step := stepState(state, "step")
	if stringValue(step["status"]) != "timed_out" {
		t.Fatalf("step=%v", step)
	}
	artifact := filepath.Join(filepath.Dir(stringValue(step["result_path"])), "late-result.txt")
	if err := os.WriteFile(artifact, []byte("late result\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{
		"schema_version": int64(1), "run_id": state["run_id"], "step_id": "step", "role": "developer",
		"status": "success", "summary": "late success",
		"artifacts":     []any{map[string]any{"path": artifact, "kind": "report", "description": "late result"}},
		"changed_files": []any{}, "tests": []any{},
	}
	if err := atomicWriteJSON(stringValue(step["result_path"]), result); err != nil {
		t.Fatal(err)
	}
	resumed, err := fixture.engine.ResumeRun(stringValue(state["run_id"]), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(resumed["status"]) != "completed" || ParseInt64(stepState(resumed, "step")["attempts"]) != 1 {
		t.Fatalf("resume=%v attempts=%v error=%v", resumed["status"], stepState(resumed, "step")["attempts"], resumed["error"])
	}
	if stepState(resumed, "step")["error"] != nil || resumed["error"] != nil {
		t.Fatalf("stale errors: run=%v step=%v", resumed["error"], stepState(resumed, "step")["error"])
	}
}

func TestCommandBlockedIsPersistedAndResumable(t *testing.T) {
	fixture := newEngineFixture(t, "command-blocked", "developer", "worktree", "", nil, nil, 30)
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "blocked" || stepErrorCode(state, "step") != "agent_blocked" {
		t.Fatalf("state=%v step=%v", state["status"], stepState(state, "step"))
	}
	fixture.engine.Client.Environ["FAKE_HERDR_MODE"] = "success"
	resumed, err := fixture.engine.ResumeRun(stringValue(state["run_id"]), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(resumed["status"]) != "completed" {
		t.Fatalf("resume=%v", resumed["status"])
	}
}

func TestWorktreeModeKeepsAgentsOutsideRepository(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	dedicated := filepath.Join(fixture.root, "dedicated-worktree")
	initGitRepo(t, dedicated, map[string]string{"initial.txt": "initial\n"})
	fixture.engine.Client.Environ["FAKE_HERDR_WORKTREE"] = dedicated
	fixture.options.UseWorktree = true
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "completed" {
		t.Fatalf("status=%v error=%v", state["status"], state["error"])
	}
	worktree, _ := filepath.Abs(stringValue(stateMap(state, "worktree")["path"]))
	repo, _ := filepath.Abs(fixture.options.Repo)
	if worktree == repo {
		t.Fatal("worktree should be outside repo")
	}
	if !boolValue(stateMap(state, "resources")["worktree_created"], false) {
		t.Fatal("worktree_created")
	}
}

func TestWorktreeModeRejectsRepositoryPath(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	fixture.engine.Client.Environ["FAKE_HERDR_WORKTREE"] = fixture.options.Repo
	fixture.options.UseWorktree = true
	_, err := fixture.engine.CreateRun(fixture.options)
	if err == nil || !strings.Contains(err.Error(), "outside the target repository") {
		t.Fatalf("expected outside-repo error, got %v", err)
	}
}

func TestChangedFilesMustStayRelativeToWorktree(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	fixture.engine.Client.Environ["FAKE_HERDR_CHANGED_FILES"] = "../escape.py"
	state := mustCreate(t, fixture)
	if stringValue(state["status"]) != "failed" || errorType(state) != "invalid_result" || !strings.Contains(stringValue(mapValue(state["error"])["message"]), "worktree") {
		t.Fatalf("status=%v error=%v", state["status"], state["error"])
	}
}

func TestArtifactsAreLimitedToRunDirAndWorktreeRegardlessOfReadPolicy(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	active := filepath.Join(root, "active")
	sibling := filepath.Join(root, "sibling")
	initGitRepo(t, repo, map[string]string{"repo-report.txt": "repo\n"})
	runGit(t, repo, "worktree", "add", "--quiet", "--detach", active)
	runGit(t, repo, "worktree", "add", "--quiet", "--detach", sibling)
	linkedArtifact := filepath.Join(sibling, "linked-report.txt")
	if err := os.WriteFile(linkedArtifact, []byte("linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worktreeArtifact := filepath.Join(active, "worktree-report.txt")
	if err := os.WriteFile(worktreeArtifact, []byte("worktree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"run_id": "run-test", "run_dir": filepath.Join(root, "run"), "repo": repo,
		"worktree": map[string]any{"path": active},
	}
	spec := StepSpec{
		ID: "step", Role: "scout", ReadPolicy: "repo-and-dependencies", WritePolicy: "none",
		Contract: Contract{ArtifactKinds: []string{"report"}},
	}
	result := map[string]any{
		"schema_version": 1, "run_id": "run-test", "step_id": "step", "role": "scout",
		"status": "success", "summary": "ok",
		"artifacts": []any{
			map[string]any{"path": worktreeArtifact, "kind": "report", "description": "worktree"},
		},
		"changed_files": []any{}, "tests": []any{},
	}
	if valid, reason := validateResult(result, spec, state); !valid {
		t.Fatalf("worktree artifact rejected: %s", reason)
	}
	for _, artifact := range []string{filepath.Join(repo, "repo-report.txt"), linkedArtifact} {
		result["artifacts"] = []any{map[string]any{"path": artifact, "kind": "report", "description": "outside"}}
		if valid, _ := validateResult(result, spec, state); valid {
			t.Fatalf("repo-and-dependencies accepted artifact outside run/worktree: %s", artifact)
		}
	}
	spec.ReadPolicy = "worktree"
	result["artifacts"] = []any{map[string]any{"path": worktreeArtifact, "kind": "report", "description": "worktree"}}
	if valid, _ := validateResult(result, spec, state); !valid {
		t.Fatal("worktree-only policy rejected worktree artifact")
	}
}

func TestCancelLockConflictReturnsPreloadedCompactState(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	runID := "run-cancel-locked"
	runDir, err := fixture.engine.Store.RunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	state := fixture.engine.newState(runID, fixture.options, runDir)
	state["status"] = "running"
	state["current_step"] = "step"
	state["error"] = map[string]any{"message": "still running"}
	stepState(state, "step")["status"] = "running"
	if _, err := fixture.engine.Store.Create(runID, state); err != nil {
		t.Fatal(err)
	}
	lock, err := fixture.engine.Store.Lock(runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lock.release()
	result, err := fixture.engine.CancelRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(result["status"]) != "cancel_requested" {
		t.Fatalf("status=%v", result["status"])
	}
	payload := CompactState(result)
	if payload["repo"] != state["repo"] || payload["worktree"] != stateMap(state, "worktree")["path"] {
		t.Fatalf("compact=%v", payload)
	}
	if payload["current_step"] != "step" || stringValue(mapValue(payload["steps"].(map[string]any)["step"])["status"]) != "running" {
		t.Fatalf("compact steps=%v", payload["steps"])
	}
	if stringValue(mapValue(payload["error"])["message"]) != "still running" || payload["updated_at"] != state["updated_at"] {
		t.Fatalf("compact error/updated=%v %v", payload["error"], payload["updated_at"])
	}
	if _, err := os.Stat(filepath.Join(runDir, "cancel.request")); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupUsesCurrentUniqueWorkspaceIDUnderRunLock(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	runID := "run-cleanup"
	state := fixture.engine.newState(runID, fixture.options, mustRunDir(t, fixture.engine.Store, runID))
	worktree := filepath.Join(fixture.root, "dedicated-worktree")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	state["status"] = "failed"
	stateMap(state, "resources")["worktree_created"] = true
	stateMap(state, "resources")["workspace_id"] = "w-stale-workspace"
	stateMap(state, "worktree")["path"] = worktree
	stateMap(state, "worktree")["branch"] = "panopticon/current"
	if _, err := fixture.engine.Store.Create(runID, state); err != nil {
		t.Fatal(err)
	}
	list, _ := json.Marshal(map[string]any{"worktrees": []any{map[string]any{"workspace_id": "w-current-workspace", "worktree_path": worktree, "branch": "panopticon/current"}}})
	fixture.engine.Client.Environ["FAKE_HERDR_WORKTREE_LIST"] = string(list)
	result, err := fixture.engine.Cleanup(runID, false, 24, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || !boolValue(result[0]["worktree_removed"], false) {
		t.Fatalf("cleanup result=%v", result)
	}
	if _, err := os.Stat(mustRunDir(t, fixture.engine.Store, runID)); !os.IsNotExist(err) {
		t.Fatalf("run dir still exists: %v", err)
	}
	remove := filterCalls(herdrCalls(t, fixture.logPath), "worktree", "remove")
	if len(remove) != 1 || argAfter(remove[0], "--workspace") != "w-current-workspace" {
		t.Fatalf("remove calls=%#v", remove)
	}
}

func TestCleanupRejectsLockCompetition(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	runID := "run-locked"
	state := fixture.engine.newState(runID, fixture.options, mustRunDir(t, fixture.engine.Store, runID))
	state["status"] = "failed"
	if _, err := fixture.engine.Store.Create(runID, state); err != nil {
		t.Fatal(err)
	}
	lock, err := fixture.engine.Store.Lock(runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lock.release()
	_, err = fixture.engine.Cleanup(runID, false, 24, false)
	if _, ok := err.(*StateError); !ok {
		t.Fatalf("expected StateError, got %T %v", err, err)
	}
	if _, err := os.Stat(mustRunDir(t, fixture.engine.Store, runID)); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryHonoursABlockedResultInsteadOfRerunningAgent(t *testing.T) {
	fixture := newEngineFixture(t, "success", "developer", "worktree", "", nil, nil, 30)
	runID := "run-recovery"
	runDir := mustRunDir(t, fixture.engine.Store, runID)
	state := fixture.engine.newState(runID, fixture.options, runDir)
	if _, err := fixture.engine.Store.Create(runID, state); err != nil {
		t.Fatal(err)
	}
	stepState(state, "step")["status"] = "running"
	state["status"] = "running"
	if err := fixture.engine.snapshotStep(state, fixture.options.Workflow.Steps[0], "before"); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(stringValue(state["run_dir"]), "blocked-report.txt")
	if err := os.WriteFile(artifact, []byte("blocked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{
		"schema_version": 1, "run_id": runID, "step_id": "step", "role": "developer",
		"status": "blocked", "summary": "resume later",
		"artifacts":     []any{map[string]any{"path": artifact, "kind": "report", "description": "blocked report"}},
		"changed_files": []any{}, "tests": []any{},
	}
	if err := atomicWriteJSON(stringValue(stepState(state, "step")["result_path"]), result); err != nil {
		t.Fatal(err)
	}
	if err := fixture.engine.Store.Save(runID, state); err != nil {
		t.Fatal(err)
	}
	resumed, err := fixture.engine.ResumeRun(runID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(resumed["status"]) != "blocked" || stringValue(stepState(resumed, "step")["status"]) != "blocked" {
		t.Fatalf("resume=%v step=%v", resumed["status"], stepState(resumed, "step"))
	}
	if len(filterCalls(herdrCalls(t, fixture.logPath), "agent", "prompt")) != 0 {
		t.Fatalf("agent was prompted: %#v", herdrCalls(t, fixture.logPath))
	}
}

func mustRunDir(t *testing.T, store *RunStore, runID string) string {
	t.Helper()
	path, err := store.RunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
