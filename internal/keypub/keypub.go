// SPDX-License-Identifier: AGPL-3.0-only

// Package keypub serves the PUBLIC half of a user's PGP keys: Web Key Directory
// lookups and a shareable key page (Forgejo #48, spec §9).
//
// # This is the first anonymous, cross-user, caller-parameterised surface
//
// Everything else on this origin is either behind the login gate or a constant
// (/healthz, /readyz, the sign-in page). WKD is none of those: a stranger with
// no session supplies part of the query, and the answer comes from another
// user's rows. Three consequences are designed for rather than discovered:
//
//   - THE LOOKUP CANNOT READ PRIVATE MATERIAL. Not by convention — by
//     privilege. It goes through app.wkd_public_key, a SECURITY DEFINER owned by
//     mailroom_wkd: a role that owns NO TABLES, holds column-level SELECT on
//     exactly the public columns of app.identities and app.user_keys, and is
//     used by nothing else in the database. private_key_ct, dek_id and
//     envelope_ver are simply not granted, so rewriting the function body to
//     select them fails at runtime with `permission denied`. Rewriting THIS file
//     cannot widen it either: everything here goes through that one function.
//
//     ⚠ THE MECHANISM DESCRIBED HERE UNTIL #309 WAS THE SUPERSEDED ONE. It said
//     the definer's role "OWNS the table in this deployment" and that a REVOKE
//     was "the load-bearing half". That was migrations 00031/00032's design, and
//     it could never work: PostgreSQL runs a foreign key's referential-integrity
//     action with the privileges of the TABLE OWNER, so narrowing the owner
//     either did nothing (a column revoke cannot remove a table-level privilege)
//     or broke every ON DELETE CASCADE and SET NULL into those two tables — which
//     it did, on a real instance, for hours. 00034 restored the grant and the
//     narrowing became decoration; 00035 (#266) replaced the whole approach by
//     moving the definer to a role that owns nothing. There is NO revoke on
//     mailroom_owner today, and mailroom_owner can still read everything it owns
//     — that is inherent to ownership and is no longer where the boundary sits.
//     The boundary is that the anonymous, cross-user path does not run as an
//     owner at all.
//
//   - NOTHING HERE ENUMERATES. The request names a hash, not an address, and a
//     miss is indistinguishable from a hit for an address that exists but is
//     unpublished: both are a bare 404 with no body. Publishing is what makes
//     an address findable, and it is an explicit act.
//
//   - THE WORK PER REQUEST IS ONE INDEXED LOOKUP, and a path-shape check runs
//     BEFORE the database is touched, so a malformed name costs a string
//     comparison rather than a query. That lookup is now bounded by a rate limit
//     that is ON BY DEFAULT (#264) — see ratelimit.go for why it is global
//     rather than per-IP, which is not the obvious choice and is the correct
//     one while this server has no notion of a trusted proxy.
//
//     ⚠ What it does NOT bound is malformed paths, deliberately: they never
//     reach the database, and letting them spend the budget would let garbage
//     traffic starve real lookups. Bounding those is the reverse proxy's job.
//
// # WKD serves BINARY, and that is not a detail
//
// draft-koch-openpgp-webkey-service requires the raw OpenPGP packets with
// Content-Type application/octet-stream. An ASCII-armored body is the single
// most common way to ship a WKD endpoint that returns 200 to curl and that no
// mail client will accept — the failure is invisible from the server side,
// which is exactly why there is a test asserting the absence of armor.
package keypub

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Lookup answers an anonymous WKD request. Implemented by *store.Store.
type Lookup interface {
	LookupWKDKey(ctx context.Context, domain, hash string) (key []byte, fingerprint string, found bool, err error)
}

// Service serves the public key surface.
type Service struct {
	lookup Lookup
	log    *slog.Logger
	// domain is the mail domain this instance publishes for. WKD's direct
	// method puts no domain in the path, so the server must know which one it
	// is answering for; without it a lookup for alice@other.example would be
	// answered from this instance's rows.
	domain string
	// limit bounds database work on this anonymous surface (#264). Built by
	// default — see ratelimit.go for why it is global rather than per-IP, and
	// why defaulting to off would have shipped it inert.
	limit *rateLimiter
}

// WithRateLimit replaces the default limiter. A disabled config turns limiting
// off entirely, which is an explicit act — the default is ON.
func (s *Service) WithRateLimit(cfg RateLimitConfig, now func() time.Time) *Service {
	s.limit = newRateLimiter(cfg, now)
	return s
}

// New builds the service. domain is the mail domain served by the direct
// method; it may be empty, in which case WKD is disabled rather than answering
// for a domain nobody configured.
func New(lookup Lookup, domain string, log *slog.Logger) (*Service, error) {
	if lookup == nil {
		return nil, errors.New("keypub: nil lookup")
	}
	if log == nil {
		return nil, errors.New("keypub: nil logger")
	}
	// ⚠ The key page needs PageLookup, and it used to discover that at REQUEST
	// time via a type assertion that fell back to a generic 404. That fails
	// OPEN in the worst way available here: a mis-wired page is byte-identical
	// to an unpublished key, so the symptom is "nobody's key page works" with
	// nothing red anywhere. It was caught only because a gate test stub
	// implemented one interface and not the other.
	//
	// Refusing at construction turns that into a startup failure, which is a
	// thing an operator sees.
	if _, ok := lookup.(PageLookup); !ok {
		return nil, errors.New("keypub: lookup does not implement PageLookup; the key page would 404 silently")
	}
	return &Service{
		lookup: lookup,
		log:    log,
		domain: strings.ToLower(strings.TrimSpace(domain)),
		// ⚠ ON by default. A limiter that only exists when someone remembers to
		// configure it is one that ships inert, which is how this very package's
		// publication path shipped (#48). Opting out is explicit.
		limit: newRateLimiter(DefaultRateLimit(), nil),
	}, nil
}

