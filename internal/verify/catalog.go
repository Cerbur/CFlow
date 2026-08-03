// Package verify hosts the Verification Command Catalog policy (design
// 16.1, PRD 已确认：Workflow-local Verification Command Catalog): the
// candidate policy and the deterministic discovery of candidates from
// the fixed Base Commit manifests/wrappers and the PATH executables.
//
// Discovery only produces Candidates: a Candidate is not executable
// until it is included in an approved immutable Catalog revision. The
// policy accepts executable plus argv only and rejects shells, inline
// code execution, publish/deploy, destructive Git, system management,
// escaped cwd, secret-like values, and escaping transient paths.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/internal/model"
)

// Purpose is the closed set of Catalog verification purposes
// (catalog.json enum).
type Purpose string

const (
	PurposeTaskVerify        Purpose = "task_verify"
	PurposeIntegrationVerify Purpose = "integration_verify"
	PurposeFinalVerify       Purpose = "final_verify"
	PurposeApplyVerify       Purpose = "apply_verify"
)

// Valid reports whether p is a declared Catalog Purpose.
func (p Purpose) Valid() bool {
	switch p {
	case PurposeTaskVerify, PurposeIntegrationVerify, PurposeFinalVerify, PurposeApplyVerify:
		return true
	}
	return false
}

// ExecutableKind distinguishes repository wrappers (project-relative,
// hashed from the Base Commit) from PATH-resolved executables (absolute
// path and binary hash pinned at Approval Preview).
type ExecutableKind string

const (
	KindProjectRelative ExecutableKind = "project_relative"
	KindPathExecutable  ExecutableKind = "path_executable"
)

// Candidate is one discovered or proposed verification command. It is a
// Candidate, never an executable command: only an approved immutable
// Catalog revision makes it runnable (design 16.1).
type Candidate struct {
	CommandID           string
	Purpose             Purpose
	ExecutableKind      ExecutableKind
	Executable          string
	SHA256              string
	Args                []string
	CWD                 string
	TimeoutSeconds      int
	ExpectedExitCodes   []int
	OutputLimitBytes    int
	Env                 []string
	TransientWritePaths []string
	Source              string
}

// ValidateCandidate applies the command policy to one Candidate. A
// rejection means the command cannot enter the Catalog; the policy is a
// boundary, never a sandbox guarantee.
func ValidateCandidate(c Candidate) error {
	if !nodeIDRE.MatchString(c.CommandID) {
		return reject(c, "command id must match the command id form")
	}
	if !c.Purpose.Valid() {
		return reject(c, "unknown verification purpose")
	}
	if c.Executable == "" {
		return reject(c, "executable is required")
	}
	base := filepath.Base(c.Executable)
	if shellDenied[base] {
		return reject(c, "shell interpreter executables are not allowed")
	}
	if err := validateCWD(c.CWD); err != nil {
		return reject(c, err.Error())
	}
	if err := validateArgs(c); err != nil {
		return reject(c, err.Error())
	}
	if err := validateEnv(c.Env); err != nil {
		return reject(c, err.Error())
	}
	if err := validateTransientPaths(c.TransientWritePaths); err != nil {
		return reject(c, err.Error())
	}
	if err := validateHash(c.SHA256); err != nil {
		return reject(c, "executable hash must be a 64-hex sha256")
	}
	if c.TimeoutSeconds < 1 {
		return reject(c, "timeout must be positive")
	}
	if len(c.ExpectedExitCodes) == 0 {
		return reject(c, "expected exit codes are required")
	}
	if c.OutputLimitBytes < 1 {
		return reject(c, "output limit must be positive")
	}
	return nil
}

// ---------------------------------------------------------------------------
// policy building blocks
// ---------------------------------------------------------------------------

var nodeIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// shellDenied: shell interpreters can never be Catalog executables.
var shellDenied = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"csh": true, "tcsh": true, "fish": true, "ash": true,
}

// systemManagers: package/system management tools are never verification
// commands (the demo's default policy, PRD 约束).
var systemManagers = map[string]bool{
	"systemctl": true, "service": true, "launchctl": true, "initctl": true,
	"apt": true, "apt-get": true, "yum": true, "dnf": true, "pacman": true,
	"snap": true, "flatpak": true, "rpm": true, "dpkg": true, "pkgadd": true,
	"brew": true,
}

// interpreters: inline code execution flags (-c/-e/-r) turn an
// interpreter into arbitrary code execution and are rejected.
var interpreters = map[string]bool{
	"python": true, "python2": true, "python3": true, "node": true,
	"perl": true, "ruby": true, "php": true, "deno": true, "bun": true,
}

// denyPairs: tool/subcommand pairs that publish, deploy, or push
// artifacts and can never run as verification commands.
var denyPairs = map[string][]string{
	"npm":       {"publish"},
	"yarn":      {"publish"},
	"pnpm":      {"publish"},
	"cargo":     {"publish"},
	"docker":    {"push", "deploy", "login"},
	"git":       {"push"},
	"gh":        {"release"},
	"aws":       {"deploy"},
	"kubectl":   {"apply", "create", "delete", "rollout"},
	"terraform": {"apply", "destroy"},
	"helm":      {"install", "push", "upgrade"},
}

