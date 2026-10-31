// SPDX-License-Identifier: AGPL-3.0-only

package onboarding

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"mailroom/internal/egress"
	"mailroom/internal/sync"
)

// The account-validation dial must not be usable to probe the host's network
// (#527). Until v1.70.x `ValidateAccount` dialled whatever host:port it was
// given and returned the raw dial error, so an authenticated caller could tell
// an open port from a closed one by the shape of the failure — an SSRF
// primitive and a port-scan oracle over everything the container can reach.
//
// ⚠ THE LOAD-BEARING ASSERTION IS THAT THE DIALER IS NEVER CALLED, not that an
// error came back. A guard that refused *after* connecting would still have
// leaked the answer — the connection itself is the observable — so a test that
// only checked the error would pass against the vulnerable arrangement. Hence
// the recording dialer below: for a refused host its `called` must stay false.

// recordingDialer reports whether the IMAP dial was ever attempted. It fails
// the dial deliberately: reaching it at all is what these cases measure, and a
// real session would only add surface.
type recordingDialer struct{ called bool }

func (d *recordingDialer) dial(context.Context, sync.Config) (sync.SyncSession, error) {
	d.called = true
	return nil, errors.New("dialer reached (this is the observation, not a failure)")
}

// fixedResolver answers every lookup with one address, so a case can name the
// address it is about without depending on DNS.
type fixedResolver struct{ addrs []netip.Addr }

func (r fixedResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r.addrs, nil
}

func addrs(t *testing.T, ss ...string) fixedResolver {
	t.Helper()
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("bad fixture address %q: %v", s, err)
		}
		out = append(out, a)
	}
	return fixedResolver{addrs: out}
}

func guardSpec() AccountSpec {
	return AccountSpec{
		Name: "probe", IMAPHost: "mail.example.com", SMTPHost: "smtp.example.com",
		Username: "u", Password: "p", TLS: sync.TLSImplicit,
	}
}

func TestAccountValidationRefusesNonPublicHosts(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		why  string
	}{
		{"loopback", "127.0.0.1", "the container's own services"},
		{"RFC1918 /8", "10.0.0.5", "the rest of the fleet"},
		{"RFC1918 /12", "172.16.4.9", "the rest of the fleet"},
		{"RFC1918 /16", "192.168.1.20", "the LAN"},
		// The one that matters most on a cloud VM: the instance metadata
		// endpoint hands out credentials to anything that can reach it.
		{"link-local metadata", "169.254.169.254", "cloud instance metadata"},
		{"IPv6 loopback", "::1", "the container's own services"},
		{"IPv6 unique-local", "fc00::1", "the private network"},
		// Loopback wearing an IPv6 costume. egress.CheckAddress unmaps before
		// comparing anything, which is the line that catches this.
		{"IPv4-mapped loopback", "::ffff:127.0.0.1", "loopback in disguise"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &recordingDialer{}
			_, err := ValidateAccount(context.Background(), d.dial, guardSpec(),
				WithResolver(addrs(t, tc.addr)))

			if err == nil {
				t.Fatalf("%s (%s) was ACCEPTED — Account/set can reach it", tc.addr, tc.why)
			}
			// THE assertion. An error alone does not prove the guard held.
			if d.called {
				t.Errorf("the dialer was called for %s: the connection is the observable, so "+
					"refusing after dialling still answers 'is this port open?'", tc.addr)
			}
			// Distinguishable from a genuine connection failure (#527 requires
			// this explicitly): the caller must not be able to read the network's
			// shape out of the refusal.
			if !errors.Is(err, ErrHostNotPermitted) {
				t.Errorf("refusal for %s is not ErrHostNotPermitted (%v) — indistinguishable "+
					"from 'connection refused' puts the oracle back", tc.addr, err)
			}
			if !errors.Is(err, egress.ErrBlockedAddress) {
				t.Errorf("refusal for %s does not wrap egress.ErrBlockedAddress (%v) — the "+
					"policy should be traceable to the one package that owns it", tc.addr, err)
			}
			// The way out must be discoverable from the error itself.
			if !strings.Contains(err.Error(), "MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS") {
				t.Errorf("refusal for %s does not name the escape hatch: %v", tc.addr, err)
			}
		})
	}
}

// ⚠ THE POSITIVE CONTROL. #527 asks for it by name, and without it every case
// above is satisfied by a guard that refuses everything — which would be a
// perfectly green test suite and a product that cannot add an account.
func TestAccountValidationStillDialsAPublicHost(t *testing.T) {
	d := &recordingDialer{}
	_, err := ValidateAccount(context.Background(), d.dial, guardSpec(),
		WithResolver(addrs(t, "93.184.216.34")))

	if !d.called {
		t.Fatal("a public host was NOT dialled — the guard refuses everything, so the " +
			"refusal cases above prove nothing")
	}
	// And the error that comes back is the DIAL's, not the policy's — the other
	// half of "distinguishable": a real connection failure must not masquerade
	// as a policy refusal either.
	if errors.Is(err, ErrHostNotPermitted) {
		t.Errorf("a dial failure was reported as a policy refusal: %v", err)
	}
}

