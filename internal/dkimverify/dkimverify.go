// SPDX-License-Identifier: AGPL-3.0-only

// Package dkimverify verifies the DKIM signatures of a raw message in-process
// (#776). It is the anti-corruption layer (spec §3) around a third-party DKIM
// library: callers see only the Result and Status types defined here, and the
// library's errors are mapped to Status in exactly one place, classify.
//
// It is READ-ONLY measurement machinery. Nothing in the delivery, store or
// display path calls it; the first consumer is `mailroom-admin auth-report
// --authserv --no-ar --dkim`, which asks how many messages that carry no
// Authentication-Results would authenticate if Mailroom checked them itself.
//
// BOUNDS, all of them reported rather than silent:
//   - at most MaxMessageBytes of a message are verified; a larger one is a
//     single `unsupported` result with detail "too large";
//   - at most MaxLookups DNS lookups per Verifier. Once spent, further key
//     lookups fail as `temperror` with detail "lookup budget spent", and
//     Stats().BudgetRefused counts them so the bound is visible;
//   - at most maxSignatures signatures per message are verified.
package dkimverify

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// Status is the outcome of verifying one signature.
type Status string

const (
	// StatusPass: the signature verified against the published key.
	StatusPass Status = "pass"
	// StatusFailBody: the body hash did not match (the body changed after signing).
	StatusFailBody Status = "fail_body"
	// StatusFailSignature: the signature did not verify over the headers.
	StatusFailSignature Status = "fail_signature"
	// StatusKeyNotFound: no usable key record at selector._domainkey.domain.
	StatusKeyNotFound Status = "key_not_found"
	// StatusKeyRevoked: the key record exists with an empty p= (revoked).
	StatusKeyRevoked Status = "key_revoked"
	// StatusTempError: a transient problem (DNS failure or timeout, or the lookup
	// budget was spent). A retry might give another answer.
	StatusTempError Status = "temperror"
	// StatusPermError: the signature or key record is malformed, expired, or
	// otherwise unusable, in a way a retry will not change.
	StatusPermError Status = "permerror"
	// StatusUnsupported: something this verifier declines to evaluate (weak or
	// unknown algorithm, the insecure l= tag, a message over the size bound).
	StatusUnsupported Status = "unsupported"
)

// Result is the outcome for one DKIM-Signature header.
type Result struct {
	Domain   string // d= (lowercased); "" when the tag could not be read
	Selector string // s=; "" when it could not be read
	Status   Status
	Detail   string // short, fixed vocabulary; never key material or signature bytes
	// SignedHeaders is the signature's h= list: lowercased header names in
	// order, a name repeated once per instance signed (an OVERSIGNED header
	// appears more times than the message holds it). nil when the tag could not
	// be matched to this result. Used to tell whether a passing signature can be
	// defeated by a prepended header (RFC 6376 §8.15; ADR-036).
	SignedHeaders []string
}

const (
	// DefaultMaxMessageBytes is how much of a message is verified.
	DefaultMaxMessageBytes = 25 << 20
	// DefaultMaxLookups is the DNS lookup budget per Verifier.
	DefaultMaxLookups = 5000
	// DefaultLookupTimeout bounds one DNS lookup.
	DefaultLookupTimeout = 5 * time.Second

	maxSignatures = 10

	detailBudget   = "lookup budget spent"
	detailTooLarge = "too large"
)

// LookupTXTFunc returns the TXT records for name. Contract: an EMPTY result with
// a nil error means "no such record" (it is cached as a negative answer and
// reported key_not_found); a non-nil error means the lookup FAILED (temperror).
type LookupTXTFunc func(ctx context.Context, name string) ([]string, error)

// Options configure a Verifier. Zero values take the defaults above.
type Options struct {
	LookupTXT       LookupTXTFunc // default: DefaultLookupTXT (the system resolver)
	LookupTimeout   time.Duration // per lookup
	MaxLookups      int           // total DNS lookups (cache misses) per Verifier
	MaxMessageBytes int           // messages larger than this are unsupported
}

// Stats is what a Verifier has spent so far.
type Stats struct {
	Budget        int // MaxLookups
	Lookups       int // DNS lookups actually issued
	CacheHits     int // key lookups answered from the cache
	BudgetRefused int // key lookups refused because the budget was spent (each is one temperror)
}

// Verifier verifies messages. It caches DNS answers (found, not found and
// failed) by name for its own lifetime, so make one per run. Safe for
// concurrent use.
type Verifier struct {
	lookup  LookupTXTFunc
	timeout time.Duration
	max     int
	maxMsg  int

	mu    sync.Mutex
	cache map[string]cacheEntry
	stats Stats
}

