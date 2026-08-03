package compile_test

// Compiler mutation and golden tests (Task 11 brief Steps 1-2, 6): the
// deterministic skeleton, the restricted Patch IR, the Schema/DAG/
// coverage/scope/Catalog/route/budget checks, canonical serialization,
// and cross-run hash stability. Fixtures live in tests/testdata/compiler.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/internal/compile"
	"cflow.local/cflow/internal/model"
)

// ---------------------------------------------------------------------------
// fixture loading
// ---------------------------------------------------------------------------

// fixtureDoc is the on-disk shape of one compiler fixture: the compile
// request sections (plan ref, spec bodies, catalog body, optional patch)
// plus the hard-cap configuration.
type fixtureDoc struct {
	PlanRef        model.ArtifactRef `yaml:"plan_ref"`
	Specs          []map[string]any  `yaml:"specs"`
	Catalog        map[string]any    `yaml:"catalog"`
	Patch          map[string]any    `yaml:"patch"`
	MaxConcurrency int               `yaml:"max_concurrency"`
	MaxTotalRuns   int               `yaml:"max_total_runs"`
	MaxTaskTimeout int               `yaml:"max_task_timeout"`
}

func fixturePath(name string) string {
	return filepath.Join("..", "..", "tests", "testdata", "compiler", name)
}

func loadRequest(t *testing.T, name string) compile.CompileRequest {
	t.Helper()
	data, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var doc fixtureDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	req := compile.CompileRequest{
		PlanRef:        doc.PlanRef,
		WorkflowID:     "wf-1",
		MaxConcurrency: doc.MaxConcurrency,
		MaxTotalRuns:   doc.MaxTotalRuns,
		MaxTaskTimeout: doc.MaxTaskTimeout,
	}
	for _, spec := range doc.Specs {
		body, err := yaml.Marshal(spec)
		if err != nil {
			t.Fatalf("serialize spec: %v", err)
		}
		req.SpecBodies = append(req.SpecBodies, body)
	}
	if doc.Catalog != nil {
		body, err := yaml.Marshal(doc.Catalog)
		if err != nil {
			t.Fatalf("serialize catalog: %v", err)
		}
		req.CatalogBody = body
		revision, _ := doc.Catalog["revision"].(int)
		req.CatalogRef = model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactCatalog, Revision: revision}
	}
	if doc.Patch != nil {
		body, err := yaml.Marshal(doc.Patch)
		if err != nil {
			t.Fatalf("serialize patch: %v", err)
		}
		req.Patch = body
	}
	return req
}

// validCompileRequest builds the brief's canonical request from the
// valid fixture. It panics on fixture errors so the brief's verbatim
// tests can call it without a testing.T.
func validCompileRequest() compile.CompileRequest {
	data, err := os.ReadFile(fixturePath("valid.yaml"))
	if err != nil {
		panic(err)
	}
	var doc fixtureDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		panic(err)
	}
	req := compile.CompileRequest{
		PlanRef:        doc.PlanRef,
		WorkflowID:     "wf-1",
		MaxConcurrency: doc.MaxConcurrency,
		MaxTotalRuns:   doc.MaxTotalRuns,
		MaxTaskTimeout: doc.MaxTaskTimeout,
	}
	for _, spec := range doc.Specs {
		body, err := yaml.Marshal(spec)
		if err != nil {
			panic(err)
		}
		req.SpecBodies = append(req.SpecBodies, body)
	}
	body, err := yaml.Marshal(doc.Catalog)
	if err != nil {
		panic(err)
	}
	req.CatalogBody = body
	revision, _ := doc.Catalog["revision"].(int)
	req.CatalogRef = model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactCatalog, Revision: revision}
	return req
}

// rewriteSpec parses one spec body, applies a change, and re-serializes
// it canonically.
func rewriteSpec(t *testing.T, body []byte, change func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(body, &m); err != nil {
		t.Fatalf("parse spec body: %v", err)
	}
	change(m)
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("serialize spec body: %v", err)
	}
	return out
}

// rewriteCatalog parses the catalog body, applies a change, and
// re-serializes it canonically.
func rewriteCatalog(t *testing.T, body []byte, change func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(body, &m); err != nil {
		t.Fatalf("parse catalog body: %v", err)
	}
	change(m)
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("serialize catalog body: %v", err)
	}
	return out
}

