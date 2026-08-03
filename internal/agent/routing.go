// Immutable Routing Policy (Task 16, design 14.2/14.4, PRD 已确认：Agent
// 交互主协议). The RoutingPolicySet is the per-Purpose ordered approved
// binding list fixed at Execution Approval: for every Purpose the
// workflow references, the approved Provider bindings in order — the
// primary first, then the approved fallbacks. Each RouteBinding pins the
// executable identity facts observed at Approval (path, sha256, CLI
// version, dialect, registry revision) together with the approved model,
// hard budget, timeout, prompt hash, Provider default-permission
// disclosure, and the per-operation capability lists. Before every
// Start/Resume the Runtime re-detects the executable and
// Compare-and-Swaps it against the approved binding; any drift is
// PROVIDER_PROTOCOL_BINDING_CHANGED and nothing may start (PRD 约束 306).
package agent

import (
	"bytes"
	"encoding/json"
	"fmt"

	"cflow.local/cflow/internal/model"
)

// RouteBinding is one immutable approved Provider binding of one Purpose
// route. Provider is the registry Provider name; Model the approved
// optional model ("" = Provider default); BudgetUSD the approved hard
// budget; TimeoutSeconds the approved timeout; PromptHash the approved
// prompt body hash; Disclosure the Provider default-permission
// disclosure text; DialectID and RegistryRevision the exact protocol
// facts the Approval was judged against; StartCapabilities and
// ResumeCapabilities the approved per-operation capability lists;
// ExecutablePath/ExecutableSHA256/CLIVersion the executable identity
// facts observed at Approval (empty for in-process test adapters whose
// binary identity is not pin-verified). Hash is the binding's content
// hash, excluded from its own digest.
type RouteBinding struct {
	Provider           string
	Model              string
	BudgetUSD          float64
	TimeoutSeconds     int
	PromptHash         string
	Disclosure         string
	DialectID          string
	RegistryRevision   string
	StartCapabilities  []string
	ResumeCapabilities []string
	ExecutablePath     string
	ExecutableSHA256   string
	CLIVersion         string

	Hash string `json:"-"`
}

// RoutingPolicy is one Purpose's ordered approved binding list: the
// primary first, then the approved fallbacks in order. Hash is the
// policy's content hash, excluded from its own digest.
type RoutingPolicy struct {
	Purpose  model.AgentPurpose
	Bindings []RouteBinding

	Hash string `json:"-"`
}

// RoutingPolicySet is the immutable set of per-Purpose policies a
// workflow's Execution Approval bound. It is parsed from the immutable
// routing-policy Artifact; nothing mutates it. ConfigModel and
// ConfigFallbacks are the resolved strict-configuration routing inputs
// (design 20.1) the Approval bound together with the policies: editing
// the configuration after the Approval changes the resolved content and
// the dispatch drift gate refuses with APPROVAL_INPUT_CHANGED even when
// the edited value happens to equal a Spec's own route value.
type RoutingPolicySet struct {
	// ConfigModel is the approved default route model ("" = none
	// configured; the Spec's explicit route model still wins per route).
	ConfigModel string
	// ConfigFallbacks are the approved ordered fallback Providers of
	// every Purpose route ("" = none configured).
	ConfigFallbacks []string

	Policies []RoutingPolicy

	Hash string `json:"-"`
}

// Policy returns the approved policy of one Purpose (false when the
// Purpose has no approved route).
func (s *RoutingPolicySet) Policy(purpose model.AgentPurpose) (RoutingPolicy, bool) {
	if s == nil {
		return RoutingPolicy{}, false
	}
	for _, p := range s.Policies {
		if p.Purpose == purpose {
			return p, true
		}
	}
	return RoutingPolicy{}, false
}

// Resolve returns the approved binding of one Provider inside one
// Purpose (false when the Purpose or the Provider is not approved).
func (s *RoutingPolicySet) Resolve(purpose model.AgentPurpose, provider string) (RouteBinding, bool) {
	pol, ok := s.Policy(purpose)
	if !ok {
		return RouteBinding{}, false
	}
	for _, b := range pol.Bindings {
		if b.Provider == provider {
			return b, true
		}
	}
	return RouteBinding{}, false
}

