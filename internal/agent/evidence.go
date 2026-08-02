// Persistent evidence (design 14.3): the Runtime persists only redacted
// complete events plus protocol/prompt/input hashes. The evidence writer
// owns the managed layout under the EvidenceDir and uses the Task 3 path
// guard primitives with the same atomic owner-only discipline as the Task
// 5 Artifact Store: every file is born 0600 via O_CREATE|O_EXCL, existing
// paths are never reused, and a Session manifest or Context Bundle
// Revision is never rewritten in place.
//
// Layout:
//
//	<dir>/sessions/<cflow-session>/manifest.json    final session manifest
//	<dir>/events/<cflow-session>.jsonl              redacted complete events
//	<dir>/bundles/<cflow-session>/rev-<n>.json      immutable Context Bundles
//	<dir>/violations.jsonl                          redacted protocol evidence
//
// All paths are keyed by the CFlow-generated opaque Session id, never by
// Provider-controlled strings.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// bundleSchemaVersion is the Context Bundle manifest schema version.
const bundleSchemaVersion = "1"

// persistedEvent is one redacted complete event line.
type persistedEvent struct {
	Seq               uint64            `json:"seq"`
	Type              EventType         `json:"type"`
	SessionID         ProviderSessionID `json:"session_id"`
	AtMillis          int64             `json:"at_ms"`
	Text              string            `json:"text,omitempty"`
	Tool              string            `json:"tool,omitempty"`
	Input             string            `json:"input,omitempty"`
	Output            string            `json:"output,omitempty"`
	InputTokens       int64             `json:"input_tokens,omitempty"`
	OutputTokens      int64             `json:"output_tokens,omitempty"`
	CostUSD           float64           `json:"cost_usd,omitempty"`
	Result            string            `json:"result,omitempty"`
	Code              string            `json:"code,omitempty"`
	Message           string            `json:"message,omitempty"`
	FrameHash         string            `json:"frame_hash"`
	RedactionRevision string            `json:"redaction_revision"`
}

// persistedViolation is one redacted protocol evidence line (PRD
// 已确认：未知 Provider CLI 协议 Fail-closed item 5: the frame boundary and
// redacted raw evidence are saved).
type persistedViolation struct {
	At            time.Time         `json:"at"`
	SessionID     ProviderSessionID `json:"session_id,omitempty"`
	Code          model.Code        `json:"code"`
	FrameHash     string            `json:"frame_hash,omitempty"`
	RedactedFrame string            `json:"redacted_frame,omitempty"`
	Message       string            `json:"message"`
}

// persistedEventRef is the frame-hash record of one complete event inside
// the session manifest.
type persistedEventRef struct {
	Seq       uint64    `json:"seq"`
	Type      EventType `json:"type"`
	FrameHash string    `json:"frame_hash"`
}

// persistedFinding is one protocol finding recorded on the manifest.
type persistedFinding struct {
	Code      model.Code `json:"code"`
	FrameHash string     `json:"frame_hash,omitempty"`
	Text      string     `json:"text"`
}

// persistedBundleRef names the Context Bundle Revision a Lost Session's
// manifest carries (revision and hash only: the path differs per CFLOW_HOME
// and must never enter a manifest digest).
type persistedBundleRef struct {
	Revision int    `json:"revision"`
	Hash     string `json:"sha256"`
}

// persistedSession is the canonical final Session manifest. ManifestHash
// is the sha256 of the canonical serialization excluding its own field,
// so consumers can verify the persisted record byte-for-byte.
type persistedSession struct {
	SessionID         string              `json:"session_id"`
	Provider          string              `json:"provider"`
	ProviderSessionID string              `json:"provider_session_id"`
	Purpose           model.AgentPurpose  `json:"purpose"`
	Status            model.SessionStatus `json:"status"`
	Supersedes        string              `json:"supersedes_session_id,omitempty"`
	StartedAt         time.Time           `json:"started_at"`
	EndedAt           time.Time           `json:"ended_at"`
	ExitCode          int                 `json:"exit_code"`
	PromptHash        string              `json:"prompt_hash"`
	InputHash         string              `json:"input_hash"`
	RedactionRevision string              `json:"redaction_revision"`
	Events            []persistedEventRef `json:"events"`
	Findings          []persistedFinding  `json:"findings,omitempty"`
	ContextBundle     *persistedBundleRef `json:"context_bundle,omitempty"`
	ManifestHash      string              `json:"manifest_hash"`
}

