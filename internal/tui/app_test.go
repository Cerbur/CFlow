package tui

// Root TUI Model tests (TUI tasks 9, 10, 14): the Model loads the real
// read-only Workspace projection through the shared Application, page
// navigation reaches every lifecycle page, user actions map to the exact
// typed Application Commands, Enter alone never approves, and the
// controlled-stop protocol executes the real Pause and Force Stop.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// recordingController wraps the shared Application and records every
// typed Command the TUI issues and every EscalateStop call.
type recordingController struct {
	ctrl      controller
	executed  []app.Command
	escalated int
}

func (r *recordingController) Execute(ctx context.Context, cmd app.Command) (app.Outcome, error) {
	r.executed = append(r.executed, cmd)
	return r.ctrl.Execute(ctx, cmd)
}

func (r *recordingController) Query(ctx context.Context, q app.Query) (app.View, error) {
	return r.ctrl.Query(ctx, q)
}

func (r *recordingController) DriveOnce(ctx context.Context, wf model.WorkflowID) (app.DriveOutcome, error) {
	return r.ctrl.DriveOnce(ctx, wf)
}

func (r *recordingController) EscalateStop() {
	r.escalated++
	r.ctrl.EscalateStop()
}

// hasExecuted reports whether the controller executed a command of the
// given type.
func (r *recordingController) hasExecuted(anyCommand any) bool {
	for _, c := range r.executed {
		switch anyCommand.(type) {
		case app.ResumeWorkflowCommand:
			if _, ok := c.(app.ResumeWorkflowCommand); ok {
				return true
			}
		case app.PauseWorkflowCommand:
			if _, ok := c.(app.PauseWorkflowCommand); ok {
				return true
			}
		case app.CancelWorkflowCommand:
			if _, ok := c.(app.CancelWorkflowCommand); ok {
				return true
			}
		case app.ApprovePlanCommand:
			if _, ok := c.(app.ApprovePlanCommand); ok {
				return true
			}
		case app.ApproveExecutionCommand:
			if _, ok := c.(app.ApproveExecutionCommand); ok {
				return true
			}
		case app.GeneratePlanCommand:
			if _, ok := c.(app.GeneratePlanCommand); ok {
				return true
			}
		case app.CheckPlanCommand:
			if _, ok := c.(app.CheckPlanCommand); ok {
				return true
			}
		case app.GenerateSpecsCommand:
			if _, ok := c.(app.GenerateSpecsCommand); ok {
				return true
			}
		case app.CompileWorkflowCommand:
			if _, ok := c.(app.CompileWorkflowCommand); ok {
				return true
			}
		case app.ExecutionDryRunCommand:
			if _, ok := c.(app.ExecutionDryRunCommand); ok {
				return true
			}
		case app.AdoptWorkspaceCommand:
			if _, ok := c.(app.AdoptWorkspaceCommand); ok {
				return true
			}
		case app.PrepareApplyCommand:
			if _, ok := c.(app.PrepareApplyCommand); ok {
				return true
			}
		case app.ExecuteApplyCommand:
			if _, ok := c.(app.ExecuteApplyCommand); ok {
				return true
			}
		case app.DryRunCommand:
			if _, ok := c.(app.DryRunCommand); ok {
				return true
			}
		case app.ExecuteCleanupCommand:
			if _, ok := c.(app.ExecuteCleanupCommand); ok {
				return true
			}
		case app.FinishDiscussionCommand:
			if _, ok := c.(app.FinishDiscussionCommand); ok {
				return true
			}
		case app.CreateWorkflowCommand:
			if _, ok := c.(app.CreateWorkflowCommand); ok {
				return true
			}
		}
	}
	return false
}

// testModel builds the root Model over the recording controller.
func testModel(rec *recordingController) Model {
	m := newModel(Dependencies{})
	m.ctrl = rec
	return m
}

// step processes one message, then runs every returned command to its
// message and feeds the results back through the Model until nothing is
// pending.
func step(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	upd, cmd := m.Update(msg)
	return runCmds(t, upd.(Model), cmd)
}

