package decision

// The verification, review, and serial Integration merge decisions
// (Task 13, design 16.2, 15.5, PRD 验收顺序): the verify Node runs the
// approved Catalog command through the Verification Engine, then an
// independent Reviewer Session judges the Task semantically (a review
// pass is evidence, never the Runtime deciding success), and the merge
// Node merges the verified Task Branch into the Integration Branch
// serially with --no-ff. A merge text conflict requests the recorded
// Integration Rollback; the Attempt settles only after the Worktree is
// restored (PRD 已确认：Merge Conflict 处理). Every decision revalidates
// the committed Dispatch Gate; the Attempt settles from immutable
// evidence, never from an Agent's claim.
//
// Same-package split of the decision package: no public seam added.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"cflow.local/cflow/internal/model"
)

// verifyFailureCode is the compiled failure code of a failed
// Verification run by node kind: a Task verify fails with
// COMMAND_FAILED (retryable); the Integration/Final verify fails with
// INTEGRATION_VERIFICATION_FAILED.
func verifyFailureCode(node *model.Node) model.Code {
	if node.Kind == model.NodeFinalVerify {
		return model.CodeIntegrationVerificationFailed
	}
	return model.CodeCommandFailed
}

// ---------------------------------------------------------------------------
// Workspace Adoption Gate (TUI task 6, design 8.4)
// ---------------------------------------------------------------------------

// decideAdoptWorkspace starts the Workspace Adoption Gate of one
// Execution-Approved Workflow whose Approval bound a frozen Change Set.
// The Application already re-verified the Workspace facts (Change Set
// re-observation, Commit Policy, Identity/Signing, Clean/Scope, Catalog
// Verification) and resolved the approved independent-review route; the
// Kernel revalidates the committed gates (aggregated workspace layout,
// EXECUTION stage, running Runtime, the bound Change Set hash, an
// unadopted Workspace, and a fresh independent Session) and records the
// Adoption Review Session. The Session's PASS verdict advances
// verified_workspace_head to the exact verified Candidate Head; a FAIL
// verdict or a drifted Workspace Blocks the Workflow and preserves the
// Workspace and the Target Branch.
//
// When the Workspace carries uncommitted native changes (Task 4, design
// 8.4 step 2), the Application allocates a managed adoption/coding Session
// (AdoptionSession/AdoptionRoute): the Kernel records that Session and
// requests its Provider start first; the post-adoption gate chain then runs
// against the NEW candidate Head and only then starts the independent
// Adoption Review Session.
func decideAdoptWorkspace(state model.State, in model.AdoptWorkspaceInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to adopt")
	}
	if state.Workflow.LayoutVersion < 2 || state.Workflow.WorkspacePath == "" || state.Workflow.WorkspaceBranch == "" {
		return model.Decision{}, model.InvalidInputFault("workspace adoption requires the aggregated workspace layout")
	}
	if state.Workflow.Stage != model.StageExecution {
		return model.Decision{}, model.InvalidInputFault("workspace adoption requires the EXECUTION stage")
	}
	if state.Workflow.Runtime != model.RuntimeRunning {
		return model.Decision{}, model.InvalidInputFault("workspace adoption requires a running workflow")
	}
	if state.Workflow.VerifiedWorkspaceHead != "" {
		return model.Decision{}, model.InvalidInputFault("the workspace is already adopted")
	}
	facts := state.Workflow.ExecutionFacts
	if facts == nil || facts.ChangeSetHash == "" {
		return model.Decision{}, model.InvalidInputFault("workspace adoption requires an execution approval bound to a change set")
	}
	if in.ChangeSetHash == "" || in.ChangeSetHash != facts.ChangeSetHash {
		return model.Decision{}, model.NewFault(model.CodeApprovalInputChanged,
			"the change set hash does not match the execution approval facts")
	}
	if in.CandidateHead == "" || in.DirtyFingerprint == "" {
		return model.Decision{}, model.InvalidInputFault("workspace adoption requires the verified workspace facts")
	}
	if err := validateFreshSession(state, in.Session); err != nil {
		return model.Decision{}, err
	}
	if in.Route == "" {
		return model.Decision{}, model.InvalidInputFault("workspace adoption requires the approved review route")
	}
	if in.AdoptionSession != "" {
		// The managed adoption path (Task 4, design 8.4 step 2): the dirty
		// Workspace first runs the adoption/coding Session that organizes
		// and commits the native changes. The independent Adoption Review
		// Session is recorded now (still STARTING) and started after the
		// adoption evidence passes, so its identity stays a fresh Session of
		// the review purpose (design 14.4). The candidate facts are recorded
		// as the observed dirty facts; the post-adoption evidence replaces
		// them when the adoption Session settles.
		if err := validateFreshSession(state, in.AdoptionSession); err != nil {
			return model.Decision{}, err
		}
		if in.AdoptionRoute == "" {
			return model.Decision{}, model.InvalidInputFault("workspace adoption requires the approved adoption route")
		}
		b := &builder{state: state}
		m := wfMut(state, state.Workflow.Stage, state.Workflow.Runtime, state.Workflow.CancelIntent)
		m.CandidateWorkspaceHead = in.CandidateHead
		m.WorkspaceDirtyFingerprint = in.DirtyFingerprint
		b.mutate(m)
		b.mutate(model.SessionAppendMutation{Session: model.Session{
			ID: in.AdoptionSession, Purpose: model.PurposeAdoption, Status: model.SessionStarting,
		}, Provider: in.AdoptionRoute})
		b.mutate(model.SessionAppendMutation{Session: model.Session{
			ID: in.Session, Purpose: model.PurposeReview, Status: model.SessionStarting,
		}, Provider: in.Route})
		b.effect(model.ProviderStartIntent{
			Session: in.AdoptionSession,
			Purpose: model.PurposeAdoption,
			Route:   in.AdoptionRoute,
		})
		return b.decision(), nil
	}
	b := &builder{state: state}
	// The verified candidate facts are recorded when the gate starts: the
	// runtime re-verified the exact Candidate Head and Dirty Fingerprint,
	// and the PASS verdict later advances verified_workspace_head to them
	// (design 8.4 step 6).
	m := wfMut(state, state.Workflow.Stage, state.Workflow.Runtime, state.Workflow.CancelIntent)
	m.CandidateWorkspaceHead = in.CandidateHead
	m.WorkspaceDirtyFingerprint = in.DirtyFingerprint
	b.mutate(m)
	b.mutate(model.SessionAppendMutation{Session: model.Session{
		ID: in.Session, Purpose: model.PurposeReview, Status: model.SessionStarting,
	}, Provider: in.Route})
	b.effect(model.ProviderStartIntent{
		Session: in.Session,
		Purpose: model.PurposeReview,
		Route:   in.Route,
	})
	return b.decision(), nil
}

