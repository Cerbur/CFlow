// Package codex implements the Codex Adapter (design 14.1): the
// fail-closed binding between the cflow.dialect.codex-jsonl.v1 wire
// dialect and the unified Agent Event contract. The adapter launches the
// codex CLI through the process Supervisor argv-only (never a shell),
// feeds the prompt through stdin, passes the managed immutable schema
// file through --output-schema, sets the working directory through -C,
// and drives the bounded JSONL frame pipeline onto the unified events
// the Runtime validates, redacts, and persists. Detection runs only the
// read-only version probe (`codex --version`, help/version only); a paid
// model request is never started by the adapter.
package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// detectTimeout bounds the read-only version probe so detection can
// never hang on a stalled binary.
const detectTimeout = 15 * time.Second

// maxProbeBytes bounds the version probe output; a version line is tiny,
// anything beyond the bound is not the version probe's contract.
const maxProbeBytes = 1 << 20

// requiredStartCaps and requiredResumeCaps mirror the Runtime's
// per-operation capability gates (design 14.2): the adapter proves its
// binding before any process launch, exactly as the Runtime does for
// every route (capability is per operation, never inferred).
var requiredStartCaps = []string{"session_id_on_start", "structured_output"}

var requiredResumeCaps = []string{"resume_by_session_id", "structured_output"}

// StartRequest is the typed, registry-approved Start argv request (brief
// acceptance). StartArgv builds argv from these fields only, so no
// untyped input can inject flags: no danger, bypass, or ignore flag can
// ever be added through this constructor.
type StartRequest struct {
	SchemaPath string // managed immutable schema file (absolute)
	Worktree   string // absolute working directory (-C)
	Model      string // approved optional model ("" = provider default)
	Prompt     string // fed through stdin
}

// ResumeRequest is the typed Resume argv request. The captured 0.141.0
// resume help accepts no -C/--cd, so the working directory travels as
// the supervised process cwd, never as an argv flag.
type ResumeRequest struct {
	SchemaPath string
	SessionID  agent.ProviderSessionID
	Prompt     string
}

// Input carries the codex-specific typed facts for one Start or Resume:
// the managed immutable schema file and the approved optional model. The
// Application materializes the purpose's embedded output schema into a
// managed file (design 14.5) and passes it here; the adapter refuses to
// launch without a proven schema path, because the argv contract always
// carries --output-schema and an unproven schema cannot promise the
// structured terminal event the pipeline requires (PRD 约束 43).
//
// ContextBundleRef names the immutable redacted Context Bundle Revision
// of the superseded LOST Session an automatic fallback successor carries
// (Task 18; design 14.4). It is a reference only — never a credential or
// an unredacted transcript — and a fresh Session's typed input omits it.
type Input struct {
	SchemaPath string `json:"schema_path"`
	Model      string `json:"model,omitempty"`
	// ContextBundleRef is the persisted bundle path of the handoff ("" for
	// a brand-new Session).
	ContextBundleRef string `json:"context_bundle_ref,omitempty"`
}

// StartArgv builds the exact Start argv (brief acceptance): exec --json
// --output-schema <file> -C <worktree> with the approved optional
// --model, and - for stdin.
func StartArgv(req StartRequest) []string {
	argv := []string{"exec", "--json", "--output-schema", req.SchemaPath, "-C", req.Worktree}
	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	return append(argv, "-")
}

// ResumeArgv builds the exact Resume argv: exec resume --json
// --output-schema <file> <session-id> -.
func ResumeArgv(req ResumeRequest) []string {
	return []string{"exec", "resume", "--json", "--output-schema", req.SchemaPath, string(req.SessionID), "-"}
}

// Adapter is the Codex Adapter over one immutable Registry binding
// (design 14.2): detection, argv construction, and capability gates all
// judge against this binding and nothing else.
type Adapter struct {
	sup     process.Supervisor
	binding agent.ProviderBinding

	mu   sync.Mutex
	runs map[agent.ProviderSessionID]*run
}

// New constructs the Codex Adapter bound to the exact Registry binding.
func New(sup process.Supervisor, binding agent.ProviderBinding) agent.Adapter {
	return &Adapter{sup: sup, binding: binding, runs: map[agent.ProviderSessionID]*run{}}
}

// ---------------------------------------------------------------------------
// Detection (PRD 已确认：未知 Provider CLI 协议 Fail-closed)
// ---------------------------------------------------------------------------

