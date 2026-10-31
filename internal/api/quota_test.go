// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQuotaAdmits(t *testing.T) {
	q := Quota{Kind: QuotaStorageBytes, Limit: 100, Used: 90}
	if !q.Admits(10) {
		t.Error("adding exactly to the limit must be admitted")
	}
	if q.Admits(11) {
		t.Error("adding past the limit must be refused")
	}
	if !q.Admits(0) || !q.Admits(-5) {
		t.Error("a delete or no-op must always be admitted")
	}
	if q.Remaining() != 10 {
		t.Errorf("Remaining = %d, want 10", q.Remaining())
	}
}

func TestQuotaUnlimited(t *testing.T) {
	for _, lim := range []int64{0, -1} {
		q := Quota{Kind: QuotaLLMDailyTokens, Limit: lim, Used: 1 << 40}
		if !q.Unlimited() {
			t.Errorf("limit %d must read as unlimited", lim)
		}
		if !q.Admits(1 << 50) {
			t.Errorf("an unlimited quota must admit any delta (limit %d)", lim)
		}
	}
}

func TestQuotaRemainingClampsAtZero(t *testing.T) {
	q := Quota{Kind: QuotaStorageBytes, Limit: 100, Used: 250}
	if q.Remaining() != 0 {
		t.Errorf("Remaining = %d, want 0 when over quota", q.Remaining())
	}
	if q.Admits(1) {
		t.Error("an already-over-quota user must not admit more")
	}
}

// TestOverQuotaShapes: the two rejection shapes are the explicit, no-data-lost
// answers the invariant requires — a /set overQuota SetError and a 413 upload.
func TestOverQuotaShapes(t *testing.T) {
	q := Quota{Kind: QuotaStorageBytes, Limit: 100, Used: 100}

	if se := overQuotaSetError(q); se.Type != setOverQuota {
		t.Errorf("SetError type = %q, want %q", se.Type, setOverQuota)
	}

	rec := httptest.NewRecorder()
	writeOverQuota(rec, q)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("upload over quota: status %d, want 413", rec.Code)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("over-quota response missing the blob hardening headers")
	}
}
