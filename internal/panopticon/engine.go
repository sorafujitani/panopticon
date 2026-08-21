package panopticon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var runTerminal = map[string]bool{"completed": true, "failed": true, "cancelled": true}
var stepSuccess = map[string]bool{"completed": true, "skipped": true}
var stepTerminal = map[string]bool{"completed": true, "skipped": true, "failed": true, "blocked": true, "timed_out": true, "cancelled": true}

const (
	maxVerificationOutput       = 4000
	workingWaitMilliseconds     = 5000
	herdrAgentStartMinTimeoutMS = 3001
	herdrAgentStartMaxTimeoutMS = 300000
)

type StartOptions struct {
	Repo           string
	Workflow       Workflow
	Task           string
	VerifyCommands [][]string
	UseWorktree    bool
	WorktreePath   string
	Branch         string
	Base           string
	Background     bool
	ScriptPath     string
}

type FlowEngine struct {
	Store    *RunStore
	Client   *HerdrClient
	workflow *Workflow
}

func NewFlowEngine(store *RunStore, client *HerdrClient) *FlowEngine {
	return &FlowEngine{Store: store, Client: client}
}

func displayCommand(command []string) string { return strings.Join(command, " ") }

func clampAgentStartTimeoutMS(timeout int) int {
	if timeout < herdrAgentStartMinTimeoutMS {
		return herdrAgentStartMinTimeoutMS
	}
	if timeout > herdrAgentStartMaxTimeoutMS {
		return herdrAgentStartMaxTimeoutMS
	}
	return timeout
}

func nowEpoch(value any) float64 {
	text, _ := value.(string)
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return 0
	}
	return float64(parsed.Unix())
}

func mapValue(value any) map[string]any {
	if table, ok := value.(map[string]any); ok {
		return table
	}
	return nil
}

func stateMap(state map[string]any, key string) map[string]any {
	table, _ := state[key].(map[string]any)
	return table
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func boolValue(value any, fallback bool) bool {
	if typed, ok := value.(bool); ok {
		return typed
	}
	return fallback
}

func intValue(value any) int {
	return int(ParseInt64(value))
}

func appendUniqueString(values []any, value string) []any {
	for _, item := range values {
		if stringValue(item) == value {
			return values
		}
	}
	return append(values, value)
}

func stringSlice(values any) []string {
	items, _ := values.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok && text != "" {
			result = append(result, text)
		}
	}
	return result
}

func (engine *FlowEngine) appendEvent(state map[string]any, event string, details map[string]any) {
	events, _ := state["events"].([]any)
	entry := map[string]any{"at": utcNow(), "event": event}
	for key, value := range details {
		entry[key] = value
	}
	state["events"] = append(events, entry)
}

func (engine *FlowEngine) save(state map[string]any) error {
	return engine.Store.Save(stringValue(state["run_id"]), state)
}

func resultContract(spec StepSpec, runID string) map[string]any {
	kinds := strings.Join(spec.Contract.ArtifactKinds, ", ")
	return map[string]any{
		"schema_version": int64(1),
		"run_id":         runID,
		"step_id":        spec.ID,
		"role":           spec.Role,
		"status":         "success | blocked | failed",
		"summary":        "short string",
		"artifacts": []any{map[string]any{
			"path":        "/absolute/path/to/an-existing-file",
			"kind":        "one of: " + kinds,
			"description": "artifact description",
		}},
		"changed_files": []string{"worktree-relative/path"},
		"tests":         []string{"description of verification performed"},
		"contract":      spec.Contract.AsMap(),
	}
}

func errorPayload(err error) map[string]any {
	result := map[string]any{"message": err.Error(), "type": errorTypeName(err)}
	if command, ok := err.(*CommandError); ok {
		result["code"] = command.Code
		if command.ReturnCode != nil {
			result["returncode"] = *command.ReturnCode
		} else {
			result["returncode"] = nil
		}
		if command.Stderr != "" {
			result["stderr"] = truncateTail(command.Stderr, 2000)
		}
	}
	return result
}

// truncateTail keeps the last `maximum` runes, matching Python's value[-maximum:].
func truncateTail(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	runes := []rune(strings.ToValidUTF8(value, "\uFFFD"))
	if len(runes) > maximum {
		return string(runes[len(runes)-maximum:])
	}
	return string(runes)
}

func errorTypeName(err error) string {
	switch err.(type) {
	case *CommandError:
		return "CommandError"
	case *FlowError:
		return "FlowError"
	case *HerdrError:
		return "HerdrError"
	case *StateError:
		return "StateError"
	default:
		return "error"
	}
}

func (engine *FlowEngine) snapshotStep(state map[string]any, spec StepSpec, phase string) error {
	if phase != "before" && phase != "after" {
		return flowError("unknown worktree snapshot phase: %s", phase)
	}
	worktree := stateMap(state, "worktree")
	if worktree == nil {
		return flowError("state worktree is invalid")
	}
	enabled, ok := worktree["enabled"].(bool)
	if !ok {
		return flowError("state worktree.enabled is invalid")
	}
	runDir := stringValue(state["run_dir"])
	snapshot, err := snapshotWorktree(stringValue(worktree["path"]), []string{runDir}, !enabled)
	if err != nil {
		return err
	}
	steps := stateMap(state, "steps")
	stepState := mapValue(steps[spec.ID])
	stepState["worktree_snapshot_"+phase] = map[string]any{"captured_at": utcNow(), "files": snapshot}
	if phase == "after" {
		before := mapValue(stepState["worktree_snapshot_before"])
		beforeFiles, validBefore := snapshotFiles(before["files"])
		if before == nil || !validBefore {
			return flowError("step %s has no worktree before snapshot", spec.ID)
		}
		changed := snapshotDiff(beforeFiles, snapshot)
		items := make([]any, 0, len(changed))
		for _, item := range changed {
			items = append(items, item)
		}
		stepState["actual_changed_files"] = items
	}
	return engine.save(state)
}

func stringMap(value map[string]any) map[string]string {
	result := map[string]string{}
	for key, item := range value {
		if text, ok := item.(string); ok {
			result[key] = text
		}
	}
	return result
}

func snapshotFiles(value any) (map[string]string, bool) {
	switch typed := value.(type) {
	case map[string]string:
		return typed, true
	case map[string]any:
		return stringMap(typed), true
	default:
		return nil, false
	}
}

func commandsFromState(value any) [][]string {
	items, _ := value.([]any)
	result := make([][]string, 0, len(items))
	for _, raw := range items {
		parts, _ := raw.([]any)
		command := make([]string, 0, len(parts))
		for _, part := range parts {
			if text, ok := part.(string); ok {
				command = append(command, text)
			}
		}
		if len(command) > 0 {
			result = append(result, command)
		}
	}
	return result
}

func (engine *FlowEngine) runVerification(state map[string]any, spec StepSpec) (map[string]any, error) {
	worktree := filepath.Clean(stringValue(stateMap(state, "worktree")["path"]))
	records := []any{}
	for _, command := range commandsFromState(state["verify_commands"]) {
		record := map[string]any{"argv": append([]string(nil), command...)}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(spec.TimeoutSec)*time.Second)
		argv := append([]string{command[0]}, command[1:]...)
		// Execute directly (like Python's shell=False); an "env" wrapper would
		// misinterpret "-"/"=" arguments and absorb the timeout kill.
		process := exec.CommandContext(ctx, argv[0], argv[1:]...)
		process.Dir = worktree
		process.Env = engine.Client.envList()
		process.Stdin = nil
		var stdout, stderr strings.Builder
		process.Stdout = &stdout
		process.Stderr = &stderr
		err := process.Run()
		if ctx.Err() == context.DeadlineExceeded {
			record["returncode"] = nil
			record["stdout"] = boundedVerification(stdout.String())
			record["stderr"] = boundedVerification(stderr.String())
			record["error"] = "timeout"
			record["success"] = false
		} else if err != nil && process.ProcessState == nil {
			record["returncode"] = nil
			record["stdout"] = ""
			record["stderr"] = boundedVerification(err.Error())
			record["error"] = errorTypeName(err)
			record["success"] = false
		} else {
			code := 0
			if process.ProcessState != nil {
				code = process.ProcessState.ExitCode()
			}
			record["returncode"] = code
			record["stdout"] = boundedVerification(stdout.String())
			record["stderr"] = boundedVerification(stderr.String())
			record["success"] = err == nil && code == 0
		}
		cancel()
		records = append(records, record)
	}
	verification := map[string]any{"commands": records, "all_succeeded": len(records) > 0}
	for _, raw := range records {
		if !boolValue(mapValue(raw)["success"], false) {
			verification["all_succeeded"] = false
		}
	}
	artifactPath := filepath.Join(stringValue(state["run_dir"]), "steps", spec.ID, "verification.json")
	if err := atomicWriteJSON(artifactPath, verification); err != nil {
		return nil, err
	}
	stepState := mapValue(stateMap(state, "steps")[spec.ID])
	stepState["verification"] = verification
	stepState["verification_artifact_path"] = artifactPath
	engine.appendEvent(state, "verification_completed", map[string]any{"step_id": spec.ID, "all_succeeded": verification["all_succeeded"]})
	if err := engine.save(state); err != nil {
		return nil, err
	}
	return verification, nil
}

func boundedVerification(value string) string { return truncateText(value, maxVerificationOutput) }