type cacheEntry struct {
	txts []string
	err  error
}

// New returns a Verifier.
func New(opts Options) *Verifier {
	v := &Verifier{lookup: opts.LookupTXT, timeout: opts.LookupTimeout, max: opts.MaxLookups,
		maxMsg: opts.MaxMessageBytes, cache: map[string]cacheEntry{}}
	if v.lookup == nil {
		v.lookup = DefaultLookupTXT
	}
	if v.timeout <= 0 {
		v.timeout = DefaultLookupTimeout
	}
	if v.max <= 0 {
		v.max = DefaultMaxLookups
	}
	if v.maxMsg <= 0 {
		v.maxMsg = DefaultMaxMessageBytes
	}
	v.stats.Budget = v.max
	return v
}

// Stats returns the lookup accounting so far.
func (v *Verifier) Stats() Stats {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stats
}

// DefaultLookupTXT resolves with the system resolver. "No such name" (NXDOMAIN
// or no TXT data) is the empty-result-nil-error case of the contract; any
// other failure is returned as an error.
func DefaultLookupTXT(ctx context.Context, name string) ([]string, error) {
	txts, err := net.DefaultResolver.LookupTXT(ctx, name)
	var de *net.DNSError
	if errors.As(err, &de) && de.IsNotFound {
		return nil, nil
	}
	return txts, err
}

// lookupError is what the library sees from our resolver wrapper. It claims to
// be temporary, which is how the library tells "could not look it up" (a
// temporary failure) from "looked it up, nothing usable" (a permanent one).
type lookupError struct {
	msg     string
	timeout bool
}

func (e *lookupError) Error() string   { return e.msg }
func (e *lookupError) Timeout() bool   { return e.timeout }
func (e *lookupError) Temporary() bool { return true }

var errBudget = &lookupError{msg: detailBudget}

// lookupFor returns the library-facing lookup bound to ctx.
func (v *Verifier) lookupFor(ctx context.Context) func(string) ([]string, error) {
	return func(name string) ([]string, error) {
		name = strings.ToLower(name)
		// Held across the lookup on purpose: the library verifies several
		// signatures concurrently, and two signatures sharing a key name must cost
		// one lookup, not two.
		v.mu.Lock()
		defer v.mu.Unlock()
		if e, ok := v.cache[name]; ok {
			v.stats.CacheHits++
			return e.txts, e.err
		}
		if v.stats.Lookups >= v.max {
			v.stats.BudgetRefused++ // not cached: every refused lookup is counted
			return nil, errBudget
		}
		v.stats.Lookups++
		lctx, cancel := context.WithTimeout(ctx, v.timeout)
		defer cancel()
		txts, err := v.lookup(lctx, name)
		var out error
		if err != nil {
			var ne net.Error
			out = &lookupError{msg: "dns lookup failed", timeout: errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded)}
		}
		v.cache[name] = cacheEntry{txts: txts, err: out}
		return txts, out
	}
}

// Verify checks every DKIM-Signature of raw and returns one Result per
// signature, in header order. A message with no signature returns an empty
// slice (that is "none", not an error). A message over the size bound returns
// one unsupported Result.
func (v *Verifier) Verify(ctx context.Context, raw []byte) []Result {
	if len(raw) > v.maxMsg {
		return []Result{{Status: StatusUnsupported, Detail: detailTooLarge}}
	}
	tags := signatureTags(raw)
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		LookupTXT: v.lookupFor(ctx), MaxVerifications: maxSignatures,
	})
	if err != nil && !errors.Is(err, dkim.ErrTooManySignatures) {
		// Not attributable to any one signature (an unreadable header, or an I/O
		// failure in one of the concurrent verifications, whose Err the library
		// clears: the verifications must not be read as passes).
		return []Result{{Status: StatusPermError, Detail: "message not verifiable"}}
	}
	out := make([]Result, 0, len(verifs)+1)
	for i, vf := range verifs {
		st, detail := classify(vf.Err)
		r := Result{Domain: strings.ToLower(vf.Domain), Status: st, Detail: detail}
		if i < len(tags) && len(tags) == len(verifs) {
			r.Selector = tags[i].selector
			r.SignedHeaders = tags[i].headers
			if r.Domain == "" {
				r.Domain = tags[i].domain
			}
		}
		out = append(out, r)
	}
	if errors.Is(err, dkim.ErrTooManySignatures) {
		out = append(out, Result{Status: StatusUnsupported, Detail: "more signatures than verified"})
	}
	return out
}

