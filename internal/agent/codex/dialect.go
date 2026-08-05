// The codex JSONL dialect (cflow.dialect.codex-jsonl.v1): the bounded
// frame decoder of design 14.3 converts one raw `codex exec --json`
// frame onto the unified Agent Event contract. The real codex wire
// (0.141.0+, confirmed by the real Cross-Provider E2E) is a
// thread/turn/item protocol: a `thread.started` frame establishes the
// thread (session) id; `turn.started` begins one turn; `item.started` /
// `item.completed` carry the agent's work items (agent_message,
// command_execution, file_change, ...); `turn.completed` ends the turn
// with usage; `error` / `turn.failed` carry terminal failures. The
// dialect inherits the id established by the validated thread.started
// for every subsequent event; a frame that explicitly claims a
// different id fails closed (the binding's conflict rule). The
// structured terminal result is the last agent_message's text (the
// model's final output under --output-schema), validated as a JSON
// object at the wire boundary. Known codex events with no unified
// mapping are skipped; unknown event types, unparseable frames, and
// unknown item types fail closed with the raw frame for redacted
// evidence.
package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
)

// wireFrame is the codex JSONL wire shape (design 14.3: a bounded frame
// decoder reads one line). Unknown fields on known event types are
// tolerated: the provider owns the wire, the dialect owns the mapping,
// and fail-closed applies to unknown event types, never to extra fields.
type wireFrame struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     json.RawMessage `json:"item"`
	Error    json.RawMessage `json:"error"`
	Message  string          `json:"message"`
	Usage    *usageFrame     `json:"usage"`
}

// itemFrame is one codex work item: the "item" field of item.started /
// item.completed frames. agent_message carries the model's output text
// (a schema-shaped JSON string under --output-schema); command_execution
// carries the shell command and its aggregated output; file_change
// carries the changed paths; other known types are diagnostic and
// skipped.
type itemFrame struct {
	ID               string       `json:"id"`
	Type             string       `json:"type"`
	Text             string       `json:"text"`
	Command          string       `json:"command"`
	AggregatedOutput string       `json:"aggregated_output"`
	ExitCode         *int         `json:"exit_code"`
	Status           string       `json:"status"`
	Changes          []fileChange `json:"changes"`
}

// fileChange is one path change inside a file_change item.
type fileChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// usageFrame is the codex usage shape of the turn.completed frame.
type usageFrame struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// knownNoopItemTypes are real codex work item types that carry no unified
// mapping (file facts, planning/reasoning telemetry, and error items).
// They are skipped, never failed: fail-closed applies to unknown events,
// not to known unmapped protocol facts.
var knownNoopItemTypes = map[string]bool{
	"file_change":            true,
	"plan":                   true,
	"reasoning":              true,
	"memory":                 true,
	"error":                  true,
	"agent_message_metadata": true,
	"tool_use_metadata":      true,
	"worktree":               true,
	"collab_tool_call":       true,
	"collab_tool_response":   true,
}

// errSessionIDMissing marks a thread.started frame that claims no thread
// id: per the binding's conflict rule missing ids fail
// PROVIDER_SESSION_ID_MISSING. The run maps this onto the fail-closed
// protocol finding with the raw frame as evidence.
var errSessionIDMissing = errors.New("thread.started frame carries no thread id")

// streamParser is the stateful decoder of one run: the established
// thread (session) id ("" before the validated thread.started), the last
// agent_message text (the structured terminal result candidate of
// turn.completed), and the reported-failure flag (the real wire emits
// `error` and `turn.failed` together for one failure; only the first is
// mapped onto a unified failed event).
type streamParser struct {
	established   agent.ProviderSessionID
	lastAgentText string
	failed        bool
}

