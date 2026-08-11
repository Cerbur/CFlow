package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
)

func queryWorkflowMenu(t *testing.T, a *Application, wf model.WorkflowID) WorkflowMenuView {
	t.Helper()
	view, err := a.Query(context.Background(), WorkflowMenuQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("workflow menu query: %v", err)
	}
	return view.(WorkflowMenuView)
}

func menuEntry(menu WorkflowMenuView, id string) (WorkflowMenuEntry, bool) {
	for _, entry := range menu.Entries {
		if entry.ID == id {
			return entry, true
		}
	}
	return WorkflowMenuEntry{}, false
}

func requireMenuAction(t *testing.T, menu WorkflowMenuView, action MenuAction) WorkflowMenuEntry {
	t.Helper()
	for _, entry := range menu.Entries {
		if entry.Kind == MenuEntryAction && entry.Action == action {
			return entry
		}
	}
	t.Fatalf("menu has no action %v: %+v", action, menu.Entries)
	return WorkflowMenuEntry{}
}

type menuEntryExpectation struct {
	id    string
	group MenuGroup
}

func requireExactMenu(t *testing.T, menu WorkflowMenuView, want []menuEntryExpectation, defaultID string) {
	t.Helper()
	if len(menu.Entries) != len(want) {
		t.Fatalf("entry count = %d, want %d: %+v", len(menu.Entries), len(want), menu.Entries)
	}
	for i, expected := range want {
		entry := menu.Entries[i]
		if entry.ID != expected.id || entry.Group != expected.group {
			t.Fatalf("entry[%d] = %s/group %d, want %s/group %d; menu = %+v",
				i, entry.ID, entry.Group, expected.id, expected.group, menu.Entries)
		}
	}
	if menu.DefaultIndex < 0 || menu.DefaultIndex >= len(menu.Entries) {
		t.Fatalf("default index = %d outside %d entries", menu.DefaultIndex, len(menu.Entries))
	}
	if got := menu.Entries[menu.DefaultIndex].ID; got != defaultID {
		t.Fatalf("default entry = %s at %d, want %s; menu = %+v", got, menu.DefaultIndex, defaultID, menu.Entries)
	}
}

