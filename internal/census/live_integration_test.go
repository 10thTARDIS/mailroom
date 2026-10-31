// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Package census_test exercises census.Dial/Run against a real Dovecot IMAP
// server (scripts/sync-test.sh), the same harness internal/sync's own live
// tests use. It is a separate (external) test package so these tests can
// only reach mailroom/internal/census through its exported API — the same
// surface a real caller (cmd/mailroom-census) is limited to.
//
// Run via scripts/sync-test.sh. Run directly
// (`go test -tags integration ./internal/census/...`) without that harness,
// every test here skips: MAILROOM_TEST_IMAP_HOST is absent.
//
// The harness's passdb accepts ANY username with MAILROOM_TEST_IMAP_PASS,
// and mail_home=/srv/mail/%Lu (dovecot.conf) gives each username its own
// mailboxes — including the auto-created SPECIAL-USE Sent/Drafts/Trash/
// Junk/Archive (namespace inbox { mailbox ... { special_use = ...; auto =
// subscribe } } blocks). So every test here mints its OWN unique username
// (census-<purpose>-<ulid>) rather than sharing the sync package's
// "sync-tester": that isolates it both from the sync tests and from
// reruns, and gives the connection-cap probe a clean per-user counter to
// measure.
package census_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/census"
	"mailroom/internal/mime"
)

// censusMailboxes is every mailbox the fixtures below populate, in the
// order both the setup and the snapshot walk them.
var censusMailboxes = []string{"INBOX", "Sent", "Drafts", "Trash", "Junk", "Archive"}

// requireEnv reads the harness's connection details and skips (missing
// HOST) or fails (present but malformed) accordingly — the same convention
// internal/sync/live_integration_test.go uses.
func requireEnv(t *testing.T) (host string, port int, pass string) {
	t.Helper()
	host = os.Getenv("MAILROOM_TEST_IMAP_HOST")
	if host == "" {
		t.Skip("MAILROOM_TEST_IMAP_HOST not set; run via scripts/sync-test.sh")
	}
	portStr := os.Getenv("MAILROOM_TEST_IMAP_PORT")
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("invalid MAILROOM_TEST_IMAP_PORT %q: %v", portStr, err)
	}
	pass = os.Getenv("MAILROOM_TEST_IMAP_PASS")
	if pass == "" {
		t.Fatalf("MAILROOM_TEST_IMAP_PASS not set")
	}
	return host, p, pass
}

// uniqueUsername mints a per-test-run username, so this test never shares
// mailboxes with another run of itself or with the sync package's own
// fixed "sync-tester" account.
func uniqueUsername(purpose string) string {
	return fmt.Sprintf("census-%s-%s", purpose, strings.ToLower(ulid.Make().String()))
}

// dialPlain opens a raw imapclient session (test code only — production
// code never imports go-imap outside internal/census/imap.go). Used for
// fixture setup and for the independent snapshot session: neither should
// go through census.Reader, since Reader has no write method at all and the
// snapshot needs to be provably independent of the code under test.
func dialPlain(t *testing.T, host string, port int, user, pass string) *imapclient.Client {
	t.Helper()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("DialInsecure: %v", err)
	}
	if err := c.Login(user, pass).Wait(); err != nil {
		_ = c.Close()
		t.Fatalf("Login: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Logout().Wait()
		_ = c.Close()
	})
	return c
}

