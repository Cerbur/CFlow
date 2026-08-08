// The Recovery Engine (design 17): Reconcile runs before every mutation
// and after abnormal exit, evaluating facts in the design's order —
// schema/Artifact compatibility, lock facts, managed process identity,
// aggregate invariants and unfinished intents, Artifact facts, Git refs/
// ancestry/Worktree registry/HEAD/status/audit refs, verification and
// review evidence, unfinished Effect Intents, approval/binding facts,
// and Scheduler readiness — and for every unfinished Effect Intent of
// the persisted ledger produces exactly one disposition:
//
//	already_completed: external facts uniquely prove the intended result;
//	safe_to_retry:      the intended effect is absent and all expected
//	                   facts still match;
//	blocked_drift:      external facts changed or cannot be uniquely
//	                   explained;
//	fatal_invariant:    authoritative evidence is missing or
//	                   contradictory beyond safe repair.
//
// The dispositions use expected-absent / expected-value compare-and-swap
// semantics that prevent duplicate Worktrees, refs, merges, or Apply
// updates. Reconcile never mutates anything: it returns typed facts and
// faults, and the Application decides (design 17.1: "It does not write
// tables directly or invoke an Agent to decide which fact to trust").
//
// This is the basic Recovery of Task 13: the full matrix (locks, managed
// process identity, Cancel/Cleanup recovery, re-execution of safe
// intents) arrives with Task 17; the fact order and the four
// dispositions are the stable contract.
package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
	"cflow.local/cflow/internal/store"
)

// Scope is the reconciliation scope. Workflow fixes the aggregate whose
// facts are reconciled; an empty Workflow reconciles project-level facts
// only (schema and Artifact compatibility).
type Scope struct {
	Workflow model.WorkflowID
}

// Disposition is the closed set of per-Intent reconciliation outcomes
// (design 17.2).
type Disposition string

const (
	AlreadyCompleted Disposition = "already_completed"
	SafeToRetry      Disposition = "safe_to_retry"
	BlockedDrift     Disposition = "blocked_drift"
	FatalInvariant   Disposition = "fatal_invariant"
)

// IntentDisposition is one unfinished Effect Intent's reconciliation
// outcome with its persisted ledger id and the reason.
type IntentDisposition struct {
	ID          string
	Intent      model.EffectIntent
	Disposition Disposition
	Reason      string
}

// ReconciliationOutcome is the typed result of one Reconcile: the
// per-Intent dispositions plus the faults the Application must act on
// (blocked_drift demands user action; fatal_invariant demands quarantine
// of the mutation path).
type ReconciliationOutcome struct {
	Dispositions []IntentDisposition
	Faults       []model.Fault
}

// RecoveryEngine is the stable Recovery seam (design 17.1). Its fact
// collectors are private; callers only Reconcile.
type RecoveryEngine struct {
	sup           process.Supervisor
	git           *gitflow.GitFlow
	home          string
	projectKey    string
	evidenceDir   string
	openView      func(ctx context.Context, wf model.WorkflowID) (store.StoreView, error)
	openArtifacts func(ctx context.Context, wf model.WorkflowID) (*artifact.Store, error)
}

// EngineOptions wires the Engine's private fact collectors: the GitFlow
// seam (Git refs, ancestry, Worktree registry, HEAD, status, audit refs),
// the managed directories (worktrees, Artifact Store, evidence), the
// Store view opener, and the Artifact Store opener.
type RecoveryEngineOptions struct {
	Supervisor    process.Supervisor
	GitFlow       *gitflow.GitFlow
	Home          string
	ProjectKey    string
	EvidenceDir   string
	OpenView      func(context.Context, model.WorkflowID) (store.StoreView, error)
	OpenArtifacts func(context.Context, model.WorkflowID) (*artifact.Store, error)
}

// NewRecoveryEngine constructs the Recovery Engine.
func NewRecoveryEngine(opts RecoveryEngineOptions) (*RecoveryEngine, error) {
	if opts.GitFlow == nil {
		return nil, model.InvalidInputFault("recovery: gitflow is required")
	}
	if opts.Home == "" || opts.ProjectKey == "" {
		return nil, model.InvalidInputFault("recovery: home and project key are required")
	}
	if opts.OpenView == nil || opts.OpenArtifacts == nil {
		return nil, model.InvalidInputFault("recovery: store and artifact collectors are required")
	}
	return &RecoveryEngine{
		sup:           opts.Supervisor,
		git:           opts.GitFlow,
		home:          opts.Home,
		projectKey:    opts.ProjectKey,
		evidenceDir:   opts.EvidenceDir,
		openView:      opts.OpenView,
		openArtifacts: opts.OpenArtifacts,
	}, nil
}

