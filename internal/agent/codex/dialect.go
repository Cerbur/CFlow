// The codex JSONL dialect (cflow.dialect.codex-jsonl.v1): the bounded
// frame decoder of design 14.3 converts one raw `codex exec --json`
// frame onto the unified Agent Event contract. Real codex frames after
// session_started carry no session id (they carry turn ids); the dialect
// inherits the id established by the validated start event, and a frame
// that explicitly claims a different id fails closed (the binding's
// conflict rule). Known codex event types with no unified mapping
// (turn_started, turn_completed, non-assistant messages) are skipped;
// unknown event types, unparseable frames, unknown message roles, and
// unknown tool states fail closed with the raw frame for redacted
// evidence.
package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cflow.local/cflow/internal/agent"
)

// wireFrame is the codex JSONL wire shape (design 14.3: a bounded frame
// decoder reads one line). Unknown fields on known event types are
// tolerated: the provider owns the wire, the dialect owns the mapping,
// and fail-closed applies to unknown event types, never to extra fields.
type wireFrame struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id"`
	Timestamp  string          `json:"timestamp"`
	TurnID     string          `json:"turn_id"`
	MessageID  string          `json:"message_id"`
	Payload    json.RawMessage `json:"payload"`
	State      string          `json:"state"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCall   *toolCall       `json:"tool_call"`
	Input      json.RawMessage `json:"input"`
	Output     json.RawMessage `json:"output"`
	Usage      *usageFrame     `json:"usage"`
	Result     json.RawMessage `json:"result"`
	Code       string          `json:"code"`
	Message    string          `json:"message"`
}

// toolCall is the codex tool_call shape inside tool_execution frames;
// arguments is the raw JSON text of the call arguments.
type toolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// usageFrame is the codex usage shape.
type usageFrame struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// messagePayload is the codex message payload shape.
type messagePayload struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// contentBlock is one codex message content block.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// knownNoopTypes are real codex JSONL event types that carry no unified
// mapping (turn lifecycle frames). They are skipped, never failed:
// fail-closed applies to unknown events, not to known unmapped protocol
// facts.
var knownNoopTypes = map[string]bool{
	"turn_started":   true,
	"turn_completed": true,
}

// skippedRoles are known codex message roles with no unified mapping.
var skippedRoles = map[string]bool{"user": true, "system": true, "developer": true}

// errSessionIDMissing marks a session_started frame that claims no
// session id: per the binding's conflict rule missing ids fail
// PROVIDER_SESSION_ID_MISSING. The run maps this onto the fail-closed
// protocol finding with the raw frame as evidence.
var errSessionIDMissing = errors.New("session_started frame carries no session id")

// parseFrame decodes one raw codex JSONL frame into a unified event.
// established is the session id the stream's validated start event
// claimed ("" before the start). skip reports known frames that map to
// no unified event and must be passed over silently. Fail-closed rules
// at the wire boundary: a start frame with no session id, a terminal
// event before any validated start, and a completion without a valid
// JSON-object result are protocol findings (the Runtime enforces the
// same rules on the unified events; the dialect holds them here so the
// offending frame is preserved as evidence).
func parseFrame(raw []byte, established *agent.ProviderSessionID) (agent.Event, bool, error) {
	var wf wireFrame
	if err := json.Unmarshal(raw, &wf); err != nil {
		return agent.Event{}, false, fmt.Errorf("frame is not a codex JSONL object: %w", err)
	}
	switch wf.Type {
	case "session_started":
		if wf.SessionID == "" {
			return agent.Event{}, false, errSessionIDMissing
		}
		*established = agent.ProviderSessionID(wf.SessionID)
		return finishEvent(agent.Event{
			Type:      agent.EventSessionStarted,
			SessionID: agent.ProviderSessionID(wf.SessionID),
		}, raw), false, nil
	case "message":
		return parseMessageFrame(wf, raw, established)
	case "tool_execution":
		return parseToolFrame(wf, raw, established)
	case "usage":
		if wf.Usage == nil {
			return agent.Event{}, false, fmt.Errorf("usage frame carries no usage payload")
		}
		ev := agent.Event{
			Type:         agent.EventUsage,
			InputTokens:  wf.Usage.InputTokens,
			OutputTokens: wf.Usage.OutputTokens,
			CostUSD:      wf.Usage.CostUSD,
		}
		if err := withSession(&ev, wf, established); err != nil {
			return agent.Event{}, false, err
		}
		return finishEvent(ev, raw), false, nil
	case "session_finished":
		if *established == "" {
			return agent.Event{}, false, fmt.Errorf("terminal event before a validated session_started")
		}
		if !isJSONObject(wf.Result) {
			return agent.Event{}, false, fmt.Errorf("session_finished frame carries no valid JSON object result")
		}
		ev := agent.Event{
			Type:   agent.EventCompleted,
			Result: string(wf.Result),
		}
		if err := withSession(&ev, wf, established); err != nil {
			return agent.Event{}, false, err
		}
		return finishEvent(ev, raw), false, nil
	case "session_failed":
		if *established == "" {
			return agent.Event{}, false, fmt.Errorf("terminal event before a validated session_started")
		}
		ev := agent.Event{
			Type:    agent.EventFailed,
			Code:    wf.Code,
			Message: wf.Message,
		}
		if err := withSession(&ev, wf, established); err != nil {
			return agent.Event{}, false, err
		}
		return finishEvent(ev, raw), false, nil
	default:
		if knownNoopTypes[wf.Type] {
			return agent.Event{}, true, nil
		}
		return agent.Event{}, false, fmt.Errorf("unknown codex JSONL event type %q", wf.Type)
	}
}

// parseMessageFrame maps a codex message frame: assistant messages become
// unified assistant_message events (the joined text of their text
// blocks); user, system, and developer messages are known unmapped roles
// and are skipped; any other role fails closed.
func parseMessageFrame(wf wireFrame, raw []byte, established *agent.ProviderSessionID) (agent.Event, bool, error) {
	if len(wf.Payload) == 0 {
		return agent.Event{}, false, fmt.Errorf("message frame carries no payload")
	}
	var payload messagePayload
	if err := json.Unmarshal(wf.Payload, &payload); err != nil {
		return agent.Event{}, false, fmt.Errorf("message payload is not a codex message: %w", err)
	}
	if skippedRoles[payload.Role] {
		return agent.Event{}, true, nil
	}
	if payload.Role != "assistant" {
		return agent.Event{}, false, fmt.Errorf("message frame carries unknown role %q", payload.Role)
	}
	var text strings.Builder
	for _, block := range payload.Content {
		if (block.Type == "text" || block.Type == "output_text") && block.Text != "" {
			text.WriteString(block.Text)
		}
	}
	ev := agent.Event{Type: agent.EventAssistantMessage, Text: text.String()}
	if err := withSession(&ev, wf, established); err != nil {
		return agent.Event{}, false, err
	}
	return finishEvent(ev, raw), false, nil
}

// parseToolFrame maps a codex tool_execution frame: state running maps
// onto tool_started; states success and error map onto tool_finished
// with the raw output text; any other state fails closed.
func parseToolFrame(wf wireFrame, raw []byte, established *agent.ProviderSessionID) (agent.Event, bool, error) {
	if wf.ToolCall == nil || wf.ToolCall.Name == "" {
		return agent.Event{}, false, fmt.Errorf("tool_execution frame carries no tool call")
	}
	var ev agent.Event
	switch wf.State {
	case "running":
		ev = agent.Event{Type: agent.EventToolStarted, Tool: wf.ToolCall.Name, Input: wf.ToolCall.Arguments}
	case "success", "error":
		ev = agent.Event{Type: agent.EventToolFinished, Tool: wf.ToolCall.Name, Output: string(wf.Output)}
	default:
		return agent.Event{}, false, fmt.Errorf("tool_execution frame carries unknown state %q", wf.State)
	}
	if err := withSession(&ev, wf, established); err != nil {
		return agent.Event{}, false, err
	}
	return finishEvent(ev, raw), false, nil
}

// withSession applies the binding's session id rule (design 14.2):
// frames after the validated start inherit the established id; a frame
// that explicitly claims a different id conflicts and fails closed.
func withSession(ev *agent.Event, wf wireFrame, established *agent.ProviderSessionID) error {
	if wf.SessionID != "" {
		if *established != "" && agent.ProviderSessionID(wf.SessionID) != *established {
			return fmt.Errorf("session id %q conflicts with the established session %q", wf.SessionID, *established)
		}
		ev.SessionID = agent.ProviderSessionID(wf.SessionID)
		return nil
	}
	ev.SessionID = *established
	return nil
}

// isJSONObject reports whether raw is a non-null JSON object, the
// minimum validity of a structured completion payload. The Runtime's
// sequence validator enforces the same rule on the unified event; the
// dialect holds it at the wire boundary so the offending frame is
// preserved as redacted evidence.
func isJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return m != nil
}

// finishEvent stamps the sha256 of the raw wire frame (the protocol hash
// the Runtime persists) onto the unified event.
func finishEvent(ev agent.Event, raw []byte) agent.Event {
	sum := sha256.Sum256(raw)
	ev.FrameHash = hex.EncodeToString(sum[:])
	return ev
}
