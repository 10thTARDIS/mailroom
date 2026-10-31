package imapclient_test

// Mailroom patch 3 (MAILROOM-PATCHES.md): STORE ±X-GM-LABELS and
// SEARCH X-GM-MSGID.

import (
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// TestStoreGmailLabelsWire pins the command's wire form: the op sign, the
// item name, system labels as atoms, user labels quoted in modified UTF-7
// with quotes and backslashes escaped, and the FETCH echo parsed back.
func TestStoreGmailLabelsWire(t *testing.T) {
	cases := []struct {
		name   string
		store  imap.StoreGmailLabels
		want   string
		echo   string
		labels []string
	}{
		{
			name:   "add system and user labels",
			store:  imap.StoreGmailLabels{Op: imap.StoreFlagsAdd, Labels: []string{`\Inbox`, "Work", "Two words"}},
			want:   `UID STORE 7 +X-GM-LABELS (\Inbox "Work" "Two words")`,
			echo:   `* 3 FETCH (X-GM-LABELS (\Inbox "Work" "Two words") UID 7)`,
			labels: []string{`\Inbox`, "Work", "Two words"},
		},
		{
			name:   "remove the inbox (archive)",
			store:  imap.StoreGmailLabels{Op: imap.StoreFlagsDel, Labels: []string{`\Inbox`}},
			want:   `UID STORE 7 -X-GM-LABELS (\Inbox)`,
			echo:   `* 3 FETCH (X-GM-LABELS () UID 7)`,
			labels: []string{},
		},
		{
			name:   "non-ASCII label in modified UTF-7",
			store:  imap.StoreGmailLabels{Op: imap.StoreFlagsAdd, Labels: []string{"été", "日本", "R&D"}},
			want:   `UID STORE 7 +X-GM-LABELS ("&AOk-t&AOk-" "&ZeVnLA-" "R&-D")`,
			echo:   `* 3 FETCH (X-GM-LABELS ("&AOk-t&AOk-" "&ZeVnLA-" "R&-D") UID 7)`,
			labels: []string{"été", "日本", "R&D"},
		},
		{
			name:   "quotes and backslashes inside a user label are escaped",
			store:  imap.StoreGmailLabels{Op: imap.StoreFlagsAdd, Labels: []string{`Say "hi"`, `a\b`}},
			want:   `UID STORE 7 +X-GM-LABELS ("Say \"hi\"" "a\\b")`,
			echo:   `* 3 FETCH (X-GM-LABELS ("Say \"hi\"" "a\\b") UID 7)`,
			labels: []string{`Say "hi"`, `a\b`},
		},
		{
			name:  "silent",
			store: imap.StoreGmailLabels{Op: imap.StoreFlagsAdd, Silent: true, Labels: []string{"Work"}},
			want:  `UID STORE 7 +X-GM-LABELS.SILENT ("Work")`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			saw := make(chan string, 1)
			var untagged []string
			if tc.echo != "" {
				untagged = []string{tc.echo}
			}
			go scriptedGmail(serverConn, untagged, saw)
			c := imapclient.New(clientConn, nil)
			defer c.Close()
			msgs, err := c.StoreGmailLabels(imap.UIDSetNum(7), &tc.store, nil).Collect()
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			cmd := <-saw
			// Strip the tag.
			if _, rest, ok := strings.Cut(cmd, " "); ok {
				cmd = rest
			}
			if cmd != tc.want {
				t.Errorf("wire = %s\n want %s", cmd, tc.want)
			}
			if tc.echo == "" {
				return
			}
			if len(msgs) != 1 {
				t.Fatalf("echo: %d messages", len(msgs))
			}
			if !reflect.DeepEqual(msgs[0].GmailLabels, tc.labels) {
				t.Errorf("echoed labels = %q, want %q (round trip through the FETCH decoder)", msgs[0].GmailLabels, tc.labels)
			}
		})
	}
}

// TestStoreGmailLabelsRefusesAmbiguousUserLabel: a user label beginning with
// a backslash would be read by Gmail as a system label, so the encoder
// refuses to send it (the command fails; nothing reaches the server as a
// valid STORE).
func TestStoreGmailLabelsRefusesAmbiguousUserLabel(t *testing.T) {
	for _, bad := range []string{`\Two words`, `\`, ""} {
		clientConn, serverConn := net.Pipe()
		saw := make(chan string, 1)
		go scriptedGmail(serverConn, nil, saw)
		c := imapclient.New(clientConn, nil)
		_, err := c.StoreGmailLabels(imap.UIDSetNum(7), &imap.StoreGmailLabels{
			Op: imap.StoreFlagsAdd, Labels: []string{bad},
		}, nil).Collect()
		if err == nil {
			t.Errorf("label %q: store succeeded, want an encoding error", bad)
		}
		select {
		case cmd := <-saw:
			t.Errorf("label %q: the server received %q", bad, cmd)
		default:
		}
		c.Close()
	}
}

// TestSearchGmailMsgID pins `UID SEARCH X-GM-MSGID <n>` for a full 64-bit id.
func TestSearchGmailMsgID(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	saw := make(chan string, 1)
	go scriptedGmail(serverConn, []string{`* SEARCH 42`}, saw)
	c := imapclient.New(clientConn, nil)
	defer c.Close()
	data, err := c.UIDSearch(&imap.SearchCriteria{GmailMsgID: []uint64{18446744073709551615}}, nil).Wait()
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	cmd := <-saw
	if !strings.HasSuffix(cmd, "UID SEARCH X-GM-MSGID 18446744073709551615") {
		t.Errorf("wire = %q", cmd)
	}
	if got := data.AllUIDs(); len(got) != 1 || got[0] != 42 {
		t.Errorf("uids = %v", got)
	}
}

// TestSearchWithoutGmailMsgIDUnchanged: a search that does not ask for the
// Gmail key does not name it.
func TestSearchWithoutGmailMsgIDUnchanged(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	saw := make(chan string, 1)
	go scriptedGmail(serverConn, []string{`* SEARCH`}, saw)
	c := imapclient.New(clientConn, nil)
	defer c.Close()
	if _, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{imap.UIDSetNum(3)}}, nil).Wait(); err != nil {
		t.Fatalf("search: %v", err)
	}
	if cmd := <-saw; strings.Contains(cmd, "X-GM") {
		t.Errorf("command %q names a Gmail key nobody asked for", cmd)
	}
}
