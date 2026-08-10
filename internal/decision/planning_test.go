package decision

import (
	"strings"
	"testing"

	"cflow.local/cflow/internal/agent"
)

func TestValidatePlanMarkdownDistinguishesEmptyAndOversized(t *testing.T) {
	if _, err := validatePlanMarkdown(nil); err == nil || err.Error() != "plan output is empty" {
		t.Fatalf("empty validation error = %v", err)
	}
	oversized := []byte(strings.Repeat("x", maxPlanBody+1))
	if _, err := validatePlanMarkdown(oversized); err == nil || err.Error() != "plan output exceeds the bounded size: 1048577 bytes > 1048576" {
		t.Fatalf("oversized validation error = %v", err)
	}
}

func TestPlanGenerationPromptMatchesValidatorSections(t *testing.T) {
	registry, err := agent.LoadPromptRegistry()
	if err != nil {
		t.Fatalf("load prompt registry: %v", err)
	}
	prompt, ok := registry.Lookup(string(agent.PurposePlanGeneration))
	if !ok {
		t.Fatal("PLAN_GENERATION prompt not found")
	}

	last := -1
	for _, section := range planRequiredSections {
		marker := "- `## " + section + "`"
		index := strings.Index(prompt.Body, marker)
		if index < 0 {
			t.Fatalf("plan prompt does not require validator section %q", section)
		}
		if index <= last {
			t.Fatalf("plan prompt lists validator sections out of order at %q", section)
		}
		last = index
	}
}
