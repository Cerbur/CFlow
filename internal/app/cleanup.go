package app

// The safe cleanup protocol (Task 20, PRD 已确认：Cleanup 仅删除安全干净的
// 衍生目录, design 17.4): the Dry Run command builds the immutable Manifest
// over the exact managed target set (the Worktrees registered in SQLite
// for the Workflow, cross-referenced with the Git Worktree Registry, plus
// explicit exact scratch paths), and the Execute command is the second
// explicit confirmation that binds the exact Manifest ID/hash, re-observes
// every item's facts for the Kernel's strict revalidation, and then removes
// each target only through the exact non-force typed Git operation after
// recollecting every fact per item. Branches, refs, commits, SQLite state,
// Events, Approvals, Artifacts, Logs, Sessions, and evidence are never
// touched; a partial result stops subsequent items and Blocks the Attempt
// for recovery to reconcile by exact path + Git Worktree Registry +
// Intent/Result. Same-package split of the Application seam: no public
// seam added.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
	"cflow.local/cflow/internal/store"
)

// ---------------------------------------------------------------------------
// Dry Run command builder: the immutable Manifest over the exact targets
// ---------------------------------------------------------------------------

// prepareCleanupDryRun builds the CleanupDryRun input: the app collects
// the managed Worktrees registered in SQLite for the Workflow (the
// Integration Worktree, every Task Worktree, and every Apply Worktree,
// cross-referenced with the Git Worktree Registry — a manually removed
// Worktree is never pretended present) and appends the explicitly
// provided exact scratch paths, validating each with the security guard.
// The Planning Snapshot is never a Cleanup target (its workflow.yaml
// manifest and planning lineage are preserved). The Kernel records the
// immutable Manifest and its hash from these facts.
func (a *Application) prepareCleanupDryRun(ctx context.Context, cmd DryRunCommand) (model.Input, model.WorkflowID, error) {
	resolved, err := a.resolveMutationWorkflow(cmd.Workflow)
	if err != nil {
		return nil, "", err
	}
	view, err := a.readAggregate(ctx, resolved, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	st := view.State
	if st.Workflow.ID == "" {
		return nil, "", model.InvalidInputFault("no such workflow: " + string(resolved))
	}
	if !st.Workflow.Runtime.IsTerminal() {
		return nil, "", model.NewFault(model.CodeCleanupWorkflowNotTerminal,
			"cleanup requires a terminal workflow")
	}
	items, err := a.collectCleanupWorktreeItems(ctx, resolved, st)
	if err != nil {
		return nil, "", err
	}
	for _, it := range cmd.Items {
		if it.Kind != model.CleanupScratch {
			return nil, "", model.InvalidInputFault("cleanup dry run accepts only explicit scratch targets")
		}
		if err := a.validateCleanupScratch(it.CanonicalPath); err != nil {
			return nil, "", err
		}
		it.Index = 0 // the Kernel assigns the identity; the app fixes the order
		items = append(items, it)
	}
	for i := range items {
		items[i].Index = i
	}
	return model.CleanupCommandInput{Kind: model.CleanupDryRun, Items: items}, resolved, nil
}

// collectCleanupWorktreeItems collects the managed Worktrees registered in
// SQLite for the Workflow, cross-referenced with the Git Worktree
// Registry: the Integration Worktree, every Task Worktree, and every
// Apply Worktree. The dry-run records each candidate's ordinary Dirty
// Fingerprint (ignored files never count toward it, the PRD Commit/Clean
// gate form) and ordinary cleanliness; the execution's re-observation is
// strictly safer (it classifies ignored files and the in-progress/lock
// state), so ignored-only content surfaces as CLEANUP_FACT_MISMATCH.
func (a *Application) collectCleanupWorktreeItems(ctx context.Context, wf model.WorkflowID, st model.State) ([]model.CleanupItem, error) {
	if a.git == nil {
		return nil, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	managed := map[string]string{} // canonical path -> expected branch
	if st.Workflow.IntegrationBranch != "" {
		managed[a.integrationWorktreePath(wf)] = st.Workflow.IntegrationBranch
	}
	for _, n := range st.Nodes {
		if n == nil || n.Kind != model.NodeAgentTask {
			continue
		}
		managed[a.taskWorktreePath(wf, n.ID)] = "cflow/" + string(wf) + "/task-" + string(n.ID)
	}
	for _, att := range st.ApplyAttempts {
		if att.Number < 1 {
			continue
		}
		managed[a.applyWorktreePath(wf, att.Number)] = a.applyBranchName(wf, att.Number)
	}
	registry, err := a.observeWorktreeRegistry(ctx)
	if err != nil {
		return nil, err
	}
	var items []model.CleanupItem
	for _, entry := range registry {
		branch, ok := managed[entry.Path]
		if !ok {
			continue // not a managed Worktree of this Workflow
		}
		status, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: entry.Path, UntrackedAll: true})
		if err != nil {
			return nil, err
		}
		stt, ok := status.(gitflow.StatusFacts)
		if !ok {
			return nil, model.InvariantFault(fmt.Errorf("git status observation has an unexpected type"))
		}
		items = append(items, model.CleanupItem{
			Kind:          model.CleanupWorktree,
			CanonicalPath: entry.Path,
			Branch:        branch,
			ExpectedHead:  stt.Head,
			Fingerprint:   dirtyFingerprint(stt.Dirty),
			Dirty:         !stt.Clean(),
			Status:        model.CleanupItemPending,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CanonicalPath < items[j].CanonicalPath })
	return items, nil
}

// ---------------------------------------------------------------------------
// Execute command builder: the second explicit confirmation
// ---------------------------------------------------------------------------

// prepareCleanupExecute builds the CleanupExecute input: the app re-reads
// the confirmed Manifest (the user's second confirmation binds the exact
// Manifest ID and hash) and re-observes every item's facts for the
// Kernel's strict revalidation. The first execution revalidates every
// item; the crash-recovery re-run of a RUNNING attempt re-requests the
// first unsettled item and the executor observes the actual state.
func (a *Application) prepareCleanupExecute(ctx context.Context, cmd ExecuteCleanupCommand) (model.Input, model.WorkflowID, error) {
	resolved, err := a.resolveMutationWorkflow(cmd.Workflow)
	if err != nil {
		return nil, "", err
	}
	view, err := a.readAggregate(ctx, resolved, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	st := view.State
	if st.Workflow.ID == "" {
		return nil, "", model.InvalidInputFault("no such workflow: " + string(resolved))
	}
	att := lastCleanupAttemptOf(st)
	if att == nil {
		return nil, "", model.InvalidInputFault("no cleanup manifest to execute")
	}
	switch att.Status {
	case model.CleanupStatusAwaitingConfirmation, model.CleanupStatusRunning:
	default:
		return nil, "", model.InvalidInputFault("cleanup attempt " + string(att.Status) + " cannot be executed")
	}
	if cmd.Manifest != att.Manifest {
		return nil, "", model.NewFault(model.CodeCleanupFactsChanged,
			"cleanup manifest identity or hash changed since the dry run")
	}
	items, err := a.reobserveCleanupItems(ctx, resolved, att, att.Status == model.CleanupStatusRunning)
	if err != nil {
		return nil, "", err
	}
	return model.CleanupCommandInput{Kind: model.CleanupExecute, Manifest: cmd.Manifest, Items: items}, resolved, nil
}

// reobserveCleanupItems re-observes every manifest item's facts for the
// Kernel's revalidation. On the crash-recovery re-run (running), an item
// whose exact target is already absent keeps the manifest facts — the
// Kernel's Running path re-requests the unsettled item and the executor
// observes the actual state. On the first execution, an absent target is
// a fact mismatch the Kernel must reject.
func (a *Application) reobserveCleanupItems(ctx context.Context, wf model.WorkflowID, att *model.CleanupAttempt, running bool) ([]model.CleanupItem, error) {
	_ = wf
	items := make([]model.CleanupItem, 0, len(att.Items))
	for _, mi := range att.Items {
		switch mi.Kind {
		case model.CleanupWorktree:
			observed, err := a.reobserveWorktreeItem(ctx, mi, running)
			if err != nil {
				return nil, err
			}
			items = append(items, observed)
		case model.CleanupScratch:
			items = append(items, reobserveScratchItem(mi, running))
		}
	}
	return items, nil
}

// reobserveWorktreeItem re-observes one managed Worktree item: the exact
// registry entry (branch), the HEAD, the cleanup fingerprint (ignored
// files classify, the strictly safer gate), and the safe-clean Dirty
// flag (content, an in-progress Git operation, or a lock file).
func (a *Application) reobserveWorktreeItem(ctx context.Context, mi model.CleanupItem, running bool) (model.CleanupItem, error) {
	registry, err := a.observeWorktreeRegistry(ctx)
	if err != nil {
		return model.CleanupItem{}, err
	}
	var entry *gitflow.WorktreeEntry
	for i := range registry {
		if registry[i].Path == mi.CanonicalPath {
			entry = &registry[i]
			break
		}
	}
	if entry == nil {
		if running {
			return mi, nil // the in-flight item; the executor observes the actual state
		}
		bad := mi
		bad.ExpectedHead = ""
		bad.Fingerprint = "target-absent"
		return bad, nil
	}
	status, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: mi.CanonicalPath, UntrackedAll: true, Ignored: true})
	if err != nil {
		return model.CleanupItem{}, err
	}
	st, ok := status.(gitflow.StatusFacts)
	if !ok {
		return model.CleanupItem{}, model.InvariantFault(fmt.Errorf("git status observation has an unexpected type"))
	}
	ipFacts, err := a.git.Observe(ctx, gitflow.WorktreeInProgress{Path: mi.CanonicalPath})
	if err != nil {
		return model.CleanupItem{}, err
	}
	ip, ok := ipFacts.(gitflow.WorktreeInProgressFacts)
	if !ok {
		return model.CleanupItem{}, model.InvariantFault(fmt.Errorf("worktree state observation has an unexpected type"))
	}
	return model.CleanupItem{
		Index:         mi.Index,
		Kind:          mi.Kind,
		CanonicalPath: mi.CanonicalPath,
		Branch:        entry.Branch,
		ExpectedHead:  st.Head,
		Fingerprint:   cleanupFingerprint(st),
		Dirty:         cleanupDirty(st, ip),
		Status:        mi.Status,
	}, nil
}

