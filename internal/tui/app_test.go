package tui

// Root TUI Model tests (TUI tasks 9, 10, 14): the Model loads the real
// read-only Workspace projection through the shared Application, page
// navigation reaches every lifecycle page, user actions map to the exact
// typed Application Commands, Enter alone never approves, and the
// controlled-stop protocol executes the real Pause and Force Stop.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/foreground"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

func TestWorkspaceQueryRetriesWithoutStaleSelection(t *testing.T) {
	ctrl := &staleWorkspaceQueryController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	msg := m.queryProjectionMsg(PageWorkspace, app.ProjectWorkspaceQuery{Selected: "wf-removed"})
	projection, ok := msg.(projectionMsg)
	if !ok || projection.err != nil {
		t.Fatalf("workspace query fallback = %#v", msg)
	}
	if len(ctrl.queries) != 2 {
		t.Fatalf("workspace queries = %d, want stale selection plus fallback", len(ctrl.queries))
	}
	if got := ctrl.queries[1].(app.ProjectWorkspaceQuery).Selected; got != "" {
		t.Fatalf("fallback selected = %q, want empty", got)
	}
	if _, ok := projection.view.(app.WorkspaceView); !ok {
		t.Fatalf("fallback view = %T, want app.WorkspaceView", projection.view)
	}
}

type staleWorkspaceQueryController struct {
	queries []app.Query
}

func (c *staleWorkspaceQueryController) Execute(context.Context, app.Command) (app.Outcome, error) {
	return app.Outcome{}, nil
}

func (c *staleWorkspaceQueryController) Query(_ context.Context, q app.Query) (app.View, error) {
	c.queries = append(c.queries, q)
	workspaceQ, ok := q.(app.ProjectWorkspaceQuery)
	if !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	if workspaceQ.Selected != "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(workspaceQ.Selected))
	}
	return app.WorkspaceView{Workflows: []app.WorkflowSummary{{ID: "wf-1"}}}, nil
}

func (*staleWorkspaceQueryController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}

func (*staleWorkspaceQueryController) EscalateStop() {}

func TestApplyProjectionNormalizesStaleWorkspaceSelection(t *testing.T) {
	m := newModel(Dependencies{})
	m.selected = "wf-removed"
	m, _ = m.applyProjection(projectionMsg{
		page: PageWorkspace,
		view: app.WorkspaceView{
			Selected:     "wf-removed",
			Workflows:    []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimePaused}},
			Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Runtime: model.RuntimePaused}},
			LegalActions: []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
		},
	})
	if m.selected != "wf-1" || m.workspace.Selected.ID != "wf-1" {
		t.Fatalf("root selection = %q, workspace selection = %q; want wf-1", m.selected, m.workspace.Selected.ID)
	}
}

func TestWorkspaceStatusStaysInsideViewport(t *testing.T) {
	m := newModel(Dependencies{})
	m.ready = true
	m.page = PageWorkspace
	m.width = 120
	m.height = 30
	m.status = "workflow created"

	out := render(m)
	if !strings.Contains(out, "status: workflow created") {
		t.Fatalf("workspace status is not rendered:\n%s", out)
	}
	if got := lipgloss.Height(out); got > m.height {
		t.Fatalf("workspace render has %d content rows for a %d-row viewport", got, m.height)
	}
	if strings.Count(out, "status:") != 1 {
		t.Fatalf("workspace status/footer should be a single row, got %q", out)
	}
}

func TestNonWorkspaceStatusPreservesDiagnosticText(t *testing.T) {
	m := newModel(Dependencies{})
	m.ready = true
	m.page = PagePauseExit
	m.width = 80
	m.height = 24
	m.status = "first diagnostic line\nsecond diagnostic line"

	out := render(m)
	if !strings.Contains(out, "status: first diagnostic line\nsecond diagnostic line\n") {
		t.Fatalf("non-Workspace status was normalized or truncated: %q", out)
	}
}

func TestWorkspaceStatusIsSingleLineAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		width  int
		height int
		status string
	}{
		{name: "narrow wrapped status", width: 20, height: 2, status: "第一行\n第二行 with a very long suffix"},
		{name: "single row viewport", width: 12, height: 1, status: "状态 with a very long message"},
		{name: "two row viewport", width: 16, height: 2, status: "two\nrows with a long suffix"},
		{name: "three row viewport", width: 18, height: 3, status: "three\nrows with a long suffix"},
		{name: "ansi and cjk", width: 24, height: 4, status: "\033[31m错误\033[0m: 发生了一个很长的问题"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(Dependencies{})
			m.ready = true
			m.page = PageWorkspace
			m.width = tc.width
			m.height = tc.height
			m.status = tc.status
			out := render(m)
			if got := lipgloss.Height(out); got > tc.height {
				t.Fatalf("render has %d rows for %d-row viewport: %q", got, tc.height, out)
			}
			rows := strings.Split(out, "\n")
			for i, row := range rows {
				if got := lipgloss.Width(row); got > tc.width {
					t.Fatalf("row %d width %d > %d: %q", i, got, tc.width, row)
				}
			}
			if tc.height == 1 {
				if !strings.Contains(out, "quit") {
					t.Fatalf("minimal Workspace footer disappeared: %q", out)
				}
			} else if !strings.Contains(out, "status:") {
				t.Fatalf("status disappeared: %q", out)
			}
			if strings.Count(out, "status:") > 1 {
				t.Fatalf("status was rendered more than once: %q", out)
			}
		})
	}
}

func TestWorkspaceMinimalViewportKeepsFooterVisible(t *testing.T) {
	for _, tc := range []struct {
		width  int
		height int
	}{
		{width: 20, height: 1},
		{width: 20, height: 2},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			m := newModel(Dependencies{})
			m.ready = true
			m.page = PageWorkspace
			m.width = tc.width
			m.height = tc.height
			m.status = "status that must not hide the footer"
			out := render(m)
			if got := lipgloss.Height(out); got > tc.height {
				t.Fatalf("render height %d > %d: %q", got, tc.height, out)
			}
			if !strings.Contains(out, "quit") {
				t.Fatalf("minimal Workspace footer is missing: %q", out)
			}
		})
	}
}

func TestWorkspaceRootCompactFooterPreservesProjectedActions(t *testing.T) {
	for _, tc := range []struct {
		width  int
		height int
	}{
		{width: 60, height: 18},
		{width: 80, height: 24},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			m := newModel(Dependencies{})
			m.ready = true
			m.page = PageWorkspace
			m.width = tc.width
			m.height = tc.height
			m.workspace = longWorkspaceViewModel()
			m.workspace.Actions = []Action{ActionResume, ActionPause, ActionCancel, ActionMigrate}
			m.status = "a long provider/status message that is optional footer detail"
			out := render(m)
			if got := lipgloss.Height(out); got > tc.height {
				t.Fatalf("render height %d > %d: %q", got, tc.height, out)
			}
			for i, line := range strings.Split(out, "\n") {
				if got := lipgloss.Width(line); got > tc.width {
					t.Fatalf("row %d width %d > %d: %q", i, got, tc.width, line)
				}
			}
			for _, want := range []string{"r resume", "p pause", "x cancel", "m migrate"} {
				if !strings.Contains(out, want) {
					t.Fatalf("compact root render misses %q: %q", want, out)
				}
			}
		})
	}
}

// recordingController wraps the shared Application and records every
// typed Command the TUI issues and every EscalateStop call.
type recordingController struct {
	ctrl      controller
	executed  []app.Command
	escalated int
}

type migrationController struct{ executed []app.Command }

