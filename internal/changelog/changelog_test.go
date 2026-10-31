// SPDX-License-Identifier: AGPL-3.0-only

package changelog

import (
	"context"
	"testing"
	"time"

	"mailroom/internal/store"
)

// TestNewWorkerDefaults pins the fallback interval, retention and batch size,
// so a caller passing non-positive values (or forgetting them) gets the
// documented weekly/90-day defaults rather than a zero-duration timer or an
// unbounded per-call delete.
func TestNewWorkerDefaults(t *testing.T) {
	w := NewWorker(nil, 0, 0, nil)
	if w.interval != defaultInterval {
		t.Errorf("interval = %v, want the default %v", w.interval, defaultInterval)
	}
	if w.retention != 90*24*time.Hour {
		t.Errorf("retention = %v, want the default 90 days", w.retention)
	}
	if w.batch != store.DefaultChangeLogCompactBatch {
		t.Errorf("batch = %d, want %d", w.batch, store.DefaultChangeLogCompactBatch)
	}
	if w.log == nil {
		t.Error("a nil logger was not replaced with slog.Default()")
	}
}

// TestHandleRejectsSystemScopedJob pins that compaction is inherently
// per-user (it walks one user's RLS-scoped change_log) — a job with no user
// id is a wiring bug, not something to silently skip or spin on.
func TestHandleRejectsSystemScopedJob(t *testing.T) {
	w := NewWorker(nil, 0, 0, nil)
	err := w.Handle(context.Background(), store.Job{Kind: JobKind})
	if err == nil {
		t.Fatal("Handle(job with no user id) = nil error, want a rejection")
	}
}
