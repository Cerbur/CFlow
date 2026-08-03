package decision

// The planning lifecycle Result Decisions (Task 10): one Provider run
// ends (ProviderRunEnded), the Kernel validates its output, requests the
// immutable Artifact write, and the ArtifactWritten Result commits the
// authoritative lifecycle transition. Agent prose never finalizes
// anything: the Kernel judges the validated, redacted facts and only the
// user's append-only Approval can move a Plan past CHECKED (design 7.3
// invariants 1 and 2).
//
// Same-package split of the decision package: no public seam added.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/internal/model"
)

// Bounds for planning output (the Artifact Store enforces its own file
// bound; the Kernel's bounds keep one Decision's judging cheap and
// deterministic).
const (
	maxTurnText = 64 * 1024
	maxPlanBody = 1 << 20
	// maxCheckGaps bounds the structured check result's gap lists.
	maxCheckGaps = 32
	maxGapText   = 4096
)

// planRequiredSections is the PRD's authoritative Plan section list (PRD
// Plan 文件格式). A Plan output that misses any section is not a Plan.
var planRequiredSections = []string{
	"背景", "目标", "范围", "非目标", "约束", "当前实现分析",
	"推荐技术方案", "关键设计决策", "涉及模块与文件边界",
	"数据与兼容性影响", "测试与验收方案", "风险与回滚", "未决问题",
}

// ---------------------------------------------------------------------------
// ProviderRunEnded: the run settled, the Kernel judges its output
// ---------------------------------------------------------------------------

