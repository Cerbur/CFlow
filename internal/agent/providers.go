// Provider Registry (design 14.2). The embedded providers.yaml binds each
// Provider to its executable name/path policy, supported version range,
// binary identity policy, Dialect ID and event schema revision, Session
// ID event contract, Start/Resume capabilities, Cancel and Budget
// behaviour, and known incompatibilities. Loading fails closed on unknown
// dialects, malformed bindings, unknown YAML fields, or duplicates;
// OpenCode may be listed only as disabled P1 metadata and can never be
// selected.
package agent

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"

	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/protocols"
)

// dialectIDPattern is the closed form of a Dialect ID: cflow.dialect.<id>.v<n>.
const dialectIDPattern = `^cflow\.dialect\.[a-z0-9-]+\.v[0-9]+$`

var dialectIDRE = regexp.MustCompile(dialectIDPattern)

// knownDialects are the Dialect IDs this binary's adapters can parse.
// Every enabled Provider must bind one of them; a disabled P1 provider
// must still use the dialect ID form but can never be selected.
var knownDialects = map[string]bool{
	"cflow.dialect.fake.v1":               true,
	"cflow.dialect.codex-jsonl.v1":        true,
	"cflow.dialect.claude-stream-json.v1": true,
}

// ProviderStatus declares whether a Provider may be selected.
type ProviderStatus string

const (
	// ProviderEnabled Providers can be selected for routes.
	ProviderEnabled ProviderStatus = "enabled"
	// ProviderDisabledP1 Providers are listed as P1 metadata only and can
	// never be selected (OpenCode).
	ProviderDisabledP1 ProviderStatus = "disabled-p1"
)

// Valid reports whether s is a declared Provider Status.
func (s ProviderStatus) Valid() bool {
	switch s {
	case ProviderEnabled, ProviderDisabledP1:
		return true
	}
	return false
}

// String renders the Provider Status.
func (s ProviderStatus) String() string { return string(s) }

// ExecutablePolicy binds the executable name and the path resolution
// policy of one Provider.
type ExecutablePolicy struct {
	Name       string `json:"name"`
	PathPolicy string `json:"path_policy"`
}

// DialectBinding names the wire dialect and its event schema revision.
type DialectBinding struct {
	ID                  string `json:"id"`
	EventSchemaRevision string `json:"event_schema_revision"`
}

// SessionContract states how the Provider Session ID is established and
// carried by protocol events (design 14.3).
type SessionContract struct {
	StartEvent     string   `json:"start_event"`
	IDField        string   `json:"id_field"`
	TerminalEvents []string `json:"terminal_events"`
	ConflictRule   string   `json:"conflict_rule"`
}

// ProviderBinding is one immutable Provider protocol binding (design
// 14.2): executable policy, supported version range, binary identity
// policy, dialect, Session event contract, Start/Resume capabilities,
// Cancel and Budget behaviour, and known incompatibilities. Hash is the
// binding's content hash, excluded from its own digest.
type ProviderBinding struct {
	Name                   string           `json:"name"`
	Status                 ProviderStatus   `json:"status"`
	Revision               string           `json:"revision"`
	Executable             ExecutablePolicy `json:"executable"`
	VersionRange           string           `json:"version_range"`
	BinaryIdentity         string           `json:"binary_identity"`
	Dialect                DialectBinding   `json:"dialect"`
	SessionContract        SessionContract  `json:"session_contract"`
	StartCapabilities      []string         `json:"start_capabilities"`
	ResumeCapabilities     []string         `json:"resume_capabilities"`
	CancelBehavior         string           `json:"cancel_behavior"`
	BudgetBehavior         string           `json:"budget_behavior"`
	KnownIncompatibilities []string         `json:"known_incompatibilities"`
	Hash                   string           `json:"hash,omitempty"`
}

// ProviderRegistry is an immutable registry of Provider bindings.
type ProviderRegistry struct {
	revision string
	byName   map[string]ProviderBinding
}