func (engine *FlowEngine) newState(runID string, options StartOptions, runDir string) map[string]any {
	repo := canonicalPath(options.Repo)
	steps := map[string]any{}
	for _, spec := range options.Workflow.Steps {
		stepDir := filepath.Join(runDir, "steps", spec.ID)
		steps[spec.ID] = map[string]any{
			"id": spec.ID, "role": spec.Role, "kind": spec.Kind,
			"depends_on":  cloneStrings(spec.DependsOn),
			"read_policy": spec.ReadPolicy, "write_policy": spec.WritePolicy,
			"timeout_seconds": spec.TimeoutSec, "condition": nullableString(spec.Condition),
			"reuse_agent": nullableString(spec.ReuseAgent), "submit_key": nullableString(spec.SubmitKey),
			"agent_args": cloneStrings(spec.AgentArgs), "contract": spec.Contract.AsMap(),
			"status": "pending", "attempts": 0, "result_path": filepath.Join(stepDir, "result.json"),
			"agent": nil, "started_at": nil, "finished_at": nil, "error": nil, "result": nil,
			"worktree_snapshot_before": nil, "worktree_snapshot_after": nil,
			"actual_changed_files": []string{}, "verification": nil, "verification_artifact_path": nil,
		}
	}
	verify := make([]any, 0, len(options.VerifyCommands))
	for _, command := range options.VerifyCommands {
		verify = append(verify, cloneStrings(command))
	}
	workflowMap := options.Workflow.AsMap()
	return map[string]any{
		"schema_version": int64(1), "run_id": runID, "status": "created", "task": options.Task,
		"repo": repo, "run_dir": runDir, "created_at": utcNow(), "updated_at": utcNow(), "current_step": nil,
		"workflow": workflowMap, "verify_commands": verify,
		"worktree": map[string]any{"enabled": options.UseWorktree, "path": repo, "branch": nullableString(options.Branch), "base": nullableString(options.Base)},
		"resources": map[string]any{
			"workspace_id": nil, "orchestrator_tab_id": nil, "orchestrator_pane_id": nil,
			"tabs": map[string]any{}, "panes": []any{}, "worktree_created": false,
			"worktree_owned": false, "workspace_owned": false, "owned_tabs": []any{}, "owned_panes": []any{},
			"cleanup": map[string]any{"status": "pending", "attempts": 0, "errors": []any{}},
		},
		"orchestrator": map[string]any{"script_path": nullableString(options.ScriptPath), "background": options.Background},
		"steps":        steps, "events": []any{map[string]any{"at": utcNow(), "event": "created"}},
	}
}

func (engine *FlowEngine) requiredID(payload any, kind string) (string, error) {
	value, _ := ExtractID(payload, kind)
	if value == "" {
		return "", flowError("could not extract Herdr %s id from response", kind)
	}
	return value, nil
}

func normalizeResourcePath(value, fallback string) string {
	if value == "" {
		return canonicalPath(fallback)
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(fallback, path)
	}
	return canonicalPath(path)
}

func addOwned(resources map[string]any, key, value string) {
	if value == "" {
		return
	}
	items, _ := resources[key].([]any)
	resources[key] = appendUniqueString(items, value)
}

func addPane(resources map[string]any, pane string) {
	panes, _ := resources["panes"].([]any)
	resources["panes"] = appendUniqueString(panes, pane)
	addOwned(resources, "owned_panes", pane)
}

func (engine *FlowEngine) createWorkspaceTab(cwd, label, workspaceID string) (string, string, string, bool, bool, bool, error) {
	workspaceOwned := false
	if workspaceID == "" {
		payload, err := engine.Client.RunJSON([]string{"workspace", "create", "--cwd", cwd, "--label", label, "--no-focus"}, 60*time.Second)
		if err != nil {
			return "", "", "", false, false, false, err
		}
		workspaceID, err = engine.requiredID(payload, "workspace")
		if err != nil {
			return "", "", "", false, false, false, err
		}
		workspaceOwned = true
	}
	payload, err := engine.Client.RunJSON([]string{"tab", "create", "--workspace", workspaceID, "--cwd", cwd, "--label", label, "--no-focus"}, 60*time.Second)
	if err != nil {
		return workspaceID, "", "", workspaceOwned, false, false, err
	}
	tabID, err := engine.requiredID(payload, "tab")
	if err != nil {
		return workspaceID, "", "", workspaceOwned, false, false, err
	}
	paneID, err := engine.requiredID(payload, "pane")
	if err != nil {
		return workspaceID, tabID, "", workspaceOwned, true, false, err
	}
	return workspaceID, tabID, paneID, workspaceOwned, true, true, nil
}

func (engine *FlowEngine) provision(state map[string]any, options StartOptions) error {
	repo := filepath.Clean(stringValue(state["repo"]))
	resources := stateMap(state, "resources")
	worktree := stateMap(state, "worktree")
	workspaceID := stringValue(resources["workspace_id"])
	tabID := stringValue(resources["orchestrator_tab_id"])
	paneID := stringValue(resources["orchestrator_pane_id"])
	if options.UseWorktree {
		branch := options.Branch
		if branch == "" {
			branch = "panopticon/" + stringValue(state["run_id"])
		}
		arguments := []string{"worktree", "create", "--cwd", repo, "--branch", branch, "--label", "flow-" + lastRunPart(stringValue(state["run_id"])), "--no-focus"}
		if options.Base != "" {
			arguments = append(arguments, "--base", options.Base)
		}
		if options.WorktreePath != "" {
			path, _ := filepath.Abs(options.WorktreePath)
			arguments = append(arguments, "--path", path)
		}
		payload, err := engine.Client.RunJSONAt(arguments, 120*time.Second, repo)
		if err != nil {
			return err
		}
		rawPath := ExtractPath(payload)
		if rawPath == "" {
			return flowError("Herdr worktree create response has no path")
		}
		worktreePath := normalizeResourcePath(rawPath, repo)
		if info, err := os.Stat(worktreePath); err != nil || !info.IsDir() {
			return flowError("Herdr worktree is not a directory: %s", worktreePath)
		}
		if isWithin(worktreePath, repo) {
			return flowError("dedicated worktree must be outside the target repository: %s", worktreePath)
		}
		worktree["path"] = worktreePath
		worktree["branch"] = branch
		resources["worktree_created"] = true
		resources["worktree_owned"] = true
		workspaceID, _ = ExtractID(payload, "workspace")
		tabID, _ = ExtractID(payload, "tab")
		paneID, _ = ExtractID(payload, "pane")
		if workspaceID != "" {
			resources["workspace_id"] = workspaceID
			resources["workspace_owned"] = true
		}
		if tabID != "" {
			resources["orchestrator_tab_id"] = tabID
			addOwned(resources, "owned_tabs", tabID)
		}
		if paneID != "" {
			resources["orchestrator_pane_id"] = paneID
			addPane(resources, paneID)
		}
		if err := engine.save(state); err != nil {
			return err
		}
		if workspaceID == "" || tabID == "" || paneID == "" {
			createdWorkspace, createdTab, createdPane, workspaceOwned, tabOwned, paneOwned, createErr := engine.createWorkspaceTab(worktreePath, "flow-"+lastRunPart(stringValue(state["run_id"])), workspaceID)
			if createdWorkspace != "" {
				workspaceID = createdWorkspace
				resources["workspace_id"] = createdWorkspace
			}
			if createdTab != "" {
				tabID = createdTab
				resources["orchestrator_tab_id"] = createdTab
				addOwned(resources, "owned_tabs", createdTab)
			}
			if createdPane != "" {
				paneID = createdPane
				resources["orchestrator_pane_id"] = createdPane
				addPane(resources, createdPane)
			}
			if workspaceOwned {
				resources["workspace_owned"] = true
			}
			if tabOwned && tabID != "" {
				addOwned(resources, "owned_tabs", tabID)
			}
			if paneOwned && paneID != "" {
				addPane(resources, paneID)
			}
			if saveErr := engine.save(state); saveErr != nil {
				return saveErr
			}
			if createErr != nil {
				return createErr
			}
		}
	} else {
		createdWorkspace, createdTab, createdPane, workspaceOwned, tabOwned, paneOwned, createErr := engine.createWorkspaceTab(repo, "flow-"+lastRunPart(stringValue(state["run_id"])), "")
		workspaceID, tabID, paneID = createdWorkspace, createdTab, createdPane
		if workspaceID != "" {
			resources["workspace_id"] = workspaceID
		}
		if tabID != "" {
			resources["orchestrator_tab_id"] = tabID
			addOwned(resources, "owned_tabs", tabID)
		}
		if paneID != "" {
			resources["orchestrator_pane_id"] = paneID
			addPane(resources, paneID)
		}
		resources["workspace_owned"] = workspaceOwned
		if tabOwned && tabID != "" {
			addOwned(resources, "owned_tabs", tabID)
		}
		if paneOwned && paneID != "" {
			addPane(resources, paneID)
		}
		if saveErr := engine.save(state); saveErr != nil {
			return saveErr
		}
		if createErr != nil {
			return createErr
		}
	}
	resources["workspace_id"] = workspaceID
	resources["orchestrator_tab_id"] = tabID
	resources["orchestrator_pane_id"] = paneID
	if tabID != "" {
		addOwned(resources, "owned_tabs", tabID)
	}
	if paneID != "" {
		addPane(resources, paneID)
	}
	engine.appendEvent(state, "resources_created", map[string]any{"workspace_id": workspaceID, "tab_id": tabID, "pane_id": paneID, "worktree": worktree})
	return engine.save(state)
}

func lastRunPart(runID string) string {
	if len(runID) <= 8 {
		return runID
	}
	return runID[len(runID)-8:]
}

