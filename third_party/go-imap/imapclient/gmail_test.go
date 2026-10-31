package imapclient_test

// Mailroom patch (MAILROOM-PATCHES.md): X-GM-EXT-1 FETCH items.

import (
	"bufio"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// scriptedGmail answers every UID command with the given untagged lines and
// reports the command line it saw.
func scriptedGmail(conn net.Conn, untagged []string, sawCmd chan<- string) {
	defer conn.Close()
	w := bufio.NewWriter(conn)
	r := bufio.NewReader(conn)
	put := func(s string) { w.WriteString(s + "\r\n"); w.Flush() }
	put("* OK [CAPABILITY IMAP4rev1 X-GM-EXT-1] Gimap ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		tag := f[0]
		switch strings.ToUpper(f[1]) {
		case "UID":
			sawCmd <- strings.TrimSpace(line)
			for _, u := range untagged {
				put(u)
			}
			put(tag + " OK Success")
		case "LOGOUT":
			put("* BYE")
			put(tag + " OK")
			return
		default:
			put(tag + " BAD unexpected")
		}
	}
}

func TestFetchGmailExtensions(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	saw := make(chan string, 1)
	go scriptedGmail(serverConn, []string{
		// Real-shaped: system labels as backslash atoms AND as Gmail's quoted
		// form, a label with a space, modified UTF-7, a quoted escape, a
		// literal, and the items in a different order than requested.
		`* 1 FETCH (X-GM-THRID 1266894439832287888 X-GM-MSGID 1278455344230334865 UID 4 X-GM-LABELS (\Inbox \Sent "\\Important" "Muy Importante" "&AOk-t&AOk-" {7}` + "\r\n" + `Lit lab "Quote \"q\"" Plain) FLAGS (\Seen))`,
		`* 2 FETCH (UID 5 X-GM-LABELS () X-GM-MSGID 1)`,
	}, saw)
	c := imapclient.New(clientConn, nil)
	defer c.Close()

	msgs, err := c.Fetch(imap.UIDSetNum(4, 5), &imap.FetchOptions{
		UID: true, Flags: true, GmailMsgID: true, GmailThrID: true, GmailLabels: true,
	}).Collect()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	cmd := <-saw
	for _, item := range []string{"X-GM-MSGID", "X-GM-THRID", "X-GM-LABELS"} {
		if !strings.Contains(cmd, item) {
			t.Errorf("command %q does not request %s", cmd, item)
		}
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	m := msgs[0]
	if m.GmailMsgID != 1278455344230334865 || m.GmailThrID != 1266894439832287888 || m.UID != 4 {
		t.Errorf("ids: msgid=%d thrid=%d uid=%d", m.GmailMsgID, m.GmailThrID, m.UID)
	}
	want := []string{`\Inbox`, `\Sent`, `\Important`, "Muy Importante", "été", "Lit lab", `Quote "q"`, "Plain"}
	if !reflect.DeepEqual(m.GmailLabels, want) {
		t.Errorf("labels = %q, want %q", m.GmailLabels, want)
	}
	if len(m.Flags) != 1 || m.Flags[0] != imap.FlagSeen {
		t.Errorf("flags after labels = %v", m.Flags)
	}
	if msgs[1].GmailLabels == nil || len(msgs[1].GmailLabels) != 0 {
		t.Errorf("empty list = %#v, want empty non-nil", msgs[1].GmailLabels)
	}
	if msgs[1].GmailMsgID != 1 {
		t.Errorf("msgid = %d", msgs[1].GmailMsgID)
	}
}

// TestFetchWithoutGmailItemsUnchanged: a FETCH that does not ask for the
// Gmail items does not name them (the patch must not change other commands).
func TestFetchWithoutGmailItemsUnchanged(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	saw := make(chan string, 1)
	go scriptedGmail(serverConn, []string{`* 1 FETCH (UID 4 FLAGS ())`}, saw)
	c := imapclient.New(clientConn, nil)
	defer c.Close()
	if _, err := c.Fetch(imap.UIDSetNum(4), &imap.FetchOptions{UID: true, Flags: true}).Collect(); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if cmd := <-saw; strings.Contains(cmd, "X-GM") {
		t.Errorf("command %q names a Gmail item nobody asked for", cmd)
	}
}
