// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
)

// The domain sets behind the sender warnings (#397, spec §11).
//
// Two questions, and they are genuinely different:
//
//   - **Which domains are MINE?** A stranger claiming one of them, from
//     outside, without authenticating, is spoofing the user to themselves.
//   - **Which domains do I correspond with?** A domain that RENDERS like one of
//     those, and is not it, is imitating a relationship the user has.
//
// ⚠ "CORRESPOND WITH" MEANS "HAS WRITTEN TO ME", AND THAT IS A CONCESSION TO
// THE DATA. app.contact_addresses carries `outbound_count` as well, and its own
// schema comment argues that mail the user SENT "is the signal that actually
// means correspondence" — which is true, and which is 0 on all 343 addresses of
// the mailbox this was measured against, because that mailbox sends no mail.
// Keying on it would define zero known domains and make the whole feature a
// mechanism that cannot fire, which this project has shipped before and now
// checks for. The weaker definition is used deliberately.

// SenderDomains is the user's own domains and the domains they have received
// mail from.
type SenderDomains struct {
	// Own comes from app.identities: a fixed identity's domain, and a
	// domain_wildcard's whole domain.
	Own []string
	// Corresponded is every distinct domain in app.contact_addresses.
	//
	// ⚠ ITS SIZE IS UNBOUNDED IN PRINCIPLE and small in practice (a few hundred on the
	// corpus this was built against). It is read once per Email/get that asks
	// for the property, not once per message; if a mailbox ever makes that read
	// expensive, the fix is a materialised set rather than a per-message query.
	Corresponded []string
}

// ownDomainsSQL is THE definition of "the user's own domains": a fixed
// identity's domain and a domain_wildcard's whole domain, enabled identities
// only, for user $1. Shared by SenderDomainsFor (the selfSpoof warning) and
// ownIdentityDomains (learned placement's domain-fallback block, #714), so the
// two cannot disagree about which domains are the user's.
const ownDomainsSQL = `
		SELECT 'own' AS kind, lower(domain) AS domain
		  FROM app.identities
		 WHERE user_id = $1 AND enabled AND domain IS NOT NULL
		UNION
		SELECT 'own', lower(split_part(address::text, '@', 2))
		  FROM app.identities
		 WHERE user_id = $1 AND enabled AND address IS NOT NULL`

// ownIdentityDomains returns ownDomainsSQL's set.
func (tx *UserTx) ownIdentityDomains(ctx context.Context) (map[string]bool, error) {
	rows, err := tx.tx.Query(ctx, ownDomainsSQL, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: own domains: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var kind, domain string
		if err := rows.Scan(&kind, &domain); err != nil {
			return nil, fmt.Errorf("store: own domain row: %w", err)
		}
		if domain != "" {
			out[domain] = true
		}
	}
	return out, rows.Err()
}

// SenderDomainsFor reads both sets in one round trip.
func (tx *UserTx) SenderDomainsFor(ctx context.Context) (SenderDomains, error) {
	var out SenderDomains
	rows, err := tx.tx.Query(ctx, ownDomainsSQL+`
		UNION
		SELECT 'corresponded', lower(split_part(address::text, '@', 2))
		  FROM app.contact_addresses
		 WHERE user_id = $1`,
		tx.userID.String())
	if err != nil {
		return out, fmt.Errorf("store: sender domains: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, domain string
		if err := rows.Scan(&kind, &domain); err != nil {
			return out, fmt.Errorf("store: sender domain row: %w", err)
		}
		if domain == "" {
			continue
		}
		if kind == "own" {
			out.Own = append(out.Own, domain)
		} else {
			out.Corresponded = append(out.Corresponded, domain)
		}
	}
	return out, rows.Err()
}