// appendMessage APPENDs raw to mailbox with the given flags/internal date
// and returns its assigned UID, failing the test if the server doesn't
// return one via APPENDUID (this harness asserts Dovecot's default UIDPLUS
// support, exactly like internal/sync's own live tests do).
func appendMessage(t *testing.T, c *imapclient.Client, mailbox string, flags []imap.Flag, when time.Time, raw []byte) uint32 {
	t.Helper()
	cmd := c.Append(mailbox, int64(len(raw)), &imap.AppendOptions{Flags: flags, Time: when})
	if _, err := cmd.Write(raw); err != nil {
		_ = cmd.Close()
		t.Fatalf("Append(%s) write: %v", mailbox, err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("Append(%s) close: %v", mailbox, err)
	}
	data, err := cmd.Wait()
	if err != nil {
		t.Fatalf("Append(%s) wait: %v", mailbox, err)
	}
	if data.UID == 0 {
		t.Fatalf("Append(%s): server did not return an APPENDUID; harness assumes UIDPLUS support", mailbox)
	}
	return uint32(data.UID)
}

// syntheticMessage builds a minimal RFC 5322 message with a unique
// Message-ID, for the Drafts/Archive fixtures that don't need to come from
// the MIME corpus.
func syntheticMessage(subject string) []byte {
	msgID := fmt.Sprintf("<census-%d@example.com>", time.Now().UnixNano())
	return []byte(fmt.Sprintf(
		"From: Census Tester <census-tester@example.com>\r\n"+
			"To: Census Tester <census-tester@example.com>\r\n"+
			"Subject: %s\r\n"+
			"Date: Mon, 01 Jan 2024 10:00:00 +0000\r\n"+
			"Message-ID: %s\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"\r\n"+
			"Body for %s.\r\n", subject, msgID, subject))
}

// fixture is one corpus message the test seeded into INBOX, remembered so
// the same-definition assertion can find it again by UID.
type fixture struct {
	File string
	Raw  []byte
	UID  uint32
	Year int
}

// seedCorpusIntoInbox APPENDs every internal/mime testdata corpus fixture
// into INBOX, with INTERNALDATEs spread across 2024/2025/2026 (cycling
// through the corpus in file order, which filepath.Glob returns sorted, so
// the year assignment is deterministic) and alternating \Seen/unseen.
func seedCorpusIntoInbox(t *testing.T, c *imapclient.Client) []fixture {
	t.Helper()
	corpusDir := filepath.Join("..", "mime", "testdata", "corpus")
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.eml"))
	if err != nil {
		t.Fatalf("glob corpus: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no fixtures found under %s", corpusDir)
	}

	years := []int{2024, 2025, 2026}
	out := make([]fixture, 0, len(files))
	for i, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading fixture %s: %v", f, err)
		}
		year := years[i%len(years)]
		when := time.Date(year, 6, 15, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)
		var flags []imap.Flag
		if i%2 == 0 {
			flags = []imap.Flag{imap.FlagSeen}
		}
		uid := appendMessage(t, c, "INBOX", flags, when, raw)
		out = append(out, fixture{File: filepath.Base(f), Raw: raw, UID: uid, Year: year})
	}
	return out
}

// statSnap is the STATUS facts the snapshot cares about, with an explicit
// Has* flag per optional field rather than a pointer, so reflect.DeepEqual
// between two snapshots is exact.
type statSnap struct {
	Messages      uint32
	HasMessages   bool
	UIDNext       uint32
	UIDValidity   uint32
	Unseen        uint32
	HasUnseen     bool
	HighestModSeq uint64
}

type msgSnap struct {
	UID   uint32
	Flags []string // sorted
	Size  int64    // RFC822.SIZE
}

type mailboxSnap struct {
	Name     string
	Status   statSnap
	Messages []msgSnap // sorted by UID
}

// snapshotAll is the read-only acceptance test's ground truth: STATUS plus
// a read-only SELECT and FETCH of UID+FLAGS+RFC822.SIZE for every message
// in every census mailbox, taken through a session that never runs
// census.Reader code. Called twice — immediately before and immediately
// after a census.Run — its two results must be byte-identical, or the
// census mutated something it examined.
func snapshotAll(t *testing.T, c *imapclient.Client, condstore bool) []mailboxSnap {
	t.Helper()
	out := make([]mailboxSnap, 0, len(censusMailboxes))
	for _, name := range censusMailboxes {
		out = append(out, snapshotOne(t, c, name, condstore))
	}
	return out
}

