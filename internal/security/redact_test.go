package security_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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

// testRegistry builds a one-rule registry whose rule declares no
// category, so the redactor emits the untyped "[REDACTED]" placeholder
// that the brief's mandated test asserts on.
func testRegistry(pattern string) security.Registry {
	return security.Registry{
		Revision: "test-1",
		Rules: []security.Rule{{
			ID:      "test-rule-1",
			Pattern: pattern,
		}},
	}
}

func registry(rules ...security.Rule) security.Registry {
	return security.Registry{Revision: "test-rev", Rules: rules}
}

func redactRule(id, category, pattern string) security.Rule {
	return security.Rule{ID: id, Category: category, Pattern: pattern}
}

// TestRedactorFindsSecretAcrossFrames is the brief-mandated test: a
// secret split by a frame boundary must be redacted before any output,
// and the output must contain a redaction placeholder.
func TestRedactorFindsSecretAcrossFrames(t *testing.T) {
	r := security.NewRedactor(testRegistry("sk-[A-Za-z0-9]+"))
	firstFrame := []byte("token=sk-ab")
	secondFrame := []byte("c123\n")
	first, err := r.WriteFrame(firstFrame)
	requireNoError(t, err)
	second, err := r.WriteFrame(secondFrame)
	requireNoError(t, err)
	got := first.Text + second.Text
	// Premise guard: the brief's forbidden value "sk-abc123" is exactly
	// the secret the joined raw frames leak, so the absence clause below
	// is non-vacuous: a raw pass-through must fail this test, and any
	// future edit that breaks that reachability fails here instead of
	// silently weakening the brief-mandated assertion.
	if !strings.Contains(string(firstFrame)+string(secondFrame), "sk-abc123") {
		t.Fatal("test premise broken: joined raw frames must contain the forbidden value")
	}
	if strings.Contains(got, "sk-abc123") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("unsafe output %q", got)
	}
}

// TestRedactorRedactsSecretSplitAcrossOneByteFrames: a secret delivered
// one byte per frame must never appear in the joined output.
func TestRedactorRedactsSecretSplitAcrossOneByteFrames(t *testing.T) {
	r := security.NewRedactor(testRegistry("sk-[A-Za-z0-9]+"))
	var got strings.Builder
	for _, b := range []byte("token=sk-abc12345\n") {
		f, err := r.WriteFrame([]byte{b})
		requireNoError(t, err)
		got.WriteString(f.Text)
	}
	f, err := r.Flush()
	requireNoError(t, err)
	got.WriteString(f.Text)
	out := got.String()
	if strings.Contains(out, "sk-abc12345") || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("unsafe output %q", out)
	}
}

// TestRedactorRedactsJSONEscapedSecret: a secret inside a JSON string,
// including inside escaped quotes, is replaced.
func TestRedactorRedactsJSONEscapedSecret(t *testing.T) {
	r := security.NewRedactor(testRegistry("sk-[A-Za-z0-9]+"))
	f, err := r.WriteFrame([]byte("{\"output\":\"token=sk-abc12345\",\"nested\":\"{\\\"credential\\\":\\\"sk-abc12345\\\"}\"}\n"))
	requireNoError(t, err)
	flush, err := r.Flush()
	requireNoError(t, err)
	out := f.Text + flush.Text
	if strings.Contains(out, "sk-abc12345") || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("unsafe output %q", out)
	}
}

// TestRedactorRedactsSecretInsideANSISequence: a secret hidden inside an
// ANSI escape sequence is still replaced.
func TestRedactorRedactsSecretInsideANSISequence(t *testing.T) {
	r := security.NewRedactor(testRegistry("sk-[A-Za-z0-9]+"))
	f, err := r.WriteFrame([]byte("\x1b[1;31mtoken=sk-abc12345\x1b[0m\n"))
	requireNoError(t, err)
	flush, err := r.Flush()
	requireNoError(t, err)
	out := f.Text + flush.Text
	if strings.Contains(out, "sk-abc12345") || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("unsafe output %q", out)
	}
}

