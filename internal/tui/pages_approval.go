package tui

// The Execution Approval page (design §6.2, TUI task 14): the preview
// tabs (Plan/Spec/DAG/Diff) and the fixed Hash/Scope/Route/Budget/Git
// Policy Inspector. Every confirmation defaults to NO: Enter alone never
// approves; the user must explicitly select Yes.

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

// ApprovalModel is the renderable Execution Approval page.
type ApprovalModel struct {
	Preview  app.ExecutionPreviewView
	Tab      ApprovalTab
	Confirmed bool // the explicit Yes/No confirmation state
	Yes      bool   // the selected confirm answer (Enter alone never confirms)
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
			// Enter alone never approves: it moves the confirmation to
			// the explicit Yes/No choice, or toggles Yes only when the
			// choice was already selected.
			if !m.Confirmed {
				m.Confirmed = true
				m.Yes = false
			} else {
				m.Yes = !m.Yes
			}
		case msg.Code == 'y' || msg.Code == 'Y':
			m.Confirmed = true
			m.Yes = true
		case msg.Code == 'n' || msg.Code == 'N':
			m.Confirmed = true
			m.Yes = false
		case IsQuit(msg) || IsCtrlC(msg):
			return m, tea.Quit
		}
	}
	return m, nil
}

// RenderApproval renders the Approval page.
func RenderApproval(m ApprovalModel) string {
	var b strings.Builder
	pv := m.Preview
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
	if !m.Confirmed {
		b.WriteString("approve? Enter to choose, y/n, q to quit (defaults to no)\n")
	} else {
		mark := "no"
		if m.Yes {
			mark = "yes"
		}
		fmt.Fprintf(&b, "confirm: %s (Enter toggles; Enter alone never approves)\n", mark)
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