// Reconcile evaluates the facts in the design's order (17.1) and returns
// the typed ReconciliationOutcome. A hard failure of the authoritative
// fact sources (schema incompatibility, unreadable store) returns an
// error; per-Intent contradictions become outcome Faults.
func (e *RecoveryEngine) Reconcile(ctx context.Context, scope Scope) (ReconciliationOutcome, error) {
	out := ReconciliationOutcome{}

	// 1. CFLOW_HOME posture and schema/Artifact compatibility: the
	// mutation path requires a safe home and an interpretable database
	// (the Task 7 hook's checks, now part of the Engine).
	if err := ensureHome(e.home); err != nil {
		return out, err
	}
	if _, err := security.CheckHome(security.HomeRequest{Path: e.home}); err != nil {
		return out, err
	}
	if scope.Workflow == "" {
		return out, nil
	}

	// 4. Aggregate invariants and unfinished intents (the View loads the
	// state and the pending Intent ledger).
	view, err := e.openView(ctx, scope.Workflow)
	if err != nil {
		return out, err
	}
	state := view.State
	if state.Workflow.ID == "" {
		// No workflow row: nothing to reconcile at this scope.
		return out, nil
	}
	if err := model.ValidateState(state); err != nil {
		return out, model.InvariantFault(fmt.Errorf("recovery: aggregate invariants cannot be reconciled: %w", err))
	}
	// 9. Approval and binding facts: the append-only Approvals must
	// reference persisted Artifacts (an Approval whose Artifact vanished
	// is drift the user must act on).
	for _, ap := range state.Approvals {
		store, err := e.openArtifacts(ctx, scope.Workflow)
		if err != nil {
			return out, err
		}
		for _, ref := range ap.Refs {
			if _, err := store.Get(ctx, ref); err != nil {
				out.Faults = append(out.Faults, *model.NewFault(model.CodeDirtyWorktreeDrifted,
					"recovery: approved artifact is missing: "+ref.String()))
			}
		}
	}
	// 6. Quarantine audit facts: every Branch Quarantine Record pins its
	// discovered HEAD through a unique append-only
	// refs/cflow/<workflow>/quarantine/<quarantine-id> audit Ref. A
	// missing Ref or a Ref at a different HEAD is drift the user must act
	// on — the quarantine evidence must never vanish (PRD 已确认：漂移窗口
	// Commit 的隔离与替代执行 step 1).
	for _, q := range state.Quarantines {
		if q.AuditRef == "" {
			out.Faults = append(out.Faults, *model.NewFault(model.CodeDirtyWorktreeDrifted,
				"recovery: quarantine record "+q.ID+" carries no audit ref"))
			continue
		}
		facts, err := e.git.Observe(ctx, gitflow.RefLookup{Ref: q.AuditRef})
		if err != nil {
			out.Faults = append(out.Faults, *model.NewFault(model.CodeDirtyWorktreeDrifted,
				"recovery: quarantine audit ref facts are unreadable: "+q.AuditRef))
			continue
		}
		rf, ok := facts.(gitflow.RefFacts)
		if !ok || !rf.Exists || rf.Value != q.ToHead {
			out.Faults = append(out.Faults, *model.NewFault(model.CodeDirtyWorktreeDrifted,
				"recovery: quarantine audit ref is missing or moved: "+q.AuditRef))
		}
	}

	// 5-8. Per-Intent reconciliation: Artifact facts, Git facts,
	// verification/review evidence, and the unfinished Effect Intents of
	// the persisted ledger (design 17.2). Every ledger entry receives
	// exactly one disposition; the facts are collected per kind.
	for _, pe := range view.PendingEffects {
		d, err := e.classify(ctx, scope.Workflow, state, pe)
		if err != nil {
			return out, err
		}
		out.Dispositions = append(out.Dispositions, d)
		switch d.Disposition {
		case BlockedDrift:
			out.Faults = append(out.Faults, *model.NewFault(model.CodeDirtyWorktreeDrifted,
				"recovery: "+d.Reason))
		case FatalInvariant:
			out.Faults = append(out.Faults, *model.NewFault(model.CodeStateInvariantViolation,
				"recovery: "+d.Reason))
		}
	}
	return out, nil
}

// classify produces the exactly-one disposition of one unfinished Effect
// Intent from the collected external facts (design 17.2).
func (e *RecoveryEngine) classify(ctx context.Context, wf model.WorkflowID, state model.State, pe store.PendingEffect) (IntentDisposition, error) {
	base := IntentDisposition{ID: pe.ID, Intent: pe.Intent}
	switch intent := pe.Intent.(type) {
	case model.IntegrationMergeIntent:
		return e.classifyMerge(ctx, wf, state, base, intent)
	case model.IntegrationRollbackIntent:
		return e.classifyRollback(ctx, wf, state, base, intent)
	case model.WorkspaceMergeIntent:
		return e.classifyWorkspaceMerge(ctx, wf, state, base, intent)
	case model.WorkspaceRollbackIntent:
		return e.classifyWorkspaceRollback(ctx, wf, state, base, intent)
	case model.LayoutMigrationIntent:
		return e.classifyLayoutMigration(ctx, wf, state, base, intent)
	case model.GitAuditRefCreateIntent:
		return e.classifyAuditRef(ctx, base, intent)
	case model.TaskWorktreeCreateIntent:
		return e.classifyTaskWorktree(ctx, wf, state, base, intent)
	case model.IntegrationWorktreeCreateIntent:
		return e.classifyWorktreeAt(ctx, base, e.integrationPath(wf), "integration",
			intent.BaseCommit, "")
	case model.PlanningWorktreeCreateIntent:
		return e.classifyWorktreeAt(ctx, base, e.planningPath(wf), "planning",
			intent.BaseCommit, "")
	case model.WorkspaceWorktreeCreateIntent:
		// The Workspace branch is created by, and only by, this Intent's
		// create (design 8.1); classify against the intent's own Path.
		return e.classifyWorktreeAt(ctx, base, intent.Path, "workspace",
			intent.BaseHead, intent.Branch)
	case model.VerificationRunIntent:
		return e.classifyVerification(ctx, wf, base, intent)
	case model.ProviderStartIntent, model.ProviderResumeIntent:
		var session model.SessionID
		switch t := intent.(type) {
		case model.ProviderStartIntent:
			session = t.Session
		case model.ProviderResumeIntent:
			session = t.Session
		}
		return e.classifyProviderSession(base, state, session)
	case model.ArtifactWriteIntent:
		return e.classifyArtifact(ctx, wf, base, intent)
	case model.WorkflowCompileIntent:
		return e.classifyWorkflowCompile(ctx, wf, base)
	case model.ManagedProcessStopIntent:
		return e.classifyProcessStop(base, state, intent.Process)
	case model.GitCommitInspectIntent:
		return base.with(SafeToRetry, "a commit observation is safely re-runnable"), nil
	case model.ApplyStagingCreateIntent:
		return e.classifyApplyStaging(ctx, base, state, intent.Apply)
	case model.ApplyFastForwardIntent:
		return e.classifyApplyDelivery(base, state, intent.Apply)
	case model.CleanupWorktreeRemoveIntent:
		return e.classifyCleanupWorktree(ctx, base, state, intent)
	case model.CleanupScratchRemoveIntent:
		return e.classifyCleanupScratch(base, state, intent)
	case model.ProviderCancelIntent:
		return base.with(FatalInvariant,
			"the effect is not reconcilable by this build; authoritative evidence is missing"), nil
	default:
		return base.with(FatalInvariant, "unknown effect intent in the ledger"), nil
	}
}

