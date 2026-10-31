// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The attachment quarantine's enforcement point (#396, ADR-027 §8), against a
// real Postgres with RLS and a real encrypted blob store.
//
// ⚠ EVERY TEST HERE IS NEGATIVE-CONTROLLED, AND THAT IS NOT DECORATION. Three
// assertions about the attachment sandbox once passed against a page that could
// not have loaded at all (#131) — the tests proved nothing and looked green for
// weeks. A refusal test has exactly that shape: "the bytes did not arrive" is
// also what a broken fixture, a wrong URL, an unseeded message and a 404 all
// look like.
//
// So each test below establishes that the SAME request succeeds before the state
// that should block it exists, or after the release that should unblock it. The
// 403 is then attributable to the verdict rather than to the harness.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/attachscan"
	"mailroom/internal/mime"
	"mailroom/internal/store"
)

// seedAttachmentRows projects rawWithAttachment's parts into app.attachments
// with the digests production would compute — attachscan.PartHash over the
// decoded bytes, not a digest invented here. A test that keyed content its own
// way would be exercising a different identity from the one the quarantine uses.
func seedAttachmentRows(t *testing.T, h *composeHarness, userID, msgID ulid.ULID, raw []byte, status map[string]string) map[string][]byte {
	t.Helper()
	msg, err := mime.ParseBytes(raw)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	hashes := map[string][]byte{}
	for _, ap := range msg.AttachmentParts() {
		st := status[ap.Path]
		if st == "" {
			st = "clean"
		}
		hash := attachscan.PartHash(ap)
		hashes[ap.Path] = hash
		if _, err := h.super.Exec(context.Background(), `
			INSERT INTO app.attachments
				(id, user_id, message_id, part_path, filename, content_type,
				 size_bytes, scan_status, part_hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (message_id, part_path) DO UPDATE
			   SET scan_status = EXCLUDED.scan_status, part_hash = EXCLUDED.part_hash`,
			store.NewULID().String(), userID.String(), msgID.String(), ap.Path,
			ap.Node.Filename, ap.Node.ContentType, ap.Node.OctetSize, st, hash); err != nil {
			t.Fatalf("seed attachment row %s: %v", ap.Path, err)
		}
	}
	return hashes
}

// partBlobIDs asks Email/get for the message's part blobIds, exactly as a client
// would — so the URLs under test are the ones a client is actually handed.
func partBlobIDs(t *testing.T, h *composeHarness, f apiFixture, msgID ulid.ULID) (messageBlob string, byName map[string]string) {
	t.Helper()
	res := call(t, h.api.emailGet, f.UserID, map[string]any{
		"ids":        []string{msgID.String()},
		"properties": []string{"id", "blobId", "attachments"},
	})
	email := res["list"].([]any)[0].(map[string]any)
	byName = map[string]string{}
	for _, at := range email["attachments"].([]any) {
		obj := at.(map[string]any)
		byName[obj["name"].(string)] = obj["blobId"].(string)
	}
	return email["blobId"].(string), byName
}

