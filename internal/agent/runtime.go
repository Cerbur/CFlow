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
	"io"
	"sort"
	"sync"
	"time"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// RuntimeOptions wires one Runtime: the deterministic Clock and ID source
// (brief Step 5: byte-identical manifests for fixed Clock/ID input), the
// immutable Provider Registry every binding check consults, the redaction
// policy, the managed evidence root, the Provider Adapters, and the
// immutable Routing Policy Set the Execution Approval bound (Task 16:
// nil keeps the registry-level checks for commands outside an approved
// workflow).
type RuntimeOptions struct {
	Now         func() time.Time
	IDs         model.IDSource
	Registry    *ProviderRegistry
	Redaction   security.Registry
	EvidenceDir string
	Adapters    map[string]Adapter
	Routing     *RoutingPolicySet
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
	routing   *RoutingPolicySet

	mu         sync.Mutex
	byCFlow    map[model.SessionID]*sessionRecord
	byProvider map[ProviderSessionID]*sessionRecord
	live       map[model.RunID]*liveRun
	// detectCache holds the verified Installation facts of the dispatch
	// CAS pre-pass (Task 16): every Start/Resume of the pass reuses the
	// same verified identity instead of re-detecting per operation.
	detectCache map[string]Installation
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
		now:         now,
		ids:         ids,
		registry:    opts.Registry,
		redaction:   opts.Redaction,
		evidence:    ev,
		adapters:    opts.Adapters,
		routing:     opts.Routing,
		byCFlow:     map[model.SessionID]*sessionRecord{},
		byProvider:  map[ProviderSessionID]*sessionRecord{},
		live:        map[model.RunID]*liveRun{},
		detectCache: map[string]Installation{},
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
// Routing policy attachment and the dispatch CAS pre-pass (Task 16,
// design 14.2, PRD 约束 306)
// ---------------------------------------------------------------------------

// SetRoutingPolicy attaches the immutable Routing Policy Set the
// workflow's Execution Approval bound (nil detaches it). The policy is
// read before every operation and never mutated after attachment.
func (r *Runtime) SetRoutingPolicy(set *RoutingPolicySet) {
	r.mu.Lock()
	r.routing = set
	r.mu.Unlock()
}

// FallbackBundle returns the latest persisted immutable Context Bundle
// of one Session (the cross-Provider handoff of an unrecoverable Resume;
// false when none exists). The successor Session of an automatic
// fallback reads the LOST original's bundle through its Supersedes link
// (design 14.4): the bundle survives across dispatch passes because it
// lives in the evidence root, never in the pass's Runtime ledger.
func (r *Runtime) FallbackBundle(sessionID model.SessionID) (ContextBundle, bool) {
	pb, path, ok := r.evidence.readLatestBundle(sessionID)
	if !ok {
		return ContextBundle{}, false
	}
	return ContextBundle{
		SchemaVersion:     pb.SchemaVersion,
		Revision:          pb.Revision,
		Hash:              pb.Hash,
		Path:              path,
		SessionID:         model.SessionID(pb.SessionID),
		ProviderSessionID: ProviderSessionID(pb.ProviderSessionID),
		Purpose:           pb.Purpose,
		CreatedAt:         pb.CreatedAt,
		Context: ContextInput{
			Requirement:        pb.Requirement,
			Plan:               pinFromPersisted(pb.Plan),
			Spec:               pinFromPersisted(pb.Spec),
			Catalog:            pinFromPersisted(pb.Catalog),
			Workflow:           pinFromPersisted(pb.Workflow),
			RepositoryBaseline: pb.RepositoryBaseline,
			StageSummary:       pb.StageSummary,
			Decisions:          append([]string(nil), pb.Decisions...),
			FailureEvidence:    evidenceFromPersisted(pb.FailureEvidence),
			OpenQuestions:      append([]string(nil), pb.OpenQuestions...),
			PermissionBoundary: pb.PermissionBoundary,
		},
		RedactionRevision: pb.RedactionRevision,
	}, true
}

// RouteBinding returns the approved binding of one Purpose route from
// the immutable Routing Policy Set the Execution Approval bound (false
// when the Purpose or the Provider has no approved binding). The
// Application reads the approved model/budget here to build the typed
// Adapter input of a routed Session (design 14.5).
func (r *Runtime) RouteBinding(purpose model.AgentPurpose, provider string) (RouteBinding, bool) {
	r.mu.Lock()
	set := r.routing
	r.mu.Unlock()
	if set == nil {
		return RouteBinding{}, false
	}
	return set.Resolve(purpose, provider)
}

// VerifyBindings runs the dispatch CAS pre-pass: every Provider the
// approved policies reference is re-detected and Compare-and-Swapped
// against its approved binding before any Attempt is allocated or any
// Provider process starts. A drift closes the Dispatch Gate with
// PROVIDER_PROTOCOL_BINDING_CHANGED (or PROVIDER_PROTOCOL_UNSUPPORTED)
// and nothing may start. Verified detections are cached for the pass so
// every Start/Resume compares the same verified identity.
func (r *Runtime) VerifyBindings(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	set := r.routing
	r.mu.Unlock()
	if set == nil {
		return nil
	}
	for _, p := range set.Policies {
		for _, b := range p.Bindings {
			inst, err := r.detect(ctx, b.Provider)
			if err != nil {
				return err
			}
			if err := CompareInstallation(inst, b, false); err != nil {
				return err
			}
			// Cache the verified installation for the pass: every
			// Start/Resume compares the same verified identity instead
			// of re-detecting per operation.
			r.mu.Lock()
			r.detectCache[b.Provider] = inst
			r.mu.Unlock()
		}
	}
	return nil
}

// detect returns the verified Installation of one Provider: the cached
// pre-pass result when one exists (the dispatch CAS already proved it
// against the approved binding), else a fresh Detect.
func (r *Runtime) detect(ctx context.Context, name string) (Installation, error) {
	r.mu.Lock()
	if inst, ok := r.detectCache[name]; ok {
		r.mu.Unlock()
		return inst, nil
	}
	r.mu.Unlock()
	ad := r.adapters[name]
	if ad == nil {
		return Installation{}, model.InvalidInputFault("no adapter bound for provider " + name)
	}
	return ad.Detect(ctx)
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
	binding, ad, err := r.resolveProvider(ctx, req.Purpose, req.Provider, false)
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
		sessionID:  req.SessionID,
		promptHash: promptHash,
		inputHash:  inputHash,
		runID:      model.RunID(r.ids(model.IDRun)),
		startedAt:  r.now(),
	})
}

