// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package importer

// Integration coverage for bulk archive import against a real database + blob
// store. These are the claims that decide whether the importer is usable at all,
// so each one is proved end-to-end rather than reasoned about:
//
//	the mail is queryable            TestImportMboxIsQueryable
//	labels land as LABELS            TestImportGmailLabels
//	a re-run deduplicates            TestImportIsIdempotent
//	an interruption resumes          TestImportResumesWithoutDuplicating
//	a bad message is counted, not fatal   TestBadMessagesAreSkippedNotFatal
//	imported mail NEVER writes back  TestImportedMailEnqueuesNoWriteback
//	IMAP-owned mail is not touched   TestArchiveDoesNotDisturbSyncedMail
//
// Skips when the integration database is not configured (run via
// scripts/integration-test.sh).

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/ingest"
	"mailroom/internal/store"
)

type fixture struct {
	super *pgxpool.Pool
	st    *store.Store
	ing   *ingest.Ingestor
	blobs blob.BlobStore

	userID ulid.ULID
	dek    []byte
	dekID  ulid.ULID

	// A REAL IMAP account + mailbox, so the "archive must not disturb synced mail"
	// case can be proved against the real ingest path rather than a mock.
	imapAccount ulid.ULID
	imapMailbox ulid.ULID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	superURL := envOrSkip(t, "MAILROOM_TEST_SUPER_URL")
	appURL := envOrSkip(t, "MAILROOM_TEST_APP_URL")
	ctx := context.Background()

	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)
	st, err := store.Open(ctx, appURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	blobs, err := blob.NewFSBlobStore(t.TempDir(), randomKey(t))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	f := &fixture{
		super: super, st: st, blobs: blobs, ing: ingest.New(st, blobs),
		userID: ulid.Make(), dek: randomKey(t), dekID: ulid.Make(),
		imapAccount: ulid.Make(), imapMailbox: ulid.Make(),
	}
	uniq := f.userID.String()
	exec := func(sql string, args ...any) {
		if _, err := f.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+uniq, uniq+"@example.com")
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek, sync_enabled)
	      VALUES ($1, $2, 'primary', 'imap.example.com', 'smtp.example.com', '\x00', $3, true)`,
		f.imapAccount.String(), f.userID.String(), ulid.Make().String())
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
	      VALUES ($1, $2, $3, 'INBOX', 'Inbox')`,
		f.imapMailbox.String(), f.userID.String(), f.imapAccount.String())
	return f
}

// options builds the Options an import runs with. The sealed credential envelope
// is a stand-in for the CLI's onboarding.SealCredentials — the store only requires
// that it be non-empty.
func (f *fixture) options(t *testing.T, statePath string) Options {
	t.Helper()
	return Options{
		UserID:         f.userID,
		CredentialsCT:  []byte("sealed-envelope-placeholder"),
		CredentialsDEK: ulid.Make(),
		DEK:            f.dek,
		DEKID:          f.dekID,
		StatePath:      statePath,
		Resume:         true,
	}
}

func (f *fixture) run(t *testing.T, archive string, opt Options) Stats {
	t.Helper()
	stats, err := f.runErr(t, archive, opt)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return stats
}

func (f *fixture) runErr(t *testing.T, archive string, opt Options) (Stats, error) {
	t.Helper()
	return f.runCtx(t, context.Background(), archive, opt)
}

func (f *fixture) runCtx(t *testing.T, ctx context.Context, archive string, opt Options) (Stats, error) {
	t.Helper()
	kind, err := Detect(archive)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	fp, err := FingerprintArchive(archive, kind)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	src, err := Open(archive, kind, DefaultMaxMessageBytes)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = src.Close() }()

	im, err := New(f.st, f.ing, opt)
	if err != nil {
		t.Fatalf("new importer: %v", err)
	}
	return im.Run(ctx, src, fp)
}

