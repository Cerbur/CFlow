package app

// The Commit Policy monitor and post-Safety-Stop settle (Task 17, PRD
// 已确认：Commit Policy 漂移立即安全停止 / 漂移窗口 Commit 的隔离与替代执
// 行): while a commit-capable managed process runs, the monitor re-observes
// the effective fingerprint probe-less no slower than once per second and
// compares it to the approved fingerprint; on drift it fixes the pre-stop
// HEAD of every active Task/Integration Worktree, cancels every active
// chain, and records the safety stop fact — the interrupted Attempts'
// settle then commits the Safety Stop intent atomically (gate close, Run
// STOPPING, stop_reason COMMIT_POLICY_DRIFT) before any process stop.
// After the pass settles, the post-stop scan compares every fixed pre-head
// with the final HEAD: a window Commit receives the immutable Quarantine
// Record with its unique audit Ref (created before the settle, so rows and
// refs commit together); without one, the freshly observed Preflight is
// recorded and the Workflow pauses at the COMMIT_POLICY confirmation gate.
// Same-package split of the Application seam: no public seam added.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

var osMkdirAll = os.MkdirAll
var osWriteFile = os.WriteFile

// policyWorktree is one pre-stop snapshot: the fixed HEAD of an active
// Worktree at the Safety Stop request, with the Branch it carries and the
// Node it belongs to ("" for the Integration Worktree).
type policyWorktree struct {
	Head   string
	Branch string
	Node   model.NodeID
	Err    error
}

// policyInterval is the monitor's recompute period (PRD step 5: no slower
// than once per second while a commit-capable managed process is active).
func (a *Application) policyInterval() time.Duration {
	if a.policyPollInterval > 0 {
		return a.policyPollInterval
	}
	return time.Second
}

// approvedFingerprint is the Commit Policy fingerprint the active
// execution facts bind ("" when no policy gate exists yet).
func (a *Application) approvedFingerprint(ctx context.Context, wf model.WorkflowID) string {
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil || view.State.Workflow.ExecutionFacts == nil {
		return ""
	}
	return view.State.Workflow.ExecutionFacts.Fingerprint
}

// observeFingerprint runs the probe-less Commit Policy observation.
func (a *Application) observeFingerprint(ctx context.Context) (gitflow.FingerprintFacts, error) {
	if a.git == nil {
		return gitflow.FingerprintFacts{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	res, err := a.git.Observe(ctx, gitflow.FingerprintObserve{Revision: "monitor"})
	if err != nil {
		return gitflow.FingerprintFacts{}, err
	}
	facts, ok := res.(gitflow.FingerprintFacts)
	if !ok {
		return gitflow.FingerprintFacts{}, model.InvariantFault(fmt.Errorf("fingerprint observation has an unexpected type"))
	}
	return facts, nil
}

// monitorPolicy is the Commit Policy monitor of one commit-capable chain
// (PRD step 5): every recompute period it re-observes the fingerprint
// probe-less and compares it to the approved fingerprint. A mismatch
// triggers the Policy Safety Stop: the pre-stop Worktree heads are fixed,
// every active chain is cancelled, and the interrupted Attempts' settle
// commits the Safety Stop intent atomically before any process stop.
func (a *Application) monitorPolicy(passCtx context.Context, wf model.WorkflowID) {
	approved := a.approvedFingerprint(context.WithoutCancel(passCtx), wf)
	if approved == "" {
		return // no policy gate exists yet: nothing to monitor
	}
	// The first recompute runs immediately at the Session start, then at
	// most once per recompute period (PRD step 5: no slower than once per
	// second — checking more often never violates the bound and shrinks
	// the drift window).
	if a.policyMismatch(passCtx, approved) {
		a.triggerPolicyStop(passCtx, wf)
		return
	}
	interval := a.policyInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-passCtx.Done():
			return
		case <-ticker.C:
			if a.policyMismatch(passCtx, approved) {
				a.triggerPolicyStop(passCtx, wf)
				return
			}
		}
	}
}

