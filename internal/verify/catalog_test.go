package verify

// Verification Catalog policy tests (Task 11 brief Step 2): the
// candidate policy rejects shells, inline-code flags, publish/deploy,
// destructive Git, system management, escaped cwd, secret-like values,
// shell metacharacters, and escaping transient paths; the deterministic
// discovery finds the fixed Base Commit wrappers and resolves PATH
// executables.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validCandidate() Candidate {
	return Candidate{
		CommandID:         "verify",
		Purpose:           PurposeTaskVerify,
		ExecutableKind:    KindProjectRelative,
		Executable:        "scripts/verify.sh",
		SHA256:            strings.Repeat("a", 64),
		CWD:               ".",
		TimeoutSeconds:    600,
		ExpectedExitCodes: []int{0},
		OutputLimitBytes:  10485760,
		Env:               []string{"PATH", "TMPDIR", "LANG", "LC_ALL"},
		Source:            "base-commit-wrapper:scripts/verify.sh@sha256:" + strings.Repeat("a", 64),
	}
}

func TestCatalogPolicyAcceptsWrapperAndPathExecutable(t *testing.T) {
	for _, c := range []Candidate{
		validCandidate(),
		func() Candidate {
			c := validCandidate()
			c.CommandID = "go"
			c.Purpose = PurposeFinalVerify
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/local/bin/go"
			c.Args = []string{"test", "./..."}
			c.Env = []string{"PATH", "HOME"}
			c.Source = "path-executable:/usr/local/bin/go@sha256:" + strings.Repeat("b", 64)
			return c
		}(),
	} {
		if err := ValidateCandidate(c); err != nil {
			t.Fatalf("candidate %s should pass policy: %v", c.CommandID, err)
		}
	}
}

func TestCatalogPolicyRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Candidate)
	}{
		{"shell interpreter", func(c *Candidate) { c.Executable = "scripts/sh" }},
		{"shell basename", func(c *Candidate) { c.Executable = "/bin/bash" }},
		{"inline code python -c", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/python3"
			c.Args = []string{"-c", "print(1)"}
		}},
		{"inline code node -e", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/node"
			c.Args = []string{"-e", "1"}
		}},
		{"inline code go run", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/local/bin/go"
			c.Args = []string{"run", "."}
		}},
		{"publish npm", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/npm"
			c.Args = []string{"publish"}
		}},
		{"publish cargo", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/cargo"
			c.Args = []string{"publish"}
		}},
		{"deploy docker push", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/docker"
			c.Args = []string{"push", "img"}
		}},
		{"deploy git push", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/git"
			c.Args = []string{"push", "origin", "main"}
		}},
		{"release gh", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/gh"
			c.Args = []string{"release", "create", "v1"}
		}},
		{"destructive git reset", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/git"
			c.Args = []string{"reset", "--hard"}
		}},
		{"destructive git clean", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/git"
			c.Args = []string{"clean", "-fd"}
		}},
		{"destructive git rebase", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/git"
			c.Args = []string{"rebase", "main"}
		}},
		{"destructive git checkout force", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/git"
			c.Args = []string{"checkout", "--force", "main"}
		}},
		{"destructive git branch delete", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/git"
			c.Args = []string{"branch", "-D", "old"}
		}},
		{"system management systemctl", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/systemctl"
			c.Args = []string{"restart", "docker"}
		}},
		{"system management apt", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/bin/apt"
			c.Args = []string{"install", "x"}
		}},
		{"system management brew", func(c *Candidate) {
			c.ExecutableKind = KindPathExecutable
			c.Executable = "/usr/local/bin/brew"
			c.Args = []string{"install", "x"}
		}},
		{"escaped cwd parent", func(c *Candidate) { c.CWD = "../etc" }},
		{"escaped cwd absolute", func(c *Candidate) { c.CWD = "/etc" }},
		{"secret-like token arg", func(c *Candidate) { c.Args = []string{"--token=abc123"} }},
		{"secret-like password arg", func(c *Candidate) { c.Args = []string{"--password", "hunter2"} }},
		{"secret-like private key", func(c *Candidate) { c.Args = []string{"--key", "-----BEGIN PRIVATE KEY-----"} }},
		{"shell metacharacter semicolon", func(c *Candidate) { c.Args = []string{"a; rm -rf /"} }},
		{"shell metacharacter pipe", func(c *Candidate) { c.Args = []string{"a | b"} }},
		{"shell metacharacter substitution", func(c *Candidate) { c.Args = []string{"$(whoami)"} }},
		{"shell metacharacter backtick", func(c *Candidate) { c.Args = []string{"`id`"} }},
		{"shell metacharacter redirect", func(c *Candidate) { c.Args = []string{"x > /etc/passwd"} }},
		{"escaping transient path", func(c *Candidate) { c.TransientWritePaths = []string{"../outside"} }},
		{"absolute transient path", func(c *Candidate) { c.TransientWritePaths = []string{"/tmp/x"} }},
		{"secret env name", func(c *Candidate) { c.Env = []string{"GITHUB_TOKEN"} }},
		{"invalid command id", func(c *Candidate) { c.CommandID = "Bad ID" }},
		{"invalid command id uppercase", func(c *Candidate) { c.CommandID = "Verify" }},
		{"empty executable", func(c *Candidate) { c.Executable = "" }},
		{"malformed sha256", func(c *Candidate) { c.SHA256 = "zzz" }},
		{"zero timeout", func(c *Candidate) { c.TimeoutSeconds = 0 }},
		{"empty expected exits", func(c *Candidate) { c.ExpectedExitCodes = nil }},
		{"invalid purpose", func(c *Candidate) { c.Purpose = "arbitrary" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCandidate()
			tc.mutate(&c)
			if err := ValidateCandidate(c); err == nil {
				t.Fatalf("candidate %s should be rejected", c.CommandID)
			}
		})
	}
}

