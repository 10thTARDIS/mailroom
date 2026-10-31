// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		op   string
		err  error
		want ErrorKind
	}{
		{
			name: "nil error",
			op:   "select",
			err:  nil,
			want: ErrorKindUnknown,
		},
		{
			name: "authentication failed code",
			op:   "login",
			err:  &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAuthenticationFailed, Text: "bad creds"},
			want: ErrorKindAuth,
		},
		{
			name: "authorization failed code",
			op:   "select",
			err:  &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAuthorizationFailed},
			want: ErrorKindAuth,
		},
		{
			name: "unavailable code is transient",
			op:   "select",
			err:  &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeUnavailable},
			want: ErrorKindTransient,
		},
		{
			name: "plain NO on login without code is auth",
			op:   "login",
			err:  &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Login failed"},
			want: ErrorKindAuth,
		},
		{
			name: "plain NO on select without code is protocol, not auth",
			op:   "select",
			err:  &imap.Error{Type: imap.StatusResponseTypeNo, Text: "no such mailbox"},
			want: ErrorKindProtocol,
		},
		{
			name: "plain BAD on fetch is protocol",
			op:   "fetch",
			err:  &imap.Error{Type: imap.StatusResponseTypeBad, Text: "malformed command"},
			want: ErrorKindProtocol,
		},
		{
			name: "context deadline exceeded is transient",
			op:   "select",
			err:  context.DeadlineExceeded,
			want: ErrorKindTransient,
		},
		{
			name: "context canceled is transient",
			op:   "select",
			err:  context.Canceled,
			want: ErrorKindTransient,
		},
		{
			name: "net.Error is transient",
			op:   "dial",
			err:  &net.DNSError{Err: "no such host", IsTimeout: true},
			want: ErrorKindTransient,
		},
		{
			name: "generic error is unknown",
			op:   "select",
			err:  errors.New("something odd happened"),
			want: ErrorKindUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.op, tt.err); got != tt.want {
				t.Errorf("classify(%q, %v) = %v, want %v", tt.op, tt.err, got, tt.want)
			}
		})
	}
}

// migaduSearchRefusal is the EXACT text Migadu returned (#638),
// copied from a sync log. The whole point of a text match is that it
// matches the real string, so the test uses the real string.
const migaduSearchRefusal = "search rate limit exceeded: 60 searches in 1m0s, please wait 42s before trying again"

// TestClassifyRateLimited covers the refusal #638 is about, in both the form the
// standard defines and the form the server actually sends — and, just as
// importantly, the refusals that must NOT be read as throttling.
func TestClassifyRateLimited(t *testing.T) {
	tests := []struct {
		name           string
		op             string
		err            error
		wantKind       ErrorKind
		wantRetryAfter time.Duration
	}{
		{
			name:           "migadu plain NO on liveuids, no response code",
			op:             "liveuids",
			err:            &imap.Error{Type: imap.StatusResponseTypeNo, Text: migaduSearchRefusal},
			wantKind:       ErrorKindRateLimited,
			wantRetryAfter: 42 * time.Second,
		},
		{
			name:           "RFC 5530 [LIMIT] response code",
			op:             "liveuids",
			err:            &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "Too many commands"},
			wantKind:       ErrorKindRateLimited,
			wantRetryAfter: 0, // the server named no wait
		},
		{
			name:           "[LIMIT] with a named wait",
			op:             "search",
			err:            &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "throttled, retry after 30 seconds"},
			wantKind:       ErrorKindRateLimited,
			wantRetryAfter: 30 * time.Second,
		},
		{
			name:           "writeback's UIDPRESENT search is paced by the same budget",
			op:             "uidpresent",
			err:            &imap.Error{Type: imap.StatusResponseTypeNo, Text: migaduSearchRefusal},
			wantKind:       ErrorKindRateLimited,
			wantRetryAfter: 42 * time.Second,
		},

		// --- CONTROLS: these must never be read as throttling ---------------
		{
			name:     "control: a plain NO from select is protocol, not rate limited",
			op:       "select",
			err:      &imap.Error{Type: imap.StatusResponseTypeNo, Text: "no such mailbox"},
			wantKind: ErrorKindProtocol,
		},
		{
			name:     "control: an auth failure is auth, not rate limited",
			op:       "login",
			err:      &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAuthenticationFailed, Text: "bad creds"},
			wantKind: ErrorKindAuth,
		},
		{
			name:     "control: an ordinary NO on a SEARCH op is still protocol",
			op:       "liveuids",
			err:      &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Mailbox does not exist"},
			wantKind: ErrorKindProtocol,
		},
		{
			name: "control: the text match is scoped to SEARCH ops",
			// Same words, a non-SEARCH op. An APPEND refused for a quota policy
			// that happens to say "rate limit" must not stop the observation loop.
			op:       "append",
			err:      &imap.Error{Type: imap.StatusResponseTypeNo, Text: migaduSearchRefusal},
			wantKind: ErrorKindProtocol,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, retryAfter := classifyRefusal(tt.op, tt.err)
			if kind != tt.wantKind {
				t.Errorf("classifyRefusal(%q, %v) kind = %v, want %v", tt.op, tt.err, kind, tt.wantKind)
			}
			if retryAfter != tt.wantRetryAfter {
				t.Errorf("classifyRefusal(%q, %v) retryAfter = %v, want %v",
					tt.op, tt.err, retryAfter, tt.wantRetryAfter)
			}

			// The exported helpers are the only supported way to ask, so assert
			// on them too, over the *Error a real op would hand back.
			opErr := newOpError(tt.op, tt.err)
			if got := IsRateLimited(opErr); got != (tt.wantKind == ErrorKindRateLimited) {
				t.Errorf("IsRateLimited(%v) = %v, want %v", opErr, got, tt.wantKind == ErrorKindRateLimited)
			}
			if got := RetryAfterOf(opErr); got != tt.wantRetryAfter {
				t.Errorf("RetryAfterOf(%v) = %v, want %v", opErr, got, tt.wantRetryAfter)
			}
		})
	}
}

