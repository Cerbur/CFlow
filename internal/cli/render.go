package cli

// Line-oriented rendering of Views and Outcomes (design 20): the CLI
// renders redacted bounded fields and immutable references only. Every
// free-form value passes through the Redactor before terminal display
// (design 19.2).

import (
	"fmt"
	"io"
	"strings"
	"time"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// renderer owns one Redactor for one render pass.
type renderer struct {
	red *security.Redactor
}

func newRenderer(reg security.Registry) *renderer {
	return &renderer{red: security.NewRedactor(reg)}
}

// text redacts one bounded text value. On any redaction failure the value
// is replaced by the stable placeholder: raw content never reaches the
// terminal.
func (r *renderer) text(s string) string {
	if s == "" {
		return ""
	}
	frame, err := r.red.WriteFrame([]byte(s))
	if err != nil {
		return "[REDACTED]"
	}
	flushed, err := r.red.Flush()
	if err != nil {
		return "[REDACTED]"
	}
	return frame.Text + flushed.Text
}

func (r *renderer) orDash(s string) string {
	if s == "" {
		return "-"
	}
	return r.text(s)
}

// ---------------------------------------------------------------------------
// read projections
// ---------------------------------------------------------------------------

// renderPlan renders the active Plan Revision's review state.
func renderPlan(w io.Writer, v app.PlanView, reg security.Registry) {
	r := newRenderer(reg)
	if v.Revision == 0 {
		fmt.Fprintln(w, "no plan")
		return
	}
	fmt.Fprintf(w, "workflow: %s\n", v.Workflow)
	fmt.Fprintf(w, "stage: %s\n", v.Stage)
	fmt.Fprintf(w, "runtime: %s\n", v.Runtime)
	fmt.Fprintf(w, "plan status: %s\n", v.PlanStatus)
	fmt.Fprintf(w, "revision %d\n", v.Revision)
	fmt.Fprintf(w, "sha256: %s\n", r.text(v.Hash))
	if v.Approved {
		fmt.Fprintln(w, "approved: true")
	}
}

func renderList(w io.Writer, v app.ListView, reg security.Registry) {
	r := newRenderer(reg)
	if len(v.Workflows) == 0 {
		fmt.Fprintln(w, "no workflows")
		return
	}
	fmt.Fprintln(w, "workflow\tstage\truntime\ttarget branch")
	for _, ws := range v.Workflows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ws.ID, ws.Stage, ws.Runtime, r.text(ws.TargetBranch))
	}
}

func renderStatus(w io.Writer, v app.StatusView, reg security.Registry) {
	r := newRenderer(reg)
	if v.Workflow == "" {
		fmt.Fprintln(w, "no workflows")
		return
	}
	fmt.Fprintf(w, "workflow: %s\n", v.Workflow)
	fmt.Fprintf(w, "stage: %s\n", v.Stage)
	fmt.Fprintf(w, "runtime: %s\n", v.Runtime)
	fmt.Fprintf(w, "target branch: %s\n", r.orDash(v.TargetBranch))
	fmt.Fprintf(w, "base commit: %s\n", r.orDash(v.BaseCommit))
	fmt.Fprintf(w, "integration branch: %s\n", r.orDash(v.IntegrationBranch))
	if v.PlanStatus != "" {
		fmt.Fprintf(w, "plan: %s\n", v.PlanStatus)
		if v.PlanRevision > 0 {
			fmt.Fprintf(w, "plan revision: %d\n", v.PlanRevision)
			fmt.Fprintf(w, "plan sha256: %s\n", r.text(v.PlanHash))
		}
		if v.PlanApproved {
			fmt.Fprintln(w, "plan approved: true")
		}
	}
	fmt.Fprintf(w, "integration head: %s\n", r.orDash(v.IntegrationHead))
	if v.Run != nil {
		gate := "closed"
		if v.Run.DispatchGate {
			gate = "open"
		}
		fmt.Fprintf(w, "run: %s %s (dispatch %s)\n", v.Run.ID, v.Run.Status, gate)
	} else {
		fmt.Fprintln(w, "run: -")
	}
	if len(v.Processes) == 0 {
		fmt.Fprintln(w, "processes: none")
	} else {
		fmt.Fprintln(w, "processes:")
		for _, p := range v.Processes {
			fmt.Fprintf(w, "  %s %s %s\n", p.ID, p.Purpose, p.Status)
		}
	}
	if len(v.Findings) == 0 {
		fmt.Fprintln(w, "findings: none")
	} else {
		fmt.Fprintln(w, "findings:")
		for _, f := range v.Findings {
			fmt.Fprintf(w, "  %s %s %s %s\n", f.ID, f.Code, f.Scope, r.text(f.Text))
		}
	}
}

