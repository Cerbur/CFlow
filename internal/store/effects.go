package store

// Event payload and Effect Intent codecs (design 6.2, 9.2): the closed
// union of Effect Intents round-trips through the effects table with a
// stable kind discriminator; Event payloads carry only redacted, bounded
// data and immutable references.

import (
	"encoding/json"
	"fmt"

	"cflow.local/cflow/internal/model"
)

// ---------------------------------------------------------------------------
// codecs
// ---------------------------------------------------------------------------

// approvalGateType maps the model Approval Kind to the PRD approvals
// gate_type value (PRD 核心数据库表: PLAN/EXECUTION/COMMIT_POLICY).
func approvalGateType(kind model.ApprovalKind) string {
	switch kind {
	case model.ApprovalPlan:
		return "PLAN"
	case model.ApprovalExecution:
		return "EXECUTION"
	case model.ApprovalCommitPolicy:
		return "COMMIT_POLICY"
	}
	return string(kind)
}

// gateToApprovalKind maps a PRD approvals gate_type back to the model
// Approval Kind. Unknown future gates pass through unchanged.
func gateToApprovalKind(gate string) model.ApprovalKind {
	switch gate {
	case "PLAN":
		return model.ApprovalPlan
	case "EXECUTION":
		return model.ApprovalExecution
	case "COMMIT_POLICY":
		return model.ApprovalCommitPolicy
	}
	return model.ApprovalKind(gate)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func intOrNil(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// marshalEvidence encodes the immutable EvidenceRef list of one Attempt.
func marshalEvidence(ev []model.EvidenceRef) string {
	if len(ev) == 0 {
		return "[]"
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return "[]"
	}
	return string(body)
}

// marshalKeys encodes a Quiesce Snapshot of Attempt keys.
func marshalKeys(keys []model.AttemptKey) string {
	if len(keys) == 0 {
		return "[]"
	}
	body, err := json.Marshal(keys)
	if err != nil {
		return "[]"
	}
	return string(body)
}

// effectKindOf maps one Effect Intent of the closed union to its stable
// persisted kind.
func effectKindOf(i model.EffectIntent) (string, error) {
	switch i.(type) {
	case model.ArtifactWriteIntent:
		return "artifact-write", nil
	case model.ProviderStartIntent:
		return "provider-start", nil
	case model.ProviderResumeIntent:
		return "provider-resume", nil
	case model.NativeBootstrapIntent:
		return "native-bootstrap", nil
	case model.ProviderCancelIntent:
		return "provider-cancel", nil
	case model.PlanningWorktreeCreateIntent:
		return "planning-worktree-create", nil
	case model.WorkspaceWorktreeCreateIntent:
		return "workspace-worktree-create", nil
	case model.IntegrationWorktreeCreateIntent:
		return "integration-worktree-create", nil
	case model.TaskWorktreeCreateIntent:
		return "task-worktree-create", nil
	case model.GitCommitInspectIntent:
		return "git-commit-inspect", nil
	case model.GitAuditRefCreateIntent:
		return "git-audit-ref-create", nil
	case model.IntegrationMergeIntent:
		return "integration-merge", nil
	case model.IntegrationRollbackIntent:
		return "integration-rollback", nil
	case model.WorkspaceMergeIntent:
		return "workspace-merge", nil
	case model.WorkspaceRollbackIntent:
		return "workspace-rollback", nil
	case model.LayoutMigrationIntent:
		return "layout-migration", nil
	case model.VerificationRunIntent:
		return "verification-run", nil
	case model.WorkflowCompileIntent:
		return "workflow-compile", nil
	case model.ApplyStagingCreateIntent:
		return "apply-staging-create", nil
	case model.ApplyFastForwardIntent:
		return "apply-fast-forward", nil
	case model.ManagedProcessStopIntent:
		return "managed-process-stop", nil
	case model.CleanupWorktreeRemoveIntent:
		return "cleanup-worktree-remove", nil
	case model.CleanupScratchRemoveIntent:
		return "cleanup-scratch-remove", nil
	}
	return "", fmt.Errorf("unknown effect intent %T", i)
}

// effectFromKind decodes a persisted Effect Intent of the closed union.
func effectFromKind(kind string, payload []byte) (model.EffectIntent, error) {
	var err error
	switch kind {
	case "artifact-write":
		var v model.ArtifactWriteIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "provider-start":
		var v model.ProviderStartIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "provider-resume":
		var v model.ProviderResumeIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "native-bootstrap":
		var v model.NativeBootstrapIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "provider-cancel":
		var v model.ProviderCancelIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "planning-worktree-create":
		var v model.PlanningWorktreeCreateIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "workspace-worktree-create":
		var v model.WorkspaceWorktreeCreateIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "integration-worktree-create":
		var v model.IntegrationWorktreeCreateIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "task-worktree-create":
		var v model.TaskWorktreeCreateIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "git-commit-inspect":
		var v model.GitCommitInspectIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "git-audit-ref-create":
		var v model.GitAuditRefCreateIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "integration-merge":
		var v model.IntegrationMergeIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "integration-rollback":
		var v model.IntegrationRollbackIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "workspace-merge":
		var v model.WorkspaceMergeIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "workspace-rollback":
		var v model.WorkspaceRollbackIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "layout-migration":
		var v model.LayoutMigrationIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "verification-run":
		var v model.VerificationRunIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "workflow-compile":
		var v model.WorkflowCompileIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "apply-staging-create":
		var v model.ApplyStagingCreateIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "apply-fast-forward":
		var v model.ApplyFastForwardIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "managed-process-stop":
		var v model.ManagedProcessStopIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "cleanup-worktree-remove":
		var v model.CleanupWorktreeRemoveIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	case "cleanup-scratch-remove":
		var v model.CleanupScratchRemoveIntent
		err = json.Unmarshal(payload, &v)
		return v, err
	}
	return nil, fmt.Errorf("unknown effect kind %q", kind)
}
