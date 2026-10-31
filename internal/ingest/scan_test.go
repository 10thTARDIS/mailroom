// SPDX-License-Identifier: AGPL-3.0-only

package ingest

// The scan taxonomy (#394, ADR-027 §4b, §5a, §6).
//
// These tests exercise scanMessage against a fake Scanner rather than a daemon,
// because the states that matter operationally — dead, alive-then-dead, a file
// that takes the daemon down three times running — are seconds of fake and
// hours of container. The wire vocabulary itself is tested against captured
// output from a real clamd in internal/clamd, and the whole client is exercised
// against a live daemon by scripts/clamav-test.sh.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	"mailroom/internal/clamd"
	"mailroom/internal/mime"
)

// fakeScanner answers from a script. scans/pings count calls so a test can
// assert something was NOT contacted — the difference between "recorded
// unknown" and "recorded unknown without sending the bytes anywhere".
type fakeScanner struct {
	verdict  clamd.Verdict
	scanErr  error
	pingErr  error
	versErr  error
	database int
	scans    int
	pings    int
	versions int
	lastData []byte
}

func (f *fakeScanner) Scan(_ context.Context, data []byte) (clamd.Verdict, error) {
	f.scans++
	f.lastData = data
	if f.scanErr != nil {
		return clamd.Verdict{}, f.scanErr
	}
	return f.verdict, nil
}

func (f *fakeScanner) Ping(context.Context) error {
	f.pings++
	return f.pingErr
}

// Version answers with database, or 28110 when a test did not care. versions
// counts calls so the TTL cache can be asserted rather than assumed — #407
// warns that a version read must not become a per-message round trip, and a
// cache nobody counts is a cache nobody knows is working.
func (f *fakeScanner) Version(context.Context) (clamd.Version, error) {
	f.versions++
	if f.versErr != nil {
		return clamd.Version{}, f.versErr
	}
	db := f.database
	if db == 0 {
		db = 28110
	}
	return clamd.Version{Engine: "1.5.4", Database: db}, nil
}

func ingestorWith(s Scanner) *Ingestor {
	return &Ingestor{scanner: s, breaker: newScanBreaker()}
}

// oneAttachment is a minimal message with a single non-text leaf, which is what
// mime classifies as an attachment.
const oneAttachment = "From: a@example.com\r\n" +
	"To: b@example.com\r\n" +
	"Subject: with a file\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=bnd\r\n" +
	"\r\n" +
	"--bnd\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"body\r\n" +
	"--bnd\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"thing.bin\"\r\n" +
	"\r\n" +
	"PAYLOAD\r\n" +
	"--bnd--\r\n"

func parse(t *testing.T, raw string) (*mime.Message, [32]byte) {
	t.Helper()
	m, err := mime.ParseBytes([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m, sha256.Sum256([]byte(raw))
}

// The ordinary path, and the control for everything below it: a configured
// scanner produces a per-part verdict carrying clamd's own state.
func TestAScanProducesAVerdictPerPart(t *testing.T) {
	fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Flagged, Signature: "Eicar-Test-Signature"}}
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)

	got, _, err := in.scanMessage(context.Background(), sum, m)
	if err != nil {
		t.Fatalf("scanMessage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("verdicts = %d, want 1", len(got))
	}
	if got[0].Status != string(clamd.Flagged) || got[0].Signature != "Eicar-Test-Signature" {
		t.Errorf("verdict = %+v, want flagged/Eicar-Test-Signature", got[0])
	}
	if got[0].PartPath == "" {
		t.Error("no part path — part_path is NOT NULL and a verdict must name a part a download URL can resolve")
	}
	// The bytes that went to the daemon are the DECODED part, not the raw
	// message: scanning the base64 would find nothing in anything.
	if !strings.Contains(string(fs.lastData), "PAYLOAD") {
		t.Errorf("the scanner was sent %q, want the decoded part content", fs.lastData)
	}
}