// TestQuarantineGateRefusesFlaggedAndUnknown is the enforcement claim itself.
//
// ⚠ STEP 1 IS THE NEGATIVE CONTROL AND IT IS THE MOST IMPORTANT STEP. Without
// it, every later 403 could be produced by a fixture that never worked, and this
// file would be #131 again with a different subject.
func TestQuarantineGateRefusesFlaggedAndUnknown(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h, a, []byte(rawWithAttachment))
	messageBlob, byName := partBlobIDs(t, h, a, msgID)

	srv := httptest.NewServer(blobHandler(h, a))
	defer srv.Close()

	pdfURL := downloadURL(a.UserID.String(), byName["invoice.pdf"], "invoice.pdf")
	htmlURL := downloadURL(a.UserID.String(), byName["payload.html"], "payload.html")

	// ── 1. CONTROL: with no projected rows at all, both parts download. ──────
	// This is also the `unscanned` case in disguise and it must stay open: an
	// instance running no scanner has no rows and no verdicts, and a gate that
	// blocked on absence would break every attachment in that mailbox.
	if body, resp := getBody(t, srv, pdfURL); resp.StatusCode != http.StatusOK {
		t.Fatalf("CONTROL FAILED: the PDF is not downloadable before any verdict exists (status %d) — "+
			"every refusal below would then prove nothing", resp.StatusCode)
	} else if !bytes.HasPrefix(body, []byte("%PDF-1.4")) {
		t.Fatalf("CONTROL FAILED: the PDF download returned %q, not the part bytes", body)
	}

	// ── 2. Flag one part, mark the other unknown. ───────────────────────────
	seedAttachmentRows(t, h, a.UserID, msgID, []byte(rawWithAttachment), map[string]string{
		"2": "flagged",
		"3": "unknown",
	})

	// ⚠ Errorf, not Fatalf, from here down: a Fatalf makes every assertion
	// below it unreachable, so a probe that deletes the mechanism cannot tell
	// which of them actually guard it.
	body, resp := getBody(t, srv, pdfURL)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a FLAGGED part downloaded with status %d, want 403", resp.StatusCode)
	}
	if bytes.Contains(body, []byte("%PDF")) {
		t.Errorf("the refusal body contains the part bytes: %q", body)
	}
	if got := resp.Header.Get("X-Mailroom-Attachment-Scan"); got != "flagged" {
		t.Errorf("X-Mailroom-Attachment-Scan = %q, want flagged", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — a cached 403 survives the release that clears it", got)
	}

	// ⚠ `unknown` GETS THE SAME DOOR (§5b). If it did not, an attacker would
	// never have to beat the scanner, only avoid it: pad past the size limit or
	// send an encrypted zip with the password in the body.
	body3, resp3 := getBody(t, srv, htmlURL)
	if resp3.StatusCode != http.StatusForbidden {
		t.Errorf("an UNKNOWN part downloaded with status %d, want 403 — §5b gives it the same "+
			"enforcement as flagged", resp3.StatusCode)
	}
	if bytes.Contains(body3, []byte("<script>")) {
		t.Errorf("the refusal body contains the part bytes: %q", body3)
	}
	if got := resp3.Header.Get("X-Mailroom-Attachment-Scan"); got != "unknown" {
		t.Errorf("X-Mailroom-Attachment-Scan = %q, want unknown", got)
	}

	// And the two states say DIFFERENT things, which is the other half of §5b:
	// "we could not check this" is not "we checked and it is bad".
	if flagged, unknown := errorDetail(t, body), errorDetail(t, body3); flagged == unknown {
		t.Errorf("flagged and unknown produced identical wording %q — §5b asks for the same "+
			"friction and different explanations", flagged)
	}

	// ── 3. The PREVIEW route is gated too. ──────────────────────────────────
	// §8 names the download route as the single enforcement point because it is
	// the one that covers CID rendering and foreign clients — but this route
	// hands the bytes to a browser's PDF engine for INLINE rendering, which is
	// the more dangerous of the two doors.
	_, presp := getBody(t, srv, previewURL(a.UserID.String(), byName["invoice.pdf"], "invoice.pdf"))
	if presp.StatusCode != http.StatusForbidden {
		t.Errorf("the preview route served a flagged part with status %d, want 403", presp.StatusCode)
	}

	// ── 4. The WHOLE MESSAGE is deliberately NOT blocked (§11, §12). ────────
	// Refusing to hand someone their own mail is user-hostile, and a message
	// with one bad attachment must stay readable, forwardable and exportable.
	// It must not be silent about it, though.
	whole, wresp := getBody(t, srv, downloadURL(a.UserID.String(), messageBlob, "message.eml"))
	if wresp.StatusCode != http.StatusOK {
		t.Errorf("the raw message download was refused (status %d) — the message text must stay "+
			"readable even when a part is quarantined", wresp.StatusCode)
	}
	if len(whole) != len(rawWithAttachment) {
		t.Errorf("the raw message download returned %d bytes, want %d", len(whole), len(rawWithAttachment))
	}
	if got := wresp.Header.Get("X-Mailroom-Attachment-Scan"); got != "flagged" {
		t.Errorf("the raw message download's X-Mailroom-Attachment-Scan = %q, want flagged — "+
			"an ungated door must at least not be a silent one", got)
	}

	// ── 5. CONTROL AGAIN: clear the verdict, and the same URL serves. ───────
	// This is what makes the 403 attributable to the STATE. Without it, a route
	// that 403'd unconditionally would pass every assertion above.
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.attachments SET scan_status='clean' WHERE message_id=$1 AND part_path='2'`,
		msgID.String()); err != nil {
		t.Fatalf("clear verdict: %v", err)
	}
	if body5, resp5 := getBody(t, srv, pdfURL); resp5.StatusCode != http.StatusOK {
		t.Errorf("after the verdict was cleared the PDF is still refused (status %d) — the gate is "+
			"not keyed on the verdict", resp5.StatusCode)
	} else if !bytes.HasPrefix(body5, []byte("%PDF-1.4")) {
		t.Errorf("after clearing, the download returned %q", body5)
	}
}

// TestQuarantineReleaseActsOnContentNotOneRow is ADR-027 §8's other half.
//
// ⚠ THE BUG THIS EXCLUDES HAS ALREADY SHIPPED ONCE IN THIS PROJECT, in a
// different place: a writeback that hit 1 of N copies. The same attachment sits
// on N different messages (in practice most quarantined rows are shared this
// way), so a decision keyed on a row would leave identical
// bytes reachable through a message it did not name.
func TestQuarantineReleaseActsOnContentNotOneRow(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)

	// Two DIFFERENT messages carrying the SAME attachment bytes. The second
	// differs only in its Subject, so the messages hash differently while part
	// 2's bytes are identical — which is exactly the shape the corpus has.
	rawOther := "Subject: Another\r\n" + rawWithAttachment
	msgA := seedRawMessage(t, h, a, []byte(rawWithAttachment))
	msgB := seedRawMessage(t, h, a, []byte(rawOther))

	hashesA := seedAttachmentRows(t, h, a.UserID, msgA, []byte(rawWithAttachment), nil)
	hashesB := seedAttachmentRows(t, h, a.UserID, msgB, []byte(rawOther), nil)
	if !bytes.Equal(hashesA["2"], hashesB["2"]) {
		t.Fatalf("FIXTURE FAILED: the two messages' part 2 digests differ, so this test is not "+
			"measuring shared content at all (%x vs %x)", hashesA["2"], hashesB["2"])
	}

	_, byNameA := partBlobIDs(t, h, a, msgA)
	_, byNameB := partBlobIDs(t, h, a, msgB)
	srv := httptest.NewServer(blobHandler(h, a))
	defer srv.Close()
	urlA := downloadURL(a.UserID.String(), byNameA["invoice.pdf"], "invoice.pdf")
	urlB := downloadURL(a.UserID.String(), byNameB["invoice.pdf"], "invoice.pdf")

	// CONTROL: both start clean and both download.
	for name, u := range map[string]string{"A": urlA, "B": urlB} {
		if _, resp := getBody(t, srv, u); resp.StatusCode != http.StatusOK {
			t.Fatalf("CONTROL FAILED: message %s's copy is not downloadable while clean (status %d)",
				name, resp.StatusCode)
		}
	}

	// ── 1. Flag ONLY message A's copy, through the production write path. ───
	// The store must propagate it to B, whose row still says `clean`.
	ctx := context.Background()
	if err := h.api.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		return tx.SetScanVerdicts(ctx, msgA, []store.ScanVerdict{
			{PartPath: "2", Status: "flagged", PartHash: hashesA["2"]},
		}, nil)
	}); err != nil {
		t.Fatalf("flag A's copy: %v", err)
	}

	var bStatus string
	if err := h.super.QueryRow(ctx,
		`SELECT scan_status FROM app.attachments WHERE message_id=$1 AND part_path='2'`,
		msgB.String()).Scan(&bStatus); err != nil {
		t.Fatalf("read B's row: %v", err)
	}
	if bStatus != "flagged" {
		t.Errorf("B's copy of the same bytes is %q after A's copy was flagged, want flagged — "+
			"a quarantine that reaches one of N copies is the writeback bug in a new place", bStatus)
	}
	if _, resp := getBody(t, srv, urlB); resp.StatusCode != http.StatusForbidden {
		t.Errorf("message B's copy of flagged content downloaded with status %d, want 403", resp.StatusCode)
	}

	// ⚠ AND B GOT A CHANGE ROW. B's Email object changed — its quarantine state
	// moved — so a client that never refetches B would keep offering a download
	// the server now refuses.
	var bChanges int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_id=$2 AND object_type='Email'`,
		a.UserID.String(), msgB.String()).Scan(&bChanges); err != nil {
		t.Fatalf("count B's change rows: %v", err)
	}
	if bChanges == 0 {
		t.Errorf("propagating the flag to message B appended no change_log row — clients will not " +
			"learn that B's attachment is now quarantined")
	}

	// ── 2. Release from A. Both copies must open. ───────────────────────────
	res := call(t, h.api.emailReleaseAttachment, a.UserID, map[string]any{
		"id": msgA.String(), "partId": "2",
	})
	affected := res["affectedEmailIds"].([]any)
	if len(affected) != 2 {
		t.Errorf("the release reported %d affected emails, want 2 — a release is about CONTENT, "+
			"and the caller must be able to see it reached more than the message they named: %v",
			len(affected), affected)
	}
	for name, u := range map[string]string{"A": urlA, "B": urlB} {
		body, resp := getBody(t, srv, u)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("after the release, message %s's copy is still refused (status %d)", name, resp.StatusCode)
			continue
		}
		if !bytes.HasPrefix(body, []byte("%PDF-1.4")) {
			t.Errorf("after the release, message %s's copy returned %q", name, body)
		}
	}

	// ⚠ THE VERDICT SURVIVED THE RELEASE. ADR-027 §5 modelled `released` as a
	// value of scan_status, which would have erased what the scanner said — and
	// §5's own stability table means it would never have come back. Here the
	// row still reads `flagged` and the gate is open anyway, so a client can
	// say "we flagged this, you released it".
	var afterRelease string
	if err := h.super.QueryRow(ctx,
		`SELECT scan_status FROM app.attachments WHERE message_id=$1 AND part_path='2'`,
		msgA.String()).Scan(&afterRelease); err != nil {
		t.Fatalf("read A's row: %v", err)
	}
	if afterRelease != "flagged" {
		t.Errorf("scan_status is %q after a release, want flagged — the release must not destroy "+
			"the verdict it overrides", afterRelease)
	}

	// ── 3. A RE-SCAN MUST NOT RESURRECT THE QUARANTINE. ─────────────────────
	// §5's warning: a malicious verdict essentially never reverts, so re-scan
	// WILL re-flag a released attachment. Under the old model a forgotten
	// `IS DISTINCT FROM 'released'` in any writer would revoke the decision;
	// here nothing a scanner writes can reach the release table, and this is
	// the test that says so rather than the comment.
	if err := h.api.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		return tx.SetScanVerdicts(ctx, msgA, []store.ScanVerdict{
			{PartPath: "2", Status: "flagged", PartHash: hashesA["2"], Signature: "Eicar-Test-Signature"},
		}, nil)
	}); err != nil {
		t.Fatalf("re-scan A: %v", err)
	}
	if _, resp := getBody(t, srv, urlA); resp.StatusCode != http.StatusOK {
		t.Errorf("a re-scan re-flagging the part revoked the user's release (status %d) — that is "+
			"the recurring fight §5 exists to prevent", resp.StatusCode)
	}

	// ── 4. Revoking the release re-arms the gate on every copy. ─────────────
	call(t, h.api.emailReleaseAttachment, a.UserID, map[string]any{
		"id": msgA.String(), "partId": "2", "revoke": true,
	})
	for name, u := range map[string]string{"A": urlA, "B": urlB} {
		if _, resp := getBody(t, srv, u); resp.StatusCode != http.StatusForbidden {
			t.Errorf("after revoking the release, message %s's copy is still served (status %d)",
				name, resp.StatusCode)
		}
	}
}

