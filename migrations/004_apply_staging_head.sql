-- 004_apply_staging_head.sql — forward-only evolution: the Apply Attempt's
-- recorded staging head (Task 19, design 15.5). StagingHead is the
-- verified combined staging head the explicit delivery fast-forwards the
-- Target to; persisting it lets the delivery assert the live Apply Branch
-- ref equals the reviewed head after any crash and store round-trip — a
-- locally tampered Apply Branch ref fails closed as
-- EVIDENCE_SUBJECT_CHANGED instead of fast-forwarding the Target to an
-- unreviewed head. The column defaults to '' so existing rows stay valid.
--
-- The chain is forward-only: this file never alters 001/002/003 rows or
-- columns.

ALTER TABLE apply_attempts ADD COLUMN staging_head TEXT NOT NULL DEFAULT '';
