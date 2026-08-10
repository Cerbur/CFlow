package app

// Native requirement discussion tests (TUI task 12, design §9): Prepare
// runs the managed bootstrap binding the Provider's own session identity,
// the Bridge return persists the process exit facts and moves the Session
// to INTERACTIVE_IDLE, Continue resumes the SAME Provider Session, Switch
// requires a DIFFERENT provider with a new session and a persisted
// superseded linkage, and Finish drives a managed structured resume on the
// same Provider Session that produces the immutable, schema-validated
// ArtifactDiscussionHandoff.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/claude"
	"cflow.local/cflow/internal/agent/codex"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// recordingAdapter wraps one Adapter and records the Input of the first
// Start request, so a test can assert the managed bootstrap input carried
// the switch successor's Context Bundle content.
type recordingAdapter struct {
	agent.Adapter
	mu    sync.Mutex
	input any
}

func (r *recordingAdapter) Start(ctx context.Context, req agent.StartRequest) (agent.Run, error) {
	r.mu.Lock()
	r.input = req.Input
	r.mu.Unlock()
	return r.Adapter.Start(ctx, req)
}

func (r *recordingAdapter) Resume(ctx context.Context, req agent.ResumeRequest) (agent.Run, error) {
	r.mu.Lock()
	r.input = req.Input
	r.mu.Unlock()
	return r.Adapter.Resume(ctx, req)
}

// bootstrapScript is a planning fixture whose session_started establishes
// the Provider's own session id; the managed bootstrap stops the start run
// after the validated start event.
func bootstrapScript(sessionID string) string {
	return bootstrapScriptDialect("cflow.dialect.fake.v1", sessionID)
}

// bootstrapScriptDialect is the dialect-parameterized bootstrap fixture.
func bootstrapScriptDialect(dialect, sessionID string) string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"` + dialect + `","purpose":"planning","session_id":"` + sessionID + `","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"` + sessionID + `","at_ms":0}
{"type":"assistant_message","session_id":"` + sessionID + `","text":"bootstrap","at_ms":10}
{"type":"session_finished","session_id":"` + sessionID + `","result":{},"at_ms":20}`
}

// handoffResumeScript is a planning fixture whose resume ("ok") produces
// the strict handoff content fields the managed structured resume returns.
func handoffResumeScript(sessionID, contentJSON string) string {
	return handoffResumeScriptDialect("cflow.dialect.fake.v1", sessionID, contentJSON)
}

func handoffResumeScriptDialect(dialect, sessionID, contentJSON string) string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"` + dialect + `","purpose":"planning","session_id":"` + sessionID + `","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"` + sessionID + `","at_ms":0}
{"type":"assistant_message","session_id":"` + sessionID + `","text":"assembling the handoff","at_ms":10}
{"type":"session_finished","session_id":"` + sessionID + `","result":` + contentJSON + `,"at_ms":20}`
}

// validHandoffDecisions is the strict handoff content the user types
// (content fields only; the runtime facts are bound by CFlow).
const validHandoffDecisions = `{"targets":"division by zero must error","constraints":"no external dependencies","non_goals":"no other arithmetic changes","acceptance_criteria":"Divide returns a typed error on zero","open_questions":"error wording","user_decisions":[{"topic":"error type","decision":"typed error"}]}`

// prepareNative returns the prepared native discussion outcome of one
// bootstrap-loaded Application.
func (fx *planningFixture) prepareNative(t *testing.T, wf model.WorkflowID, providerSession string) (Outcome, *Application) {
	t.Helper()
	a := fx.app(bootstrapScript(providerSession))
	out, err := a.Execute(context.Background(),
		PrepareNativeDiscussionCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		t.Fatalf("prepare native discussion: %v", err)
	}
	return out, a
}

// returnNative sends the Bridge return command of one interactive turn.
func returnNative(t *testing.T, a *Application, wf model.WorkflowID, session model.SessionID, code int, providerSession string) {
	t.Helper()
	_, err := a.Execute(context.Background(), NativeDiscussionReturnCommand{
		Workflow: wf, Session: session,
		Exit:            process.Exit{Code: code, Fact: process.FactProcessExit},
		Provider:        "fake",
		ProviderSession: agent.ProviderSessionID(providerSession),
	})
	if err != nil {
		t.Fatalf("native discussion return: %v", err)
	}
}

