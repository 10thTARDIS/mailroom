// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func csrfDo(t *testing.T, method, path string, hdr map[string]string) (*httptest.ResponseRecorder, bool, string) {
	t.Helper()
	var logs bytes.Buffer
	reached := false
	h := CSRFGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = io.WriteString(w, "ok")
	}), "/jmap", slog.New(slog.NewTextHandler(&logs, nil)))
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.AddCookie(&http.Cookie{Name: "mailroom_session", Value: "SECRET-COOKIE"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, reached, logs.String()
}

func TestCSRFGuardFetchMetadata(t *testing.T) {
	json := "application/json"
	for _, c := range []struct {
		name   string
		method string
		path   string
		hdr    map[string]string
		want   int
	}{
		{"cross-site", "POST", "/jmap", map[string]string{"Sec-Fetch-Site": "cross-site", "Content-Type": json}, 403},
		{"same-site", "POST", "/jmap", map[string]string{"Sec-Fetch-Site": "same-site", "Content-Type": json}, 403},
		{"same-origin", "POST", "/jmap", map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": json}, 200},
		{"none", "POST", "/jmap", map[string]string{"Sec-Fetch-Site": "none", "Content-Type": json}, 200},
		{"absent (older client)", "POST", "/jmap", map[string]string{"Content-Type": json}, 200},
		{"json with charset", "POST", "/jmap", map[string]string{"Content-Type": "application/json; charset=utf-8"}, 200},
		{"text/plain", "POST", "/jmap", map[string]string{"Content-Type": "text/plain"}, 415},
		{"form", "POST", "/jmap", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, 415},
		{"no content type", "POST", "/jmap", nil, 415},
		{"upload cross-site", "POST", "/jmap/upload/acc/", map[string]string{"Sec-Fetch-Site": "cross-site", "Content-Type": "image/png"}, 403},
		{"upload same-origin any type", "POST", "/jmap/upload/acc/", map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": "image/png"}, 200},
		{"cross-site GET unaffected", "GET", "/jmap/session", map[string]string{"Sec-Fetch-Site": "cross-site"}, 200},
		{"cross-site DELETE", "DELETE", "/mcp", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec, reached, logs := csrfDo(t, c.method, c.path, c.hdr)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if reached != (c.want == 200) {
				t.Errorf("handler reached = %v with status %d", reached, rec.Code)
			}
			if c.want != 200 && !strings.Contains(logs, "csrf:") {
				t.Errorf("refusal not logged: %q", logs)
			}
			if strings.Contains(logs, "SECRET-COOKIE") {
				t.Errorf("log leaked the cookie: %q", logs)
			}
		})
	}
}
