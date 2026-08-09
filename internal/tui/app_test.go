package tui

// Root TUI Model tests (TUI tasks 9, 10, 14): the Model loads the real
// read-only Workspace projection through the shared Application, page
// navigation reaches every lifecycle page, user actions map to the exact
// typed Application Commands, Enter alone never approves, and the
// controlled-stop protocol executes the real Pause and Force Stop.

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

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
