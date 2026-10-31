// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
)

// This file holds the canonical Identity layer (spec §5, "the alias story") —
// and, with it, THE SEND-AS SECURITY BOUNDARY.
//
// An Identity is a verified send-as right, and it is the ONLY thing the server
// will send mail as (spec §5: "the server refuses any From address not matching
// a verified identity/domain for the authenticated user — even though the
// provider would also reject it. Defense in depth for multi-user: one user's
// misconfiguration must never burn another domain's reputation from a shared
// instance."). ResolveSendIdentity is that check; every send path goes through
// it, and it is RLS-scoped, so one user's identities can never authorize
// another user's From.
//
// Two kinds (app.identity_kind):
//
//	fixed           — one exact address ("you@example.com").
//	domain_wildcard — any address at a domain ("*@example.com"). This is the
//	                  catchall/wildcard-send model spec §5 describes (arbitrary
//	                  per-site addresses land in one mailbox, there is no finite
//	                  alias list to enumerate, and the provider is configured to
//	                  let the account send as any of them). It maps exactly onto
//	                  RFC 8621 §6's own wildcard form, where an Identity whose
//	                  email local part is "*" means "any local part at this
//	                  domain" — so no Mailroom extension is needed to express it.
//
// An Identity IS a JMAP-visible object (changelog.go ObjectIdentity), so every
// create/update/destroy appends exactly one Identity change_log row in the SAME
// UserTx as the mutation (the load-bearing invariant — CLAUDE.md).

// IdentityKind mirrors the app.identity_kind enum.
type IdentityKind string

const (
	// IdentityFixed is one exact verified address.
	IdentityFixed IdentityKind = "fixed"
	// IdentityWildcard verifies every address at a domain (spec §5 catchall).
	IdentityWildcard IdentityKind = "domain_wildcard"
)

// ErrIdentityNotFound is returned when an identity id does not resolve for the
// caller (RLS makes another user's id indistinguishable from a missing one).
var ErrIdentityNotFound = errors.New("store: identity not found")

// ErrIdentityExists is returned when a create would collide with the caller's
// UNIQUE (user_id, address) / UNIQUE (user_id, domain) index.
var ErrIdentityExists = errors.New("store: identity already exists")

// ErrFromNotVerified is the SECURITY answer: the requested From address does
// not match any enabled, verified identity of this user. Callers MUST refuse
// the send (spec §5); it is never downgraded to a warning.
var ErrFromNotVerified = errors.New("store: from address is not a verified identity for this user")

// Identity is the canonical projection of a send-as right.
type Identity struct {
	ID            ulid.ULID
	AccountID     ulid.ULID
	Kind          IdentityKind
	Address       string // set for IdentityFixed
	Domain        string // set for IdentityWildcard
	DisplayName   string
	SignatureHTML string
	Enabled       bool
	// SigningKeyID is the key bound as this identity's default_key_id WHEN THAT
	// KEY CAN STILL SIGN, and nil otherwise — no key bound, or one that is
	// revoked, decrypt-only, or holds no private material (#492).
	//
	// It is the same question SigningKeyForIdentity answers, and both reads share
	// signingKeyUsable so that "does this address sign" cannot have two answers:
	// a client told "Will sign" by this projection and a send path that then
	// declines to sign would be the projection lying about its own bytes.
	//
	// The KEY ID, never key material — the id is already what UserKey/get
	// publishes, so this adds no exposure and gives the send line its one fact.
	//
	// Since #796 it is also nil when the key's own PACKETS say it can no longer
	// sign (primary expired or revoked in its packets): see scanIdentities.
	SigningKeyID *ulid.ULID
}

// Email renders the identity as an RFC 8621 §6 Identity "email" value: the
// exact address for a fixed identity, and the wildcard form "*@domain" for a
// domain-wildcard one (which RFC 8621 defines as "any local part at this
// domain").
func (i Identity) Email() string {
	if i.Kind == IdentityWildcard {
		return "*@" + i.Domain
	}
	return i.Address
}

