// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// LearnedSender/get, forget, trust and LearnedSorting/status (#167, #169)
// against a real Postgres: the shapes the web client codes against, the refusal
// to API keys, and that no sender address ever reaches app.audit_log.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/compose"
	"mailroom/internal/store"
)

func TestLearnedSenderShapes(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)

	// get: an address never filed still gets an entry, with filings 0.
	res := call(t, h.api.learnedSenderGet, a.UserID, map[string]any{"senders": []string{"  Alice@Example.COM "}})
	if nf := asArr(res["notFound"]); res["notFound"] == nil || len(nf) != 0 {
		t.Errorf("notFound = %v, want an empty list", res["notFound"])
	}
	list := asArr(res["list"])
	if len(list) != 1 {
		t.Fatalf("list = %v, want one entry", res["list"])
	}
	e := list[0].(map[string]any)
	if e["sender"] != "alice@example.com" || e["decidedBy"] != "none" || e["trusted"] != false || e["forgottenAt"] != nil {
		t.Errorf("entry = %v", e)
	}
	addr := e["address"].(map[string]any)
	dom := e["domain"].(map[string]any)
	if addr["filings"] != float64(0) || addr["verdict"] != "none" || addr["acting"] != false || addr["areaId"] != nil || addr["reason"] != "none" {
		t.Errorf("address side = %v", addr)
	}
	if dom["domain"] != "example.com" || dom["acting"] != false {
		t.Errorf("domain side = %v", dom)
	}

	// One input that cannot be keyed does not fail the batch: it gets an empty
	// entry and notFound stays empty.
	mix := call(t, h.api.learnedSenderGet, a.UserID, map[string]any{"senders": []string{"not-an-address", "alice@example.com"}})
	ml := asArr(mix["list"])
	if len(ml) != 2 || len(asArr(mix["notFound"])) != 0 {
		t.Fatalf("mixed batch = %v, want two entries and an empty notFound", mix)
	}
	if bad := ml[0].(map[string]any); bad["sender"] != "not-an-address" || bad["decidedBy"] != "none" || bad["address"].(map[string]any)["filings"] != float64(0) || bad["address"].(map[string]any)["verdict"] != "none" {
		t.Errorf("the unkeyable entry = %v", bad)
	}
	if good := ml[1].(map[string]any); good["sender"] != "alice@example.com" || good["trustUseful"] != true {
		t.Errorf("the keyable entry = %v", good)
	}

	// trustUseful is false at one of the user's own identity domains.
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO app.identities (id, user_id, account_id, kind, address, display_name, enabled)
		VALUES ($1, $2, $3, 'fixed', 'me@mine-own.example', 'me', true)`,
		store.NewULID().String(), a.UserID.String(), a.AccountID.String()); err != nil {
		t.Fatalf("identity: %v", err)
	}
	own := call(t, h.api.learnedSenderGet, a.UserID, map[string]any{"senders": []string{"robot@mine-own.example", "robot@elsewhere.example"}})
	ol := asArr(own["list"])
	if ol[0].(map[string]any)["trustUseful"] != false || ol[1].(map[string]any)["trustUseful"] != true {
		t.Errorf("trustUseful = %v / %v, want false at an own domain, true elsewhere", ol[0].(map[string]any)["trustUseful"], ol[1].(map[string]any)["trustUseful"])
	}

	// Forget works for an accented address; trusting it is still refused.
	call(t, h.api.learnedSenderForget, a.UserID, map[string]any{"sender": "Jos\u00e9@example.com", "scope": "address"})
	if _, mErr := h.api.learnedSenderTrust(context.Background(), a.UserID, mustJSON(t, map[string]any{"sender": "jos\u00e9@example.com", "trusted": true})); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("trusting an accented address: got %+v, want invalidArguments", mErr)
	}

	// Bounds: 0 and 51 senders are refused.
	for _, bad := range [][]string{{}, make([]string, 51)} {
		for i := range bad {
			if bad[i] == "" {
				bad[i] = "x@example.com"
			}
		}
		if _, mErr := h.api.learnedSenderGet(context.Background(), a.UserID, mustJSON(t, map[string]any{"senders": bad})); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("senders %v: got %+v, want invalidArguments", bad, mErr)
		}
	}

	// trust, then untrust; the idempotent repeat adds no Activity entry.
	tr := call(t, h.api.learnedSenderTrust, a.UserID, map[string]any{"sender": "Robot@Example.org", "trusted": true})
	if tr["updated"].(map[string]any)["trusted"] != true {
		t.Errorf("trust response = %v", tr)
	}
	call(t, h.api.learnedSenderTrust, a.UserID, map[string]any{"sender": "robot@example.org", "trusted": true})
	// Trust is for an exact ASCII address only.
	for _, bad := range []string{"kévin@example.org", "Kevin@example.org"} {
		if _, mErr := h.api.learnedSenderTrust(context.Background(), a.UserID, mustJSON(t, map[string]any{"sender": bad, "trusted": true})); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("trusting %q: got %+v, want invalidArguments", bad, mErr)
		}
	}
	if _, mErr := h.api.learnedSenderTrust(context.Background(), a.UserID, mustJSON(t, map[string]any{"sender": "robot@example.org"})); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("trust without trusted: got %+v, want invalidArguments", mErr)
	}

	// forget: the fresh state carries the mark; scope is required and closed.
	fg := call(t, h.api.learnedSenderForget, a.UserID, map[string]any{"sender": "robot@example.org", "scope": "address"})
	f := fg["forgotten"].(map[string]any)
	if f["forgottenAt"] == nil || f["trusted"] != true {
		t.Errorf("forget response = %v", fg)
	}
	if _, mErr := h.api.learnedSenderForget(context.Background(), a.UserID, mustJSON(t, map[string]any{"sender": "robot@example.org", "scope": "everything"})); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("scope everything: got %+v, want invalidArguments", mErr)
	}
	call(t, h.api.learnedSenderTrust, a.UserID, map[string]any{"sender": "robot@example.org", "trusted": false})

	// status shape.
	st := call(t, h.api.learnedSortingStatus, a.UserID, map[string]any{})
	for _, k := range []string{"enabled", "windowDays", "placed", "corrected", "precision", "lowerBound", "sendersLearned", "heldBack", "heldBackSince", "areas"} {
		if _, ok := st[k]; !ok {
			t.Errorf("status has no %q: %v", k, st)
		}
	}
	if st["windowDays"] != float64(30) || st["precision"] != nil || st["placed"] != float64(0) {
		t.Errorf("status = %v", st)
	}
	if hb := st["heldBack"].(map[string]any); len(hb) == 0 || hb["thin"] != float64(0) {
		t.Errorf("heldBack = %v, want the whole vocabulary with zeros", hb)
	} else if _, bad := hb["placed"]; bad {
		t.Errorf("heldBack lists \"placed\": %v", hb)
	}

	// Activity: one entry each for forget, trust and untrust, naming the sender.
	act := call(t, h.api.activityList, a.UserID, map[string]any{"limit": 100})
	var outcomes []string
	for _, x := range asArr(act["list"]) {
		m := x.(map[string]any)
		if m["kind"] == "learned_sender" && m["keyName"] == "robot@example.org" {
			outcomes = append(outcomes, m["outcome"].(string))
		}
	}
	if len(outcomes) != 3 {
		t.Errorf("learned_sender activity entries = %v, want forget, trust and untrust once each", outcomes)
	}
}

// API keys are refused all four methods; the session is the positive control.
func TestLearnedSenderMethodsRefusedToKeys(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	token, _ := h.newKey(t, a.UserID, map[string]any{"name": "reader", "scopes": []string{"mail:read", "mail:move"}})
	for method, args := range map[string]map[string]any{
		"LearnedSender/get":     {"senders": []string{"x@example.com"}},
		"LearnedSender/forget":  {"sender": "x@example.com", "scope": "address"},
		"LearnedSender/trust":   {"sender": "x@example.com", "trusted": true},
		"LearnedSorting/status": {},
	} {
		name, res := h.keyCall(t, token, method, args)
		wantForbidden(t, method+" for a key", name, res)
	}
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.learned_sender_trust WHERE user_id = $1`, a.UserID.String()).Scan(&n); err != nil || n != 0 {
		t.Errorf("a refused key still trusted a sender (%d rows, %v)", n, err)
	}
	// Positive control.
	call(t, h.api.learnedSenderTrust, a.UserID, map[string]any{"sender": "x@example.com", "trusted": true})
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.learned_sender_trust WHERE user_id = $1`, a.UserID.String()).Scan(&n); err != nil || n != 1 {
		t.Errorf("the session's own trust was not stored (%d rows, %v)", n, err)
	}
}

// ⚠ audit_log survives a hard delete of the user (user_id is ON DELETE SET
// NULL), so a sender address in it would outlive the account. Activity shows the
// address while the user exists, and nothing in audit_log holds it afterwards.
func TestLearnedSenderAddressNeverReachesTheAuditLog(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	const addr = "private-sender-xyz@example.net"
	call(t, h.api.learnedSenderTrust, a.UserID, map[string]any{"sender": addr, "trusted": true})
	call(t, h.api.learnedSenderForget, a.UserID, map[string]any{"sender": addr, "scope": "domain"})
	call(t, h.api.learnedSenderTrust, a.UserID, map[string]any{"sender": addr, "trusted": false})

	leaked := func() int {
		var n int
		if err := h.super.QueryRow(context.Background(), `
			SELECT count(*) FROM app.audit_log
			 WHERE position($1 in coalesce(object_id, '')) > 0 OR position($1 in detail::text) > 0
			    OR position($1 in actor) > 0 OR position($1 in action) > 0`, "private-sender-xyz").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// While the user exists: Activity names the sender, and the audit rows exist
	// (the control that the test can see them) without carrying it.
	act := call(t, h.api.activityList, a.UserID, map[string]any{"limit": 100})
	named := 0
	for _, x := range asArr(act["list"]) {
		if m := x.(map[string]any); m["kind"] == "learned_sender" && m["keyName"] == addr {
			named++
		}
	}
	if named != 3 {
		t.Errorf("Activity names the sender on %d entries, want 3", named)
	}
	var rows int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.audit_log WHERE user_id = $1 AND object_type = 'LearnedSender'`, a.UserID.String()).Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("audit rows for learned sender events = %d (%v), want 3", rows, err)
	}
	if n := leaked(); n != 0 {
		t.Errorf("%d audit row(s) carry the sender address", n)
	}

	// After the hard delete: the audit rows survive, anonymised, and still no address.
	if err := h.store.SystemTx(context.Background(), func(tx *store.SystemTx) error {
		return tx.HardDeleteUser(context.Background(), a.UserID, "test:learned-sender")
	}); err != nil {
		t.Fatalf("HardDeleteUser: %v", err)
	}
	if n := leaked(); n != 0 {
		t.Errorf("after hard delete, %d audit row(s) carry the sender address", n)
	}
	var left int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.learned_sender_events WHERE address = $1`, addr).Scan(&left); err != nil || left != 0 {
		t.Errorf("learned_sender_events rows left after hard delete = %d (%v), want 0", left, err)
	}
}

// ── LearnedSender/get with a message (#169) ──────────────────────────────────

const trust3MX = "mx.example.test"

func trust3Entry(t *testing.T, h *composeHarness, user ulid.ULID, args map[string]any) map[string]any {
	t.Helper()
	res := call(t, h.api.learnedSenderGet, user, args)
	list := asArr(res["list"])
	if len(list) != 1 {
		t.Fatalf("list = %v, want one entry", res["list"])
	}
	return list[0].(map[string]any)
}

func trust3Reason(e map[string]any) any { return e["address"].(map[string]any)["reason"] }

// The card passes the message it was opened from; the server runs the arrival
// path's sender checks on that message's own headers.
func TestLearnedSenderGetWithAMessage(t *testing.T) {
	ah := newAgentHarness(t)
	h := ah.composeHarness
	h.api = New(h.store, WithComposer(compose.NewComposer(h.blobs, fakeKeys{}, time.Millisecond)),
		WithTrustedAuthservIDs([]string{trust3MX}))
	a, b := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@mine-own.example", "")

	noPass := seedMessageWithAuth(t, h, a, "alice@example.com")
	pass := seedMessageWithAuth(t, h, a, "alice@example.com", trust3MX+"; dmarc=pass header.from=example.com")
	fail := seedMessageWithAuth(t, h, a, "alice@example.com", trust3MX+"; dmarc=fail header.from=example.com")
	spoof := seedMessageWithAuth(t, h, a, "me@mine-own.example")
	get := func(user ulid.ULID, sender string, msg ulid.ULID) map[string]any {
		args := map[string]any{"senders": []string{sender}}
		if msg != (ulid.ULID{}) {
			args["messageId"] = msg.String()
		}
		return trust3Entry(t, h, user, args)
	}
	// Evidence: the user filed one message from alice into an area, so the
	// decision acts and a refusal is what stops it.
	filed := seedMessageWithAuth(t, h, a, "alice@example.com", "elsewhere.example; dkim=none") // distinct bytes: one blob per content hash
	label := ah.areasOf(t, a.UserID)["Updates"].LabelID
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by) VALUES ($1, $2, $3, 'user')`,
		a.UserID.String(), filed.String(), label.String()); err != nil {
		t.Fatal(err)
	}

	// No messageId: unchanged. No reason "unauthenticated", no trustWouldHelp.
	e := get(a.UserID, "alice@example.com", ulid.ULID{})
	if e["trustWouldHelp"] != false || trust3Reason(e) == "unauthenticated" {
		t.Errorf("without messageId: %v", e)
	}

	// No trusted pass: refused only for authentication; trust would help.
	e = get(a.UserID, "alice@example.com", noPass)
	if trust3Reason(e) != "unauthenticated" || e["trustWouldHelp"] != true || e["address"].(map[string]any)["acting"] != false {
		t.Errorf("no trusted pass: %v", e)
	}
	// CONTROL: a trusted pass is acted on, and trust would not change anything.
	e = get(a.UserID, "alice@example.com", pass)
	if e["trustWouldHelp"] != false || trust3Reason(e) != nil || e["address"].(map[string]any)["acting"] != true {
		t.Errorf("trusted pass: %v", e)
	}
	// A trusted failure: not helped by trust, and reported as failed.
	e = get(a.UserID, "alice@example.com", fail)
	if e["trustWouldHelp"] != false || trust3Reason(e) != "failed" {
		t.Errorf("trusted dmarc=fail: %v", e)
	}
	// A self-spoof warning: not helped by trust (and trust cannot help at an
	// own domain at all).
	e = get(a.UserID, "me@mine-own.example", spoof)
	if e["trustWouldHelp"] != false {
		t.Errorf("self-spoof: %v", e)
	}
	// A message from another sender is ignored: unchanged behaviour.
	e = get(a.UserID, "carol@example.com", noPass)
	if e["trustWouldHelp"] != false || trust3Reason(e) == "unauthenticated" {
		t.Errorf("message from a different sender: %v", e)
	}
	// Another user's message id is ignored, with no error.
	other := seedMessageWithAuth(t, h, b, "alice@example.com")
	e = get(a.UserID, "alice@example.com", other)
	if e["trustWouldHelp"] != false || trust3Reason(e) == "unauthenticated" {
		t.Errorf("another user's message honoured: %v", e)
	}
	// CONTROL for the above: the owner reads the same message fine.
	if e = get(b.UserID, "alice@example.com", other); e["trustWouldHelp"] != true {
		t.Errorf("control: owner's own message: %v", e)
	}
	// A garbage id is ignored too.
	e = trust3Entry(t, h, a.UserID, map[string]any{"senders": []string{"alice@example.com"}, "messageId": "nope"})
	if e["trustWouldHelp"] != false {
		t.Errorf("garbage id: %v", e)
	}

	// After trusting: trusted is true and the no-pass message is acted on.
	call(t, h.api.learnedSenderTrust, a.UserID, map[string]any{"sender": "alice@example.com", "trusted": true})
	e = get(a.UserID, "alice@example.com", noPass)
	if e["trusted"] != true || e["trustWouldHelp"] != false || e["address"].(map[string]any)["acting"] != true {
		t.Errorf("after trusting: %v", e)
	}
	// Trust waives the absence of a pass, never a trusted failure.
	e = get(a.UserID, "alice@example.com", fail)
	if e["address"].(map[string]any)["acting"] != false || trust3Reason(e) != "failed" {
		t.Errorf("trusted sender, trusted dmarc=fail: %v", e)
	}
}
