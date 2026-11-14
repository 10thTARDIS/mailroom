// SPDX-License-Identifier: AGPL-3.0-only

package store

// Which switch is holding an account's writeback off (ADR-028 amendment A5).
const (
	// WritebackHeldByAccount: the account's own writeback_enabled column is false
	// (ingest-only mode, ADR-028 §1). The user can change this one.
	WritebackHeldByAccount = "account"
	// WritebackHeldByInstance: MAILROOM_WRITEBACK_DISABLED is set (ADR-028 §2). It
	// is a deployment setting no API call can lift.
	WritebackHeldByInstance = "instance"
	// WritebackHeldByProvider: the provider hold, a Gmail OAuth account whose
	// write path is not enabled yet (ADR-047 §3, GmailWritebackHeld).
	WritebackHeldByProvider = "provider"
)

// EffectiveWriteback answers "will Mailroom write to this account's mailbox", and
// when it will not, which switch is the reason.
//
// ⚠ THE COLUMN IS NOT THE ANSWER. app.accounts.writeback_enabled is the
// per-account half of three conditions ANDed together, and a client that reads
// it alone reports an account as writing back while the instance flag or the
// provider hold has it parked. That was Account/get's behaviour (#554 item 6):
// it said writebackEnabled:true for an account that was in fact shadowed.
//
// When more than one switch applies the reason is the first of account,
// instance, provider, because that is the order of who can change it: the user
// can lift the first, the operator the second, and only a release the third.
// effective is true exactly when the reason is empty.
//
// It is pure, like oauthmail.WritebackHold, so the API's Account/get, its
// Account/getHealth and any report can share the one evaluation rather than each
// re-deriving the AND (three review rounds of ADR-028 were spent on predicates
// that disagreed about the same fact).
//
// ⚠ IT IS NOT WHAT THE ENFORCEMENT CALLS. The enqueue (EnqueueIntent's SQL) and
// the drain (Runner.DrainAccount) evaluate the same three conditions inside
// their own transactions, from the account row, and nothing ties them to this
// function: it is the REPORTING answer for the API. If a fourth switch is ever
// added, it has to be added in all three places.
func EffectiveWriteback(a Account, instanceDisabled bool) (effective bool, heldBy string) {
	switch {
	case !a.WritebackEnabled:
		return false, WritebackHeldByAccount
	case instanceDisabled:
		return false, WritebackHeldByInstance
	case GmailWritebackHeld(a.AuthKind, a.ProviderProfile):
		return false, WritebackHeldByProvider
	}
	return true, ""
}
