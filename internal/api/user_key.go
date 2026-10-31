// SPDX-License-Identifier: AGPL-3.0-only

package api

// UserKey/get (spec §9, #48; #448/#374 increment 1; expiry and WKD facts #795/
// #796): the login's OWN PGP keys, public facts only. Gated on the capPGP
// capability (urn:mailroom:pgp, capability.go/service.go).
//
// THE WRITE SIDE is user_key_manage.go (#796): UserKey/generate, /import, /set,
// /extend, /revoke and /export. Every operation that needs the private half
// goes to the crypto process (ADR-024) through KeyCustody; the rest (bind,
// publish, retire) are store writes. Each appends the Key (and, where an
// Identity's projection moves, Identity) change_log rows the store functions
// document.
//
// NO UserKey/changes. Keys are a handful per user, and `state` below is a hash
// of the rows (and of the clock-derived flags) recomputed on every call, good
// for "did anything about my keys change since I last asked" cache-busting. It
// is NOT a change_log sequence number. The Key change_log rows exist for the
// invariant and for any future /changes; nothing reads them as a cursor today.
//
// READ-TIME PROJECTIONS OF THE CLOCK (#795): expiresAt, expired, usable,
// expiringSoon and canSign are computed from the key's packets at the moment of
// the call (pgpkeys.Assess). Nothing stored changes when a key's date passes, so
// they move without a change_log row, exactly as Identity.signingKeyId does
// (ADR-040 amendment).
//
// NEVER SERIALISED: PrivateKeyCT, DekID (there is no DEK for pgp rows — see
// InsertUserKey), EnvelopeVer, and the public key bytes themselves. A client
// needs the FACTS (kind, fingerprint, whether it is published, whether it can
// still sign, when it stops working), not the key material — exactly
// Contact.keys' own rule from the other side of a correspondence.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
	"mailroom/internal/wkd"
)

// userIDsJSON renders a key's self-signed user IDs (#812) for UserKey/get and
// ContactKey/get: [{name, email, revoked, primary}], primary first. The strings
// are already cleaned by pgpkeys.UserIDs (control and format characters
// removed, length capped); they are still untrusted text for the client. An
// unreadable key lists none.
func userIDsJSON(pub []byte, now time.Time) []map[string]any {
	out := []map[string]any{}
	uids, err := pgpkeys.UserIDs(pub, now)
	if err != nil {
		return out
	}
	for _, u := range uids {
		out = append(out, map[string]any{
			"name": u.Name, "email": u.Email, "revoked": u.Revoked, "primary": u.Primary,
		})
	}
	return out
}

// boundAddressListed says whether the key validly names the address of the
// identity it is bound to (#812): a user ID that is not revoked, whose
// self-signature has not expired, with that email — pgpkeys.NamesAddress, the
// same gate a correspondent's client applies before encrypting to it. nil when
// the key is unbound or bound to a wildcard (there is no one address).
func boundAddressListed(pub []byte, ident *store.Identity, now time.Time) any {
	if ident == nil || ident.Kind != store.IdentityFixed || ident.Address == "" {
		return nil
	}
	return pgpkeys.NamesAddress(pub, ident.Address, now)
}

// keyFacts is what a UserKey's own packets (and columns) say at one moment.
type keyFacts struct {
	expiresAt    *time.Time
	expired      bool
	usable       bool
	expiringSoon bool
	canSign      bool
}

// ownKeyFacts reads one own key at now. usable and canSign use the SAME reading
// compose uses (pgpkeys.OwnKeyFitness) plus the column half, so a key this
// reports usable is one an outgoing Autocrypt header will carry and one this
// reports canSign is one compose will sign with. An unreadable key keeps
// OwnKeyFitness's old treatment and has no date.
func ownKeyFacts(k store.UserKey, now time.Time) keyFacts {
	var f keyFacts
	live := !k.DecryptOnly && k.RevokedAt == nil
	advertise, sign := pgpkeys.OwnKeyFitness(k.PublicKey, now)
	f.usable = live && advertise
	f.canSign = live && sign && len(k.PrivateKeyCT) > 0
	if v, err := pgpkeys.Assess(k.PublicKey, now); err == nil {
		if until, never := v.UsableUntil(); !never {
			u := until.UTC()
			f.expiresAt = &u
		}
		f.expired = v.Expired(now)
		f.expiringSoon = f.usable && v.ExpiringSoon(now)
	}
	return f
}

