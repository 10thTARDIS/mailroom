// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

// THE GMAIL SYNC HOLD (#52, ADR-047; coordinator decision 2026-10-27).
//
// Without Mailroom's Gmail sync model (All Mail once plus X-GM-LABELS, a
// daily download budget, newest-first import), syncing a Gmail account would
// walk every label folder with no byte budget — exactly the
// pattern that can trip Google's daily IMAP download limit and suspend the
// person's IMAP everywhere, their phone included. So a connected Gmail OAuth
// account is created with sync OFF, carrying a stable reason, and every door
// that could switch it on refuses while the hold stands:
//
//   - the connect callback creates (and a reconnect re-attaches) it with
//     sync_enabled = false (connect.go);
//   - Account/set syncEnabled:true is refused (internal/api/account.go);
//   - mailroom-admin enable-account is refused (cmd/mailroom-admin);
//   - Account/getHealth reports the reason as syncHold (computed, not stored);
//   - and, behind all of them, the sync resolver refuses to prepare a held
//     account at all (syncrunner.ErrSyncHeld), so an account enabled by any
//     other route is still never dialled for sync.
//
// Increment 5a BUILT the model (internal/mailsync/gmail.go: All Mail, Spam and
// Trash only, X-GM-LABELS, the daily budget, newest-first with a backfill
// floor, the label sweep) and the read-only census (mailroom-admin
// gmail-census), and left this switch FALSE on purpose: the points ADR-047
// marks UNVERIFIED (does a label change bump MODSEQ? the consumer download
// limit) are settled by a live test, and writeback is 5b.
// Flipping GmailSyncReady to true is still the ONE line that lifts the hold;
// the model is fully exercised in tests with the hold injected off
// (syncrunner.WithSyncHold). Microsoft accounts are not held (no comparable
// download cap; their CONDSTORE-less flag gap is increment 6).

// GmailSyncReady says whether Gmail accounts may sync. The model exists
// (increment 5a); this flips after increment 5b and a live test.
const GmailSyncReady = false

// THE GMAIL WRITEBACK HOLD (#52, ADR-047 §3, ADR-028). Increment 5a brought
// the Gmail READ path; increment 5b BUILT Mailroom's Gmail writeback
// (internal/writeback/gmail.go: STORE ±X-GM-LABELS, MOVE to and from Trash
// and Spam, no EXPUNGE outside them) and left this switch FALSE on purpose:
// what Gmail does with a STORE naming a label that does not exist, a MOVE
// into All Mail and an EXPUNGE in Drafts is UNVERIFIED until a
// live test. While it is false a Gmail OAuth account is INGEST-ONLY, and that
// is forced, not defaulted: the connect callback creates it with writeback
// off, Account/set refuses writebackEnabled:true for it
// (internal/api/account.go), the store suppresses every intent for it
// whatever the column says (store.EnqueueIntent; store.gmailWritebackHeldSQL),
// and the writeback runner refuses to dial it even for a row that is somehow
// pending (writeback.Runner.DrainAccount; store.GmailWritebackHeld).
//
// It is a SIBLING of GmailSyncReady, not the same switch, deliberately: the
// operator can lift sync first and run the account ingest-only (ADR-028's
// safe mode) while the read path's open questions are settled, and only then
// let Mailroom write to the person's Gmail.

// GmailWritebackReady says whether Mailroom may write to Gmail OAuth
// accounts. Flip it together with store.GmailWritebackReady (a test checks
// they agree), after a live test.
const GmailWritebackReady = false

// The writeback hold's stable reason.
const (
	WritebackHoldGmailPending        = "gmail_writeback_pending"
	WritebackHoldGmailPendingMessage = "Changes you make in Mailroom are not sent back to Gmail yet; that arrives in a later update."
)

// WritebackHold reports whether an account's writeback must stay off, and
// why. Pure, like SyncHold.
func WritebackHold(authKind, providerProfile string) (code, message string, held bool) {
	if !GmailWritebackReady && authKind == "oauth2" && providerProfile == "gmail" {
		return WritebackHoldGmailPending, WritebackHoldGmailPendingMessage, true
	}
	return "", "", false
}

// The hold's stable reason: a machine code the UI can key on and the text it
// can show.
const (
	SyncHoldGmailPending        = "gmail_sync_pending"
	SyncHoldGmailPendingMessage = "Gmail sync arrives in the next update; your account is connected and ready."
)

// ProviderForProfile maps an OAuth account's provider profile back to its
// provider, for an account whose grant is gone (disconnected): "gmail" is
// Google, "microsoft" is Microsoft, anything else is unknown ("").
func ProviderForProfile(profile string) string {
	switch profile {
	case "gmail":
		return ProviderGoogle
	case "microsoft":
		return ProviderMicrosoft
	}
	return ""
}

// SyncHold reports whether an account's sync must stay off, and why. It is a
// pure function of the account's kind and profile and the switch above, so
// there is nothing to store and nothing to migrate when the switch flips.
func SyncHold(authKind, providerProfile string) (code, message string, held bool) {
	if !GmailSyncReady && authKind == "oauth2" && providerProfile == "gmail" {
		return SyncHoldGmailPending, SyncHoldGmailPendingMessage, true
	}
	return "", "", false
}
