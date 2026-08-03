package compile

// The compiler's validation phases (design 11): Spec dependency
// validation, write-scope conflicts, acceptance coverage, route
// capability, and resource lock names. Same-package split of the
// deterministic Compiler: no public seam added.

import (
	"fmt"
	"sort"
	"strings"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
)

func parseSpecs(bodies [][]byte) ([]Spec, error) {
	if len(bodies) == 0 {
		return nil, schemaInvalid("no specs to compile")
	}
	type parsed struct {
		body []byte
		spec Spec
	}
	all := make([]parsed, 0, len(bodies))
	seen := map[string]bool{}
	for _, body := range bodies {
		s, err := ParseSpec(body)
		if err != nil {
			return nil, err
		}
		if !nodeIDRE.MatchString(s.ID) {
			return nil, schemaInvalid(fmt.Sprintf("spec id %q must match the node id form", s.ID))
		}
		if seen[s.ID] {
			return nil, schemaInvalid(fmt.Sprintf("duplicate spec id %q", s.ID))
		}
		seen[s.ID] = true
		all = append(all, parsed{body: body, spec: s})
	}
	// Canonical Spec order: sorted by id. The request's own body slice is
	// reordered in place so the compile evidence hashes are canonical too.
	sort.Slice(all, func(i, j int) bool { return all[i].spec.ID < all[j].spec.ID })
	specs := make([]Spec, 0, len(all))
	for i, p := range all {
		bodies[i] = p.body
		specs = append(specs, p.spec)
	}
	return specs, nil
}

func validateCatalog(catalog Catalog) error {
	seen := map[string]bool{}
	for _, e := range catalog.Entries {
		if seen[e.CommandID] {
			return schemaInvalid(fmt.Sprintf("duplicate catalog command id %q", e.CommandID))
		}
		seen[e.CommandID] = true
		if !validSourceHash(e.Source) {
			return schemaInvalid(fmt.Sprintf("catalog entry %q carries an invalid executable hash", e.CommandID))
		}
	}
	return nil
}

// validSourceHash accepts the identity convention
// "<kind>:<path>@sha256:<64 hex>"; a malformed hash suffix is a changed
// executable identity and fails the compile.
func validSourceHash(source string) bool {
	idx := strings.LastIndex(source, "@sha256:")
	if idx < 0 {
		return true // no hash declared: the identity is bound by the catalog hash
	}
	return sha256HexRE.MatchString(source[idx+len("@sha256:"):])
}

func validateDependencies(specs []Spec) error {
	byID := map[string]bool{}
	for _, s := range specs {
		byID[s.ID] = true
	}
	adj := map[string][]string{}
	for _, s := range specs {
		for _, dep := range s.DependsOn {
			if !byID[dep] {
				return schemaInvalid(fmt.Sprintf("spec %s depends on unknown spec %q", s.ID, dep))
			}
			adj[s.ID] = append(adj[s.ID], dep)
		}
	}
	// Cycle detection (DFS with colors).
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		color[id] = gray
		for _, dep := range adj[id] {
			switch color[dep] {
			case gray:
				return schemaInvalid(fmt.Sprintf("spec dependency cycle involving %s and %s", id, dep))
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[id] = black
		return nil
	}
	for _, s := range specs {
		if color[s.ID] == white {
			if err := visit(s.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateWriteScopes rejects overlapping write scopes without ordering
// or a shared lock (PRD: 重叠写范围必须有依赖顺序或共享 Resource Lock).
func validateWriteScopes(specs []Spec) error {
	for i := 0; i < len(specs); i++ {
		for j := i + 1; j < len(specs); j++ {
			if !scopesOverlap(specs[i].WriteScope, specs[j].WriteScope) {
				continue
			}
			ordered := depReachable(specs, specs[i].ID, specs[j].ID) ||
				depReachable(specs, specs[j].ID, specs[i].ID)
			sharedLock := sharedLockName(specs[i].Locks, specs[j].Locks)
			if !ordered && sharedLock == "" {
				return model.NewFault(model.CodeScopeViolation, fmt.Sprintf(
					"specs %s and %s have overlapping write scopes without ordering or a shared lock", specs[i].ID, specs[j].ID))
			}
		}
	}
	return nil
}

// scopesOverlap reports whether any write-scope entry of a overlaps any
// entry of b (directory-prefix containment after normalizing trailing
// glob markers).
func scopesOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if scopeEntryOverlaps(x, y) {
				return true
			}
		}
	}
	return false
}

func scopeEntryOverlaps(x, y string) bool {
	x = normalizeScope(x)
	y = normalizeScope(y)
	return x == y || strings.HasPrefix(x, y+"/") || strings.HasPrefix(y, x+"/")
}

func normalizeScope(entry string) string {
	return strings.TrimSuffix(strings.TrimSuffix(entry, "/"), "/**")
}

// depReachable reports whether from transitively depends on to.
func depReachable(specs []Spec, from, to string) bool {
	adj := map[string][]string{}
	for _, s := range specs {
		adj[s.ID] = s.DependsOn
	}
	seen := map[string]bool{}
	var walk func(id string) bool
	walk = func(id string) bool {
		if id == to {
			return true
		}
		if seen[id] {
			return false
		}
		seen[id] = true
		for _, dep := range adj[id] {
			if walk(dep) {
				return true
			}
		}
		return false
	}
	return walk(from)
}

func sharedLockName(a, b []string) string {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return x
			}
		}
	}
	return ""
}