// classifyMerge classifies one unfinished IntegrationMergeIntent from the
// Integration Worktree facts (design 17.2, PRD 已确认：Merge Conflict 处
// 理): the expected pre-merge HEAD is the compare-and-swap value, the
// verified Task Commit must be contained by the new Integration HEAD,
// and a Git-clean Worktree is required — a completed merge is never
// repeated. A terminal Workflow whose Integration Worktree was removed by
// a Cleanup no longer owes its historical merge: the subject is gone, the
// merge is never pretended present or re-run.
func (e *RecoveryEngine) classifyMerge(ctx context.Context, wf model.WorkflowID, state model.State, base IntentDisposition, intent model.IntegrationMergeIntent) (IntentDisposition, error) {
	if state.Workflow.Runtime.IsTerminal() {
		if present, err := e.worktreeRegistered(ctx, e.integrationPath(wf)); err == nil && !present {
			return base.with(AlreadyCompleted,
				"the integration worktree was removed by cleanup; the historical merge is not owed"), nil
		}
	}
	status, err := e.integrationStatus(ctx, wf, "")
	if err != nil {
		return base.with(FatalInvariant, "integration worktree facts are unreadable: "+err.Error()), nil
	}
	head := status.Head
	switch {
	case head == intent.BaseHead && status.Clean():
		// The merge is absent and the expected facts still match.
		return base.with(SafeToRetry, "no merge exists; the recorded pre-merge head still matches"), nil
	case head != intent.BaseHead && !status.Clean():
		// The worktree changed and is dirty: cannot be uniquely explained.
		if e.isDescendant(ctx, head, intent.BaseHead) {
			return base.with(BlockedDrift, "the integration worktree is dirty after the merge"), nil
		}
		return base.with(FatalInvariant, "the integration head is not a descendant of the recorded pre-merge head and the worktree is dirty"), nil
	case head != intent.BaseHead && status.Clean():
		if !e.isDescendant(ctx, head, intent.BaseHead) {
			return base.with(FatalInvariant, "the integration head is not a descendant of the recorded pre-merge head"), nil
		}
		if !e.isDescendant(ctx, head, intent.VerifiedCommit) {
			return base.with(BlockedDrift, "the integration head does not contain the verified task commit"), nil
		}
		return base.with(AlreadyCompleted, "the integration head advanced with the verified task history contained"), nil
	default:
		return base.with(BlockedDrift, "the integration worktree is dirty before any merge"), nil
	}
}

// classifyRollback classifies one unfinished IntegrationRollbackIntent:
// the recorded pre-merge HEAD is the expected value. A terminal Workflow
// whose Integration Worktree was removed by Cleanup no longer owes its
// historical rollback.
func (e *RecoveryEngine) classifyRollback(ctx context.Context, wf model.WorkflowID, state model.State, base IntentDisposition, intent model.IntegrationRollbackIntent) (IntentDisposition, error) {
	if state.Workflow.Runtime.IsTerminal() {
		if present, err := e.worktreeRegistered(ctx, e.integrationPath(wf)); err == nil && !present {
			return base.with(AlreadyCompleted,
				"the integration worktree was removed by cleanup; the historical rollback is not owed"), nil
		}
	}
	status, err := e.integrationStatus(ctx, wf, intent.Head)
	if err != nil {
		return base.with(BlockedDrift, "integration worktree facts are unreadable: "+err.Error()), nil
	}
	if status.Head == intent.Head && status.Clean() {
		return base.with(AlreadyCompleted, "the integration worktree is restored to the recorded pre-merge head"), nil
	}
	return base.with(BlockedDrift, "the integration worktree was not restored to the recorded pre-merge head"), nil
}

