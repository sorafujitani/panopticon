package panopticon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type StateError struct{ Message string }

func (e *StateError) Error() string { return e.Message }

func stateError(format string, args ...any) error {
	return &StateError{Message: fmt.Sprintf(format, args...)}
}

var waitTerminal = map[string]bool{
	"completed": true,
	"failed":    true,
	"cancelled": true,
	"blocked":   true,
}

const (
	DefaultWaitTimeoutSeconds  = 3600.0
	DefaultWaitIntervalSeconds = 1.0
	compactMaxSteps            = 32
	compactMaxStepIDLength     = 128
	compactMaxStepTextLength   = 1000
	compactMaxRunErrorLength   = 3000
	compactMaxErrorIdentifier  = 256
	compactMaxPathLength       = 2000
	compactMaxOutputBytes      = 12 * 1024
)

func safeText(value any) (result string) {
	defer func() {
		if recover() != nil {
			result = "<unserializable>"
		}
	}()
	if value == nil {
		return "<nil>"
	}
	return strings.ToValidUTF8(fmt.Sprint(value), "�")
}

func jsonText(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return safeText(value)
	}
	return strings.ToValidUTF8(string(encoded), "�")
}

func truncateText(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	runes := []rune(strings.ToValidUTF8(value, "�"))
	if len(runes) > maximum {
		return string(runes[:maximum])
	}
	return string(runes)
}

func boundedText(value any, maximum int, stringifyJSON bool) any {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok {
		return truncateText(text, maximum)
	}
	if stringifyJSON {
		return truncateText(jsonText(value), maximum)
	}
	return truncateText(safeText(value), maximum)
}

func compactValue(value any) any {
	return boundedText(value, compactMaxOutputBytes, true)
}

func compactError(value any, maximum int) any {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok {
		return truncateText(text, maximum)
	}
	table, ok := value.(map[string]any)
	if !ok {
		return boundedText(value, maximum, true)
	}
	result := map[string]any{}
	for _, key := range []string{"type", "code", "message", "returncode", "stderr"} {
		item, exists := table[key]
		if !exists {
			continue
		}
		if key == "type" || key == "code" || key == "returncode" {
			result[key] = compactErrorIdentifier(item)
		} else {
			result[key] = boundedErrorText(item, maximum)
		}
	}
	if len(result) > 0 {
		return result
	}
	return map[string]any{"message": boundedText(value, maximum, true)}
}

func boundedErrorText(value any, maximum int) any {
	return boundedText(value, maximum, true)
}

func compactErrorIdentifier(value any) any {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok {
		return truncateText(text, compactMaxErrorIdentifier)
	}
	switch value.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		if encoded, err := json.Marshal(value); err == nil && len([]rune(string(encoded))) <= compactMaxErrorIdentifier {
			return value
		}
	}
	bounded := boundedText(value, compactMaxErrorIdentifier, true)
	if text, ok := bounded.(string); ok && text != "" {
		return text
	}
	return "<unserializable>"
}

func CompactState(state map[string]any) map[string]any {
	steps := map[string]any{}
	if rawSteps, ok := state["steps"].(map[string]any); ok {
		count := 0
		for stepID, raw := range rawSteps {
			if count >= compactMaxSteps {
				break
			}
			step, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			var summary any
			if result, ok := step["result"].(map[string]any); ok {
				summary = result["summary"]
			} else {
				summary = step["summary"]
			}
			steps[truncateText(safeText(stepID), compactMaxStepIDLength)] = map[string]any{
				"status":  compactValue(step["status"]),
				"summary": boundedText(summary, compactMaxStepTextLength, true),
				"error":   compactError(step["error"], compactMaxStepTextLength),
			}
			count++
		}
	}
	worktreePath := any(nil)
	if worktree, ok := state["worktree"].(map[string]any); ok {
		worktreePath = worktree["path"]
	} else {
		worktreePath = state["worktree"]
	}
	payload := map[string]any{
		"run_id":       compactValue(state["run_id"]),
		"status":       compactValue(state["status"]),
		"current_step": compactValue(state["current_step"]),
		"repo":         boundedText(state["repo"], compactMaxPathLength, false),
		"worktree":     boundedText(worktreePath, compactMaxPathLength, false),
		"steps":        steps,
		"error":        compactError(state["error"], compactMaxRunErrorLength),
		"updated_at":   compactValue(state["updated_at"]),
	}
	return fitCompactPayload(payload)
}

