// Package tui is the full-screen Bubble Tea interface of CFlow (design
// §1: the TUI is the default entry point on an interactive terminal). The
// root Model renders the read-only project workspace and the lifecycle
// pages, drives the explicit user confirmations, runs the Foreground
// Runner and the Native Session Bridge, and implements the controlled
// stop protocol. It never decides lifecycle transitions itself: every
// state change goes through a typed Application Command, and every page
// renders the Application's own Views and legal actions.
package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/cli"
	"cflow.local/cflow/internal/foreground"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/native"
	"cflow.local/cflow/internal/security"
)

// Dependencies is what the TUI needs to run: the same command-tree
// Dependencies plus the optional input/output streams (tests inject a
// Fake terminal; production uses the process's stdin/stdout).
type Dependencies struct {
	CLI     cli.Dependencies
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	Program *tea.Program // nil: the Run call creates the default program
}

// controller is the typed Runtime seam the TUI drives. The shared
// *app.Application satisfies it; tests wrap it with a recording
// controller to assert the exact typed commands a key press issues.
type controller interface {
	Execute(context.Context, app.Command) (app.Outcome, error)
	Query(context.Context, app.Query) (app.View, error)
	DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error)
	// EscalateStop jumps a running controlled stop to the force-kill
	// phase (the second Ctrl+C, design §12.1).
	EscalateStop()
}

// Run launches the full-screen TUI and returns when the user quits.
// Run is the top-level entry the bare `cflow` command calls on an
// interactive terminal; it never mutates a Workflow by itself.
func Run(ctx context.Context, deps Dependencies) error {
	prog := deps.Program
	if prog == nil {
		in := deps.In
		out := deps.Out
		if in == nil {
			in = os.Stdin
		}
		if out == nil {
			out = os.Stdout
		}
		prog = tea.NewProgram(newModel(deps), tea.WithInput(in), tea.WithOutput(out))
	}
	model, err := prog.Run()
	if err != nil {
		return err
	}
	if m, ok := model.(Model); ok && m.err != nil {
		return m.err
	}
	return nil
}

// Page is one screen of the TUI.
type Page int

const (
	PageWorkspace Page = iota
	PageDiscussion
	PagePlanApproval
	PageExecutionApproval
	PageExecution
	PageBlocked
	PageTerminal
	PageCreate
	PageCancel
	PagePauseExit
	PageMigration
)

// navPages is the lifecycle navigation order (left/right on the
// workspace cycles through the lifecycle pages).
var navPages = []Page{
	PageWorkspace, PageDiscussion, PagePlanApproval, PageExecutionApproval,
	PageExecution, PageBlocked, PageTerminal,
}

// Model is the root TUI model: the read-only workspace projection, the
// lifecycle pages, the runner state, and the controlled-stop protocol.
type Model struct {
	width  int
	height int
	ready  bool
	err    error

	deps Dependencies
	ctrl controller

	// page is the current screen.
	page Page

	// selected is the workflow the workspace focuses.
	selected model.WorkflowID
	// workspace is the renderable workspace projection.
	workspace WorkspaceModel
	// provider is the discussion provider route of the selected
	// workflow ("" until a session exists; the create page falls back
	// to the first healthy provider).
	provider string

	discussion       DiscussionPage
	plan             app.PlanView
	preview          app.ExecutionPreviewView
	approval         ApprovalModel
	execution        ExecutionModel
	terminal         TerminalModel
	cancel           app.CancelSummaryView
	migration        app.MigrationPreviewView
	migrationConfirm migrationConfirmation

	// pendingDecision is the Runtime's reason the Foreground Runner
	// stopped at (design §11.2): the Execution page surfaces the
	// decision panel while it is set.
	pendingDecision string

	// createInput is the Create Workflow name field.
	createInput string
	// createConfirm is true after the user submitted the create name for
	// the explicit confirmation (Task 5: Enter alone never creates).
	createConfirm bool
	// createDirty is the queried target Git facts the Create page displays
	// (dirty state, fingerprint, and isolation); nil until the read-only
	// DiscoveryQuery projection loads.
	createDirty *app.DiscoveryView
	// status is the transient status line.
	status            string
	pendingPlanStatus string

	// Runner state: running is true while the Foreground Runner is
	// active; runCancel is the runner's context; eventCh is the
	// committed-event subscription the Execution page consumes.
	running   bool
	runCancel context.CancelFunc
	eventCh   <-chan model.Event
	sink      *foreground.EventSink

	// stop tracks the controlled-stop state (design §12.1): the first
	// Ctrl+C requests the controlled Pause; the second is the Force
	// Stop of an active Runner; q on an active Runner shows the Pause
	// and Exit confirmation instead of quitting directly.
	stop stopState
	// prevPage is the page the Pause and Exit prompt returns to.
	prevPage Page
	// resumeThenRun is set when the Execution page requested the resume
	// as the start of the Foreground Runner: the runner starts once the
	// resume committed.
	resumeThenRun bool
}

type migrationConfirmation uint8

const (
	migrationConfirmNone migrationConfirmation = iota
	migrationConfirmPrepare
	migrationConfirmExecute
)

// stopState is the two-phase stop state of an active Runner.
type stopState int

const (
	stopIdle stopState = iota
	stopFirstCtrlC
	stopPauseAndExit
)

// ---------------------------------------------------------------------------
// messages
// ---------------------------------------------------------------------------

// projectionMsg delivers one page projection loaded through the
// controller (a read-only Query; the page renders it).
type projectionMsg struct {
	page Page
	view app.View
	err  error
}

// appLoadedMsg delivers the shared Application opened by the Init
// command (the TUI never constructs the Application per key press).
type appLoadedMsg struct {
	ctrl controller
	err  error
}

// commandDoneMsg delivers the result of one typed Application Command.
type commandDoneMsg struct {
	cmd app.Command
	out app.Outcome
	err error
}

// runnerEventMsg delivers one committed event of the Foreground Runner.
type runnerEventMsg struct{ ev model.Event }

// eventsClosedMsg ends the event pump (the runner finished).
type eventsClosedMsg struct{}

// runnerDoneMsg delivers the terminal result of the Foreground Runner.
type runnerDoneMsg struct {
	res foreground.Result
	err error
}

// nativeDoneMsg delivers the result of one Native Session Bridge turn.
type nativeDoneMsg struct {
	result native.Result
	err    error
}

// reportLoadedMsg delivers the rendered Final Report of the Terminal
// page.
type reportLoadedMsg struct {
	markdown string
	err      error
}

// NewModel returns the initial root model.
func NewModel() Model { return newModel(Dependencies{}) }

// newModel returns the initial root model with the given dependencies.
func newModel(deps Dependencies) Model {
	return Model{
		deps:       deps,
		page:       PageWorkspace,
		discussion: DiscussionPage{},
		approval:   ApprovalModel{},
		execution:  ExecutionModel{},
		terminal:   NewTerminalModel(),
		sink:       foreground.NewEventSink(),
	}
}

// Init is the initial command: open the shared Application (through the
// injected seam or the headless CLI's default construction) and load the
// read-only project workspace. Opening the TUI never resumes, dispatches,
// applies, or cleans up anything.
func (m Model) Init() tea.Cmd {
	if m.ctrl != nil {
		// The tests inject the controller directly.
		return func() tea.Msg { return appLoadedMsg{ctrl: m.ctrl} }
	}
	return func() tea.Msg {
		var (
			a   *app.Application
			err error
		)
		if m.deps.CLI.OpenApplication != nil {
			a, err = m.deps.CLI.OpenApplication(context.Background())
		} else {
			a, err = cli.OpenApplication(context.Background(), m.deps.CLI)
		}
		if err != nil {
			return appLoadedMsg{err: err}
		}
		return appLoadedMsg{ctrl: a}
	}
}

