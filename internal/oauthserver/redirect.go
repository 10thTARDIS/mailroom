// SPDX-License-Identifier: AGPL-3.0-only

package oauthserver

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// Redirect URIs (ADR-050 section 4). A redirect URI is https, or http to a
// loopback host (RFC 8252 section 7.3). Anything else, private-use schemes
// included, is refused, as the MCP specification requires.

const maxRedirectURILen = 2048

// ErrRedirectURI is the reason a redirect URI is not acceptable.
var ErrRedirectURI = errors.New("redirect URI must be https, or http to localhost, 127.0.0.1 or [::1], with no fragment")

func isLoopbackHost(h string) bool {
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// ValidateRedirectURI reports whether raw is an acceptable redirect URI.
func ValidateRedirectURI(raw string) error {
	if raw == "" || len(raw) > maxRedirectURILen {
		return ErrRedirectURI
	}
	for _, c := range raw {
		if c < 0x21 || c == 0x7f || c == '\\' {
			return ErrRedirectURI
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || strings.Contains(raw, "#") {
		return ErrRedirectURI
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
	}
	return ErrRedirectURI
}

// MatchRedirectURI reports whether presented matches one of the registered
// URIs: exactly, except that a loopback URI matches on any port. A registered
// URI that is itself unacceptable never matches, and neither does an
// unacceptable presented one.
func MatchRedirectURI(registered []string, presented string) bool {
	if ValidateRedirectURI(presented) != nil {
		return false
	}
	p, err := url.Parse(presented)
	if err != nil {
		return false
	}
	for _, r := range registered {
		if ValidateRedirectURI(r) != nil {
			continue
		}
		if r == presented {
			return true
		}
		ru, err := url.Parse(r)
		if err != nil {
			continue
		}
		if ru.Scheme == "http" && isLoopbackHost(ru.Hostname()) && p.Scheme == "http" &&
			ru.Hostname() == p.Hostname() && ru.EscapedPath() == p.EscapedPath() && ru.RawQuery == p.RawQuery {
			return true
		}
	}
	return false
}

// RedirectHost is the host the consent screen shows for a redirect URI.
func RedirectHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return strings.ToLower(h)
}

// AllLoopback reports whether every URI is a loopback one.
func AllLoopback(uris []string) bool {
	if len(uris) == 0 {
		return false
	}
	for _, r := range uris {
		u, err := url.Parse(r)
		if err != nil || !isLoopbackHost(u.Hostname()) {
			return false
		}
	}
	return true
}