// TestNativeDiscussionPrepareBindsRealProviderSession (remediation plan
// requirement 1): the managed bootstrap captures the Provider's own session
// identity from the validated session_started event; the recorded
// ProviderSessionID is never a CFlow Session id.
func TestNativeDiscussionPrepareBindsRealProviderSession(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	out, a := fx.prepareNative(t, wf, "provider-sess-1")
	if out.SessionID == "" {
		t.Fatal("prepare carried no session id")
	}
	if out.Native == nil {
		t.Fatal("prepare carried no native bridge request")
	}
	if out.Native.ProviderSession != agent.ProviderSessionID("provider-sess-1") {
		t.Fatalf("provider session = %q, want %q", out.Native.ProviderSession, "provider-sess-1")
	}
	if string(out.Native.ProviderSession) == string(out.SessionID) {
		t.Fatalf("the CFlow session id %q was used as the provider identity", out.SessionID)
	}
	// The persisted Session record binds the Provider's own session id.
	iv := fx.inspect(wf)
	if len(iv.Sessions) == 0 {
		t.Fatal("no session persisted")
	}
	s := iv.Sessions[len(iv.Sessions)-1]
	if s.ProviderSessionID != "provider-sess-1" {
		t.Fatalf("persisted provider session = %q, want %q", s.ProviderSessionID, "provider-sess-1")
	}
	if s.Status != model.SessionStarting {
		t.Fatalf("session status = %s, want STARTING", s.Status)
	}
	_ = a
}

// TestNativeDiscussionClaudeBootstrapUsesTypedInput guards the real Claude
// adapter contract: Native Discussion bootstrap must pass the managed
// claude.Input, not the generic discussion input.
func TestNativeDiscussionClaudeBootstrapUsesTypedInput(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-claude-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatal(err)
	}
	ad := namedFake(reg, "claude")
	if err := ad.LoadScript([]byte(bootstrapScriptDialect("cflow.dialect.claude-stream-json.v1", "claude-sess-1"))); err != nil {
		t.Fatal(err)
	}
	rec := &recordingAdapter{Adapter: ad}
	a := fx.appWithAdapters(map[string]agent.Adapter{"claude": rec})
	if _, err := a.Execute(context.Background(), PrepareNativeDiscussionCommand{
		Workflow: wf,
		Provider: "claude",
	}); err != nil {
		t.Fatalf("prepare claude native discussion: %v", err)
	}

	rec.mu.Lock()
	gotInput := rec.input
	rec.mu.Unlock()
	if _, ok := gotInput.(claude.Input); !ok {
		t.Fatalf("claude bootstrap input type = %T, want claude.Input", gotInput)
	}
}

func TestNativeDiscussionClaudeFinishUsesHandoffSchema(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-claude-finish", false)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatal(err)
	}
	ad := namedFake(reg, "claude")
	if err := ad.LoadScript([]byte(handoffResumeScriptDialect("cflow.dialect.claude-stream-json.v1", "claude-sess-finish", validHandoffDecisions))); err != nil {
		t.Fatal(err)
	}
	rec := &recordingAdapter{Adapter: ad}
	a := fx.appWithAdapters(map[string]agent.Adapter{"claude": rec})
	out, err := a.Execute(context.Background(), PrepareNativeDiscussionCommand{
		Workflow: wf,
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("prepare claude native discussion: %v", err)
	}
	_, err = a.Execute(context.Background(), NativeDiscussionReturnCommand{
		Workflow:        wf,
		Session:         out.SessionID,
		Exit:            process.Exit{Code: 0, Fact: process.FactProcessExit},
		Provider:        "claude",
		ProviderSession: agent.ProviderSessionID("claude-sess-finish"),
	})
	if err != nil {
		t.Fatalf("native discussion return: %v", err)
	}
	if _, err := a.Execute(context.Background(), FreezeDiscussionCommand{Workflow: wf, Session: out.SessionID}); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if _, err := a.Execute(context.Background(), FinishDiscussionCommand{
		Workflow:  wf,
		Session:   out.SessionID,
		Decisions: []byte(validHandoffDecisions),
	}); err != nil {
		t.Fatalf("finish claude native discussion: %v", err)
	}
	rec.mu.Lock()
	gotInput := rec.input
	rec.mu.Unlock()
	in, ok := gotInput.(claude.Input)
	if !ok {
		t.Fatalf("claude finish input type = %T, want claude.Input", gotInput)
	}
	if in.SchemaJSON != nativeDiscussionHandoffSchema {
		t.Fatalf("claude finish schema = %s, want handoff schema", in.SchemaJSON)
	}
}