func (engine *FlowEngine) CreateRun(options StartOptions) (map[string]any, error) {
	if err := RequireHerdrEnv(engine.Client.Environ); err != nil {
		return nil, err
	}
	if os.Getenv("PANOPTICON_CHILD") == "1" {
		return nil, herdrError("cannot recursively start Panopticon from a child agent with PANOPTICON_CHILD=1")
	}
	repo, err := filepath.Abs(options.Repo)
	if err != nil {
		return nil, flowError("invalid repo: %v", err)
	}
	if info, err := os.Stat(repo); err != nil || !info.IsDir() {
		return nil, flowError("repo is not a directory: %s", repo)
	}
	options.Repo = repo
	if options.ScriptPath == "" {
		options.ScriptPath = executablePath()
	}
	runID := MakeRunID()
	runDir, err := engine.Store.RunDir(runID)
	if err != nil {
		return nil, err
	}
	state := engine.newState(runID, options, runDir)
	if _, err := engine.Store.Create(runID, state); err != nil {
		return nil, err
	}
	for _, spec := range options.Workflow.Steps {
		if err := os.MkdirAll(filepath.Dir(stringValue(mapValue(stateMap(state, "steps")[spec.ID])["result_path"])), 0o700); err != nil {
			return nil, err
		}
	}
	provisionErr := func() error {
		lock, err := engine.Store.Lock(runID)
		if err != nil {
			return err
		}
		return lock.With(func() error {
			current, err := engine.Store.Load(runID)
			if err != nil {
				return err
			}
			if err := engine.provision(current, options); err != nil {
				return err
			}
			current["status"] = "running"
			engine.appendEvent(current, "provisioned", nil)
			return engine.save(current)
		})
	}()
	if provisionErr != nil {
		_ = engine.markTerminalFailure(runID, "provision_failed", provisionErr)
		engine.cleanupTerminal(runID, true)
		return nil, flowError("failed to initialize run %s: %v", runID, provisionErr)
	}
	if !options.Background {
		return engine.ResumeRun(runID, &options.Workflow)
	}
	state, err = engine.Store.Load(runID)
	if err != nil {
		return nil, err
	}
	if err := engine.launchBackground(runID, state, options); err != nil {
		_ = engine.markTerminalFailure(runID, "orchestrator_launch_failed", err)
		engine.cleanupTerminal(runID, true)
		return nil, flowError("failed to start run %s in the background: %v", runID, err)
	}
	return engine.Store.Load(runID)
}

func (engine *FlowEngine) markTerminalFailure(runID, event string, failure error) error {
	lock, lockErr := engine.Store.Lock(runID)
	if lockErr != nil {
		return lockErr
	}
	return lock.With(func() error {
		state, loadErr := engine.Store.Load(runID)
		if loadErr != nil {
			return loadErr
		}
		state["status"] = "failed"
		state["error"] = errorPayload(failure)
		engine.appendEvent(state, event, map[string]any{"error": state["error"]})
		return engine.save(state)
	})
}

func executablePath() string {
	if path, err := os.Executable(); err == nil {
		return path
	}
	return "panopticon"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func commandText(arguments []string) string {
	quoted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		quoted = append(quoted, shellQuote(argument))
	}
	return strings.Join(quoted, " ")
}

func (engine *FlowEngine) launchBackground(runID string, state map[string]any, options StartOptions) error {
	resources := stateMap(state, "resources")
	paneID := stringValue(resources["orchestrator_pane_id"])
	if paneID == "" {
		return flowError("orchestrator pane not found")
	}
	script := options.ScriptPath
	if script == "" {
		script = executablePath()
	}
	arguments := []string{script, "resume", "--run-id", runID, "--foreground", "--state-root", engine.Store.Root, "--herdr-bin", engine.Client.ResolvedExecutable()}
	result, err := engine.Client.RunRaw([]string{"pane", "run", paneID, commandText(arguments)}, 30*time.Second)
	if err != nil {
		return err
	}
	if result.ReturnCode != 0 {
		return &CommandError{Message: fmt.Sprintf("Herdr pane run failed: %s", strings.TrimSpace(result.Stdout)), Argv: append([]string{engine.Client.Executable}, "pane", "run", paneID, commandText(arguments)), ReturnCode: &result.ReturnCode, Code: "command_failed", Stdout: result.Stdout, Stderr: result.Stderr}
	}
	lock, err := engine.Store.Lock(runID)
	if err != nil {
		return err
	}
	return lock.With(func() error {
		current, err := engine.Store.Load(runID)
		if err != nil {
			return err
		}
		orchestrator := stateMap(current, "orchestrator")
		orchestrator["command"] = arguments
		orchestrator["launched_at"] = utcNow()
		engine.appendEvent(current, "orchestrator_launched", map[string]any{"pane_id": paneID})
		return engine.save(current)
	})
}

func (engine *FlowEngine) loadResult(state map[string]any, spec StepSpec) (map[string]any, error) {
	stepState := mapValue(stateMap(state, "steps")[spec.ID])
	path := stringValue(stepState["result_path"])
	value, err := readJSON(path)
	if err != nil {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			return nil, nil
		}
		return nil, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, nil
	}
	return result, nil
}

func (engine *FlowEngine) conditionMatches(condition string, state map[string]any) (bool, error) {
	if strings.TrimSpace(condition) == "" {
		return true, nil
	}
	expression := strings.TrimSpace(condition)
	var expected *bool
	if strings.Contains(expression, "==") {
		parts := strings.SplitN(expression, "==", 2)
		expression = strings.TrimSpace(parts[0])
		rawExpected := strings.ToLower(strings.TrimSpace(parts[1]))
		if rawExpected != "true" && rawExpected != "false" {
			return false, flowError("condition comparison value must be true or false: %s", condition)
		}
		value := rawExpected == "true"
		expected = &value
	}
	parts := strings.Split(expression, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false, flowError("condition must use step.field format: %s", condition)
	}
	stepState := mapValue(stateMap(state, "steps")[parts[0]])
	result := mapValue(stepState["result"])
	if result == nil {
		return false, nil
	}
	value := result[parts[1]]
	if expected != nil {
		actual, ok := value.(bool)
		return ok && actual == *expected, nil
	}
	return truthy(value), nil
}

// truthy mirrors Python's bool() for JSON-decoded values.
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	case int64:
		return typed != 0
	case int:
		return typed != 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

func (engine *FlowEngine) dependencyState(state map[string]any, spec StepSpec) (bool, string) {
	steps := stateMap(state, "steps")
	for _, dependency := range spec.DependsOn {
		status := stringValue(mapValue(steps[dependency])["status"])
		if status == "failed" || status == "timed_out" || status == "cancelled" || status == "blocked" {
			return false, dependency
		}
		if !stepSuccess[status] {
			return false, ""
		}
	}
	return true, ""
}

func (engine *FlowEngine) agentName(state map[string]any, spec StepSpec) string {
	runID := []rune(stringValue(state["run_id"]))
	if len(runID) > 8 {
		runID = runID[len(runID)-8:]
	}
	var suffix strings.Builder
	for _, character := range strings.ToLower(string(runID)) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' || character == '-' {
			suffix.WriteRune(character)
			continue
		}
		suffix.WriteRune('-')
	}
	name := "pan-" + suffix.String() + "-" + spec.ID
	if runes := []rune(name); len(runes) > 31 {
		return string(runes[:31])
	}
	return name
}

func agentStartArgs(name string, spec StepSpec, paneID string) []string {
	arguments := []string{"agent", "start", name, "--kind", spec.Kind, "--pane", paneID, "--timeout", strconv.Itoa(clampAgentStartTimeoutMS(spec.TimeoutSec * 1000))}
	if len(spec.AgentArgs) > 0 {
		arguments = append(arguments, "--")
		arguments = append(arguments, spec.AgentArgs...)
	}
	return arguments
}

func (engine *FlowEngine) ensureAgent(state map[string]any, spec StepSpec) (string, error) {
	steps := stateMap(state, "steps")
	stepState := mapValue(steps[spec.ID])
	if spec.ReuseAgent != "" {
		sourceState := mapValue(steps[spec.ReuseAgent])
		sourceAgent := mapValue(sourceState["agent"])
		if sourceAgent == nil || stringValue(sourceAgent["target"]) == "" {
			return "", flowError("reuse_agent target has no agent: %s", spec.ReuseAgent)
		}
		copied := cloneMap(sourceAgent)
		copied["reused_from"] = spec.ReuseAgent
		if _, exists := copied["agent_args"]; !exists {
			copied["agent_args"] = cloneStrings(spec.AgentArgs)
		}
		stepState["agent"] = copied
		return stringValue(copied["target"]), nil
	}
	if existing := mapValue(stepState["agent"]); existing != nil && stringValue(existing["target"]) != "" {
		existing["agent_args"] = cloneStrings(spec.AgentArgs)
		if !boolValue(existing["started"], true) {
			paneID := stringValue(existing["pane_id"])
			if _, err := engine.Client.RunJSON(agentStartArgs(stringValue(existing["name"]), spec, paneID), time.Duration(spec.TimeoutSec+30)*time.Second); err != nil {
				return "", err
			}
			existing["started"] = true
			if err := engine.save(state); err != nil {
				return "", err
			}
		}
		return stringValue(existing["target"]), nil
	}
	workspaceID := stringValue(stateMap(state, "resources")["workspace_id"])
	if workspaceID == "" {
		return "", flowError("state has no workspace id")
	}
	worktree := stringValue(stateMap(state, "worktree")["path"])
	payload, err := engine.Client.RunJSON([]string{"tab", "create", "--workspace", workspaceID, "--cwd", worktree, "--label", "flow-" + spec.ID, "--env", "PANOPTICON_CHILD=1", "--no-focus"}, 60*time.Second)
	if err != nil {
		return "", err
	}
	tabID, err := engine.requiredID(payload, "tab")
	if err != nil {
		return "", err
	}
	paneID, err := engine.requiredID(payload, "pane")
	if err != nil {
		resources := stateMap(state, "resources")
		addOwned(resources, "owned_tabs", tabID)
		stateMap(resources, "tabs")[spec.ID] = tabID
		if saveErr := engine.save(state); saveErr != nil {
			failure := fmt.Errorf("agent partial resource state: %w", saveErr)
			return "", failure
		}
		return "", err
	}
	name := engine.agentName(state, spec)
	stepState["agent"] = map[string]any{"name": name, "target": name, "kind": spec.Kind, "tab_id": tabID, "pane_id": paneID, "agent_args": cloneStrings(spec.AgentArgs), "started": false}
	resources := stateMap(state, "resources")
	tabs := stateMap(resources, "tabs")
	tabs[spec.ID] = tabID
	addOwned(resources, "owned_tabs", tabID)
	addPane(resources, paneID)
	if err := engine.save(state); err != nil {
		return "", err
	}
	if _, err := engine.Client.RunJSON(agentStartArgs(name, spec, paneID), time.Duration(spec.TimeoutSec+30)*time.Second); err != nil {
		return "", err
	}
	stepState["agent"].(map[string]any)["started"] = true
	engine.appendEvent(state, "agent_started", map[string]any{"step_id": spec.ID, "target": name, "pane_id": paneID})
	if err := engine.save(state); err != nil {
		return "", err
	}
	return name, nil
}

