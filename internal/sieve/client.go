// SPDX-License-Identifier: AGPL-3.0-only

package sieve

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/egress"
)

// DefaultPort is the IANA ManageSieve port (RFC 5804 §1.8), on which the
// session starts in cleartext and MUST be upgraded with STARTTLS.
const DefaultPort = 4190

// DefaultTimeout bounds one whole session (connect, TLS, auth, every command).
const DefaultTimeout = 30 * time.Second

// Config addresses and authenticates one ManageSieve session.
type Config struct {
	Host     string
	Port     int
	Username string
	Password string

	// ImplicitTLS dials TLS immediately. Otherwise the session starts in
	// cleartext and MUST be upgraded via STARTTLS before AUTHENTICATE.
	ImplicitTLS bool

	// AllowPrivateHosts is MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS (#527): the
	// same switch that governs the account's IMAP and SMTP dials.
	AllowPrivateHosts bool
	// Resolver is the DNS seam for the egress guard; nil is the system resolver.
	Resolver egress.Resolver

	// Timeout bounds the whole session. Zero means DefaultTimeout.
	Timeout time.Duration

	// TLSConfig overrides the TLS settings (tests trust their own CA with it;
	// production leaves it nil and gets ServerName=Host, TLS 1.2 minimum).
	TLSConfig *tls.Config
	// AllowInsecureNoTLS permits authenticating over cleartext when the server
	// offers no STARTTLS. ONLY for the in-process test server; never an
	// operator-facing mode.
	AllowInsecureNoTLS bool
}

// Capabilities is the server's capability listing (RFC 5804 §1.7).
type Capabilities struct {
	// Implementation is the server's self-description ("Dovecot Pigeonhole").
	Implementation string
	// Version is the ManageSieve protocol version ("1.0").
	Version string
	// Sieve is the set of Sieve extensions the server supports, lower-cased
	// and sorted ("fileinto", "vacation", "include", ...).
	Sieve []string
	// SASL is the set of authentication mechanisms, upper-cased.
	SASL []string
	// StartTLS reports whether STARTTLS is offered (only meaningful before TLS).
	StartTLS bool
	// Other is every other capability, by upper-cased name, with its value
	// ("NOTIFY": "mailto", "MAXREDIRECTS": "4", "OWNER": ...).
	Other map[string]string
}

// HasSieve reports whether the server supports the named Sieve extension.
func (c Capabilities) HasSieve(ext string) bool {
	ext = strings.ToLower(ext)
	for _, e := range c.Sieve {
		if e == ext {
			return true
		}
	}
	return false
}

// Script is one LISTSCRIPTS entry.
type Script struct {
	Name   string
	Active bool
}

// Reader is the read-only ManageSieve surface the rest of Mailroom programs
// against (spec §3 anti-corruption layer). *Session implements it; a test can
// substitute its own.
type Reader interface {
	Capabilities() Capabilities
	// PreAuthCapabilities is the listing the server gave before authentication
	// (after TLS). Some servers advertise more once logged in.
	PreAuthCapabilities() Capabilities
	// TLS reports the negotiated TLS version name, or "" for cleartext.
	TLS() string
	ListScripts(ctx context.Context) ([]Script, error)
	GetScript(ctx context.Context, name string) (string, error)
	Logout(ctx context.Context) error
	Close() error
}

// readVerbs is EVERY verb this package may send. A verb not in this set is
// refused by Session.do before anything is written — see the package doc.
// PUTSCRIPT, SETACTIVE, DELETESCRIPT, RENAMESCRIPT, CHECKSCRIPT and HAVESPACE
// are deliberately absent.
var readVerbs = map[string]bool{
	"CAPABILITY":   true,
	"STARTTLS":     true,
	"AUTHENTICATE": true,
	"LISTSCRIPTS":  true,
	"GETSCRIPT":    true,
	"LOGOUT":       true,
}

// ErrVerbNotPermitted is returned when anything asks a session to send a verb
// outside its allowlist (readVerbs for Session, writeSessionVerbs for
// WriteSession).
var ErrVerbNotPermitted = errors.New("sieve: verb not permitted on this client")

// ErrNoTLS is returned when the server offers neither implicit TLS nor
// STARTTLS: credentials are never sent in cleartext.
var ErrNoTLS = errors.New("sieve: server offers no TLS; refusing to send credentials over a cleartext connection")

// ErrNoPlainAuth is returned when the server does not offer SASL PLAIN, the
// only mechanism this client implements.
var ErrNoPlainAuth = errors.New("sieve: server does not offer SASL PLAIN")

// ErrHostNotPermitted wraps egress.ErrBlockedAddress for a host the egress
// policy refuses, mirroring provider.ErrHostNotPermitted.
var ErrHostNotPermitted = errors.New("sieve: host is not a permitted connection target")

