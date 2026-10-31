// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

type testMaster struct{ m []byte }

func (k testMaster) MasterKey(context.Context) ([]byte, error) {
	return append([]byte(nil), k.m...), nil
}

type smtpTestTokens struct{ calls int }

func (f *smtpTestTokens) TokenSource(ulid.ULID, ulid.ULID) xoauth2.TokenSource {
	return smtpTestSource{f}
}

type smtpTestSource struct{ f *smtpTestTokens }

func (s smtpTestSource) Token(context.Context) (xoauth2.Secret, error) {
	s.f.calls++
	return "ya29.smtp-test", nil
}
func (smtpTestSource) Reject(xoauth2.Secret) {}

func (f *smtpTestTokens) HostsFor(provider string) (xoauth2.Hosts, bool) {
	if provider != "google" {
		return xoauth2.Hosts{}, false
	}
	return xoauth2.Hosts{IMAPHost: "imap.gmail.com", IMAPPort: 993,
		SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPImplicitTLS: true}, true
}

// MEDIUM-3 (security review of #894): an OAuth send goes to the PROVIDER's
// SMTP server with the provider's TLS mode; a row naming any other server is
// refused before a token is even fetched. A password send still goes where
// its row says.
func TestSMTPLoginPinsOAuthHosts(t *testing.T) {
	master := make([]byte, 32)
	userID := ulid.Make()
	oauthCT, err := onboarding.SealCredentials(master, userID, onboarding.Credentials{
		IMAPUsername: "you@example.com", OAuth: &onboarding.OAuthSecret{Provider: "google", RefreshToken: "r"}})
	if err != nil {
		t.Fatal(err)
	}
	pwCT, err := onboarding.SealCredentials(master, userID, onboarding.Credentials{IMAPUsername: "u", IMAPPassword: "p"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		host     string
		port     int
		ct       []byte
		wantHost string
		wantTLS  bool
		wantErr  bool
	}{
		{"oauth row matches", "smtp.gmail.com", 465, oauthCT, "smtp.gmail.com", true, false},
		{"oauth row names an attacker host", "smtp.attacker.test", 465, oauthCT, "", false, true},
		{"oauth row names another port", "smtp.gmail.com", 587, oauthCT, "", false, true},
		{"password account uses its row", "smtp.attacker.test", 587, pwCT, "smtp.attacker.test", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens := &smtpTestTokens{}
			k := &keyResolver{keys: testMaster{master}, tokens: tokens}
			login, err := k.SMTPAuth(context.Background(), userID,
				store.Account{ID: ulid.Make(), SMTPHost: tc.host, SMTPPort: tc.port, CredentialsCT: tc.ct})
			if tc.wantErr {
				if !errors.Is(err, xoauth2.ErrHostMismatch) {
					t.Fatalf("SMTPAuth = %v, want ErrHostMismatch", err)
				}
				if login.Host != "" || tokens.calls != 0 {
					t.Errorf("refused account produced host %q, %d token fetches", login.Host, tokens.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if login.Host != tc.wantHost || login.ImplicitTLS != tc.wantTLS || login.Auth == nil {
				t.Errorf("login = %s:%d tls=%v, want %s tls=%v", login.Host, login.Port, login.ImplicitTLS, tc.wantHost, tc.wantTLS)
			}
		})
	}
}
