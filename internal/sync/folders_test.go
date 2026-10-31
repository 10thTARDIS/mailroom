// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"mailroom/internal/egress/egresstest"
)

// scriptedIMAP is a minimal IMAP server for FolderAdmin: it advertises caps,
// records every command line after LOGIN, answers STATUS with statusLine, and
// OKs everything else.
type scriptedIMAP struct {
	caps       string
	statusLine string // the attribute list of every untagged STATUS response

	mu   stdsync.Mutex
	seen []string
}

func (s *scriptedIMAP) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *scriptedIMAP) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	w := bufio.NewWriter(c)
	r := bufio.NewReader(c)
	put := func(l string) { _, _ = w.WriteString(l + "\r\n"); _ = w.Flush() }
	put("* OK [CAPABILITY " + s.caps + "] ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		tag, cmd := f[0], strings.ToUpper(f[1])
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), f[0]))
		if cmd != "LOGIN" {
			s.mu.Lock()
			s.seen = append(s.seen, rest)
			s.mu.Unlock()
		}
		switch cmd {
		case "CAPABILITY":
			put("* CAPABILITY " + s.caps)
			put(tag + " OK done")
		case "LOGIN":
			put(tag + " OK [CAPABILITY " + s.caps + "] logged in")
		case "STATUS":
			put("* STATUS " + f[2] + " " + s.statusLine)
			put(tag + " OK done")
		case "SELECT", "EXAMINE":
			put("* 3 EXISTS")
			put("* OK [UIDVALIDITY 7] ok")
			put("* OK [UIDNEXT 4] ok")
			put(tag + " OK [READ-WRITE] done")
		case "LOGOUT":
			put("* BYE")
			put(tag + " OK done")
			return
		default:
			put(tag + " OK done")
		}
	}
}

func dialScripted(t *testing.T, srv *scriptedIMAP) FolderAdmin {
	t.Helper()
	ln := egresstest.Listen(t, srv.serve)
	fa, err := DialFolderAdmin(context.Background(), Config{
		Host: "127.0.0.1", Port: ln.Port(), Username: "you@example.com", Password: "pw",
		TLS: TLSInsecure, DialTimeout: 5 * time.Second, AllowPrivateHosts: true,
	})
	if err != nil {
		t.Fatalf("DialFolderAdmin: %v", err)
	}
	t.Cleanup(func() { _ = fa.Close() })
	return fa
}

func sentAny(cmds []string, verbs ...string) []string {
	var hit []string
	for _, c := range cmds {
		u := strings.ToUpper(c)
		for _, v := range verbs {
			if strings.HasPrefix(u, v) {
				hit = append(hit, c)
			}
		}
	}
	return hit
}

// MoveAll on a server WITHOUT MOVE must refuse before sending anything — in
// particular not go-imap's COPY + STORE \Deleted + EXPUNGE fallback. The
// second case is the negative control: the same recorder, with MOVE
// advertised, does see the UID MOVE, so an empty recording above means
// "nothing was sent", not "the recorder is blind".
func TestMoveAllRefusesWithoutMoveAndNeverFallsBack(t *testing.T) {
	noMove := &scriptedIMAP{caps: "IMAP4rev1 UIDPLUS UNSELECT", statusLine: "(MESSAGES 3 UIDNEXT 4 UIDVALIDITY 7)"}
	fa := dialScripted(t, noMove)
	if fa.SupportsMove() {
		t.Fatal("SupportsMove() = true on a server that did not advertise MOVE")
	}
	err := fa.MoveAll(context.Background(), "Src", "Dst")
	if !errors.Is(err, ErrMoveUnsupported) {
		t.Fatalf("MoveAll = %v, want ErrMoveUnsupported", err)
	}
	_, _ = fa.Status(context.Background(), "Src") // a round trip, so every earlier command has been read
	if hit := sentAny(noMove.commands(), "SELECT", "UID COPY", "COPY", "UID STORE", "STORE", "UID EXPUNGE", "EXPUNGE", "UID MOVE", "MOVE"); len(hit) != 0 {
		t.Errorf("a refused MoveAll still sent %v", hit)
	}

	withMove := &scriptedIMAP{caps: "IMAP4rev1 UIDPLUS UNSELECT MOVE"}
	fa = dialScripted(t, withMove)
	if err := fa.MoveAll(context.Background(), "Src", "Dst"); err != nil {
		t.Fatalf("MoveAll with MOVE advertised: %v", err)
	}
	if hit := sentAny(withMove.commands(), "UID MOVE 1:* "); len(hit) != 1 {
		t.Errorf("negative control: the recorder saw %v, want exactly one `UID MOVE 1:*`", withMove.commands())
	}
	if hit := sentAny(withMove.commands(), "UID COPY", "COPY", "EXPUNGE"); len(hit) != 0 {
		t.Errorf("MoveAll used the fallback although MOVE is advertised: %v", hit)
	}
}

