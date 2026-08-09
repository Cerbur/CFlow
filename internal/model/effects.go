package model

// ---------------------------------------------------------------------------
// Effect Intents (design 6.3): the closed union of Runtime-owned
// operations. An external Effect is not executed until its Intent and
// expected facts commit; its Result is an immutable evidence input to
// another Decision.
// ---------------------------------------------------------------------------

// EffectIntent is the closed union of typed Effect Intents.
type EffectIntent interface{ isEffectIntent() }

// ArtifactWriteIntent persists one Artifact Revision. Ref names the
// target identity; Revision 0 means the executor assigns the next
// Revision of the type (the aggregate does not track every type's
// counter). Body is the authored body; Producer binds the Artifact to
// the Session lineage that produced it (design 10.1).
type ArtifactWriteIntent struct {
	Ref      ArtifactRef
	Body     []byte
	Producer AgentPurpose
	Session  SessionID
}

func (ArtifactWriteIntent) isEffectIntent() {}

// ProviderStartIntent starts a Provider Session for one Agent Purpose.
// Supersedes is the Provider Session ID of the Session this run succeeds
// in its role lineage (design 14.4); the Runtime verifies the lineage
// before any Provider call. Node fixes the allocated Attempt's Node for a
// coding Session: the executor runs it only inside that Node's Task
// Worktree (PRD Worktree 策略; Task 12). Process is the Application-
// allocated managed Process identity the Kernel records as RUNNING with
// the Session (the controlled-stop ledger, design 13.3); the executor
// binds the live process facts to it.
type ProviderStartIntent struct {
	Session    SessionID
	Purpose    AgentPurpose
	Route      string
	Supersedes string
	Node       NodeID
	Process    ProcessID
}

func (ProviderStartIntent) isEffectIntent() {}

// ProviderResumeIntent resumes a Provider Session.
type ProviderResumeIntent struct {
	Session SessionID
	Purpose AgentPurpose
	Process ProcessID
}

func (ProviderResumeIntent) isEffectIntent() {}

// ProviderCancelIntent cancels a Provider Session.
type ProviderCancelIntent struct {
	Session SessionID
}

func (ProviderCancelIntent) isEffectIntent() {}

// WorkspaceWorktreeCreateIntent creates the single long-lived Workspace
// Branch/Worktree of a new Workflow (design 8.1). BaseHead is the
// expected-HEAD value the branch is created from; Branch and Path are the
// identity facts the Application derived through the layout Resolver and
// the PostgreSQL persisted (design 6.2 rule 6, TUI task 4).
type WorkspaceWorktreeCreateIntent struct {
	Workflow WorkflowID
	BaseHead string
	Branch   string
	Path     string
}

func (WorkspaceWorktreeCreateIntent) isEffectIntent() {}

// PlanningWorktreeCreateIntent creates the Planning Snapshot Worktree
// fixed at the recorded Base Commit (design 15.2). The Base Commit is an
// expected-HEAD value, fixes before the Effect (design 6.2 rule 6).
// Task 4 keeps this only as the Legacy Layout path; new Workflows use
// WorkspaceWorktreeCreateIntent.
type PlanningWorktreeCreateIntent struct {
	Workflow   WorkflowID
	BaseCommit string
}

func (PlanningWorktreeCreateIntent) isEffectIntent() {}

// IntegrationWorktreeCreateIntent creates the Integration Branch/Worktree.
type IntegrationWorktreeCreateIntent struct {
	Workflow   WorkflowID
	BaseCommit string
}

func (IntegrationWorktreeCreateIntent) isEffectIntent() {}

// TaskWorktreeCreateIntent creates one isolated Task Branch/Worktree.
type TaskWorktreeCreateIntent struct {
	Node     NodeID
	Branch   string
	BaseHead string
}

func (TaskWorktreeCreateIntent) isEffectIntent() {}

// GitCommitInspectIntent observes Git commit facts.
type GitCommitInspectIntent struct {
	Ref string
}

func (GitCommitInspectIntent) isEffectIntent() {}

// GitAuditRefCreateIntent creates one append-only audit Ref.
type GitAuditRefCreateIntent struct {
	Ref  string
	Head string
}

func (GitAuditRefCreateIntent) isEffectIntent() {}

// IntegrationMergeIntent performs one serial --no-ff Integration merge.
// BaseHead is the recorded Integration HEAD the merge must observe
// (compare-and-swap); TaskBranch and VerifiedCommit fix the exact Task
// Branch and the accepted Commit the merge must bring in, so the merge
// can never be retargeted (PRD 约束: Merge 前再次比较已验收 Commit、Task
// Branch HEAD 和 Git-clean 状态; design 15.5).
type IntegrationMergeIntent struct {
	Node           NodeID
	BaseHead       string
	TaskBranch     string
	VerifiedCommit string
}

func (IntegrationMergeIntent) isEffectIntent() {}

// IntegrationRollbackIntent restores the managed Integration Worktree to
// a recorded pre-merge HEAD after a failed merge. Attempt fixes the
// Attempt whose failure the rollback settles; FailureCode carries the
// typed failure the Attempt settles with once the Worktree is restored
// ("" means the default MERGE_CONFLICT).
type IntegrationRollbackIntent struct {
	Head        string
	Attempt     AttemptKey
	FailureCode Code
}

func (IntegrationRollbackIntent) isEffectIntent() {}

