// The unified event pipeline (design 14.3): after the Adapter's bounded
// frame decoder and dialect parser have produced unified Events, this
// file validates the protocol sequence against the registry binding,
// redacts every text-bearing field through the Task 3 Redactor, and
// renders the redacted facts the evidence writer persists. Nothing
// bypasses this pipeline: unknown event types, conflicting Session IDs,
// missing required start events, malformed terminal frames, and invalid
// completion payloads stop the affected process and produce a
// non-retryable protocol Finding (PROVIDER_PROTOCOL_VIOLATION /
// PROVIDER_SESSION_ID_MISSING per the binding's conflict rule).
package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// requiredStartCapabilities are the protocol capabilities a Start route
// must prove from the registry binding before any Provider call (PRD
// 已确认：未知 Provider CLI 协议 Fail-closed).
var requiredStartCapabilities = []string{"session_id_on_start", "structured_output"}

// requiredResumeCapabilities are the protocol capabilities a Resume route
// must prove from the registry binding before any Provider call. Resume
// capabilities are validated separately from Start capabilities (PRD
// Agent Adapter: capability is per operation, never inferred).
var requiredResumeCapabilities = []string{"resume_by_session_id", "structured_output"}

// bindingHas reports whether the binding declares every capability in
// required.
func bindingHas(binding ProviderBinding, required []string) bool {
	for _, want := range required {
		ok := false
		for _, have := range binding.StartCapabilities {
			if have == want {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// bindingHasResume reports whether the binding declares every resume
// capability in required.
func bindingHasResume(binding ProviderBinding, required []string) bool {
	for _, want := range required {
		ok := false
		for _, have := range binding.ResumeCapabilities {
			if have == want {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// missingIDCode derives the fail-closed code for a frame that carries no
// Session id from the binding's conflict rule (design 14.2): the Fake
// dialect fails with PROVIDER_PROTOCOL_VIOLATION, Codex and Claude with
// PROVIDER_SESSION_ID_MISSING, exactly as their bindings declare.
func missingIDCode(binding ProviderBinding) model.Code {
	if strings.Contains(binding.SessionContract.ConflictRule, string(model.CodeProviderSessionIDMissing)) {
		return model.CodeProviderSessionIDMissing
	}
	return model.CodeProviderProtocolViolation
}

// protocolSequence is the per-stream protocol state machine (design
// 14.3). bound is the session id a Resume stream must re-affirm ("" on
// Start). The binding's session contract names the start event and the
// conflict rule; the unified terminal set is PRD-fixed (completed,
// failed).
type protocolSequence struct {
	binding     ProviderBinding
	bound       ProviderSessionID
	started     bool
	established ProviderSessionID
	terminated  bool
	seq         uint64
}

// newProtocolSequence builds the validator for one stream.
func newProtocolSequence(binding ProviderBinding, bound ProviderSessionID) *protocolSequence {
	return &protocolSequence{binding: binding, bound: bound}
}

// accept validates one unified event in sequence, assigns its protocol
// sequence number, and returns a *ProtocolError on any violation.
func (s *protocolSequence) accept(ev *Event) error {
	s.seq++
	ev.Seq = s.seq
	if s.terminated {
		return s.violation(ev, "event after the terminal frame")
	}
	if ev.Type == EventType(s.binding.SessionContract.StartEvent) {
		if s.started {
			return s.violation(ev, "duplicate start event")
		}
		s.started = true
		if ev.SessionID == "" {
			return s.missingID(ev, "start event carries no session id")
		}
		if s.bound != "" && ev.SessionID != s.bound {
			return s.violation(ev, "start event does not re-affirm the resumed session")
		}
		s.established = ev.SessionID
		return nil
	}
	if !ev.Type.Valid() {
		return s.violation(ev, "unknown unified event type")
	}
	if !s.started {
		return s.violation(ev, "missing required start event")
	}
	if ev.SessionID == "" {
		return s.missingID(ev, "event carries no session id")
	}
	if ev.SessionID != s.established {
		return s.violation(ev, "session id conflicts with the established session")
	}
	switch ev.Type {
	case EventCompleted:
		if !isJSONObject(ev.Result) {
			return s.violation(ev, "completion payload is not a valid JSON object")
		}
		s.terminated = true
	case EventFailed:
		if ev.Code == "" || ev.Message == "" {
			return s.violation(ev, "failure payload must carry code and message")
		}
		s.terminated = true
	}
	return nil
}

// completed reports whether the stream reached a terminal event.
func (s *protocolSequence) completed() bool { return s.terminated }

func (s *protocolSequence) violation(ev *Event, message string) error {
	return &ProtocolError{
		Code:      model.CodeProviderProtocolViolation,
		SessionID: s.established,
		Frame:     nil,
		Message:   fmt.Sprintf("%s (event %s, frame %d)", message, ev.Type, s.seq),
	}
}

func (s *protocolSequence) missingID(ev *Event, message string) error {
	return &ProtocolError{
		Code:      missingIDCode(s.binding),
		SessionID: s.established,
		Frame:     nil,
		Message:   fmt.Sprintf("%s (event %s, frame %d)", message, ev.Type, s.seq),
	}
}

// isJSONObject reports whether text is a non-null JSON object, the
// minimum validity of a structured completion payload.
func isJSONObject(text string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return false
	}
	return m != nil
}

// ---------------------------------------------------------------------------
// Redaction stage (Task 3 Redactor; PRD 脱敏)
// ---------------------------------------------------------------------------

// redactText redacts one bounded text frame to completion: WriteFrame
// emits the provably safe prefix and withholds the boundary tail, Flush
// emits the fully redacted tail. An event field is the atomic redaction
// frame; a failure poisons the Redactor and fails the pipeline closed.
func redactText(red *security.Redactor, text string) (string, error) {
	if text == "" {
		return "", nil
	}
	frame, err := red.WriteFrame([]byte(text))
	if err != nil {
		return "", err
	}
	flush, err := red.Flush()
	if err != nil {
		return "", err
	}
	return frame.Text + flush.Text, nil
}

// redactEvent redacts every text-bearing field of one unified event and
// returns a copy carrying only redacted content. Session ids and usage
// numbers are non-secret facts and pass through. A redaction failure
// returns the SENSITIVE_DATA_REDACTION_FAILED fault and nothing of the
// event may be persisted.
func redactEvent(red *security.Redactor, ev *Event) (*Event, error) {
	out := *ev
	var err error
	if out.Text, err = redactText(red, ev.Text); err != nil {
		return nil, err
	}
	if out.Tool, err = redactText(red, ev.Tool); err != nil {
		return nil, err
	}
	if out.Input, err = redactText(red, ev.Input); err != nil {
		return nil, err
	}
	if out.Output, err = redactText(red, ev.Output); err != nil {
		return nil, err
	}
	if out.Result, err = redactText(red, ev.Result); err != nil {
		return nil, err
	}
	if out.Message, err = redactText(red, ev.Message); err != nil {
		return nil, err
	}
	return &out, nil
}
