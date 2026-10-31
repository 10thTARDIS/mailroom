// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// PGP/MIME decrypt on open (#47 increment 6b, ADR-037), through Email/get and
// the download route, against a real Postgres with RLS and a real encrypted
// blob store.
//
// The decrypter here is go-crypto IN-TEST with a key the test generates: the
// crypto process's own decrypt path (opaque errors, SED refusal, caps, audit)
// is pinned in internal/pgpkeyd. What is pinned HERE is what the application
// does with plaintext once it has it — renders it through the ordinary body
// pipeline, scans attachments on demand, and writes it NOWHERE.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/clamd"
	"mailroom/internal/compose"
)

// plaintextMarker appears ONLY inside the ciphertext. Finding it anywhere in the
// database or the blob directory after a decrypt means plaintext was persisted.
const plaintextMarker = "zq-plaintext-marker-7f3a91"

const decryptedEntity = "From: CEO <ceo@yourbank.example>\r\n" +
	"Reply-To: thief@attacker.example\r\n" +
	"Content-Type: multipart/mixed; boundary=\"inner\"\r\n" +
	"\r\n" +
	"--inner\r\n" +
	"Content-Type: multipart/alternative; boundary=\"alt\"\r\n" +
	"\r\n" +
	"--alt\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nsecret text " + plaintextMarker + "\r\n" +
	"--alt\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>secret html " + plaintextMarker + "</p><script>alert(1)</script>\r\n" +
	"--alt--\r\n" +
	"--inner\r\n" +
	"Content-Type: application/pdf; name=\"plan.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"plan.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQKJSBwbGFuIHpxLXBsYWludGV4dC1tYXJrZXItN2YzYTkxCg==\r\n" + // "%PDF-1.4\n% plan zq-plaintext-marker-7f3a91\n"
	"--inner--\r\n"

const decryptedPDF = "%PDF-1.4\n% plan " + plaintextMarker + "\n"

// localDecrypter is go-crypto with a test-held key, counting calls.
type localDecrypter struct {
	mu    sync.Mutex
	key   *openpgp.Entity
	calls int
	fail  bool
}

// It never reports an embedded (RFC 3156 section 6.2) signature: the tests of
// that path use the real crypto service (pgp_signature_embedded_integration_test.go).
func (d *localDecrypter) Decrypt(_ context.Context, _, _ ulid.ULID, _ string, ct []byte, _ [][]byte) (DecryptResult, error) {
	d.mu.Lock()
	d.calls++
	fail := d.fail
	d.mu.Unlock()
	if fail {
		return DecryptResult{}, errors.New("crypto process: decrypt_failed: the message could not be decrypted")
	}
	blk, err := armor.Decode(bytes.NewReader(ct))
	if err != nil {
		return DecryptResult{}, err
	}
	md, err := openpgp.ReadMessage(blk.Body, openpgp.EntityList{d.key}, nil, nil)
	if err != nil {
		return DecryptResult{}, err
	}
	entity, err := io.ReadAll(md.UnverifiedBody)
	return DecryptResult{Entity: entity}, err
}

func (d *localDecrypter) count() int { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }

// fakeScanner records what it was asked to scan and answers as configured.
type fakeScanner struct {
	mu      sync.Mutex
	scanned [][]byte
	state   clamd.State
	sig     string
	err     error
}

func (s *fakeScanner) Scan(_ context.Context, data []byte) (clamd.Verdict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanned = append(s.scanned, append([]byte(nil), data...))
	if s.err != nil {
		return clamd.Verdict{}, s.err
	}
	return clamd.Verdict{State: s.state, Signature: s.sig}, nil
}

func (s *fakeScanner) set(st clamd.State, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state, s.err, s.scanned = st, err, nil
}