// No scanner configured is the pre-#394 behaviour and must stay silent: no
// verdicts, so the rows keep app.attachments' 'unscanned' default.
func TestNoScannerWritesNoVerdicts(t *testing.T) {
	in := &Ingestor{breaker: newScanBreaker()}
	m, sum := parse(t, oneAttachment)
	got, _, err := in.scanMessage(context.Background(), sum, m)
	if err != nil || got != nil {
		t.Fatalf("scanMessage = (%v, %v), want (nil, nil)", got, err)
	}
}

// ⚠ ADR-027 §5a. internal/mime truncates at 64 MiB / MaxDepth / MaxParts, and a
// truncated walk sees a PREFIX. A clean verdict on a prefix, while the whole
// payload stays downloadable as raw .eml, is a green badge on a file nobody
// read to the end.
func TestATruncatedParseForcesUnknownWithoutScanning(t *testing.T) {
	fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)
	m.Truncated = true

	got, _, err := in.scanMessage(context.Background(), sum, m)
	if err != nil {
		t.Fatalf("scanMessage: %v", err)
	}
	if len(got) != 1 || got[0].Status != string(clamd.Unknown) {
		t.Fatalf("verdicts = %+v, want a single unknown", got)
	}
	// Not merely overridden afterwards: the bytes never went anywhere. A design
	// that scanned and then discarded the answer would burn scanner time on
	// every oversized message.
	if fs.scans != 0 {
		t.Errorf("the scanner was called %d times for a truncated parse", fs.scans)
	}
}

// ⚠ ADR-027 §6. WKD publishes the operator's key, so any stranger can encrypt
// to this mailbox. clamd scanning ciphertext returns OK — the row would say
// `clean` about a file that was never examined in plaintext, on a path the
// ATTACKER chose. This is the one place where the scanner working perfectly
// produces the worst possible answer.
func TestAnEncryptedMessageForcesUnknownWithoutScanning(t *testing.T) {
	const encrypted = "From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: sealed\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=bnd\r\n" +
		"\r\n" +
		"--bnd\r\n" +
		"Content-Type: application/pgp-encrypted\r\n" +
		"\r\n" +
		"Version: 1\r\n" +
		"--bnd\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"\r\n" +
		"-----BEGIN PGP MESSAGE-----\r\n" +
		"ciphertext\r\n" +
		"-----END PGP MESSAGE-----\r\n" +
		"--bnd--\r\n"

	fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}
	in := ingestorWith(fs)
	m, sum := parse(t, encrypted)

	got, _, err := in.scanMessage(context.Background(), sum, m)
	if err != nil {
		t.Fatalf("scanMessage: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("an encrypted message with parts produced no verdicts at all")
	}
	for _, v := range got {
		if v.Status != string(clamd.Unknown) {
			t.Errorf("part %q = %q, want unknown — a clean verdict on ciphertext is a badge on an unexamined file",
				v.PartPath, v.Status)
		}
	}
	if fs.scans != 0 {
		t.Errorf("ciphertext was sent to the scanner %d times", fs.scans)
	}
}

// #405: the encrypted SUBTREE is unknown, its plaintext sibling is scanned, and
// a forwarded encrypted message (message/rfc822) does not touch the outer parts.
func TestOnlyTheEncryptedSubtreeIsForcedUnknown(t *testing.T) {
	const inner = "Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=in\r\n\r\n" +
		"--in\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
		"--in\r\nContent-Type: application/octet-stream\r\n\r\nciphertext\r\n--in--\r\n"
	const head = "From: a@example.com\r\nSubject: mixed\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=o\r\n\r\n" +
		"--o\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"r.pdf\"\r\n\r\n%PDF-1.4\r\n"

	for _, tc := range []struct {
		name      string
		raw       string
		wantScans int
		wantUnk   int
	}{
		{"encrypted sibling", head + "--o\r\n" + inner + "--o--\r\n", 1, 2},
		{"forwarded encrypted message", head + "--o\r\nContent-Type: message/rfc822\r\n\r\nFrom: x@example.com\r\nMIME-Version: 1.0\r\n" + inner + "\r\n--o--\r\n", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}
			m, sum := parse(t, tc.raw)
			got, _, err := ingestorWith(fs).scanMessage(context.Background(), sum, m)
			if err != nil {
				t.Fatalf("scanMessage: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("no verdicts; the case proves nothing")
			}
			unk := 0
			for _, v := range got {
				if v.Status == string(clamd.Unknown) {
					unk++
				}
			}
			if unk != tc.wantUnk || fs.scans != tc.wantScans {
				t.Errorf("unknown=%d scans=%d, want unknown=%d scans=%d (verdicts %+v)", unk, fs.scans, tc.wantUnk, tc.wantScans, got)
			}
		})
	}
}

