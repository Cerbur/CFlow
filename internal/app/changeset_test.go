package app

// Freeze Discussion Change Set Application tests (TUI task 5): the
// Runtime-generated immutable ArtifactChangeSet freeze captures the exact
// Git facts of the Workspace at the bound discussion Session turn, and a
// re-freeze after further turns produces the next Revision while every
// earlier Revision stays byte-identical.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"cflow.local/cflow/internal/model"
)

// requireArtifact asserts the outcome surfaced an Artifact reference of
// the expected type and returns it.
func requireArtifact(t *testing.T, out Outcome, typ model.ArtifactType) model.ArtifactRef {
	t.Helper()
	if out.ChangeSet == nil {
		t.Fatalf("outcome carried no ChangeSet view: %+v", out)
	}
	if out.ChangeSet.Ref.Type != typ {
		t.Fatalf("artifact ref = %+v, want type %s", out.ChangeSet.Ref, typ)
	}
	if out.ChangeSet.Ref.Revision < 1 || out.ChangeSet.Ref.Hash == "" {
		t.Fatalf("artifact ref is incomplete: %+v", out.ChangeSet.Ref)
	}
	return out.ChangeSet.Ref
}

// getArtifact reads one immutable Artifact body through the workflow's
// Artifact Store.
func getArtifact(t *testing.T, fx *planningFixture, ref model.ArtifactRef) []byte {
	t.Helper()
	store, err := fx.app().artifactStore(ref.Workflow)
	if err != nil {
		t.Fatal(err)
	}
	body, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	return body
}

// requireJSONFields asserts the body is a JSON object carrying every
// required field.
func requireJSONFields(t *testing.T, body []byte, fields ...string) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("artifact body is not a JSON object: %v", err)
	}
	for _, f := range fields {
		if _, ok := obj[f]; !ok {
			t.Fatalf("artifact body is missing the %q field: %s", f, body)
		}
	}
}

// TestFreezeDiscussionCapturesCompleteChangeSet drives the TUI task 5
// failure test: after a discussion Session turn with candidate Workspace
// content (a committed change and an Untracked file), freezing captures
// the complete immutable Change Set Revision with every required field.
func TestFreezeDiscussionCapturesCompleteChangeSet(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := fx.app(discussionScript("d1", "division by zero must error")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "division by zero must error", Provider: "fake"})
	if err != nil {
		t.Fatalf("discuss: %v", err)
	}
	if out.SessionID == "" {
		t.Fatal("discussion outcome carried no session id")
	}
	session := out.SessionID

	// The candidate Workspace carries one committed change and one
	// Untracked file the freeze must inventory.
	ws := fx.workspacePath(wf)
	feature := filepath.Join(ws, "feature.txt")
	if err := os.WriteFile(feature, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, ws, "add", "feature.txt")
	gitAt(t, ws, "commit", "-q", "-m", "candidate change")
	notes := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(notes, []byte("untracked"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err = fx.app().Execute(context.Background(),
		FreezeDiscussionCommand{Workflow: wf, Session: session})
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	ref := requireArtifact(t, out, model.ArtifactChangeSet)
	body := getArtifact(t, fx, ref)
	requireJSONFields(t, body, "base_commit", "candidate_head", "verified_head",
		"tracked_diff", "untracked", "dirty_fingerprint", "session_id", "content_hash")

	// The body is the Runtime-fixed snapshot of the observed Git facts:
	// the candidate commit is in the range, the changed tracked path and
	// the Untracked file are inventoried, and the Session is bound.
	var cs model.ChangeSet
	if err := json.Unmarshal(body, &cs); err != nil {
		t.Fatalf("change set body does not decode into the model type: %v", err)
	}
	if cs.BaseCommit == "" || cs.CandidateHead == "" || cs.VerifiedHead == "" {
		t.Fatalf("change set heads are incomplete: %+v", cs)
	}
	if len(cs.Commits) != 1 {
		t.Fatalf("change set commits = %+v, want one committed candidate", cs.Commits)
	}
	if !trackedContains(cs.TrackedDiff, "feature.txt") {
		t.Fatalf("tracked diff misses the candidate change: %+v", cs.TrackedDiff)
	}
	if !untrackedContains(cs.Untracked, "notes.txt") {
		t.Fatalf("untracked inventory misses notes.txt: %+v", cs.Untracked)
	}
	if cs.SessionID != string(session) {
		t.Fatalf("change set session = %q, want %q", cs.SessionID, session)
	}
	if cs.DirtyFingerprint == "" || cs.ContentHash == "" {
		t.Fatalf("change set fingerprint/hash missing: %+v", cs)
	}
	if out.ChangeSet.Dirty != true {
		t.Fatalf("change set view reports the candidate workspace as clean: %+v", out.ChangeSet)
	}
}

// TestFreezeDiscussionRevisionBehavior asserts a re-freeze after further
// discussion turns produces revision 2 while revision 1 stays
// byte-identical with an unchanged artifact Hash (TUI task 5 step 4).
func TestFreezeDiscussionRevisionBehavior(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := fx.app(discussionScript("d1", "division by zero must error")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "division by zero must error", Provider: "fake"})
	if err != nil {
		t.Fatalf("discuss: %v", err)
	}
	session := out.SessionID

	out, err = fx.app().Execute(context.Background(),
		FreezeDiscussionCommand{Workflow: wf, Session: session})
	if err != nil {
		t.Fatalf("first freeze: %v", err)
	}
	first := requireArtifact(t, out, model.ArtifactChangeSet)
	if first.Revision != 1 {
		t.Fatalf("first freeze revision = %d, want 1", first.Revision)
	}
	firstBody := getArtifact(t, fx, first)

	// Further discussion turns continue the Session lineage; freezing
	// again must produce revision 2.
	fx.discussSeq++
	cont, err := fx.app(discussionScript("d2", "the error must be typed")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "the error must be typed", Provider: "fake"})
	if err != nil {
		t.Fatalf("continued discussion: %v", err)
	}
	out, err = fx.app().Execute(context.Background(),
		FreezeDiscussionCommand{Workflow: wf, Session: cont.SessionID})
	if err != nil {
		t.Fatalf("second freeze: %v", err)
	}
	second := requireArtifact(t, out, model.ArtifactChangeSet)
	if second.Revision != 2 {
		t.Fatalf("second freeze revision = %d, want 2", second.Revision)
	}

	// Revision 1 is untouched: same Revision, same Hash, byte-identical
	// body.
	store, err := fx.app().artifactStore(wf)
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.Get(context.Background(), first)
	if err != nil {
		t.Fatalf("revision 1 no longer readable: %v", err)
	}
	if string(again) != string(firstBody) {
		t.Fatal("revision 1 body changed between freezes")
	}
}

func trackedContains(entries []model.ChangeSetEntry, path string) bool {
	for _, e := range entries {
		if e.Path == path {
			return true
		}
	}
	return false
}

func untrackedContains(entries []model.ChangeSetEntry, path string) bool {
	return trackedContains(entries, path)
}
