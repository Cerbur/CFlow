package agent_test

// Agent Routing tests (Task 16, brief Step 1): the immutable per-Purpose
// RoutingPolicy resolution and the Compare-and-Swap drift gate. Covers
// route absent, fallback unapproved, Resume supported for Start but not
// Resume, same Provider/different Session accepted, same Session/different
// Purpose rejected, the ordered fallback binding selection through a real
// second Adapter, Context Bundle redaction, and the routing policy body
// round-trip.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/codex"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// ---------------------------------------------------------------------------
// policy fixture helpers
// ---------------------------------------------------------------------------

// bindingFor builds the immutable RouteBinding of one enabled registry
// Provider for a routing policy (the same shape the Application resolves
// at Execution Dry Run; the test pins the executable identity facts it
// observed itself).
func bindingFor(t *testing.T, reg *agent.ProviderRegistry, name string, path, sha, version string) agent.RouteBinding {
	t.Helper()
	b, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("provider %q is not bound in the registry", name)
	}
	return agent.RouteBinding{
		Provider:           name,
		Model:              "default",
		BudgetUSD:          10,
		TimeoutSeconds:     1800,
		PromptHash:         strings.Repeat("p0", 32),
		Disclosure:         "Provider default permissions; no sandbox guarantee.",
		DialectID:          b.Dialect.ID,
		RegistryRevision:   b.Revision,
		StartCapabilities:  append([]string(nil), b.StartCapabilities...),
		ResumeCapabilities: append([]string(nil), b.ResumeCapabilities...),
		ExecutablePath:     path,
		ExecutableSHA256:   sha,
		CLIVersion:         version,
	}
}

// policySet builds the RoutingPolicySet one test Runtime is approved
// with, keyed by Purpose.
func policySet(policies ...agent.RoutingPolicy) *agent.RoutingPolicySet {
	return &agent.RoutingPolicySet{Policies: policies}
}

func policy(purpose model.AgentPurpose, bindings ...agent.RouteBinding) agent.RoutingPolicy {
	return agent.RoutingPolicy{Purpose: purpose, Bindings: bindings}
}

// newPolicyRuntime builds a Runtime approved with the given policy over
// the Fake Adapter. script serves the run; seed hydrates the resume
// target.
func newPolicyRuntime(t *testing.T, script string, set *agent.RoutingPolicySet) *agent.Runtime {
	t.Helper()
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	ad := fake.New(reg)
	requireNoError(t, ad.LoadScript([]byte(script)))
	rt, err := agent.NewRuntime(agent.RuntimeOptions{
		Now:         fixedClock,
		IDs:         model.SequentialIDSource(),
		Registry:    reg,
		Redaction:   testRedactionRegistry(),
		EvidenceDir: tempRoot(t),
		Adapters:    map[string]agent.Adapter{"fake": ad},
		Routing:     set,
	})
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, rt.Close()) })
	sc, err := fake.ParseScript([]byte(script))
	requireNoError(t, err)
	if sc.Seed && sc.SessionID != "" && sc.Purpose.Valid() {
		requireNoError(t, rt.Hydrate(context.Background(), []agent.SessionFact{{
			Session: model.Session{
				ProviderSessionID: sc.SessionID,
				Purpose:           sc.Purpose,
				Status:            model.SessionActive,
			},
		}}))
	}
	return rt
}

// notFoundRun is a seeded Implementer session whose native Resume fails
// unrecoverably (the fake's not-found outcome), the fallback trigger.
func notFoundRun(id string) string {
	return fmtHeader("implementation", id, `"resume":"not-found","seed":true`)
}

func fmtHeader(purpose, sessionID, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return "{\"fixture\":\"fake-run\",\"script_version\":1,\"provider\":\"fake\",\"dialect\":\"cflow.dialect.fake.v1\",\"purpose\":\"" + purpose + "\",\"session_id\":\"" + sessionID + "\",\"exit_code\":0" + extra + "}\n" +
		"{\"type\":\"session_started\",\"session_id\":\"" + sessionID + "\",\"at_ms\":0}\n" +
		"{\"type\":\"assistant_message\",\"session_id\":\"" + sessionID + "\",\"text\":\"Working.\",\"at_ms\":10}\n" +
		"{\"type\":\"session_finished\",\"session_id\":\"" + sessionID + "\",\"result\":{\"ok\":true},\"at_ms\":20}\n"
}

// hashBytes digests bytes (the executable identity fact).
func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// routing resolution and CAS (brief Step 1 case list)
// ---------------------------------------------------------------------------

