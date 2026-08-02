// Package fake implements the deterministic Fake Adapter (design 14.1,
// 22.1): scripted runs with virtual timing, exit facts, crash points, and
// Resume outcomes declared as JSONL fixtures. No real process is
// involved; the Fake never needs the process Supervisor. It implements
// the stable agent.Adapter contract and can deterministically stop at
// every event boundary.
//
// Fixture format (cflow.dialect.fake.v1): one JSON object per line. The
// optional first line is the header declaring the fixture kind, purpose,
// session id, exit fact, resume outcome, crash point (crash_after), stop
// point (stop_after), and the seed hint for pre-existing sessions. The
// remaining lines are wire event frames, either full JSON objects
// ({"type":"session_started","session_id":"p1",...}) or the compact
// shorthand "type:value" / "type:a|b|c" used by the verbatim runtime
// tests. The wire terminal events session_finished and session_failed map
// onto the unified completed and failed events.
package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
)

// Frame bounds of the fixture decoder (design 14.3: bounded frame
// decoder). A line beyond the bound or a script beyond the frame count
// fails closed.
const (
	maxFrameBytes = 1 << 20
	maxFrameCount = 100000
)

// Script is one parsed Fake fixture. Frames are decoded lazily from the
// raw lines at drain time, so malformed frames surface as protocol
// violations at the event boundary exactly like a real byte stream.
type Script struct {
	Name       string
	Provider   string
	Dialect    string
	Purpose    model.AgentPurpose
	SessionID  string
	ExitCode   int
	Resume     string // "ok" | "not-found" | "unsupported" | "crashed"
	CrashAfter int    // 0 = never; stop the stream after this many frames
	StopAfter  int    // 0 = never; block at this boundary until Cancel
	Seed       bool   // the declared session pre-exists (resume target)

	rawLines [][]byte
}

// scriptHeader is the strict fixture header shape. Unknown fields fail
// the load.
type scriptHeader struct {
	Fixture       string `json:"fixture"`
	ScriptVersion int    `json:"script_version"`
	Provider      string `json:"provider"`
	Dialect       string `json:"dialect"`
	Purpose       string `json:"purpose"`
	SessionID     string `json:"session_id"`
	ExitCode      int    `json:"exit_code"`
	Resume        string `json:"resume"`
	CrashAfter    int    `json:"crash_after"`
	StopAfter     int    `json:"stop_after"`
	Seed          bool   `json:"seed"`
}

// wireFrame is the fake dialect wire shape (snake_case). The dialect
// parser converts it onto the unified Agent Event model.
type wireFrame struct {
	Type         string          `json:"type"`
	SessionID    string          `json:"session_id"`
	AtMillis     int64           `json:"at_ms"`
	Text         string          `json:"text"`
	Tool         string          `json:"tool"`
	Input        json.RawMessage `json:"input"`
	Output       json.RawMessage `json:"output"`
	InputTokens  int64           `json:"input_tokens"`
	OutputTokens int64           `json:"output_tokens"`
	CostUSD      float64         `json:"cost_usd"`
	Result       json.RawMessage `json:"result"`
	Code         string          `json:"code"`
	Message      string          `json:"message"`
}

// ParseScript parses one fixture text. The first line must be either a
// strict header (when it carries the fixture key) or a parseable frame;
// the frame lines are validated lazily at drain time.
func ParseScript(data []byte) (*Script, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("fake script is empty")
	}
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > maxFrameCount {
		return nil, fmt.Errorf("fake script exceeds the frame bound")
	}
	sc := &Script{}
	start := 0
	var probe map[string]any
	if err := json.Unmarshal(lines[0], &probe); err == nil {
		if _, isHeader := probe["fixture"]; isHeader {
			h, err := parseHeader(lines[0])
			if err != nil {
				return nil, err
			}
			sc.Name = "inline"
			sc.Provider = h.Provider
			sc.Dialect = h.Dialect
			sc.Purpose = model.AgentPurpose(h.Purpose)
			sc.SessionID = h.SessionID
			sc.ExitCode = h.ExitCode
			sc.Resume = h.Resume
			sc.CrashAfter = h.CrashAfter
			sc.StopAfter = h.StopAfter
			sc.Seed = h.Seed
			start = 1
		}
	} else {
		if len(bytes.TrimSpace(lines[0])) == 0 {
			return nil, fmt.Errorf("fake script is empty")
		}
		if _, _, err := parseFrameLine(lines[0]); err != nil {
			return nil, fmt.Errorf("fake script first line: %w", err)
		}
	}
	for _, line := range lines[start:] {
		if len(line) > maxFrameBytes {
			return nil, fmt.Errorf("fake script frame exceeds the bounded size")
		}
	}
	sc.rawLines = lines[start:]
	return sc, nil
}