// decideProviderRunEnded dispatches one settled Provider run by its role
// lineage and the Workflow stage. A failed or cancelled run settles the
// Session with a failure Finding and leaves the Workflow where it is for
// the user to retry; a completed run moves to the flow-specific output
// validation.
func decideProviderRunEnded(state model.State, in model.EffectResultInput) (model.Decision, error) {
	if in.Session.ID == "" || !in.Session.Purpose.Valid() {
		return model.Decision{}, model.InvalidInputFault("provider run result carries no valid session")
	}
	created := findSessionState(state, in.Session.ID)
	if created == nil {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("provider run result references unknown session %s", in.Session.ID))
	}
	if created.Purpose != in.Session.Purpose {
		return model.Decision{}, model.NewFault(model.CodeSessionIndependenceViolation,
			"provider run result changed the session purpose")
	}

	if in.Session.Status == model.SessionCompleted {
		switch created.Purpose {
		case model.PurposePlanning:
			switch state.Workflow.Stage {
			case model.StageRequirementDiscussion:
				return decideDiscussionTurnResult(state, in, created)
			case model.StagePlanGeneration:
				return decidePlanGenerated(state, in, created)
			}
		case model.PurposePlanCheck:
			if state.Workflow.Stage == model.StagePlanCheck {
				return decideCheckResult(state, in, created)
			}
		case model.PurposeSpecGeneration:
			switch state.Workflow.Stage {
			case model.StageSpecGeneration, model.StageWorkflowGeneration:
				return decideSpecsGenerated(state, in, created)
			}
		case model.PurposeWorkflowOptimization:
			if state.Workflow.Stage == model.StageWorkflowGeneration {
				return decidePatchProposed(state, in, created)
			}
		case model.PurposeImplementation, model.PurposeRepair:
			// A settled coding or Repair Session records its Session facts
			// only; the Attempt's outcome is judged from Git evidence by
			// the Commit gate (Task 13), never from the Agent's output
			// (design 7.3 invariant 1; PRD 已确认：DIRTY_TASK_WORKTREE 原地
			// Repair).
			if state.Workflow.Stage == model.StageExecution {
				return decideImplementationRunEnded(state, in, created)
			}
		case model.PurposeReview:
			// The independent Reviewer Session's verdict is evidence; the
			// Kernel judges it (design 16.2: review never replaces
			// deterministic verification).
			if state.Workflow.Stage == model.StageExecution {
				return decideReviewRunEnded(state, in, created)
			}
		}
		return model.Decision{}, model.InvariantFault(fmt.Errorf(
			"provider run completed in an unexpected stage %s for purpose %s",
			state.Workflow.Stage, created.Purpose))
	}
	if !in.Session.Status.IsTerminal() {
		return model.Decision{}, model.InvariantFault(fmt.Errorf(
			"provider run result carries non-terminal session status %s", in.Session.Status))
	}

	// A failed or cancelled run: for a coding or review Session inside
	// the EXECUTION stage, the RUNNING Attempt settles failed with the
	// compiled failure code (an interrupted run never charges the Retry
	// Budget, PRD 失败分类). Planning Sessions settle with the failure
	// code and a Finding; the Workflow stays where it is. A failed Check
	// also restores the Plan to DRAFT review: the Checker produced no
	// judgment, so the Plan cannot stay CHECKING (the user retries the
	// independent Check, mirroring the invalid-output path).
	code := in.FailureCode
	if code == "" {
		code = model.CodeAgentProcessCrashed
	}
	switch created.Purpose {
	case model.PurposeImplementation, model.PurposeReview, model.PurposeRepair:
		if state.Workflow.Stage == model.StageExecution {
			if attempt := attemptBySession(state, created.ID); attempt != nil {
				node := state.Nodes[attempt.Key.Node]
				if node == nil {
					return model.Decision{}, model.InvariantFault(fmt.Errorf("attempt %s has no node", attempt.Key))
				}
				b := &builder{state: state}
				b.mutate(sessionEnd(state, created, in))
				if code == model.CodeUserInterrupted {
					// A user Ctrl+C interruption is never a Provider
					// failure and never charges the Retry Budget (PRD
					// 失败分类, USER_INTERRUPTED): the Attempt settles
					// INTERRUPTED carrying the interruption code and no
					// successor is allocated.
					return settleInterrupted(state, node, attempt, model.EffectResultInput{
						Kind:        model.AttemptEnded,
						Attempt:     attempt.Key,
						Outcome:     model.OutcomeInterrupted,
						FailureCode: model.CodeUserInterrupted,
						EndHead:     in.EndHead,
					}, b)
				}
				if err := appendFallbackSuccessor(b, state, created, in); err != nil {
					return model.Decision{}, err
				}
				return decideAttemptFailure(state, node, attempt, model.EffectResultInput{
					Kind:             model.AttemptEnded,
					Attempt:          attempt.Key,
					Outcome:          model.OutcomeFailed,
					FailureCode:      code,
					EndHead:          in.EndHead,
					SuccessorSession: in.SuccessorSession,
				}, code, b)
			}
		}
	}
	b := &builder{state: state}
	if created.Purpose == model.PurposePlanCheck && state.Plan != nil &&
		state.Plan.Status == model.PlanChecking {
		b.mutate(model.PlanMutation{Status: model.PlanDraft})
	}
	b.mutate(sessionEnd(state, created, in))
	b.mutate(model.FindingAppendMutation{Finding: model.Finding{
		ID:       model.FindingID(fmt.Sprintf("finding-%d", len(state.Findings)+1)),
		Code:     code,
		Scope:    model.ScopeSession,
		Subject:  string(created.ID),
		Blocking: false,
		Text:     code.String(),
		Seq:      state.NextEventSeq,
	}})
	b.event(model.EventFindingOpened, "", model.AttemptKey{}, code, "provider run failed")
	return b.decision(), nil
}

// appendFallbackSuccessor persists the automatic fallback successor
// Session in the same settle Decision (design 14.4): the row carries
// supersedes_session_id and the fallback Provider, and the successor
// Attempt references it — the "one successor per Lost original" lineage
// is durable in the Store, never only in the pass's Runtime ledger. The
// Runtime allocated the successor Session; the Kernel validates the
// facts and writes the row. A result without a successor (no fallback)
// is unchanged.
func appendFallbackSuccessor(b *builder, state model.State, created *model.Session, in model.EffectResultInput) error {
	s := in.SuccessorSession
	if s.ID == "" {
		return nil
	}
	if s.Purpose != created.Purpose {
		return model.NewFault(model.CodeSessionIndependenceViolation,
			"the fallback successor must keep the lost session's purpose")
	}
	if s.Supersedes != created.ID {
		return model.InvariantFault(fmt.Errorf(
			"fallback successor %s must supersede the lost session %s", s.ID, created.ID))
	}
	if s.Provider == "" {
		return model.InvalidInputFault("fallback successor carries no provider")
	}
	if s.Status != model.SessionStarting {
		return model.InvalidInputFault("fallback successor must be allocated STARTING")
	}
	b.mutate(model.SessionAppendMutation{Session: s, Provider: s.Provider})
	return nil
}