// TestRouteAbsentFailsClosed: a Purpose with no approved route in the
// immutable RoutingPolicy can never start; the Runtime fails closed with
// PROVIDER_PROTOCOL_UNSUPPORTED before any adapter call and no Session is
// established.
func TestRouteAbsentFailsClosed(t *testing.T) {
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	set := policySet(policy(model.PurposePlanning, bindingFor(t, reg, "fake", "", "", "")))
	rt := newPolicyRuntime(t, fmtHeader("implementation", "r1", ""), set)
	_, err = rt.Start(context.Background(), fixtureStart(agent.PurposeImplementer))
	assertFaultCode(t, err, model.CodeProviderProtocolUnsupported)
	if n := len(rt.Sessions()); n != 0 {
		t.Fatalf("route absent must not establish a session, got %d", n)
	}
}

// TestFallbackUnapprovedFailsClosed: an unrecoverable native Resume whose
// Purpose has no approved fallback binding fails closed with
// PROVIDER_PROTOCOL_UNSUPPORTED. The original Session is preserved (never
// marked LOST) and no successor lineage or Context Bundle is created: an
// unapproved Fallback never substitutes silently (PRD 约束 306).
func TestFallbackUnapprovedFailsClosed(t *testing.T) {
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	set := policySet(policy(model.PurposeImplementation, bindingFor(t, reg, "fake", "", "", "")))
	rt := newPolicyRuntime(t, notFoundRun("f1"), set)
	_, err = rt.Resume(context.Background(), fixtureResume(agent.PurposeImplementer, "f1"))
	assertFaultCode(t, err, model.CodeProviderProtocolUnsupported)
	if n := countSuccessors(t, rt); n != 0 {
		t.Fatalf("no successor may be created without an approved fallback, got %d", n)
	}
	fact, err := rt.Inspect(context.Background(), agent.ProviderSessionID("f1"))
	requireNoError(t, err)
	if fact.Session.Status != model.SessionActive {
		t.Fatalf("the original session must be preserved on an unapproved fallback, got %s", fact.Session.Status)
	}
}

// TestResumeRequiresResumeCapabilityOfExactBinding: a binding approved
// for Start but not for Resume can never be natively resumed; the
// per-operation capability gate is never inferred across operations
// (PRD 已确认). The Resume fails closed and the original Session is
// preserved.
func TestResumeRequiresResumeCapabilityOfExactBinding(t *testing.T) {
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	startOnly := bindingFor(t, reg, "fake", "", "", "")
	startOnly.ResumeCapabilities = nil // Start only: no Resume capabilities
	set := policySet(policy(model.PurposeImplementation, startOnly))
	rt := newPolicyRuntime(t, notFoundRun("s2"), set)
	_, err = rt.Resume(context.Background(), fixtureResume(agent.PurposeImplementer, "s2"))
	assertFaultCode(t, err, model.CodeProviderProtocolUnsupported)
	fact, err := rt.Inspect(context.Background(), agent.ProviderSessionID("s2"))
	requireNoError(t, err)
	if fact.Session.Status != model.SessionActive {
		t.Fatalf("a binding without Resume capabilities must not mutate the session, got %s", fact.Session.Status)
	}
}

// newPolicyRuntimeScripts builds a Runtime approved with the given policy
// over a Fake Adapter loaded with one script per Purpose.
func newPolicyRuntimeScripts(t *testing.T, scripts []string, set *agent.RoutingPolicySet) *agent.Runtime {
	t.Helper()
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	ad := fake.New(reg)
	for _, s := range scripts {
		requireNoError(t, ad.LoadScript([]byte(s)))
	}
	rt, err := agent.NewRuntime(agent.RuntimeOptions{
		Now:         fixedClock,
		IDs:         model.SequentialIDSource(),
		Registry:    reg,
		Redaction:   testRedactionRegistry(),
		EvidenceDir: tempRoot(t),
		Adapters:    map[string]agent.Adapter{"fake": ad},
		Routing:     set,
	})
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, rt.Close()) })
	return rt
}

