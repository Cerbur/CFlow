package agent

import (
	"sort"
	"strings"
	"testing"

	"cflow.local/cflow/protocols"
)

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// promptSnapshots collects the registry revision and every entry hash for
// one load, deterministically ordered.
func promptSnapshots(reg *PromptRegistry) []string {
	snap := []string{reg.Revision()}
	purposes := make([]string, 0, len(reg.byPurpose))
	for p := range reg.byPurpose {
		purposes = append(purposes, p)
	}
	sort.Strings(purposes)
	for _, p := range purposes {
		snap = append(snap, reg.byPurpose[p].Hash)
	}
	return snap
}

func providerSnapshots(reg *ProviderRegistry) []string {
	snap := []string{reg.Revision()}
	names := make([]string, 0, len(reg.byName))
	for n := range reg.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		snap = append(snap, reg.byName[n].Hash)
	}
	return snap
}

func equalSnapshots(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRegistryHashesByteIdenticalAcrossRuns: ten loads of both registries
// produce byte-identical registry revisions and per-entry hashes (brief
// Step 5): registry changes can never mutate the meaning of an existing
// Session, because every reference pins revision plus content hash.
func TestRegistryHashesByteIdenticalAcrossRuns(t *testing.T) {
	var wantP, wantPr []string
	for i := 0; i < 10; i++ {
		p, err := LoadPromptRegistry()
		requireNoError(t, err)
		pr, err := LoadProviderRegistry()
		requireNoError(t, err)
		if p.Revision() == "" || pr.Revision() == "" {
			t.Fatal("registry revisions must not be empty")
		}
		if i == 0 {
			wantP, wantPr = promptSnapshots(p), providerSnapshots(pr)
			continue
		}
		if !equalSnapshots(promptSnapshots(p), wantP) {
			t.Fatalf("prompt registry hash changed on run %d", i)
		}
		if !equalSnapshots(providerSnapshots(pr), wantPr) {
			t.Fatalf("provider registry hash changed on run %d", i)
		}
	}
}

// TestPromptHashRetention: every prompt entry retains a non-empty content
// hash that is stable across loads and pins the exact prompt content.
func TestPromptHashRetention(t *testing.T) {
	p1, err := LoadPromptRegistry()
	requireNoError(t, err)
	p2, err := LoadPromptRegistry()
	requireNoError(t, err)

	pg, ok := p1.Lookup("PLAN_GENERATION")
	if !ok {
		t.Fatal("PLAN_GENERATION prompt not found")
	}
	if pg.Hash == "" || len(pg.Hash) != 64 {
		t.Fatalf("prompt hash is not a 64-character digest: %q", pg.Hash)
	}
	again, ok := p2.Lookup("PLAN_GENERATION")
	if !ok || again.Hash != pg.Hash {
		t.Fatalf("prompt hash not retained across loads: %q vs %q", pg.Hash, again.Hash)
	}

	// The hash pins content: changing the body changes the hash.
	modified := pg
	modified.Body += "\n"
	if modified.entryHash() == pg.Hash {
		t.Fatal("prompt hash does not pin the body content")
	}

	if _, ok := p1.Lookup("NO_SUCH_PURPOSE"); ok {
		t.Fatal("unknown purpose must not resolve")
	}
}

// TestPromptRegistryHeaderContract: every embedded prompt begins with a
// machine-parsed header binding purpose, revision, input_schema and
// output_schema, and the purpose set matches the PRD Agent roles.
func TestPromptRegistryHeaderContract(t *testing.T) {
	reg, err := LoadPromptRegistry()
	requireNoError(t, err)

	if len(reg.byPurpose) != 9 {
		t.Fatalf("expected 9 embedded prompts, got %d", len(reg.byPurpose))
	}
	for purpose, p := range reg.byPurpose {
		if !strings.HasSuffix(p.File, ".md") || p.File == "" {
			t.Fatalf("prompt %s has no file name", purpose)
		}
		if p.Purpose != purpose {
			t.Fatalf("prompt %s registered under %s", p.Purpose, purpose)
		}
		if p.Revision == "" || p.InputSchema == "" || p.OutputSchema == "" {
			t.Fatalf("prompt %s has an incomplete header (revision=%q input=%q output=%q)",
				p.File, p.Revision, p.InputSchema, p.OutputSchema)
		}
		if !strings.HasPrefix(p.Body, "#") {
			t.Fatalf("prompt %s has no markdown body", p.File)
		}
		if !AgentPurpose(purpose).Valid() {
			t.Fatalf("purpose %s is not a declared Agent Purpose", purpose)
		}
	}

	wantPurposes := map[string]bool{
		"REQUIREMENT_DISCUSSION": true,
		"PLAN_GENERATION":        true,
		"PLAN_CHECK":             true,
		"SPEC_GENERATION":        true,
		"WORKFLOW_OPTIMIZATION":  true,
		"TASK_IMPLEMENTATION":    true,
		"TASK_REVIEW":            true,
		"TASK_REPAIR":            true,
		"FINAL_VERIFICATION":     true,
	}
	for p := range reg.byPurpose {
		if !wantPurposes[p] {
			t.Fatalf("unexpected prompt purpose %s", p)
		}
	}
}

// TestProviderRegistryUnknownDialectFailClosed: a provider binding whose
// dialect is not known to this binary, or whose dialect id does not match
// the dialect form, fails the whole registry load.
func TestProviderRegistryUnknownDialectFailClosed(t *testing.T) {
	base := `providers:
  - name: mystery
    status: enabled
    revision: "1.0.0"
    executable: {name: "mystery", path_policy: "x"}
    version_range: ">=1.0.0"
    binary_identity_policy: "x"
    dialect: {id: "cflow.dialect.mystery.v1", event_schema_revision: "1"}
    session_contract: {start_event: "session_started", id_field: "session_id", terminal_events: ["session_finished"], conflict_rule: "x"}
    start_capabilities: [jsonl_events]
    resume_capabilities: [jsonl_events]
    cancel_behavior: "x"
    budget_behavior: "x"
    known_incompatibilities: []
`
	if _, err := parseProviders([]byte(base)); err == nil {
		t.Fatal("unknown dialect must fail the registry load")
	}

	badForm := strings.Replace(base, "cflow.dialect.mystery.v1", "mystery-dialect", 1)
	if _, err := parseProviders([]byte(badForm)); err == nil {
		t.Fatal("dialect id outside the dialect form must fail the registry load")
	}

	unknownField := base + "  - name: extra\n    status: enabled\n    unknown_key: true\n"
	if _, err := parseProviders([]byte(unknownField)); err == nil {
		t.Fatal("unknown YAML fields must fail the registry load")
	}
}

// TestProviderRegistryFailClosedSelection: unknown providers cannot be
// selected, and a disabled P1 provider (OpenCode) is visible as metadata
// but can never be selected.
func TestProviderRegistryFailClosedSelection(t *testing.T) {
	reg, err := LoadProviderRegistry()
	requireNoError(t, err)

	if _, err := reg.Select("no-such-provider"); err == nil {
		t.Fatal("unknown provider must not be selectable")
	}
	if _, err := reg.Select("opencode"); err == nil {
		t.Fatal("disabled P1 provider must not be selectable")
	}
	oc, ok := reg.Lookup("opencode")
	if !ok || oc.Status != ProviderDisabledP1 {
		t.Fatalf("opencode must be listed as disabled P1 metadata, got %+v", oc)
	}
	for _, name := range []string{"fake", "codex", "claude"} {
		b, err := reg.Select(name)
		requireNoError(t, err)
		if b.Name != name || b.Status != ProviderEnabled {
			t.Fatalf("unexpected binding for %s: %+v", name, b)
		}
	}
}

// TestProviderRegistryReorderedYAMLMaps: reordering YAML map keys inside
// the bindings file yields the same registry revision and entry hashes,
// so the registry canonical serialization is order-insensitive.
func TestProviderRegistryReorderedYAMLMaps(t *testing.T) {
	original, err := LoadProviderRegistry()
	requireNoError(t, err)

	data, err := protocols.FS.ReadFile("providers.yaml")
	requireNoError(t, err)
	text := string(data)

	// Reorder the fake provider's executable map keys.
	execReordered := strings.Replace(text,
		`    executable:
      name: "cflow-fake-agent"
      path_policy: "in-process deterministic test adapter; never launched from PATH"`,
		`    executable:
      path_policy: "in-process deterministic test adapter; never launched from PATH"
      name: "cflow-fake-agent"`, 1)
	if execReordered == text {
		t.Fatal("test fixture: fake executable block not found for reordering")
	}
	// Reorder the codex session contract map keys.
	contractReordered := strings.Replace(execReordered,
		`    session_contract:
      start_event: "session_started"
      id_field: "session_id"
      terminal_events: ["session_finished", "session_failed"]`,
		`    session_contract:
      terminal_events: ["session_finished", "session_failed"]
      id_field: "session_id"
      start_event: "session_started"`, 1)
	if contractReordered == execReordered {
		t.Fatal("test fixture: codex session contract block not found for reordering")
	}

	parsed, err := parseProviders([]byte(contractReordered))
	requireNoError(t, err)
	if parsed.Revision() != original.Revision() {
		t.Fatalf("reordered YAML maps changed the registry revision: %s vs %s", parsed.Revision(), original.Revision())
	}
	fake, _ := parsed.Lookup("fake")
	origFake, _ := original.Lookup("fake")
	if fake.Hash != origFake.Hash || fake.Hash == "" {
		t.Fatalf("reordered YAML maps changed the entry hash: %s vs %s", fake.Hash, origFake.Hash)
	}
}

// TestProviderRegistryCodexBindingCapturedFacts (task 14 fixture
// capture): the codex binding pins the captured 0.146.0 protocol facts —
// the executable name, the supported version range (covering the captured
// baseline and excluding 2.x), the JSONL dialect, the session contract,
// and the Start/Resume capability gates the Runtime applies.
func TestProviderRegistryCodexBindingCapturedFacts(t *testing.T) {
	reg, err := LoadProviderRegistry()
	requireNoError(t, err)
	b, ok := reg.Lookup("codex")
	if !ok {
		t.Fatal("codex binding missing from the registry")
	}
	if b.Executable.Name != "codex" {
		t.Fatalf("codex executable name = %q, want %q", b.Executable.Name, "codex")
	}
	if b.VersionRange != ">=0.80.0 <2.0.0" {
		t.Fatalf("codex version range = %q, want the captured binding %q", b.VersionRange, ">=0.80.0 <2.0.0")
	}
	if !versionInRange("0.146.0", b.VersionRange) {
		t.Fatal("the captured baseline 0.146.0 must be inside the codex supported range")
	}
	if versionInRange("2.0.0", b.VersionRange) {
		t.Fatal("2.0.0 must be outside the codex supported range")
	}
	if b.Dialect.ID != "cflow.dialect.codex-jsonl.v1" || b.Dialect.EventSchemaRevision != "2" {
		t.Fatalf("codex dialect = %+v", b.Dialect)
	}
	if b.SessionContract.StartEvent != "session_started" || b.SessionContract.IDField != "thread_id" {
		t.Fatalf("codex session contract = %+v", b.SessionContract)
	}
	// The Start and Resume capability gates the Runtime applies to every
	// route must pass for the captured binding.
	if !bindingHas(b, requiredStartCapabilities) {
		t.Fatal("codex binding must pass the required Start capability gate")
	}
	if !bindingHasResume(b, requiredResumeCapabilities) {
		t.Fatal("codex binding must pass the required Resume capability gate")
	}
}

// TestProviderRegistryClaudeBindingCapturedFacts (task 15 fixture
// capture): the claude binding pins the captured 2.1.220 protocol facts —
// the executable name, the supported version range (covering the captured
// baseline and excluding 3.x), the stream-json dialect, the session
// contract, and the Start/Resume capability gates the Runtime applies.
func TestProviderRegistryClaudeBindingCapturedFacts(t *testing.T) {
	reg, err := LoadProviderRegistry()
	requireNoError(t, err)
	b, ok := reg.Lookup("claude")
	if !ok {
		t.Fatal("claude binding missing from the registry")
	}
	if b.Executable.Name != "claude" {
		t.Fatalf("claude executable name = %q, want %q", b.Executable.Name, "claude")
	}
	if b.VersionRange != ">=1.0.0 <3.0.0" {
		t.Fatalf("claude version range = %q, want the captured binding %q", b.VersionRange, ">=1.0.0 <3.0.0")
	}
	if !versionInRange("2.1.220", b.VersionRange) {
		t.Fatal("the captured baseline 2.1.220 must be inside the claude supported range")
	}
	if versionInRange("3.0.0", b.VersionRange) {
		t.Fatal("3.0.0 must be outside the claude supported range")
	}
	if b.Dialect.ID != "cflow.dialect.claude-stream-json.v1" || b.Dialect.EventSchemaRevision != "1" {
		t.Fatalf("claude dialect = %+v", b.Dialect)
	}
	if b.SessionContract.StartEvent != "session_started" || b.SessionContract.IDField != "session_id" {
		t.Fatalf("claude session contract = %+v", b.SessionContract)
	}
	// The Start and Resume capability gates the Runtime applies to every
	// route must pass for the captured binding.
	if !bindingHas(b, requiredStartCapabilities) {
		t.Fatal("claude binding must pass the required Start capability gate")
	}
	if !bindingHasResume(b, requiredResumeCapabilities) {
		t.Fatal("claude binding must pass the required Resume capability gate")
	}
}

// TestProviderRegistryClaudeUnknownDialectFailClosed: a claude provider
// binding whose dialect is unknown to this binary, or whose version range
// is missing, fails the whole registry load (PRD 已确认：未知 Provider CLI
// 协议 Fail-closed).
func TestProviderRegistryClaudeUnknownDialectFailClosed(t *testing.T) {
	base := `providers:
  - name: claude
    status: enabled
    revision: "1.0.0"
    executable: {name: "claude", path_policy: "PATH-resolved at Execution Approval"}
    version_range: ">=1.0.0 <3.0.0"
    binary_identity_policy: "PATH-resolved absolute path pinned with binary sha256 at Execution Approval"
    dialect: {id: "cflow.dialect.claude-stream-json.v2", event_schema_revision: "1"}
    session_contract: {start_event: "session_started", id_field: "session_id", terminal_events: ["session_finished", "session_failed"], conflict_rule: "x"}
    start_capabilities: [stream_json, structured_output, session_id_on_start]
    resume_capabilities: [stream_json, structured_output, resume_by_session_id]
    cancel_behavior: "SIGTERM to the process group; controlled stop drains the stream before termination"
    budget_behavior: "native budget limit supported"
    known_incompatibilities: []
`
	if _, err := parseProviders([]byte(base)); err == nil {
		t.Fatal("a claude binding with an unknown dialect revision must fail the registry load")
	}
	missingRange := strings.Replace(base, `version_range: ">=1.0.0 <3.0.0"`, `version_range: ""`, 1)
	missingRange = strings.Replace(missingRange, "cflow.dialect.claude-stream-json.v2", "cflow.dialect.claude-stream-json.v1", 1)
	if _, err := parseProviders([]byte(missingRange)); err == nil {
		t.Fatal("a claude binding without a supported version range must fail the registry load")
	}
}

// TestProviderRegistryCodexUnknownDialectFailClosed: a codex provider
// binding whose dialect is unknown to this binary, or whose version range
// is missing, fails the whole registry load (PRD 已确认：未知 Provider CLI
// 协议 Fail-closed).
func TestProviderRegistryCodexUnknownDialectFailClosed(t *testing.T) {
	base := `providers:
  - name: codex
    status: enabled
    revision: "1.0.0"
    executable: {name: "codex", path_policy: "PATH-resolved at Execution Approval"}
    version_range: ">=0.80.0 <2.0.0"
    binary_identity_policy: "PATH-resolved absolute path pinned with binary sha256 at Execution Approval"
    dialect: {id: "cflow.dialect.codex-jsonl.v2", event_schema_revision: "1"}
    session_contract: {start_event: "session_started", id_field: "session_id", terminal_events: ["session_finished", "session_failed"], conflict_rule: "x"}
    start_capabilities: [jsonl_events, structured_output, session_id_on_start]
    resume_capabilities: [jsonl_events, structured_output, resume_by_session_id]
    cancel_behavior: "SIGTERM to the process group"
    budget_behavior: "no native budget limit"
    known_incompatibilities: []
`
	if _, err := parseProviders([]byte(base)); err == nil {
		t.Fatal("a codex binding with an unknown dialect revision must fail the registry load")
	}
	missingRange := strings.Replace(base, `version_range: ">=0.80.0 <2.0.0"`, `version_range: ""`, 1)
	missingRange = strings.Replace(missingRange, "cflow.dialect.codex-jsonl.v2", "cflow.dialect.codex-jsonl.v1", 1)
	if _, err := parseProviders([]byte(missingRange)); err == nil {
		t.Fatal("a codex binding without a supported version range must fail the registry load")
	}
}

// versionInRange is the test-local check that the captured baseline
// version satisfies the codex binding's declared range (the adapter owns
// the production range matcher; this pins the binding contract).
func versionInRange(version, constraint string) bool {
	parse := func(s string) ([3]int, bool) {
		parts := strings.SplitN(s, ".", 3)
		if len(parts) != 3 {
			return [3]int{}, false
		}
		var v [3]int
		for i, p := range parts {
			n := 0
			for _, c := range p {
				if c < '0' || c > '9' {
					return [3]int{}, false
				}
				n = n*10 + int(c-'0')
			}
			v[i] = n
		}
		return v, true
	}
	want, ok := parse(version)
	if !ok {
		return false
	}
	for _, tok := range strings.Fields(constraint) {
		op, boundText := "", tok
		for _, candidate := range []string{">=", "<=", ">", "<"} {
			if strings.HasPrefix(tok, candidate) {
				op, boundText = candidate, strings.TrimPrefix(tok, candidate)
				break
			}
		}
		bound, ok := parse(boundText)
		if !ok {
			return false
		}
		switch op {
		case ">=":
			if lt(want, bound) {
				return false
			}
		case "<=":
			if gt(want, bound) {
				return false
			}
		case ">":
			if !gt(want, bound) {
				return false
			}
		case "<":
			if !lt(want, bound) {
				return false
			}
		case "":
			if want != bound {
				return false
			}
		}
	}
	return true
}

func lt(a, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func gt(a, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// TestProviderRegistryBindingsComplete: every enabled binding carries the
// full design 14.2 binding set, and disabled OpenCode carries P1 metadata
// only.
func TestProviderRegistryBindingsComplete(t *testing.T) {
	reg, err := LoadProviderRegistry()
	requireNoError(t, err)
	for _, name := range []string{"fake", "codex", "claude"} {
		b, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("provider %s missing", name)
		}
		if b.Name == "" || b.Revision == "" || b.Hash == "" || b.VersionRange == "" ||
			b.BinaryIdentity == "" || b.CancelBehavior == "" || b.BudgetBehavior == "" {
			t.Fatalf("provider %s has an incomplete binding: %+v", name, b)
		}
		if b.Executable.Name == "" || b.Executable.PathPolicy == "" {
			t.Fatalf("provider %s has an incomplete executable policy", name)
		}
		if !strings.HasPrefix(b.Dialect.ID, "cflow.dialect.") || b.Dialect.EventSchemaRevision == "" {
			t.Fatalf("provider %s has an incomplete dialect binding", name)
		}
		if b.SessionContract.StartEvent == "" || b.SessionContract.IDField == "" ||
			len(b.SessionContract.TerminalEvents) == 0 || b.SessionContract.ConflictRule == "" {
			t.Fatalf("provider %s has an incomplete session contract", name)
		}
		if len(b.StartCapabilities) == 0 || len(b.ResumeCapabilities) == 0 {
			t.Fatalf("provider %s declares no start or resume capabilities", name)
		}
	}
}
