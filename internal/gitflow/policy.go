package gitflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// Commit Identity and Signing Preflight (PRD 已确认：Git Commit Identity
// 与 Signing Preflight). Before any commit-capable operation the
// Application runs CommitPreflight; afterwards it verifies the actual
// Commit evidence with VerifyCommit (design 15.4). GitFlow never writes
// Git config, never disables signing, never falls back to unsigned
// commits, and never reads or copies private keys or passphrases.

// commitPreflight resolves the effective author/committer identity and
// signing policy, normalizes them into a stable policy fingerprint
// (timestamps, temporary paths, and secrets excluded), runs the isolated
// non-interactive signed probe when signing is enabled, and returns the
// immutable Preflight evidence.
func (g *GitFlow) commitPreflight(ctx context.Context, op CommitPreflight) (GitResult, error) {
	if op.Revision == "" {
		return nil, model.InvalidInputFault("gitflow: preflight revision is required")
	}
	timeout := op.ProbeTimeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	env := childEnv()

	versionOut, _, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "--version")
	if err != nil {
		return nil, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, identityNotConfigured()
	}
	gitVersion := strings.TrimSpace(strings.TrimPrefix(string(versionOut), "git version "))

	author, err := g.resolveIdent(ctx, env, "GIT_AUTHOR_IDENT")
	if err != nil {
		return nil, err
	}
	committer, err := g.resolveIdent(ctx, env, "GIT_COMMITTER_IDENT")
	if err != nil {
		return nil, err
	}
	if author.Source, err = g.identitySource(ctx, env, "GIT_AUTHOR_NAME", "user.name"); err != nil {
		return nil, err
	}
	if committer.Source, err = g.identitySource(ctx, env, "GIT_COMMITTER_NAME", "user.name"); err != nil {
		return nil, err
	}

	policy, err := g.signingPolicy(ctx, env)
	if err != nil {
		return nil, err
	}

	fingerprint := commitPolicyFingerprint(g.git, gitVersion, author, committer, policy)

	var probe ProbeFacts
	if policy.Enabled {
		probe, err = g.probeSigning(ctx, env, author, committer, policy, timeout)
		if err != nil {
			return nil, err
		}
	} else {
		probe = ProbeFacts{Required: false}
	}

	ev := PreflightEvidence{
		Revision:          op.Revision,
		PolicyFingerprint: fingerprint,
		GitVersion:        gitVersion,
		ResolvedAt:        time.Now().UTC().Format(time.RFC3339),
		Author:            author,
		Committer:         committer,
		Signing:           policy,
		Probe:             probe,
	}
	ev.EvidenceHash = evidenceHash(ev)
	return ev, nil
}

// resolveIdent resolves one effective identity through `git var`, which
// honors the forwarded environment first and the repository's effective
// configuration second. Missing or illegal identities block with
// GIT_IDENTITY_NOT_CONFIGURED.
func (g *GitFlow) resolveIdent(ctx context.Context, env map[string]string, varName string) (Identity, error) {
	out, _, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "var", varName)
	if err != nil {
		return Identity{}, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return Identity{}, identityNotConfigured()
	}
	ident, err := parseIdent(strings.TrimSpace(string(out)))
	if err != nil {
		return Identity{}, identityNotConfigured()
	}
	return ident, nil
}

// parseIdent parses the `git var` ident format "Name <email> ts tz" and
// validates both parts. The timestamp and timezone are dropped: they are
// never part of the policy.
func parseIdent(line string) (Identity, error) {
	lt := strings.Index(line, " <")
	if lt < 0 {
		return Identity{}, errors.New("ident has no email")
	}
	name := line[:lt]
	rest := line[lt+2:]
	gt := strings.Index(rest, ">")
	if gt < 0 {
		return Identity{}, errors.New("ident has no closing bracket")
	}
	email := rest[:gt]
	if name == "" || email == "" {
		return Identity{}, errors.New("ident is empty")
	}
	if !strings.Contains(email, "@") {
		return Identity{}, errors.New("ident email is illegal")
	}
	for _, r := range name + email {
		if r < 0x20 || r == 0x7f {
			return Identity{}, errors.New("ident contains a control character")
		}
	}
	if strings.ContainsAny(name, "<>") || strings.ContainsAny(email, "<>") {
		return Identity{}, errors.New("ident contains an illegal character")
	}
	return Identity{Name: name, Email: email}, nil
}

