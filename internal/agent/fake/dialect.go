// The fake dialect parser (cflow.dialect.fake.v1): the bounded frame
// decoder and dialect parser of design 14.3, converting wire frames onto
// the unified Agent Event model. A malformed or unknown frame yields a
// *ProtocolError carrying the raw bytes for redacted evidence, so the
// Runtime fails the affected process closed.
package fake

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
)

// ---------------------------------------------------------------------------
// Dialect parser (fake wire -> unified Agent Event)
// ---------------------------------------------------------------------------

// parseFrameLine decodes one wire line: a strict JSON object (the
// faithful wire form, compact=false) or the compact "type:value"
// shorthand (compact=true).
func parseFrameLine(line []byte) (wireFrame, bool, error) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return wireFrame{}, false, fmt.Errorf("empty frame line")
	}
	if trimmed[0] == '{' {
		var wf wireFrame
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&wf); err != nil {
			return wireFrame{}, false, fmt.Errorf("frame is not a valid fake dialect JSON object: %w", err)
		}
		return wf, false, nil
	}
	wf, err := parseCompactFrame(trimmed)
	return wf, true, err
}

// parseCompactFrame decodes "type:value" and "type:a|b|c" lines.
func parseCompactFrame(line []byte) (wireFrame, error) {
	i := bytes.IndexByte(line, ':')
	if i <= 0 {
		return wireFrame{}, fmt.Errorf("compact frame must be type:value")
	}
	typ := string(line[:i])
	parts := bytes.Split(line[i+1:], []byte("|"))
	need := func(n int) error {
		if len(parts) != n {
			return fmt.Errorf("compact %s takes %d value(s)", typ, n)
		}
		return nil
	}
	switch typ {
	case "session_started":
		if err := need(1); err != nil {
			return wireFrame{}, err
		}
		return wireFrame{Type: typ, SessionID: string(parts[0])}, nil
	case "assistant_delta", "assistant_message":
		if err := need(1); err != nil {
			return wireFrame{}, err
		}
		return wireFrame{Type: typ, Text: string(parts[0])}, nil
	case "tool_started":
		if err := need(2); err != nil {
			return wireFrame{}, err
		}
		return wireFrame{Type: typ, Tool: string(parts[0]), Input: rawPart(parts[1])}, nil
	case "tool_finished":
		if err := need(2); err != nil {
			return wireFrame{}, err
		}
		return wireFrame{Type: typ, Tool: string(parts[0]), Output: rawPart(parts[1])}, nil
	case "usage":
		if err := need(3); err != nil {
			return wireFrame{}, err
		}
		in, err := strconv.ParseInt(string(parts[0]), 10, 64)
		if err != nil {
			return wireFrame{}, fmt.Errorf("compact usage input tokens are not a number")
		}
		out, err := strconv.ParseInt(string(parts[1]), 10, 64)
		if err != nil {
			return wireFrame{}, fmt.Errorf("compact usage output tokens are not a number")
		}
		cost, err := strconv.ParseFloat(string(parts[2]), 64)
		if err != nil {
			return wireFrame{}, fmt.Errorf("compact usage cost is not a number")
		}
		return wireFrame{Type: typ, InputTokens: in, OutputTokens: out, CostUSD: cost}, nil
	case "session_finished":
		if err := need(1); err != nil {
			return wireFrame{}, err
		}
		return wireFrame{Type: typ, Result: rawPart(parts[0])}, nil
	case "session_failed":
		if err := need(2); err != nil {
			return wireFrame{}, err
		}
		return wireFrame{Type: typ, Code: string(parts[0]), Message: string(parts[1])}, nil
	default:
		return wireFrame{}, fmt.Errorf("unknown compact frame type %q", typ)
	}
}

// rawPart wraps a compact value as raw JSON text (it may be a JSON
// literal or plain text).
func rawPart(part []byte) json.RawMessage {
	return json.RawMessage(part)
}

// wireToEvent converts one validated wire frame onto the unified Agent
// Event model. The wire terminal events map onto the unified completed
// and failed events; every event carries the sha256 of its raw frame (the
// protocol hash the Runtime persists).
func wireToEvent(w wireFrame, raw []byte) (agent.Event, error) {
	ev := agent.Event{
		SessionID:    agent.ProviderSessionID(w.SessionID),
		AtMillis:     w.AtMillis,
		Text:         w.Text,
		Tool:         w.Tool,
		Input:        string(w.Input),
		Output:       string(w.Output),
		InputTokens:  w.InputTokens,
		OutputTokens: w.OutputTokens,
		CostUSD:      w.CostUSD,
		Result:       string(w.Result),
		Code:         w.Code,
		Message:      w.Message,
	}
	switch w.Type {
	case "session_started":
		ev.Type = agent.EventSessionStarted
	case "assistant_delta":
		ev.Type = agent.EventAssistantDelta
	case "assistant_message":
		ev.Type = agent.EventAssistantMessage
	case "tool_started":
		ev.Type = agent.EventToolStarted
	case "tool_finished":
		ev.Type = agent.EventToolFinished
	case "usage":
		ev.Type = agent.EventUsage
	case "session_finished":
		ev.Type = agent.EventCompleted
	case "session_failed":
		ev.Type = agent.EventFailed
	default:
		return agent.Event{}, &agent.ProtocolError{
			Code:    model.CodeProviderProtocolViolation,
			Frame:   raw,
			Message: fmt.Sprintf("fake dialect: unknown event type %q", w.Type),
		}
	}
	ev.FrameHash = hashHex(raw)
	return ev, nil
}

// hashHex digests raw frame bytes.
func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