// ---------------------------------------------------------------------------
// Managed bootstrap (design §9.1, TUI task 12)
// ---------------------------------------------------------------------------

// Bootstrap runs the managed Provider start/bootstrap of one native
// interactive discussion Session: the Runtime resolves the approved
// Provider binding (fail closed on drift), starts the Provider, captures
// the Provider's OWN session identity from the validated session_started
// event, and stops the start run — the interactive terminal continues the
// exact Session later through the Native Session Bridge. The bootstrap
// only establishes the context and Session; it never represents the
// discussion as complete. It fails closed when the Provider returns no
// session id or the binding drifts, and it never uses a CFlow Session id
// as the Provider identity.
func (r *Runtime) Bootstrap(ctx context.Context, req BootstrapRequest) (*BootstrapResult, error) {
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
	if req.SessionID == "" {
		return nil, model.InvalidInputFault("a bootstrap requires the CFlow session identity")
	}
	binding, ad, err := r.resolveProvider(ctx, req.Purpose, req.Provider, false)
	if err != nil {
		return nil, err
	}
	arun, err := ad.Start(ctx, StartRequest{
		Purpose:   req.Purpose,
		Provider:  req.Provider,
		Prompt:    req.Prompt,
		Input:     req.Input,
		CWD:       req.CWD,
		SessionID: req.SessionID,
	})
	if err != nil {
		return nil, err
	}
	red := security.NewRedactor(r.redaction)
	seq := newProtocolSequence(binding, "")
	cfg := drainConfig{
		binding:   binding,
		purpose:   req.Purpose,
		provider:  req.Provider,
		sessionID: req.SessionID,
		runID:     model.RunID(r.ids(model.IDRun)),
		startedAt: r.now(),
	}
	lr := &liveRun{}
	r.mu.Lock()
	r.live[cfg.runID] = lr
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.live, cfg.runID)
		r.mu.Unlock()
	}()

	var session *sessionRecord
	for {
		ev, err := arun.Next(ctx)
		if err != nil {
			if session == nil {
				// Only a clean stream end means the Provider supplied no
				// session identity. Preserve process, protocol, auth, and
				// context errors so the caller can act on the real cause.
				if !errors.Is(err, io.EOF) {
					var crash *ProcessCrash
					if errors.As(err, &crash) {
						message, redErr := r.redactCrashMessage(crash.Message)
						if redErr != nil {
							return nil, redErr
						}
						return nil, &ProcessCrash{ExitCode: crash.ExitCode, Message: message}
					}
					return nil, err
				}
				return nil, model.NewFault(model.CodeProviderSessionIDMissing,
					"the discussion bootstrap ended before establishing the provider session id")
			}
			break
		}
		if err := seq.accept(&ev); err != nil {
			return nil, err
		}
		if ev.Type == EventSessionStarted {
			if ev.SessionID == "" {
				return nil, model.NewFault(model.CodeProviderSessionIDMissing,
					"the provider session_started event carried no session id")
			}
			session, err = r.establish(ctx, cfg, ev.SessionID)
			if err != nil {
				return nil, err
			}
			r.mu.Lock()
			session.runID = cfg.runID
			session.session.Status = model.SessionActive
			lr.setHandle(RunHandle{RunID: cfg.runID, Session: session.session.ID, ProviderSessionID: ev.SessionID})
			r.mu.Unlock()
			redEv, err := redactEvent(red, &ev)
			if err != nil {
				return nil, err
			}
			if err := r.persistEvent(ctx, cfg, session, redEv); err != nil {
				return nil, err
			}
			// Stop the start run: the interactive terminal continues the
			// exact Session through the Bridge. The adapter's controlled
			// stop unregisters the run at its next event boundary. A stop
			// failure fails the bootstrap closed (the Session was already
			// established; the caller must not proceed as if the start run
			// is fully stopped).
			lr.cancel()
			if err := ad.Cancel(ctx, RunHandle{RunID: cfg.runID, Session: session.session.ID, ProviderSessionID: ev.SessionID}); err != nil {
				return nil, err
			}
			break
		}
		// A frame before the start event (bounded redacted evidence only;
		// the protocol sequence already validated it).
		redEv, err := redactEvent(red, &ev)
		if err != nil {
			return nil, err
		}
		if err := r.persistEvent(ctx, cfg, session, redEv); err != nil {
			return nil, err
		}
	}
	if session == nil {
		return nil, model.NewFault(model.CodeProviderSessionIDMissing,
			"the discussion bootstrap established no provider session")
	}
	return &BootstrapResult{Session: session.session, Provider: req.Provider}, nil
}

