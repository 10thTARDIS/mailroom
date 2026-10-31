// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Account presentation surface (mailroom#26, spec §12): the per-account `color`
// is readable and settable through Account/get and Account/set, an invalid color
// is a clean per-object SetError (never a 500/panic), and the provider's static
// `outboundDailyQuota` cap is exposed READ-ONLY — settable attempts are rejected.
// Runs against real Postgres + RLS via the account-management API seam.

import (
	"testing"
)

func TestAccountColorAndReadOnlyQuota(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	api := newAccountAPI(t, h.store)

	// Create with a color: the created object echoes it and carries the read-only
	// outboundDailyQuota (the schema default of 100).
	res := call(t, api.accountSet, a.UserID, map[string]any{
		"create": map[string]any{
			"c1": map[string]any{
				"name":     "colored",
				"imapHost": "imap.example.com",
				"smtpHost": "smtp.example.com",
				"username": "you@example.com",
				"password": "pw",
				"color":    "#FF8800",
			},
		},
	})
	obj, ok := res["created"].(map[string]any)["c1"].(map[string]any)
	if !ok {
		t.Fatalf("account not created: %v", res)
	}
	if obj["color"] != "#FF8800" {
		t.Errorf("created color = %v, want #FF8800", obj["color"])
	}
	if obj["outboundDailyQuota"] != float64(100) {
		t.Errorf("created outboundDailyQuota = %v, want 100 (read-only default)", obj["outboundDailyQuota"])
	}
	id := obj["id"].(string)

	getColor := func() any {
		got := call(t, api.accountGet, a.UserID, map[string]any{"ids": []string{id}})
		return got["list"].([]any)[0].(map[string]any)["color"]
	}

	// Account/get returns the color and the quota.
	got := call(t, api.accountGet, a.UserID, map[string]any{"ids": []string{id}})
	gotObj := got["list"].([]any)[0].(map[string]any)
	if gotObj["color"] != "#FF8800" {
		t.Errorf("get color = %v, want #FF8800", gotObj["color"])
	}
	if gotObj["outboundDailyQuota"] != float64(100) {
		t.Errorf("get outboundDailyQuota = %v, want 100", gotObj["outboundDailyQuota"])
	}

	// Patch the color; get reflects the new value.
	res = call(t, api.accountSet, a.UserID, map[string]any{
		"update": map[string]any{id: map[string]any{"color": "#00AAFF"}},
	})
	if _, ok := res["updated"].(map[string]any)[id]; !ok {
		t.Fatalf("color patch not applied: %v", res)
	}
	if c := getColor(); c != "#00AAFF" {
		t.Errorf("after patch color = %v, want #00AAFF", c)
	}

	// null clears the color to SQL NULL.
	call(t, api.accountSet, a.UserID, map[string]any{
		"update": map[string]any{id: map[string]any{"color": nil}},
	})
	if c := getColor(); c != nil {
		t.Errorf("after null patch color = %v, want nil", c)
	}

	// An invalid color is a clean per-object SetError, not a panic/500, and the
	// account is left unchanged.
	res = call(t, api.accountSet, a.UserID, map[string]any{
		"update": map[string]any{id: map[string]any{"color": "red"}},
	})
	se, ok := res["notUpdated"].(map[string]any)[id].(map[string]any)
	if !ok {
		t.Fatalf("invalid color should be notUpdated: %v", res)
	}
	if se["type"] != setInvalidProps {
		t.Errorf("invalid color error type = %v, want %s", se["type"], setInvalidProps)
	}
	if c := getColor(); c != nil {
		t.Errorf("invalid color patch mutated the account: color = %v, want nil", c)
	}

	// outboundDailyQuota is READ-ONLY: an attempt to set it is rejected per-object.
	res = call(t, api.accountSet, a.UserID, map[string]any{
		"update": map[string]any{id: map[string]any{"outboundDailyQuota": 5}},
	})
	if _, ok := res["notUpdated"].(map[string]any)[id]; !ok {
		t.Errorf("outboundDailyQuota must not be settable, got %v", res)
	}
}
