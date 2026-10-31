// SPDX-License-Identifier: AGPL-3.0-only

package auth

// Silent session renewal and identity refresh (#937, ADR-048 sections 4 and 4a).
//
// A session whose last provider confirmation is older than Config.RenewInterval
// is renewed BEFORE the request is served: a server-to-server refresh_token
// grant, then the same identity path as a login (ID token verification, the
// UserInfo backfill, ResolveUser). Three things keep that safe:
//
//   - Serialised, not raced. Authentik treats a refresh token as single-use, so
//     two renewals of one session must never both present it. In this process a
//     singleflight per session id collapses concurrent requests into one
//     attempt; across processes SessionStore.Renew holds a per-session advisory
//     lock and re-reads the row, so the loser of a race uses the winner's
//     result and makes no call.
//   - Bounded. The whole attempt (token call plus UserInfo call plus
//     ResolveUser) shares Config.RenewTimeout, and runs detached from any one
//     request's context so a client that disconnects does not fail the others.
//   - Honest about failure. Only the provider (or the resolver) REFUSING the
//     user ends the session. A provider that refuses this server, or cannot be
//     reached, leaves the session as it is and serves it within the outage
//     allowance; a renewal that could not re-check identity never counts as one
//     that did.
//
// Nothing here logs or wraps a token: the refresh token, the new ID token and
// the access token travel as xoauth2.Secret, the provider's error_description is
// scrubbed and capped, and errors from go-oidc (which can quote a token) are
// replaced by fixed messages.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"mailroom/internal/xoauth2"
)

// maxTokenResponse caps how much of a token-endpoint answer is read.
const maxTokenResponse = 1 << 20

// renewKind is how one renewal attempt ended, from the request's point of view.
type renewKind int

const (
	// renewFailed: nothing was confirmed and nothing was refused (a provider
	// outage, a server-side refusal, no refresh token, a back-off in force).
	// The request is judged by the outage allowance.
	renewFailed renewKind = iota
	// renewConfirmed: the session is confirmed as of lastChecked.
	renewConfirmed
	// renewRefused: the provider or the resolver refused the user, or the
	// session is revoked. The request is unauthenticated.
	renewRefused
)

type renewOutcome struct {
	kind        renewKind
	lastChecked time.Time
}

// renewState is the in-process half of renewal. Its zero value is ready.
type renewState struct {
	flight singleflight.Group

	semOnce sync.Once
	sem     chan struct{}

	mu        sync.Mutex
	failed    map[ulid.ULID]time.Time // session -> when the last attempt failed
	lastPrune time.Time
}

// tryAcquire takes one of n renewal slots without waiting. It returns the
// slot's release function, or nil when every slot is taken. Pairing the release
// with the acquire this way means no path can release a slot it never took
// (which would block for ever and wedge the session's singleflight key).
// n below 1 is treated as 1: an unbuffered channel would never admit anyone,
// and renewal would silently stop.
func (s *renewState) tryAcquire(n int) (release func()) {
	s.semOnce.Do(func() { s.sem = make(chan struct{}, max(n, 1)) })
	select {
	case s.sem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.sem }) }
	default:
		return nil
	}
}

// backingOff reports whether a failed attempt for id is recent enough that no
// new one may start.
func (s *renewState) backingOff(id ulid.ULID, now time.Time, spacing time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.failed[id]
	return ok && now.Sub(at) < spacing
}

// noteFailure records a failed attempt and prunes entries older than spacing, at
// most once per spacing, so the map is bounded by the sessions that failed in
// the last couple of spacings.
func (s *renewState) noteFailure(id ulid.ULID, now time.Time, spacing time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed == nil {
		s.failed = map[ulid.ULID]time.Time{}
	}
	s.failed[id] = now
	if now.Sub(s.lastPrune) >= spacing {
		for k, at := range s.failed {
			if now.Sub(at) >= spacing {
				delete(s.failed, k)
			}
		}
		s.lastPrune = now
	}
}

