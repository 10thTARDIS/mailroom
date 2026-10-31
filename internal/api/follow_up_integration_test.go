// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// fuMail seeds a real message (bytes in the encrypted blob store, read back
// through the API's own raw source) and then places it in time and thread.
func fuMail(t *testing.T, h *composeHarness, f apiFixture, thread ulid.ULID, from, to, subject string, daysAgo float64, headers string) ulid.ULID {
	t.Helper()
	msgID := fmt.Sprintf("<fu-%d@example.test>", time.Now().UnixNano())
	raw := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: %s\r\n%sContent-Type: text/plain\r\n\r\nHello.\r\n",
		from, to, subject, msgID, headers)
	id := seedRawMessage(t, h, f, []byte(raw))
	at := time.Now().UTC().Add(-time.Duration(daysAgo * float64(24*time.Hour)))
	irt := any(nil)
	for _, line := range strings.Split(headers, "\r\n") {
		if v, ok := strings.CutPrefix(line, "In-Reply-To: "); ok {
			irt = v
		}
	}
	if _, err := h.super.Exec(context.Background(), `
		UPDATE app.messages SET received_at = $2, from_address = $3, subject = $4, thread_id = $5,
		       message_id_hint = $6, in_reply_to = $7, from_name = ''
		 WHERE id = $1`, id.String(), at, from, subject, thread.String(), msgID, irt); err != nil {
		t.Fatalf("place message: %v", err)
	}
	return id
}

