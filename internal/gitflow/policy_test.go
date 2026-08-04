package gitflow_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// preflight runs the Commit Preflight through the repo-bound GitFlow.
func preflight(t *testing.T, repo *Repo, rev string, timeout time.Duration) gitflow.PreflightEvidence {
	t.Helper()
	res, err := repo.flow().Execute(context.Background(), gitflow.CommitPreflight{
		Revision:     rev,
		ProbeTimeout: timeout,
	})
	if err != nil {
		t.Fatalf("commit preflight: %v", err)
	}
	ev, ok := res.(gitflow.PreflightEvidence)
	if !ok {
		t.Fatalf("preflight result type %T, want PreflightEvidence", res)
	}
	return ev
}

// enableSSHSigning configures the repository for ssh-format commit signing
// with the fixture key.
func enableSSHSigning(t *testing.T, repo *Repo) string {
	t.Helper()
	key := newSSHKey(t, repo)
	repo.git("config", "commit.gpgsign", "true")
	repo.git("config", "gpg.format", "ssh")
	repo.git("config", "user.signingkey", key)
	return key
}

func TestPreflightResolvesIdentityAndSigningPolicy(t *testing.T) {
	repo := newCommittedRepo(t)
	ev := preflight(t, repo, "test-1", 0)

	if ev.Revision != "test-1" {
		t.Fatalf("revision = %q", ev.Revision)
	}
	if ev.Author.Name != "Test User" || ev.Author.Email != "test@example.com" {
		t.Fatalf("author = %+v, want Test User", ev.Author)
	}
	if ev.Committer != ev.Author {
		t.Fatalf("committer = %+v, want author", ev.Committer)
	}
	if ev.Signing.Enabled {
		t.Fatal("signing enabled without commit.gpgsign")
	}
	if ev.Probe.Required || ev.Probe.Ran {
		t.Fatal("probe ran for an unsigned policy")
	}
	if ev.PolicyFingerprint == "" || ev.EvidenceHash == "" {
		t.Fatal("missing fingerprint or evidence hash")
	}
	if ev.GitVersion == "" {
		t.Fatal("missing git version")
	}
}

func TestPreflightMissingIdentity(t *testing.T) {
	repo := newCommittedRepo(t)
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	_, err := repo.flow().Execute(context.Background(), gitflow.CommitPreflight{Revision: "missing"})
	if code := faultCode(t, err); code != model.CodeGitIdentityNotConfigured {
		t.Fatalf("missing identity code = %s, want GIT_IDENTITY_NOT_CONFIGURED", code)
	}
}

func TestPreflightRequiresRevision(t *testing.T) {
	repo := newCommittedRepo(t)
	_, err := repo.flow().Execute(context.Background(), gitflow.CommitPreflight{})
	if code := faultCode(t, err); code != model.CodeInvalidInput {
		t.Fatalf("empty revision code = %s, want INVALID_INPUT", code)
	}
}

func TestPreflightFingerprintExcludesTimestampsAndTempPaths(t *testing.T) {
	repo := newCommittedRepo(t)
	t.Setenv("GIT_AUTHOR_DATE", "2000-01-01T00:00:00Z")
	a := preflight(t, repo, "t1", 0)
	t.Setenv("GIT_AUTHOR_DATE", "2030-01-01T00:00:00Z")
	b := preflight(t, repo, "t2", 0)

	if a.PolicyFingerprint != b.PolicyFingerprint {
		t.Fatal("fingerprint changed with the ident timestamp")
	}
	if strings.Contains(a.PolicyFingerprint, repo.Tmp) {
		t.Fatal("fingerprint leaks a temporary path")
	}
	if strings.Contains(a.PolicyFingerprint, "2000-01-01") || strings.Contains(b.PolicyFingerprint, "2030-01-01") {
		t.Fatal("fingerprint leaks a timestamp")
	}
}

func TestPreflightFingerprintChangesOnIdentity(t *testing.T) {
	repo := newCommittedRepo(t)
	a := preflight(t, repo, "a", 0)
	t.Setenv("GIT_AUTHOR_NAME", "Someone Else")
	t.Setenv("GIT_AUTHOR_EMAIL", "else@example.com")
	b := preflight(t, repo, "b", 0)
	if a.PolicyFingerprint == b.PolicyFingerprint {
		t.Fatal("fingerprint did not change with identity")
	}
}

func TestPreflightFingerprintChangesOnSigningPolicy(t *testing.T) {
	repo := newCommittedRepo(t)
	a := preflight(t, repo, "a", 0)
	enableSSHSigning(t, repo)
	b := preflight(t, repo, "b", 0)
	if a.PolicyFingerprint == b.PolicyFingerprint {
		t.Fatal("fingerprint did not change with signing policy")
	}
	if b.Signing.Format != "ssh" || b.Signing.Key == "" {
		t.Fatalf("signing policy = %+v, want ssh format with a key", b.Signing)
	}
}

