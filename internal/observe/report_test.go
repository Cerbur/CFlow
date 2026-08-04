// The Final Report read-model tests (Task 18, design 21, PRD 最终报告示
// 例): the report is a read model over approved hashes, Git facts,
// Sessions, Attempts, Findings, verification manifests, migration
// compatibility, security posture, permissions, and Apply state; report
// generation never changes Workflow state; every free-form value passes
// through the Redactor; a stale approved Artifact hash is reported
// STALE; an incomplete Finding renders safely; the trust boundary is
// disclosed; Apply renders as not run until Gate 3; and the Event export
// is byte-stable.
package observe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

var reportNow = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

func reportBuild() BuildInfo {
	return BuildInfo{Version: "0.0.0-test", SourceCommit: "abcd1234",
		GoVersion: "go1.26.5", OS: "darwin", Arch: "arm64"}
}

// reportInputFixture is a completed Workflow read model input: every
// delivery chain Node SUCCEEDED, the Execution Approval bound the active
// artifact hashes, one verification manifest and review session exist,
// and the workflow completed without changing the Target Branch.
func reportInputFixture() ReportInput {
	st := model.State{
		Now: reportNow,
		Workflow: model.Workflow{
			ID: "wf-1", Project: "p-1", Stage: model.StageCompleted,
			Runtime: model.RuntimeSucceeded, TargetBranch: "main",
			BaseCommit: "base-1", IntegrationBranch: "cflow/wf-1/integration",
			IntegrationHead: "int-2",
			ExecutionFacts: &model.ExecutionFacts{
				PlanHash: "plan-h", SpecHashes: []string{"spec-1"}, CatalogHash: "cat-1",
				WorkflowHash: "wf-1", RoutingHash: "r-1", BudgetHash: "b-1",
				CommitPolicyHash: "cp-1", Fingerprint: "fp-1",
				SpecRevision: 1, CatalogRevision: 1, WorkflowRevision: 1, PreflightRevision: 2,
			},
		},
		Plan: &model.Plan{Revision: 1, Status: model.PlanApproved,
			Artifact: model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: "plan-h"},
			Hash:     "plan-h"},
		Nodes: map[model.NodeID]*model.Node{},
		Runs: []model.Run{
			{ID: "run-1", Status: model.RunSucceeded, DispatchGate: false,
				StartedAt: reportNow.Add(-2 * time.Hour), EndedAt: reportNow},
		},
		Sessions: []model.Session{
			{ID: "s-impl", Purpose: model.PurposeImplementation, Status: model.SessionCompleted,
				Provider: "codex", ProviderSessionID: "sx-1"},
			{ID: "s-review", Purpose: model.PurposeReview, Status: model.SessionCompleted,
				Provider: "claude", ProviderSessionID: "sx-2"},
			{ID: "s-final", Purpose: model.PurposeFinalVerification, Status: model.SessionCompleted,
				Provider: "claude", ProviderSessionID: "sx-3"},
		},
		Findings:     []model.Finding{},
		NextEventSeq: 42,
	}
	for _, n := range []struct {
		id   string
		kind model.NodeKind
	}{
		{"task-1", model.NodeAgentTask}, {"verify-1", model.NodeVerify}, {"merge-1", model.NodeMerge},
		{"task-2", model.NodeAgentTask}, {"verify-2", model.NodeVerify}, {"merge-2", model.NodeMerge},
		{"final-verify", model.NodeFinalVerify},
	} {
		st.Nodes[model.NodeID(n.id)] = &model.Node{ID: model.NodeID(n.id), Kind: n.kind, Status: model.NodeSucceeded}
	}
	st.Attempts = map[model.AttemptKey]*model.Attempt{}
	keys := []struct {
		node string
		n    int
		head string
		ev   model.EvidenceKind
	}{
		{"task-1", 1, "task-1-head", model.EvidenceCommit},
		{"task-1", 2, "task-1-head-2", model.EvidenceCommit},
		{"merge-1", 1, "int-1", model.EvidenceCommit},
		{"task-2", 1, "task-2-head", model.EvidenceCommit},
		{"merge-2", 1, "int-2", model.EvidenceCommit},
		{"verify-1", 1, "task-1-head-2", model.EvidenceReviewResult},
		{"verify-2", 1, "task-2-head", model.EvidenceReviewResult},
		{"final-verify", 1, "int-2", model.EvidenceReviewResult},
	}
	for _, k := range keys {
		key := model.AttemptKey{Node: model.NodeID(k.node), Number: model.AttemptNumber(k.n)}
		st.Attempts[key] = &model.Attempt{Key: key, Status: model.AttemptSucceeded,
			StartHead: "base-1", EndHead: k.head,
			Evidence:     []model.EvidenceRef{{Kind: k.ev, Hash: k.head, Subject: "cflow/wf-1/integration"}},
			RetryCharged: k.node == "task-1" && k.n == 2,
			StartedAt:    reportNow.Add(-2 * time.Hour), EndedAt: reportNow.Add(-time.Hour)}
	}
	return ReportInput{
		Build:       reportBuild(),
		GeneratedAt: reportNow,
		State:       st,
		ActiveArtifacts: []model.ArtifactRef{
			{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: "plan-h"},
			{Workflow: "wf-1", Type: model.ArtifactSpec, Revision: 1, Hash: "spec-1"},
			{Workflow: "wf-1", Type: model.ArtifactCatalog, Revision: 1, Hash: "cat-1"},
			{Workflow: "wf-1", Type: model.ArtifactWorkflow, Revision: 1, Hash: "wf-1"},
			{Workflow: "wf-1", Type: model.ArtifactRoutingPolicy, Revision: 1, Hash: "r-1"},
			{Workflow: "wf-1", Type: model.ArtifactBudgetPolicy, Revision: 1, Hash: "b-1"},
		},
		VerificationManifests: []model.EvidenceManifest{
			{SchemaVersion: "1.0.0", Node: "verify-1", CommandID: "verify",
				Purpose: "task_verify", Passed: true, Hash: "manifest-1"},
			{SchemaVersion: "1.0.0", Node: "final-verify", CommandID: "final-verify",
				Purpose: "final_verify", Passed: true, Hash: "final-manifest-1"},
		},
		Migration: ReportMigration{
			SchemaVersion: 4, ChecksumsVerified: true,
			Applied: []AppliedMigration{{Version: 1, ID: "cflow-001-initial"},
				{Version: 2, ID: "cflow-002-cleanup-apply"},
				{Version: 3, ID: "cflow-003-integration-head"},
				{Version: 4, ID: "cflow-004-apply-staging-head"}},
			BackupVerified: true,
		},
		Security: ReportSecurity{
			HomeMode: "0700", FileMode: "0600",
			RedactionRevision: "r4", RawFramesPersisted: false,
			AtRestEncryption: "none",
		},
		TrustBoundary: "agents run with the provider's default permissions and the user's existing provider configuration; CFlow provides no sandbox guarantee",
		EventExport: ReportEventExport{
			Path: "/home/cflow/projects/p-1/workflows/wf-1/events.jsonl",
			From: 1, To: 42, Stable: true,
		},
	}
}