// TestQuarantinedPartCannotBeAttachedToADraft closes ADR-027 §11's "a user
// forwards malware" path.
//
// ⚠ THIS IS A STRICTER DOOR THAN THE DOWNLOAD, deliberately. Downloading
// quarantined bytes is a decision about the user's own machine; attaching them
// puts them on the wire to somebody else.
func TestQuarantinedPartCannotBeAttachedToADraft(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	msgID := seedRawMessage(t, h, a, []byte(rawWithAttachment))
	_, byName := partBlobIDs(t, h, a, msgID)
	blobID := byName["invoice.pdf"]

	create := func(key string) map[string]any {
		return map[string]any{
			"create": map[string]any{
				key: map[string]any{
					"from":        []map[string]any{{"email": "me@example.com"}},
					"to":          []map[string]any{{"email": "friend@example.net"}},
					"subject":     "fwd",
					"bodyValues":  map[string]any{"1": map[string]any{"value": "see attached"}},
					"textBody":    []map[string]any{{"partId": "1"}},
					"attachments": []any{map[string]any{"blobId": blobID, "type": "application/pdf"}},
				},
			},
		}
	}

	// CONTROL: with no verdict, the attach succeeds. Without this the refusal
	// below could be any of a dozen unrelated draft-creation failures — a
	// missing identity, a bad blobId, an unseeded mailbox.
	res := call(t, h.api.emailSet, a.UserID, create("c1"))
	if created, _ := res["created"].(map[string]any); len(created) != 1 {
		t.Fatalf("CONTROL FAILED: attaching an unscanned part did not create a draft (%#v) — "+
			"the refusal below would prove nothing", res)
	}

	seedAttachmentRows(t, h, a.UserID, msgID, []byte(rawWithAttachment), map[string]string{"2": "flagged"})

	res2 := call(t, h.api.emailSet, a.UserID, create("c2"))
	if created, _ := res2["created"].(map[string]any); len(created) != 0 {
		t.Errorf("a draft was created carrying a QUARANTINED attachment — §11's forward-malware path: %#v", created)
	}
	e := setError(t, res2, "notCreated", "c2")
	if e == nil {
		t.Fatalf("Email/set did not refuse the quarantined attachment: %#v", res2)
	}
	if desc, _ := e["description"].(string); !strings.Contains(desc, "quarantined") {
		t.Errorf("SetError description = %q, want it to name the quarantine so a client can explain it", desc)
	}
}

