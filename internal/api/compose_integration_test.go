// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration tests for the compose/send JMAP surface (mailroom#22, spec §5)
// against a real Postgres with RLS enforced.
//
// The load-bearing one is TestEmailSetCreateRefusesAnUnverifiedFrom: it drives
// the SECURITY BOUNDARY through the actual client-facing method, because that is
// where a client would attack it. A server-side check that exists in the store
// but is bypassable from /jmap protects nothing.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/blob"
	"mailroom/internal/compose"
	"mailroom/internal/store"
)

// composeHarness is an apiHarness whose API has a real Composer over a temp-dir
// blob store.
type composeHarness struct {
	*apiHarness
	blobs blob.BlobStore
}

// fakeKeys is a compose.Keys with a fixed key: the crypto hierarchy is tested in
// internal/crypto, and the send path only needs A key, not THE key.
type fakeKeys struct{}

func (fakeKeys) MailDEK(context.Context, ulid.ULID) ([]byte, ulid.ULID, error) {
	dek := make([]byte, 32)
	for i := range dek {
		dek[i] = byte(i)
	}
	return dek, store.NewULID(), nil
}

func (fakeKeys) SMTPAuth(_ context.Context, _ ulid.ULID, acct store.Account) (compose.SMTPLogin, error) {
	return compose.PasswordLogin(acct, "user", "pass"), nil
}

func newComposeHarness(t *testing.T) *composeHarness {
	t.Helper()
	h := newAPIHarness(t)

	pathKey := make([]byte, 32)
	blobs, err := blob.NewFSBlobStore(filepath.Join(t.TempDir(), "blobs"), pathKey)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	// Rebuild the API with a composer wired in (a zero undo window keeps the
	// tests from having to wait for one).
	h.api = New(h.store, WithComposer(compose.NewComposer(blobs, fakeKeys{}, time.Millisecond)))
	return &composeHarness{apiHarness: h, blobs: blobs}
}

// seedSendable gives a fixture user the two things a send needs: a Drafts/Sent
// mailbox pair and a verified identity.
func seedSendable(t *testing.T, super *pgxpool.Pool, f apiFixture, address, wildcardDomain string) (draftsID, sentID ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	draftsID, sentID = store.NewULID(), store.NewULID()
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	      VALUES ($1,$2,$3,'Drafts','Drafts','\Drafts')`,
		draftsID.String(), f.UserID.String(), f.AccountID.String())
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	      VALUES ($1,$2,$3,'Sent','Sent','\Sent')`,
		sentID.String(), f.UserID.String(), f.AccountID.String())

	if address != "" {
		exec(`INSERT INTO app.identities (id, user_id, account_id, kind, address, enabled)
		      VALUES ($1,$2,$3,'fixed',$4,true)`,
			store.NewULID().String(), f.UserID.String(), f.AccountID.String(), address)
	}
	if wildcardDomain != "" {
		exec(`INSERT INTO app.identities (id, user_id, account_id, kind, domain, enabled)
		      VALUES ($1,$2,$3,'domain_wildcard',$4,true)`,
			store.NewULID().String(), f.UserID.String(), f.AccountID.String(), wildcardDomain)
	}
	return draftsID, sentID
}

// setError extracts a per-object /set error from a notCreated/notUpdated map.
func setError(t *testing.T, res map[string]any, bucket, key string) map[string]any {
	t.Helper()
	m, ok := res[bucket].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %#v", bucket, res[bucket])
	}
	e, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("no %s entry for %q in %#v", bucket, key, m)
	}
	return e
}

