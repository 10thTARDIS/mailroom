// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"mailroom/internal/egress/egresstest"
	"mailroom/internal/xoauth2"
)

// IMAP XOAUTH2 (ADR-046) through the real Dial, against a scripted server.
// Every assertion is about what the SERVER saw as well as what Dial returned:
// "an error came back" is also what a client that never authenticated, or
// authenticated the wrong way, would produce.

const imapTestToken = xoauth2.Secret("ya29.IMAP-SENTINEL-ACCESS-TOKEN-do-not-log")

type xoauthIMAPScript struct {
	caps string // CAPABILITY list in the greeting
	// verdict for an initial response: "ok", "challenge" (Gmail: failure
	// challenge, then NO), "challenge-bad" (failure challenge, then BAD to the
	// empty answer), "no" (Microsoft: bare NO, no challenge).
	verdict func(ir string) string
}

type xoauthIMAPServer struct {
	mu        stdsync.Mutex
	irs       []string // decoded initial responses
	answers   []string // raw lines sent in answer to the failure challenge
	commands  []string // command verbs seen
	loginArgs []string
}

func (s *xoauthIMAPServer) snapshot() (irs, answers, commands, loginArgs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.irs...), append([]string(nil), s.answers...),
		append([]string(nil), s.commands...), append([]string(nil), s.loginArgs...)
}

func (s *xoauthIMAPServer) handle(sc xoauthIMAPScript) func(net.Conn) {
	return func(c net.Conn) {
		defer func() { _ = c.Close() }()
		w := bufio.NewWriter(c)
		r := bufio.NewReader(c)
		put := func(str string) { _, _ = w.WriteString(str + "\r\n"); _ = w.Flush() }
		readLine := func() (string, bool) {
			l, err := r.ReadString('\n')
			return strings.TrimRight(l, "\r\n"), err == nil
		}
		put("* OK [CAPABILITY " + sc.caps + "] ready")
		for {
			line, ok := readLine()
			if !ok {
				return
			}
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			tag, cmd := f[0], strings.ToUpper(f[1])
			s.mu.Lock()
			s.commands = append(s.commands, cmd)
			if cmd == "LOGIN" {
				s.loginArgs = append(s.loginArgs, f[2:]...)
			}
			s.mu.Unlock()
			switch cmd {
			case "CAPABILITY":
				put("* CAPABILITY " + sc.caps)
				put(tag + " OK done")
			case "AUTHENTICATE":
				var b64 string
				if len(f) >= 4 {
					b64 = f[3] // SASL-IR
				} else {
					put("+ ")
					if b64, ok = readLine(); !ok {
						return
					}
				}
				dec, _ := base64.StdEncoding.DecodeString(b64)
				s.mu.Lock()
				s.irs = append(s.irs, string(dec))
				s.mu.Unlock()
				switch v := sc.verdict(string(dec)); v {
				case "ok":
					put(tag + " OK [CAPABILITY IMAP4rev1] authenticated")
				case "no":
					put(tag + " NO AUTHENTICATE failed.")
				case "challenge", "challenge-bad":
					put("+ " + base64.StdEncoding.EncodeToString(
						[]byte(`{"status":"401","schemes":"bearer","scope":"https://mail.google.com/"}`)))
					ans, ok := readLine()
					if !ok {
						return
					}
					s.mu.Lock()
					s.answers = append(s.answers, ans)
					s.mu.Unlock()
					if v == "challenge-bad" {
						put(tag + " BAD invalid response")
					} else {
						put(tag + " NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)")
					}
				}
			case "LOGIN":
				put(tag + " OK logged in")
			case "LOGOUT":
				put("* BYE")
				put(tag + " OK bye")
				return
			default:
				put(tag + " BAD unexpected")
			}
		}
	}
}

func xoauthIR(user string) string {
	return "user=" + user + "\x01auth=Bearer " + imapTestToken.Reveal() + "\x01\x01"
}

