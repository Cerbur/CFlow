// The Task gate and Verification Engine contract (Task 13, design 15.4
// and 16.2): before any Task may enter Verification the Engine requires a
// clean Worktree, a Commit range that is a descendant of the immutable
// Task Base and append-only from the prior Attempt end, a unique audit
// Ref, a Commit identity/signing match against the approved Preflight,
// and changed paths contained by the Spec write scope. Verification then
// runs the approved Catalog entry with exact executable/argv/cwd/env
// identity, bounded redacted output, and pre/post Git facts. The
// fixtures drive real Git repositories and real supervised processes
// (design 22.1: tests assert through observable facts).
package verify_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/verify"
)

// ---------------------------------------------------------------------------
// small real-git fixture helpers
// ---------------------------------------------------------------------------

// gitRunner runs real git argv in one working directory.
type gitRunner struct {
	t   *testing.T
	dir string
}

func (g gitRunner) git(args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = g.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (g gitRunner) write(rel, content string) {
	g.t.Helper()
	path := filepath.Join(g.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		g.t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		g.t.Fatalf("write %s: %v", path, err)
	}
}

func (g gitRunner) head() string {
	g.t.Helper()
	out, err := exec.Command("git", "-C", g.dir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		g.t.Fatalf("rev-parse HEAD: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo creates a committed repository with a configured identity.
func newRepo(t *testing.T) *gitRunner {
	t.Helper()
	dir := t.TempDir()
	r := gitRunner{t: t, dir: dir}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.name", "Test User")
	r.git("config", "user.email", "test@example.com")
	r.write("README.md", "# fixture\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	return &r
}

// ---------------------------------------------------------------------------
// taskGateFixture: one Task Branch/Worktree with an implementation Commit
// ---------------------------------------------------------------------------

// taskGateFixture owns a repository, a Task Worktree created from the
// immutable Task Base, the approved identity/signing policy the gate
// verifies against, the write scope, the Catalog the verification runs
// through, and a verification-run counter the tests assert on.
type taskGateFixture struct {
	t            *testing.T
	sup          process.Supervisor
	engine       *verify.Engine
	repo         *gitRunner
	baseHead     string
	taskBranch   string
	taskWorktree string
	taskID       string
	writeScope   []string
	author       gitflow.Identity
	committer    gitflow.Identity
	signing      gitflow.SigningPolicy
	catalogBody  []byte
	catalogRef   model.CatalogRef
	commandID    string
	verifyRuns   int
}

// wrapperName is the project-relative wrapper the fixture's Catalog
// entries reference (fixed at the Base Commit).
const wrapperName = "scripts/verify.sh"

// newTaskGateFixture builds the base repository with the default
// verification wrapper, the Task Worktree from the base, and an
// implementation Commit that satisfies the gate by default.
func newTaskGateFixture(t *testing.T) *taskGateFixture {
	t.Helper()
	return newTaskGateFixtureWithWrapper(t, "#!/bin/sh\nexit 0\n")
}

// newTaskGateFixtureWithWrapper builds the fixture with a caller-provided
// wrapper committed at the Base Commit (the wrapper's content is part of
// the approved executable identity; the Catalog pins its hash).
func newTaskGateFixtureWithWrapper(t *testing.T, wrapper string) *taskGateFixture {
	t.Helper()
	return newTaskGateFixtureWithBase(t, wrapper, nil)
}

// newTaskGateFixtureWithBase additionally commits caller-provided base
// files (e.g. a .gitignore the verification-output tests need inside the
// Task Worktree's history).
func newTaskGateFixtureWithBase(t *testing.T, wrapper string, baseFiles map[string]string) *taskGateFixture {
	t.Helper()
	repo := newRepo(t)
	repo.write(wrapperName, wrapper)
	repo.write("src/add.ts", "export const add = (a: number, b: number) => a + b;\n")
	for rel, content := range baseFiles {
		repo.write(rel, content)
	}
	repo.git("add", "-A")
	repo.git("commit", "-q", "-m", "add wrapper and source")

	// t.TempDir() may sit behind a symlink (/var -> /private/var); the
	// worktree registry reports canonical paths.
	canonDir, err := filepath.EvalSymlinks(repo.dir)
	if err != nil {
		t.Fatalf("canonical repo dir: %v", err)
	}
	sup := process.NewSupervisor(process.NewOSAdapter())
	flow, err := gitflow.NewGitFlow(sup, canonDir)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	base := repo.head()

	taskID := "task-s01"
	taskBranch := "cflow/wf-1/task-s01"
	taskWorktree := filepath.Join(canonDir, "wt", taskID)
	repo.git("worktree", "add", "-q", "-b", taskBranch, taskWorktree, base)
	repo.git("-C", taskWorktree, "commit", "--allow-empty", "-q", "-m", "implement")

	// The approved identity/signing policy: the repository's configured
	// identity, unsigned.
	policy := gitflow.SigningPolicy{Enabled: false, Format: "openpgp"}
	identity := gitflow.Identity{Name: "Test User", Email: "test@example.com"}

	body, err := verify.CatalogBody(1, []verify.Candidate{
		{
			CommandID: "verify", Purpose: verify.PurposeTaskVerify,
			ExecutableKind: verify.KindProjectRelative, Executable: wrapperName,
			CWD: ".", TimeoutSeconds: 60, ExpectedExitCodes: []int{0},
			OutputLimitBytes: 4096, Env: []string{"PATH"},
			Source: fmt.Sprintf("base-commit-wrapper:%s@sha256:%s", wrapperName, hashFile(t, filepath.Join(repo.dir, wrapperName))),
		},
	})
	if err != nil {
		t.Fatalf("catalog body: %v", err)
	}
	fx := &taskGateFixture{
		t: t, sup: sup,
		repo: repo, baseHead: base,
		taskBranch: taskBranch, taskWorktree: taskWorktree, taskID: taskID,
		writeScope: []string{"src", "scripts"},
		author:     identity, committer: identity, signing: policy,
		catalogBody: body, catalogRef: model.CatalogRef{Revision: 1, Hash: sha256Hex(t, body)},
		commandID: "verify",
	}
	fx.engine, err = verify.NewEngine(verify.EngineOptions{
		Supervisor: sup, GitFlow: flow,
		LoadCatalog: func(ctx context.Context, ref model.CatalogRef) ([]byte, error) {
			if ref != fx.catalogRef {
				return nil, fmt.Errorf("unexpected catalog ref %+v", ref)
			}
			return fx.catalogBody, nil
		},
	})
	if err != nil {
		t.Fatalf("new verify engine: %v", err)
	}
	return fx
}

// gateRequest is the fixture's standard TaskGate request.
func (fx *taskGateFixture) gateRequest(attempt int, prior string) verify.TaskGateRequest {
	fx.t.Helper()
	return verify.TaskGateRequest{
		WorkflowID:      "wf-1",
		TaskID:          fx.taskID,
		TaskBranch:      fx.taskBranch,
		TaskBase:        fx.baseHead,
		AttemptNumber:   attempt,
		PriorAttemptEnd: prior,
		WriteScope:      append([]string(nil), fx.writeScope...),
		Author:          fx.author,
		Committer:       fx.committer,
		Signing:         fx.signing,
		Worktree:        fx.taskWorktree,
	}
}

// writeTask writes one file into the Task Worktree (the Agent's coded
// output, fixture-side).
func (fx *taskGateFixture) writeTask(rel, content string) {
	fx.t.Helper()
	path := filepath.Join(fx.taskWorktree, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fx.t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		fx.t.Fatalf("write %s: %v", path, err)
	}
}

// commitTask commits the Task Worktree with the fixture identity.
func (fx *taskGateFixture) commitTask(message string) {
	fx.t.Helper()
	fx.repo.git("-C", fx.taskWorktree, "add", "-A")
	fx.repo.git("-C", fx.taskWorktree, "commit", "-q", "-m", message)
}

// WriteUntracked writes one Git-visible untracked file into the Task
// Worktree (the verbatim gate scenario).
func (fx *taskGateFixture) WriteUntracked(rel, content string) {
	fx.t.Helper()
	path := filepath.Join(fx.taskWorktree, filepath.FromSlash(rel))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		fx.t.Fatalf("write untracked %s: %v", path, err)
	}
}

// RequestVerification runs the Task gate and, when the gate passes, one
// Verification run through the fixture Catalog.
func (fx *taskGateFixture) RequestVerification() error {
	fx.t.Helper()
	res, err := fx.engine.TaskGate(context.Background(), fx.gateRequest(1, ""))
	if err != nil {
		return err
	}
	fx.verifyRuns++
	_, err = fx.engine.Run(context.Background(), verify.VerificationRequest{
		Catalog:     fx.catalogRef,
		CommandID:   fx.commandID,
		Purpose:     verify.PurposeTaskVerify,
		Worktree:    fx.taskWorktree,
		CommitRange: fx.baseHead + ".." + res.Head,
	})
	return err
}

// gateAndRun runs the gate and one Verification run, returning the
// Manifest.
func (fx *taskGateFixture) gateAndRun() (model.EvidenceManifest, error) {
	fx.t.Helper()
	res, err := fx.engine.TaskGate(context.Background(), fx.gateRequest(1, ""))
	if err != nil {
		return model.EvidenceManifest{}, err
	}
	return fx.engine.Run(context.Background(), verify.VerificationRequest{
		Catalog:     fx.catalogRef,
		CommandID:   fx.commandID,
		Purpose:     verify.PurposeTaskVerify,
		Worktree:    fx.taskWorktree,
		CommitRange: fx.baseHead + ".." + res.Head,
	})
}

// RequireNoVerificationRun asserts that no Verification run happened.
func (fx *taskGateFixture) RequireNoVerificationRun() {
	fx.t.Helper()
	if fx.verifyRuns != 0 {
		fx.t.Fatalf("verification ran %d times, want 0 (gate must fail before any run)", fx.verifyRuns)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertFaultCode(t *testing.T, err error, code model.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected fault %s, got nil error", code)
	}
	got, ok := model.CodeOf(err)
	if !ok {
		t.Fatalf("expected fault %s, got non-fault error: %v", code, err)
	}
	if got != code {
		t.Fatalf("fault code = %s, want %s (%v)", got, code, err)
	}
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return sha256Hex(t, data)
}

func sha256Hex(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

// ---------------------------------------------------------------------------
// The Task gate (design 15.4, PRD 已确认：Provider 默认权限与 Commit/Clean
// Worktree Gate)
// ---------------------------------------------------------------------------

// TestDirtyTaskCannotEnterVerification (brief Step 1, verbatim): a
// Git-visible untracked file in the Task Worktree blocks with
// DIRTY_TASK_WORKTREE and no Verification run ever happens.
func TestDirtyTaskCannotEnterVerification(t *testing.T) {
	fx := newTaskGateFixture(t)
	fx.WriteUntracked("unexpected.txt", "dirty")
	err := fx.RequestVerification()
	assertFaultCode(t, err, model.CodeDirtyTaskWorktree)
	fx.RequireNoVerificationRun()
}

// TestGateRequiresImplementationCommit: a Task whose HEAD equals the
// immutable Task Base has no implementation Commit.
func TestGateRequiresImplementationCommit(t *testing.T) {
	fx := newTaskGateFixture(t)
	fx.repo.git("-C", fx.taskWorktree, "reset", "--hard", "-q", fx.baseHead)
	fx.repo.git("-C", fx.taskWorktree, "clean", "-fdq")
	err := fx.RequestVerification()
	assertFaultCode(t, err, model.CodeMissingImplementationCommit)
	fx.RequireNoVerificationRun()
}

// TestGateRejectsRewrittenHistory: once an Attempt end is recorded,
// amending the Commit rewrites recorded history; the successor Attempt
// cannot verify (TASK_HISTORY_REWRITTEN, never auto-repaired).
func TestGateRejectsRewrittenHistory(t *testing.T) {
	fx := newTaskGateFixture(t)
	first, err := fx.engine.TaskGate(context.Background(), fx.gateRequest(1, ""))
	if err != nil {
		t.Fatalf("first gate: %v", err)
	}
	fx.repo.git("-C", fx.taskWorktree, "commit", "--amend", "-q", "--allow-empty", "-m", "amended")
	second, err := fx.engine.TaskGate(context.Background(), fx.gateRequest(2, first.Head))
	if err == nil {
		t.Fatalf("second gate passed with rewritten history (head %s)", second.Head)
	}
	assertFaultCode(t, err, model.CodeTaskHistoryRewritten)
}

// TestGateRejectsScopeViolation: a Commit touching a path outside the
// Spec write scope fails over the FULL task_base_commit..HEAD range.
func TestGateRejectsScopeViolation(t *testing.T) {
	fx := newTaskGateFixture(t)
	fx.writeTask("docs/outside.md", "out of scope\n")
	fx.commitTask("out of scope")
	err := fx.RequestVerification()
	assertFaultCode(t, err, model.CodeScopeViolation)
	fx.RequireNoVerificationRun()
}

// TestGateRejectsWrongIdentity: a Commit whose author/committer identity
// does not match the approved Preflight blocks with COMMIT_POLICY_MISMATCH.
func TestGateRejectsWrongIdentity(t *testing.T) {
	fx := newTaskGateFixture(t)
	fx.repo.git("-C", fx.taskWorktree, "-c", "user.name=Other", "-c", "user.email=other@example.com",
		"commit", "--allow-empty", "-q", "--author=Other <other@example.com>", "-m", "wrong identity")
	err := fx.RequestVerification()
	assertFaultCode(t, err, model.CodeCommitPolicyMismatch)
	fx.RequireNoVerificationRun()
}

// TestGatePassesCleanCommit: the clean, in-scope, policy-matching Commit
// passes the gate, pins its audit Ref, and reaches a passing Verification.
func TestGatePassesCleanCommit(t *testing.T) {
	fx := newTaskGateFixture(t)
	res, err := fx.engine.TaskGate(context.Background(), fx.gateRequest(1, ""))
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if res.Head == "" || res.AuditRef == "" {
		t.Fatalf("gate result carries no head/audit ref: %+v", res)
	}
	manifest, err := fx.engine.Run(context.Background(), verify.VerificationRequest{
		Catalog: fx.catalogRef, CommandID: fx.commandID, Purpose: verify.PurposeTaskVerify,
		Worktree: fx.taskWorktree, CommitRange: fx.baseHead + ".." + res.Head,
	})
	if err != nil {
		t.Fatalf("verification: %v", err)
	}
	if !manifest.Passed {
		t.Fatalf("clean verification did not pass: %+v", manifest)
	}
}

// ---------------------------------------------------------------------------
// The Verification Run contract (design 16.2)
// ---------------------------------------------------------------------------

// TestVerificationRejectsChangedCatalogExecutable: the executable
// identity must match the approval facts; a Task that modified the
// Catalog-pinned wrapper cannot verify (EVIDENCE_SUBJECT_CHANGED).
func TestVerificationRejectsChangedCatalogExecutable(t *testing.T) {
	fx := newTaskGateFixture(t)
	fx.writeTask(wrapperName, "#!/bin/sh\nexit 0 # changed\n")
	fx.commitTask("modify the catalog wrapper")
	_, err := fx.gateAndRun()
	assertFaultCode(t, err, model.CodeEvidenceSubjectChanged)
}

// TestVerificationCapturesExitFacts: a wrapper exiting outside the
// expected codes fails with the exit fact recorded.
func TestVerificationCapturesExitFacts(t *testing.T) {
	fx := newTaskGateFixtureWithWrapper(t, "#!/bin/sh\necho boom >&2\nexit 3\n")
	manifest, err := fx.gateAndRun()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if manifest.Passed {
		t.Fatalf("manifest passed with exit code 3: %+v", manifest)
	}
	if manifest.ExitCode != 3 || manifest.ExitFact != "exit" {
		t.Fatalf("exit facts = %d/%s, want 3/exit", manifest.ExitCode, manifest.ExitFact)
	}
	if manifest.Reason != "exit" {
		t.Fatalf("reason = %q, want exit", manifest.Reason)
	}
	if !strings.Contains(manifest.Output, "boom") {
		t.Fatalf("redacted output missing the frame: %q", manifest.Output)
	}
	if manifest.Hash == "" {
		t.Fatalf("manifest carries no self-hash")
	}
}

// TestVerificationRejectsTrackedOutput: the command modifying a tracked
// file fails verification (tracked changes fail).
func TestVerificationRejectsTrackedOutput(t *testing.T) {
	fx := newTaskGateFixtureWithWrapper(t, "#!/bin/sh\necho touched >> src/add.ts\nexit 0\n")
	manifest, err := fx.gateAndRun()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if manifest.Passed {
		t.Fatalf("manifest passed with tracked output: %+v", manifest)
	}
	if manifest.Reason != "tracked-output" {
		t.Fatalf("reason = %q, want tracked-output", manifest.Reason)
	}
}

// TestVerificationRejectsUntrackedOutput: Git-visible untracked output
// fails verification.
func TestVerificationRejectsUntrackedOutput(t *testing.T) {
	fx := newTaskGateFixtureWithWrapper(t, "#!/bin/sh\necho out > stray.txt\nexit 0\n")
	manifest, err := fx.gateAndRun()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if manifest.Passed {
		t.Fatalf("manifest passed with untracked output: %+v", manifest)
	}
	if manifest.Reason != "untracked-output" {
		t.Fatalf("reason = %q, want untracked-output", manifest.Reason)
	}
}

// TestVerificationRejectsIgnoredOutputOutsideDeclaredPaths: ignored
// output outside the entry's declared transient paths fails verification.
func TestVerificationRejectsIgnoredOutputOutsideDeclaredPaths(t *testing.T) {
	fx := newTaskGateFixtureWithBase(t, "#!/bin/sh\necho out > stray.log\nexit 0\n",
		map[string]string{".gitignore": "stray.log\n"})
	manifest, err := fx.gateAndRun()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if manifest.Passed {
		t.Fatalf("manifest passed with ignored output outside declared paths: %+v", manifest)
	}
	if manifest.Reason != "ignored-output-outside-transient-paths" {
		t.Fatalf("reason = %q, want ignored-output-outside-transient-paths", manifest.Reason)
	}
}

// TestVerificationAllowsIgnoredTransientOutput: ignored output within the
// entry's declared transient paths is permitted (design 16.2).
func TestVerificationAllowsIgnoredTransientOutput(t *testing.T) {
	fx := newTaskGateFixtureWithBase(t, "#!/bin/sh\nmkdir -p build\necho out > build/out.txt\nexit 0\n",
		map[string]string{".gitignore": "build/\n"})
	// The successor Catalog Revision declares the transient path.
	body, err := verify.CatalogBody(1, []verify.Candidate{
		{
			CommandID: "verify", Purpose: verify.PurposeTaskVerify,
			ExecutableKind: verify.KindProjectRelative, Executable: wrapperName,
			CWD: ".", TimeoutSeconds: 60, ExpectedExitCodes: []int{0},
			OutputLimitBytes: 4096, Env: []string{"PATH"},
			TransientWritePaths: []string{"build/"},
			Source:              fmt.Sprintf("base-commit-wrapper:%s@sha256:%s", wrapperName, hashFile(t, filepath.Join(fx.repo.dir, wrapperName))),
		},
	})
	if err != nil {
		t.Fatalf("catalog body: %v", err)
	}
	fx.catalogBody = body
	fx.catalogRef = model.CatalogRef{Revision: 1, Hash: sha256Hex(t, body)}
	manifest, err := fx.gateAndRun()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !manifest.Passed {
		t.Fatalf("manifest failed with transient output inside declared paths: %+v", manifest)
	}
}

// TestValidateCatalogRejectsIdentityMismatch: a Catalog ref whose body
// does not match the ref identity cannot be validated.
func TestValidateCatalogRejectsIdentityMismatch(t *testing.T) {
	fx := newTaskGateFixture(t)
	other := model.CatalogRef{Revision: 1, Hash: strings.Repeat("0", 64)}
	if _, err := fx.engine.ValidateCatalog(context.Background(), other); err == nil {
		t.Fatalf("ValidateCatalog accepted a mismatched ref identity")
	}
}