func (m *migrationController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	m.executed = append(m.executed, cmd)
	return app.Outcome{Workflow: "wf-legacy"}, nil
}
func (m *migrationController) Query(_ context.Context, q app.Query) (app.View, error) {
	switch q.(type) {
	case app.ProjectWorkspaceQuery:
		return app.WorkspaceView{
			Selected: "wf-legacy", Workflows: []app.WorkflowSummary{{ID: "wf-legacy", Runtime: model.RuntimePaused}},
			Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-legacy", LayoutVersion: 1}},
			LegalActions: []app.LegalAction{{Label: "Migrate layout", Hint: "layout-migration"}},
		}, nil
	case app.LayoutMigrationPreviewQuery:
		return app.MigrationPreviewView{Workflow: "wf-legacy", From: 1, To: 2,
			ManifestHash: "manifest-1", Moves: []model.PathMove{{Kind: model.MoveKindArtifact, Source: "/old", Destination: "/new"}}}, nil
	}
	return nil, model.InvalidInputFault("unexpected query")
}
func (*migrationController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}
func (*migrationController) EscalateStop() {}

// TestModelMigrationEntryPointsDefaultNo drives the TUI's explicit
// Preview/Prepare/Execute surface. Enter at either confirmation is No;
// only an explicit y sends the typed mutation command.
func TestModelMigrationEntryPointsDefaultNo(t *testing.T) {
	ctrl := &migrationController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m = press(t, m, 'm', 0) // read-only Preview
	if !strings.Contains(render(m), "manifest-1") {
		t.Fatalf("migration preview page not opened:\n%s", render(m))
	}
	m = press(t, m, 'p', 0)
	m = press(t, m, tea.KeyEnter, 0)
	if len(ctrl.executed) != 0 {
		t.Fatalf("Enter confirmed Prepare: %v", ctrl.executed)
	}
	m = press(t, m, 'p', 0)
	m = press(t, m, 'y', 0)
	if len(ctrl.executed) != 1 {
		t.Fatalf("explicit y did not Prepare: %v", ctrl.executed)
	}
	if _, ok := ctrl.executed[0].(app.PrepareLayoutMigrationCommand); !ok {
		t.Fatalf("Prepare command type = %T", ctrl.executed[0])
	}
	m = press(t, m, 'e', 0)
	m = press(t, m, tea.KeyEnter, 0)
	if len(ctrl.executed) != 1 {
		t.Fatalf("Enter confirmed Execute: %v", ctrl.executed)
	}
	m = press(t, m, 'e', 0)
	m = press(t, m, 'y', 0)
	if len(ctrl.executed) != 2 {
		t.Fatalf("explicit y did not Execute: %v", ctrl.executed)
	}
	if _, ok := ctrl.executed[1].(app.ExecuteLayoutMigrationCommand); !ok {
		t.Fatalf("Execute command type = %T", ctrl.executed[1])
	}
}

// preparedMigrationController returns a complete PREPARED migration
// preview so the render test can assert the full evidence.
type preparedMigrationController struct{}

func (preparedMigrationController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	return app.Outcome{Workflow: "wf-legacy"}, nil
}
func (preparedMigrationController) Query(_ context.Context, q app.Query) (app.View, error) {
	switch q.(type) {
	case app.ProjectWorkspaceQuery:
		return app.WorkspaceView{
			Selected: "wf-legacy", Workflows: []app.WorkflowSummary{{ID: "wf-legacy", Runtime: model.RuntimePaused}},
			Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-legacy", LayoutVersion: 1}},
			LegalActions: []app.LegalAction{{Label: "Migrate layout", Hint: "layout-migration"}},
		}, nil
	case app.LayoutMigrationPreviewQuery:
		return app.MigrationPreviewView{
			Workflow: "wf-legacy", From: 1, To: 2, Status: "PREPARED",
			MigrationID: "migration-wf-legacy-abc123", ManifestHash: "manifest-1",
			ManifestPath: "/cflow/projects/p/wf-legacy/state/layout-migrations/migration-wf-legacy-abc123.json",
			BackupPath:   "/cflow/projects/p/wf-legacy/state/layout-migrations/migration-wf-legacy-abc123.db.backup",
			BackupHash:   "backup-1", BackupSize: 4096,
			SourceSnapshotHash:      "snapshot-hash-1",
			ExpectedWorkspacePath:   "/cflow/projects/p/wf-legacy/workspace",
			ExpectedWorkspaceBranch: "cflow/wf-legacy/integration",
			ExpectedWorkspaceHead:   "1111111111111111111111111111111111111111",
			Moves: []model.PathMove{
				{Kind: model.MoveKindWorktree, Source: "/cflow/worktrees/p/wf-legacy/integration",
					Destination: "/cflow/projects/p/wf-legacy/workspace",
					Branch:      "cflow/wf-legacy/integration", Head: "1111111111111111111111111111111111111111"},
				{Kind: model.MoveKindArtifact, Source: "/cflow/projects/p/wf-legacy/workflows/wf-legacy/artifacts",
					Destination: "/cflow/projects/p/wf-legacy/artifacts", Digest: "digest-1"},
			},
		}, nil
	}
	return nil, model.InvalidInputFault("unexpected query")
}
func (preparedMigrationController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}
func (preparedMigrationController) EscalateStop() {}

// TestModelMigrationRenderShowsCompleteEvidence proves the TUI migration
// page renders the full prepared evidence (finding 6): migration row/
// status, manifest and backup identity, database impact, and per-move
// branch/head/digest.
func TestModelMigrationRenderShowsCompleteEvidence(t *testing.T) {
	ctrl := &preparedMigrationController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m = press(t, m, 'm', 0) // read-only Preview
	out := render(m)
	for _, want := range []string{
		"status: PREPARED",
		"migration id: migration-wf-legacy-abc123",
		"manifest path:",
		"backup:",
		"source snapshot: snapshot-hash-1",
		"database impact: workspace=",
		"branch=cflow/wf-legacy/integration",
		"digest=digest-1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("migration render missing %q:\n%s", want, out)
		}
	}
}

// blockedController returns a BLOCKED workspace whose LegalActions the
// test controls: resumeLegal decides whether the Runtime permits Resume.
type blockedController struct {
	executed    []app.Command
	resumeLegal bool
}

func (c *blockedController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	return app.Outcome{Workflow: "wf-1"}, nil
}
func (c *blockedController) Query(_ context.Context, q app.Query) (app.View, error) {
	if _, ok := q.(app.ProjectWorkspaceQuery); !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	actions := []app.LegalAction{{Label: "Inspect", Hint: "blocked"}}
	if c.resumeLegal {
		actions = append(actions, app.LegalAction{Label: "Resume", Kind: model.ResumeWorkflow})
	}
	return app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimeBlocked}},
		Lifecycle: &app.WorkflowLifecycleView{
			Status:  app.StatusView{Workflow: "wf-1", Stage: model.StageExecution, Runtime: model.RuntimeBlocked},
			Blocked: true,
		},
		LegalActions: actions,
		Health:       app.HealthView{GitAvailable: true, Providers: []app.ProviderHealth{{Name: "fake", Compatible: true}}},
	}, nil
}
func (*blockedController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}
func (*blockedController) EscalateStop() {}

