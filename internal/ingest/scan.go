// SPDX-License-Identifier: AGPL-3.0-only

package ingest

// Malware scanning at ingest (#394, ADR-027 §4, §4b, §4c, §5a, §6, §10c).
//
// # Where the scan happens, and why it is not inside the transaction
//
// scanMessage runs BEFORE Store.UserTx opens. clamd takes 32–366 ms per
// attachment (ADR-027 §4a, measured on DISTINCT files — the first measurement
// timed the same file 50 times and read clamd's content-hash cache as a scan),
// and holding a Postgres transaction open across that would put scanner latency
// inside the database's lock and snapshot horizon. The MIME parse already
// happens out here for the same reason. The verdicts it produces are written
// INSIDE the transaction, alongside the rows they describe, so a message can
// never be visible with its parts unjudged.
//
// # The taxonomy, which is the part that makes synchronous scanning safe
//
// There are exactly two outcomes: a per-part verdict, or a refusal of the whole
// pass. Getting the boundary wrong in either direction is expensive.
//
//   - Treat an outage as per-file → mail is ingested and marked `unknown` while
//     the scanner is merely down, and the fail-closed guarantee is decorative.
//   - Treat a per-file failure as an outage → cursors never advance, so the same
//     message is re-fetched first on every pass forever. All ingest for the
//     account halts at a stranger's choosing (ADR-027 §4b: a remote kill switch,
//     strictly worse than the malware it would catch).
//
// internal/clamd already draws most of that line: only a daemon that fails to
// answer a PING yields ErrDaemonUnreachable. What it CANNOT see is the case
// where scanning a file kills the daemon, because then the error genuinely is
// connection-level and the PING genuinely fails. That is what the circuit
// breaker below is for, and it is the only thing here that is not a
// straightforward mapping.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"mailroom/internal/attachscan"
	"mailroom/internal/clamd"
	"mailroom/internal/mime"
	"mailroom/internal/store"
)

// ErrScannerUnavailable is the sentinel Ingest returns when the malware scanner
// is unreachable. It is the SECOND load-bearing pause signal, alongside
// ErrQuotaExhausted, and it is deliberately a distinct one.
//
// ⚠ REUSING ErrQuotaExhausted WOULD BE A LIE TOLD TO THE OPERATOR. The sync
// engine matches on the sentinel and builds account health out of err.Error()
// (syncrunner.recordHealth), so a scanner outage would surface as "paused: over
// the storage quota" — sending them to look at disk usage while a container is
// down. ADR-027 §4c requires a sentinel that names the scanner and says the mail
// is safe where it is.
//
// ⚠ AND THE PAUSE HAS A SECOND HALF THAT IS EASY TO MISS. Withholding cursors
// makes the refusal a retry; SKIPPING THE REAPER is what stops "we could not
// scan this" from becoming "the user deleted this". mailsync.SyncAccountWith
// returns early on this sentinel for exactly that reason — see
// mailsync/scanner_test.go, whose negative control proves the reaper would
// otherwise run on the account's OTHER mailboxes while this one refused.
var ErrScannerUnavailable = errors.New("ingest: paused: the malware scanner is unreachable")

// ScannerUnavailableError carries the underlying transport failure behind an
// ErrScannerUnavailable so account health can name it. It satisfies both
// errors.Is(err, ErrScannerUnavailable) and errors.Is(err, clamd.ErrDaemonUnreachable).
type ScannerUnavailableError struct {
	// Cause is the scanner client's own error, kept so the operator reads
	// "connection refused" rather than a sanitised summary.
	Cause error
	// Consecutive is how many attempts have now failed on THIS message with the
	// daemon proven alive beforehand — the circuit breaker's count. It is in
	// the health string because "the scanner is down" and "one message has
	// taken the daemon down twice and will be waved through as unknown on the
	// third" are different operational situations with different fixes.
	Consecutive int
}

func (e *ScannerUnavailableError) Error() string {
	return fmt.Sprintf(
		"paused: the malware scanner is unreachable, so mail is left on the provider — unscanned, and not deleted (attributable failures for this message: %d; %v)",
		e.Consecutive, e.Cause)
}

func (e *ScannerUnavailableError) Is(target error) bool { return target == ErrScannerUnavailable }

// Unwrap exposes the client's own error, so errors.Is(err, clamd.ErrDaemonUnreachable)
// still answers at the seam where that distinction matters.
func (e *ScannerUnavailableError) Unwrap() error { return e.Cause }

