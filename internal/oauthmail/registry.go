// SPDX-License-Identifier: AGPL-3.0-only

// Package oauthmail is Mailroom's mail-provider OAuth (#52, ADR-046): the
// provider registry, the refresh client for a provider's token endpoint, and
// the per-account token Manager that the sync and send paths ask for access
// tokens.
//
// The token model, in one place:
//
//   - The REFRESH token is sealed inside app.accounts.credentials_ct
//     (onboarding.Credentials.OAuth), next to where password accounts keep
//     their password, so there is one sealed-secret location (ADR-013).
//   - The queryable half of the grant — provider, subject, scopes, state, the
//     rotation generation — is app.oauth_grants (store.OAuthGrant).
//   - ACCESS tokens live only in this process's memory (Manager), are never
//     persisted, and are refreshed five minutes before they expire. After a
//     restart the first connection pays one HTTPS round trip.
//   - Every token travels as xoauth2.Secret, which renders "[redacted]"
//     through every formatting path.
//
// Provider hosts — authorization, token, IMAP and SMTP — are FIXED by the
// registry. A user can never point a bearer token at a host they typed.
package oauthmail

import (
	"errors"
	"fmt"
	"regexp"
	"sort"

	"mailroom/internal/xoauth2"
)

// Provider names, as stored in app.oauth_grants.provider and used in the
// per-provider callback path (/oauth/callback/{name}).
const (
	ProviderGoogle    = "google"
	ProviderMicrosoft = "microsoft"
)

// Client is the operator's OAuth client registration for one provider.
type Client struct {
	ID     string
	Secret xoauth2.Secret
}

// Configured reports whether both halves are set. A provider is enabled only
// when its client is fully configured (ADR-046 §3).
func (c Client) Configured() bool { return c.ID != "" && !c.Secret.Empty() }

// Provider is one registry entry: data, not behaviour.
type Provider struct {
	// Name is ProviderGoogle or ProviderMicrosoft.
	Name string
	// AuthURL / TokenURL / RevokeURL are the provider's endpoints. RevokeURL is
	// empty when the provider has no programmatic revocation for this kind of
	// grant (Microsoft, ADR-046 §6).
	AuthURL   string
	TokenURL  string
	RevokeURL string
	// Scopes is exactly what the authorization request asks for.
	Scopes []string
	// RequiredScopes must all be granted; a partial grant is refused (Google's
	// granular consent lets a user untick the mail scope).
	RequiredScopes []string
	// RefreshSendsScope: include `scope` in the refresh request (Microsoft's
	// v2 endpoint expects it; Google does not need it).
	RefreshSendsScope bool
	// IMAP and SMTP endpoints, fixed. IMAP is always implicit TLS.
	IMAPHost        string
	IMAPPort        int
	SMTPHost        string
	SMTPPort        int
	SMTPImplicitTLS bool
	// Profile is the app.accounts.provider_profile an account gets.
	Profile string
	// AddressClaims are the ID-token claims, in order, that give the mailbox
	// address.
	AddressClaims []string
	// Client is the operator's registration.
	Client Client
	// Tenant is the Microsoft authority segment ("common", "consumers",
	// "organizations", a tenant id or domain); empty for Google. The ID-token
	// check narrows the accepted tenant by it (idtoken.go).
	Tenant string
}

// The scope URLs. https://mail.google.com/ is the only Google scope that
// grants IMAP and SMTP (it is "restricted"); Microsoft's two are delegated
// permissions limited to the mail protocols.
const (
	scopeGoogleMail    = "https://mail.google.com/"
	scopeMicrosoftIMAP = "https://outlook.office.com/IMAP.AccessAsUser.All"
	scopeMicrosoftSMTP = "https://outlook.office.com/SMTP.Send"
)

