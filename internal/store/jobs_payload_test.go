// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

// The job-payload contract (#309 item 3), tested where it can actually be
// falsified — no database, no seeded rows, just the rule.
//
// ⚠ WHY THIS EXISTS SEPARATELY FROM THE INTEGRATION TEST. The first version of
// this coverage was only the integration walk over app.jobs. Probing it showed
// the gap: widening the allowlist to admit a "subject" key left the walk GREEN,
// because no row in the test database carried one. The walk verifies the table;
// it cannot verify the rule, and the rule is the part a future change breaks.
func TestPayloadKeysOutsideAllowlist(t *testing.T) {
	cases := []struct {
		name      string
		kind      string
		keys      []string
		wantBad   []string
		wantKnown bool
	}{
		{"submit carries its id", "submit", []string{"submission_id"}, nil, true},
		{"a kind that takes no payload", "purge_trash", nil, nil, true},

		// THE ONE THAT MATTERS: content, not a reference. This is the shape that
		// would turn app.dequeue_due_jobs into a cross-user mail conduit, because
		// the dispatcher receives whole rows for every user.
		{"submit smuggling a subject line", "submit",
			[]string{"submission_id", "subject"}, []string{"subject"}, true},
		{"a body, on a kind that should carry nothing", "snooze_wake",
			[]string{"body_text"}, []string{"body_text"}, true},
		{"day-one setup carries its run id", "setup_import", []string{"run_id"}, nil, true},
		// The setup worker re-reads the run under the target user's identity, so
		// there is never a reason for a folder name or a label to be in the
		// payload — and a folder name IS mail-adjacent content on a table the
		// dispatcher reads across users.
		{"setup smuggling a folder name", "setup_import",
			[]string{"run_id", "folder_name"}, []string{"folder_name"}, true},
		// The image prefetch (#922) re-derives each URL from the stored message;
		// a URL in the payload would put a per-recipient token on a table the
		// dispatcher reads across users.
		{"prefetch carries its id and rounds", ProxyPrefetchJobKind, []string{"message_id", "round", "rate_rounds"}, nil, true},
		{"prefetch smuggling an image url", ProxyPrefetchJobKind,
			[]string{"message_id", "url"}, []string{"url"}, true},

		// An undescribed kind is rejected wholesale rather than waved through:
		// "not in the map" must not read as "nothing to check".
		{"an undeclared kind", "exfiltrate", []string{"anything"}, []string{"anything"}, false},
		{"an undeclared kind with no payload", "brand_new", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad, known := PayloadKeysOutsideAllowlist(tc.kind, tc.keys)
			if known != tc.wantKnown {
				t.Errorf("knownKind = %v, want %v", known, tc.wantKnown)
			}
			if len(bad) != len(tc.wantBad) {
				t.Fatalf("bad = %v, want %v", bad, tc.wantBad)
			}
			for i := range bad {
				if bad[i] != tc.wantBad[i] {
					t.Errorf("bad[%d] = %q, want %q", i, bad[i], tc.wantBad[i])
				}
			}
		})
	}
}

// Every kind the allowlist describes must be one the code actually enqueues, and
// vice versa. Without this the map could drift into describing kinds that no
// longer exist while a live one goes undescribed — and an undescribed kind is
// exactly what PayloadKeysOutsideAllowlist rejects wholesale, so the drift would
// surface as a confusing runtime rejection rather than here.
func TestJobPayloadKeysCoversEveryLiveKind(t *testing.T) {
	// Mirrors the kind constants in compose, snooze, purge, ml, drawer, setup
	// changelog, outofoffice and oauthmail. They are literals here for the same reason the map is —
	// importing those packages from the store would invert the dependency
	// direction.
	live := []string{"submit", "snooze_wake", "purge_trash", "ml_train", "drawer_sweep", "setup_import", "change_log_compact", "out_of_office_sync", "oauth_reauth_notify", ProxyPrefetchJobKind}
	for _, k := range live {
		if _, ok := jobPayloadKeys[k]; !ok {
			t.Errorf("job kind %q is enqueued by the codebase but jobPayloadKeys does not "+
				"describe it — every kind must declare what its payload may carry (#309 item 3)", k)
		}
	}
	if len(jobPayloadKeys) != len(live) {
		t.Errorf("jobPayloadKeys describes %d kinds but the codebase enqueues %d. "+
			"A stale entry here is a claim about a mechanism that no longer exists.",
			len(jobPayloadKeys), len(live))
	}
}
