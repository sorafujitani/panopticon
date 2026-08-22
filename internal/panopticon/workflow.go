package panopticon

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

//go:embed assets/workflows/* assets/prompts/*
var embeddedAssets embed.FS

type WorkflowError struct{ Message string }

func (e *WorkflowError) Error() string { return e.Message }

func workflowError(format string, args ...any) error {
	return &WorkflowError{Message: fmt.Sprintf(format, args...)}
}

type Contract struct {
	RequiredFields        []string
	ArtifactKinds         []string
	RequiredBooleanFields []string
	RequiredListFields    []string
}

func cloneStrings(values []string) []string {
	result := make([]string, len(values))
	copy(result, values)
	return result
}

func (c Contract) AsMap() map[string]any {
	return map[string]any{
		"schema_version":          int64(1),
		"required_fields":         cloneStrings(c.RequiredFields),
		"artifact_kinds":          cloneStrings(c.ArtifactKinds),
		"required_boolean_fields": cloneStrings(c.RequiredBooleanFields),
		"required_list_fields":    cloneStrings(c.RequiredListFields),
	}
}

type StepSpec struct {
	ID          string
	Role        string
	Kind        string
	DependsOn   []string
	ReadPolicy  string
	WritePolicy string
	TimeoutSec  int
	Template    string
	Contract    Contract
	Condition   string
	ReuseAgent  string
	SubmitKey   string
	Model       string
	Effort      string
	AgentArgs   []string
}

type ControllerSpec struct {
	Role       string
	Kind       string
	ReadPolicy string
	TimeoutSec int
	Template   string
	SubmitKey  string
	AgentArgs  []string
	MaxRetries int
}

func (c ControllerSpec) StepSpec() StepSpec {
	return StepSpec{
		ID: "controller", Role: c.Role, Kind: c.Kind, ReadPolicy: c.ReadPolicy,
		WritePolicy: "none", TimeoutSec: c.TimeoutSec, Template: c.Template,
		SubmitKey: c.SubmitKey, AgentArgs: cloneStrings(c.AgentArgs),
	}
}

type Workflow struct {
	Name          string
	Version       int
	Path          string
	Controller    *ControllerSpec
	Steps         []StepSpec
	DefaultVerify [][]string
	Digest        string
}

func (w Workflow) StepMap() map[string]StepSpec {
	result := make(map[string]StepSpec, len(w.Steps))
	for _, step := range w.Steps {
		result[step.ID] = step
	}
	return result
}

func (w Workflow) AsMap() map[string]any {
	steps := make([]any, 0, len(w.Steps))
	for _, step := range w.Steps {
		steps = append(steps, map[string]any{
			"id":              step.ID,
			"role":            step.Role,
			"kind":            step.Kind,
			"depends_on":      cloneStrings(step.DependsOn),
			"read_policy":     step.ReadPolicy,
			"write_policy":    step.WritePolicy,
			"timeout_seconds": int64(step.TimeoutSec),
			"template":        step.Template,
			"condition":       nullableString(step.Condition),
			"reuse_agent":     nullableString(step.ReuseAgent),
			"submit_key":      nullableString(step.SubmitKey),
			"model":           nullableString(step.Model),
			"effort":          nullableString(step.Effort),
			"agent_args":      cloneStrings(step.AgentArgs),
			"contract":        step.Contract.AsMap(),
		})
	}
	verify := make([]any, 0, len(w.DefaultVerify))
	for _, command := range w.DefaultVerify {
		verify = append(verify, cloneStrings(command))
	}
	var controller any
	if w.Controller != nil {
		controller = map[string]any{
			"role": w.Controller.Role, "kind": w.Controller.Kind,
			"read_policy": w.Controller.ReadPolicy, "write_policy": "none",
			"timeout_seconds": int64(w.Controller.TimeoutSec), "template": w.Controller.Template,
			"submit_key": nullableString(w.Controller.SubmitKey), "agent_args": cloneStrings(w.Controller.AgentArgs),
			"max_retries": int64(w.Controller.MaxRetries), "result_contract": controllerResultContractMap(),
		}
	}
	return map[string]any{
		"name":           w.Name,
		"version":        int64(w.Version),
		"path":           w.Path,
		"digest":         w.Digest,
		"controller":     controller,
		"default_verify": verify,
		"steps":          steps,
	}
}

