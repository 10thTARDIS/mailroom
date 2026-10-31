// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"context"
	"strings"
	"testing"
	"time"
)

// CheckAuth (#655) is the AUTH-only credential check set-account-credentials
// makes before sealing an SMTP login. It rides on the same open() as Send, so it
// inherits the cleartext refusal — asserted here the strong way, as "the server
// never saw AUTH", not "an error came back".
func TestCheckAuthNeverAuthenticatesOverCleartext(t *testing.T) {
	srv, host, port := startRecordingSMTP(t, []string{"SIZE 35882577"}) // no STARTTLS

	err := loopbackSender.CheckAuth(context.Background(), SendRequest{
		Host: host, Port: port, Username: "u@example.com", Password: "hunter2-secret", Timeout: 5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Fatalf("CheckAuth over cleartext = %v, want a cleartext refusal", err)
	}
	if srv.sawCommand("AUTH") {
		t.Errorf("the server received AUTH over a cleartext connection: %q", srv.seen())
	}
}

// With the test-only cleartext allowance the check authenticates, then quits,
// and sends NO message: MAIL/RCPT/DATA are never issued.
func TestCheckAuthAuthenticatesAndSendsNothing(t *testing.T) {
	srv, host, port := startRecordingSMTP(t, []string{"AUTH PLAIN"})

	err := loopbackSender.CheckAuth(context.Background(), SendRequest{
		Host: host, Port: port, Username: "u@example.com", Password: "pw",
		Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if err != nil {
		t.Fatalf("CheckAuth: %v", err)
	}
	if !srv.sawCommand("AUTH") {
		t.Errorf("CheckAuth never authenticated: %q", srv.seen())
	}
	for _, verb := range []string{"MAIL", "RCPT", "DATA"} {
		if srv.sawCommand(verb) {
			t.Errorf("CheckAuth issued %s; it must send nothing: %q", verb, srv.seen())
		}
	}
}

// The zero-value SMTPSender is the guarded one: loopback is refused before any
// byte is sent.
func TestCheckAuthIsEgressGuarded(t *testing.T) {
	srv, host, port := startRecordingSMTP(t, []string{"AUTH PLAIN"})
	err := SMTPSender{}.CheckAuth(context.Background(), SendRequest{
		Host: host, Port: port, Username: "u", Password: "p", Timeout: 5 * time.Second, AllowInsecureNoTLS: true,
	})
	if err == nil || !strings.Contains(err.Error(), "not a permitted") {
		t.Fatalf("CheckAuth to loopback = %v, want an egress refusal", err)
	}
	if len(srv.seen()) != 0 {
		t.Errorf("the guarded sender still talked to loopback: %q", srv.seen())
	}
}
