// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// SavedSearch over the wire (#57).
//
// What is worth proving here is not "a row round-trips". It is the three claims the design
// rests on:
//
//   1. THE QUERY SURVIVES VERBATIM. It is stored as the operator string precisely so a
//      saved search runs through the same path as typing it; a server that normalised,
//      trimmed differently, or re-quoted anything would break that equivalence silently,
//      and the client would keep showing the user text that no longer matched what ran.
//   2. AN UPDATE IS A FULL OBJECT and behaves like one, so a partial write cannot move a
//      folder out of the sidebar while renaming it (the #170 class of bug).
//   3. A REFUSAL IS PER-OBJECT, naming the offending property, so one bad create cannot
//      take down a batch and the client can point at the field to fix.

import (
	"strings"
	"testing"

	"mailroom/internal/store"
)

func savedFromSet(t *testing.T, res map[string]any, bucket, key string) map[string]any {
	t.Helper()
	b, _ := res[bucket].(map[string]any)
	obj, ok := b[key].(map[string]any)
	if !ok {
		t.Fatalf("no %s[%s] in %#v", bucket, key, res)
	}
	return obj
}

func TestSavedSearchKeepsTheQueryVerbatim(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// A query with everything a normaliser might be tempted to touch: a quoted phrase
	// with a space, a negation, mixed case, an operator with a colon in its value, and
	// runs of internal whitespace.
	query := `from:Alice@Example.com -label:"Big Project"  subject:"end of year" is:unread`

	res := call(t, h.api.savedSearchSet, a.UserID, map[string]any{
		"create": map[string]any{
			"s1": map[string]any{"name": "Alice unread", "query": query},
		},
	})
	created := savedFromSet(t, res, "created", "s1")
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %#v", created)
	}
	if got, _ := created["query"].(string); got != query {
		t.Errorf("create echoed query\n got %q\nwant %q", got, query)
	}
	// Pinned by default when the client does not say — saving a search whose point is to
	// be a folder should not need a second control.
	if pinned, _ := created["inSidebar"].(bool); !pinned {
		t.Error("inSidebar defaulted to false; a new saved search should be a folder")
	}

	got := call(t, h.api.savedSearchGet, a.UserID, map[string]any{"ids": []string{id}})
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("get returned %d objects: %#v", len(list), got)
	}
	obj, _ := list[0].(map[string]any)
	if q, _ := obj["query"].(string); q != query {
		t.Errorf("stored query differs from what was sent\n got %q\nwant %q", q, query)
	}
}

func TestSavedSearchUpdateIsAFullObject(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.savedSearchSet, a.UserID, map[string]any{
		"create": map[string]any{
			"s1": map[string]any{
				"name": "pinned one", "query": "is:unread", "inSidebar": true, "position": 3,
			},
		},
	})
	id, _ := savedFromSet(t, res, "created", "s1")["id"].(string)

	// An update that omits inSidebar and position must reset them to the object's
	// defaults, not silently keep the old values. A client that sends a whole object
	// gets exactly what it sent; one that sends half gets the documented default.
	call(t, h.api.savedSearchSet, a.UserID, map[string]any{
		"update": map[string]any{
			id: map[string]any{"name": "pinned one", "query": "is:flagged"},
		},
	})
	got := call(t, h.api.savedSearchGet, a.UserID, map[string]any{"ids": []string{id}})
	list, _ := got["list"].([]any)
	obj, _ := list[0].(map[string]any)
	if q, _ := obj["query"].(string); q != "is:flagged" {
		t.Errorf("query = %q, want the update to have landed", q)
	}
	if p, ok := obj["position"].(float64); !ok || p != 0 {
		t.Errorf("position = %v, want 0 — an omitted field is reset, not kept", obj["position"])
	}
	if pinned, _ := obj["inSidebar"].(bool); !pinned {
		t.Error("inSidebar = false, want the documented default of true")
	}
}

