// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Package consolidate_test drives consolidation end to end against a real
// Dovecot (scripts/sync-test.sh): the plan, the moves, the deletions, and the
// re-run, through sync.DialFolderAdmin — the connection the command uses.
//
// Every test mints its own IMAP username. The harness's static passdb accepts
// any name and mail_home=/srv/mail/%Lu gives each its own mailboxes, including
// the auto-created SPECIAL-USE Sent/Drafts/Trash/Junk/Archive, so a test here
// shares nothing with internal/sync's or internal/census's tests or with a
// rerun of itself.
//
// Without the harness (MAILROOM_TEST_IMAP_HOST unset) every test skips.
package consolidate_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/consolidate"
	"mailroom/internal/sync"
)

type liveEnv struct {
	host string
	port int
	user string
	pass string
}

func newLiveEnv(t *testing.T) liveEnv {
	t.Helper()
	host := os.Getenv("MAILROOM_TEST_IMAP_HOST")
	if host == "" {
		t.Skip("MAILROOM_TEST_IMAP_HOST not set; run via scripts/sync-test.sh")
	}
	port, err := strconv.Atoi(os.Getenv("MAILROOM_TEST_IMAP_PORT"))
	if err != nil {
		t.Fatalf("MAILROOM_TEST_IMAP_PORT: %v", err)
	}
	pass := os.Getenv("MAILROOM_TEST_IMAP_PASS")
	if pass == "" {
		t.Fatal("MAILROOM_TEST_IMAP_PASS not set")
	}
	return liveEnv{host: host, port: port, pass: pass,
		user: "consolidate-" + strings.ToLower(ulid.Make().String())}
}

// raw is an independent go-imap client: the fixture builder and the witness.
// It never goes through the code under test.
func (e liveEnv) raw(t *testing.T) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialInsecure(net.JoinHostPort(e.host, strconv.Itoa(e.port)), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Login(e.user, e.pass).Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	t.Cleanup(func() { _ = c.Logout().Wait(); _ = c.Close() })
	return c
}

func (e liveEnv) admin(t *testing.T) sync.FolderAdmin {
	t.Helper()
	fa, err := sync.DialFolderAdmin(context.Background(), sync.Config{
		Host: e.host, Port: e.port, Username: e.user, Password: e.pass, TLS: sync.TLSInsecure,
		// Dovecot is on a private Docker network, which the #625 guard refuses by
		// default: this is the harness's MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS.
		AllowPrivateHosts: true,
	})
	if err != nil {
		t.Fatalf("DialFolderAdmin: %v", err)
	}
	t.Cleanup(func() { _ = fa.Close() })
	return fa
}

func create(t *testing.T, c *imapclient.Client, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := c.Create(n, nil).Wait(); err != nil {
			t.Fatalf("create %q: %v", n, err)
		}
	}
}

func put(t *testing.T, c *imapclient.Client, mailbox string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		raw := []byte(fmt.Sprintf("From: sender@example.com\r\nTo: you@example.com\r\n"+
			"Subject: %s %d\r\nMessage-ID: <%s-%d-%s@example.com>\r\nDate: Mon, 6 Jan 2025 10:00:00 +0000\r\n\r\nbody\r\n",
			mailbox, i, strings.ReplaceAll(mailbox, "/", "."), i, ulid.Make()))
		cmd := c.Append(mailbox, int64(len(raw)), nil)
		if _, err := cmd.Write(raw); err != nil {
			t.Fatalf("append %q: %v", mailbox, err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatalf("append %q: %v", mailbox, err)
		}
		if _, err := cmd.Wait(); err != nil {
			t.Fatalf("append %q: %v", mailbox, err)
		}
	}
}

// count is the WITNESS: STATUS MESSAGES from the independent raw client.
func count(t *testing.T, c *imapclient.Client, mailbox string) uint32 {
	t.Helper()
	d, err := c.Status(mailbox, &imap.StatusOptions{NumMessages: true}).Wait()
	if err != nil {
		t.Fatalf("status %q: %v", mailbox, err)
	}
	return *d.NumMessages
}

// tree is LIST "" "*" from the raw client: name -> attributes.
func tree(t *testing.T, c *imapclient.Client) map[string]string {
	t.Helper()
	data, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := map[string]string{}
	for _, d := range data {
		var attrs []string
		for _, a := range d.Attrs {
			attrs = append(attrs, string(a))
		}
		sort.Strings(attrs)
		out[d.Mailbox] = strings.Join(attrs, " ")
	}
	return out
}

func run(t *testing.T, fa sync.FolderAdmin, o consolidate.Options, apply bool) (*consolidate.Plan, string) {
	t.Helper()
	p, err := consolidate.BuildPlan(context.Background(), fa, nil, o)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	var out bytes.Buffer
	if err := consolidate.WriteReport(&out, p); err != nil {
		t.Fatal(err)
	}
	if apply {
		out.WriteString("\nAPPLYING\n")
		if _, err := consolidate.Apply(context.Background(), fa, p, &out); err != nil {
			t.Fatalf("Apply: %v\n%s", err, out.String())
		}
	}
	return p, out.String()
}

