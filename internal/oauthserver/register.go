// SPDX-License-Identifier: AGPL-3.0-only

package oauthserver

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"mailroom/internal/store"
)

// Dynamic Client Registration (RFC 7591), ADR-050 section 4. POST /oauth/register
// is UNAUTHENTICATED by nature: anyone who can reach the server can call it. So
// everything it accepts is bounded, and it stores the least it can.
//
//   - Public clients only (token_endpoint_auth_method none). No client secret
//     and no registration access token (RFC 7592 management is not offered), so
//     a registration is nothing but a name and a list of redirect URIs.
//   - The client name is SELF-ASSERTED. It is cleaned the way a learned contact
//     name is (control, directional and invisible format characters removed),
//     bounded, and the consent screen labels it unverified. client_uri, logo_uri,
//     tos_uri, policy_uri and every other field are ignored and never stored:
//     a self-asserted logo or link on the consent page is a phishing aid.
//   - The body is capped at 16 KiB, every redirect URI at 2048 bytes, 10 URIs.
//   - Per-address limits (below) and a global cap on registrations no grant uses.
//
// A registration no grant uses is deleted 24 hours after it was made by the
// OAuth sweep; one a live grant names is kept while the grant lives.

// Registration bounds.
const (
	maxRegistrationBody     = 16 << 10
	maxRegistrationNameRune = 100
	maxRegistrationURIs     = 10
	// RegistrationsPerAddressPerDay is the most registrations one address may make
	// in 24 hours.
	RegistrationsPerAddressPerDay = 50
	// registrationBurst and registrationRefill are the burst limiter: a client
	// that registers more than a handful at once is not a person connecting an app.
	registrationBurst  = 5
	registrationRefill = 10 * time.Second
	// maxRegistrationAddrs bounds the limiter's memory.
	maxRegistrationAddrs = 4096
	// The whole process registers at most registrationGlobalBurst apps at once and
	// then one every registrationGlobalRefill (30 a minute), whoever asks and from
	// however many addresses, so filling the cap of unused registrations takes
	// hours, not seconds, and the sweep (every 15 minutes) can keep up.
	registrationGlobalBurst  = 30
	registrationGlobalRefill = 2 * time.Second
	// registrationConcurrency is how many registrations may be inside the store
	// at once. Each holds a database connection while it waits on the cap's lock,
	// and the pool is small and shared with every other request, so a flood must
	// queue here, not there.
	registrationConcurrency = 2
	registrationQueueWait   = 500 * time.Millisecond
)

// regLimiter limits registrations per client ADDRESS: at most
// RegistrationsPerAddressPerDay in any 24 hours, and a token-bucket burst.
//
// The key is the connection's address, an IPv6 address folded to its /64 (one
// subscriber commonly holds a whole /64, so keying on the full address would give
// a stranger 2^64 budgets). Behind a reverse proxy the address is the proxy's, so
// every client behind one proxy shares one budget and a stranger can exhaust it
// for all of them. That is the accepted cost of limiting an unauthenticated
// endpoint at all: the alternative, no limit, lets one address fill the client
// table. (Trusting a forwarded header here would let a stranger mint addresses at
// will and so defeat the limit entirely.) An operator whose proxy shares an
// address with real users can switch the endpoint off (MAILROOM_OAUTH_DCR=off)
// and use client metadata documents.
//
// The table of addresses is bounded and FAILS CLOSED: when it is full and no
// address is idle (nothing at its daily count, nothing short of a full burst),
// a NEW address is refused until one frees. A known address's count is never
// forgotten to make room, because forgetting is exactly what an attacker with
// many addresses would be buying. On top sits a process-wide rate (global*),
// which no number of addresses gets around.
type regLimiter struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]*regBucket

	gTokens float64
	gLast   time.Time
}

type regBucket struct {
	tokens float64
	last   time.Time
	done   []time.Time // registrations made in the last 24 hours, oldest first
}

func newRegLimiter() *regLimiter {
	return &regLimiter{now: time.Now, m: map[string]*regBucket{}, gTokens: registrationGlobalBurst}
}

