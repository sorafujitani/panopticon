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
	AgentArgs   []string
}

type Workflow struct {
	Name          string
	Version       int
	Path          string
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
			"agent_args":      cloneStrings(step.AgentArgs),
			"contract":        step.Contract.AsMap(),
		})
	}
	verify := make([]any, 0, len(w.DefaultVerify))
	for _, command := range w.DefaultVerify {
		verify = append(verify, cloneStrings(command))
	}
	return map[string]any{
		"name":           w.Name,
		"version":        int64(w.Version),
		"path":           w.Path,
		"digest":         w.Digest,
		"default_verify": verify,
		"steps":          steps,
	}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

var stepIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)

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
			return nil, workflowError("workflow が見つかりません: %s", path)
		}
		return content, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, workflowError("workflow が見つかりません: %s", path)
		}
		return nil, workflowError("workflow を読めません: %s: %v", path, err)
	}
	return content, nil
}

func asString(value any, label string) (string, error) {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", workflowError("%s は空でない文字列で指定してください", label)
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
		return nil, workflowError("%s は文字列配列で指定してください", label)
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, workflowError("%s は文字列配列で指定してください", label)
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
		return nil, workflowError("%s は空でない文字列配列で指定してください", label)
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, workflowError("%s は空でない文字列配列で指定してください", label)
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
		return nil, workflowError("%s はコマンド配列の配列で指定してください", label)
	}
	commands := make([][]string, 0, len(items))
	for index, raw := range items {
		parts, ok := raw.([]any)
		if !ok || len(parts) == 0 {
			return nil, workflowError("%s[%d] は空でない文字列配列で指定してください", label, index)
		}
		command := make([]string, 0, len(parts))
		for _, token := range parts {
			text, ok := token.(string)
			if !ok || text == "" {
				return nil, workflowError("%s[%d] は空でない文字列配列で指定してください", label, index)
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
			return "", workflowError("prompt template が見つかりません: %s", text)
		}
		if _, err := os.Stat(resolved); err != nil {
			return "", workflowError("prompt template が見つかりません: %s", resolved)
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
			return "", workflowError("prompt template が見つかりません: embedded:%s", candidate)
		}
		return "embedded:" + candidate, nil
	}
	resolved, err := filepath.Abs(filepath.Join(filepath.Dir(workflowPath), text))
	if err != nil {
		return "", fmt.Errorf("template relative path: %w", err)
	}
	if _, err := os.Stat(resolved); err != nil {
		return "", workflowError("prompt template が見つかりません: %s", resolved)
	}
	cleaned := filepath.Clean(resolved)
	return cleaned, nil
}

