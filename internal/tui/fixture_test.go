package tui

// Shared TUI test fixture: a real temporary git repository plus an
// Application over the deterministic Fake Adapter. Every test drives the
// actual root Model; nothing ever invokes a real Provider.

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
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// tuiFixture builds one Fake-driven Application over a real repository.
type tuiFixture struct {
	t    *testing.T
	sup  process.Supervisor
	root string
	home string
	ids  model.IDSource
	now  func() time.Time
	seq  int
}

// newTUIFixture creates the repository (with the verification wrappers)
// and a canonical CFLOW_HOME. The repo root is canonicalized so the Git
// Worktree Registry paths match (macOS temp roots resolve through
// /var -> /private/var).
func newTUIFixture(t *testing.T) *tuiFixture {
	t.Helper()
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	root := filepath.Join(tempRoot, "repo")
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
	git("config", "user.name", "Test User")
	git("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(root, "init.txt"), []byte("init"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	// The verification wrappers the Catalog discovers from the Base
	// Commit (the deterministic verify/final/apply commands exit 0).
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
	return &tuiFixture{
		t: t, sup: process.NewSupervisor(process.NewOSAdapter()), root: root,
		home: home,
		ids:  model.SequentialIDSource(),
		now:  func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

// newApp builds a fresh Application with the given Fake scripts loaded.
func (fx *tuiFixture) newApp(scripts ...string) *app.Application {
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
			Adapters:    map[string]agent.Adapter{"fake": ad},
			EvidenceDir: filepath.Join(fx.home, "evidence"),
		},
	})
	if err != nil {
		fx.t.Fatal(err)
	}
	return a
}

// appRef is the shared-Application holder of one test: the TUI opens the
// Application through its OpenApplication seam, and the test observes
// the same instance for the authoritative assertions.
type appRef struct {
	fx      *tuiFixture
	scripts []string
	a       *app.Application
}

// open is the cli.Dependencies.OpenApplication seam.
func (r *appRef) open(ctx context.Context) (*app.Application, error) {
	if r.a == nil {
		r.a = r.fx.newApp(r.scripts...)
	}
	return r.a, nil
}

// next yields a deterministic unique fixture session id.
func (fx *tuiFixture) next(prefix string) string {
	fx.seq++
	return prefix + string(rune('0'+fx.seq%10)) + string(rune('0'+(fx.seq/10)%10))
}

// fakeScript is the deterministic wire fixture of one purpose.
func fakeScript(purpose, sessionID, result string) string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"` + purpose +
		`","session_id":"` + sessionID + `","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"` + sessionID + `","at_ms":0}
{"type":"assistant_message","session_id":"` + sessionID + `","text":"running","at_ms":10}
{"type":"session_finished","session_id":"` + sessionID + `","result":` + result + `,"at_ms":20}`
}

// planScript is the deterministic Plan Revision output. Its terminal result
// also carries the strict handoff content fields so the SAME planning
// fixture serves the managed discussion finish resume (the Application
// binds the runtime facts; the plan generation extracts plan_markdown).
func planScript(sessionID string) string {
	return fakeScript("planning", sessionID,
		`{"plan_markdown":`+jsonQuote(validPlanMarkdown)+`,`+handoffContentFields+`}`)
}

// handoffContentFields is the strict handoff content fields the user types
// in the discussion handoff editor (content fields only; the runtime facts
// — workflow_id, session_id, change_set — are bound by CFlow's managed
// resume). It is also the content the managed resume fixture produces.
const handoffContentFields = `"targets":"division by zero must error","constraints":"no external dependencies","non_goals":"no other arithmetic changes","acceptance_criteria":"Divide returns a typed error on zero","open_questions":"error wording","user_decisions":[{"topic":"error type","decision":"typed error"}]`

// checkPlanPassScript is the independent Plan Check PASS verdict.
func checkPlanPassScript(sessionID string) string {
	return fakeScript("plan-check", sessionID,
		`{"decision":"pass","summary":"ok","blockingGaps":[],"nonBlockingSuggestions":[],"confidence":0.9}`)
}

// specScript is the deterministic Spec Generation output.
func specScript(sessionID string) string {
	specJSON := `{"id":"s01","goal":"implement divide","depends_on":[],"write_scope":["src/divide/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":2}`
	return fakeScript("spec-generation", sessionID, `{"specs":[`+specJSON+`],"proposed_commands":[]}`)
}

// workflowScript is the deterministic Workflow Optimization patch.
func workflowScript(sessionID string) string {
	patch := `{"schema":"cflow-workflow-patch-1","operations":[{"op":"add_checkpoint","node_id":"merge-s01"}]}`
	return fakeScript("workflow-optimization", sessionID, patch)
}

// reviewPassScript is the deterministic Review/Adoption PASS verdict.
func reviewPassScript(sessionID string) string {
	return fakeScript("review", sessionID, `{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n"}`)
}

// implementationScript is the deterministic Coding run that writes the
// divide source into the task worktree and commits it.
func implementationScript(sessionID string) string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":"` + sessionID + `","exit_code":0,"resume":"ok","tasks":{"task-s01":{"writes":[{"path":"src/divide/divide.go","content":"package divide\n\n// Divide returns a/b.\nfunc Divide(a, b int) (int, error) {\n\treturn a / b, nil\n}\n"}],"commit":"implement divide"}}}
{"type":"session_started","session_id":"` + sessionID + `","at_ms":0}
{"type":"assistant_message","session_id":"` + sessionID + `","text":"implemented","at_ms":10}
{"type":"session_finished","session_id":"` + sessionID + `","result":{"summary":"implemented"},"at_ms":20}`
}

// finalVerifyPassScript is the deterministic Final Verification PASS.
func finalVerifyPassScript(sessionID string) string {
	return fakeScript("final-verification", sessionID, `{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n"}`)
}

// applyVerifyPassScript is the deterministic Apply Verification PASS.
func applyVerifyPassScript(sessionID string) string {
	return fakeScript("apply-verification", sessionID,
		`{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n- combined result verified\n"}`)
}

// fullFlowScripts is the complete deterministic script set of the main
// chain: planning, check, specs, workflow, adoption/task review,
// implementation, final verification, and apply verification.
func (fx *tuiFixture) fullFlowScripts() []string {
	return []string{
		planScript(fx.next("p")),
		checkPlanPassScript(fx.next("c")),
		specScript(fx.next("s")),
		workflowScript(fx.next("w")),
		implementationScript(fx.next("i")),
		reviewPassScript(fx.next("r")),
		finalVerifyPassScript(fx.next("f")),
		applyVerifyPassScript(fx.next("a")),
	}
}

// stubFakeAgentOnPath creates the deterministic native interactive stub
// ("cflow-fake-agent") the Fake Adapter's interactive resume launches
// and puts it first on PATH (the stub exits 0 immediately).
func (fx *tuiFixture) stubFakeAgentOnPath() string {
	fx.t.Helper()
	dir := fx.t.TempDir()
	stub := filepath.Join(dir, "cflow-fake-agent")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		fx.t.Fatal(err)
	}
	fx.t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// git runs one git command in the fixture repository.
func (fx *tuiFixture) git(args ...string) string {
	fx.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = fx.root
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test User", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fx.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitOK runs one git command tolerating failures (for assertions).
func (fx *tuiFixture) gitOK(args ...string) (string, error) {
	fx.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = fx.root
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// jsonQuote renders s as a JSON string literal.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
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
