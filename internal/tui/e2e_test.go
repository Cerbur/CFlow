package tui

// Fake TUI E2E (TUI task 16): the deterministic, fully-Fake-provider
// lifecycle driven through the ACTUAL root TUI — the Bubble Tea Program
// runs the real root Model over a Fake terminal (an os.Pipe input) and
// the shared Application. Every key press flows through the TUI; the
// test never calls the Application for the lifecycle steps. The flow
// covers create → native discussion → plan/execution approvals →
// adoption → foreground runner → final report → protected apply →
// explicit cleanup, and asserts the authoritative Git/DB facts
// afterwards. No real Provider is ever invoked.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/cli"
	"cflow.local/cflow/internal/layout"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/platform"
	"cflow.local/cflow/internal/security"
)

// syncBuffer is the thread-safe screen capture: the renderer writes
// frames while the test polls for markers. The Bubble Tea diff renderer
// writes CHANGED text verbatim, so the E2E waits on the fragments that
// appear only when the model rendered the projection (new lines and
// changed values), plus the authoritative app state.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// visibleTerminalText removes terminal control sequences from the diff
// renderer's append-only capture so semantic markers remain contiguous.
func visibleTerminalText(s string) string {
	return xansi.Strip(s)
}

func TestVisibleTerminalTextStripsInterleavedControlSequences(t *testing.T) {
	got := visibleTerminalText("APPRO\x1b[2CE\x1b[0mD")
	if got != "APPROED" {
		t.Fatalf("visible terminal text = %q, want %q", got, "APPROED")
	}
	got = visibleTerminalText("APPROV\x1b[1;1HE\x1b[0mD")
	if got != "APPROVED" {
		t.Fatalf("interleaved control sequence was not removed: %q", got)
	}
}

// fakeTerminal keys: plain text is written verbatim; enter is CR; tab is
// HT; ctrl+c is 0x03; arrows are the CSI sequences.
const (
	keyEnter = "\r"
	keyTab   = "\t"
	keyEsc   = "\x1b"
	keyCtrlC = "\x03"
	keyRight = "\x1b[C"
	keyLeft  = "\x1b[D"
	keyUp    = "\x1b[A"
	keyDown  = "\x1b[B"
)

