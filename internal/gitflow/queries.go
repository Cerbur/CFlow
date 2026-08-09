package gitflow

import (
	"context"
	"strconv"
	"strings"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// Standalone observation queries (design 15.3): the worktree registry,
// commit facts, ref existence, and exact history ranges.

// worktreeList parses `git worktree list --porcelain -z` (design 15.3:
// worktree registry entries).
func (g *GitFlow) worktreeList(ctx context.Context) (WorktreeFacts, error) {
	out, _, exit, err := g.run(ctx, g.dir, childEnv(), defaultGitTimeout, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return WorktreeFacts{}, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return WorktreeFacts{}, model.InvalidInputFault("gitflow: not a git repository")
	}
	out = trimTrailingNewline(out)
	var entries []WorktreeEntry
	var cur *WorktreeEntry
	for _, f := range strings.Split(string(out), "\x00") {
		switch {
		case f == "":
			if cur != nil {
				entries = append(entries, *cur)
				cur = nil
			}
		case strings.HasPrefix(f, "worktree "):
			cur = &WorktreeEntry{Path: strings.TrimPrefix(f, "worktree ")}
		case strings.HasPrefix(f, "HEAD "):
			head := strings.TrimPrefix(f, "HEAD ")
			if isAllZeros(head) {
				head = "" // unborn worktree
			}
			cur.Head = head
		case strings.HasPrefix(f, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(f, "branch "), "refs/heads/")
		case f == "detached":
			cur.Detached = true
		case f == "bare":
			cur.Bare = true
		case strings.HasPrefix(f, "locked"):
			cur.Locked = true
		case strings.HasPrefix(f, "prunable"):
			cur.Prunable = true
		}
	}
	if cur != nil {
		entries = append(entries, *cur)
	}
	return WorktreeFacts{Entries: entries}, nil
}

func isAllZeros(s string) bool {
	for _, r := range s {
		if r != '0' {
			return false
		}
	}
	return true
}

// commitInspect assembles one commit's structured evidence (design 15.3:
// author, committer, signature and signer facts; ancestry).
func (g *GitFlow) commitInspect(ctx context.Context, q CommitInspect) (CommitFacts, error) {
	if q.Ref != "HEAD" {
		if err := validateRefName(q.Ref); err != nil {
			return CommitFacts{}, err
		}
	}
	const format = "%H%x00%P%x00%an%x00%ae%x00%cn%x00%ce%x00%at%x00%ct%x00%s"
	out, _, exit, err := g.run(ctx, g.dir, childEnv(), defaultGitTimeout, "log", "-1", "--format="+format, q.Ref)
	if err != nil {
		return CommitFacts{}, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return CommitFacts{}, model.InvalidInputFault("gitflow: unknown commit ref")
	}
	parts := strings.Split(string(out), "\x00")
	if len(parts) != 9 {
		return CommitFacts{}, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: unparsable commit facts")
	}
	authorTime, err1 := strconv.ParseInt(parts[6], 10, 64)
	commitTime, err2 := strconv.ParseInt(parts[7], 10, 64)
	if err1 != nil || err2 != nil {
		return CommitFacts{}, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: unparsable commit timestamps")
	}
	head := parts[0]
	parents := strings.Fields(parts[1])
	signature, err := g.signatureFacts(ctx, head, childEnv())
	if err != nil {
		return CommitFacts{}, err
	}
	return CommitFacts{
		Ref:        q.Ref,
		Head:       head,
		Parents:    parents,
		Author:     Identity{Name: parts[2], Email: parts[3]},
		Committer:  Identity{Name: parts[4], Email: parts[5]},
		AuthorTime: authorTime,
		CommitTime: commitTime,
		Subject:    strings.TrimSpace(parts[8]),
		Signature:  signature,
	}, nil
}

// signatureFacts reads the gpgsig header (signature presence) and the
// verify-commit outcome (validity) for one commit.
func (g *GitFlow) signatureFacts(ctx context.Context, head string, env map[string]string) (SignatureFacts, error) {
	out, _, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "cat-file", "commit", head)
	if err != nil {
		return SignatureFacts{}, err
	}
	present := exit.Fact == process.FactProcessExit && exit.Code == 0 && hasGpgsig(out)
	verified := false
	if present {
		_, _, verifyExit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "verify-commit", head)
		if err != nil {
			return SignatureFacts{}, err
		}
		verified = verifyExit.Fact == process.FactProcessExit && verifyExit.Code == 0
	}
	return SignatureFacts{Present: present, Verified: verified}, nil
}

func hasGpgsig(raw []byte) bool {
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "gpgsig ") {
			return true
		}
	}
	return false
}

