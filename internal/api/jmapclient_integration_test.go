// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration && jmapclient

package api

// A real third-party JMAP client against the real server (#33, ADR-002).
//
// ADR-002 promises that third-party JMAP clients can connect. That is a claim
// about the wire, and the only test of a claim about the wire is a client that
// was written by somebody else. This drives rockorager/go-jmap, the library the
// aerc mail client is built on, over real HTTP: a real listener, the real bearer
// gate in front, the real store on a real Postgres with RLS, and an API key
// presented as `Authorization: Bearer`, exactly as cmd/mailroom mounts them.
//
// HOW IT FAILS. Every step below is one thing a mail client does. A step that
// fails and is not in testdata/jmapclient-known-gaps.txt fails the job: that is
// the regression gate. A step that fails and IS listed is reported as a known
// non-conformance (skipped, with the reason on the line). A step that PASSES but
// is still listed also fails the job, so the list cannot rot into a record of
// gaps that were fixed long ago. Fixing a gap therefore means deleting its line.
//
// The build tag keeps the go-jmap dependency out of every other lane: the
// shipped binary and the default and integration test builds never compile it.
//
// Test data is synthetic, and every exchange with the server is recorded
// (MAILROOM_JMAPCLIENT_RECORD names the file to write).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	nmail "net/mail"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail"
	jemail "git.sr.ht/~rockorager/go-jmap/mail/email"
	jmailbox "git.sr.ht/~rockorager/go-jmap/mail/mailbox"
	jthread "git.sr.ht/~rockorager/go-jmap/mail/thread"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/mime"
	"mailroom/internal/rcptproj"
	"mailroom/internal/search"
	"mailroom/internal/store"
)

// ---- recording -----------------------------------------------------------------

type jcExchange struct {
	Step     string          `json:"step"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Status   int             `json:"status"`
	Request  json.RawMessage `json:"request,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// jcRecorder is an http.RoundTripper that records what the client sent and what
// the server answered, tagged with the step that was running.
type jcRecorder struct {
	next http.RoundTripper
	log  *jcLog
}

// jcLog is the one ordered log every client and raw call writes to.
type jcLog struct {
	mu      sync.Mutex
	cur     string
	entries []jcExchange
}

func (l *jcLog) setStep(s string) { l.mu.Lock(); l.cur = s; l.mu.Unlock() }

func (r *jcRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	resp, err := r.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	r.log.mu.Lock()
	r.log.entries = append(r.log.entries, jcExchange{Step: r.log.cur, Method: req.Method, URL: req.URL.String(), Status: resp.StatusCode,
		Request: jcJSONOrString(reqBody), Response: jcJSONOrString(respBody)})
	r.log.mu.Unlock()
	return resp, nil
}

func jcJSONOrString(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		return b
	}
	s, _ := json.Marshal(string(b))
	return s
}

// ---- the environment -----------------------------------------------------------

type jcEnv struct {
	t   *testing.T
	h   *agentHarness
	srv *httptest.Server
	rec *jcLog
	f   apiFixture

	roToken, draftToken string
	draft               *jmap.Client

	drafts         ulid.ULID
	m0, m1, m2, m3 ulid.ULID // seed (plain), alternative+attachment, list mail, html only
	t1, t2         ulid.ULID // threads of m2 and m3 (m0 and m1 share f.ThreadID)
	attachmentBlob string
}

func (e *jcEnv) acct() jmap.ID { return jmap.ID(e.f.UserID.String()) }

// newClient builds a go-jmap client the way aerc does: the session endpoint is
// the well-known URL, authentication is a bearer token. The recorder wraps
// whatever transport go-jmap installed.
func (e *jcEnv) newClient(token string) *jmap.Client {
	c := &jmap.Client{SessionEndpoint: e.srv.URL + "/.well-known/jmap"}
	c.WithAccessToken(token)
	c.HttpClient.Transport = e.recorderOver(c.HttpClient.Transport)
	return c
}

// recorderOver wraps a transport so its traffic lands in the one shared log.
func (e *jcEnv) recorderOver(next http.RoundTripper) http.RoundTripper {
	return &jcRecorder{next: next, log: e.rec}
}

// absolutize is the client-side workaround for relative session URLs (a gap the
// "session.urls-are-absolute" step records): go-jmap hands the apiUrl straight to
// net/http, which cannot POST to "/jmap". It lets every OTHER step run.
func (e *jcEnv) absolutize(c *jmap.Client) {
	if err := c.Authenticate(); err != nil {
		e.t.Fatalf("authenticate: %v", err)
	}
	abs := func(u string) string {
		if strings.HasPrefix(u, "/") {
			return e.srv.URL + u
		}
		return u
	}
	c.Lock()
	c.Session.APIURL = abs(c.Session.APIURL)
	c.Session.DownloadURL = abs(c.Session.DownloadURL)
	c.Session.UploadURL = abs(c.Session.UploadURL)
	c.Session.EventSourceURL = abs(c.Session.EventSourceURL)
	c.Unlock()
}

// raw posts a JMAP request body as the given token and returns the decoded
// response envelope. It is for the checks go-jmap's typed structs cannot make
// (a typed struct drops zero values and unknown properties, which is exactly
// what a property-level conformance check must see).
func (e *jcEnv) raw(token string, using []string, calls ...[]any) (int, []jcInvocation, json.RawMessage) {
	e.t.Helper()
	mc := make([]any, 0, len(calls))
	for i, c := range calls {
		mc = append(mc, []any{c[0], c[1], fmt.Sprintf("c%d", i)})
	}
	body, _ := json.Marshal(map[string]any{"using": using, "methodCalls": mc})
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/jmap", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Transport: e.recorderOver(http.DefaultTransport)}
	resp, err := cl.Do(req)
	if err != nil {
		e.t.Fatalf("raw: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var env struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	_ = json.Unmarshal(b, &env)
	var out []jcInvocation
	for _, mr := range env.MethodResponses {
		if len(mr) != 3 {
			continue
		}
		var inv jcInvocation
		_ = json.Unmarshal(mr[0], &inv.Name)
		inv.Args = mr[1]
		_ = json.Unmarshal(mr[2], &inv.CallID)
		out = append(out, inv)
	}
	return resp.StatusCode, out, b
}

type jcInvocation struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

func (i jcInvocation) obj() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(i.Args, &m)
	return m
}

var jcUsingMail = []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"}

// rawOne is raw for a single call that must answer with the method's own name.
func (e *jcEnv) rawOne(token, method string, args any) (map[string]any, error) {
	status, invs, body := e.raw(token, jcUsingMail, []any{method, args})
	if status != http.StatusOK || len(invs) != 1 {
		return nil, fmt.Errorf("%s: HTTP %d, %d responses: %s", method, status, len(invs), jcTrunc(string(body)))
	}
	if invs[0].Name == "error" {
		return nil, fmt.Errorf("%s: method error %s", method, jcTrunc(string(invs[0].Args)))
	}
	if invs[0].Name != method {
		return nil, fmt.Errorf("%s answered as %q: %s", method, invs[0].Name, jcTrunc(string(invs[0].Args)))
	}
	return invs[0].obj(), nil
}

func jcTrunc(s string) string {
	if len(s) > 600 {
		return s[:600] + "..."
	}
	return s
}

// do runs a typed go-jmap request and turns a method-level error into a Go error.
func (e *jcEnv) do(c *jmap.Client, req *jmap.Request) ([]*jmap.Invocation, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	for _, inv := range resp.Responses {
		if me, ok := inv.Args.(*jmap.MethodError); ok {
			return nil, fmt.Errorf("%s: method error: %w", inv.Name, me)
		}
	}
	return resp.Responses, nil
}

// ---- fixtures ------------------------------------------------------------------

const jcPlain = "From: Bob Builder <bob@example.net>\r\n" +
	"To: Test User <user@example.com>\r\n" +
	"Subject: Lunch plans\r\n" +
	"Date: Mon, 02 Nov 2026 10:00:00 +0000\r\n" +
	"Message-ID: <lunch-1@example.net>\r\n" +
	"List-Id: <plans.example.net>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Shall we do noon on Thursday? The usual place works for me.\r\n"

const jcHTML = "From: Carol <carol@example.org>\r\n" +
	"To: user@example.com\r\n" +
	"Cc: dave@example.org\r\n" +
	"Subject: Newsletter\r\n" +
	"Date: Tue, 03 Nov 2026 08:30:00 +0000\r\n" +
	"Message-ID: <news-1@example.org>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<html><body><p>Hello <b>world</b></p></body></html>\r\n"

const jcSeedPlain = "From: Friend <friend@example.com>\r\n" +
	"To: Test User <user@example.com>\r\n" +
	"Subject: seed subject\r\n" +
	"Date: Sun, 01 Nov 2026 09:00:00 +0000\r\n" +
	"Message-ID: <seed-jc@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"A short note.\r\n"

// jcPutBlob stores raw bytes in the real encrypted blob store and registers the
// blob row, returning the content hash.
func jcPutBlob(t *testing.T, h *composeHarness, f apiFixture, raw []byte) []byte {
	t.Helper()
	ctx := context.Background()
	dek, dekID, err := fakeKeys{}.MailDEK(ctx, f.UserID)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := h.blobs.Put(ctx, f.UserID, dek, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := h.blobs.StoredSize(ctx, f.UserID, hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, f.UserID.String(), hash[:], len(raw), stored, dekID.String()); err != nil {
		t.Fatal(err)
	}
	return hash[:]
}

func jcExec(t *testing.T, h *composeHarness, sql string, args ...any) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed: %v\n%s", err, sql)
	}
}

