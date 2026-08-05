package app

import (
	"strings"
	"testing"
)

func TestRenderPromptFillsInputBlock(t *testing.T) {
	body := "Role text.\n<CFLOW_INPUT>\n</CFLOW_INPUT>\nConstraints."
	got := renderPrompt(body, map[string]string{"spec": "id: s02"})
	if !strings.Contains(got, `"spec":"id: s02"`) && !strings.Contains(got, `"spec": "id: s02"`) {
		t.Fatalf("rendered prompt must carry the spec input: %q", got)
	}
	if !strings.Contains(got, "<CFLOW_INPUT>") || !strings.Contains(got, "</CFLOW_INPUT>") {
		t.Fatalf("rendered prompt must keep the input block markers: %q", got)
	}
	if strings.Contains(got, "<CFLOW_INPUT>\n</CFLOW_INPUT>") {
		t.Fatalf("rendered prompt must not keep the empty input block: %q", got)
	}
}

func TestRenderPromptNoBlockUnchanged(t *testing.T) {
	body := "no input block here"
	if got := renderPrompt(body, map[string]string{"spec": "x"}); got != body {
		t.Fatalf("prompt without the block must be unchanged: %q", got)
	}
}
