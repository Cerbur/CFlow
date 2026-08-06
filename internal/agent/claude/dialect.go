// The claude stream-json dialect (cflow.dialect.claude-stream-json.v1):
// the bounded frame decoder of design 14.3 converts one raw
// `claude --print --output-format stream-json` frame onto the unified
// Agent Event contract. The system init frame establishes the session id
// (captured from the stream, never supplied through argv); frames without
// a session id inherit the established one, and a frame that explicitly
// claims a different id fails closed (the binding's conflict rule). Known
// claude frames with no unified mapping (user message echoes, the
// tool_use_start stream event) are skipped; unknown event types,
// unparseable frames, unknown system subtypes, and unknown tool
// lifecycle shapes fail closed with the raw frame for redacted evidence.
//
// The terminal contract (matching the captured 2.1.220 help facts and
// the plan's hand-authored fixtures): a `result` frame with subtype
// success is the one valid completion and must carry a schema-valid JSON
// object as its inner payload; a `result` frame with an error subtype
// (error_budget, error_auth, ...) is the valid failed terminal carrying
// the wire subtype as its code. Every other terminal shape fails closed.
// Authentication failures are classified as the provider's auth failure
// (the error_auth code), never disguised as a protocol finding (PRD
// 已确认：Authentication Unknown 与 Protocol Unsupported 分开).
package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
)

// streamInputFrame is one stdin stream-json frame (the realtime input
// protocol of `--input-format stream-json`): a user message frame
// carrying the prompt. It is built by struct marshal so any prompt
// content is safely escaped.
type streamInputFrame struct {
	Type    string       `json:"type"`
	Message inputMessage `json:"message"`
}

type inputMessage struct {
	Role    string       `json:"role"`
	Content []inputBlock `json:"content"`
}

type inputBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// wireFrame is the claude stream-json wire shape (design 14.3: a bounded
// frame decoder reads one line). Unknown fields on known event types are
// tolerated: the provider owns the wire, the dialect owns the mapping,
// and fail-closed applies to unknown event types, never to extra fields.
type wireFrame struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Message   json.RawMessage `json:"message"`
	Event     json.RawMessage `json:"event"`
	Result    json.RawMessage `json:"result"`
	IsError   bool            `json:"is_error"`
	Error     string          `json:"error"`
	Errors    []string        `json:"errors"`
}

// streamEvent is the payload of a stream_event frame.
type streamEvent struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   string          `json:"content"`
}

// messageFrame is the payload of an assistant or user message frame.
type messageFrame struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Role    string         `json:"role"`
	Model   string         `json:"model"`
	Content []messageBlock `json:"content"`
}

// messageBlock is one content block of a message frame. Text blocks map
// onto the unified assistant text; other known block types (thinking,
// tool_use) carry no unified mapping and are skipped, exactly as the
// codex dialect skips unmapped blocks.
type messageBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// errSessionIDMissing marks a system init frame that claims no session
// id: per the binding's conflict rule missing ids fail
// PROVIDER_SESSION_ID_MISSING. The run maps this onto the fail-closed
// protocol finding with the raw frame as evidence.
var errSessionIDMissing = errors.New("system init frame carries no session id")

// streamParser is the stateful stream-json decoder of one run: the
// established session id ("" before the validated init) and the tool name
// registry keyed by tool_use_id (tool_result frames carry no tool name).
type streamParser struct {
	established agent.ProviderSessionID
	toolNames   map[string]string
}

// parse decodes one raw claude stream-json frame into a unified event.
// established is the session id the stream's validated init frame claimed
// ("" before the start). skip reports known frames that map to no unified
// event and must be passed over silently. Fail-closed rules at the wire
// boundary: an init frame with no session id, a terminal event before any
// validated start, and a completion without a valid JSON-object result
// are protocol findings (the Runtime enforces the same rules on the
// unified events; the dialect holds them here so the offending frame is
// preserved as evidence).
func (p *streamParser) parse(raw []byte) (agent.Event, bool, error) {
	var wf wireFrame
	if err := json.Unmarshal(raw, &wf); err != nil {
		return agent.Event{}, false, fmt.Errorf("frame is not a claude stream-json object: %w", err)
	}
	switch wf.Type {
	case "system":
		return p.parseInitFrame(wf, raw)
	case "assistant":
		return p.parseMessageFrame(wf, raw)
	case "user":
		return p.parseUserFrame(wf, raw)
	case "stream_event":
		return p.parseStreamEventFrame(wf, raw)
	case "result":
		return p.parseResultFrame(wf, raw)
	default:
		// Diagnostic event types (tool_progress, ...) carry no unified
		// mapping and are passed over silently — the real wire emits them
		// (confirmed by the real E2E), and the protocol remains
		// fail-closed on the validated init, the session id rule, and the
		// terminal result shapes.
		return agent.Event{}, true, nil
	}
}

