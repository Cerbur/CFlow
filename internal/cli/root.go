// Package cli owns the line-oriented command surface: argument parsing,
// rendering, signal translation, and the stable process exit classes.
// The CLI never writes state, calls Git or Provider executables, runs
// external commands directly, or decides lifecycle transitions
// (design 20).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/spf13/cobra"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/claude"
	"cflow.local/cflow/internal/agent/codex"
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/config"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// Dependencies assembles what the command tree needs. Redaction is the
// embedded redaction rule registry every render and export path uses
// (design 19.2); a zero registry passes text through. OpenApplication
// replaces the default Application construction: tests inject the Fake
// Adapter with fixture scripts, production builds the GitFlow and the
// embedded registries over the working directory. RunTUI is the default
// full-screen entry point the bare `cflow` command calls on an
// interactive terminal (design §1, TUI task 9); it never mutates a
// Workflow by itself.
type Dependencies struct {
	Build           observe.BuildInfo
	Redaction       security.Registry
	OpenApplication func(ctx context.Context) (*app.Application, error)
	RunTUI          func(ctx context.Context) error
	// IsTerminal reports whether the process stdio is an interactive
	// terminal (default: the real isatty check; tests inject false or a
	// fake terminal).
	IsTerminal func() bool
}

// NewRoot builds the cflow command tree. version, help, and doctor are
// non-mutating: they never read, create, or modify CFLOW_HOME. The
// project commands route exclusively through the Application. A bare
// `cflow` on an interactive terminal launches the full-screen TUI;
// every subcommand behaves exactly as before (the headless CLI is
// preserved).
func NewRoot(deps Dependencies) *cobra.Command {
	root := &cobra.Command{
		Use:           "cflow",
		Short:         "local-first coding-agent workflow lifecycle CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return cmd.Help()
			}
			if deps.RunTUI == nil {
				return cmd.Help()
			}
			if terminal := deps.IsTerminal; terminal != nil && !terminal() {
				// No interactive terminal: a stable diagnostic, never a
				// mutation (design §1: bare cflow needs a TTY to run the
				// TUI; the headless subcommands stay available).
				fmt.Fprintln(cmd.OutOrStdout(),
					"cflow: no interactive terminal; run a subcommand (cflow status, cflow --help)")
				return nil
			}
			return deps.RunTUI(cmd.Context())
		},
	}
	root.AddCommand(newVersionCmd(deps.Build))
	root.AddCommand(newDoctorCmd(deps.Build))
	for _, cmd := range projectCommands(deps) {
		root.AddCommand(cmd)
	}
	return root
}

// Class is one of the stable process exit classes. The exact numeric
// values are contract (design 20); they are asserted centrally by
// TestExitCodeMapping and never scattered as literals elsewhere.
type Class int

const (
	ClassSuccess            Class = 0   // requested read or mutation reached its defined successful outcome
	ClassInvalidInput       Class = 2   // invalid command or user input
	ClassUserActionRequired Class = 3   // safe user action is required; Workflow is Paused or Blocked
	ClassLocalEnvironment   Class = 4   // local environment or compatibility precondition failed
	ClassInvariantFailure   Class = 5   // Runtime invariant failed or facts cannot be safely reconciled
	ClassInterrupted        Class = 130 // user interruption completed through the controlled-stop protocol
)

// Code returns the numeric process exit code.
func (c Class) Code() int { return int(c) }

// Error is a CLI error bound to exactly one exit class.
type Error struct {
	Class Class
	Msg   string
}

func (e *Error) Error() string { return e.Msg }

// ExitCode is the single central mapping from any error to a process exit
// class. Unrecognized errors count as invalid input (2); configuration
// failures are local environment preconditions (4); typed Fault
// categories map through faultClass; a cancelled command context is a
// user interruption (130).
func ExitCode(err error) int {
	if err == nil {
		return ClassSuccess.Code()
	}
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Class.Code()
	}
	var cfg *config.Error
	if errors.As(err, &cfg) {
		return ClassLocalEnvironment.Code()
	}
	var f *model.Fault
	if errors.As(err, &f) {
		return faultClass(f).Code()
	}
	if errors.Is(err, context.Canceled) {
		return ClassInterrupted.Code()
	}
	return ClassInvalidInput.Code()
}

// faultClass maps the typed Fault categories to the stable exit classes
// (design 20). This is the single central mapping: commands never pick
// their own exit codes, and the exact numeric values are asserted
// centrally by the exit-class tests.
func faultClass(f *model.Fault) Class {
	switch f.Category {
	case model.CatInvalidInput:
		return ClassInvalidInput
	case model.CatUserActionRequired:
		return ClassUserActionRequired
	case model.CatSafetyStop:
		// Safety stop: active work must be stopped before facts can be
		// trusted; the local environment or compatibility precondition
		// failed.
		return ClassLocalEnvironment
	case model.CatInvariantFailure:
		return ClassInvariantFailure
	case model.CatRetryableAttemptFailure:
		// Retryable attempt failures surface as Outcomes, never as command
		// errors; if one does, the requested outcome cannot be safely
		// reconciled.
		return ClassInvariantFailure
	}
	return ClassInvalidInput
}