// parseHeader strictly parses one fixture header line.
func parseHeader(line []byte) (scriptHeader, error) {
	var h scriptHeader
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return scriptHeader{}, fmt.Errorf("fake script header: %w", err)
	}
	if h.Fixture != "fake-run" {
		return scriptHeader{}, fmt.Errorf("fake script: unknown fixture kind %q", h.Fixture)
	}
	if h.ScriptVersion != 1 {
		return scriptHeader{}, fmt.Errorf("fake script: unsupported fixture version %d", h.ScriptVersion)
	}
	if h.Provider != "fake" {
		return scriptHeader{}, fmt.Errorf("fake script: unknown provider %q", h.Provider)
	}
	if !model.AgentPurpose(h.Purpose).Valid() {
		return scriptHeader{}, fmt.Errorf("fake script: unknown purpose %q", h.Purpose)
	}
	if h.SessionID == "" {
		return scriptHeader{}, fmt.Errorf("fake script: a header must declare the session id")
	}
	switch h.Resume {
	case "", "ok", "not-found", "unsupported", "crashed":
	default:
		return scriptHeader{}, fmt.Errorf("fake script: unknown resume outcome %q", h.Resume)
	}
	if h.CrashAfter < 0 || h.StopAfter < 0 {
		return scriptHeader{}, fmt.Errorf("fake script: crash and stop points must not be negative")
	}
	return h, nil
}

// Adapter is the deterministic Fake Adapter over one Provider Registry.
type Adapter struct {
	reg     *agent.ProviderRegistry
	binding agent.ProviderBinding

	mu      sync.Mutex
	scripts []Script
	runs    map[agent.ProviderSessionID]*run
}

// New constructs the Fake Adapter bound to the "fake" registry entry.
func New(reg *agent.ProviderRegistry) *Adapter {
	binding, _ := reg.Select("fake") // "fake" is always enabled in the embedded registry
	return &Adapter{
		reg:     reg,
		binding: binding,
		runs:    map[agent.ProviderSessionID]*run{},
	}
}

// LoadScript parses and registers one fixture text. The dialect must
// match the Fake binding.
func (a *Adapter) LoadScript(data []byte) error {
	sc, err := ParseScript(data)
	if err != nil {
		return err
	}
	if sc.Dialect != "" && sc.Dialect != a.binding.Dialect.ID {
		return fmt.Errorf("fake script dialect %q does not match the binding dialect %q", sc.Dialect, a.binding.Dialect.ID)
	}
	a.mu.Lock()
	a.scripts = append(a.scripts, *sc)
	a.mu.Unlock()
	return nil
}

// LoadDir registers every *.jsonl fixture in dir, sorted by name for
// deterministic selection.
func (a *Adapter) LoadDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		sc, err := ParseScript(data)
		if err != nil {
			return fmt.Errorf("fixture %s: %w", name, err)
		}
		if sc.Dialect != a.binding.Dialect.ID {
			return fmt.Errorf("fixture %s: dialect %q does not match the binding dialect %q", name, sc.Dialect, a.binding.Dialect.ID)
		}
		sc.Name = name
		a.mu.Lock()
		a.scripts = append(a.scripts, *sc)
		a.mu.Unlock()
	}
	return nil
}

// Scripts returns a copy of the loaded scripts.
func (a *Adapter) Scripts() []Script {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Script, 0, len(a.scripts))
	for _, s := range a.scripts {
		out = append(out, s)
	}
	return out
}

// selectScript picks the script bound to the purpose, falling back to the
// single loaded script (single-script fixture mode used by runtime tests).
func (a *Adapter) selectScript(purpose model.AgentPurpose) (*Script, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.scripts) == 0 {
		return nil, false
	}
	for i := range a.scripts {
		if a.scripts[i].Purpose == purpose {
			return &a.scripts[i], true
		}
	}
	if len(a.scripts) == 1 {
		return &a.scripts[0], true
	}
	return nil, false
}