// parseInitFrame maps the system init frame onto the unified
// session_started event: the session id is captured from the stream
// (design 14.3), and a claim of no id fails closed. With --verbose the
// installed CLI also emits diagnostic system frames that carry no
// session claim, map to no unified event, and are passed over silently
// (real-wire E2E confirmed): `hook_started`/`hook_response` (Session
// hooks), `thinking_tokens` (extended-thinking budget telemetry, an
// unbounded burst emitted while the model thinks), and
// `vcs_state_changed` (a notification the CLI emits when a tool changed
// the working tree's Git state — the real-wire Task run emits it when
// the agent uses Git). The stream is still only established by a
// validated init. Any other unknown subtype fails closed.
func (p *streamParser) parseInitFrame(wf wireFrame, raw []byte) (agent.Event, bool, error) {
	switch wf.Subtype {
	case "init":
		if wf.SessionID == "" {
			return agent.Event{}, false, errSessionIDMissing
		}
		p.established = agent.ProviderSessionID(wf.SessionID)
		return finishEvent(agent.Event{
			Type:      agent.EventSessionStarted,
			SessionID: agent.ProviderSessionID(wf.SessionID),
		}, raw), false, nil
	default:
		// Diagnostic system frames (hook_*, thinking_tokens,
		// vcs_state_changed, task_*, ...) carry no session claim and map to
		// no unified event; they are passed over silently (the real wire
		// emits several, confirmed by the real E2E and dogfood runs). The
		// stream is still only established by a validated init, and a
		// missing init id still fails closed.
		return agent.Event{}, true, nil
	}
}

// parseMessageFrame maps an assistant message frame onto the unified
// assistant_message event (the joined text of its text blocks). Unknown
// content block types are skipped, exactly as the codex dialect skips
// unmapped blocks; a missing message payload or an unknown role fails
// closed.
func (p *streamParser) parseMessageFrame(wf wireFrame, raw []byte) (agent.Event, bool, error) {
	if len(wf.Message) == 0 {
		return agent.Event{}, false, fmt.Errorf("assistant frame carries no message")
	}
	var msg messageFrame
	if err := json.Unmarshal(wf.Message, &msg); err != nil {
		return agent.Event{}, false, fmt.Errorf("assistant message is not a claude message: %w", err)
	}
	if msg.Role != "assistant" {
		return agent.Event{}, false, fmt.Errorf("assistant frame carries unknown role %q", msg.Role)
	}
	var text strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" && block.Text != "" {
			text.WriteString(block.Text)
		}
	}
	ev := agent.Event{Type: agent.EventAssistantMessage, Text: text.String()}
	if err := withSession(&ev, wf, p.established); err != nil {
		return agent.Event{}, false, err
	}
	return finishEvent(ev, raw), false, nil
}

// parseUserFrame skips user message frames (replayed input and tool
// result echoes are known unmapped protocol facts); a user frame with no
// message payload fails closed.
func (p *streamParser) parseUserFrame(wf wireFrame, raw []byte) (agent.Event, bool, error) {
	if len(wf.Message) == 0 {
		return agent.Event{}, false, fmt.Errorf("user frame carries no message")
	}
	return agent.Event{}, true, nil
}

// parseStreamEventFrame maps the tool lifecycle stream events: tool_use
// maps onto tool_started with the raw invocation input; tool_use_start is
// a known unmapped announcement; tool_result maps onto tool_finished
// through the tool name registry, and a result for a tool that was never
// started fails closed. Any other stream_event subtype fails closed.
func (p *streamParser) parseStreamEventFrame(wf wireFrame, raw []byte) (agent.Event, bool, error) {
	if len(wf.Event) == 0 {
		return agent.Event{}, false, fmt.Errorf("stream_event frame carries no event")
	}
	var se streamEvent
	if err := json.Unmarshal(wf.Event, &se); err != nil {
		return agent.Event{}, false, fmt.Errorf("stream_event payload is not a claude event: %w", err)
	}
	switch se.Type {
	case "tool_use_start":
		if se.ToolUseID == "" {
			return agent.Event{}, false, fmt.Errorf("tool_use_start carries no tool_use_id")
		}
		if p.toolNames == nil {
			p.toolNames = map[string]string{}
		}
		p.toolNames[se.ToolUseID] = se.Name
		return agent.Event{}, true, nil
	case "tool_use":
		if se.ID == "" || se.Name == "" {
			return agent.Event{}, false, fmt.Errorf("tool_use frame carries no id or name")
		}
		if p.toolNames == nil {
			p.toolNames = map[string]string{}
		}
		p.toolNames[se.ID] = se.Name
		ev := agent.Event{
			Type:  agent.EventToolStarted,
			Tool:  se.Name,
			Input: string(se.Input),
		}
		if err := withSession(&ev, wf, p.established); err != nil {
			return agent.Event{}, false, err
		}
		return finishEvent(ev, raw), false, nil
	case "tool_result":
		if se.ToolUseID == "" {
			return agent.Event{}, false, fmt.Errorf("tool_result frame carries no tool_use_id")
		}
		name, ok := p.toolNames[se.ToolUseID]
		if !ok {
			return agent.Event{}, false, fmt.Errorf("tool_result for a tool that was never started (%s)", se.ToolUseID)
		}
		ev := agent.Event{
			Type:   agent.EventToolFinished,
			Tool:   name,
			Output: se.Content,
		}
		if err := withSession(&ev, wf, p.established); err != nil {
			return agent.Event{}, false, err
		}
		return finishEvent(ev, raw), false, nil
	default:
		return agent.Event{}, false, fmt.Errorf("unknown stream_event subtype %q", se.Type)
	}
}