// jcMessage inserts one live Inbox message backed by real bytes.
func jcMessage(t *testing.T, h *composeHarness, f apiFixture, thread ulid.ULID, subject, fromName, fromAddr string,
	received time.Time, hasAtt bool, raw string) ulid.ULID {
	t.Helper()
	hash := jcPutBlob(t, h, f, []byte(raw))
	id := store.NewULID()
	// The columns ingest fills from the headers, so the fixture is a message as
	// the real pipeline would have stored it.
	var sentAt any
	var msgID any
	if m, err := nmail.ReadMessage(strings.NewReader(raw)); err == nil {
		if d, err := m.Header.Date(); err == nil {
			sentAt = d.UTC()
		}
		if v := m.Header.Get("Message-ID"); v != "" {
			msgID = v
		}
	}
	jcExec(t, h, `INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
	        from_name, from_address, received_at, size_bytes, has_attachments, sent_at, message_id_hint)
	      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		id.String(), f.UserID.String(), hash, thread.String(), subject, "preview of "+subject,
		fromName, fromAddr, received, len(raw), hasAtt, sentAt, msgID)
	jcFillRecipients(t, h, id, raw)
	nextSeededUID++
	jcExec(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
	      VALUES ($1,$2,$3,$4,$5,$6,1)`, store.NewULID().String(), f.UserID.String(), id.String(),
		f.AccountID.String(), f.InboxID.String(), nextSeededUID)
	return id
}

// jcFillRecipients stores the recipient and List-Id columns exactly as ingest
// derives them (ADR-055): through the one shared projection, over the bytes.
func jcFillRecipients(t *testing.T, h *composeHarness, id ulid.ULID, raw string) {
	t.Helper()
	m, _ := mime.ParseBytes([]byte(raw))
	c := rcptproj.Project(m)
	jcExec(t, h, `UPDATE app.messages SET rcpt_to=$2, rcpt_cc=$3, rcpt_bcc=$4, rcpt_to_sort=$5, list_id_key=$6 WHERE id=$1`,
		id.String(), c.To, c.Cc, c.Bcc, c.ToSort, c.ListIDKey)
}

// ---- the steps -----------------------------------------------------------------

type jcStep struct {
	id  string // stable, and the key in the known-gaps file
	rfc string // the RFC section the step exercises
	run func(e *jcEnv) error
}

// jcSteps lists the steps in the order they run. The read-only steps come first
// and the ones that change the account (a keyword, a move, a draft) come last, so
// every read-only expectation is about the same fixture.
func jcSteps() []jcStep {
	steps := jcReadSteps()
	for _, c := range jcFilterCases() {
		steps = append(steps, jcStep{"email.query.filter." + c.id, c.rfc, jcFilterStep(c)})
	}
	for _, c := range jcOperatorCases() {
		steps = append(steps, jcStep{"email.query.operator." + c.id, c.rfc, jcFilterStep(c)})
	}
	for _, p := range []string{"size", "from", "to", "subject"} {
		steps = append(steps, jcStep{"email.query.sort." + p, "8621 §4.4.2", jcSortStep(p, "", "")})
	}
	steps = append(steps,
		jcStep{"email.query.sort.hasKeyword", "8621 §4.4.2", jcSortStep("hasKeyword", "$flagged", "")},
		jcStep{"email.query.sort.collation", "8620 §5.5", jcSortStep("receivedAt", "", "i;ascii-casemap")})
	for _, p := range []string{"header:List-Id:asText", "header:Message-ID:asMessageIds", "header:From:asAddresses",
		"header:To:asAddresses:all", "header:Date:asDate", "header:Subject", "header:X-Absent:asText", "headers"} {
		steps = append(steps, jcStep{"email.get.property." + p, "8621 §4.1.2", jcHeaderStep(p)})
	}
	return append(steps, jcMutatingSteps()...)
}

func jcReadSteps() []jcStep {
	return []jcStep{
		// RFC 8620 §2 session discovery
		{"session.wellknown-and-session-agree", "8620 §2.2", stepSessionAgree},
		{"session.requires-auth", "8620 §2", stepSessionAuth},
		{"session.capabilities-and-accounts", "8620 §2", stepSessionShape},
		{"session.urls-are-absolute", "8620 §2", stepSessionAbsolute},
		{"session.readonly-key-account-is-readonly", "8620 §2", stepSessionReadOnly},
		{"session.mail-capability-complete", "8621 §1.3.1", stepMailCapability},

		// RFC 8621 §2 Mailbox
		{"mailbox.get-all", "8621 §2.1", stepMailboxGet},
		{"mailbox.get-properties", "8621 §2", stepMailboxProps},
		{"mailbox.query-by-role", "8621 §2.3", stepMailboxQuery},
		{"mailbox.changes", "8621 §2.2", stepMailboxChanges},

		// RFC 8621 §4 Email
		{"email.query.in-mailbox-received-desc", "8621 §4.4", stepQueryBasic},
		{"email.query.position-limit-total", "8620 §5.5", stepQueryPosition},
		{"email.query.anchor", "8620 §5.5", stepQueryAnchor},
		{"email.query.negative-position", "8620 §5.5", stepQueryNegativePosition},
		{"email.query.collapse-threads", "8621 §4.4", stepQueryCollapse},
		{"email.get.default-properties", "8621 §4.2", stepGetDefaults},
		{"email.get.listed-properties", "8621 §4.1", stepGetListed},
		{"email.get.body-values", "8621 §4.2", stepGetBodyValues},
		{"email.get.not-found", "8620 §5.1", stepGetNotFound},
		{"email.get.malformed-id", "8620 §5.1", stepGetMalformedID},
		{"email.get.message-id-properties", "8621 §4.1.2.3", stepGetMessageID},
		{"email.get.ids-null", "8620 §5.1", stepGetAllIDsNull},
		{"identity.get", "8621 §6.1", stepIdentityGet},
		{"email.get.result-reference", "8620 §3.7", stepGetBackRef},
		{"email.get.max-body-value-bytes", "8621 §4.2", stepGetTruncate},
		{"thread.get", "8621 §3", stepThreadGet},
		{"searchsnippet.get", "8621 §5", stepSnippet},
		{"blob.download-attachment", "8620 §6.2", stepDownload},

		// protocol envelope
		{"error.unknown-method", "8620 §3.6.2", stepUnknownMethod},
		{"error.unknown-capability", "8620 §3.6.1", stepUnknownCapability},
		{"error.not-json", "8620 §3.6.1", stepNotJSON},
		{"error.invalid-arguments", "8620 §3.6.2", stepInvalidArguments},
	}
}

func jcMutatingSteps() []jcStep {
	return []jcStep{
		// state and changes
		{"email.changes.keyword-update", "8620 §5.2", stepEmailChanges},
		{"email.changes.invalid-state", "8620 §5.2", stepEmailChangesBadState},
		{"email.changes.max-changes", "8620 §5.2", stepEmailChangesMax},
		{"email.querychanges", "8620 §5.6", stepQueryChanges},
		{"email.querychanges.session", "8620 §5.6", stepQueryChangesSession},
		{"mailbox.changes.after-move", "8621 §2.2", stepMailboxChangesAfterMove},
		{"thread.state-changes", "8621 §3", stepThreadChanges},

		// writes
		{"email.set.readonly-key-refused", "8620 §3.6.2", stepReadOnlyRefused},
		{"email.set.create-draft", "8621 §4.6", stepDraftCreate},
		{"email.set.create-draft-minimal", "8621 §4.6", stepDraftMinimal},
	}
}

func jcFail(format string, a ...any) error { return fmt.Errorf(format, a...) }

func (e *jcEnv) session() (map[string]any, error) {
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/jmap/session", nil)
	req.Header.Set("Authorization", "Bearer "+e.roToken)
	resp, err := (&http.Client{Transport: e.recorderOver(http.DefaultTransport)}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func stepSessionAgree(e *jcEnv) error {
	a, err := e.session()
	if err != nil {
		return err
	}
	c := e.newClient(e.roToken)
	if err := c.Authenticate(); err != nil {
		return jcFail("client could not fetch the session from /.well-known/jmap: %v", err)
	}
	if c.Session.Username != a["username"] {
		return jcFail("/.well-known/jmap and /jmap/session disagree: %q vs %q", c.Session.Username, a["username"])
	}
	return nil
}

func stepSessionAuth(e *jcEnv) error {
	for _, tok := range []string{"", "mr_nope"} {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/.well-known/jmap", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := (&http.Client{Transport: e.recorderOver(http.DefaultTransport)}).Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			return jcFail("token %q: status %d, want 401", tok, resp.StatusCode)
		}
	}
	return nil
}

func stepSessionShape(e *jcEnv) error {
	s, err := e.session()
	if err != nil {
		return err
	}
	caps, _ := s["capabilities"].(map[string]any)
	for _, urn := range []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:submission"} {
		if _, ok := caps[urn]; !ok {
			return jcFail("capability %s not advertised", urn)
		}
	}
	core, _ := caps["urn:ietf:params:jmap:core"].(map[string]any)
	for _, k := range []string{"maxSizeUpload", "maxConcurrentUpload", "maxSizeRequestObject", "maxConcurrentRequests",
		"maxCallsInRequest", "maxObjectsInGet", "maxObjectsInSet", "collationAlgorithms"} {
		if _, ok := core[k]; !ok {
			return jcFail("core capability lacks %s", k)
		}
	}
	accts, _ := s["accounts"].(map[string]any)
	if len(accts) != 1 {
		return jcFail("want one account, got %d", len(accts))
	}
	prim, _ := s["primaryAccounts"].(map[string]any)
	if prim["urn:ietf:params:jmap:mail"] != e.f.UserID.String() {
		return jcFail("primaryAccounts[mail] = %v", prim["urn:ietf:params:jmap:mail"])
	}
	for _, k := range []string{"username", "apiUrl", "downloadUrl", "uploadUrl", "eventSourceUrl", "state"} {
		if _, ok := s[k]; !ok {
			return jcFail("session lacks %s", k)
		}
	}
	return nil
}

