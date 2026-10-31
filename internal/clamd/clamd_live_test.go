//go:build clamav

// SPDX-License-Identifier: AGPL-3.0-only

package clamd

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// Live tests against a REAL clamd, behind the `clamav` build tag — the same
// shape as the GnuPG interop suite (#263), and for the same reason: a protocol
// client verified only against strings its own author wrote proves that the
// author is self-consistent, not that the protocol was understood.
//
// ⚠ THESE FAIL, THEY DO NOT SKIP, when the daemon is configured wrongly. A
// suite that skips on a misconfigured scanner is a suite that goes green in
// exactly the situation it exists to detect.
//
// Run via scripts/clamav-test.sh, which stands up a daemon with the mandatory
// AlertExceedsMax/AlertEncrypted settings (ADR-027 §5a).

func liveClient(t *testing.T) *Client {
	t.Helper()
	addr := os.Getenv("MAILROOM_TEST_CLAMD_ADDR")
	if addr == "" {
		t.Fatal("MAILROOM_TEST_CLAMD_ADDR is unset — these tests require a real clamd " +
			"and must not silently pass without one")
	}
	network := os.Getenv("MAILROOM_TEST_CLAMD_NET")
	if network == "" {
		network = "tcp"
	}
	return &Client{Network: network, Address: addr}
}

