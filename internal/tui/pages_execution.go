package tui

// The Execution page (design §6.2, TUI task 14): the live DAG, Task,
// Agent, Cost, and Log panes driven by the Runner's committed events. The
// page only breaks to the Decision Panel on DriveNeedsUser; ordinary Task
// events update the DAG/Inspector.

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/model"
)

// ExecutionPane is one pane of the Execution page.
type ExecutionPane int

const (
	PaneDAG ExecutionPane = iota
	PaneTask
	PaneAgent
	PaneCost
	PaneLog
)

// ExecutionModel is the renderable live Execution page.
type ExecutionModel struct {
	Workflow model.WorkflowID
	Pane     ExecutionPane
	// NodeStates is the last committed DAG state (id -> status).
	NodeStates map[model.NodeID]model.NodeStatus
	// Log is the bounded tail of committed events.
	Log []string
	// AgentNames is the routed agent summary.
	AgentNames []string
}

// NewExecutionModel returns the empty Execution page.
func NewExecutionModel(wf model.WorkflowID) ExecutionModel {
	return ExecutionModel{
		Workflow:   wf,
		NodeStates: map[model.NodeID]model.NodeStatus{},
	}
}

// WithWorkflow returns the page bound to the given workflow (the first
// binding wins; the page never switches workflows by itself).
func (m ExecutionModel) WithWorkflow(wf model.WorkflowID) ExecutionModel {
	if m.Workflow == "" {
		m.Workflow = wf
	}
	return m
}

// Init is the initial command (none).
func (m ExecutionModel) Init() tea.Cmd { return nil }

// OnEvent applies one committed event to the page (ordinary Task events
// only update the DAG/Log — never break the page).
func (m ExecutionModel) OnEvent(ev model.Event) ExecutionModel {
	if ev.Node != "" {
		if st := nodeStatusForEvent(ev); st != "" {
			m.NodeStates[ev.Node] = st
		}
	}
	if len(m.Log) >= 200 {
		m.Log = append(m.Log[:0:0], m.Log[len(m.Log)-100:]...)
	}
	m.Log = append(m.Log, fmt.Sprintf("%d %s %s", ev.Seq, ev.Kind, ev.Text))
	return m
}

// nodeStatusForEvent derives the node status from one committed event.
func nodeStatusForEvent(ev model.Event) model.NodeStatus {
	switch ev.Kind {
	case model.EventNodeStarted:
		return model.NodeRunning
	case model.EventNodeSucceeded:
		return model.NodeSucceeded
	case model.EventNodeFailed:
		return model.NodeFailed
	case model.EventNodeCancelled:
		return model.NodeCancelled
	}
	return ""
}

// Update handles one message on the Execution page. The quit keys
// belong to the root Model's controlled-stop protocol; the page only
// navigates its panes.
func (m ExecutionModel) Update(msg tea.Msg) (ExecutionModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case IsLeft(msg):
			if m.Pane > PaneDAG {
				m.Pane--
			}
		case IsRight(msg):
			if m.Pane < PaneLog {
				m.Pane++
			}
		}
	}
	return m, nil
}

// RenderExecution renders the Execution page.
func RenderExecution(m ExecutionModel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "execution — workflow %s\n", m.Workflow)
	panes := []string{"dag", "task", "agent", "cost", "log"}
	for i, p := range panes {
		if i == int(m.Pane) {
			b.WriteString(">")
		} else {
			b.WriteString(" ")
		}
		b.WriteString(p + "  ")
	}
	b.WriteString("\n\n")
	b.WriteString(renderExecutionPane(m))
	return b.String()
}

func renderExecutionPane(m ExecutionModel) string {
	var b strings.Builder
	switch m.Pane {
	case PaneDAG:
		for id, st := range m.NodeStates {
			fmt.Fprintf(&b, "%s = %s\n", id, st)
		}
	case PaneTask:
		for id, st := range m.NodeStates {
			if strings.HasPrefix(string(id), "task-") {
				fmt.Fprintf(&b, "%s = %s\n", id, st)
			}
		}
	case PaneAgent:
		for _, n := range m.AgentNames {
			fmt.Fprintf(&b, "%s\n", n)
		}
	case PaneCost:
		b.WriteString("cost: (bounded by the approved budgets)\n")
	case PaneLog:
		for _, l := range m.Log {
			fmt.Fprintf(&b, "%s\n", l)
		}
	}
	return b.String()
}

// RenderBlocked renders the Blocked page: only the Runtime legal actions
// (the workspace never offers a fabricated continuation).
func RenderBlocked(m WorkspaceModel) string {
	var b strings.Builder
	b.WriteString("workflow BLOCKED\n")
	if m.Lifecycle != nil {
		fmt.Fprintf(&b, "workflow %s  %s/%s\n", m.Lifecycle.ID, m.Lifecycle.Stage, m.Lifecycle.Runtime)
	}
	b.WriteString("legal actions:\n")
	for _, a := range m.Actions {
		fmt.Fprintf(&b, "  %s\n", string(a))
	}
	b.WriteString("(the workflow stays blocked until the user acts; no automatic retry)\n")
	return b.String()
}