// policyMismatch reports whether the probe-less observation of the
// effective fingerprint differs from the approved fingerprint.
func (a *Application) policyMismatch(passCtx context.Context, approved string) bool {
	facts, err := a.observeFingerprint(context.WithoutCancel(passCtx))
	if err != nil {
		return false // unreadable: keep monitoring; the post-commit gate still enforces
	}
	return facts.PolicyFingerprint != approved
}

// triggerPolicyStop fixes the pre-stop facts and cancels every active
// chain: from this moment no new external action may start (PRD step 1 —
// the gate closes atomically in the first interrupted Attempt's settle).
func (a *Application) triggerPolicyStop(passCtx context.Context, wf model.WorkflowID) {
	settleCtx := context.WithoutCancel(passCtx)
	a.policyMu.Lock()
	if a.policyDrift || a.policySettling {
		// A settle is in progress (or already armed): a stale monitor poll
		// must never re-arm the drift snapshot with post-stop pre-heads.
		a.policyMu.Unlock()
		return
	}
	a.policyDrift = true
	a.policyCode = model.CodeCommitPolicySafetyStopRequested
	a.policyPreHeads = a.activeWorktreeHeads(settleCtx, wf)
	passCancel := a.passCancel
	a.policyMu.Unlock()
	if passCancel != nil {
		passCancel()
	}
}

// policyDriftCode is the failure code the interrupted Attempts of a
// stopped pass settle with: the Safety Stop code when a drift triggered
// the stop, the user interruption code otherwise.
func (a *Application) policyDriftCode() model.Code {
	a.policyMu.Lock()
	defer a.policyMu.Unlock()
	if a.policyDrift {
		return a.policyCode
	}
	return model.CodeUserInterrupted
}

// policyDriftPending reports whether a Safety Stop was triggered in this
// Application session.
func (a *Application) policyDriftPending() bool {
	a.policyMu.Lock()
	defer a.policyMu.Unlock()
	return a.policyDrift
}

// takePolicyDriftSnapshot claims and clears the drift facts of the
// settled pass (one settle per pass) and marks the settle in progress so
// a racing monitor poll can never re-arm the snapshot mid-settle.
func (a *Application) takePolicyDriftSnapshot() (map[string]policyWorktree, bool) {
	a.policyMu.Lock()
	defer a.policyMu.Unlock()
	if !a.policyDrift {
		return nil, false
	}
	heads := a.policyPreHeads
	a.policyDrift = false
	a.policyCode = ""
	a.policyPreHeads = nil
	a.policySettling = true
	return heads, true
}

// activeWorktreeHeads fixes the pre-stop HEAD of every active Worktree at
// the Safety Stop request: the Task Worktrees of every RUNNING agent-task
// Node (the Worktree registry path is deterministic) and the Integration
// Worktree.
func (a *Application) activeWorktreeHeads(ctx context.Context, wf model.WorkflowID) map[string]policyWorktree {
	out := map[string]policyWorktree{}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		out["unresolved-workflow:"+string(wf)] = policyWorktree{Err: err}
		return out
	}
	st := view.State
	for _, n := range st.Nodes {
		if n.Kind != model.NodeAgentTask || n.Status != model.NodeRunning {
			continue
		}
		path, err := a.taskWorktreePath(ctx, wf, n.ID)
		if err != nil {
			out["unresolved-task:"+string(n.ID)] = policyWorktree{Branch: n.Branch, Node: n.ID, Err: err}
			continue
		}
		status, err := a.observeWorktree(ctx, path, "")
		if err != nil {
			out[path] = policyWorktree{Branch: n.Branch, Node: n.ID, Err: err}
			continue
		}
		if status.Head == "" {
			out[path] = policyWorktree{Branch: n.Branch, Node: n.ID,
				Err: model.InvariantFault(fmt.Errorf("active task worktree %s has no observable HEAD", n.ID))}
			continue
		}
		out[path] = policyWorktree{Head: status.Head, Branch: n.Branch, Node: n.ID}
	}
	// The layout-aware delivery worktree: Workspace on Layout 2, Integration
	// on Layout 1. A layout failure is retained as blocking snapshot evidence.
	deliveryBranch, _, path, pathErr := a.deliveryFacts(ctx, wf, st)
	if pathErr != nil {
		out["unresolved-delivery:"+string(wf)] = policyWorktree{Err: pathErr}
		return out
	}
	if status, err := a.observeWorktree(ctx, path, ""); err != nil {
		out[path] = policyWorktree{Branch: deliveryBranch, Err: err}
	} else if status.Head == "" {
		out[path] = policyWorktree{Branch: deliveryBranch,
			Err: model.InvariantFault(fmt.Errorf("delivery worktree has no observable HEAD"))}
	} else {
		out[path] = policyWorktree{Head: status.Head, Branch: deliveryBranch}
	}
	return out
}

