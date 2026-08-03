// Package config loads CFlow's one strict local configuration file and
// resolves it against explicit CLI input and embedded safe defaults.
//
// The schema is closed: unknown keys and invalid values are rejected with
// yaml.Decoder.KnownFields(true), and credentials, scripts, and raw
// command strings are impossible because no such key exists in the
// schema. Provider-owned configuration is never read or copied. Only
// CFLOW_HOME may carry persistent Runtime configuration (design 20.1).
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"

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
	Concurrency *int `yaml:"concurrency"`
}

// Overrides are explicit per-command CLI inputs for the current command.
// They win over File values (design 20.1).
type Overrides struct {
	Concurrency *int
}

// Resolved is the validated configuration consumed by the Runtime.
type Resolved struct {
	Concurrency int
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
	// The shadow struct keeps KnownFields strictness and validates the
	// scalar type at the decode boundary: yaml coerces floating-point
	// scalars into int fields silently, so every value must prove it is
	// an integer before it enters the File. The field is a value
	// yaml.Node: the pointer form cannot decode scalars with this yaml
	// version ("cannot unmarshal !!int into yaml.Node"), which would make
	// every configured value fail closed.
	var raw struct {
		Concurrency yaml.Node `yaml:"concurrency"`
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
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return File{}, &Error{Msg: fmt.Sprintf("parse config %s: more than one YAML document", path)}
		}
		return File{}, &Error{Msg: fmt.Sprintf("parse config %s: %v", path, err)}
	}
	return f, nil
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
// only concurrency default that can never collide with a future schedule.
func builtInSafeDefaults() Resolved {
	return Resolved{Concurrency: 1}
}

func applyFile(out *Resolved, file File) {
	if file.Concurrency != nil {
		out.Concurrency = *file.Concurrency
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
	return out, nil
}