// setSig is set plus the detection name a flagged verdict carries.
func (s *fakeScanner) setSig(st clamd.State, sig string) {
	s.set(st, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sig = sig
}

func (s *fakeScanner) seen() [][]byte { s.mu.Lock(); defer s.mu.Unlock(); return s.scanned }

type decryptHarness struct {
	*composeHarness
	blobDir string
	dec     *localDecrypter
	scan    *fakeScanner
}

func newDecryptHarness(t *testing.T, withScanner bool) *decryptHarness {
	t.Helper()
	h := newAPIHarness(t)
	dir := filepath.Join(t.TempDir(), "blobs")
	blobs, err := blob.NewFSBlobStore(dir, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := openpgp.NewEntity("Reader", "", "reader@example.com", &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519,
	})
	if err != nil {
		t.Fatal(err)
	}
	dec := &localDecrypter{key: key}
	sc := &fakeScanner{state: clamd.Clean}
	opts := []Option{WithComposer(compose.NewComposer(blobs, fakeKeys{}, time.Millisecond)), WithDecrypter(dec)}
	if withScanner {
		opts = append(opts, WithOnDemandScanner(sc))
	}
	h.api = New(h.store, opts...)
	return &decryptHarness{composeHarness: &composeHarness{apiHarness: h, blobs: blobs}, blobDir: dir, dec: dec, scan: sc}
}

// encryptedRFC3156 encrypts entity to key and wraps it as RFC 3156 §4, by hand.
func encryptedRFC3156(t *testing.T, key *openpgp.Entity, entity string) []byte {
	t.Helper()
	var ct bytes.Buffer
	aw, err := armor.Encode(&ct, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := openpgp.Encrypt(aw, openpgp.EntityList{key}, nil, &openpgp.FileHints{IsBinary: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(entity))
	_ = w.Close()
	_ = aw.Close()
	return []byte("From: Alice <alice@example.com>\r\n" +
		"To: Reader <reader@example.com>\r\n" +
		"Subject: Quarterly report\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/encrypted; boundary=\"enc\"; protocol=\"application/pgp-encrypted\"\r\n" +
		"\r\n" +
		"This is an OpenPGP/MIME encrypted message (RFC 3156).\r\n" +
		"--enc\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
		"--enc\r\nContent-Type: application/octet-stream; name=\"encrypted.asc\"\r\n\r\n" +
		ct.String() + "\r\n--enc--\r\n")
}

var bodyProps = []string{"id", "textBody", "htmlBody", "attachments", "bodyValues", "replyTo",
	"encrypted", "decrypted", "pgpSignature"}

func getEmail(t *testing.T, h *decryptHarness, f apiFixture, id ulid.ULID, props []string) map[string]any {
	t.Helper()
	res := call(t, h.api.emailGet, f.UserID, map[string]any{
		"ids": []string{id.String()}, "properties": props,
		"fetchTextBodyValues": true, "fetchHTMLBodyValues": true,
	})
	list := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get = %#v", res)
	}
	return list[0].(map[string]any)
}

func TestDecryptOnOpenRendersThePlaintextThroughTheBodyPipeline(t *testing.T) {
	h := newDecryptHarness(t, true)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h.composeHarness, a, encryptedRFC3156(t, h.dec.key, decryptedEntity))

	// A list-shaped request asking only "is it encrypted?" DECRYPTS NOTHING.
	obj := getEmail(t, h, a, msgID, []string{"id", "encrypted", "decrypted"})
	if obj["encrypted"] != true || obj["decrypted"] != nil {
		t.Errorf("no body asked for: encrypted=%v decrypted=%v, want true and null", obj["encrypted"], obj["decrypted"])
	}
	if n := h.dec.count(); n != 0 {
		t.Fatalf("asking only for `encrypted` made %d decrypt calls, want 0", n)
	}

	obj = getEmail(t, h, a, msgID, bodyProps)
	if h.dec.count() != 1 {
		t.Errorf("decrypt calls = %d, want 1", h.dec.count())
	}
	if obj["encrypted"] != true || obj["decrypted"] != true {
		t.Fatalf("encrypted=%v decrypted=%v, want true/true", obj["encrypted"], obj["decrypted"])
	}
	values := obj["bodyValues"].(map[string]any)
	text := obj["textBody"].([]any)[0].(map[string]any)
	html := obj["htmlBody"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(text["partId"].(string), "e.") {
		t.Errorf("decrypted partId %q does not carry the decrypted prefix", text["partId"])
	}
	tv := values[text["partId"].(string)].(map[string]any)["value"].(string)
	if !strings.Contains(tv, "secret text "+plaintextMarker) {
		t.Errorf("text body = %q, want the plaintext", tv)
	}
	hv := values[html["partId"].(string)].(map[string]any)["value"].(string)
	if !strings.Contains(hv, "secret html") || strings.Contains(hv, "<script") {
		t.Errorf("html body = %q — the decrypted HTML must go through the same sanitiser as any mail", hv)
	}
	atts := obj["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["name"] != "plan.pdf" {
		t.Fatalf("attachments = %#v, want the decrypted plan.pdf", atts)
	}
	if blobID := atts[0].(map[string]any)["blobId"].(string); !strings.Contains(blobID, ".e.") {
		t.Errorf("decrypted attachment blobId %q does not name the decrypted tree", blobID)
	}
	// Inner identity headers never surface (pgpmime.Rewrap's allowlist).
	if rt := obj["replyTo"]; rt != nil {
		t.Errorf("replyTo = %#v: a Reply-To hidden in the ciphertext reached the Email", rt)
	}
	if obj["pgpSignature"] != nil {
		t.Errorf("pgpSignature = %#v for an unsigned entity", obj["pgpSignature"])
	}

	// FAILURE: the honest state, the stored structure, and no detail.
	h.dec.mu.Lock()
	h.dec.fail = true
	h.dec.mu.Unlock()
	obj = getEmail(t, h, a, msgID, bodyProps)
	if obj["encrypted"] != true || obj["decrypted"] != false {
		t.Errorf("failed decrypt: encrypted=%v decrypted=%v, want true/false", obj["encrypted"], obj["decrypted"])
	}
	for _, v := range obj["bodyValues"].(map[string]any) {
		if strings.Contains(v.(map[string]any)["value"].(string), plaintextMarker) {
			t.Error("plaintext was served after a failed decrypt")
		}
	}
}

// TestDecryptOnOpenWritesNothing measures checklist item 7 — "plaintext is never
// persisted" — against the stores rather than asserting it: every table in
// schema app, every file in the blob directory, before and after Email/get and
// a decrypted-part download.
func TestDecryptOnOpenWritesNothing(t *testing.T) {
	h := newDecryptHarness(t, true)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h.composeHarness, a, encryptedRFC3156(t, h.dec.key, decryptedEntity))

	before := snapshotStores(t, h)
	obj := getEmail(t, h, a, msgID, bodyProps)
	if obj["decrypted"] != true {
		t.Fatalf("CONTROL FAILED: the message did not decrypt, so 'nothing written' would be vacuous")
	}
	blobID := obj["attachments"].([]any)[0].(map[string]any)["blobId"].(string)
	srv := httptest.NewServer(blobHandler(h.composeHarness, a))
	defer srv.Close()
	body, resp := getBody(t, srv, downloadURL(a.UserID.String(), blobID, "plan.pdf"))
	if resp.StatusCode != http.StatusOK || string(body) != decryptedPDF {
		t.Fatalf("CONTROL FAILED: decrypted part download = %d %q", resp.StatusCode, body)
	}
	after := snapshotStores(t, h)

	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed across a decrypt-on-open: %v -> %v", k, v, after[k])
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("%s appeared across a decrypt-on-open", k)
		}
	}
	// And the marker is nowhere in the database, in any column of any table.
	rows, err := h.super.Query(context.Background(),
		`SELECT tablename FROM pg_tables WHERE schemaname = 'app'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) < 40 {
		t.Fatalf("catalog query saw only %d app tables; it is not looking where it thinks", len(tables))
	}
	for _, tbl := range tables {
		var n int
		if err := h.super.QueryRow(context.Background(),
			`SELECT count(*) FROM app.`+tbl+` t WHERE t::text LIKE '%`+plaintextMarker+`%'`).Scan(&n); err != nil {
			t.Errorf("app.%s: %v", tbl, err)
			continue
		}
		if n != 0 {
			t.Errorf("app.%s holds %d rows containing decrypted plaintext", tbl, n)
		}
	}
	// CONTROL on that search: it finds the marker when it IS present.
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET snippet = $1 WHERE id = $2`, plaintextMarker, msgID.String()); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.messages t WHERE t::text LIKE '%`+plaintextMarker+`%'`).Scan(&n)
	if n != 1 {
		t.Fatal("the marker search cannot find a marker that is there; the assertion above is vacuous")
	}
}

// snapshotStores counts every app table's rows, the change_log high-water mark,
// and every blob file with its size and mtime.
func snapshotStores(t *testing.T, h *decryptHarness) map[string]any {
	t.Helper()
	ctx := context.Background()
	out := map[string]any{}
	rows, err := h.super.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'app'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, tbl := range tables {
		var n int
		if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.`+tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out["rows:"+tbl] = n
	}
	var seq any
	_ = h.super.QueryRow(ctx, `SELECT max(seq) FROM app.change_log`).Scan(&seq)
	out["change_log:max(seq)"] = seq
	_ = filepath.WalkDir(h.blobDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := os.Stat(p)
		if err == nil {
			out["file:"+p] = info.Size()
			out["mtime:"+p] = info.ModTime().UnixNano()
		}
		return nil
	})
	return out
}

