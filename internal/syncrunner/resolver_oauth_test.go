// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

type fakeTokens struct {
	calls int
	tok   xoauth2.Secret
}

func (f *fakeTokens) TokenSource(ulid.ULID, ulid.ULID) xoauth2.TokenSource { return fakeSource{f} }

type fakeSource struct{ f *fakeTokens }

func (s fakeSource) Token(context.Context) (xoauth2.Secret, error) {
	s.f.calls++
	return s.f.tok, nil
}
func (fakeSource) Reject(xoauth2.Secret) {}

// googleHosts stands in for the registry's Google entry.
var googleHosts = xoauth2.Hosts{IMAPHost: "imap.gmail.com", IMAPPort: 993,
	SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPImplicitTLS: true}

func (f *fakeTokens) HostsFor(provider string) (xoauth2.Hosts, bool) {
	if provider == "google" {
		return googleHosts, true
	}
	return xoauth2.Hosts{}, false
}

// An OAuth account (ADR-046) gets XOAUTH2 with the token fetched per dial, and
// no Username/Password; without a token source it is refused, never dialled
// with LOGIN. A password account is untouched (TestResolverPrepare).
func TestResolverPrepareOAuthAccount(t *testing.T) {
	master := testMasterKey()
	userID := ulid.Make()
	ct, err := onboarding.SealCredentials(master, userID, onboarding.Credentials{
		IMAPUsername: "you@example.com",
		OAuth:        &onboarding.OAuthSecret{Provider: "google", RefreshToken: "1//refresh-sentinel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	acct := store.Account{ID: ulid.Make(), IMAPHost: "imap.gmail.com", IMAPPort: 993, AuthKind: "oauth2", AuthState: store.OAuthStateActive, CredentialsCT: ct}

	if _, err := NewCredentialResolver(fakeKeys{master}).Prepare(context.Background(), userID, acct); !errors.Is(err, ErrOAuthNotConfigured) {
		t.Fatalf("Prepare without a token source = %v, want ErrOAuthNotConfigured", err)
	}

	tokens := &fakeTokens{tok: "ya29.access"}
	req, err := NewCredentialResolver(fakeKeys{master}, WithAccessTokens(tokens)).Prepare(context.Background(), userID, acct)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if req.Config.Auth == nil {
		t.Fatal("an OAuth account got no Auth mechanism")
	}
	if req.Config.Username != "" || req.Config.Password != "" {
		t.Errorf("an OAuth account carries LOGIN credentials %q/%q", req.Config.Username, req.Config.Password)
	}
	if err := req.Config.Validate(); err != nil {
		t.Errorf("resolved OAuth config invalid: %v", err)
	}
	if tokens.calls != 0 {
		t.Error("Prepare fetched a token; it must be fetched at authentication time")
	}
}

// REGRESSION: a password account still resolves to LOGIN with its own
// credentials even when the resolver has a token source (the sealed JSON
// itself is pinned in onboarding's TestPasswordCredentialsJSONUnchanged).
func TestPasswordCredentialsUnchangedByOAuthField(t *testing.T) {
	master := testMasterKey()
	userID := ulid.Make()
	ct, err := onboarding.SealCredentials(master, userID, onboarding.Credentials{IMAPUsername: "u@example.com", IMAPPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := NewCredentialResolver(fakeKeys{master}, WithAccessTokens(&fakeTokens{})).Prepare(context.Background(), userID,
		store.Account{ID: ulid.Make(), IMAPHost: "imap.example.com", CredentialsCT: ct})
	if err != nil {
		t.Fatal(err)
	}
	if req.Config.Auth != nil || req.Config.Username != "u@example.com" || req.Config.Password != "pw" {
		t.Errorf("password account resolved to Auth=%v user=%q", req.Config.Auth != nil, req.Config.Username)
	}
}

// MEDIUM-3 (security review of #894): an OAuth account is dialled at its
// PROVIDER's host, never its row's. A row naming another host is refused —
// Prepare returns no Config at all, so nothing can dial it — and no token is
// fetched. A password account still dials exactly what its row says.
func TestResolverPinsOAuthHostsToTheRegistry(t *testing.T) {
	master := testMasterKey()
	userID := ulid.Make()
	oauthCT, err := onboarding.SealCredentials(master, userID, onboarding.Credentials{
		IMAPUsername: "you@example.com",
		OAuth:        &onboarding.OAuthSecret{Provider: "google", RefreshToken: "1//r"},
	})
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
		wantErr  error
	}{
		{"oauth row matches", "imap.gmail.com", 993, oauthCT, "imap.gmail.com", nil},
		{"oauth row with default port", "imap.gmail.com", 0, oauthCT, "imap.gmail.com", nil},
		{"oauth row names an attacker host", "imap.attacker.test", 993, oauthCT, "", xoauth2.ErrHostMismatch},
		{"oauth row names another port", "imap.gmail.com", 143, oauthCT, "", xoauth2.ErrHostMismatch},
		{"password account uses its row", "imap.attacker.test", 993, pwCT, "imap.attacker.test", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens := &fakeTokens{tok: "ya29.access"}
			req, err := NewCredentialResolver(fakeKeys{master}, WithAccessTokens(tokens)).Prepare(context.Background(), userID,
				store.Account{ID: ulid.Make(), IMAPHost: tc.host, IMAPPort: tc.port, CredentialsCT: tc.ct})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Prepare = %v, want %v", err, tc.wantErr)
				}
				if req.Config.Host != "" {
					t.Errorf("a refused account still produced a Config for %q", req.Config.Host)
				}
				if tokens.calls != 0 {
					t.Error("a token was fetched for a refused account")
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if req.Config.Host != tc.wantHost {
				t.Errorf("dial host = %q, want %q", req.Config.Host, tc.wantHost)
			}
		})
	}
	// A provider the server does not have configured is refused too.
	msCT, _ := onboarding.SealCredentials(master, userID, onboarding.Credentials{
		IMAPUsername: "you@example.com", OAuth: &onboarding.OAuthSecret{Provider: "microsoft", RefreshToken: "r"}})
	if _, err := NewCredentialResolver(fakeKeys{master}, WithAccessTokens(&fakeTokens{})).Prepare(context.Background(), userID,
		store.Account{ID: ulid.Make(), IMAPHost: "outlook.office365.com", CredentialsCT: msCT}); !errors.Is(err, ErrOAuthNotConfigured) {
		t.Errorf("unconfigured provider = %v, want ErrOAuthNotConfigured", err)
	}
}

// #52, ADR-046 §5: an account whose grant needs its user to reconnect is
// refused BEFORE anything is opened — no envelope, no token, so no dial — and
// the refusal is the reauth sentinel the health surfaces key on. The
// credentials here are garbage on purpose: if Prepare got as far as opening
// them, the error would be a decryption failure instead.
func TestResolverPrepareRefusesAwaitingReconnect(t *testing.T) {
	tokens := &fakeTokens{tok: "ya29.access"}
	acct := store.Account{ID: ulid.Make(), IMAPHost: "imap.gmail.com", IMAPPort: 993, AuthKind: "oauth2",
		AuthState: store.OAuthStateNeedsReauth, CredentialsCT: []byte("not an envelope")}
	_, err := NewCredentialResolver(fakeKeys{testMasterKey()}, WithAccessTokens(tokens)).Prepare(context.Background(), ulid.Make(), acct)
	if !errors.Is(err, ErrAwaitingReconnect) || !errors.Is(err, xoauth2.ErrReauthRequired) {
		t.Fatalf("Prepare = %v, want ErrAwaitingReconnect (an ErrReauthRequired)", err)
	}
	// Disconnected (an OAuth account with no grant) is refused the same way.
	disc := acct
	disc.AuthState = ""
	if _, err := NewCredentialResolver(fakeKeys{testMasterKey()}, WithAccessTokens(tokens)).Prepare(context.Background(), ulid.Make(), disc); !errors.Is(err, ErrAwaitingReconnect) {
		t.Fatalf("Prepare of a disconnected account = %v, want ErrAwaitingReconnect", err)
	}
	if tokens.calls != 0 {
		t.Error("a token was fetched for an account waiting to be reconnected")
	}
	// The other states still prepare normally (operator_error keeps trying on
	// the normal backoff; active is ordinary).
	master := testMasterKey()
	userID := ulid.Make()
	ct, err := onboarding.SealCredentials(master, userID, onboarding.Credentials{
		IMAPUsername: "you@example.com",
		OAuth:        &onboarding.OAuthSecret{Provider: "google", RefreshToken: "1//refresh-sentinel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{store.OAuthStateActive, store.OAuthStateOperatorError} {
		acct := store.Account{ID: ulid.Make(), IMAPHost: "imap.gmail.com", IMAPPort: 993, AuthKind: "oauth2",
			AuthState: st, CredentialsCT: ct}
		if _, err := NewCredentialResolver(fakeKeys{master}, WithAccessTokens(tokens)).Prepare(context.Background(), userID, acct); err != nil {
			t.Errorf("state %s: Prepare = %v", st, err)
		}
	}
}