// Google is the registry entry for Gmail and Google Workspace.
func Google(c Client) Provider {
	return Provider{
		Name:           ProviderGoogle,
		AuthURL:        "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:       "https://oauth2.googleapis.com/token",
		RevokeURL:      "https://oauth2.googleapis.com/revoke",
		Scopes:         []string{"openid", "email", scopeGoogleMail},
		RequiredScopes: []string{scopeGoogleMail},
		IMAPHost:       "imap.gmail.com",
		IMAPPort:       993,
		SMTPHost:       "smtp.gmail.com",
		// Probed 2026-10-27: 465 implicit TLS advertises XOAUTH2.
		SMTPPort:        465,
		SMTPImplicitTLS: true,
		Profile:         "gmail",
		AddressClaims:   []string{"email"},
		Client:          c,
	}
}

// tenantPattern bounds the Microsoft tenant: "common", "consumers",
// "organizations", a GUID or a verified domain. It is interpolated into the
// endpoint URLs, so nothing that could change their shape is accepted.
var tenantPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)

// Microsoft is the registry entry for Outlook.com and Microsoft 365. tenant
// is "common" (personal accounts and any organization, a design decision
// for v1.96) unless the operator narrows it.
//
// SMTP: smtp.office365.com:587 with STARTTLS, which the 2026-10-27 probe saw
// advertise XOAUTH2. Microsoft's Outlook.com settings page names
// smtp-mail.outlook.com for consumer accounts; whether office365's host
// serves them too is settled in a live test (increment 0b), and
// the registry is the one place to change it.
func Microsoft(c Client, tenant string) (Provider, error) {
	if tenant == "" {
		tenant = "common"
	}
	if !tenantPattern.MatchString(tenant) {
		return Provider{}, fmt.Errorf("oauthmail: microsoft tenant %q is not a tenant name, id or domain", tenant)
	}
	base := "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/"
	return Provider{
		Name:              ProviderMicrosoft,
		AuthURL:           base + "authorize",
		TokenURL:          base + "token",
		Scopes:            []string{"openid", "email", "offline_access", scopeMicrosoftIMAP, scopeMicrosoftSMTP},
		RequiredScopes:    []string{scopeMicrosoftIMAP, scopeMicrosoftSMTP},
		RefreshSendsScope: true,
		IMAPHost:          "outlook.office365.com",
		IMAPPort:          993,
		SMTPHost:          "smtp.office365.com",
		SMTPPort:          587,
		SMTPImplicitTLS:   false,
		Profile:           "microsoft",
		AddressClaims:     []string{"email", "preferred_username"},
		Client:            c,
		Tenant:            tenant,
	}, nil
}

// Hosts returns the provider's fixed mail endpoints.
func (p Provider) Hosts() xoauth2.Hosts {
	return xoauth2.Hosts{IMAPHost: p.IMAPHost, IMAPPort: p.IMAPPort,
		SMTPHost: p.SMTPHost, SMTPPort: p.SMTPPort, SMTPImplicitTLS: p.SMTPImplicitTLS}
}

// Registry is the set of ENABLED providers. A provider whose client is not
// fully configured is absent: Lookup fails, the connect button is not offered
// (increment 3), and an existing grant for it is an operator error.
type Registry struct {
	byName map[string]Provider
}

// NewRegistry builds a registry from candidate providers, keeping those whose
// client is configured. Two entries with one name is a programming error.
func NewRegistry(candidates ...Provider) (*Registry, error) {
	r := &Registry{byName: map[string]Provider{}}
	for _, p := range candidates {
		if p.Name == "" || p.TokenURL == "" {
			return nil, errors.New("oauthmail: registry entry without a name or token URL")
		}
		if _, dup := r.byName[p.Name]; dup {
			return nil, fmt.Errorf("oauthmail: provider %q registered twice", p.Name)
		}
		if p.Client.Configured() {
			r.byName[p.Name] = p
		}
	}
	return r, nil
}

// Lookup returns the enabled provider named name.
func (r *Registry) Lookup(name string) (Provider, bool) {
	if r == nil {
		return Provider{}, false
	}
	p, ok := r.byName[name]
	return p, ok
}

// Enabled lists the enabled providers' names, sorted.
func (r *Registry) Enabled() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