// decideAdoptionCodingRunEnded settles one managed adoption/coding Session
// (Task 4, design 8.4 step 2). The adoption output is judged by evidence,
// never by a claim: a PASS requires the Workspace to be clean at a
// Candidate Head that advanced past the recorded pre-adoption Head (a new
// Commit exists). A failed or crashed Session, a dirty Workspace, or no new
// Commit Blocks the Workflow through adoptionFailure — the Workspace, the
// Change Set, and the Target Branch stay untouched. On a PASS the Kernel
// re-binds the frozen Change Set facts to the post-adoption revision the
// Runtime re-froze, advances the candidate facts, and starts the
// independent Adoption Review Session recorded by decideAdoptWorkspace.
func decideAdoptionCodingRunEnded(state model.State, in model.EffectResultInput, created *model.Session) (model.Decision, error) {
	attempt := attemptBySession(state, created.ID)
	if attempt != nil {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("adoption session %s has an execution attempt", created.ID))
	}
	b := &builder{state: state}
	b.mutate(sessionEnd(state, created, in))
	if in.Session.Status != model.SessionCompleted {
		code := in.FailureCode
		if code == "" {
			code = model.CodeAgentProcessCrashed
		}
		return adoptionFailure(b, state, created, code), nil
	}
	if in.EndDirtyFingerprint != "" {
		return adoptionFailure(b, state, created, model.CodeDirtyWorktreeDrifted), nil
	}
	if in.EndHead == "" || in.EndHead == state.Workflow.CandidateWorkspaceHead {
		return adoptionFailure(b, state, created, model.CodeMissingImplementationCommit), nil
	}
	// The adoption evidence passed: the Workspace is clean at the NEW
	// candidate Head, and the Runtime re-froze the Change Set against it.
	m := wfMut(state, state.Workflow.Stage, state.Workflow.Runtime, state.Workflow.CancelIntent)
	m.CandidateWorkspaceHead = in.EndHead
	m.WorkspaceDirtyFingerprint = in.EndDirtyFingerprint
	b.mutate(m)
	if in.Artifact.Hash != "" {
		// The active Change Set reference advances to the post-adoption
		// revision the adoption re-froze (the frozen Change Set facts are
		// re-bound to the new candidate Head).
		b.mutate(model.ArtifactRefMutation{
			Type: model.ArtifactChangeSet, Revision: in.Artifact.Revision,
			Path: in.Artifact.String(), Hash: in.Artifact.Hash,
		})
	}
	// Start the independent Adoption Review Session the adoption gate
	// recorded (design 8.4 step 5).
	review, ok := adoptionReviewSessionOf(state)
	if !ok {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("the adoption review session is missing"))
	}
	b.effect(model.ProviderStartIntent{
		Session: review.ID,
		Purpose: model.PurposeReview,
		Route:   review.Provider,
	})
	return b.decision(), nil
}

// adoptionReviewSessionOf returns the recorded independent Adoption Review
// Session of the Workspace Adoption Gate (the fresh review-purpose Session
// decideAdoptWorkspace recorded before the adoption/coding Session ran).
func adoptionReviewSessionOf(state model.State) (model.Session, bool) {
	for _, s := range state.Sessions {
		if s.Purpose == model.PurposeReview && s.Status == model.SessionStarting {
			return s, true
		}
	}
	return model.Session{}, false
}

// decideAdoptionReviewRunEnded settles one Workspace Adoption Review
// Session: the review pass is evidence (a structured PASS/FAIL verdict),
// never the Runtime deciding success. A PASS writes the exact verified
// Workspace facts (CandidateHead, VerifiedHead, and the clean Dirty
// Fingerprint at the adopted Head) and opens dispatch to the adopted
// Workspace; a FAIL or an unparsable verdict Blocks the Workflow with a
// blocking Finding while the Workspace, the Change Set, and the Target
// Branch stay untouched (design 8.4 step 7).
func decideAdoptionReviewRunEnded(state model.State, in model.EffectResultInput, created *model.Session) (model.Decision, error) {
	attempt := attemptBySession(state, created.ID)
	if attempt != nil {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("adoption review session %s has an execution attempt", created.ID))
	}
	b := &builder{state: state}
	b.mutate(sessionEnd(state, created, in))
	if in.Session.Status != model.SessionCompleted {
		code := in.FailureCode
		if code == "" {
			code = model.CodeAgentProcessCrashed
		}
		return adoptionFailure(b, state, created, code), nil
	}
	verdict, err := parseReviewVerdict(in.Body)
	if err != nil {
		return adoptionFailure(b, state, created, model.CodeSemanticReviewFailed), nil
	}
	if !verdict {
		return adoptionFailure(b, state, created, model.CodeSemanticReviewFailed), nil
	}
	// The Adoption PASS advances the verified Workspace Head to the exact
	// Candidate Head the Runtime re-verified and records the clean Dirty
	// Fingerprint at that Head (design 8.4 step 6).
	m := wfMut(state, state.Workflow.Stage, state.Workflow.Runtime, state.Workflow.CancelIntent)
	m.CandidateWorkspaceHead = state.Workflow.CandidateWorkspaceHead
	m.VerifiedWorkspaceHead = state.Workflow.CandidateWorkspaceHead
	m.WorkspaceDirtyFingerprint = in.EndDirtyFingerprint
	b.mutate(m)
	b.event(model.EventWorkflowResumed, "", model.AttemptKey{}, "", "workspace adopted")
	return b.decision(), nil
}

