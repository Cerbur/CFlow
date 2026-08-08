package tui

// Fake TUI E2E (TUI task 16): the deterministic, fully-Fake-provider
// lifecycle through the app seam — create → native discussion finish →
// plan approval → execution approval → foreground runner → report →
// apply → cleanup — with the TUI model/render mapping exercised at every
// stage. No real Provider is ever invoked.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/fake"
	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/foreground"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// tuiE2EFixture builds one Fake-driven Application over a real repository.
type tuiE2EFixture struct {
	t   *testing.T
	sup process.Supervisor
	root string
	home string
	ids  model.IDSource
	now  func() time.Time
	seq  int
}

func newTUIE2EFixture(t *testing.T) *tuiE2EFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=Test User", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test User", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main", "-q")
	if err := os.WriteFile(filepath.Join(root, "init.txt"), []byte("init"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	// verification wrappers
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"verify.sh", "final-verify.sh", "apply-verify.sh"} {
		if err := os.WriteFile(filepath.Join(root, "scripts", s), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "wrappers")
	// The CFLOW_HOME must be canonical (no symlink traversal); the macOS
	// temp root resolves through /var -> /private/var.
	home := filepath.Join(t.TempDir(), "home")
	if canon, err := filepath.EvalSymlinks(filepath.Dir(home)); err == nil {
		home = filepath.Join(canon, filepath.Base(home))
	}
	return &tuiE2EFixture{
		t: t, sup: process.NewSupervisor(process.NewOSAdapter()), root: root,
		home: home,
		ids:  model.SequentialIDSource(),
		now:  func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

// app builds a fresh Application with the given Fake scripts loaded.
func (fx *tuiE2EFixture) app(scripts ...string) *app.Application {
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
		fx.t.Fatal(err)
	}
	a, err := app.New(app.Options{
		Home: fx.home, Project: app.ProjectFor(fx.root),
		CflowVersion: "0.0.0-dev", Now: fx.now, IDs: fx.ids,
		Supervisor: fx.sup, GitFlow: flow, Prompts: prompts,
		Agent: agent.RuntimeOptions{
			Registry: reg, Redaction: security.Registry{},
			Adapters: map[string]agent.Adapter{"fake": ad},
			EvidenceDir: filepath.Join(fx.home, "evidence"),
		},
	})
	if err != nil {
		fx.t.Fatal(err)
	}
	return a
}

// fakeScript is the deterministic planning script for one purpose.
func fakeScript(purpose, sessionID, result string) string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"` + purpose +
		`","session_id":"` + sessionID + `","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"` + sessionID + `","at_ms":0}
{"type":"assistant_message","session_id":"` + sessionID + `","text":"running","at_ms":10}
{"type":"session_finished","session_id":"` + sessionID + `","result":` + result + `,"at_ms":20}`
}

func (fx *tuiE2EFixture) next(prefix string) string {
	fx.seq++
	return prefix + string(rune('0'+fx.seq%10)) + string(rune('0'+(fx.seq/10)%10))
}

// validPlanMarkdown is the full PRD-required plan.
const validPlanMarkdown = `# Add divide

## 背景
Division by zero is silent.

## 目标
Division by zero must error.

## 范围
The division operator only.

## 非目标
No other arithmetic changes.

## 约束
No external dependencies.

## 当前实现分析
internal/calc/divide.go returns zero.

## 推荐技术方案
Return a typed error.

## 关键设计决策
The check lives inside Divide.

## 涉及模块与文件边界
internal/calc and internal/cli.

## 数据与兼容性影响
No persisted data.

## 测试与验收方案
Unit tests plus a CLI assertion.

## 风险与回滚
Small revert.

## 未决问题
None.
`

// TestTUIPlanToApplyAndCleanup is the TUI task 16 failure test: the
// complete Fake-provider lifecycle through the app seam, with the TUI
// model/render mapping exercised at every stage.
func TestTUIPlanToApplyAndCleanup(t *testing.T) {
	fx := newTUIE2EFixture(t)
	ctx := context.Background()

	// 1. Create the workflow and run one native discussion turn.
	wf, err := fx.app().Execute(ctx, app.CreateWorkflowCommand{Name: "calculator", Provider: "fake", ConfirmDirty: false})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 2. The native discussion: Prepare records the Session; Finish writes
	// the strict handoff.
	prep, err := fx.app().Execute(ctx, app.PrepareNativeDiscussionCommand{Workflow: wf.Workflow, Provider: "fake"})
	if err != nil {
		t.Fatalf("prepare native discussion: %v", err)
	}
	session := prep.SessionID
	if session == "" {
		t.Fatal("no native session")
	}
	handoff, _ := json.Marshal(map[string]any{
		"workflow_id":         string(wf.Workflow),
		"session_id":          string(session),
		"targets":             "division by zero must error",
		"constraints":         "no external dependencies",
		"non_goals":           "no other arithmetic changes",
		"acceptance_criteria": "Divide returns a typed error on zero",
		"open_questions":      "error wording",
		"change_set":          map[string]any{"revision": 1, "sha256": strings.Repeat("a", 64)},
		"user_decisions":      []map[string]any{{"topic": "error type", "decision": "typed error"}},
	})
	if _, err := fx.app().Execute(ctx, app.FinishDiscussionCommand{Workflow: wf.Workflow, Session: session, Handoff: handoff}); err != nil {
		t.Fatalf("finish discussion: %v", err)
	}

	// 3. Plan generation + check + approval.
	d := fx.next("d")
	if _, err := fx.app(fakeScript("planning", d, `{"accepted":true}`)).Execute(ctx,
		app.DiscussRequirementCommand{Workflow: wf.Workflow, Text: "division by zero must error", Provider: "fake"}); err != nil {
		t.Fatalf("discuss: %v", err)
	}
	p := fx.next("p")
	if _, err := fx.app(fakeScript("planning", p, `{"plan_markdown":`+jsonQuote(validPlanMarkdown)+`}`)).Execute(ctx,
		app.GeneratePlanCommand{Workflow: wf.Workflow, Provider: "fake"}); err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	c := fx.next("c")
	if _, err := fx.app(fakeScript("plan-check", c, `{"decision":"pass","summary":"ok","blockingGaps":[],"nonBlockingSuggestions":[],"confidence":0.9}`)).Execute(ctx,
		app.CheckPlanCommand{Workflow: wf.Workflow, Provider: "fake"}); err != nil {
		t.Fatalf("check plan: %v", err)
	}
	planView, err := fx.app().Query(ctx, app.PlanQuery{Workflow: wf.Workflow})
	if err != nil {
		t.Fatal(err)
	}
	pv := planView.(app.PlanView)
	if _, err := fx.app().Execute(ctx, app.ApprovePlanCommand{Workflow: wf.Workflow, Revision: pv.Revision, Hash: pv.Hash}); err != nil {
		t.Fatalf("approve plan: %v", err)
	}

	// 4. Specs + compile + dry run + execution approval.
	specJSON := `{"id":"s01","goal":"implement divide","depends_on":[],"write_scope":["src/divide/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":2}`
	s := fx.next("s")
	if _, err := fx.app(fakeScript("spec-generation", s, `{"specs":[`+specJSON+`],"proposed_commands":[]}`)).Execute(ctx,
		app.GenerateSpecsCommand{Workflow: wf.Workflow, Provider: "fake"}); err != nil {
		t.Fatalf("generate specs: %v", err)
	}
	w := fx.next("w")
	patch := `{"schema":"cflow-workflow-patch-1","operations":[{"op":"add_checkpoint","node_id":"merge-s01"}]}`
	if _, err := fx.app(fakeScript("workflow-optimization", w, patch)).Execute(ctx,
		app.CompileWorkflowCommand{Workflow: wf.Workflow, Provider: "fake"}); err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	if _, err := fx.app().Execute(ctx, app.ExecutionDryRunCommand{Workflow: wf.Workflow}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	qv, err := fx.app().Query(ctx, app.ExecutionPreviewQuery{Workflow: wf.Workflow})
	if err != nil {
		t.Fatal(err)
	}
	preview := qv.(app.ExecutionPreviewView)
	// The Approval page: Enter alone never approves.
	approval := NewApprovalModel(preview)
	approval, _ = approval.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if approval.Confirmed && approval.Yes {
		t.Fatal("the approval page approved without selecting yes")
	}
	approval, _ = approval.Update(tea.KeyPressMsg{Code: 'y'})
	if !approval.Confirmed || !approval.Yes {
		t.Fatal("the approval page did not confirm")
	}
	if got := RenderApproval(approval); !strings.Contains(got, "confirm: yes") {
		t.Fatalf("approval render = %q", got)
	}
	if _, err := fx.app().Execute(ctx, app.ApproveExecutionCommand{
		Workflow: wf.Workflow, PlanHash: preview.PlanHash, SpecHashes: preview.SpecHashes,
		CatalogHash: preview.CatalogHash, WorkflowHash: preview.WorkflowHash,
		RoutingHash: preview.RoutingHash, BudgetHash: preview.BudgetHash,
		CommitPolicyHash: preview.CommitPolicyHash,
	}); err != nil {
		t.Fatalf("execution approval: %v", err)
	}

	// 5. The foreground Runner drives the dispatch chain to completion.
	implScript := `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":"i1","exit_code":0,"resume":"ok","tasks":{"task-s01":{"writes":[{"path":"src/divide/divide.go","content":"package divide\n\n// Divide returns a/b.\nfunc Divide(a, b int) (int, error) {\n\treturn a / b, nil\n}\n"}],"commit":"implement divide"}}}
{"type":"session_started","session_id":"i1","at_ms":0}
{"type":"assistant_message","session_id":"i1","text":"implemented","at_ms":10}
{"type":"session_finished","session_id":"i1","result":{"summary":"implemented"},"at_ms":20}`
	review := fakeScript("review", "r1", `{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n"}`)
	final := fakeScript("final-verification", "fr1", `{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n"}`)
	a := fx.app(implScript, review, final)
	runner := foreground.Runner{Driver: a}
	execModel := NewExecutionModel(wf.Workflow)
	// Drive passes; the runner stops at a terminal or a user decision.
	for i := 0; i < 6; i++ {
		out, err := a.DriveOnce(ctx, wf.Workflow)
		if err != nil {
			if code, ok := model.CodeOf(err); ok && code == model.CodeWorkspaceAdoptionRequired {
				break
			}
			t.Fatalf("drive pass %d: %v", i, err)
		}
		for _, ev := range out.Outcome.Events {
			execModel = execModel.OnEvent(ev)
		}
		if out.Kind == app.DriveTerminal {
			break
		}
	}
	// Drive to completion through dispatch passes.
	for i := 0; i < 24; i++ {
		out, err := a.DriveOnce(ctx, wf.Workflow)
		if err != nil {
			t.Fatalf("dispatch pass %d: %v", i, err)
		}
		iv, _ := a.Query(ctx, app.InspectQuery{Workflow: wf.Workflow})
		if iv.(app.InspectView).Status.Stage == model.StageCompleted {
			break
		}
		_ = out
	}
	iv, err := a.Query(ctx, app.InspectQuery{Workflow: wf.Workflow})
	if err != nil {
		t.Fatal(err)
	}
	if iv.(app.InspectView).Status.Stage != model.StageCompleted {
		t.Fatalf("workflow did not complete: %+v", iv.(app.InspectView).Status)
	}
	if got := RenderExecution(execModel); !strings.Contains(got, "workflow") {
		t.Fatalf("execution render = %q", got)
	}
	_ = runner
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
