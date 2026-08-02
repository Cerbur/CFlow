package artifact

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func requireError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

// requireFaultCode asserts err is a model.Fault carrying exactly code.
func requireFaultCode(t *testing.T, err error, code model.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected fault %s, got nil", code)
	}
	got, ok := model.CodeOf(err)
	if !ok {
		t.Fatalf("expected fault %s, got non-fault error %v", code, err)
	}
	if got != code {
		t.Fatalf("expected fault %s, got %s (%v)", code, got, err)
	}
}

// tempRoot returns a temporary directory whose full path is free of
// symlinks and whose mode is owner-only, presenting the CFLOW_HOME
// posture the Security Guard requires. On macOS, os.TempDir() lives under
// /var/folders and /var is a symlink to /private/var, so the guard would
// reject the raw path; on Go 1.26 the testing package also creates
// t.TempDir() as 0755, which the guard rejects, so the fixture chmods the
// resolved root to 0700 (the guard reports, never repairs).
func tempRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	if err := os.Chmod(p, 0o700); err != nil {
		t.Fatalf("chmod temp root: %v", err)
	}
	return p
}

// testRedactionRegistry builds the redaction policy the test store is
// constructed with: a provider-token rule and an API key rule.
func testRedactionRegistry() security.Registry {
	return security.Registry{
		Revision: "test-1",
		Rules: []security.Rule{
			{ID: "provider-token", Category: "provider_token", Pattern: `sk-[A-Za-z0-9]{16,}`},
			{ID: "api-key", Category: "api_key", Pattern: `AKIA[0-9A-Z]{16}`},
		},
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(tempRoot(t), testRedactionRegistry())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func injectFailure(t *testing.T, s *Store, p FaultPoint) {
	t.Helper()
	if s.inject == nil {
		s.inject = map[FaultPoint]struct{}{}
	}
	s.inject[p] = struct{}{}
}

// fixtureArtifact returns the canonical plan envelope the golden and
// round-trip tests are pinned to: a plan Markdown body with YAML front
// matter whose keys are deliberately NOT sorted.
func fixtureArtifact() model.ArtifactEnvelope {
	return model.ArtifactEnvelope{
		Type:          model.ArtifactPlan,
		Revision:      1,
		SchemaVersion: "1.0.0",
		ContentHash:   "",
		Payload: []byte("---\n" +
			"title: Fix login bug\n" +
			"workflow_id: wf-1\n" +
			"revision: 1\n" +
			"---\n" +
			"\n" +
			"# Fix login bug\n"),
	}
}

// fixturePlanBody builds a schema-valid plan body (front matter plus
// Markdown) for the given title.
func fixturePlanBody(wf, title string, revision int) []byte {
	return []byte("---\n" +
		"title: " + title + "\n" +
		"workflow_id: " + wf + "\n" +
		"revision: " + strconv.Itoa(revision) + "\n" +
		"---\n" +
		"\n" +
		"# " + title + "\n")
}

func fixturePlanRequest(wf, title string, revision int) PutRequest {
	return PutRequest{
		WorkflowID:    model.WorkflowID(wf),
		Type:          model.ArtifactPlan,
		Revision:      revision,
		SchemaVersion: "1.0.0",
		CreatedAt:     "2026-08-03T00:00:00Z",
		Body:          fixturePlanBody(wf, title, revision),
	}
}

// expectedHash computes the digest Put will assign to a request whose body
// contains no secrets (redaction is then the identity).
func expectedHash(t *testing.T, req PutRequest) string {
	t.Helper()
	canonical, err := Canonicalize(model.ArtifactEnvelope{
		Type:          req.Type,
		Revision:      req.Revision,
		SchemaVersion: req.SchemaVersion,
		Payload:       req.Body,
	})
	requireNoError(t, err)
	return HashCanonical(canonical)
}

func artifactPath(t *testing.T, s *Store, ref model.ArtifactRef) string {
	t.Helper()
	return filepath.Join(s.root, string(ref.Workflow), string(ref.Type),
		strconv.Itoa(ref.Revision), ref.Hash)
}

func mustPut(t *testing.T, s *Store, req PutRequest) model.ArtifactRef {
	t.Helper()
	ref, err := s.Put(context.Background(), req)
	requireNoError(t, err)
	return ref
}

func mustGet(t *testing.T, s *Store, ref model.ArtifactRef) []byte {
	t.Helper()
	body, err := s.Get(context.Background(), ref)
	requireNoError(t, err)
	return body
}

// ---------------------------------------------------------------------------
// Canonical serialization (brief Step 1: golden canonicalization)
// ---------------------------------------------------------------------------

// TestCanonicalHashIgnoresEnvelopeHashField is the brief's mandated test.
// The model envelope (Task 2, fixed shape) names the field ContentHash;
// the brief's draft spelled it ContentSHA256, which cannot compile against
// the fixed model type, so the field access uses the model's name while
// the test body is verbatim.
func TestCanonicalHashIgnoresEnvelopeHashField(t *testing.T) {
	a := fixtureArtifact()
	a.ContentHash = "different"
	first, err := Canonicalize(a)
	requireNoError(t, err)
	a.ContentHash = ""
	second, err := Canonicalize(a)
	requireNoError(t, err)
	got, want := HashCanonical(first), HashCanonical(second)
	if got != want {
		t.Fatalf("%s != %s", got, want)
	}
}

// TestCanonicalGoldenPlanBytes pins the canonical serialization algorithm
// for a plan body (YAML front matter + Markdown): front matter keys are
// sorted, line endings are LF, and the digest wrapper binds
// schema_version, artifact_type, revision and the canonical content while
// excluding content_sha256 entirely.
func TestCanonicalGoldenPlanBytes(t *testing.T) {
	got, err := Canonicalize(fixtureArtifact())
	requireNoError(t, err)
	want := `{"schema_version":"1.0.0","artifact_type":"plan","revision":1,` +
		`"content":"---\nrevision: 1\ntitle: Fix login bug\nworkflow_id: wf-1\n---\n\n# Fix login bug\n"}`
	if string(got) != want {
		t.Fatalf("golden canonical bytes mismatch\n got: %s\nwant: %s", got, want)
	}
	hash := HashCanonical(got)
	if len(hash) != 64 {
		t.Fatalf("hash is not 64 hex characters: %q", hash)
	}
	for _, c := range hash {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("hash is not lowercase hex: %q", hash)
		}
	}
}

// TestCanonicalGoldenSpecBytes pins canonical serialization of a
// structured (YAML-authored) body: canonical JSON with sorted keys and
// preserved numbers.
func TestCanonicalGoldenSpecBytes(t *testing.T) {
	payload := []byte("id: spec-1\n" +
		"goal: Fix the login bug\n" +
		"depends_on: []\n" +
		"write_scope: [src/login]\n" +
		"acceptance:\n" +
		"  verification_command_ids: [cmd-1]\n")
	got, err := Canonicalize(model.ArtifactEnvelope{
		Type:          model.ArtifactSpec,
		Revision:      1,
		SchemaVersion: "1.0.0",
		Payload:       payload,
	})
	requireNoError(t, err)
	want := `{"schema_version":"1.0.0","artifact_type":"spec","revision":1,` +
		`"content":{"acceptance":{"verification_command_ids":["cmd-1"]},` +
		`"depends_on":[],"goal":"Fix the login bug","id":"spec-1","write_scope":["src/login"]}}`
	if string(got) != want {
		t.Fatalf("golden canonical bytes mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestCanonicalReorderedYAMLMaps: reordering the YAML front matter keys
// (including quoting changes) yields byte-identical canonical content.
func TestCanonicalReorderedYAMLMaps(t *testing.T) {
	reordered := []byte("---\n" +
		"revision: 1\n" +
		"workflow_id: wf-1\n" +
		"title: \"Fix login bug\"\n" +
		"---\n" +
		"\n" +
		"# Fix login bug\n")
	first, err := Canonicalize(fixtureArtifact())
	requireNoError(t, err)
	second, err := Canonicalize(model.ArtifactEnvelope{
		Type:          model.ArtifactPlan,
		Revision:      1,
		SchemaVersion: "1.0.0",
		Payload:       reordered,
	})
	requireNoError(t, err)
	if string(first) != string(second) {
		t.Fatalf("reordered YAML maps changed canonical bytes\n got: %s\nwant: %s", second, first)
	}
}

// TestCanonicalLineEndings: CRLF and CR line endings normalize to LF, and
// a UTF-8 BOM is stripped, so the canonical bytes are identical.
func TestCanonicalLineEndings(t *testing.T) {
	lf := fixtureArtifact()
	crlf := fixtureArtifact()
	crlf.Payload = []byte(strings.ReplaceAll(string(crlf.Payload), "\n", "\r\n"))
	cr := fixtureArtifact()
	cr.Payload = []byte(strings.ReplaceAll(string(cr.Payload), "\n", "\r"))
	bom := fixtureArtifact()
	bom.Payload = append([]byte("\xEF\xBB\xBF"), bom.Payload...)

	base, err := Canonicalize(lf)
	requireNoError(t, err)
	for name, other := range map[string]model.ArtifactEnvelope{"crlf": crlf, "cr": cr, "bom": bom} {
		got, err := Canonicalize(other)
		requireNoError(t, err)
		if string(got) != string(base) {
			t.Fatalf("%s line endings changed canonical bytes", name)
		}
	}
}

// TestCanonicalRoundTrip: canonical bytes decode back to the same
// envelope fields (type, revision, schema version, payload); the content
// hash field is never serialized.
func TestCanonicalRoundTrip(t *testing.T) {
	canonical, err := Canonicalize(fixtureArtifact())
	requireNoError(t, err)
	var fe fileEnvelope
	if err := json.Unmarshal(canonical, &fe); err != nil {
		t.Fatalf("canonical bytes do not parse: %v", err)
	}
	if fe.ArtifactType != model.ArtifactPlan || fe.Revision != 1 || fe.SchemaVersion != "1.0.0" {
		t.Fatalf("round trip lost envelope fields: %+v", fe)
	}
	if fe.ContentSHA256 != "" || fe.WorkflowID != "" || fe.CreatedAt != "" {
		t.Fatalf("canonical bytes must exclude the hash and store metadata: %+v", fe)
	}
	payload, err := canonicalizeBody(model.ArtifactPlan, fixtureArtifact().Payload)
	requireNoError(t, err)
	if string(fe.Content) != string(payload) {
		t.Fatalf("round trip changed the payload\n got: %s\nwant: %s", fe.Content, payload)
	}
}

// TestCanonicalRejectsInvalidBodies: unparseable or non-UTF-8 bodies fail
// closed instead of producing ambiguous bytes.
func TestCanonicalRejectsInvalidBodies(t *testing.T) {
	cases := []struct {
		name string
		env  model.ArtifactEnvelope
		code model.Code
	}{
		{"invalid-yaml", model.ArtifactEnvelope{Type: model.ArtifactSpec, Revision: 1, SchemaVersion: "1.0.0", Payload: []byte(`{"a": `)}, model.CodeSchemaInvalid},
		{"duplicate-yaml-keys", model.ArtifactEnvelope{Type: model.ArtifactSpec, Revision: 1, SchemaVersion: "1.0.0", Payload: []byte("a: 1\na: 2\n")}, model.CodeSchemaInvalid},
		{"invalid-utf8-plan", model.ArtifactEnvelope{Type: model.ArtifactPlan, Revision: 1, SchemaVersion: "1.0.0", Payload: []byte("---\ntitle: X\n---\n\n# \xffX\n")}, model.CodeSchemaInvalid},
		{"invalid-utf8-report", model.ArtifactEnvelope{Type: model.ArtifactReport, Revision: 1, SchemaVersion: "1.0.0", Payload: []byte("# \xffX\n")}, model.CodeSchemaInvalid},
		{"empty-plan", model.ArtifactEnvelope{Type: model.ArtifactPlan, Revision: 1, SchemaVersion: "1.0.0", Payload: []byte("")}, model.CodeInvalidInput},
		{"unknown-type", model.ArtifactEnvelope{Type: model.ArtifactType("banana"), Revision: 1, SchemaVersion: "1.0.0", Payload: []byte("x")}, model.CodeInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Canonicalize(tc.env)
			requireFaultCode(t, err, tc.code)
		})
	}
}

// ---------------------------------------------------------------------------
// Atomic write protocol (brief Step 1: atomicity and identity tests)
// ---------------------------------------------------------------------------

// TestAtomicWriteInterruptedRename: a rename that never completes leaves
// no target file and no partial content; the store stays usable.
func TestAtomicWriteInterruptedRename(t *testing.T) {
	s := newTestStore(t)
	req := fixturePlanRequest("wf-1", "Fix login bug", 1)
	hash := expectedHash(t, req)
	ref := model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: hash}

	injectFailure(t, s, FailBeforeRename)
	_, err := s.Put(context.Background(), req)
	if err == nil || err.Error() != errInjected.Error() {
		t.Fatalf("expected injected failure, got %v", err)
	}

	target := artifactPath(t, s, ref)
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Fatalf("interrupted rename left a target file: %v", statErr)
	}
	dir := filepath.Dir(target)
	entries, err := os.ReadDir(dir)
	requireNoError(t, err)
	for _, e := range entries {
		if isContentFile(e.Name()) {
			t.Fatalf("interrupted rename left content file %s", e.Name())
		}
	}
	if _, err := s.Get(context.Background(), ref); err == nil {
		t.Fatal("interrupted rename must not be readable")
	}

	// The store recovers: a retry through the recorded intent succeeds.
	delete(s.inject, FailBeforeRename)
	got, err := s.Put(context.Background(), req)
	requireNoError(t, err)
	if got != ref {
		t.Fatalf("retry ref mismatch: got %v want %v", got, ref)
	}
}

// TestAtomicWriteFailureAfterRename: a failure after the rename but before
// verification removes the unverified target; the store stays usable.
func TestAtomicWriteFailureAfterRename(t *testing.T) {
	s := newTestStore(t)
	req := fixturePlanRequest("wf-1", "Fix login bug", 1)
	hash := expectedHash(t, req)
	ref := model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: hash}

	injectFailure(t, s, FailAfterRename)
	_, err := s.Put(context.Background(), req)
	if err == nil || err.Error() != errInjected.Error() {
		t.Fatalf("expected injected failure, got %v", err)
	}
	if _, statErr := os.Lstat(artifactPath(t, s, ref)); !os.IsNotExist(statErr) {
		t.Fatalf("failed write left an unverified target: %v", statErr)
	}
	delete(s.inject, FailAfterRename)
	got, err := s.Put(context.Background(), req)
	requireNoError(t, err)
	if got != ref {
		t.Fatalf("retry ref mismatch: got %v want %v", got, ref)
	}
}