// adoptionFailure Blocks the Workflow on a failed Workspace Adoption
// Review: a blocking Finding is recorded and the Runtime moves to BLOCKED
// while the Workspace, the Change Set, and the Target Branch stay
// untouched (design 8.4 step 7).
func adoptionFailure(b *builder, state model.State, created *model.Session, code model.Code) model.Decision {
	pol, _ := model.Policy(code)
	b.mutate(model.FindingAppendMutation{Finding: model.Finding{
		ID:       model.FindingID(fmt.Sprintf("finding-%d", len(state.Findings)+1)),
		Code:     code,
		Scope:    pol.Scope,
		Subject:  string(created.ID),
		Blocking: true,
		Text:     code.String(),
		Seq:      state.NextEventSeq,
	}})
	b.event(model.EventFindingOpened, "", model.AttemptKey{}, code, "workspace adoption review failed")
	b.mutate(wfMutStatus(state, model.RuntimeBlocked))
	b.event(model.EventWorkflowBlocked, "", model.AttemptKey{}, "", "workflow blocked by the workspace adoption review")
	return b.decision()
}

// ---------------------------------------------------------------------------
// Final Verify dispatch and settlement
// ---------------------------------------------------------------------------

// decideFinalVerifyDispatch allocates the Final Verify Node (Task 18,
// PRD 最终验收): the RUNNING Attempt commits at the recorded Integration
// HEAD, the independent Final Reviewer Session of the FINAL_VERIFICATION
// purpose is recorded, the Workflow moves to the FINAL_VERIFICATION
// stage, and the VerificationRun Intent requests the approved final-verify
// Catalog command over the full Integration range base_commit..head. The
// Final Reviewer is always a fresh Session of its own purpose — it can
// never be or chain the implementer's Session (design 14.4).
func decideFinalVerifyDispatch(state model.State, in model.DispatchInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to dispatch")
	}
	switch state.Workflow.Stage {
	case model.StageExecution, model.StageFinalVerification:
	default:
		return model.Decision{}, model.InvalidInputFault(
			"final verify dispatch requires the EXECUTION or FINAL_VERIFICATION stage")
	}
	run := activeRun(state)
	if run == nil || run.Status != model.RunRunning || !run.DispatchGate {
		return model.Decision{}, model.NewFault(model.CodeDispatchGateClosed,
			"dispatch gate is closed; no new node may start")
	}
	node := state.Nodes[in.Node]
	if node == nil {
		return model.Decision{}, model.InvalidInputFault("unknown node " + string(in.Node))
	}
	if node.Kind != model.NodeFinalVerify {
		return model.Decision{}, model.InvalidInputFault(
			"node kind " + string(node.Kind) + " is not a final verify node")
	}
	switch node.Status {
	case model.NodePending, model.NodeReady:
	default:
		return model.Decision{}, model.InvalidInputFault(
			"node " + string(node.ID) + " cannot be allocated from status " + string(node.Status))
	}
	if in.Session == "" || in.Route == "" {
		return model.Decision{}, model.InvalidInputFault(
			"final verify allocation requires the final reviewer session and the approved route")
	}
	// The Final Verify runs over the full accepted delivery: on the
	// aggregated workspace layout the Workspace verified Head is the
	// complete result (design 8.5, TUI task 7); on the legacy layout the
	// Integration Head is (Task 18).
	if state.Workflow.LayoutVersion >= 2 {
		if state.Workflow.VerifiedWorkspaceHead == "" {
			return model.Decision{}, model.InvalidInputFault(
				"final verify allocation requires a verified workspace head")
		}
	} else if state.Workflow.IntegrationHead == "" {
		return model.Decision{}, model.InvalidInputFault(
			"final verify allocation requires a recorded integration head")
	}
	facts := state.Workflow.ExecutionFacts
	if facts == nil || facts.CatalogRevision < 1 || facts.CatalogHash == "" {
		return model.Decision{}, model.InvalidInputFault(
			"final verify allocation requires the approved verification catalog")
	}
	if err := validateFreshSession(state, in.Session); err != nil {
		return model.Decision{}, err
	}
	number := nextAttemptNumber(state, node.ID)
	key := model.AttemptKey{Node: node.ID, Number: number}
	b := &builder{state: state}
	b.mutate(model.NodeStatusMutation{Node: node.ID, Status: model.NodeRunning, RetryCharged: node.RetryCharged})
	b.event(model.EventNodeStarted, node.ID, key, "", "final verify node started")
	// The independent Final Reviewer Session is a fresh Session of the
	// final-verification purpose (design 14.4): it can never share the
	// implementer's lineage.
	b.mutate(model.SessionAppendMutation{Session: model.Session{
		ID: in.Session, Purpose: model.PurposeFinalVerification, Status: model.SessionStarting,
	}, Provider: in.Route})
	if in.Process != "" {
		b.mutate(model.ProcessAppendMutation{Process: model.ProcessRecord{
			ID: in.Process, Session: in.Session, Purpose: model.PurposeFinalVerification,
			Status: model.ProcessStatusRunning, StartedAt: state.Now,
		}})
	}
	head := state.Workflow.IntegrationHead
	if state.Workflow.LayoutVersion >= 2 {
		// Aggregated workspace layout (design 8.5, TUI task 7): the Final
		// Verify covers the full verified Workspace result.
		head = state.Workflow.VerifiedWorkspaceHead
	}
	b.mutate(model.AttemptAppendMutation{Attempt: model.Attempt{
		Key:       key,
		Session:   in.Session,
		Status:    model.AttemptRunning,
		StartHead: head,
		StartedAt: state.Now,
	}})
	b.event(model.EventAttemptCreated, node.ID, key, "", "final verify attempt created")
	// The Workflow enters the final acceptance stage for the last node of
	// the delivery chain (PRD 状态机与持久化模型: EXECUTION ->
	// FINAL_VERIFICATION -> COMPLETED).
	b.mutate(wfMut(state, model.StageFinalVerification, state.Workflow.Runtime, state.Workflow.CancelIntent))
	b.event(model.EventStageChanged, "", model.AttemptKey{}, "", "stage changed to FINAL_VERIFICATION")
	b.effect(model.VerificationRunIntent{
		Node: node.ID,
		Catalog: model.CatalogRef{
			Revision: facts.CatalogRevision, Hash: facts.CatalogHash,
		},
		CommitRange: state.Workflow.BaseCommit + ".." + head,
	})
	return b.decision(), nil
}

