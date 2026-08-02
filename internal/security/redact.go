// Streaming, versioned redaction (design 19.2). The Redactor receives
// bounded text frames (structured values arrive as their own frames;
// callers never render them before redaction) and returns redacted text
// plus the rule revision and the non-secret IDs of the rules that fired.
//
// Cross-frame safety: every frame is matched against the concatenation of
// the withheld tail and the new frame. The tail is the minimal bounded
// suffix that may still become part of a secret: raw values live in
// memory only for the duration of one match pass. A match that is already
// provably complete (a following byte that cannot extend it) is replaced
// by its category placeholder immediately; plain bytes closer than
// suffixWindow to the stream end are withheld in case they are a secret
// prefix. A match still extending at the frame boundary that exceeds the
// window cannot be proven safe: the frame fails closed with
// SENSITIVE_DATA_REDACTION_FAILED and nothing is persisted (PRD 脱敏 6).
//
// Fail-closed: invalid UTF-8, NUL bytes, oversized frames, unparseable
// rules, or an unbounded secret candidate poison the Redactor. After a
// failure every call returns the same fault; the runtime stops the
// affected process instead of persisting partial output. Secrets are
// replaced by stable placeholders like [REDACTED:provider_token]; raw
// values, hashes of values, and lengths are never retained or returned.
package security

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"cflow.local/cflow/internal/model"
)

// suffixWindow is the number of trailing bytes withheld from output at
// any moment: the maximum secret length the Redactor can prove safe
// across frame boundaries. Rules must not define matches longer than the
// window; an extendable match exceeding it fails closed.
const suffixWindow = 4096

// maxFrameBytes bounds one WriteFrame call. A larger frame is treated as
// content that cannot be safely checked and fails closed.
const maxFrameBytes = 1 << 20

// Rule is one redaction rule: a non-secret ID, a stable placeholder
// category, and a RE2 pattern. Rules are CFlow-owned embedded policy,
// never user input.
type Rule struct {
	ID       string
	Category string
	Pattern  string
}

// Registry is the versioned rule set the Redactor compiles. Revision is
// persisted with every redacted frame so artifacts can name the exact
// policy that produced them (PRD 脱敏 8).
type Registry struct {
	Revision string
	Rules    []Rule
}

// RedactedFrame is the safe output of one WriteFrame or Flush call: only
// redacted text, the rule revision, and non-secret rule IDs.
type RedactedFrame struct {
	Text         string
	RuleRevision string
	RulesApplied []string
}

// Redactor is a streaming redactor for one registry. It is not safe for
// concurrent use: a stream is one goroutine's sequential sequence of
// frames.
type Redactor struct {
	registry Registry
	compiled []compiledRule
	tail     []byte // raw withheld suffix, always <= suffixWindow bytes
	failed   error  // fail-closed poison after any unprovable frame
}

type compiledRule struct {
	rule Rule
	re   *regexp.Regexp
}

// matchSpan is one claimed span of the combined buffer, attributed to
// the rule that wins the overlap (earliest start, then earliest rule).
type matchSpan struct {
	start, end int
	category   string
	id         string
	ruleIdx    int
}

// NewRedactor compiles the registry. A rule that cannot compile poisons
// the Redactor: WriteFrame then fails closed rather than emitting output
// under an incomplete policy.
func NewRedactor(reg Registry) *Redactor {
	r := &Redactor{registry: reg}
	for _, rule := range reg.Rules {
		re, err := regexp.Compile(rule.Pattern)
		if err != nil {
			r.failed = r.fail(fmt.Sprintf("redaction rule %q failed to compile", rule.ID))
			return r
		}
		r.compiled = append(r.compiled, compiledRule{rule: rule, re: re})
	}
	return r
}

// WriteFrame redacts one bounded text frame and returns the safe output.
// On any failure it returns a SENSITIVE_DATA_REDACTION_FAILED fault and
// poisons the Redactor; the frame's content must not be persisted.
func (r *Redactor) WriteFrame(frame []byte) (RedactedFrame, error) {
	if r.failed != nil {
		return RedactedFrame{}, r.failed
	}
	if len(frame) > maxFrameBytes {
		return RedactedFrame{}, r.fail("frame exceeds the bounded size limit")
	}
	buf := make([]byte, 0, len(r.tail)+len(frame))
	buf = append(buf, r.tail...)
	buf = append(buf, frame...)
	if !utf8.Valid(buf) || bytes.IndexByte(buf, 0) >= 0 {
		return RedactedFrame{}, r.fail("frame is not bounded safe text")
	}
	spans := r.findSpans(buf)
	for _, sp := range spans {
		if sp.end == len(buf) && sp.end-sp.start > suffixWindow {
			return RedactedFrame{}, r.fail("secret candidate spans the frame boundary beyond the bounded window")
		}
	}
	text, applied, tail := render(buf, spans, suffixWindow)
	r.tail = tail
	return RedactedFrame{Text: text, RuleRevision: r.registry.Revision, RulesApplied: applied}, nil
}

