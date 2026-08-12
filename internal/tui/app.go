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
	Context context.Context
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	// OperationLog receives bounded JSONL diagnostics for GUI/TUI debugging.
	// It is never used as an authority for Runtime state.
	OperationLog io.Writer
	Program      *tea.Program // nil: the Run call creates the default program
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
	if ctx == nil {
		ctx = context.Background()
	}
	deps.Context = ctx
	if deps.OperationLog == nil {
		// Structured operation diagnostics must never share the renderer's
		// stderr stream: stderr is part of the terminal surface and would
		// corrupt the full-screen TUI. The production entry point injects
		// the managed .cflow log file; tests and embedders without a sink
		// simply discard diagnostics.
		deps.OperationLog = io.Discard
	}
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
		// Cancellation is delivered to the Model as a normal message so an
		// active Foreground Runner can unwind and join before Bubble Tea
		// exits. tea.WithContext would stop the program underneath the Model
		// and could strand the runner command goroutine.
		prog = tea.NewProgram(newModel(deps), tea.WithInput(in), tea.WithOutput(out))
	}
	model, runErr := prog.Run()
	if m, ok := model.(Model); ok {
		if runErr != nil && m.running {
			// Bubble Tea may return an event-loop/renderer error before the
			// normal runnerDoneMsg reaches Update. Cancel and join directly
			// before propagating that error.
			m.forceStop()
		}
		// A context-driven stop may return the last Model before the runner's
		// command message is processed. The command owns this join channel;
		// wait here as a final safety net against an unattached Runner.
		if m.running && m.runnerDone != nil {
			<-m.runnerDone
		}
		if m.err != nil {
			return m.err
		}
	}
	if runErr != nil {
		return runErr
	}
	return nil
}

// Page is one screen of the TUI.
type Page int

const (
	PageWorkspace Page = iota
	PageWorkflowMenu
	PageReadonlyWorkspace
	PageActionPreview
	PageCreatePreview
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

type commandState struct {
	inFlight     bool
	generation   uint64 // latest mutating command generation
	pending      uint64 // mutating command awaiting acknowledgement
	ackPage      Page   // projection page that owns the command's facts
	workflow     model.WorkflowID
	ackRetries   uint8
	projectionID uint64 // monotonic read-only query sequence
	operationSeq uint64
	operationID  string
}

type contextDoneMsg struct{}

// Model is the root TUI model: the read-only workspace projection, the
// lifecycle pages, the runner state, and the controlled-stop protocol.
type Model struct {
	width  int
	height int
	ready  bool
	err    error

	deps  Dependencies
	ctx   context.Context
	ctrl  controller
	trace *operationLogger

	// page is the current screen.
	page Page
	// navigation is UI-only. Runtime lifecycle state remains authoritative in
	// Application projections and is never inferred from these frames.
	navigation NavigationStack
	// workflowMenu is the authoritative Task 2 projection currently bound to
	// the selected Workflow. workflowMenuModel is its pure TUI presentation
	// copy; neither owns Runtime state.
	workflowMenu            app.WorkflowMenuView
	workflowMenuModel       WorkflowMenuModel
	workflowMenuIndex       int
	workflowMenuError       string
	workflowMenuPreviewItem MenuItem
	// readonly is the bounded, query-backed workspace for the selected menu
	// route. It contains no command or Runtime authority.
	readonly ReadonlyWorkspaceModel

	// selected is the workflow the workspace focuses.
	selected model.WorkflowID
	// workspace is the renderable workspace projection.
	workspace WorkspaceViewModel
	// provider is the discussion provider route of the selected
	// workflow ("" until a session exists; the create page falls back
	// to the first healthy provider).
	provider string
	// commandState is shared by Model copies so a mutating key cannot enqueue
	// another Application command before the previous command's projection
	// acknowledgement arrives.
	commandState      *commandState
	lastProjectionID  uint64
	projectionBlocked bool
	blockedAckPage    Page
	blockedWorkflow   model.WorkflowID

	discussion         DiscussionPage
	plan               app.PlanView
	preview            app.ExecutionPreviewView
	approval           ApprovalModel
	execution          ExecutionModel
	terminal           TerminalModel
	reportRequestID    uint64
	cancel             app.CancelSummaryView
	migration          app.MigrationPreviewView
	migrationConfirm   migrationConfirmation
	migrationPreviewed bool
	cancelPreview      bool
	actionPreviewed    bool

	// pendingDecision is the Runtime's reason the Foreground Runner
	// stopped at (design §11.2): the Execution page surfaces the
	// decision panel while it is set.
	pendingDecision string

	// createInput is the Create Workflow name field.
	createInput string
	// createConfirm is true after the user submitted the create name for
	// the Create Preview (Task 6: the next Enter executes).
	createConfirm bool
	// createAwaitingWorkspace remains true between a successful
	// CreateWorkflowCommand and its bound Workspace acknowledgement. The
	// navigation transition waits for that authoritative projection.
	createAwaitingWorkspace bool
	// createDirty is the queried target Git facts the Create page displays
	// (dirty state, fingerprint, and isolation); nil until the read-only
	// DiscoveryQuery projection loads.
	createDirty *app.DiscoveryView
	// commandPalette is the transient global command surface. It is UI-only;
	// selected commands are translated into the existing typed stop seam.
	commandPalette CommandPaletteModel
	// status is the transient status line.
	status                  string
	pendingProjectionStatus string
	pendingPlanStatus       string
	// pendingPlanApproval preserves an explicit user approval while the
	// CheckPlan command's authoritative projection is still in flight.
	// The approval is issued only after the refreshed revision/hash is
	// visible, so this never bypasses the stale-plan guard.
	pendingPlanApproval bool
	// pendingPlanCheck preserves an explicit check request while the
	// GeneratePlan acknowledgement projection is still in flight. The
	// request is issued only after a fresh revision/hash is visible.
	pendingPlanCheck          bool
	pendingPlanCheckOperation string
	planCheckInFlight         bool

	// Runner state: running is true while the Foreground Runner is
	// active; runCancel is the runner's context; eventCh is the
	// committed-event subscription the Execution page consumes.
	running        bool
	runCancel      context.CancelFunc
	eventCh        <-chan model.Event
	sink           *foreground.EventSink
	runnerWorkflow model.WorkflowID
	runnerID       uint64

	// stop tracks the controlled-stop state (design §12.1): the first
	// Ctrl+C requests the controlled Pause; the second is the Force
	// Stop of an active Runner. Process exit remains controlled by Ctrl+C;
	// Task 7 adds the /exit UI entry point.
	stop stopState
	// prevPage is the page the Pause and Exit prompt returns to.
	prevPage Page
	// quitAfterRunner keeps the TUI alive until the Foreground Runner has
	// delivered runnerDoneMsg after a controlled or forced stop.
	quitAfterRunner bool
	runnerDone      chan struct{}
	// pauseCommandPending keeps a requested controlled pause joined with the
	// Runner completion. A stop is not complete until both facts arrive.
	pauseCommandPending bool
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
	page              Page
	workflow          model.WorkflowID
	generation        uint64 // unique read-only query sequence
	commandGeneration uint64 // non-zero only for a command acknowledgement query
	operationID       string
	view              app.View
	err               error
}

// appLoadedMsg delivers the shared Application opened by the Init
// command (the TUI never constructs the Application per key press).
type appLoadedMsg struct {
	ctrl controller
	err  error
}

// commandDoneMsg delivers the result of one typed Application Command.
type commandDoneMsg struct {
	cmd         app.Command
	generation  uint64
	operationID string
	out         app.Outcome
	err         error
}

// runnerEventMsg delivers one committed event of the Foreground Runner.
type runnerEventMsg struct {
	ev       model.Event
	workflow model.WorkflowID
	runnerID uint64
}

// eventsClosedMsg ends the event pump (the runner finished).
type eventsClosedMsg struct{}

// runnerDoneMsg delivers the terminal result of the Foreground Runner.
type runnerDoneMsg struct {
	res      foreground.Result
	err      error
	workflow model.WorkflowID
	runnerID uint64
}

// nativeDoneMsg delivers the result of one Native Session Bridge turn.
type nativeDoneMsg struct {
	result native.Result
	err    error
}

// reportLoadedMsg delivers the rendered Final Report of the Terminal
// page.
type reportLoadedMsg struct {
	markdown  string
	err       error
	workflow  model.WorkflowID
	requestID uint64
}

// NewModel returns the initial root model.
func NewModel() Model { return newModel(Dependencies{}) }

// newModel returns the initial root model with the given dependencies.
func newModel(deps Dependencies) Model {
	modelContext := deps.Context
	if modelContext == nil {
		modelContext = context.Background()
	}
	return Model{
		deps:           deps,
		ctx:            modelContext,
		page:           PageWorkspace,
		navigation:     NavigationStack{Frames: []NavigationFrame{{Layer: LayerHome, Page: PageWorkspace}}},
		commandState:   new(commandState),
		trace:          newOperationLogger(deps.OperationLog),
		discussion:     DiscussionPage{},
		approval:       ApprovalModel{},
		execution:      ExecutionModel{},
		terminal:       NewTerminalModel(),
		sink:           foreground.NewEventSink(),
		commandPalette: NewCommandPalette(),
	}
}

// Init is the initial command: open the shared Application (through the
// injected seam or the headless CLI's default construction) and load the
// read-only project workspace. Opening the TUI never resumes, dispatches,
// applies, or cleans up anything.
func (m Model) Init() tea.Cmd {
	if m.ctrl != nil {
		// The tests inject the controller directly.
		return tea.Batch(
			func() tea.Msg { return appLoadedMsg{ctrl: m.ctrl} },
			waitContext(m.context()),
		)
	}
	open := func() tea.Msg {
		var (
			a   *app.Application
			err error
		)
		if m.deps.CLI.OpenApplication != nil {
			a, err = m.deps.CLI.OpenApplication(m.context())
		} else {
			a, err = cli.OpenApplication(m.context(), m.deps.CLI)
		}
		if err != nil {
			return appLoadedMsg{err: err}
		}
		return appLoadedMsg{ctrl: a}
	}
	return tea.Batch(open, waitContext(m.context()))
}

func waitContext(ctx context.Context) tea.Cmd {
	if ctx.Done() == nil {
		// context.Background/context.TODO can never be cancelled; do not
		// create a permanently blocked command (especially in headless
		// synchronous test drivers).
		return nil
	}
	return func() tea.Msg {
		<-ctx.Done()
		return contextDoneMsg{}
	}
}

// queryProjectionMsg runs one read-only Query of the given page and
// returns the projection message.
func (m Model) queryProjectionMsg(page Page, q app.Query) tea.Msg {
	return m.queryProjectionMsgAt(page, q, m.nextProjectionID(), 0)
}

func (m Model) queryProjectionMsgAt(page Page, q app.Query, generation, commandGeneration uint64) tea.Msg {
	workflow := queryWorkflowID(q)
	operationID := ""
	if m.commandState != nil {
		operationID = m.commandState.operationID
	}
	if _, ok := q.(app.WorkflowMenuQuery); ok {
		m.traceUIActionFor(uiActionWorkflowMenuQuery, workflow, operationID)
	}
	m.trace.write(operationLogEntry{
		OperationID:       operationID,
		Workflow:          string(workflow),
		Page:              pageName(page),
		Kind:              "query_started",
		Query:             operationType(q),
		Generation:        generation,
		CommandGeneration: commandGeneration,
	})
	view, err := m.ctrl.Query(m.context(), q)
	if err != nil && page == PageWorkspace {
		// A workflow can disappear between two read-only projections. Retry the
		// aggregate query without the stale selection so the Application can
		// choose the first remaining workflow; this changes no Runtime state.
		if workspaceQ, ok := q.(app.ProjectWorkspaceQuery); ok && workspaceQ.Selected != "" && isMissingWorkflowError(err) {
			view, err = m.ctrl.Query(m.context(), app.ProjectWorkspaceQuery{})
			// Keep the fallback projection unbound. Its selected Workflow is
			// authoritative and applyProjection will normalize local selection.
			workflow = ""
		}
	}
	m.trace.write(operationLogEntry{
		OperationID:       operationID,
		Workflow:          string(workflow),
		Page:              pageName(page),
		Kind:              "query_result",
		Query:             operationType(q),
		Generation:        generation,
		CommandGeneration: commandGeneration,
		View:              operationType(view),
		Result:            operationResult(err),
		ErrorCode:         operationErrorCode(err),
	})
	return projectionMsg{
		page:              page,
		workflow:          workflow,
		generation:        generation,
		commandGeneration: commandGeneration,
		operationID:       operationID,
		view:              view,
		err:               err,
	}
}

// queryWorkflowID extracts the workflow binding carried by TUI projections.
// Workspace queries with an empty selection are intentionally unbound because
// they are the stale-selection recovery path that normalizes to the first
// remaining workflow.
func queryWorkflowID(q app.Query) model.WorkflowID {
	switch q := q.(type) {
	case app.ProjectWorkspaceQuery:
		return q.Selected
	case app.DiscussionReturnQuery:
		return q.Workflow
	case app.PlanQuery:
		return q.Workflow
	case app.StatusQuery:
		return q.Workflow
	case app.InspectQuery:
		return q.Workflow
	case app.LogsQuery:
		return q.Workflow
	case app.ExecutionPreviewQuery:
		return q.Workflow
	case app.ReportQuery:
		return q.Workflow
	case app.CancelSummaryQuery:
		return q.Workflow
	case app.LayoutMigrationPreviewQuery:
		return q.Workflow
	case app.WorkflowMenuQuery:
		return q.Workflow
	default:
		return ""
	}
}

func isMissingWorkflowError(err error) bool {
	code, ok := model.CodeOf(err)
	return ok && code == model.CodeInvalidInput && strings.Contains(err.Error(), "no such workflow:")
}

// isExpectedEmptyProjectionError identifies a read-only lifecycle projection
// that is intentionally absent until a later lifecycle step has produced its
// facts. It is deliberately narrower than CodeInvalidInput: a real query
// failure must keep the mutating-command gate closed, while the execution
// approval page is allowed to render an empty preview before specs/workflow
// compilation and the dry run exist.
func (m Model) isExpectedEmptyProjectionError(page Page, workflow model.WorkflowID, commandGeneration uint64, err error) bool {
	if page != PageExecutionApproval || workflow == "" || err == nil {
		return false
	}
	code, ok := model.CodeOf(err)
	if !ok || code != model.CodeInvalidInput ||
		!strings.Contains(err.Error(), "execution inputs are incomplete; no preview is available") {
		return false
	}
	// Before Specs are generated, Execution Approval has a valid empty state.
	// Once a command is waiting for a compiled workflow or dry-run preview, the
	// same error is a failed acknowledgement and must remain blocking. When a
	// Specs command has just completed, its pending status is the explicit
	// command/lifecycle evidence that this empty state is expected.
	if m.pendingProjectionStatus == "specs generated" {
		if m.workspace.Lifecycle != nil && workflow != "" && m.workspace.Lifecycle.ID != workflow {
			return false
		}
		// While the command is still in flight, only its origin-page
		// acknowledgement may normalize the empty preview. A normal
		// navigation query must not borrow the pending Specs status.
		if m.commandState != nil && m.commandState.inFlight {
			return m.commandState.ackPage == page && commandGeneration == m.commandState.pending
		}
		// Once the command has completed, the workspace lifecycle fact is
		// the authority for the intentionally empty pre-compile state.
		return m.workspace.Lifecycle != nil && m.workspace.Lifecycle.Stage == model.StageSpecGeneration
	}
	if m.commandState != nil && m.commandState.inFlight {
		return false
	}
	if m.workspace.Lifecycle == nil || m.workspace.Lifecycle.ID != workflow || m.workspace.Lifecycle.Stage != model.StageSpecGeneration {
		return false
	}
	return true
}

func projectionViewMatches(page Page, view app.View) bool {
	switch page {
	case PageWorkspace, PageBlocked, PageExecution, PageTerminal:
		_, ok := view.(app.WorkspaceView)
		return ok
	case PageWorkflowMenu:
		_, ok := view.(app.WorkflowMenuView)
		return ok
	case PageReadonlyWorkspace:
		return true
	case PageDiscussion:
		_, ok := view.(app.DiscussionReturnView)
		return ok
	case PagePlanApproval:
		_, ok := view.(app.PlanView)
		return ok
	case PageExecutionApproval:
		_, ok := view.(app.ExecutionPreviewView)
		return ok
	case PageCreate:
		_, ok := view.(app.DiscoveryView)
		return ok
	case PageCancel:
		_, ok := view.(app.CancelSummaryView)
		return ok
	case PageMigration:
		_, ok := view.(app.MigrationPreviewView)
		return ok
	default:
		return false
	}
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
		return m, m.queryCmd(PageWorkspace, app.ProjectWorkspaceQuery{})
	case contextDoneMsg:
		// Keep Bubble Tea alive until the Foreground Runner reports its
		// terminal result. Context cancellation is a stop request, not an
		// excuse to abandon the Runner goroutine or its event subscription.
		if m.running {
			m.quitAfterRunner = true
			m.pauseCommandPending = true
			workflow := m.activeRunnerWorkflow()
			if m.runCancel != nil {
				m.runCancel()
			}
			if workflow != "" {
				// The root context is already cancelled. Use a detached command
				// context so the controlled-stop intent can still be persisted.
				return m, m.executeCmdWithContext(context.WithoutCancel(m.context()), app.PauseWorkflowCommand{Workflow: workflow})
			}
			m.forceStop()
			return m, nil
		}
		return m, tea.Quit
	case projectionMsg:
		return m.applyProjection(msg)
	case commandDoneMsg:
		return m.applyCommand(msg)
	case runnerEventMsg:
		if msg.runnerID != 0 && msg.runnerID != m.runnerID {
			return m, nil
		}
		if msg.workflow != "" && m.runnerWorkflow != "" && msg.workflow != m.runnerWorkflow {
			return m, nil
		}
		m.execution = m.execution.OnEvent(msg.ev)
		if m.running {
			return m, m.pumpEvents(m.runnerWorkflow, m.runnerID)
		}
		// The runner is no longer active (a terminal path already cleared
		// running/eventCh): apply the in-flight event but never re-pump —
		// re-pumping would capture the cleared nil eventCh and leak a
		// goroutine blocked on a nil channel forever.
		return m, nil
	case eventsClosedMsg:
		return m, nil
	case runnerDoneMsg:
		if msg.runnerID != 0 && msg.runnerID != m.runnerID {
			return m, nil
		}
		if msg.workflow != "" && m.runnerWorkflow != "" && msg.workflow != m.runnerWorkflow {
			return m, nil
		}
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
		if msg.requestID != 0 && msg.requestID != m.reportRequestID {
			return m, nil
		}
		if msg.workflow != "" && msg.workflow != m.selected {
			return m, nil
		}
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
		// A renderer failure must still join the Foreground Runner before the
		// TUI exits. Cancellation requests the unwind, while runnerDoneMsg
		// remains the only proof that the goroutine and event subscription are
		// gone (design §16).
		if m.running {
			m.quitAfterRunner = true
			if m.runCancel != nil {
				m.runCancel()
			}
		}
		return m, nil
	}
	return m, nil
}