func setWorkflowStageRuntime(t *testing.T, a *Application, wf model.WorkflowID, stage model.WorkflowStage, runtime model.RuntimeStatus) {
	t.Helper()
	db, err := sql.Open("sqlite", a.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result, err := db.Exec(`UPDATE workflows SET stage = ?, runtime_status = ? WHERE id = ?`, stage, runtime, wf)
	if err != nil {
		t.Fatalf("update workflow state: %v", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("updated rows = %d, err = %v, want 1", rows, err)
	}
}

func putMenuReport(t *testing.T, a *Application, wf model.WorkflowID, revision int, purpose string) model.ArtifactRef {
	t.Helper()
	artifacts, err := a.artifactStore(wf)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	body := []byte("# Final Report\n")
	if purpose == "preflight" {
		body = []byte(`{"probe":"PASS"}`)
	}
	ref, err := artifacts.Put(context.Background(), artifact.PutRequest{
		WorkflowID: wf, Type: model.ArtifactReport, Revision: revision,
		SchemaVersion: "1.0.0", CreatedAt: time.Unix(1700000000, 0).UTC().Format(time.RFC3339),
		Producer: artifact.ProducerRef{Purpose: purpose}, Body: body,
	})
	if err != nil {
		t.Fatalf("write %s report fact: %v", purpose, err)
	}
	return ref
}

func recordMenuPreflight(t *testing.T, a *Application, wf model.WorkflowID, id string, ref model.ArtifactRef) {
	t.Helper()
	db, err := sql.Open("sqlite", a.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO git_commit_preflights
		(id, workflow_id, revision, repository_context, git_version,
		 commit_policy_fingerprint, identity_json, signing_policy_json,
		 probe_status, artifact_path, artifact_sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, wf, ref.Revision, "repository:test", "git-test", "fingerprint",
		`{}`, `{}`, "PASS", ref.String(), ref.Hash, time.Unix(1700000000, 0).UTC().Format(time.RFC3339))
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatalf("record preflight fact: insert = %v, close = %v", err, closeErr)
	}
}

func TestWorkflowMenuRequiresExplicitWorkflow(t *testing.T) {
	fx := newPlanningFixture(t)
	a := fx.app()
	_, err := a.Query(context.Background(), WorkflowMenuQuery{})
	if code, ok := model.CodeOf(err); !ok || code != model.CodeInvalidInput {
		t.Fatalf("empty workflow error = %v, want INVALID_INPUT", err)
	}
}

func TestWorkflowMenuNewDiscussionStartsFromAuthoritativeReturnFacts(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("menu-new-discussion", false)
	if err != nil {
		t.Fatal(err)
	}

	menu := queryWorkflowMenu(t, fx.app(), wf)
	if menu.Workflow != wf || menu.Name != "menu-new-discussion" {
		t.Fatalf("menu identity = %s/%q, want %s/%q", menu.Workflow, menu.Name, wf, "menu-new-discussion")
	}
	if len(menu.Entries) == 0 || menu.DefaultIndex < 0 || menu.DefaultIndex >= len(menu.Entries) {
		t.Fatalf("invalid menu/default index: %+v", menu)
	}
	if got := menu.Entries[menu.DefaultIndex].Action; got != MenuActionStartDiscussion {
		t.Fatalf("default action = %v, want StartDiscussion", got)
	}
	current, ok := menuEntry(menu, "current-stage")
	if !ok || current.Kind != MenuEntryReadonly || current.Route != MenuRouteCurrentStage {
		t.Fatalf("current stage entry = %+v, present = %v", current, ok)
	}
	if _, ok := menuEntry(menu, "event-log"); !ok {
		t.Fatalf("created workflow has no Event Log entry: %+v", menu.Entries)
	}
	if _, ok := menuEntry(menu, "plan"); ok {
		t.Fatalf("workflow without a Plan exposed Plan/Evidence: %+v", menu.Entries)
	}
	if _, ok := menuEntry(menu, "final-report"); ok {
		t.Fatalf("workflow without a Report exposed Final Report: %+v", menu.Entries)
	}
}

func TestWorkflowMenuBoundDiscussionContinuesFromAuthoritativeReturnFacts(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("menu-bound-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	_, a := fx.prepareNative(t, wf, "provider-menu-session")

	menu := queryWorkflowMenu(t, a, wf)
	if got := menu.Entries[menu.DefaultIndex].Action; got != MenuActionContinueDiscussion {
		t.Fatalf("default action = %v, want ContinueDiscussion", got)
	}
	requireMenuAction(t, menu, MenuActionContinueDiscussion)
	for _, entry := range menu.Entries {
		if entry.Action == MenuActionStartDiscussion {
			t.Fatalf("bound resumable Session exposed StartDiscussion: %+v", entry)
		}
	}
}

func TestWorkflowMenuContainsOnlyProjectedActions(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("menu-paused-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	_, a := fx.prepareNative(t, wf, "provider-paused-session")
	if _, err := a.Execute(context.Background(), PauseWorkflowCommand{Workflow: wf}); err != nil {
		t.Fatalf("pause workflow: %v", err)
	}

	menu := queryWorkflowMenu(t, a, wf)
	if got := menu.Entries[menu.DefaultIndex].Action; got != MenuActionResume && got != MenuActionContinueDiscussion {
		t.Fatalf("default entry = %+v", menu.Entries[menu.DefaultIndex])
	}
	for _, entry := range menu.Entries {
		if entry.Kind == MenuEntryAction && (entry.Action == MenuActionApply || entry.Action == MenuActionCleanup) {
			t.Fatalf("illegal action entry appeared: %+v", entry)
		}
	}
}

func TestMenuProjectionBlockedRuntimeDefaultsToInspect(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("menu-blocked", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app()
	setWorkflowStageRuntime(t, a, wf, model.StageRequirementDiscussion, model.RuntimeBlocked)

	menu := queryWorkflowMenu(t, a, wf)
	if got := menu.Entries[menu.DefaultIndex].Action; got != MenuActionInspectBlocked {
		t.Fatalf("blocked default action = %v, want InspectBlocked", got)
	}
	requireMenuAction(t, menu, MenuActionInspectBlocked)
}

func TestMenuProjectionPlanRouteWithAndWithoutCheckEvidence(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("menu-plan", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app()
	if _, ok := menuEntry(queryWorkflowMenu(t, a, wf), "plan"); ok {
		t.Fatal("Plan/Evidence appeared before an active Plan existed")
	}

	a = fx.app(planScript("menu-plan", validPlan()))
	if _, err := a.Execute(context.Background(), GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	draftMenu := queryWorkflowMenu(t, a, wf)
	plan, ok := menuEntry(draftMenu, "plan")
	if !ok || plan.Route != MenuRoutePlan || plan.Kind != MenuEntryReadonly {
		t.Fatalf("draft Plan entry = %+v, present = %v", plan, ok)
	}

	a = fx.app(checkScript("menu-check", "pass"))
	if _, err := a.Execute(context.Background(), CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("check plan: %v", err)
	}
	checkedMenu := queryWorkflowMenu(t, a, wf)
	if _, ok := menuEntry(checkedMenu, "plan"); !ok {
		t.Fatalf("checked Plan lost Plan/Evidence route: %+v", checkedMenu.Entries)
	}
}

func TestMenuProjectionCompletedWorkflowRequiresReportFact(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("menu-completed", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app()
	setWorkflowStageRuntime(t, a, wf, model.StageCompleted, model.RuntimeSucceeded)
	if _, ok := menuEntry(queryWorkflowMenu(t, a, wf), "final-report"); ok {
		t.Fatal("Final Report appeared without an Artifact fact")
	}

	putMenuReport(t, a, wf, 1, "completion")
	menu := queryWorkflowMenu(t, a, wf)
	report, ok := menuEntry(menu, "final-report")
	if !ok || report.Kind != MenuEntryReadonly || report.Route != MenuRouteReport {
		t.Fatalf("Final Report entry = %+v, present = %v", report, ok)
	}
}

func TestMenuProjectionPreflightReportIsNotFinalReport(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("menu-preflight", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app(planScript("menu-preflight-plan", validPlan()))
	if _, err := a.Execute(context.Background(), GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("generate Plan fact: %v", err)
	}
	ref := putMenuReport(t, a, wf, 1, "preflight")
	recordMenuPreflight(t, a, wf, "menu-preflight-1", ref)
	if _, ok := menuEntry(queryWorkflowMenu(t, a, wf), "final-report"); ok {
		t.Fatal("commit preflight evidence was exposed as a Final Report")
	}
	setWorkflowStageRuntime(t, a, wf, model.StageCompleted, model.RuntimeSucceeded)
	if _, ok := menuEntry(queryWorkflowMenu(t, a, wf), "final-report"); ok {
		t.Fatal("completed state with only preflight evidence was exposed as a Final Report")
	}
}

func TestMenuProjectionCompletionReportSurvivesNewPreflight(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("menu-completion-shadow", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app(planScript("menu-completion-shadow-plan", validPlan()))
	if _, err := a.Execute(context.Background(), GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("generate Plan fact: %v", err)
	}
	initialPreflight := putMenuReport(t, a, wf, 1, "preflight")
	recordMenuPreflight(t, a, wf, "menu-shadow-preflight-1", initialPreflight)
	setWorkflowStageRuntime(t, a, wf, model.StageCompleted, model.RuntimeSucceeded)
	putMenuReport(t, a, wf, 2, "completion")
	laterPreflight := putMenuReport(t, a, wf, 3, "preflight")
	recordMenuPreflight(t, a, wf, "menu-shadow-preflight-3", laterPreflight)

	menu := queryWorkflowMenu(t, a, wf)
	if _, ok := menuEntry(menu, "final-report"); !ok {
		t.Fatalf("later preflight shadowed the immutable completion report: %+v", menu.Entries)
	}
}

func TestMenuProjectionEntryOrderAndDefaults(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(*testing.T) WorkflowMenuView
		want      []menuEntryExpectation
		defaultID string
	}{
		{
			name: "new",
			setup: func(t *testing.T) WorkflowMenuView {
				fx := newPlanningFixture(t)
				wf, err := fx.create("menu-order-new", false)
				if err != nil {
					t.Fatal(err)
				}
				return queryWorkflowMenu(t, fx.app(), wf)
			},
			want: []menuEntryExpectation{
				{id: "start-discussion", group: MenuGroupContinue},
				{id: "current-stage", group: MenuGroupView},
				{id: "event-log", group: MenuGroupView},
			},
			defaultID: "start-discussion",
		},
		{
			name: "paused",
			setup: func(t *testing.T) WorkflowMenuView {
				fx := newPlanningFixture(t)
				wf, err := fx.create("menu-order-paused", false)
				if err != nil {
					t.Fatal(err)
				}
				_, a := fx.prepareNative(t, wf, "provider-order-paused")
				if _, err := a.Execute(context.Background(), PauseWorkflowCommand{Workflow: wf}); err != nil {
					t.Fatalf("pause workflow: %v", err)
				}
				return queryWorkflowMenu(t, a, wf)
			},
			want: []menuEntryExpectation{
				{id: "resume", group: MenuGroupContinue},
				{id: "continue-discussion", group: MenuGroupContinue},
				{id: "current-stage", group: MenuGroupView},
				{id: "event-log", group: MenuGroupView},
			},
			defaultID: "resume",
		},
		{
			name: "blocked",
			setup: func(t *testing.T) WorkflowMenuView {
				fx := newPlanningFixture(t)
				wf, err := fx.create("menu-order-blocked", false)
				if err != nil {
					t.Fatal(err)
				}
				a := fx.app()
				setWorkflowStageRuntime(t, a, wf, model.StageRequirementDiscussion, model.RuntimeBlocked)
				return queryWorkflowMenu(t, a, wf)
			},
			want: []menuEntryExpectation{
				{id: "inspect-blocked", group: MenuGroupContinue},
				{id: "start-discussion", group: MenuGroupContinue},
				{id: "current-stage", group: MenuGroupView},
				{id: "event-log", group: MenuGroupView},
			},
			defaultID: "inspect-blocked",
		},
		{
			name: "running",
			setup: func(t *testing.T) WorkflowMenuView {
				fx := newPlanningFixture(t)
				wf, err := fx.create("menu-order-running", false)
				if err != nil {
					t.Fatal(err)
				}
				_, a := fx.prepareNative(t, wf, "provider-order-running")
				return queryWorkflowMenu(t, a, wf)
			},
			want: []menuEntryExpectation{
				{id: "continue-discussion", group: MenuGroupContinue},
				{id: "current-stage", group: MenuGroupView},
				{id: "event-log", group: MenuGroupView},
				{id: "pause", group: MenuGroupControl},
				{id: "cancel", group: MenuGroupControl},
			},
			defaultID: "continue-discussion",
		},
		{
			name: "completed",
			setup: func(t *testing.T) WorkflowMenuView {
				fx := newPlanningFixture(t)
				wf, err := fx.create("menu-order-completed", false)
				if err != nil {
					t.Fatal(err)
				}
				a := fx.app()
				setWorkflowStageRuntime(t, a, wf, model.StageCompleted, model.RuntimeSucceeded)
				putMenuReport(t, a, wf, 1, "completion")
				return queryWorkflowMenu(t, a, wf)
			},
			want: []menuEntryExpectation{
				{id: "current-stage", group: MenuGroupView},
				{id: "event-log", group: MenuGroupView},
				{id: "final-report", group: MenuGroupView},
				{id: "pause", group: MenuGroupControl},
				{id: "cancel", group: MenuGroupControl},
			},
			defaultID: "pause",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireExactMenu(t, tt.setup(t), tt.want, tt.defaultID)
		})
	}
}

func TestMenuProjectionExecutionFactsGateReadonlyRoutes(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := drivePlanningToApproval(t, fx)
	preview := driveToExecutionGate(t, fx, wf)
	approveExecution(t, fx, wf, preview)
	a := fx.app()
	seedNodeRow(t, a.dbPath, wf, "menu-task", "menu-task", "agent-task", "PENDING", "")
	menu := queryWorkflowMenu(t, a, wf)

	for _, id := range []string{"plan", "specs", "catalog", "workflow-dag", "task-graph"} {
		entry, ok := menuEntry(menu, id)
		if !ok || entry.Kind != MenuEntryReadonly || entry.Group != MenuGroupView {
			t.Fatalf("readonly fact %q = %+v, present = %v; menu = %+v", id, entry, ok, menu.Entries)
		}
	}
}