// parse decodes one raw codex JSONL frame into a unified event. skip
// reports known frames that map to no unified event and must be passed
// over silently. Fail-closed rules at the wire boundary: a thread.started
// with no id, a terminal event before any validated thread.started, a
// conflicting thread id, and a turn.completed without a valid JSON-object
// result are protocol findings (the Runtime enforces the same rules on
// the unified events; the dialect holds them here so the offending frame
// is preserved as evidence).
func (p *streamParser) parse(raw []byte) (agent.Event, bool, error) {
	var wf wireFrame
	if err := json.Unmarshal(raw, &wf); err != nil {
		return agent.Event{}, false, fmt.Errorf("frame is not a codex JSONL object: %w", err)
	}
	switch wf.Type {
	case "thread.started":
		if wf.ThreadID == "" {
			return agent.Event{}, false, errSessionIDMissing
		}
		if p.established != "" && agent.ProviderSessionID(wf.ThreadID) != p.established {
			return agent.Event{}, false, fmt.Errorf("thread id %q conflicts with the established session %q", wf.ThreadID, p.established)
		}
		p.established = agent.ProviderSessionID(wf.ThreadID)
		return finishEvent(agent.Event{
			Type:      agent.EventSessionStarted,
			SessionID: p.established,
		}, raw), false, nil
	case "turn.started":
		return agent.Event{}, true, nil
	case "item.started":
		return p.parseItemStarted(wf, raw)
	case "item.completed":
		return p.parseItemCompleted(wf, raw)
	case "item.updated":
		// An in-flight item progress update (the real E2E wire emits it for
		// long-running items); the authoritative item facts arrive on the
		// matching item.started/item.completed frames, so the update is a
		// known unmapped frame and passes over silently.
		return agent.Event{}, true, nil
	case "turn.completed":
		if p.established == "" {
			return agent.Event{}, false, fmt.Errorf("terminal event before a validated thread.started")
		}
		if !isJSONObjectText(p.lastAgentText) {
			return agent.Event{}, false, fmt.Errorf("turn.completed carries no valid JSON object result")
		}
		return finishEvent(agent.Event{
			Type:      agent.EventCompleted,
			Result:    p.lastAgentText,
			SessionID: p.established,
		}, raw), false, nil
	case "error":
		if p.failed {
			return agent.Event{}, true, nil
		}
		if p.established == "" {
			return agent.Event{}, false, fmt.Errorf("terminal event before a validated thread.started")
		}
		p.failed = true
		return finishEvent(agent.Event{
			Type:      agent.EventFailed,
			Code:      string(model.CodeProviderError),
			Message:   wf.Message,
			SessionID: p.established,
		}, raw), false, nil
	case "turn.failed":
		if p.failed {
			return agent.Event{}, true, nil
		}
		if p.established == "" {
			return agent.Event{}, false, fmt.Errorf("terminal event before a validated thread.started")
		}
		p.failed = true
		return finishEvent(agent.Event{
			Type:      agent.EventFailed,
			Code:      string(model.CodeProviderError),
			Message:   errorMessage(wf.Error, wf.Message),
			SessionID: p.established,
		}, raw), false, nil
	default:
		return agent.Event{}, false, fmt.Errorf("unknown codex JSONL event type %q", wf.Type)
	}
}

// parseItemStarted maps an item.started frame: a command_execution item
// is the tool call launch (tool_started with the shell command as the
// input); every other item type is a known unmapped fact and is skipped.
func (p *streamParser) parseItemStarted(wf wireFrame, raw []byte) (agent.Event, bool, error) {
	if p.established == "" {
		return agent.Event{}, false, fmt.Errorf("item frame before a validated thread.started")
	}
	var it itemFrame
	if err := json.Unmarshal(wf.Item, &it); err != nil {
		return agent.Event{}, false, fmt.Errorf("item frame is not a codex item: %w", err)
	}
	switch it.Type {
	case "command_execution":
		return finishEvent(agent.Event{
			Type:      agent.EventToolStarted,
			Tool:      "shell",
			Input:     it.Command,
			SessionID: p.established,
		}, raw), false, nil
	default:
		return agent.Event{}, true, nil
	}
}

// parseItemCompleted maps an item.completed frame: an agent_message item
// is the model's output (assistant_message) and becomes the candidate
// terminal result text; a command_execution item completes the launched
// tool (tool_finished with the aggregated output); other known item
// types are skipped.
func (p *streamParser) parseItemCompleted(wf wireFrame, raw []byte) (agent.Event, bool, error) {
	if p.established == "" {
		return agent.Event{}, false, fmt.Errorf("item frame before a validated thread.started")
	}
	var it itemFrame
	if err := json.Unmarshal(wf.Item, &it); err != nil {
		return agent.Event{}, false, fmt.Errorf("item frame is not a codex item: %w", err)
	}
	switch it.Type {
	case "agent_message":
		if it.Text != "" {
			p.lastAgentText = it.Text
		}
		return finishEvent(agent.Event{
			Type:      agent.EventAssistantMessage,
			Text:      it.Text,
			SessionID: p.established,
		}, raw), false, nil
	case "command_execution":
		return finishEvent(agent.Event{
			Type:      agent.EventToolFinished,
			Tool:      "shell",
			Output:    it.AggregatedOutput,
			SessionID: p.established,
		}, raw), false, nil
	default:
		if knownNoopItemTypes[it.Type] {
			return agent.Event{}, true, nil
		}
		return agent.Event{}, false, fmt.Errorf("item frame carries unknown type %q", it.Type)
	}
}

// errorMessage extracts the message of a turn.failed error payload,
// falling back to the frame's own message field and then the raw error
// text.
func errorMessage(raw json.RawMessage, fallback string) string {
	var e struct {
		Message string `json:"message"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return e.Message
	}
	if fallback != "" {
		return fallback
	}
	return string(raw)
}

// isJSONObjectText reports whether text is a non-null JSON object, the
// minimum validity of a structured completion payload. The Runtime's
// sequence validator enforces the same rule on the unified event; the
// dialect holds it at the wire boundary so the offending frame is
// preserved as redacted evidence.
func isJSONObjectText(text string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
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
