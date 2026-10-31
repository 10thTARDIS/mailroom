// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TLS EVERYWHERE, on the submission path (spec §11, Forgejo #39).
//
// This is the one place in Mailroom where a plaintext credential is handed to a
// remote server, so "TLS everywhere" is not a slogan here — it is the difference
// between a mailbox password on the wire and not. sender.go says it refuses; the
// tests below make it prove it, and they prove it in the strongest available
// form: not "Send returned an error", but "THE SERVER NEVER SAW AN AUTH COMMAND".
//
// An error return can be produced by a server that already has your password.

// loopbackSender is the sender these tests drive. Their servers listen on
// 127.0.0.1, which the submission dial's egress guard refuses by default (#625
// §3), so the guard is lifted here exactly as MAILROOM_ALLOW_PRIVATE_ACCOUNT_
// HOSTS lifts it. ⚠ Without it every test below would pass for the WRONG reason
// — "no AUTH was seen" is also true of a connection that was never made — so
// the guard's own behaviour is tested separately, in sender_guard_test.go.
var loopbackSender = SMTPSender{AllowPrivateHosts: true}

// recordingSMTP is a plaintext ESMTP server that REMEMBERS every command it was
// given. It never offers STARTTLS.
type recordingSMTP struct {
	mu   sync.Mutex
	cmds []string
}

func (s *recordingSMTP) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

func (s *recordingSMTP) sawCommand(verb string) bool {
	for _, c := range s.seen() {
		if strings.HasPrefix(strings.ToUpper(c), verb) {
			return true
		}
	}
	return false
}

// startRecordingSMTP runs the server on a random port. caps is the EHLO
// capability list — the point of the default is that STARTTLS is NOT in it.
func startRecordingSMTP(t *testing.T, caps []string) (*recordingSMTP, string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	s := &recordingSMTP{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, caps)
		}
	}()

	h, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return s, h, n
}

func (s *recordingSMTP) serve(conn net.Conn, caps []string) {
	defer func() { _ = conn.Close() }()
	w := bufio.NewWriter(conn)
	r := bufio.NewReader(conn)
	line := func(str string) { _, _ = w.WriteString(str + "\r\n"); _ = w.Flush() }

	line("220 recording ESMTP ready")
	for {
		raw, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(raw)

		s.mu.Lock()
		s.cmds = append(s.cmds, cmd)
		s.mu.Unlock()

		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			all := append([]string{"recording greets you"}, caps...)
			for i, c := range all {
				sep := "-"
				if i == len(all)-1 {
					sep = " "
				}
				line("250" + sep + c)
			}
		case strings.HasPrefix(upper, "AUTH PLAIN "):
			line("235 2.7.0 Authentication successful")
		case strings.HasPrefix(upper, "QUIT"):
			line("221 Bye")
			return
		default:
			line("250 OK")
		}
	}
}

// THE ASSERTION THAT MATTERS: against a server that offers no TLS, the sender
// abandons the connection and the SERVER NEVER SEES `AUTH`. Not a 5xx, not a
// wrapped error — the credential is never transmitted.
func TestSendNeverAuthenticatesOverCleartext(t *testing.T) {
	srv, host, port := startRecordingSMTP(t, []string{"SIZE 35882577", "8BITMIME"}) // no STARTTLS

	_, err := loopbackSender.Send(context.Background(), SendRequest{
		Host: host, Port: port,
		Username: "operator@example.com",
		Password: "hunter2-the-real-mailbox-password",
		MailFrom: "operator@example.com",
		RcptTo:   []string{"someone@example.com"},
		Data:     []byte("Subject: hi\r\n\r\nhello\r\n"),
		Timeout:  5 * time.Second,
		// AllowInsecureNoTLS is DELIBERATELY not set: that is the production shape.
	})
	if err == nil {
		t.Fatal("Send SUCCEEDED over a cleartext connection")
	}
	if !strings.Contains(err.Error(), "cleartext") {
		t.Errorf("error = %v, want a refusal naming the cleartext connection", err)
	}

	// The whole point. Nothing after EHLO was ever sent.
	for _, verb := range []string{"AUTH", "MAIL", "RCPT", "DATA"} {
		if srv.sawCommand(verb) {
			t.Errorf("the server received %s over a cleartext connection.\nconversation: %q",
				verb, srv.seen())
		}
	}
	// And the password is not anywhere in what was transmitted.
	for _, c := range srv.seen() {
		if strings.Contains(c, "hunter2") || strings.Contains(c, "operator@example.com") {
			t.Errorf("a credential crossed a cleartext wire: %q", c)
		}
	}
	// Nor in the error we surface (CLAUDE.md: never log credentials).
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error text leaks the password: %v", err)
	}
}