// TestSameProviderDifferentSessionsAccepted: Planner and Implementer on
// the same Provider are independent Sessions: both Start, each claims its
// own provider session id, and the Runtime keeps two distinct CFlow
// Session identities (design 14.4).
func TestSameProviderDifferentSessionsAccepted(t *testing.T) {
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	set := policySet(
		policy(model.PurposePlanning, bindingFor(t, reg, "fake", "", "", "")),
		policy(model.PurposeImplementation, bindingFor(t, reg, "fake", "", "", "")),
	)
	rt := newPolicyRuntimeScripts(t, []string{
		fmtHeader("planning", "p1", ""),
		fmtHeader("implementation", "i1", ""),
	}, set)
	res1, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	requireNoError(t, err)
	res2, err := rt.Start(context.Background(), fixtureStart(agent.PurposeImplementer))
	requireNoError(t, err)
	if res1.Session.ID == res2.Session.ID {
		t.Fatalf("same provider must never reuse a CFlow session identity")
	}
	if res1.Session.ProviderSessionID == res2.Session.ProviderSessionID {
		t.Fatalf("same provider must never reuse a provider session identity")
	}
	if len(rt.Sessions()) != 2 {
		t.Fatalf("expected two independent sessions, got %d", len(rt.Sessions()))
	}
}

// TestSameSessionDifferentPurposeRejected: a Start that claims a provider
// session id already bound to another Purpose crosses the role lineage
// even under an approved routing policy (design 14.4).
func TestSameSessionDifferentPurposeRejected(t *testing.T) {
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	set := policySet(
		policy(model.PurposePlanning, bindingFor(t, reg, "fake", "", "", "")),
		policy(model.PurposeImplementation, bindingFor(t, reg, "fake", "", "", "")),
	)
	rt := newPolicyRuntimeScripts(t, []string{
		fmtHeader("planning", "s3", ""),
		fmtHeader("implementation", "s3", ""),
	}, set)
	if _, err := rt.Start(context.Background(), fixtureStart(agent.PurposeImplementer)); err != nil {
		t.Fatalf("first implementer start must succeed: %v", err)
	}
	_, err = rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	assertFaultCode(t, err, model.CodeSessionIndependenceViolation)
}

// ---------------------------------------------------------------------------
// ordered fallback through a real second Adapter
// ---------------------------------------------------------------------------

// stubCodexOnPath places a stub codex executable on PATH and returns its
// path (the codex adapter resolves and hashes it during Detect).
func stubCodexOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "codex")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho codex-cli 0.141.0\n"), 0o700); err != nil {
		t.Fatalf("write stub codex: %v", err)
	}
	t.Setenv("PATH", dir)
	return p
}

// TestFallbackSelectsNextApprovedBinding: an unrecoverable native Resume
// falls back to the next approved binding of the Purpose. The successor
// Session carries supersedes_session_id, its Provider is the approved
// fallback, and the fallback binding passed the Compare-and-Swap through
// the real codex Adapter's detection (the read-only version probe).
func TestFallbackSelectsNextApprovedBinding(t *testing.T) {
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	codexPath := stubCodexOnPath(t)
	codexData, err := os.ReadFile(codexPath)
	requireNoError(t, err)
	fa, sup := process.NewFakeSupervisor()
	rec := &startRecorder{Supervisor: sup}
	codexBinding, _ := reg.Select("codex")
	ad := codex.New(rec, codexBinding)
	set := policySet(policy(model.PurposeImplementation,
		bindingFor(t, reg, "fake", "", "", ""),
		bindingFor(t, reg, "codex", codexPath, hashBytes(codexData), "0.141.0"),
	))
	fakeAd := fake.New(reg)
	requireNoError(t, fakeAd.LoadScript([]byte(notFoundRun("f4"))))
	rt, err := agent.NewRuntime(agent.RuntimeOptions{
		Now:         fixedClock,
		IDs:         model.SequentialIDSource(),
		Registry:    reg,
		Redaction:   testRedactionRegistry(),
		EvidenceDir: tempRoot(t),
		Adapters:    map[string]agent.Adapter{"fake": fakeAd, "codex": ad},
		Routing:     set,
	})
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, rt.Close()) })
	requireNoError(t, rt.Hydrate(context.Background(), []agent.SessionFact{{
		Session: model.Session{
			ProviderSessionID: "f4",
			Purpose:           model.PurposeImplementation,
			Status:            model.SessionActive,
		},
	}}))

	type resumeOutcome struct {
		res *agent.ResumeResult
		err error
	}
	out := make(chan resumeOutcome, 1)
	go func() {
		res, err := rt.Resume(context.Background(), fixtureResume(agent.PurposeImplementer, "f4"))
		out <- resumeOutcome{res, err}
	}()
	// The fallback's Detect runs the read-only version probe through the
	// codex adapter's supervisor: script the stub's version line once the
	// probe appears.
	hnd := rec.waitStarts(t, 1)
	fa.EmitOutput(hnd, process.Stdout, []byte("codex-cli 0.141.0\n"))
	fa.ExitGroup(hnd, 0)
	select {
	case oc := <-out:
		requireNoError(t, oc.err)
		if oc.res.Fallback == nil {
			t.Fatal("expected a resume fallback to the next approved binding")
		}
		fb := oc.res.Fallback
		if fb.SuccessorSession.Provider != "codex" {
			t.Fatalf("successor provider = %q, want the approved fallback codex", fb.SuccessorSession.Provider)
		}
		if fb.SuccessorSession.Supersedes != fb.LostSession.ID {
			t.Fatalf("successor must supersede the lost original via supersedes_session_id")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("resume fallback hung")
	}
}

