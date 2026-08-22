package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	runIDPattern  = regexp.MustCompile(`- run_id: ` + "`" + `([^` + "`" + `]+)` + "`")
	stepIDPattern = regexp.MustCompile(`- step_id: ` + "`" + `([^` + "`" + `]+)` + "`")
	rolePattern   = regexp.MustCompile(`- role: ` + "`" + `([^` + "`" + `]+)` + "`")
)

func envValue(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func flagValue(args []string, flag string) (string, bool) {
	for index, value := range args {
		if value != flag {
			continue
		}
		if index+1 >= len(args) {
			return "", false
		}
		return args[index+1], true
	}
	return "", false
}

func marshalJSON(value any, indent bool) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if indent {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func emit(value any) int {
	encoded, err := marshalJSON(value, false)
	if err != nil {
		return 1
	}
	_, _ = os.Stdout.Write(append(encoded, '\n'))
	return 0
}

func emitError(code, message string) int {
	if message == "" {
		message = code
	}
	encoded, err := marshalJSON(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	}, false)
	if err != nil {
		return 1
	}
	_, _ = os.Stderr.Write(append(encoded, '\n'))
	return 1
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".fake-tmp")
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func logArguments(args []string) {
	path := os.Getenv("FAKE_HERDR_LOG")
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	encoded, err := marshalJSON(args, false)
	if err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(encoded, '\n'))
}

func promptValue(prompt, prefix string) (string, bool) {
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix)), true
		}
	}
	return "", false
}

func promptJSON(prompt, marker string) (any, bool) {
	start := strings.Index(prompt, marker)
	if start < 0 {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(prompt[start+len(marker):]))
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

func asMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func mapValue(value map[string]any, key string) map[string]any {
	if nested := asMap(value[key]); nested != nil {
		return nested
	}
	nested := map[string]any{}
	value[key] = nested
	return nested
}

func stringValue(value any, fallback string) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fallback
}

func intValue(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	}
	return fallback
}

func trimBackticks(value string) string {
	return strings.Trim(strings.TrimSpace(value), "`")
}

func parseContractKind(prompt string) string {
	marker := strings.Index(prompt, "## JSON contract")
	if marker < 0 {
		return "report"
	}
	start := strings.Index(prompt[marker:], "{")
	if start < 0 {
		return "report"
	}
	value, ok := decodeJSONPrefix(prompt[marker+start:])
	if !ok {
		return "report"
	}
	contract := asMap(value)
	if contract == nil {
		return "report"
	}
	artifacts, ok := contract["artifacts"].([]any)
	if !ok || len(artifacts) == 0 {
		return "report"
	}
	kind := ""
	if artifact := asMap(artifacts[0]); artifact != nil {
		kind = stringValue(artifact["kind"], "")
	}
	if kind == "" {
		return "report"
	}
	kind = strings.TrimPrefix(kind, "one of: ")
	if comma := strings.IndexByte(kind, ','); comma >= 0 {
		kind = kind[:comma]
	}
	return strings.TrimSpace(kind)
}

func decodeJSONPrefix(text string) (any, bool) {
	decoder := json.NewDecoder(strings.NewReader(text))
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

func decodeJSON(text string) (any, bool) {
	value, ok := decodeJSONPrefix(text)
	if !ok {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	var ignored any
	if err := decoder.Decode(&ignored); err != nil {
		return nil, false
	}
	if err := decoder.Decode(&ignored); err != io.EOF {
		return nil, false
	}
	return value, true
}

func parseConfiguredList(value string) []any {
	if value == "" {
		return []any{}
	}
	parsed, ok := decodeJSON(value)
	if !ok {
		return []any{value}
	}
	if list, ok := parsed.([]any); ok {
		return list
	}
	return []any{value}
}

func safeRelative(path string) bool {
	if path == "" || filepath.IsAbs(path) {
		return false
	}
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return false
		}
	}
	cleaned := filepath.Clean(path)
	return cleaned != "." && cleaned != ".."
}

