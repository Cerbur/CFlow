package decision

import (
	"encoding/json"
	"fmt"
	"time"

	"cflow.local/cflow/internal/model"
)

// The protected Apply protocol (PRD 已确认：显式受保护 Apply, design 15.5):
// Apply is a SEPARATE Attempt after Workflow completion that never
// reopens the completed Workflow state. The request records the Target
// HEAD, the Integration HEAD, and the Commit Policy facts; the staging
// runs only in an isolated Apply Worktree (the executor's concern); the
// independent Apply Verification Session's verdict is judged here; the
// explicit delivery re-binds the exact facts; and the final
// compare-and-swap fast-forward result is settled from the observed
// actual Target ref. A failure never changes the Target Branch or the
// completed Workflow state. A Target Drift always starts a NEW Attempt
// from the new heads; the old verification conclusions are never reused.
//
// Commit Policy drift (PRD 约束 40-41) blocks the Attempt with
// COMMIT_POLICY_CONFIRMATION_REQUIRED before any staging; the explicit
// confirmation binds the exact Attempt, the Target/Integration heads,
// and the fresh Preflight Revision/hash/fingerprint, and any later
// change voids the input. A Target Drift that changed the
// Wrapper/Manifest/Executable identity blocks with COMMAND_IDENTITY_CHANGED
// and only an append-only APPLY_CATALOG approval of the newly
// discovered, validated, and fixed Catalog Revision may continue
// (PRD 已确认：Apply Command Identity Drift).

// decideApply handles the user Apply interaction: the request (Prepare)
// and the explicit delivery (Execute) are the closed command kinds; the
// policy/catalog confirmation is the separate ApplyPolicyConfirmation
// input.
func decideApply(state model.State, in model.ApplyCommandInput) (model.Decision, error) {
	switch in.Kind {
	case model.ApplyRequest:
		return applyRequest(state, in)
	case model.ApplyExecute:
		return applyExecute(state, in)
	default:
		return model.Decision{}, model.InvalidInputFault("unsupported apply command")
	}
}

// applyRequest opens (or re-opens) one Apply Attempt against a completed
// Workflow. The Integration output may not come from a quarantined
// Branch. The attempt records the exact Target/Integration HEAD and the
// Commit Policy Preflight facts its delivery must re-bind, allocates the
// independent Apply Verification Session, and requests the isolated
// staging. When the recorded Commit Policy fingerprint differs from the
// approved Execution facts, the attempt blocks at
// COMMIT_POLICY_CONFIRMATION_REQUIRED before any staging (PRD 约束 40-41).
func applyRequest(state model.State, in model.ApplyCommandInput) (model.Decision, error) {
	if state.Workflow.Stage != model.StageCompleted || state.Workflow.Runtime != model.RuntimeSucceeded {
		return model.Decision{}, model.InvalidInputFault("apply requires a completed workflow")
	}
	if branchQuarantined(state, state.Workflow.IntegrationBranch) {
		return model.Decision{}, model.NewFault(model.CodeCommitDuringPolicyDriftWindow,
			"a quarantined integration branch can never re-enter Apply")
	}
	if in.TargetHead == "" || in.IntegrationHead == "" {
		return model.Decision{}, model.InvalidInputFault("apply requires target and integration HEAD values")
	}
	if in.PreflightHash == "" || in.Fingerprint == "" {
		return model.Decision{}, model.InvalidInputFault("apply requires the commit-policy preflight facts")
	}
	if in.ReviewSession == "" || in.ReviewRoute == "" || in.ReviewProcess == "" {
		return model.Decision{}, model.InvalidInputFault("apply requires the independent apply verification session allocation")
	}
	b := &builder{state: state}
	last := lastApplyAttempt(state)
	att, created, stagingWillRun := applyAttemptFor(state, in, last, b)
	_ = created
	if !stagingWillRun {
		// A request against an already verified or in-flight attempt is a
		// no-op: no session allocation is appended (the delivery runs
		// through ExecuteApply).
		return b.decision(), nil
	}
	// The policy gate runs before the session/process allocations: a gate
	// block never allocates — and never orphans — the independent Apply
	// Verification Session or the ONE restricted Merge Resolution Session
	// (a blocked decision records no SessionStarting/Running rows). The
	// gate blocks when the attempt's recorded Commit Policy fingerprint
	// differs from the Execution Approval's, unless an append-only
	// confirmation already bound this exact attempt (PRD 约束 40-41: the
	// approval is the immutable record; a later re-drift still fails
	// closed in the staging executor, which revalidates the live
	// fingerprint). The Workflow stays COMPLETED and the Target unchanged;
	// an attempt that already passed its staging is never re-blocked.
	if facts := state.Workflow.ExecutionFacts; facts != nil && facts.Fingerprint != "" && att.Fingerprint != facts.Fingerprint {
		if !applyConfirmationRecorded(state, att.ID) {
			b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyBlocked, EndedAt: state.Now})
			b.event(model.EventApplyBlocked, "", model.AttemptKey{}, model.CodeCommitPolicyConfirmationRequired,
				"commit policy drifted since the execution approval; apply blocked until the exact confirmation")
			return b.decision(), nil
		}
	}
	if !sessionExists(state, in.ReviewSession) {
		// The independent Apply Verification Session is appended once per
		// staging run: a fresh allocation for a new or re-opened attempt,
		// the persisted SessionStarting record for an interrupted staging
		// re-run.
		if err := validateFreshSession(state, in.ReviewSession); err != nil {
			return model.Decision{}, err
		}
		b.mutate(model.SessionAppendMutation{Session: model.Session{
			ID: in.ReviewSession, Purpose: model.PurposeApplyVerification, Status: model.SessionStarting,
			Provider: in.ReviewRoute,
		}, Provider: in.ReviewRoute})
		b.mutate(model.ProcessAppendMutation{Process: model.ProcessRecord{
			ID: in.ReviewProcess, Session: in.ReviewSession, Purpose: model.PurposeApplyVerification,
			Status: model.ProcessStatusRunning, StartedAt: state.Now,
		}})
	}
	if in.ResolutionSession != "" {
		if err := validateFreshSession(state, in.ResolutionSession); err != nil {
			return model.Decision{}, err
		}
		if in.ResolutionProcess == "" {
			return model.Decision{}, model.InvalidInputFault("the resolution allocation requires a process identity")
		}
		b.mutate(model.SessionAppendMutation{Session: model.Session{
			ID: in.ResolutionSession, Purpose: model.PurposeRepair, Status: model.SessionStarting,
		}, Provider: in.ReviewRoute})
		b.mutate(model.ProcessAppendMutation{Process: model.ProcessRecord{
			ID: in.ResolutionProcess, Session: in.ResolutionSession, Purpose: model.PurposeRepair,
			Status: model.ProcessStatusRunning, StartedAt: state.Now,
		}})
	}
	b.effect(model.ApplyStagingCreateIntent{
		Apply:             att.ID,
		TargetHead:        att.TargetHead,
		IntegrationHead:   att.IntegrationHead,
		ResolutionSession: in.ResolutionSession,
	})
	return b.decision(), nil
}

