// Package gitflow implements the GitFlow module (design 15): the Git
// foundation of the CFlow Runtime. It owns project discovery, structured
// Git facts, Worktree lifecycle primitives, and the Commit Identity and
// Signing Preflight. GitFlow never formats prose for humans; it produces
// typed facts and typed results.
//
// The stable interface (design 15.1):
//
//	type GitFlow struct { /* private process.Supervisor dependency */ }
//	func (g *GitFlow) Observe(context.Context, GitQuery) (GitFacts, error)
//	func (g *GitFlow) Execute(context.Context, GitOperation) (GitResult, error)
//
// GitQuery, GitOperation, GitFacts, and GitResult are closed unions.
// GitFlow never accepts arbitrary Git argv from callers: every child
// process is launched through the Process Supervisor (Task 6) from an
// embedded argv template in which each non-static argument is a field
// that passed a dedicated validator (canonical path, refname, or full
// object hash) before any process started.
//
// Isolation guarantees (PRD 启动与项目识别, Worktree 策略, Git Commit
// Identity 与 Signing Preflight):
//
//   - GitFlow never runs mutating Git commands in the user's working tree,
//     never creates the target repository's first commit, never runs
//     git init on the target repository, never stashes or checks out the
//     user's changes, and never writes global or local Git config.
//   - The user's dirty working tree is fingerprinted from normalized
//     porcelain-v2 classifications plus content hashes of untracked files;
//     file contents are hashed in bounded memory and never copied into a
//     Workflow artifact.
//   - The Commit Preflight resolves the effective author/committer
//     identity and signing policy, normalizes them into a policy
//     fingerprint (timestamps, temporary paths, and secrets excluded),
//     and when signing is enabled runs an isolated, non-interactive,
//     time-bounded signed probe in a CFlow-managed temporary repository.
//     A probe failure, timeout, or any interactive signer blocks with
//     GIT_SIGNING_PREFLIGHT_FAILED; GitFlow never falls back to unsigned
//     commits and never pops credential prompts.
//
// Child processes receive a curated environment: a fixed allowlist
// forwarded from the parent (identity and config override variables,
// HOME, PATH, the SSH agent socket) plus fixed policy overrides
// (GIT_TERMINAL_PROMPT=0). GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE,
// credential helpers, and display variables are never forwarded, so a
// supervised Git process can neither be redirected to another repository
// nor pop interactive prompts.
package gitflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// Bounds: one observation or worktree operation runs at most
// defaultGitTimeout; the signing probe defaults to probeTimeout unless the
// caller tightens it.
const (
	defaultGitTimeout   = 60 * time.Second
	defaultProbeTimeout = 10 * time.Second
)

// GitFlow is the stable Git seam (design 15.1). Its dependencies are
// private; callers only Observe and Execute.
type GitFlow struct {
	sup process.Supervisor
	git string // absolute git executable resolved once
	dir string // canonical working directory this GitFlow serves
}

// NewGitFlow resolves the git executable and canonicalizes the working
// directory the GitFlow serves. The directory must be absolute and must
// resolve through no more than one symlink level (realpath semantics);
// whether it is inside a Git repository is a ProjectDiscovery fact, not a
// construction error (PRD: non-Git directories only offer doctor).
func NewGitFlow(sup process.Supervisor, dir string) (*GitFlow, error) {
	if sup == nil {
		return nil, model.InvalidInputFault("gitflow: supervisor is required")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, model.InvalidInputFault("gitflow: git executable not found")
	}
	canon, err := canonicalDir(dir)
	if err != nil {
		return nil, err
	}
	return &GitFlow{sup: sup, git: git, dir: canon}, nil
}

// ---------------------------------------------------------------------------
// Closed unions
// ---------------------------------------------------------------------------

// GitQuery is the closed union of observation queries. There is no free
// form: every query is a typed struct whose fields are validated before
// any Git process starts.
type GitQuery interface{ isGitQuery() }

// ProjectDiscovery observes the repository containing the GitFlow's
// working directory: canonical root, attached branch, HEAD and its
// unborn/detached classification, porcelain-v2 status with the Dirty
// Fingerprint, the worktree registry, and the Project Key.
type ProjectDiscovery struct{}

