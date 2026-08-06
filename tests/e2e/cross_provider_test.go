// Package e2e: the Cross-Provider acceptance (Task 18, PRD Gate 2 与已确
// 认：真实 Cross-Provider E2E). TestRealCrossProvider is the opt-in real
// Codex/Claude run, gated by CFLOW_E2E_REAL=1: it NEVER executes without
// the environment variable because it costs real model requests and runs
// with the providers' default permissions — the user must approve the
// exact Dry Run, routes/models/budgets, the default-permission trust
// boundary, and the potential network/cost BEFORE the gate is set. Its
// default (off) behavior is a safe skip.
//
// TestDialectEquivalentCrossProvider is the offline deterministic
// equivalent the Gate 2 suite runs: two parallel Tasks routed to the
// codex and claude provider names, executed by the deterministic Fake
// Adapter registered under both names, producing real Commits in real
// Worktrees, with independent Review Sessions, serial --no-ff merges,
// the Final Verify over the full Integration range with an independent
// Final Reviewer, completion with the immutable Final Report, and an
// unchanged Target Branch.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/claude"
	"cflow.local/cflow/internal/agent/codex"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// ---------------------------------------------------------------------------
// the offline dialect-equivalent Cross-Provider flow
// ---------------------------------------------------------------------------

// dualProviderSpecs is the deterministic Spec set of the cross-provider
// fixture: two independent Tasks, S01 routed to codex and S02 routed to
// claude, disjoint write scopes, each verified through the approved
// "verify" wrapper.
const dualProviderSpecs = `{"id":"s01","goal":"implement multiply","depends_on":[],"write_scope":["src/multiply.ts","test/multiply.test.ts"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"],"review_required":true},"route":{"provider":"codex","model":"default","budget":10},"timeout_seconds":600,"max_retry":2}
{"id":"s02","goal":"implement divide with a clear exception on zero divisor","depends_on":[],"write_scope":["src/divide.ts","test/divide.test.ts"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"],"review_required":true},"route":{"provider":"claude","model":"default","budget":10},"timeout_seconds":600,"max_retry":2}`

// dualProviderSpecScript wraps the two Specs in the Session output.
func dualProviderSpecScript(sessionID string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"spec-generation","session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","session_id":%q,"text":"Splitting the plan.","at_ms":10}
{"type":"session_finished","session_id":%q,"result":{"specs":[%s],"proposed_commands":[]},"at_ms":20}`,
		sessionID, sessionID, sessionID, sessionID, strings.ReplaceAll(dualProviderSpecs, "\n", ","))
}

// finalReviewScript is the deterministic FINAL_VERIFICATION Session
// output: a structured PASS verdict over the full Integration result.
func finalReviewScript() string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"final-verification","session_id":"fr1","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"fr1","at_ms":0}
{"type":"assistant_message","session_id":"fr1","text":"Reviewed the full integration result.","at_ms":10}
{"type":"session_finished","session_id":"fr1","result":{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n- plan acceptance criteria verified\n"},"at_ms":20}`
}

// crossProviderApp builds an Application over the fixture with the
// deterministic Fake Adapter registered under the codex and claude
// provider names too, each instance bound to its provider's registry
// binding: the routing, budget, and dispatch machinery sees two real
// provider identities (the detection facts of every adapter match the
// approved binding of its provider, so the dispatch CAS passes) while
// every Session runs the deterministic Fake dialect with the serving
// provider's declared dialect (the offline equivalent of two real
// Provider CLIs).
func (fx *e2eFixture) crossProviderApp(scripts ...string) *app.Application {
	fx.t.Helper()
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		fx.t.Fatalf("provider registry: %v", err)
	}
	prompts, err := agent.LoadPromptRegistry()
	if err != nil {
		fx.t.Fatalf("prompt registry: %v", err)
	}
	dialectOf := func(name string) string {
		b, err := reg.Select(name)
		if err != nil {
			fx.t.Fatalf("binding %s: %v", name, err)
		}
		return b.Dialect.ID
	}
	load := func(ad *fake.Adapter, name, dialect string) {
		for _, s := range scripts {
			// The fixture scripts declare the fake dialect; each serving
			// adapter's binding validates its own declared dialect. Every
			// declared provider session id is prefixed with the serving
			// provider's name, so two adapters sharing one Runtime never
			// claim the same provider session id (the Runtime rejects a
			// duplicate claim as an in-use id).
			script := strings.ReplaceAll(s, "cflow.dialect.fake.v1", dialect)
			script = strings.ReplaceAll(script, `"session_id":"`, `"session_id":"`+name+"-")
			if err := ad.LoadScript([]byte(script)); err != nil {
				fx.t.Fatalf("load fake script: %v", err)
			}
		}
	}
	fakeAd := fake.New(reg)
	load(fakeAd, "fake", dialectOf("fake"))
	codexAd := fake.NewNamed(reg, "codex")
	load(codexAd, "codex", dialectOf("codex"))
	claudeAd := fake.NewNamed(reg, "claude")
	load(claudeAd, "claude", dialectOf("claude"))
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
			Adapters:    map[string]agent.Adapter{"fake": fakeAd, "codex": codexAd, "claude": claudeAd},
			EvidenceDir: filepath.Join(fx.home, "evidence"),
		},
	})
	if err != nil {
		fx.t.Fatalf("new application: %v", err)
	}
	return a
}