// applyAttemptFor decides the attempt the request operates on: a blocked
// attempt whose recorded heads still match the fresh observations is
// re-opened (the user's explicit retry of the same attempt); an attempt
// already staging, awaiting confirmation, or running with matching heads
// is a no-op (the retry of an unsettled or verified staging; the
// delivery runs through ExecuteApply); anything else — including every
// drifted head — starts a NEW attempt from the fresh heads (PRD: a
// drifted Target always starts a new Attempt; the old verification
// conclusions are never reused). created reports whether a new attempt
// was appended; stagingWillRun reports whether the staging effect will
// run (false for the no-op of an already verified or in-flight attempt).
func applyAttemptFor(state model.State, in model.ApplyCommandInput, last *model.ApplyAttempt, b *builder) (model.ApplyAttempt, bool, bool) {
	if last != nil && in.TargetHead == last.TargetHead && in.IntegrationHead == last.IntegrationHead {
		switch last.Status {
		case model.ApplyAwaitingConfirmation, model.ApplyRunning:
			// The staging already passed and awaits the explicit delivery,
			// or the delivery is in flight: the request is a no-op.
			return *last, false, false
		case model.ApplyStaging:
			// A staging interrupted before its result: re-run the staging
			// against the same recorded facts (the executor re-observes
			// every git fact; the Apply Worktree is reused).
			att := *last
			att.ReviewSession = in.ReviewSession
			att.ReviewRoute = in.ReviewRoute
			att.ReviewProcess = in.ReviewProcess
			return att, false, true
		case model.ApplyBlocked:
			// The explicit retry of the same blocked attempt.
			att := *last
			att.Status = model.ApplyStaging
			att.EndedAt = time.Time{}
			att.ReviewSession = in.ReviewSession
			att.ReviewRoute = in.ReviewRoute
			att.ReviewProcess = in.ReviewProcess
			b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyStaging})
			b.event(model.EventApplyRestarted, "", model.AttemptKey{}, "", "apply attempt re-opened for staging")
			return att, false, true
		}
	}
	// A fresh attempt: the recorded heads are the fresh observations.
	number := len(state.ApplyAttempts) + 1
	att := model.ApplyAttempt{
		ID:              model.ApplyAttemptID(fmt.Sprintf("apply-%d", number)),
		Number:          number,
		Status:          model.ApplyStaging,
		TargetHead:      in.TargetHead,
		IntegrationHead: in.IntegrationHead,
		Preflight:       in.Preflight,
		PreflightHash:   in.PreflightHash,
		Fingerprint:     in.Fingerprint,
		ReviewSession:   in.ReviewSession,
		ReviewRoute:     in.ReviewRoute,
		ReviewProcess:   in.ReviewProcess,
		StartedAt:       state.Now,
	}
	b.mutate(model.ApplyAppendMutation{ApplyAttempt: att})
	b.event(model.EventApplyAttemptCreated, "", model.AttemptKey{}, "", "apply attempt created")
	return att, true, true
}