// TestModelBlockedPageIssuesNoResumeWithoutLegalAction: the Blocked page
// issues a Resume command ONLY when the Runtime LegalActions include it.
// A blocked workflow whose LegalActions contain NO Resume renders no
// resume key/hint and pressing r issues no Resume command.
func TestModelBlockedPageIssuesNoResumeWithoutLegalAction(t *testing.T) {
	ctrl := &blockedController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m.page = PageBlocked
	if got := render(m); strings.Contains(got, "r resume") {
		t.Fatalf("blocked page hard-codes the resume hint:\n%s", got)
	}
	m = press(t, m, 'r', 0)
	if len(ctrl.executed) != 0 {
		t.Fatalf("blocked page without a resume legal action executed %v", ctrl.executed)
	}
}

// TestModelBlockedPageKeepsResumeWhenLegal: when the Runtime LegalActions
// DO contain Resume the Blocked page renders the hint and pressing r
// issues the typed ResumeWorkflowCommand.
func TestModelBlockedPageKeepsResumeWhenLegal(t *testing.T) {
	ctrl := &blockedController{resumeLegal: true}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m.page = PageBlocked
	if got := render(m); !strings.Contains(got, "r resume") {
		t.Fatalf("blocked page lost the runtime resume hint:\n%s", got)
	}
	m = press(t, m, 'r', 0)
	if len(ctrl.executed) != 1 {
		t.Fatalf("blocked page with a resume legal action executed %v", ctrl.executed)
	}
	if _, ok := ctrl.executed[0].(app.ResumeWorkflowCommand); !ok {
		t.Fatalf("blocked resume command type = %T", ctrl.executed[0])
	}
}

// workspaceActionsController returns one workspace whose LegalActions the
// test controls (a PAUSED workflow for the resume tests).
type workspaceActionsController struct {
	executed    []app.Command
	resumeLegal bool
}

func (c *workspaceActionsController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	return app.Outcome{Workflow: "wf-1"}, nil
}
func (c *workspaceActionsController) Query(_ context.Context, q app.Query) (app.View, error) {
	if _, ok := q.(app.ProjectWorkspaceQuery); !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	var actions []app.LegalAction
	if c.resumeLegal {
		actions = append(actions, app.LegalAction{Label: "Resume", Kind: model.ResumeWorkflow})
	}
	return app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimePaused}},
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{Workflow: "wf-1", Stage: model.StageWorkflowGeneration, Runtime: model.RuntimePaused},
		},
		LegalActions: actions,
		Health:       app.HealthView{GitAvailable: true},
	}, nil
}
func (*workspaceActionsController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}
func (*workspaceActionsController) EscalateStop() {}

// TestModelWorkspaceResumeRequiresLegalAction: the Workspace r key is not
// an unconditional Resume; it executes ResumeWorkflowCommand only when the
// Runtime LegalActions include it.
func TestModelWorkspaceResumeRequiresLegalAction(t *testing.T) {
	// Without the resume legal action the key is a no-op.
	ctrl := &workspaceActionsController{}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m = press(t, m, 'r', 0)
	if len(ctrl.executed) != 0 {
		t.Fatalf("workspace r without a resume legal action executed %v", ctrl.executed)
	}
	// With the resume legal action the key issues the typed command.
	ctrl2 := &workspaceActionsController{resumeLegal: true}
	m2 := load(t, testModel(&recordingController{ctrl: ctrl2}))
	m2 = press(t, m2, 'r', 0)
	if len(ctrl2.executed) != 1 {
		t.Fatalf("workspace r with a resume legal action executed %v", ctrl2.executed)
	}
	if _, ok := ctrl2.executed[0].(app.ResumeWorkflowCommand); !ok {
		t.Fatalf("workspace resume command type = %T", ctrl2.executed[0])
	}
}

// executionController is the Execution page seam: the workspace projection
// and the DriveOnce result the test controls. resumeErr, when set, makes
// the Runtime reject the Resume command (the stale-projection case: the
// workspace still shows Resume against an already-RUNNING workflow).
type executionController struct {
	executed   []app.Command
	driveCalls int
	actions    []app.LegalAction
	runtime    model.RuntimeStatus
	resumeErr  error
}

func (c *executionController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	if _, ok := cmd.(app.ResumeWorkflowCommand); ok && c.resumeErr != nil {
		return app.Outcome{}, c.resumeErr
	}
	return app.Outcome{Workflow: "wf-1"}, nil
}
func (c *executionController) Query(_ context.Context, q app.Query) (app.View, error) {
	if _, ok := q.(app.ProjectWorkspaceQuery); !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	return app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: c.runtime}},
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{Workflow: "wf-1", Stage: model.StageExecution, Runtime: c.runtime},
		},
		LegalActions: c.actions,
		Health:       app.HealthView{GitAvailable: true},
	}, nil
}
func (c *executionController) DriveOnce(_ context.Context, _ model.WorkflowID) (app.DriveOutcome, error) {
	c.driveCalls++
	return app.DriveOutcome{Kind: app.DriveTerminal, Reason: "terminal"}, nil
}
func (*executionController) EscalateStop() {}

// TestModelExecutionResumeDrivenByLegalActions: the Execution page issues
// the Resume command ONLY when the Runtime LegalActions include it; a
// workflow without the resume legal action starts the Foreground Runner
// directly and never sends ResumeWorkflowCommand.
func TestModelExecutionResumeDrivenByLegalActions(t *testing.T) {
	// A PAUSED workflow whose LegalActions include Resume: r resumes first.
	paused := &executionController{
		actions: []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
		runtime: model.RuntimePaused,
	}
	m := load(t, testModel(&recordingController{ctrl: paused}))
	m.page = PageExecution
	m = press(t, m, 'r', 0)
	if len(paused.executed) != 1 {
		t.Fatalf("execution r with a resume legal action executed %v", paused.executed)
	}
	if _, ok := paused.executed[0].(app.ResumeWorkflowCommand); !ok {
		t.Fatalf("execution resume command type = %T", paused.executed[0])
	}

	// A RUNNING workflow whose LegalActions contain NO Resume: r starts the
	// runner directly and never issues ResumeWorkflowCommand.
	running := &executionController{
		actions: []app.LegalAction{{Label: "Pause", Kind: model.PauseWorkflow}},
		runtime: model.RuntimeRunning,
	}
	m2 := load(t, testModel(&recordingController{ctrl: running}))
	m2.page = PageExecution
	m2 = press(t, m2, 'r', 0)
	if len(running.executed) != 0 {
		t.Fatalf("execution r without a resume legal action executed %v", running.executed)
	}
	if running.driveCalls != 1 {
		t.Fatalf("the runner was not started (drive calls = %d)", running.driveCalls)
	}
}

// TestModelExecutionResumeRejectedStartsRunner covers the stale-projection
// window after an execution approval: the workflow is already RUNNING but
// the workspace projection still renders Resume as a legal action. Pressing
// r issues a ResumeWorkflowCommand the Kernel rejects; the rejected resume
// must clear the pending resume-then-run and fall back to starting the
// Foreground Runner directly (DriveOnce is a safe bounded step over the
// already-running workflow).
func TestModelExecutionResumeRejectedStartsRunner(t *testing.T) {
	ctrl := &executionController{
		actions:   []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
		runtime:   model.RuntimeRunning,
		resumeErr: model.InvalidInputFault("resume rejected: workflow is already running"),
	}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.page = PageExecution
	m = press(t, m, 'r', 0)
	if len(ctrl.executed) != 1 {
		t.Fatalf("execution r executed %v, want exactly the rejected resume", ctrl.executed)
	}
	if _, ok := ctrl.executed[0].(app.ResumeWorkflowCommand); !ok {
		t.Fatalf("execution resume command type = %T", ctrl.executed[0])
	}
	if ctrl.driveCalls != 1 {
		t.Fatalf("the runner was not started after the rejected resume (drive calls = %d)", ctrl.driveCalls)
	}
	if m.resumeThenRun {
		t.Fatal("resumeThenRun was not cleared after the rejected resume")
	}
}