// classifyWorkspaceMerge classifies one unfinished WorkspaceMergeIntent
// from the Workspace Worktree facts (design 8.5, TUI task 7): the
// Expected Workspace Head is the compare-and-sawp value, the verified Task
// Commit must be contained by the new Workspace Head, and a Git-clean
// Workspace is required — a completed merge is never repeated. A terminal
// Workflow whose Workspace was removed by a Cleanup no longer owes its
// historical merge.
func (e *RecoveryEngine) classifyWorkspaceMerge(ctx context.Context, wf model.WorkflowID, state model.State, base IntentDisposition, intent model.WorkspaceMergeIntent) (IntentDisposition, error) {
	if state.Workflow.Runtime.IsTerminal() {
		if present, err := e.worktreeRegistered(ctx, e.workspacePath(wf)); err == nil && !present {
			return base.with(AlreadyCompleted,
				"the workspace worktree was removed by cleanup; the historical merge is not owed"), nil
		}
	}
	status, err := e.workspaceStatus(ctx, wf, "")
	if err != nil {
		return base.with(FatalInvariant, "workspace worktree facts are unreadable: "+err.Error()), nil
	}
	head := status.Head
	switch {
	case head == intent.ExpectedWorkspaceHead && status.Clean():
		// The merge is absent and the expected facts still match.
		return base.with(SafeToRetry, "no workspace merge exists; the recorded expected head still matches"), nil
	case head != intent.ExpectedWorkspaceHead && !status.Clean():
		// The worktree changed and is dirty: cannot be uniquely explained.
		if e.isDescendant(ctx, head, intent.ExpectedWorkspaceHead) {
			return base.with(BlockedDrift, "the workspace is dirty after the merge"), nil
		}
		return base.with(FatalInvariant, "the workspace head is not a descendant of the recorded expected head and the worktree is dirty"), nil
	case head != intent.ExpectedWorkspaceHead && status.Clean():
		if !e.isDescendant(ctx, head, intent.ExpectedWorkspaceHead) {
			return base.with(FatalInvariant, "the workspace head is not a descendant of the recorded expected head"), nil
		}
		if !e.isDescendant(ctx, head, intent.VerifiedCommit) {
			return base.with(BlockedDrift, "the workspace head does not contain the verified task commit"), nil
		}
		return base.with(AlreadyCompleted, "the workspace head advanced with the verified task history contained"), nil
	default:
		return base.with(BlockedDrift, "the workspace is dirty before any merge"), nil
	}
}

// classifyWorkspaceRollback classifies one unfinished
// WorkspaceRollbackIntent: the recorded pre-merge HEAD is the expected
// value. A terminal Workflow whose Workspace was removed by Cleanup no
// longer owes its historical rollback.
func (e *RecoveryEngine) classifyWorkspaceRollback(ctx context.Context, wf model.WorkflowID, state model.State, base IntentDisposition, intent model.WorkspaceRollbackIntent) (IntentDisposition, error) {
	if state.Workflow.Runtime.IsTerminal() {
		if present, err := e.worktreeRegistered(ctx, e.workspacePath(wf)); err == nil && !present {
			return base.with(AlreadyCompleted,
				"the workspace worktree was removed by cleanup; the historical rollback is not owed"), nil
		}
	}
	status, err := e.workspaceStatus(ctx, wf, intent.Head)
	if err != nil {
		return base.with(BlockedDrift, "workspace worktree facts are unreadable: "+err.Error()), nil
	}
	if status.Head == intent.Head && status.Clean() {
		return base.with(AlreadyCompleted, "the workspace is restored to the recorded pre-merge head"), nil
	}
	return base.with(BlockedDrift, "the workspace was not restored to the recorded pre-merge head"), nil
}

// classifyLayoutMigration classifies one unfinished LayoutMigrationIntent
// (TUI task 8, design §7.4) from the actual filesystem and persisted
// facts across the four crash windows:
//
//  1. the Intent was persisted but nothing moved yet — every source is
//     still at its legacy path: SafeToRetry (continue from move 0);
//  2. some moves landed — the completed moves are counted from the
//     actual state (sources absent, destinations present): SafeToRetry
//     (continue from the first incomplete move);
//  3. every move landed but the DB Layout facts did not advance —
//     AlreadyCompleted for the moves (the DB transaction is the caller's
//     next step);
//  4. the persisted Layout facts already advanced to Version 2 —
//     AlreadyCompleted.
//
// A move that is neither source-present nor destination-present, or a
// destination that exists while its source also exists, is drift the
// user must act on (BlockedDrift); an unusable manifest is a Fatal
// Invariant.
func (e *RecoveryEngine) classifyLayoutMigration(ctx context.Context, wf model.WorkflowID, state model.State, base IntentDisposition, intent model.LayoutMigrationIntent) (IntentDisposition, error) {
	if state.Workflow.LayoutVersion >= 2 {
		return base.with(AlreadyCompleted, "the workflow layout already advanced to version 2"), nil
	}
	if len(intent.Moves) == 0 {
		return base.with(FatalInvariant, "the layout migration intent carries no moves"), nil
	}
	for i, mv := range intent.Moves {
		srcPresent := e.pathExists(ctx, mv.Source)
		dstPresent := e.pathExists(ctx, mv.Destination)
		switch {
		case srcPresent && !dstPresent:
			// This move has not landed yet: continue from here.
			return base.with(SafeToRetry, "layout migration move "+itoa(i)+" has not landed yet"), nil
		case !srcPresent && dstPresent:
			// This move landed; inspect the next one.
		case srcPresent && dstPresent:
			return base.with(BlockedDrift,
				"layout migration source and destination both exist for move "+itoa(i)), nil
		default:
			return base.with(BlockedDrift,
				"layout migration move "+itoa(i)+" has neither source nor destination"), nil
		}
	}
	// Every move landed: the DB Layout facts advance in the next
	// transaction (AlreadyCompleted for the moves themselves).
	return base.with(AlreadyCompleted, "every layout migration move landed; the database facts advance next"), nil
}

