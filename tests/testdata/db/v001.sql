-- 001_initial.sql — initial normalized State Store schema.
--
-- Authoritative shape: PRD docs/cflow-prd.md "核心数据库表". The PRD SQL is
-- behavior-fixing pseudocode; this file translates it faithfully, keeping
-- every foreign key, unique constraint, and identity column, and adds only
-- the design-required adaptations noted inline:
--
--   * workflows.aggregate_version      — per-Workflow optimistic-concurrency
--                                        version for the Store compare-and-swap
--                                        (design 9.2, 8.1).
--   * approvals.seq / findings.scope|subject|finding_text|seq — the Decision
--                                        Kernel's Approval/Finding records are
--                                        persisted faithfully so hydration
--                                        round-trips the aggregate.
--   * managed_processes.exit_code, run_id nullable, pid/start_token
--     placeholders — OS-level process identity lives in the platform adapter
--     (design 13.2); the aggregate records lineage and settled state.
--   * runs.dispatch_gate                — the Run dispatch gate the Kernel
--                                        closes on Quiescing/Safety Stop.
--   * apply_attempts.git_commit_preflight_type — the full ArtifactRef of the
--                                        bound Commit Preflight.
--   * status CHECK constraints          — the closed status sets of the model
--                                        (design 7, PRD 状态机与持久化模型).
--   * effects table                     — persisted Effect Intents committed
--                                        atomically with the Decision (design
--                                        6.2, 9.2).
--
-- Migration versions, IDs, and hashes are immutable once released. The
-- first migration also creates schema_migrations; its baseline row needs no
-- backup (PRD 决策 5).

CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    migration_id TEXT NOT NULL UNIQUE,
    migration_sha256 TEXT NOT NULL,
    cflow_version TEXT NOT NULL,
    backup_manifest_path TEXT,
    backup_manifest_sha256 TEXT,
    applied_at TEXT NOT NULL
);

CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    project_key TEXT NOT NULL UNIQUE,
    canonical_path TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    git_root TEXT NOT NULL,
    git_remote TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    last_opened_at TEXT NOT NULL
);

CREATE TABLE workflows (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    stage TEXT NOT NULL CHECK (stage IN ('REQUIREMENT_DISCUSSION','PLAN_GENERATION','PLAN_CHECK','SPEC_GENERATION','WORKFLOW_GENERATION','EXECUTION','FINAL_VERIFICATION','COMPLETED')),
    runtime_status TEXT NOT NULL CHECK (runtime_status IN ('PENDING','RUNNING','PAUSED','BLOCKED','FAILED','SUCCEEDED','CANCELLED')),
    plan_status TEXT CHECK (plan_status IN ('DRAFT','CHECKING','CHECKED','APPROVED','STALE','REJECTED')),
    aggregate_version INTEGER NOT NULL DEFAULT 0,
    active_run_id TEXT,
    target_branch TEXT,
    base_commit TEXT,
    initial_worktree_dirty INTEGER NOT NULL DEFAULT 0,
    initial_dirty_fingerprint TEXT,
    integration_branch TEXT,
    cancel_requested_at TEXT,
    cancelled_at TEXT,
    cancelled_by TEXT,
    cancel_reason TEXT,
    revision INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY(project_id) REFERENCES projects(id)
);

CREATE TABLE workflow_artifact_refs (
    workflow_id TEXT NOT NULL,
    artifact_type TEXT NOT NULL,
    active_revision INTEGER NOT NULL,
    artifact_path TEXT NOT NULL,
    artifact_sha256 TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(workflow_id, artifact_type),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    spec_id TEXT NOT NULL DEFAULT '',
    replaces_task_id TEXT,
    title TEXT NOT NULL DEFAULT '',
    cached_projection_status TEXT,
    worktree_path TEXT,
    branch_name TEXT,
    task_base_commit TEXT,
    implementation_commits_json TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(workflow_id, spec_id),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(replaces_task_id) REFERENCES tasks(id)
);

CREATE TABLE findings (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    workflow_id TEXT,
    task_id TEXT,
    code TEXT NOT NULL,
    severity TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'OPEN',
    scope TEXT NOT NULL DEFAULT '',
    subject TEXT NOT NULL DEFAULT '',
    finding_text TEXT NOT NULL DEFAULT '',
    seq INTEGER NOT NULL DEFAULT 0,
    evidence_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    resolved_at TEXT,
    resolution_json TEXT,
    FOREIGN KEY(project_id) REFERENCES projects(id),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(task_id) REFERENCES tasks(id)
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    task_id TEXT,
    supersedes_session_id TEXT,
    purpose TEXT NOT NULL,
    provider TEXT NOT NULL DEFAULT '',
    model TEXT,
    provider_session_id TEXT,
    context_bundle_revision INTEGER,
    context_bundle_path TEXT,
    context_bundle_sha256 TEXT,
    status TEXT NOT NULL CHECK (status IN ('STARTING','ACTIVE','INTERRUPTED','PAUSED','COMPLETED','FAILED','CANCELLED','LOST')),
    started_at TEXT NOT NULL,
    ended_at TEXT,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(supersedes_session_id) REFERENCES sessions(id)
);

