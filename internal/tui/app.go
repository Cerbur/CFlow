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

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/cli"
	"cflow.local/cflow/internal/foreground"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/native"
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
	// status is the transient status line.
	status string

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
		return m, m.pumpEvents()
	case eventsClosedMsg:
		return m, nil
	case runnerDoneMsg:
		return m.applyRunnerDone(msg)
	case nativeDoneMsg:
		if msg.err != nil {
			m.status = "native session: " + msg.err.Error()
		} else {
			m.status = fmt.Sprintf("native session %s returned (exit %d)", msg.result.Session, msg.result.Exit.Code)
		}
		return m, m.reloadCmd()
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
				for _, p := range v.Health.Providers {
					if p.Compatible {
						m.provider = p.Name
						break
					}
				}
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
		}
	case PageExecutionApproval:
		if v, ok := msg.view.(app.ExecutionPreviewView); ok {
			m.preview = v
			m.approval = ApprovalModel{Preview: v}
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
			return m, tea.Quit
		}
		return m, nil
	}
	switch msg.cmd.(type) {
	case app.CreateWorkflowCommand:
		m.selected = msg.out.Workflow
		m.status = "workflow created"
		m.page = PageWorkspace
		return m, m.reloadCmd()
	case app.PauseWorkflowCommand:
		if m.stop == stopPauseAndExit {
			// The Pause and Exit confirmation: the pause completed, so
			// no background process is left; finish the exit.
			return m, tea.Quit
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
			return m, tea.Batch(m.reloadCmd(), m.startRunner())
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
		m.status = "plan generated"
		return m, m.reloadCmd()
	case app.CheckPlanCommand:
		m.status = "plan checked"
		return m, m.reloadCmd()
	case app.ApprovePlanCommand:
		m.status = "plan approved"
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
func (m Model) applyRunnerDone(msg runnerDoneMsg) (Model, tea.Cmd) {
	m.running = false
	m.runCancel = nil
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
				// processes (never just a local flag).
				cmds = append(cmds, m.executeCmd(app.PauseWorkflowCommand{Workflow: m.selected}))
			}
			return m, tea.Batch(cmds...)
		default:
			// The second Ctrl+C is the Force Stop of the active Runner
			// (and of any running controlled stop).
			m.forceStop()
			return m, tea.Quit
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
		return m, tea.Quit
	case IsEnter(msg) || msg.Code == 'n' || msg.Code == 'N' || msg.Code == tea.KeyEsc:
		m.stop = stopIdle
		m.page = m.prevPage
		return m, nil
	}
	return m, nil
}

