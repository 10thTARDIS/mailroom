// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/outofoffice"
	"mailroom/internal/store"
)

// stubOOF answers Set with a fixed error, so the API's mapping of each kind
// of failure to a SetError is pinned without a ManageSieve server.
type stubOOF struct {
	err error
	got outofoffice.Input
}

func (s *stubOOF) List(context.Context, ulid.ULID) ([]outofoffice.View, error) { return nil, nil }
func (s *stubOOF) Get(context.Context, ulid.ULID, ulid.ULID) (outofoffice.View, error) {
	return outofoffice.View{}, nil
}
func (s *stubOOF) Check(context.Context, ulid.ULID, ulid.ULID) (outofoffice.View, error) {
	return outofoffice.View{}, nil
}
func (s *stubOOF) Set(_ context.Context, _ ulid.ULID, id ulid.ULID, in outofoffice.Input) (outofoffice.View, error) {
	s.got = in
	if s.err != nil {
		return outofoffice.View{}, s.err
	}
	return outofoffice.View{OutOfOffice: store.OutOfOffice{AccountID: id, Enabled: in.Enabled, ReplyDays: in.ReplyDays}}, nil
}

func TestOutOfOfficeSetMapsEveryFailure(t *testing.T) {
	h := newAPIHarness(t)
	f := seedAPIUser(t, h.super, "oofapi-"+store.NewULID().String())
	full := map[string]any{
		"isEnabled": true, "fromDate": "2026-11-20T00:00:00Z", "toDate": nil, "subject": "Away",
		"textBody": "Back soon", "replyDays": 3, "fromAddress": "", "contactsOnly": false,
	}
	call := func(stub *stubOOF, patch map[string]any) map[string]any {
		t.Helper()
		h.api.outOfOffice = stub
		raw, mErr := h.api.outOfOfficeSet(context.Background(), f.UserID, mustJSON(t, map[string]any{
			"update": map[string]any{f.AccountID.String(): patch},
		}))
		if mErr != nil {
			t.Fatalf("OutOfOffice/set: %+v", mErr)
		}
		var res map[string]any
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	notUpdated := func(res map[string]any) map[string]any {
		nu, _ := res["notUpdated"].(map[string]any)
		e, _ := nu[f.AccountID.String()].(map[string]any)
		return e
	}

	cases := []struct {
		name     string
		err      error
		wantType string
		wantText string
	}{
		{"a foreign active script", &outofoffice.ForeignActiveError{Script: "webmail"}, "forbidden", `"webmail" is active`},
		{"an invalid form", fmt.Errorf("%w: the last day has already passed", outofoffice.ErrInvalid), "invalidProperties", "already passed"},
		{"too large", fmt.Errorf("%w: 50000 bytes", outofoffice.ErrTooLarge), "overQuota", "50000 bytes"},
		{"no contacts", outofoffice.ErrNoContacts, "invalidProperties", "nobody you have written to"},
		{"the provider unreachable", fmt.Errorf("dial: connection refused"), "forbidden", "nothing was saved"},
		{"not this user's account", store.ErrAccountNotFound, "notFound", ""},
	}
	for _, c := range cases {
		e := notUpdated(call(&stubOOF{err: c.err}, full))
		if e == nil || e["type"] != c.wantType || !strings.Contains(fmt.Sprint(e["description"]), c.wantText) {
			t.Errorf("%s: notUpdated = %v, want type %s containing %q", c.name, e, c.wantType, c.wantText)
		}
	}
	// The refusal is the user's explanation and is passed through WORD FOR
	// WORD, not wrapped in "the mail server did not accept…": the server did
	// accept the question, and the answer is the sentence naming the script.
	refusal := &outofoffice.ForeignActiveError{Script: "webmail"}
	if e := notUpdated(call(&stubOOF{err: refusal}, full)); e["description"] != refusal.Error() {
		t.Errorf("refusal description = %q, want exactly %q", e["description"], refusal.Error())
	}

	// A partial object is refused before the service is asked.
	stub := &stubOOF{}
	partial := map[string]any{"isEnabled": true}
	if e := notUpdated(call(stub, partial)); e == nil || e["type"] != "invalidProperties" {
		t.Errorf("partial update: %v", e)
	}
	// A whole one reaches the service intact.
	res := call(stub, full)
	if up, _ := res["updated"].(map[string]any); up[f.AccountID.String()] == nil {
		t.Fatalf("whole update not applied: %v", res)
	}
	if !stub.got.Enabled || stub.got.From == nil || stub.got.From.Format("2006-01-02") != "2026-11-20" || stub.got.To != nil || stub.got.ReplyDays != 3 {
		t.Fatalf("service got %+v", stub.got)
	}
}
