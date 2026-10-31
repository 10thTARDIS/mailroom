// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// Integration coverage for canonical ingest against a real database + blob
// store: content-hash dedup, provenance, flag merge, incremental threading,
// and the change_log invariant. Skips when the integration database is not
// configured (run via scripts/integration-test.sh).

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/autocrypt"
	"mailroom/internal/blob"
	"mailroom/internal/clamd"
	"mailroom/internal/store"
)

type fixture struct {
	super     *pgxpool.Pool
	st        *store.Store
	ing       *Ingestor
	userID    ulid.ULID
	accountID ulid.ULID
	mailboxA  ulid.ULID
	mailboxB  ulid.ULID
	dek       []byte
	dekID     ulid.ULID
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

	key := randomKey(t)
	blobs, err := blob.NewFSBlobStore(t.TempDir(), key)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	f := &fixture{
		super: super, st: st, ing: New(st, blobs),
		userID: ulid.Make(), accountID: ulid.Make(),
		mailboxA: ulid.Make(), mailboxB: ulid.Make(),
		dek: randomKey(t), dekID: ulid.Make(),
	}
	f.seed(t)
	return f
}

func (f *fixture) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	uniq := f.userID.String()
	exec := func(sql string, args ...any) {
		if _, err := f.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+uniq, uniq+"@example.com")
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'primary', 'imap.example.com', 'smtp.example.com', '\x00', $3)`,
		f.accountID.String(), f.userID.String(), ulid.Make().String())
	for _, m := range []struct {
		id   ulid.ULID
		name string
	}{{f.mailboxA, "INBOX"}, {f.mailboxB, "Archive"}} {
		exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
		      VALUES ($1, $2, $3, $4, $4)`,
			m.id.String(), f.userID.String(), f.accountID.String(), m.name)
	}
}

func (f *fixture) prov(mailbox ulid.ULID, uid int64, flags []string) Provenance {
	return Provenance{
		AccountID: f.accountID, MailboxID: mailbox, UID: uid, UIDValidity: 1,
		RemoteFlags: flags, InternalDate: time.Unix(1_700_000_000+uid, 0).UTC(),
	}
}

