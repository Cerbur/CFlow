package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/config"
)

// TestLoadRejectsUnknownKeys: the strict schema must reject any key it
// does not declare (design 20.1). This is the brief-mandated test.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := writeConfig(t, "concurrency: 2\nunknown_key: true\n")
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown_key") {
		t.Fatalf("expected unknown-key error, got %v", err)
	}
}

// TestLoadRejectsCredentialsAndCommandStringsBySchemaAbsence: credentials,
// scripts, and raw command strings are impossible because no such key
// exists in the schema; any attempt is an unknown-key failure.
func TestLoadRejectsCredentialsAndCommandStringsBySchemaAbsence(t *testing.T) {
	for _, content := range []string{
		"command: [sh, -c, echo unsafe]\n",
		"token: sk-secret-value\n",
		"script: |\n  rm -rf /\n",
	} {
		path := writeConfig(t, content)
		if _, err := config.Load(path); err == nil {
			t.Fatalf("expected schema-absence rejection for %q", content)
		}
	}
}

// TestLoadRejectsInvalidValueTypes: strict decoding rejects a value that
// cannot be decoded into the declared schema type.
func TestLoadRejectsInvalidValueTypes(t *testing.T) {
	for _, content := range []string{
		"concurrency: many\n",
		"concurrency: 2.5\n",
		"concurrency: [1, 2]\n",
		"concurrency: \"2\"\n",
	} {
		path := writeConfig(t, content)
		if _, err := config.Load(path); err == nil {
			t.Fatalf("expected type rejection for %q", content)
		}
	}
}

// TestLoadEmptyConfigFileIsValid: an empty or comment-only file means
// "no configuration", which resolves to embedded safe defaults.
func TestLoadEmptyConfigFileIsValid(t *testing.T) {
	path := writeConfig(t, "# no configuration\n")
	file, err := config.Load(path)
	if err != nil {
		t.Fatalf("empty config must load: %v", err)
	}
	got, err := config.Resolve(file, config.Overrides{})
	if err != nil || got.Concurrency != 1 {
		t.Fatalf("got %#v, %v", got, err)
	}
}

// TestLoadRejectsMultipleDocuments: the strict file is exactly one
// YAML document.
func TestLoadRejectsMultipleDocuments(t *testing.T) {
	path := writeConfig(t, "concurrency: 1\n---\nconcurrency: 2\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected multi-document rejection")
	}
}

// TestLoadMissingFileFails: an absent CFLOW_HOME/config.yaml is a local
// environment precondition failure, surfaced as a config.Error.
func TestLoadMissingFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected missing-file error")
	}
}

// TestResolvePrecedence: explicit CLI input wins over the file value.
// This is the brief-mandated test.
func TestResolvePrecedence(t *testing.T) {
	got, err := config.Resolve(config.File{Concurrency: ptr(2)}, config.Overrides{Concurrency: ptr(3)})
	if err != nil || got.Concurrency != 3 {
		t.Fatalf("got %#v, %v", got, err)
	}
}

// TestResolveFileOverridesDefault: the file value wins over the embedded
// safe default.
func TestResolveFileOverridesDefault(t *testing.T) {
	got, err := config.Resolve(config.File{Concurrency: ptr(5)}, config.Overrides{})
	if err != nil || got.Concurrency != 5 {
		t.Fatalf("got %#v, %v", got, err)
	}
}

// TestResolveUsesEmbeddedSafeDefaults: with no file value and no CLI
// override, the embedded safe default applies.
func TestResolveUsesEmbeddedSafeDefaults(t *testing.T) {
	got, err := config.Resolve(config.File{}, config.Overrides{})
	if err != nil || got.Concurrency != 1 {
		t.Fatalf("got %#v, %v", got, err)
	}
}

// TestResolveRejectsInvalidConcurrency: resolved values are validated;
// concurrency below one is rejected.
func TestResolveRejectsInvalidConcurrency(t *testing.T) {
	for _, v := range []int{0, -1, -8} {
		_, err := config.Resolve(config.File{Concurrency: ptr(v)}, config.Overrides{})
		if err == nil {
			t.Fatalf("expected rejection for concurrency %d", v)
		}
	}
}

func ptr(v int) *int { return &v }

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