// applyProjection stores one loaded page projection. Only a narrowly
// classified lifecycle absence degrades to an empty state (the Execution
// Approval preview before specs/workflow compilation and the dry run); real
// projection errors remain visible and cannot acknowledge a command.
func (m Model) applyProjection(msg projectionMsg) (result Model, cmd tea.Cmd) {
	applied := false
	defer func() {
		kind := "projection_dropped"
		status := "stale_or_blocked"
		if applied {
			kind = "projection_applied"
			status = "accepted"
		}
		result.trace.write(operationLogEntry{
			OperationID:       msg.operationID,
			Workflow:          string(msg.workflow),
			Page:              pageName(msg.page),
			Kind:              kind,
			Generation:        msg.generation,
			CommandGeneration: msg.commandGeneration,
			View:              operationType(msg.view),
			Result:            status,
			ErrorCode:         operationErrorCode(msg.err),
		})
	}()
	// Execution Approval has a valid empty state before the specs, compiled
	// workflow, and dry-run facts exist. Normalize only that exact expected
	// absence into an empty view; all other projection errors remain errors and
	// must not acknowledge a completed command.
	if m.isExpectedEmptyProjectionError(msg.page, msg.workflow, msg.commandGeneration, msg.err) {
		msg.err = nil
		msg.view = app.ExecutionPreviewView{Workflow: msg.workflow}
	}
	acknowledgesCommand := m.commandState != nil &&
		m.commandState.inFlight &&
		msg.commandGeneration == m.commandState.pending &&
		msg.page == m.commandState.ackPage &&
		((msg.page == PageWorkspace && msg.workflow == "" && m.commandState.workflow == "") ||
			(msg.workflow != "" && m.commandState.workflow != "" && msg.workflow == m.commandState.workflow))
	createdWorkflowAcknowledged := acknowledgesCommand && msg.page == PageWorkspace && m.createAwaitingWorkspace
	// A query can finish after navigation or selection changed. Drop a
	// workflow-bound result that no longer belongs to the root selection; the
	// workspace fallback remains unbound and is allowed to normalize selection.
	if msg.generation != 0 && msg.generation < m.lastProjectionID && !acknowledgesCommand {
		return m, nil
	}
	if msg.workflow != "" && m.selected != "" && msg.workflow != m.selected && !acknowledgesCommand {
		return m, nil
	}
	if msg.page != PageWorkspace && msg.page != PageBlocked && m.page != msg.page && !acknowledgesCommand {
		return m, nil
	}
	if m.commandState != nil && m.commandState.inFlight &&
		msg.commandGeneration != 0 && msg.commandGeneration != m.commandState.pending {
		return m, nil
	}
	// A failed refresh is not an acknowledgement: the command gate remains
	// closed so stale legal actions cannot be issued against old facts. A
	// later matching successful projection can still release the gate.
	if msg.err != nil {
		if msg.page == PageWorkflowMenu {
			m.workflowMenu = app.WorkflowMenuView{Workflow: msg.workflow}
			m.workflowMenuModel = WorkflowMenuModel{Workflow: msg.workflow}
			m.workflowMenuError = msg.err.Error()
			m.status = "workflow menu: " + msg.err.Error()
			return m, nil
		}
		if acknowledgesCommand {
			return m.retryOrBlockCommandAcknowledgement(msg.page)
		}
		if msg.page == PageWorkspace {
			m.err = msg.err
			return m, nil
		}
		if msg.page == PageReadonlyWorkspace {
			m.readonly.Loaded = true
			m.readonly.Error = msg.err.Error()
			m.status = ""
			return m, nil
		}
		m.status = msg.err.Error()
		return m, nil
	}
	if acknowledgesCommand && !projectionViewMatches(msg.page, msg.view) {
		// A nil or wrong-typed successful query is not evidence that the
		// command's facts were refreshed. Keep the gate closed until the
		// expected projection type arrives.
		return m.retryOrBlockCommandAcknowledgement(msg.page)
	}
	if msg.commandGeneration != 0 && !acknowledgesCommand {
		return m, nil
	}
	if acknowledgesCommand && msg.workflow != "" && m.selected != "" && msg.workflow != m.selected {
		// The command completed for a workflow that is no longer selected.
		// Release the gate without publishing its status into the new
		// workflow's UI or applying the old projection to it.
		m.commandState.inFlight = false
		m.pendingProjectionStatus = ""
		if msg.generation > m.lastProjectionID {
			m.lastProjectionID = msg.generation
		}
		return m, nil
	}
	if acknowledgesCommand {
		m.commandState.inFlight = false
		if m.pendingProjectionStatus != "" {
			m.status = m.pendingProjectionStatus
			m.pendingProjectionStatus = ""
		}
	}
	if msg.page != PageWorkspace && msg.page != PageBlocked && m.page != msg.page {
		// The command's page projection is still a valid completion
		// acknowledgement even if the user navigated away before it arrived;
		// do not overwrite the page that is now visible.
		if msg.generation > m.lastProjectionID {
			m.lastProjectionID = msg.generation
		}
		return m, nil
	}
	if msg.generation > m.lastProjectionID {
		m.lastProjectionID = msg.generation
	}
	if msg.page == PageWorkspace && msg.commandGeneration == 0 && m.commandState != nil && !m.commandState.inFlight {
		if !m.projectionBlocked || (m.blockedAckPage == PageWorkspace && msg.workflow == m.blockedWorkflow) {
			m.projectionBlocked = false
		}
	}
	if m.projectionBlocked && msg.commandGeneration == 0 && msg.page == m.blockedAckPage &&
		msg.workflow != "" && msg.workflow == m.blockedWorkflow {
		m.projectionBlocked = false
	}
	switch msg.page {
	case PageWorkflowMenu:
		if v, ok := msg.view.(app.WorkflowMenuView); ok {
			m.workflowMenu = v
			m.workflowMenuModel = MapWorkflowMenu(v)
			m.workflowMenuIndex = m.workflowMenuModel.Selected
			m.workflowMenuError = ""
		}
	case PageReadonlyWorkspace:
		expectedWorkflow := m.selected
		if expectedWorkflow == "" {
			expectedWorkflow = m.workflowMenuModel.Workflow
		}
		if m.workflowMenuModel.Workflow != "" && expectedWorkflow != "" && m.workflowMenuModel.Workflow != expectedWorkflow {
			break
		}
		if readonlyProjectionMatches(m.readonly.Route, expectedWorkflow, msg.view) {
			m.readonly = MapReadonlyWorkspace(m.workflowMenuModel, m.readonly.Route, msg.view)
		}
	case PageWorkspace, PageBlocked, PageExecution, PageTerminal:
		if v, ok := msg.view.(app.WorkspaceView); ok {
			previousSelected := m.selected
			m.workspace = MapWorkspace(v)
			// Keep command routing aligned with the normalized ViewModel
			// selection when a projection refers to a workflow that disappeared.
			m.selected = m.workspace.Selected.ID
			if previousSelected != m.selected {
				m.resetSelectionState()
			}
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
			// Page queries run asynchronously. A slower query started
			// before CheckPlan may arrive after the fresh CHECKED
			// projection and must not roll the local approval state back.
			if v.Workflow == m.plan.Workflow &&
				v.AggregateVersion < m.plan.AggregateVersion {
				return m, nil
			}
			m.plan = v
			m.approval = ApprovalModel{Plan: v}
			if m.page == PagePlanApproval {
				if status, reached := planProjectionStatus(v, m.pendingPlanStatus); reached {
					m.status = status
					pending := m.pendingPlanStatus
					m.pendingPlanStatus = ""
					if pending == "plan checked" {
						m.planCheckInFlight = false
						if v.PlanStatus != model.PlanChecked {
							m.pendingPlanApproval = false
						}
					}
				}
			}
			if m.pendingPlanApproval && v.PlanStatus == model.PlanChecked &&
				v.Revision >= 1 && v.Hash != "" {
				m.pendingPlanApproval = false
				m.planCheckInFlight = false
				m.status = "approving the refreshed plan revision…"
				applied = true
				return m, m.executeCmd(app.ApprovePlanCommand{
					Workflow: m.selected, Revision: v.Revision, Hash: v.Hash,
				})
			}
			if m.pendingPlanCheck &&
				!m.commandInFlight() &&
				v.Stage == model.StagePlanCheck &&
				v.Revision >= 1 && v.Hash != "" {
				operationID := m.pendingPlanCheckOperation
				m.pendingPlanCheck = false
				m.pendingPlanCheckOperation = ""
				m.status = "running the independent plan check…"
				applied = true
				return m, m.executeCmdWithOperation(m.context(), app.CheckPlanCommand{
					Workflow: m.selected, Provider: m.discussionProvider(),
				}, operationID)
			}
			if v.PlanStatus == model.PlanChecked {
				m.planCheckInFlight = false
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
	if createdWorkflowAcknowledged {
		applied = true
		m.createAwaitingWorkspace = false
		// Creation replaces the transient Create route with a fresh Home root;
		// the new Workflow Menu is its child. This keeps Esc synchronized with
		// the visible page and prevents a successful create from returning to
		// the obsolete name editor.
		m = m.resetNavigationHome()
		m = m.enterWorkflowMenu()
		m.workflowMenu = app.WorkflowMenuView{Workflow: m.selected}
		m.workflowMenuModel = WorkflowMenuModel{Workflow: m.selected}
		m.workflowMenuIndex = 0
		m.workflowMenuError = ""
		return m, m.queryCmd(PageWorkflowMenu, app.WorkflowMenuQuery{Workflow: m.selected})
	}
	applied = true
	return m, nil
}

// retryOrBlockCommandAcknowledgement gives a failed command projection a
// bounded recovery path. After the bound, mutation stays disabled until a
// successful workspace projection rebuilds fresh facts; this avoids both a
// permanent in-flight wedge and issuing a new command from stale facts.
func (m Model) retryOrBlockCommandAcknowledgement(page Page) (Model, tea.Cmd) {
	if m.commandState == nil {
		return m, nil
	}
	if m.commandState.ackRetries < 3 {
		m.commandState.ackRetries++
		workflow := m.commandState.workflow
		if page == PageWorkspace {
			return m, m.commandQueryCmd(PageWorkspace, app.ProjectWorkspaceQuery{Selected: workflow}, m.commandState.pending)
		}
		if q, ok := pageQuery(page, workflow); ok {
			return m, m.commandQueryCmd(page, q, m.commandState.pending)
		}
	}
	m.commandState.inFlight = false
	m.projectionBlocked = true
	m.blockedAckPage = m.commandState.ackPage
	m.blockedWorkflow = m.commandState.workflow
	m.pendingProjectionStatus = ""
	m.status = "projection refresh failed; refreshing facts…"
	return m, m.queryCmd(PageWorkspace, app.ProjectWorkspaceQuery{Selected: m.commandState.workflow})
}

// reloadCmd reloads the workspace projection and the current page's
// projection after one command changed the Runtime facts.
func commandAckPage(page Page) Page {
	// Execution, Blocked, and Terminal render facts from the Workspace
	// projection. Their commands must therefore be acknowledged by that
	// projection rather than by a page query that reloadCmd does not issue.
	switch page {
	case PageCreate, PagePauseExit, PageActionPreview, PageExecution, PageBlocked, PageTerminal:
		return PageWorkspace
	default:
		return page
	}
}

func (m Model) reloadCmd() tea.Cmd {
	var cmds []tea.Cmd
	workspaceWorkflow := m.selected
	if m.commandState != nil && (m.commandState.inFlight || m.projectionBlocked) && m.commandState.ackPage == PageWorkspace {
		workspaceWorkflow = m.commandState.workflow
		if m.commandState.inFlight {
			cmds = append(cmds, m.commandQueryCmd(PageWorkspace, app.ProjectWorkspaceQuery{Selected: workspaceWorkflow}, m.commandState.pending))
		} else {
			cmds = append(cmds, m.queryCmd(PageWorkspace, app.ProjectWorkspaceQuery{Selected: workspaceWorkflow}))
		}
	} else {
		cmds = append(cmds, m.queryCmd(PageWorkspace, app.ProjectWorkspaceQuery{Selected: workspaceWorkflow}))
	}
	if q, ok := pageQuery(m.page, m.selected); ok {
		cmds = append(cmds, m.reloadQueryCmd(m.page, q))
	}
	// Navigation is allowed while a mutation is in flight. If it moved away
	// from the command's origin page, reload that page as an explicit command
	// acknowledgement as well; the current page's ordinary query cannot
	// release the mutation gate.
	if m.commandState != nil && (m.commandState.inFlight || m.projectionBlocked) &&
		m.commandState.ackPage != PageWorkspace && m.page != m.commandState.ackPage {
		if q, ok := pageQuery(m.commandState.ackPage, m.commandState.workflow); ok {
			if m.commandState.inFlight {
				cmds = append(cmds, m.commandQueryCmd(m.commandState.ackPage, q, m.commandState.pending))
			} else {
				cmds = append(cmds, m.queryCmd(m.commandState.ackPage, q))
			}
		}
	}
	return tea.Batch(cmds...)
}

func pageQuery(page Page, workflow model.WorkflowID) (app.Query, bool) {
	switch page {
	case PageDiscussion:
		return app.DiscussionReturnQuery{Workflow: workflow}, true
	case PagePlanApproval:
		return app.PlanQuery{Workflow: workflow}, true
	case PageExecutionApproval:
		return app.ExecutionPreviewQuery{Workflow: workflow}, true
	case PageCancel:
		return app.CancelSummaryQuery{Workflow: workflow}, true
	case PageMigration:
		return app.LayoutMigrationPreviewQuery{Workflow: workflow}, true
	default:
		return nil, false
	}
}

func (m Model) reloadQueryCmd(page Page, q app.Query) tea.Cmd {
	if m.commandState != nil && m.commandState.inFlight && page == m.commandState.ackPage {
		return m.commandQueryCmd(page, q, m.commandState.pending)
	}
	return m.queryCmd(page, q)
}

func (m *Model) awaitProjectionStatus(status string) {
	m.pendingProjectionStatus = status
	m.status = "refreshing updated facts…"
}

// applyCommand handles one finished typed Application Command.
func (m Model) applyCommand(msg commandDoneMsg) (Model, tea.Cmd) {
	if m.commandState != nil && msg.generation != m.commandState.pending {
		// A stop command may legitimately overtake an older mutating command.
		// Its completion still settles the stop join, but must not apply stale
		// outcome handling or clear the newer command's gate.
		if _, ok := msg.cmd.(app.PauseWorkflowCommand); ok {
			m.pauseCommandPending = false
			if m.stop == stopPauseAndExit {
				// /exit requested a controlled pause, but the Application
				// rejected it. Force-stop managed processes, then keep the
				// TUI on a diagnosable recovery page instead of quitting while
				// the stop outcome is ambiguous.
				m.forceStop()
				m.quitAfterRunner = false
				m.stop = stopIdle
				m.page = PagePauseExit
				m.status = fmt.Sprintf("%v", msg.err)
				m.projectionBlocked = false
				m.blockedAckPage = PageWorkspace
				m.blockedWorkflow = ""
				return m, nil
			}
		}
		if _, ok := msg.cmd.(app.ResumeWorkflowCommand); ok {
			m.resumeThenRun = false
		}
		return m, nil
	}
	if msg.err != nil {
		if m.commandState != nil {
			m.commandState.inFlight = false
			m.projectionBlocked = true
			m.blockedAckPage = m.commandState.ackPage
			m.blockedWorkflow = m.commandState.workflow
		}
		m.pendingProjectionStatus = ""
		if _, ok := msg.cmd.(app.PauseWorkflowCommand); ok {
			m.pauseCommandPending = false
			if m.stop == stopPauseAndExit {
				// /exit requested a controlled pause, but the Application
				// rejected it. Force-stop managed processes, then keep the
				// TUI on a diagnosable recovery page instead of quitting while
				// the stop outcome is ambiguous.
				m.forceStop()
				m.quitAfterRunner = false
				m.stop = stopIdle
				m.page = PagePauseExit
				m.status = fmt.Sprintf("%v", msg.err)
				m.projectionBlocked = false
				m.blockedAckPage = PageWorkspace
				m.blockedWorkflow = ""
				return m, nil
			}
			if m.quitAfterRunner {
				// A forced stop can finish the Runner before the controlled
				// Pause command reports its failure. Preserve the second-Ctrl+C
				// quit request and join whichever side is still unwinding.
				return m.quit()
			}
		}
		if _, ok := msg.cmd.(app.CheckPlanCommand); ok {
			m.planCheckInFlight = false
			m.pendingPlanApproval = false
			m.pendingPlanCheck = false
			m.pendingPlanCheckOperation = ""
		}
		m.status = fmt.Sprintf("%v", msg.err)
		if resume, ok := msg.cmd.(app.ResumeWorkflowCommand); ok && m.resumeThenRun {
			// The Execution page requested the resume as the run start but
			// the Runtime rejected it (the stale projection can still show
			// Resume right after an execution approval while the workflow is
			// already RUNNING). Clear the pending resume and fall back to
			// starting the Foreground Runner directly: DriveOnce is a safe
			// bounded step that stops when it cannot progress. Without this
			// the pending resume-then-run would stay dangling and a later
			// successful resume would double-start the runner.
			m.resumeThenRun = false
			if resume.Workflow == m.selected {
				m.projectionBlocked = false
				return m.startRunner()
			}
			return m, nil
		}
		return m, m.reloadCmd()
	}
	switch msg.cmd.(type) {
	case app.CreateWorkflowCommand:
		m.selected = msg.out.Workflow
		m.createAwaitingWorkspace = msg.out.Workflow != ""
		if m.commandState != nil && msg.out.Workflow != "" {
			// Creation changes the selected Workflow identity. Bind the
			// acknowledgement refresh to the newly created aggregate rather
			// than the pre-existing selection captured at key time.
			m.commandState.workflow = msg.out.Workflow
		}
		m.awaitProjectionStatus("workflow created")
		m.page = PageWorkspace
		m.createDirty = nil
		m.createConfirm = false
		return m, m.reloadCmd()
	case app.PauseWorkflowCommand:
		m.pauseCommandPending = false
		if m.stop == stopPauseAndExit || m.quitAfterRunner {
			// The pause command and Runner completion are both required before
			// exiting. quit() joins the Runner when it is still unwinding.
			return m.quit()
		}
		m.status = "workflow paused (controlled stop)"
		m.pendingDecision = ""
		return m, m.reloadCmd()
	case app.ResumeWorkflowCommand:
		resume := msg.cmd.(app.ResumeWorkflowCommand)
		m.awaitProjectionStatus("workflow resumed")
		m.pendingDecision = ""
		if m.resumeThenRun {
			// The Execution page requested the resume as the run start.
			m.resumeThenRun = false
			if resume.Workflow == m.selected {
				m, run := m.startRunner()
				return m, tea.Batch(m.reloadCmd(), run)
			}
		}
		return m, m.reloadCmd()
	case app.CancelWorkflowCommand:
		m.awaitProjectionStatus("workflow cancelled")
		m, _ = m.popNavigation()
		return m, m.reloadCmd()
	case app.AdoptWorkspaceCommand:
		m.awaitProjectionStatus("workspace adopted")
		m.pendingDecision = ""
		return m, m.reloadCmd()
	case app.PrepareNativeDiscussionCommand:
		if msg.out.Native != nil {
			m.status = "starting the native discussion terminal…"
			return m, newNativeExecCmd(m.context(), msg.out.Native)
		}
		m.status = "native discussion prepared"
		return m, m.reloadCmd()
	case app.ContinueNativeDiscussionCommand:
		if msg.out.Native != nil {
			m.status = "continuing the native discussion terminal…"
			return m, newNativeExecCmd(m.context(), msg.out.Native)
		}
		// The outcome carried no Bridge request (a re-armed Session without
		// recoverable binding facts): fall back to the plain projection reload
		// instead of launching a terminal that would run no supervised process.
		return m, m.reloadCmd()
	case app.SwitchAgentCommand:
		if msg.out.Native != nil {
			m.status = "starting the switched native discussion terminal…"
			return m, newNativeExecCmd(m.context(), msg.out.Native)
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
		m.awaitProjectionStatus("discussion finished")
		return m, m.reloadCmd()
	case app.GeneratePlanCommand:
		m.planCheckInFlight = false
		m.pendingPlanApproval = false
		if finding := latestFinding(msg.out.Findings, model.CodeSchemaInvalid); finding != nil {
			m.pendingPlanStatus = ""
			m.status = "plan output rejected: " + finding.Text
			return m, m.reloadCmd()
		}
		m.pendingPlanStatus = "plan generated"
		m.status = "plan generation finished; refreshing plan projection…"
		return m, m.reloadCmd()
	case app.CheckPlanCommand:
		m.pendingPlanCheck = false
		m.pendingPlanCheckOperation = ""
		m.planCheckInFlight = true
		m.pendingPlanStatus = "plan checked"
		m.status = "plan check finished; refreshing plan projection…"
		return m, m.reloadCmd()
	case app.ApprovePlanCommand:
		m.pendingPlanStatus = "plan approved"
		m.status = "plan approval finished; refreshing plan projection…"
		return m, m.reloadCmd()
	case app.GenerateSpecsCommand:
		m.awaitProjectionStatus("specs generated")
		return m, m.reloadCmd()
	case app.CompileWorkflowCommand:
		m.awaitProjectionStatus("workflow compiled")
		return m, m.reloadCmd()
	case app.ExecutionDryRunCommand:
		m.awaitProjectionStatus("execution dry run complete")
		return m, m.reloadCmd()
	case app.ApproveExecutionCommand:
		m.awaitProjectionStatus("execution approved")
		m, _ = m.popNavigation()
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageExecution, Workflow: m.selected})
		return m, m.reloadCmd()
	case app.PrepareApplyCommand:
		if msg.out.Apply != nil {
			attempt := *msg.out.Apply
			m.terminal.applyAttempt = &attempt
			m.terminal.ApplyPreview = renderApplyAttempt(*msg.out.Apply)
			m.awaitProjectionStatus("apply staged (preview ready)")
		}
		return m, m.reloadCmd()
	case app.ExecuteApplyCommand:
		m.awaitProjectionStatus("apply delivered to the target branch")
		m.terminal.Confirmed = false
		m.terminal.Previewed = false
		return m, m.reloadCmd()
	case app.DryRunCommand:
		if msg.out.Cleanup != nil {
			m.terminal.CleanupPreview = renderCleanupAttempt(*msg.out.Cleanup)
			ref := msg.out.Cleanup.Manifest
			m.terminal.cleanupRef = &ref
			m.awaitProjectionStatus("cleanup dry run manifest ready")
		}
		return m, m.reloadCmd()
	case app.ExecuteCleanupCommand:
		m.awaitProjectionStatus("cleanup executed")
		m.terminal.Confirmed = false
		m.terminal.Previewed = false
		return m, m.reloadCmd()
	case app.PrepareLayoutMigrationCommand:
		m.migrationConfirm = migrationConfirmNone
		m.status = "layout migration prepared; immutable intent persisted"
		return m, m.reloadCmd()
	case app.ExecuteLayoutMigrationCommand:
		m.migrationConfirm = migrationConfirmNone
		m.migrationPreviewed = false
		m.awaitProjectionStatus("legacy layout migrated")
		m, _ = m.popNavigation()
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
	// stopPauseAndExit means the confirmation page is visible; it becomes an
	// actual exit request only after its Enter action arms the pause command. This
	// avoids quitting merely because a Runner reaches a terminal result while
	// the user is still deciding.
	requestedQuit := m.quitAfterRunner || (m.stop == stopPauseAndExit && m.pauseCommandPending)
	if msg.err != nil {
		m.err = msg.err
		if requestedQuit && !m.pauseCommandPending {
			m.quitAfterRunner = false
			m.stop = stopIdle
			return m, tea.Quit
		}
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
	if requestedQuit {
		if m.pauseCommandPending {
			// Keep quitAfterRunner set for the second-Ctrl+C path; the pause
			// completion will perform the final quit. stopPauseAndExit is
			// likewise retained until its confirmation command completes.
			return m, nil
		}
		// The runner and the requested pause are both complete.
		m.quitAfterRunner = false
		m.stop = stopIdle
		return m, tea.Quit
	}
	m.quitAfterRunner = false
	return m, m.reloadCmd()
}

// handleKey routes one key press through the controlled-stop protocol and
// the active page.
func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	// Ctrl+C retains the controlled-stop protocol globally. Task 7 adds the
	// only normal exit path through /exit.
	if IsCtrlC(msg) {
		switch m.stop {
		case stopIdle:
			m.stop = stopFirstCtrlC
			var cmds []tea.Cmd
			workflow := m.activeRunnerWorkflow()
			if workflow != "" {
				// The first Ctrl+C requests the controlled Pause: the
				// typed command closes dispatch and stops the managed
				// processes, and the run context is cancelled so the real
				// Runner stops (never just a local flag).
				if m.runCancel != nil {
					m.runCancel()
				}
				m.pauseCommandPending = true
				cmds = append(cmds, m.executeCmd(app.PauseWorkflowCommand{Workflow: workflow}))
			}
			return m, tea.Batch(cmds...)
		default:
			// The second Ctrl+C is the Force Stop of the active Runner
			// (and of any running controlled stop).
			m.forceStop()
			return m.quit()
		}
	}
	// An open palette owns every ordinary key. The only exception is Ctrl+C,
	// which remains the process-wide controlled-stop signal above.
	if m.commandPalette.Open {
		var event CommandPaletteEvent
		m.commandPalette, event = m.commandPalette.Update(msg)
		switch event {
		case CommandPaletteExit:
			m.traceUIAction(uiActionCommandPaletteExecute)
			return m.handleGlobalExit()
		case CommandPaletteClose, CommandPaletteNone:
			return m, nil
		}
	}
	// Slash is global only when the current page does not own text input. It
	// is consumed here so no page handler sees the opener a second time.
	if IsSlash(msg) && !m.typingText() {
		m.traceUIAction(uiActionCommandPaletteOpen)
		m.commandPalette = NewCommandPalette()
		m.commandPalette.Open = true
		m.commandPalette.Input = "/"
		return m, nil
	}
	if m.commandInFlight() && !m.typingText() &&
		!(m.page == PagePlanApproval &&
			(msg.Code == 'k' || msg.Code == 'K') &&
			m.pendingPlanStatus == "plan generated") &&
		blocksWhileCommandInFlight(msg) {
		m.status = "command in progress; waiting for refreshed facts"
		return m, nil
	}
	// Create Workspace is a text input, so it bypasses the general
	// non-typing Esc guard above. It still follows the NavigationStack: Esc
	// leaves the nested Create frame and restores Home instead of assigning a
	// page directly.
	if msg.Code == tea.KeyEsc && !m.createConfirm && m.page == PageCreate && m.navigation.Current().Page == PageCreate {
		m.createInput = ""
		m.createConfirm = false
		m.createDirty = nil
		m, _ = m.popNavigation()
		return m, nil
	}
	if msg.Code == tea.KeyEsc && !m.typingText() && m.navigation.Current().Page == m.page {
		if m.navigation.Current().Layer == LayerHome {
			return m, nil
		}
		m, _ = m.popNavigation()
		if m.page == PageWorkspace {
			m.restoreHomeWorkflowRow()
		}
		return m, nil
	}
	if m.navigationIsNested() && msg.Code == tea.KeyTab {
		return m, nil
	}
	if (msg.Code == 'b' || msg.Code == 'B') && !m.typingText() {
		return m.resetNavigationHome(), nil
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
	case PageWorkflowMenu:
		switch {
		case IsUp(msg):
			return m.moveWorkflowMenuSelection(-1), nil
		case IsDown(msg):
			return m.moveWorkflowMenuSelection(1), nil
		case IsEnter(msg):
			item, ok := m.selectedWorkflowMenuItem()
			if !ok {
				return m, nil
			}
			m.traceUIAction(uiActionWorkflowMenuSelect)
			if item.Action == app.MenuActionCancel {
				m.cancelPreview = false
			}
			if item.Action == app.MenuActionMigrate {
				m.migrationPreviewed = false
			}
			if item.Action == app.MenuActionResume || item.Action == app.MenuActionPause || item.Action == app.MenuActionStartRunner {
				m.actionPreviewed = false
			}
			return m.routeWorkflowMenuItem(item)
		}
		return m, nil
	case PageReadonlyWorkspace:
		switch {
		case IsUp(msg):
			return moveReadonlySelection(m, -1), nil
		case IsDown(msg):
			return moveReadonlySelection(m, 1), nil
		}
		return m, nil
	case PageActionPreview, PageCreatePreview:
		if m.page == PageActionPreview {
			return m.handleActionPreviewKey(msg)
		}
		return m, nil
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

// handleGlobalExit translates the sole global command into either immediate
// TUI exit or the existing controlled Pause-and-Exit preview.
func (m Model) handleGlobalExit() (Model, tea.Cmd) {
	if m.commandInFlight() || m.pauseCommandPending || m.quitAfterRunner || m.stop != stopIdle {
		m.status = "cannot exit while a managed command or stop is still in progress"
		return m, nil
	}
	if m.running {
		m.prevPage = m.page
		m.page = PagePauseExit
		m.stop = stopIdle
		return m, nil
	}
	return m, tea.Quit
}

// typingText reports whether the active page is a text input.
func (m Model) typingText() bool {
	switch m.page {
	case PageCreate:
		return !m.commandInFlight()
	case PageDiscussion:
		return m.discussion.Editing
	}
	return false
}

func (m Model) activeRunnerWorkflow() model.WorkflowID {
	if (m.running || m.pauseCommandPending || m.quitAfterRunner) && m.runnerWorkflow != "" {
		return m.runnerWorkflow
	}
	return m.selected
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

// quit requests TUI exit. If a Foreground Runner is active, Bubble Tea must
// remain alive until runnerDoneMsg proves its goroutine and event subscription
// have unwound; cancellation alone is not a join.
func (m Model) quit() (Model, tea.Cmd) {
	if m.running {
		m.quitAfterRunner = true
		return m, nil
	}
	m.clearRunner()
	return m, tea.Quit
}

// resetSelectionState clears UI facts that are bound to the selected workflow.
// It is shared by explicit navigation and stale-selection recovery.
func (m *Model) resetSelectionState() {
	m.provider = ""
	m.discussion = DiscussionPage{}
	m.approval = ApprovalModel{}
	m.plan = app.PlanView{}
	m.preview = app.ExecutionPreviewView{}
	m.readonly = ReadonlyWorkspaceModel{}
	m.execution = NewExecutionModel("")
	m.terminal = NewTerminalModel()
	m.createDirty = nil
	m.pendingPlanStatus = ""
	m.pendingPlanApproval = false
	m.pendingPlanCheck = false
	m.pendingPlanCheckOperation = ""
	m.planCheckInFlight = false
	m.migration = app.MigrationPreviewView{}
	m.migrationConfirm = migrationConfirmNone
	m.migrationPreviewed = false
	m.cancelPreview = false
	m.actionPreviewed = false
	m.pendingDecision = ""
	m.pendingProjectionStatus = ""
	m.resumeThenRun = false
	m.projectionBlocked = false
	m.blockedAckPage = PageWorkspace
	m.blockedWorkflow = ""
}

func (m Model) commandInFlight() bool {
	return m.commandState != nil && (m.commandState.inFlight || m.projectionBlocked)
}

func (m Model) context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func blocksWhileCommandInFlight(msg tea.KeyPressMsg) bool {
	// Navigation is read-only and remains available while the command's
	// refreshed projection is in flight. Mutating page actions stay gated so a
	// key cannot issue a second Application command against stale facts.
	switch msg.Code {
	case tea.KeyTab, tea.KeyUp, tea.KeyDown, tea.KeyLeft, tea.KeyRight, tea.KeyEsc, 'b', 'B':
		return false
	}
	return !IsCtrlC(msg)
}

func (m Model) nextProjectionID() uint64 {
	if m.commandState == nil {
		return 0
	}
	m.commandState.projectionID++
	return m.commandState.projectionID
}

func (m Model) beginOperation(action string) string {
	if m.commandState == nil {
		return ""
	}
	m.commandState.operationSeq++
	operationID := fmt.Sprintf("op-%d", m.commandState.operationSeq)
	m.trace.write(operationLogEntry{
		OperationID: operationID,
		Workflow:    string(m.selected),
		Page:        pageName(m.page),
		Kind:        "user_action",
		Action:      action,
	})
	return operationID
}

// executeCmd runs one typed Application Command and delivers its Outcome.
func (m Model) executeCmd(cmd app.Command) tea.Cmd {
	return m.executeCmdWithOperation(m.context(), cmd, m.beginOperation("command."+operationType(cmd)))
}

func (m Model) executeCmdWithContext(ctx context.Context, cmd app.Command) tea.Cmd {
	return m.executeCmdWithOperation(ctx, cmd, m.beginOperation("command."+operationType(cmd)))
}

func (m Model) executeCmdWithOperation(ctx context.Context, cmd app.Command, operationID string) tea.Cmd {
	generation := uint64(0)
	if m.commandState != nil {
		m.commandState.ackPage = commandAckPage(m.page)
		m.commandState.workflow = m.selected
		m.commandState.operationID = operationID
		m.commandState.generation++
		generation = m.commandState.generation
		m.commandState.pending = generation
		m.commandState.inFlight = true
		m.commandState.ackRetries = 0
		m.projectionBlocked = false
		m.blockedAckPage = PageWorkspace
		m.blockedWorkflow = ""
	}
	m.trace.write(operationLogEntry{
		OperationID: operationID,
		Workflow:    string(m.selected),
		Page:        pageName(m.page),
		Kind:        "command_started",
		Command:     operationType(cmd),
		Generation:  generation,
	})
	return func() tea.Msg {
		out, err := m.ctrl.Execute(ctx, cmd)
		m.trace.write(operationLogEntry{
			OperationID: operationID,
			Workflow:    string(m.selected),
			Page:        pageName(m.page),
			Kind:        "command_result",
			Command:     operationType(cmd),
			Generation:  generation,
			Result:      operationResult(err),
			ErrorCode:   operationErrorCode(err),
		})
		return commandDoneMsg{
			cmd:         cmd,
			generation:  generation,
			operationID: operationID,
			out:         out,
			err:         err,
		}
	}
}

// ---------------------------------------------------------------------------
// page key handlers
// ---------------------------------------------------------------------------

// handlePauseExitKey handles the Pause and Exit confirmation.
func (m Model) handlePauseExitKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case IsEnter(msg):
		m.stop = stopPauseAndExit
		if m.runCancel != nil {
			m.runCancel()
		}
		workflow := m.activeRunnerWorkflow()
		if workflow != "" {
			m.pauseCommandPending = true
			return m, m.executeCmd(app.PauseWorkflowCommand{Workflow: workflow})
		}
		m.forceStop()
		return m.quit()
	case msg.Code == tea.KeyEsc:
		m.stop = stopIdle
		m.page = m.prevPage
		return m, nil
	}
	return m, nil
}

// handleCreateKey handles the Create Workflow form (Task 6): the TUI
// first queries and displays the target's dirty state, fingerprint, and
// isolation; Enter submits the name for the Create Preview, and Enter on
// that preview issues the typed CreateWorkflowCommand, carrying
// ConfirmDirty: true exactly when the queried target is dirty.
func (m Model) handleCreateKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.createConfirm {
		// The Create Preview accepts only Enter. Esc returns to editing while
		// preserving the name; y/Y/n/N/q are ordinary, non-confirming input.
		switch {
		case IsEnter(msg):
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
		case msg.Code == tea.KeyEsc:
			m.createConfirm = false
			m.status = ""
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
		// Enter submits the name for the Create Preview; it never creates.
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

// handleWorkspaceKey handles Home selection and routing. New Workflow is a
// selectable UI row; Home has no standalone mutation shortcut and never
// mutates Runtime state.
func (m Model) handleWorkspaceKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case IsUp(msg):
		return m.moveSelection(-1)
	case IsDown(msg):
		return m.moveSelection(1)
	case IsEnter(msg):
		if m.selected == "" {
			m = m.pushNavigation(NavigationFrame{Layer: LayerCreateWorkspace, Page: PageCreate})
			m.createInput = ""
			m.createConfirm = false
			m.createDirty = nil
			return m, m.queryCmd(PageCreate, app.DiscoveryQuery{})
		}
		m = m.enterWorkflowMenu()
		m.workflowMenu = app.WorkflowMenuView{Workflow: m.selected}
		m.workflowMenuModel = WorkflowMenuModel{Workflow: m.selected}
		m.workflowMenuIndex = 0
		m.workflowMenuError = ""
		return m, m.queryCmd(PageWorkflowMenu, app.WorkflowMenuQuery{Workflow: m.selected})
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

func latestFinding(findings []model.Finding, code model.Code) *model.Finding {
	for i := len(findings) - 1; i >= 0; i-- {
		if findings[i].Code == code {
			return &findings[i]
		}
	}
	return nil
}

func planProjectionStatus(v app.PlanView, pending string) (string, bool) {
	switch pending {
	case "plan generated":
		return "plan generated", v.Stage == model.StagePlanCheck && v.Revision >= 1 && v.Hash != ""
	case "plan checked":
		switch {
		case v.PlanStatus == model.PlanChecked:
			return "plan checked", true
		case v.Stage == model.StageRequirementDiscussion && v.PlanStatus == model.PlanDraft:
			return "plan check needs discussion", true
		case v.Stage == model.StagePlanGeneration && v.PlanStatus == model.PlanDraft:
			return "plan check needs revision", true
		case v.PlanStatus == model.PlanRejected:
			return "plan rejected", true
		default:
			return "", false
		}
	case "plan approved":
		return "plan approved", v.PlanStatus == model.PlanApproved
	default:
		return "", false
	}
}

// handleMigrationKey owns the explicit TUI Preview -> Prepare -> Execute
// protocol. The selected action must be previewed with Enter and the next
// Enter issues the typed command bound to the displayed manifest hash.
func (m Model) handleMigrationKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.migrationConfirm != migrationConfirmNone {
		switch {
		case IsEnter(msg):
			if !m.migrationPreviewed {
				m.migrationPreviewed = true
				m.status = "layout migration preview ready; press Enter to execute"
				return m, nil
			}
			confirm := m.migrationConfirm
			m.migrationConfirm = migrationConfirmNone
			m.migrationPreviewed = false
			if confirm == migrationConfirmPrepare {
				return m, m.executeCmd(app.PrepareLayoutMigrationCommand{Workflow: m.selected, ManifestHash: m.migration.ManifestHash})
			}
			return m, m.executeCmd(app.ExecuteLayoutMigrationCommand{Workflow: m.selected, ManifestHash: m.migration.ManifestHash})
		}
		return m, nil
	}
	switch {
	case msg.Code == 'p' || msg.Code == 'P':
		m.migrationConfirm = migrationConfirmPrepare
		m.migrationPreviewed = false
		return m, nil
	case msg.Code == 'e' || msg.Code == 'E':
		m.migrationConfirm = migrationConfirmExecute
		m.migrationPreviewed = false
		return m, nil
	}
	return m, nil
}

// moveSelection moves the Home selection by one row. The UI-only New row
// clears workflow-bound presentation facts without querying or mutating the
// Application. Existing rows reload their authoritative Workspace projection.
func (m Model) moveSelection(delta int) (Model, tea.Cmd) {
	if m.running {
		m.status = "stop the active runner before changing workflow"
		return m, nil
	}
	if len(m.workspace.Rows) == 0 {
		return m, nil
	}
	idx := 0
	for i, row := range m.workspace.Rows {
		if row.Kind == WorkflowRowExisting && row.ID == m.selected {
			idx = i
			break
		}
	}
	idx += delta
	if idx < 0 {
		idx = len(m.workspace.Rows) - 1
	}
	if idx >= len(m.workspace.Rows) {
		idx = 0
	}
	row := m.workspace.Rows[idx]
	if row.Kind == WorkflowRowNew {
		m.selected = ""
		m.workspace.SelectedRow = idx
		m.workspace.Selected = WorkflowItem{}
		m.workspace.Lifecycle = nil
		m.workspace.Actions = nil
		m.resetSelectionState()
		return m, nil
	}
	m.selected = row.ID
	m.workspace.SelectedRow = idx
	m.resetSelectionState()
	return m, m.queryCmd(PageWorkspace, app.ProjectWorkspaceQuery{Selected: m.selected})
}

// queryCmd loads one page projection through a read-only Query.
func (m Model) queryCmd(page Page, q app.Query) tea.Cmd {
	return m.queryCmdWithCommandGeneration(page, q, 0)
}

func (m Model) commandQueryCmd(page Page, q app.Query, commandGeneration uint64) tea.Cmd {
	return m.queryCmdWithCommandGeneration(page, q, commandGeneration)
}

func (m Model) queryCmdWithCommandGeneration(page Page, q app.Query, commandGeneration uint64) tea.Cmd {
	generation := m.nextProjectionID()
	return func() tea.Msg {
		return m.queryProjectionMsgAt(page, q, generation, commandGeneration)
	}
}

// handleDiscussionKey handles the native Discussion Return Page.
func (m Model) handleDiscussionKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.discussion.Editing {
		return m.handleHandoffKey(msg)
	}
	switch {
	case msg.Code == tea.KeyEsc:
		return m, nil
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
		return m.openStageActionPreview(app.MenuActionStartDiscussion, "Start Native Discussion")
	case ReturnContinue:
		return m.openStageActionPreview(app.MenuActionContinueDiscussion, "Continue Same Session")
	case ReturnFinish:
		return m.openStageActionPreview(app.MenuActionFinishDiscussion, "Finish Discussion")
	case ReturnSwitch:
		return m.openStageActionPreview(app.MenuActionSwitchDiscussion, "Switch Discussion Agent")
	case ReturnPause:
		return m.openStageActionPreview(app.MenuActionPause, "Pause Workflow")
	case ReturnCancel:
		m.cancelPreview = false
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageCancel, Workflow: m.selected})
		return m, m.queryCmd(PageCancel, app.CancelSummaryQuery{Workflow: m.selected})
	}
	return m, nil
}

func (m Model) openStageActionPreview(action app.MenuAction, label string) (Model, tea.Cmd) {
	m.workflowMenuPreviewItem = MenuItem{Action: action, Label: label, SourceIndex: m.workflowMenuIndex}
	m.actionPreviewed = false
	m.traceUIAction(uiActionActionPreviewOpen)
	return m.pushNavigation(NavigationFrame{Layer: LayerActionPreview, Page: PageActionPreview, Workflow: m.selected}), nil
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
	command, err := m.switchAgentCommand()
	if err != nil {
		return func() tea.Msg { return commandDoneMsg{err: err} }
	}
	return m.executeCmd(command)
}

func (m Model) switchAgentCommand() (app.SwitchAgentCommand, error) {
	current := m.discussion.Provider
	alt := ""
	for _, p := range m.workspace.Health.Providers {
		if p.Compatible && p.Name != current {
			alt = p.Name
			break
		}
	}
	if alt == "" {
		return app.SwitchAgentCommand{}, fmt.Errorf("no different provider is available to switch to")
	}
	reason := m.discussion.SwitchReason
	if strings.TrimSpace(reason) == "" {
		reason = "user switched the discussion agent"
	}
	return app.SwitchAgentCommand{
		Workflow: m.selected,
		Session:  model.SessionID(m.discussion.Session),
		Provider: alt,
		Reason:   reason,
	}, nil
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
		m.discussion.Handoff = string(content)
		return m.openStageActionPreview(app.MenuActionFinishDiscussion, "Finish Discussion")
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
// new Plan Revision, 'k' runs the independent check, and the second Enter
// after the displayed preview issues ApprovePlanCommand.
func (m Model) handlePlanApprovalKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.selected == "" {
		m.status = "no workflow selected"
		return m, nil
	}
	switch {
	case msg.Code == 'g' || msg.Code == 'G':
		m.planCheckInFlight = false
		m.pendingPlanApproval = false
		m.pendingPlanCheck = false
		m.pendingPlanCheckOperation = ""
		m.status = "generating a plan revision…"
		operationID := m.beginOperation("plan_generate")
		return m, m.executeCmdWithOperation(m.context(), app.GeneratePlanCommand{
			Workflow: m.selected, Provider: m.discussionProvider(),
		}, operationID)
	case msg.Code == 'k' || msg.Code == 'K':
		// GeneratePlan settles before the asynchronous page projection
		// reload is applied. Refuse a stale check request locally instead
		// of sending CheckPlan against the still-visible PLAN_GENERATION
		// stage.
		if m.plan.Stage != model.StagePlanCheck || m.plan.Revision < 1 || m.plan.Hash == "" {
			if m.pendingPlanStatus == "plan generated" ||
				(m.commandState != nil && m.commandState.inFlight) {
				m.pendingPlanCheck = true
				m.pendingPlanCheckOperation = m.beginOperation("plan_check")
				m.status = "plan check queued; waiting for refreshed PLAN_CHECK"
				return m, nil
			}
			m.status = "plan projection is still refreshing; wait for PLAN_CHECK"
			return m, nil
		}
		m.status = "running the independent plan check…"
		operationID := m.beginOperation("plan_check")
		return m, m.executeCmdWithOperation(m.context(), app.CheckPlanCommand{
			Workflow: m.selected, Provider: m.discussionProvider(),
		}, operationID)
	case msg.Code == tea.KeyEsc:
		return m, nil
	}
	upd, cmd := m.approval.Update(msg)
	m.approval = upd
	if m.approval.Confirmed {
		if m.plan.PlanStatus != model.PlanChecked || m.plan.Revision < 1 || m.plan.Hash == "" {
			if m.planCheckInFlight || m.pendingPlanStatus == "plan checked" {
				m.pendingPlanApproval = true
				m.approval.Confirmed = false
				m.status = "plan approval queued; waiting for refreshed PLAN_CHECK"
				return m, nil
			}
			m.status = "no plan revision to approve"
			m.approval.Confirmed = false
			return m, nil
		}
		m.approval.Confirmed = false
		m.approval.Previewed = false
		return m, m.executeCmd(app.ApprovePlanCommand{Workflow: m.selected, Revision: m.plan.Revision, Hash: m.plan.Hash})
	}
	return m, cmd
}

// handleExecutionApprovalKey handles the Execution Approval page: 's'
// generates the Specs, 'w' compiles the Dynamic Workflow, 'd' runs the
// Execution Dry Run, and the second Enter after the exact displayed preview
// issues ApproveExecutionCommand binding the exact preview
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
	case msg.Code == tea.KeyEsc:
		return m, nil
	}
	upd, cmd := m.approval.Update(msg)
	m.approval = upd
	if m.approval.Confirmed {
		// The Approval binds the exact displayed hashes: a partial
		// preview (e.g., before the dry run fixed the routing, budget,
		// and commit-policy hashes) can never be approved.
		if m.preview.PlanHash == "" || len(m.preview.SpecHashes) == 0 ||
			m.preview.CatalogHash == "" || m.preview.WorkflowHash == "" ||
			m.preview.CommitPolicyHash == "" {
			m.status = "no complete execution preview to approve; run the dry run first"
			m.approval.Confirmed = false
			return m, nil
		}
		m.approval.Confirmed = false
		m.approval.Previewed = false
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
		if hasAction(m.workspace.Actions, ActionResume) {
			return m.openStageActionPreview(app.MenuActionResume, "Resume and Start Runner")
		}
		return m.openStageActionPreview(app.MenuActionStartRunner, "Start Runner")
	case msg.Code == 'a' || msg.Code == 'A':
		if m.pendingDecision != "" {
			return m.openStageActionPreview(app.MenuActionAdoptWorkspace, "Adopt Workspace")
		}
		return m, nil
	case msg.Code == tea.KeyEsc:
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
		return m.openStageActionPreview(app.MenuActionResume, "Resume Workflow")
	case msg.Code == tea.KeyEsc:
		return m, nil
	}
	return m, nil
}

