package app

// Agent Runtime routing tests (Task 16, brief Step 1): the verbatim
// TestBindingDriftStopsBeforeAttemptAllocation, protocol/config drift
// pausing before start without spending Retry, the resume fallback facts
// the Application reports and the automatic successor Attempt the Kernel
// charges, and the user Ctrl+C interruption that never charges. Real
// repositories, real SQLite, the deterministic Fake Adapter plus the real
// codex Adapter over a scripted supervisor (design 22.1).

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/codex"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// assertFaultCode asserts that err is a model Fault carrying exactly code.
func assertFaultCode(t *testing.T, err error, code model.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected fault %s, got nil error", code)
	}
	got, ok := model.CodeOf(err)
	if !ok {
		t.Fatalf("expected fault %s, got a non-fault error: %v", code, err)
	}
	if got != code {
		t.Fatalf("expected fault %s, got %s (%v)", code, got, err)
	}
}

// ---------------------------------------------------------------------------
// approvedRuntimeFixture (brief Step 1): one real repo and CFLOW_HOME; the
// planning phases run on the deterministic Fake Adapter; the Execution Dry
// Run and the dispatch run through an Application that also carries the
// real codex Adapter over a recording Fake Supervisor with a stub codex
// executable on PATH, so the approved routing pin is a real observed
// executable identity that tests can drift.
// ---------------------------------------------------------------------------

// codexRouteSpec is the fixture Spec whose Task routes to the codex
// provider (the drift gate target).
const codexRouteSpec = `{"id":"s01","goal":"implement divide","depends_on":[],"write_scope":["src/divide/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"codex","model":"gpt-5","budget":10},"timeout_seconds":1800,"max_retry":2}`

type approvedRuntime struct {
	*planningFixture
	fa    *process.FakeAdapter
	rec   *startRecorder
	codex string // stub codex executable path
	wf    model.WorkflowID
}

// approvedRuntimeFixture drives the full lifecycle through the Execution
// Approval of a Spec routed to codex and returns the fixture ready for
// dispatch (the brief Step 1 verbatim constructor).
func approvedRuntimeFixture(t *testing.T) *approvedRuntime {
	t.Helper()
	fx := &approvedRuntime{planningFixture: newExecutionFixture(t)}
	fx.codex = stubCodexOnPath(t)
	fa, sup := process.NewFakeSupervisor()
	fx.fa = fa
	fx.rec = &startRecorder{Supervisor: sup}
	fx.wf = fx.driveToApprovedCodexRoute(t, codexRouteSpec)
	return fx
}

// routingApp builds an Application over the fixture repository with the
// Fake Adapter plus the real codex Adapter (the drift gate's provider).
func (fx *approvedRuntime) routingApp(scripts ...string) *Application {
	fx.t.Helper()
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		fx.t.Fatalf("provider registry: %v", err)
	}
	prompts, err := agent.LoadPromptRegistry()
	if err != nil {
		fx.t.Fatalf("prompt registry: %v", err)
	}
	ad := fake.New(reg)
	for _, s := range scripts {
		if err := ad.LoadScript([]byte(s)); err != nil {
			fx.t.Fatalf("load fake script: %v", err)
		}
	}
	codexBinding, err := reg.Select("codex")
	if err != nil {
		fx.t.Fatalf("codex binding: %v", err)
	}
	flow, err := gitflow.NewGitFlow(fx.sup, fx.root)
	if err != nil {
		fx.t.Fatalf("new gitflow: %v", err)
	}
	a, err := New(Options{
		Home:         fx.home,
		Project:      ProjectFor(fx.root),
		CflowVersion: "0.0.0-dev",
		Now:          fx.now,
		IDs:          fx.ids,
		Supervisor:   fx.sup,
		GitFlow:      flow,
		Prompts:      prompts,
		Agent: agent.RuntimeOptions{
			Registry:    reg,
			Redaction:   security.Registry{},
			Adapters:    map[string]agent.Adapter{"fake": ad, "codex": codex.New(fx.rec, codexBinding)},
			EvidenceDir: filepath.Join(fx.home, "evidence"),
		},
	})
	if err != nil {
		fx.t.Fatalf("new application: %v", err)
	}
	return a
}

