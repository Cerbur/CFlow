package compile

// The deterministic skeleton construction, parallel groups, restricted
// Patch application, resource lock injection, and canonical
// serialization (design 11). Same-package split of the deterministic
// Compiler: no public seam added.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
)

func buildSkeleton(specs []Spec, catalog Catalog, workflowID string, revision int) (Workflow, error) {
	if revision < 1 {
		revision = 1
	}
	var nodes []WorkflowNode
	edges := map[string]bool{}
	addEdge := func(from, to string) { edges[from+"\x00"+to] = true }

	commands := map[string]CatalogEntry{}
	for _, e := range catalog.Entries {
		commands[e.CommandID] = e
	}
	finalEntry, ok := finalVerifyEntry(catalog)
	if !ok {
		return Workflow{}, schemaInvalid("the catalog has no final_verify command; the FinalVerify node cannot be built")
	}

	for _, s := range specs {
		taskID := "task-" + s.ID
		nodes = append(nodes, WorkflowNode{
			ID: taskID, Type: nodeTypeAgentTask, SpecID: s.ID,
			TimeoutSeconds: s.TimeoutSeconds, MaxRetry: s.MaxRetry,
		})
		mergeID := "merge-" + s.ID
		verifyIDs := make([]string, 0, len(s.Acceptance.VerificationCommandIDs))
		for i, cmd := range s.Acceptance.VerificationCommandIDs {
			entry := commands[cmd]
			id := "verify-" + s.ID
			if len(s.Acceptance.VerificationCommandIDs) > 1 {
				id = fmt.Sprintf("verify-%s-%d", s.ID, i+1)
			}
			nodes = append(nodes, WorkflowNode{
				ID: id, Type: nodeTypeVerify, SpecID: s.ID,
				CommandID: cmd, TimeoutSeconds: entry.TimeoutSeconds,
			})
			addEdge(taskID, id)
			verifyIDs = append(verifyIDs, id)
		}
		nodes = append(nodes, WorkflowNode{ID: mergeID, Type: nodeTypeMerge, SpecID: s.ID})
		for _, v := range verifyIDs {
			addEdge(v, mergeID)
		}
		for _, dep := range s.DependsOn {
			addEdge("merge-"+dep, taskID)
		}
	}

	// Final verification coverage: exactly one FinalVerify, reachable
	// from every Merge.
	nodes = append(nodes, WorkflowNode{
		ID: "final-verify", Type: nodeTypeFinalVerify,
		CommandID: finalEntry.CommandID, TimeoutSeconds: finalEntry.TimeoutSeconds,
	})
	for _, s := range specs {
		addEdge("merge-"+s.ID, "final-verify")
	}

	wf := Workflow{
		Schema:     workflowSchema,
		WorkflowID: workflowID,
		Revision:   revision,
		Nodes:      nodes,
	}
	for e := range edges {
		parts := strings.SplitN(e, "\x00", 2)
		wf.Edges = append(wf.Edges, WorkflowEdge{From: parts[0], To: parts[1]})
	}
	return wf, nil
}

// finalVerifyEntry is the deterministic FinalVerify command: the first
// final_verify entry in canonical command order.
func finalVerifyEntry(catalog Catalog) (CatalogEntry, bool) {
	var entries []CatalogEntry
	for _, e := range catalog.Entries {
		if e.Purpose == "final_verify" {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return CatalogEntry{}, false
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].CommandID < entries[j].CommandID })
	return entries[0], true
}

// ParallelGroups is the deterministic longest-path level decomposition
// of a compiled Workflow: group 0 is the set of roots, and every later
// group holds the nodes whose longest path depth is the group index.
// Nodes within a group are ordered canonically.
func ParallelGroups(wf Workflow) [][]string {
	return parallelGroups(wf)
}

