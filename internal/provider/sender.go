// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/egress"
	"mailroom/internal/xoauth2"
)

// This file is Mailroom's SMTP SUBMISSION anti-corruption layer (spec §3): the
// only place net/smtp appears. Everything above it (internal/compose, the send
// job) talks to the Sender interface in Mailroom's own types, so swapping the
// SMTP client — or stubbing it in a test — changes nothing else.
//
// It never logs credentials or message bytes (CLAUDE.md): the only thing it
// surfaces on failure is the server's own status line, and even that is capped
// by the caller before it is persisted.

// SendRequest is one submission: the envelope, and the exact bytes to transmit.
type SendRequest struct {
	// Host/Port address the provider's submission service.
	Host string
	Port int

	// Auth authenticates the submission (ADR-046): PasswordAuth or
	// XOAuth2Auth. When nil, Username/Password are used as AUTH PLAIN, which is
	// what every caller did before the field existed; an empty Username with a
	// nil Auth sends no AUTH at all (the in-process test servers).
	Auth     SMTPAuth
	Username string
	Password string

	// ImplicitTLS dials TLS immediately (SMTPS, port 465 — what real accounts
	// use). Otherwise the connection starts in plaintext and MUST upgrade via
	// STARTTLS: see Send, which refuses to authenticate over cleartext.
	ImplicitTLS bool

	// MailFrom is the envelope sender. It has ALREADY been validated against
	// the user's verified identities (spec §5); this layer transmits, it does
	// not authorize.
	MailFrom string
	// RcptTo is the envelope recipient set (To + Cc + Bcc).
	RcptTo []string
	// Data is the RFC 5322 message, with any Bcc header already stripped.
	Data []byte

	// Timeout bounds the whole conversation. Zero means DefaultSendTimeout.
	Timeout time.Duration

	// TLSConfig overrides the TLS settings (tests use it; production leaves it
	// nil and gets ServerName=Host, TLS 1.2 minimum).
	TLSConfig *tls.Config
	// AllowInsecureNoTLS permits submission over a plaintext connection with no
	// STARTTLS. It exists ONLY for the in-process test server; a real provider
	// always offers TLS, and sending credentials without it is not a mode
	// Mailroom offers to an operator.
	AllowInsecureNoTLS bool
}

// DefaultSendTimeout bounds an SMTP submission.
const DefaultSendTimeout = 60 * time.Second

// SendResult reports a successful submission.
type SendResult struct {
	// Response is the provider's final status line (after DATA). It is what the
	// user sees as "delivery accepted by …", and it is what a bounce is later
	// correlated against. It never contains message content.
	Response string
	// SMTPUTF8 reports whether the server advertised RFC 6531 (EAI).
	SMTPUTF8 bool
}

// ErrPermanent marks a submission failure the provider will never accept on
// retry (a 5xx: bad recipient, rejected sender, message too large). The caller
// fails the submission and surfaces it, rather than burning retries and then
// dead-lettering an hour later.
var ErrPermanent = errors.New("provider: permanent submission failure")

// ErrEAIUnsupported is returned when an envelope address is non-ASCII, which
// needs SMTPUTF8 (RFC 6531).
//
// Spec §18.1 / issue #17 requires that such an address be rejected "with a clear
// error" when the SERVER does not support it. Mailroom currently rejects it in
// every case, because the CLIENT cannot do it either: net/smtp is frozen and has
// no way to emit the SMTPUTF8 parameter on MAIL FROM, so even a server that
// advertises the extension would be handed a non-conforming command. This is a
// clear, early error rather than a mangled or silently-dropped send. Lifting it
// means swapping the SMTP client behind this interface — which is exactly what
// the interface is for (spec §3).
var ErrEAIUnsupported = errors.New("provider: internationalized (non-ASCII) addresses need SMTPUTF8 (RFC 6531), which this SMTP client cannot emit")

// Sender submits a message to a provider's SMTP service. It is an interface so
// the send path unit-tests with no network.
type Sender interface {
	Send(ctx context.Context, req SendRequest) (SendResult, error)
}

// SMTPSender is the real Sender, over net/smtp.
//
// ITS ZERO VALUE IS THE GUARDED ONE. Every submission dial goes through an
// egress.Dialer: the account's SMTP host is resolved once, every resolved
// address must pass the egress address policy, and the checked IP literal is
// what gets dialled (#625 §3). The host was checked when the account was
// validated, but that answer is stale by the time mail is sent: a name that
// resolved public then can resolve to loopback or the metadata service now (DNS
// rebinding), and an unguarded dial would hand that address the account's
// password in the AUTH command. TLS still verifies against the HOSTNAME
// (ServerName), so the literal changes where the bytes go, never whose
// certificate is accepted.
type SMTPSender struct {
	// AllowPrivateHosts is MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS (#527): permit
	// an SMTP host on a private, loopback or link-local address. The same
	// switch governs account validation, so validation and use apply ONE rule.
	AllowPrivateHosts bool
	// Resolver is the DNS seam for the guard; nil means the system resolver.
	Resolver egress.Resolver
}