// driveToApprovedCodexRoute runs the execution lifecycle through the
// Execution Approval of one Spec routed to codex. The Dry Run's routing
// resolution detects the codex binding through its read-only version
// probe; the test scripts the probe's stdout.
func (fx *approvedRuntime) driveToApprovedCodexRoute(t *testing.T, specJSON string) model.WorkflowID {
	t.Helper()
	wf := drivePlanningToApproval(t, fx.planningFixture)
	if _, err := fx.app(specOutputScript("s1", specJSON)).Execute(context.Background(),
		GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("spec generation: %v", err)
	}
	if _, err := fx.app(patchOutputScript("w1", checkpointPatch)).Execute(context.Background(),
		CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("workflow compilation: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := fx.routingApp().Execute(context.Background(), ExecutionDryRunCommand{Workflow: wf})
		done <- err
	}()
	hnd := fx.rec.waitStarts(t, 1)
	fx.fa.EmitOutput(hnd, process.Stdout, []byte("codex-cli 0.141.0\n"))
	fx.fa.ExitGroup(hnd, 0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execution dry run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("execution dry run did not settle")
	}
	qv, err := fx.routingApp().Query(context.Background(), ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("execution preview: %v", err)
	}
	pv := qv.(ExecutionPreviewView)
	if pv.RoutingHash == "" || pv.BudgetHash == "" {
		t.Fatalf("the execution preview must bind the immutable routing and budget hashes")
	}
	approveExecution(t, fx.planningFixture, wf, pv)
	return wf
}

// ReplaceProviderBinary overwrites the codex executable with different
// bytes: the executable identity pin the approval observed changes.
func (fx *approvedRuntime) ReplaceProviderBinary(name string, data []byte) {
	fx.t.Helper()
	if name != "codex" {
		fx.t.Fatalf("fixture only pins the codex executable, got %q", name)
	}
	if err := os.WriteFile(fx.codex, data, 0o700); err != nil {
		fx.t.Fatalf("replace codex binary: %v", err)
	}
}

// Dispatch runs one dispatch pass through the routing Application,
// scripting the CAS pre-pass's read-only version probe (start 2).
func (fx *approvedRuntime) Dispatch(node string) error {
	fx.t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := fx.routingApp().Execute(context.Background(), DispatchCommand{Workflow: fx.wf})
		done <- err
	}()
	hnd := fx.rec.waitStarts(fx.t, 2)
	fx.fa.EmitOutput(hnd, process.Stdout, []byte("codex-cli 0.141.0\n"))
	fx.fa.ExitGroup(hnd, 0)
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		fx.t.Fatal("dispatch did not settle")
		return nil
	}
}

// DispatchNoProbe runs one dispatch pass whose pre-pass fails before any
// detection (config drift); no probe is scripted.
func (fx *approvedRuntime) DispatchNoProbe(node string) error {
	fx.t.Helper()
	_, err := fx.routingApp().Execute(context.Background(), DispatchCommand{Workflow: fx.wf})
	return err
}

// RequireAttemptCount asserts the persisted Attempt count of one Node.
func (fx *approvedRuntime) RequireAttemptCount(node string, want int) {
	fx.t.Helper()
	iv := fx.inspect(fx.wf)
	got := 0
	for _, a := range iv.Attempts {
		if string(a.Key.Node) == node {
			got++
		}
	}
	if got != want {
		fx.t.Fatalf("attempts of %s = %d, want %d", node, got, want)
	}
}

// RequireProviderStarts asserts the number of Provider model-request
// process starts the codex Adapter launched (the read-only version probes
// of detection are never Provider starts).
func (fx *approvedRuntime) RequireProviderStarts(want int) {
	fx.t.Helper()
	got := 0
	for _, s := range fx.rec.recordedSpecs() {
		if !isVersionProbe(s) {
			got++
		}
	}
	if got != want {
		fx.t.Fatalf("provider starts = %d, want %d", got, want)
	}
}

// changedHashBinary is a codex executable with different bytes but the
// same version line: the drift is the binary identity, not the version.
func changedHashBinary(t *testing.T) []byte {
	t.Helper()
	return []byte("#!/bin/sh\necho codex-cli 0.141.0\n# drifted binary identity\n")
}

// stubCodexOnPath places a stub codex executable on PATH and returns its
// path (the codex adapter resolves and hashes it during Detect).
func stubCodexOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "codex")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho codex-cli 0.141.0\n"), 0o700); err != nil {
		t.Fatalf("write stub codex: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return p
}

