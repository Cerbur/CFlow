// Embedded strict schema validation (design 10.2 step 1, PRD 约束 10).
// Every agent-authored Artifact body is validated against its embedded
// JSON Schema before redaction and canonical serialization; unsupported
// bodies fail with SCHEMA_INVALID and nothing is persisted.
//
// The validator implements a deliberate JSON Schema (draft-07) subset:
// type, required, properties, additionalProperties, items, enum, const,
// minLength, maxLength, pattern, minimum, maximum, minItems, maxItems.
// The embedded schemas never use $ref, composition keywords, or
// conditionals, so none are implemented. Validation errors carry property
// paths but never body values, so no secret can leak into fault text.
package artifact

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"unicode/utf8"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/schemas"
)

// schemaFiles is the closed set of embedded schema names the Artifact
// Store validates against.
var schemaFiles = map[string]bool{
	"plan-envelope.json":  true,
	"spec.json":           true,
	"catalog.json":        true,
	"workflow.json":       true,
	"workflow-patch.json": true,
}

// bodySchema maps each agent-authored Artifact Type to the embedded schema
// that validates its body. report and cleanup-manifest are authored by the
// Runtime itself and carry no agent body contract.
var bodySchema = map[model.ArtifactType]string{
	model.ArtifactPlan:     "plan-envelope.json",
	model.ArtifactSpec:     "spec.json",
	model.ArtifactCatalog:  "catalog.json",
	model.ArtifactWorkflow: "workflow.json",
}

// ValidateBody validates a YAML (or JSON) body against the named embedded
// schema. It is the same validation the Artifact Store applies on Put and
// is available to the Workflow Compiler for Patch IR validation.
func ValidateBody(schemaName string, body []byte) error {
	if !schemaFiles[schemaName] {
		return model.InvalidInputFault("unknown schema name")
	}
	if len(body) == 0 {
		return schemaFault(schemaName, "body is empty")
	}
	return validateBodyJSON(schemaName, body)
}

// validateBody applies the type's embedded schema to an authored body. A
// plan body carries its contract in the YAML front matter; the other
// agent-authored bodies carry theirs in the whole document. The Spec
// Artifact additionally accepts a spec set: a non-empty YAML/JSON
// sequence whose items are Spec objects (the multi-Spec pipeline the
// Scheduler consumes, Task 12), each validated against spec.json with the
// same strict rules — the array form adds no relaxed fields.
func validateBody(typ model.ArtifactType, body []byte) error {
	name, ok := bodySchema[typ]
	if !ok {
		return nil
	}
	if typ == model.ArtifactPlan {
		front, _, err := splitFrontMatter(body)
		if err != nil {
			return err
		}
		return validateBodyJSON(name, front)
	}
	if typ == model.ArtifactSpec {
		return validateSpecBody(body)
	}
	return validateBodyJSON(name, body)
}

// validateSpecBody validates a Spec Artifact body: one Spec object, or a
// spec set whose every item satisfies spec.json.
func validateSpecBody(body []byte) error {
	value, err := yamlToValue(body)
	if err != nil {
		return schemaFault("spec.json", "body is not parseable YAML or JSON")
	}
	schema, err := loadSchema("spec.json")
	if err != nil {
		return schemaFault("spec.json", "embedded schema cannot be loaded")
	}
	switch v := value.(type) {
	case []any:
		if len(v) == 0 {
			return schemaFault("spec.json", "spec set is empty")
		}
		for i, item := range v {
			if _, ok := item.(map[string]any); !ok {
				return schemaFault("spec.json", fmt.Sprintf("spec set item [%d] is not an object", i))
			}
			if err := validateValue(item, schema, fmt.Sprintf("[%d]", i)); err != nil {
				return schemaFault("spec.json", err.Error())
			}
		}
		return nil
	case map[string]any:
		if err := validateValue(v, schema, ""); err != nil {
			return schemaFault("spec.json", err.Error())
		}
		return nil
	}
	return schemaFault("spec.json", "body must be a spec object or a non-empty spec set")
}

// validateBodyJSON parses a YAML/JSON body and validates it against the
// named embedded schema.
func validateBodyJSON(name string, body []byte) error {
	value, err := yamlToValue(body)
	if err != nil {
		return schemaFault(name, "body is not parseable YAML or JSON")
	}
	schema, err := loadSchema(name)
	if err != nil {
		return schemaFault(name, "embedded schema cannot be loaded")
	}
	if err := validateValue(value, schema, ""); err != nil {
		return schemaFault(name, err.Error())
	}
	return nil
}

// loadSchema parses one embedded schema file into its JSON value.
func loadSchema(name string) (map[string]any, error) {
	data, err := schemas.FS.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var s any
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	m, ok := s.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema %s root is not an object", name)
	}
	return m, nil
}

// schemaFault builds the fail-closed SCHEMA_INVALID fault. The message
// names the schema and the offending path but never body values.
func schemaFault(name, msg string) error {
	return model.NewFault(model.CodeSchemaInvalid,
		fmt.Sprintf("body fails schema %q: %s", name, msg))
}

