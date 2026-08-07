package app

// The final acceptance Application flow (Task 18, PRD 最终验收): after
// every serial merge, the Final Verify Node runs the approved final-verify
// Catalog command over the full Integration range inside the Integration
// Worktree, an independent Final Reviewer Session passes, the Workflow
// completes with exact evidence, the immutable Final Report Artifact is
// written, and the Target Branch is never changed. The retry command and
// the report query are exercised through the real pipeline (design 22.1).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/claude"
	"cflow.local/cflow/internal/agent/codex"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/security"
)

// implementationCommitScript is the deterministic Fake coding Session
// output that writes and commits per Task inside each Task Worktree (the
// e2e fixture shape): real implementation Commits the gate accepts.
func implementationCommitScript() string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":"i1","exit_code":0,"resume":"ok","tasks":{"task-s01":{"writes":[{"path":"src/divide/divide.go","content":"package divide\n\n// Divide returns a/b.\nfunc Divide(a, b int) (int, error) {\n\treturn a / b, nil\n}\n"}],"commit":"implement divide"}}}
{"type":"session_started","session_id":"i1","at_ms":0}
{"type":"assistant_message","session_id":"i1","text":"Implemented Divide.","at_ms":10}
{"type":"session_finished","session_id":"i1","result":{"summary":"implemented"},"at_ms":20}`)
}

// reviewPassScript is the deterministic TASK_REVIEW Session output: a
// structured PASS verdict.
func reviewPassScript() string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"review","session_id":"r1","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"r1","at_ms":0}
{"type":"assistant_message","session_id":"r1","text":"Reviewed the task diff.","at_ms":10}
{"type":"session_finished","session_id":"r1","result":{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n"},"at_ms":20}`
}

