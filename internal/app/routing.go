package app

// The immutable routing and budget resolution of one Execution Approval
// (Task 16, design 14.2/20.1, PRD 已确认：Agent 交互主协议): the per-Purpose
// ordered approved Provider bindings with the approved model, budget,
// timeout, prompt hash, Provider default-permission disclosure, and the
// observed executable identity facts. Resolved values come from the
// strict CFLOW_HOME/config.yaml resolution (Task 1) or the explicit
// Spec routes, and become immutable approval inputs: the routing-policy
// and budget-policy Artifacts bind them by hash, and the dispatch drift
// gate re-resolves the content (never re-detecting) before the CAS
// pre-pass re-detects the executable identity. Same-package split of the
// Application seam: no public seam added.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/claude"
	"cflow.local/cflow/internal/agent/codex"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/compile"
	"cflow.local/cflow/internal/config"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// providerTrustBoundary is the Provider default-permission disclosure
// every routing binding records and the Dry Run displays (PRD 约束 323):
// agents run with the provider's default permissions and the user's
// existing provider configuration; CFlow provides no sandbox guarantee.
const providerTrustBoundary = "agents run with the provider's default permissions and the user's existing provider configuration; CFlow provides no sandbox guarantee"

// routedPurposes is the canonical Purpose order of an execution routing
// policy: the Implementer lineage, then the independent Reviewer lineage
// (the Reviewer Session of a Task runs on the Task's approved route,
// design 16.2), then the independent Repair lineage (a DIRTY_TASK_WORKTREE
// successor runs on the Task's approved route with the repair purpose,
// PRD 已确认：DIRTY_TASK_WORKTREE 原地 Repair), then the independent Final
// Reviewer lineage (Task 18: the Final Reviewer Session of the final
// acceptance runs on the approved route set).
var routedPurposes = []model.AgentPurpose{
	model.PurposeImplementation, model.PurposeReview, model.PurposeRepair,
	model.PurposeFinalVerification,
}

// resolvedConfig loads and resolves the strict local configuration
// (design 20.1). An absent configuration file means "no configuration":
// the embedded safe defaults apply. A present but broken file fails
// every routing decision closed: routing/budget values are approval
// inputs, never guessed.
func (a *Application) resolvedConfig() (config.Resolved, error) {
	path := filepath.Join(a.home, "config.yaml")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return config.Resolve(config.File{}, config.Overrides{})
		}
		return config.Resolved{}, err
	}
	file, err := config.Load(path)
	if err != nil {
		return config.Resolved{}, err
	}
	return config.Resolve(file, config.Overrides{})
}

// resolveRoutingInputs resolves the immutable routing policy of one
// workflow's Execution Approval from the approved Spec set, the strict
// local configuration, and the Provider Registry: every routed Purpose
// binds the ordered approved Providers — the routed Providers in Spec
// order first, then the configured fallbacks — each with the approved
// model, budget, timeout, prompt hash, and disclosure. detect=true runs
// the read-only version probe of every referenced executable once (the
// Dry Run; detection results are shared across the Purposes that bind
// the same Provider) and pins the observed identity facts. detect=false
// re-resolves the same content without detection: the dispatch drift
// gate compares content first, and the CAS pre-pass re-detects the
// executable identity afterwards.
func (a *Application) resolveRoutingInputs(ctx context.Context, wf model.WorkflowID, detect bool) (*agent.RoutingPolicySet, error) {
	if a.agent.Registry == nil {
		return nil, model.InvalidInputFault("routing requires the provider registry")
	}
	cfg, err := a.resolvedConfig()
	if err != nil {
		return nil, err
	}
	specs, err := a.approvedSpecs(ctx, wf)
	if err != nil {
		return nil, err
	}
	res := &routingResolution{a: a, cfg: cfg, detect: detect}
	if detect {
		res.installs = map[string]agent.Installation{}
	}
	set := &agent.RoutingPolicySet{
		ConfigModel:     cfg.Model,
		ConfigFallbacks: append([]string(nil), cfg.Fallbacks...),
	}
	for _, purpose := range routedPurposes {
		pol, err := res.purposePolicy(ctx, purpose, specs)
		if err != nil {
			return nil, err
		}
		set.Policies = append(set.Policies, pol)
	}
	return set, nil
}

