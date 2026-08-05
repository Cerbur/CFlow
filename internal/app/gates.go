package app

// The Task 13 executors: the Task Commit/Clean/Scope gate, the
// deterministic Verification run, the independent Reviewer Session, the
// serial --no-ff Integration merge and its conflict rollback, and the
// append-only audit Ref. Same-package split of the Application seam: no
// public seam added. Every executor reports typed facts; the Kernel
// settles Attempts (design 6.2 rule 5).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/compile"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/verify"
)

// verificationRun executes one approved Catalog entry through the
// Verification Engine (design 16.2) and persists the Evidence Manifest
// to the managed evidence root. Engine-level failures (identity drift,
// executable mismatch) become typed failed results, never dangling
// Attempts. The Final Verify Node (Task 18, PRD 最终验收) runs the
// approved final-verify Catalog command over the full Integration range
// inside the Integration Worktree.
func (a *Application) verificationRun(ctx context.Context, wf model.WorkflowID, intent model.VerificationRunIntent) (model.EffectResultInput, error) {
	fail := func(code model.Code, text string) model.EffectResultInput {
		return model.EffectResultInput{
			Kind:         model.VerificationRunEnded,
			Attempt:      a.runningAttemptKey(ctx, wf, intent.Node),
			Passed:       false,
			FailureCode:  code,
			PreMergeHead: "",
			Reason:       text,
		}
	}
	if _, err := a.readCatalogBody(ctx, wf, intent.Catalog); err != nil {
		return fail(model.CodeEvidenceSubjectChanged, "catalog body cannot be read"), nil
	}
	commandID, _, purpose, worktree, err := a.verificationNodeFacts(ctx, wf, intent.Node)
	if err != nil {
		return fail(model.CodeEvidenceSubjectChanged, err.Error()), nil
	}
	engine, err := verify.NewEngine(verify.EngineOptions{
		Supervisor: a.supervisor, GitFlow: a.git, Redaction: a.redaction,
		LoadCatalog: func(ctx context.Context, ref model.CatalogRef) ([]byte, error) {
			return a.readCatalogBody(ctx, wf, ref)
		},
	})
	if err != nil {
		return fail(model.CodeEvidenceSubjectChanged, "verification engine cannot be built"), nil
	}
	manifest, err := engine.Run(ctx, verify.VerificationRequest{
		Node:        intent.Node,
		Catalog:     intent.Catalog,
		CommandID:   commandID,
		Purpose:     purpose,
		Worktree:    worktree,
		CommitRange: intent.CommitRange,
	})
	if err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}
	if err := a.writeVerificationManifest(wf, intent.Node, manifest); err != nil {
		return fail(model.CodeSensitiveDataRedactionFailed, "verification evidence cannot be persisted"), nil
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return fail(model.CodeSensitiveDataRedactionFailed, "verification evidence cannot be serialized"), nil
	}
	return model.EffectResultInput{
		Kind:         model.VerificationRunEnded,
		Attempt:      a.runningAttemptKey(ctx, wf, intent.Node),
		Passed:       manifest.Passed,
		Manifest:     body,
		ManifestHash: manifest.Hash,
		Reason:       manifest.Reason,
	}, nil
}

