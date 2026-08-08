// Package tui is the full-screen Bubble Tea interface of CFlow (design
// §1: the TUI is the default entry point on an interactive terminal). It
// renders the read-only project workspace and drives the explicit user
// confirmations; it never decides lifecycle transitions itself.
package tui

// The workspace screen (TUI task 10): the workflow column, the lifecycle
// main column, and the inspector; a narrow terminal collapses the
// inspector into the main column.

import (
	"fmt"
	"strings"
)

// RenderWorkspace renders the workspace screen at the given width.
// Narrow screens (below narrowWidth) collapse the inspector into the
// main column.
func RenderWorkspace(m WorkspaceModel, width int) string {
	if width <= 0 {
		width = 80
	}
	left := renderWorkflowColumn(m)
	middle := renderLifecycleColumn(m)
	right := renderInspectorColumn(m)
	if width < narrowWidth {
		return left + "\n\n" + middle + "\n\n" + right
	}
	return left + "\n" + middle + "\n" + right
}

// narrowWidth is the width below which the inspector becomes a detail
// page below the main column.
const narrowWidth = 100

func renderWorkflowColumn(m WorkspaceModel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "project: %s (%s)\n", m.Project.Name, m.Project.Root)
	b.WriteString("workflows:\n")
	for _, w := range m.Workflows {
		mark := " "
		if w.ID == m.Selected.ID {
			mark = ">"
		}
		action := string(w.Action)
		if action == "" {
			action = "-"
		}
		fmt.Fprintf(&b, " %s %s  %-18s %-12s [%s]\n", mark, w.ID, w.Stage, w.Runtime, action)
	}
	if len(m.Workflows) == 0 {
		b.WriteString("   (no workflows yet)\n")
	}
	b.WriteString("\nhealth: ")
	if m.Health.GitAvailable {
		b.WriteString("git ok")
	} else {
		b.WriteString("git unavailable")
	}
	for _, p := range m.Health.Providers {
		state := "ok"
		if !p.Compatible {
			state = "incompatible"
		}
		fmt.Fprintf(&b, "; %s=%s", p.Name, state)
	}
	b.WriteString("\n")
	return b.String()
}

func renderLifecycleColumn(m WorkspaceModel) string {
	var b strings.Builder
	if m.Lifecycle == nil {
		b.WriteString("select a workflow to inspect its lifecycle")
		return b.String()
	}
	lc := m.Lifecycle
	fmt.Fprintf(&b, "workflow %s  %s/%s\n", lc.ID, lc.Stage, lc.Runtime)
	if lc.Target != "" {
		fmt.Fprintf(&b, "target: %s\n", lc.Target)
	}
	if lc.Plan != nil {
		status := lc.Plan.Status
		if lc.Plan.Approved {
			status = "APPROVED"
		}
		fmt.Fprintf(&b, "plan: revision %d %s\n", lc.Plan.Revision, status)
	}
	if lc.Adopted {
		fmt.Fprintf(&b, "workspace: adopted (verified %s)\n", shortHead(lc.Head))
	} else if lc.Head != "" {
		fmt.Fprintf(&b, "workspace: candidate %s\n", shortHead(lc.Head))
	}
	if lc.Blocked {
		b.WriteString("status: BLOCKED (inspect the findings)\n")
	}
	if len(m.Actions) > 0 {
		fmt.Fprintf(&b, "actions: %s\n", strings.Join(actionsText(m.Actions), ", "))
	}
	return b.String()
}

func renderInspectorColumn(m WorkspaceModel) string {
	var b strings.Builder
	b.WriteString("inspector:\n")
	if m.Lifecycle == nil {
		b.WriteString("  -")
		return b.String()
	}
	lc := m.Lifecycle
	fmt.Fprintf(&b, "  stage: %s\n", lc.Stage)
	fmt.Fprintf(&b, "  runtime: %s\n", lc.Runtime)
	if lc.Plan != nil {
		fmt.Fprintf(&b, "  plan revision: %d\n", lc.Plan.Revision)
		fmt.Fprintf(&b, "  plan hash: %s\n", shortHead(lc.Plan.Hash))
	}
	return b.String()
}

func actionsText(actions []Action) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, string(a))
	}
	return out
}

func shortHead(head string) string {
	if len(head) <= 12 {
		return head
	}
	return head[:12]
}
