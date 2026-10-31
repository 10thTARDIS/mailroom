// SPDX-License-Identifier: AGPL-3.0-only

// Package clamd is Mailroom's client for the ClamAV daemon (#394, ADR-027).
//
// It is an anti-corruption layer (spec §3): callers see Mailroom's own verdict
// vocabulary, never clamd's wire strings. That matters more here than usual,
// because clamd's protocol reports three genuinely different situations through
// one channel and the difference between them is the difference between "this
// attachment is quarantined" and "this account's mail has stopped arriving".
//
// # Everything below was MEASURED against clamav/clamav:latest, not assumed
//
// ADR-027 §4b and §5a both flagged parts of this as judgment to settle before
// coding. The probes (2026-09-01):
//
//	INSTREAM 1 KB clean          -> "stream: OK\0"
//	INSTREAM EICAR               -> "stream: Eicar-Test-Signature FOUND\0"
//	INSTREAM encrypted zip       -> "stream: Heuristics.Encrypted.Zip FOUND\0"
//	INSTREAM oversize-in-archive -> "stream: Heuristics.Limits.Exceeded.MaxFileSize FOUND\0"
//	INSTREAM 110 MB              -> "INSTREAM size limit exceeded. ERROR\0"
//	                                AND the connection closes mid-write
//	PING immediately after       -> "PONG\0"
//
// Three findings shaped this file:
//
//  1. READ THE RESPONSE EVEN AFTER THE WRITE FAILS. Exceeding StreamMaxLength
//     gives a broken pipe on write, with the explanatory error string already
//     in the socket buffer.
//
//     ⚠ AN EARLIER VERSION OF THIS COMMENT CLAIMED THIS WAS WHAT PREVENTED A
//     FAKE OUTAGE. That was wrong, and the probe that was supposed to prove it
//     went green: skipping the read still yields Unknown, because Scan's PING
//     runs next and the daemon answers. **PING is the mechanism that keeps a
//     per-file failure from becoming an outage** (§4b step 2), not this read.
//
//     What the read actually buys is the REASON. Without it the operator sees
//     "broken pipe" instead of "INSTREAM size limit exceeded", which is the
//     difference between a diagnosable log and a mystery. It also makes the
//     verdict correct if PING ever became unavailable. Worth doing — but it is
//     a diagnosability property, and the safety property lives elsewhere.
//
//  2. ⚠ StreamMaxLength IS 100 MB IN THE OFFICIAL IMAGE, not the 25 MB the ADR
//     and cmd/mailroom-admin/attachmentreport.go both assume (clamconf reports
//     104857600). 25 MB is upstream's documented default; it is not what a
//     deployed clamd enforces. See MaxStreamBytes.
//
//  3. ⚠ STOCK CLAMAV CANNOT PRODUCE "unknown". AlertExceedsMax and
//     AlertEncrypted are BOTH disabled by default, so a too-deep archive or an
//     encrypted zip returns plain OK — a clean verdict on a file nothing looked
//     inside. Enabling them turns those into DETECTIONS, which a naive mapping
//     would quarantine as malware. Hence the family prefixes below.
package clamd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// State is Mailroom's verdict vocabulary. It deliberately does not include
// `unscanned` or `released`: those are STORE states (no scanner ran; a human
// overrode a result), not things a scan can return.
type State string

const (
	// Clean means clamd looked and found nothing. ⚠ Only meaningful with the
	// signature version and a timestamp beside it (ADR-027 §1a, §5c): it is a
	// claim about what the signatures knew, not a property of the file.
	Clean State = "clean"
	// Flagged means clamd matched a real signature.
	Flagged State = "flagged"
	// Unknown means a scan was attempted or deliberately skipped and no
	// determination could be made. ⚠ ADR-027 §5b assigns it the SAME
	// enforcement as Flagged — otherwise an attacker never has to beat the
	// scanner, only avoid it, which is commodity technique. That enforcement
	// is §8's download gate, and as of increment 5 (#396) it EXISTS: a part in
	// this state is refused at the download and preview routes, and cannot be
	// attached to an outgoing draft, until the user releases its content.
	Unknown State = "unknown"
)

// Verdict is one attachment's scan outcome.
type Verdict struct {
	State State
	// Signature is clamd's own name for what it matched ("Eicar-Test-Signature",
	// "Heuristics.Encrypted.Zip"). Kept verbatim for the operator's logs and
	// never shown to a user as-is.
	Signature string
	// Reason explains an Unknown in Mailroom's words, for the operator.
	Reason string
}

// ErrDaemonUnreachable means the daemon itself did not answer — established by
// a PING that failed AFTER a scan error, never inferred from the scan error
// alone.
//
// ⚠ IT IS DELIBERATELY NOT internal/ingest's ErrQuotaExhausted, and that is not
// cosmetic. The sync engine matches on the sentinel and builds account health
// from err.Error(); reusing quota's would report "over storage quota" during a
// scanner outage — a false statement to the operator at the exact moment they
// need a true one (ADR-027 §4c).
var ErrDaemonUnreachable = errors.New("clamd: daemon unreachable; mail left on the server unscanned")

