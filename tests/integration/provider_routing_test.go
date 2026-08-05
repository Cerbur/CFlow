// Package integration: the Cross-Provider routing end to end (Task 16,
// brief Step 5). One workflow carries two independent Specs routed to the
// codex and claude Providers; after the Execution Approval the dispatch
// pass runs the two coding Sessions concurrently on the two real dialect
// Adapters (scripted supervisors, stub executables on PATH). The fixture
// scripting proves the two runs overlap in real time, the shared fixture
// clock proves the virtual-time overlap of their Attempts, and the two
// Sessions retain independent Session IDs. Planner/Checker independence
// is re-asserted on the same fake Provider.
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/claude"
	"cflow.local/cflow/internal/agent/codex"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// capturedClaudeVersion is the version line the stub claude reports (the
// captured 2.1.220 binding's probe text).
const capturedClaudeVersion = "2.1.220 (Claude Code)"

// dualProviderSpecs routes two independent Tasks to codex and claude.
const dualProviderSpecs = `{"id":"s01","goal":"implement add","depends_on":[],"write_scope":["src/calc/add/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"codex","model":"gpt-5","budget":10},"timeout_seconds":1800,"max_retry":2}
{"id":"s02","goal":"implement subtract","depends_on":[],"write_scope":["src/calc/sub/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"claude","model":"claude-sonnet-4-5","budget":10},"timeout_seconds":1800,"max_retry":2}`

// dualSpecsScript wraps the two Specs in the Session output (the spec set
// joins onto the session_finished line).
func dualSpecsScript(sessionID string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"spec-generation","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Splitting the plan.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"specs":[%s],"proposed_commands":[]},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID),
		strings.ReplaceAll(dualProviderSpecs, "\n", ","))
}

// providerFixture owns one real repository and CFLOW_HOME plus the real
// codex and claude Adapters over one recording Fake Supervisor, with stub
// executables on PATH.
type providerFixture struct {
	*parallelFixture
	fa     *process.FakeAdapter
	rec    *startRecorder
	codex  string // stub codex executable path
	claude string // stub claude executable path
}

func newProviderFixture(t *testing.T) *providerFixture {
	t.Helper()
	fx := &providerFixture{parallelFixture: newParallelFixture(t)}
	fa, sup := process.NewFakeSupervisor()
	fx.fa = fa
	fx.rec = &startRecorder{Supervisor: sup}
	fx.codex = stubExecutable(t, "codex", "#!/bin/sh\necho codex-cli 0.146.0\n")
	fx.claude = stubExecutable(t, "claude", "#!/bin/sh\necho "+capturedClaudeVersion+"\n")
	return fx
}

// stubExecutable places one stub CLI executable on PATH and returns its
// path (the adapters resolve and hash it during Detect).
func stubExecutable(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o700); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return p
}

// providerApp builds an Application over the fixture repository with the
// Fake Adapter plus the real codex and claude Adapters.
func (fx *providerFixture) providerApp(scripts ...string) *app.Application {
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
	codexBinding, _ := reg.Select("codex")
	claudeBinding, _ := reg.Select("claude")
	flow, err := gitflow.NewGitFlow(fx.sup, fx.repo.Root)
	if err != nil {
		fx.t.Fatalf("new gitflow: %v", err)
	}
	a, err := app.New(app.Options{
		Home:         fx.home,
		Project:      app.ProjectFor(fx.repo.Root),
		CflowVersion: "0.0.0-dev",
		Now:          fx.now,
		IDs:          fx.ids,
		Supervisor:   fx.sup,
		GitFlow:      flow,
		Prompts:      prompts,
		Agent: agent.RuntimeOptions{
			Registry:  reg,
			Redaction: security.Registry{},
			Adapters: map[string]agent.Adapter{
				"fake":   ad,
				"codex":  codex.New(fx.rec, codexBinding),
				"claude": claude.New(fx.rec, claudeBinding),
			},
			EvidenceDir: filepath.Join(fx.home, "evidence"),
		},
	})
	if err != nil {
		fx.t.Fatalf("new application: %v", err)
	}
	return a
}