func controllerResultContractMap() map[string]any {
	return map[string]any{
		"schema_version":  int64(1),
		"required_fields": []string{"schema_version", "run_id", "step_id", "role", "observed_step", "observed_attempt", "action", "next_step", "reason", "user_summary"},
		"actions":         []string{"continue", "retry", "block", "fail"},
	}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

var stepIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)

var supportedPiEfforts = map[string]bool{
	"off": true, "minimal": true, "low": true, "medium": true,
	"high": true, "xhigh": true, "max": true,
}

var supportedAgentKinds = map[string]bool{
	"pi": true, "claude": true, "codex": true, "gemini": true, "cursor": true,
	"devin": true, "agy": true, "cline": true, "omp": true, "mastracode": true,
	"opencode": true, "copilot": true, "kimi": true, "kiro": true, "droid": true,
	"amp": true, "grok": true, "hermes": true, "kilo": true, "qodercli": true,
	"qwen": true, "maki": true,
}

func embeddedRead(name string) ([]byte, error) {
	name = strings.TrimPrefix(name, "embedded:")
	content, err := embeddedAssets.ReadFile(filepath.ToSlash(filepath.Join("assets", name)))
	if err != nil {
		return nil, fmt.Errorf("workflow operation: %w", err)
	}
	return content, nil
}

func readWorkflowFile(path string) ([]byte, error) {
	if strings.HasPrefix(path, "embedded:") {
		content, err := embeddedRead(strings.TrimPrefix(path, "embedded:"))
		if err != nil {
			return nil, workflowError("workflow not found: %s", path)
		}
		return content, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, workflowError("workflow not found: %s", path)
		}
		return nil, workflowError("cannot read workflow: %s: %v", path, err)
	}
	return content, nil
}

func asString(value any, label string) (string, error) {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", workflowError("%s must be a non-empty string", label)
	}
	trimmed := strings.TrimSpace(text)
	return trimmed, nil
}

func asStringList(value any, label string) ([]string, error) {
	if value == nil {
		return []string{}, nil
	}
	items, ok := value.([]any)
	if !ok {
		if stringItems, ok := value.([]string); ok {
			result := make([]string, 0, len(stringItems))
			for _, item := range stringItems {
				if strings.TrimSpace(item) != "" {
					result = append(result, item)
				}
			}
			return result, nil
		}
		return nil, workflowError("%s must be an array of strings", label)
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, workflowError("%s must be an array of strings", label)
		}
		if strings.TrimSpace(text) != "" {
			result = append(result, text)
		}
	}
	return result, nil
}

func asAgentArgs(value any, label string) ([]string, error) {
	if value == nil {
		return []string{}, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, workflowError("%s must be a non-empty array of strings", label)
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, workflowError("%s must be a non-empty array of strings", label)
		}
		result = append(result, text)
	}
	return result, nil
}

func parseCommandList(value any, label string) ([][]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, workflowError("%s must be an array of command arrays", label)
	}
	commands := make([][]string, 0, len(items))
	for index, raw := range items {
		parts, ok := raw.([]any)
		if !ok || len(parts) == 0 {
			return nil, workflowError("%s[%d] must be a non-empty array of strings", label, index)
		}
		command := make([]string, 0, len(parts))
		for _, token := range parts {
			text, ok := token.(string)
			if !ok || text == "" {
				return nil, workflowError("%s[%d] must be a non-empty array of strings", label, index)
			}
			command = append(command, text)
		}
		commands = append(commands, command)
	}
	return commands, nil
}