// validateAcceptance binds every acceptance command to an existing
// Catalog entry with the task verification purpose.
func validateAcceptance(specs []Spec, catalog Catalog) error {
	byID := map[string]CatalogEntry{}
	for _, e := range catalog.Entries {
		byID[e.CommandID] = e
	}
	for _, s := range specs {
		if len(s.Acceptance.VerificationCommandIDs) == 0 {
			return schemaInvalid(fmt.Sprintf("spec %s has no verification commands", s.ID))
		}
		for _, cmd := range s.Acceptance.VerificationCommandIDs {
			entry, ok := byID[cmd]
			if !ok {
				return schemaInvalid(fmt.Sprintf("spec %s references unknown catalog command %q", s.ID, cmd))
			}
			if entry.Purpose != "task_verify" {
				return schemaInvalid(fmt.Sprintf(
					"spec %s references command %q with purpose %q; a task acceptance command must be task_verify",
					s.ID, cmd, entry.Purpose))
			}
		}
	}
	return nil
}

// validateRoutes binds every Spec route to an enabled Provider whose
// protocol capabilities satisfy structured events, Session identity, and
// the budget contract, and validates the Resource Lock names the
// Compiler injects.
func validateRoutes(specs []Spec, reg *agent.ProviderRegistry) error {
	for _, s := range specs {
		if s.Route == nil {
			return schemaInvalid(fmt.Sprintf("spec %s has no route", s.ID))
		}
		binding, err := reg.Select(s.Route.Provider)
		if err != nil {
			return schemaInvalid(fmt.Sprintf("spec %s route: %v", s.ID, err))
		}
		if !hasCap(binding.StartCapabilities, "structured_output") ||
			!hasCap(binding.StartCapabilities, "session_id_on_start") {
			return schemaInvalid(fmt.Sprintf(
				"spec %s route provider %q lacks the structured session capabilities", s.ID, binding.Name))
		}
		if s.TimeoutSeconds < 1 || s.MaxRetry < 0 {
			return schemaInvalid(fmt.Sprintf("spec %s carries an invalid timeout or retry bound", s.ID))
		}
		for _, lock := range s.Locks {
			if strings.TrimSpace(lock) == "" || strings.ContainsAny(lock, " \t") {
				return schemaInvalid(fmt.Sprintf("spec %s declares an invalid resource lock name %q", s.ID, lock))
			}
			if strings.HasPrefix(lock, "integration:") {
				return schemaInvalid(fmt.Sprintf(
					"spec %s declares the reserved integration lock name %q", s.ID, lock))
			}
		}
	}
	return nil
}

// buildSkeleton constructs the deterministic AgentTask/Verify/Merge
// chain per Spec, the dependency edges, and the single FinalVerify.