// integrationMerge performs one serial --no-ff Integration merge (design
// 15.5): the pre-merge facts are re-observed (recorded pre-merge HEAD,
// clean Integration Worktree, accepted Commit still the Task Branch
// HEAD), the Commit Preflight runs before the merge, and the Merge
// Commit's identity is verified after it. A text conflict or a failed
// post-merge check returns a typed failed result carrying the recorded
// pre-merge HEAD; the Kernel requests the Integration Rollback.
func (a *Application) integrationMerge(ctx context.Context, wf model.WorkflowID, intent model.IntegrationMergeIntent) (model.EffectResultInput, error) {
	attempt := a.runningAttemptKey(ctx, wf, intent.Node)
	fail := func(code model.Code, reason string) model.EffectResultInput {
		return model.EffectResultInput{
			Kind: model.IntegrationMergeFailed, Attempt: attempt,
			FailureCode: code, PreMergeHead: intent.BaseHead, Reason: reason,
		}
	}
	path := a.integrationWorktreePath(wf)
	if a.git == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}

	// Pre-merge facts (PRD: Merge 前再次比较已验收 Commit、Task Branch HEAD
	// 和 Git-clean 状态).
	status, err := a.observeWorktree(ctx, path, intent.BaseHead)
	if err != nil {
		return fail(model.CodeStateInvariantViolation, "pre-merge integration status is unreadable"), nil
	}
	if !status.Clean() {
		return fail(model.CodeDirtyTaskWorktree, "the integration worktree is not git-clean before the merge"), nil
	}
	refFacts, err := a.git.Observe(ctx, gitflow.RefLookup{Ref: "refs/heads/" + intent.TaskBranch})
	if err != nil {
		return fail(model.CodeStateInvariantViolation, "task branch facts are unreadable"), nil
	}
	rf, ok := refFacts.(gitflow.RefFacts)
	if !ok || !rf.Exists || rf.Value != intent.VerifiedCommit {
		return fail(model.CodeEvidenceSubjectChanged, "the task branch moved after verification"), nil
	}

	// The Commit Preflight runs before the Merge Commit is created (PRD
	// 已确认：Merge Conflict 处理).
	preflightRes, err := a.git.Execute(ctx, gitflow.CommitPreflight{
		Revision: fmt.Sprintf("merge-%s-%s", wf, attempt.Node),
	})
	if err != nil {
		return fail(model.CodeGitIdentityNotConfigured, "merge commit preflight failed"), nil
	}
	preflight, ok := preflightRes.(gitflow.PreflightEvidence)
	if !ok {
		return fail(model.CodeStateInvariantViolation, "merge preflight result has an unexpected type"), nil
	}

	res, err := a.git.Execute(ctx, gitflow.MergeIntegration{Path: path, Branch: intent.TaskBranch})
	if err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, "the integration merge did not complete"), nil
	}
	switch r := res.(type) {
	case gitflow.MergeConflictResult:
		return fail(model.CodeMergeConflict, "text conflict; the integration worktree stays at the pre-merge head for the rollback"), nil
	case gitflow.MergeResult:
		// The Merge Commit's identity must match the just-run Preflight.
		if _, err := a.git.Execute(ctx, gitflow.VerifyCommit{
			Ref: r.Head, ExpectedAuthor: preflight.Author,
			ExpectedCommitter: preflight.Committer, ExpectedSigning: preflight.Signing,
		}); err != nil {
			return fail(model.CodeCommitPolicyMismatch, "merge commit identity does not match the preflight"), nil
		}
		// Post-merge check: the Worktree is clean at the Merge Commit.
		post, err := a.observeWorktree(ctx, path, r.Head)
		if err != nil {
			return fail(model.CodeStateInvariantViolation, "post-merge status is unreadable"), nil
		}
		if !post.Clean() {
			return fail(model.CodeMergeConflict, "post-merge verification failed: the integration worktree is not git-clean"), nil
		}
		return model.EffectResultInput{
			Kind: model.IntegrationMerged, Attempt: attempt,
			EndHead: r.Head,
			Evidence: model.EvidenceRef{
				Kind: model.EvidenceCommit, Hash: r.Head, Subject: intent.TaskBranch,
			},
			EvidenceRefs: []model.EvidenceRef{
				{Kind: model.EvidenceCommit, Hash: r.Head, Subject: intent.TaskBranch},
				{Kind: model.EvidenceGitSnapshot, Hash: r.Head, Subject: "integration"},
			},
		}, nil
	default:
		return fail(model.CodeStateInvariantViolation, "merge result has an unexpected type"), nil
	}
}