// TestAttachmentScanPropertyReportsReleasedAt is #449: the attachment viewer
// wants to print "released by you · <date>", which needs a timestamp on the
// user's own release decision, not just the `released` bool.
//
// ⚠ THE CONTROL IS STEP 1, as elsewhere in this file: without it, `releasedAt`
// reading null below could just as easily mean the property is broken as mean
// the fixture has not released anything yet.
func TestAttachmentScanPropertyReportsReleasedAt(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h, a, []byte(rawWithAttachment))
	seedAttachmentRows(t, h, a.UserID, msgID, []byte(rawWithAttachment), map[string]string{"2": "flagged"})

	getPart2 := func() map[string]any {
		t.Helper()
		res := call(t, h.api.emailGet, a.UserID, map[string]any{
			"ids":        []string{msgID.String()},
			"properties": []string{"id", "attachmentScan"},
		})
		email := res["list"].([]any)[0].(map[string]any)
		scan, ok := email["attachmentScan"].(map[string]any)
		if !ok {
			t.Fatalf("Email.attachmentScan is %#v, want an object", email["attachmentScan"])
		}
		parts, ok := scan["parts"].(map[string]any)
		if !ok {
			t.Fatalf("attachmentScan.parts is %#v, want an object", scan["parts"])
		}
		part, ok := parts["2"].(map[string]any)
		if !ok {
			t.Fatalf("attachmentScan.parts[\"2\"] is %#v, want an object", parts["2"])
		}
		return part
	}

	// ── 1. CONTROL: before any release, released is false and releasedAt is
	// null. Without this, "releasedAt is null" after step 2's assertion would
	// prove nothing about whether the field ever gets populated at all.
	before := getPart2()
	if before["released"] != false {
		t.Fatalf("CONTROL FAILED: released = %v before any release, want false", before["released"])
	}
	if before["releasedAt"] != nil {
		t.Errorf("releasedAt = %v before any release, want null", before["releasedAt"])
	}

	// ── 2. Release, through the production JMAP method, and re-read. ────────
	releaseStart := time.Now().UTC()
	call(t, h.api.emailReleaseAttachment, a.UserID, map[string]any{
		"id": msgID.String(), "partId": "2",
	})

	after := getPart2()
	if after["released"] != true {
		t.Fatalf("released = %v after Email/releaseAttachment, want true", after["released"])
	}
	rawAt, ok := after["releasedAt"].(string)
	if !ok || rawAt == "" {
		t.Fatalf("releasedAt = %#v after a release, want a UTCDate string", after["releasedAt"])
	}
	parsed, err := time.Parse(time.RFC3339, rawAt)
	if err != nil {
		t.Fatalf("releasedAt = %q did not parse as a JMAP UTCDate: %v", rawAt, err)
	}
	if parsed.Before(releaseStart.Add(-time.Minute)) || parsed.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("releasedAt = %v is not within the last minute of the release (started %v)",
			parsed, releaseStart)
	}
}