// MaxStreamBytes is the largest payload this client will send.
//
// ⚠ MEASURED, NOT DOCUMENTED. clamav/clamav:latest reports
// StreamMaxLength = 104857600 (100 MB). Upstream's *documented* default is
// 25 MB and that is what ADR-027 and attachmentreport.go say; the deployed
// daemon does not enforce it.
//
// It is configurable because it MUST match the daemon's own setting. A
// mismatch is safe in both directions but wasteful in one: lower than clamd's
// means files it could have scanned become Unknown without being asked; higher
// means a wasted round trip that ends in the size-limit error, which maps to
// Unknown anyway.
const MaxStreamBytes = 100 * 1024 * 1024

// Timeouts. ⚠ ADR-027 §4c: there is NO WithTimeout or WithDeadline anywhere in
// syncrunner, mailsync or ingest today. A clamd that accepts a connection and
// then stalls would hang the sync goroutine forever — the pass never returns,
// recordHealth never runs, and NO ERROR IS EVER RECORDED. A hung scanner is
// strictly worse than a dead one, because a dead one is visible.
//
// Generous on purpose: clamd stalls for seconds while reloading its signature
// database, and treating a reload as an outage would refuse mail for no reason.
const (
	DefaultDialTimeout = 10 * time.Second
	DefaultScanTimeout = 90 * time.Second
	DefaultPingTimeout = 5 * time.Second
)

// Heuristic families that mean "could not determine", not "malicious".
//
// ⚠ THIS MAPPING IS THE DIFFERENCE BETWEEN FOUR STATES AND THREE. With
// AlertExceedsMax/AlertEncrypted enabled — which ADR-027 §5a requires, because
// without them these cases return plain OK — clamd reports both as DETECTIONS.
// Mapping them to Flagged would quarantine every password-protected zip as
// malware; not enabling them at all would call an unopened archive clean.
//
// Prefixes verified against real clamd output, not documentation:
//
//	Heuristics.Encrypted.Zip
//	Heuristics.Limits.Exceeded.MaxFileSize
var unknownSignaturePrefixes = []string{
	"Heuristics.Encrypted.",
	"Heuristics.Limits.Exceeded.",
}

// Client talks to one clamd.
type Client struct {
	// Network and Address are as net.Dial takes them. In deployment this is a
	// unix socket, which is what lets the sidecar run with no network at all
	// (ADR-027 §10b) — unlike the crypto process, which needs the database and
	// therefore an internal network instead (#385).
	Network string
	Address string

	DialTimeout time.Duration
	ScanTimeout time.Duration
	MaxBytes    int64
}

func (c *Client) dialTimeout() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return DefaultDialTimeout
}

func (c *Client) scanTimeout() time.Duration {
	if c.ScanTimeout > 0 {
		return c.ScanTimeout
	}
	return DefaultScanTimeout
}

func (c *Client) maxBytes() int64 {
	if c.MaxBytes > 0 {
		return c.MaxBytes
	}
	return MaxStreamBytes
}

func (c *Client) dial(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, c.Network, c.Address)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	return conn, nil
}