// Fallback returns the next approved binding after provider inside one
// Purpose: the ordered fallback of an unrecoverable Resume. False when
// the Purpose has no approved fallback beyond the current Provider
// (PRD 约束 306: an unapproved Fallback never substitutes silently).
func (s *RoutingPolicySet) Fallback(purpose model.AgentPurpose, provider string) (RouteBinding, bool) {
	pol, ok := s.Policy(purpose)
	if !ok {
		return RouteBinding{}, false
	}
	for i, b := range pol.Bindings {
		if b.Provider != provider {
			continue
		}
		if i+1 < len(pol.Bindings) {
			return pol.Bindings[i+1], true
		}
		return RouteBinding{}, false
	}
	return RouteBinding{}, false
}

// PromptHash digests one prompt body exactly as the Runtime records
// prompt hashes (design 14.5): the binding's approved prompt pin and the
// Runtime's recorded prompt hash of a Start always match.
func PromptHash(text string) string { return hashText(text) }

// ContentEqual reports whether two policy sets approve the same route
// content: Purpose, Provider, model, budget, timeout, prompt hash,
// disclosure, protocol facts, and capability lists. The executable
// identity pins (path, sha256, CLI version) are observed facts, not
// approval content: a fresh resolution without re-detection must compare
// equal to the approved set when nothing configured changed, and differ
// when config, Specs, or the Registry changed (the config-drift gate).
func ContentEqual(a, b *RoutingPolicySet) bool {
	if a == nil || b == nil {
		return a == b
	}
	ca := a.cloneBlankPins()
	cb := b.cloneBlankPins()
	return string(ca) == string(cb)
}

// cloneBlankPins serializes the set with the executable pins blanked.
func (s *RoutingPolicySet) cloneBlankPins() []byte {
	clone := *s
	clone.Hash = ""
	clone.ConfigFallbacks = append([]string(nil), s.ConfigFallbacks...)
	clone.Policies = make([]RoutingPolicy, len(s.Policies))
	for i, p := range s.Policies {
		cp := p
		cp.Hash = ""
		cp.Bindings = make([]RouteBinding, len(p.Bindings))
		for j, b := range p.Bindings {
			b.Hash = ""
			b.ExecutablePath = ""
			b.ExecutableSHA256 = ""
			b.CLIVersion = ""
			cp.Bindings[j] = b
		}
		clone.Policies[i] = cp
	}
	data, err := json.Marshal(clone)
	if err != nil {
		panic(fmt.Sprintf("agent: routing policy cannot be serialized: %v", err))
	}
	return data
}

// routingPolicySetBody is the canonical Artifact body of one Routing
// Policy Set (the exact serialization the Execution Approval binds by
// hash). SchemaVersion keeps the body readable across binary versions.
type routingPolicySetBody struct {
	SchemaVersion   int             `json:"schema_version"`
	ConfigModel     string          `json:"config_model,omitempty"`
	ConfigFallbacks []string        `json:"config_fallbacks,omitempty"`
	Policies        []RoutingPolicy `json:"policies"`
}

// MarshalRoutingPolicySet serializes the set into its canonical body.
func MarshalRoutingPolicySet(s *RoutingPolicySet) ([]byte, error) {
	body := routingPolicySetBody{
		SchemaVersion:   1,
		ConfigModel:     s.ConfigModel,
		ConfigFallbacks: append([]string(nil), s.ConfigFallbacks...),
		Policies:        s.Policies,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, model.InvariantFault(fmt.Errorf("routing policy cannot be serialized"))
	}
	return data, nil
}

// ParseRoutingPolicySet parses and validates a routing-policy Artifact
// body. Unknown fields fail the parse (the body is canonical policy).
func ParseRoutingPolicySet(data []byte) (*RoutingPolicySet, error) {
	var body routingPolicySetBody
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return nil, model.InvalidInputFault("routing policy body is not canonical: " + err.Error())
	}
	if body.SchemaVersion != 1 {
		return nil, model.InvalidInputFault(fmt.Sprintf(
			"routing policy schema version %d is not supported", body.SchemaVersion))
	}
	set := &RoutingPolicySet{
		ConfigModel:     body.ConfigModel,
		ConfigFallbacks: append([]string(nil), body.ConfigFallbacks...),
		Policies:        body.Policies,
	}
	if err := set.validate(); err != nil {
		return nil, err
	}
	return set, nil
}

