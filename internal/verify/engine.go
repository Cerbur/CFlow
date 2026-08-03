// The Verification Engine (design 16): it revalidates the immutable
// Catalog identity and Purpose before every run, runs the approved
// Catalog entry through the Process Supervisor with exact
// executable/argv/cwd/environment-name identity, captures bounded
// redacted output and exit/timeout facts plus pre/post Git facts, and
// returns the hashed Evidence Manifest. It also owns the Task Commit
// gate (design 15.4, PRD 已确认：Provider 默认权限与 Commit/Clean Worktree
// Gate): before any Task may enter Verification the Engine requires a
// Git-clean Worktree, at least one implementation Commit whose HEAD is a
// descendant of the immutable Task Base, append-only history from the
// prior Attempt end, a unique append-only audit Ref, a Commit
// identity/signing match against the approved Preflight, and changed
// paths contained by the Spec write scope over the FULL
// task_base_commit..HEAD range. The Engine never fixes a gate failure by
// add/commit/stash/reset/clean/amend; it reports typed faults and the
// Application settles the Attempt from the facts.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cflow.local/cflow/internal/compile"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// Engine is the stable Verification seam (design 16.2). Its dependencies
// are private; callers only ValidateCatalog, Run, and TaskGate.
type Engine struct {
	sup         process.Supervisor
	git         *gitflow.GitFlow
	redaction   security.Registry
	loadCatalog func(ctx context.Context, ref model.CatalogRef) ([]byte, error)
}

// EngineOptions wires one Engine: the Process Supervisor and the GitFlow
// seam for pre/post Git facts, the redaction policy every captured
// output byte passes through, and the immutable Catalog body loader (the
// Application reads the Artifact Store; the Engine never touches the
// Store itself).
type EngineOptions struct {
	Supervisor  process.Supervisor
	GitFlow     *gitflow.GitFlow
	Redaction   security.Registry
	LoadCatalog func(ctx context.Context, ref model.CatalogRef) ([]byte, error)
}

// NewEngine constructs the Engine.
func NewEngine(opts EngineOptions) (*Engine, error) {
	if opts.Supervisor == nil {
		return nil, model.InvalidInputFault("verify: supervisor is required")
	}
	if opts.GitFlow == nil {
		return nil, model.InvalidInputFault("verify: gitflow is required")
	}
	if opts.LoadCatalog == nil {
		return nil, model.InvalidInputFault("verify: catalog loader is required")
	}
	return &Engine{
		sup:         opts.Supervisor,
		git:         opts.GitFlow,
		redaction:   opts.Redaction,
		loadCatalog: opts.LoadCatalog,
	}, nil
}

// ---------------------------------------------------------------------------
// Catalog validation (design 16.1)
// ---------------------------------------------------------------------------

// ValidatedCatalog is the revalidated immutable Catalog Revision: the
// parsed entries by command id plus the exact identity the ref binds.
type ValidatedCatalog struct {
	Ref     model.CatalogRef
	Entries map[string]ValidatedEntry
}

// ValidatedEntry is one policy-validated Catalog entry with its
// executable kind and the pinned executable hash the approval facts bind
// (parsed from the entry's source; the Catalog body carries no hash
// field, the source is the pin).
type ValidatedEntry struct {
	compile.CatalogEntry
	ExecutableKind ExecutableKind
	SHA256         string
}

