// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"mailroom/internal/api"
	"mailroom/internal/metrics"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestSummariseMethodTimings(t *testing.T) {
	calls := []api.MethodTiming{
		{Name: "Email/get", Duration: ms(200)},
		{Name: "Email/query", Duration: ms(4000)},
		{Name: "Area/getSummary", Duration: ms(700)},
		{Name: "Email/get", Duration: ms(210)},
		{Name: "Email/query", Duration: ms(4200), Failed: true},
	}
	got := summariseMethodTimings(calls)
	want := "Email/query×2=8200ms Area/getSummary×1=700ms Email/get×2=410ms"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if summariseMethodTimings(nil) != "" {
		t.Error("an empty list must summarise to the empty string")
	}
}

func TestJMAPTimingObserverLogsOnlyAtThreshold(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	m := metrics.New()
	obs := jmapTimingObserver(m, logger)
	calls := []api.MethodTiming{{Name: "Email/query", Duration: ms(900)}}

	obs(api.RequestTiming{UserID: ulid.Make(), Total: slowJMAPRequest - time.Millisecond, Methods: calls})
	if buf.Len() != 0 {
		t.Fatalf("a request under the threshold must not log, got %q", buf.String())
	}
	obs(api.RequestTiming{UserID: ulid.Make(), Total: slowJMAPRequest, Methods: calls})
	out := buf.String()
	for _, want := range []string{"slow jmap request", "total_ms=1000", "calls=1", "Email/query×1=900ms", "user_id="} {
		if !strings.Contains(out, want) {
			t.Errorf("log line missing %q: %s", want, out)
		}
	}

	// Both requests fed the histograms, whether or not they logged.
	if n := testutil.CollectAndCount(m.JMAPMethodDuration); n != 1 {
		t.Errorf("method histogram series = %d, want 1", n)
	}
	if n := testutil.CollectAndCount(m.JMAPRequestDuration); n != 1 {
		t.Errorf("request histogram series = %d, want 1", n)
	}
}
