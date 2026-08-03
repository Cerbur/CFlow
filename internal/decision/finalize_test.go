// The final acceptance chain (Task 18, design 16.2, PRD 最终验收): the
// Final Verify Node runs every approved final-verify Catalog entry over
// the full Integration range inside the Integration Worktree, an
// independent Final Reviewer Session (FINAL_VERIFICATION purpose) judges
// the integration result bound to the exact Integration HEAD, and the
// Workflow completes only with exact evidence: every Node SUCCEEDED, no
// Blocking Finding, and the bound Integration HEAD unchanged. The Final
// Reviewer can never reuse the implementer's Session lineage, and a
// completion whose evidence subject moved fails closed with
// EVIDENCE_SUBJECT_CHANGED. Completion records COMPLETED without
// changing the Target Branch.
package decision_test

import (
	"testing"

	"cflow.local/cflow/internal/decision"
	"cflow.local/cflow/internal/model"
)

// applyDecision applies one Decision's mutations to the fixture state in
// order, mirroring the Store's in-order application for the subset the
// final acceptance chain produces (the review settle and the completion).
func applyDecision(t *testing.T, st *model.State, d model.Decision) {
	t.Helper()
	for _, m := range d.Mutations {
		switch mm := m.(type) {
		case model.WorkflowMutation:
			st.Workflow.ID, st.Workflow.Project = mm.ID, mm.Project
			st.Workflow.Stage, st.Workflow.Runtime = mm.Stage, mm.Runtime
			st.Workflow.TargetBranch, st.Workflow.BaseCommit = mm.TargetBranch, mm.BaseCommit
			st.Workflow.IntegrationBranch, st.Workflow.IntegrationHead = mm.IntegrationBranch, mm.IntegrationHead
			st.Workflow.CancelIntent = mm.CancelIntent
		case model.RunMutation:
			for i := range st.Runs {
				if st.Runs[i].ID == mm.ID {
					st.Runs[i].Status = mm.Status
					st.Runs[i].DispatchGate = mm.DispatchGate
				}
			}
		case model.NodeStatusMutation:
			if n := st.Nodes[mm.Node]; n != nil {
				n.Status = mm.Status
				n.RetryCharged = mm.RetryCharged
			}
		case model.SessionAppendMutation:
			st.Sessions = append(st.Sessions, mm.Session)
		case model.SessionEndMutation:
			for i := range st.Sessions {
				if st.Sessions[i].ID == mm.ID {
					st.Sessions[i].Status = mm.Status
					st.Sessions[i].ProviderSessionID = mm.ProviderSessionID
				}
			}
		case model.AttemptAppendMutation:
			if st.Attempts == nil {
				st.Attempts = map[model.AttemptKey]*model.Attempt{}
			}
			att := mm.Attempt
			st.Attempts[att.Key] = &att
		case model.AttemptEndMutation:
			if a := st.Attempts[mm.Key]; a != nil {
				a.Status = mm.Status
				a.EndHead = mm.EndHead
				a.EndDirtyFingerprint = mm.EndDirtyFingerprint
				a.FailureCode = mm.FailureCode
				a.Evidence = mm.Evidence
				a.RetryCharged = mm.RetryCharged
				a.EndedAt = mm.EndedAt
			}
		case model.FindingAppendMutation:
			st.Findings = append(st.Findings, mm.Finding)
		case model.ProcessAppendMutation:
			st.Processes = append(st.Processes, mm.Process)
		case model.ProcessEndMutation:
			for i := range st.Processes {
				if st.Processes[i].ID == mm.ID {
					st.Processes[i].Status = mm.Status
					st.Processes[i].EndedAt = mm.EndedAt
				}
			}
		}
	}
}