func runCmds(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	switch batch := msg.(type) {
	case tea.BatchMsg:
		for _, c := range batch {
			m = runCmds(t, m, c)
		}
		return m
	default:
		return step(t, m, msg)
	}
}

// load drives the Init command (the read-only workspace load) on a
// sized terminal.
func load(t *testing.T, m Model) Model {
	t.Helper()
	m = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	return runCmds(t, m, m.Init())
}

// press is one key press through the Model.
func press(t *testing.T, m Model, code rune, mod tea.KeyMod) Model {
	t.Helper()
	return step(t, m, tea.KeyPressMsg{Code: code, Mod: mod})
}

// TestModelLoadsRealWorkspaceView is the root-model failure test: the
// Model queries the shared Application and renders the real Workspace
// View; opening the TUI never resumes, dispatches, applies, or cleans
// up.
func TestModelLoadsRealWorkspaceView(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "calculator", Provider: "fake", ConfirmDirty: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))

	if m.workspace.Project.Name != "repo" {
		t.Fatalf("project = %+v", m.workspace.Project)
	}
	if len(m.workspace.Workflows) != 1 || m.workspace.Workflows[0].ID != wf.Workflow {
		t.Fatalf("workflows = %+v", m.workspace.Workflows)
	}
	got := render(m)
	for _, want := range []string{"project:", "workflows:", "workflow " + string(wf.Workflow), "health:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("workspace render misses %q:\n%s", want, got)
		}
	}
	// The read-only load never started the lifecycle: no Resume/Pause/
	// Dispatch/Apply/Cleanup command ran and the workflow state is
	// unchanged.
	if rec.hasExecuted(app.ResumeWorkflowCommand{}) || rec.hasExecuted(app.PauseWorkflowCommand{}) ||
		rec.hasExecuted(app.PrepareApplyCommand{}) || rec.hasExecuted(app.DryRunCommand{}) {
		t.Fatalf("the workspace load executed mutation commands: %v", rec.executed)
	}
	view, err := a.Query(ctx, app.StatusQuery{Workflow: wf.Workflow})
	if err != nil {
		t.Fatal(err)
	}
	if st := view.(app.StatusView); st.Stage != model.StageRequirementDiscussion || st.Runtime != model.RuntimePending {
		t.Fatalf("workflow after the TUI load = %s/%s, want REQUIREMENT_DISCUSSION/PENDING", st.Stage, st.Runtime)
	}
}

// TestModelNavigationReachesLifecyclePages: the lifecycle navigation
// (left/right) reaches every lifecycle page and the render stays pure.
func TestModelNavigationReachesLifecyclePages(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "calculator", Provider: "fake", ConfirmDirty: true}); err != nil {
		t.Fatal(err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))
	if m.page != PageWorkspace {
		t.Fatalf("initial page = %d", m.page)
	}
	// Tab cycles through the lifecycle pages; the render never panics
	// and every page is reachable.
	want := []Page{PageDiscussion, PagePlanApproval, PageExecutionApproval, PageExecution, PageBlocked, PageTerminal}
	visited := map[Page]bool{}
	for _, w := range want {
		m = press(t, m, tea.KeyTab, 0)
		if m.page != w {
			t.Fatalf("after tab: page = %d, want %d", m.page, w)
		}
		visited[m.page] = true
		if got := render(m); !strings.Contains(got, "\n") {
			t.Fatalf("page %d render = %q", m.page, got)
		}
	}
	for _, w := range want {
		if !visited[w] {
			t.Fatalf("lifecycle page %d never reached", w)
		}
	}
	// The workspace left/right arrows also move the lifecycle (the
	// page-local arrows keep their meaning on the other pages).
	m = press(t, m, tea.KeyEsc, 0)
	if m.page != PageWorkspace {
		t.Fatalf("esc did not return to the workspace: %d", m.page)
	}
	m = press(t, m, tea.KeyRight, 0)
	if m.page != PageDiscussion {
		t.Fatalf("after right: page = %d, want discussion", m.page)
	}
	m = press(t, m, tea.KeyRight, 0)
	if m.page != PagePlanApproval {
		t.Fatalf("after right: page = %d, want plan approval", m.page)
	}
	m = press(t, m, tea.KeyEsc, 0)
	m = press(t, m, tea.KeyRight, 0)
	if m.page != PageDiscussion {
		t.Fatalf("after right: page = %d, want discussion", m.page)
	}
	m = press(t, m, tea.KeyLeft, 0)
	if m.page != PageWorkspace {
		t.Fatalf("after left: page = %d, want workspace", m.page)
	}
	// Navigation never executed a mutation.
	for _, c := range rec.executed {
		switch c.(type) {
		case app.ResumeWorkflowCommand, app.PauseWorkflowCommand, app.CancelWorkflowCommand,
			app.ApprovePlanCommand, app.ApproveExecutionCommand:
			t.Fatalf("navigation executed %T", c)
		}
	}
}