// driveToExecutionApproval runs the planning and execution lifecycle
// through the Execution Approval of the dual-provider Spec set and
// returns the workflow identity plus the preview.
func (fx *providerFixture) driveToExecutionApproval(t *testing.T) (model.WorkflowID, app.ExecutionPreviewView) {
	t.Helper()
	wf := fx.CreateWorkflow("dual-provider")
	fx.Discuss(wf, "a calculator with add and subtract")
	plan := fx.GeneratePlan(wf)
	if plan.SessionID == "" {
		t.Fatal("plan generation carried no session id")
	}
	check := fx.CheckPlan(wf)
	if check.SessionID == plan.SessionID {
		t.Fatal("checker reused the planner session (independent role lineage)")
	}
	pv := fx.planView(wf)
	if err := fx.ApprovePlan(wf, pv.Revision, pv.Hash); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	cfg := filepath.Join(fx.home, "config.yaml")
	if err := os.MkdirAll(fx.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("concurrency: 4\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := fx.app(dualSpecsScript("s1")).Execute(context.Background(),
		app.GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("spec generation: %v", err)
	}
	if _, err := fx.app(calculatorPatchScript("w1")).Execute(context.Background(),
		app.CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("workflow compilation: %v", err)
	}
	// The Dry Run's routing resolution detects the referenced providers
	// in the resolution order (each a read-only version probe; each
	// scripted by executable). The resolution detects codex before
	// claude (Spec order), so the codex probe is consumed first.
	done := make(chan error, 1)
	go func() {
		_, err := fx.providerApp().Execute(context.Background(), app.ExecutionDryRunCommand{Workflow: wf})
		done <- err
	}()
	hCodex := fx.rec.waitNextMatch(t, probeOf(fx.codex))
	fx.fa.EmitOutput(hCodex, process.Stdout, []byte("codex-cli 0.146.0\n"))
	fx.fa.ExitGroup(hCodex, 0)
	hClaude := fx.rec.waitNextMatch(t, probeOf(fx.claude))
	fx.fa.EmitOutput(hClaude, process.Stdout, []byte(capturedClaudeVersion+"\n"))
	fx.fa.ExitGroup(hClaude, 0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execution dry run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("execution dry run did not settle")
	}
	qview, err := fx.providerApp().Query(context.Background(), app.ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("execution preview: %v", err)
	}
	preview := qview.(app.ExecutionPreviewView)
	if preview.RoutingHash == "" || preview.BudgetHash == "" {
		t.Fatal("the preview must bind the routing and budget hashes")
	}
	if _, err := fx.app().Execute(context.Background(), app.ApproveExecutionCommand{
		Workflow:         wf,
		PlanHash:         preview.PlanHash,
		SpecHashes:       preview.SpecHashes,
		CatalogHash:      preview.CatalogHash,
		WorkflowHash:     preview.WorkflowHash,
		RoutingHash:      preview.RoutingHash,
		BudgetHash:       preview.BudgetHash,
		CommitPolicyHash: preview.CommitPolicyHash,
	}); err != nil {
		t.Fatalf("execution approval: %v", err)
	}
	return wf, preview
}

// ---------------------------------------------------------------------------
// startRecorder and scripting
// ---------------------------------------------------------------------------

type startRecorder struct {
	process.Supervisor
	mu       sync.Mutex
	starts   []process.Handle
	specs    []process.ProcessSpec
	consumed []bool
}

func (r *startRecorder) Start(ctx context.Context, spec process.ProcessSpec) (process.Handle, process.Events, error) {
	h, evs, err := r.Supervisor.Start(ctx, spec)
	r.mu.Lock()
	r.starts = append(r.starts, h)
	r.specs = append(r.specs, spec)
	r.consumed = append(r.consumed, false)
	r.mu.Unlock()
	return h, evs, err
}

// waitNextMatch blocks until a recorded start that matches pred has not
// been scripted yet and consumes it: concurrent probes and runs are
// matched by executable and argv, never by order, and every start is
// scripted exactly once.
func (r *startRecorder) waitNextMatch(t *testing.T, pred func(process.ProcessSpec) bool) process.Handle {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for i, s := range r.specs {
			if !r.consumed[i] && pred(s) {
				r.consumed[i] = true
				h := r.starts[i]
				r.mu.Unlock()
				return h
			}
		}
		r.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for a matching supervisor start")
	return process.Handle{}
}

// emitFrames writes one fixture's frames to a running process and exits
// the group.
func (fx *providerFixture) emitFrames(h process.Handle, frames string) {
	fx.t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(frames), "\n") {
		if line == "" {
			continue
		}
		fx.fa.EmitOutput(h, process.Stdout, []byte(line+"\n"))
	}
	fx.fa.ExitGroup(h, 0)
}

