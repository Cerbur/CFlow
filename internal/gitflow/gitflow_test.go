// Package gitflow_test exercises the GitFlow module against real temporary
// repositories created through the Process Supervisor (design 22.1: Git
// facts are tested with real Git, never with a mocked binary). Every test
// repository is created under its own temporary directory and runs with
// system and global Git configs disabled, so a developer's real
// ~/.gitconfig can never leak into test facts.
package gitflow_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// testEnv is the deterministic environment every test Git process runs
// under: system and global Git configs are disabled so the host user's
// configuration can never influence test facts, and terminal prompts are
// impossible.
func testEnv() map[string]string {
	return map[string]string{
		"PATH":                os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_TERMINAL_PROMPT": "0",
	}
}

// Repo is one real temporary Git repository used as test fixture. All Git
// commands run through the Process Supervisor with an argv-only spec and
// the isolated test environment; no shell is ever involved.
type Repo struct {
	t    *testing.T
	sup  process.Supervisor
	Git  string // absolute git executable
	Tmp  string // canonical temporary root (parent of Root)
	WTs  string // CFlow-managed worktree root for this fixture (0700)
	Root string // canonical repository root (the user's working tree)
}

// resolveGit finds the absolute git executable once per fixture.
func resolveGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git executable not found: %v", err)
	}
	abs, err := filepath.Abs(git)
	if err != nil {
		t.Fatalf("resolve git path: %v", err)
	}
	return abs
}

// newRepo creates and initializes a temporary repository on branch main.
// The branch is passed explicitly because the isolated test environment
// has no init.defaultbranch.
func newRepo(t *testing.T) *Repo {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	canon, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize temp dir: %v", err)
	}
	r := &Repo{
		t:    t,
		sup:  process.NewSupervisor(process.NewOSAdapter()),
		Git:  resolveGit(t),
		Tmp:  canon,
		Root: filepath.Join(canon, "repo"),
	}
	r.WTs = filepath.Join(canon, "cflow-worktrees")
	if err := os.Mkdir(r.Root, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if err := os.Mkdir(r.WTs, 0o700); err != nil {
		t.Fatalf("mkdir worktrees root: %v", err)
	}
	r.git("init", "-b", "main", "-q", r.Root)
	r.gitAt(r.Root, "config", "user.name", "Test User")
	r.gitAt(r.Root, "config", "user.email", "test@example.com")
	return r
}

// newCommittedRepo creates a repository with one initial commit.
func newCommittedRepo(t *testing.T) *Repo {
	t.Helper()
	r := newRepo(t)
	r.write("init.txt", "init")
	r.gitAt(r.Root, "add", "init.txt")
	r.gitAt(r.Root, "commit", "-q", "-m", "init")
	return r
}

// newRepoAt creates a repository inside a named subdirectory of the
// temporary root, for discovery tests that exercise non-ASCII or nested
// canonical paths.
func newRepoAt(t *testing.T, name string) *Repo {
	t.Helper()
	r := newRepo(t)
	// Relocate the repository into the named subdirectory.
	nested := filepath.Join(r.Tmp, name)
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	if err := os.Rename(r.Root, filepath.Join(nested, "repo")); err != nil {
		t.Fatalf("move repo into %s: %v", name, err)
	}
	r.Root = filepath.Join(nested, "repo")
	r.git("init", "-b", "main", "-q", r.Root)
	r.gitAt(r.Root, "config", "user.name", "Test User")
	r.gitAt(r.Root, "config", "user.email", "test@example.com")
	return r
}

// Path returns an absolute path inside the repository.
func (r *Repo) Path(rel string) string {
	return filepath.Join(r.Root, filepath.FromSlash(rel))
}

// WtPath returns an absolute managed-worktree path under the fixture's
// worktree root.
func (r *Repo) WtPath(name string) string {
	return filepath.Join(r.WTs, name)
}

// flow returns a GitFlow bound to the repository root.
func (r *Repo) flow() *gitflow.GitFlow {
	f, err := gitflow.NewGitFlow(r.sup, r.Root)
	if err != nil {
		r.t.Fatalf("new gitflow: %v", err)
	}
	return f
}

