// SPDX-License-Identifier: AGPL-3.0-only

package ical

import (
	"strings"

	"mailroom/internal/mime"
	"mailroom/internal/ml"
)

// IS THIS CALENDAR MESSAGE REALLY FROM WHO IT SPEAKS FOR? (#725 security review)
//
// A CANCEL or a higher-SEQUENCE REQUEST changes what the user is shown about
// OTHER messages: the genuine invitation's card goes "cancelled" or "out of
// date". A UID is not a secret (it is in every copy of the invitation, and
// every attendee has one), and ORGANIZER is a string the sender typed. So
// matching on (UID, organiser) alone let anybody who had seen an invitation
// cancel it on the card. Verified is the gate a message must pass before it
// may affect another message's card.
//
// It reuses the project's sender-authentication signal — the TOPMOST
// Authentication-Results header bearing an authserv-id the operator trusts
// (MAILROOM_AUTHSERV_IDS), the same parse learned placement acts on (#714) —
// and binds it to the person the calendar speaks for:
//
//   - REQUEST / CANCEL: the ORGANIZER;
//   - REPLY: every ATTENDEE it reports (in practice, one).
//
// That person must be the message's ONE From address, case-insensitively, and
// the trusted header must carry a DMARC pass aligned to that From's domain.
//
// ⚠ EXACTLY ONE From FIELD (mime.Header.FieldCount, the raw count — empty and
// unparsable fields included). With two, which author a receiving MTA's DMARC
// verdict was about is unknowable from here, and From() picks among them.
//
// ⚠ THE Sender HEADER IS NOT HONOURED, deliberately, though RFC 6047 §3 lets
// an organiser's delegate (an assistant) send on their behalf with the
// organiser in From or the delegate in Sender. DMARC authenticates the From
// DOMAIN, never the Sender mailbox, so accepting "Sender = ORGANIZER" would
// let anyone at the From's domain — or any mailbox there at all — forge a
// cancellation for any colleague's meeting. A delegated message therefore
// shows its own card marked unverified and changes no other; that is the
// accepted cost (security review of #725).
//
// ⚠ FAIL CLOSED WITH NO TRUSTED AUTHSERV-ID. Learned placement falls back to
// "did not clearly fail" when the operator names none; that fallback is a pass
// for any spoof that simply carries no Authentication-Results at all, so it is
// NOT used here. An instance with MAILROOM_AUTHSERV_IDS unset treats every
// update and cancellation as unverified: each still shows its own card, marked
// as such, and changes no other.
func Verified(inv *Invite, parsed *mime.Message, trustedAuthservIDs []string) bool {
	if inv == nil || parsed == nil || len(trustedAuthservIDs) == 0 {
		return false
	}
	var who []string
	switch inv.Method {
	case MethodRequest, MethodCancel:
		if inv.Organizer == nil {
			return false
		}
		who = []string{inv.Organizer.Email}
	case MethodReply:
		for _, a := range inv.Attendees {
			who = append(who, a.Email)
		}
	}
	if len(who) == 0 {
		return false
	}
	if parsed.Header.FieldCount("From") != 1 {
		return false
	}
	from := parsed.Header.From()
	if len(from) != 1 {
		return false
	}
	fromDomain := domainOf(from[0].Address)
	if fromDomain == "" {
		return false
	}
	auth := ml.TopmostTrustedAuthResults(parsed.Header.Values("Authentication-Results"), trustedAuthservIDs)
	// STRICT alignment (#846): "Verified" asserts this exact organizer
	// address; relaxing it is its own decision, not learned placement's.
	if !auth.DMARCPassFor(fromDomain, ml.DMARCStrict) {
		return false
	}
	for _, w := range who {
		if !strings.EqualFold(w, from[0].Address) {
			return false
		}
	}
	return true
}

func domainOf(addr string) string {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 || i == len(addr)-1 {
		return ""
	}
	return strings.ToLower(addr[i+1:])
}