// wkdLocation is where a correspondent's client looks a key up (the DIRECT
// form, the one this instance answers; see pgp-publish), and whether this
// instance serves that address at all: it answers only for MAILROOM_WKD_DOMAIN.
func wkdLocation(ident store.Identity, wkdDomain string) (url string, servable bool) {
	if ident.Kind != store.IdentityFixed || ident.Address == "" {
		return "", false
	}
	local, domain, err := wkd.SplitAddress(ident.Address)
	if err != nil {
		return "", false
	}
	return "https://" + domain + wkd.DirectPath(local),
		wkdDomain != "" && strings.EqualFold(domain, wkdDomain)
}

// userKeyObject renders a store.UserKey as a JMAP UserKey object, honoring a
// requested property subset (id is always present). Built field-by-field, like
// contactObject, so a struct field added to store.UserKey later (a private
// half, a wrapping secret) has no path onto the wire by accident.
func (a *API) userKeyObject(k store.UserKey, idents map[ulid.ULID]store.Identity, now time.Time, props map[string]bool) map[string]any {
	var identity, wkdURL, listed any
	wkdServable := false
	if k.IdentityID != nil {
		identity = k.IdentityID.String()
		if ident, ok := idents[*k.IdentityID]; ok {
			if u, s := wkdLocation(ident, a.wkdDomain); u != "" {
				wkdURL, wkdServable = u, s
			}
			listed = boundAddressListed(k.PublicKey, &ident, now)
		}
	}
	var origin any
	if k.Origin != store.KeyOriginUnrecorded {
		origin = string(k.Origin)
	}
	f := ownKeyFacts(k, now)
	var replacedBy any
	if k.ReplacedBy != nil {
		replacedBy = k.ReplacedBy.String()
	}
	full := map[string]any{
		"id":           k.ID.String(),
		"identityId":   identity,
		"kind":         string(k.Kind),
		"fingerprint":  k.Fingerprint,
		"wkdPublished": k.WKDPublished,
		"decryptOnly":  k.DecryptOnly,
		"createdAt":    jmapDate(k.CreatedAt),
		"revokedAt":    jmapDateOrNil(k.RevokedAt),
		// #795/#796 (read-time, see the file comment).
		"expiresAt":    jmapDateOrNil(f.expiresAt),
		"expired":      f.expired,
		"usable":       f.usable,
		"expiringSoon": f.expiringSoon,
		"canSign":      f.canSign,
		"origin":       origin,
		"wkdUrl":       wkdURL,
		"wkdServable":  wkdServable,
		// #815: when the key stopped being used (null: still in use, or NOT
		// RECORDED for one retired before v1.93), and what replaced it.
		"retiredAt":  jmapDateOrNil(k.RetiredAt),
		"replacedBy": replacedBy,
		// #812: the key's own user IDs, and whether one names the bound address.
		"userIds":            userIDsJSON(k.PublicKey, now),
		"boundAddressListed": listed,
	}
	if props == nil {
		return full
	}
	out := map[string]any{"id": k.ID.String()}
	for p, v := range full {
		if props[p] {
			out[p] = v
		}
	}
	return out
}

// ---- UserKey/get -----------------------------------------------------------

type userKeyGetArgs struct {
	AccountID  string    `json:"accountId"`
	IDs        *[]string `json:"ids"`
	Properties []string  `json:"properties"`
}