// ancestryCheck reports whether one commit is an ancestor of another with
// `git merge-base --is-ancestor <ancestor> <descendant>` (the adoption's
// new-commit closure). Exit 0 means ancestor, exit 1 means not; any other
// exit or a non-resolvable commit fails closed.
func (g *GitFlow) ancestryCheck(ctx context.Context, q AncestryCheck) (AncestryFacts, error) {
	if err := validateHead(q.Ancestor); err != nil {
		return AncestryFacts{}, err
	}
	if err := validateHead(q.Descendant); err != nil {
		return AncestryFacts{}, err
	}
	_, _, exit, err := g.run(ctx, g.dir, childEnv(), defaultGitTimeout, "merge-base", "--is-ancestor", q.Ancestor, q.Descendant)
	if err != nil {
		return AncestryFacts{}, err
	}
	facts := AncestryFacts{Ancestor: q.Ancestor, Descendant: q.Descendant}
	switch {
	case exit.Fact == process.FactProcessExit && exit.Code == 0:
		facts.AncestorOf = true
		return facts, nil
	case exit.Fact == process.FactProcessExit && exit.Code == 1:
		facts.AncestorOf = false
		return facts, nil
	default:
		return AncestryFacts{}, model.InvalidInputFault("gitflow: cannot resolve the ancestry check")
	}
}

// branchInspect observes one working tree's attached Branch and HEAD:
// `git symbolic-ref --quiet HEAD` for the attached local branch (only
// refs/heads counts as attached) and `git rev-parse --verify HEAD` for the
// full HEAD hash. A detached or unborn working tree reports the detached
// Branch/HEAD facts, never an error.
func (g *GitFlow) branchInspect(ctx context.Context, q BranchInspect) (BranchFacts, error) {
	dir := g.dir
	if q.Dir != "" {
		canon, err := canonicalDir(q.Dir)
		if err != nil {
			return BranchFacts{}, err
		}
		dir = canon
	}
	env := childEnv()
	branch := ""
	out, _, exit, err := g.run(ctx, dir, env, defaultGitTimeout, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return BranchFacts{}, err
	}
	if exit.Fact == process.FactProcessExit && exit.Code == 0 {
		sym := strings.TrimSpace(string(out))
		if strings.HasPrefix(sym, "refs/heads/") {
			branch = strings.TrimPrefix(sym, "refs/heads/")
		}
	}
	head, err := g.revParseHead(ctx, dir, env)
	if err != nil {
		return BranchFacts{}, err
	}
	return BranchFacts{
		Dir:      dir,
		Branch:   branch,
		Head:     head,
		Detached: branch == "" && head != "",
		Exists:   branch != "" || head != "",
	}, nil
}

// refLookup reports one ref's existence and expected value (design 15.3:
// ref existence and expected value).
func (g *GitFlow) refLookup(ctx context.Context, q RefLookup) (RefFacts, error) {
	if err := validateRefName(q.Ref); err != nil {
		return RefFacts{}, err
	}
	if q.Expected != "" {
		if err := validateHead(q.Expected); err != nil {
			return RefFacts{}, err
		}
	}
	out, _, exit, err := g.run(ctx, g.dir, childEnv(), defaultGitTimeout, "rev-parse", "--verify", "--quiet", q.Ref)
	if err != nil {
		return RefFacts{}, err
	}
	facts := RefFacts{Ref: q.Ref, Expected: q.Expected}
	switch {
	case exit.Fact == process.FactProcessExit && exit.Code == 0:
		facts.Exists = true
		facts.Value = strings.TrimSpace(string(out))
	case exit.Fact == process.FactProcessExit && exit.Code == 1:
		// Missing ref: an expected observation result, not a failure.
	default:
		return RefFacts{}, model.InvalidInputFault("gitflow: cannot resolve ref")
	}
	facts.Matches = facts.Exists && (q.Expected == "" || facts.Value == q.Expected)
	return facts, nil
}

// historyRange reports the exact commit list and changed paths of
// From..To (design 15.3: exact changed paths and commit range).
func (g *GitFlow) historyRange(ctx context.Context, q HistoryRange) (RangeFacts, error) {
	if err := validateHead(q.From); err != nil {
		return RangeFacts{}, err
	}
	if err := validateHead(q.To); err != nil {
		return RangeFacts{}, err
	}
	out, _, exit, err := g.run(ctx, g.dir, childEnv(), defaultGitTimeout, "rev-list", q.From+".."+q.To)
	if err != nil {
		return RangeFacts{}, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return RangeFacts{}, model.InvalidInputFault("gitflow: unknown commit in range")
	}
	var commits []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			commits = append(commits, line)
		}
	}
	pathsOut, _, exit, err := g.run(ctx, g.dir, childEnv(), defaultGitTimeout, "diff", "--name-only", "-z", q.From+".."+q.To)
	if err != nil {
		return RangeFacts{}, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return RangeFacts{}, model.InvalidInputFault("gitflow: unknown commit in range")
	}
	var paths []string
	for _, p := range strings.Split(string(trimTrailingNewline(pathsOut)), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return RangeFacts{From: q.From, To: q.To, Commits: commits, ChangedPaths: paths}, nil
}