func (s *renewState) clearFailure(id ulid.ULID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failed, id)
}

// renew renews one stale session, or reports why it could not. Concurrent calls
// for one session share a single attempt.
func (a *Authenticator) renew(ctx context.Context, ref SessionRef) renewOutcome {
	if a.renewal.backingOff(ref.ID, a.clock(), a.cfg.RenewRetrySpacing) {
		return renewOutcome{kind: renewFailed}
	}
	ch := a.renewal.flight.DoChan(ref.ID.String(), func() (any, error) {
		// Detached: the attempt belongs to every request waiting on it, not to
		// whichever one happened to start it.
		return a.renewOnce(context.WithoutCancel(ctx), ref), nil
	})
	// A ceiling on the wait, independent of the request: every phase is bounded
	// (A by 2*RenewTimeout+renewWriteTimeout, B by RenewTimeout, C by
	// renewWriteTimeout), so an attempt that has not answered by then is stuck,
	// and this request is served within the allowance instead of waiting on it.
	ceiling := time.NewTimer(3*a.cfg.RenewTimeout + 2*renewWriteTimeout)
	defer ceiling.Stop()
	select {
	case res := <-ch:
		return res.Val.(renewOutcome)
	case <-ctx.Done():
		// This client left; the attempt carries on for the others.
		return renewOutcome{kind: renewFailed}
	case <-ceiling.C:
		a.logger().ErrorContext(ctx, "session renewal did not finish within its ceiling; serving within the outage allowance",
			"session", ref.ID.String())
		return renewOutcome{kind: renewFailed}
	}
}

// workClass is how the provider work (phase A) ended.
type workClass int

const (
	// workFailed: nothing was consumed and nothing is written. The zero value,
	// so a callback that never ran reads as a failure.
	workFailed workClass = iota
	// workRotated: the grant succeeded and the rotation is in the transaction.
	workRotated
	// workRefused: the provider (or the ID token's subject) refused the user; the
	// store revokes the session in the same transaction.
	workRefused
)

// renewWriteTimeout is reserved, out of the lock transaction's context, for the
// final write and commit that follow the provider's answer, so the provider
// budget expiring cannot land between the grant consuming a single-use token and
// the rotation being stored. It also bounds phase C and a revocation.
const renewWriteTimeout = 3 * time.Second

// grantOutcome is what phase A leaves for phases B and C.
type grantOutcome struct {
	class    workClass
	subject  string         // the session user's stored subject
	access   xoauth2.Secret // used only for UserInfo, then dropped
	verified bool           // the refresh response carried an ID token that verified
	// badIDToken: the refresh response carried an ID token that did NOT verify
	// (a JWKS refetch failing during key rotation, say). The rotation is still
	// stored, but the renewal is transient: falling back to UserInfo alone
	// could demote an admin whose groups only the ID token carries.
	badIDToken bool
	claims     idTokenClaims // the verified ID token's claims
}

