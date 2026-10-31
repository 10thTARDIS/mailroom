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
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"mailroom/internal/egress"
	"mailroom/internal/egress/egresstest"
)

// THE IMAP DIAL IS GUARDED ON EVERY CONNECTION, not only at validation (#625
// §3). Sync, the pool, IDLE and the writeback drain all reach the server
// through Dial, so these tests drive Dial itself.
//
// ⚠ The host in the refusal cases is "localhost", deliberately. The guard
// resolves through the injected Resolver; a dial WITHOUT the guard resolves
// through the system, where localhost is 127.0.0.1 — the listener's address.
// So if the guard is removed, the connection demonstrably arrives at the
// listener and the test fails on what the socket saw, not merely on an error
// that a connect-then-fail dialler would also produce.

const guardTestPassword = "hunter2-the-real-mailbox-password"

// serveIMAP is just enough IMAP for Dial's greeting + LOGIN: every tagged
// command is answered OK, with capabilities up front so the client need not ask.
func serveIMAP(c net.Conn) {
	defer func() { _ = c.Close() }()
	w := bufio.NewWriter(c)
	r := bufio.NewReader(c)
	put := func(s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }
	put("* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		tag, cmd := f[0], strings.ToUpper(f[1])
		switch cmd {
		case "CAPABILITY":
			put("* CAPABILITY IMAP4rev1 AUTH=PLAIN")
			put(tag + " OK CAPABILITY completed")
		case "LOGOUT":
			put("* BYE")
			put(tag + " OK LOGOUT completed")
			return
		default:
			put(tag + " OK [CAPABILITY IMAP4rev1] " + cmd + " completed")
		}
	}
}

func guardConfig(host string, port int, r egress.Resolver) Config {
	return Config{
		Host: host, Port: port,
		Username: "you@example.com", Password: guardTestPassword,
		TLS:         TLSInsecure, // the listener speaks plaintext; the guard is transport-independent
		DialTimeout: 5 * time.Second,
		Resolver:    r,
	}
}

func assertRefused(t *testing.T, err error, host string) {
	t.Helper()
	if err == nil {
		t.Fatal("Dial SUCCEEDED against a refused address")
	}
	if !errors.Is(err, egress.ErrBlockedAddress) {
		t.Fatalf("err = %v, want one wrapping egress.ErrBlockedAddress", err)
	}
	var se *Error
	if !errors.As(err, &se) || se.Kind != ErrorKindConfig {
		t.Errorf("err = %#v, want a *sync.Error of kind config (a refusal is not a network blip)", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, host) {
		t.Errorf("refusal does not name the host %q: %v", host, msg)
	}
	if !strings.Contains(msg, "MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS") {
		t.Errorf("refusal does not name the way out: %v", msg)
	}
	if strings.Contains(msg, guardTestPassword) {
		t.Errorf("refusal leaks the password: %v", msg)
	}
}

func TestDialRefusesNonPublicResolutionWithoutConnecting(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::ffff:127.0.0.1", "fe80::1%eth0"} {
		t.Run(addr, func(t *testing.T) {
			ln := egresstest.Listen(t, serveIMAP)
			sess, err := Dial(context.Background(), guardConfig("localhost", ln.Port(),
				egresstest.NewResolver(t, []string{addr})))
			if sess != nil {
				_ = sess.Close()
			}
			// The socket first: it is the observation that matters, and assertRefused
			// stops the test.
			if n := ln.Connections(t); n != 0 {
				t.Errorf("resolving to %s: %d connection(s) reached the listener — the "+
					"credential follows a connection, so this is the harm itself", addr, n)
			}
			assertRefused(t, err, "localhost")
		})
	}
}

func TestDialAllowPrivateHostsConnects(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::ffff:127.0.0.1"} {
		t.Run(addr, func(t *testing.T) {
			ln := egresstest.Listen(t, serveIMAP)
			cfg := guardConfig("localhost", ln.Port(), egresstest.NewResolver(t, []string{addr}))
			cfg.AllowPrivateHosts = true
			sess, err := Dial(context.Background(), cfg)
			if err != nil {
				t.Fatalf("AllowPrivateHosts did not permit %s: %v", addr, err)
			}
			_ = sess.Close()
			if n := ln.Connections(t); n != 1 {
				t.Errorf("listener saw %d connection(s), want exactly 1", n)
			}
		})
	}
}

