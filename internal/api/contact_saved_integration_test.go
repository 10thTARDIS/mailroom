// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The #816 Contact surface end to end (saved, userName, notes, the new query
// arguments, Contact/merge and Contact/separate), and the cross-user probes
// for the two new methods, registered into crossUserProbes so
// TestRLSCrossUserEveryMethod runs them with the rest.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func init() {
	crossUserProbes["Contact/merge"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		// Two fresh contacts of A's for the positive control, so the shared
		// fixture contact is not merged away under the other probes.
		mk := func(u rlsUser, label string) ulid.ULID {
			id := store.NewULID()
			superExec(t, h, `INSERT INTO app.contacts (id, user_id, display_name) VALUES ($1, $2, '')`, id.String(), u.UserID.String())
			superExec(t, h, `INSERT INTO app.contact_addresses (id, user_id, contact_id, address) VALUES ($1, $2, $3, $4)`,
				store.NewULID().String(), u.UserID.String(), id.String(), label+"-"+strings.ToLower(id.String())+"@example.net")
			return id
		}
		mine1, mine2 := mk(a, "m1"), mk(a, "m2")
		for _, args := range []map[string]any{
			{"into": mine1.String(), "from": b.ContactID.String()},
			{"into": b.ContactID.String(), "from": mine1.String()},
		} {
			res, mErr := invokeAs(t, h, "Contact/merge", a.UserID, args)
			if mErr == nil || mErr.Type != errNotFound {
				t.Errorf("A merging with B's contact %v: got %v %+v, want notFound", args, res, mErr)
			}
		}
		var mergedInto *string
		superScan(t, h, &mergedInto, `SELECT merged_into FROM app.contacts WHERE id = $1`, b.ContactID.String())
		var owner string
		superScan(t, h, &owner, `SELECT contact_id FROM app.contact_addresses WHERE user_id = $1 AND address = $2`,
			b.UserID.String(), b.ContactAddr)
		if mergedInto != nil || owner != b.ContactID.String() {
			t.Errorf("B's contact was touched: merged_into=%v address owner=%s", mergedInto, owner)
		}
		var mine1Addrs int
		superScan(t, h, &mine1Addrs, `SELECT count(*) FROM app.contact_addresses WHERE contact_id = $1`, mine1.String())
		if mine1Addrs != 1 {
			t.Errorf("a refused merge moved addresses: %d", mine1Addrs)
		}
		// Positive control: A's own pair merges.
		if _, mErr := invokeAs(t, h, "Contact/merge", a.UserID, map[string]any{"into": mine1.String(), "from": mine2.String()}); mErr != nil {
			t.Errorf("control: A merging A's own contacts: %+v", mErr)
		}
	}
	crossUserProbes["Contact/separate"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		var addrID string
		superScan(t, h, &addrID, `SELECT id FROM app.contact_addresses WHERE user_id = $1 AND contact_id = $2 LIMIT 1`,
			b.UserID.String(), b.ContactID.String())
		res, mErr := invokeAs(t, h, "Contact/separate", a.UserID, map[string]any{"id": b.ContactID.String(), "addressIds": []string{addrID}})
		if mErr == nil || mErr.Type != errNotFound {
			t.Errorf("A separating B's contact: got %v %+v, want notFound", res, mErr)
		}
		// A naming B's address on A's own contact: not one of A's addresses.
		res, mErr = invokeAs(t, h, "Contact/separate", a.UserID, map[string]any{"id": a.ContactID.String(), "addressIds": []string{addrID}})
		if mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("A separating B's address out of A's contact: got %v %+v, want invalidArguments", res, mErr)
		}
		var owner string
		superScan(t, h, &owner, `SELECT contact_id FROM app.contact_addresses WHERE id = $1`, addrID)
		if owner != b.ContactID.String() {
			t.Errorf("B's address moved to %s", owner)
		}
		// Positive control: B naming its own one-address contact is FOUND (and
		// refused only because a contact keeps one address).
		if _, mErr := invokeAs(t, h, "Contact/separate", b.UserID, map[string]any{"id": b.ContactID.String(), "addressIds": []string{addrID}}); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("control: B separating its only address: %+v, want invalidArguments", mErr)
		}
	}
}

func callErr(t *testing.T, fn methodFn, userID ulid.ULID, args any) *MethodError {
	t.Helper()
	_, mErr := fn(context.Background(), userID, mustJSON(t, args))
	return mErr
}