// conn is one authenticated ManageSieve connection. It is UNEXPORTED and holds
// the verb allowlist it was opened with: Session opens it with readVerbs and
// WriteSession with writeSessionVerbs, and do refuses anything outside the set
// it carries. That is what keeps the two types' guarantees separate while they
// share every byte of protocol code.
type conn struct {
	nc      net.Conn
	rd      *reader
	caps    Capabilities
	preAuth Capabilities
	tlsName string
	verbs   map[string]bool
}

// Session is one authenticated, read-only ManageSieve session.
type Session struct{ c *conn }

var _ Reader = (*Session)(nil)

// Open dials, upgrades to TLS, authenticates with SASL PLAIN and returns a
// session ready for read commands. The context's deadline (or cfg.Timeout)
// bounds the whole session, not only the open.
func Open(ctx context.Context, cfg Config) (*Session, error) {
	c, err := openConn(ctx, cfg, readVerbs)
	if err != nil {
		return nil, err
	}
	return &Session{c: c}, nil
}

func openConn(ctx context.Context, cfg Config, verbs map[string]bool) (*conn, error) {
	if cfg.Host == "" {
		return nil, errors.New("sieve: no host")
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	d := &egress.Dialer{AllowPrivate: cfg.AllowPrivateHosts, Resolver: cfg.Resolver, Timeout: timeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if errors.Is(err, egress.ErrBlockedAddress) {
			return nil, fmt.Errorf("%w: %s (%w). If this instance must reach a mail server on "+
				"a private network, set MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS=1", ErrHostNotPermitted, addr, err)
		}
		return nil, fmt.Errorf("sieve: dial %s: %w", addr, err)
	}
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = nc.SetDeadline(deadline)

	tlsCfg := cfg.TLSConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	}

	s := &conn{nc: nc, rd: newReader(nc), verbs: verbs}
	if cfg.ImplicitTLS {
		if err := s.upgrade(ctx, tlsCfg); err != nil {
			_ = nc.Close()
			return nil, err
		}
	}

	// The greeting is a capability listing followed by OK.
	greet, err := s.rd.readResponse(smallResponseBudget)
	if err != nil {
		_ = s.nc.Close()
		return nil, fmt.Errorf("sieve: greeting from %s: %w", addr, err)
	}
	if greet.status != "OK" {
		_ = s.nc.Close()
		return nil, &ServerError{Verb: "greeting", Status: greet.status, Code: greet.code, Text: greet.text}
	}
	s.caps = parseCapabilities(greet.data)

	if s.tlsName == "" {
		if s.caps.StartTLS {
			if _, err := s.do("STARTTLS"); err != nil {
				_ = s.nc.Close()
				return nil, err
			}
			if err := s.upgrade(ctx, tlsCfg); err != nil {
				_ = s.nc.Close()
				return nil, err
			}
			// RFC 5804 §2.2: after TLS the server MUST re-issue its capabilities.
			after, err := s.rd.readResponse(smallResponseBudget)
			if err != nil {
				_ = s.nc.Close()
				return nil, fmt.Errorf("sieve: capabilities after STARTTLS: %w", err)
			}
			if after.status != "OK" {
				_ = s.nc.Close()
				return nil, &ServerError{Verb: "STARTTLS", Status: after.status, Code: after.code, Text: after.text}
			}
			s.caps = parseCapabilities(after.data)
		} else if !cfg.AllowInsecureNoTLS {
			_ = s.nc.Close()
			return nil, fmt.Errorf("%w (%s)", ErrNoTLS, addr)
		}
	}
	s.preAuth = s.caps

	if !containsFold(s.caps.SASL, "PLAIN") {
		_ = s.nc.Close()
		return nil, fmt.Errorf("%w (offered: %s)", ErrNoPlainAuth, strings.Join(s.caps.SASL, " "))
	}
	ir := base64.StdEncoding.EncodeToString([]byte("\x00" + cfg.Username + "\x00" + cfg.Password))
	if _, err := s.do("AUTHENTICATE", "PLAIN", ir); err != nil {
		_ = s.nc.Close()
		// The error is the server's NO text; the credential is never in it.
		return nil, err
	}

	// Ask again once authenticated: servers advertise more (MAXREDIRECTS,
	// OWNER) after login, and this is the listing the report describes.
	resp, err := s.do("CAPABILITY")
	if err != nil {
		_ = s.nc.Close()
		return nil, err
	}
	s.caps = parseCapabilities(resp.data)
	return s, nil
}

func (s *conn) upgrade(ctx context.Context, cfg *tls.Config) error {
	tconn := tls.Client(s.nc, cfg)
	if err := tconn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("sieve: tls handshake: %w", err)
	}
	s.nc = tconn
	s.rd = newReader(tconn)
	s.tlsName = tls.VersionName(tconn.ConnectionState().Version)
	return nil
}