// THE SECURITY BOUNDARY, through the client-facing method (spec §5): "the server
// refuses any From address not matching a verified identity/domain for the
// authenticated user". A client that asks to compose as someone else's address
// gets `forbiddenFrom` and NOTHING is written.
func TestEmailSetCreateRefusesAnUnverifiedFrom(t *testing.T) {
	h := newComposeHarness(t)
	ctx := context.Background()
	a, b := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	// B has their own verified domain. A must not be able to borrow it.
	seedSendable(t, h.super, b, "", "b-domain.example.com")

	create := func(from string) map[string]any {
		return map[string]any{
			"create": map[string]any{
				"c1": map[string]any{
					"from":    []map[string]any{{"email": from}},
					"to":      []map[string]any{{"email": "friend@example.net"}},
					"subject": "hello",
					"bodyValues": map[string]any{
						"1": map[string]any{"value": "hi"},
					},
					"textBody": []map[string]any{{"partId": "1"}},
				},
			},
		}
	}

	for _, from := range []string{
		"ceo@some-bank.example",         // never verified
		"someone-else@example.com",      // right domain, but A only verified ONE address
		"anything@b-domain.example.com", // ANOTHER USER's verified domain
	} {
		res := call(t, h.api.emailSet, a.UserID, create(from))
		e := setError(t, res, "notCreated", "c1")
		if e["type"] != setForbiddenFrom {
			t.Fatalf("From %q: SetError type = %v, want %q — the server MUST refuse to compose as an unverified address",
				from, e["type"], setForbiddenFrom)
		}
		if created, _ := res["created"].(map[string]any); len(created) != 0 {
			t.Fatalf("From %q: something was created anyway: %#v", from, created)
		}
	}

	// Nothing was written for A at all.
	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.messages WHERE user_id = $1 AND is_draft`,
		a.UserID.String()).Scan(&n); err != nil {
		t.Fatalf("count drafts: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d draft(s) were written despite every From being refused", n)
	}

	// The verified address, by contrast, composes fine.
	res := call(t, h.api.emailSet, a.UserID, create("me@example.com"))
	created, _ := res["created"].(map[string]any)
	if len(created) != 1 {
		t.Fatalf("a verified From must compose: %#v", res)
	}
}

// A wildcard (catchall) identity authorizes any address at its domain — that is
// the alias story (spec §5), and it is what lets a reply come from the exact
// per-site address the sender used, with no per-alias setup.
func TestEmailSetCreateHonorsTheCatchallWildcard(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	drafts, _ := seedSendable(t, h.super, a, "", "catchall.example.com")

	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"create": map[string]any{
			"c1": map[string]any{
				// An address that exists nowhere in the database — the whole point
				// of a catchall is that there is no finite alias list.
				"from":       []map[string]any{{"email": "some-random-site@catchall.example.com"}},
				"to":         []map[string]any{{"email": "friend@example.net"}},
				"subject":    "re: your receipt",
				"inReplyTo":  []string{"<parent@example.net>"},
				"references": []string{"<root@example.net>"},
				"bodyValues": map[string]any{"1": map[string]any{"value": "thanks"}},
				"textBody":   []map[string]any{{"partId": "1"}},
			},
		},
	})
	created, _ := res["created"].(map[string]any)
	obj, ok := created["c1"].(map[string]any)
	if !ok {
		t.Fatalf("the derived alias was refused: %#v", res)
	}

	// It is a $draft, in the Drafts mailbox, right now — not after the next sync.
	kw, _ := obj["keywords"].(map[string]any)
	if kw["$draft"] != true {
		t.Errorf("keywords = %#v, want $draft", kw)
	}
	// And it is $seen (#610): a draft the user is actively writing cannot be
	// "unread". Every autosave mints a fresh revision, so without this each
	// one is born unread and inflates the unread count.
	if kw["$seen"] != true {
		t.Errorf("keywords = %#v, want $seen — a draft the user is writing is never unread (#610)", kw)
	}
	mb, _ := obj["mailboxIds"].(map[string]any)
	if mb[drafts.String()] != true {
		t.Errorf("mailboxIds = %#v, want the Drafts mailbox", mb)
	}
	// The Message-ID is under the SENDING IDENTITY's domain (spec §5).
	msgID, _ := obj["messageId"].([]any)
	if len(msgID) != 1 {
		t.Fatalf("messageId = %#v", obj["messageId"])
	}
	if got, _ := msgID[0].(string); len(got) < len("@catchall.example.com") ||
		got[len(got)-len("@catchall.example.com"):] != "@catchall.example.com" {
		t.Errorf("Message-ID = %q, want it under the sending identity's domain", got)
	}
	// Threading headers survived.
	if ir, _ := obj["inReplyTo"].([]any); len(ir) != 1 || ir[0] != "parent@example.net" {
		t.Errorf("inReplyTo = %#v", obj["inReplyTo"])
	}
	refs, _ := obj["references"].([]any)
	if len(refs) != 2 || refs[0] != "root@example.net" || refs[1] != "parent@example.net" {
		t.Errorf("references = %#v, want the parent's chain plus the parent", refs)
	}

	// The queued IMAP APPEND must carry BOTH flags (#610): \Draft (unchanged)
	// and \Seen, so a provider that files the copy verbatim — and an ingest
	// that later merges its flags back — agrees with the local row instead of
	// flipping it back to unread.
	ctx := context.Background()
	msgIDStr, _ := obj["id"].(string)
	if msgIDStr == "" {
		t.Fatalf("created object has no id: %#v", obj)
	}
	var payload string
	if err := h.super.QueryRow(ctx,
		`SELECT payload::text FROM app.writeback_intents
		  WHERE user_id=$1 AND message_id=$2 AND op='append'`,
		a.UserID.String(), msgIDStr).Scan(&payload); err != nil {
		t.Fatalf("no append intent was enqueued: %v", err)
	}
	for _, want := range []string{`\Draft`, `\Seen`} {
		if !strings.Contains(payload, want) {
			t.Errorf("append intent payload = %s, want it to carry %s", payload, want)
		}
	}
}

// EmailSubmission/set create is a SEND: it validates the From out of the message
// BYTES, so a client cannot get around the check by naming a different identity.
func TestEmailSubmissionSetValidatesFromAgainstTheMessage(t *testing.T) {
	h := newComposeHarness(t)
	ctx := context.Background()
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")

	// Compose a legitimate draft.
	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"create": map[string]any{
			"c1": map[string]any{
				"from":       []map[string]any{{"email": "me@example.com"}},
				"to":         []map[string]any{{"email": "friend@example.net"}},
				"subject":    "hi",
				"bodyValues": map[string]any{"1": map[string]any{"value": "hi"}},
				"textBody":   []map[string]any{{"partId": "1"}},
			},
		},
	})
	created, _ := res["created"].(map[string]any)
	obj, ok := created["c1"].(map[string]any)
	if !ok {
		t.Fatalf("draft not created: %#v", res)
	}
	emailID, _ := obj["id"].(string)

	// A submission naming an identity that does NOT authorize the message's From
	// is refused: the identity is re-derived from the bytes, never trusted from
	// the request.
	var otherIdentity string
	if err := h.super.QueryRow(ctx,
		`INSERT INTO app.identities (id, user_id, account_id, kind, address, enabled)
		 VALUES ($1,$2,$3,'fixed','other@example.com',true) RETURNING id`,
		store.NewULID().String(), a.UserID.String(), a.AccountID.String()).Scan(&otherIdentity); err != nil {
		t.Fatalf("seed second identity: %v", err)
	}
	bad := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{
			"s1": map[string]any{"emailId": emailID, "identityId": otherIdentity},
		},
	})
	e := setError(t, bad, "notCreated", "s1")
	if e["type"] != setForbiddenFrom {
		t.Fatalf("SetError type = %v, want forbiddenFrom: a client must not be able to send a message under an identity that does not authorize its From", e["type"])
	}

	// The honest submission is scheduled, and it is cancelable (Undo Send).
	good := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{
			"s1": map[string]any{"emailId": emailID},
		},
		"onSuccessUpdateEmail": map[string]any{
			"#s1": map[string]any{"keywords/$draft": nil},
		},
	})
	sub, _ := good["created"].(map[string]any)
	sobj, ok := sub["s1"].(map[string]any)
	if !ok {
		t.Fatalf("submission not created: %#v", good)
	}
	if sobj["undoStatus"] != "pending" {
		t.Errorf("undoStatus = %v, want pending (the send is held, and cancelable)", sobj["undoStatus"])
	}
	subID, _ := sobj["id"].(string)

	// onSuccessUpdateEmail cleared $draft in the same transaction as the send was
	// scheduled — the client did not have to make a second, racy call.
	var isDraft bool
	if err := h.super.QueryRow(ctx,
		`SELECT is_draft FROM app.messages WHERE user_id = $1 AND id = $2`,
		a.UserID.String(), emailID).Scan(&isDraft); err != nil {
		t.Fatalf("read draft: %v", err)
	}
	if isDraft {
		t.Error("onSuccessUpdateEmail did not clear $draft")
	}

	// A durable job exists to actually send it — a send must survive a restart.
	var jobs int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.jobs WHERE user_id = $1 AND kind = 'submit' AND state = 'pending'`,
		a.UserID.String()).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("pending submit jobs = %d, want 1 — a scheduled send that no job will fire is lost mail", jobs)
	}

	// Undo Send: cancel inside the window.
	undo := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"update": map[string]any{subID: map[string]any{"undoStatus": "canceled"}},
	})
	if upd, _ := undo["updated"].(map[string]any); len(upd) != 1 {
		t.Fatalf("undo failed: %#v", undo)
	}
	var state string
	if err := h.super.QueryRow(ctx,
		`SELECT state FROM app.submissions WHERE id = $1`, subID).Scan(&state); err != nil {
		t.Fatalf("read submission: %v", err)
	}
	if state != store.SubmissionCanceled {
		t.Errorf("submission state = %q, want canceled", state)
	}
}