// dualProviderRequirement is the real Cross-Provider requirement. The
// dualProviderSpecs implement multiply and divide only (two Tasks routed
// to codex and claude) — the requirement deliberately does not ask for a
// README update, so the plan and the Final Reviewer cannot demand a
// change no Spec covers.
const dualProviderRequirement = "增加 multiply 和 divide。divide 遇到除数为零时抛出明确异常。增加单元测试。"

// dualProviderPlan is the deterministic plan Markdown matching the
// dual-provider requirement and Spec set (no README task): every section
// the plan envelope requires, with the write scopes exactly the two
// Specs implement.
const dualProviderPlan = `# 计算器增强计划

## 背景
计算器目前只有 add 和 subtract。
## 目标
增加 multiply 和 divide，除数为零时抛出明确异常，增加单元测试。
## 范围
src 与 test 目录。
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
src/multiply.ts、src/divide.ts、test/*.test.ts。
## 数据与兼容性影响
新增独立模块与测试，不影响既有 add/subtract 行为。
## 测试与验收方案
新增 multiply 与 divide 单元测试，运行 npm test 验收。
## 风险与回滚
实现缺陷由 Task Review 拦截；失败可回滚对应提交。
## 未决问题
无。`

// dualProviderPlanScript wraps the dual-provider plan in a Session
// output (the planScript helper uses the README-scoped validPlan; the
// dual E2E must not present a plan that asks for a change no Spec
// covers).
func dualProviderPlanScript(id string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"planning","session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","session_id":%q,"text":"Planning.","at_ms":10}
{"type":"session_finished","session_id":%q,"result":{"plan_markdown":%q},"at_ms":20}`, id, id, id, id, dualProviderPlan)
}

// driveDualToExecutionApproval runs the planning lifecycle with the
// dual-provider Spec set through the Execution Approval and returns the
// workflow identity. The planning phases (discussion through
// compilation) always run on the deterministic Fake Adapter (provider
// "fake"); the Execution Dry Run — whose routing policy records the
// detected executable identity of the routed providers — and the
// Execution Approval run through the caller-provided runtime App, so the
// approved binding facts come from exactly the adapters the dispatch
// will use.
func (fx *e2eFixture) driveDualToExecutionApproval(t *testing.T) model.WorkflowID {
	t.Helper()
	return fx.driveDualToExecutionApprovalWith(t, fx.crossProviderApp())
}

// driveDualToExecutionApprovalWith is driveDualToExecutionApproval with
// an explicit runtime App for the Dry Run and the Approval.
func (fx *e2eFixture) driveDualToExecutionApprovalWith(t *testing.T, runtimeApp *app.Application) model.WorkflowID {
	t.Helper()
	wf := fx.createWorkflow("dual-provider")
	fx.discussSeq++
	if _, err := fx.crossProviderApp(discussionScript("d1")).Execute(context.Background(),
		app.DiscussRequirementCommand{Workflow: wf, Text: dualProviderRequirement, Provider: "fake"}); err != nil {
		t.Fatalf("discuss: %v", err)
	}
	fx.planSeq++
	if _, err := fx.crossProviderApp(dualProviderPlanScript("p1")).Execute(context.Background(),
		app.GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	fx.checkSeq++
	if _, err := fx.crossProviderApp(checkScript("c1")).Execute(context.Background(),
		app.CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("check plan: %v", err)
	}
	pv := fx.planView(wf)
	if _, err := fx.crossProviderApp().Execute(context.Background(),
		app.ApprovePlanCommand{Workflow: wf, Revision: pv.Revision, Hash: pv.Hash}); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	if _, err := fx.crossProviderApp(dualProviderSpecScript("s1")).Execute(context.Background(),
		app.GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("generate specs: %v", err)
	}
	if _, err := fx.crossProviderApp(patchScript("w1")).Execute(context.Background(),
		app.CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	if _, err := runtimeApp.Execute(context.Background(),
		app.ExecutionDryRunCommand{Workflow: wf}); err != nil {
		t.Fatalf("execution dry run: %v", err)
	}
	qview, err := runtimeApp.Query(context.Background(), app.ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("execution preview: %v", err)
	}
	preview := qview.(app.ExecutionPreviewView)
	// The Dry Run discloses the Provider default-permission trust boundary
	// (PRD 约束 30) — the approval preview names it, never a sandbox claim.
	if !strings.Contains(preview.TrustBoundary, "default permissions") || strings.Contains(preview.TrustBoundary, "sandboxed=true") {
		t.Fatalf("trust boundary not disclosed in the preview: %q", preview.TrustBoundary)
	}
	if _, err := runtimeApp.Execute(context.Background(), app.ApproveExecutionCommand{
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

// dispatchUntilCompleted drives dispatch passes with the given runtime
// App until the Workflow is COMPLETED (the Final Verify chain and the
// exact-evidence completion ran) or the pass budget is exhausted.
func (fx *e2eFixture) dispatchUntilCompleted(t *testing.T, wf model.WorkflowID, appl *app.Application) app.InspectView {
	t.Helper()
	for i := 0; i < 24; i++ {
		if _, err := appl.Execute(context.Background(), app.DispatchCommand{Workflow: wf}); err != nil {
			t.Fatalf("dispatch pass %d: %v", i, err)
		}
		iv := fx.inspect(wf)
		if iv.Status.Stage == model.StageCompleted {
			return iv
		}
	}
	iv := fx.inspect(wf)
	t.Fatalf("workflow did not complete within the pass budget: nodes=%+v attempts=%s", nodeStatuses(iv), attemptsSummary(iv))
	return iv
}

// attemptsSummary renders each Attempt's node, status and failure code for
// diagnosing a stalled real-provider workflow.
func attemptsSummary(iv app.InspectView) string {
	parts := make([]string, 0, len(iv.Attempts))
	for i := range iv.Attempts {
		a := iv.Attempts[i]
		parts = append(parts, fmt.Sprintf("%s=%s/%s", a.Key.Node, a.Status, a.FailureCode))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// TestDialectEquivalentCrossProvider (brief Step 5: the offline
// dialect-equivalent concurrency path): two parallel Tasks routed to the
// codex and claude provider names overlap in virtual time with
// independent Session IDs, real Commits land in real Worktrees, the
// Reviews are independent, the merges are serial --no-ff, the Final
// Verify runs over the full Integration range with an independent Final
// Reviewer, the Workflow completes with the immutable Final Report, and
// the Target Branch never changes.
func TestDialectEquivalentCrossProvider(t *testing.T) {
	fx := newE2EFixture(t)
	wf := fx.driveDualToExecutionApproval(t)

	iv := fx.dispatchUntilCompleted(t, wf, fx.crossProviderApp(implementationScript(), reviewScript(), finalReviewScript()))

	// Every delivery chain Node SUCCEEDED including the Final Verify.
	for _, id := range []string{
		"task-s01", "verify-s01", "merge-s01",
		"task-s02", "verify-s02", "merge-s02",
		"final-verify",
	} {
		if statusOf(iv, id) != model.NodeSucceeded {
			t.Fatalf("node %s status = %s, want SUCCEEDED (%+v)", id, statusOf(iv, id), nodeStatuses(iv))
		}
	}

	// The two codex/claude Tasks dispatched in one pass: their coding
	// Attempts share the same virtual-time instant.
	var started time.Time
	taskAttempts := 0
	for i := range iv.Attempts {
		at := &iv.Attempts[i]
		if !strings.HasPrefix(string(at.Key.Node), "task-s") {
			continue
		}
		taskAttempts++
		if started.IsZero() {
			started = at.StartedAt
		}
		if !at.StartedAt.Equal(started) {
			t.Fatalf("task %s started at %v, want the shared pass instant %v (cross-provider overlap)", at.Key.Node, at.StartedAt, started)
		}
	}
	if taskAttempts != 2 {
		t.Fatalf("task attempts = %d, want 2", taskAttempts)
	}

	// Independent Session lineages: the implementation Sessions ran on
	// the two distinct routes and the review Sessions never share a
	// provider session id with them.
	implProviders := map[string]bool{}
	implIDs := map[string]bool{}
	reviewIDs := map[string]bool{}
	for _, s := range iv.Sessions {
		switch s.Purpose {
		case model.PurposeImplementation:
			implProviders[s.Provider] = true
			implIDs[s.ProviderSessionID] = true
		case model.PurposeReview:
			reviewIDs[s.ProviderSessionID] = true
		}
	}
	if !implProviders["codex"] || !implProviders["claude"] {
		t.Fatalf("implementation sessions did not run on both routes: %v", implProviders)
	}
	for id := range reviewIDs {
		if implIDs[id] {
			t.Fatalf("a review session reused an implementation provider session id %q", id)
		}
	}

	// Serial --no-ff Integration merges.
	merged := git(fx.repo, "rev-list", "--count", "--merges", "cflow/"+string(wf)+"/integration")
	if n, err := strconv.Atoi(merged); err != nil || n < 2 {
		t.Fatalf("integration merge commits = %q, want at least 2 serial --no-ff merges", merged)
	}

	// The Final Verify bound the exact Integration HEAD: its Attempt's
	// StartHead is the head the Final Reviewer verified and the Workflow
	// completed against.
	fv := attemptOf(iv, "final-verify")
	if fv == nil || fv.StartHead == "" || fv.StartHead != iv.Status.IntegrationHead {
		t.Fatalf("final-verify attempt %+v does not bind the integration head %s", fv, iv.Status.IntegrationHead)
	}

	// The immutable Final Report renders PASSED with Apply not run.
	view, err := fx.crossProviderApp().Query(context.Background(),
		app.ReportQuery{Workflow: wf, Build: observe.BuildInfo{Version: "0.0.0-dev"}})
	if err != nil {
		t.Fatalf("report query: %v", err)
	}
	rv := view.(app.ReportView)
	if rv.Report.Result != "PASSED" || rv.Report.Apply.Status != "NOT_RUN" {
		t.Fatalf("report result/apply = %s/%s, want PASSED/NOT_RUN", rv.Report.Result, rv.Report.Apply.Status)
	}

	// The Target Branch never changed: the user branch stays at the Base
	// Commit with the workflow recorded on the integration branch only.
	if out := git(fx.repo, "branch", "--show-current"); strings.TrimSpace(out) != "main" {
		t.Fatalf("target branch moved to %q", out)
	}
	if out := git(fx.repo, "rev-parse", "HEAD"); strings.TrimSpace(out) != fx.baseCommit() {
		t.Fatalf("target HEAD moved from the base commit")
	}
}

// baseCommit is the recorded Base Commit of the fixture repository.
func (fx *e2eFixture) baseCommit() string {
	return git(fx.repo, "rev-parse", "HEAD")
}

// ---------------------------------------------------------------------------
// the opt-in real Cross-Provider E2E (brief Step 6; approval-gated)
// ---------------------------------------------------------------------------

// codexAdapter builds the real Codex Adapter over one binding.
func codexAdapter(sup process.Supervisor, binding agent.ProviderBinding) agent.Adapter {
	return codex.New(sup, binding)
}

// claudeAdapter builds the real Claude Adapter over one binding.
func claudeAdapter(sup process.Supervisor, binding agent.ProviderBinding) agent.Adapter {
	return claude.New(sup, binding)
}

// realCrossProviderApp builds the Application whose codex and claude
// adapters are the REAL dialect adapters over the OS supervisor (the
// production executables): the Dry Run records their detection facts,
// the dispatch CAS re-detects and compares the same identities, and
// every Start/Resume launches the real CLI through the real argv
// (StartArgv / stream-json). No Fake adapter is registered under the
// codex or claude names on this path.
func (fx *e2eFixture) realCrossProviderApp(t *testing.T) *app.Application {
	t.Helper()
	sup := process.NewSupervisor(process.NewOSAdapter())
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatalf("provider registry: %v", err)
	}
	prompts, err := agent.LoadPromptRegistry()
	if err != nil {
		t.Fatalf("prompt registry: %v", err)
	}
	codexBinding, err := reg.Select("codex")
	if err != nil {
		t.Fatalf("codex binding: %v", err)
	}
	claudeBinding, err := reg.Select("claude")
	if err != nil {
		t.Fatalf("claude binding: %v", err)
	}
	flow, err := gitflow.NewGitFlow(sup, fx.repo)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	a, err := app.New(app.Options{
		Home:         fx.home,
		Project:      app.ProjectFor(fx.repo),
		CflowVersion: "0.0.0-dev",
		Now:          fx.now,
		IDs:          fx.ids,
		Supervisor:   sup,
		GitFlow:      flow,
		Prompts:      prompts,
		Agent: agent.RuntimeOptions{
			Registry:    reg,
			Redaction:   security.Registry{},
			Adapters:    map[string]agent.Adapter{"codex": codexAdapter(sup, codexBinding), "claude": claudeAdapter(sup, claudeBinding)},
			EvidenceDir: filepath.Join(fx.home, "evidence"),
		},
	})
	if err != nil {
		t.Fatalf("new application: %v", err)
	}
	return a
}

// requireRealRoutingEvidence proves the approved routing policy was
// recorded from the REAL provider executables: the codex and claude
// bindings of the immutable routing-policy Artifact must carry the
// detected executable path, sha256, and CLI version. Only the real
// adapters' detection produces those facts — the deterministic Fake
// reports an empty identity — so a pass of the gated run is genuine
// evidence of real provider execution (review fix: the real test must
// never fall back to Fake adapters under the provider names).
func (fx *e2eFixture) requireRealRoutingEvidence(t *testing.T, wf model.WorkflowID) {
	t.Helper()
	root := filepath.Join(fx.home, "projects", app.ProjectFor(fx.repo).Key, "workflows", string(wf), "artifacts")
	store, err := artifact.New(root, security.Registry{})
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	ref, err := store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactRoutingPolicy})
	if err != nil {
		t.Fatalf("routing policy artifact: %v", err)
	}
	body, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("routing policy body: %v", err)
	}
	set, err := agent.ParseRoutingPolicySet(body)
	if err != nil {
		t.Fatalf("parse routing policy: %v", err)
	}
	have := map[string]agent.RouteBinding{}
	for _, p := range set.Policies {
		for _, b := range p.Bindings {
			if _, dup := have[b.Provider]; !dup {
				have[b.Provider] = b
			}
		}
	}
	for _, name := range []string{"codex", "claude"} {
		b, ok := have[name]
		if !ok {
			t.Fatalf("routing policy has no %s binding: %+v", name, have)
		}
		if b.ExecutablePath == "" || b.ExecutableSHA256 == "" || b.CLIVersion == "" {
			t.Fatalf("routing binding for %s carries no real executable identity (path %q sha256 %q cli %q); the real adapters were not detected",
				name, b.ExecutablePath, b.ExecutableSHA256, b.CLIVersion)
		}
	}
}

// TestRealCrossProvider (brief Step 6) is the explicitly authorized real
// Codex/Claude E2E: the real Adapters over the OS supervisor drive two
// parallel Tasks routed to codex and claude, real Commits in real
// Worktrees, independent Review Sessions, deterministic Verification,
// serial Integration merges, the Final Verify/Review, the final report,
// and an unchanged Target Branch. No Fake adapter is registered under
// the codex or claude names on this path, and the approved routing
// evidence is asserted to carry the real executable identity facts.
//
// It NEVER runs without CFLOW_E2E_REAL=1: the gate requires the user to
// have approved the exact Dry Run, the provider routes/models/budgets,
// the default-permission trust boundary, and the potential network/cost.
// Its default (off) behavior is a safe skip; a failure of an authorized
// run is retained as evidence and is never hidden by a Fake result.
func TestRealCrossProvider(t *testing.T) {
	if os.Getenv("CFLOW_E2E_REAL") != "1" {
		t.Skip("CFLOW_E2E_REAL=1 required: the real Cross-Provider E2E runs paid model requests with the providers' default permissions; it must be explicitly approved (exact Dry Run, routes/models/budgets, trust boundary, network/cost) before the gate is set")
	}
	fx := newE2EFixture(t)
	a := fx.realCrossProviderApp(t)
	// The Dry Run and the Execution Approval run through the real App, so
	// the immutable routing policy records the detected identity of the
	// real codex and claude executables (read-only version probes; no
	// model request yet).
	wf := fx.driveDualToExecutionApprovalWith(t, a)
	fx.requireRealRoutingEvidence(t, wf)

	// The dispatch passes launch the real CLIs: the CAS pre-pass
	// re-detects the same executable identities and every Start submits
	// the real argv (codex exec --json --output-schema ...; claude
	// --print --input-format stream-json ...) over the OS supervisor.
	iv := fx.dispatchUntilCompleted(t, wf, a)
	for _, id := range []string{
		"task-s01", "verify-s01", "merge-s01",
		"task-s02", "verify-s02", "merge-s02",
		"final-verify",
	} {
		if statusOf(iv, id) != model.NodeSucceeded {
			t.Fatalf("node %s status = %s, want SUCCEEDED", id, statusOf(iv, id))
		}
	}
	// The implementation Sessions ran on the two real routes.
	implProviders := map[string]bool{}
	for _, s := range iv.Sessions {
		if s.Purpose == model.PurposeImplementation {
			implProviders[s.Provider] = true
		}
	}
	if !implProviders["codex"] || !implProviders["claude"] {
		t.Fatalf("implementation sessions did not run on both real routes: %v", implProviders)
	}
	// The wire terminal result shapes (ledger obligation from Task 15):
	// the real provider runs must settle through the validated unified
	// events with structured terminal results — the review verdict is
	// judged from the session_finished result, never from exit codes.
	for _, s := range iv.Sessions {
		if s.Purpose == model.PurposeReview || s.Purpose == model.PurposeFinalVerification {
			if s.Status != model.SessionCompleted {
				t.Fatalf("review session %s settled %s, want COMPLETED", s.ID, s.Status)
			}
		}
	}
	if out := git(fx.repo, "branch", "--show-current"); strings.TrimSpace(out) != "main" {
		t.Fatalf("target branch moved to %q", out)
	}

	// Record the redacted Gate 3 evidence (observe.ReleaseEvidenceFile, kind
	// "real-cross-provider") when the controller set CFLOW_REAL_E2E_EVIDENCE:
	// the report artifact hash of the run's Final Report, bound to the release
	// candidate facts the controller injects (CFLOW_REAL_E2E_BINARY_SHA256 /
	// CFLOW_REAL_E2E_SOURCE_COMMIT). Symmetric with the dogfood evidence
	// writer; without the env vars the run still completes but records no
	// on-disk evidence.
	if path := os.Getenv("CFLOW_REAL_E2E_EVIDENCE"); path != "" {
		binarySHA := os.Getenv("CFLOW_REAL_E2E_BINARY_SHA256")
		sourceCommit := os.Getenv("CFLOW_REAL_E2E_SOURCE_COMMIT")
		if binarySHA == "" || sourceCommit == "" {
			t.Fatalf("CFLOW_REAL_E2E_EVIDENCE requires CFLOW_REAL_E2E_BINARY_SHA256 and CFLOW_REAL_E2E_SOURCE_COMMIT")
		}
		store, err := artifact.New(filepath.Join(fx.home, "projects", app.ProjectFor(fx.repo).Key, "workflows", string(wf), "artifacts"), security.Registry{})
		if err != nil {
			t.Fatalf("report artifact store: %v", err)
		}
		ref, err := store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactReport})
		if err != nil {
			t.Fatalf("resolve final report: %v", err)
		}
		ev := observe.ReleaseEvidenceFile{
			Kind:         "real-cross-provider",
			BinarySHA256: binarySHA,
			SourceCommit: sourceCommit,
			Reviewed:     true,
			ReportHash:   ref.Hash,
		}
		body, err := json.MarshalIndent(ev, "", "  ")
		if err != nil {
			t.Fatalf("marshal real-e2e evidence: %v", err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write real-e2e evidence: %v", err)
		}
	}
}
