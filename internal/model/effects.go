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
// Worktree (PRD Worktree 策略; Task 12).
type ProviderStartIntent struct {
	Session    SessionID
	Purpose    AgentPurpose
	Route      string
	Supersedes string
	Node       NodeID
}

func (ProviderStartIntent) isEffectIntent() {}

// ProviderResumeIntent resumes a Provider Session.
type ProviderResumeIntent struct {
	Session SessionID
	Purpose AgentPurpose
}

func (ProviderResumeIntent) isEffectIntent() {}

// ProviderCancelIntent cancels a Provider Session.
type ProviderCancelIntent struct {
	Session SessionID
}

func (ProviderCancelIntent) isEffectIntent() {}

// PlanningWorktreeCreateIntent creates the Planning Snapshot Worktree
// fixed at the recorded Base Commit (design 15.2). The Base Commit is an
// expected-HEAD value, fixed before the Effect (design 6.2 rule 6).
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
// Attempt whose failure the rollback settles.
type IntegrationRollbackIntent struct {
	Head    string
	Attempt AttemptKey
}

func (IntegrationRollbackIntent) isEffectIntent() {}

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
// Apply Worktree.
type ApplyStagingCreateIntent struct {
	Apply           ApplyAttemptID
	TargetHead      string
	IntegrationHead string
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