// TestDiscoverWrappersFindsBaseCommitCommands: the deterministic
// discovery finds the fixed wrapper set at the repository root and binds
// each wrapper to its fixed purpose and content hash.
func TestDiscoverWrappersFindsBaseCommitCommands(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("scripts/verify.sh", "#!/bin/sh\nexit 0\n")
	write("scripts/final-verify.sh", "#!/bin/sh\n./verify.sh\n")
	write("mvnw", "#!/bin/sh\necho maven\n")

	cands, err := DiscoverWrappers(root)
	if err != nil {
		t.Fatalf("discover wrappers: %v", err)
	}
	byID := map[string]Candidate{}
	for _, c := range cands {
		byID[c.CommandID] = c
	}
	if len(byID) != 3 {
		t.Fatalf("discovered %d wrappers, want 3: %v", len(byID), idsOf(cands))
	}
	if c := byID["verify"]; c.Purpose != PurposeTaskVerify || c.Executable != "scripts/verify.sh" {
		t.Fatalf("verify candidate = %+v", c)
	}
	if c := byID["final-verify"]; c.Purpose != PurposeFinalVerify || c.Executable != "scripts/final-verify.sh" {
		t.Fatalf("final-verify candidate = %+v", c)
	}
	if c := byID["mvnw"]; c.Purpose != PurposeTaskVerify || c.Executable != "mvnw" {
		t.Fatalf("mvnw candidate = %+v", c)
	}
	// Repository wrappers are hashed from the base snapshot.
	want := fileHash(t, filepath.Join(root, "scripts", "verify.sh"))
	if byID["verify"].SHA256 != want {
		t.Fatalf("verify hash = %s, want the file content hash %s", byID["verify"].SHA256, want)
	}
	// Every discovered wrapper passes the policy.
	for _, c := range cands {
		if err := ValidateCandidate(c); err != nil {
			t.Fatalf("discovered candidate %s fails policy: %v", c.CommandID, err)
		}
	}
}

// TestDiscoverPathExecutablesResolvesAndHashes: PATH executables are
// resolved to their absolute path and the binary is hashed; the fixed
// set only produces candidates when the executable is actually on PATH.
func TestDiscoverPathExecutablesResolvesAndHashes(t *testing.T) {
	cands, err := DiscoverPathExecutables()
	if err != nil {
		t.Fatalf("discover path executables: %v", err)
	}
	for _, c := range cands {
		if c.ExecutableKind != KindPathExecutable || !filepath.IsAbs(c.Executable) {
			t.Fatalf("path executable candidate = %+v", c)
		}
		if _, err := os.Stat(c.Executable); err != nil {
			t.Fatalf("resolved executable %s does not exist: %v", c.Executable, err)
		}
		if len(c.SHA256) != 64 {
			t.Fatalf("path executable %s has no binary hash", c.CommandID)
		}
		if err := ValidateCandidate(c); err != nil {
			t.Fatalf("path executable candidate %s fails policy: %v", c.CommandID, err)
		}
	}
}

func idsOf(cands []Candidate) []string {
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.CommandID)
	}
	return ids
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