// TestAtomicExistingIdenticalContentRejected: an existing target path is
// rejected even when the content appears equal; idempotency is resolved
// through recorded intent, never by overwriting.
func TestAtomicExistingIdenticalContentRejected(t *testing.T) {
	s := newTestStore(t)
	req := fixturePlanRequest("wf-1", "Fix login bug", 1)
	ref := mustPut(t, s, req)

	_, err := s.Put(context.Background(), req)
	requireFaultCode(t, err, model.CodeInvalidInput)

	body := mustGet(t, s, ref)
	if !strings.Contains(string(body), "Fix login bug") {
		t.Fatalf("original artifact changed: %s", body)
	}
}

// TestAtomicExistingConflictingContentRejected: the same revision with
// different content is a revision conflict and never overwrites the
// stored artifact.
func TestAtomicExistingConflictingContentRejected(t *testing.T) {
	s := newTestStore(t)
	req := fixturePlanRequest("wf-1", "Fix login bug", 1)
	ref := mustPut(t, s, req)

	conflict := req
	conflict.Body = fixturePlanBody("wf-1", "Fix logout bug", 1)
	_, err := s.Put(context.Background(), conflict)
	requireFaultCode(t, err, model.CodeInvalidInput)

	body := mustGet(t, s, ref)
	if strings.Contains(string(body), "logout") {
		t.Fatalf("conflicting write overwrote the artifact: %s", body)
	}
}