// reobserveScratchItem re-observes one exact scratch item: the target
// must still exist for the first execution; on the crash-recovery re-run
// an absent exact target keeps the manifest facts (the executor settles
// from the actual state).
func reobserveScratchItem(mi model.CleanupItem, running bool) model.CleanupItem {
	if _, err := os.Lstat(mi.CanonicalPath); os.IsNotExist(err) {
		if running {
			return mi
		}
		bad := mi
		bad.Fingerprint = "target-absent"
		return bad
	}
	return mi
}

// ---------------------------------------------------------------------------
// the safe-clean facts of one target
// ---------------------------------------------------------------------------

// cleanupFingerprint is the strictly-safer fingerprint of the cleanup
// re-observation: the ordinary Dirty Fingerprint extended with the
// ignored-file set. A Worktree with no ignored content yields the ordinary
// fingerprint (the Manifest comparison is exact); ignored content changes
// the fingerprint, so a target that the Task gate would call clean but the
// cleanup gate must refuse surfaces as CLEANUP_FACT_MISMATCH.
func cleanupFingerprint(st gitflow.StatusFacts) string {
	ordinary := dirtyFingerprint(st.Dirty)
	if len(st.Ignored) == 0 {
		return ordinary
	}
	var ignored []string
	for _, e := range st.Ignored {
		ignored = append(ignored, e.Path)
	}
	sort.Strings(ignored)
	h := sha256.New()
	h.Write([]byte(ordinary))
	for _, p := range ignored {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// cleanupDirty is the strictly-safer cleanliness of the cleanup gate: any
// staged, unstaged, untracked, or ignored content, an in-progress Git
// operation (merge/rebase/cherry-pick/revert/bisect), or an
// administrative lock file. Git would refuse to remove any of these; the
// gate refuses before any deletion is attempted (PRD: Git refuses removal
// → CFlow refuses, never force).
func cleanupDirty(st gitflow.StatusFacts, ip gitflow.WorktreeInProgressFacts) bool {
	if len(st.Staged) > 0 || len(st.Unstaged) > 0 || len(st.Untracked) > 0 || len(st.Ignored) > 0 {
		return true
	}
	return ip.InProgress || ip.Locked
}

// validateCleanupScratch validates one exact scratch deletion target
// through the security guard: absolute and canonical (no symlink
// component or final symlink), owned by the effective user, never the
// filesystem root, `~`, an unresolved-variable path, the Workspace or
// CFLOW_HOME root, or a broad ancestor of either, and resolving inside
// CFLOW_HOME.
func (a *Application) validateCleanupScratch(path string) error {
	facts, err := security.CheckCleanupScratch(security.CleanupScratchRequest{
		Path:          path,
		HomeRoot:      a.home,
		WorkspaceRoot: a.project.Root,
	})
	if err != nil {
		return err
	}
	if !facts.InsideRoot {
		return model.InvalidInputFault("scratch target must resolve inside CFLOW_HOME")
	}
	return nil
}

// validateCleanupWorktreeTarget re-validates one managed Worktree target
// immediately before its removal: the exact canonical path must be under
// the workflow's managed root — the aggregated <home>/projects/<key>/
// <workflow-id>/ on Layout Version 2 (design 8.5, TUI task 7), the legacy
// <home>/worktrees/<key>/<workflow-id>/ on Layout 1 — and owned by the
// effective user. A mismatch is CLEANUP_FACT_MISMATCH and deletes
// nothing.
func (a *Application) validateCleanupWorktreeTarget(wf model.WorkflowID, path string) error {
	root := filepath.Join(a.home, "worktrees", a.project.Key, string(wf))
	if a.workflowLayout(context.Background(), wf) >= 2 {
		root = a.layout.WorkflowRoot(wf)
	}
	if !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return model.NewFault(model.CodeCleanupFactsChanged,
			"worktree target escapes the recorded managed root")
	}
	owned, err := security.OwnerIsEffectiveUser(path)
	if err != nil {
		return model.NewFault(model.CodeCleanupFactsChanged,
			"worktree target cannot be inspected")
	}
	if !owned {
		return model.NewFault(model.CodeCleanupFactsChanged,
			"worktree target is not owned by the effective user")
	}
	return nil
}

// ---------------------------------------------------------------------------
// executors
// ---------------------------------------------------------------------------

// cleanupWorktreeRemove removes one confirmed Worktree item (design 17.4):
// the exact target is re-validated (containment, owner), every fact is
// recollected from the Git Worktree Registry and the safe-clean status,
// and the removal runs only through the exact non-force `git worktree
// remove` operation. A drifted fact is CLEANUP_FACT_MISMATCH, a
// non-safe-clean target is CLEANUP_TARGET_DIRTY, and a refused or failed
// removal command is CLEANUP_ITEM_FAILED; all are typed item results the
// Kernel settles (the Attempt Blocks with partial results explicit). A
// target already absent is the crash-recovery outcome: the removal
// already happened and is reported Removed, never pretended undone.
func (a *Application) cleanupWorktreeRemove(ctx context.Context, wf model.WorkflowID, intent model.CleanupWorktreeRemoveIntent) (model.EffectResultInput, error) {
	fail := func(code model.Code, reason string) model.EffectResultInput {
		return model.EffectResultInput{Kind: model.CleanupItemFailedResult,
			CleanupAttempt: intent.Cleanup, ItemIndex: intent.Item, FailureCode: code, Reason: reason}
	}
	if a.git == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	item, ok := cleanupManifestItem(view.State, intent.Cleanup, intent.Item)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("cleanup attempt or item is missing"))
	}
	// Recollect every fact per item before the deletion. An exact target
	// absent from the Git Worktree Registry is the crash-recovery outcome:
	// the removal already happened and is reported Removed, never pretended
	// present.
	registry, err := a.observeWorktreeRegistry(ctx)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	var entry *gitflow.WorktreeEntry
	for i := range registry {
		if registry[i].Path == item.CanonicalPath {
			entry = &registry[i]
			break
		}
	}
	if entry == nil {
		return model.EffectResultInput{Kind: model.CleanupItemRemovedResult,
			CleanupAttempt: intent.Cleanup, ItemIndex: intent.Item}, nil
	}
	if err := a.validateCleanupWorktreeTarget(wf, item.CanonicalPath); err != nil {
		code, _ := model.CodeOf(err)
		if code == "" {
			code = model.CodeCleanupFactsChanged
		}
		return fail(code, err.Error()), nil
	}
	status, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: item.CanonicalPath, UntrackedAll: true, Ignored: true})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	st, ok := status.(gitflow.StatusFacts)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("git status observation has an unexpected type"))
	}
	ipFacts, err := a.git.Observe(ctx, gitflow.WorktreeInProgress{Path: item.CanonicalPath})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	ip, ok := ipFacts.(gitflow.WorktreeInProgressFacts)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("worktree state observation has an unexpected type"))
	}
	if entry.Branch != item.Branch || entry.Detached || st.Head != item.ExpectedHead {
		return fail(model.CodeCleanupFactsChanged,
			"worktree facts drifted from the confirmed manifest"), nil
	}
	if cleanupFingerprint(st) != item.Fingerprint {
		return fail(model.CodeCleanupFactsChanged,
			"worktree content drifted from the confirmed manifest"), nil
	}
	if cleanupDirty(st, ip) {
		return fail(model.CodeCleanupTargetDirty,
			"worktree is not safe-clean (content, an in-progress git operation, or a lock)"), nil
	}
	// The exact non-force removal. Git refusing (a race dirtied or locked
	// the target) is refused, never escalated to a force.
	res, err := a.git.Execute(ctx, gitflow.RemoveWorktree{Path: item.CanonicalPath})
	if err != nil {
		if code, ok := model.CodeOf(err); ok &&
			(code == model.CodeCleanupTargetDirty || code == model.CodeCleanupFactsChanged) {
			return fail(code, err.Error()), nil
		}
		return fail(model.CodeCleanupItemFailed, "worktree removal command failed"), nil
	}
	wr, ok := res.(gitflow.WorktreeRemovedResult)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("worktree removal has an unexpected result"))
	}
	// The post-removal verification. A crash here leaves the item REQUESTED
	// with the removal already done; recovery settles from the actual state.
	if _, err := a.observeWorktreeRegistry(ctx); err != nil {
		return model.EffectResultInput{}, err
	}
	if a.worktreeRegistered(ctx, wr.Path) {
		return fail(model.CodeCleanupFactsChanged, "worktree is still registered after the removal"), nil
	}
	if _, err := os.Lstat(wr.Path); err == nil {
		return fail(model.CodeCleanupFactsChanged, "worktree directory is still present after the removal"), nil
	}
	return model.EffectResultInput{Kind: model.CleanupItemRemovedResult,
		CleanupAttempt: intent.Cleanup, ItemIndex: intent.Item}, nil
}