func (f *fixture) changeCount(t *testing.T, typ, op string) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = $2 AND op = $3`,
		f.userID.String(), typ, op).Scan(&n); err != nil {
		t.Fatalf("change count: %v", err)
	}
	return n
}

func (f *fixture) scalar(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

func TestIngestNewMessageThenDedup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	raw := rawMessage("Hello", "m1@example.com", nil)
	sum := sha256.Sum256(raw)

	// First ingest: a brand-new canonical message.
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 101, []string{`\Seen`}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Fatal("first ingest should create a message")
	}
	if res.ContentHash != sum {
		t.Error("content hash mismatch")
	}
	// Message, blob, thread, provenance all exist.
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1 AND content_hash=$2`, f.userID.String(), sum[:]); got != 1 {
		t.Errorf("message count = %d, want 1", got)
	}
	if ok, err := f.ing.blobs.Exists(ctx, f.userID, blob.ContentHash(sum)); err != nil || !ok {
		t.Errorf("blob not stored: ok=%v err=%v", ok, err)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.message_provenance WHERE message_id=$1`, res.MessageID.String()); got != 1 {
		t.Errorf("provenance count = %d, want 1", got)
	}
	if got := f.scalar(t, `SELECT is_seen::int FROM app.messages WHERE id=$1`, res.MessageID.String()); got != 1 {
		t.Error("is_seen should be true from \\Seen flag")
	}
	// change_log invariant: Email created + Thread created.
	if got := f.changeCount(t, "Email", "created"); got != 1 {
		t.Errorf("Email created changes = %d, want 1", got)
	}
	if got := f.changeCount(t, "Thread", "created"); got != 1 {
		t.Errorf("Thread created changes = %d, want 1", got)
	}

	// Second ingest: same bytes, different mailbox — dedup, new provenance,
	// flags unchanged (still \Seen) so only a membership-driven Email update.
	res2, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxB, 55, []string{`\Seen`}))
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created {
		t.Error("second ingest of identical bytes must dedup, not create")
	}
	if res2.MessageID != res.MessageID {
		t.Error("dedup should resolve to the same message id")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE user_id=$1 AND content_hash=$2`, f.userID.String(), sum[:]); got != 1 {
		t.Errorf("still want exactly 1 message after dedup, got %d", got)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.message_provenance WHERE message_id=$1`, res.MessageID.String()); got != 2 {
		t.Errorf("provenance count = %d, want 2 (two locations)", got)
	}
	if got := f.changeCount(t, "Email", "updated"); got != 1 {
		t.Errorf("Email updated changes = %d, want 1 (new location)", got)
	}
}

func TestIngestReplyJoinsThread(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	root, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawMessage("Topic", "root@example.com", nil), f.prov(f.mailboxA, 1, nil))
	if err != nil {
		t.Fatal(err)
	}
	reply, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawMessage("Re: Topic", "reply@example.com", []string{"root@example.com"}), f.prov(f.mailboxA, 2, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Created {
		t.Fatal("reply is new content, should be created")
	}
	if reply.ThreadID != root.ThreadID {
		t.Errorf("reply thread %s != root thread %s — should join", reply.ThreadID, root.ThreadID)
	}
	if got := f.scalar(t, `SELECT message_count FROM app.threads WHERE id=$1`, root.ThreadID.String()); got != 2 {
		t.Errorf("thread message_count = %d, want 2", got)
	}
	// One Thread created (root) + one Thread updated (reply joined).
	if got := f.changeCount(t, "Thread", "created"); got != 1 {
		t.Errorf("Thread created = %d, want 1", got)
	}
	if got := f.changeCount(t, "Thread", "updated"); got != 1 {
		t.Errorf("Thread updated = %d, want 1", got)
	}
}

func TestIngestFlagMergeAcrossLocations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	raw := rawMessage("Flags", "flags@example.com", nil)

	// Unseen in location A.
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 10, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.scalar(t, `SELECT is_seen::int FROM app.messages WHERE id=$1`, res.MessageID.String()); got != 0 {
		t.Fatal("should start unseen")
	}
	// Seen in location B → merged canonical is_seen = true.
	if _, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxB, 20, []string{`\Seen`})); err != nil {
		t.Fatal(err)
	}
	if got := f.scalar(t, `SELECT is_seen::int FROM app.messages WHERE id=$1`, res.MessageID.String()); got != 1 {
		t.Error("is_seen should merge to true (seen in any location)")
	}
}

// RefreshFlags (#523): the flags-only path the CONDSTORE flag-reconcile pass
// takes instead of a full re-ingest — no body, no parse, no scan, no blob
// access, but the SAME change_log discipline: a real flag change appends
// exactly one Email `updated` row, a repeat of the same flags appends none,
// and an unknown coordinate is refused with zero writes at all.
func TestRefreshFlags(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	raw := rawMessage("Flags only", "flagsonly@example.com", nil)

	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 30, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.scalar(t, `SELECT is_seen::int FROM app.messages WHERE id=$1`, res.MessageID.String()); got != 0 {
		t.Fatal("should start unseen")
	}
	before := f.changeCount(t, "Email", "updated")

	// A \Seen flag arrives on the server — RefreshFlags, no body in sight.
	fr, err := f.ing.RefreshFlags(ctx, f.userID, f.prov(f.mailboxA, 30, []string{`\Seen`}))
	if err != nil {
		t.Fatal(err)
	}
	if !fr.Found {
		t.Fatal("RefreshFlags did not find the live provenance row Ingest just created")
	}
	if fr.MessageID != res.MessageID {
		t.Errorf("MessageID = %s, want %s", fr.MessageID, res.MessageID)
	}
	if !fr.Changed {
		t.Error("Changed = false, want true — is_seen just flipped")
	}
	if got := f.scalar(t, `SELECT is_seen::int FROM app.messages WHERE id=$1`, res.MessageID.String()); got != 1 {
		t.Error("is_seen was not set by RefreshFlags")
	}
	if got := f.changeCount(t, "Email", "updated") - before; got != 1 {
		t.Errorf("Email updated changes = %d, want exactly 1", got)
	}

	// The SAME flags again: found, nothing changed, no new change_log row.
	before2 := f.changeCount(t, "Email", "updated")
	fr2, err := f.ing.RefreshFlags(ctx, f.userID, f.prov(f.mailboxA, 30, []string{`\Seen`}))
	if err != nil {
		t.Fatal(err)
	}
	if !fr2.Found {
		t.Fatal("RefreshFlags lost the row on a repeat call")
	}
	if fr2.Changed {
		t.Error("Changed = true on a repeat of the same flags")
	}
	if got := f.changeCount(t, "Email", "updated") - before2; got != 0 {
		t.Errorf("a repeat of the same flags appended %d change_log row(s), want 0", got)
	}

	// An unknown coordinate: not found, and NOTHING written — the caller must
	// fall back to the full body path instead.
	before3 := f.changeCount(t, "Email", "updated")
	fr3, err := f.ing.RefreshFlags(ctx, f.userID, f.prov(f.mailboxA, 999, []string{`\Seen`}))
	if err != nil {
		t.Fatal(err)
	}
	if fr3.Found {
		t.Fatal("RefreshFlags reported Found for a UID it was never given provenance for")
	}
	if got := f.changeCount(t, "Email", "updated") - before3; got != 0 {
		t.Errorf("an unknown coordinate appended %d change_log row(s), want 0", got)
	}
}

func rawMessage(subject, msgID string, refs []string) []byte {
	var b strings.Builder
	b.WriteString("From: Alice <alice@example.com>\r\n")
	b.WriteString("To: bob@example.com\r\n")
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Message-ID: <%s>\r\n", msgID)
	if len(refs) > 0 {
		bracketed := make([]string, len(refs))
		for i, r := range refs {
			bracketed[i] = "<" + r + ">"
		}
		fmt.Fprintf(&b, "References: %s\r\n", strings.Join(bracketed, " "))
		fmt.Fprintf(&b, "In-Reply-To: <%s>\r\n", refs[len(refs)-1])
	}
	b.WriteString("Date: Mon, 12 Jul 2026 10:00:00 +0000\r\n\r\n")
	fmt.Fprintf(&b, "Body of %s\r\n", subject)
	return []byte(b.String())
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := crand.Read(k); err != nil {
		t.Fatalf("random key: %v", err)
	}
	return k
}

func envOrSkip(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	return v
}

// ---------------------------------------------------------------------------
// Malware scanning end to end (#394, ADR-027 §4).
//
// scan_test.go proves the taxonomy against a fake scanner and
// store/attachments_integration_test.go proves SetScanVerdicts against a real
// database. Neither proves the JOIN: that a verdict produced before the
// transaction actually lands on the row inside it. That is what these cover.
// ---------------------------------------------------------------------------

const msgWithAttachment = "From: sender@example.com\r\n" +
	"To: me@example.com\r\n" +
	"Subject: here is a file\r\n" +
	"Message-ID: <scan-1@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=bnd\r\n" +
	"\r\n" +
	"--bnd\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"the body\r\n" +
	"--bnd\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"payload.bin\"\r\n" +
	"\r\n" +
	"PAYLOAD\r\n" +
	"--bnd--\r\n"

// withScanner rebuilds the fixture's Ingestor around a scanner. The fixture's
// default Ingestor has none, which is the pre-#394 behaviour every other test
// in this file relies on.
func (f *fixture) withScanner(t *testing.T, s Scanner) {
	t.Helper()
	key := randomKey(t)
	blobs, err := blob.NewFSBlobStore(t.TempDir(), key)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	f.ing = New(f.st, blobs, WithScanner(s))
}

func (f *fixture) statusOf(t *testing.T, msgID ulid.ULID, part string) string {
	t.Helper()
	var s string
	if err := f.super.QueryRow(context.Background(),
		`SELECT scan_status FROM app.attachments WHERE user_id = $1 AND message_id = $2 AND part_path = $3`,
		f.userID.String(), msgID.String(), part).Scan(&s); err != nil {
		t.Fatalf("read scan_status: %v", err)
	}
	return s
}

// The verdict reaches the row, in the same transaction as the message.
func TestIngestRecordsTheScanVerdictOnTheProjectedRow(t *testing.T) {
	f := newFixture(t)
	f.withScanner(t, &fakeScanner{verdict: clamd.Verdict{State: clamd.Flagged, Signature: "Eicar-Test-Signature"}})

	res, err := f.ing.Ingest(context.Background(), f.userID, f.dek, f.dekID,
		[]byte(msgWithAttachment), f.prov(f.mailboxA, 1, nil))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := f.statusOf(t, res.MessageID, "2"); got != "flagged" {
		t.Errorf("scan_status = %q, want %q — the verdict never reached the row", got, "flagged")
	}

	// The control: the same message through a CLEAN scanner records clean, so
	// the assertion above is about the verdict rather than about a default.
	f2 := newFixture(t)
	f2.withScanner(t, &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}})
	res2, err := f2.ing.Ingest(context.Background(), f2.userID, f2.dek, f2.dekID,
		[]byte(msgWithAttachment), f2.prov(f2.mailboxA, 1, nil))
	if err != nil {
		t.Fatalf("ingest (control): %v", err)
	}
	if got := f2.statusOf(t, res2.MessageID, "2"); got != "clean" {
		t.Fatalf("control scan_status = %q, want %q", got, "clean")
	}
}

// ⚠ FAIL-CLOSED, PROVEN AT THE DATABASE. A scanner outage must leave NOTHING
// behind — no canonical message, no provenance, no change_log row — because the
// mail is still on the provider and the next pass will present it again. A
// partial write here would be a message the user can see with parts nothing has
// judged, which is the state synchronous scanning exists to make impossible.
//
// It is also the proof that the scan really does run BEFORE the transaction
// opens: a scan inside the transaction would still roll back, but this test
// would then be silent about the ordering it is named for, so the row count of
// zero is checked against a control that ingests the same bytes successfully.
func TestAScannerOutageWritesNothingAtAll(t *testing.T) {
	f := newFixture(t)
	f.withScanner(t, &fakeScanner{
		pingErr: errors.New("connect: connection refused"),
		scanErr: clamd.ErrDaemonUnreachable,
	})

	_, err := f.ing.Ingest(context.Background(), f.userID, f.dek, f.dekID,
		[]byte(msgWithAttachment), f.prov(f.mailboxA, 1, nil))
	if err == nil {
		t.Fatal("ingest succeeded during a scanner outage — mail was admitted unscanned")
	}
	if !errors.Is(err, ErrScannerUnavailable) {
		t.Fatalf("err = %v, want ErrScannerUnavailable so mailsync pauses the account", err)
	}

	for _, q := range []struct {
		what string
		sql  string
	}{
		{"messages", `SELECT count(*) FROM app.messages WHERE user_id = $1`},
		{"attachments", `SELECT count(*) FROM app.attachments WHERE user_id = $1`},
		{"provenance", `SELECT count(*) FROM app.message_provenance WHERE user_id = $1`},
		{"change_log", `SELECT count(*) FROM app.change_log WHERE user_id = $1`},
	} {
		if n := f.scalar(t, q.sql, f.userID.String()); n != 0 {
			t.Errorf("%s rows = %d after a refused ingest, want 0", q.what, n)
		}
	}

	// THE CONTROL. The same fixture, the same bytes, a working scanner: all four
	// counts move. Without it, "0 rows everywhere" could be true because the
	// fixture never writes anything.
	f.withScanner(t, &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}})
	if _, err := f.ing.Ingest(context.Background(), f.userID, f.dek, f.dekID,
		[]byte(msgWithAttachment), f.prov(f.mailboxA, 1, nil)); err != nil {
		t.Fatalf("control ingest failed, so the zero counts above prove nothing: %v", err)
	}
	for _, q := range []struct {
		what string
		sql  string
	}{
		{"messages", `SELECT count(*) FROM app.messages WHERE user_id = $1`},
		{"attachments", `SELECT count(*) FROM app.attachments WHERE user_id = $1`},
		{"provenance", `SELECT count(*) FROM app.message_provenance WHERE user_id = $1`},
		{"change_log", `SELECT count(*) FROM app.change_log WHERE user_id = $1`},
	} {
		if n := f.scalar(t, q.sql, f.userID.String()); n == 0 {
			t.Errorf("control: %s rows = 0, so the refusal assertions above are vacuous", q.what)
		}
	}
}

// TestIngestSameMessageIDJoinsThread covers #210's live half: two messages
// bearing the SAME Message-ID belong to one conversation (RFC 5322 §3.6.4
// makes a Message-ID globally unique), even when neither carries References.
//
// ⚠ THIS IS THE PATH THE APPLICATION ACTUALLY USES. internal/thread's JWZ
// implementation has the same rule, but it has no importers — the running
// threader is store.FindThreadByHints, and before this fix its three match
// arms covered references in both directions and never "an existing message
// IS this message".
//
// Observed on a real mailbox before the fix: duplicate Message-ID
// groups split across separate threads — every one a single message
// re-delivered through more than one catchall alias.
//
// The two messages carry DIFFERENT subjects on purpose. That keeps their
// content hashes (and #206 dedup hashes) distinct, so they survive as two
// messages rather than collapsing into one — and it means the join under test
// can only have come from the Message-ID, not from any subject heuristic.
func TestIngestSameMessageIDJoinsThread(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawMessage("Newsletter issue 12", "dup@example.com", nil), f.prov(f.mailboxA, 1, nil))
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawMessage("Newsletter issue 12 (via alias)", "dup@example.com", nil), f.prov(f.mailboxA, 2, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Created {
		t.Fatal("the second copy has distinct content and must be its own message, not a dedup hit")
	}
	if second.ThreadID != first.ThreadID {
		t.Errorf("second thread %s != first thread %s — one Message-ID is one conversation",
			second.ThreadID, first.ThreadID)
	}

	// ⚠ The later message JOINS; it must not displace the earlier one. The
	// arriving message is the only one whose thread is chosen, so this is
	// structural rather than defended — assert it anyway, because a future
	// "merge the threads" implementation could move the original instead.
	if got := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND thread_id=$2`,
		first.MessageID.String(), first.ThreadID.String()); got != 1 {
		t.Errorf("the first message was moved out of its own thread by a later duplicate")
	}
	if got := f.scalar(t, `SELECT message_count FROM app.threads WHERE id=$1`, first.ThreadID.String()); got != 2 {
		t.Errorf("thread message_count = %d, want 2", got)
	}
}