// eicar is the standard antivirus test string, assembled at runtime so this
// source file is not itself a file every scanner on the planet quarantines.
func eicar() []byte {
	return []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$` + `EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`)
}

func TestLiveCleanFileIsClean(t *testing.T) {
	c := liveClient(t)
	v, err := c.Scan(t.Context(), []byte("an entirely ordinary attachment"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if v.State != Clean {
		t.Errorf("State = %q (%s), want %q", v.State, v.Reason, Clean)
	}
}

// The positive control for detection. If this does not flag, every other
// assertion in this file is meaningless — a daemon with no signatures loaded
// would return OK for everything and the suite would look healthy.
func TestLiveEicarIsFlagged(t *testing.T) {
	c := liveClient(t)
	v, err := c.Scan(t.Context(), eicar())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if v.State != Flagged {
		t.Fatalf("State = %q (%s), want %q — the daemon may have no signature database loaded, "+
			"which would make every other test in this file vacuous", v.State, v.Reason, Flagged)
	}
	if v.Signature == "" {
		t.Error("flagged with no signature name — the operator cannot tell what matched")
	}
}

// ⚠ Commodity evasion, not an exotic case: an encrypted archive with the
// password in the message body. It must be Unknown — clamd could not look
// inside — and NOT Flagged, which would quarantine every legitimate
// password-protected zip as malware.
func TestLiveEncryptedArchiveIsUnknownNotFlagged(t *testing.T) {
	c := liveClient(t)
	// A minimal ZIP with the encryption bit set. Go's archive/zip cannot write
	// encrypted entries, so the general-purpose flag is set by hand — which is
	// all clamd's heuristic looks at.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "secret.txt", Method: zip.Store, Flags: 0x1})
	if err != nil {
		t.Fatalf("zip header: %v", err)
	}
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	v, err := c.Scan(t.Context(), buf.Bytes())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if v.State == Flagged {
		t.Fatalf("an encrypted archive was FLAGGED (%s) — every password-protected zip "+
			"would be quarantined as malware", v.Signature)
	}
	if v.State != Unknown {
		t.Fatalf("State = %q (%s), want %q. If this is Clean, AlertEncrypted is not enabled "+
			"and ADR-027 §5a's four-state model does not exist on this daemon",
			v.State, v.Reason, Unknown)
	}
}

// ⚠ THE ONE THAT MATTERS MOST, and the reason this file exists.
//
// Exceeding StreamMaxLength makes clamd reply "INSTREAM size limit exceeded.
// ERROR" **and close the connection mid-write**. A client that returns on the
// write error reports a bare broken pipe, which its own taxonomy then has to
// treat as a possible outage — and an outage refuses the whole sync pass. That
// is a remote kill switch operated by whoever sends the mail (ADR-027 §4b).
//
// This sends MORE than the daemon accepts while our own pre-check is set higher
// still, so the wire path is genuinely exercised rather than short-circuited.
func TestLiveOversizeStreamIsPerFileUnknownNotAnOutage(t *testing.T) {
	c := liveClient(t)
	c.MaxBytes = 512 * 1024 * 1024 // deliberately above the daemon's limit
	c.ScanTimeout = 3 * time.Minute

	// Incompressible, so nothing downstream can shrink it below the limit.
	payload := make([]byte, 110*1024*1024)
	if _, err := rand.Read(payload[:1024]); err != nil {
		t.Fatalf("seed: %v", err)
	}

	v, err := c.Scan(t.Context(), payload)
	if errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("an oversize attachment was reported as a DAEMON OUTAGE: %v\n"+
			"That halts ingest for the account at a stranger's choosing — the exact "+
			"failure ADR-027 §4b exists to prevent.", err)
	}
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if v.State != Unknown {
		t.Errorf("State = %q (%s), want %q", v.State, v.Reason, Unknown)
	}

	// ⚠ AND THE REASON MUST NAME THE SIZE LIMIT. Asserting only on the state
	// does NOT cover the read-after-write-error rule: probed, and skipping that
	// read still yields Unknown, because Scan's PING runs next and the daemon
	// answers. The state is protected by PING; only the reason is protected by
	// the read. Without this assertion the operator gets "broken pipe" for an
	// oversize attachment and nothing goes red.
	if !strings.Contains(v.Reason, "size limit exceeded") {
		t.Errorf("Reason = %q, want it to name the size limit — the client is "+
			"discarding clamd's explanation and reporting a bare transport error", v.Reason)
	}

	// And the daemon must still be usable immediately afterwards — proving the
	// connection close was per-request, not a crash.
	if err := c.Ping(t.Context()); err != nil {
		t.Errorf("daemon did not answer PING after an oversize stream: %v", err)
	}
}

// The outage path, proven rather than assumed. A dead daemon MUST produce
// ErrDaemonUnreachable — that sentinel is what makes the sync engine withhold
// cursors and skip the reaper, so unscanned mail is left on the server instead
// of appearing deleted (ADR-027 §4c).
func TestLiveDeadDaemonIsAnOutage(t *testing.T) {
	// A port nothing listens on, on the same host the live daemon runs on.
	c := &Client{Network: "tcp", Address: "127.0.0.1:1", DialTimeout: 2 * time.Second}
	_, err := c.Scan(context.Background(), []byte("anything"))
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("Scan against a dead daemon returned %v, want ErrDaemonUnreachable — "+
			"without that sentinel the sync engine cannot tell a scanner outage from "+
			"a storage-quota pause, and account health would report the wrong cause", err)
	}
}

// A verdict must never be Clean by default. This drives the client at a socket
// that accepts and then says nothing, which is the shape of a stalled daemon.
func TestLiveStalledDaemonDoesNotReturnClean(t *testing.T) {
	ln, err := listenLocal()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		// Accept, then stall — never reply, never close.
		time.Sleep(30 * time.Second)
		_ = conn.Close()
	}()

	c := &Client{Network: "tcp", Address: ln.Addr().String(), ScanTimeout: 2 * time.Second}
	v, err := c.Scan(context.Background(), []byte("payload"))
	if err == nil && v.State == Clean {
		t.Fatal("a stalled daemon produced a CLEAN verdict — a hang must never read as safe")
	}
}

// listenLocal opens a loopback listener for the stall test.
func listenLocal() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

// ⚠ THE FRESHNESS FACTS, AGAINST A REAL DAEMON (#395, ADR-027 §5c).
//
// parseVersion is unit-tested against a string this author captured, which
// proves the parser matches that capture and nothing more. This proves the
// capture still describes the protocol — and it is the whole basis of the
// re-scan predicate and the DB-age fault, so a format change here silently
// freezes re-scanning rather than breaking it loudly.
func TestLiveVersionReportsEngineDatabaseAndAge(t *testing.T) {
	v, err := liveClient(t).Version(t.Context())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.Engine == "" {
		t.Error("no engine version — §5c's re-scan on an engine MAJOR change has nothing to compare")
	}
	if v.Database <= 0 {
		t.Errorf("Database = %d, want a real signature version; the re-scan predicate compares against this", v.Database)
	}

	// ⚠ THE PART THAT CANNOT BE FAKED BY A PARSER TEST. §5c's whole fail-open
	// problem is that "the database has not moved" and "the database CANNOT
	// move" look identical without an age. A daemon that reports no publication
	// time would leave every re-scan tier gated on a version that can never
	// change, with nothing to notice it.
	age, ok := v.Age(time.Now())
	if !ok {
		t.Fatalf("the daemon reported NO publication timestamp (%+v) — DB age is unmeasurable, "+
			"so a freshclam that stopped updating would be invisible", v)
	}
	if age < 0 {
		t.Errorf("database published in the future: age = %v", age)
	}
	t.Logf("engine=%s database=%d published=%s age=%s stale=%v",
		v.Engine, v.Database, v.Published.Format(time.RFC3339), age.Truncate(time.Minute), v.Stale(time.Now()))
}

// Version must report an outage the same way Scan does, or a scanner that has
// gone away reads as "engine unknown, database 0" — which is indistinguishable
// from a daemon whose signatures never loaded, and would be acted on as a
// freshness fault rather than an outage.
func TestLiveVersionOnADeadDaemonIsAnOutage(t *testing.T) {
	dead := &Client{Network: "unix", Address: "/nonexistent/clamd.sock"}
	if _, err := dead.Version(t.Context()); !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("Version against a dead daemon = %v, want ErrDaemonUnreachable", err)
	}
	// Positive control: the same call against the LIVE daemon succeeds, so the
	// assertion above is about the daemon being dead and not about the method.
	if _, err := liveClient(t).Version(t.Context()); err != nil {
		t.Fatalf("the control failed: %v", err)
	}
}