// TestAtomicFileBorn0600: the artifact file is born 0600, every managed
// directory is born 0700, and no temporary file survives a successful
// write.
func TestAtomicFileBorn0600(t *testing.T) {
	s := newTestStore(t)
	ref := mustPut(t, s, fixturePlanRequest("wf-1", "Fix login bug", 1))

	info, err := os.Stat(artifactPath(t, s, ref))
	requireNoError(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact file mode = %o, want 600", info.Mode().Perm())
	}
	for _, dir := range []string{
		filepath.Join(s.root, "wf-1"),
		filepath.Join(s.root, "wf-1", "plan"),
		filepath.Join(s.root, "wf-1", "plan", "1"),
	} {
		dinfo, err := os.Stat(dir)
		requireNoError(t, err)
		if dinfo.Mode().Perm() != 0o700 {
			t.Fatalf("managed dir %s mode = %o, want 700", dir, dinfo.Mode().Perm())
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "wf-1", "plan", "1"))
	requireNoError(t, err)
	if len(entries) != 1 || entries[0].Name() != ref.Hash {
		t.Fatalf("revision dir holds unexpected entries: %v", entries)
	}
}

// TestPutGetRoundTrip: Put returns the immutable reference and Get returns
// the canonical content, stable across independent stores over the same
// root.
func TestPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	req := fixturePlanRequest("wf-1", "Fix login bug", 1)
	ref := mustPut(t, s, req)
	if ref.Workflow != "wf-1" || ref.Type != model.ArtifactPlan || ref.Revision != 1 {
		t.Fatalf("unexpected ref: %v", ref)
	}
	if ref.Hash != expectedHash(t, req) {
		t.Fatalf("ref hash %s does not match the canonical digest %s", ref.Hash, expectedHash(t, req))
	}

	// Reopen through a fresh store over the same root: bytes are stable.
	s2, err := New(s.root, testRedactionRegistry())
	requireNoError(t, err)
	body := mustGet(t, s2, ref)
	if !strings.Contains(string(body), "# Fix login bug") {
		t.Fatalf("unexpected payload: %s", body)
	}
}