// TestModelWorkspaceSelectionIsReadOnly: up/down only changes the
// selected workflow (pure UI state); no command is executed.
func TestModelWorkspaceSelectionIsReadOnly(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "one", Provider: "fake", ConfirmDirty: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "two", Provider: "fake", ConfirmDirty: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))
	if m.selected != first.Workflow {
		t.Fatalf("initial selection = %s, want %s", m.selected, first.Workflow)
	}
	m = press(t, m, tea.KeyDown, 0)
	if m.selected != second.Workflow {
		t.Fatalf("selection after down = %s, want %s", m.selected, second.Workflow)
	}
	if len(rec.executed) != 0 {
		t.Fatalf("selection executed commands: %v", rec.executed)
	}
}

// TestModelActionsMapToTypedCommands: the workspace legal actions map to
// the exact typed Application Commands.
func TestModelActionsMapToTypedCommands(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx, scripts: []string{planScript(fx.next("p"))}}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "calculator", Provider: "fake", ConfirmDirty: true}); err != nil {
		t.Fatal(err)
	}
	// The workflow must be running for the legal Pause action; the
	// planning session (the fake script) makes it RUNNING.
	if _, err := a.Execute(ctx, app.GeneratePlanCommand{Workflow: ref.list()[0], Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	// Pause it through the controlled stop so the legal Resume action
	// exists.
	if _, err := a.Execute(ctx, app.PauseWorkflowCommand{Workflow: ref.list()[0]}); err != nil {
		t.Fatal(err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))

	// 'r' → ResumeWorkflowCommand.
	m = press(t, m, 'r', 0)
	if !rec.hasExecuted(app.ResumeWorkflowCommand{}) {
		t.Fatalf("r did not execute ResumeWorkflowCommand: %v", rec.executed)
	}
	// Pause it again and 'x' → the cancel confirmation; Enter alone
	// never cancels; 'y' cancels.
	if _, err := a.Execute(ctx, app.PauseWorkflowCommand{Workflow: ref.list()[0]}); err != nil {
		t.Fatal(err)
	}
	before := len(rec.executed)
	m = press(t, m, 'x', 0)
	if m.page != PageCancel {
		t.Fatalf("x did not open the cancel page: %d", m.page)
	}
	m = press(t, m, tea.KeyEnter, 0)
	if m.page != PageWorkspace || rec.hasExecuted(app.CancelWorkflowCommand{}) {
		t.Fatalf("Enter alone cancelled the workflow")
	}
	m = press(t, m, 'x', 0)
	m = press(t, m, 'y', 0)
	if !rec.hasExecuted(app.CancelWorkflowCommand{}) || len(rec.executed) == before {
		t.Fatalf("y did not execute CancelWorkflowCommand: %v", rec.executed)
	}
}

// TestModelPlanApprovalMapsToTypedCommand: 'g' generates the plan, 'k'
// checks it, and the explicit confirmation issues ApprovePlanCommand
// with the exact revision and hash.
func TestModelPlanApprovalMapsToTypedCommand(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx, scripts: []string{
		planScript(fx.next("p")), checkPlanPassScript(fx.next("c")),
	}}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fx.createWithDiscussion(ctx, a)
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))
	// Navigate to the Plan Approval page.
	m = press(t, m, tea.KeyRight, 0) // discussion
	m = press(t, m, tea.KeyRight, 0) // plan approval
	if m.page != PagePlanApproval {
		t.Fatalf("page = %d, want plan approval", m.page)
	}
	// 'g' generates the plan.
	m = press(t, m, 'g', 0)
	if !rec.hasExecuted(app.GeneratePlanCommand{}) {
		t.Fatalf("g did not execute GeneratePlanCommand: %v", rec.executed)
	}
	if m.plan.Revision != 1 || m.plan.Hash == "" {
		t.Fatalf("plan after generate = %+v", m.plan)
	}
	// 'k' runs the independent check.
	m = press(t, m, 'k', 0)
	if !rec.hasExecuted(app.CheckPlanCommand{}) {
		t.Fatalf("k did not execute CheckPlanCommand: %v", rec.executed)
	}
	// Enter alone never approves.
	m = press(t, m, tea.KeyEnter, 0)
	if rec.hasExecuted(app.ApprovePlanCommand{}) {
		t.Fatal("Enter alone approved the plan")
	}
	// 'y' approves the exact plan.
	m = press(t, m, 'y', 0)
	if !rec.hasExecuted(app.ApprovePlanCommand{}) {
		t.Fatalf("y did not approve: %v", rec.executed)
	}
	for _, c := range rec.executed {
		if ap, ok := c.(app.ApprovePlanCommand); ok {
			if ap.Revision != 1 || ap.Hash == "" || ap.Hash != m.plan.Hash {
				t.Fatalf("approve = %+v, plan = %+v", ap, m.plan)
			}
		}
	}
}