// NotYetAvailable labels checks that later tasks implement. It is part of
// the doctor contract: unimplemented stateful checks are reported, never
// guessed.
const NotYetAvailable = "NOT_YET_AVAILABLE"

func newVersionCmd(build observe.BuildInfo) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print build identity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			renderBuildIdentity(cmd.OutOrStdout(), build)
			return nil
		},
	}
}

func newDoctorCmd(build observe.BuildInfo) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "report build identity, tool availability, and check status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			renderDoctor(cmd.OutOrStdout(), build)
			return nil
		},
	}
}

func renderBuildIdentity(w io.Writer, b observe.BuildInfo) {
	fmt.Fprintf(w, "cflow %s\n", b.Version)
	fmt.Fprintf(w, "  source commit: %s\n", b.SourceCommit)
	fmt.Fprintf(w, "  dirty: %v\n", b.Dirty)
	fmt.Fprintf(w, "  go: %s\n", b.GoVersion)
	fmt.Fprintf(w, "  os/arch: %s/%s\n", b.OS, b.Arch)
	fmt.Fprintf(w, "  registries: migration=%s artifact=%s provider=%s prompt=%s\n",
		hashOrUnset(b.Registries.Migration),
		hashOrUnset(b.Registries.Artifact),
		hashOrUnset(b.Registries.Provider),
		hashOrUnset(b.Registries.Prompt))
}

func hashOrUnset(hash string) string {
	if hash == "" {
		return "unset"
	}
	return hash
}

// toolChecks is the fixed set of tools doctor probes on PATH. Probing
// uses exec.LookPath only: no process is spawned and nothing executes.
var toolChecks = []string{"git", "go", "node", "npm", "codex", "claude"}

// statefulChecks are checks that later tasks implement. doctor reports
// them as NOT_YET_AVAILABLE instead of guessing a result. The provider
// protocol bindings check became real in Task 16 (renderProviderBindings
// below).
var statefulChecks = []string{
	"configuration validation",
	"home security posture",
	"store compatibility",
	"git commit policy",
}

// renderDoctor writes a read-only report: build identity, tool
// availability on PATH, the detected provider protocol bindings, and the
// status of the remaining stateful checks. It performs no filesystem
// mutation and never reads or creates CFLOW_HOME.
func renderDoctor(w io.Writer, build observe.BuildInfo) {
	renderBuildIdentity(w, build)
	fmt.Fprintf(w, "tools:\n")
	for _, tool := range toolChecks {
		if path, err := exec.LookPath(tool); err == nil {
			fmt.Fprintf(w, "  %s: found (%s)\n", tool, path)
		} else {
			fmt.Fprintf(w, "  %s: not found\n", tool)
		}
	}
	renderProviderBindings(w)
	fmt.Fprintf(w, "stateful checks:\n")
	for _, check := range statefulChecks {
		fmt.Fprintf(w, "  %s: %s\n", check, NotYetAvailable)
	}
}

// renderProviderBindings reports every enabled Provider binding's
// detected installation (Task 16, ledger obligation from Tasks 14/15):
// the read-only doctor runs each enabled binding's version probe through
// its real Adapter — a process is only ever started for the read-only
// version probe, never a paid model request, and nothing is written.
// Authentication is never probed (that would require a model request)
// and stays a distinct UNKNOWN fact.
func renderProviderBindings(w io.Writer) {
	fmt.Fprintf(w, "provider protocol bindings:\n")
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		fmt.Fprintf(w, "  registry: %v\n", err)
		return
	}
	sup := process.NewSupervisor(process.NewOSAdapter())
	for _, name := range reg.EnabledNames() {
		binding, err := reg.Select(name)
		if err != nil {
			fmt.Fprintf(w, "  %s: %v\n", name, err)
			continue
		}
		ad, ok := dialectAdapter(name, sup, binding)
		if !ok {
			fmt.Fprintf(w, "  %s: NO_ADAPTER\n", name)
			continue
		}
		inst, err := ad.Detect(context.Background())
		if err != nil {
			fmt.Fprintf(w, "  %s: DETECT_FAILED (%v)\n", name, err)
			continue
		}
		switch inst.Compatibility {
		case agent.CompatibilitySupported:
			fmt.Fprintf(w, "  %s: SUPPORTED (executable %s, sha256 %s, cli %s, dialect %s, registry revision %s)\n",
				name, inst.ExecutablePath, inst.ExecutableSHA256, inst.CLIVersion, inst.DialectID, inst.RegistryRevision)
		default:
			fmt.Fprintf(w, "  %s: %s\n", name, inst.Compatibility)
		}
	}
	fmt.Fprintf(w, "authentication: UNKNOWN (the read-only doctor never starts a model request)\n")
}

// dialectAdapter binds the real Adapter of one enabled Provider for the
// read-only doctor detection.
func dialectAdapter(name string, sup process.Supervisor, binding agent.ProviderBinding) (agent.Adapter, bool) {
	switch name {
	case "codex":
		return codex.New(sup, binding), true
	case "claude":
		return claude.New(sup, binding), true
	}
	return nil, false
}