// routingResolution carries the shared state of one routing resolution
// pass: the resolved configuration and the per-Provider detection cache
// (the Dry Run detects each referenced executable exactly once).
type routingResolution struct {
	a        *Application
	cfg      config.Resolved
	detect   bool
	installs map[string]agent.Installation
}

// installation returns the detected Installation of one Provider: the
// pass's cached result when one exists, else a fresh Detect ("" facts
// when the pass does not detect — the dispatch content gate).
func (r *routingResolution) installation(ctx context.Context, provider string) (agent.Installation, error) {
	if !r.detect {
		return agent.Installation{}, nil
	}
	if inst, ok := r.installs[provider]; ok {
		return inst, nil
	}
	ad := r.a.agent.Adapters[provider]
	if ad == nil {
		return agent.Installation{}, model.InvalidInputFault("no adapter bound for provider " + provider)
	}
	inst, err := ad.Detect(ctx)
	if err != nil {
		return agent.Installation{}, err
	}
	r.installs[provider] = inst
	return inst, nil
}

// approvedSpecs parses the active Spec set of one workflow.
func (a *Application) approvedSpecs(ctx context.Context, wf model.WorkflowID) ([]compile.Spec, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, err
	}
	ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactSpec})
	if err != nil {
		return nil, model.InvalidInputFault("routing requires the approved spec set")
	}
	body, err := store.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	return a.parseSpecSet(body)
}

// purposePolicy builds one Purpose's ordered approved binding list: the
// distinct routed Providers in Spec order, then the configured fallback
// Providers (deduplicated: a Provider is bound once per Purpose).
func (r *routingResolution) purposePolicy(ctx context.Context, purpose model.AgentPurpose, specs []compile.Spec) (agent.RoutingPolicy, error) {
	prompt, ok := r.a.promptForPurpose(purpose)
	if !ok {
		return agent.RoutingPolicy{}, model.InvalidInputFault(
			"no embedded prompt for the routed purpose " + purpose.String())
	}
	var providers []string
	for _, s := range specs {
		if s.Route == nil {
			continue
		}
		if !containsString(providers, s.Route.Provider) {
			providers = append(providers, s.Route.Provider)
		}
	}
	for _, f := range r.cfg.Fallbacks {
		if !containsString(providers, f) {
			providers = append(providers, f)
		}
	}
	pol := agent.RoutingPolicy{Purpose: purpose}
	for _, provider := range providers {
		b, err := r.routeBinding(ctx, provider, specs, prompt.Body)
		if err != nil {
			return agent.RoutingPolicy{}, err
		}
		pol.Bindings = append(pol.Bindings, b)
	}
	if len(pol.Bindings) == 0 {
		return agent.RoutingPolicy{}, model.InvalidInputFault(
			"no approved route for purpose " + purpose.String())
	}
	return pol, nil
}

// routeBinding resolves one approved Provider binding of a Purpose
// route. The route's model, budget, and timeout are the approved Spec's
// explicit route values of the first Spec routed to the Provider; the
// resolved config default model fills an unspecified route model, and
// the configured hard cap bounds a route without an explicit budget
// (design 20.1). The binding is proven through the same
// Compare-and-Swap path every operation runs: at the Dry Run with the
// observed executable identity facts, at dispatch without detection
// (the synthetic installation carries exactly the registry facts, so
// the CAS passes unless the binding lacks a capability or the registry
// revision drifted).
func (r *routingResolution) routeBinding(ctx context.Context, provider string, specs []compile.Spec, prompt string) (agent.RouteBinding, error) {
	reg := r.a.agent.Registry
	entry, err := reg.Select(provider)
	if err != nil {
		return agent.RouteBinding{}, model.NewFault(model.CodeProviderProtocolUnsupported,
			"route provider cannot be selected: "+err.Error())
	}
	routeModel, budgetUSD, timeout := "", 0.0, 0
	for _, s := range specs {
		if s.Route == nil || s.Route.Provider != provider {
			continue
		}
		routeModel = s.Route.Model
		budgetUSD = s.Route.Budget
		timeout = s.TimeoutSeconds
		break
	}
	if routeModel == "" {
		routeModel = r.cfg.Model
	}
	if budgetUSD == 0 && r.cfg.MaxUSDPerRun > 0 {
		budgetUSD = r.cfg.MaxUSDPerRun
	}
	rb := agent.RouteBinding{
		Provider:           provider,
		Model:              routeModel,
		BudgetUSD:          budgetUSD,
		TimeoutSeconds:     timeout,
		PromptHash:         agent.PromptHash(prompt),
		Disclosure:         providerTrustBoundary,
		DialectID:          entry.Dialect.ID,
		RegistryRevision:   entry.Revision,
		StartCapabilities:  append([]string(nil), entry.StartCapabilities...),
		ResumeCapabilities: append([]string(nil), entry.ResumeCapabilities...),
	}
	inst, err := r.installation(ctx, provider)
	if err != nil {
		return agent.RouteBinding{}, err
	}
	if !r.detect {
		// The no-detect re-resolution proves the same start-capability
		// and protocol facts against the registry: the synthetic
		// installation carries exactly the registry facts, so the CAS
		// passes unless the binding lacks a capability or the registry
		// revision drifted.
		inst.Compatibility = agent.CompatibilitySupported
		inst.DialectID = entry.Dialect.ID
		inst.RegistryRevision = entry.Revision
	}
	if err := agent.CompareInstallation(inst, rb, false); err != nil {
		return agent.RouteBinding{}, err
	}
	rb.ExecutablePath = inst.ExecutablePath
	rb.ExecutableSHA256 = inst.ExecutableSHA256
	rb.CLIVersion = inst.CLIVersion
	return rb, nil
}

