// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mailroom/internal/xoauth2"
)

// scriptedReplies is an SMTP server answering AUTH with authReply and MAIL
// FROM with mailReply, counting AUTH commands.
func scriptedReplies(t *testing.T, authReply, mailReply string) (host string, port int, auths func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	n := 0
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
					u := strings.ToUpper(strings.TrimSpace(l))
					switch {
					case strings.HasPrefix(u, "EHLO"):
						put("250-x")
						put("250 AUTH LOGIN XOAUTH2")
					case strings.HasPrefix(u, "AUTH"):
						mu.Lock()
						n++
						mu.Unlock()
						put(authReply)
					case strings.HasPrefix(u, "MAIL"):
						put(mailReply)
					case strings.HasPrefix(u, "QUIT"):
						put("221 bye")
						return
					default:
						put("250 OK")
					}
				}
			}(c)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(p)
	return h, port, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// 535 5.7.139 is the organisation's policy, not a bad token: it is
// ErrSMTPAuthDisabled (and permanent), the token is NOT handed back, and
// there is no second AUTH with a fresh one.
//
// PROBE: remove the isSMTPAuthDisabled branch in authenticate and this goes
// red (the token is rejected, two AUTHs, and no ErrSMTPAuthDisabled).
func TestSMTPAuthDisabledIsReceiveOnlyNotReauth(t *testing.T) {
	host, port, auths := scriptedReplies(t,
		"535 5.7.139 Authentication unsuccessful, SmtpClientAuthentication is disabled for the Tenant. Visit https://aka.example/smtp_auth_disabled for more information.",
		"250 OK")
	src := &seqSource{tokens: []xoauth2.Secret{smtpTestToken, "ya29.second"}}
	_, err := loopbackSender.Send(context.Background(), SendRequest{
		Host: host, Port: port, Auth: XOAuth2Auth("you@example.com", src),
		MailFrom: "you@example.com", RcptTo: []string{"them@example.net"}, Data: []byte("x"),
		Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if !errors.Is(err, ErrSMTPAuthDisabled) || !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want ErrSMTPAuthDisabled and ErrPermanent", err)
	}
	if _, rejected := src.snapshot(); len(rejected) != 0 {
		t.Errorf("the token was handed back (%d Rejects): a policy refusal is not a bad token", len(rejected))
	}
	if n := auths(); n != 1 {
		t.Errorf("AUTH sent %d times, want 1", n)
	}
	if strings.Contains(err.Error(), smtpTestToken.Reveal()) {
		t.Error("the token is in the error")
	}
}

// An ordinary 535 is still the token-refresh retry, not the policy case.
func TestOrdinary535IsNotSMTPAuthDisabled(t *testing.T) {
	host, port, _ := scriptedReplies(t, "535 5.7.3 Authentication unsuccessful", "250 OK")
	src := &seqSource{tokens: []xoauth2.Secret{smtpTestToken}}
	err := loopbackSender.CheckAuth(context.Background(), SendRequest{
		Host: host, Port: port, Auth: XOAuth2Auth("you@example.com", src),
		Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if errors.Is(err, ErrSMTPAuthDisabled) {
		t.Fatalf("a plain 535 5.7.3 was read as SMTP AUTH disabled: %v", err)
	}
}

// Exchange's Send As refusal surfaces as its own permanent cause.
func TestSendAsDeniedIsPermanentAndNamed(t *testing.T) {
	host, port, _ := scriptedReplies(t, "235 2.7.0 Accepted",
		"550 5.2.252 SendAsDenied; you@example.com not allowed to send as other@example.com")
	src := &seqSource{tokens: []xoauth2.Secret{smtpTestToken}}
	_, err := loopbackSender.Send(context.Background(), SendRequest{
		Host: host, Port: port, Auth: XOAuth2Auth("you@example.com", src),
		MailFrom: "other@example.com", RcptTo: []string{"them@example.net"}, Data: []byte("x"),
		Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if !errors.Is(err, ErrSendAsDenied) || !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want ErrSendAsDenied and ErrPermanent", err)
	}
}

func TestMicrosoftProfileTables(t *testing.T) {
	for _, c := range []struct {
		name, want string
	}{
		{"Sent Items", `\Sent`}, {"sent items", `\Sent`}, {"Deleted Items", `\Trash`},
		{"Junk Email", `\Junk`}, {"Junk E-mail", `\Junk`}, {"Drafts", `\Drafts`},
		{"Archive", `\Archive`}, {"Inbox/Sent Items", ""}, {"Work", ""},
	} {
		if got := FolderSpecialUse(ProfileMicrosoft, c.name, '/'); got != c.want {
			t.Errorf("FolderSpecialUse(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	if FolderSpecialUse("generic", "Sent Items", '/') != "" {
		t.Error("generic profile maps a folder name")
	}
	if !NonMailFolder(ProfileMicrosoft, "Calendar/Birthdays", '/') || NonMailFolder(ProfileMicrosoft, "Inbox", '/') {
		t.Error("NonMailFolder misclassifies")
	}
	if OutboundDailyQuota(ProfileMicrosoft) != 100 || OutboundDailyQuota("generic") != 0 {
		t.Error("outbound quota table")
	}
	if !strings.Contains(IMAPAccessHint(ProfileMicrosoft), "Forwarding and IMAP") {
		t.Error("the Microsoft hint does not name the Outlook.com setting")
	}
}
