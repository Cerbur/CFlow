// Package e2e: the CFlow Self-Dogfood acceptance (Task 22, brief Step 4,
// PRD 第二层：CFlow 自举 Dogfood, design 23). TestDogfood is the opt-in real
// self-Dogfood run: it builds the candidate binary from the current source,
// copies it to an immutable path OUTSIDE the CFlow repository, hashes it,
// and runs a CFlow-managed workflow against the CFlow repository itself
// with a bounded documentation-or-test-only requirement, at least two Tasks
// routed across Codex and Claude, independent Reviews, full deterministic
// Verification, serial Integration, the final report, and the protected
// Apply.
//
// It NEVER executes without CFLOW_DOGFOOD_REAL=1, because it costs real
// model requests, runs with the providers' default permissions, and applies
// the dogfood output to the target branch of the CFlow repository itself —
// the user must approve the exact Dry Run, the provider routes/models/
// budgets, the default-permission trust boundary, the network/cost, the
// bounded requirement, and the Apply target BEFORE the gate is set. Its
// default (off) behavior is a safe skip.
//
// TestDogfoodPreflight is the offline deterministic dogfood-equivalent the
// Gate 3 suite runs: it builds a real fixture — a real Git target
// workspace, the original developer workspace, and the candidate binary
// copied to an immutable path outside the target with its SHA-256 pinned —
// and proves observe.ValidateDogfoodPreflight accepts only genuine,
// uncontaminated preflight facts and rejects every violation (dirty source,
// binary inside the repository, target contamination, unbounded or
// unapproved requirements, missing cross-provider routes) with
// CodeEvidenceSubjectChanged. The workflow-driving machinery the real run
// exercises (planning, cross-provider dispatch, independent reviews,
// deterministic Verification, Integration merges, the final report, and the
// protected Apply) is itself covered offline by
// TestDialectEquivalentCrossProvider and the apply integration tests.
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/claude"
	"cflow.local/cflow/internal/agent/codex"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/process"
)

// ---------------------------------------------------------------------------
// the offline deterministic dogfood-equivalent (Gate 3 suite)
// ---------------------------------------------------------------------------