// handleTerminalKey handles the Terminal page (Report/Apply/Cleanup):
// 'r' renders the Final Report, 'p' stages the Apply, 'c' produces the
// Cleanup Dry Run Manifest, and the second Enter after the current section's
// preview executes the Apply or Cleanup command.
func (m Model) handleTerminalKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.selected == "" {
		m.status = "no workflow selected"
		return m, nil
	}
	switch {
	case msg.Code == 'r' || msg.Code == 'R':
		m.status = "rendering the final report…"
		workflow := m.selected
		m.reportRequestID++
		requestID := m.reportRequestID
		return m, func() tea.Msg {
			view, err := m.ctrl.Query(m.context(), app.ReportQuery{Workflow: workflow, Build: m.deps.CLI.Build})
			if err != nil {
				return reportLoadedMsg{workflow: workflow, requestID: requestID, err: err}
			}
			if rv, ok := view.(app.ReportView); ok {
				return reportLoadedMsg{workflow: workflow, requestID: requestID, markdown: rv.Markdown}
			}
			return reportLoadedMsg{workflow: workflow, requestID: requestID, err: fmt.Errorf("unexpected report projection")}
		}
	case msg.Code == 'p' || msg.Code == 'P':
		return m.openStageActionPreview(app.MenuActionApply, "Stage Apply")
	case msg.Code == 'c' || msg.Code == 'C':
		return m.openStageActionPreview(app.MenuActionCleanup, "Prepare Cleanup Dry Run")
	case msg.Code == tea.KeyEsc:
	}
	upd, cmd := m.terminal.Update(msg)
	m.terminal = upd
	if m.terminal.Confirmed {
		switch m.terminal.Section {
		case SectionApply:
			if m.terminal.ApplyPreview == "" {
				m.status = "no staged apply to deliver; stage it first"
				m.terminal.Confirmed = false
				return m, nil
			}
			m.terminal.Confirmed = false
			m.terminal.Previewed = false
			m.status = "delivering the apply…"
			attempt := m.terminal.applyAttempt
			if attempt == nil || attempt.ID == "" || attempt.TargetHead == "" || attempt.IntegrationHead == "" ||
				attempt.Preflight.Revision < 1 || attempt.Preflight.Hash == "" || attempt.PreflightHash == "" || attempt.Fingerprint == "" {
				m.status = "no complete apply preview to deliver; stage it again"
				m.terminal.Confirmed = false
				return m, nil
			}
			return m, m.executeCmd(app.ExecuteApplyCommand{
				Workflow: m.selected, AttemptID: attempt.ID, TargetHead: attempt.TargetHead,
				IntegrationHead: attempt.IntegrationHead, Preflight: attempt.Preflight,
				PreflightRevision: attempt.Preflight.Revision, PreflightHash: attempt.PreflightHash,
				Fingerprint: attempt.Fingerprint,
			})
		case SectionCleanup:
			if m.terminal.CleanupPreview == "" {
				m.status = "no cleanup manifest to execute; produce the dry run first"
				m.terminal.Confirmed = false
				return m, nil
			}
			m.terminal.Confirmed = false
			m.terminal.Previewed = false
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

// handleCancelKey handles the Cancel Preview -> Execute flow.
func (m Model) handleCancelKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.selected == "" {
		m.status = "no workflow selected"
		return m, nil
	}
	switch {
	case IsEnter(msg):
		if !m.cancelPreview {
			m.cancelPreview = true
			m.status = "cancel preview ready; press Enter to execute"
			return m, nil
		}
		m.status = "cancelling the workflow…"
		return m, m.executeCmd(app.CancelWorkflowCommand{Workflow: m.selected, Reason: "user confirmed cancel in the TUI"})
	}
	return m, nil
}

// handleActionPreviewKey executes only the action selected by the
// Application-owned Workflow Menu. The first Enter displays the explicit
// preview state; the second Enter issues the typed command. Start Runner
// continues through the existing Execution page and runner seam.
func (m Model) handleActionPreviewKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if !IsEnter(msg) {
		return m, nil
	}
	if !m.actionPreviewed {
		m.actionPreviewed = true
		m.status = "action preview ready; press Enter to execute"
		return m, nil
	}
	m.actionPreviewed = false
	operationID := m.currentOperationID()
	switch m.workflowMenuPreviewItem.Action {
	case app.MenuActionResume:
		m.status = "resuming…"
		cmd := m.executeActionPreviewCommand(app.ResumeWorkflowCommand{Workflow: m.selected})
		m, _ = m.popNavigation()
		if m.navigation.Current().Layer == LayerWorkflowMenu {
			m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageExecution, Workflow: m.selected})
		}
		return m, cmd
	case app.MenuActionPause:
		m.status = "pausing…"
		cmd := m.executeActionPreviewCommand(app.PauseWorkflowCommand{Workflow: m.selected})
		return m, cmd
	case app.MenuActionStartRunner:
		operationID = m.beginOperation("runner.start")
		m.traceUIActionFor(uiActionActionPreviewConfirm, m.selected, operationID)
		m, _ = m.popNavigation()
		if m.navigation.Current().Layer == LayerStageWorkspace {
			m, _ = m.popNavigation()
		}
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageExecution, Workflow: m.selected})
		return m.startRunner()
	case app.MenuActionApply:
		m.terminal.Section = SectionApply
		cmd := m.executeActionPreviewCommand(app.PrepareApplyCommand{Workflow: m.selected})
		m, _ = m.popNavigation()
		return m, cmd
	case app.MenuActionCleanup:
		m.terminal.Section = SectionCleanup
		cmd := m.executeActionPreviewCommand(app.DryRunCommand{Workflow: m.selected})
		m, _ = m.popNavigation()
		return m, cmd
	case app.MenuActionAdoptWorkspace:
		cmd := m.executeActionPreviewCommand(app.AdoptWorkspaceCommand{Workflow: m.selected})
		m, _ = m.popNavigation()
		return m, cmd
	case app.MenuActionStartDiscussion:
		cmd := m.executeActionPreviewCommand(app.PrepareNativeDiscussionCommand{Workflow: m.selected, Provider: m.discussionProvider()})
		m, _ = m.popNavigation()
		return m, cmd
	case app.MenuActionContinueDiscussion:
		cmd := m.executeActionPreviewCommand(app.ContinueNativeDiscussionCommand{Workflow: m.selected, Session: model.SessionID(m.discussion.Session)})
		m, _ = m.popNavigation()
		return m, cmd
	case app.MenuActionFinishDiscussion:
		var command app.Command
		if m.discussion.Handoff != "" {
			command = app.FinishDiscussionCommand{Workflow: m.selected, Session: model.SessionID(m.discussion.Session), Decisions: []byte(m.discussion.Handoff)}
		} else {
			command = app.FreezeDiscussionCommand{Workflow: m.selected, Session: model.SessionID(m.discussion.Session)}
		}
		cmd := m.executeActionPreviewCommand(command)
		m, _ = m.popNavigation()
		return m, cmd
	case app.MenuActionSwitchDiscussion:
		command, err := m.switchAgentCommand()
		if err != nil {
			m.traceUIActionFor(uiActionActionPreviewConfirm, m.selected, operationID)
			cmd := func() tea.Msg { return commandDoneMsg{err: err} }
			m, _ = m.popNavigation()
			return m, cmd
		}
		cmd := m.executeActionPreviewCommand(command)
		m, _ = m.popNavigation()
		return m, cmd
	default:
		return m, nil
	}
}

