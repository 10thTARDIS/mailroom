// SPDX-License-Identifier: AGPL-3.0-only

package api

// Own-key management over JMAP (#796, ADR-040; the server half of Settings →
// Keys). Gated on capPGP (urn:mailroom:pgp).
//
//	UserKey/generate  {accountId, identityId, expiresDays? | expiresAt?, bind?}
//	UserKey/import    {accountId, privateKey, passphrase?}
//	UserKey/set       {accountId, update: {id: {identityId | wkdPublished | decryptOnly}}}
//	UserKey/extend    {accountId, id, days | expiresAt}
//	UserKey/revoke    {accountId, id, reason, confirm}
//	UserKey/export    {accountId, id, passphrase}
//
// ⚠ THE BOUNDARY (ADR-024). Generate, import, extend, revoke and export need the
// private half, so they go to the crypto process through KeyCustody and the
// application never holds a private key it could use: import carries one IN
// (the user's own, on its way to custody) and export carries one OUT, wrapped
// under a passphrase the user chose (pgpwire.ExportWrapped). Bind, publish and
// retire touch no key material and are store writes, as in mailroom-admin.
//
// ⚠ PASSPHRASES AND PRIVATE KEYS ARE NEVER LOGGED, ECHOED OR STORED. They live
// in the request arguments and in the one crypto-process call. A refusal's
// description is either a fixed sentence or the crypto process's own message,
// which is written to quote nothing it was given and is still scrubbed here
// (scrubCustodyMessage) so a regression there cannot reach the client.
//
// EXPORT IS INLINE, NOT A BLOB. The wrapped key comes back in the method
// response (`armoredKey`) and the client turns it into a download itself. No
// blob is written: the blob store is for mail, it is persistent and indexed by
// content hash, and a "short-lived single-use" blob would be a second copy of
// the key on disk with a deletion to get right. A JMAP response is already
// exactly once.
//
// RATE LIMITS. Generate, import and export spend a per-user token from their
// own bucket (keyOpLimiter, the same token bucket the request limiter uses):
// each is a crypto-process call that does real work (key generation, an S2K
// passphrase derivation), and export hands out the key. Extend is cheap and
// revoke is exempt on purpose: a user whose key was compromised must never be
// told "try again later".
//
// CHANGE ROWS. Each store write appends exactly the rows it documents, and a
// write that changes nothing appends none (BindKeyToIdentity, UnbindUserKey,
// PublishKey, UnpublishKey, RetireUserKey). The crypto process's own writes
// (StoreVault: InsertUserKey, ReplaceUserKeyMaterial, RevokeUserKey) append
// theirs. Export writes nothing.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
	"mailroom/internal/wkd"
)

// KeyCustody is the crypto process's key verbs, in this package's terms
// (ADR-024). cmd/mailroom adapts *pgpwire.Client to it; this package never
// imports the wire. A refusal by the crypto process arrives as *CustodyError.
type KeyCustody interface {
	GenerateKey(ctx context.Context, userID ulid.ULID, name, email string, exp KeyExpiry) (CustodyKey, error)
	// ImportKey hands an ASCII-armored PRIVATE key (and its passphrase, if
	// protected) to custody. alreadyStored: that fingerprint was already held.
	ImportKey(ctx context.Context, userID ulid.ULID, armoredPrivate, passphrase string) (key CustodyKey, alreadyStored bool, err error)
	ExtendKey(ctx context.Context, userID, keyID ulid.ULID, exp KeyExpiry) (CustodyKey, error)
	RevokeKey(ctx context.Context, userID, keyID ulid.ULID, reason, confirm string) (CustodyKey, error)
	// ExportKey returns the key armored with its secret packets encrypted under
	// passphrase.
	ExportKey(ctx context.Context, userID, keyID ulid.ULID, passphrase string) (key CustodyKey, armoredWrapped string, err error)
}

// KeyExpiry is how long a generated or extended key stays valid (#813): Days
// from now (generate: from creation; 0 = never), or exactly At. At most one is
// set; neither means the default (generate only). The crypto process computes
// the packet lifetime from the key's own creation time, so At is the packets'
// expiry to the second.
type KeyExpiry struct {
	Days *int
	At   *time.Time
}

// CustodyKey is what custody says about a key: public facts only.
type CustodyKey struct {
	KeyID       string
	Fingerprint string
}

// CustodyError is the crypto process's own refusal. Code is the wire's error
// code (pgpwire.ErrorCode's values, below); Message is its text, written by the
// crypto process to quote nothing it was handed.
type CustodyError struct {
	Code    string
	Message string
}

func (e *CustodyError) Error() string { return "crypto process: " + e.Code + ": " + e.Message }

