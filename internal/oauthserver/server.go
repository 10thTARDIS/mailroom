// SPDX-License-Identifier: AGPL-3.0-only

// Package oauthserver is Mailroom's OAuth 2.1 authorization server for /mcp
// (ADR-050). A grant is an API key row of kind 'oauth', so everything an
// approved client may do is decided by the gate ADR-042 already enforces; this
// package only issues, rotates and revokes the tokens that resolve to it.
//
// It owns no database access: everything goes through internal/store.
package oauthserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// MCPResourcePath is the resource the tokens are issued for.
const MCPResourcePath = "/mcp"

// Token lifetimes (ADR-050 section 3, section 5).
const (
	AccessTokenTTL  = time.Hour
	RefreshTokenTTL = 30 * 24 * time.Hour
	CodeTTL         = 60 * time.Second
)

// Policy is the key policy the consent screen presents: the closed scope set
// and the limit bounds ApiKey/set enforces. cmd/mailroom fills it from
// internal/api so there is one source.
type Policy struct {
	Scopes         []string // the closed set, in display order
	Destructive    []string // scopes that may wait for approval
	DefaultRate    int
	MaxRate        int
	DefaultBudget  int
	MaxBudget      int
	DefaultSuspend bool
	// DefaultScopes is what an authorization request that names no scope asks
	// for, and what the protected-resource metadata advertises.
	DefaultScopes []string
}

// GrantApproval is what a consent decision asks the grant writer to store.
type GrantApproval struct {
	ClientID, ClientName, ClientKind, RedirectHost string
	Scopes                                         []string
	ConfirmScopes                                  []string
	// AccountIDs nil means every account.
	AccountIDs       []string
	RatePerMinute    int
	DailyBudget      int
	SuspendOnAnomaly bool
	// AcknowledgeSendAndDelete is set only when the user typed the confirmation.
	AcknowledgeSendAndDelete bool
}

// ApproveFunc creates or updates the grant for the signed-in user inside tx,
// through the same validation and audit ApiKey/set uses (internal/api). A
// non-empty refusal is a user-facing reason; err is a server fault.
type ApproveFunc func(ctx context.Context, tx *store.UserTx, in GrantApproval) (key store.APIKey, refusal string, err error)

// Config configures a Server.
type Config struct {
	// Issuer is the origin (https://host[:port]); the resource is Issuer+/mcp.
	Issuer string
	Store  *store.Store
	Policy Policy
	// Approve writes the grant. Required.
	Approve ApproveFunc
	// Alert pushes the refresh-reuse anomaly to the user. Optional.
	Alert func(userID ulid.ULID, key store.APIKey)
	// CIMD is the metadata fetcher; nil builds the guarded one from
	// AllowPrivateClientHosts.
	CIMD Doer
	// CIMDTimeout bounds one metadata fetch; zero means 5 seconds.
	CIMDTimeout             time.Duration
	AllowPrivateClientHosts bool
	// DCR is MAILROOM_OAUTH_DCR. Parsed now; the endpoint arrives later, so the
	// metadata never advertises a registration endpoint in this release.
	DCR     bool
	Limiter *auth.FailureLimiter
	Logger  *slog.Logger
}

// Server is the authorization server.
type Server struct {
	issuer   string
	resource string
	st       *store.Store
	policy   Policy
	approve  ApproveFunc
	alert    func(ulid.ULID, store.APIKey)
	cimd     Doer
	cimdTTO  time.Duration
	limiter  *auth.FailureLimiter
	// authLimiter is charged on every authorization request: the endpoint is
	// reachable by anyone and may cause one outbound metadata fetch.
	authLimiter *auth.FailureLimiter
	log         *slog.Logger
}

// New builds a Server.
func New(c Config) (*Server, error) {
	u, err := url.Parse(c.Issuer)
	if err != nil || u.Host == "" || u.Path != "" {
		return nil, fmt.Errorf("oauthserver: issuer %q must be an origin", c.Issuer)
	}
	if c.Store == nil || c.Approve == nil {
		return nil, errors.New("oauthserver: a store and a grant writer are required")
	}
	s := &Server{issuer: c.Issuer, resource: c.Issuer + MCPResourcePath, st: c.Store, policy: c.Policy,
		approve: c.Approve, alert: c.Alert, cimd: c.CIMD, limiter: c.Limiter, log: c.Logger}
	s.cimdTTO = c.CIMDTimeout
	if s.cimdTTO <= 0 {
		s.cimdTTO = cimdTimeout
	}
	if s.cimd == nil {
		s.cimd = NewCIMDClient(c.AllowPrivateClientHosts)
	}
	if s.limiter == nil {
		s.limiter = auth.NewFailureLimiter(0, 0)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.authLimiter = auth.NewFailureLimiter(30, 2*time.Second)
	return s, nil
}

// UseLimiter makes the token and revocation endpoints charge their failures to
// l, the bearer gate's per-address failure limiter, so a guesser has ONE budget
// across both (ADR-050 section 6).
func (s *Server) UseLimiter(l *auth.FailureLimiter) {
	if l != nil {
		s.limiter = l
	}
}

// Resource is the canonical resource indicator.
func (s *Server) Resource() string { return s.resource }

// MetadataURL is the protected-resource metadata URL for /mcp.
func (s *Server) MetadataURL() string {
	return s.issuer + "/.well-known/oauth-protected-resource" + MCPResourcePath
}

// Challenge is the WWW-Authenticate value of a refused /mcp request.
func (s *Server) Challenge() string {
	return `Bearer realm="mailroom", resource_metadata="` + s.MetadataURL() + `", scope="` +
		strings.Join(s.policy.DefaultScopes, " ") + `"`
}

// NotFound is the handler of every OAuth route when the surface is off.
func NotFound() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
}

