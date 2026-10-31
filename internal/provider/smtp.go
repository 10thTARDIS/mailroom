// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SMTPCapabilities is the subset of advertised ESMTP extensions relevant to
// the provider profile (spec §18.1). Detected via EHLO, without authenticating
// or sending anything.
type SMTPCapabilities struct {
	// SMTPUTF8 reports RFC 6531 support — whether internationalized (EAI)
	// addresses can be sent. When false, compose must reject non-ASCII
	// addresses with a clear error (spec §18.1, issue #17).
	SMTPUTF8 bool
	// STARTTLS reports whether the server offered STARTTLS on the plaintext
	// port (before any upgrade).
	STARTTLS bool
	// TLS reports whether the capabilities were read over a TLS connection
	// (implicit TLS, or after a successful STARTTLS upgrade).
	TLS bool
	// Auth lists advertised SASL mechanisms (from the AUTH extension).
	Auth []string
	// Size is the SIZE extension's byte limit, or 0 if unadvertised.
	Size int64
}

type probeConfig struct {
	implicitTLS bool
	startTLS    bool
	tlsConfig   *tls.Config
	timeout     time.Duration
	// allowPrivate lifts the egress address policy on the probe's dial; see
	// WithAllowPrivateHosts.
	allowPrivate bool
}

// WithAllowPrivateHosts permits a host on a private, loopback or link-local
// address — MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS, for a probe of a server on
// the instance's own network. Without it the probe dial is guarded exactly as
// submission is (dialSMTP).
func WithAllowPrivateHosts() ProbeOption { return func(c *probeConfig) { c.allowPrivate = true } }

// ProbeOption configures ProbeSMTP.
type ProbeOption func(*probeConfig)

// WithImplicitTLS wraps the connection in TLS immediately (SMTPS, typically
// port 465) before the EHLO handshake.
func WithImplicitTLS() ProbeOption { return func(c *probeConfig) { c.implicitTLS = true } }

// WithoutSTARTTLS disables the STARTTLS upgrade on a plaintext connection.
// Capabilities are then read from the pre-TLS EHLO only. Primarily for
// probing servers (or tests) that do not offer STARTTLS.
func WithoutSTARTTLS() ProbeOption { return func(c *probeConfig) { c.startTLS = false } }

// WithTLSConfig sets the TLS configuration used for implicit TLS and STARTTLS.
func WithTLSConfig(t *tls.Config) ProbeOption { return func(c *probeConfig) { c.tlsConfig = t } }

// WithTimeout bounds the whole probe.
func WithTimeout(d time.Duration) ProbeOption { return func(c *probeConfig) { c.timeout = d } }

// ProbeSMTP connects to host:port, performs the ESMTP EHLO handshake (dropping
// STARTTLS to re-read post-upgrade capabilities unless disabled), and reports
// the advertised extensions. It never authenticates or sends mail. This is how
// Mailroom answers the SMTPUTF8/EAI question for a provider profile without a
// support ticket (spec §18.1, issue #17).
func ProbeSMTP(ctx context.Context, host string, port int, opts ...ProbeOption) (SMTPCapabilities, error) {
	cfg := probeConfig{startTLS: true, timeout: 15 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}

	dialCtx := ctx
	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := dialSMTP(dialCtx, cfg.allowPrivate, nil, addr)
	if err != nil {
		return SMTPCapabilities{}, err
	}
	// Bound the whole ESMTP conversation by the context deadline if present.
	if dl, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	overTLS := false
	if cfg.implicitTLS {
		tconn := tls.Client(conn, tlsConfigFor(cfg, host))
		if err := tconn.HandshakeContext(dialCtx); err != nil {
			_ = conn.Close()
			return SMTPCapabilities{}, fmt.Errorf("provider: tls handshake %s: %w", addr, err)
		}
		conn, overTLS = tconn, true
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return SMTPCapabilities{}, fmt.Errorf("provider: smtp greet %s: %w", addr, err)
	}
	defer func() { _ = client.Close() }()

	// NewClient already sent EHLO. Note STARTTLS as advertised on the
	// plaintext connection before any upgrade.
	hasStartTLS, _ := client.Extension("STARTTLS")
	if cfg.startTLS && !overTLS && hasStartTLS {
		if err := client.StartTLS(tlsConfigFor(cfg, host)); err != nil {
			return SMTPCapabilities{}, fmt.Errorf("provider: starttls %s: %w", addr, err)
		}
		overTLS = true // StartTLS re-issues EHLO; extensions now reflect the TLS session
	}

	caps := SMTPCapabilities{STARTTLS: hasStartTLS, TLS: overTLS}
	if ok, _ := client.Extension("SMTPUTF8"); ok {
		caps.SMTPUTF8 = true
	}
	if ok, mechs := client.Extension("AUTH"); ok {
		caps.Auth = strings.Fields(mechs)
	}
	if ok, sz := client.Extension("SIZE"); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(sz), 10, 64); err == nil {
			caps.Size = n
		}
	}
	return caps, nil
}

func tlsConfigFor(cfg probeConfig, host string) *tls.Config {
	if cfg.tlsConfig != nil {
		return cfg.tlsConfig
	}
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}