var _ Sender = SMTPSender{}

// Send performs one ESMTP submission: connect, upgrade to TLS, authenticate,
// MAIL FROM / RCPT TO / DATA.
//
// TLS is not optional. Credentials go over the wire in the AUTH command, so a
// connection that is neither implicit-TLS nor successfully STARTTLS-upgraded is
// abandoned rather than authenticated (the one exception, AllowInsecureNoTLS,
// exists for the in-process test server and is never set in production).
func (s SMTPSender) Send(ctx context.Context, req SendRequest) (SendResult, error) {
	if req.MailFrom == "" {
		return SendResult{}, errors.New("provider: submission has no MAIL FROM")
	}
	if len(req.RcptTo) == 0 {
		return SendResult{}, errors.New("provider: submission has no recipients")
	}
	ctx, cancel := withSendTimeout(ctx, req.Timeout)
	defer cancel()

	client, addr, err := s.open(ctx, req)
	if err != nil {
		return SendResult{}, err
	}
	// A closure over the variable, not the value: authenticate's one retry
	// replaces client with a fresh connection, and that is the one to close.
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()

	utf8OK, _ := client.Extension("SMTPUTF8")
	if needsEAI(req.MailFrom, req.RcptTo) {
		return SendResult{}, fmt.Errorf("%w (%s)", ErrEAIUnsupported, addr)
	}

	if client, err = s.authenticate(ctx, client, req); err != nil {
		return SendResult{}, err
	}

	if err := client.Mail(req.MailFrom); err != nil {
		return SendResult{}, classifySMTP(fmt.Errorf("provider: mail from: %w", err), err)
	}
	for _, rcpt := range req.RcptTo {
		if err := client.Rcpt(rcpt); err != nil {
			return SendResult{}, classifySMTP(fmt.Errorf("provider: rcpt to: %w", err), err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return SendResult{}, classifySMTP(fmt.Errorf("provider: data: %w", err), err)
	}
	if _, err := w.Write(req.Data); err != nil {
		_ = w.Close()
		return SendResult{}, fmt.Errorf("provider: write message: %w", err)
	}
	if err := w.Close(); err != nil {
		// This is where the provider's accept/reject verdict lands.
		return SendResult{}, classifySMTP(fmt.Errorf("provider: submit: %w", err), err)
	}
	_ = client.Quit()

	return SendResult{Response: "accepted for delivery", SMTPUTF8: utf8OK}, nil
}

// withSendTimeout bounds a conversation by timeout (zero: DefaultSendTimeout).
func withSendTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// open is the connection half of a submission, shared by Send and CheckAuth so
// both are held to the same rules: the guarded dial, TLS (implicit or a
// successful STARTTLS) and the refusal to go on over cleartext. The caller owns
// the returned client and must Close it; ctx must already carry the deadline.
func (s SMTPSender) open(ctx context.Context, req SendRequest) (*smtp.Client, string, error) {
	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	conn, err := dialSMTP(ctx, s.AllowPrivateHosts, s.Resolver, addr)
	if err != nil {
		return nil, addr, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	tlsCfg := req.TLSConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: req.Host, MinVersion: tls.VersionTLS12}
	}

	overTLS := false
	if req.ImplicitTLS {
		tconn := tls.Client(conn, tlsCfg)
		if err := tconn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, addr, fmt.Errorf("provider: tls handshake %s: %w", addr, err)
		}
		conn, overTLS = tconn, true
	}

	client, err := smtp.NewClient(conn, req.Host)
	if err != nil {
		_ = conn.Close()
		return nil, addr, fmt.Errorf("provider: smtp greet %s: %w", addr, err)
	}

	if !overTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsCfg); err != nil {
				_ = client.Close()
				return nil, addr, fmt.Errorf("provider: starttls %s: %w", addr, err)
			}
			overTLS = true
		}
	}
	if !overTLS && !req.AllowInsecureNoTLS {
		_ = client.Close()
		return nil, addr, fmt.Errorf(
			"provider: %s offers no TLS; refusing to send credentials over a cleartext connection", addr)
	}
	return client, addr, nil
}

// CheckAuth connects and authenticates with req.Auth (or req.Username/
// req.Password), then
// quits WITHOUT sending anything: an AUTH-only credential check for a CLI that
// is about to seal an SMTP login (#655). It uses the same guarded dial and TLS
// rules as Send. Only Host, Port, Username, Password, ImplicitTLS, Timeout,
// TLSConfig and AllowInsecureNoTLS are read. The error is the server's, never
// the credential's.
func (s SMTPSender) CheckAuth(ctx context.Context, req SendRequest) error {
	auth := req.auth()
	if auth == nil {
		return errors.New("provider: smtp auth check: no username")
	}
	ctx, cancel := withSendTimeout(ctx, req.Timeout)
	defer cancel()
	client, _, err := s.open(ctx, req)
	if err != nil {
		return err
	}
	if client, err = s.authenticate(ctx, client, req); err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	_ = client.Quit()
	return nil
}

