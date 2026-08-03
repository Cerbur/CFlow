// Package config loads CFlow's one strict local configuration file and
// resolves it against explicit CLI input and embedded safe defaults.
//
// The schema is closed: unknown keys and invalid values are rejected with
// yaml.Decoder.KnownFields(true), and credentials, scripts, and raw
// command strings are impossible because no such key exists in the
// schema. Provider-owned configuration is never read or copied. Only
// CFLOW_HOME may carry persistent Runtime configuration (design 20.1).
//
// Routing and budget values (design 20.1) resolve here: the approved
// model default and the ordered approved fallback Providers of every
// Purpose, and the hard budget cap of one Agent run. Resolved values
// become immutable inputs to the Execution Approval (the routing-policy
// and budget-policy Artifacts bind them by hash); editing the file after
// an Approval changes the resolved hashes and requires a successor
// Approval.
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Error is a configuration failure. The CLI maps every config.Error to
// exit class 4 (local environment or compatibility precondition failure).
type Error struct {
	Msg string
}

func (e *Error) Error() string { return e.Msg }

// File is the decoded CFLOW_HOME/config.yaml schema. Pointer fields stay
// nil when absent so precedence can distinguish "not configured" from an
// explicit value.
type File struct {
	Concurrency *int     `yaml:"concurrency"`
	Routing     *Routing `yaml:"routing"`
	Budget      *Budget  `yaml:"budget"`
}

// Routing is the strict routing section: the approved default model ("" =
// Provider default) and the ordered approved fallback Providers of every
// Purpose route.
type Routing struct {
	Model     string   `yaml:"model"`
	Fallbacks []string `yaml:"fallbacks"`
}

// Budget is the strict budget section: the approved hard budget cap of
// one Agent run in USD (0 = no cap).
type Budget struct {
	MaxUSDPerRun *float64 `yaml:"max_usd_per_run"`
}

// Overrides are explicit per-command CLI inputs for the current command.
// They win over File values (design 20.1).
type Overrides struct {
	Concurrency *int
}

// Resolved is the validated configuration consumed by the Runtime.
type Resolved struct {
	Concurrency int
	// Model is the approved default model of a route ("" = Provider
	// default; the Spec's explicit route model wins over it).
	Model string
	// Fallbacks are the ordered approved fallback Providers of every
	// Purpose route.
	Fallbacks []string
	// MaxUSDPerRun is the approved hard budget cap of one Agent run in
	// USD (0 = unlimited; the Spec's explicit route budget wins over it).
	MaxUSDPerRun float64
}