// Scanner is the malware scanner ingest depends on, named as an interface here
// rather than taking *clamd.Client directly — the same anti-corruption reasoning
// as LabelClassifier, and the same testability argument: the interesting states
// of a scanner (dead, alive-then-dead, flagging) are a few lines of fake and
// hours of container.
//
// Ping is part of the interface rather than an implementation detail because
// ingest needs it for a decision the client cannot make on its own — see
// scanBreaker.
//
// Version is here for #407: a verdict is a claim about what the signatures knew,
// and a verdict written without that number is one `attachment-report` has to
// count as "never scanned".
type Scanner interface {
	Scan(ctx context.Context, data []byte) (clamd.Verdict, error)
	Ping(ctx context.Context) error
	Version(ctx context.Context) (clamd.Version, error)
}

// scanVersionTTL is how long a read of the daemon's VERSION is reused across
// messages.
//
// ⚠ IT IS A CACHE BECAUSE INGEST RUNS INSIDE THE SYNC PATH, and a socket round
// trip per message would put a fixed cost on every delivery to save a number
// that changes a few times a day. freshclam publishes on the order of hours;
// a minute of staleness is far inside that.
//
// ⚠ AND A STALE READ IS THE SAFE DIRECTION, WHICH IS WHY A CACHE IS ALLOWED AT
// ALL. The cached value can only be OLDER than the truth, so a row stamped from
// it stays behind the new current and re-scan picks it up. Reading the version
// AFTER the scan is the failure mode #407 names: those rows would claim a scan
// against signatures that were not loaded when the bytes went through, look
// current, and stop being re-scanned.
const scanVersionTTL = time.Minute

// scanVersion returns the signature-database version to stamp, reading it at
// most once per scanVersionTTL.
//
// It returns nil when the daemon would not say. That is not a failure worth
// refusing over — the scan itself may still succeed — and an unstamped row is
// honest: `attachment-report` counts it as never scanned and re-scan's IS NULL
// arm picks it up. Claiming a version we did not read would be the lie.
func (in *Ingestor) scanVersion(ctx context.Context) *store.ScanStamp {
	now := time.Now()
	in.versionMu.Lock()
	defer in.versionMu.Unlock()
	// ⚠ THE TTL GATES THE FAILURE TOO. Keying the cache on "we have a version"
	// rather than on "we asked recently" would make a daemon that answers PING
	// but not VERSION cost a socket round trip on every single message —
	// a per-message call is the thing this cache exists to prevent, and the
	// broken case is exactly when it would arrive.
	if !in.versionAt.IsZero() && now.Sub(in.versionAt) < scanVersionTTL {
		if in.version == nil {
			return nil
		}
		return &store.ScanStamp{At: now.UTC(), DBVersion: in.version.Database, Engine: in.version.Engine}
	}
	in.versionAt = now
	v, err := in.scanner.Version(ctx)
	if err != nil {
		slog.WarnContext(ctx, "ingest: the scanner would not report its signature version; verdicts from "+
			"this pass are recorded WITHOUT a stamp, so they count as never scanned and will be re-scanned",
			"err", err)
		in.version = nil
		return nil
	}
	in.version = &v
	return &store.ScanStamp{At: now.UTC(), DBVersion: v.Database, Engine: v.Engine}
}

// WithScanner injects the malware scanner (ADR-027 §4). When set, the ingest
// path scans every attachment part BEFORE its transaction opens and records a
// verdict in app.attachments inside it. ⚠ It RECORDS a verdict; nothing
// acts on one until ADR-027 §8's download gate (increment 5).
//
// ⚠ UNSET IS NOT A DEGRADED MODE, it is the pre-#394 behaviour: rows keep the
// 'unscanned' default, which §5d's aggregate reports as its own state rather
// than folding into "clean". Like WithScorer and WithClassifier, the many
// non-serving Ingestors (the drill, the CLIs, the sync tests) construct
// unchanged and scan nothing.
func WithScanner(s Scanner) Option {
	return func(in *Ingestor) { in.scanner = s }
}

// scanBreakerThreshold is how many separate ingest attempts must fail on ONE
// message — each with the daemon proven alive immediately beforehand — before
// its parts are recorded `unknown` and ingest moves on.
//
// 3 is a judgement, and the direction of the risk is what picks it. Too high
// and a crash-file wedges the account for hours; too low and an ordinary
// scanner blip waves a message through unscanned. ADR-027 §5b assigns `unknown`
// the SAME enforcement as `flagged`, so once §8's download gate lands (increment
// 5) waving one through is a quarantine with a different explanation rather than
// an open door — which is why the threshold leans low.
//
// ⚠ Until that gate exists, NOTHING is enforced: an `unknown` verdict is a
// record, not a restriction. Writing it the other way round would be a claim
// about code nobody has written.
const scanBreakerThreshold = 3