// scriptedSource is a TokenSource that hands out tokens in order (the last one
// repeats), or err, and records every Reject.
type scriptedSource struct {
	mu       stdsync.Mutex
	tokens   []xoauth2.Secret
	err      error
	calls    int
	rejected []xoauth2.Secret
}

func (s *scriptedSource) Token(context.Context) (xoauth2.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	i := s.calls - 1
	if i >= len(s.tokens) {
		i = len(s.tokens) - 1
	}
	return s.tokens[i], nil
}

func (s *scriptedSource) Reject(tok xoauth2.Secret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected = append(s.rejected, tok)
}

func (s *scriptedSource) snapshot() (calls int, rejected []xoauth2.Secret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, append([]xoauth2.Secret(nil), s.rejected...)
}

func staticToken(tok xoauth2.Secret) *scriptedSource {
	return &scriptedSource{tokens: []xoauth2.Secret{tok}}
}

// dialXOAuthScript dials the scripted server with cfg's auth fields.
func dialXOAuthScript(t *testing.T, sc xoauthIMAPScript, mutate func(*Config)) (*xoauthIMAPServer, SyncSession, error) {
	t.Helper()
	srv := &xoauthIMAPServer{}
	// Implicit TLS with a certificate for "localhost" from a fresh test CA:
	// an XOAUTH2 Config refuses TLSInsecure and InsecureSkipVerify, so the
	// scripted server is reached over VERIFIED TLS, as a real one would be.
	srvTLS, pool := localhostTLS(t)
	handle := srv.handle(sc)
	ln := egresstest.Listen(t, func(c net.Conn) {
		tc := tls.Server(c, srvTLS)
		if err := tc.Handshake(); err != nil {
			_ = c.Close()
			return
		}
		handle(tc)
	})
	cfg := Config{
		Host: "localhost", Port: ln.Port(), TLS: TLSImplicit, DialTimeout: 5 * time.Second,
		Resolver: egresstest.NewResolver(t, []string{"127.0.0.1"}), AllowPrivateHosts: true,
		rootCAs: pool,
	}
	mutate(&cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := Dial(ctx, cfg)
	if sess != nil {
		t.Cleanup(func() { _ = sess.Close() })
	}
	return srv, sess, err
}

// localhostTLS returns a server TLS config for "localhost" and a pool that
// trusts it.
func localhostTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}

func withXOAuth2(user string, tok xoauth2.TokenSource) func(*Config) {
	return func(c *Config) { c.Auth = XOAuth2Auth(user, tok) }
}

func TestXOAuth2DialSucceeds(t *testing.T) {
	for _, caps := range []string{
		"IMAP4rev1 SASL-IR AUTH=XOAUTH2 AUTH=PLAIN", // Gmail: initial response on the command line
		"IMAP4rev1 AUTH=XOAUTH2",                    // no SASL-IR: the response follows an empty "+"
	} {
		t.Run(caps, func(t *testing.T) {
			want := xoauthIR("you@example.com")
			srv, sess, err := dialXOAuthScript(t, xoauthIMAPScript{caps: caps, verdict: func(ir string) string {
				if ir == want {
					return "ok"
				}
				return "no"
			}}, withXOAuth2("you@example.com", staticToken(imapTestToken)))
			if err != nil || sess == nil {
				t.Fatalf("Dial: %v", err)
			}
			irs, _, cmds, _ := srv.snapshot()
			if len(irs) != 1 || irs[0] != want {
				t.Fatalf("server received initial responses %q, want exactly the XOAUTH2 one", irs)
			}
			for _, c := range cmds {
				if c == "LOGIN" {
					t.Errorf("an OAuth dial also sent LOGIN: %q", cmds)
				}
			}
		})
	}
}