// stepSessionAbsolute is the gap that stops a stock client cold: it checks the
// URLs are usable by a client that does not know the server's public origin, by
// actually asking go-jmap to make a call with the session as served.
func stepSessionAbsolute(e *jcEnv) error {
	c := e.newClient(e.roToken)
	_, err := e.do(c, &jmap.Request{Calls: nil}) // an empty request still POSTs to apiUrl
	if err != nil {
		return jcFail("go-jmap cannot call the apiUrl the session advertises: %v", err)
	}
	return nil
}

func stepSessionReadOnly(e *jcEnv) error {
	s, err := e.session()
	if err != nil {
		return err
	}
	accts, _ := s["accounts"].(map[string]any)
	a, _ := accts[e.f.UserID.String()].(map[string]any)
	if a["isReadOnly"] != true {
		return jcFail("account.isReadOnly = %v for a mail:read-only key (a client would offer compose/delete UI)", a["isReadOnly"])
	}
	return nil
}

func stepMailCapability(e *jcEnv) error {
	s, err := e.session()
	if err != nil {
		return err
	}
	caps, _ := s["capabilities"].(map[string]any)
	m, _ := caps["urn:ietf:params:jmap:mail"].(map[string]any)
	var missing []string
	for _, k := range []string{"maxMailboxesPerEmail", "maxMailboxDepth", "maxSizeMailboxName",
		"maxSizeAttachmentsPerEmail", "emailQuerySortOptions", "mayCreateTopLevelMailbox"} {
		if _, ok := m[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return jcFail("mail capability lacks %v", missing)
	}
	// What the capability promises must be what Email/query accepts.
	opts, _ := m["emailQuerySortOptions"].([]any)
	for _, o := range opts {
		name := fmt.Sprint(o)
		cmp := map[string]any{"property": name, "isAscending": true}
		if name == "hasKeyword" {
			cmp["keyword"] = "$seen" // RFC 8621 4.4.2: the keyword is part of the comparator
		}
		if _, err := e.rawOne(e.roToken, "Email/query", map[string]any{
			"sort": []any{cmp}, "limit": 1}); err != nil {
			return jcFail("advertised sort option %q is refused: %v", name, err)
		}
	}
	return nil
}

func (e *jcEnv) mailboxes(c *jmap.Client) ([]*jmailbox.Mailbox, string, error) {
	req := &jmap.Request{}
	req.Invoke(&jmailbox.Get{Account: e.acct()})
	invs, err := e.do(c, req)
	if err != nil {
		return nil, "", err
	}
	r, ok := invs[0].Args.(*jmailbox.GetResponse)
	if !ok {
		return nil, "", jcFail("unexpected response %T", invs[0].Args)
	}
	return r.List, r.State, nil
}

func stepMailboxGet(e *jcEnv) error {
	c := e.newClient(e.roToken)
	e.absolutize(c)
	list, state, err := e.mailboxes(c)
	if err != nil {
		return err
	}
	if state == "" {
		return jcFail("Mailbox/get returned no state")
	}
	roles := map[jmailbox.Role]*jmailbox.Mailbox{}
	for _, m := range list {
		if m.ID == "" || m.Name == "" {
			return jcFail("mailbox without id or name: %+v", m)
		}
		roles[m.Role] = m
	}
	in := roles[jmailbox.RoleInbox]
	if in == nil || string(in.ID) != e.f.InboxID.String() {
		return jcFail("no inbox-role mailbox with the Inbox id (roles seen: %v)", len(roles))
	}
	if roles[jmailbox.RoleTrash] == nil {
		return jcFail("no trash-role mailbox")
	}
	if in.TotalEmails != 4 {
		return jcFail("inbox totalEmails = %d, want 4", in.TotalEmails)
	}
	if in.Rights == nil || !in.Rights.MayReadItems {
		return jcFail("inbox myRights.mayReadItems missing")
	}
	return nil
}

func stepMailboxProps(e *jcEnv) error {
	want := []string{"id", "name", "parentId", "role", "sortOrder", "totalEmails", "unreadEmails", "totalThreads",
		"unreadThreads", "myRights", "isSubscribed"}
	res, err := e.rawOne(e.roToken, "Mailbox/get", map[string]any{"ids": []string{e.f.InboxID.String()}, "properties": want})
	if err != nil {
		return err
	}
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		return jcFail("want 1 mailbox, got %d", len(list))
	}
	m, _ := list[0].(map[string]any)
	var missing []string
	for _, k := range want {
		if _, ok := m[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return jcFail("Mailbox/get omitted requested properties %v", missing)
	}
	// Default (no properties) must carry the full RFC set too.
	res, err = e.rawOne(e.roToken, "Mailbox/get", map[string]any{"ids": []string{e.f.InboxID.String()}})
	if err != nil {
		return err
	}
	m, _ = res["list"].([]any)[0].(map[string]any)
	for _, k := range want {
		if _, ok := m[k]; !ok {
			return jcFail("Mailbox/get with no properties omitted %q", k)
		}
	}
	return nil
}

func stepMailboxQuery(e *jcEnv) error {
	c := e.newClient(e.roToken)
	e.absolutize(c)
	req := &jmap.Request{}
	req.Invoke(&jmailbox.Query{Account: e.acct(), Filter: &jmailbox.FilterCondition{Role: jmailbox.RoleInbox}})
	invs, err := e.do(c, req)
	if err != nil {
		return err
	}
	r := invs[0].Args.(*jmailbox.QueryResponse)
	if len(r.IDs) != 1 || string(r.IDs[0]) != e.f.InboxID.String() {
		return jcFail("Mailbox/query role=inbox -> %v", r.IDs)
	}
	return nil
}

func stepMailboxChanges(e *jcEnv) error {
	c := e.newClient(e.roToken)
	e.absolutize(c)
	_, state, err := e.mailboxes(c)
	if err != nil {
		return err
	}
	req := &jmap.Request{}
	req.Invoke(&jmailbox.Changes{Account: e.acct(), SinceState: state})
	invs, err := e.do(c, req)
	if err != nil {
		return err
	}
	r := invs[0].Args.(*jmailbox.ChangesResponse)
	if r.OldState != state || r.NewState != state || len(r.Created)+len(r.Updated)+len(r.Destroyed) != 0 {
		return jcFail("changes since the current state should be empty: %+v", r)
	}
	return nil
}

func (e *jcEnv) emailQuery(c *jmap.Client, q *jemail.Query) (*jemail.QueryResponse, error) {
	q.Account = e.acct()
	req := &jmap.Request{}
	req.Invoke(q)
	invs, err := e.do(c, req)
	if err != nil {
		return nil, err
	}
	return invs[0].Args.(*jemail.QueryResponse), nil
}

func (e *jcEnv) roClient() *jmap.Client {
	c := e.newClient(e.roToken)
	e.absolutize(c)
	return c
}

func idStrings(ids []jmap.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

func stepQueryBasic(e *jcEnv) error {
	r, err := e.emailQuery(e.roClient(), &jemail.Query{
		Filter: &jemail.FilterCondition{InMailbox: jmap.ID(e.f.InboxID.String())},
		Sort:   []*jemail.SortComparator{{Property: "receivedAt", IsAscending: false}},
		Limit:  50,
	})
	if err != nil {
		return err
	}
	// newest first: m3 (Nov 3), m2 (Nov 2), m0 (Nov 1) / m1 (older, fixed by constant)
	got := idStrings(r.IDs)
	if len(got) != 4 {
		return jcFail("inbox query returned %d ids, want 4: %v", len(got), got)
	}
	if got[0] != e.m3.String() || got[1] != e.m2.String() {
		return jcFail("receivedAt desc order wrong: %v", got)
	}
	if r.QueryState == "" {
		return jcFail("no queryState")
	}
	return nil
}

func stepQueryCollapse(e *jcEnv) error {
	r, err := e.emailQuery(e.roClient(), &jemail.Query{CollapseThreads: true, CalculateTotal: true,
		Sort: []*jemail.SortComparator{{Property: "receivedAt"}}})
	if err != nil {
		return err
	}
	// m0 and m1 share a thread: four messages, three threads.
	if len(r.IDs) != 3 || r.Total != 3 {
		return jcFail("collapseThreads: %d ids, total %d, want 3/3", len(r.IDs), r.Total)
	}
	return nil
}

// rfcDefaultEmailProps is the property set RFC 8621 §4.2 says Email/get returns
// when `properties` is omitted.
var rfcDefaultEmailProps = []string{"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt",
	"messageId", "inReplyTo", "references", "sender", "from", "to", "cc", "bcc", "replyTo", "subject", "sentAt",
	"hasAttachment", "preview", "bodyValues", "textBody", "htmlBody", "attachments"}

func stepGetDefaults(e *jcEnv) error {
	res, err := e.rawOne(e.roToken, "Email/get", map[string]any{"ids": []string{e.m2.String()}})
	if err != nil {
		return err
	}
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		return jcFail("want 1 email, got %d", len(list))
	}
	m, _ := list[0].(map[string]any)
	var missing []string
	for _, k := range rfcDefaultEmailProps {
		if _, ok := m[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return jcFail("Email/get with no properties omitted default properties %v", missing)
	}
	if res["state"] == nil || res["state"] == "" {
		return jcFail("no state")
	}
	return nil
}

func stepGetListed(e *jcEnv) error {
	// The way a client asks: through go-jmap's typed Get.
	c := e.roClient()
	req := &jmap.Request{}
	req.Invoke(&jemail.Get{Account: e.acct(), IDs: []jmap.ID{jmap.ID(e.m2.String()), jmap.ID(e.m1.String())},
		Properties: []string{"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt", "sentAt",
			"from", "to", "cc", "replyTo", "subject", "messageId", "inReplyTo", "references", "preview", "hasAttachment"}})
	invs, err := e.do(c, req)
	if err != nil {
		return err
	}
	r := invs[0].Args.(*jemail.GetResponse)
	if len(r.List) != 2 {
		return jcFail("got %d emails", len(r.List))
	}
	var m2 *jemail.Email
	for _, m := range r.List {
		if string(m.ID) == e.m2.String() {
			m2 = m
		}
	}
	if m2 == nil {
		return jcFail("m2 missing")
	}
	var bad []string
	chk := func(ok bool, what string) {
		if !ok {
			bad = append(bad, what)
		}
	}
	chk(m2.Subject == "Lunch plans", "subject")
	chk(len(m2.From) == 1 && m2.From[0].Email == "bob@example.net" && m2.From[0].Name == "Bob Builder", "from")
	chk(len(m2.To) == 1 && m2.To[0].Email == "user@example.com", "to")
	chk(m2.ThreadID != "", "threadId")
	chk(m2.BlobID != "", "blobId")
	chk(m2.MailboxIDs[jmap.ID(e.f.InboxID.String())], "mailboxIds")
	chk(m2.Keywords["$flagged"], "keywords $flagged")
	chk(m2.Size > 0, "size")
	chk(m2.ReceivedAt != nil, "receivedAt")
	chk(m2.SentAt != nil && m2.SentAt.UTC().Format("2006-01-02T15:04") == "2026-11-02T10:00", "sentAt (Date header)")
	chk(m2.Preview != "", "preview")
	if len(bad) > 0 {
		return jcFail("Email/get returned wrong or missing: %v", bad)
	}
	return nil
}

func jcKeys(m map[string]any) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func stepGetBodyValues(e *jcEnv) error {
	// What a client asks to display a message: the typed library, both fetch flags.
	c := e.roClient()
	req := &jmap.Request{}
	req.Invoke(&jemail.Get{Account: e.acct(), IDs: []jmap.ID{jmap.ID(e.m1.String())},
		Properties:          []string{"id", "textBody", "htmlBody", "attachments", "bodyValues", "bodyStructure"},
		BodyProperties:      []string{"partId", "blobId", "size", "name", "type", "charset", "disposition", "cid", "subParts"},
		FetchTextBodyValues: true, FetchHTMLBodyValues: true})
	invs, err := e.do(c, req)
	if err != nil {
		return err
	}
	r := invs[0].Args.(*jemail.GetResponse)
	if len(r.List) != 1 {
		return jcFail("got %d emails", len(r.List))
	}
	m := r.List[0]
	if len(m.TextBody) == 0 || len(m.HTMLBody) == 0 {
		return jcFail("textBody/htmlBody empty: %d/%d", len(m.TextBody), len(m.HTMLBody))
	}
	if len(m.BodyValues) == 0 || m.BodyValues[m.TextBody[0].PartID] == nil || m.BodyValues[m.TextBody[0].PartID].Value == "" {
		return jcFail("no bodyValues for textBody part %q: %v", m.TextBody[0].PartID, m.BodyValues)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].BlobID == "" || m.Attachments[0].Name == "" {
		return jcFail("attachments: %+v", m.Attachments)
	}
	e.attachmentBlob = string(m.Attachments[0].BlobID)
	if m.BodyStructure == nil || m.BodyStructure.Type != "multipart/mixed" && m.BodyStructure.Type != "multipart/alternative" {
		return jcFail("bodyStructure: %+v", m.BodyStructure)
	}
	// fetchAllBodyValues is the third flag; go-jmap exposes it.
	req = &jmap.Request{}
	req.Invoke(&jemail.Get{Account: e.acct(), IDs: []jmap.ID{jmap.ID(e.m1.String())},
		Properties: []string{"id", "bodyValues"}, FetchAllBodyValues: true})
	invs, err = e.do(c, req)
	if err != nil {
		return jcFail("fetchAllBodyValues: %v", err)
	}
	if len(invs[0].Args.(*jemail.GetResponse).List[0].BodyValues) == 0 {
		return jcFail("fetchAllBodyValues returned no bodyValues")
	}
	return nil
}

func stepGetNotFound(e *jcEnv) error {
	missing := store.NewULID().String()
	res, err := e.rawOne(e.roToken, "Email/get", map[string]any{"ids": []string{e.m2.String(), missing},
		"properties": []string{"id"}})
	if err != nil {
		return err
	}
	nf, _ := res["notFound"].([]any)
	if len(nf) != 1 || nf[0] != missing {
		return jcFail("notFound = %v, want [%s]", nf, missing)
	}
	return nil
}

// A malformed id cannot name an object: RFC 8620 §5.1 puts ids that "could not
// be found" in notFound, and a server must not silently drop what it was asked for.
func stepGetMalformedID(e *jcEnv) error {
	res, err := e.rawOne(e.roToken, "Email/get", map[string]any{"ids": []string{e.m2.String(), "not-an-id"},
		"properties": []string{"id"}})
	if err != nil {
		return nil // a method error (invalidArguments) is a legitimate answer
	}
	nf, _ := res["notFound"].([]any)
	if len(nf) != 1 || nf[0] != "not-an-id" {
		return jcFail("a malformed id was neither reported in notFound nor refused: notFound = %v, list has %d",
			nf, len(res["list"].([]any)))
	}
	return nil
}

func stepGetMessageID(e *jcEnv) error {
	res, err := e.rawOne(e.roToken, "Email/get", map[string]any{"ids": []string{e.m2.String(), e.m1.String()},
		"properties": []string{"id", "messageId", "inReplyTo", "references"}})
	if err != nil {
		return err
	}
	for _, it := range res["list"].([]any) {
		m := it.(map[string]any)
		if m["id"] == e.m2.String() {
			if v, _ := m["messageId"].([]any); len(v) != 1 || v[0] != "lunch-1@example.net" {
				return jcFail("messageId = %v, want [\"lunch-1@example.net\"] (RFC 8621 §4.1.2.3: angle brackets stripped)", m["messageId"])
			}
		}
		if m["id"] == e.m1.String() {
			// no In-Reply-To / References header on the fixture: the RFC says null
			if m["inReplyTo"] != nil || m["references"] != nil {
				return jcFail("absent In-Reply-To/References must be null: inReplyTo=%v references=%v", m["inReplyTo"], m["references"])
			}
		}
	}
	return nil
}

func stepGetAllIDsNull(e *jcEnv) error {
	// RFC 8620 §5.1: ids = null returns every object (a server may refuse with requestTooLarge).
	res, err := e.rawOne(e.roToken, "Email/get", map[string]any{"ids": nil, "properties": []string{"id"}})
	if err != nil {
		return err
	}
	if l, _ := res["list"].([]any); len(l) < 4 {
		return jcFail("ids=null returned %d emails", len(l))
	}
	return nil
}

func stepIdentityGet(e *jcEnv) error {
	// A client that wants to send needs the identities (RFC 8621 §6.1).
	status, invs, body := e.raw(e.draftToken, []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:submission"},
		[]any{"Identity/get", map[string]any{}})
	if status != http.StatusOK || len(invs) != 1 || invs[0].Name != "Identity/get" {
		return jcFail("Identity/get with a draft key: %s", jcTrunc(string(body)))
	}
	if l, _ := invs[0].obj()["list"].([]any); len(l) == 0 {
		return jcFail("no identities")
	}
	return nil
}

func stepGetBackRef(e *jcEnv) error {
	// The pattern every client uses: query, then get the page via #ids, one round trip.
	status, invs, body := e.raw(e.roToken, jcUsingMail,
		[]any{"Email/query", map[string]any{"filter": map[string]any{"inMailbox": e.f.InboxID.String()},
			"sort": []any{map[string]any{"property": "receivedAt", "isAscending": false}}, "limit": 3}},
		[]any{"Email/get", map[string]any{"#ids": map[string]any{"resultOf": "c0", "name": "Email/query", "path": "/ids"},
			"properties": []string{"id", "subject"}}})
	if status != http.StatusOK || len(invs) != 2 || invs[1].Name != "Email/get" {
		return jcFail("back-reference failed: %d %s", status, jcTrunc(string(body)))
	}
	if l, _ := invs[1].obj()["list"].([]any); len(l) != 3 {
		return jcFail("back-referenced get returned %d, want 3", len(l))
	}
	return nil
}

func stepGetTruncate(e *jcEnv) error {
	res, err := e.rawOne(e.roToken, "Email/get", map[string]any{"ids": []string{e.m2.String()},
		"properties": []string{"bodyValues", "textBody"}, "fetchTextBodyValues": true, "maxBodyValueBytes": 10})
	if err != nil {
		return err
	}
	m, _ := res["list"].([]any)[0].(map[string]any)
	bv, _ := m["bodyValues"].(map[string]any)
	var any1 map[string]any
	for _, v := range bv {
		any1, _ = v.(map[string]any)
	}
	if any1 == nil {
		return jcFail("no bodyValues")
	}
	if v, _ := any1["value"].(string); len(v) > 10 || any1["isTruncated"] != true {
		return jcFail("maxBodyValueBytes=10: value %q isTruncated=%v", v, any1["isTruncated"])
	}
	return nil
}

func stepThreadGet(e *jcEnv) error {
	c := e.roClient()
	req := &jmap.Request{}
	req.Invoke(&jemail.Get{Account: e.acct(), IDs: []jmap.ID{jmap.ID(e.m1.String())}, Properties: []string{"threadId"}})
	invs, err := e.do(c, req)
	if err != nil {
		return err
	}
	tid := invs[0].Args.(*jemail.GetResponse).List[0].ThreadID
	req = &jmap.Request{}
	req.Invoke(&jthread.Get{Account: e.acct(), IDs: []jmap.ID{tid}})
	invs, err = e.do(c, req)
	if err != nil {
		return err
	}
	r := invs[0].Args.(*jthread.GetResponse)
	if len(r.List) != 1 || len(r.List[0].EmailIDs) != 2 {
		return jcFail("thread: %+v", r.List)
	}
	if string(r.List[0].EmailIDs[0]) != e.m1.String() {
		return jcFail("Thread.emailIds must be oldest first (RFC 8621 §3): %v", idStrings(r.List[0].EmailIDs))
	}
	if r.State == "" {
		return jcFail("Thread/get returned no state")
	}
	return nil
}

func stepSnippet(e *jcEnv) error {
	// go-jmap's own searchsnippet.Get names itself "Mailbox/get" (an upstream
	// bug), so a client cannot use the typed one: a raw call is what a client
	// working around that would send.
	res, err := e.rawOne(e.roToken, "SearchSnippet/get", map[string]any{
		"filter": map[string]any{"text": "noon"}, "emailIds": []string{e.m2.String()}})
	if err != nil {
		return err
	}
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		return jcFail("SearchSnippet/get returned %d snippets, want 1", len(list))
	}
	s, _ := list[0].(map[string]any)
	if s["emailId"] != e.m2.String() {
		return jcFail("snippet emailId = %v", s["emailId"])
	}
	for _, k := range []string{"subject", "preview"} {
		if _, ok := s[k]; !ok {
			return jcFail("snippet lacks %s", k)
		}
	}
	return nil
}

func stepDownload(e *jcEnv) error {
	if e.attachmentBlob == "" {
		return jcFail("no attachment blob id (email.get.body-values did not run)")
	}
	c := e.roClient()
	rc, err := c.Download(e.acct(), jmap.ID(e.attachmentBlob))
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	if len(b) == 0 {
		return jcFail("downloaded blob is empty")
	}
	return nil
}

// ---- state, changes -------------------------------------------------------------

func (e *jcEnv) emailGetState(c *jmap.Client) (string, error) {
	req := &jmap.Request{}
	req.Invoke(&jemail.Get{Account: e.acct(), IDs: []jmap.ID{jmap.ID(e.m0.String())}, Properties: []string{"id"}})
	invs, err := e.do(c, req)
	if err != nil {
		return "", err
	}
	return invs[0].Args.(*jemail.GetResponse).State, nil
}

// setKeyword changes a message THROUGH THE API, as the user's own session (the
// web client): a read-only key could not, and that is the point of the key.
func (e *jcEnv) setKeyword(id ulid.ULID, kw string, on bool) {
	e.t.Helper()
	var v any = true
	if !on {
		v = nil
	}
	call(e.t, e.h.api.emailSet, e.f.UserID, map[string]any{"update": map[string]any{
		id.String(): map[string]any{"keywords/" + kw: v}}})
}

func stepEmailChanges(e *jcEnv) error {
	c := e.roClient()
	before, err := e.emailGetState(c)
	if err != nil {
		return err
	}
	e.setKeyword(e.m3, "$seen", true)
	req := &jmap.Request{}
	req.Invoke(&jemail.Changes{Account: e.acct(), SinceState: before})
	invs, err := e.do(c, req)
	if err != nil {
		return err
	}
	r := invs[0].Args.(*jemail.ChangesResponse)
	if r.OldState != before || r.NewState == before || r.NewState == "" {
		return jcFail("states: old %q new %q (before %q)", r.OldState, r.NewState, before)
	}
	if len(r.Updated) != 1 || string(r.Updated[0]) != e.m3.String() || len(r.Created) != 0 || len(r.Destroyed) != 0 {
		return jcFail("changes = %+v, want exactly m3 updated", r)
	}
	// and the new state is what Email/get now reports
	if now, _ := e.emailGetState(c); now != r.NewState {
		return jcFail("Email/get state %q != Email/changes newState %q", now, r.NewState)
	}
	// and the change is visible
	res, err := e.rawOne(e.roToken, "Email/get", map[string]any{"ids": []string{e.m3.String()}, "properties": []string{"keywords"}})
	if err != nil {
		return err
	}
	kw, _ := res["list"].([]any)[0].(map[string]any)["keywords"].(map[string]any)
	if kw["$seen"] != true {
		return jcFail("keywords after update = %v", kw)
	}
	return nil
}

func stepEmailChangesBadState(e *jcEnv) error {
	status, invs, body := e.raw(e.roToken, jcUsingMail, []any{"Email/changes", map[string]any{"sinceState": "no-such-state"}})
	if status != http.StatusOK || len(invs) != 1 || invs[0].Name != "error" {
		return jcFail("bad sinceState: %d %s", status, jcTrunc(string(body)))
	}
	if t := invs[0].obj()["type"]; t != "cannotCalculateChanges" {
		return jcFail("error type %v, want cannotCalculateChanges", t)
	}
	return nil
}

func stepEmailChangesMax(e *jcEnv) error {
	c := e.roClient()
	start, err := e.emailGetState(c)
	if err != nil {
		return err
	}
	e.setKeyword(e.m0, "$flagged", true)
	e.setKeyword(e.m1, "$flagged", true)
	e.setKeyword(e.m1, "$flagged", false)
	e.setKeyword(e.m0, "$flagged", false)
	e.setKeyword(e.m2, "$answered", true)
	res, err := e.rawOne(e.roToken, "Email/changes", map[string]any{"sinceState": start, "maxChanges": 1})
	if err != nil {
		return err
	}
	if res["hasMoreChanges"] != true {
		return jcFail("maxChanges=1 with 3 changed emails: hasMoreChanges = %v", res["hasMoreChanges"])
	}
	n := len(res["created"].([]any)) + len(res["updated"].([]any)) + len(res["destroyed"].([]any))
	if n != 1 {
		return jcFail("maxChanges=1 returned %d ids", n)
	}
	// following newState to the end converges
	cur := res["newState"].(string)
	for i := 0; i < 10 && res["hasMoreChanges"] == true; i++ {
		if res, err = e.rawOne(e.roToken, "Email/changes", map[string]any{"sinceState": cur, "maxChanges": 1}); err != nil {
			return jcFail("continuing past hasMoreChanges: %v", err)
		}
		cur = res["newState"].(string)
	}
	final, _ := e.emailGetState(c)
	if cur != final {
		return jcFail("paging Email/changes ended at %q, Email/get says %q", cur, final)
	}
	return nil
}

// queryChangesCase runs the Email/queryChanges round trip with `call` standing in
// for the transport: through the client (a key) or straight at the handler (the
// user's own session, which is what the web client is).
func (e *jcEnv) queryChangesCase(query func(args map[string]any) (map[string]any, error),
	changes func(args map[string]any) (map[string]any, error)) error {
	filter := map[string]any{"inMailbox": e.f.InboxID.String()}
	sorts := []any{map[string]any{"property": "receivedAt", "isAscending": false}}
	r, err := query(map[string]any{"filter": filter, "sort": sorts})
	if err != nil {
		return err
	}
	if r["canCalculateChanges"] != true {
		return jcFail("canCalculateChanges = %v; a client cannot use Email/queryChanges", r["canCalculateChanges"])
	}
	// A new message arrives in the inbox: queryChanges must report it as added.
	nm := e.m3
	e.setKeyword(nm, "$answered", true) // an update that does not change membership
	qc, err := changes(map[string]any{"filter": filter, "sort": sorts, "sinceQueryState": r["queryState"]})
	if err != nil {
		return err
	}
	if qc["oldQueryState"] != r["queryState"] || qc["newQueryState"] == nil || qc["newQueryState"] == "" {
		return jcFail("queryChanges states: %v", qc)
	}
	return nil
}

// stepQueryChanges is the key's side of the same exchange: Email/queryChanges is
// closed to API keys (ADR-042 §3), so the honest answer from Email/query is
// canCalculateChanges=false (RFC 8620 §5.5), and a client that believes it and
// never calls queryChanges is not refused. Calling it anyway is still refused.
func stepQueryChanges(e *jcEnv) error {
	filter := map[string]any{"inMailbox": e.f.InboxID.String()}
	sorts := []any{map[string]any{"property": "receivedAt", "isAscending": false}}
	r, err := e.rawOne(e.roToken, "Email/query", map[string]any{"filter": filter, "sort": sorts})
	if err != nil {
		return err
	}
	if r["canCalculateChanges"] != false {
		return jcFail("canCalculateChanges = %v for a key, which Email/queryChanges refuses; want false", r["canCalculateChanges"])
	}
	if _, err := e.rawOne(e.roToken, "Email/queryChanges", map[string]any{
		"filter": filter, "sort": sorts, "sinceQueryState": r["queryState"]}); err == nil {
		return jcFail("Email/queryChanges answered a key; ADR-042 §3 closes it to keys")
	}
	return nil
}

// stepQueryChangesSession is the same exchange as the user's own session, so the
// method itself stays covered while the key path is a known limit.
func stepQueryChangesSession(e *jcEnv) error {
	run := func(method string) func(a map[string]any) (map[string]any, error) {
		return func(a map[string]any) (map[string]any, error) {
			raw, mErr := e.h.api.methods[method](context.Background(), e.f.UserID, mustJSON(e.t, a))
			if mErr != nil {
				return nil, jcFail("%s: %+v", method, mErr)
			}
			var out map[string]any
			_ = json.Unmarshal(raw, &out)
			return out, nil
		}
	}
	return e.queryChangesCase(run("Email/query"), run("Email/queryChanges"))
}

func stepMailboxChangesAfterMove(e *jcEnv) error {
	c := e.roClient()
	_, before, err := e.mailboxes(c)
	if err != nil {
		return err
	}
	// Move m0 to Trash through the API.
	call(e.t, e.h.api.emailSet, e.f.UserID, map[string]any{"update": map[string]any{
		e.m0.String(): map[string]any{"mailboxIds": map[string]any{e.f.TrashID.String(): true}}}})
	req := &jmap.Request{}
	req.Invoke(&jmailbox.Changes{Account: e.acct(), SinceState: before})
	invs, err := e.do(c, req)
	if err != nil {
		return err
	}
	r := invs[0].Args.(*jmailbox.ChangesResponse)
	got := map[string]bool{}
	for _, id := range r.Updated {
		got[string(id)] = true
	}
	list, after, err := e.mailboxes(c)
	if err != nil {
		return err
	}
	for _, m := range list {
		if string(m.ID) == e.f.TrashID.String() && m.TotalEmails != 1 {
			return jcFail("trash totalEmails = %d after the move, want 1", m.TotalEmails)
		}
	}
	if !got[e.f.InboxID.String()] || !got[e.f.TrashID.String()] {
		return jcFail("a move changes both mailboxes' counts (Mailbox/get shows them), but Mailbox/changes since %q "+
			"reported updated = %v (newState %q, Mailbox/get state %q)", before, idStrings(r.Updated), r.NewState, after)
	}
	return nil
}

func stepThreadChanges(e *jcEnv) error {
	res, err := e.rawOne(e.roToken, "Thread/get", map[string]any{"ids": []string{e.f.ThreadID.String()}})
	if err != nil {
		return err
	}
	st, _ := res["state"].(string)
	if st == "" {
		return jcFail("Thread/get has no state")
	}
	if _, err := e.rawOne(e.roToken, "Thread/changes", map[string]any{"sinceState": st}); err != nil {
		return err
	}
	return nil
}

// ---- writes ---------------------------------------------------------------------

func stepReadOnlyRefused(e *jcEnv) error {
	status, invs, body := e.raw(e.roToken, jcUsingMail, []any{"Email/set", map[string]any{
		"update": map[string]any{e.m2.String(): map[string]any{"keywords/$seen": true}}}})
	if status != http.StatusOK || len(invs) != 1 {
		return jcFail("HTTP %d %s", status, jcTrunc(string(body)))
	}
	if invs[0].Name == "error" {
		if t := invs[0].obj()["type"]; t != "forbidden" {
			return jcFail("method error type %v, want forbidden", t)
		}
		return nil
	}
	nu, _ := invs[0].obj()["notUpdated"].(map[string]any)
	if nu[e.m2.String()] == nil {
		return jcFail("a mail:read key's Email/set update was not refused: %s", jcTrunc(string(invs[0].Args)))
	}
	return nil
}

func stepDraftCreate(e *jcEnv) error {
	c := e.draft
	if c == nil {
		return jcFail("no draft client")
	}
	// The create a stock client sends: mailboxIds + $draft keyword + the
	// RFC 8621 §4.6 body shape (bodyValues + textBody partId).
	var from = []*mail.Address{{Name: "Test User", Email: "me@example.com"}}
	req := &jmap.Request{}
	req.Invoke(&jemail.Set{Account: e.acct(), Create: map[jmap.ID]*jemail.Email{
		"k1": {
			MailboxIDs: map[jmap.ID]bool{jmap.ID(e.drafts.String()): true},
			Keywords:   map[string]bool{"$draft": true, "$seen": true},
			From:       from,
			To:         []*mail.Address{{Name: "Alice", Email: "alice@example.net"}},
			Subject:    "A draft from a stock client",
			BodyValues: map[string]*jemail.BodyValue{"1": {Value: "Hello from go-jmap."}},
			TextBody:   []*jemail.BodyPart{{PartID: "1", Type: "text/plain"}},
		}}})
	invs, err := e.do(c, req)
	if err != nil {
		return err
	}
	r := invs[0].Args.(*jemail.SetResponse)
	if nc := r.NotCreated["k1"]; nc != nil {
		return jcFail("draft not created: %s %v", nc.Type, ptrStr(nc.Description))
	}
	cr := r.Created["k1"]
	if cr == nil || cr.ID == "" {
		return jcFail("no created entry: %+v", r.Created)
	}
	if r.OldState == "" || r.NewState == "" || r.OldState == r.NewState {
		return jcFail("states old %q new %q", r.OldState, r.NewState)
	}
	// The draft must be readable back as a draft in the Drafts mailbox.
	res, err := e.rawOne(e.draftToken, "Email/get", map[string]any{"ids": []string{string(cr.ID)},
		"properties": []string{"subject", "keywords", "mailboxIds", "to"}})
	if err != nil {
		return err
	}
	l, _ := res["list"].([]any)
	if len(l) != 1 {
		return jcFail("created draft not readable")
	}
	m := l[0].(map[string]any)
	if kw, _ := m["keywords"].(map[string]any); kw["$draft"] != true {
		return jcFail("created draft lacks $draft: %v", m["keywords"])
	}
	if mb, _ := m["mailboxIds"].(map[string]any); mb[e.drafts.String()] != true {
		return jcFail("created draft not in Drafts: %v", m["mailboxIds"])
	}
	return nil
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func stepDraftMinimal(e *jcEnv) error {
	// Omit mailboxIds and keywords: a client that lets the server choose.
	status, invs, body := e.raw(e.draftToken, jcUsingMail, []any{"Email/set", map[string]any{"create": map[string]any{
		"k2": map[string]any{
			"from": []any{map[string]any{"email": "me@example.com"}}, "to": []any{map[string]any{"email": "alice@example.net"}},
			"subject": "Minimal", "bodyValues": map[string]any{"1": map[string]any{"value": "x"}},
			"textBody": []any{map[string]any{"partId": "1"}}}}}})
	if status != http.StatusOK || len(invs) != 1 || invs[0].Name != "Email/set" {
		return jcFail("HTTP %d %s", status, jcTrunc(string(body)))
	}
	if nc, _ := invs[0].obj()["notCreated"].(map[string]any); nc["k2"] != nil {
		return jcFail("notCreated: %v", nc["k2"])
	}
	return nil
}

// ---- the envelope ---------------------------------------------------------------

func stepUnknownMethod(e *jcEnv) error {
	_, invs, body := e.raw(e.roToken, jcUsingMail, []any{"Email/frobnicate", map[string]any{}})
	if len(invs) != 1 || invs[0].Name != "error" || invs[0].obj()["type"] != "unknownMethod" {
		return jcFail("want a method-level unknownMethod error: %s", jcTrunc(string(body)))
	}
	return nil
}

func stepUnknownCapability(e *jcEnv) error {
	status, _, body := e.raw(e.roToken, []string{"urn:ietf:params:jmap:core", "urn:example:nope"},
		[]any{"Core/echo", map[string]any{}})
	var pd struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	_ = json.Unmarshal(body, &pd)
	if status != http.StatusBadRequest || pd.Type != "urn:ietf:params:jmap:error:unknownCapability" || pd.Status != 400 {
		return jcFail("HTTP %d %s", status, jcTrunc(string(body)))
	}
	return nil
}

func stepNotJSON(e *jcEnv) error {
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/jmap", strings.NewReader("this is not json"))
	req.Header.Set("Authorization", "Bearer "+e.roToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: e.recorderOver(http.DefaultTransport)}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var pd struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(b, &pd)
	if resp.StatusCode != http.StatusBadRequest || pd.Type != "urn:ietf:params:jmap:error:notJSON" {
		return jcFail("HTTP %d %s", resp.StatusCode, jcTrunc(string(b)))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		return jcFail("problem details Content-Type = %q, want application/problem+json (RFC 7807, 8620 §3.6.1)", ct)
	}
	return nil
}

func stepInvalidArguments(e *jcEnv) error {
	_, invs, body := e.raw(e.roToken, jcUsingMail, []any{"Email/get", map[string]any{"ids": "not-a-list"}})
	if len(invs) != 1 || invs[0].Name != "error" || invs[0].obj()["type"] != "invalidArguments" {
		return jcFail("want invalidArguments: %s", jcTrunc(string(body)))
	}
	return nil
}

// ---- generated steps: one step per condition, sort and property -----------------
//
// They are separate steps ON PURPOSE. The known-gaps file is keyed by step id, so
// a step that bundled eight filter conditions would let a ninth, newly broken one
// hide behind the eight that are already listed. One id per thing a client can
// ask for means a regression in any of them is a new, unlisted failure.

func stepQueryPosition(e *jcEnv) error {
	r, err := e.emailQuery(e.roClient(), &jemail.Query{
		Filter:   &jemail.FilterCondition{InMailbox: jmap.ID(e.f.InboxID.String())},
		Sort:     []*jemail.SortComparator{{Property: "receivedAt", IsAscending: false}},
		Position: 1, Limit: 2, CalculateTotal: true})
	if err != nil {
		return err
	}
	if len(r.IDs) != 2 || r.Position != 1 || r.Total != 4 {
		return jcFail("position=1 limit=2 total: ids=%d position=%d total=%d, want 2/1/4", len(r.IDs), r.Position, r.Total)
	}
	if string(r.IDs[0]) != e.m2.String() {
		return jcFail("position=1 should start at the second newest (m2): %v", idStrings(r.IDs))
	}
	return nil
}

func stepQueryAnchor(e *jcEnv) error {
	r, err := e.emailQuery(e.roClient(), &jemail.Query{
		Filter: &jemail.FilterCondition{InMailbox: jmap.ID(e.f.InboxID.String())},
		Sort:   []*jemail.SortComparator{{Property: "receivedAt", IsAscending: false}},
		Anchor: jmap.ID(e.m2.String()), Limit: 2})
	if err != nil {
		return err
	}
	if len(r.IDs) == 0 || string(r.IDs[0]) != e.m2.String() || r.Position != 1 {
		return jcFail("anchor=m2 (second newest): position %d, ids %v; want position 1 starting at m2", r.Position, idStrings(r.IDs))
	}
	return nil
}

func stepQueryNegativePosition(e *jcEnv) error {
	r, err := e.emailQuery(e.roClient(), &jemail.Query{
		Filter:   &jemail.FilterCondition{InMailbox: jmap.ID(e.f.InboxID.String())},
		Sort:     []*jemail.SortComparator{{Property: "receivedAt", IsAscending: false}},
		Position: -1, Limit: 5})
	if err != nil {
		return err
	}
	if len(r.IDs) != 1 {
		return jcFail("position=-1 returned %d ids, want the last 1", len(r.IDs))
	}
	return nil
}

type jcFilterCase struct {
	id   string
	rfc  string
	cond func(e *jcEnv) jemail.Filter
	want func(e *jcEnv) []ulid.ULID
}

func jcDay(d int) *time.Time { t := time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC); return &t }

func jcFilterCases() []jcFilterCase {
	fc := func(f jemail.FilterCondition) func(*jcEnv) jemail.Filter {
		return func(*jcEnv) jemail.Filter { return &f }
	}
	all := func(e *jcEnv) []ulid.ULID { return []ulid.ULID{e.m0, e.m1, e.m2, e.m3} }
	one := func(pick func(e *jcEnv) ulid.ULID) func(*jcEnv) []ulid.ULID {
		return func(e *jcEnv) []ulid.ULID { return []ulid.ULID{pick(e)} }
	}
	m2 := one(func(e *jcEnv) ulid.ULID { return e.m2 })
	m3 := one(func(e *jcEnv) ulid.ULID { return e.m3 })
	m1 := one(func(e *jcEnv) ulid.ULID { return e.m1 })
	return []jcFilterCase{
		{"inMailbox", "8621 §4.4.1", func(e *jcEnv) jemail.Filter {
			return &jemail.FilterCondition{InMailbox: jmap.ID(e.f.InboxID.String())}
		}, all},
		{"inMailboxOtherThan", "8621 §4.4.1", func(e *jcEnv) jemail.Filter {
			return &jemail.FilterCondition{InMailboxOtherThan: []jmap.ID{jmap.ID(e.f.TrashID.String())}}
		}, all},
		{"from", "8621 §4.4.1", fc(jemail.FilterCondition{From: "bob@example.net"}), m2},
		{"from-name", "8621 §4.4.1", fc(jemail.FilterCondition{From: "Bob"}), m2},
		// Control for the one above: carol@example.com appears only in m1's To header.
		{"from-ignores-to-header", "8621 §4.4.1", fc(jemail.FilterCondition{From: "carol@example.com"}),
			func(*jcEnv) []ulid.ULID { return nil }},
		{"subject", "8621 §4.4.1", fc(jemail.FilterCondition{Subject: "lunch"}), m2},
		{"to", "8621 §4.4.1", fc(jemail.FilterCondition{To: "user@example.com"}),
			func(e *jcEnv) []ulid.ULID { return []ulid.ULID{e.m0, e.m2, e.m3} }},
		{"cc", "8621 §4.4.1", fc(jemail.FilterCondition{Cc: "dave@example.org"}), m3},
		{"body", "8621 §4.4.1", fc(jemail.FilterCondition{Body: "Thursday"}), m2},
		{"text", "8621 §4.4.1", fc(jemail.FilterCondition{Text: "noon"}), m2},
		{"after", "8621 §4.4.1", fc(jemail.FilterCondition{After: jcDay(3)}), m3},
		{"before", "8621 §4.4.1", fc(jemail.FilterCondition{Before: jcDay(2)}),
			func(e *jcEnv) []ulid.ULID { return []ulid.ULID{e.m1, e.m0} }},
		{"hasKeyword", "8621 §4.4.1", fc(jemail.FilterCondition{HasKeyword: "$flagged"}), m2},
		{"notKeyword", "8621 §4.4.1", fc(jemail.FilterCondition{NotKeyword: "$flagged"}),
			func(e *jcEnv) []ulid.ULID { return []ulid.ULID{e.m0, e.m1, e.m3} }},
		{"hasAttachment", "8621 §4.4.1", fc(jemail.FilterCondition{HasAttachment: true}), m1},
		{"minSize", "8621 §4.4.1", fc(jemail.FilterCondition{MinSize: uint64(len(jcAltSize()))}), m1},
		// maxSize is EXCLUSIVE (RFC 8621 4.4.1: the size must be less than it): the
		// plain fixture's own size is the boundary and does not match itself.
		{"maxSize", "8621 §4.4.1", fc(jemail.FilterCondition{MaxSize: uint64(len(jcPlain))}),
			func(e *jcEnv) []ulid.ULID { return []ulid.ULID{e.m0, e.m3} }},
		{"header", "8621 §4.4.1", fc(jemail.FilterCondition{Header: []string{"List-Id", "plans.example.net"}}), m2},
		{"someInThreadHaveKeyword", "8621 §4.4.1", fc(jemail.FilterCondition{SomeInThreadHaveKeyword: "$flagged"}), m2},
		{"noneInThreadHaveKeyword", "8621 §4.4.1", fc(jemail.FilterCondition{NoneInThreadHaveKeyword: "$flagged"}),
			func(e *jcEnv) []ulid.ULID { return []ulid.ULID{e.m0, e.m1, e.m3} }},
		{"allInThreadHaveKeyword", "8621 §4.4.1", fc(jemail.FilterCondition{AllInThreadHaveKeyword: "$flagged"}), m2},
	}
}

func jcOperatorCases() []jcFilterCase {
	op := func(o jmap.Operator, cs ...jemail.Filter) *jemail.FilterOperator {
		return &jemail.FilterOperator{Operator: o, Conditions: cs}
	}
	cond := func(f jemail.FilterCondition) jemail.Filter { return &f }
	return []jcFilterCase{
		{"AND", "8620 §5.5", func(*jcEnv) jemail.Filter {
			return op(jmap.OperatorAND, cond(jemail.FilterCondition{From: "example.net"}), cond(jemail.FilterCondition{Subject: "lunch"}))
		}, func(e *jcEnv) []ulid.ULID { return []ulid.ULID{e.m2} }},
		{"OR", "8620 §5.5", func(*jcEnv) jemail.Filter {
			return op(jmap.OperatorOR, cond(jemail.FilterCondition{From: "bob@example.net"}), cond(jemail.FilterCondition{From: "carol@example.org"}))
		}, func(e *jcEnv) []ulid.ULID { return []ulid.ULID{e.m2, e.m3} }},
		{"NOT", "8620 §5.5", func(*jcEnv) jemail.Filter {
			return op(jmap.OperatorNOT, cond(jemail.FilterCondition{HasKeyword: "$flagged"}))
		}, func(e *jcEnv) []ulid.ULID { return []ulid.ULID{e.m0, e.m1, e.m3} }},
		{"nested", "8620 §5.5", func(e *jcEnv) jemail.Filter {
			return op(jmap.OperatorAND, cond(jemail.FilterCondition{InMailbox: jmap.ID(e.f.InboxID.String())}),
				op(jmap.OperatorNOT, cond(jemail.FilterCondition{HasKeyword: "$flagged"})))
		}, func(e *jcEnv) []ulid.ULID { return []ulid.ULID{e.m0, e.m1, e.m3} }},
	}
}

func jcFilterStep(c jcFilterCase) func(e *jcEnv) error {
	return func(e *jcEnv) error {
		r, err := e.emailQuery(e.roClient(), &jemail.Query{Filter: c.cond(e)})
		if err != nil {
			return err
		}
		got := idStrings(r.IDs)
		var want []string
		for _, id := range c.want(e) {
			want = append(want, id.String())
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return jcFail("got %d ids, want %d (%v vs %v)", len(got), len(want), got, want)
		}
		return nil
	}
}

func jcSortStep(prop, keyword, collation string) func(e *jcEnv) error {
	return func(e *jcEnv) error {
		c := e.roClient()
		var asc, desc []string
		for _, ascending := range []bool{true, false} {
			r, err := e.emailQuery(c, &jemail.Query{Sort: []*jemail.SortComparator{
				{Property: prop, Keyword: keyword, Collation: jmap.CollationAlgo(collation), IsAscending: ascending}}})
			if err != nil {
				return jcFail("isAscending=%v: %w", ascending, err)
			}
			if len(r.IDs) != 4 {
				return jcFail("isAscending=%v returned %d ids, want 4", ascending, len(r.IDs))
			}
			if ascending {
				asc = idStrings(r.IDs)
			} else {
				desc = idStrings(r.IDs)
			}
		}
		// The order each property implies for the fixture (ties make "to" and the
		// keyword sort order-free: they are checked for being accepted).
		name := func(ids []string) string {
			n := map[string]string{e.m0.String(): "m0", e.m1.String(): "m1", e.m2.String(): "m2", e.m3.String(): "m3"}
			var out []string
			for _, id := range ids {
				out = append(out, n[id])
			}
			return strings.Join(out, ",")
		}
		switch prop {
		case "subject": // Lunch plans, Newsletter, Quarterly report, seed subject
			if name(asc) != "m2,m3,m1,m0" {
				return jcFail("subject ascending = %s, want m2,m3,m1,m0", name(asc))
			}
		case "from": // Alice, Bob Builder, Carol, Friend
			if name(asc) != "m1,m2,m3,m0" {
				return jcFail("from ascending = %s, want m1,m2,m3,m0", name(asc))
			}
		case "size": // the multipart fixture is the largest
			if asc[3] != e.m1.String() || desc[0] != e.m1.String() {
				return jcFail("size: largest message not last ascending / first descending: %s | %s", name(asc), name(desc))
			}
		}
		return nil
	}
}

func jcHeaderStep(prop string) func(e *jcEnv) error {
	return func(e *jcEnv) error {
		res, err := e.rawOne(e.roToken, "Email/get", map[string]any{"ids": []string{e.m2.String()}, "properties": []string{prop}})
		if err != nil {
			return err
		}
		list, _ := res["list"].([]any)
		if len(list) != 1 {
			return jcFail("no email")
		}
		m, _ := list[0].(map[string]any)
		if _, ok := m[prop]; !ok {
			return jcFail("requested property %q silently dropped (got keys %v); an unknown property must be invalidArguments, "+
				"a supported one must be returned", prop, jcKeys(m))
		}
		return nil
	}
}

// jcAltSize is the size of the multipart fixture, so size filters have a value
// that separates it from the small ones.
func jcAltSize() string { return alternativeWithAttachment }

// ---- the test ------------------------------------------------------------------

// knownGaps reads testdata/jmapclient-known-gaps.txt: `step-id<TAB>gap<TAB>why`.
func knownGaps(t *testing.T, steps []jcStep) map[string]string {
	t.Helper()
	b, err := os.ReadFile("testdata/jmapclient-known-gaps.txt")
	if err != nil {
		t.Fatalf("known-gaps file: %v", err)
	}
	valid := map[string]bool{}
	for _, s := range steps {
		valid[s.id] = true
	}
	out := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[1]) == "" {
			t.Fatalf("known-gaps line %d: want `step-id<TAB>gap-ref<TAB>why`: %q", i+1, line)
		}
		if !valid[parts[0]] {
			t.Fatalf("known-gaps line %d names %q, which is not a step", i+1, parts[0])
		}
		out[parts[0]] = parts[1] + ": " + parts[2]
	}
	return out
}