// The custody error codes this package acts on (pgpwire.Code*'s values; a test
// in cmd/mailroom pins them).
const (
	CustodyInvalidRequest = "invalid_request"
	CustodyNotFound       = "not_found"
	CustodyDenied         = "denied"
	CustodyRateLimited    = "rate_limited"
)

// WithKeyCustody wires own-key management (#796). Nil (the default) means the
// six methods answer keyManagementUnavailable, and the capability says so.
func WithKeyCustody(c KeyCustody) Option { return func(a *API) { a.custody = c } }

// WithWKDDomain tells UserKey/get and UserKey/set which mail domain this
// instance publishes keys for over WKD (MAILROOM_WKD_DOMAIN). Empty means WKD
// is off: publishing is refused with wkdNotConfigured rather than writing a
// flag nothing will serve.
func WithWKDDomain(domain string) Option {
	return func(a *API) { a.wkdDomain = strings.ToLower(strings.TrimSpace(domain)) }
}

// Key-operation rate limit: a burst of keyOpBurst, then one per
// keyOpRefill, per user, shared by generate, import and export.
const (
	keyOpBurst  = 5
	keyOpRefill = time.Minute
)

func newKeyOpLimiter() *RateLimiter {
	return NewRateLimiter(RateLimitConfig{PerSecond: 1 / keyOpRefill.Seconds(), Burst: keyOpBurst})
}

// MinExportPassphrase equals pgpwire.MinExportPassphrase (a test in
// cmd/mailroom pins it). Checked here too so the refusal is a clean
// invalidArguments before any crypto-process call.
const MinExportPassphrase = 12

// Method error types of the own-key methods (each distinct, so a client can
// switch on them).
const (
	errKeyManagementUnavailable = "keyManagementUnavailable"
	errRateLimited              = "rateLimited"
	errPublicKeyRefused         = "publicKeyRefused"
	errNotFound                 = "notFound"
	errWildcardIdentity         = "wildcardIdentity"
)

// SetError types of UserKey/set.
const (
	setKeyIsDefault      = "keyIsDefault"
	setKeyIsPublished    = "keyIsPublished"
	setKeyRetired        = "keyRetired"
	setWKDNotConfigured  = "wkdNotConfigured"
	setNotPublishable    = "notPublishable"
	setOneChangeAtATime  = "oneChangeAtATime"
	setRetireIsPermanent = "retireIsPermanent"
)

func (a *API) needCustody() *MethodError {
	if a.custody == nil {
		return &MethodError{Type: errKeyManagementUnavailable,
			Description: "this server has no crypto process wired, so it cannot manage keys"}
	}
	return nil
}

func (a *API) spendKeyOp(userID ulid.ULID) *MethodError {
	if ok, retry := a.keyOpLimiter.Allow(userID); !ok {
		return &MethodError{Type: errRateLimited,
			Description: fmt.Sprintf("too many key operations; try again in %ds", int(retry.Seconds())+1)}
	}
	return nil
}

// privateMarker matches anything that looks like the start of armored private
// key material.
var privateMarker = regexp.MustCompile(`(?i)PRIVATE KEY`)

// scrubCustodyMessage returns msg unless it could carry something it must not:
// a secret the caller supplied (passphrase, the key text) or private key
// armor. The crypto process is already tested never to quote its input; this
// is the second line, at the edge where a regression would reach a browser.
func scrubCustodyMessage(msg string, secrets ...string) string {
	if privateMarker.MatchString(msg) {
		return "the crypto process refused that request"
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(msg, s) {
			return "the crypto process refused that request"
		}
	}
	return msg
}

// custodyMethodError maps a custody failure to a method error. invalidType is
// what an invalid_request becomes for this method (invalidKey for import,
// invalidArguments elsewhere). Nothing from a transport failure is relayed.
func custodyMethodError(ctx context.Context, op string, userID ulid.ULID, err error, invalidType string, secrets ...string) *MethodError {
	var ce *CustodyError
	if !errors.As(err, &ce) {
		slog.ErrorContext(ctx, "crypto process call failed", "op", op, "user_id", userID.String())
		return &MethodError{Type: errServerFail, Description: "the crypto process could not be reached"}
	}
	switch ce.Code {
	case CustodyInvalidRequest:
		return &MethodError{Type: invalidType, Description: scrubCustodyMessage(ce.Message, secrets...)}
	case CustodyNotFound:
		return &MethodError{Type: errNotFound, Description: "no such key"}
	case CustodyDenied:
		// The key id came from this user's request and RLS-scoped reads; a
		// denial means the crypto process saw another user's key. Say only
		// "not found" to the client and shout in the log.
		slog.ErrorContext(ctx, "CONFUSED DEPUTY: the crypto process refused a key as not the caller's",
			"op", op, "user_id", userID.String())
		return &MethodError{Type: errNotFound, Description: "no such key"}
	case CustodyRateLimited:
		return &MethodError{Type: errRateLimited, Description: "too many key operations; try again shortly"}
	default:
		slog.ErrorContext(ctx, "crypto process refused a key operation", "op", op, "user_id", userID.String(), "code", ce.Code)
		return &MethodError{Type: errServerFail, Description: "the crypto process could not complete that"}
	}
}

