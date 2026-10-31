// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration tests for the blob endpoints (#85) against a real Postgres with
// RLS enforced AND a real encrypted blob store.
//
// The load-bearing one is TestDownloadRefusesAnotherUsersBlob. The blob store is
// CONTENT-ADDRESSED: the blobId a client sends IS a hash of the bytes. So the
// only thing standing between "user B learned user A's blobId" and "user B has
// user A's attachment" is that the route resolves the blob against the caller's
// own rows, under RLS — never against the hash alone. That is a claim about the
// code, and this test is what makes it a fact.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
)

// rawWithAttachment is a small multipart/mixed message: a text body (part 1) and
// two attachments — a PDF (part 2) and, deliberately, an HTML one (part 3),
// because an HTML attachment served inline from our own origin is stored XSS.
const rawWithAttachment = "From: Alice <alice@example.com>\r\n" +
	"To: You <you@example.com>\r\n" +
	"Subject: Invoice\r\n" +
	"Date: Mon, 6 Jan 2025 10:00:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"bnd\"\r\n" +
	"\r\n" +
	"--bnd\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"See attached.\r\n" +
	"--bnd\r\n" +
	"Content-Type: application/pdf; name=\"invoice.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQKJVBJTEVT\r\n" + // "%PDF-1.4\n%PILES"
	"--bnd\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"Content-Disposition: attachment; filename=\"payload.html\"\r\n" +
	"\r\n" +
	"<script>alert(document.domain)</script>\r\n" +
	"--bnd--\r\n"

// blobHandler mounts the real HTTP surface behind the only thing that ever
// injects an identity: the auth middleware (spec §10).
func blobHandler(h *composeHarness, f apiFixture) http.Handler {
	return auth.DevAuth(h.api.Handler(), f.UserID)
}

// TestDownloadServesAnAttachment is the day-one gap: the session advertises
// downloadUrl, Email/get hands out part blobIds, and until #85 every one of them
// was a 404.
func TestDownloadServesAnAttachment(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h, a, []byte(rawWithAttachment))

	// The blobIds come from Email/get, exactly as a client would get them.
	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{msgID.String()},
		"properties": []string{"id", "blobId", "attachments"},
	})
	list := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get returned %d emails", len(list))
	}
	email := list[0].(map[string]any)
	attachments := email["attachments"].([]any)
	if len(attachments) != 2 {
		t.Fatalf("attachments = %d, want 2 (pdf + html)", len(attachments))
	}

	byName := map[string]map[string]any{}
	for _, at := range attachments {
		obj := at.(map[string]any)
		byName[obj["name"].(string)] = obj
	}

	srv := httptest.NewServer(blobHandler(h, a))
	defer srv.Close()

	// 1. The PDF: served, with its bytes intact and the defensive headers on.
	pdf := byName["invoice.pdf"]
	resp := get(t, srv, downloadURL(a.UserID.String(), pdf["blobId"].(string), "invoice.pdf"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download invoice.pdf: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.HasPrefix(body, []byte("%PDF-1.4")) {
		t.Errorf("body = %q, want the decoded PDF bytes", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q", ct)
	}
	assertDefensiveHeaders(t, resp)

	// 2. The HTML attachment: NEVER text/html. This is the stored-XSS boundary.
	html := byName["payload.html"]
	resp2 := get(t, srv, downloadURL(a.UserID.String(), html["blobId"].(string), "payload.html"))
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("download payload.html: status %d", resp2.StatusCode)
	}
	if ct := resp2.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("an HTML attachment was served as %q — that is stored XSS on our own origin", ct)
	}
	if cd := resp2.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment disposition", cd)
	}
	assertDefensiveHeaders(t, resp2)

	// 3. The bare (unsuffixed) blobId is the whole raw message.
	resp3 := get(t, srv, downloadURL(a.UserID.String(), email["blobId"].(string), "message.eml"))
	defer func() { _ = resp3.Body.Close() }()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("download the message: status %d", resp3.StatusCode)
	}
	whole, _ := io.ReadAll(resp3.Body)
	if string(whole) != rawWithAttachment {
		t.Errorf("the whole-message download did not return the raw bytes (%d bytes)", len(whole))
	}
	if ct := resp3.Header.Get("Content-Type"); ct != "message/rfc822" {
		t.Errorf("Content-Type = %q, want message/rfc822", ct)
	}
}

