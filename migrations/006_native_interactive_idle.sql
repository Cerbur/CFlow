-- 006_native_interactive_idle.sql — forward-only evolution: the native
-- interactive requirement discussion Session status INTERACTIVE_IDLE (TUI
-- workflow design §9, Task 12). The model added the status in Task 12, but
-- the sessions status CHECK constraint predates it; without this migration
-- the Store cannot persist the Bridge-return state that makes a Session
-- exactly resumable by the same Provider Session.
--
-- The sessions table is rebuilt (SQLite cannot alter a CHECK constraint in
-- place) with the identical column shape plus INTERACTIVE_IDLE in the
-- status CHECK. Foreign-key enforcement is deferred to the transaction
-- boundary so the rebuild is atomic; the migration chain is forward-only —
-- this file never alters 001-005 rows or columns.

PRAGMA defer_foreign_keys=ON;

CREATE TABLE sessions_new (
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
    status TEXT NOT NULL CHECK (status IN ('STARTING','ACTIVE','INTERRUPTED','PAUSED','INTERACTIVE_IDLE','COMPLETED','FAILED','CANCELLED','LOST')),
    started_at TEXT NOT NULL,
    ended_at TEXT,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(supersedes_session_id) REFERENCES sessions(id)
);

INSERT INTO sessions_new SELECT * FROM sessions;

DROP TABLE sessions;

ALTER TABLE sessions_new RENAME TO sessions;
