package panopticon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type HerdrError struct{ Message string }

func (e *HerdrError) Error() string { return e.Message }

func herdrError(format string, args ...any) error {
	return &HerdrError{Message: fmt.Sprintf(format, args...)}
}

type CommandError struct {
	Message    string
	Argv       []string
	ReturnCode *int
	Code       string
	Stdout     string
	Stderr     string
}

func (e *CommandError) Error() string { return e.Message }

func RequireHerdrEnv(environ map[string]string) error {
	if environ == nil {
		environ = currentEnvironment()
	}
	if environ["HERDR_ENV"] != "1" {
		return herdrError("run inside a Herdr-managed pane with HERDR_ENV=1")
	}
	return nil
}

type RawResult struct {
	Stdout     string
	Stderr     string
	ReturnCode int
}

var idKeys = map[string][]string{
	"workspace": {"workspace_id", "workspaceid", "workspace"},
	"tab":       {"tab_id", "tabid", "tab"},
	"pane":      {"pane_id", "paneid", "pane", "root_pane", "rootpane", "root_pane_id"},
	"agent":     {"agent_id", "agentid", "agent", "agent_name", "agentname"},
	"worktree":  {"worktree_id", "worktreeid", "worktree"},
}

var idPrefixes = map[string][]string{
	"workspace": {"w"},
	"tab":       {"w"},
	"pane":      {"w"},
	"worktree":  {"w", "worktree", "wt"},
}

var pathKeys = []string{"worktree_path", "worktreepath", "checkout_path", "checkoutpath", "path", "cwd", "working_directory", "workingdirectory"}
var agentStatusKeys = []string{"agent_status", "agentstatus"}
var genericStatusKeys = []string{"status", "state", "lifecycle"}
var agentContainerKeys = []string{"agent", "agent_info", "agentinfo"}
var errorKeys = []string{"code", "error_code", "errorcode", "error", "type"}

func normalizeKey(value string) string { return strings.ToLower(strings.ReplaceAll(value, "-", "_")) }

