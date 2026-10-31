// SPDX-License-Identifier: AGPL-3.0-only

//go:build sievelive

package sieve

// Live tests against a REAL Dovecot Pigeonhole ManageSieve server
// (scripts/sieve-live-test.sh starts the stock dovecot/dovecot image, whose
// static passdb accepts any user with the password "pass"). The fake server in
// sievetest copies Pigeonhole's bytes; this is what keeps that copy honest.
//
// It FAILS rather than skips when the server is not configured: a protocol
// suite that skips when the server is missing goes green in exactly the
// situation it exists to catch (#263, #159).

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const liveMainScript = "require [\"fileinto\", \"vacation\", \"include\"];\r\n" +
	"# a comment with \"quotes\" and ; semicolons\r\n" +
	"if anyof (header :contains \"subject\" \"[list]\", address :domain :is \"from\" \"example.com\") {\r\n" +
	"  fileinto \"Lists\";\r\n" +
	"  stop;\r\n" +
	"}\r\n" +
	"vacation :days 7 :subject \"Away\" :addresses [\"me@example.com\"] text:\r\n" +
	"I am away until Monday.\r\n" +
	"..a dot-stuffed line\r\n" +
	".\r\n" +
	";\r\n"

const liveOtherScript = "keep;\r\n"

func liveAddr(t *testing.T) (string, int) {
	t.Helper()
	host := os.Getenv("MAILROOM_TEST_SIEVE_HOST")
	port, _ := strconv.Atoi(os.Getenv("MAILROOM_TEST_SIEVE_PORT"))
	if host == "" || port == 0 {
		t.Fatal("MAILROOM_TEST_SIEVE_HOST/PORT unset: run via scripts/sieve-live-test.sh (a skip here would be green with no server)")
	}
	return host, port
}

func liveUser(t *testing.T) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "live" + hex.EncodeToString(b)
}

// seedRaw writes scripts with the RAW protocol, deliberately not through this
// package: the client under test has no write verbs, and must not grow any for
// a test's convenience.
func seedRaw(t *testing.T, host string, port int, user string, cmds ...string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 10*time.Second)
	if err != nil {
		t.Fatalf("seed dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(conn)
	expectOK := func(what string) {
		for {
			ln, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("seed %s: %v", what, err)
			}
			if strings.HasPrefix(ln, "OK") {
				return
			}
			if strings.HasPrefix(ln, "NO") || strings.HasPrefix(ln, "BYE") {
				t.Fatalf("seed %s: %s", what, strings.TrimSpace(ln))
			}
		}
	}
	expectOK("greeting")
	_, _ = fmt.Fprint(conn, "STARTTLS\r\n")
	expectOK("STARTTLS")
	// The stock image ships a self-signed certificate for no particular name;
	// this suite tests the protocol, and TLS verification is covered by the
	// unit tests against sievetest's CA-trusted certificate.
	tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("seed tls: %v", err)
	}
	br = bufio.NewReader(tc)
	conn = tc
	expectOK("post-TLS capabilities")
	ir := base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00pass"))
	_, _ = fmt.Fprintf(conn, "AUTHENTICATE \"PLAIN\" \"%s\"\r\n", ir)
	expectOK("AUTHENTICATE")
	for _, c := range cmds {
		_, _ = fmt.Fprint(conn, c)
		expectOK(strings.SplitN(c, " ", 2)[0])
	}
	_, _ = fmt.Fprint(conn, "LOGOUT\r\n")
}

func putRaw(name, body string) string {
	return fmt.Sprintf("PUTSCRIPT %q {%d+}\r\n%s\r\n", name, len(body), body)
}

func liveOpen(t *testing.T, host string, port int, user, pass string) (*Session, error) {
	t.Helper()
	return Open(context.Background(), Config{
		Host: host, Port: port, Username: user, Password: pass,
		AllowPrivateHosts: true,                                  // the server is on the CI docker network
		TLSConfig:         &tls.Config{InsecureSkipVerify: true}, // stock self-signed image; see seedRaw
	})
}