func resolveTemplate(workflowPath, value string) (string, error) {
	text, err := asString(value, "steps[].template")
	if err != nil {
		return "", fmt.Errorf("template path: %w", err)
	}
	if filepath.IsAbs(text) {
		resolved, err := filepath.Abs(text)
		if err != nil {
			return "", workflowError("prompt template not found: %s", text)
		}
		if _, err := os.Stat(resolved); err != nil {
			return "", workflowError("prompt template not found: %s", resolved)
		}
		cleaned := filepath.Clean(resolved)
		return cleaned, nil
	}
	if strings.HasPrefix(workflowPath, "embedded:") {
		candidate := filepath.ToSlash(filepath.Join(filepath.Dir(strings.TrimPrefix(workflowPath, "embedded:")), text))
		candidate = strings.TrimPrefix(filepath.Clean(candidate), "../")
		if strings.HasPrefix(candidate, "prompts/") {
			candidate = strings.TrimPrefix(candidate, "")
		}
		if _, err := embeddedRead(candidate); err != nil {
			return "", workflowError("prompt template not found: embedded:%s", candidate)
		}
		return "embedded:" + candidate, nil
	}
	resolved, err := filepath.Abs(filepath.Join(filepath.Dir(workflowPath), text))
	if err != nil {
		return "", fmt.Errorf("template relative path: %w", err)
	}
	if _, err := os.Stat(resolved); err != nil {
		return "", workflowError("prompt template not found: %s", resolved)
	}
	cleaned := filepath.Clean(resolved)
	return cleaned, nil
}

func parseContract(value any, stepID string) (Contract, error) {
	table, ok := value.(map[string]any)
	if !ok {
		return Contract{}, workflowError("step %s: contract must be a table", stepID)
	}
	required, err := asStringList(table["required_fields"], fmt.Sprintf("step %s.contract.required_fields", stepID))
	if err != nil {
		return Contract{}, fmt.Errorf("workflow contract: %w", err)
	}
	if len(required) == 0 {
		return Contract{}, workflowError("step %s: contract.required_fields is required", stepID)
	}
	artifacts, err := asStringList(table["artifact_kinds"], fmt.Sprintf("step %s.contract.artifact_kinds", stepID))
	if err != nil {
		return Contract{}, fmt.Errorf("workflow contract: %w", err)
	}
	if len(artifacts) == 0 {
		return Contract{}, workflowError("step %s: artifact_kinds must contain at least one item", stepID)
	}
	booleans, err := asStringList(table["required_boolean_fields"], fmt.Sprintf("step %s.contract.required_boolean_fields", stepID))
	if err != nil {
		return Contract{}, fmt.Errorf("workflow contract: %w", err)
	}
	lists, err := asStringList(table["required_list_fields"], fmt.Sprintf("step %s.contract.required_list_fields", stepID))
	if err != nil {
		return Contract{}, fmt.Errorf("workflow contract: %w", err)
	}
	return Contract{RequiredFields: required, ArtifactKinds: artifacts, RequiredBooleanFields: booleans, RequiredListFields: lists}, nil
}