func TestContactSavedSurfaceEndToEnd(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	run := strings.ToLower(store.NewULID().String())
	alice := "alice-" + run + "@corp.test"
	bob := "bob-" + run + "@corp.test"
	seedContact(t, h, a.UserID, "alice", alice, "")
	seedContact(t, h, a.UserID, "Bob Learned", bob, "")
	aliceID, bobID := contactIDFor(t, h, a.UserID, alice), contactIDFor(t, h, a.UserID, bob)

	get := func(id ulid.ULID) map[string]any {
		t.Helper()
		res := call(t, h.api.contactGet, a.UserID, map[string]any{"ids": []string{id.String()}})
		list := res["list"].([]any)
		if len(list) != 1 {
			t.Fatalf("Contact/get %s: %v", id, res)
		}
		return list[0].(map[string]any)
	}
	obj := get(aliceID)
	if obj["isSaved"] != false || obj["savedAt"] != nil || obj["userName"] != nil || obj["name"] != "alice" ||
		obj["notes"] != "" || obj["inboundCount"] != float64(1) || obj["outboundCount"] != float64(0) ||
		obj["firstMailAt"] == nil || obj["lastMailAt"] == nil {
		t.Errorf("fresh contact = %v", obj)
	}
	if addrs := obj["addresses"].([]any); len(addrs) != 1 || addrs[0].(map[string]any)["id"] == "" {
		t.Errorf("addresses carry no id: %v", obj["addresses"])
	}

	// Contact/set: save, name, notes in one patch; one state step.
	res := call(t, h.api.contactSet, a.UserID, map[string]any{"update": map[string]any{
		aliceID.String(): map[string]any{"isSaved": true, "userName": "  Alice Mine\u202e ", "notes": "met at\r\nthe thing"},
		bobID.String():   map[string]any{"userName": strings.Repeat("x", 201)},
	}})
	if _, ok := res["updated"].(map[string]any)[aliceID.String()]; !ok {
		t.Fatalf("alice not updated: %v", res)
	}
	nu := res["notUpdated"].(map[string]any)[bobID.String()].(map[string]any)
	if nu["type"] != setInvalidProps || !strings.Contains(mustJSONString(t, nu["properties"]), "userName") {
		t.Errorf("over-long userName: %v", nu)
	}
	obj = get(aliceID)
	if obj["isSaved"] != true || obj["savedAt"] == nil || obj["userName"] != "Alice Mine" || obj["name"] != "Alice Mine" ||
		obj["notes"] != "met at\nthe thing" || obj["displayName"] != "alice" {
		t.Errorf("after set = %v", obj)
	}
	// displayName stays refused by name.
	res = call(t, h.api.contactSet, a.UserID, map[string]any{"update": map[string]any{
		aliceID.String(): map[string]any{"displayName": "x"}}})
	if _, ok := res["notUpdated"].(map[string]any)[aliceID.String()]; !ok {
		t.Errorf("displayName was accepted: %v", res)
	}

	// Contact/query: isSaved + sort by name; the unsaved bob is excluded; the
	// autocomplete form still finds both.
	res = call(t, h.api.contactQuery, a.UserID, map[string]any{
		"filter": map[string]any{"isSaved": true, "text": run},
		"sort":   []map[string]any{{"property": "name"}}, "position": 0, "limit": 500})
	if ids := res["ids"].([]any); len(ids) != 1 || ids[0] != aliceID.String() || res["total"] != float64(1) || res["limit"] != float64(100) {
		t.Errorf("saved query = %v", res)
	}
	res = call(t, h.api.contactQuery, a.UserID, map[string]any{"filter": map[string]any{"text": run}})
	if ids := res["ids"].([]any); len(ids) != 2 {
		t.Errorf("autocomplete form = %v, want both", res)
	}
	if mErr := callErr(t, h.api.contactQuery, a.UserID, map[string]any{"sort": []map[string]any{{"property": "seenCount"}}}); mErr == nil || mErr.Type != errUnsupportedSort {
		t.Errorf("unknown sort: %+v", mErr)
	}

	// Contact/merge bob into alice; bob is destroyed for /changes and get.
	before := call(t, h.api.contactGet, a.UserID, map[string]any{"ids": []string{}})["state"].(string)
	res = call(t, h.api.contactMerge, a.UserID, map[string]any{"into": aliceID.String(), "from": bobID.String()})
	if res["id"] != aliceID.String() || res["destroyedId"] != bobID.String() || res["addressesMoved"] != float64(1) || res["oldState"] != before {
		t.Errorf("merge = %v", res)
	}
	g := call(t, h.api.contactGet, a.UserID, map[string]any{"ids": []string{bobID.String()}})
	if nf := g["notFound"].([]any); len(nf) != 1 {
		t.Errorf("merged-away contact still readable: %v", g)
	}
	ch := call(t, h.api.contactChanges, a.UserID, map[string]any{"sinceState": before})
	if d := ch["destroyed"].([]any); len(d) != 1 || d[0] != bobID.String() {
		t.Errorf("changes after merge = %v", ch)
	}
	if u := ch["updated"].([]any); len(u) != 1 || u[0] != aliceID.String() {
		t.Errorf("changes after merge = %v", ch)
	}
	obj = get(aliceID)
	addrs := obj["addresses"].([]any)
	if len(addrs) != 2 || obj["inboundCount"] != float64(2) {
		t.Fatalf("merged contact = %v", obj)
	}
	var bobAddrID string
	for _, x := range addrs {
		if m := x.(map[string]any); m["address"] == bob {
			bobAddrID = m["id"].(string)
		}
	}
	if mErr := callErr(t, h.api.contactMerge, a.UserID, map[string]any{"into": aliceID.String(), "from": aliceID.String()}); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("self-merge: %+v", mErr)
	}

	// Contact/separate bob's address back out: a new contact, named as bob was.
	res = call(t, h.api.contactSeparate, a.UserID, map[string]any{"id": aliceID.String(), "addressIds": []string{bobAddrID}})
	created := res["created"].([]any)
	if len(created) != 1 || res["addressContacts"].(map[string]any)[bobAddrID] != created[0] {
		t.Fatalf("separate = %v", res)
	}
	// #834: every address the merge moved came back, so the merge was undone
	// on alice; nothing it wrote was edited since. keptFields is an array,
	// never null.
	if kept, ok := res["keptFields"].([]any); res["restored"] != true || !ok || len(kept) != 0 {
		t.Errorf("separate restored=%v keptFields=%#v, want true and []", res["restored"], res["keptFields"])
	}
	nid := ulid.MustParse(created[0].(string))
	if o := get(nid); o["displayName"] != "Bob Learned" || len(o["addresses"].([]any)) != 1 {
		t.Errorf("separated contact = %v", o)
	}
	var raw json.RawMessage
	raw, mErr := h.api.contactSeparate(context.Background(), a.UserID, mustJSON(t, map[string]any{"id": aliceID.String(), "addressIds": []string{}}))
	if mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("empty addressIds: %s %+v", raw, mErr)
	}
}
