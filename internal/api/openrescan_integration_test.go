// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Re-scan on open at the download gate (ADR-056), end to end: a real Postgres, a
// real encrypted blob store, the real rescan.OpenRescan with a scripted scanner.
//
// ⚠ EACH REFUSAL HAS A CONTROL. "403" is also what a broken fixture answers, so the
// same URL is fetched on a message that must NOT be re-scanned (it is young) and it
// downloads: the 403 is then attributable to the fresh verdict.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/clamd"
	"mailroom/internal/rescan"
)

type scriptScanner struct {
	mu  sync.Mutex
	seq []clamd.Verdict
	n   int
}

func (s *scriptScanner) Scan(context.Context, []byte) (clamd.Verdict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.seq[s.n%len(s.seq)]
	s.n++
	return v, nil
}

func (s *scriptScanner) Version(context.Context) (clamd.Version, error) {
	return clamd.Version{Engine: "1.5.4", Database: 28110, Published: time.Now()}, nil
}

// byBytesScanner calls the PDF infected and the HTML part a transient failure.
type byBytesScanner struct{ scriptScanner }

func (b *byBytesScanner) Scan(_ context.Context, data []byte) (clamd.Verdict, error) {
	b.mu.Lock()
	b.n++
	b.mu.Unlock()
	if bytes.HasPrefix(data, []byte("%PDF")) {
		return clamd.Verdict{State: clamd.Flagged, Signature: "Test-Signature"}, nil
	}
	return clamd.Verdict{State: clamd.Unknown, Reason: "scan failed but the daemon is alive", Transient: true}, nil
}

type apiBlobs struct{ a *API }

func (b apiBlobs) Raw(ctx context.Context, u ulid.ULID, h []byte) ([]byte, error) {
	return b.a.blobs.ReadRaw(ctx, u, h)
}

func ageMessage(t *testing.T, h *composeHarness, user, msg ulid.ULID, days int) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET received_at = now() - make_interval(days => $3) WHERE user_id=$1 AND id=$2`,
		user.String(), msg.String(), days); err != nil {
		t.Fatalf("age message: %v", err)
	}
}

func TestDownloadRescansOldMailAndRefusesByTheFreshVerdict(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	// Answers by the part's bytes, not by call order: only STALE parts are scanned
	// now, so a part left stale by a transient answer is asked again on the next
	// request and a call-order script would hand it the other part's verdict.
	sc := &byBytesScanner{}
	h.api.openRescan = rescan.NewOpenRescan(h.store, apiBlobs{h.api}, sc, nil, nil)
	srv := httptest.NewServer(blobHandler(h, a))
	defer srv.Close()

	// CONTROL: a YOUNG message with the same stale `clean` rows downloads and is not scanned.
	young := seedRawMessage(t, h, a, []byte(rawWithAttachment))
	_, youngParts := partBlobIDs(t, h, a, young)
	seedAttachmentRows(t, h, a.UserID, young, []byte(rawWithAttachment), nil)
	if _, resp := getBody(t, srv, downloadURL(a.UserID.String(), youngParts["invoice.pdf"], "invoice.pdf")); resp.StatusCode != http.StatusOK {
		t.Fatalf("CONTROL FAILED: young message part status %d, want 200", resp.StatusCode)
	}
	if sc.n != 0 {
		t.Fatalf("CONTROL FAILED: a young message was re-scanned (%d scans)", sc.n)
	}

	// The same bytes as OLD mail (a distinct message: the blob is shared, the row is not).
	old := seedRawMessage(t, h, a, []byte(rawWithAttachment+"\r\n"))
	_, oldParts := partBlobIDs(t, h, a, old)
	seedAttachmentRows(t, h, a.UserID, old, []byte(rawWithAttachment), nil)
	ageMessage(t, h, a.UserID, old, 400)

	// Part 2 comes back flagged, part 3 transiently unknown. The infected part is
	// refused at once; the other keeps its stored clean and is served.
	if _, resp := getBody(t, srv, downloadURL(a.UserID.String(), oldParts["invoice.pdf"], "invoice.pdf")); resp.StatusCode != http.StatusForbidden {
		t.Errorf("an attachment flagged by the re-scan downloaded with status %d, want 403", resp.StatusCode)
	}
	if _, resp := getBody(t, srv, downloadURL(a.UserID.String(), oldParts["payload.html"], "payload.html")); resp.StatusCode != http.StatusOK {
		t.Errorf("the part whose re-scan failed transiently got status %d, want its stored clean to stand (200)", resp.StatusCode)
	}
	// The preview door reads the same fresh verdict.
	if _, resp := getBody(t, srv, previewURL(a.UserID.String(), oldParts["invoice.pdf"], "invoice.pdf")); resp.StatusCode != http.StatusForbidden {
		t.Errorf("preview of the flagged part status %d, want 403", resp.StatusCode)
	}
}

// hangOnPDF hangs on the PDF part and calls the HTML part infected.
type hangOnPDF struct{ scriptScanner }

func (h *hangOnPDF) Scan(ctx context.Context, data []byte) (clamd.Verdict, error) {
	if bytes.HasPrefix(data, []byte("%PDF")) {
		<-ctx.Done()
		return clamd.Verdict{}, ctx.Err()
	}
	return clamd.Verdict{State: clamd.Flagged, Signature: "Test-Signature"}, nil
}

// One slow part must not shield the others (security review MEDIUM-1): downloading
// the part that hangs is served under its stored verdict (fail open), and the
// other part, found infected, is refused. The control for the 403 is
// TestDownloadRescansOldMailAndRefusesByTheFreshVerdict (young mail downloads,
// old mail is refused by the fresh verdict); the probe that restores the abort
// turns this test red.
func TestAHungPartDoesNotStopAnInfectedOneBeingRefused(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	h.api.openRescan = rescan.NewOpenRescan(h.store, apiBlobs{h.api}, &hangOnPDF{}, nil, nil).WithSyncBudget(600 * time.Millisecond)
	srv := httptest.NewServer(blobHandler(h, a))
	defer srv.Close()

	old := seedRawMessage(t, h, a, []byte(rawWithAttachment))
	_, parts := partBlobIDs(t, h, a, old)
	seedAttachmentRows(t, h, a.UserID, old, []byte(rawWithAttachment), nil)
	ageMessage(t, h, a.UserID, old, 400)

	// The requested part hangs: served under its stored clean.
	if _, resp := getBody(t, srv, downloadURL(a.UserID.String(), parts["invoice.pdf"], "invoice.pdf")); resp.StatusCode != http.StatusOK {
		t.Errorf("the hung part got status %d, want 200 (fail open under its stored verdict)", resp.StatusCode)
	}
	// The other part was refreshed in the same pass and is refused.
	if _, resp := getBody(t, srv, downloadURL(a.UserID.String(), parts["payload.html"], "payload.html")); resp.StatusCode != http.StatusForbidden {
		t.Errorf("the infected part got status %d, want 403: the hung part shielded it", resp.StatusCode)
	}
}
