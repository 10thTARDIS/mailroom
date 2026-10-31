// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mailroom/internal/xoauth2"
)

// SMTP XOAUTH2 (ADR-046), against a scripted server that speaks the mechanism
// the way Gmail documents it: the initial response on the AUTH line, and on
// failure a 334 challenge carrying base64 JSON that the client must answer with
// an EMPTY line before the server sends 535.

const smtpTestToken = xoauth2.Secret("ya29.SMTP-SENTINEL-ACCESS-TOKEN-do-not-log")

type scriptedXOAuth2SMTP struct {
	mu sync.Mutex
	// irs are the decoded initial responses received; replies the lines the
	// client sent in answer to a 334 challenge (raw, before decoding).
	irs     []string
	replies []string
	cmds    []string
}

func (s *scriptedXOAuth2SMTP) snapshot() (irs, replies, cmds []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.irs...), append([]string(nil), s.replies...), append([]string(nil), s.cmds...)
}

// startXOAuth2SMTP serves until the test ends. accept decides the verdict for
// an initial response; when it refuses, the server sends the failure challenge
// (or, with bareRefusal, answers 535 at once).
func startXOAuth2SMTP(t *testing.T, mechs string, accept func(ir string) bool, bareRefusal bool) (*scriptedXOAuth2SMTP, string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &scriptedXOAuth2SMTP{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, mechs, accept, bareRefusal)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return s, h, n
}

func (s *scriptedXOAuth2SMTP) serve(conn net.Conn, mechs string, accept func(string) bool, bareRefusal bool) {
	defer func() { _ = conn.Close() }()
	w := bufio.NewWriter(conn)
	r := bufio.NewReader(conn)
	put := func(str string) { _, _ = w.WriteString(str + "\r\n"); _ = w.Flush() }
	put("220 scripted ESMTP")
	for {
		raw, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(raw, "\r\n")
		s.mu.Lock()
		s.cmds = append(s.cmds, cmd)
		s.mu.Unlock()
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			put("250-scripted")
			put("250 AUTH " + mechs)
		case strings.HasPrefix(upper, "AUTH XOAUTH2 "):
			dec, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(cmd[len("AUTH XOAUTH2 "):]))
			s.mu.Lock()
			s.irs = append(s.irs, string(dec))
			s.mu.Unlock()
			if accept(string(dec)) {
				put("235 2.7.0 Accepted")
				continue
			}
			if bareRefusal {
				put("535 5.7.3 Authentication unsuccessful")
				continue
			}
			put("334 " + base64.StdEncoding.EncodeToString(
				[]byte(`{"status":"401","schemes":"bearer","scope":"https://mail.google.com/"}`)))
			reply, err := r.ReadString('\n')
			if err != nil {
				return
			}
			s.mu.Lock()
			s.replies = append(s.replies, strings.TrimRight(reply, "\r\n"))
			s.mu.Unlock()
			put("535-5.7.8 Username and Password not accepted.")
			put("535 5.7.8 https://support.example.com/mail/?p=BadCredentials")
		case upper == "DATA":
			put("354 go ahead")
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
			}
			put("250 2.0.0 queued")
		case strings.HasPrefix(upper, "QUIT"):
			put("221 bye")
			return
		default:
			put("250 OK")
		}
	}
}

func wantIR(user string) string {
	return "user=" + user + "\x01auth=Bearer " + smtpTestToken.Reveal() + "\x01\x01"
}