// authenticate runs the request's AUTH on client (none when req has no
// mechanism) and returns the client to continue on. On a 535 to an XOAUTH2
// token it hands the token back to the source (Reject), opens a FRESH
// connection and authenticates once more with whatever the source issues next
// (ADR-046 §5, obligation (a) of §9): a cached token the provider has already
// revoked must not fail a send that a refresh would have let through. A new
// connection rather than a second AUTH on the old one, because not every
// server keeps talking after a refusal. Exactly one retry; the second verdict
// is final.
//
// On error the client it was given (and any it opened) is closed. The error
// text is the server's, never the credential's.
func (s SMTPSender) authenticate(ctx context.Context, client *smtp.Client, req SendRequest) (*smtp.Client, error) {
	auth := req.auth()
	if auth == nil {
		return client, nil
	}
	for attempt := 0; ; attempt++ {
		cl, err := auth.client(ctx, req.Host)
		if err != nil {
			_ = client.Close()
			return nil, tokenError(err)
		}
		err = client.Auth(cl)
		if err == nil {
			return client, nil
		}
		_ = client.Close()
		if isSMTPAuthDisabled(err) {
			// 535 5.7.139: the provider accepted WHO we are and refuses SMTP
			// AUTH for the mailbox or tenant as a policy. The token is fine —
			// handing it back would only force a pointless refresh — and a
			// retry with a fresh one gets the same answer.
			return nil, fmt.Errorf("%w: %w: %v", ErrPermanent, ErrSMTPAuthDisabled, err)
		}
		// Every refused token is handed back, the final one included
		// (security review L1 of #897): otherwise a token refused twice would
		// stay cached and every later send would repeat refuse, reject,
		// refresh. Only the FIRST refusal earns a retry.
		retryable := isAuthRefused(err) && auth.reject(cl)
		if !retryable || attempt > 0 {
			return nil, classifySMTP(fmt.Errorf("provider: smtp auth: %w", err), err)
		}
		if client, _, err = s.open(ctx, req); err != nil {
			return nil, err
		}
	}
}

// ErrSMTPAuthDisabled marks Microsoft's `535 5.7.139 Authentication
// unsuccessful, SmtpClientAuthentication is disabled for the Tenant/Mailbox`
// (exact wording UNVERIFIED; the enhanced code is what is matched): the
// organisation has turned off authenticated SMTP — Security Defaults does
// it, or the tenant or mailbox setting. It is permanent until an
// administrator changes it, so the account becomes RECEIVE-ONLY in Mailroom
// (store.SendBlockedSMTPAuthDisabled) rather than retrying. It always comes
// wrapped together with ErrPermanent.
var ErrSMTPAuthDisabled = errors.New("provider: SMTP AUTH is disabled for this mailbox by its organisation (5.7.139)")

// ErrSendAsDenied marks a refusal to send as the From address (Exchange's
// 5.2.252 SendAsDenied; code UNVERIFIED): the mailbox has no Send As right
// for it. Permanent; wrapped together with ErrPermanent.
var ErrSendAsDenied = errors.New("provider: the mailbox is not permitted to send as this From address (SendAsDenied)")

// isSMTPAuthDisabled reports Microsoft's SMTP-AUTH-disabled refusal: a SERVER
// REPLY (a *textproto.Error) with code 535 whose text starts with the enhanced
// status 5.7.139.
//
// ⚠ ONLY THE SERVER'S REPLY IS EVER INSPECTED, NEVER err.Error() (review M-H1
// of #910). A transport error carries the peer's address in its text
// ("write tcp …->52.5.7.139:587: connection reset"), so substring matching
// read a network blip as a policy refusal — the send failed permanently and
// the account went receive-only.
func isSMTPAuthDisabled(err error) bool {
	tp, ok := serverReply(err)
	return ok && tp.Code == 535 && hasEnhancedCode(tp.Msg, "5.7.139")
}

// isSendAsDenied reports Exchange's Send As refusal: a 5xx SERVER REPLY whose
// text starts with the enhanced status 5.2.252. The same rule as above: never
// the error string.
func isSendAsDenied(err error) bool {
	tp, ok := serverReply(err)
	return ok && tp.Code >= 500 && tp.Code < 600 && hasEnhancedCode(tp.Msg, "5.2.252")
}

