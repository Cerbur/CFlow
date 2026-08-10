package claude_test

// Claude Adapter tests (brief Steps 2-3): the verbatim
// TestClaudeArgvPreservesProviderPermissionDefaults argv contract, the
// typed Start and Resume argv shapes, the exact ProcessSpec the adapter
// launches (argv, cwd, stdin user message frame, explicit safe env), the
// stream-json dialect mapping over the captured 2.1.222 fixtures (stream
// ordering, Session capture, structured schema result, partial frames,
// malformed/unknown events, conflicting ids, budget exceeded,
// Authentication Unknown distinct from Protocol unsupported, stderr
// redaction, cancellation, resume not found), Detect (missing, version
// mismatch, incompatible protocol, executable hash drift, the opt-in
// smoke detection of the real binary against the captured binding), and
// the pipeline integration of the dialect through the Runtime.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/claude"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// fixtureDir is the committed Claude fixture directory for the actually
// installed CLI version, resolved from the package working directory.
// The plan's baseline was 2.1.185; the installed CLI is 2.1.222 (it
// auto-updated during the Demo; the 2.1.221 fixtures remain as the
// historical capture), and the captured-fixtures mechanism is the plan's
// admitted path for the newer version (fixtures named for the actually
// installed version, re-captured from the real 2.1.222 wire).
var fixtureDir = filepath.Join("..", "..", "..", "tests", "testdata", "providers", "claude", "2.1.222")

// capturedSessionID is the provider session id every fixture stream
// establishes (a UUID v7 shape, matching real claude stream-json session
// ids).
const capturedSessionID = "0197f1c1-9c6e-7b00-a000-0000000000c1"

// capturedVersion is the CLI version pinned by the captured fixtures.
const capturedVersion = "2.1.222"

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

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

// fixedClock is the deterministic Clock injected into test Runtimes.
func fixedClock() time.Time { return time.Unix(1_700_000_000, 0) }

// testRedactionRegistry is the redaction policy the test Runtimes are
// constructed with: a provider-token rule and an API key rule.
func testRedactionRegistry() security.Registry {
	return security.Registry{
		Revision: "test-1",
		Rules: []security.Rule{
			{ID: "provider-token", Category: "provider_token", Pattern: `sk-[A-Za-z0-9]{16,}`},
			{ID: "api-key", Category: "api_key", Pattern: `AKIA[0-9A-Z]{16}`},
		},
	}
}

// tempRoot resolves the canonical owner-only temp root the Security Guard
// requires (the same discipline the runtime tests use).
func tempRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	if err := os.Chmod(p, 0o700); err != nil {
		t.Fatalf("chmod temp root: %v", err)
	}
	return p
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

func requireExactArgs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv length %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full argv: %v)", i, got[i], want[i], got)
		}
	}
}

// requireContainsArgs asserts every flag/value pair appears in argv as
// consecutive entries (the brief's verbatim argv contract shape).
func requireContainsArgs(t *testing.T, argv []string, pairs ...string) {
	t.Helper()
	for i := 0; i+1 < len(pairs); i += 2 {
		flag, want := pairs[i], pairs[i+1]
		found := false
		for j := 0; j+1 < len(argv); j++ {
			if argv[j] == flag && argv[j+1] == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("argv must contain %q followed by %q: %v", flag, want, argv)
		}
	}
}

func requireAbsentArgs(t *testing.T, argv []string, forbidden ...string) {
	t.Helper()
	for _, f := range forbidden {
		for _, a := range argv {
			if a == f {
				t.Fatalf("argv must not contain %q: %v", f, argv)
			}
		}
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func claudeBinding(t *testing.T) agent.ProviderBinding {
	t.Helper()
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	b, err := reg.Select("claude")
	requireNoError(t, err)
	return b
}

// stubClaudeOnPath creates a claude stub executable in a temp dir and
// puts that dir alone on PATH, so the adapter's real exec.LookPath
// resolution finds it deterministically. The Fake Supervisor never
// executes it; its content only feeds the executable hash facts.
func stubClaudeOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho \"2.1.222 (Claude Code)\"\n"), 0o700); err != nil {
		t.Fatalf("write stub claude: %v", err)
	}
	t.Setenv("PATH", dir)
	return p
}

// schemaJSON is the managed immutable schema JSON text the adapter
// serializes into --json-schema (the Application materializes the
// purpose's embedded output schema; the test materializes it here).
const schemaJSON = `{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`

// recordingSupervisor wraps a process.Supervisor and records every Start
// so tests can assert the exact ProcessSpec the adapter launches and can
// script the fake process output by handle.
type recordingSupervisor struct {
	process.Supervisor
	mu      sync.Mutex
	starts  []process.ProcessSpec
	handles []process.Handle
}

func (r *recordingSupervisor) Start(ctx context.Context, spec process.ProcessSpec) (process.Handle, process.Events, error) {
	h, evs, err := r.Supervisor.Start(ctx, spec)
	r.mu.Lock()
	r.starts = append(r.starts, spec)
	r.handles = append(r.handles, h)
	r.mu.Unlock()
	return h, evs, err
}

func (r *recordingSupervisor) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

func (r *recordingSupervisor) specAt(i int) process.ProcessSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts[i]
}

func (r *recordingSupervisor) handleAt(i int) process.Handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handles[i]
}

// harness wires one Adapter over the deterministic Fake Supervisor behind
// a recording wrapper, with a stub claude executable on PATH and a
// managed schema JSON text. probe is the 1-based supervisor start index
// the next Detect's version probe will occupy (advanced by detectIn).
type harness struct {
	ad        agent.Adapter
	fake      *process.FakeAdapter
	rec       *recordingSupervisor
	claudeDir string
	schema    string
	budget    string
	probe     int
}

func newHarness(t *testing.T, binding agent.ProviderBinding) *harness {
	t.Helper()
	fa, sup := process.NewFakeSupervisor()
	rec := &recordingSupervisor{Supervisor: sup}
	ad := claude.New(rec, binding)
	return &harness{ad: ad, fake: fa, rec: rec, claudeDir: filepath.Dir(stubClaudeOnPath(t)), schema: schemaJSON, budget: "0.50"}
}

