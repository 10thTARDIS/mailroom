// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"mailroom/internal/clamd"
	"mailroom/internal/jobs"
	"mailroom/internal/metrics"
)

// ⚠ WHAT THIS PINS (#524). jobs.Metrics and jobs.NopMetrics have existed since
// the dispatcher was written, and jobs.Config defaults Metrics to NopMetrics{}.
// Nothing in cmd/ ever passed anything else, so every dead-letter the
// dispatcher reported was discarded before it reached the registry that /metrics
// serves. mailroom_job_dead_letters_total was therefore a permanent 0, and an
// alert on "jobs are dead-lettering" could not fire.
//
// The compile-time half of the wiring is guarded by the assertion below that
// dispatcherMetrics satisfies jobs.Metrics; the behavioural half is that a
// reported dead-letter actually moves the counter, per kind.
var _ jobs.Metrics = dispatcherMetrics{}

func TestDeadLetteredMovesThePrometheusCounterPerKind(t *testing.T) {
	m := metrics.New()
	d := dispatcherMetrics{m: m}

	d.DeadLettered("purge_trash")
	d.DeadLettered("purge_trash")
	d.DeadLettered("ml_train")

	if got := testutil.ToFloat64(m.JobDeadLettersTotal.WithLabelValues("purge_trash")); got != 2 {
		t.Errorf("purge_trash dead letters = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.JobDeadLettersTotal.WithLabelValues("ml_train")); got != 1 {
		t.Errorf("ml_train dead letters = %v, want 1", got)
	}

	// The kind must be a LABEL, not baked into the metric name: an alert wants
	// to see WHICH recurring chain died, and purge_trash dying is the one that
	// eventually fills the disk. Comparing the exposition text pins the label
	// name and the values together, so renaming the label is a red test rather
	// than a silently reshaped dashboard.
	const expected = `
# HELP mailroom_job_dead_letters_total Total jobs that exhausted retries and landed in the dead-letter state, by kind.
# TYPE mailroom_job_dead_letters_total counter
mailroom_job_dead_letters_total{kind="ml_train"} 1
mailroom_job_dead_letters_total{kind="purge_trash"} 2
`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(expected),
		"mailroom_job_dead_letters_total"); err != nil {
		t.Error(err)
	}
}

// A metric nobody increments serves a constant 0, which reads as "healthy"
// rather than "unwired" — so the absence of a signal must be distinguishable
// from a real zero. This asserts the counter is REGISTERED on the private
// registry even before anything is counted, which is what makes a later 0 an
// honest measurement rather than a missing series.
func TestJobDeadLettersIsRegisteredBeforeAnythingIsCounted(t *testing.T) {
	m := metrics.New()
	// Touching the label set is what materialises a *Vec child series.
	m.JobDeadLettersTotal.WithLabelValues("purge_trash")

	if got := testutil.CollectAndCount(m.Registry(), "mailroom_job_dead_letters_total"); got != 1 {
		t.Errorf("registered series for mailroom_job_dead_letters_total = %d, want 1", got)
	}
}

// The depth gauge must carry the counted backlog through unchanged. It is a
// Gauge, not a Counter, so a wiring that accidentally accumulated would climb
// forever and look like a runaway queue; setting the same value twice must
// leave it where it is.
func TestObserveQueueDepthSetsRatherThanAccumulates(t *testing.T) {
	m := metrics.New()
	d := dispatcherMetrics{m: m}

	d.ObserveQueueDepth(4096)
	if got := testutil.ToFloat64(m.JobQueueDepth); got != 4096 {
		t.Errorf("job queue depth = %v, want 4096", got)
	}

	d.ObserveQueueDepth(4096)
	if got := testutil.ToFloat64(m.JobQueueDepth); got != 4096 {
		t.Errorf("after reporting the same depth twice = %v, want 4096 — a Gauge must "+
			"SET, not accumulate", got)
	}

	// A drained queue must be able to report zero. If the wiring only ever set
	// non-zero values, the gauge would stick at its high-water mark and an alert
	// would never clear.
	d.ObserveQueueDepth(0)
	if got := testutil.ToFloat64(m.JobQueueDepth); got != 0 {
		t.Errorf("after the queue drained = %v, want 0", got)
	}
}

// ⚠ WHAT THIS PINS (#641). scannerVersionSink is what rescan.Worker.Handle
// calls on every scanner VERSION read; a sink that built but never wrote
// either gauge would leave both series at their permanent zero default — the
// "declared, registered, served, never set" shape #524 already named once for
// eight of ten metrics in this file.
func TestScannerVersionSinkSetsBothGauges(t *testing.T) {
	m := metrics.New()
	sink := scannerVersionSink(m)
	published := time.Date(2026, 9, 23, 6, 24, 42, 0, time.UTC)

	sink(clamd.Version{Engine: "1.5.4", Database: 28132, Published: published})

	if got := testutil.ToFloat64(m.ScannerSignatureVersion); got != 28132 {
		t.Errorf("signature version = %v, want 28132", got)
	}
	if got, want := testutil.ToFloat64(m.ScannerSignaturePublishedTimestamp), float64(published.Unix()); got != want {
		t.Errorf("signature published timestamp = %v, want %v", got, want)
	}
}

