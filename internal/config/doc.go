// SPDX-License-Identifier: AGPL-3.0-only

// Package config loads Mailroom's runtime configuration from the
// environment (spec §16 ops, CLAUDE.md database-access rules).
//
// Sensitive values follow the Docker-secrets convention: for a variable
// FOO, if FOO_FILE is set it is read as the source of truth and FOO itself
// is ignored — so nothing sensitive needs to be passed as plaintext env in
// a container that mounts /run/secrets. internal/config never interprets
// secret contents itself; e.g. the master-key file path is carried as-is
// for internal/crypto to read later.
package config
