package tui

// Approval and Execution page tests: Enter enters an explicit preview and
// the next Enter requests execution; ordinary Task events
// only update the Execution DAG — the page never breaks on progress.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// approvalPreview is a minimal execution preview.
func approvalPreview() app.ExecutionPreviewView {
	return app.ExecutionPreviewView{
		Workflow: "wf-1", PlanHash: "plan-h", CatalogHash: "cat-h",
		WorkflowHash: "wf-h", RoutingHash: "r-h", BudgetHash: "b-h",
		CommitPolicyHash: "cp-h", ChangeSetHash: "cs-h",
		Routes:  []app.RoutePreview{{Provider: "fake", Model: "default"}},
		Budgets: []app.BudgetPreview{{NodeID: "task-s01", MaxRetry: 2}},
	}
}

// update applies one key press to an Approval model.
func update(m ApprovalModel, msg tea.KeyMsg) (ApprovalModel, tea.Cmd) {
	u, cmd := m.Update(msg)
	return u, cmd
}

// TestApprovalRequiresPreviewThenEnter verifies the two-step confirmation.
func TestApprovalDefaultsToNo(t *testing.T) {
	m := NewApprovalModel(approvalPreview())
	m, _ = update(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.Previewed || m.Confirmed {
		t.Fatalf("first Enter did not enter preview: %+v", m)
	}
	m, _ = update(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.Confirmed {
		t.Fatalf("second Enter did not request Enter-only execution: %+v", m)
	}
}

func TestApprovalYNAreOrdinaryInput(t *testing.T) {
	m := NewApprovalModel(approvalPreview())
	for _, key := range []rune{'y', 'Y', 'n', 'N'} {
		m, _ = update(m, tea.KeyPressMsg{Code: key})
		if m.Confirmed {
			t.Fatalf("%q acted as confirmation: %+v", key, m)
		}
	}
}

func TestTerminalYNAreOrdinaryInput(t *testing.T) {
	m := NewTerminalModel()
	for _, key := range []rune{'y', 'Y', 'n', 'N'} {
		m, _ = m.Update(tea.KeyPressMsg{Code: key})
		if m.Confirmed || m.Previewed {
			t.Fatalf("%q acted as confirmation: %+v", key, m)
		}
	}
	got := RenderTerminal(m)
	for _, forbidden := range []string{"y/n", "q to quit", "defaults to no"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("terminal copy advertises legacy confirmation %q: %s", forbidden, got)
		}
	}
}

// TestApprovalEnterAloneNeverApproves: Enter toggles the confirmation to
// no and never approves directly.
func TestApprovalEnterAloneNeverApproves(t *testing.T) {
	m := NewApprovalModel(approvalPreview())
	for i := 0; i < 3; i++ {
		m, _ = update(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	if !m.Confirmed {
		t.Fatalf("repeated Enter used the legacy answer state: %+v", m)
	}
}

// TestApprovalTabsNavigate: left/right switch the preview tabs.
func TestApprovalTabsNavigate(t *testing.T) {
	m := NewApprovalModel(approvalPreview())
	if m.Tab != TabPlan {
		t.Fatalf("initial tab = %d", m.Tab)
	}
	m, _ = update(m, tea.KeyPressMsg{Code: tea.KeyRight})
	if m.Tab != TabSpec {
		t.Fatalf("tab after right = %d", m.Tab)
	}
	m, _ = update(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.Tab != TabPlan {
		t.Fatalf("tab after left = %d", m.Tab)
	}
	got := RenderApproval(m)
	for _, want := range []string{"execution approval", "plan hash:", "commit policy:", "Enter to preview"} {
		if !strings.Contains(got, want) {
			t.Fatalf("approval render misses %q:\n%s", want, got)
		}
	}
}

// TestExecutionOrdinaryEventsUpdateDAG: an ordinary Task event updates
// the DAG and the page stays on the Execution view (no break to a
// decision panel).
func TestExecutionOrdinaryEventsUpdateDAG(t *testing.T) {
	m := NewExecutionModel("wf-1")
	m = m.OnEvent(model.Event{Seq: 1, Kind: model.EventNodeStarted, Node: "task-s01", Text: "started"})
	m = m.OnEvent(model.Event{Seq: 2, Kind: model.EventNodeSucceeded, Node: "task-s01", Text: "succeeded"})
	if m.NodeStates["task-s01"] != model.NodeSucceeded {
		t.Fatalf("dag state = %+v", m.NodeStates)
	}
	if len(m.Log) != 2 {
		t.Fatalf("log = %v", m.Log)
	}
	got := RenderExecution(m)
	if !strings.Contains(got, "task-s01 = SUCCEEDED") {
		t.Fatalf("execution render misses the DAG state:\n%s", got)
	}
}

// TestExecutionOnlyNeedsUserBreaks: the page model itself never breaks;
// the runner surface (Task 13) reports DriveNeedsUser and the TUI
// switches to the Decision Panel — ordinary progress never does.
func TestExecutionOnlyNeedsUserBreaks(t *testing.T) {
	m := NewExecutionModel("wf-1")
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if m.Pane != PaneTask {
		t.Fatalf("pane = %d", m.Pane)
	}
	// Ordinary events keep the page on the Execution view.
	m = m.OnEvent(model.Event{Seq: 3, Kind: model.EventRunStarted, Text: "run started"})
	if m.Pane != PaneTask {
		t.Fatalf("an ordinary event switched the page away")
	}
}

func TestRenderExecutionUsesWorkbenchLayout(t *testing.T) {
	m := NewExecutionModel("wf-1")
	m.NodeStates["S06"] = model.NodeRunning
	m.Log = []string{"12 Agent session resumed"}
	got := RenderExecution(m)
	for _, want := range []string{"CFlow", "EXECUTION", "TASK GRAPH", "INSPECTOR", "S06", "Provider:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("execution render misses %q:\n%s", want, got)
		}
	}
}