// decideApplyStagingResult settles one staging effect result. A passing
// staging (the isolated Apply Worktree holds the verified combined
// result and the deterministic apply verification passed) requests the
// independent Apply Verification Session; the verdict is judged by
// decideApplyReviewRunEnded. A failed staging blocks the attempt with
// the typed code and settles the never-started review Session.
func decideApplyStagingResult(state model.State, in model.EffectResultInput) (model.Decision, error) {
	att := findApplyAttempt(state, in.ApplyAttempt)
	if att == nil || att.Status != model.ApplyStaging {
		return model.Decision{}, model.InvalidInputFault("unknown or non-staging apply attempt")
	}
	b := &builder{state: state}
	switch in.Kind {
	case model.ApplyStagingSucceeded:
		if in.EndHead == "" || in.ManifestHash == "" {
			return model.Decision{}, model.InvariantFault(fmt.Errorf(
				"apply staging success carries no staging head or verification manifest"))
		}
		b.mutate(model.ApplyMutation{ID: att.ID, StagingHead: in.EndHead})
		// The independent Apply Verification Session runs next. Its
		// allocation facts are derived from the persisted Session and
		// Process records (the attempt's in-memory fields do not survive
		// the store round-trip).
		session, route, process := applyReviewFacts(state)
		b.effect(model.ProviderStartIntent{
			Session: session, Purpose: model.PurposeApplyVerification,
			Route: route, Process: process,
		})
	case model.ApplyStagingFailed:
		code := in.FailureCode
		if code == "" {
			code = model.CodeStateInvariantViolation
		}
		b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyBlocked, EndedAt: state.Now})
		b.event(model.EventApplyBlocked, "", model.AttemptKey{}, code, "apply staging blocked")
		settleApplySessionNeverStarted(b, state)
	default:
		return model.Decision{}, model.InvalidInputFault("unsupported apply staging result")
	}
	return b.decision(), nil
}

// decideApplyReviewRunEnded judges the independent Apply Verification
// Session's verdict (design 16.2: review never replaces deterministic
// verification; the Kernel judges the verdict). The review Session must
// be independent — its provider session id can never be shared — and the
// deterministic apply verification manifest must ride the result. A PASS
// leaves the attempt awaiting the explicit delivery; anything else
// blocks it. The completed Workflow is never altered.
func decideApplyReviewRunEnded(state model.State, in model.EffectResultInput, created *model.Session) (model.Decision, error) {
	for _, s := range state.Sessions {
		if s.ID != created.ID && s.ProviderSessionID != "" && s.ProviderSessionID == in.Session.ProviderSessionID {
			return model.Decision{}, model.NewFault(model.CodeSessionIndependenceViolation,
				"the apply verification session reuses an existing session's provider session id")
		}
	}
	att := applyAttemptStaging(state)
	if att == nil {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("apply review has no staging attempt"))
	}
	if in.ManifestHash == "" {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("apply review carries no deterministic verification manifest"))
	}
	verdict, err := parseReviewVerdict(in.Body)
	b := &builder{state: state}
	b.mutate(sessionEnd(state, created, in))
	// The independent Apply Verification Session settled: its managed
	// process record is stopped so the completed Workflow carries no
	// active process (the Cleanup gate, design 17.4, requires no managed
	// processes; the record remains the durable ledger of the run). The
	// stop runs through the managed stop effect and settles the record
	// from the typed stopped fact.
	if p := processBySession(state, created.ID); p != nil && p.Status == model.ProcessStatusRunning {
		b.effect(model.ManagedProcessStopIntent{Process: p.ID})
	}
	if err != nil || !verdict {
		b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyBlocked, EndedAt: state.Now})
		b.event(model.EventApplyBlocked, "", model.AttemptKey{}, model.CodeSemanticReviewFailed,
			"apply verification review failed")
		return b.decision(), nil
	}
	b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyAwaitingConfirmation})
	b.event(model.EventApplyVerified, "", model.AttemptKey{}, "", "apply staging verified")
	return b.decision(), nil
}

// decideApplyReviewFailed settles a failed or cancelled Apply
// Verification Session: the attempt blocks with the typed code and the
// completed Workflow stays untouched.
func decideApplyReviewFailed(state model.State, in model.EffectResultInput, created *model.Session, code model.Code) (model.Decision, error) {
	att := applyAttemptStaging(state)
	if att == nil {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("apply review failure has no staging attempt"))
	}
	b := &builder{state: state}
	b.mutate(sessionEnd(state, created, in))
	b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyBlocked, EndedAt: state.Now})
	b.event(model.EventApplyBlocked, "", model.AttemptKey{}, code, "apply verification review failed")
	return b.decision(), nil
}