// TestRedactorRedactsProviderTokenForms: each known provider token form
// is replaced by its category placeholder.
func TestRedactorRedactsProviderTokenForms(t *testing.T) {
	r := security.NewRedactor(registry(
		redactRule("github-pat", "github_token", "ghp_[A-Za-z0-9]{30,}"),
		redactRule("aws-access-key", "aws_access_key", "AKIA[0-9A-Z]{16}"),
		redactRule("slack-token", "slack_token", "xox[baprs]-[0-9A-Za-z-]{10,}"),
		redactRule("jwt", "jwt", "eyJ[A-Za-z0-9_-]{20,}\\.[A-Za-z0-9_-]{20,}\\.[A-Za-z0-9_-]{20,}"),
	))
	cases := []struct {
		secret      string
		placeholder string
	}{
		{"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890", "[REDACTED:github_token]"},
		{"AKIAIOSFODNN7EXAMPLE", "[REDACTED:aws_access_key]"},
		{"xoxb-123456789012-345678901234-abcdefghijklmnopqrstuvwx", "[REDACTED:slack_token]"},
		{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", "[REDACTED:jwt]"},
	}
	for _, tc := range cases {
		out := redactAll(t, r, []byte(tc.secret+"\n"))
		if strings.Contains(out, tc.secret) || !strings.Contains(out, tc.placeholder) {
			t.Fatalf("unsafe output %q for %s", out, tc.secret)
		}
	}
}

// TestRedactorRedactsEnvAssignments: secret-like environment assignments
// are replaced wholesale.
func TestRedactorRedactsEnvAssignments(t *testing.T) {
	r := security.NewRedactor(registry(redactRule(
		"env-assignment", "env_value",
		`(?i)\b[a-z0-9_]*(?:token|secret|password|passwd|credential|api[_-]?key|access[_-]?key|auth|private[_-]?key)[a-z0-9_]*\s*=\s*[^\s"',;]+`,
	)))
	out := redactAll(t, r, []byte("export ANTHROPIC_API_KEY=sk-ant-abc123def456\n"))
	if strings.Contains(out, "sk-ant-abc123def456") || !strings.Contains(out, "[REDACTED:env_value]") {
		t.Fatalf("unsafe output %q", out)
	}
}

// TestRedactorRejectsUnparseableBinaryFrame: content that cannot be
// checked as text (invalid UTF-8 or NUL bytes) fails closed with
// SENSITIVE_DATA_REDACTION_FAILED and produces no output.
func TestRedactorRejectsUnparseableBinaryFrame(t *testing.T) {
	for _, frame := range [][]byte{
		{0xff, 0xfe, 0xfd},
		[]byte("token=sk-abc12345\x00tail\n"),
	} {
		r := security.NewRedactor(testRegistry("sk-[A-Za-z0-9]+"))
		_, err := r.WriteFrame(frame)
		requireFaultCode(t, err, model.CodeSensitiveDataRedactionFailed)
	}
}

// TestRedactorRejectsOversizedFrame: a frame beyond the bounded size
// limit cannot be proven safe and fails closed.
func TestRedactorRejectsOversizedFrame(t *testing.T) {
	r := security.NewRedactor(testRegistry("sk-[A-Za-z0-9]+"))
	frame := make([]byte, 2<<20) // 2 MiB, beyond the 1 MiB bounded limit
	for i := range frame {
		frame[i] = 'a'
	}
	_, err := r.WriteFrame(frame)
	requireFaultCode(t, err, model.CodeSensitiveDataRedactionFailed)
}

// TestRedactorFailsClosedAfterError: after one failure the redactor stays
// poisoned: nothing can be emitted afterwards.
func TestRedactorFailsClosedAfterError(t *testing.T) {
	r := security.NewRedactor(testRegistry("sk-[A-Za-z0-9]+"))
	if _, err := r.WriteFrame([]byte("bad\x00binary")); err == nil {
		t.Fatal("binary frame accepted")
	}
	if _, err := r.WriteFrame([]byte("token=sk-abc12345\n")); err == nil {
		t.Fatal("write after failure accepted")
	}
	if _, err := r.Flush(); err == nil {
		t.Fatal("flush after failure accepted")
	}
}

// TestRedactorRejectsExtendableMatchBeyondWindow: a secret candidate
// still extending at the frame boundary that exceeds the bounded suffix
// window cannot be proven safe and fails closed (PRD 脱敏 6).
func TestRedactorRejectsExtendableMatchBeyondWindow(t *testing.T) {
	r := security.NewRedactor(testRegistry("sk-[A-Za-z0-9]+"))
	frame := []byte("sk-" + strings.Repeat("a", 5000))
	_, err := r.WriteFrame(frame)
	requireFaultCode(t, err, model.CodeSensitiveDataRedactionFailed)
}

// TestRedactorFlushEmitsWithheldTail: the withheld tail is emitted,
// redacted, when the stream ends; nothing raw survives.
func TestRedactorFlushEmitsWithheldTail(t *testing.T) {
	r := security.NewRedactor(testRegistry("sk-[A-Za-z0-9]+"))
	first, err := r.WriteFrame([]byte("token=sk-abc12345"))
	requireNoError(t, err)
	flush, err := r.Flush()
	requireNoError(t, err)
	out := first.Text + flush.Text
	if strings.Contains(out, "sk-abc12345") || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("unsafe output %q", out)
	}
}