// cleanupScratchRemove removes one confirmed exact scratch item (design
// 17.4): the security guard re-validates the exact path (a symlink, a
// managed root, or a broad ancestor is never removed; a dir-internal
// symlink is never followed — os.RemoveAll removes the symlink itself),
// and the removal is verified after. An absent exact target is the
// crash-recovery outcome and is reported Removed.
func (a *Application) cleanupScratchRemove(ctx context.Context, wf model.WorkflowID, intent model.CleanupScratchRemoveIntent) (model.EffectResultInput, error) {
	fail := func(code model.Code, reason string) model.EffectResultInput {
		return model.EffectResultInput{Kind: model.CleanupItemFailedResult,
			CleanupAttempt: intent.Cleanup, ItemIndex: intent.Item, FailureCode: code, Reason: reason}
	}
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	item, ok := cleanupManifestItem(view.State, intent.Cleanup, intent.Item)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("cleanup attempt or item is missing"))
	}
	// An absent exact target is the crash-recovery outcome (already
	// removed) and is reported Removed, never pretended present.
	if _, err := os.Lstat(item.CanonicalPath); os.IsNotExist(err) {
		return model.EffectResultInput{Kind: model.CleanupItemRemovedResult,
			CleanupAttempt: intent.Cleanup, ItemIndex: intent.Item}, nil
	} else if err != nil {
		return fail(model.CodeCleanupItemFailed, "scratch target cannot be inspected"), nil
	}
	if err := a.validateCleanupScratch(item.CanonicalPath); err != nil {
		code, ok := model.CodeOf(err)
		if !ok {
			code = model.CodeCleanupItemFailed
		}
		return fail(code, err.Error()), nil
	}
	if err := os.RemoveAll(item.CanonicalPath); err != nil {
		return fail(model.CodeCleanupItemFailed, "scratch removal failed"), nil
	}
	if _, err := os.Lstat(item.CanonicalPath); err == nil {
		return fail(model.CodeCleanupItemFailed, "scratch target is still present after the removal"), nil
	}
	return model.EffectResultInput{Kind: model.CleanupItemRemovedResult,
		CleanupAttempt: intent.Cleanup, ItemIndex: intent.Item}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// observeWorktreeRegistry observes the Git Worktree Registry entries.
