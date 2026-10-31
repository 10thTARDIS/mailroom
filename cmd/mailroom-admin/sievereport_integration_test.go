// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// Integration coverage for sieve-report (#50) against a real Postgres: the
// account rows come through UserTx under RLS, the credential is opened from a
// REAL sealed row (onboarding.SealCredentials via store.CreateAccount), and the
// session it logs in with goes to the fake ManageSieve server. The pure report
// is covered in sievereport_test.go; this proves the database half.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"mailroom/internal/sieve/sievetest"
	"mailroom/internal/store"
)

func TestSieveReportLoadsSealedCredentialsUnderRLS(t *testing.T) {
	h := newAccountsReportHarness(t)
	ctx := context.Background()
	keys := fakeMasterKeyProvider{key: accountsReportMaster}

	tagA := "sieve-a-" + store.NewULID().String()
	tagB := "sieve-b-" + store.NewULID().String()
	userA := seedAccountsReportUser(t, h.super, tagA)
	userB := seedAccountsReportUser(t, h.super, tagB)
	createSealedAccount(t, h.store, userA, "mine", "a@example.com", "pw-a")
	createSealedAccount(t, h.store, userB, "theirs", "b@example.com", "pw-b")

	targets, err := loadSieveTargets(ctx, h.store, keys, userA, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(targets) != 1 || targets[0].Name != "mine" || targets[0].Username != "a@example.com" ||
		targets[0].Password != "pw-a" || targets[0].IMAPHost != "imap.example.com" {
		t.Fatalf("targets = %+v", targets)
	}

	// RLS negative: user A cannot name user B's account.
	if _, err := loadSieveTargets(ctx, h.store, keys, userA, "theirs"); err == nil ||
		!strings.Contains(err.Error(), `no account named "theirs"`) {
		t.Fatalf("naming another user's account: err = %v", err)
	}
	// Positive control for the line above: B CAN name it.
	if got, err := loadSieveTargets(ctx, h.store, keys, userB, "theirs"); err != nil || len(got) != 1 {
		t.Fatalf("owner naming its own account: %+v, %v", got, err)
	}

	// End to end: the opened credential logs in to the fake and the report runs.
	f := sievetest.New(t, sievetest.Options{
		Username: "a@example.com", Password: "pw-a",
		Scripts: []sievetest.Script{{Name: "s", Body: "require \"vacation\";\r\nvacation \"x\";\r\n", Active: true}},
	})
	var buf bytes.Buffer
	failed := reportSieve(ctx, &buf, targets, sieveReportOptions{
		Host: f.Host(), Port: f.Port(), AllowPrivateHosts: true, TLSConfig: f.ClientTLSConfig(),
	})
	if failed != 0 || !strings.Contains(buf.String(), "vacation action: PRESENT") {
		t.Fatalf("failed=%d\n%s", failed, buf.String())
	}
	if f.WriteAttempts() != 0 {
		t.Fatal("the report sent a state-changing verb")
	}
}