func compactJSONSize(payload map[string]any) int {
	encoded, err := marshalIndented(payload)
	if err != nil {
		return compactMaxOutputBytes + 1
	}
	return len(encoded) + 1
}

func shrinkTextField(container map[string]any, field string) bool {
	value, ok := container[field].(string)
	if ok && value != "" {
		runes := []rune(value)
		if len(runes) <= 1 {
			container[field] = ""
		} else {
			container[field] = string(runes[:len(runes)/2])
		}
		return true
	}
	if field == "error" {
		if nested, ok := container[field].(map[string]any); ok {
			for _, key := range []string{"stderr", "message"} {
				if shrinkTextField(nested, key) {
					return true
				}
			}
		}
	}
	return false
}

func fitCompactPayload(payload map[string]any) map[string]any {
	for compactJSONSize(payload) > compactMaxOutputBytes {
		changed := false
		if steps, ok := payload["steps"].(map[string]any); ok {
			for _, raw := range steps {
				step, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				changed = shrinkTextField(step, "summary") || changed
				changed = shrinkTextField(step, "error") || changed
			}
		}
		if changed {
			continue
		}
		for _, field := range []string{"error", "repo", "worktree", "current_step", "updated_at"} {
			if shrinkTextField(payload, field) {
				changed = true
			}
		}
		if changed {
			continue
		}
		if steps, ok := payload["steps"].(map[string]any); ok {
			for _, raw := range steps {
				if step, ok := raw.(map[string]any); ok && shrinkTextField(step, "status") {
					changed = true
				}
			}
		}
		if changed {
			continue
		}
		if steps, ok := payload["steps"].(map[string]any); ok {
			shortened := map[string]any{}
			for key, value := range steps {
				shortKey := truncateText(key, maxInt(1, len([]rune(key))/2))
				if _, exists := shortened[shortKey]; !exists {
					shortened[shortKey] = value
				}
				if shortKey != key {
					changed = true
				}
			}
			if changed {
				payload["steps"] = shortened
				continue
			}
			for key := range steps {
				delete(steps, key)
				changed = true
				break
			}
		}
		if changed {
			continue
		}
		for _, field := range []string{"run_id", "status"} {
			if shrinkTextField(payload, field) {
				changed = true
				break
			}
		}
		if !changed {
			break
		}
	}
	return payload
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func marshalIndented(value any) ([]byte, error) {
	var builder strings.Builder
	encoder := json.NewEncoder(&builder)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return []byte(builder.String()), nil
}

func WaitForTerminal(store *RunStore, runID string, timeout, interval time.Duration) (map[string]any, bool, error) {
	state, timedOut, err := WaitForTerminalContext(context.Background(), store, runID, timeout, interval)
	return state, timedOut, err
}

func WaitForTerminalContext(ctx context.Context, store *RunStore, runID string, timeout, interval time.Duration) (map[string]any, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		state, err := store.Load(runID)
		if err != nil {
			return nil, false, err
		}
		if status, _ := state["status"].(string); waitTerminal[status] {
			return state, false, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return state, true, nil
		}
		sleepFor := interval
		if sleepFor > remaining {
			sleepFor = remaining
		}
		timer := time.NewTimer(sleepFor)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return state, false, ctx.Err()
		case <-timer.C:
		}
	}
}

func utcNow() string {
	return time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
}

func MakeRunID() string {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("run-%s-%08x", time.Now().UTC().Format("20060102T150405Z"), time.Now().UnixNano())
	}
	return fmt.Sprintf("run-%s-%s", time.Now().UTC().Format("20060102T150405Z"), hex.EncodeToString(buffer))
}

