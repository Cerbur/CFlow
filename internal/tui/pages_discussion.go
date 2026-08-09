package tui

// The native discussion Return Page (design §9.2, TUI task 12): after
// one interactive turn the page offers Start/Continue Same Session,
// Finish, Switch Agent, Pause, and Cancel. Navigation only updates the
// UI selection; Finish/Continue are explicit commands. Finish freezes
// the Change Set (the Runtime facts) and opens the handoff editor where
// the user supplies only the content fields; the merged strict handoff
// body is validated by the Application against discussion-handoff.json.

import (
	"encoding/json"
	"fmt"
	"strings"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// DiscussionReturnAction is one legal action of the Return Page.
type DiscussionReturnAction int

const (
	ReturnStart DiscussionReturnAction = iota
	ReturnContinue
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
	// ChangeSetRef is the frozen Change Set Revision/Hash the handoff
	// merge binds (nil until the first freeze).
	ChangeSetRef *model.ArtifactRef
	Actions      []DiscussionReturnAction
	// Selected is the highlighted action index.
	Selected int
	// Editing is true while the handoff content editor is open.
	Editing bool
	// Handoff is the user-typed handoff content JSON.
	Handoff string
	// Status is the transient editor error/status line.
	Status string
	// Loaded is true once the Return projection arrived: before that the
	// page shows a loading placeholder and no action may be activated
	// (the projection and the action list arrive together).
	Loaded bool
}

// MapDiscussionReturn maps the app's Return projection to the page. A
// session-less workflow offers the Start action; a bound session offers
// the full return actions.
func MapDiscussionReturn(v app.DiscussionReturnView) DiscussionPage {
	p := DiscussionPage{
		Workflow: string(v.Workflow),
		Session:  string(v.Session),
		Provider: v.Provider,
		Loaded:   true,
	}
	if v.ChangeSet != nil {
		p.ChangeSet = fmt.Sprintf("rev %d (%s)", v.ChangeSet.Revision, shortHead(v.ChangeSet.Hash))
		ref := *v.ChangeSet
		p.ChangeSetRef = &ref
	}
	if len(v.Actions) == 0 {
		p.Actions = []DiscussionReturnAction{ReturnStart}
		return p
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
	if len(p.Actions) == 0 {
		p.Actions = []DiscussionReturnAction{ReturnStart}
	}
	return p
}

// ActionLabel renders one action.
func ActionLabel(a DiscussionReturnAction) string {
	switch a {
	case ReturnStart:
		return "Start Native Discussion"
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
	if !p.Loaded {
		b.WriteString("discussion\n(loading the native discussion session…)\n")
		return b.String()
	}
	b.WriteString("discussion return\n")
	fmt.Fprintf(&b, "workflow: %s\n", p.Workflow)
	if p.Session != "" {
		fmt.Fprintf(&b, "session:  %s (%s)\n", p.Session, p.Provider)
	}
	if p.ChangeSet != "" {
		fmt.Fprintf(&b, "change set: %s\n", p.ChangeSet)
	} else {
		b.WriteString("change set: (not frozen yet — Finish freezes it)\n")
	}
	b.WriteString("\n")
	if p.Editing {
		renderHandoffEditor(&b, p)
		return b.String()
	}
	for i, a := range p.Actions {
		mark := " "
		if i == p.Selected {
			mark = ">"
		}
		fmt.Fprintf(&b, " %s %s\n", mark, ActionLabel(a))
	}
	return b.String()
}

// renderHandoffEditor renders the strict handoff content editor: the
// user types ONLY the content fields; the Runtime facts (workflow id,
// session id, frozen Change Set) are merged by CFlow and re-validated
// by the Application.
func renderHandoffEditor(b *strings.Builder, p DiscussionPage) {
	b.WriteString("handoff content (JSON):\n")
	b.WriteString("  targets, constraints, non_goals, acceptance_criteria,\n")
	b.WriteString("  open_questions, user_decisions\n")
	if p.ChangeSet != "" {
		fmt.Fprintf(b, "  change set: %s (merged by CFlow)\n", p.ChangeSet)
	}
	fmt.Fprintf(b, "\n> %s_\n", p.Handoff)
	if p.Status != "" {
		fmt.Fprintf(b, "\n%s\n", p.Status)
	}
	b.WriteString("\nEnter to finish the discussion, Esc to cancel\n")
}

// buildHandoff merges the user's handoff content fields with the
// authoritative Runtime facts (the workflow, the bound Session, and the
// frozen Change Set Revision/Hash) into the strict handoff body the
// Application validates against discussion-handoff.json. User-supplied
// values for the Runtime-fact keys are refused: the facts are never
// invented by the interface.
func buildHandoff(content string, wf model.WorkflowID, session model.SessionID, ref *model.ArtifactRef) ([]byte, error) {
	var user map[string]any
	if err := json.Unmarshal([]byte(content), &user); err != nil {
		return nil, fmt.Errorf("the handoff content is not valid JSON: %v", err)
	}
	if user == nil {
		return nil, fmt.Errorf("the handoff content must be a JSON object")
	}
	for _, k := range []string{"workflow_id", "session_id", "change_set"} {
		if _, ok := user[k]; ok {
			return nil, fmt.Errorf("the handoff field %q is a CFlow runtime fact and cannot be typed", k)
		}
	}
	if ref == nil || ref.Revision < 1 || len(ref.Hash) < 64 {
		return nil, fmt.Errorf("no frozen change set exists; finish again after the freeze")
	}
	body, err := json.Marshal(map[string]any{
		"workflow_id":         string(wf),
		"session_id":          string(session),
		"targets":             user["targets"],
		"constraints":         user["constraints"],
		"non_goals":           user["non_goals"],
		"acceptance_criteria": user["acceptance_criteria"],
		"open_questions":      user["open_questions"],
		"change_set":          map[string]any{"revision": ref.Revision, "sha256": ref.Hash},
		"user_decisions":      user["user_decisions"],
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}