func parseContract(value any, stepID string) (Contract, error) {
	table, ok := value.(map[string]any)
	if !ok {
		return Contract{}, workflowError("step %s: contract table が必要です", stepID)
	}
	required, err := asStringList(table["required_fields"], fmt.Sprintf("step %s.contract.required_fields", stepID))
	if err != nil {
		return Contract{}, fmt.Errorf("workflow contract: %w", err)
	}
	if len(required) == 0 {
		return Contract{}, workflowError("step %s: contract.required_fields は必須です", stepID)
	}
	artifacts, err := asStringList(table["artifact_kinds"], fmt.Sprintf("step %s.contract.artifact_kinds", stepID))
	if err != nil {
		return Contract{}, fmt.Errorf("workflow contract: %w", err)
	}
	if len(artifacts) == 0 {
		return Contract{}, workflowError("step %s: artifact_kinds は1つ以上必要です", stepID)
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
		return StepSpec{}, workflowError("steps は table の配列で指定してください")
	}
	id, err := asString(table["id"], "steps[].id")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	if !stepIDPattern.MatchString(id) {
		return StepSpec{}, workflowError("step id が不正です: %q", id)
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
		return StepSpec{}, workflowError("step %s: 未対応の agent kind: %s", id, kind)
	}
	depends, err := asStringList(table["depends_on"], "step "+id+".depends_on")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	for _, dependency := range depends {
		if dependency == id {
			return StepSpec{}, workflowError("step %s: 自分自身には依存できません", id)
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
		return StepSpec{}, workflowError("step %s: read_policy が不正です: %s", id, readPolicy)
	}
	if !map[string]bool{"none": true, "worktree": true, "repo-and-dependencies": true}[writePolicy] {
		return StepSpec{}, workflowError("step %s: write_policy が不正です: %s", id, writePolicy)
	}
	timeout, err := asInt(table["timeout_seconds"])
	if err != nil || timeout < 1 || timeout > 86400 {
		return StepSpec{}, workflowError("step %s: timeout_seconds は1〜86400の整数です", id)
	}
	templateValue, ok := table["template"].(string)
	if !ok {
		return StepSpec{}, workflowError("steps[].template は空でない文字列で指定してください")
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
	agentArgs, err := asAgentArgs(table["agent_args"], "step "+id+".agent_args")
	if err != nil {
		return StepSpec{}, fmt.Errorf("workflow step: %w", err)
	}
	return StepSpec{ID: id, Role: role, Kind: kind, DependsOn: depends, ReadPolicy: readPolicy, WritePolicy: writePolicy, TimeoutSec: timeout, Template: template, Contract: contract, Condition: condition, ReuseAgent: reuseAgent, SubmitKey: submitKey, AgentArgs: agentArgs}, nil
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
			return workflowError("step id が重複しています")
		}
		byID[step.ID] = step
	}
	for _, step := range steps {
		for _, dependency := range step.DependsOn {
			if _, exists := byID[dependency]; !exists {
				return workflowError("step %s: 未定義の依存先: %s", step.ID, dependency)
			}
		}
		if step.ReuseAgent != "" {
			if _, exists := byID[step.ReuseAgent]; !exists {
				return workflowError("step %s: reuse_agent の参照先がありません: %s", step.ID, step.ReuseAgent)
			}
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return workflowError("workflow に循環依存があります: %s", id)
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
		return nil, workflowError("[workflow] は table で指定してください")
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
			return Workflow{}, workflowError("workflow が見つかりません: %s", path)
		}
		path = filepath.Clean(absolute)
	}
	content, err := readWorkflowFile(path)
	if err != nil {
		return Workflow{}, fmt.Errorf("workflow load: %w", err)
	}
	raw, err := ParseTOML(string(content))
	if err != nil {
		return Workflow{}, workflowError("workflow TOML が不正です (%s): %v", path, err)
	}
	data, err := workflowData(raw)
	if err != nil {
		return Workflow{}, fmt.Errorf("workflow load: %w", err)
	}
	version := 1
	if value, exists := data["version"]; exists {
		parsedVersion, parseErr := asInt(value)
		if parseErr != nil {
			return Workflow{}, workflowError("未対応の workflow version: %v", value)
		}
		version = parsedVersion
	}
	if version != 1 {
		return Workflow{}, workflowError("未対応の workflow version: %d", version)
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
		return Workflow{}, workflowError("workflow.steps は1つ以上の table 配列が必要です")
	}
	steps := make([]StepSpec, 0, len(rawSteps))
	for _, rawStep := range rawSteps {
		step, err := parseStep(rawStep, path)
		if err != nil {
			return Workflow{}, fmt.Errorf("workflow load: %w", err)
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
		return Workflow{}, workflowError("default_verify は1つ以上必要です")
	}
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	return Workflow{Name: name, Version: version, Path: path, Steps: steps, DefaultVerify: defaultVerify, Digest: digest}, nil
}

func LoadRepoConfig(repo string) (map[string]any, error) {
	path := filepath.Join(repo, ".panopticon.toml")
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, workflowError("repo の .panopticon.toml を読めません: %v", err)
	}
	raw, err := ParseTOML(string(content))
	if err != nil {
		return nil, workflowError("repo の .panopticon.toml が不正です: %v", err)
	}
	return raw, nil
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
	candidates := []string{}
	if filepath.IsAbs(requestedPath) {
		candidates = append(candidates, requestedPath)
	} else if strings.HasSuffix(requestedPath, ".toml") || strings.ContainsAny(requestedPath, `/\\`) {
		candidates = append(candidates, filepath.Join(repo, requestedPath), filepath.Join(repo, ".panopticon", requestedPath))
		if packageRoot != "" {
			candidates = append(candidates, filepath.Join(packageRoot, requestedPath))
		}
		if filepath.Dir(requestedPath) == "." {
			candidates = append(candidates, filepath.Join(repo, "workflows", requestedPath), filepath.Join(repo, ".panopticon", "workflows", requestedPath))
			if packageRoot != "" {
				candidates = append(candidates, filepath.Join(packageRoot, "workflows", requestedPath))
			}
		}
	} else {
		candidates = append(candidates, filepath.Join(repo, "workflows", requestedPath+".toml"), filepath.Join(repo, ".panopticon", "workflows", requestedPath+".toml"))
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
	failure := workflowError("workflow '%s' が見つかりません。検索先: %s", requested, strings.Join(candidates, ", "))
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
				return nil, workflowError("--verify の引用が不正です: %v", err)
			}
			if len(command) == 0 {
				return nil, workflowError("--verify に空のコマンドは指定できません")
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