// TestDogfoodPreflight proves the deterministic dogfood harness against a
// real fixture: a real Git target workspace, the original developer
// workspace, and the running test binary copied to an immutable path
// outside the target with its SHA-256 pinned. The preflight accepts only
// genuine facts and rejects every contamination with
// CodeEvidenceSubjectChanged.
func TestDogfoodPreflight(t *testing.T) {
	canon, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonical temp dir: %v", err)
	}
	target := filepath.Join(canon, "target-repo")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	git(target, "init", "-q", "-b", "main")
	git(target, "config", "user.name", "Dogfood Tester")
	git(target, "config", "user.email", "dogfood@example.com")
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("# target\n"), 0o644); err != nil {
		t.Fatalf("write target README: %v", err)
	}
	git(target, "add", "-A")
	git(target, "commit", "-q", "-m", "target base")
	sourceCommit := strings.TrimSpace(git(target, "rev-parse", "HEAD"))

	original := filepath.Join(canon, "original-workspace")
	if err := os.MkdirAll(original, 0o700); err != nil {
		t.Fatalf("mkdir original: %v", err)
	}

	// The candidate binary copied to an immutable path outside the target
	// repository (design 23: "The Dogfood binary is copied outside the
	// target repository and treated as immutable").
	immutableDir := filepath.Join(canon, "immutable")
	if err := os.MkdirAll(immutableDir, 0o700); err != nil {
		t.Fatalf("mkdir immutable: %v", err)
	}
	binPath := filepath.Join(immutableDir, "cflow")
	src, err := os.ReadFile(os.Args[0]) // the running test binary is the candidate build
	if err != nil {
		t.Fatalf("read running binary: %v", err)
	}
	if err := os.WriteFile(binPath, src, 0o555); err != nil {
		t.Fatalf("copy candidate binary: %v", err)
	}
	if err := os.Chmod(immutableDir, 0o500); err != nil {
		t.Fatalf("seal immutable dir: %v", err)
	}
	// Restore the sealed permissions so the temporary directory can be
	// removed at test cleanup.
	t.Cleanup(func() { os.Chmod(immutableDir, 0o700) })
	sum := sha256.Sum256(src)
	binarySHA256 := hex.EncodeToString(sum[:])

	valid := observe.DogfoodPreflight{
		BinaryPath:          binPath,
		BinarySHA256:        binarySHA256,
		SourceCommit:        sourceCommit,
		SourceClean:         true,
		TargetWorkspace:     target,
		OriginalWorkspace:   original,
		RequirementBound:    "docs-or-tests-only",
		RequirementApproved: true,
		Routes:              []string{"codex", "claude"},
	}
	if err := observe.ValidateDogfoodPreflight(valid); err != nil {
		t.Fatalf("valid dogfood preflight rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*observe.DogfoodPreflight)
	}{
		{"dirty candidate source", func(p *observe.DogfoodPreflight) { p.SourceClean = false }},
		{"binary hash mismatch", func(p *observe.DogfoodPreflight) { p.BinarySHA256 = strings.Repeat("0", 64) }},
		{"binary inside the target repository", func(p *observe.DogfoodPreflight) {
			p.BinaryPath = filepath.Join(p.TargetWorkspace, "bin", "cflow")
		}},
		{"target is the original developer workspace", func(p *observe.DogfoodPreflight) {
			p.TargetWorkspace = p.OriginalWorkspace
		}},
		{"requirement not approved", func(p *observe.DogfoodPreflight) { p.RequirementApproved = false }},
		{"unbounded requirement", func(p *observe.DogfoodPreflight) { p.RequirementBound = "" }},
		{"single provider route", func(p *observe.DogfoodPreflight) { p.Routes = []string{"codex"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.mutate(&p)
			err := observe.ValidateDogfoodPreflight(p)
			code, ok := model.CodeOf(err)
			if !ok || code != model.CodeEvidenceSubjectChanged {
				t.Fatalf("fault = %v, want %s", err, model.CodeEvidenceSubjectChanged)
			}
		})
	}
}

// TestDogfoodCandidateMatchesReleaseBuild proves the dogfood candidate is
// built with the exact release metadata linker flags the release pipeline
// stamps (scripts/check-cross-build.sh, scripts/gate3.sh): two identical-
// ldflags builds are byte-identical, so the immutable dogfood candidate
// SHA-256 equals the release candidate's for the same source and toolchain
// (the Gate 3 validation rejects a different binary with
// EVIDENCE_SUBJECT_CHANGED), and the built candidate's version output
// carries the pinned source Commit and registry hashes — the assertion the
// real dogfood run relies on in a git worktree, where the Go toolchain
// stamps no VCS metadata.
func TestDogfoodCandidateMatchesReleaseBuild(t *testing.T) {
	repo := dogfoodRepoRoot(t)
	ldflags := dogfoodReleaseLDFlags(t, repo)
	binA := dogfoodBuildWithFlags(t, repo, t.TempDir(), ldflags)
	binB := dogfoodBuildWithFlags(t, repo, t.TempDir(), ldflags)
	sumA, sumB := sha256.Sum256(readBinary(t, binA)), sha256.Sum256(readBinary(t, binB))
	if hex.EncodeToString(sumA[:]) != hex.EncodeToString(sumB[:]) {
		t.Fatalf("identical release ldflags produced different candidate hashes %x vs %x", sumA, sumB)
	}
	ver, err := exec.Command(binA, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("candidate version: %v", err)
	}
	sourceCommit := strings.TrimSpace(git(repo, "rev-parse", "HEAD"))
	for _, want := range []string{
		sourceCommit,
		"migration=" + metadataValue(t, repo, "migration"),
		"artifact=" + metadataValue(t, repo, "artifact"),
		"provider=" + metadataValue(t, repo, "provider"),
		"prompt=" + metadataValue(t, repo, "prompt"),
	} {
		if !strings.Contains(string(ver), want) {
			t.Fatalf("candidate version output misses %q:\n%s", want, ver)
		}
	}
}

// readBinary reads one built candidate binary.
func readBinary(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	return body
}

// metadataValue returns one release-metadata value (migration, artifact,
// provider, prompt, schema_version), resolved from the repository root.
func metadataValue(t *testing.T, repo, key string) string {
	t.Helper()
	return releaseMetadataValues(t, repo)[key]
}

// releaseMetadataValues runs scripts/release-metadata from the repository
// root (the same source the release pipeline uses) and returns its
// key=value map.
func releaseMetadataValues(t *testing.T, repo string) map[string]string {
	t.Helper()
	values := map[string]string{}
	cmd := exec.Command("go", "run", "./scripts/release-metadata")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("release metadata: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if key, val, ok := strings.Cut(line, "="); ok {
			values[key] = val
		}
	}
	return values
}

