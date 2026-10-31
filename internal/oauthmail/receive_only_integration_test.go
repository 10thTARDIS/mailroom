// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package oauthmail

import (
	"fmt"
	"net/url"
	"testing"

	"mailroom/internal/provider"
)

// THE CONNECT-TIME SMTP PROBE (ADR-046 §12). A Microsoft account whose
// organisation turned SMTP AUTH off (5.7.139) still CONNECTS — it is marked
// receive-only, not refused — and a reconnect whose probe succeeds clears the
// mark (the person's way to recheck after an administrator changed it). A
// Google account is never probed. A Microsoft account gets the profile's
// stated daily send cap.
//
// PROBE: remove the checkSending call in complete() and the first assertion
// goes red.
func TestConnectMicrosoftReceiveOnlyAndRecheck(t *testing.T) {
	h := newConnectHarness(t)
	a := h.user()
	h.smtpErr = fmt.Errorf("%w: %w: 535 5.7.139", provider.ErrPermanent, provider.ErrSMTPAuthDisabled)
	bob := fakeWho{sub: "ms-sub-bob", email: "bob@example.com"}
	res := h.connect(a, ProviderMicrosoft, bob)
	if res.Get("oauth") != ResultConnected {
		t.Fatalf("result = %v: a 5.7.139 tenant must still connect", res)
	}
	acct := mustAccount(t, res)
	blocked := func() string {
		var code *string
		_ = h.super.QueryRow(h.ctx, `SELECT send_blocked_code FROM app.account_sync_status WHERE account_id = $1`,
			acct.String()).Scan(&code)
		if code == nil {
			return ""
		}
		return *code
	}
	if got := blocked(); got != "smtp_auth_disabled" {
		t.Fatalf("send_blocked_code = %q after a 5.7.139 probe, want smtp_auth_disabled", got)
	}
	var quota int
	if err := h.super.QueryRow(h.ctx, `SELECT outbound_daily_quota FROM app.accounts WHERE id = $1`, acct.String()).Scan(&quota); err != nil {
		t.Fatal(err)
	}
	if quota != provider.OutboundDailyQuota(provider.ProfileMicrosoft) {
		t.Errorf("outbound_daily_quota = %d, want the Microsoft profile's %d", quota, provider.OutboundDailyQuota(provider.ProfileMicrosoft))
	}

	// A transient probe failure changes nothing.
	h.smtpErr = fmt.Errorf("dial: connection reset")
	code, state := h.idp.authorize(t, ProviderMicrosoft, h.start(a, ProviderMicrosoft, &acct), bob)
	if r := h.callback(a, ProviderMicrosoft, url.Values{"code": {code}, "state": {state}}); r.Get("oauth") != ResultReconnected {
		t.Fatalf("reconnect = %v", r)
	}
	if got := blocked(); got != "smtp_auth_disabled" {
		t.Errorf("a network failure unblocked sending: %q", got)
	}

	// The administrator turned SMTP AUTH on; reconnect rechecks and clears.
	h.smtpErr = nil
	code, state = h.idp.authorize(t, ProviderMicrosoft, h.start(a, ProviderMicrosoft, &acct), bob)
	if r := h.callback(a, ProviderMicrosoft, url.Values{"code": {code}, "state": {state}}); r.Get("oauth") != ResultReconnected {
		t.Fatalf("reconnect = %v", r)
	}
	if got := blocked(); got != "" {
		t.Errorf("send_blocked_code = %q after a successful probe, want cleared", got)
	}

	// Google: no probe.
	before := h.smtpChecks
	h.connect(h.user(), ProviderGoogle, alice)
	if h.smtpChecks != before {
		t.Errorf("a Google connect ran the SMTP probe")
	}
}