// TestHistoricalVersionsRemainReadable: older revisions are never touched
// by later writes (design 10.3).
func TestHistoricalVersionsRemainReadable(t *testing.T) {
	s := newTestStore(t)
	req1 := fixturePlanRequest("wf-1", "Fix login bug", 1)
	ref1 := mustPut(t, s, req1)
	req2 := fixturePlanRequest("wf-1", "Fix login bug", 2)
	ref2 := mustPut(t, s, req2)

	b1 := mustGet(t, s, ref1)
	b2 := mustGet(t, s, ref2)
	if string(b1) == string(b2) {
		t.Fatal("revisions must differ")
	}
	if !strings.Contains(string(b2), "# Fix login bug") {
		t.Fatalf("unexpected payload: %s", b2)
	}
}

// TestPutRejectsUnsupportedSchemaVersion: a writer cannot create an
// artifact this binary cannot read back (design 10.3).
func TestPutRejectsUnsupportedSchemaVersion(t *testing.T) {
	s := newTestStore(t)
	req := fixturePlanRequest("wf-1", "Fix login bug", 1)
	req.SchemaVersion = "99.0.0"
	_, err := s.Put(context.Background(), req)
	requireFaultCode(t, err, model.CodeArtifactSchemaUnsupported)

	if _, statErr := os.Lstat(filepath.Join(s.root, "wf-1")); !os.IsNotExist(statErr) {
		t.Fatal("unsupported schema version must not touch the filesystem")
	}
}