func (ProjectDiscovery) isGitQuery() {}

// GitStatus observes the porcelain-v2 status of one working tree (default
// the GitFlow working directory). ExpectedHead, when set, must equal the
// actual HEAD or the observation fails closed (expected-HEAD mismatch).
// UntrackedAll adds --untracked-files=all so every Git-visible untracked
// file is classified individually (the PRD Commit/Clean gate form);
// Ignored additionally classifies ignored files (verification transient
// output, design 16.2). Ignored files never count toward the Dirty
// Fingerprint.
// FingerprintObserve recomputes the effective Commit Policy fingerprint
// without the signing probe (PRD 已确认：Commit Policy 漂移立即安全停止 step
// 5: the periodic monitor reads only public effective configuration, never
// Signer Secrets, and never runs a signature probe per poll). Revision
// names the observation for its evidence identity.
type FingerprintObserve struct {
	Revision string
}

func (FingerprintObserve) isGitQuery() {}

// WorktreeInProgress observes whether a managed Worktree carries an
// in-progress merge/rebase/cherry-pick/revert/bisect or an administrative
// lock file (design 17.4: the safe-clean gate refuses a target with an
// unfinished Git operation or a lock; git worktree remove would refuse it).
type WorktreeInProgress struct {
	Path string // exact canonical Worktree path
}

func (WorktreeInProgress) isGitQuery() {}

type GitStatus struct {
	Dir          string // absolute canonical directory; empty means the bound directory
	ExpectedHead string // full commit hash; empty means no expectation
	UntrackedAll bool
	Ignored      bool
}

func (GitStatus) isGitQuery() {}

// WorktreeList observes the repository's worktree registry.
type WorktreeList struct{}

func (WorktreeList) isGitQuery() {}

// CommitInspect observes one commit's facts: parents, author, committer,
// timestamps, signature presence and verification.
type CommitInspect struct {
	Ref string // refname or full hash
}

func (CommitInspect) isGitQuery() {}

// RefLookup observes one ref's existence and value. Expected, when set,
// records whether the actual value matches.
type RefLookup struct {
	Ref      string
	Expected string // full commit hash; empty means no expectation
}

func (RefLookup) isGitQuery() {}

// HistoryRange observes the exact commit list and changed paths of
// From..To.
type HistoryRange struct {
	From string // full commit hash
	To   string // full commit hash
}

func (HistoryRange) isGitQuery() {}

// GitOperation is the closed union of Git operations. Operations are
// compare-and-swap guarded: GitFlow verifies the repository state it was
// told to expect before and after every mutation.
type GitOperation interface{ isGitOperation() }

// CreatePlanningSnapshot creates the Planning Snapshot Worktree (design
// 15.2): a detached worktree fixed at BaseCommit. No branch or ref is
// created and the user's target branch never moves.
type CreatePlanningSnapshot struct {
	BaseCommit string // full commit hash, the recorded Workflow Base
	Path       string // canonical destination worktree path
}

func (CreatePlanningSnapshot) isGitOperation() {}

// CreateWorkspace creates the single long-lived Workspace Branch/Worktree
// from BaseHead at the recorded Branch and Path (design 8.1; TUI task 4).
// The branch must not already exist; the user's target branch never moves.
type CreateWorkspace struct {
	Branch   string // refname (without refs/heads/ prefix)
	BaseHead string // full commit hash, the recorded Workflow Base
	Path     string // canonical destination worktree path
}

func (CreateWorkspace) isGitOperation() {}

// CreateIntegration creates the Integration Branch/Worktree from
// BaseCommit (design 15.2, only after Execution Approval). The branch
// must not already exist.
type CreateIntegration struct {
	Branch     string // refname (without refs/heads/ prefix)
	BaseCommit string // full commit hash
	Path       string // canonical destination worktree path
}

func (CreateIntegration) isGitOperation() {}

