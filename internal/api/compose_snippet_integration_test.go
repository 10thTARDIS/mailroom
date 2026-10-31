// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// ComposeSnippet over the wire (#56, ADR-017).
//
// The store tests already pin the default-slot arithmetic. What is left to prove is the
// JMAP contract a client actually depends on:
//
//   - a create round-trips every field, including the identity binding and the flag,
//     because the client decides which picker offers a snippet from exactly those two;
//   - an UPDATE IS A FULL OBJECT and is treated as one, so a save that omits a field
//     clears it rather than silently keeping the old value. That is the deal ADR-017
//     makes with the client, and a half-honoured full-object write is the #170 bug
//     wearing a different hat;
//   - a refused write comes back as a `notCreated`/`notUpdated` SetError with the
//     offending property named, not as a method error that takes the whole request
//     down with it;
//   - every accepted write moves the state string, so other devices resync.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// seedIdentityFor gives a fixture user one concrete sending identity and returns its id,
// which is what a per-identity signature binds to.
func seedIdentityFor(t *testing.T, super *pgxpool.Pool, f apiFixture, address string) ulid.ULID {
	t.Helper()
	id := store.NewULID()
	if _, err := super.Exec(context.Background(),
		`INSERT INTO app.identities (id, user_id, account_id, kind, address, enabled)
		 VALUES ($1,$2,$3,'fixed',$4,true)`,
		id.String(), f.UserID.String(), f.AccountID.String(), address); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	return id
}

// snippetFromSet pulls the object out of a `created`/`updated` map, failing loudly with
// the whole response when it is not there — a bare nil-map panic tells you nothing.
func snippetFromSet(t *testing.T, res map[string]any, bucket, key string) map[string]any {
	t.Helper()
	b, _ := res[bucket].(map[string]any)
	obj, ok := b[key].(map[string]any)
	if !ok {
		t.Fatalf("no %s[%s] in %#v", bucket, key, res)
	}
	return obj
}

func TestComposeSnippetRoundTripsEveryField(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// A signature BOUND to an identity, defaulted. The binding is the field with real
	// consequence: get it wrong and the wrong name goes on a message.
	identityID := seedIdentityFor(t, h.super, a, "work@example.com")
	res := call(t, h.api.composeSnippetSet, a.UserID, map[string]any{
		"create": map[string]any{
			"s1": map[string]any{
				"kind":       "signature",
				"name":       "work",
				"bodyText":   "regards, me",
				"bodyHtml":   "<p>regards, me</p>",
				"identityId": identityID.String(),
				"isDefault":  true,
			},
		},
	})
	created := snippetFromSet(t, res, "created", "s1")
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %#v", created)
	}

	got := call(t, h.api.composeSnippetGet, a.UserID, map[string]any{"ids": []string{id}})
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("get by id returned %d objects, want 1: %#v", len(list), got)
	}
	obj, _ := list[0].(map[string]any)
	for _, c := range []struct{ field, want string }{
		{"kind", "signature"},
		{"name", "work"},
		{"bodyText", "regards, me"},
		{"bodyHtml", "<p>regards, me</p>"},
		{"identityId", identityID.String()},
	} {
		if s, _ := obj[c.field].(string); s != c.want {
			t.Errorf("%s = %q, want %q", c.field, s, c.want)
		}
	}
	if d, _ := obj["isDefault"].(bool); !d {
		t.Errorf("isDefault = false, want true: %#v", obj)
	}

	// A FULL-OBJECT UPDATE that omits identityId must CLEAR the binding, not keep it.
	// A client editing a signature's text and dropping the binding by accident should
	// see the binding gone — the alternative is a write whose effect depends on which
	// fields the client happened to send, which is unpredictable by construction.
	call(t, h.api.composeSnippetSet, a.UserID, map[string]any{
		"update": map[string]any{
			id: map[string]any{
				"kind":     "signature",
				"name":     "work",
				"bodyText": "cheers",
			},
		},
	})
	got = call(t, h.api.composeSnippetGet, a.UserID, map[string]any{"ids": []string{id}})
	list, _ = got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("get after update returned %d objects: %#v", len(list), got)
	}
	obj, _ = list[0].(map[string]any)
	if obj["identityId"] != nil {
		t.Errorf("identityId = %v, want null — a full-object update did not clear it", obj["identityId"])
	}
	if s, _ := obj["bodyHtml"].(string); s != "" {
		t.Errorf("bodyHtml = %q, want cleared by the full-object update", s)
	}
	if d, _ := obj["isDefault"].(bool); d {
		t.Errorf("isDefault stayed true through an update that omitted it")
	}
	if s, _ := obj["bodyText"].(string); s != "cheers" {
		t.Errorf("bodyText = %q, want %q", s, "cheers")
	}
}