// budgetPolicyBody is the canonical budget-policy Artifact body: the
// configured hard cap and the per-routed-node approved budgets (design
// 20.1). The body is fully deterministic from the Spec set and the
// configuration, so the dispatch drift gate re-resolves it without
// detection and compares the content.
type budgetPolicyBody struct {
	SchemaVersion int `json:"schema_version"`
	// MaxUSDPerRun is the approved hard budget cap of one Agent run in
	// USD (0 = no cap).
	MaxUSDPerRun float64 `json:"max_usd_per_run"`
	// Nodes are the approved per-routed-node budgets in Spec order.
	Nodes []budgetPolicyNode `json:"nodes"`
}

// budgetPolicyNode is one routed Node's approved budget facts.
type budgetPolicyNode struct {
	Node           string  `json:"node"`
	BudgetUSD      float64 `json:"budget_usd"`
	TimeoutSeconds int     `json:"timeout_seconds"`
	MaxRetry       int     `json:"max_retry"`
}

// resolveBudgetInputs builds the immutable budget-policy body of one
// Execution Approval from the approved Spec set and the strict local
// configuration.
func (a *Application) resolveBudgetInputs(ctx context.Context, wf model.WorkflowID) ([]byte, error) {
	cfg, err := a.resolvedConfig()
	if err != nil {
		return nil, err
	}
	specs, err := a.approvedSpecs(ctx, wf)
	if err != nil {
		return nil, err
	}
	body := budgetPolicyBody{SchemaVersion: 1, MaxUSDPerRun: cfg.MaxUSDPerRun}
	for _, s := range specs {
		budget := 0.0
		if s.Route != nil {
			budget = s.Route.Budget
			if budget == 0 && cfg.MaxUSDPerRun > 0 {
				budget = cfg.MaxUSDPerRun
			}
		}
		body.Nodes = append(body.Nodes, budgetPolicyNode{
			Node:           "task-" + s.ID,
			BudgetUSD:      budget,
			TimeoutSeconds: s.TimeoutSeconds,
			MaxRetry:       s.MaxRetry,
		})
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, model.InvariantFault(fmt.Errorf("budget policy cannot be serialized"))
	}
	return data, nil
}

