// SPDX-License-Identifier: AGPL-3.0-only

package rescan

import (
	"context"
	"errors"
	"testing"
	"time"

	"mailroom/internal/clamd"
	"mailroom/internal/mime"
)

// ⚠ THE TWIN OF internal/ingest/scan_test.go, AND IT MUST STAY ONE.
//
// The rules themselves live in internal/attachscan and are pinned there. What
// these cover is that THIS loop applies all of them — because the two loops are
// deliberately separate (ingest is fail-closed with a circuit breaker, rescan is
// best-effort), and a rule applied by one loop and not the other would make an
// attachment's verdict depend on which path touched it last.
//
// If you add a rule to one of these tables, add it to the other.

type fakeScanner struct {
	verdict clamd.Verdict
	err     error
	scans   int
}

func (f *fakeScanner) Scan(context.Context, []byte) (clamd.Verdict, error) {
	f.scans++
	if f.err != nil {
		return clamd.Verdict{}, f.err
	}
	return f.verdict, nil
}
func (f *fakeScanner) Version(context.Context) (clamd.Version, error) {
	return clamd.Version{Engine: "1.5.4", Database: 28110}, nil
}

func parse(t *testing.T, raw string) *mime.Message {
	t.Helper()
	m, err := mime.ParseBytes([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

const withAttachment = "From: a@example.com\r\nSubject: s\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
	"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
	"--b\r\nContent-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"f.bin\"\r\n\r\nPAYLOAD\r\n--b--\r\n"

const encryptedBody = "From: a@example.com\r\nSubject: s\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=b\r\n\r\n" +
	"--b\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
	"--b\r\nContent-Type: application/octet-stream\r\n\r\nciphertext\r\n--b--\r\n"

// An ordinary message with its own attachment that also carries an encrypted
// message forwarded as message/rfc822 (#405). The outer attachment is plaintext.
const forwardedEncrypted = "From: a@example.com\r\nSubject: fwd\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=o\r\n\r\n" +
	"--o\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
	"--o\r\nContent-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=\"r.pdf\"\r\n\r\n%PDF-1.4\r\n" +
	"--o\r\nContent-Type: message/rfc822\r\n\r\n" + encryptedBody + "\r\n--o--\r\n"

// Labelled signed-data, but the label is the sender's word (#405).
const opaqueSigned = "From: a@example.com\r\nSubject: s\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: application/pkcs7-mime; smime-type=signed-data; name=smime.p7m\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\nMIIDVgYJKoZIhvcNAQcC\r\n"

const emptyPart = "From: a@example.com\r\nSubject: s\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
	"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
	"--b\r\nContent-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"e.bin\"\r\n\r\n--b--\r\n"

func TestScanPartsAppliesEveryForcedUnknownRule(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		truncated bool
		wantState string
		wantScans int
	}{
		{name: "ordinary", raw: withAttachment, wantState: "clean", wantScans: 1},
		// §6: ciphertext must never be blessed, and must not be sent.
		{name: "encrypted body", raw: encryptedBody, wantState: "unknown", wantScans: 0},
		// #405: smime-type is sender-written, so signed-data is still held.
		{name: "pkcs7-mime labelled signed-data", raw: opaqueSigned, wantState: "unknown", wantScans: 0},
		// §5a: a prefix cannot be clean.
		{name: "truncated walk", raw: withAttachment, truncated: true, wantState: "unknown", wantScans: 0},
		// clamd says OK to an empty stream, so zero bytes must not reach it.
		{name: "empty part", raw: emptyPart, wantState: "unknown", wantScans: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := parse(t, tc.raw)
			m.Truncated = tc.truncated
			fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}

			got, err := ScanParts(context.Background(), fs, m)
			if err != nil {
				t.Fatalf("ScanParts: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("no verdicts; this case proves nothing")
			}
			for i, v := range got {
				if v.Status != tc.wantState {
					t.Errorf("part %d (%s) = %q, want %q", i, v.PartPath, v.Status, tc.wantState)
				}
			}
			// ⚠ Agreeing on the verdict while disagreeing on whether to ASK is
			// the same answer for opposite reasons, and only one of them is the
			// rule §5a and §6 describe.
			if fs.scans != tc.wantScans {
				t.Errorf("daemon calls = %d, want %d", fs.scans, tc.wantScans)
			}
		})
	}
}

// ⚠ A SCAN ERROR ABANDONS THE WHOLE MESSAGE RATHER THAN PART-WRITING IT.
// Stamping the parts that happened to succeed would advance their version while
// the rest stayed behind — the message would look partly fresh, and the next
// pass would redo it anyway. Keeping the message a single unit of work is what
// makes an interrupted chunk self-heal.
func TestScanPartsAbandonsTheMessageOnAScanError(t *testing.T) {
	fs := &fakeScanner{err: errors.New("connection reset")}
	got, err := ScanParts(context.Background(), fs, parse(t, withAttachment))
	if err == nil {
		t.Fatalf("ScanParts returned %+v and no error; a partial message would be stamped as fresh", got)
	}
	if got != nil {
		t.Errorf("ScanParts returned %d verdicts alongside an error; nothing may be written", len(got))
	}
	// Control: the same call with a working scanner DOES produce verdicts, so
	// the assertion above is about the error and not about the fixture.
	ok := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}
	if v, err := ScanParts(context.Background(), ok, parse(t, withAttachment)); err != nil || len(v) == 0 {
		t.Fatalf("the control failed (%v, %d verdicts); the assertion above proves nothing", err, len(v))
	}
}

