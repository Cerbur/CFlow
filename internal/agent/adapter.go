// Agent Adapter seam (design 14.1) and the unified Agent Event model
// (PRD 统一事件模型). The Adapter interface is the stable ledger contract:
// every Provider adapter (Fake, Codex, Claude, OpenCode P1) implements it
// and nothing else may touch a Provider process. Adapters map their
// Provider's wire dialect onto the unified events below; the Runtime
// validates the protocol sequence, redacts, renders, and persists.
package agent

import (
	"context"
	"fmt"

	"time"

	"cflow.local/cflow/internal/model"
)

// Purpose constants name the six Runtime role lineages of design 14.4 in
// the canonical model vocabulary (CONTEXT.md: Agent Purpose). Planner and
// Plan Checker may use the same Provider but never the same Session;
// Implementer, Repairer, Task Reviewer, and Final Reviewer are likewise
// independent: a successor Session must keep the superseded Session's
// purpose (SESSION_INDEPENDENCE_VIOLATION otherwise).
const (
	PurposePlanner       = model.PurposePlanning
	PurposePlanChecker   = model.PurposePlanCheck
	PurposeImplementer   = model.PurposeImplementation
	PurposeRepairer      = model.PurposeRepair
	PurposeTaskReviewer  = model.PurposeReview
	PurposeFinalReviewer = model.PurposeFinalVerification
)

// ProviderSessionID is the Provider-managed conversation identity carried
// by the validated session_started event. Session identity is a protocol
// fact, never stdout prose or an exit code (design 14.3).
type ProviderSessionID string

// ProtocolCompatibility is the closed result of Detect (PRD 已确认：未知
// Provider CLI 协议 Fail-closed). Only SUPPORTED may start or resume.
type ProtocolCompatibility string

const (
	// CompatibilityMissing: the executable is not installed.
	CompatibilityMissing ProtocolCompatibility = "MISSING"
	// CompatibilitySupported: the executable, version, dialect, and
	// capabilities match the registry binding.
	CompatibilitySupported ProtocolCompatibility = "SUPPORTED"
	// CompatibilityUnknownVersion: the executable exists but its version is
	// outside the binding's supported range.
	CompatibilityUnknownVersion ProtocolCompatibility = "UNKNOWN_VERSION"
	// CompatibilityIncompatibleProtocol: the executable exists but its
	// protocol cannot be proven compatible with the binding.
	CompatibilityIncompatibleProtocol ProtocolCompatibility = "INCOMPATIBLE_PROTOCOL"
)

// Valid reports whether c is a declared compatibility.
func (c ProtocolCompatibility) Valid() bool {
	switch c {
	case CompatibilityMissing, CompatibilitySupported, CompatibilityUnknownVersion, CompatibilityIncompatibleProtocol:
		return true
	}
	return false
}

// String renders the compatibility.
func (c ProtocolCompatibility) String() string { return string(c) }

// Capabilities is the per-operation capability set of one Provider
// (PRD Agent Adapter). Structured-output and resume capabilities are
// never inferred across operations: Start and Resume are checked
// separately against the binding.
type Capabilities struct {
	StructuredEvents               bool
	ResumableSession               bool
	SessionIDInEventStream         bool
	NativeInteractiveResume        bool
	StructuredOutputSchemaOnStart  bool
	StructuredOutputSchemaOnResume bool
	BudgetLimit                    bool
}

// Installation is the verified result of Detect: the executable facts and
// the exact registry revision, dialect, and capabilities it was judged
// against (PRD Agent Adapter). Only facts, never claims.
type Installation struct {
	Compatibility    ProtocolCompatibility
	ExecutablePath   string
	ExecutableSHA256 string
	CLIVersion       string
	RegistryRevision string
	DialectID        string
	Capabilities     Capabilities
}

// StartRequest starts a brand-new Provider Session for one purpose. The
// Runtime records the prompt and structured input hashes, so a later
// prompt update can never change the meaning of an existing Session
// (design 14.5). Supersedes names the provider session id of the session
// this run succeeds; the Runtime verifies the role lineage before any
// Provider call (design 14.4).
type StartRequest struct {
	Purpose    model.AgentPurpose
	Provider   string
	Prompt     string
	Input      any
	CWD        string
	Timeout    time.Duration // zero: provider default
	Supersedes ProviderSessionID
}

