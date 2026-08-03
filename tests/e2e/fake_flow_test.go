// Package e2e: the deterministic Gate 1 end-to-end flow (Task 13, brief
// Step 5, PRD 第一层：确定性 Fixture Gate). The calculator fixture runs
// through the real pipeline: real Git repositories, real Task Commits
// produced by the Fake Provider in real Worktrees, the approved `npm
// test` command through the Verification Catalog, independent structured
// Reviews, and serial --no-ff merges into Integration. The fixture tools
// (node/npm) are prerequisites of the Gate 1 evidence; a dedicated
// scenario preserves the user's fixture working-tree dirt.
package e2e

import (
	"context"
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
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// e2eFixture owns one real repository seeded from the calculator fixture,
// one CFLOW_HOME, and the deterministic Clock/ID source.
type e2eFixture struct {
	t          *testing.T
	sup        process.Supervisor
	repo       string
	home       string
	ids        model.IDSource
	now        func() time.Time
	discussSeq int
	planSeq    int
	checkSeq   int
}

// newE2EFixture builds the calculator repository (with the verification
// wrappers fixed at the Base Commit) and the CFLOW_HOME.
func newE2EFixture(t *testing.T) *e2eFixture {
	t.Helper()
	requireFixtureTools(t)
	canon, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonical temp dir: %v", err)
	}
	repo := filepath.Join(canon, "repo")
	if err := copyDir(filepath.Join("calculator"), repo); err != nil {
		t.Fatalf("seed calculator fixture: %v", err)
	}
	// The verification wrappers are part of the deterministic Base Commit:
	// the Catalog discovers them and the Task runs `npm test` through them
	// (PRD 已确认：Workflow-local Verification Command Catalog).
	scripts := filepath.Join(repo, "scripts")
	if err := os.MkdirAll(scripts, 0o700); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	verify := "#!/bin/sh\nexec npm test\n"
	if err := os.WriteFile(filepath.Join(scripts, "verify.sh"), []byte(verify), 0o755); err != nil {
		t.Fatalf("write verify.sh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "final-verify.sh"), []byte(verify), 0o755); err != nil {
		t.Fatalf("write final-verify.sh: %v", err)
	}
	git(repo, "init", "-q", "-b", "main")
	git(repo, "config", "user.name", "Test User")
	git(repo, "config", "user.email", "test@example.com")
	git(repo, "add", "-A")
	git(repo, "commit", "-q", "-m", "calculator base")
	home := filepath.Join(canon, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	// The concurrency cap permits the three independent Tasks of one pass.
	cfg := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(cfg, []byte("concurrency: 4\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return &e2eFixture{
		t: t, sup: process.NewSupervisor(process.NewOSAdapter()),
		repo: repo, home: home,
		ids: model.SequentialIDSource(),
		now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

// app builds a fresh Application over the fixture with the given inline
// Fake fixture scripts.
func (fx *e2eFixture) app(scripts ...string) *app.Application {
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
	flow, err := gitflow.NewGitFlow(fx.sup, fx.repo)
	if err != nil {
		fx.t.Fatalf("new gitflow: %v", err)
	}
	a, err := app.New(app.Options{
		Home:         fx.home,
		Project:      app.ProjectFor(fx.repo),
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

// driveToExecutionApproval runs the planning lifecycle through the
// Execution Approval with the four calculator Specs and returns the
// workflow identity.
func (fx *e2eFixture) driveToExecutionApproval(t *testing.T) model.WorkflowID {
	t.Helper()
	wf := fx.createWorkflow("calculator")
	fx.discussSeq++
	if _, err := fx.app(discussionScript("d1")).Execute(context.Background(),
		app.DiscussRequirementCommand{Workflow: wf, Text: requirement, Provider: "fake"}); err != nil {
		t.Fatalf("discuss: %v", err)
	}
	fx.planSeq++
	if _, err := fx.app(planScript("p1")).Execute(context.Background(),
		app.GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	fx.checkSeq++
	if _, err := fx.app(checkScript("c1")).Execute(context.Background(),
		app.CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("check plan: %v", err)
	}
	pv := fx.planView(wf)
	if _, err := fx.app().Execute(context.Background(),
		app.ApprovePlanCommand{Workflow: wf, Revision: pv.Revision, Hash: pv.Hash}); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	if _, err := fx.app(specScript("s1")).Execute(context.Background(),
		app.GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("generate specs: %v", err)
	}
	if _, err := fx.app(patchScript("w1")).Execute(context.Background(),
		app.CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	if _, err := fx.app().Execute(context.Background(),
		app.ExecutionDryRunCommand{Workflow: wf}); err != nil {
		t.Fatalf("execution dry run: %v", err)
	}
	qview, err := fx.app().Query(context.Background(), app.ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("execution preview: %v", err)
	}
	preview := qview.(app.ExecutionPreviewView)
	if _, err := fx.app().Execute(context.Background(), app.ApproveExecutionCommand{
		Workflow:         wf,
		PlanHash:         preview.PlanHash,
		SpecHashes:       preview.SpecHashes,
		CatalogHash:      preview.CatalogHash,
		WorkflowHash:     preview.WorkflowHash,
		RoutingHash:      preview.RoutingHash,
		BudgetHash:       preview.BudgetHash,
		CommitPolicyHash: preview.CommitPolicyHash,
	}); err != nil {
		t.Fatalf("execution approval: %v", err)
	}
	return wf
}

// dispatchUntilMerged drives DispatchCommand passes until every merge
// Node of the workflow is SUCCEEDED (or the pass budget is exhausted).
func (fx *e2eFixture) dispatchUntilMerged(t *testing.T, wf model.WorkflowID) app.InspectView {
	t.Helper()
	appl := fx.app(implementationScript(), reviewScript())
	for i := 0; i < 24; i++ {
		if _, err := appl.Execute(context.Background(), app.DispatchCommand{Workflow: wf}); err != nil {
			t.Fatalf("dispatch pass %d: %v", i, err)
		}
		iv := fx.inspect(wf)
		if allMerged(iv) {
			return iv
		}
	}
	iv := fx.inspect(wf)
	t.Fatalf("merges did not complete within the pass budget: %+v", nodeStatuses(iv))
	return iv
}

func (fx *e2eFixture) createWorkflow(name string) model.WorkflowID {
	fx.t.Helper()
	return fx.createWorkflowConfirmed(name, false)
}

// createWorkflowConfirmed creates the workflow with the confirmed-dirty
// isolation flag (the user's pre-existing fixture dirt is isolated, never
// touched).
func (fx *e2eFixture) createWorkflowConfirmed(name string, confirmDirty bool) model.WorkflowID {
	fx.t.Helper()
	out, err := fx.app().Execute(context.Background(),
		app.CreateWorkflowCommand{Name: name, Provider: "fake", ConfirmDirty: confirmDirty})
	if err != nil {
		fx.t.Fatalf("create workflow: %v", err)
	}
	return out.Workflow
}

// driveToExecutionApprovalWithDirty is the planning lifecycle with the
// user's fixture dirt confirmed-isolated at creation.
func (fx *e2eFixture) driveToExecutionApprovalWithDirty(t *testing.T) model.WorkflowID {
	t.Helper()
	wf := fx.createWorkflowConfirmed("calculator", true)
	fx.discussSeq++
	if _, err := fx.app(discussionScript("d1")).Execute(context.Background(),
		app.DiscussRequirementCommand{Workflow: wf, Text: requirement, Provider: "fake"}); err != nil {
		t.Fatalf("discuss: %v", err)
	}
	fx.planSeq++
	if _, err := fx.app(planScript("p1")).Execute(context.Background(),
		app.GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	fx.checkSeq++
	if _, err := fx.app(checkScript("c1")).Execute(context.Background(),
		app.CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("check plan: %v", err)
	}
	pv := fx.planView(wf)
	if _, err := fx.app().Execute(context.Background(),
		app.ApprovePlanCommand{Workflow: wf, Revision: pv.Revision, Hash: pv.Hash}); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	if _, err := fx.app(specScript("s1")).Execute(context.Background(),
		app.GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("generate specs: %v", err)
	}
	if _, err := fx.app(patchScript("w1")).Execute(context.Background(),
		app.CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	if _, err := fx.app().Execute(context.Background(),
		app.ExecutionDryRunCommand{Workflow: wf}); err != nil {
		t.Fatalf("execution dry run: %v", err)
	}
	qview, err := fx.app().Query(context.Background(), app.ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("execution preview: %v", err)
	}
	preview := qview.(app.ExecutionPreviewView)
	if _, err := fx.app().Execute(context.Background(), app.ApproveExecutionCommand{
		Workflow:         wf,
		PlanHash:         preview.PlanHash,
		SpecHashes:       preview.SpecHashes,
		CatalogHash:      preview.CatalogHash,
		WorkflowHash:     preview.WorkflowHash,
		RoutingHash:      preview.RoutingHash,
		BudgetHash:       preview.BudgetHash,
		CommitPolicyHash: preview.CommitPolicyHash,
	}); err != nil {
		t.Fatalf("execution approval: %v", err)
	}
	return wf
}

func (fx *e2eFixture) planView(wf model.WorkflowID) app.PlanView {
	fx.t.Helper()
	view, err := fx.app().Query(context.Background(), app.PlanQuery{Workflow: wf})
	if err != nil {
		fx.t.Fatalf("plan query: %v", err)
	}
	return view.(app.PlanView)
}

func (fx *e2eFixture) inspect(wf model.WorkflowID) app.InspectView {
	fx.t.Helper()
	view, err := fx.app().Query(context.Background(), app.InspectQuery{Workflow: wf})
	if err != nil {
		fx.t.Fatalf("inspect: %v", err)
	}
	return view.(app.InspectView)
}

// worktreeBase is the deterministic managed worktree root of one
// workflow.
func (fx *e2eFixture) worktreeBase(wf model.WorkflowID) string {
	return filepath.Join(fx.home, "worktrees", app.ProjectFor(fx.repo).Key, string(wf))
}

// ---------------------------------------------------------------------------
// fixture scripts
// ---------------------------------------------------------------------------

const requirement = "增加 multiply 和 divide。divide 遇到除数为零时抛出明确异常。增加单元测试并更新 README。"

// validPlan is the deterministic plan Markdown with every PRD-required
// section.
const validPlan = `# 计算器增强计划

## 背景
计算器目前只有 add 和 subtract。
## 目标
增加 multiply 和 divide，除数为零时抛出明确异常，增加单元测试并更新 README。
## 范围
src 与 test 目录以及 README.md。
## 非目标
不引入新的依赖。
## 约束
使用 Node 内置测试运行器。
## 当前实现分析
src/add.ts 与 src/subtract.ts 已存在并有测试。
## 推荐技术方案
新增 src/multiply.ts、src/divide.ts 与相应测试。
## 关键设计决策
divide 除数为零时抛出明确异常。
## 涉及模块与文件边界
src/multiply.ts、src/divide.ts、test/*.test.ts、README.md。
## 数据与兼容性影响
无。
## 测试与验收方案
npm test 通过。
## 风险与回滚
低风险。
## 未决问题
无。
`

func discussionScript(id string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"planning","session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","session_id":%q,"text":"Understood.","at_ms":10}
{"type":"session_finished","session_id":%q,"result":{"summary":"ok"},"at_ms":20}`, id, id, id, id)
}

func planScript(id string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"planning","session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","session_id":%q,"text":"Planning.","at_ms":10}
{"type":"session_finished","session_id":%q,"result":{"plan_markdown":%q},"at_ms":20}`, id, id, id, id, validPlan)
}

func checkScript(id string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"plan-check","session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","session_id":%q,"text":"Checking.","at_ms":10}
{"type":"session_finished","session_id":%q,"result":{"decision":"pass","summary":"ok","blockingGaps":[],"nonBlockingSuggestions":[],"confidence":1},"at_ms":20}`, id, id, id, id)
}

// calculatorSpecs is the deterministic Spec set (brief Step 5): S01
// multiply and S02 divide/error in parallel with S03 README, and S04
// integration tests after S01/S02. The write scopes are disjoint; every
// Spec runs the "verify" wrapper (`npm test`) from the Catalog.
const calculatorSpecs = `{"id":"s01","goal":"implement multiply","depends_on":[],"write_scope":["src/multiply.ts","test/multiply.test.ts"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"],"review_required":true},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":300,"max_retry":2}
{"id":"s02","goal":"implement divide with a clear exception on zero divisor","depends_on":[],"write_scope":["src/divide.ts","test/divide.test.ts"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"],"review_required":true},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":300,"max_retry":2}
{"id":"s03","goal":"update the README with the new operations","depends_on":[],"write_scope":["README.md"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"],"review_required":true},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":300,"max_retry":2}
{"id":"s04","goal":"add integration tests covering multiply and divide together","depends_on":["s01","s02"],"write_scope":["test/integration.test.ts"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"],"review_required":true},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":300,"max_retry":2}`

func specScript(id string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"spec-generation","session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","session_id":%q,"text":"Splitting the plan.","at_ms":10}
{"type":"session_finished","session_id":%q,"result":{"specs":[%s],"proposed_commands":[]},"at_ms":20}`,
		id, id, id, id, strings.ReplaceAll(calculatorSpecs, "\n", ","))
}

func patchScript(id string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"workflow-optimization","session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","session_id":%q,"text":"Proposing a scheduling patch.","at_ms":10}
{"type":"session_finished","session_id":%q,"result":{"schema":"cflow-workflow-patch-1","operations":[{"op":"pin_route","node_id":"task-s01","provider":"fake"}]},"at_ms":20}`,
		id, id, id, id)
}

// implementationScript is the deterministic Fake coding fixture: one
// shared implementation Session whose per-Task plans write the real
// calculator code into each Task Worktree and create the real
// implementation Commit (the simulated Agent's own git add/commit).
func implementationScript() string {
	task := func(id, commit string, writes ...string) string {
		parts := make([]string, 0, len(writes)/2)
		for i := 0; i+1 < len(writes); i += 2 {
			parts = append(parts, fmt.Sprintf(`{"path":%q,"content":%q}`, writes[i], writes[i+1]))
		}
		return fmt.Sprintf(`%q:{"writes":[%s],"commit":%q}`, id, strings.Join(parts, ","), commit)
	}
	tasks := strings.Join([]string{
		task("task-s01", "implement multiply",
			"src/multiply.ts", multiplySource,
			"test/multiply.test.ts", multiplyTest),
		task("task-s02", "implement divide with zero-divisor error",
			"src/divide.ts", divideSource,
			"test/divide.test.ts", divideTest),
		task("task-s03", "update the README",
			"README.md", readmeUpdated),
		task("task-s04", "add integration tests",
			"test/integration.test.ts", integrationTest),
	}, ",")
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":"i1","exit_code":0,"resume":"ok","tasks":{%s}}
{"type":"session_started","session_id":"i1","at_ms":0}
{"type":"assistant_message","session_id":"i1","text":"Implemented the task.","at_ms":10}
{"type":"session_finished","session_id":"i1","result":{"summary":"implemented"},"at_ms":20}`, tasks)
}

// noCommitImplementationScript is the fixture_missing_commit_and_dirty_
// worktree scenario: the coding Session writes its files but never
// commits, leaving the Task Worktree dirty.
func noCommitImplementationScript() string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":"i1","exit_code":0,"resume":"ok","tasks":{"task-s01":{"writes":[{"path":"src/multiply.ts","content":%q}]}}}
{"type":"session_started","session_id":"i1","at_ms":0}
{"type":"assistant_message","session_id":"i1","text":"Wrote the file but never committed.","at_ms":10}
{"type":"session_finished","session_id":"i1","result":{"summary":"implemented"},"at_ms":20}`, multiplySource)
}

func reviewScript() string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"review","session_id":"r1","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"r1","at_ms":0}
{"type":"assistant_message","session_id":"r1","text":"Reviewed the task diff.","at_ms":10}
{"type":"session_finished","session_id":"r1","result":{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n- acceptance coverage present\n"},"at_ms":20}`)
}

// ---------------------------------------------------------------------------
// calculator source files
// ---------------------------------------------------------------------------

const multiplySource = `// Multiply returns the product of two numbers.
export function multiply(a: number, b: number): number {
  return a * b;
}
`

const multiplyTest = `import { test } from "node:test";
import assert from "node:assert/strict";
import { multiply } from "../src/multiply.ts";

test("multiply returns the product", () => {
  assert.equal(multiply(3, 4), 12);
});
`

const divideSource = `// Divide returns the quotient of two numbers. A zero divisor raises a
// clear exception.
export function divide(a: number, b: number): number {
  if (b === 0) {
    throw new Error("division by zero");
  }
  return a / b;
}
`

const divideTest = `import { test } from "node:test";
import assert from "node:assert/strict";
import { divide } from "../src/divide.ts";

test("divide returns the quotient", () => {
  assert.equal(divide(10, 4), 2.5);
});

test("divide raises a clear exception on a zero divisor", () => {
  assert.throws(() => divide(1, 0), /division by zero/);
});
`

const readmeUpdated = "# Calculator\n" +
	"\n" +
	"A minimal TypeScript calculator used as the deterministic fixture of the\n" +
	"CFlow Gate 1 end-to-end demo.\n" +
	"\n" +
	"## Operations\n" +
	"\n" +
	"- `add(a, b)` — returns `a + b`.\n" +
	"- `subtract(a, b)` — returns `a - b`.\n" +
	"- `multiply(a, b)` — returns `a * b`.\n" +
	"- `divide(a, b)` — returns `a / b`; raises a clear exception when `b`\n" +
	"  is zero.\n" +
	"\n" +
	"## Running the tests\n" +
	"\n" +
	"```sh\n" +
	"npm test\n" +
	"```\n" +
	"\n" +
	"The tests run through Node's built-in test runner (`node --test`); the\n" +
	"fixture has no npm dependencies.\n"

const integrationTest = `import { test } from "node:test";
import assert from "node:assert/strict";
import { multiply } from "../src/multiply.ts";
import { divide } from "../src/divide.ts";

test("multiply and divide compose", () => {
  assert.equal(divide(multiply(6, 7), 3), 14);
});

test("division by zero composes into a clear error", () => {
  assert.throws(() => divide(multiply(1, 1), 0), /division by zero/);
});
`

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func requireFixtureTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"node", "npm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("Gate 1 fixture prerequisite missing: %q is not on PATH (%v)", tool, err)
		}
	}
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, e := range entries {
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(from, to); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func git(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("git %v: %v: %s", args, err, out))
	}
	return strings.TrimSpace(string(out))
}

func allMerged(iv app.InspectView) bool {
	for _, n := range iv.Nodes {
		if n.Kind == model.NodeMerge && n.Status != model.NodeSucceeded {
			return false
		}
	}
	return true
}

func nodeStatuses(iv app.InspectView) map[model.NodeID]model.NodeStatus {
	out := map[model.NodeID]model.NodeStatus{}
	for _, n := range iv.Nodes {
		out[n.ID] = n.Status
	}
	return out
}

func statusOf(iv app.InspectView, id string) model.NodeStatus {
	return nodeStatuses(iv)[model.NodeID(id)]
}

func attemptOf(iv app.InspectView, node string) *model.Attempt {
	for i := range iv.Attempts {
		if iv.Attempts[i].Key.Node == model.NodeID(node) {
			return &iv.Attempts[i]
		}
	}
	return nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---------------------------------------------------------------------------
// Gate 1 scenarios
// ---------------------------------------------------------------------------

// TestFakePlanToIntegration (brief Step 2/5): the calculator fixture
// reaches Integration — every Task coded real Commits in its own
// Worktree, the approved `npm test` passed through the Catalog, the
// independent Reviewer passed, and the serial --no-ff merges advanced
// the Integration Branch.
func TestFakePlanToIntegration(t *testing.T) {
	fx := newE2EFixture(t)
	wf := fx.driveToExecutionApproval(t)

	iv := fx.dispatchUntilMerged(t, wf)

	// The full delivery chain: task -> verify -> merge, every Node of the
	// four Tasks SUCCEEDED; the Final Verify is Task 18's node.
	for _, id := range []string{
		"task-s01", "verify-s01", "merge-s01",
		"task-s02", "verify-s02", "merge-s02",
		"task-s03", "verify-s03", "merge-s03",
		"task-s04", "verify-s04", "merge-s04",
	} {
		if statusOf(iv, id) != model.NodeSucceeded {
			t.Fatalf("node %s status = %s, want SUCCEEDED (%+v)", id, statusOf(iv, id), nodeStatuses(iv))
		}
	}
	if statusOf(iv, "final-verify") == model.NodeSucceeded {
		t.Fatalf("final-verify must not run before Gate 2")
	}

	// Every coding Attempt succeeded with the audit Ref evidence pinned.
	for _, id := range []string{"task-s01", "task-s02", "task-s03", "task-s04"} {
		att := attemptOf(iv, id)
		if att == nil || att.Status != model.AttemptSucceeded {
			t.Fatalf("attempt of %s = %+v, want SUCCEEDED", id, att)
		}
		if len(att.Evidence) == 0 {
			t.Fatalf("attempt %s carries no evidence", id)
		}
	}

	// The Integration Branch exists with a serial --no-ff Merge Commit per
	// Task, and the merged source is present in the Integration Worktree.
	base := fx.worktreeBase(wf)
	integration := filepath.Join(base, "integration")
	for _, file := range []string{"src/multiply.ts", "src/divide.ts", "test/integration.test.ts"} {
		if !pathExists(filepath.Join(integration, file)) {
			t.Fatalf("merged file %s missing from the integration worktree", file)
		}
	}
	merged := git(fx.repo, "rev-list", "--count", "--merges", "cflow/"+string(wf)+"/integration")
	if n, err := strconv.Atoi(merged); err != nil || n < 4 {
		t.Fatalf("integration merge commits = %q, want at least 4 serial --no-ff merges", merged)
	}
	// The audit Refs pin every Attempt end.
	for _, id := range []string{"task-s01", "task-s02", "task-s03", "task-s04"} {
		ref := "refs/cflow/" + string(wf) + "/tasks/" + id + "/attempts/1"
		if out := git(fx.repo, "rev-parse", "--verify", "--quiet", ref); out == "" {
			t.Fatalf("audit ref %s missing", ref)
		}
	}
	// The user's working tree and the planning snapshot were never
	// touched by any Task.
	for _, root := range []string{fx.repo, filepath.Join(base, "planning")} {
		for _, file := range []string{"src/multiply.ts", "src/divide.ts"} {
			if pathExists(filepath.Join(root, file)) {
				t.Fatalf("task output leaked into %s", root)
			}
		}
	}
}

// TestFakeMissingCommitAndDirtyWorktree (PRD fixture
// fixture_missing_commit_and_dirty_worktree): a coding Session that
// writes files but never commits leaves the Task Worktree dirty — the
// Attempt fails with DIRTY_TASK_WORKTREE, no Verification ever runs, and
// the user's fixture working-tree dirt is preserved untouched.
func TestFakeMissingCommitAndDirtyWorktree(t *testing.T) {
	fx := newE2EFixture(t)
	// The user's fixture working-tree dirt: an untracked file in the
	// repository before the workflow is created (confirmed isolation).
	dirt := filepath.Join(fx.repo, "user-notes.txt")
	if err := os.WriteFile(dirt, []byte("my uncommitted notes\n"), 0o600); err != nil {
		t.Fatalf("write user dirt: %v", err)
	}
	wf := fx.driveToExecutionApprovalWithDirty(t)

	appl := fx.app(noCommitImplementationScript(), reviewScript())
	for i := 0; i < 6; i++ {
		if _, err := appl.Execute(context.Background(), app.DispatchCommand{Workflow: wf}); err != nil {
			t.Fatalf("dispatch pass %d: %v", i, err)
		}
	}
	iv := fx.inspect(wf)
	att := attemptOf(iv, "task-s01")
	if att == nil || att.FailureCode != model.CodeDirtyTaskWorktree {
		t.Fatalf("task-s01 attempt = %+v, want a DIRTY_TASK_WORKTREE failure", att)
	}
	if statusOf(iv, "verify-s01") == model.NodeRunning || statusOf(iv, "verify-s01") == model.NodeSucceeded {
		t.Fatalf("verification ran for the dirty task: %+v", nodeStatuses(iv))
	}
	// The user's dirt is preserved byte-for-byte; CFlow never stashed,
	// committed, reset, or cleaned it.
	data, err := os.ReadFile(dirt)
	if err != nil {
		t.Fatalf("user dirt vanished: %v", err)
	}
	if string(data) != "my uncommitted notes\n" {
		t.Fatalf("user dirt changed: %q", data)
	}
	// The dirty Task Worktree is preserved for repair (never discarded).
	if !pathExists(filepath.Join(fx.worktreeBase(wf), "tasks", "task-s01", "src", "multiply.ts")) {
		t.Fatalf("the dirty task worktree content was discarded")
	}
}
