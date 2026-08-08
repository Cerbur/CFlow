package store

import (
	"errors"
	"testing"

	"cflow.local/cflow/internal/model"
)

// workspaceFixtureDecision creates the fixture Workflow with Layout Version 2
// workspace facts, mirroring the Kernel create decision for a native-
// discussion Workflow (design 7/8, Task 3 step 1).
func workspaceFixtureDecision(state model.State) (model.Decision, error) {
	if state.Workflow.ID != "" {
		return model.Decision{}, errors.New("workflow already exists")
	}
	return model.Decision{
		Mutations: []model.Mutation{model.WorkflowMutation{
			ID: "wf-1", Project: fixtureProjectID,
			Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePending,
			TargetBranch: "main", BaseCommit: "base-1",
			LayoutVersion:             2,
			WorkspacePath:             "/cflow/projects/p/wf-1/workspace",
			WorkspaceBranch:           "cflow/wf-1/workspace",
			CandidateWorkspaceHead:    "c2",
			VerifiedWorkspaceHead:     "c1",
			WorkspaceDirtyFingerprint: "sha256:abc",
		}},
		Events: []model.Event{{
			Seq: state.NextEventSeq, Kind: model.EventWorkflowCreated,
			Workflow: "wf-1", Text: "workflow created", At: state.Now,
		}},
	}, nil
}

// TestWorkspaceLayoutRoundTrip: the Layout Version 2 workspace facts are
// persisted authoritatively and survive a full transact/reopen/View cycle
// byte-for-byte (plan Task 3 step 1).
func TestWorkspaceLayoutRoundTrip(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, workspaceFixtureDecision)
	s.Close()

	// Reopen the same database file and rebuild the aggregate from the
	// database alone.
	re := openTestStoreAt(t, s.path)
	view := mustView(t, re)
	wf := view.State.Workflow
	if wf.LayoutVersion != 2 {
		t.Fatalf("layout version = %d, want 2", wf.LayoutVersion)
	}
	if wf.WorkspacePath != "/cflow/projects/p/wf-1/workspace" {
		t.Fatalf("workspace path = %q", wf.WorkspacePath)
	}
	if wf.WorkspaceBranch != "cflow/wf-1/workspace" {
		t.Fatalf("workspace branch = %q", wf.WorkspaceBranch)
	}
	if wf.CandidateWorkspaceHead != "c2" || wf.VerifiedWorkspaceHead != "c1" {
		t.Fatalf("workspace heads = c2/%s", wf.CandidateWorkspaceHead)
	}
	if wf.WorkspaceDirtyFingerprint != "sha256:abc" {
		t.Fatalf("workspace dirty fingerprint = %q", wf.WorkspaceDirtyFingerprint)
	}
}

// TestWorkspaceLayoutFactsSurviveMutation: a later Workflow Mutation (e.g.
// a stage transition) preserves the Layout Version 2 workspace facts,
// because the Kernel passes them through wholesale via wfMut. A stale
// mutation that drops them would wipe the row (spec review Important
// finding: the mutation replaces the row wholesale).
func TestWorkspaceLayoutFactsSurviveMutation(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, workspaceFixtureDecision)

	// A follow-up mutation, as the Kernel emits it: identical to
	// workspaceFixtureDecision except the Runtime Status advances.
	followUp := func(state model.State) (model.Decision, error) {
		return model.Decision{
			Mutations: []model.Mutation{model.WorkflowMutation{
				ID:                        state.Workflow.ID,
				Project:                   state.Workflow.Project,
				Stage:                     model.StageRequirementDiscussion,
				Runtime:                   model.RuntimeRunning,
				TargetBranch:              state.Workflow.TargetBranch,
				BaseCommit:                state.Workflow.BaseCommit,
				IntegrationBranch:         state.Workflow.IntegrationBranch,
				IntegrationHead:           state.Workflow.IntegrationHead,
				LayoutVersion:             state.Workflow.LayoutVersion,
				WorkspacePath:             state.Workflow.WorkspacePath,
				WorkspaceBranch:           state.Workflow.WorkspaceBranch,
				CandidateWorkspaceHead:    state.Workflow.CandidateWorkspaceHead,
				VerifiedWorkspaceHead:     state.Workflow.VerifiedWorkspaceHead,
				WorkspaceDirtyFingerprint: state.Workflow.WorkspaceDirtyFingerprint,
			}},
			Events: []model.Event{{
				Seq:  state.NextEventSeq + 1,
				Kind: model.EventStageChanged, Workflow: state.Workflow.ID,
				Text: "runtime change", At: state.Now,
			}},
		}, nil
	}
	mustTransact(t, s, 1, followUp)

	wf := mustView(t, s).State.Workflow
	if wf.Runtime != model.RuntimeRunning {
		t.Fatalf("runtime = %s, want %s", wf.Runtime, model.RuntimeRunning)
	}
	if wf.LayoutVersion != 2 {
		t.Fatalf("layout version = %d, want 2 after mutation", wf.LayoutVersion)
	}
	if wf.WorkspacePath != "/cflow/projects/p/wf-1/workspace" {
		t.Fatalf("workspace path = %q after mutation", wf.WorkspacePath)
	}
	if wf.WorkspaceBranch != "cflow/wf-1/workspace" {
		t.Fatalf("workspace branch = %q after mutation", wf.WorkspaceBranch)
	}
	if wf.CandidateWorkspaceHead != "c2" || wf.VerifiedWorkspaceHead != "c1" {
		t.Fatalf("workspace heads = c2/%s after mutation", wf.CandidateWorkspaceHead)
	}
	if wf.WorkspaceDirtyFingerprint != "sha256:abc" {
		t.Fatalf("workspace dirty fingerprint = %q after mutation", wf.WorkspaceDirtyFingerprint)
	}
}

// TestWorkspaceLayoutCreateWithZeroVersionDefaultsTo1: a create mutation
// that predates workspace wiring carries LayoutVersion 0; the store must
// never persist a tri-state, and normalizes it to the design default 1
// (code-quality review Important finding: the column is NOT NULL and
// COALESCE(layout_version,1) cannot rescue a literal 0).
func TestWorkspaceLayoutCreateWithZeroVersionDefaultsTo1(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, func(state model.State) (model.Decision, error) {
		return model.Decision{
			Mutations: []model.Mutation{model.WorkflowMutation{
				ID: "wf-1", Project: fixtureProjectID,
				Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePending,
				TargetBranch: "main", BaseCommit: "base-1",
			}},
			Events: []model.Event{{
				Seq: state.NextEventSeq, Kind: model.EventWorkflowCreated,
				Workflow: "wf-1", Text: "workflow created", At: state.Now,
			}},
		}, nil
	})

	wf := mustView(t, s).State.Workflow
	if wf.LayoutVersion != 1 {
		t.Fatalf("layout version = %d, want default 1, not 0", wf.LayoutVersion)
	}

	s.Close()
	re := openTestStoreAt(t, s.path)
	if got := mustView(t, re).State.Workflow.LayoutVersion; got != 1 {
		t.Fatalf("layout version after reopen = %d, want 1", got)
	}
}