// TestDownloadRefusesAnotherUsersBlob is THE security test for #85.
//
// User B knows user A's blobId exactly — the content hash and the part path.
// Every way B can ask for it must be a 404:
//
//	B's own account id + A's blobId   → the store lookup runs under B's RLS
//	                                     identity and finds nothing.
//	A's account id + A's blobId       → the {accountId} path segment is not an
//	                                     identity; it is checked against the
//	                                     authenticated user and rejected.
//
// If this test ever goes green by accident (say, because someone "optimized" the
// route to read the blob store directly by hash), the blob store's own per-user
// namespacing is the only thing left — and defense in depth is not a substitute
// for the check.
func TestDownloadRefusesAnotherUsersBlob(t *testing.T) {
	h := newComposeHarness(t)
	a, b := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h, a, []byte(rawWithAttachment))

	// A's own view: the exact blobIds, as A's client sees them.
	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{msgID.String()},
		"properties": []string{"id", "blobId", "attachments"},
	})
	email := res["list"].([]any)[0].(map[string]any)
	messageBlobID := email["blobId"].(string)
	partBlobID := email["attachments"].([]any)[0].(map[string]any)["blobId"].(string)

	// Sanity: A really can download it (otherwise the negative below proves
	// nothing at all).
	srvA := httptest.NewServer(blobHandler(h, a))
	defer srvA.Close()
	if resp := get(t, srvA, downloadURL(a.UserID.String(), partBlobID, "invoice.pdf")); resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("the owner cannot download their own blob: status %d", resp.StatusCode)
	}

	// Now B, authenticated as B, holding A's ids.
	srvB := httptest.NewServer(blobHandler(h, b))
	defer srvB.Close()
	for _, tc := range []struct{ name, url string }{
		{"B's account, A's part blob", downloadURL(b.UserID.String(), partBlobID, "invoice.pdf")},
		{"B's account, A's message blob", downloadURL(b.UserID.String(), messageBlobID, "message.eml")},
		{"A's account id in the path", downloadURL(a.UserID.String(), partBlobID, "invoice.pdf")},
		{"A's account id, message blob", downloadURL(a.UserID.String(), messageBlobID, "message.eml")},
	} {
		resp := get(t, srvB, tc.url)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 — B READ A'S ATTACHMENT (%d bytes)",
				tc.name, resp.StatusCode, len(body))
		}
		if bytes.Contains(body, []byte("%PDF")) || bytes.Contains(body, []byte("alice@example.com")) {
			t.Errorf("%s: the response body carries A's content", tc.name)
		}
	}
}