func absolutePath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(absolute)
}

func writeText(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o600)
}

func writeResult(path string, payload map[string]any) error {
	encoded, err := marshalJSON(payload, true)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(encoded, '\n'))
}

func statePath() string {
	if configured := os.Getenv("FAKE_HERDR_STATE"); configured != "" {
		return configured
	}
	if logPath := os.Getenv("FAKE_HERDR_LOG"); logPath != "" {
		return strings.TrimSuffix(logPath, filepath.Ext(logPath)) + ".state.json"
	}
	return absolutePath(".fake-herdr-state.json")
}

func readState() map[string]any {
	content, err := os.ReadFile(statePath())
	if err != nil {
		return map[string]any{}
	}
	var state map[string]any
	if err := json.Unmarshal(content, &state); err != nil || state == nil {
		return map[string]any{}
	}
	return state
}

func writeState(state map[string]any) error {
	encoded, err := marshalJSON(state, false)
	if err != nil {
		return err
	}
	return writeAtomic(statePath(), encoded)
}

func promptResultPath(prompt string) string {
	value, ok := promptValue(prompt, "RESULT_PATH=")
	if !ok {
		return ""
	}
	return value
}

func promptWorktree(prompt string) string {
	if value, ok := promptValue(prompt, "- worktree: "); ok && value != "" {
		return trimBackticks(value)
	}
	if value, ok := promptValue(prompt, "- Dedicated worktree only: "); ok && value != "" {
		return trimBackticks(value)
	}
	return ""
}

func controllerResult(prompt string, runID, stepID string) (map[string]any, error) {
	controllerWorktree, _ := promptValue(prompt, "- Worktree: ")
	controllerWorktree = trimBackticks(controllerWorktree)
	if changedFile := os.Getenv("FAKE_HERDR_CONTROLLER_CHANGED_FILE"); changedFile != "" && controllerWorktree != "" && safeRelative(changedFile) {
		if err := writeText(filepath.Join(controllerWorktree, changedFile), "fake controller change\n"); err != nil {
			return nil, err
		}
	}

	contextValue, _ := promptJSON(prompt, "CONTROL_CONTEXT=")
	eligibleValue, _ := promptJSON(prompt, "ELIGIBLE_NEXT_STEPS=")
	allowedValue, _ := promptJSON(prompt, "ALLOWED_ACTIONS=")
	context := asMap(contextValue)
	completion := asMap(context["completion"])

	action, actionSet := os.LookupEnv("FAKE_HERDR_CONTROLLER_ACTION")
	if status := stringValue(completion["status"], ""); status == "failed" || status == "blocked" || status == "timed_out" {
		if failedAction, ok := os.LookupEnv("FAKE_HERDR_CONTROLLER_ACTION_FAILED"); ok {
			action = failedAction
			actionSet = true
		}
	}
	if !actionSet || action == "" {
		if allowed, ok := allowedValue.([]any); ok && len(allowed) > 0 {
			action = stringValue(allowed[0], "fail")
		} else {
			action = "fail"
		}
	}

	var nextStep any
	if configured, ok := os.LookupEnv("FAKE_HERDR_CONTROLLER_NEXT_STEP"); ok {
		nextStep = configured
	} else if action == "retry" {
		nextStep = completion["step_id"]
	} else if action == "continue" {
		if eligible, ok := eligibleValue.([]any); ok && len(eligible) > 0 {
			nextStep = eligible[0]
		}
	}

	reason := envValue("FAKE_HERDR_CONTROLLER_REASON", "fake "+action)
	return map[string]any{
		"schema_version":   1,
		"run_id":           runID,
		"step_id":          "controller",
		"role":             "controller",
		"observed_step":    stringValue(completion["step_id"], "__start__"),
		"observed_attempt": intValue(completion["attempt"], 0),
		"action":           action,
		"next_step":        nextStep,
		"reason":           reason,
		"user_summary":     "fake controller " + action,
	}, nil
}