// LoadProviderRegistry parses the embedded providers.yaml into an
// immutable registry. Unknown dialects, malformed bindings, unknown YAML
// fields, or duplicates fail the whole load.
func LoadProviderRegistry() (*ProviderRegistry, error) {
	data, err := protocols.FS.ReadFile("providers.yaml")
	if err != nil {
		return nil, fmt.Errorf("provider registry: %w", err)
	}
	return parseProviders(data)
}

// providerYAML is the on-disk binding shape.
type providerYAML struct {
	Name            string         `yaml:"name"`
	Status          string         `yaml:"status"`
	Revision        string         `yaml:"revision"`
	Executable      executableYAML `yaml:"executable"`
	VersionRange    string         `yaml:"version_range"`
	BinaryIdentity  string         `yaml:"binary_identity_policy"`
	Dialect         dialectYAML    `yaml:"dialect"`
	SessionContract sessionYAML    `yaml:"session_contract"`
	Start           []string       `yaml:"start_capabilities"`
	Resume          []string       `yaml:"resume_capabilities"`
	Cancel          string         `yaml:"cancel_behavior"`
	Budget          string         `yaml:"budget_behavior"`
	Incompat        []string       `yaml:"known_incompatibilities"`
}

type executableYAML struct {
	Name       string `yaml:"name"`
	PathPolicy string `yaml:"path_policy"`
}

type dialectYAML struct {
	ID                  string `yaml:"id"`
	EventSchemaRevision string `yaml:"event_schema_revision"`
}

type sessionYAML struct {
	StartEvent     string   `yaml:"start_event"`
	IDField        string   `yaml:"id_field"`
	TerminalEvents []string `yaml:"terminal_events"`
	ConflictRule   string   `yaml:"conflict_rule"`
}

