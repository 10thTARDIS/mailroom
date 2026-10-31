// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mailroom/internal/xoauth2"
)

// FailureClass is what a failed refresh means, and therefore who must act
// (ADR-046 §5). It is the core of honest re-authentication: telling every user
// to reconnect when the operator's client secret expired would be a lie told
// to everyone at once.
type FailureClass int

const (
	// FailureTransient: network, timeout, provider 5xx or 429, or an answer we
	// could not read. Retry with the existing backoff; the grant is untouched.
	FailureTransient FailureClass = iota + 1
	// FailureNeedsReauth: the provider said invalid_grant. The refresh token is
	// dead (revoked, expired, idle six months, password changed, evicted by the
	// 100-token limit). Only the user reconnecting fixes it.
	FailureNeedsReauth
	// FailureOperator: the provider refused this SERVER — invalid_client or
	// unauthorized_client (a wrong, expired or deleted client registration), a
	// request it calls malformed, or a provider that is no longer configured.
	// Only the operator fixes it; users are told to contact them.
	FailureOperator
)

// String implements fmt.Stringer.
func (c FailureClass) String() string {
	switch c {
	case FailureTransient:
		return "transient"
	case FailureNeedsReauth:
		return "needs_reauth"
	case FailureOperator:
		return "operator_error"
	default:
		return "unknown"
	}
}

// RefreshError is a failed refresh. Its text carries the provider name, the
// class, the OAuth error code and a scrubbed, capped error_description —
// never a token, never the raw response body.
type RefreshError struct {
	Provider string
	Class    FailureClass
	// Code is the OAuth error code ("invalid_grant"), or one of ours for
	// failures with none ("network", "http_503", "malformed_response",
	// "provider_not_configured").
	Code string
	// Description is the provider's error_description, with every secret the
	// request carried scrubbed out, control characters removed, and capped.
	Description string
	// Err is the underlying transport error, if any.
	Err error
}

// Error implements error.
func (e *RefreshError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "oauthmail: %s token refresh failed (%s): %s", e.Provider, e.Class, e.Code)
	if e.Description != "" {
		b.WriteString(": ")
		b.WriteString(e.Description)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the transport error.
func (e *RefreshError) Unwrap() error { return e.Err }

// Is maps the class onto the protocol-neutral sentinels in internal/xoauth2,
// which internal/sync and compose classify without importing this package.
func (e *RefreshError) Is(target error) bool {
	switch e.Class {
	case FailureNeedsReauth:
		return target == xoauth2.ErrReauthRequired
	case FailureOperator:
		return target == xoauth2.ErrProviderMisconfigured
	case FailureTransient:
		return target == xoauth2.ErrTokenUnavailable
	}
	return false
}

// TokenResponse is a successful refresh.
type TokenResponse struct {
	AccessToken xoauth2.Secret
	// RefreshToken is set when the provider ROTATED the refresh token
	// (Microsoft does on every use); empty when it did not (Google).
	RefreshToken xoauth2.Secret
	// IDToken is the OpenID Connect ID token, present on a code exchange that
	// asked for `openid` (the connect flow reads its claims); usually absent
	// on a refresh.
	IDToken xoauth2.Secret
	// ExpiresIn is the access token's lifetime as the provider stated it, or
	// defaultAccessLifetime when it did not say.
	ExpiresIn time.Duration
	// Scopes is the granted scope list when the provider returned one.
	Scopes []string
}

const (
	// maxTokenResponse caps how much of a token-endpoint answer is read.
	maxTokenResponse = 1 << 20
	// defaultAccessLifetime is assumed when a provider omits expires_in. Both
	// providers state about an hour; assuming less only refreshes earlier.
	defaultAccessLifetime = 30 * time.Minute
)

// Refresh exchanges refresh for a new access token at p's token endpoint.
// hc nil means a private client with a timeout; the caller also bounds the
// call through ctx. Whatever client is given, redirects are NEVER followed (a
// token endpoint that redirects would carry the refresh token and client
// secret somewhere else on the next hop): a 3xx is answered as transient.
//
// The token endpoint is the registry's, never anything user-supplied, so this
// request does not go through the account egress guard: there is no
// user-controlled host to rebind.
func (p Provider) Refresh(ctx context.Context, hc *http.Client, refresh xoauth2.Secret) (TokenResponse, error) {
	if refresh.Empty() {
		return TokenResponse{}, &RefreshError{Provider: p.Name, Class: FailureNeedsReauth,
			Code: "no_refresh_token"}
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh.Reveal()},
		"client_id":     {p.Client.ID},
		"client_secret": {p.Client.Secret.Reveal()},
	}
	if p.RefreshSendsScope && len(p.Scopes) > 0 {
		form.Set("scope", strings.Join(p.Scopes, " "))
	}
	return p.tokenCall(ctx, hc, form, refresh)
}