func parseStep(value any, workflowPath string) (StepSpec, error) {
	table, ok := value.(map[string]any)
	if !ok {
		return StepSpec{}, workflowError("steps must be an array of tables")
	}
	id, err := asString(table["id"], "steps[].id")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	if !stepIDPattern.MatchString(id) {
		return StepSpec{}, workflowError("invalid step id: %q", id)
	}
	role, err := asString(table["role"], "step "+id+".role")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	kind, err := asString(table["kind"], "step "+id+".kind")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	kind = strings.ToLower(kind)
	if !supportedAgentKinds[kind] {
		return StepSpec{}, workflowError("step %s: unsupported agent kind: %s", id, kind)
	}
	depends, err := asStringList(table["depends_on"], "step "+id+".depends_on")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	for _, dependency := range depends {
		if dependency == id {
			return StepSpec{}, workflowError("step %s: cannot depend on itself", id)
		}
	}
	readPolicy, err := asString(table["read_policy"], "step "+id+".read_policy")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	writePolicy, err := asString(table["write_policy"], "step "+id+".write_policy")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	if !map[string]bool{"none": true, "worktree": true, "repo-and-dependencies": true}[readPolicy] {
		return StepSpec{}, workflowError("step %s: invalid read_policy: %s", id, readPolicy)
	}
	if !map[string]bool{"none": true, "worktree": true, "repo-and-dependencies": true}[writePolicy] {
		return StepSpec{}, workflowError("step %s: invalid write_policy: %s", id, writePolicy)
	}
	timeout, err := asInt(table["timeout_seconds"])
	if err != nil || timeout < 1 || timeout > 86400 {
		return StepSpec{}, workflowError("step %s: timeout_seconds must be an integer from 1 to 86400", id)
	}
	templateValue, ok := table["template"].(string)
	if !ok {
		return StepSpec{}, workflowError("steps[].template must be a non-empty string")
	}
	template, err := resolveTemplate(workflowPath, templateValue)
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	contract, err := parseContract(table["contract"], id)
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	optionalString := func(key string) (string, error) {
		value, exists := table[key]
		if !exists || value == nil || value == "" {
			return "", nil
		}
		parsed, parseErr := asString(value, fmt.Sprintf("step %s.%s", id, key))
		if parseErr != nil {
			return "", fmt.Errorf("optional step field: %w", parseErr)
		}
		return parsed, nil
	}
	condition, err := optionalString("condition")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	reuseAgent, err := optionalString("reuse_agent")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	submitKey, err := optionalString("submit_key")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	model, err := optionalString("model")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	effort, err := optionalString("effort")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	if effort != "" {
		effort = strings.ToLower(effort)
		if !supportedPiEfforts[effort] {
			return StepSpec{}, workflowError("step %s: invalid effort: %s", id, effort)
		}
	}
	if (model != "" || effort != "") && kind != "pi" {
		return StepSpec{}, workflowError("step %s: model and effort are only supported for kind pi", id)
	}
	if reuseAgent != "" && (model != "" || effort != "") {
		return StepSpec{}, workflowError("step %s: model and effort cannot be set with reuse_agent; the reused agent keeps its original settings", id)
	}
	agentArgs, err := asAgentArgs(table["agent_args"], "step "+id+".agent_args")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	if model != "" && containsAgentOption(agentArgs, "--model") {
		return StepSpec{}, workflowError("step %s: model conflicts with --model in agent_args", id)
	}
	if effort != "" && containsAgentOption(agentArgs, "--thinking") {
		return StepSpec{}, workflowError("step %s: effort conflicts with --thinking in agent_args", id)
	}
	return StepSpec{ID: id, Role: role, Kind: kind, DependsOn: depends, ReadPolicy: readPolicy, WritePolicy: writePolicy, TimeoutSec: timeout, Template: template, Contract: contract, Condition: condition, ReuseAgent: reuseAgent, SubmitKey: submitKey, Model: model, Effort: effort, AgentArgs: agentArgs}, nil
}

func containsAgentOption(arguments []string, option string) bool {
	for _, argument := range arguments {
		if argument == option || strings.HasPrefix(argument, option+"=") {
			return true
		}
	}
	return false
}

func parseController(value any, workflowPath string) (*ControllerSpec, error) {
	if value == nil {
		return nil, nil
	}
	table, ok := value.(map[string]any)
	if !ok {
		return nil, workflowError("controller must be a table")
	}
	if writePolicy, exists := table["write_policy"]; exists && writePolicy != "none" {
		return nil, workflowError("controller.write_policy must be none")
	}
	synthetic := cloneMap(table)
	synthetic["id"] = "controller"
	synthetic["depends_on"] = []any{}
	synthetic["write_policy"] = "none"
	synthetic["contract"] = map[string]any{
		"required_fields": []any{"schema_version"},
		"artifact_kinds":  []any{"report"},
	}
	step, err := parseStep(synthetic, workflowPath)
	if err != nil {
		return nil, fmt.Errorf("workflow controller: %w", err)
	}
	maxRetries := 1
	if raw, exists := table["max_retries"]; exists {
		maxRetries, err = asInt(raw)
		if err != nil || maxRetries < 0 || maxRetries > 10 {
			return nil, workflowError("controller.max_retries must be an integer from 0 to 10")
		}
	}
	return &ControllerSpec{
		Role: step.Role, Kind: step.Kind, ReadPolicy: step.ReadPolicy,
		TimeoutSec: step.TimeoutSec, Template: step.Template, SubmitKey: step.SubmitKey,
		AgentArgs: step.AgentArgs, MaxRetries: maxRetries,
	}, nil
}