func (a *Authenticator) renewOnce(ctx context.Context, ref SessionRef) renewOutcome {
	// Bound the renewals in flight: each holds a pooled database connection
	// across the provider call. An overflow is NOT a failed attempt (no retry
	// spacing), so the session's next request tries again.
	release := a.renewal.tryAcquire(a.cfg.RenewConcurrency)
	if release == nil {
		return renewOutcome{kind: renewFailed}
	}
	defer release()

	now := a.clock()
	fail := func() renewOutcome {
		a.renewal.noteFailure(ref.ID, now, a.cfg.RenewRetrySpacing)
		return renewOutcome{kind: renewFailed}
	}
	log := a.logger().With("session", ref.ID.String())

	// Phase A, under the per-session lock: the provider grant, and the durable
	// rotation of the token it consumed. The transaction's context outlives the
	// provider budget by renewWriteTimeout (see renewGrant).
	actx, cancel := context.WithTimeout(ctx, 2*a.cfg.RenewTimeout+renewWriteTimeout)
	defer cancel()
	var ph grantOutcome
	res, err := a.cfg.SessionStore.Renew(actx, ref, now.Add(-a.cfg.RenewInterval),
		func(ctx context.Context, cur RenewCurrent) RenewDecision {
			var d RenewDecision
			d, ph = a.renewGrant(ctx, ref, cur)
			return d
		})
	if err != nil {
		// A store failure says nothing about the session: serve within the
		// allowance, never 401, never revoke.
		log.ErrorContext(ctx, "session renewal could not use the store", "err", err)
		if ph.class == workRotated {
			log.ErrorContext(ctx, "session renewal: the provider rotated the refresh token but storing it failed; the next renewal may be refused")
		}
		return fail()
	}
	switch {
	case res.Revoked:
		return renewOutcome{kind: renewRefused}
	case !res.Ran:
		// Another process renewed while this one waited: its result stands.
		a.renewal.clearFailure(ref.ID)
		return renewOutcome{kind: renewConfirmed, lastChecked: orNow(res.LastCheckedAt, now)}
	case ph.class != workRotated:
		return fail()
	case ph.badIDToken:
		// The rotation is stored (phase A); identity is not re-checked on an
		// ID token that would not verify.
		return fail()
	}

	// Phase B, outside any transaction: who is the user now?
	bctx, bcancel := context.WithTimeout(ctx, a.cfg.RenewTimeout)
	defer bcancel()
	switch _, verdict := a.renewIdentity(bctx, log, ref, ph); verdict {
	case identTransient:
		return fail()
	case identRefused:
		a.revokeRefused(ctx, log, ref)
		return renewOutcome{kind: renewRefused}
	}

	// Phase C: confirm, only if no other renewal has moved the generation on.
	cctx, ccancel := context.WithTimeout(ctx, renewWriteTimeout)
	defer ccancel()
	at, _, revoked, err := a.cfg.SessionStore.Confirm(cctx, ref, res.Generation)
	if err != nil {
		log.ErrorContext(ctx, "session renewal could not be confirmed in the store", "err", err)
		return fail()
	}
	if revoked {
		// Signed out (or refused elsewhere) while this renewal ran: the
		// revocation wins over the confirmation it raced.
		return renewOutcome{kind: renewRefused}
	}
	// ok == false means another process renewed since: that renewal did the
	// confirming, and this request counts as confirmed too.
	a.renewal.clearFailure(ref.ID)
	return renewOutcome{kind: renewConfirmed, lastChecked: orNow(at, now)}
}

func orNow(t, now time.Time) time.Time {
	if t.IsZero() {
		return now
	}
	return t
}

// revokeRefused ends the session as refused, on a context of its own so a
// request that has given up does not leave a refused session alive.
func (a *Authenticator) revokeRefused(ctx context.Context, log *slog.Logger, ref SessionRef) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renewWriteTimeout)
	defer cancel()
	if err := a.cfg.SessionStore.Revoke(rctx, ref, revokeRefused); err != nil {
		log.ErrorContext(ctx, "revoking a refused session failed", "err", err)
	}
}