// TestModelExecutionApprovalMapsToTypedCommand: 's' generates the specs,
// 'w' compiles the workflow, 'd' runs the dry run, and the explicit
// confirmation issues ApproveExecutionCommand binding the exact preview
// hashes (including the frozen Change Set).
func TestModelExecutionApprovalMapsToTypedCommand(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx, scripts: []string{
		planScript(fx.next("p")), checkPlanPassScript(fx.next("c")),
		specScript(fx.next("s")), workflowScript(fx.next("w")),
	}}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wf := fx.createWithDiscussion(ctx, a)
	if err := fx.approvePlan(ctx, a, wf); err != nil {
		t.Fatal(err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))
	// Navigate: right to the discussion page, then Tab twice to the
	// execution approval page.
	m = press(t, m, tea.KeyRight, 0)
	m = press(t, m, tea.KeyTab, 0)
	m = press(t, m, tea.KeyTab, 0)
	if m.page != PageExecutionApproval {
		t.Fatalf("page = %d, want execution approval", m.page)
	}
	m = press(t, m, 's', 0)
	if !rec.hasExecuted(app.GenerateSpecsCommand{}) {
		t.Fatalf("s did not generate specs: %v", rec.executed)
	}
	m = press(t, m, 'w', 0)
	if !rec.hasExecuted(app.CompileWorkflowCommand{}) {
		t.Fatalf("w did not compile: %v", rec.executed)
	}
	m = press(t, m, 'd', 0)
	if !rec.hasExecuted(app.ExecutionDryRunCommand{}) {
		t.Fatalf("d did not run the dry run: %v", rec.executed)
	}
	pv := m.preview
	if pv.PlanHash == "" || pv.ChangeSetHash == "" {
		t.Fatalf("preview = %+v, want the plan and change set hashes", pv)
	}
	// Enter alone never approves.
	m = press(t, m, tea.KeyEnter, 0)
	if rec.hasExecuted(app.ApproveExecutionCommand{}) {
		t.Fatal("Enter alone approved the execution")
	}
	m = press(t, m, 'y', 0)
	if !rec.hasExecuted(app.ApproveExecutionCommand{}) {
		t.Fatalf("y did not approve the execution: %v", rec.executed)
	}
	for _, c := range rec.executed {
		if ap, ok := c.(app.ApproveExecutionCommand); ok {
			if ap.PlanHash != pv.PlanHash || ap.CatalogHash != pv.CatalogHash ||
				ap.WorkflowHash != pv.WorkflowHash || ap.ChangeSetHash != pv.ChangeSetHash {
				t.Fatalf("approve = %+v, preview = %+v", ap, pv)
			}
		}
	}
}

