// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// EMAILSUBMISSION/QUERY (#653): the Outbox after a reload.
//
// The web client's Outbox was empty after every reload: its bootstrap never
// asked for submissions, and EmailSubmission/get with `ids: null` would have
// answered the 100 most recent only, silently. These tests pin the query the
// bootstrap now makes — above all `inOutbox`, which must select exactly what
// web/src/app/ui/outbox.ts outboxSubmissions shows: the LATEST submission per
// message, if it is scheduled, or failed and not dismissed.

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"mailroom/internal/store"
)

// supersede records a NEWER submission for the same message as `older`, in
// `state` — what a re-send leaves behind. Its id is minted after `older`'s, so
// it is the later one by both id and created_at.
func supersede(t *testing.T, h *composeHarness, older, state string) string {
	t.Helper()
	id := store.NewULID().String()
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO app.submissions
		  (id, user_id, account_id, identity_id, message_id, from_address, envelope_to, state)
		SELECT $2, user_id, account_id, identity_id, message_id, from_address, envelope_to, $3
		  FROM app.submissions WHERE id = $1`, older, id, state); err != nil {
		t.Fatalf("supersede %s: %v", older, err)
	}
	return id
}

func messageOf(t *testing.T, h *composeHarness, submissionID string) string {
	t.Helper()
	var id string
	if err := h.super.QueryRow(context.Background(),
		`SELECT message_id FROM app.submissions WHERE id = $1`, submissionID).Scan(&id); err != nil {
		t.Fatalf("message of %s: %v", submissionID, err)
	}
	return id
}

func querySubmissions(t *testing.T, h *composeHarness, a apiFixture, args map[string]any) map[string]any {
	t.Helper()
	return call(t, h.api.submissionQuery, a.UserID, args)
}

func queryError(t *testing.T, h *composeHarness, a apiFixture, args map[string]any) *MethodError {
	t.Helper()
	_, mErr := h.api.submissionQuery(context.Background(), a.UserID, mustJSON(t, args))
	return mErr
}

func idsOf(res map[string]any) []string {
	var out []string
	for _, v := range asArr(res["ids"]) {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// submissionQueryFixture is one user's submissions in every state the Outbox
// has to tell apart, created in this order (so newest-first is the reverse).
type submissionQueryFixture struct {
	scheduled, failed, dismissed, sent, canceled, sending string
	// supersededBySent is a failure answered by a re-send that has gone;
	// supersededBySched one answered by a re-send still in its window.
	supersededBySent, resentSent   string
	supersededBySched, resentSched string
	all                            []string
}

func seedSubmissionQuery(t *testing.T, h *composeHarness, a apiFixture, from string) submissionQueryFixture {
	t.Helper()
	seedSendable(t, h.super, a, from, "")
	var f submissionQueryFixture
	mk := func(subject, state string) string { return submitAndSetState(t, h, a, subject, state) }
	f.scheduled = mk("in the window", "scheduled")
	f.failed = mk("refused", "failed")
	f.dismissed = mk("refused and dismissed", "failed")
	if res := dismiss(t, h, a, f.dismissed, true); res["updated"] == nil {
		t.Fatalf("dismiss: %#v", res)
	}
	f.sent = mk("gone", "sent")
	f.canceled = mk("undone", "canceled")
	f.sending = mk("in flight", "sending")
	f.supersededBySent = mk("refused then re-sent", "failed")
	f.resentSent = supersede(t, h, f.supersededBySent, "sent")
	f.supersededBySched = mk("refused then re-sent later", "failed")
	f.resentSched = supersede(t, h, f.supersededBySched, "scheduled")
	f.all = []string{f.scheduled, f.failed, f.dismissed, f.sent, f.canceled, f.sending,
		f.supersededBySent, f.resentSent, f.supersededBySched, f.resentSched}
	return f
}

// reversed is newest-first for ids given oldest-first.
func reversed(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Reverse(out)
	return out
}

func TestSubmissionQueryInOutbox(t *testing.T) {
	h := newComposeHarness(t)
	a, b := seedTwo(t, h.super)
	f := seedSubmissionQuery(t, h, a, "me@example.com")
	// B has a scheduled and a failed send: both in B's Outbox, neither in A's.
	seedSendable(t, h.super, b, "me@example.com", "") // submitAndSetState drafts From me@example.com
	bSched := submitAndSetState(t, h, b, "b pending", "scheduled")
	bFailed := submitAndSetState(t, h, b, "b refused", "failed")

	res := querySubmissions(t, h, a, map[string]any{"filter": map[string]any{"inOutbox": true}, "calculateTotal": true})

	// Included: the scheduled send, the undismissed failure, and the re-send
	// still in its window. Excluded: the dismissed failure, sent, canceled,
	// sending (undoStatus "final", which the Outbox does not show), both
	// superseded failures, the re-send that has gone, and all of B's.
	want := reversed(f.scheduled, f.failed, f.resentSched)
	if got := idsOf(res); !slices.Equal(got, want) {
		t.Errorf("inOutbox ids = %v\nwant %v (newest first)\nfixture %+v", got, want, f)
	}
	if res["total"] != float64(3) {
		t.Errorf("inOutbox total = %v, want 3", res["total"])
	}
	for _, id := range []string{bSched, bFailed} {
		if slices.Contains(idsOf(res), id) {
			t.Errorf("A's Outbox query returned B's submission %s (RLS)", id)
		}
	}
	// No limit was asked for, so the one applied is reported (RFC 8620 §5.5).
	if res["limit"] != float64(maxSubmissionQueryLimit) {
		t.Errorf("limit = %v with none requested, want the applied %d reported", res["limit"], maxSubmissionQueryLimit)
	}
	if res["canCalculateChanges"] != false {
		t.Errorf("canCalculateChanges = %v, want false", res["canCalculateChanges"])
	}
	// queryState is the EmailSubmission state string.
	get := call(t, h.api.submissionGet, a.UserID, map[string]any{"ids": []string{}})
	if res["queryState"] != get["state"] {
		t.Errorf("queryState = %v, EmailSubmission state = %v; want equal", res["queryState"], get["state"])
	}
}

// Each exclusion on its own, so a failure names the clause that broke rather
// than a set difference.
func TestSubmissionQueryInOutboxExclusions(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	f := seedSubmissionQuery(t, h, a, "me@example.com")
	got := idsOf(querySubmissions(t, h, a, map[string]any{"filter": map[string]any{"inOutbox": true}}))
	for name, id := range map[string]string{
		"a DISMISSED failure":                       f.dismissed,
		"a failure SUPERSEDED by a sent re-send":    f.supersededBySent,
		"a failure SUPERSEDED by a pending re-send": f.supersededBySched,
		"a sent submission":                         f.sent,
		"a canceled submission":                     f.canceled,
		"a submission in flight (final)":            f.sending,
		"a sent re-send":                            f.resentSent,
	} {
		if slices.Contains(got, id) {
			t.Errorf("inOutbox included %s (%s)", name, id)
		}
	}
	for name, id := range map[string]string{
		"a scheduled send":        f.scheduled,
		"an undismissed failure":  f.failed,
		"a re-send in its window": f.resentSched,
	} {
		if !slices.Contains(got, id) {
			t.Errorf("inOutbox left out %s (%s)", name, id)
		}
	}
}

func TestSubmissionQueryUndoStatusAndEmailIDs(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	f := seedSubmissionQuery(t, h, a, "me@example.com")

	cases := map[string][]string{
		"pending":  reversed(f.scheduled, f.resentSched),
		"canceled": {f.canceled},
		"final": reversed(f.failed, f.dismissed, f.sent, f.sending,
			f.supersededBySent, f.resentSent, f.supersededBySched),
	}
	for status, want := range cases {
		res := querySubmissions(t, h, a, map[string]any{"filter": map[string]any{"undoStatus": status}})
		if got := idsOf(res); !slices.Equal(got, want) {
			t.Errorf("undoStatus %s = %v, want %v", status, got, want)
		}
		if res["total"] != float64(len(want)) {
			t.Errorf("undoStatus %s total = %v, want %d", status, res["total"], len(want))
		}
	}

	// emailIds: both submissions of a re-sent message, newest first.
	msg := messageOf(t, h, f.supersededBySent)
	res := querySubmissions(t, h, a, map[string]any{"filter": map[string]any{"emailIds": []string{msg}}})
	if got, want := idsOf(res), reversed(f.supersededBySent, f.resentSent); !slices.Equal(got, want) {
		t.Errorf("emailIds [%s] = %v, want %v", msg, got, want)
	}
	// ANDed with undoStatus.
	res = querySubmissions(t, h, a, map[string]any{"filter": map[string]any{
		"emailIds": []string{msg}, "undoStatus": "final"}})
	if got := idsOf(res); len(got) != 2 {
		t.Errorf("emailIds+final = %v, want both (failed and sent are both final)", got)
	}
	// An empty list matches nothing, not everything.
	res = querySubmissions(t, h, a, map[string]any{"filter": map[string]any{"emailIds": []string{}}})
	if got := idsOf(res); len(got) != 0 || res["total"] != float64(0) {
		t.Errorf("emailIds [] = %v (total %v), want none", got, res["total"])
	}
}

// Refusals: nothing the method does not implement is silently ignored.
func TestSubmissionQueryRefusals(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")

	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"an RFC condition this server does not implement": {
			map[string]any{"filter": map[string]any{"identityIds": []string{a.AccountID.String()}}}, errUnsupportedFilter},
		"an unknown condition": {
			map[string]any{"filter": map[string]any{"isOutbox": true}}, errUnsupportedFilter},
		"a FilterOperator": {
			map[string]any{"filter": map[string]any{"operator": "AND", "conditions": []any{}}}, errUnsupportedFilter},
		"a sort": {
			map[string]any{"sort": []map[string]any{{"property": "sentAt"}}}, errUnsupportedSort},
		"an anchor": {
			map[string]any{"anchor": a.AccountID.String()}, errInvalidArguments},
		"inOutbox false": {
			map[string]any{"filter": map[string]any{"inOutbox": false}}, errInvalidArguments},
		"an unknown undoStatus": {
			map[string]any{"filter": map[string]any{"undoStatus": "sent"}}, errInvalidArguments},
		"a negative limit": {
			map[string]any{"limit": -1}, errInvalidArguments},
	} {
		mErr := queryError(t, h, a, tc.args)
		if mErr == nil || mErr.Type != tc.want {
			t.Errorf("%s: got %+v, want %s", name, mErr, tc.want)
		}
	}
}

// Paging returns every row exactly once, the total is the whole result on
// every page, and a clamped limit is reported rather than applied silently.
func TestSubmissionQueryPaging(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	f := seedSubmissionQuery(t, h, a, "me@example.com")
	want := reversed(f.all...)

	var got []string
	for pos := 0; pos < 20; pos += 3 {
		res := querySubmissions(t, h, a, map[string]any{"position": pos, "limit": 3, "calculateTotal": true})
		if res["total"] != float64(len(f.all)) {
			t.Fatalf("page at %d: total = %v, want %d", pos, res["total"], len(f.all))
		}
		if res["position"] != float64(pos) {
			t.Errorf("page at %d: position = %v", pos, res["position"])
		}
		if _, ok := res["limit"]; ok {
			t.Errorf("page at %d: limit %v reported although the requested 3 was applied", pos, res["limit"])
		}
		page := idsOf(res)
		if len(page) > 3 {
			t.Fatalf("page at %d has %d ids, limit 3", pos, len(page))
		}
		got = append(got, page...)
	}
	if !slices.Equal(got, want) {
		t.Errorf("paged ids = %v\nwant       %v (each exactly once, newest first)", got, want)
	}

	// A negative position counts from the end (RFC 8620 §5.5).
	res := querySubmissions(t, h, a, map[string]any{"position": -2, "limit": 5})
	if got := idsOf(res); !slices.Equal(got, want[len(want)-2:]) || res["position"] != float64(len(want)-2) {
		t.Errorf("position -2 = %v at %v, want %v at %d", got, res["position"], want[len(want)-2:], len(want)-2)
	}

	// limit: 0 is the total and no ids.
	res = querySubmissions(t, h, a, map[string]any{"limit": 0, "calculateTotal": true})
	if len(idsOf(res)) != 0 || res["total"] != float64(len(f.all)) {
		t.Errorf("limit 0 = %v total %v, want no ids and total %d", idsOf(res), res["total"], len(f.all))
	}

	// Over the maximum: clamped AND reported.
	res = querySubmissions(t, h, a, map[string]any{"limit": maxSubmissionQueryLimit + 1})
	if res["limit"] != float64(maxSubmissionQueryLimit) {
		t.Errorf("limit %d: reported limit = %v, want %d", maxSubmissionQueryLimit+1, res["limit"], maxSubmissionQueryLimit)
	}
}

// The bootstrap's shape, through the dispatcher: the query's ids feed
// EmailSubmission/get through an RFC 8620 §3.7 back-reference, and the
// `using` gate admits the method under the submission capability.
func TestSubmissionQueryBackReferenceIntoGet(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	f := seedSubmissionQuery(t, h, a, "me@example.com")

	req := Request{
		Using: []string{capCore, capMail, capSubmission, capCompose},
		MethodCalls: []Invocation{
			{Name: "EmailSubmission/query", Args: mustJSON(t, map[string]any{
				"filter": map[string]any{"inOutbox": true}, "calculateTotal": true}), CallID: "q"},
			{Name: "EmailSubmission/get", Args: mustJSON(t, map[string]any{
				"#ids": map[string]any{"resultOf": "q", "name": "EmailSubmission/query", "path": "/ids"},
			}), CallID: "g"},
		},
	}
	resp := h.api.dispatch(context.Background(), a.UserID, req)
	if len(resp.MethodResponses) != 2 {
		t.Fatalf("got %d responses", len(resp.MethodResponses))
	}
	if q := resp.MethodResponses[0]; q.Name != "EmailSubmission/query" {
		t.Fatalf("first response = %q (%s)", q.Name, q.Args)
	}
	get := resp.MethodResponses[1]
	if get.Name != "EmailSubmission/get" {
		t.Fatalf("second response = %q (%s)", get.Name, get.Args)
	}
	var gr getResult
	if err := json.Unmarshal(get.Args, &gr); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, obj := range gr.List {
		id, _ := obj["id"].(string)
		got = append(got, id)
	}
	if want := reversed(f.scheduled, f.failed, f.resentSched); !slices.Equal(got, want) {
		t.Errorf("EmailSubmission/get via #ids = %v, want %v", got, want)
	}
	if len(gr.NotFound) != 0 {
		t.Errorf("notFound = %v, want none", gr.NotFound)
	}

	// Without the submission capability the method is not callable at all.
	req.Using = []string{capCore, capMail}
	resp = h.api.dispatch(context.Background(), a.UserID, req)
	if resp.MethodResponses[0].Name != "error" {
		t.Errorf("EmailSubmission/query without %s answered %q, want an error", capSubmission, resp.MethodResponses[0].Name)
	}
}