// completedTasksFixture is the final acceptance state: every delivery
// chain Node of the approved execution is SUCCEEDED with immutable
// evidence, the Workflow is at the FINAL_VERIFICATION stage with the
// Final Verify Node RUNNING and its Final Reviewer Session bound to the
// Integration HEAD (StartHead "int-1"), and the implementer Session
// carries the provider session id the reuse test must never share.
func completedTasksFixture() model.State {
	st := workflowState(model.StageFinalVerification, model.RuntimeRunning)
	st.Workflow.ExecutionFacts = &model.ExecutionFacts{
		PlanHash: "plan-1", SpecHashes: []string{"spec-1"}, CatalogHash: "cat-1",
		WorkflowHash: "wf-1", CatalogRevision: 1,
	}
	addRun(&st, model.RunRunning, true)
	chain := []struct {
		id   string
		kind model.NodeKind
	}{
		{"task-1", model.NodeAgentTask}, {"verify-1", model.NodeVerify}, {"merge-1", model.NodeMerge},
		{"task-2", model.NodeAgentTask}, {"verify-2", model.NodeVerify}, {"merge-2", model.NodeMerge},
	}
	for _, c := range chain {
		addNode(&st, c.id, c.kind, model.NodeSucceeded, 0)
		key := addAttempt(&st, c.id, 1, model.AttemptSucceeded)
		st.Attempts[key].EndHead = "int-1"
		st.Attempts[key].Evidence = []model.EvidenceRef{
			{Kind: model.EvidenceCommit, Hash: "int-1", Subject: "cflow/integration/wf-1"},
		}
		st.Attempts[key].EndedAt = fixedNow
	}
	addNode(&st, "final-verify", model.NodeFinalVerify, model.NodeRunning, 0)
	fkey := addAttempt(&st, "final-verify", 1, model.AttemptRunning)
	st.Attempts[fkey].Session = "final-review-1"
	st.Attempts[fkey].StartHead = "int-1"
	st.Attempts[fkey].StartedAt = fixedNow
	st.Sessions = append(st.Sessions,
		model.Session{ID: "final-review-1", Purpose: model.PurposeFinalVerification,
			Status: model.SessionStarting, Provider: "fake"},
		model.Session{ID: "impl-1", Purpose: model.PurposeImplementation,
			Status: model.SessionCompleted, ProviderSessionID: "impl-provider-1"},
	)
	return st
}

// finalReviewEnded builds the settled Final Reviewer Session result
// input bound to the Final Verify Node's manifest.
func finalReviewEnded(providerSessionID string, body string) model.Input {
	return model.EffectResultInput{
		Kind: model.ProviderRunEnded,
		Session: model.Session{
			ID: "final-review-1", ProviderSessionID: providerSessionID,
			Purpose: model.PurposeFinalVerification, Status: model.SessionCompleted,
		},
		Body:         []byte(body),
		ManifestHash: "final-manifest-1",
	}
}

// ---------------------------------------------------------------------------
// the Final Verify chain (PRD 最终验收: 全量构建与测试 + 独立 Final Reviewer)
// ---------------------------------------------------------------------------

// TestFinalVerifyDispatchRunsFullRangeInIntegration: the Final Verify
// allocation commits the RUNNING Attempt at the recorded Integration
// HEAD, records the fresh Final Reviewer Session of the FINAL_VERIFICATION
// purpose, moves the Workflow to the FINAL_VERIFICATION stage, and
// requests the approved final-verify Catalog command over the full
// Integration range.
func TestFinalVerifyDispatchRunsFullRangeInIntegration(t *testing.T) {
	st := completedTasksFixture()
	st.Nodes["final-verify"].Status = model.NodePending
	st.Workflow.Stage = model.StageExecution
	got, err := decision.Decide(st, model.DispatchInput{Node: "final-verify", Session: "final-review-2", Route: "fake"})
	requireNoError(t, err)
	requireNode(t, got, "final-verify", model.NodeRunning)
	requireStatus(t, got, model.StageFinalVerification, model.RuntimeRunning)
	requireEffect(t, got, model.VerificationRunIntent{
		Node:        "final-verify",
		Catalog:     model.CatalogRef{Revision: 1, Hash: "cat-1"},
		CommitRange: "base-1..int-1",
	})
	var reviewSession model.SessionID
	for _, m := range got.Mutations {
		sm, ok := m.(model.SessionAppendMutation)
		if !ok {
			continue
		}
		if sm.Session.Purpose != model.PurposeFinalVerification {
			t.Fatalf("final verify records a non-final-verification session: %+v", sm.Session)
		}
		reviewSession = sm.Session.ID
	}
	if reviewSession != "final-review-2" {
		t.Fatalf("final verify did not record the fresh final reviewer session")
	}
	for _, m := range got.Mutations {
		if am, ok := m.(model.AttemptAppendMutation); ok && am.Attempt.Key.Node == "final-verify" {
			if am.Attempt.StartHead != "int-1" {
				t.Fatalf("final verify attempt binds head %s, want int-1", am.Attempt.StartHead)
			}
		}
	}
}

// TestFinalVerifyDispatchRejectsImplementerSession: the Final Reviewer
// must be a fresh Session of the FINAL_VERIFICATION purpose; the
// implementer's Session identity is refused at allocation.
func TestFinalVerifyDispatchRejectsImplementerSession(t *testing.T) {
	st := completedTasksFixture()
	st.Nodes["final-verify"].Status = model.NodePending
	_, err := decision.Decide(st, model.DispatchInput{Node: "final-verify", Session: "impl-1", Route: "fake"})
	if err == nil {
		t.Fatal("final verify dispatch accepted the implementer session")
	}
}