func agentResult(prompt, runID, stepID, role string) (map[string]any, error) {
	resultPath := promptResultPath(prompt)
	if resultPath == "" {
		return nil, fmt.Errorf("fake Herdr cannot parse RESULT_PATH")
	}
	worktree := promptWorktree(prompt)
	artifact := filepath.Join(filepath.Dir(resultPath), "fake-"+stepID+".txt")
	if err := writeText(artifact, "fake artifact\n"); err != nil {
		return nil, err
	}

	roleKey := strings.ToUpper(role)
	changedValue := ""
	if value, ok := os.LookupEnv("FAKE_HERDR_CHANGED_FILES_" + roleKey); ok {
		changedValue = value
	} else {
		changedValue = os.Getenv("FAKE_HERDR_CHANGED_FILES")
	}
	changedFiles := parseConfiguredList(changedValue)

	actualValue := ""
	if value, ok := os.LookupEnv("FAKE_HERDR_ACTUAL_CHANGED_FILES_" + roleKey); ok {
		actualValue = value
	} else if value, ok := os.LookupEnv("FAKE_HERDR_ACTUAL_CHANGED_FILES"); ok {
		actualValue = value
	} else {
		actualValue = changedValue
	}
	actualFiles := parseConfiguredList(actualValue)
	if worktree != "" {
		for _, value := range actualFiles {
			changedFile, ok := value.(string)
			if !ok || !safeRelative(changedFile) {
				continue
			}
			if err := writeText(filepath.Join(worktree, changedFile), "fake change\n"); err != nil {
				return nil, err
			}
		}
	}

	status := envValue("FAKE_HERDR_MODE", "success")
	if status != "success" && status != "blocked" && status != "failed" {
		status = "success"
	}
	verified := true
	if role == "verifier" {
		if override, ok := os.LookupEnv("FAKE_HERDR_VERIFIED"); ok {
			verified = strings.ToLower(override) == "true"
		} else {
			engineVerification, _ := promptJSON(prompt, "ENGINE_VERIFICATION=")
			verification := asMap(engineVerification)
			value, ok := verification["all_succeeded"].(bool)
			verified = ok && value
		}
	}
	artifactKind := os.Getenv("FAKE_HERDR_ARTIFACT_KIND")
	if artifactKind == "" {
		artifactKind = parseContractKind(prompt)
	}
	return map[string]any{
		"schema_version": 1,
		"run_id":         runID,
		"step_id":        stepID,
		"role":           role,
		"status":         status,
		"summary":        "fake " + status,
		"artifacts": []any{map[string]any{
			"path":        absolutePath(artifact),
			"kind":        artifactKind,
			"description": "fake artifact",
		}},
		"changed_files": changedFiles,
		"tests":         []any{"fake test"},
		"findings":      []any{},
		"decision":      "approved",
		"needs_fixer":   false,
		"verified":      verified,
		"verification":  []any{"fake verification"},
	}, nil
}

func promptBoundaries(prompt string) (string, string, string, error) {
	runIDMatch := runIDPattern.FindStringSubmatch(prompt)
	stepIDMatch := stepIDPattern.FindStringSubmatch(prompt)
	roleMatch := rolePattern.FindStringSubmatch(prompt)
	if len(runIDMatch) < 2 || len(stepIDMatch) < 2 || len(roleMatch) < 2 {
		return "", "", "", fmt.Errorf("fake Herdr cannot parse the prompt's fixed boundaries")
	}
	return runIDMatch[1], stepIDMatch[1], roleMatch[1], nil
}

func resultFromPrompt(prompt string) (map[string]any, error) {
	runID, stepID, role, err := promptBoundaries(prompt)
	if err != nil {
		return nil, err
	}
	if role == "controller" {
		return controllerResult(prompt, runID, stepID)
	}
	return agentResult(prompt, runID, stepID, role)
}