// keyObjectAfter re-reads one key and renders it, for the method responses.
func (a *API) keyObjectAfter(ctx context.Context, userID, keyID ulid.ULID) (map[string]any, *MethodError) {
	var obj map[string]any
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		k, ok, err := tx.GetUserKey(ctx, keyID)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !ok {
			return &MethodError{Type: errNotFound, Description: "no such key"}
		}
		idents, err := identityMap(ctx, tx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		obj = a.userKeyObject(k, idents, time.Now().UTC(), nil)
		return nil
	})
	return obj, mErr
}

func parseKeyID(raw string) (ulid.ULID, *MethodError) {
	id, err := ulid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ulid.ULID{}, &MethodError{Type: errInvalidArguments, Description: "id must be a key id"}
	}
	return id, nil
}

// ---- UserKey/generate ------------------------------------------------------

type userKeyGenerateArgs struct {
	AccountID  string `json:"accountId"`
	IdentityID string `json:"identityId"`
	// ExpiresDays: days until the key (primary and encryption subkey) expires.
	// Absent or null means the two-year default; 0 means never.
	ExpiresDays *int `json:"expiresDays"`
	// ExpiresAt: the alternative (#813), an exact expiry (parseExpiresAt).
	ExpiresAt *string `json:"expiresAt"`
	// Bind: also make the new key the identity's signing key, in a second
	// step after the key exists.
	Bind bool `json:"bind"`
}

// userKeyGenerate makes a key for one of the user's FIXED identities. The name
// and address on the key come from the identity, never from free text: a key is
// published at, and signs for, an address the user has verified.
func (a *API) userKeyGenerate(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args userKeyGenerateArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if mErr := a.needCustody(); mErr != nil {
		return nil, mErr
	}
	identityID, err := ulid.Parse(strings.TrimSpace(args.IdentityID))
	if err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "identityId must be an identity id"}
	}
	exp, mErr := generateExpiry(args.ExpiresDays, args.ExpiresAt, time.Now().UTC())
	if mErr != nil {
		return nil, mErr
	}

	var ident store.Identity
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		list, err := tx.GetIdentities(ctx, []ulid.ULID{identityID})
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if len(list) == 0 {
			return &MethodError{Type: errNotFound, Description: "no such identity"}
		}
		ident = list[0]
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	if ident.Kind != store.IdentityFixed || ident.Address == "" {
		return nil, &MethodError{Type: errWildcardIdentity,
			Description: "a key names one address, and a domain wildcard has none; generate it for a fixed address"}
	}
	if mErr := a.spendKeyOp(userID); mErr != nil {
		return nil, mErr
	}

	ck, err := a.custody.GenerateKey(ctx, userID, ident.DisplayName, ident.Address, exp)
	if err != nil {
		return nil, custodyMethodError(ctx, "generate", userID, err, errInvalidArguments)
	}
	keyID, perr := ulid.Parse(ck.KeyID)
	if perr != nil {
		return nil, &MethodError{Type: errServerFail, Description: "the crypto process returned an unreadable key id"}
	}

	var bound any
	if args.Bind {
		// A second transaction, after custody committed the key: the key exists
		// either way, and a bind that fails is reported, not undone.
		if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
			ok, err := tx.BindKeyToIdentity(ctx, keyID, identityID)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			if !ok {
				return &MethodError{Type: errNotFound, Description: "the new key or the identity disappeared before binding"}
			}
			return nil
		}); mErr != nil {
			return nil, mErr
		}
		bound = identityID.String()
	}
	obj, mErr := a.keyObjectAfter(ctx, userID, keyID)
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{
		"accountId":   userID.String(),
		"id":          keyID.String(),
		"fingerprint": ck.Fingerprint,
		"boundTo":     bound,
		"key":         obj,
	}), nil
}

// MaxKeyLifetimeDays equals pgpwire.MaxKeyLifetimeDays (pinned by a test in
// cmd/mailroom), the cap mailroom-admin's --expires-days applies.
const MaxKeyLifetimeDays = 10 * 365

// MinKeyLifetime equals pgpwire.MinKeyLifetime (pinned likewise): the
// shortest exact expiry, one day from now (#813).
const MinKeyLifetime = 24 * time.Hour

