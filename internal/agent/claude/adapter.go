// Package claude implements the Claude Adapter (design 14.1): the
// fail-closed binding between the cflow.dialect.claude-stream-json.v1
// wire dialect and the unified Agent Event contract. The adapter launches
// the claude CLI through the process Supervisor argv-only (never a
// shell) in non-interactive print mode (`--print`), feeds the prompt
// through stdin as one validated stream-json user message frame and
// closes stdin deterministically, passes the managed immutable schema
// JSON text through --json-schema and the approved hard budget through
// --max-budget-usd, sets the working directory as the supervised process
// cwd, and drives the bounded stream-json frame pipeline onto the unified
// events the Runtime validates, redacts, and persists. Detection runs
// only the read-only version probe (`claude --version`); a paid model
// request is never started by the adapter.
package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// untyped input can inject flags: no danger, bypass, ignore, permission,
// or tool allow/deny flag can ever be added through this constructor.
// Prompt travels through stdin as a stream-json user frame, never argv.
type StartRequest struct {
	SchemaJSON   string // managed immutable schema JSON text (--json-schema)
	MaxBudgetUSD string // approved hard budget in USD (--max-budget-usd)
	Model        string // approved optional model ("" = provider default)
	Prompt       string // fed through stdin as a validated user frame
}

// ResumeRequest is the typed Resume argv request: the Start argv plus
// --resume <session-id>. The session identity travels through --resume,
// never through --session-id: the stream's validated start event is the
// only Session authority (design 14.3).
type ResumeRequest struct {
	SchemaJSON   string
	MaxBudgetUSD string
	Model        string
	SessionID    agent.ProviderSessionID
	Prompt       string
}

// Input carries the claude-specific typed facts for one Start or Resume:
// the managed immutable schema JSON text (the argv contract always
// carries --json-schema), the approved hard budget (--max-budget-usd),
// and the approved optional model. The Application materializes the
// purpose's embedded output schema and passes its content here; the
// adapter refuses to launch without a proven schema, because the argv
// contract always carries --json-schema and an unproven schema cannot
// promise the structured terminal event the pipeline requires (PRD 约束
// 43).
//
// ContextBundleRef names the immutable redacted Context Bundle Revision
// of the superseded LOST Session an automatic fallback successor carries
// (Task 18; design 14.4). It is a reference only — never a credential or
// an unredacted transcript — and a fresh Session's typed input omits it.
type Input struct {
	SchemaJSON   string `json:"schema_json"`
	MaxBudgetUSD string `json:"max_budget_usd"`
	Model        string `json:"model,omitempty"`
	// ContextBundleRef is the persisted bundle path of the handoff ("" for
	// a brand-new Session).
	ContextBundleRef string `json:"context_bundle_ref,omitempty"`
}

// StartArgv builds the exact Start argv (brief acceptance): noninteractive
// `claude --print --input-format stream-json --output-format stream-json
// --verbose --json-schema <json> --max-budget-usd <amount>` with the
// approved optional --model. Existing Provider permission defaults remain
// in force: no permission-bypass or allowlist flag is ever added.
//
// --verbose is required by the installed Claude CLI (>= 2.1.221) when
// --print is combined with --output-format stream-json; without it the
// CLI exits 1 with "requires --verbose" before emitting any event (the
// real-wire E2E confirmed this). The verbose diagnostics arrive as
// `system` frames with `hook_started`/`hook_response` subtypes, which
// the dialect passes over (dialect.go).
func StartArgv(req StartRequest) []string {
	argv := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--json-schema", req.SchemaJSON,
		"--max-budget-usd", req.MaxBudgetUSD,
	}
	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	return argv
}

// ResumeArgv builds the exact Resume argv: the Start argv plus
// --resume <session-id>.
func ResumeArgv(req ResumeRequest) []string {
	argv := StartArgv(StartRequest{
		SchemaJSON:   req.SchemaJSON,
		MaxBudgetUSD: req.MaxBudgetUSD,
		Model:        req.Model,
	})
	return append(argv, "--resume", string(req.SessionID))
}

// Adapter is the Claude Adapter over one immutable Registry binding
// (design 14.2): detection, argv construction, and capability gates all
// judge against this binding and nothing else.
type Adapter struct {
	sup     process.Supervisor
	binding agent.ProviderBinding

	mu   sync.Mutex
	runs map[agent.ProviderSessionID]*run
}

// New constructs the Claude Adapter bound to the exact Registry binding.
func New(sup process.Supervisor, binding agent.ProviderBinding) agent.Adapter {
	return &Adapter{sup: sup, binding: binding, runs: map[agent.ProviderSessionID]*run{}}
}

// ---------------------------------------------------------------------------
// Detection (PRD 已确认：未知 Provider CLI 协议 Fail-closed)
// ---------------------------------------------------------------------------

// Detect runs the read-only version probe (`claude --version`, never a
// model request), hashes the resolved executable, and returns
// MISSING/SUPPORTED/UNKNOWN_VERSION/INCOMPATIBLE_PROTOCOL against the
// binding's supported range and dialect. Only facts are reported: the
// executable path and sha256, the parsed CLI version, the binding
// revision, dialect, and capabilities. Authentication is a separate
// concern (PRD: Authentication Unknown is never disguised as Protocol
// Unsupported; the doctor reports it separately in Task 16).
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

// probeVersion runs `claude --version` through the supervisor and returns
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
		return "", fmt.Errorf("claude version probe failed (fact %d, code %d)", exit.Fact, exit.Code)
	}
	return out.String(), nil
}

// ---------------------------------------------------------------------------
// Start and Resume (argv-only launch)
// ---------------------------------------------------------------------------