func looksLikeID(value, kind string) bool {
	if value == "" || strings.HasPrefix(value, "cli:") {
		return false
	}
	if kind == "agent" {
		return true
	}
	for _, prefix := range idPrefixes[kind] {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func scalarFromNode(node any, kind string, keyHint string) string {
	if value, ok := node.(string); ok && looksLikeID(value, kind) {
		return value
	}
	if table, ok := node.(map[string]any); ok {
		exact := map[string]bool{}
		for _, key := range idKeys[kind] {
			exact[normalizeKey(key)] = true
		}
		for key, value := range table {
			if exact[normalizeKey(key)] {
				if result := scalarFromNode(value, kind, normalizeKey(key)); result != "" {
					return result
				}
			}
		}
		for key, value := range table {
			if normalizeKey(key) == "id" {
				if result := scalarFromNode(value, kind, keyHint); result != "" {
					return result
				}
			}
		}
		for _, value := range table {
			if result := scalarFromNode(value, kind, keyHint); result != "" {
				return result
			}
		}
	}
	if list, ok := node.([]any); ok {
		for _, value := range list {
			if result := scalarFromNode(value, kind, keyHint); result != "" {
				return result
			}
		}
	}
	return ""
}

func ExtractID(payload any, kind string) (string, error) {
	kind = strings.ToLower(kind)
	if _, ok := idKeys[kind]; !ok {
		return "", herdrError("unknown Herdr id kind: %s", kind)
	}
	if table, ok := payload.(map[string]any); ok {
		exact := map[string]bool{}
		for _, key := range idKeys[kind] {
			exact[normalizeKey(key)] = true
		}
		for key, value := range table {
			if exact[normalizeKey(key)] {
				if result := scalarFromNode(value, kind, normalizeKey(key)); result != "" {
					return result, nil
				}
			}
		}
		for key, value := range table {
			if normalizeKey(key) == "result" {
				if result, err := ExtractID(value, kind); err == nil && result != "" {
					return result, nil
				}
			}
		}
		for _, value := range table {
			if result := scalarFromNode(value, kind, ""); result != "" {
				return result, nil
			}
		}
	}
	if result := scalarFromNode(payload, kind, ""); result != "" {
		return result, nil
	}
	return "", nil
}

func extractByKeys(payload any, keys []string, stringOnly bool) any {
	wanted := map[string]bool{}
	for _, key := range keys {
		wanted[normalizeKey(key)] = true
	}
	if table, ok := payload.(map[string]any); ok {
		for key, value := range table {
			if wanted[normalizeKey(key)] {
				if !stringOnly {
					return value
				}
				if _, ok := value.(string); ok {
					return value
				}
				if nested := extractByKeys(value, keys, stringOnly); nested != nil {
					return nested
				}
			}
		}
		for _, value := range table {
			if nested := extractByKeys(value, keys, stringOnly); nested != nil {
				return nested
			}
		}
	}
	if list, ok := payload.([]any); ok {
		for _, value := range list {
			if nested := extractByKeys(value, keys, stringOnly); nested != nil {
				return nested
			}
		}
	}
	return nil
}

func ExtractPath(payload any) string {
	if value, ok := extractByKeys(payload, pathKeys, true).(string); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return ""
}

func normalizeStatus(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(text))
}

func ExtractStatus(payload any) string {
	if status := normalizeStatus(extractByKeys(payload, agentStatusKeys, true)); status != "" {
		return status
	}
	if table, ok := payload.(map[string]any); ok {
		for key, value := range table {
			if containsNormalized(agentContainerKeys, normalizeKey(key)) {
				if status := normalizeStatus(extractByKeys(value, append(agentStatusKeys, genericStatusKeys...), true)); status != "" {
					return status
				}
			}
		}
		for key, value := range table {
			if map[string]bool{"result": true, "response": true, "data": true, "payload": true}[normalizeKey(key)] {
				if status := ExtractStatus(value); status != "" {
					return status
				}
			}
		}
	}
	return normalizeStatus(extractByKeys(payload, genericStatusKeys, true))
}

func containsNormalized(values []string, value string) bool {
	for _, item := range values {
		if normalizeKey(item) == value {
			return true
		}
	}
	return false
}

func ExtractErrorCode(payload any) string {
	value := extractByKeys(payload, errorKeys, false)
	if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(text)), " ", "_")
	}
	if table, ok := value.(map[string]any); ok {
		if nested := extractByKeys(table, errorKeys, false); nested != nil {
			if text, ok := nested.(string); ok {
				return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(text)), " ", "_")
			}
		}
	}
	return ""
}

func parseJSONValue(text string) (any, bool) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

func ParseJSONOutput(text string) any {
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return nil
	}
	if value, ok := parseJSONValue(stripped); ok {
		return value
	}
	var documents []any
	for index, character := range text {
		if character != '{' && character != '[' {
			continue
		}
		if value, ok := parseJSONValue(text[index:]); ok {
			documents = append(documents, value)
		}
	}
	if len(documents) == 0 {
		return nil
	}
	return documents[len(documents)-1]
}

func renderPayload(text string) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > 4000 {
		runes = runes[len(runes)-4000:]
	}
	return string(runes)
}

type HerdrClient struct {
	Executable string
	Environ    map[string]string
}

func NewHerdrClient(executable string, environ map[string]string) *HerdrClient {
	if executable == "" {
		executable = os.Getenv("HERDR_BIN")
	}
	if executable == "" {
		executable = "herdr"
	}
	if environ == nil {
		environ = currentEnvironment()
	} else {
		copied := map[string]string{}
		for key, value := range environ {
			copied[key] = value
		}
		environ = copied
	}
	return &HerdrClient{Executable: executable, Environ: environ}
}

func currentEnvironment() map[string]string {
	result := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

func (client *HerdrClient) ResolvedExecutable() string {
	if info, err := os.Stat(client.Executable); err == nil && !info.IsDir() {
		if absolute, err := filepath.Abs(client.Executable); err == nil {
			return absolute
		}
	}
	if path, err := exec.LookPath(client.Executable); err == nil {
		if absolute, err := filepath.Abs(path); err == nil {
			return absolute
		}
		return path
	}
	return client.Executable
}

func (client *HerdrClient) envList() []string {
	keys := make([]string, 0, len(client.Environ))
	for key := range client.Environ {
		keys = append(keys, key)
	}
	// Stable environment ordering makes diagnostics and tests reproducible.
	sortStrings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+client.Environ[key])
	}
	return result
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && values[cursor] < values[cursor-1]; cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}