// persistedPin is one artifact revision pin inside a Context Bundle.
type persistedPin struct {
	Type     string `json:"type"`
	Revision int    `json:"revision"`
	Hash     string `json:"sha256"`
}

// persistedEvidence is one failure evidence reference inside a bundle.
type persistedEvidence struct {
	Kind    string `json:"kind"`
	Hash    string `json:"sha256"`
	Subject string `json:"subject,omitempty"`
}

// persistedBundle is the canonical immutable Context Bundle manifest.
// Hash is excluded from its own digest, like every registry entry.
type persistedBundle struct {
	SchemaVersion      string              `json:"schema_version"`
	Revision           int                 `json:"revision"`
	Hash               string              `json:"sha256"`
	SessionID          string              `json:"session_id"`
	ProviderSessionID  string              `json:"provider_session_id"`
	Purpose            model.AgentPurpose  `json:"purpose"`
	CreatedAt          time.Time           `json:"created_at"`
	Requirement        string              `json:"requirement"`
	Plan               *persistedPin       `json:"plan,omitempty"`
	Spec               *persistedPin       `json:"spec,omitempty"`
	Catalog            *persistedPin       `json:"catalog,omitempty"`
	Workflow           *persistedPin       `json:"workflow,omitempty"`
	RepositoryBaseline string              `json:"repository_baseline,omitempty"`
	StageSummary       string              `json:"stage_summary,omitempty"`
	Decisions          []string            `json:"decisions,omitempty"`
	FailureEvidence    []persistedEvidence `json:"failure_evidence,omitempty"`
	OpenQuestions      []string            `json:"open_questions,omitempty"`
	PermissionBoundary string              `json:"permission_boundary"`
	RedactionRevision  string              `json:"redaction_revision"`
}

// evidenceWriter owns the managed evidence layout. All writes go through
// the Security Guard primitives; a guard failure fails the runtime closed.
type evidenceWriter struct {
	dir       string
	redaction security.Registry

	mu       sync.Mutex
	sessions map[model.SessionID]*os.File // open events files
}

// newEvidenceWriter validates the EvidenceDir through the guard and
// creates the managed subdirectories (0700) that do not exist.
func newEvidenceWriter(dir string, redaction security.Registry) (*evidenceWriter, error) {
	if dir == "" {
		return nil, nil
	}
	w := &evidenceWriter{dir: dir, redaction: redaction, sessions: map[model.SessionID]*os.File{}}
	for _, sub := range []string{"sessions", "events", "bundles"} {
		if err := ensureSensitiveDir(filepath.Join(dir, sub)); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// root reports the managed root.
func (w *evidenceWriter) root() string { return w.dir }

// ensureSensitiveDir validates an existing managed directory or creates
// it 0700 through the guard; an existing path is never reused.
func ensureSensitiveDir(path string) error {
	if _, err := os.Lstat(path); err == nil {
		_, err := security.CheckPath(security.PathRequest{Path: path, Kind: security.KindDir})
		return err
	} else if !os.IsNotExist(err) {
		return err
	}
	return security.CreateSensitiveDir(path)
}

// appendEvent persists one redacted complete event line for a Session,
// creating the events file 0600 on first use.
func (w *evidenceWriter) appendEvent(ctx context.Context, session model.SessionID, line persistedEvent) error {
	if w == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("evidence: event cannot be serialized: %w", err)
	}
	data = append(data, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	f := w.sessions[session]
	if f == nil {
		if err := ensureSensitiveDir(filepath.Join(w.dir, "events")); err != nil {
			return err
		}
		path := filepath.Join(w.dir, "events", string(session)+".jsonl")
		f, err = security.CreateSensitiveFile(path)
		if err != nil {
			return err
		}
		w.sessions[session] = f
	}
	if err := writeAll(f, data); err != nil {
		return fmt.Errorf("evidence: event cannot be written: %w", err)
	}
	return nil
}

// closeSession flushes and closes one Session's events file.
func (w *evidenceWriter) closeSession(session model.SessionID) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	f := w.sessions[session]
	if f == nil {
		return nil
	}
	delete(w.sessions, session)
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("evidence: events cannot be flushed: %w", err)
	}
	return f.Close()
}