// ValidateCatalog revalidates one immutable Catalog Revision: the body's
// revision must match the ref identity exactly, the body must parse, and
// every entry must pass the command policy. The ref identity itself is
// bound by the loader: the Application reads the body through the
// immutable Artifact Store, whose Get verifies the exact content hash of
// the ref before any byte is returned, so a drifted ref fails closed at
// the load. A mismatch is an identity drift: the Approval facts no
// longer hold (EVIDENCE_SUBJECT_CHANGED).
func (e *Engine) ValidateCatalog(ctx context.Context, ref model.CatalogRef) (ValidatedCatalog, error) {
	if ref.Revision < 1 || ref.Hash == "" {
		return ValidatedCatalog{}, evidenceSubjectChanged("catalog ref is incomplete")
	}
	body, err := e.loadCatalog(ctx, ref)
	if err != nil {
		return ValidatedCatalog{}, evidenceSubjectChanged("catalog body cannot be loaded")
	}
	catalog, err := compile.ParseCatalog(body)
	if err != nil {
		return ValidatedCatalog{}, evidenceSubjectChanged("catalog body cannot be parsed")
	}
	if catalog.Revision != ref.Revision {
		return ValidatedCatalog{}, evidenceSubjectChanged("catalog revision does not match the ref identity")
	}
	vc := ValidatedCatalog{Ref: ref, Entries: map[string]ValidatedEntry{}}
	for _, entry := range catalog.Entries {
		kind := KindProjectRelative
		if filepath.IsAbs(entry.Executable) {
			kind = KindPathExecutable
		}
		vc.Entries[entry.CommandID] = ValidatedEntry{
			CatalogEntry:   entry,
			ExecutableKind: kind,
			SHA256:         sha256FromSource(entry.Source),
		}
	}
	return vc, nil
}

// sha256FromSource parses the pinned executable hash from an entry's
// source ("...@sha256:<64hex>"; "" when the source carries no pin).
func sha256FromSource(source string) string {
	i := strings.LastIndex(source, "@sha256:")
	if i < 0 || i+len("@sha256:")+64 > len(source) {
		return ""
	}
	h := source[i+len("@sha256")+1:]
	if len(h) < 64 {
		return ""
	}
	h = h[:64]
	for _, r := range h {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return ""
		}
	}
	return h
}

// ---------------------------------------------------------------------------
// The Task Commit gate (design 15.4)
// ---------------------------------------------------------------------------

// TaskGateRequest carries the immutable Task gate facts: the Task
// identity, the immutable Task Base, the prior Attempt end (for the
// append-only check), the Spec write scope, and the approved Commit
// identity/signing policy the gate verifies the actual Commit against.
type TaskGateRequest struct {
	WorkflowID      model.WorkflowID
	TaskID          string
	TaskBranch      string
	TaskBase        string
	AttemptNumber   int
	PriorAttemptEnd string
	WriteScope      []string
	Author          gitflow.Identity
	Committer       gitflow.Identity
	Signing         gitflow.SigningPolicy
	Worktree        string
}

// TaskGateResult is the passing gate's evidence: the accepted HEAD, its
// Commit facts, the full Task range facts, the pinned audit Ref, and the
// clean status observation.
type TaskGateResult struct {
	Head        string
	CommitFacts gitflow.CommitFacts
	Range       gitflow.RangeFacts
	AuditRef    string
	Status      gitflow.StatusFacts
}

