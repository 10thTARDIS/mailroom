// SPDX-License-Identifier: AGPL-3.0-only

package sync_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"mailroom/internal/sync"
	"mailroom/internal/sync/gmailtest"
)

// THE X-GM-EXT-1 READ PATH THROUGH THE SEAM (#52, ADR-047). Before the
// vendored go-imap patch, a FETCH response carrying any X-GM-* item failed
// the WHOLE command ("unsupported msg-att name"), so this is the first test
// that can exist at all. It drives the production sync.Dial and FetchByUID
// against the scripted fake, with labels written the ways Gmail writes them:
// system labels as backslash atoms AND in Gmail's quoted form, a label with a
// space, an escaped quote, modified UTF-7, and a literal.
func TestFetchGmailItemsThroughSeam(t *testing.T) {
	g := gmailtest.New(t)
	g.AddMailbox(&gmailtest.Mailbox{Name: "[Gmail]/All Mail", Attrs: []string{`\All`}})
	g.Append("[Gmail]/All Mail", &gmailtest.Message{
		MsgID: 1278455344230334865, ThrID: 1266894439832287888,
		WireLabels: `\Inbox "\\Important" \Sent \Starred "Muy Importante" "Say \"hi\"" "&AOk-t&AOk-" "Proj&AOk-ts/2026" {7}` + "\r\n" + `Lit lab`,
		Body:       []byte("Subject: x\r\n\r\nbody\r\n"),
		Flags:      []string{`\Seen`, `\Flagged`},
	})
	g.Append("[Gmail]/All Mail", &gmailtest.Message{MsgID: 7, WireLabels: ``, Body: []byte("x")})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := sync.Dial(ctx, g.Listen(sync.TLSInsecure))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = sess.Close() }()
	if !sess.Supports(sync.CapabilityGmailExt) {
		t.Fatal("the fake advertises X-GM-EXT-1 but Supports says no")
	}
	if _, err := sess.Select(ctx, "[Gmail]/All Mail", sync.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("examine: %v", err)
	}
	var got []sync.Message
	err = sess.FetchByUID(ctx, sync.UIDSetRange(1, 0), sync.FetchItems{Flags: true, RFC822Size: true, GmailExt: true},
		func(m sync.Message) error { got = append(got, m); return nil })
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	m := got[0]
	if m.GmailMsgID != 1278455344230334865 || m.GmailThrID != 1266894439832287888 {
		t.Errorf("ids = %d / %d", m.GmailMsgID, m.GmailThrID)
	}
	want := []string{`\Inbox`, `\Important`, `\Sent`, `\Starred`, "Muy Importante", `Say "hi"`, "été", "Projéts/2026", "Lit lab"}
	if !reflect.DeepEqual(m.GmailLabels, want) {
		t.Errorf("labels\n got %q\nwant %q", m.GmailLabels, want)
	}
	if len(m.Flags) != 2 {
		t.Errorf("flags = %v (the items after X-GM-LABELS must still parse)", m.Flags)
	}
	if got[1].GmailLabels == nil || len(got[1].GmailLabels) != 0 {
		t.Errorf("an empty X-GM-LABELS () = %#v, want empty and non-nil", got[1].GmailLabels)
	}

	// Without GmailExt the command does not name the items at all.
	g.ResetCounters()
	if err := sess.FetchByUID(ctx, sync.UIDSetNum(1), sync.FetchItems{Flags: true},
		func(sync.Message) error { return nil }); err != nil {
		t.Fatalf("plain fetch: %v", err)
	}
	for _, f := range g.Fetches() {
		if strings.Contains(f, "X-GM") {
			t.Errorf("a FETCH that did not ask for X-GM items named them: %s", f)
		}
	}
}

// The modified UTF-7 the fake writes is the one go-imap decodes: a sanity
// check on the fixture itself, so the test above is not agreeing with a
// broken encoder.
func TestGmailFakeEncodesModifiedUTF7(t *testing.T) {
	for in, want := range map[string]string{
		"été":      "&AOk-t&AOk-",
		"a&b":      "a&-b",
		"Plain":    "Plain",
		"日本語":      "&ZeVnLIqe-",
		"Projéts/": "Proj&AOk-ts/",
	} {
		if got := gmailtest.EncodeMUTF7(in); got != want {
			t.Errorf("encode(%q) = %q, want %q", in, got, want)
		}
	}
}