func cloneMap(source map[string]any) map[string]any {
	result := map[string]any{}
	for key, value := range source {
		result[key] = value
	}
	return result
}

func readTemplate(path string) (string, error) {
	if strings.HasPrefix(path, "embedded:") {
		content, err := embeddedRead(strings.TrimPrefix(path, "embedded:"))
		if err != nil {
			failure := flowError("cannot read prompt template: %s: %v", path, err)
			return "", failure
		}
		text := string(content)
		return text, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		failure := flowError("cannot read prompt template: %s: %v", path, err)
		return "", failure
	}
	text := string(content)
	return text, nil
}

func (engine *FlowEngine) prompt(state map[string]any, spec StepSpec) (string, error) {
	text, err := readTemplate(spec.Template)
	if err != nil {
		return "", fmt.Errorf("prompt template: %w", err)
	}
	stepState := mapValue(stateMap(state, "steps")[spec.ID])
	dependencyResults := map[string]string{}
	for _, dependency := range spec.DependsOn {
		dependencyResults[dependency] = stringValue(mapValue(stateMap(state, "steps")[dependency])["result_path"])
	}
	engineVerification := mapValue(stepState["verification"])
	verificationArtifact := stringValue(stepState["verification_artifact_path"])
	verifyJSON, _ := json.MarshalIndent(state["verify_commands"], "", "  ")
	dependencyJSON, _ := json.MarshalIndent(dependencyResults, "", "  ")
	engineVerificationJSON, _ := json.MarshalIndent(valueOrEmptyMap(engineVerification), "", "  ")
	contractJSON, _ := json.MarshalIndent(resultContract(spec, stringValue(state["run_id"])), "", "  ")
	replacements := map[string]string{
		"TASK": stringValue(state["task"]), "RUN_ID": stringValue(state["run_id"]), "STEP_ID": spec.ID, "ROLE": spec.Role,
		"WORKTREE_PATH": filepath.Clean(stringValue(stateMap(state, "worktree")["path"])), "RUN_DIR": filepath.Clean(stringValue(state["run_dir"])),
		"RESULT_PATH": filepath.Clean(stringValue(stepState["result_path"])), "DEPENDENCY_RESULTS": string(dependencyJSON),
		"VERIFY_COMMANDS": string(verifyJSON), "ENGINE_VERIFICATION": string(engineVerificationJSON), "VERIFICATION_ARTIFACT": verificationArtifact,
		"READ_POLICY": spec.ReadPolicy, "WRITE_POLICY": spec.WritePolicy, "JSON_CONTRACT": string(contractJSON),
	}
	for key, value := range replacements {
		text = strings.ReplaceAll(text, "{{"+key+"}}", value)
	}
	return strings.TrimRight(text, "\r\n") + "\n", nil
}

func valueOrEmptyMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func (engine *FlowEngine) markStep(state map[string]any, spec StepSpec, status string, details map[string]any) {
	stepState := mapValue(stateMap(state, "steps")[spec.ID])
	stepState["status"] = status
	if stepTerminal[status] {
		stepState["finished_at"] = utcNow()
	}
	if stepSuccess[status] {
		stepState["error"] = nil
	}
	if details != nil {
		if value, exists := details["error"]; exists {
			stepState["error"] = value
		}
	}
	engine.appendEvent(state, "step_"+status, mergeMaps(map[string]any{"step_id": spec.ID}, details))
}

func mergeMaps(base, extra map[string]any) map[string]any {
	for key, value := range extra {
		base[key] = value
	}
	return base
}

func (engine *FlowEngine) markCancelled(state map[string]any) error {
	current := state["current_step"]
	for _, raw := range stateMap(state, "steps") {
		step := mapValue(raw)
		if !stepTerminal[stringValue(step["status"])] {
			step["status"] = "cancelled"
			step["finished_at"] = utcNow()
		}
	}
	state["status"] = "cancelled"
	state["current_step"] = current
	engine.appendEvent(state, "cancelled", nil)
	if err := engine.Store.ClearCancelRequest(stringValue(state["run_id"])); err != nil {
		return err
	}
	return engine.save(state)
}