// getBody performs the request and returns the body with the response, so a
// refusal can be checked for the absence of the bytes as well as for its status.
func getBody(t *testing.T, srv *httptest.Server, url string) ([]byte, *http.Response) {
	t.Helper()
	resp := get(t, srv, url)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body, resp
}

// errorDetail pulls the human-readable half out of a JSON error response.
func errorDetail(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Detail
}

// previewURL is downloadURL's inline sibling — the second door onto the same
// bytes, and the one that renders them rather than saving them.
func previewURL(accountID, blobID, name string) string {
	return fmt.Sprintf("/jmap/preview/%s/%s/%s", accountID, blobID, name)
}

// ── #410: the browser cache must not be able to defeat the gate ─────────────
//
// ⚠ Go's httptest client implements NO HTTP CACHE, so an in-process test cannot
// observe a browser reusing a cached response — which is why the original suite
// missed this entirely: it asserted `no-store` on the 403 and never looked at
// the 200's headers at all.
//
// Two things ARE observable here, and together they are stronger than the header
// assertion the issue proposed:
//
//  1. the 200 is revalidatable (no `immutable`, and an ETag to revalidate with);
//  2. ⚠ a CONDITIONAL request for a part that has since been flagged is answered
//     403, NOT 304 — which is the actual property. A 304 is permission to reuse
//     the cached bytes, so emitting one for a quarantined part discloses exactly
//     what the 403 refuses.
func TestAPartResponseStaysRevalidatableSoTheGateKeepsWorking(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h, a, []byte(rawWithAttachment))
	messageBlob, byName := partBlobIDs(t, h, a, msgID)

	srv := httptest.NewServer(blobHandler(h, a))
	defer srv.Close()
	pdfURL := downloadURL(a.UserID.String(), byName["invoice.pdf"], "invoice.pdf")

	// ── 1. CONTROL: the part downloads while nothing is flagged. ────────────
	body, resp := getBody(t, srv, pdfURL)
	if resp.StatusCode != http.StatusOK || !bytes.HasPrefix(body, []byte("%PDF-1.4")) {
		t.Fatalf("CONTROL FAILED: the PDF is not downloadable before any verdict (status %d) — "+
			"nothing below can be attributed to caching", resp.StatusCode)
	}

	// ── 2. The 200 must be revalidatable. ───────────────────────────────────
	cc := resp.Header.Get("Cache-Control")
	if strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q on a part response. `immutable` tells the browser NEVER to "+
			"revalidate, so once a part has been fetched clean — which every inline CID image is, "+
			"with zero clicks, on message open — a later flag can never reach it. That is the "+
			"zero-click case ADR-027 §8 exists for, re-opened by a header", cc)
	}
	if !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want it to force revalidation", cc)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("no ETag on a part response — without a validator the browser must re-download the " +
			"whole part on every message open, which is the cost `no-cache` exists to avoid")
	}

	// ── 3. CONTROL: a conditional request for a CLEAN part is 304. ──────────
	// Without this, the 403 below could just mean conditional requests are
	// broken or ignored, rather than that the gate reached them.
	if _, resp304 := getBodyWith(t, srv, pdfURL, map[string]string{"If-None-Match": etag}); resp304.StatusCode != http.StatusNotModified {
		t.Fatalf("CONTROL FAILED: a conditional request for a clean part returned %d, want 304 — "+
			"the revalidation path is not working, so the assertion below proves nothing",
			resp304.StatusCode)
	}

	// ── 4. Flag it, then revalidate with the SAME ETag. ─────────────────────
	seedAttachmentRows(t, h, a.UserID, msgID, []byte(rawWithAttachment), map[string]string{"2": "flagged"})

	cbody, cresp := getBodyWith(t, srv, pdfURL, map[string]string{"If-None-Match": etag})
	if cresp.StatusCode == http.StatusNotModified {
		t.Errorf("a conditional request for a FLAGGED part was answered 304 — that is permission to " +
			"reuse the cached bytes, so it discloses exactly what the 403 refuses. The gate must " +
			"run before the conditional answer")
	}
	if cresp.StatusCode != http.StatusForbidden {
		t.Errorf("a conditional request for a flagged part returned %d, want 403", cresp.StatusCode)
	}
	if bytes.Contains(cbody, []byte("%PDF")) {
		t.Errorf("the conditional refusal carried the part bytes: %q", cbody)
	}

	// ── 5. The RAW MESSAGE download must revalidate too. ────────────────────
	// ⚠ Its BODY is immutable and its HEADERS are not: X-Mailroom-Attachment-Scan
	// is read from the same rows a re-scan rewrites, and the code that sets it
	// says the refusal "must not be SILENT". A cached-immutable response is
	// silent again. The bytes here are deliberately not gated (§11).
	rawURL := downloadURL(a.UserID.String(), messageBlob, "message.eml")
	_, rawResp := getBody(t, srv, rawURL)
	if rawResp.StatusCode != http.StatusOK {
		t.Fatalf("CONTROL FAILED: the raw message is not downloadable (status %d)", rawResp.StatusCode)
	}
	if got := rawResp.Header.Get("X-Mailroom-Attachment-Scan"); got != "flagged" {
		t.Errorf("X-Mailroom-Attachment-Scan = %q on the raw download, want flagged", got)
	}
	if rawCC := rawResp.Header.Get("Cache-Control"); strings.Contains(rawCC, "immutable") {
		t.Errorf("Cache-Control = %q on a raw message carrying a scan-state header — the header is a "+
			"function of mutable state, so caching it immutably freezes the verdict the browser "+
			"happened to see first", rawCC)
	}
}

