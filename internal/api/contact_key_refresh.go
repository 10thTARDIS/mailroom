// SPDX-License-Identifier: AGPL-3.0-only

package api

// ContactKey/refresh (#818, ADR-040 §15): "check for an updated key". Re-fetch a
// held correspondent key from WKD and merge any renewal.
//
//	ContactKey/refresh {accountId, id}
//	  -> {accountId, id, result, checkedAt, key, addresses: [{address, outcome}],
//	      newKeyId?, newFingerprint?}
//
// THE EXISTING DISCOVERY PATH, NOT A NEW ONE. The lookup is the KeyFetcher
// RecipientKey/lookup uses (internal/wkd through the SSRF-guarded egress
// client: https only, private and loopback addresses refused, a key-sized byte
// cap, a timeout), and what it finds goes through store.UpsertContactKeyDetail,
// which merges a same-fingerprint copy with pgpkeys.Merge (ADR-040 §1).
//
// result is one of:
//   - renewed: a copy of THIS key was served and the merge moved its end of
//     usability later (the renewal is now held);
//   - revoked: the merge brought a revocation the held copy lacked;
//   - updated: the merge changed the held copy without extending or revoking
//     it (a new user ID or subkey);
//   - unchanged: this key was served and added nothing;
//   - differentKey: a DIFFERENT usable key is served for the address. It is
//     stored (source wkd) and, when no pin exists for the address, the held key
//     is pinned first, so the new key surfaces as keyChanged and waits for
//     ContactKey/decide. It is never chosen for encryption silently.
//   - notFound: every address answered "no key" (or served nothing usable);
//   - unreachable: no lookup got an answer (DNS, TLS, timeout, or the egress
//     guard refusing the host). Never folded into unchanged.
//
// LIMITS. Per user (refreshBurst, then one per refreshRefill), per domain
// across all users (refreshDomainBurst, then one per refreshDomainRefill; the
// stranger's server is the thing protected), and per key: no refresh sooner
// than refreshMinInterval after the last (app.contact_keys.checked_at, claimed
// atomically before the network is touched). Each refusal is rateLimited and
// touches nothing.
//
// CHANGE ROWS AND EVENTS: a merge that changed the key writes its event and the
// Contact `updated` row (store); a different key stored here writes its
// first_seen event and, unlike a key found by ingest, the Contact `updated` row
// too, because the user asked and the client will show it. A refresh that
// changes nothing writes no row and no event (checked_at is bookkeeping, off
// the wire).

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
	"mailroom/internal/wkd"
)

// Refresh limits (see the file comment).
const (
	refreshBurst         = 10
	refreshRefill        = 30 * time.Second
	refreshDomainBurst   = 10
	refreshDomainRefill  = 6 * time.Second
	refreshMinInterval   = 15 * time.Minute
	maxRefreshAddresses  = 3
	refreshLookupTimeout = recipientLookupBudget
)

// Results of ContactKey/refresh.
const (
	refreshRenewed      = "renewed"
	refreshRevoked      = "revoked"
	refreshUpdated      = "updated"
	refreshUnchanged    = "unchanged"
	refreshDifferentKey = "differentKey"
	refreshNotFound     = "notFound"
	refreshUnreachable  = "unreachable"
)

const errWKDUnavailable = "wkdUnavailable"

// refreshLimits is the per-user and per-domain budget of ContactKey/refresh.
type refreshLimits struct {
	once        sync.Once
	user        *RateLimiter
	domain      *RateLimiter
	minInterval time.Duration
}

func (a *API) refreshLimit() *refreshLimits {
	a.refresh.once.Do(func() {
		if a.refresh.user == nil {
			a.refresh.user = NewRateLimiter(RateLimitConfig{PerSecond: 1 / refreshRefill.Seconds(), Burst: refreshBurst})
		}
		if a.refresh.domain == nil {
			a.refresh.domain = NewRateLimiter(RateLimitConfig{PerSecond: 1 / refreshDomainRefill.Seconds(), Burst: refreshDomainBurst})
		}
		if a.refresh.minInterval == 0 {
			a.refresh.minInterval = refreshMinInterval
		}
	})
	return &a.refresh
}

// domainBucket keys the shared per-user limiter by domain instead: the first
// 16 bytes of the domain's SHA-256, as a ULID-shaped key. A collision would
// only share a budget between two domains.
func domainBucket(domain string) ulid.ULID {
	sum := sha256.Sum256([]byte(strings.ToLower(domain)))
	var id ulid.ULID
	copy(id[:], sum[:16])
	return id
}

type contactKeyRefreshArgs struct {
	AccountID string `json:"accountId"`
	ID        string `json:"id"`
}

