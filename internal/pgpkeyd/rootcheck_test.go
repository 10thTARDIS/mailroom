// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"mailroom/internal/crypto"
	"mailroom/internal/store"
)

// fakeRootCheckStore is app.pgp_root_check for the unit suite: at most one row,
// and the real table's never-replace contract. The real table enforces that
// with a grant, which a double cannot have — the integration suite proves the
// grant (internal/store/pgp_root_check_integration_test.go); this file proves
// the verdict drawn from what the store hands back.
type fakeRootCheckStore struct {
	row *store.PGPRootCheck
}

func (f *fakeRootCheckStore) EnsureRootCheck(_ context.Context, c store.PGPRootCheck) (store.PGPRootCheck, bool, error) {
	if f.row != nil {
		return *f.row, false, nil
	}
	rec := store.PGPRootCheck{
		Info:       c.Info,
		Value:      append([]byte(nil), c.Value...),
		RecordedAt: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC),
	}
	f.row = &rec
	return rec, true, nil
}

// checkRoot is a distinct, deterministic root per seed.
func checkRoot(seed byte) []byte {
	root := make([]byte, crypto.KeySize)
	for i := range root {
		root[i] = seed ^ byte(i*29+1)
	}
	return root
}

func checkService(t *testing.T, root []byte, log *slog.Logger) *Service {
	t.Helper()
	svc, err := NewService(root, NewMemoryVault(), log)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// TestVerifyRootRecordsOnceThenVerifies is the fresh-install case and its
// positive control: the first boot records, the second verifies — and what is
// recorded is THIS root's value under THIS scheme, or the refusal below would
// be comparing against nothing in particular.
func TestVerifyRootRecordsOnceThenVerifies(t *testing.T) {
	ctx := context.Background()
	root := checkRoot(0xA5)
	st := &fakeRootCheckStore{}

	got, err := checkService(t, root, discardLogger()).VerifyRoot(ctx, st)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if got != RootRecorded {
		t.Fatalf("first boot outcome = %q, want %q", got, RootRecorded)
	}
	want, err := RootCheckValue(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.row == nil || !bytes.Equal(st.row.Value, want) || st.row.Info != rootCheckInfo {
		t.Fatalf("recorded %+v, want scheme %q and this root's check value", st.row, rootCheckInfo)
	}

	got, err = checkService(t, root, discardLogger()).VerifyRoot(ctx, st)
	if err != nil {
		t.Fatalf("second boot, same root: %v", err)
	}
	if got != RootVerified {
		t.Fatalf("second boot outcome = %q, want %q", got, RootVerified)
	}
}

// TestVerifyRootRefusesADifferentRoot is the case #525 exists for.
func TestVerifyRootRefusesADifferentRoot(t *testing.T) {
	ctx := context.Background()
	st := &fakeRootCheckStore{}
	if _, err := checkService(t, checkRoot(0xA5), discardLogger()).VerifyRoot(ctx, st); err != nil {
		t.Fatalf("recording boot: %v", err)
	}
	recorded := append([]byte(nil), st.row.Value...)

	got, err := checkService(t, checkRoot(0x5A), discardLogger()).VerifyRoot(ctx, st)
	if !errors.Is(err, ErrRootMismatch) {
		t.Fatalf("a different root: err = %v, want ErrRootMismatch", err)
	}
	if got != "" {
		t.Errorf("a refused boot reported outcome %q; a caller must have nothing to mistake for success", got)
	}
	if !bytes.Equal(st.row.Value, recorded) {
		t.Error("the refused root's boot changed the recorded value")
	}
}

// TestVerifyRootRefusesAnUnknownSchemeEvenWhenTheBytesMatch: a check recorded
// under another scheme cannot vouch for this root, so it is refused — but NOT
// as a mismatch, because the remedy for the two is different and one of them is
// "replace the root", which would be wrong here.
//
// ⚠ THE STORED BYTES ARE THIS ROOT'S OWN VALUE, deliberately. With different
// bytes the value comparison alone would refuse, and the test would pass with
// the scheme check deleted. With matching bytes only the scheme check can.
func TestVerifyRootRefusesAnUnknownSchemeEvenWhenTheBytesMatch(t *testing.T) {
	root := checkRoot(0xA5)
	value, err := RootCheckValue(root)
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeRootCheckStore{row: &store.PGPRootCheck{Info: "mailroom-pgp-root-check-v0", Value: value}}

	got, err := checkService(t, root, discardLogger()).VerifyRoot(context.Background(), st)
	if err == nil {
		t.Fatalf("a check recorded under an unknown scheme was accepted (outcome %q)", got)
	}
	if errors.Is(err, ErrRootMismatch) {
		t.Errorf("an unknown scheme was reported as a root MISMATCH (%v) — that sends the operator "+
			"to replace a root that may be the right one", err)
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("error %q does not say the scheme is the problem", err)
	}
}

// TestRootCheckValueIsItsOwnDerivation: deterministic, specific to the root,
// the length the table's CHECK demands, and never a wrapping key. The value is
// stored in the clear, so the last property is the one that matters.
//
// The empty-salt wrapping key is the sharpest comparison available: HKDF treats
// an empty salt and no salt identically, so against it the two derivations
// differ ONLY in their info label — which is precisely the separation claimed.
func TestRootCheckValueIsItsOwnDerivation(t *testing.T) {
	a, b := checkRoot(0xA5), checkRoot(0x5A)
	va, err := RootCheckValue(a)
	if err != nil {
		t.Fatal(err)
	}
	again, err := RootCheckValue(a)
	if err != nil {
		t.Fatal(err)
	}
	vb, err := RootCheckValue(b)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(va, again) {
		t.Error("the same root derived two different check values — no boot could ever verify")
	}
	if bytes.Equal(va, vb) {
		t.Error("two different roots derived the same check value — no mismatch could ever be seen")
	}
	if len(va) != 32 {
		t.Errorf("check value is %d bytes; app.pgp_root_check's CHECK admits exactly 32", len(va))
	}
	for _, salt := range []string{"", "01HZXK5V8Q3M2N1P0R9S8T7W6Y"} {
		wk, err := WrappingKey(a, salt)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(wk, va) {
			t.Errorf("the stored check value equals the wrapping key for salt %q", salt)
		}
	}
}

// TestVerifyRootLogsNoKeyMaterial: internal/pgpkeyd has a clean record on
// sensitive logging, and #525 asks that it stay that way. All three outcomes
// are logged here through a real JSON handler, which renders a []byte field as
// base64, so hex and base64 are both searched for — in the log AND in the
// returned error, which cmd/mailroom-crypto prints.
//
// ⚠ The outcome markers are required as well, or this passes against a logger
// that wrote nothing.
func TestVerifyRootLogsNoKeyMaterial(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	ctx := context.Background()
	rootA, rootB := checkRoot(0xA5), checkRoot(0x5A)
	st := &fakeRootCheckStore{}

	if _, err := checkService(t, rootA, log).VerifyRoot(ctx, st); err != nil {
		t.Fatalf("recording boot: %v", err)
	}
	if _, err := checkService(t, rootA, log).VerifyRoot(ctx, st); err != nil {
		t.Fatalf("verifying boot: %v", err)
	}
	_, mismatch := checkService(t, rootB, log).VerifyRoot(ctx, st)
	if !errors.Is(mismatch, ErrRootMismatch) {
		t.Fatalf("mismatching boot: err = %v, want ErrRootMismatch", mismatch)
	}

	out := buf.String()
	for _, want := range []string{`"outcome":"recorded"`, `"outcome":"verified"`, `"outcome":"mismatch"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s — the search below would pass against a silent logger", want)
		}
	}

	out += "\n" + mismatch.Error()
	va, err := RootCheckValue(rootA)
	if err != nil {
		t.Fatal(err)
	}
	vb, err := RootCheckValue(rootB)
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string][]byte{
		"root A": rootA, "root B": rootB, "check value A": va, "check value B": vb,
	} {
		for enc, s := range map[string]string{
			"hex":    hex.EncodeToString(secret),
			"HEX":    strings.ToUpper(hex.EncodeToString(secret)),
			"base64": base64.StdEncoding.EncodeToString(secret),
		} {
			if strings.Contains(out, s) {
				t.Errorf("%s appears in the log or error (%s)", name, enc)
			}
		}
	}
}
