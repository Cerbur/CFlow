package tui

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenOperationLogCreatesOwnerOnlyJSONLFile(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve test parent: %v", err)
	}
	home := filepath.Join(parent, ".cflow")

	file, err := OpenOperationLog(home)
	if err != nil {
		t.Fatalf("open operation log: %v", err)
	}
	if _, err := file.WriteString(`{"kind":"user_action","action":"plan_check"}` + "\n"); err != nil {
		file.Close()
		t.Fatalf("write operation log: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close operation log: %v", err)
	}

	logPath := filepath.Join(home, "logs", operationLogName)
	for _, path := range []string{home, filepath.Dir(logPath), logPath} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("stat %s: %v", path, statErr)
		}
		wantMode := os.FileMode(0o700)
		if path == logPath {
			wantMode = 0o600
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("%s mode = %o, want %o", path, got, wantMode)
		}
	}

	input, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("read operation log: %v", err)
	}
	defer input.Close()
	scanner := bufio.NewScanner(input)
	if !scanner.Scan() {
		t.Fatalf("operation log has no JSONL record")
	}
	var entry map[string]string
	if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
		t.Fatalf("decode operation log JSONL: %v", err)
	}
	if entry["kind"] != "user_action" || entry["action"] != "plan_check" {
		t.Fatalf("operation log entry = %#v", entry)
	}
	if scanner.Scan() {
		t.Fatal("operation log contains an unexpected extra record")
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan operation log: %v", err)
	}
}

func TestOpenOperationLogAppendsToExistingFile(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve test parent: %v", err)
	}
	home := filepath.Join(parent, ".cflow")

	first, err := OpenOperationLog(home)
	if err != nil {
		t.Fatalf("open first operation log: %v", err)
	}
	if _, err := first.WriteString(`{"kind":"first"}` + "\n"); err != nil {
		first.Close()
		t.Fatalf("write first record: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first operation log: %v", err)
	}

	second, err := OpenOperationLog(home)
	if err != nil {
		t.Fatalf("open second operation log: %v", err)
	}
	if _, err := second.WriteString(`{"kind":"second"}` + "\n"); err != nil {
		second.Close()
		t.Fatalf("write second record: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second operation log: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(home, "logs", operationLogName))
	if err != nil {
		t.Fatalf("read appended operation log: %v", err)
	}
	if string(body) != "{\"kind\":\"first\"}\n{\"kind\":\"second\"}\n" {
		t.Fatalf("appended operation log = %q", body)
	}
}
