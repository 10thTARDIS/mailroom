// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// #59 through the JMAP surface: Contact/set and the Contact object's markers,
// Contact/query's exact-address and marker filters, `is:vip` in Email/query,
// WebPushSubscription's notifyMode, DeskPreferences' quietHours and
// Account.notifyPush — including what each refuses.

import (
	"testing"

	"mailroom/internal/store"
)

func contactIDByAddress(t *testing.T, h *apiHarness, f apiFixture, addr string) string {
	t.Helper()
	res := call(t, h.api.contactQuery, f.UserID, map[string]any{"filter": map[string]any{"address": addr}})
	ids := asArr(res["ids"])
	if len(ids) != 1 {
		t.Fatalf("Contact/query address=%s: %v, want one id", addr, res)
	}
	return ids[0].(string)
}

func TestContactSetMarkersAndFilters(t *testing.T) {
	h := newAPIHarness(t)
	a := seedAPIUser(t, h.super, "senders-"+store.NewULID().String())
	for _, addr := range []string{"boss@example.com", "pest@example.com", "big.boss@example.com"} {
		c := store.NewULID().String()
		mustExecAPI(t, h, `INSERT INTO app.contacts (id, user_id) VALUES ($1, $2)`, c, a.UserID.String())
		mustExecAPI(t, h, `INSERT INTO app.contact_addresses (id, user_id, contact_id, address) VALUES ($1,$2,$3,$4)`,
			store.NewULID().String(), a.UserID.String(), c, addr)
	}
	// address is EXACT: boss@ must not also find big.boss@ (a substring would).
	boss := contactIDByAddress(t, h, a, "BOSS@example.com")
	pest := contactIDByAddress(t, h, a, "pest@example.com")

	res := call(t, h.api.contactSet, a.UserID, map[string]any{"update": map[string]any{
		boss: map[string]any{"isVip": true},
		pest: map[string]any{"isBlocked": true, "blockDestination": "trash"},
	}})
	if len(asMap(res["updated"])) != 2 || res["oldState"] == res["newState"] {
		t.Fatalf("Contact/set: %v", res)
	}

	got := call(t, h.api.contactGet, a.UserID, map[string]any{"ids": []string{boss, pest}})
	byID := map[string]map[string]any{}
	for _, o := range asArr(got["list"]) {
		m := o.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if byID[boss]["isVip"] != true || byID[boss]["isBlocked"] != false || byID[boss]["blockedAt"] != nil {
		t.Errorf("boss: %v", byID[boss])
	}
	if byID[pest]["isBlocked"] != true || byID[pest]["blockDestination"] != "trash" || byID[pest]["blockedAt"] == nil {
		t.Errorf("pest: %v", byID[pest])
	}

	for filter, want := range map[string]string{"isVip": boss, "isBlocked": pest} {
		res := call(t, h.api.contactQuery, a.UserID, map[string]any{"filter": map[string]any{filter: true}})
		ids := asArr(res["ids"])
		if len(ids) != 1 || ids[0] != want || res["total"] != float64(1) {
			t.Errorf("Contact/query %s:true = %v, want [%s]", filter, res, want)
		}
	}

	// Refusals: by name, never silently.
	res = call(t, h.api.contactSet, a.UserID, map[string]any{
		"update":  map[string]any{boss: map[string]any{"displayName": "Renamed"}, pest: map[string]any{"blockDestination": "spam"}},
		"create":  map[string]any{"c1": map[string]any{"isVip": true}},
		"destroy": []string{boss},
	})
	for _, bucket := range []struct{ name, key string }{{"notUpdated", boss}, {"notUpdated", pest}, {"notCreated", "c1"}, {"notDestroyed", boss}} {
		if !mapHasKey(res, bucket.name, bucket.key) {
			t.Errorf("Contact/set did not refuse %s in %s: %v", bucket.key, bucket.name, res)
		}
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func TestSearchIsVIP(t *testing.T) {
	x := newOpsFixture(t)
	mustExecAPI(t, x.h, `UPDATE app.messages SET from_address = 'boss@example.com' WHERE id = $1`, x.ids["pinned"].String())
	vip := func(f apiFixture, addr string) {
		c := store.NewULID().String()
		mustExecAPI(t, x.h, `INSERT INTO app.contacts (id, user_id, vip) VALUES ($1, $2, true)`, c, f.UserID.String())
		mustExecAPI(t, x.h, `INSERT INTO app.contact_addresses (id, user_id, contact_id, address) VALUES ($1,$2,$3,$4)`,
			store.NewULID().String(), f.UserID.String(), c, addr)
	}
	vip(x.a, "boss@example.com")
	// The OTHER user marks the address all of a's other mail comes from as VIP;
	// a's search must not see b's marker.
	vip(x.b, "friend@example.com")

	x.expect(t, "is:vip", "pinned")
	x.expect(t, "invoice -is:vip", without(everyday, "pinned")...)
	x.expect(t, "is:vip OR is:muted", "pinned", "muted")
}

func TestPushModeUpdateAndValidation(t *testing.T) {
	h := newAPIHarness(t)
	a := seedAPIUser(t, h.super, "pushmode-"+store.NewULID().String())
	res := call(t, h.api.pushSet, a.UserID, map[string]any{"create": map[string]any{"d": map[string]any{
		"endpoint": "https://push.example.com/d", "keys": map[string]any{"p256dh": "p", "auth": "a"},
	}}})
	id := asMap(asMap(res["created"])["d"])["id"].(string)

	res = call(t, h.api.pushSet, a.UserID, map[string]any{"update": map[string]any{id: map[string]any{"notifyMode": "above_line_or_vip"}}})
	if !mapHasKey(res, "updated", id) {
		t.Fatalf("mode update refused: %v", res)
	}
	got := call(t, h.api.pushGet, a.UserID, map[string]any{})
	if m := asMap(asArr(got["list"])[0])["notifyMode"]; m != "above_line_or_vip" {
		t.Errorf("notifyMode = %v after update", m)
	}
	for _, bad := range []map[string]any{{"notifyMode": "loud"}, {"deviceName": "x"}} {
		res = call(t, h.api.pushSet, a.UserID, map[string]any{"update": map[string]any{id: bad}})
		if !mapHasKey(res, "notUpdated", id) {
			t.Errorf("update %v was not refused: %v", bad, res)
		}
	}
	// Re-registering the same endpoint (what "turn on" does) keeps the mode.
	call(t, h.api.pushSet, a.UserID, map[string]any{"create": map[string]any{"d": map[string]any{
		"endpoint": "https://push.example.com/d", "keys": map[string]any{"p256dh": "p2", "auth": "a2"},
	}}})
	got = call(t, h.api.pushGet, a.UserID, map[string]any{})
	if m := asMap(asArr(got["list"])[0])["notifyMode"]; m != "above_line_or_vip" {
		t.Errorf("re-registering reset notifyMode to %v", m)
	}
}

func TestDeskPreferencesQuietHours(t *testing.T) {
	h := newAPIHarness(t)
	a := seedAPIUser(t, h.super, "quiet-"+store.NewULID().String())
	id := a.UserID.String()
	got := call(t, h.api.deskPreferencesGet, a.UserID, map[string]any{})
	q := asMap(asMap(asArr(got["list"])[0])["quietHours"])
	if q["enabled"] != false || q["allowVip"] != true || q["start"] != float64(1320) || q["end"] != float64(420) {
		t.Fatalf("default quietHours = %v (want off, 22:00-07:00, VIPs allowed)", q)
	}
	good := map[string]any{"enabled": true, "start": 1380, "end": 360, "days": []int{1, 2, 3, 4, 5},
		"timeZone": "Europe/London", "allowVip": false}
	res := call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{"update": map[string]any{id: map[string]any{"quietHours": good}}})
	if !mapHasKey(res, "updated", id) {
		t.Fatalf("quietHours update refused: %v", res)
	}
	got = call(t, h.api.deskPreferencesGet, a.UserID, map[string]any{})
	q = asMap(asMap(asArr(got["list"])[0])["quietHours"])
	if q["timeZone"] != "Europe/London" || len(asArr(q["days"])) != 5 || q["allowVip"] != false {
		t.Errorf("quietHours read back %v", q)
	}
	with := func(k string, v any) map[string]any {
		out := map[string]any{}
		for kk, vv := range good {
			out[kk] = vv
		}
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
		return out
	}
	for name, bad := range map[string]map[string]any{
		"zone":     with("timeZone", "Mars/Olympus"),
		"start":    with("start", 1440),
		"same":     with("end", 1380),
		"day":      with("days", []int{7}),
		"missing":  with("allowVip", nil),
		"negative": with("end", -1),
	} {
		res := call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{"update": map[string]any{id: map[string]any{"quietHours": bad}}})
		if !mapHasKey(res, "notUpdated", id) {
			t.Errorf("%s: invalid quietHours accepted: %v", name, bad)
		}
	}
}

func TestAccountNotifyPushProperty(t *testing.T) {
	h := newAPIHarness(t)
	a := seedAPIUser(t, h.super, "acctpush-"+store.NewULID().String())
	acct := a.AccountID.String()
	got := call(t, h.api.accountGet, a.UserID, map[string]any{"ids": []string{acct}})
	if asMap(asArr(got["list"])[0])["notifyPush"] != true {
		t.Fatalf("notifyPush default: %v", got)
	}
	res := call(t, h.api.accountSet, a.UserID, map[string]any{"update": map[string]any{acct: map[string]any{"notifyPush": false}}})
	if !mapHasKey(res, "updated", acct) {
		t.Fatalf("notifyPush update refused: %v", res)
	}
	got = call(t, h.api.accountGet, a.UserID, map[string]any{"ids": []string{acct}})
	if asMap(asArr(got["list"])[0])["notifyPush"] != false {
		t.Errorf("notifyPush after update: %v", got)
	}
	res = call(t, h.api.accountSet, a.UserID, map[string]any{"update": map[string]any{acct: map[string]any{"notifyPush": "no"}}})
	if !mapHasKey(res, "notUpdated", acct) {
		t.Errorf("a non-boolean notifyPush was accepted: %v", res)
	}
}