// decideDiscussionTurnResult validates the CFlow-assembled turn body and
// requests the immutable turn Artifact write. The turn is linked to its
// Session lineage through the artifact Producer.
func decideDiscussionTurnResult(state model.State, in model.EffectResultInput, created *model.Session) (model.Decision, error) {
	if len(in.Body) == 0 || len(in.Body) > maxTurnText || !json.Valid(in.Body) {
		return invalidOutput(state, in, created, "requirement turn output is not a bounded JSON body", false)
	}
	b := &builder{state: state}
	b.mutate(sessionEnd(state, created, in))
	b.effect(model.ArtifactWriteIntent{
		Ref:      model.ArtifactRef{Workflow: state.Workflow.ID, Type: model.ArtifactDiscussionTurn},
		Body:     in.Body,
		Producer: model.PurposePlanning,
		Session:  created.ID,
	})
	return b.decision(), nil
}

// decidePlanGenerated validates the Plan output against the PRD's
// required sections, assembles the immutable Plan body (front matter plus
// the agent's Markdown), and requests the write. An invalid output is
// rejected: the Session settles, a schema Finding is recorded, and no
// Plan Revision is created.
func decidePlanGenerated(state model.State, in model.EffectResultInput, created *model.Session) (model.Decision, error) {
	title, err := validatePlanMarkdown(in.Body)
	if err != nil {
		return invalidOutput(state, in, created, err.Error(), false)
	}
	revision := 1
	if state.Plan != nil {
		revision = state.Plan.Revision + 1
	}
	body, err := assemblePlanBody(state, created, revision, title, in.Body)
	if err != nil {
		return invalidOutput(state, in, created, err.Error(), false)
	}
	b := &builder{state: state}
	b.mutate(sessionEnd(state, created, in))
	b.effect(model.ArtifactWriteIntent{
		Ref:      model.ArtifactRef{Workflow: state.Workflow.ID, Type: model.ArtifactPlan, Revision: revision},
		Body:     body,
		Producer: model.PurposePlanning,
		Session:  created.ID,
	})
	return b.decision(), nil
}

// decideCheckResult validates the Checker's structured result and
// requests the immutable Check Artifact write. Session independence is
// re-verified from the settled facts: the Checker's Provider Session ID
// must never reuse any existing Session's (design 14.4).
func decideCheckResult(state model.State, in model.EffectResultInput, created *model.Session) (model.Decision, error) {
	for _, s := range state.Sessions {
		if s.ID != created.ID && s.ProviderSessionID != "" && s.ProviderSessionID == in.Session.ProviderSessionID {
			return model.Decision{}, model.NewFault(model.CodeSessionIndependenceViolation,
				"the checker session reuses an existing session's provider session id")
		}
	}
	outcome, err := parseCheckResult(in.Body)
	if err != nil {
		return invalidOutput(state, in, created, err.Error(), true)
	}
	body, err := assembleCheckBody(state, created, in, outcome)
	if err != nil {
		return invalidOutput(state, in, created, err.Error(), true)
	}
	b := &builder{state: state}
	b.mutate(sessionEnd(state, created, in))
	b.effect(model.ArtifactWriteIntent{
		Ref:      model.ArtifactRef{Workflow: state.Workflow.ID, Type: model.ArtifactPlanCheck},
		Body:     body,
		Producer: model.PurposePlanCheck,
		Session:  created.ID,
	})
	return b.decision(), nil
}

// invalidOutput settles the Session and records the schema Finding for a
// Provider output the Kernel cannot accept. The command succeeds with the
// Finding; nothing is written and the Workflow stays where it is.
// restorePlan returns the Plan from CHECKING to DRAFT (only a failed
// Check changes the Plan status before the output is judged).
func invalidOutput(state model.State, in model.EffectResultInput, created *model.Session, text string, restorePlan bool) (model.Decision, error) {
	b := &builder{state: state}
	b.mutate(sessionEnd(state, created, in))
	if restorePlan {
		b.mutate(model.PlanMutation{Status: model.PlanDraft})
	}
	b.mutate(model.FindingAppendMutation{Finding: model.Finding{
		ID:       model.FindingID(fmt.Sprintf("finding-%d", len(state.Findings)+1)),
		Code:     model.CodeSchemaInvalid,
		Scope:    model.ScopeArtifact,
		Subject:  string(created.ID),
		Blocking: false,
		Text:     text,
		Seq:      state.NextEventSeq,
	}})
	b.event(model.EventFindingOpened, "", model.AttemptKey{}, model.CodeSchemaInvalid, text)
	return b.decision(), nil
}