// ── #413.1: a BARE blobId is the forward-as-.eml path ───────────────────────
//
// The draft gate read `if owned && ref.part != ""`, so the SUFFIXED form was
// checked and the whole-message form was attached unchecked — quarantined part
// and all — which is strictly more bytes on the wire to a third party, not
// fewer.
func TestAMessageCarryingAQuarantinedPartCannotBeAttachedWhole(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	msgID := seedRawMessage(t, h, a, []byte(rawWithAttachment))
	messageBlob, _ := partBlobIDs(t, h, a, msgID)

	create := func(key string) map[string]any {
		return map[string]any{"create": map[string]any{key: map[string]any{
			"from":       []map[string]any{{"email": "me@example.com"}},
			"to":         []map[string]any{{"email": "friend@example.net"}},
			"subject":    "fwd",
			"bodyValues": map[string]any{"1": map[string]any{"value": "see attached"}},
			"textBody":   []map[string]any{{"partId": "1"}},
			// ⚠ THE BARE BLOBID: the whole RFC 5322 message, forwarded as .eml.
			"attachments": []any{map[string]any{"blobId": messageBlob, "type": "message/rfc822"}},
		}}}
	}

	// CONTROL: with no verdict the whole-message attach succeeds, so the
	// refusal below is attributable to the quarantine and not to the bare form
	// being rejected for some unrelated reason.
	res := call(t, h.api.emailSet, a.UserID, create("c1"))
	if created, _ := res["created"].(map[string]any); len(created) != 1 {
		t.Fatalf("CONTROL FAILED: forwarding an unscanned message as .eml did not create a draft "+
			"(%#v) — the refusal below would prove nothing", res)
	}

	seedAttachmentRows(t, h, a.UserID, msgID, []byte(rawWithAttachment), map[string]string{"2": "flagged"})

	res2 := call(t, h.api.emailSet, a.UserID, create("c2"))
	if created, _ := res2["created"].(map[string]any); len(created) != 0 {
		t.Errorf("a draft was created carrying a whole message with a QUARANTINED part inside it — "+
			"§11's forward-malware path, through the door the part-level gate does not cover: %#v",
			created)
	}
	e := setError(t, res2, "notCreated", "c2")
	if e == nil {
		t.Fatalf("Email/set did not refuse the quarantined message: %#v", res2)
	}
	if desc, _ := e["description"].(string); !strings.Contains(desc, "quarantined") {
		t.Errorf("SetError description = %q, want it to name the quarantine", desc)
	}
}

