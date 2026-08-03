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
// failed run settles the verify Attempt with the compiled failure code
// (and cancels the never-started Reviewer Session); a passed run starts
// the Reviewer Session bound to the exact Commit/Catalog/evidence refs.
func decideVerificationRunEnded(state model.State, in model.EffectResultInput) (model.Decision, error) {
	attempt := state.Attempts[in.Attempt]
	if attempt == nil {
		return model.Decision{}, model.InvalidInputFault("unknown attempt " + in.Attempt.String())
	}
	if attempt.Status != model.AttemptRunning {
		return model.Decision{}, model.InvalidInputFault("attempt " + in.Attempt.String() + " is not running")
	}
	node := state.Nodes[attempt.Key.Node]
	if node == nil || node.Kind != model.NodeVerify {
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
		Purpose: model.PurposeReview,
		Route:   review.Provider,
		Node:    node.ID,
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
	if state.Workflow.IntegrationHead == "" {
		return model.Decision{}, model.InvalidInputFault(
			"merge allocation requires a recorded integration head")
	}
	taskNode, err := taskNodeOf(state, node)
	if err != nil {
		return model.Decision{}, err
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
	b := &builder{state: state}
	b.effect(model.IntegrationRollbackIntent{Head: in.PreMergeHead, Attempt: attempt.Key})
	return b.decision(), nil
}

// decideIntegrationRollbacked settles the failed merge Attempt after the
// managed Integration Worktree was restored: a text conflict fails with
// MERGE_CONFLICT (retryable; the Merge Node's budget governs the single
// restricted resolution Attempt of Task 17).
func decideIntegrationRollbacked(state model.State, in model.EffectResultInput) (model.Decision, error) {
	attempt := state.Attempts[in.Attempt]
	if attempt == nil || attempt.Status != model.AttemptRunning {
		return model.Decision{}, model.InvalidInputFault("unknown or non-running attempt " + in.Attempt.String())
	}
	return decideAttemptEnded(state, model.EffectResultInput{
		Kind:        model.AttemptEnded,
		Attempt:     attempt.Key,
		Outcome:     model.OutcomeFailed,
		FailureCode: model.CodeMergeConflict,
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