// applyExecute is the explicit delivery (ExecuteApply): the strict
// re-bind of the Apply Attempt, the Target/Integration heads, and the
// exact Preflight facts, then the final compare-and-swap fast-forward.
// A RUNNING attempt is the crash-recovery re-delivery: the executor
// re-observes every fact from git and settles from the actual ref, so
// the re-bind is skipped (the observed Target may already be the
// delivered staging head).
func applyExecute(state model.State, in model.ApplyCommandInput) (model.Decision, error) {
	var att *model.ApplyAttempt
	if in.Attempt != "" {
		att = findApplyAttempt(state, in.Attempt)
	} else {
		att = lastApplyAttempt(state)
	}
	if att == nil {
		return model.Decision{}, model.InvalidInputFault("no apply attempt to execute")
	}
	switch att.Status {
	case model.ApplyAwaitingConfirmation:
		if in.Preflight.Workflow != "" && (in.Preflight.Workflow != att.Preflight.Workflow ||
			in.Preflight.Type != att.Preflight.Type || in.Preflight.Revision != att.Preflight.Revision ||
			in.Preflight.Hash != att.Preflight.Hash) {
			return model.Decision{}, model.NewFault(model.CodeCommitPolicyInputChanged,
				"the apply preflight reference changed since the apply staging")
		}
		if in.TargetHead != att.TargetHead || in.IntegrationHead != att.IntegrationHead {
			return model.Decision{}, model.NewFault(model.CodeTargetHeadChanged,
				"target or integration HEAD drifted since the apply staging")
		}
		if in.PreflightHash != att.PreflightHash || in.Fingerprint != att.Fingerprint {
			return model.Decision{}, model.NewFault(model.CodeCommitPolicyInputChanged,
				"commit-policy preflight facts changed since the apply staging")
		}
	case model.ApplyRunning:
		// Crash recovery: no re-bind; the executor re-observes.
	default:
		return model.Decision{}, model.InvalidInputFault(
			"apply attempt " + string(att.Status) + " cannot be executed")
	}
	b := &builder{state: state}
	b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyRunning})
	b.effect(model.ApplyFastForwardIntent{Apply: att.ID, TargetHead: att.TargetHead})
	return b.decision(), nil
}

// decideApplyResult settles the final compare-and-swap result. Success
// marks the Apply SUCCEEDED with the observed actual Target ref; failure
// blocks the attempt with the typed code. Neither ever alters the
// completed Workflow.
func decideApplyResult(state model.State, in model.EffectResultInput) (model.Decision, error) {
	att := findApplyAttempt(state, in.ApplyAttempt)
	if att == nil || att.Status != model.ApplyRunning {
		return model.Decision{}, model.InvalidInputFault("unknown or non-running apply attempt")
	}
	b := &builder{state: state}
	switch in.Kind {
	case model.ApplyFastForwardSucceeded:
		if in.ObservedHead == "" {
			return model.Decision{}, model.InvariantFault(fmt.Errorf("apply delivery carries no observed head"))
		}
		b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplySucceeded, EndedAt: state.Now, StagingHead: in.ObservedHead})
		b.event(model.EventApplySucceeded, "", model.AttemptKey{}, "", "apply delivered "+in.ObservedHead)
	case model.ApplyFastForwardFailed:
		code := in.FailureCode
		if code == "" {
			code = model.CodeTargetHeadChanged
		}
		b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyBlocked, EndedAt: state.Now})
		b.event(model.EventApplyBlocked, "", model.AttemptKey{}, code, "apply delivery blocked")
	default:
		return model.Decision{}, model.InvalidInputFault("unsupported apply delivery result")
	}
	return b.decision(), nil
}