// TestIngestUnrelatedMessageDoesNotJoinThread is the control for the test
// above. Without it, "the second message joined the first's thread" would also
// pass against a threader that put EVERYTHING in one thread.
func TestIngestUnrelatedMessageDoesNotJoinThread(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawMessage("Newsletter issue 12", "one@example.com", nil), f.prov(f.mailboxA, 1, nil))
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawMessage("Something else entirely", "two@example.com", nil), f.prov(f.mailboxA, 2, nil))
	if err != nil {
		t.Fatal(err)
	}
	if other.ThreadID == first.ThreadID {
		t.Errorf("unrelated messages share thread %s — the Message-ID arm is matching too much",
			first.ThreadID)
	}
}

// --- Autocrypt key discovery (#48) -----------------------------------------

// autocryptKey generates a real OpenPGP key and returns its binary public half
// plus its fingerprint. Real key material, because the ingest path runs it
// through pgpkeys.Parse and a fake would prove nothing about that.
func autocryptKey(t *testing.T, name, email string) (pub []byte, fingerprint string) {
	t.Helper()
	e, err := openpgp.NewEntity(name, "", email, nil)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	return buf.Bytes(), strings.ToUpper(fmt.Sprintf("%X", e.PrimaryKey.Fingerprint))
}

// rawFrom builds a message from `from` with optional extra headers.
func rawFrom(from, subject, msgID string, extra ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	b.WriteString("To: bob@example.com\r\n")
	for _, h := range extra {
		b.WriteString(h)
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Message-ID: <%s>\r\n", msgID)
	b.WriteString("Date: Mon, 12 Jul 2026 10:00:00 +0000\r\n\r\n")
	fmt.Fprintf(&b, "Body of %s\r\n", subject)
	return []byte(b.String())
}

// TestIngestStoresAutocryptKey is the inbound half of #48 end to end: a
// correspondent's key arrives in a header and lands in app.contact_keys against
// the contact ingest created from the same message.
func TestIngestStoresAutocryptKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	pub, fpr := autocryptKey(t, "Carol Example", "carol@example.com")
	hdr := autocrypt.HeaderName + ": " + autocrypt.Header("carol@example.com", pub, false)

	if _, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawFrom("Carol <carol@example.com>", "Hello", "ac1@example.com", hdr),
		f.prov(f.mailboxA, 1, nil)); err != nil {
		t.Fatal(err)
	}

	if got := f.scalar(t, `SELECT count(*) FROM app.contact_keys WHERE fingerprint=$1 AND source='autocrypt'`, fpr); got != 1 {
		t.Fatalf("contact_keys rows for the sender's fingerprint = %d, want 1", got)
	}
	// It must hang off the CONTACT for that address, not float unattached.
	if got := f.scalar(t, `SELECT count(*) FROM app.contact_keys k
	                        JOIN app.contact_addresses a ON a.contact_id = k.contact_id
	                       WHERE k.fingerprint=$1 AND a.address='carol@example.com'`, fpr); got != 1 {
		t.Error("the stored key is not attached to the sender's contact")
	}
	// The stored bytes must be the key itself, not the base64 or the header.
	if got := f.scalar(t, `SELECT count(*) FROM app.contact_keys WHERE fingerprint=$1 AND public_key=$2`,
		fpr, pub); got != 1 {
		t.Error("stored public_key is not the binary key that arrived")
	}
}