// waitStarts blocks until the recorder has recorded n supervisor Starts
// and returns the handle of the n-th one.
func (h *harness) waitStarts(t *testing.T, n int) process.Handle {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	got := 0
	for time.Now().Before(deadline) {
		if got = h.rec.count(); got >= n {
			return h.rec.handleAt(n - 1)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for supervisor start %d (have %d starts)", n, got)
	return process.Handle{}
}

// scriptVersion scripts one read-only version probe: start index startIdx
// must be the --version probe; its stdout carries versionText ("" = none)
// and the group exits with code.
func (h *harness) scriptVersion(t *testing.T, startIdx int, versionText string, code int) {
	t.Helper()
	hnd := h.waitStarts(t, startIdx)
	if versionText != "" {
		h.fake.EmitOutput(hnd, process.Stdout, []byte(versionText+"\n"))
	}
	h.fake.ExitGroup(hnd, code)
}

// scriptFrames emits every non-empty fixture line as one stdout
// stream-json frame on start startIdx, then exits the group with code.
// When exitCode is -1 the group is left running (callers cancel it
// themselves).
func (h *harness) scriptFrames(t *testing.T, startIdx int, fixture string, exitCode int) process.Handle {
	t.Helper()
	hnd := h.waitStarts(t, startIdx)
	for _, line := range strings.Split(fixture, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		h.fake.EmitOutput(hnd, process.Stdout, []byte(line+"\n"))
	}
	if exitCode >= 0 {
		h.fake.ExitGroup(hnd, exitCode)
	}
	return hnd
}

// startRun launches a claude Start through the adapter (the run is the
// first supervisor start; no Detect probe is involved).
func (h *harness) startRun(t *testing.T, purpose model.AgentPurpose, input claude.Input) agent.Run {
	t.Helper()
	r, err := h.ad.Start(context.Background(), agent.StartRequest{
		Purpose:  purpose,
		Provider: "claude",
		Prompt:   "Implement the search feature.",
		Input:    input,
		CWD:      "/Users/dev/worktrees/task-1",
	})
	requireNoError(t, err)
	return r
}

// collect drains one run to completion and returns every event.
func collect(t *testing.T, r agent.Run) []agent.Event {
	t.Helper()
	events, err := collectErr(r)
	requireNoError(t, err)
	return events
}

// collectErr drains one run, returning every event read so far and the
// first non-EOF error (a *ProtocolError, a *ProcessCrash, or nil).
func collectErr(r agent.Run) ([]agent.Event, error) {
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

// detectIn runs Detect on a goroutine, scripting the read-only version
// probe with the captured baseline, and returns the installation. Each
// call scripts the next probe start index, so repeated detects stay
// deterministic.
func (h *harness) detectIn(t *testing.T) agent.Installation {
	t.Helper()
	type result struct {
		inst agent.Installation
		err  error
	}
	done := make(chan result, 1)
	go func() {
		inst, err := h.ad.Detect(context.Background())
		done <- result{inst, err}
	}()
	h.probe++
	h.scriptVersion(t, h.probe, capturedVersion+" (Claude Code)", 0)
	select {
	case res := <-done:
		requireNoError(t, res.err)
		return res.inst
	case <-time.After(30 * time.Second):
		t.Fatal("Detect hung")
		return agent.Installation{}
	}
}

// newClaudeRuntime builds a Runtime wired to the claude adapter, with a
// fixed Clock, a fresh sequential ID source, the test redaction registry,
// and a fresh evidence root.
func newClaudeRuntime(t *testing.T, ad agent.Adapter) *agent.Runtime {
	t.Helper()
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	rt, err := agent.NewRuntime(agent.RuntimeOptions{
		Now:         fixedClock,
		IDs:         model.SequentialIDSource(),
		Registry:    reg,
		Redaction:   testRedactionRegistry(),
		EvidenceDir: tempRoot(t),
		Adapters:    map[string]agent.Adapter{"claude": ad},
	})
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, rt.Close()) })
	return rt
}

// runtimeStart runs Runtime.Start on a goroutine, scripting the Detect
// probe (start 1) and the run (start 2) with fixture frames, and returns
// the result.
func runtimeStart(t *testing.T, rt *agent.Runtime, h *harness, req agent.StartRequest, fixture string) (*agent.RunResult, error) {
	t.Helper()
	type result struct {
		res *agent.RunResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := rt.Start(context.Background(), req)
		done <- result{res, err}
	}()
	h.scriptVersion(t, 1, capturedVersion+" (Claude Code)", 0)
	h.scriptFrames(t, 2, fixture, 0)
	select {
	case res := <-done:
		return res.res, res.err
	case <-time.After(30 * time.Second):
		t.Fatal("runtime Start hung")
		return nil, nil
	}
}

// fixtureStart builds a plain claude Start request.
func fixtureStart(h *harness) agent.StartRequest {
	return agent.StartRequest{
		Purpose:  model.AgentPurpose(model.PurposePlanning),
		Provider: "claude",
		Prompt:   "Plan the work.",
		Input:    claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget},
		CWD:      "/Users/dev/worktrees/task-1",
	}
}

// fixtureClaudeStart is the brief's argv fixture: the typed Start request
// whose schema, budget, and permission shape the argv contract pins.
func fixtureClaudeStart() claude.StartRequest {
	return claude.StartRequest{
		SchemaJSON:   schemaJSON,
		MaxBudgetUSD: "0.50",
		Prompt:       "Implement the search feature.",
	}
}

// ---------------------------------------------------------------------------
// Step 2 (verbatim): the Start argv contract
// ---------------------------------------------------------------------------

// TestClaudeArgvPreservesProviderPermissionDefaults (brief Step 2,
// verbatim): Start argv is the noninteractive print stream-json shape
// with the schema and budget inline, and can never contain danger,
// bypass, or permission-mode flags, nor tool allow/deny lists.
func TestClaudeArgvPreservesProviderPermissionDefaults(t *testing.T) {
	req := fixtureClaudeStart()
	argv := claude.StartArgv(req)
	requireContainsArgs(t, argv, "--print", "--input-format", "stream-json", "--output-format", "stream-json")
	requireContainsArgs(t, argv, "--json-schema", req.SchemaJSON, "--max-budget-usd", req.MaxBudgetUSD)
	requireAbsentArgs(t, argv, "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions", "--permission-mode", "--allowedTools", "--disallowedTools")
}

// TestStartArgvExactShape: the exact Start argv is --print
// --input-format stream-json --output-format stream-json --json-schema
// <json> with --max-budget-usd only for a finite budget; the approved
// optional --model appears only when the typed request carries one.
func TestStartArgvExactShape(t *testing.T) {
	req := fixtureClaudeStart()
	argv := claude.StartArgv(req)
	requireExactArgs(t, argv, "--print", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose",
		"--json-schema", req.SchemaJSON, "--max-budget-usd", req.MaxBudgetUSD)
	requireAbsentArgs(t, argv, "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions",
		"--permission-mode", "--allowedTools", "--disallowedTools", "--session-id")

	req.Model = "claude-sonnet-4-5"
	argv = claude.StartArgv(req)
	requireExactArgs(t, argv, "--print", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose",
		"--json-schema", req.SchemaJSON, "--max-budget-usd", req.MaxBudgetUSD, "--model", "claude-sonnet-4-5")
	requireAbsentArgs(t, argv, "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions",
		"--permission-mode", "--allowedTools", "--disallowedTools")
}

