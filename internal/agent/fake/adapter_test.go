package fake_test

// Fake Adapter tests: registry-bound detection, deterministic scripted
// streams (byte-identical for identical input), dialect parsing failures,
// crash points, Resume outcomes, and deterministic stop at every event
// boundary (brief Steps 4-5).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/model"
)

// fixtureDir is the committed Fake fixture directory, resolved from the
// package working directory.
var fixtureDir = filepath.Join("..", "..", "..", "tests", "testdata", "providers", "fake")

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

func newAdapter(t *testing.T) *fake.Adapter {
	t.Helper()
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	return fake.New(reg)
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// collect drains one run to completion and returns every event.
func collect(t *testing.T, r agent.Run) []agent.Event {
	t.Helper()
	var events []agent.Event
	for {
		ev, err := r.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("unexpected drain error: %v", err)
		}
		events = append(events, ev)
	}
}

// TestFakeDetectMatchesRegistryBinding: detection reports SUPPORTED with
// the exact registry revision, dialect, and capabilities of the Fake
// binding (design 14.2).
func TestFakeDetectMatchesRegistryBinding(t *testing.T) {
	reg, err := agent.LoadProviderRegistry()
	requireNoError(t, err)
	binding, err := reg.Select("fake")
	requireNoError(t, err)
	ad := fake.New(reg)
	inst, err := ad.Detect(context.Background())
	requireNoError(t, err)
	if inst.Compatibility != agent.CompatibilitySupported {
		t.Fatalf("expected SUPPORTED compatibility, got %s", inst.Compatibility)
	}
	if inst.DialectID != binding.Dialect.ID {
		t.Fatalf("detection dialect %q must match the binding %q", inst.DialectID, binding.Dialect.ID)
	}
	// The detection reports the binding revision the installation was
	// judged against — the same contract the codex and claude adapters
	// pin (the routing Compare-and-Swap compares the reported revision
	// against the binding's pinned revision, never the aggregate).
	if inst.RegistryRevision != binding.Revision {
		t.Fatalf("detection must report the binding revision, got %q", inst.RegistryRevision)
	}
	if !inst.Capabilities.StructuredEvents || !inst.Capabilities.ResumableSession ||
		!inst.Capabilities.SessionIDInEventStream {
		t.Fatalf("detection capabilities must reflect the binding, got %+v", inst.Capabilities)
	}
}

// TestFakeLoadDirLoadsEveryFixture: every committed fixture parses and is
// registered under its declared purpose.
func TestFakeLoadDirLoadsEveryFixture(t *testing.T) {
	ad := newAdapter(t)
	requireNoError(t, ad.LoadDir(fixtureDir))
	scripts := ad.Scripts()
	if len(scripts) != 5 {
		t.Fatalf("expected 5 committed fixtures, got %d", len(scripts))
	}
	byID := map[string]bool{}
	for _, s := range scripts {
		if !s.Purpose.Valid() || s.SessionID == "" {
			t.Fatalf("fixture %s must declare a valid purpose and session id", s.Name)
		}
		byID[s.SessionID] = true
	}
	for _, id := range []string{"p1", "p2", "c1", "c2", "p3"} {
		if !byID[id] {
			t.Fatalf("fixture session %s must be loaded", id)
		}
	}
}

// TestFakeParseRejectsMalformedScript: an unparseable first line or an
// unknown header field fails the load closed.
func TestFakeParseRejectsMalformedScript(t *testing.T) {
	ad := newAdapter(t)
	if err := ad.LoadScript([]byte("not json at all\n")); err == nil {
		t.Fatalf("a non-JSON first line must fail the load")
	}
	if err := ad.LoadScript([]byte(`{"fixture":"fake-run","bogus_field":1}` + "\n")); err == nil {
		t.Fatalf("an unknown header field must fail the load")
	}
	if err := ad.LoadScript([]byte(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.other.v1","purpose":"planning","session_id":"x"}` + "\n")); err == nil {
		t.Fatalf("a dialect outside the Fake binding must fail the load")
	}
	if _, err := fake.ParseScript([]byte("")); err == nil {
		t.Fatalf("an empty script must fail to parse")
	}
}

// TestFakeStartStreamsDeterministicEvents: the planning-pass fixture
// streams its seven unified events with protocol sequence, virtual timing,
// and tool payloads; two fresh adapters over the same input produce
// byte-identical streams (brief Step 5 determinism).
func TestFakeStartStreamsDeterministicEvents(t *testing.T) {
	text := readFixture(t, "planning-pass.jsonl")
	run := func() []agent.Event {
		ad := newAdapter(t)
		requireNoError(t, ad.LoadScript([]byte(text)))
		r, err := ad.Start(context.Background(), agent.StartRequest{
			Purpose:  agent.PurposePlanner,
			Provider: "fake",
			Prompt:   "plan",
		})
		requireNoError(t, err)
		return collect(t, r)
	}
	events := run()
	if len(events) != 7 {
		t.Fatalf("expected 7 unified events, got %d", len(events))
	}
	wantTypes := []agent.EventType{
		agent.EventSessionStarted,
		agent.EventAssistantMessage,
		agent.EventToolStarted,
		agent.EventToolFinished,
		agent.EventAssistantMessage,
		agent.EventUsage,
		agent.EventCompleted,
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("event %d: expected %s, got %s", i, want, events[i].Type)
		}
	}
	if events[0].SessionID != "p1" {
		t.Fatalf("session id must come from the wire, got %q", events[0].SessionID)
	}
	if events[3].Tool != "read_file" || events[3].Output == "" {
		t.Fatalf("tool payloads must be unified, got %+v", events[3])
	}
	if events[5].InputTokens != 1200 || events[5].CostUSD != 0.012 {
		t.Fatalf("usage must be unified, got %+v", events[5])
	}
	if events[6].Type == agent.EventCompleted && events[6].Result == "" {
		t.Fatalf("completion payload must be unified")
	}
	again := run()
	for i := range events {
		a, err := json.Marshal(events[i])
		requireNoError(t, err)
		b, err := json.Marshal(again[i])
		requireNoError(t, err)
		if string(a) != string(b) {
			t.Fatalf("event %d must be byte-identical across adapters", i)
		}
	}
}