// throttled consumes a token and, when the budget is spent, writes the 429 and
// reports true so the caller returns without touching the database.
//
// ⚠ IT IS CALLED AFTER THE SHAPE CHECK AND BEFORE THE LOOKUP, and both halves of
// that are deliberate:
//
//   - After the shape check, so a flood of malformed paths cannot starve real
//     lookups. Those cost a string comparison and never reach the database;
//     bounding them is the reverse proxy's job, not this process's.
//   - Before the lookup, so the 429 is identical whether the hash exists or not.
//     A limiter applied after would answer differently for a published address
//     than an unpublished one, and quietly become the enumeration oracle this
//     whole package is built to avoid.
func (s *Service) throttled(w http.ResponseWriter, r *http.Request) bool {
	ok, retry := s.limit.allow()
	if ok {
		return false
	}
	secs := int(retry.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	// No body: this response goes to anyone, and it carries no information
	// about what was asked for.
	http.Error(w, "too many requests", http.StatusTooManyRequests)
	s.log.Warn("keypub: anonymous key lookup throttled", "path", r.URL.Path, "retry_after_s", secs)
	return true
}

// Enabled reports whether WKD is configured. A disabled service still serves
// its routes — it answers 404 — so that the route inventory does not change
// shape based on configuration, which would make the gate tests conditional.
func (s *Service) Enabled() bool { return s.domain != "" }

// hashLen is the fixed length of a WKD directory name: 20 bytes of SHA-1 in
// z-base-32. Anything else is not a WKD request and is refused before the
// database is consulted.
const hashLen = 32

// HUHandler serves /.well-known/openpgpkey/hu/<hash> — the direct method.
//
// The response is the BINARY public key. No armor, no JSON, no wrapper.
func (s *Service) HUHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The draft asks servers to allow cross-origin reads; some webmail
		// clients look keys up from the browser. It exposes nothing that is not
		// already public by definition.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		// This is not HTML and must never be sniffed as any.
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if !s.Enabled() {
			http.NotFound(w, r)
			return
		}

		hash := strings.TrimPrefix(r.URL.Path, "/.well-known/openpgpkey/hu/")
		if !validHash(hash) {
			// Refused without touching the database. A malformed path is the
			// cheapest thing an anonymous caller can send, and it stays cheap.
			http.NotFound(w, r)
			return
		}
		if s.throttled(w, r) {
			return
		}

		key, fpr, found, err := s.lookup.LookupWKDKey(r.Context(), s.domain, hash)
		if err != nil {
			// Do not relay the error: this response goes to anyone.
			s.log.Error("wkd lookup failed", "err", err)
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		if !found || len(key) == 0 {
			// The stdlib's GENERIC 404 — deliberately not a message of our own.
			// An unpublished address and a nonexistent one must be byte-identical
			// from out here, so nothing may distinguish "no such user" from "not
			// published"; a body that carried either would turn this endpoint
			// into an address oracle, one guess at a time.
			http.NotFound(w, r)
			return
		}
		if bytes.Contains(key, []byte("-----BEGIN PGP")) {
			// Stored armor would be a bug upstream, and serving it would make
			// this endpoint silently useless to every real client. Refuse
			// rather than serve something that looks like it worked.
			s.log.Error("wkd: stored key is armored, not binary", "fingerprint", fpr)
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}

		// ⚠ application/octet-stream is REQUIRED by the draft. text/plain is the
		// tempting substitution — the response is small and inspectable with
		// curl, so it looks harmless — and it makes the endpoint non-conformant
		// while every server-side signal stays green. TestHUHandlerServesBinary-
		// NotArmor asserts this exact value, and has been negative-controlled by
		// flipping this line and confirming it goes red.
		w.Header().Set("Content-Type", "application/octet-stream")
		// Public, stable, and fetched by clients that may retry often. An hour
		// is short enough that a withdrawal takes effect while a person is
		// still paying attention.
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(key); err != nil {
			s.log.Warn("wkd: write response", "err", err)
		}
	})
}

// PolicyHandler serves /.well-known/openpgpkey/policy.
//
// Its EXISTENCE is the signal — the draft defines the file as the marker that a
// domain supports WKD, and an empty body is valid. A domain serving keys but no
// policy file is, to some clients, not a WKD domain at all.
func (s *Service) PolicyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !s.Enabled() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.WriteHeader(http.StatusOK)
	})
}

func validHash(h string) bool {
	if len(h) != hashLen {
		return false
	}
	for i := 0; i < len(h); i++ {
		if !strings.ContainsRune(zBase32Chars, rune(h[i])) {
			return false
		}
	}
	return true
}

// zBase32Chars mirrors the alphabet in internal/wkd. It is duplicated as a
// VALIDATION set rather than imported as the encoder, because this is a check
// on untrusted input and should not reach into the encoder's internals.
const zBase32Chars = "ybndrfg8ejkmcpqxot1uwisza345h769"

// (DirectPathFor was removed: it claimed the key page used it, and the key page
// calls wkd.DirectPath directly. A helper whose doc names a caller it does not
// have is worse than no helper.)