// waitSupervisorStart blocks until the recording supervisor has recorded
// n process starts and returns the n-th handle.
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

// ---------------------------------------------------------------------------
// Context Bundle redaction (PRD 已确认：Session Resume 失败与跨 Provider 上
// 下文交接: no unredacted raw stream or secret survives)
// ---------------------------------------------------------------------------

// TestContextBundleRedactsProviderTokens: the immutable Context Bundle of
// an unrecoverable Resume redacts provider tokens before it is returned
// or persisted; the raw secret never reaches the bundle or the evidence
// file. The redaction contract is independent of routing (the fallback
// keeps the original Provider when no policy binds one, exactly as the
// Task 9 fallback tests drive it).
func TestContextBundleRedactsProviderTokens(t *testing.T) {
	rt := newFakeRuntime(t, readFixture(t, "resume-missing.jsonl"))
	req := fixtureResume(agent.PurposeImplementer, "c2")
	req.Context.Requirement = "Add search with token sk-abc123def4567890 embedded."
	req.Context.StageSummary = "Implementation started; API key AKIA1234567890ABCDEF used."
	res, err := rt.Resume(context.Background(), req)
	requireNoError(t, err)
	if res.Fallback == nil {
		t.Fatalf("expected a resume fallback")
	}
	b := res.Fallback.ContextBundle
	if strings.Contains(b.Context.Requirement, "sk-abc123def4567890") {
		t.Fatalf("the returned bundle must be redacted, got %q", b.Context.Requirement)
	}
	if !strings.Contains(b.Context.Requirement, "[REDACTED:provider_token]") {
		t.Fatalf("the returned bundle must carry the redaction placeholder, got %q", b.Context.Requirement)
	}
	if strings.Contains(b.Context.StageSummary, "AKIA1234567890ABCDEF") {
		t.Fatalf("the returned bundle must redact api keys, got %q", b.Context.StageSummary)
	}
	rev1, err := os.ReadFile(filepath.Join(evidenceRoot(t, rt), "bundles", string(res.Fallback.LostSession.ID), "rev-1.json"))
	requireNoError(t, err)
	if strings.Contains(string(rev1), "sk-abc123def4567890") || strings.Contains(string(rev1), "AKIA1234567890ABCDEF") {
		t.Fatalf("the persisted bundle must never carry the raw secret")
	}
	if !strings.Contains(string(rev1), "[REDACTED:provider_token]") {
		t.Fatalf("the persisted bundle must carry the redaction placeholder")
	}
}

// ---------------------------------------------------------------------------
// routing policy body round-trip (the Artifact contract)
// ---------------------------------------------------------------------------

// TestRoutingPolicyBodyRoundTrip: the canonical routing-policy Artifact
// body parses back into the same immutable policy set, and the content
// comparison ignores the observed executable pins while catching a
// changed provider.
func TestRoutingPolicyBodyRoundTrip(t *testing.T) {
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	set := policySet(
		policy(model.PurposeImplementation, bindingFor(t, reg, "fake", "", "", ""), bindingFor(t, reg, "codex", "/usr/bin/codex", strings.Repeat("ab", 32), "0.141.0")),
		policy(model.PurposeReview, bindingFor(t, reg, "fake", "", "", "")),
	)
	body, err := agent.MarshalRoutingPolicySet(set)
	requireNoError(t, err)
	parsed, err := agent.ParseRoutingPolicySet(body)
	requireNoError(t, err)
	if !agent.ContentEqual(set, parsed) {
		t.Fatal("the parsed policy must approve the same route content")
	}
	// A changed provider is content drift.
	drifting := policySet(
		policy(model.PurposeImplementation, bindingFor(t, reg, "claude", "", "", ""), bindingFor(t, reg, "codex", "/usr/bin/codex", strings.Repeat("ab", 32), "0.141.0")),
		policy(model.PurposeReview, bindingFor(t, reg, "fake", "", "", "")),
	)
	if agent.ContentEqual(set, drifting) {
		t.Fatal("a changed provider must be content drift")
	}
}