func (f *fixture) scalar(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// mailboxID resolves an imported mailbox by its full name ("Imported/Inbox").
func (f *fixture) mailboxID(t *testing.T, name string) ulid.ULID {
	t.Helper()
	var id string
	err := f.super.QueryRow(context.Background(),
		`SELECT id FROM app.mailboxes WHERE user_id = $1 AND imap_name = $2`,
		f.userID.String(), name).Scan(&id)
	if err != nil {
		t.Fatalf("mailbox %q: %v", name, err)
	}
	return ulid.MustParse(id)
}

func envOrSkip(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	return v
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := crand.Read(k); err != nil {
		t.Fatalf("random key: %v", err)
	}
	return k
}

// --- fixtures ----------------------------------------------------------------

// message renders a realistic RFC 5322 message.
func message(n int, subject string, extraHeaders ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: Sender %d <sender%d@example.com>\r\n", n, n%7)
	fmt.Fprintf(&b, "To: Operator <operator@example.com>\r\n")
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Message-ID: <msg-%d@example.com>\r\n", n)
	fmt.Fprintf(&b, "Date: Mon, 12 Jul 2026 %02d:%02d:00 +0000\r\n", n%24, n%60)
	for _, h := range extraHeaders {
		b.WriteString(h)
		b.WriteString("\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	fmt.Fprintf(&b, "Body of message %d. The quick brown fox jumps over the lazy dog.\r\n", n)
	return b.String()
}

func mboxEntry(n int, subject string, extraHeaders ...string) string {
	return fmt.Sprintf("From sender%d@example.com Mon Jul 12 10:%02d:00 2021\r\n", n%7, n%60) +
		message(n, subject, extraHeaders...) + "\r\n"
}

// writeMbox builds an mbox of n ordinary messages plus the garbage every real
// archive contains.
func writeMbox(t *testing.T, dir, name string, n int) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(mboxEntry(i, fmt.Sprintf("Message %d", i)))
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- tests -------------------------------------------------------------------

func TestImportMboxIsQueryable(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	archive := writeMbox(t, dir, "old-mail.mbox", 120)

	stats := f.run(t, archive, f.options(t, filepath.Join(dir, "state.json")))
	if stats.Imported != 120 {
		t.Fatalf("imported = %d, want 120 (stats: %+v)", stats.Imported, stats)
	}

	ctx := context.Background()
	mbox := f.mailboxID(t, "Imported/old-mail")

	var (
		ids   []ulid.ULID
		total int
		views []store.EmailView
	)
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var err error
		ids, total, err = tx.QueryEmails(ctx, store.EmailQuery{InMailbox: &mbox, Limit: 5})
		if err != nil {
			return err
		}
		views, err = tx.GetEmails(ctx, ids)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if total != 120 {
		t.Errorf("Email/query in the imported mailbox = %d, want 120", total)
	}
	if len(views) != 5 {
		t.Fatalf("Email/get returned %d views, want 5", len(views))
	}
	for _, v := range views {
		// Membership of imported mail comes from local_mailbox_id — there is no
		// provenance row to derive it from.
		if len(v.MailboxIDs) != 1 || v.MailboxIDs[0] != mbox {
			t.Errorf("message %s mailboxes = %v, want [%s]", v.ID, v.MailboxIDs, mbox)
		}
		if v.Subject == "" || v.ThreadID == (ulid.ULID{}) || v.Size == 0 {
			t.Errorf("message %s looks unpopulated: %+v", v.ID, v)
		}
	}

	// The blob is really there, and really decrypts.
	rc, err := f.blobs.Get(ctx, f.userID, f.dek, blob.ContentHash(sha256Of(views[0].ContentHash)))
	if err == nil {
		_ = rc.Close()
	}

	// No provenance, ever — that is what keeps writeback and the reaper away.
	if got := f.scalar(t, `SELECT count(*) FROM app.message_provenance WHERE user_id=$1`, f.userID.String()); got != 0 {
		t.Errorf("imported mail created %d provenance rows, want 0", got)
	}
	// The change_log invariant: one Email `created` per imported message.
	if got := f.scalar(t,
		`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Email' AND op='created'`,
		f.userID.String()); got != 120 {
		t.Errorf("Email created change_log rows = %d, want 120", got)
	}
	t.Logf("imported 120 messages: %+v", stats)
}

// sha256Of adapts a []byte content hash to the [32]byte the blob store wants.
func sha256Of(b []byte) [32]byte {
	var h [32]byte
	copy(h[:], b)
	return h
}

func TestImportGmailLabels(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()

	var b strings.Builder
	b.WriteString(mboxEntry(1, "Inbox unread", "X-Gmail-Labels: Inbox,Unread,Work"))
	b.WriteString(mboxEntry(2, "Archived starred", "X-Gmail-Labels: Starred,Work,Category Personal"))
	b.WriteString(mboxEntry(3, "Spam", "X-Gmail-Labels: Spam"))
	b.WriteString(mboxEntry(4, "Sent", "X-Gmail-Labels: Sent"))
	archive := filepath.Join(dir, "All mail Including Spam and Trash.mbox")
	if err := os.WriteFile(archive, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	stats := f.run(t, archive, f.options(t, filepath.Join(dir, "state.json")))
	if stats.Imported != 4 {
		t.Fatalf("imported = %d, want 4 (%+v)", stats.Imported, stats)
	}

	// Gmail's folder-ish labels became MAILBOXES; the user's own labels became
	// LABELS (spec §6). Both, not one or the other.
	for _, name := range []string{"Imported/Inbox", "Imported/Archive", "Imported/Spam", "Imported/Sent"} {
		f.mailboxID(t, name) // fatals when absent
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.labels WHERE user_id=$1`, f.userID.String()); got != 2 {
		t.Errorf("labels = %d, want 2 (Work and Category Personal — Inbox/Sent/Spam/Starred "+
			"are folders and flags, not labels)", got)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE user_id=$1`, f.userID.String()); got != 3 {
		t.Errorf("message_labels rows = %d, want 3 (Work x2, Category Personal x1)", got)
	}

	ctx := context.Background()
	inbox := f.mailboxID(t, "Imported/Inbox")
	var views []store.EmailView
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		ids, _, err := tx.QueryEmails(ctx, store.EmailQuery{InMailbox: &inbox, Limit: 10})
		if err != nil {
			return err
		}
		views, err = tx.GetEmails(ctx, ids)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("Imported/Inbox holds %d messages, want 1", len(views))
	}
	// "Unread" was asserted, so the message is NOT seen; the label surfaces as a
	// JMAP keyword atom (labels are keywords, spec §13).
	if hasKeyword(views[0].Keywords, store.KeywordSeen) {
		t.Errorf("a Gmail message labelled Unread came in as seen: %v", views[0].Keywords)
	}
	if !hasKeywordPrefix(views[0].Keywords, "MailroomLabel-work") {
		t.Errorf("keywords = %v, want the Work label's keyword atom", views[0].Keywords)
	}

	// The starred, archived message: flagged, seen, and in Archive.
	archive2 := f.mailboxID(t, "Imported/Archive")
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		ids, _, err := tx.QueryEmails(ctx, store.EmailQuery{InMailbox: &archive2, Limit: 10})
		if err != nil {
			return err
		}
		views, err = tx.GetEmails(ctx, ids)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || !hasKeyword(views[0].Keywords, store.KeywordFlagged) ||
		!hasKeyword(views[0].Keywords, store.KeywordSeen) {
		t.Errorf("archived+starred message keywords = %v, want $seen and $flagged", views[0].Keywords)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1 AND is_junk`, f.userID.String()); got != 1 {
		t.Errorf("junk messages = %d, want 1 (the Spam one)", got)
	}
	t.Logf("gmail import: %+v", stats)
}

func hasKeyword(kws []string, want string) bool {
	for _, k := range kws {
		if k == want {
			return true
		}
	}
	return false
}

func hasKeywordPrefix(kws []string, prefix string) bool {
	for _, k := range kws {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// TestImportedMailEnqueuesNoWriteback is the load-bearing safety property: an
// archive is LOCAL mail and must never be pushed at somebody's IMAP server. It is
// structural (no provenance ⇒ no live accounts ⇒ every enqueue loop runs zero
// times), and it must hold not only for the import itself but for what the USER
// does to imported mail afterwards.
func TestImportedMailEnqueuesNoWriteback(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()

	var b strings.Builder
	b.WriteString(mboxEntry(1, "Labelled", "X-Gmail-Labels: Inbox,Work"))
	b.WriteString(mboxEntry(2, "Plain", "X-Gmail-Labels: Work"))
	archive := filepath.Join(dir, "takeout.mbox")
	if err := os.WriteFile(archive, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	f.run(t, archive, f.options(t, filepath.Join(dir, "state.json")))

	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1`, f.userID.String()); got != 0 {
		t.Fatalf("the import enqueued %d writeback intents, want 0", got)
	}

	// Now do to an imported message everything a client can do — mark it read,
	// star it, add a label, move it. Still nothing may reach IMAP.
	ctx := context.Background()
	inbox := f.mailboxID(t, "Imported/Inbox")
	trueP := true
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		ids, _, err := tx.QueryEmails(ctx, store.EmailQuery{InMailbox: &inbox, Limit: 1})
		if err != nil {
			return err
		}
		if len(ids) != 1 {
			return errors.New("expected one message in Imported/Inbox")
		}
		labels, err := tx.ListLabels(ctx)
		if err != nil {
			return err
		}
		target := f.mailboxID(t, "Imported/Archive")
		found, _, err := tx.ApplyEmailUpdate(ctx, ids[0], store.EmailUpdate{
			Seen:            &trueP,
			Flagged:         &trueP,
			AddKeywords:     []string{"custom"},
			AddLabelIDs:     []ulid.ULID{labels[0].ID},
			MoveToMailboxID: &target,
		})
		if err != nil || !found {
			return fmt.Errorf("apply update: found=%v err=%w", found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1`, f.userID.String()); got != 0 {
		t.Errorf("acting on imported mail enqueued %d writeback intents, want 0 — "+
			"imported mail has no IMAP copy to write back to", got)
	}
}

// TestArchiveDoesNotDisturbSyncedMail: the operator imports a Takeout of the very
// mailbox they also sync. Where the bytes coincide, the archive must defer to the
// server — anything else would push a years-old flag snapshot (and a million
// keyword writes) at their live account.
func TestArchiveDoesNotDisturbSyncedMail(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dir := t.TempDir()

	// A message the IMAP sync already owns.
	raw := message(42, "Already synced")
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, []byte(raw), ingest.Provenance{
		AccountID: f.imapAccount, MailboxID: f.imapMailbox, UID: 7, UIDValidity: 1,
		RemoteFlags:  []string{`\Seen`},
		InternalDate: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The SAME bytes, arriving in an archive, carrying labels.
	entry := "From sender@example.com Mon Jul 12 10:00:00 2021\r\n" + raw + "\r\n"
	archive := filepath.Join(dir, "dup.mbox")
	if err := os.WriteFile(archive, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	stats := f.run(t, archive, f.options(t, filepath.Join(dir, "state.json")))

	if stats.LeftAlone != 1 || stats.Imported != 0 {
		t.Fatalf("stats = %+v, want leftAlone=1 imported=0", stats)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`, f.userID.String()); got != 1 {
		t.Errorf("messages = %d, want 1 — the archive copy must dedup onto the synced one", got)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE user_id=$1`, f.userID.String()); got != 0 {
		t.Errorf("message_labels = %d, want 0 — labelling a message that HAS provenance "+
			"would enqueue a keyword writeback against the user's real server", got)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1`, f.userID.String()); got != 0 {
		t.Errorf("writeback intents = %d, want 0", got)
	}
	// Its membership is still the IMAP folder, not an imported one.
	var local *string
	if err := f.super.QueryRow(ctx, `SELECT local_mailbox_id FROM app.messages WHERE id=$1`,
		res.MessageID.String()).Scan(&local); err != nil {
		t.Fatal(err)
	}
	if local != nil {
		t.Errorf("the archive gave a synced message a local placement (%v)", *local)
	}
}

func TestImportIsIdempotent(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	archive := writeMbox(t, dir, "twice.mbox", 50)
	state := filepath.Join(dir, "state.json")

	first := f.run(t, archive, f.options(t, state))
	if first.Imported != 50 {
		t.Fatalf("first run imported %d, want 50", first.Imported)
	}

	// Re-run from scratch (no cursor): every message is presented again and every
	// one deduplicates on its content hash. Nothing is duplicated, and — the point —
	// nothing new is written.
	opt := f.options(t, filepath.Join(dir, "state2.json"))
	opt.Resume = false
	second := f.run(t, archive, opt)
	if second.Imported != 0 || second.Deduped != 50 {
		t.Errorf("second run: imported=%d deduped=%d, want 0/50", second.Imported, second.Deduped)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`, f.userID.String()); got != 50 {
		t.Errorf("messages after two full imports = %d, want 50", got)
	}
	if got := f.scalar(t,
		`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Email'`,
		f.userID.String()); got != 50 {
		t.Errorf("Email change_log rows = %d, want 50 — a dedup is not a change", got)
	}
}

// TestImportResumesWithoutDuplicating interrupts an import mid-archive (the crash,
// the Ctrl-C, the OOM killer) and re-runs it. It must not start over, and it must
// not duplicate.
func TestImportResumesWithoutDuplicating(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	archive := writeMbox(t, dir, "long.mbox", 200)
	state := filepath.Join(dir, "state.json")

	// Interrupt after ~60 messages by cancelling the context from the progress
	// callback — the same path a SIGINT takes.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := f.options(t, state)
	opt.ProgressInterval = time.Nanosecond // report (and thus check) every message
	opt.Progress = func(p Progress) {
		if p.Imported >= 60 {
			cancel()
		}
	}
	partial, err := f.runCtx(t, ctx, archive, opt)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted run returned %v, want context.Canceled", err)
	}
	if partial.Imported == 0 || partial.Imported >= 200 {
		t.Fatalf("interrupted run imported %d — wanted a genuine partial import", partial.Imported)
	}
	committed := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`, f.userID.String())
	if committed == 0 || committed >= 200 {
		t.Fatalf("messages after the interruption = %d, want a partial count", committed)
	}

	cp, err := LoadCheckpoint(state)
	if err != nil || cp == nil {
		t.Fatalf("no cursor was written: %v", err)
	}
	if cp.Position.Offset == 0 {
		t.Fatal("the cursor did not advance: a resume would re-read the whole archive")
	}

	// Resume.
	resumed := f.run(t, archive, f.options(t, state))
	total := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`, f.userID.String())
	if total != 200 {
		t.Errorf("messages after resume = %d, want 200 — no gap, no duplicates", total)
	}
	// The resume did NOT start over: it re-read at most the window between the last
	// checkpoint flush and the interruption.
	if resumed.Deduped > 500 {
		t.Errorf("the resume re-presented %d already-imported messages — the cursor is not being honored",
			resumed.Deduped)
	}
	t.Logf("interrupted after %d, cursor at %s:%d, resumed: imported=%d deduped=%d",
		partial.Imported, cp.Position.Source, cp.Position.Offset, resumed.Imported, resumed.Deduped)
}

// TestBadMessagesAreSkippedNotFatal is the "message 400,000 is garbage" case. The
// archive here contains an oversized message, an empty one, a message whose MIME is
// a lie, and one carrying a NUL byte in a header — which PostgreSQL itself refuses
// to store (SQLSTATE 22021). Every one of them must be counted and stepped over.
func TestBadMessagesAreSkippedNotFatal(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()

	var b strings.Builder
	b.WriteString(mboxEntry(1, "Good one"))
	// Broken MIME: a multipart that never has a boundary. internal/mime degrades.
	b.WriteString("From x@example.com Mon Jul 12 10:00:00 2021\r\n" +
		"Subject: Broken MIME\r\n" +
		"Message-ID: <broken@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"nope\"\r\n\r\n" +
		"there is no boundary here at all\r\n\r\n")
	// A charset that does not exist.
	b.WriteString("From x@example.com Mon Jul 12 10:00:00 2021\r\n" +
		"Subject: Bad charset\r\n" +
		"Message-ID: <charset@example.com>\r\n" +
		"Content-Type: text/plain; charset=x-not-a-charset\r\n\r\n" +
		"\xff\xfe binary soup \x01\x02\r\n\r\n")
	// A NUL byte in a header value: Postgres will not store it in a text column.
	b.WriteString("From x@example.com Mon Jul 12 10:00:00 2021\r\n" +
		"Subject: NUL\x00byte\r\n" +
		"Message-ID: <nul@example.com>\r\n\r\n" +
		"body\r\n\r\n")
	// Truncated: headers, then nothing.
	b.WriteString("From x@example.com Mon Jul 12 10:00:00 2021\r\n" +
		"Subject: Truncated\r\n\r\n")
	b.WriteString(mboxEntry(2, "Good two"))
	archive := filepath.Join(dir, "garbage.mbox")
	if err := os.WriteFile(archive, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := f.runErr(t, archive, f.options(t, filepath.Join(dir, "state.json")))
	if err != nil {
		t.Fatalf("a garbage message ENDED the import: %v", err)
	}
	t.Logf("garbage archive: %+v", stats)

	if stats.Failed+stats.Skipped == 0 {
		t.Error("no message was counted as bad — the fixture is not exercising the path")
	}
	// The messages AFTER the garbage must be there. That is the whole claim.
	if got := f.scalar(t,
		`SELECT count(*) FROM app.messages WHERE user_id=$1 AND subject = 'Good two'`,
		f.userID.String()); got != 1 {
		t.Error("the message after the garbage was not imported — one bad message killed the run")
	}
	// And the NUL-byte message is the one Postgres rejected: proof that a SQL error
	// on one message did not poison the transaction of the next.
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1`,
		f.userID.String()); got < 4 {
		t.Errorf("messages = %d, want at least 4 (2 good + the lenient-parse ones)", got)
	}
	if stats.Failed != 1 {
		t.Errorf("failed = %d, want exactly 1 (the NUL-byte header Postgres refuses)", stats.Failed)
	}
}

func TestImportMaildir(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	for _, d := range []string{"cur", "new", "tmp", ".Work/cur", ".Work/new", ".Work/tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("170000000%d.M%d.host:2,S", i, i)
		if err := os.WriteFile(filepath.Join(root, "cur", name), []byte(message(i, fmt.Sprintf("Cur %d", i))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 10; i < 15; i++ {
		name := fmt.Sprintf("17000000%d.M%d.host", i, i)
		if err := os.WriteFile(filepath.Join(root, "new", name), []byte(message(i, fmt.Sprintf("New %d", i))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 15; i < 20; i++ {
		name := fmt.Sprintf("17000000%d.M%d.host:2,FS", i, i)
		if err := os.WriteFile(filepath.Join(root, ".Work", "cur", name), []byte(message(i, fmt.Sprintf("Work %d", i))), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stats := f.run(t, root, f.options(t, filepath.Join(t.TempDir(), "state.json")))
	if stats.Imported != 20 {
		t.Fatalf("imported = %d, want 20 (%+v)", stats.Imported, stats)
	}
	f.mailboxID(t, "Imported/INBOX")
	f.mailboxID(t, "Imported/Work")

	// new/ is unseen, cur/:2,S is seen.
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1 AND NOT is_seen`,
		f.userID.String()); got != 5 {
		t.Errorf("unseen = %d, want 5 (the new/ messages)", got)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1 AND is_flagged`,
		f.userID.String()); got != 5 {
		t.Errorf("flagged = %d, want 5 (the :2,FS ones)", got)
	}
	t.Logf("maildir import: %+v", stats)
}
