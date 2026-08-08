-- 005_workspace_layout.sql — forward-only evolution: one long-lived
-- workspace Worktree per Workflow and the Layout facts that pin it
-- (TUI workflow design 7/8, Task 3). Each Workflow created under the
-- aggregated layout binds a Layout Version (1 = legacy integration
-- branch, 2 = aggregated workspace), its canonical Workspace path and
-- branch, the candidate and verified workspace heads, and the dirty
-- fingerprint known when the facts were written. The legacy
-- IntegrationBranch/IntegrationHead columns remain untouched for read-only
-- identification of pre-aggregation Workflows. layout_migrations records
-- explicit per-Workflow layout adoption.
--
-- The chain is forward-only: this file never alters 001/002/003/004 rows
-- or columns.

ALTER TABLE workflows ADD COLUMN layout_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE workflows ADD COLUMN workspace_path TEXT NOT NULL DEFAULT '';
ALTER TABLE workflows ADD COLUMN workspace_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE workflows ADD COLUMN candidate_workspace_head TEXT NOT NULL DEFAULT '';
ALTER TABLE workflows ADD COLUMN verified_workspace_head TEXT NOT NULL DEFAULT '';
ALTER TABLE workflows ADD COLUMN workspace_dirty_fingerprint TEXT NOT NULL DEFAULT '';

CREATE TABLE layout_migrations (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    status TEXT NOT NULL,
    manifest_path TEXT NOT NULL,
    manifest_sha256 TEXT NOT NULL,
    created_at TEXT NOT NULL,
    completed_at TEXT,
    FOREIGN KEY (workflow_id) REFERENCES workflows(id)
);