CREATE TABLE runs (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('STARTING','RUNNING','QUIESCING','STOPPING','INTERRUPTED','BLOCKED','SUCCEEDED','FAILED','CANCELLED')),
    dispatch_gate INTEGER NOT NULL DEFAULT 1,
    pid INTEGER,
    heartbeat_at TEXT,
    stop_requested_at TEXT,
    stop_reason TEXT,
    force_stop_requested_at TEXT,
    quiesce_requested_at TEXT,
    blocking_finding_id TEXT,
    quiesce_snapshot_json TEXT NOT NULL DEFAULT '[]',
    started_at TEXT NOT NULL,
    ended_at TEXT,
    error_code TEXT,
    error_message TEXT,
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(blocking_finding_id) REFERENCES findings(id)
);

CREATE TABLE leases (
    scope_type TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    workflow_id TEXT,
    run_id TEXT,
    pid INTEGER NOT NULL,
    process_start_token TEXT NOT NULL,
    acquired_at TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(scope_type, scope_key),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);

CREATE TABLE nodes (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    task_id TEXT,
    supersedes_node_id TEXT,
    node_type TEXT NOT NULL,
    definition_sha256 TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('PENDING','READY','RUNNING','SUCCEEDED','FAILED','CANCELLED','SKIPPED')),
    current_attempt_number INTEGER NOT NULL DEFAULT 0,
    retry_budget_consumed INTEGER NOT NULL DEFAULT 0,
    max_retry_budget INTEGER NOT NULL DEFAULT 0,
    last_error_code TEXT,
    last_error_message TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(task_id) REFERENCES tasks(id),
    FOREIGN KEY(supersedes_node_id) REFERENCES nodes(id)
);

CREATE TABLE node_attempts (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL,
    attempt_number INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('READY','RUNNING','SUCCEEDED','FAILED','INTERRUPTED','CANCELLED')),
    session_id TEXT,
    start_head_commit TEXT,
    start_dirty_fingerprint TEXT,
    end_head_commit TEXT,
    end_dirty_fingerprint TEXT,
    end_head_audit_ref TEXT,
    started_at TEXT NOT NULL,
    ended_at TEXT,
    evidence_manifest_json TEXT NOT NULL DEFAULT '{}',
    retry_budget_charged INTEGER NOT NULL DEFAULT 0,
    interruption_reason TEXT,
    error_code TEXT,
    error_message TEXT,
    UNIQUE(node_id, attempt_number),
    FOREIGN KEY(node_id) REFERENCES nodes(id),
    FOREIGN KEY(session_id) REFERENCES sessions(id)
);

CREATE TABLE managed_processes (
    id TEXT PRIMARY KEY,
    run_id TEXT,
    session_id TEXT,
    process_type TEXT NOT NULL,
    pid INTEGER NOT NULL DEFAULT 0,
    process_start_token TEXT NOT NULL DEFAULT '',
    process_group_id TEXT,
    status TEXT NOT NULL CHECK (status IN ('RUNNING','EXITED','STOPPED')),
    exit_code INTEGER NOT NULL DEFAULT 0,
    started_at TEXT NOT NULL,
    ended_at TEXT,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    FOREIGN KEY(run_id) REFERENCES runs(id),
    FOREIGN KEY(session_id) REFERENCES sessions(id)
);

CREATE TABLE cleanup_attempts (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('DRY_RUN','AWAITING_CONFIRMATION','RUNNING','SUCCEEDED','FAILED','BLOCKED','CANCELLED')),
    plan_path TEXT NOT NULL DEFAULT '',
    plan_sha256 TEXT NOT NULL DEFAULT '',
    requested_by TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    ended_at TEXT,
    error_code TEXT,
    error_message TEXT,
    UNIQUE(workflow_id, id),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);

CREATE TABLE cleanup_items (
    id TEXT PRIMARY KEY,
    cleanup_attempt_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    target_type TEXT NOT NULL,
    canonical_path TEXT NOT NULL,
    expected_branch TEXT,
    expected_head_commit TEXT,
    expected_fingerprint TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('PENDING','REQUESTED','COMPLETED','FAILED')),
    started_at TEXT,
    ended_at TEXT,
    error_code TEXT,
    error_message TEXT,
    UNIQUE(cleanup_attempt_id, ordinal),
    FOREIGN KEY(cleanup_attempt_id) REFERENCES cleanup_attempts(id)
);

