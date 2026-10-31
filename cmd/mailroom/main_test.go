// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/config"
	imapsync "mailroom/internal/sync"
)

// oidcCfg is a Config whose OIDC block is complete (config.OIDCConfigured()).
func oidcCfg() *config.Config {
	return &config.Config{
		OIDCIssuer:       "https://auth.example.com/application/o/mailroom/",
		OIDCClientID:     "mailroom",
		OIDCClientSecret: "secret",
		OIDCRedirectURL:  "https://mail.example.com/auth/callback",
		SessionKey:       []byte(strings.Repeat("k", config.MinSessionKeyLen)),
	}
}

func TestOperatorIdentity(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *config.Config
		wantSubject string
		wantEmail   string
		wantErr     string
	}{
		{
			name: "oidc with operator",
			cfg: func() *config.Config {
				c := oidcCfg()
				c.OperatorSubject = "operator-sub"
				c.OperatorEmail = "operator@example.com"
				return c
			}(),
			wantSubject: "operator-sub",
			wantEmail:   "operator@example.com",
		},
		{
			// Legal: the instance serves users seeded out of band and syncs
			// nothing. No operator, no provisioning, no sync loop.
			name: "oidc without operator",
			cfg:  oidcCfg(),
		},
		{
			name: "oidc operator without email is an error",
			cfg: func() *config.Config {
				c := oidcCfg()
				c.OperatorSubject = "operator-sub"
				return c
			}(),
			wantErr: config.EnvOperatorEmail,
		},
		{
			// The dev-auth subject IS the operator (config.go).
			name: "dev-auth",
			cfg: &config.Config{
				DevAuthSubject: "dev-operator",
				DevAuthEmail:   "dev@example.com",
			},
			wantSubject: "dev-operator",
			wantEmail:   "dev@example.com",
		},
		{
			name:    "dev-auth without email is an error",
			cfg:     &config.Config{DevAuthSubject: "dev-operator"},
			wantErr: config.EnvDevAuthEmail,
		},
		{
			// OIDC wins outright: the dev-auth fields are not the operator's,
			// and the OIDC operator fields are (here: absent).
			name: "oidc beats dev-auth",
			cfg: func() *config.Config {
				c := oidcCfg()
				c.DevAuthSubject = "dev-operator"
				c.DevAuthEmail = "dev@example.com"
				return c
			}(),
			wantSubject: "",
			wantEmail:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subject, email, err := operatorIdentity(tt.cfg)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("operatorIdentity() error = nil, want an error naming %s", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to name %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("operatorIdentity() error = %v", err)
			}
			if subject != tt.wantSubject || email != tt.wantEmail {
				t.Errorf("operatorIdentity() = (%q, %q), want (%q, %q)",
					subject, email, tt.wantSubject, tt.wantEmail)
			}
		})
	}
}

func TestHasGroup(t *testing.T) {
	cases := []struct {
		name   string
		groups []string
		want   bool
	}{
		{"member", []string{"staff", adminGroup, "other"}, true},
		{"non-member", []string{"staff", "other"}, false},
		{"empty", nil, false},
		{"exact match only", []string{adminGroup + "-not"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasGroup(tc.groups, adminGroup); got != tc.want {
				t.Errorf("hasGroup(%v) = %v, want %v", tc.groups, got, tc.want)
			}
		})
	}
}

func TestDisplayNameFromClaims(t *testing.T) {
	cases := []struct {
		name   string
		claims auth.Claims
		want   string
	}{
		{"name wins", auth.Claims{Name: "Ada Lovelace", PreferredUsername: "ada"}, "Ada Lovelace"},
		{"falls back to preferred_username", auth.Claims{PreferredUsername: "ada"}, "ada"},
		{"blank when neither (never synthesized from email)", auth.Claims{Email: "ada@example.com"}, ""},
		{"trims whitespace", auth.Claims{Name: "  Ada  "}, "Ada"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayNameFromClaims(tc.claims); got != tc.want {
				t.Errorf("displayNameFromClaims(%+v) = %q, want %q", tc.claims, got, tc.want)
			}
		})
	}
}

// A verified token with no email claim cannot provision an honest account, so the
// resolver refuses it BEFORE touching the store (fail closed). Passing a nil store
// proves the store is never reached on this path.
func TestProvisioningResolverRefusesTokenWithoutEmail(t *testing.T) {
	resolve := provisioningResolver(nil, "", quietLogger())
	if _, err := resolve(context.Background(), auth.Claims{Subject: "sub-1", Groups: []string{adminGroup}}); err == nil {
		t.Fatal("provisioningResolver accepted a token with no email claim; want a refusal")
	}
}

// operatorSubjectMismatch is the pure decision behind Forgejo #574's two log
// lines (the ErrLoginEmailTaken hint and the "created a NEW user" warning):
// provisioningResolver's DB-touching branches around it are covered by the
// store and auth integration tests instead (this is the seam that lets the
// decision itself be tested without one, per this repo's DB-free-unit-test
// convention — see operatorIdentity above).
func TestOperatorSubjectMismatch(t *testing.T) {
	cases := []struct {
		name            string
		operatorSubject string
		tokenSubject    string
		want            bool
	}{
		{"no operator configured", "", "sub-real", false},
		{"neither set", "", "", false},
		{"subjects agree", "sub-real", "sub-real", false},
		{"subjects disagree", "sub-configured-wrong", "sub-real", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := operatorSubjectMismatch(tc.operatorSubject, tc.tokenSubject); got != tc.want {
				t.Errorf("operatorSubjectMismatch(%q, %q) = %v, want %v",
					tc.operatorSubject, tc.tokenSubject, got, tc.want)
			}
		})
	}
}