// settlePolicyDrift runs the post-stop scan and settle: every fixed
// pre-head is compared with the final HEAD and the window Commits are
// scanned; a window Commit receives the immutable Quarantine Record with
// its unique audit Ref (created before the settle, so the rows and the
// refs commit together), and the Workflow Blocks. Without one, the fresh
// Commit Preflight is observed and the Workflow pauses at the
// COMMIT_POLICY_CONFIRMATION_REQUIRED gate (PRD steps 6-7).
func (a *Application) settlePolicyDrift(ctx context.Context, st *store.Store, wf model.WorkflowID) error {
	heads, ok := a.takePolicyDriftSnapshot()
	if !ok {
		return nil
	}
	defer a.finishPolicySettle()
	settleCtx := context.WithoutCancel(ctx)
	var window []model.WindowCommit
	for path, pre := range heads {
		if pre.Err != nil {
			return pre.Err
		}
		final, err := a.observeWorktree(settleCtx, path, "")
		if err != nil {
			return err
		}
		if final.Head == "" {
			return model.InvariantFault(fmt.Errorf("safety-stop worktree %s has no observable HEAD", path))
		}
		if final.Head == pre.Head {
			continue
		}
		// Scan the drift window (pre, final]: commits the stop request
		// could not observe atomically.
		hasCommits, err := a.windowHasCommits(settleCtx, pre.Head, final.Head)
		if err != nil {
			return err
		}
		if !hasCommits {
			continue
		}
		window = append(window, model.WindowCommit{
			Branch:   pre.Branch,
			FromHead: pre.Head,
			ToHead:   final.Head,
			Node:     pre.Node,
		})
	}
	if len(window) > 0 {
		return a.settleDriftWindowQuarantine(settleCtx, st, wf, window)
	}
	return a.settleDriftConfirmation(settleCtx, st, wf)
}

// windowHasCommits reports whether the half-open commit range (from, to]
// is non-empty.
func (a *Application) windowHasCommits(ctx context.Context, from, to string) (bool, error) {
	if a.git == nil {
		return false, model.InvariantFault(fmt.Errorf("git seam is not configured for policy-window inspection"))
	}
	facts, err := a.git.Observe(ctx, gitflow.HistoryRange{From: from, To: to})
	if err != nil {
		return false, err
	}
	rf, ok := facts.(gitflow.RangeFacts)
	if !ok {
		return false, model.InvariantFault(fmt.Errorf("policy-window history observation has an unexpected type"))
	}
	return len(rf.Commits) > 0, nil
}