// getBodyWith is getBody with request headers — needed for conditional requests,
// which are the only in-process way to observe that the gate is reachable on a
// revalidation (#410).
func getBodyWith(t *testing.T, srv *httptest.Server, path string, headers map[string]string) ([]byte, *http.Response) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body, resp
}

// rawWithTextAttachment carries a text/plain part dispositioned as an
// ATTACHMENT, which is the exact population #413.2 is about: it IS projected and
// scannable (mime.IsAttachmentLeaf counts a text part as an attachment when the
// disposition says so), so the download route gates it — and fetchAllBodyValues
// was emitting its content anyway.
//
// ⚠ text/plain ON PURPOSE, not text/html. An HTML part goes through
// SanitizeHTML on the way out, so a marker inside a <script> is stripped for an
// unrelated reason and the assertion would pass against a broken gate. Plain
// text is emitted verbatim, so the marker's presence means exactly one thing.
const rawWithTextAttachment = "From: Alice <alice@example.com>\r\n" +
	"To: You <you@example.com>\r\n" +
	"Subject: Notes\r\n" +
	"Date: Mon, 6 Jan 2025 10:00:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"bnd\"\r\n" +
	"\r\n" +
	"--bnd\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"See attached.\r\n" +
	"--bnd\r\n" +
	"Content-Type: text/plain; charset=utf-8; name=\"notes.txt\"\r\n" +
	"Content-Disposition: attachment; filename=\"notes.txt\"\r\n" +
	"\r\n" +
	"QUARANTINED-MARKER-9f3a\r\n" +
	"--bnd--\r\n"