// The control for the two tests above: this fixture and this scanner DO produce
// `clean` when neither condition holds. Without it, "forced unknown" could be
// true because the fake never returns anything else.
func TestTheForcedUnknownControlProducesCleanOtherwise(t *testing.T) {
	fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)

	got, _, err := in.scanMessage(context.Background(), sum, m)
	if err != nil {
		t.Fatalf("scanMessage: %v", err)
	}
	if len(got) != 1 || got[0].Status != string(clamd.Clean) {
		t.Fatalf("verdicts = %+v, want a single clean — the two forced-unknown tests above prove nothing otherwise", got)
	}
	if fs.scans != 1 {
		t.Errorf("scans = %d, want 1", fs.scans)
	}
}

// ⚠ THE OUTAGE CASE. Dead before the message was touched → refuse, and count
// NOTHING against the message. Counting here is what would make a long outage
// drip mail through as `unknown`, one message per threshold passes, with the
// scanner still dead.
func TestAnOutageRefusesAndNeverMovesTheBreaker(t *testing.T) {
	fs := &fakeScanner{
		pingErr: errors.New("connect: connection refused"),
		scanErr: clamd.ErrDaemonUnreachable,
	}
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)

	// Far more attempts than the threshold. A naive consecutive-failure counter
	// would have opened the breaker several times over by now.
	for i := 0; i < scanBreakerThreshold*3; i++ {
		got, _, err := in.scanMessage(context.Background(), sum, m)
		if err == nil {
			t.Fatalf("attempt %d: the pass was ADMITTED during an outage (verdicts=%+v) — "+
				"the breaker counted failures the daemon's own death caused", i, got)
		}
		if !errors.Is(err, ErrScannerUnavailable) {
			t.Fatalf("attempt %d: err = %v, want ErrScannerUnavailable", i, err)
		}
	}
	if in.breaker.open(sum) {
		t.Error("the breaker opened during a pure outage")
	}
}

// ⚠ THE CRASH CASE (§4b step 3). The daemon answers PING, then dies scanning
// THIS message — the file is the problem, and the error is connection-level
// either way. After the threshold the message is admitted with `unknown`
// verdicts rather than halting the account forever at a stranger's choosing.
func TestAMessageThatKillsTheDaemonEventuallyGetsUnknownRatherThanHaltingTheAccount(t *testing.T) {
	fs := &fakeScanner{scanErr: clamd.ErrDaemonUnreachable} // pingErr nil: alive, then dead
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)

	for i := 1; i < scanBreakerThreshold; i++ {
		if _, _, err := in.scanMessage(context.Background(), sum, m); !errors.Is(err, ErrScannerUnavailable) {
			t.Fatalf("attempt %d: err = %v, want the pass refused while the breaker is still closed", i, err)
		}
	}

	got, _, err := in.scanMessage(context.Background(), sum, m)
	if err != nil {
		t.Fatalf("attempt %d still refused (%v) — one message can now halt the account forever, "+
			"which ADR-027 §4b calls a remote kill switch operated by whoever sends the mail",
			scanBreakerThreshold, err)
	}
	if len(got) != 1 || got[0].Status != string(clamd.Unknown) {
		t.Fatalf("verdicts = %+v, want a single unknown", got)
	}

	// And once open it stays open WITHOUT touching the daemon again: re-sending
	// a file that has already taken clamd down three times is how a breaker
	// becomes a loop.
	before := fs.scans
	if _, _, err := in.scanMessage(context.Background(), sum, m); err != nil {
		t.Fatalf("a message past the breaker was refused again: %v", err)
	}
	if fs.scans != before {
		t.Errorf("scans went %d -> %d: the open breaker re-sent the file that killed the daemon", before, fs.scans)
	}
}

