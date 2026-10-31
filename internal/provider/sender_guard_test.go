// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"mailroom/internal/egress"
	"mailroom/internal/egress/egresstest"
)

// THE SUBMISSION DIAL IS GUARDED (#625 §3). The account's SMTP host was checked
// when the account was validated; by the time mail is sent it may resolve
// somewhere else. The AUTH command carries the mailbox password, so the
// assertion that matters is on the SOCKET: a refused host leaves the listener
// with zero connections.
//
// The host is "localhost" on purpose: the guarded dial resolves through the
// injected Resolver, but an UNGUARDED dial resolves through the system, where
// localhost is the listener's own address — so removing the guard makes the
// connection arrive, and the test fails on what the socket saw.

const guardPassword = "hunter2-the-real-mailbox-password"

func guardRequest(port int) SendRequest {
	return SendRequest{
		Host: "localhost", Port: port,
		Username: "operator@example.com", Password: guardPassword,
		MailFrom: "operator@example.com", RcptTo: []string{"someone@example.com"},
		Data: []byte("Subject: hi\r\n\r\nhello\r\n"), Timeout: 5 * time.Second,
		AllowInsecureNoTLS: true, // the recording server is plaintext; the guard is transport-independent
	}
}

func assertSMTPRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Send SUCCEEDED to a refused address")
	}
	if !errors.Is(err, ErrHostNotPermitted) || !errors.Is(err, egress.ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrHostNotPermitted wrapping egress.ErrBlockedAddress", err)
	}
	if errors.Is(err, ErrPermanent) {
		t.Errorf("a policy refusal is dressed as a provider 5xx: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"localhost", "MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q: %v", want, msg)
		}
	}
	if strings.Contains(msg, guardPassword) {
		t.Errorf("refusal leaks the password: %v", msg)
	}
}

func recordingSMTPListener(t *testing.T) (*egresstest.Listener, *recordingSMTP) {
	t.Helper()
	srv := &recordingSMTP{}
	ln := egresstest.Listen(t, func(c net.Conn) { srv.serve(c, []string{"AUTH PLAIN"}) })
	return ln, srv
}

func TestSendRefusesNonPublicResolutionWithoutConnecting(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::ffff:127.0.0.1", "fe80::1%eth0"} {
		t.Run(addr, func(t *testing.T) {
			ln, srv := recordingSMTPListener(t)
			s := SMTPSender{Resolver: egresstest.NewResolver(t, []string{addr})}
			_, err := s.Send(context.Background(), guardRequest(ln.Port()))
			if n := ln.Connections(t); n != 0 {
				t.Errorf("resolving to %s: %d connection(s) reached the SMTP listener", addr, n)
			}
			if got := srv.seen(); len(got) != 0 {
				t.Errorf("the server received commands: %q", got)
			}
			assertSMTPRefused(t, err)
		})
	}
}

func TestSendAllowPrivateHostsConnects(t *testing.T) {
	ln, srv := recordingSMTPListener(t)
	s := SMTPSender{AllowPrivateHosts: true, Resolver: egresstest.NewResolver(t, []string{"127.0.0.1"})}
	// The recording server answers 250 to everything, so net/smtp gives up at
	// AUTH (it wants 235). What is asserted is that the guard let the dial
	// through and the conversation began: a connection, and an EHLO.
	_, err := s.Send(context.Background(), guardRequest(ln.Port()))
	if errors.Is(err, egress.ErrBlockedAddress) || errors.Is(err, ErrHostNotPermitted) {
		t.Fatalf("AllowPrivateHosts did not permit a loopback SMTP host: %v", err)
	}
	if n := ln.Connections(t); n != 1 {
		t.Errorf("listener saw %d connection(s), want 1", n)
	}
	if !srv.sawCommand("EHLO") {
		t.Errorf("no SMTP conversation took place: %q (err %v)", srv.seen(), err)
	}
}

// TestSendRefusesARebindAfterValidation: validation saw a public answer for the
// SMTP host; the submission's dial gets loopback.
func TestSendRefusesARebindAfterValidation(t *testing.T) {
	ln, srv := recordingSMTPListener(t)
	r := egresstest.NewResolver(t, []string{"93.184.216.34"}, []string{"127.0.0.1"})
	if err := egress.CheckHost(context.Background(), r, "localhost"); err != nil {
		t.Fatalf("validation-time check refused the public answer: %v", err)
	}
	_, err := SMTPSender{Resolver: r}.Send(context.Background(), guardRequest(ln.Port()))
	if n := ln.Connections(t); n != 0 {
		t.Errorf("the rebound submission reached the loopback listener (%d connection(s))", n)
	}
	if srv.sawCommand("AUTH") {
		t.Errorf("the credential was sent to the rebound address: %q", srv.seen())
	}
	assertSMTPRefused(t, err)
}

// TestGuardedSMTPSVerifiesTheHostname: through the guard (resolved to the
// loopback literal), SMTPS with a certificate for the HOSTNAME succeeds, and
// one valid only for the dialled IP is refused.
func TestGuardedSMTPSVerifiesTheHostname(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dns    []string
		ips    []net.IP
		wantOK bool
	}{
		{"cert for the hostname", []string{"smtp.example.com"}, nil, true},
		{"cert only for the dialled IP", nil, []net.IP{net.ParseIP("127.0.0.1")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srvCert, pool := testCertificate(t, tc.dns, tc.ips)
			rec := &recordingSMTP{}
			ln := egresstest.Listen(t, func(c net.Conn) {
				sc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{srvCert}})
				if err := sc.Handshake(); err != nil {
					_ = c.Close()
					return
				}
				rec.serve(sc, []string{"AUTH PLAIN"})
			})
			s := SMTPSender{AllowPrivateHosts: true, Resolver: egresstest.NewResolver(t, []string{"127.0.0.1"})}
			req := guardRequest(ln.Port())
			req.Host = "smtp.example.com"
			req.ImplicitTLS = true
			req.AllowInsecureNoTLS = false
			// The production config is {ServerName: req.Host, MinVersion: TLS1.2};
			// this is that plus the test CA.
			req.TLSConfig = &tls.Config{ServerName: req.Host, MinVersion: tls.VersionTLS12, RootCAs: pool}
			// The server only speaks SMTP after a completed handshake, so an EHLO
			// in its record IS the observation that TLS verification accepted it.
			// (It answers 250 to AUTH, so Send still ends in an auth error.)
			_, err := s.Send(context.Background(), req)
			if tc.wantOK {
				if err != nil && strings.Contains(err.Error(), "tls handshake") {
					t.Fatalf("a certificate for the hostname was refused through the guard: %v", err)
				}
				if !rec.sawCommand("EHLO") {
					t.Errorf("no SMTP conversation over TLS took place: %q (err %v)", rec.seen(), err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "tls handshake") {
					t.Fatalf("err = %v, want a TLS handshake failure: a certificate valid only for the dialled IP must not be accepted", err)
				}
				if len(rec.seen()) != 0 {
					t.Errorf("the server got SMTP commands over an unverified session: %q", rec.seen())
				}
			}
			if n := ln.Connections(t); n != 1 {
				t.Errorf("listener saw %d connection(s), want 1", n)
			}
		})
	}
}

// testCertificate issues a leaf for exactly the given DNS names and IPs from a
// fresh CA, and returns the leaf plus a pool trusting that CA.
func testCertificate(t *testing.T, dnsNames []string, ips []net.IP) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: dnsNames, IPAddresses: ips,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, pool
}
