// SPDX-License-Identifier: AGPL-3.0-only

// Package db embeds the forward-only schema migrations so the migrate
// command and the CI drift check run the exact SQL shipped in the binary.
//
// The reference full schema lives beside them in db/schema.sql (ADR-008);
// docs/migrations.md explains how the two are kept in agreement.
package db

import "embed"

// Migrations holds the goose migrations under db/migrations.
//
//go:embed migrations/*.sql
var Migrations embed.FS
