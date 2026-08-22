package panopticon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestGitSnapshotUsesStatusAndIgnoresCache(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root, map[string]string{".gitignore": ".cache/\n", "tracked.txt": "initial\n"})
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cache", "ignored.bin"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "space name.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "git.log")
	wrapper := "#!/bin/sh\n" +
		"printf '%s\\n' \"LC_ALL=$LC_ALL\" \"LANG=$LANG\" \"LANGUAGE=$LANGUAGE\" > \"" + logPath + "\"\n" +
		"printf '%s\\0' \"$@\" >> \"" + logPath + "\"\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	snapshot, err := snapshotWorktree(root, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(logged)
	if !strings.Contains(text, "LC_ALL=C") || !strings.Contains(text, "LANG=C") || !strings.Contains(text, "LANGUAGE=C") {
		t.Fatalf("locale env missing: %q", text)
	}
	if !strings.Contains(text, "status") || !strings.Contains(text, "--porcelain=v1") || !strings.Contains(text, "-z") || !strings.Contains(text, "--untracked-files=all") {
		t.Fatalf("git argv missing: %q", text)
	}
	if _, ok := snapshot["tracked.txt"]; !ok {
		t.Fatalf("tracked missing: %#v", snapshot)
	}
	if _, ok := snapshot["space name.txt"]; !ok {
		t.Fatalf("untracked missing: %#v", snapshot)
	}
	if _, ok := snapshot[".cache/ignored.bin"]; ok {
		t.Fatal("ignored cache file was snapshotted")
	}
	if !strings.Contains(snapshot["tracked.txt"], "git: M:file:") && !strings.Contains(snapshot["tracked.txt"], "git:M :file:") && !strings.Contains(snapshot["tracked.txt"], "git:") {
		t.Fatalf("tracked snapshot=%q", snapshot["tracked.txt"])
	}
	if !strings.Contains(snapshot["space name.txt"], "git:??:file:") {
		t.Fatalf("untracked snapshot=%q", snapshot["space name.txt"])
	}
}

func TestGitSnapshotDetectsDirtyRechangeRenameAndDelete(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root, map[string]string{"existing.txt": "initial\n", "old name.txt": "rename me\n", "deleted.txt": "delete me\n"})
	existing := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(existing, []byte("dirty before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotWorktree(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("dirty after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode()
	if err := os.Chmod(existing, mode^0o100); err != nil {
		t.Fatal(err)
	}
	after, err := snapshotWorktree(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if diff := snapshotDiff(before, after); strings.Join(diff, ",") != "existing.txt" {
		t.Fatalf("rechange diff=%v", diff)
	}
	runGit(t, root, "mv", "old name.txt", "new name.txt")
	runGit(t, root, "rm", "deleted.txt")
	snapshot, err := snapshotWorktree(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot["new name.txt"]; !ok {
		t.Fatalf("rename dest missing: %#v", snapshot)
	}
	if _, ok := snapshot["old name.txt"]; !ok {
		t.Fatalf("rename source missing: %#v", snapshot)
	}
	if !strings.Contains(snapshot["new name.txt"], "original:old name.txt") {
		t.Fatalf("rename origin=%q", snapshot["new name.txt"])
	}
	if !strings.Contains(snapshot["old name.txt"], "git:") || !strings.Contains(snapshot["old name.txt"], "deleted") {
		t.Fatalf("old name snapshot=%q", snapshot["old name.txt"])
	}
	if !strings.Contains(snapshot["deleted.txt"], "git:") || !strings.Contains(snapshot["deleted.txt"], "deleted") {
		t.Fatalf("deleted snapshot=%q", snapshot["deleted.txt"])
	}
	diff := snapshotDiff(after, snapshot)
	if strings.Join(diff, ",") != "deleted.txt,new name.txt,old name.txt" {
		t.Fatalf("rename/delete diff=%v", diff)
	}
	info, err = os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	modeToken := ":" + strconv.FormatUint(uint64(info.Mode().Perm()), 8) + ":"
	if !strings.Contains(snapshot["existing.txt"], modeToken) {
		t.Fatalf("mode missing from %q want %s", snapshot["existing.txt"], modeToken)
	}
}

func TestGitSnapshotParsesCopyRecord(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root, map[string]string{"source.txt": "source\n"})
	if err := os.WriteFile(filepath.Join(root, "copy name.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	installFakeGit(t, bin, "copy")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	snapshot, err := snapshotWorktree(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	if !strings.Contains(snapshot["copy name.txt"], "original:source.txt") {
		t.Fatalf("copy snapshot=%q", snapshot["copy name.txt"])
	}
}

func TestGitSnapshotRejectsDirectoryStatusPath(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "submodule"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	installFakeGit(t, bin, "directory")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := snapshotWorktree(root, nil, false)
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected directory snapshot error, got %v", err)
	}
}

func TestGitStatusFailureFailsClosed(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root, map[string]string{"tracked.txt": "tracked\n"})
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'fatal: index unavailable' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := snapshotWorktree(root, nil, true)
	if err == nil || !strings.Contains(err.Error(), "git status failed") {
		t.Fatalf("expected git status failure, got %v", err)
	}
}

func TestNonGitNoWorktreeUsesFilesystemFallback(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "normal.txt"), []byte("normal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotWorktree(root, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot["normal.txt"]; !ok {
		t.Fatalf("fallback snapshot=%#v", snapshot)
	}
	if _, err := snapshotWorktree(root, nil, false); err == nil {
		t.Fatal("expected fail-closed without fallback")
	}
}

func TestSpecialSnapshotUsesRawUnixMode(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	snapshot, err := snapshotWorktree(root, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := snapshot["pipe"]
	if !ok {
		t.Fatalf("fifo missing: %#v", snapshot)
	}
	// Python 参照実装と同じ生 st_mode (S_IFIFO|0644 = 010644) を使う。
	if !strings.HasPrefix(value, "special:10644:") {
		t.Fatalf("special snapshot=%q want raw st_mode prefix special:10644:", value)
	}
}

func TestFileDigestDetectsSwappedPath(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.txt")
	second := filepath.Join(root, "second.txt")
	contents := []byte("identical contents\n")
	if err := os.WriteFile(first, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Lstat(second)
	if err != nil {
		t.Fatal(err)
	}
	// 同一内容・同一サイズでも dev/ino が異なるファイルとの不一致を検出する。
	if _, err := fileDigest(first, secondInfo); err == nil || !strings.Contains(err.Error(), "changed during worktree snapshot") {
		t.Fatalf("swapped path was not detected: %v", err)
	}
}