func (m Model) executeActionPreviewCommand(command app.Command) tea.Cmd {
	operationID := m.beginOperation("command." + operationType(command))
	m.traceUIActionFor(uiActionActionPreviewConfirm, m.selected, operationID)
	return m.executeCmdWithOperation(m.context(), command, operationID)
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
	workflow := m.selected
	ctx, cancel := context.WithCancel(m.context())
	ch, id := m.sink.Subscribe()
	m.runnerID++
	runnerID := m.runnerID
	runnerDone := make(chan struct{})
	m.runnerWorkflow = workflow
	m.runnerDone = runnerDone
	m.runCancel = cancel
	m.running = true
	m.eventCh = ch
	return m, tea.Batch(
		func() tea.Msg {
			defer func() {
				m.sink.Unsubscribe(id)
				close(runnerDone)
			}()
			r := &foreground.Runner{
				Driver: m.ctrl,
				OnEvent: func(ev model.Event) {
					m.sink.Publish(ev)
				},
			}
			res, err := r.Run(ctx, workflow)
			if err != nil {
				return runnerDoneMsg{err: err, workflow: workflow, runnerID: runnerID}
			}
			return runnerDoneMsg{res: res, workflow: workflow, runnerID: runnerID}
		},
		m.pumpEvents(workflow, runnerID),
	)
}

