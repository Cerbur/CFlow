package cli

// The stateful project commands (design 20). Each command's
// responsibilities are limited to parsing arguments, calling the
// Application, rendering the projection or outcome, and returning the
// central exit classes. The CLI never writes SQLite or Artifacts, never
// calls Git or Provider executables, and never decides state transitions.

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/spf13/cobra"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// projectCommands builds the stateful command surface.
func projectCommands(deps Dependencies) []*cobra.Command {
	return []*cobra.Command{
		{
			Use:   "list",
			Short: "list project workflows",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd)
				defer stop()
				a, err := openApplication(ctx, deps.Redaction)
				if err != nil {
					return err
				}
				view, err := a.Query(ctx, app.ListQuery{})
				if err != nil {
					return err
				}
				renderList(cmd.OutOrStdout(), view.(app.ListView), deps.Redaction)
				return nil
			},
		},
		{
			Use:   "status [workflow-id]",
			Short: "show workflow status",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd)
				defer stop()
				a, err := openApplication(ctx, deps.Redaction)
				if err != nil {
					return err
				}
				view, err := a.Query(ctx, app.StatusQuery{Workflow: workflowArg(args)})
				if err != nil {
					return err
				}
				renderStatus(cmd.OutOrStdout(), view.(app.StatusView), deps.Redaction)
				return nil
			},
		},
		{
			Use:   "inspect [workflow-id]",
			Short: "show the full workflow aggregate",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd)
				defer stop()
				a, err := openApplication(ctx, deps.Redaction)
				if err != nil {
					return err
				}
				view, err := a.Query(ctx, app.InspectQuery{Workflow: workflowArg(args)})
				if err != nil {
					return err
				}
				renderInspect(cmd.OutOrStdout(), view.(app.InspectView), deps.Redaction)
				return nil
			},
		},
		{
			Use:   "logs [workflow-id]",
			Short: "show the redacted event log",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd)
				defer stop()
				a, err := openApplication(ctx, deps.Redaction)
				if err != nil {
					return err
				}
				view, err := a.Query(ctx, app.LogsQuery{Workflow: workflowArg(args)})
				if err != nil {
					return err
				}
				renderLogs(cmd.OutOrStdout(), view.(app.LogsView), deps.Redaction)
				return nil
			},
		},
		{
			Use:   "pause [workflow-id]",
			Short: "pause a running workflow",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return executeMutation(cmd, deps, app.PauseWorkflowCommand{Workflow: workflowArg(args)})
			},
		},
		{
			Use:   "resume [workflow-id]",
			Short: "resume a paused workflow",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return executeMutation(cmd, deps, app.ResumeWorkflowCommand{Workflow: workflowArg(args)})
			},
		},
		{
			Use:   "cancel [workflow-id]",
			Short: "cancel a workflow",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return executeMutation(cmd, deps, app.CancelWorkflowCommand{Workflow: workflowArg(args)})
			},
		},
		{
			Use:   "dry-run [workflow-id]",
			Short: "produce the cleanup dry-run manifest",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return executeMutation(cmd, deps, app.DryRunCommand{Workflow: workflowArg(args)})
			},
		},
	}
}

// executeMutation runs one mutation command and renders its Outcome.
func executeMutation(cmd *cobra.Command, deps Dependencies, command app.Command) error {
	ctx, stop := commandContext(cmd)
	defer stop()
	a, err := openApplication(ctx, deps.Redaction)
	if err != nil {
		return err
	}
	out, err := a.Execute(ctx, command)
	if err != nil {
		return err
	}
	renderOutcome(cmd.OutOrStdout(), command, out, deps.Redaction)
	return nil
}

// commandContext wires the user interruption (design 20): a SIGINT
// cancels the command context, and the central exit mapping turns the
// cancellation into exit class 130. The first/second controlled-stop
// command translation arrives with the full stop protocol (Task 17).
func commandContext(cmd *cobra.Command) (context.Context, func()) {
	return signal.NotifyContext(cmd.Context(), os.Interrupt)
}

func workflowArg(args []string) model.WorkflowID {
	if len(args) == 1 {
		return model.WorkflowID(args[0])
	}
	return ""
}

// openApplication assembles the Application for one stateful command from
// the environment (design 20.1): CFLOW_HOME or the default ~/.cflow, and
// the current working directory as the project root.
func openApplication(ctx context.Context, redaction security.Registry) (*app.Application, error) {
	home, err := resolveHome()
	if err != nil {
		return nil, err
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	return app.New(app.Options{
		Home:         home,
		Project:      app.ProjectFor(root),
		CflowVersion: observe.Version,
		Redaction:    redaction,
		Supervisor:   process.NewSupervisor(process.NewOSAdapter()),
	})
}

// resolveHome resolves CFLOW_HOME: the environment variable wins, then
// the default ~/.cflow.
func resolveHome() (string, error) {
	home := os.Getenv("CFLOW_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		home = filepath.Join(userHome, ".cflow")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve CFLOW_HOME: %w", err)
	}
	return abs, nil
}