// CreateTask creates one isolated Task Branch/Worktree from BaseHead (the
// verified Integration HEAD recorded when the Task became Ready). An
// unknown BaseHead is a fail-closed expected-HEAD mismatch.
type CreateTask struct {
	Branch   string // refname (without refs/heads/ prefix)
	BaseHead string // full commit hash
	Path     string // canonical destination worktree path
}

func (CreateTask) isGitOperation() {}

// CreateAuditRef creates one append-only audit ref with expected-absent
// semantics: the ref must not exist. It is never overwritten or moved.
type CreateAuditRef struct {
	Ref  string // full refname (must start with refs/)
	Head string // full commit hash the ref must point to
}

func (CreateAuditRef) isGitOperation() {}

// CommitPreflight resolves the effective identity and signing policy,
// runs the isolated signed probe when signing is enabled, and returns
// immutable Preflight evidence (PRD 已确认：Git Commit Identity 与
// Signing Preflight).
type CommitPreflight struct {
	Revision     string        // caller-assigned immutable revision, e.g. "wf-1"
	ProbeTimeout time.Duration // 0 uses the default hard timeout
}

func (CommitPreflight) isGitOperation() {}

// VerifyCommit verifies a commit's actual author, committer, and signing
// evidence against the approved Preflight policy (design 15.4). Any
// mismatch blocks with COMMIT_POLICY_MISMATCH.
type VerifyCommit struct {
	Ref               string
	ExpectedAuthor    Identity
	ExpectedCommitter Identity
	ExpectedSigning   SigningPolicy
}

func (VerifyCommit) isGitOperation() {}

// MergeIntegration performs one serial --no-ff merge of branch into the
// managed Worktree at path (design 15.5; the same primitive serves the
// serial Integration merges and the isolated Apply staging merge). The
// merge is compare-and-swap guarded: the caller records the expected
// pre-merge HEAD with a GitStatus observation; a text conflict returns a
// typed MergeConflictResult (never an error) and leaves the worktree in
// the conflicted merge state for the RollbackMerge operation. Message
// overrides the Merge Commit subject ("" uses the default).
type MergeIntegration struct {
	Path    string // canonical managed Worktree path
	Branch  string // Branch refname (without refs/heads/ prefix)
	Message string // optional Merge Commit subject override
}

func (MergeIntegration) isGitOperation() {}

// CreateApply creates the isolated Apply Branch/Worktree from the
// recorded Target HEAD (PRD 已确认：显式受保护 Apply step 2): the staging
// merge never runs in the user's working tree. The branch must not
// already exist and the destination path must be outside every existing
// worktree.
type CreateApply struct {
	Branch   string // refname (without refs/heads/ prefix)
	BaseHead string // full commit hash, the recorded Target HEAD
	Path     string // canonical destination worktree path
}

func (CreateApply) isGitOperation() {}

// UpdateRef performs the final compare-and-swap fast-forward of one ref
// (PRD 已确认：显式受保护 Apply step 6): the expected old value is
// re-observed, the new head must be a descendant of the expected head (a
// fast-forward; there is no force-update form anywhere in this path),
// and the ref is updated only with `git update-ref <ref> <new>
// <expected>` — the atomic expected-value form. The observed actual ref
// is the reported outcome.
type UpdateRef struct {
	Ref      string // full refname (refs/heads/<target>)
	New      string // full commit hash, the verified staging head
	Expected string // full commit hash, the recorded Target HEAD
}

func (UpdateRef) isGitOperation() {}

// CompleteMerge finishes a conflicted Apply staging merge inside the
// managed Apply Worktree (design 15.5: the ONE restricted Merge
// Resolution Attempt): the exact conflict files are staged (scope-bound)
// and the merge commit is created with the recorded parents. Any
// change outside the conflict files fails closed; the Merge Commit must
// carry both parents and the Worktree must be clean afterwards.
type CompleteMerge struct {
	Path          string   // canonical Apply Worktree path
	ConflictFiles []string // the exact conflicted paths (write scope)
	Message       string   // Merge Commit subject
}

func (CompleteMerge) isGitOperation() {}