// TestIngestWithoutAutocryptStoresNoKey is the control. Without it, the test
// above would pass against an ingest that stored a key for every message.
func TestIngestWithoutAutocryptStoresNoKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawFrom("Dave <dave@example.com>", "No key here", "ac2@example.com"),
		f.prov(f.mailboxA, 1, nil)); err != nil {
		t.Fatal(err)
	}
	// ⚠ SCOPED TO THIS FIXTURE'S USER. The harness reads as superuser, so an
	// unscoped count sees every other test's rows — this assertion failed on
	// exactly that before the filter was added, which is a good demonstration
	// that a count without a denominator is not a measurement.
	if got := f.scalar(t, `SELECT count(*) FROM app.contact_keys WHERE user_id=$1`,
		f.userID.String()); got != 0 {
		t.Fatalf("contact_keys rows for THIS user = %d, want 0 for a message carrying no Autocrypt header", got)
	}
}

// TestIngestRefusesAutocryptKeyClaimingAnotherAddress is the security control
// Autocrypt Level 1 requires.
//
// ⚠ Without the From check, a sender could attach a key claiming to be someone
// else's address, and a later encrypt-to-contact would reach for the attacker's
// key when writing to that third party. The message must still ingest normally —
// this is a refused key, not a refused message.
func TestIngestRefusesAutocryptKeyClaimingAnotherAddress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	pub, fpr := autocryptKey(t, "Victim Bank", "bank@example.com")
	// Mallory sends, but the header claims the bank's address.
	hdr := autocrypt.HeaderName + ": " + autocrypt.Header("bank@example.com", pub, false)

	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawFrom("Mallory <mallory@example.com>", "Your statement", "ac3@example.com", hdr),
		f.prov(f.mailboxA, 1, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Fatal("the message must still ingest — a bad key claim is not a bad message")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.contact_keys WHERE fingerprint=$1`, fpr); got != 0 {
		t.Fatalf("stored %d key(s) for a header claiming an address other than From, want 0", got)
	}
}

// TestIngestAutocryptKeyIsIdempotent — the same key arriving again is a
// re-sighting, not a second key. Mail from one correspondent arrives constantly,
// so this is the common case rather than an edge one.
func TestIngestAutocryptKeyIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	pub, fpr := autocryptKey(t, "Erin Example", "erin@example.com")
	hdr := autocrypt.HeaderName + ": " + autocrypt.Header("erin@example.com", pub, false)

	for i, id := range []string{"ac4a@example.com", "ac4b@example.com"} {
		if _, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
			rawFrom("Erin <erin@example.com>", fmt.Sprintf("Message %d", i), id, hdr),
			f.prov(f.mailboxA, int64(i+1), nil)); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.contact_keys WHERE fingerprint=$1`, fpr); got != 1 {
		t.Fatalf("contact_keys rows = %d after two messages carrying the same key, want 1", got)
	}
}