// limitKey is what an address is limited as: an IPv4 address itself, an IPv6
// address as its /64 (an IPv4-mapped one as the IPv4 address).
func limitKey(addr string) string {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	a = a.Unmap()
	if a.Is6() {
		p, err := a.Prefix(64)
		if err != nil {
			return addr
		}
		return p.String()
	}
	return a.String()
}

// bucket returns the address's bucket, or nil when the address is new and the
// table is full of addresses that are not idle.
func (l *regLimiter) bucket(key string, now time.Time) *regBucket {
	b := l.m[key]
	if b == nil {
		if len(l.m) >= maxRegistrationAddrs {
			for k, v := range l.m {
				v.prune(now)
				if len(v.done) == 0 && v.refilled(now) {
					delete(l.m, k)
				}
			}
			if len(l.m) >= maxRegistrationAddrs {
				return nil
			}
		}
		b = &regBucket{tokens: registrationBurst, last: now}
		l.m[key] = b
	}
	if el := now.Sub(b.last); el > 0 {
		b.tokens = min(registrationBurst, b.tokens+el.Seconds()/registrationRefill.Seconds())
		b.last = now
	}
	b.prune(now)
	return b
}

func (b *regBucket) prune(now time.Time) {
	i := 0
	for i < len(b.done) && !b.done[i].After(now.Add(-24*time.Hour)) {
		i++
	}
	b.done = b.done[i:]
}

func (b *regBucket) refilled(now time.Time) bool {
	return b.tokens+now.Sub(b.last).Seconds()/registrationRefill.Seconds() >= registrationBurst
}

// admit charges one attempt to the burst bucket and refuses it, with how long
// to wait, when the burst is spent, the daily count is, or the table is full.
func (l *regLimiter) admit(addr string) (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.bucket(limitKey(addr), now)
	if b == nil {
		return false, time.Minute
	}
	if len(b.done) >= RegistrationsPerAddressPerDay {
		return false, b.done[0].Add(24 * time.Hour).Sub(now)
	}
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) * float64(registrationRefill))
	}
	b.tokens--
	return true, 0
}

// admitGlobal charges one registration to the process-wide rate.
func (l *regLimiter) admitGlobal() (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.gLast.IsZero() {
		l.gLast = now
	}
	if el := now.Sub(l.gLast); el > 0 {
		l.gTokens = min(registrationGlobalBurst, l.gTokens+el.Seconds()/registrationGlobalRefill.Seconds())
		l.gLast = now
	}
	if l.gTokens < 1 {
		return false, time.Duration((1 - l.gTokens) * float64(registrationGlobalRefill))
	}
	l.gTokens--
	return true, 0
}

// registered records a registration that was stored.
func (l *regLimiter) registered(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if b := l.bucket(limitKey(addr), now); b != nil {
		b.done = append(b.done, now)
	}
}

// registrationRequest is the RFC 7591 metadata Mailroom reads. Anything else in
// the document is ignored.
type registrationRequest struct {
	RedirectURIs  []string `json:"redirect_uris"`
	ClientName    *string  `json:"client_name"`
	AuthMethod    *string  `json:"token_endpoint_auth_method"`
	GrantTypes    []string `json:"grant_types"`
	ResponseTypes []string `json:"response_types"`
}

// registrationError is an RFC 7591 section 3.2.2 error.
func registrationError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func invalidMetadata(w http.ResponseWriter, desc string) {
	registrationError(w, http.StatusBadRequest, "invalid_client_metadata", desc)
}

