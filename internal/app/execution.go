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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"

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
	wrappers, err := verify.DiscoverWrappers(a.planningCWD(ctx, wf))
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
// Proposal promotion (PRD 已确认：Workflow-local Verification Command
// Catalog steps 2-3: the Spec Agent proposes new commands; CFlow
// validates each Proposal with the Catalog policy and writes the
// successor immutable Catalog Revision)
// ---------------------------------------------------------------------------

// proposalDoc is the structured proposed_commands section of the Spec
// Generation output (the spec prompt's output contract).
type proposalDoc struct {
	ProposedCommands []proposal `yaml:"proposed_commands"`
}

// proposal is one agent-proposed verification command.
type proposal struct {
	CommandID           string   `yaml:"command_id"`
	Executable          string   `yaml:"executable"`
	Args                []string `yaml:"args"`
	CWD                 string   `yaml:"cwd"`
	Purpose             string   `yaml:"purpose"`
	TimeoutSeconds      int      `yaml:"timeout_seconds"`
	ExpectedExitCodes   []int    `yaml:"expected_exit_codes"`
	MaxOutputBytes      int      `yaml:"max_output_bytes"`
	Env                 []string `yaml:"env"`
	TransientWritePaths []string `yaml:"transient_write_paths"`
}

// promoteCatalogProposals validates the Spec Generation Session's
// proposed commands with the Catalog policy and writes the successor
// immutable Catalog Revision (the Agent can never add directly to the
// executable Catalog). A Proposal whose project-relative wrapper is not
// fixed at the Base Commit, or that fails the command policy, is
// rejected and never enters the Catalog; the Compiler later rejects any
// Spec that references a rejected command id. Returns the successor
// reference, or the zero ref when no Proposal was accepted.
func (a *Application) promoteCatalogProposals(ctx context.Context, wf model.WorkflowID, output []byte, session model.SessionID) (model.ArtifactRef, error) {
	var doc proposalDoc
	if err := yaml.Unmarshal(output, &doc); err != nil || len(doc.ProposedCommands) == 0 {
		return model.ArtifactRef{}, nil
	}
	store, err := a.artifactStore(wf)
	if err != nil {
		return model.ArtifactRef{}, err
	}
	ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactCatalog})
	if err != nil {
		return model.ArtifactRef{}, err
	}
	body, err := store.Get(ctx, ref)
	if err != nil {
		return model.ArtifactRef{}, err
	}
	catalog, err := compile.ParseCatalog(body)
	if err != nil {
		return model.ArtifactRef{}, err
	}
	candidates := make([]verify.Candidate, 0, len(catalog.Entries)+len(doc.ProposedCommands))
	for _, e := range catalog.Entries {
		candidates = append(candidates, catalogEntryCandidate(e))
	}
	base := a.planningCWD(ctx, wf)
	accepted := 0
	for _, p := range doc.ProposedCommands {
		c := proposalCandidate(p, base)
		if err := verify.ValidateCandidate(c); err != nil {
			continue // rejected: never enters the executable Catalog
		}
		candidates = append(candidates, c)
		accepted++
	}
	if accepted == 0 {
		return model.ArtifactRef{}, nil
	}
	next := ref.Revision + 1
	out, err := verify.CatalogBody(next, candidates)
	if err != nil {
		return model.ArtifactRef{}, err
	}
	return store.Put(ctx, artifact.PutRequest{
		WorkflowID:    wf,
		Type:          model.ArtifactCatalog,
		Revision:      next,
		SchemaVersion: "1.0.0",
		CreatedAt:     a.now().UTC().Format(time.RFC3339),
		Producer:      artifact.ProducerRef{Purpose: string(model.PurposeSpecGeneration), SessionID: string(session)},
		Body:          out,
	})
}

// proposalCandidate maps one structured Proposal to a Candidate. The
// executable must be a project-relative wrapper fixed at the Base Commit
// (hashed from Base; unbound sources are rejected by the policy).
func proposalCandidate(p proposal, base string) verify.Candidate {
	c := verify.Candidate{
		CommandID:           p.CommandID,
		Purpose:             verify.Purpose(p.Purpose),
		ExecutableKind:      verify.KindProjectRelative,
		Executable:          p.Executable,
		Args:                append([]string(nil), p.Args...),
		CWD:                 p.CWD,
		TimeoutSeconds:      p.TimeoutSeconds,
		ExpectedExitCodes:   append([]int(nil), p.ExpectedExitCodes...),
		OutputLimitBytes:    p.MaxOutputBytes,
		Env:                 append([]string(nil), p.Env...),
		TransientWritePaths: append([]string(nil), p.TransientWritePaths...),
	}
	if c.CWD == "" {
		c.CWD = "."
	}
	if !pathEscapesBase(c.Executable) {
		if data, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(c.Executable))); err == nil {
			c.SHA256 = sha256Hex(data)
			c.Source = fmt.Sprintf("agent-proposal:%s@sha256:%s", c.Executable, c.SHA256)
			return c
		}
	}
	// Unbound source: the wrapper does not exist at the Base Commit (or
	// the path escapes it); an empty executable fails the policy.
	c.Executable = ""
	return c
}

// catalogEntryCandidate round-trips one immutable Catalog entry into a
// Candidate so the successor Revision preserves every existing entry.
func catalogEntryCandidate(e compile.CatalogEntry) verify.Candidate {
	kind := verify.KindProjectRelative
	if filepath.IsAbs(e.Executable) {
		kind = verify.KindPathExecutable
	}
	return verify.Candidate{
		CommandID:           e.CommandID,
		Purpose:             verify.Purpose(e.Purpose),
		ExecutableKind:      kind,
		Executable:          e.Executable,
		SHA256:              sourceHash(e.Source),
		Args:                append([]string(nil), e.Args...),
		CWD:                 e.CWD,
		TimeoutSeconds:      e.TimeoutSeconds,
		ExpectedExitCodes:   append([]int(nil), e.ExpectedExitCodes...),
		OutputLimitBytes:    e.MaxOutputBytes,
		Env:                 append([]string(nil), e.Env...),
		TransientWritePaths: append([]string(nil), e.TransientWritePaths...),
		Source:              e.Source,
	}
}

// pathEscapesBase reports whether a project-relative path leaves the
// managed repository root.
func pathEscapesBase(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
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
	// The Spec Artifact body carries the spec set (one Spec object or a
	// non-empty sequence); every Spec is compiled (Task 12, multi-Spec
	// pipeline).
	specBodies, err := splitSpecSetBody(specBody)
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
		SpecBodies:     specBodies,
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
	// The applied Patch operations are compile evidence: the Kernel
	// records them as non-blocking Findings so the Dry Run and the
	// Execution Approval gate display exactly what the user approves.
	applied := make([]string, 0,
		len(out.Evidence.PinnedRoutes)+len(out.Evidence.ConcurrencyCaps)+len(out.Evidence.BudgetTightenings))
	for _, p := range out.Evidence.PinnedRoutes {
		applied = append(applied, fmt.Sprintf("pin_route %s -> %s", p.NodeID, p.Provider))
	}
	for _, c := range out.Evidence.ConcurrencyCaps {
		applied = append(applied, fmt.Sprintf("reduce_concurrency %s -> %d", c.NodeID, c.MaxParallel))
	}
	for _, t := range out.Evidence.BudgetTightenings {
		applied = append(applied, fmt.Sprintf("tighten_budget %s -> %v", t.NodeID, t.Budget))
	}
	return model.EffectResultInput{Kind: model.WorkflowCompiled, Body: out.Body, RejectedOps: rejected, AppliedOps: applied}, nil
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
		ChangeSetHash:    facts.ChangeSetHash,
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
	// The approved Spec set: one route preview per Spec, and the Budget
	// previews of every agent_task Node bound to its Spec's approved
	// budget (Task 12 multi-Spec pipeline).
	specBodies, err := splitSpecSetBody(specBody)
	if err != nil {
		return nil, err
	}
	specs := make([]compile.Spec, 0, len(specBodies))
	for _, body := range specBodies {
		spec, err := compile.ParseSpec(body)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
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

	// Routes and budgets come from the approved Specs; the parallel
	// groups come from the compiled DAG.
	for _, s := range specs {
		if s.Route != nil {
			pv.Routes = append(pv.Routes, RoutePreview{
				NodeID: "task-" + s.ID, Provider: s.Route.Provider, Model: s.Route.Model,
			})
		}
	}
	specByID := map[string]compile.Spec{}
	for _, s := range specs {
		specByID[s.ID] = s
	}
	for _, n := range wfIR.Nodes {
		if n.Type != "agent_task" {
			continue
		}
		budget := 0.0
		if spec, ok := specByID[n.SpecID]; ok && spec.Route != nil {
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
		"planning snapshot: " + a.planningCWD(ctx, wf),
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
