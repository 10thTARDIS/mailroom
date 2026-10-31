// SPDX-License-Identifier: AGPL-3.0-only

// Package outofoffice is the out-of-office (vacation) reply (#50). It is
// written to the provider as a ManageSieve script, so the provider sends the
// replies.
//
// The provider runs ONE active Sieve script, and the user may already have
// one. So the rules are strict; they are design decisions
// (2026-10-18):
//
//   - Mailroom installs, activates and deletes exactly one script of its own,
//     "mailroom-vacation" (ScriptName). It never writes, activates, deletes or
//     reads any other script.
//   - Turning the reply on is REFUSED while any other script is active, with a
//     sentence naming that script (Install). A textual merge with somebody
//     else's script was rejected, and this provider has no `include`.
//   - Turning it off restores exactly what was there before. Install accepts
//     only the state where nothing was active, so turning off is SETACTIVE ""
//     followed by DELETESCRIPT (Remove). If the user has since activated
//     another script, it is left active and only Mailroom's inactive script is
//     deleted.
//   - Drift is recorded and shown, never repaired by force: a timer never
//     activates a script (Reconcile).
//
// The window is enforced twice. The script carries a currentdate guard, and
// jobs fire at the start (to look at the server) and at the end (to remove the
// script). The provider sends the replies, so they are invisible to the
// outbound ledger; see docs/adr/ADR-034.
package outofoffice