// TestDialRefusesARebindAfterValidation: validation's check saw a public
// answer; the sync pass's dial gets loopback. The dial must be refused and the
// loopback listener must see nothing.
func TestDialRefusesARebindAfterValidation(t *testing.T) {
	ln := egresstest.Listen(t, serveIMAP)
	r := egresstest.NewResolver(t, []string{"93.184.216.34"}, []string{"127.0.0.1"})

	// Validation: onboarding's pre-dial check (egress.CheckHost) passes.
	if err := egress.CheckHost(context.Background(), r, "localhost"); err != nil {
		t.Fatalf("validation-time check refused the public answer: %v", err)
	}

	// A later sync pass.
	sess, err := Dial(context.Background(), guardConfig("localhost", ln.Port(), r))
	if sess != nil {
		_ = sess.Close()
	}
	if n := ln.Connections(t); n != 0 {
		t.Errorf("the rebound sync dial reached the loopback listener (%d connection(s))", n)
	}
	assertRefused(t, err, "localhost")
}

// ---- TLS still verifies the HOSTNAME -----------------------------------------

// tlsIMAPListener serves implicit-TLS IMAP with a certificate carrying exactly
// the given DNS names and IPs, signed by a fresh CA the returned pool trusts.
func tlsIMAPListener(t *testing.T, dnsNames []string, ips []net.IP) (*egresstest.Listener, *x509.CertPool) {
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
	caCert, _ := x509.ParseCertificate(caDER)
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
	srvCfg := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	ln := egresstest.Listen(t, func(c net.Conn) {
		tc := tls.Server(c, srvCfg)
		if err := tc.Handshake(); err != nil {
			_ = c.Close()
			return
		}
		serveIMAP(tc)
	})
	return ln, pool
}

// TestGuardedTLSDialVerifiesTheHostname: the guard dials an IP literal, and
// that must not change WHOSE certificate is accepted. The account's hostname
// resolves (through the guard) to 127.0.0.1:
//
//   - a certificate for the hostname is accepted — dialling the literal did
//     not break verification;
//   - a certificate valid ONLY for the IP 127.0.0.1 is REFUSED — so
//     verification is against the name the operator typed, not the address
//     the guard happened to dial.
//
// It drives dialClient with Dial's own TLS config (cfg.imapTLSConfig(), which
// sets ServerName) plus the test CA, so ServerName is production-derived.
func TestGuardedTLSDialVerifiesTheHostname(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dns    []string
		ips    []net.IP
		wantOK bool
	}{
		{"cert for the hostname", []string{"imap.example.com"}, nil, true},
		{"cert only for the dialled IP", nil, []net.IP{net.ParseIP("127.0.0.1")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, pool := tlsIMAPListener(t, tc.dns, tc.ips)
			cfg := Config{
				Host: "imap.example.com", Port: ln.Port(),
				Username: "u", Password: "p", TLS: TLSImplicit, DialTimeout: 5 * time.Second,
				AllowPrivateHosts: true, // the test server is on loopback
				Resolver:          egresstest.NewResolver(t, []string{"127.0.0.1"}),
			}
			tlsCfg := cfg.imapTLSConfig()
			tlsCfg.RootCAs = pool
			if tlsCfg.ServerName != "imap.example.com" {
				t.Fatalf("ServerName = %q, want the hostname", tlsCfg.ServerName)
			}
			client, err := dialClient(context.Background(), cfg, &imapclient.Options{TLSConfig: tlsCfg})
			if client != nil {
				defer func() { _ = client.Close() }()
			}
			if tc.wantOK && err != nil {
				t.Fatalf("a certificate for the hostname was refused through the guard: %v", err)
			}
			if !tc.wantOK {
				if err == nil {
					t.Fatal("a certificate valid only for the dialled IP was ACCEPTED — TLS is verifying the literal, not the hostname")
				}
				var herr x509.HostnameError
				if !errors.As(err, &herr) {
					t.Errorf("err = %v, want an x509.HostnameError (a hostname mismatch, not some other failure)", err)
				}
			}
			if n := ln.Connections(t); n != 1 {
				t.Errorf("listener saw %d connection(s), want 1 (the dial went through the guard to the checked literal)", n)
			}
		})
	}
}