// ---------------------------------------------------------------------------
// the opt-in real self-Dogfood (brief Step 4; approval-gated)
// ---------------------------------------------------------------------------

// dogfoodRepoRoot resolves the CFlow repository root (the dogfood target)
// from the test working directory.
func dogfoodRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		t.Fatalf("resolve repository root: %v (%s)", err, out)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		t.Fatalf("empty repository root")
	}
	return root
}

// dogfoodRequireClean forbids a dogfood run over uncommitted project source
// (PRD 已确认：Dogfood 不针对未提交工作区).
func dogfoodRequireClean(t *testing.T, repo string) {
	t.Helper()
	if out := strings.TrimSpace(git(repo, "status", "--porcelain")); out != "" {
		t.Fatalf("the CFlow repository is not Git-clean; a dogfood run requires a committed workspace:\n%s", out)
	}
}

// dogfoodReleaseLDFlags derives the release metadata linker flags exactly as
// the release pipeline stamps them (scripts/check-cross-build.sh and
// scripts/gate3.sh): the version, the source Commit, the dirty flag, the
// applied schema version, and the migration/Artifact/Provider/prompt
// registry hashes from scripts/release-metadata. The dogfood candidate is
// built with these identical flags so its immutable SHA-256 equals the
// release candidate's for the same source and toolchain — otherwise the
// Gate 3 validation would reject the dogfood evidence with
// EVIDENCE_SUBJECT_CHANGED (a different binary hash), and in a git worktree
// (where the Go toolchain stamps no VCS metadata) the version output would
// report an unset source Commit.
func dogfoodReleaseLDFlags(t *testing.T, repo string) string {
	t.Helper()
	values := releaseMetadataValues(t, repo)
	version := os.Getenv("CFLOW_RELEASE_VERSION")
	if version == "" {
		version = "0.1.0-demo3"
	}
	dirty := 0
	if out := strings.TrimSpace(git(repo, "status", "--porcelain")); out != "" {
		dirty = 1
	}
	sourceCommit := strings.TrimSpace(git(repo, "rev-parse", "HEAD"))
	return fmt.Sprintf("-X cflow.local/cflow/internal/observe.Version=%s"+
		" -X cflow.local/cflow/internal/observe.SourceCommit=%s"+
		" -X cflow.local/cflow/internal/observe.sourceDirty=%d"+
		" -X cflow.local/cflow/internal/observe.schemaVersion=%s"+
		" -X cflow.local/cflow/internal/observe.MigrationHash=%s"+
		" -X cflow.local/cflow/internal/observe.ArtifactHash=%s"+
		" -X cflow.local/cflow/internal/observe.ProviderHash=%s"+
		" -X cflow.local/cflow/internal/observe.PromptHash=%s",
		version, sourceCommit, dirty, values["schema_version"], values["migration"],
		values["artifact"], values["provider"], values["prompt"])
}

// dogfoodBuildCandidate builds the candidate binary from the current source
// (CGO disabled, trimmed) with the release metadata linker flags, into a
// directory outside the repository.
func dogfoodBuildCandidate(t *testing.T, repo, dir string) string {
	t.Helper()
	return dogfoodBuildWithFlags(t, repo, dir, dogfoodReleaseLDFlags(t, repo))
}

// dogfoodBuildWithFlags builds the candidate binary with the given linker
// flags into a directory outside the repository.
func dogfoodBuildWithFlags(t *testing.T, repo, dir, ldflags string) string {
	t.Helper()
	bin := filepath.Join(dir, "cflow")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", bin, "./cmd/cflow")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build candidate: %v\n%s", err, out)
	}
	return bin
}

