// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"mailroom/internal/xoauth2"
)

// THE ID TOKEN, AND HOW MUCH OF IT IS CHECKED (ADR-046 §3).
//
// The connect flow learns WHO signed in — the provider account's subject, and
// its address — from the ID token in the code-exchange response. Its claims
// are checked: issuer, audience (and azp when there are several audiences),
// expiry, issue time, the nonce this flow sent, a usable subject, and for
// Google `email_verified`.
//
// Its SIGNATURE is not verified, deliberately, under OIDC Core §3.1.3.7 (6):
// "If the ID Token is received via direct communication between the Client
// and the Token Endpoint (which it is in this flow), the TLS server validation
// MAY be used to validate the issuer in place of checking the token
// signature." That is exactly this flow's situation, and it holds strictly:
//
//   - the token endpoint is the registry's constant, reached over verified TLS
//     with a client that never follows a redirect (tokenCall), so the response
//     comes from the provider or the TLS ecosystem is already broken;
//   - the request carried the client secret AND the PKCE verifier, so nobody
//     else could have redeemed the code;
//   - a party able to forge this response could equally forge the access token
//     beside it, which a signature on the ID token would not protect.
//
// Verifying signatures would add a JWKS fetch-and-cache path to each provider
// (for Microsoft's multi-tenant /common, with per-tenant issuers) and buy
// nothing against that threat model. What the claims checks DO buy: a token
// minted for another client (audience), a stale or replayed one (expiry,
// nonce), and a provider mix-up (issuer per provider).
//
// The address is not trusted on the ID token's word either: the final arbiter
// is a successful XOAUTH2 IMAP sign-in as that address with the access token
// (Connector.checkMailbox). Identity binding — "is this the same mailbox as
// before" — uses the SUBJECT only, never the address, because Microsoft's
// `email` claim is not verified in every tenant.

// idClaims are the ID-token claims the connect flow reads.
type idClaims struct {
	Issuer            string          `json:"iss"`
	Audience          json.RawMessage `json:"aud"`
	AuthorizedParty   string          `json:"azp"`
	Expiry            json.Number     `json:"exp"`
	IssuedAt          json.Number     `json:"iat"`
	Nonce             string          `json:"nonce"`
	Subject           string          `json:"sub"`
	Email             string          `json:"email"`
	EmailVerified     json.RawMessage `json:"email_verified"`
	PreferredUsername string          `json:"preferred_username"`
	TenantID          string          `json:"tid"`
}

// Identity is who completed the provider's sign-in.
type Identity struct {
	Subject string
	Address string
}

// idTokenSkew tolerates clock drift between this server and the provider.
const idTokenSkew = 5 * time.Minute

// maxIDToken bounds what is parsed; real ID tokens are ~1-2 KiB.
const maxIDToken = 16 << 10

// msaTenantID is the tenant every personal Microsoft account's tokens name.
const msaTenantID = "9188040d-6c67-4c5b-b112-36a304b66dad"

