package cli

// The stateful project commands (design 20). Each command's
// responsibilities are limited to parsing arguments, calling the
// Application, rendering the projection or outcome, and returning the
// central exit classes. The CLI never writes SQLite or Artifacts, never
// calls Git or Provider executables, and never decides state transitions.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/process"
)

// projectCommands builds the stateful command surface.
func projectCommands(deps Dependencies) []*cobra.Command {
	cmds := []*cobra.Command{
		{
			Use:   "list",
			Short: "list project workflows",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd)
				defer stop()
				a, err := openApplication(ctx, deps)
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
				a, err := openApplication(ctx, deps)
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
				a, err := openApplication(ctx, deps)
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
				a, err := openApplication(ctx, deps)
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
		{
			Use:   "workflow-create <name>",
			Short: "create a workflow for the current git repository",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				provider, _ := cmd.Flags().GetString("provider")
				assumeYes, _ := cmd.Flags().GetBool("yes")
				ctx, stop := commandContext(cmd)
				defer stop()
				a, err := openApplication(ctx, deps)
				if err != nil {
					return err
				}
				view, err := a.Query(ctx, app.DiscoveryQuery{})
				if err != nil {
					return err
				}
				dv := view.(app.DiscoveryView)
				if dv.Unborn {
					return model.InvalidInputFault("repository has no commits; a workflow requires a valid HEAD")
				}
				if dv.Detached {
					return model.InvalidInputFault("detached HEAD cannot create a new workflow")
				}
				confirm := assumeYes
				if dv.Dirty && !assumeYes {
					fmt.Fprintf(cmd.OutOrStdout(),
						"user workspace is dirty (%d staged, %d unstaged, %d untracked); it will be isolated and never enter the workflow. continue? [y/N] ",
						dv.StagedCount, dv.UnstagedCount, dv.UntrackedCount)
					line, err := readLine(cmd)
					if err != nil {
						return err
					}
					confirm = strings.EqualFold(strings.TrimSpace(line), "y")
					if !confirm {
						return model.InvalidInputFault("workflow creation aborted: the user workspace is dirty")
					}
				}
				out, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: args[0], Provider: provider, ConfirmDirty: confirm})
				if err != nil {
					return err
				}
				renderOutcome(cmd.OutOrStdout(), app.CreateWorkflowCommand{}, out, deps.Redaction)
				return nil
			},
		},
		{
			Use:   "discuss [workflow-id]",
			Short: "submit one requirement discussion turn from stdin",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				provider, _ := cmd.Flags().GetString("provider")
				text, err := readDiscussionInput(cmd)
				if err != nil {
					return err
				}
				return executeMutation(cmd, deps, app.DiscussRequirementCommand{
					Workflow: workflowArg(args), Text: text, Provider: provider,
				})
			},
		},
		{
			Use:   "plan-generate [workflow-id]",
			Short: "produce a new plan revision",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				provider, _ := cmd.Flags().GetString("provider")
				return executeMutation(cmd, deps, app.GeneratePlanCommand{
					Workflow: workflowArg(args), Provider: provider,
				})
			},
		},
		{
			Use:   "plan-check [workflow-id]",
			Short: "run an independent plan check",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				provider, _ := cmd.Flags().GetString("provider")
				return executeMutation(cmd, deps, app.CheckPlanCommand{
					Workflow: workflowArg(args), Provider: provider,
				})
			},
		},
		{
			Use:   "plan-approve [workflow-id]",
			Short: "approve the exact active plan revision and hash",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd)
				defer stop()
				a, err := openApplication(ctx, deps)
				if err != nil {
					return err
				}
				view, err := a.Query(ctx, app.PlanQuery{Workflow: workflowArg(args)})
				if err != nil {
					return err
				}
				pv := view.(app.PlanView)
				if pv.Revision == 0 || pv.Hash == "" {
					return model.InvalidInputFault("no plan to approve")
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"approve plan revision %d with sha256 %s (status %s)? [y/N] ",
					pv.Revision, pv.Hash, pv.PlanStatus)
				line, err := readLine(cmd)
				if err != nil {
					return err
				}
				if !strings.EqualFold(strings.TrimSpace(line), "y") {
					return model.InvalidInputFault("plan approval aborted")
				}
				return executeMutation(cmd, deps, app.ApprovePlanCommand{
					Workflow: workflowArg(args), Revision: pv.Revision, Hash: pv.Hash,
				})
			},
		},
		{
			Use:   "plan-show [workflow-id]",
			Short: "show the active plan revision's review state",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd)
				defer stop()
				a, err := openApplication(ctx, deps)
				if err != nil {
					return err
				}
				view, err := a.Query(ctx, app.PlanQuery{Workflow: workflowArg(args)})
				if err != nil {
					return err
				}
				renderPlan(cmd.OutOrStdout(), view.(app.PlanView), deps.Redaction)
				return nil
			},
		},
		{
			Use:   "spec-generate [workflow-id]",
			Short: "discover the verification catalog and generate the spec",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				provider, _ := cmd.Flags().GetString("provider")
				return executeMutation(cmd, deps, app.GenerateSpecsCommand{
					Workflow: workflowArg(args), Provider: provider,
				})
			},
		},
		{
			Use:   "compile-workflow [workflow-id]",
			Short: "compile the approved specs into the restricted workflow",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				provider, _ := cmd.Flags().GetString("provider")
				return executeMutation(cmd, deps, app.CompileWorkflowCommand{
					Workflow: workflowArg(args), Provider: provider,
				})
			},
		},
		{
			Use:   "execution-dry-run [workflow-id]",
			Short: "run the commit preflight and pause at the execution approval gate",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return executeExecutionDryRun(cmd, deps, workflowArg(args))
			},
		},
		{
			Use:   "execution-show [workflow-id]",
			Short: "show the execution approval preview",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd)
				defer stop()
				a, err := openApplication(ctx, deps)
				if err != nil {
					return err
				}
				view, err := a.Query(ctx, app.ExecutionPreviewQuery{Workflow: workflowArg(args)})
				if err != nil {
					return err
				}
				renderExecutionPreview(cmd.OutOrStdout(), view.(app.ExecutionPreviewView), deps.Redaction)
				return nil
			},
		},
		{
			Use:   "execution-approve [workflow-id]",
			Short: "approve the exact displayed execution inputs",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd)
				defer stop()
				a, err := openApplication(ctx, deps)
				if err != nil {
					return err
				}
				view, err := a.Query(ctx, app.ExecutionPreviewQuery{Workflow: workflowArg(args)})
				if err != nil {
					return err
				}
				pv := view.(app.ExecutionPreviewView)
				renderExecutionPreview(cmd.OutOrStdout(), pv, deps.Redaction)
				fmt.Fprintf(cmd.OutOrStdout(),
					"approve execution of plan %s, spec %s, catalog %s, workflow %s? [y/N] ",
					shortRef(pv.Plan), shortRef(pv.Spec), shortRef(pv.Catalog), shortRef(pv.WorkflowArtifact))
				line, err := readLine(cmd)
				if err != nil {
					return err
				}
				if !strings.EqualFold(strings.TrimSpace(line), "y") {
					return model.InvalidInputFault("execution approval aborted")
				}
				return executeMutation(cmd, deps, app.ApproveExecutionCommand{
					Workflow:         workflowArg(args),
					PlanHash:         pv.PlanHash,
					SpecHashes:       pv.SpecHashes,
					CatalogHash:      pv.CatalogHash,
					WorkflowHash:     pv.WorkflowHash,
					RoutingHash:      pv.RoutingHash,
					BudgetHash:       pv.BudgetHash,
					CommitPolicyHash: pv.CommitPolicyHash,
				})
			},
		},
	}
	// The planning commands share the discussion Agent route flag.
	for _, name := range []string{"workflow-create", "discuss", "plan-generate", "plan-check",
		"spec-generate", "compile-workflow"} {
		findCommand(cmds, name).Flags().String("provider", "fake", "provider route")
	}
	findCommand(cmds, "workflow-create").Flags().Bool("yes", false, "assume yes for the dirty-workspace confirmation")
	return cmds
}