// integrationRollback restores the managed Integration Worktree to the
// recorded pre-merge HEAD (RollbackMerge: `git merge --abort` for a
// conflicted merge, the guarded reset for a committed merge that failed
// its post-merge checks; PRD 已确认：Merge Conflict 处理). Only the managed
// Integration Worktree is touched. When a committed merge is rolled
// back, the failed Merge Commit's hash is captured as evidence before
// the restore, so the failure stays auditable (PRD: 保留失败 Commit 证
// 据); the typed FailureCode rides the result so the Attempt settles with
// the code the executor observed.
func (a *Application) integrationRollback(ctx context.Context, wf model.WorkflowID, intent model.IntegrationRollbackIntent) (model.EffectResultInput, error) {
	if a.git == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	path := a.integrationWorktreePath(wf)
	evidence := []model.EvidenceRef{
		{Kind: model.EvidenceGitSnapshot, Hash: intent.Head, Subject: "integration"},
	}
	// A committed merge that failed its post-merge checks advanced the
	// head; its Commit is captured as evidence before the restore.
	if pre, err := a.observeWorktree(ctx, path, ""); err == nil && pre.Head != "" && pre.Head != intent.Head {
		evidence = append([]model.EvidenceRef{
			{Kind: model.EvidenceCommit, Hash: pre.Head, Subject: "integration"},
		}, evidence...)
	}
	res, err := a.git.Execute(ctx, gitflow.RollbackMerge{Path: path, ExpectedHead: intent.Head})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	rr, ok := res.(gitflow.RollbackResult)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("rollback result has an unexpected type"))
	}
	return model.EffectResultInput{
		Kind: model.IntegrationRollbacked, Attempt: intent.Attempt,
		EndHead: rr.Head, FailureCode: intent.FailureCode,
		Evidence:     evidence[0],
		EvidenceRefs: evidence,
	}, nil
}

// gitAuditRefCreate creates one append-only audit Ref (expected-absent
// compare-and-swap, GitFlow).
func (a *Application) gitAuditRefCreate(ctx context.Context, intent model.GitAuditRefCreateIntent) (model.EffectResultInput, error) {
	if a.git == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	if _, err := a.git.Execute(ctx, gitflow.CreateAuditRef{Ref: intent.Ref, Head: intent.Head}); err != nil {
		return model.EffectResultInput{}, err
	}
	return model.EffectResultInput{Kind: model.GitAuditRefCreated}, nil
}