func TestFetchAllBodyValuesDoesNotReturnAQuarantinedTextAttachment(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	msgID := seedRawMessage(t, h, a, []byte(rawWithTextAttachment))

	get := func() map[string]any {
		t.Helper()
		return call(t, h.api.emailGet, a.UserID, map[string]any{
			"ids":                []string{msgID.String()},
			"properties":         []string{"id", "bodyValues"},
			"fetchAllBodyValues": true,
		})
	}
	marker := func(res map[string]any) bool {
		t.Helper()
		return strings.Contains(mustJSONString(t, res), "QUARANTINED-MARKER-9f3a")
	}

	// ── CONTROL: unscanned, the content comes back. ─────────────────────────
	// Without this, "the marker is absent" below is also what a wrong partId, an
	// unseeded message, or a handler that returns no body values looks like.
	if !marker(get()) {
		t.Fatalf("CONTROL FAILED: fetchAllBodyValues did not return the text attachment's content " +
			"before any verdict existed — its absence below would prove nothing")
	}

	// ── Flag it. The same bytes the download route now refuses. ─────────────
	seedAttachmentRows(t, h, a.UserID, msgID, []byte(rawWithTextAttachment), map[string]string{"2": "flagged"})

	if marker(get()) {
		t.Errorf("fetchAllBodyValues returned a QUARANTINED text attachment's content verbatim as a " +
			"JSON string. The download route refuses these exact bytes; this is the same content " +
			"through a door the gate did not cover")
	}

	// ⚠ AND THE MESSAGE IS STILL READABLE. The whole design rests on "the
	// message text stays readable" — a gate that withheld the body too would be
	// a worse failure than the one being fixed.
	res := get()
	if !strings.Contains(mustJSONString(t, res), "See attached.") {
		t.Errorf("the message's own body text disappeared along with the quarantined attachment — "+
			"the gate is withholding the message, not the part: %v", mustJSONString(t, res))
	}
}

// mustJSONString renders a result for substring assertions.
func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