// pumpEvents forwards one committed Runner event to the Execution page.
// A terminal path can clear eventCh while a pump command is still in
// flight (the renderer-error and the runner's event send come from
// different goroutines with no happens-before edge): a cleared channel
// must end the pump with eventsClosedMsg instead of blocking forever on
// a nil channel (a leaked goroutine).
func (m Model) pumpEvents(workflow model.WorkflowID, runnerID uint64) tea.Cmd {
	ch := m.eventCh
	return func() tea.Msg {
		if ch == nil {
			return eventsClosedMsg{}
		}
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return runnerEventMsg{ev: ev, workflow: workflow, runnerID: runnerID}
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
	ctx    context.Context
	req    native.Request
	result *native.Result
	err    error
}

func (c *nativeExec) Run() error {
	result, err := (native.Bridge{}).Run(c.ctx, c.req)
	c.result = &result
	c.err = err
	return err
}

func (c *nativeExec) SetStdin(r io.Reader)  { c.req.Terminal.In = r }
func (c *nativeExec) SetStdout(w io.Writer) { c.req.Terminal.Out = w }
func (c *nativeExec) SetStderr(w io.Writer) { c.req.Terminal.Err = w }

// newNativeExecCmd builds the blocking-exec adapter of one prepared
// native discussion turn.
func newNativeExecCmd(ctx context.Context, req *app.NativeBridgeRequest) tea.Cmd {
	cmd := &nativeExec{
		ctx: ctx,
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
		base := fmt.Sprintf("cflow: %v\n\nuse /exit to exit", m.err)
		if m.commandPalette.Open {
			return overlayCommandPalette(base, RenderCommandPalette(m.commandPalette, m.width, m.height), m.width, m.height)
		}
		return base
	}
	if !m.ready {
		base := "cflow\n\nuse /exit to exit"
		if m.commandPalette.Open {
			return overlayCommandPalette(base, RenderCommandPalette(m.commandPalette, m.width, m.height), m.width, m.height)
		}
		return base
	}
	base := renderPersistentWorkbench(m)
	if m.commandPalette.Open {
		return overlayCommandPalette(base, RenderCommandPalette(m.commandPalette, m.width, m.height), m.width, m.height)
	}
	return base
}

// renderPersistentWorkbench keeps the three-column shell stable while the
// selected page changes only the center WORKSPACE content. The page enum and
// navigation stack remain UI state for routing and acknowledgement binding;
// they are deliberately not allowed to replace the entire screen.
func renderPersistentWorkbench(m Model) string {
	width, height := m.width, m.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 1
	}
	header := renderWorkspaceHeaderLines(m.workspace, width)
	footer := renderWorkspaceFooter(m.workspace, m.status, width)
	bodyHeight := max(4, height-len(header)-1)

	center := workspacePanelWithHeight("WORKSPACE", renderCenterWorkspaceLines(m), width, bodyHeight)
	layout := workspaceLayoutFor(width, height)
	if layout == layoutMinimal {
		// Keep the small-viewport policy from RenderWorkspace: a partial panel
		// is worse than a compact center state, and the persistent shell is
		// represented by the bounded header/content/footer rows here.
		return joinWorkspaceSections(header, strings.Join(renderCenterWorkspaceLines(m), "\n"), footer, width, height)
	}
	switch layout {
	case layoutWide:
		available := width - 2*2
		leftWidth := clamp(24, available/4, 34)
		rightWidth := clamp(28, available/4, 36)
		middleWidth := available - leftWidth - rightWidth
		if middleWidth >= 32 {
			left := workspacePanelWithHeight("WORKFLOWS", renderWorkflowLines(m.workspace), leftWidth, bodyHeight)
			right := workspacePanelWithHeight("INSPECTOR", renderInspectorLines(m.workspace), rightWidth, bodyHeight)
			return joinWorkspaceSections(header, joinWorkspaceColumns([]string{left, center, right}, []int{leftWidth, middleWidth, rightWidth}), footer, width, height)
		}
	case layoutMedium:
		available := width - 2
		leftWidth := clamp(24, available/3, 34)
		mainWidth := available - leftWidth
		if mainWidth >= 32 {
			left := workspacePanelWithHeight("WORKFLOWS", renderWorkflowLines(m.workspace), leftWidth, bodyHeight)
			center = workspacePanelWithHeight("WORKSPACE", renderCenterWorkspaceLines(m), mainWidth, bodyHeight)
			return joinWorkspaceSections(header, joinWorkspaceColumns([]string{left, center}, []int{leftWidth, mainWidth}), footer, width, height)
		}
	}

	// At compact sizes the existing responsive policy collapses the shell to
	// one bounded center panel. The content state still changes in the center;
	// no full-screen page renderer is introduced as a second layout.
	return joinWorkspaceSections(header, center, footer, width, height)
}

