// The Agent Runtime (design 14): it allocates CFlow Sessions through
// validated start events, drives every Provider event through the unified
// pipeline (design 14.3), enforces Session lineage (14.4), and owns the
// Resume fallback with immutable Context Bundles. The Runtime reports
// facts and faults; it never charges Retry budgets — the Decision Kernel
// charges from the Fault table (design 14.4 step 5).
package agent

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// RuntimeOptions wires one Runtime: the deterministic Clock and ID source
// (brief Step 5: byte-identical manifests for fixed Clock/ID input), the
// immutable Provider Registry every binding check consults, the redaction
// policy, the managed evidence root, and the Provider Adapters.
type RuntimeOptions struct {
	Now         func() time.Time
	IDs         model.IDSource
	Registry    *ProviderRegistry
	Redaction   security.Registry
	EvidenceDir string
	Adapters    map[string]Adapter
}

// Runtime is the orchestration seam over the Adapter contract. It is safe
// for concurrent use: the ledger, the live-run table, and the evidence
// writer are mutex-guarded; one Runtime serves one Project.
type Runtime struct {
	now       func() time.Time
	ids       model.IDSource
	registry  *ProviderRegistry
	redaction security.Registry
	evidence  *evidenceWriter
	adapters  map[string]Adapter

	mu         sync.Mutex
	byCFlow    map[model.SessionID]*sessionRecord
	byProvider map[ProviderSessionID]*sessionRecord
	live       map[model.RunID]*liveRun
}

// sessionRecord is the Runtime's ledger fact for one Session: the
// canonical model record plus the Provider it runs on and the live run
// handle while a run is draining.
type sessionRecord struct {
	session  model.Session
	provider string
	runID    model.RunID
}

// liveRun is one draining run's cancellation state and handle.
type liveRun struct {
	mu        sync.Mutex
	handle    RunHandle
	cancelled bool
}

func (l *liveRun) cancel() {
	l.mu.Lock()
	l.cancelled = true
	l.mu.Unlock()
}

func (l *liveRun) isCancelled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cancelled
}

func (l *liveRun) setHandle(h RunHandle) {
	l.mu.Lock()
	l.handle = h
	l.mu.Unlock()
}

func (l *liveRun) getHandle() RunHandle {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.handle
}

// NewRuntime constructs a Runtime. The Clock and ID source default to
// deterministic sequential sources when nil; the registry is required;
// the evidence root, when set, must be an absolute safe path (created
// 0700 through the Security Guard).
func NewRuntime(opts RuntimeOptions) (*Runtime, error) {
	if opts.Registry == nil {
		return nil, model.InvalidInputFault("runtime requires the provider registry")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Unix(0, 0) }
	}
	ids := opts.IDs
	if ids == nil {
		ids = model.SequentialIDSource()
	}
	ev, err := newEvidenceWriter(opts.EvidenceDir, opts.Redaction)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		now:        now,
		ids:        ids,
		registry:   opts.Registry,
		redaction:  opts.Redaction,
		evidence:   ev,
		adapters:   opts.Adapters,
		byCFlow:    map[model.SessionID]*sessionRecord{},
		byProvider: map[ProviderSessionID]*sessionRecord{},
		live:       map[model.RunID]*liveRun{},
	}, nil
}

// Close flushes and closes every open evidence file.
func (r *Runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.evidence == nil {
		return nil
	}
	for id := range r.evidence.sessions {
		if err := r.evidence.closeSession(id); err != nil {
			return err
		}
	}
	return nil
}

// EvidenceDir reports the managed evidence root ("" when none).
func (r *Runtime) EvidenceDir() (string, error) {
	if r.evidence == nil {
		return "", nil
	}
	return r.evidence.root(), nil
}