// pathExists reports whether one path exists (stat, ignoring errors).
func (e *RecoveryEngine) pathExists(ctx context.Context, path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// classifyAuditRef classifies one unfinished GitAuditRefCreateIntent with
// expected-absent/expected-value semantics: the append-only Ref must
// either not exist (safe to create) or carry exactly the expected value.
func (e *RecoveryEngine) classifyAuditRef(ctx context.Context, base IntentDisposition, intent model.GitAuditRefCreateIntent) (IntentDisposition, error) {
	facts, err := e.git.Observe(ctx, gitflow.RefLookup{Ref: intent.Ref})
	if err != nil {
		return base.with(BlockedDrift, "audit ref facts are unreadable"), nil
	}
	rf := facts.(gitflow.RefFacts)
	switch {
	case !rf.Exists:
		return base.with(SafeToRetry, "the audit ref is absent; the expected-absent compare-and-swap still holds"), nil
	case rf.Value == intent.Head:
		return base.with(AlreadyCompleted, "the audit ref exists at the expected head"), nil
	default:
		return base.with(BlockedDrift, "the audit ref exists at a different head"), nil
	}
}

// classifyTaskWorktree classifies one unfinished TaskWorktreeCreateIntent
// from the Worktree registry: expected-absent/expected-value semantics
// prevent a duplicate Worktree. The Worktree location is deterministic:
// the aggregated temporary root <root>/tmp/tasks/<node> on Layout Version
// 2 (design 8.5, TUI task 7), the legacy worktrees/<project-key>/
// <workflow-id>/tasks/<node> on Layout 1 (PRD 全局目录结构).
func (e *RecoveryEngine) classifyTaskWorktree(ctx context.Context, wf model.WorkflowID, state model.State, base IntentDisposition, intent model.TaskWorktreeCreateIntent) (IntentDisposition, error) {
	path := filepath.Join(e.home, "worktrees", e.projectKey, string(wf), "tasks", string(intent.Node))
	if state.Workflow.LayoutVersion >= 2 {
		path = filepath.Join(e.home, "projects", e.projectKey, string(wf), "tmp", "tasks", string(intent.Node))
	}
	entries, err := e.worktreeEntries(ctx)
	if err != nil {
		// The registry cannot be read (e.g. the directory is not a Git
		// repository): the intended Worktree is absent, so the
		// expected-absent compare-and-swap still holds.
		return base.with(SafeToRetry, "the worktree registry is unreadable; the worktree is absent"), nil
	}
	entry, ok := entries[path]
	switch {
	case !ok:
		return base.with(SafeToRetry, "the task worktree is absent; the expected-absent compare-and-swap still holds"), nil
	case entry.Branch == intent.Branch && !entry.Detached && e.isDescendant(ctx, entry.Head, intent.BaseHead):
		// The Worktree was created from the Task Base; the coding Commit
		// legitimately advanced its HEAD afterwards.
		return base.with(AlreadyCompleted, "the task worktree exists at the expected branch, descended from the task base"), nil
	default:
		return base.with(BlockedDrift, "the task worktree exists but drifted from the expected branch or base"), nil
	}
}

// classifyWorktreeAt classifies the Planning/Integration Worktree
// creation intents against a derived path.
func (e *RecoveryEngine) classifyWorktreeAt(ctx context.Context, base IntentDisposition, path, name, expectedHead, expectedBranch string) (IntentDisposition, error) {
	entries, err := e.worktreeEntries(ctx)
	if err != nil {
		// The registry cannot be read (e.g. the directory is not a Git
		// repository): the intended Worktree is absent, so the
		// expected-absent compare-and-swap still holds.
		return base.with(SafeToRetry, "the worktree registry is unreadable; the worktree is absent"), nil
	}
	entry, ok := entries[path]
	switch {
	case !ok:
		return base.with(SafeToRetry, "the "+name+" worktree is absent; the expected-absent compare-and-swap still holds"), nil
	case entry.Head == expectedHead || (expectedHead != "" && e.isDescendant(ctx, entry.Head, expectedHead)):
		// The Worktree was created at the expected head; serial merges
		// legitimately advance the Integration HEAD afterwards.
		return base.with(AlreadyCompleted, "the "+name+" worktree exists, descended from the expected head"), nil
	default:
		return base.with(BlockedDrift, "the "+name+" worktree exists but drifted from the expected head"), nil
	}
}

// classifyVerification classifies one unfinished VerificationRunIntent
// from the persisted Evidence Manifest: the manifest is the unique proof
// the run completed (already_completed); its absence with the expected
// Worktree facts still matching is safe to retry.
func (e *RecoveryEngine) classifyVerification(ctx context.Context, wf model.WorkflowID, base IntentDisposition, intent model.VerificationRunIntent) (IntentDisposition, error) {
	manifest, err := e.readVerificationManifest(wf, intent.Node)
	if err != nil {
		return base.with(BlockedDrift, "verification evidence is unreadable"), nil
	}
	if manifest != nil {
		if manifest.CatalogRef.Revision == intent.Catalog.Revision &&
			manifest.CatalogRef.Hash == intent.Catalog.Hash &&
			manifest.CommitRange == intent.CommitRange {
			return base.with(AlreadyCompleted, "the verification evidence manifest proves the run completed"), nil
		}
		return base.with(BlockedDrift, "the verification evidence manifest does not match the intent identity"), nil
	}
	return base.with(SafeToRetry, "no verification evidence exists; the run is safely re-runnable"), nil
}

// classifyApplyStaging classifies one unfinished ApplyStagingCreateIntent
// from the Apply Attempt ledger (design 17.2, PRD 已确认：显式受保护
// Apply): a staging the attempt no longer owes is already completed; a
// staging the attempt still owes is safely re-runnable — the staging
// executor re-observes every git fact and reuses the Apply Worktree, so
// the re-run is exactly the retry the PRD prescribes. A missing attempt
// is an invariant failure.
func (e *RecoveryEngine) classifyApplyStaging(ctx context.Context, base IntentDisposition, state model.State, apply model.ApplyAttemptID) (IntentDisposition, error) {
	att := findApplyAttempt(state, apply)
	if att == nil {
		return base.with(FatalInvariant, "the apply attempt is missing from the aggregate"), nil
	}
	switch att.Status {
	case model.ApplyStaging:
		return base.with(SafeToRetry,
			"the apply staging is re-runnable; the executor re-observes every fact and reuses the apply worktree"), nil
	case model.ApplyAwaitingConfirmation, model.ApplyRunning, model.ApplySucceeded,
		model.ApplyBlocked, model.ApplyFailed, model.ApplyCancelled:
		return base.with(AlreadyCompleted, "the apply staging is no longer owed"), nil
	}
	return base.with(FatalInvariant, "the apply attempt carries an unknown status"), nil
}

// classifyApplyDelivery classifies one unfinished ApplyFastForwardIntent
// from the Apply Attempt ledger: only a RUNNING attempt owes the
// compare-and-swap (safely re-runnable — the executor observes the
// actual ref and never re-swaps a delivered Target); every other status
// proves the delivery settled already or is inconsistent.
func (e *RecoveryEngine) classifyApplyDelivery(base IntentDisposition, state model.State, apply model.ApplyAttemptID) (IntentDisposition, error) {
	att := findApplyAttempt(state, apply)
	if att == nil {
		return base.with(FatalInvariant, "the apply attempt is missing from the aggregate"), nil
	}
	switch att.Status {
	case model.ApplyRunning:
		return base.with(SafeToRetry,
			"the apply delivery is re-runnable; the executor observes the actual target ref and never re-swaps a delivered target"), nil
	case model.ApplySucceeded, model.ApplyBlocked, model.ApplyFailed, model.ApplyCancelled:
		return base.with(AlreadyCompleted, "the apply delivery is no longer owed"), nil
	case model.ApplyStaging, model.ApplyAwaitingConfirmation:
		return base.with(FatalInvariant, "the apply delivery intent precedes its state"), nil
	}
	return base.with(FatalInvariant, "the apply attempt carries an unknown status"), nil
}

// findApplyAttempt returns one Apply Attempt of the aggregate.
func findApplyAttempt(state model.State, id model.ApplyAttemptID) *model.ApplyAttempt {
	for i := range state.ApplyAttempts {
		if state.ApplyAttempts[i].ID == id {
			return &state.ApplyAttempts[i]
		}
	}
	return nil
}

// classifyCleanupWorktree classifies one unfinished cleanup Worktree
// removal from the exact target's registry state (design 17.4 partial
// recovery): a settled item is already completed; an item whose exact
// target is absent from the Git Worktree Registry was already removed
// (never pretended present); a target still registered at the exact
// expected Branch/HEAD is safely re-runnable (the removal never ran); a
// target that drifted is blocked. The pending Intent ledger only ever
// holds the already-Requested item, so recovery never starts a new
// deletion beyond that set.
func (e *RecoveryEngine) classifyCleanupWorktree(ctx context.Context, base IntentDisposition, state model.State, intent model.CleanupWorktreeRemoveIntent) (IntentDisposition, error) {
	item, ok := cleanupItemOf(state, intent.Cleanup, intent.Item)
	if !ok {
		return base.with(FatalInvariant, "the cleanup attempt or item is missing from the aggregate"), nil
	}
	if item.Status.IsTerminal() {
		return base.with(AlreadyCompleted, "the cleanup item already settled"), nil
	}
	entries, err := e.worktreeEntries(ctx)
	if err != nil {
		return base.with(BlockedDrift, "the worktree registry is unreadable"), nil
	}
	entry, present := entries[item.CanonicalPath]
	switch {
	case !present:
		return base.with(AlreadyCompleted, "the target worktree is already absent from the registry"), nil
	case entry.Branch == item.Branch && !entry.Detached && entry.Head == item.ExpectedHead:
		return base.with(SafeToRetry, "the target worktree still matches the confirmed manifest; the removal is re-runnable"), nil
	default:
		return base.with(BlockedDrift, "the target worktree drifted from the confirmed manifest"), nil
	}
}

// classifyCleanupScratch classifies one unfinished cleanup scratch removal
// from the exact target's filesystem state: an absent exact target was
// already removed; a still-present exact target is safely re-runnable.
func (e *RecoveryEngine) classifyCleanupScratch(base IntentDisposition, state model.State, intent model.CleanupScratchRemoveIntent) (IntentDisposition, error) {
	item, ok := cleanupItemOf(state, intent.Cleanup, intent.Item)
	if !ok {
		return base.with(FatalInvariant, "the cleanup attempt or item is missing from the aggregate"), nil
	}
	if item.Status.IsTerminal() {
		return base.with(AlreadyCompleted, "the cleanup item already settled"), nil
	}
	if _, err := os.Lstat(item.CanonicalPath); os.IsNotExist(err) {
		return base.with(AlreadyCompleted, "the exact scratch target is already absent"), nil
	} else if err != nil {
		return base.with(BlockedDrift, "the scratch target cannot be inspected"), nil
	}
	return base.with(SafeToRetry, "the exact scratch target is still present and removable"), nil
}

// cleanupItemOf returns one Cleanup item of one Cleanup Attempt.
func cleanupItemOf(state model.State, att model.CleanupAttemptID, index int) (model.CleanupItem, bool) {
	for i := range state.CleanupAttempts {
		if state.CleanupAttempts[i].ID != att {
			continue
		}
		if index < 0 || index >= len(state.CleanupAttempts[i].Items) {
			return model.CleanupItem{}, false
		}
		return state.CleanupAttempts[i].Items[index], true
	}
	return model.CleanupItem{}, false
}

// classifyProviderSession classifies one unfinished Provider start/resume
// intent from the Session ledger: a terminal Session proves the run
// settled; an open Session means the effect (a live run) is absent.
func (e *RecoveryEngine) classifyProviderSession(base IntentDisposition, state model.State, id model.SessionID) (IntentDisposition, error) {
	for _, s := range state.Sessions {
		if s.ID != id {
			continue
		}
		if s.Status.IsTerminal() {
			return base.with(AlreadyCompleted, "the provider session settled terminal"), nil
		}
		return base.with(SafeToRetry, "the provider session is open and no live run exists"), nil
	}
	return base.with(BlockedDrift, "the provider session the intent started no longer exists"), nil
}

// classifyArtifact classifies one unfinished ArtifactWriteIntent from the
// Artifact Store facts (design 17.1 order 5). The Kernel's Intent names
// the target Revision (the content hash is executor-assigned), so the
// revision directory is the identity: a revision directory carrying its
// content file proves the write completed; a directory without its
// content file is an orphan (blocked — the file vanished after the
// write); an absent revision is safe to write.
func (e *RecoveryEngine) classifyArtifact(ctx context.Context, wf model.WorkflowID, base IntentDisposition, intent model.ArtifactWriteIntent) (IntentDisposition, error) {
	store, err := e.openArtifacts(ctx, wf)
	if err != nil {
		return base.with(BlockedDrift, "artifact facts are unreadable"), nil
	}
	if intent.Ref.Revision == 0 {
		// The executor assigns the next Revision; any written Revision of
		// the type proves the write chain completed.
		ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: intent.Ref.Type})
		if err == nil && ref.Revision >= 1 {
			return base.with(AlreadyCompleted, "the artifact type carries a written revision"), nil
		}
		return base.with(SafeToRetry, "no artifact revision of the type exists"), nil
	}
	if intent.Ref.Hash != "" {
		// The intent pinned an exact content identity: the file must read
		// back with it.
		if _, err := store.Get(ctx, intent.Ref); err == nil {
			return base.with(AlreadyCompleted, "the artifact revision reads back with the exact identity"), nil
		}
	}
	dir := filepath.Join(e.artifactRoot(wf), string(wf), string(intent.Ref.Type), fmt.Sprintf("%d", intent.Ref.Revision))
	entries, err := os.ReadDir(dir)
	switch {
	case err == nil:
		for _, entry := range entries {
			if isArtifactContent(entry) {
				return base.with(AlreadyCompleted, "the artifact revision directory carries its content file"), nil
			}
		}
		return base.with(BlockedDrift, "the artifact revision directory exists without its content file"), nil
	case os.IsNotExist(err):
		return base.with(SafeToRetry, "the artifact revision is absent"), nil
	default:
		return base.with(BlockedDrift, "the artifact revision directory cannot be inspected"), nil
	}
}

