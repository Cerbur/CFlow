package schedule_test

// The pure DAG Scheduler tests (Task 12, design 12): readiness is computed
// only from persisted Node/Attempt/dependency/gate facts, never from Task
// display status. The brief-mandated tests (Step 1) are verbatim; the
// additional cases cover dependency blocking, max concurrency, and budget
// unavailability at the pure-policy level (the lock-conflict, gate-race,
// one-project-writer, and cross-project cases are Application tests).

import (
	"slices"
	"strings"
	"testing"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/schedule"
)

// dagFixture is the pure Scheduler input of one test.
type dagFixture struct {
	Graph  model.GraphSnapshot
	Policy model.DispatchPolicy
}

// fixtureDAG builds a GraphSnapshot from a compact declaration: bare node
// ids are independent roots, and "S04<-S01,S02" declares a node whose
// dependencies are the named nodes. Every node is a READY agent-task, the
// status the install/readiness machinery produces before allocation.
func fixtureDAG(parts ...string) dagFixture {
	var nodes []model.GraphNode
	for _, p := range parts {
		id := p
		var deps []model.NodeID
		if i := strings.Index(p, "<-"); i >= 0 {
			id = p[:i]
			for _, d := range strings.Split(p[i+2:], ",") {
				deps = append(deps, model.NodeID(d))
			}
		}
		nodes = append(nodes, model.GraphNode{
			ID: model.NodeID(id), Kind: model.NodeAgentTask,
			Status: model.NodeReady, Dependencies: deps,
		})
	}
	return dagFixture{
		Graph:  model.GraphSnapshot{Nodes: nodes, DispatchGateOpen: true},
		Policy: model.DispatchPolicy{},
	}
}

// requireNodeIDs asserts the exact eligible set in canonical order.
func requireNodeIDs(t *testing.T, got model.DispatchDecision, want ...string) {
	t.Helper()
	gotIDs := make([]string, 0, len(got.Eligible))
	for _, id := range got.Eligible {
		gotIDs = append(gotIDs, string(id))
	}
	if !slices.Equal(gotIDs, want) {
		t.Fatalf("eligible = %v, want %v (reasons: %v)", gotIDs, want, got.Reasons)
	}
}

func requireReason(t *testing.T, got model.DispatchDecision, id string, want string) {
	t.Helper()
	reason, ok := got.Reasons[model.NodeID(id)]
	if !ok {
		t.Fatalf("no reason recorded for %s in %+v", id, got)
	}
	if !strings.Contains(reason, want) {
		t.Fatalf("reason for %s = %q, want it to contain %q", id, reason, want)
	}
}

// TestReadySelectsIndependentTasksInCanonicalOrder (brief Step 1,
// verbatim): the three independent Tasks are eligible in canonical order;
// S04 waits on S01 and S02.
func TestReadySelectsIndependentTasksInCanonicalOrder(t *testing.T) {
	state := fixtureDAG("S01", "S02", "S03", "S04<-S01,S02")
	got := (schedule.Scheduler{}).Next(state.Graph, state.Policy)
	requireNodeIDs(t, got, "S01", "S02", "S03")
}

// TestDependencyBlocking: a Node whose dependency Merge has not succeeded
// is not eligible and carries the reason; once the dependencies succeed it
// becomes eligible.
func TestDependencyBlocking(t *testing.T) {
	state := fixtureDAG("S01", "S02", "S03", "S04<-S01,S02")
	got := (schedule.Scheduler{}).Next(state.Graph, state.Policy)
	requireReason(t, got, "S04", "dependency S01 has not succeeded")

	// The dependencies merge; S04 becomes eligible while S01 and S02 stay
	// terminal and are never re-allocated.
	for i := range state.Graph.Nodes {
		switch state.Graph.Nodes[i].ID {
		case "S01", "S02":
			state.Graph.Nodes[i].Status = model.NodeSucceeded
		}
	}
	got = (schedule.Scheduler{}).Next(state.Graph, state.Policy)
	requireNodeIDs(t, got, "S03", "S04")
}

// TestMaxConcurrency: the dispatch policy caps the number of running
// Nodes; the excess eligible Nodes wait with the reason recorded.
func TestMaxConcurrency(t *testing.T) {
	state := fixtureDAG("S01", "S02", "S03")
	state.Policy.MaxConcurrency = 2
	got := (schedule.Scheduler{}).Next(state.Graph, state.Policy)
	requireNodeIDs(t, got, "S01", "S02")
	requireReason(t, got, "S03", "concurrency")
}

// TestMaxConcurrencyCountsRunningNodes: already RUNNING Nodes consume the
// cap, so a Ready Node may wait even when the eligible set is empty.
func TestMaxConcurrencyCountsRunningNodes(t *testing.T) {
	state := fixtureDAG("S01", "S02")
	for i := range state.Graph.Nodes {
		if state.Graph.Nodes[i].ID == "S01" {
			state.Graph.Nodes[i].Status = model.NodeRunning
		}
	}
	state.Policy.MaxConcurrency = 1
	got := (schedule.Scheduler{}).Next(state.Graph, state.Policy)
	requireNodeIDs(t, got)
	requireReason(t, got, "S02", "concurrency")
	requireReason(t, got, "S01", "running")
}

// TestBudgetUnavailable: a per-kind budget cap leaves later eligible Nodes
// unallocated with the reason recorded.
func TestBudgetUnavailable(t *testing.T) {
	state := fixtureDAG("S01", "S02", "S03")
	state.Policy.Budgets = map[model.NodeKind]int{model.NodeAgentTask: 1}
	got := (schedule.Scheduler{}).Next(state.Graph, state.Policy)
	requireNodeIDs(t, got, "S01")
	requireReason(t, got, "S02", "budget")
}

// TestDispatchGateClosed: a closed Dispatch Gate makes every Node
// ineligible, whatever its readiness.
func TestDispatchGateClosed(t *testing.T) {
	state := fixtureDAG("S01", "S02")
	state.Graph.DispatchGateOpen = false
	got := (schedule.Scheduler{}).Next(state.Graph, state.Policy)
	requireNodeIDs(t, got)
	requireReason(t, got, "S01", "gate is closed")
	requireReason(t, got, "S02", "gate is closed")
}

// TestTerminalAndRunningNodesAreNotEligible: readiness is never inferred
// from a display status; only the persisted Node status decides.
func TestTerminalAndRunningNodesAreNotEligible(t *testing.T) {
	state := fixtureDAG("S01", "S02", "S03")
	for i := range state.Graph.Nodes {
		switch state.Graph.Nodes[i].ID {
		case "S01":
			state.Graph.Nodes[i].Status = model.NodeRunning
		case "S02":
			state.Graph.Nodes[i].Status = model.NodeSucceeded
		}
	}
	got := (schedule.Scheduler{}).Next(state.Graph, state.Policy)
	requireNodeIDs(t, got, "S03")
	requireReason(t, got, "S01", "running")
	requireReason(t, got, "S02", "SUCCEEDED")
}