// sessionEnd settles the created Session record with the run's Provider
// facts and the Provider route it ran on.
func sessionEnd(state model.State, created *model.Session, in model.EffectResultInput) model.SessionEndMutation {
	return model.SessionEndMutation{
		ID:                created.ID,
		ProviderSessionID: in.Session.ProviderSessionID,
		Status:            in.Session.Status,
		EndedAt:           state.Now,
	}
}

// ---------------------------------------------------------------------------
// ArtifactWritten: the immutable body exists; commit the transition
// ---------------------------------------------------------------------------

// decideArtifactWritten records the written Artifact Revision and commits
// the lifecycle transition it gates. The Plan Check branch re-judges the
// echoed Check body: the written content is the immutable evidence the
// transition binds.
func decideArtifactWritten(state model.State, in model.EffectResultInput) (model.Decision, error) {
	ref := in.Artifact
	if ref.Workflow != state.Workflow.ID || !ref.Type.Valid() || ref.Revision < 1 || ref.Hash == "" {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("artifact write result carries an invalid reference %+v", ref))
	}
	switch ref.Type {
	case model.ArtifactPlan:
		return planRevisionRecorded(state, ref)
	case model.ArtifactDiscussionTurn:
		// The turn Artifact's identity is the Producer linkage to its
		// Session lineage; no aggregate row records it.
		return model.Decision{}, nil
	case model.ArtifactPlanCheck:
		return checkResultCommitted(state, in)
	case model.ArtifactSpec:
		return specRevisionRecorded(state, ref)
	case model.ArtifactWorkflow:
		return workflowRevisionRecorded(state, ref)
	default:
		return model.Decision{}, model.InvariantFault(fmt.Errorf("artifact write result for an unexpected type %s", ref.Type))
	}
}

// specRevisionRecorded records the Spec Revision and moves the Workflow
// to WORKFLOW_GENERATION (PRD 主状态转换: SPEC_GENERATION -> WORKFLOW_GENERATION).
func specRevisionRecorded(state model.State, ref model.ArtifactRef) (model.Decision, error) {
	b := &builder{state: state}
	b.mutate(model.ArtifactRefMutation{
		Type: ref.Type, Revision: ref.Revision, Path: artifactRefPath(ref), Hash: ref.Hash,
	})
	if state.Workflow.Stage != model.StageWorkflowGeneration {
		b.mutate(wfMut(state, model.StageWorkflowGeneration, state.Workflow.Runtime, state.Workflow.CancelIntent))
		b.event(model.EventStageChanged, "", model.AttemptKey{}, "", "stage changed to WORKFLOW_GENERATION")
	}
	return b.decision(), nil
}

// workflowRevisionRecorded records the compiled Dynamic Workflow
// Revision. The Workflow stays at WORKFLOW_GENERATION until the user's
// Execution Dry Run pauses the gate.
func workflowRevisionRecorded(state model.State, ref model.ArtifactRef) (model.Decision, error) {
	b := &builder{state: state}
	b.mutate(model.ArtifactRefMutation{
		Type: ref.Type, Revision: ref.Revision, Path: artifactRefPath(ref), Hash: ref.Hash,
	})
	return b.decision(), nil
}

// ---------------------------------------------------------------------------
// Spec generation output and the Workflow Optimization Patch (Task 11)
// ---------------------------------------------------------------------------

// maxSpecOutput bounds one Spec Generation output document.
const maxSpecOutput = 1 << 20

// specOutput is the structured Spec Generation Session output: the
// `specs` list (each body satisfies spec.json) plus the proposed
// commands the Runtime validates into a successor Catalog Revision.
type specOutput struct {
	Specs            []map[string]any `yaml:"specs"`
	ProposedCommands []map[string]any `yaml:"proposed_commands"`
}