// do sends one command and reads its response. It is the ONLY writer to the
// connection, and it refuses any verb outside the connection's allowlist
// before writing. A NO or BYE is returned as a *ServerError.
func (s *conn) do(verb string, args ...string) (response, error) {
	if !s.verbs[verb] {
		return response{}, fmt.Errorf("%w: %s", ErrVerbNotPermitted, Printable(verb))
	}
	var sb strings.Builder
	sb.WriteString(verb)
	for _, a := range args {
		sb.WriteByte(' ')
		sb.WriteString(quote(a))
	}
	sb.WriteString("\r\n")
	if _, err := io.WriteString(s.nc, sb.String()); err != nil {
		return response{}, fmt.Errorf("sieve: %s: %w", verb, err)
	}
	budget := smallResponseBudget
	switch verb {
	case "GETSCRIPT":
		budget = scriptResponseBudget
	case "LISTSCRIPTS":
		budget = listResponseBudget
	}
	resp, err := s.rd.readResponse(budget)
	if err != nil {
		return response{}, fmt.Errorf("sieve: %s: %w", verb, err)
	}
	if resp.status != "OK" {
		return resp, &ServerError{Verb: verb, Status: resp.status, Code: resp.code, Text: resp.text}
	}
	return resp, nil
}

func (s *conn) listScripts() ([]Script, error) {
	resp, err := s.do("LISTSCRIPTS")
	if err != nil {
		return nil, err
	}
	if len(resp.data) > MaxScripts {
		return nil, fmt.Errorf("%w: LISTSCRIPTS returned %d entries, more than the %d accepted", ErrProtocol, len(resp.data), MaxScripts)
	}
	out := make([]Script, 0, len(resp.data))
	for _, ln := range resp.data {
		if len(ln) == 0 || ln[0].kind != tokString {
			return nil, fmt.Errorf("%w: LISTSCRIPTS line does not start with a name", ErrProtocol)
		}
		sc := Script{Name: ln[0].text}
		if len(ln) > 1 && ln[1].kind == tokAtom && strings.EqualFold(ln[1].text, "ACTIVE") {
			sc.Active = true
		}
		out = append(out, sc)
	}
	return out, nil
}

func (s *conn) getScript(name string) (string, error) {
	resp, err := s.do("GETSCRIPT", name)
	if err != nil {
		return "", err
	}
	if len(resp.data) != 1 || len(resp.data[0]) != 1 || resp.data[0][0].kind != tokString {
		return "", fmt.Errorf("%w: GETSCRIPT did not return exactly one string", ErrProtocol)
	}
	return resp.data[0][0].text, nil
}

func (s *conn) logout() error {
	_, err := s.do("LOGOUT")
	_ = s.nc.Close()
	return err
}

// do is the read session's single writer; see conn.do.
func (s *Session) do(verb string, args ...string) (response, error) { return s.c.do(verb, args...) }

// Capabilities returns the post-authentication capability listing.
func (s *Session) Capabilities() Capabilities { return s.c.caps }

// PreAuthCapabilities returns the listing given before authentication.
func (s *Session) PreAuthCapabilities() Capabilities { return s.c.preAuth }

// TLS returns the negotiated TLS version name ("TLS 1.3"), or "" in cleartext.
func (s *Session) TLS() string { return s.c.tlsName }

// ListScripts returns every script on the server, in the server's order.
func (s *Session) ListScripts(context.Context) ([]Script, error) { return s.c.listScripts() }

// GetScript returns one script's body, bounded by MaxScriptBytes.
func (s *Session) GetScript(_ context.Context, name string) (string, error) {
	return s.c.getScript(name)
}

// Logout ends the session politely. The connection is closed either way.
func (s *Session) Logout(context.Context) error { return s.c.logout() }

// Close closes the connection without LOGOUT.
func (s *Session) Close() error { return s.c.nc.Close() }

// parseCapabilities reads capability lines: "NAME" ["value"].
func parseCapabilities(data []line) Capabilities {
	c := Capabilities{Other: map[string]string{}}
	for _, ln := range data {
		if len(ln) == 0 || ln[0].kind != tokString {
			continue
		}
		name := strings.ToUpper(ln[0].text)
		val := ""
		if len(ln) > 1 && ln[1].kind == tokString {
			val = ln[1].text
		}
		switch name {
		case "IMPLEMENTATION":
			c.Implementation = Printable(val)
		case "VERSION":
			c.Version = Printable(val)
		case "SIEVE":
			for _, f := range strings.Fields(val) {
				c.Sieve = append(c.Sieve, strings.ToLower(Printable(f)))
			}
			sort.Strings(c.Sieve)
		case "SASL":
			for _, f := range strings.Fields(val) {
				c.SASL = append(c.SASL, strings.ToUpper(Printable(f)))
			}
		case "STARTTLS":
			c.StartTLS = true
		default:
			c.Other[Printable(name)] = Printable(val)
		}
	}
	return c
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