// dogfoodImmutableCopy copies the candidate to a read-only path outside the
// target repository and returns the path and the pinned SHA-256.
func dogfoodImmutableCopy(t *testing.T, repo, bin, dir string) (string, string) {
	t.Helper()
	immutableDir := filepath.Join(dir, "immutable")
	if err := os.MkdirAll(immutableDir, 0o700); err != nil {
		t.Fatalf("mkdir immutable: %v", err)
	}
	dst := filepath.Join(immutableDir, "cflow")
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if err := os.WriteFile(dst, body, 0o555); err != nil {
		t.Fatalf("copy candidate to immutable path: %v", err)
	}
	if err := os.Chmod(immutableDir, 0o500); err != nil {
		t.Fatalf("seal immutable dir: %v", err)
	}
	// Restore the sealed permissions so the temporary directory can be
	// removed at test cleanup.
	t.Cleanup(func() { os.Chmod(immutableDir, 0o700) })
	sum := sha256.Sum256(body)
	return dst, hex.EncodeToString(sum[:])
}

// dogfoodApp builds the Application over the dogfood repository whose codex
// and claude adapters are the REAL dialect adapters over a fresh OS
// supervisor (the production executables): the Dry Run records their
// detection facts, the dispatch CAS re-detects and compares the same
// identities, and every Start/Resume launches the real CLI. No Fake adapter
// is registered under the codex or claude names on this path. The planning
// phases run through the deterministic Fake adapter (dogfoodPlanningApp);
// the Execution Dry Run, the Execution Approval, the dispatch, and the
// protected Apply run through this real App.
func dogfoodApp(t *testing.T, repo, home string) *app.Application {
	t.Helper()
	sup := process.NewSupervisor(process.NewOSAdapter())
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatalf("provider registry: %v", err)
	}
	prompts, err := agent.LoadPromptRegistry()
	if err != nil {
		t.Fatalf("prompt registry: %v", err)
	}
	codexBinding, err := reg.Select("codex")
	if err != nil {
		t.Fatalf("codex binding: %v", err)
	}
	claudeBinding, err := reg.Select("claude")
	if err != nil {
		t.Fatalf("claude binding: %v", err)
	}
	flow, err := gitflow.NewGitFlow(sup, repo)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	a, err := app.New(app.Options{
		Home:         home,
		Project:      app.ProjectFor(repo),
		CflowVersion: "0.1.0-demo3",
		Now:          func() time.Time { return time.Unix(1700000000, 0).UTC() },
		IDs:          model.SequentialIDSource(),
		Supervisor:   sup,
		GitFlow:      flow,
		Prompts:      prompts,
		Agent: agent.RuntimeOptions{
			Registry:    reg,
			Adapters:    map[string]agent.Adapter{"codex": codex.New(sup, codexBinding), "claude": claude.New(sup, claudeBinding)},
			EvidenceDir: filepath.Join(home, "evidence"),
		},
	})
	if err != nil {
		t.Fatalf("new application: %v", err)
	}
	return a
}

// dogfoodPlanningApp builds the Application over the dogfood repository with
// the deterministic Fake Adapter for the planning phases (discussion through
// compilation). The fixture scripts run with the fake dialect; the real App
// (dogfoodApp) runs the Execution Dry Run, Approval, dispatch, and Apply.
func dogfoodPlanningApp(t *testing.T, repo, home string, scripts ...string) *app.Application {
	t.Helper()
	sup := process.NewSupervisor(process.NewOSAdapter())
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatalf("provider registry: %v", err)
	}
	prompts, err := agent.LoadPromptRegistry()
	if err != nil {
		t.Fatalf("prompt registry: %v", err)
	}
	ad := fake.New(reg)
	for _, s := range scripts {
		if err := ad.LoadScript([]byte(s)); err != nil {
			t.Fatalf("load fake script: %v", err)
		}
	}
	flow, err := gitflow.NewGitFlow(sup, repo)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	a, err := app.New(app.Options{
		Home:         home,
		Project:      app.ProjectFor(repo),
		CflowVersion: "0.1.0-demo3",
		Now:          func() time.Time { return time.Unix(1700000000, 0).UTC() },
		IDs:          model.SequentialIDSource(),
		Supervisor:   sup,
		GitFlow:      flow,
		Prompts:      prompts,
		Agent: agent.RuntimeOptions{
			Registry:    reg,
			Adapters:    map[string]agent.Adapter{"fake": ad},
			EvidenceDir: filepath.Join(home, "evidence"),
		},
	})
	if err != nil {
		t.Fatalf("new application: %v", err)
	}
	return a
}

// dogfoodRequirement is the bounded documentation-or-test-only requirement
// approved by the user before any dogfood run (PRD 已确认：只读或文档/测试受限
// 要求).
const dogfoodRequirement = "Add a short documentation-only note under docs/ that describes the local-first boundary, and add one bounded Go test under internal/observe that asserts the build identity renders. Do not change production source files."

