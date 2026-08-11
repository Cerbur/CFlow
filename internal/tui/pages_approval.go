package tui

// The Approval pages (design §6.2, TUI task 14): the Plan Approval and
// the Execution Approval preview tabs (Plan/Spec/DAG/Diff) with the
// fixed Hash/Scope/Route/Budget/Git Policy Inspector. Enter first enters
// the exact facts preview and Enter again issues the typed approval command.
// The quit keys belong to the root Model's controlled-stop protocol; a page
// never quits by itself.

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/app"
)

// ApprovalTab is one preview tab of the Approval page.
type ApprovalTab int

const (
	TabPlan ApprovalTab = iota
	TabSpec
	TabDAG
	TabDiff
)

// ApprovalModel is the renderable Approval page: the Plan Approval when
// Plan is set, the Execution Approval when Preview is set.
type ApprovalModel struct {
	Plan      app.PlanView
	Preview   app.ExecutionPreviewView
	Tab       ApprovalTab
	Previewed bool // the exact facts preview has been entered
	Confirmed bool // a second Enter requested execution; transient UI state
}

// NewApprovalModel maps the execution preview into the page.
func NewApprovalModel(pv app.ExecutionPreviewView) ApprovalModel {
	return ApprovalModel{Preview: pv}
}

// Init is the initial command (none).
func (m ApprovalModel) Init() tea.Cmd { return nil }

// Update handles one message on the Approval page.
func (m ApprovalModel) Update(msg tea.Msg) (ApprovalModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case IsLeft(msg):
			if m.Tab > TabPlan {
				m.Tab--
			}
		case IsRight(msg):
			if m.Tab < TabDiff {
				m.Tab++
			}
		case IsEnter(msg):
			if !m.Previewed {
				m.Previewed = true
			} else {
				m.Confirmed = true
			}
		}
	}
	return m, nil
}

// RenderPlanApproval renders the Plan Approval page.
func RenderPlanApproval(pv app.PlanView, m ApprovalModel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "plan approval — workflow %s  %s/%s\n", pv.Workflow, pv.Stage, pv.Runtime)
	status := string(pv.PlanStatus)
	if pv.Approved {
		status = "APPROVED"
	}
	fmt.Fprintf(&b, "plan: revision %d  %s\n", pv.Revision, status)
	if pv.Hash != "" {
		fmt.Fprintf(&b, "hash: %s\n", pv.Hash)
	}
	b.WriteString("\n")
	if !m.Previewed {
		b.WriteString("Enter to preview the exact plan facts; Esc back\n")
	} else {
		b.WriteString("PREVIEW READY — Enter approve, Esc back\n")
	}
	return b.String()
}

// RenderApproval renders the Execution Approval page.
func RenderApproval(m ApprovalModel) string {
	var b strings.Builder
	pv := m.Preview
	if pv.Workflow == "" {
		b.WriteString("execution approval — (no preview yet; run the execution dry run first)\n")
		b.WriteString("\nEnter to preview when the exact facts are available; Esc back\n")
		return b.String()
	}
	fmt.Fprintf(&b, "execution approval — workflow %s\n", pv.Workflow)
	tabs := []string{"plan", "spec", "dag", "diff"}
	for i, t := range tabs {
		if i == int(m.Tab) {
			b.WriteString(">")
		} else {
			b.WriteString(" ")
		}
		b.WriteString(t + "  ")
	}
	b.WriteString("\n\n")
	b.WriteString(renderApprovalTab(m))
	b.WriteString("\n")
	b.WriteString(renderApprovalInspector(pv))
	b.WriteString("\n")
	if !m.Previewed {
		b.WriteString("Enter to preview the exact execution facts; Esc back\n")
	} else {
		b.WriteString("PREVIEW READY — Enter approve, Esc back\n")
	}
	return b.String()
}

func renderApprovalTab(m ApprovalModel) string {
	pv := m.Preview
	switch m.Tab {
	case TabPlan:
		if pv.Plan != nil {
			return fmt.Sprintf("plan rev %d — hash %s", pv.Plan.Revision, shortHead(pv.Plan.Hash))
		}
		return "plan: (none)"
	case TabSpec:
		if pv.Spec != nil {
			return fmt.Sprintf("spec rev %d — hash %s", pv.Spec.Revision, shortHead(pv.Spec.Hash))
		}
		return "spec: (none)"
	case TabDAG:
		groups := ""
		for i, g := range pv.ParallelGroups {
			if i > 0 {
				groups += " | "
			}
			groups += strings.Join(g, ",")
		}
		return "dag parallel groups: " + groups
	case TabDiff:
		return "diff: (the candidate change set)"
	}
	return ""
}

func renderApprovalInspector(pv app.ExecutionPreviewView) string {
	var b strings.Builder
	b.WriteString("inspector:\n")
	fmt.Fprintf(&b, "  plan hash:       %s\n", shortHead(pv.PlanHash))
	fmt.Fprintf(&b, "  catalog hash:    %s\n", shortHead(pv.CatalogHash))
	fmt.Fprintf(&b, "  workflow hash:   %s\n", shortHead(pv.WorkflowHash))
	fmt.Fprintf(&b, "  routing hash:    %s\n", shortHead(pv.RoutingHash))
	fmt.Fprintf(&b, "  budget hash:     %s\n", shortHead(pv.BudgetHash))
	fmt.Fprintf(&b, "  commit policy:   %s\n", shortHead(pv.CommitPolicyHash))
	if pv.ChangeSetHash != "" {
		fmt.Fprintf(&b, "  change set:      %s\n", shortHead(pv.ChangeSetHash))
	}
	for _, r := range pv.Routes {
		fmt.Fprintf(&b, "  route: %s (%s)\n", r.Provider, r.Model)
	}
	for _, bu := range pv.Budgets {
		fmt.Fprintf(&b, "  budget: %s max-retry %d\n", bu.NodeID, bu.MaxRetry)
	}
	return b.String()
}