// reviewProviderStart runs the independent Reviewer Session (design
// 16.2, TASK_REVIEW purpose) inside the Task Worktree, bound to the
// exact Commit/Catalog/evidence refs through its typed input block. The
// Reviewer is a non-coding Session: the Worktree's HEAD and Git-visible
// state must be unchanged (UNEXPECTED_AGENT_MUTATION otherwise), and the
// result echoes the verification manifest hash so the Kernel can record
// the test-result evidence.
func (a *Application) reviewProviderStart(ctx context.Context, wf model.WorkflowID, intent model.ProviderStartIntent, cmd model.Input, rt *agent.Runtime) (model.EffectResultInput, error) {
	if rt == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("agent runtime is not configured for this application"))
	}
	prompt, ok := a.promptForPurpose(model.PurposeReview)
	if !ok {
		return model.EffectResultInput{}, model.InvalidInputFault("no embedded prompt for the review purpose")
	}
	_, taskNode, err := a.verifyNodeFacts(ctx, wf, intent.Node)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	cwd := a.taskWorktreePath(wf, taskNode)
	pre, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	input, err := a.reviewSessionInput(ctx, wf, intent.Node, taskNode)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	res, err := rt.Start(ctx, agent.StartRequest{
		Purpose:   intent.Purpose,
		Provider:  intent.Route,
		Prompt:    renderPrompt(prompt.Body, input),
		Input:     a.providerTypedInput(ctx, rt, intent.Purpose, intent.Route, input),
		CWD:       cwd,
		SessionID: intent.Session,
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if err := a.verifySnapshotUnchanged(ctx, cwd, pre); err != nil {
		return model.EffectResultInput{}, err
	}
	out, err := a.runOutcome(cmd, res)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	// The review verdict is the Session's terminal result (the Kernel
	// judges the structured PASS/FAIL verdict); the planning body
	// assembler knows no dispatch commands, so the body is carried
	// explicitly.
	if res.Terminal != nil {
		out.Body = []byte(res.Terminal.Result)
	}
	// The review result carries the verification manifest hash so the
	// Kernel binds the deterministic test-result evidence to the review
	// pass (design 16.2: review never replaces deterministic
	// verification).
	out.ManifestHash = a.verificationManifestHash(wf, intent.Node)
	return out, nil
}

// finalReviewProviderStart runs the independent Final Reviewer Session
// (Task 18, PRD 最终验收: 独立 Final Reviewer). The Final Reviewer is a
// non-coding Session inside the Integration Worktree, bound to the exact
// Plan/Spec/Catalog/Workflow refs, the Integration Branch and HEAD, the
// Target Branch, and the deterministic Final Verification Manifest. The
// Worktree's HEAD and Git-visible state must be unchanged
// (UNEXPECTED_AGENT_MUTATION otherwise); the result echoes the final
// verification manifest hash so the Kernel records the deterministic
// test-result evidence with the review pass.
func (a *Application) finalReviewProviderStart(ctx context.Context, wf model.WorkflowID, intent model.ProviderStartIntent, cmd model.Input, rt *agent.Runtime) (model.EffectResultInput, error) {
	if rt == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("agent runtime is not configured for this application"))
	}
	prompt, ok := a.promptForPurpose(model.PurposeFinalVerification)
	if !ok {
		return model.EffectResultInput{}, model.InvalidInputFault("no embedded prompt for the final review purpose")
	}
	cwd := a.integrationWorktreePath(wf)
	pre, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	input, err := a.finalReviewSessionInput(ctx, wf, intent.Node)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	res, err := rt.Start(ctx, agent.StartRequest{
		Purpose:   intent.Purpose,
		Provider:  intent.Route,
		Prompt:    renderPrompt(prompt.Body, input),
		Input:     a.providerTypedInput(ctx, rt, intent.Purpose, intent.Route, input),
		CWD:       cwd,
		SessionID: intent.Session,
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if err := a.verifySnapshotUnchanged(ctx, cwd, pre); err != nil {
		return model.EffectResultInput{}, err
	}
	out, err := a.runOutcome(cmd, res)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if res.Terminal != nil {
		out.Body = []byte(res.Terminal.Result)
	}
	out.ManifestHash = a.verificationManifestHash(wf, intent.Node)
	return out, nil
}

// finalReviewSessionInput builds the Final Reviewer's typed input block:
// the Plan, the Spec set, the Verification Catalog, the compiled
// Workflow, the Integration Branch and HEAD, the Target Branch, and the
// deterministic Final Verification Manifest (the FINAL_REVIEW prompt's
// input contract). The Final Reviewer is bound to the exact refs and the
// Integration HEAD it verifies; completion later requires the same head.
func (a *Application) finalReviewSessionInput(ctx context.Context, wf model.WorkflowID, verifyNode model.NodeID) (any, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, err
	}
	manifestBody, err := a.readVerificationManifestFile(wf, verifyNode)
	if err != nil {
		return nil, err
	}
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return nil, err
	}
	st := view.State
	return struct {
		Plan              string `json:"plan"`
		Spec              string `json:"spec"`
		Catalog           string `json:"catalog"`
		Workflow          string `json:"workflow"`
		IntegrationBranch string `json:"integration_branch"`
		IntegrationHead   string `json:"integration_head"`
		TargetBranch      string `json:"target_branch"`
		Verification      string `json:"verification"`
	}{
		Plan:              string(readArtifact(ctx, store, wf, model.ArtifactPlan)),
		Spec:              string(readArtifact(ctx, store, wf, model.ArtifactSpec)),
		Catalog:           string(readArtifact(ctx, store, wf, model.ArtifactCatalog)),
		Workflow:          string(readArtifact(ctx, store, wf, model.ArtifactWorkflow)),
		IntegrationBranch: st.Workflow.IntegrationBranch,
		IntegrationHead:   st.Workflow.IntegrationHead,
		TargetBranch:      st.Workflow.TargetBranch,
		Verification:      string(manifestBody),
	}, nil
}

// reviewSessionInput builds the Reviewer's typed input block: the Spec,
// the Verification Catalog, the Task's Commit range, the Worktree, and
// the deterministic Verification Manifest (the TASK_REVIEW prompt's
// input contract).
func (a *Application) reviewSessionInput(ctx context.Context, wf model.WorkflowID, verifyNode, taskNode model.NodeID) (any, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, err
	}
	manifestBody, err := a.readVerificationManifestFile(wf, verifyNode)
	if err != nil {
		return nil, err
	}
	worktree := a.taskWorktreePath(wf, taskNode)
	return struct {
		Spec         string `json:"spec"`
		Catalog      string `json:"catalog"`
		CommitRange  string `json:"commit_range"`
		Worktree     string `json:"worktree"`
		Verification string `json:"verification"`
		Diff         string `json:"diff"`
	}{
		Spec:         string(readArtifact(ctx, store, wf, model.ArtifactSpec)),
		Catalog:      string(readArtifact(ctx, store, wf, model.ArtifactCatalog)),
		CommitRange:  manifestRange(manifestBody),
		Worktree:     worktree,
		Verification: string(manifestBody),
		Diff:         a.gitDiff(ctx, worktree, manifestRange(manifestBody)),
	}, nil
}