// decideVerifyDispatch allocates one verify Node (design 12/16): the
// RUNNING Attempt commits with the Commit under verification, the
// independent Reviewer Session is recorded (its route is the approved
// Spec route), and the VerificationRun Intent requests the approved
// Catalog command over the full Task range task_base_commit..end_head
// (derived from the aggregate, never from display facts).
func decideVerifyDispatch(state model.State, in model.DispatchInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to dispatch")
	}
	if state.Workflow.Stage != model.StageExecution {
		return model.Decision{}, model.InvalidInputFault("dispatch requires the EXECUTION stage")
	}
	run := activeRun(state)
	if run == nil || run.Status != model.RunRunning || !run.DispatchGate {
		return model.Decision{}, model.NewFault(model.CodeDispatchGateClosed,
			"dispatch gate is closed; no new node may start")
	}
	node := state.Nodes[in.Node]
	if node == nil {
		return model.Decision{}, model.InvalidInputFault("unknown node " + string(in.Node))
	}
	if node.Kind != model.NodeVerify {
		return model.Decision{}, model.InvalidInputFault(
			"node kind " + string(node.Kind) + " is not a verify node")
	}
	switch node.Status {
	case model.NodePending, model.NodeReady:
	default:
		return model.Decision{}, model.InvalidInputFault(
			"node " + string(node.ID) + " cannot be allocated from status " + string(node.Status))
	}
	if in.Session == "" || in.Route == "" {
		return model.Decision{}, model.InvalidInputFault(
			"verify allocation requires the reviewer session and the approved route")
	}
	facts := state.Workflow.ExecutionFacts
	if facts == nil || facts.CatalogRevision < 1 || facts.CatalogHash == "" {
		return model.Decision{}, model.InvalidInputFault(
			"verify allocation requires the approved verification catalog")
	}
	taskNode, err := taskNodeOf(state, node)
	if err != nil {
		return model.Decision{}, err
	}
	if branchQuarantined(state, taskNode.Branch) || taskNode.Status == model.NodeFailed {
		// A quarantined Branch can never re-enter Verify: the delivery
		// chain is closed permanently (design 7.3 invariant 10, PRD 已确
		// 认：漂移窗口 Commit 的隔离与替代执行 step 1).
		return model.Decision{}, model.NewFault(model.CodeCommitDuringPolicyDriftWindow,
			"the task branch is quarantined; verification is closed for it")
	}
	taskAttempt := succeededAttemptOf(state, taskNode.ID)
	if taskAttempt == nil || taskAttempt.EndHead == "" {
		return model.Decision{}, model.InvalidInputFault(
			"verify node " + string(node.ID) + " has no verified task commit to check")
	}
	if err := validateFreshSession(state, in.Session); err != nil {
		return model.Decision{}, err
	}
	number := nextAttemptNumber(state, node.ID)
	key := model.AttemptKey{Node: node.ID, Number: number}
	b := &builder{state: state}
	b.mutate(model.NodeStatusMutation{Node: node.ID, Status: model.NodeRunning, RetryCharged: node.RetryCharged})
	b.event(model.EventNodeStarted, node.ID, key, "", "verify node started")
	// The independent Reviewer Session is a fresh Session of the review
	// purpose (design 14.4): it can never share the implementer's lineage.
	b.mutate(model.SessionAppendMutation{Session: model.Session{
		ID: in.Session, Purpose: model.PurposeReview, Status: model.SessionStarting,
	}, Provider: in.Route})
	if in.Process != "" {
		// The chain's managed process is RUNNING with the Reviewer Session.
		b.mutate(model.ProcessAppendMutation{Process: model.ProcessRecord{
			ID: in.Process, Session: in.Session, Purpose: model.PurposeReview,
			Status: model.ProcessStatusRunning, StartedAt: state.Now,
		}})
	}
	b.mutate(model.AttemptAppendMutation{Attempt: model.Attempt{
		Key:       key,
		Session:   in.Session,
		Status:    model.AttemptRunning,
		StartHead: taskAttempt.EndHead,
		StartedAt: state.Now,
	}})
	b.event(model.EventAttemptCreated, node.ID, key, "", "verify attempt created")
	b.effect(model.VerificationRunIntent{
		Node: node.ID,
		Catalog: model.CatalogRef{
			Revision: facts.CatalogRevision, Hash: facts.CatalogHash,
		},
		CommitRange: taskAttempt.StartHead + ".." + taskAttempt.EndHead,
	})
	return b.decision(), nil
}

