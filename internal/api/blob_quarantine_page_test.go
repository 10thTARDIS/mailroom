// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func quarantineReq(mode string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/jmap/download/A/B.1/x.pdf", nil)
	if mode != "" {
		r.Header.Set("Sec-Fetch-Mode", mode)
	}
	return r
}

// "Open link in new tab" on a blocked attachment reaches the route as a
// navigation (#805): it must read as a page, with the same status and headers,
// and never carry content.
func TestBlobQuarantinedNavigationGetsAnExplainingPage(t *testing.T) {
	rec := httptest.NewRecorder()
	blobQuarantined(rec, quarantineReq("navigate"), "flagged", "Eicar-Test-Signature", false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"Attachment blocked", "flagged by the malware scanner", "Detection: Eicar-Test-Signature", `href="/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	// The contract that did not change.
	h := rec.Header()
	if h.Get("X-Mailroom-Attachment-Scan") != "flagged" || h.Get("X-Mailroom-Attachment-Signature") != "Eicar-Test-Signature" ||
		h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers changed: %v", h)
	}
	// Meets the app's CSP: nothing may run or load, and the page asks for nothing.
	if csp := h.Get("Content-Security-Policy"); csp != "default-src 'none'; sandbox; frame-ancestors 'none'" {
		t.Errorf("CSP = %q", csp)
	}
	for _, bad := range []string{"<script", "<style", " style=", " onclick", "<img", "<link", "<iframe"} {
		if strings.Contains(strings.ToLower(body), bad) {
			t.Errorf("page contains %q", bad)
		}
	}
}

// A fetch() keeps today's answer, so the web client's own handling and every
// other programmatic caller are untouched.
func TestBlobQuarantinedFetchKeepsTheJSON(t *testing.T) {
	for _, mode := range []string{"cors", "same-origin", ""} {
		rec := httptest.NewRecorder()
		blobQuarantined(rec, quarantineReq(mode), "unknown", "", false)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("mode %q: status = %d", mode, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("mode %q: Content-Type = %q, want JSON", mode, ct)
		}
		if !strings.Contains(rec.Body.String(), "attachmentQuarantined") {
			t.Errorf("mode %q: body = %s", mode, rec.Body.String())
		}
	}
}

// The detection name is scanner text; it is escaped, whatever it holds, and it
// appears only for a flagged part.
func TestBlobQuarantinedPageEscapesTheDetectionName(t *testing.T) {
	rec := httptest.NewRecorder()
	blobQuarantined(rec, quarantineReq("navigate"), "flagged", `<script>alert(1)</script>&"'`, false)
	body := rec.Body.String()
	if strings.Contains(body, "<script>") {
		t.Fatalf("detection name reached the page as markup:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;&amp;&#34;&#39;") {
		t.Errorf("escaped name missing:\n%s", body)
	}
	// unknown never names a detection, even if one is passed.
	rec = httptest.NewRecorder()
	blobQuarantined(rec, quarantineReq("navigate"), "unknown", "Eicar", false)
	if strings.Contains(rec.Body.String(), "Eicar") || !strings.Contains(rec.Body.String(), "could not be checked") {
		t.Errorf("unknown page wrong:\n%s", rec.Body.String())
	}
}

// A preview frame gets the same page, framable by our own origin only, and
// without a link the frame could not follow.
func TestBlobQuarantinedPreviewFrameIsFramableBySelfOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	blobQuarantined(rec, quarantineReq("navigate"), "flagged", "X", true)
	h := rec.Header()
	if h.Get("X-Frame-Options") != "SAMEORIGIN" || h.Get("Content-Security-Policy") != "default-src 'none'; sandbox; frame-ancestors 'self'" {
		t.Errorf("preview refusal headers = %v", h)
	}
	if strings.Contains(rec.Body.String(), "<a ") {
		t.Error("framed page carries a link")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d", rec.Code)
	}
}