// TestModelExecutionHintDrivenByLegalActions pins the reloaded-UI signal of
// the E2E: the Execution page hint is driven by the Runtime LegalActions.
// A PAUSED workflow (Resume legal) renders "r resume & run"; once the
// post-approval projection reloads the RUNNING workflow (no Resume legal)
// the hint drops the resume and renders "r start the runner".
func TestModelExecutionHintDrivenByLegalActions(t *testing.T) {
	paused := &executionController{
		actions: []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
		runtime: model.RuntimePaused,
	}
	m := load(t, testModel(&recordingController{ctrl: paused}))
	m.page = PageExecution
	if got := render(m); !strings.Contains(got, "r resume & run") {
		t.Fatalf("paused execution hint lost the resume:\n%s", got)
	}

	running := &executionController{
		actions: []app.LegalAction{{Label: "Pause", Kind: model.PauseWorkflow}},
		runtime: model.RuntimeRunning,
	}
	m2 := load(t, testModel(&recordingController{ctrl: running}))
	m2.page = PageExecution
	got := render(m2)
	if !strings.Contains(got, "r start the runner") {
		t.Fatalf("running execution hint did not drop the resume:\n%s", got)
	}
	if strings.Contains(got, "resume & run") {
		t.Fatalf("running execution hint still offers the resume:\n%s", got)
	}
}

// createController is the Create page seam: it answers the workspace load
// and the DiscoveryQuery with the target Git facts the test controls, and
// records every CreateWorkflowCommand. discoveryErr, when set, makes the
// DiscoveryQuery fail so no target facts ever load.
type createController struct {
	executed     []app.Command
	dirty        bool
	discoveryErr error
}

func (c *createController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	return app.Outcome{Workflow: "wf-new"}, nil
}
func (c *createController) Query(_ context.Context, q app.Query) (app.View, error) {
	switch q.(type) {
	case app.ProjectWorkspaceQuery:
		return app.WorkspaceView{
			Project: app.ProjectView{Name: "repo", Root: "/repo"},
			Health:  app.HealthView{GitAvailable: true, Providers: []app.ProviderHealth{{Name: "fake", Compatible: true}}},
		}, nil
	case app.DiscoveryQuery:
		if c.discoveryErr != nil {
			return nil, c.discoveryErr
		}
		return app.DiscoveryView{
			Branch: "main", Head: "0123456789abcdef",
			Dirty: c.dirty, DirtyFingerprint: "sha256:deadbeef",
			StagedCount: 1, UnstagedCount: 0, UntrackedCount: 1,
		}, nil
	}
	return nil, model.InvalidInputFault("unexpected query")
}
func (*createController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}
func (*createController) EscalateStop() {}

// createPage opens the Create page and types the workflow name.
func createPage(t *testing.T, m Model, name string) Model {
	t.Helper()
	m = press(t, m, 'n', 0)
	return typeText(t, m, name)
}

// typeText types one string through the Model as individual text key
// presses (the create name and handoff inputs use KeyPressMsg.Text).
func typeText(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m = step(t, m, tea.KeyPressMsg{Code: r, Text: string(r), Mod: 0})
	}
	return m
}

// TestCreateDirtyTargetEnterDoesNotCreate: a dirty target is queried and
// displayed before creation; the confirmation defaults to No, so Enter
// (both to submit the name and on the confirmation) never creates.
func TestCreateDirtyTargetEnterDoesNotCreate(t *testing.T) {
	ctrl := &createController{dirty: true}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m = createPage(t, m, "calculator")
	got := render(m)
	for _, want := range []string{"DIRTY", "dirty fingerprint: sha256:deadbeef", "will not touch your files"} {
		if !strings.Contains(got, want) {
			t.Fatalf("create page misses %q:\n%s", want, got)
		}
	}
	// Enter submits the name for the confirmation; it never creates.
	m = press(t, m, tea.KeyEnter, 0)
	if len(ctrl.executed) != 0 {
		t.Fatalf("Enter created the workflow: %v", ctrl.executed)
	}
	// Enter on the confirmation is No too.
	m = press(t, m, tea.KeyEnter, 0)
	if len(ctrl.executed) != 0 {
		t.Fatalf("Enter confirmed the workflow: %v", ctrl.executed)
	}
}

// TestCreateDirtyTargetYConfirmsDirty: only an explicit y sends the create
// command with ConfirmDirty: true on a dirty target.
func TestCreateDirtyTargetYConfirmsDirty(t *testing.T) {
	ctrl := &createController{dirty: true}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m = createPage(t, m, "calculator")
	m = press(t, m, tea.KeyEnter, 0) // submit the name for the confirmation
	m = press(t, m, 'y', 0)          // the explicit confirmation
	if len(ctrl.executed) != 1 {
		t.Fatalf("explicit y did not create: %v", ctrl.executed)
	}
	cc, ok := ctrl.executed[0].(app.CreateWorkflowCommand)
	if !ok {
		t.Fatalf("create command type = %T", ctrl.executed[0])
	}
	if cc.Name != "calculator" || !cc.ConfirmDirty {
		t.Fatalf("create = %+v, want Name calculator and ConfirmDirty:true", cc)
	}
}

// TestCreateCleanTargetCreatesWithoutDirtyFlag: a clean target creates
// with an explicit y and carries no dirty flag.
func TestCreateCleanTargetCreatesWithoutDirtyFlag(t *testing.T) {
	ctrl := &createController{dirty: false}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m = createPage(t, m, "calculator")
	if got := render(m); !strings.Contains(got, "clean") {
		t.Fatalf("clean target create page misses the clean state:\n%s", got)
	}
	m = press(t, m, tea.KeyEnter, 0) // submit the name for the confirmation
	m = press(t, m, 'y', 0)          // the explicit confirmation
	if len(ctrl.executed) != 1 {
		t.Fatalf("explicit y did not create: %v", ctrl.executed)
	}
	cc, ok := ctrl.executed[0].(app.CreateWorkflowCommand)
	if !ok {
		t.Fatalf("create command type = %T", ctrl.executed[0])
	}
	if cc.ConfirmDirty {
		t.Fatalf("clean target create carried a dirty flag: %+v", cc)
	}
}