func renderCenterWorkspaceLines(m Model) []string {
	var text string
	switch m.page {
	case PageWorkspace:
		return renderLifecycleLines(m.workspace)
	case PageWorkflowMenu:
		return workflowMenuContentLines(m.workflowMenuModel, m.workflowMenuError)
	case PageReadonlyWorkspace:
		return readonlyWorkspaceContentLines(m.readonly)
	case PageActionPreview:
		text = renderWorkflowActionPreview(m.workflowMenuModel, m.workflowMenuPreviewItem)
	case PageDiscussion:
		text = RenderDiscussionReturn(m.discussion) + m.hints()
	case PagePlanApproval:
		text = RenderPlanApproval(m.plan, m.approval) + m.hints()
	case PageExecutionApproval:
		text = RenderApproval(m.approval) + m.hints()
	case PageExecution:
		text = RenderExecutionAt(m.execution, m.width)
		if m.pendingDecision != "" {
			text += renderDecisionPanel(m.pendingDecision)
		}
		text += m.hints()
	case PageBlocked:
		text = RenderBlocked(m.workspace) + m.hints()
	case PageTerminal:
		text = RenderTerminal(m.terminal) + m.hints()
	case PageCreate:
		text = renderCreate(m) + m.hints()
	case PageCancel:
		text = renderCancel(m) + m.hints()
	case PagePauseExit:
		text = renderPauseExit()
	case PageMigration:
		text = renderMigration(m) + m.hints()
	default:
		return []string{"workspace content unavailable"}
	}
	text = strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		return []string{"workspace content unavailable"}
	}
	return strings.Split(text, "\n")
}