// gitDiff renders the bounded commit diff of one commit range inside a
// Task Worktree, so the independent Reviewer can judge the actual changes
// (the review prompt requires the diff; the Reviewer Session never runs
// executable commands itself). An empty or failing diff returns "".
func (a *Application) gitDiff(ctx context.Context, worktree, rangeSpec string) string {
	if worktree == "" || rangeSpec == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, "git", "-C", worktree, "diff", rangeSpec).CombinedOutput()
	if err != nil {
		return ""
	}
	if len(out) > 32*1024 {
		out = out[:32*1024]
	}
	return string(out)
}

// taskGateResult runs the Task Commit/Clean/Scope gate after the coding
// Session settles and builds the AttemptEnded input the Kernel judges
// (PRD 已确认：Provider 默认权限与 Commit/Clean Worktree Gate). A gate
// failure is a typed failed result with the PRD code; CFlow never fixes
// the Worktree itself.
func (a *Application) taskGateResult(ctx context.Context, wf model.WorkflowID, node model.NodeID, writeScope []string) (model.EffectResultInput, error) {
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	st := view.State
	attempt := runningAttemptOfState(st, node)
	if attempt == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("gate ran without a running attempt for %s", node))
	}
	nd := st.Nodes[node]
	if nd == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("gate ran without a node %s", node))
	}
	prior := priorAttemptEnd(st, node)
	preflight, ok := a.readPreflightEvidence(ctx, wf, st)
	if !ok {
		// The recorded Commit Preflight the Execution Approval bound is
		// missing: the Commit identity cannot be verified, so the gate
		// fails closed with COMMIT_POLICY_MISMATCH.
		return gateEnded(attempt.Key, model.CodeCommitPolicyMismatch, gateEndFacts(ctx, a, wf, node)), nil
	}
	engine, err := verify.NewEngine(verify.EngineOptions{
		Supervisor: a.supervisor, GitFlow: a.git, Redaction: a.redaction,
		LoadCatalog: func(ctx context.Context, ref model.CatalogRef) ([]byte, error) {
			return a.readCatalogBody(ctx, wf, ref)
		},
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	gate, err := engine.TaskGate(ctx, verify.TaskGateRequest{
		WorkflowID:      wf,
		TaskID:          string(node),
		TaskBranch:      nd.Branch,
		TaskBase:        nd.BaseCommit,
		AttemptNumber:   int(attempt.Key.Number),
		PriorAttemptEnd: prior,
		WriteScope:      writeScope,
		Author:          preflight.Author,
		Committer:       preflight.Committer,
		Signing:         preflight.Signing,
		Worktree:        a.taskWorktreePath(wf, node),
	})
	if err != nil {
		code, _ := model.CodeOf(err)
		return gateEnded(attempt.Key, code, gateEndFacts(ctx, a, wf, node)), nil
	}
	return model.EffectResultInput{
		Kind:     model.AttemptEnded,
		Attempt:  attempt.Key,
		Outcome:  model.OutcomeSucceeded,
		EndHead:  gate.Head,
		Evidence: model.EvidenceRef{Kind: model.EvidenceCommit, Hash: gate.Head, Subject: nd.Branch},
		EvidenceRefs: []model.EvidenceRef{
			{Kind: model.EvidenceCommit, Hash: gate.Head, Subject: nd.Branch},
			{Kind: model.EvidenceGitSnapshot, Hash: gate.Head, Subject: string(node)},
		},
	}, nil
}

// gateEnded builds the failed AttemptEnded input with the Worktree's
// current end facts (the PRD: a failed Attempt immutably records its
// start/end HEAD and Dirty Fingerprint).
func gateEnded(key model.AttemptKey, code model.Code, facts gitflow.StatusFacts) model.EffectResultInput {
	return model.EffectResultInput{
		Kind:                model.AttemptEnded,
		Attempt:             key,
		Outcome:             model.OutcomeFailed,
		FailureCode:         code,
		EndHead:             facts.Head,
		EndDirtyFingerprint: dirtyFingerprint(facts.Dirty),
	}
}

// gateEndFacts observes the Task Worktree's current end facts after a
// failed gate.
func gateEndFacts(ctx context.Context, a *Application, wf model.WorkflowID, node model.NodeID) gitflow.StatusFacts {
	facts, err := a.observeWorktree(ctx, a.taskWorktreePath(wf, node), "")
	if err != nil {
		return gitflow.StatusFacts{}
	}
	return facts
}

// runningAttemptOfState returns the RUNNING Attempt of one Node.
func runningAttemptOfState(st model.State, node model.NodeID) *model.Attempt {
	for k, a := range st.Attempts {
		if k.Node == node && a.Status == model.AttemptRunning {
			return a
		}
	}
	return nil
}

// readyAttemptOfState returns the highest-numbered READY Attempt of one
// Node (the current budgeted successor awaiting its dispatch; nil when
// none). The highest number is the live successor — lower-numbered READY
// markers are stale bookkeeping rows the retry chain leaves behind.
func readyAttemptOfState(st model.State, node model.NodeID) *model.Attempt {
	var found *model.Attempt
	for k, a := range st.Attempts {
		if k.Node != node || a.Status != model.AttemptReady {
			continue
		}
		if found == nil || k.Number > found.Key.Number {
			aa := a
			found = aa
		}
	}
	return found
}

// sessionOfState returns one Session of the aggregate (nil when
// unknown).
func sessionOfState(st model.State, id model.SessionID) *model.Session {
	for i := range st.Sessions {
		if st.Sessions[i].ID == id {
			return &st.Sessions[i]
		}
	}
	return nil
}

// priorAttemptEnd returns the terminal end HEAD of the previous Attempt
// of one Node ("" when none): the append-only anchor of the gate.
func priorAttemptEnd(st model.State, node model.NodeID) string {
	var best model.AttemptKey
	var end string
	for k, a := range st.Attempts {
		if k.Node != node || !a.Status.IsTerminal() {
			continue
		}
		if end == "" || k.Number > best.Number {
			best = k
			end = a.EndHead
		}
	}
	return end
}

// runningAttemptKey is the RUNNING Attempt key of one Node ("" when
// none).
func (a *Application) runningAttemptKey(ctx context.Context, wf model.WorkflowID, node model.NodeID) model.AttemptKey {
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return model.AttemptKey{}
	}
	if att := runningAttemptOfState(view.State, node); att != nil {
		return att.Key
	}
	return model.AttemptKey{}
}