// WorkspaceMergeIntent merges one verified Task Branch into the single
// long-lived Workspace (design 8.5, TUI task 7): ExpectedWorkspaceHead is
// the current verified Workspace Head the merge must observe
// (compare-and-sawp); TaskBranch and VerifiedCommit fix the exact Task
// Branch and the accepted Commit the merge must bring in. Parallel sibling
// Tasks may share an old Base, but every merge Intent fixes the LATEST
// verified Workspace Head at scheduling time; merges are serial --no-ff
// and never auto-rebase or rewrite Task history.
type WorkspaceMergeIntent struct {
	Node                  NodeID
	ExpectedWorkspaceHead string
	TaskBranch            string
	VerifiedCommit        string
}

func (WorkspaceMergeIntent) isEffectIntent() {}

// WorkspaceRollbackIntent restores the managed Workspace Worktree to a
// recorded pre-merge HEAD after a failed Workspace merge. Attempt fixes
// the Attempt whose failure the rollback settles; FailureCode carries the
// typed failure the Attempt settles with once the Worktree is restored
// ("" means the default MERGE_CONFLICT).
type WorkspaceRollbackIntent struct {
	Head        string
	Attempt     AttemptKey
	FailureCode Code
}

func (WorkspaceRollbackIntent) isEffectIntent() {}

// PathMoveKind is one migration move kind (TUI task 8).
type PathMoveKind string

const (
	// MoveKindWorktree moves one managed Git Worktree.
	MoveKindWorktree PathMoveKind = "worktree"
	// MoveKindArtifact moves one managed directory or file.
	MoveKindArtifact PathMoveKind = "artifact"
)

// PathMove is one exact source→destination move of a Legacy Layout
// Migration. Branch and Head bind a Worktree move to its registered
// identity.
type PathMove struct {
	Kind        PathMoveKind `json:"kind"`
	Source      string       `json:"source"`
	Destination string       `json:"destination"`
	Branch      string       `json:"branch,omitempty"`
	Head        string       `json:"head,omitempty"`
	Digest      string       `json:"digest,omitempty"`
}

// LayoutMigrationIntent performs the explicit Legacy Layout Migration of
// one Layout Version 1 workflow into the aggregated layout (design §7.4,
// TUI task 8): the ordered PathMoves move the legacy Worktrees
// (`git worktree move`) and the legacy Artifacts root (safe path move)
// into the aggregated workflow root, and the persisted Layout facts
// advance to Version 2. Moves is the exact ordered list the Preview
// derived and Prepare bound by manifest hash; Done counts the moves
// already completed (recovery continues from the actual state).
type LayoutMigrationIntent struct {
	MigrationID  string     `json:"migration_id"`
	Workflow     WorkflowID `json:"workflow"`
	ManifestHash string     `json:"manifest_hash"`
	PreviewHash  string     `json:"preview_hash"`
	Moves        []PathMove `json:"moves"`
	// Done is retained for forward/backward codec compatibility. Recovery
	// does not trust it: source/destination and Git registry facts are the
	// authoritative per-step progress.
	Done int `json:"done,omitempty"`
}

func (LayoutMigrationIntent) isEffectIntent() {}

// VerificationRunIntent runs one approved Catalog Entry by identity.
type VerificationRunIntent struct {
	Node        NodeID
	Catalog     CatalogRef
	CommitRange string
}

func (VerificationRunIntent) isEffectIntent() {}

// WorkflowCompileIntent compiles the approved Specs, Verification
// Catalog, and the validated Patch IR into the canonical Dynamic
// Workflow body (design 11). PatchBody is the restricted Patch IR the
// WORKFLOW_OPTIMIZATION Session produced; the executor resolves the
// Spec and Catalog bodies from the Artifact Store and returns the
// compiled body plus the inert rejected Patch operations.
type WorkflowCompileIntent struct {
	PatchBody []byte
}

func (WorkflowCompileIntent) isEffectIntent() {}

// ApplyStagingCreateIntent stages the Integration output in an isolated
// Apply Worktree (PRD 已确认：显式受保护 Apply steps 1-4): the executor
// revalidates the user workspace and the Commit Policy, creates the
// Apply Branch/Worktree from the recorded Target HEAD, merges the
// Integration Branch with --no-ff (the ONE restricted Merge Resolution
// Session when ResolutionSession is set), verifies the Merge Commit
// against the Preflight, and runs the full deterministic apply
// verification. The user's working tree is never touched.
type ApplyStagingCreateIntent struct {
	Apply           ApplyAttemptID
	TargetHead      string
	IntegrationHead string
	// ResolutionSession is the ONE restricted Merge Resolution Session
	// allocated for this staging run ("" when none).
	ResolutionSession SessionID
}

func (ApplyStagingCreateIntent) isEffectIntent() {}

// ApplyFastForwardIntent performs the compare-and-swap Target fast-forward.
type ApplyFastForwardIntent struct {
	Apply      ApplyAttemptID
	TargetHead string
}

func (ApplyFastForwardIntent) isEffectIntent() {}

// ManagedProcessStopIntent stops one managed process through the
// controlled-stop protocol.
type ManagedProcessStopIntent struct {
	Process ProcessID
}

func (ManagedProcessStopIntent) isEffectIntent() {}

// CleanupWorktreeRemoveIntent removes one confirmed Worktree item.
type CleanupWorktreeRemoveIntent struct {
	Cleanup CleanupAttemptID
	Item    int
}

func (CleanupWorktreeRemoveIntent) isEffectIntent() {}

// CleanupScratchRemoveIntent removes one confirmed scratch item.
type CleanupScratchRemoveIntent struct {
	Cleanup CleanupAttemptID
	Item    int
}

func (CleanupScratchRemoveIntent) isEffectIntent() {}