func (a *Application) observeWorktreeRegistry(ctx context.Context) ([]gitflow.WorktreeEntry, error) {
	facts, err := a.git.Observe(ctx, gitflow.WorktreeList{})
	if err != nil {
		return nil, err
	}
	registry, ok := facts.(gitflow.WorktreeFacts)
	if !ok {
		return nil, model.InvariantFault(fmt.Errorf("worktree list observation has an unexpected type"))
	}
	return registry.Entries, nil
}

// worktreeRegistered reports whether one exact path is in the Git
// Worktree Registry.
func (a *Application) worktreeRegistered(ctx context.Context, path string) bool {
	entries, err := a.observeWorktreeRegistry(ctx)
	if err != nil {
		return true // fail closed: an unreadable registry never proves absence
	}
	for _, e := range entries {
		if e.Path == path {
			return true
		}
	}
	return false
}

// cleanupManifestItem returns one manifest item of one Cleanup Attempt.
func cleanupManifestItem(st model.State, att model.CleanupAttemptID, index int) (model.CleanupItem, bool) {
	for i := range st.CleanupAttempts {
		if st.CleanupAttempts[i].ID != att {
			continue
		}
		if index < 0 || index >= len(st.CleanupAttempts[i].Items) {
			return model.CleanupItem{}, false
		}
		return st.CleanupAttempts[i].Items[index], true
	}
	return model.CleanupItem{}, false
}

func lastCleanupAttemptOf(st model.State) *model.CleanupAttempt {
	if len(st.CleanupAttempts) == 0 {
		return nil
	}
	return &st.CleanupAttempts[len(st.CleanupAttempts)-1]
}

func findCleanupAttemptOf(st model.State, id model.CleanupAttemptID) *model.CleanupAttempt {
	for i := range st.CleanupAttempts {
		if st.CleanupAttempts[i].ID == id {
			return &st.CleanupAttempts[i]
		}
	}
	return nil
}