// executeExecutionDryRun runs the Execution Dry Run command and renders
// the full preview it pauses the workflow for.
func executeExecutionDryRun(cmd *cobra.Command, deps Dependencies, wf model.WorkflowID) error {
	ctx, stop := commandContext(cmd)
	defer stop()
	a, err := openApplication(ctx, deps)
	if err != nil {
		return err
	}
	out, err := a.Execute(ctx, app.ExecutionDryRunCommand{Workflow: wf})
	if err != nil {
		return err
	}
	renderOutcome(cmd.OutOrStdout(), app.ExecutionDryRunCommand{}, out, deps.Redaction)
	view, err := a.Query(ctx, app.ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		return err
	}
	renderExecutionPreview(cmd.OutOrStdout(), view.(app.ExecutionPreviewView), deps.Redaction)
	return nil
}

// shortRef renders "type:revision" for a prompt.
func shortRef(ref *model.ArtifactRef) string {
	if ref == nil {
		return "-"
	}
	return fmt.Sprintf("%s:%d", ref.Type, ref.Revision)
}

func findCommand(cmds []*cobra.Command, name string) *cobra.Command {
	for _, c := range cmds {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// executeMutation runs one mutation command and renders its Outcome.
func executeMutation(cmd *cobra.Command, deps Dependencies, command app.Command) error {
	ctx, stop := commandContext(cmd)
	defer stop()
	a, err := openApplication(ctx, deps)
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

// readLine reads one scripted stdin line (a full-screen TUI is never
// used; every prompt is a line-oriented y/N question).
func readLine(cmd *cobra.Command) (string, error) {
	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// readDiscussionInput reads the requirement text from scripted stdin
// until the closing "/done" marker or EOF (PRD 需求讨论交互).
func readDiscussionInput(cmd *cobra.Command) (string, error) {
	scanner := bufio.NewScanner(cmd.InOrStdin())
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "/done" {
			break
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
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
// the current working directory as the project root. The GitFlow seam is
// built over the working directory, and the embedded Provider and Prompt
// registries feed the deterministic Fake Adapter (the demo's only
// Adapter until Tasks 14/15; CFLOW_FAKE_SCRIPTS may point at a fixture
// directory for local runs).
func openApplication(ctx context.Context, deps Dependencies) (*app.Application, error) {
	if deps.OpenApplication != nil {
		return deps.OpenApplication(ctx)
	}
	home, err := resolveHome()
	if err != nil {
		return nil, err
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	sup := process.NewSupervisor(process.NewOSAdapter())
	flow, err := gitflow.NewGitFlow(sup, root)
	if err != nil {
		return nil, err
	}
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		return nil, err
	}
	prompts, err := agent.LoadPromptRegistry()
	if err != nil {
		return nil, err
	}
	ad := fake.New(reg)
	if dir := os.Getenv("CFLOW_FAKE_SCRIPTS"); dir != "" {
		if err := ad.LoadDir(dir); err != nil {
			return nil, err
		}
	}
	return app.New(app.Options{
		Home:         home,
		Project:      app.ProjectFor(root),
		CflowVersion: observe.Version,
		Redaction:    deps.Redaction,
		Supervisor:   sup,
		GitFlow:      flow,
		Prompts:      prompts,
		Agent: agent.RuntimeOptions{
			Registry:    reg,
			Redaction:   deps.Redaction,
			Adapters:    map[string]agent.Adapter{"fake": ad},
			EvidenceDir: filepath.Join(home, "evidence"),
		},
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