// classify is THE place the library's errors become our Status. The library
// exports IsPermFail and IsTempFail but not its signature-failure type, and it
// distinguishes the cases below only by message, so those messages are matched
// here and nowhere else. The tests exercise each mapped case against the real
// library (a library upgrade that rewords an error turns one of them red).
//
//	library outcome                                   Status         Detail
//	nil                                               pass           ""
//	temporary failure (DNS error, timeout, budget)    temperror      "dns lookup failed" | "lookup budget spent"
//	permanent: "key revoked" (empty p=)               key_revoked    "key revoked"
//	permanent: "no valid key found" / "no key for     key_not_found  "no key record"
//	  signature" (empty or failed-permanent lookup)
//	permanent: "signature has expired"                permerror      "signature expired"
//	permanent: "multiple TXT records found for key"   permerror      "multiple key records"
//	permanent: "unsupported ..." / "... too weak" /   unsupported    "unsupported algorithm or key"
//	  "too short" / "inappropriate ..."
//	permanent: anything else (malformed tags, v=,     permerror      "malformed signature or key"
//	  missing tags, From not signed, bad base64, ...)
//	other: "body hash did not verify"                 fail_body      "body hash mismatch"
//	other: "signature did not verify"                 fail_signature "signature mismatch"
//	other: "insecure body length tag" (l=)            unsupported    "l= tag rejected"
//	other: anything else                              fail_signature "signature invalid"
func classify(err error) (Status, string) {
	if err == nil {
		return StatusPass, ""
	}
	msg := err.Error()
	switch {
	case dkim.IsTempFail(err):
		if strings.Contains(msg, detailBudget) {
			return StatusTempError, detailBudget
		}
		return StatusTempError, "dns lookup failed"
	case dkim.IsPermFail(err):
		switch {
		case strings.Contains(msg, "key revoked"):
			return StatusKeyRevoked, "key revoked"
		case strings.Contains(msg, "no valid key found"), strings.Contains(msg, "no key for signature"):
			return StatusKeyNotFound, "no key record"
		case strings.Contains(msg, "signature has expired"):
			return StatusPermError, "signature expired"
		case strings.Contains(msg, "multiple TXT records"):
			return StatusPermError, "multiple key records"
		case strings.Contains(msg, "unsupported"), strings.Contains(msg, "too weak"),
			strings.Contains(msg, "too short"), strings.Contains(msg, "inappropriate"):
			return StatusUnsupported, "unsupported algorithm or key"
		}
		return StatusPermError, "malformed signature or key"
	case strings.Contains(msg, "body hash did not verify"):
		return StatusFailBody, "body hash mismatch"
	case strings.Contains(msg, "signature did not verify"):
		return StatusFailSignature, "signature mismatch"
	case strings.Contains(msg, "insecure body length tag"):
		return StatusUnsupported, "l= tag rejected"
	}
	return StatusFailSignature, "signature invalid"
}

// ---- reading d= and s= for each signature, in header order ----

type sigTag struct {
	domain, selector string
	headers          []string // h=, lowercased, in order
}

// signatureTags returns d= and s= of each DKIM-Signature header, in header
// order, which is the order the library reports its verifications in. The
// library does not expose the selector. Reading stops at the end of the header
// block; the block is bounded.
func signatureTags(raw []byte) []sigTag {
	end := len(raw)
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if i := bytes.Index(raw, sep); i >= 0 && i < end {
			end = i
		}
	}
	if end > 256<<10 {
		end = 256 << 10
	}
	block := strings.ReplaceAll(string(raw[:end]), "\r\n", "\n")
	var fields []string // unfolded header fields
	for _, line := range strings.Split(block, "\n") {
		if line != "" && (line[0] == ' ' || line[0] == '\t') && len(fields) > 0 {
			fields[len(fields)-1] += " " + strings.TrimSpace(line)
			continue
		}
		fields = append(fields, line)
	}
	var out []sigTag
	for _, f := range fields {
		i := strings.IndexByte(f, ':')
		if i <= 0 || !strings.EqualFold(strings.TrimSpace(f[:i]), "dkim-signature") {
			continue
		}
		var t sigTag
		for _, part := range strings.Split(f[i+1:], ";") {
			k, val, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			val = strings.Join(strings.Fields(val), "")
			switch strings.TrimSpace(k) {
			case "d":
				t.domain = strings.ToLower(val)
			case "s":
				t.selector = val
			case "h":
				for _, h := range strings.Split(strings.ToLower(val), ":") {
					if h != "" {
						t.headers = append(t.headers, h)
					}
				}
			}
		}
		out = append(out, t)
	}
	return out
}
