// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"mailroom/internal/egress"
	"mailroom/internal/xoauth2"
)

// ErrorKind classifies a sync error so callers (the sync engine, account
// health reporting — spec §12) can decide whether to retry, back off, or
// surface a user-facing "reauthenticate" prompt without needing to know
// anything about IMAP wire-level status codes.
type ErrorKind int

const (
	// ErrorKindUnknown covers errors that don't fit a more specific bucket;
	// treat conservatively (do not retry blindly).
	ErrorKindUnknown ErrorKind = iota
	// ErrorKindConfig indicates a caller-supplied Config was invalid;
	// retrying without changing the config will not help.
	ErrorKindConfig
	// ErrorKindAuth indicates the server rejected credentials or
	// authorization; retrying without user intervention will not help.
	ErrorKindAuth
	// ErrorKindTransient indicates a network hiccup, timeout, canceled
	// context, or a server-reported temporary condition; safe to retry
	// with backoff.
	ErrorKindTransient
	// ErrorKindProtocol indicates the server returned something unexpected
	// (a NO/BAD outside the recognized auth/transient cases, or malformed
	// data); may indicate a server incompatibility.
	ErrorKindProtocol
	// ErrorKindRateLimited indicates the server refused the command because
	// we asked TOO OFTEN, not because anything is wrong with the account, the
	// mailbox, or the command. It is deliberately NOT ErrorKindTransient:
	// transient means "safe to retry with backoff", and the whole point of
	// this kind is that the caller must STOP ASKING for a while rather than
	// retry. Error.RetryAfter carries the server's own wait when it named one.
	//
	// It exists because of #638: an account with hundreds of mailboxes hit
	// Migadu's 60-SEARCH-per-minute ceiling on every observation pass, and
	// every refusal was recorded as that mailbox's own failure and followed
	// immediately by the next SEARCH — hundreds of commands per pass that the
	// server had already said it would not answer.
	ErrorKindRateLimited
)

// String implements fmt.Stringer.
func (k ErrorKind) String() string {
	switch k {
	case ErrorKindConfig:
		return "config"
	case ErrorKindAuth:
		return "auth"
	case ErrorKindTransient:
		return "transient"
	case ErrorKindProtocol:
		return "protocol"
	case ErrorKindRateLimited:
		return "rate_limited"
	default:
		return "unknown"
	}
}

// Error is the error type returned by every SyncSession/Dial operation. It
// never exposes go-imap types in its own fields; Unwrap gives access to the
// underlying cause for errors.Is/As against stdlib error types (net.Error,
// context.DeadlineExceeded, etc).
type Error struct {
	Kind ErrorKind
	// Op names the operation that failed: "dial", "login", "list",
	// "select", "fetch", "store", "copy", "move", "expunge", "append",
	// "noop", "idle", "logout".
	Op  string
	Err error

	// RetryAfter is how long the server asked us to wait before asking again,
	// when it said so in the refusal text (Migadu: "please wait 42s before
	// trying again"). ZERO MEANS THE SERVER DID NOT SAY — never "retry
	// immediately". It is only ever set alongside ErrorKindRateLimited; read
	// it through RetryAfterOf rather than touching the field, so no caller
	// grows its own idea of what a rate-limit refusal looks like.
	RetryAfter time.Duration
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("sync: %s: %v", e.Op, e.Err)
}

// Unwrap supports errors.Is/errors.As against the underlying cause.
func (e *Error) Unwrap() error {
	return e.Err
}

// classify inspects an error returned by the underlying go-imap client and
// buckets it into an ErrorKind. It is classifyRefusal without the retry hint,
// kept because most callers only want the bucket.
func classify(op string, err error) ErrorKind {
	kind, _ := classifyRefusal(op, err)
	return kind
}

// newOpError wraps err as this package's own *Error for operation op, with the
// kind AND the server's retry hint both decided in one place. Every
// go-imap-facing error in this package is built through it, so a rate-limit
// refusal can never reach a caller with its RetryAfter silently dropped.
func newOpError(op string, err error) *Error {
	kind, retryAfter := classifyRefusal(op, err)
	return &Error{Kind: kind, Op: op, Err: err, RetryAfter: retryAfter}
}

