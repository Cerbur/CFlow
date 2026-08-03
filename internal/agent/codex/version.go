// Semantic version parsing for the codex CLI identity probe. The
// captured 0.141.0 fixture prints `codex-cli 0.141.0`; the supported
// version range comes from the registry binding (">=0.80.0 <2.0.0" for
// the captured codex binding). Matching is a plain triple comparison;
// any unparseable probe output or constraint fails closed.
package codex

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// semverRE matches the first X.Y.Z triple anywhere in the probe output.
// A pre-release suffix ("0.141.0-dev") matches its triple and is judged
// by that triple.
var semverRE = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)

// version is one parsed semantic version triple.
type version struct {
	major, minor, patch int
}

// String renders the canonical triple.
func (v version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

// parseVersion extracts the first semantic version triple from text.
func parseVersion(text string) (version, bool) {
	m := semverRE.FindString(text)
	if m == "" {
		return version{}, false
	}
	parts := strings.Split(m, ".")
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	patch, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return version{}, false
	}
	return version{major: major, minor: minor, patch: patch}, true
}

// compare orders two versions: -1, 0, or +1.
func (v version) compare(o version) int {
	for _, pair := range [][2]int{{v.major, o.major}, {v.minor, o.minor}, {v.patch, o.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

// inRange reports whether v satisfies every space-separated constraint of
// the binding's version range, e.g. ">=0.80.0 <2.0.0". A token is an
// operator (<, <=, >, >=) followed by a version; a bare version means
// exact equality. Any unparseable constraint fails closed.
func inRange(v version, constraint string) bool {
	if strings.TrimSpace(constraint) == "" {
		return false
	}
	for _, tok := range strings.Fields(constraint) {
		op := ""
		rest := tok
		for _, candidate := range []string{">=", "<=", ">", "<"} {
			if strings.HasPrefix(tok, candidate) {
				op = candidate
				rest = tok[len(candidate):]
				break
			}
		}
		w, ok := parseVersion(rest)
		if !ok {
			return false
		}
		c := v.compare(w)
		switch op {
		case ">=":
			if c < 0 {
				return false
			}
		case "<=":
			if c > 0 {
				return false
			}
		case ">":
			if c <= 0 {
				return false
			}
		case "<":
			if c >= 0 {
				return false
			}
		case "":
			if c != 0 {
				return false
			}
		}
	}
	return true
}