func atomicWriteJSON(path string, payload any) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Dir(path), 0o700)
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return stateError("cannot write temporary JSON state file: %s: %v", path, err)
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	encoded, err := marshalIndented(payload)
	if err != nil {
		return stateError("cannot encode JSON state: %v", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return stateError("cannot write JSON state: %s: %v", path, err)
	}
	if err := temporary.Sync(); err != nil {
		return stateError("cannot sync JSON state: %s: %v", path, err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		return stateError("cannot set JSON state permissions: %s: %v", path, err)
	}
	if err := temporary.Close(); err != nil {
		return stateError("cannot close JSON state: %s: %v", path, err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return stateError("cannot replace JSON state: %s: %v", path, err)
	}
	if directory, err := os.Open(filepath.Dir(path)); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func readJSON(path string) (any, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, stateError("state not found: %s", path)
	}
	if err != nil {
		return nil, stateError("cannot read JSON state: %s: %v", path, err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, stateError("JSON state is corrupted: %s: %v", path, err)
	}
	return value, nil
}

// staleLockGrace より新しいロックは、別プロセスが O_EXCL 作成直後で
// まだ内容を書き切っていない可能性があるため回収しない。
const staleLockGrace = 10 * time.Second

type lockPayload struct {
	pid       int64
	createdAt string
}

func readLockPayload(path string) (lockPayload, fs.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return lockPayload{}, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return lockPayload{}, nil, err
	}
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return lockPayload{}, info, err
	}
	table, ok := value.(map[string]any)
	if !ok {
		return lockPayload{}, info, stateError("run lock payload is not an object: %s", path)
	}
	payload := lockPayload{}
	if number, ok := table["pid"].(json.Number); ok {
		payload.pid, _ = number.Int64()
	}
	if text, ok := table["created_at"].(string); ok {
		payload.createdAt = text
	}
	return payload, info, nil
}

type RunLock struct {
	path string
	held bool
}

func (lock *RunLock) acquire() error {
	if err := os.MkdirAll(filepath.Dir(lock.path), 0o700); err != nil {
		return stateError("cannot create the parent directory for the run lock: %v", err)
	}
	payload := map[string]any{"pid": os.Getpid(), "created_at": utcNow()}
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(lock.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			encoded, _ := json.Marshal(payload)
			_, writeErr := file.Write(encoded)
			_ = file.Close()
			if writeErr != nil {
				_ = os.Remove(lock.path)
				return stateError("cannot write run lock: %s: %v", lock.path, writeErr)
			}
			lock.held = true
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return stateError("cannot acquire run lock: %s: %v", lock.path, err)
		}
		existing, info, readErr := readLockPayload(lock.path)
		if readErr == nil && pidAlive(existing.pid) {
			return stateError("run is already active in another process (pid=%d)", existing.pid)
		}
		// 空や部分書き込みのロックは生存中の別プロセスが書き込み中の可能性があるため、
		// 新鮮なロックは pid を解釈できても回収しない。
		if readErr != nil || time.Since(info.ModTime()) < staleLockGrace {
			return stateError("cannot acquire run lock: %s", lock.path)
		}
		// 削除前に読み直し、同一内容・同一ファイルであることを確認してから回収する。
		rechecked, recheckInfo, recheckErr := readLockPayload(lock.path)
		if recheckErr != nil || rechecked != existing || !os.SameFile(info, recheckInfo) {
			return stateError("cannot acquire run lock: %s", lock.path)
		}
		_ = os.Remove(lock.path)
	}
	return stateError("cannot acquire run lock: %s", lock.path)
}

func (lock *RunLock) release() {
	if !lock.held {
		return
	}
	_ = os.Remove(lock.path)
	lock.held = false
}

func (lock *RunLock) With(fn func() error) error {
	if err := lock.acquire(); err != nil {
		return err
	}
	defer lock.release()
	return fn()
}

type RunStore struct{ Root string }

func NewRunStore(root string) (*RunStore, error) {
	if root == "" {
		root = os.Getenv("PANOPTICON_STATE_DIR")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, stateError("cannot determine HOME: %v", err)
		}
		root = filepath.Join(home, ".local", "state", "panopticon", "runs")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, stateError("invalid state root: %v", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, stateError("cannot create state directory: %s: %v", absolute, err)
	}
	_ = os.Chmod(absolute, 0o700)
	return &RunStore{Root: filepath.Clean(absolute)}, nil
}

func (store *RunStore) RunDir(runID string) (string, error) {
	if runID == "" || filepath.Base(runID) != runID || runID == "." || runID == ".." {
		return "", stateError("invalid run id: %q", runID)
	}
	return filepath.Join(store.Root, runID), nil
}

func (store *RunStore) StatePath(runID string) (string, error) {
	directory, err := store.RunDir(runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "state.json"), nil
}