// generateExpiry validates generate's two expiry arguments: at most one of
// them, expiresDays in 0..MaxKeyLifetimeDays, expiresAt per parseExpiresAt.
func generateExpiry(days *int, at *string, now time.Time) (KeyExpiry, *MethodError) {
	switch {
	case days != nil && at != nil:
		return KeyExpiry{}, &MethodError{Type: errInvalidArguments,
			Description: "give expiresDays or expiresAt, not both"}
	case days != nil && (*days < 0 || *days > MaxKeyLifetimeDays):
		return KeyExpiry{}, &MethodError{Type: errInvalidArguments,
			Description: fmt.Sprintf("expiresDays must be between 0 (never) and %d", MaxKeyLifetimeDays)}
	case at != nil:
		t, mErr := parseExpiresAt(*at, now)
		if mErr != nil {
			return KeyExpiry{}, mErr
		}
		return KeyExpiry{At: &t}, nil
	}
	return KeyExpiry{Days: days}, nil
}

// parseExpiresAt reads an exact expiry (#813, ADR-040 §12):
//
//   - a date "YYYY-MM-DD" means the END OF THAT DAY IN UTC, 23:59:59Z. The
//     client maps the date the user picked to this form; the server never
//     guesses a timezone;
//   - a UTCDate or RFC 3339 date-time with an offset is that instant, in UTC,
//     truncated to the second (OpenPGP's resolution).
//
// It must be at least MinKeyLifetime and at most MaxKeyLifetimeDays from now.
func parseExpiresAt(raw string, now time.Time) (time.Time, *MethodError) {
	raw = strings.TrimSpace(raw)
	var t time.Time
	if d, err := time.Parse("2006-01-02", raw); err == nil {
		t = d.Add(24*time.Hour - time.Second)
	} else if dt, err := time.Parse(time.RFC3339, raw); err == nil {
		t = dt.UTC().Truncate(time.Second)
	} else {
		return time.Time{}, &MethodError{Type: errInvalidArguments,
			Description: "expiresAt must be a date (YYYY-MM-DD) or a UTCDate"}
	}
	if d := t.Sub(now); d < MinKeyLifetime || d > time.Duration(MaxKeyLifetimeDays)*24*time.Hour {
		return time.Time{}, &MethodError{Type: errInvalidArguments,
			Description: fmt.Sprintf("expiresAt must be at least one day and at most %d days from now", MaxKeyLifetimeDays)}
	}
	return t, nil
}

// ---- UserKey/import --------------------------------------------------------

type userKeyImportArgs struct {
	AccountID string `json:"accountId"`
	// PrivateKey is an ASCII-armored PRIVATE KEY BLOCK, or the base64 of a
	// binary secret key (JSON cannot carry raw bytes).
	PrivateKey string `json:"privateKey"`
	// Passphrase unlocks a protected key (a pgp-export-wrapped file, a
	// Thunderbird or GnuPG export). Never logged, echoed or stored.
	Passphrase string `json:"passphrase"`
}

// maxImportPrivateKey bounds the privateKey argument (armored text, so twice
// the decoded key cap ContactKey/import uses).
const maxImportPrivateKey = 2 * pgpkeys.MaxKeyBytes

func (a *API) userKeyImport(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args userKeyImportArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		// json's message can quote a value; never relay it for this method.
		return nil, &MethodError{Type: errInvalidArguments, Description: "the arguments are not valid JSON for UserKey/import"}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if mErr := a.needCustody(); mErr != nil {
		return nil, mErr
	}
	if len(args.PrivateKey) > maxImportPrivateKey {
		return nil, &MethodError{Type: errTooLarge, Description: "that key is too large to import"}
	}
	text := strings.TrimSpace(args.PrivateKey)
	if text == "" {
		return nil, &MethodError{Type: errInvalidArguments, Description: "privateKey is required"}
	}
	armored, mErr := privateKeyArmor(text)
	if mErr != nil {
		return nil, mErr
	}
	if mErr := a.spendKeyOp(userID); mErr != nil {
		return nil, mErr
	}
	ck, already, err := a.custody.ImportKey(ctx, userID, armored, args.Passphrase)
	if err != nil {
		return nil, custodyMethodError(ctx, "import", userID, err, errInvalidKey, args.Passphrase, text)
	}
	keyID, perr := ulid.Parse(ck.KeyID)
	if perr != nil && already {
		// The crypto process names an already-held key by fingerprint only;
		// find the row it means.
		keyID, perr = a.ownKeyByFingerprint(ctx, userID, ck.Fingerprint)
	}
	if perr != nil {
		return nil, &MethodError{Type: errServerFail, Description: "the crypto process returned an unreadable key id"}
	}
	obj, mErr := a.keyObjectAfter(ctx, userID, keyID)
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{
		"accountId":     userID.String(),
		"id":            keyID.String(),
		"fingerprint":   ck.Fingerprint,
		"alreadyStored": already,
		"key":           obj,
	}), nil
}