// Identity/get exposes the wildcard in RFC 8621's own form ("*@domain"), so a
// stock JMAP client understands the catchall model with no Mailroom extension.
// Identity/set create may NOT invent a send-as right.
func TestIdentitySurface(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "", "catchall.example.com")

	got := call(t, h.api.identityGet, a.UserID, map[string]any{"ids": nil})
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Identity/get returned %d identities, want 1", len(list))
	}
	id0, _ := list[0].(map[string]any)
	if id0["email"] != "*@catchall.example.com" {
		t.Errorf("identity email = %v, want the RFC 8621 wildcard form *@catchall.example.com", id0["email"])
	}
	if id0["isWildcard"] != true {
		t.Errorf("isWildcard = %v, want true", id0["isWildcard"])
	}

	// Creating an identity for an address the catchall already covers is fine:
	// it grants nothing new, it just names an alias.
	ok := call(t, h.api.identitySet, a.UserID, map[string]any{
		"create": map[string]any{
			"i1": map[string]any{"email": "shop@catchall.example.com", "name": "Shop"},
		},
	})
	if created, _ := ok["created"].(map[string]any); len(created) != 1 {
		t.Fatalf("pinning a name to an address the wildcard already covers should be allowed: %#v", ok)
	}

	// Creating one for an UNVERIFIED domain is forbidden — otherwise Identity/set
	// would be a trivial bypass of the whole From-validation boundary.
	bad := call(t, h.api.identitySet, a.UserID, map[string]any{
		"create": map[string]any{
			"i2": map[string]any{"email": "ceo@some-bank.example"},
		},
	})
	e := setError(t, bad, "notCreated", "i2")
	if e["type"] != setForbidden {
		t.Fatalf("SetError type = %v, want forbidden: a client must not be able to grant itself a new send-as right", e["type"])
	}

	// Nor may a client re-point an existing identity at a new address.
	upd := call(t, h.api.identitySet, a.UserID, map[string]any{
		"update": map[string]any{
			id0["id"].(string): map[string]any{"email": "ceo@some-bank.example"},
		},
	})
	ue := setError(t, upd, "notUpdated", id0["id"].(string))
	if ue["type"] != setInvalidProps {
		t.Fatalf("SetError type = %v, want invalidProperties: an identity's address is its verification and is immutable", ue["type"])
	}
}

