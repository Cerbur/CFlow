package agent

// Persistent evidence hardening tests (security finding): openEventFile
// re-opens an existing Session events file for append; the open must carry
// O_NOFOLLOW so a symlink swapped in after the guarded CheckPath can never
// be followed (closing the TOCTOU symlink-follow window).

import (
	"os"
	"path/filepath"
	"testing"
)

// evidenceTempRoot resolves the canonical owner-only temp root the guarded
// paths require (the same discipline as the package tests).
func evidenceTempRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve evidence temp root: %v", err)
	}
	if err := os.Chmod(p, 0o700); err != nil {
		t.Fatalf("chmod evidence temp root: %v", err)
	}
	return p
}

// TestOpenEventFileRefusesSymlinkPath asserts a final-component symlink at
// the events path is never followed: the guarded open fails closed. The
// O_NOFOLLOW flag on the existing-file open closes the TOCTOU window
// between the guarded CheckPath and the open itself — a race a deterministic
// unit test cannot force, so this pins the fail-closed property.
func TestOpenEventFileRefusesSymlinkPath(t *testing.T) {
	dir := evidenceTempRoot(t)
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("write events file: %v", err)
	}
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("sensitive\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove events file: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink events path: %v", err)
	}
	if _, err := openEventFile(path); err == nil {
		t.Fatal("openEventFile followed the symlinked events path")
	}
}
