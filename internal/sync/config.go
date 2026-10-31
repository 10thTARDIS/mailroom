// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"mailroom/internal/egress"
)

// TLSMode selects how Dial establishes transport security.
type TLSMode int

const (
	// TLSImplicit dials with TLS from the first byte (IMAPS, typically port
	// 993). This is the default and the expected mode for real accounts.
	TLSImplicit TLSMode = iota
	// TLSStartTLS dials in plaintext (typically port 143) and upgrades via
	// STARTTLS before authenticating.
	TLSStartTLS
	// TLSInsecure never negotiates TLS. Only ever appropriate against a
	// local/test IMAP server (the Dovecot harness, issue #4) — never use
	// this against a real account.
	TLSInsecure
)

// String implements fmt.Stringer.
func (m TLSMode) String() string {
	switch m {
	case TLSImplicit:
		return "implicit"
	case TLSStartTLS:
		return "starttls"
	case TLSInsecure:
		return "insecure"
	default:
		return fmt.Sprintf("TLSMode(%d)", int(m))
	}
}

// defaultDialTimeout bounds the TCP connect phase when Config.DialTimeout
// is unset.
const defaultDialTimeout = 30 * time.Second

// Config holds connection parameters for a single IMAP account (spec §4:
// "Account = one IMAP/SMTP login"). It is mailroom's own type — go-imap
// never appears here.
type Config struct {
	Host string
	Port int
	// Username/Password are the PASSWORD mechanism: LOGIN, used when Auth is
	// nil. Every password account takes this path, unchanged by ADR-046.
	Username string
	Password string
	TLS      TLSMode

	// Auth, when set, replaces LOGIN with another mechanism (XOAuth2Auth for
	// OAuth accounts, ADR-046); Username and Password must then be empty, so a
	// Config can never say two different things about how it signs in.
	Auth Auth

	// ServerName overrides the hostname used for TLS certificate
	// verification (SNI). Empty means use Host; mainly useful for test
	// harnesses that connect by IP.
	ServerName string

	// DialTimeout bounds the connect phase: resolution, the TCP connect (spread
	// across the host's addresses) and, for TLSImplicit, the TLS handshake —
	// the same span tls.DialWithDialer's timeout covered before the dial was
	// guarded. Zero means defaultDialTimeout.
	DialTimeout time.Duration

	// InsecureSkipVerify disables TLS certificate verification. Only ever
	// set true in test harnesses against ephemeral self-signed servers.
	InsecureSkipVerify bool

	// SearchLimiter paces every UID SEARCH this session issues — LiveUIDs'
	// observation and writeback's SearchMessageID/UIDPresent, the three
	// UIDSearch call sites in session.go (#638).
	//
	// NIL MEANS NO PACING, and that is the default: tests, the Dovecot harness,
	// and any deployment against a server with no SEARCH ceiling behave exactly
	// as they did before this field existed. Validate does not require it.
	//
	// The server's budget is per ACCOUNT, not per connection, so every session
	// built for one account must be handed the SAME Limiter value — see
	// syncrunner.CredentialResolver, which owns the per-account registry.
	SearchLimiter Limiter

	// AllowPrivateHosts lifts the egress address policy on this session's dial:
	// MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS (#527), for a mail server that lives
	// on the instance's own network.
	//
	// ⚠ FALSE IS THE GUARDED VALUE, and it is the zero value on purpose. Dial
	// resolves Host once, refuses it unless EVERY address it resolves to is
	// publicly routable (egress.CheckAddress), and connects to the checked IP
	// literal — so a host that resolved public when the account was validated
	// and resolves to 127.0.0.1 or 169.254.169.254 now (DNS rebinding) is
	// refused before any connection is made, rather than handed the account's
	// password in LOGIN (#625 §3). A caller that forgets to set this fails
	// CLOSED: a LAN account stops syncing with an error naming this switch.
	// syncrunner.CredentialResolver and onboarding set it from config.
	AllowPrivateHosts bool

	// rootCAs replaces the system roots for certificate verification. It is
	// unexported on purpose: only this package's tests set it, to reach a
	// local server over VERIFIED TLS (an OAuth Config refuses TLSInsecure and
	// InsecureSkipVerify).
	rootCAs *x509.CertPool

	// Resolver is the DNS seam for that guard; nil means the system resolver.
	// Tests inject a scripted one to make rebinding observable without DNS.
	Resolver egress.Resolver
}

// Validate checks that cfg is usable, without performing any I/O.
func (c Config) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("sync: config: host is required")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("sync: config: port %d out of range", c.Port)
	}
	if c.Auth != nil {
		if c.Username != "" || c.Password != "" {
			return fmt.Errorf("sync: config: set Auth or Username/Password, not both")
		}
		if err := c.Auth.validate(); err != nil {
			return err
		}
		// A bearer token is as good as a password to whoever reads it: an
		// OAuth session is never allowed without verified TLS. (Tests reach
		// their own servers through a trusted test CA instead.)
		if c.TLS == TLSInsecure || c.InsecureSkipVerify {
			return fmt.Errorf("sync: config: %s requires verified TLS (no TLSInsecure, no InsecureSkipVerify)", c.Auth.mechanism())
		}
	} else {
		if c.Username == "" {
			return fmt.Errorf("sync: config: username is required")
		}
		if c.Password == "" {
			return fmt.Errorf("sync: config: password is required")
		}
	}
	switch c.TLS {
	case TLSImplicit, TLSStartTLS, TLSInsecure:
	default:
		return fmt.Errorf("sync: config: invalid TLS mode %v", c.TLS)
	}
	if c.DialTimeout < 0 {
		return fmt.Errorf("sync: config: dial timeout must not be negative")
	}
	return nil
}

// authenticate signs in on a connected client: Auth's mechanism when set,
// otherwise LOGIN with Username/Password (the password path, byte-for-byte what
// Dial always sent).
func (c Config) authenticate(ctx context.Context, client *imapclient.Client) error {
	if c.Auth != nil {
		return c.Auth.authenticate(ctx, client)
	}
	return client.Login(c.Username, c.Password).Wait()
}

// addr returns the "host:port" dial target.
func (c Config) addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// tlsServerName returns the hostname to use for TLS certificate
// verification / SNI.
func (c Config) tlsServerName() string {
	if c.ServerName != "" {
		return c.ServerName
	}
	return c.Host
}

// dialTimeout returns DialTimeout, or defaultDialTimeout when unset.
func (c Config) dialTimeout() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return defaultDialTimeout
}