// decideVerificationRunEnded routes one VerificationRunEnded result: a
// failed run settles the verify (or final verify) Attempt with the
// compiled failure code (and cancels the never-started Reviewer Session);
// a passed run starts the independent Reviewer Session bound to the exact
// Commit/Catalog/evidence refs — the TASK_REVIEW Session for a verify
// Node, the FINAL_VERIFICATION Session for the Final Verify Node.
func decideVerificationRunEnded(state model.State, in model.EffectResultInput) (model.Decision, error) {
	attempt := state.Attempts[in.Attempt]
	if attempt == nil {
		return model.Decision{}, model.InvalidInputFault("unknown attempt " + in.Attempt.String())
	}
	if attempt.Status != model.AttemptRunning {
		return model.Decision{}, model.InvalidInputFault("attempt " + in.Attempt.String() + " is not running")
	}
	node := state.Nodes[attempt.Key.Node]
	if node == nil || (node.Kind != model.NodeVerify && node.Kind != model.NodeFinalVerify) {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("verification result references a non-verify node"))
	}
	b := &builder{state: state}
	if !in.Passed {
		// The never-started Reviewer Session settles cancelled in the
		// same Decision that settles the failed Attempt. The compiled
		// failure code: the executor's typed code when the run was
		// refused (identity drift), else the node's verification code.
		code := in.FailureCode
		if code == "" {
			code = verifyFailureCode(node)
		}
		cancelSession(b, state, attempt.Session)
		evidence := model.EvidenceRef{
			Kind: model.EvidenceTestResult, Hash: in.ManifestHash, Subject: string(node.ID),
		}
		return decideAttemptFailure(state, node, attempt, model.EffectResultInput{
			Kind:        model.AttemptEnded,
			Attempt:     attempt.Key,
			Outcome:     model.OutcomeFailed,
			FailureCode: code,
			Evidence:    evidence,
		}, code, b)
	}
	review := findSessionState(state, attempt.Session)
	if review == nil {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("verify attempt %s has no reviewer session", attempt.Key))
	}
	b.effect(model.ProviderStartIntent{
		Session: attempt.Session,
		// The Reviewer Session's own purpose rides the start intent: the
		// Final Verify Node starts the FINAL_VERIFICATION reviewer, never
		// a TASK_REVIEW Session (design 14.4).
		Purpose: review.Purpose,
		Route:   review.Provider,
		Node:    node.ID,
		Process: processOfAttempt(state, attempt.Key),
	})
	return b.decision(), nil
}

// decideReviewRunEnded settles one Reviewer Session: the review pass is
// evidence (a structured PASS/FAIL verdict in the review report), never
// the Runtime deciding success; a pass moves the verify Attempt to
// SUCCEEDED with the test-result and review-result evidence, a fail or an
// unparsable report settles it with SEMANTIC_REVIEW_FAILED (retryable).
// Session independence is re-verified: the Reviewer must never reuse any
// existing Session's provider id (design 14.4).
func decideReviewRunEnded(state model.State, in model.EffectResultInput, created *model.Session) (model.Decision, error) {
	for _, s := range state.Sessions {
		if s.ID != created.ID && s.ProviderSessionID != "" && s.ProviderSessionID == in.Session.ProviderSessionID {
			return model.Decision{}, model.NewFault(model.CodeSessionIndependenceViolation,
				"the reviewer session reuses an existing session's provider session id")
		}
	}
	attempt := attemptBySession(state, created.ID)
	if attempt == nil {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("reviewer session %s has no running attempt", created.ID))
	}
	node := state.Nodes[attempt.Key.Node]
	verdict, err := parseReviewVerdict(in.Body)
	if err != nil {
		b := &builder{state: state}
		b.mutate(sessionEnd(state, created, in))
		return decideAttemptFailure(state, node, attempt, model.EffectResultInput{
			Kind: model.AttemptEnded, Attempt: attempt.Key,
			Outcome: model.OutcomeFailed, FailureCode: model.CodeSemanticReviewFailed,
		}, model.CodeSemanticReviewFailed, b)
	}
	b := &builder{state: state}
	b.mutate(sessionEnd(state, created, in))
	reviewEvidence := model.EvidenceRef{
		Kind: model.EvidenceReviewResult, Hash: sha256Hex(in.Body), Subject: string(created.ID),
	}
	testEvidence := model.EvidenceRef{
		Kind: model.EvidenceTestResult, Hash: in.ManifestHash, Subject: string(node.ID),
	}
	if !verdict {
		return decideAttemptFailure(state, node, attempt, model.EffectResultInput{
			Kind: model.AttemptEnded, Attempt: attempt.Key,
			Outcome: model.OutcomeFailed, FailureCode: model.CodeSemanticReviewFailed,
		}, model.CodeSemanticReviewFailed, b)
	}
	settleNodeSucceeded(b, state, node, attempt, model.EffectResultInput{
		Kind:         model.AttemptEnded,
		Attempt:      attempt.Key,
		Outcome:      model.OutcomeSucceeded,
		EndHead:      attempt.StartHead,
		Evidence:     reviewEvidence,
		EvidenceRefs: []model.EvidenceRef{testEvidence, reviewEvidence},
	})
	return b.decision(), nil
}