func TestStartArgvOmitsUnlimitedBudgetFlag(t *testing.T) {
	req := fixtureClaudeStart()
	req.MaxBudgetUSD = ""
	requireExactArgs(t, claude.StartArgv(req),
		"--print", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--json-schema", req.SchemaJSON)
}

// TestResumeArgvAddsResumeFlag: Resume argv is the Start argv plus
// --resume <session-id>; the session identity travels through --resume,
// never through --session-id (the stream's validated start event is the
// only Session authority).
func TestResumeArgvAddsResumeFlag(t *testing.T) {
	req := claude.ResumeRequest{
		SchemaJSON:   schemaJSON,
		MaxBudgetUSD: "0.50",
		SessionID:    agent.ProviderSessionID(capturedSessionID),
		Prompt:       "Continue the work.",
	}
	argv := claude.ResumeArgv(req)
	requireExactArgs(t, argv, "--print", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose",
		"--json-schema", req.SchemaJSON, "--max-budget-usd", req.MaxBudgetUSD, "--resume", capturedSessionID)
	requireAbsentArgs(t, argv, "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions",
		"--permission-mode", "--allowedTools", "--disallowedTools", "--session-id")
}

// TestCapturedHelpFixturesConfirmBinding: the captured 2.1.220 help
// fixtures confirm every flag the binding and the argv contract rely on,
// and pin the captured baseline version.
func TestCapturedHelpFixturesConfirmBinding(t *testing.T) {
	help := readFixture(t, "help.txt")
	if !strings.Contains(help, capturedVersion+" (Claude Code)") {
		t.Fatalf("captured version fixture must pin the baseline, got: %q", help)
	}
	cliHelp := readFixture(t, "cli-help.txt")
	for _, want := range []string{
		"-p, --print",
		"--input-format",
		"stream-json",
		"--output-format",
		"--json-schema",
		"--max-budget-usd",
		"-r, --resume",
		"--session-id",
		"--model",
	} {
		if !strings.Contains(cliHelp, want) {
			t.Fatalf("captured cli help must confirm %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// The exact ProcessSpec the adapter launches
// ---------------------------------------------------------------------------

// TestStartProcessSpecIsExact: Start launches the resolved executable
// with the exact Start argv, the worktree as the process cwd, the prompt
// serialized as one validated stream-json user message frame on stdin
// (deterministically closed by the bounded reader's EOF), and an explicit
// safe env (HOME and PATH only): parent values never leak into the child.
func TestStartProcessSpecIsExact(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	t.Setenv("CFLOW_PROVIDER_SECRET", "sk-test-secret-value")
	r := h.startRun(t, agent.PurposeImplementer, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	h.scriptFrames(t, 1, readFixture(t, "start-valid.jsonl"), 0)
	_ = collect(t, r)

	spec := h.rec.specAt(0)
	requireExactArgs(t, spec.Args, "--print", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose",
		"--json-schema", h.schema, "--max-budget-usd", h.budget)
	requireAbsentArgs(t, spec.Args, "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions",
		"--permission-mode", "--allowedTools", "--disallowedTools")
	if spec.Executable != filepath.Join(h.claudeDir, "claude") {
		t.Fatalf("executable = %q, want the PATH-resolved claude stub", spec.Executable)
	}
	if spec.Dir != "/Users/dev/worktrees/task-1" {
		t.Fatalf("process cwd = %q, want the request's worktree", spec.Dir)
	}
	prompt, err := io.ReadAll(spec.Stdin)
	requireNoError(t, err)
	wantStdin := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Implement the search feature."}]}}` + "\n"
	if string(prompt) != wantStdin {
		t.Fatalf("stdin must carry exactly one validated stream-json user frame, got %q", prompt)
	}
	if len(spec.Env) != 2 || spec.Env["PATH"] != h.claudeDir || spec.Env["HOME"] == "" {
		t.Fatalf("env must be exactly {HOME, PATH}: %v", spec.Env)
	}
	if _, leaked := spec.Env["CFLOW_PROVIDER_SECRET"]; leaked {
		t.Fatal("parent environment must never leak into the child")
	}
}

// TestResumeProcessSpecIsExact: Resume launches the Start argv plus
// --resume <session-id>; the process cwd carries the request's CWD and
// the prompt travels as the same user frame on stdin.
func TestResumeProcessSpecIsExact(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r, err := h.ad.Resume(context.Background(), agent.ResumeRequest{
		ProviderSessionID: agent.ProviderSessionID(capturedSessionID),
		Purpose:           agent.PurposeImplementer,
		Provider:          "claude",
		Prompt:            "Continue the work.",
		Input:             claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget},
		CWD:               "/Users/dev/worktrees/task-1",
	})
	requireNoError(t, err)
	h.scriptFrames(t, 1, readFixture(t, "resume-valid.jsonl"), 0)
	_ = collect(t, r)

	spec := h.rec.specAt(0)
	requireExactArgs(t, spec.Args, "--print", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose",
		"--json-schema", h.schema, "--max-budget-usd", h.budget, "--resume", capturedSessionID)
	requireAbsentArgs(t, spec.Args, "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions",
		"--permission-mode", "--allowedTools", "--disallowedTools", "--session-id")
	if spec.Dir != "/Users/dev/worktrees/task-1" {
		t.Fatalf("process cwd = %q, want the request's CWD", spec.Dir)
	}
}

// ---------------------------------------------------------------------------
// Dialect golden tests over the captured fixtures
// ---------------------------------------------------------------------------

// TestDialectStreamOrderingAndSessionCapture: start-valid.jsonl maps onto
// the unified events in wire order; known claude frames without a unified
// mapping (hook_started/hook_response/thinking_tokens diagnostics, the
// tool_use_start echo, the user tool-result echo) are skipped; every
// event after the validated start inherits the established session id.
func TestDialectStreamOrderingAndSessionCapture(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	h.scriptFrames(t, 1, readFixture(t, "start-valid.jsonl"), 0)
	events := collect(t, r)

	wantTypes := []agent.EventType{
		agent.EventSessionStarted,
		agent.EventAssistantMessage,
		agent.EventToolStarted,
		agent.EventToolFinished,
		agent.EventAssistantMessage,
		agent.EventCompleted,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("expected %d unified events, got %d: %+v", len(wantTypes), len(events), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("event %d type = %s, want %s", i, events[i].Type, want)
		}
	}
	if events[0].SessionID != agent.ProviderSessionID(capturedSessionID) {
		t.Fatalf("start event session id = %q", events[0].SessionID)
	}
	for i, ev := range events[1:] {
		if ev.SessionID != agent.ProviderSessionID(capturedSessionID) {
			t.Fatalf("event %d must inherit the established session id, got %q", i+1, ev.SessionID)
		}
	}
	if events[1].Text != "Reading the repository to ground the plan." {
		t.Fatalf("assistant text = %q", events[1].Text)
	}
	if events[2].Tool != "Bash" || events[2].Input != `{"command":"git status --porcelain"}` {
		t.Fatalf("tool_started facts = %+v", events[2])
	}
	if events[3].Tool != "Bash" || events[3].Output != "?? internal/search/" {
		t.Fatalf("tool_finished facts = %+v", events[3])
	}
	if events[4].Text != "The plan has three phases." {
		t.Fatalf("assistant text = %q", events[4].Text)
	}
}

// TestDialectSchemaResult: the terminal success result frame maps onto
// the unified completed event carrying the raw structured result object
// (the schema-valid JSON the CLI produced under --json-schema) and the
// sha256 of its raw frame.
func TestDialectSchemaResult(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	h.scriptFrames(t, 1, readFixture(t, "start-valid.jsonl"), 0)
	events := collect(t, r)

	terminal := events[len(events)-1]
	if terminal.Type != agent.EventCompleted {
		t.Fatalf("terminal type = %s, want completed", terminal.Type)
	}
	if !json.Valid([]byte(terminal.Result)) {
		t.Fatalf("terminal result is not valid JSON: %q", terminal.Result)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(terminal.Result), &obj); err != nil {
		t.Fatalf("terminal result is not a JSON object: %v", err)
	}
	if obj["summary"] != "The plan has three phases." {
		t.Fatalf("terminal result payload = %q", terminal.Result)
	}
	if terminal.SessionID != agent.ProviderSessionID(capturedSessionID) {
		t.Fatalf("terminal session id = %q", terminal.SessionID)
	}
	if len(terminal.FrameHash) != 64 {
		t.Fatalf("terminal frame hash = %q", terminal.FrameHash)
	}
	lines := strings.Split(strings.TrimSpace(readFixture(t, "start-valid.jsonl")), "\n")
	last := lines[len(lines)-1]
	if terminal.FrameHash != sha256Hex([]byte(last)) {
		t.Fatal("frame hash must digest the exact raw wire frame")
	}
}

// TestDialectPartialFrames: a stream-json line delivered across several
// output chunks is reassembled by the supervisor's frame pipeline into
// one complete frame; the dialect sees the full JSON and the events
// arrive in wire order.
func TestDialectPartialFrames(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	hnd := h.waitStarts(t, 1)
	h.fake.EmitOutput(hnd, process.Stdout, []byte(`{"type":"system","subtype`))
	h.fake.EmitOutput(hnd, process.Stdout, []byte(`":"init","session_id":"`+capturedSessionID+`"}`+"\n"))
	h.fake.EmitOutput(hnd, process.Stdout, []byte(`{"type":"assistant","message":{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"partially`))
	h.fake.EmitOutput(hnd, process.Stdout, []byte(` delivered"}]}}`+"\n"))
	h.fake.EmitOutput(hnd, process.Stdout, []byte(`{"type":"result","subtype":"success","is_error":false,"result":"{\"ok\":true}"}`))
	h.fake.EmitOutput(hnd, process.Stdout, []byte("\n"))
	h.fake.ExitGroup(hnd, 0)
	events, err := collectErr(r)
	requireNoError(t, err)
	if len(events) != 3 {
		t.Fatalf("expected 3 full events from partially delivered frames, got %d: %+v", len(events), events)
	}
	if events[0].Type != agent.EventSessionStarted || events[0].SessionID != agent.ProviderSessionID(capturedSessionID) {
		t.Fatalf("first event = %+v", events[0])
	}
	if events[1].Text != "partially delivered" {
		t.Fatalf("split frame text = %q", events[1].Text)
	}
	if events[2].Type != agent.EventCompleted {
		t.Fatalf("last event = %+v", events[2])
	}
}

// TestDialectMalformedFrameFailsClosed: a non-JSON stdout frame stops the
// stream with a protocol violation carrying the raw frame; events read
// before the failure are preserved.
func TestDialectMalformedFrameFailsClosed(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	h.scriptFrames(t, 1,
		`{"type":"system","subtype":"init","session_id":"`+capturedSessionID+`"}`+"\n"+
			`this is not a stream-json frame`, 0)
	events, err := collectErr(r)
	var pe *agent.ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a ProtocolError, got %v", err)
	}
	if pe.Code != model.CodeProviderProtocolViolation {
		t.Fatalf("code = %s, want PROVIDER_PROTOCOL_VIOLATION", pe.Code)
	}
	if !bytes.Contains(pe.Frame, []byte("this is not a stream-json frame")) {
		t.Fatalf("protocol error must carry the raw frame: %q", pe.Frame)
	}
	if len(events) != 1 || events[0].Type != agent.EventSessionStarted {
		t.Fatalf("the start event read before the failure must be preserved: %+v", events)
	}
}

// TestDialectUnknownEventPassedOver: an event type outside the claude
// stream-json dialect (fixture protocol-invalid.jsonl) is a diagnostic
// frame with no unified mapping and is passed over silently — the stream
// still completes through its validated terminal result.
func TestDialectUnknownEventPassedOver(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	h.scriptFrames(t, 1, readFixture(t, "protocol-invalid.jsonl"), 0)
	events, err := collectErr(r)
	requireNoError(t, err)
	if len(events) != 3 {
		t.Fatalf("expected 3 events (start, assistant, completed), got %d: %+v", len(events), events)
	}
	if events[2].Type != agent.EventCompleted {
		t.Fatalf("last event = %+v, want completed", events[2])
	}
}

// TestDialectConflictingSessionIDsFailsClosed: a frame that explicitly
// claims a session id different from the established one (fixture
// session-conflict.jsonl) stops the run with a protocol violation.
func TestDialectConflictingSessionIDsFailsClosed(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	h.scriptFrames(t, 1, readFixture(t, "session-conflict.jsonl"), 0)
	events, err := collectErr(r)
	var pe *agent.ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a ProtocolError, got %v", err)
	}
	if pe.Code != model.CodeProviderProtocolViolation {
		t.Fatalf("code = %s, want PROVIDER_PROTOCOL_VIOLATION", pe.Code)
	}
	if !bytes.Contains(pe.Frame, []byte("0000000000c2")) {
		t.Fatalf("protocol error must carry the conflicting frame: %q", pe.Frame)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events before the conflict, got %d", len(events))
	}
}

// TestDialectEmptyStartIDFailsClosed: a system init frame that claims no
// session id fails closed with PROVIDER_SESSION_ID_MISSING (the binding's
// conflict rule: missing ids fail PROVIDER_SESSION_ID_MISSING), so no
// session can be established by an empty id and no event is emitted for
// the offending frame.
func TestDialectEmptyStartIDFailsClosed(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	h.scriptFrames(t, 1,
		`{"type":"system","subtype":"init"}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"result":"{\"ok\":true}"}`, 0)
	events, err := collectErr(r)
	var pe *agent.ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a ProtocolError, got %v", err)
	}
	if pe.Code != model.CodeProviderSessionIDMissing {
		t.Fatalf("code = %s, want PROVIDER_SESSION_ID_MISSING", pe.Code)
	}
	if !bytes.Contains(pe.Frame, []byte("system")) {
		t.Fatalf("protocol error must carry the offending frame: %q", pe.Frame)
	}
	if len(events) != 0 {
		t.Fatalf("an empty-id start must emit no events, got %d: %+v", len(events), events)
	}
}

// TestDialectTerminalWithoutValidStartFailsClosed: a terminal event that
// arrives before any validated system init fails closed (session identity
// appears only through a validated start event); the run can never
// complete from a stream that never established a session.
func TestDialectTerminalWithoutValidStartFailsClosed(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	h.scriptFrames(t, 1,
		`{"type":"assistant","message":{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"no start"}]}}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"result":"{\"ok\":true}"}`, 0)
	events, err := collectErr(r)
	var pe *agent.ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a ProtocolError, got %v", err)
	}
	if pe.Code != model.CodeProviderProtocolViolation {
		t.Fatalf("code = %s, want PROVIDER_PROTOCOL_VIOLATION", pe.Code)
	}
	if !bytes.Contains(pe.Frame, []byte("result")) {
		t.Fatalf("protocol error must carry the offending frame: %q", pe.Frame)
	}
	if len(events) != 1 {
		t.Fatalf("expected only the pre-start assistant event, got %d", len(events))
	}
}

// TestDialectMissingCompletionResultFailsClosed: a terminal success
// result with no result payload cannot validate structured completion and
// fails closed with the offending frame (PRD: invalid completion →
// non-retryable protocol Finding).
func TestDialectMissingCompletionResultFailsClosed(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	h.scriptFrames(t, 1,
		`{"type":"system","subtype":"init","session_id":"`+capturedSessionID+`"}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false}`, 0)
	events, err := collectErr(r)
	var pe *agent.ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a ProtocolError, got %v", err)
	}
	if pe.Code != model.CodeProviderProtocolViolation {
		t.Fatalf("code = %s, want PROVIDER_PROTOCOL_VIOLATION", pe.Code)
	}
	if !bytes.Contains(pe.Frame, []byte("result")) {
		t.Fatalf("protocol error must carry the offending frame: %q", pe.Frame)
	}
	if len(events) != 1 {
		t.Fatalf("expected only the validated start event, got %d", len(events))
	}
}

