// The redacted Markdown rendering of the Final Execution Report (Task 18,
// PRD 最终报告示例). Same-package split of the observe package: no public
// seam added.
package observe

import (
	"fmt"
	"io"
	"strings"
	"time"

	"cflow.local/cflow/internal/security"
)

// ---------------------------------------------------------------------------
// redacted Markdown rendering (PRD 最终报告示例)
// ---------------------------------------------------------------------------

// RenderMarkdown renders the report as redacted Markdown. Every
// free-form value passes through the Redactor; a redaction failure
// replaces the value with the stable placeholder, so raw content never
// reaches the terminal (design 19.2).
func RenderMarkdown(r Report, reg security.Registry) string {
	var sb strings.Builder
	red := security.NewRedactor(reg)
	text := func(s string) string {
		if s == "" {
			return "-"
		}
		frame, err := red.WriteFrame([]byte(s))
		if err != nil {
			return "[REDACTED]"
		}
		flushed, err := red.Flush()
		if err != nil {
			return "[REDACTED]"
		}
		return frame.Text + flushed.Text
	}
	renderReport(&sb, r, text)
	return sb.String()
}

func renderReport(w io.Writer, r Report, text func(string) string) {
	fmt.Fprintln(w, "# CFlow Execution Report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Workflow: %s\n", r.Workflow.ID)
	fmt.Fprintf(w, "Plan Revision: %d\n", r.Workflow.PlanRevision)
	fmt.Fprintf(w, "Result: %s\n", r.Result)
	fmt.Fprintf(w, "Target Branch: %s\n", text(r.Workflow.TargetBranch))
	fmt.Fprintf(w, "Integration Branch: %s\n", text(r.Workflow.IntegrationBranch))
	fmt.Fprintf(w, "Stage: %s  Runtime: %s\n", r.Workflow.Stage, r.Workflow.Runtime)
	fmt.Fprintf(w, "Generated: %s\n", r.GeneratedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "Binary: cflow %s (source %s, go %s, %s/%s)\n",
		r.Build.Version, r.Build.SourceCommit, r.Build.GoVersion, r.Build.OS, r.Build.Arch)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Summary")
	fmt.Fprintf(w, "Tasks: %d\n", r.Summary.Tasks)
	fmt.Fprintf(w, "Completed: %d\n", r.Summary.Completed)
	fmt.Fprintf(w, "Retries: %d\n", r.Summary.Retries)
	fmt.Fprintf(w, "Agent Sessions: %d\n", r.Summary.Sessions)
	fmt.Fprintf(w, "Duration: %s\n", text(r.Summary.Duration))

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Commits")
	for _, c := range r.Commits {
		heads := "-"
		if len(c.Commits) > 0 {
			heads = strings.Join(c.Commits, ", ")
		}
		fmt.Fprintf(w, "| %s | %s | %s |\n", text(c.Task), text(heads), text(c.MergeHead))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Approved Artifacts")
	for _, art := range r.Artifacts {
		state := "approved"
		if art.Revision == 0 {
			state = "missing"
		} else if art.Stale {
			state = "STALE"
		}
		fmt.Fprintf(w, "| %s | %d | %s | %s |\n", art.Type, art.Revision, text(art.Hash), state)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Git Commit Policy")
	fmt.Fprintf(w, "Preflight Revision: %d\n", r.CommitPolicy.PreflightRevision)
	fmt.Fprintf(w, "Fingerprint: %s\n", text(r.CommitPolicy.Fingerprint))
	fmt.Fprintf(w, "Verified Commits: %d\n", r.CommitPolicy.VerifiedCommits)
	fmt.Fprintf(w, "Policy Mismatches: %d\n", r.CommitPolicy.PolicyMismatches)
	fmt.Fprintf(w, "Quarantined Branches: %d\n", r.CommitPolicy.QuarantinedBranches)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Verification")
	for _, v := range r.Verification {
		fmt.Fprintf(w, "| %s / %s | %s | %s | %s |\n",
			text(v.CommandID), text(v.Purpose), verdict(v.Passed), text(v.Review), text(v.Hash))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Local Data Protection")
	fmt.Fprintf(w, "At-rest Encryption: %s\n", text(r.Security.AtRestEncryption))
	fmt.Fprintf(w, "Directory/File Modes: %s / %s\n", text(r.Security.HomeMode), text(r.Security.FileMode))
	fmt.Fprintf(w, "Redactor Revision: %s\n", text(r.Security.RedactionRevision))
	fmt.Fprintf(w, "Raw Provider Frames Persisted: no\n")
	fmt.Fprintln(w, "Retention: indefinite; user-controlled account/disk/backup protection")

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## State Compatibility")
	fmt.Fprintf(w, "Database Schema: %d\n", r.Migration.SchemaVersion)
	checks := "verified"
	if !r.Migration.ChecksumsVerified {
		checks = "not verified"
	}
	fmt.Fprintf(w, "Migration Registry: checksums %s\n", checks)
	for _, m := range r.Migration.Applied {
		fmt.Fprintf(w, "  v%d %s sha256:%s\n", m.Version, text(m.ID), text(m.SHA256))
	}
	backup := "verified"
	if !r.Migration.BackupVerified {
		backup = "not verified"
	}
	fmt.Fprintf(w, "Latest Migration Backup: backup %s\n", backup)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Agent Runtime Evidence")
	for _, s := range r.Sessions {
		fmt.Fprintf(w, "| %s | %s | %s | %s |\n", s.Purpose, text(s.Provider), string(s.ID), s.Status)
	}
	fmt.Fprintln(w, "Permission Boundary: provider defaults; not sandboxed by CFlow (design 19.3)")

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Permissions and Trust Boundary")
	fmt.Fprintf(w, "Trust Boundary: %s\n", text(r.Permissions.TrustBoundary))

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Apply")
	fmt.Fprintf(w, "Status: %s (%s)\n", r.Apply.Status, text(r.Apply.Detail))

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Cleanup")
	fmt.Fprintf(w, "Status: %s (%s)\n", r.Cleanup.Status, text(r.Cleanup.Detail))

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Findings")
	if len(r.Findings) == 0 {
		fmt.Fprintln(w, "none")
	}
	for _, f := range r.Findings {
		kind := "non-blocking"
		if f.Blocking {
			kind = "blocking"
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s |\n",
			text(string(f.ID)), f.Code, text(string(f.Subject)), kind, text(f.Text))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Event Export")
	fmt.Fprintf(w, "Path: %s\n", text(r.EventExport.Path))
	fmt.Fprintf(w, "Event Sequence: %d..%d\n", r.EventExport.From, r.EventExport.To)
	if r.EventExport.Stable {
		fmt.Fprintln(w, "Stable: yes (rebuildable from the SQLite Event sequence; never the recovery stream)")
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Remaining Risks")
	for _, risk := range r.Risks {
		fmt.Fprintf(w, "- %s\n", text(risk))
	}
}

// verdict renders a verification outcome.
func verdict(passed bool) string {
	if passed {
		return "Passed"
	}
	return "Failed"
}