func renderInspect(w io.Writer, v app.InspectView, reg security.Registry) {
	r := newRenderer(reg)
	renderStatus(w, v.Status, reg)
	if v.Plan == nil {
		fmt.Fprintln(w, "plan: none")
	} else {
		fmt.Fprintf(w, "plan: revision %d %s %s\n", v.Plan.Revision, v.Plan.Status, r.RefOrDash(v.Plan.Artifact))
	}
	fmt.Fprintf(w, "nodes: %d\n", len(v.Nodes))
	if len(v.Nodes) > 0 {
		for _, n := range v.Nodes {
			fmt.Fprintf(w, "  %s %s %s (retry %d/%d)\n", n.ID, n.Kind, n.Status, n.RetryCharged, n.RetryBudget)
		}
	}
	fmt.Fprintf(w, "attempts: %d\n", len(v.Attempts))
	fmt.Fprintf(w, "approvals: %d\n", len(v.Approvals))
	fmt.Fprintf(w, "sessions: %d\n", len(v.Sessions))
	fmt.Fprintf(w, "runs: %d\n", len(v.Runs))
	for _, run := range v.Runs {
		gate := "closed"
		if run.DispatchGate {
			gate = "open"
		}
		fmt.Fprintf(w, "  %s %s (dispatch %s)\n", run.ID, run.Status, gate)
	}
	fmt.Fprintf(w, "quarantines: %d\n", len(v.Quarantines))
	fmt.Fprintf(w, "apply attempts: %d\n", len(v.ApplyAttempts))
	fmt.Fprintf(w, "cleanup attempts: %d\n", len(v.CleanupAttempts))
	fmt.Fprintf(w, "pending effects: %d\n", len(v.PendingEffects))
	for _, pe := range v.PendingEffects {
		fmt.Fprintf(w, "  %s\n", pe)
	}
}

func renderLogs(w io.Writer, v app.LogsView, reg security.Registry) {
	r := newRenderer(reg)
	if len(v.Events) == 0 {
		fmt.Fprintln(w, "no events")
		return
	}
	for _, e := range v.Events {
		at := e.At.UTC().Format(time.RFC3339)
		code := ""
		if e.Code != "" {
			code = " " + string(e.Code)
		}
		fmt.Fprintf(w, "%d %s%s %s %s\n", e.Seq, e.Kind, code, at, r.text(e.Text))
	}
}

// renderExecutionPreview renders the Execution Approval Dry Run (PRD
// 已确认：两个用户批准门): the exact Revisions and hashes, routes,
// budgets, Commit Preflight fingerprint, trust boundary, Worktree plan,
// parallel groups, and command identities.
func renderExecutionPreview(w io.Writer, v app.ExecutionPreviewView, reg security.Registry) {
	r := newRenderer(reg)
	fmt.Fprintf(w, "workflow: %s\n", v.Workflow)
	fmt.Fprintf(w, "stage: %s\n", v.Stage)
	fmt.Fprintf(w, "runtime: %s\n", v.Runtime)
	renderPreviewRef(w, r, "plan", v.Plan)
	renderPreviewRef(w, r, "spec", v.Spec)
	renderPreviewRef(w, r, "catalog", v.Catalog)
	renderPreviewRef(w, r, "workflow", v.WorkflowArtifact)
	if v.Preflight != nil {
		fmt.Fprintf(w, "preflight: revision %d\n", v.Preflight.Revision)
		fmt.Fprintf(w, "  sha256: %s\n", r.text(v.Preflight.EvidenceHash))
		fmt.Fprintf(w, "  fingerprint: %s\n", r.text(v.Preflight.Fingerprint))
		if v.Preflight.GitVersion != "" {
			fmt.Fprintf(w, "  git: %s\n", r.text(v.Preflight.GitVersion))
		}
	} else {
		fmt.Fprintln(w, "preflight: none")
	}
	if len(v.Routes) == 0 {
		fmt.Fprintln(w, "routes: none")
	} else {
		fmt.Fprintln(w, "routes:")
		for _, rt := range v.Routes {
			fmt.Fprintf(w, "  %s -> %s (%s)\n", rt.NodeID, r.text(rt.Provider), r.text(rt.Model))
		}
	}
	if len(v.Budgets) == 0 {
		fmt.Fprintln(w, "budgets: none")
	} else {
		fmt.Fprintln(w, "budgets:")
		for _, b := range v.Budgets {
			fmt.Fprintf(w, "  %s timeout %ds retry %d budget %v\n", b.NodeID, b.TimeoutSeconds, b.MaxRetry, b.Budget)
		}
	}
	fmt.Fprintf(w, "cost: %d agent runs, %d retries\n", v.TotalAgentRuns, v.TotalRetries)
	if len(v.ParallelGroups) == 0 {
		fmt.Fprintln(w, "parallel groups: none")
	} else {
		fmt.Fprintln(w, "parallel groups:")
		for i, g := range v.ParallelGroups {
			fmt.Fprintf(w, "  group %d: %s\n", i, strings.Join(g, ", "))
		}
	}
	if len(v.Locks) == 0 {
		fmt.Fprintln(w, "resource locks: none")
	} else {
		fmt.Fprintln(w, "resource locks:")
		for _, l := range v.Locks {
			fmt.Fprintf(w, "  %s -> %s\n", l.NodeID, r.text(l.Lock))
		}
	}
	if len(v.CommandIdentities) == 0 {
		fmt.Fprintln(w, "command identities: none")
	} else {
		fmt.Fprintln(w, "command identities:")
		for _, c := range v.CommandIdentities {
			hash := c.SHA256
			if len(hash) > 12 {
				hash = hash[:12]
			}
			fmt.Fprintf(w, "  %s %s sha256:%s (%s)\n", c.CommandID, r.text(c.Executable), r.text(hash), c.Purpose)
		}
	}
	fmt.Fprintln(w, "worktree plan:")
	for _, line := range v.WorktreePlan {
		fmt.Fprintf(w, "  %s\n", r.text(line))
	}
	if v.TrustBoundary != "" {
		fmt.Fprintf(w, "trust boundary: %s\n", r.text(v.TrustBoundary))
	}
	if len(v.Findings) == 0 {
		fmt.Fprintln(w, "findings: none")
	} else {
		fmt.Fprintln(w, "findings:")
		for _, f := range v.Findings {
			fmt.Fprintf(w, "  %s %s %s\n", f.ID, f.Code, r.text(f.Text))
		}
	}
}