// TestGetSchemaTooNew: a stored artifact whose schema version this binary
// cannot read fails with ARTIFACT_SCHEMA_UNSUPPORTED before the body is
// deserialized — the file's body is deliberately unverifiable.
func TestGetSchemaTooNew(t *testing.T) {
	s := newTestStore(t)
	dir := filepath.Join(s.root, "wf-1", "plan", "1")
	requireNoError(t, os.MkdirAll(dir, 0o700))
	hash := strings.Repeat("a", 64)
	fe := fileEnvelope{
		SchemaVersion: "99.0.0",
		ArtifactType:  model.ArtifactPlan,
		WorkflowID:    "wf-1",
		Revision:      1,
		CreatedAt:     "2026-08-03T00:00:00Z",
		ContentSHA256: "not-a-real-hash",
		Content:       json.RawMessage(`"garbage"`),
	}
	data, err := json.Marshal(fe)
	requireNoError(t, err)
	requireNoError(t, os.WriteFile(filepath.Join(dir, hash), data, 0o600))

	ref := model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: hash}
	_, err = s.Get(context.Background(), ref)
	requireFaultCode(t, err, model.CodeArtifactSchemaUnsupported)
}

// TestGetDetectsTamperedContent: modifying the stored content (while
// keeping a structurally valid file) fails the content hash verification.
func TestGetDetectsTamperedContent(t *testing.T) {
	s := newTestStore(t)
	ref := mustPut(t, s, fixturePlanRequest("wf-1", "Fix login bug", 1))
	path := artifactPath(t, s, ref)

	data, err := os.ReadFile(path)
	requireNoError(t, err)
	var fe fileEnvelope
	requireNoError(t, json.Unmarshal(data, &fe))
	fe.Content = json.RawMessage(`"tampered"`)
	tampered, err := json.Marshal(fe)
	requireNoError(t, err)
	requireNoError(t, os.WriteFile(path, tampered, 0o600))

	_, err = s.Get(context.Background(), ref)
	requireError(t, err)
	if _, ok := model.CodeOf(err); !ok {
		t.Fatalf("expected a fault, got %v", err)
	}

	// Trailing bytes are not canonical either.
	requireNoError(t, os.WriteFile(path, append(data, '\n'), 0o600))
	_, err = s.Get(context.Background(), ref)
	requireError(t, err)
}