// decideSpecsGenerated judges the Spec Generation output. The pipeline
// binds one active Spec Revision whose body carries the Spec set: one
// Spec object or a non-empty sequence of Spec objects (the multi-Spec
// pipeline the Scheduler consumes, Task 12; the Compiler absorbs N
// Specs). Every Spec is validated individually and the set is
// re-serialized canonically. The Runtime already validated the proposed
// commands into the Catalog reference the Result carries; the Kernel
// records that revision and requests the immutable Spec write.
func decideSpecsGenerated(state model.State, in model.EffectResultInput, created *model.Session) (model.Decision, error) {
	if len(in.Body) == 0 || len(in.Body) > maxSpecOutput {
		return invalidOutput(state, in, created, "spec generation output is empty or exceeds the bounded size", false)
	}
	var out specOutput
	if err := yaml.Unmarshal(in.Body, &out); err != nil {
		return invalidOutput(state, in, created, "spec generation output is not a structured document", false)
	}
	if len(out.Specs) == 0 {
		return invalidOutput(state, in, created,
			"spec generation output must carry at least one spec", false)
	}
	for _, m := range out.Specs {
		if _, err := validateSpecOutput(m); err != nil {
			return invalidOutput(state, in, created, err.Error(), false)
		}
	}
	specBody, err := yaml.Marshal(out.Specs)
	if err != nil {
		return invalidOutput(state, in, created, "spec output cannot be serialized", false)
	}
	b := &builder{state: state}
	if in.CatalogRef.Type == model.ArtifactCatalog && in.CatalogRef.Revision >= 1 && in.CatalogRef.Hash != "" {
		b.mutate(model.ArtifactRefMutation{
			Type: in.CatalogRef.Type, Revision: in.CatalogRef.Revision,
			Path: artifactRefPath(in.CatalogRef), Hash: in.CatalogRef.Hash,
		})
	}
	b.mutate(sessionEnd(state, created, in))
	b.effect(model.ArtifactWriteIntent{
		Ref:      model.ArtifactRef{Workflow: state.Workflow.ID, Type: model.ArtifactSpec},
		Body:     specBody,
		Producer: model.PurposeSpecGeneration,
		Session:  created.ID,
	})
	return b.decision(), nil
}

// specAllowedKeys is the closed set of Spec body keys (spec.json has
// additionalProperties: false; the Kernel's light structural check keeps
// free argv and unknown fields out of the artifact before the strict
// schema validation on write).
var specAllowedKeys = map[string]bool{
	"id": true, "goal": true, "depends_on": true, "write_scope": true,
	"read_scope": true, "locks": true, "acceptance": true, "route": true,
	"timeout_seconds": true, "max_retry": true,
}

// validateSpecOutput checks one Spec body structurally and returns its
// canonical serialization (yaml.v3 sorts map keys, so the re-serialized
// body is deterministic).
func validateSpecOutput(m map[string]any) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("spec output carries an empty spec")
	}
	for key := range m {
		if !specAllowedKeys[key] {
			return nil, fmt.Errorf("spec output carries the disallowed field %q", key)
		}
	}
	for _, required := range []string{"id", "goal", "depends_on", "write_scope", "acceptance"} {
		if _, ok := m[required]; !ok {
			return nil, fmt.Errorf("spec output is missing the required field %q", required)
		}
	}
	if id, ok := m["id"].(string); !ok || id == "" {
		return nil, fmt.Errorf("spec id must be a non-empty string")
	}
	if goal, ok := m["goal"].(string); !ok || goal == "" {
		return nil, fmt.Errorf("spec goal must be a non-empty string")
	}
	if deps, ok := m["depends_on"].([]any); !ok {
		return nil, fmt.Errorf("spec depends_on must be a list")
	} else {
		for _, d := range deps {
			if _, ok := d.(string); !ok {
				return nil, fmt.Errorf("spec depends_on entries must be strings")
			}
		}
	}
	if scope, ok := m["write_scope"].([]any); !ok || len(scope) == 0 {
		return nil, fmt.Errorf("spec write_scope must be a non-empty list")
	}
	acc, ok := m["acceptance"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("spec acceptance must be an object")
	}
	cmds, ok := acc["verification_command_ids"].([]any)
	if !ok || len(cmds) == 0 {
		return nil, fmt.Errorf("spec acceptance must reference verification commands")
	}
	for _, c := range cmds {
		if _, ok := c.(string); !ok {
			return nil, fmt.Errorf("spec acceptance command ids must be strings")
		}
	}
	body, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("spec output cannot be serialized")
	}
	return body, nil
}

