// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"mailroom/internal/xoauth2"
)

// Provider-side revocation of a session's refresh token (RFC 7009, ADR-048
// section 5), so signing a device out ends its grant at the provider as well as
// here.
//
// It is BEST EFFORT and never gates a sign-out: the session row is revoked first,
// so the cookie is dead whatever this returns, and the caller only logs an error.
// It reuses renewal's client (no redirects, an owned transport) and its
// client-authentication choice (tokenEndpointRequest), so the two cannot differ.
//
// The refresh token is a bearer credential: it travels as an xoauth2.Secret, goes
// only into the request body, and no error built here contains it. The provider's
// answer is reduced to a status and a validated OAuth error code.

// revokeTimeout bounds the call, like renewal's: a stalled provider delays a
// sign-out by at most this long.
var revokeTimeout = 5 * time.Second

// RevokeRefreshToken asks the provider to revoke token. With no
// revocation_endpoint in discovery it does nothing and returns nil. RFC 7009
// answers 200 even for a token that is unknown or already revoked, so any 2xx is
// success.
func (a *Authenticator) RevokeRefreshToken(ctx context.Context, token xoauth2.Secret) error {
	if a.revocationEndpoint == "" || token.Empty() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()

	secrets := []xoauth2.Secret{token, xoauth2.Secret(a.oauth2Config.ClientSecret)}
	req, err := a.tokenEndpointRequest(ctx, a.revocationEndpoint, url.Values{
		"token":           {token.Reveal()},
		"token_type_hint": {"refresh_token"},
	})
	if err != nil {
		return errors.New("auth: token revocation: the revocation endpoint is not a usable URL")
	}
	resp, err := renewHTTPClient.Do(req)
	if err != nil {
		// A *url.Error names the method and URL; scrubbed anyway.
		return fmt.Errorf("auth: token revocation: request failed: %s", xoauth2.CleanDescription(err.Error(), secrets...))
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var parsed struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	code := xoauth2.OAuthErrorCode(parsed.Error)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// Not followed: it would carry the token and client secret elsewhere.
		code = "redirect_not_followed"
	}
	return fmt.Errorf("auth: token revocation: the provider answered %d %s", resp.StatusCode, code)
}
