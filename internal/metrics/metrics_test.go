// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRegistersWithoutPanic(t *testing.T) {
	m := New()
	if m == nil {
		t.Fatal("New() = nil")
	}

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(families) != 0 {
		// Nothing has been observed yet; vecs with labels start empty and
		// contribute no series until touched. That's expected, not a bug.
		t.Logf("Gather() returned %d families before any observation", len(families))
	}
}

func TestMetricsAreGatherableAfterObservation(t *testing.T) {
	m := New()

	m.SyncLagSeconds.WithLabelValues("account-1").Set(42)
	m.IdleConnState.WithLabelValues("account-1").Set(2)
	m.ActionQueueDepth.WithLabelValues("account-1").Set(3)
	m.WritebackFailuresTotal.WithLabelValues("account-1").Inc()
	m.IndexCursorLagSeconds.Set(1.5)
	m.JobQueueDepth.Set(7)
	m.JobDeadLettersTotal.WithLabelValues("snooze").Inc()
	m.OutboundLedgerLevel.WithLabelValues("account-1").Set(0.5)
	m.SyncDisabledAccounts.WithLabelValues("user-1").Set(1)
	m.ScannerSignaturePublishedTimestamp.Set(1790150682)
	m.ScannerSignatureVersion.Set(28132)

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}

	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}

	want := []string{
		"mailroom_sync_lag_seconds",
		"mailroom_idle_connection_state",
		"mailroom_action_queue_depth",
		"mailroom_writeback_failures_total",
		"mailroom_index_cursor_lag_seconds",
		"mailroom_job_queue_depth",
		"mailroom_job_dead_letters_total",
		"mailroom_outbound_ledger_level",
		"mailroom_sync_disabled_accounts",
		"mailroom_scanner_signature_published_timestamp_seconds",
		"mailroom_scanner_signature_version",
	}
	for _, name := range want {
		if !names[name] {
			t.Errorf("Gather() missing metric family %q", name)
		}
	}
}

func TestHandlerServesMetrics(t *testing.T) {
	m := New()
	m.JobQueueDepth.Set(5)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "mailroom_job_queue_depth") {
		t.Errorf("body does not contain mailroom_job_queue_depth:\n%s", rec.Body.String())
	}
}