// decideMergeDispatch allocates one merge Node (design 15.5): the
// RUNNING Attempt commits at the current Integration HEAD and the serial
// --no-ff merge Intent fixes the exact Task Branch and the accepted
// Commit it must bring in (PRD: Merge 前再次比较已验收 Commit、Task Branch
// HEAD 和 Git-clean 状态).
func decideMergeDispatch(state model.State, in model.DispatchInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to dispatch")
	}
	if state.Workflow.Stage != model.StageExecution {
		return model.Decision{}, model.InvalidInputFault("dispatch requires the EXECUTION stage")
	}
	run := activeRun(state)
	if run == nil || run.Status != model.RunRunning || !run.DispatchGate {
		return model.Decision{}, model.NewFault(model.CodeDispatchGateClosed,
			"dispatch gate is closed; no new node may start")
	}
	node := state.Nodes[in.Node]
	if node == nil {
		return model.Decision{}, model.InvalidInputFault("unknown node " + string(in.Node))
	}
	if node.Kind != model.NodeMerge {
		return model.Decision{}, model.InvalidInputFault(
			"node kind " + string(node.Kind) + " is not a merge node")
	}
	switch node.Status {
	case model.NodePending, model.NodeReady:
	default:
		return model.Decision{}, model.InvalidInputFault(
			"node " + string(node.ID) + " cannot be allocated from status " + string(node.Status))
	}
	taskNode, err := taskNodeOf(state, node)
	if err != nil {
		return model.Decision{}, err
	}
	if branchQuarantined(state, taskNode.Branch) || taskNode.Status == model.NodeFailed {
		// A quarantined Branch never merges into the trusted delivery
		// chain (design 7.3 invariant 10, PRD 已确认 step 1).
		return model.Decision{}, model.NewFault(model.CodeCommitDuringPolicyDriftWindow,
			"the task branch is quarantined; the merge path is closed for it")
	}
	taskAttempt := succeededAttemptOf(state, taskNode.ID)
	if taskAttempt == nil || taskAttempt.EndHead == "" {
		return model.Decision{}, model.InvalidInputFault(
			"merge node " + string(node.ID) + " has no verified task commit to merge")
	}
	number := nextAttemptNumber(state, node.ID)
	key := model.AttemptKey{Node: node.ID, Number: number}
	b := &builder{state: state}
	b.mutate(model.NodeStatusMutation{Node: node.ID, Status: model.NodeRunning, RetryCharged: node.RetryCharged})
	b.event(model.EventNodeStarted, node.ID, key, "", "merge node started")
	if state.Workflow.LayoutVersion >= 2 {
		// Aggregated workspace layout (TUI task 7, design 8.5): the merge
		// base is the current verified Workspace Head — the only legal
		// Task base — and the serial --no-ff merge advances the same
		// Workspace Branch. Sibling Tasks may share an old Base, but the
		// Intent fixes the latest verified Head at scheduling time; no
		// auto-rebase ever rewrites Task history. When no adoption ran
		// (no Change Set bound), the Runtime-observed workspace Head the
		// pass fixed at readiness is the merge base.
		base := state.Workflow.VerifiedWorkspaceHead
		if base == "" {
			base = in.BaseHead
		}
		if base == "" {
			return model.Decision{}, model.InvariantFault(fmt.Errorf("merge allocation requires a workspace head"))
		}
		b.mutate(model.AttemptAppendMutation{Attempt: model.Attempt{
			Key:       key,
			Status:    model.AttemptRunning,
			StartHead: base,
			StartedAt: state.Now,
		}})
		b.event(model.EventAttemptCreated, node.ID, key, "", "merge attempt created")
		b.effect(model.WorkspaceMergeIntent{
			Node:                  node.ID,
			ExpectedWorkspaceHead: base,
			TaskBranch:            taskBranch(state.Workflow.ID, taskNode.ID),
			VerifiedCommit:        taskAttempt.EndHead,
		})
		return b.decision(), nil
	}
	if state.Workflow.IntegrationHead == "" {
		return model.Decision{}, model.InvalidInputFault(
			"merge allocation requires a recorded integration head")
	}
	b.mutate(model.AttemptAppendMutation{Attempt: model.Attempt{
		Key:       key,
		Status:    model.AttemptRunning,
		StartHead: state.Workflow.IntegrationHead,
		StartedAt: state.Now,
	}})
	b.event(model.EventAttemptCreated, node.ID, key, "", "merge attempt created")
	b.effect(model.IntegrationMergeIntent{
		Node:           node.ID,
		BaseHead:       state.Workflow.IntegrationHead,
		TaskBranch:     taskBranch(state.Workflow.ID, taskNode.ID),
		VerifiedCommit: taskAttempt.EndHead,
	})
	return b.decision(), nil
}

// decideIntegrationMerged settles one successful serial --no-ff merge:
// the Attempt succeeds with the Merge Commit evidence and the
// Integration HEAD advances to the Merge Commit (the aggregate's
// IntegrationHead only ever moves through verified merges).
func decideIntegrationMerged(state model.State, in model.EffectResultInput) (model.Decision, error) {
	attempt := state.Attempts[in.Attempt]
	if attempt == nil || attempt.Status != model.AttemptRunning {
		return model.Decision{}, model.InvalidInputFault("unknown or non-running attempt " + in.Attempt.String())
	}
	node := state.Nodes[attempt.Key.Node]
	if node == nil || node.Kind != model.NodeMerge {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("merge result references a non-merge node"))
	}
	if in.EndHead == "" || in.EndHead == state.Workflow.IntegrationHead {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("merge result did not advance the integration head"))
	}
	b := &builder{state: state}
	m := wfMut(state, state.Workflow.Stage, state.Workflow.Runtime, state.Workflow.CancelIntent)
	m.IntegrationHead = in.EndHead
	commitEvidence := model.EvidenceRef{
		Kind: model.EvidenceCommit, Hash: in.EndHead, Subject: state.Workflow.IntegrationBranch,
	}
	settleNodeSucceeded(b, state, node, attempt, model.EffectResultInput{
		Kind:         model.AttemptEnded,
		Attempt:      attempt.Key,
		Outcome:      model.OutcomeSucceeded,
		EndHead:      in.EndHead,
		Evidence:     commitEvidence,
		EvidenceRefs: []model.EvidenceRef{commitEvidence},
	}, m)
	return b.decision(), nil
}

