// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"strings"

	"mailroom/internal/kind"
	"mailroom/internal/mime"
	"mailroom/internal/store"
)

// FunctionSignalsFromHeader extracts the #156 function signals — the rest of
// internal/kind's Facts that lives only in the message BYTES, migration 00016
// having already covered List-Id/List-Unsubscribe/the recipient count — from a
// parsed message's headers.
//
// EXPORTED SO THERE IS EXACTLY ONE IMPLEMENTATION. projectionFrom (below)
// calls this at live ingest; `mailroom-admin backfill-function-signals` calls
// it too, over headers re-parsed out of a stored blob. That mirrors
// DeriveAddressedIdentity, which the alias backfill calls for the identical
// reason (its own comment: "deliberately the same function, so this can never
// drift from what the live path would store") — a header-name lookup
// duplicated across two call sites is exactly how a backfill quietly starts
// disagreeing with what ingest would have written for the same message.
//
// Every field is the RAW header value (or, for DKIM, the raw list of d=
// domains) — never a verdict. See migration 00027 for why, and for the
// NULL-semantics decision this function's callers both honor: an absent
// header and a not-yet-extracted column read the same way, so this returns ""
// (or a nil slice) for "no such header", and the store layer collapses that to
// SQL NULL on write.
func FunctionSignalsFromHeader(h mime.Header) store.FunctionSignals {
	feedbackID := strings.TrimSpace(h.Get("Feedback-ID"))
	if feedbackID == "" {
		// Gmail's own header is Feedback-ID; some ESPs emit X-Feedback-ID
		// instead. The SAME fallback cmd/mailroom-admin's `classify` and
		// `kind-sweep` reports already use, so every reader of this signal
		// agrees on which senders carry it.
		feedbackID = strings.TrimSpace(h.Get("X-Feedback-ID"))
	}
	return store.FunctionSignals{
		ListUnsubscribePost: strings.TrimSpace(h.Get("List-Unsubscribe-Post")),
		AutoSubmitted:       strings.TrimSpace(h.Get("Auto-Submitted")),
		Precedence:          strings.TrimSpace(h.Get("Precedence")),
		FeedbackID:          feedbackID,
		// DKIM-Signature repeats once per signing domain (a multi-tenant ESP
		// signs with its own key AND the brand's aligned one), so every value
		// goes to kind.DKIMDomains — the ONE extraction kind.go's own doc
		// comment insists on, so this projection and the live classifier can
		// never disagree about which domains signed a message.
		DKIMDomains: kind.DKIMDomains(h.Values("DKIM-Signature")),
		ReturnPath:  strings.TrimSpace(h.Get("Return-Path")),
	}
}