// TestFakeScriptSelectionByPurpose: with several scripts loaded, Start
// selects the script bound to the requested purpose.
func TestFakeScriptSelectionByPurpose(t *testing.T) {
	ad := newAdapter(t)
	requireNoError(t, ad.LoadScript([]byte(readFixture(t, "planning-pass.jsonl"))))
	requireNoError(t, ad.LoadScript([]byte(readFixture(t, "coding-success.jsonl"))))
	r, err := ad.Start(context.Background(), agent.StartRequest{
		Purpose:  agent.PurposeImplementer,
		Provider: "fake",
		Prompt:   "code",
	})
	requireNoError(t, err)
	ev, err := r.Next(context.Background())
	requireNoError(t, err)
	if ev.SessionID != "c1" {
		t.Fatalf("implementer start must select the coding fixture, got session %q", ev.SessionID)
	}
}

// TestFakeRejectsMalformedFrameAtDrain: a malformed line surfaces as a
// protocol violation at the event boundary, never silently.
func TestFakeRejectsMalformedFrameAtDrain(t *testing.T) {
	ad := newAdapter(t)
	requireNoError(t, ad.LoadScript([]byte("session_started:m2\nnot json at all\n")))
	r, err := ad.Start(context.Background(), agent.StartRequest{
		Purpose:  agent.PurposePlanner,
		Provider: "fake",
		Prompt:   "plan",
	})
	requireNoError(t, err)
	if _, err := r.Next(context.Background()); err != nil {
		t.Fatalf("first frame must parse: %v", err)
	}
	_, err = r.Next(context.Background())
	var pe *agent.ProtocolError
	if !errors.As(err, &pe) || pe.Code != model.CodeProviderProtocolViolation {
		t.Fatalf("expected a protocol violation on the malformed frame, got %v", err)
	}
	if len(pe.Frame) == 0 {
		t.Fatalf("the protocol error must carry the raw frame for redacted evidence")
	}
}

// TestFakeRejectsUnknownWireEvent: an event type outside the closed Fake
// dialect is a protocol violation.
func TestFakeRejectsUnknownWireEvent(t *testing.T) {
	ad := newAdapter(t)
	requireNoError(t, ad.LoadScript([]byte("session_started:m3\nmystery_event:boom\n")))
	r, err := ad.Start(context.Background(), agent.StartRequest{
		Purpose:  agent.PurposePlanner,
		Provider: "fake",
		Prompt:   "plan",
	})
	requireNoError(t, err)
	if _, err := r.Next(context.Background()); err != nil {
		t.Fatalf("first frame must parse: %v", err)
	}
	_, err = r.Next(context.Background())
	var pe *agent.ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a protocol error, got %v", err)
	}
}

// TestFakeCrashPointStopsStream: a fixture-declared crash point stops the
// stream at the boundary with a ProcessCrash fact carrying the exit code.
func TestFakeCrashPointStopsStream(t *testing.T) {
	script := fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"planning","session_id":"cr1","exit_code":0,"crash_after":2}
