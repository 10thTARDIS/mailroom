// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"sort"

	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// authResultsObject renders the `authResults` property of the JMAP Email object
// (urn:mailroom:senderauth, #55 increment 1): what SPF, DKIM and DMARC actually
// said about this message, as reported by the servers that handled it.
//
// ⚠ IT REPORTS FACTS, NOT A VERDICT, and that is the whole design. What Mailroom
// already PERSISTED was a verdict — one conservative boolean, app.messages
// .auth_fail — and increment 0 measured what that costs: on a real
// corpus the boolean is false for every message while DKIM has failed
// outright on some, DMARC evaluation has errored on others, and more carry an explicit
// `none`. Every message a display should say something about is invisible in it.
// So the client is handed the per-method results and decides; a second boolean
// here would repeat the mistake one layer up.
//
// THREE STATES, AND CONFLATING ANY TWO OF THEM IS A LIE:
//
//   - **null** — the question does not apply. Mailroom wrote this message (a
//     draft, or a copy of something the user sent), so it never crossed an
//     authenticating hop and has no Authentication-Results BY CONSTRUCTION.
//     ⚠ A large share of the headerless messages on a real corpus are these.
//     Reporting them as unauthenticated would be actively wrong — it would tell
//     a user their own outbox looks forged.
//   - **present: false** — the message arrived from outside and carried no
//     authentication at all. Nobody vouched for it. That is a real and different
//     thing from the above, and it is the state a green badge most dangerously
//     papers over.
//   - **present: true** — the methods below ran; read them.
//
// A METHOD MAY HOLD SEVERAL RESULTS AT ONCE and they are all preserved. One
// passing DKIM signature alongside a failing one is ordinary for multi-signature
// mail and is not a failure; collapsing that pair to either member would invent
// a verdict the header does not contain. A method absent from the header is null
// rather than an empty array, for the same reason the whole property is null
// above: "did not run" is not "ran and said nothing".
// ⚠ THE NULL CASE IS NARROWER THAN "WE WROTE IT". It is "we wrote it AND there
// is nothing to report". A Sent copy that round-trips through IMAP can come back
// carrying an Authentication-Results our own server stamped on the way out, and
// that is real information about our outbound authentication — so it is shown.
// Suppressing on authorship alone would have discarded it to keep a rule tidy.
func authResultsObject(a ml.AuthResults, locallyAuthored bool) any {
	if locallyAuthored && !a.Present {
		return nil
	}
	out := map[string]any{"present": a.Present}
	for _, m := range ml.AuthMethods {
		out[m] = authMethodResults(a, m)
	}
	return out
}

// authNoticeObject decides the `authResults` Email property (#766):
//
//   - no authserv-id configured → null (a notice on everything is furniture);
//   - a trusted header exists → authResultsObject (today's object);
//   - otherwise, locally authored or in Sent/Drafts (self) → null: such mail
//     crosses no authenticating hop, and "unauthenticated" on a user's own
//     outbox is wrong;
//   - otherwise, Mailroom's own DKIM check at ingest passed aligned with the
//     From domain (#776) → localDKIMObject (`source: "mailroom"`);
//   - otherwise → `present: false` ("unauthenticated"), WHETHER OR NOT
//     untrusted Authentication-Results headers are present.
//
// ⚠ A LOCAL FAIL, AN UNALIGNED PASS AND ANY OTHER LOCAL RESULT CHANGE NOTHING:
// local is read only through LocalDKIMPass, and only when no trusted header
// exists (the provider's verdict always wins). local may be nil.
//
// ⚠ UNTRUSTED HEADERS DO NOT CHANGE THE NOTICE. They are attacker-writable
// (RFC 8601 §5), so a header the sender wrote must never be able to turn a
// warning off. The old-provider mail that motivated a quiet notice is handled
// by date-bounded trust instead (ADR-035).
func authNoticeObject(trusted ml.AuthResults, configured, locallyAuthored, self bool, local *store.LocalDKIM) any {
	switch ml.AuthNoticeFor(configured, trusted.Present, locallyAuthored, self, local.LocalDKIMPass()) {
	case ml.NoticeTrusted:
		return authResultsObject(trusted, locallyAuthored)
	case ml.NoticeLocalDKIM:
		return localDKIMObject(*local)
	case ml.NoticeUnauthenticated:
		return authResultsObject(trusted, false)
	default:
		return nil
	}
}

// localDKIMObject is the `authResults` of a message Mailroom itself verified
// at ingest (#776, ADR-036): only reachable for an ALIGNED local pass with no
// trusted header (ml.NoticeLocalDKIM). `source: "mailroom"` is what tells a
// client this verdict is Mailroom's and not the provider's; dkimDomain is the
// signing domain (d=). The other methods are null: Mailroom checked DKIM only.
func localDKIMObject(v store.LocalDKIM) any {
	out := map[string]any{"present": true}
	for _, m := range ml.AuthMethods {
		out[m] = nil
	}
	out["dkim"] = []string{"pass"}
	out["source"] = "mailroom"
	out["dkimDomain"] = v.Domain
	return out
}

// authMethodResults renders one method's result keywords as a sorted array, or
// null when the method did not appear at all.
//
// Sorted because the parser holds results in a map and Go randomises map order:
// without this, two Email/get calls on the SAME message could return the same
// facts in a different order, which reads to a client — and to a diffing test —
// as the message having changed.
func authMethodResults(a ml.AuthResults, method string) any {
	got := a.Results[method]
	if len(got) == 0 {
		return nil
	}
	out := make([]string, 0, len(got))
	for k := range got {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