// queryProjectionMsg runs one read-only Query of the given page and
// returns the projection message.
func (m Model) queryProjectionMsg(page Page, q app.Query) tea.Msg {
	view, err := m.ctrl.Query(context.Background(), q)
	return projectionMsg{page: page, view: view, err: err}
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		return m, nil
	case appLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.ctrl = msg.ctrl
		return m, func() tea.Msg {
			return m.queryProjectionMsg(PageWorkspace, app.ProjectWorkspaceQuery{})
		}
	case projectionMsg:
		return m.applyProjection(msg)
	case commandDoneMsg:
		return m.applyCommand(msg)
	case runnerEventMsg:
		m.execution = m.execution.OnEvent(msg.ev)
		if m.running {
			return m, m.pumpEvents()
		}
		// The runner is no longer active (a terminal path already cleared
		// running/eventCh): apply the in-flight event but never re-pump —
		// re-pumping would capture the cleared nil eventCh and leak a
		// goroutine blocked on a nil channel forever.
		return m, nil
	case eventsClosedMsg:
		return m, nil
	case runnerDoneMsg:
		return m.applyRunnerDone(msg)
	case nativeDoneMsg:
		if msg.err != nil {
			m.status = "native session: " + msg.err.Error()
			return m, m.reloadCmd()
		}
		// The Bridge return persists the process exit facts and moves the
		// Session to INTERACTIVE_IDLE; the Return actions are legal only
		// per the revalidated facts (design §9.2, TUI task 12).
		m.status = fmt.Sprintf("native session %s returned (exit %d)", msg.result.Session, msg.result.Exit.Code)
		return m, m.executeCmd(app.NativeDiscussionReturnCommand{
			Workflow:        m.selected,
			Session:         msg.result.Session,
			Exit:            msg.result.Exit,
			Provider:        msg.result.Provider,
			ProviderSession: agent.ProviderSessionID(msg.result.ProviderSession),
		})
	case reportLoadedMsg:
		if msg.err != nil {
			m.status = "report: " + msg.err.Error()
		} else {
			m.terminal.Report = msg.markdown
			m.status = "final report rendered"
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case error:
		m.err = msg
		// A renderer failure never leaves a background runner: clear the
		// ownership state (design §16: the Runtime is unchanged; the user
		// can recover through the Headless CLI).
		m.clearRunner()
		return m, nil
	}
	return m, nil
}

// applyProjection stores one loaded page projection. A projection
// error on a lifecycle page degrades to the page's empty state (e.g.,
// the Execution Approval preview does not exist before the specs and
// the dry run); only the workspace load failure is fatal.
func (m Model) applyProjection(msg projectionMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		if msg.page == PageWorkspace {
			m.err = msg.err
			return m, nil
		}
		m.status = msg.err.Error()
		return m, nil
	}
	switch msg.page {
	case PageWorkspace, PageBlocked, PageExecution, PageTerminal:
		if v, ok := msg.view.(app.WorkspaceView); ok {
			m.workspace = MapWorkspace(v)
			m.selected = v.Selected
			if m.provider == "" {
				m.provider = preferredProvider(v.Health.Providers)
			}
			if msg.page == PageExecution && m.workspace.Lifecycle != nil {
				m.execution = m.execution.WithWorkflow(m.workspace.Lifecycle.ID)
			}
		}
	case PageDiscussion:
		if v, ok := msg.view.(app.DiscussionReturnView); ok {
			// The handoff editor survives the projection reloads that
			// follow the freeze (the editor state is UI state; the
			// projection refresh only re-renders the facts).
			editing, handoff, status, selected := m.discussion.Editing, m.discussion.Handoff, m.discussion.Status, m.discussion.Selected
			m.discussion = MapDiscussionReturn(v)
			m.discussion.Editing = editing
			m.discussion.Handoff = handoff
			m.discussion.Status = status
			m.discussion.Selected = selected
			if v.Provider != "" {
				m.provider = v.Provider
			}
		}
	case PagePlanApproval:
		if v, ok := msg.view.(app.PlanView); ok {
			m.plan = v
			m.approval = ApprovalModel{Plan: v}
			if m.page == PagePlanApproval && planProjectionReached(v, m.pendingPlanStatus) {
				m.status = m.pendingPlanStatus
				m.pendingPlanStatus = ""
			}
		}
	case PageExecutionApproval:
		if v, ok := msg.view.(app.ExecutionPreviewView); ok {
			m.preview = v
			m.approval = ApprovalModel{Preview: v}
		}
	case PageCreate:
		if v, ok := msg.view.(app.DiscoveryView); ok {
			// The read-only Discovery projection: the Create page surfaces
			// the target's dirty state, fingerprint, and isolation before
			// the explicit confirmation (Task 5).
			m.createDirty = &v
		}
	case PageCancel:
		if v, ok := msg.view.(app.CancelSummaryView); ok {
			m.cancel = v
		}
	case PageMigration:
		if v, ok := msg.view.(app.MigrationPreviewView); ok {
			m.migration = v
		}
	}
	return m, nil
}

// reloadCmd reloads the workspace projection and the current page's
// projection after one command changed the Runtime facts.
func (m Model) reloadCmd() tea.Cmd {
	var cmds []tea.Cmd
	cmds = append(cmds, func() tea.Msg {
		return m.queryProjectionMsg(PageWorkspace, app.ProjectWorkspaceQuery{Selected: m.selected})
	})
	switch m.page {
	case PageDiscussion:
		cmds = append(cmds, func() tea.Msg {
			return m.queryProjectionMsg(PageDiscussion, app.DiscussionReturnQuery{Workflow: m.selected})
		})
	case PagePlanApproval:
		cmds = append(cmds, func() tea.Msg {
			return m.queryProjectionMsg(PagePlanApproval, app.PlanQuery{Workflow: m.selected})
		})
	case PageExecutionApproval:
		cmds = append(cmds, func() tea.Msg {
			return m.queryProjectionMsg(PageExecutionApproval, app.ExecutionPreviewQuery{Workflow: m.selected})
		})
	case PageCancel:
		cmds = append(cmds, func() tea.Msg {
			return m.queryProjectionMsg(PageCancel, app.CancelSummaryQuery{Workflow: m.selected})
		})
	case PageMigration:
		cmds = append(cmds, func() tea.Msg {
			return m.queryProjectionMsg(PageMigration, app.LayoutMigrationPreviewQuery{Workflow: m.selected})
		})
	}
	return tea.Batch(cmds...)
}