// TestRedactorMetadataCarriesRevisionAndRuleIDs: the frame carries the
// rule revision and only non-secret rule IDs of the rules that fired.
func TestRedactorMetadataCarriesRevisionAndRuleIDs(t *testing.T) {
	r := security.NewRedactor(registry(
		redactRule("sk-token", "provider_token", "sk-[A-Za-z0-9]{8,}"),
		redactRule("github-pat", "github_token", "ghp_[A-Za-z0-9]{30,}"),
	))
	first, err := r.WriteFrame([]byte("a=sk-abc12345 b=ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890\n"))
	requireNoError(t, err)
	flush, err := r.Flush()
	requireNoError(t, err)
	all := []security.RedactedFrame{first, flush}
	for _, f := range all {
		if f.RuleRevision != "test-rev" {
			t.Fatalf("revision %q, want test-rev", f.RuleRevision)
		}
		for _, id := range f.RulesApplied {
			if id != "sk-token" && id != "github-pat" {
				t.Fatalf("unexpected rule id %q", id)
			}
		}
	}
	// Both rules fired somewhere across the stream; IDs are sorted.
	var ids []string
	for _, f := range all {
		ids = append(ids, f.RulesApplied...)
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "github-pat" || ids[1] != "sk-token" {
		t.Fatalf("rules applied %v, want both rules sorted", ids)
	}
}

// redactAll feeds every byte slice through WriteFrame, then Flush, and
// returns the joined output.
func redactAll(t *testing.T, r *security.Redactor, frames ...[]byte) string {
	t.Helper()
	var out strings.Builder
	for _, f := range frames {
		rf, err := r.WriteFrame(f)
		requireNoError(t, err)
		out.WriteString(rf.Text)
	}
	flush, err := r.Flush()
	requireNoError(t, err)
	out.WriteString(flush.Text)
	return out.String()
}

// ---------------------------------------------------------------------------
// Corpus (tests/testdata/redaction/corpus.json)
// ---------------------------------------------------------------------------

type corpusFixture struct {
	Revision string       `json:"revision"`
	Cases    []corpusCase `json:"cases"`
}

type corpusCase struct {
	ID                string       `json:"id"`
	Rules             []corpusRule `json:"rules"`
	Frames            []string     `json:"frames"`
	Forbidden         []string     `json:"forbidden"`
	ExpectPlaceholder []string     `json:"expectPlaceholders"`
	ExpectError       bool         `json:"expectError"`
	Oversized         bool         `json:"oversized"`
}

type corpusRule struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Pattern  string `json:"pattern"`
}

// TestRedactorCorpus runs every corpus case against a fresh redactor and
// asserts: no forbidden secret appears in the joined output, expected
// placeholders appear, failures surface as SENSITIVE_DATA_REDACTION_FAILED
// and persist nothing, and the whole run is byte-for-byte deterministic
// (design 22.2).
func TestRedactorCorpus(t *testing.T) {
	path := filepath.Join("..", "..", "tests", "testdata", "redaction", "corpus.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var fixture corpusFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("corpus has no cases")
	}

	run := func() map[string]string {
		outputs := make(map[string]string, len(fixture.Cases))
		for _, tc := range fixture.Cases {
			outputs[tc.ID] = runCorpusCase(t, fixture.Revision, tc)
		}
		return outputs
	}

	first := run()
	second := run()
	for id, out := range first {
		if second[id] != out {
			t.Fatalf("case %s is not deterministic", id)
		}
	}
}