// patchRemovingNode builds a Patch IR that attempts to remove one node.
// No Patch operation may delete a node; the Compiler must reject it.
func patchRemovingNode(nodeID string) []byte {
	body, err := yaml.Marshal(map[string]any{
		"schema":     "cflow-workflow-patch-1",
		"operations": []any{map[string]any{"op": "remove_node", "node_id": nodeID}},
	})
	if err != nil {
		panic(err)
	}
	return body
}

func validSpecYAML() string {
	return `id: s01
goal: implement divide
depends_on: []
write_scope: ["src/divide/**"]
read_scope: []
locks: []
acceptance:
  verification_command_ids: [verify]
route:
  provider: fake
  model: default
  budget: 10
timeout_seconds: 1800
max_retry: 2
`
}

func assertFaultCode(t *testing.T, err error, code model.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected fault %s, got nil error", code)
	}
	got, ok := model.CodeOf(err)
	if !ok || got != code {
		t.Fatalf("error = %v, want fault code %s", err, code)
	}
}

// ---------------------------------------------------------------------------
// brief Step 1 verbatim tests
// ---------------------------------------------------------------------------

func TestPatchCannotRemoveVerifyNode(t *testing.T) {
	req := validCompileRequest()
	req.Patch = patchRemovingNode("verify-S01")
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeWorkflowPatchForbidden)
}

func TestSpecRejectsFreeArgv(t *testing.T) {
	spec := validSpecYAML() + "\ncommand: [sh, -c, echo unsafe]\n"
	_, err := compile.ParseSpec([]byte(spec))
	assertFaultCode(t, err, model.CodeSchemaInvalid)
}

// ---------------------------------------------------------------------------
// deterministic skeleton
// ---------------------------------------------------------------------------

// TestCompileBuildsDeterministicSkeleton asserts the full deterministic
// skeleton: one AgentTask/Verify/Merge chain per Spec (sorted
// canonically), dependency preservation through merge edges, one
// FinalVerify reachable from every merge, per-node timeouts and retries,
// the integration resource lock on every merge, and the canonical
// serialization and hash.
func TestCompileBuildsDeterministicSkeleton(t *testing.T) {
	out, err := (&compile.Compiler{}).Compile(context.Background(), validCompileRequest())
	if err != nil {
		t.Fatalf("compile valid request: %v", err)
	}
	if out.Hash == "" || out.Hash != hashOf(t, out.Body) {
		t.Fatalf("output hash %q does not match the canonical body", out.Hash)
	}
	wf, err := compile.ParseWorkflow(out.Body)
	if err != nil {
		t.Fatalf("compiled body is not a valid workflow: %v", err)
	}
	nodes := map[string]compile.WorkflowNode{}
	for _, n := range wf.Nodes {
		nodes[n.ID] = n
	}
	// Exactly one AgentTask, Verify, and Merge per Spec plus one FinalVerify.
	if len(wf.Nodes) != 10 {
		t.Fatalf("nodes = %d, want 10: %v", len(wf.Nodes), nodeIDs(wf))
	}
	for _, id := range []string{"task-s01", "verify-s01", "merge-s01",
		"task-s02", "verify-s02", "merge-s02", "task-s03", "verify-s03", "merge-s03"} {
		if _, ok := nodes[id]; !ok {
			t.Fatalf("missing skeleton node %s", id)
		}
	}
	fv, ok := nodes["final-verify"]
	if !ok {
		t.Fatal("missing final-verify node")
	}
	if fv.Type != "final_verify" || fv.CommandID != "final-verify" {
		t.Fatalf("final-verify node = %+v", fv)
	}
	if nodes["verify-s01"].CommandID != "verify" || nodes["verify-s01"].SpecID != "s01" {
		t.Fatalf("verify node = %+v", nodes["verify-s01"])
	}
	if nodes["task-s01"].TimeoutSeconds != 1800 || nodes["task-s01"].MaxRetry != 2 {
		t.Fatalf("task node budgets = %+v", nodes["task-s01"])
	}
	if nodes["verify-s01"].TimeoutSeconds != 600 {
		t.Fatalf("verify timeout = %d, want the catalog entry's 600", nodes["verify-s01"].TimeoutSeconds)
	}

	edges := edgeSet(wf)
	// Chain edges.
	for _, e := range [][2]string{
		{"task-s01", "verify-s01"}, {"verify-s01", "merge-s01"},
		{"task-s02", "verify-s02"}, {"verify-s02", "merge-s02"},
		{"task-s03", "verify-s03"}, {"verify-s03", "merge-s03"},
	} {
		if !edges[e[0]+"->"+e[1]] {
			t.Fatalf("missing chain edge %s -> %s", e[0], e[1])
		}
	}
	// Dependency preservation: task-s02 waits for merge-s01.
	if !edges["merge-s01->task-s02"] {
		t.Fatal("dependency preservation: task-s02 does not wait for merge-s01")
	}
	// Final verification coverage: every merge feeds the FinalVerify.
	for _, m := range []string{"merge-s01", "merge-s02", "merge-s03"} {
		if !edges[m+"->final-verify"] {
			t.Fatalf("final verify is unreachable from %s", m)
		}
	}
	// Every node is reachable from a root.
	reach := reachableFromRoots(wf)
	for _, n := range wf.Nodes {
		if !reach[n.ID] {
			t.Fatalf("node %s is unreachable from the roots", n.ID)
		}
	}

	// Resource lock injection: every merge carries the integration lock.
	locks := map[string]string{}
	for _, l := range out.Evidence.Locks {
		locks[l.NodeID] = l.Lock
	}
	for _, m := range []string{"merge-s01", "merge-s02", "merge-s03"} {
		if locks[m] != "integration:wf-1" {
			t.Fatalf("merge %s lock = %q, want integration:wf-1", m, locks[m])
		}
	}

	// Budgets: total agent runs and retries are computable.
	if out.Evidence.TotalAgentRuns != 8 || out.Evidence.TotalRetries != 5 {
		t.Fatalf("budget totals = runs %d retries %d, want 8/5", out.Evidence.TotalAgentRuns, out.Evidence.TotalRetries)
	}
	// Parallel groups are reported in canonical order.
	if len(out.Evidence.ParallelGroups) == 0 {
		t.Fatal("no parallel groups reported")
	}
	if len(out.Evidence.ParallelGroups[0]) != 2 {
		t.Fatalf("first parallel group = %v, want the two independent tasks", out.Evidence.ParallelGroups[0])
	}
}