// applyCommand handles one finished typed Application Command.
func (m Model) applyCommand(msg commandDoneMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		m.status = fmt.Sprintf("%v", msg.err)
		if m.stop == stopPauseAndExit {
			// The pause failed; the user still asked to exit. Never
			// leave a background runner: force-stop and quit.
			m.forceStop()
			return m.quit()
		}
		if _, ok := msg.cmd.(app.ResumeWorkflowCommand); ok && m.resumeThenRun {
			// The Execution page requested the resume as the run start but
			// the Runtime rejected it (the stale projection can still show
			// Resume right after an execution approval while the workflow is
			// already RUNNING). Clear the pending resume and fall back to
			// starting the Foreground Runner directly: DriveOnce is a safe
			// bounded step that stops when it cannot progress. Without this
			// the pending resume-then-run would stay dangling and a later
			// successful resume would double-start the runner.
			m.resumeThenRun = false
			return m.startRunner()
		}
		return m, nil
	}
	switch msg.cmd.(type) {
	case app.CreateWorkflowCommand:
		m.selected = msg.out.Workflow
		m.status = "workflow created"
		m.page = PageWorkspace
		m.createDirty = nil
		m.createConfirm = false
		return m, m.reloadCmd()
	case app.PauseWorkflowCommand:
		if m.stop == stopPauseAndExit {
			// The Pause and Exit confirmation: the pause completed, so
			// no background process is left; finish the exit.
			return m.quit()
		}
		m.status = "workflow paused (controlled stop)"
		m.pendingDecision = ""
		return m, m.reloadCmd()
	case app.ResumeWorkflowCommand:
		m.status = "workflow resumed"
		m.pendingDecision = ""
		if m.resumeThenRun {
			// The Execution page requested the resume as the run start.
			m.resumeThenRun = false
			m, run := m.startRunner()
			return m, tea.Batch(m.reloadCmd(), run)
		}
		return m, m.reloadCmd()
	case app.CancelWorkflowCommand:
		m.status = "workflow cancelled"
		m.page = PageWorkspace
		return m, m.reloadCmd()
	case app.AdoptWorkspaceCommand:
		m.status = "workspace adopted"
		m.pendingDecision = ""
		return m, m.reloadCmd()
	case app.PrepareNativeDiscussionCommand:
		if msg.out.Native != nil {
			m.status = "starting the native discussion terminal…"
			return m, newNativeExecCmd(msg.out.Native)
		}
		m.status = "native discussion prepared"
		return m, m.reloadCmd()
	case app.ContinueNativeDiscussionCommand:
		if msg.out.Native != nil {
			m.status = "continuing the native discussion terminal…"
			return m, newNativeExecCmd(msg.out.Native)
		}
		// The outcome carried no Bridge request (a re-armed Session without
		// recoverable binding facts): fall back to the plain projection reload
		// instead of launching a terminal that would run no supervised process.
		return m, m.reloadCmd()
	case app.SwitchAgentCommand:
		if msg.out.Native != nil {
			m.status = "starting the switched native discussion terminal…"
			return m, newNativeExecCmd(msg.out.Native)
		}
		return m, m.reloadCmd()
	case app.FreezeDiscussionCommand:
		if msg.out.ChangeSet != nil {
			// The Runtime facts are fixed: the handoff editor opens
			// with the frozen Change Set Revision/Hash, and the user
			// supplies only the content fields.
			m.discussion.Editing = true
			m.discussion.Handoff = ""
			m.discussion.Status = ""
			m.discussion.ChangeSetRef = &msg.out.ChangeSet.Ref
			m.status = "change set rev " + itoa(msg.out.ChangeSet.Ref.Revision) + " frozen; type the handoff content"
		}
		return m, m.reloadCmd()
	case app.FinishDiscussionCommand:
		m.discussion.Editing = false
		m.discussion.Handoff = ""
		m.status = "discussion finished"
		return m, m.reloadCmd()
	case app.GeneratePlanCommand:
		m.pendingPlanStatus = "plan generated"
		m.status = "plan generation finished; refreshing plan projection…"
		return m, m.reloadCmd()
	case app.CheckPlanCommand:
		m.pendingPlanStatus = "plan checked"
		m.status = "plan check finished; refreshing plan projection…"
		return m, m.reloadCmd()
	case app.ApprovePlanCommand:
		m.pendingPlanStatus = "plan approved"
		m.status = "plan approval finished; refreshing plan projection…"
		return m, m.reloadCmd()
	case app.GenerateSpecsCommand:
		m.status = "specs generated"
		return m, m.reloadCmd()
	case app.CompileWorkflowCommand:
		m.status = "workflow compiled"
		return m, m.reloadCmd()
	case app.ExecutionDryRunCommand:
		m.status = "execution dry run complete"
		return m, m.reloadCmd()
	case app.ApproveExecutionCommand:
		m.status = "execution approved"
		m.page = PageExecution
		return m, m.reloadCmd()
	case app.PrepareApplyCommand:
		if msg.out.Apply != nil {
			m.terminal.ApplyPreview = renderApplyAttempt(*msg.out.Apply)
			m.status = "apply staged (preview ready)"
		}
		return m, m.reloadCmd()
	case app.ExecuteApplyCommand:
		m.status = "apply delivered to the target branch"
		m.terminal.Confirmed = false
		m.terminal.Yes = false
		return m, m.reloadCmd()
	case app.DryRunCommand:
		if msg.out.Cleanup != nil {
			m.terminal.CleanupPreview = renderCleanupAttempt(*msg.out.Cleanup)
			ref := msg.out.Cleanup.Manifest
			m.terminal.cleanupRef = &ref
			m.status = "cleanup dry run manifest ready"
		}
		return m, m.reloadCmd()
	case app.ExecuteCleanupCommand:
		m.status = "cleanup executed"
		m.terminal.Confirmed = false
		m.terminal.Yes = false
		return m, m.reloadCmd()
	case app.PrepareLayoutMigrationCommand:
		m.migrationConfirm = migrationConfirmNone
		m.status = "layout migration prepared; immutable intent persisted"
		return m, m.reloadCmd()
	case app.ExecuteLayoutMigrationCommand:
		m.migrationConfirm = migrationConfirmNone
		m.status = "legacy layout migrated"
		m.page = PageWorkspace
		return m, m.reloadCmd()
	}
	return m, m.reloadCmd()
}

// applyRunnerDone handles the terminal result of the Foreground Runner.
// Every terminal path clears the ownership state exactly once: the run
// closure's defer unsubscribes the event subscription, and this clears
// running, runCancel, and eventCh (Task 6).
func (m Model) applyRunnerDone(msg runnerDoneMsg) (Model, tea.Cmd) {
	m.running = false
	m.runCancel = nil
	m.eventCh = nil
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	switch msg.res.Reason {
	case foreground.StopTerminal:
		m.status = "runner: " + string(msg.res.Reason)
		m.pendingDecision = ""
	case foreground.StopNeedsUser:
		m.pendingDecision = msg.res.Last.Reason
		m.status = "runner: user decision required"
	case foreground.StopNoSafeProgress, foreground.StopCancelled, foreground.StopFailed:
		m.status = "runner: " + string(msg.res.Reason)
		m.pendingDecision = ""
	}
	if m.stop == stopPauseAndExit {
		// The runner is gone; finish the requested exit.
		return m, tea.Quit
	}
	return m, m.reloadCmd()
}

// handleKey routes one key press through the controlled-stop protocol and
// the active page.
func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	// The controlled-stop protocol owns q and Ctrl+C globally (design
	// §12.1): a page can never quit directly while a Runner is active.
	if IsCtrlC(msg) {
		switch m.stop {
		case stopIdle:
			m.stop = stopFirstCtrlC
			var cmds []tea.Cmd
			if m.selected != "" {
				// The first Ctrl+C requests the controlled Pause: the
				// typed command closes dispatch and stops the managed
				// processes, and the run context is cancelled so the real
				// Runner stops (never just a local flag).
				if m.runCancel != nil {
					m.runCancel()
				}
				cmds = append(cmds, m.executeCmd(app.PauseWorkflowCommand{Workflow: m.selected}))
			}
			return m, tea.Batch(cmds...)
		default:
			// The second Ctrl+C is the Force Stop of the active Runner
			// (and of any running controlled stop).
			m.forceStop()
			return m.quit()
		}
	}
	// Tab cycles the lifecycle pages from any page (left/right keep
	// their page-local meaning on the Approval/Execution/Terminal
	// pages).
	if msg.Code == tea.KeyTab {
		return m.moveNav(1)
	}
	// q is the quit key ONLY outside the text inputs (the create form
	// and the handoff editor): inside them 'q' is a typed character.
	if IsQuit(msg) && !m.typingText() {
		switch m.stop {
		case stopIdle:
			if m.running {
				// An active Runner: q shows Pause and Exit instead of
				// quitting directly (processes never orphan).
				m.prevPage = m.page
				m.stop = stopPauseAndExit
				m.page = PagePauseExit
				return m, nil
			}
			return m, tea.Quit
		default:
			// Already in the stop protocol: q cancels it and returns to
			// the page the user was on.
			m.stop = stopIdle
			m.page = m.prevPage
			return m, nil
		}
	}
	switch m.page {
	case PagePauseExit:
		return m.handlePauseExitKey(msg)
	case PageCreate:
		return m.handleCreateKey(msg)
	case PageCancel:
		return m.handleCancelKey(msg)
	case PageMigration:
		return m.handleMigrationKey(msg)
	case PageWorkspace:
		return m.handleWorkspaceKey(msg)
	case PageDiscussion:
		return m.handleDiscussionKey(msg)
	case PagePlanApproval:
		return m.handlePlanApprovalKey(msg)
	case PageExecutionApproval:
		return m.handleExecutionApprovalKey(msg)
	case PageExecution:
		return m.handleExecutionKey(msg)
	case PageBlocked:
		return m.handleBlockedKey(msg)
	case PageTerminal:
		return m.handleTerminalKey(msg)
	}
	return m, nil
}

// typingText reports whether the active page is a text input ('q' is a
// typed character there, never the quit key).
func (m Model) typingText() bool {
	switch m.page {
	case PageCreate:
		return true
	case PageDiscussion:
		return m.discussion.Editing
	}
	return false
}