// isArtifactContent reports whether one directory entry is an Artifact
// content file (not the atomic-write temp or a subdirectory).
func isArtifactContent(entry os.DirEntry) bool {
	return !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".")
}

// classifyWorkflowCompile classifies one unfinished WorkflowCompileIntent
// from the compiled Workflow Artifact.
func (e *RecoveryEngine) classifyWorkflowCompile(ctx context.Context, wf model.WorkflowID, base IntentDisposition) (IntentDisposition, error) {
	store, err := e.openArtifacts(ctx, wf)
	if err != nil {
		return base.with(BlockedDrift, "artifact facts are unreadable"), nil
	}
	if _, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactWorkflow}); err == nil {
		return base.with(AlreadyCompleted, "the compiled workflow artifact exists"), nil
	}
	return base.with(SafeToRetry, "the compiled workflow artifact is absent"), nil
}

// classifyProcessStop classifies one unfinished ManagedProcessStopIntent:
// the ledger's process row is the only fact, and a RUNNING row is never
// verifiable — the model records no OS identity (design 13.2: PID plus
// start token stays in the process adapter), so a restarting Runtime
// cannot Inspect the row. Claiming the process stopped would silently
// settle a provider child that may have survived a killed cflow and can
// keep writing to its Worktree, so an unverified RUNNING row fails
// closed: the disposition is blocked_drift and the mutation blocks with
// a user-action fault demanding manual confirmation of the process's OS
// state before anything resumes. A settled row (EXITED/STOPPED) or an
// absent record is already stopped.
func (e *RecoveryEngine) classifyProcessStop(base IntentDisposition, state model.State, id model.ProcessID) (IntentDisposition, error) {
	for _, p := range state.Processes {
		if p.ID != id {
			continue
		}
		if p.Status == model.ProcessStatusRunning {
			return base.with(BlockedDrift,
				"the managed process "+string(id)+" is RUNNING but its OS identity was not persisted; confirm the process is gone before retrying"), nil
		}
		return base.with(AlreadyCompleted, "the managed process is no longer running"), nil
	}
	return base.with(AlreadyCompleted, "the managed process record no longer exists"), nil
}