// renewGrant is phase A's provider work, run inside the store's lock. It makes
// the one refresh_token grant and, on success, decides the rotation to store:
// the new refresh token (the old one is now dead), and the new ID token only if
// it verified.
func (a *Authenticator) renewGrant(ctx context.Context, ref SessionRef, cur RenewCurrent) (RenewDecision, grantOutcome) {
	log := a.logger().With("session", ref.ID.String())
	failed := grantOutcome{class: workFailed}
	if cur.RefreshToken.Empty() {
		// No refresh token (no offline_access): the session lives to the
		// allowance, exactly as before renewal existed. Said once at sign-in,
		// not here.
		return RenewDecision{Kind: RenewLeave}, failed
	}
	// The provider call and the ID token's JWKS fetch share RenewTimeout, but
	// never run into the time reserved for the write that follows.
	budget := a.cfg.RenewTimeout
	if dl, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(dl)-renewWriteTimeout)
	}
	if budget <= 0 {
		log.WarnContext(ctx, "session renewal: no time left for the provider call; serving within the outage allowance")
		return RenewDecision{Kind: RenewLeave}, failed
	}
	pctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	grant, gerr := a.refreshGrant(pctx, cur.RefreshToken)
	if gerr != nil {
		switch gerr.class {
		case grantRefused:
			log.InfoContext(ctx, "session renewal refused by the provider", "code", gerr.code, "description", gerr.description)
			return RenewDecision{Kind: RenewRefuse}, grantOutcome{class: workRefused}
		case grantServerRefused:
			log.ErrorContext(ctx, "session renewal: the provider refused this server's credentials; sessions are served within the outage allowance",
				"code", gerr.code, "description", gerr.description)
		default:
			log.WarnContext(ctx, "session renewal failed; serving within the outage allowance",
				"code", gerr.code, "description", gerr.description)
		}
		return RenewDecision{Kind: RenewLeave}, failed
	}

	// From here the provider has consumed the old refresh token (Authentik's are
	// single-use): whatever else goes wrong, the rotation is stored. A provider
	// that did not rotate sends none, and the stored token is kept.
	d := RenewDecision{Kind: RenewRotate, RefreshToken: grant.refresh}
	out := grantOutcome{class: workRotated, subject: cur.Subject, access: grant.access}

	if !grant.idToken.Empty() {
		idt, err := a.verifier.Verify(pctx, grant.idToken.Reveal())
		if err != nil {
			// go-oidc's errors can quote the token; log a fixed message. The ID
			// token is not stored, and identity must come from UserInfo.
			log.WarnContext(ctx, "session renewal: the refreshed ID token did not verify; serving within the outage allowance")
			out.badIDToken = true
			return d, out
		}
		// The subject is checked BEFORE ResolveUser, so a mismatched subject
		// can never provision or touch another user.
		if idt.Subject != cur.Subject {
			log.WarnContext(ctx, "session renewal refused: the ID token's subject is not the session user's")
			return RenewDecision{Kind: RenewRefuse}, grantOutcome{class: workRefused}
		}
		var extra idTokenClaims
		if err := idt.Claims(&extra); err != nil {
			log.WarnContext(ctx, "session renewal: the refreshed ID token's claims were unreadable; identity will be taken from UserInfo")
			return d, out
		}
		out.verified, out.claims = true, extra
		d.IDToken = grant.idToken
	}
	return d, out
}

type identityVerdict int

const (
	identOK identityVerdict = iota
	identTransient
	identRefused
)