// TestModelCtrlCExecutesControlledPause: the first Ctrl+C executes the
// real controlled Pause (the typed PauseWorkflowCommand changes the
// Runtime); the second Ctrl+C is the Force Stop (EscalateStop) and
// quits.
func TestModelCtrlCExecutesControlledPause(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx, scripts: []string{planScript(fx.next("p"))}}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "calculator", Provider: "fake", ConfirmDirty: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(ctx, app.GeneratePlanCommand{Workflow: ref.list()[0], Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))

	// The first Ctrl+C requests the controlled Pause: the typed command
	// closes dispatch and stops the managed processes.
	m = press(t, m, KeyCtrlCRune, tea.ModCtrl)
	if m.stop != stopFirstCtrlC {
		t.Fatalf("stop = %d, want first-ctrl-c", m.stop)
	}
	if !rec.hasExecuted(app.PauseWorkflowCommand{}) {
		t.Fatalf("the first Ctrl+C did not execute the controlled pause: %v", rec.executed)
	}
	view, err := a.Query(ctx, app.StatusQuery{Workflow: ref.list()[0]})
	if err != nil {
		t.Fatal(err)
	}
	if st := view.(app.StatusView); st.Runtime != model.RuntimePaused {
		t.Fatalf("runtime after the first Ctrl+C = %s, want PAUSED", st.Runtime)
	}

	// The second Ctrl+C is the Force Stop: it escalates the controlled
	// stop and quits.
	m2, cmd := m.Update(tea.KeyPressMsg{Code: KeyCtrlCRune, Mod: tea.ModCtrl})
	if rec.escalated != 1 {
		t.Fatalf("the second Ctrl+C did not call EscalateStop: %d", rec.escalated)
	}
	if _, ok := m2.(Model); !ok {
		t.Fatalf("the second Ctrl+C changed the model type")
	}
	if cmd == nil {
		t.Fatal("the second Ctrl+C did not quit")
	}
}