// Register is POST /oauth/register. It exists only when Dynamic Client
// Registration is on; otherwise Register answers 404 like every OAuth route
// that is off.
func (s *Server) Register() http.Handler {
	if !s.dcr {
		return NotFound()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr := clientIP(r)
		if ok, retry := s.regLimit.admit(addr); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			registrationError(w, http.StatusTooManyRequests, "slow_down",
				"too many registrations from this address; wait and try again")
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			invalidMetadata(w, "the body must be application/json")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRegistrationBody))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				registrationError(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata",
					"the registration is larger than 16 KiB")
				return
			}
			invalidMetadata(w, "the body could not be read")
			return
		}
		var req registrationRequest
		if err := json.Unmarshal(body, &req); err != nil {
			invalidMetadata(w, "the body must be one JSON object of client metadata")
			return
		}
		grants, responses, name, errCode, errDesc := validateRegistration(req)
		if errCode != "" {
			registrationError(w, http.StatusBadRequest, errCode, errDesc)
			return
		}
		// Few registrations inside the store at once: each holds a pooled database
		// connection while it waits on the cap's lock.
		select {
		case s.regSem <- struct{}{}:
			defer func() { <-s.regSem }()
		case <-time.After(registrationQueueWait):
			w.Header().Set("Retry-After", "1")
			registrationError(w, http.StatusTooManyRequests, "slow_down", "too many registrations are being processed; try again")
			return
		case <-r.Context().Done():
			return
		}
		if ok, retry := s.regLimit.admitGlobal(); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			registrationError(w, http.StatusTooManyRequests, "slow_down", "this server is registering apps as fast as it allows; try again shortly")
			return
		}
		c, stored, freeAt, err := s.st.RegisterOAuthClient(r.Context(), name, req.RedirectURIs, s.maxUnused)
		if err != nil {
			s.log.ErrorContext(r.Context(), "oauth: register client", "err", err)
			registrationError(w, http.StatusServiceUnavailable, "server_error", "the server could not complete the request")
			return
		}
		if !stored {
			// No place frees before the oldest unused registration is 24 hours old
			// and the next sweep (every 15 minutes) has run.
			wait := max(time.Minute, time.Until(freeAt)+RegistrationSweepInterval)
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
			registrationError(w, http.StatusServiceUnavailable, "server_error",
				"too many applications are waiting to be approved; try again later")
			return
		}
		s.regLimit.registered(addr)
		s.log.InfoContext(r.Context(), "oauth: client registered", "client_id", c.ID)
		writeJSON(w, http.StatusCreated, map[string]any{
			"client_id":                  c.ID,
			"client_id_issued_at":        c.CreatedAt.Unix(),
			"client_name":                name,
			"redirect_uris":              req.RedirectURIs,
			"grant_types":                grants,
			"response_types":             responses,
			"token_endpoint_auth_method": "none",
		})
	})
}

// validateRegistration checks the metadata and returns what the response
// repeats. A non-empty code is the RFC 7591 error to answer.
func validateRegistration(req registrationRequest) (grants, responses []string, name, code, desc string) {
	bad := func(d string) ([]string, []string, string, string, string) {
		return nil, nil, "", "invalid_client_metadata", d
	}
	if req.AuthMethod != nil && *req.AuthMethod != "none" {
		return bad("only public clients are supported: token_endpoint_auth_method must be none")
	}
	grants = req.GrantTypes
	if len(grants) == 0 {
		grants = []string{"authorization_code"}
	}
	for _, g := range grants {
		if g != "authorization_code" && g != "refresh_token" {
			return bad("grant_types may contain only authorization_code and refresh_token")
		}
	}
	responses = req.ResponseTypes
	if len(responses) == 0 {
		responses = []string{"code"}
	}
	for _, rt := range responses {
		if rt != "code" {
			return bad("response_types may contain only code")
		}
	}
	if n := len(req.RedirectURIs); n < 1 || n > maxRegistrationURIs {
		return nil, nil, "", "invalid_redirect_uri", "between 1 and 10 redirect_uris are required"
	}
	for _, u := range req.RedirectURIs {
		if err := ValidateRedirectURI(u); err != nil {
			return nil, nil, "", "invalid_redirect_uri", "a redirect URI is not acceptable: " + err.Error()
		}
	}
	if req.ClientName == nil {
		return bad("client_name is required")
	}
	name = store.CleanLearnedContactName(*req.ClientName)
	if name == "" {
		return bad("client_name is required")
	}
	if utf8.RuneCountInString(name) > maxRegistrationNameRune {
		return bad("client_name is longer than 100 characters")
	}
	return slices.Clone(grants), slices.Clone(responses), name, "", ""
}

// RegistrationSweepInterval is how often the OAuth sweep runs, and so how soon
// after an unused registration turns 24 hours old its place is free again.
const RegistrationSweepInterval = 15 * time.Minute
