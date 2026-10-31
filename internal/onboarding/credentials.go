// SPDX-License-Identifier: AGPL-3.0-only

package onboarding

import (
	"encoding/json"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
)

// CredentialsInfoV1 is the HKDF domain-separation label for the per-user
// account-credentials key (spec §9). It is versioned so a future scheme can
// derive a distinct key without colliding with v1 material.
const CredentialsInfoV1 = "mailroom-credentials-v1"

// CredentialsSchemeV1 is the sentinel stored in app.accounts.credentials_dek.
// The 0.5 credential scheme DERIVES its key (storage-free) rather than wrapping
// a random DEK, so this column records only the scheme version, not a key
// reference. It is a stable, human-recognizable ULID, not a random id.
var CredentialsSchemeV1 = SchemeULID("cred-scheme-v1")

// Credentials is the account secret sealed into app.accounts.credentials_ct.
// The IMAP username lives here (there is no plaintext username column in 0.5),
// so opening the envelope yields everything needed to authenticate. SMTP fields
// default to the IMAP values when the account uses one login for both.
type Credentials struct {
	IMAPUsername string `json:"imap_username"`
	IMAPPassword string `json:"imap_password"`
	SMTPUsername string `json:"smtp_username,omitempty"`
	SMTPPassword string `json:"smtp_password,omitempty"`

	// OAuth is set for an OAuth account (app.accounts.auth_kind 'oauth2',
	// ADR-046) and nil for every password account. For an OAuth account
	// IMAPUsername is the mailbox address (the XOAUTH2 user) and the password
	// fields are empty. omitempty keeps a password account's sealed JSON
	// byte-identical to what it was before this field existed.
	OAuth *OAuthSecret `json:"oauth,omitempty"`
}

// OAuthSecret is the secret half of an OAuth grant: which provider issued it
// and the refresh token. It lives here, inside the sealed envelope, so there is
// still exactly one place a mail-account secret is stored (ADR-046 §2); the
// queryable half of the grant (state, scopes, generation) is app.oauth_grants.
// Access tokens are never stored anywhere.
type OAuthSecret struct {
	Provider     string `json:"provider"`
	RefreshToken string `json:"refresh_token"`
	// Subject is the provider account the secret was issued to (the ID
	// token's `sub`). Not secret; kept here because it must SURVIVE a
	// disconnect, which deletes the grant row and empties RefreshToken: a later
	// reconnect of the same Mailroom account is accepted only for the same
	// subject, so a disconnected account's mail can never be re-attached to a
	// different mailbox (ADR-046 §3).
	Subject string `json:"subject,omitempty"`
}

// DeriveCredentialsKey derives the per-user account-credentials key from the
// master key: HKDF(master, salt=userID, info=CredentialsInfoV1). The caller
// owns the returned key and must crypto.Zero it when done.
func DeriveCredentialsKey(master []byte, userID ulid.ULID) ([]byte, error) {
	key, err := crypto.DeriveKey(master, userID[:], CredentialsInfoV1)
	if err != nil {
		return nil, fmt.Errorf("onboarding: derive credentials key: %w", err)
	}
	return key, nil
}

// SealCredentials envelope-encrypts creds under the per-user credentials key,
// binding the user id as associated data. The result is stored verbatim in
// app.accounts.credentials_ct.
func SealCredentials(master []byte, userID ulid.ULID, creds Credentials) ([]byte, error) {
	key, err := DeriveCredentialsKey(master, userID)
	if err != nil {
		return nil, err
	}
	defer crypto.Zero(key)

	pt, err := json.Marshal(creds)
	if err != nil {
		return nil, fmt.Errorf("onboarding: marshal credentials: %w", err)
	}
	defer crypto.Zero(pt)

	ct, err := crypto.Seal(key, nil, pt, userID[:])
	if err != nil {
		return nil, fmt.Errorf("onboarding: seal credentials: %w", err)
	}
	return ct, nil
}

// OpenCredentials re-derives the per-user credentials key and opens ct,
// recovering the account secret. A wrong user id (salt/AAD) or tampered
// ciphertext fails authentication and yields no plaintext.
func OpenCredentials(master []byte, userID ulid.ULID, ct []byte) (Credentials, error) {
	key, err := DeriveCredentialsKey(master, userID)
	if err != nil {
		return Credentials{}, err
	}
	defer crypto.Zero(key)

	pt, err := crypto.Open(key, ct, userID[:])
	if err != nil {
		return Credentials{}, fmt.Errorf("onboarding: open credentials: %w", err)
	}
	defer crypto.Zero(pt)

	var creds Credentials
	if err := json.Unmarshal(pt, &creds); err != nil {
		return Credentials{}, fmt.Errorf("onboarding: unmarshal credentials: %w", err)
	}
	return creds, nil
}

// SchemeULID builds a stable sentinel ULID from a short ASCII label, recording
// a scheme version in a *_dek column without persisting a random key. Any
// 16-byte value renders to a valid Crockford-base32 ULID string, so it passes
// the app.ulid domain check. It is shared with internal/syncrunner, which
// needs an analogous sentinel for the derived mail-blob DEK.
func SchemeULID(label string) ulid.ULID {
	var u ulid.ULID
	copy(u[:], label)
	return u
}