// dogfoodSpecs routes two independent Tasks across codex and claude with
// write scopes strictly inside docs/ and internal/observe test files, each
// requiring the deterministic verify command and an independent review.
//
// The approval process must ensure the CFlow repository Base Commit carries
// the deterministic verification wrappers the Catalog discovers (scripts/
// verify.sh for task verification, scripts/final-verify.sh for the final
// verify over the full integration range, and scripts/apply-verify.sh for
// the apply verification) — these are CFlow's own scripts the dogfood run
// then executes, bounded to pass on the docs/tests-only output.
const dogfoodSpecs = `{"id":"s01","goal":"add a documentation-only note under docs/ describing the local-first boundary","depends_on":[],"write_scope":["docs/cflow-local-first.md"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"],"review_required":true},"route":{"provider":"codex","model":"default","budget":10},"timeout_seconds":300,"max_retry":2}
{"id":"s02","goal":"add one bounded build-identity render test under internal/observe","depends_on":[],"write_scope":["internal/observe/build_render_test.go"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"],"review_required":true},"route":{"provider":"claude","model":"default","budget":10},"timeout_seconds":300,"max_retry":2}`

// writeDogfoodEvidence writes the self-Dogfood evidence file (observe.
// ReleaseEvidenceFile, kind "dogfood") that the Gate 3 validation consumes.
// The path comes from CFLOW_DOGFOOD_EVIDENCE; without it the run still
// completes but records no on-disk evidence.
func writeDogfoodEvidence(t *testing.T, path, binarySHA256, sourceCommit, target, original, wfHash, oldHead, newHead string) {
	t.Helper()
	if path == "" {
		return
	}
	ev := observe.ReleaseEvidenceFile{
		Kind:              "dogfood",
		BinarySHA256:      binarySHA256,
		SourceCommit:      sourceCommit,
		Reviewed:          true,
		TargetWorkspace:   target,
		OriginalWorkspace: original,
		RequirementHash:   sha256HexString(dogfoodRequirement),
		Routes:            []string{"codex", "claude"},
		WorkflowHash:      wfHash,
		ApplyOldHead:      oldHead,
		ApplyNewHead:      newHead,
	}
	body, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		t.Fatalf("marshal dogfood evidence: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write dogfood evidence: %v", err)
	}
}

// dogfoodFixture drives one dogfood workflow over the CFlow repository: the
// deterministic planning phases through the Fake App and the real execution
// through the real App.
type dogfoodFixture struct {
	t    *testing.T
	repo string
	home string
}

func (fx *dogfoodFixture) planningApp(scripts ...string) *app.Application {
	fx.t.Helper()
	return dogfoodPlanningApp(fx.t, fx.repo, fx.home, scripts...)
}

func (fx *dogfoodFixture) realApp() *app.Application {
	fx.t.Helper()
	return dogfoodApp(fx.t, fx.repo, fx.home)
}

func (fx *dogfoodFixture) createWorkflow() model.WorkflowID {
	fx.t.Helper()
	out, err := fx.planningApp().Execute(context.Background(),
		app.CreateWorkflowCommand{Name: "dogfood", Provider: "fake"})
	if err != nil {
		fx.t.Fatalf("create dogfood workflow: %v", err)
	}
	return out.Workflow
}

func (fx *dogfoodFixture) planView(wf model.WorkflowID) app.PlanView {
	fx.t.Helper()
	view, err := fx.planningApp().Query(context.Background(), app.PlanQuery{Workflow: wf})
	if err != nil {
		fx.t.Fatalf("plan query: %v", err)
	}
	return view.(app.PlanView)
}

func (fx *dogfoodFixture) inspect(wf model.WorkflowID) app.InspectView {
	fx.t.Helper()
	view, err := fx.planningApp().Query(context.Background(), app.InspectQuery{Workflow: wf})
	if err != nil {
		fx.t.Fatalf("inspect: %v", err)
	}
	return view.(app.InspectView)
}

func (fx *dogfoodFixture) targetHead() string {
	fx.t.Helper()
	return strings.TrimSpace(git(fx.repo, "rev-parse", "refs/heads/main"))
}

