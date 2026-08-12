package tui

// Fake TUI E2E (TUI task 11): the deterministic, fully-Fake-provider
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
	"reflect"
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

func TestTUIWorkflowMenuResumeActionPreview(t *testing.T) {
	var log bytes.Buffer
	base := &resumeWorkflowController{menu: app.WorkflowMenuView{
		Workflow: "wf-paused",
		Name:     "calculator",
		Runtime:  model.RuntimePaused,
		Entries: []app.WorkflowMenuEntry{{
			ID: "resume", Kind: app.MenuEntryAction, Label: "Resume",
			Action: app.MenuActionResume,
		}},
		DefaultIndex: 0,
	}, workspace: app.WorkspaceView{
		Selected:     "wf-paused",
		Workflows:    []app.WorkflowSummary{{ID: "wf-paused", Name: "calculator", Runtime: model.RuntimeRunning}},
		Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-paused", Name: "calculator", Runtime: model.RuntimeRunning}},
		LegalActions: []app.LegalAction{{Kind: model.PauseWorkflow, Label: "Pause"}},
	}}
	ctrl := &recordingController{ctrl: base}
	m := newModel(Dependencies{OperationLog: &log})
	m.ctrl = ctrl
	m.selected = "wf-paused"
	m.workspace.Rows = []WorkflowRow{{Kind: WorkflowRowExisting, ID: "wf-paused", Name: "calculator"}}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.page != PageWorkflowMenu {
		t.Fatalf("Home Enter page = %v, want Workflow Menu", m.page)
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.page != PageActionPreview {
		t.Fatalf("Resume menu entry page = %v, want Action Preview", m.page)
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.actionPreviewed == false || len(ctrl.executed) != 0 {
		t.Fatalf("first Action Preview Enter = previewed %v executes %d, want preview only", m.actionPreviewed, len(ctrl.executed))
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.page != PageExecution || len(ctrl.executed) != 1 {
		t.Fatalf("confirmed Resume page = %v executes %d, want Execution and one typed command", m.page, len(ctrl.executed))
	}
	resume, ok := ctrl.executed[0].(app.ResumeWorkflowCommand)
	if !ok || !reflect.DeepEqual(resume, app.ResumeWorkflowCommand{Workflow: "wf-paused"}) {
		t.Fatalf("executed command = %#v, want ResumeWorkflowCommand{Workflow: wf-paused}", ctrl.executed[0])
	}
	if m.commandInFlight() {
		t.Fatal("Resume acknowledgement left command in flight")
	}
	if m.workspace.Lifecycle == nil || !hasAction(m.workspace.Actions, ActionPause) {
		t.Fatalf("refreshed Workspace facts/legal actions = %+v/%v, want running + pause", m.workspace.Lifecycle, m.workspace.Actions)
	}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.page != PageWorkflowMenu {
		t.Fatalf("Execution Esc page = %v, want Workflow Menu", m.page)
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.page != PageWorkspace {
		t.Fatalf("Workflow Menu Esc page = %v, want Home", m.page)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	m = step(t, m, tea.KeyPressMsg{Text: "exit"})
	_, quit := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if quit == nil {
		t.Fatal("/exit Enter did not return the TUI quit command")
	}
}

type resumeWorkflowController struct {
	menu      app.WorkflowMenuView
	workspace app.WorkspaceView
}

func (*resumeWorkflowController) Execute(context.Context, app.Command) (app.Outcome, error) {
	return app.Outcome{}, nil
}

func (c *resumeWorkflowController) Query(_ context.Context, q app.Query) (app.View, error) {
	switch q.(type) {
	case app.WorkflowMenuQuery:
		return c.menu, nil
	case app.ProjectWorkspaceQuery:
		return c.workspace, nil
	default:
		return nil, model.InvalidInputFault("unexpected query")
	}
}

func (*resumeWorkflowController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}

func (*resumeWorkflowController) EscalateStop() {}

func TestTUIFirstKeyboardJourneyReturnsFromDiscussionToHome(t *testing.T) {
	ctrl := &firstKeyboardJourneyController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.workspace.Rows = []WorkflowRow{{Kind: WorkflowRowNew, Name: "NEW WORKFLOW"}}

	// Home → New Workflow → name → Create Preview → Create: both confirms
	// are Enter-only.
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = step(t, m, tea.KeyPressMsg{Text: "calculator"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.page != PageWorkflowMenu {
		t.Fatalf("after create page = %v, want Workflow Menu", m.page)
	}
	if len(ctrl.executed) != 1 {
		t.Fatalf("create executions = %d, want one", len(ctrl.executed))
	}
	if _, ok := ctrl.executed[0].(app.CreateWorkflowCommand); !ok {
		t.Fatalf("create command = %#v", ctrl.executed[0])
	}

	// Workflow Menu → Start Native Discussion → deterministic fake session.
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.page != PageDiscussion {
		t.Fatalf("Start Native Discussion page = %v, want Discussion Return", m.page)
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.page != PageDiscussion || !ctrl.discussionStarted {
		t.Fatalf("fake discussion start page=%v started=%v", m.page, ctrl.discussionStarted)
	}

	// Discussion Return → Workflow Menu → Home. These are actual stack pops,
	// not direct page assignments.
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.page != PageWorkflowMenu {
		t.Fatalf("Discussion Esc page = %v, want Workflow Menu", m.page)
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.page != PageWorkspace {
		t.Fatalf("Workflow Menu Esc page = %v, want Home", m.page)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	m = step(t, m, tea.KeyPressMsg{Text: "exit"})
	_, quit := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if quit == nil {
		t.Fatal("/exit Enter did not return the TUI quit command")
	}
}

type firstKeyboardJourneyController struct {
	executed          []app.Command
	discussionStarted bool
}

func (c *firstKeyboardJourneyController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	switch cmd.(type) {
	case app.CreateWorkflowCommand:
		return app.Outcome{Workflow: "wf-first"}, nil
	case app.PrepareNativeDiscussionCommand:
		c.discussionStarted = true
	}
	return app.Outcome{}, nil
}

func (c *firstKeyboardJourneyController) Query(_ context.Context, q app.Query) (app.View, error) {
	switch q := q.(type) {
	case app.DiscoveryQuery:
		return app.DiscoveryView{Root: "/repo", Branch: "main", Head: "head", ProjectKey: "repo"}, nil
	case app.ProjectWorkspaceQuery:
		if !hasWorkflowCommand(c.executed, app.CreateWorkflowCommand{}) {
			return app.WorkspaceView{}, nil
		}
		return app.WorkspaceView{
			Selected:  q.Selected,
			Workflows: []app.WorkflowSummary{{ID: "wf-first", Name: "calculator", Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePaused}},
			Lifecycle: &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-first", Name: "calculator", Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePaused}},
		}, nil
	case app.WorkflowMenuQuery:
		return app.WorkflowMenuView{
			Workflow: q.Workflow,
			Name:     "calculator", Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePaused,
			Entries: []app.WorkflowMenuEntry{{
				ID: "start-discussion", Group: app.MenuGroupContinue, Kind: app.MenuEntryAction,
				Label: "Start Native Discussion", Route: app.MenuRouteDiscussion, Action: app.MenuActionStartDiscussion,
			}}, DefaultIndex: 0,
		}, nil
	case app.DiscussionReturnQuery:
		view := app.DiscussionReturnView{Workflow: q.Workflow, Provider: "fake"}
		if c.discussionStarted {
			view.Session = "fake-session"
			view.Actions = []string{"continue"}
		}
		return view, nil
	default:
		return nil, model.InvalidInputFault("unexpected query")
	}
}

func (c *firstKeyboardJourneyController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}

func (*firstKeyboardJourneyController) EscalateStop() {}

func hasWorkflowCommand(commands []app.Command, want app.Command) bool {
	for _, command := range commands {
		switch want.(type) {
		case app.CreateWorkflowCommand:
			if _, ok := command.(app.CreateWorkflowCommand); ok {
				return true
			}
		}
	}
	return false
}

// fakeTerminal keys: plain text is written verbatim; enter is CR; ctrl+c is
// 0x03; arrows are the CSI sequences.
const (
	keyEnter = "\r"
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
		if !runFinished {
			// Fatal polling assertions skip the normal /exit path. Kill the
			// Bubble Tea program explicitly, then wait for Run to return
			// before releasing the pipe descriptors. This prevents a failed
			// test from leaving the input reader or renderer goroutine alive.
			prog.Kill()
			select {
			case <-runDone:
				runFinished = true
			case <-time.After(5 * time.Second):
				t.Errorf("tui program did not stop during test cleanup")
			}
		}
		// Closing the writer first makes an input reader blocked on the pipe
		// observable as EOF; closing the reader then releases both descriptors.
		_ = termOut.Close()
		_ = termIn.Close()
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
	waitApp := func(label string, cond func(context.Context) bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		for !cond(ctx) {
			if ctx.Err() != nil {
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
	keys(keyDown)  // select NEW WORKFLOW
	keys(keyEnter) // enter the Create Workspace form
	waitOutput("create workflow")
	keys("calculator")
	// The Create page surfaces the target's dirty facts before the
	// confirmation; the E2E target is clean.
	waitOutput("target working tree: clean")
	waitOutput("will not touch your files")
	keys(keyEnter) // Enter only submits the name for the confirmation
	keys(keyEnter) // Enter creates from the exact preview
	waitApp("workflow created in application", func(ctx context.Context) bool { return len(ref.listContext(ctx)) == 1 })
	waitProjectWriterIdle("workflow creation")
	// The TUI processed the create (the workspace renders the workflow
	// with the status line).
	waitOutput("workflow created")
	waitOutput("REQUIREMENT_DISCUSSION")
	factCtx, factCancel := context.WithTimeout(context.Background(), 30*time.Second)
	wfIDs := ref.listContext(factCtx)
	factCancel()
	if len(wfIDs) != 1 {
		t.Fatalf("workflow list after creation = %v", wfIDs)
	}
	wf := wfIDs[0]

	// ---- native discussion ----
	waitOutput("Start Native Discussion")
	keys(keyEnter) // Start Native Discussion (prepare + bridge turn)
	waitOutput("Continue Same Session")
	keys(keyDown)  // select Finish (Continue is the default selection)
	keys(keyEnter) // freeze the change set → handoff editor
	waitOutput("optional handoff guidance (JSON)")
	keys(keyEnter) // finish the discussion
	waitApp("discussion finished", func(ctx context.Context) bool {
		return sessionCompleted(ctx, t, ref.a, wf)
	})
	waitProjectWriterIdle("discussion finish")
	waitOutput("discussion finished")

	// ---- plan approval ----
	keys(keyEsc)   // Discussion Return → Workflow Menu
	keys(keyEsc)   // Workflow Menu → Home
	keys(keyEnter) // reopen the selected Workflow Menu
	keys(keyDown)  // select the next available action route
	keys(keyEnter) // open the selected stage
	waitOutput("plan approval")
	keys("g") // generate the plan
	waitApp("plan generated", func(ctx context.Context) bool { return planRevision(ctx, t, ref.a, wf) >= 1 })
	waitProjectWriterIdle("plan generation")
	waitBuffer("plan hash rendered", func(s string) bool { return hexHash(s, "hash:") })
	keys("k") // independent check
	waitApp("plan checked", func(ctx context.Context) bool {
		return planStatus(ctx, t, ref.a, wf) == model.PlanChecked
	})
	waitProjectWriterIdle("plan check")
	// The Application fact can settle before the asynchronous Plan Approval
	// projection reload reaches the Bubble Tea model. Wait for the rendered
	// checked revision before sending the approval key; otherwise the key can
	// race the stale page model and be rejected as "no plan revision".
	waitBuffer("checked plan rendered", func(s string) bool {
		return strings.Contains(s, "CHECKED")
	})
	keys(keyEnter) // first Enter opens the exact approval preview
	keys(keyEnter) // second Enter approves
	waitApp("plan approved", func(ctx context.Context) bool { return planStatus(ctx, t, ref.a, wf) == model.PlanApproved })
	waitProjectWriterIdle("plan approval")
	// After approval the plan page advances to the next lifecycle stage. The
	// diff renderer may split or overwrite the APPROVED status text in its
	// append-only capture, so use the stage transition as the stable marker
	// that the refreshed Plan projection has arrived.
	waitBuffer("plan approval advanced", func(s string) bool {
		return strings.Contains(s, "SPEC_GENERATION")
	})

	// ---- execution approval ----
	keys(keyEsc)   // return to Workflow Menu
	keys(keyEsc)   // return to Home
	keys(keyEnter) // reopen the selected Workflow Menu
	keys(keyDown)  // select the next available action route
	keys(keyEnter) // open Execution Approval
	waitOutput("execution approval")
	keys("s") // generate the specs
	waitApp("spec session completed", func(ctx context.Context) bool {
		return sessionCompletedForPurpose(ctx, t, ref.a, wf, model.PurposeSpecGeneration)
	})
	waitApp("specs generated", func(ctx context.Context) bool {
		return workflowStage(ctx, t, ref.a, wf) == model.StageWorkflowGeneration
	})
	waitOutput("specs generated")
	waitApp("spec artifacts ready", func(ctx context.Context) bool {
		resolver := layout.Resolver{Home: fx.home, ProjectKey: app.ProjectFor(fx.root).Key}
		store, err := artifact.NewWorkflow(resolver.WorkflowRoot(wf), wf, security.Registry{})
		if err != nil {
			return false
		}
		if _, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactSpec}); err != nil {
			return false
		}
		_, err = store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactCatalog})
		return err == nil
	})
	waitProjectWriterIdle("spec generation")
	// The authoritative artifact facts above and the lock barrier prove the
	// previous command settled; no retry is needed. The execution approval
	// page remains active while compilation is requested, so send the single
	// mutating compile command directly rather than waiting on a transient
	// footer fragment that may already exist in the append-only capture.
	keys("w") // compile the workflow
	waitApp("workflow compiled", func(ctx context.Context) bool {
		return executionPreview(ctx, t, ref.a, wf).WorkflowHash != ""
	})
	waitOutput("workflow compiled")
	waitProjectWriterIdle("workflow compilation")
	// The dry run requires the compiled workflow. The authoritative wait above
	// is followed by the single mutating dry-run command.
	keys("d") // execution dry run
	waitApp("dry run ready", func(ctx context.Context) bool {
		return executionPreview(ctx, t, ref.a, wf).CommitPolicyHash != ""
	})
	waitOutput("execution dry run complete")
	waitProjectWriterIdle("execution dry run")
	// Wait for the refreshed approval page before sending the mutating
	// confirmation. The commit-policy line is absent from the pre-dry-run
	// page, so this cannot be satisfied by an earlier stale frame.
	previewCtx, previewCancel := context.WithTimeout(context.Background(), 30*time.Second)
	preview := executionPreview(previewCtx, t, ref.a, wf)
	previewCancel()
	if len(preview.CommitPolicyHash) < 12 {
		t.Fatalf("dry-run commit policy hash = %q, want at least 12 characters", preview.CommitPolicyHash)
	}
	waitBuffer("execution preview rendered", func(s string) bool {
		// The append-only diff-renderer capture cannot associate a changed
		// value with its original line after cursor movement. The commit
		// policy hash is absent before dry-run, so its rendered short hash is
		// an unambiguous post-dry-run projection marker.
		return strings.Contains(s, preview.CommitPolicyHash[:12])
	})
	keys(keyEnter) // first Enter opens the exact execution preview
	keys(keyEnter) // second Enter approves the execution
	waitApp("execution approved", func(ctx context.Context) bool {
		return workflowRuntime(ctx, t, ref.a, wf) == model.RuntimeRunning &&
			workflowStage(ctx, t, ref.a, wf) == model.StageExecution
	})
	waitProjectWriterIdle("execution approval")
	// The authoritative DB facts can settle before the TUI applies the
	// workspace acknowledgement. This status is emitted only by that
	// projection acknowledgement, so it prevents a stale "start the run"
	// hint from accepting the next key under -race.
	waitOutput("execution approved")

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
	waitApp("workspace adopted", func(ctx context.Context) bool { return workspaceAdopted(ctx, t, ref.a, wf) })
	waitProjectWriterIdle("workspace adoption")
	// commandDoneMsg clears the decision and triggers the projection reload;
	// wait for its unique status acknowledgement before issuing the next key.
	waitOutput("workspace adopted")
	keys("r") // run again
	waitApp("workflow completed", func(ctx context.Context) bool { return workflowStage(ctx, t, ref.a, wf) == model.StageCompleted })
	// The DB stage can settle before runnerDoneMsg reaches the Bubble Tea
	// model. This acknowledgement proves m.running is cleared before the
	// test navigates and later quits.
	waitOutput("runner: terminal")

	// ---- final report ----
	keys(keyEsc)   // Execution → Workflow Menu
	keys(keyEsc)   // Workflow Menu → Home
	keys(keyEnter) // reopen the selected Workflow Menu
	keys(keyDown)  // select the terminal action route
	keys(keyEnter) // open Terminal
	// The terminal page has a unique section marker; the generic word
	// "terminal" is already present in the preceding runner status.
	waitOutput(">report")
	keys("r") // render the final report
	waitBuffer("final report rendered", func(s string) bool {
		return strings.Contains(s, "# CFlow Execution Report")
	})

	// ---- protected apply ----
	keys(keyRight) // → apply section within Terminal
	keys("p")      // stage the apply
	waitBuffer("apply preview rendered", func(s string) bool {
		return strings.Contains(s, "AWAITING_CONFIRMATION")
	})
	waitOutput("apply staged (preview ready)")
	// The explicit delivery requires the second Enter; the first only previews.
	keys(keyEnter)
	waitApp("apply not delivered by enter", func(ctx context.Context) bool {
		return applyStatus(ctx, t, ref.a, wf) == model.ApplyAwaitingConfirmation
	})
	keys(keyEnter)
	waitApp("apply delivered", func(ctx context.Context) bool { return applyStatus(ctx, t, ref.a, wf) == model.ApplySucceeded })
	waitOutput("apply delivered")

	// The Target Working Tree: HEAD, Index, and files are synchronized
	// with the delivered Apply head.
	requireAppliedWorkingTree(t, fx, ref.a, wf)

	// Snapshot the preserved workflow directories (everything except the
	// code directories the Cleanup may delete) before the Cleanup.
	preserved := snapshotWorkflowEntries(t, fx, wf)

	// ---- explicit cleanup ----
	keys(keyRight) // → cleanup section within Terminal
	keys("c")      // cleanup dry run
	waitOutput("cleanup dry run manifest ready")
	waitApp("cleanup manifest", func(ctx context.Context) bool { return cleanupStatus(ctx, t, ref.a, wf) != "" })
	waitProjectWriterIdle("cleanup dry run")
	keys(keyEnter) // preview the bound manifest
	keys(keyEnter) // execute the bound manifest
	waitApp("cleanup executed", func(ctx context.Context) bool {
		return cleanupStatus(ctx, t, ref.a, wf) == model.CleanupStatusSucceeded
	})
	waitOutput("cleanup executed")

	// The Cleanup deleted exactly the code directories and preserved the
	// artifacts, evidence, report, database, and refs.
	requireCleanupPreservation(t, fx, wf, preserved)

	// q is ordinary input; /exit Enter is the only idle exit path.
	keys("q")
	keys("/exit" + keyEnter)
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
	return r.listContext(context.Background())
}

func (r *appRef) listContext(ctx context.Context) []model.WorkflowID {
	view, err := r.a.Query(ctx, app.ListQuery{})
	if err != nil {
		return nil
	}
	var ids []model.WorkflowID
	for _, w := range view.(app.ListView).Workflows {
		ids = append(ids, w.ID)
	}
	return ids
}

func statusOf(ctx context.Context, t *testing.T, a *app.Application, wf model.WorkflowID) app.StatusView {
	t.Helper()
	view, err := a.Query(ctx, app.StatusQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return view.(app.StatusView)
}

func workflowStage(ctx context.Context, t *testing.T, a *app.Application, wf model.WorkflowID) model.WorkflowStage {
	t.Helper()
	return statusOf(ctx, t, a, wf).Stage
}

func workflowRuntime(ctx context.Context, t *testing.T, a *app.Application, wf model.WorkflowID) model.RuntimeStatus {
	t.Helper()
	return statusOf(ctx, t, a, wf).Runtime
}

func sessionCompleted(ctx context.Context, t *testing.T, a *app.Application, wf model.WorkflowID) bool {
	t.Helper()
	view, err := a.Query(ctx, app.InspectQuery{Workflow: wf})
	if err != nil {
		return false
	}
	sessions := view.(app.InspectView).Sessions
	if len(sessions) == 0 {
		return false
	}
	return sessions[len(sessions)-1].Status == model.SessionCompleted
}

func sessionCompletedForPurpose(ctx context.Context, t *testing.T, a *app.Application, wf model.WorkflowID, purpose model.AgentPurpose) bool {
	t.Helper()
	view, err := a.Query(ctx, app.InspectQuery{Workflow: wf})
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

func planRevision(ctx context.Context, t *testing.T, a *app.Application, wf model.WorkflowID) int {
	t.Helper()
	view, err := a.Query(ctx, app.PlanQuery{Workflow: wf})
	if err != nil {
		return 0
	}
	return view.(app.PlanView).Revision
}

func planStatus(ctx context.Context, t *testing.T, a *app.Application, wf model.WorkflowID) model.PlanStatus {
	t.Helper()
	view, err := a.Query(ctx, app.PlanQuery{Workflow: wf})
	if err != nil {
		return ""
	}
	return view.(app.PlanView).PlanStatus
}

func executionPreview(ctx context.Context, t *testing.T, a *app.Application, wf model.WorkflowID) app.ExecutionPreviewView {
	t.Helper()
	view, err := a.Query(ctx, app.ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		return app.ExecutionPreviewView{}
	}
	return view.(app.ExecutionPreviewView)
}

func workspaceAdopted(ctx context.Context, t *testing.T, a *app.Application, wf model.WorkflowID) bool {
	t.Helper()
	view, err := a.Query(ctx, app.ProjectWorkspaceQuery{Selected: wf})
	if err != nil {
		return false
	}
	lc := view.(app.WorkspaceView).Lifecycle
	return lc != nil && lc.Adopted
}

func applyStatus(ctx context.Context, t *testing.T, a *app.Application, wf model.WorkflowID) model.ApplyStatus {
	t.Helper()
	view, err := a.Query(ctx, app.InspectQuery{Workflow: wf})
	if err != nil {
		return ""
	}
	attempts := view.(app.InspectView).ApplyAttempts
	if len(attempts) == 0 {
		return ""
	}
	return attempts[len(attempts)-1].Status
}

func cleanupStatus(ctx context.Context, t *testing.T, a *app.Application, wf model.WorkflowID) model.CleanupStatus {
	t.Helper()
	view, err := a.Query(ctx, app.InspectQuery{Workflow: wf})
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	view, err := a.Query(ctx, app.InspectQuery{Workflow: wf})
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