// TestCreateMissingFactsYFailsClosed: when the DiscoveryQuery projection has
// not loaded (createDirty == nil), an explicit y on the confirmation never
// issues CreateWorkflowCommand — the create is fail-closed on the missing
// target facts instead of guessing the dirty state.
func TestCreateMissingFactsYFailsClosed(t *testing.T) {
	ctrl := &createController{dirty: true, discoveryErr: model.InvalidInputFault("discovery failed")}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m = createPage(t, m, "calculator")
	if got := render(m); !strings.Contains(got, "loading git facts") {
		t.Fatalf("create page without the queried facts:\n%s", got)
	}
	m = press(t, m, tea.KeyEnter, 0) // submit the name for the confirmation
	m = press(t, m, 'y', 0)          // confirm without the target facts
	if len(ctrl.executed) != 0 {
		t.Fatalf("y created without the target facts: %v", ctrl.executed)
	}
	if got := render(m); !strings.Contains(got, "target facts unavailable") {
		t.Fatalf("create page did not refuse the missing facts:\n%s", got)
	}
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
	for _, want := range []string{"project:", "workflows:", "workflow calculator (" + string(wf.Workflow) + ")", "health:"} {
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

// TestModelPlanCheckWaitsForFreshProjection prevents a stale Plan Approval
// projection from issuing CheckPlan while the GeneratePlan reload is still
// in flight.
func TestModelPlanCheckWaitsForFreshProjection(t *testing.T) {
	rec := &recordingController{ctrl: &migrationController{}}
	m := newModel(Dependencies{})
	m.ctrl = rec
	m.page = PagePlanApproval
	m.selected = "wf-1"
	m.plan = app.PlanView{
		Workflow: "wf-1",
		Stage:    model.StagePlanGeneration,
		Runtime:  model.RuntimeRunning,
	}

	// The command has completed, but its asynchronous projection reload has
	// not been applied yet: this is the timing shown in the user report.
	m, _ = m.applyCommand(commandDoneMsg{cmd: app.GeneratePlanCommand{}})
	m = press(t, m, 'k', 0)

	for _, cmd := range rec.executed {
		if _, ok := cmd.(app.CheckPlanCommand); ok {
			t.Fatalf("stale projection issued CheckPlanCommand: %v", rec.executed)
		}
	}
	if !strings.Contains(m.status, "wait") {
		t.Fatalf("status = %q, want a wait message", m.status)
	}
}

// TestModelPlanApprovalWaitsForCheckedProjection preserves an explicit y
// pressed after the CheckPlan command settled but before the refreshed
// PlanView arrived. The approval must bind the revision/hash from that
// fresh projection instead of dropping the user's action or using stale
// local data.
func TestModelPlanApprovalWaitsForCheckedProjection(t *testing.T) {
	rec := &recordingController{ctrl: &migrationController{}}
	m := newModel(Dependencies{})
	m.ctrl = rec
	m.page = PagePlanApproval
	m.selected = "wf-1"
	m.plan = app.PlanView{
		Workflow:   "wf-1",
		Stage:      model.StagePlanGeneration,
		Runtime:    model.RuntimePaused,
		PlanStatus: model.PlanDraft,
	}

	m, _ = m.applyCommand(commandDoneMsg{cmd: app.CheckPlanCommand{}})
	m = press(t, m, 'y', 0)
	if rec.hasExecuted(app.ApprovePlanCommand{}) {
		t.Fatal("stale projection issued ApprovePlanCommand")
	}
	if !m.pendingPlanApproval {
		t.Fatal("explicit approval was not queued while projection was stale")
	}

	m, cmd := m.applyProjection(projectionMsg{
		page: PagePlanApproval,
		view: app.PlanView{
			Workflow:   "wf-1",
			Stage:      model.StagePlanCheck,
			Runtime:    model.RuntimePaused,
			PlanStatus: model.PlanChecked,
			Revision:   1,
			Hash:       "fresh-plan-hash",
		},
	})
	if cmd == nil {
		t.Fatal("fresh checked projection did not resume the queued approval")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("queued approval command returned no completion message")
	}
	var approved app.ApprovePlanCommand
	for _, executed := range rec.executed {
		if got, ok := executed.(app.ApprovePlanCommand); ok {
			approved = got
		}
	}
	if approved.Revision != 1 || approved.Hash != "fresh-plan-hash" {
		t.Fatalf("approval = %+v, want fresh revision/hash", approved)
	}
}

func TestModelPlanProjectionIgnoresOutOfOrderOlderState(t *testing.T) {
	rec := &recordingController{ctrl: &migrationController{}}
	m := newModel(Dependencies{})
	m.ctrl = rec
	m.page = PagePlanApproval
	m.selected = "wf-1"

	m, _ = m.applyProjection(projectionMsg{
		page: PagePlanApproval,
		view: app.PlanView{
			Workflow:         "wf-1",
			AggregateVersion: 10,
			Stage:            model.StagePlanCheck,
			Runtime:          model.RuntimePaused,
			PlanStatus:       model.PlanChecked,
			Revision:         1,
			Hash:             "fresh-plan-hash",
		},
	})
	m, _ = m.applyProjection(projectionMsg{
		page: PagePlanApproval,
		view: app.PlanView{
			Workflow:         "wf-1",
			AggregateVersion: 9,
			Stage:            model.StagePlanCheck,
			Runtime:          model.RuntimePaused,
			PlanStatus:       model.PlanChecking,
			Revision:         1,
			Hash:             "fresh-plan-hash",
		},
	})
	if m.plan.PlanStatus != model.PlanChecked || m.plan.AggregateVersion != 10 {
		t.Fatalf("older projection replaced fresh state: %+v", m.plan)
	}

	m = press(t, m, 'y', 0)
	var approved app.ApprovePlanCommand
	for _, executed := range rec.executed {
		if got, ok := executed.(app.ApprovePlanCommand); ok {
			approved = got
		}
	}
	if approved.Revision != 1 || approved.Hash != "fresh-plan-hash" {
		t.Fatalf("approval = %+v, want fresh revision/hash", approved)
	}
}

func TestModelPlanCheckTerminalProjectionSettlesStatus(t *testing.T) {
	tests := []struct {
		name   string
		view   app.PlanView
		status string
	}{
		{
			name: "needs discussion",
			view: app.PlanView{
				Stage:      model.StageRequirementDiscussion,
				Runtime:    model.RuntimeRunning,
				PlanStatus: model.PlanDraft,
			},
			status: "plan check needs discussion",
		},
		{
			name: "needs revision",
			view: app.PlanView{
				Stage:      model.StagePlanGeneration,
				Runtime:    model.RuntimeRunning,
				PlanStatus: model.PlanDraft,
			},
			status: "plan check needs revision",
		},
		{
			name: "rejected",
			view: app.PlanView{
				Stage:      model.StagePlanCheck,
				Runtime:    model.RuntimePaused,
				PlanStatus: model.PlanRejected,
			},
			status: "plan rejected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(Dependencies{})
			m.page = PagePlanApproval
			m.selected = "wf-1"
			m.plan = app.PlanView{
				Workflow:   "wf-1",
				Stage:      model.StagePlanCheck,
				PlanStatus: model.PlanDraft,
			}
			m.planCheckInFlight = true
			m.pendingPlanStatus = "plan checked"
			m.pendingPlanApproval = true
			m.status = "plan check finished; refreshing plan projection…"

			tt.view.Workflow = "wf-1"
			m, _ = m.applyProjection(projectionMsg{page: PagePlanApproval, view: tt.view})

			if m.status != tt.status {
				t.Fatalf("status = %q, want %q", m.status, tt.status)
			}
			if m.pendingPlanStatus != "" {
				t.Fatalf("pending plan status = %q, want empty", m.pendingPlanStatus)
			}
			if m.planCheckInFlight {
				t.Fatal("plan check remained in flight after terminal projection")
			}
			if m.pendingPlanApproval {
				t.Fatal("approval remained queued after non-checked terminal projection")
			}
		})
	}
}

// TestModelPlanGeneratedStatusFollowsProjection prevents the command
// callback from claiming success before the refreshed PlanView is visible.
func TestModelPlanGeneratedStatusFollowsProjection(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PagePlanApproval
	m.selected = "wf-1"
	m.plan = app.PlanView{
		Workflow: "wf-1",
		Stage:    model.StagePlanGeneration,
		Runtime:  model.RuntimeRunning,
	}

	m, _ = m.applyCommand(commandDoneMsg{cmd: app.GeneratePlanCommand{}})
	if strings.Contains(m.status, "plan generated") {
		t.Fatalf("command callback claimed generated before projection: %q", m.status)
	}

	m, _ = m.applyProjection(projectionMsg{
		page: PagePlanApproval,
		view: app.PlanView{
			Workflow:   "wf-1",
			Stage:      model.StagePlanGeneration,
			Runtime:    model.RuntimeRunning,
			Revision:   0,
			PlanStatus: model.PlanDraft,
		},
	})
	if strings.Contains(m.status, "plan generated") {
		t.Fatalf("stale projection claimed generated: %q", m.status)
	}

	m, _ = m.applyProjection(projectionMsg{
		page: PagePlanApproval,
		view: app.PlanView{
			Workflow:   "wf-1",
			Stage:      model.StagePlanCheck,
			Runtime:    model.RuntimePaused,
			Revision:   1,
			Hash:       "plan-hash",
			PlanStatus: model.PlanDraft,
		},
	})
	if m.status != "plan generated" {
		t.Fatalf("fresh projection status = %q, want plan generated", m.status)
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

// ---------------------------------------------------------------------------
// Task 6: Foreground Runner ownership
// ---------------------------------------------------------------------------

// blockingController drives the Foreground Runner into a bounded wait: its
// DriveOnce returns a DriveWaiting outcome whose channel never closes, so
// the Runner blocks until its run context is cancelled (design §12.1).
type blockingController struct {
	executed []app.Command
	wait     chan struct{}
}

func (c *blockingController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	return app.Outcome{Workflow: "wf-1"}, nil
}

func (c *blockingController) Query(_ context.Context, q app.Query) (app.View, error) {
	if _, ok := q.(app.ProjectWorkspaceQuery); !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	return app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimeRunning}},
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{Workflow: "wf-1", Stage: model.StageExecution, Runtime: model.RuntimeRunning},
		},
		Health: app.HealthView{GitAvailable: true},
	}, nil
}

