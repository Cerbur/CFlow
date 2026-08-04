package model

import "fmt"

// ArtifactType names one kind of immutable CFlow Artifact. An Artifact
// Revision's identity includes its type, revision, schema version, and
// content hash (CONTEXT.md: Artifact Revision).
type ArtifactType string

const (
	ArtifactPlan            ArtifactType = "plan"
	ArtifactSpec            ArtifactType = "spec"
	ArtifactWorkflow        ArtifactType = "workflow"
	ArtifactCatalog         ArtifactType = "catalog"
	ArtifactReport          ArtifactType = "report"
	ArtifactCleanupManifest ArtifactType = "cleanup-manifest"
	// ArtifactDiscussionTurn is one immutable, redacted requirement
	// discussion turn linked to its Session lineage through the artifact
	// Producer (Task 10; PRD 需求讨论交互).
	ArtifactDiscussionTurn ArtifactType = "discussion-turn"
	// ArtifactPlanCheck is one immutable Plan Check result (PRD Plan
	// Check 交互).
	ArtifactPlanCheck ArtifactType = "plan-check"
	// ArtifactRoutingPolicy is the immutable per-Purpose approved routing
	// policy of one Execution Approval (Task 16, design 14.2): the
	// ordered approved Provider bindings with the observed executable
	// identity facts. The Execution Approval binds its hash; editing
	// configuration after the Approval changes the resolved content and
	// requires a successor Approval (design 20.1).
	ArtifactRoutingPolicy ArtifactType = "routing-policy"
	// ArtifactBudgetPolicy is the immutable budget policy of one
	// Execution Approval: the configured hard cap and the per-routed-node
	// approved budgets (design 20.1).
	ArtifactBudgetPolicy ArtifactType = "budget-policy"
)

// Valid reports whether t is a declared Artifact Type.
func (t ArtifactType) Valid() bool {
	switch t {
	case ArtifactPlan, ArtifactSpec, ArtifactWorkflow, ArtifactCatalog, ArtifactReport, ArtifactCleanupManifest,
		ArtifactDiscussionTurn, ArtifactPlanCheck, ArtifactRoutingPolicy, ArtifactBudgetPolicy:
		return true
	}
	return false
}

// String renders the Artifact Type.
func (t ArtifactType) String() string { return string(t) }

// ArtifactRef identifies one immutable Artifact Revision by
// (workflow_id, artifact_type, revision, sha256) (design 7.2). The hash is
// part of the identity, so an Approval that binds a Ref binds exact
// content.
type ArtifactRef struct {
	Workflow WorkflowID
	Type     ArtifactType
	Revision int
	Hash     string
}

// String renders the reference in canonical form.
func (r ArtifactRef) String() string {
	return fmt.Sprintf("%s/%s/%d/%s", r.Workflow, r.Type, r.Revision, r.Hash)
}

// ArtifactEnvelope is the canonical serializable Artifact shape the
// immutable Artifact Store hashes and persists (stable interface ledger,
// design 10).
type ArtifactEnvelope struct {
	Type          ArtifactType
	Revision      int
	SchemaVersion string
	ContentHash   string
	Payload       []byte
}

// CatalogRef identifies one immutable Verification Catalog Revision by
// revision and content hash (design 16.1).
type CatalogRef struct {
	Revision int
	Hash     string
}

// ApprovalKind distinguishes the append-only user decisions. Plan and
// Execution Approval are the two normal gates; Commit Policy Approval
// binds an exact Git identity/signing Preflight Revision and fingerprint.
type ApprovalKind string

const (
	ApprovalPlan         ApprovalKind = "plan"
	ApprovalExecution    ApprovalKind = "execution"
	ApprovalCommitPolicy ApprovalKind = "commit-policy"
	// ApprovalApplyCatalog is the append-only APPLY_CATALOG approval of a
	// Target Drift that changed the Wrapper/Manifest/Executable identity
	// (PRD 已确认：Apply Command Identity Drift): it binds the exact Apply
	// Attempt, the Target/Integration HEADs, and the newly discovered,
	// validated, and fixed Apply Verification Catalog Revision.
	ApprovalApplyCatalog ApprovalKind = "apply-catalog"
)

// Valid reports whether k is a declared Approval Kind.
func (k ApprovalKind) Valid() bool {
	switch k {
	case ApprovalPlan, ApprovalExecution, ApprovalCommitPolicy, ApprovalApplyCatalog:
		return true
	}
	return false
}

// String renders the Approval Kind.
func (k ApprovalKind) String() string { return string(k) }

// Approval is one append-only user decision accepting one exact set of
// Artifact Revisions, routing, budgets, and commit-policy facts. It is
// never revoked and never generalised to other hashes (design 7, 15.6).
type Approval struct {
	ID          ApprovalID
	Kind        ApprovalKind
	Seq         uint64
	Refs        []ArtifactRef
	Fingerprint string
	// PreflightRevision is the exact Git Commit Preflight Revision the
	// approval bound (the git_commit_preflight_revision row column).
	// latestConfirmedCommitPolicy derives the confirmed policy from
	// approvals that bind the latest Preflight Revision (PRD 已确认：
	// Replacement Execution Approval 吸收 Policy 确认 item 4).
	PreflightRevision int
	// DecisionContext is the immutable JSON decision-context of the
	// approval (decision_context_json). A Replacement Execution Approval
	// records reason=COMMIT_POLICY_DRIFT_REPLACEMENT, the superseded
	// Execution Approval ID, the Quarantine ID set, the Reconciliation
	// Manifest Revision/Hash, and absorbs_commit_policy_confirmation=true
	// (PRD 已确认 item 2).
	DecisionContext string
}

// ExecutionFacts are the immutable inputs an Execution Approval binds by
// exact hash: the active Artifact revisions, routing, budgets, and the
// current Git Commit Preflight Revision/hash/fingerprint (design 20.1).
// Editing configuration never silently mutates an approved Workflow; it
// requires a successor revision with a new Execution Approval.
type ExecutionFacts struct {
	PlanHash         string
	SpecHashes       []string
	CatalogHash      string
	WorkflowHash     string
	RoutingHash      string
	BudgetHash       string
	CommitPolicyHash string
	Fingerprint      string
	// SpecRevision/CatalogRevision/WorkflowRevision are the active
	// revisions of the execution Artifacts (the Approval row binds
	// revisions and hashes together).
	SpecRevision     int
	CatalogRevision  int
	WorkflowRevision int
	// PreflightRevision is the revision of the latest recorded Commit
	// Preflight row (0 when none exists yet).
	PreflightRevision int
}

// Matches reports whether the candidate Execution Approval input binds the
// exact same facts as the active ExecutionFacts.
func (f *ExecutionFacts) Matches(planHash string, specHashes []string, catalogHash, workflowHash, routingHash, budgetHash, commitPolicyHash string) bool {
	if f == nil {
		return false
	}
	if f.PlanHash != planHash || f.CatalogHash != catalogHash || f.WorkflowHash != workflowHash ||
		f.RoutingHash != routingHash || f.BudgetHash != budgetHash || f.CommitPolicyHash != commitPolicyHash {
		return false
	}
	if len(f.SpecHashes) != len(specHashes) {
		return false
	}
	for i := range f.SpecHashes {
		if f.SpecHashes[i] != specHashes[i] {
			return false
		}
	}
	return true
}