// serverReply finds the SMTP server's reply in err, if there is one.
func serverReply(err error) (*textproto.Error, bool) {
	var tp *textproto.Error
	if err == nil || !errors.As(err, &tp) || tp == nil {
		return nil, false
	}
	return tp, true
}

// hasEnhancedCode reports whether a reply text starts with the RFC 3463
// enhanced status code (optionally after a repeated 3-digit reply code, as
// some servers send in the text). Only the first line counts, and the code
// must be followed by a space, the end, or a non-digit/non-dot character, so
// "5.7.1390" is not "5.7.139".
func hasEnhancedCode(msg, code string) bool {
	line := strings.TrimSpace(strings.SplitN(msg, "\n", 2)[0])
	if len(line) > 4 && isDigit(line[0]) && isDigit(line[1]) && isDigit(line[2]) && (line[3] == ' ' || line[3] == '-') {
		line = strings.TrimSpace(line[4:])
	}
	if !strings.HasPrefix(line, code) {
		return false
	}
	rest := line[len(code):]
	return rest == "" || (rest[0] != '.' && !isDigit(rest[0]))
}

// isAuthRefused reports a 535, the server's verdict on the credential (RFC
// 4954 §6: "Authentication credentials invalid"). Gmail answers 535-5.7.8,
// Microsoft 535 5.7.3. Nothing else is a reason to drop a token.
func isAuthRefused(err error) bool {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return tp.Code == 535
	}
	return strings.HasPrefix(strings.TrimSpace(err.Error()), "535")
}

// tokenError classifies a failure to OBTAIN a token for XOAUTH2 (nothing was
// sent): transient when the token source says so, so the send is retried with
// backoff; otherwise permanent — a grant that needs the user to reconnect, or
// an operator's broken client registration, is not fixed by retrying the same
// submission five times.
func tokenError(err error) error {
	if errors.Is(err, xoauth2.ErrTokenUnavailable) {
		return fmt.Errorf("provider: smtp auth: %w", err)
	}
	return fmt.Errorf("%w: provider: smtp auth: %w", ErrPermanent, err)
}

// auth resolves the request's mechanism: Auth when set, else AUTH PLAIN from
// Username/Password (the pre-ADR-046 behaviour, unchanged), else nil — no AUTH.
func (r SendRequest) auth() SMTPAuth {
	if r.Auth != nil {
		return r.Auth
	}
	if r.Username != "" {
		return PasswordAuth(r.Username, r.Password)
	}
	return nil
}

// ErrHostNotPermitted is returned when the SMTP host resolves to an address the
// egress policy refuses. It wraps egress.ErrBlockedAddress, and it is its own
// sentinel so the send worker fails the submission with this cause instead of
// retrying it: waiting does not make a refused address permitted, and each
// retry would only re-resolve and refuse again.
var ErrHostNotPermitted = errors.New("provider: smtp host is not a permitted connection target")

// dialSMTP is the ONE submission dial, shared by Send and ProbeSMTP, so the
// guard cannot be present on one and missing from the other.
func dialSMTP(ctx context.Context, allowPrivate bool, r egress.Resolver, addr string) (net.Conn, error) {
	d := &egress.Dialer{AllowPrivate: allowPrivate, Resolver: r}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err == nil {
		return conn, nil
	}
	if errors.Is(err, egress.ErrBlockedAddress) {
		// Names the host and the refused address (both are in err) and the way
		// out. Nothing credential-shaped is in scope here, so none can leak.
		return nil, fmt.Errorf("%w: %s (%w). If this instance must reach a mail server on "+
			"a private network, set MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS=1",
			ErrHostNotPermitted, addr, err)
	}
	return nil, fmt.Errorf("provider: dial %s: %w", addr, err)
}

// needsEAI reports whether any envelope address requires RFC 6531.
func needsEAI(from string, rcpt []string) bool {
	if !isASCII(from) {
		return true
	}
	for _, r := range rcpt {
		if !isASCII(r) {
			return true
		}
	}
	return false
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// classifySMTP wraps a failure with ErrPermanent when the server returned a 5xx.
// A 4xx (or a network error) stays retryable: the durable job will try again.
func classifySMTP(wrapped error, cause error) error {
	if isSendAsDenied(cause) {
		return fmt.Errorf("%w: %w: %v", ErrPermanent, ErrSendAsDenied, wrapped)
	}
	var tp *textproto.Error
	if errors.As(cause, &tp) && tp.Code >= 500 && tp.Code < 600 {
		return fmt.Errorf("%w: %v", ErrPermanent, wrapped)
	}
	// net/smtp does not always type its errors; fall back to the text prefix.
	msg := strings.TrimSpace(cause.Error())
	if len(msg) >= 3 && msg[0] == '5' && isDigit(msg[1]) && isDigit(msg[2]) {
		return fmt.Errorf("%w: %v", ErrPermanent, wrapped)
	}
	return wrapped
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