// decidePatchProposed validates the Workflow Optimization output as a
// restricted Patch IR (bounded, structured) and requests the Compilation
// Effect: the Compiler validates the Patch against the deterministic
// skeleton and returns the canonical Dynamic Workflow body.
func decidePatchProposed(state model.State, in model.EffectResultInput, created *model.Session) (model.Decision, error) {
	if len(in.Body) == 0 || len(in.Body) > maxPlanBody {
		return invalidOutput(state, in, created, "workflow optimization output is empty or exceeds the bounded size", false)
	}
	var patch struct {
		Schema     string `yaml:"schema"`
		Operations []any  `yaml:"operations"`
	}
	if err := yaml.Unmarshal(in.Body, &patch); err != nil {
		return invalidOutput(state, in, created, "workflow optimization output is not a structured patch", false)
	}
	if patch.Schema != "cflow-workflow-patch-1" || len(patch.Operations) == 0 {
		return invalidOutput(state, in, created, "workflow optimization output is not a restricted patch document", false)
	}
	b := &builder{state: state}
	b.mutate(sessionEnd(state, created, in))
	b.effect(model.WorkflowCompileIntent{PatchBody: in.Body})
	return b.decision(), nil
}

// decideWorkflowCompiled judges the compiled Dynamic Workflow body: the
// rejected (inert) Patch operations and the applied Patch operations
// both become non-blocking Compile Findings visible in Dry Run — the
// user at the Execution Approval gate sees exactly which scheduling
// operations were applied — and the canonical body is written as the
// immutable Workflow Revision.
func decideWorkflowCompiled(state model.State, in model.EffectResultInput) (model.Decision, error) {
	if len(in.Body) == 0 || len(in.Body) > maxPlanBody {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("compiled workflow body is empty or exceeds the bounded size"))
	}
	b := &builder{state: state}
	n := 0
	recordCompileFinding := func(code model.Code, text string) {
		n++
		b.mutate(model.FindingAppendMutation{Finding: model.Finding{
			ID:       model.FindingID(fmt.Sprintf("finding-%d", len(state.Findings)+n)),
			Code:     code,
			Scope:    model.ScopeWorkflowRevision,
			Subject:  string(state.Workflow.ID),
			Blocking: false,
			Text:     text,
			Seq:      state.NextEventSeq + uint64(n-1),
		}})
		b.event(model.EventFindingOpened, "", model.AttemptKey{}, code, text)
	}
	for _, op := range in.RejectedOps {
		recordCompileFinding(model.CodeWorkflowPatchForbidden, "rejected patch: "+op)
	}
	for _, op := range in.AppliedOps {
		recordCompileFinding(model.CodeWorkflowPatchApplied, "applied patch: "+op)
	}
	b.effect(model.ArtifactWriteIntent{
		Ref:      model.ArtifactRef{Workflow: state.Workflow.ID, Type: model.ArtifactWorkflow},
		Body:     in.Body,
		Producer: model.PurposeWorkflowOptimization,
	})
	return b.decision(), nil
}

// decideIntegrationWorktreeCreated records the Integration Worktree HEAD
// (the recorded Base Commit at approval) into the aggregate.
func decideIntegrationWorktreeCreated(state model.State, in model.EffectResultInput) (model.Decision, error) {
	if in.IntegrationHead == "" {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("integration worktree result carries no head"))
	}
	b := &builder{state: state}
	m := wfMut(state, state.Workflow.Stage, state.Workflow.Runtime, state.Workflow.CancelIntent)
	m.IntegrationHead = in.IntegrationHead
	b.mutate(m)
	return b.decision(), nil
}

// planRevisionRecorded moves the Workflow to PLAN_CHECK with the new
// immutable Plan Revision in DRAFT review.
func planRevisionRecorded(state model.State, ref model.ArtifactRef) (model.Decision, error) {
	b := &builder{state: state}
	b.mutate(model.ArtifactRefMutation{
		Type: ref.Type, Revision: ref.Revision, Path: artifactRefPath(ref), Hash: ref.Hash,
	})
	b.mutate(model.PlanMutation{Status: model.PlanDraft})
	b.mutate(wfMut(state, model.StagePlanCheck, state.Workflow.Runtime, state.Workflow.CancelIntent))
	b.event(model.EventPlanGenerated, "", model.AttemptKey{}, "", "plan revision recorded")
	b.event(model.EventStageChanged, "", model.AttemptKey{}, "", "stage changed to PLAN_CHECK")
	return b.decision(), nil
}