// RollbackMerge restores the managed Integration Worktree to the
// recorded pre-merge HEAD (design 15.5, PRD 已确认：Merge Conflict 处理).
// Three states are restored: an unchanged clean Worktree (a refused
// merge: verified no-op), a conflicted merge in progress (`git merge
// --abort`), and a merge that COMMITTED but failed its post-merge checks
// (the guarded `git reset --hard` to the recorded head, refuse-to-run
// when the current HEAD is not a descendant of it — a foreign or
// replaced history is never destroyed). Every path ends in a fail-closed
// verification of the expected HEAD and a clean worktree; only the
// managed Integration Worktree is ever touched.
type RollbackMerge struct {
	Path         string // canonical Integration Worktree path
	ExpectedHead string // the recorded pre-merge HEAD
}

func (RollbackMerge) isGitOperation() {}

// RemoveWorktree removes one managed Worktree through the exact,
// non-force `git worktree remove <path>` form (design 17.4, PRD 已确认：
// Cleanup 仅删除安全干净的衍生目录). The caller records the expected
// registry entry and the safe-clean facts before calling; GitFlow
// re-verifies the path is a registered Worktree and runs the removal
// WITHOUT --force — a dirty, locked, or occupied Worktree is refused with
// CLEANUP_TARGET_DIRTY and is never force-removed. The post-removal
// verification lives with the caller (the executor re-observes the
// registry and the directory), so a crash after the removal leaves the
// Intent pending and recovery settles from the actual state.
type RemoveWorktree struct {
	Path string // exact canonical Worktree path
}

func (RemoveWorktree) isGitOperation() {}

// MoveWorktree moves one managed Git Worktree to a new canonical path
// with `git worktree move` (design §7.4, TUI task 8: the explicit Legacy
// Layout Migration). The source must be a registered Worktree and the
// destination must not exist and must be outside every existing worktree;
// the Worktree's attached Branch and HEAD follow the move untouched.
// A dirty or in-progress source is refused (git worktree move refuses it
// anyway); the fail-closed post-move verification re-observes the
// registry and the destination.
type MoveWorktree struct {
	From string // exact canonical source Worktree path
	To   string // exact canonical destination Worktree path
}

func (MoveWorktree) isGitOperation() {}

// FastForwardWorkingTree delivers one verified Apply staging head to the
// user's original working tree with `git merge --ff-only` (design §13.2,
// TUI task 15): the Root must be clean and attached to the expected
// Branch at the expected HEAD, the new head must be a descendant (a
// fast-forward), and the resulting HEAD/Index/Worktree are re-verified.
// No reset, force, stash, or checkout argv ever appears.
type FastForwardWorkingTree struct {
	Root     string // the user's original working tree root
	Branch   string // the branch the root is attached to
	Expected string // full commit hash, the recorded Target HEAD
	New      string // full commit hash, the verified staging head
}

func (FastForwardWorkingTree) isGitOperation() {}

// FastForwardWorkingTreeResult reports the observed delivery outcome.
type FastForwardWorkingTreeResult struct {
	Head  string
	Clean bool
}

func (FastForwardWorkingTreeResult) isGitResult() {}

// GitFacts is the closed union of structured facts. Facts are data, never
// formatted prose: callers make decisions, GitFlow reports truth.
type GitFacts interface{ isGitFacts() }

// FingerprintFacts is the probe-less Commit Policy observation (design
// 15.6): the normalized fingerprint, the effective identity and signing
// policy, and the evidence identity of the observation. It never carries
// Signer Secrets.
type FingerprintFacts struct {
	Revision          string
	PolicyFingerprint string
	GitVersion        string
	Author            Identity
	Committer         Identity
	Signing           SigningPolicy
	EvidenceHash      string
}

func (FingerprintFacts) isGitFacts() {}

// ProjectFacts is the discovery fact set (design 15.3).
type ProjectFacts struct {
	Root       string      // canonical repository root
	Branch     string      // attached local branch, "" when detached or unborn
	Head       string      // full HEAD hash, "" when unborn
	Unborn     bool        // repository has no commits
	Detached   bool        // HEAD is not attached to a local branch
	Status     StatusFacts // porcelain-v2 status of the user's working tree
	Worktrees  []WorktreeEntry
	ProjectKey string // readable slug plus canonical-path hash (PRD 启动与项目识别)
}