// TestNativeDiscussionAllocatesRunIDsAcrossWorkflows verifies that the
// globally unique runs.id identity is not restarted at run-1 for every
// Workflow. A second Workflow must be able to start its first native
// discussion even after another Workflow already opened a Run.
func TestNativeDiscussionAllocatesRunIDsAcrossWorkflows(t *testing.T) {
	fx := newPlanningFixture(t)
	first, err := fx.create("first-native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	fx.prepareNative(t, first, "provider-sess-1")

	second, err := fx.create("second-native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := fx.prepareNative(t, second, "provider-sess-2")
	if out.SessionID == "" {
		t.Fatal("second native discussion did not start")
	}
}

// TestNativeDiscussionReturnPersistsFactsAndIdles (remediation plan
// requirement 3): on the Bridge return the Kernel persists the process
// exit facts and moves the Session to INTERACTIVE_IDLE. A non-zero exit is
// NOT a discussion failure by itself.
func TestNativeDiscussionReturnPersistsFactsAndIdles(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	out, a := fx.prepareNative(t, wf, "provider-sess-1")

	// A non-zero exit is a normal return: the Session becomes
	// INTERACTIVE_IDLE and the process record carries the exit code.
	returnNative(t, a, wf, out.SessionID, 3, "provider-sess-1")

	iv := fx.inspect(wf)
	s := iv.Sessions[len(iv.Sessions)-1]
	if s.Status != model.SessionInteractiveIdle {
		t.Fatalf("session status = %s, want INTERACTIVE_IDLE", s.Status)
	}
	if s.ProviderSessionID != "provider-sess-1" {
		t.Fatalf("the binding drifted on return: %q", s.ProviderSessionID)
	}
	proc := iv.Processes[len(iv.Processes)-1]
	if proc.Status != model.ProcessStatusExited || proc.ExitCode != 3 {
		t.Fatalf("process exit facts = %+v, want EXITED/3", proc)
	}

	// The Return Page offers the return actions per the revalidated facts.
	qv, err := a.Query(context.Background(), DiscussionReturnQuery{Workflow: wf})
	if err != nil {
		t.Fatal(err)
	}
	dv := qv.(DiscussionReturnView)
	if dv.SessionStatus != model.SessionInteractiveIdle {
		t.Fatalf("return view session status = %s", dv.SessionStatus)
	}
	if len(dv.Actions) == 0 {
		t.Fatal("the return page offers no actions after a revalidated return")
	}
}

// TestNativeDiscussionReturnRejectsBindingDrift (remediation plan
// requirement 3): a return whose echoed binding does not match the recorded
// facts fails closed.
func TestNativeDiscussionReturnRejectsBindingDrift(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	out, a := fx.prepareNative(t, wf, "provider-sess-1")
	_, err = a.Execute(context.Background(), NativeDiscussionReturnCommand{
		Workflow: wf, Session: out.SessionID,
		Exit:            process.Exit{Code: 0, Fact: process.FactProcessExit},
		Provider:        "fake",
		ProviderSession: agent.ProviderSessionID("foreign-session"),
	})
	if err == nil {
		t.Fatal("a return with a foreign provider session was accepted")
	}
	if code, ok := model.CodeOf(err); !ok || code != model.CodeProviderBindingChanged {
		t.Fatalf("binding drift fault = %v, want PROVIDER_PROTOCOL_BINDING_CHANGED", err)
	}
}

// TestNativeDiscussionContinueResumesSameSession (remediation plan
// requirement 2): Continue re-arms the SAME CFlow Session and the SAME
// Provider Session — never a new identity.
func TestNativeDiscussionContinueResumesSameSession(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	out, a := fx.prepareNative(t, wf, "provider-sess-1")
	returnNative(t, a, wf, out.SessionID, 0, "provider-sess-1")

	cont, err := a.Execute(context.Background(),
		ContinueNativeDiscussionCommand{Workflow: wf, Session: out.SessionID})
	if err != nil {
		t.Fatalf("continue native discussion: %v", err)
	}
	if cont.SessionID != out.SessionID {
		t.Fatalf("continue session = %q, want the same session %q", cont.SessionID, out.SessionID)
	}
	if cont.Native == nil {
		t.Fatal("continue carried no native bridge request")
	}
	if cont.Native.ProviderSession != agent.ProviderSessionID("provider-sess-1") {
		t.Fatalf("continue provider session = %q, want the SAME %q", cont.Native.ProviderSession, "provider-sess-1")
	}
	// The continued turn's Bridge return persists a FRESH process record and
	// moves the same Session back to INTERACTIVE_IDLE.
	returnNative(t, a, wf, out.SessionID, 0, "provider-sess-1")
	iv := fx.inspect(wf)
	s := iv.Sessions[len(iv.Sessions)-1]
	if s.Status != model.SessionInteractiveIdle {
		t.Fatalf("session after the continued return = %s, want INTERACTIVE_IDLE", s.Status)
	}
	if s.ID != out.SessionID {
		t.Fatalf("continue created a new session %q, want the same %q", s.ID, out.SessionID)
	}
}

// TestNativeDiscussionSwitchRequiresDifferentProvider (remediation plan
// requirement 2): a switch to the SAME provider fails closed; a switch to a
// different provider creates a NEW CFlow Session whose Provider Session is
// established by a fresh managed start, and persists the superseded linkage
// and the switch reason.
func TestNativeDiscussionSwitchRequiresDifferentProvider(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	out, a := fx.prepareNative(t, wf, "provider-sess-1")
	returnNative(t, a, wf, out.SessionID, 0, "provider-sess-1")

	// Same provider: fail closed.
	if _, err := a.Execute(context.Background(), SwitchAgentCommand{
		Workflow: wf, Session: out.SessionID, Provider: "fake", Reason: "no change",
	}); err == nil {
		t.Fatal("a switch to the same provider was accepted")
	} else if code, ok := model.CodeOf(err); !ok || code != model.CodeSessionIndependenceViolation {
		t.Fatalf("same-provider switch fault = %v, want SESSION_INDEPENDENCE_VIOLATION", err)
	}

	// Different provider (a second named fake instance bound to the
	// registry's codex entry): the switch creates a new Session whose
	// bootstrap establishes the new Provider's own session id and reads the
	// superseded discussion's immutable Context Bundle.
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatal(err)
	}
	ad := namedFake(reg, "codex")
	if err := ad.LoadScript([]byte(bootstrapScriptDialect("cflow.dialect.codex-jsonl.v1", "codex-sess-1"))); err != nil {
		t.Fatal(err)
	}
	rec := &recordingAdapter{Adapter: ad}
	swA := fx.appWithAdapters(map[string]agent.Adapter{"codex": rec})
	swOut, err := swA.Execute(context.Background(), SwitchAgentCommand{
		Workflow: wf, Session: out.SessionID, Provider: "codex", Reason: "switch to codex for detail",
	})
	if err != nil {
		t.Fatalf("switch-agent: %v", err)
	}
	if swOut.SessionID == "" || swOut.SessionID == out.SessionID {
		t.Fatalf("switch created session %q, want a NEW session", swOut.SessionID)
	}
	if swOut.Native == nil || swOut.Native.ProviderSession != agent.ProviderSessionID("codex-sess-1") {
		t.Fatalf("switch bridge request = %+v, want the codex provider session", swOut.Native)
	}
	// The superseded linkage and the switch reason are persisted.
	iv := fx.inspect(wf)
	var news, old *model.Session
	for i := range iv.Sessions {
		if iv.Sessions[i].ID == swOut.SessionID {
			news = &iv.Sessions[i]
		}
		if iv.Sessions[i].ID == out.SessionID {
			old = &iv.Sessions[i]
		}
	}
	if news == nil {
		t.Fatal("the switched session was not persisted")
	}
	if news.Supersedes != out.SessionID || news.Provider != "codex" || news.ProviderSessionID != "codex-sess-1" {
		t.Fatalf("switched session = %+v", *news)
	}
	// The successor session record carries the created Context Bundle
	// reference (the sessions table context_bundle columns).
	if news.ContextBundleRevision < 1 || news.ContextBundlePath == "" || news.ContextBundleSha256 == "" {
		t.Fatalf("the switched session record does not carry the context bundle reference: %+v", *news)
	}
	if old == nil {
		t.Fatal("the superseded session is missing")
	}
	foundReason := false
	for _, f := range iv.Status.Findings {
		if f.Code == model.CodeSessionSuperseded && f.Subject == string(out.SessionID) && f.Text == "switch-agent: switch to codex for detail" {
			foundReason = true
		}
	}
	if !foundReason {
		t.Fatalf("the switch reason was not persisted as a finding: %+v", iv.Status.Findings)
	}
	// The successor managed bootstrap input carries the bundle CONTENT, so
	// the successor Provider starts with the prior discussion context.
	rec.mu.Lock()
	gotInput := rec.input
	rec.mu.Unlock()
	if gotInput == nil {
		t.Fatal("the switch bootstrap carried no managed start input")
	}
	nb, ok := gotInput.(codex.Input)
	if !ok {
		t.Fatalf("switch bootstrap input type = %T, want codex.Input", gotInput)
	}
	if nb.ContextBundleRef == "" {
		t.Fatal("the switch bootstrap input did not carry the context bundle reference")
	}
	if nb.ContextBundleRef != news.ContextBundlePath {
		t.Fatalf("bootstrap input bundle ref = %q, want %q",
			nb.ContextBundleRef, news.ContextBundlePath)
	}
}

// TestHasNativeDiscussionPropagatesReadError (fail-open closure, security
// finding): hasNativeDiscussion never swallows an aggregate read error — a
// workflow with an unreadable aggregate must fail closed (plan generation
// is refused), never silently classified as "no native discussion".
func TestHasNativeDiscussionPropagatesReadError(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	out, a := fx.prepareNative(t, wf, "provider-sess-1")
	returnNative(t, a, wf, out.SessionID, 0, "provider-sess-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.hasNativeDiscussion(ctx, wf); err == nil {
		t.Fatal("hasNativeDiscussion swallowed the aggregate read error")
	}
}

// TestNativeDiscussionFinishUsesManagedResume (remediation plan requirement
// 4): Finish drives a managed structured resume on the SAME Provider
// Session that produces the immutable ArtifactDiscussionHandoff; the caller
// never supplies a hand-written body as the authority.
func TestNativeDiscussionFinishUsesManagedResume(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	out, a := fx.prepareNative(t, wf, "provider-sess-1")
	returnNative(t, a, wf, out.SessionID, 0, "provider-sess-1")

	// Freeze the Change Set first (the TUI freezes before the handoff editor).
	frozen, err := a.Execute(context.Background(),
		FreezeDiscussionCommand{Workflow: wf, Session: out.SessionID})
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	csRef := frozen.ChangeSet.Ref

	// The managed resume on the SAME provider session produces the handoff
	// content; the Application binds the authoritative runtime facts and
	// validates the strict schema.
	finishA := fx.app(handoffResumeScript("provider-sess-1", validHandoffDecisions))
	if _, err := finishA.Execute(context.Background(), FinishDiscussionCommand{
		Workflow: wf, Session: out.SessionID, Decisions: []byte(validHandoffDecisions),
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	store, err := finishA.artifactStore(wf)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactDiscussionHandoff})
	if err != nil {
		t.Fatalf("resolve handoff: %v", err)
	}
	body, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.ValidateBody("discussion-handoff.json", body); err != nil {
		t.Fatalf("stored handoff fails the schema: %v", err)
	}
	var hf map[string]any
	if err := json.Unmarshal(body, &hf); err != nil {
		t.Fatal(err)
	}
	cs := hf["change_set"].(map[string]any)
	if cs["revision"] != float64(csRef.Revision) || cs["sha256"] != csRef.Hash {
		t.Fatalf("handoff change set = %v, want rev %d %s", cs, csRef.Revision, csRef.Hash)
	}
	if hf["session_id"] != string(out.SessionID) {
		t.Fatalf("handoff session = %v, want %q", hf["session_id"], out.SessionID)
	}
}

// TestNativeDiscussionPlanGenerationRequiresHandoff (remediation plan
// requirement 5): a workflow whose native discussion produced discussion
// turns but never finished (no Handoff + Change Set) cannot generate a
// Plan from the discussion alone — the Application refuses before any
// provider run.
func TestNativeDiscussionPlanGenerationRequiresHandoff(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	out, a := fx.prepareNative(t, wf, "provider-sess-1")
	returnNative(t, a, wf, out.SessionID, 0, "provider-sess-1")
	// No freeze, no finish: no Handoff and no frozen Change Set exist.
	if _, err := a.Execute(context.Background(),
		GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err == nil {
		t.Fatal("plan generation without a handoff and change set was accepted")
	}
}

// TestNativeDiscussionFinishRejectsManagedOutput (remediation plan
// requirement 4): a managed resume that produces invalid content is refused
// and nothing is written.
func TestNativeDiscussionFinishRejectsManagedOutput(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	out, a := fx.prepareNative(t, wf, "provider-sess-1")
	returnNative(t, a, wf, out.SessionID, 0, "provider-sess-1")
	if _, err := a.Execute(context.Background(),
		FreezeDiscussionCommand{Workflow: wf, Session: out.SessionID}); err != nil {
		t.Fatal(err)
	}
	// The managed output is missing the strict fields and binds a runtime fact.
	bad := `{"workflow_id":"x","targets":"t"}`
	finishA := fx.app(handoffResumeScript("provider-sess-1", bad))
	_, err = finishA.Execute(context.Background(), FinishDiscussionCommand{
		Workflow: wf, Session: out.SessionID, Decisions: []byte(validHandoffDecisions),
	})
	if err == nil {
		t.Fatal("an invalid managed handoff was accepted")
	}
	if code, ok := model.CodeOf(err); !ok || code != model.CodeSchemaInvalid {
		t.Fatalf("fault = %v, want SCHEMA_INVALID", err)
	}
}
