// Package agent hosts the Agent Runtime seam (design 14): the immutable
// Provider Registry (14.2) and Prompt Registry (14.5). Adapters arrive in
// a later task; the registries are the embedded, versioned policy
// resources that Sessions, routes, and evidence bind to.
//
// Every entry carries a revision and a content hash, and every registry
// load derives a deterministic registry revision, so registry changes can
// never mutate the meaning of an existing Session: a Session record pins
// the exact Prompt reference and Provider binding it was created with.
// Loading fails closed on unparseable, unknown, or duplicated content.
package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/prompts"
)

// AgentPurpose is the constrained role assigned to a Session (PRD Agent
// 角色; CONTEXT.md: Agent Purpose). Exactly one Session is used per
// purpose and role lineage.
type AgentPurpose string

const (
	PurposeRequirementDiscussion AgentPurpose = "REQUIREMENT_DISCUSSION"
	PurposePlanGeneration        AgentPurpose = "PLAN_GENERATION"
	PurposePlanCheck             AgentPurpose = "PLAN_CHECK"
	PurposeSpecGeneration        AgentPurpose = "SPEC_GENERATION"
	PurposeWorkflowOptimization  AgentPurpose = "WORKFLOW_OPTIMIZATION"
	PurposeTaskImplementation    AgentPurpose = "TASK_IMPLEMENTATION"
	PurposeTaskReview            AgentPurpose = "TASK_REVIEW"
	PurposeTaskRepair            AgentPurpose = "TASK_REPAIR"
	PurposeFinalVerification     AgentPurpose = "FINAL_VERIFICATION"
)

// Valid reports whether p is a declared Agent Purpose.
func (p AgentPurpose) Valid() bool {
	switch p {
	case PurposeRequirementDiscussion, PurposePlanGeneration, PurposePlanCheck,
		PurposeSpecGeneration, PurposeWorkflowOptimization, PurposeTaskImplementation,
		PurposeTaskReview, PurposeTaskRepair, PurposeFinalVerification:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Prompt Registry (design 14.5, PRD Prompt 版本化)
// ---------------------------------------------------------------------------

// Prompt is one embedded, versioned prompt template. It is addressed by
// Agent Purpose plus registry revision and content hash; prompts may
// request structured output but never grant routes, permissions,
// executable commands, budgets, approvals, or lifecycle state.
type Prompt struct {
	File         string `json:"file"`
	Purpose      string `json:"purpose"`
	Revision     string `json:"revision"`
	InputSchema  string `json:"input_schema"`
	OutputSchema string `json:"output_schema"`
	Body         string `json:"body"`
	Hash         string `json:"hash,omitempty"`
}

// PromptRegistry is an immutable registry of embedded prompts. It exposes
// no mutators; Lookup returns values, never internal state.
type PromptRegistry struct {
	revision  string
	byPurpose map[string]Prompt
}

// LoadPromptRegistry parses every embedded prompt file into an immutable
// registry. Any unparseable header, unknown purpose, or duplicate purpose
// fails the whole load.
func LoadPromptRegistry() (*PromptRegistry, error) {
	entries, err := prompts.FS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("prompt registry: %w", err)
	}
	reg := &PromptRegistry{byPurpose: map[string]Prompt{}}
	for _, e := range entries {
		if e.IsDir() || !hasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := prompts.FS.ReadFile(e.Name())
		if err != nil {
			return nil, fmt.Errorf("prompt registry: read %s: %w", e.Name(), err)
		}
		p, err := parsePromptFile(e.Name(), data)
		if err != nil {
			return nil, err
		}
		if _, dup := reg.byPurpose[p.Purpose]; dup {
			return nil, fmt.Errorf("prompt registry: duplicate purpose %q", p.Purpose)
		}
		reg.byPurpose[p.Purpose] = p
	}
	if len(reg.byPurpose) == 0 {
		return nil, fmt.Errorf("prompt registry: no prompts embedded")
	}
	reg.revision = promptRegistryHash(reg)
	return reg, nil
}

// Lookup returns the prompt bound to one Agent Purpose.
func (r *PromptRegistry) Lookup(purpose string) (Prompt, bool) {
	p, ok := r.byPurpose[purpose]
	return p, ok
}

// Revision returns the deterministic registry revision: the digest of the
// canonical serialization of every entry, so any prompt update creates a
// new registry revision while historical Session records retain the
// original reference.
func (r *PromptRegistry) Revision() string { return r.revision }

// promptHeader is the machine-parsed header every prompt file begins
// with. Unknown header fields fail the load.
type promptHeader struct {
	Purpose      string `yaml:"purpose"`
	Revision     string `yaml:"revision"`
	InputSchema  string `yaml:"input_schema"`
	OutputSchema string `yaml:"output_schema"`
}

// parsePromptFile parses one prompt file: a --- delimited YAML header
// binding purpose, revision, input_schema, and output_schema, followed by
// the Markdown body.
func parsePromptFile(name string, data []byte) (Prompt, error) {
	front, body, err := splitPromptHeader(data)
	if err != nil {
		return Prompt{}, fmt.Errorf("prompt %s: %w", name, err)
	}
	var h promptHeader
	dec := yaml.NewDecoder(bytes.NewReader(front))
	dec.KnownFields(true)
	if err := dec.Decode(&h); err != nil {
		return Prompt{}, fmt.Errorf("prompt %s: header: %w", name, err)
	}
	if h.Purpose == "" || h.Revision == "" || h.InputSchema == "" || h.OutputSchema == "" {
		return Prompt{}, fmt.Errorf("prompt %s: header must bind purpose, revision, input_schema and output_schema", name)
	}
	if !AgentPurpose(h.Purpose).Valid() {
		return Prompt{}, fmt.Errorf("prompt %s: purpose %q is not a declared Agent Purpose", name, h.Purpose)
	}
	p := Prompt{
		File:         name,
		Purpose:      h.Purpose,
		Revision:     h.Revision,
		InputSchema:  h.InputSchema,
		OutputSchema: h.OutputSchema,
		Body:         strings.TrimLeft(string(body), "\n"),
	}
	p.Hash = p.entryHash()
	return p, nil
}

// splitPromptHeader splits a prompt file at its closing front matter
// marker. The header is required.
func splitPromptHeader(data []byte) (front, body []byte, err error) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil, nil, fmt.Errorf("missing front matter header")
	}
	idx := bytes.Index(data[4:], []byte("\n---\n"))
	if idx < 0 {
		return nil, nil, fmt.Errorf("front matter header is not terminated")
	}
	end := 4 + idx
	return data[4:end], data[end+5:], nil
}

// entryHash is the content hash of one prompt: the digest of its canonical
// serialization with the hash field excluded from its own digest.
func (p Prompt) entryHash() string {
	h := p
	h.Hash = ""
	return sha256Hex(jsonMarshal(h))
}

// promptRegistryHash digests the canonical serialization of every entry,
// ordered by purpose.
func promptRegistryHash(reg *PromptRegistry) string {
	purposes := make([]string, 0, len(reg.byPurpose))
	for p := range reg.byPurpose {
		purposes = append(purposes, p)
	}
	sort.Strings(purposes)
	entries := make([]Prompt, 0, len(purposes))
	for _, p := range purposes {
		entries = append(entries, reg.byPurpose[p])
	}
	return sha256Hex(jsonMarshal(entries))
}

// jsonMarshal canonicalizes one registry value for digesting. The
// registered structs always marshal; an error here is a build bug.
func jsonMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("agent: registry value cannot be serialized: %v", err))
	}
	return data
}

// sha256Hex digests canonical serialized bytes.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