func pendingPrompt(state map[string]any, paneID string) (string, string, bool, bool) {
	pending := asMap(state["pending_prompts"])
	for target, value := range pending {
		entry := asMap(value)
		if entry != nil && stringValue(entry["pane_id"], "") == paneID {
			prompt, ok := entry["prompt"].(string)
			return target, prompt, true, ok
		}
	}
	return "", "", false, false
}

func setAgentState(state map[string]any, target, lifecycle string, prompt any) {
	agents := mapValue(state, "agents")
	entry := map[string]any{"state": lifecycle}
	if prompt != nil {
		entry["prompt"] = prompt
	}
	agents[target] = entry
}

func pendingPromptCommand(args []string) int {
	paneID := ""
	if len(args) > 2 {
		paneID = args[2]
	}
	submitKey := ""
	if len(args) > 3 {
		submitKey = args[3]
	}
	state := readState()
	target, prompt, found, valid := pendingPrompt(state, paneID)
	if !found {
		return emitError("no_pending_prompt", "")
	}
	if !valid {
		return emitError("invalid_pending_prompt", "")
	}
	expectedKey := envValue("FAKE_HERDR_EXPECTED_SUBMIT_KEY", envValue("FAKE_HERDR_SUBMIT_KEY", "ctrl+enter"))
	if submitKey != expectedKey {
		return emitError("invalid_key", "expected "+expectedKey)
	}
	pending := mapValue(state, "pending_prompts")
	delete(pending, target)

	mode := envValue("FAKE_HERDR_MODE", "success")
	if mode == "timeout" {
		setAgentState(state, target, "timeout", nil)
		if err := writeState(state); err != nil {
			return emitError("state_write_failed", err.Error())
		}
		return 0
	}
	resultPath := promptResultPath(prompt)
	if resultPath == "" {
		return emitError("missing_result_path", "")
	}
	if mode == "delayed-working-before-result" || mode == "transient-idle-before-result" {
		lifecycle := "working"
		if mode == "delayed-working-before-result" {
			lifecycle = "delayed_working"
		}
		setAgentState(state, target, lifecycle, prompt)
		if err := writeState(state); err != nil {
			return emitError("state_write_failed", err.Error())
		}
		return 0
	}
	result, err := resultFromPrompt(prompt)
	if err != nil {
		return emitError("result_failed", err.Error())
	}
	if err := writeResult(resultPath, result); err != nil {
		return emitError("result_write_failed", err.Error())
	}
	lifecycle := "working"
	if mode == "fast-success" || mode == "blocked-timeout" {
		lifecycle = "settled"
	}
	setAgentState(state, target, lifecycle, nil)
	if err := writeState(state); err != nil {
		return emitError("state_write_failed", err.Error())
	}
	return 0
}

func agentGetCommand(args []string) int {
	target := ""
	if len(args) > 2 {
		target = args[2]
	}
	state := readState()
	entry := asMap(asMap(state["agents"])[target])
	lifecycle := stringValue(entry["state"], "")
	mode := envValue("FAKE_HERDR_MODE", "success")
	status := "unknown"
	switch {
	case mode == "blocked" || mode == "blocked-timeout":
		status = "blocked"
	case mode == "fast-success" || lifecycle == "settled":
		status = "idle"
	case mode == "result-working-timeout" || lifecycle == "working" || lifecycle == "working_observed":
		status = "working"
	}
	return emit(map[string]any{
		"id": "cli:agent:get",
		"result": map[string]any{
			"agent": map[string]any{"agent_status": status},
			"type":  "agent_info",
		},
	})
}

