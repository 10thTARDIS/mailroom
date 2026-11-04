// SPDX-License-Identifier: AGPL-3.0-only

package oauthserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// oauthError is an RFC 6749 section 5.2 error.
type oauthError struct {
	status int
	code   string
	desc   string
}

func (e *oauthError) Error() string { return e.code + ": " + e.desc }

func badGrant(desc string) *oauthError {
	return &oauthError{http.StatusBadRequest, "invalid_grant", desc}
}
func badRequest(desc string) *oauthError {
	return &oauthError{http.StatusBadRequest, "invalid_request", desc}
}
func serverFault() *oauthError {
	return &oauthError{http.StatusServiceUnavailable, "server_error", "the server could not complete the request"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	// RFC 6749 section 5.1: a token response must never be cached.
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeOAuthError(w http.ResponseWriter, e *oauthError) {
	writeJSON(w, e.status, map[string]string{"error": e.code, "error_description": e.desc})
}

// formBody reads a small form-encoded POST body.
func formBody(w http.ResponseWriter, r *http.Request) *oauthError {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		return badRequest("the body must be application/x-www-form-urlencoded")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		return badRequest("the form could not be read")
	}
	// Parameters travel in the body only; a token in a URL would be logged.
	r.URL.RawQuery = ""
	return nil
}

// failed charges a failed attempt to the per-address limiter, which is
// consulted ONLY here, on the failure path (ADR-042 section 2): a client that
// succeeds is never refused because strangers failed from the same address.
func (s *Server) failed(w http.ResponseWriter, r *http.Request, e *oauthError) {
	addr := clientIP(r)
	if blocked, retry := s.limiter.Blocked(addr); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "slow_down",
			"error_description": "too many failed attempts"})
		return
	}
	s.limiter.Fail(addr)
	writeOAuthError(w, e)
}

// Token is POST /oauth/token for public clients (no client authentication; PKCE
// carries the proof).
func (s *Server) Token() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := formBody(w, r); e != nil {
			s.failed(w, r, e)
			return
		}
		var (
			resp map[string]any
			e    *oauthError
		)
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			resp, e = s.codeGrant(r)
		case "refresh_token":
			resp, e = s.refreshGrant(r)
		case "":
			e = badRequest("grant_type is required")
		default:
			e = &oauthError{http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token"}
		}
		if e != nil {
			s.failed(w, r, e)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// validVerifier: RFC 7636 section 4.1, 43 to 128 unreserved characters.
func validVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, c := range v {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("-._~", c) {
			return false
		}
	}
	return true
}

func (s *Server) codeGrant(r *http.Request) (map[string]any, *oauthError) {
	ctx := r.Context()
	f := r.PostForm
	code, redirectURI, clientID, verifier := f.Get("code"), f.Get("redirect_uri"), f.Get("client_id"), f.Get("code_verifier")
	switch {
	case code == "":
		return nil, badRequest("code is required")
	case verifier == "":
		return nil, badRequest("code_verifier is required (PKCE)")
	case clientID == "" || redirectURI == "":
		return nil, badRequest("client_id and redirect_uri are required")
	}
	if res := f.Get("resource"); res != "" && !s.resourceMatches(res) {
		return nil, &oauthError{http.StatusBadRequest, "invalid_target", "the resource must be " + s.resource}
	}
	c, found, err := s.st.ConsumeOAuthCode(ctx, hashOf(code))
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: consume code", "err", err)
		return nil, serverFault()
	}
	if !found {
		return nil, badGrant("the code is not valid")
	}
	if c.Replay {
		// OAuth 2.1 4.1.3: a code presented twice revokes what it produced.
		s.revokeFamilyAnomaly(ctx, c.UserID, c.APIKeyID, c.ID, store.AnomalyCodeReplay, "oauth.code_replay", clientIP(r))
		return nil, badGrant("the code was already used")
	}
	switch {
	case !c.ExpiresAt.After(time.Now()):
		return nil, badGrant("the code has expired")
	case !c.UserOK:
		return nil, badGrant("the code is not valid")
	case c.ClientID != clientID:
		return nil, badGrant("the code was issued to a different client")
	case c.RedirectURI != redirectURI:
		return nil, badGrant("the redirect_uri does not match the authorization request")
	case !validVerifier(verifier):
		return nil, badGrant("the code_verifier is not valid")
	case subtle.ConstantTimeCompare([]byte(s256(verifier)), []byte(c.CodeChallenge)) != 1:
		return nil, badGrant("the code_verifier does not match")
	}
	var out map[string]any
	var gerr *oauthError
	err = s.st.UserTx(ctx, c.UserID, func(tx *store.UserTx) error {
		k, ok, err := tx.GetAPIKey(ctx, c.APIKeyID)
		if err != nil {
			return err
		}
		if !ok || !k.Active() || k.Kind != store.KeyKindOAuth || k.OAuthClientID != clientID {
			gerr = badGrant("the grant is no longer active")
			return nil
		}
		out, err = s.issue(ctx, tx, k, c.ID, c.Resource, k.Scopes)
		return err
	})
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: issue tokens", "err", err)
		return nil, serverFault()
	}
	return out, gerr
}