// TestRetryAfterIgnoresTheRateWindow pins the one parse that would be worse than
// no parse: Migadu's message names the WINDOW ("60 searches in 1m0s") before it
// names the wait ("please wait 42s"). A pattern that grabbed the first number
// followed by a unit would return 60 — or 0 from "1m0s" — and the caller would
// report a wait the server never asked for.
func TestRetryAfterIgnoresTheRateWindow(t *testing.T) {
	if got := parseRetryAfter(migaduSearchRefusal); got != 42*time.Second {
		t.Errorf("parseRetryAfter(%q) = %v, want 42s", migaduSearchRefusal, got)
	}
	if got := parseRetryAfter("search rate limit exceeded: 60 searches in 1m0s"); got != 0 {
		t.Errorf("parseRetryAfter with no wait named = %v, want 0 (the server did not say)", got)
	}
}

// TestIsRateLimitedOnForeignErrors is the negative control for the exported
// helpers: they answer about OUR error type, and must not guess from text.
func TestIsRateLimitedOnForeignErrors(t *testing.T) {
	if IsRateLimited(nil) {
		t.Error("IsRateLimited(nil) = true")
	}
	if IsRateLimited(errors.New(migaduSearchRefusal)) {
		t.Error("IsRateLimited on a bare error carrying the refusal text = true; " +
			"only a classified *Error may answer yes")
	}
	if got := RetryAfterOf(errors.New(migaduSearchRefusal)); got != 0 {
		t.Errorf("RetryAfterOf on a bare error = %v, want 0", got)
	}
}

func TestErrorKindString(t *testing.T) {
	tests := []struct {
		kind ErrorKind
		want string
	}{
		{ErrorKindUnknown, "unknown"},
		{ErrorKindConfig, "config"},
		{ErrorKindAuth, "auth"},
		{ErrorKindTransient, "transient"},
		{ErrorKindProtocol, "protocol"},
		{ErrorKindRateLimited, "rate_limited"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("ErrorKind(%d).String() = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestErrorUnwrapAndMessage(t *testing.T) {
	cause := errors.New("boom")
	err := &Error{Kind: ErrorKindTransient, Op: "fetch", Err: cause}

	if !errors.Is(err, cause) {
		t.Error("errors.Is(err, cause) = false, want true (Unwrap should expose cause)")
	}

	msg := err.Error()
	if msg == "" {
		t.Error("Error() returned empty string")
	}
	// sanity: message should mention the op and the underlying cause text.
	if !strings.Contains(msg, "fetch") || !strings.Contains(msg, "boom") {
		t.Errorf("Error() = %q, want it to mention op and cause", msg)
	}
}