// decideApplyPolicyConfirm is the user's explicit confirmation of a
// blocked Apply Attempt (PRD 约束 40-41, Apply Command Identity Drift):
// it binds the exact Apply Attempt, the Target/Integration heads, and
// the fresh Preflight Revision/hash/fingerprint. Any change since the
// attempt was recorded voids the input. When the block was a
// COMMAND_IDENTITY_CHANGED the confirmation carries the newly
// discovered, validated, and fixed Apply Verification Catalog Revision
// and records the append-only APPLY_CATALOG approval; otherwise it
// records the Commit Policy confirmation. The attempt then re-opens for
// a full staging re-run; the Workflow's completed state and its history
// Execution Approval are never changed.
func decideApplyPolicyConfirm(state model.State, in model.ApplyPolicyConfirmationInput) (model.Decision, error) {
	att := findApplyAttempt(state, in.Attempt)
	if att == nil || att.Status != model.ApplyBlocked {
		return model.Decision{}, model.InvalidInputFault("no blocked apply attempt to confirm")
	}
	if in.TargetHead != att.TargetHead || in.IntegrationHead != att.IntegrationHead {
		return model.Decision{}, model.NewFault(model.CodeTargetHeadChanged,
			"a bound HEAD changed since the apply attempt was recorded; the confirmation input is void")
	}
	if in.Fingerprint != att.Fingerprint {
		return model.Decision{}, model.NewFault(model.CodeCommitPolicyInputChanged,
			"the commit policy changed again; the confirmation input is void")
	}
	if in.PreflightHash == "" || in.Fingerprint == "" || in.Preflight.Revision < 1 || in.Preflight.Hash == "" {
		return model.Decision{}, model.InvalidInputFault(
			"the confirmation requires the exact new preflight revision, hash, and fingerprint")
	}
	b := &builder{state: state}
	kind := model.ApprovalCommitPolicy
	refs := []model.ArtifactRef{in.Preflight}
	ctx := map[string]string{
		"attempt": string(in.Attempt), "target_head": in.TargetHead,
		"integration_head": in.IntegrationHead,
	}
	if in.CatalogRef.Revision >= 1 && in.CatalogRef.Hash != "" {
		kind = model.ApprovalApplyCatalog
		ctx["catalog_revision"] = fmt.Sprintf("%d", in.CatalogRef.Revision)
		ctx["catalog_hash"] = in.CatalogRef.Hash
		refs = append(refs, model.ArtifactRef{
			Workflow: state.Workflow.ID, Type: model.ArtifactCatalog,
			Revision: in.CatalogRef.Revision, Hash: in.CatalogRef.Hash,
		})
	}
	decisionContext, _ := json.Marshal(ctx)
	b.mutate(model.ApprovalAppendMutation{Approval: model.Approval{
		ID:                model.ApprovalID(fmt.Sprintf("approval-%d", len(state.Approvals)+1)),
		Kind:              kind,
		Seq:               state.NextEventSeq,
		Refs:              refs,
		Fingerprint:       in.Fingerprint,
		PreflightRevision: in.Preflight.Revision,
		DecisionContext:   string(decisionContext),
	}})
	b.mutate(model.ApplyConfirmationMutation{
		ID: att.ID, Preflight: in.Preflight, PreflightHash: in.PreflightHash, Fingerprint: in.Fingerprint,
	})
	b.event(model.EventCommitPolicyConfirmed, "", model.AttemptKey{}, "", "apply policy confirmation recorded")
	// The attempt re-opens and the full staging re-runs with the
	// confirmed facts (the executor revalidates the fingerprint and the
	// Catalog identity against the confirmed references). The
	// confirmation's staging run carries its own independent Apply
	// Verification Session allocation (the blocked staging's session was
	// settled cancelled; a fresh one is appended).
	if !sessionExists(state, in.ReviewSession) {
		if err := validateFreshSession(state, in.ReviewSession); err != nil {
			return model.Decision{}, err
		}
		b.mutate(model.SessionAppendMutation{Session: model.Session{
			ID: in.ReviewSession, Purpose: model.PurposeApplyVerification, Status: model.SessionStarting,
			Provider: in.ReviewRoute,
		}, Provider: in.ReviewRoute})
		b.mutate(model.ProcessAppendMutation{Process: model.ProcessRecord{
			ID: in.ReviewProcess, Session: in.ReviewSession, Purpose: model.PurposeApplyVerification,
			Status: model.ProcessStatusRunning, StartedAt: state.Now,
		}})
	}
	// The confirmation's staging re-run mirrors the request's ONE
	// restricted Merge Resolution allocation (the command builder mirrors
	// applyResolutionNeeded): a worktree that still holds a conflicted
	// merge gets the resolution session, so the confirm itself can
	// complete the conflict without an extra retry.
	if in.ResolutionSession != "" {
		if err := validateFreshSession(state, in.ResolutionSession); err != nil {
			return model.Decision{}, err
		}
		if in.ResolutionProcess == "" {
			return model.Decision{}, model.InvalidInputFault("the resolution allocation requires a process identity")
		}
		b.mutate(model.SessionAppendMutation{Session: model.Session{
			ID: in.ResolutionSession, Purpose: model.PurposeRepair, Status: model.SessionStarting,
		}, Provider: in.ReviewRoute})
		b.mutate(model.ProcessAppendMutation{Process: model.ProcessRecord{
			ID: in.ResolutionProcess, Session: in.ResolutionSession, Purpose: model.PurposeRepair,
			Status: model.ProcessStatusRunning, StartedAt: state.Now,
		}})
	}
	b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyStaging})
	b.event(model.EventApplyRestarted, "", model.AttemptKey{}, "", "apply attempt re-opened after the confirmation")
	b.effect(model.ApplyStagingCreateIntent{
		Apply: att.ID, TargetHead: att.TargetHead, IntegrationHead: att.IntegrationHead,
		ResolutionSession: in.ResolutionSession,
	})
	return b.decision(), nil
}

// settleApplySessionNeverStarted settles the apply-verification Session
// and Process records the request allocated when the staging failed
// before the review could run (derived from the persisted records).
func settleApplySessionNeverStarted(b *builder, state model.State) {
	session, _, process := applyReviewFacts(state)
	if session != "" {
		b.mutate(model.SessionEndMutation{ID: session, Status: model.SessionCancelled, EndedAt: state.Now})
	}
	if process != "" {
		b.mutate(model.ProcessEndMutation{ID: process, Status: model.ProcessStatusStopped, EndedAt: state.Now})
	}
}

// sessionExists reports whether a Session identity is already recorded.
func sessionExists(state model.State, id model.SessionID) bool {
	for _, s := range state.Sessions {
		if s.ID == id {
			return true
		}
	}
	return false
}

