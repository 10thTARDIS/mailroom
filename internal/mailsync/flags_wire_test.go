// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"context"
	"testing"
	"time"

	"mailroom/internal/sync"
	"mailroom/internal/sync/gmailtest"
)

// These drive the PRODUCTION sync.Dial, go-imap (with the vendored patches)
// and the engine against a scripted IMAP server speaking real wire bytes, with
// the in-memory store and ingester — so "the response lacked FLAGS" is what
// the server actually sent, not what a fake session pretended (#909).

// wireFixture is a generic (non-Gmail) server with INBOX holding UIDs 1 and 2,
// both recorded \Seen in Mailroom.
func wireFixture(t *testing.T, condStore bool) (*gmailtest.Server, *fakeStore, *fakeIngester, *storedRow) {
	t.Helper()
	g := gmailtest.New(t)
	g.GmailExt = false
	g.CondStore = condStore
	g.AddMailbox(&gmailtest.Mailbox{Name: "INBOX", UIDValidity: 10})
	g.Append("INBOX", &gmailtest.Message{Body: []byte("Subject: one\r\n\r\n1\r\n"), Flags: []string{`\Seen`}})
	g.Append("INBOX", &gmailtest.Message{Body: []byte("Subject: two\r\n\r\n2\r\n"), Flags: []string{`\Seen`}})
	st, ing := newFakeStore(), newFakeIngester()
	row := st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 2, highestModSeq: 1}, 1, 2)
	row.provFlags = map[int64][]string{1: {`\Seen`}, 2: {`\Seen`}}
	ing.st = st
	return g, st, ing, row
}

func wireSync(t *testing.T, g *gmailtest.Server, st *fakeStore, ing *fakeIngester, opts ...Option) Summary {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req := testReq()
	req.Config = g.Listen(sync.TLSInsecure)
	sum, err := New(st, ing, nil, opts...).SyncAccount(ctx, req)
	if err != nil {
		t.Logf("sync: %v", err)
	}
	return sum
}

// A server that sends a message's FLAGS in a SECOND FETCH response: go-imap
// routes that one to the unilateral handler, so the message reaches the
// engine without FLAGS. Before #909 the CONDSTORE pass refreshed it with an
// empty set, clearing \Seen. Its flags must be untouched.
//
// PROBE: revert go-imap patch 2 (third_party/go-imap/imapclient/fetch.go) or
// the skip in refreshFlagsCounting and this goes red.
func TestWireSplitFlagsLeaveFlagsUntouched(t *testing.T) {
	g, st, ing, row := wireFixture(t, true)
	g.SetFlags("INBOX", 2, `\Seen`) // bump MODSEQ so the flag pass asks
	g.SetFlagFault("INBOX", 2, gmailtest.LabelsSplit)
	sum := wireSync(t, g, st, ing)
	if got := row.provFlags[2]; len(got) != 1 || got[0] != `\Seen` {
		t.Fatalf("UID 2 flags = %q after a split FLAGS response, want untouched [\\Seen] (#909)", got)
	}
	if sum.FlagsAbsent == 0 {
		t.Errorf("FlagsAbsent = 0: the split response was not noticed")
	}
}

// The other half of absent-versus-empty: a server that answers FLAGS () — the
// message was marked unread elsewhere — IS a reading, and must be applied.
// Without go-imap patch 2, FLAGS () parsed as nil and the #909 skip would
// have read every genuinely unread message as "no reading".
//
// PROBE: revert patch 2 (the nil -> empty slice in populateItemData) and this
// goes red.
func TestWireEmptyFlagsAreAReading(t *testing.T) {
	g, st, ing, row := wireFixture(t, true)
	g.SetFlags("INBOX", 1) // unread elsewhere; MODSEQ bumped
	wireSync(t, g, st, ing)
	if got, ok := row.provFlags[1]; !ok || got == nil || len(got) != 0 {
		t.Fatalf("UID 1 flags = %#v after FLAGS (), want the empty reading applied", got)
	}
}

// A response cut off before FLAGS, then the connection drops: the message
// buffer handed on holds only what was parsed (its UID), with no error until
// the command closes. Its flags must be untouched.
func TestWireTruncatedFetchLeavesFlagsUntouched(t *testing.T) {
	g, st, ing, row := wireFixture(t, true)
	g.SetFlags("INBOX", 2, `\Seen`)
	g.SetFlagFault("INBOX", 2, gmailtest.LabelsTruncated)
	sum := wireSync(t, g, st, ing)
	if got := row.provFlags[2]; len(got) != 1 || got[0] != `\Seen` {
		t.Fatalf("UID 2 flags = %q after a truncated FETCH, want untouched [\\Seen] (#909)", got)
	}
	if len(sum.Errors) == 0 {
		t.Errorf("the truncated FETCH was not reported as a failure")
	}
}

// The CONDSTORE-less sweep end to end, Outlook-shaped (no CONDSTORE): a \Seen
// removed on the server (marked unread on a phone) WITHOUT any MODSEQ is
// observed with the sweep on, and never with it off.
func TestWireSweepSeesAQuietRemoteChange(t *testing.T) {
	for _, on := range []bool{true, false} {
		g, st, ing, row := wireFixture(t, false)
		g.SetFlagsQuietly("INBOX", 1) // unread on the phone
		var opts []Option
		if on {
			opts = append(opts, WithFlagSweep(sweepOn))
		}
		wireSync(t, g, st, ing, opts...)
		got := row.provFlags[1]
		if on && len(got) != 0 {
			t.Errorf("sweep ON: UID 1 flags = %q, want [] (the phone's unread)", got)
		}
		if !on && (len(got) != 1 || got[0] != `\Seen`) {
			t.Errorf("sweep OFF: UID 1 flags = %q, want the stale [\\Seen] — something else observed it", got)
		}
	}
}
