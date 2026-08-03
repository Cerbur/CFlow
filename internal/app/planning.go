package app

// The planning lifecycle's Application-side machinery (Task 10):
// project discovery through GitFlow, the per-command Agent Runtime, the
// per-workflow Artifact Store, and the workflow.yaml static identity
// manifest (PRD Workflow 元信息). Same-package split of the Application
// seam: no public seam added.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
	"cflow.local/cflow/internal/store"
)

// discoverProject observes the canonical Git facts through the GitFlow
// seam (PRD 启动与项目识别).
func (a *Application) discoverProject(ctx context.Context) (gitflow.ProjectFacts, error) {
	if a.git == nil {
		return gitflow.ProjectFacts{}, model.NewFault(model.CodeStateInvariantViolation,
			"git seam is not configured for this application")
	}
	facts, err := a.git.Observe(ctx, gitflow.ProjectDiscovery{})
	if err != nil {
		return gitflow.ProjectFacts{}, err
	}
	pf, ok := facts.(gitflow.ProjectFacts)
	if !ok {
		return gitflow.ProjectFacts{}, model.InvariantFault(fmt.Errorf("project discovery returned an unexpected fact type"))
	}
	return pf, nil
}

// queryDiscovery projects the Git facts for the CLI's creation gate.
func (a *Application) queryDiscovery(ctx context.Context) (View, error) {
	facts, err := a.discoverProject(ctx)
	if err != nil {
		return nil, err
	}
	d := facts.Status.Dirty
	return DiscoveryView{
		Root:             facts.Root,
		Branch:           facts.Branch,
		Head:             facts.Head,
		Unborn:           facts.Unborn,
		Detached:         facts.Detached,
		Dirty:            d.StagedCount+d.UnstagedCount+d.UntrackedCount > 0,
		DirtyFingerprint: "sha256:" + d.Combined,
		ProjectKey:       facts.ProjectKey,
		StagedCount:      d.StagedCount,
		UnstagedCount:    d.UnstagedCount,
		UntrackedCount:   d.UntrackedCount,
	}, nil
}

// agentRuntime constructs the per-command Agent Runtime and hydrates the
// persisted Session facts (the Recovery path, design 14.4: the
// Application hydrates from the Store). nil when the Application has no
// Agent configuration; Provider effects fail closed then.
func (a *Application) agentRuntime(ctx context.Context, st model.State) (*agent.Runtime, error) {
	if a.agent.Registry == nil {
		return nil, nil
	}
	cfg := a.agent
	if cfg.Now == nil {
		cfg.Now = a.now
	}
	// The Runtime's evidence writer expects its root to exist (its own
	// guarded creation covers only the subdirectories).
	if cfg.EvidenceDir != "" {
		if _, err := os.Stat(cfg.EvidenceDir); err != nil {
			if err := security.CreateSensitiveDir(cfg.EvidenceDir); err != nil {
				return nil, err
			}
		}
	}
	rt, err := agent.NewRuntime(cfg)
	if err != nil {
		return nil, err
	}
	facts := make([]agent.SessionFact, 0, len(st.Sessions))
	for _, s := range st.Sessions {
		facts = append(facts, agent.SessionFact{Session: s, Provider: s.Provider})
	}
	if err := rt.Hydrate(ctx, facts); err != nil {
		rt.Close()
		return nil, err
	}
	return rt, nil
}

// artifactStore opens the immutable Artifact Store of one workflow
// lazily (per-workflow root under the workflow directory, PRD 全局目录
// 结构).
func (a *Application) artifactStore(wf model.WorkflowID) (*artifact.Store, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.artifacts[wf]; s != nil {
		return s, nil
	}
	st, err := artifact.New(filepath.Join(a.workflowDir(wf), "artifacts"), a.redaction)
	if err != nil {
		return nil, err
	}
	a.artifacts[wf] = st
	return st, nil
}

// planningWorktreePath is the deterministic Planning Snapshot location
// (PRD 全局目录结构: worktrees/<project-key>/<workflow-id>/planning).
func (a *Application) planningWorktreePath(wf model.WorkflowID) string {
	return filepath.Join(a.home, "worktrees", a.project.Key, string(wf), "planning")
}

