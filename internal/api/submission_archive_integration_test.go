// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// EmailSubmission/set create's `archiveOnSuccess` (#648): recorded on the
// submission and echoed, so a client can tell this server understood it. The
// archive itself — only once SENT — is internal/compose's
// send_archive_integration_test.go; this is the request surface.

import (
	"testing"

	"mailroom/internal/store"
)

func TestSubmissionRecordsAndEchoesArchiveOnSuccess(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	draft := draftFor(t, h, a, "me@example.com", "send and archive")
	parent := store.NewULID().String()

	res := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{
			// The duplicate collapses; the stored list is the distinct ids.
			"s": map[string]any{"emailId": draft, "archiveOnSuccess": []string{parent, parent}},
		},
	})
	obj, ok := res["created"].(map[string]any)["s"].(map[string]any)
	if !ok {
		t.Fatalf("submit refused: %#v", res)
	}
	echo, _ := obj["archiveOnSuccess"].([]any)
	if len(echo) != 1 || echo[0] != parent {
		t.Errorf("created.archiveOnSuccess = %#v, want [%s] — a client reads this to know "+
			"the server will archive once sent", obj["archiveOnSuccess"], parent)
	}
	if got := sendCount(t, h, `SELECT count(*) FROM app.submissions
		 WHERE user_id = $1 AND archive_on_success = ARRAY[$2]::app.ulid[]`,
		a.UserID.String(), parent); got != 1 {
		t.Errorf("submissions recording archive_on_success = [%s]: %d, want 1", parent, got)
	}

	// The control: a plain send echoes null and stores NULL.
	draft2 := draftFor(t, h, a, "me@example.com", "plain send")
	res2 := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{"s": map[string]any{"emailId": draft2}},
	})
	obj2, ok := res2["created"].(map[string]any)["s"].(map[string]any)
	if !ok {
		t.Fatalf("plain submit refused: %#v", res2)
	}
	if v, present := obj2["archiveOnSuccess"]; !present || v != nil {
		t.Errorf("plain send archiveOnSuccess = %#v (present %v), want null", v, present)
	}
	if got := sendCount(t, h, `SELECT count(*) FROM app.submissions
		 WHERE user_id = $1 AND archive_on_success IS NULL`, a.UserID.String()); got != 1 {
		t.Errorf("submissions with NULL archive_on_success = %d, want 1 (the plain send)", got)
	}
}

// A malformed id is refused — before anything durable exists — rather than
// silently dropped: a Send & archive that quietly archives nothing is the
// failure this whole change is about.
func TestSubmissionRefusesAMalformedArchiveOnSuccess(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	draft := draftFor(t, h, a, "me@example.com", "bad archive id")

	res := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{
			"s": map[string]any{"emailId": draft, "archiveOnSuccess": []string{"not-an-id"}},
		},
	})
	nc, _ := res["notCreated"].(map[string]any)
	se, ok := nc["s"].(map[string]any)
	if !ok || se["type"] != setInvalidProps {
		t.Fatalf("want notCreated.s of type %s, got %#v", setInvalidProps, res)
	}
	if got := sendCount(t, h, `SELECT count(*) FROM app.submissions WHERE user_id = $1`,
		a.UserID.String()); got != 0 {
		t.Errorf("submissions = %d after a refused create, want 0", got)
	}
}
