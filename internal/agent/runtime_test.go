package agent_test

// Agent Runtime tests (brief Step 1): the two verbatim protocol and
// independence tests, plus the coverage cases: malformed JSONL, missing
// terminal event, invalid schema payload, output redaction failure, Resume
// capability absent, Resume Session not found, cancellation at an event
// boundary, Context Bundle hash stability, and byte-identical Session and
// Context Bundle manifests for fixed Clock/ID input (brief Step 5).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// fixedClock is the deterministic Clock injected into every test Runtime
// (brief Step 5: byte-identical manifests for fixed Clock/ID input).
var fixedClock = func() time.Time { return time.Unix(1_700_000_000, 0) }

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
// requires. On macOS os.TempDir() lives under the /var symlink and is born
// 0755, so the fixture resolves symlinks and chmods to 0700 (the same
// discipline the artifact store tests use).
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

// requireNoError fails the test on an unexpected error.
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

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

// newFakeRuntime builds a Runtime wired to a single-script Fake Adapter
// (single-script mode serves any purpose), with a fixed Clock, a fresh
// sequential ID source, the test redaction registry, and a fresh evidence
// root. When the script header declares seed: true, the declared session
// is hydrated as an existing ACTIVE session (a resume target).
func newFakeRuntime(t *testing.T, script string) *agent.Runtime {
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

// readFixture returns the content of one committed Fake fixture.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "testdata", "providers", "fake", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// fixtureStart builds a plain Fake Start request.
func fixtureStart(purpose model.AgentPurpose) agent.StartRequest {
	return agent.StartRequest{
		Purpose:  purpose,
		Provider: "fake",
		Prompt:   "Plan the work.",
		Input:    map[string]any{"requirement": "Add search"},
	}
}

// fixtureStartWithParent builds a Start request superseding the provider
// session id parent.
func fixtureStartWithParent(purpose model.AgentPurpose, parent string) agent.StartRequest {
	req := fixtureStart(purpose)
	req.Supersedes = agent.ProviderSessionID(parent)
	return req
}

// fixtureResume builds a Fake Resume request carrying a full fallback
// context for the Context Bundle.
func fixtureResume(purpose model.AgentPurpose, sessionID string) agent.ResumeRequest {
	return agent.ResumeRequest{
		ProviderSessionID: agent.ProviderSessionID(sessionID),
		Purpose:           purpose,
		Provider:          "fake",
		Prompt:            "Continue the work.",
		Input:             map[string]any{"note": "resume"},
		Context:           fixtureContextInput(),
	}
}

// fixtureContextInput is the minimum Context Bundle content list (PRD
// 已确认：Session Resume 失败与跨 Provider 上下文交接): requirement, active
// Plan/Spec/Catalog/Workflow revision+hash, repository baseline, stage
// summary, confirmed decisions, failure evidence, open questions, and the
// permission boundary.
func fixtureContextInput() agent.ContextInput {
	return agent.ContextInput{
		Requirement:        "Add search to the docs site.",
		Plan:               agent.ArtifactPin{Type: "plan", Revision: 3, Hash: strings.Repeat("a1", 32)},
		Spec:               agent.ArtifactPin{Type: "spec", Revision: 2, Hash: strings.Repeat("b2", 32)},
		Catalog:            agent.ArtifactPin{Type: "catalog", Revision: 1, Hash: strings.Repeat("c3", 32)},
		Workflow:           agent.ArtifactPin{Type: "workflow", Revision: 4, Hash: strings.Repeat("d4", 32)},
		RepositoryBaseline: "9f86d081884c7d659a2feaa0c55ad015",
		StageSummary:       "Implementation started; index package written.",
		Decisions:          []string{"Scope is docs search only."},
		FailureEvidence:    []model.EvidenceRef{{Kind: model.EvidenceProtocolEvent, Hash: strings.Repeat("e5", 32), Subject: "resume"}},
		OpenQuestions:      []string{"Should search index the generated site?"},
		PermissionBoundary: "Fake provider default permissions; no sandbox claim.",
	}
}

// validRun builds a valid single-session Implementer fixture text whose
// session pre-exists (seed: true), so it can serve as a resume target and
// as a parent lineage in the independence test.
func validRun(id string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":%q,"exit_code":0,"resume":"ok","seed":true}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","text":"Working on the implementation.","at_ms":10}
{"type":"session_finished","result":{"commits":["feat: x"],"tests_passed":3},"at_ms":20}
`, id, id)
}

// ---------------------------------------------------------------------------
// Verbatim brief Step 1 tests
// ---------------------------------------------------------------------------

// TestConflictingSessionIDsFailClosed (brief Step 1, verbatim): a stream
// that establishes session a and then claims session b stops the run and
// produces a non-retryable protocol Finding.
func TestConflictingSessionIDsFailClosed(t *testing.T) {
	rt := newFakeRuntime(t, "session_started:a\nsession_started:b\n")
	_, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	assertFaultCode(t, err, model.CodeProviderProtocolViolation)
}

// TestReviewerCannotReuseImplementerSession (brief Step 1, verbatim): a
// Task Reviewer Start superseding an Implementer Session crosses the role
// lineage and fails with SESSION_INDEPENDENCE_VIOLATION (design 14.4).
func TestReviewerCannotReuseImplementerSession(t *testing.T) {
	rt := newFakeRuntime(t, validRun("s1"))
	_, err := rt.Start(context.Background(), fixtureStartWithParent(agent.PurposeTaskReviewer, "s1"))
	assertFaultCode(t, err, model.CodeSessionIndependenceViolation)
}

// ---------------------------------------------------------------------------
// Protocol fail-closed coverage (design 14.3, PRD 已确认：未知 Provider CLI
// 协议 Fail-closed)
// ---------------------------------------------------------------------------

// TestMalformedJSONLFailsClosed: an unparseable frame stops the affected
// process with a protocol violation; nothing is treated as success.
func TestMalformedJSONLFailsClosed(t *testing.T) {
	rt := newFakeRuntime(t, "session_started:m2\nnot json at all\n")
	_, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	assertFaultCode(t, err, model.CodeProviderProtocolViolation)
}

// TestMissingStartEventFailsClosed: a stream that never declares
// session_started cannot identify a Session (acceptance: Session ID
// appears through a validated start event).
func TestMissingStartEventFailsClosed(t *testing.T) {
	rt := newFakeRuntime(t, "assistant_message:hello\n")
	_, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	assertFaultCode(t, err, model.CodeProviderProtocolViolation)
}

// TestMissingSessionIDFailsClosed: a JSON frame after session_started
// that carries no session id fails per the Fake binding's conflict rule
// (PROVIDER_PROTOCOL_VIOLATION). The JSON wire form is the faithful form:
// compact shorthand inherits the established id.
func TestMissingSessionIDFailsClosed(t *testing.T) {
	rt := newFakeRuntime(t, "session_started:m1\n{\"type\":\"assistant_message\",\"text\":\"no id here\"}\n")
	_, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	assertFaultCode(t, err, model.CodeProviderProtocolViolation)
}

// TestUnknownEventTypeFailsClosed: an event type outside the closed Fake
// dialect (fixture protocol-invalid.jsonl) stops the run with a protocol
// violation.
func TestUnknownEventTypeFailsClosed(t *testing.T) {
	rt := newFakeRuntime(t, readFixture(t, "protocol-invalid.jsonl"))
	_, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	assertFaultCode(t, err, model.CodeProviderProtocolViolation)
}

// TestInvalidSchemaPayloadFailsClosed: a completed payload that is not a
// JSON object is an unverified completion and fails closed.
func TestInvalidSchemaPayloadFailsClosed(t *testing.T) {
	rt := newFakeRuntime(t, "session_started:m3\nsession_finished:\"just a string\"\n")
	_, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	assertFaultCode(t, err, model.CodeProviderProtocolViolation)
}

// TestMissingTerminalEventFailsClosed: a stream that ends without a
// terminal event never completes; the exit code cannot override the
// missing structured completion and the run fails with a retryable crash
// (design: a validated event schema, not stdout prose or exit code,
// identifies structured completion).
func TestMissingTerminalEventFailsClosed(t *testing.T) {
	rt := newFakeRuntime(t, "session_started:m4\nassistant_message:never finishes\n")
	_, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	assertFaultCode(t, err, model.CodeAgentProcessCrashed)
}

// TestCrashPointFailsClosed: a fixture-declared crash point (process died
// mid-stream, exit code 0) cannot complete the run.
func TestCrashPointFailsClosed(t *testing.T) {
	script := header("crash", "planning", "cr1", `"crash_after":2`) + "\n" +
		"session_started:cr1\nassistant_message:about to crash\nassistant_message:never seen\n"
	rt := newFakeRuntime(t, script)
	_, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	assertFaultCode(t, err, model.CodeAgentProcessCrashed)
}

// TestOutputRedactionFailureFailsClosed: a NUL byte in assistant output
// poisons the Redactor and the pipeline fails closed with
// SENSITIVE_DATA_REDACTION_FAILED (Task 3 contract).
func TestOutputRedactionFailureFailsClosed(t *testing.T) {
	rt := newFakeRuntime(t, "session_started:r1\nassistant_message:token sk-abc123def4567890\u0000boom\n")
	_, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	assertFaultCode(t, err, model.CodeSensitiveDataRedactionFailed)
}

// ---------------------------------------------------------------------------
// Successful and failed runs (redacted evidence, hashes)
// ---------------------------------------------------------------------------

// TestPlanningPassRunSucceeds: the planning-pass fixture completes, the
// Session appears through a validated start event, and the runtime
// persists only redacted complete events plus protocol/prompt/input
// hashes.
func TestPlanningPassRunSucceeds(t *testing.T) {
	rt := newFakeRuntime(t, readFixture(t, "planning-pass.jsonl"))
	res, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	requireNoError(t, err)
	if res.Status != model.RunSucceeded {
		t.Fatalf("expected SUCCEEDED run, got %s", res.Status)
	}
	if res.Session.ProviderSessionID != "p1" {
		t.Fatalf("session id should come from the validated start event, got %q", res.Session.ProviderSessionID)
	}
	if res.Session.Status != model.SessionCompleted {
		t.Fatalf("expected COMPLETED session, got %s", res.Session.Status)
	}
	if res.Terminal == nil || res.Terminal.Type != agent.EventCompleted {
		t.Fatalf("expected a completed terminal event")
	}
	if len(res.Events) != 7 {
		t.Fatalf("expected 7 unified events, got %d", len(res.Events))
	}
	if res.Events[0].Type != agent.EventSessionStarted || res.Events[0].Seq != 1 {
		t.Fatalf("first event must be the validated session_started")
	}
	if res.PromptHash == "" || res.InputHash == "" {
		t.Fatalf("prompt/input hashes must be recorded")
	}
	manifest, err := os.ReadFile(filepath.Join(evidenceRoot(t, rt), "sessions", string(res.Session.ID), "manifest.json"))
	requireNoError(t, err)
	if res.ManifestHash == "" || !strings.Contains(string(manifest), res.ManifestHash) {
		t.Fatalf("session manifest must be persisted and carry its own hash")
	}
	for i, ev := range res.Events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d must carry protocol sequence %d", i, i+1)
		}
	}
}

// TestRedactionAppliedToPersistedEvidence: only redacted complete events
// are persisted; a provider token in assistant output is replaced by its
// stable placeholder and never appears on disk.
func TestRedactionAppliedToPersistedEvidence(t *testing.T) {
	script := "session_started:p9\nassistant_message:token sk-abc123def4567890 leaked\nsession_finished:{\"ok\":true}\n"
	rt := newFakeRuntime(t, script)
	res, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	requireNoError(t, err)
	if !strings.Contains(res.Events[1].Text, "[REDACTED:provider_token]") {
		t.Fatalf("event text must be redacted, got %q", res.Events[1].Text)
	}
	if strings.Contains(res.Events[1].Text, "sk-abc123def4567890") {
		t.Fatalf("raw secret must never appear in the run result")
	}
	events, err := os.ReadFile(filepath.Join(evidenceRoot(t, rt), "events", string(res.Session.ID)+".jsonl"))
	requireNoError(t, err)
	if strings.Contains(string(events), "sk-abc123def4567890") {
		t.Fatalf("raw secret must never be persisted")
	}
	if !strings.Contains(string(events), "[REDACTED:provider_token]") {
		t.Fatalf("persisted evidence must carry the redacted placeholder")
	}
}

// TestPlanningReviseRunFails: an agent-declared session_failed is a
// terminal fact, never a success; the run FAILS with the provider's code
// and message recorded.
func TestPlanningReviseRunFails(t *testing.T) {
	rt := newFakeRuntime(t, readFixture(t, "planning-revise.jsonl"))
	res, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	requireNoError(t, err)
	if res.Status != model.RunFailed {
		t.Fatalf("expected FAILED run, got %s", res.Status)
	}
	if res.Session.Status != model.SessionFailed {
		t.Fatalf("expected FAILED session, got %s", res.Session.Status)
	}
	if res.Terminal == nil || res.Terminal.Type != agent.EventFailed {
		t.Fatalf("expected a failed terminal event")
	}
	if res.Terminal.Code != "PLAN_REVISION_REQUIRED" {
		t.Fatalf("terminal failure code must be recorded, got %q", res.Terminal.Code)
	}
}

// TestCodingSuccessRunSucceeds: the coding-success fixture completes with
// tool events and usage.
func TestCodingSuccessRunSucceeds(t *testing.T) {
	rt := newFakeRuntime(t, readFixture(t, "coding-success.jsonl"))
	res, err := rt.Start(context.Background(), fixtureStart(agent.PurposeImplementer))
	requireNoError(t, err)
	if res.Status != model.RunSucceeded {
		t.Fatalf("expected SUCCEEDED run, got %s", res.Status)
	}
	if len(res.Events) != 9 {
		t.Fatalf("expected 9 unified events, got %d", len(res.Events))
	}
	if res.Events[3].Type != agent.EventToolStarted || res.Events[3].Tool != "write_file" {
		t.Fatalf("tool events must carry tool and input, got %+v", res.Events[3])
	}
	var sawUsage bool
	for _, ev := range res.Events {
		if ev.Type == agent.EventUsage {
			sawUsage = true
			if ev.InputTokens != 5200 || ev.OutputTokens != 1400 {
				t.Fatalf("usage tokens must be recorded, got %+v", ev)
			}
		}
	}
	if !sawUsage {
		t.Fatalf("usage event must be unified into the run")
	}
}

// TestStartWithUnknownParentFailsClosed: a successor Start whose parent
// session is unknown cannot establish lineage.
func TestStartWithUnknownParentFailsClosed(t *testing.T) {
	rt := newFakeRuntime(t, validRun("s1"))
	_, err := rt.Start(context.Background(), fixtureStartWithParent(agent.PurposeImplementer, "ghost"))
	assertFaultCode(t, err, model.CodeInvalidInput)
}

// TestStartCannotClaimSessionOfAnotherPurpose: a Start whose script claims
// a provider session id already bound to another purpose crosses the
// lineage (design 14.4).
func TestStartCannotClaimSessionOfAnotherPurpose(t *testing.T) {
	script := "session_started:s2\nassistant_message:x\nsession_finished:{\"ok\":true}\n"
	rt := newFakeRuntime(t, script)
	if _, err := rt.Start(context.Background(), fixtureStart(agent.PurposeImplementer)); err != nil {
		t.Fatalf("first implementer start must succeed: %v", err)
	}
	_, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	assertFaultCode(t, err, model.CodeSessionIndependenceViolation)
}

// ---------------------------------------------------------------------------
// Resume (design 14.4)
// ---------------------------------------------------------------------------

// TestResumeSessionNotFound: resuming a provider session the Runtime has
// no facts for fails closed before any provider call.
func TestResumeSessionNotFound(t *testing.T) {
	rt := newFakeRuntime(t, validRun("s1"))
	_, err := rt.Resume(context.Background(), fixtureResume(agent.PurposeImplementer, "ghost"))
	assertFaultCode(t, err, model.CodeInvalidInput)
}

// TestResumePurposeMismatchFailsClosed: resuming a session under a
// different purpose violates Session independence.
func TestResumePurposeMismatchFailsClosed(t *testing.T) {
	rt := newFakeRuntime(t, validRun("s1"))
	_, err := rt.Resume(context.Background(), fixtureResume(agent.PurposePlanner, "s1"))
	assertFaultCode(t, err, model.CodeSessionIndependenceViolation)
}

// TestResumeTerminalSessionFailsClosed: a terminal Session (COMPLETED or
// LOST) can never be resumed (design 14.4): the resume fails closed with
// SESSION_INDEPENDENCE_VIOLATION and no successor lineage may be created
// from a terminal session.
func TestResumeTerminalSessionFailsClosed(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		script := header("resume-ok", "implementation", "s1", `"resume":"ok","seed":true`) + "\n" +
			"session_started:s1\nassistant_message:continuing\nsession_finished:{\"done\":true}\n"
		rt := newFakeRuntime(t, script)
		if _, err := rt.Resume(context.Background(), fixtureResume(agent.PurposeImplementer, "s1")); err != nil {
			t.Fatalf("first resume must succeed: %v", err)
		}
		_, err := rt.Resume(context.Background(), fixtureResume(agent.PurposeImplementer, "s1"))
		assertFaultCode(t, err, model.CodeSessionIndependenceViolation)
		if n := countSuccessors(t, rt); n != 0 {
			t.Fatalf("no successor lineage may be created from a terminal session, got %d", n)
		}
	})
	t.Run("lost", func(t *testing.T) {
		rt := newFakeRuntime(t, readFixture(t, "resume-missing.jsonl"))
		res, err := rt.Resume(context.Background(), fixtureResume(agent.PurposeImplementer, "c2"))
		requireNoError(t, err)
		if res.Fallback == nil {
			t.Fatalf("expected a resume fallback")
		}
		_, err = rt.Resume(context.Background(), fixtureResume(agent.PurposeImplementer, "c2"))
		assertFaultCode(t, err, model.CodeSessionIndependenceViolation)
		if n := countSuccessors(t, rt); n != 1 {
			t.Fatalf("a second resume of a LOST session must not chain a second successor, got %d", n)
		}
	})
}

// countSuccessors counts the ledger sessions that supersede another
// session (successor lineage records).
func countSuccessors(t *testing.T, rt *agent.Runtime) int {
	t.Helper()
	n := 0
	for _, f := range rt.Sessions() {
		if f.Session.Supersedes != "" {
			n++
		}
	}
	return n
}

// TestResumeCapabilityAbsent: a provider that cannot resume the session
// blocks with PROVIDER_PROTOCOL_UNSUPPORTED and never creates a fallback.
func TestResumeCapabilityAbsent(t *testing.T) {
	script := header("unsupported-resume", "planning", "u1", `"resume":"unsupported","seed":true`) + "\n" +
		"session_started:u1\nassistant_message:x\nsession_finished:{\"ok\":true}\n"
	rt := newFakeRuntime(t, script)
	_, err := rt.Resume(context.Background(), fixtureResume(agent.PurposePlanner, "u1"))
	assertFaultCode(t, err, model.CodeProviderProtocolUnsupported)
}

// TestResumeOkContinuesSession: a successful native Resume re-establishes
// the session and runs to its terminal event.
func TestResumeOkContinuesSession(t *testing.T) {
	script := header("resume-ok", "implementation", "s1", `"resume":"ok","seed":true`) + "\n" +
		"session_started:s1\nassistant_message:continuing\nsession_finished:{\"done\":true}\n"
	rt := newFakeRuntime(t, script)
	res, err := rt.Resume(context.Background(), fixtureResume(agent.PurposeImplementer, "s1"))
	requireNoError(t, err)
	if res.Fallback != nil {
		t.Fatalf("a successful resume must not fall back")
	}
	if res.Run == nil || res.Run.Status != model.RunSucceeded {
		t.Fatalf("expected a SUCCEEDED resumed run")
	}
	if res.Session.Status != model.SessionCompleted {
		t.Fatalf("expected COMPLETED session, got %s", res.Session.Status)
	}
}

// TestResumeFailureCreatesLostOriginalAndSuccessorBundle: when native
// Resume fails (fixture resume-missing.jsonl), the original Session is
// retained as LOST, an immutable redacted Context Bundle is written, the
// successor Adapter capabilities are validated, and a successor Session
// with supersedes_session_id is created. Retry charging is left to the
// Decision Kernel.
func TestResumeFailureCreatesLostOriginalAndSuccessorBundle(t *testing.T) {
	rt := newFakeRuntime(t, readFixture(t, "resume-missing.jsonl"))
	res, err := rt.Resume(context.Background(), fixtureResume(agent.PurposeImplementer, "c2"))
	requireNoError(t, err)
	if res.Fallback == nil {
		t.Fatalf("expected a resume fallback")
	}
	fb := res.Fallback
	if fb.LostSession.ProviderSessionID != "c2" || fb.LostSession.Status != model.SessionLost {
		t.Fatalf("original session must be retained as LOST, got %+v", fb.LostSession)
	}
	b := fb.ContextBundle
	if b.Revision != 1 || b.Hash == "" {
		t.Fatalf("context bundle must be versioned and hashed, got %+v", b)
	}
	if b.Context.Requirement != "Add search to the docs site." {
		t.Fatalf("bundle must carry the requirement, got %q", b.Context.Requirement)
	}
	if b.Context.Plan.Revision != 3 || b.Context.Workflow.Revision != 4 {
		t.Fatalf("bundle must pin the active artifact revisions, got %+v", b.Context.Plan)
	}
	if b.Context.RepositoryBaseline == "" || b.Context.StageSummary == "" || len(b.Context.Decisions) == 0 ||
		len(b.Context.OpenQuestions) == 0 || b.Context.PermissionBoundary == "" || len(b.Context.FailureEvidence) == 0 {
		t.Fatalf("bundle must carry the PRD minimum content list")
	}
	if fb.SuccessorSession.Status != model.SessionStarting {
		t.Fatalf("successor session must be created STARTING, got %s", fb.SuccessorSession.Status)
	}
	if fb.SuccessorSession.Supersedes != fb.LostSession.ID {
		t.Fatalf("successor must point at the original session via supersedes_session_id")
	}
	if fb.SuccessorSession.Purpose != agent.PurposeImplementer {
		t.Fatalf("successor must keep the original purpose (lineage), got %s", fb.SuccessorSession.Purpose)
	}
	// The immutable bundle and the LOST manifest are durable.
	rev1, err := os.ReadFile(filepath.Join(evidenceRoot(t, rt), "bundles", string(fb.LostSession.ID), "rev-1.json"))
	requireNoError(t, err)
	if !strings.Contains(string(rev1), b.Hash) {
		t.Fatalf("the persisted bundle must carry its own hash")
	}
	_, err = os.Stat(filepath.Join(evidenceRoot(t, rt), "sessions", string(fb.LostSession.ID), "manifest.json"))
	requireNoError(t, err)
}

// TestContextBundleRevisionImmutableAndHashStable: Context Bundle updates
// always create a new Revision and never modify a persisted Revision in
// place; identical inputs with a fixed Clock yield a byte-identical
// Revision manifest.
func TestContextBundleRevisionImmutableAndHashStable(t *testing.T) {
	rt := newFakeRuntime(t, validRun("s1"))
	sess := findSession(t, rt, "s1")
	req := agent.ContextBundleRequest{
		SessionID:         sess.ID,
		ProviderSessionID: agent.ProviderSessionID(sess.ProviderSessionID),
		Purpose:           sess.Purpose,
		Context:           fixtureContextInput(),
	}
	b1, err := rt.CreateContextBundle(context.Background(), req)
	requireNoError(t, err)
	b2, err := rt.CreateContextBundle(context.Background(), req)
	requireNoError(t, err)
	if b1.Revision != 1 || b2.Revision != 2 {
		t.Fatalf("bundle revisions must be allocated sequentially, got %d and %d", b1.Revision, b2.Revision)
	}
	root := evidenceRoot(t, rt)
	rev1Path := filepath.Join(root, "bundles", string(sess.ID), "rev-1.json")
	rev2Path := filepath.Join(root, "bundles", string(sess.ID), "rev-2.json")
	rev1a, err := os.ReadFile(rev1Path)
	requireNoError(t, err)
	rev1b, err := os.ReadFile(rev1Path)
	requireNoError(t, err)
	if string(rev1a) != string(rev1b) {
		t.Fatalf("a persisted Revision must never be modified in place")
	}
	if _, err := os.Stat(rev2Path); err != nil {
		t.Fatalf("the update must create a new Revision file: %v", err)
	}
	// A fresh Runtime with the same fixed Clock/IDs and inputs produces a
	// byte-identical Revision 1 manifest (hash stability).
	rt2 := newFakeRuntime(t, validRun("s1"))
	sess2 := findSession(t, rt2, "s1")
	b3, err := rt2.CreateContextBundle(context.Background(), agent.ContextBundleRequest{
		SessionID:         sess2.ID,
		ProviderSessionID: agent.ProviderSessionID(sess2.ProviderSessionID),
		Purpose:           sess2.Purpose,
		Context:           fixtureContextInput(),
	})
	requireNoError(t, err)
	if b3.Hash != b1.Hash {
		t.Fatalf("identical bundle inputs must produce the same hash, got %s and %s", b3.Hash, b1.Hash)
	}
	rev1c, err := os.ReadFile(filepath.Join(evidenceRoot(t, rt2), "bundles", string(sess2.ID), "rev-1.json"))
	requireNoError(t, err)
	if string(rev1c) != string(rev1a) {
		t.Fatalf("bundle manifests must be byte-identical for fixed Clock/ID input")
	}
}

// ---------------------------------------------------------------------------
// Cancellation at every event boundary (design 14.1, Fake capability)
// ---------------------------------------------------------------------------

// TestCancellationStopsAtEventBoundary: a run paused at a declared stop
// point (stop_after) is cancelled between events; the runtime stops the
// Fake at the boundary and settles the run and session as CANCELLED.
func TestCancellationStopsAtEventBoundary(t *testing.T) {
	script := header("cancel-target", "implementation", "c3", `"stop_after":1`) + "\n" +
		"session_started:c3\nassistant_message:working\nusage:5|2|0.001\nsession_finished:{\"done\":true}\n"
	rt := newFakeRuntime(t, script)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type startOutcome struct {
		res *agent.RunResult
		err error
	}
	out := make(chan startOutcome, 1)
	go func() {
		res, err := rt.Start(ctx, fixtureStart(agent.PurposeImplementer))
		out <- startOutcome{res, err}
	}()
	fact := waitSessionActive(t, rt, "c3")
	requireNoError(t, rt.Cancel(context.Background(), fact.Handle))
	select {
	case oc := <-out:
		if oc.err != nil {
			t.Fatalf("a cancelled run must settle, got error: %v", oc.err)
		}
		if oc.res.Status != model.RunCancelled {
			t.Fatalf("expected CANCELLED run, got %s", oc.res.Status)
		}
		if oc.res.Session.Status != model.SessionCancelled {
			t.Fatalf("expected CANCELLED session, got %s", oc.res.Session.Status)
		}
		if len(oc.res.Events) != 1 {
			t.Fatalf("the run must stop at the first event boundary, got %d events", len(oc.res.Events))
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("cancelled run did not settle")
	}
}

// ---------------------------------------------------------------------------
// Deterministic manifests for fixed Clock/ID input (brief Step 5)
// ---------------------------------------------------------------------------

// runPlanningOnce runs the planning-pass fixture once through a fresh
// Runtime and returns the persisted manifest and events bytes.
func runPlanningOnce(t *testing.T) (manifest, events []byte) {
	t.Helper()
	rt := newFakeRuntime(t, readFixture(t, "planning-pass.jsonl"))
	res, err := rt.Start(context.Background(), fixtureStart(agent.PurposePlanner))
	requireNoError(t, err)
	if res.Status != model.RunSucceeded {
		t.Fatalf("planning pass must succeed, got %s", res.Status)
	}
	root := evidenceRoot(t, rt)
	manifest, err = os.ReadFile(filepath.Join(root, "sessions", string(res.Session.ID), "manifest.json"))
	requireNoError(t, err)
	events, err = os.ReadFile(filepath.Join(root, "events", string(res.Session.ID)+".jsonl"))
	requireNoError(t, err)
	return manifest, events
}

// TestSessionManifestByteIdenticalAcrossRuns: two fresh Runtimes with the
// same fixed Clock and ID source persist byte-identical Session manifests
// and redacted event streams (brief Step 5).
func TestSessionManifestByteIdenticalAcrossRuns(t *testing.T) {
	m1, e1 := runPlanningOnce(t)
	m2, e2 := runPlanningOnce(t)
	if string(m1) != string(m2) {
		t.Fatalf("session manifests must be byte-identical for fixed Clock/ID input")
	}
	if string(e1) != string(e2) {
		t.Fatalf("redacted event evidence must be byte-identical for fixed Clock/ID input")
	}
}

// runResumeFallbackOnce runs the resume-missing fallback once through a
// fresh Runtime and returns the LOST manifest, the bundle, and the
// successor facts.
func runResumeFallbackOnce(t *testing.T) (lostManifest, bundle []byte, successor model.Session) {
	t.Helper()
	rt := newFakeRuntime(t, readFixture(t, "resume-missing.jsonl"))
	res, err := rt.Resume(context.Background(), fixtureResume(agent.PurposeImplementer, "c2"))
	requireNoError(t, err)
	if res.Fallback == nil {
		t.Fatalf("expected a resume fallback")
	}
	root := evidenceRoot(t, rt)
	lostManifest, err = os.ReadFile(filepath.Join(root, "sessions", string(res.Fallback.LostSession.ID), "manifest.json"))
	requireNoError(t, err)
	bundle, err = os.ReadFile(filepath.Join(root, "bundles", string(res.Fallback.LostSession.ID), "rev-1.json"))
	requireNoError(t, err)
	return lostManifest, bundle, res.Fallback.SuccessorSession
}

// TestContextBundleManifestByteIdenticalAcrossRuns: the resume fallback
// persists byte-identical LOST Session manifests and Context Bundle
// manifests for fixed Clock/ID input (brief Step 5).
func TestContextBundleManifestByteIdenticalAcrossRuns(t *testing.T) {
	l1, b1, s1 := runResumeFallbackOnce(t)
	l2, b2, s2 := runResumeFallbackOnce(t)
	if string(l1) != string(l2) {
		t.Fatalf("lost session manifests must be byte-identical for fixed Clock/ID input")
	}
	if string(b1) != string(b2) {
		t.Fatalf("context bundle manifests must be byte-identical for fixed Clock/ID input")
	}
	if s1.Supersedes != s2.Supersedes || s1.ID != s2.ID || s1.Purpose != s2.Purpose {
		t.Fatalf("successor facts must be deterministic, got %+v and %+v", s1, s2)
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// header builds a fixture header line from the shared fields plus extra
// key:value JSON pairs.
func header(script, purpose, sessionID, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":%q,"session_id":%q,"exit_code":0%s}`, purpose, sessionID, extra)
}