// IdentityRow is the input to CreateIdentity.
type IdentityRow struct {
	AccountID     ulid.ULID
	Kind          IdentityKind
	Address       string // fixed only
	Domain        string // wildcard only
	DisplayName   string
	SignatureHTML string
	Enabled       bool
}

// IdentityPatch is a partial update. A nil pointer means "leave unchanged".
// The address/domain are deliberately absent: they ARE the verification, so
// they are immutable — re-pointing an existing identity at a new address would
// be a privilege escalation with no verification step.
type IdentityPatch struct {
	DisplayName   *string
	SignatureHTML *string
	Enabled       *bool
}

// SendIdentity is the result of the From-validation check: the verified
// identity that authorizes an address, the account whose SMTP submission will
// carry it, and the literal address to put in the From header (which for a
// wildcard identity is the caller's derived alias, not the identity's own).
type SendIdentity struct {
	IdentityID  ulid.ULID
	AccountID   ulid.ULID
	Kind        IdentityKind
	FromAddress string
	DisplayName string
	// ViaSubaddress means the authorization came from the SUBADDRESS FALLBACK:
	// FromAddress is "me+tag@d" and the identity found is the one for "me@d".
	// Callers that care whether an identity row exists for this exact address —
	// Identity/set's duplicate check — must consult this, because Kind is the
	// BASE identity's kind and says nothing about FromAddress.
	ViaSubaddress bool
}

// NormalizeAddress lowercases and trims an email address for comparison. It is
// deliberately conservative: it does NOT strip sub-addressing (+tag) or dots,
// because under a catchall those are meaningful distinct addresses.
func NormalizeAddress(addr string) string {
	a := strings.TrimSpace(addr)
	a = strings.Trim(a, "<>")
	return strings.ToLower(strings.TrimSpace(a))
}

// AddressDomain returns the domain part of an address, lowercased ("" if the
// address has no exactly-one "@").
func AddressDomain(addr string) string {
	a := NormalizeAddress(addr)
	at := strings.LastIndex(a, "@")
	if at <= 0 || at == len(a)-1 {
		return ""
	}
	if strings.Count(a, "@") != 1 {
		return ""
	}
	return a[at+1:]
}