// The session must advertise the submission + compose capabilities in the
// TOP-LEVEL set, not just per-account: a client may only `using` what is at the
// top level, so a URN that appears only in accountCapabilities leaves its
// methods unreachable. (This exact bug has shipped here once.)
func TestSessionAdvertisesSubmission(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)

	// The user id is injected the only way it ever is: by the auth middleware
	// (auth.DevAuth is the test-visible one). The API never takes it from a
	// parameter (spec §10).
	req := httptest.NewRequest(http.MethodGet, "/jmap/session", nil)
	rec := httptest.NewRecorder()
	auth.DevAuth(http.HandlerFunc(h.api.SessionHandler), a.UserID).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session: status %d", rec.Code)
	}

	var session struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Accounts     map[string]struct {
			AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}

	for _, urn := range []string{capSubmission, capCompose} {
		if _, ok := session.Capabilities[urn]; !ok {
			t.Errorf("%q is missing from the TOP-LEVEL capabilities: a client may only `using` what is there, so its methods would be unreachable", urn)
		}
		for id, acct := range session.Accounts {
			if _, ok := acct.AccountCapabilities[urn]; !ok {
				t.Errorf("%q is missing from accountCapabilities of %s", urn, id)
			}
		}
	}

	for _, m := range []string{"EmailSubmission/get", "EmailSubmission/set", "EmailSubmission/changes",
		"Identity/get", "Identity/set", "Identity/changes"} {
		if _, ok := h.api.methods[m]; !ok {
			t.Errorf("method %q is not registered", m)
		}
	}
}