// privateKeyArmor returns the input as an armored private key block. Armor is
// passed through; a PUBLIC key block is refused (the user's own key needs its
// secret half, and a correspondent's key belongs in ContactKey/import); any
// other text must be the base64 of a binary key, which is armored here (the
// crypto process's import takes armor).
func privateKeyArmor(text string) (string, *MethodError) {
	if strings.HasPrefix(text, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		return "", &MethodError{Type: errPublicKeyRefused,
			Description: "that is a PUBLIC key; your own key needs its secret half (a correspondent's key goes in ContactKey/import)"}
	}
	if strings.HasPrefix(text, "-----BEGIN") {
		return text, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text), ""))
	if err != nil || len(raw) == 0 {
		return "", &MethodError{Type: errInvalidKey,
			Description: "privateKey must be an armored private key or the base64 of a binary one"}
	}
	var b strings.Builder
	w, err := armor.Encode(&b, "PGP PRIVATE KEY BLOCK", nil)
	if err != nil {
		return "", &MethodError{Type: errServerFail, Description: "the key could not be prepared for import"}
	}
	if _, err := w.Write(raw); err != nil {
		return "", &MethodError{Type: errServerFail, Description: "the key could not be prepared for import"}
	}
	if err := w.Close(); err != nil {
		return "", &MethodError{Type: errServerFail, Description: "the key could not be prepared for import"}
	}
	for i := range raw {
		raw[i] = 0
	}
	return b.String(), nil
}

// ownKeyByFingerprint finds the caller's key with this fingerprint.
func (a *API) ownKeyByFingerprint(ctx context.Context, userID ulid.ULID, fpr string) (ulid.ULID, error) {
	var id ulid.ULID
	found := false
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		keys, err := tx.ListUserKeys(ctx)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if strings.EqualFold(k.Fingerprint, fpr) {
				id, found = k.ID, true
				return nil
			}
		}
		return nil
	})
	if err == nil && !found {
		err = errors.New("api: the already-stored key is not in the store")
	}
	return id, err
}

// ---- UserKey/extend --------------------------------------------------------

type userKeyExtendArgs struct {
	AccountID string `json:"accountId"`
	ID        string `json:"id"`
	// Days from NOW: 1..MaxKeyLifetimeDays. Or, instead (#813):
	Days int `json:"days"`
	// ExpiresAt: the exact new expiry (parseExpiresAt). Exactly one of the two.
	ExpiresAt *string `json:"expiresAt"`
}

func (a *API) userKeyExtend(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args userKeyExtendArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if mErr := a.needCustody(); mErr != nil {
		return nil, mErr
	}
	keyID, mErr := parseKeyID(args.ID)
	if mErr != nil {
		return nil, mErr
	}
	var exp KeyExpiry
	switch {
	case args.ExpiresAt != nil && args.Days != 0:
		return nil, &MethodError{Type: errInvalidArguments, Description: "give days or expiresAt, not both"}
	case args.ExpiresAt != nil:
		t, mErr := parseExpiresAt(*args.ExpiresAt, time.Now().UTC())
		if mErr != nil {
			return nil, mErr
		}
		exp.At = &t
	case args.Days <= 0 || args.Days > MaxKeyLifetimeDays:
		return nil, &MethodError{Type: errInvalidArguments,
			Description: fmt.Sprintf("days must be between 1 and %d", MaxKeyLifetimeDays)}
	default:
		d := args.Days
		exp.Days = &d
	}
	if mErr := a.ownKeyExists(ctx, userID, keyID); mErr != nil {
		return nil, mErr
	}
	if _, err := a.custody.ExtendKey(ctx, userID, keyID, exp); err != nil {
		return nil, custodyMethodError(ctx, "extend", userID, err, errInvalidArguments)
	}
	obj, mErr := a.keyObjectAfter(ctx, userID, keyID)
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"id":        keyID.String(),
		"expiresAt": obj["expiresAt"],
		"key":       obj,
	}), nil
}

// ownKeyExists answers notFound for a key id that is not the caller's BEFORE
// the crypto process is asked: RLS makes another user's key invisible here, so
// the crypto process's own ownership check (which logs a confused deputy) is
// never the first line for an ordinary wrong id.
func (a *API) ownKeyExists(ctx context.Context, userID, keyID ulid.ULID) *MethodError {
	return a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		_, ok, err := tx.GetUserKey(ctx, keyID)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !ok {
			return &MethodError{Type: errNotFound, Description: "no such key"}
		}
		return nil
	})
}