// TaskGate runs the Commit/Clean/Scope gate (PRD 已确认：Provider 默认权
// 限与 Commit/Clean Worktree Gate; 验收顺序). The gate never mutates the
// Task Branch or Worktree; the only write is the append-only audit Ref
// pinning the Attempt's end Commit (Runtime-internal, expected-absent
// compare-and-swap). A failure is a typed Fault with the PRD code; CFlow
// never fixes gate failures by add/commit/stash/reset/clean/amend.
func (e *Engine) TaskGate(ctx context.Context, req TaskGateRequest) (TaskGateResult, error) {
	if req.WorkflowID == "" || req.TaskID == "" || req.TaskBranch == "" || req.TaskBase == "" {
		return TaskGateResult{}, model.InvalidInputFault("verify: task gate facts are incomplete")
	}
	if req.AttemptNumber < 1 {
		return TaskGateResult{}, model.InvalidInputFault("verify: attempt number must be positive")
	}
	if req.Worktree == "" {
		return TaskGateResult{}, model.InvalidInputFault("verify: task worktree is required")
	}
	if err := validateGateHead(req.TaskBase); err != nil {
		return TaskGateResult{}, err
	}

	// 1. Git-clean Worktree with every Git-visible untracked file
	// classified individually (the PRD gate form).
	status, err := observeWorktree(ctx, e.git, req.Worktree, true, false)
	if err != nil {
		return TaskGateResult{}, err
	}
	if !status.Clean() {
		return TaskGateResult{}, model.NewFault(model.CodeDirtyTaskWorktree,
			"task worktree is not git-clean; the agent's changes are preserved, never discarded")
	}
	head := status.Head

	// 2. The Commit belongs to the Task Branch (the worktree registry).
	wf, err := e.git.Observe(ctx, gitflow.WorktreeList{})
	if err != nil {
		return TaskGateResult{}, err
	}
	registry := wf.(gitflow.WorktreeFacts)
	found := false
	for _, entry := range registry.Entries {
		if entry.Path == req.Worktree {
			if entry.Branch != req.TaskBranch {
				return TaskGateResult{}, model.NewFault(model.CodeTaskHistoryRewritten,
					"task worktree is no longer attached to the task branch")
			}
			found = true
			break
		}
	}
	if !found {
		return TaskGateResult{}, model.NewFault(model.CodeTaskHistoryRewritten,
			"task worktree is missing from the worktree registry")
	}

	// 3. At least one implementation Commit.
	if head == "" || head == req.TaskBase {
		return TaskGateResult{}, model.NewFault(model.CodeMissingImplementationCommit,
			"the task has no implementation commit beyond the task base")
	}

	// 4. HEAD is a descendant of the immutable Task Base (base is an
	// ancestor of HEAD: no commit of HEAD's history lies outside base).
	back, err := rangeOf(ctx, e.git, head, req.TaskBase)
	if err != nil {
		return TaskGateResult{}, err
	}
	if len(back.Commits) > 0 {
		return TaskGateResult{}, model.NewFault(model.CodeTaskHistoryRewritten,
			"task head is not a descendant of the immutable task base")
	}

	// 5. Append-only from the prior Attempt end (equal allowed only when
	// the task already carries a valid implementation commit, which the
	// checks above guarantee).
	if req.PriorAttemptEnd != "" {
		if err := validateGateHead(req.PriorAttemptEnd); err != nil {
			return TaskGateResult{}, err
		}
		back, err := rangeOf(ctx, e.git, head, req.PriorAttemptEnd)
		if err != nil {
			return TaskGateResult{}, err
		}
		if len(back.Commits) > 0 {
			return TaskGateResult{}, model.NewFault(model.CodeTaskHistoryRewritten,
				"task history was rewritten after the prior attempt end")
		}
	}

	// 6. The append-only audit Ref must be unique for this Attempt, and
	// is pinned to the end Commit (expected-absent compare-and-swap;
	// PRD: 创建或校验审计 Ref 是 Runtime 内建 Git 操作).
	auditRef := fmt.Sprintf("refs/cflow/%s/tasks/%s/attempts/%d", req.WorkflowID, req.TaskID, req.AttemptNumber)
	refFacts, err := e.git.Observe(ctx, gitflow.RefLookup{Ref: auditRef})
	if err != nil {
		return TaskGateResult{}, err
	}
	if rf, ok := refFacts.(gitflow.RefFacts); ok && rf.Exists {
		return TaskGateResult{}, model.NewFault(model.CodeTaskHistoryRewritten,
			"the attempt's audit ref already exists; the attempt was already recorded")
	}
	if _, err := e.git.Execute(ctx, gitflow.CreateAuditRef{Ref: auditRef, Head: head}); err != nil {
		return TaskGateResult{}, err
	}

	// 7. The actual Commit identity/signing must match the approved
	// Preflight (design 15.4).
	if _, err := e.git.Execute(ctx, gitflow.VerifyCommit{
		Ref: head, ExpectedAuthor: req.Author, ExpectedCommitter: req.Committer, ExpectedSigning: req.Signing,
	}); err != nil {
		return TaskGateResult{}, err
	}

	// 8. Changed paths over the FULL task_base_commit..HEAD range must be
	// contained by the Spec write scope.
	full, err := rangeOf(ctx, e.git, req.TaskBase, head)
	if err != nil {
		return TaskGateResult{}, err
	}
	for _, changed := range full.ChangedPaths {
		if !scopeContains(req.WriteScope, changed) {
			return TaskGateResult{}, model.NewFault(model.CodeScopeViolation,
				fmt.Sprintf("task changed path %q outside the write scope", changed))
		}
	}

	commitFacts, err := e.git.Observe(ctx, gitflow.CommitInspect{Ref: head})
	if err != nil {
		return TaskGateResult{}, err
	}
	cf, ok := commitFacts.(gitflow.CommitFacts)
	if !ok {
		return TaskGateResult{}, model.InvariantFault(fmt.Errorf("commit inspect result has an unexpected type"))
	}
	return TaskGateResult{Head: head, CommitFacts: cf, Range: full, AuditRef: auditRef, Status: status}, nil
}