// redactCrashMessage applies the Runtime's authoritative redaction policy
// before a Provider diagnostic can be persisted or shown to a user.
func (r *Runtime) redactCrashMessage(message string) (string, error) {
	red := security.NewRedactor(r.redaction)
	return redactText(red, message)
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
	if req.Prompt == "" {
		return nil, model.InvalidInputFault("prompt is required")
	}
	r.mu.Lock()
	rec := r.byProvider[req.ProviderSessionID]
	r.mu.Unlock()
	if rec == nil {
		return nil, model.InvalidInputFault("provider session not found for resume")
	}
	// The Session's Provider is a ledger fact (the hydrated record from
	// the Store); the request may name it or leave it to the ledger. An
	// unknown ledger provider fails closed.
	provider := req.Provider
	if provider == "" {
		provider = rec.provider
	}
	if provider == "" {
		return nil, model.InvalidInputFault("provider is required")
	}
	req.Provider = provider
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
	// Native Resume is only attempted when the exact binding supports
	// Resume (per-operation capability, PRD 已确认: structured output and
	// Resume are never inferred across operations). A binding without the
	// Resume capabilities makes the Session unrecoverable by definition:
	// the approved fallback applies when one is bound, PROVIDER_PROTOCOL_
	// UNSUPPORTED otherwise.
	if r.routing != nil {
		rb, ok := r.routing.Resolve(req.Purpose, req.Provider)
		if !ok {
			return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
				"the session's provider is not approved for the purpose")
		}
		if !hasCaps(rb.ResumeCapabilities, requiredResumeCapabilities) {
			return r.resumeFallback(ctx, req, rec)
		}
	}
	binding, ad, err := r.resolveProvider(ctx, req.Purpose, req.Provider, true)
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

