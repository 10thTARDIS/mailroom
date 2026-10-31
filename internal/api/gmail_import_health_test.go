// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"testing"

	"mailroom/internal/oauthmail"
	"mailroom/internal/store"
)

func TestGmailImportProgressEstimate(t *testing.T) {
	on := int64(1000)
	// 100 of 1000 imported, 10 kB each: 900 left x 10,000 = 9 MB; history's
	// share of a 1 MB budget is 900 kB/day: 10 days.
	h := gmailImportOf(store.GmailImportProgress{MessagesOnServer: &on, MessagesImported: 100, BytesImported: 1_000_000}, 1_000_000)
	if h.RemainingEstimateBytes == nil || *h.RemainingEstimateBytes != 9_000_000 || h.EtaDays == nil || *h.EtaDays != 10 {
		t.Errorf("estimate = %v / %v", h.RemainingEstimateBytes, h.EtaDays)
	}
	// Nothing imported yet: no average, so no estimate — null, not zero.
	h = gmailImportOf(store.GmailImportProgress{MessagesOnServer: &on}, 1_000_000)
	if h.RemainingEstimateBytes != nil || h.EtaDays != nil {
		t.Errorf("an estimate from no data: %v / %v", h.RemainingEstimateBytes, h.EtaDays)
	}
	h = gmailImportOf(store.GmailImportProgress{MessagesOnServer: &on, MessagesImported: 1000, BytesImported: 5, HistoryComplete: true}, 1_000_000)
	if h.EtaDays == nil || *h.EtaDays != 0 {
		t.Errorf("a complete import has eta 0, got %v", h.EtaDays)
	}
}

// The effective writeback of a Gmail OAuth account is OFF while the Gmail
// writeback hold stands, whatever its column says.
func TestGmailWritebackHeldInHealth(t *testing.T) {
	if oauthmail.GmailWritebackReady {
		t.Fatal("Gmail writeback stays held until a live test flips the switch")
	}
	if !writebackHeld(store.Account{AuthKind: "oauth2", ProviderProfile: "gmail", WritebackEnabled: true}) {
		t.Error("a Gmail OAuth account's writeback is not held")
	}
	if writebackHeld(store.Account{AuthKind: "password", ProviderProfile: "gmail"}) ||
		writebackHeld(store.Account{AuthKind: "oauth2", ProviderProfile: "microsoft"}) {
		t.Error("the hold reached an account it does not govern")
	}
}