// ResumeRequest re-establishes an existing Provider Session. Context is
// the fallback Context Bundle input used only when native Resume fails
// (design 14.4, PRD 已确认：Session Resume 失败与跨 Provider 上下文交接).
type ResumeRequest struct {
	ProviderSessionID ProviderSessionID
	Purpose           model.AgentPurpose
	Provider          string
	Prompt            string
	Input             any
	CWD               string
	Timeout           time.Duration
	Context           ContextInput
}

// RunHandle names one live run for Cancel and Inspect. The Runtime
// allocates it; providers never synthesize handles.
type RunHandle struct {
	RunID             model.RunID
	Session           model.SessionID
	ProviderSessionID ProviderSessionID
}

// SessionFact is the observable truth about one Session: the canonical
// model.Session record, the Provider it runs on, the live run handle while
// a run is draining, and the hash of the persisted session manifest.
type SessionFact struct {
	Session      model.Session
	Provider     string
	Running      bool
	Handle       RunHandle
	ManifestHash string
}

// Adapter is the stable Agent Adapter seam (design 14.1, ledger):
// detection, scripted or real start/resume event streams, two-phase
// cancel, and session inspection. The interface is fixed; new Providers
// implement it, they never alter it.
type Adapter interface {
	Detect(context.Context) (Installation, error)
	Start(context.Context, StartRequest) (Run, error)
	Resume(context.Context, ResumeRequest) (Run, error)
	Cancel(context.Context, RunHandle) error
	Inspect(context.Context, ProviderSessionID) (SessionFact, error)
}

// Run is one Provider event stream (design 14.1). Next returns the next
// unified event; io.EOF ends the stream. Errors from Next are one of:
// *ProtocolError (dialect/protocol violation), *ProcessCrash (the process
// ended before its terminal event), a model.Fault, or the context error.
type Run interface {
	Next(context.Context) (Event, error)
}

// ---------------------------------------------------------------------------
// Unified Agent Events (PRD 统一事件模型)
// ---------------------------------------------------------------------------

// EventType is the closed set of unified Agent Events. Every Provider
// dialect maps onto it; the Runtime sequence validator rejects anything
// outside it.
type EventType string

const (
	EventSessionStarted   EventType = "session_started"
	EventAssistantDelta   EventType = "assistant_delta"
	EventAssistantMessage EventType = "assistant_message"
	EventToolStarted      EventType = "tool_started"
	EventToolFinished     EventType = "tool_finished"
	EventUsage            EventType = "usage"
	EventCompleted        EventType = "completed"
	EventFailed           EventType = "failed"
)

// Valid reports whether t is a declared unified event type.
func (t EventType) Valid() bool {
	switch t {
	case EventSessionStarted, EventAssistantDelta, EventAssistantMessage,
		EventToolStarted, EventToolFinished, EventUsage, EventCompleted, EventFailed:
		return true
	}
	return false
}

// String renders the event type.
func (t EventType) String() string { return string(t) }

// Event is one unified Agent Event. SessionID carries the validated
// provider session id; Seq is the protocol sequence assigned by the
// Runtime validator; AtMillis is the Provider's virtual timing fact.
// Input, Output, and Result are the raw JSON text of the wire payloads.
// FrameHash is the sha256 of the raw wire frame that produced the event
// (the protocol hash the Runtime persists).
type Event struct {
	Type         EventType
	SessionID    ProviderSessionID
	Seq          uint64
	AtMillis     int64
	Text         string
	Tool         string
	Input        string
	Output       string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Result       string
	Code         string
	Message      string
	FrameHash    string
}

// Render renders one redacted unified event for the terminal (design
// 14.3: terminal renderer sink). Callers must only ever render redacted
// events; the Runtime returns redacted events only.
func Render(ev Event) string {
	switch ev.Type {
	case EventSessionStarted:
		return fmt.Sprintf("[%06d] session started %s", ev.Seq, ev.SessionID)
	case EventAssistantDelta, EventAssistantMessage:
		return fmt.Sprintf("[%06d] assistant: %s", ev.Seq, ev.Text)
	case EventToolStarted:
		return fmt.Sprintf("[%06d] tool %s: %s", ev.Seq, ev.Tool, ev.Input)
	case EventToolFinished:
		return fmt.Sprintf("[%06d] tool %s -> %s", ev.Seq, ev.Tool, ev.Output)
	case EventUsage:
		return fmt.Sprintf("[%06d] usage: in=%d out=%d cost=$%.6f", ev.Seq, ev.InputTokens, ev.OutputTokens, ev.CostUSD)
	case EventCompleted:
		return fmt.Sprintf("[%06d] completed: %s", ev.Seq, ev.Result)
	case EventFailed:
		return fmt.Sprintf("[%06d] failed: %s %s", ev.Seq, ev.Code, ev.Message)
	}
	return fmt.Sprintf("[%06d] %s", ev.Seq, ev.Type)
}

