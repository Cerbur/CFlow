package gitflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// repoRoot resolves the canonical repository root containing dir.
func (g *GitFlow) repoRoot(ctx context.Context, dir string, env map[string]string) (string, error) {
	out, _, exit, err := g.run(ctx, dir, env, defaultGitTimeout, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return "", model.InvalidInputFault("gitflow: not a git repository")
	}
	top := strings.TrimSpace(string(out))
	root, err := filepath.EvalSymlinks(top)
	if err != nil {
		return "", model.InvalidInputFault("gitflow: repository root cannot be resolved")
	}
	return root, nil
}

// projectDiscovery assembles the startup fact set (design 15.3): the
// canonical root, the attached local branch, HEAD with its
// unborn/detached classification, the user's working-tree status with
// Dirty Fingerprint, the worktree registry, and the Project Key.
func (g *GitFlow) projectDiscovery(ctx context.Context) (ProjectFacts, error) {
	env := childEnv()
	root, err := g.repoRoot(ctx, g.dir, env)
	if err != nil {
		return ProjectFacts{}, err
	}

	// Attached local branch (PRD: only refs/heads counts as attached).
	branch := ""
	out, _, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return ProjectFacts{}, err
	}
	if exit.Fact == process.FactProcessExit && exit.Code == 0 {
		sym := strings.TrimSpace(string(out))
		if strings.HasPrefix(sym, "refs/heads/") {
			branch = strings.TrimPrefix(sym, "refs/heads/")
		}
	}

	head, err := g.revParseHead(ctx, g.dir, env)
	if err != nil {
		return ProjectFacts{}, err
	}

	unborn, detached := false, false
	switch {
	case branch != "" && head != "":
		// Attached to a local branch with a valid HEAD: the normal case.
	case branch != "" && head == "":
		unborn = true
	case branch == "" && head != "":
		detached = true
	default:
		unborn = true // no branch and no HEAD: an unborn repository
	}

	status, err := g.gitStatusAt(ctx, root, env, "")
	if err != nil {
		return ProjectFacts{}, err
	}
	wf, err := g.worktreeList(ctx)
	if err != nil {
		return ProjectFacts{}, err
	}
	return ProjectFacts{
		Root:       root,
		Branch:     branch,
		Head:       head,
		Unborn:     unborn,
		Detached:   detached,
		Status:     status,
		Worktrees:  wf.Entries,
		ProjectKey: ProjectKey(root),
	}, nil
}

// gitStatus observes one working tree's porcelain-v2 status.
func (g *GitFlow) gitStatus(ctx context.Context, q GitStatus) (StatusFacts, error) {
	dir := g.dir
	if q.Dir != "" {
		canon, err := canonicalDir(q.Dir)
		if err != nil {
			return StatusFacts{}, err
		}
		dir = canon
	}
	if q.ExpectedHead != "" {
		if err := validateHead(q.ExpectedHead); err != nil {
			return StatusFacts{}, err
		}
	}
	return g.gitStatusAt(ctx, dir, childEnv(), q.ExpectedHead)
}

// gitStatusAt reads and parses `git status --porcelain=v2 -z` in dir and
// computes the Dirty Fingerprint. expectedHead, when set, must equal the
// actual HEAD (fail-closed expected-HEAD mismatch).
func (g *GitFlow) gitStatusAt(ctx context.Context, dir string, env map[string]string, expectedHead string) (StatusFacts, error) {
	head, err := g.revParseHead(ctx, dir, env)
	if err != nil {
		return StatusFacts{}, err
	}
	if expectedHead != "" && head != expectedHead {
		return StatusFacts{}, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: HEAD does not match the expected commit")
	}
	out, _, exit, err := g.run(ctx, dir, env, defaultGitTimeout, "status", "--porcelain=v2", "-z")
	if err != nil {
		return StatusFacts{}, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return StatusFacts{}, model.InvalidInputFault("gitflow: not a git repository")
	}
	staged, unstaged, untracked, err := parsePorcelainV2(out)
	if err != nil {
		return StatusFacts{}, err
	}
	dirty, err := g.dirtyFingerprint(staged, unstaged, untracked, dir)
	if err != nil {
		return StatusFacts{}, err
	}
	return StatusFacts{
		Dir:       dir,
		Head:      head,
		Staged:    staged,
		Unstaged:  unstaged,
		Untracked: untracked,
		Dirty:     dirty,
	}, nil
}