// destructiveGit: destructive Git operations can never be verification
// commands.
var destructiveGit = map[string]bool{
	"reset": true, "clean": true, "rebase": true,
	"filter-branch": true, "gc": true,
}

// secretEnvNames: environment names that hold secrets can never be
// declared on a Catalog entry (a name that CONTAINS one of these is
// rejected too: GITHUB_TOKEN, AWS_ACCESS_KEY, ...).
var secretEnvNames = []string{
	"TOKEN", "PASSWORD", "PASSWD", "SECRET", "APIKEY", "API_KEY",
	"PRIVATE_KEY", "ACCESS_KEY", "CREDENTIAL", "AUTHORIZATION", "BEARER",
}

// secretArgPatterns: values that look like secrets can never appear in
// Catalog argv.
var secretArgPatterns = []string{
	"password", "passwd", "token", "secret", "apikey", "api_key",
	"private_key", "private-key", "credential", "bearer",
	"begin private key",
}

func validateCWD(cwd string) error {
	if cwd == "" {
		return fmt.Errorf("cwd is required")
	}
	if filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must stay inside the managed worktree")
	}
	for _, part := range strings.Split(filepath.ToSlash(cwd), "/") {
		if part == ".." {
			return fmt.Errorf("cwd must not escape the managed worktree")
		}
	}
	return nil
}

func validateArgs(c Candidate) error {
	base := filepath.Base(c.Executable)
	if systemManagers[base] {
		return fmt.Errorf("system management tools are not allowed")
	}
	for _, arg := range c.Args {
		if strings.ContainsAny(arg, ";|&`<>") || strings.Contains(arg, "$(") {
			return fmt.Errorf("argv must not carry shell metacharacters")
		}
		lower := strings.ToLower(arg)
		for _, pattern := range secretArgPatterns {
			if strings.Contains(lower, pattern) {
				return fmt.Errorf("argv must not carry secret-like values")
			}
		}
	}
	if len(c.Args) == 0 {
		return nil
	}
	first := c.Args[0]
	if interpreters[base] {
		switch first {
		case "-c", "-e", "-r", "-m":
			return fmt.Errorf("inline code execution flags are not allowed")
		}
	}
	if base == "go" && first == "run" {
		return fmt.Errorf("inline code execution is not allowed")
	}
	if denied, ok := denyPairs[base]; ok {
		for _, sub := range denied {
			if first == sub {
				return fmt.Errorf("publish/deploy operations are not allowed")
			}
		}
	}
	if base == "git" {
		if destructiveGit[first] {
			return fmt.Errorf("destructive git operations are not allowed")
		}
		if first == "checkout" && hasForceFlag(c.Args[1:]) {
			return fmt.Errorf("destructive git operations are not allowed")
		}
		if first == "branch" && hasDeleteFlag(c.Args[1:]) {
			return fmt.Errorf("destructive git operations are not allowed")
		}
	}
	return nil
}

func hasForceFlag(args []string) bool {
	for _, a := range args {
		if a == "--force" || a == "-f" || a == "--hard" {
			return true
		}
	}
	return false
}

func hasDeleteFlag(args []string) bool {
	for _, a := range args {
		if a == "-D" || a == "--delete" {
			return true
		}
	}
	return false
}

func validateEnv(env []string) error {
	for _, name := range env {
		upper := strings.ToUpper(name)
		for _, secret := range secretEnvNames {
			if strings.Contains(upper, secret) {
				return fmt.Errorf("environment must not carry secret names")
			}
		}
	}
	return nil
}

func validateTransientPaths(paths []string) error {
	for _, p := range paths {
		if err := validateCWD(p); err != nil {
			return fmt.Errorf("transient path must stay inside the managed worktree")
		}
	}
	return nil
}

func validateHash(hash string) error {
	if hash == "" {
		return nil // the Catalog body hash binds the identity
	}
	if len(hash) != 64 {
		return fmt.Errorf("invalid hash")
	}
	for _, r := range hash {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return fmt.Errorf("invalid hash")
		}
	}
	return nil
}

func reject(c Candidate, reason string) error {
	return model.InvalidInputFault(fmt.Sprintf("catalog candidate %q rejected: %s", c.CommandID, reason))
}

// ---------------------------------------------------------------------------
// deterministic discovery (PRD: from the fixed Base Commit manifests and
// wrappers; PATH executables resolved to absolute path and hash)
// ---------------------------------------------------------------------------

// wrapperCandidate is one fixed repository wrapper path and the purpose
// it binds when discovered at the Base Commit.
type wrapperCandidate struct {
	Path    string
	Purpose Purpose
}

// wrapperPaths is the deterministic repository manifest/wrapper scan
// list at the fixed Base Commit. Order defines nothing: candidates are
// emitted sorted by command id.
var wrapperPaths = []wrapperCandidate{
	{Path: "scripts/verify.sh", Purpose: PurposeTaskVerify},
	{Path: "scripts/check.sh", Purpose: PurposeTaskVerify},
	{Path: "scripts/test.sh", Purpose: PurposeTaskVerify},
	{Path: "scripts/final-verify.sh", Purpose: PurposeFinalVerify},
	{Path: "scripts/integration-verify.sh", Purpose: PurposeIntegrationVerify},
	{Path: "mvnw", Purpose: PurposeTaskVerify},
	{Path: "gradlew", Purpose: PurposeTaskVerify},
}