func (ProjectFacts) isGitFacts() {}

// StatusFacts is the normalized porcelain-v2 classification of one
// working tree plus its Dirty Fingerprint.
type StatusFacts struct {
	Dir       string
	Head      string
	Staged    []PathEntry // index differs from HEAD (X != '.')
	Unstaged  []PathEntry // worktree differs from index (Y != '.')
	Untracked []PathEntry
	Ignored   []PathEntry // '!' entries, only when the query asked for them
	Dirty     DirtyFingerprint
}

// Clean reports whether the working tree meets the PRD Commit/Clean Gate:
// no staged, unstaged, or Git-visible untracked content. Ignored files
// never count (PRD 已确认：Provider 默认权限与 Commit/Clean Worktree Gate).
func (s StatusFacts) Clean() bool {
	return len(s.Staged) == 0 && len(s.Unstaged) == 0 && len(s.Untracked) == 0
}

func (StatusFacts) isGitFacts() {}

// PathEntry is one classified path with its porcelain status codes and
// available blob hashes (hI for staged entries; worktree modes for
// unstaged; content hashes for untracked files are carried by the Dirty
// Fingerprint, not by the entry itself).
type PathEntry struct {
	Path     string // worktree-relative path
	Original string // rename/copy source path, "" otherwise
	X, Y     byte   // porcelain v2 status codes
	Hash     string // index blob hash for staged entries, else ""
	Mode     string // worktree mode (mW), "" when unavailable
}

// DirtyFingerprint covers the staged diff, the unstaged diff, and the
// untracked path list with content hashes (PRD 已确认：用户当前工作区
// 隔离). It is computed from normalized classifications; file contents
// are streamed through SHA-256 and never stored or copied. Identical
// working-tree state yields an identical fingerprint.
type DirtyFingerprint struct {
	Algorithm      string // "cflow-dirty-v1"
	StagedHash     string
	UnstagedHash   string
	UntrackedHash  string
	Combined       string // sha256(stagedHash + unstagedHash + untrackedHash)
	StagedCount    int
	UnstagedCount  int
	UntrackedCount int
}

// WorktreeFacts is the worktree registry.
type WorktreeFacts struct {
	Entries []WorktreeEntry
}

func (WorktreeFacts) isGitFacts() {}

// WorktreeInProgressFacts reports whether one managed Worktree carries an
// in-progress Git operation or an administrative lock file.
type WorktreeInProgressFacts struct {
	InProgress bool   // an unfinished merge/rebase/cherry-pick/revert/bisect
	Locked     bool   // an administrative *.lock file
	Reason     string // the state file or lock file name, for diagnostics
}

func (WorktreeInProgressFacts) isGitFacts() {}

// WorktreeEntry is one registry entry (git worktree list --porcelain).
type WorktreeEntry struct {
	Path     string // absolute worktree path
	Branch   string // attached branch ("" when detached)
	Head     string // HEAD commit ("" when unborn)
	Detached bool
	Bare     bool
	Locked   bool
	Prunable bool
}

// CommitFacts is one commit's structured evidence.
type CommitFacts struct {
	Ref        string
	Head       string // full hash
	Parents    []string
	Author     Identity
	Committer  Identity
	AuthorTime int64 // epoch seconds
	CommitTime int64 // epoch seconds
	Subject    string
	Signature  SignatureFacts
}

func (CommitFacts) isGitFacts() {}

// SignatureFacts describes one commit's signature evidence.
type SignatureFacts struct {
	Present  bool // the commit carries a gpgsig header
	Verified bool // git verify-commit exits 0 (informational; ssh signatures need an allowed-signers file)
}

// Identity is a resolved name/email pair. Source records where the value
// came from ("env" or a config scope such as "local"), when known.
type Identity struct {
	Name   string
	Email  string
	Source string
}

