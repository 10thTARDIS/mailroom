// SPDX-License-Identifier: AGPL-3.0-only

package oauthserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"mailroom/internal/egress"
	"mailroom/internal/textcheck"
)

// Client ID Metadata Documents (ADR-050 section 4). A client_id that is an
// https URL with a path is fetched through the guarded egress client, read as
// JSON, validated, and cached.

const (
	cimdTimeout  = 5 * time.Second
	cimdMaxBytes = 16 << 10
	cimdMinTTL   = 5 * time.Minute
	cimdMaxTTL   = 24 * time.Hour
	cimdDefTTL   = time.Hour
	maxClientIDs = 2048
	maxNameRunes = 200
	maxClientURI = 10
)

var manualClientID = regexp.MustCompile(`^(mc|rc)_[0-9a-f]{32}$`)

// Doer is the HTTP client the metadata fetch uses: *egress.Client in
// production.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// NewCIMDClient builds the guarded client for metadata fetches. allowPrivate
// is MAILROOM_OAUTH_ALLOW_PRIVATE_CLIENT_HOSTS: it admits private, loopback and
// unique-local addresses and nothing else; link-local (the instance-metadata
// range), multicast and the rest of the table stay refused.
func NewCIMDClient(allowPrivate bool) *egress.Client {
	p := egress.Policy{Timeout: cimdTimeout, MaxBytes: cimdMaxBytes, MaxRedirects: -1}
	if allowPrivate {
		p.Exempt = privateOK
	}
	return egress.New(p)
}

var privateOKPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("fc00::/7"),
}

func privateOK(a netip.Addr) bool {
	for _, p := range privateOKPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// IsCIMDClientID reports whether id is shaped as a metadata-document client_id:
// an https URL. Such an id is ONLY ever a CIMD client; it is never looked up
// among registered clients.
func IsCIMDClientID(id string) bool { return strings.HasPrefix(id, "https://") }

// validCIMDURL checks the client_id URL: https, a host, a real path, no
// fragment, userinfo, query-free dot segments.
func validCIMDURL(id string) (*url.URL, error) {
	if len(id) > maxClientIDs {
		return nil, errors.New("client_id is too long")
	}
	for _, c := range id {
		if c < 0x21 || c == 0x7f || c == '\\' {
			return nil, errors.New("client_id has unusable characters")
		}
	}
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || strings.Contains(id, "#") {
		return nil, errors.New("client_id must be an https URL without a fragment")
	}
	if u.Path == "" || u.Path == "/" {
		return nil, errors.New("client_id must have a path")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return nil, errors.New("client_id must not contain dot segments")
		}
	}
	return u, nil
}

// cimdDocument is the part of a fetched document Mailroom keeps.
type cimdDocument struct {
	Name         string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// fetchCIMD fetches and validates the document at clientID.
func (s *Server) fetchCIMD(ctx context.Context, clientID string) (cimdDocument, time.Duration, error) {
	u, err := validCIMDURL(clientID)
	if err != nil {
		return cimdDocument{}, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cimdTTO)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return cimdDocument{}, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.cimd.Do(req)
	if err != nil {
		return cimdDocument{}, 0, fmt.Errorf("could not fetch the client metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return cimdDocument{}, 0, fmt.Errorf("the client metadata URL answered %d", resp.StatusCode)
	}
	if mt, _, perr := mime.ParseMediaType(resp.Header.Get("Content-Type")); perr != nil ||
		(mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
		return cimdDocument{}, 0, errors.New("the client metadata is not JSON")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		return cimdDocument{}, 0, fmt.Errorf("could not read the client metadata: %w", err)
	}
	if len(body) > cimdMaxBytes {
		return cimdDocument{}, 0, errors.New("the client metadata is too large")
	}
	doc, err := parseCIMD(clientID, body)
	if err != nil {
		return cimdDocument{}, 0, err
	}
	return doc, cacheTTL(resp.Header.Get("Cache-Control")), nil
}

// parseCIMD validates a document against its client_id.
func parseCIMD(clientID string, body []byte) (cimdDocument, error) {
	var raw struct {
		ClientID   string   `json:"client_id"`
		Name       string   `json:"client_name"`
		Redirects  []string `json:"redirect_uris"`
		AuthMethod *string  `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return cimdDocument{}, errors.New("the client metadata is not valid JSON")
	}
	if raw.ClientID != clientID {
		return cimdDocument{}, errors.New("the client metadata names a different client_id")
	}
	if err := textcheck.CheckName(raw.Name); err != nil {
		return cimdDocument{}, errors.New("the client metadata's client_name " + err.Error())
	}
	name := cleanName(raw.Name)
	if name == "" {
		return cimdDocument{}, errors.New("the client metadata has no client_name")
	}
	if len(raw.Redirects) == 0 || len(raw.Redirects) > maxClientURI {
		return cimdDocument{}, errors.New("the client metadata needs between 1 and 10 redirect_uris")
	}
	for _, r := range raw.Redirects {
		if err := ValidateRedirectURI(r); err != nil {
			return cimdDocument{}, fmt.Errorf("a redirect_uri in the client metadata is not acceptable: %w", err)
		}
	}
	if raw.AuthMethod != nil && *raw.AuthMethod != "none" {
		return cimdDocument{}, errors.New("only public clients (token_endpoint_auth_method none) are supported")
	}
	return cimdDocument{Name: name, RedirectURIs: raw.Redirects}, nil
}

// cleanName strips control and format characters and bounds the length.
func cleanName(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > maxNameRunes {
		s = string(r[:maxNameRunes])
	}
	return s
}

// cacheTTL reads max-age from a Cache-Control header, clamped to 5 minutes to
// 24 hours; no-store and no-cache take the minimum, no header the default.
func cacheTTL(h string) time.Duration {
	ttl := cimdDefTTL
	for _, d := range strings.Split(strings.ToLower(h), ",") {
		d = strings.TrimSpace(d)
		switch {
		case d == "no-store" || d == "no-cache":
			ttl = cimdMinTTL
		case strings.HasPrefix(d, "max-age="):
			if n, err := strconv.Atoi(strings.TrimPrefix(d, "max-age=")); err == nil && n >= 0 {
				ttl = time.Duration(n) * time.Second
			}
		}
	}
	return min(max(ttl, cimdMinTTL), cimdMaxTTL)
}
