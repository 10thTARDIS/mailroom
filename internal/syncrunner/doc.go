// SPDX-License-Identifier: AGPL-3.0-only

// Package syncrunner drives continuous IMAP synchronization for the operator
// (spec §16): on start it runs an immediate catch-up pass, then re-syncs every
// configured interval. Each pass lists the user's sync-enabled accounts, opens
// each account's credentials, and runs a single-account sync
// (internal/mailsync) per account.
//
// # Error isolation
//
// A failure preparing or syncing one account is recorded and logged but never
// aborts the pass — the remaining accounts still sync. Only a failure LISTING
// accounts fails a pass, and even then the continuous loop logs it and keeps
// running (a transient database blip must not kill sync).
//
// # Credential + key resolution
//
// CredentialResolver re-derives the per-user credentials key, opens the sealed
// account secret (internal/onboarding), and builds the sync.Config plus the
// mail-blob DEK the ingest path needs. Two 0.5 assumptions live here:
//
//   - The mail-blob DEK is DERIVED storage-free from the master key
//     (info="mailroom-mail-blob-v1") with a scheme-version sentinel DEKID, the
//     same pattern as credentials. The wrapped random-KEK/DEK hierarchy is 1.0
//     hardening and is deliberately not used yet.
//   - IMAP transport is assumed implicit TLS (993): app.accounts has no TLS
//     column in 0.5, so the resolver dials sync.TLSImplicit. STARTTLS/insecure
//     are test-harness only.
//
// # Testability
//
// The loop depends only on narrow seams — an AccountLister, a Syncer (satisfied
// directly by *mailsync.Engine), and a Prepare func — so catch-up-on-start,
// per-account iteration, and error isolation are unit-testable with a fake sync
// function, without a live IMAP server or a database. Progress is surfaced via
// structured slog and an optional Progress callback (the hook for metrics).
package syncrunner