// TestCompileGoldenHashStable pins the canonical compiled hash of the
// valid fixture: any change to the skeleton, canonical serialization, or
// hash computation breaks this golden value (brief Step 6: golden
// Artifact hashes remain stable across runs).
func TestCompileGoldenHashStable(t *testing.T) {
	out, err := (&compile.Compiler{}).Compile(context.Background(), validCompileRequest())
	if err != nil {
		t.Fatalf("compile valid request: %v", err)
	}
	if out.Hash != "f6b7b7c5a4c058ebae7f7f4a6b372264497ba8480e8dcf558b9c06f4f9dc846f" {
		t.Fatalf("golden compiled hash changed: %s", out.Hash)
	}
}

// ---------------------------------------------------------------------------
// mutation cases (brief case list)
// ---------------------------------------------------------------------------

func TestCompileRejectsSpecCycle(t *testing.T) {
	_, err := (&compile.Compiler{}).Compile(context.Background(), loadRequest(t, "cycle.yaml"))
	assertFaultCode(t, err, model.CodeSchemaInvalid)
}

func TestCompileRejectsMissingDependency(t *testing.T) {
	req := validCompileRequest()
	req.SpecBodies[0] = rewriteSpec(t, req.SpecBodies[0], func(m map[string]any) {
		m["depends_on"] = []any{"ghost-spec"}
	})
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeSchemaInvalid)
}

func TestCompileRejectsOverlappingWriteScopes(t *testing.T) {
	_, err := (&compile.Compiler{}).Compile(context.Background(), loadRequest(t, "scope-conflict.yaml"))
	assertFaultCode(t, err, model.CodeScopeViolation)
}

// TestCompileAcceptsOverlappingWriteScopesWithSharedLock: the same
// overlap is legal when the two Specs declare a shared resource lock.
func TestCompileAcceptsOverlappingWriteScopesWithSharedLock(t *testing.T) {
	req := loadRequest(t, "scope-conflict.yaml")
	for i := range req.SpecBodies {
		req.SpecBodies[i] = rewriteSpec(t, req.SpecBodies[i], func(m map[string]any) {
			m["locks"] = []any{"shared-data"}
		})
	}
	if _, err := (&compile.Compiler{}).Compile(context.Background(), req); err != nil {
		t.Fatalf("overlap with a shared lock should compile: %v", err)
	}
}