// Client is a resolved OAuth client.
type Client struct {
	ID, Name, Kind string
	RedirectURIs   []string
	OwnerID        *ulid.ULID
}

// errClient is a client that cannot be used; its text is safe to show.
type errClient struct{ msg string }

func (e errClient) Error() string { return e.msg }

// resolveClient decides by SHAPE first: an https URL is a metadata-document
// client and is never looked up among registered clients; a minted id
// (mc_/rc_ plus 32 hex) is looked up only there; anything else is unknown.
func (s *Server) resolveClient(ctx context.Context, id string, allowFetch func() bool) (Client, error) {
	switch {
	case id == "":
		return Client{}, errClient{"The application did not say who it is (client_id is missing)."}
	case IsCIMDClientID(id):
		return s.resolveCIMD(ctx, id, allowFetch)
	case manualClientID.MatchString(id):
		c, found, err := s.st.ResolveOAuthClient(ctx, id)
		if err != nil {
			return Client{}, err
		}
		if !found {
			return Client{}, errClient{"This application is not registered."}
		}
		return Client{ID: c.ID, Name: c.Name, Kind: c.Kind, RedirectURIs: c.RedirectURIs, OwnerID: c.OwnerID}, nil
	}
	return Client{}, errClient{"This application is not registered."}
}

func (s *Server) resolveCIMD(ctx context.Context, id string, allowFetch func() bool) (Client, error) {
	if _, err := validCIMDURL(id); err != nil {
		return Client{}, errClient{"This application's identity could not be verified: " + err.Error() + "."}
	}
	if raw, _, found, err := s.st.GetCIMDDocument(ctx, id); err != nil {
		return Client{}, err
	} else if found {
		var d cimdDocument
		if json.Unmarshal(raw, &d) == nil && d.Name != "" && len(d.RedirectURIs) > 0 {
			return Client{ID: id, Name: d.Name, Kind: store.OAuthClientCIMD, RedirectURIs: d.RedirectURIs}, nil
		}
	}
	// A cache miss causes an outbound request: that is charged.
	if allowFetch != nil && !allowFetch() {
		return Client{}, errClient{"Too many applications were checked in a short time. Wait a moment and try again."}
	}
	doc, ttl, err := s.fetchCIMD(ctx, id)
	if err != nil {
		s.log.InfoContext(ctx, "oauth: client metadata refused", "client_id_host", hostOf(id), "err", err)
		return Client{}, errClient{"This application's identity could not be verified, so it cannot be approved."}
	}
	raw, _ := json.Marshal(doc)
	if _, err := s.st.PutCIMDDocument(ctx, id, raw, time.Now().Add(ttl)); err != nil {
		s.log.WarnContext(ctx, "oauth: could not cache client metadata", "err", err)
	}
	return Client{ID: id, Name: doc.Name, Kind: store.OAuthClientCIMD, RedirectURIs: doc.RedirectURIs}, nil
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// ---- small helpers ---------------------------------------------------------

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// randomSecret returns prefix plus 256 random bits, and its SHA-256.
func randomSecret(prefix string) (secret string, hash [32]byte, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", [32]byte{}, err
	}
	secret = prefix + strings.ToLower(b32.EncodeToString(buf))
	return secret, sha256.Sum256([]byte(secret)), nil
}

func hashOf(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }

// normalizeResource folds scheme and host case and ignores a trailing slash; a
// missing resource means the canonical one.
func (s *Server) resourceMatches(presented string) bool {
	if presented == "" {
		return true
	}
	u, err := url.Parse(presented)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return false
	}
	got := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimSuffix(u.EscapedPath(), "/")
	want := strings.ToLower(s.resource)
	return got == want
}

func clientIP(r *http.Request) string {
	h := r.RemoteAddr
	if i := strings.LastIndex(h, ":"); i > 0 {
		h = h[:i]
	}
	return strings.Trim(h, "[]")
}