CREATE TABLE apply_attempts (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    attempt_number INTEGER NOT NULL DEFAULT 1,
    supersedes_apply_attempt_id TEXT,
    status TEXT NOT NULL CHECK (status IN ('STAGING','AWAITING_CONFIRMATION','RUNNING','SUCCEEDED','FAILED','BLOCKED','CANCELLED')),
    target_head_at_start TEXT NOT NULL DEFAULT '',
    integration_head TEXT NOT NULL DEFAULT '',
    apply_branch TEXT NOT NULL DEFAULT '',
    apply_worktree_path TEXT NOT NULL DEFAULT '',
    verification_catalog_revision INTEGER NOT NULL DEFAULT 0,
    verification_catalog_sha256 TEXT NOT NULL DEFAULT '',
    git_commit_preflight_type TEXT NOT NULL DEFAULT 'commit-preflight',
    git_commit_preflight_revision INTEGER,
    git_commit_preflight_sha256 TEXT,
    git_commit_policy_fingerprint TEXT,
    staged_apply_commit TEXT,
    applied_target_commit TEXT,
    verification_manifest_json TEXT NOT NULL DEFAULT '{}',
    started_at TEXT NOT NULL,
    ended_at TEXT,
    error_code TEXT,
    error_message TEXT,
    UNIQUE(workflow_id, attempt_number),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(supersedes_apply_attempt_id) REFERENCES apply_attempts(id)
);

CREATE TABLE approvals (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    gate_type TEXT NOT NULL,
    decision TEXT NOT NULL,
    actor TEXT NOT NULL DEFAULT '',
    seq INTEGER NOT NULL DEFAULT 0,
    plan_revision INTEGER,
    plan_sha256 TEXT,
    specs_revision INTEGER,
    specs_sha256 TEXT,
    verification_catalog_revision INTEGER,
    verification_catalog_sha256 TEXT,
    dynamic_workflow_revision INTEGER,
    dynamic_workflow_sha256 TEXT,
    routing_policy_sha256 TEXT,
    budget_policy_sha256 TEXT,
    git_commit_preflight_revision INTEGER,
    git_commit_preflight_sha256 TEXT,
    git_commit_policy_fingerprint TEXT,
    apply_attempt_id TEXT,
    target_head_commit TEXT,
    integration_head_commit TEXT,
    decision_context_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(apply_attempt_id) REFERENCES apply_attempts(id)
);

CREATE TABLE branch_quarantines (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    task_id TEXT,
    apply_attempt_id TEXT,
    branch_kind TEXT NOT NULL DEFAULT 'branch',
    branch_name TEXT NOT NULL,
    worktree_path TEXT NOT NULL DEFAULT '',
    head_commit TEXT NOT NULL DEFAULT '',
    audit_ref TEXT NOT NULL UNIQUE,
    reason_code TEXT NOT NULL,
    evidence_manifest_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    UNIQUE(workflow_id, branch_name),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(task_id) REFERENCES tasks(id),
    FOREIGN KEY(apply_attempt_id) REFERENCES apply_attempts(id)
);

CREATE TABLE git_commit_preflights (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    repository_context TEXT NOT NULL,
    git_version TEXT NOT NULL,
    commit_policy_fingerprint TEXT NOT NULL,
    identity_json TEXT NOT NULL,
    signing_policy_json TEXT NOT NULL,
    probe_status TEXT NOT NULL,
    artifact_path TEXT NOT NULL,
    artifact_sha256 TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(workflow_id, revision),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);

CREATE TABLE git_commit_evidence (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    task_id TEXT,
    node_attempt_id TEXT,
    apply_attempt_id TEXT,
    commit_hash TEXT NOT NULL,
    commit_kind TEXT NOT NULL,
    preflight_id TEXT NOT NULL,
    author_identity_json TEXT NOT NULL,
    committer_identity_json TEXT NOT NULL,
    signature_status TEXT NOT NULL,
    signer_identity TEXT,
    evidence_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    UNIQUE(workflow_id, commit_hash),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(task_id) REFERENCES tasks(id),
    FOREIGN KEY(node_attempt_id) REFERENCES node_attempts(id),
    FOREIGN KEY(apply_attempt_id) REFERENCES apply_attempts(id),
    FOREIGN KEY(preflight_id) REFERENCES git_commit_preflights(id)
);

-- The authoritative, strictly increasing Event log (design 9.2). sequence
-- is assigned by the database transaction; payloads contain only redacted,
-- bounded data and immutable references.
CREATE TABLE events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL UNIQUE,
    project_id TEXT NOT NULL,
    workflow_id TEXT,
    run_id TEXT,
    task_id TEXT,
    apply_attempt_id TEXT,
    event_type TEXT NOT NULL,
    payload_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    FOREIGN KEY(project_id) REFERENCES projects(id),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);

-- Effect Intents committed atomically with their Decision (design 6.2): an
-- external Effect is not executed until its Intent and expected facts
-- commit; the Result is an immutable evidence input to another Decision.
CREATE TABLE effects (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING','RESULTED')),
    decision_version INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);

CREATE INDEX effects_pending_idx ON effects (workflow_id, status);