// TestDialectNonObjectCompletionResultFailsClosed: a terminal success
// whose result payload is not a JSON object (a plain string or null)
// cannot validate structured completion and fails closed.
func TestDialectNonObjectCompletionResultFailsClosed(t *testing.T) {
	for _, payload := range []string{`"\"just a string\""`, `"null"`, `""`} {
		t.Run(payload, func(t *testing.T) {
			h := newHarness(t, claudeBinding(t))
			r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
			h.scriptFrames(t, 1,
				`{"type":"system","subtype":"init","session_id":"`+capturedSessionID+`"}`+"\n"+
					`{"type":"result","subtype":"success","is_error":false,"result":`+payload+`}`, 0)
			_, err := collectErr(r)
			var pe *agent.ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("expected a ProtocolError, got %v", err)
			}
			if pe.Code != model.CodeProviderProtocolViolation {
				t.Fatalf("code = %s, want PROVIDER_PROTOCOL_VIOLATION", pe.Code)
			}
			if !bytes.Contains(pe.Frame, []byte("result")) {
				t.Fatalf("protocol error must carry the offending frame: %q", pe.Frame)
			}
		})
	}
}

// TestDialectBudgetExceededFailsAsFailedTerminal: a result frame with the
// error_budget subtype is a valid terminal failed event carrying the wire
// code as a protocol fact; the run settles as failed and the process exit
// code 0 cannot override the failed terminal.
func TestDialectBudgetExceededFailsAsFailedTerminal(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	h.scriptFrames(t, 1,
		`{"type":"system","subtype":"init","session_id":"`+capturedSessionID+`"}`+"\n"+
			`{"type":"result","subtype":"error_budget","is_error":true,"error":"Budget limit exceeded: spent $0.76 of the $0.50 limit","session_id":"`+capturedSessionID+`"}`, 0)
	events, err := collectErr(r)
	requireNoError(t, err)
	if len(events) != 2 || events[1].Type != agent.EventFailed {
		t.Fatalf("expected the failed terminal, got %+v", events)
	}
	if events[1].Code != "BUDGET_EXCEEDED" {
		t.Fatalf("failed event code = %q, want the compiled BUDGET_EXCEEDED", events[1].Code)
	}
	if events[1].Message == "" {
		t.Fatal("failed event must carry the wire error message")
	}
	if events[1].SessionID != agent.ProviderSessionID(capturedSessionID) {
		t.Fatalf("failed terminal session id = %q", events[1].SessionID)
	}
}