// ---------------------------------------------------------------------------
// fact collectors
// ---------------------------------------------------------------------------

// integrationStatus observes the Integration Worktree's HEAD and status;
// expectedHead, when set, must match (fail-closed).
func (e *RecoveryEngine) integrationStatus(ctx context.Context, wf model.WorkflowID, expectedHead string) (gitflow.StatusFacts, error) {
	facts, err := e.git.Observe(ctx, gitflow.GitStatus{
		Dir: e.integrationPath(wf), ExpectedHead: expectedHead, UntrackedAll: true,
	})
	if err != nil {
		return gitflow.StatusFacts{}, err
	}
	return facts.(gitflow.StatusFacts), nil
}

// workspaceStatus observes the aggregated Workspace Worktree status
// (design 8.5, TUI task 7: <home>/projects/<key>/<wf>/workspace).
func (e *RecoveryEngine) workspaceStatus(ctx context.Context, wf model.WorkflowID, expectedHead string) (gitflow.StatusFacts, error) {
	facts, err := e.git.Observe(ctx, gitflow.GitStatus{
		Dir: e.workspacePath(wf), ExpectedHead: expectedHead, UntrackedAll: true,
	})
	if err != nil {
		return gitflow.StatusFacts{}, err
	}
	return facts.(gitflow.StatusFacts), nil
}