// RefFacts is one ref's existence and value.
type RefFacts struct {
	Ref      string
	Exists   bool
	Value    string // full commit hash, "" when absent
	Expected string // caller expectation ("" when none)
	Matches  bool   // Exists && (Expected == "" || Value == Expected)
}

func (RefFacts) isGitFacts() {}

// RangeFacts is the exact commit list and changed paths of one range.
type RangeFacts struct {
	From         string
	To           string
	Commits      []string
	ChangedPaths []string
}

func (RangeFacts) isGitFacts() {}

// GitResult is the closed union of operation results.
type GitResult interface{ isGitResult() }

// PlanningSnapshotResult reports the created Planning Snapshot.
type PlanningSnapshotResult struct {
	Worktree string
	Head     string
}

func (PlanningSnapshotResult) isGitResult() {}

// WorkspaceWorktreeResult reports the created Workspace worktree.
type WorkspaceWorktreeResult struct {
	Worktree string
	Branch   string
	Head     string
}

func (WorkspaceWorktreeResult) isGitResult() {}

// IntegrationWorktreeResult reports the created Integration worktree.
type IntegrationWorktreeResult struct {
	Worktree string
	Branch   string
	Head     string
}

func (IntegrationWorktreeResult) isGitResult() {}

// TaskWorktreeResult reports the created Task worktree.
type TaskWorktreeResult struct {
	Worktree string
	Branch   string
	Head     string
}

func (TaskWorktreeResult) isGitResult() {}

// AuditRefResult reports the created append-only audit ref.
type AuditRefResult struct {
	Ref  string
	Head string
}

func (AuditRefResult) isGitResult() {}

// PreflightEvidence is the immutable Commit Preflight result. It records
// the normalized identity, signing policy, probe outcome, and time; it
// never records private keys, passphrases, or credential-helper output.
// The PolicyFingerprint is stable for identical policy; the EvidenceHash
// binds the exact evidence revision.
type PreflightEvidence struct {
	Revision          string
	PolicyFingerprint string
	EvidenceHash      string
	GitVersion        string
	ResolvedAt        string // RFC3339 UTC
	Author            Identity
	Committer         Identity
	Signing           SigningPolicy
	Probe             ProbeFacts
}

func (PreflightEvidence) isGitResult() {}

// SigningPolicy is the effective commit signing policy. Origins maps each
// policy config key to its scope (local/global/system/command/env), for
// diagnostics; the fingerprint uses the normalized values only.
type SigningPolicy struct {
	Enabled bool
	Format  string // openpgp, x509, or ssh (git's effective value)
	Key     string // user.signingkey value (public key ID or path, never a secret)
	Program string // effective signer program (gpg, ssh-keygen, or configured)
	Origins map[string]string
}

// ProbeFacts reports the isolated signed probe outcome.
type ProbeFacts struct {
	Required   bool
	Ran        bool
	Success    bool
	Exit       int    // probe commit exit code (-1 for timeout/signal)
	Fact       string // "exit", "timeout", "signal"
	DurationMs int64
}

// VerifyCommitResult reports that the commit's evidence matched the
// approved policy; a mismatch returns a COMMIT_POLICY_MISMATCH fault
// instead.
type VerifyCommitResult struct {
	Commit CommitFacts
}

func (VerifyCommitResult) isGitResult() {}

// MergeResult reports a completed serial --no-ff Integration merge: the
// new HEAD and the Merge Commit facts (the Merge Commit's parents are the
// pre-merge HEAD and the merged Task Branch head, preserving the Task's
// append-only history and the separate Merge Commit, PRD Worktree 策略).
type MergeResult struct {
	Path   string
	Head   string
	Commit CommitFacts
}

func (MergeResult) isGitResult() {}

// MergeConflictResult is the typed text-conflict outcome (design 15.5):
// the merge did not commit, PreMergeHead is the recorded pre-merge HEAD,
// and the worktree is left in the conflicted merge state for the
// RollbackMerge operation.
type MergeConflictResult struct {
	Path         string
	PreMergeHead string
}

func (MergeConflictResult) isGitResult() {}