// Hydrate loads persisted Session facts before Start/Resume (the Recovery
// path: the Application hydrates from the Store). Facts without a CFlow
// id receive one from the ID source; duplicates and invalid purposes fail
// closed.
func (r *Runtime) Hydrate(ctx context.Context, facts []SessionFact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range facts {
		if !f.Session.Purpose.Valid() {
			return model.InvalidInputFault("hydrated session has an unknown purpose")
		}
		if !f.Session.Status.Valid() {
			return model.InvalidInputFault("hydrated session has an unknown status")
		}
		rec := &sessionRecord{session: f.Session, provider: f.Provider}
		if rec.session.ID == "" {
			rec.session.ID = model.SessionID(r.ids(model.IDSession))
		}
		if _, dup := r.byCFlow[rec.session.ID]; dup {
			return model.InvalidInputFault("duplicate hydrated session id")
		}
		if rec.session.ProviderSessionID != "" {
			pid := ProviderSessionID(rec.session.ProviderSessionID)
			if _, dup := r.byProvider[pid]; dup {
				return model.InvalidInputFault("duplicate hydrated provider session id")
			}
			r.byProvider[pid] = rec
		}
		r.byCFlow[rec.session.ID] = rec
	}
	return nil
}

// Sessions returns a deterministic snapshot of the ledger.
func (r *Runtime) Sessions() []SessionFact {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]model.SessionID, 0, len(r.byCFlow))
	for id := range r.byCFlow {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]SessionFact, 0, len(ids))
	for _, id := range ids {
		rec := r.byCFlow[id]
		fact := SessionFact{
			Session:  rec.session,
			Provider: rec.provider,
		}
		if lr := r.live[rec.runID]; lr != nil {
			fact.Handle = lr.getHandle()
		}
		out = append(out, fact)
	}
	return out
}