// flowAt returns a GitFlow bound to an arbitrary canonical directory
// (nested current-directory discovery).
func (r *Repo) flowAt(dir string) *gitflow.GitFlow {
	f, err := gitflow.NewGitFlow(r.sup, dir)
	if err != nil {
		r.t.Fatalf("new gitflow at %s: %v", dir, err)
	}
	return f
}

// write creates a file at rel with content.
func (r *Repo) write(rel, content string) {
	writeFile(r.t, r.Path(rel), content)
}

// git runs one supervised git command in the repository root and fails
// the test on any non-zero exit.
func (r *Repo) git(args ...string) []byte {
	return r.gitAt(r.Root, args...)
}

// gitAt runs one supervised git command in dir.
func (r *Repo) gitAt(dir string, args ...string) []byte {
	out, exit, err := r.runGit(dir, nil, args...)
	if err != nil {
		r.t.Fatalf("git %v: %v", args, err)
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		r.t.Fatalf("git %v exited %+v: %s", args, exit, out)
	}
	return out
}

// gitOK runs one supervised git command and reports whether it exited 0,
// returning stdout for data-carrying non-zero exits.
func (r *Repo) gitOK(dir string, args ...string) ([]byte, bool) {
	out, exit, err := r.runGit(dir, nil, args...)
	if err != nil {
		r.t.Fatalf("git %v: %v", args, err)
	}
	return out, exit.Fact == process.FactProcessExit && exit.Code == 0
}