// ⚠ AN UNKNOWN PUBLICATION TIME MUST READ AS STALE, NOT KEEP AN OLD DATE. The
// gauge describes the database clamd has loaded NOW; carrying the previous
// database's date forward would report a freshness nobody observed.
// clamd.Version.Stale treats an unknown age as stale, and `time() - 0` makes
// the alert agree with it.
func TestScannerVersionSinkUnknownPublicationReadsAsStale(t *testing.T) {
	m := metrics.New()
	sink := scannerVersionSink(m)

	sink(clamd.Version{Engine: "1.5.4", Database: 28110, Published: time.Now().Add(-6 * time.Hour)})
	sink(clamd.Version{Engine: "1.5.4", Database: 28111}) // the daemon stopped reporting a timestamp

	if got := testutil.ToFloat64(m.ScannerSignatureVersion); got != 28111 {
		t.Errorf("signature version = %v, want 28111", got)
	}
	if got := testutil.ToFloat64(m.ScannerSignaturePublishedTimestamp); got != 0 {
		t.Errorf("signature published timestamp = %v, want 0: an unknown date kept the previous database's", got)
	}
}

// fakeVersionReader answers watchScannerVersion's reads from a script: each call
// takes the next entry, and the last one repeats.
type fakeVersionReader struct {
	mu      sync.Mutex
	answers []fakeVersionAnswer
	calls   int
}

type fakeVersionAnswer struct {
	v   clamd.Version
	err error
}

func (f *fakeVersionReader) Version(context.Context) (clamd.Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.answers[min(f.calls, len(f.answers)-1)]
	f.calls++
	return a.v, a.err
}

// ⚠ THE BOOT READ (#641). v1.79.0 left both gauges at 0 for up to a day after
// every restart, because its only reader was a re-scan job that was not due. The
// interval here is an hour, so a gauge that is set within the test's two seconds
// can only have come from the IMMEDIATE read.
func TestWatchScannerVersionReadsAtOnce(t *testing.T) {
	m := metrics.New()
	published := time.Date(2026, 9, 23, 6, 24, 42, 0, time.UTC)
	r := &fakeVersionReader{answers: []fakeVersionAnswer{{v: clamd.Version{Database: 28132, Published: published}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchScannerVersion(ctx, r, scannerVersionSink(m), time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))

	deadline := time.Now().Add(2 * time.Second)
	for testutil.ToFloat64(m.ScannerSignatureVersion) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := testutil.ToFloat64(m.ScannerSignatureVersion); got != 28132 {
		t.Fatalf("signature version = %v two seconds after start, want 28132: nothing was read at boot", got)
	}
	if got, want := testutil.ToFloat64(m.ScannerSignaturePublishedTimestamp), float64(published.Unix()); got != want {
		t.Errorf("signature published timestamp = %v, want %v", got, want)
	}
}

// A failed read keeps the gauges as they were, and the next tick recovers.
// Zeroing them on a transient socket error would page as "stale" while the
// database is in fact current.
func TestWatchScannerVersionKeepsLastValueOnError(t *testing.T) {
	m := metrics.New()
	r := &fakeVersionReader{answers: []fakeVersionAnswer{
		{v: clamd.Version{Database: 28131, Published: time.Unix(1790000000, 0)}},
		{err: errors.New("dial unix /run/clamav/clamd.sock: connection refused")},
		{v: clamd.Version{Database: 28132, Published: time.Unix(1790100000, 0)}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := scannerVersionSink(m)
	var seen []float64
	var mu sync.Mutex
	record := func(v clamd.Version) {
		sink(v)
		mu.Lock()
		seen = append(seen, testutil.ToFloat64(m.ScannerSignatureVersion))
		mu.Unlock()
	}
	go watchScannerVersion(ctx, r, record, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))

	deadline := time.Now().Add(2 * time.Second)
	for testutil.ToFloat64(m.ScannerSignatureVersion) != 28132 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		// While the failing read is the latest one, the value must be the
		// previous database's, never zero.
		if got := testutil.ToFloat64(m.ScannerSignatureVersion); got == 0 && r.callCount() > 1 {
			t.Fatalf("signature version dropped to 0 after a failed read")
		}
	}
	if got := testutil.ToFloat64(m.ScannerSignatureVersion); got != 28132 {
		t.Fatalf("signature version = %v, want 28132 after the scanner came back", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != 28131 {
		t.Errorf("sink saw %v, want 28131 first and then 28132 (the failed read calls no sink)", seen)
	}
}

func (f *fakeVersionReader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