// RollbackResult reports that the managed Integration Worktree was
// restored to the recorded pre-merge HEAD and verified clean.
type RollbackResult struct {
	Path string
	Head string
}

func (RollbackResult) isGitResult() {}

// ApplyWorktreeResult reports the created or reused Apply
// Branch/Worktree with its verified state.
type ApplyWorktreeResult struct {
	Worktree string
	Branch   string
	Head     string
}

func (ApplyWorktreeResult) isGitResult() {}

// UpdateRefResult reports the observed actual value of the ref after the
// compare-and-swap (the outcome is reported from the observation, never
// assumed).
type UpdateRefResult struct {
	Ref      string
	Observed string
}

func (UpdateRefResult) isGitResult() {}

// WorktreeRemovedResult reports the exact path whose Worktree was removed
// (a crash after the removal settles from the actual registry state).
type WorktreeRemovedResult struct {
	Path string
}

func (WorktreeRemovedResult) isGitResult() {}

// WorktreeMovedResult reports the exact source and destination of one
// moved Worktree (a crash after the move settles from the actual registry
// state).
type WorktreeMovedResult struct {
	From string
	To   string
	Head string
}

func (WorktreeMovedResult) isGitResult() {}

// ---------------------------------------------------------------------------
// Observe / Execute dispatch
// ---------------------------------------------------------------------------

// Observe runs one typed Git query and returns the matching typed facts.
func (g *GitFlow) Observe(ctx context.Context, q GitQuery) (GitFacts, error) {
	switch q := q.(type) {
	case ProjectDiscovery:
		return g.projectDiscovery(ctx)
	case GitStatus:
		return g.gitStatus(ctx, q)
	case WorktreeList:
		return g.worktreeList(ctx)
	case CommitInspect:
		return g.commitInspect(ctx, q)
	case RefLookup:
		return g.refLookup(ctx, q)
	case HistoryRange:
		return g.historyRange(ctx, q)
	case FingerprintObserve:
		return g.fingerprintObserve(ctx, q)
	case WorktreeInProgress:
		return g.worktreeInProgress(ctx, q)
	default:
		return nil, model.InvalidInputFault("gitflow: unknown git query")
	}
}

// Execute runs one typed Git operation. Every operation validates its
// inputs, verifies the expected repository state before mutating
// (compare-and-swap), and verifies the resulting state afterwards.
func (g *GitFlow) Execute(ctx context.Context, op GitOperation) (GitResult, error) {
	switch op := op.(type) {
	case CreatePlanningSnapshot:
		return g.createPlanningSnapshot(ctx, op)
	case CreateIntegration:
		return g.createIntegration(ctx, op)
	case CreateWorkspace:
		return g.createWorkspace(ctx, op)
	case CreateTask:
		return g.createTask(ctx, op)
	case CreateAuditRef:
		return g.createAuditRef(ctx, op)
	case CommitPreflight:
		return g.commitPreflight(ctx, op)
	case VerifyCommit:
		return g.verifyCommit(ctx, op)
	case MergeIntegration:
		return g.mergeIntegration(ctx, op)
	case CreateApply:
		return g.createApply(ctx, op)
	case UpdateRef:
		return g.updateRef(ctx, op)
	case CompleteMerge:
		return g.completeMerge(ctx, op)
	case RollbackMerge:
		return g.rollbackMerge(ctx, op)
	case RemoveWorktree:
		return g.removeWorktree(ctx, op)
	case MoveWorktree:
		return g.moveWorktree(ctx, op)
	case FastForwardWorkingTree:
		return g.fastForwardWorkingTree(ctx, op)
	default:
		return nil, model.InvalidInputFault("gitflow: unknown git operation")
	}
}

// ---------------------------------------------------------------------------
// Supervised Git runner
// ---------------------------------------------------------------------------

