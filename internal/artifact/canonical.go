// Canonical serialization and SHA-256 rules (design 10.1). One algorithm,
// defined here once and used by writers, readers, approvals, reports, and
// tests: the canonical content of an Artifact is a canonical JSON object
// binding schema_version, artifact_type, revision, and the canonically
// serialized body. The content_sha256 field is never part of the digest it
// describes.
//
// Body canonicalization is type-directed:
//
//   - structured Artifacts (spec, catalog, workflow, cleanup-manifest):
//     the YAML- (or JSON-) authored body is parsed and re-emitted as
//     compact canonical JSON with sorted keys and preserved numbers;
//   - plan Artifacts: the Markdown body keeps its document form — the YAML
//     front matter keys are sorted and re-emitted as canonical YAML, then
//     the Markdown part is normalized to UTF-8/LF;
//   - report Artifacts: the Markdown body is normalized to UTF-8/LF.
//
// Canonicalization is order-insensitive (reordered YAML maps, line-ending
// differences, and quoting changes produce identical bytes) and
// fail-closed on unparseable or non-UTF-8 bodies.
package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/internal/model"
)

// fileEnvelope is the persisted Artifact shape (design 10.1): the common
// envelope fields plus the canonical body. The digest covers only the
// canonical serialization of schema_version, artifact_type, revision, and
// content (the model.ArtifactEnvelope subset, with ContentHash excluded
// from its own digest); workflow_id, created_at, producer, input
// references, and redaction facts are envelope metadata that the Reader
// verifies directly against the reference and the path.
//
// Optional fields use pointers so the canonical (digest) serialization
// omits them while the stored file always carries them.
type fileEnvelope struct {
	SchemaVersion string             `json:"schema_version"`
	ArtifactType  model.ArtifactType `json:"artifact_type"`
	WorkflowID    model.WorkflowID   `json:"workflow_id,omitempty"`
	Revision      int                `json:"revision"`
	CreatedAt     string             `json:"created_at,omitempty"`
	Producer      *ProducerRef       `json:"producer,omitempty"`
	InputRefs     []InputRef         `json:"input_refs,omitempty"`
	Redaction     *redactionFacts    `json:"redaction,omitempty"`
	ContentSHA256 string             `json:"content_sha256,omitempty"`
	Content       json.RawMessage    `json:"content"`
}