// forceStop cancels the Runner and escalates the running controlled stop
// to the force-kill phase (the second Ctrl+C, design §12.1).
func (m *Model) forceStop() {
	if m.runCancel != nil {
		m.runCancel()
	}
	if m.ctrl != nil {
		m.ctrl.EscalateStop()
	}
}

// clearRunner resets the Foreground Runner ownership state (design §11,
// Task 6): it cancels any live run context and clears running, runCancel,
// and eventCh. It never unsubscribes the event sink — the run closure's
// defer owns the subscription and drops it exactly once — so terminal
// paths never double-clean. Idempotent and safe on every terminal path.
func (m *Model) clearRunner() {
	if m.runCancel != nil {
		m.runCancel()
	}
	m.running = false
	m.runCancel = nil
	m.eventCh = nil
}

// quit cleans the Foreground Runner ownership state and returns the quit
// command (design §12.1): a quit never leaves an active Runner running or
// a dangling event subscription.
func (m Model) quit() (Model, tea.Cmd) {
	m.clearRunner()
	return m, tea.Quit
}

// executeCmd runs one typed Application Command and delivers its Outcome.
func (m Model) executeCmd(cmd app.Command) tea.Cmd {
	return func() tea.Msg {
		out, err := m.ctrl.Execute(context.Background(), cmd)
		return commandDoneMsg{cmd: cmd, out: out, err: err}
	}
}

// ---------------------------------------------------------------------------
// page key handlers
// ---------------------------------------------------------------------------

// handlePauseExitKey handles the Pause and Exit confirmation.
func (m Model) handlePauseExitKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case msg.Code == 'y' || msg.Code == 'Y':
		m.stop = stopPauseAndExit
		if m.runCancel != nil {
			m.runCancel()
		}
		if m.selected != "" {
			return m, m.executeCmd(app.PauseWorkflowCommand{Workflow: m.selected})
		}
		m.forceStop()
		return m.quit()
	case IsEnter(msg) || msg.Code == 'n' || msg.Code == 'N' || msg.Code == tea.KeyEsc:
		m.stop = stopIdle
		m.page = m.prevPage
		return m, nil
	}
	return m, nil
}

// handleCreateKey handles the Create Workflow form (Task 5): the TUI
// first queries and displays the target's dirty state, fingerprint, and
// isolation; Enter only submits the name for the explicit confirmation
// (default No) and NEVER creates; only an explicit y issues the typed
// CreateWorkflowCommand, carrying ConfirmDirty: true exactly when the
// queried target is dirty.
func (m Model) handleCreateKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.createConfirm {
		// The explicit confirmation: default No. Enter alone never creates.
		switch {
		case msg.Code == 'y' || msg.Code == 'Y':
			name := strings.TrimSpace(m.createInput)
			if name == "" {
				m.createConfirm = false
				m.status = "a workflow name is required"
				return m, nil
			}
			if m.createDirty == nil {
				m.status = "target facts unavailable; press esc and retry"
				return m, nil
			}
			m.status = "creating workflow…"
			m.createInput = ""
			m.createConfirm = false
			return m, m.executeCmd(app.CreateWorkflowCommand{
				Name: name, Provider: m.createProvider(), ConfirmDirty: m.createDirty.Dirty,
			})
		case IsEnter(msg) || msg.Code == 'n' || msg.Code == 'N' || msg.Code == tea.KeyEsc:
			m.createConfirm = false
			m.status = "creation declined"
			return m, nil
		}
		return m, nil
	}
	switch {
	case msg.Code == tea.KeyEsc:
		m.page = PageWorkspace
		m.createInput = ""
		m.createConfirm = false
		m.createDirty = nil
		return m, nil
	case msg.Code == tea.KeyEnter:
		name := strings.TrimSpace(m.createInput)
		if name == "" {
			m.status = "a workflow name is required"
			return m, nil
		}
		// Enter submits the name for the confirmation; it never creates.
		m.createConfirm = true
		return m, nil
	case msg.Code == tea.KeyBackspace || msg.Code == tea.KeyDelete:
		if len(m.createInput) > 0 {
			m.createInput = m.createInput[:len(m.createInput)-1]
		}
		return m, nil
	case msg.Code == tea.KeyTab:
		return m, nil
	case msg.Text != "":
		m.createInput += msg.Text
		return m, nil
	}
	return m, nil
}

// createProvider is the deterministic create route: the first compatible
// provider the workspace health reports, else the Fake default (the
// headless CLI's default route).
func (m Model) createProvider() string {
	if m.provider != "" {
		return m.provider
	}
	return preferredProvider(m.workspace.Health.Providers)
}

// preferredProvider selects the default TUI route. Real providers take
// precedence over the deterministic Fake adapter when one is configured and
// healthy; Fake remains the fallback for local development and tests.
func preferredProvider(providers []app.ProviderHealth) string {
	for _, preferred := range []string{"claude", "codex"} {
		for _, p := range providers {
			if p.Name == preferred && p.Compatible {
				return p.Name
			}
		}
	}
	for _, p := range providers {
		if p.Compatible {
			return p.Name
		}
	}
	return "fake"
}

// handleWorkspaceKey handles the workspace screen keys: workflow
// selection, lifecycle navigation, and the legal actions.
func (m Model) handleWorkspaceKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case IsUp(msg):
		return m.moveSelection(-1)
	case IsDown(msg):
		return m.moveSelection(1)
	case IsLeft(msg):
		return m.moveNav(-1)
	case IsRight(msg):
		return m.moveNav(1)
	case msg.Code == 'n' || msg.Code == 'N':
		// Opening the Create page is a read-only step: it queries the
		// target's Git facts (dirty state, fingerprint, isolation) that the
		// explicit confirmation then renders.
		m.page = PageCreate
		m.createInput = ""
		m.createConfirm = false
		m.createDirty = nil
		return m, m.queryCmd(PageCreate, app.DiscoveryQuery{})
	case msg.Code == 'r' || msg.Code == 'R':
		if m.selected == "" || !hasAction(m.workspace.Actions, ActionResume) {
			m.status = "resume is not a legal action"
			return m, nil
		}
		m.status = "resuming…"
		return m, m.executeCmd(app.ResumeWorkflowCommand{Workflow: m.selected})
	case msg.Code == 'p' || msg.Code == 'P':
		if m.selected == "" || !hasAction(m.workspace.Actions, ActionPause) {
			m.status = "pause is not a legal action"
			return m, nil
		}
		m.status = "pausing…"
		return m, m.executeCmd(app.PauseWorkflowCommand{Workflow: m.selected})
	case msg.Code == 'x' || msg.Code == 'X':
		if m.selected == "" || !hasAction(m.workspace.Actions, ActionCancel) {
			m.status = "cancel is not a legal action"
			return m, nil
		}
		m.page = PageCancel
		return m, func() tea.Msg {
			return m.queryProjectionMsg(PageCancel, app.CancelSummaryQuery{Workflow: m.selected})
		}
	case msg.Code == 'm' || msg.Code == 'M':
		if m.selected == "" || !hasAction(m.workspace.Actions, ActionMigrate) {
			m.status = "layout migration is not a legal action"
			return m, nil
		}
		m.page = PageMigration
		m.migration = app.MigrationPreviewView{}
		m.migrationConfirm = migrationConfirmNone
		return m, m.queryCmd(PageMigration, app.LayoutMigrationPreviewQuery{Workflow: m.selected})
	}
	return m, nil
}

func hasAction(actions []Action, want Action) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

func planProjectionReached(v app.PlanView, pending string) bool {
	switch pending {
	case "plan generated":
		return v.Stage == model.StagePlanCheck && v.Revision >= 1 && v.Hash != ""
	case "plan checked":
		return v.PlanStatus == model.PlanChecked
	case "plan approved":
		return v.PlanStatus == model.PlanApproved
	default:
		return false
	}
}