// A message with nothing to scan costs nothing and touches no daemon.
func TestScanPartsSkipsMessagesWithNoAttachments(t *testing.T) {
	fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}
	got, err := ScanParts(context.Background(), fs, parse(t, "From: a@example.com\r\nSubject: s\r\n\r\ntext\r\n"))
	if err != nil || got != nil {
		t.Fatalf("ScanParts = (%v, %v), want (nil, nil)", got, err)
	}
	if fs.scans != 0 {
		t.Errorf("daemon calls = %d for a message with no attachments", fs.scans)
	}
}

// ── The reschedule interval (#415, #409) ─────────────────────────────────────
//
// ⚠ #415 ASKS FOR THE ASSERTION TO BE ON THE LOOP, NOT THE VERDICT, and it is
// right: after a failed pass the stored rows look identical whether the job
// comes back in thirty seconds or in a day. The interval is the only thing that
// distinguishes a drain from a spin, so it is the thing asserted.
func TestAPassThatChangedNothingBacksOffInsteadOfSpinning(t *testing.T) {
	w := New(nil, nil, nil, 24*time.Hour, nil, nil)

	for _, tc := range []struct {
		name      string
		remaining int
		moved     bool
		want      time.Duration
		why       string
	}{
		{
			name: "a draining backlog comes back promptly", remaining: 40, moved: true,
			want: idleAfterChunk,
			why: "this is what idleAfterChunk is FOR — removing it would make a real backlog take " +
				"a day per chunk, which is the opposite failure and just as silent",
		},
		{
			name: "a pass that moved nothing backs off", remaining: 40, moved: false,
			want: 24 * time.Hour,
			why: "nothing left the queue, so coming back in 30s re-reads, re-decrypts and re-parses " +
				"the same messages forever while starving submit on the shared dispatcher",
		},
		{
			name: "an empty queue waits out the ordinary interval", remaining: 0, moved: true,
			want: 24 * time.Hour,
			why:  "the tiers are upper bounds on frequency; a tighter loop re-asks a settled question",
		},
		{
			name: "an empty queue that moved nothing also waits", remaining: 0, moved: false,
			want: 24 * time.Hour,
			why:  "no work and no movement is the ordinary idle state, not a fault",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.nextFireIn(tc.remaining, tc.moved); got != tc.want {
				t.Errorf("nextFireIn(%d, %v) = %v, want %v — %s", tc.remaining, tc.moved, got, tc.want, tc.why)
			}
		})
	}
}

// #405: a forwarded encrypted message is unknown itself but does not poison the
// outer message's own attachment, which is scanned and clean.
func TestScanPartsForwardedEncryptedDoesNotPoisonSiblings(t *testing.T) {
	m := parse(t, forwardedEncrypted)
	fs := &fakeScanner{verdict: clamd.Verdict{State: clamd.Clean}}
	got, err := ScanParts(context.Background(), fs, m)
	if err != nil {
		t.Fatalf("ScanParts: %v", err)
	}
	byType := map[string]string{}
	for _, v := range got {
		byType[v.PartPath] = v.Status
	}
	if len(got) != 2 {
		t.Fatalf("verdicts = %+v, want 2 (the PDF and the forwarded message)", got)
	}
	unknown, clean := 0, 0
	for _, st := range byType {
		switch st {
		case "unknown":
			unknown++
		case "clean":
			clean++
		}
	}
	if unknown != 1 || clean != 1 || fs.scans != 1 {
		t.Errorf("unknown=%d clean=%d scans=%d, want 1/1/1 (%+v)", unknown, clean, fs.scans, got)
	}
}
