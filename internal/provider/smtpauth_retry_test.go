// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mailroom/internal/xoauth2"
)

// The reject hook (ADR-046 §5, obligation (a) of §9), SMTP half: a 535 to an
// XOAUTH2 token hands that token back to the source and the submission is
// retried ONCE, on a fresh connection, with whatever the source issues next.

// seqSource hands out tokens in order (the last repeats), or err, and records
// every Reject.
type seqSource struct {
	mu       sync.Mutex
	tokens   []xoauth2.Secret
	err      error
	calls    int
	rejected []xoauth2.Secret
}

func (s *seqSource) Token(context.Context) (xoauth2.Secret, error) {
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

func (s *seqSource) Reject(tok xoauth2.Secret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected = append(s.rejected, tok)
}

func (s *seqSource) snapshot() (int, []xoauth2.Secret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, append([]xoauth2.Secret(nil), s.rejected...)
}

// Both refusal shapes (Gmail's challenge then 535, Microsoft's bare 535): the
// dead cached token is rejected and the refreshed one gets the message through.
func TestXOAuth2SMTPRejectedTokenIsDroppedAndRetriedOnce(t *testing.T) {
	const fresh = xoauth2.Secret("ya29.SMTP-SENTINEL-FRESH-do-not-log")
	for _, bare := range []bool{false, true} {
		t.Run("bare="+strconv.FormatBool(bare), func(t *testing.T) {
			src := &seqSource{tokens: []xoauth2.Secret{smtpTestToken, fresh}}
			srv, host, port := startXOAuth2SMTP(t, "XOAUTH2", func(ir string) bool {
				return strings.Contains(ir, fresh.Reveal())
			}, bare)
			_, err := loopbackSender.Send(context.Background(), SendRequest{
				Host: host, Port: port, Auth: XOAuth2Auth("you@example.com", src),
				MailFrom: "you@example.com", RcptTo: []string{"them@example.net"}, Data: []byte("x"),
				Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
			})
			if err != nil {
				t.Fatalf("Send: %v — a refused cached token was not refreshed and retried", err)
			}
			if _, rejected := src.snapshot(); len(rejected) != 1 || rejected[0] != smtpTestToken {
				t.Errorf("Reject calls = %d, want exactly one naming the refused token", len(rejected))
			}
			irs, _, cmds := srv.snapshot()
			if len(irs) != 2 || !containsPrefix(cmds, "MAIL") {
				t.Errorf("server saw %d AUTHs and MAIL=%v; want 2 and the message sent", len(irs), containsPrefix(cmds, "MAIL"))
			}
		})
	}
}

// One retry, not a loop: a server refusing every token sees exactly two AUTHs.
func TestXOAuth2SMTPRetryIsNotALoop(t *testing.T) {
	src := &seqSource{tokens: []xoauth2.Secret{smtpTestToken, "ya29.B-sentinel", "ya29.C-sentinel"}}
	srv, host, port := startXOAuth2SMTP(t, "XOAUTH2", func(string) bool { return false }, true)
	err := loopbackSender.CheckAuth(context.Background(), SendRequest{
		Host: host, Port: port, Auth: XOAuth2Auth("you@example.com", src),
		Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("CheckAuth = %v, want ErrPermanent", err)
	}
	// SECURITY REVIEW L1: the FINAL refusal is rejected too, so the token
	// refused twice (the refreshed one) does not stay cached for the next
	// send to be refused with again.
	calls, rejected := src.snapshot()
	if irs, _, _ := srv.snapshot(); len(irs) != 2 || calls != 2 || len(rejected) != 2 ||
		rejected[0] != smtpTestToken || rejected[1] != "ya29.B-sentinel" {
		t.Errorf("AUTHs=%d Token calls=%d Rejects=%d; want 2, 2, and both refused tokens in order", len(irs), calls, len(rejected))
	}
}

// A password 535 is final: it would only be refused again (and some servers
// lock an account out for repeated failures).
func TestPasswordSMTP535IsNotRetried(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	auths := 0
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				w, r := bufio.NewWriter(c), bufio.NewReader(c)
				put := func(s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }
				put("220 ready")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					u := strings.ToUpper(strings.TrimSpace(l))
					switch {
					case strings.HasPrefix(u, "EHLO"):
						put("250-x")
						put("250 AUTH PLAIN")
					case strings.HasPrefix(u, "AUTH"):
						mu.Lock()
						auths++
						mu.Unlock()
						put("535 5.7.8 Authentication credentials invalid")
					case strings.HasPrefix(u, "QUIT"):
						put("221 bye")
						return
					default:
						put("250 OK")
					}
				}
			}(conn)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	err = loopbackSender.CheckAuth(context.Background(), SendRequest{
		Host: h, Port: port, Username: "u@example.com", Password: "pw",
		Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("CheckAuth = %v, want ErrPermanent", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if auths != 1 {
		t.Errorf("password AUTH sent %d times, want 1", auths)
	}
}

// A failure to OBTAIN a token sends nothing and is classified by meaning:
// transient is retried by the send job with backoff; a grant needing the user
// or a broken client registration is permanent for this submission.
func TestXOAuth2SMTPTokenFailures(t *testing.T) {
	for _, tc := range []struct {
		sentinel  error
		permanent bool
	}{
		{xoauth2.ErrTokenUnavailable, false},
		{xoauth2.ErrReauthRequired, true},
		{xoauth2.ErrProviderMisconfigured, true},
	} {
		t.Run(tc.sentinel.Error(), func(t *testing.T) {
			srv, host, port := startXOAuth2SMTP(t, "XOAUTH2", func(string) bool { return true }, false)
			src := &seqSource{err: fmt.Errorf("refresh: %w", tc.sentinel)}
			err := loopbackSender.CheckAuth(context.Background(), SendRequest{
				Host: host, Port: port, Auth: XOAuth2Auth("you@example.com", src),
				Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
			})
			if err == nil || !errors.Is(err, tc.sentinel) || errors.Is(err, ErrPermanent) != tc.permanent {
				t.Fatalf("err = %v; want %v with permanent=%v", err, tc.sentinel, tc.permanent)
			}
			if irs, _, _ := srv.snapshot(); len(irs) != 0 {
				t.Error("an AUTH was sent without a token")
			}
		})
	}
}
