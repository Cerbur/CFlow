package app

// Native requirement discussion tests (TUI task 12, design §9): Prepare
// establishes the exact interactive Session, the Return Page projects
// the bound lineage and the frozen Change Set, and Finish writes the
// immutable, schema-validated ArtifactDiscussionHandoff.

import (
	"context"
	"encoding/json"
	"testing"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
)

// validHandoff is a strict handoff body satisfying discussion-handoff.json.
func validHandoff(wf model.WorkflowID, session model.SessionID, changeSetHash string) []byte {
	body, _ := json.Marshal(map[string]any{
		"workflow_id":         string(wf),
		"session_id":          string(session),
		"targets":             "division by zero must error",
		"constraints":         "no external dependencies",
		"non_goals":           "no other arithmetic changes",
		"acceptance_criteria": "Divide returns a typed error on zero",
		"open_questions":      "error message wording",
		"change_set":          map[string]any{"revision": 1, "sha256": changeSetHash},
		"user_decisions":      []map[string]any{{"topic": "error type", "decision": "typed error"}},
	})
	return body
}

// TestNativeDiscussionPrepareReturnFinish drives the native discussion
// lifecycle: Prepare records the Session, the Return Page projects it,
// and Finish writes the immutable handoff Artifact.
func TestNativeDiscussionPrepareReturnFinish(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app()

	// Prepare: the exact Session is established.
	out, err := a.Execute(context.Background(),
		PrepareNativeDiscussionCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		t.Fatalf("prepare native discussion: %v", err)
	}
	session := out.SessionID
	if session == "" {
		t.Fatal("prepare carried no session id")
	}
	st := fx.status(wf)
	if st.Stage != model.StageRequirementDiscussion {
		t.Fatalf("stage = %s, want REQUIREMENT_DISCUSSION", st.Stage)
	}

	// Return Page: the bound session lineage and the return actions.
	qv, err := a.Query(context.Background(), DiscussionReturnQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("discussion return: %v", err)
	}
	rv := qv.(DiscussionReturnView)
	if rv.Session != session || rv.Provider != "fake" {
		t.Fatalf("return view = %+v", rv)
	}
	if len(rv.Actions) == 0 {
		t.Fatal("the return page offers no actions")
	}

	// Finish: the immutable handoff Artifact is written and validated
	// against the strict schema.
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	handoff := validHandoff(wf, session, hash)
	if _, err := a.Execute(context.Background(),
		FinishDiscussionCommand{Workflow: wf, Session: session, Handoff: handoff}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	store, err := a.artifactStore(wf)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactDiscussionHandoff})
	if err != nil {
		t.Fatalf("resolve handoff: %v", err)
	}
	body, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	// The stored body round-trips the strict contract.
	if err := artifact.ValidateBody("discussion-handoff.json", body); err != nil {
		t.Fatalf("stored handoff fails the schema: %v", err)
	}
}

// TestNativeDiscussionFinishRejectsInvalidHandoff: a handoff missing the
// strict fields is refused with SCHEMA_INVALID and nothing is written.
func TestNativeDiscussionFinishRejectsInvalidHandoff(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("native-discussion", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app()
	out, err := a.Execute(context.Background(),
		PrepareNativeDiscussionCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"workflow_id":"x"}`)
	_, err = a.Execute(context.Background(),
		FinishDiscussionCommand{Workflow: wf, Session: out.SessionID, Handoff: bad})
	if err == nil {
		t.Fatal("an invalid handoff was accepted")
	}
	if code, ok := model.CodeOf(err); !ok || code != model.CodeSchemaInvalid {
		t.Fatalf("fault = %v, want SCHEMA_INVALID", err)
	}
}