// checkResultCommitted applies the Checker's decision (PRD Plan Check
// 交互): pass pauses the Workflow with the Plan CHECKED; needs_revision
// and needs_discussion return to their stages with the Plan back in
// DRAFT; reject pauses the Workflow with the Plan REJECTED. None of these
// is a user Approval.
func checkResultCommitted(state model.State, in model.EffectResultInput) (model.Decision, error) {
	outcome, err := parseCheckResult(in.Body)
	if err != nil {
		// The echoed body was validated before the write; a re-parse
		// failure is a build bug.
		return model.Decision{}, model.InvariantFault(fmt.Errorf("check artifact body cannot be re-judged: %w", err))
	}
	b := &builder{state: state}
	b.mutate(model.ArtifactRefMutation{
		Type: in.Artifact.Type, Revision: in.Artifact.Revision,
		Path: artifactRefPath(in.Artifact), Hash: in.Artifact.Hash,
	})
	switch checkDecision(outcome.Decision) {
	case checkPass:
		b.mutate(model.PlanMutation{Status: model.PlanChecked})
		b.mutate(wfMutStatus(state, model.RuntimePaused))
		b.event(model.EventPlanCheckPassed, "", model.AttemptKey{}, "", "plan check passed")
		b.event(model.EventWorkflowPaused, "", model.AttemptKey{}, "", "workflow paused for plan approval")
	case checkNeedsRevision:
		b.mutate(model.PlanMutation{Status: model.PlanDraft})
		b.mutate(wfMut(state, model.StagePlanGeneration, model.RuntimeRunning, state.Workflow.CancelIntent))
		b.event(model.EventPlanCheckNeedsRevision, "", model.AttemptKey{}, "", "plan check needs revision")
		b.event(model.EventStageChanged, "", model.AttemptKey{}, "", "stage changed to PLAN_GENERATION")
	case checkNeedsDiscussion:
		b.mutate(model.PlanMutation{Status: model.PlanDraft})
		b.mutate(wfMut(state, model.StageRequirementDiscussion, model.RuntimeRunning, state.Workflow.CancelIntent))
		b.event(model.EventPlanCheckNeedsDiscussion, "", model.AttemptKey{}, "", "plan check needs discussion")
		b.event(model.EventStageChanged, "", model.AttemptKey{}, "", "stage changed to REQUIREMENT_DISCUSSION")
	case checkReject:
		b.mutate(model.PlanMutation{Status: model.PlanRejected})
		b.mutate(wfMutStatus(state, model.RuntimePaused))
		b.event(model.EventPlanCheckRejected, "", model.AttemptKey{}, "", "plan check rejected")
		b.event(model.EventWorkflowPaused, "", model.AttemptKey{}, "", "workflow paused after plan rejection")
	default:
		return model.Decision{}, model.InvariantFault(fmt.Errorf("unexpected check decision %q", outcome.Decision))
	}
	return b.decision(), nil
}

// artifactRefPath is the canonical store-relative location of one
// Artifact Revision (the workflow_artifact_refs path column).
func artifactRefPath(ref model.ArtifactRef) string {
	return fmt.Sprintf("%s/%d/%s", ref.Type, ref.Revision, ref.Hash)
}

// ---------------------------------------------------------------------------
// Plan output validation and body assembly
// ---------------------------------------------------------------------------

// validatePlanMarkdown checks the PRD's required sections. The body is
// the agent's Markdown: a "# <title>" heading followed by every required
// "## <section>" heading. It returns the title.
func validatePlanMarkdown(body []byte) (string, error) {
	if len(body) == 0 || len(body) > maxPlanBody {
		return "", fmt.Errorf("plan output is empty or exceeds the bounded size")
	}
	if !utf8.Valid(body) {
		return "", fmt.Errorf("plan output is not valid UTF-8")
	}
	lines := strings.Split(string(body), "\n")
	title := ""
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "# ") {
			return "", fmt.Errorf("plan output must begin with a \"# <title>\" heading")
		}
		title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
		if title == "" {
			return "", fmt.Errorf("plan output title is empty")
		}
		break
	}
	present := map[string]bool{}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			present[strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))] = true
		}
	}
	for _, sec := range planRequiredSections {
		if !present[sec] {
			return "", fmt.Errorf("plan output is missing the required section %q", sec)
		}
	}
	return title, nil
}

