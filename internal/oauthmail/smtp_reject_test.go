// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailroom/internal/provider"
)

// SECURITY REVIEW L1 of #897, end to end through the real Manager cache: an
// SMTP server that refuses every token gets the cached token and its one
// refreshed replacement, and afterwards NEITHER is left in the cache — the
// next send refreshes instead of being refused with a dead token again.
func TestSMTPRefusedTwiceLeavesNoCachedToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
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
					switch u := strings.ToUpper(strings.TrimSpace(l)); {
					case strings.HasPrefix(u, "EHLO"):
						put("250-x")
						put("250 AUTH XOAUTH2")
					case strings.HasPrefix(u, "AUTH"):
						put("535 5.7.3 Authentication unsuccessful")
					default:
						put("221 bye")
						return
					}
				}
			}(c)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)

	lc := newLeakCheck(t)
	te, srv := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		okToken([]string{sentAccess1, sentAccess2}[min(int(n)-1, 1)], "")(n, f, w)
	})
	m := newManager(t, lc, srv, newFakeGrants(ProviderGoogle), ProviderGoogle, nil)
	if _, err := m.AccessToken(context.Background(), userA, acctA); lc.err(err) != nil { // cache: access1
		t.Fatal(err)
	}
	err = provider.SMTPSender{AllowPrivateHosts: true}.CheckAuth(context.Background(), provider.SendRequest{
		Host: h, Port: port, Auth: provider.XOAuth2Auth("you@example.com", m.TokenSource(userA, acctA)),
		Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if lc.err(err) == nil {
		t.Fatal("CheckAuth succeeded against a server refusing everything")
	}
	if te.calls.Load() != 2 {
		t.Fatalf("token endpoint calls = %d, want 2 (the initial token, then one refresh for the retry)", te.calls.Load())
	}
	if _, ok := m.cached(cacheKey{userA, acctA}); ok {
		t.Fatal("a token refused on the final attempt is still cached")
	}
}