// SEND LATER, and the horizon the session advertises (#56).
//
// A submission has always been a durable job with a fire-at time, so scheduling one
// is not new machinery — it is the default (`now + the undo window`) not being
// applied. What WAS missing is that `maxDelayedSend` was advertised in the session
// and never checked, so a client could schedule a send for the year 3000 and the
// server would write the job. A limit a client is told about and a limit the server
// enforces that disagree is worse than either.
func TestEmailSubmissionSetHonoursTheAdvertisedSendLaterHorizon(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")

	draft := func() string {
		t.Helper()
		res := call(t, h.api.emailSet, a.UserID, map[string]any{
			"create": map[string]any{
				"c1": map[string]any{
					"from":       []map[string]any{{"email": "me@example.com"}},
					"to":         []map[string]any{{"email": "friend@example.net"}},
					"subject":    "later",
					"bodyValues": map[string]any{"1": map[string]any{"value": "hi"}},
					"textBody":   []map[string]any{{"partId": "1"}},
				},
			},
		})
		created, _ := res["created"].(map[string]any)
		obj, ok := created["c1"].(map[string]any)
		if !ok {
			t.Fatalf("draft not created: %#v", res)
		}
		id, _ := obj["id"].(string)
		return id
	}

	// A time INSIDE the horizon is accepted and kept — the scheduled instant must
	// survive the round trip, or "send Tuesday" silently becomes "send now".
	want := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	ok := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{
			"s1": map[string]any{"emailId": draft(), "sendAt": want.Format(time.RFC3339)},
		},
	})
	created, _ := ok["created"].(map[string]any)
	obj, isMap := created["s1"].(map[string]any)
	if !isMap {
		t.Fatalf("a scheduled send was refused: %#v", ok)
	}
	got, _ := obj["sendAt"].(string)
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("sendAt %q is not RFC 3339: %v", got, err)
	}
	if !parsed.Equal(want) {
		t.Errorf("sendAt round-tripped as %v, want %v", parsed, want)
	}

	// BEYOND the horizon is refused, as invalidProperties naming sendAt, so a client
	// can point at the field rather than guessing.
	tooFar := time.Now().Add(maxDelayedSend + 24*time.Hour).UTC()
	bad := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{
			"s1": map[string]any{"emailId": draft(), "sendAt": tooFar.Format(time.RFC3339)},
		},
	})
	e := setError(t, bad, "notCreated", "s1")
	if e["type"] != setInvalidProps {
		t.Errorf("SetError type = %v, want invalidProperties for a sendAt past maxDelayedSend", e["type"])
	}
	props, _ := e["properties"].([]any)
	if len(props) != 1 || props[0] != "sendAt" {
		t.Errorf("SetError properties = %v, want [sendAt]", props)
	}

	// AND the number enforced is the number advertised, so the two cannot drift.
	advertised, _ := submissionCapability()["maxDelayedSend"].(int)
	if advertised != int(maxDelayedSend.Seconds()) {
		t.Errorf("session advertises maxDelayedSend=%d but the server enforces %d",
			advertised, int(maxDelayedSend.Seconds()))
	}
}
