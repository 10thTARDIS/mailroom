package imapclient_test

// Mailroom patch 2 (MAILROOM-PATCHES.md): FLAGS () is empty and non-nil, a
// response without FLAGS leaves Flags nil.

import (
	"net"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestFetchFlagsAbsentVersusEmpty(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	saw := make(chan string, 1)
	go scriptedGmail(serverConn, []string{
		`* 1 FETCH (UID 4 FLAGS ())`,
		`* 2 FETCH (UID 5 RFC822.SIZE 10)`,
		`* 3 FETCH (UID 6 FLAGS (\Seen))`,
	}, saw)
	c := imapclient.New(clientConn, nil)
	defer c.Close()
	msgs, err := c.Fetch(imap.UIDSetNum(4, 5, 6), &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true}).Collect()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	<-saw
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if msgs[0].Flags == nil || len(msgs[0].Flags) != 0 {
		t.Errorf("FLAGS () = %#v, want empty and non-nil", msgs[0].Flags)
	}
	if msgs[1].Flags != nil {
		t.Errorf("no FLAGS item = %#v, want nil", msgs[1].Flags)
	}
	if len(msgs[2].Flags) != 1 || msgs[2].Flags[0] != imap.FlagSeen {
		t.Errorf("FLAGS (\\Seen) = %#v", msgs[2].Flags)
	}
}