func TestLiveReadsWhatWasSeeded(t *testing.T) {
	host, port := liveAddr(t)
	user := liveUser(t)
	seedRaw(t, host, port, user,
		putRaw("main", liveMainScript), putRaw("other script", liveOtherScript), "SETACTIVE \"main\"\r\n")

	s, err := liveOpen(t, host, port, user, "pass")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.TLS() == "" {
		t.Error("session is not over TLS")
	}
	c := s.Capabilities()
	if !strings.Contains(c.Implementation, "Pigeonhole") {
		t.Errorf("implementation = %q", c.Implementation)
	}
	for _, ext := range []string{"vacation", "include", "fileinto"} {
		if !c.HasSieve(ext) {
			t.Errorf("stock Pigeonhole should advertise %q; got %v", ext, c.Sieve)
		}
	}
	if c.HasSieve("vacation-seconds") {
		t.Error("vacation-seconds is off in stock Pigeonhole; the parse invented it")
	}
	if c.Other["MAXREDIRECTS"] == "" {
		t.Errorf("post-auth CAPABILITY should carry MAXREDIRECTS; other = %v", c.Other)
	}
	if got := s.PreAuthCapabilities().SASL; len(got) == 0 || got[0] != "PLAIN" {
		t.Errorf("post-TLS, pre-auth SASL = %v, want PLAIN", got)
	}

	scripts, err := s.ListScripts(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(scripts) != 2 {
		t.Fatalf("scripts = %+v, want 2", scripts)
	}
	byName := map[string]bool{}
	for _, sc := range scripts {
		byName[sc.Name] = sc.Active
	}
	if a, ok := byName["main"]; !ok || !a {
		t.Errorf("main should be listed ACTIVE: %+v", scripts)
	}
	if a, ok := byName["other script"]; !ok || a {
		t.Errorf("'other script' should be listed inactive: %+v", scripts)
	}

	body, err := s.GetScript(context.Background(), "main")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if body != liveMainScript {
		t.Errorf("GETSCRIPT body differs from what was uploaded:\n got %q\nwant %q", body, liveMainScript)
	}
	sum := Summarize(body)
	if !sum.Complete || !sum.Vacation || sum.Commands["fileinto"] != 1 || sum.Tests["anyof"] != 1 {
		t.Errorf("summary of the real round-tripped body is wrong: %+v", sum)
	}

	_, err = s.GetScript(context.Background(), "does-not-exist")
	var se *ServerError
	if !errors.As(err, &se) || se.Code != "NONEXISTENT" {
		t.Errorf("GETSCRIPT of a missing script: err = %v, want a NO (NONEXISTENT)", err)
	}
	if err := s.Logout(context.Background()); err != nil {
		t.Errorf("logout: %v", err)
	}

	// Nothing the read client did changed the server: still two scripts, main active.
	s2, err := liveOpen(t, host, port, user, "pass")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	again, err := s2.ListScripts(context.Background())
	if err != nil || len(again) != 2 {
		t.Fatalf("after the read session: %+v, %v", again, err)
	}
}

func TestLiveWrongPasswordIsAServerNo(t *testing.T) {
	host, port := liveAddr(t)
	_, err := liveOpen(t, host, port, liveUser(t), "not-the-password")
	var se *ServerError
	if !errors.As(err, &se) || se.Verb != "AUTHENTICATE" || se.Status != "NO" {
		t.Fatalf("err = %v, want a NO to AUTHENTICATE", err)
	}
	if strings.Contains(err.Error(), "not-the-password") {
		t.Fatal("the error text carries the password")
	}
}

func TestLiveNoActiveScript(t *testing.T) {
	host, port := liveAddr(t)
	user := liveUser(t)
	seedRaw(t, host, port, user, putRaw("inactive", liveOtherScript))
	s, err := liveOpen(t, host, port, user, "pass")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	scripts, err := s.ListScripts(context.Background())
	if err != nil || len(scripts) != 1 || scripts[0].Active {
		t.Fatalf("scripts = %+v, %v; want one inactive", scripts, err)
	}
}