// ---------------------------------------------------------------------------
// Fail-closed protocol failures (design 14.3)
// ---------------------------------------------------------------------------

// ProtocolError is a fail-closed dialect or protocol failure raised by an
// Adapter (unparseable stream, unknown wire event) or by the Runtime
// sequence validator (conflicting or missing Session ids, missing start
// event, malformed terminal frame, invalid completion payload). Frame
// carries the raw wire bytes for redacted evidence; it is never persisted
// unredacted.
type ProtocolError struct {
	Code      model.Code
	SessionID ProviderSessionID
	Frame     []byte
	Message   string
}

// Error renders the protocol failure.
func (e *ProtocolError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ProcessCrash describes an adapter process that ended before its
// terminal event (design 14.3): the exit code is a fact and can never
// complete a run (PRD 约束 43).
type ProcessCrash struct {
	ExitCode int
	Message  string
}

// Error renders the crash.
func (e *ProcessCrash) Error() string {
	return fmt.Sprintf("agent process crashed before a terminal event (exit code %d): %s", e.ExitCode, e.Message)
}

// ---------------------------------------------------------------------------
// Context Bundles (design 14.4, PRD 已确认：Session Resume 失败与跨 Provider
// 上下文交接)
// ---------------------------------------------------------------------------

// ArtifactPin pins one active Artifact Revision by type, revision, and
// content hash, so a Context Bundle never names meaning by display name.
type ArtifactPin struct {
	Type     string
	Revision int
	Hash     string
}

// ContextInput is the minimum Context Bundle content list: the
// requirement, the active Plan/Spec/Catalog/Workflow Revision+hash, the
// repository baseline, the stage summary, the confirmed decisions, the
// relevant failure evidence, the open questions, and the permission
// boundary of the target Provider.
type ContextInput struct {
	Requirement        string
	Plan               ArtifactPin
	Spec               ArtifactPin
	Catalog            ArtifactPin
	Workflow           ArtifactPin
	RepositoryBaseline string
	StageSummary       string
	Decisions          []string
	FailureEvidence    []model.EvidenceRef
	OpenQuestions      []string
	PermissionBoundary string
}

// ContextBundleRequest names the Session a bundle documents and its
// content. The Session must exist in the Runtime ledger; the bundle is
// redacted, hashed, and persisted as a new immutable Revision.
type ContextBundleRequest struct {
	SessionID         model.SessionID
	ProviderSessionID ProviderSessionID
	Purpose           model.AgentPurpose
	Context           ContextInput
}

// ContextBundle is one immutable, versioned, redacted context handoff
// (design 14.4). Hash is the sha256 of the canonical manifest excluding
// its own hash; Path is the persisted Revision file. A bundle documents
// auditable handoff; it never claims the original model's hidden state.
type ContextBundle struct {
	SchemaVersion     string
	Revision          int
	Hash              string
	Path              string
	SessionID         model.SessionID
	ProviderSessionID ProviderSessionID
	Purpose           model.AgentPurpose
	CreatedAt         time.Time
	Context           ContextInput
	RedactionRevision string
}

// RunResult is the terminal result of one Runtime Start or Resume: the
// validated redacted events, the Session record, the structured terminal
// event, the persisted manifest hash, and the recorded prompt/input
// hashes. Agent prose and exit codes never appear as completion here; a
// terminal unified event does.
type RunResult struct {
	RunID        model.RunID
	Session      model.Session
	Provider     string
	Status       model.RunStatus
	Events       []Event
	Terminal     *Event
	ExitCode     int
	PromptHash   string
	InputHash    string
	ManifestHash string
	StartedAt    time.Time
	EndedAt      time.Time
}

// FallbackResult is the outcome of an unrecoverable Resume (design 14.4):
// the original Session retained as LOST, the immutable redacted Context
// Bundle, and the successor Session carrying supersedes_session_id. The
// Decision Kernel charges any Retry budget from these facts; the Runtime
// never charges.
type FallbackResult struct {
	LostSession      model.Session
	ContextBundle    ContextBundle
	SuccessorSession model.Session
}

// ResumeResult is the outcome of one Runtime Resume: a successful native
// Resume returns Session and Run; an unrecoverable Resume returns the
// Fallback and no Run.
type ResumeResult struct {
	Session  model.Session
	Run      *RunResult
	Fallback *FallbackResult
}
