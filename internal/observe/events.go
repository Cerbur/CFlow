package observe

// The events.jsonl audit export (design 21): one redacted, bounded
// structured record per authoritative Event, written atomically with
// owner-only mode. The export is generated from the SQLite Event sequence
// and can always be rebuilt; it is never read by Recovery.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// EventLine is one redacted export record: stable Codes, domain IDs, and
// immutable references only (design 21). Human text is derived from the
// Code and may evolve without changing recovery semantics.
type EventLine struct {
	Seq      uint64 `json:"seq"`
	Kind     string `json:"kind"`
	Workflow string `json:"workflow,omitempty"`
	Node     string `json:"node,omitempty"`
	Attempt  string `json:"attempt,omitempty"`
	Code     string `json:"code,omitempty"`
	Text     string `json:"text,omitempty"`
	At       string `json:"at"`
}

// ExportEvents appends one committed Event window to the workflow's
// events.jsonl export: the complete existing content is re-emitted with
// the new lines and renamed into place atomically with 0600 (PRD Export:
// 0600 and atomic writes). Every free-form text value passes through the
// Redactor before persistence (design 19.2); a frame that cannot be
// redacted fails closed and nothing is written.
func ExportEvents(path string, events []model.Event, reg security.Registry) error {
	if len(events) == 0 {
		return nil
	}
	red := security.NewRedactor(reg)
	var sb strings.Builder
	for _, e := range events {
		line := EventLine{
			Seq:      e.Seq,
			Kind:     string(e.Kind),
			Workflow: string(e.Workflow),
			Node:     string(e.Node),
			Code:     string(e.Code),
			At:       e.At.UTC().Format(time.RFC3339Nano),
		}
		if e.Attempt.Node != "" {
			line.Attempt = e.Attempt.String()
		}
		if e.Text != "" {
			frame, err := red.WriteFrame([]byte(e.Text))
			if err != nil {
				return err
			}
			flushed, err := red.Flush()
			if err != nil {
				return err
			}
			line.Text = frame.Text + flushed.Text
		}
		body, err := json.Marshal(line)
		if err != nil {
			return fmt.Errorf("encode event export: %w", err)
		}
		sb.Write(body)
		sb.WriteByte('\n')
	}
	return appendAtomic(path, sb.String())
}

// appendAtomic re-emits the existing export content plus one segment
// through a same-directory temporary file renamed into place. A missing
// export file is an empty prefix.
func appendAtomic(path string, segment string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read event export: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".events-*.tmp")
	if err != nil {
		return fmt.Errorf("event export temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("event export mode: %w", err)
	}
	if _, err := tmp.Write(existing); err != nil {
		tmp.Close()
		return fmt.Errorf("event export write: %w", err)
	}
	if _, err := tmp.WriteString(segment); err != nil {
		tmp.Close()
		return fmt.Errorf("event export write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("event export sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("event export close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("event export rename: %w", err)
	}
	return nil
}