// renewIdentity is phase B (ADR-048 section 4a): re-establish who the user is, as
// a login does, before the session is confirmed.
//
// UserInfo is REQUIRED when there is no verified ID token, or the ID token lacks
// the email or the groups: a renewal must not confirm, or hand ResolveUser, a
// half-checked identity (a missing email refuses the user; missing groups
// demotes an admin). A required call that fails is transient. Only a provider
// with no UserInfo endpoint at all is exempt from the ID-token-lacks-groups
// case, since nothing could ever fill it.
func (a *Authenticator) renewIdentity(ctx context.Context, log *slog.Logger, ref SessionRef, ph grantOutcome) (Claims, identityVerdict) {
	token := &oauth2.Token{AccessToken: ph.access.Reveal(), TokenType: "Bearer"}
	uctx := context.WithValue(ctx, oauth2.HTTPClient, renewHTTPClient)
	extra := ph.claims
	need := !ph.verified || extra.Email == "" || len(extra.Groups) == 0
	switch {
	case need && a.canFetchUserInfo():
		if err := a.mergeFetchedUserInfo(uctx, token, ph.subject, &extra); err != nil {
			if errors.Is(err, errUserInfoSubject) {
				log.WarnContext(ctx, "session renewal refused: UserInfo's subject is not the session user's")
				return Claims{}, identRefused
			}
			log.WarnContext(ctx, "session renewal: could not re-check identity (UserInfo failed); serving within the outage allowance")
			return Claims{}, identTransient
		}
		if extra.Email == "" {
			// UserInfo answered and there is still no email: the provider's
			// answer, as at login. There is nothing to provision from.
			log.InfoContext(ctx, "session renewal refused: the provider gave no email for the user")
			return Claims{}, identRefused
		}
	case !ph.verified:
		log.WarnContext(ctx, "session renewal: no verified ID token and no UserInfo endpoint; serving within the outage allowance")
		return Claims{}, identTransient
	}

	claims := Claims{
		Subject:           ph.subject,
		Email:             extra.Email,
		EmailVerified:     extra.EmailVerified,
		PreferredUsername: extra.PreferredUsername,
		Name:              extra.Name,
		Groups:            extra.Groups,
	}
	id, err := a.resolveUser(ctx, claims)
	if err != nil {
		if errors.Is(err, ErrRefused) || errors.Is(err, ErrCannotProvision) {
			log.InfoContext(ctx, "session renewal refused: the identity resolver refused the user", "err", err)
			return Claims{}, identRefused
		}
		// Anything else (a database error, a deadline) is a failure to decide,
		// not a refusal.
		log.WarnContext(ctx, "session renewal: the identity resolver failed; serving within the outage allowance", "err", err)
		return Claims{}, identTransient
	}
	if id != ref.UserID {
		log.ErrorContext(ctx, "session renewal refused: the session user's subject now resolves to a different user",
			"resolved_user", id.String(), "session_user", ref.UserID.String())
		return Claims{}, identRefused
	}
	return claims, identOK
}

// grantClass is who must act on a failed refresh grant.
type grantClass int

const (
	grantTransient     grantClass = iota // network, timeout, 5xx, 429, 3xx, unreadable
	grantRefused                         // the provider refused this USER
	grantServerRefused                   // the provider refused this SERVER
)

type grantError struct {
	class       grantClass
	code        string // an OAuth error code, or ours ("network", "http_503", ...)
	description string // the provider's, scrubbed and capped
}

type tokenGrant struct {
	access  xoauth2.Secret
	refresh xoauth2.Secret // "" when the provider did not rotate
	idToken xoauth2.Secret // "" when the response carried none
}