func snapshotOne(t *testing.T, c *imapclient.Client, name string, condstore bool) mailboxSnap {
	t.Helper()
	sd, err := c.Status(name, &imap.StatusOptions{
		NumMessages:   true,
		UIDNext:       true,
		UIDValidity:   true,
		NumUnseen:     true,
		HighestModSeq: condstore,
	}).Wait()
	if err != nil {
		t.Fatalf("STATUS(%s): %v", name, err)
	}
	snap := mailboxSnap{Name: name}
	if sd.NumMessages != nil {
		snap.Status.Messages = *sd.NumMessages
		snap.Status.HasMessages = true
	}
	snap.Status.UIDNext = uint32(sd.UIDNext)
	snap.Status.UIDValidity = sd.UIDValidity
	if sd.NumUnseen != nil {
		snap.Status.Unseen = *sd.NumUnseen
		snap.Status.HasUnseen = true
	}
	snap.Status.HighestModSeq = sd.HighestModSeq

	selData, err := c.Select(name, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatalf("SELECT(%s) read-only: %v", name, err)
	}
	if selData.NumMessages == 0 {
		return snap
	}

	var seq imap.SeqSet
	seq.AddRange(1, 0) // "1:*"
	fetchCmd := c.Fetch(seq, &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true})
	for {
		md := fetchCmd.Next()
		if md == nil {
			break
		}
		buf, err := md.Collect()
		if err != nil {
			_ = fetchCmd.Close()
			t.Fatalf("FETCH(%s): %v", name, err)
		}
		// ⚠ KEEP \Recent IN THIS SNAPSHOT. It looks like session noise, and it is
		// the ONLY thing here that catches Examine regressing to a read-write
		// SELECT on its own: measured 2026-09-14 against this harness, a SELECT
		// with the census's metadata-only fetch left UNSEEN and HIGHESTMODSEQ
		// untouched, and the test went red solely because SELECT claimed \Recent.
		// (SELECT plus a non-peek BODY[] went red on \Seen as well; EXAMINE plus
		// the same BODY[] stayed green, which is the server-side layer working.)
		flags := make([]string, len(buf.Flags))
		for i, f := range buf.Flags {
			flags[i] = string(f)
		}
		sort.Strings(flags)
		snap.Messages = append(snap.Messages, msgSnap{UID: uint32(buf.UID), Flags: flags, Size: buf.RFC822Size})
	}
	if err := fetchCmd.Close(); err != nil {
		t.Fatalf("FETCH(%s) close: %v", name, err)
	}
	sort.Slice(snap.Messages, func(i, j int) bool { return snap.Messages[i].UID < snap.Messages[j].UID })
	return snap
}

func sumSizes(snap mailboxSnap) int64 {
	var total int64
	for _, m := range snap.Messages {
		total += m.Size
	}
	return total
}

// knownAttachmentVerdictDivergences lists corpus fixtures where Dovecot's
// own BODYSTRUCTURE and internal/mime's parse of the SAME bytes are
// expected to disagree about whether the message has an attachment, with
// the measured reason. Populated only if TestLiveCensusReadOnlyAndAccurate
// actually found one — see that test's same-definition assertion. Empty
// means none were found: every fixture agreed.
var knownAttachmentVerdictDivergences = map[string]string{}