// probeOf matches the read-only --version probe of one executable.
func probeOf(path string) func(process.ProcessSpec) bool {
	return func(s process.ProcessSpec) bool {
		return s.Executable == path && len(s.Args) == 1 && s.Args[0] == "--version"
	}
}

// TestCodexAndClaudeTasksRunConcurrently (brief Step 5): the two
// independent Tasks of one workflow run their coding Sessions on the
// codex and claude dialect Adapters concurrently — the codex run is in
// flight when the claude run starts — while retaining independent Session
// IDs, and their Attempts share the fixture clock instant (virtual-time
// overlap).
func TestCodexAndClaudeTasksRunConcurrently(t *testing.T) {
	fx := newProviderFixture(t)
	wf, _ := fx.driveToExecutionApproval(t)

	codexFrames := readTestdata(t, "providers/codex/0.146.0/start-valid.jsonl")
	claudeFrames := readTestdata(t, "providers/claude/2.1.220/start-valid.jsonl")

	done := make(chan error, 1)
	go func() {
		_, err := fx.providerApp().Execute(context.Background(), app.DispatchCommand{Workflow: wf})
		done <- err
	}()

	// The dispatch CAS pre-pass re-detects both bindings (sequential by
	// selected node order), then each node chain's Runtime Start
	// re-detects its binding (concurrent) and launches the run. Every
	// probe is scripted by executable, never by order.
	hCodexProbe := fx.rec.waitNextMatch(t, probeOf(fx.codex))
	fx.fa.EmitOutput(hCodexProbe, process.Stdout, []byte("codex-cli 0.146.0\n"))
	fx.fa.ExitGroup(hCodexProbe, 0)
	hClaudeProbe := fx.rec.waitNextMatch(t, probeOf(fx.claude))
	fx.fa.EmitOutput(hClaudeProbe, process.Stdout, []byte(capturedClaudeVersion+"\n"))
	fx.fa.ExitGroup(hClaudeProbe, 0)

	// Real-time overlap: wait for the codex run to start and keep it in
	// flight while the claude run starts; only then emit both runs.
	hCodexRun := fx.rec.waitNextMatch(t, func(s process.ProcessSpec) bool {
		return s.Executable == fx.codex && len(s.Args) > 0 && s.Args[0] == "exec"
	})
	hClaudeRun := fx.rec.waitNextMatch(t, func(s process.ProcessSpec) bool {
		return s.Executable == fx.claude && len(s.Args) > 0 && s.Args[0] == "--print"
	})
	if hCodexRun == hClaudeRun {
		t.Fatal("the two dialect runs must be distinct processes")
	}
	fx.emitFrames(hCodexRun, codexFrames)
	fx.emitFrames(hClaudeRun, claudeFrames)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("dispatch did not settle")
	}

	iv := fx.Inspect(wf)
	statusByID := map[model.NodeID]model.NodeStatus{}
	for _, n := range iv.Nodes {
		statusByID[n.ID] = n.Status
	}
	for _, id := range []string{"task-s01", "task-s02"} {
		if statusByID[model.NodeID(id)] != model.NodeReady {
			t.Fatalf("node %s status = %s, want READY (gate failure with a budgeted successor)", id, statusByID[model.NodeID(id)])
		}
	}

	// Virtual-time overlap: the coding Attempts of both routed Tasks
	// share the fixture clock instant of the dispatch pass (the same
	// proof as the parallel Tasks test, now across two Providers). The
	// Task 13 gate settles each coding run (the captured fixtures write
	// no Commit) with MISSING_IMPLEMENTATION_COMMIT and a budgeted READY
	// successor, so each routed Task carries two Attempts sharing the
	// pass instant.
	var startedAt time.Time
	settled := 0
	taskAttempts := 0
	for _, at := range iv.Attempts {
		if !strings.HasPrefix(string(at.Key.Node), "task-s") {
			t.Fatalf("attempt %s is not a task attempt", at.Key)
		}
		taskAttempts++
		if startedAt.IsZero() {
			startedAt = at.StartedAt
		}
		if !at.StartedAt.Equal(startedAt) {
			t.Fatalf("attempt %s started at %v, want the shared instant %v (virtual-time overlap)", at.Key, at.StartedAt, startedAt)
		}
		if at.Status.IsTerminal() {
			settled++
			if at.FailureCode != model.CodeMissingImplementationCommit {
				t.Fatalf("attempt %s failed with %s, want MISSING_IMPLEMENTATION_COMMIT", at.Key, at.FailureCode)
			}
		}
	}
	if taskAttempts != 4 {
		t.Fatalf("task attempts = %d, want 4 (one original and one budgeted successor per routed Task)", taskAttempts)
	}
	if settled != 2 {
		t.Fatalf("settled attempts = %d, want 2 (the gate settles each coding run in the pass)", settled)
	}

	// Session independence: the two coding Sessions carry independent
	// CFlow identities and independent Provider Session IDs (the codex
	// 0.141.0 and claude 2.1.220 captured fixtures), and the Planner and
	// Checker Sessions of the planning phases stayed independent too.
	providerSessions := map[string]string{} // provider_session_id -> cflow session id
	implSessions := 0
	for _, s := range iv.Sessions {
		if s.Purpose == model.PurposeImplementation {
			implSessions++
		}
		if id := s.ProviderSessionID; id != "" {
			if prev, dup := providerSessions[id]; dup {
				t.Fatalf("provider session id %q reused by %s and %s", id, prev, s.ID)
			}
			providerSessions[id] = string(s.ID)
		}
	}
	if implSessions != 2 {
		t.Fatalf("implementation sessions = %d, want 2", implSessions)
	}
	for _, want := range []string{"0197f1c1-9c6e-7a00-a000-000000000001", "0197f1c1-9c6e-7b00-a000-0000000000c1"} {
		if _, ok := providerSessions[want]; !ok {
			t.Fatalf("the %s dialect session id is missing from the aggregate", want)
		}
	}

	// Both dialect Adapters really launched: the recorded run argv is the
	// codex exec --output-schema contract and the claude --print
	// stream-json contract (never a shell, never bypass flags).
	sawCodexRun, sawClaudeRun := false, false
	for _, s := range fx.rec.allSpecs() {
		switch {
		case s.Executable == fx.codex && len(s.Args) > 0 && s.Args[0] == "exec":
			sawCodexRun = true
			if !strings.Contains(strings.Join(s.Args, " "), "--output-schema") {
				t.Fatalf("codex run argv must carry the managed schema, got %v", s.Args)
			}
		case s.Executable == fx.claude && len(s.Args) > 0 && s.Args[0] == "--print":
			sawClaudeRun = true
			if !strings.Contains(strings.Join(s.Args, " "), "--input-format stream-json") {
				t.Fatalf("claude run argv must carry the stream-json contract, got %v", s.Args)
			}
		}
	}
	if !sawCodexRun || !sawClaudeRun {
		t.Fatalf("both dialect adapters must launch runs (codex %v, claude %v)", sawCodexRun, sawClaudeRun)
	}
}

// allSpecs returns a snapshot of every recorded ProcessSpec.
func (r *startRecorder) allSpecs() []process.ProcessSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]process.ProcessSpec(nil), r.specs...)
}