// A host that publishes a public address ALONGSIDE a private one is refused
// outright. Otherwise whether the guard holds would depend on resolver
// ordering — which the other side chooses.
func TestAccountValidationRefusesMixedResolution(t *testing.T) {
	d := &recordingDialer{}
	_, err := ValidateAccount(context.Background(), d.dial, guardSpec(),
		WithResolver(addrs(t, "93.184.216.34", "127.0.0.1")))

	if d.called {
		t.Error("dialled a host that also resolves to loopback")
	}
	if !errors.Is(err, ErrHostNotPermitted) {
		t.Errorf("mixed resolution was not refused as policy: %v", err)
	}
}

// The escape hatch works, and is off unless asked for. A LAN mail server is a
// legitimate account target for some deployments (#527's explicit decision);
// what must never happen is it being reachable by default.
func TestAllowPrivateHostsIsOptInAndWorks(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		d := &recordingDialer{}
		if _, err := ValidateAccount(context.Background(), d.dial, guardSpec(),
			WithResolver(addrs(t, "192.168.1.20"))); !errors.Is(err, ErrHostNotPermitted) {
			t.Errorf("private host was not refused by default: %v", err)
		}
		if d.called {
			t.Error("private host was dialled by default")
		}
	})

	t.Run("permitted when explicitly allowed", func(t *testing.T) {
		d := &recordingDialer{}
		_, err := ValidateAccount(context.Background(), d.dial, guardSpec(),
			WithResolver(addrs(t, "192.168.1.20")), WithAllowPrivateHosts(true))
		if !d.called {
			t.Fatal("WithAllowPrivateHosts(true) did not permit a LAN host — the documented " +
				"escape hatch does not work, so the deployment it exists for is broken")
		}
		if errors.Is(err, ErrHostNotPermitted) {
			t.Errorf("still refused as policy despite the allowance: %v", err)
		}
	})
}

// perHostResolver answers LookupNetIP per the queried hostname, unlike
// fixedResolver (which ignores it and answers every lookup the same way). The
// SMTP-host cases below need that: they give the IMAP and SMTP hosts DIFFERENT
// addresses to prove the SMTP check is a real, independent check rather than
// happening to piggy-back on the IMAP one being refused first.
type perHostResolver map[string][]netip.Addr

func (r perHostResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	return r[host], nil
}

// TestAccountValidationRefusesPrivateSMTPHost is #625 §3's cheap half: onboarding
// used to run the egress guard on spec.IMAPHost only, so a public IMAP host
// paired with an SMTP host on a private network sailed through validation even
// though the submission path dials that SMTP host later — the same oracle one
// hop downstream.
func TestAccountValidationRefusesPrivateSMTPHost(t *testing.T) {
	spec := guardSpec() // IMAPHost=mail.example.com, SMTPHost=smtp.example.com
	resolver := perHostResolver{
		"mail.example.com": {netip.MustParseAddr("93.184.216.34")}, // public: IMAP passes
		"smtp.example.com": {netip.MustParseAddr("192.168.1.20")},  // private: SMTP must not
	}
	d := &recordingDialer{}
	_, err := ValidateAccount(context.Background(), d.dial, spec, WithResolver(resolver))

	if d.called {
		t.Error("dialled IMAP despite the SMTP host resolving to a private address — " +
			"the check must run, and refuse, before any dial")
	}
	if !errors.Is(err, ErrHostNotPermitted) {
		t.Errorf("private SMTP host was not refused as policy: %v", err)
	}
	if !strings.Contains(err.Error(), "smtp") {
		t.Errorf("refusal does not name the SMTP host as the one refused: %v", err)
	}
	if !strings.Contains(err.Error(), "MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS") {
		t.Errorf("refusal for a private SMTP host does not name the escape hatch: %v", err)
	}
}

// The escape hatch covers the SMTP host too — one switch, not two half-fixed
// paths.
func TestAccountValidationAllowsPrivateSMTPHostWhenPermitted(t *testing.T) {
	spec := guardSpec()
	resolver := perHostResolver{
		"mail.example.com": {netip.MustParseAddr("93.184.216.34")},
		"smtp.example.com": {netip.MustParseAddr("192.168.1.20")},
	}
	d := &recordingDialer{}
	_, err := ValidateAccount(context.Background(), d.dial, spec,
		WithResolver(resolver), WithAllowPrivateHosts(true))

	if !d.called {
		t.Fatal("WithAllowPrivateHosts(true) did not permit a private SMTP host through to the IMAP dial")
	}
	if errors.Is(err, ErrHostNotPermitted) {
		t.Errorf("still refused as policy despite the allowance: %v", err)
	}
}

// An IP literal in the spec must be checked too — it never reaches the
// resolver, so a guard that only inspected DNS answers would wave it through.
func TestAccountValidationChecksIPLiteralHosts(t *testing.T) {
	spec := guardSpec()
	spec.IMAPHost = "127.0.0.1"
	d := &recordingDialer{}
	_, err := ValidateAccount(context.Background(), d.dial, spec,
		WithResolver(addrs(t, "93.184.216.34"))) // resolver would say "public"

	if d.called {
		t.Error("dialled a loopback IP LITERAL — the literal path skips the resolver")
	}
	if !errors.Is(err, ErrHostNotPermitted) {
		t.Errorf("IP literal was not refused as policy: %v", err)
	}
}