// ---- UserKey/revoke --------------------------------------------------------

type userKeyRevokeArgs struct {
	AccountID string `json:"accountId"`
	ID        string `json:"id"`
	// Reason: "compromised", "superseded" or "retired" (RFC 9580 reason codes).
	Reason string `json:"reason"`
	// Confirm: the last 8 hex digits of the key's fingerprint, which the crypto
	// process checks against the key it resolved.
	Confirm string `json:"confirm"`
}

var revokeReasons = map[string]bool{"compromised": true, "superseded": true, "retired": true}

var hex8 = regexp.MustCompile(`^[0-9A-Fa-f]{8}$`)

func (a *API) userKeyRevoke(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args userKeyRevokeArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if mErr := a.needCustody(); mErr != nil {
		return nil, mErr
	}
	keyID, mErr := parseKeyID(args.ID)
	if mErr != nil {
		return nil, mErr
	}
	if !revokeReasons[args.Reason] {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: `reason must be "compromised", "superseded" or "retired"`}
	}
	if !hex8.MatchString(strings.TrimSpace(args.Confirm)) {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: "confirm must be the last 8 characters of the key's fingerprint"}
	}
	if mErr := a.ownKeyExists(ctx, userID, keyID); mErr != nil {
		return nil, mErr
	}
	if _, err := a.custody.RevokeKey(ctx, userID, keyID, args.Reason, strings.TrimSpace(args.Confirm)); err != nil {
		return nil, custodyMethodError(ctx, "revoke", userID, err, errInvalidArguments)
	}
	obj, mErr := a.keyObjectAfter(ctx, userID, keyID)
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"id":        keyID.String(),
		"revokedAt": obj["revokedAt"],
		"key":       obj,
	}), nil
}

// ---- UserKey/export --------------------------------------------------------

type userKeyExportArgs struct {
	AccountID  string `json:"accountId"`
	ID         string `json:"id"`
	Passphrase string `json:"passphrase"`
}

func (a *API) userKeyExport(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args userKeyExportArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "the arguments are not valid JSON for UserKey/export"}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if mErr := a.needCustody(); mErr != nil {
		return nil, mErr
	}
	keyID, mErr := parseKeyID(args.ID)
	if mErr != nil {
		return nil, mErr
	}
	if len([]rune(args.Passphrase)) < MinExportPassphrase {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: fmt.Sprintf("the passphrase must be at least %d characters: it is the only thing protecting the file", MinExportPassphrase)}
	}
	if mErr := a.ownKeyExists(ctx, userID, keyID); mErr != nil {
		return nil, mErr
	}
	if mErr := a.spendKeyOp(userID); mErr != nil {
		return nil, mErr
	}
	ck, wrapped, err := a.custody.ExportKey(ctx, userID, keyID, args.Passphrase)
	if err != nil {
		return nil, custodyMethodError(ctx, "export", userID, err, errInvalidArguments, args.Passphrase)
	}
	suffix := ck.Fingerprint
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return mustMarshal(map[string]any{
		"accountId":   userID.String(),
		"id":          keyID.String(),
		"fingerprint": ck.Fingerprint,
		"armoredKey":  wrapped,
		"fileName":    "mailroom-key-" + strings.ToLower(suffix) + ".asc",
	}), nil
}

// ---- UserKey/set -----------------------------------------------------------

type userKeySetArgs struct {
	AccountID string                     `json:"accountId"`
	IfInState *string                    `json:"ifInState"`
	Create    map[string]json.RawMessage `json:"create"`
	Update    map[string]json.RawMessage `json:"update"`
	Destroy   []string                   `json:"destroy"`
}

