package app

// The execution lifecycle's Application-side machinery (Task 11):
// Verification Catalog discovery and the immutable Catalog Revision
// write, the Git Commit Preflight observation and report write, the
// Workflow Compilation effect, the Integration Worktree creation after
// the Execution Approval, and the Execution Approval preview projection
// (PRD 已确认：两个用户批准门, PRD 已确认：Workflow-local Verification
// Command Catalog). Same-package split of the Application seam: no
// public seam added.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/compile"
	"cflow.local/cflow/internal/config"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/store"
	"cflow.local/cflow/internal/verify"
)

// ---------------------------------------------------------------------------
// Verification Catalog assembly (Runtime-authored, immutable)
// ---------------------------------------------------------------------------

// assembleCatalog discovers the Verification Command candidates from the
// fixed Base Commit (the Planning Snapshot Worktree) and the PATH
// executables, validates each with the Catalog policy, and writes the
// next immutable Catalog Revision through the Artifact Store. Discovery
// only produces Candidates: the written Revision is what an Execution
// Approval may bind (design 16.1).
func (a *Application) assembleCatalog(ctx context.Context, wf model.WorkflowID, session model.SessionID) (model.ArtifactRef, error) {
	wrappers, err := verify.DiscoverWrappers(a.planningWorktreePath(wf))
	if err != nil {
		return model.ArtifactRef{}, err
	}
	pathExecs, err := verify.DiscoverPathExecutables()
	if err != nil {
		return model.ArtifactRef{}, err
	}
	candidates := append(wrappers, pathExecs...)
	valid := make([]verify.Candidate, 0, len(candidates))
	for _, c := range candidates {
		if err := verify.ValidateCandidate(c); err == nil {
			valid = append(valid, c)
		}
	}
	store, err := a.artifactStore(wf)
	if err != nil {
		return model.ArtifactRef{}, err
	}
	revision := 1
	if latest, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactCatalog}); err == nil {
		revision = latest.Revision + 1
	}
	body, err := verify.CatalogBody(revision, valid)
	if err != nil {
		return model.ArtifactRef{}, err
	}
	return store.Put(ctx, artifact.PutRequest{
		WorkflowID:    wf,
		Type:          model.ArtifactCatalog,
		Revision:      revision,
		SchemaVersion: "1.0.0",
		CreatedAt:     a.now().UTC().Format(time.RFC3339),
		Producer:      artifact.ProducerRef{Purpose: string(model.PurposeSpecGeneration), SessionID: string(session)},
		Body:          body,
	})
}

// ---------------------------------------------------------------------------
// Git Commit Preflight (PRD 已确认：Git Commit Identity 与 Signing
// Preflight)
// ---------------------------------------------------------------------------

