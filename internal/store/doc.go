// SPDX-License-Identifier: AGPL-3.0-only

// Package store is the only way Mailroom code touches the database
// (mailroom-data-model.md §5, ADR-010).
//
// It exposes exactly two doors:
//
//   - Store.UserTx — requires a user identity, sets mailroom.user_id
//     transaction-locally so RLS policies scope every statement. Fail
//     closed: no identity, no rows.
//   - Store.SystemTx — identity deliberately unset (user rows invisible);
//     cross-user work happens only through narrow SECURITY DEFINER
//     functions such as app.dequeue_due_jobs.
//
// The load-bearing invariant (CLAUDE.md): every mutation of a JMAP-visible
// object appends a change_log row in the same transaction, via the helpers
// in this package.
package store