// Flush emits the withheld tail, fully redacted, at the end of a stream.
// With no future bytes every match is final, so the whole tail can be
// proven safe. Callers must Flush before persisting the last segment;
// WriteFrame may be called again afterwards.
func (r *Redactor) Flush() (RedactedFrame, error) {
	if r.failed != nil {
		return RedactedFrame{}, r.failed
	}
	if len(r.tail) == 0 {
		return RedactedFrame{RuleRevision: r.registry.Revision}, nil
	}
	spans := r.findSpans(r.tail)
	var sb strings.Builder
	applied := make([]string, 0, len(spans))
	pos := 0
	for _, sp := range spans {
		if sp.start > pos {
			sb.Write(r.tail[pos:sp.start])
		}
		sb.WriteString(placeholder(sp.category))
		applied = append(applied, sp.id)
		pos = sp.end
	}
	if pos < len(r.tail) {
		sb.Write(r.tail[pos:])
	}
	r.tail = nil
	return RedactedFrame{Text: sb.String(), RuleRevision: r.registry.Revision, RulesApplied: sortedUnique(applied)}, nil
}

// fail records the fail-closed fault once and returns it.
func (r *Redactor) fail(reason string) error {
	if r.failed == nil {
		r.failed = model.NewFault(model.CodeSensitiveDataRedactionFailed, reason)
	}
	return r.failed
}

// findSpans collects every rule match into merged, non-overlapping spans.
// Overlapping spans are claimed as one span from the earliest start (ties
// go to the earliest rule), covering the union, so no byte of a losing
// match can be emitted as plain text.
func (r *Redactor) findSpans(buf []byte) []matchSpan {
	var spans []matchSpan
	for ri, cr := range r.compiled {
		for _, m := range cr.re.FindAllIndex(buf, -1) {
			if m[1] <= m[0] {
				continue
			}
			spans = append(spans, matchSpan{start: m[0], end: m[1], category: cr.rule.Category, id: cr.rule.ID, ruleIdx: ri})
		}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].ruleIdx < spans[j].ruleIdx
	})
	merged := spans[:0]
	for _, sp := range spans {
		n := len(merged)
		if n == 0 || sp.start > merged[n-1].end {
			merged = append(merged, sp)
			continue
		}
		if sp.end > merged[n-1].end {
			merged[n-1].end = sp.end
		}
	}
	return merged
}

// render computes the emitted text, the applied rule IDs, and the new
// withheld tail for one combined buffer.
func render(buf []byte, spans []matchSpan, window int) (string, []string, []byte) {
	// cut is the emission boundary: plain bytes at or beyond it stay in
	// the withheld tail, and any match still extending at the buffer end
	// pulls the boundary back to its start.
	cut := len(buf) - window
	if cut < 0 {
		cut = 0
	}
	for _, sp := range spans {
		if sp.end == len(buf) && sp.start < cut {
			cut = sp.start
		}
	}
	applied := make(map[string]bool)
	var sb strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp.start > pos {
			emit := min(cut, sp.start)
			if emit > pos {
				sb.Write(buf[pos:emit])
			}
			pos = emit
		}
		if sp.end < len(buf) {
			// Provably complete: the placeholder stands in for the whole
			// match now; these bytes leave the stream.
			sb.WriteString(placeholder(sp.category))
			applied[sp.id] = true
			pos = sp.end
		} else {
			// Still extending at the stream end: withhold the whole span
			// in case a future frame completes it.
			pos = sp.start
		}
	}
	if emit := min(cut, len(buf)); emit > pos {
		sb.Write(buf[pos:emit])
	}
	return sb.String(), sortedUnique(keys(applied)), buildTail(buf, spans, cut)
}

// buildTail returns the raw withheld suffix: everything at or beyond cut,
// minus the bytes of provably complete matches (already emitted as
// placeholders, so future buffers do not need them). The result is at
// most window bytes.
func buildTail(buf []byte, spans []matchSpan, cut int) []byte {
	var tail []byte
	segStart := cut
	for _, sp := range spans {
		if sp.end <= cut {
			continue
		}
		if sp.start > segStart {
			tail = append(tail, buf[segStart:sp.start]...)
		}
		if sp.end < len(buf) {
			segStart = sp.end // complete match: emitted, bytes excluded
		} else {
			tail = append(tail, buf[sp.start:sp.end]...) // extendable: withheld raw
			segStart = sp.end
		}
	}
	if segStart < len(buf) {
		tail = append(tail, buf[segStart:]...)
	}
	return tail
}

// placeholder renders the stable placeholder. A rule that declares no
// category yields the untyped "[REDACTED]"; categorized rules yield
// "[REDACTED:<category>]" like "[REDACTED:provider_token]". The
// placeholder never carries the value, a hash of it, or its length.
func placeholder(category string) string {
	if category == "" {
		return "[REDACTED]"
	}
	return "[REDACTED:" + category + "]"
}

// sortedUnique returns a sorted, de-duplicated copy of ids.
func sortedUnique(ids []string) []string {
	if len(ids) < 2 {
		return ids
	}
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