func TestGenerateReportReadsCompletedWorkflow(t *testing.T) {
	r, err := GenerateReport(reportInputFixture())
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	if r.Result != "PASSED" {
		t.Fatalf("result = %s, want PASSED", r.Result)
	}
	if r.Summary.Tasks != 2 || r.Summary.Completed != 2 || r.Summary.Retries != 1 || r.Summary.Sessions != 3 {
		t.Fatalf("summary = %+v", r.Summary)
	}
	if r.Workflow.TargetBranch != "main" || r.Workflow.IntegrationHead != "int-2" {
		t.Fatalf("workflow facts = %+v", r.Workflow)
	}
	for _, row := range r.Artifacts {
		if row.Stale {
			t.Fatalf("artifact %s marked stale with matching hashes", row.Type)
		}
	}
	if r.Apply.Status != "NOT_RUN" {
		t.Fatalf("apply status = %s, want NOT_RUN until Gate 3", r.Apply.Status)
	}
	if len(r.Verification) != 2 || !r.Verification[1].Passed {
		t.Fatalf("verification rows = %+v", r.Verification)
	}
}

// TestReportMarksStaleArtifactHash: an active Artifact Revision whose
// hash no longer matches the approved Execution facts is reported STALE,
// never silently treated as the approved reference.
func TestReportMarksStaleArtifactHash(t *testing.T) {
	in := reportInputFixture()
	in.ActiveArtifacts[3] = model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactWorkflow,
		Revision: 1, Hash: "wf-2-drifted"}
	r, err := GenerateReport(in)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	found := false
	for _, row := range r.Artifacts {
		if row.Type == model.ArtifactWorkflow {
			found = true
			if !row.Stale {
				t.Fatalf("workflow artifact not marked stale: %+v", row)
			}
		}
	}
	if !found {
		t.Fatalf("workflow artifact row missing: %+v", r.Artifacts)
	}
	md := strings.ToLower(RenderMarkdown(r, security.Registry{}))
	if !strings.Contains(md, "stale") {
		t.Fatalf("markdown does not disclose the stale artifact:\n%s", md)
	}
}