func asInt(value any) (int, error) {
	switch typed := value.(type) {
	case int64:
		if typed > int64(^uint(0)>>1) || typed < -int64(^uint(0)>>1)-1 {
			return 0, fmt.Errorf("integer out of range")
		}
		return int(typed), nil
	case int:
		return typed, nil
	case string:
		parsed, err := strconv.Atoi(typed)
		return parsed, fmt.Errorf("workflow integer: %w", err)
	default:
		return 0, fmt.Errorf("not an integer")
	}
}

func validateGraph(steps []StepSpec) error {
	byID := make(map[string]StepSpec, len(steps))
	for _, step := range steps {
		if _, exists := byID[step.ID]; exists {
			return workflowError("duplicate step id")
		}
		byID[step.ID] = step
	}
	for _, step := range steps {
		for _, dependency := range step.DependsOn {
			if _, exists := byID[dependency]; !exists {
				return workflowError("step %s: undefined dependency: %s", step.ID, dependency)
			}
		}
		if step.ReuseAgent != "" {
			if _, exists := byID[step.ReuseAgent]; !exists {
				return workflowError("step %s: reuse_agent target not found: %s", step.ID, step.ReuseAgent)
			}
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return workflowError("workflow contains a cycle: %s", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dependency := range byID[id].DependsOn {
			if err := visit(dependency); err != nil {
				return fmt.Errorf("workflow operation: %w", err)
			}
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	for _, step := range steps {
		if err := visit(step.ID); err != nil {
			return fmt.Errorf("workflow operation: %w", err)
		}
	}
	return nil
}

func workflowData(raw map[string]any) (map[string]any, error) {
	nested, exists := raw["workflow"]
	if !exists || nested == nil {
		return raw, nil
	}
	table, ok := nested.(map[string]any)
	if !ok {
		return nil, workflowError("[workflow] must be a table")
	}
	merged := make(map[string]any, len(raw)+len(table))
	for key, value := range raw {
		merged[key] = value
	}
	for key, value := range table {
		merged[key] = value
	}
	return merged, nil
}

func LoadWorkflow(path string) (Workflow, error) {
	if !strings.HasPrefix(path, "embedded:") {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return Workflow{}, workflowError("workflow not found: %s", path)
		}
		path = filepath.Clean(absolute)
	}
	content, err := readWorkflowFile(path)
	if err != nil {
		return Workflow{}, fmt.Errorf("workflow load: %w", err)
	}
	raw, err := ParseTOML(string(content))
	if err != nil {
		return Workflow{}, workflowError("invalid workflow TOML (%s): %v", path, err)
	}
	data, err := workflowData(raw)
	if err != nil {
		return Workflow{}, fmt.Errorf("workflow load: %w", err)
	}
	version := 1
	if value, exists := data["version"]; exists {
		parsedVersion, parseErr := asInt(value)
		if parseErr != nil {
			return Workflow{}, workflowError("unsupported workflow version: %v", value)
		}
		version = parsedVersion
	}
	if version != 1 {
		return Workflow{}, workflowError("unsupported workflow version: %d", version)
	}
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if value, exists := data["name"]; exists {
		name, err = asString(value, "workflow.name")
		if err != nil {
			return Workflow{}, fmt.Errorf("workflow load: %w", err)
		}
	}
	rawSteps, ok := data["steps"].([]any)
	if !ok || len(rawSteps) == 0 {
		return Workflow{}, workflowError("workflow.steps must contain at least one table")
	}
	controller, err := parseController(data["controller"], path)
	if err != nil {
		return Workflow{}, fmt.Errorf("workflow load: %w", err)
	}
	steps := make([]StepSpec, 0, len(rawSteps))
	for _, rawStep := range rawSteps {
		step, err := parseStep(rawStep, path)
		if err != nil {
			return Workflow{}, fmt.Errorf("workflow load: %w", err)
		}
		if controller != nil && step.ID == "controller" {
			return Workflow{}, workflowError("step id controller is reserved when using the control plane")
		}
		steps = append(steps, step)
	}
	if err := validateGraph(steps); err != nil {
		return Workflow{}, fmt.Errorf("workflow load: %w", err)
	}
	defaultVerify, err := parseCommandList(data["default_verify"], "default_verify")
	if err != nil {
		return Workflow{}, fmt.Errorf("workflow load: %w", err)
	}
	if len(defaultVerify) == 0 {
		return Workflow{}, workflowError("default_verify must contain at least one command")
	}
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	return Workflow{Name: name, Version: version, Path: path, Controller: controller, Steps: steps, DefaultVerify: defaultVerify, Digest: digest}, nil
}

func LoadRepoConfig(repo string) (map[string]any, error) {
	return loadConfigFile(filepath.Join(repo, ".panopticon.toml"), "repo .panopticon.toml")
}

func userConfigDir() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("PANOPTICON_CONFIG_DIR")); configured != "" {
		if strings.HasPrefix(configured, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", workflowError("cannot resolve PANOPTICON_CONFIG_DIR: %v", err)
			}
			configured = filepath.Join(home, strings.TrimPrefix(configured, "~/"))
		}
		absolute, err := filepath.Abs(configured)
		if err != nil {
			return "", workflowError("cannot resolve PANOPTICON_CONFIG_DIR: %v", err)
		}
		return filepath.Clean(absolute), nil
	}
	base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// User config is optional. Explicit PANOPTICON_CONFIG_DIR errors above,
			// but a missing default home must not break repo or absolute workflows.
			return "", nil
		}
		base = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(base) {
		absolute, err := filepath.Abs(base)
		if err != nil {
			return "", workflowError("cannot resolve user config directory: %v", err)
		}
		base = absolute
	}
	return filepath.Clean(filepath.Join(base, "panopticon")), nil
}