func renderPreviewRef(w io.Writer, r *renderer, name string, ref *model.ArtifactRef) {
	if ref == nil {
		fmt.Fprintf(w, "%s: none\n", name)
		return
	}
	fmt.Fprintf(w, "%s: revision %d\n", name, ref.Revision)
	fmt.Fprintf(w, "  sha256: %s\n", r.text(ref.Hash))
}

// ---------------------------------------------------------------------------
// mutation outcomes
// ---------------------------------------------------------------------------

func renderOutcome(w io.Writer, cmd app.Command, out app.Outcome, reg security.Registry) {
	r := newRenderer(reg)
	switch cmd.(type) {
	case app.DryRunCommand:
		if out.Cleanup == nil {
			fmt.Fprintln(w, "no cleanup manifest produced")
			return
		}
		c := out.Cleanup
		fmt.Fprintf(w, "cleanup manifest %s (%s, %d items)\n", c.ID, c.Status, len(c.Items))
		for _, it := range c.Items {
			fmt.Fprintf(w, "  [%d] %s %s branch=%s head=%s\n",
				it.Index, it.Kind, r.text(it.CanonicalPath), r.orDash(it.Branch), r.orDash(it.ExpectedHead))
		}
		fmt.Fprintf(w, "manifest: %s\n", c.Manifest)
	case app.CreateWorkflowCommand:
		fmt.Fprintf(w, "workflow %s created\n", out.Workflow)
		fmt.Fprintf(w, "  stage: %s\n", out.Stage)
		fmt.Fprintf(w, "  runtime: %s\n", out.Runtime)
	case app.DiscussRequirementCommand, app.GeneratePlanCommand, app.CheckPlanCommand:
		fmt.Fprintf(w, "workflow %s\n", out.Workflow)
		fmt.Fprintf(w, "  stage: %s\n", out.Stage)
		fmt.Fprintf(w, "  runtime: %s\n", out.Runtime)
		if out.SessionID != "" {
			fmt.Fprintf(w, "  session: %s\n", out.SessionID)
		}
		fmt.Fprintf(w, "  events committed: %d\n", len(out.Events))
		for _, f := range out.Findings {
			fmt.Fprintf(w, "  finding: %s %s\n", f.Code, r.text(f.Text))
		}
	case app.ApprovePlanCommand:
		fmt.Fprintf(w, "workflow %s\n", out.Workflow)
		fmt.Fprintln(w, "plan approved")
		fmt.Fprintf(w, "  stage: %s\n", out.Stage)
	default:
		fmt.Fprintf(w, "workflow %s %s\n", out.Workflow, strings.ToLower(string(out.Runtime)))
		fmt.Fprintf(w, "  stage: %s\n", out.Stage)
		fmt.Fprintf(w, "  events committed: %d\n", len(out.Events))
		if out.Restricted {
			fmt.Fprintln(w, "  restricted safety path: managed processes stopped (design 6.1)")
		}
		for _, f := range out.Findings {
			fmt.Fprintf(w, "  finding: %s %s\n", f.Code, r.text(f.Text))
		}
	}
	if out.ExportErr != nil {
		fmt.Fprintf(w, "warning: events export failed: %v\n", out.ExportErr)
	}
}

// RefOrDash renders one ArtifactRef or a dash.
func (r *renderer) RefOrDash(ref model.ArtifactRef) string {
	if ref.Hash == "" {
		return "-"
	}
	return ref.String()
}
