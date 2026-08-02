package model

// ---------------------------------------------------------------------------
// Effect Intents (design 6.3): the closed union of Runtime-owned
// operations. An external Effect is not executed until its Intent and
// expected facts commit; its Result is an immutable evidence input to
// another Decision.
// ---------------------------------------------------------------------------

// EffectIntent is the closed union of typed Effect Intents.
type EffectIntent interface{ isEffectIntent() }

// ArtifactWriteIntent persists one Artifact Revision.
type ArtifactWriteIntent struct {
	Ref ArtifactRef
}

func (ArtifactWriteIntent) isEffectIntent() {}

// ProviderStartIntent starts a Provider Session for one Agent Purpose.
type ProviderStartIntent struct {
	Session SessionID
	Purpose AgentPurpose
	Route   string
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

// PlanningWorktreeCreateIntent creates the Planning Snapshot Worktree.
type PlanningWorktreeCreateIntent struct {
	Workflow WorkflowID
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
type IntegrationMergeIntent struct {
	Node     NodeID
	BaseHead string
}

func (IntegrationMergeIntent) isEffectIntent() {}

// IntegrationRollbackIntent restores the managed Integration Worktree to
// a recorded pre-merge HEAD.
type IntegrationRollbackIntent struct {
	Head string
}

func (IntegrationRollbackIntent) isEffectIntent() {}

// VerificationRunIntent runs one approved Catalog Entry by identity.
type VerificationRunIntent struct {
	Node        NodeID
	Catalog     CatalogRef
	CommitRange string
}

func (VerificationRunIntent) isEffectIntent() {}

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