// issue mints an access and a refresh token for the grant in the family.
func (s *Server) issue(ctx context.Context, tx *store.UserTx, k store.APIKey, family ulid.ULID, resource string, scopes []string) (map[string]any, error) {
	access, accessHash, err := auth.NewOAuthToken(auth.OAuthAccessPrefix)
	if err != nil {
		return nil, err
	}
	refresh, refreshHash, err := auth.NewOAuthToken(auth.OAuthRefreshPrefix)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if err := tx.InsertOAuthTokens(ctx,
		store.NewOAuthToken{Kind: store.OAuthTokenAccess, Hash: accessHash, APIKeyID: k.ID, FamilyID: family,
			Resource: resource, Scopes: scopes, ExpiresAt: now.Add(AccessTokenTTL)},
		store.NewOAuthToken{Kind: store.OAuthTokenRefresh, Hash: refreshHash, APIKeyID: k.ID, FamilyID: family,
			Resource: resource, Scopes: scopes, ExpiresAt: now.Add(RefreshTokenTTL)}); err != nil {
		return nil, err
	}
	return map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(AccessTokenTTL.Seconds()),
		"refresh_token": refresh, "scope": strings.Join(scopes, " ")}, nil
}

func (s *Server) refreshGrant(r *http.Request) (map[string]any, *oauthError) {
	ctx := r.Context()
	f := r.PostForm
	token, clientID := f.Get("refresh_token"), f.Get("client_id")
	if token == "" {
		return nil, badRequest("refresh_token is required")
	}
	// A public client names itself on every refresh (OAuth 2.1 4.3), and it must
	// be the grant's client.
	if clientID == "" {
		return nil, badRequest("client_id is required")
	}
	if res := f.Get("resource"); res != "" && !s.resourceMatches(res) {
		return nil, &oauthError{http.StatusBadRequest, "invalid_target", "the resource must be " + s.resource}
	}
	// Only a well-formed refresh token reaches the database.
	if !auth.WellFormedOAuthToken(token, auth.OAuthRefreshPrefix) {
		return nil, badGrant("the refresh token is not valid")
	}
	rt, found, err := s.st.ConsumeOAuthRefresh(ctx, hashOf(token))
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: consume refresh token", "err", err)
		return nil, serverFault()
	}
	switch {
	case !found || rt.Revoked:
		return nil, badGrant("the refresh token is not valid")
	case rt.Reuse:
		// Rotation: a token already used is presented again. Whoever holds the
		// other copy, both sessions end; the grant stays (ADR-050 section 3).
		s.revokeFamilyAnomaly(ctx, rt.UserID, rt.APIKeyID, rt.FamilyID, store.AnomalyRefreshReuse, "oauth.refresh_reuse", clientIP(r))
		return nil, badGrant("the refresh token was already used")
	case !rt.ExpiresAt.After(time.Now()) || !rt.UserOK:
		return nil, badGrant("the refresh token is not valid")
	}
	var out map[string]any
	var gerr *oauthError
	err = s.st.UserTx(ctx, rt.UserID, func(tx *store.UserTx) error {
		k, ok, err := tx.GetAPIKey(ctx, rt.APIKeyID)
		if err != nil {
			return err
		}
		if !ok || !k.Active() || k.Kind != store.KeyKindOAuth || k.OAuthClientID != clientID {
			gerr = badGrant("the grant is no longer active")
			return nil
		}
		// The scope may never be widened: naming a scope the grant does not hold
		// is refused. A narrower request is ignored, and the response says so:
		// the gate enforces the grant's own current scopes whatever a token was
		// asked for, so the response reports exactly those (RFC 6749 5.1 lets
		// the server issue a different scope as long as it says which).
		for _, x := range strings.Fields(f.Get("scope")) {
			if !slices.Contains(k.Scopes, x) {
				gerr = &oauthError{http.StatusBadRequest, "invalid_scope", "a refresh cannot widen the scope"}
				return nil
			}
		}
		scopes := k.Scopes
		out, err = s.issue(ctx, tx, k, rt.FamilyID, rt.Resource, scopes)
		return err
	})
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: refresh", "err", err)
		return nil, serverFault()
	}
	return out, gerr
}

// revokeFamilyAnomaly ends a family (every access and refresh token from one
// authorization), records the anomaly in api_key_events and the audit log, and
// pushes the alert for a reused refresh token. The grant itself stays.
func (s *Server) revokeFamilyAnomaly(ctx context.Context, userID, keyID, family ulid.ULID, anomaly, action, ip string) {
	ctx = context.WithoutCancel(ctx)
	var key store.APIKey
	err := s.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		n, err := tx.RevokeOAuthFamily(ctx, family)
		if err != nil {
			return err
		}
		k, ok, err := tx.GetAPIKey(ctx, keyID)
		if err != nil || !ok {
			return err
		}
		key = k
		if _, err := tx.InsertAPIKeyEvent(ctx, store.APIKeyEvent{KeyID: keyID, Method: "oauth/token",
			Outcome: store.KeyOutcomeAnomaly, Anomaly: anomaly, ClientIP: ip,
			Summary: map[string]any{"familyId": family.String(), "tokensRevoked": n}}); err != nil {
			return err
		}
		return tx.AuditAPIKeySystem(ctx, action, keyID, map[string]any{"familyId": family.String(), "tokensRevoked": n})
	})
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: could not record a family revocation", "anomaly", anomaly, "err", err)
		return
	}
	if anomaly == store.AnomalyRefreshReuse && s.alert != nil && key.ID != (ulid.ULID{}) {
		s.alert(userID, key)
	}
}