func agentWaitCommand(args []string) int {
	target := ""
	if len(args) > 2 {
		target = args[2]
	}
	until, hasUntil := flagValue(args, "--until")
	state := readState()
	agents := asMap(state["agents"])
	entry := asMap(agents[target])
	lifecycle := stringValue(entry["state"], "")
	mode := envValue("FAKE_HERDR_MODE", "success")
	if mode == "timeout" || lifecycle == "timeout" || (mode == "result-working-timeout" && !hasUntil) {
		return emitError("timeout", "")
	}
	if hasUntil && until == "working" {
		switch {
		case mode == "delayed-working-before-result" && lifecycle == "delayed_working":
			setAgentState(state, target, "working", entry["prompt"])
			if err := writeState(state); err != nil {
				return emitError("state_write_failed", err.Error())
			}
			return emitError("timeout", "")
		case lifecycle == "working":
			setAgentState(state, target, "working_observed", entry["prompt"])
			if err := writeState(state); err != nil {
				return emitError("state_write_failed", err.Error())
			}
			return emit(map[string]any{"status": "working"})
		case mode == "transient-idle-before-result" && lifecycle == "transient_idle":
			setAgentState(state, target, "working_observed_final", entry["prompt"])
			if err := writeState(state); err != nil {
				return emitError("state_write_failed", err.Error())
			}
			return emit(map[string]any{"status": "working"})
		default:
			return emitError("timeout", "")
		}
	}

	if mode == "delayed-working-before-result" && lifecycle == "working_observed" {
		return finishPendingAgent(state, entry, target)
	}
	if mode == "transient-idle-before-result" && lifecycle == "working_observed" {
		setAgentState(state, target, "transient_idle", entry["prompt"])
		if err := writeState(state); err != nil {
			return emitError("state_write_failed", err.Error())
		}
		return emit(map[string]any{"status": "idle"})
	}
	if mode == "transient-idle-before-result" && lifecycle == "working_observed_final" {
		return finishPendingAgent(state, entry, target)
	}
	if lifecycle == "working" || lifecycle == "working_observed" || lifecycle == "settled" {
		status := "idle"
		if mode == "blocked" {
			status = "blocked"
		}
		setAgentState(state, target, "settled", nil)
		if err := writeState(state); err != nil {
			return emitError("state_write_failed", err.Error())
		}
		return emit(map[string]any{"status": status})
	}
	return emit(map[string]any{"status": "completed"})
}

func finishPendingAgent(state map[string]any, entry map[string]any, target string) int {
	prompt, ok := entry["prompt"].(string)
	if !ok {
		return emitError("missing_result_path", "")
	}
	resultPath := promptResultPath(prompt)
	if resultPath == "" {
		return emitError("missing_result_path", "")
	}
	result, err := resultFromPrompt(prompt)
	if err != nil {
		return emitError("result_failed", err.Error())
	}
	if err := writeResult(resultPath, result); err != nil {
		return emitError("result_write_failed", err.Error())
	}
	setAgentState(state, target, "settled", nil)
	if err := writeState(state); err != nil {
		return emitError("state_write_failed", err.Error())
	}
	return emit(map[string]any{"status": "idle"})
}

func agentPromptCommand(args []string) int {
	target, prompt := "", ""
	if len(args) > 2 {
		target = args[2]
	}
	if len(args) > 3 {
		prompt = args[3]
	}
	mode := envValue("FAKE_HERDR_MODE", "success")
	if mode == "command-blocked" {
		return emitError("agent_blocked", "")
	}
	if mode == "command-failed" {
		return emitError("agent_failed", "")
	}
	if promptResultPath(prompt) == "" {
		return emitError("missing_result_path", "")
	}
	if _, _, _, err := promptBoundaries(prompt); err != nil {
		return emitError("result_failed", err.Error())
	}
	if !contains(args, "--wait") {
		state := readState()
		mapValue(state, "pending_prompts")[target] = map[string]any{
			"pane_id": "w-fake-agent-pane",
			"prompt":  prompt,
		}
		setAgentState(state, target, "awaiting_submit", nil)
		if err := writeState(state); err != nil {
			return emitError("state_write_failed", err.Error())
		}
		return 0
	}
	result, err := resultFromPrompt(prompt)
	if err != nil {
		return emitError("result_failed", err.Error())
	}
	if err := writeResult(promptResultPath(prompt), result); err != nil {
		return emitError("result_write_failed", err.Error())
	}
	return emit(map[string]any{"status": "completed"})
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func runFakeGit() int {
	switch os.Getenv("FAKE_GIT_MODE") {
	case "copy":
		_, _ = os.Stdout.Write([]byte("C  copy name.txt\x00source.txt\x00"))
		return 0
	case "directory":
		_, _ = os.Stdout.Write([]byte(" M submodule\x00"))
		return 0
	case "failure":
		_, _ = os.Stderr.Write([]byte("fatal: index unavailable\n"))
		return 1
	default:
		return 0
	}
}

func runVerificationFixture(args []string) (int, bool) {
	if len(args) != 1 {
		return 0, false
	}
	switch args[0] {
	case "--verify-success":
		_, _ = fmt.Fprintln(os.Stdout, "ok")
		return 0, true
	case "--verify-failure":
		_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("o", 6000))
		_, _ = fmt.Fprintln(os.Stderr, strings.Repeat("e", 6000))
		return 3, true
	default:
		return 0, false
	}
}