var (
	microsoftIssuer = regexp.MustCompile(`^https://login\.microsoftonline\.com/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/v2\.0$`)
	tenantGUID      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// errIdentity is the class of every ID-token refusal; the detail names the
// check, never a claim value an attacker could have chosen.
var errIdentity = errors.New("oauthmail: the provider's identity token was not acceptable")

func identityErr(why string) error { return fmt.Errorf("%w: %s", errIdentity, why) }

// verifyIDToken checks raw against p and the flow's nonce and returns who
// signed in. See the file comment for what is and is not checked, and why.
func (p Provider) verifyIDToken(raw xoauth2.Secret, nonce string, now time.Time) (Identity, error) {
	tok := raw.Reveal()
	if tok == "" {
		return Identity{}, identityErr("no id_token in the token response")
	}
	if len(tok) > maxIDToken {
		return Identity{}, identityErr("id_token too large")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Identity{}, identityErr("id_token is not a JWS compact serialization")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return Identity{}, identityErr("id_token payload is not base64url")
	}
	var c idClaims
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return Identity{}, identityErr("id_token payload is not a JSON object")
	}

	if err := p.checkIssuer(c); err != nil {
		return Identity{}, err
	}
	auds, err := audiences(c.Audience)
	if err != nil {
		return Identity{}, err
	}
	if !contains(auds, p.Client.ID) {
		return Identity{}, identityErr("id_token audience is not this server's client")
	}
	if len(auds) > 1 && c.AuthorizedParty != p.Client.ID {
		return Identity{}, identityErr("id_token has several audiences and azp is not this server's client")
	}
	exp, err := c.Expiry.Int64()
	if err != nil || exp <= 0 {
		return Identity{}, identityErr("id_token has no expiry")
	}
	if now.After(time.Unix(exp, 0).Add(idTokenSkew)) {
		return Identity{}, identityErr("id_token has expired")
	}
	if iat, err := c.IssuedAt.Int64(); err == nil && time.Unix(iat, 0).After(now.Add(idTokenSkew)) {
		return Identity{}, identityErr("id_token was issued in the future")
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1 {
		return Identity{}, identityErr("id_token nonce does not match this sign-in")
	}
	if !usableSubject(c.Subject) {
		return Identity{}, identityErr("id_token has no usable subject")
	}

	addr := ""
	for _, claim := range p.AddressClaims {
		var v string
		switch claim {
		case "email":
			v = c.Email
		case "preferred_username":
			v = c.PreferredUsername
		}
		if v = strings.TrimSpace(v); v != "" {
			addr = v
			break
		}
	}
	if p.Name == ProviderGoogle && addr != "" && !truthy(c.EmailVerified) {
		return Identity{}, identityErr("Google says this address is not verified")
	}
	if !plausibleAddress(addr) {
		return Identity{}, identityErr("id_token names no mailbox address")
	}
	return Identity{Subject: c.Subject, Address: strings.ToLower(addr)}, nil
}

// checkIssuer pins the issuer to the provider: Google's two documented forms;
// Microsoft's per-tenant v2.0 issuer, consistent with `tid` and with the
// operator's tenant narrowing.
func (p Provider) checkIssuer(c idClaims) error {
	switch p.Name {
	case ProviderGoogle:
		if c.Issuer == "https://accounts.google.com" || c.Issuer == "accounts.google.com" {
			return nil
		}
		return identityErr("id_token issuer is not Google")
	case ProviderMicrosoft:
		m := microsoftIssuer.FindStringSubmatch(c.Issuer)
		if m == nil {
			return identityErr("id_token issuer is not a Microsoft v2.0 tenant issuer")
		}
		tid := m[1]
		if c.TenantID != "" && !strings.EqualFold(c.TenantID, tid) {
			return identityErr("id_token tid does not match its issuer")
		}
		switch t := strings.ToLower(p.Tenant); {
		case t == "" || t == "common":
		case t == "consumers":
			if tid != msaTenantID {
				return identityErr("this server accepts personal Microsoft accounts only")
			}
		case t == "organizations":
			if tid == msaTenantID {
				return identityErr("this server accepts work or school Microsoft accounts only")
			}
		case tenantGUID.MatchString(t):
			if tid != t {
				return identityErr("this server accepts one Microsoft tenant only")
			}
		default:
			// A verified domain: its tenant id is not knowable offline, so the
			// authority itself (login.microsoftonline.com/<domain>) is what
			// narrowed the sign-in.
		}
		return nil
	}
	return identityErr("unknown provider")
}

func audiences(raw json.RawMessage) ([]string, error) {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil && len(many) > 0 {
		return many, nil
	}
	return nil, identityErr("id_token has no audience")
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// truthy reads email_verified, which Google sends as a boolean (and some
// libraries as the string "true").
func truthy(raw json.RawMessage) bool {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && strings.EqualFold(s, "true")
}

// usableSubject: non-empty, bounded, printable — it is stored and compared.
func usableSubject(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// plausibleAddress: one @, something either side, no spaces or controls,
// bounded. It becomes the XOAUTH2 user and the account's identity address; the
// IMAP sign-in is what proves it.
func plausibleAddress(s string) bool {
	if len(s) < 3 || len(s) > 254 || strings.Count(s, "@") != 1 {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at == 0 || at == len(s)-1 {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || r == '\x01' {
			return false
		}
	}
	return true
}