func TestPreflightProbeSuccessWithSSHKey(t *testing.T) {
	repo := newCommittedRepo(t)
	enableSSHSigning(t, repo)
	headBefore := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	configBefore := strings.TrimSpace(string(repo.git("config", "--local", "--list")))

	ev := preflight(t, repo, "ssh-1", 0)

	if !ev.Signing.Enabled || ev.Signing.Format != "ssh" {
		t.Fatalf("signing policy = %+v", ev.Signing)
	}
	if !ev.Probe.Required || !ev.Probe.Ran || !ev.Probe.Success {
		t.Fatalf("probe facts = %+v, want a successful run", ev.Probe)
	}
	if ev.Probe.Exit != 0 {
		t.Fatalf("probe exit = %d, want 0", ev.Probe.Exit)
	}
	// The probe must not touch the target repository: no ref movement, no
	// config writes, no new commits.
	if head := strings.TrimSpace(string(repo.git("rev-parse", "HEAD"))); head != headBefore {
		t.Fatalf("target HEAD moved to %q, want %q", head, headBefore)
	}
	if cfg := strings.TrimSpace(string(repo.git("config", "--local", "--list"))); cfg != configBefore {
		t.Fatalf("local config changed by preflight:\nbefore: %s\nafter: %s", configBefore, cfg)
	}
	if commits := strings.TrimSpace(string(repo.git("rev-list", "--count", "HEAD"))); commits != "1" {
		t.Fatalf("target gained commits: %s", commits)
	}
	if _, err := os.Stat(filepath.Join(repo.Root, "cflow-signing-probe.txt")); err == nil {
		t.Fatal("probe leaked a file into the target repository")
	}
}

