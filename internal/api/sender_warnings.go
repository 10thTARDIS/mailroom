// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ml"
	"mailroom/internal/senderwarn"
	"mailroom/internal/store"
)

// Email.senderWarnings — the last unbuilt #55 item (#397, spec §11).
//
// # Why it sits beside authResults rather than inside it
//
// `authResults` answers "did the servers that handled this message vouch for
// it?". This answers "is the domain it came from one you know, or one that
// looks like one?". A lookalike domain **passes authentication perfectly** —
// it belongs to the attacker, who configured SPF and DKIM for it like anyone
// else. The two properties are adjacent and independent, and a client shows
// them in the same place for the same reader.
//
// ⚠ WHAT MEASUREMENT REMOVED FROM THIS FEATURE, because the shape of the issue
// invited two things that do not work:
//
//   - **No "external sender" marking.** A personal mailbox is a catch-all;
//     almost none of the messages come from the user's own domains. Nearly 100% external is not a
//     signal, it is a background colour.
//   - **No display-name impersonation.** "The name matches a contact but the
//     address does not" fires on a few percent of the corpus and is legitimate every
//     time — ESPs sending on a brand's behalf, which authenticate correctly
//     because the ESP signs as itself. The rule measures delegation.
//
// See internal/senderwarn for the reasoning and the numbers.

// senderWarningsObject renders the property, or nil when there is nothing to
// say — which is the overwhelming majority of mail, by measurement and by
// design.
//
// ⚠ nil, NOT AN EMPTY ARRAY, and it is the same restraint `senderAuth.ts`
// documents: a green "verified" badge would fire on 99.4% of mail and become
// furniture. An empty array invites a client to render "no problems found" for
// every message, which is that badge with extra steps.
func senderWarningsObject(ev store.EmailView, body *emailBody, ix *senderwarn.Index) any {
	if ix == nil {
		return nil
	}
	var authTrusted ml.AuthResults
	fromFields := 0
	var local *store.LocalDKIM
	if body != nil {
		authTrusted = body.authTrusted
		fromFields = body.fromFields
		local = body.localDKIM
	}
	ws := ix.Check(senderwarn.Sender{
		Domain:          domainOf(ev.FromAddress),
		LocallyAuthored: ev.LocallyAuthored,
		// ⚠ "AFFIRMATIVELY PASSED", not "did not fail". A message with no
		// Authentication-Results at all has passed nothing, and for mail
		// claiming the user's OWN domain that absence is the entire signal —
		// a spoofer does not helpfully add a failing header.
		// ⚠ authTrusted, NOT auth (#414). An Authentication-Results header is
		// attacker-writable unless its authserv-id is checked (RFC 8601 §5), so
		// reading the display parse here let a spoofer switch off the warning
		// about their own spoofing with one header line.
		// #776: Mailroom's own aligned DKIM pass is cryptographic proof that
		// the From domain signed the mail, so it counts too, but only when no
		// trusted header exists (a provider's verdict wins, fail included), and
		// never a local fail or an unaligned pass (LocalDKIMPass).
		AuthPassed: authAffirmativelyPassed(authTrusted) || (!authTrusted.Present && local.LocalDKIMPass()),
		// #754: the raw From field count from the bytes loadBodies parsed.
		// Two From fields raise multipleFrom, whatever the domain says.
		FromFields: fromFields,
	})
	if len(ws) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(ws))
	for _, w := range ws {
		item := map[string]any{"kind": w.Kind, "domain": w.Domain}
		if w.Resembles != "" {
			item["resembles"] = w.Resembles
		}
		if w.Unicode != "" {
			item["unicode"] = w.Unicode
		}
		out = append(out, item)
	}
	return out
}

// authAffirmativelyPassed reports DKIM or DMARC saying `pass`. It is
// ml.AuthResults.AffirmativelyPassed (one definition, shared with learned
// placement's own use of the selfSpoof check, #714); see its doc for why SPF
// alone is not enough.
func authAffirmativelyPassed(a ml.AuthResults) bool {
	return a.AffirmativelyPassed()
}

// domainOf takes the part after the last '@'. Deliberately not a full address
// parser: this runs on `app.messages.from_address`, which the ingest path has
// already parsed and normalised, so re-parsing here would be a second opinion
// about what the sender's address is.
func domainOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == '@' {
			return addr[i+1:]
		}
	}
	return ""
}

// resolveSenderIndex builds the user's known-domain index once per Email/get,
// and only when the client asked for the property.
//
// ⚠ ONCE PER REQUEST, NOT ONCE PER MESSAGE. The set is the whole correspondent
// history (a few hundred domains on the corpus this was built against); doing it per
// message would turn a reading-pane fetch into a query per Email.
func (a *API) resolveSenderIndex(ctx context.Context, userID ulid.ULID, props map[string]bool) (*senderwarn.Index, *MethodError) {
	if !props["senderWarnings"] {
		return nil, nil
	}
	var d store.SenderDomains
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		d, err = tx.SenderDomainsFor(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	return senderwarn.NewIndex(d.Own, d.Corresponded), nil
}