// identitySource records where an identity value came from: "env" when
// the environment overrides it, otherwise the effective config scope.
func (g *GitFlow) identitySource(ctx context.Context, env map[string]string, envKey, configKey string) (string, error) {
	if _, ok := env[envKey]; ok {
		return "env", nil
	}
	_, scope, set, err := g.configValue(ctx, env, configKey)
	if err != nil {
		return "", err
	}
	if set {
		return scope, nil
	}
	return "", nil // git's own fallback (e.g. the OS account name)
}

// configValue reads one effective config value with its scope.
// set=false means the key is absent (exit 1), which is data, not an
// error.
func (g *GitFlow) configValue(ctx context.Context, env map[string]string, key string) (value, scope string, set bool, err error) {
	out, _, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "config", "--show-scope", "--get", key)
	if err != nil {
		return "", "", false, err
	}
	switch {
	case exit.Fact == process.FactProcessExit && exit.Code == 0:
		line := strings.TrimSpace(string(out))
		if i := strings.Index(line, "\t"); i >= 0 {
			return line[i+1:], line[:i], true, nil
		}
		return line, "", true, nil
	case exit.Fact == process.FactProcessExit && exit.Code == 1:
		return "", "", false, nil
	default:
		return "", "", false, model.InvalidInputFault("gitflow: git configuration cannot be read")
	}
}

// signingPolicy resolves the effective commit signing policy: mode,
// format, key, signer program, and each key's config scope for
// diagnostics.
func (g *GitFlow) signingPolicy(ctx context.Context, env map[string]string) (SigningPolicy, error) {
	origins := map[string]string{}
	record := func(key, scope string) {
		if scope != "" {
			origins[key] = scope
		}
	}

	enabled := false
	if v, scope, set, err := g.configValue(ctx, env, "commit.gpgsign"); err != nil {
		return SigningPolicy{}, err
	} else if set {
		record("commit.gpgsign", scope)
		enabled = v == "true"
	}

	format := "openpgp"
	if v, scope, set, err := g.configValue(ctx, env, "gpg.format"); err != nil {
		return SigningPolicy{}, err
	} else if set {
		record("gpg.format", scope)
		format = v
	}

	key := ""
	if v, scope, set, err := g.configValue(ctx, env, "user.signingkey"); err != nil {
		return SigningPolicy{}, err
	} else if set {
		record("user.signingkey", scope)
		key = v
	}

	programKey, defaultProgram := "gpg.program", "gpg"
	if format == "ssh" {
		programKey, defaultProgram = "gpg.ssh.program", "ssh-keygen"
	}
	program := defaultProgram
	if v, scope, set, err := g.configValue(ctx, env, programKey); err != nil {
		return SigningPolicy{}, err
	} else if set {
		record(programKey, scope)
		program = v
	}

	return SigningPolicy{
		Enabled: enabled,
		Format:  format,
		Key:     key,
		Program: program,
		Origins: origins,
	}, nil
}

// commitPolicyFingerprint normalizes the effective identity and signing
// policy into a stable fingerprint. Timestamps, temporary paths, and
// secrets are excluded by construction: only the resolved values that
// define the Commit policy are hashed.
func commitPolicyFingerprint(gitExe, gitVersion string, author, committer Identity, pol SigningPolicy) string {
	s := strings.Join([]string{
		"cflow.commit-policy.v1",
		gitExe,
		gitVersion,
		author.Name,
		author.Email,
		committer.Name,
		committer.Email,
		strconv.FormatBool(pol.Enabled),
		pol.Format,
		pol.Key,
		pol.Program,
	}, "\n")
	return sha256Hex(s)
}