// userKeySet updates the store-only facts of a key: its binding, its WKD
// publication and its retirement. ONE kind of change per update (with the one
// combination retire-and-unbind), so every update is one store write with its
// own savepoint and its own documented change rows; a client sends a second
// update for a second change. Keys are created by UserKey/generate and
// /import and never destroyed (a generated key is the only copy: retire or
// revoke it instead).
func (a *API) userKeySet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args userKeySetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	res := setResult{AccountID: userID.String()}
	if len(args.Create) > 0 {
		res.NotCreated = map[string]SetError{}
		for cid := range args.Create {
			res.NotCreated[cid] = SetError{Type: setForbidden,
				Description: "keys are created with UserKey/generate or UserKey/import"}
		}
	}
	if len(args.Destroy) > 0 {
		res.NotDestroyed = map[string]SetError{}
		for _, id := range args.Destroy {
			res.NotDestroyed[id] = SetError{Type: setForbidden,
				Description: "keys are never destroyed (a generated key is the only copy); retire or revoke it instead"}
		}
	}

	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		before, err := tx.ListUserKeys(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		now := time.Now().UTC()
		res.OldState = userKeyState(before, now)
		if args.IfInState != nil && *args.IfInState != res.OldState {
			return &MethodError{Type: errStateMismatch, Description: "the keys have changed since that state"}
		}
		for rawID, patch := range args.Update {
			extra, serr := a.userKeyUpdate(ctx, tx, rawID, patch)
			if serr != nil {
				if res.NotUpdated == nil {
					res.NotUpdated = map[string]SetError{}
				}
				res.NotUpdated[rawID] = *serr
				continue
			}
			if res.Updated == nil {
				res.Updated = map[string]any{}
			}
			res.Updated[rawID] = extra
		}
		after, err := tx.ListUserKeys(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		res.NewState = userKeyState(after, now)
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(res), nil
}

type userKeyPatch struct {
	identityID   *string // set and nil: unbind
	hasIdentity  bool
	wkdPublished *bool
	decryptOnly  *bool
}

func parseUserKeyPatch(raw json.RawMessage) (userKeyPatch, *SetError) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return userKeyPatch{}, &SetError{Type: setInvalidPatch, Description: "the patch must be an object"}
	}
	var p userKeyPatch
	var bad []string
	for k, v := range m {
		switch k {
		case "identityId":
			p.hasIdentity = true
			if string(v) != "null" {
				var s string
				if err := json.Unmarshal(v, &s); err != nil {
					bad = append(bad, k)
					continue
				}
				p.identityID = &s
			}
		case "wkdPublished":
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				bad = append(bad, k)
				continue
			}
			p.wkdPublished = &b
		case "decryptOnly":
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				bad = append(bad, k)
				continue
			}
			p.decryptOnly = &b
		default:
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		return userKeyPatch{}, &SetError{Type: setInvalidProps, Properties: bad,
			Description: "only identityId, wkdPublished and decryptOnly can be changed"}
	}
	return p, nil
}

// addressNotInKey is the bind WARNING (#812), not an error: the binding is
// made, and updated[id] carries it so the client can say that correspondents'
// clients may refuse or flag a key that does not name the address it signs for.
const addressNotInKey = "addressNotInKey"

// userKeyUpdate applies one patch. Each branch is ONE store call with its own
// savepoint, so a refusal leaves nothing half-written. A bind returns the
// server-set facts for updated[id] (#812): boundAddressListed, and the
// addressNotInKey warning when it is false.
func (a *API) userKeyUpdate(ctx context.Context, tx *store.UserTx, rawID string, raw json.RawMessage) (any, *SetError) {
	serr := a.userKeyApply(ctx, tx, rawID, raw)
	if serr != nil {
		return nil, serr
	}
	p, _ := parseUserKeyPatch(raw)
	if !p.hasIdentity || p.identityID == nil || p.decryptOnly != nil || p.wkdPublished != nil {
		return nil, nil
	}
	keyID, _ := ulid.Parse(rawID)
	key, found, err := tx.GetUserKey(ctx, keyID)
	if err != nil || !found || key.IdentityID == nil {
		return nil, nil
	}
	list, err := tx.GetIdentities(ctx, []ulid.ULID{*key.IdentityID})
	if err != nil || len(list) == 0 {
		return nil, nil
	}
	listed := boundAddressListed(key.PublicKey, &list[0], time.Now().UTC())
	if listed == nil {
		return nil, nil
	}
	out := map[string]any{"boundAddressListed": listed}
	if listed == false {
		out["warning"] = addressNotInKey
	}
	return out, nil
}

