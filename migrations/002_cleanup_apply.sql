-- 002_cleanup_apply.sql — forward-only evolution: immutable Cleanup
-- Manifest bindings (PRD "Cleanup 目标清单与执行结果").
--
-- 001 already carries the PRD's cleanup_attempts/cleanup_items/apply_attempts
-- tables; this migration adds the strict, checkable binding contract on top:
-- every Cleanup Attempt's confirmed Manifest (path + SHA-256) is pinned by
-- one append-only binding row whose binding_sha256 fixes the canonical
-- binding record, and the Store maintains the index the "only pending items
-- of the same confirmed Manifest may be retried" rule queries (design 17.4).
-- The chain is forward-only: this file never alters 001 rows or columns.

CREATE TABLE cleanup_manifest_bindings (
    cleanup_attempt_id TEXT PRIMARY KEY,
    manifest_path TEXT NOT NULL,
    manifest_sha256 TEXT NOT NULL,
    binding_sha256 TEXT NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY (cleanup_attempt_id) REFERENCES cleanup_attempts(id)
);

CREATE INDEX cleanup_items_attempt_status_idx ON cleanup_items (cleanup_attempt_id, status);
