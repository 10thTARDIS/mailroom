// SPDX-License-Identifier: AGPL-3.0-only

package onboarding

import (
	"context"
	"errors"
	"strings"
	"testing"

	"mailroom/internal/egress"
	"mailroom/internal/egress/egresstest"
	"mailroom/internal/sync"
)

// #625 §3's second half, at validation: the pre-dial check (egress.CheckHost)
// and the dial are two resolutions, so a host can pass the first and rebind
// before the second. The dial is now guarded too, so validation's own IMAP
// connection is refused — through the PRODUCTION sync.Dial (no dialer
// override), and observed on a real listener.
func TestValidationRefusesARebindBetweenCheckAndDial(t *testing.T) {
	ln := egresstest.Listen(t, nil)
	// Two public answers — the IMAP check, then the SMTP check — then loopback
	// for the dial. "localhost" so a guard-less dial (which resolves through the
	// system) would reach the listener: see internal/sync/dial_guard_test.go.
	r := egresstest.NewResolver(t, []string{"93.184.216.34"}, []string{"93.184.216.34"}, []string{"127.0.0.1"})
	spec := AccountSpec{
		Name: "rebind", IMAPHost: "localhost", IMAPPort: ln.Port(), SMTPHost: "smtp.example.com",
		Username: "u", Password: "hunter2-the-real-mailbox-password", TLS: sync.TLSInsecure,
	}

	_, err := ValidateAccount(context.Background(), nil, spec, WithResolver(r))

	if n := ln.Connections(t); n != 0 {
		t.Errorf("validation's dial reached the rebound loopback address (%d connection(s))", n)
	}
	if !errors.Is(err, ErrHostNotPermitted) || !errors.Is(err, egress.ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrHostNotPermitted wrapping egress.ErrBlockedAddress — the same "+
			"policy refusal the pre-dial check gives", err)
	}
	if !strings.Contains(err.Error(), "MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS") {
		t.Errorf("refusal does not name the escape hatch: %v", err)
	}
	if strings.Contains(err.Error(), spec.Password) {
		t.Errorf("refusal leaks the password: %v", err)
	}
	// Same latch behaviour as the pre-dial refusal of the same condition.
	if latchesSync(err) {
		t.Errorf("a dial-time policy refusal latches sync off, but the pre-dial refusal does not: %v", err)
	}
	if n := r.Calls(); n != 3 {
		t.Errorf("resolver answered %d lookups, want 3 (imap check, smtp check, the dial)", n)
	}
}

// The validation dial carries the SAME switch and resolver the pre-dial check
// used, so validation and every later dial apply one rule.
func TestValidationDialCarriesThePolicy(t *testing.T) {
	for _, allow := range []bool{false, true} {
		var got sync.Config
		dial := func(_ context.Context, cfg sync.Config) (sync.SyncSession, error) {
			got = cfg
			return nil, errors.New("recorded")
		}
		r := egresstest.NewResolver(t, []string{"93.184.216.34"})
		_, _ = ValidateAccount(context.Background(), dial, guardSpec(),
			WithResolver(r), WithAllowPrivateHosts(allow))
		if got.AllowPrivateHosts != allow {
			t.Errorf("allow=%v: the validation dial's Config.AllowPrivateHosts = %v", allow, got.AllowPrivateHosts)
		}
		if got.Resolver != egress.Resolver(r) {
			t.Errorf("allow=%v: the validation dial did not get the injected resolver", allow)
		}
	}
}