// resumeFallback implements design 14.4 steps 1-4 plus the approved
// fallback resolution (Task 16): the ordered next binding of the
// Purpose's immutable RoutingPolicy, never a silently chosen Provider
// (PRD 约束 306).
func (r *Runtime) resumeFallback(ctx context.Context, req ResumeRequest, rec *sessionRecord) (*ResumeResult, error) {
	// 0. Resolve the approved fallback binding before any mutation: an
	// unapproved fallback fails closed and preserves the original Session
	// (it is never marked LOST and no Context Bundle is created).
	provider := req.Provider
	if r.routing != nil {
		fb, ok := r.routing.Fallback(req.Purpose, req.Provider)
		if !ok {
			return nil, model.NewFault(model.CodeProviderProtocolUnsupported,
				"no approved fallback binding for the purpose")
		}
		provider = fb.Provider
	}

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
	// capabilities of the approved fallback binding (Compare-and-Swap).
	if _, _, err := r.resolveProvider(ctx, req.Purpose, provider, false); err != nil {
		return nil, err
	}

	// 4. Create the successor Session with supersedes_session_id, keeping
	// the original purpose (the role lineage), on the approved fallback
	// Provider.
	r.mu.Lock()
	succ := &sessionRecord{
		session: model.Session{
			ID:         model.SessionID(r.ids(model.IDSession)),
			Purpose:    rec.session.Purpose,
			Status:     model.SessionStarting,
			Supersedes: rec.session.ID,
			Provider:   provider,
		},
		provider: provider,
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
// a fresh Detect result Compare-and-Swaps the binding. When the Runtime
// carries the immutable Routing Policy Set of an Execution Approval, the
// approved per-Purpose binding is resolved first and the Compare-and-Swap
// covers the full approved identity — executable path, sha256, CLI
// version, dialect, registry revision, and the operation's capability
// list (Task 16, PRD 约束 306). Without a policy (commands outside an
// approved workflow) the registry-level checks apply. Mismatches block
// before any Provider call and no Attempt is allocated.
func (r *Runtime) resolveProvider(ctx context.Context, purpose model.AgentPurpose, name string, resume bool) (ProviderBinding, Adapter, error) {
	if err := ctx.Err(); err != nil {
		return ProviderBinding{}, nil, err
	}
	binding, err := r.registry.Select(name)
	if err != nil {
		return ProviderBinding{}, nil, model.NewFault(model.CodeProviderProtocolUnsupported,
			"provider cannot be selected: "+err.Error())
	}
	r.mu.Lock()
	routing := r.routing
	r.mu.Unlock()
	if routing != nil {
		// The approved per-Purpose binding of the immutable Routing
		// Policy Set: the Compare-and-Swap covers the full approved
		// identity — executable path, sha256, CLI version, dialect,
		// registry revision, and the operation's capability list. The
		// detection is the pre-pass's cached verified result when one
		// exists (the dispatch CAS already proved it), a fresh Detect
		// otherwise (design 14.4 step 2: an unrecoverable Resume
		// validates the fallback binding this way).
		rb, ok := routing.Resolve(purpose, name)
		if !ok {
			return ProviderBinding{}, nil, model.NewFault(model.CodeProviderProtocolUnsupported,
				"the route is not approved for the purpose")
		}
		ad := r.adapters[name]
		if ad == nil {
			return ProviderBinding{}, nil, model.InvalidInputFault("no adapter bound for provider " + name)
		}
		inst, err := r.detect(ctx, name)
		if err != nil {
			return ProviderBinding{}, nil, err
		}
		if err := CompareInstallation(inst, rb, resume); err != nil {
			return ProviderBinding{}, nil, err
		}
		return binding, ad, nil
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