// verifyNodeFacts parses the compiled Workflow Artifact and returns the
// verify Node's approved Catalog command id and its Task dependency's
// node id (the Worktree the verification runs inside).
func (a *Application) verifyNodeFacts(ctx context.Context, wf model.WorkflowID, node model.NodeID) (commandID string, taskNode model.NodeID, err error) {
	commandID, taskNode, _, _, err = a.verificationNodeFacts(ctx, wf, node)
	return commandID, taskNode, err
}

// verificationNodeFacts parses the compiled Workflow Artifact and returns
// one verification Node's approved Catalog command id, its Task
// dependency ("" for the Final Verify Node), the approved Catalog
// purpose, and the Worktree the verification runs inside: the Task
// Worktree for a verify Node, the Integration Worktree for the Final
// Verify Node (Task 18, PRD 最终验收: 全量构建与测试 over the full
// Integration range).
func (a *Application) verificationNodeFacts(ctx context.Context, wf model.WorkflowID, node model.NodeID) (commandID string, taskNode model.NodeID, purpose verify.Purpose, worktree string, err error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return "", "", "", "", err
	}
	body := readArtifact(ctx, store, wf, model.ArtifactWorkflow)
	wfIR, err := compile.ParseWorkflow(body)
	if err != nil {
		return "", "", "", "", fmt.Errorf("the compiled workflow cannot be parsed")
	}
	var found *compile.WorkflowNode
	for i := range wfIR.Nodes {
		if wfIR.Nodes[i].ID == string(node) {
			found = &wfIR.Nodes[i]
			break
		}
	}
	if found == nil {
		return "", "", "", "", fmt.Errorf("verify node is missing from the compiled workflow")
	}
	commandID = found.CommandID
	if commandID == "" {
		return "", "", "", "", fmt.Errorf("verify node references no catalog command")
	}
	if nodeKindOf(found) == model.NodeFinalVerify {
		return commandID, "", verify.PurposeFinalVerify, a.integrationWorktreePath(wf), nil
	}
	taskNode, err = taskDependencyNode(wfIR, found)
	if err != nil {
		return "", "", "", "", err
	}
	return commandID, taskNode, verify.PurposeTaskVerify, a.taskWorktreePath(wf, taskNode), nil
}