// TestGetRejectsEnvelopeMismatch: a file whose envelope claims a
// different workflow than its path and reference fails verification.
func TestGetRejectsEnvelopeMismatch(t *testing.T) {
	s := newTestStore(t)
	ref := mustPut(t, s, fixturePlanRequest("wf-1", "Fix login bug", 1))
	path := artifactPath(t, s, ref)

	data, err := os.ReadFile(path)
	requireNoError(t, err)
	var fe fileEnvelope
	requireNoError(t, json.Unmarshal(data, &fe))
	fe.WorkflowID = "wf-2"
	rewritten, err := json.Marshal(fe)
	requireNoError(t, err)
	requireNoError(t, os.WriteFile(path, rewritten, 0o600))

	_, err = s.Get(context.Background(), ref)
	requireError(t, err)
}

// TestGetRejectsGroupWritableMode: a file with group or other permission
// bits is never read (owner, mode, canonical path are verified on every
// Get).
func TestGetRejectsGroupWritableMode(t *testing.T) {
	s := newTestStore(t)
	ref := mustPut(t, s, fixturePlanRequest("wf-1", "Fix login bug", 1))
	path := artifactPath(t, s, ref)

	requireNoError(t, os.Chmod(path, 0o644))
	_, err := s.Get(context.Background(), ref)
	requireFaultCode(t, err, model.CodeInsecureCFLOWHomePermissions)
}

// TestGetMissingArtifact: reads of artifacts that are not present fail
// with a not-found input fault.
func TestGetMissingArtifact(t *testing.T) {
	s := newTestStore(t)
	ref := model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: strings.Repeat("b", 64)}
	_, err := s.Get(context.Background(), ref)
	requireFaultCode(t, err, model.CodeInvalidInput)

	bad := model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: "zz"}
	_, err = s.Get(context.Background(), bad)
	requireFaultCode(t, err, model.CodeInvalidInput)
}