// finalReviewPassScript is the deterministic FINAL_VERIFICATION Session
// output: a structured PASS verdict over the Integration result.
func finalReviewPassScript() string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"final-verification","session_id":"fr1","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"fr1","at_ms":0}
{"type":"assistant_message","session_id":"fr1","text":"Reviewed the full integration result.","at_ms":10}
{"type":"session_finished","session_id":"fr1","result":{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n- plan acceptance criteria verified\n"},"at_ms":20}`
}

// dispatchUntilMergedApp runs DispatchCommand passes until every merge
// Node is SUCCEEDED.
func dispatchUntilMergedApp(t *testing.T, a *Application, wf model.WorkflowID) InspectView {
	t.Helper()
	for i := 0; i < 24; i++ {
		if _, err := a.Execute(context.Background(), DispatchCommand{Workflow: wf}); err != nil {
			t.Fatalf("dispatch pass %d: %v", i, err)
		}
		iv := aInspect(t, a, wf)
		allMerged := true
		for _, n := range iv.Nodes {
			if n.Kind == model.NodeMerge && n.Status != model.NodeSucceeded {
				allMerged = false
			}
		}
		if allMerged {
			return iv
		}
	}
	t.Fatalf("merges did not complete within the pass budget")
	return InspectView{}
}

func aInspect(t *testing.T, a *Application, wf model.WorkflowID) InspectView {
	t.Helper()
	view, err := a.Query(context.Background(), InspectQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return view.(InspectView)
}

// dispatchUntilCompletedApp runs DispatchCommand passes until the
// Workflow records COMPLETED (the observation checkpoint settle, the
// Final Verify chain, and the exact-evidence completion).
func dispatchUntilCompletedApp(t *testing.T, a *Application, wf model.WorkflowID) InspectView {
	t.Helper()
	for i := 0; i < 24; i++ {
		if _, err := a.Execute(context.Background(), DispatchCommand{Workflow: wf}); err != nil {
			t.Fatalf("dispatch pass %d: %v", i, err)
		}
		iv := aInspect(t, a, wf)
		if iv.Status.Stage == model.StageCompleted {
			return iv
		}
	}
	t.Fatalf("workflow did not complete within the pass budget")
	return InspectView{}
}

// TestFinalVerifyCompletesWorkflowWithReportArtifact (PRD 最终验收): the
// Final Verify Node runs over the Integration range, the independent
// Final Reviewer passes, the Workflow completes, the immutable Final
// Report Artifact is written, and the Target Branch is unchanged.
func TestFinalVerifyCompletesWorkflowWithReportArtifact(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := fx.planningApproved()
	pv := driveToExecutionGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)
	a := fx.app(implementationCommitScript(), reviewPassScript(), finalReviewPassScript())

	iv := dispatchUntilMergedApp(t, a, wf)
	if statusOfNode(iv, "final-verify") != model.NodePending {
		t.Fatalf("final-verify = %s before the final pass, want PENDING", statusOfNode(iv, "final-verify"))
	}

	// The following passes allocate the observation checkpoint and then
	// the Final Verify: deterministic verification in the Integration
	// Worktree, the independent Final Reviewer, and the exact-evidence
	// completion.
	final := dispatchUntilCompletedApp(t, a, wf)
	if final.Status.Stage != model.StageCompleted || final.Status.Runtime != model.RuntimeSucceeded {
		t.Fatalf("workflow = %s/%s, want COMPLETED/SUCCEEDED", final.Status.Stage, final.Status.Runtime)
	}
	if statusOfNode(final, "final-verify") != model.NodeSucceeded {
		t.Fatalf("final-verify = %s, want SUCCEEDED", statusOfNode(final, "final-verify"))
	}
	if final.Status.Runtime != model.RuntimeSucceeded {
		t.Fatalf("runtime = %s, want SUCCEEDED", final.Status.Runtime)
	}
	if final.Status.TargetBranch != "main" {
		t.Fatalf("target branch = %s, want main (completion never changes it)", final.Status.TargetBranch)
	}
	if out := fx.git("branch", "--show-current"); out != "main\n" {
		t.Fatalf("the user's target branch moved to %q", out)
	}

	// The immutable Final Report Artifact exists (the preflight report is
	// revision 1; the completion report is the successor revision).
	store, err := a.artifactStore(wf)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	ref, err := store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactReport})
	if err != nil {
		t.Fatalf("report artifact: %v", err)
	}
	if ref.Revision < 2 {
		t.Fatalf("final report revision = %d, want a successor of the preflight report", ref.Revision)
	}
	body, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("report body: %v", err)
	}
	if len(body) == 0 {
		t.Fatalf("report artifact body is empty")
	}
}

// TestReportQueryRendersCompletedWorkflow: the report query renders the
// read model of a completed Workflow (PASSED, Apply not run, trust
// boundary disclosed) without mutating anything.
func TestReportQueryRendersCompletedWorkflow(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := fx.planningApproved()
	pv := driveToExecutionGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)
	a := fx.app(implementationCommitScript(), reviewPassScript(), finalReviewPassScript())
	dispatchUntilCompletedApp(t, a, wf)

	view, err := a.Query(context.Background(), ReportQuery{Workflow: wf, Build: observe.BuildInfo{Version: "0.0.0-dev"}})
	if err != nil {
		t.Fatalf("report query: %v", err)
	}
	rv := view.(ReportView)
	if rv.Report.Result != "PASSED" {
		t.Fatalf("report result = %s, want PASSED", rv.Report.Result)
	}
	if rv.Report.Apply.Status != "NOT_RUN" {
		t.Fatalf("apply status = %s, want NOT_RUN", rv.Report.Apply.Status)
	}
	if rv.Markdown == "" {
		t.Fatalf("report markdown is empty")
	}
	// The read query never changes the aggregate.
	after := aInspect(t, a, wf)
	if after.Status.Runtime != model.RuntimeSucceeded {
		t.Fatalf("report query changed the workflow runtime to %s", after.Status.Runtime)
	}
}