// handleMigrationKey owns the explicit TUI Preview -> Prepare -> Execute
// protocol. Enter and n are No at both confirmations; only y executes a
// typed Application command bound to the displayed manifest hash.
func (m Model) handleMigrationKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.migrationConfirm != migrationConfirmNone {
		switch {
		case msg.Code == 'y' || msg.Code == 'Y':
			confirm := m.migrationConfirm
			m.migrationConfirm = migrationConfirmNone
			if confirm == migrationConfirmPrepare {
				return m, m.executeCmd(app.PrepareLayoutMigrationCommand{Workflow: m.selected, ManifestHash: m.migration.ManifestHash})
			}
			return m, m.executeCmd(app.ExecuteLayoutMigrationCommand{Workflow: m.selected, ManifestHash: m.migration.ManifestHash})
		case IsEnter(msg) || msg.Code == 'n' || msg.Code == 'N' || msg.Code == tea.KeyEsc:
			m.migrationConfirm = migrationConfirmNone
			m.status = "layout migration confirmation declined"
			return m, nil
		}
		return m, nil
	}
	switch {
	case msg.Code == 'p' || msg.Code == 'P':
		m.migrationConfirm = migrationConfirmPrepare
		return m, nil
	case msg.Code == 'e' || msg.Code == 'E':
		m.migrationConfirm = migrationConfirmExecute
		return m, nil
	case msg.Code == tea.KeyEsc:
		m.page = PageWorkspace
		return m, nil
	}
	return m, nil
}

// moveSelection moves the workflow selection by one row and reloads the
// workspace projection (navigation is a read-only UI state change).
func (m Model) moveSelection(delta int) (Model, tea.Cmd) {
	if len(m.workspace.Workflows) == 0 {
		return m, nil
	}
	idx := 0
	for i, w := range m.workspace.Workflows {
		if w.ID == m.selected {
			idx = i
		}
	}
	idx += delta
	if idx < 0 {
		idx = len(m.workspace.Workflows) - 1
	}
	if idx >= len(m.workspace.Workflows) {
		idx = 0
	}
	m.selected = m.workspace.Workflows[idx].ID
	m.provider = ""
	m.discussion = DiscussionPage{}
	m.plan = app.PlanView{}
	m.preview = app.ExecutionPreviewView{}
	m.pendingDecision = ""
	return m, func() tea.Msg {
		return m.queryProjectionMsg(PageWorkspace, app.ProjectWorkspaceQuery{Selected: m.selected})
	}
}

// moveNav moves the lifecycle navigation by one page and loads the
// target page's read-only projection (navigation is pure UI state
// change; the projection is a Query, never a mutation).
func (m Model) moveNav(delta int) (Model, tea.Cmd) {
	idx := 0
	for i, p := range navPages {
		if p == m.page {
			idx = i
		}
	}
	idx += delta
	idx = (idx + len(navPages)) % len(navPages)
	m.page = navPages[idx]
	switch m.page {
	case PageDiscussion:
		m.discussion = DiscussionPage{}
		return m, m.queryCmd(PageDiscussion, app.DiscussionReturnQuery{Workflow: m.selected})
	case PagePlanApproval:
		m.approval = ApprovalModel{Plan: m.plan}
		return m, m.queryCmd(PagePlanApproval, app.PlanQuery{Workflow: m.selected})
	case PageExecutionApproval:
		m.approval = ApprovalModel{Preview: m.preview}
		return m, m.queryCmd(PageExecutionApproval, app.ExecutionPreviewQuery{Workflow: m.selected})
	}
	return m, nil
}

// queryCmd loads one page projection through a read-only Query.
func (m Model) queryCmd(page Page, q app.Query) tea.Cmd {
	return func() tea.Msg {
		return m.queryProjectionMsg(page, q)
	}
}

// handleDiscussionKey handles the native Discussion Return Page.
func (m Model) handleDiscussionKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.discussion.Editing {
		return m.handleHandoffKey(msg)
	}
	switch {
	case msg.Code == tea.KeyEsc || IsQuit(msg):
		m.page = PageWorkspace
		return m, nil
	case IsLeft(msg):
		return m.moveNav(-1)
	case IsRight(msg):
		return m.moveNav(1)
	case IsUp(msg):
		if m.discussion.Selected > 0 {
			m.discussion.Selected--
		}
		return m, nil
	case IsDown(msg):
		if m.discussion.Selected < len(m.discussion.Actions)-1 {
			m.discussion.Selected++
		}
		return m, nil
	case IsEnter(msg):
		return m.activateDiscussionAction()
	}
	return m, nil
}

// activateDiscussionAction runs the selected Return Page action.
func (m Model) activateDiscussionAction() (Model, tea.Cmd) {
	if !m.discussion.Loaded {
		m.status = "the discussion page is still loading"
		return m, nil
	}
	if m.selected == "" || len(m.discussion.Actions) == 0 {
		m.status = "no discussion session"
		return m, nil
	}
	action := m.discussion.Actions[m.discussion.Selected]
	switch action {
	case ReturnStart:
		m.status = "preparing the native discussion…"
		return m, m.executeCmd(app.PrepareNativeDiscussionCommand{Workflow: m.selected, Provider: m.discussionProvider()})
	case ReturnContinue:
		// Continue resumes the SAME Provider Session on the SAME provider.
		m.status = "continuing the native discussion…"
		return m, m.executeCmd(app.ContinueNativeDiscussionCommand{Workflow: m.selected, Session: model.SessionID(m.discussion.Session)})
	case ReturnFinish:
		m.status = "freezing the change set…"
		return m, m.executeCmd(app.FreezeDiscussionCommand{Workflow: m.selected, Session: model.SessionID(m.discussion.Session)})
	case ReturnSwitch:
		m.status = "switching the discussion session…"
		return m, m.switchAgentCmd()
	case ReturnPause:
		m.page = PageWorkspace
		return m, m.executeCmd(app.PauseWorkflowCommand{Workflow: m.selected})
	case ReturnCancel:
		m.page = PageCancel
		return m, func() tea.Msg {
			return m.queryProjectionMsg(PageCancel, app.CancelSummaryQuery{Workflow: m.selected})
		}
	}
	return m, nil
}

// discussionProvider is the discussion route of the selected workflow
// (the bound session's provider, else the workspace default).
func (m Model) discussionProvider() string {
	if m.provider != "" {
		return m.provider
	}
	return "fake"
}

// switchAgentCmd issues the switch-agent command: a DIFFERENT provider than
// the bound Session's and the user-supplied reason. Without a different
// healthy provider the switch fails closed without a mutation.
func (m Model) switchAgentCmd() tea.Cmd {
	current := m.discussion.Provider
	alt := ""
	for _, p := range m.workspace.Health.Providers {
		if p.Compatible && p.Name != current {
			alt = p.Name
			break
		}
	}
	if alt == "" {
		return func() tea.Msg {
			return commandDoneMsg{err: fmt.Errorf("no different provider is available to switch to")}
		}
	}
	reason := m.discussion.SwitchReason
	if strings.TrimSpace(reason) == "" {
		reason = "user switched the discussion agent"
	}
	return m.executeCmd(app.SwitchAgentCommand{
		Workflow: m.selected,
		Session:  model.SessionID(m.discussion.Session),
		Provider: alt,
		Reason:   reason,
	})
}

// handleHandoffKey handles the handoff content editor.
func (m Model) handleHandoffKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case msg.Code == tea.KeyEsc:
		m.discussion.Editing = false
		m.discussion.Handoff = ""
		return m, nil
	case msg.Code == tea.KeyEnter:
		content, err := handoffDecisions(m.discussion.Handoff, m.selected, model.SessionID(m.discussion.Session), m.discussion.ChangeSetRef)
		if err != nil {
			m.discussion.Status = err.Error()
			return m, nil
		}
		m.discussion.Status = ""
		m.discussion.Editing = false
		m.status = "finishing the discussion…"
		return m, m.executeCmd(app.FinishDiscussionCommand{
			Workflow: m.selected, Session: model.SessionID(m.discussion.Session), Decisions: content,
		})
	case msg.Code == tea.KeyBackspace || msg.Code == tea.KeyDelete:
		if len(m.discussion.Handoff) > 0 {
			m.discussion.Handoff = m.discussion.Handoff[:len(m.discussion.Handoff)-1]
		}
		return m, nil
	case msg.Text != "":
		m.discussion.Handoff += msg.Text
		return m, nil
	}
	return m, nil
}