// parsePorcelainV2 parses `git status --porcelain=v2 -z` into the
// tracked/staged/untracked classifications. Paths are the last
// space-separated field of each entry, so paths containing spaces survive
// verbatim; rename/copy entries carry their original path in the
// following NUL field. An unexpected entry fails closed rather than being
// silently dropped.
func parsePorcelainV2(out []byte) (staged, unstaged, untracked []PathEntry, err error) {
	// -z output is NUL-terminated and contains no newlines; the runner
	// joins frames with '\n', so the trailing separator is trimmed.
	out = trimTrailingNewline(out)
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		switch {
		case strings.HasPrefix(f, "# "):
			// Defensive: -z output carries no header lines.
		case strings.HasPrefix(f, "? "):
			parts := strings.SplitN(f, " ", 2)
			if len(parts) != 2 {
				return nil, nil, nil, unparsableStatus()
			}
			untracked = append(untracked, PathEntry{Path: cleanRel(parts[1]), X: '?', Y: '?'})
		case strings.HasPrefix(f, "u "):
			parts := strings.SplitN(f, " ", 10)
			if len(parts) != 10 || len(parts[1]) != 2 {
				return nil, nil, nil, unparsableStatus()
			}
			e := PathEntry{Path: cleanRel(parts[9]), X: parts[1][0], Y: parts[1][1], Mode: parts[4]}
			classify(e, &staged, &unstaged)
		case strings.HasPrefix(f, "1 "):
			parts := strings.SplitN(f, " ", 9)
			if len(parts) != 9 || len(parts[1]) != 2 {
				return nil, nil, nil, unparsableStatus()
			}
			// 1 XY sub mH mI mW hH hI path
			e := PathEntry{Path: cleanRel(parts[8]), X: parts[1][0], Y: parts[1][1], Hash: parts[7], Mode: parts[5]}
			classify(e, &staged, &unstaged)
		case strings.HasPrefix(f, "2 "):
			parts := strings.SplitN(f, " ", 10)
			if len(parts) != 10 || len(parts[1]) != 2 {
				return nil, nil, nil, unparsableStatus()
			}
			// 2 XY sub mH mI mW hH hI X<score> path \0 origPath
			e := PathEntry{Path: cleanRel(parts[9]), X: parts[1][0], Y: parts[1][1], Hash: parts[7], Mode: parts[5]}
			if i+1 < len(fields) {
				e.Original = cleanRel(fields[i+1])
				i++
			}
			classify(e, &staged, &unstaged)
		default:
			return nil, nil, nil, unparsableStatus()
		}
	}
	return staged, unstaged, untracked, nil
}

func unparsableStatus() error {
	return model.NewFault(model.CodeStateInvariantViolation, "gitflow: unparsable status entry")
}

// classify routes one tracked entry to its staged/unstaged lists by the
// porcelain X/Y codes.
func classify(e PathEntry, staged, unstaged *[]PathEntry) {
	if e.X != '.' {
		*staged = append(*staged, e)
	}
	if e.Y != '.' {
		*unstaged = append(*unstaged, e)
	}
}

// cleanRel strips the trailing slash git appends to untracked directories.
func cleanRel(p string) string {
	return strings.TrimSuffix(p, "/")
}

// trimTrailingNewline removes the single '\n' the runner appends after
// the final frame of a NUL-terminated stream.
func trimTrailingNewline(b []byte) []byte {
	return bytes.TrimSuffix(b, []byte("\n"))
}

