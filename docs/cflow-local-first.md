# CFlow's Local-First Boundary

CFlow keeps the state it manages on the user's machine. This includes the
runtime state, SQLite database, artifacts, redacted session indexes and logs,
evidence, worktrees, and locks. Local filesystem, Git, and process facts are
the basis for recovery and audit; CFlow does not depend on a CFlow-hosted
control plane.

Local-first does not mean offline. Provider CLIs such as Codex and Claude,
their existing configuration, and user-approved verification commands may
access the network. It also does not mean that CFlow provides an operating
system sandbox: the Demo does not uniformly block network access or prove
that provider and verification processes cannot access paths outside a task
worktree.

Provider-owned data is outside CFlow's local persistence guarantee. Remote
model requests, account data, and any records retained by a provider remain
governed by that provider's terms and the user's provider configuration.
CFlow's guarantee applies only to CFlow-managed data.

CFlow itself does not:

- create a cloud account or upload workflow state;
- provide remote team collaboration or remote approvals; or
- automatically push or fetch, create pull requests, or modify remote Git
  refs.

`CFLOW_HOME` must be on a local filesystem where CFlow can verify owner-only
permissions and reliable advisory locking. Network filesystems and
synchronized directories whose permission or lock semantics cannot be
verified are outside the Demo's supported boundary.