// nodeKindOf maps one compiled Workflow Node's type to its kernel kind
// ("" when the type is not a verification node).
func nodeKindOf(n *compile.WorkflowNode) model.NodeKind {
	if n == nil {
		return ""
	}
	switch n.Type {
	case "final_verify":
		return model.NodeFinalVerify
	case "verify":
		return model.NodeVerify
	}
	return ""
}

// taskDependencyNode resolves the agent-task dependency of one verify
// Node from the compiled workflow edges.
func taskDependencyNode(wfIR compile.Workflow, node *compile.WorkflowNode) (model.NodeID, error) {
	for _, e := range wfIR.Edges {
		if e.To != node.ID {
			continue
		}
		for _, n := range wfIR.Nodes {
			if n.ID == e.From && n.Type == "agent_task" {
				return model.NodeID(n.ID), nil
			}
		}
	}
	return "", fmt.Errorf("verify node has no task dependency in the compiled workflow")
}

// readCatalogBody reads one immutable Catalog Revision body by identity.
func (a *Application) readCatalogBody(ctx context.Context, wf model.WorkflowID, ref model.CatalogRef) ([]byte, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, err
	}
	return store.Get(ctx, model.ArtifactRef{
		Workflow: wf, Type: model.ArtifactCatalog, Revision: ref.Revision, Hash: ref.Hash,
	})
}

// readPreflightEvidence reads the recorded Commit Preflight report the
// Execution Approval bound (revision and hash from the aggregate's
// ExecutionFacts) and returns the immutable evidence the gate verifies
// the actual Commit against.
func (a *Application) readPreflightEvidence(ctx context.Context, wf model.WorkflowID, st model.State) (gitflow.PreflightEvidence, bool) {
	facts := st.Workflow.ExecutionFacts
	if facts == nil || facts.PreflightRevision < 1 || facts.CommitPolicyHash == "" {
		return gitflow.PreflightEvidence{}, false
	}
	store, err := a.artifactStore(wf)
	if err != nil {
		return gitflow.PreflightEvidence{}, false
	}
	body, err := store.Get(ctx, model.ArtifactRef{
		Workflow: wf, Type: model.ArtifactReport,
		Revision: facts.PreflightRevision, Hash: facts.CommitPolicyHash,
	})
	if err != nil {
		return gitflow.PreflightEvidence{}, false
	}
	// The report Artifact's canonical content is the report JSON encoded
	// as a JSON string (the Store canonicalizes every body per type).
	var raw string
	if err := json.Unmarshal(body, &raw); err != nil {
		return gitflow.PreflightEvidence{}, false
	}
	var ev gitflow.PreflightEvidence
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return gitflow.PreflightEvidence{}, false
	}
	return ev, true
}

// writeVerificationManifest persists one Evidence Manifest at the
// deterministic evidence path the Recovery Engine reads (design 17.1
// order 7): <evidence>/verification/<workflow>/<node>.json.
func (a *Application) writeVerificationManifest(wf model.WorkflowID, node model.NodeID, m model.EvidenceManifest) error {
	if a.agent.EvidenceDir == "" {
		return nil
	}
	dir := filepath.Join(a.agent.EvidenceDir, "verification", string(wf))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, string(node)+".json"), data, 0o600)
}