// userKeyGetResult is getResult plus the instance's WKD facts (#796): whether
// this server publishes keys at all (MAILROOM_WKD_DOMAIN) and for which domain.
type userKeyGetResult struct {
	getResult
	WKDConfigured bool `json:"wkdConfigured"`
	WKDDomain     any  `json:"wkdDomain"`
}

func (a *API) userKeyGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args userKeyGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	var props map[string]bool
	if args.Properties != nil {
		props = make(map[string]bool, len(args.Properties))
		for _, p := range args.Properties {
			props[p] = true
		}
	}

	var (
		all       []store.UserKey
		idents    map[ulid.ULID]store.Identity
		byID      = make(map[ulid.ULID]store.UserKey)
		requested []requestedID
		notFound  = []string{}
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		// ALWAYS the whole (small) set: RLS already scopes it to the caller, and
		// a user's own key count is a handful, never a list worth paginating —
		// this ALSO doubles as the source `state` hashes below, and as the map
		// a by-id request resolves against, so a key belonging to another user
		// (or an id that never existed) is simply absent, exactly like every
		// other cross-user /get in this package.
		all, err = tx.ListUserKeys(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if idents, err = identityMap(ctx, tx); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		for _, k := range all {
			byID[k.ID] = k
		}
		if args.IDs == nil {
			// ids:null means "all my keys" (RFC 8620 §5.1).
			for _, k := range all {
				requested = append(requested, requestedID{raw: k.ID.String(), id: k.ID, ok: true})
			}
		} else {
			_, notFound, requested = parseIDs(*args.IDs)
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	now := time.Now().UTC()
	out := make([]map[string]any, 0, len(requested))
	for _, req := range requested {
		if !req.ok {
			notFound = append(notFound, req.raw)
			continue
		}
		k, ok := byID[req.id]
		if !ok {
			notFound = append(notFound, req.raw)
			continue
		}
		out = append(out, a.userKeyObject(k, idents, now, props))
	}
	var domain any
	if a.wkdDomain != "" {
		domain = a.wkdDomain
	}
	return mustMarshal(userKeyGetResult{
		getResult: getResult{
			AccountID: userID.String(),
			State:     userKeyState(all, now),
			List:      out,
			NotFound:  notFound,
		},
		WKDConfigured: a.wkdDomain != "",
		WKDDomain:     domain,
	}), nil
}

// identityMap reads the caller's identities by id: the address a bound key's
// WKD location is computed from.
func identityMap(ctx context.Context, tx *store.UserTx) (map[ulid.ULID]store.Identity, error) {
	list, err := tx.ListIdentities(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[ulid.ULID]store.Identity, len(list))
	for _, i := range list {
		m[i.ID] = i
	}
	return m, nil
}

// userKeyState hashes the rows ListUserKeys returned into a short,
// deterministic string. See the file doc comment for why this is not a
// change_log sequence number: there is none worth a cursor. Every field that can
// change AFTER a key is created is folded in — binding, revocation, WKD
// publication, decrypt-only retirement, the public key bytes (an extend
// re-signs them) — and so are the clock-derived flags, so a key crossing its
// warning window or its expiry also moves this value.
func userKeyState(keys []store.UserKey, now time.Time) string {
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k.ID.String()))
		h.Write([]byte(k.Fingerprint))
		if k.IdentityID != nil {
			h.Write([]byte(k.IdentityID.String()))
		}
		h.Write([]byte{0})
		if k.RevokedAt != nil {
			h.Write([]byte(k.RevokedAt.UTC().Format("2006-01-02T15:04:05.999999999Z")))
		}
		h.Write([]byte{0})
		if k.ReplacedBy != nil {
			h.Write([]byte(k.ReplacedBy.String()))
		}
		f := ownKeyFacts(k, now)
		for _, b := range []bool{k.WKDPublished, k.DecryptOnly, f.expired, f.usable, f.expiringSoon, f.canSign} {
			if b {
				h.Write([]byte{1})
			} else {
				h.Write([]byte{2})
			}
		}
		sum := sha256.Sum256(k.PublicKey)
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