// TestFinalVerificationFailureBlocksWithIntegrationVerificationFailed:
// a failed final Verification settles the Attempt with
// INTEGRATION_VERIFICATION_FAILED (never retryable) and cancels the
// never-started Final Reviewer Session.
func TestFinalVerificationFailureBlocksWithIntegrationVerificationFailed(t *testing.T) {
	st := completedTasksFixture()
	got, err := decision.Decide(st, model.EffectResultInput{
		Kind:    model.VerificationRunEnded,
		Attempt: model.AttemptKey{Node: "final-verify", Number: 1},
		Passed:  false, ManifestHash: "final-manifest-1",
	})
	requireNoError(t, err)
	requireNode(t, got, "final-verify", model.NodeFailed)
	requireSessionSettled(t, got, "final-review-1", model.SessionCancelled)
	requireAttemptFailure(t, got, model.AttemptKey{Node: "final-verify", Number: 1}, model.CodeIntegrationVerificationFailed)
	requireStatus(t, got, model.StageFinalVerification, model.RuntimeBlocked)
}

// TestFinalVerificationPassRequestsFinalReviewer: a passed final run
// starts the independent Final Reviewer Session on the recorded review
// route with the FINAL_VERIFICATION purpose.
func TestFinalVerificationPassRequestsFinalReviewer(t *testing.T) {
	st := completedTasksFixture()
	got, err := decision.Decide(st, model.EffectResultInput{
		Kind:    model.VerificationRunEnded,
		Attempt: model.AttemptKey{Node: "final-verify", Number: 1},
		Passed:  true, ManifestHash: "final-manifest-1",
	})
	requireNoError(t, err)
	requireEffect(t, got, model.ProviderStartIntent{
		Session: "final-review-1", Purpose: model.PurposeFinalVerification,
		Route: "fake", Node: "final-verify",
	})
}

// TestFinalReviewerSessionReuseFailsClosed: the Final Reviewer must
// never share the implementer's provider session id (design 14.4) — the
// final review verdict is rejected with SESSION_INDEPENDENCE_VIOLATION.
func TestFinalReviewerSessionReuseFailsClosed(t *testing.T) {
	st := completedTasksFixture()
	_, err := decision.Decide(st, finalReviewEnded("impl-provider-1", `{"decision":"PASS","report":"PASS\n"}`))
	assertFaultCode(t, err, model.CodeSessionIndependenceViolation)
}

// TestFinalReviewVerdictPassSettlesFinalVerify: a passed final review
// verdict is evidence — the Final Verify Attempt settles SUCCEEDED with
// the final test-result and review-result evidence, and the Workflow
// stays at FINAL_VERIFICATION awaiting the exact-evidence completion.
func TestFinalReviewVerdictPassSettlesFinalVerify(t *testing.T) {
	st := completedTasksFixture()
	got, err := decision.Decide(st, finalReviewEnded("final-review-provider-1", `{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n"}`))
	requireNoError(t, err)
	requireNode(t, got, "final-verify", model.NodeSucceeded)
	requireEvent(t, got, model.EventAttemptSucceeded)
	requireEvent(t, got, model.EventNodeSucceeded)
	for _, m := range got.Mutations {
		if am, ok := m.(model.AttemptEndMutation); ok && am.Key.Node == "final-verify" {
			if len(am.Evidence) != 2 {
				t.Fatalf("final review pass carries %d evidence refs, want 2", len(am.Evidence))
			}
		}
	}
}

// TestFinalReviewVerdictFailSettlesSemanticReviewFailed: a failed final
// review verdict (or an unparsable report) settles the Final Verify
// Attempt with SEMANTIC_REVIEW_FAILED; the Final Reviewer pass is never
// the Runtime deciding success.
func TestFinalReviewVerdictFailSettlesSemanticReviewFailed(t *testing.T) {
	st := completedTasksFixture()
	got, err := decision.Decide(st, finalReviewEnded("final-review-provider-2", `{"decision":"FAIL","report":"FAIL\n\nblocking issue\n"}`))
	requireNoError(t, err)
	requireNode(t, got, "final-verify", model.NodeFailed)
	requireAttemptFailure(t, got, model.AttemptKey{Node: "final-verify", Number: 1}, model.CodeSemanticReviewFailed)

	st2 := completedTasksFixture()
	got2, err := decision.Decide(st2, finalReviewEnded("final-review-provider-3", "no verdict line here"))
	requireNoError(t, err)
	requireNode(t, got2, "final-verify", model.NodeFailed)
	requireAttemptFailure(t, got2, model.AttemptKey{Node: "final-verify", Number: 1}, model.CodeSemanticReviewFailed)
}

// ---------------------------------------------------------------------------
// completion (PRD 最终验收: 生成最终报告，Workflow Completed)
// ---------------------------------------------------------------------------