// Load decodes exactly one strict YAML document from path. An absent or
// unreadable file, an unknown key, an invalid value, or an extra YAML
// document is an error. An empty or comment-only file is valid and means
// "no configuration".
func Load(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, &Error{Msg: fmt.Sprintf("read config %s: %v", path, err)}
	}
	// The shadow struct keeps KnownFields strictness and validates every
	// scalar type at the decode boundary: yaml coerces floating-point
	// scalars into int fields silently, so every value must prove its
	// type before it enters the File. The fields are value yaml.Nodes:
	// the pointer form cannot decode scalars with this yaml version
	// ("cannot unmarshal !!int into yaml.Node"), which would make every
	// configured value fail closed.
	var raw struct {
		Concurrency yaml.Node   `yaml:"concurrency"`
		Routing     *rawRouting `yaml:"routing"`
		Budget      *rawBudget  `yaml:"budget"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		if err != io.EOF {
			return File{}, &Error{Msg: fmt.Sprintf("parse config %s: %v", path, err)}
		}
		// No document: zero File, embedded safe defaults apply.
		return File{}, nil
	}
	var f File
	if raw.Concurrency.Kind != 0 {
		if raw.Concurrency.ShortTag() != "!!int" {
			return File{}, &Error{Msg: fmt.Sprintf("parse config %s: concurrency must be an integer", path)}
		}
		var v int
		if err := raw.Concurrency.Decode(&v); err != nil {
			return File{}, &Error{Msg: fmt.Sprintf("parse config %s: concurrency: %v", path, err)}
		}
		f.Concurrency = &v
	}
	if raw.Routing != nil {
		r, err := decodeRouting(path, raw.Routing)
		if err != nil {
			return File{}, err
		}
		f.Routing = &r
	}
	if raw.Budget != nil {
		b, err := decodeBudget(path, raw.Budget)
		if err != nil {
			return File{}, err
		}
		f.Budget = &b
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return File{}, &Error{Msg: fmt.Sprintf("parse config %s: more than one YAML document", path)}
		}
		return File{}, &Error{Msg: fmt.Sprintf("parse config %s: %v", path, err)}
	}
	return f, nil
}

// rawRouting is the strict routing section shadow: every value must prove
// its scalar type.
type rawRouting struct {
	Model     yaml.Node   `yaml:"model"`
	Fallbacks []yaml.Node `yaml:"fallbacks"`
}

func decodeRouting(path string, raw *rawRouting) (Routing, error) {
	var r Routing
	if raw.Model.Kind != 0 {
		if raw.Model.ShortTag() != "!!str" {
			return Routing{}, &Error{Msg: fmt.Sprintf("parse config %s: routing.model must be a string", path)}
		}
		if err := raw.Model.Decode(&r.Model); err != nil {
			return Routing{}, &Error{Msg: fmt.Sprintf("parse config %s: routing.model: %v", path, err)}
		}
	}
	for i, n := range raw.Fallbacks {
		if n.ShortTag() != "!!str" {
			return Routing{}, &Error{Msg: fmt.Sprintf("parse config %s: routing.fallbacks[%d] must be a string", path, i)}
		}
		var name string
		if err := n.Decode(&name); err != nil {
			return Routing{}, &Error{Msg: fmt.Sprintf("parse config %s: routing.fallbacks[%d]: %v", path, i, err)}
		}
		r.Fallbacks = append(r.Fallbacks, name)
	}
	return r, nil
}

// rawBudget is the strict budget section shadow.
type rawBudget struct {
	MaxUSDPerRun yaml.Node `yaml:"max_usd_per_run"`
}

func decodeBudget(path string, raw *rawBudget) (Budget, error) {
	var b Budget
	if raw.MaxUSDPerRun.Kind != 0 {
		if raw.MaxUSDPerRun.ShortTag() != "!!float" && raw.MaxUSDPerRun.ShortTag() != "!!int" {
			return Budget{}, &Error{Msg: fmt.Sprintf("parse config %s: budget.max_usd_per_run must be a number", path)}
		}
		var v float64
		if err := raw.MaxUSDPerRun.Decode(&v); err != nil {
			return Budget{}, &Error{Msg: fmt.Sprintf("parse config %s: budget.max_usd_per_run: %v", path, err)}
		}
		b.MaxUSDPerRun = &v
	}
	return b, nil
}

// Resolve applies precedence explicitly: CLI overrides first, then the
// file configuration, then the embedded safe default, and validates the
// result (design 20.1).
func Resolve(file File, cli Overrides) (Resolved, error) {
	out := builtInSafeDefaults()
	applyFile(&out, file)
	applyOverrides(&out, cli)
	return validate(out)
}

// builtInSafeDefaults is the embedded fallback: serial execution is the
// only concurrency default that can never collide with a future schedule;
// no model override, no fallback Provider, and no budget cap are the
// safe routing defaults (the Spec's explicit route is the only approved
// binding then).
func builtInSafeDefaults() Resolved {
	return Resolved{Concurrency: 1}
}

func applyFile(out *Resolved, file File) {
	if file.Concurrency != nil {
		out.Concurrency = *file.Concurrency
	}
	if file.Routing != nil {
		out.Model = file.Routing.Model
		out.Fallbacks = append([]string(nil), file.Routing.Fallbacks...)
	}
	if file.Budget != nil && file.Budget.MaxUSDPerRun != nil {
		out.MaxUSDPerRun = *file.Budget.MaxUSDPerRun
	}
}

func applyOverrides(out *Resolved, cli Overrides) {
	if cli.Concurrency != nil {
		out.Concurrency = *cli.Concurrency
	}
}

func validate(out Resolved) (Resolved, error) {
	if out.Concurrency < 1 {
		return Resolved{}, &Error{Msg: fmt.Sprintf("invalid concurrency %d: must be at least 1", out.Concurrency)}
	}
	if strings.TrimSpace(out.Model) == "" && out.Model != "" {
		return Resolved{}, &Error{Msg: "invalid routing.model: must not be blank"}
	}
	// The approved model travels in Provider argv (--model): a leading
	// dash or whitespace could inject flags, so it must be a plain
	// identifier (PRD 约束: argv-only launch).
	for _, r := range out.Model {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return Resolved{}, &Error{Msg: "invalid routing.model: must not contain whitespace"}
		}
	}
	if len(out.Model) > 0 && out.Model[0] == '-' {
		return Resolved{}, &Error{Msg: "invalid routing.model: must not start with '-'"}
	}
	for i, name := range out.Fallbacks {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t") {
			return Resolved{}, &Error{Msg: fmt.Sprintf("invalid routing.fallbacks[%d]: provider names must be plain identifiers", i)}
		}
	}
	if out.MaxUSDPerRun < 0 {
		return Resolved{}, &Error{Msg: fmt.Sprintf("invalid budget.max_usd_per_run %v: must not be negative", out.MaxUSDPerRun)}
	}
	return out, nil
}