// Gmail's failure shape: the failure challenge must be answered with an EMPTY
// response (go-imap renders an empty continuation as "="), and the refusal is
// an AUTH error carrying only the challenge's status — not the token, and not
// the scope the challenge also names.
func TestXOAuth2DialFailureChallengeIsAuth(t *testing.T) {
	for _, verdict := range []string{"challenge", "challenge-bad"} {
		t.Run(verdict, func(t *testing.T) {
			srv, sess, err := dialXOAuthScript(t, xoauthIMAPScript{
				caps:    "IMAP4rev1 SASL-IR AUTH=XOAUTH2",
				verdict: func(string) string { return verdict },
			}, withXOAuth2("you@example.com", staticToken(imapTestToken)))
			if sess != nil || err == nil {
				t.Fatal("Dial succeeded against a refusing server")
			}
			var se *Error
			if !errors.As(err, &se) || se.Kind != ErrorKindAuth {
				t.Fatalf("err = %v, want ErrorKindAuth", err)
			}
			msg := err.Error()
			if strings.Contains(msg, imapTestToken.Reveal()) {
				t.Fatalf("the access token leaked into the error: %s", msg)
			}
			if strings.Contains(msg, "mail.google.com") {
				t.Errorf("the challenge's scope reached the error (only the status may): %s", msg)
			}
			if !strings.Contains(msg, "status 401") {
				t.Errorf("error %q does not carry the challenge status", msg)
			}
			// Two attempts (the refused token is rejected and the retry asks
			// again, ADR-046 §5), each challenge answered with one empty
			// response.
			_, answers, _, _ := srv.snapshot()
			if len(answers) != 2 {
				t.Fatalf("client answered %d failure challenges (%q), want 2 (the attempt and its one retry)", len(answers), answers)
			}
			for _, a := range answers {
				if a != "" && a != "=" {
					t.Fatalf("client answered the failure challenge with %q, want an empty response", a)
				}
			}
		})
	}
}

// OBLIGATION (a) of ADR-046 §9: a refused token is handed back to the source
// (Reject) and the authentication is retried ONCE with whatever the source
// issues next. Here the first token is dead and the refreshed one is good:
// the dial must succeed, with exactly one Reject naming the dead token.
func TestXOAuth2RejectedTokenIsDroppedAndRetriedOnce(t *testing.T) {
	const fresh = xoauth2.Secret("ya29.IMAP-SENTINEL-FRESH-TOKEN-do-not-log")
	for _, refusal := range []string{"challenge", "no"} {
		t.Run(refusal, func(t *testing.T) {
			src := &scriptedSource{tokens: []xoauth2.Secret{imapTestToken, fresh}}
			srv, sess, err := dialXOAuthScript(t, xoauthIMAPScript{
				caps: "IMAP4rev1 SASL-IR AUTH=XOAUTH2",
				verdict: func(ir string) string {
					if strings.Contains(ir, fresh.Reveal()) {
						return "ok"
					}
					return refusal
				},
			}, withXOAuth2("you@example.com", src))
			if err != nil || sess == nil {
				t.Fatalf("Dial: %v — a refused cached token was not refreshed and retried", err)
			}
			calls, rejected := src.snapshot()
			if calls != 2 {
				t.Errorf("token source asked %d times, want 2", calls)
			}
			if len(rejected) != 1 || rejected[0] != imapTestToken {
				t.Errorf("Reject calls = %d (first is the refused token: %v), want exactly the refused token",
					len(rejected), len(rejected) > 0 && rejected[0] == imapTestToken)
			}
			if irs, _, _, _ := srv.snapshot(); len(irs) != 2 {
				t.Errorf("server saw %d AUTHENTICATEs, want 2", len(irs))
			}
		})
	}
}

