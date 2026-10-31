// SPDX-License-Identifier: AGPL-3.0-only

package sieve

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"mailroom/internal/sieve/sievetest"
)

const fakeActive = "require [\"fileinto\", \"vacation\"];\r\n" +
	"if header :contains \"subject\" \"x\" { fileinto \"Junk\"; stop; }\r\n" +
	"vacation :days 3 :subject \"Away\" text:\r\nI am away.\r\n.\r\n;\r\n"

func newFake(t *testing.T, opts sievetest.Options) *sievetest.Server {
	t.Helper()
	if opts.Username == "" {
		opts.Username, opts.Password = "alice@example.com", "s3cret-pw"
	}
	if opts.Scripts == nil {
		opts.Scripts = []sievetest.Script{
			{Name: "roundcube", Body: fakeActive, Active: true},
			{Name: "old \"quoted\" name", Body: "keep;\r\n"},
		}
	}
	return sievetest.New(t, opts)
}

func openFake(t *testing.T, f *sievetest.Server, mutate func(*Config)) (*Session, error) {
	t.Helper()
	cfg := Config{
		Host: f.Host(), Port: f.Port(),
		Username: "alice@example.com", Password: "s3cret-pw",
		AllowPrivateHosts: true, TLSConfig: f.ClientTLSConfig(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return Open(context.Background(), cfg)
}

func TestOpenStartTLSListAndGet(t *testing.T) {
	f := newFake(t, sievetest.Options{})
	s, err := openFake(t, f, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.TLS() == "" {
		t.Fatal("STARTTLS was offered but the session is not over TLS")
	}
	c := s.Capabilities()
	if c.Implementation != "Dovecot Pigeonhole" || c.Version != "1.0" {
		t.Errorf("caps = %+v", c)
	}
	if !c.HasSieve("vacation") || !c.HasSieve("INCLUDE") || c.HasSieve("vacation-seconds") {
		t.Errorf("sieve extensions parsed wrong: %v", c.Sieve)
	}
	if c.Other["MAXREDIRECTS"] != "4" {
		t.Errorf("post-auth CAPABILITY not used: other = %v", c.Other)
	}
	if got := s.PreAuthCapabilities().SASL; !reflect.DeepEqual(got, []string{"PLAIN"}) {
		t.Errorf("pre-auth SASL (after TLS) = %v, want [PLAIN]", got)
	}

	scripts, err := s.ListScripts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Script{{Name: "roundcube", Active: true}, {Name: "old \"quoted\" name"}}
	if !reflect.DeepEqual(scripts, want) {
		t.Errorf("scripts = %+v, want %+v", scripts, want)
	}
	body, err := s.GetScript(context.Background(), "roundcube")
	if err != nil || body != fakeActive {
		t.Errorf("GETSCRIPT = %q, %v", body, err)
	}
	// A name with a quote in it survives our quoting and the server's parse.
	if body, err := s.GetScript(context.Background(), "old \"quoted\" name"); err != nil || body != "keep;\r\n" {
		t.Errorf("GETSCRIPT of a quoted name = %q, %v", body, err)
	}
	_, err = s.GetScript(context.Background(), "missing")
	var se *ServerError
	if !errors.As(err, &se) || se.Code != "NONEXISTENT" || se.Status != "NO" {
		t.Errorf("missing script: %v", err)
	}
	if err := s.Logout(context.Background()); err != nil {
		t.Errorf("logout: %v", err)
	}
}

func TestOpenImplicitTLS(t *testing.T) {
	f := newFake(t, sievetest.Options{TLS: sievetest.ImplicitTLS})
	s, err := openFake(t, f, func(c *Config) { c.ImplicitTLS = true })
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if s.TLS() == "" {
		t.Fatal("implicit TLS session reports cleartext")
	}
	for _, v := range f.Verbs() {
		if v == "STARTTLS" {
			t.Error("STARTTLS sent on an implicit-TLS session")
		}
	}
}

// TestRefusesToAuthenticateInCleartext: a server offering no TLS gets no
// AUTHENTICATE, and the credential never crosses the wire in any form.
func TestRefusesToAuthenticateInCleartext(t *testing.T) {
	f := newFake(t, sievetest.Options{TLS: sievetest.NoTLS})
	_, err := openFake(t, f, nil)
	if !errors.Is(err, ErrNoTLS) {
		t.Fatalf("err = %v, want ErrNoTLS", err)
	}
	for _, v := range f.Verbs() {
		if v == "AUTHENTICATE" {
			t.Fatal("AUTHENTICATE was sent over cleartext")
		}
	}
	raw := f.Raw()
	ir := base64.StdEncoding.EncodeToString([]byte("\x00alice@example.com\x00s3cret-pw"))
	if bytes.Contains(raw, []byte(ir)) || bytes.Contains(raw, []byte("s3cret-pw")) {
		t.Fatal("the credential reached the wire in cleartext")
	}
}

// TestCleartextAllowedOnlyWhenExplicit is the positive control for the test
// above: the same server WITH the test-only switch authenticates, so the
// refusal above is the TLS rule, not some other failure.
func TestCleartextAllowedOnlyWhenExplicit(t *testing.T) {
	f := newFake(t, sievetest.Options{TLS: sievetest.NoTLS})
	s, err := openFake(t, f, func(c *Config) { c.AllowInsecureNoTLS = true })
	if err != nil {
		t.Fatalf("open with AllowInsecureNoTLS: %v", err)
	}
	_ = s.Close()
}

func TestWrongPasswordIsAServerError(t *testing.T) {
	f := newFake(t, sievetest.Options{})
	_, err := openFake(t, f, func(c *Config) { c.Password = "wrong-pw-marker" })
	var se *ServerError
	if !errors.As(err, &se) || se.Verb != "AUTHENTICATE" {
		t.Fatalf("err = %v, want a ServerError from AUTHENTICATE", err)
	}
	if strings.Contains(err.Error(), "wrong-pw-marker") {
		t.Fatal("the error carries the password")
	}
}

func TestNoPlainMechanismIsRefusedBeforeAuth(t *testing.T) {
	f := newFake(t, sievetest.Options{SASL: "SCRAM-SHA-256"})
	_, err := openFake(t, f, nil)
	if !errors.Is(err, ErrNoPlainAuth) {
		t.Fatalf("err = %v, want ErrNoPlainAuth", err)
	}
}

// TestEgressGuardRefusesBeforeDialling: with private hosts not allowed, a
// loopback ManageSieve host is refused and the server never sees a connection.
func TestEgressGuardRefusesBeforeDialling(t *testing.T) {
	f := newFake(t, sievetest.Options{})
	_, err := openFake(t, f, func(c *Config) { c.AllowPrivateHosts = false })
	if !errors.Is(err, ErrHostNotPermitted) {
		t.Fatalf("err = %v, want ErrHostNotPermitted", err)
	}
	if len(f.Raw()) != 0 || len(f.Verbs()) != 0 {
		t.Fatal("the refused host was dialled anyway")
	}
}

// TestWriteVerbNeverReachesTheWire is the read-only guarantee: Session.do, the
// only writer, refuses every state-changing verb (in any case) before a byte
// is written, and the fake — which records every byte — never sees one.
func TestWriteVerbNeverReachesTheWire(t *testing.T) {
	f := newFake(t, sievetest.Options{})
	s, err := openFake(t, f, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	writes := [][]string{
		{"PUTSCRIPT", "mailroom", "vacation \"x\";"},
		{"SETACTIVE", "mailroom"},
		{"SETACTIVE", ""},
		{"DELETESCRIPT", "roundcube"},
		{"RENAMESCRIPT", "roundcube", "gone"},
		{"CHECKSCRIPT", "keep;"},
		{"HAVESPACE", "x", "1"},
		{"putscript", "lower", "keep;"},
		{"UNAUTHENTICATE"},
	}
	for _, w := range writes {
		if _, err := s.do(w[0], w[1:]...); !errors.Is(err, ErrVerbNotPermitted) {
			t.Errorf("do(%s) = %v, want ErrVerbNotPermitted", w[0], err)
		}
	}
	// The session is still usable: nothing half-written desynchronised it.
	if _, err := s.ListScripts(context.Background()); err != nil {
		t.Fatalf("session broken after refused verbs: %v", err)
	}
	if err := s.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.WriteAttempts(); n != 0 {
		t.Fatalf("the server received %d state-changing verb(s)", n)
	}
	up := strings.ToUpper(string(f.Raw()))
	for _, v := range []string{"PUTSCRIPT", "SETACTIVE", "DELETESCRIPT", "RENAMESCRIPT", "CHECKSCRIPT", "HAVESPACE", "UNAUTHENTICATE"} {
		if strings.Contains(up, v) {
			t.Errorf("%s reached the wire", v)
		}
	}
	// And every verb that DID reach it is a read verb.
	for _, v := range f.Verbs() {
		if !readVerbs[v] {
			t.Errorf("non-read verb %q reached the server", v)
		}
	}
}

// TestSessionMethodSetIsReadOnly pins Session's exported surface. Adding a
// write method (PutScript, SetActive, ...) must be a deliberate change to this
// list, reviewed as the end of "report first" (#50), not an accident.
func TestSessionMethodSetIsReadOnly(t *testing.T) {
	typ := reflect.TypeOf(&Session{})
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	want := []string{"Capabilities", "Close", "GetScript", "ListScripts", "Logout", "PreAuthCapabilities", "TLS"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Session's exported methods = %v, want exactly %v", got, want)
	}
	vs := make([]string, 0, len(readVerbs))
	for v := range readVerbs {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	wantVerbs := []string{"AUTHENTICATE", "CAPABILITY", "GETSCRIPT", "LISTSCRIPTS", "LOGOUT", "STARTTLS"}
	if !reflect.DeepEqual(vs, wantVerbs) {
		t.Fatalf("readVerbs = %v, want exactly %v", vs, wantVerbs)
	}
}