// handlePlanApprovalKey handles the Plan Approval page: 'g' generates a
// new Plan Revision, 'k' runs the independent check, and the explicit
// confirmation (Enter alone never approves) issues ApprovePlanCommand.
func (m Model) handlePlanApprovalKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.selected == "" {
		m.status = "no workflow selected"
		return m, nil
	}
	switch {
	case msg.Code == 'g' || msg.Code == 'G':
		m.status = "generating a plan revision…"
		return m, m.executeCmd(app.GeneratePlanCommand{Workflow: m.selected, Provider: m.discussionProvider()})
	case msg.Code == 'k' || msg.Code == 'K':
		// GeneratePlan settles before the asynchronous page projection
		// reload is applied. Refuse a stale check request locally instead
		// of sending CheckPlan against the still-visible PLAN_GENERATION
		// stage.
		if m.plan.Stage != model.StagePlanCheck || m.plan.Revision < 1 || m.plan.Hash == "" {
			m.status = "plan projection is still refreshing; wait for PLAN_CHECK"
			return m, nil
		}
		m.status = "running the independent plan check…"
		return m, m.executeCmd(app.CheckPlanCommand{Workflow: m.selected, Provider: m.discussionProvider()})
	case msg.Code == tea.KeyEsc || IsQuit(msg):
		m.page = PageWorkspace
		return m, nil
	}
	upd, cmd := m.approval.Update(msg)
	m.approval = upd
	if m.approval.Confirmed && m.approval.Yes {
		if m.plan.Revision < 1 || m.plan.Hash == "" {
			m.status = "no plan revision to approve"
			m.approval.Confirmed = false
			m.approval.Yes = false
			return m, nil
		}
		m.approval.Confirmed = false
		m.approval.Yes = false
		return m, m.executeCmd(app.ApprovePlanCommand{Workflow: m.selected, Revision: m.plan.Revision, Hash: m.plan.Hash})
	}
	return m, cmd
}

// handleExecutionApprovalKey handles the Execution Approval page: 's'
// generates the Specs, 'w' compiles the Dynamic Workflow, 'd' runs the
// Execution Dry Run, and the explicit confirmation (Enter alone never
// approves) issues ApproveExecutionCommand binding the exact preview
// hashes.
func (m Model) handleExecutionApprovalKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.selected == "" {
		m.status = "no workflow selected"
		return m, nil
	}
	switch {
	case msg.Code == 's' || msg.Code == 'S':
		m.status = "generating the specs…"
		return m, m.executeCmd(app.GenerateSpecsCommand{Workflow: m.selected, Provider: m.discussionProvider()})
	case msg.Code == 'w' || msg.Code == 'W':
		m.status = "compiling the workflow…"
		return m, m.executeCmd(app.CompileWorkflowCommand{Workflow: m.selected, Provider: m.discussionProvider()})
	case msg.Code == 'd' || msg.Code == 'D':
		m.status = "running the execution dry run…"
		return m, m.executeCmd(app.ExecutionDryRunCommand{Workflow: m.selected})
	case msg.Code == tea.KeyEsc || IsQuit(msg):
		m.page = PageWorkspace
		return m, nil
	}
	upd, cmd := m.approval.Update(msg)
	m.approval = upd
	if m.approval.Confirmed && m.approval.Yes {
		// The Approval binds the exact displayed hashes: a partial
		// preview (e.g., before the dry run fixed the routing, budget,
		// and commit-policy hashes) can never be approved.
		if m.preview.PlanHash == "" || len(m.preview.SpecHashes) == 0 ||
			m.preview.CatalogHash == "" || m.preview.WorkflowHash == "" ||
			m.preview.CommitPolicyHash == "" {
			m.status = "no complete execution preview to approve; run the dry run first"
			m.approval.Confirmed = false
			m.approval.Yes = false
			return m, nil
		}
		m.approval.Confirmed = false
		m.approval.Yes = false
		return m, m.executeCmd(app.ApproveExecutionCommand{
			Workflow:         m.selected,
			PlanHash:         m.preview.PlanHash,
			SpecHashes:       m.preview.SpecHashes,
			CatalogHash:      m.preview.CatalogHash,
			WorkflowHash:     m.preview.WorkflowHash,
			RoutingHash:      m.preview.RoutingHash,
			BudgetHash:       m.preview.BudgetHash,
			CommitPolicyHash: m.preview.CommitPolicyHash,
			ChangeSetHash:    m.preview.ChangeSetHash,
		})
	}
	return m, cmd
}

// handleExecutionKey handles the Execution page: 'r' resumes & runs, 'a'
// runs the Workspace Adoption Gate, and the page panes navigate with
// left/right. The Resume command is issued ONLY when the Runtime's
// LegalActions include it (a PAUSED workflow); otherwise the runner starts
// directly (design §5.3: the TUI never re-infers the state machine).
func (m Model) handleExecutionKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.selected == "" {
		m.status = "no workflow selected"
		return m, nil
	}
	switch {
	case msg.Code == 'r' || msg.Code == 'R':
		if m.running {
			m.status = "a runner is already active"
			return m, nil
		}
		m.status = "starting the foreground runner…"
		m.pendingDecision = ""
		if hasAction(m.workspace.Actions, ActionResume) {
			m.resumeThenRun = true
			return m, m.executeCmd(app.ResumeWorkflowCommand{Workflow: m.selected})
		}
		return m.startRunner()
	case msg.Code == 'a' || msg.Code == 'A':
		if m.pendingDecision != "" {
			m.status = "adopting the workspace…"
			return m, m.executeCmd(app.AdoptWorkspaceCommand{Workflow: m.selected})
		}
		return m, nil
	case msg.Code == tea.KeyEsc || IsQuit(msg):
		m.page = PageWorkspace
		return m, nil
	}
	upd, _ := m.execution.Update(msg)
	m.execution = upd
	return m, nil
}

// handleBlockedKey handles the Blocked page: the Resume key is offered
// ONLY when the Runtime's LegalActions include it (design §5.3: the TUI
// renders and issues only the Runtime's legal actions; a blocked workflow
// whose LegalActions contain no Resume offers no resume key).
func (m Model) handleBlockedKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case msg.Code == 'r' || msg.Code == 'R':
		if m.selected == "" || !hasAction(m.workspace.Actions, ActionResume) {
			m.status = "resume is not a legal action"
			return m, nil
		}
		m.status = "resuming…"
		return m, m.executeCmd(app.ResumeWorkflowCommand{Workflow: m.selected})
	case msg.Code == tea.KeyEsc || IsQuit(msg):
		m.page = PageWorkspace
		return m, nil
	case IsLeft(msg):
		return m.moveNav(-1)
	case IsRight(msg):
		return m.moveNav(1)
	}
	return m, nil
}

// handleTerminalKey handles the Terminal page (Report/Apply/Cleanup):
// 'r' renders the Final Report, 'p' stages the Apply, 'c' produces the
// Cleanup Dry Run Manifest, and the explicit confirmations (Enter alone
// never delivers or deletes) execute the Apply and the Cleanup.
func (m Model) handleTerminalKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.selected == "" {
		m.status = "no workflow selected"
		return m, nil
	}
	switch {
	case msg.Code == 'r' || msg.Code == 'R':
		m.status = "rendering the final report…"
		return m, func() tea.Msg {
			view, err := m.ctrl.Query(context.Background(), app.ReportQuery{Workflow: m.selected, Build: m.deps.CLI.Build})
			if err != nil {
				return reportLoadedMsg{err: err}
			}
			if rv, ok := view.(app.ReportView); ok {
				return reportLoadedMsg{markdown: rv.Markdown}
			}
			return reportLoadedMsg{err: fmt.Errorf("unexpected report projection")}
		}
	case msg.Code == 'p' || msg.Code == 'P':
		m.status = "staging the apply…"
		return m, m.executeCmd(app.PrepareApplyCommand{Workflow: m.selected})
	case msg.Code == 'c' || msg.Code == 'C':
		m.status = "producing the cleanup dry run manifest…"
		return m, m.executeCmd(app.DryRunCommand{Workflow: m.selected})
	case msg.Code == tea.KeyEsc || IsQuit(msg):
		m.page = PageWorkspace
		return m, nil
	}
	upd, cmd := m.terminal.Update(msg)
	m.terminal = upd
	if m.terminal.Confirmed && m.terminal.Yes {
		switch m.terminal.Section {
		case SectionApply:
			if m.terminal.ApplyPreview == "" {
				m.status = "no staged apply to deliver; stage it first"
				m.terminal.Confirmed = false
				m.terminal.Yes = false
				return m, nil
			}
			m.terminal.Confirmed = false
			m.terminal.Yes = false
			m.status = "delivering the apply…"
			return m, m.executeCmd(app.ExecuteApplyCommand{Workflow: m.selected})
		case SectionCleanup:
			if m.terminal.CleanupPreview == "" {
				m.status = "no cleanup manifest to execute; produce the dry run first"
				m.terminal.Confirmed = false
				m.terminal.Yes = false
				return m, nil
			}
			m.terminal.Confirmed = false
			m.terminal.Yes = false
			m.status = "executing the cleanup…"
			return m, m.executeCmd(app.ExecuteCleanupCommand{Workflow: m.selected, Manifest: m.cleanupManifest()})
		}
	}
	return m, cmd
}