// dirtyFingerprint normalizes the classifications into three stable
// hashes plus a combined fingerprint (PRD 已确认：用户当前工作区隔离).
// Untracked content is streamed through SHA-256 here and never stored.
func (g *GitFlow) dirtyFingerprint(staged, unstaged, untracked []PathEntry, base string) (DirtyFingerprint, error) {
	sortPaths(staged)
	sortPaths(unstaged)
	sortPaths(untracked)

	var stagedLines, unstagedLines, untrackedLines strings.Builder
	for _, e := range staged {
		stagedLines.WriteString(e.Path)
		stagedLines.WriteByte(0)
		stagedLines.WriteByte(e.X)
		stagedLines.WriteByte(0)
		stagedLines.WriteString(e.Hash)
		stagedLines.WriteByte(0)
		stagedLines.WriteString(e.Mode)
		stagedLines.WriteByte(0)
		stagedLines.WriteString(e.Original)
		stagedLines.WriteByte('\n')
	}
	for _, e := range unstaged {
		unstagedLines.WriteString(e.Path)
		unstagedLines.WriteByte(0)
		unstagedLines.WriteByte(e.Y)
		unstagedLines.WriteByte(0)
		unstagedLines.WriteString(e.Mode)
		unstagedLines.WriteByte(0)
		unstagedLines.WriteString(e.Original)
		unstagedLines.WriteByte('\n')
	}
	for _, e := range untracked {
		content, err := untrackedContentHash(filepath.Join(base, e.Path))
		if err != nil {
			return DirtyFingerprint{}, model.NewFault(model.CodeStateInvariantViolation,
				"gitflow: cannot hash untracked content")
		}
		untrackedLines.WriteString(e.Path)
		untrackedLines.WriteByte(0)
		untrackedLines.WriteString(content)
		untrackedLines.WriteByte('\n')
	}

	stagedHash := sha256Hex(stagedLines.String())
	unstagedHash := sha256Hex(unstagedLines.String())
	untrackedHash := sha256Hex(untrackedLines.String())
	return DirtyFingerprint{
		Algorithm:      "cflow-dirty-v1",
		StagedHash:     stagedHash,
		UnstagedHash:   unstagedHash,
		UntrackedHash:  untrackedHash,
		Combined:       sha256Hex(stagedHash + unstagedHash + untrackedHash),
		StagedCount:    len(staged),
		UnstagedCount:  len(unstaged),
		UntrackedCount: len(untracked),
	}, nil
}

// untrackedContentHash hashes one untracked path without following
// symlinks (security guard model: never follow a symlink out of the
// managed tree). Files are streamed; directories are walked in lexical
// order and hashed over their relative paths, kinds, and child hashes.
func untrackedContentHash(abs string) (string, error) {
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	switch {
	case info.Mode().IsRegular():
		h := sha256.New()
		f, err := os.Open(abs)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		return "f:" + hex.EncodeToString(h.Sum(nil)), nil
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return "", err
		}
		return "l:" + sha256Hex(target), nil
	case info.IsDir():
		var lines []string
		err := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == abs {
				return nil
			}
			rel := strings.TrimPrefix(p, abs+string(filepath.Separator))
			switch {
			case d.IsDir():
				return nil // children carry the directory's identity
			case d.Type()&fs.ModeSymlink != 0:
				target, err := os.Readlink(p)
				if err != nil {
					return err
				}
				lines = append(lines, rel+"\x00l:"+sha256Hex(target))
			case d.Type().IsRegular():
				child, err := untrackedContentHash(p)
				if err != nil {
					return err
				}
				lines = append(lines, rel+"\x00"+child)
			default:
				lines = append(lines, rel+"\x00o:"+d.Type().String())
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		return "d:" + sha256Hex(strings.Join(lines, "\n")), nil
	default:
		return "o:" + info.Mode().String(), nil
	}
}

// worktreeList parses `git worktree list --porcelain -z` (design 15.3:
