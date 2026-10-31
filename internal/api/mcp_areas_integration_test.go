// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"mailroom/internal/store"
)

// THE AREA TOOLS (#49, ADR-044 §4): list_areas, move_to_area and
// search_mail's areaId, driven with the MCP SDK's own client through the real
// bearer gate (mcp_integration_test.go's stack).
//
// RED PROBES: make move_to_area skip its Email/get after the set and count
// every updated message as moved, and the topic-label case reports "moved"
// for a message still in Primary; drop the inArea line in search_mail and the
// area search returns the whole mailbox.
func TestMCPAreaTools(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	areas := h.areasOf(t, a.UserID)
	srv := h.mcpServer(t, a.UserID)
	token, _ := h.newKey(t, a.UserID, map[string]any{"name": "organiser", "scopes": presetOrganise})
	s := mcpConnect(t, srv, token)

	// list_areas: every area, in desk order, with no label ids exposed.
	out := mustOK(t, "list_areas", mcpCall(t, s, "list_areas", map[string]any{}))
	list := asArr(out["areas"])
	if len(list) != len(areas) {
		t.Fatalf("list_areas: %d areas, want %d: %v", len(list), len(areas), out)
	}
	var names []string
	for _, x := range list {
		names = append(names, asObj(x)["name"].(string))
	}
	if names[0] != "Primary" {
		t.Errorf("list_areas order = %v, want desk order (Primary first)", names)
	}

	filings, hand := h.filingEvidence(t, a.UserID)
	m1, m2 := agentMessage(t, h, a), agentMessage(t, h, a)
	ids := []string{m1.String(), m2.String()}
	out = mustOK(t, "move_to_area", mcpCall(t, s, "move_to_area", map[string]any{"emailIds": ids, "area": "Updates"}))
	if got := toStrings(out["emailIds"]); !slices.Equal(got, sorted(slices.Clone(ids))) {
		t.Fatalf("move_to_area moved %v, want %v (%v)", got, ids, out)
	}
	for _, m := range []string{m1.String(), m2.String()} {
		var by string
		_ = h.super.QueryRow(ctx, `SELECT applied_by FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
			m, areas["Updates"].LabelID.String()).Scan(&by)
		if by != store.LabelByAgent {
			t.Errorf("%s: the tool's filing is applied_by %q, want %q", m, by, store.LabelByAgent)
		}
	}
	if f2, h2 := h.filingEvidence(t, a.UserID); f2 != filings || h2 != hand {
		t.Errorf("move_to_area changed the evidence: filings %d -> %d, hand-applied %d -> %d", filings, f2, hand, h2)
	}

	// search_mail by area finds exactly them.
	out = mustOK(t, "search_mail", mcpCall(t, s, "search_mail", map[string]any{"areaId": areas["Updates"].ID.String()}))
	var found []string
	for _, x := range asArr(out["messages"]) {
		found = append(found, asObj(x)["id"].(string))
	}
	if !slices.Equal(sorted(found), sorted(slices.Clone(ids))) {
		t.Errorf("search_mail areaId=Updates = %v, want %v", found, ids)
	}

	// Again: already there, said rather than counted as moved.
	out = mustOK(t, "move_to_area", mcpCall(t, s, "move_to_area", map[string]any{"emailIds": ids, "area": areas["Updates"].ID.String()}))
	if len(toStrings(out["emailIds"])) != 0 || len(toStrings(out["alreadyThere"])) != 2 {
		t.Errorf("a second move: %v, want nothing moved and two alreadyThere", out)
	}

	// The owner's own filing is refused, and named.
	own := agentMessage(t, h, a)
	call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{own.String(): refilePatch(areas, "Promotions")}})
	res := mcpCall(t, s, "move_to_area", map[string]any{"emailIds": []string{own.String()}, "area": "Updates"})
	if !res.IsError || toolErrType(res) != setForbidden || !strings.Contains(fmt.Sprint(res.Object()), "by hand") {
		t.Errorf("moving the owner's own filing: %+v, want a forbidden tool error saying it was filed by hand", res)
	}

	// A topic label feeding an EARLIER area keeps the message there: reported
	// as notMoved, never as moved.
	topic := createLabels(t, h, a, "topic", 1)[0]
	if _, err := h.super.Exec(ctx, `INSERT INTO app.area_feeds (id, user_id, area_id, label_id) VALUES ($1, $2, $3, $4)`,
		store.NewULID().String(), a.UserID.String(), areas["Primary"].ID.String(), topic); err != nil {
		t.Fatal(err)
	}
	stuck := agentMessage(t, h, a)
	call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{stuck.String(): map[string]any{"labelIds/" + topic: true}}})
	res = mcpCall(t, s, "move_to_area", map[string]any{"emailIds": []string{stuck.String()}, "area": "Updates"})
	if !res.IsError || toolErrType(res) != "notMoved" {
		t.Errorf("a message held in Primary by a topic label: %+v, want a notMoved tool error", res)
	}

	// Unknown area: a clear notFound, nothing written.
	res = mcpCall(t, s, "move_to_area", map[string]any{"emailIds": []string{m1.String()}, "area": "No such area"})
	if !res.IsError || toolErrType(res) != "notFound" {
		t.Errorf("an unknown area: %+v, want notFound", res)
	}
}

// An account-restricted key lists areas and files its own accounts' mail,
// never another's. The tool reads the message first, and Email/get already
// treats another account's mail as notFound, so no update is even sent; the
// update path's own restriction is TestRestrictedKeyAreaMoveKeepsToItsAccounts
// (dropping keyVisibleMessages from emailSet's update loop leaves THIS test
// green, for that reason, and turns that one red).
func TestMCPMoveToAreaKeepsToTheRestriction(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	areas := h.areasOf(t, a.UserID)
	acct2, mbox2 := store.NewULID(), store.NewULID()
	if _, err := h.super.Exec(ctx, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1,$2,'second','mail.example.com','smtp.example.com',$3,$4)`,
		acct2.String(), a.UserID.String(), []byte{1}, store.NewULID().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		VALUES ($1,$2,$3,'INBOX','Inbox 2','\Inbox')`, mbox2.String(), a.UserID.String(), acct2.String()); err != nil {
		t.Fatal(err)
	}
	f2 := a
	f2.AccountID, f2.InboxID = acct2, mbox2
	theirs := agentMessage(t, h, f2)

	srv := h.mcpServer(t, a.UserID)
	one, _ := h.newKey(t, a.UserID, map[string]any{"name": "one", "scopes": presetOrganise,
		"accountIds": []string{a.AccountID.String()}})
	s := mcpConnect(t, srv, one)
	mustOK(t, "list_areas", mcpCall(t, s, "list_areas", map[string]any{}))
	res := mcpCall(t, s, "move_to_area", map[string]any{"emailIds": []string{theirs.String()}, "area": "Updates"})
	if !res.IsError || toolErrType(res) != "notFound" {
		t.Fatalf("a restricted key filing another account's mail: %+v, want notFound", res)
	}
	var n int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
		theirs.String(), areas["Updates"].LabelID.String()).Scan(&n)
	if n != 0 {
		t.Fatal("the other account's message was filed")
	}
}
