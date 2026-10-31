// SPDX-License-Identifier: AGPL-3.0-only

package api

// RecipientKey/lookup (#47, ADR-038): "can I encrypt to these addresses, and with
// what?" — the question a compose window asks before it offers an Encrypt switch.
// Gated on capPGP (urn:mailroom:pgp).
//
// A METHOD, NOT A PROPERTY. Whether an address has a usable key depends on the
// RECIPIENT and the CLOCK (a key can expire between two calls), not on any object
// the user owns, and answering it may involve a network lookup. Neither fits an
// Identity or Email property, so it is its own method, in the style of
// EmailSubmission/getSendQuota: a computed read.
//
// WHAT IT RETURNS: per address, whether a usable key is held, its fingerprint and
// how it reached us — and NEVER the key itself. A client needs the fact, and the
// fingerprint to show; the bytes stay server-side, exactly Contact.keys' rule.
//
// WHAT IT MAY DO TO STATE. With `wkd` true (the default) an address with no usable
// stored key is looked up over WKD (internal/wkd, through the SSRF-guarded egress
// client, bounded in time and in number). A key found there is stored in
// app.contact_keys with source 'wkd' — a new key writes no change_log row
// (store.UpsertContactKey); a renewal of a key already held is MERGED into it and
// writes the Contact `updated` row (#795, ADR-040) — and is then what Submit
// uses. Submit itself NEVER touches the network: the key a lookup
// found is the key the send encrypts to.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/compose"
	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
	"mailroom/internal/wkd"
)

// KeyFetcher looks up a stranger's key over WKD. *wkd.Fetcher satisfies it.
type KeyFetcher interface {
	Fetch(ctx context.Context, addr string) (key []byte, sourceURL string, err error)
}

// WithKeyFetcher wires WKD lookups for RecipientKey/lookup. Nil (the default)
// means only already-stored keys are consulted, and every `wkd` outcome is
// "unavailable".
func WithKeyFetcher(f KeyFetcher) Option { return func(a *API) { a.keyFetcher = f } }

const (
	// maxRecipientKeyAddresses bounds one call. A compose window has a handful of
	// recipients; each unresolved one can cost a network round trip to a host the
	// sender of the original mail chose.
	maxRecipientKeyAddresses = 50
	// maxRecipientWKDLookups bounds how many of them may go to the network in one
	// call. The rest report wkd "skipped" and can be asked again.
	maxRecipientWKDLookups = 10
	// recipientLookupBudget bounds the whole network phase.
	recipientLookupBudget = 15 * time.Second
	// recipientLookupParallel is how many lookups run at once.
	recipientLookupParallel = 4
)

// What happened on the WKD side, per address. A closed set, so a client can switch
// on it, and so an empty "wkd" can never be confused with "nothing found".
const (
	wkdNotAsked    = "notAsked"    // the caller passed wkd:false
	wkdUnavailable = "unavailable" // this server has no fetcher wired
	wkdSkipped     = "skipped"     // a stored key was usable, or the per-call limit was reached
	wkdFound       = "found"       // a usable key was fetched and stored
	wkdNone        = "none"        // 404 on both WKD paths: the ordinary "not published"
	wkdUnusable    = "unusable"    // a key was served but cannot be encrypted to (expired, revoked, no encryption subkey, unreadable)
	wkdFailed      = "failed"      // the fetch itself failed (DNS, TLS, timeout, refused by the egress guard)
)

// Why an address is not usable (the `reason` of a RecipientKey/lookup entry,
// null when usable). A closed set: expired and revoked are compose's
// UnusableExpired / UnusableRevoked.
const (
	recipientReasonNoKey      = "noKey"
	recipientReasonKeyChanged = "keyChanged"
)

type recipientKeyLookupArgs struct {
	AccountID string   `json:"accountId"`
	Addresses []string `json:"addresses"`
	WKD       *bool    `json:"wkd"`
}