// decideIntegrationMergeFailed records the failed merge and requests the
// recorded Integration Rollback: the managed Integration Worktree is
// restored to the pre-merge HEAD before the Attempt settles (design
// 15.5; PRD 已确认：Merge Conflict 处理).
func decideIntegrationMergeFailed(state model.State, in model.EffectResultInput) (model.Decision, error) {
	attempt := state.Attempts[in.Attempt]
	if attempt == nil || attempt.Status != model.AttemptRunning {
		return model.Decision{}, model.InvalidInputFault("unknown or non-running attempt " + in.Attempt.String())
	}
	if in.PreMergeHead == "" {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("merge failure carries no pre-merge head"))
	}
	// The typed failure code rides the rollback Intent so the Attempt
	// settles with the code the executor observed (a text conflict, a
	// committed merge whose post-merge checks failed, or a refused
	// merge), never with an invented one.
	code := in.FailureCode
	if code == "" {
		code = model.CodeMergeConflict
	}
	b := &builder{state: state}
	b.effect(model.IntegrationRollbackIntent{Head: in.PreMergeHead, Attempt: attempt.Key, FailureCode: code})
	return b.decision(), nil
}

// decideIntegrationRollbacked settles the failed merge Attempt after the
// managed Integration Worktree was restored: a text conflict fails with
// MERGE_CONFLICT (retryable; the Merge Node's budget governs the single
// restricted resolution Attempt of Task 17), and a committed merge that
// failed its post-merge checks settles with the typed code the executor
// observed.
func decideIntegrationRollbacked(state model.State, in model.EffectResultInput) (model.Decision, error) {
	attempt := state.Attempts[in.Attempt]
	if attempt == nil || attempt.Status != model.AttemptRunning {
		return model.Decision{}, model.InvalidInputFault("unknown or non-running attempt " + in.Attempt.String())
	}
	code := in.FailureCode
	if code == "" {
		code = model.CodeMergeConflict
	}
	return decideAttemptEnded(state, model.EffectResultInput{
		Kind:        model.AttemptEnded,
		Attempt:     attempt.Key,
		Outcome:     model.OutcomeFailed,
		FailureCode: code,
		Evidence:    in.Evidence,
	})
}

// ---------------------------------------------------------------------------
// Workspace merge settlement (design 8.5, TUI task 7)
// ---------------------------------------------------------------------------

// decideWorkspaceMerged settles one successful serial --no-ff Workspace
// merge: the Attempt succeeds with the Merge Commit evidence and the
// VERIFIED Workspace Head advances to the Merge Commit — the aggregate's
// VerifiedWorkspaceHead only ever moves through verified merges, so every
// successor Task branches from the fully accepted history.
func decideWorkspaceMerged(state model.State, in model.EffectResultInput) (model.Decision, error) {
	attempt := state.Attempts[in.Attempt]
	if attempt == nil || attempt.Status != model.AttemptRunning {
		return model.Decision{}, model.InvalidInputFault("unknown or non-running attempt " + in.Attempt.String())
	}
	node := state.Nodes[attempt.Key.Node]
	if node == nil || node.Kind != model.NodeMerge {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("workspace merge result references a non-merge node"))
	}
	if in.EndHead == "" || in.EndHead == state.Workflow.VerifiedWorkspaceHead {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("workspace merge result did not advance the verified workspace head"))
	}
	b := &builder{state: state}
	m := wfMut(state, state.Workflow.Stage, state.Workflow.Runtime, state.Workflow.CancelIntent)
	m.VerifiedWorkspaceHead = in.EndHead
	commitEvidence := model.EvidenceRef{
		Kind: model.EvidenceCommit, Hash: in.EndHead, Subject: state.Workflow.WorkspaceBranch,
	}
	settleNodeSucceeded(b, state, node, attempt, model.EffectResultInput{
		Kind:         model.AttemptEnded,
		Attempt:      attempt.Key,
		Outcome:      model.OutcomeSucceeded,
		EndHead:      in.EndHead,
		Evidence:     commitEvidence,
		EvidenceRefs: []model.EvidenceRef{commitEvidence},
	}, m)
	return b.decision(), nil
}

// decideWorkspaceMergeFailed records the failed Workspace merge and
// requests the recorded Workspace Rollback: the managed Workspace Worktree
// is restored to the pre-merge HEAD before the Attempt settles (design
// 8.5, TUI task 7; PRD 已确认：Merge Conflict 处理).
func decideWorkspaceMergeFailed(state model.State, in model.EffectResultInput) (model.Decision, error) {
	attempt := state.Attempts[in.Attempt]
	if attempt == nil || attempt.Status != model.AttemptRunning {
		return model.Decision{}, model.InvalidInputFault("unknown or non-running attempt " + in.Attempt.String())
	}
	if in.PreMergeHead == "" {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("workspace merge failure carries no pre-merge head"))
	}
	code := in.FailureCode
	if code == "" {
		code = model.CodeMergeConflict
	}
	b := &builder{state: state}
	b.effect(model.WorkspaceRollbackIntent{Head: in.PreMergeHead, Attempt: attempt.Key, FailureCode: code})
	return b.decision(), nil
}