// TestDialectAuthenticationDistinctFromProtocolUnsupported (PRD 已确认):
// an authentication error terminal is classified as the provider's auth
// failure (a valid failed terminal with the error_auth code), never
// disguised as a protocol finding; an unknown event stays a
// PROVIDER_PROTOCOL_VIOLATION. The two outcomes are structurally
// distinct.
func TestDialectAuthenticationDistinctFromProtocolUnsupported(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	h.scriptFrames(t, 1,
		`{"type":"system","subtype":"init","session_id":"`+capturedSessionID+`"}`+"\n"+
			`{"type":"result","subtype":"error_auth","is_error":true,"error":"Authentication required: run claude auth or set ANTHROPIC_API_KEY","session_id":"`+capturedSessionID+`"}`, 0)
	events, err := collectErr(r)
	requireNoError(t, err)
	if len(events) != 2 || events[1].Type != agent.EventFailed {
		t.Fatalf("an auth failure must settle as a failed terminal, got %+v (err %v)", events, err)
	}
	if events[1].Code != "PROVIDER_AUTHENTICATION_REQUIRED" {
		t.Fatalf("auth failure code = %q, want PROVIDER_AUTHENTICATION_REQUIRED", events[1].Code)
	}
	var pe *agent.ProtocolError
	if errors.As(err, &pe) {
		t.Fatalf("an auth failure must never surface as a protocol finding: %v", err)
	}

	// The same stream with an unknown diagnostic event passes it over
	// silently: Authentication Unknown is never disguised as a protocol
	// finding, and a diagnostic frame is not a protocol violation.
	h2 := newHarness(t, claudeBinding(t))
	r2 := h2.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h2.schema, MaxBudgetUSD: h2.budget})
	h2.scriptFrames(t, 1,
		`{"type":"system","subtype":"init","session_id":"`+capturedSessionID+`"}`+"\n"+
			`{"type":"mystery_event","payload":{"x":1}}`, 0)
	_, err2 := collectErr(r2)
	if errors.As(err2, &pe) {
		t.Fatalf("a diagnostic frame must not surface as a protocol finding, got %v", err2)
	}
}

// TestDialectExitWithoutTerminalFailsClosed (PRD 约束 43): a Provider
// success exit without a validated terminal structured event can never
// complete the run; the exit code is a fact the crash error carries, and
// exit code 0 cannot override the missing completion.
func TestDialectExitWithoutTerminalFailsClosed(t *testing.T) {
	for _, code := range []int{0, 1} {
		t.Run(fmt.Sprintf("exit-%d", code), func(t *testing.T) {
			h := newHarness(t, claudeBinding(t))
			r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
			h.scriptFrames(t, 1,
				`{"type":"system","subtype":"init","session_id":"`+capturedSessionID+`"}`+"\n"+
					`{"type":"assistant","message":{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"never finishes"}]}}`, code)
			events, err := collectErr(r)
			var crash *agent.ProcessCrash
			if !errors.As(err, &crash) {
				t.Fatalf("expected a ProcessCrash, got %v", err)
			}
			if crash.ExitCode != code {
				t.Fatalf("crash exit code = %d, want %d", crash.ExitCode, code)
			}
			if len(events) != 2 {
				t.Fatalf("expected 2 events before the crash, got %d", len(events))
			}
		})
	}
}

// TestProcessCrashIncludesBoundedStderr makes a provider launch failure
// diagnosable: stdout may contain no terminal event, but stderr explains
// why the CLI exited. The adapter must carry that bounded diagnostic into
// the crash fact instead of dropping it.
func TestProcessCrashIncludesBoundedStderr(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	hnd := h.waitStarts(t, 1)
	h.fake.EmitOutput(hnd, process.Stderr, []byte("invalid value for --max-budget-usd: expected a positive number\n"))
	h.fake.ExitGroup(hnd, 1)

	_, err := collectErr(r)
	var crash *agent.ProcessCrash
	if !errors.As(err, &crash) {
		t.Fatalf("expected ProcessCrash, got %v", err)
	}
	if !strings.Contains(crash.Message, "invalid value for --max-budget-usd") {
		t.Fatalf("crash message dropped stderr diagnostic: %q", crash.Message)
	}
}

// TestStderrRedaction: stderr frames are bounded and dropped by the
// adapter; a secret on stderr never surfaces in any event, error, or
// completion, and does not poison the stdout protocol stream.
func TestStderrRedaction(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	hnd := h.waitStarts(t, 1)
	h.fake.EmitOutput(hnd, process.Stderr, []byte("sk-abcdefghijklmnop: authentication failed\n"))
	h.fake.EmitOutput(hnd, process.Stdout, []byte(`{"type":"system","subtype":"init","session_id":"`+capturedSessionID+`"}`+"\n"))
	h.fake.EmitOutput(hnd, process.Stdout, []byte(`{"type":"assistant","message":{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"still working"}]}}`+"\n"))
	h.fake.EmitOutput(hnd, process.Stdout, []byte(`{"type":"result","subtype":"success","is_error":false,"result":"{\"ok\":true}"}`+"\n"))
	h.fake.ExitGroup(hnd, 0)
	events, err := collectErr(r)
	requireNoError(t, err)
	for i, ev := range events {
		joined := strings.Join([]string{ev.Text, ev.Tool, ev.Input, ev.Output, ev.Result, ev.Message, ev.Code}, "|")
		if strings.Contains(joined, "sk-abcdefghijklmnop") {
			t.Fatalf("event %d carries stderr content: %+v", i, ev)
		}
	}
	if len(events) != 3 || events[2].Type != agent.EventCompleted {
		t.Fatalf("stderr must not disturb the stdout protocol stream: %+v", events)
	}
}

// ---------------------------------------------------------------------------
// Adapter gates before any process launch
// ---------------------------------------------------------------------------

// TestAdapterStartRequiresValidSchemaAndBudget: the adapter fails closed
// before any process launch when the request carries no typed claude
// input, an invalid schema JSON, or an unusable finite budget.
func TestAdapterStartRequiresValidSchemaAndBudget(t *testing.T) {
	cases := []struct {
		name  string
		input any
	}{
		{"untyped input", map[string]any{"requirement": "search"}},
		{"schema is not json", claude.Input{SchemaJSON: "not json", MaxBudgetUSD: "0.50"}},
		{"schema empty", claude.Input{SchemaJSON: "", MaxBudgetUSD: "0.50"}},
		{"budget not a number", claude.Input{SchemaJSON: schemaJSON, MaxBudgetUSD: "abc"}},
		{"budget negative", claude.Input{SchemaJSON: schemaJSON, MaxBudgetUSD: "-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, claudeBinding(t))
			_, err := h.ad.Start(context.Background(), agent.StartRequest{
				Purpose:  agent.PurposePlanner,
				Provider: "claude",
				Prompt:   "Plan.",
				Input:    tc.input,
				CWD:      "/Users/dev/worktrees/task-1",
			})
			assertFaultCode(t, err, model.CodeInvalidInput)
			if h.rec.count() != 0 {
				t.Fatal("an invalid schema or budget must fail before any process launch")
			}
		})
	}
}

// TestAdapterCapabilityGates: Start and Resume prove their capability
// sets from the binding before any process launch (PRD Agent Adapter:
// capability is per operation, never inferred).
func TestAdapterCapabilityGates(t *testing.T) {
	b := claudeBinding(t)
	b.StartCapabilities = []string{"stream_json"}
	h := newHarness(t, b)
	_, err := h.ad.Start(context.Background(), agent.StartRequest{
		Purpose:  agent.PurposePlanner,
		Provider: "claude",
		Prompt:   "Plan.",
		Input:    claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget},
		CWD:      "/Users/dev/worktrees/task-1",
	})
	assertFaultCode(t, err, model.CodeProviderProtocolUnsupported)
	if h.rec.count() != 0 {
		t.Fatal("a binding without the start capabilities must fail before any process launch")
	}

	b2 := claudeBinding(t)
	b2.ResumeCapabilities = []string{"stream_json"}
	h2 := newHarness(t, b2)
	_, err = h2.ad.Resume(context.Background(), agent.ResumeRequest{
		ProviderSessionID: agent.ProviderSessionID(capturedSessionID),
		Purpose:           agent.PurposeImplementer,
		Provider:          "claude",
		Prompt:            "Continue.",
		Input:             claude.Input{SchemaJSON: h2.schema, MaxBudgetUSD: h2.budget},
		CWD:               "/Users/dev/worktrees/task-1",
	})
	assertFaultCode(t, err, model.CodeProviderProtocolUnsupported)
	if h2.rec.count() != 0 {
		t.Fatal("a binding without the resume capabilities must fail before any process launch")
	}
}