// CreateIdentity inserts a verified send-as right and appends the Identity
// change_log row (the invariant). The unique-violation path runs under a
// SAVEPOINT so a duplicate is a RECOVERABLE ErrIdentityExists rather than an
// aborted transaction (SQLSTATE 25P02) — the same rule as CreateLabel: a
// /set that reports one object in notCreated must not discard the rest of the
// batch.
func (tx *UserTx) CreateIdentity(ctx context.Context, row IdentityRow) (ulid.ULID, error) {
	var addr, domain any
	switch row.Kind {
	case IdentityFixed:
		a := NormalizeAddress(row.Address)
		if AddressDomain(a) == "" {
			return ulid.ULID{}, fmt.Errorf("store: create identity: %q is not a valid address", row.Address)
		}
		addr = a
	case IdentityWildcard:
		d := strings.ToLower(strings.TrimSpace(row.Domain))
		if d == "" || strings.Contains(d, "@") {
			return ulid.ULID{}, fmt.Errorf("store: create identity: %q is not a valid domain", row.Domain)
		}
		domain = d
	default:
		return ulid.ULID{}, fmt.Errorf("store: create identity: unknown kind %q", row.Kind)
	}

	id := NewULID()
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create identity (savepoint): %w", err)
	}
	if _, err := sp.Exec(ctx, `
		INSERT INTO app.identities
			(id, user_id, account_id, kind, address, domain, display_name,
			 signature_html, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id.String(), tx.userID.String(), row.AccountID.String(), string(row.Kind),
		addr, domain, row.DisplayName, row.SignatureHTML, row.Enabled); err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if isUniqueViolation(err) {
			return ulid.ULID{}, ErrIdentityExists
		}
		return ulid.ULID{}, fmt.Errorf("store: create identity: %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return ulid.ULID{}, fmt.Errorf("store: create identity (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectIdentity, id, OpCreated); err != nil {
		return ulid.ULID{}, err
	}
	return id, nil
}

// identityColumns is the SELECT list shared by every identity read, in the
// order scanIdentities expects. Aliased (`i`, `k`) because identityFrom joins
// app.user_keys, and both tables have an `id`, a `user_id` and a `kind`.
const identityColumns = `i.id, i.account_id, i.kind, COALESCE(i.address, ''),
	COALESCE(i.domain, ''), i.display_name, i.signature_html, i.enabled, k.id, k.public_key`

// identityFrom is the FROM clause shared by every identity read: the identity,
// plus the key bound as its default WHEN THAT KEY CAN STILL SIGN (#492).
//
// A LEFT JOIN, so an identity with no usable key is still returned with a NULL
// signing key rather than disappearing — the difference between "this address
// will not sign" and "this address does not exist".
//
// ⚠ The predicate belongs in the JOIN's ON, not in a WHERE: moved to WHERE it
// would filter out the identity itself, which is the classic left-join mistake
// and would make every keyless identity invisible to Identity/get.
const identityFrom = `FROM app.identities i
	  LEFT JOIN app.user_keys k
	         ON k.id = i.default_key_id
	        AND k.user_id = i.user_id
	        AND ` + signingKeyUsable

// ListIdentities returns every identity of the caller, in a stable order
// (fixed addresses first, then wildcards, then by id) — the total order
// Identity/get with ids:null needs.
func (tx *UserTx) ListIdentities(ctx context.Context) ([]Identity, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT `+identityColumns+`
		  `+identityFrom+`
		 WHERE i.user_id = $1
		 ORDER BY i.kind, COALESCE(i.address, i.domain), i.id`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list identities: %w", err)
	}
	defer rows.Close()
	return scanIdentities(rows)
}

// GetIdentities returns the identities for the requested ids (RLS-scoped).
func (tx *UserTx) GetIdentities(ctx context.Context, ids []ulid.ULID) ([]Identity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT `+identityColumns+`
		  `+identityFrom+`
		 WHERE i.user_id = $1 AND i.id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: get identities: %w", err)
	}
	defer rows.Close()
	return scanIdentities(rows)
}