// driveToApproval runs the dogfood planning lifecycle through the Execution
// Approval. The Execution Dry Run and the Execution Approval run through the
// real App so the immutable routing policy records the detected identity of
// the real codex and claude executables.
func (fx *dogfoodFixture) driveToApproval(wf model.WorkflowID) {
	fx.t.Helper()
	if _, err := fx.planningApp().Execute(context.Background(),
		app.DiscussRequirementCommand{Workflow: wf, Text: dogfoodRequirement, Provider: "fake"}); err != nil {
		fx.t.Fatalf("discuss: %v", err)
	}
	if _, err := fx.planningApp().Execute(context.Background(),
		app.GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		fx.t.Fatalf("generate plan: %v", err)
	}
	if _, err := fx.planningApp().Execute(context.Background(),
		app.CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		fx.t.Fatalf("check plan: %v", err)
	}
	pv := fx.planView(wf)
	if _, err := fx.planningApp().Execute(context.Background(),
		app.ApprovePlanCommand{Workflow: wf, Revision: pv.Revision, Hash: pv.Hash}); err != nil {
		fx.t.Fatalf("approve plan: %v", err)
	}
	if _, err := fx.planningApp(dogfoodSpecScript("s1")).Execute(context.Background(),
		app.GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		fx.t.Fatalf("generate specs: %v", err)
	}
	if _, err := fx.planningApp(patchScript("w1")).Execute(context.Background(),
		app.CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		fx.t.Fatalf("compile workflow: %v", err)
	}
	a := fx.realApp()
	if _, err := a.Execute(context.Background(), app.ExecutionDryRunCommand{Workflow: wf}); err != nil {
		fx.t.Fatalf("execution dry run: %v", err)
	}
	qview, err := a.Query(context.Background(), app.ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		fx.t.Fatalf("execution preview: %v", err)
	}
	preview := qview.(app.ExecutionPreviewView)
	if _, err := a.Execute(context.Background(), app.ApproveExecutionCommand{
		Workflow:         wf,
		PlanHash:         preview.PlanHash,
		SpecHashes:       preview.SpecHashes,
		CatalogHash:      preview.CatalogHash,
		WorkflowHash:     preview.WorkflowHash,
		RoutingHash:      preview.RoutingHash,
		BudgetHash:       preview.BudgetHash,
		CommitPolicyHash: preview.CommitPolicyHash,
	}); err != nil {
		fx.t.Fatalf("execution approval: %v", err)
	}
}

// dispatchUntilCompleted drives dispatch passes through the real App until
// the Workflow is COMPLETED (the real-provider sessions, deterministic
// Verification, independent Reviews, serial --no-ff merges, and the Final
// Verify/Review ran) or the pass budget is exhausted.
func (fx *dogfoodFixture) dispatchUntilCompleted(wf model.WorkflowID) app.InspectView {
	fx.t.Helper()
	a := fx.realApp()
	for i := 0; i < 24; i++ {
		if _, err := a.Execute(context.Background(), app.DispatchCommand{Workflow: wf}); err != nil {
			fx.t.Fatalf("dispatch pass %d: %v", i, err)
		}
		iv := fx.inspect(wf)
		if iv.Status.Stage == model.StageCompleted {
			return iv
		}
	}
	iv := fx.inspect(wf)
	fx.t.Fatalf("dogfood workflow did not complete within the dispatch budget")
	return iv
}