// refreshCandidate is one address the key is looked up at, and what came back.
type refreshCandidate struct {
	address string
	body    []byte
	err     error
	outcome string
}

func (a *API) contactKeyRefresh(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args contactKeyRefreshArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	keyID, mErr := parseKeyID(args.ID)
	if mErr != nil {
		return nil, mErr
	}
	if a.keyFetcher == nil {
		return nil, &MethodError{Type: errWKDUnavailable, Description: "this server has no WKD fetcher wired, so it cannot look keys up"}
	}
	lim := a.refreshLimit()
	now := time.Now().UTC()

	// 1. The key, its addresses, and the per-key interval (read only).
	var (
		held       store.ContactKey
		candidates []string
	)
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		k, found, err := tx.ContactKeyByID(ctx, keyID)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !found {
			return &MethodError{Type: errNotFound, Description: "no such key"}
		}
		held = k
		last, _, err := tx.ContactKeyLastChecked(ctx, keyID)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if last != nil && now.Sub(*last) < lim.minInterval {
			return tooSoon(*last, lim.minInterval, now)
		}
		contacts, err := tx.GetContacts(ctx, []ulid.ULID{k.ContactID})
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		var contactAddrs []string
		if len(contacts) == 1 {
			for _, ca := range contacts[0].Addresses {
				contactAddrs = append(contactAddrs, store.NormalizeAddress(ca.Address))
			}
		}
		candidates = refreshAddresses(k.PublicKey, contactAddrs)
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	if len(candidates) == 0 {
		return nil, &MethodError{Type: errInvalidArguments, Description: "that key names no address WKD could be asked about"}
	}

	// 2. The budgets, then the claim on the per-key slot.
	if ok, retry := lim.user.Allow(userID); !ok {
		return nil, &MethodError{Type: errRateLimited,
			Description: fmt.Sprintf("too many key checks; try again in %ds", int(retry.Seconds())+1)}
	}
	for _, addr := range candidates {
		_, domain, _ := wkd.SplitAddress(addr)
		if ok, retry := lim.domain.Allow(domainBucket(domain)); !ok {
			return nil, &MethodError{Type: errRateLimited,
				Description: fmt.Sprintf("too many key checks for %s; try again in %ds", domain, int(retry.Seconds())+1)}
		}
	}
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		last, err := tx.ClaimContactKeyCheck(ctx, keyID, now, lim.minInterval)
		switch {
		case errors.Is(err, store.ErrCheckedTooRecently) && last != nil:
			return tooSoon(*last, lim.minInterval, now)
		case errors.Is(err, store.ErrContactKeyNotFound):
			return &MethodError{Type: errNotFound, Description: "no such key"}
		case err != nil:
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}

	// 3. The network, outside any transaction.
	lookupCtx, cancel := context.WithTimeout(ctx, refreshLookupTimeout)
	results := make([]refreshCandidate, len(candidates))
	var wg sync.WaitGroup
	for i, addr := range candidates {
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			body, _, err := a.keyFetcher.Fetch(lookupCtx, addr)
			results[i] = refreshCandidate{address: addr, body: body, err: err}
		}(i, addr)
	}
	wg.Wait()
	cancel()

	// 4. What was served: copies of this key, and different usable keys.
	type newKey struct {
		address string
		pub     []byte
		fpr     string
	}
	var (
		sameCopies [][]byte
		different  *newKey
		anyFailed  bool
	)
	for i := range results {
		r := &results[i]
		switch {
		case r.err == nil:
			r.outcome = wkdUnusable
			blocks, err := pgpkeys.SplitKeys(r.body)
			if err != nil {
				continue
			}
			for _, b := range blocks {
				k, err := pgpkeys.Parse(b)
				if err != nil {
					continue
				}
				if k.Fingerprint == held.Fingerprint {
					sameCopies = append(sameCopies, b)
					r.outcome = wkdFound
					continue
				}
				if _, err := pgpkeys.EncryptionFingerprintFor(b, r.address, time.Now().UTC()); err == nil {
					if different == nil {
						different = &newKey{address: r.address, pub: b, fpr: k.Fingerprint}
					}
					if r.outcome != wkdFound {
						r.outcome = wkdFound
					}
				}
			}
		case errors.Is(r.err, wkd.ErrNoKey):
			r.outcome = wkdNone
		default:
			anyFailed = true
			r.outcome = wkdFailed
			slog.InfoContext(ctx, "ContactKey/refresh: WKD lookup failed",
				"user_id", userID.String(), "key_id", keyID.String(), "err", r.err)
		}
	}

	// 5. Store what was found, in one transaction.
	var (
		mergeKind string
		newID     *ulid.ULID
		after     store.ContactKey
		pins      map[string]string
	)
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		for _, copyBytes := range sameCopies {
			r, err := tx.UpsertContactKeyDetail(ctx, store.ContactKeyRow{
				ContactID: held.ContactID, Kind: held.Kind, Fingerprint: held.Fingerprint,
				PublicKey: copyBytes, Source: store.ContactKeyWKD,
			})
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			if r.Merged && mergeKind != store.KeyEventRevokedSeen {
				mergeKind = r.EventKind
			}
		}
		if different != nil {
			// The held key becomes the pin first when there is none, so the new
			// key is a CHANGE the user decides on, never a silent replacement.
			if err := tx.PinContactKeyIfAbsent(ctx, different.address, held.Fingerprint, held.Source); err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			r, err := tx.UpsertContactKeyDetail(ctx, store.ContactKeyRow{
				ContactID: held.ContactID, Kind: store.KeyPGP, Fingerprint: different.fpr,
				PublicKey: different.pub, Source: store.ContactKeyWKD,
			})
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			if r.Created {
				if _, err := tx.AppendChange(ctx, store.ObjectContact, held.ContactID, store.OpUpdated); err != nil {
					return &MethodError{Type: errServerFail, Description: err.Error()}
				}
			}
			if r.ID != (ulid.ULID{}) {
				id := r.ID
				newID = &id
			}
		}
		k, found, err := tx.ContactKeyByID(ctx, keyID)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !found {
			return &MethodError{Type: errNotFound, Description: "the key was removed while it was being checked"}
		}
		after = k
		byContact, err := tx.ContactKeyPinsFor(ctx, []ulid.ULID{held.ContactID})
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		pins = byContact[held.ContactID]
		return nil
	}); mErr != nil {
		return nil, mErr
	}

	result := refreshNotFound
	switch {
	case mergeKind == store.KeyEventRevokedSeen:
		result = refreshRevoked
	case mergeKind == store.KeyEventRenewed:
		result = refreshRenewed
	case mergeKind == store.KeyEventUpdated:
		result = refreshUpdated
	case different != nil:
		result = refreshDifferentKey
	case len(sameCopies) > 0:
		result = refreshUnchanged
	case anyFailed:
		// At least one address could not be reached (if another answered, it
		// served nothing usable): the key's whereabouts are unknown, which is
		// not "not found" and never "unchanged".
		result = refreshUnreachable
	}
	addrs := make([]map[string]any, 0, len(results))
	for _, r := range results {
		addrs = append(addrs, map[string]any{"address": r.address, "outcome": r.outcome})
	}
	out := map[string]any{
		"accountId": userID.String(),
		"id":        keyID.String(),
		"result":    result,
		"checkedAt": jmapDate(now),
		"key":       contactKeyObject(after, pins, time.Now().UTC()),
		"addresses": addrs,
	}
	if different != nil {
		out["newFingerprint"] = different.fpr
		if newID != nil {
			out["newKeyId"] = newID.String()
		}
	}
	return mustMarshal(out), nil
}