// cleanupManifest is the exact Manifest Ref of the produced Cleanup Dry
// Run ("" when none was produced).
func (m Model) cleanupManifest() model.ArtifactRef {
	if m.terminal.cleanupRef != nil {
		return *m.terminal.cleanupRef
	}
	return model.ArtifactRef{}
}

// handleCancelKey handles the Cancel confirmation (default No; Enter
// alone never cancels).
func (m Model) handleCancelKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.selected == "" {
		m.status = "no workflow selected"
		m.page = PageWorkspace
		return m, nil
	}
	switch {
	case msg.Code == tea.KeyEsc || IsQuit(msg):
		m.page = PageWorkspace
		return m, nil
	case msg.Code == 'y' || msg.Code == 'Y':
		m.status = "cancelling the workflow…"
		return m, m.executeCmd(app.CancelWorkflowCommand{Workflow: m.selected, Reason: "user confirmed cancel in the TUI"})
	case msg.Code == 'n' || msg.Code == 'N' || IsEnter(msg):
		m.page = PageWorkspace
		return m, nil
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Foreground Runner
// ---------------------------------------------------------------------------

// startRunner launches the Foreground Runner over the shared
// controller: it drives the approved workflow to a terminal state, a
// user decision, or a safe stop, streaming the committed events into
// the Execution page. The root Model owns the runner lifecycle (design
// §11): startRunner returns the updated Model with running, runCancel,
// eventCh and the subscription set. Starting a Run while one is already
// active is refused (idempotent guard) — no second runner and no second
// subscription.
func (m Model) startRunner() (Model, tea.Cmd) {
	if m.running {
		m.status = "a runner is already active"
		return m, nil
	}
	if m.selected == "" {
		return m, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, id := m.sink.Subscribe()
	m.runCancel = cancel
	m.running = true
	m.eventCh = ch
	return m, tea.Batch(
		func() tea.Msg {
			defer m.sink.Unsubscribe(id)
			r := &foreground.Runner{
				Driver: m.ctrl,
				OnEvent: func(ev model.Event) {
					m.sink.Publish(ev)
				},
			}
			res, err := r.Run(ctx, m.selected)
			if err != nil {
				return runnerDoneMsg{err: err}
			}
			return runnerDoneMsg{res: res}
		},
		m.pumpEvents(),
	)
}

// pumpEvents forwards one committed Runner event to the Execution page.
// A terminal path can clear eventCh while a pump command is still in
// flight (the renderer-error and the runner's event send come from
// different goroutines with no happens-before edge): a cleared channel
// must end the pump with eventsClosedMsg instead of blocking forever on
// a nil channel (a leaked goroutine).
func (m Model) pumpEvents() tea.Cmd {
	ch := m.eventCh
	return func() tea.Msg {
		if ch == nil {
			return eventsClosedMsg{}
		}
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return runnerEventMsg{ev: ev}
	}
}

// ---------------------------------------------------------------------------
// Native Session Bridge
// ---------------------------------------------------------------------------

// nativeExec is the Bubble Tea blocking-Exec adapter of the Native
// Session Bridge (design §9.2, TUI task 12): the Program suspends its
// renderer, attaches the terminal streams, runs the supervised
// interactive process, and restores the renderer when the turn ends.
type nativeExec struct {
	req    native.Request
	result *native.Result
	err    error
}

func (c *nativeExec) Run() error {
	result, err := (native.Bridge{}).Run(context.Background(), c.req)
	c.result = &result
	c.err = err
	return err
}

func (c *nativeExec) SetStdin(r io.Reader)  { c.req.Terminal.In = r }
func (c *nativeExec) SetStdout(w io.Writer) { c.req.Terminal.Out = w }
func (c *nativeExec) SetStderr(w io.Writer) { c.req.Terminal.Err = w }

// newNativeExecCmd builds the blocking-exec adapter of one prepared
// native discussion turn.
func newNativeExecCmd(req *app.NativeBridgeRequest) tea.Cmd {
	cmd := &nativeExec{
		req: native.Request{
			Workflow:        req.Workflow,
			Session:         req.Session,
			Provider:        req.Provider,
			ProviderSession: req.ProviderSession,
			Worktree:        req.Worktree,
			Adapter:         req.Adapter,
			Supervisor:      req.Supervisor,
		},
	}
	return tea.Exec(cmd, func(err error) tea.Msg {
		var res native.Result
		if cmd.result != nil {
			res = *cmd.result
		}
		return nativeDoneMsg{result: res, err: err}
	})
}

// ---------------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------------

// View renders the current screen.
func (m Model) View() tea.View {
	v := tea.NewView(render(m))
	v.AltScreen = true
	return v
}

// render is the pure screen renderer of the root model.
func render(m Model) string {
	if m.err != nil {
		return fmt.Sprintf("cflow: %v\n\npress q to quit", m.err)
	}
	if !m.ready {
		return "cflow\n\npress q to quit"
	}
	var b strings.Builder
	switch m.page {
	case PageWorkspace:
		b.WriteString(RenderWorkspace(m.workspace, m.width))
	case PageDiscussion:
		b.WriteString(RenderDiscussionReturn(m.discussion))
		b.WriteString(m.hints())
	case PagePlanApproval:
		b.WriteString(RenderPlanApproval(m.plan, m.approval))
		b.WriteString(m.hints())
	case PageExecutionApproval:
		b.WriteString(RenderApproval(m.approval))
		b.WriteString(m.hints())
	case PageExecution:
		b.WriteString(RenderExecutionAt(m.execution, m.width))
		if m.pendingDecision != "" {
			b.WriteString(renderDecisionPanel(m.pendingDecision))
		}
		b.WriteString(m.hints())
	case PageBlocked:
		b.WriteString(RenderBlocked(m.workspace))
		b.WriteString(m.hints())
	case PageTerminal:
		b.WriteString(RenderTerminal(m.terminal))
		b.WriteString(m.hints())
	case PageCreate:
		b.WriteString(renderCreate(m))
		b.WriteString(m.hints())
	case PageCancel:
		b.WriteString(renderCancel(m))
		b.WriteString(m.hints())
	case PagePauseExit:
		b.WriteString(RenderPauseExit())
	case PageMigration:
		b.WriteString(renderMigration(m))
		b.WriteString(m.hints())
	}
	if m.status != "" {
		fmt.Fprintf(&b, "\nstatus: %s\n", m.status)
	}
	return b.String()
}

// hints is the key hint footer of the current page. The workflow-action
// hints are driven ONLY by the Runtime LegalActions projection (design
// §5.3): a page never advertises a key whose action the Runtime does not
// currently permit.
func (m Model) hints() string {
	switch m.page {
	case PageWorkspace:
		return workspaceHints(m.workspace)
	case PageDiscussion:
		return "\n↑/↓ action  Enter run  b workspace\n"
	case PagePlanApproval:
		return "\ng generate plan  k check plan  Enter/y confirm  q quit\n"
	case PageExecutionApproval:
		return "\ns generate specs  w compile workflow  d dry run  Enter/y confirm  q quit\n"
	case PageExecution:
		// The hint is driven by the Runtime LegalActions: "r resume & run"
		// only while the Runtime permits Resume (a PAUSED workflow); once the
		// post-approval projection reloads the RUNNING workflow the hint
		// becomes the plain "r start the runner" (the runner is started
		// directly; it resumes first only when Resume is legal).
		if hasAction(m.workspace.Actions, ActionResume) {
			return "\nr resume & run  a adopt workspace  ←/→ panes  b workspace\n"
		}
		return "\nr start the runner  a adopt workspace  ←/→ panes  b workspace\n"
	case PageBlocked:
		return blockedHints(m.workspace)
	case PageTerminal:
		return "\n←/→ section  r report  p stage apply  c cleanup dry run  Enter/y confirm  q quit\n"
	case PageCreate:
		return createHints(m)
	case PageCancel:
		return "\nEnter/y cancel (default no), n/esc back\n"
	case PagePauseExit:
		return ""
	case PageMigration:
		return "\np prepare  e execute  y confirm  Enter/n default no  Esc back\n"
	}
	return ""
}

// workspaceHints renders the Workspace page hint from the selected
// workflow's Runtime LegalActions only.
func workspaceHints(m WorkspaceModel) string {
	parts := []string{"↑/↓ select workflow", "←/→ lifecycle", "n create", "q quit"}
	if hasAction(m.Actions, ActionResume) {
		parts = append(parts, "r resume")
	}
	if hasAction(m.Actions, ActionPause) {
		parts = append(parts, "p pause")
	}
	if hasAction(m.Actions, ActionCancel) {
		parts = append(parts, "x cancel")
	}
	if hasAction(m.Actions, ActionMigrate) {
		parts = append(parts, "m migrate")
	}
	return "\n" + strings.Join(parts, "  ") + "\n"
}

// blockedHints renders the Blocked page hint from the Runtime LegalActions
// only: Resume appears solely when the Runtime permits it.
func blockedHints(m WorkspaceModel) string {
	parts := []string{"b workspace"}
	if hasAction(m.Actions, ActionResume) {
		parts = append(parts, "r resume")
	}
	return "\n" + strings.Join(parts, "  ") + "\n"
}

// createHints renders the Create page hint: the confirmation defaults to
// No, so Enter alone never creates; only an explicit y does.
func createHints(m Model) string {
	if m.createConfirm {
		return "\ny create (default no), Enter/n decline, esc back to edit\n"
	}
	return "\ntype the workflow name; Enter review, esc cancel\n"
}

func renderMigration(m Model) string {
	var b strings.Builder
	fmt.Fprintf(&b, "legacy layout migration %s\n", m.migration.Workflow)
	r := func(value string) string { return redactMigrationText(m.deps.CLI.Redaction, value) }
	fmt.Fprintf(&b, "layout: %d -> %d\nstatus: %s\nmanifest: %s\n", m.migration.From, m.migration.To, r(m.migration.Status), r(m.migration.ManifestHash))
	if m.migration.MigrationID != "" {
		fmt.Fprintf(&b, "migration id: %s\n", r(m.migration.MigrationID))
	}
	if m.migration.ManifestPath != "" {
		fmt.Fprintf(&b, "manifest path: %s\n", r(m.migration.ManifestPath))
	}
	if m.migration.BackupPath != "" {
		fmt.Fprintf(&b, "backup: %s sha256=%s size=%d\n", r(m.migration.BackupPath), r(m.migration.BackupHash), m.migration.BackupSize)
	}
	if m.migration.SourceSnapshotHash != "" {
		fmt.Fprintf(&b, "source snapshot: %s\n", r(m.migration.SourceSnapshotHash))
	}
	fmt.Fprintf(&b, "database impact: workspace=%s branch=%s head=%s\n",
		r(m.migration.ExpectedWorkspacePath), r(m.migration.ExpectedWorkspaceBranch), r(m.migration.ExpectedWorkspaceHead))
	for i, move := range m.migration.Moves {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   -> %s\n   branch=%s head=%s digest=%s\n",
			i+1, move.Kind, r(move.Source), r(move.Destination), r(move.Branch), r(move.Head), r(move.Digest))
	}
	switch m.migrationConfirm {
	case migrationConfirmPrepare:
		b.WriteString("\nPersist immutable manifest and recoverable intent? [y/N]\n")
	case migrationConfirmExecute:
		b.WriteString("\nExecute the exact persisted migration intent? [y/N]\n")
	}
	return b.String()
}

func redactMigrationText(reg security.Registry, value string) string {
	if value == "" {
		return "-"
	}
	red := security.NewRedactor(reg)
	frame, err := red.WriteFrame([]byte(value))
	if err != nil {
		return "[REDACTED]"
	}
	flushed, err := red.Flush()
	if err != nil {
		return "[REDACTED]"
	}
	return frame.Text + flushed.Text
}

// renderCreate renders the Create Workflow page: the read-only Discovery
// projection surfaces the target's dirty state, dirty fingerprint, and
// isolation before the default-No confirmation (Task 5).
func renderCreate(m Model) string {
	var b strings.Builder
	b.WriteString("create workflow\n")
	b.WriteString("project: " + m.workspace.Project.Name + " (" + m.workspace.Project.Root + ")\n")
	b.WriteString("provider: " + m.createProvider() + "\n")
	if d := m.createDirty; d != nil {
		if d.Branch != "" {
			fmt.Fprintf(&b, "target: %s @ %s\n", d.Branch, shortHead(d.Head))
		}
		if d.Dirty {
			fmt.Fprintf(&b, "target: DIRTY (%d staged, %d unstaged, %d untracked)\n",
				d.StagedCount, d.UnstagedCount, d.UntrackedCount)
			fmt.Fprintf(&b, "dirty fingerprint: %s\n", d.DirtyFingerprint)
		} else {
			b.WriteString("target working tree: clean\n")
		}
		b.WriteString("isolation: this workflow will not touch your files until explicit apply\n")
	} else {
		b.WriteString("target: (loading git facts…)\n")
	}
	if m.createConfirm {
		fmt.Fprintf(&b, "name: %s\n", m.createInput)
		b.WriteString("create workflow? [y/N]\n")
	} else {
		fmt.Fprintf(&b, "name: %s_\n", m.createInput)
		b.WriteString("enter review, then y to create (default no)\n")
	}
	return b.String()
}

func renderCancel(m Model) string {
	var b strings.Builder
	b.WriteString("cancel workflow?\n")
	if m.cancel.Workflow != "" {
		fmt.Fprintf(&b, "workflow %s  %s/%s\n", m.cancel.Workflow, m.cancel.Stage, m.cancel.Runtime)
		fmt.Fprintf(&b, "active nodes: %d  active sessions: %d  worktrees: %d\n",
			len(m.cancel.ActiveNodes), len(m.cancel.ActiveSessions), len(m.cancel.Worktrees))
	}
	b.WriteString("every artifact, session, worktree, branch, commit, and audit ref is preserved.\n")
	b.WriteString("confirm: no (Enter to choose; Enter alone never cancels)\n")
	return b.String()
}

// renderDecisionPanel renders the decision panel the Foreground Runner
// stopped at (design §11.2: the TUI surfaces the Runtime's reason and
// the legal actions).
func renderDecisionPanel(reason string) string {
	return fmt.Sprintf("\ndecision required: %s\n", reason)
}

// renderApplyAttempt renders the staged Apply preview of the Terminal
// page.
func renderApplyAttempt(a model.ApplyAttempt) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apply %s — %s\n", a.ID, a.Status)
	if a.StagingHead != "" {
		fmt.Fprintf(&b, "delivery head: %s\n", shortHead(a.StagingHead))
	}
	fmt.Fprintf(&b, "the target branch changes ONLY on the explicit delivery confirmation.\n")
	return b.String()
}

// renderCleanupAttempt renders the Cleanup Dry Run Manifest of the
// Terminal page.
func renderCleanupAttempt(a model.CleanupAttempt) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cleanup manifest %s — %s (%d exact targets)\n", a.ID, a.Status, len(a.Items))
	for _, it := range a.Items {
		fmt.Fprintf(&b, "  - %s %s\n", it.Kind, it.CanonicalPath)
	}
	b.WriteString("artifacts, evidence, reports, the database, and the refs are never deleted.\n")
	return b.String()
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