// Start launches one claude Session through the supervisor. The argv is
// built only from typed fields allowed by the binding; the prompt is fed
// through stdin as one validated stream-json user message frame, the
// schema through --json-schema, the approved budget through
// --max-budget-usd, and the cwd as the supervised process cwd.
func (a *Adapter) Start(ctx context.Context, req agent.StartRequest) (agent.Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !bindingHasCap(a.binding.StartCapabilities, requiredStartCaps) {
		return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"claude binding lacks the required start capabilities")
	}
	in, err := requestInput(req.Input)
	if err != nil {
		return nil, err
	}
	if req.CWD != "" && !filepath.IsAbs(req.CWD) {
		return nil, model.InvalidInputFault("claude working directory must be an absolute path")
	}
	path, err := exec.LookPath(a.binding.Executable.Name)
	if err != nil {
		return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"claude executable cannot be resolved: "+err.Error())
	}
	stdin, err := userFrame(req.Prompt)
	if err != nil {
		return nil, err
	}
	spec := process.ProcessSpec{
		Executable: path,
		Args: StartArgv(StartRequest{
			SchemaJSON:   in.SchemaJSON,
			MaxBudgetUSD: in.MaxBudgetUSD,
			Model:        in.Model,
			Prompt:       req.Prompt,
		}),
		Dir:     req.CWD,
		Env:     safeEnv(),
		Stdin:   stdin,
		Timeout: req.Timeout,
	}
	return a.launch(ctx, spec)
}

// Resume re-establishes one Provider Session: the Start argv plus
// --resume <session-id>, the working directory as the supervised process
// cwd.
func (a *Adapter) Resume(ctx context.Context, req agent.ResumeRequest) (agent.Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !bindingHasCap(a.binding.ResumeCapabilities, requiredResumeCaps) {
		return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"claude binding lacks the required resume capabilities")
	}
	if req.ProviderSessionID == "" {
		return nil, model.InvalidInputFault("claude resume requires the provider session id")
	}
	in, err := requestInput(req.Input)
	if err != nil {
		return nil, err
	}
	if req.CWD != "" && !filepath.IsAbs(req.CWD) {
		return nil, model.InvalidInputFault("claude working directory must be an absolute path")
	}
	path, err := exec.LookPath(a.binding.Executable.Name)
	if err != nil {
		return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"claude executable cannot be resolved: "+err.Error())
	}
	stdin, err := userFrame(req.Prompt)
	if err != nil {
		return nil, err
	}
	spec := process.ProcessSpec{
		Executable: path,
		Args: ResumeArgv(ResumeRequest{
			SchemaJSON:   in.SchemaJSON,
			MaxBudgetUSD: in.MaxBudgetUSD,
			Model:        in.Model,
			SessionID:    req.ProviderSessionID,
			Prompt:       req.Prompt,
		}),
		Dir:     req.CWD,
		Env:     safeEnv(),
		Stdin:   stdin,
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
		Provider: "claude",
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

// requestInput extracts the claude typed input facts; any other input
// shape fails closed (the schema and budget are mandatory, never
// guessed).
func requestInput(input any) (Input, error) {
	in, ok := input.(Input)
	if !ok {
		return Input{}, model.InvalidInputFault("claude requires the typed claude.Input carrying the managed schema and budget")
	}
	if err := validateSchemaJSON(in.SchemaJSON); err != nil {
		return Input{}, err
	}
	if err := validateBudget(in.MaxBudgetUSD); err != nil {
		return Input{}, err
	}
	return in, nil
}

// validateSchemaJSON verifies the managed immutable schema JSON text is
// well-formed JSON before any launch: an unproven schema cannot promise
// the structured terminal event the pipeline requires (PRD 约束 43).
func validateSchemaJSON(schema string) error {
	if schema == "" {
		return model.InvalidInputFault("claude requires the managed schema JSON text")
	}
	if !json.Valid([]byte(schema)) {
		return model.InvalidInputFault("claude schema JSON is not valid JSON")
	}
	return nil
}

// validateBudget verifies the approved hard budget is a non-negative
// decimal amount: the argv contract always carries --max-budget-usd and
// an unusable amount can never reach argv.
func validateBudget(amount string) error {
	if amount == "" {
		return model.InvalidInputFault("claude requires the approved budget amount")
	}
	v, err := strconv.ParseFloat(amount, 64)
	if err != nil || v < 0 {
		return model.InvalidInputFault("claude budget amount must be a non-negative decimal")
	}
	return nil
}

// userFrame serializes the prompt as one validated stream-json user
// message frame and closes stdin deterministically: the frame is built by
// a struct marshal (never string concatenation, so any prompt content is
// safely escaped) and the bounded reader's EOF closes stdin.
func userFrame(prompt string) (*strings.Reader, error) {
	frame, err := json.Marshal(streamInputFrame{
		Type: "user",
		Message: inputMessage{
			Role:    "user",
			Content: []inputBlock{{Type: "text", Text: prompt}},
		},
	})
	if err != nil {
		return nil, model.InvalidInputFault("claude prompt cannot be serialized as a stream-json frame")
	}
	return strings.NewReader(string(frame) + "\n"), nil
}

// safeEnv is the explicit child environment: only HOME and PATH, the
// minimal values the claude CLI needs to locate its configuration. No
// parent value can leak into the child through this seam (design 13.1);
// tokens and provider-owned configuration are never copied.
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
// binding's Start and Resume capability lists (design 14.2). The Claude
// binding declares a native budget limit, so BudgetLimit is reported from
// the binding's budget behaviour, never inferred.
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
		BudgetLimit:                    strings.Contains(b.BudgetBehavior, "native budget limit"),
	}
}