// TestTUIPlanToApplyAndCleanup drives the complete lifecycle through the
// real root TUI keyboard flow and asserts the authoritative Git and DB
// facts: the Target Working Tree HEAD/Index/files after the Apply, and
// the exact Cleanup deletion set.
func TestTUIPlanToApplyAndCleanup(t *testing.T) {
	fx := newTUIFixture(t)
	fx.stubFakeAgentOnPath()
	ref := &appRef{fx: fx, scripts: fx.fullFlowScripts()}

	termIn, termOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// The pipe ends are closed by the test cleanup after the program exits,
	// including the fatal-assertion path where the normal quit key is skipped.
	screen := syncBuffer{}

	prog := tea.NewProgram(
		newModel(Dependencies{
			CLI: cli.Dependencies{
				Build:           observe.BuildInfo{Version: "0.0.0-e2e", SourceCommit: "e2e"},
				OpenApplication: ref.open,
			},
		}),
		tea.WithInput(termIn),
		tea.WithOutput(&screen),
		tea.WithWindowSize(120, 40),
	)
	runDone := make(chan error, 1)
	runFinished := false
	t.Cleanup(func() {
		// Closing the writer first makes an input reader blocked on the pipe
		// observable as EOF; closing the reader then releases both descriptors
		// even when a fatal polling assertion aborts the test before the normal
		// quit path. On the happy path Run has already returned, so this is only
		// descriptor cleanup.
		_ = termOut.Close()
		_ = termIn.Close()
		if !runFinished {
			select {
			case <-runDone:
			case <-time.After(5 * time.Second):
			}
		}
	})
	go func() {
		_, err := prog.Run()
		runDone <- err
	}()

	// waitOutput blocks until the rendered screen contains the marker. The
	// Bubble Tea diff renderer may place cursor/style control sequences between
	// characters, so predicates always inspect the ANSI-stripped stream.
	waitOutput := func(marker string) {
		t.Helper()
		deadline := time.Now().Add(120 * time.Second)
		for !strings.Contains(visibleTerminalText(screen.String()), marker) {
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting for screen %q\n--- screen ---\n%s", marker, screen.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// waitApp blocks until the app predicate holds.
	waitApp := func(label string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(120 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting for app %q\n--- screen ---\n%s", label, screen.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// waitBuffer blocks until the visible screen buffer satisfies the
	// predicate. The Bubble Tea diff renderer writes cursor/style control
	// sequences between changed fragments; stripping them makes semantic
	// markers stable without changing the production renderer.
	waitBuffer := func(label string, pred func(string) bool) {
		t.Helper()
		deadline := time.Now().Add(120 * time.Second)
		for !pred(visibleTerminalText(screen.String())) {
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting for screen %q\n--- screen ---\n%s", label, screen.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	keys := func(s string) {
		if _, err := termOut.Write([]byte(s)); err != nil {
			t.Fatalf("write keys: %v", err)
		}
	}
	// The TUI deliberately runs each Application command in a background
	// Bubble Tea command. A committed fact can be queryable slightly before
	// that command releases the project-writer lock, so lifecycle steps use
	// the real advisory lock as a test-only completion barrier rather than
	// retrying mutating keys or relying on transient status text.
	locks, err := platform.OpenLockSet(filepath.Join(fx.home, "locks"), nil)
	if err != nil {
		t.Fatal(err)
	}
	projectKey := app.ProjectFor(fx.root).Key
	waitProjectWriterIdle := func(label string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		hold, err := locks.ProjectWriter(ctx, projectKey)
		if err != nil {
			t.Fatalf("waiting for %s: %v", label, err)
		}
		hold.Release()
	}

	// The initial workspace: the read-only load rendered the project.
	waitOutput("no workflows yet")

	// ---- create the workflow through the TUI form ----
	keys("n")
	waitOutput("create workflow")
	keys("calculator")
	// The Create page surfaces the target's dirty facts before the
	// confirmation; the E2E target is clean.
	waitOutput("target working tree: clean")
	waitOutput("will not touch your files")
	keys(keyEnter) // Enter only submits the name for the confirmation
	keys("y")      // only an explicit y creates (the clean target carries no dirty flag)
	waitApp("workflow created in application", func() bool { return len(ref.list()) == 1 })
	waitProjectWriterIdle("workflow creation")
	// The TUI processed the create (the workspace renders the workflow
	// with the status line).
	waitOutput("workflow created")
	waitOutput("REQUIREMENT_DISCUSSION")
	wf := ref.list()[0]

	// ---- native discussion ----
	keys(keyRight) // → discussion (the workspace arrows navigate)
	waitOutput("Start Native Discussion")
	keys(keyEnter) // Start Native Discussion (prepare + bridge turn)
	waitOutput("Continue Same Session")
	keys(keyDown)  // select Finish (Continue is the default selection)
	keys(keyEnter) // freeze the change set → handoff editor
	waitOutput("optional handoff guidance (JSON)")
	keys(keyEnter) // finish the discussion
	waitApp("discussion finished", func() bool {
		return sessionCompleted(t, ref.a, wf)
	})
	waitProjectWriterIdle("discussion finish")

	// ---- plan approval ----
	keys(keyTab) // → plan approval
	waitOutput("plan approval")
	keys("g") // generate the plan
	waitApp("plan generated", func() bool { return planRevision(t, ref.a, wf) >= 1 })
	waitProjectWriterIdle("plan generation")
	waitBuffer("plan hash rendered", func(s string) bool { return hexHash(s, "hash:") })
	keys("k") // independent check
	waitApp("plan checked", func() bool {
		return planStatus(t, ref.a, wf) == model.PlanChecked
	})
	waitProjectWriterIdle("plan check")
	// The Application fact can settle before the asynchronous Plan Approval
	// projection reload reaches the Bubble Tea model. Wait for the rendered
	// checked revision before sending the approval key; otherwise the key can
	// race the stale page model and be rejected as "no plan revision".
	waitBuffer("checked plan rendered", func(s string) bool {
		return strings.Contains(s, "CHECKED")
	})
	keys("y") // approve (explicit confirmation)
	waitApp("plan approved", func() bool { return planStatus(t, ref.a, wf) == model.PlanApproved })
	waitProjectWriterIdle("plan approval")
	waitBuffer("plan approval rendered", func(s string) bool {
		return strings.Contains(s, "APPROVED")
	})

	// ---- execution approval ----
	keys(keyTab) // → execution approval
	waitOutput("execution approval")
	keys("s") // generate the specs
	waitApp("spec session completed", func() bool {
		return sessionCompletedForPurpose(t, ref.a, wf, model.PurposeSpecGeneration)
	})
	waitApp("specs generated", func() bool { return workflowStage(t, ref.a, wf) == model.StageWorkflowGeneration })
	waitApp("spec artifacts ready", func() bool {
		resolver := layout.Resolver{Home: fx.home, ProjectKey: app.ProjectFor(fx.root).Key}
		store, err := artifact.NewWorkflow(resolver.WorkflowRoot(wf), wf, security.Registry{})
		if err != nil {
			return false
		}
		if _, err := store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactSpec}); err != nil {
			return false
		}
		_, err = store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactCatalog})
		return err == nil
	})
	waitProjectWriterIdle("spec generation")
	// The authoritative artifact facts above and the lock barrier prove the
	// the next mutating key is sent; no retry is needed.
	// The execution preview is intentionally incomplete until compilation has
	// run, so the approval page is rendered as "no preview yet" here. Its
	// compile action is still the deterministic TUI synchronization point; wait
	// for that action before issuing the single mutating compile command.
	waitBuffer("compile action rendered", func(s string) bool {
		return strings.Contains(s, "w compile workflow")
	})
	keys("w") // compile the workflow
	waitApp("workflow compiled", func() bool {
		return executionPreview(t, ref.a, wf).WorkflowHash != ""
	})
	waitProjectWriterIdle("workflow compilation")
	// The dry run requires the compiled workflow. The authoritative wait above
	// is followed by the single mutating dry-run command.
	keys("d") // execution dry run
	waitApp("dry run ready", func() bool {
		return executionPreview(t, ref.a, wf).CommitPolicyHash != ""
	})
	waitProjectWriterIdle("execution dry run")
	// Wait for the refreshed approval page before sending the mutating
	// confirmation. The commit-policy line is absent from the pre-dry-run
	// page, so this cannot be satisfied by an earlier stale frame.
	preview := executionPreview(t, ref.a, wf)
	waitBuffer("execution preview rendered", func(s string) bool {
		// The append-only diff-renderer capture cannot associate a changed
		// value with its original line after cursor movement. The commit
		// policy hash is absent before dry-run, so its rendered short hash is
		// an unambiguous post-dry-run projection marker.
		return strings.Contains(s, preview.CommitPolicyHash[:12])
	})
	keys("y") // approve the execution (binds the frozen change set)
	waitApp("execution approved", func() bool {
		return workflowRuntime(t, ref.a, wf) == model.RuntimeRunning &&
			workflowStage(t, ref.a, wf) == model.StageExecution
	})
	waitProjectWriterIdle("execution approval")

	// ---- execution + workspace adoption + foreground runner ----
	// The Execution page first renders the stale projection (the dry run
	// paused the workflow, so Resume is still a legal action) and only after
	// the post-approval projection reload renders the reloaded signal: the
	// workflow is now RUNNING, so Resume is no longer a legal action and the
	// hint drops the resume ("start the runner" instead of "resume & run").
	// The marker is the diff renderer's changed fragment ("start the
	// runner") — the hint keeps the common "r " prefix on screen, so the
	// contiguous "r start the runner" is not guaranteed to be written.
	// Waiting on the fragment guards the reload — pressing r before it would
	// hit the stale Resume projection.
	waitOutput("start the run")
	keys("r") // start the runner
	// The runner stops at the Workspace Adoption Gate (the execution
	// approval bound the frozen Change Set).
	waitBuffer("decision panel rendered", func(s string) bool {
		return strings.Contains(s, "decision required:")
	})
	// The decision panel is rendered before the single mutating adoption
	// command, avoiding duplicate adoption requests.
	keys("a") // adopt the workspace
	waitApp("workspace adopted", func() bool { return workspaceAdopted(t, ref.a, wf) })
	waitProjectWriterIdle("workspace adoption")
	keys("r") // run again
	waitApp("workflow completed", func() bool { return workflowStage(t, ref.a, wf) == model.StageCompleted })

	// ---- final report ----
	keys(keyTab) // → blocked
	keys(keyTab) // → terminal
	waitOutput("terminal")
	keys("r") // render the final report
	waitBuffer("final report rendered", func(s string) bool {
		return strings.Contains(s, "# CFlow Execution Report")
	})

	// ---- protected apply ----
	keys(keyRight) // → apply section
	keys("p")      // stage the apply
	waitBuffer("apply preview rendered", func(s string) bool {
		return strings.Contains(s, "AWAITING_CONFIRMATION")
	})
	// The explicit delivery: Enter alone must not deliver; y delivers.
	keys(keyEnter)
	waitApp("apply not delivered by enter", func() bool {
		return applyStatus(t, ref.a, wf) == model.ApplyAwaitingConfirmation
	})
	keys("y")
	waitApp("apply delivered", func() bool { return applyStatus(t, ref.a, wf) == model.ApplySucceeded })

	// The Target Working Tree: HEAD, Index, and files are synchronized
	// with the delivered Apply head.
	requireAppliedWorkingTree(t, fx, ref.a, wf)

	// Snapshot the preserved workflow directories (everything except the
	// code directories the Cleanup may delete) before the Cleanup.
	preserved := snapshotWorkflowEntries(t, fx, wf)

	// ---- explicit cleanup ----
	keys(keyRight) // → cleanup section
	keys("c")      // cleanup dry run
	waitOutput("cleanup dry run manifest ready")
	waitApp("cleanup manifest", func() bool { return cleanupStatus(t, ref.a, wf) != "" })
	keys("y") // execute the bound manifest
	waitApp("cleanup executed", func() bool { return cleanupStatus(t, ref.a, wf) == model.CleanupStatusSucceeded })

	// The Cleanup deleted exactly the code directories and preserved the
	// artifacts, evidence, report, database, and refs.
	requireCleanupPreservation(t, fx, wf, preserved)

	// quit through the normal key (no runner is active anymore).
	keys("q")
	err = <-runDone
	runFinished = true
	if err != nil {
		t.Fatalf("tui run: %v", err)
	}
}

// handoffContentJSON remains available for tests that exercise optional
// structured guidance. The normal E2E path leaves the editor empty so the
// managed resume uses the existing discussion context.
const handoffContentJSON = `{"targets":"division by zero must error","constraints":"no external dependencies","non_goals":"no other arithmetic changes","acceptance_criteria":"Divide returns a typed error on zero","open_questions":"error wording","user_decisions":[{"topic":"error type","decision":"typed error"}]}`

// hexHash reports whether s contains the prefix followed by at least 12
// lowercase hex digits (the rendered plan/preview hash lines).
func hexHash(s, prefix string) bool {
	idx := strings.Index(s, prefix)
	if idx < 0 {
		return false
	}
	rest := strings.TrimLeft(s[idx+len(prefix):], " ")
	n := 0
	for _, c := range rest {
		if !strings.ContainsRune("0123456789abcdef", c) {
			break
		}
		n++
	}
	return n >= 12
}

// ---------------------------------------------------------------------------
// authoritative app facts the test waits on
// ---------------------------------------------------------------------------

func (r *appRef) list() []model.WorkflowID {
	view, err := r.a.Query(context.Background(), app.ListQuery{})
	if err != nil {
		return nil
	}
	var ids []model.WorkflowID
	for _, w := range view.(app.ListView).Workflows {
		ids = append(ids, w.ID)
	}
	return ids
}

func statusOf(t *testing.T, a *app.Application, wf model.WorkflowID) app.StatusView {
	t.Helper()
	view, err := a.Query(context.Background(), app.StatusQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return view.(app.StatusView)
}

func workflowStage(t *testing.T, a *app.Application, wf model.WorkflowID) model.WorkflowStage {
	t.Helper()
	return statusOf(t, a, wf).Stage
}

func workflowRuntime(t *testing.T, a *app.Application, wf model.WorkflowID) model.RuntimeStatus {
	t.Helper()
	return statusOf(t, a, wf).Runtime
}

func sessionCompleted(t *testing.T, a *app.Application, wf model.WorkflowID) bool {
	t.Helper()
	view, err := a.Query(context.Background(), app.InspectQuery{Workflow: wf})
	if err != nil {
		return false
	}
	sessions := view.(app.InspectView).Sessions
	if len(sessions) == 0 {
		return false
	}
	return sessions[len(sessions)-1].Status == model.SessionCompleted
}

func sessionCompletedForPurpose(t *testing.T, a *app.Application, wf model.WorkflowID, purpose model.AgentPurpose) bool {
	t.Helper()
	view, err := a.Query(context.Background(), app.InspectQuery{Workflow: wf})
	if err != nil {
		return false
	}
	for i := len(view.(app.InspectView).Sessions) - 1; i >= 0; i-- {
		session := view.(app.InspectView).Sessions[i]
		if session.Purpose == purpose {
			return session.Status == model.SessionCompleted
		}
	}
	return false
}

func planRevision(t *testing.T, a *app.Application, wf model.WorkflowID) int {
	t.Helper()
	view, err := a.Query(context.Background(), app.PlanQuery{Workflow: wf})
	if err != nil {
		return 0
	}
	return view.(app.PlanView).Revision
}

func planStatus(t *testing.T, a *app.Application, wf model.WorkflowID) model.PlanStatus {
	t.Helper()
	view, err := a.Query(context.Background(), app.PlanQuery{Workflow: wf})
	if err != nil {
		return ""
	}
	return view.(app.PlanView).PlanStatus
}

func executionPreview(t *testing.T, a *app.Application, wf model.WorkflowID) app.ExecutionPreviewView {
	t.Helper()
	view, err := a.Query(context.Background(), app.ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		return app.ExecutionPreviewView{}
	}
	return view.(app.ExecutionPreviewView)
}

func workspaceAdopted(t *testing.T, a *app.Application, wf model.WorkflowID) bool {
	t.Helper()
	view, err := a.Query(context.Background(), app.ProjectWorkspaceQuery{Selected: wf})
	if err != nil {
		return false
	}
	lc := view.(app.WorkspaceView).Lifecycle
	return lc != nil && lc.Adopted
}

func applyStatus(t *testing.T, a *app.Application, wf model.WorkflowID) model.ApplyStatus {
	t.Helper()
	view, err := a.Query(context.Background(), app.InspectQuery{Workflow: wf})
	if err != nil {
		return ""
	}
	attempts := view.(app.InspectView).ApplyAttempts
	if len(attempts) == 0 {
		return ""
	}
	return attempts[len(attempts)-1].Status
}

func cleanupStatus(t *testing.T, a *app.Application, wf model.WorkflowID) model.CleanupStatus {
	t.Helper()
	view, err := a.Query(context.Background(), app.InspectQuery{Workflow: wf})
	if err != nil {
		return ""
	}
	attempts := view.(app.InspectView).CleanupAttempts
	if len(attempts) == 0 {
		return ""
	}
	return attempts[len(attempts)-1].Status
}

// ---------------------------------------------------------------------------
// authoritative Git facts
// ---------------------------------------------------------------------------

// requireAppliedWorkingTree asserts the Apply updated the original
// working tree: HEAD equals the reviewed Apply head the Workflow
// recorded, the index and the worktree are clean, the delivered file
// content is on disk, and the cflow audit refs exist.
func requireAppliedWorkingTree(t *testing.T, fx *tuiFixture, a *app.Application, wf model.WorkflowID) {
	t.Helper()
	head, err := fx.gitOK("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("target head: %v", err)
	}
	if head == "" {
		t.Fatal("target working tree has no HEAD after the apply")
	}
	// The delivered head is the exact staging head the Apply recorded.
	view, err := a.Query(context.Background(), app.InspectQuery{Workflow: wf})
	if err != nil {
		t.Fatal(err)
	}
	attempts := view.(app.InspectView).ApplyAttempts
	if len(attempts) == 0 || attempts[len(attempts)-1].StagingHead == "" {
		t.Fatalf("the apply recorded no delivered head: %+v", attempts)
	}
	if head != attempts[len(attempts)-1].StagingHead {
		t.Fatalf("target head %s != the delivered apply head %s", head, attempts[len(attempts)-1].StagingHead)
	}
	// The index and worktree are clean (no staged, unstaged, or
	// untracked residue).
	if out, err := fx.gitOK("status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("target working tree not clean after the apply: %q (%v)", out, err)
	}
	// The delivered file is present with the verified content.
	content, err := os.ReadFile(filepath.Join(fx.root, "src", "divide", "divide.go"))
	if err != nil {
		t.Fatalf("delivered file: %v", err)
	}
	if !strings.Contains(string(content), "func Divide(a, b int) (int, error)") {
		t.Fatalf("delivered file content = %q", content)
	}
	// The cflow audit refs exist (the branch mainline and the audits).
	refs, err := fx.gitOK("for-each-ref", "--format=%(refname)", "refs/cflow/")
	if err != nil || strings.TrimSpace(refs) == "" {
		t.Fatalf("no cflow refs after the apply: %q (%v)", refs, err)
	}
}

// snapshotWorkflowEntries records the workflow directories that must
// survive the Cleanup: the aggregated root's non-code entries (the
// Cleanup may delete only the workspace and the tmp worktrees).
func snapshotWorkflowEntries(t *testing.T, fx *tuiFixture, wf model.WorkflowID) []string {
	t.Helper()
	wfRoot := workflowRoot(t, fx, wf)
	entries, err := os.ReadDir(wfRoot)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() == "workspace" || e.Name() == "tmp" {
			continue
		}
		names = append(names, e.Name())
	}
	return names
}

// workflowRoot resolves the aggregated root of one workflow.
func workflowRoot(t *testing.T, fx *tuiFixture, wf model.WorkflowID) string {
	t.Helper()
	root := filepath.Join(fx.home, "projects")
	projectDirs, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(projectDirs) != 1 {
		t.Fatalf("project dirs = %v", projectDirs)
	}
	return filepath.Join(root, projectDirs[0].Name(), string(wf))
}

// requireCleanupPreservation asserts the Cleanup deleted exactly the
// aggregated code directories (workspace, tmp/tasks/*, tmp/apply-*) and
// preserved every other workflow directory, the artifacts, the
// database, and the refs.
func requireCleanupPreservation(t *testing.T, fx *tuiFixture, wf model.WorkflowID, preserved []string) {
	t.Helper()
	wfRoot := workflowRoot(t, fx, wf)

	// Deleted: the aggregated code directories — the Workspace and
	// every Task/Apply worktree (the empty tmp parent may remain).
	if _, err := os.Stat(filepath.Join(wfRoot, "workspace")); err == nil {
		t.Fatalf("cleanup left the workspace behind")
	}
	tmp := filepath.Join(wfRoot, "tmp")
	if entries, err := os.ReadDir(tmp); err == nil {
		for _, e := range entries {
			if e.IsDir() && e.Name() == "tasks" {
				sub, err2 := os.ReadDir(filepath.Join(tmp, "tasks"))
				if err2 == nil && len(sub) > 0 {
					t.Fatalf("cleanup left tmp/tasks entries behind: %v", sub)
				}
				continue
			}
			t.Fatalf("cleanup left tmp/%s behind", e.Name())
		}
	}
	// Preserved: every non-code aggregated-root entry that existed
	// before the Cleanup.
	for _, name := range preserved {
		if _, err := os.Stat(filepath.Join(wfRoot, name)); err != nil {
			t.Fatalf("cleanup deleted the preserved %s directory: %v", name, err)
		}
	}
	// Preserved: every immutable Artifact under its fixed aggregated
	// category/type directory.
	for name, rel := range map[string]string{
		"plan":               "plans/plan",
		"plan-check":         "reviews/plan-check",
		"spec":               "specs/spec",
		"catalog":            "specs/catalog",
		"workflow":           "workflows/workflow",
		"discussion-handoff": "discussion/discussion-handoff",
		"change-set":         "evidence/change-set",
		"report":             "reports/report",
		"routing-policy":     "evidence/routing-policy",
		"budget-policy":      "evidence/budget-policy",
	} {
		if _, err := os.Stat(filepath.Join(wfRoot, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("cleanup deleted the %s artifacts: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fx.home, "cflow.db")); err != nil {
		t.Fatalf("cleanup deleted the authoritative database: %v", err)
	}
	// Preserved: the git refs.
	refs, err := fx.gitOK("for-each-ref", "--format=%(refname)", "refs/cflow/")
	if err != nil || strings.TrimSpace(refs) == "" {
		t.Fatalf("cleanup removed the cflow refs: %q (%v)", refs, err)
	}
}