// overlayWorkspaceStatus keeps transient command feedback in the Workspace's
// single footer row without widening the pure RenderWorkspace input contract.
// Replacing the already-bounded final line preserves the original height and
// avoids adding a second status row.
func overlayWorkspaceStatus(frame string, workspace WorkspaceViewModel, status string, width int) string {
	if status == "" {
		return frame
	}
	lines := strings.Split(frame, "\n")
	if len(lines) == 0 {
		return frame
	}
	lines[len(lines)-1] = renderWorkspaceFooter(workspace, status, width)
	return strings.Join(lines, "\n")
}

// hints is the key hint footer of the current page. The workflow-action
// hints are driven ONLY by the Runtime LegalActions projection (design
// §5.3): a page never advertises a key whose action the Runtime does not
// currently permit.
func (m Model) hints() string {
	switch m.page {
	case PageDiscussion:
		return "\n↑/↓ action  Enter run  b workspace\n"
	case PagePlanApproval:
		return "\ng generate plan  k check plan  Enter preview/approve  Esc back  / command\n"
	case PageExecutionApproval:
		return "\ns generate specs  w compile workflow  d dry run  Enter preview/approve  Esc back  / command\n"
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
		return "\n←/→ section  r report  p stage apply  c cleanup dry run  Enter preview/execute  Esc back  / command\n"
	case PageCreate:
		return createHints(m)
	case PageCancel:
		return "\nEsc back  / command\n"
	case PagePauseExit:
		return ""
	case PageMigration:
		return "\np select prepare  e select execute  Enter preview/execute  Esc back  / command\n"
	}
	return ""
}