// pathExecutables is the deterministic PATH executable set: each entry
// is resolved through PATH to its absolute path and the binary is
// hashed. A missing executable simply produces no candidate.
var pathExecutables = []wrapperCandidate{
	{Path: "go", Purpose: PurposeTaskVerify},
}

// demo command defaults for discovered candidates (PRD Catalog 数据
// 格式): the repository cwd, the deterministic command timeout, output
// bound, environment names, and expected exit codes.
const (
	defaultTimeoutSeconds   = 600
	defaultOutputLimitBytes = 10485760
)

var defaultEnv = []string{"PATH", "TMPDIR", "LANG", "LC_ALL"}

// DiscoverWrappers discovers the repository wrapper candidates at root
// (the Planning Snapshot Worktree, fixed at the Base Commit) and hashes
// each wrapper from the Base snapshot.
func DiscoverWrappers(root string) ([]Candidate, error) {
	var out []Candidate
	for _, w := range wrapperPaths {
		path := filepath.Join(root, filepath.FromSlash(w.Path))
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		out = append(out, Candidate{
			CommandID:         stem(w.Path),
			Purpose:           w.Purpose,
			ExecutableKind:    KindProjectRelative,
			Executable:        w.Path,
			SHA256:            sha256Hex(data),
			CWD:               ".",
			TimeoutSeconds:    defaultTimeoutSeconds,
			ExpectedExitCodes: []int{0},
			OutputLimitBytes:  defaultOutputLimitBytes,
			Env:               append([]string(nil), defaultEnv...),
			Source:            fmt.Sprintf("base-commit-wrapper:%s@sha256:%s", w.Path, sha256Hex(data)),
		})
	}
	sortCandidates(out)
	return out, nil
}

// DiscoverPathExecutables resolves the fixed PATH executable set to
// absolute paths and binary hashes.
func DiscoverPathExecutables() ([]Candidate, error) {
	var out []Candidate
	for _, w := range pathExecutables {
		abs, err := exec.LookPath(w.Path)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, err
		}
		out = append(out, Candidate{
			CommandID:         stem(w.Path),
			Purpose:           w.Purpose,
			ExecutableKind:    KindPathExecutable,
			Executable:        abs,
			SHA256:            sha256Hex(data),
			Args:              []string{"test", "./..."},
			CWD:               ".",
			TimeoutSeconds:    defaultTimeoutSeconds,
			ExpectedExitCodes: []int{0},
			OutputLimitBytes:  defaultOutputLimitBytes,
			Env:               append([]string(nil), defaultEnv...),
			Source:            fmt.Sprintf("path-executable:%s@sha256:%s", abs, sha256Hex(data)),
		})
	}
	sortCandidates(out)
	return out, nil
}

// stem derives the deterministic command id from a wrapper path:
// scripts/verify.sh -> verify, mvnw -> mvnw, go -> go.
func stem(path string) string {
	base := filepath.Base(filepath.ToSlash(path))
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func sortCandidates(cands []Candidate) {
	sort.Slice(cands, func(i, j int) bool { return cands[i].CommandID < cands[j].CommandID })
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// catalog body assembly (catalog.json)
// ---------------------------------------------------------------------------

// CatalogBody assembles the canonical immutable Catalog artifact body
// for one revision from validated candidates. Entries are sorted by
// command id; every candidate must pass the policy.
func CatalogBody(revision int, candidates []Candidate) ([]byte, error) {
	if revision < 1 {
		return nil, model.InvalidInputFault("catalog revision must be positive")
	}
	if len(candidates) == 0 {
		return nil, model.InvalidInputFault("catalog requires at least one entry")
	}
	entries := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		if err := ValidateCandidate(c); err != nil {
			return nil, err
		}
		entries = append(entries, map[string]any{
			"command_id":            c.CommandID,
			"executable":            c.Executable,
			"args":                  strSlice(c.Args),
			"cwd":                   c.CWD,
			"purpose":               string(c.Purpose),
			"timeout_seconds":       c.TimeoutSeconds,
			"expected_exit_codes":   intSlice(c.ExpectedExitCodes),
			"max_output_bytes":      c.OutputLimitBytes,
			"env":                   strSlice(c.Env),
			"transient_write_paths": strSlice(c.TransientWritePaths),
			"source":                c.Source,
		})
	}
	body, err := yaml.Marshal(map[string]any{"revision": revision, "entries": entries})
	if err != nil {
		return nil, model.InvariantFault(err)
	}
	return body, nil
}

func strSlice(in []string) []string {
	if in == nil {
		return []string{}
	}
	return append([]string(nil), in...)
}

func intSlice(in []int) []int {
	if in == nil {
		return []int{}
	}
	return append([]int(nil), in...)
}