func (a *API) userKeyApply(ctx context.Context, tx *store.UserTx, rawID string, raw json.RawMessage) *SetError {
	keyID, err := ulid.Parse(rawID)
	if err != nil {
		return &SetError{Type: setNotFound, Description: "no such key"}
	}
	p, serr := parseUserKeyPatch(raw)
	if serr != nil {
		return serr
	}
	n := 0
	for _, set := range []bool{p.hasIdentity, p.wkdPublished != nil, p.decryptOnly != nil} {
		if set {
			n++
		}
	}
	retireAndUnbind := p.decryptOnly != nil && *p.decryptOnly && p.hasIdentity && p.identityID == nil && p.wkdPublished == nil
	switch {
	case n == 0:
		return nil // an empty patch is a no-op (RFC 8620 §5.3)
	case n > 1 && !retireAndUnbind:
		return &SetError{Type: setOneChangeAtATime,
			Description: "change one of identityId, wkdPublished or decryptOnly per update (decryptOnly: true may come with identityId: null)"}
	}

	key, found, err := tx.GetUserKey(ctx, keyID)
	if err != nil {
		return &SetError{Type: errServerFail, Description: err.Error()}
	}
	if !found {
		return &SetError{Type: setNotFound, Description: "no such key"}
	}

	switch {
	case p.decryptOnly != nil:
		if !*p.decryptOnly {
			if key.DecryptOnly {
				return &SetError{Type: setRetireIsPermanent, Properties: []string{"decryptOnly"},
					Description: "a retired key cannot be put back into use; generate or import another"}
			}
			return nil // already not retired: no-op
		}
		_, err := tx.RetireUserKey(ctx, keyID, retireAndUnbind)
		switch {
		case errors.Is(err, store.ErrKeyIsDefault):
			return &SetError{Type: setKeyIsDefault, Properties: []string{"decryptOnly"},
				Description: "that key is an identity's signing key; send identityId: null with decryptOnly: true to unbind it in the same step, or bind another key first"}
		case err != nil:
			return &SetError{Type: errServerFail, Description: err.Error()}
		}
		return nil

	case p.hasIdentity && p.identityID == nil:
		_, err := tx.UnbindUserKey(ctx, keyID)
		switch {
		case errors.Is(err, store.ErrKeyPublished):
			return &SetError{Type: setKeyIsPublished, Properties: []string{"identityId"},
				Description: "that key is published in WKD at its address; unpublish it first"}
		case err != nil:
			return &SetError{Type: errServerFail, Description: err.Error()}
		}
		return nil

	case p.hasIdentity:
		identityID, err := ulid.Parse(*p.identityID)
		if err != nil {
			return &SetError{Type: setInvalidProps, Properties: []string{"identityId"}, Description: "not an identity id"}
		}
		if key.DecryptOnly || key.RevokedAt != nil {
			return &SetError{Type: setKeyRetired, Properties: []string{"identityId"},
				Description: "a retired or revoked key cannot sign, so it cannot be bound"}
		}
		ok, err := tx.BindKeyToIdentity(ctx, keyID, identityID)
		if err != nil {
			return &SetError{Type: errServerFail, Description: err.Error()}
		}
		if !ok {
			return &SetError{Type: setInvalidProps, Properties: []string{"identityId"}, Description: "no such identity"}
		}
		return nil

	case *p.wkdPublished:
		return a.publishOwnKey(ctx, tx, key)

	default:
		if _, err := tx.UnpublishKey(ctx, keyID); err != nil {
			return &SetError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}
}

// publishOwnKey puts a bound key in WKD, the way pgp-publish does, but refuses
// the two silent no-ops a CLI operator could see and a browser user could not:
// WKD switched off on this instance, and an address at a domain this instance
// does not answer for. Either would commit `wkdPublished: true` for a key no
// correspondent can ever fetch.
func (a *API) publishOwnKey(ctx context.Context, tx *store.UserTx, key store.UserKey) *SetError {
	if a.wkdDomain == "" {
		return &SetError{Type: setWKDNotConfigured, Properties: []string{"wkdPublished"},
			Description: "this server does not publish keys over WKD (MAILROOM_WKD_DOMAIN is not set)"}
	}
	if key.IdentityID == nil {
		return &SetError{Type: setNotPublishable, Properties: []string{"wkdPublished"},
			Description: "bind the key to an address first: WKD publishes a key AT an address"}
	}
	list, err := tx.GetIdentities(ctx, []ulid.ULID{*key.IdentityID})
	if err != nil {
		return &SetError{Type: errServerFail, Description: err.Error()}
	}
	if len(list) == 0 || list[0].Kind != store.IdentityFixed || list[0].Address == "" {
		return &SetError{Type: setNotPublishable, Properties: []string{"wkdPublished"},
			Description: "only a key bound to a fixed address can be published; a domain wildcard has no single address"}
	}
	local, domain, err := wkd.SplitAddress(list[0].Address)
	if err != nil {
		return &SetError{Type: setNotPublishable, Properties: []string{"wkdPublished"}, Description: "the bound address does not parse"}
	}
	if !strings.EqualFold(domain, a.wkdDomain) {
		return &SetError{Type: setNotPublishable, Properties: []string{"wkdPublished"},
			Description: "this server publishes keys only for " + a.wkdDomain + "; the key's address is at " + domain}
	}
	ok, err := tx.PublishKey(ctx, key.ID, wkd.HashLocalPart(local))
	switch {
	case errors.Is(err, store.ErrNotPublishable):
		return &SetError{Type: setNotPublishable, Properties: []string{"wkdPublished"},
			Description: strings.TrimPrefix(err.Error(), store.ErrNotPublishable.Error()+": ")}
	case err != nil:
		return &SetError{Type: errServerFail, Description: err.Error()}
	case !ok:
		return &SetError{Type: setNotFound, Description: "no such key"}
	}
	return nil
}
