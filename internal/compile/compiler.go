package compile

// The deterministic Workflow Compiler (design 11, PRD 已确认：Dynamic
// Workflow 生成模型): it validates the Spec set, Verification Catalog,
// and optional restricted Patch IR; builds the deterministic
// AgentTask/Verify/Merge skeleton plus Checkpoints and one FinalVerify;
// validates the final verification coverage, resource locks, budgets,
// routes, and concurrency; and emits the canonical Dynamic Workflow
// body with a stable hash.
//
// The Compiler is deterministic for the same canonical inputs and
// Compiler version: Specs and edges are sorted canonically, every body
// is validated against the embedded schema, and the canonical
// serialization is the fixed struct order. A Patch may only reduce
// concurrency, choose already eligible routes, add non-approval
// Checkpoints, or tighten budgets; forbidden operations fail with
// WORKFLOW_PATCH_FORBIDDEN and inert operations are skipped with a
// Compile Finding, never replacing the deterministic skeleton.

import (
	"context"
	"fmt"
	"regexp"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
)

// CompilerVersion is the deterministic compiler revision recorded in the
// compile evidence: the same canonical inputs compiled by a different
// Compiler version are a different compilation.
const CompilerVersion = "cflow-compiler-1"

var nodeIDRE = regexp.MustCompile(nodeIDPattern)
var sha256HexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Compiler compiles approved Specs into the restricted Dynamic Workflow
// (stable interface ledger: private schema and limits).
type Compiler struct{}

// CompileRequest is the full canonical input set of one compilation
// (design 11): the approved Plan reference, the validated Spec bodies,
// the validated Verification Catalog body with its expected immutable
// reference, the optional Patch IR, the workflow identity and revision,
// and the user's hard limits. Max* limits of 0 mean "unlimited".
type CompileRequest struct {
	PlanRef        model.ArtifactRef
	WorkflowID     string
	Revision       int
	SpecBodies     [][]byte
	CatalogBody    []byte
	CatalogRef     model.ArtifactRef
	Patch          []byte
	MaxConcurrency int
	MaxTotalRuns   int
	MaxTaskTimeout int
}

// CompiledWorkflow is the canonical output of one compilation: the
// immutable body, its content hash, the compile evidence, and the inert
// Patch operations that were rejected with Compile Findings.
type CompiledWorkflow struct {
	Body        []byte
	Hash        string
	Evidence    Evidence
	RejectedOps []RejectedPatchOp
}

// RejectedPatchOp is one schema-valid Patch operation the Compiler
// skipped because it could not apply to this skeleton. The deterministic
// skeleton stands; the Finding remains visible in Dry Run.
type RejectedPatchOp struct {
	Op     string
	NodeID string
	Reason string
}

// Evidence is the compile's deterministic evidence: input hashes, the
// computed budgets, the parallel groups, the injected resource locks,
// and the applied Patch operations.
type Evidence struct {
	CompilerVersion   string
	PlanHash          string
	SpecHashes        []string
	CatalogHash       string
	TotalAgentRuns    int
	TotalRetries      int
	ParallelGroups    [][]string
	Locks             []LockAssignment
	PinnedRoutes      []RoutePin
	ConcurrencyCaps   []ConcurrencyCap
	BudgetTightenings []BudgetTightening
}

// LockAssignment binds one node to one Resource Lock.
type LockAssignment struct {
	NodeID string
	Lock   string
}

// RoutePin is one applied pin_route Patch operation.
type RoutePin struct {
	NodeID   string
	Provider string
}

// ConcurrencyCap is one applied reduce_concurrency Patch operation.
type ConcurrencyCap struct {
	NodeID      string
	MaxParallel int
}

// BudgetTightening is one applied tighten_budget Patch operation.
type BudgetTightening struct {
	NodeID string
	Budget float64
}

