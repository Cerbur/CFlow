package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
)

// TestCodingSessionInputOwnSpec: the coding Session of a multi-Spec
// workflow receives ONLY its own Spec (the real Cross-Provider E2E
// regression: a claude Task once received null/whole-set input and
// implemented a sibling Spec).
func TestCodingSessionInputOwnSpec(t *testing.T) {
	a, _ := fixtureApplication(t)
	ctx := context.Background()
	wf := model.WorkflowID("workflow-1")
	store, err := a.artifactStore(wf)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	specBody := []byte(`[{"id":"s01","goal":"implement multiply","depends_on":[],"write_scope":["src/multiply.ts"],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"codex","model":"default","budget":10}},{"id":"s02","goal":"implement divide with a clear exception on zero divisor","depends_on":[],"write_scope":["src/divide.ts","test/divide.test.ts"],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"claude","model":"default","budget":10}}]`)
	if _, err := store.Put(ctx, artifact.PutRequest{
		WorkflowID: wf, Type: model.ArtifactSpec, Revision: 1, SchemaVersion: "1.0.0",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Producer:  artifact.ProducerRef{Purpose: "spec-generation"},
		Body:      specBody,
	}); err != nil {
		t.Fatalf("put spec artifact: %v", err)
	}
	in, err := a.sessionInput(ctx, wf, model.DispatchInput{Node: "task-s02"})
	if err != nil {
		t.Fatalf("session input: %v", err)
	}
	ci, ok := in.(*codingSessionInput)
	if !ok {
		t.Fatalf("session input is %T, want *codingSessionInput (value %+v)", in, in)
	}
	if !strings.Contains(ci.Spec, "divide") || strings.Contains(ci.Spec, "multiply") {
		t.Fatalf("coding session must receive only its own spec, got: %q", ci.Spec)
	}
}