func loadConfigFile(path, label string) (map[string]any, error) {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, workflowError("cannot read %s: %v", label, err)
	}
	raw, err := ParseTOML(string(content))
	if err != nil {
		return nil, workflowError("invalid %s: %v", label, err)
	}
	return raw, nil
}

func LoadUserConfig() (map[string]any, error) {
	directory, err := userConfigDir()
	if err != nil {
		return nil, err
	}
	if directory == "" {
		return map[string]any{}, nil
	}
	return loadConfigFile(filepath.Join(directory, "config.toml"), "user config.toml")
}

func configuredWorkflowName(config map[string]any) string {
	containers := []map[string]any{config}
	for _, key := range []string{"flow", "workflow"} {
		if table, ok := config[key].(map[string]any); ok {
			containers = append(containers, table)
		}
	}
	for _, table := range containers {
		for _, key := range []string{"workflow", "name"} {
			if value, ok := table[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func configuredWorkflowFromSources(requested string, configs ...map[string]any) string {
	if strings.TrimSpace(requested) != "" {
		return requested
	}
	for _, config := range configs {
		if configured := configuredWorkflowName(config); configured != "" {
			return configured
		}
	}
	return ""
}

func ResolveWorkflowPath(repo, requested, packageRoot string, config map[string]any) (string, error) {
	resolvedRepo, err := filepath.Abs(repo)
	if err != nil {
		failure := fmt.Errorf("repo path: %w", err)
		return "", failure
	}
	repo = resolvedRepo
	if packageRoot != "" {
		resolvedPackageRoot, err := filepath.Abs(packageRoot)
		if err != nil {
			failure := fmt.Errorf("package root: %w", err)
			return "", failure
		}
		packageRoot = resolvedPackageRoot
	}
	if requested == "" {
		requested = configuredWorkflowName(config)
	}
	if requested == "" {
		requested = "standard"
	}
	requestedPath := requested
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(requestedPath, "~/") {
		requestedPath = filepath.Join(home, strings.TrimPrefix(requestedPath, "~/"))
	}
	configDirectory, err := userConfigDir()
	if err != nil {
		return "", err
	}
	candidates := []string{}
	if filepath.IsAbs(requestedPath) {
		candidates = append(candidates, requestedPath)
	} else if strings.HasSuffix(requestedPath, ".toml") || strings.ContainsAny(requestedPath, `/\\`) {
		candidates = append(candidates, filepath.Join(repo, requestedPath), filepath.Join(repo, ".panopticon", requestedPath))
		if configDirectory != "" {
			candidates = append(candidates, filepath.Join(configDirectory, requestedPath))
		}
		if packageRoot != "" {
			candidates = append(candidates, filepath.Join(packageRoot, requestedPath))
		}
		if filepath.Dir(requestedPath) == "." {
			candidates = append(candidates, filepath.Join(repo, "workflows", requestedPath), filepath.Join(repo, ".panopticon", "workflows", requestedPath))
			if configDirectory != "" {
				candidates = append(candidates, filepath.Join(configDirectory, "workflows", requestedPath))
			}
			if packageRoot != "" {
				candidates = append(candidates, filepath.Join(packageRoot, "workflows", requestedPath))
			}
		}
	} else {
		candidates = append(candidates, filepath.Join(repo, "workflows", requestedPath+".toml"), filepath.Join(repo, ".panopticon", "workflows", requestedPath+".toml"))
		if configDirectory != "" {
			candidates = append(candidates, filepath.Join(configDirectory, "workflows", requestedPath+".toml"))
		}
		if packageRoot != "" {
			candidates = append(candidates, filepath.Join(packageRoot, "workflows", requestedPath+".toml"))
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			absolute, absErr := filepath.Abs(candidate)
			if absErr != nil {
				continue
			}
			cleaned := filepath.Clean(absolute)
			return cleaned, nil
		}
	}
	if requested == "standard" {
		if _, err := embeddedRead("workflows/standard.toml"); err == nil {
			return "embedded:workflows/standard.toml", nil
		}
	}
	failure := workflowError("workflow '%s' not found. Searched: %s", requested, strings.Join(candidates, ", "))
	return "", failure
}

func commandsFromConfig(config map[string]any) ([][]string, error) {
	candidates := []any{}
	for _, key := range []string{"verification", "verify"} {
		if table, ok := config[key].(map[string]any); ok {
			candidates = append(candidates, table["commands"], table["command"])
		}
	}
	candidates = append(candidates, config["verification_commands"], config["verify_commands"])
	for _, candidate := range candidates {
		if candidate != nil {
			commands, err := parseCommandList(candidate, "repo verification")
			if err != nil {
				return nil, fmt.Errorf("configured verification: %w", err)
			}
			return commands, nil
		}
	}
	return nil, nil
}

func ResolveVerifyCommands(cliValues []string, config map[string]any, workflow Workflow) ([][]string, error) {
	if len(cliValues) > 0 {
		commands := make([][]string, 0, len(cliValues))
		for _, value := range cliValues {
			command, err := shellSplit(value)
			if err != nil {
				return nil, workflowError("invalid --verify quoting: %v", err)
			}
			if len(command) == 0 {
				return nil, workflowError("--verify cannot specify an empty command")
			}
			commands = append(commands, command)
		}
		return commands, nil
	}
	configured, err := commandsFromConfig(config)
	if err != nil {
		return nil, fmt.Errorf("repository verification: %w", err)
	}
	if len(configured) > 0 {
		return configured, nil
	}
	return workflow.DefaultVerify, nil
}

func resolveVerifyCommandsFromSources(cliValues []string, workflow Workflow, configs ...map[string]any) ([][]string, error) {
	if len(cliValues) > 0 {
		return ResolveVerifyCommands(cliValues, nil, workflow)
	}
	for index, config := range configs {
		configured, err := commandsFromConfig(config)
		if err != nil {
			labels := []string{"repository", "user"}
			label := "configuration"
			if index < len(labels) {
				label = labels[index]
			}
			return nil, fmt.Errorf("%s verification: %w", label, err)
		}
		if len(configured) > 0 {
			return configured, nil
		}
	}
	return workflow.DefaultVerify, nil
}

func shellSplit(value string) ([]string, error) {
	var result []string
	var current strings.Builder
	quote := byte(0)
	escaped := false
	flush := func() {
		if current.Len() > 0 {
			result = append(result, current.String())
			current.Reset()
		}
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if escaped {
			current.WriteByte(character)
			escaped = false
			continue
		}
		if quote == '\'' {
			if character == '\'' {
				quote = 0
			} else {
				current.WriteByte(character)
			}
			continue
		}
		if quote == '"' {
			switch character {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				current.WriteByte(character)
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '\\':
			escaped = true
		case ' ', '\t', '\n', '\r':
			flush()
		default:
			current.WriteByte(character)
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated quote or escape")
	}
	flush()
	return result, nil
}