func TestXOAuth2SMTPSuccess(t *testing.T) {
	srv, host, port := startXOAuth2SMTP(t, "LOGIN XOAUTH2", func(ir string) bool { return ir == wantIR("you@example.com") }, false)
	res, err := loopbackSender.Send(context.Background(), SendRequest{
		Host: host, Port: port, Auth: XOAuth2Auth("you@example.com", xoauth2.StaticToken(smtpTestToken)),
		MailFrom: "you@example.com", RcptTo: []string{"them@example.net"}, Data: []byte("Subject: t\r\n\r\nx\r\n"),
		Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Response == "" {
		t.Error("no response recorded")
	}
	irs, _, cmds := srv.snapshot()
	if len(irs) != 1 || irs[0] != wantIR("you@example.com") {
		t.Fatalf("server saw initial responses %q, want exactly the XOAUTH2 one", irs)
	}
	for _, c := range cmds {
		if strings.HasPrefix(strings.ToUpper(c), "AUTH PLAIN") {
			t.Errorf("an OAuth send also tried AUTH PLAIN: %q", cmds)
		}
	}
}

// The failure challenge is answered with an EMPTY line (not "=", not the token
// again), the 535 that follows is a permanent failure, and neither the token
// nor the challenge's scope reaches the error.
func TestXOAuth2SMTPFailureChallenge(t *testing.T) {
	srv, host, port := startXOAuth2SMTP(t, "XOAUTH2", func(string) bool { return false }, false)
	_, err := loopbackSender.Send(context.Background(), SendRequest{
		Host: host, Port: port, Auth: XOAuth2Auth("you@example.com", xoauth2.StaticToken(smtpTestToken)),
		MailFrom: "you@example.com", RcptTo: []string{"them@example.net"}, Data: []byte("x"),
		Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if err == nil {
		t.Fatal("Send succeeded against a refusing server")
	}
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("err = %v, want ErrPermanent (a 535)", err)
	}
	if strings.Contains(err.Error(), smtpTestToken.Reveal()) {
		t.Fatalf("the access token leaked into the error: %v", err)
	}
	// Two conversations (the refused token is rejected and the send retries
	// once on a fresh connection, ADR-046 §5), each challenge answered with
	// one empty line.
	_, replies, cmds := srv.snapshot()
	if len(replies) != 2 || replies[0] != "" || replies[1] != "" {
		t.Fatalf("client answered the challenges with %q, want two empty lines (attempt + one retry)", replies)
	}
	for _, c := range cmds {
		if strings.HasPrefix(strings.ToUpper(c), "MAIL") {
			t.Errorf("MAIL FROM was sent after a refused AUTH: %q", cmds)
		}
	}
}

// Microsoft's shape: 535 straight away, no challenge.
func TestXOAuth2SMTPBareRefusal(t *testing.T) {
	_, host, port := startXOAuth2SMTP(t, "LOGIN XOAUTH2", func(string) bool { return false }, true)
	err := loopbackSender.CheckAuth(context.Background(), SendRequest{
		Host: host, Port: port, Auth: XOAuth2Auth("you@example.com", xoauth2.StaticToken(smtpTestToken)),
		Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("CheckAuth = %v, want ErrPermanent", err)
	}
	if strings.Contains(err.Error(), smtpTestToken.Reveal()) {
		t.Fatalf("the access token leaked into the error: %v", err)
	}
}

// A server that does not offer XOAUTH2 is never sent the token.
func TestXOAuth2SMTPMechanismNotOffered(t *testing.T) {
	srv, host, port := startXOAuth2SMTP(t, "PLAIN LOGIN", func(string) bool { return true }, false)
	err := loopbackSender.CheckAuth(context.Background(), SendRequest{
		Host: host, Port: port, Auth: XOAuth2Auth("you@example.com", xoauth2.StaticToken(smtpTestToken)),
		Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if err == nil {
		t.Fatal("CheckAuth succeeded against a server without XOAUTH2")
	}
	if _, _, cmds := srv.snapshot(); containsPrefix(cmds, "AUTH") {
		t.Errorf("the server was sent AUTH although it never offered XOAUTH2: %q", cmds)
	}
}

// The mechanism itself refuses an unencrypted connection to anything but
// localhost, and a server name that is not the host the token was meant for —
// net/smtp's PlainAuth rules, which the token deserves as much as a password.
// (The end-to-end tests above run on 127.0.0.1, which those rules exempt,
// exactly as they exempt it for PLAIN.)
func TestXOAuth2SMTPRefusesCleartextAndWrongHost(t *testing.T) {
	a, err := XOAuth2Auth("you@example.com", xoauth2.StaticToken(smtpTestToken)).client(context.Background(), "smtp.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		info smtp.ServerInfo
		want error
	}{
		{"no TLS to a remote host", smtp.ServerInfo{Name: "smtp.example.com", TLS: false, Auth: []string{"XOAUTH2"}}, errXOAuth2NoTLS},
		{"TLS but another host", smtp.ServerInfo{Name: "evil.example.net", TLS: true, Auth: []string{"XOAUTH2"}}, errXOAuth2WrongHost},
		{"not offered", smtp.ServerInfo{Name: "smtp.example.com", TLS: true, Auth: []string{"PLAIN"}}, errXOAuth2Mechanism},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ir, err := a.Start(&tc.info)
			if !errors.Is(err, tc.want) {
				t.Errorf("Start = %v, want %v", err, tc.want)
			}
			if ir != nil {
				t.Errorf("Start produced an initial response (%d bytes) while refusing", len(ir))
			}
		})
	}
	// Positive control: the same mechanism over TLS to the right host proceeds.
	mech, ir, err := a.Start(&smtp.ServerInfo{Name: "smtp.example.com", TLS: true, Auth: []string{"XOAUTH2"}})
	if err != nil || mech != "XOAUTH2" || string(ir) != wantIR("you@example.com") {
		t.Fatalf("Start over TLS = %q, %d bytes, %v; want XOAUTH2 and the initial response", mech, len(ir), err)
	}
}

// REGRESSION: a password submission (Auth nil, Username set) still sends AUTH
// PLAIN with exactly those credentials, and PasswordAuth is the same thing.
func TestPasswordSubmissionUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  SendRequest
	}{
		{"Username/Password fields", SendRequest{Username: "u@example.com", Password: "pw-1"}},
		{"PasswordAuth", SendRequest{Auth: PasswordAuth("u@example.com", "pw-1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, host, port := startRecordingSMTP(t, []string{"AUTH PLAIN"})
			req := tc.req
			req.Host, req.Port, req.Timeout, req.AllowInsecureNoTLS = host, port, 5*time.Second, true
			if err := loopbackSender.CheckAuth(context.Background(), req); err != nil {
				t.Fatalf("CheckAuth: %v", err)
			}
			want := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00u@example.com\x00pw-1"))
			if !containsExact(srv.seen(), want) {
				t.Errorf("server saw %q, want %q", srv.seen(), want)
			}
		})
	}
}

func containsPrefix(cmds []string, prefix string) bool {
	for _, c := range cmds {
		if strings.HasPrefix(strings.ToUpper(c), prefix) {
			return true
		}
	}
	return false
}

func containsExact(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}
