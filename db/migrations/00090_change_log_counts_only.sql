-- SPDX-License-Identifier: AGPL-3.0-only
--
-- change_log.counts_only (ADR-052, #115): a Mailbox `updated` row that says ONLY
-- "this mailbox's counts may have moved" (a message arrived, was moved, read or
-- deleted), as opposed to a rename, role or scope change. Mailbox/changes uses it
-- to answer RFC 8621 2.2's `updatedProperties` with just the count properties
-- when every row for a mailbox in the window is such a row, and null otherwise.
--
-- EXPAND-ONLY (ADR-008): one column with a constant default, which PostgreSQL
-- stores in the catalogue (no table rewrite, no lock beyond the catalogue
-- update). Every existing row reads false, which yields `updatedProperties:
-- null`: the RFC's answer for "the server cannot say". Nothing reads the column
-- but the Changes scan, and compaction deletes a seq prefix whatever it holds.

-- +goose Up
ALTER TABLE app.change_log ADD COLUMN counts_only boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE app.change_log DROP COLUMN IF EXISTS counts_only;