// TestPutValidatesBodyAgainstSchema: every agent-authored body is
// validated against its embedded schema before anything is written.
func TestPutValidatesBodyAgainstSchema(t *testing.T) {
	s := newTestStore(t)

	// A spec without the PRD-mandated write_scope is rejected.
	spec := fixturePlanRequest("wf-1", "x", 1)
	spec.Type = model.ArtifactSpec
	spec.Body = []byte("id: spec-1\ngoal: x\nacceptance:\n  verification_command_ids: [cmd-1]\n")
	_, err := s.Put(context.Background(), spec)
	requireFaultCode(t, err, model.CodeSchemaInvalid)

	// A plan without YAML front matter is rejected.
	noFront := fixturePlanRequest("wf-1", "Fix login bug", 1)
	noFront.Body = []byte("# Fix login bug\n")
	_, err = s.Put(context.Background(), noFront)
	requireFaultCode(t, err, model.CodeSchemaInvalid)

	// A plan whose front matter misses required fields is rejected.
	badFront := fixturePlanRequest("wf-1", "Fix login bug", 1)
	badFront.Body = []byte("---\nworkflow_id: wf-1\nrevision: 1\n---\n\n# Fix login bug\n")
	_, err = s.Put(context.Background(), badFront)
	requireFaultCode(t, err, model.CodeSchemaInvalid)

	// Numeric bounds are enforced on integer scalars: a spec timeout or
	// retry budget below its schema minimum must fail.
	numericBodies := map[string]model.ArtifactType{
		"spec-timeout-zero":     model.ArtifactSpec,
		"spec-retry-negative":   model.ArtifactSpec,
		"catalog-revision-zero": model.ArtifactCatalog,
		"workflow-timeout-zero": model.ArtifactWorkflow,
	}
	reqNumeric := fixturePlanRequest("wf-1", "x", 1)
	for name, typ := range numericBodies {
		reqNumeric.Type = typ
		reqNumeric.Body = numericViolationBody(typ)
		t.Run(name, func(t *testing.T) {
			_, err := s.Put(context.Background(), reqNumeric)
			requireFaultCode(t, err, model.CodeSchemaInvalid)
		})
	}

	// A valid spec body passes.
	validSpec := fixturePlanRequest("wf-1", "x", 1)
	validSpec.Type = model.ArtifactSpec
	validSpec.Body = []byte("id: spec-1\ngoal: x\ndepends_on: []\nwrite_scope: [src/login]\nacceptance:\n  verification_command_ids: [cmd-1]\n")
	ref, err := s.Put(context.Background(), validSpec)
	requireNoError(t, err)
	if ref.Type != model.ArtifactSpec {
		t.Fatalf("unexpected ref: %v", ref)
	}

	// Bodies at or above the schema minimums pass.
	reqBounds := fixturePlanRequest("wf-1", "x", 2)
	reqBounds.Type = model.ArtifactSpec
	reqBounds.Body = []byte("id: spec-1\ngoal: x\ndepends_on: []\nwrite_scope: [src/login]\n" +
		"acceptance:\n  verification_command_ids: [cmd-1]\ntimeout_seconds: 5\nmax_retry: 0\n")
	_, err = s.Put(context.Background(), reqBounds)
	requireNoError(t, err)
}

// numericViolationBody returns a body for typ whose integer value violates
// the schema's minimum bound.
func numericViolationBody(typ model.ArtifactType) []byte {
	switch typ {
	case model.ArtifactSpec:
		return []byte("id: spec-1\ngoal: x\ndepends_on: []\nwrite_scope: [src/login]\n" +
			"acceptance:\n  verification_command_ids: [cmd-1]\ntimeout_seconds: 0\nmax_retry: -5\n")
	case model.ArtifactCatalog:
		return []byte("revision: 0\nentries:\n" +
			"  - command_id: cmd-1\n    executable: /usr/bin/true\n    args: []\n" +
			"    cwd: /repo\n    purpose: task_verify\n    timeout_seconds: 60\n" +
			"    expected_exit_codes: [0]\n    max_output_bytes: 1048576\n    source: fixture\n")
	case model.ArtifactWorkflow:
		return []byte("schema: cflow-workflow-1\nworkflow_id: wf-1\nrevision: 1\nnodes:\n" +
			"  - id: agent-1\n    type: agent_task\n    spec_id: spec-1\n    timeout_seconds: 0\nedges: []\n")
	}
	return nil
}

