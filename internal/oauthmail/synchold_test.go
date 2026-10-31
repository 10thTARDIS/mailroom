// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"testing"

	"mailroom/internal/store"
)

// THE TWO HALVES OF THE GMAIL WRITEBACK SWITCH AGREE (#52, increment 5b).
// oauthmail.GmailWritebackReady gates Account/set and getHealth; its mirror
// store.GmailWritebackReady gates EnqueueIntent and the writeback runner's
// drain (store cannot import oauthmail). Flipping one without the other would
// leave the settings and the queue disagreeing about whether Mailroom writes
// to Gmail, so the flip is two lines, and this test says so.
func TestGmailWritebackSwitchesAgree(t *testing.T) {
	if GmailWritebackReady != store.GmailWritebackReady {
		t.Fatalf("oauthmail.GmailWritebackReady = %v but store.GmailWritebackReady = %v: flip both",
			GmailWritebackReady, store.GmailWritebackReady)
	}
	for _, tc := range []struct{ kind, profile string }{
		{"oauth2", "gmail"}, {"oauth2", "microsoft"}, {"password", "gmail"}, {"password", "generic"},
	} {
		_, _, held := WritebackHold(tc.kind, tc.profile)
		if held != store.GmailWritebackHeld(tc.kind, tc.profile) {
			t.Errorf("WritebackHold(%s, %s) = %v, store.GmailWritebackHeld = %v", tc.kind, tc.profile,
				held, store.GmailWritebackHeld(tc.kind, tc.profile))
		}
	}
}

// While GmailSyncReady is false, exactly the Gmail OAuth accounts are held.
func TestSyncHold(t *testing.T) {
	if GmailSyncReady {
		t.Skip("the Gmail sync model has landed; nothing is held")
	}
	for _, tc := range []struct {
		kind, profile string
		held          bool
	}{
		{"oauth2", "gmail", true},
		{"oauth2", "microsoft", false},
		{"password", "gmail", false}, // an app-password Gmail account is the user's own choice
		{"password", "generic", false},
	} {
		code, msg, held := SyncHold(tc.kind, tc.profile)
		if held != tc.held || (held && (code != SyncHoldGmailPending || msg != SyncHoldGmailPendingMessage)) ||
			(!held && (code != "" || msg != "")) {
			t.Errorf("SyncHold(%s, %s) = %q, %q, %v; want held=%v", tc.kind, tc.profile, code, msg, held, tc.held)
		}
	}
}
