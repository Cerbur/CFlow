package cli_test

// The finalization CLI surface (Task 18, PRD 必须提供的 CLI): every
// required command answers --help with exit class 0, a missing full
// command is invalid input (class 2), `apply` and `cleanup --execute`
// return the stable NOT_YET_AVAILABLE finding until their Gate 3 tasks
// land, `inspect task` renders one Task's evidence, `retry` drives the
// dispatch pass, and the report command renders the read model.

import (
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/model"
)

// requiredCommands is the PRD "必须提供的 CLI" surface this task makes
// complete: every command must answer --help with exit class 0.
var requiredCommands = []string{
	"list", "status", "resume", "inspect", "logs", "retry", "pause",
	"cancel", "cleanup", "dry-run", "doctor", "apply",
}

// TestRequiredCommandsHelpExits0: every required command renders its help
// (usage) with exit class 0, never touching CFLOW_HOME.
func TestRequiredCommandsHelpExits0(t *testing.T) {
	for _, name := range requiredCommands {
		t.Run(name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "absent")
			out, code := runCLI(t, home, name, "--help")
			if code != 0 || !strings.Contains(out, "Usage:") {
				t.Fatalf("cflow %s --help: code=%d out=%q", name, code, out)
			}
			if pathExists(home) {
				t.Fatalf("cflow %s --help created CFLOW_HOME", name)
			}
		})
	}
}

// TestBareCflowRendersRootHelp: the bare `cflow` command renders the
// root help and exits 0.
func TestBareCflowRendersRootHelp(t *testing.T) {
	out, code := runCLI(t, filepath.Join(t.TempDir(), "absent"))
	if code != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("bare cflow: code=%d out=%q", code, out)
	}
}

// TestUnknownCommandExits2: an unknown command is invalid input.
func TestUnknownCommandExits2(t *testing.T) {
	_, code := runCLI(t, filepath.Join(t.TempDir(), "absent"), "nosuch")
	if code != 2 {
		t.Fatalf("code=%d, want 2", code)
	}
}

// TestInspectTaskRequiresTaskID (brief case list: "missing full
// command"): `cflow inspect task` without a task id is invalid input.
func TestInspectTaskRequiresTaskID(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLI(t, home, "inspect", "task")
	requireExit(t, out, code, 2)
}

// TestRetryRequiresTaskID: `cflow retry` without a task id is invalid
// input.
func TestRetryRequiresTaskID(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLI(t, home, "retry")
	requireExit(t, out, code, 2)
}

// TestApplyRequiresCompletedWorkflowAtCLI: the protected Apply (PRD 已确
// 认：显式受保护 Apply) is a post-completion delivery; `cflow apply` on a
// workflow that is not completed refuses with INVALID_INPUT (exit class
// 2), never claiming an apply.
func TestApplyRequiresCompletedWorkflowAtCLI(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLIWithError(t, home, "apply", "wf-1")
	requireExit(t, out, code, 2)
	if !strings.Contains(out, "apply requires a completed workflow") {
		t.Fatalf("apply output missing the completed-workflow gate:\n%s", out)
	}
}

// TestCleanupExecuteUnknownManifestIsInvalid: the explicit cleanup
// execution (Task 20) binds the exact Manifest ID/hash; an unknown
// manifest id is invalid input (exit class 2), never a claim that a
// deletion ran.
func TestCleanupExecuteUnknownManifestIsInvalid(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLIWithError(t, home, "cleanup", "wf-1", "--execute", "plan-1")
	requireExit(t, out, code, 2)
	if !strings.Contains(out, "no cleanup manifest with id plan-1") {
		t.Fatalf("cleanup --execute output missing the unknown-manifest gate:\n%s", out)
	}
}

// TestCleanupDryRunOnActiveWorkflowExits3: the cleanup dry-run entry
// behaves exactly like `dry-run`: a non-terminal workflow is a safe
// user-action requirement (exit class 3).
func TestCleanupDryRunOnActiveWorkflowExits3(t *testing.T) {
	home, _ := seedCLIProject(t)
	dbPath := filepath.Join(home, "cflow.db")
	seedWorkflow(t, dbPath, model.WorkflowCommandInput{Kind: model.StartWorkflow, Workflow: "wf-1"})
	out, code := runCLI(t, home, "cleanup", "wf-1")
	requireExit(t, out, code, 3)
}

// TestRetryUnknownTaskExits2: `cflow retry <task-id>` on a workflow whose
// graph carries no such task is invalid input.
func TestRetryUnknownTaskExits2(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLIWithError(t, home, "retry", "task-nosuch", "wf-1")
	requireExit(t, out, code, 2)
}

// TestInspectTaskUnknownExits2: `cflow inspect task <task-id>` of an
// unknown task is invalid input.
func TestInspectTaskUnknownExits2(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLI(t, home, "inspect", "task", "task-nosuch", "wf-1")
	requireExit(t, out, code, 2)
}

// TestReportRendersReadModel: the report command renders the read model
// of one workflow (a PENDING workflow reports a non-terminal result,
// Apply not run) with exit class 0 and never changes state.
func TestReportRendersReadModel(t *testing.T) {
	home, _ := seedCLIProject(t)
	out, code := runCLI(t, home, "report", "wf-1")
	requireExit(t, out, code, 0)
	for _, want := range []string{"CFlow Execution Report", "Result:", "not run", "wf-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report output missing %q:\n%s", want, out)
		}
	}
}

// TestDoctorRendersProviderBindings (ledger obligation from Tasks
// 14/15/16): the read-only doctor renders the provider protocol bindings
// section and the UNKNOWN authentication fact, and never starts a model
// request.
func TestDoctorRendersProviderBindings(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	out, code := runCLI(t, home, "doctor")
	requireExit(t, out, code, 0)
	for _, want := range []string{
		"provider protocol bindings:",
		"authentication: UNKNOWN",
		"codex:",
		"claude:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
}