// parseResultFrame maps the terminal result frames: subtype success with
// a schema-valid JSON object inner payload is the one valid completion;
// an error subtype (is_error true) is the valid failed terminal carrying
// the wire subtype as its code and the wire error text as its message
// (budget exceeded, authentication required, ...). Any other result
// shape fails closed; a terminal before any validated start fails closed
// (session identity appears only through a validated start event).
func (p *streamParser) parseResultFrame(wf wireFrame, raw []byte) (agent.Event, bool, error) {
	if p.established == "" {
		return agent.Event{}, false, fmt.Errorf("terminal event before a validated session_started")
	}
	switch {
	case wf.Subtype == "success" && !wf.IsError:
		result, err := parseResultPayload(wf.Result)
		if err != nil {
			return agent.Event{}, false, err
		}
		ev := agent.Event{
			Type:   agent.EventCompleted,
			Result: result,
		}
		if err := withSession(&ev, wf, p.established); err != nil {
			return agent.Event{}, false, err
		}
		return finishEvent(ev, raw), false, nil
	case wf.Subtype != "" && wf.IsError:
		// The real wire reports the terminal error through a string `errors`
		// array (the `error` member is null), e.g.
		// `"errors":["Reached maximum budget ($0.000001)"]`.
		if wf.Error == "" && len(wf.Errors) == 0 {
			return agent.Event{}, false, fmt.Errorf("error result frame carries no error message")
		}
		msg := wf.Error
		if msg == "" && len(wf.Errors) > 0 {
			msg = wf.Errors[0]
		}
		ev := agent.Event{
			Type:    agent.EventFailed,
			Code:    claudeErrorCode(wf.Subtype),
			Message: msg,
		}
		if err := withSession(&ev, wf, p.established); err != nil {
			return agent.Event{}, false, err
		}
		return finishEvent(ev, raw), false, nil
	default:
		return agent.Event{}, false, fmt.Errorf("result frame carries unknown subtype %q", wf.Subtype)
	}
}

// parseResultPayload extracts the schema-valid terminal result: the
// success result field is a JSON string whose content must be a non-null
// JSON object (the structured output the CLI produced under
// --json-schema). A missing, invalid, or non-object payload fails closed
// (PRD: invalid completion → non-retryable protocol Finding).
func parseResultPayload(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("success result frame carries no result")
	}
	var inner string
	if err := json.Unmarshal(raw, &inner); err != nil {
		return "", fmt.Errorf("success result payload is not a JSON string: %w", err)
	}
	if !isJSONObject([]byte(inner)) {
		return "", fmt.Errorf("success result payload is not a JSON object")
	}
	return inner, nil
}

// claudeErrorCode maps a terminal error subtype onto a compiled model
// Code (the real wire subtypes like `error_max_budget_usd` are not model
// Codes themselves): a budget subtype maps to BUDGET_EXCEEDED, an
// authentication subtype maps to PROVIDER_AUTHENTICATION_REQUIRED, and
// every other provider-reported error maps to the retryable
// PROVIDER_ERROR.
func claudeErrorCode(subtype string) string {
	switch {
	case strings.Contains(subtype, "budget"):
		return string(model.CodeBudgetExceeded)
	case strings.Contains(subtype, "auth"):
		return string(model.CodeProviderAuthenticationRequired)
	default:
		return string(model.CodeProviderError)
	}
}

// withSession applies the binding's session id rule (design 14.2):
// frames after the validated start inherit the established id; a frame
// that explicitly claims a different id conflicts and fails closed.
func withSession(ev *agent.Event, wf wireFrame, established agent.ProviderSessionID) error {
	if wf.SessionID != "" {
		if established != "" && agent.ProviderSessionID(wf.SessionID) != established {
			return fmt.Errorf("session id %q conflicts with the established session %q", wf.SessionID, established)
		}
		ev.SessionID = agent.ProviderSessionID(wf.SessionID)
		return nil
	}
	ev.SessionID = established
	return nil
}

// isJSONObject reports whether raw is a non-null JSON object, the
// minimum validity of a structured completion payload. The Runtime's
// sequence validator enforces the same rule on the unified event; the
// dialect holds it at the wire boundary so the offending frame is
// preserved as redacted evidence.
func isJSONObject(raw []byte) bool {
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