// A message that scans successfully must not carry earlier failures forever —
// otherwise one restart-time blip becomes a permanent `unknown`.
func TestASuccessfulScanClearsTheBreaker(t *testing.T) {
	fs := &fakeScanner{scanErr: clamd.ErrDaemonUnreachable}
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)

	if _, _, err := in.scanMessage(context.Background(), sum, m); err == nil {
		t.Fatal("expected a refusal")
	}
	fs.scanErr = nil
	fs.verdict = clamd.Verdict{State: clamd.Clean}
	if _, _, err := in.scanMessage(context.Background(), sum, m); err != nil {
		t.Fatalf("recovery scan failed: %v", err)
	}

	// Now prove the count really is back at zero: with the daemon dying again,
	// it must take the FULL threshold to open, not one more attempt.
	fs.scanErr = clamd.ErrDaemonUnreachable
	for i := 1; i < scanBreakerThreshold; i++ {
		if _, _, err := in.scanMessage(context.Background(), sum, m); err == nil {
			t.Fatalf("attempt %d after recovery was admitted — the earlier failure was never cleared", i)
		}
	}
}

// The health string carries the running count, because "the scanner is down"
// and "one message is two failures from being waved through" are different
// operational situations with different fixes.
func TestTheRefusalNamesItsAttemptCount(t *testing.T) {
	fs := &fakeScanner{scanErr: clamd.ErrDaemonUnreachable}
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)

	_, _, err := in.scanMessage(context.Background(), sum, m)
	var sue *ScannerUnavailableError
	if !errors.As(err, &sue) {
		t.Fatalf("err = %v, want a *ScannerUnavailableError the runner can read figures off", err)
	}
	if sue.Consecutive != 1 {
		t.Errorf("Consecutive = %d, want 1", sue.Consecutive)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(sue.Consecutive)) {
		t.Errorf("health string %q does not carry the attempt count", err.Error())
	}
}

// A message with no attachments never contacts the scanner. Most mail has none,
// and a PING per message would be a round trip bought for nothing.
func TestAMessageWithNoAttachmentsNeverContactsTheScanner(t *testing.T) {
	fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}
	in := ingestorWith(fs)
	m, sum := parse(t, "From: a@example.com\r\nSubject: plain\r\n\r\njust text\r\n")

	got, _, err := in.scanMessage(context.Background(), sum, m)
	if err != nil || got != nil {
		t.Fatalf("scanMessage = (%v, %v), want (nil, nil)", got, err)
	}
	if fs.scans != 0 || fs.pings != 0 {
		t.Errorf("scans=%d pings=%d for a message with no attachments, want 0/0", fs.scans, fs.pings)
	}
}