// scanBreaker implements ADR-027 §4b step 3: the defence against a file that
// CRASHES the daemon, where the resulting error is connection-level and
// therefore indistinguishable from an outage at the socket.
//
// ⚠ THE COUNTER ONLY MOVES ON EVIDENCE, and that is the whole design. A naive
// "N consecutive failures for this message" counter opens during a genuine
// outage too — the cursor never advances, so the SAME message is retried first
// on every pass, and after N passes the breaker would wave it through as
// `unknown`, then the next message, and the next: a slow drip of mail admitted
// while the scanner is still dead. So a failure is counted ONLY when the daemon
// answered a PING immediately BEFORE the scan that failed. The count then means
// "the service was up and this message took it down", which is the crash case
// and nothing else.
//
// Once open, the message never reaches the daemon again in this process's
// lifetime — re-sending a file that has already killed clamd three times is how
// a breaker becomes a loop. A restart resets it, which is the right
// granularity: an operator who has changed something gets a fresh answer.
type scanBreaker struct {
	mu     sync.Mutex
	counts map[[32]byte]int
}

func newScanBreaker() *scanBreaker { return &scanBreaker{counts: map[[32]byte]int{}} }

// record counts one attributable failure and reports the running total.
func (b *scanBreaker) record(sum [32]byte) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counts[sum]++
	return b.counts[sum]
}

// open reports whether this message has already been given up on.
func (b *scanBreaker) open(sum [32]byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counts[sum] >= scanBreakerThreshold
}

// clear forgets a message that scanned cleanly through. Without it the map
// would grow for the life of the process, and a message that failed once during
// a restart would carry that against it forever.
func (b *scanBreaker) clear(sum [32]byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.counts, sum)
}