// Detect reports SUPPORTED with the exact registry revision, dialect, and
// capabilities of the Fake binding (design 14.2).
func (a *Adapter) Detect(ctx context.Context) (agent.Installation, error) {
	if err := ctx.Err(); err != nil {
		return agent.Installation{}, err
	}
	return agent.Installation{
		Compatibility:    agent.CompatibilitySupported,
		RegistryRevision: a.reg.Revision(),
		DialectID:        a.binding.Dialect.ID,
		Capabilities:     capsFromBinding(a.binding),
	}, nil
}

// Start streams the script bound to the request's purpose.
func (a *Adapter) Start(ctx context.Context, req agent.StartRequest) (agent.Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sc, ok := a.selectScript(req.Purpose)
	if !ok {
		return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"fake adapter has no script for the requested purpose")
	}
	r := newRun(a, *sc, agent.ProviderSessionID(sc.SessionID))
	a.mu.Lock()
	a.runs[r.key] = r
	a.mu.Unlock()
	return r, nil
}

// Resume replays the script's declared Resume outcome: "ok" streams the
// script re-affirming the resumed session; "not-found" and "crashed" are
// unrecoverable provider failures (they trigger the Runtime's fallback);
// "unsupported" blocks with PROVIDER_PROTOCOL_UNSUPPORTED and never
// falls back.
func (a *Adapter) Resume(ctx context.Context, req agent.ResumeRequest) (agent.Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sc, ok := a.selectScript(req.Purpose)
	if !ok {
		return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"fake adapter has no script for the requested purpose")
	}
	switch sc.Resume {
	case "", "ok":
		r := newRun(a, *sc, req.ProviderSessionID)
		a.mu.Lock()
		a.runs[r.key] = r
		a.mu.Unlock()
		return r, nil
	case "not-found":
		return nil, fmt.Errorf("fake: provider session %q is not resumable", req.ProviderSessionID)
	case "crashed":
		return nil, &agent.ProcessCrash{
			ExitCode: sc.ExitCode,
			Message:  "fake: resumed provider process crashed",
		}
	case "unsupported":
		return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"fake: resume is not supported for this session")
	}
	return nil, model.InvalidInputFault("fake: unknown resume outcome")
}

// Cancel performs the controlled stop of one live run: the run stops at
// the next event boundary (deterministic per-boundary stop). A run that
// already ended is a no-op.
func (a *Adapter) Cancel(ctx context.Context, handle agent.RunHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	r := a.runs[handle.ProviderSessionID]
	a.mu.Unlock()
	if r == nil {
		return nil
	}
	r.stop()
	return nil
}

// Inspect reports the live truth about one Fake run.
func (a *Adapter) Inspect(ctx context.Context, id agent.ProviderSessionID) (agent.SessionFact, error) {
	if err := ctx.Err(); err != nil {
		return agent.SessionFact{}, err
	}
	a.mu.Lock()
	r := a.runs[id]
	a.mu.Unlock()
	fact := agent.SessionFact{
		Session:  model.Session{ProviderSessionID: string(id)},
		Provider: "fake",
	}
	if r != nil {
		fact.Running = true
		fact.Handle = agent.RunHandle{ProviderSessionID: id}
	}
	return fact, nil
}

// capsFromBinding derives the installation capabilities from the Fake
// binding's Start and Resume capability lists.
func capsFromBinding(b agent.ProviderBinding) agent.Capabilities {
	has := func(list []string, want string) bool {
		for _, have := range list {
			if have == want {
				return true
			}
		}
		return false
	}
	return agent.Capabilities{
		StructuredEvents:               has(b.StartCapabilities, "structured_output"),
		ResumableSession:               has(b.ResumeCapabilities, "resume_by_session_id"),
		SessionIDInEventStream:         has(b.StartCapabilities, "session_id_on_start"),
		NativeInteractiveResume:        has(b.ResumeCapabilities, "in_process"),
		StructuredOutputSchemaOnStart:  has(b.StartCapabilities, "structured_output"),
		StructuredOutputSchemaOnResume: has(b.ResumeCapabilities, "structured_output"),
		BudgetLimit:                    false, // the binding declares no native budget
	}
}