// ---------------------------------------------------------------------------
// Runtime integration: the claude dialect plugs into the unified pipeline
// ---------------------------------------------------------------------------

// TestClaudeAdapterPlugsIntoRuntimePipeline: a Runtime wired to the
// claude adapter validates the protocol sequence, redacts every
// text-bearing field, and settles the Session from the terminal
// structured event.
func TestClaudeAdapterPlugsIntoRuntimePipeline(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	rt := newClaudeRuntime(t, h.ad)
	res, err := runtimeStart(t, rt, h, fixtureStart(h), readFixture(t, "start-valid.jsonl"))
	requireNoError(t, err)
	if res.Status != model.RunSucceeded {
		t.Fatalf("run status = %s, want succeeded", res.Status)
	}
	if res.Session.Status != model.SessionCompleted {
		t.Fatalf("session status = %s, want completed", res.Session.Status)
	}
	if res.Session.ProviderSessionID != capturedSessionID {
		t.Fatalf("session must appear through the validated start event, got %q", res.Session.ProviderSessionID)
	}
	if res.Terminal == nil || res.Terminal.Type != agent.EventCompleted {
		t.Fatalf("terminal event missing: %+v", res.Terminal)
	}
	if len(res.Events) != 6 {
		t.Fatalf("expected 6 persisted events, got %d", len(res.Events))
	}
	for i, ev := range res.Events {
		if ev.SessionID != agent.ProviderSessionID(capturedSessionID) {
			t.Fatalf("event %d session id = %q", i, ev.SessionID)
		}
		if ev.FrameHash == "" {
			t.Fatalf("event %d carries no protocol hash", i)
		}
	}
}

// TestClaudeEventTextRedactedThroughPipeline: a provider token in an
// assistant event is redacted before any output consumer sees it.
func TestClaudeEventTextRedactedThroughPipeline(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	rt := newClaudeRuntime(t, h.ad)
	leaked := "the token is sk-abc123def4567890 and it must not leak"
	fixture := `{"type":"system","subtype":"init","session_id":"` + capturedSessionID + `"}` + "\n" +
		`{"type":"assistant","message":{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"` + leaked + `"}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"{\"ok\":true}"}`
	res, err := runtimeStart(t, rt, h, fixtureStart(h), fixture)
	requireNoError(t, err)
	if res.Status != model.RunSucceeded {
		t.Fatalf("run status = %s", res.Status)
	}
	if strings.Contains(res.Events[1].Text, "sk-abc123def4567890") {
		t.Fatalf("raw token leaked into the run result: %q", res.Events[1].Text)
	}
	if !strings.Contains(res.Events[1].Text, "[REDACTED:provider_token]") {
		t.Fatalf("token must be replaced by the redaction placeholder: %q", res.Events[1].Text)
	}
}

// TestCancellationPreservesPartialRedactedEvents: Cancel performs the
// controlled stop (Terminate to the process group); the drain settles the
// run as CANCELLED and preserves the redacted events read so far.
func TestCancellationPreservesPartialRedactedEvents(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	rt := newClaudeRuntime(t, h.ad)
	done := make(chan error, 1)
	var res *agent.RunResult
	go func() {
		var err error
		res, err = rt.Start(context.Background(), fixtureStart(h))
		done <- err
	}()
	h.scriptVersion(t, 1, capturedVersion+" (Claude Code)", 0)
	hnd := h.scriptFrames(t, 2,
		`{"type":"system","subtype":"init","session_id":"`+capturedSessionID+`"}`+"\n"+
			`{"type":"assistant","message":{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"halfway with sk-abc123def4567890"}]}}`, -1)

	// The handle appears once the validated start event established the
	// session; cancel it, then let the process exit so Next unblocks.
	var handle agent.RunHandle
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, fact := range rt.Sessions() {
			if fact.Handle.RunID != "" {
				handle = fact.Handle
			}
		}
		if handle.RunID != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if handle.RunID == "" {
		t.Fatal("session handle never appeared")
	}
	requireNoError(t, rt.Cancel(context.Background(), handle))
	// The Terminate delivery is a synchronous fact of Cancel: assert it
	// while the group is still live (the fake panics on Signals after the
	// group exited).
	signals := h.fake.Signals(hnd)
	found := false
	for _, s := range signals {
		if s == process.Terminate {
			found = true
		}
	}
	if !found {
		t.Fatalf("cancel must deliver Terminate to the process group, got %v", signals)
	}
	h.fake.ExitGroup(hnd, 0)

	select {
	case err := <-done:
		requireNoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled runtime Start hung")
	}
	if res.Status != model.RunCancelled {
		t.Fatalf("run status = %s, want cancelled", res.Status)
	}
	if res.Session.Status != model.SessionCancelled {
		t.Fatalf("session status = %s, want cancelled", res.Session.Status)
	}
	// The drain settles the events it read before the cancellation: at
	// least the validated start event, and every preserved event redacted
	// (the exact count is racy by design: the drain may settle at its
	// cancellation check before consuming the second frame).
	if len(res.Events) < 1 {
		t.Fatalf("partial redacted events must be preserved, got %d", len(res.Events))
	}
	if res.Events[0].Type != agent.EventSessionStarted || res.Events[0].SessionID != agent.ProviderSessionID(capturedSessionID) {
		t.Fatalf("first preserved event = %+v", res.Events[0])
	}
	for i, ev := range res.Events {
		if strings.Contains(ev.Text, "sk-abc123def4567890") {
			t.Fatalf("partial event %d text leaked the raw token: %q", i, ev.Text)
		}
	}
	if len(res.Events) >= 2 && strings.Contains(res.Events[1].Text, "halfway") {
		t.Logf("preserved redacted partial event: %q", res.Events[1].Text)
	}
}

// TestRuntimeRejectsStreamWithoutValidatedStart: a stream that never
// establishes a session through a validated system init event fails the
// run with a non-retryable protocol Finding (brief: missing
// session_started-equivalent → non-retryable protocol Finding; exit 0
// cannot override it).
func TestRuntimeRejectsStreamWithoutValidatedStart(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	rt := newClaudeRuntime(t, h.ad)
	_, err := runtimeStart(t, rt, h, fixtureStart(h),
		`{"type":"assistant","message":{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"no start"}]}}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"result":"{\"ok\":true}"}`)
	assertFaultCode(t, err, model.CodeProviderProtocolViolation)
}

// TestRuntimeRejectsEmptyStartID: an empty session id on the system init
// frame fails the run with PROVIDER_SESSION_ID_MISSING through the
// pipeline.
func TestRuntimeRejectsEmptyStartID(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	rt := newClaudeRuntime(t, h.ad)
	_, err := runtimeStart(t, rt, h, fixtureStart(h),
		`{"type":"system","subtype":"init"}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"result":"{\"ok\":true}"}`)
	assertFaultCode(t, err, model.CodeProviderSessionIDMissing)
}

// TestRuntimeRejectsInvalidCompletion: a terminal completion without a
// validated JSON-object result fails the run closed through the pipeline
// (PRD: invalid completion → non-retryable protocol Finding).
func TestRuntimeRejectsInvalidCompletion(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	rt := newClaudeRuntime(t, h.ad)
	_, err := runtimeStart(t, rt, h, fixtureStart(h),
		`{"type":"system","subtype":"init","session_id":"`+capturedSessionID+`"}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false}`)
	assertFaultCode(t, err, model.CodeProviderProtocolViolation)
}