// ⚠ A SHUTDOWN IS NOT A SCANNER OUTAGE. internal/clamd was fixed so a cancelled
// caller does not become ErrDaemonUnreachable; this is the same rule one layer
// up. Without it, every restart that lands mid-scan pauses the account and
// records account health "the malware scanner is unreachable" — about a scanner
// that was fine, for a condition that has already gone away.
func TestACancelledIngestIsNotAScannerOutage(t *testing.T) {
	fs := &fakeScanner{scanErr: context.Canceled}
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := in.scanMessage(ctx, sum, m)
	if err == nil {
		t.Fatal("a cancelled scan produced a verdict")
	}
	if errors.Is(err, ErrScannerUnavailable) {
		t.Errorf("a cancelled ingest was reported as a scanner outage: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	// And it left no mark: a shutdown must not push a perfectly good message
	// toward the circuit breaker's threshold.
	for i := 0; i < scanBreakerThreshold*2; i++ {
		_, _, _ = in.scanMessage(ctx, sum, m)
	}
	if in.breaker.open(sum) {
		t.Error("repeated cancellations opened the circuit breaker — a restart loop would mark good mail unknown")
	}
}

// ⚠ ZERO BYTES CANNOT BE CLEAN. clamd answers "stream: OK" to an empty
// INSTREAM, so a part that decoded to nothing would be recorded `clean` — a
// green verdict about a file nothing read. internal/mime reaches this state on
// its own: a body read error is a WARNING, the leaf keeps whatever it got, and
// unlike the size limit it does NOT set Truncated, so forcedUnknownReason never
// sees it.
func TestAnEmptyPartIsUnknownRatherThanCleanOnNothing(t *testing.T) {
	const emptyPart = "From: a@example.com\r\n" +
		"Subject: nothing attached\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=bnd\r\n" +
		"\r\n" +
		"--bnd\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"body\r\n" +
		"--bnd\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"empty.bin\"\r\n" +
		"\r\n" +
		"--bnd--\r\n"

	// A scanner that would say CLEAN about anything it is handed — which is
	// exactly what a real clamd does with an empty stream.
	fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}
	in := ingestorWith(fs)
	m, sum := parse(t, emptyPart)

	parts := m.AttachmentParts()
	if len(parts) != 1 || len(parts[0].Node.Data) != 0 {
		t.Fatalf("fixture: got %d parts with %d bytes; this test needs exactly one EMPTY attachment part",
			len(parts), len(parts[0].Node.Data))
	}

	got, _, err := in.scanMessage(context.Background(), sum, m)
	if err != nil {
		t.Fatalf("scanMessage: %v", err)
	}
	if len(got) != 1 || got[0].Status != string(clamd.Unknown) {
		t.Fatalf("verdicts = %+v, want a single unknown — 'clamd found nothing in nothing' is not a claim about a file", got)
	}
	if fs.scans != 0 {
		t.Errorf("zero bytes were sent to the scanner %d times", fs.scans)
	}
}

// ── #407: a verdict is a claim about what the signatures knew ────────────────
//
// ⚠ INGEST USED TO WRITE A VERDICT WITH NO STAMP, and the consequence was not
// cosmetic. `AttachmentVerdictCoverage.NeverScanned` — the ONE field whose
// documented purpose is "so a backlog that is not being cleared is visible as a
// number, not as silence" — counted mail scanned seconds ago, and every newly
// ingested message came back for an immediate redundant re-scan on the
// sequential dispatcher that `submit` shares.

// The stamp carries the daemon's own numbers, not invented ones.
func TestAScanIsStampedWithTheDaemonsVersion(t *testing.T) {
	fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}, database: 28113}
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)

	_, stamp, err := in.scanMessage(context.Background(), sum, m)
	if err != nil {
		t.Fatalf("scanMessage: %v", err)
	}
	if stamp == nil {
		t.Fatal("no stamp — the row would carry three NULL freshness columns, so attachment-report " +
			"counts freshly scanned mail as never scanned and re-scan picks it up again immediately")
	}
	if stamp.DBVersion != 28113 || stamp.Engine != "1.5.4" {
		t.Errorf("stamp = %d/%q, want 28113/1.5.4 — the numbers must be the daemon's, since the whole "+
			"point is comparing them against what it reports later", stamp.DBVersion, stamp.Engine)
	}
	if stamp.At.IsZero() {
		t.Error("stamp.At is zero — ADR-027 §1a surfaces the age of a verdict, and it cannot be aged " +
			"from a zero time")
	}
}