// blockedHints renders the Blocked page hint from the Runtime LegalActions
// only: Resume appears solely when the Runtime permits it.
func blockedHints(m WorkspaceViewModel) string {
	parts := []string{"b workspace"}
	if hasAction(m.Actions, ActionResume) {
		parts = append(parts, "r resume")
	}
	return "\n" + strings.Join(parts, "  ") + "\n"
}

// createHints renders the Create Workspace -> Create Preview Enter-only flow.
func createHints(m Model) string {
	if m.createConfirm {
		return "\nEnter create, esc back to edit\n"
	}
	return "\ntype the workflow name; Enter review, esc cancel\n"
}

func renderPauseExit() string {
	return "a workflow is running.\nPause and Exit? Enter execute or retry, Esc back (controlled stop waits for the runner to join; Ctrl+C remains available)\n"
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
		if m.migrationPreviewed {
			b.WriteString("\nPREVIEW READY — Enter execute prepare; Esc back.\n")
		} else {
			b.WriteString("\nPrepare selected — Enter preview; Esc back.\n")
		}
	case migrationConfirmExecute:
		if m.migrationPreviewed {
			b.WriteString("\nPREVIEW READY — Enter execute migration; Esc back.\n")
		} else {
			b.WriteString("\nExecute selected — Enter preview; Esc back.\n")
		}
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

// renderCreate renders the Create Workspace and Create Preview states. The
// read-only Discovery projection surfaces the target's dirty state, dirty
// fingerprint, and isolation before the Enter-only confirmation (Task 6).
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
		b.WriteString("create workflow?\n")
	} else {
		fmt.Fprintf(&b, "name: %s_\n", m.createInput)
		b.WriteString("Enter review, then Enter to create\n")
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
	if m.cancelPreview {
		b.WriteString("PREVIEW READY — Enter execute cancellation; Esc back.\n")
	} else {
		b.WriteString("Enter preview cancellation; Esc back.\n")
	}
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
	if a.TargetHead != "" {
		fmt.Fprintf(&b, "target head: %s\n", shortHead(a.TargetHead))
	}
	if a.IntegrationHead != "" {
		fmt.Fprintf(&b, "integration head: %s\n", shortHead(a.IntegrationHead))
	}
	if a.Preflight.Revision > 0 {
		fmt.Fprintf(&b, "preflight: rev %d %s\n", a.Preflight.Revision, shortHead(a.Preflight.Hash))
	}
	if a.PreflightHash != "" {
		fmt.Fprintf(&b, "preflight hash: %s\n", shortHead(a.PreflightHash))
	}
	if a.Fingerprint != "" {
		fmt.Fprintf(&b, "fingerprint: %s\n", shortHead(a.Fingerprint))
	}
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