// scanMessage produces a verdict for every attachment part of one message,
// together with the scanner state that produced them.
//
// It returns (nil, nil, nil) when there is no scanner or nothing to scan, a
// verdict list on success, and ONLY *ScannerUnavailableError as an error — a
// per-part problem is always a verdict, never an error.
//
// ⚠ THE STAMP IS READ BEFORE ANY BYTES GO TO THE DAEMON (#407). Reading it
// afterwards would let a mid-message freshclam update mark these rows as scanned
// against signatures that were not loaded when the scan ran — and a row that
// looks current stops being re-scanned, so the error would be permanent. This is
// the same discipline internal/rescan applies per chunk, and it is deliberately
// the same reasoning written in both places.
func (in *Ingestor) scanMessage(ctx context.Context, sum [32]byte, parsed *mime.Message) ([]store.ScanVerdict, *store.ScanStamp, error) {
	if in.scanner == nil || parsed == nil {
		return nil, nil, nil
	}
	parts := parsed.AttachmentParts()
	if len(parts) == 0 {
		return nil, nil, nil
	}
	hash := hex.EncodeToString(sum[:])
	stamp := in.scanVersion(ctx)

	// The two message-level reasons a scan could not mean anything, checked
	// before any bytes go near the daemon.
	if reason, forced := attachscan.ForcedUnknown(parsed); forced {
		slog.InfoContext(ctx, "ingest: attachments recorded unknown without a scan; nothing looked inside these",
			"content_hash", hash, "parts", len(parts), "reason", reason)
		return attachscan.Unknown(parts), stamp, nil
	}
	// ADR-027 section 6, scoped to the encrypted subtree (#405): ciphertext parts
	// are unknown and never sent; siblings of an encrypted node are scanned.
	enc := attachscan.EncryptedPaths(parsed)
	if attachscan.AllEncrypted(parts, enc) {
		slog.InfoContext(ctx, "ingest: attachments recorded unknown without a scan; every part is inside encrypted content",
			"content_hash", hash, "parts", len(parts))
		return attachscan.Unknown(parts), stamp, nil
	}
	if in.breaker.open(sum) {
		slog.WarnContext(ctx, "ingest: this message has taken the scanner down repeatedly and is no longer being sent to it; its attachments are unknown",
			"content_hash", hash, "parts", len(parts), "threshold", scanBreakerThreshold)
		return attachscan.Unknown(parts), stamp, nil
	}

	// ⚠ THE PRE-SCAN PING IS NOT A HEALTH CHECK, IT IS THE ATTRIBUTION. Compare
	// its answer with the scan's: alive-then-dead is a file that killed the
	// daemon (count it against the message); dead-then-dead is an outage (count
	// nothing, refuse the pass). Without it the two are the same socket error
	// and the breaker cannot tell them apart — see scanBreaker.
	//
	// A failing PING here does NOT itself refuse. It may be a blip, and the scan
	// below may well succeed; all it decides is how a failure would be read.
	daemonWasAlive := in.scanner.Ping(ctx) == nil

	verdicts := make([]store.ScanVerdict, 0, len(parts))
	for i, ap := range parts {
		// ⚠ ZERO BYTES CANNOT BE CLEAN. clamd answers "stream: OK" to an empty
		// INSTREAM, so sending nothing produces a confident clean verdict about
		// a part nothing read — the §5a failure in miniature. internal/mime
		// reaches this state on its own: walkLeaf records a body read error as a
		// WARNING and leaves the leaf's Data as whatever it got, which can be
		// nothing, and unlike the size limit it does NOT set Truncated.
		//
		// ⚠ What this does NOT catch is a PARTIAL read — some bytes, but not all
		// — for the same reason: the parser keeps that failure in msg.Warnings
		// and the Structure carries no per-part read status to key on, so
		// OctetSize equals what was read and cannot disagree with it. Naming the
		// gap rather than matching warning strings; closing it needs the parser
		// to say so per part.
		if enc[ap.Path] {
			verdicts = append(verdicts, attachscan.Unknown([]mime.AttachmentPart{ap})...)
			continue
		}
		if attachscan.HasNoBytes(ap) {
			slog.InfoContext(ctx, "ingest: an attachment decoded to zero bytes; recorded unknown rather than scanning nothing",
				"content_hash", hash, "part_path", ap.Path, "declared_type", ap.Node.ContentType)
			verdicts = append(verdicts, attachscan.Unknown([]mime.AttachmentPart{ap})...)
			continue
		}
		v, err := in.scanner.Scan(ctx, ap.Node.Data)
		if err != nil {
			// ⚠ A SHUTDOWN IS NOT A SCANNER OUTAGE, and conflating them would
			// undo one layer up exactly what internal/clamd.Scan was fixed to
			// stop doing. Every restart that happens to land mid-scan would
			// otherwise raise account health as "the malware scanner is
			// unreachable" — a false statement to the operator at the moment
			// they are least able to check it — and would pause an account for
			// a reason that had already gone away. Propagated as itself, so
			// mailsync treats it as the ordinary per-mailbox failure it is.
			if ctx.Err() != nil {
				return nil, nil, err
			}
			if !daemonWasAlive {
				// It was already down before this message was touched, so
				// nothing here is attributable to the mail.
				return nil, nil, &ScannerUnavailableError{Cause: err, Consecutive: 0}
			}
			n := in.breaker.record(sum)
			if n < scanBreakerThreshold {
				return nil, nil, &ScannerUnavailableError{Cause: err, Consecutive: n}
			}
			// The breaker opens. Every REMAINING part is unknown too: the daemon
			// is in an unknown state after being taken down mid-message, so a
			// verdict on part i+1 would be a claim we cannot support.
			slog.WarnContext(ctx, "ingest: scanning this message took the scanner down repeatedly; admitting it with unknown verdicts rather than halting the account",
				"content_hash", hash, "failures", n, "err", err)
			return append(verdicts, attachscan.Unknown(parts[i:])...), stamp, nil
		}
		logVerdict(ctx, hash, ap.Path, v)
		verdicts = append(verdicts, attachscan.Verdict(ap, v))
	}
	in.breaker.clear(sum)
	return verdicts, stamp, nil
}

// logVerdict writes the detection record (ADR-027 §10c).
//
// ⚠ THE FILENAME IS DELIBERATELY ABSENT, and that is a decision rather than an
// oversight. clamd never learns it — we speak INSTREAM precisely so no filename
// ever reaches the scanner or its logs (§10c) — and it is attacker-controlled
// text whose whole purpose in this threat model is deception (§11's double
// extensions and bidi overrides). Putting it in a shared log would re-introduce
// on our side the thing INSTREAM removes on clamd's, and hand an attacker a
// string in the operator's console. content_hash plus part_path names the part
// unambiguously for anyone who needs to look it up.
func logVerdict(ctx context.Context, hash, partPath string, v clamd.Verdict) {
	switch v.State {
	case clamd.Flagged:
		// The message is still stored and still readable — nothing is deleted
		// automatically, because the scanner is the component most likely to be
		// wrong (§8). What changes is that the part is gated.
		slog.WarnContext(ctx, "ingest: MALWARE DETECTED in an attachment; the message is stored and readable, the part is quarantined",
			"content_hash", hash, "part_path", partPath, "signature", v.Signature)
	case clamd.Unknown:
		slog.InfoContext(ctx, "ingest: an attachment could not be determined; nothing looked inside it",
			"content_hash", hash, "part_path", partPath, "signature", v.Signature, "reason", v.Reason)
	}
}