func (engine *FlowEngine) worktreeChangeError(state map[string]any, spec StepSpec, result map[string]any) string {
	stepState := mapValue(stateMap(state, "steps")[spec.ID])
	actual := stringSlice(stepState["actual_changed_files"])
	worktree := filepath.Clean(stringValue(stateMap(state, "worktree")["path"]))
	changed, ok := result["changed_files"].([]any)
	if !ok {
		return fmt.Sprintf("step %s has no worktree after snapshot", spec.ID)
	}
	declared := []string{}
	seen := map[string]bool{}
	for _, raw := range changed {
		item, ok := raw.(string)
		if !ok {
			return "changed_files must be an array of strings"
		}
		if item == "" || filepath.IsAbs(item) {
			return fmt.Sprintf("changed_files is outside the worktree: %q", item)
		}
		candidate := filepath.Clean(filepath.Join(worktree, filepath.FromSlash(item)))
		if !isWithin(candidate, worktree) || candidate == worktree {
			return fmt.Sprintf("changed_files is outside the worktree: %q", item)
		}
		normalized, _ := filepath.Rel(worktree, candidate)
		normalized = filepath.ToSlash(normalized)
		if seen[normalized] {
			return fmt.Sprintf("changed_files contains duplicates: %v", declared)
		}
		seen[normalized] = true
		declared = append(declared, normalized)
	}
	sort.Strings(actual)
	sort.Strings(declared)
	if spec.WritePolicy == "none" {
		if len(actual) > 0 {
			return fmt.Sprintf("read-only step modified the worktree: %v", actual)
		}
		return ""
	}
	if !equalStrings(actual, declared) {
		return fmt.Sprintf("actual worktree diff does not match changed_files: declared=%v, actual=%v", declared, actual)
	}
	return ""
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func artifactAllowedRoots(_ StepSpec, state map[string]any) []string {
	return []string{
		canonicalPath(stringValue(state["run_dir"])),
		canonicalPath(stringValue(stateMap(state, "worktree")["path"])),
	}
}

func validateResult(result map[string]any, spec StepSpec, state map[string]any) (bool, string) {
	contract := spec.Contract
	for _, field := range contract.RequiredFields {
		if _, exists := result[field]; !exists {
			return false, "required field is missing: " + field
		}
	}
	if ParseInt64(result["schema_version"]) != 1 {
		return false, "schema_version must be 1"
	}
	if stringValue(result["run_id"]) != stringValue(state["run_id"]) {
		return false, "result.run_id does not match the current run"
	}
	if stringValue(result["step_id"]) != spec.ID {
		return false, "result.step_id does not match the current step"
	}
	if stringValue(result["role"]) != spec.Role {
		return false, "result.role does not match the workflow role"
	}
	status := stringValue(result["status"])
	if status != "success" && status != "blocked" && status != "failed" {
		return false, "result.status must be success, blocked, or failed"
	}
	if strings.TrimSpace(stringValue(result["summary"])) == "" {
		return false, "result.summary must be a non-empty string"
	}
	artifacts, ok := result["artifacts"].([]any)
	if !ok || len(artifacts) == 0 {
		return false, "result.artifacts must be a non-empty array"
	}
	worktree := stringValue(stateMap(state, "worktree")["path"])
	allowedRoots := artifactAllowedRoots(spec, state)
	for index, raw := range artifacts {
		artifact := mapValue(raw)
		if artifact == nil {
			return false, fmt.Sprintf("artifacts[%d] must be an object", index)
		}
		path := stringValue(artifact["path"])
		if !filepath.IsAbs(path) {
			return false, fmt.Sprintf("artifacts[%d].path must be absolute", index)
		}
		absolute := canonicalPath(path)
		if !isWithin(absolute, allowedRoots...) {
			return false, fmt.Sprintf("artifacts[%d].path is outside the allowed reference roots: %s", index, absolute)
		}
		if info, err := os.Stat(absolute); err != nil || info.IsDir() {
			return false, fmt.Sprintf("artifacts[%d].path does not exist: %s", index, absolute)
		}
		kind := stringValue(artifact["kind"])
		if !containsString(spec.Contract.ArtifactKinds, kind) {
			return false, fmt.Sprintf("artifacts[%d].kind is invalid: %q", index, kind)
		}
		if strings.TrimSpace(stringValue(artifact["description"])) == "" {
			return false, fmt.Sprintf("artifacts[%d].description is required", index)
		}
	}
	changed, ok := result["changed_files"].([]any)
	if !ok {
		return false, "changed_files must be an array of strings"
	}
	for index, raw := range changed {
		item, ok := raw.(string)
		if !ok || item == "" || filepath.IsAbs(item) {
			return false, fmt.Sprintf("changed_files[%d] must be a worktree-relative path: %q", index, item)
		}
		candidate := filepath.Clean(filepath.Join(worktree, filepath.FromSlash(item)))
		if candidate == worktree || !isWithin(candidate, worktree) {
			return false, fmt.Sprintf("changed_files[%d] is outside the worktree: %q", index, item)
		}
	}
	if spec.WritePolicy == "none" && len(changed) > 0 {
		return false, fmt.Sprintf("read-only step reported changed_files: %v", changed)
	}
	tests, ok := result["tests"].([]any)
	if !ok {
		return false, "tests must be an array of strings"
	}
	for _, item := range tests {
		if _, ok := item.(string); !ok {
			return false, "tests must be an array of strings"
		}
	}
	for _, field := range contract.RequiredBooleanFields {
		if _, ok := result[field].(bool); !ok {
			return false, fmt.Sprintf("%s must be a boolean", field)
		}
	}
	for _, field := range contract.RequiredListFields {
		if _, ok := result[field].([]any); !ok {
			return false, fmt.Sprintf("%s must be an array", field)
		}
	}
	if spec.Role == "reviewer" {
		decision := stringValue(result["decision"])
		if decision != "approved" && decision != "needs_fixer" {
			return false, "reviewer decision must be approved or needs_fixer"
		}
		if boolValue(result["needs_fixer"], false) != (decision == "needs_fixer") {
			return false, "reviewer decision and needs_fixer do not match"
		}
	}
	if spec.Role == "verifier" {
		if _, ok := result["verified"].(bool); !ok {
			return false, "verifier verified must be a boolean"
		}
	}
	return true, "ok"
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (engine *FlowEngine) applyResult(state map[string]any, spec StepSpec, result map[string]any) error {
	stepState := mapValue(stateMap(state, "steps")[spec.ID])
	stepState["result"] = result
	status := stringValue(result["status"])
	if status == "blocked" {
		engine.markStep(state, spec, "blocked", map[string]any{"result_summary": result["summary"]})
		state["status"] = "blocked"
	} else if status == "failed" {
		engine.markStep(state, spec, "failed", map[string]any{"result_summary": result["summary"]})
		state["status"] = "failed"
		state["error"] = map[string]any{"message": result["summary"], "type": "agent_result_failed"}
	} else if spec.Role == "verifier" {
		verification := mapValue(stepState["verification"])
		if !boolValue(verification["all_succeeded"], false) {
			error := map[string]any{"message": "engine verification commands did not all succeed", "type": "verification_failed"}
			engine.markStep(state, spec, "failed", map[string]any{"error": error})
			state["status"] = "failed"
			state["error"] = error
		} else if !boolValue(result["verified"], false) {
			error := map[string]any{"message": "verifier returned verified=false", "type": "verification_failed"}
			engine.markStep(state, spec, "failed", map[string]any{"error": error})
			state["status"] = "failed"
			state["error"] = error
		} else {
			engine.markStep(state, spec, "completed", map[string]any{"result_summary": result["summary"]})
		}
	} else {
		engine.markStep(state, spec, "completed", map[string]any{"result_summary": result["summary"]})
	}
	return engine.save(state)
}

func (engine *FlowEngine) applyRecoveredResult(state map[string]any, spec StepSpec) (bool, bool, error) {
	result, err := engine.loadResult(state, spec)
	if err != nil || result == nil {
		return false, false, err
	}
	valid, _ := validateResult(result, spec, state)
	if !valid {
		return false, false, nil
	}
	stepState := mapValue(stateMap(state, "steps")[spec.ID])
	before := mapValue(stepState["worktree_snapshot_before"])
	if before == nil {
		failure := map[string]any{"message": fmt.Sprintf("step %s has no worktree before snapshot", spec.ID), "type": "worktree_snapshot_failed"}
		engine.markStep(state, spec, "failed", map[string]any{"error": failure})
		state["status"] = "failed"
		state["error"] = failure
		return true, true, engine.save(state)
	}
	if _, validBefore := snapshotFiles(before["files"]); !validBefore {
		err := map[string]any{"message": fmt.Sprintf("step %s has no worktree before snapshot", spec.ID), "type": "worktree_snapshot_failed"}
		engine.markStep(state, spec, "failed", map[string]any{"error": err})
		state["status"] = "failed"
		state["error"] = err
		return true, true, engine.save(state)
	}
	if err := engine.snapshotStep(state, spec, "after"); err != nil {
		errorValue := map[string]any{"message": err.Error(), "type": "worktree_snapshot_failed"}
		engine.markStep(state, spec, "failed", map[string]any{"error": errorValue})
		state["status"] = "failed"
		state["error"] = errorValue
		return true, true, engine.save(state)
	}
	if message := engine.worktreeChangeError(state, spec, result); message != "" {
		errorValue := map[string]any{"message": message, "type": "worktree_change_mismatch"}
		engine.markStep(state, spec, "failed", map[string]any{"error": errorValue})
		state["status"] = "failed"
		state["error"] = errorValue
		return true, true, engine.save(state)
	}
	return true, true, engine.applyResult(state, spec, result)
}

func (engine *FlowEngine) recoverRunningStep(state map[string]any, spec StepSpec) (bool, error) {
	found, recovered, err := engine.applyRecoveredResult(state, spec)
	if err != nil || found {
		return recovered, err
	}
	agent := mapValue(mapValue(stateMap(state, "steps")[spec.ID])["agent"])
	if agent == nil || stringValue(agent["target"]) == "" {
		mapValue(stateMap(state, "steps")[spec.ID])["status"] = "pending"
		return false, engine.save(state)
	}
	payload, err := engine.Client.RunJSON([]string{"agent", "wait", stringValue(agent["target"]), "--timeout", strconv.Itoa(spec.TimeoutSec * 1000)}, time.Duration(spec.TimeoutSec+30)*time.Second)
	if err != nil {
		if command, ok := err.(*CommandError); ok && (command.Code == "agent_blocked" || command.Code == "blocked") {
			engine.markStep(state, spec, "blocked", map[string]any{"error": errorPayload(err)})
			state["status"] = "blocked"
			return true, engine.save(state)
		}
		mapValue(stateMap(state, "steps")[spec.ID])["status"] = "pending"
		mapValue(stateMap(state, "steps")[spec.ID])["error"] = errorPayload(err)
		return false, engine.save(state)
	}
	found, recovered, err = engine.applyRecoveredResult(state, spec)
	if err != nil || found {
		return recovered, err
	}
	if ExtractStatus(payload) == "blocked" {
		engine.markStep(state, spec, "blocked", map[string]any{"error": map[string]any{"message": "agent is blocked"}})
		state["status"] = "blocked"
		return true, engine.save(state)
	}
	mapValue(stateMap(state, "steps")[spec.ID])["status"] = "pending"
	return false, engine.save(state)
}

func isTimeoutError(err error) bool {
	command, ok := err.(*CommandError)
	if !ok {
		return false
	}
	return command.Code == "timeout" || command.Code == "timeout_or_start_failed" || strings.Contains(command.Code, "timeout")
}

func (engine *FlowEngine) runRawChecked(args []string, timeout time.Duration, description string) error {
	result, err := engine.Client.RunRaw(args, timeout)
	if err != nil {
		return err
	}
	if result.ReturnCode != 0 {
		code := ExtractErrorCode(ParseJSONOutput(result.Stderr))
		if code == "" {
			code = "command_failed"
		}
		return &CommandError{Message: fmt.Sprintf("%s failed", description), Argv: append([]string{engine.Client.Executable}, args...), ReturnCode: &result.ReturnCode, Code: code, Stdout: result.Stdout, Stderr: result.Stderr}
	}
	return nil
}

func (engine *FlowEngine) waitAgent(target, until string, timeoutMS int) (any, error) {
	args := []string{"agent", "wait", target}
	if until != "" {
		args = append(args, "--until", until)
	}
	args = append(args, "--timeout", strconv.Itoa(timeoutMS))
	payload, err := engine.Client.RunJSON(args, time.Duration(timeoutMS+5000)*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if ExtractStatus(payload) == "blocked" {
		return nil, &CommandError{Message: fmt.Sprintf("agent %s is blocked", target), Argv: append([]string{engine.Client.Executable}, args...), Code: "agent_blocked", Stdout: jsonText(payload)}
	}
	return payload, nil
}

func (engine *FlowEngine) currentAgentStatus(target string) string {
	payload, err := engine.Client.RunJSON([]string{"agent", "get", target}, 30*time.Second)
	if err != nil {
		return ""
	}
	return ExtractStatus(payload)
}

func (engine *FlowEngine) handleCustomSubmissionTimeout(state map[string]any, spec StepSpec, target string, timeoutErr error) error {
	result, _ := engine.loadResult(state, spec)
	status := engine.currentAgentStatus(target)
	if status == "blocked" {
		return &CommandError{Message: fmt.Sprintf("agent %s is blocked", target), Argv: []string{engine.Client.Executable, "agent", "get", target}, Code: "agent_blocked"}
	}
	if result != nil && (status == "idle" || status == "done") {
		return nil
	}
	return timeoutErr
}

func (engine *FlowEngine) waitCustomSubmission(state map[string]any, spec StepSpec, target string) error {
	deadline := time.Now().Add(time.Duration(spec.TimeoutSec) * time.Second)
	workingTimeout := min(spec.TimeoutSec*1000, workingWaitMilliseconds)
	if _, err := engine.waitAgent(target, "working", workingTimeout); err != nil {
		if !isTimeoutError(err) {
			return err
		}
		return engine.handleCustomSubmissionTimeout(state, spec, target, err)
	}

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return engine.handleCustomSubmissionTimeout(state, spec, target, &CommandError{Message: fmt.Sprintf("agent %s timed out while waiting for result.json", target), Argv: []string{engine.Client.Executable, "agent", "wait", target}, Code: "timeout"})
		}
		_, err := engine.waitAgent(target, "", int(remaining/time.Millisecond))
		if err != nil {
			if isTimeoutError(err) {
				return engine.handleCustomSubmissionTimeout(state, spec, target, err)
			}
			return err
		}
		result, _ := engine.loadResult(state, spec)
		if result != nil {
			return nil
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			continue
		}
		_, err = engine.waitAgent(target, "working", int(remaining/time.Millisecond))
		if err != nil {
			if isTimeoutError(err) {
				return engine.handleCustomSubmissionTimeout(state, spec, target, err)
			}
			return err
		}
	}
}

func (engine *FlowEngine) submitPrompt(state map[string]any, spec StepSpec, target, prompt string) error {
	promptArgs := []string{"agent", "prompt", target, prompt}
	if spec.SubmitKey == "" {
		payload, err := engine.Client.RunJSON(append(promptArgs, "--wait", "--timeout", strconv.Itoa(spec.TimeoutSec*1000)), time.Duration(spec.TimeoutSec+30)*time.Second)
		if err != nil {
			return err
		}
		if ExtractStatus(payload) == "blocked" {
			return &CommandError{Message: fmt.Sprintf("agent %s is blocked", target), Argv: append([]string{engine.Client.Executable}, promptArgs...), Code: "agent_blocked", Stdout: jsonText(payload)}
		}
		return nil
	}
	if err := engine.runRawChecked(promptArgs, time.Duration(spec.TimeoutSec+30)*time.Second, "agent prompt"); err != nil {
		return err
	}
	agent := mapValue(mapValue(stateMap(state, "steps")[spec.ID])["agent"])
	paneID := stringValue(agent["pane_id"])
	if paneID == "" {
		return flowError("step %s: no agent pane for submit_key", spec.ID)
	}
	if err := engine.runRawChecked([]string{"pane", "send-keys", paneID, spec.SubmitKey}, 30*time.Second, "pane send-keys"); err != nil {
		return err
	}
	return engine.waitCustomSubmission(state, spec, target)
}

func (engine *FlowEngine) executeStep(state map[string]any, spec StepSpec) error {
	stepState := mapValue(stateMap(state, "steps")[spec.ID])
	resultPath := stringValue(stepState["result_path"])
	_ = os.Remove(resultPath)
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
		return err
	}
	state["current_step"] = spec.ID
	stepState["status"] = "running"
	stepState["attempts"] = intValue(stepState["attempts"]) + 1
	stepState["started_at"] = utcNow()
	stepState["finished_at"] = nil
	stepState["error"] = nil
	stepState["worktree_snapshot_before"] = nil
	stepState["worktree_snapshot_after"] = nil
	stepState["actual_changed_files"] = []any{}
	stepState["verification"] = nil
	stepState["verification_artifact_path"] = nil
	state["status"] = "running"
	engine.appendEvent(state, "step_started", map[string]any{"step_id": spec.ID, "attempt": stepState["attempts"]})
	if err := engine.save(state); err != nil {
		return err
	}
	if err := engine.snapshotStep(state, spec, "before"); err != nil {
		errorValue := map[string]any{"message": err.Error(), "type": "worktree_snapshot_failed"}
		engine.markStep(state, spec, "failed", map[string]any{"error": errorValue})
		state["status"] = "failed"
		state["error"] = errorValue
		return engine.save(state)
	}
	var failureStatus string
	var failure error
	if spec.Role == "verifier" {
		_, failure = engine.runVerification(state, spec)
	}
	var target, prompt string
	if failure == nil {
		target, failure = engine.ensureAgent(state, spec)
	}
	if failure == nil {
		prompt, failure = engine.prompt(state, spec)
	}
	if failure == nil {
		failure = engine.submitPrompt(state, spec, target, prompt)
	}
	if failure != nil {
		if command, ok := failure.(*CommandError); ok && (command.Code == "agent_blocked" || command.Code == "blocked") {
			failureStatus = "blocked"
		} else if isTimeoutError(failure) {
			failureStatus = "timed_out"
		} else {
			failureStatus = "failed"
		}
	}
	if snapshotErr := engine.snapshotStep(state, spec, "after"); snapshotErr != nil {
		failureStatus = "failed"
		failure = snapshotErr
	}
	actual := stringSlice(stepState["actual_changed_files"])
	if spec.WritePolicy == "none" && len(actual) > 0 {
		errorValue := map[string]any{
			"message": fmt.Sprintf("read-only step modified the worktree: %v", actual),
			"type":    "worktree_changed_read_only",
		}
		engine.markStep(state, spec, "failed", map[string]any{"error": errorValue})
		state["status"] = "failed"
		state["error"] = errorValue
		return engine.save(state)
	}
	if failureStatus != "" {
		errorValue := map[string]any{"message": "step failed"}
		if failure != nil {
			errorValue = errorPayload(failure)
		}
		engine.markStep(state, spec, failureStatus, map[string]any{"error": errorValue})
		if failureStatus == "blocked" {
			state["status"] = "blocked"
		} else {
			state["status"] = "failed"
			state["error"] = errorValue
		}
		return engine.save(state)
	}
	result, err := engine.loadResult(state, spec)
	if err != nil {
		return err
	}
	if result == nil {
		errorValue := map[string]any{"message": "result.json was not generated after the agent prompt", "type": "missing_result"}
		engine.markStep(state, spec, "failed", map[string]any{"error": errorValue})
		state["status"] = "failed"
		state["error"] = errorValue
		return engine.save(state)
	}
	valid, reason := validateResult(result, spec, state)
	if !valid {
		errorValue := map[string]any{"message": reason, "type": "invalid_result"}
		engine.markStep(state, spec, "failed", map[string]any{"error": errorValue})
		state["status"] = "failed"
		state["error"] = errorValue
		return engine.save(state)
	}
	if message := engine.worktreeChangeError(state, spec, result); message != "" {
		errorValue := map[string]any{"message": message, "type": "worktree_change_mismatch"}
		engine.markStep(state, spec, "failed", map[string]any{"error": errorValue})
		state["status"] = "failed"
		state["error"] = errorValue
		return engine.save(state)
	}
	return engine.applyResult(state, spec, result)
}

