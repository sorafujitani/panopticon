package panopticon

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func canonicalPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	absolute = filepath.Clean(absolute)
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(resolved)
	}
	parts := []string{}
	current := absolute
	for {
		parent := filepath.Dir(current)
		parts = append([]string{filepath.Base(current)}, parts...)
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Clean(filepath.Join(append([]string{resolved}, parts...)...))
		}
		if parent == current {
			return absolute
		}
		current = parent
	}
}

func isWithin(path string, roots ...string) bool {
	candidate := canonicalPath(path)
	for _, root := range roots {
		absolute := canonicalPath(root)
		if candidate == absolute || strings.HasPrefix(candidate, absolute+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func readSymlinkTarget(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", flowError("worktree snapshot を取得できません: %s: %v", path, err)
	}
	return target, nil
}

func fileDigest(path string, info fs.FileInfo) (string, error) {
	hash := sha256.New()
	file, err := os.Open(path)
	if err != nil {
		failure := flowError("worktree snapshot を取得できません: %s: %v", path, err)
		return "", failure
	}
	buffer := make([]byte, 1024*1024)
	_, copyErr := io.CopyBuffer(hash, file, buffer)
	closeErr := file.Close()
	if copyErr != nil {
		failure := flowError("worktree snapshot を取得できません: %s: %v", path, copyErr)
		return "", failure
	}
	if closeErr != nil {
		failure := flowError("worktree snapshot を取得できません: %s: %v", path, closeErr)
		return "", failure
	}
	after, err := os.Lstat(path)
	if err != nil {
		failure := flowError("worktree snapshot を取得できません: %s: %v", path, err)
		return "", failure
	}
	if after.Size() != info.Size() || after.ModTime() != info.ModTime() || after.Mode().Perm() != info.Mode().Perm() || after.Mode() != info.Mode() {
		failure := flowError("worktree snapshot 中にファイルが変更されました: %s", path)
		return "", failure
	}
	digest := fmt.Sprintf("%x", hash.Sum(nil))
	return digest, nil
}

func snapshotFilesystem(root string, excluded []string) (map[string]string, error) {
	result := map[string]string{}
	isExcluded := func(path string) bool {
		for _, item := range excluded {
			if isWithin(path, item) {
				return true
			}
		}
		return false
	}
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return flowError("worktree snapshot を取得できません: %s: %v", path, err)
		}
		if path == root {
			return nil
		}
		if entry.Name() == ".git" || isExcluded(path) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return flowError("worktree snapshot を取得できません: %s: %v", path, err)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relative worktree path: %w", err)
		}
		relative = filepath.ToSlash(relative)
		mode := info.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			target, err := readSymlinkTarget(path)
			if err != nil {
				return fmt.Errorf("symlink snapshot: %w", err)
			}
			result[relative] = fmt.Sprintf("symlink:%o:%s", mode.Perm(), target)
		case mode.IsDir():
			return nil
		case mode.IsRegular():
			digest, err := fileDigest(path, info)
			if err != nil {
				return fmt.Errorf("file snapshot: %w", err)
			}
			result[relative] = fmt.Sprintf("file:%o:%s", mode.Perm(), digest)
		default:
			result[relative] = fmt.Sprintf("special:%o:%d:%d", mode, info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("filesystem snapshot: %w", walkErr)
	}
	return result, nil
}

