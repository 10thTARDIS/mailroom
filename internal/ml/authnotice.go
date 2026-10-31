// SPDX-License-Identifier: AGPL-3.0-only

package ml

// The reading pane's sender-authentication notice (#766), as a CATEGORY.
//
// ONE DEFINITION, used by the `authResults` Email property (internal/api) and
// by `mailroom-admin auth-report --authserv` / `from-field-report`, so a report
// of "how many messages would show a notice" is computed by the code that
// decides it and not by a restatement that could drift.

// AuthNotice names why the notice is, or is not, shown for one message.
type AuthNotice string

const (
	// NoticeUnconfigured: the deployment names no authserv-id, so the server
	// sends no notice at all (a notice on everything is furniture, #625).
	NoticeUnconfigured AuthNotice = "unconfigured"
	// NoticeTrusted: some header bears a trusted authserv-id; its verdicts are
	// shown (which may still end in no visible warning if they all passed).
	NoticeTrusted AuthNotice = "trusted"
	// NoticeLocalDKIM: no trusted header, but Mailroom's own DKIM check at
	// ingest passed with a d= aligned to the From domain (#776, ADR-036). The
	// reading pane says "Verified by Mailroom" instead of "unauthenticated".
	// Only an aligned PASS puts a message here; a local fail never changes the
	// category.
	NoticeLocalDKIM AuthNotice = "local_dkim"
	// NoticeUnauthenticated: inbound, non-self mail with no trusted
	// Authentication-Results header, whether or not UNTRUSTED ones are present.
	// ⚠ An untrusted header must not change the notice: it is attacker-writable,
	// and letting it suppress a warning is the hole RFC 8601 §5 describes. (A
	// "silence on untrusted-only mail" rule was built and withdrawn for that
	// reason, #766; date-bounded trust covers the old-provider case instead.)
	NoticeUnauthenticated AuthNotice = "unauthenticated"
	// NoticeSelf: no trusted header, and the message is the user's own (locally
	// authored, in Sent or Drafts, or From one of their identity addresses).
	NoticeSelf AuthNotice = "self"
)

// AuthNoticeFor classifies one message.
//
// trustedPresent is TrustedAuthResults(...).Present for the ids ml.TrustedAt
// resolved at this message's received time.
//
// localPass is whether Mailroom's own DKIM check passed, aligned (#776;
// store.LocalDKIM.LocalDKIMPass). It is consulted only when no trusted header
// exists, so a provider's verdict always wins, and AFTER self, so the user's
// own mail keeps showing no notice.
//
// ⚠ THE ORDER IS THE POLICY. Untrusted headers are not an input at all.
func AuthNoticeFor(configured, trustedPresent bool, locallyAuthored, self, localPass bool) AuthNotice {
	switch {
	case !configured:
		return NoticeUnconfigured
	case trustedPresent:
		return NoticeTrusted
	case locallyAuthored || self:
		return NoticeSelf
	case localPass:
		return NoticeLocalDKIM
	default:
		return NoticeUnauthenticated
	}
}
