package app

// Native requirement discussion (design §9, TUI task 12): the TUI runs
// one Provider's native interactive resume of an exact CFlow Session in
// the Workflow's Workspace through the Bridge, then the Return Page
// offers Continue/Finish/Switch/Pause/Cancel. Finish freezes the Change
// Set (if none exists yet) and writes the immutable, schema-validated
// ArtifactDiscussionHandoff — the only discussion input Plan generation
// consumes.

import (
	"context"
	"encoding/json"
	"fmt"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

// prepareNativeDiscussion is the prepare-case of PrepareNativeDiscussionCommand:
// it validates the workflow and the approved route and allocates the
// fresh interactive Session identity.
func (a *Application) prepareNativeDiscussion(ctx context.Context, c PrepareNativeDiscussionCommand) (model.Input, model.WorkflowID, error) {
	wf, err := a.resolveMutationWorkflow(c.Workflow)
	if err != nil {
		return nil, "", err
	}
	if c.Provider == "" || len(c.Provider) > 128 {
		return nil, "", model.InvalidInputFault("a discussion provider is required and bounded")
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	if view.State.Workflow.ID != wf {
		return nil, "", model.InvalidInputFault("the workflow does not exist")
	}
	if view.State.Workflow.Stage != model.StageRequirementDiscussion {
		return nil, "", model.InvalidInputFault("native discussion requires the REQUIREMENT_DISCUSSION stage")
	}
	return model.PrepareNativeDiscussionInput{
		Provider: c.Provider,
		Session:  model.SessionID(a.ids(model.IDSession)),
	}, wf, nil
}

// queryDiscussionReturn projects the native discussion Return Page.
func (a *Application) queryDiscussionReturn(ctx context.Context, q DiscussionReturnQuery) (View, error) {
	wf, err := a.resolveQueryWorkflow(q.Workflow)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if wf == "" {
		return nil, model.InvalidInputFault("no workflow")
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if view.State.Workflow.ID == "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(wf))
	}
	dv := DiscussionReturnView{Workflow: wf}
	for i := len(view.State.Sessions) - 1; i >= 0; i-- {
		s := view.State.Sessions[i]
		if s.Purpose == model.PurposePlanning && s.Provider != "" {
			dv.Session = s.ID
			dv.Provider = s.Provider
			break
		}
	}
	if dv.Session != "" {
		dv.Actions = []string{"continue", "finish", "switch-agent", "pause", "cancel"}
	}
	// The frozen Change Set Ref (the latest revision), if one exists.
	if ref, err := a.latestChangeSetRef(ctx, wf); err == nil && ref.Hash != "" {
		r := ref
		dv.ChangeSet = &r
	}
	return dv, nil
}

// prepareFinish validates the FinishDiscussionCommand input and returns
// the input for the kernel settle (Finish is a session-driven command).
func (a *Application) prepareFinish(ctx context.Context, c FinishDiscussionCommand) (model.Input, model.WorkflowID, error) {
	wf, err := a.resolveMutationWorkflow(c.Workflow)
	if err != nil {
		return nil, "", err
	}
	if !c.Session.Valid() {
		return nil, "", model.InvalidInputFault("finishing a discussion requires the bound session identity")
	}
	if len(c.Handoff) == 0 || len(c.Handoff) > 64*1024 {
		return nil, "", model.InvalidInputFault("the discussion handoff is required and bounded")
	}
	if !json.Valid(c.Handoff) {
		return nil, "", model.NewFault(model.CodeSchemaInvalid, "the discussion handoff is not canonical JSON")
	}
	// The handoff body must satisfy the strict embedded schema.
	if err := artifact.ValidateBody("discussion-handoff.json", c.Handoff); err != nil {
		return nil, "", err
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	if view.State.Workflow.ID != wf {
		return nil, "", model.InvalidInputFault("the workflow does not exist")
	}
	session := findSessionState(view.State, c.Session)
	if session == nil {
		return nil, "", model.InvalidInputFault("the discussion session is not bound to this workflow")
	}
	return model.FinishDiscussionInput{
		Session: c.Session,
		Handoff: append([]byte(nil), c.Handoff...),
	}, wf, nil
}

// findSessionState returns one Session of the aggregate.
func findSessionState(state model.State, id model.SessionID) *model.Session {
	for i := range state.Sessions {
		if state.Sessions[i].ID == id {
			return &state.Sessions[i]
		}
	}
	return nil
}

var _ = fmt.Sprintf
