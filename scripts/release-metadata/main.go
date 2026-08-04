// Command release-metadata prints the deterministic release metadata the
// release pipeline stamps into the candidate binary through linker flags
// (Task 22, design 23): the applied SQLite schema version, the migration
// registry hash, the Artifact/IR schema compatibility hash, the Provider
// registry hash, the prompt registry hash, and the enabled Provider binding
// hashes.
//
// Every value is derived from the embedded registries, never from Git or
// the environment, so a rebuild from the same source and toolchain produces
// the same values and the same binary. gate1.sh/gate2.sh record them in
// their Manifests; check-cross-build.sh and gate3.sh stamp them with
// -ldflags "-X cflow.local/cflow/internal/observe.<Var>=<value>" and prove
// the stamping through `go version -m` on the built binary.
//
// Output is `key=value` per line, ordered, so the shell can `eval` it.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"cflow.local/cflow/internal/agent"
	migrationfs "cflow.local/cflow/migrations"
	schemafs "cflow.local/cflow/schemas"
)

func main() {
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		die(err)
	}
	prompts, err := agent.LoadPromptRegistry()
	if err != nil {
		die(err)
	}
	values := map[string]string{
		"schema_version": schemaVersion(),
		"migration":      migrationRegistryHash(),
		"artifact":       artifactCompatibilityHash(),
		"provider":       reg.Revision(),
		"prompt":         prompts.Revision(),
	}
	for _, name := range reg.EnabledNames() {
		binding, err := reg.Select(name)
		if err == nil {
			values[name] = binding.Hash
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, values[k])
	}
}

// schemaVersion is the applied SQLite schema version: the highest version
// of the embedded forward-only migration chain (design 23 build metadata:
// the schema range the binary supports).
func schemaVersion() string {
	names := embeddedSQLFiles()
	max := 0
	for _, n := range names {
		num := 0
		for _, ch := range strings.TrimSuffix(n, ".sql") {
			if ch < '0' || ch > '9' {
				break
			}
			num = num*10 + int(ch-'0')
		}
		if num > max {
			max = num
		}
	}
	return strconv.Itoa(max)
}

// migrationRegistryHash digests the canonical serialization of the embedded
// forward-only migration registry: each migration file's name followed by
// its content, in ascending version order. The per-migration content
// digests the Store pins separately; this is the registry-level revision.
func migrationRegistryHash() string {
	h := sha256.New()
	for _, n := range embeddedSQLFiles() {
		body, err := migrationfs.FS.ReadFile(n)
		if err != nil {
			die(err)
		}
		h.Write([]byte(n))
		h.Write([]byte{0})
		h.Write(body)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// artifactCompatibilityHash digests the canonical serialization of the
// embedded Artifact/IR schema contracts (schemas/*.json): the validation
// contracts the Artifact compatibility registry reads, sorted by file name.
func artifactCompatibilityHash() string {
	h := sha256.New()
	names, err := fs.Glob(schemafs.FS, "*.json")
	if err != nil {
		die(err)
	}
	sort.Strings(names)
	for _, n := range names {
		body, err := schemafs.FS.ReadFile(n)
		if err != nil {
			die(err)
		}
		h.Write([]byte(n))
		h.Write([]byte{0})
		h.Write(body)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func embeddedSQLFiles() []string {
	names, err := fs.Glob(migrationfs.FS, "*.sql")
	if err != nil {
		die(err)
	}
	sort.Strings(names)
	return names
}

func die(v any) {
	fmt.Fprintln(os.Stderr, "release-metadata:", v)
	os.Exit(1)
}