func (client *HerdrClient) runArgs(args []string) []string {
	return append([]string{client.Executable}, args...)
}

func (client *HerdrClient) RunRaw(args []string, timeout time.Duration) (RawResult, error) {
	return client.RunRawAt(args, timeout, "")
}

func (client *HerdrClient) RunRawAt(args []string, timeout time.Duration, directory string) (RawResult, error) {
	if err := RequireHerdrEnv(client.Environ); err != nil {
		return RawResult{}, err
	}
	available := false
	if info, err := os.Stat(client.Executable); err == nil && !info.IsDir() {
		available = true
	} else if _, err := exec.LookPath(client.Executable); err == nil {
		available = true
	}
	if !available {
		return RawResult{}, herdrError("herdr executable not found: %s", client.Executable)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, "env", append([]string{client.Executable}, args...)...)
	command.Dir = directory
	command.Env = client.envList()
	command.Stdin = nil
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return RawResult{}, &CommandError{Message: fmt.Sprintf("Herdr command timed out: %s", strings.Join(client.runArgs(args), " ")), Argv: client.runArgs(args), Code: "timeout_or_start_failed"}
	}
	if err != nil && command.ProcessState == nil {
		return RawResult{}, &CommandError{Message: fmt.Sprintf("failed to start Herdr command: %s", strings.Join(client.runArgs(args), " ")), Argv: client.runArgs(args), Code: "timeout_or_start_failed", Stdout: stdout.String(), Stderr: stderr.String()}
	}
	returnCode := 0
	if command.ProcessState != nil {
		returnCode = command.ProcessState.ExitCode()
	}
	return RawResult{Stdout: stdout.String(), Stderr: stderr.String(), ReturnCode: returnCode}, nil
}

func (client *HerdrClient) RunJSON(args []string, timeout time.Duration) (any, error) {
	return client.RunJSONAt(args, timeout, "")
}

func (client *HerdrClient) RunJSONAt(args []string, timeout time.Duration, directory string) (any, error) {
	result, err := client.RunRawAt(args, timeout, directory)
	if err != nil {
		return nil, err
	}
	payload := ParseJSONOutput(result.Stdout)
	if payload == nil {
		payload = ParseJSONOutput(result.Stderr)
	}
	if result.ReturnCode != 0 {
		code := ExtractErrorCode(payload)
		if code == "" {
			code = "command_failed"
		}
		details := renderPayload(result.Stderr)
		if details == "" {
			details = renderPayload(result.Stdout)
		}
		message := fmt.Sprintf("Herdr command failed (%s): %s", code, strings.Join(client.runArgs(args), " "))
		if details != "" {
			message += "\n" + details
		}
		return nil, &CommandError{Message: message, Argv: client.runArgs(args), ReturnCode: intPointer(result.ReturnCode), Code: code, Stdout: result.Stdout, Stderr: result.Stderr}
	}
	if payload == nil {
		return nil, herdrError("Herdr did not return JSON: %s", strings.Join(client.runArgs(args), " "))
	}
	return payload, nil
}

func (client *HerdrClient) RunText(args []string, timeout time.Duration) (string, error) {
	return client.RunTextAt(args, timeout, "")
}

func (client *HerdrClient) RunTextAt(args []string, timeout time.Duration, directory string) (string, error) {
	result, err := client.RunRawAt(args, timeout, directory)
	if err != nil {
		return "", err
	}
	if result.ReturnCode != 0 {
		payload := ParseJSONOutput(result.Stderr)
		if payload == nil {
			payload = ParseJSONOutput(result.Stdout)
		}
		code := ExtractErrorCode(payload)
		if code == "" {
			code = "command_failed"
		}
		return "", &CommandError{Message: fmt.Sprintf("Herdr command failed (%s): %s\n%s", code, strings.Join(client.runArgs(args), " "), renderPayload(result.Stderr+result.Stdout)), Argv: client.runArgs(args), ReturnCode: intPointer(result.ReturnCode), Code: code, Stdout: result.Stdout, Stderr: result.Stderr}
	}
	return strings.TrimSpace(result.Stdout), nil
}

func intPointer(value int) *int { return &value }
