package integration

// The Release Fault Matrix injectors — part 2 (Task 21, brief Step 3): the
// Provider protocol frames, the Git identity/signing/policy preflights, the
// Recovery Effect-Intent dispositions, the Final Report/export interruption,
// and the phantom-row class (a PurposeRepair resolution Session/Process the
// request path never settles). Injectors run only from this _test package.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/claude"
	"cflow.local/cflow/internal/agent/codex"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/recovery"
	"cflow.local/cflow/internal/security"
	"cflow.local/cflow/internal/store"
)

// ---------------------------------------------------------------------------
// Provider protocol frames
// ---------------------------------------------------------------------------

// scriptedSup records every supervised start so probes can script the fake
// process output deterministically (the adapter test harness pattern).
type scriptedSup struct {
	process.Supervisor
	mu      sync.Mutex
	starts  []process.ProcessSpec
	handles []process.Handle
}

func (s *scriptedSup) Start(ctx context.Context, spec process.ProcessSpec) (process.Handle, process.Events, error) {
	h, evs, err := s.Supervisor.Start(ctx, spec)
	s.mu.Lock()
	s.starts = append(s.starts, spec)
	s.handles = append(s.handles, h)
	s.mu.Unlock()
	return h, evs, err
}

func (s *scriptedSup) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.starts)
}

func (s *scriptedSup) handleAt(i int) process.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handles[i]
}

// waitStarts blocks until the recorder holds n starts.
func waitStarts(t *testing.T, rec *scriptedSup, n int) process.Handle {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if rec.count() >= n {
			return rec.handleAt(n - 1)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for supervisor start %d (have %d)", n, rec.count())
	return process.Handle{}
}

// scriptVersion scripts one version-probe stdout line and exit.
func scriptVersion(t *testing.T, rec *scriptedSup, fake *process.FakeAdapter, n int, version string, code int) {
	t.Helper()
	h := waitStarts(t, rec, n)
	if version != "" {
		fake.EmitOutput(h, process.Stdout, []byte(version+"\n"))
	}
	fake.ExitGroup(h, code)
}

// scriptFrames emits every fixture line as one stdout frame, then exits.
func scriptFrames(t *testing.T, rec *scriptedSup, fake *process.FakeAdapter, n int, fixture string, code int) {
	t.Helper()
	h := waitStarts(t, rec, n)
	for _, line := range strings.Split(fixture, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fake.EmitOutput(h, process.Stdout, []byte(line+"\n"))
	}
	if code >= 0 {
		fake.ExitGroup(h, code)
	}
}

func providerSchema(t *testing.T) string {
	t.Helper()
	return `{"type":"object"}`
}

// protocolHarness builds a fake-supervisor-backed provider adapter and a
// Runtime whose evidence root is a fresh canonical temp dir (the scan
// target for the no-raw-evidence probe).
func protocolHarness(t *testing.T, provider string) (*scriptedSup, *process.FakeAdapter, agent.Adapter, *agent.Runtime, string) {
	t.Helper()
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatalf("provider registry: %v", err)
	}
	fake, sup := process.NewFakeSupervisor()
	rec := &scriptedSup{Supervisor: sup}
	var ad agent.Adapter
	switch provider {
	case "codex":
		b, err := reg.Select("codex")
		if err != nil {
			t.Fatalf("select codex: %v", err)
		}
		ad = codex.New(rec, b)
	case "claude":
		b, err := reg.Select("claude")
		if err != nil {
			t.Fatalf("select claude: %v", err)
		}
		ad = claude.New(rec, b)
	default:
		t.Fatalf("unknown provider %q", provider)
	}
	evidence := canonTemp(t)
	rt, err := agent.NewRuntime(agent.RuntimeOptions{
		Now:         func() time.Time { return time.Unix(1700000000, 0).UTC() },
		IDs:         model.SequentialIDSource(),
		Registry:    reg,
		Redaction:   artifactRedaction(),
		EvidenceDir: evidence,
		Adapters:    map[string]agent.Adapter{provider: ad},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rec, fake, ad, rt, evidence
}

// scanForRaw walks dir recursively and reports whether any file contains
// the raw value: the recursive-CFLOW_HOME no-raw-evidence probe.
func scanForRaw(t *testing.T, dir, raw string) bool {
	t.Helper()
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), raw) {
			found = true
		}
		return nil
	})
	return found
}

