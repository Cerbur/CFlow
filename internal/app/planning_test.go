package app

// Planning lifecycle Application tests (Task 10): creation gates through
// the real GitFlow seam, the workflow.yaml static manifest, the
// discussion Session lineage, Plan generation validation, the exact
// revision/hash Approval, and the Planning Snapshot mutation compare
// (design 22.1: real repositories, real SQLite, deterministic Fake
// Adapter).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

func execGit(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test User", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	return cmd
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func requireFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s content = %q, want %q", path, got, want)
	}
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// planningFixture builds the Application over a real committed repository
// and a deterministic Fake Adapter, one script per phase.
type planningFixture struct {
	t    *testing.T
	sup  process.Supervisor
	root string
	home string
	ids  model.IDSource
	now  func() time.Time

	discussSeq int
	planSeq    int
	checkSeq   int

	// probe records the dispatch protocol steps of the execution fixture
	// (Task 12): the RUNNING Attempt commit before the Coding Session
	// start. It is installed on the dispatch Application only.
	probe *callProbe
	// wf is the workflow identity the execution fixture dispatched.
	wf model.WorkflowID
}

func newPlanningFixture(t *testing.T) *planningFixture {
	t.Helper()
	root := fixtureRepo(t)
	return &planningFixture{
		t:    t,
		sup:  process.NewSupervisor(process.NewOSAdapter()),
		root: root,
		home: filepath.Join(tempRoot(t), "home"),
		ids:  model.SequentialIDSource(),
		now:  func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func (fx *planningFixture) app(scripts ...string) *Application {
	fx.t.Helper()
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		fx.t.Fatalf("provider registry: %v", err)
	}
	prompts, err := agent.LoadPromptRegistry()
	if err != nil {
		fx.t.Fatalf("prompt registry: %v", err)
	}
	ad := fake.New(reg)
	for _, s := range scripts {
		if err := ad.LoadScript([]byte(s)); err != nil {
			fx.t.Fatalf("load fake script: %v", err)
		}
	}
	flow, err := gitflow.NewGitFlow(fx.sup, fx.root)
	if err != nil {
		fx.t.Fatalf("new gitflow: %v", err)
	}
	a, err := New(Options{
		Home:         fx.home,
		Project:      ProjectFor(fx.root),
		CflowVersion: "0.0.0-dev",
		Now:          fx.now,
		IDs:          fx.ids,
		Supervisor:   fx.sup,
		GitFlow:      flow,
		Prompts:      prompts,
		Agent: agent.RuntimeOptions{
			Registry:    reg,
			Redaction:   security.Registry{},
			Adapters:    map[string]agent.Adapter{"fake": ad},
			EvidenceDir: filepath.Join(fx.home, "evidence"),
		},
	})
	if err != nil {
		fx.t.Fatalf("new application: %v", err)
	}
	return a
}

func (fx *planningFixture) create(name string, confirm bool) (model.WorkflowID, error) {
	fx.t.Helper()
	out, err := fx.app().Execute(context.Background(),
		CreateWorkflowCommand{Name: name, Provider: "fake", ConfirmDirty: confirm})
	if err != nil {
		return "", err
	}
	return out.Workflow, nil
}

func (fx *planningFixture) planView(wf model.WorkflowID) PlanView {
	fx.t.Helper()
	view, err := fx.app().Query(context.Background(), PlanQuery{Workflow: wf})
	if err != nil {
		fx.t.Fatalf("plan query: %v", err)
	}
	return view.(PlanView)
}

// validPlan is the fixture's deterministic plan output: all PRD required
// sections.
func validPlan() string {
	return `# Add divide

## 背景

Division by zero returns a silent zero.

## 目标

Division by zero must error.

## 范围

The division operator only.

## 非目标

No other arithmetic operator changes.

## 约束

No external dependencies; CLI surface unchanged.

## 当前实现分析

internal/calc/divide.go returns the zero value.

## 推荐技术方案

Return a typed error from Divide.

## 关键设计决策

The check lives inside Divide.

## 涉及模块与文件边界

internal/calc and internal/cli.

## 数据与兼容性影响

No persisted data.

## 测试与验收方案

Unit tests plus a CLI-level assertion.

## 风险与回滚

One caller to update; small revert.

## 未决问题

None.
`
}

// missingSectionPlan drops one required section.
func missingSectionPlan() string {
	lines := strings.Split(validPlan(), "\n")
	var out []string
	for _, line := range lines {
		if strings.HasPrefix(line, "## 未决问题") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func discussionScript(sessionID, text string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"planning","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Understood: %s","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"accepted":true},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), text, strconv.Quote(sessionID))
}

func planScript(sessionID, markdown string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"planning","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Drafting the plan.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"plan_markdown":%s},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(markdown))
}

func checkScript(sessionID, decision string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"plan-check","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Reviewing the plan.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":%s,"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), checkResultJSON(decision))
}

func checkResultJSON(decision string) string {
	return fmt.Sprintf(`{"decision":%s,"summary":"reviewed","blockingGaps":[],"nonBlockingSuggestions":[],"confidence":0.9}`,
		strconv.Quote(decision))
}

// ---------------------------------------------------------------------------
// creation gates (PRD 启动与项目识别)
// ---------------------------------------------------------------------------

func TestCreateWorkflowRequiresGitRoot(t *testing.T) {
	fx := newPlanningFixture(t)
	// A non-Git directory (no repository, no HEAD) only fails closed.
	plain := filepath.Join(tempRoot(t), "not-a-repo")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	flow, err := gitflow.NewGitFlow(fx.sup, plain)
	if err != nil {
		t.Fatalf("new gitflow over a non-git dir: %v", err)
	}
	a, err := New(Options{Home: filepath.Join(tempRoot(t), "home"), Project: ProjectFor(fx.root),
		GitFlow: flow, Now: fx.now, IDs: fx.ids})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(context.Background(),
		CreateWorkflowCommand{Name: "x", Provider: "fake", ConfirmDirty: false}); err == nil {
		t.Fatal("create over a non-git directory succeeded")
	}
}

func TestCreateWorkflowRejectsDetachedHead(t *testing.T) {
	fx := newPlanningFixture(t)
	gitAt(t, fx.root, "checkout", "-q", "--detach")
	if _, err := fx.create("detached", false); err == nil {
		t.Fatal("create on a detached HEAD succeeded")
	}
}

func TestCreateWorkflowDirtyRequiresConfirmation(t *testing.T) {
	fx := newPlanningFixture(t)
	if err := os.WriteFile(filepath.Join(fx.root, "wip.txt"), []byte("wip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.create("dirty", false); err == nil {
		t.Fatal("create on a dirty workspace without confirmation succeeded")
	}
	wf, err := fx.create("dirty", true)
	if err != nil {
		t.Fatalf("create with confirmation: %v", err)
	}
	// The dirty file never enters the Planning Snapshot and stays
	// byte-identical in the user's tree.
	snap := filepath.Join(fx.home, "worktrees", ProjectFor(fx.root).Key, string(wf), "planning")
	if pathExists(filepath.Join(snap, "wip.txt")) {
		t.Fatal("dirty user file leaked into the planning snapshot")
	}
	requireFileContent(t, filepath.Join(fx.root, "wip.txt"), "wip")
}

func TestCreateWorkflowWritesStaticManifest(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(fx.home, "projects", ProjectFor(fx.root).Key,
		"workflows", string(wf), "workflow.yaml"))
	if err != nil {
		t.Fatalf("read workflow.yaml: %v", err)
	}
	text := string(manifest)
	for _, want := range []string{
		"schema_version: 2",
		"workflow_id: " + string(wf),
		"name: add divide",
		"target_branch: main",
		"initial_worktree_dirty: false",
		"workspace_branch: cflow/" + string(wf) + "/workspace",
		"workspace:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("workflow.yaml missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "base_commit: ") {
		t.Fatalf("workflow.yaml missing the base commit:\n%s", text)
	}
	// The workspace worktree exists at the base and is clean.
	flow, _ := gitflow.NewGitFlow(fx.sup, fx.root)
	facts, err := flow.Observe(context.Background(), gitflow.GitStatus{
		Dir: filepath.Join(fx.home, "projects", ProjectFor(fx.root).Key, string(wf), "workspace"),
	})
	if err != nil {
		t.Fatalf("workspace status: %v", err)
	}
	st := facts.(gitflow.StatusFacts)
	if st.Dirty.StagedCount+st.Dirty.UnstagedCount+st.Dirty.UntrackedCount != 0 {
		t.Fatalf("workspace is not clean: %+v", st.Dirty)
	}
}

// ---------------------------------------------------------------------------
// discussion and plan generation
// ---------------------------------------------------------------------------

func TestDiscussionPersistsSessionAndTurnArtifact(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := fx.app(discussionScript("d1", "division by zero must error")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "division by zero must error", Provider: "fake"})
	if err != nil {
		t.Fatalf("discuss: %v", err)
	}
	if out.SessionID == "" {
		t.Fatal("discussion outcome carried no session id")
	}
	// The turn Artifact exists in the immutable store with the session
	// lineage producer.
	store, err := fx.app().artifactStore(wf)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactDiscussionTurn})
	if err != nil {
		t.Fatalf("resolve turn artifact: %v", err)
	}
	body, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	var turn struct {
		SessionID string `json:"session_id"`
		User      string `json:"user"`
	}
	if err := json.Unmarshal(body, &turn); err != nil {
		t.Fatalf("turn artifact is not the CFlow JSON body: %v", err)
	}
	if turn.SessionID != string(out.SessionID) || !strings.Contains(turn.User, "division by zero") {
		t.Fatalf("turn artifact = %+v", turn)
	}
}