// readVerificationManifestFile reads the persisted manifest of one
// verify Node (nil when absent).
func (a *Application) readVerificationManifestFile(wf model.WorkflowID, node model.NodeID) ([]byte, error) {
	if a.agent.EvidenceDir == "" {
		return nil, nil
	}
	path := filepath.Join(a.agent.EvidenceDir, "verification", string(wf), string(node)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// verificationManifestHash is the persisted manifest's self-hash ("" when
// absent).
func (a *Application) verificationManifestHash(wf model.WorkflowID, node model.NodeID) string {
	body, err := a.readVerificationManifestFile(wf, node)
	if err != nil || body == nil {
		return ""
	}
	var m model.EvidenceManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return m.Hash
}

// manifestRange extracts the CommitRange field of a persisted manifest.
func manifestRange(body []byte) string {
	if body == nil {
		return ""
	}
	var m model.EvidenceManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return m.CommitRange
}

// integrationWorktreePath is the deterministic Integration Worktree
// location (PRD 全局目录结构).
func (a *Application) integrationWorktreePath(wf model.WorkflowID) string {
	return filepath.Join(a.home, "worktrees", a.project.Key, string(wf), "integration")
}

// observeWorktree observes one worktree's status with the expected HEAD.
func (a *Application) observeWorktree(ctx context.Context, dir, expectedHead string) (gitflow.StatusFacts, error) {
	if a.git == nil {
		return gitflow.StatusFacts{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	facts, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: dir, ExpectedHead: expectedHead, UntrackedAll: true})
	if err != nil {
		return gitflow.StatusFacts{}, err
	}
	st, ok := facts.(gitflow.StatusFacts)
	if !ok {
		return gitflow.StatusFacts{}, model.InvariantFault(fmt.Errorf("git status observation has an unexpected type"))
	}
	return st, nil
}

// reuseWorktreeDrift reports the drift failure code when the Task
// Worktree of a successor Attempt no longer matches the prior Attempt's
// end evidence ("" when the facts match and the reuse is safe): the
// Worktree HEAD, status, and Dirty Fingerprint must equal the failed or
// interrupted Attempt's end facts before a repair or a resume may reuse
// the exact Branch/Worktree (PRD 已确认：DIRTY_TASK_WORKTREE 原地 Repair;
// 已确认：Ctrl+C 两阶段有限停止 step 7). CFlow never auto-fixes the
// Worktree; a mismatch Blocks with the typed drift code.
func (a *Application) reuseWorktreeDrift(ctx context.Context, wf model.WorkflowID, node model.NodeID) string {
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return string(model.CodeDirtyWorktreeDrifted)
	}
	st := view.State
	att := runningAttemptOfState(st, node)
	if att == nil {
		return ""
	}
	prior := priorAttemptFactsOf(st, node, att.Key.Number)
	if prior == nil || prior.EndHead == "" {
		return "" // the first Attempt: the Worktree is created fresh
	}
	status, err := a.observeWorktree(ctx, a.taskWorktreePath(wf, node), "")
	if err != nil {
		if ctx.Err() != nil {
			return "" // the pass is interrupted: the stop protocol settles the attempt
		}
		return string(model.CodeDirtyWorktreeDrifted)
	}
	if status.Head != prior.EndHead || dirtyFingerprint(status.Dirty) != prior.EndDirtyFingerprint {
		if prior.Status == model.AttemptInterrupted {
			return string(model.CodeInterruptedWorktreeDrifted)
		}
		return string(model.CodeDirtyWorktreeDrifted)
	}
	return ""
}

// driftEndFacts observes the current Task Worktree end facts of one Node
// for the failed Attempt result of a reuse drift ("" on an unreadable
// Worktree).
func (a *Application) driftEndFacts(ctx context.Context, wf model.WorkflowID, node model.NodeID) (head, dirty string) {
	status, err := a.observeWorktree(ctx, a.taskWorktreePath(wf, node), "")
	if err != nil {
		return "", ""
	}
	return status.Head, dirtyFingerprint(status.Dirty)
}

// priorAttemptFactsOf returns the highest-numbered terminal Attempt below
// number with its recorded end facts (nil when none).
func priorAttemptFactsOf(st model.State, node model.NodeID, number model.AttemptNumber) *model.Attempt {
	var best *model.Attempt
	for k, a := range st.Attempts {
		if k.Node != node || k.Number >= number || !a.Status.IsTerminal() {
			continue
		}
		if best == nil || k.Number > best.Key.Number {
			aa := a
			best = aa
		}
	}
	return best
}