// TestModelQShowsPauseAndExit: q on an active Runner shows the Pause and
// Exit confirmation instead of quitting directly; y pauses through the
// typed command and quits after the pause completes.
func TestModelQShowsPauseAndExit(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx, scripts: []string{planScript(fx.next("p"))}}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "calculator", Provider: "fake", ConfirmDirty: true}); err != nil {
		t.Fatal(err)
	}
	// The workflow must be RUNNING for the controlled pause to settle.
	if _, err := a.Execute(ctx, app.GeneratePlanCommand{Workflow: ref.list()[0], Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))
	// An active Runner (the Execution page is live).
	m.running = true
	m.page = PageExecution

	// q shows the Pause and Exit confirmation; it never quits directly.
	m2, cmd := m.Update(tea.KeyPressMsg{Code: KeyQuit})
	if cmd != nil {
		t.Fatal("q quit directly while a runner is active")
	}
	m2m := m2.(Model)
	if m2m.page != PagePauseExit || m2m.stop != stopPauseAndExit {
		t.Fatalf("q state = page %d stop %d, want pause-and-exit", m2m.page, m2m.stop)
	}
	if got := render(m2m); !strings.Contains(got, "Pause and Exit") {
		t.Fatalf("pause-exit render = %q", got)
	}

	// n cancels the exit and returns to the page the user was on (the
	// runner stays active).
	m3, cmd := m2m.Update(tea.KeyPressMsg{Code: 'n'})
	if cmd != nil || m3.(Model).page != PageExecution || m3.(Model).stop != stopIdle || !m3.(Model).running {
		t.Fatalf("n did not cancel the pause-and-exit: page=%d stop=%d running=%v",
			m3.(Model).page, m3.(Model).stop, m3.(Model).running)
	}

	// y pauses through the typed command; the exit completes when the
	// pause finished.
	m4, cmd := m2m.Update(tea.KeyPressMsg{Code: 'y'})
	if cmd == nil {
		t.Fatal("y produced no pause command")
	}
	// The pause command runs (the typed command executes the controlled
	// pause).
	msg := cmd()
	if done, ok := msg.(commandDoneMsg); !ok || done.err != nil {
		t.Fatalf("the pause command failed: %v", msg)
	}
	if !rec.hasExecuted(app.PauseWorkflowCommand{}) {
		t.Fatalf("y did not execute the controlled pause: %v", rec.executed)
	}
	// The pause completion finishes the exit (no runner is left behind).
	m5, quitCmd := m4.(Model).Update(msg)
	if quitCmd == nil {
		t.Fatal("the pause-and-exit did not quit after the pause completed")
	}
	_ = m5
	// The same flow works through the first Ctrl+C path: q after the
	// first Ctrl+C also shows the confirmation.
	m7 := m
	m7.stop = stopFirstCtrlC
	m7.page = PageExecution
	_, cmd = m7.Update(tea.KeyPressMsg{Code: KeyQuit})
	if cmd != nil {
		t.Fatal("q after the first Ctrl+C quit directly")
	}
}

// createWithDiscussion drives the requirement discussion setup through
// the Application: create, prepare the native session, freeze the Change
// Set, and finish with the strict handoff. Returns the workflow id.
func (fx *tuiFixture) createWithDiscussion(ctx context.Context, a *app.Application) model.WorkflowID {
	fx.t.Helper()
	out, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "calculator", Provider: "fake", ConfirmDirty: true})
	if err != nil {
		fx.t.Fatalf("create: %v", err)
	}
	wf := out.Workflow
	prep, err := a.Execute(ctx, app.PrepareNativeDiscussionCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		fx.t.Fatalf("prepare native discussion: %v", err)
	}
	frozen, err := a.Execute(ctx, app.FreezeDiscussionCommand{Workflow: wf, Session: prep.SessionID})
	if err != nil {
		fx.t.Fatalf("freeze: %v", err)
	}
	ref := frozen.ChangeSet.Ref
	handoff, err := json.Marshal(map[string]any{
		"workflow_id":         string(wf),
		"session_id":          string(prep.SessionID),
		"targets":             "division by zero must error",
		"constraints":         "no external dependencies",
		"non_goals":           "no other arithmetic changes",
		"acceptance_criteria": "Divide returns a typed error on zero",
		"open_questions":      "error wording",
		"change_set":          map[string]any{"revision": ref.Revision, "sha256": ref.Hash},
		"user_decisions":      []map[string]any{{"topic": "error type", "decision": "typed error"}},
	})
	if err != nil {
		fx.t.Fatal(err)
	}
	if _, err := a.Execute(ctx, app.FinishDiscussionCommand{Workflow: wf, Session: prep.SessionID, Handoff: handoff}); err != nil {
		fx.t.Fatalf("finish: %v", err)
	}
	return wf
}

// approvePlan approves the active plan through the Application.
func (fx *tuiFixture) approvePlan(ctx context.Context, a *app.Application, wf model.WorkflowID) error {
	fx.t.Helper()
	if _, err := a.Execute(ctx, app.GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		return err
	}
	if _, err := a.Execute(ctx, app.CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		return err
	}
	view, err := a.Query(ctx, app.PlanQuery{Workflow: wf})
	if err != nil {
		return err
	}
	pv := view.(app.PlanView)
	_, err = a.Execute(ctx, app.ApprovePlanCommand{Workflow: wf, Revision: pv.Revision, Hash: pv.Hash})
	return err
}
