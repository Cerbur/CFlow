package integration

// The Release Fault Matrix harness (Task 21, brief Step 2): TestReleaseFaultMatrix
// reads tests/testdata/faults/matrix.yaml (the executable fault matrix), and for
// every row dispatches to the injector registered under the row's inject point.
// A row whose inject point has no registered injector FAILS the harness listing
// the row id — unsupported inject points are never silently skipped. Each
// injector begins with a fresh CFLOW_HOME and Git fixture, injects the fault
// through a test-only constructor dependency, and reports the externally
// observable facts after restart; the harness asserts they agree with the
// matrix row (one stable code, disposition, retry charge, dispatch behavior,
// and persistent evidence per row). The release binary exposes no environment
// flag, CLI flag, debug endpoint, or mutable configuration that enables fault
// injection: every injector lives in this _test package or reaches seams only
// through in-package test constructors.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// matrixRow is one row of tests/testdata/faults/matrix.yaml. The row carries
// exactly the fields of the Task 21 brief: id, setup, inject, expected_code,
// expected_disposition, retry_charge, dispatch, evidence.
type matrixRow struct {
	ID           string   `yaml:"id"`
	Setup        string   `yaml:"setup"`
	Inject       string   `yaml:"inject"`
	ExpectedCode string   `yaml:"expected_code"`
	Disposition  string   `yaml:"expected_disposition"`
	RetryCharge  bool     `yaml:"retry_charge"`
	Dispatch     string   `yaml:"dispatch"`
	Evidence     []string `yaml:"evidence"`
}

// rowResult is the deterministic probe of one matrix row: the observable
// facts the injector measured after the restart.
type rowResult struct {
	Code        string
	Disposition string
	RetryCharge bool
	Dispatch    string
	Evidence    map[string]bool
}

// evidenceOf builds the evidence set of one rowResult.
func evidenceOf(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// faultInjector is one deterministic matrix probe: given the row it drives
// the setup + injected point and returns the observable facts.
type faultInjector func(t *testing.T, row matrixRow) rowResult

// faultInjectors maps every matrix inject point to its registered probe.
// Injectors are declared in fault_matrix_rows_test.go (same package), so
// the release binary can never carry them.
var faultInjectors = map[string]faultInjector{}

// loadMatrix parses tests/testdata/faults/matrix.yaml from the package
// working directory (go test runs with the package directory as cwd).
func loadMatrix(t *testing.T) []matrixRow {
	t.Helper()
	path := filepath.Join("..", "testdata", "faults", "matrix.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fault matrix: %v", err)
	}
	var rows []matrixRow
	if err := yaml.Unmarshal(data, &rows); err != nil {
		t.Fatalf("parse fault matrix: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("fault matrix has no rows")
	}
	for _, r := range rows {
		if r.ID == "" || r.Inject == "" {
			t.Fatalf("matrix row is missing id or inject: %+v", r)
		}
		switch r.Dispatch {
		case "open", "closed", "closed_until_reconciled":
		default:
			t.Fatalf("row %s has an unknown dispatch %q", r.ID, r.Dispatch)
		}
	}
	return rows
}

// TestReleaseFaultMatrix runs the complete deterministic release matrix:
// every row through its registered injector, with the observed facts
// compared against the table. Unhandled rows (no registered injector) FAIL
// the harness listing every row id — the matrix never silently skips an
// unsupported inject point. The -count=20 verification proves each row
// settles with one stable disposition.
func TestReleaseFaultMatrix(t *testing.T) {
	rows := loadMatrix(t)

	seen := make(map[string]bool, len(rows))
	var unhandled []string
	for _, row := range rows {
		if seen[row.ID] {
			t.Fatalf("matrix has a duplicate row id %q", row.ID)
		}
		seen[row.ID] = true
		if faultInjectors[row.Inject] == nil {
			unhandled = append(unhandled, row.ID)
		}
	}
	if len(unhandled) > 0 {
		sort.Strings(unhandled)
		t.Fatalf("fault matrix rows with no registered injector (never silently skipped):\n  %s",
			strings.Join(unhandled, "\n  "))
	}

	for _, row := range rows {
		row := row
		t.Run(row.ID, func(t *testing.T) {
			got := faultInjectors[row.Inject](t, row)
			assertRow(t, row, got)
		})
	}
}

// assertRow compares one probe's observed facts against its matrix row.
func assertRow(t *testing.T, row matrixRow, got rowResult) {
	t.Helper()
	if row.ExpectedCode != "" {
		if got.Code != row.ExpectedCode {
			t.Fatalf("code = %q, want %q (disposition %q)", got.Code, row.ExpectedCode, got.Disposition)
		}
	} else if got.Code != "" {
		t.Fatalf("row expects no fault, observed code %q (disposition %q)", got.Code, got.Disposition)
	}
	if got.Disposition != row.Disposition {
		t.Fatalf("disposition = %q, want %q", got.Disposition, row.Disposition)
	}
	if got.RetryCharge != row.RetryCharge {
		t.Fatalf("retry charge = %v, want %v", got.RetryCharge, row.RetryCharge)
	}
	if got.Dispatch != row.Dispatch {
		t.Fatalf("dispatch = %q, want %q", got.Dispatch, row.Dispatch)
	}
	for _, key := range row.Evidence {
		if !got.Evidence[key] {
			t.Fatalf("missing evidence %q (have %v)", key, sortedKeys(got.Evidence))
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