func (engine *FlowEngine) execute(state map[string]any, workflow Workflow) error {
	engine.workflow = &workflow
	for {
		cancelRequested, err := engine.Store.CancelRequested(stringValue(state["run_id"]))
		if err != nil {
			return err
		}
		if cancelRequested {
			return engine.markCancelled(state)
		}
		progress := false
		for _, spec := range workflow.Steps {
			stepState := mapValue(stateMap(state, "steps")[spec.ID])
			status := stringValue(stepState["status"])
			if stepSuccess[status] {
				continue
			}
			if status == "running" {
				recovered, err := engine.recoverRunningStep(state, spec)
				if err != nil {
					return err
				}
				progress = progress || recovered
				if stringValue(state["status"]) == "blocked" || stringValue(state["status"]) == "failed" {
					return nil
				}
				if stepSuccess[stringValue(stepState["status"])] {
					continue
				}
			}
			ready, failedDependency := engine.dependencyState(state, spec)
			if failedDependency != "" {
				errorValue := map[string]any{"message": "dependency step did not succeed: " + failedDependency, "type": "blocked_dependency", "dependency": failedDependency}
				engine.markStep(state, spec, "blocked", map[string]any{"error": errorValue})
				state["status"] = "blocked"
				state["error"] = errorValue
				return engine.save(state)
			}
			if !ready {
				continue
			}
			matches, err := engine.conditionMatches(spec.Condition, state)
			if err != nil {
				return err
			}
			if !matches {
				engine.markStep(state, spec, "skipped", map[string]any{"reason": "condition_false"})
				progress = true
				if err := engine.save(state); err != nil {
					return err
				}
				continue
			}
			if status == "failed" || status == "blocked" || status == "timed_out" {
				errorValue := mapValue(stepState["error"])
				recoverableLateResult := status == "timed_out" || (status == "failed" && (stringValue(errorValue["type"]) == "invalid_result" || stringValue(errorValue["type"]) == "missing_result"))
				if recoverableLateResult {
					found, recovered, err := engine.applyRecoveredResult(state, spec)
					if err != nil {
						return err
					}
					if found {
						progress = progress || recovered
						if stringValue(state["status"]) == "blocked" || stringValue(state["status"]) == "failed" || stringValue(state["status"]) == "cancelled" {
							return nil
						}
						if stepSuccess[stringValue(stepState["status"])] {
							state["error"] = nil
							if err := engine.save(state); err != nil {
								return err
							}
							continue
						}
					}
				}
				stepState["status"] = "pending"
				stepState["error"] = nil
			}
			if err := engine.executeStep(state, spec); err != nil {
				return err
			}
			progress = true
			if stringValue(state["status"]) == "blocked" || stringValue(state["status"]) == "failed" {
				return nil
			}
		}
		allSuccess := true
		for _, raw := range stateMap(state, "steps") {
			if !stepSuccess[stringValue(mapValue(raw)["status"])] {
				allSuccess = false
				break
			}
		}
		if allSuccess {
			state["status"] = "completed"
			state["current_step"] = nil
			state["error"] = nil
			engine.appendEvent(state, "completed", nil)
			return engine.save(state)
		}
		if !progress {
			errorValue := map[string]any{"message": "a step with unresolved dependencies remains", "type": "workflow_deadlock"}
			state["status"] = "blocked"
			state["error"] = errorValue
			engine.appendEvent(state, "blocked", map[string]any{"error": errorValue})
			return engine.save(state)
		}
	}
}

func workflowFromState(state map[string]any) (Workflow, error) {
	workflow := mapValue(state["workflow"])
	path := stringValue(workflow["path"])
	if path == "" {
		return Workflow{}, flowError("state has no workflow")
	}
	storedDigest := stringValue(workflow["digest"])
	if storedDigest == "" {
		return Workflow{}, flowError("state has no workflow TOML digest")
	}
	loaded, err := LoadWorkflow(path)
	if err != nil {
		return Workflow{}, err
	}
	if loaded.Digest != storedDigest {
		return Workflow{}, flowError("workflow TOML changed after run creation (state=%s, current=%s)", storedDigest, loaded.Digest)
	}
	if loaded.Name != stringValue(workflow["name"]) {
		return Workflow{}, flowError("state workflow does not match the requested workflow")
	}
	return loaded, nil
}

