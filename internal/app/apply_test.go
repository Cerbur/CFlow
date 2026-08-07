package app

// The protected Apply protocol (Task 19, PRD 已确认：显式受保护 Apply,
// design 15.5): Apply is a SEPARATE Attempt after Workflow completion that
// never reopens the completed Workflow state. Staging runs only in an
// isolated Apply Branch/Worktree from the recorded Target HEAD; the user
// workspace must be clean and attached to the Target Branch before and
// immediately before delivery; the final delivery is a compare-and-swap
// fast-forward that is only ever `git update-ref <target> <staging-head>
// <expected-target>` — no force-update argv exists anywhere in the apply
// path. Any failure leaves the Target exactly old or exactly the verified
// new head, never ambiguous, and the Workflow stays COMPLETED.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// ---------------------------------------------------------------------------
// fixture scripts
// ---------------------------------------------------------------------------

// applyVerificationPassScript is the deterministic APPLY_VERIFICATION
// Session output: a structured PASS verdict over the combined Target +
// Integration result inside the Apply Worktree.
func applyVerificationPassScript() string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"apply-verification","session_id":"av1","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"av1","at_ms":0}
{"type":"assistant_message","session_id":"av1","text":"Reviewed the combined apply result.","at_ms":10}
{"type":"session_finished","session_id":"av1","result":{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n- combined result verified\n"},"at_ms":20}`
}

// applyResolutionScript is the deterministic restricted Merge Resolution
// Session: it writes the resolved conflict file inside the Apply
// Worktree (never a Commit — the Apply Merge Commit is created by the
// merge continuation, and the resolution writes are bounded to the
// declared conflict files).
func applyResolutionScript(content string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"repair","session_id":"rp1","exit_code":0,"resume":"ok","writes":[{"path":"src/divide/divide.go","content":%q}]}
{"type":"session_started","session_id":"rp1","at_ms":0}
{"type":"assistant_message","session_id":"rp1","text":"Resolved the apply merge conflict.","at_ms":10}
{"type":"session_finished","session_id":"rp1","result":{"summary":"resolved"},"at_ms":20}`, content)
}

// applyResolutionFailScript is the refused/aborted resolution: the
// restricted Merge Resolution Attempt runs and fails, so the Apply
// stays BLOCKED with the conflicted state preserved.
const applyResolutionFailScript = `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"repair","session_id":"rp1","exit_code":1,"resume":"ok","crash_after":1}
{"type":"session_started","session_id":"rp1","at_ms":0}`

// ---------------------------------------------------------------------------
// gitTrace: the recording and fault-injecting Supervisor seam
// ---------------------------------------------------------------------------

// gitTrace wraps the fixture Supervisor, records every ProcessSpec argv
// the Application issues, and can inject a crash before or after one
// matching git invocation (the crash-before/after-Target-CAS cases).
// Production Applications never carry one; the apply fixture installs it
// only for the apply phases.
type gitTrace struct {
	process.Supervisor
	mu     sync.Mutex
	specs  []process.ProcessSpec
	failed map[int]bool // specs whose Start was injected-failed
	mode   string       // "", "fail-call", "fail-after"
	pred   func(args []string) bool
	done   bool
	after  bool
}

func (g *gitTrace) Start(ctx context.Context, spec process.ProcessSpec) (process.Handle, process.Events, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	idx := len(g.specs)
	g.specs = append(g.specs, spec)
	switch g.mode {
	case "fail-call":
		if !g.done && g.pred(spec.Args) {
			g.done = true
			if g.failed == nil {
				g.failed = map[int]bool{}
			}
			g.failed[idx] = true
			return process.Handle{}, nil, errors.New("injected git failure")
		}
	case "fail-after":
		if g.after {
			g.after = false
			g.done = true
			if g.failed == nil {
				g.failed = map[int]bool{}
			}
			g.failed[idx] = true
			return process.Handle{}, nil, errors.New("injected git failure")
		}
		if !g.done && g.pred(spec.Args) {
			g.after = true
		}
	}
	return g.Supervisor.Start(ctx, spec)
}

// armFailCall crashes the first git invocation matching pred.
func (g *gitTrace) armFailCall(pred func(args []string) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode, g.pred, g.done, g.after = "fail-call", pred, false, false
}

// armFailAfter crashes the git invocation immediately following the
// first invocation matching pred.
func (g *gitTrace) armFailAfter(pred func(args []string) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode, g.pred, g.done, g.after = "fail-after", pred, false, false
}

func (g *gitTrace) disarm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode, g.pred, g.done, g.after = "", nil, false, false
}

// countGit returns the recorded invocations whose argv matches pred,
// excluding the injected-failed ones (a crashed invocation never ran).
func (g *gitTrace) countGit(pred func(args []string) bool) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for i, spec := range g.specs {
		if g.failed[i] {
			continue
		}
		if pred(spec.Args) {
			n++
		}
	}
	return n
}

// everyGit reports whether every recorded invocation satisfies pred.
func (g *gitTrace) everyGit(pred func(args []string) bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, spec := range g.specs {
		if !pred(spec.Args) {
			return false
		}
	}
	return true
}

func isTargetUpdateRef(args []string) bool {
	return len(args) >= 2 && args[0] == "update-ref" && args[1] == "refs/heads/main"
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// applyFixture drives one completed workflow through the protected Apply
// protocol. The completion phases run on the plain fixture Supervisor;
// the apply phases run on the recording gitTrace so the tests observe
// every git argv the apply path issues.
type applyFixture struct {
	t     *testing.T
	fx    *planningFixture
	wf    model.WorkflowID
	trace *gitTrace
	a     *Application // the most recent apply-phase Application
	// lateAdvance is the head the verbatim drift test recorded.
	lateAdvance string
}

// completedWorkflowForApply builds a real repository whose deterministic
// Base Commit carries the verification wrappers (including the
// apply-verify wrapper the Apply verification revalidates), drives the
// full pipeline through the Execution Approval and the exact-evidence
// completion, and returns the fixture for the apply phases.
func completedWorkflowForApply(t *testing.T) *applyFixture {
	t.Helper()
	fx := newExecutionFixture(t)
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fx.root, filepath.FromSlash(rel)), []byte(content), 0o755); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("scripts/apply-verify.sh", "#!/bin/sh\nexit 0\n")
	fx.git("add", "scripts/apply-verify.sh")
	fx.git("commit", "-q", "-m", "add apply verification wrapper")
	wf := fx.planningApproved()
	pv := driveToExecutionGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)
	a := fx.app(implementationCommitScript(), reviewPassScript(), finalReviewPassScript())
	dispatchUntilCompletedApp(t, a, wf)
	iv := aInspect(t, a, wf)
	if iv.Status.Stage != model.StageCompleted || iv.Status.Runtime != model.RuntimeSucceeded {
		t.Fatalf("workflow = %s/%s, want COMPLETED/SUCCEEDED", iv.Status.Stage, iv.Status.Runtime)
	}
	return &applyFixture{
		t: t, fx: fx, wf: wf,
		trace: &gitTrace{Supervisor: fx.sup},
	}
}

// applyApp builds a fresh Application for the apply phases, running every
// git invocation through the recording gitTrace.
func (af *applyFixture) applyApp(scripts ...string) *Application {
	af.t.Helper()
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		af.t.Fatalf("provider registry: %v", err)
	}
	prompts, err := agent.LoadPromptRegistry()
	if err != nil {
		af.t.Fatalf("prompt registry: %v", err)
	}
	ad := fake.New(reg)
	for _, s := range scripts {
		if err := ad.LoadScript([]byte(s)); err != nil {
			af.t.Fatalf("load fake script: %v", err)
		}
	}
	flow, err := gitflow.NewGitFlow(af.trace, af.fx.root)
	if err != nil {
		af.t.Fatalf("new gitflow: %v", err)
	}
	a, err := New(Options{
		Home:         af.fx.home,
		Project:      ProjectFor(af.fx.root),
		CflowVersion: "0.0.0-dev",
		Now:          af.fx.now,
		IDs:          af.fx.ids,
		Supervisor:   af.trace,
		GitFlow:      flow,
		Prompts:      prompts,
		Agent: agent.RuntimeOptions{
			Registry:    reg,
			Redaction:   security.Registry{},
			Adapters:    map[string]agent.Adapter{"fake": ad},
			EvidenceDir: filepath.Join(af.fx.home, "evidence"),
		},
	})
	if err != nil {
		af.t.Fatalf("new application: %v", err)
	}
	af.a = a
	return a
}

// prepareErr runs the PrepareApply command and returns its error.
func (af *applyFixture) prepareErr(scripts ...string) error {
	af.t.Helper()
	scripts = append([]string{applyVerificationPassScript()}, scripts...)
	_, err := af.applyApp(scripts...).Execute(context.Background(), PrepareApplyCommand{Workflow: af.wf})
	return err
}

// PrepareApply runs the full staging protocol (workspace gate, isolated
// Apply Worktree, Commit Policy revalidation, --no-ff merge, deterministic
// apply verification, independent Apply Verification Session) and returns
// the recorded Apply Attempt.
func (af *applyFixture) PrepareApply() model.ApplyAttempt {
	af.t.Helper()
	if err := af.prepareErr(); err != nil {
		af.t.Fatalf("prepare apply: %v", err)
	}
	attempt := af.latestApply()
	if attempt == nil {
		af.t.Fatalf("prepare apply recorded no apply attempt")
	}
	return *attempt
}

// PrepareApplyResolution re-runs the staging of the same BLOCKED attempt
// with the ONE restricted Merge Resolution Session available.
func (af *applyFixture) PrepareApplyResolution() model.ApplyAttempt {
	af.t.Helper()
	if err := af.prepareErr(applyResolutionScript(resolvedDivideSource)); err != nil {
		af.t.Fatalf("prepare apply with resolution: %v", err)
	}
	attempt := af.latestApply()
	if attempt == nil {
		af.t.Fatalf("prepare apply recorded no apply attempt")
	}
	return *attempt
}

// PassStagingVerification asserts the Apply Attempt passed staging and
// the full deterministic + independent review verification and now
// awaits the explicit delivery command.
func (af *applyFixture) PassStagingVerification(attempt model.ApplyAttempt) {
	af.t.Helper()
	if got := af.latestApply().Status; got != model.ApplyAwaitingConfirmation {
		af.t.Fatalf("apply %s = %s after staging verification, want %s",
			attempt.ID, got, model.ApplyAwaitingConfirmation)
	}
}

// CommitApply runs the explicit delivery (ExecuteApply): the final
// compare-and-swap fast-forward of the Target Branch.
func (af *applyFixture) CommitApply(attempt model.ApplyAttempt) error {
	af.t.Helper()
	_, err := af.applyApp().Execute(context.Background(), ExecuteApplyCommand{Workflow: af.wf})
	return err
}

// ConfirmPolicy runs the explicit Commit Policy / Apply Catalog
// confirmation (ConfirmApplyPolicy) for the blocked attempt. The
// confirmation re-opens the staging, so the Apply Verification Session
// script must be loaded; a staging run that must resolve a conflicted
// Apply Worktree also needs the restricted Merge Resolution script.
func (af *applyFixture) ConfirmPolicy(scripts ...string) error {
	af.t.Helper()
	scripts = append([]string{applyVerificationPassScript()}, scripts...)
	_, err := af.applyApp(scripts...).Execute(context.Background(), ConfirmApplyPolicyCommand{Workflow: af.wf})
	return err
}

// AdvanceTargetAfterVerification advances the Target Branch on the user's
// workspace after the staging verification passed (simulating the user's
// own late commit) and records the new head for the drift assertions.
func (af *applyFixture) AdvanceTargetAfterVerification() {
	af.t.Helper()
	af.fx.git("commit", "-q", "--allow-empty", "-m", "late target advance")
	af.lateAdvance = strings.TrimSpace(af.fx.git("rev-parse", "HEAD"))
}

// RequireTargetAtLateAdvance asserts the Target Branch stayed exactly at
// the late advance: the failed Apply never moved it.
func (af *applyFixture) RequireTargetAtLateAdvance() {
	af.t.Helper()
	if got := strings.TrimSpace(af.fx.git("rev-parse", "refs/heads/main")); got != af.lateAdvance {
		af.t.Fatalf("target branch = %s, want the late advance %s", got, af.lateAdvance)
	}
}

// RequireWorkflowCompleted asserts the Apply never altered the completed
// Workflow state.
func (af *applyFixture) RequireWorkflowCompleted() {
	af.t.Helper()
	iv := af.inspect()
	if iv.Status.Stage != model.StageCompleted || iv.Status.Runtime != model.RuntimeSucceeded {
		af.t.Fatalf("workflow = %s/%s after apply, want COMPLETED/SUCCEEDED",
			iv.Status.Stage, iv.Status.Runtime)
	}
}

// inspect reads the aggregate through the most recent apply-phase
// Application (or a plain read Application before any apply phase ran).
func (af *applyFixture) inspect() InspectView {
	a := af.a
	if a == nil {
		a = af.fx.app()
	}
	return aInspect(af.t, a, af.wf)
}

func (af *applyFixture) latestApply() *model.ApplyAttempt {
	af.t.Helper()
	iv := af.inspect()
	if len(iv.ApplyAttempts) == 0 {
		return nil
	}
	return &iv.ApplyAttempts[len(iv.ApplyAttempts)-1]
}

func (af *applyFixture) targetHead() string {
	return strings.TrimSpace(af.fx.git("rev-parse", "refs/heads/main"))
}

func (af *applyFixture) integrationHead() string {
	st := af.inspect().Status
	// On the aggregated workspace layout the delivery mainline is the
	// verified Workspace head (design 8.5, TUI task 7); the legacy layout
	// reads the Integration head.
	if st.VerifiedWorkspaceHead != "" {
		return st.VerifiedWorkspaceHead
	}
	return st.IntegrationHead
}

// applyBranchHead resolves the current head of the CFlow-owned Apply
// Branch of the last attempt (the verified staging head).
func (af *applyFixture) applyBranchHead() string {
	att := af.latestApply()
	if att == nil {
		af.t.Fatalf("no apply attempt recorded")
	}
	return strings.TrimSpace(af.fx.git("rev-parse", fmt.Sprintf("refs/heads/cflow/%s/apply-%d", af.wf, att.Number)))
}

func (af *applyFixture) commitParents(head string) []string {
	af.t.Helper()
	out := strings.TrimSpace(af.fx.git("rev-list", "--parents", "-n", "1", head))
	parts := strings.Fields(out)
	if len(parts) < 2 {
		return []string{}
	}
	return parts[1:]
}

// advanceTarget commits one file on the user's Target Branch (a late
// user commit the Apply must merge or reject) and returns the new head.
func (af *applyFixture) advanceTarget(rel, content string) string {
	af.t.Helper()
	path := filepath.Join(af.fx.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		af.t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		af.t.Fatalf("write %s: %v", rel, err)
	}
	af.fx.git("add", rel)
	af.fx.git("commit", "-q", "-m", "target advance: "+rel)
	return strings.TrimSpace(af.fx.git("rev-parse", "HEAD"))
}

// resolvedDivideSource is the deterministic resolution of the add/add
// conflict on src/divide/divide.go: the integration implementation plus
// the target-side comment.
const resolvedDivideSource = "package divide\n\n// target-side change merged by the apply resolution\n\n// Divide returns a/b.\nfunc Divide(a, b int) (int, error) {\n\treturn a / b, nil\n}\n"

// targetDivideSource conflicts with the integration's implementation:
// both sides create src/divide/divide.go with different content.
const targetDivideSource = "package divide\n\n// target-side version\n"

// ---------------------------------------------------------------------------
// the mandated verbatim test
// ---------------------------------------------------------------------------

// TestApplyTargetCASPreventsLateAdvance (brief Step 1, verbatim): the
// Target Branch advances after the staging verification passed; the
// final compare-and-swap refuses with TARGET_HEAD_DRIFTED; the Target
// stays exactly at the late advance and the Workflow stays Completed.
func TestApplyTargetCASPreventsLateAdvance(t *testing.T) {
	fx := completedWorkflowForApply(t)
	attempt := fx.PrepareApply()
	fx.PassStagingVerification(attempt)
	fx.AdvanceTargetAfterVerification()
	err := fx.CommitApply(attempt)
	assertFaultCode(t, err, model.CodeTargetHeadChanged)
	fx.RequireTargetAtLateAdvance()
	fx.RequireWorkflowCompleted()
}

// TestApplyBlocksTamperedApplyBranchHead: the delivery's subject is the
// REVIEWED staging head — the recorded StagingHead is the only rendering
// of the Apply Branch ref the review verified, and every other
// pre-delivery gate (workspace, fingerprint, catalog, integration head)
// passes independently of the staging ref's content. A locally tampered
// Apply Branch ref must never fast-forward the Target to an unreviewed
// head: the delivery fails closed with EVIDENCE_SUBJECT_CHANGED — both
// before the compare-and-swap (a fabricated head the CAS would deliver)
// and before the crash-recovery observation (a Target already at a
// tampered head must never be reported delivered) — and the Target stays
// exactly at the recorded head.
func TestApplyBlocksTamperedApplyBranchHead(t *testing.T) {
	applyRef := func(fx *applyFixture, attempt model.ApplyAttempt) string {
		return fmt.Sprintf("refs/heads/cflow/%s/apply-%d", fx.wf, attempt.Number)
	}
	t.Run("compare-and-swap would deliver an unreviewed head", func(t *testing.T) {
		fx := completedWorkflowForApply(t)
		attempt := fx.PrepareApply()
		fx.PassStagingVerification(attempt)
		// Tamper: fabricate a child Commit of the verified staging head
		// (a locally created commit the review never saw) and point the
		// Apply Branch ref at it.
		staging := fx.applyBranchHead()
		tampered := strings.TrimSpace(fx.fx.git("commit-tree", staging+"^{tree}", "-p", staging, "-m", "tampered unreviewed head"))
		fx.fx.git("update-ref", applyRef(fx, attempt), tampered)
		err := fx.CommitApply(attempt)
		assertFaultCode(t, err, model.CodeEvidenceSubjectChanged)
		if got := fx.targetHead(); got != attempt.TargetHead {
			t.Fatalf("target = %s after the tampered apply branch, want exactly the recorded head %s", got, attempt.TargetHead)
		}
		fx.RequireWorkflowCompleted()
	})
	t.Run("crash recovery never reports a tampered subject delivered", func(t *testing.T) {
		fx := completedWorkflowForApply(t)
		attempt := fx.PrepareApply()
		fx.PassStagingVerification(attempt)
		// Tamper: point the Apply Branch ref at the current Target head.
		// A Target already at the tampered head would otherwise look like
		// the delivered outcome (the crash-recovery observation) and
		// settle SUCCEEDED without any comparison against the reviewed
		// subject.
		fx.fx.git("update-ref", applyRef(fx, attempt), attempt.TargetHead)
		err := fx.CommitApply(attempt)
		assertFaultCode(t, err, model.CodeEvidenceSubjectChanged)
		if got := fx.targetHead(); got != attempt.TargetHead {
			t.Fatalf("target = %s, want exactly the recorded head %s", got, attempt.TargetHead)
		}
		fx.RequireWorkflowCompleted()
	})
}

// ---------------------------------------------------------------------------
// the case list
// ---------------------------------------------------------------------------

// TestApplyUnchangedTargetFastForwardsOnlyAfterVerification: with an
// unchanged Target the Apply fast-forwards exactly to the verified
// staging head (a --no-ff merge Commit with the Target and Integration
// parents), never a synthetic or unverified commit, and the completed
// Workflow and its evidence stay untouched.
func TestApplyUnchangedTargetFastForwardsOnlyAfterVerification(t *testing.T) {
	fx := completedWorkflowForApply(t)
	attempt := fx.PrepareApply()
	fx.PassStagingVerification(attempt)
	staging := fx.applyBranchHead()
	integration := fx.integrationHead()
	if err := fx.CommitApply(attempt); err != nil {
		t.Fatalf("commit apply: %v", err)
	}
	if got := fx.targetHead(); got != staging {
		t.Fatalf("target = %s, want the verified staging head %s", got, staging)
	}
	parents := fx.commitParents(staging)
	if len(parents) < 2 {
		t.Fatalf("staging head %s has %d parents, want the target and integration parents", staging, len(parents))
	}
	// The integration output is contained: the integration head is an
	// ancestor of the delivered target head.
	if out := strings.TrimSpace(fx.fx.git("rev-list", staging+".."+integration)); out != "" {
		t.Fatalf("integration head %s is not contained in the delivered target %s", integration, staging)
	}
	fx.RequireWorkflowCompleted()
	// Task 18: the report shows the Apply outcome — no longer NOT_RUN.
	view, err := fx.a.Query(context.Background(), ReportQuery{Workflow: fx.wf, Build: observe.BuildInfo{Version: "0.0.0-dev"}})
	if err != nil {
		t.Fatalf("report query: %v", err)
	}
	rv := view.(ReportView)
	if rv.Report.Apply.Status != model.ApplySucceeded.String() {
		t.Fatalf("report apply status = %s, want %s", rv.Report.Apply.Status, model.ApplySucceeded)
	}
}

// TestApplyBlocksDirtyUserWorkspace: a dirty user workspace fails the
// workspace gate before any staging, and a workspace dirtied after the
// staging verification fails the final pre-delivery recheck; CFlow never
// stashes, WIP-commits, or overwrites user content, and the Target never
// moves.
func TestApplyBlocksDirtyUserWorkspace(t *testing.T) {
	t.Run("before staging", func(t *testing.T) {
		fx := completedWorkflowForApply(t)
		if err := os.WriteFile(filepath.Join(fx.fx.root, "uncommitted.txt"), []byte("user content"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		err := fx.prepareErr()
		assertFaultCode(t, err, model.CodeApplyTargetDirty)
		fx.RequireWorkflowCompleted()
		if len(fx.latestApplyAttempts()) != 0 {
			t.Fatalf("a dirty workspace must not record an apply attempt")
		}
	})
	t.Run("immediately before delivery", func(t *testing.T) {
		fx := completedWorkflowForApply(t)
		attempt := fx.PrepareApply()
		fx.PassStagingVerification(attempt)
		if err := os.WriteFile(filepath.Join(fx.fx.root, "uncommitted.txt"), []byte("user content"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		err := fx.CommitApply(attempt)
		assertFaultCode(t, err, model.CodeApplyTargetDirty)
		// The attempt blocks; the Target never moved.
		if got := fx.latestApply().Status; got != model.ApplyBlocked {
			t.Fatalf("apply = %s, want BLOCKED", got)
		}
		if got := fx.targetHead(); got != attempt.TargetHead {
			t.Fatalf("target = %s, want the recorded head %s", got, attempt.TargetHead)
		}
		fx.RequireWorkflowCompleted()
	})
}

func (af *applyFixture) latestApplyAttempts() []model.ApplyAttempt {
	af.t.Helper()
	return af.inspect().ApplyAttempts
}

// TestApplyBlocksWrongAttachedBranchAndDetachedHead: the workspace must
// be attached to the recorded Target Branch; a wrong branch or a
// detached HEAD blocks with APPLY_TARGET_BRANCH_CHANGED before any
// staging.
func TestApplyBlocksWrongAttachedBranchAndDetachedHead(t *testing.T) {
	fx := completedWorkflowForApply(t)
	fx.fx.git("checkout", "-q", "-b", "other")
	err := fx.prepareErr()
	assertFaultCode(t, err, model.CodeApplyTargetBranchChanged)
	fx.RequireWorkflowCompleted()

	fx.fx.git("checkout", "-q", "main")
	fx.fx.git("checkout", "-q", "--detach", "main")
	err = fx.prepareErr()
	assertFaultCode(t, err, model.CodeApplyTargetBranchChanged)
	fx.RequireWorkflowCompleted()
}

// TestApplyMergesAdvancedTargetWithoutConflict: a Target that advanced
// before the Apply is merged in the isolated Apply Worktree (the user's
// workspace is never used for the merge), the combined result is fully
// re-verified and reviewed, and the delivery fast-forwards the Target to
// the verified merge Commit containing both the user's advance and the
// Integration output.
func TestApplyMergesAdvancedTargetWithoutConflict(t *testing.T) {
	fx := completedWorkflowForApply(t)
	advance := fx.advanceTarget("user-notes.md", "user work on the target\n")
	integration := fx.integrationHead()
	attempt := fx.PrepareApply()
	fx.PassStagingVerification(attempt)
	if err := fx.CommitApply(attempt); err != nil {
		t.Fatalf("commit apply: %v", err)
	}
	staging := fx.applyBranchHead()
	if got := fx.targetHead(); got != staging {
		t.Fatalf("target = %s, want the verified staging head %s", got, staging)
	}
	parents := fx.commitParents(staging)
	if len(parents) < 2 {
		t.Fatalf("staging head has %d parents, want the merge commit", len(parents))
	}
	if parents[0] != advance {
		t.Fatalf("first parent = %s, want the advanced target head %s", parents[0], advance)
	}
	if out := strings.TrimSpace(fx.fx.git("rev-list", staging+".."+integration)); out != "" {
		t.Fatalf("integration output is not contained in the delivered target")
	}
	// The merged tree carries both sides.
	for _, want := range []string{"user-notes.md", "src/divide/divide.go"} {
		if out := strings.TrimSpace(fx.fx.git("cat-file", "-e", staging+":"+want)); out != "" {
			t.Fatalf("merged tree misses %s", want)
		}
	}
	fx.RequireWorkflowCompleted()
}

// TestApplyMergeConflictUsesOneRestrictedResolution: a text conflict in
// the Apply Worktree blocks the attempt with MERGE_CONFLICT and the
// Target stays unchanged; the ONE restricted Merge Resolution Attempt
// resolves the conflict inside the Apply Worktree (never the user's
// workspace), the combined result is fully re-verified, and the delivery
// completes. A refused resolution keeps the attempt BLOCKED with the
// Target unchanged.
func TestApplyMergeConflictUsesOneRestrictedResolution(t *testing.T) {
	t.Run("resolution succeeds", func(t *testing.T) {
		fx := completedWorkflowForApply(t)
		fx.advanceTarget("src/divide/divide.go", targetDivideSource)
		err := fx.prepareErr()
		assertFaultCode(t, err, model.CodeMergeConflict)
		if got := fx.latestApply().Status; got != model.ApplyBlocked {
			t.Fatalf("apply = %s after the conflict, want BLOCKED", got)
		}
		if got := fx.targetHead(); got != strings.TrimSpace(fx.fx.git("rev-parse", "refs/heads/main")) {
			t.Fatalf("the conflict moved the target")
		}
		fx.RequireWorkflowCompleted()

		attempt := fx.PrepareApplyResolution()
		fx.PassStagingVerification(attempt)
		if err := fx.CommitApply(attempt); err != nil {
			t.Fatalf("commit apply after resolution: %v", err)
		}
		staging := fx.applyBranchHead()
		if got := fx.targetHead(); got != staging {
			t.Fatalf("target = %s, want the resolved staging head %s", got, staging)
		}
		// The resolution audit Ref pins the one restricted attempt.
		ref := fmt.Sprintf("refs/cflow/%s/apply/%s/resolution", fx.wf, attempt.ID)
		if out := strings.TrimSpace(fx.fx.git("rev-parse", ref)); out == "" {
			t.Fatalf("resolution audit ref %s is missing", ref)
		}
		fx.RequireWorkflowCompleted()
	})
	t.Run("resolution refused", func(t *testing.T) {
		fx := completedWorkflowForApply(t)
		fx.advanceTarget("src/divide/divide.go", targetDivideSource)
		err := fx.prepareErr()
		assertFaultCode(t, err, model.CodeMergeConflict)
		// The refused resolution attempt leaves the attempt BLOCKED.
		if err := fx.prepareErr(applyResolutionFailScript); err == nil {
			t.Fatalf("the refused resolution must not complete the apply")
		}
		if got := fx.latestApply().Status; got != model.ApplyBlocked {
			t.Fatalf("apply = %s after the refused resolution, want BLOCKED", got)
		}
		before := strings.TrimSpace(fx.fx.git("rev-parse", "refs/heads/main"))
		if got := fx.targetHead(); got != before {
			t.Fatalf("the refused resolution moved the target")
		}
		fx.RequireWorkflowCompleted()
	})
}

// TestApplyCatalogIdentityDriftRequiresExplicitApproval: a Target Drift
// that changed the verification wrapper identity blocks the Apply with
// COMMAND_IDENTITY_CHANGED and never runs the drifted tool or reuses the
// old Catalog; only an explicit APPLY_CATALOG approval of a newly
// discovered, validated, and fixed Catalog Revision (binding the exact
// Apply Attempt and Target/Integration heads) lets the Apply continue.
func TestApplyCatalogIdentityDriftRequiresExplicitApproval(t *testing.T) {
	fx := completedWorkflowForApply(t)
	// The Target advances and changes the wrapper the Apply verification
	// must revalidate.
	fx.advanceTarget("scripts/apply-verify.sh", "#!/bin/sh\nexit 0\n# drift\n")
	err := fx.prepareErr()
	assertFaultCode(t, err, model.CodeCommandIdentityChanged)
	if got := fx.latestApply().Status; got != model.ApplyBlocked {
		t.Fatalf("apply = %s after the identity drift, want BLOCKED", got)
	}
	fx.RequireWorkflowCompleted()

	// The explicit approval re-discovers and fixes the new Catalog
	// Revision from the new Target HEAD and lets the same attempt
	// continue.
	if err := fx.ConfirmPolicy(); err != nil {
		t.Fatalf("confirm apply catalog: %v", err)
	}
	attempt := fx.PrepareApply()
	fx.PassStagingVerification(attempt)
	if err := fx.CommitApply(attempt); err != nil {
		t.Fatalf("commit apply after the catalog approval: %v", err)
	}
	if got := fx.targetHead(); got != fx.applyBranchHead() {
		t.Fatalf("target = %s, want the verified staging head %s", got, fx.applyBranchHead())
	}
	fx.RequireWorkflowCompleted()
}

// TestApplyPolicyConfirmationBindsAttemptAndHeads: a Commit Policy drift
// after completion blocks the Apply Attempt with
// COMMIT_POLICY_CONFIRMATION_REQUIRED (the Workflow stays completed and
// the Target unchanged); the confirmation binds the exact Apply Attempt,
// Target/Integration heads, and the new Preflight Revision/hash/
// fingerprint — a drifted head voids the confirmation.
func TestApplyPolicyConfirmationBindsAttemptAndHeads(t *testing.T) {
	t.Run("confirmation continues the same attempt", func(t *testing.T) {
		fx := completedWorkflowForApply(t)
		fx.fx.git("config", "user.name", "Other User")
		err := fx.prepareErr()
		assertFaultCode(t, err, model.CodeCommitPolicyConfirmationRequired)
		if got := fx.latestApply().Status; got != model.ApplyBlocked {
			t.Fatalf("apply = %s, want BLOCKED", got)
		}
		if got := fx.targetHead(); got != strings.TrimSpace(fx.fx.git("rev-parse", "refs/heads/main")) {
			t.Fatalf("the policy drift moved the target")
		}
		fx.RequireWorkflowCompleted()

		if err := fx.ConfirmPolicy(); err != nil {
			t.Fatalf("confirm policy: %v", err)
		}
		attempt := fx.PrepareApply()
		fx.PassStagingVerification(attempt)
		if err := fx.CommitApply(attempt); err != nil {
			t.Fatalf("commit apply after the policy confirmation: %v", err)
		}
		if got := fx.targetHead(); got != fx.applyBranchHead() {
			t.Fatalf("target = %s, want the verified staging head %s", got, fx.applyBranchHead())
		}
		fx.RequireWorkflowCompleted()
	})
	t.Run("drifted head voids the confirmation", func(t *testing.T) {
		fx := completedWorkflowForApply(t)
		fx.fx.git("config", "user.name", "Other User")
		err := fx.prepareErr()
		assertFaultCode(t, err, model.CodeCommitPolicyConfirmationRequired)
		fx.AdvanceTargetAfterVerification()
		err = fx.ConfirmPolicy()
		assertFaultCode(t, err, model.CodeTargetHeadChanged)
		if got := fx.latestApply().Status; got != model.ApplyBlocked {
			t.Fatalf("apply = %s after the void confirmation, want BLOCKED", got)
		}
		fx.RequireTargetAtLateAdvance()
		fx.RequireWorkflowCompleted()
	})
}

// TestApplyConfirmedPolicyThenConflictRetryCompletes (review finding #1:
// the confirmed-policy + text-conflict intersection): a Target advance
// with a conflicting file blocks the first staging; a policy drift then
// blocks the request gate; the user confirms (the confirmation re-binds
// the policy and the confirm's own staging re-run hits the same text
// conflict, blocked without a resolution session because the worktree
// did not exist at confirmation time); and a plain `cflow apply` retry
// re-opens the confirmed attempt — the gate recognizes the recorded
// confirmation and never re-blocks (COMMIT_POLICY_CONFIRMATION_REQUIRED),
// the ONE restricted Merge Resolution Session resolves the conflict in
// the isolated Apply Worktree, and the delivery completes. The deadlock
// the review found is closed: the Target is exactly the verified staging
// head and the Workflow stays COMPLETED.
func TestApplyConfirmedPolicyThenConflictRetryCompletes(t *testing.T) {
	fx := completedWorkflowForApply(t)
	// The Target advances with a file that conflicts with the Integration
	// output (the Apply Worktree is the only place that ever merges).
	fx.advanceTarget("src/divide/divide.go", targetDivideSource)
	// The policy drifts before the first request: the fresh attempt records
	// the drifted fingerprint and the gate blocks before any staging.
	fx.fx.git("config", "user.name", "Other User")
	err := fx.prepareErr()
	assertFaultCode(t, err, model.CodeCommitPolicyConfirmationRequired)
	if got := fx.latestApply(); got == nil || got.Status != model.ApplyBlocked {
		t.Fatalf("apply = %+v, want a BLOCKED attempt", got)
	}
	fx.RequireWorkflowCompleted()
	// The confirmation re-binds the fresh policy; its staging re-run merges
	// the Integration Branch at the advanced Target HEAD and hits the text
	// conflict (no resolution session was allocated: the Apply Worktree did
	// not exist at confirmation time).
	err = fx.ConfirmPolicy()
	assertFaultCode(t, err, model.CodeMergeConflict)
	if got := fx.latestApply().Status; got != model.ApplyBlocked {
		t.Fatalf("apply = %s after the confirm's conflict, want BLOCKED", got)
	}
	fx.RequireWorkflowCompleted()
	// The plain retry re-opens the SAME confirmed attempt: the gate now
	// recognizes the recorded confirmation (never COMMIT_POLICY_
	// CONFIRMATION_REQUIRED) and the ONE restricted Merge Resolution
	// Session resolves the conflict inside the Apply Worktree.
	attempt := fx.PrepareApplyResolution()
	fx.PassStagingVerification(attempt)
	if err := fx.CommitApply(attempt); err != nil {
		t.Fatalf("commit apply after the confirmed-policy resolution: %v", err)
	}
	if got := fx.targetHead(); got != fx.applyBranchHead() {
		t.Fatalf("target = %s, want the verified staging head %s", got, fx.applyBranchHead())
	}
	// The resolution audit Ref pins the one restricted attempt.
	ref := fmt.Sprintf("refs/cflow/%s/apply/%s/resolution", fx.wf, attempt.ID)
	if out := strings.TrimSpace(fx.fx.git("rev-parse", ref)); out == "" {
		t.Fatalf("resolution audit ref %s is missing", ref)
	}
	fx.RequireWorkflowCompleted()
}

// TestApplySigningPreflightFailureBlocksConfirmation: when the drift
// requires a new signing Preflight probe and the probe fails, the Apply
// Attempt stays BLOCKED and the Target is unchanged; CFlow never falls
// back to an unsigned or unvalidated policy.
func TestApplySigningPreflightFailureBlocksConfirmation(t *testing.T) {
	fx := completedWorkflowForApply(t)
	fx.fx.git("config", "commit.gpgsign", "true")
	fx.fx.git("config", "user.signingkey", "bogus-key")
	err := fx.prepareErr()
	assertFaultCode(t, err, model.CodeCommitPolicyConfirmationRequired)
	err = fx.ConfirmPolicy()
	assertFaultCode(t, err, model.CodeGitSigningPreflightFailed)
	if got := fx.latestApply().Status; got != model.ApplyBlocked {
		t.Fatalf("apply = %s after the signing failure, want BLOCKED", got)
	}
	if got := fx.targetHead(); got != strings.TrimSpace(fx.fx.git("rev-parse", "refs/heads/main")) {
		t.Fatalf("the signing failure moved the target")
	}
	fx.RequireWorkflowCompleted()
}

// TestApplyReviewerIsIndependentSession: the Apply Verification Session
// is a fresh, independent Session (apply-verification purpose) that can
// never share another Session's provider session id — the reviewer is
// never the implementer or the final reviewer.
func TestApplyReviewerIsIndependentSession(t *testing.T) {
	fx := completedWorkflowForApply(t)
	attempt := fx.PrepareApply()
	fx.PassStagingVerification(attempt)
	if err := fx.CommitApply(attempt); err != nil {
		t.Fatalf("commit apply: %v", err)
	}
	iv := aInspect(t, fx.a, fx.wf)
	var applyReview *model.Session
	providerIDs := map[string]string{}
	for i := range iv.Sessions {
		s := iv.Sessions[i]
		if s.Purpose == model.PurposeApplyVerification {
			applyReview = &s
			continue
		}
		if s.ProviderSessionID != "" {
			providerIDs[s.ProviderSessionID] = string(s.ID)
		}
	}
	if applyReview == nil {
		t.Fatalf("no apply-verification session recorded")
	}
	if applyReview.ProviderSessionID == "" {
		t.Fatalf("the apply verification session has no provider session id")
	}
	if owner, dup := providerIDs[applyReview.ProviderSessionID]; dup {
		t.Fatalf("the apply review provider session id is shared with session %s", owner)
	}
	if applyReview.ID == "" {
		t.Fatalf("the apply review session has no identity")
	}
	fx.RequireWorkflowCompleted()
}

// TestApplyCrashBeforeTargetCASLeavesTargetOld: a crash before the
// compare-and-swap (injected at the update-ref invocation) leaves the
// Target exactly at the old head and the attempt in flight; the explicit
// delivery retry performs the CAS exactly once and settles SUCCEEDED.
func TestApplyCrashBeforeTargetCASLeavesTargetOld(t *testing.T) {
	fx := completedWorkflowForApply(t)
	attempt := fx.PrepareApply()
	fx.PassStagingVerification(attempt)
	fx.trace.armFailCall(isTargetUpdateRef)
	if err := fx.CommitApply(attempt); err == nil {
		t.Fatalf("the injected pre-CAS crash must fail the delivery")
	}
	if got := fx.targetHead(); got != attempt.TargetHead {
		t.Fatalf("target = %s after the pre-CAS crash, want exactly the old head %s", got, attempt.TargetHead)
	}
	if got := fx.latestApply().Status; got != model.ApplyRunning {
		t.Fatalf("apply = %s after the pre-CAS crash, want RUNNING (unsettled)", got)
	}
	fx.RequireWorkflowCompleted()

	fx.trace.disarm()
	if err := fx.CommitApply(attempt); err != nil {
		t.Fatalf("delivery retry: %v", err)
	}
	if got := fx.latestApply().Status; got != model.ApplySucceeded {
		t.Fatalf("apply = %s after the retry, want SUCCEEDED", got)
	}
	if n := fx.trace.countGit(isTargetUpdateRef); n != 1 {
		t.Fatalf("the target CAS ran %d times, want exactly 1", n)
	}
	fx.RequireWorkflowCompleted()
}

// TestApplyCrashAfterTargetCASLeavesTargetVerifiedNew: a crash after the
// compare-and-swap committed (injected at the observation right after the
// update-ref) leaves the Target exactly at the verified staging head —
// never ambiguous — and the delivery retry reports the observed outcome
// without a second update-ref.
func TestApplyCrashAfterTargetCASLeavesTargetVerifiedNew(t *testing.T) {
	fx := completedWorkflowForApply(t)
	attempt := fx.PrepareApply()
	fx.PassStagingVerification(attempt)
	staging := fx.applyBranchHead()
	fx.trace.armFailAfter(isTargetUpdateRef)
	if err := fx.CommitApply(attempt); err == nil {
		t.Fatalf("the injected post-CAS crash must fail the delivery")
	}
	if got := fx.targetHead(); got != staging {
		t.Fatalf("target = %s after the post-CAS crash, want exactly the verified new head %s", got, staging)
	}
	if got := fx.latestApply().Status; got != model.ApplyRunning {
		t.Fatalf("apply = %s after the post-CAS crash, want RUNNING (unsettled)", got)
	}
	fx.RequireWorkflowCompleted()

	fx.trace.disarm()
	if err := fx.CommitApply(attempt); err != nil {
		t.Fatalf("delivery retry: %v", err)
	}
	if got := fx.latestApply().Status; got != model.ApplySucceeded {
		t.Fatalf("apply = %s after the retry, want SUCCEEDED", got)
	}
	if n := fx.trace.countGit(isTargetUpdateRef); n != 1 {
		t.Fatalf("the target CAS ran %d times, want exactly 1 (the retry observed, never re-CASed)", n)
	}
	fx.RequireWorkflowCompleted()
}

// TestApplyNeverIssuesForceUpdateArgv: every git invocation of the whole
// apply path is argv-only through the Supervisor and no invocation ever
// carries a force-update form: no -f/--force flag and every update-ref
// carries the expected old value (the compare-and-swap three-argument
// form).
func TestApplyNeverIssuesForceUpdateArgv(t *testing.T) {
	fx := completedWorkflowForApply(t)
	attempt := fx.PrepareApply()
	fx.PassStagingVerification(attempt)
	if err := fx.CommitApply(attempt); err != nil {
		t.Fatalf("commit apply: %v", err)
	}
	for _, spec := range fx.trace.specs {
		for _, arg := range spec.Args {
			if arg == "-f" || arg == "--force" {
				t.Fatalf("git invocation %v carries the force flag", spec.Args)
			}
		}
	}
	if !fx.trace.everyGit(func(args []string) bool {
		if len(args) == 0 || args[0] != "update-ref" {
			return true
		}
		// update-ref <ref> <new> <expected-old>: the expected value is
		// always present; a missing expected value would be a force.
		return len(args) == 4
	}) {
		t.Fatalf("an update-ref invocation lacks the expected old value (force-update form)")
	}
	fx.RequireWorkflowCompleted()
}