// The retry is ONE retry: a server that refuses every token sees exactly two
// attempts, the source exactly one Reject per refusal it caused before the
// final verdict, and the verdict is an auth error. A loop here would hammer
// the provider and the token endpoint on every pass.
func TestXOAuth2RetryIsNotALoop(t *testing.T) {
	src := &scriptedSource{tokens: []xoauth2.Secret{imapTestToken, "ya29.SECOND-sentinel", "ya29.THIRD-sentinel"}}
	srv, sess, err := dialXOAuthScript(t, xoauthIMAPScript{
		caps:    "IMAP4rev1 SASL-IR AUTH=XOAUTH2",
		verdict: func(string) string { return "no" },
	}, withXOAuth2("you@example.com", src))
	if sess != nil {
		t.Fatal("Dial succeeded")
	}
	var se *Error
	if !errors.As(err, &se) || se.Kind != ErrorKindAuth {
		t.Fatalf("err = %v, want ErrorKindAuth", err)
	}
	if irs, _, _, _ := srv.snapshot(); len(irs) != 2 {
		t.Fatalf("server saw %d AUTHENTICATEs, want exactly 2 (one retry, no loop)", len(irs))
	}
	// Both refused tokens are handed back, the final one included (review
	// L1): the second token must not stay cached for the next connection.
	calls, rejected := src.snapshot()
	if calls != 2 || len(rejected) != 2 || rejected[0] != imapTestToken || rejected[1] != "ya29.SECOND-sentinel" {
		t.Errorf("source: %d Token calls, Rejects %d (want 2 and both refused tokens, in order)", calls, len(rejected))
	}
}

// A refusal that is NOT a credential verdict (the server does not offer the
// mechanism) rejects nothing and does not retry.
func TestXOAuth2NoRejectWithoutCredentialVerdict(t *testing.T) {
	src := staticToken(imapTestToken)
	_, _, err := dialXOAuthScript(t, xoauthIMAPScript{
		caps:    "IMAP4rev1 AUTH=PLAIN",
		verdict: func(string) string { return "ok" },
	}, withXOAuth2("you@example.com", src))
	if err == nil {
		t.Fatal("Dial succeeded")
	}
	if _, rejected := src.snapshot(); len(rejected) != 0 {
		t.Errorf("Reject called %d times for a non-credential failure", len(rejected))
	}
}

// Microsoft's documented failure: `NO AUTHENTICATE failed.`, no response code,
// no challenge (#888's shape). Through XOAUTH2 it is an AUTH error whatever Op
// Dial wraps it under, because the refusal is classified by what it is.
func TestXOAuth2DialBareNOIsAuth(t *testing.T) {
	_, sess, err := dialXOAuthScript(t, xoauthIMAPScript{
		caps:    "IMAP4 IMAP4rev1 AUTH=XOAUTH2 LOGINDISABLED SASL-IR",
		verdict: func(string) string { return "no" },
	}, withXOAuth2("you@example.com", staticToken(imapTestToken)))
	if sess != nil {
		t.Fatal("Dial succeeded")
	}
	var se *Error
	if !errors.As(err, &se) || se.Kind != ErrorKindAuth {
		t.Fatalf("err = %v, want ErrorKindAuth", err)
	}
	if strings.Contains(err.Error(), imapTestToken.Reveal()) {
		t.Fatalf("the access token leaked into the error: %v", err)
	}
}

// A server that does not advertise AUTH=XOAUTH2 is never sent the token, and
// that is a configuration problem, not a rejected credential.
func TestXOAuth2DialMechanismNotOffered(t *testing.T) {
	src := staticToken(imapTestToken)
	srv, sess, err := dialXOAuthScript(t, xoauthIMAPScript{
		caps:    "IMAP4rev1 AUTH=PLAIN",
		verdict: func(string) string { return "ok" },
	}, withXOAuth2("you@example.com", src))
	if sess != nil {
		t.Fatal("Dial succeeded")
	}
	var se *Error
	if !errors.As(err, &se) || se.Kind != ErrorKindConfig {
		t.Fatalf("err = %v, want ErrorKindConfig", err)
	}
	if _, _, cmds, _ := srv.snapshot(); containsCmd(cmds, "AUTHENTICATE") || containsCmd(cmds, "LOGIN") {
		t.Errorf("the server was sent credentials it never asked for: %q", cmds)
	}
	if calls, _ := src.snapshot(); calls != 0 {
		t.Error("a token was fetched for a server that cannot accept it")
	}
}