// applyConfirmationRecorded reports whether an append-only Commit Policy
// or APPLY_CATALOG approval already bound the exact Apply Attempt (PRD
// 约束 40-41, Apply Command Identity Drift). The confirmation is the
// immutable record; the request's policy gate must never re-block a
// confirmed attempt — a later re-drift still fails closed in the staging
// executor, which revalidates the live fingerprint against the attempt.
func applyConfirmationRecorded(state model.State, att model.ApplyAttemptID) bool {
	for i := len(state.Approvals) - 1; i >= 0; i-- {
		ap := state.Approvals[i]
		if ap.Kind != model.ApprovalCommitPolicy && ap.Kind != model.ApprovalApplyCatalog {
			continue
		}
		var ctx map[string]string
		if err := json.Unmarshal([]byte(ap.DecisionContext), &ctx); err != nil {
			continue
		}
		if ctx["attempt"] == string(att) {
			return true
		}
	}
	return false
}

// applyReviewFacts derives the independent Apply Verification Session
// allocation from the persisted records: the latest apply-verification
// Session that has not started and its Provider route, plus the matching
// running Process.
func applyReviewFacts(state model.State) (model.SessionID, string, model.ProcessID) {
	var session model.SessionID
	route := ""
	for i := range state.Sessions {
		if state.Sessions[i].Purpose == model.PurposeApplyVerification &&
			state.Sessions[i].Status == model.SessionStarting {
			session = state.Sessions[i].ID
			route = state.Sessions[i].Provider
		}
	}
	var process model.ProcessID
	for i := range state.Processes {
		if state.Processes[i].Purpose == model.PurposeApplyVerification &&
			state.Processes[i].Status == model.ProcessStatusRunning {
			process = state.Processes[i].ID
		}
	}
	return session, route, process
}

// applyAttemptStaging returns the apply attempt currently in the staging
// phase (nil when none).
func applyAttemptStaging(state model.State) *model.ApplyAttempt {
	for i := range state.ApplyAttempts {
		if state.ApplyAttempts[i].Status == model.ApplyStaging {
			return &state.ApplyAttempts[i]
		}
	}
	return nil
}

func lastApplyAttempt(state model.State) *model.ApplyAttempt {
	if len(state.ApplyAttempts) == 0 {
		return nil
	}
	return &state.ApplyAttempts[len(state.ApplyAttempts)-1]
}

func findApplyAttempt(state model.State, id model.ApplyAttemptID) *model.ApplyAttempt {
	for i := range state.ApplyAttempts {
		if state.ApplyAttempts[i].ID == id {
			return &state.ApplyAttempts[i]
		}
	}
	return nil
}

// processBySession returns the managed process record bound to one
// Session (nil when none exists).
func processBySession(state model.State, session model.SessionID) *model.ProcessRecord {
	for i := range state.Processes {
		if state.Processes[i].Session == session {
			return &state.Processes[i]
		}
	}
	return nil
}

// decideCancel is the recoverable cancellation protocol: the intent is
// persisted and dispatch closes first; the terminal CANCELLED Decision is
// committed only after all managed processes settle and facts reconcile
// (PRD 状态机与持久化模型, design 17.4). Cancel never allocates a Retry,
// starts a Provider, or generates an Artifact (design 6.1).
func decideCancel(state model.State, in model.WorkflowCommandInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to cancel")
	}
	if state.Workflow.Runtime.IsTerminal() {
		return model.Decision{}, model.InvalidInputFault("workflow is already terminal")
	}
	b := &builder{state: state}
	intent := &model.CancelIntent{RequestedSeq: state.NextEventSeq, Reason: in.Reason}
	b.mutate(wfMut(state, state.Workflow.Stage, state.Workflow.Runtime, intent))
	b.event(model.EventWorkflowCancelRequested, "", model.AttemptKey{}, "", "cancel requested")
	if run := activeRun(state); run != nil && !run.Status.IsTerminal() {
		b.mutate(model.RunMutation{ID: run.ID, Status: model.RunStopping, DispatchGate: false})
		b.event(model.EventRunStopped, "", model.AttemptKey{}, "", "run stopping")
	}
	stopRunningProcesses(b, state)
	if !hasRunningAttempt(state) && !hasRunningProcess(state) {
		finishCancel(b, state, intent)
	}
	return b.decision(), nil
}

// decideCleanup handles the user Cleanup interaction: an immutable Dry
// Run Manifest first, then an execution that revalidates every item's
// facts against the exact confirmed Manifest (design 17.4).
func decideCleanup(state model.State, in model.CleanupCommandInput) (model.Decision, error) {
	switch in.Kind {
	case model.CleanupDryRun:
		return cleanupDryRun(state, in)
	case model.CleanupExecute:
		return cleanupExecute(state, in)
	default:
		return model.Decision{}, model.InvalidInputFault("unsupported cleanup command")
	}
}

