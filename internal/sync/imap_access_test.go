// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"errors"
	"testing"

	"mailroom/internal/xoauth2"
)

// THE IMAP-OFF HINT (ADR-046 §12). Microsoft refuses a token with a bare
// `NO AUTHENTICATE failed.` both when the token is stale and when IMAP is
// switched off for the mailbox (Outlook.com's default). The two are told
// apart by the retry: a refusal of the token the provider issued in place of
// a refused one is ErrIMAPAccessRefused; a first refusal followed by success
// is nothing at all.
//
// PROBE: drop `refused.retried = true` in xoauth2Auth.authenticate and the
// first case goes red.
func TestFreshTokenRefusalIsIMAPAccessRefused(t *testing.T) {
	src := &scriptedSource{tokens: []xoauth2.Secret{imapTestToken, "ya29.FRESH-sentinel"}}
	_, sess, err := dialXOAuthScript(t, xoauthIMAPScript{
		caps:    "IMAP4rev1 SASL-IR AUTH=XOAUTH2",
		verdict: func(string) string { return "no" },
	}, withXOAuth2("you@example.com", src))
	if sess != nil {
		t.Fatal("Dial succeeded")
	}
	if !errors.Is(err, ErrIMAPAccessRefused) {
		t.Fatalf("err = %v, want ErrIMAPAccessRefused", err)
	}
	var se *Error
	if !errors.As(err, &se) || se.Kind != ErrorKindAuth {
		t.Errorf("err = %v, want it still ErrorKindAuth", err)
	}
}

// A non-credential failure is not the hint.
func TestMechanismMissingIsNotIMAPAccessRefused(t *testing.T) {
	_, _, err := dialXOAuthScript(t, xoauthIMAPScript{
		caps:    "IMAP4rev1 AUTH=PLAIN",
		verdict: func(string) string { return "ok" },
	}, withXOAuth2("you@example.com", staticToken(imapTestToken)))
	if errors.Is(err, ErrIMAPAccessRefused) {
		t.Fatalf("err = %v read as IMAP access refused", err)
	}
}