func (store *RunStore) Lock(runID string) (*RunLock, error) {
	directory, err := store.RunDir(runID)
	if err != nil {
		return nil, err
	}
	return &RunLock{path: filepath.Join(directory, "run.lock")}, nil
}

func (store *RunStore) Create(runID string, state map[string]any) (string, error) {
	directory, err := store.RunDir(runID)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		if os.IsExist(err) {
			return "", stateError("run already exists: %s", runID)
		}
		return "", stateError("cannot create run directory: %s: %v", directory, err)
	}
	if err := os.Mkdir(filepath.Join(directory, "steps"), 0o700); err != nil {
		return "", stateError("cannot create steps directory: %s: %v", directory, err)
	}
	statePath := filepath.Join(directory, "state.json")
	if err := atomicWriteJSON(statePath, state); err != nil {
		return "", err
	}
	return directory, nil
}

func (store *RunStore) Load(runID string) (map[string]any, error) {
	path, err := store.StatePath(runID)
	if err != nil {
		return nil, err
	}
	value, err := readJSON(path)
	if err != nil {
		return nil, err
	}
	state, ok := value.(map[string]any)
	if !ok {
		return nil, stateError("state top level is not an object: %s", runID)
	}
	return state, nil
}

func (store *RunStore) Save(runID string, state map[string]any) error {
	state["updated_at"] = utcNow()
	path, err := store.StatePath(runID)
	if err != nil {
		return err
	}
	return atomicWriteJSON(path, state)
}

func (store *RunStore) List() ([]map[string]any, error) {
	entries := []map[string]any{}
	directoryEntries, err := os.ReadDir(store.Root)
	if os.IsNotExist(err) {
		return entries, nil
	}
	if err != nil {
		return nil, stateError("cannot read state directory: %s: %v", store.Root, err)
	}
	for _, entry := range directoryEntries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(store.Root, entry.Name(), "state.json")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		value, err := readJSON(path)
		if err != nil {
			entries = append(entries, map[string]any{"run_id": entry.Name(), "status": "corrupt", "error": err.Error()})
			continue
		}
		state, ok := value.(map[string]any)
		if !ok {
			entries = append(entries, map[string]any{"run_id": entry.Name(), "status": "corrupt", "error": "state top level is not an object"})
			continue
		}
		workflowName := any(nil)
		if workflow, ok := state["workflow"].(map[string]any); ok {
			workflowName = workflow["name"]
		}
		entries = append(entries, map[string]any{
			"run_id":       valueOr(state["run_id"], entry.Name()),
			"status":       valueOr(state["status"], "unknown"),
			"workflow":     workflowName,
			"task":         state["task"],
			"created_at":   state["created_at"],
			"updated_at":   state["updated_at"],
			"current_step": state["current_step"],
		})
	}
	sort.SliceStable(entries, func(left, right int) bool {
		leftTime := firstString(entries[left]["updated_at"], entries[left]["created_at"])
		rightTime := firstString(entries[right]["updated_at"], entries[right]["created_at"])
		return leftTime > rightTime
	})
	return entries, nil
}

func valueOr(value, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func (store *RunStore) RequestCancel(runID string) (string, error) {
	directory, err := store.RunDir(runID)
	if err != nil {
		return "", err
	}
	path := filepath.Join(directory, "cancel.request")
	if err := atomicWriteJSON(path, map[string]any{"requested_at": utcNow(), "pid": os.Getpid()}); err != nil {
		return "", err
	}
	return path, nil
}

func (store *RunStore) CancelRequested(runID string) (bool, error) {
	directory, err := store.RunDir(runID)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(filepath.Join(directory, "cancel.request"))
	return err == nil, nil
}

func (store *RunStore) ClearCancelRequest(runID string) error {
	directory, err := store.RunDir(runID)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(directory, "cancel.request")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (store *RunStore) Remove(runID string) error {
	directory, err := store.RunDir(runID)
	if err != nil {
		return err
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return stateError("run not found: %s", runID)
	}
	if err := os.RemoveAll(directory); err != nil {
		return stateError("cannot remove run state: %s: %v", directory, err)
	}
	return nil
}

func pidAlive(pid int64) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(int(pid), 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func ParseInt64(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		result, _ := typed.Int64()
		return result
	case float64:
		return int64(typed)
	case int:
		return int64(typed)
	case string:
		result, _ := strconv.ParseInt(typed, 10, 64)
		return result
	default:
		return 0
	}
}