func (engine *FlowEngine) ResumeRun(runID string, requested *Workflow) (map[string]any, error) {
	if err := RequireHerdrEnv(engine.Client.Environ); err != nil {
		return nil, err
	}
	lock, err := engine.Store.Lock(runID)
	if err != nil {
		return nil, err
	}
	var final map[string]any
	lockErr := lock.With(func() error {
		state, err := engine.Store.Load(runID)
		if err != nil {
			return err
		}
		workflow, err := workflowFromState(state)
		if err != nil {
			return err
		}
		if requested != nil {
			if requested.Digest != workflow.Digest || requested.Name != workflow.Name {
				return flowError("requested workflow digest/name does not match state")
			}
			workflow = *requested
		}
		status := stringValue(state["status"])
		if status == "completed" || status == "cancelled" {
			final = state
			return nil
		}
		if status == "failed" {
			if err := engine.prepareFailedRunForResume(state, workflow); err != nil {
				return err
			}
		}
		state["status"] = "running"
		engine.appendEvent(state, "resumed", nil)
		if err := engine.save(state); err != nil {
			return err
		}
		engine.workflow = &workflow
		if err := engine.execute(state, workflow); err != nil {
			errorValue := errorPayload(err)
			state["status"] = "failed"
			state["error"] = errorValue
			engine.appendEvent(state, "engine_failed", map[string]any{"error": errorValue})
			if saveErr := engine.save(state); saveErr != nil {
				return saveErr
			}
		}
		final = state
		return nil
	})
	if lockErr != nil {
		return nil, lockErr
	}
	if final == nil {
		final, err = engine.Store.Load(runID)
		if err != nil {
			return nil, err
		}
	}
	if runTerminal[stringValue(final["status"])] {
		engine.cleanupTerminal(runID, true)
		final, _ = engine.Store.Load(runID)
	}
	return final, nil
}

func cleanupStatus(state map[string]any) string {
	return stringValue(cleanupResourceStatus(state)["status"])
}

func cleanupCompleted(state map[string]any) bool {
	return cleanupStatus(state) == "completed"
}

func (engine *FlowEngine) prepareFailedRunForResume(state map[string]any, workflow Workflow) error {
	status := cleanupStatus(state)
	if status == "partial" {
		if err := engine.performCleanup(state, true); err != nil {
			return err
		}
		status = cleanupStatus(state)
	}
	if status == "partial" {
		return flowError("cannot resume because previous resource cleanup is incomplete")
	}
	if status == "completed" {
		return engine.reprovisionForResume(state, workflow)
	}
	return nil
}

func (engine *FlowEngine) reprovisionForResume(state map[string]any, workflow Workflow) error {
	resources := stateMap(state, "resources")
	resources["workspace_id"] = nil
	resources["orchestrator_tab_id"] = nil
	resources["orchestrator_pane_id"] = nil
	resources["tabs"] = map[string]any{}
	resources["panes"] = []any{}
	resources["owned_tabs"] = []any{}
	resources["owned_panes"] = []any{}
	resources["workspace_owned"] = false
	resources["worktree_created"] = false
	resources["worktree_owned"] = false
	resources["cleanup"] = map[string]any{"status": "pending", "attempts": 0, "errors": []any{}}
	for _, raw := range stateMap(state, "steps") {
		mapValue(raw)["agent"] = nil
	}
	worktree := stateMap(state, "worktree")
	options := StartOptions{Repo: stringValue(state["repo"]), Workflow: workflow, Task: stringValue(state["task"]), VerifyCommands: commandsFromState(state["verify_commands"]), UseWorktree: boolValue(worktree["enabled"], true), WorktreePath: stringValue(worktree["path"]), Branch: stringValue(worktree["branch"]), Base: stringValue(worktree["base"]), Background: false, ScriptPath: stringValue(stateMap(state, "orchestrator")["script_path"])}
	engine.workflow = &workflow
	return engine.provision(state, options)
}

func (engine *FlowEngine) CancelRun(runID string) (map[string]any, error) {
	if err := RequireHerdrEnv(engine.Client.Environ); err != nil {
		return nil, err
	}
	state, err := engine.Store.Load(runID)
	if err != nil {
		return nil, err
	}
	if runTerminal[stringValue(state["status"])] {
		return state, nil
	}
	lock, err := engine.Store.Lock(runID)
	if err != nil {
		return nil, err
	}
	lockErr := lock.With(func() error {
		current, err := engine.Store.Load(runID)
		if err != nil {
			return err
		}
		if runTerminal[stringValue(current["status"])] {
			state = current
			return nil
		}
		if err := engine.markCancelled(current); err != nil {
			return err
		}
		state = current
		return nil
	})
	if lockErr != nil {
		if _, ok := lockErr.(*StateError); ok {
			_, requestErr := engine.Store.RequestCancel(runID)
			if requestErr != nil {
				return nil, requestErr
			}
			state["status"] = "cancel_requested"
			state["message"] = "wrote a cancellation request to the running process"
			return state, nil
		}
		return nil, lockErr
	}
	if runTerminal[stringValue(state["status"])] {
		engine.cleanupTerminal(runID, true)
		state, _ = engine.Store.Load(runID)
	}
	return state, nil
}

func cleanupResourceStatus(state map[string]any) map[string]any {
	resources := stateMap(state, "resources")
	if resources == nil {
		resources = map[string]any{}
		state["resources"] = resources
	}
	cleanup := mapValue(resources["cleanup"])
	if cleanup == nil {
		cleanup = map[string]any{"status": "pending", "attempts": 0, "errors": []any{}}
		resources["cleanup"] = cleanup
	}
	return cleanup
}

func (engine *FlowEngine) shouldDeferCleanup(state map[string]any) bool {
	if os.Getenv("PANOPTICON_CLEANUP_WORKER") == "1" {
		return false
	}
	worktree := stateMap(state, "worktree")
	resources := stateMap(state, "resources")
	if !boolValue(resources["worktree_created"], false) {
		return stringValue(resources["orchestrator_pane_id"]) != "" && os.Getenv("HERDR_PANE_ID") == stringValue(resources["orchestrator_pane_id"])
	}
	cwd, err := os.Getwd()
	if err == nil && isWithin(cwd, stringValue(worktree["path"])) {
		return true
	}
	return os.Getenv("HERDR_PANE_ID") == stringValue(resources["orchestrator_pane_id"])
}

func (engine *FlowEngine) cleanupTerminal(runID string, removeWorktree bool) {
	state, err := engine.Store.Load(runID)
	if err != nil || !runTerminal[stringValue(state["status"])] {
		return
	}
	if stringValue(cleanupResourceStatus(state)["status"]) == "completed" {
		return
	}
	if engine.shouldDeferCleanup(state) {
		engine.scheduleCleanup(runID, removeWorktree)
		return
	}
	lock, err := engine.Store.Lock(runID)
	if err != nil {
		return
	}
	_ = lock.With(func() error {
		current, err := engine.Store.Load(runID)
		if err != nil {
			return err
		}
		if !runTerminal[stringValue(current["status"])] {
			return nil
		}
		return engine.performCleanup(current, removeWorktree)
	})
}

func (engine *FlowEngine) scheduleCleanup(runID string, removeWorktree bool) {
	lock, err := engine.Store.Lock(runID)
	if err != nil {
		return
	}
	_ = lock.With(func() error {
		current, err := engine.Store.Load(runID)
		if err != nil {
			return err
		}
		cleanup := cleanupResourceStatus(current)
		cleanup["status"] = "scheduled"
		cleanup["scheduled_at"] = utcNow()
		cleanup["remove_worktree"] = removeWorktree
		engine.appendEvent(current, "cleanup_scheduled", map[string]any{"remove_worktree": removeWorktree})
		return engine.save(current)
	})
	current, err := engine.Store.Load(runID)
	if err != nil {
		return
	}
	script := stringValue(stateMap(current, "orchestrator")["script_path"])
	if script == "" {
		script = executablePath()
	}
	repo := stringValue(current["repo"])
	arguments := []string{script, "cleanup-resources", "--run-id", runID, "--state-root", engine.Store.Root, "--herdr-bin", engine.Client.ResolvedExecutable()}
	command := exec.Command("env", append([]string{script}, arguments[1:]...)...)
	command.Dir = repo
	command.Env = engine.Client.envList()
	command.Env = append(command.Env, "PANOPTICON_CLEANUP_WORKER=1")
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		_ = engine.recordCleanupError(runID, err)
		return
	}
	if err := command.Process.Release(); err != nil {
		_ = engine.recordCleanupError(runID, err)
	}
}

func (engine *FlowEngine) recordCleanupError(runID string, err error) error {
	lock, lockErr := engine.Store.Lock(runID)
	if lockErr != nil {
		return lockErr
	}
	return lock.With(func() error {
		state, errLoad := engine.Store.Load(runID)
		if errLoad != nil {
			return errLoad
		}
		cleanup := cleanupResourceStatus(state)
		cleanup["status"] = "partial"
		cleanup["last_error"] = errorPayload(err)
		return engine.save(state)
	})
}