// cleanupDryRun produces the immutable Manifest over the candidate target
// set. Cleanup targets require terminal Workflow state and no managed
// processes; the Manifest's identity and hash are fixed here and the
// execution confirmation must bind them exactly.
func cleanupDryRun(state model.State, in model.CleanupCommandInput) (model.Decision, error) {
	if !state.Workflow.Runtime.IsTerminal() {
		return model.Decision{}, model.NewFault(model.CodeCleanupWorkflowNotTerminal,
			"cleanup requires a terminal workflow")
	}
	if hasRunningProcess(state) {
		return model.Decision{}, model.NewFault(model.CodeCleanupActiveProcess,
			"cleanup requires no managed processes")
	}
	b := &builder{state: state}
	items := append([]model.CleanupItem(nil), in.Items...)
	for i := range items {
		items[i].Status = model.CleanupItemPending
	}
	att := model.CleanupAttempt{
		ID:     model.CleanupAttemptID(fmt.Sprintf("cleanup-%d", len(state.CleanupAttempts)+1)),
		Status: model.CleanupStatusAwaitingConfirmation,
		Manifest: model.ArtifactRef{
			Workflow: state.Workflow.ID,
			Type:     model.ArtifactCleanupManifest,
			Revision: 1,
			Hash:     model.CleanupManifestHash(items),
		},
		Items:     items,
		StartedAt: state.Now,
	}
	b.mutate(model.CleanupAppendMutation{CleanupAttempt: att})
	b.event(model.EventCleanupAttemptCreated, "", model.AttemptKey{}, "", "cleanup manifest created")
	return b.decision(), nil
}

// cleanupExecute is the explicit confirmation and execution: the app
// re-observes every item's facts and the Kernel revalidates them against
// the exact confirmed Manifest (design 17.4). The first execution
// (AWAITING_CONFIRMATION) revalidates the manifest hash and every item's
// facts — any drift is CLEANUP_FACT_MISMATCH, a dirty target is
// CLEANUP_TARGET_DIRTY — then requests the first pending item. A RUNNING
// attempt is the crash recovery: an interruption left an item REQUESTED,
// so the first unsettled item is re-requested and the executor re-observes
// every fact per item and settles from the actual state (an already-removed
// target reports Removed; a still-present target is re-checked and removed;
// nothing beyond the already-Requested set is ever started).
func cleanupExecute(state model.State, in model.CleanupCommandInput) (model.Decision, error) {
	att := lastCleanupAttempt(state)
	if att == nil {
		return model.Decision{}, model.InvalidInputFault("no cleanup attempt to execute")
	}
	// The confirmation binds the exact Manifest identity and hash; a
	// changed Manifest is CLEANUP_FACT_MISMATCH with no deletion.
	if in.Manifest != att.Manifest {
		return model.Decision{}, model.NewFault(model.CodeCleanupFactsChanged,
			"cleanup manifest identity or hash changed since the dry run")
	}
	if !state.Workflow.Runtime.IsTerminal() {
		return model.Decision{}, model.NewFault(model.CodeCleanupWorkflowNotTerminal,
			"cleanup requires a terminal workflow")
	}
	if hasRunningProcess(state) {
		return model.Decision{}, model.NewFault(model.CodeCleanupActiveProcess,
			"cleanup requires no managed processes")
	}
	// The execution re-confirms no Project Mutation Quarantine and no
	// active Apply (PRD 已确认：Cleanup 仅删除安全干净的衍生目录; the Workflow
	// is terminal, but a quarantine or an in-flight Apply makes the facts
	// unsafe to remove).
	if projectMutationQuarantined(state) {
		return model.Decision{}, model.NewFault(model.CodeCleanupQuarantined,
			"cleanup requires no project mutation quarantine")
	}
	if hasActiveApply(state) {
		return model.Decision{}, model.NewFault(model.CodeCleanupActiveApply,
			"cleanup requires no in-flight apply attempt")
	}
	var next *model.CleanupItem
	switch att.Status {
	case model.CleanupStatusAwaitingConfirmation:
		if model.CleanupManifestHash(in.Items) != att.Manifest.Hash {
			return model.Decision{}, model.NewFault(model.CodeCleanupFactsChanged,
				"observed cleanup facts no longer match the confirmed manifest")
		}
		manifestItems := map[int]model.CleanupItem{}
		for _, it := range att.Items {
			manifestItems[it.Index] = it
		}
		for _, it := range in.Items {
			mi, ok := manifestItems[it.Index]
			if !ok || it.Kind != mi.Kind || it.CanonicalPath != mi.CanonicalPath ||
				it.Branch != mi.Branch || it.ExpectedHead != mi.ExpectedHead || it.Fingerprint != mi.Fingerprint {
				return model.Decision{}, model.NewFault(model.CodeCleanupFactsChanged,
					"cleanup item facts drifted from the confirmed manifest")
			}
			if it.Dirty {
				return model.Decision{}, model.NewFault(model.CodeCleanupTargetDirty,
					"cleanup target is dirty")
			}
		}
		next = firstPendingItem(att)
		if next == nil {
			return model.Decision{}, model.InvalidInputFault("cleanup manifest has no pending items")
		}
	case model.CleanupStatusRunning:
		// Crash recovery: re-request the first unsettled item. The executor
		// re-observes every fact per item and settles from the actual
		// state — an already-removed target reports Removed without
		// pretending it was deleted now; a still-present target is
		// re-validated and removed (design 17.4 partial recovery).
		next = firstUnsettledItem(att)
		if next == nil {
			return model.Decision{}, model.InvalidInputFault(
				"cleanup manifest has no unsettled items to recover")
		}
	default:
		return model.Decision{}, model.InvalidInputFault(
			"cleanup attempt " + string(att.Status) + " cannot be executed")
	}
	b := &builder{state: state}
	b.mutate(model.CleanupMutation{ID: att.ID, Status: model.CleanupStatusRunning})
	b.mutate(model.CleanupItemMutation{Attempt: att.ID, Index: next.Index, Status: model.CleanupItemRequested})
	b.event(model.EventCleanupItemRequested, "", model.AttemptKey{}, "", "cleanup item requested")
	requestCleanupEffect(b, att, next)
	return b.decision(), nil
}