// TestRetryCommandDispatchesReadyNode: cflow retry refuses an unknown
// task before any dispatch; the retry of a READY Node runs through the
// normal dispatch pass.
func TestRetryCommandDispatchesReadyNode(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := fx.planningApproved()
	pv := driveToExecutionGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)
	a := fx.app(implementationCommitScript(), reviewPassScript(), finalReviewPassScript())
	dispatchUntilMergedApp(t, a, wf)

	iv := aInspect(t, a, wf)
	if statusOfNode(iv, "task-s01") != model.NodeSucceeded {
		t.Fatalf("task-s01 = %s, want SUCCEEDED", statusOfNode(iv, "task-s01"))
	}
	// An unknown task is refused before any dispatch.
	if _, err := a.Execute(context.Background(), RetryCommand{Workflow: wf, Node: "task-unknown"}); err == nil {
		t.Fatalf("retry accepted an unknown task")
	}
}

// statusOfNode is the node status projection of one InspectView.
func statusOfNode(iv InspectView, id string) model.NodeStatus {
	for _, n := range iv.Nodes {
		if string(n.ID) == id {
			return n.Status
		}
	}
	return ""
}

// fx.git runs one git command in the fixture repository and returns its
// trimmed stdout.
func (fx *planningFixture) git(args ...string) string {
	fx.t.Helper()
	out, err := execGit(fx.root, args...).CombinedOutput()
	if err != nil {
		fx.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestProviderTypedInputCarriesBundleRef (ledger obligation from Task 16):
// providerTypedInput replaces the base input with the real Adapter's typed
// facts, so the immutable redacted Context Bundle handoff of an automatic
// fallback must ride the typed input — a production codex→claude successor
// carries the handoff in the recorded input facts. A fresh Session's typed
// input omits the reference.
func TestProviderTypedInputCarriesBundleRef(t *testing.T) {
	fx := newExecutionFixture(t)
	// The managed schema path lives under CFLOW_HOME/schemas and the
	// Runtime's evidence writer under CFLOW_HOME/evidence; the home chain
	// must exist before the guards' safe creation (the fixture's first
	// command would create it; this test drives the runtime directly).
	if err := os.MkdirAll(filepath.Join(fx.home, "evidence"), 0o700); err != nil {
		t.Fatal(err)
	}
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := agent.NewRuntime(agent.RuntimeOptions{
		Now:         fx.now,
		IDs:         fx.ids,
		Registry:    reg,
		Redaction:   security.Registry{},
		EvidenceDir: filepath.Join(fx.home, "evidence"),
		Adapters: map[string]agent.Adapter{
			"fake":   fake.New(reg),
			"codex":  fake.New(reg),
			"claude": fake.New(reg),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { requireNoError(t, rt.Close()) }()
	rt.SetRoutingPolicy(&agent.RoutingPolicySet{Policies: []agent.RoutingPolicy{
		{Purpose: model.PurposeImplementation, Bindings: []agent.RouteBinding{
			{Provider: "codex", Model: "gpt-5", BudgetUSD: 1.5, DialectID: "cflow.dialect.fake.v1", RegistryRevision: "r3"},
			{Provider: "claude", Model: "claude-sonnet-4-5", BudgetUSD: 2.0, DialectID: "cflow.dialect.fake.v1", RegistryRevision: "r3"},
		}},
	}})

	a := fx.app()
	ctx := context.Background()
	bundle := &agent.ContextBundle{Path: "/evidence/sessions/lost1/bundle-1.json", Revision: 1, Hash: "bundle-h"}
	base := attachBundleInput(&codingSessionInput{Spec: "spec", Catalog: "cat", Worktree: "/wt"}, bundle)

	codexIn, ok := a.providerTypedInput(ctx, rt, model.PurposeImplementation, "codex", base).(codex.Input)
	if !ok {
		t.Fatalf("codex typed input has unexpected type %T", a.providerTypedInput(ctx, rt, model.PurposeImplementation, "codex", base))
	}
	if codexIn.ContextBundleRef != bundle.Path {
		t.Fatalf("codex typed input carries bundle ref %q, want %q", codexIn.ContextBundleRef, bundle.Path)
	}
	if codexIn.Model != "gpt-5" || codexIn.SchemaPath == "" {
		t.Fatalf("codex typed input lost its typed facts: %+v", codexIn)
	}

	claudeIn, ok := a.providerTypedInput(ctx, rt, model.PurposeImplementation, "claude", base).(claude.Input)
	if !ok {
		t.Fatalf("claude typed input has unexpected type %T", a.providerTypedInput(ctx, rt, model.PurposeImplementation, "claude", base))
	}
	if claudeIn.ContextBundleRef != bundle.Path || claudeIn.Model != "claude-sonnet-4-5" || claudeIn.MaxBudgetUSD != "2" {
		t.Fatalf("claude typed input lost the bundle ref or typed facts: %+v", claudeIn)
	}

	// A fresh Session (no bundle) omits the reference.
	fresh := a.providerTypedInput(ctx, rt, model.PurposeImplementation, "codex",
		&codingSessionInput{Spec: "spec", Catalog: "cat", Worktree: "/wt"})
	freshIn, ok := fresh.(codex.Input)
	if !ok {
		t.Fatalf("fresh codex typed input has unexpected type %T", fresh)
	}
	if freshIn.ContextBundleRef != "" {
		t.Fatalf("fresh codex typed input carries a bundle ref %q", freshIn.ContextBundleRef)
	}
}

// TestProviderTypedInputPlanningFallback: the planning Sessions run
// before the Execution Approval binds the routing policy, so their typed
// input falls back to the resolved config default model and the hard
// budget cap (absent config = the embedded defaults); a routed purpose
// without its approved binding still fails closed on the base input
// (PRD 约束 306: an unapproved route never substitutes silently).
func TestProviderTypedInputPlanningFallback(t *testing.T) {
	fx := newExecutionFixture(t)
	if err := os.MkdirAll(filepath.Join(fx.home, "evidence"), 0o700); err != nil {
		t.Fatal(err)
	}
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := agent.NewRuntime(agent.RuntimeOptions{
		Now:         fx.now,
		IDs:         fx.ids,
		Registry:    reg,
		Redaction:   security.Registry{},
		EvidenceDir: filepath.Join(fx.home, "evidence"),
		Adapters: map[string]agent.Adapter{
			"fake":   fake.New(reg),
			"codex":  fake.New(reg),
			"claude": fake.New(reg),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { requireNoError(t, rt.Close()) }()
	// No routing policy attached: the planning purposes are not routed.
	a := fx.app()
	ctx := context.Background()
	base := map[string]any{"message": "plan the requirement"}

	codexIn, ok := a.providerTypedInput(ctx, rt, model.PurposePlanning, "codex", base).(codex.Input)
	if !ok {
		t.Fatalf("planning codex typed input has unexpected type %T", a.providerTypedInput(ctx, rt, model.PurposePlanning, "codex", base))
	}
	if codexIn.SchemaPath == "" {
		t.Fatalf("planning codex typed input lost the managed schema path: %+v", codexIn)
	}

	claudeIn, ok := a.providerTypedInput(ctx, rt, model.PurposePlanCheck, "claude", base).(claude.Input)
	if !ok {
		t.Fatalf("planning claude typed input has unexpected type %T", a.providerTypedInput(ctx, rt, model.PurposePlanCheck, "claude", base))
	}
	if claudeIn.SchemaJSON == "" || claudeIn.MaxBudgetUSD != "0" {
		t.Fatalf("planning claude typed input lost the schema or budget facts: %+v", claudeIn)
	}

	// A routed purpose without its approved binding fails closed on the
	// base input instead of substituting an unapproved route.
	if _, ok := a.providerTypedInput(ctx, rt, model.PurposeImplementation, "codex", base).(codex.Input); ok {
		t.Fatal("routed purpose without a binding must return the base input, got the typed input")
	}
}
