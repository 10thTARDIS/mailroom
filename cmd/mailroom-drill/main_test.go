// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/pgpkeyd"
	"mailroom/internal/store"
)

func clearDrillEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		config.EnvDatabaseURL, config.EnvDatabaseURL + "_FILE",
		config.EnvDatabasePasswordFile,
		config.EnvMasterKeyFile, config.EnvBlobRoot, config.EnvSearchRoot,
		pgpkeyd.EnvRootKeyFile,
	} {
		t.Setenv(k, "")
	}
}

// TestLoadDatabasePasswordFile pins the #537 path: a plain database URL with
// no password, plus MAILROOM_DATABASE_PASSWORD_FILE, ends up with the file's
// password injected into settings.databaseURL.
func TestLoadDatabasePasswordFile(t *testing.T) {
	clearDrillEnv(t)
	t.Setenv(config.EnvDatabaseURL, "postgres://mailroom_app@127.0.0.1:5432/mailroom?sslmode=disable")

	pwPath := filepath.Join(t.TempDir(), "app_password")
	const want = "p@ss/wo:rd%'# x"
	if err := os.WriteFile(pwPath, []byte(want+"\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	t.Setenv(config.EnvDatabasePasswordFile, pwPath)

	s, err := load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(s.databaseURL, "mailroom_app:") {
		t.Fatalf("databaseURL = %q, does not look like it carries a password", s.databaseURL)
	}
}

func TestLoadDatabaseURLRequired(t *testing.T) {
	clearDrillEnv(t)
	if _, err := load(); err == nil {
		t.Fatal("load() error = nil, want error for missing database URL")
	}
}

// --- #551: the check-value comparison and the combined step-5 verdict -------

// fakeRootCheckStore is app.pgp_root_check for this unit suite: at most one
// row, and the real table's never-replace contract (INSERT-only, no UPDATE).
// Mirrors internal/pgpkeyd's own fakeRootCheckStore in rootcheck_test.go —
// this file is the interface's SECOND consumer, proving the SAME contract
// from the other side.
type fakeRootCheckStore struct {
	row *store.PGPRootCheck
	err error
}

func (f *fakeRootCheckStore) EnsureRootCheck(_ context.Context, candidate store.PGPRootCheck) (store.PGPRootCheck, bool, error) {
	if f.err != nil {
		return store.PGPRootCheck{}, false, f.err
	}
	if f.row != nil {
		return *f.row, false, nil
	}
	rec := candidate
	f.row = &rec
	return rec, true, nil
}

// testRoot is a distinct, deterministic 32-byte root per seed byte.
func testRoot(seed byte) []byte {
	root := make([]byte, crypto.KeySize)
	for i := range root {
		root[i] = seed ^ byte(i*29+1)
	}
	return root
}

// TestVerifyRootCheckValueAbsent: no row recorded yet. The call itself must
// record one (the trust-on-first-use step pgpkeyd.VerifyRoot's own first boot
// takes), but the STATUS reported for THIS call is absent — never a pass.
func TestVerifyRootCheckValueAbsent(t *testing.T) {
	st := &fakeRootCheckStore{}
	status, err := verifyRootCheckValue(context.Background(), st, testRoot(0x11))
	if err != nil {
		t.Fatalf("verifyRootCheckValue: %v", err)
	}
	if status != rootCheckAbsent {
		t.Fatalf("status = %v, want rootCheckAbsent", status)
	}
	if st.row == nil {
		t.Fatal("verifyRootCheckValue did not record a row on first sight, unlike a real first boot")
	}
}

// TestVerifyRootCheckValueMatched: the recorded row is exactly
// RootCheckValue(root) under the expected scheme.
func TestVerifyRootCheckValueMatched(t *testing.T) {
	root := testRoot(0x22)
	value, err := pgpkeyd.RootCheckValue(root)
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeRootCheckStore{row: &store.PGPRootCheck{Info: pgpRootCheckInfo, Value: value}}

	status, err := verifyRootCheckValue(context.Background(), st, root)
	if err != nil {
		t.Fatalf("verifyRootCheckValue: %v", err)
	}
	if status != rootCheckMatched {
		t.Fatalf("status = %v, want rootCheckMatched", status)
	}
}

// TestVerifyRootCheckValueMismatched: a row is recorded, but for a DIFFERENT
// root — the disaster case ADR-024/#525 exist to catch.
func TestVerifyRootCheckValueMismatched(t *testing.T) {
	recordedRoot := testRoot(0x33)
	value, err := pgpkeyd.RootCheckValue(recordedRoot)
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeRootCheckStore{row: &store.PGPRootCheck{Info: pgpRootCheckInfo, Value: value}}

	wrongRoot := testRoot(0x44)
	status, err := verifyRootCheckValue(context.Background(), st, wrongRoot)
	if err != nil {
		t.Fatalf("verifyRootCheckValue: %v", err)
	}
	if status != rootCheckMismatched {
		t.Fatalf("status = %v, want rootCheckMismatched", status)
	}
}

// TestVerifyRootCheckValueUnverifiableScheme: the recorded row uses a scheme
// this binary does not compute. Not the SAME failure as a wrong root, but
// refused the same way — a root that cannot be verified has not been.
func TestVerifyRootCheckValueUnverifiableScheme(t *testing.T) {
	root := testRoot(0x55)
	value, err := pgpkeyd.RootCheckValue(root)
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeRootCheckStore{row: &store.PGPRootCheck{Info: "mailroom-pgp-root-check-v2", Value: value}}

	status, err := verifyRootCheckValue(context.Background(), st, root)
	if err != nil {
		t.Fatalf("verifyRootCheckValue: %v", err)
	}
	if status != rootCheckMismatched {
		t.Fatalf("status = %v, want rootCheckMismatched for an unverifiable scheme", status)
	}
}

// TestVerifyRootCheckValueStoreError: EnsureRootCheck itself fails (e.g. the
// database is unreachable). Must propagate, not silently read as absent.
func TestVerifyRootCheckValueStoreError(t *testing.T) {
	st := &fakeRootCheckStore{err: errors.New("boom")}
	if _, err := verifyRootCheckValue(context.Background(), st, testRoot(0x66)); err == nil {
		t.Fatal("verifyRootCheckValue: want an error when the store fails, got nil")
	}
}

// TestPGPRootVerdictMatchedNoKeys: the narrow proof #551 exists to add — a
// matching check value with ZERO stored keys is now a real (if narrower) pass,
// not the old UNPROVEN wording.
func TestPGPRootVerdictMatchedNoKeys(t *testing.T) {
	result, lines := pgpRootVerdict(rootCheckMatched, 0, 0)
	if !result.present || !result.rootCheckMatched || result.stored != 0 || result.unsealed != 0 {
		t.Fatalf("result = %+v, want present+rootCheckMatched with 0 keys", result)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "matches this database's own recorded root check") {
		t.Fatalf("lines = %q, want it to state the check-value match", joined)
	}
	if !strings.Contains(joined, "narrower claim") {
		t.Fatalf("lines = %q, want it to disclaim the stronger \"opens a key\" claim", joined)
	}
}

// TestPGPRootVerdictMatchedWithKeys: keys exist AND the check value matches —
// the issue's "still unseal them all: that remains the stronger check".
// Both proofs must show up, not just one.
func TestPGPRootVerdictMatchedWithKeys(t *testing.T) {
	result, lines := pgpRootVerdict(rootCheckMatched, 3, 3)
	if !result.present || !result.rootCheckMatched || result.stored != 3 || result.unsealed != 3 {
		t.Fatalf("result = %+v, want present+rootCheckMatched with 3/3 keys", result)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "3/3") {
		t.Fatalf("lines = %q, want the 3/3 unseal count", joined)
	}
	if !strings.Contains(joined, "also matches") {
		t.Fatalf("lines = %q, want it to ALSO report the check-value match alongside the unseal count", joined)
	}
}

// TestPGPRootVerdictAbsentNoKeys: the pre-#551 case, wording preserved.
func TestPGPRootVerdictAbsentNoKeys(t *testing.T) {
	result, lines := pgpRootVerdict(rootCheckAbsent, 0, 0)
	if !result.present || result.rootCheckMatched || result.stored != 0 || result.unsealed != 0 {
		t.Fatalf("result = %+v, want present without rootCheckMatched and 0 keys", result)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "nothing to") || !strings.Contains(joined, "unseal") {
		t.Fatalf("lines = %q, want the old \"nothing to unseal\" wording preserved", joined)
	}
	if strings.Contains(joined, "PROVEN") {
		t.Fatalf("lines = %q, an absent check value must never be folded into a pass", joined)
	}
}

// TestPGPRootVerdictAbsentWithKeys: no recorded check value, but the keys
// still unseal — the pre-#551 pass-via-unseal path, unaffected.
func TestPGPRootVerdictAbsentWithKeys(t *testing.T) {
	result, lines := pgpRootVerdict(rootCheckAbsent, 2, 2)
	if !result.present || result.rootCheckMatched || result.stored != 2 || result.unsealed != 2 {
		t.Fatalf("result = %+v, want present with 2/2 keys and no check-value claim", result)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "also matches") {
		t.Fatalf("lines = %q, must NOT claim a check-value match when the row was absent", joined)
	}
}