func newJCEnv(t *testing.T) *jcEnv {
	t.Helper()
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	e := &jcEnv{t: t, h: h, f: a, rec: &jcLog{}}

	// Synthetic mail with real bytes behind it. m0/m1 share the seed thread.
	e.t1, e.t2 = store.NewULID(), store.NewULID()
	for _, th := range []ulid.ULID{e.t1, e.t2} {
		jcExec(t, h.composeHarness, `INSERT INTO app.threads (id, user_id, subject, message_count, last_received_at)
		      VALUES ($1,$2,'t',1, now())`, th.String(), a.UserID.String())
	}
	// m0 is the seeded message: give it real bytes and a fixed date.
	hash := jcPutBlob(t, h.composeHarness, a, []byte(jcSeedPlain))
	jcExec(t, h.composeHarness, `UPDATE app.messages SET content_hash=$1, size_bytes=$2, received_at=$3,
	        from_name='Friend', from_address='friend@example.com', sent_at=$3, message_id_hint='<seed-jc@example.com>'
	        WHERE id=$4`,
		hash, len(jcSeedPlain), time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), a.MessageID.String())
	jcFillRecipients(t, h.composeHarness, a.MessageID, jcSeedPlain)
	e.m0 = a.MessageID
	e.m1 = jcMessage(t, h.composeHarness, a, a.ThreadID, "Quarterly report", "Alice", "alice@example.com",
		time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC), true, alternativeWithAttachment)
	e.m2 = jcMessage(t, h.composeHarness, a, e.t1, "Lunch plans", "Bob Builder", "bob@example.net",
		time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC), false, jcPlain)
	e.m3 = jcMessage(t, h.composeHarness, a, e.t2, "Newsletter", "Carol", "carol@example.org",
		time.Date(2026, 11, 3, 8, 30, 0, 0, time.UTC), false, jcHTML)

	// A Drafts mailbox and an identity, so a draft can be created.
	e.drafts, _ = seedSendable(t, h.super, a, "me@example.com", "")

	// Keys: a read-only key, and one that can also draft.
	//
	// The read-only key is the two read scopes: mail:read, and search:read because
	// Email/query conditions on from/to/subject/body are text conditions and need it
	// (a deliberate key limit, ADR-042 §3, not a gap). The rate is the maximum, so
	// the program's few hundred calls do not trip the per-key meter.
	e.roToken, _ = h.newKey(t, a.UserID, map[string]any{"name": "jmap client (read only)",
		"scopes": []string{"mail:read", "search:read"}, "ratePerMinute": 600})
	e.draftToken, _ = h.newKey(t, a.UserID, map[string]any{"name": "jmap client (draft)",
		"scopes": []string{"mail:read", "search:read", "mail:draft"}, "ratePerMinute": 600})

	// Initial keyword state, made through the API as the user.
	e.setKeyword(e.m2, "$flagged", true)

	// A search backend, as cmd/mailroom always installs one: Email/query text
	// conditions (from, to, subject, body, text) and SearchSnippet/get are served
	// by it and by nothing else. In-memory here; indexed from the seeded rows.
	content := search.NewBlobContent(h.blobs, func(ctx context.Context, u ulid.ULID) ([]byte, error) {
		dek, _, err := fakeKeys{}.MailDEK(ctx, u)
		return dek, err
	})
	ix, err := search.NewIndexer(t.TempDir(), search.MemoryFactory, search.NewStoreSource(h.store), content)
	if err != nil {
		t.Fatalf("search indexer: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	h.api.SetSearch(search.NewService(ix, search.NewStoreResolver(h.store)))
	if _, err := ix.Reindex(context.Background(), a.UserID); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	// The harness's gate answers 599 when there is no bearer token at all (its
	// session-path stub); cmd/mailroom answers 401 on JMAP paths. Mount the same
	// gate with a 401 so the unauthenticated case is observable.
	gate := &auth.BearerGate{Resolve: h.store.ResolveAPIKeyToken, Limiter: auth.NewFailureLimiter(1000, time.Millisecond)}
	// The service's per-user request limiter is not what this checks, and a fast
	// runner sends the whole program inside one bucket's burst: CI answered 429
	// from the eighth-to-last step on, while a slower host never did. A
	// non-positive config disables it (RateLimitConfig's documented opt-out).
	// The key's own per-minute rate (ADR-042 §5) stays in force.
	h.api.limiter = NewRateLimiter(RateLimitConfig{})
	e.srv = httptest.NewServer(gate.Wrap(h.api.Handler(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})))
	t.Cleanup(e.srv.Close)
	e.draft = e.newClient(e.draftToken)
	e.absolutize(e.draft)
	return e
}