func hasGitMetadata(root string) bool {
	current, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	for {
		marker := filepath.Join(current, ".git")
		if info, err := os.Stat(marker); err == nil && (info.IsDir() || !info.IsDir()) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

func statusPath(root string, raw []byte) (string, string, error) {
	decoded := string(raw)
	if decoded == "" || filepath.IsAbs(decoded) {
		failure := flowError("git status の path が worktree 相対ではありません: %q", decoded)
		return "", "", failure
	}
	parts := strings.Split(filepath.ToSlash(decoded), "/")
	for _, part := range parts {
		if part == ".." || part == "" {
			failure := flowError("git status の path が worktree 相対ではありません: %q", decoded)
			return "", "", failure
		}
	}
	path := filepath.Join(append([]string{root}, parts...)...)
	if !isWithin(path, root) {
		failure := flowError("git status の path が worktree の外側です: %q", decoded)
		return "", "", failure
	}
	relative := filepath.ToSlash(filepath.Join(parts...))
	return relative, path, nil
}

type statusEntry struct {
	status   string
	path     []byte
	original []byte
	hasOrig  bool
}

func parseGitStatus(output []byte) ([]statusEntry, error) {
	if len(output) == 0 {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, flowError("git status の NUL 終端が不正です")
	}
	fields := bytes.Split(output[:len(output)-1], []byte{0})
	valid := map[byte]bool{' ': true, 'M': true, 'A': true, 'R': true, 'D': true, '?': true, 'C': true, 'U': true, 'T': true, '!': true}
	entries := []statusEntry{}
	for index := 0; index < len(fields); index++ {
		record := fields[index]
		if len(record) < 4 || record[2] != ' ' || !valid[record[0]] || !valid[record[1]] {
			return nil, flowError("git status の status/path record が不正です")
		}
		entry := statusEntry{status: string(record[:2]), path: append([]byte(nil), record[3:]...)}
		if len(entry.path) == 0 {
			return nil, flowError("git status の path が空です")
		}
		if strings.Contains(entry.status, "R") || strings.Contains(entry.status, "C") {
			index++
			if index >= len(fields) || len(fields[index]) == 0 {
				return nil, flowError("git status の rename/copy 元 path がありません")
			}
			entry.original = append([]byte(nil), fields[index]...)
			entry.hasOrig = true
		}
		if entry.status != "!!" {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func gitSnapshotPath(path, status, original string, allowMissing bool) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) && (allowMissing || strings.Contains(status, "D")) {
			origin := ""
			if original != "" {
				origin = ":original:" + original
			}
			return fmt.Sprintf("git:%s:deleted%s", status, origin), nil
		}
		failure := flowError("git status の path を snapshot できません: %s: %v", path, err)
		return "", failure
	}
	mode := info.Mode()
	origin := ""
	if original != "" {
		origin = ":original:" + original
	}
	if mode&os.ModeSymlink != 0 {
		target, err := readSymlinkTarget(path)
		if err != nil {
			return "", fmt.Errorf("git symlink snapshot: %w", err)
		}
		value := fmt.Sprintf("git:%s:symlink:%o:%s%s", status, mode.Perm(), target, origin)
		return value, nil
	}
	if mode.IsDir() {
		failure := flowError("git status の directory を snapshot できません: %s", path)
		return "", failure
	}
	if mode.IsRegular() {
		digest, err := fileDigest(path, info)
		if err != nil {
			return "", fmt.Errorf("git file snapshot: %w", err)
		}
		value := fmt.Sprintf("git:%s:file:%o:%s%s", status, mode.Perm(), digest, origin)
		return value, nil
	}
	value := fmt.Sprintf("git:%s:special:%o:%d:%d%s", status, mode, info.Size(), info.ModTime().UnixNano(), origin)
	return value, nil
}

func snapshotWorktree(worktree string, excluded []string, allowFilesystemFallback bool) (map[string]string, error) {
	info, err := os.Stat(worktree)
	if err != nil || !info.IsDir() {
		return nil, flowError("worktree が directory ではありません: %s", worktree)
	}
	command := exec.Command("git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	command.Dir = worktree
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "LANGUAGE=C")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	if runErr != nil {
		if allowFilesystemFallback && !hasGitMetadata(worktree) && strings.Contains(strings.ToLower(stderr.String()), "not a git repository") {
			return snapshotFilesystem(worktree, excluded)
		}
		return nil, flowError("git status が失敗しました (returncode=%d): %s", exitCode(command), strings.TrimSpace(stderr.String()))
	}
	entries, err := parseGitStatus(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("snapshot operation: %w", err)
	}
	result := map[string]string{}
	for _, entry := range entries {
		relative, path, err := statusPath(worktree, entry.path)
		if err != nil {
			return nil, fmt.Errorf("snapshot operation: %w", err)
		}
		if pathExcluded(path, excluded) {
			continue
		}
		original := ""
		var originalPath string
		if entry.hasOrig {
			original, originalPath, err = statusPath(worktree, entry.original)
			if err != nil {
				return nil, fmt.Errorf("snapshot operation: %w", err)
			}
			if pathExcluded(originalPath, excluded) {
				continue
			}
		}
		if _, exists := result[relative]; exists {
			return nil, flowError("git status に同じ path が重複しています: %s", relative)
		}
		snapshot, err := gitSnapshotPath(path, entry.status, original, false)
		if err != nil {
			return nil, fmt.Errorf("snapshot operation: %w", err)
		}
		result[relative] = snapshot
		if strings.Contains(entry.status, "R") {
			if !entry.hasOrig {
				return nil, flowError("git status の rename 元 path がありません")
			}
			if _, exists := result[original]; exists {
				return nil, flowError("git status に同じ path が重複しています: %s", original)
			}
			oldSnapshot, err := gitSnapshotPath(originalPath, entry.status, "", true)
			if err != nil {
				return nil, fmt.Errorf("snapshot operation: %w", err)
			}
			result[original] = oldSnapshot
		}
	}
	return result, nil
}

func pathExcluded(path string, excluded []string) bool {
	for _, item := range excluded {
		if isWithin(path, item) {
			return true
		}
	}
	return false
}

func exitCode(command *exec.Cmd) int {
	if command.ProcessState == nil {
		return -1
	}
	return command.ProcessState.ExitCode()
}

func snapshotDiff(before, after map[string]string) []string {
	seen := map[string]bool{}
	for path := range before {
		seen[path] = true
	}
	for path := range after {
		seen[path] = true
	}
	result := []string{}
	for path := range seen {
		if before[path] != after[path] {
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result
}