// decideCleanupResult settles one item's independent result. A removed
// item completes and the next pending item is requested; when no item
// remains pending the Cleanup Attempt Succeeds. A failed item Blocks the
// Attempt with partial results explicit, and never alters the Workflow's
// terminal state (PRD Cleanup Failure Codes).
func decideCleanupResult(state model.State, in model.EffectResultInput) (model.Decision, error) {
	att := findCleanupAttempt(state, in.CleanupAttempt)
	if att == nil {
		return model.Decision{}, model.InvalidInputFault("unknown cleanup attempt")
	}
	if in.ItemIndex < 0 || in.ItemIndex >= len(att.Items) {
		return model.Decision{}, model.InvalidInputFault("cleanup item index out of range")
	}
	item := &att.Items[in.ItemIndex]
	if item.Status != model.CleanupItemRequested {
		return model.Decision{}, model.InvalidInputFault("cleanup item is not requested")
	}
	b := &builder{state: state}
	switch in.Kind {
	case model.CleanupItemRemovedResult:
		b.mutate(model.CleanupItemMutation{Attempt: att.ID, Index: in.ItemIndex, Status: model.CleanupItemCompleted})
		b.event(model.EventCleanupItemCompleted, "", model.AttemptKey{}, "", "cleanup item completed")
		if next := firstPendingItem(att); next != nil {
			b.mutate(model.CleanupItemMutation{Attempt: att.ID, Index: next.Index, Status: model.CleanupItemRequested})
			b.event(model.EventCleanupItemRequested, "", model.AttemptKey{}, "", "cleanup item requested")
			requestCleanupEffect(b, att, next)
		} else {
			b.mutate(model.CleanupMutation{ID: att.ID, Status: model.CleanupStatusSucceeded, EndedAt: state.Now})
		}
	case model.CleanupItemFailedResult:
		b.mutate(model.CleanupItemMutation{Attempt: att.ID, Index: in.ItemIndex, Status: model.CleanupItemFailed, FailureCode: in.FailureCode})
		b.event(model.EventCleanupItemFailed, "", model.AttemptKey{}, "", "cleanup item failed")
		b.mutate(model.CleanupMutation{ID: att.ID, Status: model.CleanupStatusBlocked, EndedAt: state.Now})
	}
	return b.decision(), nil
}

func lastCleanupAttempt(state model.State) *model.CleanupAttempt {
	if len(state.CleanupAttempts) == 0 {
		return nil
	}
	return &state.CleanupAttempts[len(state.CleanupAttempts)-1]
}

func findCleanupAttempt(state model.State, id model.CleanupAttemptID) *model.CleanupAttempt {
	for i := range state.CleanupAttempts {
		if state.CleanupAttempts[i].ID == id {
			return &state.CleanupAttempts[i]
		}
	}
	return nil
}

func firstPendingItem(att *model.CleanupAttempt) *model.CleanupItem {
	for i := range att.Items {
		if att.Items[i].Status == model.CleanupItemPending {
			return &att.Items[i]
		}
	}
	return nil
}

// firstUnsettledItem returns the first item that is not terminal
// (REQUESTED in flight or PENDING), the crash-recovery continuation point
// of a RUNNING Cleanup Attempt. Terminal items — COMPLETED or FAILED —
// are never reopened.
func firstUnsettledItem(att *model.CleanupAttempt) *model.CleanupItem {
	for i := range att.Items {
		if !att.Items[i].Status.IsTerminal() {
			return &att.Items[i]
		}
	}
	return nil
}

func requestCleanupEffect(b *builder, att *model.CleanupAttempt, item *model.CleanupItem) {
	if item.Kind == model.CleanupWorktree {
		b.effect(model.CleanupWorktreeRemoveIntent{Cleanup: att.ID, Item: item.Index})
	} else {
		b.effect(model.CleanupScratchRemoveIntent{Cleanup: att.ID, Item: item.Index})
	}
}