// observePreflight runs the Commit Preflight and writes the immutable
// preflight report Artifact. The evidence is observed before the Dry Run
// decision; the Kernel records the exact row revision in the same
// transaction that pauses the gate.
func (a *Application) observePreflight(ctx context.Context, wf model.WorkflowID, nextRevision int) (model.PreflightFacts, error) {
	if a.git == nil {
		return model.PreflightFacts{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	res, err := a.git.Execute(ctx, gitflow.CommitPreflight{
		Revision: fmt.Sprintf("preflight-%d", nextRevision),
	})
	if err != nil {
		return model.PreflightFacts{}, err
	}
	ev, ok := res.(gitflow.PreflightEvidence)
	if !ok {
		return model.PreflightFacts{}, model.InvariantFault(fmt.Errorf("preflight result has an unexpected type"))
	}
	report, err := json.Marshal(ev)
	if err != nil {
		return model.PreflightFacts{}, model.InvariantFault(fmt.Errorf("preflight evidence cannot be serialized"))
	}
	store, err := a.artifactStore(wf)
	if err != nil {
		return model.PreflightFacts{}, err
	}
	ref, err := store.Put(ctx, artifact.PutRequest{
		WorkflowID:    wf,
		Type:          model.ArtifactReport,
		Revision:      nextRevision,
		SchemaVersion: "1.0.0",
		CreatedAt:     a.now().UTC().Format(time.RFC3339),
		Producer:      artifact.ProducerRef{Purpose: "preflight"},
		Body:          report,
	})
	if err != nil {
		return model.PreflightFacts{}, err
	}
	identityJSON, _ := json.Marshal(ev.Author)
	signingJSON, _ := json.Marshal(ev.Signing)
	return model.PreflightFacts{
		EvidenceHash:      ref.Hash,
		GitVersion:        ev.GitVersion,
		RepositoryContext: "repository:" + a.project.Key,
		Fingerprint:       ev.PolicyFingerprint,
		IdentityJSON:      string(identityJSON),
		SigningPolicyJSON: string(signingJSON),
		ProbeStatus:       probeStatus(ev.Probe),
		ProbeRequired:     ev.Probe.Required,
		ProbeSuccess:      ev.Probe.Success,
		ArtifactPath:      fmt.Sprintf("%s/%d/%s", model.ArtifactReport, nextRevision, ref.Hash),
	}, nil
}

// probeStatus renders the Probe outcome as the immutable row status.
func probeStatus(p gitflow.ProbeFacts) string {
	if !p.Ran {
		return "NOT_RUN"
	}
	if p.Success {
		return "PASS"
	}
	return "FAIL"
}

// ---------------------------------------------------------------------------
// Workflow Compilation effect (design 11)
// ---------------------------------------------------------------------------

// workflowCompile runs the deterministic Compiler over the approved
// Spec, the active Verification Catalog Revision, and the validated
// Patch IR. The compiled canonical body and the inert rejected Patch
// operations are returned as the immutable Result evidence.
func (a *Application) workflowCompile(ctx context.Context, wf model.WorkflowID, intent model.WorkflowCompileIntent) (model.EffectResultInput, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	resolve := func(typ model.ArtifactType) (model.ArtifactRef, []byte, error) {
		ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: typ})
		if err != nil {
			return model.ArtifactRef{}, nil, err
		}
		body, err := store.Get(ctx, ref)
		if err != nil {
			return model.ArtifactRef{}, nil, err
		}
		return ref, body, nil
	}
	planRef, _, err := resolve(model.ArtifactPlan)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	_, specBody, err := resolve(model.ArtifactSpec)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	catalogRef, catalogBody, err := resolve(model.ArtifactCatalog)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	workflowRef, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactWorkflow})
	revision := 1
	if err == nil {
		revision = workflowRef.Revision + 1
	}
	out, err := (&compile.Compiler{}).Compile(ctx, compile.CompileRequest{
		PlanRef:        planRef,
		WorkflowID:     string(wf),
		Revision:       revision,
		SpecBodies:     [][]byte{specBody},
		CatalogBody:    catalogBody,
		CatalogRef:     catalogRef,
		Patch:          intent.PatchBody,
		MaxConcurrency: a.concurrencyCap(),
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	rejected := make([]string, 0, len(out.RejectedOps))
	for _, op := range out.RejectedOps {
		rejected = append(rejected, fmt.Sprintf("%s %s: %s", op.Op, op.NodeID, op.Reason))
	}
	return model.EffectResultInput{Kind: model.WorkflowCompiled, Body: out.Body, RejectedOps: rejected}, nil
}

// concurrencyCap is the user's configured concurrency bound (PRD 并发上
// 限: 不超过用户配置). The safe default is serial execution, matching the
// config package's built-in default.
func (a *Application) concurrencyCap() int {
	file, err := config.Load(filepath.Join(a.home, "config.yaml"))
	if err != nil {
		return 1
	}
	resolved, err := config.Resolve(file, config.Overrides{})
	if err != nil {
		return 1
	}
	return resolved.Concurrency
}

// ---------------------------------------------------------------------------
// Integration Worktree creation (PRD Worktree 策略: only after the
// Execution Approval)
// ---------------------------------------------------------------------------

// integrationWorktreeCreate creates the Integration Branch/Worktree from
// the recorded Base Commit. The branch must not already exist; the
// expected HEAD is fixed before the Effect (design 6.2 rule 6).
func (a *Application) integrationWorktreeCreate(ctx context.Context, intent model.IntegrationWorktreeCreateIntent) (model.EffectResultInput, error) {
	if a.git == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	path := filepath.Join(a.home, "worktrees", a.project.Key, string(intent.Workflow), "integration")
	if err := a.ensureWorktreeParent(path); err != nil {
		return model.EffectResultInput{}, err
	}
	res, err := a.git.Execute(ctx, gitflow.CreateIntegration{
		Branch:     "cflow/" + string(intent.Workflow) + "/integration",
		BaseCommit: intent.BaseCommit,
		Path:       path,
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	iv, ok := res.(gitflow.IntegrationWorktreeResult)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("integration worktree result has an unexpected type"))
	}
	return model.EffectResultInput{Kind: model.IntegrationWorktreeCreated, IntegrationHead: iv.Head}, nil
}

// ---------------------------------------------------------------------------
// Execution Approval preview projection (the Dry Run display)
// ---------------------------------------------------------------------------

// queryExecutionPreview assembles the full Execution Approval preview
// from the aggregate facts and the immutable Artifact bodies. The view
// carries exactly the references the Approval compare-and-swap binds.
func (a *Application) queryExecutionPreview(ctx context.Context, q ExecutionPreviewQuery) (View, error) {
	wf, err := a.resolveQueryWorkflow(q.Workflow)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if wf == "" {
		return nil, model.InvalidInputFault("no workflow")
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if view.State.Workflow.ID == "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(wf))
	}
	facts := view.State.Workflow.ExecutionFacts
	if facts == nil || facts.PlanHash == "" || len(facts.SpecHashes) == 0 ||
		facts.CatalogHash == "" || facts.WorkflowHash == "" {
		return nil, model.InvalidInputFault("execution inputs are incomplete; no preview is available")
	}
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, err
	}
	resolve := func(typ model.ArtifactType) (model.ArtifactRef, error) {
		return store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: typ})
	}
	planRef, err := resolve(model.ArtifactPlan)
	if err != nil {
		return nil, err
	}
	specRef, err := resolve(model.ArtifactSpec)
	if err != nil {
		return nil, err
	}
	catalogRef, err := resolve(model.ArtifactCatalog)
	if err != nil {
		return nil, err
	}
	workflowRef, err := resolve(model.ArtifactWorkflow)
	if err != nil {
		return nil, err
	}

	pv := ExecutionPreviewView{
		Workflow:         wf,
		Stage:            view.State.Workflow.Stage,
		Runtime:          view.State.Workflow.Runtime,
		Plan:             &planRef,
		Spec:             &specRef,
		Catalog:          &catalogRef,
		WorkflowArtifact: &workflowRef,
		PlanHash:         facts.PlanHash,
		SpecHashes:       append([]string(nil), facts.SpecHashes...),
		CatalogHash:      facts.CatalogHash,
		WorkflowHash:     facts.WorkflowHash,
		RoutingHash:      facts.RoutingHash,
		BudgetHash:       facts.BudgetHash,
		CommitPolicyHash: facts.CommitPolicyHash,
		Findings:         view.State.Findings,
	}
	if facts.CommitPolicyHash != "" {
		pv.Preflight = &PreflightPreview{
			Revision:     facts.PreflightRevision,
			EvidenceHash: facts.CommitPolicyHash,
			Fingerprint:  facts.Fingerprint,
		}
	}

	specBody, err := store.Get(ctx, specRef)
	if err != nil {
		return nil, err
	}
	spec, err := compile.ParseSpec(specBody)
	if err != nil {
		return nil, err
	}
	catalogBody, err := store.Get(ctx, catalogRef)
	if err != nil {
		return nil, err
	}
	catalog, err := compile.ParseCatalog(catalogBody)
	if err != nil {
		return nil, err
	}
	workflowBody, err := store.Get(ctx, workflowRef)
	if err != nil {
		return nil, err
	}
	wfIR, err := compile.ParseWorkflow(workflowBody)
	if err != nil {
		return nil, err
	}

	// Routes and budgets come from the approved Spec; the parallel
	// groups come from the compiled DAG.
	if spec.Route != nil {
		pv.Routes = append(pv.Routes, RoutePreview{
			NodeID: "task-" + spec.ID, Provider: spec.Route.Provider, Model: spec.Route.Model,
		})
	}
	for _, n := range wfIR.Nodes {
		if n.Type != "agent_task" {
			continue
		}
		budget := 0.0
		if spec.Route != nil {
			budget = spec.Route.Budget
		}
		pv.Budgets = append(pv.Budgets, BudgetPreview{
			NodeID: n.ID, TimeoutSeconds: n.TimeoutSeconds, MaxRetry: n.MaxRetry, Budget: budget,
		})
		pv.TotalAgentRuns += 1 + n.MaxRetry
		pv.TotalRetries += n.MaxRetry
	}
	for _, n := range wfIR.Nodes {
		if n.Type == "merge" {
			pv.Locks = append(pv.Locks, LockPreview{NodeID: n.ID, Lock: "integration:" + string(wf)})
		}
	}
	pv.ParallelGroups = compile.ParallelGroups(wfIR)

	// Command identities: every Catalog entry's pinned executable and
	// hash, so the Dry Run shows exactly what an Approval binds.
	for _, e := range catalog.Entries {
		pv.CommandIdentities = append(pv.CommandIdentities, CommandIdentity{
			CommandID: e.CommandID, Executable: e.Executable,
			SHA256: sourceHash(e.Source), Purpose: e.Purpose,
		})
	}

	// The Worktree plan: the Integration Branch is withheld until the
	// Execution Approval; task worktrees are created at readiness from
	// the verified Integration HEAD (PRD Worktree 策略).
	integrationPath := filepath.Join(a.home, "worktrees", a.project.Key, string(wf), "integration")
	pv.WorktreePlan = []string{
		"integration branch: cflow/" + string(wf) + "/integration",
		"integration worktree: " + integrationPath,
		"planning snapshot: " + a.planningWorktreePath(wf),
		"task worktrees: created at readiness from the verified integration head",
	}

	// The Provider default-permission trust boundary (PRD 约束 323):
	// agents run with the provider's default permissions and the user's
	// existing provider configuration; CFlow does not provide a sandbox
	// guarantee.
	reg, err := agent.LoadProviderRegistry()
	if err == nil {
		pv.TrustBoundary = fmt.Sprintf(
			"agents run with the provider's default permissions and the user's existing provider configuration; CFlow provides no sandbox guarantee (cflow %s, registry revision %s)",
			observe.Version, reg.Revision())
	}
	return pv, nil
}

// sourceHash extracts the pinned sha256 from a Catalog source identity
// ("<kind>:<path>@sha256:<hex>"); "" when the identity carries none.
func sourceHash(source string) string {
	idx := strings.LastIndexByte(source, '@')
	if idx < 0 {
		return ""
	}
	suffix := source[idx+1:]
	if len(suffix) > len("sha256:") && suffix[:len("sha256:")] == "sha256:" {
		return suffix[len("sha256:"):]
	}
	return ""
}
