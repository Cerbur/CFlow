package cli

import (
	"testing"

	"cflow.local/cflow/internal/model"
)

func TestRuntimeIDSourceIsUniqueAcrossApplicationInstances(t *testing.T) {
	first := runtimeIDSource()
	second := runtimeIDSource()

	firstID := first(model.IDWorkflow)
	secondID := second(model.IDWorkflow)
	if firstID == "" || secondID == "" {
		t.Fatalf("runtime workflow IDs must be non-empty: %q, %q", firstID, secondID)
	}
	if firstID == secondID {
		t.Fatalf("runtime workflow IDs collide across application instances: %q", firstID)
	}
}