// probeSigning proves that the resolved signer is usable non-interactively
// (PRD step 3-4): a signed commit is created in a CFlow-managed temporary
// repository with the same git executable, the inherited environment, and
// the resolved identity and signing policy passed explicitly via -c flags
// and environment overrides. The target repository's refs and worktrees
// are never touched, stdin is closed, no TTY exists, and a hard timeout
// bounds the probe. Failure, timeout, or an unsigned result blocks with
// GIT_SIGNING_PREFLIGHT_FAILED; there is no unsigned fallback.
func (g *GitFlow) probeSigning(ctx context.Context, env map[string]string, author, committer Identity, pol SigningPolicy, timeout time.Duration) (ProbeFacts, error) {
	probeEnv := map[string]string{}
	for k, v := range env {
		probeEnv[k] = v
	}
	probeEnv["GIT_CONFIG_NOSYSTEM"] = "1"
	probeEnv["GIT_CONFIG_GLOBAL"] = "/dev/null"
	probeEnv["GIT_AUTHOR_NAME"] = author.Name
	probeEnv["GIT_AUTHOR_EMAIL"] = author.Email
	probeEnv["GIT_COMMITTER_NAME"] = committer.Name
	probeEnv["GIT_COMMITTER_EMAIL"] = committer.Email

	tmp, err := os.MkdirTemp("", "cflow-git-probe-")
	if err != nil {
		return ProbeFacts{}, signingPreflightFailed()
	}
	defer os.RemoveAll(tmp)

	if _, _, exit, err := g.run(ctx, tmp, probeEnv, defaultGitTimeout, "init", "-q"); err != nil {
		return ProbeFacts{}, err
	} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return ProbeFacts{}, signingPreflightFailed()
	}

	// Resolve a relative ssh signing key against the target repository
	// root; the probe repository itself has no such file.
	key := pol.Key
	if pol.Format == "ssh" && key != "" && !filepath.IsAbs(key) {
		if root, err := g.repoRoot(ctx, g.dir, env); err == nil {
			if _, err := os.Stat(filepath.Join(root, key)); err == nil {
				key = filepath.Join(root, key)
			}
		}
	}

	args := []string{"-c", "commit.gpgsign=true", "-c", "gpg.format=" + pol.Format}
	if pol.Format == "ssh" {
		args = append(args, "-c", "gpg.ssh.program="+pol.Program)
	} else {
		args = append(args, "-c", "gpg.program="+pol.Program)
	}
	if key != "" {
		args = append(args, "-c", "user.signingkey="+key)
	}
	args = append(args, "commit", "--allow-empty", "-m", "cflow signing probe")

	start := time.Now()
	_, _, exit, err := g.run(ctx, tmp, probeEnv, timeout, args...)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		return ProbeFacts{}, err
	}
	if exit.Fact == process.FactTimeout {
		return ProbeFacts{Ran: true, Success: false, Exit: -1, Fact: "timeout", DurationMs: duration}, signingPreflightFailed()
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return ProbeFacts{Ran: true, Success: false, Exit: exit.Code, Fact: "exit", DurationMs: duration}, signingPreflightFailed()
	}

	// The probe must prove a signature was actually produced, not merely
	// that git exited zero.
	shaOut, _, exit, err := g.run(ctx, tmp, probeEnv, defaultGitTimeout, "rev-parse", "HEAD")
	if err != nil {
		return ProbeFacts{}, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return ProbeFacts{}, signingPreflightFailed()
	}
	catOut, _, exit, err := g.run(ctx, tmp, probeEnv, defaultGitTimeout, "cat-file", "commit", strings.TrimSpace(string(shaOut)))
	if err != nil {
		return ProbeFacts{}, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 || !hasGpgsig(catOut) {
		return ProbeFacts{}, signingPreflightFailed()
	}
	return ProbeFacts{Required: true, Ran: true, Success: true, Exit: 0, Fact: "exit", DurationMs: duration}, nil
}