// writeRoutingPolicies resolves the routing and budget policies of one
// Execution Dry Run and writes them as the next immutable Artifact
// Revisions (the Execution Approval binds their hashes, design 20.1). An
// unapproved route or fallback Provider fails the Dry Run closed.
func (a *Application) writeRoutingPolicies(ctx context.Context, wf model.WorkflowID) (routingRef, budgetRef model.ArtifactRef, err error) {
	set, err := a.resolveRoutingInputs(ctx, wf, true)
	if err != nil {
		return model.ArtifactRef{}, model.ArtifactRef{}, err
	}
	body, err := agent.MarshalRoutingPolicySet(set)
	if err != nil {
		return model.ArtifactRef{}, model.ArtifactRef{}, err
	}
	budgetBody, err := a.resolveBudgetInputs(ctx, wf)
	if err != nil {
		return model.ArtifactRef{}, model.ArtifactRef{}, err
	}
	store, err := a.artifactStore(wf)
	if err != nil {
		return model.ArtifactRef{}, model.ArtifactRef{}, err
	}
	next := func(typ model.ArtifactType) int {
		if ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: typ}); err == nil {
			return ref.Revision + 1
		}
		return 1
	}
	ts := a.now().UTC().Format(time.RFC3339)
	put := func(typ model.ArtifactType, payload []byte) (model.ArtifactRef, error) {
		return store.Put(ctx, artifact.PutRequest{
			WorkflowID:    wf,
			Type:          typ,
			Revision:      next(typ),
			SchemaVersion: "1.0.0",
			CreatedAt:     ts,
			Producer:      artifact.ProducerRef{Purpose: "execution-dry-run"},
			Body:          payload,
		})
	}
	routingRef, err = put(model.ArtifactRoutingPolicy, body)
	if err != nil {
		return model.ArtifactRef{}, model.ArtifactRef{}, err
	}
	budgetRef, err = put(model.ArtifactBudgetPolicy, budgetBody)
	if err != nil {
		return model.ArtifactRef{}, model.ArtifactRef{}, err
	}
	return routingRef, budgetRef, nil
}

// managedCodingSchema is the managed immutable output schema of a coding
// Session (design 14.5): the structured terminal result is a JSON
// object, so the minimal object schema proves the structured contract to
// the dialect CLIs (the same materialization the adapter tests drive).
const managedCodingSchema = `{"type":"object"}`

// providerTypedInput decorates a Session input with the typed Adapter
// facts of the routed Provider (design 14.5): the managed immutable
// output schema and the approved model/budget of the binding the
// Execution Approval bound. The Fake Adapter accepts any input; the real
// dialect Adapters refuse to launch without their typed facts.
//
// providerTypedInput replaces the base input with the typed Adapter
// input, so the immutable redacted Context Bundle handoff of an
// automatic fallback rides the typed input as a reference: a production
// codex→claude successor's recorded input facts carry the handoff (Task
// 16 ledger obligation; the typed inputs carry the bundle path, never a
// credential or an unredacted transcript).
func (a *Application) providerTypedInput(ctx context.Context, rt *agent.Runtime, purpose model.AgentPurpose, provider string, base any) any {
	if rt == nil {
		return base
	}
	rb, ok := rt.RouteBinding(purpose, provider)
	if !ok {
		return base
	}
	bundleRef := contextBundleRefOf(base)
	switch provider {
	case "codex":
		return codex.Input{
			SchemaPath:       a.managedSchemaPath(ctx),
			Model:            rb.Model,
			ContextBundleRef: bundleRef,
		}
	case "claude":
		return claude.Input{
			SchemaJSON:       managedCodingSchema,
			MaxBudgetUSD:     strconv.FormatFloat(rb.BudgetUSD, 'f', -1, 64),
			Model:            rb.Model,
			ContextBundleRef: bundleRef,
		}
	}
	return base
}

// contextBundleRefOf extracts the immutable Context Bundle handoff
// reference of a Session start input ("" when the input carries none).
func contextBundleRefOf(base any) string {
	if c, ok := base.(*codingSessionInput); ok && c.ContextBundle != nil {
		return c.ContextBundle.Path
	}
	return ""
}

// managedSchemaPath materializes the managed immutable output schema
// file under CFLOW_HOME (design 14.5: the Application owns the managed
// schema file; the dialect CLI receives its absolute path) and returns
// it. The write is idempotent and never modifies an existing file.
func (a *Application) managedSchemaPath(ctx context.Context) string {
	dir := filepath.Join(a.home, "schemas")
	if err := security.CreateSensitiveDir(dir); err != nil {
		return ""
	}
	path := filepath.Join(dir, "coding-output.json")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if err := os.WriteFile(path, []byte(managedCodingSchema), 0o600); err != nil {
		return ""
	}
	return path
}

// approvedRoutingPolicy parses the routing-policy Artifact the Execution
// Approval bound.
func (a *Application) approvedRoutingPolicy(ctx context.Context, wf model.WorkflowID) (*agent.RoutingPolicySet, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, err
	}
	ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactRoutingPolicy})
	if err != nil {
		return nil, model.InvalidInputFault("no routing policy is bound; re-run the execution dry run")
	}
	body, err := store.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	return agent.ParseRoutingPolicySet(body)
}

// containsString reports whether a slice contains a value.
func containsString(have []string, want string) bool {
	for _, h := range have {
		if h == want {
			return true
		}
	}
	return false
}