// run launches one argv-only git process in dir with the exact env and
// returns its framed stdout and stderr plus the typed exit fact. Output
// overflow is an error, never silent truncation.
func (g *GitFlow) run(ctx context.Context, dir string, env map[string]string, timeout time.Duration, args ...string) (stdout, stderr []byte, exit process.Exit, err error) {
	if dir == "" {
		dir = g.dir
	}
	h, events, err := g.sup.Start(ctx, process.ProcessSpec{
		Executable: g.git,
		Args:       args,
		Dir:        dir,
		Env:        env,
		Timeout:    timeout,
	})
	if err != nil {
		return nil, nil, process.Exit{}, model.InvalidInputFault("gitflow: cannot start git")
	}
	var out, errOut bytes.Buffer
	overflowed := false
	for ev := range events {
		switch ev.Kind {
		case process.EventFrameOut:
			out.Write(ev.Frame)
			out.WriteByte('\n')
		case process.EventFrameErr:
			errOut.Write(ev.Frame)
			errOut.WriteByte('\n')
		case process.EventOverflowOut, process.EventOverflowErr:
			overflowed = true
		}
	}
	exit, err = g.sup.Wait(ctx, h)
	if overflowed {
		return out.Bytes(), errOut.Bytes(), exit, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: git output exceeded the bounded budget")
	}
	return out.Bytes(), errOut.Bytes(), exit, err
}

// childEnv builds the exact environment for supervised git children: a
// curated allowlist forwarded from the parent plus fixed policy
// overrides. Nothing else leaks (design 13.1: no parent-environment
// inheritance beyond the allowlist).
func childEnv() map[string]string {
	forward := []string{
		"HOME", "PATH", "TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME",
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM",
		"SSH_AUTH_SOCK",
	}
	env := map[string]string{
		// Fixed policy: terminal prompts are always impossible.
		"GIT_TERMINAL_PROMPT": "0",
	}
	for _, key := range forward {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	// GIT_CONFIG_COUNT with its numbered KEY/VALUE slots is part of the
	// effective configuration and is forwarded verbatim.
	if count, ok := os.LookupEnv("GIT_CONFIG_COUNT"); ok {
		env["GIT_CONFIG_COUNT"] = count
		for i := 0; i < 1024; i++ {
			suffix := strconv.Itoa(i)
			key := "GIT_CONFIG_KEY_" + suffix
			if v, ok := os.LookupEnv(key); ok {
				env[key] = v
			} else {
				break
			}
			val := "GIT_CONFIG_VALUE_" + suffix
			if v, ok := os.LookupEnv(val); ok {
				env[val] = v
			}
		}
	}
	return env
}

// revParseHead resolves the current HEAD of dir, returning "" when the
// repository is unborn.
func (g *GitFlow) revParseHead(ctx context.Context, dir string, env map[string]string) (string, error) {
	out, _, exit, err := g.run(ctx, dir, env, defaultGitTimeout, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return "", nil // unborn
	}
	return strings.TrimSpace(string(out)), nil
}

// ---------------------------------------------------------------------------
// Project Key (PRD 启动与项目识别)
// ---------------------------------------------------------------------------

// ProjectKey derives the stable project key from the canonical repository
// root:
//
//	canonicalPath = realpath(gitRoot)
//	normalizedPath = Unicode NFC(canonicalPath)
//	slug = removeLeadingSlash -> replace path separators with dash
//	       -> replace unsafe chars with "_" -> truncate 80
//	hash = sha256(normalizedPath).substring(0, 10)
//	projectKey = slug + "--" + hash
//
// The collision pair /a-b/c and /a/b-c shares the slug "a-b-c" but never
// the hash, so full keys never collide. The hash covers the normalized
// path, so NFC/NFD-equivalent names produce the same key.
func ProjectKey(canonicalRoot string) string {
	normalized := norm.NFC.String(canonicalRoot)
	sum := sha256.Sum256([]byte(normalized))
	return slugify(normalized) + "--" + hex.EncodeToString(sum[:])[:10]
}

// slugify renders the readable slug: leading slash removed, path
// separators become dashes, everything outside the ASCII safe set becomes
// an underscore, truncated to 80 characters.
func slugify(normalized string) string {
	s := strings.TrimPrefix(normalized, "/")
	s = strings.ReplaceAll(s, string(filepath.Separator), "-")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// sortPaths gives deterministic fingerprint ordering.
func sortPaths(entries []PathEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
}
