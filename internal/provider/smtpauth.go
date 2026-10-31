// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/smtp"

	"mailroom/internal/xoauth2"
)

// SMTPAuth is how a submission authenticates (ADR-046). It is Mailroom's type,
// not net/smtp's: the only implementations are this package's, built with
// PasswordAuth or XOAuth2Auth, so net/smtp stays the private detail of this file
// and sender.go (spec §3).
type SMTPAuth interface {
	// mechanism names the SASL mechanism, for tests. Never secret.
	mechanism() string
	// client builds the net/smtp Auth for one conversation with host, the name
	// the connection was opened to (the mechanism refuses any other). For
	// XOAUTH2 this is where the access token is asked for, so its error is
	// the token source's (one of xoauth2's sentinels).
	client(ctx context.Context, host string) (smtp.Auth, error)
	// reject reports that the server refused the credential cl carried with a
	// 535, and says whether a second attempt could differ: true for XOAUTH2
	// (the refused token is dropped, so the next client call refreshes),
	// false for a password, which would only be refused again.
	reject(cl smtp.Auth) bool
}

// PasswordAuth is AUTH PLAIN with a username and password: exactly what every
// submission sent before ADR-046 (net/smtp's PlainAuth, which refuses a
// connection without TLS unless it is to localhost).
func PasswordAuth(username, password string) SMTPAuth {
	return passwordAuth{username: username, password: password}
}

type passwordAuth struct{ username, password string }

func (passwordAuth) mechanism() string { return "PLAIN" }

func (a passwordAuth) client(_ context.Context, host string) (smtp.Auth, error) {
	return smtp.PlainAuth("", a.username, a.password, host), nil
}

func (passwordAuth) reject(smtp.Auth) bool { return false }

// XOAuth2Auth is AUTH XOAUTH2 with access tokens from src, asked for at the
// moment the conversation authenticates (never cached here). A 535 hands the
// refused token back to src.Reject and the sender retries once (ADR-046 §5).
func XOAuth2Auth(username string, src xoauth2.TokenSource) SMTPAuth {
	return xoauth2SMTP{username: username, src: src}
}

// xoauth2SMTP holds no token itself, only the source; the client it builds
// holds the token as xoauth2.Held, never a bare Secret: those fields are
// unexported, and fmt renders unexported fields without calling their methods
// (the rule on xoauth2.Secret). The redacting methods below cover the cases
// where fmt and slog CAN call them (a SendRequest's exported Auth field, a log
// attribute).
type xoauth2SMTP struct {
	username string
	src      xoauth2.TokenSource
}

func (xoauth2SMTP) mechanism() string { return "XOAUTH2" }

func (a xoauth2SMTP) client(ctx context.Context, host string) (smtp.Auth, error) {
	if a.src == nil {
		return nil, errors.New("provider: XOAUTH2 without a token source")
	}
	tok, err := a.src.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("access token: %w", err)
	}
	return &xoauth2Client{username: a.username, token: xoauth2.Hold(tok), host: host}, nil
}

func (a xoauth2SMTP) reject(cl smtp.Auth) bool {
	xc, ok := cl.(*xoauth2Client)
	if !ok || a.src == nil {
		return false
	}
	a.src.Reject(xc.token.Get())
	return true
}

func (a xoauth2SMTP) String() string             { return "XOAUTH2(" + a.username + ", [redacted])" }
func (a xoauth2SMTP) GoString() string           { return a.String() }
func (a xoauth2SMTP) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(a.String())) }
func (a xoauth2SMTP) LogValue() slog.Value       { return slog.StringValue(a.String()) }

// errXOAuth2NoTLS / errXOAuth2WrongHost mirror net/smtp's PlainAuth refusals:
// a bearer token is as good as a password to whoever reads it.
var (
	errXOAuth2NoTLS     = errors.New("provider: refusing to send an OAuth token over an unencrypted connection")
	errXOAuth2WrongHost = errors.New("provider: wrong host name for XOAUTH2")
	errXOAuth2Mechanism = errors.New("provider: server does not offer AUTH XOAUTH2")
)

// xoauth2Client is the net/smtp Auth for XOAUTH2.
type xoauth2Client struct {
	username string
	token    xoauth2.Held
	host     string
}

func (c *xoauth2Client) String() string {
	return "XOAUTH2(" + c.username + "@" + c.host + ", [redacted])"
}
func (c *xoauth2Client) GoString() string           { return c.String() }
func (c *xoauth2Client) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }
func (c *xoauth2Client) LogValue() slog.Value       { return slog.StringValue(c.String()) }

// Start implements smtp.Auth. Its refusals mirror smtp.PlainAuth exactly: TLS
// is required unless the server is localhost (the in-process test servers),
// and the server name must be the host the token was meant for.
func (c *xoauth2Client) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !isLocalhost(server.Name) {
		return "", nil, errXOAuth2NoTLS
	}
	if server.Name != c.host {
		return "", nil, errXOAuth2WrongHost
	}
	if !offers(server.Auth, "XOAUTH2") {
		return "", nil, errXOAuth2Mechanism
	}
	ir, err := xoauth2.InitialResponse(c.username, c.token.Get())
	if err != nil {
		return "", nil, err
	}
	return "XOAUTH2", ir, nil
}

// Next implements smtp.Auth. The only challenge XOAUTH2 defines is the failure
// challenge (334 + base64 JSON), and it must be answered with an EMPTY response
// so the server completes AUTH with its 535. Nothing from the challenge is
// returned or logged.
func (c *xoauth2Client) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return []byte{}, nil
	}
	return nil, nil
}

func isLocalhost(name string) bool {
	return name == "localhost" || name == "127.0.0.1" || name == "::1"
}

func offers(mechs []string, want string) bool {
	for _, m := range mechs {
		if m == want {
			return true
		}
	}
	return false
}