func (a *API) recipientKeyLookup(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args recipientKeyLookupArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if len(args.Addresses) == 0 {
		return nil, &MethodError{Type: errInvalidArguments, Description: "addresses must list at least one address"}
	}
	if len(args.Addresses) > maxRecipientKeyAddresses {
		return nil, &MethodError{Type: errRequestTooLarge,
			Description: "addresses lists more than the server will look up in one call"}
	}
	askWKD := args.WKD == nil || *args.WKD

	// Normalised, de-duplicated, in request order. An address that is not an
	// address is refused: it would otherwise be turned into a WKD URL.
	addrs := make([]string, 0, len(args.Addresses))
	seen := map[string]bool{}
	for _, raw := range args.Addresses {
		n := store.NormalizeAddress(raw)
		if n == "" || strings.ContainsAny(n, " \t\r\n,<>\"") {
			return nil, &MethodError{Type: errInvalidArguments, Description: "addresses must be plain email addresses"}
		}
		if _, _, err := wkd.SplitAddress(n); err != nil {
			return nil, &MethodError{Type: errInvalidArguments, Description: "addresses must be plain email addresses"}
		}
		if !seen[n] {
			seen[n] = true
			addrs = append(addrs, n)
		}
	}

	type result struct {
		address     string
		usable      bool
		fingerprint string
		source      string
		wkd         string
		// keyChanged: a fingerprint is pinned for this address and the key now
		// resolved is a different one. The address is NOT usable until the user
		// confirms (ContactKey/decide, ADR-039).
		keyChanged bool
		pinned     string
		// expiresAt: the chosen key's end of usability (nil = never).
		// unusable/expiredAt: why there is no usable key, when it is a lapsed
		// one (#795): "expired" with the date, or "revoked".
		expiresAt *time.Time
		unusable  string
		expiredAt *time.Time
	}
	results := make(map[string]*result, len(addrs))
	for _, ad := range addrs {
		results[ad] = &result{address: ad, wkd: wkdNotAsked}
	}

	// Phase 1: stored keys, one transaction, no network.
	now := time.Now().UTC()
	var missing []string
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		for _, ad := range addrs {
			res, err := compose.ResolveRecipient(ctx, tx, ad, now)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			if res.Found {
				r := results[ad]
				r.fingerprint, r.source, r.wkd = res.Key.Fingerprint, res.Key.Source, wkdSkipped
				r.usable = !res.KeyChanged
				r.keyChanged, r.pinned = res.KeyChanged, res.PinnedFingerprint
				r.expiresAt = res.ExpiresAt
				continue
			}
			results[ad].unusable, results[ad].expiredAt = res.Unusable, res.ExpiredAt
			missing = append(missing, ad)
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}

	// Phase 2: the network, outside any transaction (a held connection per slow
	// stranger's server would be the cost of doing it inside one).
	type fetched struct {
		address string
		key     []byte
		outcome string
	}
	var found []fetched
	if len(missing) > 0 {
		switch {
		case !askWKD:
			// results already say notAsked
		case a.keyFetcher == nil:
			for _, ad := range missing {
				results[ad].wkd = wkdUnavailable
			}
		default:
			lookupCtx, cancel := context.WithTimeout(ctx, recipientLookupBudget)
			var (
				mu  sync.Mutex
				wg  sync.WaitGroup
				sem = make(chan struct{}, recipientLookupParallel)
			)
			for i, ad := range missing {
				if i >= maxRecipientWKDLookups {
					results[ad].wkd = wkdSkipped
					continue
				}
				wg.Add(1)
				sem <- struct{}{}
				go func(ad string) {
					defer wg.Done()
					defer func() { <-sem }()
					out := fetched{address: ad}
					raw, _, err := a.keyFetcher.Fetch(lookupCtx, ad)
					switch {
					case err == nil:
						// Parse is the size / malformed / private-material gate;
						// EncryptionFingerprint is the usability rule.
						if _, perr := pgpkeys.EncryptionFingerprintFor(raw, ad, time.Now().UTC()); perr != nil {
							out.outcome = wkdUnusable
						} else {
							out.key, out.outcome = raw, wkdFound
						}
					case errors.Is(err, wkd.ErrNoKey):
						out.outcome = wkdNone
					default:
						out.outcome = wkdFailed
					}
					mu.Lock()
					found = append(found, out)
					mu.Unlock()
				}(ad)
			}
			wg.Wait()
			cancel()
		}
	}

	// Phase 3: store what was found, one transaction, so Submit can use it.
	for _, f := range found {
		results[f.address].wkd = f.outcome
	}
	var toStore []fetched
	for _, f := range found {
		if f.outcome == wkdFound {
			toStore = append(toStore, f)
		}
	}
	if len(toStore) > 0 {
		if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
			for _, f := range toStore {
				fpr, fp := storeFetchedKey(ctx, tx, f.address, f.key)
				if fp != nil {
					return fp
				}
				if fpr == "" {
					// Could not be filed (no contact could be made): not usable for
					// Submit, so not reported usable.
					results[f.address].wkd = wkdUnusable
					continue
				}
				// Report what Submit WILL do: resolve again, through the same rules
				// (source rank, address, pin), rather than assuming the key just
				// stored is the one chosen.
				res, err := compose.ResolveRecipient(ctx, tx, f.address, time.Now().UTC())
				if err != nil {
					return &MethodError{Type: errServerFail, Description: err.Error()}
				}
				r := results[f.address]
				if !res.Found {
					r.wkd = wkdUnusable
					continue
				}
				r.fingerprint, r.source = res.Key.Fingerprint, res.Key.Source
				r.usable = !res.KeyChanged
				r.keyChanged, r.pinned = res.KeyChanged, res.PinnedFingerprint
				r.expiresAt, r.unusable, r.expiredAt = res.ExpiresAt, "", nil
			}
			return nil
		}); mErr != nil {
			return nil, mErr
		}
	}

	list := make([]map[string]any, 0, len(addrs))
	for _, ad := range addrs {
		r := results[ad]
		var fpr, src any
		if r.usable {
			fpr, src = r.fingerprint, r.source
		}
		var pinned any
		if r.keyChanged {
			// Shown even though the address is not usable: a changed key is the
			// one case where the client must be able to say which fingerprints.
			fpr, src, pinned = r.fingerprint, r.source, r.pinned
		}
		// #795: the dates a compose window needs to say "Alice's key expires on
		// 2 Nov" (expiresAt, expiringSoon) or "Alice's key expired on 18 Oct"
		// (reason "expired", expiredAt) instead of a generic "no usable key".
		var expiresAt, reason, expiredAt any
		expiringSoon := false
		if r.usable || r.keyChanged {
			expiresAt = jmapDateOrNil(r.expiresAt)
			expiringSoon = r.usable && r.expiresAt != nil && r.expiresAt.Sub(now) < pgpkeys.ExpiryWarningWindow
		}
		switch {
		case r.keyChanged:
			reason = recipientReasonKeyChanged
		case r.usable:
		case r.unusable != "":
			reason = r.unusable
			expiredAt = jmapDateOrNil(r.expiredAt)
		default:
			reason = recipientReasonNoKey
		}
		list = append(list, map[string]any{
			"address":           r.address,
			"usable":            r.usable,
			"fingerprint":       fpr,
			"source":            src,
			"wkd":               r.wkd,
			"keyChanged":        r.keyChanged,
			"pinnedFingerprint": pinned,
			"expiresAt":         expiresAt,
			"expiringSoon":      expiringSoon,
			"reason":            reason,
			"expiredAt":         expiredAt,
		})
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"list":      list,
	}), nil
}

