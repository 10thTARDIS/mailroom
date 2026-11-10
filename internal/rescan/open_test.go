// SPDX-License-Identifier: AGPL-3.0-only

package rescan

// Unit tests (no database) for the open-time re-scan's bounds: the per-part
// budget, the breaker, the version negative cache, the global enqueue cap and the
// transient-unknown rule. The integration tests run the same paths against
// Postgres; these are the ones that can fail without it.
//
// ⚠ EVERY "DID NOT HAPPEN" ASSERTION HAS A CONTROL BESIDE IT: the same fixture
// with the one thing changed that must make it happen.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"mailroom/internal/clamd"
	"mailroom/internal/store"
)

// bytesScanner answers by the part's bytes: a part whose bytes contain `hang`
// blocks until its context ends, one containing `flag` is FOUND, `boom` is a
// connection error, anything else is clean. Safe for concurrent use.
type bytesScanner struct {
	mu               sync.Mutex
	hang, flag, boom string
	seen             []string
	versionErr       error
	versionCalls     int
	db               int
}

func (s *bytesScanner) Scan(ctx context.Context, data []byte) (clamd.Verdict, error) {
	s.mu.Lock()
	s.seen = append(s.seen, string(data))
	s.mu.Unlock()
	switch {
	case s.hang != "" && bytes.Contains(data, []byte(s.hang)):
		<-ctx.Done()
		return clamd.Verdict{}, errors.Join(errors.New("clamd: scan abandoned"), ctx.Err())
	case s.boom != "" && bytes.Contains(data, []byte(s.boom)):
		return clamd.Verdict{}, errors.New("dial tcp: connection refused")
	case s.flag != "" && bytes.Contains(data, []byte(s.flag)):
		return clamd.Verdict{State: clamd.Flagged, Signature: "Test-Signature"}, nil
	}
	return clamd.Verdict{State: clamd.Clean}, nil
}

func (s *bytesScanner) Version(context.Context) (clamd.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versionCalls++
	if s.versionErr != nil {
		return clamd.Version{}, s.versionErr
	}
	return clamd.Version{Engine: "1.5.4", Database: 28110, Published: time.Now()}, nil
}