func runCorpusCase(t *testing.T, revision string, tc corpusCase) string {
	t.Helper()
	rules := make([]security.Rule, 0, len(tc.Rules))
	ruleIDs := make(map[string]bool, len(tc.Rules))
	for _, cr := range tc.Rules {
		rules = append(rules, redactRule(cr.ID, cr.Category, cr.Pattern))
		ruleIDs[cr.ID] = true
	}
	r := security.NewRedactor(security.Registry{Revision: revision, Rules: rules})

	var out strings.Builder
	frames := tc.Frames
	if tc.Oversized {
		frames = []string{strings.Repeat("a", 2<<20)}
	}
	for _, f := range frames {
		rf, err := r.WriteFrame([]byte(f))
		if tc.ExpectError {
			if err == nil {
				t.Fatalf("case %s: expected SENSITIVE_DATA_REDACTION_FAILED, got output %q", tc.ID, rf.Text)
			}
			requireFaultCode(t, err, model.CodeSensitiveDataRedactionFailed)
			if rf.Text != "" {
				t.Fatalf("case %s: error frame emitted partial output %q", tc.ID, rf.Text)
			}
			return out.String()
		}
		requireNoError(t, err)
		out.WriteString(rf.Text)
		for _, id := range rf.RulesApplied {
			if !ruleIDs[id] {
				t.Fatalf("case %s: unknown rule id %q in metadata", tc.ID, id)
			}
		}
	}
	flush, err := r.Flush()
	requireNoError(t, err)
	out.WriteString(flush.Text)
	for _, id := range flush.RulesApplied {
		if !ruleIDs[id] {
			t.Fatalf("case %s: unknown rule id %q in metadata", tc.ID, id)
		}
	}

	for _, forbidden := range tc.Forbidden {
		if strings.Contains(out.String(), forbidden) {
			t.Fatalf("case %s: forbidden value %q survived redaction in %q", tc.ID, forbidden, out.String())
		}
	}
	for _, want := range tc.ExpectPlaceholder {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("case %s: placeholder %q missing from %q", tc.ID, want, out.String())
		}
	}
	return out.String()
}

// ---------------------------------------------------------------------------
// Release fault matrix rows (Task 21, ledger obligation (c)): the redaction
// fail-closed and short-body deferred minors. These mirror the matrix rows
// redaction_fail_closed / redaction_short_body at the unit level.
// ---------------------------------------------------------------------------

// TestMatrixRedactionFailClosedUnparseableRule: a rule that cannot compile
// poisons the Redactor; every later call fails closed with
// SENSITIVE_DATA_REDACTION_FAILED and no output is emitted under an
// incomplete policy (PRD 脱敏 6).
func TestMatrixRedactionFailClosedUnparseableRule(t *testing.T) {
	r := security.NewRedactor(security.Registry{Revision: "matrix-1", Rules: []security.Rule{
		{ID: "unparseable", Category: "secret", Pattern: `(`},
	}})
	frame, err := r.WriteFrame([]byte("token=sk-abc123456\n"))
	requireFaultCode(t, err, model.CodeSensitiveDataRedactionFailed)
	if frame.Text != "" {
		t.Fatalf("a poisoned redactor emitted output %q", frame.Text)
	}
	if _, err := r.Flush(); err == nil {
		t.Fatal("Flush after the poison succeeded")
	}
	pol, _ := model.Policy(model.CodeSensitiveDataRedactionFailed)
	if pol.Category != model.CatSafetyStop || !pol.CloseDispatch {
		t.Fatalf("policy(%s) = %+v, want SAFETY_STOP with dispatch closed", model.CodeSensitiveDataRedactionFailed, pol)
	}
}

// TestMatrixRedactionShortBodyFlushesFullyRedacted: a short body (the whole
// stream within the withholding window) with a trailing secret emits the
// placeholder and never the raw value — the withheld tail flushes fully
// redacted (the short-body deferred minor).
func TestMatrixRedactionShortBodyFlushesFullyRedacted(t *testing.T) {
	r := security.NewRedactor(testRegistry("sk-[A-Za-z0-9]+"))
	first, err := r.WriteFrame([]byte("credential=sk-abc123456789\n"))
	requireNoError(t, err)
	flush, err := r.Flush()
	requireNoError(t, err)
	out := first.Text + flush.Text
	if strings.Contains(out, "sk-abc123456789") {
		t.Fatalf("short body leaked the raw secret: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("short body produced no placeholder: %q", out)
	}
	// The stream ends redacted and the redactor is reusable for a new
	// stream (no poison from the flush).
	second := security.NewRedactor(testRegistry("sk-[A-Za-z0-9]+"))
	out2 := redactAll(t, second, []byte("plain text\n"))
	if strings.Contains(out2, "[REDACTED]") {
		t.Fatalf("a clean stream was over-redacted: %q", out2)
	}
}