func TestCallbackPath(t *testing.T) {
	ok := map[string]string{
		"https://mail.example.com/auth/callback": "/auth/callback",
		"http://localhost:8080/oidc/cb":          "/oidc/cb",
	}
	for in, want := range ok {
		got, err := callbackPath(in)
		if err != nil {
			t.Fatalf("callbackPath(%q) error = %v", in, err)
		}
		if got != want {
			t.Errorf("callbackPath(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{
		"https://mail.example.com",  // no path
		"https://mail.example.com/", // root: would swallow the whole app
		"https://mail.example.com" + loginPath,
		"https://mail.example.com" + logoutPath,
		"://nonsense",
	}
	for _, in := range bad {
		if got, err := callbackPath(in); err == nil {
			t.Errorf("callbackPath(%q) = %q, nil; want an error", in, got)
		}
	}
}

// The one place a declared password is legitimately revealed is the hand-off to
// onboarding, which seals it. Prove the conversion actually carries it (and the
// TLS mode a real account needs).
func TestAccountSpecCarriesCredentials(t *testing.T) {
	spec := accountSpec(config.Account{
		Name:         "personal",
		IMAPHost:     "imap.example.com",
		IMAPPort:     993,
		SMTPHost:     "smtp.example.com",
		SMTPPort:     465,
		Username:     "you@example.com",
		Password:     config.Secret("hunter2"),
		SMTPPassword: config.Secret("other"),
	})

	if spec.Password != "hunter2" || spec.SMTPPassword != "other" {
		t.Errorf("accountSpec did not carry the passwords through to onboarding")
	}
	if spec.TLS != imapsync.TLSImplicit {
		t.Errorf("TLS = %v, want implicit TLS for a real account", spec.TLS)
	}
	if spec.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true — a declared account must never skip verification")
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("spec.Validate() = %v, want a complete spec", err)
	}
	if spec.WritebackEnabled != nil {
		t.Errorf("WritebackEnabled = %v, want nil when the accounts file left it unset", *spec.WritebackEnabled)
	}
}

// TestAPIRateLimitConfigFromConfig pins #624 §3: MAILROOM_RATE_LIMIT_RPS /
// MAILROOM_RATE_LIMIT_BURST are parsed by config.Load but were never actually
// applied to the API — buildAppHandler always got api.New's compiled 50/100
// default. This proves a configured value reaches api.WithRateLimit's input
// unchanged, and that the documented "0 disables" contract holds all the way
// through: NewRateLimiter on the converted config returns nil (no limiter),
// matching RateLimitConfig.Enabled()'s own definition rather than restating it.
func TestAPIRateLimitConfigFromConfig(t *testing.T) {
	cfg := &config.Config{RateLimitPerSecond: 12.5, RateLimitBurst: 7}
	got := apiRateLimitConfig(cfg)
	want := api.RateLimitConfig{PerSecond: 12.5, Burst: 7}
	if got != want {
		t.Errorf("apiRateLimitConfig(cfg) = %+v, want %+v", got, want)
	}
	if !got.Enabled() {
		t.Errorf("apiRateLimitConfig(cfg).Enabled() = false, want true for a positive rate and burst")
	}

	// The operator's explicit disable knob (either field 0) must reach the
	// limiter as disabled, not silently fall back to the compiled default.
	disabled := &config.Config{RateLimitPerSecond: 0, RateLimitBurst: 0}
	got = apiRateLimitConfig(disabled)
	if got.Enabled() {
		t.Errorf("apiRateLimitConfig(disabled cfg) = %+v, want Enabled() == false", got)
	}
	if rl := api.NewRateLimiter(got); rl != nil {
		t.Error("api.NewRateLimiter(apiRateLimitConfig(disabled cfg)) != nil, want limiting off")
	}
}

// TestAccountSpecCarriesWritebackEnabled pins the ADR-028 path that lets the
// production account be declared already shadowed in the accounts file: an
// explicit false must reach onboarding.AccountSpec, not silently become nil
// (which onboarding.AddAccount would then hand to CreateAccount as "schema
// default true" — the exact shadowing failure this field exists to prevent).
func TestAccountSpecCarriesWritebackEnabled(t *testing.T) {
	shadowed := false
	spec := accountSpec(config.Account{
		Name:             "personal",
		IMAPHost:         "imap.example.com",
		SMTPHost:         "smtp.example.com",
		Username:         "you@example.com",
		Password:         config.Secret("hunter2"),
		WritebackEnabled: &shadowed,
	})
	if spec.WritebackEnabled == nil || *spec.WritebackEnabled {
		t.Errorf("WritebackEnabled = %v, want a pointer to false", spec.WritebackEnabled)
	}
}