func TestCompileRejectsUnknownCatalogID(t *testing.T) {
	req := validCompileRequest()
	req.SpecBodies[0] = rewriteSpec(t, req.SpecBodies[0], func(m map[string]any) {
		m["acceptance"] = map[string]any{"verification_command_ids": []any{"ghost-command"}}
	})
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeSchemaInvalid)
}

func TestCompileRejectsWrongCommandPurpose(t *testing.T) {
	req := validCompileRequest()
	req.SpecBodies[0] = rewriteSpec(t, req.SpecBodies[0], func(m map[string]any) {
		m["acceptance"] = map[string]any{"verification_command_ids": []any{"final-verify"}}
	})
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeSchemaInvalid)
}

func TestCompileRejectsChangedExecutableHash(t *testing.T) {
	req := validCompileRequest()
	req.CatalogBody = rewriteCatalog(t, req.CatalogBody, func(m map[string]any) {
		entries := m["entries"].([]any)
		entry := entries[0].(map[string]any)
		entry["source"] = "base-commit-wrapper:scripts/verify.sh@sha256:not-a-hash"
	})
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeSchemaInvalid)
}

func TestCompileRejectsExpandedBudget(t *testing.T) {
	req := validCompileRequest()
	req.SpecBodies[0] = rewriteSpec(t, req.SpecBodies[0], func(m map[string]any) {
		m["max_retry"] = 1000
	})
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeBudgetExceeded)
}

func TestCompileRejectsRemovedMergeNode(t *testing.T) {
	req := validCompileRequest()
	req.Patch = patchRemovingNode("merge-s01")
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeWorkflowPatchForbidden)
}

func TestCompileRejectsUnreachableFinalVerify(t *testing.T) {
	req := validCompileRequest()
	req.CatalogBody = rewriteCatalog(t, req.CatalogBody, func(m map[string]any) {
		entries := m["entries"].([]any)
		m["entries"] = entries[:1] // drop the final_verify entry
	})
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeSchemaInvalid)
}

func TestCompileRejectsCatalogMismatch(t *testing.T) {
	req := loadRequest(t, "catalog-mismatch.yaml")
	req.CatalogRef.Revision = 1 // the active immutable revision differs from the body's
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeSchemaInvalid)
}

func TestCompileRejectsForbiddenPatch(t *testing.T) {
	_, err := (&compile.Compiler{}).Compile(context.Background(), loadRequest(t, "forbidden-patch.yaml"))
	assertFaultCode(t, err, model.CodeWorkflowPatchForbidden)
}

func TestCompileRejectsNonEligibleRoutePin(t *testing.T) {
	req := validCompileRequest()
	req.Patch = patchBody(t, []patchOp{{Op: "pin_route", NodeID: "task-s01", Provider: "unknown-provider"}})
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeWorkflowPatchForbidden)
}

func TestCompileRejectsBudgetRaisePatch(t *testing.T) {
	req := validCompileRequest()
	req.Patch = patchBody(t, []patchOp{{Op: "tighten_budget", NodeID: "task-s01", Budget: 20}})
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeWorkflowPatchForbidden)
}

func TestCompileRejectsConcurrencyRaisePatch(t *testing.T) {
	req := validCompileRequest()
	req.Patch = patchBody(t, []patchOp{{Op: "reduce_concurrency", NodeID: "task-s01", MaxParallel: 4}})
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeWorkflowPatchForbidden)
}

func TestCompileRejectsUnknownProviderRoute(t *testing.T) {
	req := validCompileRequest()
	req.SpecBodies[0] = rewriteSpec(t, req.SpecBodies[0], func(m map[string]any) {
		m["route"] = map[string]any{"provider": "unknown-provider", "model": "default", "budget": 10}
	})
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeSchemaInvalid)
}

func TestCompileRejectsReservedLockName(t *testing.T) {
	req := validCompileRequest()
	req.SpecBodies[0] = rewriteSpec(t, req.SpecBodies[0], func(m map[string]any) {
		m["locks"] = []any{"integration:wf-1"}
	})
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeSchemaInvalid)
}

