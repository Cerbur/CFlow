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
				ctx, stop := commandContext(cmd, nil)
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
				ctx, stop := commandContext(cmd, nil)
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
			Args:  cobra.MaximumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd, nil)
				defer stop()
				a, err := openApplication(ctx, deps)
				if err != nil {
					return err
				}
				// `cflow inspect task <task-id> [workflow-id]` renders one
				// Task's delivery evidence (PRD 必须提供的 CLI).
				if len(args) > 0 && args[0] == "task" {
					if len(args) < 2 {
						return model.InvalidInputFault("inspect task requires a task id")
					}
					view, err := a.Query(ctx, app.InspectQuery{Workflow: workflowArg(args[2:])})
					if err != nil {
						return err
					}
					return renderTaskInspect(cmd.OutOrStdout(), view.(app.InspectView),
						model.NodeID(args[1]), deps.Redaction)
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
				ctx, stop := commandContext(cmd, nil)
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
			Short: "cancel a workflow (all evidence is preserved)",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd, nil)
				defer stop()
				a, err := openApplication(ctx, deps)
				if err != nil {
					return err
				}
				wf := workflowArg(args)
				view, err := a.Query(ctx, app.CancelSummaryQuery{Workflow: wf})
				if err != nil {
					return err
				}
				renderCancelSummary(cmd.OutOrStdout(), view.(app.CancelSummaryView), deps.Redaction)
				// Default-negative explicit confirmation (PRD 已确认：Cancel
				// 逻辑终止 step 1): the Agent never initiates or confirms a
				// Cancel.
				fmt.Fprintf(cmd.OutOrStdout(),
					"cancel workflow %s? This stops the workflow permanently; every artifact, session, worktree, branch, commit, and audit ref is preserved. [y/N] ", wf)
				line, err := readLine(cmd)
				if err != nil {
					return err
				}
				if !strings.EqualFold(strings.TrimSpace(line), "y") {
					return model.InvalidInputFault("cancel aborted")
				}
				out, err := a.Execute(ctx, app.CancelWorkflowCommand{Workflow: wf, Reason: "user confirmed cancel"})
				if err != nil {
					return err
				}
				renderOutcome(cmd.OutOrStdout(), app.CancelWorkflowCommand{}, out, deps.Redaction)
				return nil
			},
		},
		{
			Use:   "policy-confirm [workflow-id]",
			Short: "confirm the exact new commit policy after a drift safety stop",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd, nil)
				defer stop()
				a, err := openApplication(ctx, deps)
				if err != nil {
					return err
				}
				wf := workflowArg(args)
				view, err := a.Query(ctx, app.PolicyConfirmationQuery{Workflow: wf})
				if err != nil {
					return err
				}
				pv := view.(app.PolicyConfirmationView)
				renderPolicyConfirmation(cmd.OutOrStdout(), pv, deps.Redaction)
				if !pv.Pending {
					return model.InvalidInputFault("no pending commit policy confirmation")
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"confirm commit policy fingerprint %s (preflight revision %d)? [y/N] ",
					shortHash(pv.Fingerprint), pv.PreflightRevision)
				line, err := readLine(cmd)
				if err != nil {
					return err
				}
				if !strings.EqualFold(strings.TrimSpace(line), "y") {
					return model.InvalidInputFault("commit policy confirmation aborted")
				}
				out, err := a.Execute(ctx, app.CommitPolicyConfirmCommand{
					Workflow: wf, PreflightRevision: pv.PreflightRevision,
					PreflightHash: pv.PreflightHash, Fingerprint: pv.Fingerprint,
				})
				if err != nil {
					return err
				}
				renderOutcome(cmd.OutOrStdout(), app.CommitPolicyConfirmCommand{}, out, deps.Redaction)
				return nil
			},
		},
		{
			Use:   "replacement-preview [workflow-id]",
			Short: "generate the replacement execution of a quarantined workflow",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd, nil)
				defer stop()
				a, err := openApplication(ctx, deps)
				if err != nil {
					return err
				}
				wf := workflowArg(args)
				if _, err := a.Execute(ctx, app.ReplacementPreviewCommand{Workflow: wf}); err != nil {
					return err
				}
				view, err := a.Query(ctx, app.ReplacementPreviewQuery{Workflow: wf})
				if err != nil {
					return err
				}
				renderReplacementPreview(cmd.OutOrStdout(), view.(app.ReplacementPreviewView), deps.Redaction)
				return nil
			},
		},
		{
			Use:   "replacement-approve [workflow-id]",
			Short: "approve the unified replacement execution approval",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd, nil)
				defer stop()
				a, err := openApplication(ctx, deps)
				if err != nil {
					return err
				}
				wf := workflowArg(args)
				view, err := a.Query(ctx, app.ReplacementPreviewQuery{Workflow: wf})
				if err != nil {
					return err
				}
				pv := view.(app.ReplacementPreviewView)
				renderReplacementPreview(cmd.OutOrStdout(), pv, deps.Redaction)
				if len(pv.Quarantines) == 0 || pv.Manifest.Hash == "" {
					return model.InvalidInputFault("no replacement preview to approve; run replacement-preview first")
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"approve the replacement execution binding %d quarantines and manifest %s? [y/N] ",
					len(pv.Quarantines), shortHash(pv.Manifest.Hash))
				line, err := readLine(cmd)
				if err != nil {
					return err
				}
				if !strings.EqualFold(strings.TrimSpace(line), "y") {
					return model.InvalidInputFault("replacement approval aborted")
				}
				ids := make([]string, 0, len(pv.Quarantines))
				for _, q := range pv.Quarantines {
					ids = append(ids, q.ID)
				}
				out, err := a.Execute(ctx, app.ApproveReplacementCommand{
					Workflow:             wf,
					PlanHash:             pv.PlanHash,
					SpecHashes:           pv.SpecHashes,
					CatalogHash:          pv.CatalogHash,
					WorkflowHash:         pv.WorkflowHash,
					RoutingHash:          pv.RoutingHash,
					BudgetHash:           pv.BudgetHash,
					PreflightRevision:    pv.Preflight.Revision,
					PreflightHash:        pv.Preflight.EvidenceHash,
					Fingerprint:          pv.NewFingerprint,
					QuarantineIDs:        ids,
					SupersededApprovalID: pv.SupersededApprovalID,
					ManifestRevision:     pv.Manifest.Revision,
					ManifestHash:         pv.Manifest.Hash,
				})
				if err != nil {
					return err
				}
				renderOutcome(cmd.OutOrStdout(), app.ApproveReplacementCommand{}, out, deps.Redaction)
				return nil
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
			Use:   "retry <task-id> [workflow-id]",
			Short: "drive one dispatch pass for a task with a ready retry",
			Args:  cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return executeMutation(cmd, deps, app.RetryCommand{
					Workflow: workflowArg(args[1:]), Node: model.NodeID(args[0]),
				})
			},
		},
		{
			Use:   "cleanup [workflow-id]",
			Short: "produce the cleanup dry-run manifest (execute lands with a later task)",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				// The cleanup dry-run entry (PRD 必须提供的 CLI); the
				// exact-confirmation execute protocol lands with Task 20
				// and returns the stable NOT_YET_AVAILABLE finding.
				if execute, _ := cmd.Flags().GetString("execute"); execute != "" {
					return model.NewFault(model.CodeNotYetAvailable,
						"cleanup --execute is not yet available; the exact-confirmation cleanup protocol lands with a later task")
				}
				return executeMutation(cmd, deps, app.DryRunCommand{Workflow: workflowArg(args)})
			},
		},
		{
			Use:   "apply [workflow-id]",
			Short: "apply preflight entry (the protected apply lands with a later task)",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				// The Gate 3 protected Apply (Target compare-and-swap,
				// design 18) lands with a later task; until then the
				// command returns the stable NOT_YET_AVAILABLE finding
				// and never claims an apply.
				return model.NewFault(model.CodeNotYetAvailable,
					"the protected apply to the target branch is not yet available; the workflow stays on the integration branch")
			},
		},
		{
			Use:   "report [workflow-id]",
			Short: "render the final execution report read model",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := commandContext(cmd, nil)
				defer stop()
				a, err := openApplication(ctx, deps)
				if err != nil {
					return err
				}
				view, err := a.Query(ctx, app.ReportQuery{Workflow: workflowArg(args), Build: deps.Build})
				if err != nil {
					return err
				}
				fmt.Fprint(cmd.OutOrStdout(), view.(app.ReportView).Markdown)
				return nil
			},
		},
		{
			Use:   "workflow-create <name>",
			Short: "create a workflow for the current git repository",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				provider, _ := cmd.Flags().GetString("provider")
				assumeYes, _ := cmd.Flags().GetBool("yes")
				ctx, stop := commandContext(cmd, nil)
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
				ctx, stop := commandContext(cmd, nil)
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
				ctx, stop := commandContext(cmd, nil)
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
				ctx, stop := commandContext(cmd, nil)
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
				ctx, stop := commandContext(cmd, nil)
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
	findCommand(cmds, "cleanup").Flags().String("execute", "", "execute a produced cleanup manifest (not yet available)")
	return cmds
}