// renewHTTPClient is the token and UserInfo client: an owned transport (not
// http.DefaultTransport, which any package can reconfigure) with TLS 1.2 as the
// floor, and redirects NEVER followed, since a redirect would carry the refresh
// token and client secret to another host. The per-call timeout is the
// context's.
var renewHTTPClient = &http.Client{
	Transport: renewTransport(),
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func renewTransport() *http.Transport {
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

// prefersClientSecretPost reports whether the provider's advertised
// token_endpoint_auth_methods_supported rules out HTTP Basic but allows the
// client credentials in the body. An empty list means the default (Basic).
func prefersClientSecretPost(methods []string) bool {
	return len(methods) > 0 &&
		!slices.Contains(methods, "client_secret_basic") &&
		slices.Contains(methods, "client_secret_post")
}

// tokenEndpointRequest builds a POST of form to one of the provider's
// client-authenticated endpoints (the token endpoint, RFC 7009's revocation
// endpoint), with the client's credentials as HTTP Basic (RFC 6749 2.3.1) or, when
// discovery advertises only client_secret_post, in the body. It is the ONE place
// the choice is made, so renewal and revocation cannot disagree about it. The
// caller sends it with renewHTTPClient.
func (a *Authenticator) tokenEndpointRequest(ctx context.Context, endpoint string, form url.Values) (*http.Request, error) {
	if a.tokenAuthPost {
		form.Set("client_id", a.oauth2Config.ClientID)
		form.Set("client_secret", a.oauth2Config.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if !a.tokenAuthPost {
		// RFC 6749 2.3.1: the id and secret are form-urlencoded before Basic.
		req.SetBasicAuth(url.QueryEscape(a.oauth2Config.ClientID), url.QueryEscape(a.oauth2Config.ClientSecret))
	}
	return req, nil
}

// refreshGrant presents refresh at the provider's token endpoint ONCE. It is
// deliberately not oauth2.Config.TokenSource: with an unset auth style that
// library retries a failed request with the other style, which would present a
// single-use refresh token twice (ADR-048 section 4).
func (a *Authenticator) refreshGrant(ctx context.Context, refresh xoauth2.Secret) (tokenGrant, *grantError) {
	secret := xoauth2.Secret(a.oauth2Config.ClientSecret)
	secrets := []xoauth2.Secret{refresh, secret}
	req, err := a.tokenEndpointRequest(ctx, a.oauth2Config.Endpoint.TokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh.Reveal()},
	})
	if err != nil {
		return tokenGrant{}, &grantError{class: grantServerRefused, code: "bad_token_url"}
	}

	resp, err := renewHTTPClient.Do(req)
	if err != nil {
		// A *url.Error names the method, the URL and the cause; scrubbed anyway.
		return tokenGrant{}, &grantError{class: grantTransient, code: "network",
			description: xoauth2.CleanDescription(err.Error(), secrets...)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse))
	if err != nil {
		return tokenGrant{}, &grantError{class: grantTransient, code: "network",
			description: xoauth2.CleanDescription(err.Error(), secrets...)}
	}

	var parsed struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		IDToken          string `json:"id_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	jsonErr := json.Unmarshal(body, &parsed)

	if resp.StatusCode == http.StatusOK && jsonErr == nil && parsed.Error == "" {
		if parsed.AccessToken == "" {
			return tokenGrant{}, &grantError{class: grantTransient, code: "malformed_response"}
		}
		return tokenGrant{
			access:  xoauth2.Secret(parsed.AccessToken),
			refresh: xoauth2.Secret(parsed.RefreshToken),
			idToken: xoauth2.Secret(parsed.IDToken),
		}, nil
	}

	gerr := &grantError{
		code:        xoauth2.OAuthErrorCode(parsed.Error),
		description: xoauth2.CleanDescription(parsed.ErrorDescription, secrets...),
	}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Not followed. Not the provider's verdict on the grant.
		gerr.class, gerr.code, gerr.description = grantTransient, "http_3xx", ""
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		gerr.class = grantTransient
		if gerr.code == "" {
			gerr.code = fmt.Sprintf("http_%d", resp.StatusCode)
		}
	case jsonErr != nil || parsed.Error == "":
		// A 4xx (or a 200) that is not an OAuth answer: most likely an
		// intermediary, not the provider's verdict on this user.
		gerr.class, gerr.code, gerr.description = grantTransient, fmt.Sprintf("http_%d", resp.StatusCode), ""
	default:
		gerr.class = classifyGrantCode(gerr.code)
	}
	return tokenGrant{}, gerr
}

// classifyGrantCode maps an RFC 6749 5.2 error code to who must act. It mirrors
// internal/oauthmail's classification of the same codes.
func classifyGrantCode(code string) grantClass {
	switch code {
	case "invalid_grant", "interaction_required", "login_required", "consent_required":
		return grantRefused
	case "temporarily_unavailable", "server_error":
		return grantTransient
	default:
		// invalid_client, unauthorized_client, invalid_request, invalid_scope,
		// unsupported_grant_type and anything unknown: this server's request or
		// credentials were refused. No user action fixes that.
		return grantServerRefused
	}
}
