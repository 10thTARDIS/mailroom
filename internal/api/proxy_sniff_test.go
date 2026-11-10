// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// Synthetic signatures: just enough leading bytes to sniff, never a real image.
const (
	sigJPEG = "\xff\xd8\xff\xe0-synthetic-jpeg"
	sigGIF  = "GIF89a-synthetic-gif"
	sigWEBP = "RIFF\x10\x00\x00\x00WEBPVP8 -synthetic"
	sigAVIF = "\x00\x00\x00\x1cftypavif-synthetic"
)

// TestProxyDecidesOnTheBytesNotTheLabel (#26): a real raster image is served
// whatever its Content-Type says, as the SNIFFED type; and nothing that does not
// sniff as one is served whatever its label says.
func TestProxyDecidesOnTheBytesNotTheLabel(t *testing.T) {
	accepted := []struct {
		name, label, body, want string
	}{
		{"jpg label", "image/jpg", sigJPEG, "image/jpeg"},
		{"octet-stream png", "application/octet-stream", pngSig, "image/png"},
		{"missing type gif", "", sigGIF, "image/gif"},
		{"text label webp", "text/plain", sigWEBP, "image/webp"},
		{"wrong image label", "image/gif", sigJPEG, "image/jpeg"},
		{"avif", "binary/octet-stream", sigAVIF, "image/avif"},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			h := newProxyHarness(t, ProxyConfig{}, stubFetcher{resp: imgResp(200, tc.label, tc.body)}, "")
			rec := h.doOK()
			if rec.Code != 200 {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.want {
				t.Errorf("served Content-Type = %q, want the sniffed %q", got, tc.want)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("nosniff missing")
			}
			if rec.Body.String() != tc.body {
				t.Error("the peeked bytes were lost: body differs from upstream")
			}
		})
	}
	refused := []struct{ name, label, body string }{
		{"svg", "image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg"><script>1</script></svg>`},
		{"svg labelled png", "image/png", `<?xml version="1.0"?><svg/>`},
		{"html labelled png", "image/png", `<html><script>alert(1)</script></html>`},
		{"polyglot, text first", "image/png", "<!-- " + pngSig},
		{"signature not at the start", "image/jpeg", " " + sigJPEG},
		{"empty body", "image/png", ""},
		{"text", "text/plain", "hello"},
	}
	for _, tc := range refused {
		t.Run("refuse/"+tc.name, func(t *testing.T) {
			h := newProxyHarness(t, ProxyConfig{}, stubFetcher{resp: imgResp(200, tc.label, tc.body)}, "")
			rec := h.doOK()
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415", rec.Code)
			}
			if len(h.outcomes) != 1 || h.outcomes[0].Reason != reasonContentType {
				t.Errorf("outcomes = %+v, want one %s refusal", h.outcomes, reasonContentType)
			}
		})
	}
}

// A body that fails before its first byte is a body failure, not a type refusal.
func TestProxyBodyFailureBeforeFirstByteKeepsItsReason(t *testing.T) {
	h := newProxyHarness(t, ProxyConfig{}, stubFetcher{resp: &http.Response{StatusCode: 200,
		Header: http.Header{"Content-Type": {"image/png"}}, Body: errBody{err: io.ErrUnexpectedEOF}}}, "")
	if rec := h.doOK(); rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if len(h.outcomes) != 1 || h.outcomes[0].Reason != reasonBodyAborted {
		t.Errorf("outcomes = %+v, want body_aborted", h.outcomes)
	}
}

// TestProxyProtocolRelativeIsHTTPSAndOnlyThat (#26): //host/x reaches egress as
// https; every other scheme is still refused by egress's scheme rule.
func TestProxyProtocolRelativeIsHTTPSAndOnlyThat(t *testing.T) {
	if got := normalizeProxyTarget("//img.example/a.png"); got != "https://img.example/a.png" {
		t.Errorf("normalizeProxyTarget = %q", got)
	}
	for _, u := range []string{"javascript:alert(1)", "data:image/png;base64,AAAA", "ftp://img.example/a.png", "/relative.png"} {
		if got := normalizeProxyTarget(u); got != u {
			t.Errorf("%q was rewritten to %q; only the // form may be", u, got)
		}
	}
	// Through the REAL egress client: the scheme check is what refuses (control),
	// and a protocol-relative URL is no longer refused on its scheme. The host is
	// a reserved .example name, so the fetch fails at resolution, not on a scheme.
	p := newImageProxy(ProxyConfig{Enabled: true, Timeout: 2 * time.Second, MaxBytes: 1 << 20}, nil)
	p.client = newProxyEgressClient(p.cfg)
	for _, bad := range []string{"javascript:alert(1)", "data:text/html,x"} {
		_, _, fail := p.openProxyImage(context.Background(), bad)
		if fail == nil || fail.outcome.Reason != reasonSchemeRefused {
			t.Errorf("%q: fail = %+v, want scheme_refused", bad, fail)
		}
	}
	_, _, fail := p.openProxyImage(context.Background(), "//img.example/a.png")
	if fail == nil {
		t.Fatal("a reserved .example host cannot have served an image")
	}
	if fail.outcome.Reason == reasonSchemeRefused {
		t.Errorf("protocol-relative URL still refused on its scheme: %+v", fail)
	}
}