func TestDecryptedAttachmentIsScannedAtDownload(t *testing.T) {
	h := newDecryptHarness(t, true)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h.composeHarness, a, encryptedRFC3156(t, h.dec.key, decryptedEntity))
	obj := getEmail(t, h, a, msgID, bodyProps)
	pdfBlob := obj["attachments"].([]any)[0].(map[string]any)["blobId"].(string)
	textBlob := obj["textBody"].([]any)[0].(map[string]any)["blobId"].(string)
	srv := httptest.NewServer(blobHandler(h.composeHarness, a))
	defer srv.Close()
	url := downloadURL(a.UserID.String(), pdfBlob, "plan.pdf")

	// CLEAN: served, and the scanner saw exactly the decrypted bytes.
	h.scan.set(clamd.Clean, nil)
	body, resp := getBody(t, srv, url)
	if resp.StatusCode != http.StatusOK || string(body) != decryptedPDF {
		t.Fatalf("clean decrypted attachment: %d %q", resp.StatusCode, body)
	}
	if seen := h.scan.seen(); len(seen) != 1 || string(seen[0]) != decryptedPDF {
		t.Errorf("scanner saw %q, want exactly the decrypted part once", seen)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store for decrypted bytes", cc)
	}

	// FLAGGED: refused, with the verdict and the detection name, which the web
	// client needs to explain the refusal in-app rather than land on a bare 403.
	h.scan.setSig(clamd.Flagged, "Eicar-Test-Signature")
	body, resp = getBody(t, srv, url)
	if got := resp.Header.Get("X-Mailroom-Attachment-Signature"); got != "Eicar-Test-Signature" {
		t.Errorf("signature header = %q, want Eicar-Test-Signature", got)
	}
	// A HEAD carries the same refusal (the client checks with one before it
	// navigates), and no body.
	if hr, err := http.Head(srv.URL + url); err != nil {
		t.Fatal(err)
	} else {
		_ = hr.Body.Close()
		if hr.StatusCode != http.StatusForbidden || hr.Header.Get("X-Mailroom-Attachment-Scan") != "flagged" ||
			hr.Header.Get("X-Mailroom-Attachment-Signature") != "Eicar-Test-Signature" {
			t.Errorf("HEAD of a flagged decrypted attachment: %d scan=%q sig=%q", hr.StatusCode,
				hr.Header.Get("X-Mailroom-Attachment-Scan"), hr.Header.Get("X-Mailroom-Attachment-Signature"))
		}
	}
	if resp.StatusCode != http.StatusForbidden || bytes.Contains(body, []byte("%PDF")) {
		t.Errorf("flagged decrypted attachment: status %d, body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Mailroom-Attachment-Scan"); got != "flagged" {
		t.Errorf("scan header = %q, want flagged", got)
	}
	// PREVIEW is the same gate.
	if _, resp := getBody(t, srv, previewURL(a.UserID.String(), pdfBlob, "plan.pdf")); resp.StatusCode != http.StatusForbidden {
		t.Errorf("flagged decrypted attachment previewed with status %d", resp.StatusCode)
	}

	// SCANNER DOWN: unknown, which blocks exactly as flagged does (§5b).
	h.scan.set(clamd.Clean, errors.New("clamd unreachable"))
	_, resp = getBody(t, srv, url)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Mailroom-Attachment-Scan") != "unknown" {
		t.Errorf("scanner outage: status %d scan %q, want 403 unknown", resp.StatusCode, resp.Header.Get("X-Mailroom-Attachment-Scan"))
	}

	// A RELEASE of the same content (by digest) opens the door, as everywhere.
	h.scan.set(clamd.Flagged, nil)
	sum := sha256Of([]byte(decryptedPDF))
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.attachment_releases (user_id, part_hash) VALUES ($1, $2)`, a.UserID.String(), sum); err != nil {
		t.Fatal(err)
	}
	if body, resp := getBody(t, srv, url); resp.StatusCode != http.StatusOK || string(body) != decryptedPDF {
		t.Errorf("released content still refused: %d", resp.StatusCode)
	}

	// BODY TEXT is not an attachment leaf: not scanned, not gated — as at ingest.
	h.scan.set(clamd.Flagged, nil)
	if _, resp := getBody(t, srv, downloadURL(a.UserID.String(), textBlob, "body.txt")); resp.StatusCode != http.StatusOK {
		t.Errorf("decrypted body text download: status %d", resp.StatusCode)
	}
	if n := len(h.scan.seen()); n != 0 {
		t.Errorf("body text was sent to the scanner (%d scans)", n)
	}

	// DECRYPT FAILS: no bytes, no detail.
	h.dec.mu.Lock()
	h.dec.fail = true
	h.dec.mu.Unlock()
	body, resp = getBody(t, srv, url)
	if resp.StatusCode != http.StatusUnprocessableEntity || bytes.Contains(body, []byte("%PDF")) {
		t.Errorf("undecryptable part: %d %q", resp.StatusCode, body)
	}

	// A decrypted part cannot be attached to a draft: its plaintext would be stored.
	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"create": map[string]any{"d1": map[string]any{
			"mailboxIds":  map[string]bool{a.InboxID.String(): true},
			"subject":     "fwd",
			"attachments": []map[string]any{{"blobId": pdfBlob, "type": "application/pdf", "name": "plan.pdf"}},
		}},
	})
	if nc, _ := res["notCreated"].(map[string]any); nc["d1"] == nil {
		t.Errorf("a decrypted part was attached to a draft: %#v", res)
	}
}

// With no scanner configured, decrypted attachments are `unscanned`, which does
// not block (ADR-027 §8) — the same as every attachment on such an instance.
func TestDecryptedAttachmentWithNoScannerIsServed(t *testing.T) {
	h := newDecryptHarness(t, false)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h.composeHarness, a, encryptedRFC3156(t, h.dec.key, decryptedEntity))
	obj := getEmail(t, h, a, msgID, bodyProps)
	pdfBlob := obj["attachments"].([]any)[0].(map[string]any)["blobId"].(string)
	srv := httptest.NewServer(blobHandler(h.composeHarness, a))
	defer srv.Close()
	if body, resp := getBody(t, srv, downloadURL(a.UserID.String(), pdfBlob, "plan.pdf")); resp.StatusCode != http.StatusOK || string(body) != decryptedPDF {
		t.Errorf("no scanner: %d %q", resp.StatusCode, body)
	}
}

func sha256Of(b []byte) []byte { s := sha256.Sum256(b); return s[:] }
