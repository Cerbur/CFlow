package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicRenameNoReplacePreservesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	destination := filepath.Join(dir, "destination")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("destination"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicRenameNoReplace(source, destination); err == nil {
		t.Fatal("atomic no-replace overwrote an existing destination")
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "destination" {
		t.Fatalf("destination = %q", got)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source was lost: %v", err)
	}
}