session_started:cr1
assistant_message:about to crash
assistant_message:never seen
`)
	ad := newAdapter(t)
	requireNoError(t, ad.LoadScript([]byte(script)))
	r, err := ad.Start(context.Background(), agent.StartRequest{
		Purpose:  agent.PurposePlanner,
		Provider: "fake",
		Prompt:   "plan",
	})
	requireNoError(t, err)
	if _, err := r.Next(context.Background()); err != nil {
		t.Fatalf("first frame must parse: %v", err)
	}
	if _, err := r.Next(context.Background()); err != nil {
		t.Fatalf("second frame must parse: %v", err)
	}
	_, err = r.Next(context.Background())
	var crash *agent.ProcessCrash
	if !errors.As(err, &crash) {
		t.Fatalf("expected a process crash at the crash point, got %v", err)
	}
}

// TestFakeResumeOutcomes: the scripted Resume outcomes are deterministic.
func TestFakeResumeOutcomes(t *testing.T) {
	resume := func(outcome string) error {
		script := fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":"c2","exit_code":7,"resume":%q}
session_started:c2
assistant_message:x
session_finished:{"done":true}
`, outcome)
		ad := newAdapter(t)
		requireNoError(t, ad.LoadScript([]byte(script)))
		_, err := ad.Resume(context.Background(), agent.ResumeRequest{
			ProviderSessionID: "c2",
			Purpose:           agent.PurposeImplementer,
			Provider:          "fake",
			Prompt:            "continue",
		})
		return err
	}
	if err := resume("ok"); err != nil {
		t.Fatalf("resume ok must return a run: %v", err)
	}
	if err := resume("not-found"); err == nil {
		t.Fatalf("resume not-found must fail")
	}
	assertFaultCode(t, resume("unsupported"), model.CodeProviderProtocolUnsupported)
	if err := resume("crashed"); err == nil {
		t.Fatalf("resume crashed must fail")
	} else {
		var crash *agent.ProcessCrash
		if !errors.As(err, &crash) || crash.ExitCode != 7 {
			t.Fatalf("resume crashed must carry the exit fact, got %v", err)
		}
	}
}

// TestFakeCancelStopsAtBoundary: a run paused at its declared stop point
// yields events up to the boundary, then stops deterministically on
// Cancel (design 14.1: the Fake can deterministically stop at every event
// boundary).
func TestFakeCancelStopsAtBoundary(t *testing.T) {
	script := fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":"c3","exit_code":0,"stop_after":1}
session_started:c3
assistant_message:working
session_finished:{"done":true}
`)
	ad := newAdapter(t)
	requireNoError(t, ad.LoadScript([]byte(script)))
	r, err := ad.Start(context.Background(), agent.StartRequest{
		Purpose:  agent.PurposeImplementer,
		Provider: "fake",
		Prompt:   "code",
	})
	requireNoError(t, err)
	ev, err := r.Next(context.Background())
	requireNoError(t, err)
	if ev.Type != agent.EventSessionStarted {
		t.Fatalf("first event must be session_started, got %s", ev.Type)
	}
	requireNoError(t, ad.Cancel(context.Background(), agent.RunHandle{ProviderSessionID: "c3"}))
	_, err = r.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("the run must stop at the next boundary with EOF, got %v", err)
	}
}

// TestFakeInspectReportsRunning: Inspect reports the live run of an
// established session.
func TestFakeInspectReportsRunning(t *testing.T) {
	ad := newAdapter(t)
	requireNoError(t, ad.LoadScript([]byte(readFixture(t, "planning-pass.jsonl"))))
	r, err := ad.Start(context.Background(), agent.StartRequest{
		Purpose:  agent.PurposePlanner,
		Provider: "fake",
		Prompt:   "plan",
	})
	requireNoError(t, err)
	defer func() {
		for {
			if _, err := r.Next(context.Background()); err != nil {
				break
			}
		}
	}()
	fact, err := ad.Inspect(context.Background(), agent.ProviderSessionID("p1"))
	requireNoError(t, err)
	if !fact.Running || fact.Handle.ProviderSessionID != "p1" {
		t.Fatalf("inspect must report the live run, got %+v", fact)
	}
}

// TestFakeCompactFrameForms: the compact fixture shorthand binds values
// per event type (the verbatim runtime tests drive the Fake through it).
func TestFakeCompactFrameForms(t *testing.T) {
	ad := newAdapter(t)
	script := "session_started:a\nassistant_delta:typing\nassistant_message:hello\n" +
		"tool_started:read|{\"path\":\"x\"}\ntool_finished:read|{\"ok\":true}\n" +
		"usage:3|2|0.001\nsession_failed:CODE|message text\n"
	requireNoError(t, ad.LoadScript([]byte(script)))
	r, err := ad.Start(context.Background(), agent.StartRequest{
		Purpose:  agent.PurposePlanner,
		Provider: "fake",
		Prompt:   "plan",
	})
	requireNoError(t, err)
	var events []agent.Event
	for {
		ev, err := r.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		requireNoError(t, err)
		events = append(events, ev)
	}
	if len(events) != 7 {
		t.Fatalf("expected 7 compact events, got %d", len(events))
	}
	if events[1].Text != "typing" || events[3].Tool != "read" ||
		events[5].InputTokens != 3 || events[6].Code != "CODE" {
		t.Fatalf("compact values must bind per type, got %+v", events)
	}
	if !strings.Contains(events[6].Message, "message text") {
		t.Fatalf("compact failed payload must carry the message")
	}
}