// Detect runs the read-only version probe (`codex --version`, never a
// model request), hashes the resolved executable, and returns
// MISSING/SUPPORTED/UNKNOWN_VERSION/INCOMPATIBLE_PROTOCOL against the
// binding's supported range and dialect. Only facts are reported: the
// executable path and sha256, the parsed CLI version, the binding
// revision, dialect, and capabilities.
func (a *Adapter) Detect(ctx context.Context) (agent.Installation, error) {
	if err := ctx.Err(); err != nil {
		return agent.Installation{}, err
	}
	inst := agent.Installation{
		Compatibility:    agent.CompatibilityMissing,
		RegistryRevision: a.binding.Revision,
		DialectID:        a.binding.Dialect.ID,
		Capabilities:     capsFromBinding(a.binding),
	}
	path, err := exec.LookPath(a.binding.Executable.Name)
	if err != nil {
		return inst, nil // MISSING: no executable, no identity facts
	}
	hash, err := hashFile(path)
	if err != nil {
		inst.Compatibility = agent.CompatibilityIncompatibleProtocol
		return inst, nil
	}
	inst.ExecutablePath = path
	inst.ExecutableSHA256 = hash
	versionText, err := a.probeVersion(ctx, path)
	if err != nil {
		inst.Compatibility = agent.CompatibilityIncompatibleProtocol
		return inst, nil
	}
	v, ok := parseVersion(versionText)
	if !ok {
		inst.Compatibility = agent.CompatibilityIncompatibleProtocol
		return inst, nil
	}
	inst.CLIVersion = v.String()
	if !inRange(v, a.binding.VersionRange) {
		inst.Compatibility = agent.CompatibilityUnknownVersion
		return inst, nil
	}
	inst.Compatibility = agent.CompatibilitySupported
	return inst, nil
}

// probeVersion runs `codex --version` through the supervisor and returns
// the bounded stdout text. The probe is read-only and never starts a
// paid model request.
func (a *Adapter) probeVersion(ctx context.Context, path string) (string, error) {
	h, events, err := a.sup.Start(ctx, process.ProcessSpec{
		Executable:     path,
		Args:           []string{"--version"},
		Env:            safeEnv(),
		MaxOutputBytes: maxProbeBytes,
		Timeout:        detectTimeout,
	})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	overflowed := false
	for ev := range events {
		switch ev.Kind {
		case process.EventFrameOut:
			out.Write(ev.Frame)
		case process.EventOverflowOut:
			overflowed = true
		}
	}
	exit, err := a.sup.Wait(ctx, h)
	if err != nil {
		return "", err
	}
	if overflowed || exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return "", fmt.Errorf("codex version probe failed (fact %d, code %d)", exit.Fact, exit.Code)
	}
	return out.String(), nil
}

// ---------------------------------------------------------------------------
// Start and Resume (argv-only launch)
// ---------------------------------------------------------------------------

// Start launches one codex Session through the supervisor. The argv is
// built only from typed fields allowed by the binding; the prompt is fed
// through stdin, the schema through the managed immutable file, and the
// cwd through -C.
func (a *Adapter) Start(ctx context.Context, req agent.StartRequest) (agent.Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !bindingHasCap(a.binding.StartCapabilities, requiredStartCaps) {
		return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"codex binding lacks the required start capabilities")
	}
	in, err := requestInput(req.Input)
	if err != nil {
		return nil, err
	}
	if err := proveSchema(in.SchemaPath); err != nil {
		return nil, err
	}
	if req.CWD != "" && !filepath.IsAbs(req.CWD) {
		return nil, model.InvalidInputFault("codex working directory must be an absolute path")
	}
	path, err := exec.LookPath(a.binding.Executable.Name)
	if err != nil {
		return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"codex executable cannot be resolved: "+err.Error())
	}
	spec := process.ProcessSpec{
		Executable: path,
		Args: StartArgv(StartRequest{
			SchemaPath: in.SchemaPath,
			Worktree:   req.CWD,
			Model:      in.Model,
			Prompt:     req.Prompt,
		}),
		Dir:     req.CWD,
		Env:     safeEnv(),
		Stdin:   strings.NewReader(req.Prompt),
		Timeout: req.Timeout,
	}
	return a.launch(ctx, spec)
}