func injectProviderProtocolViolation(t *testing.T, _ matrixRow) rowResult {
	rec, fake, _, rt, evidence := protocolHarness(t, "codex")
	leaked := "sk-abc123def4567890"
	schema := filepath.Join(t.TempDir(), "plan-envelope.json")
	if err := os.WriteFile(schema, []byte(`{"type":"object"}`), 0o600); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	fixture := `{"type":"session_started","timestamp":"2026-07-01T12:00:00.000Z","session_id":"0197f1c1-9c6e-7a00-a000-00000000000a","cwd":"/Users/dev/worktrees/task-1","turn_id":"0197f1c1-9c6e-7a00-a000-00000000000b"}` + "\n" +
		`{"type":"message","timestamp":"2026-07-01T12:00:01.000Z","message_id":"0197f1c1-9c6e-7a00-a000-00000000000c","turn_id":"0197f1c1-9c6e-7a00-a000-00000000000b","payload":{"role":"assistant","content":[{"type":"text","text":"token ` + leaked + ` in the frame"}]}}` + "\n" +
		`{"type":"mystery_event","timestamp":"2026-07-01T12:00:02.000Z","payload":{"x":1}}`

	done := make(chan error, 1)
	go func() {
		_, err := rt.Start(context.Background(), agent.StartRequest{
			Purpose:  model.PurposePlanning,
			Provider: "codex",
			Prompt:   "Plan the work.",
			Input:    codex.Input{SchemaPath: schema},
			CWD:      "/Users/dev/worktrees/task-1",
		})
		done <- err
	}()
	scriptVersion(t, rec, fake, 1, "codex-cli 0.141.0", 0)
	scriptFrames(t, rec, fake, 2, fixture, 0)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the protocol violation was accepted")
		}
		code := faultCodeOf(err)
		ev := evidenceOf("no_raw_frame_persisted", "session_untrusted")
		if !scanForRaw(t, evidence, leaked) {
			ev["no_raw_frame_persisted"] = true
		}
		if code == "PROVIDER_PROTOCOL_VIOLATION" {
			ev["session_untrusted"] = true
		}
		d, dispatch := dispositionDispatch(code)
		return rowResult{Code: code, Disposition: d, RetryCharge: retryChargeOf(code), Dispatch: dispatch, Evidence: ev}
	case <-time.After(30 * time.Second):
		t.Fatal("runtime Start hung on the protocol violation")
		return rowResult{}
	}
}

func injectProviderSessionIDMissing(t *testing.T, _ matrixRow) rowResult {
	rec, fake, ad, _, _ := protocolHarness(t, "claude")
	r, err := ad.Start(context.Background(), agent.StartRequest{
		Purpose:  model.PurposePlanning,
		Provider: "claude",
		Prompt:   "Plan the work.",
		Input:    claude.Input{SchemaJSON: providerSchema(t), MaxBudgetUSD: "0.50"},
		CWD:      "/Users/dev/worktrees/task-1",
	})
	if err != nil {
		t.Fatalf("claude start: %v", err)
	}
	scriptFrames(t, rec, fake, 1,
		`{"type":"system","subtype":"init"}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"result":"{\"ok\":true}"}`, 0)
	events, err := collectRunErr(r)
	var pe *agent.ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a ProtocolError, got %v", err)
	}
	ev := evidenceOf("no_event_persisted", "offending_frame_carried")
	if len(events) == 0 {
		ev["no_event_persisted"] = true
	}
	if strings.Contains(string(pe.Frame), "system") {
		ev["offending_frame_carried"] = true
	}
	code := string(pe.Code)
	d, dispatch := dispositionDispatch(code)
	return rowResult{Code: code, Disposition: d, RetryCharge: retryChargeOf(code), Dispatch: dispatch, Evidence: ev}
}

func injectProviderProtocolUnsupported(t *testing.T, _ matrixRow) rowResult {
	// A provider installation whose protocol/version this binary cannot
	// support blocks before any process is started (PRD 失败分类:
	// PROVIDER_PROTOCOL_UNSUPPORTED, 不启动 Provider).
	inst := agent.Installation{Compatibility: agent.CompatibilityUnknownVersion,
		DialectID: "unknown-dialect", RegistryRevision: "unknown-registry"}
	err := agent.CompareInstallation(inst, agent.RouteBinding{}, false)
	ev := evidenceOf("no_process_started")
	// The pure compare-and-swap never launches anything; the evidence is
	// that the call itself is the gate (no supervisor seam was touched).
	if err != nil {
		ev["no_process_started"] = true
	}
	return faultRow(err, "no_process_started")
}

func injectProviderBindingDrift(t *testing.T, _ matrixRow) rowResult {
	// An executable identity drift from the approved binding blocks with
	// PROVIDER_PROTOCOL_BINDING_CHANGED before any process starts (PRD 已确
	// 认：Executable 身份漂移关闭 Dispatch，重新生成 Dry Run/Execution
	// Approval).
	inst := agent.Installation{Compatibility: agent.CompatibilitySupported,
		ExecutablePath:   "/approved/codex",
		ExecutableSHA256: "fresh-sha",
		CLIVersion:       "0.141.0",
		DialectID:        "cflow.dialect.codex.v1",
		RegistryRevision: "reg-1",
	}
	binding := agent.RouteBinding{
		StartCapabilities: []string{"session_id_on_start", "structured_output"},
		ExecutablePath:    "/approved/codex",
		ExecutableSHA256:  "approved-sha",
		CLIVersion:        "0.141.0",
		DialectID:         "cflow.dialect.codex.v1",
		RegistryRevision:  "reg-1",
	}
	err := agent.CompareInstallation(inst, binding, false)
	ev := evidenceOf("no_process_started", "drift_pinned")
	if err != nil {
		ev["no_process_started"] = true
	}
	if faultCodeOf(err) == "PROVIDER_PROTOCOL_BINDING_CHANGED" {
		ev["drift_pinned"] = true
	}
	return faultRow(err, "no_process_started", "drift_pinned")
}