// ensureWorktreeParent creates the managed worktree chain 0700 through
// the security guard.
func (a *Application) ensureWorktreeParent(path string) error {
	parent := filepath.Dir(path)
	for _, dir := range []string{
		filepath.Join(a.home, "worktrees"),
		filepath.Join(a.home, "worktrees", a.project.Key),
		parent,
	} {
		if _, err := os.Stat(dir); err == nil {
			continue
		}
		if err := security.CreateSensitiveDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// planningPrompt selects the embedded prompt for one planning command
// (design 14.5: prompts are addressed by Agent Purpose).
func (a *Application) planningPrompt(cmd model.Input) (agent.Prompt, bool) {
	reg := a.promptRegistry()
	if reg == nil {
		return agent.Prompt{}, false
	}
	purpose := ""
	switch cmd.(type) {
	case model.DiscussRequirementInput:
		purpose = "REQUIREMENT_DISCUSSION"
	case model.GeneratePlanInput:
		purpose = "PLAN_GENERATION"
	case model.CheckPlanInput:
		purpose = "PLAN_CHECK"
	case model.SpecGenerationInput:
		purpose = "SPEC_GENERATION"
	case model.WorkflowCompilationInput:
		purpose = "WORKFLOW_OPTIMIZATION"
	case model.DispatchInput:
		purpose = "TASK_IMPLEMENTATION"
	}
	if purpose == "" {
		return agent.Prompt{}, false
	}
	return reg.Lookup(purpose)
}

// promptForPurpose resolves the embedded prompt of one Agent Purpose
// (design 14.5: prompts are addressed by Agent Purpose).
func (a *Application) promptForPurpose(purpose model.AgentPurpose) (agent.Prompt, bool) {
	reg := a.promptRegistry()
	if reg == nil {
		return agent.Prompt{}, false
	}
	name := ""
	switch purpose {
	case model.PurposePlanning:
		name = "REQUIREMENT_DISCUSSION"
	case model.PurposePlanCheck:
		name = "PLAN_CHECK"
	case model.PurposeSpecGeneration:
		name = "SPEC_GENERATION"
	case model.PurposeWorkflowOptimization:
		name = "WORKFLOW_OPTIMIZATION"
	case model.PurposeImplementation:
		name = "TASK_IMPLEMENTATION"
	case model.PurposeReview:
		name = "TASK_REVIEW"
	case model.PurposeRepair:
		name = "TASK_REPAIR"
	case model.PurposeFinalVerification:
		name = "FINAL_VERIFICATION"
	}
	if name == "" {
		return agent.Prompt{}, false
	}
	return reg.Lookup(name)
}

// promptRegistry returns the embedded Prompt Registry, loading it once.
func (a *Application) promptRegistry() *agent.PromptRegistry {
	if a.prompts == nil {
		loaded, err := agent.LoadPromptRegistry()
		if err != nil {
			return nil
		}
		a.prompts = loaded
	}
	return a.prompts
}

// ---------------------------------------------------------------------------
// workflow.yaml: static identity and Artifact Manifest (PRD Workflow 元
// 信息). It never stores stage, runtime_status, plan_status, or
// active_run_id: those live in SQLite.
// ---------------------------------------------------------------------------

type workflowManifest struct {
	SchemaVersion int                `yaml:"schema_version"`
	WorkflowID    string             `yaml:"workflow_id"`
	ProjectID     string             `yaml:"project_id"`
	Name          string             `yaml:"name"`
	Repository    repositoryManifest `yaml:"repository"`
	Bindings      bindingsManifest   `yaml:"bindings"`
	Planning      planningManifest   `yaml:"planning_snapshot"`
	Artifacts     artifactsManifest  `yaml:"artifacts"`
	CreatedAt     string             `yaml:"created_at"`
}

type repositoryManifest struct {
	CanonicalPath           string `yaml:"canonical_path"`
	TargetBranch            string `yaml:"target_branch"`
	BaseCommit              string `yaml:"base_commit"`
	InitialWorktreeDirty    bool   `yaml:"initial_worktree_dirty"`
	InitialDirtyFingerprint string `yaml:"initial_dirty_fingerprint,omitempty"`
	IntegrationBranch       string `yaml:"integration_branch"`
}

type bindingsManifest struct {
	ConfigSHA256   string `yaml:"config_sha256,omitempty"`
	PromptSHA256   string `yaml:"prompt_sha256,omitempty"`
	ProtocolSHA256 string `yaml:"protocol_sha256,omitempty"`
}

type planningManifest struct {
	Worktree string `yaml:"worktree"`
	Head     string `yaml:"head"`
}

type artifactsManifest struct {
	ActivePlan *activePlanManifest `yaml:"active_plan,omitempty"`
}

type activePlanManifest struct {
	Revision int    `yaml:"revision"`
	Path     string `yaml:"path"`
	SHA256   string `yaml:"sha256"`
}

// writeWorkflowManifest records the static identity of a created
// Workflow: the repository baseline, the initial dirty facts, the
// config/prompt/protocol hashes, and the Planning Snapshot intent and
// result (brief Step 3).
func (a *Application) writeWorkflowManifest(ctx context.Context, wf model.WorkflowID, create CreateWorkflowCommand, snap gitflow.PlanningSnapshotResult) error {
	facts, err := a.discoverProject(ctx)
	if err != nil {
		return err
	}
	d := facts.Status.Dirty
	m := workflowManifest{
		SchemaVersion: 1,
		WorkflowID:    string(wf),
		ProjectID:     a.project.Key,
		Name:          create.Name,
		Repository: repositoryManifest{
			CanonicalPath:           facts.Root,
			TargetBranch:            facts.Branch,
			BaseCommit:              facts.Head,
			InitialWorktreeDirty:    d.StagedCount+d.UnstagedCount+d.UntrackedCount > 0,
			InitialDirtyFingerprint: "sha256:" + d.Combined,
			IntegrationBranch:       "cflow/" + string(wf) + "/integration",
		},
		Bindings: bindingsManifest{
			ConfigSHA256:   sha256File(filepath.Join(a.home, "config.yaml")),
			PromptSHA256:   promptHashOf(a.promptRegistry(), "REQUIREMENT_DISCUSSION"),
			ProtocolSHA256: providerBindingHash(create.Provider),
		},
		Planning:  planningManifest{Worktree: snap.Worktree, Head: snap.Head},
		CreatedAt: a.now().UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	body, err := yaml.Marshal(m)
	if err != nil {
		return model.InvariantFault(fmt.Errorf("workflow manifest cannot be serialized"))
	}
	return a.writeManifest(wf, filepath.Join(a.workflowDir(wf), "workflow.yaml"), body)
}

// refreshPlanManifest updates the Artifact Manifest's active_plan entry
// after a Plan Revision was recorded. The read goes through the caller's
// open write Store (the mutation lock batch is still held, so no second
// Schema lock may be taken).
func (a *Application) refreshPlanManifest(ctx context.Context, wf model.WorkflowID, st *store.Store) error {
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return err
	}
	plan := view.State.Plan
	if plan == nil {
		return nil
	}
	path := filepath.Join(a.workflowDir(wf), "workflow.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		return model.InvariantFault(fmt.Errorf("workflow manifest cannot be read: %w", err))
	}
	var m workflowManifest
	if err := yaml.Unmarshal(body, &m); err != nil {
		return model.InvariantFault(fmt.Errorf("workflow manifest cannot be parsed: %w", err))
	}
	m.Artifacts.ActivePlan = &activePlanManifest{
		Revision: plan.Revision,
		Path:     planDisplayPath(plan.Revision),
		SHA256:   plan.Hash,
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return model.InvariantFault(fmt.Errorf("workflow manifest cannot be serialized"))
	}
	return a.writeManifest(wf, path, out)
}

// writeManifest persists the workflow.yaml through the security guard: a
// fresh file is born 0600 without replacement; an existing file (the
// manifest update) is verified and atomically replaced.
func (a *Application) writeManifest(wf model.WorkflowID, path string, body []byte) error {
	if err := a.ensureWorkflowDir(wf); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		if _, err := security.CheckPath(security.PathRequest{Path: path, Kind: security.KindFile}); err != nil {
			return err
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, body, 0o600); err != nil {
			return model.InvariantFault(fmt.Errorf("workflow manifest cannot be written"))
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return model.InvariantFault(fmt.Errorf("workflow manifest cannot be installed"))
		}
		return nil
	}
	f, err := security.CreateSensitiveFile(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return model.InvariantFault(fmt.Errorf("workflow manifest cannot be written"))
	}
	if err := f.Close(); err != nil {
		return model.InvariantFault(fmt.Errorf("workflow manifest cannot be closed"))
	}
	return nil
}

// planDisplayPath is the PRD display path of a Plan Revision
// (plan/plan-001.md), relative to the workflow directory.
func planDisplayPath(revision int) string {
	return fmt.Sprintf("plan/plan-%03d.md", revision)
}

func promptHashOf(reg *agent.PromptRegistry, purpose string) string {
	if reg == nil {
		return ""
	}
	p, ok := reg.Lookup(purpose)
	if !ok {
		return ""
	}
	return p.Hash
}

// providerBindingHash is the content hash of one Provider's protocol
// binding ("" when the provider cannot be selected).
func providerBindingHash(provider string) string {
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		return ""
	}
	binding, ok := reg.Lookup(provider)
	if !ok {
		return ""
	}
	return binding.Hash
}

func sha256File(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