// A STATUS answer without MESSAGES must be an error, never 0 — the delete
// decision is "STATUS says 0". Negative control: the same server with
// MESSAGES reports the number.
func TestStatusWithoutMessagesIsAnError(t *testing.T) {
	fa := dialScripted(t, &scriptedIMAP{caps: "IMAP4rev1 UNSELECT", statusLine: "(UIDNEXT 5 UIDVALIDITY 7)"})
	if st, err := fa.Status(context.Background(), "Old"); err == nil {
		t.Fatalf("Status = %+v with no MESSAGES in the answer; want an error, not a zero", st)
	}
	fa = dialScripted(t, &scriptedIMAP{caps: "IMAP4rev1 UNSELECT", statusLine: "(MESSAGES 4 UIDNEXT 5 UIDVALIDITY 7)"})
	st, err := fa.Status(context.Background(), "Old")
	if err != nil || st.Messages != 4 || st.UIDNext != 5 || st.UIDValidity != 7 {
		t.Fatalf("Status = %+v, %v; want MESSAGES 4 UIDNEXT 5 UIDVALIDITY 7", st, err)
	}
}

// DELETE of the folder this connection has selected (it does, right after
// moving out of it) deselects first: Dovecot ends the whole session when the
// selected mailbox is deleted. With UNSELECT, UNSELECT; without it, EXAMINE
// INBOX — never CLOSE, which expunges.
func TestDeleteDeselectsTheSelectedFolderFirst(t *testing.T) {
	for _, tc := range []struct {
		caps, want string
	}{
		{"IMAP4rev1 UIDPLUS UNSELECT MOVE", "UNSELECT"},
		{"IMAP4rev1 UIDPLUS MOVE", "EXAMINE INBOX"},
	} {
		srv := &scriptedIMAP{caps: tc.caps, statusLine: "(MESSAGES 0 UIDNEXT 5 UIDVALIDITY 7)"}
		fa := dialScripted(t, srv)
		if err := fa.MoveAll(context.Background(), "Src", "Dst"); err != nil {
			t.Fatal(err)
		}
		if err := fa.DeleteMailbox(context.Background(), "Src"); err != nil {
			t.Fatal(err)
		}
		cmds := srv.commands()
		var order []string
		for _, c := range cmds {
			u := strings.ToUpper(c)
			switch {
			case strings.HasPrefix(u, "UNSELECT"), strings.HasPrefix(u, "EXAMINE"), strings.HasPrefix(u, "DELETE"),
				strings.HasPrefix(u, "CLOSE"):
				order = append(order, strings.Trim(c, " "))
			}
		}
		if len(order) != 2 || !strings.HasPrefix(strings.ToUpper(order[0]), tc.want) || !strings.HasPrefix(strings.ToUpper(order[1]), "DELETE") {
			t.Errorf("caps %q: deselect/delete sequence = %v, want %s then DELETE", tc.caps, order, tc.want)
		}
	}
}