func (c *blockingController) DriveOnce(_ context.Context, _ model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{Kind: app.DriveWaiting, Wait: c.wait}, nil
}

func (*blockingController) EscalateStop() {}

// runBatchMessages runs a Batch command's sub-commands to completion and
// returns their messages. The synchronous test harness cannot run a
// blocking Runner, so the cancellation tests run it in a goroutine.
func runBatchMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	var results []tea.Msg
	switch batch := msg.(type) {
	case tea.BatchMsg:
		for _, c := range batch {
			if c != nil {
				results = append(results, c())
			}
		}
	default:
		results = append(results, msg)
	}
	return results
}

// hasRunnerStopped reports whether the messages contain the Runner's
// terminal result with the given stop reason.
func hasRunnerStopped(msgs []tea.Msg, reason foreground.StopReason) bool {
	for _, msg := range msgs {
		if rd, ok := msg.(runnerDoneMsg); ok && rd.err == nil {
			return rd.res.Reason == reason
		}
	}
	return false
}

// TestRunnerStartOwnsState: the root Model owns the Foreground Runner
// lifecycle (design §11, Task 6): startRunner returns the updated Model
// with running, runCancel, eventCh and the subscription set, and the
// runner-done terminal path clears them exactly once.
func TestRunnerStartOwnsState(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.selected = "wf-1"

	m2, runCmd := m.startRunner()
	if !m2.running || m2.runCancel == nil || m2.eventCh == nil {
		t.Fatalf("runner ownership after start: running=%v runCancel=%v eventCh=%v",
			m2.running, m2.runCancel == nil, m2.eventCh == nil)
	}
	if runCmd == nil {
		t.Fatal("startRunner returned no runner command")
	}

	// A terminal run clears the ownership state exactly once.
	m3 := runCmds(t, m2, runCmd)
	if m3.running || m3.runCancel != nil || m3.eventCh != nil {
		t.Fatalf("runner ownership after done: running=%v runCancel=%v eventCh=%v",
			m3.running, m3.runCancel != nil, m3.eventCh != nil)
	}
}

// TestRunnerDuplicateStartRefused: starting a Run while one is already
// active is refused (Task 6): no second runner command and no second
// subscription (the event channel is unchanged).
func TestRunnerDuplicateStartRefused(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.selected = "wf-1"

	m, firstCmd := m.startRunner()
	firstCh := m.eventCh
	if firstCmd == nil {
		t.Fatal("the first start produced no runner command")
	}

	m2, secondCmd := m.startRunner()
	if secondCmd != nil {
		t.Fatal("duplicate start issued a second runner command")
	}
	if m2.eventCh != firstCh {
		t.Fatal("duplicate start replaced the event subscription")
	}
	if !m2.running {
		t.Fatal("duplicate start cleared the running state")
	}
	if !strings.Contains(m2.status, "already active") {
		t.Fatalf("duplicate start status = %q", m2.status)
	}
}

// TestRunnerRunKeyRefusesDuplicate: pressing r while a Runner is active
// refuses the second start (the Execution page never spawns a second
// runner or a second subscription).
func TestRunnerRunKeyRefusesDuplicate(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.selected = "wf-1"
	m.page = PageExecution

	m, _ = m.startRunner()
	m2 := press(t, m, 'r', 0)
	if !strings.Contains(m2.status, "already active") {
		t.Fatalf("r on an active runner status = %q", m2.status)
	}
	if ctrl.driveCalls != 0 {
		t.Fatalf("duplicate r drove the runner again (drive calls = %d)", ctrl.driveCalls)
	}
}