// UpdateIdentity applies a partial update and appends exactly one Identity
// change_log row when something actually changes (a no-op patch is a true
// no-op). The address/domain are immutable (see IdentityPatch).
func (tx *UserTx) UpdateIdentity(ctx context.Context, id ulid.ULID, patch IdentityPatch) (found bool, err error) {
	var (
		name    string
		sig     string
		enabled bool
	)
	err = tx.tx.QueryRow(ctx, `
		SELECT display_name, signature_html, enabled
		  FROM app.identities
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`,
		tx.userID.String(), id.String()).Scan(&name, &sig, &enabled)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: update identity (read): %w", err)
	}

	nName, nSig, nEnabled := name, sig, enabled
	if patch.DisplayName != nil {
		nName = *patch.DisplayName
	}
	if patch.SignatureHTML != nil {
		nSig = *patch.SignatureHTML
	}
	if patch.Enabled != nil {
		nEnabled = *patch.Enabled
	}
	if nName == name && nSig == sig && nEnabled == enabled {
		return true, nil // true no-op: no change_log row
	}

	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.identities
		   SET display_name = $3, signature_html = $4, enabled = $5
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String(), nName, nSig, nEnabled); err != nil {
		return false, fmt.Errorf("store: update identity (write): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectIdentity, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteIdentity removes a send-as right and appends the Identity change_log
// row. Submissions reference identities with a plain (RESTRICT) FK, so an
// identity that has already sent mail cannot be deleted — that is deliberate:
// the audit trail of what was sent as whom must not be erasable by deleting
// the identity. The caller surfaces that as a per-object /set error.
func (tx *UserTx) DeleteIdentity(ctx context.Context, id ulid.ULID) (found bool, err error) {
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT: an FK violation must not poison the tx
	if err != nil {
		return false, fmt.Errorf("store: delete identity (savepoint): %w", err)
	}
	var deleted string
	err = sp.QueryRow(ctx, `
		DELETE FROM app.identities WHERE user_id = $1 AND id = $2 RETURNING id`,
		tx.userID.String(), id.String()).Scan(&deleted)
	if err != nil {
		if isNoRows(err) {
			if cerr := sp.Commit(ctx); cerr != nil {
				return false, fmt.Errorf("store: delete identity (release): %w", cerr)
			}
			return false, nil
		}
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		return false, fmt.Errorf("store: delete identity: %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return false, fmt.Errorf("store: delete identity (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectIdentity, id, OpDestroyed); err != nil {
		return false, err
	}
	return true, nil
}

// ResolveSendIdentity is the SERVER-SIDE FROM VALIDATION (spec §5) — the one
// function every send path must clear. It answers: may THIS user send as THIS
// address, and if so, under which identity and account?
//
// Precedence:
//  1. an enabled `fixed` identity whose address matches exactly; then
//  2. an enabled `domain_wildcard` identity whose domain matches the address's
//     domain (the catchall/wildcard-send model — RFC 8621's "*@domain").
//
// Anything else is ErrFromNotVerified. Disabled identities never authorize a
// send. The query is RLS-scoped, so another user's identity is not merely
// deprioritized — it is invisible.
func (tx *UserTx) ResolveSendIdentity(ctx context.Context, from string) (SendIdentity, error) {
	addr := NormalizeAddress(from)
	domain := AddressDomain(addr)
	if domain == "" {
		return SendIdentity{}, ErrFromNotVerified
	}

	var (
		si    SendIdentity
		id    string
		acct  string
		kind  string
		dName string
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT id, account_id, kind, display_name
		  FROM app.identities
		 WHERE user_id = $1
		   AND enabled = true
		   AND ( (kind = 'fixed'           AND address = $2)
		      OR (kind = 'domain_wildcard' AND domain  = $3) )
		 ORDER BY (kind = 'fixed') DESC, id
		 LIMIT 1`,
		tx.userID.String(), addr, domain).Scan(&id, &acct, &kind, &dName)
	if err != nil {
		if isNoRows(err) {
			// Nothing verified this address outright. LAST resort — see
			// resolveSubaddress for why it is last and not folded into the
			// query above.
			return tx.resolveSubaddress(ctx, addr)
		}
		return SendIdentity{}, fmt.Errorf("store: resolve send identity: %w", err)
	}

	if si.IdentityID, err = ulid.Parse(id); err != nil {
		return SendIdentity{}, fmt.Errorf("store: parse identity id %q: %w", id, err)
	}
	if si.AccountID, err = ulid.Parse(acct); err != nil {
		return SendIdentity{}, fmt.Errorf("store: parse identity account id %q: %w", acct, err)
	}
	si.Kind = IdentityKind(kind)
	si.FromAddress = addr
	si.DisplayName = dName
	return si, nil
}

// resolveSubaddress authorizes "me+tag@d" on the strength of a verified FIXED
// identity for "me@d" (#62; RFC 5233 calls it subaddressing).
//
// WHY THIS SHAPE, because a more obvious one is unsafe. The tempting version is
// "authorize a subaddress once a message has ARRIVED at it" — the provider has
// then demonstrably delivered it, so no per-provider guessing is needed. That
// evidence is FORGEABLE: we are an IMAP client, not the MDA, so we never see the
// real RCPT TO, and `Delivered-To` is just bytes a sender can write (see
// internal/alias/derive.go, which is built entirely around that fact). Wiring it
// to authorization would let a hostile sender mint entries in the user's own
// From picker, and would make this the first path in the codebase where message
// CONTENT widens what the user may send as. Identity/set refuses to do that on
// purpose — it grants nothing ResolveSendIdentity does not already allow.
//
// So this consults NO message data at all. It is a database fact (a verified
// identity for the base address) plus a syntactic rule (RFC 5233's delimiter).
// Its one failure mode is a provider that does not implement subaddressing, and
// that surfaces as a bounced send: visible, recoverable, and not a security
// boundary.
//
// LAST, deliberately. It runs only when the main query matched nothing, so an
// exact fixed identity and a domain_wildcard both still win — a catchall user's
// behaviour is bit-for-bit what it was before this existed, which is the
// non-regression the tests pin.
func (tx *UserTx) resolveSubaddress(ctx context.Context, addr string) (SendIdentity, error) {
	base, ok := SubaddressBase(addr)
	if !ok {
		return SendIdentity{}, ErrFromNotVerified
	}

	var si SendIdentity
	var id, acct, dName string
	// FIXED ONLY, and no recursion. A wildcard would have matched the domain in
	// the caller's query already, so reaching here means none covers it; and the
	// base is stripped once, so "me+a+b@d" resolves against "me@d" and never
	// walks further.
	err := tx.tx.QueryRow(ctx, `
		SELECT id, account_id, display_name
		  FROM app.identities
		 WHERE user_id = $1
		   AND enabled = true
		   AND kind = 'fixed'
		   AND address = $2
		 ORDER BY id
		 LIMIT 1`,
		tx.userID.String(), base).Scan(&id, &acct, &dName)
	if err != nil {
		if isNoRows(err) {
			return SendIdentity{}, ErrFromNotVerified
		}
		return SendIdentity{}, fmt.Errorf("store: resolve subaddress identity: %w", err)
	}
	if si.IdentityID, err = ulid.Parse(id); err != nil {
		return SendIdentity{}, fmt.Errorf("store: parse identity id %q: %w", id, err)
	}
	if si.AccountID, err = ulid.Parse(acct); err != nil {
		return SendIdentity{}, fmt.Errorf("store: parse identity account id %q: %w", acct, err)
	}
	si.Kind = IdentityFixed
	// The address the user is sending AS — the subaddress, not the base. It is
	// what goes in the From line, and (via ingest's DeriveAddressedIdentity) what
	// is recorded as the message's `addressed_address`, so `deliveredTo` and the
	// `alias` rule field keep naming the exact address a sender used.
	si.FromAddress = addr
	si.DisplayName = dName
	si.ViaSubaddress = true
	return si, nil
}

// SubaddressBase strips an RFC 5233 subaddress: "me+shop@d" -> "me@d".
//
// ok is false when there is nothing to strip or the result would be nonsense —
// no "+", an empty base local-part ("+tag@d"), an empty detail ("me+@d"), or a
// missing domain. Splitting at the FIRST "+" is what RFC 5233 specifies: the
// detail is everything after the delimiter, so "me+a+b@d" has base "me@d".
//
// This is NOT part of address normalization, and must not become part of it:
// NormalizeAddress deliberately keeps "+tag" because under a catchall those are
// meaningful DISTINCT addresses. Stripping belongs only to the authorization
// fallback above, where the base is a fact being looked up rather than an
// identity being compared.
func SubaddressBase(addr string) (string, bool) {
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return "", false
	}
	local, domain := addr[:at], addr[at+1:]
	plus := strings.Index(local, "+")
	if plus <= 0 || plus == len(local)-1 {
		return "", false
	}
	return local[:plus] + "@" + domain, true
}

// MatchVerifiedAddress returns the first of the candidate addresses that this
// user may send as, and its identity — the store half of the reply-From
// derivation (spec §5: default From = the exact address the message was
// delivered to). Candidates are tried in the caller's precedence order; the
// first verified one wins. ok is false when none is verified.
func (tx *UserTx) MatchVerifiedAddress(ctx context.Context, candidates []string) (SendIdentity, bool, error) {
	for _, c := range candidates {
		si, err := tx.ResolveSendIdentity(ctx, c)
		if err == nil {
			return si, true, nil
		}
		if !errors.Is(err, ErrFromNotVerified) {
			return SendIdentity{}, false, err
		}
	}
	return SendIdentity{}, false, nil
}

// DefaultIdentityForAccount returns the identity a new compose defaults to when
// nothing better is derivable: the account's first enabled fixed identity, else
// its first enabled wildcard. found is false when the account has no enabled
// identity at all (in which case the account cannot send, and the caller must
// say so rather than guess an address).
func (tx *UserTx) DefaultIdentityForAccount(ctx context.Context, accountID ulid.ULID) (Identity, bool, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT `+identityColumns+`
		  `+identityFrom+`
		 WHERE i.user_id = $1 AND i.account_id = $2 AND i.enabled = true
		 ORDER BY (i.kind = 'fixed') DESC, i.id
		 LIMIT 1`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return Identity{}, false, fmt.Errorf("store: default identity: %w", err)
	}
	defer rows.Close()
	out, err := scanIdentities(rows)
	if err != nil {
		return Identity{}, false, err
	}
	if len(out) == 0 {
		return Identity{}, false, nil
	}
	return out[0], true, nil
}

// scanIdentities drains an identity result set in identityColumns order.
func scanIdentities(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Identity, error) {
	var out []Identity
	for rows.Next() {
		var (
			i         Identity
			id, acct  string
			kind      string
			addr, dom string
			// A POINTER, so a NULL from the LEFT JOIN survives the scan as "no
			// usable signing key" instead of collapsing into a zero ULID that
			// would render as a real key id nothing can resolve.
			signingKey *string
			// The bound key's public half, read only to ask its packets
			// whether it can still sign (see below). Never kept.
			signingPub []byte
		)
		if err := rows.Scan(&id, &acct, &kind, &addr, &dom,
			&i.DisplayName, &i.SignatureHTML, &i.Enabled, &signingKey, &signingPub); err != nil {
			return nil, fmt.Errorf("store: scan identity: %w", err)
		}
		var err error
		if i.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse identity id %q: %w", id, err)
		}
		if i.AccountID, err = ulid.Parse(acct); err != nil {
			return nil, fmt.Errorf("store: parse identity account id %q: %w", acct, err)
		}
		// ⚠ THE PACKETS, TOO (#796, closing ADR-040's Consequences gap). The JOIN
		// applies the column half of "can this key sign" (signingKeyUsable); a
		// bound key whose PRIMARY has expired passes it and yet compose sends
		// unsigned (pgpkeys.OwnKeyFitness). Reading the same function here is
		// what keeps "Will sign" from promising a signature the send path will
		// not make. It is a READ-TIME projection of the clock, like UserKey's
		// expiresAt: nothing stored changes when a key's date passes, so there
		// is no mutation and no change_log row (ADR-040 amendment). A key whose
		// packets cannot be read keeps the old answer, exactly as compose does.
		if signingKey != nil {
			if _, canSign := pgpkeys.OwnKeyFitness(signingPub, time.Now()); !canSign {
				signingKey = nil
			}
		}
		if signingKey != nil {
			kid, err := ulid.Parse(*signingKey)
			if err != nil {
				return nil, fmt.Errorf("store: parse identity signing key id %q: %w", *signingKey, err)
			}
			i.SigningKeyID = &kid
		}
		i.Kind = IdentityKind(kind)
		i.Address = addr
		i.Domain = dom
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan identities: %w", err)
	}
	return out, nil
}