// TestReportRedactsFreeFormValues: every free-form value passes through
// the Redactor before rendering; the raw secret never reaches the
// output.
func TestReportRedactsFreeFormValues(t *testing.T) {
	in := reportInputFixture()
	in.State.Findings = []model.Finding{{
		ID: "f-1", Code: model.CodeScopeViolation, Scope: model.ScopeAttempt,
		Subject: "task-1", Blocking: true,
		Text: "violation with token sk-ant-abcdef1234567890",
		Seq:  41,
	}}
	reg := security.Registry{Revision: "r4", Rules: []security.Rule{
		{ID: "anthropic-token", Category: "anthropic_token",
			Pattern: `sk-ant-[A-Za-z0-9]+`},
	}}
	r, err := GenerateReport(in)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	md := RenderMarkdown(r, reg)
	if strings.Contains(md, "sk-ant-abcdef1234567890") {
		t.Fatalf("raw secret reached the report output:\n%s", md)
	}
	if !strings.Contains(md, "[REDACTED:anthropic_token]") && !strings.Contains(md, "REDACTED") {
		t.Fatalf("report does not show a redaction placeholder:\n%s", md)
	}
}

// TestReportRendersIncompleteFindingSafely: an incomplete Finding
// (missing Code or Subject) renders with stable placeholders and never
// panics.
func TestReportRendersIncompleteFindingSafely(t *testing.T) {
	in := reportInputFixture()
	in.State.Findings = []model.Finding{{ID: "f-1", Seq: 41}}
	r, err := GenerateReport(in)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	md := RenderMarkdown(r, security.Registry{})
	if !strings.Contains(md, "f-1") {
		t.Fatalf("finding row missing from the report:\n%s", md)
	}
}

// TestReportDisclosesTrustBoundary: the Provider default-permission
// trust boundary (PRD 约束 30) is disclosed in the report, never shown
// as sandboxed.
func TestReportDisclosesTrustBoundary(t *testing.T) {
	in := reportInputFixture()
	in.TrustBoundary = ""
	r, err := GenerateReport(in)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	md := strings.ToLower(RenderMarkdown(r, security.Registry{}))
	if !strings.Contains(md, "trust boundary") || strings.Contains(md, "sandboxed=true") {
		t.Fatalf("trust boundary disclosure missing or falsely sandboxed:\n%s", md)
	}
}

// TestReportRendersMigrationAndSecurityPosture: the State Compatibility
// and Local Data Protection sections report the schema version, verified
// checksums, modes, redaction revision, and raw-frame posture.
func TestReportRendersMigrationAndSecurityPosture(t *testing.T) {
	in := reportInputFixture()
	r, err := GenerateReport(in)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	md := strings.ToLower(RenderMarkdown(r, security.Registry{}))
	for _, want := range []string{
		"schema: 4", "cflow-004-apply-staging-head", "checksums verified",
		"backup verified", "0700", "0600", "r4", "raw provider frames persisted: no",
		"at-rest encryption: none",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("report missing %q:\n%s", want, md)
		}
	}
}

// TestReportShowsApplyAsNotRun: until the Gate 3 protected Apply lands,
// the report shows the Apply outcome as not run, never as applied.
func TestReportShowsApplyAsNotRun(t *testing.T) {
	in := reportInputFixture()
	r, err := GenerateReport(in)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	md := RenderMarkdown(r, security.Registry{})
	if !strings.Contains(md, "not run") {
		t.Fatalf("apply status missing from the report:\n%s", md)
	}
}

// TestReportGenerationNeverChangesWorkflowState: GenerateReport is a
// pure read model; the input State is untouched.
func TestReportGenerationNeverChangesWorkflowState(t *testing.T) {
	in := reportInputFixture()
	before := in.State
	if _, err := GenerateReport(in); err != nil {
		t.Fatalf("generate report: %v", err)
	}
	if in.State.Workflow.Stage != before.Workflow.Stage || in.State.Workflow.Runtime != before.Workflow.Runtime ||
		len(in.State.Findings) != len(before.Findings) || len(in.State.Nodes) != len(before.Nodes) {
		t.Fatalf("report generation mutated the workflow state")
	}
}

