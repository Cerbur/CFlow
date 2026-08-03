-- 003_integration_head.sql — forward-only evolution: the aggregate's
-- IntegrationHead (Task 13, design 15.5). The workflows row records the
-- current HEAD of the CFlow-owned Integration Branch, advanced only by
-- verified serial --no-ff merges; the merge allocation decisions and the
-- Recovery Engine reconcile against the recorded value. The column
-- defaults to '' so existing rows stay valid; only the Application's
-- serialized merge protocol ever moves it.
--
-- The chain is forward-only: this file never alters 001/002 rows or
-- columns.

ALTER TABLE workflows ADD COLUMN integration_head TEXT NOT NULL DEFAULT '';
