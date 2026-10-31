// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// TestSyncResolverHonorsAllowPrivateAccountHosts: `mailroom-admin sync` dials
// every account's IMAP host through the guarded sync.Dial (#625 §3), so the
// operator's MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS must reach the Configs its
// resolver builds — or a LAN account the server syncs would be refused here.
// It drives syncResolverOptions, the exact option set runSync passes.
func TestSyncResolverHonorsAllowPrivateAccountHosts(t *testing.T) {
	userID := ulid.Make()
	ct, err := onboarding.SealCredentials(allowPrivateHostsTestMaster, userID, onboarding.Credentials{
		IMAPUsername: "u", IMAPPassword: "p",
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	acct := store.Account{ID: ulid.Make(), IMAPHost: "10.0.0.5", IMAPPort: 993, CredentialsCT: ct}
	for _, allow := range []bool{false, true} {
		d := &deps{keys: fakeKeyProvider{key: allowPrivateHostsTestMaster}, allowPrivateAccountHosts: allow}
		req, err := syncrunner.NewCredentialResolver(d.keys, syncResolverOptions(d)...).Prepare(context.Background(), userID, acct)
		if err != nil {
			t.Fatalf("allow=%v: prepare: %v", allow, err)
		}
		if req.Config.AllowPrivateHosts != allow {
			t.Errorf("allow=%v: the sync dial's Config.AllowPrivateHosts = %v", allow, req.Config.AllowPrivateHosts)
		}
	}
}
