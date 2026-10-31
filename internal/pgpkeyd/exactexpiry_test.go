// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// An exact expiry (#813, ADR-040 §12), through the real daemon: the packets of
// the primary key AND the encryption subkey say the chosen instant to the
// second, for generate and for extend, and the out-of-range requests are
// refused before anything is made.

import (
	"context"
	"testing"
	"time"

	"mailroom/internal/pgpwire"
)

func timep(t time.Time) *time.Time { return &t }

func TestGenerateWithAnExactExpiry(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()
	// An instant with an odd number of seconds and a fraction: the packets have
	// second resolution, so the fraction is dropped and the seconds are kept.
	at := time.Now().UTC().Add(14*lcDay + 3*time.Hour + 17*time.Minute + 5*time.Second + 600*time.Millisecond)
	want := at.Truncate(time.Second)

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "exact@example.com", ExpiresAt: &at})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	v := assessStored(t, h, gen.Key.KeyID, time.Now())
	if v.PrimaryExpires == nil || !v.PrimaryExpires.Equal(want) {
		t.Errorf("primary key expires %v, want exactly %v", v.PrimaryExpires, want)
	}
	if v.EncryptionExpires == nil || !v.EncryptionExpires.Equal(want) {
		t.Errorf("encryption subkey expires %v, want exactly %v", v.EncryptionExpires, want)
	}
	if gen.Key.UsableUntil == nil || !gen.Key.UsableUntil.Equal(want) {
		t.Errorf("usable_until = %v, want %v", gen.Key.UsableUntil, want)
	}
}

func TestGenerateRefusesBadExactExpiries(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()
	now := time.Now().UTC()
	cases := map[string]pgpwire.GenerateRequest{
		"in the past":       {ExpiresAt: timep(now.Add(-lcDay))},
		"under a day":       {ExpiresAt: timep(now.Add(23 * time.Hour))},
		"over ten years":    {ExpiresAt: timep(now.Add(time.Duration(pgpwire.MaxKeyLifetimeDays+1) * lcDay))},
		"days and an exact": {ExpiresAt: timep(now.Add(30 * lcDay)), ExpiresDays: intp(30)},
	}
	for name, req := range cases {
		req.UserID, req.Email = user, "bad@example.com"
		_, err := h.client.Generate(ctx, req)
		if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
			t.Errorf("%s: err = %v, want invalid_request", name, err)
		}
	}
	// Control: the shortest allowed is accepted.
	if _, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "short@example.com",
		ExpiresAt: timep(now.Add(pgpwire.MinKeyLifetime + time.Minute))}); err != nil {
		t.Errorf("a key valid for a day and a minute was refused: %v", err)
	}
}

func TestExtendToAnExactExpiry(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()
	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "ext@example.com", ExpiresDays: intp(3)})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(400*lcDay + 41*time.Second)
	want := at.Truncate(time.Second)
	if _, err := h.client.Extend(ctx, pgpwire.ExtendRequest{UserID: user, KeyID: gen.Key.KeyID, ExpiresAt: &at}); err != nil {
		t.Fatalf("Extend: %v", err)
	}
	v := assessStored(t, h, gen.Key.KeyID, time.Now())
	if v.PrimaryExpires == nil || !v.PrimaryExpires.Equal(want) || v.EncryptionExpires == nil || !v.EncryptionExpires.Equal(want) {
		t.Errorf("extended key expires primary %v / subkey %v, want both exactly %v", v.PrimaryExpires, v.EncryptionExpires, want)
	}
	for name, req := range map[string]pgpwire.ExtendRequest{
		"both":        {Days: 10, ExpiresAt: &at},
		"neither":     {},
		"in the past": {ExpiresAt: timep(time.Now().Add(-time.Hour))},
	} {
		req.UserID, req.KeyID = user, gen.Key.KeyID
		_, err := h.client.Extend(ctx, req)
		if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
			t.Errorf("extend %s: err = %v, want invalid_request", name, err)
		}
	}
}