// tooSoon is the per-key interval refusal.
func tooSoon(last time.Time, interval time.Duration, now time.Time) *MethodError {
	wait := last.Add(interval).Sub(now)
	return &MethodError{Type: errRateLimited,
		Description: fmt.Sprintf("that key was checked at %s; try again in %ds", jmapDate(last), int(wait.Seconds())+1)}
}

// refreshAddresses is where a key is looked up: the addresses its user IDs name
// that are also its contact's, else the contact's own addresses (a key with no
// address in its user IDs is still the contact's), at most maxRefreshAddresses,
// each one a plain address WKD can be asked about.
func refreshAddresses(pub []byte, contactAddrs []string) []string {
	own := map[string]bool{}
	for _, c := range contactAddrs {
		own[c] = true
	}
	var named []string
	if k, err := pgpkeys.Parse(pub); err == nil {
		for _, ad := range k.Addresses {
			if own[ad] {
				named = append(named, ad)
			}
		}
	}
	if len(named) == 0 {
		named = contactAddrs
	}
	var out []string
	seen := map[string]bool{}
	for _, ad := range named {
		if ad == "" || seen[ad] || strings.ContainsAny(ad, " \t\r\n,<>\"") {
			continue
		}
		if _, _, err := wkd.SplitAddress(ad); err != nil {
			continue
		}
		seen[ad] = true
		out = append(out, ad)
		if len(out) == maxRefreshAddresses {
			break
		}
	}
	return out
}