// TestReportMarkdownIsDeterministic: identical inputs render byte-identical
// reports.
func TestReportMarkdownIsDeterministic(t *testing.T) {
	in := reportInputFixture()
	reg := security.Registry{Revision: "r4"}
	r1, err := GenerateReport(in)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	r2, err := GenerateReport(in)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	if a, b := RenderMarkdown(r1, reg), RenderMarkdown(r2, reg); a != b {
		t.Fatalf("report rendering is not deterministic")
	}
}

// TestEventExportIsStable: the events.jsonl export is byte-stable — a
// later Event window appends exactly the same records a fresh export of
// that window produces, never reordering, dropping, or re-redacting an
// earlier record, and the raw secret never reaches the file.
func TestEventExportIsStable(t *testing.T) {
	reg := security.Registry{Revision: "r4", Rules: []security.Rule{
		{ID: "anthropic-token", Category: "anthropic_token", Pattern: `sk-ant-[A-Za-z0-9]+`},
	}}
	window1 := []model.Event{
		{Seq: 1, Kind: model.EventWorkflowCreated, Workflow: "wf-1", Code: "", Text: "workflow created", At: reportNow},
		{Seq: 2, Kind: model.EventFindingOpened, Workflow: "wf-1", Code: model.CodeScopeViolation,
			Text: "token sk-ant-abcdef1234567890 leaked", At: reportNow},
		{Seq: 3, Kind: model.EventWorkflowSucceeded, Workflow: "wf-1", Text: "workflow completed", At: reportNow},
	}
	window2 := []model.Event{
		{Seq: 4, Kind: model.EventRunStarted, Workflow: "wf-1", Text: "run started", At: reportNow},
		{Seq: 5, Kind: model.EventRunSucceeded, Workflow: "wf-1", Text: "run succeeded", At: reportNow},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := ExportEvents(path, window1, reg); err != nil {
		t.Fatalf("export window 1: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if strings.Contains(string(first), "sk-ant-abcdef1234567890") {
		t.Fatalf("raw secret reached the event export")
	}
	// A fresh export of the second window is the exact append segment.
	refDir := t.TempDir()
	refPath := filepath.Join(refDir, "events.jsonl")
	if err := ExportEvents(refPath, window2, reg); err != nil {
		t.Fatalf("export reference window: %v", err)
	}
	ref, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatalf("read reference export: %v", err)
	}
	if err := ExportEvents(path, window2, reg); err != nil {
		t.Fatalf("append window 2: %v", err)
	}
	combined, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read export: %v", err)
	}
	want := string(first) + string(ref)
	if string(combined) != want {
		t.Fatalf("event export is not stable:\n%q\n---\nwant\n%q", combined, want)
	}
}

// TestReportSurfacesUnsettledRepairRow (Task 21 ledger obligation (b)): a
// PurposeRepair resolution Session/Process the Apply request path allocated
// but never settled (the phantom-row class) must surface in the report's
// Remaining Risks instead of silently vanishing — the Final Report never
// hides an open row, and the workflow's PASSED result is not contradicted.
func TestReportSurfacesUnsettledRepairRow(t *testing.T) {
	in := reportInputFixture()
	in.State.Sessions = append(in.State.Sessions, model.Session{
		ID: "rs-1", Purpose: model.PurposeRepair, Status: model.SessionStarting, Provider: "fake",
	})
	in.State.Processes = append(in.State.Processes, model.ProcessRecord{
		ID: "rp-1", Session: "rs-1", Purpose: model.PurposeRepair,
		Status: model.ProcessStatusRunning, StartedAt: reportNow,
	})
	r, err := GenerateReport(in)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	if r.Result != "PASSED" {
		t.Fatalf("result = %s, want PASSED", r.Result)
	}
	foundProcess := false
	foundSession := false
	for _, risk := range r.Risks {
		if strings.Contains(risk, "rp-1") && strings.Contains(risk, "RUNNING") {
			foundProcess = true
		}
		if strings.Contains(risk, "rs-1") && strings.Contains(risk, "STARTING") {
			foundSession = true
		}
	}
	if !foundProcess || !foundSession {
		t.Fatalf("the unsettled repair rows are missing from Risks: %+v", r.Risks)
	}
}