// #647 END TO END on a real server, in the order the operator will run it.
func TestLiveConsolidateTheIssueMailbox(t *testing.T) {
	e := newLiveEnv(t)
	c := e.raw(t)
	create(t, c, "INBOX/a", "INBOX/b", "INBOX/c", "Archive/2025/01", "Old", "Trash/x")
	put(t, c, "INBOX", 1)
	put(t, c, "INBOX/a", 3)
	put(t, c, "INBOX/b", 2)
	put(t, c, "Archive/2025/01", 2)
	put(t, c, "Sent", 1)
	put(t, c, "Trash", 1)

	treeBefore := tree(t, c)
	t.Logf("tree before: %v", treeBefore)
	special := map[string]uint32{}
	for _, mb := range []string{"Sent", "Drafts", "Trash", "Junk"} {
		special[mb] = count(t, c, mb)
	}
	inbox0, archive0 := count(t, c, "INBOX"), count(t, c, "Archive")

	fa := e.admin(t)
	inboxRun := consolidate.Options{From: "INBOX/*", Into: "INBOX", DeleteEmpty: []string{"INBOX/*", "Old", "Trash/*"}}
	archiveRun := consolidate.Options{From: "Archive/*", Into: "Archive", DeleteEmpty: []string{"Archive/*"}}

	// The report alone changes nothing.
	_, report := run(t, fa, inboxRun, false)
	t.Logf("REPORT (inbox run, before --apply):\n%s", report)
	if got := count(t, c, "INBOX/a"); got != 3 || count(t, c, "INBOX") != inbox0 {
		t.Fatalf("the REPORT moved mail: INBOX/a now %d", got)
	}

	_, out := run(t, fa, inboxRun, true)
	t.Logf("APPLY (inbox run):\n%s", out)
	_, out = run(t, fa, archiveRun, true)
	t.Logf("APPLY (archive run):\n%s", out)

	if got := count(t, c, "INBOX"); got != inbox0+5 {
		t.Errorf("INBOX = %d, want %d + 5", got, inbox0)
	}
	if got := count(t, c, "Archive"); got != archive0+2 {
		t.Errorf("Archive = %d, want %d + 2", got, archive0)
	}
	after := tree(t, c)
	for _, gone := range []string{"INBOX/a", "INBOX/b", "INBOX/c", "Archive/2025/01", "Archive/2025", "Old", "Trash/x"} {
		if _, ok := after[gone]; ok {
			t.Errorf("%s still exists after consolidation: %v", gone, after)
		}
	}
	for mb, n := range special {
		if got := count(t, c, mb); got != n {
			t.Errorf("special-use %s changed: %d -> %d", mb, n, got)
		}
		if role(after[mb]) == "" || role(after[mb]) != role(treeBefore[mb]) {
			t.Errorf("special-use %s role changed: %q -> %q", mb, treeBefore[mb], after[mb])
		}
	}
	for _, mb := range []string{"INBOX", "Archive"} {
		if _, ok := after[mb]; !ok {
			t.Errorf("%s is gone", mb)
		}
	}

	// A second run of each has nothing to do.
	for _, o := range []consolidate.Options{inboxRun, archiveRun} {
		p, report := run(t, fa, o, false)
		tot := p.Totals()
		if n := tot.MoveFolders + tot.DeleteNow + tot.DeleteAfterMove; n != 0 || !strings.Contains(report, "Nothing to do") {
			t.Errorf("second run of --from %q plans %d operations:\n%s", o.From, n, report)
		}
	}
}

// The refusals hold against a real server's SPECIAL-USE attributes: Sent is
// not emptied, empty Drafts/Junk are not deleted, INBOX is neither.
func TestLiveSpecialUseAndInboxAreRefused(t *testing.T) {
	e := newLiveEnv(t)
	c := e.raw(t)
	put(t, c, "Sent", 2)
	put(t, c, "INBOX", 1)
	fa := e.admin(t)
	p, out := run(t, fa, consolidate.Options{From: "Sent", Into: "INBOX",
		DeleteEmpty: []string{"Drafts", "Junk", "INBOX"}}, true)
	t.Logf("%s", out)
	for _, f := range append(append([]*consolidate.Folder(nil), p.Moves...), p.Deletes...) {
		if f.Action != consolidate.ActionRefuse {
			t.Errorf("%s planned as %q, want REFUSE", f.Name, f.Action)
		}
	}
	if len(p.Moves) != 1 || len(p.Deletes) != 3 {
		t.Fatalf("the patterns matched %d sources and %d targets, want 1 and 3 — the refusals were not exercised", len(p.Moves), len(p.Deletes))
	}
	if count(t, c, "Sent") != 2 || count(t, c, "INBOX") != 1 {
		t.Error("a refused folder's mail moved")
	}
	after := tree(t, c)
	for _, mb := range []string{"Drafts", "Junk", "INBOX"} {
		if _, ok := after[mb]; !ok {
			t.Errorf("refused deletion target %s was deleted", mb)
		}
	}
}

// role keeps only the SPECIAL-USE attributes of a tree entry: \HasChildren
// legitimately flips when a child folder (Trash/x) is deleted.
func role(attrs string) string {
	var keep []string
	for _, a := range strings.Fields(attrs) {
		switch a {
		case `\Sent`, `\Drafts`, `\Trash`, `\Junk`, `\Archive`, `\All`, `\Flagged`:
			keep = append(keep, a)
		}
	}
	return strings.Join(keep, " ")
}
