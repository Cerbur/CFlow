// Package decision implements the pure Decision Kernel: legal transitions,
// approval comparison, failure classification, retry charge, dispatch
// gating, and the required Events and Effects (design 5, 8). It is pure:
// no function in this package imports database/sql, os, os/exec,
// path/filepath, or a CFlow infrastructure package, and the Kernel is a
// deterministic function of (State, Input).
package decision

import (
	"fmt"

	"cflow.local/cflow/internal/model"
)

// Decide is the pure transition dispatcher (design 6.2, 8.1). It rejects
// an invalid aggregate before any input-specific handling and routes the
// closed Input union to the decision functions by domain concern.
func Decide(state model.State, input model.Input) (model.Decision, error) {
	if err := model.ValidateState(state); err != nil {
		return model.Decision{}, model.InvariantFault(err)
	}
	switch in := input.(type) {
	case model.WorkflowCommandInput:
		return decideWorkflow(state, in)
	case model.EffectResultInput:
		return decideEffectResult(state, in)
	case model.ReconcileInput:
		return decideReconcile(state, in)
	case model.PlanApprovalInput:
		return decidePlanApproval(state, in)
	case model.ExecutionApprovalInput:
		return decideExecutionApproval(state, in)
	case model.AgentEventInput:
		return decideAgentEvent(state, in)
	case model.ApplyCommandInput:
		return decideApply(state, in)
	case model.CleanupCommandInput:
		return decideCleanup(state, in)
	default:
		return model.Decision{}, model.InvalidInputFault("unsupported input")
	}
}

// builder accumulates one Decision against an immutable input State. Event
// sequence numbers are assigned consecutively from State.NextEventSeq, so
// identical State/Input always produce byte-identical Decisions.
type builder struct {
	state model.State
	d     model.Decision
}

func (b *builder) mutate(m model.Mutation) {
	b.d.Mutations = append(b.d.Mutations, m)
}

func (b *builder) event(kind model.EventKind, node model.NodeID, attempt model.AttemptKey, code model.Code, text string) {
	b.d.Events = append(b.d.Events, model.Event{
		Seq:      b.state.NextEventSeq + uint64(len(b.d.Events)),
		Kind:     kind,
		Workflow: b.state.Workflow.ID,
		Node:     node,
		Attempt:  attempt,
		Code:     code,
		Text:     text,
		At:       b.state.Now,
	})
}

func (b *builder) effect(e model.EffectIntent) {
	b.d.Effect = e
}

func (b *builder) decision() model.Decision { return b.d }

// wfMut builds a WorkflowMutation carrying the full set of aggregate
// fields the Kernel owns; the mutation replaces them wholesale, so every
// caller passes through the current values except the ones it changes.
func wfMut(state model.State, stage model.WorkflowStage, rt model.RuntimeStatus, intent *model.CancelIntent) model.WorkflowMutation {
	return model.WorkflowMutation{
		ID:                state.Workflow.ID,
		Project:           state.Workflow.Project,
		Stage:             stage,
		Runtime:           rt,
		TargetBranch:      state.Workflow.TargetBranch,
		BaseCommit:        state.Workflow.BaseCommit,
		IntegrationBranch: state.Workflow.IntegrationBranch,
		IntegrationHead:   state.Workflow.IntegrationHead,
		CancelIntent:      intent,
	}
}

// wfMutStatus builds a WorkflowMutation changing only the Runtime Status.
func wfMutStatus(state model.State, rt model.RuntimeStatus) model.WorkflowMutation {
	return wfMut(state, state.Workflow.Stage, rt, state.Workflow.CancelIntent)
}

// newRun allocates the next Run identity deterministically from the
// aggregate (design 7.2: opaque, locally generated, never from display
// names; design 6.2 rule 6: fixed before any Effect).
func newRun(state model.State, status model.RunStatus, gate bool) model.Run {
	return model.Run{
		ID:           model.RunID(fmt.Sprintf("run-%d", len(state.Runs)+1)),
		Status:       status,
		DispatchGate: gate,
		StartedAt:    state.Now,
	}
}

func activeRun(state model.State) *model.Run {
	for i := range state.Runs {
		if !state.Runs[i].Status.IsTerminal() {
			return &state.Runs[i]
		}
	}
	return nil
}

func activeQuiescing(state model.State) bool {
	r := activeRun(state)
	return r != nil && r.Status == model.RunQuiescing
}

func hasRunningAttempt(state model.State) bool {
	for _, a := range state.Attempts {
		if a.Status == model.AttemptRunning {
			return true
		}
	}
	return false
}

// hasRunningAttemptExcept reports whether any Attempt other than exclude
// is still RUNNING.
func hasRunningAttemptExcept(state model.State, exclude model.AttemptKey) bool {
	for k, a := range state.Attempts {
		if k != exclude && a.Status == model.AttemptRunning {
			return true
		}
	}
	return false
}

func hasRunningProcess(state model.State) bool {
	for _, p := range state.Processes {
		if p.Status == model.ProcessStatusRunning {
			return true
		}
	}
	return false
}

func hasBlockingFinding(state model.State) bool {
	for _, f := range state.Findings {
		if f.Blocking {
			return true
		}
	}
	return false
}

func anyFailedNode(state model.State) bool {
	for _, n := range state.Nodes {
		if n.Status == model.NodeFailed {
			return true
		}
	}
	return false
}

// branchQuarantined reports whether a Branch carries a permanent
// Quarantine record. Quarantined Branches and Commits can never re-enter
// Verify, Merge, Final Verify, or Apply (design 7.3 invariant 10).
func branchQuarantined(state model.State, branch string) bool {
	for _, q := range state.Quarantines {
		if q.Branch == branch {
			return true
		}
	}
	return false
}

// stopRunningProcesses emits the first outstanding ManagedProcessStop
// Effect Intent, skipping any excluded processes (the ones that just
// settled in this decision's own mutations). At most one Effect may be
// requested per Decision; the Effect loop returns the next Result and the
// Kernel continues.
func stopRunningProcesses(b *builder, state model.State, exclude ...model.ProcessID) {
	for _, p := range state.Processes {
		if p.Status != model.ProcessStatusRunning {
			continue
		}
		skip := false
		for _, ex := range exclude {
			if p.ID == ex {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		b.effect(model.ManagedProcessStopIntent{Process: p.ID})
		return
	}
}

func findProcess(state model.State, id model.ProcessID) *model.ProcessRecord {
	for i := range state.Processes {
		if state.Processes[i].ID == id {
			return &state.Processes[i]
		}
	}
	return nil
}