func runHerdr(args []string) int {
	logArguments(args)
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(os.Stdout, "herdr 0.8.2")
		return 0
	}
	if len(args) == 0 {
		return emit(map[string]any{"status": "ok"})
	}
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	if len(args) > 1 {
		command += " " + args[1]
	}
	switch command {
	case "workspace create":
		return emit(map[string]any{
			"workspace_id": "w-fake-workspace",
			"tab_id":       "w-fake-tab",
			"pane_id":      "w-fake-pane",
		})
	case "tab create":
		return emit(map[string]any{
			"tab_id":  "w-fake-agent-tab",
			"pane_id": "w-fake-agent-pane",
		})
	case "worktree create":
		pathValue, _ := flagValue(args, "--path")
		if pathValue == "" {
			pathValue = os.Getenv("FAKE_HERDR_WORKTREE")
		}
		if pathValue == "" {
			return emit(map[string]any{"error": map[string]any{"code": "missing_worktree_path"}})
		}
		path := absolutePath(pathValue)
		if err := os.MkdirAll(path, 0o755); err != nil {
			return emitError("worktree_create_failed", err.Error())
		}
		return emit(map[string]any{
			"worktree_id":   "wt-fake-worktree",
			"worktree_path": path,
			"workspace_id":  "w-fake-workspace",
			"tab_id":        "w-fake-tab",
			"pane_id":       "w-fake-pane",
		})
	case "worktree list":
		if configured := os.Getenv("FAKE_HERDR_WORKTREE_LIST"); configured != "" {
			value, ok := decodeJSON(configured)
			if !ok {
				return emit(map[string]any{"error": map[string]any{"code": "invalid_worktree_list"}})
			}
			return emit(value)
		}
		return emit(map[string]any{
			"worktrees": []any{map[string]any{
				"workspace_id":  envValue("FAKE_HERDR_CURRENT_WORKSPACE", "w-current-workspace"),
				"worktree_path": os.Getenv("FAKE_HERDR_WORKTREE"),
				"branch":        envValue("FAKE_HERDR_BRANCH", "panopticon/current"),
			}},
		})
	case "worktree remove":
		return emit(map[string]any{"status": "removed"})
	case "pane run":
		return 0
	case "pane send-keys":
		return pendingPromptCommand(args)
	case "agent start":
		return emit(map[string]any{"status": "started"})
	case "agent get":
		return agentGetCommand(args)
	case "agent wait":
		return agentWaitCommand(args)
	case "agent prompt":
		return agentPromptCommand(args)
	default:
		return emit(map[string]any{"status": "ok"})
	}
}

func main() {
	args := os.Args[1:]
	base := filepath.Base(os.Args[0])
	if base == "git" || base == "git.exe" {
		os.Exit(runFakeGit())
	}
	if code, ok := runVerificationFixture(args); ok {
		os.Exit(code)
	}
	os.Exit(runHerdr(args))
}