func newUnitOpen(sc Scanner) (*OpenRescan, *clock) {
	o := NewOpenRescan(nil, nil, sc, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	o.now = c.now
	return o, c
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Three attachments: "AAAA..." (part 2, largest), "BBB" (part 3, small),
// "CCCCC" (part 4, medium).
const threeParts = "From: a@example.com\r\nSubject: s\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=bnd\r\n\r\n--bnd\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
	"--bnd\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"a.bin\"\r\n\r\nAAAAAAAAAAAA\r\n" +
	"--bnd\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"b.bin\"\r\n\r\nBBB\r\n" +
	"--bnd\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"c.bin\"\r\n\r\nCCCCC\r\n" +
	"--bnd--\r\n"

var allStale = map[string]bool{"2": true, "3": true, "4": true}

func verdictsByPath(v []store.ScanVerdict) map[string]string {
	m := map[string]string{}
	for _, x := range v {
		m[x.PartPath] = x.Status
	}
	return m
}

// MEDIUM-1: a part that hangs does not shield the part behind it. A hangs, B is
// FOUND: B's verdict is produced, A's is not (it keeps its stored one), and the
// hang is counted as ONE timeout.
func TestAHungPartDoesNotShieldTheOthers(t *testing.T) {
	sc := &bytesScanner{hang: "AAAA", flag: "BBB"}
	o, _ := newUnitOpen(sc)
	start := time.Now()

	got, err := o.scanStale(context.Background(), parse(t, threeParts), allStale, "2", start.Add(600*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	v := verdictsByPath(got.verdicts)
	if v["3"] != "flagged" {
		t.Errorf("part 3 = %q, want flagged: a hung part in front of it stopped it being refreshed (verdicts %v)", v["3"], v)
	}
	if _, wrote := v["2"]; wrote {
		t.Errorf("the hung part got a verdict %q; it must keep its stored one", v["2"])
	}
	if v["4"] != "clean" {
		t.Errorf("part 4 = %q, want clean", v["4"])
	}
	if got.timeouts != 1 || got.unavailable {
		t.Errorf("timeouts=%d unavailable=%v, want 1 timeout and the scanner still available", got.timeouts, got.unavailable)
	}
	// The hung part took its slice (half of the budget, being the requested one),
	// not all of it.
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("took %v: the hung part consumed more than its slice", d)
	}

	// CONTROL: with nothing hanging the same message yields three verdicts and no
	// timeout, so the one missing above is the hang's doing.
	ok := &bytesScanner{flag: "BBB"}
	o2, _ := newUnitOpen(ok)
	got, err = o2.scanStale(context.Background(), parse(t, threeParts), allStale, "2", time.Now().Add(time.Second))
	if err != nil || len(got.verdicts) != 3 || got.timeouts != 0 {
		t.Errorf("CONTROL FAILED: verdicts=%d timeouts=%d err=%v", len(got.verdicts), got.timeouts, err)
	}
}

// The order: the requested part first, then smallest first. And only STALE parts
// are sent.
func TestStaleOnlyRequestedFirstThenSmallest(t *testing.T) {
	sc := &bytesScanner{}
	o, _ := newUnitOpen(sc)

	_, err := o.scanStale(context.Background(), parse(t, threeParts), allStale, "2", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"AAAAAAAAAAAA", "BBB", "CCCCC"}
	if len(sc.seen) != 3 || sc.seen[0] != want[0] || sc.seen[1] != want[1] || sc.seen[2] != want[2] {
		t.Errorf("scan order = %q, want %q (requested first, then smallest)", sc.seen, want)
	}

	sc2 := &bytesScanner{}
	o2, _ := newUnitOpen(sc2)
	_, _ = o2.scanStale(context.Background(), parse(t, threeParts), map[string]bool{"2": true, "4": true}, "", time.Now().Add(time.Second))
	if len(sc2.seen) != 2 || sc2.seen[0] != "CCCCC" || sc2.seen[1] != "AAAAAAAAAAAA" {
		t.Errorf("scan = %q, want only the stale parts, smallest first", sc2.seen)
	}
}

// A scanner error that is not a deadline is the scanner failing: stop, report
// it unavailable (the caller counts one breaker failure), keep what finished.
func TestAScannerErrorIsUnavailableNotATimeout(t *testing.T) {
	sc := &bytesScanner{boom: "CCCCC"}
	o, _ := newUnitOpen(sc)
	got, err := o.scanStale(context.Background(), parse(t, threeParts), allStale, "", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !got.unavailable || got.timeouts != 0 {
		t.Errorf("unavailable=%v timeouts=%d, want unavailable and no timeout", got.unavailable, got.timeouts)
	}
	if v := verdictsByPath(got.verdicts); v["3"] != "clean" || len(v) != 1 {
		t.Errorf("verdicts %v, want only the part scanned before the failure", v)
	}
}

// MEDIUM-1: one failure does not open the breaker; three within the window do;
// failures older than the window do not count.
func TestBreakerOpensOnlyAfterThreeFailuresInTheWindow(t *testing.T) {
	o, c := newUnitOpen(&bytesScanner{})
	o.recordFailure()
	o.recordFailure()
	if o.breakerOpen() {
		t.Fatal("the breaker opened after 2 failures")
	}
	o.recordFailure()
	if !o.breakerOpen() {
		t.Fatal("CONTROL FAILED: three failures within the window did not open the breaker")
	}
	c.add(BreakerPause + time.Second)
	if o.breakerOpen() {
		t.Error("the breaker stayed open past its pause")
	}

	// Spread beyond the window: never three at once.
	o2, c2 := newUnitOpen(&bytesScanner{})
	for i := 0; i < 5; i++ {
		o2.recordFailure()
		c2.add(BreakerWindow/2 + time.Second)
	}
	if o2.breakerOpen() {
		t.Error("failures spaced wider than the window opened the breaker")
	}
}

// LOW-1: a failed VERSION call is remembered for versionFailTTL (no dial), counts
// once toward the breaker, and is retried after the TTL.
func TestFailedVersionIsCachedAndCountsOnce(t *testing.T) {
	sc := &bytesScanner{versionErr: errors.New("connection refused")}
	o, c := newUnitOpen(sc)

	for i := 0; i < 5; i++ {
		if _, err := o.currentVersion(context.Background()); err == nil {
			t.Fatal("expected the cached failure")
		}
	}
	if sc.versionCalls != 1 {
		t.Errorf("VERSION dialled %d times for 5 calls, want 1", sc.versionCalls)
	}
	if len(o.failures) != 1 {
		t.Errorf("breaker failures = %d, want 1 (cached answers are not new failures)", len(o.failures))
	}
	c.add(versionFailTTL + time.Second)
	_, _ = o.currentVersion(context.Background())
	if sc.versionCalls != 2 {
		t.Errorf("after the TTL VERSION dialled %d times, want 2", sc.versionCalls)
	}
	// CONTROL: a success is cached the other way and clears the failure.
	sc.mu.Lock()
	sc.versionErr = nil
	sc.mu.Unlock()
	c.add(versionFailTTL + time.Second)
	if _, err := o.currentVersion(context.Background()); err != nil {
		t.Errorf("CONTROL FAILED: a healthy scanner reported %v", err)
	}
}

// MEDIUM-2: the per-process cap on enqueues.
func TestGlobalEnqueueCap(t *testing.T) {
	o, c := newUnitOpen(&bytesScanner{})
	var taken []time.Time
	for i := 0; i < GlobalEnqueueCap; i++ {
		at, ok := o.reserveEnqueue()
		if !ok {
			t.Fatalf("CONTROL FAILED: reservation %d refused below the cap", i+1)
		}
		taken = append(taken, at)
		c.add(time.Millisecond)
	}
	if _, ok := o.reserveEnqueue(); ok {
		t.Fatal("reservation past the cap was allowed")
	}
	o.releaseEnqueue(taken[0])
	if _, ok := o.reserveEnqueue(); !ok {
		t.Error("a released slot (INSERT created nothing) was not reusable")
	}
	c.add(GlobalCapWindow + time.Second)
	if _, ok := o.reserveEnqueue(); !ok {
		t.Error("the window did not slide")
	}
}

// LOW-2: a transient unknown is dropped whenever the prior verdict did not block.
func TestTransientUnknownDroppedOverAnyNonBlockingPrior(t *testing.T) {
	tr := store.ScanVerdict{PartPath: "2", Status: "unknown", Transient: true}
	policy := store.ScanVerdict{PartPath: "2", Status: "unknown"}
	for _, tc := range []struct {
		name  string
		prior []store.PartScanState
		v     store.ScanVerdict
		drop  bool
	}{
		{"clean", []store.PartScanState{{PartPath: "2", Status: "clean"}}, tr, true},
		{"unscanned", []store.PartScanState{{PartPath: "2", Status: "unscanned"}}, tr, true},
		{"empty status", []store.PartScanState{{PartPath: "2"}}, tr, true},
		{"flagged but released", []store.PartScanState{{PartPath: "2", Status: "flagged", Released: true}}, tr, true},
		// Controls: a part that already blocks takes the new answer, and a policy
		// unknown is always written.
		{"flagged", []store.PartScanState{{PartPath: "2", Status: "flagged"}}, tr, false},
		{"unknown", []store.PartScanState{{PartPath: "2", Status: "unknown"}}, tr, false},
		{"policy unknown over clean", []store.PartScanState{{PartPath: "2", Status: "clean"}}, policy, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, dropped := dropTransientRegressions(tc.prior, []store.ScanVerdict{tc.v})
			if (dropped == 1) != tc.drop || (len(out) == 0) != tc.drop {
				t.Errorf("dropped=%d kept=%d, want drop=%v", dropped, len(out), tc.drop)
			}
		})
	}
}