// Ping reports whether the daemon answers. It is the ONLY way this package
// concludes "outage" (ADR-027 §4b step 2): a scan error is never enough,
// because the most dangerous per-file failures — oversize, and a file that
// crashes the scanner — present as connection-level errors.
func (c *Client) Ping(ctx context.Context) error {
	conn, err := c.dial(ctx, DefaultPingTimeout)
	if err != nil {
		return fmt.Errorf("clamd: ping dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(DefaultPingTimeout))
	if _, err := conn.Write([]byte("zPING\x00")); err != nil {
		return fmt.Errorf("clamd: ping write: %w", err)
	}
	b, _ := io.ReadAll(conn)
	if !strings.Contains(string(b), "PONG") {
		return fmt.Errorf("clamd: ping got %q, want PONG", strings.TrimRight(string(b), "\x00"))
	}
	return nil
}

// Scan sends one attachment's bytes and returns Mailroom's verdict.
//
// ⚠ THE ERROR RETURN MEANS OUTAGE, OR THE CALLER GAVE UP. NOTHING ELSE. Every
// per-file problem — too large, unreadable, malformed, too slow, a file that
// kills the daemon — comes back as a Verdict with State Unknown and a nil
// error, so ingest proceeds. Only a daemon that fails to answer a PING produces
// ErrDaemonUnreachable, and only that refuses the sync pass. ADR-027 §4b: "a
// single message must never pause the account", because the alternative is a
// remote kill switch operated by whoever sends the mail.
//
// The one other error is the caller's own context ending (shutdown), returned
// as itself so a restart is never recorded as a scanner outage.
func (c *Client) Scan(ctx context.Context, data []byte) (Verdict, error) {
	// Step 1: size pre-check, BEFORE contacting the daemon. Cheap, and it keeps
	// the common oversize case off the wire entirely.
	if int64(len(data)) > c.maxBytes() {
		return Verdict{
			State:  Unknown,
			Reason: fmt.Sprintf("attachment is %d bytes, over the %d-byte scan limit", len(data), c.maxBytes()),
		}, nil
	}

	scanCtx, cancel := context.WithTimeout(ctx, c.scanTimeout())
	defer cancel()

	resp, scanErr := c.instream(scanCtx, data)
	if scanErr != nil {
		// ⚠ THE CALLER'S CONTEXT ENDING IS NOT AN OUTAGE. A shutdown cancels
		// ctx mid-scan; reporting that as ErrDaemonUnreachable would make every
		// restart look like a scanner failure in account health.
		if ctx.Err() != nil {
			return Verdict{}, fmt.Errorf("clamd: scan abandoned: %w", ctx.Err())
		}
		// Step 2: a scan error is NOT an outage until PING says so.
		//
		// ⚠ IT PINGS ON ctx, NOT scanCtx, AND THAT IS THE WHOLE POINT. scanCtx
		// is the one that just expired: a file that stalls the daemon past
		// ScanTimeout leaves it Done, so a PING issued on it fails INSTANTLY
		// with "context deadline exceeded" against a daemon that is answering
		// perfectly. That would report a per-file timeout as a service outage —
		// and because the sync engine refuses the whole pass on this sentinel
		// and the cursor never advances, the same file would be retried first
		// on every subsequent pass forever. A remote kill switch operated by
		// whoever sends the mail, which is precisely what ADR-027 §4b forbids.
		if perr := c.Ping(ctx); perr != nil {
			return Verdict{}, fmt.Errorf("%w (scan: %v; ping: %v)", ErrDaemonUnreachable, scanErr, perr)
		}
		// Daemon alive: the failure belongs to this file, not the service.
		return Verdict{
			State:  Unknown,
			Reason: fmt.Sprintf("scan failed but the daemon is alive: %v", scanErr),
		}, nil
	}
	return parseResponse(resp)
}

// instream performs the INSTREAM exchange and returns clamd's reply.
//
// ⚠ IT ALWAYS READS, EVEN WHEN THE WRITE FAILED. Measured: sending 110 MB to a
// 100 MB daemon yields "INSTREAM size limit exceeded. ERROR" AND a broken pipe
// on the next write. Returning on the write error would discard the
// explanation and leave the caller with a bare "broken pipe".
//
// ⚠ This is about the REASON, not the state: Scan's PING already keeps an
// oversize file from being read as an outage. Probed — skipping this read
// leaves the verdict correct and only the log worse.
func (c *Client) instream(ctx context.Context, data []byte) (string, error) {
	conn, err := c.dial(ctx, c.dialTimeout())
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return "", fmt.Errorf("write command: %w", err)
	}

	const chunk = 64 * 1024
	var writeErr error
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(end-off))
		if _, err := conn.Write(hdr[:]); err != nil {
			writeErr = fmt.Errorf("write chunk header at %d: %w", off, err)
			break
		}
		if _, err := conn.Write(data[off:end]); err != nil {
			writeErr = fmt.Errorf("write chunk at %d: %w", off, err)
			break
		}
	}
	if writeErr == nil {
		var terminator [4]byte
		if _, err := conn.Write(terminator[:]); err != nil {
			writeErr = fmt.Errorf("write terminator: %w", err)
		}
	}

	body, readErr := io.ReadAll(conn)
	resp := strings.TrimRight(string(body), "\x00\n")

	// A reply, even alongside a write error, is the authoritative answer.
	if resp != "" {
		return resp, nil
	}
	if writeErr != nil {
		return "", writeErr
	}
	if readErr != nil {
		return "", fmt.Errorf("read reply: %w", readErr)
	}
	return "", errors.New("empty reply")
}

// parseResponse maps clamd's wire vocabulary to Mailroom's.
func parseResponse(resp string) (Verdict, error) {
	switch {
	case strings.HasSuffix(resp, "OK"):
		return Verdict{State: Clean}, nil

	case strings.HasSuffix(resp, "ERROR"):
		// ⚠ MEASURED: this is how an oversize stream reports, and it arrives
		// with a closed connection. It is per-file, never an outage — the
		// caller has already established the daemon is alive by this point.
		return Verdict{
			State:  Unknown,
			Reason: "clamd refused the stream: " + resp,
		}, nil

	case strings.HasSuffix(resp, "FOUND"):
		sig := signatureOf(resp)
		for _, p := range unknownSignaturePrefixes {
			if strings.HasPrefix(sig, p) {
				// A heuristic, not a signature: clamd is telling us it could
				// not look inside, in the only channel it has.
				return Verdict{
					State:     Unknown,
					Signature: sig,
					Reason:    "clamd could not fully examine this: " + sig,
				}, nil
			}
		}
		return Verdict{State: Flagged, Signature: sig}, nil
	}

	// An unrecognised reply is NOT clean. Defaulting to Clean here would make
	// every future protocol change silently safe-looking.
	return Verdict{
		State:  Unknown,
		Reason: "unrecognised clamd reply: " + resp,
	}, nil
}

// signatureOf pulls the signature name out of "stream: NAME FOUND".
func signatureOf(resp string) string {
	s := strings.TrimSuffix(resp, " FOUND")
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return strings.TrimSpace(s)
}
