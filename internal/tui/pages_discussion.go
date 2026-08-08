package tui

// The native discussion Return Page (design §9.2, TUI task 12): after
// one interactive turn the page offers Continue Same Session, Finish,
// Switch Agent, Pause, and Cancel. Navigation only updates the UI
// selection; Finish/Continue are explicit commands.

import (
	"fmt"
	"strings"

	"cflow.local/cflow/internal/app"
)

// DiscussionReturnAction is one legal action of the Return Page.
type DiscussionReturnAction int

const (
	ReturnContinue DiscussionReturnAction = iota
	ReturnFinish
	ReturnSwitch
	ReturnPause
	ReturnCancel
)

// DiscussionPage is the renderable Return Page state.
type DiscussionPage struct {
	Workflow  string
	Session   string
	Provider  string
	ChangeSet string
	Actions   []DiscussionReturnAction
	// Selected is the highlighted action index.
	Selected int
}

// MapDiscussionReturn maps the app's Return projection to the page.
func MapDiscussionReturn(v app.DiscussionReturnView) DiscussionPage {
	p := DiscussionPage{
		Workflow: string(v.Workflow),
		Session:  string(v.Session),
		Provider: v.Provider,
	}
	if v.ChangeSet != nil {
		p.ChangeSet = fmt.Sprintf("rev %d (%s)", v.ChangeSet.Revision, shortHead(v.ChangeSet.Hash))
	}
	for _, a := range v.Actions {
		switch a {
		case "continue":
			p.Actions = append(p.Actions, ReturnContinue)
		case "finish":
			p.Actions = append(p.Actions, ReturnFinish)
		case "switch-agent":
			p.Actions = append(p.Actions, ReturnSwitch)
		case "pause":
			p.Actions = append(p.Actions, ReturnPause)
		case "cancel":
			p.Actions = append(p.Actions, ReturnCancel)
		}
	}
	return p
}

// ActionLabel renders one action.
func ActionLabel(a DiscussionReturnAction) string {
	switch a {
	case ReturnContinue:
		return "Continue Same Session"
	case ReturnFinish:
		return "Finish"
	case ReturnSwitch:
		return "Switch Agent"
	case ReturnPause:
		return "Pause"
	case ReturnCancel:
		return "Cancel"
	}
	return ""
}

// RenderDiscussionReturn renders the Return Page.
func RenderDiscussionReturn(p DiscussionPage) string {
	var b strings.Builder
	b.WriteString("discussion return\n")
	fmt.Fprintf(&b, "workflow: %s\n", p.Workflow)
	fmt.Fprintf(&b, "session:  %s (%s)\n", p.Session, p.Provider)
	if p.ChangeSet != "" {
		fmt.Fprintf(&b, "change set: %s\n", p.ChangeSet)
	} else {
		b.WriteString("change set: (not frozen yet — Finish freezes it)\n")
	}
	b.WriteString("\n")
	for i, a := range p.Actions {
		mark := " "
		if i == p.Selected {
			mark = ">"
		}
		fmt.Fprintf(&b, " %s %s\n", mark, ActionLabel(a))
	}
	return b.String()
}