// TestPutRedactsBeforePersisting: secrets in the body are replaced by
// stable placeholders before canonical serialization, and the raw value
// never reaches the filesystem or the reference hash. The body is longer
// than the Redactor's 4096-byte withholding window so the redacted text
// keeps its stream order (a short body with a trailing secret fails the
// canonical parse closed instead).
func TestPutRedactsBeforePersisting(t *testing.T) {
	s := newTestStore(t)
	const token = "sk-abcdefghijklmnop123456"
	req := fixturePlanRequest("wf-1", "Fix login bug", 1)
	req.Body = []byte("---\ntitle: Fix login bug\nworkflow_id: wf-1\nrevision: 1\n---\n\n" +
		"# Fix login bug\n\nCredential: " + token + "\n\n" +
		strings.Repeat("padding line keeping the body beyond the redaction window\n", 320))
	ref := mustPut(t, s, req)

	raw, err := os.ReadFile(artifactPath(t, s, ref))
	requireNoError(t, err)
	if strings.Contains(string(raw), token) {
		t.Fatal("raw secret reached the artifact file")
	}
	if !strings.Contains(string(raw), "[REDACTED:provider_token]") {
		t.Fatalf("redaction placeholder missing from artifact file: %s", raw)
	}
	body := mustGet(t, s, ref)
	if !strings.Contains(string(body), "[REDACTED:provider_token]") {
		t.Fatalf("stored payload is not redacted: %s", body)
	}
	if ref.Hash == expectedHash(t, req) {
		t.Fatal("reference must hash the redacted content, not the raw body")
	}
}

// TestResolveExactAndLatest: Resolve binds (workflow, type, revision) to
// the stored content hash; revision 0 resolves the latest revision
// present on disk.
func TestResolveExactAndLatest(t *testing.T) {
	s := newTestStore(t)
	ref1 := mustPut(t, s, fixturePlanRequest("wf-1", "Fix login bug", 1))
	ref2 := mustPut(t, s, fixturePlanRequest("wf-1", "Fix login bug", 2))

	for _, tc := range []struct {
		revision int
		want     model.ArtifactRef
	}{
		{1, ref1},
		{2, ref2},
		{0, ref2},
	} {
		got, err := s.Resolve(context.Background(), ResolveRequest{
			WorkflowID: "wf-1",
			Type:       model.ArtifactPlan,
			Revision:   tc.revision,
		})
		requireNoError(t, err)
		if got != tc.want {
			t.Fatalf("resolve revision %d: got %v want %v", tc.revision, got, tc.want)
		}
	}

	_, err := s.Resolve(context.Background(), ResolveRequest{
		WorkflowID: "wf-1",
		Type:       model.ArtifactPlan,
		Revision:   3,
	})
	requireFaultCode(t, err, model.CodeInvalidInput)
}

// TestPutRejectsInvalidRequests: malformed requests fail before any
// filesystem mutation.
func TestPutRejectsInvalidRequests(t *testing.T) {
	s := newTestStore(t)
	base := fixturePlanRequest("wf-1", "Fix login bug", 1)
	cases := map[string]PutRequest{
		"empty-workflow":  with(base, func(r *PutRequest) { r.WorkflowID = "" }),
		"workflow-slash":  with(base, func(r *PutRequest) { r.WorkflowID = "wf/1" }),
		"workflow-dotdot": with(base, func(r *PutRequest) { r.WorkflowID = ".." }),
		"unknown-type":    with(base, func(r *PutRequest) { r.Type = model.ArtifactType("banana") }),
		"revision-zero":   with(base, func(r *PutRequest) { r.Revision = 0 }),
		"empty-schema":    with(base, func(r *PutRequest) { r.SchemaVersion = "" }),
		"bad-created-at":  with(base, func(r *PutRequest) { r.CreatedAt = "yesterday" }),
		"empty-body":      with(base, func(r *PutRequest) { r.Body = nil }),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.Put(context.Background(), req)
			requireFaultCode(t, err, model.CodeInvalidInput)
		})
	}
}

func with(base PutRequest, mutate func(*PutRequest)) PutRequest {
	r := base
	mutate(&r)
	return r
}

// TestValidateBodyWorkflowPatch: the scheduling Patch schema is exposed
// for the Compiler; unknown schemas and invalid bodies fail closed.
func TestValidateBodyWorkflowPatch(t *testing.T) {
	ok := []byte("schema: cflow-workflow-patch-1\noperations:\n  - op: tighten_budget\n    node_id: verify-1\n    budget: 5\n")
	requireNoError(t, ValidateBody("workflow-patch.json", ok))

	bad := []byte("schema: cflow-workflow-patch-1\noperations:\n  - op: delete_acceptance\n")
	err := ValidateBody("workflow-patch.json", bad)
	requireFaultCode(t, err, model.CodeSchemaInvalid)

	err = ValidateBody("no-such-schema.json", ok)
	requireFaultCode(t, err, model.CodeInvalidInput)
}