func TestSavedSearchRefusalsAreSetErrorsNamingTheProperty(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// Seed one so the duplicate-name case has something to collide with.
	call(t, h.api.savedSearchSet, a.UserID, map[string]any{
		"create": map[string]any{"first": map[string]any{"name": "taken", "query": "is:unread"}},
	})

	res := call(t, h.api.savedSearchSet, a.UserID, map[string]any{
		"create": map[string]any{
			"dupe":     map[string]any{"name": "taken", "query": "is:flagged"},
			"noName":   map[string]any{"name": "   ", "query": "is:unread"},
			"noQuery":  map[string]any{"name": "empty", "query": "   "},
			"longName": map[string]any{"name": strings.Repeat("x", 200), "query": "is:unread"},
			// NEGATIVE CONTROL in the same batch: one bad create must not take down
			// the good one, which is the whole point of per-object errors.
			"good": map[string]any{"name": "fine", "query": "is:unread"},
		},
	})

	notCreated, _ := res["notCreated"].(map[string]any)
	for _, key := range []string{"dupe", "noName", "noQuery", "longName"} {
		e, ok := notCreated[key].(map[string]any)
		if !ok {
			t.Errorf("%s was not refused: %#v", key, res)
			continue
		}
		if typ, _ := e["type"].(string); typ != "invalidProperties" {
			t.Errorf("%s type = %q, want invalidProperties", key, typ)
		}
		// The PROPERTY is what lets a client point at the field the user must change.
		props, _ := e["properties"].([]any)
		if len(props) == 0 {
			t.Errorf("%s named no properties, so a client cannot say what to fix: %#v", key, e)
		}
	}
	if _, ok := res["created"].(map[string]any)["good"]; !ok {
		t.Errorf("the valid saved search in the batch was not created: %#v", res)
	}

	// A taken name specifically names `name` and nothing else — that is the field to fix.
	if e, ok := notCreated["dupe"].(map[string]any); ok {
		props, _ := e["properties"].([]any)
		if len(props) != 1 || props[0] != "name" {
			t.Errorf("a taken name should name exactly [name], got %v", props)
		}
	}
}

func TestSavedSearchChangesTracksWrites(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	before, _ := call(t, h.api.savedSearchGet, a.UserID, map[string]any{})["state"].(string)
	if before == "" {
		t.Fatal("SavedSearch/get returned no state string")
	}

	res := call(t, h.api.savedSearchSet, a.UserID, map[string]any{
		"create": map[string]any{"s1": map[string]any{"name": "new", "query": "is:unread"}},
	})
	id, _ := savedFromSet(t, res, "created", "s1")["id"].(string)

	ch := call(t, h.api.savedSearchChanges, a.UserID, map[string]any{"sinceState": before})
	if !strInArr(ch["created"], id) {
		t.Errorf("the new saved search is not in /changes created: %#v", ch)
	}
	after, _ := call(t, h.api.savedSearchGet, a.UserID, map[string]any{})["state"].(string)
	if after == before {
		t.Errorf("state did not move after a create (still %q)", before)
	}

	// A destroy is reported too — a folder deleted on one device must vanish from the
	// others' sidebars rather than lingering until a full resync.
	call(t, h.api.savedSearchSet, a.UserID, map[string]any{"destroy": []string{id}})
	ch = call(t, h.api.savedSearchChanges, a.UserID, map[string]any{"sinceState": after})
	if !strInArr(ch["destroyed"], id) {
		t.Errorf("the destroyed saved search is not in /changes destroyed: %#v", ch)
	}
	// And an unknown id is notDestroyed, not a method error.
	res = call(t, h.api.savedSearchSet, a.UserID, map[string]any{
		"destroy": []string{store.NewULID().String()},
	})
	if len(res["notDestroyed"].(map[string]any)) != 1 {
		t.Errorf("destroying an unknown id was not reported: %#v", res)
	}
}
