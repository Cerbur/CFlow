package cli_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/cli"
	"cflow.local/cflow/internal/config"
	"cflow.local/cflow/internal/observe"
)

// TestVersionDoesNotCreateHome: version is non-mutating; it must run
// without creating CFLOW_HOME and its output must never say "unknown".
// This is the brief-mandated test.
func TestVersionDoesNotCreateHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	out, code := runCLI(t, home, "version")
	if code != 0 || strings.Contains(out, "unknown") || pathExists(home) {
		t.Fatalf("code=%d out=%q homeExists=%v", code, out, pathExists(home))
	}
}

// TestHelpDoesNotCreateHome: help is non-mutating.
func TestHelpDoesNotCreateHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	out, code := runCLI(t, home, "help")
	if code != 0 || !strings.Contains(out, "Usage:") || pathExists(home) {
		t.Fatalf("code=%d out=%q homeExists=%v", code, out, pathExists(home))
	}
}

// TestDoctorDoesNotCreateHome: doctor is strictly read-only and must not
// create CFLOW_HOME. Unimplemented stateful checks are labeled
// NOT_YET_AVAILABLE, never guessed.
func TestDoctorDoesNotCreateHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	out, code := runCLI(t, home, "doctor")
	if code != 0 || !strings.Contains(out, cli.NotYetAvailable) || pathExists(home) {
		t.Fatalf("code=%d out=%q homeExists=%v", code, out, pathExists(home))
	}
}

// TestDoctorReportsBuildAndToolAvailability: the read-only doctor reports
// build identity, tool availability, and the status of stateful checks.
func TestDoctorReportsBuildAndToolAvailability(t *testing.T) {
	out, code := runCLI(t, filepath.Join(t.TempDir(), "absent"), "doctor")
	if code != 0 {
		t.Fatalf("code=%d out=%q", code, out)
	}
	for _, want := range []string{
		"cflow ",
		"0.0.0-test",
		"tools:",
		"git:",
		"stateful checks:",
		cli.NotYetAvailable,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q in:\n%s", want, out)
		}
	}
}

// TestVersionReportsBuildIdentity: version reports version, source
// commit, dirty flag, Go version, OS/architecture, and every embedded
// registry hash.
func TestVersionReportsBuildIdentity(t *testing.T) {
	out, code := runCLI(t, filepath.Join(t.TempDir(), "absent"), "version")
	if code != 0 {
		t.Fatalf("code=%d out=%q", code, out)
	}
	for _, want := range []string{
		"cflow ",
		"0.0.0-test",
		"abcd1234",
		"dirty: true",
		"go1.26.5",
		"darwin/arm64",
		"migration=reg-migration",
		"artifact=reg-artifact",
		"provider=reg-provider",
		"prompt=reg-prompt",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("version output missing %q in:\n%s", want, out)
		}
	}
}

// TestVersionRendersUnsetRegistries: registry hashes that no loader has
// filled yet render as "unset", never as fabricated values.
func TestVersionRendersUnsetRegistries(t *testing.T) {
	build := observe.BuildInfo{Version: "0.0.0-dev", SourceCommit: "unset"}
	out, code := runCLIWith(t, filepath.Join(t.TempDir(), "absent"), build, "version")
	if code != 0 {
		t.Fatalf("code=%d out=%q", code, out)
	}
	for _, want := range []string{
		"source commit: unset",
		"migration=unset", "artifact=unset", "provider=unset", "prompt=unset",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("version output missing %q in:\n%s", want, out)
		}
	}
}

// TestExitCodeMapping: the exact numeric exit classes are contract
// (design 20); they are asserted centrally and never scattered as
// literals elsewhere.
func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"invalid input", &cli.Error{Class: cli.ClassInvalidInput, Msg: "bad input"}, 2},
		{"user action required", &cli.Error{Class: cli.ClassUserActionRequired, Msg: "approve"}, 3},
		{"local environment", &cli.Error{Class: cli.ClassLocalEnvironment, Msg: "env"}, 4},
		{"invariant failure", &cli.Error{Class: cli.ClassInvariantFailure, Msg: "invariant"}, 5},
		{"interrupted", &cli.Error{Class: cli.ClassInterrupted, Msg: "interrupt"}, 130},
		{"wrapped cli error keeps class", fmt.Errorf("wrap: %w", &cli.Error{Class: cli.ClassUserActionRequired, Msg: "wrapped"}), 3},
		{"config error is local environment", &config.Error{Msg: "unknown key"}, 4},
		{"wrapped config error stays class 4", fmt.Errorf("wrap: %w", &config.Error{Msg: "bad file"}), 4},
		{"plain error is invalid input", errors.New("boom"), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cli.ExitCode(tc.err); got != tc.want {
				t.Fatalf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestUnknownConfigKeyFailsWithExitClass4: an unknown configuration key
// is a local environment precondition failure and must surface as exit
// class 4 through the central mapping.
func TestUnknownConfigKeyFailsWithExitClass4(t *testing.T) {
	path := writeConfig(t, "concurrency: 2\nunknown_key: true\n")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected unknown-key error")
	}
	if got := cli.ExitCode(err); got != 4 {
		t.Fatalf("ExitCode = %d, want 4 (local environment), err=%v", got, err)
	}
}

// TestUnknownFlagIsInvalidInput: cobra-level usage errors map centrally
// to exit class 2.
func TestUnknownFlagIsInvalidInput(t *testing.T) {
	_, code := runCLI(t, filepath.Join(t.TempDir(), "absent"), "version", "--bogus")
	if code != 2 {
		t.Fatalf("code=%d, want 2", code)
	}
}

// TestVersionRejectsExtraArgs: positional arguments are invalid input.
func TestVersionRejectsExtraArgs(t *testing.T) {
	_, code := runCLI(t, filepath.Join(t.TempDir(), "absent"), "version", "extra")
	if code != 2 {
		t.Fatalf("code=%d, want 2", code)
	}
}

func fixtureBuild() observe.BuildInfo {
	return observe.BuildInfo{
		Version:      "0.0.0-test",
		SourceCommit: "abcd1234",
		Dirty:        true,
		GoVersion:    "go1.26.5",
		OS:           "darwin",
		Arch:         "arm64",
		Registries: observe.RegistryHashes{
			Migration: "reg-migration",
			Artifact:  "reg-artifact",
			Provider:  "reg-provider",
			Prompt:    "reg-prompt",
		},
	}
}

func runCLI(t *testing.T, home string, args ...string) (string, int) {
	t.Helper()
	return runCLIWith(t, home, fixtureBuild(), args...)
}

func runCLIWith(t *testing.T, home string, build observe.BuildInfo, args ...string) (string, int) {
	t.Helper()
	t.Setenv("CFLOW_HOME", home)
	var out, errBuf bytes.Buffer
	root := cli.NewRoot(cli.Dependencies{Build: build})
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err := root.Execute()
	return out.String() + errBuf.String(), cli.ExitCode(err)
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