// A token source failure classifies by meaning, and nothing is sent.
func TestXOAuth2DialTokenFailures(t *testing.T) {
	for _, tc := range []struct {
		sentinel error
		want     ErrorKind
	}{
		{xoauth2.ErrReauthRequired, ErrorKindAuth},
		{xoauth2.ErrProviderMisconfigured, ErrorKindConfig},
		{xoauth2.ErrTokenUnavailable, ErrorKindTransient},
	} {
		t.Run(tc.want.String(), func(t *testing.T) {
			srv, sess, err := dialXOAuthScript(t, xoauthIMAPScript{
				caps:    "IMAP4rev1 SASL-IR AUTH=XOAUTH2",
				verdict: func(string) string { return "ok" },
			}, withXOAuth2("you@example.com", &scriptedSource{err: fmt.Errorf("oauthmail: refresh failed: %w", tc.sentinel)}))
			if sess != nil {
				t.Fatal("Dial succeeded without a token")
			}
			var se *Error
			if !errors.As(err, &se) || se.Kind != tc.want {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if _, _, cmds, _ := srv.snapshot(); containsCmd(cmds, "AUTHENTICATE") {
				t.Errorf("AUTHENTICATE was sent with no token: %q", cmds)
			}
		})
	}
}

// REGRESSION: with Auth nil, Dial still sends LOGIN with Username/Password,
// and never AUTHENTICATE.
func TestPasswordDialStillUsesLogin(t *testing.T) {
	srv, sess, err := dialXOAuthScript(t, xoauthIMAPScript{
		caps:    "IMAP4rev1 SASL-IR AUTH=XOAUTH2 AUTH=PLAIN",
		verdict: func(string) string { return "ok" },
	}, func(c *Config) { c.Username, c.Password = "you@example.com", "hunter2" })
	if err != nil || sess == nil {
		t.Fatalf("Dial: %v", err)
	}
	_, _, cmds, args := srv.snapshot()
	if !containsCmd(cmds, "LOGIN") || containsCmd(cmds, "AUTHENTICATE") {
		t.Fatalf("commands = %q, want LOGIN and no AUTHENTICATE", cmds)
	}
	if len(args) != 2 || strings.Trim(args[0], `"`) != "you@example.com" || strings.Trim(args[1], `"`) != "hunter2" {
		t.Errorf("LOGIN args = %q", args)
	}
}

func TestConfigValidateAuth(t *testing.T) {
	base := Config{Host: "imap.example.com", Port: 993}
	tok := staticToken(imapTestToken)
	for _, tc := range []struct {
		name string
		cfg  func(Config) Config
		ok   bool
	}{
		{"password", func(c Config) Config { c.Username, c.Password = "u", "p"; return c }, true},
		{"xoauth2", func(c Config) Config { c.Auth = XOAuth2Auth("u@example.com", tok); return c }, true},
		{"both", func(c Config) Config {
			c.Auth = XOAuth2Auth("u@example.com", tok)
			c.Username = "u"
			return c
		}, false},
		{"xoauth2 without user", func(c Config) Config { c.Auth = XOAuth2Auth("", tok); return c }, false},
		{"xoauth2 without token source", func(c Config) Config { c.Auth = XOAuth2Auth("u@example.com", nil); return c }, false},
		// L5: a bearer token never goes over unverified TLS.
		{"xoauth2 over TLSInsecure", func(c Config) Config {
			c.Auth = XOAuth2Auth("u@example.com", tok)
			c.TLS = TLSInsecure
			return c
		}, false},
		{"xoauth2 with InsecureSkipVerify", func(c Config) Config {
			c.Auth = XOAuth2Auth("u@example.com", tok)
			c.InsecureSkipVerify = true
			return c
		}, false},
		{"password over TLSInsecure (unchanged)", func(c Config) Config {
			c.Username, c.Password, c.TLS = "u", "p", TLSInsecure
			return c
		}, true},
		{"neither", func(c Config) Config { return c }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg(base).Validate()
			if (err == nil) != tc.ok {
				t.Errorf("Validate = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func containsCmd(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}