// validate fails closed on a policy set that cannot be dispatched: an
// unknown Purpose, an empty binding list, an empty Provider, a Provider
// bound twice, missing protocol facts, or a binding without the Start
// capabilities (such a binding could never have been approved).
func (s *RoutingPolicySet) validate() error {
	if len(s.Policies) == 0 {
		return model.InvalidInputFault("routing policy carries no purposes")
	}
	seen := map[model.AgentPurpose]bool{}
	for _, p := range s.Policies {
		if !p.Purpose.Valid() {
			return model.InvalidInputFault("routing policy carries an unknown purpose")
		}
		if seen[p.Purpose] {
			return model.InvalidInputFault("routing policy carries a duplicate purpose")
		}
		seen[p.Purpose] = true
		if len(p.Bindings) == 0 {
			return model.InvalidInputFault("routing policy purpose " + p.Purpose.String() + " carries no bindings")
		}
		providers := map[string]bool{}
		for _, b := range p.Bindings {
			if b.Provider == "" {
				return model.InvalidInputFault("routing policy carries an empty provider")
			}
			if providers[b.Provider] {
				return model.InvalidInputFault(fmt.Sprintf(
					"routing policy purpose %s binds provider %q twice", p.Purpose, b.Provider))
			}
			providers[b.Provider] = true
			if b.DialectID == "" || b.RegistryRevision == "" {
				return model.InvalidInputFault(fmt.Sprintf(
					"routing policy binding %q lacks the protocol facts", b.Provider))
			}
			if !hasCaps(b.StartCapabilities, requiredStartCapabilities) {
				return model.InvalidInputFault(fmt.Sprintf(
					"routing policy binding %q lacks the start capabilities", b.Provider))
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Compare-and-Swap (PRD 约束 306, design 14.2)
// ---------------------------------------------------------------------------

// CompareInstallation Compare-and-Swaps one fresh Detect result against
// the approved binding before an operation. A provider that is not
// SUPPORTED blocks with PROVIDER_PROTOCOL_UNSUPPORTED; an executable
// whose identity (path, sha256, CLI version), dialect, or registry
// revision drifted from the Approval blocks with
// PROVIDER_PROTOCOL_BINDING_CHANGED. In-process test adapters pin no
// executable identity: empty pin facts are skipped, never compared.
// The per-operation capability gate uses the binding's approved
// capability list for exactly the operation being started (Start and
// Resume are never inferred from each other, PRD 已确认).
func CompareInstallation(inst Installation, b RouteBinding, resume bool) error {
	if inst.Compatibility != CompatibilitySupported {
		return model.NewFault(model.CodeProviderProtocolUnsupported,
			"provider is not supported: "+string(inst.Compatibility))
	}
	caps := b.StartCapabilities
	required := requiredStartCapabilities
	if resume {
		caps = b.ResumeCapabilities
		required = requiredResumeCapabilities
	}
	if !hasCaps(caps, required) {
		op := "start"
		if resume {
			op = "resume"
		}
		return model.NewFault(model.CodeProviderProtocolUnsupported,
			fmt.Sprintf("binding lacks the required %s capabilities", op))
	}
	if b.ExecutablePath != "" && inst.ExecutablePath != b.ExecutablePath {
		return bindingChanged(fmt.Sprintf("executable path drifted: %q != approved %q", inst.ExecutablePath, b.ExecutablePath))
	}
	if b.ExecutableSHA256 != "" && inst.ExecutableSHA256 != b.ExecutableSHA256 {
		return bindingChanged("executable sha256 does not match the approved binary identity")
	}
	if b.CLIVersion != "" && inst.CLIVersion != b.CLIVersion {
		return bindingChanged(fmt.Sprintf("cli version drifted: %q != approved %q", inst.CLIVersion, b.CLIVersion))
	}
	if inst.DialectID != b.DialectID {
		return bindingChanged(fmt.Sprintf("detected dialect %q does not match the approved dialect %q", inst.DialectID, b.DialectID))
	}
	if inst.RegistryRevision != b.RegistryRevision {
		return bindingChanged("registry revision does not match the approved protocol registry")
	}
	return nil
}

// bindingChanged is the PROVIDER_PROTOCOL_BINDING_CHANGED fault
// constructor: the drift closes the Dispatch Gate and requires a
// regenerated Dry Run and Execution Approval (PRD 失败分类).
func bindingChanged(text string) error {
	return model.NewFault(model.CodeProviderBindingChanged, text)
}

// hasCaps reports whether the declared capability list contains every
// required capability.
func hasCaps(have, required []string) bool {
	for _, want := range required {
		ok := false
		for _, h := range have {
			if h == want {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