func TestLiveCensusReadOnlyAndAccurate(t *testing.T) {
	host, port, pass := requireEnv(t)
	user := uniqueUsername("accuracy")

	setupConn := dialPlain(t, host, port, user, pass)
	fixtures := seedCorpusIntoInbox(t, setupConn)
	// Drafts/Archive are selectable, non-virtual mailboxes too, so their
	// messages count in ByYear right alongside INBOX's — one fixed "now"
	// keeps their contribution to wantYearCounts below exact rather than
	// risking a UTC-year rollover between two separate time.Now() calls.
	synthNow := time.Now().UTC()
	draftRaw := syntheticMessage("Census draft fixture")
	draftUID := appendMessage(t, setupConn, "Drafts", []imap.Flag{imap.FlagDraft}, synthNow, draftRaw)
	var archiveUIDs []uint32
	for i := 0; i < 2; i++ {
		raw := syntheticMessage(fmt.Sprintf("Census archive fixture %d", i))
		archiveUIDs = append(archiveUIDs, appendMessage(t, setupConn, "Archive", nil, synthNow, raw))
	}
	t.Logf("seeded %d INBOX fixtures, 1 Drafts (UID %d), %d Archive (UIDs %v)",
		len(fixtures), draftUID, len(archiveUIDs), archiveUIDs)

	condstore := setupConn.Caps().Has(imap.CapCondStore)

	snapConn := dialPlain(t, host, port, user, pass)
	before := snapshotAll(t, snapConn, condstore)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := census.Config{Host: host, Port: port, TLS: census.TLSInsecure, Username: user, Password: pass}
	r, err := census.Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("census.Dial() error = %v", err)
	}

	rep, err := census.Run(ctx, r, time.Now())
	if err != nil {
		_ = r.Close()
		t.Fatalf("census.Run() error = %v", err)
	}

	// The same-definition assertion: census's per-message attachment
	// verdict, from the SAME connection Run just used, against
	// internal/mime's parse of the raw bytes each fixture was seeded with.
	verdicts, err := census.ExamineVerdicts(ctx, r, "INBOX")
	if err != nil {
		_ = r.Close()
		t.Fatalf("census.ExamineVerdicts(INBOX) error = %v", err)
	}

	// On the same connection, BEFORE the "after" snapshot, so the read-only
	// assertion below covers ListMessages too.
	var rows []census.MessageRow
	if err := census.ListMessages(ctx, r, func(row census.MessageRow) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		_ = r.Close()
		t.Fatalf("census.ListMessages() error = %v", err)
	}

	if err := r.Close(); err != nil {
		t.Errorf("Reader.Close() error = %v", err)
	}

	after := snapshotAll(t, snapConn, condstore)

	// The real rendering, against a real Dovecot fixture — logged rather
	// than asserted on: WriteText's exact wording is reviewed by reading it,
	// not by string-matching it in every future change.
	var rendered bytes.Buffer
	if err := census.WriteText(&rendered, rep); err != nil {
		t.Fatalf("census.WriteText() error = %v", err)
	}
	t.Logf("census.WriteText() output:\n%s", rendered.String())

	// --- THE READ-ONLY ACCEPTANCE TEST --------------------------------------
	if !reflect.DeepEqual(before, after) {
		t.Errorf("mailbox state changed across a census.Dial+Run+ExamineVerdicts+Close run "+
			"(want byte-identical snapshots):\nbefore = %+v\nafter  = %+v", before, after)
	}

	assertListMessagesMatchesSnapshot(t, rows, before, rep)

	// --- Report numbers, cross-checked against the independent snapshot -----
	byName := map[string]mailboxSnap{}
	for _, s := range before {
		byName[s.Name] = s
	}
	repByName := map[string]census.MailboxReport{}
	for _, mb := range rep.Mailboxes {
		repByName[mb.Name] = mb
	}

	for _, name := range censusMailboxes {
		snap := byName[name]
		mb, ok := repByName[name]
		if !ok {
			t.Errorf("Report.Mailboxes has no row for %q", name)
			continue
		}
		if mb.Error != "" {
			t.Errorf("Mailboxes[%q].Error = %q, want none", name, mb.Error)
			continue
		}
		wantMessages := len(snap.Messages)
		if mb.Messages != wantMessages {
			t.Errorf("Mailboxes[%q].Messages = %d, want %d (from the independent snapshot)", name, mb.Messages, wantMessages)
		}
		if wantBytes := sumSizes(snap); mb.Bytes != wantBytes {
			t.Errorf("Mailboxes[%q].Bytes = %d, want %d (sum of RFC822.SIZE from the independent snapshot)",
				name, mb.Bytes, wantBytes)
		}
	}

	if got := repByName["Drafts"].Drafts; got != 1 {
		t.Errorf("Mailboxes[Drafts].Drafts = %d, want 1", got)
	}
	if got := repByName["Drafts"].Messages; got != 1 {
		t.Errorf("Mailboxes[Drafts].Messages = %d, want 1", got)
	}

	// --- ByYear matches the INTERNALDATEs the fixtures were seeded with -----
	// ByYear is over every selectable, non-virtual mailbox (not just
	// INBOX), so Drafts' 1 message and Archive's 2 both land in
	// synthNow.Year() alongside whichever INBOX fixtures share that year.
	wantYearCounts := map[int]int{}
	for _, f := range fixtures {
		wantYearCounts[f.Year]++
	}
	wantYearCounts[synthNow.Year()] += 1 + len(archiveUIDs)
	gotYearCounts := map[int]int{}
	for _, y := range rep.ByYear {
		gotYearCounts[y.Year] = y.Messages
	}
	for year, want := range wantYearCounts {
		if got := gotYearCounts[year]; got != want {
			t.Errorf("ByYear[%d].Messages = %d, want %d", year, got, want)
		}
	}

	// --- Roles: Dovecot auto-created Sent/Drafts/Trash/Junk/Archive with
	// SPECIAL-USE (dovecot.conf), so every one of them must be found. -------
	for _, role := range []string{"sent", "drafts", "trash", "junk", "archive"} {
		names := rep.Roles[role]
		if len(names) != 1 {
			t.Errorf("Roles[%q] = %v, want exactly one mailbox", role, names)
		}
	}

	// --- The same-definition assertion --------------------------------------
	var mismatches []string
	for _, f := range fixtures {
		gotAttachment, ok := verdicts[f.UID]
		if !ok {
			t.Errorf("ExamineVerdicts(INBOX) has no verdict for %s (UID %d)", f.File, f.UID)
			continue
		}
		parsed, _ := mime.ParseBytes(f.Raw) // ParseBytes never returns a nil Message
		wantAttachment := len(parsed.Attachments) > 0

		if reason, known := knownAttachmentVerdictDivergences[f.File]; known {
			t.Logf("%s: KNOWN divergence, not asserted: %s (census=%v mime=%v)",
				f.File, reason, gotAttachment, wantAttachment)
			continue
		}
		if gotAttachment != wantAttachment {
			mismatches = append(mismatches, fmt.Sprintf(
				"%s (UID %d): census.HasAttachment (from Dovecot's BODYSTRUCTURE) = %v, "+
					"len(mime.ParseBytes(...).Attachments) > 0 = %v",
				f.File, f.UID, gotAttachment, wantAttachment))
		}
	}
	if len(mismatches) > 0 {
		t.Errorf("attachment verdict disagreement between census and internal/mime for %d fixture(s):\n  %s",
			len(mismatches), strings.Join(mismatches, "\n  "))
	}
}