// A server that STRIPS its STARTTLS advertisement (an active MITM's first move)
// gets exactly the same answer: no credential, no message.
func TestStarttlsStrippingIsRefused(t *testing.T) {
	srv, host, port := startRecordingSMTP(t, []string{"SIZE 10485760"}) // STARTTLS stripped

	_, err := loopbackSender.Send(context.Background(), SendRequest{
		Host: host, Port: port,
		Username: "u", Password: "p",
		MailFrom: "u@example.com", RcptTo: []string{"v@example.com"},
		Data: []byte("x"), Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("a STARTTLS-stripping server got a successful submission")
	}
	if srv.sawCommand("AUTH") || srv.sawCommand("DATA") {
		t.Errorf("a downgrade attack got the credential and/or the message: %q", srv.seen())
	}
}

// AllowInsecureNoTLS is the one escape hatch, and it exists for the in-process
// test server. It must be OFF by default — the zero value of a SendRequest must
// be the safe value, because that is what every production caller builds.
func TestInsecureSendIsNotTheDefault(t *testing.T) {
	var zero SendRequest
	if zero.AllowInsecureNoTLS {
		t.Error("the zero SendRequest permits cleartext submission")
	}
	if zero.TLSConfig != nil {
		t.Error("the zero SendRequest carries a TLS config override")
	}
	// And the production caller (internal/compose) never sets it — asserted for
	// the whole repository, with go/ast, in cmd/mailroom's TestNoProductionCodeDisablesTLS.
}

// CERTIFICATE VERIFICATION IS ON. The strongest way to show it: point the sender
// at a real TLS server with a SELF-SIGNED certificate and require that the
// handshake FAILS. If verification were off (or MinVersion were unset in a way
// that mattered), this would connect and then authenticate.
func TestImplicitTLSVerifiesTheCertificate(t *testing.T) {
	host, port := startSelfSignedTLSSMTP(t)

	_, err := loopbackSender.Send(context.Background(), SendRequest{
		Host: host, Port: port,
		ImplicitTLS: true,
		Username:    "operator@example.com",
		Password:    "hunter2-the-real-mailbox-password",
		MailFrom:    "operator@example.com",
		RcptTo:      []string{"someone@example.com"},
		Data:        []byte("Subject: hi\r\n\r\nhello\r\n"),
		Timeout:     5 * time.Second,
		// TLSConfig deliberately nil: this is the production path, which builds
		// &tls.Config{ServerName: Host, MinVersion: TLS1.2} — no InsecureSkipVerify.
	})
	if err == nil {
		t.Fatal("SMTPS to a SELF-SIGNED server succeeded — certificate verification is OFF")
	}
	if !strings.Contains(err.Error(), "tls handshake") {
		t.Errorf("error = %v, want a TLS handshake failure", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error text leaks the password: %v", err)
	}
}

// startSelfSignedTLSSMTP is an SMTPS listener with a certificate no root store
// trusts.
func startSelfSignedTLSSMTP(t *testing.T) (string, int) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				// If the handshake ever completes, greet — so that a regression here
				// fails LOUDLY (the sender would proceed to AUTH) rather than looking
				// like a network blip.
				_, _ = conn.Write([]byte("220 self-signed ESMTP ready\r\n"))
				_, _ = bufio.NewReader(conn).ReadString('\n')
			}()
		}
	}()

	h, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return h, n
}
