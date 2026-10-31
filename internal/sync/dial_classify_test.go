// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"mailroom/internal/egress/egresstest"
)

// THE KIND A FAILED DIAL CARRIES IS DECIDED BY WHICH STEP FAILED (#888). Dial
// used to report a refused LOGIN under op "dial", so classifyRefusal's
// `op == "login"` branch (a bare `NO` to LOGIN is a credentials problem) never
// ran from the real path and Microsoft's documented refusal,
// `NO AUTHENTICATE failed.`, came out as "protocol". The existing classify
// tests call classify("login", ...) directly and so could not see it; these
// drive the real Dial against a scripted server.

// loginScript greets with greeting (dropping the connection when it is
// empty), answers LOGIN with loginReply and STARTTLS with startTLSReply (the
// tag is prepended to each).
func loginScript(greeting, loginReply, startTLSReply string) func(net.Conn) {
	return func(c net.Conn) {
		defer func() { _ = c.Close() }()
		w := bufio.NewWriter(c)
		r := bufio.NewReader(c)
		put := func(s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }
		if greeting == "" {
			return // drop the connection at the greeting
		}
		put(greeting)
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
				put("* CAPABILITY IMAP4rev1 STARTTLS AUTH=PLAIN")
				put(tag + " OK CAPABILITY completed")
			case "STARTTLS":
				put(tag + " " + startTLSReply)
				return
			case "LOGIN":
				put(tag + " " + loginReply)
			case "LOGOUT":
				put("* BYE")
				put(tag + " OK LOGOUT completed")
				return
			default:
				put(tag + " BAD unexpected")
			}
		}
	}
}

func dialExpectingFailure(t *testing.T, mode TLSMode, handle func(net.Conn)) *Error {
	t.Helper()
	ln := egresstest.Listen(t, handle)
	cfg := guardConfig("localhost", ln.Port(), egresstest.NewResolver(t, []string{"127.0.0.1"}))
	cfg.AllowPrivateHosts = true
	cfg.TLS = mode
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := Dial(ctx, cfg)
	if sess != nil {
		_ = sess.Close()
		t.Fatal("Dial succeeded against a server that should have failed it")
	}
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("err = %#v, want *sync.Error", err)
	}
	return se
}

func TestDialClassifiesLoginRefusals(t *testing.T) {
	const greet = "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] ready"
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{"bare NO, Microsoft wording", "NO AUTHENTICATE failed."},
		{"bare NO, generic wording", "NO Login failed"},
		{"NO with AUTHENTICATIONFAILED (positive control)", "NO [AUTHENTICATIONFAILED] Invalid credentials"},
		{"NO with AUTHORIZATIONFAILED", "NO [AUTHORIZATIONFAILED] not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			se := dialExpectingFailure(t, TLSInsecure, loginScript(greet, tc.reply, ""))
			if se.Kind != ErrorKindAuth {
				t.Errorf("kind = %v, want auth (op %q, err %v)", se.Kind, se.Op, se.Err)
			}
			if se.Op != "login" {
				t.Errorf("op = %q, want login", se.Op)
			}
		})
	}
}

// Failures before LOGIN must keep their own op and must never read as a
// credentials problem: the server has not been shown the credentials, or has
// not yet said anything about them.
func TestDialNonLoginFailuresAreNotAuth(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode TLSMode
		srv  func(net.Conn)
	}{
		{"connection dropped at the greeting", TLSInsecure, loginScript("", "", "")},
		// A bare NO is exactly what op "login" turns into auth, so this proves
		// the greeting is settled under "dial" before LOGIN is classified.
		{"greeting refused with NO", TLSInsecure, loginScript("* NO server not ready", "NO AUTHENTICATE failed.", "")},
		// Implicit TLS against a plaintext server: the handshake cannot succeed.
		{"TLS handshake failure", TLSImplicit, loginScript("* OK ready", "NO AUTHENTICATE failed.", "")},
		{"STARTTLS refused with NO", TLSStartTLS,
			loginScript("* OK [CAPABILITY IMAP4rev1 STARTTLS] ready", "NO AUTHENTICATE failed.", "NO STARTTLS unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			se := dialExpectingFailure(t, tc.mode, tc.srv)
			if se.Kind == ErrorKindAuth {
				t.Errorf("classified auth (op %q, err %v)", se.Op, se.Err)
			}
			if se.Op != "dial" {
				t.Errorf("op = %q, want dial (err %v)", se.Op, se.Err)
			}
		})
	}
}