// ProducerRef names the Agent Purpose and CFlow Session that produced an
// Artifact, when applicable (design 10.1).
type ProducerRef struct {
	Purpose   string `json:"purpose,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// InputRef is one input revision/hash reference an Artifact was derived
// from (design 10.1).
type InputRef struct {
	Type     model.ArtifactType `json:"artifact_type"`
	Revision int                `json:"revision"`
	Hash     string             `json:"sha256"`
}

// redactionFacts records the redaction policy revision and the rule IDs
// that fired while the Artifact body was redacted (PRD 脱敏 8).
type redactionFacts struct {
	Revision string   `json:"revision"`
	Rules    []string `json:"rules"`
}

// Canonicalize returns the canonical serialized content of one Artifact
// envelope. The ContentHash field is excluded from the serialization and
// therefore from its own digest; the output round-trips back into the
// envelope's Type, Revision, SchemaVersion, and canonical Payload.
func Canonicalize(env model.ArtifactEnvelope) ([]byte, error) {
	if !env.Type.Valid() {
		return nil, model.InvalidInputFault("unknown artifact type")
	}
	if env.Revision < 1 {
		return nil, model.InvalidInputFault("artifact revision must be positive")
	}
	if env.SchemaVersion == "" {
		return nil, model.InvalidInputFault("schema version is required")
	}
	if len(env.Payload) == 0 {
		return nil, model.InvalidInputFault("artifact content is empty")
	}
	content, err := canonicalizeBody(env.Type, env.Payload)
	if err != nil {
		return nil, err
	}
	return marshalCanonical(fileEnvelope{
		SchemaVersion: env.SchemaVersion,
		ArtifactType:  env.Type,
		Revision:      env.Revision,
		Content:       content,
	})
}

// HashCanonical returns the SHA-256 digest of canonical serialized content
// as lowercase hex. The hash is part of the Artifact identity, so a
// reference binds exact content (design 7.2).
func HashCanonical(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// marshalCanonical emits the envelope as compact canonical JSON: struct
// field order, no HTML escaping, no trailing newline.
func marshalCanonical(e fileEnvelope) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// canonicalizeBody returns the canonical content JSON value for one body,
// dispatched by Artifact Type.
func canonicalizeBody(typ model.ArtifactType, body []byte) (json.RawMessage, error) {
	var content []byte
	var err error
	switch typ {
	case model.ArtifactPlan:
		var doc []byte
		doc, err = canonicalizePlan(body)
		if err == nil {
			content, err = json.Marshal(string(doc))
		}
	case model.ArtifactReport:
		var doc []byte
		doc, err = canonicalizeMarkdown(body)
		if err == nil {
			content, err = json.Marshal(string(doc))
		}
	default:
		content, err = yamlToJSON(body)
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(content), nil
}

// canonicalizePlan canonicalizes a plan document: sorted YAML front matter
// followed by the normalized Markdown body.
func canonicalizePlan(body []byte) ([]byte, error) {
	normalized, err := normalizeMarkdown(body)
	if err != nil {
		return nil, err
	}
	front, rest, err := splitFrontMatter(normalized)
	if err != nil {
		return nil, err
	}
	value, err := yamlToValue(front)
	if err != nil {
		return nil, schemaFault("plan-envelope.json", "front matter is not parseable YAML")
	}
	// yaml.Marshal emits map keys in sorted order and ends with a newline,
	// so reordered front matter keys canonicalize to the same bytes.
	canonicalFront, err := yaml.Marshal(value)
	if err != nil {
		return nil, schemaFault("plan-envelope.json", "front matter cannot be canonically serialized")
	}
	doc := make([]byte, 0, len(canonicalFront)+len(rest)+4)
	doc = append(doc, "---\n"...)
	doc = append(doc, canonicalFront...)
	doc = append(doc, "---\n"...)
	doc = append(doc, rest...)
	return doc, nil
}

// canonicalizeMarkdown normalizes a Markdown body to UTF-8/LF.
func canonicalizeMarkdown(body []byte) ([]byte, error) {
	return normalizeMarkdown(body)
}

// normalizeMarkdown strips a UTF-8 BOM, rejects invalid UTF-8 and NUL
// bytes, and normalizes CRLF and CR line endings to LF.
func normalizeMarkdown(body []byte) ([]byte, error) {
	if !utf8.Valid(body) {
		return nil, schemaFault("markdown", "artifact body is not valid UTF-8")
	}
	if bytes.IndexByte(body, 0) >= 0 {
		return nil, schemaFault("markdown", "artifact body contains NUL bytes")
	}
	body = bytes.TrimPrefix(body, []byte("\xEF\xBB\xBF"))
	body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	body = bytes.ReplaceAll(body, []byte("\r"), []byte("\n"))
	return body, nil
}

// splitFrontMatter splits a body whose first line is "---" at its closing
// "---" (or "...") marker, returning the YAML front matter and the rest of
// the document. A missing or unterminated block fails closed.
func splitFrontMatter(body []byte) (front, rest []byte, err error) {
	if !bytes.HasPrefix(body, []byte("---\n")) {
		return nil, nil, schemaFault("plan-envelope.json", "plan artifact body must begin with YAML front matter")
	}
	end := -1
	if idx := bytes.Index(body[4:], []byte("\n---\n")); idx >= 0 {
		end = 4 + idx
	} else if idx := bytes.Index(body[4:], []byte("\n...\n")); idx >= 0 {
		end = 4 + idx
	}
	if end < 0 {
		return nil, nil, schemaFault("plan-envelope.json", "plan artifact front matter is not terminated")
	}
	return body[4:end], body[end+5:], nil
}

// yamlToJSON parses a YAML (or JSON) body and re-emits it as compact
// canonical JSON with sorted keys and preserved numbers.
func yamlToJSON(body []byte) ([]byte, error) {
	value, err := yamlToValue(body)
	if err != nil {
		return nil, schemaFault("structured", "artifact body is not parseable YAML or JSON")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, schemaFault("structured", "artifact body cannot be canonically serialized")
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// yamlToValue parses a YAML document into plain Go values with
// deterministic semantics: mapping keys must be strings, duplicate keys
// are rejected, and scalar values resolve by tag.
func yamlToValue(body []byte) (any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	return nodeToValue(&doc)
}

// nodeToValue converts one YAML node tree into plain Go values.
func nodeToValue(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, errors.New("empty YAML document")
		}
		return nodeToValue(n.Content[0])
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Tag != "!!str" {
				return nil, errors.New("mapping key is not a string")
			}
			if _, dup := m[k.Value]; dup {
				return nil, fmt.Errorf("duplicate mapping key %q", k.Value)
			}
			val, err := nodeToValue(v)
			if err != nil {
				return nil, err
			}
			m[k.Value] = val
		}
		return m, nil
	case yaml.SequenceNode:
		seq := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			val, err := nodeToValue(c)
			if err != nil {
				return nil, err
			}
			seq = append(seq, val)
		}
		return seq, nil
	case yaml.ScalarNode:
		var out any
		if err := n.Decode(&out); err != nil {
			return nil, err
		}
		return out, nil
	default:
		return nil, errors.New("unsupported YAML node kind")
	}
}