// classifyRefusal inspects an error returned by the underlying go-imap client
// and buckets it into an ErrorKind, together with how long the server asked us
// to wait (zero when it did not say). op is the operation that produced err (see
// Error.Op); it disambiguates cases where go-imap's error alone is not
// enough, e.g. a plain "NO" during "login" almost always means bad
// credentials even without an [AUTHENTICATIONFAILED] response code, but a
// plain "NO" during "select" usually does not mean that.
//
// This is the one place go-imap's *imap.Error type is allowed to appear —
// as an internal implementation detail, never in an exported signature.
func classifyRefusal(op string, err error) (ErrorKind, time.Duration) {
	if err == nil {
		return ErrorKindUnknown, 0
	}

	// XOAUTH2 (ADR-046). Checked BEFORE the go-imap error below because
	// authRefusedError wraps one: a refusal of AUTHENTICATE is a credential
	// verdict on every op, not only under "login".
	var refused *authRefusedError
	if errors.As(err, &refused) {
		return ErrorKindAuth, 0
	}
	switch {
	case errors.Is(err, errMechanismUnsupported), errors.Is(err, errBadXOAuth2Input):
		return ErrorKindConfig, 0
	case errors.Is(err, xoauth2.ErrReauthRequired):
		// The grant is dead; only the user reconnecting fixes it.
		return ErrorKindAuth, 0
	case errors.Is(err, xoauth2.ErrProviderMisconfigured):
		// The provider refused the SERVER's client credentials: the operator
		// must act, and "reauthenticate" would be the wrong prompt.
		return ErrorKindConfig, 0
	case errors.Is(err, xoauth2.ErrTokenUnavailable):
		return ErrorKindTransient, 0
	}

	var imapErr *imap.Error
	if errors.As(err, &imapErr) {
		switch imapErr.Code {
		case imap.ResponseCodeAuthenticationFailed, imap.ResponseCodeAuthorizationFailed:
			return ErrorKindAuth, 0
		case imap.ResponseCodeUnavailable:
			// RFC 5530: "temporary failure because a subsystem is down" —
			// the canonical retry-later code.
			return ErrorKindTransient, 0
		case imap.ResponseCodeLimit:
			// RFC 5530 [LIMIT]: the command ran up against a server-imposed
			// limit. This is the STANDARD way to say "you asked too often", so
			// it is honoured on any op and without inspecting the text — a
			// server that bothers to send the code has already told us plainly.
			// Verified present in the pinned dependency:
			// github.com/emersion/go-imap/v2@v2.0.0-beta.8 response.go:35.
			return ErrorKindRateLimited, parseRetryAfter(imapErr.Text)
		}
		if isSearchOp(op) && looksRateLimited(imapErr.Text) {
			// ⚠ MIGADU SENDS A PLAIN `NO` WITH NO RESPONSE CODE (#638):
			//
			//   NO search rate limit exceeded: 60 searches in 1m0s,
			//      please wait 42s before trying again
			//
			// Without this, that refusal classified as ErrorKindProtocol, was
			// recorded as the mailbox's own failure, and the next SEARCH went
			// out milliseconds later — thousands of refused commands over hours of
			// use. Text matching is a last resort, so it is SCOPED TO THE
			// OPS THAT ISSUE A UID SEARCH: an unrelated `NO` from SELECT or
			// APPEND can never be misread as throttling however it is worded.
			return ErrorKindRateLimited, parseRetryAfter(imapErr.Text)
		}
		if op == "login" && imapErr.Type != imap.StatusResponseTypeOK {
			// Many servers reply a plain "NO Login failed" without a
			// response code; in the login op specifically, any non-OK
			// status is a credentials/authorization problem.
			return ErrorKindAuth, 0
		}
		return ErrorKindProtocol, 0
	}

	// The egress guard refused the account's host (#625 §3): it resolves to an
	// address the policy does not permit, and no connection was made. That is a
	// refusal of the CONFIGURATION, not a network hiccup — retrying immediately
	// cannot change it — so it is ErrorKindConfig, never Transient. (It is
	// checked before the net.Error case only for clarity: the egress error is
	// not a net.Error.)
	if errors.Is(err, egress.ErrBlockedAddress) {
		return ErrorKindConfig, 0
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrorKindTransient, 0
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return ErrorKindTransient, 0
	}

	return ErrorKindUnknown, 0
}

// searchOps are the Error.Op values whose command is a UID SEARCH, and so the
// only ops whose refusal text is examined for a rate-limit phrase. They are
// exactly the three UIDSearch call sites in session.go: LiveUIDs' observation
// ("liveuids") and writeback's two lookups ("search", "uidpresent"). Keep this
// in step with those call sites — an op missing here silently loses the text
// fallback on a server that sends no [LIMIT] code.
var searchOps = map[string]struct{}{
	"liveuids":   {},
	"search":     {},
	"uidpresent": {},
}

func isSearchOp(op string) bool {
	_, ok := searchOps[op]
	return ok
}

// rateLimitPhrases are the substrings (matched case-insensitively) that mark a
// refusal as throttling rather than a real failure. They are deliberately
// specific: a bare "too many" would also match answers about message counts or
// METADATA's [TOOMANY], so the phrase has to name the thing there were too many
// OF. "rate limit" is Migadu's wording and the common one.
var rateLimitPhrases = []string{
	"rate limit",
	"rate-limit",
	"ratelimit",
	"too many requests",
	"too many searches",
	"too many commands",
	"throttl", // "throttled", "throttling"
}

func looksRateLimited(text string) bool {
	lower := strings.ToLower(text)
	for _, p := range rateLimitPhrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// retryAfterPattern pulls the server's own wait out of a refusal. It is anchored
// on the verb ("please wait 42s", "retry after 30 seconds", "try again in 5s")
// rather than on a bare number-plus-s, because Migadu's message also contains
// "60 searches in 1m0s" — the WINDOW, not the wait — and reading that as the
// retry hint would be worse than having none.
var retryAfterPattern = regexp.MustCompile(
	`(?i)\b(?:please\s+)?(?:wait|retry(?:\s+(?:after|in))?|try\s+again\s+in)\s+(\d+)\s*(?:s\b|sec|second)`)

// parseRetryAfter returns the wait the server named, or zero when it named none.
func parseRetryAfter(text string) time.Duration {
	m := retryAfterPattern.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	secs, err := strconv.Atoi(m[1])
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// IsRateLimited reports whether err is a server refusal caused by asking too
// often (ErrorKindRateLimited). It is the ONLY supported way to ask: callers
// must never match on error text themselves, which is precisely what this
// package's anti-corruption seam exists to prevent.
func IsRateLimited(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == ErrorKindRateLimited
}

// RetryAfterOf returns how long the server asked the caller to wait, or zero
// when it did not say (or err is not one of ours). ZERO IS NOT "RETRY NOW" — it
// means the server gave no number, and the caller must pick its own backoff.
func RetryAfterOf(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}