// tokenCall POSTs form to p's token endpoint and reads the answer: the ONE
// request path for both the refresh (Refresh) and the connect flow's code
// exchange (Exchange), so both get the same no-redirect client, the same
// scrubbing of every secret the request carried (secrets, plus the client
// secret), the same size cap and the same failure classification.
func (p Provider) tokenCall(ctx context.Context, hc *http.Client, form url.Values, secrets ...xoauth2.Secret) (TokenResponse, error) {
	hc = noRedirects(hc)
	secrets = append(secrets, p.Client.Secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, &RefreshError{Provider: p.Name, Class: FailureOperator, Code: "bad_token_url"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		// A *url.Error names the method and the token URL (no secret: the
		// secrets are in the body) and the cause. Scrubbed anyway.
		return TokenResponse{}, &RefreshError{Provider: p.Name, Class: FailureTransient, Code: "network",
			Err: scrubbed(err, secrets...)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse))
	if err != nil {
		return TokenResponse{}, &RefreshError{Provider: p.Name, Class: FailureTransient, Code: "network",
			Err: scrubbed(err, secrets...)}
	}

	var parsed struct {
		AccessToken      string          `json:"access_token"`
		RefreshToken     string          `json:"refresh_token"`
		IDToken          string          `json:"id_token"`
		ExpiresIn        json.RawMessage `json:"expires_in"`
		Scope            string          `json:"scope"`
		Error            string          `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	jsonErr := json.Unmarshal(body, &parsed)

	if resp.StatusCode == http.StatusOK && jsonErr == nil && parsed.Error == "" {
		if parsed.AccessToken == "" {
			return TokenResponse{}, &RefreshError{Provider: p.Name, Class: FailureTransient, Code: "malformed_response"}
		}
		out := TokenResponse{
			AccessToken:  xoauth2.Secret(parsed.AccessToken),
			RefreshToken: xoauth2.Secret(parsed.RefreshToken),
			IDToken:      xoauth2.Secret(parsed.IDToken),
			ExpiresIn:    parseExpiresIn(parsed.ExpiresIn),
		}
		if parsed.Scope != "" {
			out.Scopes = strings.Fields(parsed.Scope)
		}
		return out, nil
	}

	rerr := &RefreshError{Provider: p.Name, Code: oauthCode(parsed.Error),
		Description: cleanDescription(parsed.ErrorDescription, secrets...)}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Not followed (noRedirects). Not the provider's verdict on the grant.
		rerr.Class = FailureTransient
		rerr.Code = "http_3xx"
		rerr.Description = ""
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		rerr.Class = FailureTransient
		if rerr.Code == "" {
			rerr.Code = fmt.Sprintf("http_%d", resp.StatusCode)
		}
	case jsonErr != nil || parsed.Error == "":
		// A 4xx (or a 200) we cannot read as an OAuth answer: most likely an
		// intermediary, not the provider's verdict on the grant. Do not change
		// the grant's state on it; back off and retry.
		rerr.Class = FailureTransient
		rerr.Code = fmt.Sprintf("http_%d", resp.StatusCode)
		rerr.Description = ""
	default:
		rerr.Class = classifyOAuthCode(rerr.Code)
	}
	return TokenResponse{}, rerr
}

// scrubbedError presents a transport error with every request secret removed
// from its text, while still unwrapping to the cause so errors.Is can see a
// deadline or a cancellation.
type scrubbedError struct {
	msg   string
	cause error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.cause }

func scrubbed(err error, secrets ...xoauth2.Secret) error {
	return &scrubbedError{msg: xoauth2.Scrub(err.Error(), secrets...), cause: err}
}

// defaultTokenClient is the private client used when the caller gives none:
// never http.DefaultClient, which any package can reconfigure — and never its
// transport either (security review M2 of #897). A nil Transport would mean
// http.DefaultTransport, which any imported package may replace or mutate
// (its TLS config, its proxy, its dialer); the connect flow skips the ID
// token's signature on the strength of verified TLS to the provider's token
// endpoint (idtoken.go), so that TLS must be a property of THIS code. The
// transport is therefore an owned clone taken at init, with TLS 1.2 as the
// floor and the proxy explicitly from the environment. Redirects are refused
// on top (noRedirects).
var defaultTokenClient = &http.Client{Timeout: 30 * time.Second, Transport: ownedTransport()}

func ownedTransport() *http.Transport {
	var t *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t = base.Clone()
	} else {
		t = &http.Transport{}
	}
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.MinVersion = tls.VersionTLS12
	t.Proxy = http.ProxyFromEnvironment
	return t
}

// noRedirects returns a copy of hc (or the private default) that refuses to
// follow redirects.
func noRedirects(hc *http.Client) *http.Client {
	if hc == nil {
		hc = defaultTokenClient
	}
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// classifyOAuthCode maps an RFC 6749 §5.2 error code to who must act.
func classifyOAuthCode(code string) FailureClass {
	switch code {
	case "invalid_grant",
		// OIDC's "the user must come back and sign in / consent" codes
		// (Microsoft returns them on refresh after a policy change). Only the
		// user can act on them.
		"interaction_required", "login_required", "consent_required":
		return FailureNeedsReauth
	case "temporarily_unavailable", "server_error":
		return FailureTransient
	default:
		// invalid_client, unauthorized_client, invalid_request,
		// unsupported_grant_type, invalid_scope, and anything unknown: the
		// request THIS SERVER built or the client it presented was refused.
		// No user action can fix that, so it must not read as "reconnect".
		return FailureOperator
	}
}

// oauthCode and cleanDescription are shared with the sign-in session renewal
// (internal/auth, ADR-048) and so live in internal/xoauth2. An error code is
// kept only if it looks like one, so a hostile body cannot put arbitrary text
// into state_reason or the audit log.
func oauthCode(s string) string { return xoauth2.OAuthErrorCode(s) }

func cleanDescription(s string, secrets ...xoauth2.Secret) string {
	return xoauth2.CleanDescription(s, secrets...)
}

// Bounds for a stated access-token lifetime. A provider (or anything in
// between) claiming less than minAccessLifetime would make every connection
// refresh; more than maxAccessLifetime would keep a token cached long after a
// real one would have died. Clamping before the multiplication also rules out
// overflow (10^12 seconds * time.Second does not fit an int64).
const (
	minAccessLifetime = 30 * time.Second
	maxAccessLifetime = 24 * time.Hour
)

// parseExpiresIn accepts a number or a numeric string (both occur in the
// wild), clamped to [minAccessLifetime, maxAccessLifetime]; missing or
// unreadable means defaultAccessLifetime.
func parseExpiresIn(raw json.RawMessage) time.Duration {
	if len(raw) == 0 {
		return defaultAccessLifetime
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return defaultAccessLifetime
		}
		n = json.Number(s)
	}
	secs, err := n.Int64()
	if err != nil {
		f, ferr := n.Float64()
		if ferr != nil {
			return defaultAccessLifetime
		}
		switch {
		case f >= maxAccessLifetime.Seconds():
			return maxAccessLifetime
		case f <= minAccessLifetime.Seconds():
			return minAccessLifetime
		}
		secs = int64(f)
	}
	switch {
	case secs >= int64(maxAccessLifetime/time.Second):
		return maxAccessLifetime
	case secs <= int64(minAccessLifetime/time.Second):
		return minAccessLifetime
	}
	return time.Duration(secs) * time.Second
}