// collectRunErr drains one adapter Run, returning every event and the first
// non-EOF error (the adapter test harness pattern).
func collectRunErr(r agent.Run) ([]agent.Event, error) {
	var events []agent.Event
	for {
		ev, err := r.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
}

// ---------------------------------------------------------------------------
// Git identity / signing / policy preflights
// ---------------------------------------------------------------------------

func runGitProcess(ctx context.Context, sup process.Supervisor, git, dir string, env map[string]string, args ...string) ([]byte, process.Exit, error) {
	h, events, err := sup.Start(ctx, process.ProcessSpec{
		Executable: git, Args: args, Dir: dir, Env: env, Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, process.Exit{}, err
	}
	var out []byte
	for ev := range events {
		if ev.Kind == process.EventFrameOut {
			out = append(out, ev.Frame...)
			out = append(out, '\n')
		}
	}
	exit, err := sup.Wait(ctx, h)
	return out, exit, err
}

func gitChildEnv(extra map[string]string) map[string]string {
	env := map[string]string{
		"PATH":                os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_TERMINAL_PROMPT": "0",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func commitWithEnv(t *testing.T, repo *Repo, extra map[string]string) {
	t.Helper()
	env := gitChildEnv(extra)
	full := append([]string{"commit", "-q", "--allow-empty", "-m", "test commit"}, "--no-gpg-sign")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, exit, err := runGitProcess(ctx, repo.sup, repo.Git, repo.Root, env, full...)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		t.Fatalf("commit exited %+v: %s", exit, out)
	}
}

func injectGitIdentityMissing(t *testing.T, _ matrixRow) rowResult {
	repo := newCommittedRepo(t)
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")
	headBefore := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	_, err := repo.flow().Execute(context.Background(), gitflow.CommitPreflight{Revision: "missing"})
	ev := evidenceOf("nothing_mutated")
	if head := strings.TrimSpace(string(repo.git("rev-parse", "HEAD"))); head == headBefore {
		ev["nothing_mutated"] = true
	}
	return faultRow(err, "nothing_mutated")
}

func injectGitVerifyPolicyMismatch(t *testing.T, _ matrixRow) rowResult {
	repo := newCommittedRepo(t)
	ev, err := repo.flow().Execute(context.Background(), gitflow.CommitPreflight{Revision: "v1"})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	pre := ev.(gitflow.PreflightEvidence)
	commitWithEnv(t, repo, map[string]string{
		"GIT_AUTHOR_NAME":     "Intruder",
		"GIT_AUTHOR_EMAIL":    "intruder@example.com",
		"GIT_COMMITTER_NAME":  "Intruder",
		"GIT_COMMITTER_EMAIL": "intruder@example.com",
	})
	_, err = repo.flow().Execute(context.Background(), gitflow.VerifyCommit{
		Ref: "HEAD", ExpectedAuthor: pre.Author, ExpectedCommitter: pre.Committer, ExpectedSigning: pre.Signing,
	})
	return faultRow(err, "commit_refused")
}

func injectGitSigningPreflightFailed(t *testing.T, _ matrixRow) rowResult {
	repo := newCommittedRepo(t)
	repo.git("config", "commit.gpgsign", "true")
	repo.git("config", "gpg.format", "ssh")
	repo.git("config", "user.signingkey", filepath.Join(repo.Tmp, "no-such-key"))
	_, err := repo.flow().Execute(context.Background(), gitflow.CommitPreflight{Revision: "badkey"})
	return faultRow(err, "nothing_mutated")
}

// ---------------------------------------------------------------------------
// Recovery Effect-Intent dispositions
// ---------------------------------------------------------------------------

// faultRecoveryFixture mirrors the internal/recovery fixture: one real
// repository, Integration and Task Worktrees, one Store with a workflow row,
// and a Recovery Engine over the same facts.
type faultRecoveryFixture struct {
	t            *testing.T
	sup          process.Supervisor
	repo         *Repo
	home         string
	dbPath       string
	projectKey   string
	integration  string
	taskWorktree string
	taskBranch   string
	baseHead     string
	taskHead     string
	engine       *recovery.RecoveryEngine
	mergeCount   int
	now          func() time.Time
}

const matrixWF = "wf-1"

func newFaultRecoveryFixture(t *testing.T) *faultRecoveryFixture {
	t.Helper()
	repo := newCommittedRepo(t)
	sup := process.NewSupervisor(process.NewOSAdapter())
	flow, err := gitflow.NewGitFlow(sup, repo.Root)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	home := filepath.Join(canonTemp(t), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	key := "test-project"
	worktrees := filepath.Join(home, "worktrees", key, matrixWF)
	integration := filepath.Join(worktrees, "integration")
	taskWorktree := filepath.Join(worktrees, "tasks", "task-s01")
	for _, dir := range []string{worktrees, filepath.Join(worktrees, "tasks")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	if _, err := flow.Execute(context.Background(), gitflow.CreateIntegration{
		Branch: "cflow/" + matrixWF + "/integration", BaseCommit: base, Path: integration,
	}); err != nil {
		t.Fatalf("create integration: %v", err)
	}
	taskBranch := "cflow/" + matrixWF + "/task-task-s01"
	if _, err := flow.Execute(context.Background(), gitflow.CreateTask{
		Branch: taskBranch, BaseHead: base, Path: taskWorktree,
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	repo.gitAt(taskWorktree, "commit", "-q", "--allow-empty", "-m", "implement")
	taskHead := strings.TrimSpace(string(repo.gitAt(taskWorktree, "rev-parse", "HEAD")))

	fx := &faultRecoveryFixture{
		t: t, sup: sup, repo: repo, home: home,
		dbPath: filepath.Join(home, "cflow.db"), projectKey: key,
		integration: integration, taskWorktree: taskWorktree,
		taskBranch: taskBranch, baseHead: base, taskHead: taskHead,
		now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	ctx := context.Background()
	wfStore, err := store.Open(ctx, store.OpenOptions{Path: fx.dbPath, Workflow: matrixWF, CflowVersion: "0.0.0-dev", Now: fx.now})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := wfStore.RegisterProject(ctx, "proj-1", repo.Root, "repo"); err != nil {
		t.Fatalf("register project: %v", err)
	}
	if _, err := wfStore.Transact(ctx, 0, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: []model.Mutation{model.WorkflowMutation{
			ID: matrixWF, Project: "proj-1",
			Stage: model.StageExecution, Runtime: model.RuntimeRunning,
			TargetBranch: "main", BaseCommit: base,
			IntegrationBranch: "cflow/" + matrixWF + "/integration", IntegrationHead: base,
		}}}, nil
	}); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if err := wfStore.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	fx.engine, err = recovery.NewRecoveryEngine(recovery.RecoveryEngineOptions{
		Supervisor: sup, GitFlow: flow,
		Home: home, ProjectKey: key,
		EvidenceDir: filepath.Join(home, "evidence"),
		OpenView: func(ctx context.Context, wf model.WorkflowID) (store.StoreView, error) {
			st, err := store.Open(ctx, store.OpenOptions{Path: fx.dbPath, Workflow: wf, ReadOnly: true, CflowVersion: "0.0.0-dev", Now: fx.now})
			if err != nil {
				return store.StoreView{}, err
			}
			defer st.Close()
			return st.View(ctx, store.StoreQuery{})
		},
		OpenArtifacts: func(ctx context.Context, wf model.WorkflowID) (*artifact.Store, error) {
			return artifact.New(filepath.Join(home, "projects", key, "workflows", string(wf), "artifacts"), security.Registry{})
		},
	})
	if err != nil {
		t.Fatalf("new recovery engine: %v", err)
	}
	return fx
}

// seedIntent commits one Effect Intent into the pending ledger.
func (fx *faultRecoveryFixture) seedIntent(intent model.EffectIntent) {
	fx.t.Helper()
	st, err := store.Open(context.Background(), store.OpenOptions{Path: fx.dbPath, Workflow: matrixWF, CflowVersion: "0.0.0-dev", Now: fx.now})
	if err != nil {
		fx.t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	view, err := st.View(context.Background(), store.StoreQuery{})
	if err != nil {
		fx.t.Fatalf("view: %v", err)
	}
	if _, err := st.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
		return model.Decision{Effect: intent}, nil
	}); err != nil {
		fx.t.Fatalf("seed intent %T: %v", intent, err)
	}
}

// mutate applies one raw state mutation through the Store.
func (fx *faultRecoveryFixture) mutate(mutations ...model.Mutation) {
	fx.t.Helper()
	st, err := store.Open(context.Background(), store.OpenOptions{Path: fx.dbPath, Workflow: matrixWF, CflowVersion: "0.0.0-dev", Now: fx.now})
	if err != nil {
		fx.t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	view, err := st.View(context.Background(), store.StoreQuery{})
	if err != nil {
		fx.t.Fatalf("view: %v", err)
	}
	if _, err := st.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: mutations}, nil
	}); err != nil {
		fx.t.Fatalf("mutate: %v", err)
	}
}

// mergeTask performs one real --no-ff Integration merge.
func (fx *faultRecoveryFixture) mergeTask() string {
	fx.t.Helper()
	flow, err := gitflow.NewGitFlow(fx.sup, fx.repo.Root)
	if err != nil {
		fx.t.Fatalf("new gitflow: %v", err)
	}
	res, err := flow.Execute(context.Background(), gitflow.MergeIntegration{
		Path: fx.integration, Branch: fx.taskBranch,
	})
	if err != nil {
		fx.t.Fatalf("external merge: %v", err)
	}
	fx.mergeCount++
	mr, ok := res.(gitflow.MergeResult)
	if !ok {
		fx.t.Fatalf("external merge result = %T", res)
	}
	return mr.Head
}

func (fx *faultRecoveryFixture) mustReconcile() recovery.ReconciliationOutcome {
	fx.t.Helper()
	out, err := fx.engine.Reconcile(context.Background(), recovery.Scope{Workflow: matrixWF})
	if err != nil {
		fx.t.Fatalf("reconcile: %v", err)
	}
	return out
}

// dispositionsOf returns every disposition for one intent kind name.
func dispositionsOf(out recovery.ReconciliationOutcome, kind string) []recovery.Disposition {
	var outD []recovery.Disposition
	for _, d := range out.Dispositions {
		if fmt.Sprintf("%T", d.Intent) == kind {
			outD = append(outD, d.Disposition)
		}
	}
	return outD
}

func (fx *faultRecoveryFixture) mergeIntent() model.EffectIntent {
	return model.IntegrationMergeIntent{
		Node: "merge-s01", BaseHead: fx.baseHead,
		TaskBranch: fx.taskBranch, VerifiedCommit: fx.taskHead,
	}
}

func injectRecoveryMergeCompleted(t *testing.T, _ matrixRow) rowResult {
	fx := newFaultRecoveryFixture(t)
	fx.seedIntent(fx.mergeIntent())
	fx.mergeTask() // the external merge happened; the Result never committed
	out := fx.mustReconcile()
	ev := evidenceOf("merge_commit_and_intent", "merge_not_repeated")
	if hasDisposition(out, "merge.Intent", recovery.AlreadyCompleted) {
		ev["merge_commit_and_intent"] = true
	}
	if fx.mergeCount == 1 {
		ev["merge_not_repeated"] = true
	}
	return recoveryRow("already_completed", "merge_commit_and_intent", "merge_not_repeated")
}

func injectRecoveryMergeAbsent(t *testing.T, _ matrixRow) rowResult {
	fx := newFaultRecoveryFixture(t)
	fx.seedIntent(fx.mergeIntent())
	out := fx.mustReconcile()
	ev := evidenceOf("intent_pending", "premerge_head_matches")
	if hasDisposition(out, "merge.Intent", recovery.SafeToRetry) {
		ev["intent_pending"] = true
	}
	if fx.mergeCount == 0 {
		ev["premerge_head_matches"] = true
	}
	return recoveryRow("safe_to_retry", "intent_pending", "premerge_head_matches")
}

func injectRecoveryMergeForeign(t *testing.T, _ matrixRow) rowResult {
	fx := newFaultRecoveryFixture(t)
	fx.seedIntent(fx.mergeIntent())
	// A foreign Task Branch (never verified) is merged instead.
	foreign := "cflow/" + matrixWF + "/task-foreign"
	fx.repo.git("branch", foreign, fx.baseHead)
	fx.repo.git("checkout", "-q", foreign)
	writeFile(t, fx.repo.Path("foreign.txt"), "foreign\n")
	fx.repo.git("add", "foreign.txt")
	fx.repo.git("commit", "-q", "-m", "foreign task")
	fx.repo.git("checkout", "-q", "main")
	flow, err := gitflow.NewGitFlow(fx.sup, fx.repo.Root)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	if _, err := flow.Execute(context.Background(), gitflow.MergeIntegration{
		Path: fx.integration, Branch: foreign,
	}); err != nil {
		t.Fatalf("foreign merge: %v", err)
	}
	fx.mergeCount++
	out := fx.mustReconcile()
	ev := evidenceOf("foreign_merge_evidence")
	if hasDisposition(out, "merge.Intent", recovery.BlockedDrift) {
		ev["foreign_merge_evidence"] = true
	}
	return recoveryRow("blocked_drift", "foreign_merge_evidence")
}

func injectRecoveryAuditRefMoved(t *testing.T, _ matrixRow) rowResult {
	fx := newFaultRecoveryFixture(t)
	auditRef := "refs/cflow/" + matrixWF + "/tasks/task-s01/attempts/1"
	flow, err := gitflow.NewGitFlow(fx.sup, fx.repo.Root)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	if _, err := flow.Execute(context.Background(), gitflow.CreateAuditRef{Ref: auditRef, Head: fx.taskHead}); err != nil {
		t.Fatalf("create audit ref: %v", err)
	}
	// The Ref was moved to a different Commit afterwards: evidence changed.
	fx.repo.git("update-ref", auditRef, fx.baseHead)
	fx.seedIntent(model.GitAuditRefCreateIntent{Ref: auditRef, Head: fx.taskHead})
	out := fx.mustReconcile()
	ev := evidenceOf("ref_moved_evidence")
	if hasDisposition(out, "auditRef.Intent", recovery.BlockedDrift) {
		ev["ref_moved_evidence"] = true
	}
	return recoveryRow("blocked_drift", "ref_moved_evidence")
}

func injectRecoveryArtifactOrphan(t *testing.T, _ matrixRow) rowResult {
	fx := newFaultRecoveryFixture(t)
	root := filepath.Join(fx.home, "projects", fx.projectKey, "workflows", matrixWF, "artifacts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir artifacts root: %v", err)
	}
	st, err := artifact.New(root, security.Registry{})
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	ref, err := st.Put(context.Background(), artifact.PutRequest{
		WorkflowID: matrixWF, Type: model.ArtifactReport, Revision: 1,
		SchemaVersion: "1.0.0", CreatedAt: "2026-01-01T00:00:00Z",
		Producer: artifact.ProducerRef{Purpose: "test"},
		Body:     []byte("body-1"),
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	// The content file vanishes after the write: the aggregate wrote it,
	// the file is gone — an orphan the recovery cannot claim.
	path := filepath.Join(root, string(matrixWF), "report", "1", ref.Hash)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove artifact file: %v", err)
	}
	fx.seedIntent(model.ArtifactWriteIntent{
		Ref:      model.ArtifactRef{Workflow: matrixWF, Type: model.ArtifactReport, Revision: 1, Hash: ref.Hash},
		Producer: model.PurposePlanning,
	})
	out := fx.mustReconcile()
	ev := evidenceOf("orphan_directory")
	if hasDisposition(out, "write.Intent", recovery.BlockedDrift) {
		ev["orphan_directory"] = true
	}
	return recoveryRow("blocked_drift", "orphan_directory")
}

func injectRecoveryApplyStaging(t *testing.T, _ matrixRow) rowResult {
	fx := newFaultRecoveryFixture(t)
	fx.mutate(model.ApplyAppendMutation{ApplyAttempt: model.ApplyAttempt{
		ID: "apply-1", Number: 1, Status: model.ApplyStaging,
		TargetHead: fx.taskHead, IntegrationHead: fx.taskHead,
		StartedAt: fx.now(),
	}})
	fx.seedIntent(model.ApplyStagingCreateIntent{Apply: "apply-1"})
	out := fx.mustReconcile()
	ev := evidenceOf("attempt_ledger")
	if hasDisposition(out, "stagingCreate.Intent", recovery.SafeToRetry) {
		ev["attempt_ledger"] = true
	}
	return recoveryRow("safe_to_retry", "attempt_ledger")
}

func injectRecoveryProcessStop(t *testing.T, _ matrixRow) rowResult {
	fx := newFaultRecoveryFixture(t)
	fx.mutate(
		model.SessionAppendMutation{Session: model.Session{ID: "s-repair", Purpose: model.PurposeRepair, Status: model.SessionActive}, Provider: "fake"},
		model.ProcessAppendMutation{Process: model.ProcessRecord{ID: "rp-1", Session: "s-repair", Purpose: model.PurposeRepair, Status: model.ProcessStatusRunning, StartedAt: fx.now()}},
	)
	fx.seedIntent(model.ManagedProcessStopIntent{Process: "rp-1"})
	out := fx.mustReconcile()
	code := ""
	ev := evidenceOf("process_row_persists", "dispatch_never_reopens")
	// hasDisposition matches on fmt.Sprintf("%T", intent), so the probe
	// uses the exact type name (the short names used elsewhere in this
	// suite never match %T; this probe is the live disposition check).
	if hasDisposition(out, "model.ManagedProcessStopIntent", recovery.BlockedDrift) {
		ev["unverified_running_fails_closed"] = true
	}
	// The unverified RUNNING row fails closed: a crash-restarting Runtime
	// has no OS identity to Inspect, so claiming the process stopped would
	// silently settle a provider child that may have survived the killed
	// cflow. Reconcile produced a user-action Fault (manual confirmation)
	// and is read-only, so it can never reopen dispatch on its own.
	if len(out.Faults) > 0 {
		code = string(out.Faults[0].Code)
		if code == "DIRTY_WORKTREE_DRIFTED" {
			ev["user_action_demanded"] = true
		}
	}
	d, dispatch := dispositionDispatch(code)
	return rowResult{Code: code, Disposition: d, RetryCharge: retryChargeOf(code), Dispatch: dispatch, Evidence: ev}
}

func injectRecoveryQuarantineMissing(t *testing.T, _ matrixRow) rowResult {
	fx := newFaultRecoveryFixture(t)
	fx.mutate(model.QuarantineAppendMutation{Quarantine: model.Quarantine{
		ID: "quarantine-1", AuditRef: "refs/cflow/" + matrixWF + "/quarantine/quarantine-1",
		Branch: fx.taskBranch, FromHead: fx.baseHead, ToHead: fx.taskHead,
		Code: model.CodeCommitDuringPolicyDriftWindow,
	}})
	out := fx.mustReconcile()
	code := ""
	ev := evidenceOf("quarantine_evidence_preserved")
	if len(out.Faults) > 0 {
		code = string(out.Faults[0].Code)
		if code == "DIRTY_WORKTREE_DRIFTED" {
			ev["quarantine_evidence_preserved"] = true
		}
	}
	d, dispatch := dispositionDispatch(code)
	return rowResult{Code: code, Disposition: d, RetryCharge: retryChargeOf(code), Dispatch: dispatch, Evidence: ev}
}

func hasDisposition(out recovery.ReconciliationOutcome, kind string, want recovery.Disposition) bool {
	for _, d := range out.Dispositions {
		if fmt.Sprintf("%T", d.Intent) == kind && d.Disposition == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Final Report / export interruption and the phantom-row class
// ---------------------------------------------------------------------------

// completedWorkflowStore builds a fresh CFLOW_HOME whose store carries a
// COMPLETED/SUCCEEDED workflow; withPhantom additionally seeds the
// PurposeRepair resolution Session/Process the request path allocated and
// never settled (the phantom-row class).
func completedWorkflowStore(t *testing.T, withPhantom bool) (string, string) {
	t.Helper()
	home := canonTemp(t)
	path := filepath.Join(home, "cflow.db")
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	ctx := context.Background()
	s, err := store.Open(ctx, store.OpenOptions{Path: path, Workflow: "wf-1", CflowVersion: "0.0.0-dev", Now: now})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.RegisterProject(ctx, "proj-1", canonTemp(t), "proj-1"); err != nil {
		t.Fatalf("register: %v", err)
	}
	mutations := []model.Mutation{
		model.WorkflowMutation{ID: "wf-1", Project: "proj-1",
			Stage: model.StageCompleted, Runtime: model.RuntimeSucceeded,
			TargetBranch: "main", BaseCommit: "base",
			IntegrationBranch: "cflow/wf-1/integration", IntegrationHead: "int-1"},
		model.ApplyAppendMutation{ApplyAttempt: model.ApplyAttempt{
			ID: "apply-1", Number: 1, Status: model.ApplySucceeded,
			TargetHead: "head-1", StartedAt: now(), EndedAt: now(),
		}},
	}
	if withPhantom {
		mutations = append(mutations,
			model.SessionAppendMutation{Session: model.Session{ID: "rs-1", Purpose: model.PurposeRepair, Status: model.SessionStarting}, Provider: "fake"},
			model.ProcessAppendMutation{Process: model.ProcessRecord{ID: "rp-1", Session: "rs-1", Purpose: model.PurposeRepair, Status: model.ProcessStatusRunning, StartedAt: now()}},
		)
	}
	if _, err := s.Transact(ctx, 0, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: mutations,
			Events: []model.Event{{Kind: model.EventWorkflowSucceeded, Workflow: "wf-1", Text: "workflow completed", At: now()}}}, nil
	}); err != nil {
		t.Fatalf("seed completed workflow: %v", err)
	}
	s.Close()
	return path, home
}

func reportFromStore(t *testing.T, path, home string) (observe.Report, store.StoreView) {
	t.Helper()
	ctx := context.Background()
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	s, err := store.Open(ctx, store.OpenOptions{Path: path, Workflow: "wf-1", ReadOnly: true, CflowVersion: "0.0.0-dev", Now: now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	view, err := s.View(ctx, store.StoreQuery{})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	in := observe.ReportInput{
		Build:       observe.BuildInfo{Version: "0.0.0-dev"},
		GeneratedAt: now(),
		State:       view.State,
		Migration: observe.ReportMigration{SchemaVersion: 4, ChecksumsVerified: true,
			Applied: []observe.AppliedMigration{
				{Version: 1, ID: "cflow-001-initial"},
				{Version: 2, ID: "cflow-002-cleanup-apply"},
				{Version: 3, ID: "cflow-003-integration-head"},
				{Version: 4, ID: "cflow-004-apply-staging-head"},
			}},
		Security: observe.ReportSecurity{HomeMode: "0700", FileMode: "0600"},
		EventExport: observe.ReportEventExport{Path: filepath.Join(home, "events.jsonl"),
			From: 1, To: view.NextEventSeq, Stable: true},
	}
	r, err := observe.GenerateReport(in)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	return r, view
}

func injectReportExportInterrupted(t *testing.T, _ matrixRow) rowResult {
	path, home := completedWorkflowStore(t, false)
	// An interrupted events.jsonl export: a truncated file on disk. The
	// report is a read over SQLite + Git facts and never depends on the
	// rebuildable export file (design 21: events.jsonl can always be
	// rebuilt and is never the authoritative recovery stream).
	exportPath := filepath.Join(home, "events.jsonl")
	if err := os.WriteFile(exportPath, []byte("{\"sequence\":1,\"event_type\":\"WORKFLOW_CREATED\"}\n{\"sequence\":2,\"trunc"), 0o600); err != nil {
		t.Fatalf("write truncated export: %v", err)
	}
	r, view := reportFromStore(t, path, home)
	ev := evidenceOf("report_generates", "events_rebuildable", "no_mutation")
	if r.Result == "PASSED" {
		ev["report_generates"] = true
	}
	if r.EventExport.To == view.NextEventSeq {
		ev["events_rebuildable"] = true
	}
	if r.Workflow.Runtime == model.RuntimeSucceeded {
		ev["no_mutation"] = true
	}
	return noFaultRow("already_completed", "report_generates", "events_rebuildable", "no_mutation")
}

func injectApplyPhantomRow(t *testing.T, _ matrixRow) rowResult {
	path, home := completedWorkflowStore(t, true)
	r, _ := reportFromStore(t, path, home)
	ev := evidenceOf("repair_row_persists", "report_risk_mentions_repair")
	for _, s := range r.Sessions {
		if s.Purpose == model.PurposeRepair && s.Status != model.SessionCompleted {
			ev["repair_row_persists"] = true
		}
	}
	for _, risk := range r.Risks {
		if strings.Contains(risk, "rp-1") && strings.Contains(risk, "RUNNING") {
			ev["report_risk_mentions_repair"] = true
		}
	}
	// The report still classifies the completed workflow PASSED; the phantom
	// row surfaces as a remaining risk instead of vanishing.
	if r.Result != "PASSED" {
		t.Fatalf("result = %s, want PASSED with the phantom row surfaced", r.Result)
	}
	return rowResult{Code: "", Disposition: "phantom_surfaced", RetryCharge: retryChargeOf(""), Dispatch: "open", Evidence: ev}
}

// ---------------------------------------------------------------------------
// registry
// ---------------------------------------------------------------------------

func init() {
	// migration/backup/manifest
	faultInjectors["migration_crash_before_manifest"] = injectMigrationCrashBeforeManifest
	faultInjectors["migration_crash_after_manifest"] = injectMigrationCrashAfterManifest
	faultInjectors["migration_checksum_mutated"] = injectMigrationChecksumMutated
	faultInjectors["migration_schema_too_new"] = injectMigrationSchemaTooNew
	faultInjectors["migration_path_missing"] = injectMigrationPathMissing
	faultInjectors["migration_guard_mismatch"] = injectMigrationGuardMismatch
	// SQLite commit/lock/version/contention
	faultInjectors["store_sqlite_busy"] = injectStoreSQLiteBusy
	faultInjectors["store_stale_version"] = injectStoreStaleVersion
	faultInjectors["store_constraint_atomic"] = injectStoreConstraintAtomic
	faultInjectors["store_db_failure"] = injectStoreDBFailure
	// immutable Artifact Store
	faultInjectors["artifact_crash_before_rename"] = injectArtifactCrashBeforeRename
	faultInjectors["artifact_target_contended"] = injectArtifactTargetContended
	faultInjectors["artifact_schema_unsupported"] = injectArtifactSchemaUnsupported
	faultInjectors["artifact_old_revision"] = injectArtifactOldRevision
	faultInjectors["artifact_content_mutated"] = injectArtifactContentMutated
	// process stop escalation
	faultInjectors["process_stop_escalation"] = injectProcessStopEscalation
	faultInjectors["process_stop_orphan"] = injectProcessStopOrphan
	// provider protocol
	faultInjectors["provider_protocol_violation"] = injectProviderProtocolViolation
	faultInjectors["provider_session_id_missing"] = injectProviderSessionIDMissing
	faultInjectors["provider_protocol_unsupported"] = injectProviderProtocolUnsupported
	faultInjectors["provider_binding_drift"] = injectProviderBindingDrift
	// git identity/signing/policy
	faultInjectors["git_identity_missing"] = injectGitIdentityMissing
	faultInjectors["git_verify_policy_mismatch"] = injectGitVerifyPolicyMismatch
	faultInjectors["git_signing_preflight_failed"] = injectGitSigningPreflightFailed
	// recovery dispositions
	faultInjectors["recovery_merge_completed"] = injectRecoveryMergeCompleted
	faultInjectors["recovery_merge_absent"] = injectRecoveryMergeAbsent
	faultInjectors["recovery_merge_foreign"] = injectRecoveryMergeForeign
	faultInjectors["recovery_audit_ref_moved"] = injectRecoveryAuditRefMoved
	faultInjectors["recovery_artifact_orphan"] = injectRecoveryArtifactOrphan
	faultInjectors["recovery_apply_staging"] = injectRecoveryApplyStaging
	faultInjectors["recovery_process_stop"] = injectRecoveryProcessStop
	faultInjectors["recovery_quarantine_missing"] = injectRecoveryQuarantineMissing
	// report/export and the phantom-row class
	faultInjectors["report_export_interrupted"] = injectReportExportInterrupted
	faultInjectors["apply_phantom_row"] = injectApplyPhantomRow
	// security guard paths
	faultInjectors["path_symlink_escape"] = injectPathSymlinkEscape
	faultInjectors["path_group_writable"] = injectPathGroupWritable
	faultInjectors["path_home_unsafe_mode"] = injectPathHomeUnsafeMode
	// redactor fail-closed and short-body
	faultInjectors["redaction_fail_closed"] = injectRedactionFailClosed
	faultInjectors["redaction_short_body"] = injectRedactionShortBody
}
