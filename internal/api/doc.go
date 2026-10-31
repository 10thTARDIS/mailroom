// SPDX-License-Identifier: AGPL-3.0-only

// Package api is Mailroom's client-facing protocol layer (spec §13): true
// RFC 8620 core semantics plus an RFC 8621 subset, with Mailroom features as
// capability-URN extensions, all backed by the per-user per-type change log
// (ADR-002, ADR-003).
//
// # Surface (0.5)
//
//   - GET /jmap/session (and the /.well-known/jmap alias) — the Session
//     resource (RFC 8620 §2): capabilities, the caller's single account, and
//     the API/blob/push URLs.
//   - POST /jmap — the request endpoint (RFC 8620 §3): {using, methodCalls} in,
//     {methodResponses, sessionState} out, with result-reference (#ref)
//     resolution between calls.
//
// Methods: Email/get|query|queryChanges|changes|set, Mailbox/get|query|changes|
// set, Thread/get|changes. Email/queryChanges answers with the
// cannotCalculateChanges fallback (a valid refetch trigger, RFC 8620 §5.6);
// Mailbox/set refuses all mutations in 0.5 (creating/renaming/destroying a
// mailbox is out of scope).
//
// # State strings and /changes
//
// A per-type state string is the opaque encoding of the decimal change sequence
// (data-model §3.1). Foo/changes runs the exact §3.2 collapse in the store
// (UserTx.Changes): below the compaction floor it returns
// cannotCalculateChanges; otherwise it buckets the change_log into created/
// updated/destroyed and honors maxChanges at an object boundary.
//
// # Isolation
//
// The authenticated user id comes ONLY from the token context (auth.Middleware
// injects it; it is never a request parameter — spec §10), and every data
// access funnels through internal/store's UserTx, so RLS scopes all of it to
// that user. Requesting another user's object ids yields notFound/empty, never
// their data (the negative tests in the integration suite assert this per
// endpoint — data-model §5 test 4).
//
// # Wiring
//
// This package exposes constructors only; cmd/mailroom and internal/server do
// the mounting. New(store) builds the service; Handler() returns the mux to
// mount behind auth.Middleware; NewResolveUser(store) builds the auth.ResolveUser
// that maps an OIDC subject to a user id at login.
package api