func TestComposeSnippetSetRefusalsAreSetErrors(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	identityID := seedIdentityFor(t, h.super, a, "work@example.com")

	// Each of these is a per-object refusal, so the request as a whole must SUCCEED and
	// report the failure in notCreated. A method error here would mean one bad snippet
	// in a batch took down the good ones with it.
	res := call(t, h.api.composeSnippetSet, a.UserID, map[string]any{
		"create": map[string]any{
			// A template bound to an address: a template is a message you start from,
			// not a way you sign, so the binding has no meaning (ADR-017).
			"bad1": map[string]any{
				"kind": "template", "name": "t", "bodyText": "x",
				"identityId": identityID.String(),
			},
			"bad2": map[string]any{"kind": "signature", "name": "  ", "bodyText": "x"},
			"bad3": map[string]any{"kind": "signature", "name": "empty"},
			"bad4": map[string]any{"kind": "footer", "name": "f", "bodyText": "x"},
			// NEGATIVE CONTROL in the same batch: the good one must still be created,
			// which is the whole point of per-object errors.
			"good": map[string]any{"kind": "template", "name": "ok", "bodyText": "x"},
		},
	})
	notCreated, _ := res["notCreated"].(map[string]any)
	for _, key := range []string{"bad1", "bad2", "bad3", "bad4"} {
		e, ok := notCreated[key].(map[string]any)
		if !ok {
			t.Errorf("%s was not refused: %#v", key, res)
			continue
		}
		if typ, _ := e["type"].(string); typ != "invalidProperties" {
			t.Errorf("%s error type = %q, want invalidProperties", key, typ)
		}
	}
	if _, ok := res["created"].(map[string]any)["good"]; !ok {
		t.Errorf("the valid snippet in the batch was not created: %#v", res)
	}

	// An unknown id is notUpdated / notDestroyed, again not a method error.
	unknown := store.NewULID().String()
	res = call(t, h.api.composeSnippetSet, a.UserID, map[string]any{
		"update":  map[string]any{unknown: map[string]any{"kind": "signature", "name": "n", "bodyText": "x"}},
		"destroy": []string{unknown},
	})
	if _, ok := res["notUpdated"].(map[string]any)[unknown]; !ok {
		t.Errorf("updating an unknown id was not reported: %#v", res)
	}
	if _, ok := res["notDestroyed"].(map[string]any)[unknown]; !ok {
		t.Errorf("destroying an unknown id was not reported: %#v", res)
	}
}

func TestComposeSnippetChangesTracksEveryWrite(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// The state before anything, from /get — the string a client would have cached.
	before, _ := call(t, h.api.composeSnippetGet, a.UserID, map[string]any{})["state"].(string)
	if before == "" {
		t.Fatal("ComposeSnippet/get returned no state string")
	}

	res := call(t, h.api.composeSnippetSet, a.UserID, map[string]any{
		"create": map[string]any{"s1": map[string]any{
			"kind": "signature", "name": "new", "bodyText": "x"}},
	})
	id, _ := snippetFromSet(t, res, "created", "s1")["id"].(string)

	ch := call(t, h.api.composeSnippetChanges, a.UserID, map[string]any{"sinceState": before})
	if !strInArr(ch["created"], id) {
		t.Errorf("the new snippet is not in /changes created: %#v", ch)
	}

	// And the state moved, or a client that only compares states never refetches.
	after, _ := call(t, h.api.composeSnippetGet, a.UserID, map[string]any{})["state"].(string)
	if after == before {
		t.Errorf("state did not move after a create (still %q)", before)
	}

	// A destroy is reported too — a snippet that vanished without a change row would
	// stay in every other device's picker until a full resync.
	call(t, h.api.composeSnippetSet, a.UserID, map[string]any{"destroy": []string{id}})
	ch = call(t, h.api.composeSnippetChanges, a.UserID, map[string]any{"sinceState": after})
	if !strInArr(ch["destroyed"], id) {
		t.Errorf("the destroyed snippet is not in /changes destroyed: %#v", ch)
	}
}

// A DUPLICATE NAME MUST NOT POISON THE BATCH — a defect shipped in v1.15.0 and found while
// building #57's saved searches, which have the same shape.
//
// UNIQUE(user_id, kind, name) made a duplicate name a database error, and a failed
// statement aborts the whole surrounding transaction (SQLSTATE 25P02). So the per-object
// error handling above it was decoration: one duplicate name came back as `serverFail` for
// the entire request and took every other create with it. The fix is a SAVEPOINT around the
// INSERT and the UPDATE (store/compose_snippets.go), the same pattern CreateAccount had already
// documented.
func TestComposeSnippetDuplicateNameDoesNotPoisonTheBatch(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	call(t, h.api.composeSnippetSet, a.UserID, map[string]any{
		"create": map[string]any{"first": map[string]any{
			"kind": "signature", "name": "taken", "bodyText": "x"}},
	})

	res := call(t, h.api.composeSnippetSet, a.UserID, map[string]any{
		"create": map[string]any{
			"dupe": map[string]any{"kind": "signature", "name": "taken", "bodyText": "y"},
			// The NEGATIVE CONTROL, and the whole point: this one must survive.
			"good": map[string]any{"kind": "signature", "name": "fine", "bodyText": "z"},
		},
	})

	if _, ok := res["created"].(map[string]any)["good"]; !ok {
		t.Errorf("a duplicate name lost the good create too: %#v", res)
	}
	e, ok := res["notCreated"].(map[string]any)["dupe"].(map[string]any)
	if !ok {
		t.Fatalf("the duplicate was not refused per-object: %#v", res)
	}
	if typ, _ := e["type"].(string); typ != "invalidProperties" {
		t.Errorf("type = %q, want invalidProperties", typ)
	}
	// `name` is what the user has to change, so it has to be named.
	props, _ := e["properties"].([]any)
	if len(props) != 1 || props[0] != "name" {
		t.Errorf("properties = %v, want exactly [name]", props)
	}

	// And a RENAME onto a taken name is the same hazard on the update path.
	res = call(t, h.api.composeSnippetSet, a.UserID, map[string]any{
		"create": map[string]any{"third": map[string]any{
			"kind": "signature", "name": "third", "bodyText": "w"}},
	})
	thirdID, _ := snippetFromSet(t, res, "created", "third")["id"].(string)
	res = call(t, h.api.composeSnippetSet, a.UserID, map[string]any{
		"update": map[string]any{
			thirdID: map[string]any{"kind": "signature", "name": "taken", "bodyText": "w"},
		},
	})
	if _, ok := res["notUpdated"].(map[string]any)[thirdID]; !ok {
		t.Errorf("renaming onto a taken name was not refused per-object: %#v", res)
	}
}