// parseProviders parses and validates a providers.yaml document.
func parseProviders(data []byte) (*ProviderRegistry, error) {
	var doc struct {
		Providers []providerYAML `yaml:"providers"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("provider registry: %w", err)
	}
	if len(doc.Providers) == 0 {
		return nil, fmt.Errorf("provider registry: no providers bound")
	}
	reg := &ProviderRegistry{byName: map[string]ProviderBinding{}}
	for _, py := range doc.Providers {
		b, err := validateProviderYAML(py)
		if err != nil {
			return nil, fmt.Errorf("provider registry: %s: %w", py.Name, err)
		}
		if _, dup := reg.byName[b.Name]; dup {
			return nil, fmt.Errorf("provider registry: duplicate provider %q", b.Name)
		}
		reg.byName[b.Name] = b
	}
	reg.revision = providerRegistryHash(reg)
	return reg, nil
}

// validateProviderYAML binds one entry, failing closed on unknown
// dialects, incomplete bindings, or undeclared statuses.
func validateProviderYAML(py providerYAML) (ProviderBinding, error) {
	if py.Name == "" {
		return ProviderBinding{}, fmt.Errorf("name is required")
	}
	status := ProviderStatus(py.Status)
	if !status.Valid() {
		return ProviderBinding{}, fmt.Errorf("unknown status %q", py.Status)
	}
	if py.Revision == "" {
		return ProviderBinding{}, fmt.Errorf("revision is required")
	}
	if py.Executable.Name == "" || py.Executable.PathPolicy == "" {
		return ProviderBinding{}, fmt.Errorf("executable policy is incomplete")
	}
	if py.VersionRange == "" {
		return ProviderBinding{}, fmt.Errorf("version range is required")
	}
	if py.BinaryIdentity == "" {
		return ProviderBinding{}, fmt.Errorf("binary identity policy is required")
	}
	if !dialectIDRE.MatchString(py.Dialect.ID) {
		return ProviderBinding{}, fmt.Errorf("dialect id %q does not match the dialect form", py.Dialect.ID)
	}
	if py.Dialect.EventSchemaRevision == "" {
		return ProviderBinding{}, fmt.Errorf("dialect event schema revision is required")
	}
	if status == ProviderEnabled && !knownDialects[py.Dialect.ID] {
		return ProviderBinding{}, fmt.Errorf("dialect %q is not supported by this binary", py.Dialect.ID)
	}
	if py.SessionContract.StartEvent == "" || py.SessionContract.IDField == "" ||
		len(py.SessionContract.TerminalEvents) == 0 || py.SessionContract.ConflictRule == "" {
		return ProviderBinding{}, fmt.Errorf("session contract is incomplete")
	}
	if py.Cancel == "" || py.Budget == "" {
		return ProviderBinding{}, fmt.Errorf("cancel and budget behaviour are required")
	}
	b := ProviderBinding{
		Name:                   py.Name,
		Status:                 status,
		Revision:               py.Revision,
		Executable:             ExecutablePolicy{Name: py.Executable.Name, PathPolicy: py.Executable.PathPolicy},
		VersionRange:           py.VersionRange,
		BinaryIdentity:         py.BinaryIdentity,
		Dialect:                DialectBinding{ID: py.Dialect.ID, EventSchemaRevision: py.Dialect.EventSchemaRevision},
		SessionContract:        SessionContract{StartEvent: py.SessionContract.StartEvent, IDField: py.SessionContract.IDField, TerminalEvents: append([]string(nil), py.SessionContract.TerminalEvents...), ConflictRule: py.SessionContract.ConflictRule},
		StartCapabilities:      append([]string(nil), py.Start...),
		ResumeCapabilities:     append([]string(nil), py.Resume...),
		CancelBehavior:         py.Cancel,
		BudgetBehavior:         py.Budget,
		KnownIncompatibilities: append([]string(nil), py.Incompat...),
	}
	b.Hash = b.entryHash()
	return b, nil
}

// Lookup returns a binding by name, including disabled P1 metadata.
// Returned slices are copies: the registry itself is immutable.
func (r *ProviderRegistry) Lookup(name string) (ProviderBinding, bool) {
	b, ok := r.byName[name]
	if !ok {
		return ProviderBinding{}, false
	}
	return cloneBinding(b), true
}

// Select returns a binding that may actually be selected: the Provider
// must be bound and enabled. Unknown providers and disabled P1 providers
// fail closed.
func (r *ProviderRegistry) Select(name string) (ProviderBinding, error) {
	b, ok := r.Lookup(name)
	if !ok {
		return ProviderBinding{}, fmt.Errorf("provider %q is not bound in the registry", name)
	}
	if b.Status != ProviderEnabled {
		return ProviderBinding{}, fmt.Errorf("provider %q cannot be selected: %s", name, b.Status)
	}
	return b, nil
}

// Revision returns the deterministic registry revision: the digest of the
// canonical serialization of every binding, ordered by name.
func (r *ProviderRegistry) Revision() string { return r.revision }

// EnabledNames returns the selectable Provider names in canonical order
// (the eligible route list the planning Sessions may choose from).
func (r *ProviderRegistry) EnabledNames() []string {
	var names []string
	for n, b := range r.byName {
		if b.Status == ProviderEnabled {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// entryHash is the content hash of one binding: the digest of its
// canonical serialization with the hash field excluded from its own
// digest. Reordering YAML map keys never changes it.
func (b ProviderBinding) entryHash() string {
	h := b
	h.Hash = ""
	return sha256Hex(jsonMarshal(h))
}

// providerRegistryHash digests the canonical serialization of every
// binding, ordered by name.
func providerRegistryHash(reg *ProviderRegistry) string {
	names := make([]string, 0, len(reg.byName))
	for n := range reg.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	entries := make([]ProviderBinding, 0, len(names))
	for _, n := range names {
		entries = append(entries, reg.byName[n])
	}
	return sha256Hex(jsonMarshal(entries))
}

// cloneBinding copies every slice so callers cannot mutate registry state.
func cloneBinding(b ProviderBinding) ProviderBinding {
	b.SessionContract.TerminalEvents = append([]string(nil), b.SessionContract.TerminalEvents...)
	b.StartCapabilities = append([]string(nil), b.StartCapabilities...)
	b.ResumeCapabilities = append([]string(nil), b.ResumeCapabilities...)
	b.KnownIncompatibilities = append([]string(nil), b.KnownIncompatibilities...)
	return b
}