// settleDriftWindowQuarantine creates the unique audit Refs before the
// settle (the Kernel records the same deterministic names) and feeds the
// quarantine settle decision.
func (a *Application) settleDriftWindowQuarantine(ctx context.Context, st *store.Store, wf model.WorkflowID, window []model.WindowCommit) error {
	// The quarantine rows commit FIRST: a crash between the row and its
	// audit Ref leaves a row without a Ref, which the Recovery Engine
	// flags as drift (a Ref without a row would be invisible to
	// Recovery, and the window Commit could re-enter the trusted chain
	// after the stop converged).
	if _, err := a.runDecisionLoop(ctx, st, wf, DispatchCommand{Workflow: wf},
		model.PolicyDriftSettleInput{WindowCommits: window}, false); err != nil {
		return err
	}
	// The unique audit Refs are created from the committed rows:
	// refs/cflow/<workflow>/quarantine/<quarantine-id> pinning the
	// discovered head. Expected-absent compare-and-swap: an existing ref
	// (a crashed retry) is the evidence, never overwritten.
	if a.git != nil {
		view, err := st.View(ctx, store.StoreQuery{})
		if err != nil {
			return err
		}
		for _, q := range view.State.Quarantines {
			if q.AuditRef == "" || q.ToHead == "" {
				continue
			}
			if _, err := a.git.Execute(ctx, gitflow.CreateAuditRef{Ref: q.AuditRef, Head: q.ToHead}); err != nil {
				// The rows are committed; the missing Ref is drift the
				// Recovery Engine flags. Fail closed so the caller sees it.
				return err
			}
		}
	}
	return nil
}

// settleDriftConfirmation observes the fresh Commit Preflight and feeds

// settleDriftConfirmation observes the fresh Commit Preflight and feeds
// the confirmation-gate settle decision (PRD 已确认：执行期间 Commit Policy
// 漂移确认 step 1: the new Preflight Revision is generated and fully
// validated only after the Safety Stop; a failed Preflight blocks with
// GIT_IDENTITY_NOT_CONFIGURED or GIT_SIGNING_PREFLIGHT_FAILED and no
// confirmation is offered).
func (a *Application) settleDriftConfirmation(ctx context.Context, st *store.Store, wf model.WorkflowID) error {
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return err
	}
	facts := view.State.Workflow.ExecutionFacts
	if facts == nil {
		return model.InvalidInputFault("no execution facts to confirm against")
	}
	next := facts.PreflightRevision + 1
	preflight, err := a.observePreflight(ctx, wf, next)
	if err != nil {
		return err
	}
	_, err = a.runDecisionLoop(ctx, st, wf, DispatchCommand{Workflow: wf},
		model.PolicyDriftSettleInput{Preflight: &preflight}, false)
	return err
}

// settleDriftConfirmation observes the fresh Commit Preflight and feeds

// reconciliationManifestPath is the deterministic persisted Reconciliation
// Manifest location of one workflow revision (the Recovery Engine reads
// the same path to recompute and compare).
func (a *Application) reconciliationManifestPath(ctx context.Context, wf model.WorkflowID, revision int) (string, error) {
	root, err := a.workflowEvidenceDir(ctx, wf)
	if err != nil {
		return "", err
	}
	if root == "" {
		return "", model.InvalidInputFault("no evidence root is configured for the reconciliation manifest")
	}
	return filepath.Join(root, "reconciliation", fmt.Sprintf("manifest-%d.json", revision)), nil
}

// writeReconciliationManifest persists the immutable manifest body and
// returns its self-hash (the Replacement Execution Approval binds the
// Revision and Hash; Recovery recomputes the classification and compares).
func (a *Application) writeReconciliationManifest(ctx context.Context, wf model.WorkflowID, revision int, body []byte) (string, error) {
	path, err := a.reconciliationManifestPath(ctx, wf, revision)
	if err != nil {
		return "", err
	}
	if err := mkdirAll0700(filepath.Dir(path)); err != nil {
		return "", err
	}
	if err := writeFile0600(path, body); err != nil {
		return "", err
	}
	return sha256Hex(body), nil
}

// manifestBody renders the canonical manifest JSON (map keys marshal
// deterministically).
func manifestBody(manifest model.ReconciliationManifest) ([]byte, error) {
	return json.Marshal(manifest)
}

// mkdirAll0700 creates one directory chain with owner-only mode.
func mkdirAll0700(path string) error {
	return osMkdirAll(path, 0o700)
}

// writeFile0600 writes one evidence file owner-only.
func writeFile0600(path string, data []byte) error {
	return osWriteFile(path, data, 0o600)
}

// finishPolicySettle clears the settle-in-progress marker.
func (a *Application) finishPolicySettle() {
	a.policyMu.Lock()
	defer a.policyMu.Unlock()
	a.policySettling = false
}