// storeFetchedKey files a WKD key under the contact that owns address, creating
// the contact when there is none (a recipient typed into compose is not a contact
// yet). It returns the fingerprint stored, or "" when no contact could be made.
func storeFetchedKey(ctx context.Context, tx *store.UserTx, address string, key []byte) (string, *MethodError) {
	k, err := pgpkeys.Parse(key)
	if err != nil {
		return "", nil
	}
	cid, ok, err := tx.ContactIDForAddress(ctx, address)
	if err != nil {
		return "", &MethodError{Type: errServerFail, Description: err.Error()}
	}
	if !ok {
		// The same call ingest makes for the recipients of a message. A
		// ContactSourceMessage with no From is not a sent message, so this records a
		// plain sighting and no outbound count.
		if err := tx.UpsertContactsFromMessage(ctx, store.ContactSourceMessage{
			Recipients: []store.ContactParticipant{{Address: address}},
		}); err != nil {
			return "", &MethodError{Type: errServerFail, Description: err.Error()}
		}
		cid, ok, err = tx.ContactIDForAddress(ctx, address)
		if err != nil {
			return "", &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !ok {
			return "", nil
		}
	}
	if _, err := tx.UpsertContactKey(ctx, store.ContactKeyRow{
		ContactID:   cid,
		Kind:        store.KeyPGP,
		Fingerprint: k.Fingerprint,
		PublicKey:   key,
		Source:      store.ContactKeyWKD,
	}); err != nil {
		return "", &MethodError{Type: errServerFail, Description: err.Error()}
	}
	return k.Fingerprint, nil
}
