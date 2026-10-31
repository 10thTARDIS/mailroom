// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// IsSelf is the one rule for "the user's own message" the authResults notice
// and the admin reports share (#766): live in Sent or Drafts. The From address
// is deliberately not an input (a spoofer writes it), so there is no way to
// pass one.
func TestAuthSelfFactsIsSelf(t *testing.T) {
	sent, drafts, inbox := ulid.Make(), ulid.Make(), ulid.Make()
	f := AuthSelfFacts{OutboundMailboxes: map[ulid.ULID]bool{sent: true, drafts: true}}
	cases := []struct {
		name string
		mb   []ulid.ULID
		want bool
	}{
		{"inbound (control)", []ulid.ULID{inbox}, false},
		{"in Sent", []ulid.ULID{sent}, true},
		{"in Drafts", []ulid.ULID{drafts}, true},
		{"in Inbox and Sent", []ulid.ULID{inbox, sent}, true},
		{"no mailboxes", nil, false},
	}
	for _, c := range cases {
		if got := f.IsSelf(c.mb); got != c.want {
			t.Errorf("%s: IsSelf = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTrustTimeIsZeroUnlessTheProviderDatedIt(t *testing.T) {
	at := time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)
	if got := TrustTime(at, true); !got.Equal(at) {
		t.Errorf("provider-dated: %v, want %v", got, at)
	}
	if got := TrustTime(at, false); !got.IsZero() {
		t.Errorf("undated: %v, want the zero time", got)
	}
}
