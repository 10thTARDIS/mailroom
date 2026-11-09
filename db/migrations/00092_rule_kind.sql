-- SPDX-License-Identifier: AGPL-3.0-only
--
-- A rule can optionally apply only to messages of one kind (the values the
-- header-only classifier in internal/kind returns: personal, transactional,
-- promotional, notification, list, automated, unknown).
--
-- EXPAND-ONLY (ADR-008): one nullable column with no default, a catalogue-only
-- change. NULL means "any kind", so every existing rule behaves exactly as it
-- did. The set of valid values is checked in Go (rules.Validate) rather than by
-- a CHECK constraint, so a new kind never needs a migration.

-- +goose Up
ALTER TABLE app.rules ADD COLUMN kind text;

-- +goose Down
ALTER TABLE app.rules DROP COLUMN IF EXISTS kind;