// startRecorder wraps a process.Supervisor and records every Start so the
// tests can assert the exact ProcessSpec the codex Adapter launches and
// script the fake process output by handle.
type startRecorder struct {
	process.Supervisor
	mu     sync.Mutex
	starts []process.Handle
	specs  []process.ProcessSpec
}

func (r *startRecorder) Start(ctx context.Context, spec process.ProcessSpec) (process.Handle, process.Events, error) {
	h, evs, err := r.Supervisor.Start(ctx, spec)
	r.mu.Lock()
	r.starts = append(r.starts, h)
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	return h, evs, err
}

func (r *startRecorder) recordedSpecs() []process.ProcessSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]process.ProcessSpec(nil), r.specs...)
}

// waitStarts blocks until the recorder has recorded n Starts and returns
// the n-th handle.
func (r *startRecorder) waitStarts(t *testing.T, n int) process.Handle {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := len(r.starts)
		r.mu.Unlock()
		if got >= n {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.starts[n-1]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for supervisor start %d", n)
	return process.Handle{}
}

// isVersionProbe reports whether a ProcessSpec is the read-only version
// probe of detection (never a Provider model request).
func isVersionProbe(s process.ProcessSpec) bool {
	return len(s.Args) == 1 && s.Args[0] == "--version"
}

// ---------------------------------------------------------------------------
// brief Step 1 (verbatim) and the drift gates
// ---------------------------------------------------------------------------

// TestBindingDriftStopsBeforeAttemptAllocation (brief Step 1, verbatim):
// a changed executable identity after the Execution Approval blocks the
// dispatch with PROVIDER_PROTOCOL_BINDING_CHANGED before any Attempt is
// allocated and before any Provider process starts.
func TestBindingDriftStopsBeforeAttemptAllocation(t *testing.T) {
	fx := approvedRuntimeFixture(t)
	fx.ReplaceProviderBinary("codex", changedHashBinary(t))
	err := fx.Dispatch("S01")
	assertFaultCode(t, err, model.CodeProviderBindingChanged)
	fx.RequireAttemptCount("S01", 0)
	fx.RequireProviderStarts(0)
}

// TestProtocolDriftPausesBeforeStartWithoutSpendingRetry: the drift not
// only stops before allocation — it closes the Dispatch Gate and pauses
// the Workflow (PRD 失败分类: PROVIDER_PROTOCOL_BINDING_CHANGED closes
// dispatch; a regenerated Dry Run and Execution Approval are required).
func TestProtocolDriftPausesBeforeStartWithoutSpendingRetry(t *testing.T) {
	fx := approvedRuntimeFixture(t)
	fx.ReplaceProviderBinary("codex", changedHashBinary(t))
	err := fx.Dispatch("S01")
	assertFaultCode(t, err, model.CodeProviderBindingChanged)
	fx.RequireAttemptCount("S01", 0)
	fx.RequireProviderStarts(0)
	st := fx.status(fx.wf)
	if st.Runtime != model.RuntimePaused {
		t.Fatalf("binding drift must pause the workflow, got runtime %s", st.Runtime)
	}
}

// TestConfigDriftStopsBeforeAttemptAllocation: editing the strict
// configuration after the Execution Approval changes the resolved routing
// inputs; the dispatch refuses with APPROVAL_INPUT_CHANGED before any
// Attempt or Provider start — a successor Dry Run and Execution Approval
// are required (design 20.1).
func TestConfigDriftStopsBeforeAttemptAllocation(t *testing.T) {
	fx := approvedRuntimeFixture(t)
	cfg := filepath.Join(fx.home, "config.yaml")
	if err := os.MkdirAll(fx.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("routing:\n  model: gpt-5\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	err := fx.DispatchNoProbe("S01")
	assertFaultCode(t, err, model.CodeApprovalInputChanged)
	fx.RequireAttemptCount("S01", 0)
	fx.RequireProviderStarts(0)
}

// TestUnapprovedFallbackFailsDryRun: a configured fallback that names a
// Provider that can never be selected (the disabled P1 OpenCode) fails
// the Dry Run closed with PROVIDER_PROTOCOL_UNSUPPORTED: an unapproved
// Fallback is never bound silently (PRD 约束 306).
func TestUnapprovedFallbackFailsDryRun(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := drivePlanningToApproval(t, fx)
	if _, err := fx.app(specOutputScript("s1", divideSpec)).Execute(context.Background(),
		GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("spec generation: %v", err)
	}
	if _, err := fx.app(patchOutputScript("w1", checkpointPatch)).Execute(context.Background(),
		CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("workflow compilation: %v", err)
	}
	cfg := filepath.Join(fx.home, "config.yaml")
	if err := os.MkdirAll(fx.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("routing:\n  fallbacks:\n    - opencode\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := fx.app().Execute(context.Background(), ExecutionDryRunCommand{Workflow: wf})
	assertFaultCode(t, err, model.CodeProviderProtocolUnsupported)
}

// ---------------------------------------------------------------------------
// resume fallback facts and the Kernel charge (design 14.4 step 5)
// ---------------------------------------------------------------------------

// resumeMissingScript is the deterministic Implementer fixture whose
// native Resume fails unrecoverably (the fake's not-found outcome), the
// fallback trigger of the runtime tests.
const resumeMissingScript = `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":"c2","exit_code":0,"resume":"not-found","seed":true}
{"type":"session_started","session_id":"c2","at_ms":0}
{"type":"assistant_message","session_id":"c2","text":"Halfway through the implementation.","at_ms":40}
{"type":"session_finished","session_id":"c2","result":{"done":false,"reason":"interrupted"},"at_ms":100}`

// TestResumeFallbackReportsLostFactsForKernelCharge: the Application
// reports an unrecoverable Resume as the immutable facts — the original
// Session retained as LOST with the automatic-execution failure code —
// never as a success claim; the Decision Kernel charges the approved
// budget from those facts (design 14.4 step 5).
func TestResumeFallbackReportsLostFactsForKernelCharge(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := drivePlanningToApproval(t, fx)
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatal(err)
	}
	ad := fake.New(reg)
	if err := ad.LoadScript([]byte(resumeMissingScript)); err != nil {
		t.Fatal(err)
	}
	rt, err := agent.NewRuntime(agent.RuntimeOptions{
		Now:         fx.now,
		IDs:         fx.ids,
		Registry:    reg,
		Redaction:   security.Registry{},
		EvidenceDir: filepath.Join(fx.home, "evidence"),
		Adapters:    map[string]agent.Adapter{"fake": ad},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { requireNoError(t, rt.Close()) }()
	requireNoError(t, rt.Hydrate(context.Background(), []agent.SessionFact{{
		// The hydrated ledger fact carries the Session's Provider (the
		// Store's sessions row always records it); the Resume request
		// leaves the Provider to the ledger.
		Session: model.Session{
			ProviderSessionID: "c2",
			Purpose:           model.PurposeImplementation,
			Status:            model.SessionActive,
		},
		Provider: "fake",
	}}))
	sess := rt.Sessions()[0].Session

	a := fx.app()
	input := model.DispatchInput{Node: "S01", Session: sess.ID, Route: "fake", BaseHead: "base"}
	result, err := a.executeEffect(context.Background(), model.ProviderResumeIntent{
		Session: sess.ID, Purpose: model.PurposeImplementation,
	}, false, wf, DispatchCommand{Workflow: wf}, input, rt)
	requireNoError(t, err)
	if result.Kind != model.ProviderRunEnded {
		t.Fatalf("fallback result kind = %s, want provider-run-ended", result.Kind)
	}
	if result.Session.Status != model.SessionLost || result.Session.ID != sess.ID {
		t.Fatalf("the fallback must report the original session as LOST, got %+v", result.Session)
	}
	if result.FailureCode != model.CodeAgentProcessCrashed {
		t.Fatalf("the fallback must report the automatic-execution failure code, got %s", result.FailureCode)
	}
}

// seedExecutionAttempt seeds one Node with a RUNNING Attempt bound to one
// Session directly (the persisted state the fallback charge test settles).
func seedExecutionAttempt(t *testing.T, dbPath string, wf model.WorkflowID, node, sessionID string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO tasks
		(id, workflow_id, spec_id, branch_name, task_base_commit, created_at, updated_at)
		VALUES (?, ?, ?, ?, '1111111111111111111111111111111111111111', ?, ?)`,
		node, string(wf), node, "cflow/"+string(wf)+"/task-"+node, now, now); err != nil {
		t.Fatalf("seed task row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes
		(id, workflow_id, task_id, node_type, status, retry_budget_consumed, max_retry_budget, created_at, updated_at)
		VALUES (?, ?, ?, 'agent-task', 'RUNNING', 0, 2, ?, ?)`,
		node, string(wf), node, now, now); err != nil {
		t.Fatalf("seed node row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions
		(id, workflow_id, purpose, provider, provider_session_id, status, started_at, metadata_json)
		VALUES (?, ?, 'implementation', 'fake', 'psess1', 'STARTING', ?, '{}')`,
		sessionID, string(wf), now); err != nil {
		t.Fatalf("seed session row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO node_attempts
		(id, node_id, attempt_number, status, session_id, start_head_commit, evidence_manifest_json, started_at)
		VALUES (?, ?, 1, 'RUNNING', ?, '1111111111111111111111111111111111111111', '[]', ?)`,
		fmt.Sprintf("%s-1", node), node, sessionID, now); err != nil {
		t.Fatalf("seed running attempt: %v", err)
	}
}

// settleProviderRun feeds one ProviderRunEnded result through the Kernel
// (the exact seam the effect loop uses) and returns the resulting state
// view.
func settleProviderRun(t *testing.T, fx *planningFixture, wf model.WorkflowID, in model.EffectResultInput) {
	t.Helper()
	a := fx.app()
	st, err := a.ensureWriteStore(context.Background(), wf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.runDecisionLoop(context.Background(), st, wf, DispatchCommand{Workflow: wf}, in, false); err != nil {
		t.Fatalf("settle provider run: %v", err)
	}
}

// TestFallbackChargeThroughKernel: the automatic successor Attempt of an
// unrecoverable Resume charges the approved Retry Budget — the original
// Attempt is terminal FAILED with the charge recorded, the Node returns
// READY with one charged retry, and the successor Attempt is allocated
// (design 14.4 step 5, PRD 失败分类).
func TestFallbackChargeThroughKernel(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := drivePlanningToApproval(t, fx)
	pv := driveToExecutionGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)
	seedExecutionAttempt(t, filepath.Join(fx.home, "cflow.db"), wf, "S01", "sess1")

	settleProviderRun(t, fx, wf, model.EffectResultInput{
		Kind:        model.ProviderRunEnded,
		Session:     model.Session{ID: "sess1", Purpose: model.PurposeImplementation, Status: model.SessionLost},
		FailureCode: model.CodeAgentProcessCrashed,
	})

	iv := fx.inspect(wf)
	if len(iv.Attempts) != 2 {
		t.Fatalf("attempts = %d, want the original plus the automatic successor", len(iv.Attempts))
	}
	var original, successor *model.Attempt
	for i := range iv.Attempts {
		a := &iv.Attempts[i]
		if a.Key.Node != "S01" {
			t.Fatalf("unexpected attempt %+v", a.Key)
		}
		switch a.Key.Number {
		case 1:
			original = a
		case 2:
			successor = a
		default:
			t.Fatalf("unexpected attempt number %d", a.Key.Number)
		}
	}
	if original.Status != model.AttemptFailed || original.FailureCode != model.CodeAgentProcessCrashed {
		t.Fatalf("original attempt must settle FAILED with the automatic-execution code, got %+v", original)
	}
	if !original.RetryCharged {
		t.Fatalf("the automatic fallback successor must charge the approved budget")
	}
	if successor.Status != model.AttemptReady {
		t.Fatalf("successor attempt must be allocated READY, got %s", successor.Status)
	}
	for _, n := range iv.Nodes {
		if n.ID != "S01" {
			continue
		}
		if n.Status != model.NodeReady || n.RetryCharged != 1 {
			t.Fatalf("node must return READY with one charged retry, got %s charged %d", n.Status, n.RetryCharged)
		}
	}
}

// TestAutomaticFallbackPersistsSuccessorLineage (review fix #1): the
// automatic fallback successor Session of an unrecoverable Resume is
// persisted in the settle Decision — the sessions table records the
// successor row with supersedes_session_id and the fallback Provider,
// and the successor Attempt references the successor Session (design
// 14.4: one successor per Lost original; the lineage is durable, never
// only in the pass's Runtime ledger).
func TestAutomaticFallbackPersistsSuccessorLineage(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := drivePlanningToApproval(t, fx)
	pv := driveToExecutionGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)
	seedExecutionAttempt(t, filepath.Join(fx.home, "cflow.db"), wf, "S01", "sess1")

	settleProviderRun(t, fx, wf, model.EffectResultInput{
		Kind:        model.ProviderRunEnded,
		Session:     model.Session{ID: "sess1", Purpose: model.PurposeImplementation, Status: model.SessionLost},
		FailureCode: model.CodeAgentProcessCrashed,
		// The successor Session the Runtime allocated at the fallback:
		// the settle Decision persists this exact row with its lineage.
		SuccessorSession: model.Session{
			ID:         "succ1",
			Purpose:    model.PurposeImplementation,
			Status:     model.SessionStarting,
			Supersedes: "sess1",
			Provider:   "claude",
		},
	})

	iv := fx.inspect(wf)
	if len(iv.Attempts) != 2 {
		t.Fatalf("attempts = %d, want the original plus the automatic successor", len(iv.Attempts))
	}
	var successor *model.Attempt
	for i := range iv.Attempts {
		a := &iv.Attempts[i]
		if a.Key.Number == 2 {
			successor = a
		}
	}
	if successor == nil || successor.Session != "succ1" {
		t.Fatalf("the successor attempt must reference the persisted successor session, got %+v", successor)
	}
	var found bool
	for _, s := range iv.Sessions {
		if s.ID != "succ1" {
			continue
		}
		found = true
		if s.Supersedes != "sess1" || s.Provider != "claude" || s.Status != model.SessionStarting {
			t.Fatalf("the persisted successor session must carry supersedes_session_id and the fallback provider, got %+v", s)
		}
	}
	if !found {
		t.Fatal("the successor session row is missing from the persisted sessions")
	}
	// The sessions table itself (the aggregate is read back from SQLite).
	db, err := sql.Open("sqlite", filepath.Join(fx.home, "cflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var supersedes, provider string
	if err := db.QueryRow(`SELECT COALESCE(supersedes_session_id, ''), COALESCE(provider, '')
		FROM sessions WHERE id = 'succ1' AND workflow_id = ?`, string(wf)).Scan(&supersedes, &provider); err != nil {
		t.Fatalf("read successor session row: %v", err)
	}
	if supersedes != "sess1" || provider != "claude" {
		t.Fatalf("the sessions table must record supersedes_session_id=%q and provider=%q, got %q/%q", "sess1", "claude", supersedes, provider)
	}
}

// TestSuccessorStartInputCarriesContextBundle (review fix #2): the
// successor Session's start input carries the immutable redacted Context
// Bundle handoff of the LOST original and the Supersedes lineage fact
// (design 14.4, PRD 已确认：Session Resume 失败与跨 Provider 上下文交接).
// The successor's dispatch pass reads the same persisted bundle from the
// evidence root — the handoff is never only in the pass's Runtime.
func TestSuccessorStartInputCarriesContextBundle(t *testing.T) {
	fx := newExecutionFixture(t)
	// The evidence root chain must exist before the Runtime's guarded
	// subdirectory creation (the fixture's app would create it on the
	// first command; this test builds the Runtime directly).
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
		Adapters:    map[string]agent.Adapter{"fake": fake.New(reg)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { requireNoError(t, rt.Close()) }()
	requireNoError(t, rt.Hydrate(context.Background(), []agent.SessionFact{
		{
			Session: model.Session{
				ID: "lost1", ProviderSessionID: "ps-lost",
				Purpose: model.PurposeImplementation, Status: model.SessionLost,
			},
			Provider: "fake",
		},
		{
			Session: model.Session{
				ID: "succ1", Purpose: model.PurposeImplementation,
				Status: model.SessionStarting, Supersedes: "lost1",
			},
			Provider: "claude",
		},
	}))
	bundle, err := rt.CreateContextBundle(context.Background(), agent.ContextBundleRequest{
		SessionID:         "lost1",
		ProviderSessionID: "ps-lost",
		Purpose:           model.PurposeImplementation,
		Context: agent.ContextInput{
			Requirement:  "Implement divide.",
			StageSummary: "Implementation started.",
		},
	})
	requireNoError(t, err)

	a := fx.app()
	supersedes, handed := a.successorHandoff(rt, "succ1")
	if supersedes != "ps-lost" || handed == nil {
		t.Fatalf("successor handoff must carry the superseded provider session and the bundle, got %q %v", supersedes, handed)
	}
	if handed.Hash != bundle.Hash || handed.Revision != bundle.Revision {
		t.Fatalf("the handoff must be the persisted bundle, got %+v", handed)
	}
	// A brand-new Session (no Supersedes) never receives a handoff.
	if _, h := a.successorHandoff(rt, "lost1"); h != nil {
		t.Fatal("a non-successor session must not receive a bundle handoff")
	}
	// The successor start input carries the bundle reference.
	input := attachBundleInput(&codingSessionInput{Spec: "spec", Catalog: "cat", Worktree: "/wt"}, handed)
	c, ok := input.(*codingSessionInput)
	if !ok || c.ContextBundle == nil || c.ContextBundle.Hash != bundle.Hash {
		t.Fatalf("the successor start input must carry the bundle reference, got %+v", input)
	}
	if c.ContextBundle.Context.Requirement != "Implement divide." {
		t.Fatalf("the handoff input must carry the redacted bundle content, got %q", c.ContextBundle.Context.Requirement)
	}
	if attachBundleInput(input, nil).(*codingSessionInput).ContextBundle == nil {
		t.Fatal("a nil bundle must leave the input unchanged")
	}
}

// TestUserInterruptionNeverChargesRetry: a user Ctrl+C interruption is
// never a Provider failure: the Attempt settles INTERRUPTED without a
// Retry charge and no successor Attempt is allocated (PRD 失败分类,
// USER_INTERRUPTED).
func TestUserInterruptionNeverChargesRetry(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := drivePlanningToApproval(t, fx)
	pv := driveToExecutionGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)
	seedExecutionAttempt(t, filepath.Join(fx.home, "cflow.db"), wf, "S01", "sess1")

	settleProviderRun(t, fx, wf, model.EffectResultInput{
		Kind: model.ProviderRunEnded,
		// The run settled terminal with the USER_INTERRUPTED failure code
		// (a provider-declared session_failed carrying the user
		// interruption code); the Kernel must never treat it as a Provider
		// failure or charge the Retry Budget (PRD 失败分类).
		Session:     model.Session{ID: "sess1", Purpose: model.PurposeImplementation, Status: model.SessionFailed},
		FailureCode: model.CodeUserInterrupted,
	})

	iv := fx.inspect(wf)
	if len(iv.Attempts) != 1 {
		t.Fatalf("attempts = %d, want only the interrupted original", len(iv.Attempts))
	}
	a := iv.Attempts[0]
	if a.Status != model.AttemptInterrupted || a.FailureCode != model.CodeUserInterrupted {
		t.Fatalf("attempt must settle INTERRUPTED with USER_INTERRUPTED, got %+v", a)
	}
	if a.RetryCharged {
		t.Fatalf("a user interruption must never charge the retry budget")
	}
	for _, n := range iv.Nodes {
		if n.ID == "S01" && n.RetryCharged != 0 {
			t.Fatalf("interruption must not charge the node retry budget, got %d", n.RetryCharged)
		}
	}
}

// TestRoutingHashBoundByApproval: the Execution Approval binds the
// routing and budget hashes; an approval with a stale routing hash is
// refused (APPROVAL_INPUT_CHANGED) and never opens dispatch.
func TestRoutingHashBoundByApproval(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := drivePlanningToApproval(t, fx)
	pv := driveToExecutionGate(t, fx, wf)
	pv.RoutingHash = strings.Repeat("0", 64)
	_, err := fx.app().Execute(context.Background(), ApproveExecutionCommand{
		Workflow:         wf,
		PlanHash:         pv.PlanHash,
		SpecHashes:       pv.SpecHashes,
		CatalogHash:      pv.CatalogHash,
		WorkflowHash:     pv.WorkflowHash,
		RoutingHash:      pv.RoutingHash,
		BudgetHash:       pv.BudgetHash,
		CommitPolicyHash: pv.CommitPolicyHash,
	})
	assertFaultCode(t, err, model.CodeApprovalInputChanged)
	st := fx.status(wf)
	if st.Runtime == model.RuntimeRunning || st.Stage == model.StageExecution {
		t.Fatalf("a stale routing approval must never open dispatch, got %s/%s", st.Stage, st.Runtime)
	}
}