func fuThread(t *testing.T, h *composeHarness, f apiFixture) ulid.ULID {
	t.Helper()
	id := store.NewULID()
	if _, err := h.super.Exec(context.Background(), `INSERT INTO app.threads (id, user_id) VALUES ($1,$2)`,
		id.String(), f.UserID.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

func fuList(t *testing.T, h *composeHarness, user ulid.ULID) (ids []string, items []map[string]any, res map[string]any) {
	t.Helper()
	res = call(t, h.api.followUpGet, user, map[string]any{})
	for _, it := range res["list"].([]any) {
		m := it.(map[string]any)
		items = append(items, m)
		ids = append(ids, m["emailId"].(string)+":"+m["kind"].(string))
	}
	sort.Strings(ids)
	return ids, items, res
}

// FollowUp/get and /set end to end (#58): the default settings nudge only
// unanswered sent mail at seven days; the inbound nudge is opt-in; Done is
// stored, appends one Email change row, and never resurfaces.
func TestFollowUpEndToEnd(t *testing.T) {
	h := newComposeHarness(t)
	ctx := context.Background()
	x := seedRLSUser(t, h.super, "fu-"+strings.ToLower(store.NewULID().String()))
	f := x.apiFixture
	var me string
	if err := h.super.QueryRow(ctx, `SELECT address FROM app.identities WHERE id = $1`, x.IdentityID.String()).Scan(&me); err != nil {
		t.Fatal(err)
	}

	t1, t2, t3, t4 := fuThread(t, h, f), fuThread(t, h, f), fuThread(t, h, f), fuThread(t, h, f)
	waiting := fuMail(t, h, f, t1, me, "ann@example.org", "Plans", 10, "")
	answered := fuMail(t, h, f, t2, me, "bob@example.org", "Dinner", 10, "")
	bobsAnswer := fuMail(t, h, f, t2, "bob@example.org", me, "Re: Dinner", 9, "")
	_ = fuMail(t, h, f, t3, me, "cat@example.org", "Too new", 3, "")
	inbound := fuMail(t, h, f, t4, "dan@example.org", me, "Re: Your note", 5, "In-Reply-To: <earlier@example.test>\r\n")

	ids, items, res := fuList(t, h, f.UserID)
	if want := []string{waiting.String() + ":outbound"}; fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("default settings: %v, want %v (answered, too new and inbound excluded)", ids, want)
	}
	if items[0]["subject"] != "Plans" || items[0]["who"].(map[string]any)["email"] != "ann@example.org" {
		t.Fatalf("display: %+v", items[0])
	}
	if s := res["settings"].(map[string]any); s["outbound"] != true || s["inbound"] != false || s["inboundDays"] != float64(3) {
		t.Fatalf("settings: %+v", s)
	}
	_ = answered

	// Opt in to the inbound nudge through DeskPreferences/set.
	call(t, h.api.deskPreferencesSet, f.UserID, map[string]any{
		"update": map[string]any{f.UserID.String(): map[string]any{
			"followUp": map[string]any{"outbound": true, "inbound": true, "inboundDays": 3},
		}},
	})
	ids, _, _ = fuList(t, h, f.UserID)
	// Bob's answer is itself waiting on the user now: the user wrote to Bob
	// first, so the kind classifier calls it personal (as of its arrival).
	want := []string{waiting.String() + ":outbound", inbound.String() + ":inbound", bobsAnswer.String() + ":inbound"}
	sort.Strings(want)
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("inbound opted in: %v, want %v", ids, want)
	}

	// Done: one Email change row, gone, and does not come back.
	var before, after int
	count := func() int {
		var n int
		if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Email'`,
			f.UserID.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before = count()
	set := call(t, h.api.followUpSet, f.UserID, map[string]any{"dismiss": map[string]any{waiting.String(): "outbound"}})
	if fmt.Sprint(set["dismissed"]) != "["+waiting.String()+"]" {
		t.Fatalf("dismiss: %+v", set)
	}
	after = count()
	if after-before != 1 {
		t.Fatalf("Done appended %d Email change rows, want 1", after-before)
	}
	ids, _, _ = fuList(t, h, f.UserID)
	want = []string{inbound.String() + ":inbound", bobsAnswer.String() + ":inbound"}
	sort.Strings(want)
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("after Done: %v, want %v", ids, want)
	}

	// Snooze hides a nudge until it wakes.
	if _, err := h.super.Exec(ctx, `UPDATE app.messages SET snoozed_until = now() + interval '1 day' WHERE id = $1`, inbound.String()); err != nil {
		t.Fatal(err)
	}
	if ids, _, _ = fuList(t, h, f.UserID); fmt.Sprint(ids) != fmt.Sprint([]string{bobsAnswer.String() + ":inbound"}) {
		t.Fatalf("snoozed nudge still listed: %v", ids)
	}

	// Both off: nothing, and nothing read.
	call(t, h.api.deskPreferencesSet, f.UserID, map[string]any{
		"update": map[string]any{f.UserID.String(): map[string]any{
			"followUp": map[string]any{"outbound": false, "inbound": false, "inboundDays": 3},
		}},
	})
	if _, err := h.super.Exec(ctx, `UPDATE app.messages SET snoozed_until = NULL WHERE id = $1`, inbound.String()); err != nil {
		t.Fatal(err)
	}
	if ids, _, _ = fuList(t, h, f.UserID); len(ids) != 0 {
		t.Fatalf("both switched off, still listed: %v", ids)
	}

	// Validation.
	res2, _ := h.api.deskPreferencesSet(ctx, f.UserID, mustJSON(t, map[string]any{
		"update": map[string]any{f.UserID.String(): map[string]any{
			"followUp": map[string]any{"outbound": true, "inbound": true, "inboundDays": 0},
		}},
	}))
	if !strings.Contains(string(res2), "notUpdated") || !strings.Contains(string(res2), "inboundDays") {
		t.Fatalf("inboundDays 0 accepted: %s", res2)
	}
	if mErr := func() *MethodError {
		_, e := h.api.followUpSet(ctx, f.UserID, mustJSON(t, map[string]any{"dismiss": map[string]any{waiting.String(): "sideways"}}))
		return e
	}(); mErr != nil {
		t.Fatalf("bad kind is a per-id SetError, not a method error: %+v", mErr)
	}
}

// seedFollowUpDue gives u one outbound nudge that is due: a real sent message
// from u's identity, ten days old, unanswered, in its own thread.
func seedFollowUpDue(t *testing.T, h *rlsHarness, u rlsUser) ulid.ULID {
	t.Helper()
	var me string
	if err := h.super.QueryRow(context.Background(), `SELECT address FROM app.identities WHERE id = $1`,
		u.IdentityID.String()).Scan(&me); err != nil {
		t.Fatal(err)
	}
	thread := fuThread(t, h.composeHarness, u.apiFixture)
	return fuMail(t, h.composeHarness, u.apiFixture, thread, me, "someone@example.org", "Waiting", 10, "")
}