func TestCompileRejectsConcurrencyCapExceeded(t *testing.T) {
	req := validCompileRequest()
	req.MaxConcurrency = 1 // the skeleton's first parallel group has width 2
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeBudgetExceeded)
}

func TestCompileRejectsOversizedTimeout(t *testing.T) {
	req := validCompileRequest()
	req.MaxTaskTimeout = 1000
	_, err := (&compile.Compiler{}).Compile(context.Background(), req)
	assertFaultCode(t, err, model.CodeBudgetExceeded)
}

// ---------------------------------------------------------------------------
// patch application
// ---------------------------------------------------------------------------

func TestCompileAppliesCheckpointPatch(t *testing.T) {
	req := validCompileRequest()
	req.Patch = patchBody(t, []patchOp{{Op: "add_checkpoint", NodeID: "merge-s01"}})
	out, err := (&compile.Compiler{}).Compile(context.Background(), req)
	if err != nil {
		t.Fatalf("checkpoint patch should compile: %v", err)
	}
	wf, err := compile.ParseWorkflow(out.Body)
	if err != nil {
		t.Fatalf("compiled body: %v", err)
	}
	edges := edgeSet(wf)
	nodes := map[string]compile.WorkflowNode{}
	for _, n := range wf.Nodes {
		nodes[n.ID] = n
	}
	cp, ok := nodes["checkpoint-1"]
	if !ok || cp.Type != "checkpoint" {
		t.Fatalf("checkpoint node missing: %v", nodes)
	}
	if !edges["merge-s01->checkpoint-1"] || !edges["checkpoint-1->final-verify"] {
		t.Fatalf("checkpoint edges = %v", edges)
	}
	// The DAG never carries a self-edge: the checkpoint's successor set is
	// captured before its incoming edge lands (Task 18 fix; a self-loop
	// would permanently gate the final verify).
	if edges["checkpoint-1->checkpoint-1"] {
		t.Fatalf("checkpoint edges carry a self-loop: %v", edges)
	}
	// The final verify remains reachable through the checkpoint.
	reach := reachableFromRoots(wf)
	if !reach["final-verify"] {
		t.Fatal("final-verify unreachable after the checkpoint")
	}
}

// TestCompileInertPatchOpsKeepSkeleton: a schema-valid op targeting an
// unknown node cannot weaken this skeleton; the Compiler skips it, keeps
// the deterministic skeleton, and reports a Compile Finding (visible in
// Dry Run) instead of failing.
func TestCompileInertPatchOpsKeepSkeleton(t *testing.T) {
	req := validCompileRequest()
	req.Patch = patchBody(t, []patchOp{{Op: "add_checkpoint", NodeID: "ghost-node"}})
	out, err := (&compile.Compiler{}).Compile(context.Background(), req)
	if err != nil {
		t.Fatalf("inert patch op should not fail the compile: %v", err)
	}
	if len(out.RejectedOps) != 1 {
		t.Fatalf("rejected ops = %v, want 1", out.RejectedOps)
	}
	wf, err := compile.ParseWorkflow(out.Body)
	if err != nil {
		t.Fatalf("compiled body: %v", err)
	}
	for _, n := range wf.Nodes {
		if n.Type == "checkpoint" {
			t.Fatalf("inert checkpoint op was applied: %+v", n)
		}
	}
}

func TestCompileAppliesBudgetTighteningAndRoutePin(t *testing.T) {
	req := validCompileRequest()
	req.Patch = patchBody(t, []patchOp{
		{Op: "tighten_budget", NodeID: "task-s01", Budget: 5},
		{Op: "pin_route", NodeID: "task-s01", Provider: "fake"},
	})
	out, err := (&compile.Compiler{}).Compile(context.Background(), req)
	if err != nil {
		t.Fatalf("tightening and eligible pin should compile: %v", err)
	}
	if len(out.RejectedOps) != 0 {
		t.Fatalf("rejected ops = %v, want none", out.RejectedOps)
	}
	if len(out.Evidence.BudgetTightenings) != 1 || out.Evidence.BudgetTightenings[0].Budget != 5 {
		t.Fatalf("budget tightenings = %v", out.Evidence.BudgetTightenings)
	}
	if len(out.Evidence.PinnedRoutes) != 1 || out.Evidence.PinnedRoutes[0].Provider != "fake" {
		t.Fatalf("pinned routes = %v", out.Evidence.PinnedRoutes)
	}
}