// verifyCommit verifies one commit's actual author, committer, and
// signing evidence against the approved Preflight policy (design 15.4).
// Any drift blocks with COMMIT_POLICY_MISMATCH; the caller closes the
// Dispatch Gate on that fault.
func (g *GitFlow) verifyCommit(ctx context.Context, op VerifyCommit) (GitResult, error) {
	cf, err := g.commitInspect(ctx, CommitInspect{Ref: op.Ref})
	if err != nil {
		return nil, err
	}
	var drift []string
	if cf.Author.Name != op.ExpectedAuthor.Name || cf.Author.Email != op.ExpectedAuthor.Email {
		drift = append(drift, "author identity changed")
	}
	if cf.Committer.Name != op.ExpectedCommitter.Name || cf.Committer.Email != op.ExpectedCommitter.Email {
		drift = append(drift, "committer identity changed")
	}
	if cf.Signature.Present != op.ExpectedSigning.Enabled {
		drift = append(drift, "signature presence changed")
	}
	if len(drift) > 0 {
		return nil, model.NewFault(model.CodeCommitPolicyMismatch, "gitflow: "+strings.Join(drift, ", "))
	}
	return VerifyCommitResult{Commit: cf}, nil
}

// evidenceHash binds the exact evidence revision: a canonical JSON
// serialization of the evidence (map keys marshal deterministically).
func evidenceHash(ev PreflightEvidence) string {
	ev.EvidenceHash = ""
	b, err := json.Marshal(ev)
	if err != nil {
		return ""
	}
	return sha256Hex(string(b))
}

func identityNotConfigured() error {
	return model.NewFault(model.CodeGitIdentityNotConfigured, "gitflow: commit identity is not configured")
}

func signingPreflightFailed() error {
	return model.NewFault(model.CodeGitSigningPreflightFailed, "gitflow: signing preflight probe failed")
}

// fingerprintObserve recomputes the effective Commit Policy fingerprint
// without the signing probe (PRD 已确认：Commit Policy 漂移立即安全停止 step
// 5): the monitor reads only the public effective configuration — never
// Signer Secrets — and never runs a signature probe per poll. The result
// is the normalized fingerprint plus the effective identity and signing
// policy facts for evidence.
func (g *GitFlow) fingerprintObserve(ctx context.Context, op FingerprintObserve) (GitFacts, error) {
	if op.Revision == "" {
		return nil, model.InvalidInputFault("gitflow: fingerprint observation revision is required")
	}
	env := childEnv()
	versionOut, _, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "--version")
	if err != nil {
		return nil, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, identityNotConfigured()
	}
	gitVersion := strings.TrimSpace(strings.TrimPrefix(string(versionOut), "git version "))
	author, err := g.resolveIdent(ctx, env, "GIT_AUTHOR_IDENT")
	if err != nil {
		return nil, err
	}
	committer, err := g.resolveIdent(ctx, env, "GIT_COMMITTER_IDENT")
	if err != nil {
		return nil, err
	}
	policy, err := g.signingPolicy(ctx, env)
	if err != nil {
		return nil, err
	}
	facts := FingerprintFacts{
		Revision:          op.Revision,
		PolicyFingerprint: commitPolicyFingerprint(g.git, gitVersion, author, committer, policy),
		GitVersion:        gitVersion,
		Author:            author,
		Committer:         committer,
		Signing:           policy,
	}
	facts.EvidenceHash = fingerprintFactsHash(facts)
	return facts, nil
}

// fingerprintFactsHash binds the exact observation evidence: a canonical
// JSON serialization of the facts (map keys marshal deterministically).
func fingerprintFactsHash(f FingerprintFacts) string {
	f.EvidenceHash = ""
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	return sha256Hex(string(b))
}