// parallelGroups is the deterministic longest-path level decomposition:
// group 0 is the set of roots, and every later group holds the nodes
// whose longest path depth is the group index. Nodes within a group are
// ordered canonically.
func parallelGroups(wf Workflow) [][]string {
	adj := map[string][]string{}
	indegree := map[string]int{}
	for _, n := range wf.Nodes {
		indegree[n.ID] = 0
	}
	for _, e := range wf.Edges {
		adj[e.From] = append(adj[e.From], e.To)
		indegree[e.To]++
	}
	level := map[string]int{}
	for id := range indegree {
		level[id] = 0
	}
	queue := make([]string, 0, len(wf.Nodes))
	for id, d := range indegree {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, next := range adj[id] {
			if level[next] < level[id]+1 {
				level[next] = level[id] + 1
			}
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	// Canonical group order: group index, then sorted node ids.
	maxLevel := 0
	for _, l := range level {
		if l > maxLevel {
			maxLevel = l
		}
	}
	groups := make([][]string, maxLevel+1)
	for id, l := range level {
		groups[l] = append(groups[l], id)
	}
	for _, g := range groups {
		sort.Strings(g)
	}
	return groups
}

// applyPatch validates the restricted Patch IR against the deterministic
// skeleton and applies only legal operations. Forbidden operations
// (removal, weakened dependencies, bypassed Merge, raised ceilings,
// non-eligible routes) fail with WORKFLOW_PATCH_FORBIDDEN; schema-valid
// operations that cannot apply to this skeleton are skipped with a
// Compile Finding and never replace the skeleton.
func applyPatch(wf *Workflow, patchBody []byte, specs []Spec, groups [][]string, reg *agent.ProviderRegistry) ([]RejectedPatchOp, []RoutePin, []ConcurrencyCap, []BudgetTightening, error) {
	patch, err := parsePatch(patchBody)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	exists := map[string]bool{}
	for _, n := range wf.Nodes {
		exists[n.ID] = true
	}
	groupOf := map[string]int{}
	for i, g := range groups {
		for _, id := range g {
			groupOf[id] = i
		}
	}
	specBudget := map[string]float64{}
	for _, s := range specs {
		if s.Route != nil {
			specBudget[s.ID] = s.Route.Budget
		}
	}
	nodeBudget := func(nodeID string) float64 {
		for _, n := range wf.Nodes {
			if n.ID == nodeID {
				if b, ok := specBudget[n.SpecID]; ok {
					return b
				}
				return 0
			}
		}
		return 0
	}
	successors := func(nodeID string) []string {
		var out []string
		for _, e := range wf.Edges {
			if e.From == nodeID {
				out = append(out, e.To)
			}
		}
		return out
	}

	var rejected []RejectedPatchOp
	var pins []RoutePin
	var caps []ConcurrencyCap
	var tightenings []BudgetTightening
	checkpoints := 0

	for _, op := range patch.Operations {
		switch op.Op {
		case "reduce_concurrency":
			if !exists[op.NodeID] {
				rejected = append(rejected, RejectedPatchOp{Op: op.Op, NodeID: op.NodeID, Reason: "unknown node"})
				continue
			}
			group := groups[groupOf[op.NodeID]]
			if op.MaxParallel > len(group) {
				return nil, nil, nil, nil, model.NewFault(model.CodeWorkflowPatchForbidden,
					fmt.Sprintf("reduce_concurrency on %s raises concurrency above the skeleton's group of %d", op.NodeID, len(group)))
			}
			caps = append(caps, ConcurrencyCap{NodeID: op.NodeID, MaxParallel: op.MaxParallel})
		case "pin_route":
			if !exists[op.NodeID] {
				rejected = append(rejected, RejectedPatchOp{Op: op.Op, NodeID: op.NodeID, Reason: "unknown node"})
				continue
			}
			if _, err := reg.Select(op.Provider); err != nil {
				return nil, nil, nil, nil, model.NewFault(model.CodeWorkflowPatchForbidden,
					fmt.Sprintf("pin_route on %s targets a provider that is not eligible: %v", op.NodeID, err))
			}
			pins = append(pins, RoutePin{NodeID: op.NodeID, Provider: op.Provider})
		case "add_checkpoint":
			if !exists[op.NodeID] {
				rejected = append(rejected, RejectedPatchOp{Op: op.Op, NodeID: op.NodeID, Reason: "unknown node"})
				continue
			}
			checkpoints++
			id := fmt.Sprintf("checkpoint-%d", checkpoints)
			wf.Nodes = append(wf.Nodes, WorkflowNode{ID: id, Type: nodeTypeCheckpoint})
			// The checkpoint observes the node's outputs and passes them
			// to every successor, preserving the DAG.
			wf.Edges = append(wf.Edges, WorkflowEdge{From: op.NodeID, To: id})
			for _, next := range successors(op.NodeID) {
				wf.Edges = append(wf.Edges, WorkflowEdge{From: id, To: next})
			}
			exists[id] = true
		case "tighten_budget":
			if !exists[op.NodeID] {
				rejected = append(rejected, RejectedPatchOp{Op: op.Op, NodeID: op.NodeID, Reason: "unknown node"})
				continue
			}
			current := nodeBudget(op.NodeID)
			if op.Budget > current {
				return nil, nil, nil, nil, model.NewFault(model.CodeWorkflowPatchForbidden,
					fmt.Sprintf("tighten_budget on %s raises the budget from %v to %v", op.NodeID, current, op.Budget))
			}
			tightenings = append(tightenings, BudgetTightening{NodeID: op.NodeID, Budget: op.Budget})
		default:
			return nil, nil, nil, nil, model.NewFault(model.CodeWorkflowPatchForbidden,
				fmt.Sprintf("patch operation %q is not a permitted scheduling operation", op.Op))
		}
	}
	return rejected, pins, caps, tightenings, nil
}

// injectLocks binds every Merge Node to the single integration lock and
// every Task to its Spec's declared Resource Locks. Lock names were
// validated during route validation.
func injectLocks(specs []Spec, workflowID string) []LockAssignment {
	var locks []LockAssignment
	for _, s := range specs {
		for _, lock := range s.Locks {
			locks = append(locks, LockAssignment{NodeID: "task-" + s.ID, Lock: lock})
		}
	}
	for _, s := range specs {
		locks = append(locks, LockAssignment{NodeID: "merge-" + s.ID, Lock: "integration:" + workflowID})
	}
	sort.Slice(locks, func(i, j int) bool {
		if locks[i].NodeID != locks[j].NodeID {
			return locks[i].NodeID < locks[j].NodeID
		}
		return locks[i].Lock < locks[j].Lock
	})
	return locks
}

// canonicalBody serializes the Workflow with fixed field order and
// canonically sorted nodes and edges.
func (wf Workflow) canonicalBody() ([]byte, error) {
	nodes := append([]WorkflowNode(nil), wf.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	edges := append([]WorkflowEdge(nil), wf.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	out := Workflow{
		Schema:     wf.Schema,
		WorkflowID: wf.WorkflowID,
		Revision:   wf.Revision,
		Nodes:      nodes,
		Edges:      edges,
	}
	body, err := yaml.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("workflow body cannot be serialized: %w", err)
	}
	return body, nil
}

func specHashes(bodies [][]byte) []string {
	hashes := make([]string, 0, len(bodies))
	for _, body := range bodies {
		hashes = append(hashes, sha256Hex(body))
	}
	return hashes
}

func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
