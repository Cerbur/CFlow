// The bounded build-identity render test (design 23): the Final Report's
// Binary line renders the immutable build identity — version, source
// Commit, Go version, and OS/arch — exactly as provided, with no
// timestamp and no dependence on the live process identity (host Go
// version, GOOS/GOARCH, or VCS state). Every field is pinned to a fixed
// value and the rendered line is asserted character-for-character, so
// the test cannot drift with the host toolchain or platform.
package observe

import (
	"strings"
	"testing"

	"cflow.local/cflow/internal/security"
)

// TestBuildIdentityRendersBounded: RenderMarkdown must render the pinned
// build identity into exactly one Binary line; the test never reads the
// running binary's own identity, so the expected output is fixed.
func TestBuildIdentityRendersBounded(t *testing.T) {
	md := RenderMarkdown(Report{
		Build: BuildInfo{
			Version:      "1.2.3-rc.1",
			SourceCommit: "a1b2c3d4",
			GoVersion:    "go1.26.5",
			OS:           "linux",
			Arch:         "amd64",
		},
	}, security.Registry{})

	want := "Binary: cflow 1.2.3-rc.1 (source a1b2c3d4, go go1.26.5, linux/amd64)"
	var got string
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "Binary: cflow ") {
			got = line
			break
		}
	}
	if got != want {
		t.Fatalf("build identity line = %q, want %q\nreport:\n%s", got, want, md)
	}
}