// decideWorkspaceRollbacked settles the failed Workspace merge Attempt
// after the managed Workspace Worktree was restored: a text conflict fails
// with MERGE_CONFLICT (retryable), and a committed merge that failed its
// post-merge checks settles with the typed code the executor observed.
func decideWorkspaceRollbacked(state model.State, in model.EffectResultInput) (model.Decision, error) {
	attempt := state.Attempts[in.Attempt]
	if attempt == nil || attempt.Status != model.AttemptRunning {
		return model.Decision{}, model.InvalidInputFault("unknown or non-running attempt " + in.Attempt.String())
	}
	code := in.FailureCode
	if code == "" {
		code = model.CodeMergeConflict
	}
	return decideAttemptEnded(state, model.EffectResultInput{
		Kind:        model.AttemptEnded,
		Attempt:     attempt.Key,
		Outcome:     model.OutcomeFailed,
		FailureCode: code,
		Evidence:    in.Evidence,
	})
}

// settleNodeSucceeded applies the immutable success settle of one Node's
// Attempt: the terminal Attempt facts, the SUCCEEDED Node, and the
// settlement protocols, plus any caller mutations (e.g. the Integration
// HEAD advance) in the same Decision.
func settleNodeSucceeded(b *builder, state model.State, node *model.Node, attempt *model.Attempt, in model.EffectResultInput, extra ...model.Mutation) {
	b.mutate(model.AttemptEndMutation{
		Key:                 attempt.Key,
		Status:              model.AttemptSucceeded,
		EndHead:             in.EndHead,
		EndDirtyFingerprint: in.EndDirtyFingerprint,
		Evidence:            evidenceOf(in),
		EndedAt:             state.Now,
	})
	b.event(model.EventAttemptSucceeded, node.ID, attempt.Key, "", "attempt succeeded")
	b.mutate(model.NodeStatusMutation{Node: node.ID, Status: model.NodeSucceeded, RetryCharged: node.RetryCharged})
	b.event(model.EventNodeSucceeded, node.ID, attempt.Key, "", "node succeeded")
	for _, m := range extra {
		b.mutate(m)
	}
	settleAfterAttemptEnd(state, b, attempt.Key)
}

// evidenceOf is the full immutable evidence list an Attempt end records:
// the explicit chain evidence when present, else the primary reference.
func evidenceOf(in model.EffectResultInput) []model.EvidenceRef {
	if len(in.EvidenceRefs) > 0 {
		return append([]model.EvidenceRef(nil), in.EvidenceRefs...)
	}
	if in.Evidence == (model.EvidenceRef{}) {
		return []model.EvidenceRef{}
	}
	return []model.EvidenceRef{in.Evidence}
}

// taskNodeOf derives the agent-task dependency of a verify or merge Node
// from the compiler's deterministic skeleton naming ("task-<spec>",
// "verify-<spec>", "merge-<spec>"; the aggregate persists no dependency
// edges — the Scheduler reads them from the approved plan, and the
// allocation decisions revalidate the derived Task Node against the
// committed graph).
func taskNodeOf(state model.State, node *model.Node) (*model.Node, error) {
	id := string(node.ID)
	for _, prefix := range []string{"verify-", "merge-"} {
		if strings.HasPrefix(id, prefix) {
			derived := model.NodeID("task-" + strings.TrimPrefix(id, prefix))
			task := state.Nodes[derived]
			if task == nil || task.Kind != model.NodeAgentTask {
				return nil, model.InvariantFault(fmt.Errorf(
					"node %s has no agent-task dependency in the committed graph", id))
			}
			return task, nil
		}
	}
	return nil, model.InvalidInputFault(
		"node kind " + string(node.Kind) + " has no task dependency")
}

// succeededAttemptOf returns the terminal SUCCEEDED Attempt of one Node
// with the highest Attempt number (nil when none).
func succeededAttemptOf(state model.State, node model.NodeID) *model.Attempt {
	var best *model.Attempt
	for k, a := range state.Attempts {
		if k.Node != node || a.Status != model.AttemptSucceeded {
			continue
		}
		if best == nil || k.Number > best.Key.Number {
			best = a
		}
	}
	return best
}

// attemptBySession returns the RUNNING Attempt bound to one Session.
func attemptBySession(state model.State, session model.SessionID) *model.Attempt {
	for _, a := range state.Attempts {
		if a.Session == session && a.Status == model.AttemptRunning {
			return a
		}
	}
	return nil
}

// cancelSession settles a recorded but never-started Session (the
// Reviewer Session of a failed verification) as cancelled.
func cancelSession(b *builder, state model.State, id model.SessionID) {
	s := findSessionState(state, id)
	if s == nil || s.Status.IsTerminal() {
		return
	}
	b.mutate(model.SessionEndMutation{ID: s.ID, Status: model.SessionCancelled, EndedAt: state.Now})
}

// parseReviewVerdict extracts the verdict of a review report (the
// TASK_REVIEW output contract: a `PASS` or `FAIL` verdict line). The
// Provider wire carries the report as a JSON object (the completion
// payload contract); both the structured `decision` field and the
// markdown verdict line are accepted.
func parseReviewVerdict(body []byte) (bool, error) {
	if len(body) == 0 || len(body) > maxPlanBody {
		return false, fmt.Errorf("review report is empty or exceeds the bounded size")
	}
	var structured struct {
		Decision string `json:"decision"`
	}
	if json.Unmarshal(body, &structured) == nil {
		switch structured.Decision {
		case "PASS":
			return true, nil
		case "FAIL":
			return false, nil
		}
	}
	for _, line := range strings.Split(string(body), "\n") {
		switch strings.TrimSpace(line) {
		case "PASS":
			return true, nil
		case "FAIL":
			return false, nil
		}
	}
	return false, fmt.Errorf("no PASS or FAIL verdict line")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