// ⚠ THE VERSION IS READ BEFORE THE BYTES GO OUT, AND THIS IS THE ASSERTION THAT
// SAYS SO. #407 names the failure explicitly: read it afterwards and a mid-scan
// freshclam update makes the row claim a scan against signatures that were not
// loaded when it ran — after which the row looks CURRENT and stops being
// re-scanned, so the error is permanent rather than transient.
//
// The fake records how many Version calls had happened by the time Scan was
// entered, which is the ordering itself rather than a proxy for it.
func TestTheSignatureVersionIsReadBeforeTheScan(t *testing.T) {
	fs := &orderRecordingScanner{fakeScanner: fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}}
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)

	if _, _, err := in.scanMessage(context.Background(), sum, m); err != nil {
		t.Fatalf("scanMessage: %v", err)
	}
	if fs.scans == 0 {
		t.Fatal("CONTROL FAILED: the scanner was never called, so the ordering below is unobserved")
	}
	if fs.versionsAtFirstScan != 1 {
		t.Errorf("the daemon had been asked for its version %d time(s) when the first scan started, "+
			"want 1. Reading it AFTER the bytes go out lets a mid-scan freshclam update stamp these "+
			"rows as current against signatures that never saw them — and a row that looks current "+
			"is never re-scanned again", fs.versionsAtFirstScan)
	}
}

// orderRecordingScanner notes how many Version calls preceded the first Scan.
type orderRecordingScanner struct {
	fakeScanner
	versionsAtFirstScan int
}

func (o *orderRecordingScanner) Scan(ctx context.Context, data []byte) (clamd.Verdict, error) {
	if o.scans == 0 {
		o.versionsAtFirstScan = o.versions
	}
	return o.fakeScanner.Scan(ctx, data)
}

// ⚠ A VERSION READ MUST NOT BECOME A PER-MESSAGE ROUND TRIP (#407). Ingest runs
// inside the sync path, so a socket call per message is a fixed cost on every
// delivery to fetch a number that changes a few times a day.
func TestTheSignatureVersionIsCachedAcrossMessages(t *testing.T) {
	fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)

	for i := 0; i < 5; i++ {
		if _, _, err := in.scanMessage(context.Background(), sum, m); err != nil {
			t.Fatalf("scanMessage %d: %v", i, err)
		}
	}
	if fs.scans != 5 {
		t.Fatalf("CONTROL FAILED: %d scans for 5 messages, so the loop below is not measuring five "+
			"passes", fs.scans)
	}
	if fs.versions != 1 {
		t.Errorf("the daemon was asked for its version %d times across 5 messages, want 1 — a socket "+
			"round trip per message is a fixed cost on every delivery", fs.versions)
	}
}

// ⚠ AND THE CACHE MUST GATE THE FAILURE TOO. A daemon that answers PING but not
// VERSION would otherwise cost a round trip on every single message — the exact
// thing the cache exists to prevent, arriving precisely when things are already
// wrong.
func TestAVersionFailureIsCachedAndDoesNotRefuseTheScan(t *testing.T) {
	fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}, versErr: errors.New("no VERSION for you")}
	in := ingestorWith(fs)
	m, sum := parse(t, oneAttachment)

	for i := 0; i < 4; i++ {
		got, stamp, err := in.scanMessage(context.Background(), sum, m)
		if err != nil {
			t.Fatalf("a version read failure refused the whole scan: %v — the scan itself succeeded, "+
				"and refusing here would pause an account over a number", err)
		}
		if len(got) != 1 || got[0].Status != string(clamd.Clean) {
			t.Fatalf("verdicts = %+v, want one clean — the verdict must still be produced", got)
		}
		if stamp != nil {
			t.Errorf("a stamp was produced from a version that was never read (%+v) — claiming a "+
				"signature version we did not read is the lie; leaving it NULL is honest and "+
				"re-scan's IS NULL arm picks the row up", stamp)
		}
	}
	if fs.versions != 1 {
		t.Errorf("VERSION was retried %d times across 4 messages, want 1 — keying the cache on "+
			"'we have a version' rather than 'we asked recently' reintroduces the per-message "+
			"round trip exactly when the daemon is unwell", fs.versions)
	}
}