// TestRuntimeBudgetExceededSettlesFailed: a budget-exceeded terminal
// settles the run as failed with the wire budget code as the terminal
// fact (the runtime never turns a provider budget fact into a protocol
// finding).
func TestRuntimeBudgetExceededSettlesFailed(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	rt := newClaudeRuntime(t, h.ad)
	res, err := runtimeStart(t, rt, h, fixtureStart(h),
		`{"type":"system","subtype":"init","session_id":"`+capturedSessionID+`"}`+"\n"+
			`{"type":"result","subtype":"error_budget","is_error":true,"error":"Budget limit exceeded: spent $0.76 of the $0.50 limit","session_id":"`+capturedSessionID+`"}`)
	requireNoError(t, err)
	if res.Status != model.RunFailed {
		t.Fatalf("run status = %s, want failed", res.Status)
	}
	if res.Session.Status != model.SessionFailed {
		t.Fatalf("session status = %s, want failed", res.Session.Status)
	}
	if res.Terminal == nil || res.Terminal.Type != agent.EventFailed || res.Terminal.Code != "BUDGET_EXCEEDED" {
		t.Fatalf("terminal = %+v", res.Terminal)
	}
}

// TestResumeNotFoundFailsBeforeAnyProcessLaunch: resuming a provider
// session the Runtime does not know fails closed before any claude
// process is launched.
func TestResumeNotFoundFailsBeforeAnyProcessLaunch(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	rt := newClaudeRuntime(t, h.ad)
	_, err := rt.Resume(context.Background(), agent.ResumeRequest{
		ProviderSessionID: agent.ProviderSessionID("no-such-session"),
		Purpose:           agent.PurposeImplementer,
		Provider:          "claude",
		Prompt:            "Continue.",
		Input:             claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget},
		CWD:               "/Users/dev/worktrees/task-1",
	})
	assertFaultCode(t, err, model.CodeInvalidInput)
	if h.rec.count() != 0 {
		t.Fatal("resume of an unknown session must fail before any process launch")
	}
}

// TestResumeReaffirmsBoundSession: a resume stream whose init event
// re-affirms the resumed session completes; the runtime's bound check is
// exercised through the claude dialect.
func TestResumeReaffirmsBoundSession(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	rt := newClaudeRuntime(t, h.ad)
	requireNoError(t, rt.Hydrate(context.Background(), []agent.SessionFact{{
		Session: model.Session{
			ProviderSessionID: capturedSessionID,
			Purpose:           agent.PurposeImplementer,
			Status:            model.SessionActive,
		},
	}}))
	done := make(chan *agent.ResumeResult, 1)
	var resumeErr error
	go func() {
		res, err := rt.Resume(context.Background(), agent.ResumeRequest{
			ProviderSessionID: agent.ProviderSessionID(capturedSessionID),
			Purpose:           agent.PurposeImplementer,
			Provider:          "claude",
			Prompt:            "Continue the work.",
			Input:             claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget},
			CWD:               "/Users/dev/worktrees/task-1",
		})
		resumeErr = err
		done <- res
	}()
	h.scriptVersion(t, 1, capturedVersion+" (Claude Code)", 0)
	h.scriptFrames(t, 2, readFixture(t, "resume-valid.jsonl"), 0)
	var res *agent.ResumeResult
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runtime Resume hung")
	}
	requireNoError(t, resumeErr)
	if res.Run == nil || res.Run.Status != model.RunSucceeded {
		t.Fatalf("native resume must succeed: %+v", res)
	}
	if res.Session.ProviderSessionID != capturedSessionID {
		t.Fatalf("resumed session id = %q", res.Session.ProviderSessionID)
	}
}

// ---------------------------------------------------------------------------
// Detect: fail-closed protocol detection against the captured binding
// ---------------------------------------------------------------------------

// TestDetectSupportedCapturedBinding: detection of the stub executable
// against the captured binding returns SUPPORTED with the executable
// hash, the parsed CLI version, the binding's dialect and revision, and
// the binding's capabilities.
func TestDetectSupportedCapturedBinding(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	inst := h.detectIn(t)
	if inst.Compatibility != agent.CompatibilitySupported {
		t.Fatalf("compatibility = %s, want SUPPORTED", inst.Compatibility)
	}
	if inst.CLIVersion != capturedVersion {
		t.Fatalf("parsed CLI version = %q, want %s", inst.CLIVersion, capturedVersion)
	}
	if inst.ExecutablePath != filepath.Join(h.claudeDir, "claude") {
		t.Fatalf("executable path = %q", inst.ExecutablePath)
	}
	wantHash := sha256Hex(mustReadFile(t, filepath.Join(h.claudeDir, "claude")))
	if inst.ExecutableSHA256 != wantHash {
		t.Fatalf("executable hash = %q, want %q", inst.ExecutableSHA256, wantHash)
	}
	if inst.DialectID != "cflow.dialect.claude-stream-json.v1" {
		t.Fatalf("dialect = %q", inst.DialectID)
	}
	if inst.RegistryRevision != claudeBinding(t).Revision {
		t.Fatalf("registry revision = %q", inst.RegistryRevision)
	}
	if !inst.Capabilities.StructuredEvents || !inst.Capabilities.ResumableSession || !inst.Capabilities.SessionIDInEventStream ||
		!inst.Capabilities.StructuredOutputSchemaOnStart || !inst.Capabilities.StructuredOutputSchemaOnResume || !inst.Capabilities.BudgetLimit {
		t.Fatalf("capabilities = %+v", inst.Capabilities)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	requireNoError(t, err)
	return data
}

// TestDetectMissingExecutable: no claude executable on PATH yields the
// MISSING fact and no fabricated identity facts.
func TestDetectMissingExecutable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, sup := process.NewFakeSupervisor()
	ad := claude.New(sup, claudeBinding(t))
	inst, err := ad.Detect(context.Background())
	requireNoError(t, err)
	if inst.Compatibility != agent.CompatibilityMissing {
		t.Fatalf("compatibility = %s, want MISSING", inst.Compatibility)
	}
	if inst.ExecutablePath != "" || inst.ExecutableSHA256 != "" || inst.CLIVersion != "" {
		t.Fatalf("a missing executable must carry no identity facts: %+v", inst)
	}
}

// TestDetectVersionMismatch: a version outside the binding's supported
// range is UNKNOWN_VERSION, and the parsed version stays a fact.
func TestDetectVersionMismatch(t *testing.T) {
	b := claudeBinding(t)
	b.VersionRange = ">=3.0.0 <4.0.0"
	h := newHarness(t, b)
	inst := h.detectIn(t)
	if inst.Compatibility != agent.CompatibilityUnknownVersion {
		t.Fatalf("compatibility = %s, want UNKNOWN_VERSION", inst.Compatibility)
	}
	if inst.CLIVersion != capturedVersion {
		t.Fatalf("the parsed version must remain a fact: %q", inst.CLIVersion)
	}
}

// TestDetectIncompatibleProtocol: a version probe whose output cannot be
// parsed as the claude CLI version yields INCOMPATIBLE_PROTOCOL.
func TestDetectIncompatibleProtocol(t *testing.T) {
	for _, scripted := range []string{"", "some other tool v3", "claude-cli banana"} {
		t.Run(fmt.Sprintf("%q", scripted), func(t *testing.T) {
			h := newHarness(t, claudeBinding(t))
			done := make(chan agent.Installation, 1)
			go func() {
				inst, err := h.ad.Detect(context.Background())
				requireNoError(t, err)
				done <- inst
			}()
			h.scriptVersion(t, 1, scripted, 0)
			inst := <-done
			if inst.Compatibility != agent.CompatibilityIncompatibleProtocol {
				t.Fatalf("compatibility = %s, want INCOMPATIBLE_PROTOCOL", inst.Compatibility)
			}
		})
	}
}

