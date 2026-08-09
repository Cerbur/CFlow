package cli_test

// CLI tests (design 20): the command surface parses arguments, calls the
// Application, renders redacted projections, and returns the stable exit
// classes through the one central mapping. The fixture seeds a real
// temporary SQLite database; the CLI resolves CFLOW_HOME and the project
// root from the environment.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/cli"
	"cflow.local/cflow/internal/decision"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func cliTempRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	return p
}

// seedCLIProject creates a fresh CFLOW_HOME with one registered Project
// (keyed to the current working directory) and chdirs the test into the
// project root, exactly as the CLI resolves it. It returns the home and
// the Project identity the CLI derives from the working directory.
func seedCLIProject(t *testing.T) (home string, proj app.Project) {
	t.Helper()
	root := cliTempRoot(t)
	t.Chdir(root)
	proj = app.ProjectFor(root)
	home = filepath.Join(cliTempRoot(t), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(home, "cflow.db")
	s, err := store.Open(context.Background(), store.OpenOptions{
		Path: dbPath, Workflow: "wf-1", CflowVersion: "0.0.0-dev",
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	seedProject(t, dbPath, proj.Key)
	seedWorkflow(t, dbPath, model.WorkflowCommandInput{
		Kind: model.CreateWorkflow, Workflow: "wf-1",
		Project: model.ProjectID(proj.Key), TargetBranch: "main", BaseCommit: "abc123",
		WorkspacePath:   "/home/projects/" + proj.Key + "/wf-1/workspace",
		WorkspaceBranch: "cflow/wf-1/workspace",
	})
	return home, proj
}

func seedProject(t *testing.T, dbPath, key string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO projects
		(id, project_key, canonical_path, display_name, git_root, created_at, updated_at, last_opened_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		key, key, "/"+key, key, "/"+key, now, now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// seedWorkflow applies one kernel Decision through the real Store.
func seedWorkflow(t *testing.T, dbPath string, input model.WorkflowCommandInput) {
	t.Helper()
	s, err := store.Open(context.Background(), store.OpenOptions{
		Path: dbPath, Workflow: input.Workflow, CflowVersion: "0.0.0-dev",
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	view, err := s.View(context.Background(), store.StoreQuery{})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if _, err := s.Transact(context.Background(), view.AggregateVersion, func(st model.State) (model.Decision, error) {
		d, err := decision.Decide(st, input)
		if err != nil {
			return model.Decision{}, err
		}
		return completeSeedIdentity(input, d), nil
	}); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
}

// completeSeedIdentity mirrors the Application's identity completion for
// create Decisions (the Kernel validates the input identity but derives
// the mutation from the empty aggregate).
func completeSeedIdentity(input model.Input, d model.Decision) model.Decision {
	wc, ok := input.(model.WorkflowCommandInput)
	if !ok || wc.Kind != model.CreateWorkflow {
		return d
	}
	for i, m := range d.Mutations {
		if wm, ok := m.(model.WorkflowMutation); ok {
			wm.ID = wc.Workflow
			wm.Project = wc.Project
			wm.TargetBranch = wc.TargetBranch
			wm.BaseCommit = wc.BaseCommit
			d.Mutations[i] = wm
		}
	}
	return d
}

// runCLIWithError runs the CLI the way main.go does: the command error is
// rendered to the captured output ("cflow: <err>") because cobra's
// SilenceErrors keeps it out of the streams.
func runCLIWithError(t *testing.T, home string, args ...string) (string, int) {
	t.Helper()
	t.Setenv("CFLOW_HOME", home)
	var out, errBuf bytes.Buffer
	root := cli.NewRoot(cli.Dependencies{Build: fixtureBuild()})
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err := root.Execute()
	msg := out.String() + errBuf.String()
	if err != nil {
		msg += "cflow: " + err.Error() + "\n"
	}
	return msg, cli.ExitCode(err)
}

func requireExit(t *testing.T, out string, code, want int) {
	t.Helper()
	if code != want {
		t.Fatalf("exit code = %d, want %d; output:\n%s", code, want, out)
	}
}

// ---------------------------------------------------------------------------
// read projections
// ---------------------------------------------------------------------------

// TestListOnEmptyHome: list is a pure Project read: it renders an empty
// projection, exits 0, and never creates the database (reads never
// migrate, design 6.1).
func TestListOnEmptyHome(t *testing.T) {
	home := filepath.Join(cliTempRoot(t), "home") // never created
	out, code := runCLI(t, home, "list")
	requireExit(t, out, code, 0)
	if !strings.Contains(out, "no workflows") {
		t.Fatalf("list output missing empty marker:\n%s", out)
	}
	if pathExists(filepath.Join(home, "cflow.db")) {
		t.Fatal("list created the database: reads never migrate")
	}
}

// TestListShowsSeededWorkflow: list enumerates SQLite-authoritative rows;
// no workflow directory is required.
func TestListShowsSeededWorkflow(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLI(t, home, "list")
	requireExit(t, out, code, 0)
	for _, want := range []string{"wf-1", "REQUIREMENT_DISCUSSION", "PENDING"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}
}

// TestStatusShowsSeededWorkflow: status renders the authoritative stage,
// runtime, and branch facts of one workflow.
func TestStatusShowsSeededWorkflow(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLI(t, home, "status", "wf-1")
	requireExit(t, out, code, 0)
	for _, want := range []string{"workflow: wf-1", "REQUIREMENT_DISCUSSION", "runtime: PENDING", "target branch: main"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

// TestStatusUnknownWorkflowExits2: an explicit unknown workflow is invalid
// input.
func TestStatusUnknownWorkflowExits2(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLI(t, home, "status", "nosuch")
	requireExit(t, out, code, 2)
}

// TestLayoutMigrationHeadlessEntryPoints keeps Preview, Prepare, and
// Execute available as explicit headless routes; none may be hidden behind
// an automatic migration during another command.
func TestLayoutMigrationHeadlessEntryPoints(t *testing.T) {
	root := cli.NewRoot(cli.Dependencies{})
	for _, args := range [][]string{
		{"layout-migration", "preview"},
		{"layout-migration", "prepare"},
		{"layout-migration", "execute"},
	} {
		cmd, remaining, err := root.Find(args)
		if err != nil || len(remaining) != 0 || cmd == root {
			t.Fatalf("route %v missing: cmd=%v remaining=%v err=%v", args, cmd, remaining, err)
		}
	}
}

// TestLogsRenderAuthoritativeEvents: logs renders the redacted Event
// sequence with stable Codes and sequence numbers.
func TestLogsRenderAuthoritativeEvents(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLI(t, home, "logs", "wf-1")
	requireExit(t, out, code, 0)
	if !strings.Contains(out, "WORKFLOW_CREATED") {
		t.Fatalf("logs output missing WORKFLOW_CREATED:\n%s", out)
	}
}

// TestInspectRendersAggregate: inspect renders the full aggregate facts of
// one workflow.
func TestInspectRendersAggregate(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLI(t, home, "inspect", "wf-1")
	requireExit(t, out, code, 0)
	for _, want := range []string{"workflow: wf-1", "nodes: 0", "runs: 0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspect output missing %q:\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------------------
// mutations and exit classes
// ---------------------------------------------------------------------------

// TestPauseOnPendingWorkflowExits2: an illegal transition is invalid input
// and no state changes.
func TestPauseOnPendingWorkflowExits2(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLIWithError(t, home, "pause", "wf-1")
	requireExit(t, out, code, 2)
	if !strings.Contains(out, "pause") {
		t.Fatalf("pause error missing guidance:\n%s", out)
	}
}

// TestDryRunOnActiveWorkflowExits3: cleanup dry-run on a non-terminal
// workflow is a safe user-action requirement (CLEANUP_WORKFLOW_NOT_TERMINAL
// -> exit class 3).
func TestDryRunOnActiveWorkflowExits3(t *testing.T) {
	home, _ := seedCLIProject(t)
	dbPath := filepath.Join(home, "cflow.db")
	seedWorkflow(t, dbPath, model.WorkflowCommandInput{Kind: model.StartWorkflow, Workflow: "wf-1"})
	out, code := runCLI(t, home, "dry-run", "wf-1")
	requireExit(t, out, code, 3)
}

// TestCancelOnTerminalWorkflowExits2: a terminal workflow rejects cancel
// (PRD 终止状态保护), mapped centrally as invalid input.
func TestCancelOnTerminalWorkflowExits2(t *testing.T) {
	home, _ := seedCLIProject(t)
	dbPath := filepath.Join(home, "cflow.db")
	seedWorkflow(t, dbPath, model.WorkflowCommandInput{Kind: model.StartWorkflow, Workflow: "wf-1"})
	seedWorkflow(t, dbPath, model.WorkflowCommandInput{Kind: model.CancelWorkflow, Workflow: "wf-1", Reason: "fixture"})
	out, code := runCLI(t, home, "cancel", "wf-1")
	requireExit(t, out, code, 2)
}

// TestResumeRendersOutcome: a seeded PAUSED workflow resumes through the
// Application and renders the new runtime status.
func TestResumeRendersOutcome(t *testing.T) {
	home, _ := seedCLIProject(t)
	dbPath := filepath.Join(home, "cflow.db")
	seedWorkflow(t, dbPath, model.WorkflowCommandInput{Kind: model.StartWorkflow, Workflow: "wf-1"})
	seedWorkflow(t, dbPath, model.WorkflowCommandInput{Kind: model.PauseWorkflow, Workflow: "wf-1"})
	out, code := runCLI(t, home, "resume", "wf-1")
	t.Logf("resume out=%q code=%d", out, code)
	requireExit(t, out, code, 0)
	if !strings.Contains(out, "running") {
		t.Fatalf("resume output missing running status:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// central exit-class mapping (design 20)
// ---------------------------------------------------------------------------

// TestExitCodeFaultMapping: typed Fault categories map centrally to the
// stable exit classes; commands never pick their own codes.
func TestExitCodeFaultMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid input", model.NewFault(model.CodeInvalidInput, "bad input"), 2},
		{"user action required", model.NewFault(model.CodeCleanupWorkflowNotTerminal, "not terminal"), 3},
		{"safety stop", model.NewFault(model.CodeInsecureCFLOWHomePermissions, "posture"), 4},
		{"invariant failure", model.NewFault(model.CodeStateInvariantViolation, "invariant"), 5},
		{"retryable attempt failure", model.NewFault(model.CodeAgentTimeout, "retry"), 5},
		{"wrapped fault keeps class", fmt.Errorf("wrap: %w", model.NewFault(model.CodeCleanupWorkflowNotTerminal, "wrapped")), 3},
		{"database schema too new", model.NewFault(model.CodeDatabaseSchemaTooNew, "too new"), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cli.ExitCode(tc.err); got != tc.want {
				t.Fatalf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestExitCodeContextCanceledIs130: a user interruption that aborts a
// command through its context maps centrally to 130.
func TestExitCodeContextCanceledIs130(t *testing.T) {
	if got := cli.ExitCode(context.Canceled); got != 130 {
		t.Fatalf("ExitCode(context.Canceled) = %d, want 130", got)
	}
	if got := cli.ExitCode(fmt.Errorf("wrapped: %w", context.Canceled)); got != 130 {
		t.Fatalf("wrapped context cancellation = %d, want 130", got)
	}
}

// TestPauseRejectsExtraArgs: extra positional arguments are invalid
// input, mapped centrally.
func TestPauseRejectsExtraArgs(t *testing.T) {
	home, _ := seedCLIProject(t)
	_, code := runCLI(t, home, "pause", "wf-1", "wf-2")
	requireExit(t, "", code, 2)
}