// isDescendant reports whether from's history contains to (to is an
// ancestor of from: rev-list from..to is empty).
func (e *RecoveryEngine) isDescendant(ctx context.Context, from, to string) bool {
	facts, err := e.git.Observe(ctx, gitflow.HistoryRange{From: from, To: to})
	if err != nil {
		return false
	}
	rf := facts.(gitflow.RangeFacts)
	return len(rf.Commits) == 0
}

// worktreeEntries maps the canonical Worktree registry paths to entries.
func (e *RecoveryEngine) worktreeEntries(ctx context.Context) (map[string]gitflow.WorktreeEntry, error) {
	facts, err := e.git.Observe(ctx, gitflow.WorktreeList{})
	if err != nil {
		return nil, err
	}
	entries := map[string]gitflow.WorktreeEntry{}
	for _, entry := range facts.(gitflow.WorktreeFacts).Entries {
		entries[entry.Path] = entry
	}
	return entries, nil
}

// worktreeRegistered reports whether one exact path is in the Git Worktree
// Registry.
func (e *RecoveryEngine) worktreeRegistered(ctx context.Context, path string) (bool, error) {
	entries, err := e.worktreeEntries(ctx)
	if err != nil {
		return false, err
	}
	_, ok := entries[path]
	return ok, nil
}

// verificationManifest is the persisted evidence shape the Application
// writes after every Verification run (recovery order 7): the canonical
// model.EvidenceManifest JSON (the field names bind the model's own
// serialization, so the manifest the Recovery reads is the manifest the
// Engine produced).
type verificationManifest struct {
	Node       string `json:"Node"`
	CatalogRef struct {
		Revision int    `json:"Revision"`
		Hash     string `json:"Hash"`
	} `json:"CatalogRef"`
	CommitRange string `json:"CommitRange"`
	Passed      bool   `json:"Passed"`
}

func (e *RecoveryEngine) readVerificationManifest(wf model.WorkflowID, node model.NodeID) (*verificationManifest, error) {
	if e.evidenceDir == "" {
		return nil, nil
	}
	path := filepath.Join(e.evidenceDir, "verification", string(wf), string(node)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m verificationManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("verification manifest %s cannot be parsed: %w", path, err)
	}
	return &m, nil
}

// integrationPath is the deterministic Integration Worktree location.
func (e *RecoveryEngine) integrationPath(wf model.WorkflowID) string {
	return filepath.Join(e.home, "worktrees", e.projectKey, string(wf), "integration")
}

// workspacePath is the aggregated Workspace Worktree root of one workflow
// (design 8.5, TUI task 7): <home>/projects/<key>/<workflow-id>/workspace.
func (e *RecoveryEngine) workspacePath(wf model.WorkflowID) string {
	return filepath.Join(e.home, "projects", e.projectKey, string(wf), "workspace")
}

// planningPath is the deterministic Planning Snapshot location.
func (e *RecoveryEngine) planningPath(wf model.WorkflowID) string {
	return filepath.Join(e.home, "worktrees", e.projectKey, string(wf), "planning")
}

// artifactRoot is the deterministic Artifact Store root of one workflow.
func (e *RecoveryEngine) artifactRoot(wf model.WorkflowID) string {
	return filepath.Join(e.home, "projects", e.projectKey, "workflows", string(wf), "artifacts")
}

// ensureHome creates CFLOW_HOME 0700 when missing (design 19.1); reads
// never reach this path.
func ensureHome(home string) error {
	if _, err := os.Stat(home); err == nil {
		return nil
	}
	return os.MkdirAll(home, 0o700)
}

// with builds one disposition entry.
func (b IntentDisposition) with(d Disposition, reason string) IntentDisposition {
	b.Disposition = d
	b.Reason = reason
	return b
}

// itoa renders a small integer.
func itoa(n int) string {
	return strconv.Itoa(n)
}
