// The verify/review/merge kernel decisions (Task 13, design 16.2 and
// 15.5): the verify Node allocation and the Verification run outcome,
// the independent Reviewer Session (whose pass is evidence and whose
// Session reuse of the implementer's lineage fails closed with
// SESSION_INDEPENDENCE_VIOLATION), and the serial Integration merge
// whose committed-merge failure requests the recorded Rollback and
// settles the Attempt only after the managed Worktree is restored.
package decision_test

import (
	"testing"

	"cflow.local/cflow/internal/decision"
	"cflow.local/cflow/internal/model"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// fixtureRunningVerifyNode is the verify chain mid-flight: the verify
// Node RUNNING with an Attempt bound to the Reviewer Session, the
// Reviewer Session row recorded, and the Task's SUCCEEDED Attempt whose
// EndHead is the Commit under verification. The implementer Session
// carries the provider session id the reuse test must never share.
func fixtureRunningVerifyNode() model.State {
	st := workflowState(model.StageExecution, model.RuntimeRunning)
	st.Workflow.ExecutionFacts = &model.ExecutionFacts{
		SpecHashes: []string{"spec-1"}, CatalogHash: "cat-1",
		WorkflowHash: "wf-1", CatalogRevision: 1,
	}
	addRun(&st, model.RunRunning, true)
	addNode(&st, "task-1", model.NodeAgentTask, model.NodeSucceeded, 0)
	addNode(&st, "verify-1", model.NodeVerify, model.NodeRunning, 0)
	tkey := addAttempt(&st, "task-1", 1, model.AttemptSucceeded)
	st.Attempts[tkey].EndHead = "task-head-1"
	vkey := addAttempt(&st, "verify-1", 1, model.AttemptRunning)
	st.Attempts[vkey].Session = "review-1"
	st.Attempts[vkey].StartHead = "task-head-1"
	st.Sessions = append(st.Sessions,
		model.Session{ID: "review-1", Purpose: model.PurposeReview, Status: model.SessionStarting, Provider: "fake"},
		model.Session{ID: "impl-1", Purpose: model.PurposeImplementation, Status: model.SessionCompleted, ProviderSessionID: "impl-provider-1"},
	)
	return st
}

// reviewEnded builds the settled Reviewer Session result input.
func reviewEnded(providerSessionID string, body string) model.Input {
	return model.EffectResultInput{
		Kind: model.ProviderRunEnded,
		Session: model.Session{
			ID: "review-1", ProviderSessionID: providerSessionID,
			Purpose: model.PurposeReview, Status: model.SessionCompleted,
		},
		Body:         []byte(body),
		ManifestHash: "manifest-1",
	}
}

// fixtureRunningMergeNode is the merge chain mid-flight: the merge Node
// RUNNING with an Attempt at the recorded Integration HEAD, and the
// Task's SUCCEEDED Attempt with the accepted Commit.
func fixtureRunningMergeNode() model.State {
	st := workflowState(model.StageExecution, model.RuntimeRunning) // IntegrationHead "int-1"
	addRun(&st, model.RunRunning, true)
	addNode(&st, "task-1", model.NodeAgentTask, model.NodeSucceeded, 0)
	tkey := addAttempt(&st, "task-1", 1, model.AttemptSucceeded)
	st.Attempts[tkey].EndHead = "task-head-1"
	addNode(&st, "merge-1", model.NodeMerge, model.NodeRunning, 0)
	mkey := addAttempt(&st, "merge-1", 1, model.AttemptRunning)
	st.Attempts[mkey].StartHead = "int-1"
	return st
}

// requireAttemptFailure asserts the decision ends one Attempt with the
// exact failure code.
func requireAttemptFailure(t *testing.T, got model.Decision, key model.AttemptKey, code model.Code) {
	t.Helper()
	for _, m := range got.Mutations {
		am, ok := m.(model.AttemptEndMutation)
		if !ok || am.Key != key {
			continue
		}
		if am.FailureCode != code {
			t.Fatalf("attempt %s failure code = %s, want %s", key, am.FailureCode, code)
		}
		return
	}
	t.Fatalf("decision has no end mutation for attempt %s", key)
}

// requireSessionSettled asserts one Session settles with the exact
// status.
func requireSessionSettled(t *testing.T, got model.Decision, id model.SessionID, status model.SessionStatus) {
	t.Helper()
	for _, m := range got.Mutations {
		sm, ok := m.(model.SessionEndMutation)
		if !ok || sm.ID != id {
			continue
		}
		if sm.Status != status {
			t.Fatalf("session %s settles %s, want %s", id, sm.Status, status)
		}
		return
	}
	t.Fatalf("decision has no end mutation for session %s", id)
}

// ---------------------------------------------------------------------------
// the verify Node chain
// ---------------------------------------------------------------------------

// TestVerifyDispatchRequestsVerificationRun: the allocation commits the
// RUNNING verify Attempt at the Commit under verification, records the
// independent Reviewer Session, and requests the approved Catalog
// command over the full Task range.
func TestVerifyDispatchRequestsVerificationRun(t *testing.T) {
	st := fixtureRunningVerifyNode()
	st.Nodes["verify-1"].Status = model.NodePending
	// The Reviewer Session is a fresh Session identity, allocated by the
	// Application and fixed before the Effect (design 6.2 rule 6).
	got, err := decision.Decide(st, model.DispatchInput{Node: "verify-1", Session: "review-2", Route: "fake"})
	requireNoError(t, err)
	requireNode(t, got, "verify-1", model.NodeRunning)
	requireEffect(t, got, model.VerificationRunIntent{
		Node:        "verify-1",
		Catalog:     model.CatalogRef{Revision: 1, Hash: "cat-1"},
		CommitRange: "base-1..task-head-1",
	})
}

// TestVerificationFailureSettlesCommandFailed: a failed Verification run
// settles the verify Attempt with COMMAND_FAILED and cancels the
// never-started Reviewer Session.
func TestVerificationFailureSettlesCommandFailed(t *testing.T) {
	st := fixtureRunningVerifyNode()
	got, err := decision.Decide(st, model.EffectResultInput{
		Kind:    model.VerificationRunEnded,
		Attempt: model.AttemptKey{Node: "verify-1", Number: 1},
		Passed:  false, ManifestHash: "manifest-1",
	})
	requireNoError(t, err)
	requireNode(t, got, "verify-1", model.NodeFailed)
	requireSessionSettled(t, got, "review-1", model.SessionCancelled)
	requireAttemptFailure(t, got, model.AttemptKey{Node: "verify-1", Number: 1}, model.CodeCommandFailed)
}

// TestVerificationPassRequestsReviewerSession: a passed run starts the
// independent Reviewer Session on the recorded review route.
func TestVerificationPassRequestsReviewerSession(t *testing.T) {
	st := fixtureRunningVerifyNode()
	got, err := decision.Decide(st, model.EffectResultInput{
		Kind:    model.VerificationRunEnded,
		Attempt: model.AttemptKey{Node: "verify-1", Number: 1},
		Passed:  true, ManifestHash: "manifest-1",
	})
	requireNoError(t, err)
	requireEffect(t, got, model.ProviderStartIntent{
		Session: "review-1", Purpose: model.PurposeReview, Route: "fake", Node: "verify-1",
	})
}

// TestReviewerSessionReuseFailsClosed (brief case list: "Reviewer Session
// reuse"): the Reviewer Session must never share the implementer's
// provider session id (design 14.4) — the review verdict is rejected
// with SESSION_INDEPENDENCE_VIOLATION.
func TestReviewerSessionReuseFailsClosed(t *testing.T) {
	st := fixtureRunningVerifyNode()
	_, err := decision.Decide(st, reviewEnded("impl-provider-1", `{"decision":"PASS","report":"PASS\n"}`))
	assertFaultCode(t, err, model.CodeSessionIndependenceViolation)
}

// TestReviewVerdictPassSettlesVerifyAttempt: a passed review verdict is
// evidence — the verify Attempt settles SUCCEEDED with the test-result
// and review-result evidence (design 16.2: review never replaces
// deterministic verification).
func TestReviewVerdictPassSettlesVerifyAttempt(t *testing.T) {
	st := fixtureRunningVerifyNode()
	got, err := decision.Decide(st, reviewEnded("review-provider-1", `{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n"}`))
	requireNoError(t, err)
	requireNode(t, got, "verify-1", model.NodeSucceeded)
	requireEvent(t, got, model.EventAttemptSucceeded)
	requireEvent(t, got, model.EventNodeSucceeded)
}

// TestReviewVerdictFailSettlesSemanticReviewFailed: a failed review
// verdict (or an unparsable report) settles the verify Attempt with
// SEMANTIC_REVIEW_FAILED — the review pass is never the Runtime deciding
// success.
func TestReviewVerdictFailSettlesSemanticReviewFailed(t *testing.T) {
	st := fixtureRunningVerifyNode()
	got, err := decision.Decide(st, reviewEnded("review-provider-1", `{"decision":"FAIL","report":"FAIL\n\nblocking issue\n"}`))
	requireNoError(t, err)
	requireNode(t, got, "verify-1", model.NodeFailed)
	requireAttemptFailure(t, got, model.AttemptKey{Node: "verify-1", Number: 1}, model.CodeSemanticReviewFailed)

	st2 := fixtureRunningVerifyNode()
	got2, err := decision.Decide(st2, reviewEnded("review-provider-2", "no verdict line here"))
	requireNoError(t, err)
	requireNode(t, got2, "verify-1", model.NodeFailed)
	requireAttemptFailure(t, got2, model.AttemptKey{Node: "verify-1", Number: 1}, model.CodeSemanticReviewFailed)
}

// ---------------------------------------------------------------------------
// the serial Integration merge chain (design 15.5)
// ---------------------------------------------------------------------------

// TestMergeDispatchRequestsSerialMerge: the merge allocation commits the
// RUNNING Attempt at the current Integration HEAD and fixes the exact
// Task Branch and accepted Commit the --no-ff merge must bring in.
func TestMergeDispatchRequestsSerialMerge(t *testing.T) {
	st := fixtureRunningMergeNode()
	st.Nodes["merge-1"].Status = model.NodePending
	got, err := decision.Decide(st, model.DispatchInput{Node: "merge-1"})
	requireNoError(t, err)
	requireNode(t, got, "merge-1", model.NodeRunning)
	requireEffect(t, got, model.IntegrationMergeIntent{
		Node: "merge-1", BaseHead: "int-1",
		TaskBranch: "cflow/wf-1/task-task-1", VerifiedCommit: "task-head-1",
	})
}

// TestCommittedMergeFailureRequestsRollbackAndSettles (review fix #1):
// a committed merge that failed its post-merge checks returns the typed
// failure; the Kernel requests the recorded Integration Rollback with
// the observed code, and once the managed Worktree is restored the
// Attempt settles with that code — the workflow never wedges on a
// RUNNING merge Attempt.
func TestCommittedMergeFailureRequestsRollbackAndSettles(t *testing.T) {
	st := fixtureRunningMergeNode()
	key := model.AttemptKey{Node: "merge-1", Number: 1}
	got, err := decision.Decide(st, model.EffectResultInput{
		Kind: model.IntegrationMergeFailed, Attempt: key,
		PreMergeHead: "int-1", FailureCode: model.CodeCommitPolicyMismatch,
		Reason: "merge commit identity does not match the preflight",
	})
	requireNoError(t, err)
	requireEffect(t, got, model.IntegrationRollbackIntent{
		Head: "int-1", Attempt: key, FailureCode: model.CodeCommitPolicyMismatch,
	})

	// The rollback restored the Worktree: the Attempt settles with the
	// observed code, and the failed merge Commit stays as captured
	// evidence.
	got2, err := decision.Decide(st, model.EffectResultInput{
		Kind: model.IntegrationRollbacked, Attempt: key,
		FailureCode: model.CodeCommitPolicyMismatch, EndHead: "int-1",
		Evidence: model.EvidenceRef{Kind: model.EvidenceCommit, Hash: "merge-head-1", Subject: "integration"},
	})
	requireNoError(t, err)
	requireNode(t, got2, "merge-1", model.NodeFailed)
	requireAttemptFailure(t, got2, key, model.CodeCommitPolicyMismatch)
}

// TestMergeConflictRequestsRollbackAndSettles: a text conflict follows
// the same chain and settles with MERGE_CONFLICT.
func TestMergeConflictRequestsRollbackAndSettles(t *testing.T) {
	st := fixtureRunningMergeNode()
	key := model.AttemptKey{Node: "merge-1", Number: 1}
	got, err := decision.Decide(st, model.EffectResultInput{
		Kind: model.IntegrationMergeFailed, Attempt: key,
		PreMergeHead: "int-1", FailureCode: model.CodeMergeConflict,
		Reason: "text conflict",
	})
	requireNoError(t, err)
	requireEffect(t, got, model.IntegrationRollbackIntent{
		Head: "int-1", Attempt: key, FailureCode: model.CodeMergeConflict,
	})
	got2, err := decision.Decide(st, model.EffectResultInput{
		Kind: model.IntegrationRollbacked, Attempt: key, EndHead: "int-1",
	})
	requireNoError(t, err)
	requireNode(t, got2, "merge-1", model.NodeFailed)
	requireAttemptFailure(t, got2, key, model.CodeMergeConflict)
}

// TestIntegrationMergeSuccessAdvancesHead: a verified --no-ff merge
// settles the Attempt SUCCEEDED and advances the aggregate's
// IntegrationHead to the Merge Commit (only verified merges move it).
func TestIntegrationMergeSuccessAdvancesHead(t *testing.T) {
	st := fixtureRunningMergeNode()
	key := model.AttemptKey{Node: "merge-1", Number: 1}
	got, err := decision.Decide(st, model.EffectResultInput{
		Kind: model.IntegrationMerged, Attempt: key, EndHead: "int-2",
		Evidence: model.EvidenceRef{Kind: model.EvidenceCommit, Hash: "int-2", Subject: "cflow/integration/wf-1"},
	})
	requireNoError(t, err)
	requireNode(t, got, "merge-1", model.NodeSucceeded)
	var advanced bool
	for _, m := range got.Mutations {
		if wm, ok := m.(model.WorkflowMutation); ok && wm.IntegrationHead == "int-2" {
			advanced = true
		}
	}
	if !advanced {
		t.Fatalf("merge success did not advance the recorded integration head: %+v", got.Mutations)
	}
}