func TestCompileAppliesConcurrencyReduction(t *testing.T) {
	req := validCompileRequest()
	req.Patch = patchBody(t, []patchOp{{Op: "reduce_concurrency", NodeID: "task-s01", MaxParallel: 1}})
	out, err := (&compile.Compiler{}).Compile(context.Background(), req)
	if err != nil {
		t.Fatalf("concurrency reduction should compile: %v", err)
	}
	if len(out.Evidence.ConcurrencyCaps) != 1 || out.Evidence.ConcurrencyCaps[0].MaxParallel != 1 {
		t.Fatalf("concurrency caps = %v", out.Evidence.ConcurrencyCaps)
	}
}

// ---------------------------------------------------------------------------
// determinism and canonical serialization
// ---------------------------------------------------------------------------

func TestCompileIsDeterministic(t *testing.T) {
	compiler := &compile.Compiler{}
	first, err := compiler.Compile(context.Background(), validCompileRequest())
	if err != nil {
		t.Fatalf("first compile: %v", err)
	}
	second, err := compiler.Compile(context.Background(), validCompileRequest())
	if err != nil {
		t.Fatalf("second compile: %v", err)
	}
	if string(first.Body) != string(second.Body) || first.Hash != second.Hash {
		t.Fatal("the same canonical input produced different output")
	}
}

// TestCompileCanonicalizesSpecOrder: a request whose Specs arrive in
// reversed order compiles to the byte-identical canonical output.
func TestCompileCanonicalizesSpecOrder(t *testing.T) {
	req := validCompileRequest()
	canonical, err := (&compile.Compiler{}).Compile(context.Background(), req)
	if err != nil {
		t.Fatalf("canonical compile: %v", err)
	}
	for i, j := 0, len(req.SpecBodies)-1; i < j; i, j = i+1, j-1 {
		req.SpecBodies[i], req.SpecBodies[j] = req.SpecBodies[j], req.SpecBodies[i]
	}
	reversed, err := (&compile.Compiler{}).Compile(context.Background(), req)
	if err != nil {
		t.Fatalf("reversed compile: %v", err)
	}
	if string(canonical.Body) != string(reversed.Body) || canonical.Hash != reversed.Hash {
		t.Fatal("spec order changed the canonical output")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type patchOp struct {
	Op          string
	NodeID      string
	MaxParallel int
	Provider    string
	Budget      float64
}

func patchBody(t *testing.T, ops []patchOp) []byte {
	t.Helper()
	var raw []any
	for _, op := range ops {
		m := map[string]any{"op": op.Op, "node_id": op.NodeID}
		if op.MaxParallel != 0 {
			m["max_parallel"] = op.MaxParallel
		}
		if op.Provider != "" {
			m["provider"] = op.Provider
		}
		if op.Budget != 0 {
			m["budget"] = op.Budget
		}
		raw = append(raw, m)
	}
	body, err := yaml.Marshal(map[string]any{
		"schema":     "cflow-workflow-patch-1",
		"operations": raw,
	})
	if err != nil {
		t.Fatalf("serialize patch: %v", err)
	}
	return body
}

func hashOf(t *testing.T, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func nodeIDs(wf compile.Workflow) []string {
	ids := make([]string, 0, len(wf.Nodes))
	for _, n := range wf.Nodes {
		ids = append(ids, n.ID)
	}
	return ids
}

func edgeSet(wf compile.Workflow) map[string]bool {
	set := map[string]bool{}
	for _, e := range wf.Edges {
		set[e.From+"->"+e.To] = true
	}
	return set
}

// reachableFromRoots computes the set of nodes reachable from the
// skeleton roots (nodes with no incoming edge).
func reachableFromRoots(wf compile.Workflow) map[string]bool {
	incoming := map[string]int{}
	for _, e := range wf.Edges {
		incoming[e.To]++
	}
	adj := map[string][]string{}
	for _, e := range wf.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	reach := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if reach[id] {
			return
		}
		reach[id] = true
		for _, next := range adj[id] {
			walk(next)
		}
	}
	for _, n := range wf.Nodes {
		if incoming[n.ID] == 0 {
			walk(n.ID)
		}
	}
	return reach
}