// rangeOf observes the exact commit list of from..to ("commits in to not
// in from").
func rangeOf(ctx context.Context, g *gitflow.GitFlow, from, to string) (gitflow.RangeFacts, error) {
	facts, err := g.Observe(ctx, gitflow.HistoryRange{From: from, To: to})
	if err != nil {
		return gitflow.RangeFacts{}, err
	}
	rf, ok := facts.(gitflow.RangeFacts)
	if !ok {
		return gitflow.RangeFacts{}, model.InvariantFault(fmt.Errorf("history range result has an unexpected type"))
	}
	return rf, nil
}

func validateGateHead(head string) error {
	if len(head) != 40 {
		return model.InvalidInputFault("verify: gate head must be a full commit hash")
	}
	for _, r := range head {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return model.InvalidInputFault("verify: gate head must be a full commit hash")
		}
	}
	return nil
}

// scopeContains reports whether path is contained by one write-scope
// entry (directory-prefix containment after normalizing trailing glob
// markers; the same semantics the Compiler's validation uses).
func scopeContains(scope []string, path string) bool {
	for _, entry := range scope {
		entry = strings.TrimSuffix(strings.TrimSuffix(entry, "/"), "/**")
		if path == entry || strings.HasPrefix(path, entry+"/") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The Verification Run (design 16.2)
// ---------------------------------------------------------------------------

// VerificationRequest is one approved Catalog entry run by identity:
// the verify Node, the immutable Catalog ref, the command id, the
// purpose the use must match, the managed Worktree the command runs
// inside, and the Commit range being verified.
type VerificationRequest struct {
	Node        model.NodeID
	Catalog     model.CatalogRef
	CommandID   string
	Purpose     Purpose
	Worktree    string
	CommitRange string
}

// Run executes one Verification (design 16.2). It revalidates the
// Catalog identity and Purpose, captures pre-run Git facts, resolves and
// hashes the exact executable, runs the entry through the Supervisor
// with bounded redacted output, records the exit/timeout facts and
// post-run Git facts, and returns the hashed Evidence Manifest. A
// failing command is a failing Manifest (never an error); an unrunnable
// verification (identity drift, executable mismatch, head drift) is a
// typed Fault.
func (e *Engine) Run(ctx context.Context, req VerificationRequest) (model.EvidenceManifest, error) {
	catalog, err := e.ValidateCatalog(ctx, req.Catalog)
	if err != nil {
		return model.EvidenceManifest{}, err
	}
	entry, ok := catalog.Entries[req.CommandID]
	if !ok {
		return model.EvidenceManifest{}, evidenceSubjectChanged("unknown catalog command id " + req.CommandID)
	}
	if entry.Purpose != string(req.Purpose) {
		return model.EvidenceManifest{}, evidenceSubjectChanged(
			fmt.Sprintf("catalog purpose %q does not permit the %q use", entry.Purpose, req.Purpose))
	}
	_, to, err := splitRange(req.CommitRange)
	if err != nil {
		return model.EvidenceManifest{}, err
	}

	// Pre-run Git facts (design 16.2 step 1): the Worktree HEAD must be
	// the Commit under verification and the Worktree must be Git-clean
	// before the command starts.
	pre, err := observeWorktree(ctx, e.git, req.Worktree, true, false)
	if err != nil {
		return model.EvidenceManifest{}, err
	}
	if pre.Head != to {
		return model.EvidenceManifest{}, evidenceSubjectChanged(
			"worktree head does not match the commit under verification")
	}
	if !pre.Clean() {
		return model.EvidenceManifest{}, model.NewFault(model.CodeDirtyTaskWorktree,
			"worktree is not git-clean before verification")
	}

	// The exact executable identity (design 16.2 step 2): the pinned
	// executable hash must match the approval facts.
	executable, cwd, err := e.resolveExecutable(catalog, entry, req.Worktree)
	if err != nil {
		return model.EvidenceManifest{}, err
	}
	env := envFor(entry.Env)
	timeout := time.Duration(entry.TimeoutSeconds) * time.Second
	started := time.Now()
	h, events, err := e.sup.Start(ctx, process.ProcessSpec{
		Executable:     executable,
		Args:           append([]string(nil), entry.Args...),
		Dir:            cwd,
		Env:            env,
		Timeout:        timeout,
		MaxFrameBytes:  1 << 20,
		MaxOutputBytes: int64(entry.MaxOutputBytes),
	})
	if err != nil {
		return model.EvidenceManifest{}, err
	}

	// Bounded redacted output (design 16.2 step 3): raw bytes exist only
	// in bounded memory; every frame passes the Redactor before it is
	// stored.
	red := security.NewRedactor(e.redaction)
	var output strings.Builder
	var outFrames, errFrames int
	for ev := range events {
		switch ev.Kind {
		case process.EventFrameOut, process.EventFrameErr:
			frame, rerr := red.WriteFrame(ev.Frame)
			if rerr != nil {
				return model.EvidenceManifest{}, model.NewFault(model.CodeSensitiveDataRedactionFailed,
					"verification output cannot be redacted")
			}
			if ev.Kind == process.EventFrameOut {
				outFrames++
			} else {
				errFrames++
			}
			if output.Len() < entry.MaxOutputBytes {
				remaining := entry.MaxOutputBytes - output.Len()
				if len(frame.Text) > remaining {
					output.WriteString(frame.Text[:remaining])
					output.WriteString("\n[output truncated]")
				} else {
					output.WriteString(frame.Text)
					output.WriteString("\n")
				}
			}
		}
	}
	flushed, ferr := red.Flush()
	if ferr != nil {
		return model.EvidenceManifest{}, model.NewFault(model.CodeSensitiveDataRedactionFailed,
			"verification output cannot be redacted")
	}
	if output.Len() < entry.MaxOutputBytes && flushed.Text != "" {
		output.WriteString(flushed.Text)
	}
	exit, werr := e.sup.Wait(ctx, h)
	if werr != nil {
		return model.EvidenceManifest{}, werr
	}

	// Exit and timeout facts (design 16.2 step 4).
	exitFact := "exit"
	exitCode := exit.Code
	switch exit.Fact {
	case process.FactTimeout:
		exitFact = "timeout"
		exitCode = -1
	case process.FactSignaled:
		exitFact = "signal"
		exitCode = -1
	case process.FactCancelled:
		exitFact = "cancelled"
		exitCode = -1
	}
	expected := false
	for _, c := range entry.ExpectedExitCodes {
		if exitCode == c {
			expected = true
			break
		}
	}

	// Post-run Git facts (design 16.2 step 5): HEAD must still be the
	// Commit under verification; tracked changes, Git-visible untracked
	// output, and ignored output outside the declared transient paths all
	// fail verification.
	post, err := observeWorktree(ctx, e.git, req.Worktree, true, true)
	if err != nil {
		return model.EvidenceManifest{}, err
	}
	reason := ""
	switch {
	case exitFact == "timeout":
		reason = "timeout"
	case exitFact == "cancelled":
		reason = "cancelled"
	case exitFact == "signal":
		reason = "signal"
	case !expected:
		reason = "exit"
	case post.Head != to:
		reason = "head-changed"
	case len(post.Staged)+len(post.Unstaged) > 0:
		reason = "tracked-output"
	case len(post.Untracked) > 0:
		reason = "untracked-output"
	case !ignoredWithinDeclared(entry.TransientWritePaths, post.Ignored):
		reason = "ignored-output-outside-transient-paths"
	}

	manifest := model.EvidenceManifest{
		SchemaVersion: "1.0.0",
		Node:          req.Node,
		CatalogRef:    req.Catalog,
		CommandID:     req.CommandID,
		Purpose:       string(req.Purpose),
		CommitRange:   req.CommitRange,
		Passed:        reason == "",
		Reason:        reason,
		ExitCode:      exitCode,
		ExitFact:      exitFact,
		DurationMs:    time.Since(started).Milliseconds(),
		PreGit:        model.GitFactsSummary{Head: pre.Head, Clean: pre.Clean()},
		PostGit:       model.GitFactsSummary{Head: post.Head, Clean: post.Clean()},
		Output:        output.String(),
		OutputHash:    sha256Hex([]byte(output.String())),
	}
	manifest.Hash = manifestHash(manifest)
	return manifest, nil
}

// resolveExecutable resolves and hashes the exact executable identity: a
// project-relative wrapper inside the Worktree, or the pinned absolute
// PATH executable. The pinned hash must match the approval facts.
func (e *Engine) resolveExecutable(catalog ValidatedCatalog, entry ValidatedEntry, worktree string) (executable, cwd string, err error) {
	executable = entry.Executable
	switch entry.ExecutableKind {
	case KindProjectRelative:
		joined := filepath.Join(worktree, filepath.FromSlash(entry.Executable))
		rel, rerr := filepath.Rel(worktree, joined)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", "", model.NewFault(model.CodeEvidenceSubjectChanged,
				"catalog executable escapes the managed worktree")
		}
		executable = joined
	default:
		if !filepath.IsAbs(entry.Executable) {
			return "", "", evidenceSubjectChanged("path executable is not absolute")
		}
	}
	data, rerr := os.ReadFile(executable)
	if rerr != nil {
		return "", "", evidenceSubjectChanged("catalog executable is missing")
	}
	if entry.SHA256 == "" || sha256Hex(data) != entry.SHA256 {
		return "", "", evidenceSubjectChanged("catalog executable identity changed since approval")
	}
	cwd = worktree
	if entry.CWD != "" {
		cwd = filepath.Join(worktree, filepath.FromSlash(entry.CWD))
		rel, rerr := filepath.Rel(worktree, cwd)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", "", model.NewFault(model.CodeEvidenceSubjectChanged,
				"catalog cwd escapes the managed worktree")
		}
	}
	return executable, cwd, nil
}

