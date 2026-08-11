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

	store, err := a.artifactStore(wf)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	if _, err := store.Put(context.Background(), artifact.PutRequest{
		WorkflowID: wf, Type: model.ArtifactReport, Revision: 1,
		SchemaVersion: "1.0.0", CreatedAt: time.Unix(1700000000, 0).UTC().Format(time.RFC3339),
		Producer: artifact.ProducerRef{Purpose: "completion"}, Body: []byte("# Final Report\n"),
	}); err != nil {
		t.Fatalf("write report fact: %v", err)
	}
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
	store, err := a.artifactStore(wf)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	ref, err := store.Put(context.Background(), artifact.PutRequest{
		WorkflowID: wf, Type: model.ArtifactReport, Revision: 1,
		SchemaVersion: "1.0.0", CreatedAt: time.Unix(1700000000, 0).UTC().Format(time.RFC3339),
		Producer: artifact.ProducerRef{Purpose: "preflight"}, Body: []byte(`{"probe":"PASS"}`),
	})
	if err != nil {
		t.Fatalf("write preflight report fact: %v", err)
	}
	db, err := sql.Open("sqlite", a.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO git_commit_preflights
		(id, workflow_id, revision, repository_context, git_version,
		 commit_policy_fingerprint, identity_json, signing_policy_json,
		 probe_status, artifact_path, artifact_sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"menu-preflight-1", wf, ref.Revision, "repository:test", "git-test", "fingerprint",
		`{}`, `{}`, "PASS", ref.String(), ref.Hash, time.Unix(1700000000, 0).UTC().Format(time.RFC3339))
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatalf("record preflight fact: insert = %v, close = %v", err, closeErr)
	}
	if _, ok := menuEntry(queryWorkflowMenu(t, a, wf), "final-report"); ok {
		t.Fatal("commit preflight evidence was exposed as a Final Report")
	}
	setWorkflowStageRuntime(t, a, wf, model.StageCompleted, model.RuntimeSucceeded)
	if _, ok := menuEntry(queryWorkflowMenu(t, a, wf), "final-report"); ok {
		t.Fatal("completed state with only preflight evidence was exposed as a Final Report")
	}
}
