package tui

import (
	"testing"

	"cflow.local/cflow/internal/model"
)

func TestHandoffDecisionsAllowsEmptyGuidance(t *testing.T) {
	ref := &model.ArtifactRef{
		Workflow: "wf-1",
		Revision: 2,
		Hash:     "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}

	got, err := handoffDecisions(" \n\t", "wf-1", "session-1", ref)
	if err != nil {
		t.Fatalf("empty handoff guidance: %v", err)
	}
	if string(got) != `{}` {
		t.Fatalf("empty handoff guidance = %q, want %q", got, `{}`)
	}
}