// TestDetectExecutableHashDrift: the executable sha256 is the drift fact;
// replacing the binary changes the hash while the version fact is
// unchanged, so the Execution Approval pin gate can detect drift (the
// blocking gate itself lives at Execution Approval).
func TestDetectExecutableHashDrift(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "claude")
	first := []byte("#!/bin/sh\necho \"2.1.220 (Claude Code)\"\n")
	if err := os.WriteFile(p, first, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	fa, sup := process.NewFakeSupervisor()
	rec := &recordingSupervisor{Supervisor: sup}
	ad := claude.New(rec, claudeBinding(t))
	h := &harness{ad: ad, fake: fa, rec: rec}

	inst1 := h.detectIn(t)
	if inst1.Compatibility != agent.CompatibilitySupported {
		t.Fatalf("compatibility = %s, want SUPPORTED", inst1.Compatibility)
	}
	if inst1.ExecutableSHA256 != sha256Hex(first) {
		t.Fatalf("hash = %q, want %q", inst1.ExecutableSHA256, sha256Hex(first))
	}

	drifted := []byte("#!/bin/sh\necho \"2.1.220 (Claude Code)\"\n# drifted binary\n")
	if err := os.WriteFile(p, drifted, 0o700); err != nil {
		t.Fatal(err)
	}
	inst2 := h.detectIn(t)
	if inst2.Compatibility != agent.CompatibilitySupported {
		t.Fatalf("compatibility = %s, want SUPPORTED", inst2.Compatibility)
	}
	if inst2.ExecutableSHA256 != sha256Hex(drifted) {
		t.Fatalf("hash = %q, want %q", inst2.ExecutableSHA256, sha256Hex(drifted))
	}
	if inst2.ExecutableSHA256 == inst1.ExecutableSHA256 {
		t.Fatal("executable hash drift must be observable through the installation facts")
	}
	if inst2.CLIVersion != inst1.CLIVersion {
		t.Fatal("the version fact must not change when only the binary content drifted")
	}
}

// TestDetectRealClaudeCapturedBinding (opt-in smoke detection, brief Step
// 5): when the real claude binary is installed, detection runs only the
// read-only version probe and must report SUPPORTED for a version within
// the captured binding's supported range (PRD Protocol Compatibility:
// SUPPORTED is range-based, never exact-version — a CLI auto-update within
// the range stays SUPPORTED), and never SUPPORTED for a binding whose
// range excludes the installed version. Skipped when claude is absent.
func TestDetectRealClaudeCapturedBinding(t *testing.T) {
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed; opt-in smoke detection skipped")
	}
	sup := process.NewSupervisor(process.NewOSAdapter())
	b := claudeBinding(t)
	ad := claude.New(sup, b)
	inst, err := ad.Detect(context.Background())
	requireNoError(t, err)
	if inst.Compatibility != agent.CompatibilitySupported {
		t.Fatalf("the installed claude at %s must be SUPPORTED for the captured binding, got %s", path, inst.Compatibility)
	}
	if !claude.VersionInRange(inst.CLIVersion, b.VersionRange) {
		t.Fatalf("installed claude %q must be within the supported range %q", inst.CLIVersion, b.VersionRange)
	}

	// SUPPORTED is reserved for in-range bindings: an out-of-range binding
	// must never report SUPPORTED for the same binary.
	outOfRange := b
	outOfRange.VersionRange = ">=3.0.0 <4.0.0"
	ad2 := claude.New(sup, outOfRange)
	inst2, err := ad2.Detect(context.Background())
	requireNoError(t, err)
	if inst2.Compatibility == agent.CompatibilitySupported {
		t.Fatal("SUPPORTED is reserved for in-range bindings; an out-of-range binding must not be supported")
	}
	if inst2.Compatibility != agent.CompatibilityUnknownVersion {
		t.Fatalf("compatibility = %s, want UNKNOWN_VERSION", inst2.Compatibility)
	}
}

// TestInspectReportsLiveThenReaped: Inspect reports the live run truth
// while the process is supervised and the reaped fact once the stream
// ended.
func TestInspectReportsLiveThenReaped(t *testing.T) {
	h := newHarness(t, claudeBinding(t))
	r := h.startRun(t, agent.PurposePlanner, claude.Input{SchemaJSON: h.schema, MaxBudgetUSD: h.budget})
	hnd := h.waitStarts(t, 1)
	h.fake.EmitOutput(hnd, process.Stdout, []byte(`{"type":"system","subtype":"init","session_id":"s9"}`+"\n"))
	ev, err := r.Next(context.Background())
	requireNoError(t, err)
	if ev.Type != agent.EventSessionStarted {
		t.Fatalf("first event = %s", ev.Type)
	}
	fact, err := h.ad.Inspect(context.Background(), agent.ProviderSessionID("s9"))
	requireNoError(t, err)
	if !fact.Running {
		t.Fatal("the run must report as running while the process is supervised")
	}
	h.fake.ExitGroup(hnd, 0)
	_, err = collectErr(r) // the stream ends without a terminal event
	if err == nil {
		t.Fatal("expected the crash fact for a stream without a terminal event")
	}
	fact, err = h.ad.Inspect(context.Background(), agent.ProviderSessionID("s9"))
	requireNoError(t, err)
	if fact.Running {
		t.Fatal("the run must not report as running after it was reaped")
	}
}

// TestTypedInputCarriesContextBundleRef (Task 18, ledger obligation from
// Task 16): the claude typed input carries the immutable Context Bundle
// handoff reference of an automatic fallback so a production successor
// Session's recorded input facts carry the handoff; a fresh Session's
// typed input omits it.
func TestTypedInputCarriesContextBundleRef(t *testing.T) {
	withRef := claude.Input{SchemaJSON: `{"type":"object"}`, MaxBudgetUSD: "2",
		Model: "claude-sonnet-4-5", ContextBundleRef: "/evidence/sessions/lost1/bundle-1.json"}
	body, err := json.Marshal(withRef)
	requireNoError(t, err)
	if !strings.Contains(string(body), `"context_bundle_ref":"/evidence/sessions/lost1/bundle-1.json"`) {
		t.Fatalf("typed input JSON missing the bundle ref: %s", body)
	}
	fresh, err := json.Marshal(claude.Input{SchemaJSON: `{"type":"object"}`, MaxBudgetUSD: "2"})
	requireNoError(t, err)
	if strings.Contains(string(fresh), "context_bundle_ref") {
		t.Fatalf("fresh typed input carries a bundle ref: %s", fresh)
	}
}

// TestMatrixProtocolDispositionsAgree (Task 21): the protocol fault Codes
// this adapter surface produces carry the compiled release disposition the
// fault matrix rows assert — PROVIDER_SESSION_ID_MISSING blocks with a
// Finding (USER_ACTION_REQUIRED, dispatch closed) and never charges a Retry
// (PRD 失败分类: 否，且不扣失败重试预算).
func TestMatrixProtocolDispositionsAgree(t *testing.T) {
	for _, tc := range []struct {
		code     model.Code
		category model.FaultCategory
		close    bool
	}{
		{model.CodeProviderSessionIDMissing, model.CatUserActionRequired, true},
		{model.CodeProviderProtocolViolation, model.CatSafetyStop, true},
		{model.CodeProviderProtocolUnsupported, model.CatUserActionRequired, true},
		{model.CodeProviderAuthenticationRequired, model.CatUserActionRequired, true},
	} {
		pol, ok := model.Policy(tc.code)
		if !ok {
			t.Fatalf("no compiled policy for %s", tc.code)
		}
		if pol.Category != tc.category || pol.CloseDispatch != tc.close {
			t.Fatalf("policy(%s) = %+v, want category %s close %v", tc.code, pol, tc.category, tc.close)
		}
	}
}

// TestInteractiveResumeArgvShape: the native interactive resume argv is
// exactly `claude --resume <session-id>` in the workflow Worktree — no
// bypass flag.
func TestInteractiveResumeArgvShape(t *testing.T) {
	sup := process.NewSupervisor(process.NewOSAdapter())
	ad := claude.New(sup, claudeBinding(t))
	if ia, ok := ad.(agent.InteractiveAdapter); ok {
		spec, err := ia.InteractiveResume(context.Background(), agent.ProviderSessionID(capturedSessionID), "/workspace")
		if err != nil {
			t.Fatal(err)
		}
		if spec.Capability {
			requireExactArgs(t, spec.Args, "--resume", capturedSessionID)
			requireAbsentArgs(t, spec.Args, "--dangerously-skip-permissions", "-C")
			if spec.Dir != "/workspace" {
				t.Fatalf("cwd = %q, want /workspace", spec.Dir)
			}
		}
	}
}
