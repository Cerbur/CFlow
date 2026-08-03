// Package schedule implements the pure DAG Scheduler (design 12). It is a
// concrete pure policy module: it does not start goroutines, processes, or
// database transactions itself, and it never infers readiness from Task
// display status — only the persisted Node status, the dependency edges of
// the approved compiled Workflow, and the Run Dispatch Gate decide (PRD
// 状态机与持久化模型, design 12).
package schedule

import (
	"sort"

	"cflow.local/cflow/internal/model"
)

// Scheduler is the concrete pure policy module (design 12, stable
// interface ledger):
//
//	type Scheduler struct{}
//	func (Scheduler) Next(model.GraphSnapshot, model.DispatchPolicy) model.DispatchDecision
//
// Next computes the Nodes eligible for allocation plus the reasons every
// other Node is not eligible. The Application serializes allocation
// against the Run Dispatch Gate and commits each RUNNING Attempt before
// submitting its Effect (design 12); the Kernel revalidates the gate in
// the same transaction, so no start can cross a committed closure
// (PRD 已确认：并行失败后的 Quiescing).
type Scheduler struct{}

// Next computes the eligible Node set from the pure GraphSnapshot (the
// persisted Node/Attempt state plus the Dispatch Gate) and the approved
// DispatchPolicy. The result is deterministic: eligible Nodes appear in
// canonical (sorted) id order, and every non-eligible Node carries the
// reason it cannot be allocated.
func (Scheduler) Next(snap model.GraphSnapshot, policy model.DispatchPolicy) model.DispatchDecision {
	byID := make(map[model.NodeID]model.GraphNode, len(snap.Nodes))
	for _, n := range snap.Nodes {
		byID[n.ID] = n
	}
	ids := make([]model.NodeID, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// The persisted RUNNING Nodes consume the concurrency and kind
	// budgets; the batch this pass allocates consumes the remainder.
	running := 0
	runningByKind := map[model.NodeKind]int{}
	for _, n := range byID {
		if n.Status == model.NodeRunning {
			running++
			runningByKind[n.Kind]++
		}
	}
	allocated := 0
	allocatedByKind := map[model.NodeKind]int{}

	dec := model.DispatchDecision{Reasons: map[model.NodeID]string{}}
	for _, id := range ids {
		n := byID[id]
		if reason := schedulable(n, snap, byID); reason != "" {
			dec.Reasons[id] = reason
			continue
		}
		if policy.MaxConcurrency > 0 && running+allocated >= policy.MaxConcurrency {
			dec.Reasons[id] = "max concurrency reached"
			continue
		}
		if cap, ok := policy.Budgets[n.Kind]; ok && cap > 0 &&
			runningByKind[n.Kind]+allocatedByKind[n.Kind] >= cap {
			dec.Reasons[id] = "kind budget exhausted"
			continue
		}
		dec.Eligible = append(dec.Eligible, id)
		allocated++
		allocatedByKind[n.Kind]++
	}
	return dec
}

// schedulable reports why one Node cannot be allocated from the pure
// graph facts ("" = schedulable). Readiness requires the Run to be
// Running with the Dispatch Gate open, the Node itself schedulable
// (PENDING, or READY for a budgeted retry), and every dependency Merge
// Node succeeded (design 12).
func schedulable(n model.GraphNode, snap model.GraphSnapshot, byID map[model.NodeID]model.GraphNode) string {
	if !snap.DispatchGateOpen {
		return "dispatch gate is closed"
	}
	switch n.Status {
	case model.NodePending, model.NodeReady:
	case model.NodeRunning:
		return "node is running"
	default:
		return "node is " + string(n.Status)
	}
	for _, d := range n.Dependencies {
		dep, ok := byID[d]
		if !ok {
			return "dependency " + string(d) + " does not exist"
		}
		if dep.Status != model.NodeSucceeded {
			return "dependency " + string(d) + " has not succeeded"
		}
	}
	return ""
}