// Inspect returns the observable fact of one provider session. An unknown
// session fails closed.
func (r *Runtime) Inspect(ctx context.Context, id ProviderSessionID) (SessionFact, error) {
	if err := ctx.Err(); err != nil {
		return SessionFact{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.byProvider[id]
	if rec == nil {
		return SessionFact{}, model.InvalidInputFault("unknown provider session")
	}
	fact := SessionFact{
		Session:  rec.session,
		Provider: rec.provider,
	}
	if lr := r.live[rec.runID]; lr != nil {
		fact.Handle = lr.getHandle()
		fact.Running = !sessionStatusTerminal(rec.session.Status)
	}
	return fact, nil
}

// ---------------------------------------------------------------------------
// Start (design 14.3-14.4)
// ---------------------------------------------------------------------------

// Start runs one brand-new Session through the unified pipeline. The
// Session is created only when a validated session_started event
// establishes its id; protocol violations, crashes, and redaction
// failures return a Fault and persist only redacted evidence.
func (r *Runtime) Start(ctx context.Context, req StartRequest) (*RunResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !req.Purpose.Valid() {
		return nil, model.InvalidInputFault("unknown agent purpose")
	}
	if req.Provider == "" {
		return nil, model.InvalidInputFault("provider is required")
	}
	if req.Prompt == "" {
		return nil, model.InvalidInputFault("prompt is required")
	}
	var parent model.SessionID
	if req.Supersedes != "" {
		r.mu.Lock()
		par := r.byProvider[req.Supersedes]
		r.mu.Unlock()
		if par == nil {
			return nil, model.InvalidInputFault("unknown superseded session")
		}
		if par.session.Purpose != req.Purpose {
			return nil, model.NewFault(model.CodeSessionIndependenceViolation,
				"a successor session must keep the superseded session's purpose")
		}
		parent = par.session.ID
	}
	binding, ad, err := r.resolveProvider(ctx, req.Provider, false)
	if err != nil {
		return nil, err
	}
	arun, err := ad.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	promptHash := hashText(req.Prompt)
	inputHash, err := hashInput(req.Input)
	if err != nil {
		return nil, err
	}
	return r.drain(ctx, arun, drainConfig{
		binding:    binding,
		purpose:    req.Purpose,
		provider:   req.Provider,
		supersedes: parent,
		promptHash: promptHash,
		inputHash:  inputHash,
		runID:      model.RunID(r.ids(model.IDRun)),
		startedAt:  r.now(),
	})
}

// ---------------------------------------------------------------------------
// Resume and fallback (design 14.4, PRD 已确认：Session Resume 失败与跨
// Provider 上下文交接)
// ---------------------------------------------------------------------------

// Resume re-establishes an existing Session. When native Resume fails
// unrecoverably (no protocol/auth/binding fault), the original Session is
// retained as LOST, an immutable redacted Context Bundle is persisted, the
// successor Provider's capabilities are validated, and a successor Session
// with supersedes_session_id is created. The Decision Kernel allocates the
// successor Attempt and charges the approved budget from these facts; the
// Runtime never charges.
func (r *Runtime) Resume(ctx context.Context, req ResumeRequest) (*ResumeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !req.Purpose.Valid() {
		return nil, model.InvalidInputFault("unknown agent purpose")
	}
	if req.Provider == "" {
		return nil, model.InvalidInputFault("provider is required")
	}
	if req.Prompt == "" {
		return nil, model.InvalidInputFault("prompt is required")
	}
	r.mu.Lock()
	rec := r.byProvider[req.ProviderSessionID]
	r.mu.Unlock()
	if rec == nil {
		return nil, model.InvalidInputFault("provider session not found for resume")
	}
	// A terminal Session can never be resumed: re-opening a COMPLETED or
	// CANCELLED session would revive an immutable outcome, and resuming a
	// LOST session would chain a second successor lineage from the same
	// original (design 14.4 step 4: one successor per Lost original).
	// The Runtime is the enforcement layer for Session state.
	if sessionStatusTerminal(rec.session.Status) {
		return nil, model.NewFault(model.CodeSessionIndependenceViolation,
			"a terminal session can never be resumed")
	}
	if rec.session.Purpose != req.Purpose {
		return nil, model.NewFault(model.CodeSessionIndependenceViolation,
			"a resumed session must keep its purpose")
	}
	binding, ad, err := r.resolveProvider(ctx, req.Provider, true)
	if err != nil {
		return nil, err
	}
	arun, err := ad.Resume(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isProtocolFault(err) || isProtocolError(err) {
			return nil, err
		}
		return r.resumeFallback(ctx, req, rec)
	}
	promptHash := hashText(req.Prompt)
	inputHash, err := hashInput(req.Input)
	if err != nil {
		return nil, err
	}
	result, err := r.drain(ctx, arun, drainConfig{
		binding:    binding,
		bound:      req.ProviderSessionID,
		session:    rec,
		purpose:    req.Purpose,
		provider:   req.Provider,
		promptHash: promptHash,
		inputHash:  inputHash,
		runID:      model.RunID(r.ids(model.IDRun)),
		startedAt:  r.now(),
	})
	if err != nil {
		return nil, err
	}
	return &ResumeResult{Session: result.Session, Run: result}, nil
}

// resumeFallback implements design 14.4 steps 1-4.
func (r *Runtime) resumeFallback(ctx context.Context, req ResumeRequest, rec *sessionRecord) (*ResumeResult, error) {
	// 1. Retain the original Session as LOST.
	r.mu.Lock()
	rec.session.Status = model.SessionLost
	r.mu.Unlock()

	// 2. Build the immutable redacted Context Bundle from the request's
	// context (PRD minimum content list).
	bundle, err := r.CreateContextBundle(ctx, ContextBundleRequest{
		SessionID:         rec.session.ID,
		ProviderSessionID: ProviderSessionID(rec.session.ProviderSessionID),
		Purpose:           rec.session.Purpose,
		Context:           req.Context,
	})
	if err != nil {
		return nil, err
	}

	// The LOST Session manifest records the bundle reference, so the loss
	// is auditable even when the successor is later blocked.
	if err := r.persistLostManifest(ctx, rec, bundle); err != nil {
		return nil, err
	}

	// 3. Validate the successor Adapter's capabilities (design 14.4): the
	// successor may switch Provider; it must prove the Start protocol
	// capabilities of the current binding.
	if _, _, err := r.resolveProvider(ctx, req.Provider, false); err != nil {
		return nil, err
	}

	// 4. Create the successor Session with supersedes_session_id, keeping
	// the original purpose (the role lineage).
	r.mu.Lock()
	succ := &sessionRecord{
		session: model.Session{
			ID:         model.SessionID(r.ids(model.IDSession)),
			Purpose:    rec.session.Purpose,
			Status:     model.SessionStarting,
			Supersedes: rec.session.ID,
		},
		provider: req.Provider,
	}
	r.byCFlow[succ.session.ID] = succ
	r.mu.Unlock()

	// 5. The successor Attempt allocation and Retry charge belong to the
	// Decision Kernel (Fault table); the Runtime reports the facts only.
	return &ResumeResult{
		Session: rec.session,
		Fallback: &FallbackResult{
			LostSession:      rec.session,
			ContextBundle:    bundle,
			SuccessorSession: succ.session,
		},
	}, nil
}

// persistLostManifest writes the LOST Session manifest including the
// bundle reference.
func (r *Runtime) persistLostManifest(ctx context.Context, rec *sessionRecord, bundle ContextBundle) error {
	if r.evidence == nil {
		return nil
	}
	ref := &persistedBundleRef{Revision: bundle.Revision, Hash: bundle.Hash}
	m := persistedSession{
		SessionID:         string(rec.session.ID),
		Provider:          rec.provider,
		ProviderSessionID: rec.session.ProviderSessionID,
		Purpose:           rec.session.Purpose,
		Status:            model.SessionLost,
		Supersedes:        string(rec.session.Supersedes),
		StartedAt:         r.now(),
		EndedAt:           r.now(),
		ContextBundle:     ref,
	}
	if _, err := r.evidence.writeManifest(rec.session.ID, m); err != nil {
		return err
	}
	return nil
}

// isProtocolFault reports whether an error chain carries a protocol or
// authentication Fault that must pass through instead of triggering the
// Resume fallback (PRD 约束 43: protocol/auth failures never fall back).
func isProtocolFault(err error) bool {
	var f *model.Fault
	if !errors.As(err, &f) {
		return false
	}
	switch f.Code {
	case model.CodeProviderProtocolUnsupported, model.CodeProviderProtocolViolation,
		model.CodeProviderSessionIDMissing, model.CodeProviderBindingChanged,
		model.CodeProviderAuthenticationRequired:
		return true
	}
	return false
}

// isProtocolError reports whether an error is an adapter ProtocolError.
func isProtocolError(err error) bool {
	var pe *ProtocolError
	return errors.As(err, &pe)
}

// ---------------------------------------------------------------------------
// Cancel and provider resolution
// ---------------------------------------------------------------------------

// Cancel performs the two-phase controlled stop of one live run: the
// handle is marked cancelled and the owning Adapter stops the process at
// the next event boundary. The drain settles the run as CANCELLED.
func (r *Runtime) Cancel(ctx context.Context, handle RunHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	lr := r.live[handle.RunID]
	r.mu.Unlock()
	if lr == nil {
		return model.InvalidInputFault("no live run for the handle")
	}
	lr.cancel()
	r.mu.Lock()
	rec := r.byProvider[handle.ProviderSessionID]
	r.mu.Unlock()
	if rec == nil {
		return model.InvalidInputFault("unknown provider session in handle")
	}
	ad := r.adapters[rec.provider]
	if ad == nil {
		return model.InvalidInputFault("no adapter bound for the run's provider")
	}
	return ad.Cancel(ctx, handle)
}

// resolveProvider verifies the route binding before every Start/Resume/
// fallback (PRD 已确认：未知 Provider CLI 协议 Fail-closed): the binding is
// selected, its protocol capabilities are proven, the Adapter exists, and
// a fresh Detect result Compare-and-Swaps the binding (dialect and
// compatibility). Mismatches block before any Provider call.
func (r *Runtime) resolveProvider(ctx context.Context, name string, resume bool) (ProviderBinding, Adapter, error) {
	if err := ctx.Err(); err != nil {
		return ProviderBinding{}, nil, err
	}
	binding, err := r.registry.Select(name)
	if err != nil {
		return ProviderBinding{}, nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"provider cannot be selected: "+err.Error())
	}
	if resume {
		if !bindingHasResume(binding, requiredResumeCapabilities) {
			return ProviderBinding{}, nil, model.NewFault(model.CodeProviderProtocolUnsupported,
				"provider binding lacks the required resume capabilities")
		}
	} else if !bindingHas(binding, requiredStartCapabilities) {
		return ProviderBinding{}, nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"provider binding lacks the required start capabilities")
	}
	ad := r.adapters[name]
	if ad == nil {
		return ProviderBinding{}, nil, model.InvalidInputFault("no adapter bound for provider " + name)
	}
	inst, err := ad.Detect(ctx)
	if err != nil {
		return ProviderBinding{}, nil, err
	}
	if inst.Compatibility != CompatibilitySupported {
		return ProviderBinding{}, nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"provider is not supported: "+string(inst.Compatibility))
	}
	if inst.DialectID != binding.Dialect.ID {
		return ProviderBinding{}, nil, model.NewFault(model.CodeProviderBindingChanged,
			"detected dialect does not match the approved protocol binding")
	}
	return binding, ad, nil
}