// evidenceRoot resolves the EvidenceDir of a Runtime under test.
func evidenceRoot(t *testing.T, rt *agent.Runtime) string {
	t.Helper()
	root, err := rt.EvidenceDir()
	requireNoError(t, err)
	return root
}

// findSession returns the runtime ledger fact for one provider session id.
func findSession(t *testing.T, rt *agent.Runtime, providerSessionID string) model.Session {
	t.Helper()
	for _, f := range rt.Sessions() {
		if f.Session.ProviderSessionID == providerSessionID {
			return f.Session
		}
	}
	t.Fatalf("session %q not found in the runtime ledger", providerSessionID)
	return model.Session{}
}

// waitSessionActive polls Inspect until the session is ACTIVE (the run has
// been established and the handle registered), then returns its fact.
func waitSessionActive(t *testing.T, rt *agent.Runtime, providerSessionID string) agent.SessionFact {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		fact, err := rt.Inspect(context.Background(), agent.ProviderSessionID(providerSessionID))
		if err == nil && fact.Session.Status == model.SessionActive && fact.Handle.RunID != "" {
			return fact
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %q never became active (last err: %v)", providerSessionID, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// bootstrapFixture is a planning bootstrap stream whose session_started
// establishes the Provider's own session id.
func bootstrapFixture(sessionID string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"planning","session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","text":"bootstrap","at_ms":10}
{"type":"session_finished","result":{},"at_ms":20}`, sessionID, sessionID)
}

// failingCancelAdapter wraps one Adapter and returns a fixed error from
// Cancel, so a test can assert the Runtime propagates the controlled-stop
// failure instead of swallowing it.
type failingCancelAdapter struct {
	agent.Adapter
	cancelErr error
}

func (a *failingCancelAdapter) Cancel(ctx context.Context, handle agent.RunHandle) error {
	if a.cancelErr != nil {
		return a.cancelErr
	}
	return a.Adapter.Cancel(ctx, handle)
}

// TestBootstrapPropagatesControlledStopError (fail-closed security finding):
// Runtime.Bootstrap must return the controlled-stop error instead of
// swallowing it after the start event.
func TestBootstrapPropagatesControlledStopError(t *testing.T) {
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	inner := fake.New(reg)
	requireNoError(t, inner.LoadScript([]byte(bootstrapFixture("provider-sess-1"))))
	ad := &failingCancelAdapter{Adapter: inner, cancelErr: fmt.Errorf("controlled stop failed")}
	rt, err := agent.NewRuntime(agent.RuntimeOptions{
		Now:         fixedClock,
		IDs:         model.SequentialIDSource(),
		Registry:    reg,
		Redaction:   testRedactionRegistry(),
		EvidenceDir: tempRoot(t),
		Adapters:    map[string]agent.Adapter{"fake": ad},
	})
	requireNoError(t, err)
	defer func() { _ = rt.Close() }()
	_, err = rt.Bootstrap(context.Background(), agent.BootstrapRequest{
		Purpose: model.PurposePlanning, Provider: "fake",
		Prompt: "discuss the requirement", SessionID: "cflow-sess-1",
	})
	if err == nil {
		t.Fatal("bootstrap swallowed the controlled-stop error")
	}
}