// Compile runs the deterministic compilation phases (design 11): schema
// validation, Spec dependency validation, the skeleton, final
// verification coverage, Patch validation and application, resource
// lock injection, budget and route capability validation, and canonical
// serialization with the content hash.
func (c *Compiler) Compile(ctx context.Context, req CompileRequest) (CompiledWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return CompiledWorkflow{}, err
	}

	// 1. Schema validation and typed parsing.
	specs, err := parseSpecs(req.SpecBodies)
	if err != nil {
		return CompiledWorkflow{}, err
	}
	catalog, err := parseCatalog(req.CatalogBody)
	if err != nil {
		return CompiledWorkflow{}, err
	}
	if req.CatalogRef.Revision != 0 && catalog.Revision != req.CatalogRef.Revision {
		return CompiledWorkflow{}, schemaInvalid(fmt.Sprintf(
			"catalog revision %d does not match the expected revision %d",
			catalog.Revision, req.CatalogRef.Revision))
	}
	if err := validateCatalog(catalog); err != nil {
		return CompiledWorkflow{}, err
	}

	// 2. Spec dependency validation.
	if err := validateDependencies(specs); err != nil {
		return CompiledWorkflow{}, err
	}

	// 3. Scope validation: overlapping write scopes need ordering or a
	// shared resource lock.
	if err := validateWriteScopes(specs); err != nil {
		return CompiledWorkflow{}, err
	}

	// 4. Acceptance and route validation.
	if err := validateAcceptance(specs, catalog); err != nil {
		return CompiledWorkflow{}, err
	}
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		return CompiledWorkflow{}, model.InvariantFault(err)
	}
	if err := validateRoutes(specs, reg); err != nil {
		return CompiledWorkflow{}, err
	}

	// 5. Hard budgets: the agent run total and retry total are
	// computable and must stay within the user's caps.
	totalRuns, totalRetries := 0, 0
	for _, s := range specs {
		totalRuns += 1 + s.MaxRetry
		totalRetries += s.MaxRetry
		if req.MaxTaskTimeout > 0 && s.TimeoutSeconds > req.MaxTaskTimeout {
			return CompiledWorkflow{}, model.NewFault(model.CodeBudgetExceeded,
				fmt.Sprintf("spec %s timeout %d exceeds the configured cap %d",
					s.ID, s.TimeoutSeconds, req.MaxTaskTimeout))
		}
	}
	if req.MaxTotalRuns > 0 && totalRuns > req.MaxTotalRuns {
		return CompiledWorkflow{}, model.NewFault(model.CodeBudgetExceeded,
			fmt.Sprintf("spec set requires %d agent runs, exceeding the cap %d", totalRuns, req.MaxTotalRuns))
	}

	// 6. The deterministic skeleton: one AgentTask/Verify/Merge chain per
	// Spec (canonically sorted), dependency preservation through merge
	// edges, and exactly one FinalVerify reachable from every merge.
	wf, err := buildSkeleton(specs, catalog, req.WorkflowID, req.Revision)
	if err != nil {
		return CompiledWorkflow{}, err
	}

	// 7. Parallel groups and the concurrency cap.
	groups := parallelGroups(wf)
	width := 0
	for _, g := range groups {
		if len(g) > width {
			width = len(g)
		}
	}
	if req.MaxConcurrency > 0 && width > req.MaxConcurrency {
		return CompiledWorkflow{}, model.NewFault(model.CodeBudgetExceeded,
			fmt.Sprintf("skeleton requires %d parallel tasks, exceeding the configured cap %d", width, req.MaxConcurrency))
	}

	// 8. Optional Patch IR validation and application.
	var rejected []RejectedPatchOp
	var pins []RoutePin
	var caps []ConcurrencyCap
	var tightenings []BudgetTightening
	if len(req.Patch) > 0 {
		rejected, pins, caps, tightenings, err = applyPatch(&wf, req.Patch, specs, groups, reg)
		if err != nil {
			return CompiledWorkflow{}, err
		}
	}

	// 9. Resource lock injection: every Merge Node holds the single
	// integration:<workflow-id> lock; every Task holds its Spec's locks.
	locks := injectLocks(specs, req.WorkflowID)

	// 10. Canonical serialization and hash.
	body, err := wf.canonicalBody()
	if err != nil {
		return CompiledWorkflow{}, model.InvariantFault(err)
	}
	ev := Evidence{
		CompilerVersion:   CompilerVersion,
		PlanHash:          req.PlanRef.Hash,
		SpecHashes:        specHashes(req.SpecBodies),
		CatalogHash:       sha256Hex(req.CatalogBody),
		TotalAgentRuns:    totalRuns,
		TotalRetries:      totalRetries,
		ParallelGroups:    groups,
		Locks:             locks,
		PinnedRoutes:      pins,
		ConcurrencyCaps:   caps,
		BudgetTightenings: tightenings,
	}
	return CompiledWorkflow{Body: body, Hash: sha256Hex(body), Evidence: ev, RejectedOps: rejected}, nil
}