// Resume re-establishes one Provider Session: exec resume with the
// session id. The captured 0.141.0 resume help accepts no -C/--cd, so
// the working directory travels as the supervised process cwd.
func (a *Adapter) Resume(ctx context.Context, req agent.ResumeRequest) (agent.Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !bindingHasCap(a.binding.ResumeCapabilities, requiredResumeCaps) {
		return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"codex binding lacks the required resume capabilities")
	}
	if req.ProviderSessionID == "" {
		return nil, model.InvalidInputFault("codex resume requires the provider session id")
	}
	in, err := requestInput(req.Input)
	if err != nil {
		return nil, err
	}
	if err := proveSchema(in.SchemaPath); err != nil {
		return nil, err
	}
	if req.CWD != "" && !filepath.IsAbs(req.CWD) {
		return nil, model.InvalidInputFault("codex working directory must be an absolute path")
	}
	path, err := exec.LookPath(a.binding.Executable.Name)
	if err != nil {
		return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"codex executable cannot be resolved: "+err.Error())
	}
	spec := process.ProcessSpec{
		Executable: path,
		Args: ResumeArgv(ResumeRequest{
			SchemaPath: in.SchemaPath,
			SessionID:  req.ProviderSessionID,
			Prompt:     req.Prompt,
		}),
		Dir:     req.CWD,
		Env:     safeEnv(),
		Stdin:   strings.NewReader(req.Prompt),
		Timeout: req.Timeout,
	}
	return a.launch(ctx, spec)
}

// launch starts the supervised process and returns its run.
func (a *Adapter) launch(ctx context.Context, spec process.ProcessSpec) (agent.Run, error) {
	h, events, err := a.sup.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &run{ad: a, sup: a.sup, h: h, ch: events}, nil
}

// ---------------------------------------------------------------------------
// Cancel and Inspect
// ---------------------------------------------------------------------------

// Cancel performs the controlled stop of one live run: the run is marked
// stopped (so a still-draining stream settles as CANCELLED, never as a
// crash) and deregistered, and Terminate is delivered to the exact
// process group. The partial redacted events the Runtime already read
// are preserved by the drain. An already-ended run is a no-op.
func (a *Adapter) Cancel(ctx context.Context, handle agent.RunHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	r := a.runs[handle.ProviderSessionID]
	delete(a.runs, handle.ProviderSessionID)
	a.mu.Unlock()
	if r == nil {
		return nil
	}
	r.stop()
	if r.isReaped() {
		return nil
	}
	return a.sup.Signal(ctx, r.h, process.Terminate)
}

// Inspect reports the adapter's live-run truth for one provider session.
// The Runtime ledger owns the Session facts; the adapter reports only
// whether its process is still supervised.
func (a *Adapter) Inspect(ctx context.Context, id agent.ProviderSessionID) (agent.SessionFact, error) {
	if err := ctx.Err(); err != nil {
		return agent.SessionFact{}, err
	}
	a.mu.Lock()
	r := a.runs[id]
	a.mu.Unlock()
	fact := agent.SessionFact{
		Session:  model.Session{ProviderSessionID: string(id)},
		Provider: "codex",
	}
	if r != nil && !r.isReaped() {
		fact.Running = true
		fact.Handle = agent.RunHandle{ProviderSessionID: id}
	}
	return fact, nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// bindingHasCap reports whether the declared capability list contains
// every required capability.
func bindingHasCap(have []string, required []string) bool {
	for _, want := range required {
		ok := false
		for _, h := range have {
			if h == want {
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

// requestInput extracts the codex typed input facts; any other input
// shape fails closed (the schema is mandatory, never guessed).
func requestInput(input any) (Input, error) {
	in, ok := input.(Input)
	if !ok {
		return Input{}, model.InvalidInputFault("codex requires the typed codex.Input carrying the managed schema file")
	}
	if in.SchemaPath == "" {
		return Input{}, model.InvalidInputFault("codex requires the managed schema file path")
	}
	return in, nil
}

// proveSchema verifies the managed immutable schema file exists before
// any launch: an absent schema cannot promise the structured terminal
// event the pipeline requires.
func proveSchema(path string) error {
	if !filepath.IsAbs(path) {
		return model.InvalidInputFault("codex schema file must be an absolute managed path")
	}
	if _, err := os.Stat(path); err != nil {
		return model.InvalidInputFault("codex schema file is not available: " + err.Error())
	}
	return nil
}

// safeEnv is the explicit child environment: only HOME and PATH, the
// minimal values the codex CLI needs to locate its configuration and
// spawn git. No parent value can leak into the child through this seam
// (design 13.1); tokens and provider-owned configuration are never
// copied.
func safeEnv() map[string]string {
	env := map[string]string{}
	if home, err := os.UserHomeDir(); err == nil {
		env["HOME"] = home
	}
	if path := os.Getenv("PATH"); path != "" {
		env["PATH"] = path
	}
	return env
}

// hashFile digests one executable's bytes (the binary identity fact the
// Execution Approval drift gate compares against its pin).
func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// capsFromBinding derives the installation capabilities from the
// binding's Start and Resume capability lists (design 14.2).
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