// assemblePlanBody builds the immutable Plan body: the YAML front matter
// the plan-envelope schema binds, then the agent's Markdown.
func assemblePlanBody(state model.State, created *model.Session, revision int, title string, markdown []byte) ([]byte, error) {
	front, err := yaml.Marshal(map[string]any{
		"workflow_id":       string(state.Workflow.ID),
		"revision":          revision,
		"title":             title,
		"required_sections": planRequiredSections,
	})
	if err != nil {
		return nil, fmt.Errorf("plan front matter cannot be serialized")
	}
	body := append([]byte("---\n"), front...)
	body = append(body, "---\n\n"...)
	body = append(body, markdown...)
	return body, nil
}

// ---------------------------------------------------------------------------
// Check result parsing and Check Artifact body assembly
// ---------------------------------------------------------------------------

// checkDecision is the closed set of structured Checker decisions (PRD
// Plan Check 交互).
type checkDecision string

const (
	checkPass            checkDecision = "pass"
	checkNeedsDiscussion checkDecision = "needs_discussion"
	checkNeedsRevision   checkDecision = "needs_revision"
	checkReject          checkDecision = "reject"
)

// structuredCheckResult is the Checker's structured output. Every field
// is bounded; the Kernel validates the decision before anything is
// written.
type structuredCheckResult struct {
	Decision               string   `json:"decision"`
	Summary                string   `json:"summary"`
	BlockingGaps           []string `json:"blockingGaps"`
	NonBlockingSuggestions []string `json:"nonBlockingSuggestions"`
	Confidence             float64  `json:"confidence"`
}

// parseCheckResult validates the structured Checker output. The decision
// must be one of the closed set; gap lists and texts are bounded.
func parseCheckResult(body []byte) (structuredCheckResult, error) {
	var out structuredCheckResult
	if len(body) == 0 || len(body) > maxPlanBody {
		return out, fmt.Errorf("check output is empty or exceeds the bounded size")
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("check output is not a structured JSON result")
	}
	switch checkDecision(out.Decision) {
	case checkPass, checkNeedsDiscussion, checkNeedsRevision, checkReject:
	default:
		return out, fmt.Errorf("check output carries an unknown decision %q", out.Decision)
	}
	if len(out.Summary) > maxGapText || len(out.BlockingGaps) > maxCheckGaps || len(out.NonBlockingSuggestions) > maxCheckGaps {
		return out, fmt.Errorf("check output exceeds the bounded fields")
	}
	for _, gap := range out.BlockingGaps {
		if len(gap) > maxGapText {
			return out, fmt.Errorf("check output gap exceeds the bounded size")
		}
	}
	for _, s := range out.NonBlockingSuggestions {
		if len(s) > maxGapText {
			return out, fmt.Errorf("check output suggestion exceeds the bounded size")
		}
	}
	if out.Confidence < 0 || out.Confidence > 1 {
		return out, fmt.Errorf("check output confidence is outside [0, 1]")
	}
	return out, nil
}

// assembleCheckBody builds the immutable Check Artifact body (PRD Plan
// Check 交互): the Plan Revision/Hash it judged, the decision, the
// Checker's Session and route, and the blocking gaps.
func assembleCheckBody(state model.State, created *model.Session, in model.EffectResultInput, outcome structuredCheckResult) ([]byte, error) {
	return json.Marshal(map[string]any{
		"schema_version":     1,
		"plan_revision":      state.Plan.Revision,
		"plan_sha256":        state.Plan.Hash,
		"decision":           outcome.Decision,
		"checker_provider":   in.Session.Provider,
		"checker_session_id": string(created.ID),
		"created_at":         state.Now.UTC().Format(time.RFC3339),
		"blocking_gaps":      outcome.BlockingGaps,
	})
}

func findSessionState(state model.State, id model.SessionID) *model.Session {
	for i := range state.Sessions {
		if state.Sessions[i].ID == id {
			return &state.Sessions[i]
		}
	}
	return nil
}