// TestLiveCensusProbeConnectionsAtCap proves ProbeConnections observes a
// REAL refusal against a real per-account connection limit: Dovecot's
// default mail_max_userip_connections is 10 for imap, and this harness's
// dovecot.conf does not override it (verified by reading the file — see
// scripts/sync-test.sh). If that ever changes, this test's own failure
// message is the measurement to go read, not a guess to correct in place.
func TestLiveCensusProbeConnectionsAtCap(t *testing.T) {
	host, port, pass := requireEnv(t)
	cfg := census.Config{Host: host, Port: port, TLS: census.TLSInsecure, Username: uniqueUsername("probe-cap"), Password: pass}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res := census.ProbeConnections(ctx, census.DialProbe(cfg), 15, 100*time.Millisecond)
	if !res.Refused {
		t.Fatalf("ProbeConnections(max=15) Refused = false, want true (Dovecot's default "+
			"mail_max_userip_connections is 10, and this harness does not override it): %+v", res)
	}
	if res.Held != 10 {
		t.Errorf("ProbeConnections(max=15) Held = %d, want 10 — if the harness's configured "+
			"cap genuinely differs from Dovecot's default, this is the number to change to "+
			"match it, not to relax: %+v", res.Held, res)
	}
}

// TestLiveCensusProbeConnectionsUnderCap proves the non-refusal path
// against a real server: asking for fewer connections than the cap allows
// must hold exactly that many and report no refusal.
func TestLiveCensusProbeConnectionsUnderCap(t *testing.T) {
	host, port, pass := requireEnv(t)
	cfg := census.Config{Host: host, Port: port, TLS: census.TLSInsecure, Username: uniqueUsername("probe-under"), Password: pass}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res := census.ProbeConnections(ctx, census.DialProbe(cfg), 3, 100*time.Millisecond)
	if res.Refused {
		t.Errorf("ProbeConnections(max=3) Refused = true, want false: %+v", res)
	}
	if res.Held != 3 {
		t.Errorf("ProbeConnections(max=3) Held = %d, want 3", res.Held)
	}
}

// assertListMessagesMatchesSnapshot checks the -messages listing row by row
// against the independent snapshot session: every server message appears
// exactly once with the snapshot's UIDVALIDITY and RFC822.SIZE, and the rows
// sum to the report's total. A per-message listing that disagreed with its own
// report would make any join against it meaningless.
func assertListMessagesMatchesSnapshot(t *testing.T, rows []census.MessageRow, snaps []mailboxSnap, rep *census.Report) {
	t.Helper()
	type key struct {
		mailbox string
		uid     uint32
	}
	want := map[key]int64{}
	uidValidity := map[string]uint32{}
	for _, s := range snaps {
		uidValidity[s.Name] = s.Status.UIDValidity
		for _, m := range s.Messages {
			want[key{s.Name, m.UID}] = m.Size
		}
	}
	seen := map[key]bool{}
	var total int64
	for _, row := range rows {
		k := key{row.Mailbox, row.UID}
		if seen[k] {
			t.Errorf("ListMessages emitted %s UID %d twice", row.Mailbox, row.UID)
		}
		seen[k] = true
		total += row.Size
		size, ok := want[k]
		if !ok {
			t.Errorf("ListMessages emitted %s UID %d, which the snapshot does not have", row.Mailbox, row.UID)
			continue
		}
		if row.Size != size {
			t.Errorf("ListMessages %s UID %d size = %d, snapshot RFC822.SIZE = %d", row.Mailbox, row.UID, row.Size, size)
		}
		if row.UIDValidity != uidValidity[row.Mailbox] {
			t.Errorf("ListMessages %s UIDVALIDITY = %d, snapshot STATUS = %d", row.Mailbox, row.UIDValidity, uidValidity[row.Mailbox])
		}
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("ListMessages omitted %s UID %d", k.mailbox, k.uid)
		}
	}
	if total != rep.Totals.Bytes {
		t.Errorf("ListMessages rows sum to %d bytes, report Totals.Bytes = %d", total, rep.Totals.Bytes)
	}
}