// TestJMAPClientConformance runs the whole client conformance program.
func TestJMAPClientConformance(t *testing.T) {
	steps := jcSteps()
	gaps := knownGaps(t, steps)
	e := newJCEnv(t)

	type outcome struct{ ID, RFC, Status, Detail string }
	var outcomes []outcome
	for _, s := range steps {
		s := s
		t.Run(s.id, func(t *testing.T) {
			e.t = t
			e.rec.setStep(s.id)
			err := s.run(e)
			gap, listed := gaps[s.id]
			switch {
			case err == nil && !listed:
				outcomes = append(outcomes, outcome{s.id, s.rfc, "pass", ""})
			case err == nil && listed:
				outcomes = append(outcomes, outcome{s.id, s.rfc, "STALE", gap})
				t.Errorf("step now PASSES but is still listed as a known gap (%s): delete its line from testdata/jmapclient-known-gaps.txt", gap)
			case err != nil && rateLimitedErr(err):
				// A rate-limited step tested nothing. It must never pass as a
				// known gap, or a throttled run would read as conformance.
				outcomes = append(outcomes, outcome{s.id, s.rfc, "FAIL", err.Error()})
				t.Errorf("the harness was rate limited, so this step tested nothing: %v", err)
			case err != nil && listed:
				outcomes = append(outcomes, outcome{s.id, s.rfc, "known-gap", err.Error()})
				t.Skipf("known gap %s\n%v", gap, err)
			default:
				outcomes = append(outcomes, outcome{s.id, s.rfc, "FAIL", err.Error()})
				t.Errorf("%v", err)
			}
		})
	}
	e.t = t

	var pass, gap, bad int
	for _, o := range outcomes {
		switch o.Status {
		case "pass":
			pass++
		case "known-gap":
			gap++
		default:
			bad++
		}
	}
	t.Logf("go-jmap conformance: %d steps, %d pass, %d known gaps, %d unexpected", len(outcomes), pass, gap, bad)

	if path := os.Getenv("MAILROOM_JMAPCLIENT_RECORD"); path != "" {
		rep := map[string]any{"outcomes": outcomes, "exchanges": e.rec.entries}
		b, _ := json.MarshalIndent(rep, "", " ")
		if path == "-" { // the integration script runs in a container: print it
			fmt.Printf("JMAPCLIENT-RECORD-BEGIN\n%s\nJMAPCLIENT-RECORD-END\n", b)
		} else if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Errorf("write record: %v", err)
		}
	}
}

// rateLimitedErr reports whether a step failed because the server throttled the
// harness rather than because of the behaviour the step checks.
func rateLimitedErr(err error) bool {
	m := err.Error()
	return strings.Contains(m, "HTTP 429") || strings.Contains(m, "rateLimited") || strings.Contains(m, " 429 ")
}