// runGit launches git through the Supervisor with the isolated test
// environment and a bounded timeout.
func (r *Repo) runGit(dir string, extra map[string]string, args ...string) ([]byte, process.Exit, error) {
	env := testEnv()
	for k, v := range extra {
		env[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return runProcess(ctx, r.sup, r.Git, dir, env, args...)
}

// mustObserve observes q against the repo-bound GitFlow and returns the
// concrete ProjectFacts.
func mustObserve(t *testing.T, repo *Repo, q gitflow.GitQuery) gitflow.ProjectFacts {
	t.Helper()
	facts, err := repo.flow().Observe(context.Background(), q)
	if err != nil {
		t.Fatalf("observe %T: %v", q, err)
	}
	pf, ok := facts.(gitflow.ProjectFacts)
	if !ok {
		t.Fatalf("observe %T returned %T, want ProjectFacts", q, facts)
	}
	return pf
}

// mustExecute runs one Git operation through the repo-bound GitFlow,
// injecting fixture defaults for empty worktree paths, branches, and
// preflight revisions.
func mustExecute(t *testing.T, repo *Repo, op gitflow.GitOperation) gitflow.GitResult {
	t.Helper()
	switch o := op.(type) {
	case gitflow.CreatePlanningSnapshot:
		if o.Path == "" {
			o.Path = repo.WtPath("planning")
		}
		op = o
	case gitflow.CreateIntegration:
		if o.Path == "" {
			o.Path = repo.WtPath("integration")
		}
		if o.Branch == "" {
			o.Branch = "cflow/wf-test/integration"
		}
		op = o
	case gitflow.CreateTask:
		if o.Path == "" {
			o.Path = repo.WtPath("task")
		}
		if o.Branch == "" {
			o.Branch = "cflow/wf-test/task"
		}
		op = o
	case gitflow.CommitPreflight:
		if o.Revision == "" {
			o.Revision = "test-1"
		}
		op = o
	}
	res, err := repo.flow().Execute(context.Background(), op)
	if err != nil {
		t.Fatalf("execute %T: %v", op, err)
	}
	return res
}

// faultCode extracts the model fault code from err, failing the test when
// err is not a model fault.
func faultCode(t *testing.T, err error) model.Code {
	t.Helper()
	var f *model.Fault
	if !errors.As(err, &f) {
		t.Fatalf("error is %T %v, want model.Fault", err, err)
	}
	return f.Code
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func requireFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s content = %q, want %q", path, got, want)
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func gitVersion(t *testing.T, repo *Repo) string {
	t.Helper()
	out := repo.git("--version")
	return strings.TrimSpace(string(out))
}

// ---------------------------------------------------------------------------
// Project discovery
// ---------------------------------------------------------------------------

func TestProjectDiscoveryFromNestedDirectory(t *testing.T) {
	repo := newCommittedRepo(t)
	nested := filepath.Join(repo.Root, "src", "deep", "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	facts := mustObserve(t, repo, gitflow.ProjectDiscovery{})
	wantRoot := repo.Root
	if facts.Root != wantRoot {
		t.Fatalf("Root = %q, want %q", facts.Root, wantRoot)
	}
	if facts.Branch != "main" {
		t.Fatalf("Branch = %q, want main", facts.Branch)
	}
	if facts.Unborn || facts.Detached {
		t.Fatalf("committed repo misclassified: unborn=%v detached=%v", facts.Unborn, facts.Detached)
	}
	if !isFullHex(facts.Head) {
		t.Fatalf("Head = %q, want full hex", facts.Head)
	}
	// The nested-directory discovery finds the same canonical root.
	nestedFacts, err := repo.flowAt(nested).Observe(context.Background(), gitflow.ProjectDiscovery{})
	if err != nil {
		t.Fatalf("observe from nested dir: %v", err)
	}
	nf := nestedFacts.(gitflow.ProjectFacts)
	if nf.Root != wantRoot || nf.Head != facts.Head {
		t.Fatalf("nested discovery Root=%q Head=%q, want %q %q", nf.Root, nf.Head, wantRoot, facts.Head)
	}
}

func TestProjectDiscoveryNonGitDirectory(t *testing.T) {
	repo := newRepo(t)
	plain := filepath.Join(repo.Tmp, "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := repo.flowAt(plain).Observe(context.Background(), gitflow.ProjectDiscovery{})
	if code := faultCode(t, err); code != model.CodeInvalidInput {
		t.Fatalf("non-git discovery code = %s, want INVALID_INPUT", code)
	}
}

func TestProjectDiscoveryUnbornRepository(t *testing.T) {
	repo := newRepo(t) // initialized, no commits
	facts := mustObserve(t, repo, gitflow.ProjectDiscovery{})
	if !facts.Unborn {
		t.Fatal("unborn repository not classified as Unborn")
	}
	if facts.Head != "" {
		t.Fatalf("unborn Head = %q, want empty", facts.Head)
	}
	if facts.Branch != "main" {
		t.Fatalf("unborn Branch = %q, want the symbolic-ref branch", facts.Branch)
	}
	if facts.Detached {
		t.Fatal("unborn repository must not be classified Detached")
	}
}

func TestProjectDiscoveryDetachedHead(t *testing.T) {
	repo := newCommittedRepo(t)
	head := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	repo.git("checkout", "-q", "--detach", "HEAD")
	facts := mustObserve(t, repo, gitflow.ProjectDiscovery{})
	if !facts.Detached {
		t.Fatal("detached HEAD not classified as Detached")
	}
	if facts.Branch != "" {
		t.Fatalf("detached Branch = %q, want empty", facts.Branch)
	}
	if facts.Head != head {
		t.Fatalf("detached Head = %q, want %q", facts.Head, head)
	}
}

func TestProjectDiscoveryNonASCIIRepoRoot(t *testing.T) {
	repo := newRepoAt(t, "数据-工作区-café")
	repo.write("init.txt", "init")
	repo.git("add", "init.txt")
	repo.git("commit", "-q", "-m", "init")

	facts := mustObserve(t, repo, gitflow.ProjectDiscovery{})
	if !strings.Contains(facts.Root, "数据-工作区") {
		t.Fatalf("Root = %q, want the non-ASCII segment", facts.Root)
	}
	if facts.ProjectKey != gitflow.ProjectKey(facts.Root) {
		t.Fatalf("ProjectKey = %q, want %q", facts.ProjectKey, gitflow.ProjectKey(facts.Root))
	}
	if facts.Head == "" || facts.Unborn {
		t.Fatalf("non-ASCII repo misclassified: %+v", facts)
	}
}

func TestProjectDiscoveryCanonicalRoot(t *testing.T) {
	repo := newCommittedRepo(t)
	facts := mustObserve(t, repo, gitflow.ProjectDiscovery{})
	want, err := filepath.EvalSymlinks(repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Root != want {
		t.Fatalf("Root = %q, want canonical %q", facts.Root, want)
	}
	wantKey := gitflow.ProjectKey(facts.Root)
	if facts.ProjectKey != wantKey {
		t.Fatalf("ProjectKey = %q, want %q", facts.ProjectKey, wantKey)
	}
	if !strings.HasPrefix(facts.ProjectKey, slugOf(facts.Root)+"--") {
		t.Fatalf("ProjectKey %q does not carry the readable slug", facts.ProjectKey)
	}
}

// slugOf mirrors the PRD slug rule for ASCII-safe path components.
func slugOf(root string) string {
	norm := root
	trimmed := strings.TrimPrefix(norm, "/")
	withDashes := strings.ReplaceAll(trimmed, string(filepath.Separator), "-")
	var b strings.Builder
	for _, r := range withDashes {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func isFullHex(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Project Key
// ---------------------------------------------------------------------------

func TestProjectKeyCollisionPair(t *testing.T) {
	// The PRD collision pair maps to the same slug but different hashes,
	// so the full keys never collide.
	a := gitflow.ProjectKey("/a-b/c")
	b := gitflow.ProjectKey("/a/b-c")
	if a == b {
		t.Fatalf("collision pair produced identical key %q", a)
	}
	if !strings.HasPrefix(a, "a-b-c--") || !strings.HasPrefix(b, "a-b-c--") {
		t.Fatalf("collision pair slugs = %q %q, want both a-b-c--", a, b)
	}
}

func TestProjectKeyPRDExample(t *testing.T) {
	path := "/Users/yuancheng/Documents/Code/Resume"
	key := gitflow.ProjectKey(path)
	want := "Users-yuancheng-Documents-Code-Resume--" + sha256Hex(path)[:10]
	if key != want {
		t.Fatalf("ProjectKey(%q) = %q, want %q", path, key, want)
	}
}

func TestProjectKeyUnicodeNFCEquivalence(t *testing.T) {
	// Composed and decomposed forms of the same path normalize to the
	// same key (PRD: normalizedPath = Unicode NFC(canonicalPath)).
	composed := "/Users/yuancheng/数据/café"
	decomposed := "/Users/yuancheng/数据/café"
	a := gitflow.ProjectKey(composed)
	b := gitflow.ProjectKey(decomposed)
	if a != b {
		t.Fatalf("NFC forms differ: %q vs %q", a, b)
	}
	if a == "" || !strings.Contains(a, "--") {
		t.Fatalf("key %q malformed", a)
	}
}

func TestProjectKeyNonASCIIPath(t *testing.T) {
	path := "/Users/yuancheng/Documents/数据-工作区"
	key := gitflow.ProjectKey(path)
	if !strings.HasPrefix(key, "Users-yuancheng-Documents-") {
		t.Fatalf("non-ASCII slug mangled: %q", key)
	}
	want := slugOf(path) + "--" + sha256Hex(path)[:10]
	if key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
}

func TestProjectKeyTruncation(t *testing.T) {
	long := "/" + strings.Repeat("segment-", 20) // 161 chars
	key := gitflow.ProjectKey(long)
	// The key always ends with "--" plus ten hex characters; the slug may
	// itself contain "--" (repeated components), so it is parsed from the
	// tail, never by splitting on the first separator.
	slug := key[:len(key)-12]
	if len(slug) != 80 {
		t.Fatalf("slug truncated to %d, want 80", len(slug))
	}
	if !strings.HasSuffix(key, "--"+sha256Hex(long)[:10]) {
		t.Fatalf("key %q does not carry the canonical-path hash", key)
	}
}

// ---------------------------------------------------------------------------
// Status facts and Dirty Fingerprint
// ---------------------------------------------------------------------------

func TestStatusClassification(t *testing.T) {
	repo := newCommittedRepo(t)
	repo.write("tracked.txt", "v1")
	repo.git("add", "tracked.txt")
	repo.git("commit", "-q", "-m", "add tracked")
	repo.write("staged.txt", "staged")
	repo.git("add", "staged.txt")
	repo.write("tracked.txt", "v2-dirty")
	repo.write("untracked.txt", "new")
	if err := os.Mkdir(repo.Path("dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo.Path("dir/a.txt"), "a")
	writeFile(t, repo.Path("dir/b.txt"), "b")

	facts := mustObserve(t, repo, gitflow.ProjectDiscovery{})
	staged := findPath(facts.Status.Staged, "staged.txt")
	if staged == nil || staged.X != 'A' {
		t.Fatalf("staged.txt not classified staged: %+v", facts.Status.Staged)
	}
	unstaged := findPath(facts.Status.Unstaged, "tracked.txt")
	if unstaged == nil || unstaged.Y != 'M' {
		t.Fatalf("tracked.txt not classified unstaged: %+v", facts.Status.Unstaged)
	}
	if findPath(facts.Status.Untracked, "untracked.txt") == nil {
		t.Fatalf("untracked.txt missing: %+v", facts.Status.Untracked)
	}
	if findPath(facts.Status.Untracked, "dir") == nil {
		t.Fatalf("untracked dir missing: %+v", facts.Status.Untracked)
	}
	if facts.Status.Dirty.StagedCount != 1 || facts.Status.Dirty.UnstagedCount != 1 || facts.Status.Dirty.UntrackedCount != 2 {
		t.Fatalf("dirty counts = %+v, want staged=1 unstaged=1 untracked=2", facts.Status.Dirty)
	}
}

func findPath(entries []gitflow.PathEntry, path string) *gitflow.PathEntry {
	for i := range entries {
		if entries[i].Path == path {
			return &entries[i]
		}
	}
	return nil
}

func TestDirtyFingerprintStableAcrossRuns(t *testing.T) {
	repo := newCommittedRepo(t)
	repo.write("tracked.txt", "v1")
	repo.git("add", "tracked.txt")
	repo.git("commit", "-q", "-m", "add tracked")
	repo.write("tracked.txt", "v2")
	repo.write("untracked.txt", "payload")
	if err := os.Mkdir(repo.Path("d"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo.Path("d/x.txt"), "x")
	writeFile(t, repo.Path("d/y.txt"), "y")

	a := mustObserve(t, repo, gitflow.ProjectDiscovery{})
	b := mustObserve(t, repo, gitflow.ProjectDiscovery{})
	if a.Status.Dirty.Combined != b.Status.Dirty.Combined {
		t.Fatalf("fingerprint not stable: %q vs %q", a.Status.Dirty.Combined, b.Status.Dirty.Combined)
	}
	if a.Status.Dirty.Algorithm == "" {
		t.Fatal("fingerprint has no algorithm identity")
	}
}

func TestDirtyFingerprintTracksContentChanges(t *testing.T) {
	repo := newCommittedRepo(t)
	repo.write("tracked.txt", "v1")
	repo.git("add", "tracked.txt")
	repo.git("commit", "-q", "-m", "add tracked")
	repo.write("untracked.txt", "payload")

	before := mustObserve(t, repo, gitflow.ProjectDiscovery{}).Status.Dirty
	writeFile(t, repo.Path("untracked.txt"), "payload-changed")
	after := mustObserve(t, repo, gitflow.ProjectDiscovery{}).Status.Dirty

	if before.UntrackedHash == after.UntrackedHash {
		t.Fatal("untracked content hash did not change with content")
	}
	if before.Combined == after.Combined {
		t.Fatal("combined fingerprint did not change with untracked content")
	}
	if before.StagedHash != after.StagedHash || before.UnstagedHash != after.UnstagedHash {
		t.Fatal("staged/unstaged hashes changed for an untracked-only edit")
	}
}

func TestDirtyFingerprintTracksUnstagedDiff(t *testing.T) {
	repo := newCommittedRepo(t)
	repo.write("tracked.txt", "v1")
	repo.git("add", "tracked.txt")
	repo.git("commit", "-q", "-m", "add tracked")

	before := mustObserve(t, repo, gitflow.ProjectDiscovery{}).Status.Dirty
	writeFile(t, repo.Path("tracked.txt"), "v2")
	after := mustObserve(t, repo, gitflow.ProjectDiscovery{}).Status.Dirty

	if before.UnstagedHash == after.UnstagedHash {
		t.Fatal("unstaged hash did not change with worktree edits")
	}
}

func TestDirtyFingerprintStagedDiff(t *testing.T) {
	repo := newCommittedRepo(t)
	repo.write("tracked.txt", "v1")
	repo.git("add", "tracked.txt")
	repo.git("commit", "-q", "-m", "add tracked")

	before := mustObserve(t, repo, gitflow.ProjectDiscovery{}).Status.Dirty
	writeFile(t, repo.Path("tracked.txt"), "v2")
	repo.git("add", "tracked.txt")
	after := mustObserve(t, repo, gitflow.ProjectDiscovery{}).Status.Dirty

	if before.StagedHash == after.StagedHash {
		t.Fatal("staged hash did not change with index edits")
	}
	if after.UnstagedHash != before.UnstagedHash {
		t.Fatal("unstaged hash changed when the edit was staged")
	}
}

func TestDirtyFingerprintCleanRepo(t *testing.T) {
	repo := newCommittedRepo(t)
	dirty := mustObserve(t, repo, gitflow.ProjectDiscovery{}).Status.Dirty
	if dirty.StagedCount != 0 || dirty.UnstagedCount != 0 || dirty.UntrackedCount != 0 {
		t.Fatalf("clean repo reported dirty: %+v", dirty)
	}
	// A stable fingerprint of the empty classification.
	again := mustObserve(t, repo, gitflow.ProjectDiscovery{}).Status.Dirty
	if dirty.Combined != again.Combined {
		t.Fatal("clean fingerprint unstable")
	}
}

func TestStatusExpectedHeadMismatch(t *testing.T) {
	repo := newCommittedRepo(t)
	head := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	// A matching expectation observes cleanly.
	if _, err := repo.flow().Observe(context.Background(), gitflow.GitStatus{ExpectedHead: head}); err != nil {
		t.Fatalf("matching expected head: %v", err)
	}
	// A stale expectation fails closed.
	wrong := strings.Repeat("0", len(head))
	_, err := repo.flow().Observe(context.Background(), gitflow.GitStatus{ExpectedHead: wrong})
	if code := faultCode(t, err); code != model.CodeStateInvariantViolation {
		t.Fatalf("expected-head mismatch code = %s, want STATE_INVARIANT_VIOLATION", code)
	}
}

// ---------------------------------------------------------------------------
// Commit facts, refs, history
// ---------------------------------------------------------------------------

func TestCommitInspectFacts(t *testing.T) {
	repo := newCommittedRepo(t)
	repo.write("a.txt", "a")
	repo.git("add", "a.txt")
	repo.git("commit", "-q", "-m", "second commit")
	head := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))

	facts, err := repo.flow().Observe(context.Background(), gitflow.CommitInspect{Ref: "HEAD"})
	if err != nil {
		t.Fatalf("commit inspect: %v", err)
	}
	cf := facts.(gitflow.CommitFacts)
	if cf.Head != head {
		t.Fatalf("commit head = %q, want %q", cf.Head, head)
	}
	if cf.Author.Name != "Test User" || cf.Author.Email != "test@example.com" {
		t.Fatalf("author = %+v, want Test User", cf.Author)
	}
	if cf.Committer != cf.Author {
		t.Fatalf("committer = %+v, want author", cf.Committer)
	}
	if len(cf.Parents) != 1 || !isFullHex(cf.Parents[0]) {
		t.Fatalf("parents = %v, want one full-hex parent", cf.Parents)
	}
	if cf.Subject != "second commit" {
		t.Fatalf("subject = %q", cf.Subject)
	}
	if cf.Signature.Present {
		t.Fatal("unsigned commit reported a signature")
	}
}

func TestCommitInspectSignedCommit(t *testing.T) {
	repo := newCommittedRepo(t)
	key := newSSHKey(t, repo)
	repo.git("config", "gpg.format", "ssh")
	repo.git("config", "user.signingkey", key)
	repo.git("config", "commit.gpgsign", "true")
	repo.git("commit", "-q", "--allow-empty", "-m", "signed")
	facts, err := repo.flow().Observe(context.Background(), gitflow.CommitInspect{Ref: "HEAD"})
	if err != nil {
		t.Fatalf("commit inspect: %v", err)
	}
	if !facts.(gitflow.CommitFacts).Signature.Present {
		t.Fatal("signed commit reported no signature")
	}
}

func TestCommitInspectUnknownRef(t *testing.T) {
	repo := newCommittedRepo(t)
	_, err := repo.flow().Observe(context.Background(), gitflow.CommitInspect{Ref: "does-not-exist"})
	if code := faultCode(t, err); code != model.CodeInvalidInput {
		t.Fatalf("unknown ref code = %s, want INVALID_INPUT", code)
	}
}

func TestRefLookup(t *testing.T) {
	repo := newCommittedRepo(t)
	head := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	facts, err := repo.flow().Observe(context.Background(), gitflow.RefLookup{Ref: "refs/heads/main"})
	if err != nil {
		t.Fatal(err)
	}
	rf := facts.(gitflow.RefFacts)
	if !rf.Exists || rf.Value != head {
		t.Fatalf("main ref facts = %+v, want exists with %q", rf, head)
	}
	// Expected value matching.
	facts, err = repo.flow().Observe(context.Background(), gitflow.RefLookup{Ref: "refs/heads/main", Expected: head})
	if err != nil {
		t.Fatal(err)
	}
	if !facts.(gitflow.RefFacts).Matches {
		t.Fatal("matching expected ref value reported no match")
	}
	// Missing ref.
	facts, err = repo.flow().Observe(context.Background(), gitflow.RefLookup{Ref: "refs/heads/nope"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.(gitflow.RefFacts).Exists {
		t.Fatal("missing ref reported as existing")
	}
}

func TestHistoryRange(t *testing.T) {
	repo := newCommittedRepo(t)
	first := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	repo.write("a.txt", "a")
	repo.git("add", "a.txt")
	repo.git("commit", "-q", "-m", "second")
	second := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	repo.write("b.txt", "b")
	repo.git("add", "b.txt")
	repo.git("commit", "-q", "-m", "third")

	facts, err := repo.flow().Observe(context.Background(), gitflow.HistoryRange{From: first, To: second})
	if err != nil {
		t.Fatal(err)
	}
	rf := facts.(gitflow.RangeFacts)
	if len(rf.Commits) != 1 || rf.Commits[0] != second {
		t.Fatalf("range commits = %v, want [%s]", rf.Commits, second)
	}
	if len(rf.ChangedPaths) != 1 || rf.ChangedPaths[0] != "a.txt" {
		t.Fatalf("range paths = %v, want [a.txt]", rf.ChangedPaths)
	}
}

// ---------------------------------------------------------------------------
// Argv discipline
// ---------------------------------------------------------------------------

func TestGitFlowRejectsCallerInjectedArgv(t *testing.T) {
	repo := newCommittedRepo(t)
	// Branch names that would inject argv or escape the ref namespace are
	// rejected before any git process starts.
	for _, evil := range []string{"main;rm -rf /", "-b", "refs/../..", "a~b", "a^b", "a..b", "a//b", "a b", "a@{b}"} {
		_, err := repo.flow().Execute(context.Background(), gitflow.CreateTask{
			Branch:   evil,
			BaseHead: strings.TrimSpace(string(repo.git("rev-parse", "HEAD"))),
			Path:     repo.WtPath("evil"),
		})
		if code := faultCode(t, err); code != model.CodeInvalidInput {
			t.Fatalf("evil branch %q code = %s, want INVALID_INPUT", evil, code)
		}
		if pathExists(repo.WtPath("evil")) {
			t.Fatalf("evil branch %q created a worktree", evil)
		}
	}
}

// newSSHKey generates a passphrase-less ed25519 signing key fixture and
// returns its path, skipping the test when ssh-keygen is unavailable.
func newSSHKey(t *testing.T, repo *Repo) string {
	t.Helper()
	sshKeygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not available")
	}
	key := filepath.Join(repo.Tmp, "test-key")
	cmd := exec.Command(sshKeygen, "-t", "ed25519", "-N", "", "-C", "cflow-test", "-f", key)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	return key
}

// runProcess starts one argv-only supervised process and returns its
// combined stdout, exit fact, and any launch error.
func runProcess(ctx context.Context, sup process.Supervisor, exe, dir string, env map[string]string, args ...string) ([]byte, process.Exit, error) {
	h, events, err := sup.Start(ctx, process.ProcessSpec{
		Executable: exe,
		Args:       args,
		Dir:        dir,
		Env:        env,
		Timeout:    30 * time.Second,
	})
	if err != nil {
		return nil, process.Exit{}, err
	}
	var out []byte
	for ev := range events {
		switch ev.Kind {
		case process.EventFrameOut:
			out = append(out, ev.Frame...)
			out = append(out, '\n')
		}
	}
	exit, err := sup.Wait(ctx, h)
	return out, exit, err
}