// validateValue checks one value against one schema node. Path is the
// dotted property path, "" meaning the body root.
func validateValue(v any, schema any, path string) error {
	m, ok := schema.(map[string]any)
	if !ok {
		return nil // non-object schema nodes carry no constraints
	}
	loc := func() string {
		if path == "" {
			return "body"
		}
		return "body." + path
	}
	if err := checkType(v, m["type"], loc); err != nil {
		return err
	}
	if c, ok := m["const"]; ok && !deepEqual(v, c) {
		return fmt.Errorf("at %s: value does not equal the required constant", loc())
	}
	if e, ok := m["enum"].([]any); ok {
		allowed := false
		for _, item := range e {
			if deepEqual(v, item) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("at %s: value is not one of the allowed values", loc())
		}
	}
	// Numeric bounds apply to every numeric representation: YAML integer
	// scalars decode as int/int64/uint while JSON schema numbers decode as
	// float64, and all of them must hit minimum/maximum (schemas bind
	// revision, timeout, retry and budget bounds on integer values).
	if isNumber(v) {
		n := toFloat(v)
		if min, ok := number(m["minimum"]); ok && n < min {
			return fmt.Errorf("at %s: value is below the allowed minimum", loc())
		}
		if max, ok := number(m["maximum"]); ok && n > max {
			return fmt.Errorf("at %s: value is above the allowed maximum", loc())
		}
	}
	switch tv := v.(type) {
	case string:
		n := utf8.RuneCountInString(tv)
		if min, ok := number(m["minLength"]); ok && float64(n) < min {
			return fmt.Errorf("at %s: string is shorter than the allowed minimum", loc())
		}
		if max, ok := number(m["maxLength"]); ok && float64(n) > max {
			return fmt.Errorf("at %s: string is longer than the allowed maximum", loc())
		}
		if p, ok := m["pattern"].(string); ok {
			re, err := regexp.Compile(p)
			if err != nil {
				return fmt.Errorf("at %s: embedded schema carries an invalid pattern", loc())
			}
			if !re.MatchString(tv) {
				return fmt.Errorf("at %s: value does not match the required pattern", loc())
			}
		}
	}
	if arr, ok := v.([]any); ok {
		if min, ok := number(m["minItems"]); ok && float64(len(arr)) < min {
			return fmt.Errorf("at %s: array has fewer items than required", loc())
		}
		if max, ok := number(m["maxItems"]); ok && float64(len(arr)) > max {
			return fmt.Errorf("at %s: array has more items than allowed", loc())
		}
		if items, ok := m["items"]; ok {
			for i, item := range arr {
				if err := validateValue(item, items, child(path, fmt.Sprintf("[%d]", i))); err != nil {
					return err
				}
			}
		}
	}
	if obj, ok := v.(map[string]any); ok {
		if req, ok := m["required"].([]any); ok {
			for _, r := range req {
				rs, ok := r.(string)
				if !ok {
					return fmt.Errorf("at %s: embedded schema carries an invalid required list", loc())
				}
				if _, present := obj[rs]; !present {
					return fmt.Errorf("at %s: required property %q is missing", loc(), rs)
				}
			}
		}
		props, _ := m["properties"].(map[string]any)
		for key, val := range obj {
			if ps, ok := props[key]; ok {
				if err := validateValue(val, ps, child(path, key)); err != nil {
					return err
				}
			} else if add, ok := m["additionalProperties"].(bool); ok && !add {
				return fmt.Errorf("at %s: property %q is not allowed", loc(), key)
			}
		}
	}
	return nil
}

// checkType enforces the type keyword.
func checkType(v any, want any, loc func() string) error {
	t, ok := want.(string)
	if !ok {
		return nil
	}
	matches := func() bool {
		switch t {
		case "object":
			_, ok := v.(map[string]any)
			return ok
		case "array":
			_, ok := v.([]any)
			return ok
		case "string":
			_, ok := v.(string)
			return ok
		case "boolean":
			_, ok := v.(bool)
			return ok
		case "null":
			return v == nil
		case "integer":
			return isInteger(v)
		case "number":
			return isNumber(v)
		}
		return true // unknown type keyword: the embedded schemas never use it
	}
	if !matches() {
		return fmt.Errorf("at %s: value has the wrong type (expected %s)", loc(), t)
	}
	return nil
}

// isInteger reports whether v is an integer value, accepting integral
// floats per JSON Schema semantics.
func isInteger(v any) bool {
	if isNumber(v) {
		if f, ok := v.(float64); ok {
			return f == math.Trunc(f)
		}
		return true
	}
	return false
}

// isNumber reports whether v is a JSON number.
func isNumber(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return true
	}
	return false
}

// number extracts a numeric keyword value from a parsed schema (schema
// numbers always decode as float64).
func number(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// child joins a property path component.
func child(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// deepEqual compares JSON values without reflect. Numbers compare across
// int/float representations so schema constants match YAML-decoded values.
func deepEqual(a, b any) bool {
	if isNumber(a) && isNumber(b) {
		return toFloat(a) == toFloat(b)
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bval, ok := bv[k]
			if !ok || !deepEqual(v, bval) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// toFloat converts any JSON number to float64.
func toFloat(v any) float64 {
	switch t := v.(type) {
	case int:
		return float64(t)
	case int8:
		return float64(t)
	case int16:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case uint:
		return float64(t)
	case uint8:
		return float64(t)
	case uint16:
		return float64(t)
	case uint32:
		return float64(t)
	case uint64:
		return float64(t)
	case float32:
		return float64(t)
	case float64:
		return t
	}
	return 0
}
