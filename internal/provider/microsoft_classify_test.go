// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"syscall"
	"testing"
)

// REVIEW M-H1 of #910: a transport error whose text happens to contain the
// digit runs (the peer's address 52.5.7.139 or 40.5.2.252) must never read as
// a policy refusal. Only the server's own reply counts.
//
// PROBE: put the strings.Contains(err.Error(), …) matchers back and the
// network cases go red.
func TestPolicyRefusalsMatchOnlyServerReplies(t *testing.T) {
	reset := &net.OpError{Op: "write", Net: "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP("52.5.7.139"), Port: 587}, Err: syscall.ECONNRESET}
	timeout := &net.OpError{Op: "read", Net: "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP("40.5.2.252"), Port: 587}, Err: errors.New("i/o timeout")}
	for _, c := range []struct {
		name               string
		err                error
		authOff, sendAsNot bool
	}{
		{"reset to 52.5.7.139", fmt.Errorf("provider: smtp auth: %w", reset), false, false},
		{"timeout to 40.5.2.252", fmt.Errorf("provider: mail from: %w", timeout), false, false},
		{"text naming the code, not a reply", errors.New("5.7.139 SmtpClientAuthentication is disabled; SendAsDenied 5.2.252"), false, false},
		{"535 5.7.139", &textproto.Error{Code: 535, Msg: "5.7.139 Authentication unsuccessful, SmtpClientAuthentication is disabled for the Tenant."}, true, false},
		{"535 with the code repeated", &textproto.Error{Code: 535, Msg: "535 5.7.139 Authentication unsuccessful"}, true, false},
		{"535 5.7.3 (stale token)", &textproto.Error{Code: 535, Msg: "5.7.3 Authentication unsuccessful"}, false, false},
		{"535 5.7.1390 is not 5.7.139", &textproto.Error{Code: 535, Msg: "5.7.1390 x"}, false, false},
		{"454 5.7.139 is not 535", &textproto.Error{Code: 454, Msg: "5.7.139 x"}, false, false},
		{"550 5.2.252", &textproto.Error{Code: 550, Msg: "5.2.252 SendAsDenied; not allowed"}, false, true},
		{"451 5.2.252 is not 5xx", &textproto.Error{Code: 451, Msg: "5.2.252 x"}, false, false},
	} {
		if got := isSMTPAuthDisabled(c.err); got != c.authOff {
			t.Errorf("%s: isSMTPAuthDisabled = %v, want %v", c.name, got, c.authOff)
		}
		if got := isSendAsDenied(c.err); got != c.sendAsNot {
			t.Errorf("%s: isSendAsDenied = %v, want %v", c.name, got, c.sendAsNot)
		}
	}
	// classifySMTP: the network timeout stays retryable (not permanent).
	if err := classifySMTP(fmt.Errorf("provider: mail from: %w", timeout), timeout); errors.Is(err, ErrPermanent) || errors.Is(err, ErrSendAsDenied) {
		t.Errorf("a network timeout was classified permanent: %v", err)
	}
}