// executeExecutionDryRun runs the Execution Dry Run command and renders
// the full preview it pauses the workflow for.
func executeExecutionDryRun(cmd *cobra.Command, deps Dependencies, wf model.WorkflowID) error {
	ctx, stop := commandContext(cmd, nil)
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

// executeMutation runs one mutation command and renders its Outcome. A
// user interruption (the first Ctrl+C) is translated into the controlled
// stop: the in-flight command aborted; when the Workflow is still RUNNING
// (a planning command whose Session could not settle, or an approval-gate
// pause), the pause persists CONTROLLED_STOP_REQUESTED, closes dispatch,
// and stops the managed processes (PRD 已确认：Ctrl+C 两阶段有限停止 steps
// 1 and 10). The interruption itself remains the authoritative outcome
// (exit class 130).
func executeMutation(cmd *cobra.Command, deps Dependencies, command app.Command) error {
	a, err := openApplication(cmd.Context(), deps)
	if err != nil {
		return err
	}
	ctx, stop := commandContext(cmd, a)
	defer stop()
	wf := workflowOf(command)
	out, err := a.Execute(ctx, command)
	if err != nil {
		if ctx.Err() != nil && wf != "" {
			// The controlled-stop translation: best-effort; the
			// interruption is the outcome either way.
			_, _ = a.Execute(context.Background(), app.PauseWorkflowCommand{Workflow: wf})
		}
		return err
	}
	renderOutcome(cmd.OutOrStdout(), command, out, deps.Redaction)
	return nil
}

// workflowOf is the workflow identity a mutation command addresses ("" for
// project-level commands).
func workflowOf(command app.Command) model.WorkflowID {
	switch c := command.(type) {
	case app.DiscussRequirementCommand:
		return c.Workflow
	case app.GeneratePlanCommand:
		return c.Workflow
	case app.CheckPlanCommand:
		return c.Workflow
	case app.ApprovePlanCommand:
		return c.Workflow
	case app.StartWorkflowCommand:
		return c.Workflow
	case app.PauseWorkflowCommand:
		return c.Workflow
	case app.ResumeWorkflowCommand:
		return c.Workflow
	case app.CancelWorkflowCommand:
		return c.Workflow
	case app.DryRunCommand:
		return c.Workflow
	case app.GenerateSpecsCommand:
		return c.Workflow
	case app.CompileWorkflowCommand:
		return c.Workflow
	case app.ExecutionDryRunCommand:
		return c.Workflow
	case app.ApproveExecutionCommand:
		return c.Workflow
	case app.DispatchCommand:
		return c.Workflow
	case app.CommitPolicyConfirmCommand:
		return c.Workflow
	case app.ReplacementPreviewCommand:
		return c.Workflow
	case app.ApproveReplacementCommand:
		return c.Workflow
	case app.RetryCommand:
		return c.Workflow
	}
	return ""
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

// commandContext wires the two-phase interruption (Task 17, PRD 已确认：
// Ctrl+C 两阶段有限停止): the first SIGINT cancels the command context so
// in-flight effects abort at the next context boundary; the second SIGINT
// escalates a running controlled stop to the force-kill phase through the
// Application (a nil Application makes the second signal a no-op). The
// central exit mapping turns the cancellation into exit class 130.
func commandContext(cmd *cobra.Command, a *app.Application) (context.Context, func()) {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	translateInterrupts(ctx, cancel, done, sig, func() {
		if a != nil {
			a.EscalateStop()
		}
	}, func() { signal.Stop(sig) })
	return ctx, func() {
		signal.Stop(sig)
		close(done)
		cancel()
	}
}

// translateInterrupts is the two-phase SIGINT translation (Task 7
// obligation, PRD 已确认：Ctrl+C 两阶段有限停止): the first signal cancels
// the command context (in-flight effects abort at the next context
// boundary); the second signal escalates the running controlled stop to
// the force-kill phase. done closes when the command returned (the
// translation then drains one racing signal and finishes); stop
// unregisters the signal source.
func translateInterrupts(ctx context.Context, cancel context.CancelFunc, done <-chan struct{}, sig <-chan os.Signal, escalate, stop func()) {
	go func() {
		defer stop()
		select {
		case <-sig:
			// First Ctrl+C: abort in-flight work.
			cancel()
		case <-ctx.Done():
			return // the command finished without a signal
		}
		// The second Ctrl+C escalates the controlled stop to the
		// force-kill phase whenever it arrives during the stop; a signal
		// racing the command's return is drained, never lost.
		select {
		case <-sig:
			escalate()
		case <-done:
			select {
			case <-sig:
				escalate()
			default:
			}
		}
	}()
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