func TestPreflightProbeTimeout(t *testing.T) {
	repo := newCommittedRepo(t)
	script := filepath.Join(repo.Tmp, "slow-signer.sh")
	writeFile(t, script, "#!/bin/sh\nsleep 30\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	repo.git("config", "commit.gpgsign", "true")
	repo.git("config", "gpg.format", "ssh")
	repo.git("config", "user.signingkey", filepath.Join(repo.Tmp, "missing-key"))
	repo.git("config", "gpg.ssh.program", script)

	start := time.Now()
	_, err := repo.flow().Execute(context.Background(), gitflow.CommitPreflight{
		Revision:     "timeout",
		ProbeTimeout: 1 * time.Second,
	})
	if code := faultCode(t, err); code != model.CodeGitSigningPreflightFailed {
		t.Fatalf("probe timeout code = %s, want GIT_SIGNING_PREFLIGHT_FAILED", code)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("probe timeout took %v, want a hard bounded timeout", elapsed)
	}
	// The target repository is untouched by the failed probe.
	if head := strings.TrimSpace(string(repo.git("rev-parse", "HEAD"))); head == "" {
		t.Fatal("target repository damaged by failed probe")
	}
}

func TestPreflightProbeFailureMissingKey(t *testing.T) {
	repo := newCommittedRepo(t)
	repo.git("config", "commit.gpgsign", "true")
	repo.git("config", "gpg.format", "ssh")
	repo.git("config", "user.signingkey", filepath.Join(repo.Tmp, "no-such-key"))
	_, err := repo.flow().Execute(context.Background(), gitflow.CommitPreflight{Revision: "badkey"})
	if code := faultCode(t, err); code != model.CodeGitSigningPreflightFailed {
		t.Fatalf("missing key code = %s, want GIT_SIGNING_PREFLIGHT_FAILED", code)
	}
}

func TestPreflightIsNonInteractive(t *testing.T) {
	repo := newCommittedRepo(t)
	// A signer that would block on a TTY prompt must fail the probe
	// within the bounded timeout instead of hanging or prompting.
	script := filepath.Join(repo.Tmp, "prompting-signer.sh")
	writeFile(t, script, "#!/bin/sh\nread answer\nexit 1\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	repo.git("config", "commit.gpgsign", "true")
	repo.git("config", "gpg.format", "ssh")
	repo.git("config", "user.signingkey", filepath.Join(repo.Tmp, "k"))
	repo.git("config", "gpg.ssh.program", script)

	done := make(chan error, 1)
	go func() {
		_, err := repo.flow().Execute(context.Background(), gitflow.CommitPreflight{
			Revision:     "interactive",
			ProbeTimeout: 2 * time.Second,
		})
		done <- err
	}()
	select {
	case err := <-done:
		if code := faultCode(t, err); code != model.CodeGitSigningPreflightFailed {
			t.Fatalf("interactive probe code = %s, want GIT_SIGNING_PREFLIGHT_FAILED", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("probe blocked on interactive input")
	}
}

// commitWith commits with an explicit environment override through the
// Supervisor.
func commitWith(t *testing.T, repo *Repo, extra map[string]string, args ...string) {
	t.Helper()
	env := testEnv()
	for k, v := range extra {
		env[k] = v
	}
	full := append([]string{"commit", "-q", "--allow-empty", "-m", "test commit"}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, exit, err := runProcess(ctx, repo.sup, repo.Git, repo.Root, env, full...)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		t.Fatalf("commit exited %+v: %s", exit, out)
	}
}

func TestVerifyCommitMatchesPreflight(t *testing.T) {
	repo := newCommittedRepo(t)
	ev := preflight(t, repo, "v1", 0)
	commitWith(t, repo, nil)

	res, err := repo.flow().Execute(context.Background(), gitflow.VerifyCommit{
		Ref:               "HEAD",
		ExpectedAuthor:    ev.Author,
		ExpectedCommitter: ev.Committer,
		ExpectedSigning:   ev.Signing,
	})
	if err != nil {
		t.Fatalf("verify commit: %v", err)
	}
	if _, ok := res.(gitflow.VerifyCommitResult); !ok {
		t.Fatalf("verify result type %T, want VerifyCommitResult", res)
	}
}

func TestVerifyCommitPolicyMismatch(t *testing.T) {
	repo := newCommittedRepo(t)
	ev := preflight(t, repo, "v1", 0)
	commitWith(t, repo, map[string]string{
		"GIT_AUTHOR_NAME":  "Intruder",
		"GIT_AUTHOR_EMAIL": "intruder@example.com",
	})

	_, err := repo.flow().Execute(context.Background(), gitflow.VerifyCommit{
		Ref:               "HEAD",
		ExpectedAuthor:    ev.Author,
		ExpectedCommitter: ev.Committer,
		ExpectedSigning:   ev.Signing,
	})
	if code := faultCode(t, err); code != model.CodeCommitPolicyMismatch {
		t.Fatalf("author mismatch code = %s, want COMMIT_POLICY_MISMATCH", code)
	}
}

func TestVerifyCommitSigningMismatchUnsignedPolicy(t *testing.T) {
	repo := newCommittedRepo(t)
	ev := preflight(t, repo, "v1", 0) // signing disabled
	enableSSHSigning(t, repo)
	commitWith(t, repo, nil) // signed by config

	_, err := repo.flow().Execute(context.Background(), gitflow.VerifyCommit{
		Ref:               "HEAD",
		ExpectedAuthor:    ev.Author,
		ExpectedCommitter: ev.Committer,
		ExpectedSigning:   ev.Signing, // Enabled=false
	})
	if code := faultCode(t, err); code != model.CodeCommitPolicyMismatch {
		t.Fatalf("signed-vs-unsigned code = %s, want COMMIT_POLICY_MISMATCH", code)
	}
}

func TestVerifyCommitSigningMismatchSignedPolicy(t *testing.T) {
	repo := newCommittedRepo(t)
	enableSSHSigning(t, repo)
	ev := preflight(t, repo, "v1", 0) // signing enabled

	commitWith(t, repo, nil, "--no-gpg-sign") // unsigned despite config

	_, err := repo.flow().Execute(context.Background(), gitflow.VerifyCommit{
		Ref:               "HEAD",
		ExpectedAuthor:    ev.Author,
		ExpectedCommitter: ev.Committer,
		ExpectedSigning:   ev.Signing, // Enabled=true
	})
	if code := faultCode(t, err); code != model.CodeCommitPolicyMismatch {
		t.Fatalf("unsigned-vs-signed code = %s, want COMMIT_POLICY_MISMATCH", code)
	}
}

// TestMatrixPreflightFailuresCarryReleaseDisposition (Task 21): the Git
// identity/signing/policy preflight failures the matrix rows
// (git_identity_not_configured, git_signing_preflight_failed,
// git_verify_policy_mismatch) assert carry the compiled release disposition:
// USER_ACTION_REQUIRED with the Dispatch Gate closed, never charging a Retry.
func TestMatrixPreflightFailuresCarryReleaseDisposition(t *testing.T) {
	for _, code := range []model.Code{
		model.CodeGitIdentityNotConfigured,
		model.CodeGitSigningPreflightFailed,
		model.CodeCommitPolicyMismatch,
		model.CodeCommitPolicyDrift,
		model.CodeCommitDuringPolicyDriftWindow,
	} {
		pol, ok := model.Policy(code)
		if !ok {
			t.Fatalf("no compiled policy for %s", code)
		}
		if pol.Retry.ChargesBudget {
			t.Fatalf("policy(%s) charges a retry: %+v", code, pol)
		}
		if !pol.CloseDispatch {
			t.Fatalf("policy(%s) keeps dispatch open: %+v", code, pol)
		}
	}
}