func TestPlanGenerationRejectsMissingSection(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := fx.app(planScript("p1", missingSectionPlan())).Execute(context.Background(),
		GeneratePlanCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		t.Fatalf("generate with invalid plan: %v", err)
	}
	found := false
	for _, f := range out.Findings {
		if f.Code == model.CodeSchemaInvalid {
			found = true
		}
	}
	if !found {
		t.Fatalf("invalid plan output produced no schema finding: %+v", out.Findings)
	}
	if pv := fx.planView(wf); pv.Revision != 0 || pv.PlanStatus != "" {
		t.Fatalf("invalid plan output was recorded: %+v", pv)
	}
	// The corrected output succeeds.
	fx.planSeq++
	if _, err := fx.app(planScript("p2", validPlan())).Execute(context.Background(),
		GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("generate with valid plan: %v", err)
	}
	pv := fx.planView(wf)
	if pv.Revision != 1 || pv.PlanStatus != model.PlanDraft || pv.Hash == "" {
		t.Fatalf("plan after generation = %+v", pv)
	}
}

func TestApprovePlanBindsExactRevisionAndHash(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	fx.discussSeq++
	if _, err := fx.app(discussionScript("d1", "division by zero must error")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "division by zero must error", Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.planSeq++
	if _, err := fx.app(planScript("p1", validPlan())).Execute(context.Background(),
		GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.checkSeq++
	if _, err := fx.app(checkScript("c1", "pass")).Execute(context.Background(),
		CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	pv := fx.planView(wf)
	if pv.PlanStatus != model.PlanChecked || pv.Approved {
		t.Fatalf("plan after check = %+v", pv)
	}
	// A mismatched hash is APPROVAL_INPUT_CHANGED with no mutation.
	if _, err := fx.app().Execute(context.Background(),
		ApprovePlanCommand{Workflow: wf, Revision: pv.Revision, Hash: "deadbeef"}); err == nil {
		t.Fatal("approving a mismatched hash succeeded")
	} else if code, ok := model.CodeOf(err); !ok || code != model.CodeApprovalInputChanged {
		t.Fatalf("mismatched approval error = %v, want APPROVAL_INPUT_CHANGED", err)
	}
	// The exact revision and hash approve.
	out, err := fx.app().Execute(context.Background(),
		ApprovePlanCommand{Workflow: wf, Revision: pv.Revision, Hash: pv.Hash})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if out.Stage != model.StageSpecGeneration {
		t.Fatalf("stage after approval = %s, want SPEC_GENERATION", out.Stage)
	}
	pv = fx.planView(wf)
	if !pv.Approved || pv.PlanStatus != model.PlanApproved {
		t.Fatalf("plan after approval = %+v", pv)
	}
}

// ---------------------------------------------------------------------------
// Planning Snapshot mutation compare (PRD Worktree 策略)
// ---------------------------------------------------------------------------

func TestPlanningSnapshotMutationCompare(t *testing.T) {
	fx := newPlanningFixture(t)
	a, err := New(Options{Home: filepath.Join(tempRoot(t), "home"), Project: ProjectFor(fx.root),
		GitFlow: mustFlow(fx), Now: fx.now, IDs: fx.ids})
	if err != nil {
		t.Fatal(err)
	}
	pre, err := a.observeSnapshot(context.Background(), fx.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.verifySnapshotUnchanged(context.Background(), fx.root, pre); err != nil {
		t.Fatalf("unchanged snapshot rejected: %v", err)
	}
	// A changed HEAD is an unexpected agent mutation.
	gitAt(t, fx.root, "commit", "-q", "--allow-empty", "-m", "agent mutation")
	if err := a.verifySnapshotUnchanged(context.Background(), fx.root, pre); err == nil {
		t.Fatal("changed snapshot accepted")
	} else if code, ok := model.CodeOf(err); !ok || code != model.CodeUnexpectedAgentMutation {
		t.Fatalf("snapshot mutation error = %v, want UNEXPECTED_AGENT_MUTATION", err)
	}
	// A new untracked file changes the fingerprint too.
	pre2, err := a.observeSnapshot(context.Background(), fx.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.root, "agent-write.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.verifySnapshotUnchanged(context.Background(), fx.root, pre2); err == nil {
		t.Fatal("untracked mutation accepted")
	} else if code, ok := model.CodeOf(err); !ok || code != model.CodeUnexpectedAgentMutation {
		t.Fatalf("untracked mutation error = %v, want UNEXPECTED_AGENT_MUTATION", err)
	}
}

func mustFlow(fx *planningFixture) *gitflow.GitFlow {
	fx.t.Helper()
	flow, err := gitflow.NewGitFlow(fx.sup, fx.root)
	if err != nil {
		fx.t.Fatalf("new gitflow: %v", err)
	}
	return flow
}

func gitAt(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := execGit(dir, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitOut runs git and returns stdout without the trailing newline.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := execGit(dir, args...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSuffix(string(out), "\n")
}

// workspacePath is the aggregated workspace root of one workflow under
// the fixture home (design §8, layout.Resolver.Workspace).
func (fx *planningFixture) workspacePath(wf model.WorkflowID) string {
	return filepath.Join(fx.home, "projects", ProjectFor(fx.root).Key, string(wf), "workspace")
}

// TestCreateWorkflowCreatesWritableWorkspaceAtBase asserts the TUI
// workflow creation gate (Task 4): exactly one CFlow-managed worktree —
// the long-lived Workspace on its deterministic CFlow-owned branch at the
// recorded Base HEAD — is created, the workspace directory is writable
// (planning sessions run in it), and the user's target branch never moves.
func TestCreateWorkflowCreatesWritableWorkspaceAtBase(t *testing.T) {
	fx := newPlanningFixture(t)
	root := fx.root
	baseHead := gitOut(t, root, "rev-parse", "HEAD")

	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}

	wantBranch := "cflow/" + string(wf) + "/workspace"
	ws := fx.workspacePath(wf)
	if !pathExists(ws) {
		t.Fatalf("workspace %s was not created", ws)
	}
	if head := gitOut(t, root, "rev-parse", "--verify", "refs/heads/"+wantBranch); head != baseHead {
		t.Fatalf("workspace branch HEAD = %s, want base %s", head, baseHead)
	}
	if head := gitOut(t, root, "symbolic-ref", "HEAD"); head != "refs/heads/main" {
		t.Fatalf("repository HEAD = %s, want main", head)
	}
	if v := gitOut(t, root, "worktree", "list", "--porcelain"); strings.Count(v, "worktree ") != 2 {
		t.Fatalf("expected exactly two worktrees (main + workspace), got:\n%s", v)
	}
	probe := filepath.Join(ws, "probe.txt")
	if err := os.WriteFile(probe, []byte("writable\n"), 0o600); err != nil {
		t.Fatalf("workspace %s is not writable: %v", ws, err)
	}
	if head := gitOut(t, root, "rev-parse", "HEAD"); head != baseHead {
		t.Fatalf("target branch main moved: %s -> %s", baseHead, head)
	}
}