func dogfoodSpecScript(sessionID string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"spec-generation","session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","session_id":%q,"text":"Splitting the dogfood requirement.","at_ms":10}
{"type":"session_finished","session_id":%q,"result":{"specs":[%s],"proposed_commands":[]},"at_ms":20}`,
		sessionID, sessionID, sessionID, sessionID, strings.ReplaceAll(dogfoodSpecs, "\n", ","))
}

func sha256HexString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestDogfood is the explicitly authorized real self-Dogfood: the immutable
// candidate binary runs against the CFlow repository itself with a bounded
// documentation-or-test-only requirement, two Tasks routed across the real
// codex and claude routes, independent Reviews, deterministic Verification,
// serial Integration merges, the Final Verify/Review, the final report, and
// the protected Apply that advances the Target Branch. The original
// developer workspace (the CFlow repository at its committed source) is
// preserved: the Apply only fast-forwards from the recorded Base Commit.
//
// It NEVER runs without CFLOW_DOGFOOD_REAL=1. Its default (off) behavior is
// a safe skip.
func TestDogfood(t *testing.T) {
	if os.Getenv("CFLOW_DOGFOOD_REAL") != "1" {
		t.Skip("CFLOW_DOGFOOD_REAL=1 required: the self-Dogfood run executes the candidate binary against the CFlow repository itself with real paid model requests, the providers' default permissions, and a protected Apply that advances the target branch; it must be explicitly approved (exact Dry Run, routes/models/budgets, trust boundary, network/cost, the bounded requirement, and the Apply target) before the gate is set")
	}
	repo := dogfoodRepoRoot(t)
	dogfoodRequireClean(t, repo)
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}

	// 1. Build the candidate, copy it to an immutable path outside the
	//    repository, pin its SHA-256, and prove it runs against the clean
	//    source Commit.
	bin := dogfoodBuildCandidate(t, repo, t.TempDir())
	immutable, binarySHA256 := dogfoodImmutableCopy(t, repo, bin, t.TempDir())
	sourceCommit := strings.TrimSpace(git(repo, "rev-parse", "HEAD"))
	ver, err := exec.Command(immutable, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("immutable candidate version: %v", err)
	}
	if !strings.Contains(string(ver), sourceCommit) {
		t.Fatalf("immutable candidate does not report the clean source commit %s:\n%s", sourceCommit, ver)
	}

	// 2. The deterministic preflight accepts the immutable candidate facts.
	original := filepath.Join(t.TempDir(), "original-workspace")
	if err := os.MkdirAll(original, 0o700); err != nil {
		t.Fatalf("mkdir original workspace: %v", err)
	}
	if err := observe.ValidateDogfoodPreflight(observe.DogfoodPreflight{
		BinaryPath:          immutable,
		BinarySHA256:        binarySHA256,
		SourceCommit:        sourceCommit,
		SourceClean:         true,
		TargetWorkspace:     repo,
		OriginalWorkspace:   original,
		RequirementBound:    "docs-or-tests-only",
		RequirementApproved: true,
		Routes:              []string{"codex", "claude"},
	}); err != nil {
		t.Fatalf("dogfood preflight rejected the immutable candidate: %v", err)
	}

	// 3. Drive the workflow against the CFlow repository with the real
	//    providers: two parallel Tasks routed across codex and claude with a
	//    bounded docs-or-tests-only requirement, independent Reviews,
	//    deterministic Verification, serial --no-ff merges, the Final
	//    Verify/Review, and the immutable Final Report.
	fx := &dogfoodFixture{t: t, repo: repo, home: home}
	wf := fx.createWorkflow()
	fx.driveToApproval(wf)
	iv := fx.dispatchUntilCompleted(wf)
	implProviders := map[string]bool{}
	for _, s := range iv.Sessions {
		if s.Purpose == model.PurposeImplementation {
			implProviders[s.Provider] = true
		}
	}
	if !implProviders["codex"] || !implProviders["claude"] {
		t.Fatalf("dogfood implementation sessions did not run on both real routes: %v", implProviders)
	}
	for _, id := range []string{"task-s01", "task-s02", "final-verify"} {
		if statusOf(iv, id) != model.NodeSucceeded {
			t.Fatalf("dogfood node %s status = %s, want SUCCEEDED", id, statusOf(iv, id))
		}
	}

	// 4. The protected Apply advances the Target Branch from the recorded
	//    Base Commit; the original workspace is preserved (history is never
	//    rewritten).
	oldHead := fx.targetHead()
	a := fx.realApp()
	if _, err := a.Execute(context.Background(), app.PrepareApplyCommand{Workflow: wf}); err != nil {
		t.Fatalf("prepare apply: %v", err)
	}
	if _, err := a.Execute(context.Background(), app.ExecuteApplyCommand{Workflow: wf}); err != nil {
		t.Fatalf("execute apply: %v", err)
	}
	newHead := fx.targetHead()
	if oldHead == "" || newHead == oldHead {
		t.Fatalf("dogfood apply did not advance the target (old %s new %s)", oldHead, newHead)
	}

	// 5. Record the redacted evidence for Gate 3.
	writeDogfoodEvidence(t, os.Getenv("CFLOW_DOGFOOD_EVIDENCE"), binarySHA256,
		sourceCommit, repo, original, string(wf), oldHead, newHead)
}