func (engine *FlowEngine) performCleanup(state map[string]any, removeWorktree bool) error {
	cleanup := cleanupResourceStatus(state)
	cleanup["attempts"] = intValue(cleanup["attempts"]) + 1
	errorsList := []any{}
	resources := stateMap(state, "resources")
	repo := stringValue(state["repo"])
	if removeWorktree && (boolValue(resources["worktree_owned"], false) || boolValue(resources["worktree_created"], false)) {
		removed, err := engine.removeOwnedWorktree(state)
		if err != nil {
			errorsList = append(errorsList, errorPayload(err))
		} else {
			cleanup["worktree_removed"] = removed
		}
	}
	ownedTabs := stringSlice(resources["owned_tabs"])
	ownedPanes := stringSlice(resources["owned_panes"])
	for _, tab := range ownedTabs {
		if err := engine.closeHerdrResource([]string{"tab", "close", tab}, repo); err != nil && !alreadyGone(err) {
			errorsList = append(errorsList, errorPayload(err))
		}
	}
	for _, pane := range ownedPanes {
		if err := engine.closeHerdrResource([]string{"pane", "close", pane}, repo); err != nil && !alreadyGone(err) {
			errorsList = append(errorsList, errorPayload(err))
		}
	}
	if boolValue(resources["workspace_owned"], false) {
		workspaceID := stringValue(resources["workspace_id"])
		if workspaceID != "" {
			if err := engine.closeHerdrResource([]string{"workspace", "close", workspaceID}, repo); err != nil && !alreadyGone(err) {
				errorsList = append(errorsList, errorPayload(err))
			}
		}
	}
	cleanup["errors"] = errorsList
	cleanup["finished_at"] = utcNow()
	cleanup["resources_closed"] = len(errorsList) == 0
	if len(errorsList) == 0 {
		cleanup["status"] = "completed"
	} else {
		cleanup["status"] = "partial"
	}
	engine.appendEvent(state, "cleanup_finished", map[string]any{"status": cleanup["status"], "worktree_removed": cleanup["worktree_removed"]})
	return engine.save(state)
}

func alreadyGone(err error) bool {
	command, ok := err.(*CommandError)
	if !ok {
		return false
	}
	code := strings.ToLower(command.Code)
	return strings.Contains(code, "not_found") || strings.Contains(code, "notfound") || strings.Contains(code, "already_closed") || strings.Contains(code, "closed")
}

func (engine *FlowEngine) closeHerdrResource(args []string, directory string) error {
	result, err := engine.Client.RunRawAt(args, 60*time.Second, directory)
	if err != nil {
		return err
	}
	if result.ReturnCode != 0 {
		code := ExtractErrorCode(ParseJSONOutput(result.Stderr))
		if code == "" {
			code = "command_failed"
		}
		return &CommandError{Message: fmt.Sprintf("Herdr resource cleanup failed: %s", displayCommand(args)), Argv: append([]string{engine.Client.Executable}, args...), ReturnCode: &result.ReturnCode, Code: code, Stdout: result.Stdout, Stderr: result.Stderr}
	}
	return nil
}

func (engine *FlowEngine) removeOwnedWorktree(state map[string]any) (bool, error) {
	worktree := stateMap(state, "worktree")
	path := filepath.Clean(stringValue(worktree["path"]))
	repo := filepath.Clean(stringValue(state["repo"]))
	if path == "" || path == repo || isWithin(path, repo) {
		return false, nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return true, nil
	}
	payload, err := engine.Client.RunJSONAt([]string{"worktree", "list", "--cwd", repo}, 120*time.Second, repo)
	if err != nil {
		return false, err
	}
	workspaceID, err := currentWorktreeWorkspaceID(payload, repo, path, stringValue(worktree["branch"]))
	if err != nil {
		return false, err
	}
	if err := engine.closeHerdrResource([]string{"worktree", "remove", "--workspace", workspaceID, "--force"}, repo); err != nil && !alreadyGone(err) {
		return false, err
	}
	if _, err := os.Stat(path); err == nil {
		// Herdr normally removes the checkout. Only remove a path after a
		// unique path+branch match proved that this run owns it.
		if removeErr := os.RemoveAll(path); removeErr != nil {
			return false, removeErr
		}
	}
	return true, nil
}

func currentWorktreeWorkspaceID(payload any, repo, savedPath, savedBranch string) (string, error) {
	matches := []string{}
	pathKeys := map[string]bool{"path": true, "worktree_path": true, "worktreepath": true, "checkout_path": true, "checkoutpath": true, "working_directory": true, "workingdirectory": true}
	branchKeys := map[string]bool{"branch": true, "branch_name": true, "branchname": true}
	workspaceKeys := map[string]bool{"workspace": true, "workspace_id": true, "workspaceid": true}
	var visit func(any, string)
	visit = func(node any, inherited string) {
		table, ok := node.(map[string]any)
		if ok {
			workspaceID := inherited
			for key, value := range table {
				if workspaceKeys[normalizeKey(key)] {
					if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
						workspaceID = strings.TrimSpace(text)
					} else if extracted, _ := ExtractID(value, "workspace"); extracted != "" {
						workspaceID = extracted
					}
				}
			}
			pathValue, branchValue := "", ""
			for key, value := range table {
				normalized := normalizeKey(key)
				if pathKeys[normalized] {
					pathValue = stringValue(value)
				}
				if branchKeys[normalized] {
					branchValue = stringValue(value)
				}
			}
			candidate := pathValue
			if candidate != "" && !filepath.IsAbs(candidate) {
				candidate = filepath.Join(repo, candidate)
			}
			candidate, _ = filepath.Abs(candidate)
			savedAbsolute, _ := filepath.Abs(savedPath)
			if pathValue != "" && branchValue != "" && filepath.Clean(candidate) == filepath.Clean(savedAbsolute) && branchValue == savedBranch && workspaceID != "" {
				matches = append(matches, workspaceID)
			}
			for _, value := range table {
				visit(value, workspaceID)
			}
			return
		}
		if list, ok := node.([]any); ok {
			for _, value := range list {
				visit(value, inherited)
			}
		}
	}
	visit(payload, "")
	if len(matches) != 1 {
		return "", flowError("could not find a unique workspace id for the saved path+branch in the current worktree list")
	}
	return matches[0], nil
}

func (engine *FlowEngine) Cleanup(runID string, allRuns bool, olderThanHours float64, removeWorktree bool) ([]map[string]any, error) {
	if err := RequireHerdrEnv(engine.Client.Environ); err != nil {
		return nil, err
	}
	if runID == "" && !allRuns {
		return nil, flowError("cleanup requires --run-id or --all")
	}
	cutoff := float64(time.Now().Unix()) - olderThanHours*3600
	candidateIDs := []string{}
	if runID != "" {
		candidateIDs = append(candidateIDs, runID)
	} else {
		entries, err := engine.Store.List()
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			status := stringValue(entry["status"])
			if runTerminal[status] && nowEpoch(entry["updated_at"]) <= cutoff {
				candidateIDs = append(candidateIDs, stringValue(entry["run_id"]))
			}
		}
	}
	cleaned := []map[string]any{}
	for _, currentID := range candidateIDs {
		lock, err := engine.Store.Lock(currentID)
		if err != nil {
			return nil, err
		}
		var removed bool
		var cleanupPartial bool
		var worktreeRemoved bool
		lockErr := lock.With(func() error {
			state, err := engine.Store.Load(currentID)
			if err != nil {
				return err
			}
			if !runTerminal[stringValue(state["status"])] {
				return flowError("cannot clean up active run: %s", currentID)
			}
			if runID == "" && nowEpoch(state["updated_at"]) > cutoff {
				return nil
			}
			if err := engine.performCleanup(state, removeWorktree); err != nil {
				return err
			}
			cleanup := cleanupResourceStatus(state)
			cleanupPartial = stringValue(cleanup["status"]) == "partial"
			worktreeRemoved = boolValue(cleanup["worktree_removed"], false)
			removed = true
			return nil
		})
		if lockErr != nil {
			return nil, lockErr
		}
		if !removed {
			continue
		}
		if cleanupPartial {
			cleaned = append(cleaned, map[string]any{"run_id": currentID, "status": "partial", "worktree_removed": worktreeRemoved})
			continue
		}
		if err := engine.Store.Remove(currentID); err != nil {
			return nil, err
		}
		cleaned = append(cleaned, map[string]any{"run_id": currentID, "status": "removed", "worktree_removed": worktreeRemoved})
	}
	return cleaned, nil
}

func (engine *FlowEngine) CleanupResources(runID string, removeWorktree bool) (map[string]any, error) {
	if err := RequireHerdrEnv(engine.Client.Environ); err != nil {
		return nil, err
	}
	lock, err := engine.Store.Lock(runID)
	if err != nil {
		return nil, err
	}
	var state map[string]any
	if err := lock.With(func() error {
		var err error
		state, err = engine.Store.Load(runID)
		if err != nil {
			return err
		}
		if !runTerminal[stringValue(state["status"])] {
			return flowError("cannot clean up active run: %s", runID)
		}
		return engine.performCleanup(state, removeWorktree)
	}); err != nil {
		return nil, err
	}
	return state, nil
}

func (engine *FlowEngine) dryRunPayload(repo string, workflow Workflow, task string, verify [][]string, useWorktree bool) map[string]any {
	steps := []any{}
	for _, spec := range workflow.Steps {
		steps = append(steps, map[string]any{"step_id": spec.ID, "role": spec.Role, "depends_on": spec.DependsOn, "kind": spec.Kind, "read_policy": spec.ReadPolicy, "write_policy": spec.WritePolicy, "timeout_seconds": spec.TimeoutSec, "reuse_agent": nullableString(spec.ReuseAgent), "agent_args": spec.AgentArgs, "condition": nullableString(spec.Condition), "result_contract": spec.Contract.AsMap()})
	}
	verifyJSON := []any{}
	for _, command := range verify {
		verifyJSON = append(verifyJSON, command)
	}
	absolute, _ := filepath.Abs(repo)
	return map[string]any{"dry_run": true, "repo": absolute, "task": task, "workflow": workflow.AsMap(), "verify_commands": verifyJSON, "worktree": map[string]any{"mode": map[bool]string{true: "herdr worktree create", false: "existing repo (explicit --no-worktree)"}[useWorktree], "destructive_integration": false}, "execution": steps}
}

func DryRunPayload(repo string, workflow Workflow, task string, verify [][]string, useWorktree bool) map[string]any {
	return (&FlowEngine{}).dryRunPayload(repo, workflow, task, verify, useWorktree)
}