// appendViolation persists one redacted protocol evidence line: the
// violations file is created 0600 on first use and appended to
// afterwards, always through the Security Guard.
func (w *evidenceWriter) appendViolation(line persistedViolation) error {
	if w == nil {
		return nil
	}
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("evidence: violation cannot be serialized: %w", err)
	}
	data = append(data, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	path := filepath.Join(w.dir, "violations.jsonl")
	var f *os.File
	if _, err := os.Lstat(path); err == nil {
		if _, err := security.CheckPath(security.PathRequest{Path: path, Kind: security.KindFile}); err != nil {
			return err
		}
		f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("evidence: violations cannot be opened: %w", err)
		}
	} else if os.IsNotExist(err) {
		f, err = security.CreateSensitiveFile(path)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("evidence: violations path cannot be inspected: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("evidence: violation cannot be written: %w", err)
	}
	return nil
}

// writeManifest persists the canonical Session manifest. The first final
// transition writes manifest.json; later transitions (e.g. a completed
// Session later retained as LOST) append manifest-lost-<n>.json so no
// manifest is ever rewritten in place.
func (w *evidenceWriter) writeManifest(session model.SessionID, m persistedSession) (string, error) {
	if w == nil {
		return "", nil
	}
	digest := hashBytes(marshalCanonical(m))
	m.ManifestHash = digest
	data := marshalCanonical(m)
	dir := filepath.Join(w.dir, "sessions", string(session))
	if err := ensureSensitiveDir(dir); err != nil {
		return "", err
	}
	name := "manifest.json"
	for i := 1; ; i++ {
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			if err := writeSensitiveFile(path, data); err != nil {
				return "", err
			}
			return digest, nil
		}
		name = fmt.Sprintf("manifest-lost-%d.json", i)
	}
}

// writeBundle persists one immutable Context Bundle Revision and returns
// its revision, hash, and absolute path. The revision is the next free
// number after the persisted Revisions; an existing path is never
// rewritten (never modify in place).
func (w *evidenceWriter) writeBundle(session model.SessionID, b persistedBundle) (int, string, string, error) {
	if w == nil {
		return 0, "", "", fmt.Errorf("evidence: no evidence root configured")
	}
	digest := hashBytes(marshalCanonical(b))
	b.Hash = digest
	data := marshalCanonical(b)
	dir := filepath.Join(w.dir, "bundles", string(session))
	if err := ensureSensitiveDir(dir); err != nil {
		return 0, "", "", err
	}
	revision := 1
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, "", "", err
	}
	for _, e := range entries {
		if n, ok := parseRevName(e.Name()); ok && n >= revision {
			revision = n + 1
		}
	}
	path := filepath.Join(dir, fmt.Sprintf("rev-%d.json", revision))
	if err := writeSensitiveFile(path, data); err != nil {
		return 0, "", "", err
	}
	return revision, digest, path, nil
}

// writeSensitiveFile writes one new file born 0600 via O_CREATE|O_EXCL,
// flushes it, and closes it. An existing path fails closed.
func writeSensitiveFile(path string, data []byte) error {
	f, err := security.CreateSensitiveFile(path)
	if err != nil {
		return err
	}
	if err := writeAll(f, data); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("evidence: file cannot be written")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("evidence: file cannot be flushed")
	}
	return f.Close()
}

// writeAll writes every byte, retrying short writes.
func writeAll(f *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := f.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// parseRevName parses rev-<n>.json bundle names.
func parseRevName(name string) (int, bool) {
	if !strings.HasPrefix(name, "rev-") || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "rev-"), ".json"))
	if err != nil {
		return 0, false
	}
	return n, true
}

// marshalCanonical canonicalizes one persisted value for digesting or
// writing. The registered structs always marshal; an error here is a
// build bug.
func marshalCanonical(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("agent: evidence value cannot be serialized: %v", err))
	}
	return data
}

// hashBytes digests canonical serialized bytes (sha256Hex lives in
// registry.go: every registry and manifest digest shares one helper).
func hashBytes(data []byte) string {
	return sha256Hex(data)
}
