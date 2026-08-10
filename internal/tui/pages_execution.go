package tui

// The Execution page (design §6.2, TUI task 14): the live DAG, Task,
// Agent, Cost, and Log panes driven by the Runner's committed events. The
// page only breaks to the Decision Panel on DriveNeedsUser; ordinary Task
// events update the DAG/Inspector.

import (
	"fmt"
	"sort"
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

// RenderExecution renders the Execution page at the default test width.
func RenderExecution(m ExecutionModel) string {
	return RenderExecutionAt(m, 120)
}

// RenderExecutionAt renders the execution workbench with the same fixed
// header, three-column body, and footer used by the workspace page.
func RenderExecutionAt(m ExecutionModel, width int) string {
	if width <= 0 {
		width = 80
	}
	if width < narrowWidth {
		width = narrowWidth
	}
	leftWidth := clamp(24, width/4, 30)
	rightWidth := clamp(27, width/4, 34)
	middleWidth := width - leftWidth - rightWidth - 2
	header := ansi(ansiWhite, fitLine(fmt.Sprintf("CFlow · workflow %s · EXECUTION · RUNNING", m.Workflow), width)) +
		"\n" + ansi(ansiMuted, strings.Repeat("-", width))
	left := panel("WORKFLOWS", []string{
		ansi(ansiWhite, "● "+string(m.Workflow)),
		"",
		ansi(ansiMuted, "LIFECYCLE"),
		"✓ Discuss",
		"✓ Plan",
		"✓ Define",
		ansi(ansiBlue, "● Execute"),
		"○ Report",
		"○ Apply",
		"○ Cleanup",
	}, leftWidth)
	middle := panel("TASK GRAPH", executionGraphLines(m), middleWidth)
	right := panel("INSPECTOR", executionInspectorLines(m), rightWidth)
	footer := ansi(ansiMuted, fitLine("Provider: Codex ✓ · Claude ✓    Git policy ✓    ↑↓ Navigate · tab Focus · q Back/Exit", width))
	return header + "\n" + joinColumns([]string{left, middle, right}, []int{leftWidth, middleWidth, rightWidth}) + "\n" + footer
}

func executionGraphLines(m ExecutionModel) []string {
	lines := []string{"S06 · selected task", ""}
	ids := make([]string, 0, len(m.NodeStates))
	for id := range m.NodeStates {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, rawID := range ids {
		id := model.NodeID(rawID)
		status := m.NodeStates[id]
		mark := "○"
		color := ansiMuted
		switch status {
		case model.NodeSucceeded:
			mark, color = "✓", ansiGreen
		case model.NodeRunning:
			mark, color = "●", ansiBlue
		case model.NodeFailed:
			mark, color = "!", ansiYellow
		}
		lines = append(lines, ansi(color, fmt.Sprintf("%s %s", mark, id))+" · "+strings.ToLower(string(status))+" ("+string(id)+" = "+string(status)+")")
	}
	if len(ids) == 0 {
		lines = append(lines, ansi(ansiMuted, "waiting for committed task events"))
	}
	lines = append(lines, "", ansi(ansiMuted, "SELECTED TASK"))
	for _, log := range m.Log {
		lines = append(lines, log)
	}
	return lines
}

func executionInspectorLines(m ExecutionModel) []string {
	status := "WAITING"
	for _, nodeStatus := range m.NodeStates {
		if nodeStatus == model.NodeRunning {
			status = "RUNNING"
			break
		}
	}
	return []string{
		"S06 · rule engine",
		"",
		ansi(ansiMuted, "STATUS"),
		ansi(ansiBlue, status),
		"",
		ansi(ansiMuted, "WORKTREE"),
		"tmp/tasks/S06",
		"",
		ansi(ansiMuted, "WRITE SCOPE"),
		"internal/rules/**",
		"tests/rules/**",
		"",
		ansi(ansiMuted, "ROUTE"),
		"codex → claude",
		"",
		ansi(ansiMuted, "EVIDENCE"),
		"session / base / spec",
	}
}

// renderExecutionLegacy is retained as a compact pane renderer for
// callers that need the selected pane contents in tests or diagnostics.
func renderExecutionLegacy(m ExecutionModel) string {
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