// TestCompletionRequiresEveryNodeSucceeded: completion refuses a
// Workflow that still carries a PENDING or FAILED Node.
func TestCompletionRequiresEveryNodeSucceeded(t *testing.T) {
	st := completedTasksFixture()
	st.Nodes["merge-2"].Status = model.NodePending
	_, err := decision.Decide(st, model.CompleteWorkflowInput{})
	if err == nil {
		t.Fatal("completion accepted a workflow with a pending node")
	}
}

// TestCompletionRefusesBlockingFindings: a Blocking Finding prevents
// completion even when every Node succeeded.
func TestCompletionRefusesBlockingFindings(t *testing.T) {
	st := completedTasksFixture()
	addFinding(&st, "f-1", model.CodeIntegrationVerificationFailed, "final-verify")
	_, err := decision.Decide(st, model.CompleteWorkflowInput{})
	if err == nil {
		t.Fatal("completion accepted a blocking finding")
	}
}

// TestCompletionRecordsCompletedWithoutChangingTarget: with the exact
// evidence matched (the Final Reviewer passed and the Integration HEAD
// still equals the head it verified), the Workflow records
// COMPLETED/SUCCEEDED, the Run succeeds, and the Target Branch is
// carried unchanged.
func TestCompletionRecordsCompletedWithoutChangingTarget(t *testing.T) {
	st := completedTasksFixture()
	settled, err := decision.Decide(st, finalReviewEnded("final-review-provider-1", `{"decision":"PASS","report":"PASS\n"}`))
	requireNoError(t, err)
	requireNode(t, settled, "final-verify", model.NodeSucceeded)
	applyDecision(t, &st, settled)
	got, err := decision.Decide(st, model.CompleteWorkflowInput{})
	requireNoError(t, err)
	requireStatus(t, got, model.StageCompleted, model.RuntimeSucceeded)
	requireRun(t, got, model.RunSucceeded, false)
	requireEvent(t, got, model.EventWorkflowSucceeded)
	requireEvent(t, got, model.EventRunSucceeded)
	var last *model.WorkflowMutation
	for _, m := range got.Mutations {
		if wm, ok := m.(model.WorkflowMutation); ok {
			last = &wm
		}
	}
	if last.TargetBranch != "main" || last.IntegrationBranch != "cflow/integration/wf-1" || last.IntegrationHead != "int-1" {
		t.Fatalf("completion changed protected facts: %+v", last)
	}
}

// TestFinalReviewerMustBeIndependentAndBoundToIntegrationHead is the
// mandated brief test: the Final Reviewer Session must be independent
// (SESSION_INDEPENDENCE_VIOLATION on implementer lineage reuse), and
// completion is bound to the exact Integration HEAD the Final Reviewer
// verified (EVIDENCE_SUBJECT_CHANGED when the head moved first).
func TestFinalReviewerMustBeIndependentAndBoundToIntegrationHead(t *testing.T) {
	fx := completedTasksFixture()
	review, err := decision.Decide(fx, finalReviewEnded("impl-provider-1", `{"decision":"PASS","report":"PASS\n"}`))
	if err == nil {
		t.Fatalf("expected a review error, got decision %+v", review)
	}
	assertFaultCode(t, err, model.CodeSessionIndependenceViolation)

	got, err := decision.Decide(fx, finalReviewEnded("final-review-provider-1", `{"decision":"PASS","report":"PASS\n"}`))
	requireNoError(t, err)
	requireNode(t, got, "final-verify", model.NodeSucceeded)
	applyDecision(t, &fx, got)

	// The Integration HEAD advanced after the Final Reviewer bound it.
	fx.Workflow.IntegrationHead = "int-2"

	_, err = decision.Decide(fx, model.CompleteWorkflowInput{})
	assertFaultCode(t, err, model.CodeEvidenceSubjectChanged)
}

// TestCheckpointObservationSettlesWithoutGatingFinalVerify (Task 18):
// an observation checkpoint whose Merge dependency succeeded settles
// SUCCEEDED with a non-blocking Finding — it never claims a Provider run
// and never gates the Final Verify permanently.
func TestCheckpointObservationSettlesWithoutGatingFinalVerify(t *testing.T) {
	st := completedTasksFixture()
	st.Workflow.Stage = model.StageExecution
	addNode(&st, "checkpoint-1", model.NodeCheckpoint, model.NodePending, 0)
	got, err := decision.Decide(st, model.DispatchInput{Node: "checkpoint-1"})
	requireNoError(t, err)
	requireNode(t, got, "checkpoint-1", model.NodeSucceeded)
	requireNoEffect(t, got)
	requireEvent(t, got, model.EventNodeSucceeded)
	finding := false
	for _, m := range got.Mutations {
		fm, ok := m.(model.FindingAppendMutation)
		if !ok || fm.Finding.Blocking {
			continue
		}
		finding = true
	}
	if !finding {
		t.Fatalf("checkpoint settle records no non-blocking observation finding")
	}
}