// handleCreateKey handles the Create Workflow form.
func (m Model) handleCreateKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case msg.Code == tea.KeyEsc:
		m.page = PageWorkspace
		m.createInput = ""
		return m, nil
	case msg.Code == tea.KeyEnter:
		name := strings.TrimSpace(m.createInput)
		if name == "" {
			m.status = "a workflow name is required"
			return m, nil
		}
		m.status = "creating workflow…"
		m.createInput = ""
		return m, m.executeCmd(app.CreateWorkflowCommand{
			Name: name, Provider: m.createProvider(), ConfirmDirty: true,
		})
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
	for _, p := range m.workspace.Health.Providers {
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
		m.page = PageCreate
		m.createInput = ""
		return m, nil
	case msg.Code == 'r' || msg.Code == 'R':
		if m.selected == "" {
			m.status = "no workflow selected"
			return m, nil
		}
		m.status = "resuming…"
		return m, m.executeCmd(app.ResumeWorkflowCommand{Workflow: m.selected})
	case msg.Code == 'p' || msg.Code == 'P':
		if m.selected == "" {
			m.status = "no workflow selected"
			return m, nil
		}
		m.status = "pausing…"
		return m, m.executeCmd(app.PauseWorkflowCommand{Workflow: m.selected})
	case msg.Code == 'x' || msg.Code == 'X':
		if m.selected == "" {
			m.status = "no workflow selected"
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
	case ReturnStart, ReturnContinue:
		m.status = "preparing the native discussion…"
		return m, m.executeCmd(app.PrepareNativeDiscussionCommand{Workflow: m.selected, Provider: m.discussionProvider()})
	case ReturnFinish:
		m.status = "freezing the change set…"
		return m, m.executeCmd(app.FreezeDiscussionCommand{Workflow: m.selected, Session: model.SessionID(m.discussion.Session)})
	case ReturnSwitch:
		m.status = "switching the discussion session…"
		return m, m.executeCmd(app.PrepareNativeDiscussionCommand{Workflow: m.selected, Provider: m.discussionProvider()})
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

// handleHandoffKey handles the handoff content editor.
func (m Model) handleHandoffKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case msg.Code == tea.KeyEsc:
		m.discussion.Editing = false
		m.discussion.Handoff = ""
		return m, nil
	case msg.Code == tea.KeyEnter:
		body, err := buildHandoff(m.discussion.Handoff, m.selected, model.SessionID(m.discussion.Session), m.discussion.ChangeSetRef)
		if err != nil {
			m.discussion.Status = err.Error()
			return m, nil
		}
		m.discussion.Status = ""
		m.discussion.Editing = false
		m.status = "finishing the discussion…"
		return m, m.executeCmd(app.FinishDiscussionCommand{
			Workflow: m.selected, Session: model.SessionID(m.discussion.Session), Handoff: body,
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

// workflowRuntime queries the authoritative Runtime of the selected
// workflow ("" when it cannot be observed).
func (m Model) workflowRuntime() model.RuntimeStatus {
	if m.ctrl == nil || m.selected == "" {
		return ""
	}
	v, err := m.ctrl.Query(context.Background(), app.StatusQuery{Workflow: m.selected})
	if err != nil {
		return ""
	}
	if sv, ok := v.(app.StatusView); ok {
		return sv.Runtime
	}
	return ""
}

// handleExecutionKey handles the Execution page: 'r' resumes and starts
// the Foreground Runner, 'a' runs the Workspace Adoption Gate, and the
// page panes navigate with left/right.
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
		// The authoritative runtime comes from the Application (the
		// rendered projection may lag the committed approval by a
		// frame); only a PAUSED workflow needs the resume first.
		if m.workflowRuntime() == model.RuntimePaused {
			m.resumeThenRun = true
			return m, m.executeCmd(app.ResumeWorkflowCommand{Workflow: m.selected})
		}
		return m, m.startRunner()
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

// handleBlockedKey handles the Blocked page: only the Runtime legal
// actions are offered.
func (m Model) handleBlockedKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case msg.Code == 'r' || msg.Code == 'R':
		if m.selected == "" {
			m.status = "no workflow selected"
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
// the Execution page.
func (m Model) startRunner() tea.Cmd {
	if m.selected == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.runCancel = cancel
	m.running = true
	ch, id := m.sink.Subscribe()
	m.eventCh = ch
	return tea.Batch(
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
func (m Model) pumpEvents() tea.Cmd {
	ch := m.eventCh
	return func() tea.Msg {
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
		b.WriteString(hints(m.page))
	case PageDiscussion:
		b.WriteString(RenderDiscussionReturn(m.discussion))
		b.WriteString(hints(m.page))
	case PagePlanApproval:
		b.WriteString(RenderPlanApproval(m.plan, m.approval))
		b.WriteString(hints(m.page))
	case PageExecutionApproval:
		b.WriteString(RenderApproval(m.approval))
		b.WriteString(hints(m.page))
	case PageExecution:
		b.WriteString(RenderExecution(m.execution))
		if m.pendingDecision != "" {
			b.WriteString(renderDecisionPanel(m.pendingDecision))
		}
		b.WriteString(hints(m.page))
	case PageBlocked:
		b.WriteString(RenderBlocked(m.workspace))
		b.WriteString(hints(m.page))
	case PageTerminal:
		b.WriteString(RenderTerminal(m.terminal))
		b.WriteString(hints(m.page))
	case PageCreate:
		b.WriteString(renderCreate(m))
		b.WriteString(hints(m.page))
	case PageCancel:
		b.WriteString(renderCancel(m))
		b.WriteString(hints(m.page))
	case PagePauseExit:
		b.WriteString(RenderPauseExit())
	case PageMigration:
		b.WriteString(renderMigration(m))
		b.WriteString(hints(m.page))
	}
	if m.status != "" {
		fmt.Fprintf(&b, "\nstatus: %s\n", m.status)
	}
	return b.String()
}

// hints is the fixed key hint footer of one page.
func hints(p Page) string {
	switch p {
	case PageWorkspace:
		return "\n↑/↓ select workflow  ←/→ lifecycle  n create  r resume  p pause  m migrate  x cancel  q quit\n"
	case PageDiscussion:
		return "\n↑/↓ action  Enter run  b workspace\n"
	case PagePlanApproval:
		return "\ng generate plan  k check plan  Enter/y confirm  q quit\n"
	case PageExecutionApproval:
		return "\ns generate specs  w compile workflow  d dry run  Enter/y confirm  q quit\n"
	case PageExecution:
		return "\nr resume & run  a adopt workspace  ←/→ panes  b workspace\n"
	case PageBlocked:
		return "\nr resume  b workspace\n"
	case PageTerminal:
		return "\n←/→ section  r report  p stage apply  c cleanup dry run  Enter/y confirm  q quit\n"
	case PageCreate:
		return "\ntype the workflow name; Enter create, Esc cancel\n"
	case PageCancel:
		return "\nEnter/y cancel (default no), n/esc back\n"
	case PagePauseExit:
		return ""
	case PageMigration:
		return "\np prepare  e execute  y confirm  Enter/n default no  Esc back\n"
	}
	return ""
}

func renderMigration(m Model) string {
	var b strings.Builder
	fmt.Fprintf(&b, "legacy layout migration %s\n", m.migration.Workflow)
	fmt.Fprintf(&b, "layout: %d -> %d\nmanifest: %s\n", m.migration.From, m.migration.To, m.migration.ManifestHash)
	for i, move := range m.migration.Moves {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   -> %s\n", i+1, move.Kind, move.Source, move.Destination)
	}
	switch m.migrationConfirm {
	case migrationConfirmPrepare:
		b.WriteString("\nPersist immutable manifest and recoverable intent? [y/N]\n")
	case migrationConfirmExecute:
		b.WriteString("\nExecute the exact persisted migration intent? [y/N]\n")
	}
	return b.String()
}

func renderCreate(m Model) string {
	var b strings.Builder
	b.WriteString("create workflow\n")
	b.WriteString("project: " + m.workspace.Project.Name + " (" + m.workspace.Project.Root + ")\n")
	b.WriteString("provider: " + m.createProvider() + "\n")
	fmt.Fprintf(&b, "name: %s_\n", m.createInput)
	b.WriteString("\n(user workspace changes are isolated and never enter the workflow)\n")
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
