// Request validation and body redaction for the Artifact Store (design
// 10.2, 19.2): every Put request is checked before any filesystem
// mutation, and every authored body is streamed through the Security
// Guard's Redactor before canonical serialization. Failures are
// fail-closed and never leave partial state.
package artifact

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

const (
	// maxBodyBytes bounds one authored body. The Redactor's per-frame
	// limit is 1 MiB; bodies are streamed in smaller chunks, and this
	// bound keeps a single Put bounded (design 19.2).
	maxBodyBytes = 4 << 20
	// maxFileBytes bounds one stored artifact file before it is parsed.
	maxFileBytes = 8 << 20
)

// validatePutRequest checks the caller protocol before any filesystem
// mutation.
func validatePutRequest(req PutRequest) error {
	if invalidWorkflowID(req.WorkflowID) {
		return model.InvalidInputFault("workflow id is not a safe managed path component")
	}
	if !req.Type.Valid() {
		return model.InvalidInputFault("unknown artifact type")
	}
	if req.Revision < 1 {
		return model.InvalidInputFault("artifact revision must be positive")
	}
	if req.SchemaVersion == "" {
		return model.InvalidInputFault("schema version is required")
	}
	if req.CreatedAt == "" {
		return model.InvalidInputFault("created_at is required")
	}
	if _, err := time.Parse(time.RFC3339, req.CreatedAt); err != nil {
		return model.InvalidInputFault("created_at must be an RFC 3339 timestamp")
	}
	if len(req.Body) == 0 {
		return model.InvalidInputFault("artifact body is empty")
	}
	if len(req.Body) > maxBodyBytes {
		return model.InvalidInputFault("artifact body exceeds the bounded size")
	}
	return nil
}

// validateRef checks a reference before it is turned into a path.
func validateRef(ref model.ArtifactRef) error {
	if invalidWorkflowID(ref.Workflow) {
		return model.InvalidInputFault("workflow id is not a safe managed path component")
	}
	if !ref.Type.Valid() {
		return model.InvalidInputFault("unknown artifact type")
	}
	if ref.Revision < 1 {
		return model.InvalidInputFault("artifact revision must be positive")
	}
	if !isContentFile(ref.Hash) {
		return model.InvalidInputFault("artifact hash must be 64 lowercase hex characters")
	}
	return nil
}

// invalidWorkflowID reports whether an ID is unsafe as a path component:
// IDs are opaque local identifiers and never contain path separators.
func invalidWorkflowID(id model.WorkflowID) bool {
	s := string(id)
	return s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\\x00")
}

// isContentFile reports whether a file name has the immutable content
// identity form: 64 lowercase hex characters.
func isContentFile(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, c := range name {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// revisionContentHash finds the single content file of one revision
// directory.
func revisionContentHash(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", model.InvalidInputFault("artifact revision not found")
		}
		return "", storeFault("artifact revision directory cannot be read")
	}
	found := ""
	for _, e := range entries {
		if isContentFile(e.Name()) {
			if found != "" {
				return "", storeFault("artifact revision holds multiple content files")
			}
			found = e.Name()
		}
	}
	if found == "" {
		return "", model.InvalidInputFault("artifact revision not found")
	}
	return found, nil
}

// redactBody streams the authored body through the Security Guard's
// Redactor and returns the redacted text plus the redaction facts for the
// envelope (design 10.2.2, 19.2). Any failure is fail-closed.
func redactBody(reg security.Registry, body []byte) ([]byte, redactionFacts, error) {
	red := security.NewRedactor(reg)
	const chunk = 256 << 10
	var out strings.Builder
	applied := map[string]bool{}
	revision := ""
	for len(body) > 0 {
		n := min(chunk, len(body))
		fr, err := red.WriteFrame(body[:n])
		if err != nil {
			return nil, redactionFacts{}, err
		}
		out.WriteString(fr.Text)
		for _, id := range fr.RulesApplied {
			applied[id] = true
		}
		revision = fr.RuleRevision
		body = body[n:]
	}
	fr, err := red.Flush()
	if err != nil {
		return nil, redactionFacts{}, err
	}
	out.WriteString(fr.Text)
	for _, id := range fr.RulesApplied {
		applied[id] = true
	}
	if revision == "" {
		revision = fr.RuleRevision
	}
	ids := make([]string, 0, len(applied))
	for id := range applied {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return []byte(out.String()), redactionFacts{Revision: revision, Rules: ids}, nil
}

// validateRule probe-compiles one redaction rule so a poisoned policy
// fails the Store construction instead of the first write.
func validateRule(rule security.Rule) error {
	if _, err := regexp.Compile(rule.Pattern); err != nil {
		return model.InvalidInputFault("redaction rule " + rule.ID + " failed to compile")
	}
	return nil
}