// envFor forwards exactly the declared environment names from the parent
// environment (design 16.2: environment-name identity).
func envFor(names []string) map[string]string {
	env := map[string]string{}
	for _, name := range names {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	return env
}

// ignoredWithinDeclared reports whether every ignored path stays within
// the entry's declared transient write paths (design 16.2: ignored
// transient output is permitted only within declared paths).
func ignoredWithinDeclared(declared []string, ignored []gitflow.PathEntry) bool {
	for _, p := range ignored {
		inside := false
		for _, d := range declared {
			if scopeContains([]string{d}, p.Path) {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}

// observeWorktree observes one worktree's status with the exact
// classification flags.
func observeWorktree(ctx context.Context, g *gitflow.GitFlow, dir string, untrackedAll, ignored bool) (gitflow.StatusFacts, error) {
	facts, err := g.Observe(ctx, gitflow.GitStatus{Dir: dir, UntrackedAll: untrackedAll, Ignored: ignored})
	if err != nil {
		return gitflow.StatusFacts{}, err
	}
	st, ok := facts.(gitflow.StatusFacts)
	if !ok {
		return gitflow.StatusFacts{}, model.InvariantFault(fmt.Errorf("git status observation has an unexpected type"))
	}
	return st, nil
}

// splitRange parses "from..to" into the two full commit hashes.
func splitRange(r string) (from, to string, err error) {
	parts := strings.SplitN(r, "..", 2)
	if len(parts) != 2 {
		return "", "", model.InvalidInputFault("verify: commit range must be from..to")
	}
	if err := validateGateHead(parts[0]); err != nil {
		return "", "", err
	}
	if err := validateGateHead(parts[1]); err != nil {
		return "", "", err
	}
	return parts[0], parts[1], nil
}

// manifestHash binds the exact manifest revision: the canonical JSON
// serialization excluding the Hash field itself.
func manifestHash(m model.EvidenceManifest) string {
	m.Hash = ""
	body, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return sha256Hex(body)
}

func evidenceSubjectChanged(text string) error {
	return model.NewFault(model.CodeEvidenceSubjectChanged, "verify: "+text)
}