// TestModelPauseAndExitCancelsRunner: y on the Pause and Exit
// confirmation cancels the REAL Runner (the run context is cancelled) so
// the runner stops with StopCancelled — a quit never leaves a process.
func TestModelPauseAndExitCancelsRunner(t *testing.T) {
	ctrl := &blockingController{wait: make(chan struct{})}
	rec := &recordingController{ctrl: ctrl}
	m := load(t, testModel(rec))
	m.selected = "wf-1"
	m.page = PageExecution

	m, runCmd := m.startRunner()
	if !m.running || m.runCancel == nil {
		t.Fatalf("runner did not start: running=%v runCancel=%v", m.running, m.runCancel == nil)
	}
	done := make(chan []tea.Msg, 1)
	go func() {
		done <- runBatchMessages(runCmd)
	}()

	// q shows the Pause and Exit confirmation instead of quitting directly.
	m2, cmd := m.Update(tea.KeyPressMsg{Code: KeyQuit})
	if cmd != nil {
		t.Fatal("q quit directly while a runner is active")
	}
	m2m := m2.(Model)
	if m2m.page != PagePauseExit || m2m.stop != stopPauseAndExit {
		t.Fatalf("q state = page %d stop %d, want pause-and-exit", m2m.page, m2m.stop)
	}

	// y requests the controlled pause and cancels the real runner.
	_, yCmd := m2m.Update(tea.KeyPressMsg{Code: 'y'})
	if yCmd == nil {
		t.Fatal("y produced no pause command")
	}
	if pauseMsg := yCmd(); pauseMsg == nil {
		t.Fatal("the pause command produced no message")
	}
	if !rec.hasExecuted(app.PauseWorkflowCommand{}) {
		t.Fatalf("y did not request the controlled pause: %v", rec.executed)
	}

	// The real runner stopped with StopCancelled (the run context was
	// cancelled on the controlled-stop path).
	select {
	case msgs := <-done:
		if !hasRunnerStopped(msgs, foreground.StopCancelled) {
			t.Fatalf("the runner did not stop with StopCancelled: %#v", msgs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner was not cancelled by the pause-and-exit")
	}
}

// TestModelCtrlCCancelsRunnerOnPause: the first Ctrl+C requests the
// controlled Pause AND cancels the real Runner (the run context is
// cancelled), so the runner stops with StopCancelled instead of driving
// on.
func TestModelCtrlCCancelsRunnerOnPause(t *testing.T) {
	ctrl := &blockingController{wait: make(chan struct{})}
	rec := &recordingController{ctrl: ctrl}
	m := load(t, testModel(rec))
	m.selected = "wf-1"
	m.page = PageExecution

	m, runCmd := m.startRunner()
	done := make(chan []tea.Msg, 1)
	go func() {
		done <- runBatchMessages(runCmd)
	}()

	// The first Ctrl+C requests the controlled pause and cancels the
	// runner.
	m2 := press(t, m, KeyCtrlCRune, tea.ModCtrl)
	if m2.stop != stopFirstCtrlC {
		t.Fatalf("stop = %d, want first-ctrl-c", m2.stop)
	}
	if !rec.hasExecuted(app.PauseWorkflowCommand{}) {
		t.Fatalf("the first Ctrl+C did not request the controlled pause: %v", rec.executed)
	}

	select {
	case msgs := <-done:
		if !hasRunnerStopped(msgs, foreground.StopCancelled) {
			t.Fatalf("the runner did not stop with StopCancelled: %#v", msgs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner was not cancelled by the first Ctrl+C")
	}
}

// TestModelCtrlCSecondForceStopCleansUp: the second Ctrl+C escalates the
// controlled stop to the force-kill phase (EscalateStop), quits, and
// cleans the runner ownership state.
func TestModelCtrlCSecondForceStopCleansUp(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	rec := &recordingController{ctrl: ctrl}
	m := load(t, testModel(rec))
	m.selected = "wf-1"

	m, _ = m.startRunner()
	m2 := press(t, m, KeyCtrlCRune, tea.ModCtrl)
	if m2.stop != stopFirstCtrlC {
		t.Fatalf("stop = %d, want first-ctrl-c", m2.stop)
	}
	m3, quitCmd := m2.Update(tea.KeyPressMsg{Code: KeyCtrlCRune, Mod: tea.ModCtrl})
	if rec.escalated != 1 {
		t.Fatalf("the second Ctrl+C did not escalate: %d", rec.escalated)
	}
	if quitCmd == nil {
		t.Fatal("the second Ctrl+C did not quit")
	}
	mm := m3.(Model)
	if mm.running || mm.runCancel != nil || mm.eventCh != nil {
		t.Fatalf("the second Ctrl+C left runner ownership: running=%v runCancel=%v eventCh=%v",
			mm.running, mm.runCancel != nil, mm.eventCh != nil)
	}
}

// TestRunnerDoneClearsEventChannel pins the applyRunnerDone terminal path:
// a runner error resets running, runCancel, AND the event channel (no
// stale subscription field is left on the Model).
func TestRunnerDoneClearsEventChannel(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.selected = "wf-1"

	m, runCmd := m.startRunner()
	m = runCmds(t, m, runCmd)
	if m.running || m.runCancel != nil || m.eventCh != nil {
		t.Fatalf("runner done left ownership: running=%v runCancel=%v eventCh=%v",
			m.running, m.runCancel != nil, m.eventCh != nil)
	}
}

// TestRunnerRendererErrorClearsOwnership: a renderer failure (an error
// message) never leaves a background Runner — the ownership state is
// cleared (design §16: the Runtime is unchanged; the user recovers
// through the Headless CLI).
func TestRunnerRendererErrorClearsOwnership(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.selected = "wf-1"

	m, _ = m.startRunner()
	m2 := step(t, m, model.InvalidInputFault("renderer exploded"))
	if m2.running || m2.runCancel != nil || m2.eventCh != nil {
		t.Fatalf("renderer error left runner ownership: running=%v runCancel=%v eventCh=%v",
			m2.running, m2.runCancel != nil, m2.eventCh != nil)
	}
}

// TestPumpEventsNilChannel is the nil-channel regression test: after a
// terminal path clears eventCh (applyRunnerDone, clearRunner), a pump
// command that is still in flight must terminate with eventsClosedMsg
// instead of blocking forever on a nil channel (a leaked goroutine). The
// timeout guard turns the pre-fix hang into a test failure rather than
// freezing the suite.
func TestPumpEventsNilChannel(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := testModel(&recordingController{ctrl: ctrl})
	m.eventCh = nil

	cmd := m.pumpEvents()
	if cmd == nil {
		t.Fatal("pumpEvents returned a nil command")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if _, ok := msg.(eventsClosedMsg); !ok {
			t.Fatalf("pumpEvents on a nil channel yielded %T, want eventsClosedMsg", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pumpEvents on a nil channel hung (blocked on the nil channel)")
	}
}

// TestRunnerEventInactiveNoRepump is the stale-event regression test: a
// runnerEventMsg arriving after the runner is no longer active (the
// renderer-error and the runner's event send come from different
// goroutines, so an in-flight event can arrive after clearRunner) must be
// applied to the Execution page but MUST NOT re-pump — re-pumping there
// captures the cleared nil channel and leaks the pump goroutine.
func TestRunnerEventInactiveNoRepump(t *testing.T) {
	m := testModel(&recordingController{ctrl: &executionController{runtime: model.RuntimeRunning}})
	m.running = false
	m.eventCh = nil

	upd, cmd := m.Update(runnerEventMsg{ev: model.Event{Seq: 1, Kind: model.EventRunStarted, Workflow: "wf-1", Text: "run started"}})
	if cmd != nil {
		t.Fatalf("runnerEventMsg while the runner is inactive re-pumped: cmd = %v", cmd)
	}
	mm := upd.(Model)
	if mm.running {
		t.Fatal("runnerEventMsg while inactive set the running flag")
	}
	if len(mm.execution.Log) != 1 {
		t.Fatalf("inactive runnerEventMsg was not applied to the Execution page: log = %v", mm.execution.Log)
	}
}

// TestRunnerEventActiveRepumps guards the normal-flow counterpart: while
// the runner IS active, a runnerEventMsg still re-pumps (the Execution
// page keeps consuming committed events until the runner terminal path).
func TestRunnerEventActiveRepumps(t *testing.T) {
	ch := make(chan model.Event, 1)
	ch <- model.Event{Seq: 2, Kind: model.EventNodeSucceeded, Workflow: "wf-1", Node: "n1"}
	m := testModel(&recordingController{ctrl: &executionController{runtime: model.RuntimeRunning}})
	m.running = true
	m.eventCh = ch

	upd, cmd := m.Update(runnerEventMsg{ev: model.Event{Seq: 1, Kind: model.EventRunStarted, Workflow: "wf-1", Text: "run started"}})
	if cmd == nil {
		t.Fatal("runnerEventMsg while active did not re-pump")
	}
	mm := upd.(Model)
	if !mm.running {
		t.Fatal("runnerEventMsg while active cleared the running flag")
	}
	// The re-pump command reads the next committed event.
	if msg := cmd(); msg == nil {
		t.Fatal("the re-pump command produced no message")
	}
}

// createWithDiscussion drives the requirement discussion setup through
// the Application: create, prepare the native session (managed bootstrap
// binds the Provider's own session id), the Bridge return persists the
// process exit facts and moves the Session to INTERACTIVE_IDLE, freeze the
// Change Set, and finish with the managed structured resume producing the
// strict handoff. Returns the workflow id.
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
	if prep.Native == nil {
		fx.t.Fatal("prepare carried no native bridge request")
	}
	// The Bridge return revalidates the binding and moves the Session to
	// INTERACTIVE_IDLE.
	if _, err := a.Execute(ctx, app.NativeDiscussionReturnCommand{
		Workflow: wf, Session: prep.SessionID,
		Exit:            process.Exit{Code: 0, Fact: process.FactProcessExit},
		Provider:        "fake",
		ProviderSession: prep.Native.ProviderSession,
	}); err != nil {
		fx.t.Fatalf("native discussion return: %v", err)
	}
	frozen, err := a.Execute(ctx, app.FreezeDiscussionCommand{Workflow: wf, Session: prep.SessionID})
	if err != nil {
		fx.t.Fatalf("freeze: %v", err)
	}
	_ = frozen.ChangeSet.Ref
	// Finish drives the managed structured resume on the same provider
	// session that produces the strict handoff from the user's decisions.
	if _, err := a.Execute(ctx, app.FinishDiscussionCommand{
		Workflow: wf, Session: prep.SessionID,
		Decisions: []byte(`{` + handoffContentFields + `}`),
	}); err != nil {
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

// ---------------------------------------------------------------------------
// Return Page: Continue Same Session / Switch Agent launch the native bridge
// ---------------------------------------------------------------------------

// nativeReturnController is the Return Page seam: Execute returns an Outcome
// that carries the managed Native Bridge request facts for the native
// discussion commands, so applyCommand can launch the blocking-exec adapter.
// With native=false the Outcome carries no Native request (the reload
// fallback case).
type nativeReturnController struct {
	executed []app.Command
	native   bool
}

func (c *nativeReturnController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	out := app.Outcome{Workflow: "wf-1"}
	if c.native {
		out.SessionID = "sess-1"
		out.Native = &app.NativeBridgeRequest{
			Workflow:        "wf-1",
			Session:         "sess-1",
			Provider:        "fake",
			ProviderSession: "prov-sess-1",
			Worktree:        "/cflow/projects/p/wf-1/workspace",
		}
	}
	return out, nil
}

func (c *nativeReturnController) Query(_ context.Context, q app.Query) (app.View, error) {
	if _, ok := q.(app.ProjectWorkspaceQuery); !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	return app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimePending}},
		Lifecycle: &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePending}},
		Health: app.HealthView{GitAvailable: true, Providers: []app.ProviderHealth{
			{Name: "fake", Compatible: true},
			{Name: "alt", Compatible: true},
		}},
	}, nil
}

func (*nativeReturnController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}

func (*nativeReturnController) EscalateStop() {}

// returnPage builds the root Model on the native discussion Return Page
// with the full action set (Continue is the default selection).
func returnPage(t *testing.T, ctrl controller) Model {
	t.Helper()
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m.selected = "wf-1"
	m.page = PageDiscussion
	m.discussion = DiscussionPage{
		Loaded:   true,
		Session:  "sess-1",
		Provider: "fake",
		Actions:  []DiscussionReturnAction{ReturnContinue, ReturnFinish, ReturnSwitch, ReturnPause, ReturnCancel},
	}
	return m
}

// activateReturnAction presses Enter on the current Return action and runs
// the issued typed Application Command to its commandDoneMsg, then returns
// the command applyCommand produced for that result — WITHOUT running it, so
// the test can assert whether it is the native bridge exec or the reload.
func activateReturnAction(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	upd, keyCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = upd.(Model)
	if keyCmd == nil {
		t.Fatal("Enter on the Return page produced no command")
	}
	doneMsg := keyCmd()
	done, ok := doneMsg.(commandDoneMsg)
	if !ok {
		t.Fatalf("Enter on the Return page produced %T, want commandDoneMsg", doneMsg)
	}
	m2, execCmd := m.Update(done)
	return m2.(Model), execCmd
}

// isNativeExecMsg reports whether the command is the Bubble Tea blocking-exec
// adapter of the Native Session Bridge: calling it yields the tea.execMsg the
// Program consumes to suspend the renderer and attach the terminal streams.
func isNativeExecMsg(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	msg := cmd()
	typ := reflect.TypeOf(msg)
	return typ != nil && typ.Name() == "execMsg"
}

// TestModelReturnContinueLaunchesNativeExec proves the Continue Same Session
// action (the default-selected Return action) launches the native bridge
// exec: after the controller's Execute carries the managed Native request,
// applyCommand must produce the blocking-exec adapter (never a plain reload
// that leaves the re-armed Session's RUNNING process phantom).
func TestModelReturnContinueLaunchesNativeExec(t *testing.T) {
	ctrl := &nativeReturnController{native: true}
	m := returnPage(t, ctrl)
	if len(m.discussion.Actions) == 0 || m.discussion.Actions[0] != ReturnContinue {
		t.Fatalf("Continue is not the default Return action: %+v", m.discussion.Actions)
	}
	_, execCmd := activateReturnAction(t, m)
	if len(ctrl.executed) != 1 {
		t.Fatalf("Enter on Continue executed %v", ctrl.executed)
	}
	if _, ok := ctrl.executed[0].(app.ContinueNativeDiscussionCommand); !ok {
		t.Fatalf("Continue command type = %T", ctrl.executed[0])
	}
	if !isNativeExecMsg(execCmd) {
		t.Fatal("Continue did not launch the native bridge exec")
	}
}

func TestPreferredProviderUsesConfiguredClaudeBeforeFake(t *testing.T) {
	got := preferredProvider([]app.ProviderHealth{
		{Name: "claude", Compatible: true},
		{Name: "fake", Compatible: true},
	})
	if got != "claude" {
		t.Fatalf("preferredProvider = %q, want claude", got)
	}
}

func TestPreferredProviderFallsBackToFake(t *testing.T) {
	got := preferredProvider([]app.ProviderHealth{
		{Name: "claude", Compatible: false},
		{Name: "fake", Compatible: true},
	})
	if got != "fake" {
		t.Fatalf("preferredProvider = %q, want fake", got)
	}
}

// TestModelReturnSwitchLaunchesNativeExec proves the Switch Agent action also
// launches the native bridge exec (the switched Session's interactive turn
// must run in the terminal, exactly like the Prepare case).
func TestModelReturnSwitchLaunchesNativeExec(t *testing.T) {
	ctrl := &nativeReturnController{native: true}
	m := returnPage(t, ctrl)
	m = press(t, m, tea.KeyDown, 0) // Finish
	m = press(t, m, tea.KeyDown, 0) // Switch Agent
	if m.discussion.Actions[m.discussion.Selected] != ReturnSwitch {
		t.Fatalf("selected Return action = %v, want switch-agent", m.discussion.Actions[m.discussion.Selected])
	}
	_, execCmd := activateReturnAction(t, m)
	if len(ctrl.executed) != 1 {
		t.Fatalf("Enter on Switch executed %v", ctrl.executed)
	}
	if _, ok := ctrl.executed[0].(app.SwitchAgentCommand); !ok {
		t.Fatalf("Switch command type = %T", ctrl.executed[0])
	}
	if !isNativeExecMsg(execCmd) {
		t.Fatal("Switch did not launch the native bridge exec")
	}
}

// TestModelReturnContinueWithoutNativeReloads guards the fallback: when the
// command outcome carries no Native bridge request, applyCommand must fall
// through to the plain projection reload — never launch an exec with a nil
// request and never wedge the Session.
func TestModelReturnContinueWithoutNativeReloads(t *testing.T) {
	ctrl := &nativeReturnController{native: false}
	m := returnPage(t, ctrl)
	_, execCmd := activateReturnAction(t, m)
	if execCmd == nil {
		t.Fatal("Continue without a native request produced no reload")
	}
	if isNativeExecMsg(execCmd) {
		t.Fatal("Continue without a native bridge request launched the exec")
	}
	msg := execCmd()
	if _, ok := msg.(tea.BatchMsg); !ok {
		t.Fatalf("Continue fallback produced %T, want the projection reload batch", msg)
	}
}
