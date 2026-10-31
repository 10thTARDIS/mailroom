// SPDX-License-Identifier: AGPL-3.0-only

// Package onboarding implements the operator account-add flow (spec §16): it
// seals an account's IMAP/SMTP secret, records the account in the store, and
// validates the credentials by dialing IMAP.
//
// # Credential encryption at rest (spec §9, 0.5 storage-free scheme)
//
// The account secret — the IMAP/SMTP username and password(s) — is serialized
// to a small JSON blob and sealed with an AEAD envelope under a per-user key
// DERIVED from the master key: key = crypto.DeriveKey(master, salt=userID,
// info="mailroom-credentials-v1"). The sealed envelope is stored in
// app.accounts.credentials_ct, bound to the user via associated data (the raw
// user id). Because the key is derived, nothing needs to be persisted to
// recover it: credentials_dek records only a scheme-version sentinel
// (CredentialsSchemeV1), not a wrapped key. This is envelope-encrypted at rest
// under the master key, satisfying §9 for 0.5. The wrapped random-KEK/DEK
// hierarchy (crypto.WrapKEK/WrapDEK) is deliberately NOT used here; that is
// 1.0 hardening. The runner re-derives the same key and opens the envelope at
// sync time (see internal/syncrunner).
//
// # Validation
//
// AddAccount stores the account first, then validates by dialing IMAP and
// listing mailboxes. What a failure does to sync_enabled depends on WHO failed
// (mailroom#435). If the server rejected the account — bad credentials, or a
// configuration the dialer refuses — sync_enabled is set false: retrying cannot
// help, and retrying a wrong password can lock the account at the provider. Any
// other failure (connection, DNS, TLS, timeout, a protocol surprise) leaves
// sync ENABLED, so the sync loop's per-account backoff retries it; a transient
// blip at first boot costs one pass rather than the account. Either way the row
// is kept, the returned error carries the underlying cause, and
// Result.SyncEnabled says what the account was left as.
//
// # Testability
//
// AddAccount depends on narrow seams — an AccountStore (satisfied by
// StoreAdapter over *store.Store, running each call inside a Store.UserTx per
// CLAUDE.md) and a Dialer (default sync.Dial) — so the seal/derive and
// spec-validation paths are unit-testable with fakes; the live IMAP validation
// is exercised only under the integration build tag.
package onboarding