// TestUploadThenAttachThenDownload is the compose round trip the upload endpoint
// exists for: upload bytes, attach them to a draft by blobId, and read them back
// out through the attachment's download URL.
func TestUploadThenAttachThenDownload(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")

	srv := httptest.NewServer(blobHandler(h, a))
	defer srv.Close()

	content := []byte("%PDF-1.7\nthe quarterly numbers\n")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/jmap/upload/"+a.UserID.String()+"/", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/pdf")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: status %d", resp.StatusCode)
	}
	var up struct {
		AccountID string `json:"accountId"`
		BlobID    string `json:"blobId"`
		Type      string `json:"type"`
		Size      int64  `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&up); err != nil {
		t.Fatalf("upload response: %v", err)
	}
	if up.AccountID != a.UserID.String() || up.Type != "application/pdf" || up.Size != int64(len(content)) {
		t.Fatalf("upload response = %+v", up)
	}
	if _, err := hex.DecodeString(up.BlobID); err != nil || len(up.BlobID) != 64 {
		t.Fatalf("blobId = %q", up.BlobID)
	}

	// Attach it to a draft by blobId (RFC 8621 §4.6 attachments).
	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"create": map[string]any{
			"d": map[string]any{
				"from":        []map[string]any{{"email": "me@example.com"}},
				"to":          []map[string]any{{"email": "friend@example.net"}},
				"subject":     "the numbers",
				"bodyValues":  map[string]any{"1": map[string]any{"value": "attached"}},
				"textBody":    []map[string]any{{"partId": "1"}},
				"attachments": []map[string]any{{"blobId": up.BlobID, "type": "application/pdf", "name": "q3.pdf"}},
			},
		},
	})
	if nc, ok := res["notCreated"].(map[string]any); ok && len(nc) > 0 {
		t.Fatalf("notCreated: %v", nc)
	}
	created := res["created"].(map[string]any)["d"].(map[string]any)
	if created["hasAttachment"] != true {
		t.Errorf("hasAttachment = %v, want true", created["hasAttachment"])
	}

	// Read the draft back: its attachment part must be the bytes we uploaded.
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{created["id"].(string)},
		"properties": []string{"id", "attachments"},
	})
	attachments := got["list"].([]any)[0].(map[string]any)["attachments"].([]any)
	if len(attachments) != 1 {
		t.Fatalf("the draft has %d attachments, want 1", len(attachments))
	}
	at := attachments[0].(map[string]any)
	if at["name"] != "q3.pdf" || at["type"] != "application/pdf" {
		t.Errorf("attachment = %v", at)
	}

	dl := get(t, srv, downloadURL(a.UserID.String(), at["blobId"].(string), "q3.pdf"))
	defer func() { _ = dl.Body.Close() }()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download the attachment: status %d", dl.StatusCode)
	}
	body, _ := io.ReadAll(dl.Body)
	if !bytes.Equal(body, content) {
		t.Errorf("downloaded %q, want %q", body, content)
	}
}

// TestAttachRefusesAnotherUsersBlob: attaching is a READ of the blob, so it is
// the same authorization boundary as downloading — and a quieter way to steal
// (mail yourself the victim's attachment) if it were not checked.
func TestAttachRefusesAnotherUsersBlob(t *testing.T) {
	h := newComposeHarness(t)
	a, b := seedTwo(t, h.super)
	seedSendable(t, h.super, b, "b@example.com", "")
	msgID := seedRawMessage(t, h, a, []byte(rawWithAttachment))

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{msgID.String()},
		"properties": []string{"id", "attachments"},
	})
	victimBlobID := res["list"].([]any)[0].(map[string]any)["attachments"].([]any)[0].(map[string]any)["blobId"].(string)

	// B composes a draft attaching A's blob and mails it to themselves.
	out := call(t, h.api.emailSet, b.UserID, map[string]any{
		"create": map[string]any{
			"steal": map[string]any{
				"from":        []map[string]any{{"email": "b@example.com"}},
				"to":          []map[string]any{{"email": "b@example.com"}},
				"subject":     "not mine",
				"bodyValues":  map[string]any{"1": map[string]any{"value": "hi"}},
				"textBody":    []map[string]any{{"partId": "1"}},
				"attachments": []map[string]any{{"blobId": victimBlobID, "type": "application/pdf", "name": "loot.pdf"}},
			},
		},
	})
	if created, ok := out["created"].(map[string]any); ok && len(created) > 0 {
		t.Fatalf("B attached A's blob: %v", created)
	}
	e := setError(t, out, "notCreated", "steal")
	if e["type"] != setNotFound {
		t.Errorf("SetError type = %v, want %q", e["type"], setNotFound)
	}
}

// TestUploadRefusesAnotherAccountId: the {accountId} in the path is validated
// against the authenticated user, never used as one.
func TestUploadRefusesAnotherAccountId(t *testing.T) {
	h := newComposeHarness(t)
	a, b := seedTwo(t, h.super)

	srv := httptest.NewServer(blobHandler(h, a)) // authenticated as A
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/jmap/upload/"+b.UserID.String()+"/", strings.NewReader("bytes"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("uploading into another user's account: status = %d, want 404", resp.StatusCode)
	}
}

// --- helpers ------------------------------------------------------------------

func downloadURL(accountID, blobID, name string) string {
	return fmt.Sprintf("/jmap/download/%s/%s/%s", accountID, blobID, name)
}

func get(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// assertDefensiveHeaders checks the headers that keep attacker-controlled bytes
// from becoming attacker-controlled CODE on the operator's origin.
func assertDefensiveHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "sandbox") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment disposition", cd)
	}
}

// TestUploadRefusesOverStorageQuota proves the storage-quota enforcement point:
// an upload that would push the user past app.users.quota_storage_bytes is
// refused with 413 and records NOTHING, so the client is never told bytes were
// stored that weren't. The uploaded file itself is left as an unreferenced blob
// (later GC) — the row, which is what makes it the user's, is never written.
func TestUploadRefusesOverStorageQuota(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)

	// A tiny storage cap for this user (seedTwo leaves it at the schema default).
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.users SET quota_storage_bytes = 8 WHERE id = $1`, a.UserID.String()); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	srv := httptest.NewServer(blobHandler(h, a))
	defer srv.Close()

	before := blobCount(t, h.super, a.UserID)

	// Well over the 8-byte cap.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/jmap/upload/"+a.UserID.String()+"/",
		bytes.NewReader([]byte("this payload is comfortably over eight bytes")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-quota upload: status %d, want 413", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "overQuota") {
		t.Errorf("over-quota body = %q, want an overQuota error", string(body))
	}
	if after := blobCount(t, h.super, a.UserID); after != before {
		t.Errorf("blob rows changed %d -> %d; an over-quota upload must record nothing", before, after)
	}
}

// blobCount reports how many app.blobs rows a user has (read as super, so it is
// ground truth independent of RLS).
func blobCount(t *testing.T, super *pgxpool.Pool, userID ulid.ULID) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.blobs WHERE user_id = $1`, userID.String()).Scan(&n); err != nil {
		t.Fatalf("blob count: %v", err)
	}
	return n
}
