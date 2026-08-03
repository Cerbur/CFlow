// Package integration: the Commit Policy Safety Stop end to end (Task 17,
// PRD 已确认：Commit Policy 漂移立即安全停止 / 漂移窗口 Commit 的隔离与替代
// 执行). The fixture drives the real pipeline: while a coding Session is
// active, the monitor re-observes the fingerprint probe-less (the
// injected period) and a drifted Git configuration triggers the Safety
// Stop — the gate closes, every Attempt settles INTERRUPTED without
// charge, and the post-stop scan either pauses the Workflow at the
// COMMIT_POLICY_CONFIRMATION_REQUIRED gate (no window Commit) or
// quarantines the window-Commit Branch with its unique audit Ref and
// Blocks the Workflow (window Commit).
package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// appWithPolicy builds the fixture Application with an injected Commit
// Policy monitor period and stop policy (the parallel fixture's app
// mirror).
func (fx *parallelFixture) appWithPolicy(interval time.Duration, scripts ...string) *app.Application {
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
	flow, err := gitflow.NewGitFlow(fx.sup, fx.repo.Root)
	if err != nil {
		fx.t.Fatalf("new gitflow: %v", err)
	}
	a, err := app.New(app.Options{
		Home:               fx.home,
		Project:            app.ProjectFor(fx.repo.Root),
		CflowVersion:       "0.0.0-dev",
		Now:                fx.now,
		IDs:                fx.ids,
		Supervisor:         fx.sup,
		GitFlow:            flow,
		Prompts:            prompts,
		PolicyPollInterval: interval,
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

// driftPolicy changes the effective Commit Policy fingerprint of the
// fixture repository (the monitor observes it probe-less).
func (fx *parallelFixture) driftPolicy(t *testing.T) {
	t.Helper()
	fx.repo.git("config", "user.email", "drifted@example.com")
}

// TestPolicyDriftNoWindowCommitConfirmation drives the monitor: a drifted
// Commit Policy while a coding Session runs triggers the Safety Stop, the
// post-stop scan finds no window Commit (the fixture script writes
// without committing), and the Workflow pauses at the
// COMMIT_POLICY_CONFIRMATION_REQUIRED gate — resume is refused until the
// append-only COMMIT_POLICY Approval binds the exact new Preflight (PRD
// 已确认：执行期间 Commit Policy 漂移确认).
func TestPolicyDriftNoWindowCommitConfirmation(t *testing.T) {
	fx := newParallelFixture(t)
	wf, _ := fx.driveToExecutionApproval(t)
	fx.driftPolicy(t)
	// The session blocks deterministically at the declared stop boundary
	// (the Fake's stop_after) until the Safety Stop cancels it: the
	// monitor's recompute always observes the drift while the Session is
	// live.
	a := fx.appWithPolicy(5*time.Millisecond, stopAfterImplementationScript("i1", 2))
	if _, err := a.Execute(context.Background(), app.DispatchCommand{Workflow: wf}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	iv := fx.Inspect(wf)
	if iv.Status.Runtime != model.RuntimePaused {
		t.Fatalf("runtime = %s, want PAUSED at the confirmation gate", iv.Status.Runtime)
	}
	finding := false
	for _, f := range iv.Status.Findings {
		if f.Code == model.CodeCommitPolicyConfirmationRequired {
			finding = true
		}
	}
	if !finding {
		t.Fatalf("the COMMIT_POLICY_CONFIRMATION_REQUIRED finding must be persisted")
	}
	charged := false
	for _, att := range iv.Attempts {
		if att.RetryCharged {
			charged = true
		}
	}
	if charged {
		t.Fatalf("a policy safety stop must never charge the retry budget")
	}
	// Resume is refused while the confirmation is pending.
	_, err := a.Execute(context.Background(), app.ResumeWorkflowCommand{Workflow: wf})
	if code, ok := model.CodeOf(err); !ok || code != model.CodeCommitPolicyConfirmationRequired {
		t.Fatalf("resume before the confirmation must be refused, got %v", err)
	}
	// The confirmation gate shows the exact new Preflight; the append-only
	// COMMIT_POLICY Approval binds it and unblocks resume.
	qview, err := a.Query(context.Background(), app.PolicyConfirmationQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("confirmation query: %v", err)
	}
	pv := qview.(app.PolicyConfirmationView)
	if !pv.Pending || pv.PreflightRevision < 1 || pv.Fingerprint == "" || pv.PreflightHash == "" {
		t.Fatalf("confirmation view = %+v, want the pending gate", pv)
	}
	if _, err := a.Execute(context.Background(), app.CommitPolicyConfirmCommand{
		Workflow: wf, PreflightRevision: pv.PreflightRevision,
		PreflightHash: pv.PreflightHash, Fingerprint: pv.Fingerprint,
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := a.Execute(context.Background(), app.ResumeWorkflowCommand{Workflow: wf}); err != nil {
		t.Fatalf("resume after the confirmation: %v", err)
	}
	iv = fx.Inspect(wf)
	if iv.Status.Runtime != model.RuntimeRunning {
		t.Fatalf("runtime = %s, want RUNNING after the confirmation and resume", iv.Status.Runtime)
	}
}

// stopAfterImplementationScript is the implementation fixture with the
// Fake's deterministic stop boundary (stop_after): the run blocks at the
// declared frame boundary until the Safety Stop cancels its context, so
// the monitor always observes the drift while the Session is live.
func stopAfterImplementationScript(sessionID string, stopAfter int) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":%s,"exit_code":0,"resume":"ok","stop_after":%d,"writes":[{"path":"src/calc/divide.go","content":"package calc\n\n// Divide returns a/b.\nfunc Divide(a, b int) (int, error) {\n\treturn 0, nil\n}\n"}]}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Implemented the calculator task.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"summary":"implemented"},"at_ms":20}`,
		strconv.Quote(sessionID), stopAfter, strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID))
}
