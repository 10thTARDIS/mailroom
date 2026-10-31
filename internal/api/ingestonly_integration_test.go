// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// ADR-028's ingest-only mode where a CLIENT meets it: Account/set,
// Account/getHealth and EmailSubmission/set (#521, and PR #553 review finding
// S6 — several of the mechanisms below could be deleted with CI staying green).
//
// internal/writeback and internal/purge cover the engine and the reaper. What
// had no coverage at all is the API layer: that Account/set actually CALLS the
// backlog suppression, that it refuses to enable writeback against the
// instance-level flag on BOTH the update and the create door, that getHealth
// reports the EFFECTIVE value rather than the column, and that the submission
// refusal — which was moved in front of the first durable side effect — really
// does leave nothing behind.
//
// ⚠ EVERY NEGATIVE ASSERTION HERE HAS A CONTROL, and that is not ceremony. The
// whole shape of this feature is "nothing happened", which is also exactly what
// a fixture that never got started produces (#131). So each "no row / no change
// / unchanged bytes" assertion below is paired with an identical run in which
// the same assertion must come out the other way.
//
// # The probe, and what it caught
//
// The suite passed on the first run, so each mechanism was deleted in turn and
// the failing LINE was read (CLAUDE.md). A compile error was never accepted as
// "went red" — every probe was shaped to keep the tree building.
//
//	mechanism deleted                                  | red test, and where
//	---------------------------------------------------|--------------------
//	account.go: tx.SuppressAccountIntents on the        | ...SwitchingWritebackOff
//	  writebackEnabled:false update                     |   SuppressesTheBacklog, at
//	                                                    |   "drainable intents = 5, want 0"
//	account.go prepareAccountUpdate: the instance-flag  | ...UpdateRefusesWritebackTrue
//	  refusal                                           |   UnderTheInstanceFlag, at
//	                                                    |   "the update was accepted while
//	                                                    |   MAILROOM_WRITEBACK_DISABLED holds"
//	account.go prepareAccountCreate: the same refusal   | ...CreateRefusesWritebackTrue
//	  on the create door (a SEPARATE path — deleting    |   UnderTheInstanceFlag, at
//	  one left the other's test green, both ways round) |   "an account was created with
//	                                                    |   writebackEnabled:true"
//	accountCreate.WritebackEnabled *bool -> bool        | ...CreateWritebackEnabledIsA
//	                                                    |   PointerNotABool, at the ABSENT-
//	                                                    |   property case only ("stored
//	                                                    |   writeback_enabled = false, want
//	                                                    |   true"); the explicit-false case
//	                                                    |   stayed green, which is the shape
//	account_health.go: `&& !a.store.WritebackDisabled()`| ...GetHealthReportsEffective
//	                                                    |   Writeback, ONLY the
//	                                                    |   column-true+flag-set subtest
//	account_health.go: suppressed folded into the       | the same test, all four subtests,
//	  deadLetters count (the conflation the type's doc  |   at "deadLetters.count = 4, want
//	  comment warns about)                              |   exactly 1"
//	submission.go: the whole ingest-only gate           | all four submission tests; the
//	                                                    |   writeback-enabled control green
//	submission.go: only `|| a.store.WritebackDisabled()`| ...RefusedWhenOnlyTheInstanceFlag
//	                                                    |   IsSet, and ONLY that one
//	submission.go: the gate moved BACK to after         | see below — the important one
//	  SubmitPlanned, with a cancel (the pre-fix shape)  |
//
// ⚠ THE LAST PROBE IS WHY THIS FILE ASSERTS WHAT IT DOES. Restoring the pre-fix
// ordering produced, for a refused send: one app.submissions row in state
// `canceled`, one surviving submit job, a MOVED content_hash, a replaced
// app.blobs row, a superseded_content row, and an Email `updated` entry in
// /changes. The `notCreated.type == "forbidden"` assertion stayed GREEN
// throughout — so a test that checked only the error type, or only that the
// submission was "not scheduled", would have passed against the bug. It also
// measured the thing the fixture comment claims: the bytes really were
// rewritten, so the "unchanged bytes" assertions are not vacuous.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// --- shared helpers ----------------------------------------------------------

// seedIntent writes one writeback intent directly, in the state named.
//
// It deliberately bypasses EnqueueIntent: what these tests need is a BACKLOG
// that already exists in a given state, which is precisely the case an
// enqueue-time guard cannot produce.
func (h *apiHarness) seedIntent(t *testing.T, f apiFixture, op, state, deadReason string) string {
	t.Helper()
	id := store.NewULID().String()
	var reason any
	if deadReason != "" {
		reason = deadReason
	}
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.writeback_intents
		   (id, user_id, message_id, account_id, op, payload, idempotency_key, state, dead_reason)
		 VALUES ($1,$2,$3,$4,$5,'{}'::jsonb,$6,$7::app.intent_state,$8)`,
		id, f.UserID.String(), f.MessageID.String(), f.AccountID.String(), op,
		"seed-"+id, state, reason); err != nil {
		t.Fatalf("seed %s intent: %v", state, err)
	}
	return id
}

// intentStates reports the account's intents grouped BY STATE, read as the
// superuser.
//
// ⚠ PER STATE, NEVER A TOTAL. "7 intents exist" cannot tell a ledger from a
// live queue, and which state the rows are in is the entire difference between
// this mode working and this mode being a silent no-op.
func (h *apiHarness) intentStates(t *testing.T, f apiFixture) map[string]int {
	t.Helper()
	rows, err := h.super.Query(context.Background(),
		`SELECT state::text, count(*) FROM app.writeback_intents
		  WHERE user_id = $1 AND account_id = $2 GROUP BY state`,
		f.UserID.String(), f.AccountID.String())
	if err != nil {
		t.Fatalf("count intents by state: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatalf("scan intent state: %v", err)
		}
		out[s] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count intents by state: %v", err)
	}
	return out
}

// writebackColumn reads app.accounts.writeback_enabled as the superuser —
// ground truth, not the API's own rendering of it.
func (h *apiHarness) writebackColumn(t *testing.T, accountID string) bool {
	t.Helper()
	var v bool
	if err := h.super.QueryRow(context.Background(),
		`SELECT writeback_enabled FROM app.accounts WHERE id = $1`, accountID).Scan(&v); err != nil {
		t.Fatalf("read writeback_enabled: %v", err)
	}
	return v
}

// setWritebackColumn flips the per-account switch directly, for the cases that
// need a starting state rather than to exercise Account/set.
func (h *apiHarness) setWritebackColumn(t *testing.T, accountID string, enabled bool) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.accounts SET writeback_enabled = $2 WHERE id = $1`, accountID, enabled); err != nil {
		t.Fatalf("set writeback_enabled=%v: %v", enabled, err)
	}
}

// seedSecondAccount adds a SECOND mail account to the same user, so a single
// /set batch can span one shadowed and one live account.
func seedSecondAccount(t *testing.T, super *pgxpool.Pool, f apiFixture, name string) apiFixture {
	t.Helper()
	g := f
	g.AccountID = store.NewULID()
	if _, err := super.Exec(context.Background(),
		`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		 VALUES ($1,$2,$3,'mail.example.com','smtp.example.com',$4,$5)`,
		g.AccountID.String(), g.UserID.String(), name, []byte{0x02},
		store.NewULID().String()); err != nil {
		t.Fatalf("seed second account: %v", err)
	}
	return g
}

// ============================================================================
// A. Account/set
// ============================================================================

// TestAccountSetSwitchingWritebackOffSuppressesTheBacklog is ADR-028 §4 at the
// API: the property that makes the enqueue-time guard into an actual mode.
//
// The store method has direct coverage in internal/store; nothing covered the
// API CALLING it, so `tx.SuppressAccountIntents(...)` could be deleted from
// account.go with the whole suite green — and the symptom would be an account
// shadowed in name only, whose pre-existing backlog drains on the very next
// writeback pass.
//
// The applied/dead rows are the control on the other side: a handler that
// simply set every intent to 'suppressed' would satisfy a pending-only
// assertion just as well, and would be destroying the dead-letter record the
// health panel reads.
func TestAccountSetSwitchingWritebackOffSuppressesTheBacklog(t *testing.T) {
	h := newAPIHarness(t)
	api := newAccountAPI(t, h.store)
	f := seedAPIUser(t, h.super, "wbset-off-"+store.NewULID().String())

	for _, op := range []string{"+flag", "-flag", "move", "expunge"} {
		h.seedIntent(t, f, op, "pending", "")
	}
	h.seedIntent(t, f, "+kw", "inflight", "")
	h.seedIntent(t, f, "move", "applied", "")
	h.seedIntent(t, f, "expunge", "dead", "the archive role is missing")

	before := h.intentStates(t, f)
	if before["pending"] != 4 || before["inflight"] != 1 {
		t.Fatalf("setup: states = %v, want 4 pending + 1 inflight — the rest of this test proves nothing", before)
	}
	if !h.writebackColumn(t, f.AccountID.String()) {
		t.Fatal("setup: the account starts shadowed, so there is no transition to observe")
	}

	res := call(t, api.accountSet, f.UserID, map[string]any{
		"update": map[string]any{
			f.AccountID.String(): map[string]any{"writebackEnabled": false},
		},
	})
	if !mapHasKey(res, "updated", f.AccountID.String()) {
		t.Fatalf("the account was not updated: %v", res)
	}
	if h.writebackColumn(t, f.AccountID.String()) {
		t.Error("writeback_enabled is still true after Account/set writebackEnabled:false")
	}

	after := h.intentStates(t, f)
	if n := after["pending"] + after["inflight"]; n != 0 {
		t.Errorf("drainable intents after the switch = %d, want 0 (states: %v) — the backlog "+
			"survives the flag flip and fires on the next writeback pass", n, after)
	}
	if after["suppressed"] != 5 {
		t.Errorf("suppressed = %d, want 5 (the 4 pending + the 1 inflight) (states: %v)",
			after["suppressed"], after)
	}
	// CONTROL, in the same call: suppression must take the drainable rows and
	// ONLY the drainable rows.
	if after["applied"] != 1 || after["dead"] != 1 {
		t.Errorf("terminal rows were rewritten: states = %v, want applied=1 dead=1 left alone", after)
	}
}

// TestAccountSetLeavesTheBacklogAloneWithoutWritebackFalse is the NEGATIVE
// CONTROL for the test above, and without it that test would pass just as well
// against a handler that suppressed the backlog on EVERY Account/set — which
// would silently park a live account's queue whenever the user renamed it.
func TestAccountSetLeavesTheBacklogAloneWithoutWritebackFalse(t *testing.T) {
	h := newAPIHarness(t)
	api := newAccountAPI(t, h.store)
	f := seedAPIUser(t, h.super, "wbset-keep-"+store.NewULID().String())

	for _, op := range []string{"+flag", "move"} {
		h.seedIntent(t, f, op, "pending", "")
	}
	h.seedIntent(t, f, "+kw", "inflight", "")

	for _, patch := range []map[string]any{
		{"name": "renamed-" + store.NewULID().String()}, // no writeback property at all
		{"writebackEnabled": true},                      // the property, the other way
	} {
		res := call(t, api.accountSet, f.UserID, map[string]any{
			"update": map[string]any{f.AccountID.String(): patch},
		})
		if !mapHasKey(res, "updated", f.AccountID.String()) {
			t.Fatalf("patch %v was not applied: %v", patch, res)
		}
		states := h.intentStates(t, f)
		if states["pending"] != 2 || states["inflight"] != 1 {
			t.Errorf("patch %v: states = %v, want the backlog untouched (2 pending, 1 inflight)", patch, states)
		}
		if states["suppressed"] != 0 {
			t.Errorf("patch %v: %d intent(s) were suppressed by an update that did not turn "+
				"writeback off (states: %v)", patch, states["suppressed"], states)
		}
	}
}

// TestAccountSetUpdateRefusesWritebackTrueUnderTheInstanceFlag is ADR-028 §2 on
// the UPDATE door.
//
// Accepting it would store a value that cannot take effect, and Account/get
// would then tell the operator writeback is on for an account that structurally
// cannot write — "a setting that means its own opposite", which is the defect
// ADR-020 was written about.
//
// The second half is the required negative control: with the instance flag
// UNSET the identical request must succeed. Without it, this test passes
// against a build that refuses writebackEnabled:true unconditionally, which
// would be a worse bug wearing the same clothes.
func TestAccountSetUpdateRefusesWritebackTrueUnderTheInstanceFlag(t *testing.T) {
	h := newAPIHarness(t)
	api := newAccountAPI(t, h.store)
	f := seedAPIUser(t, h.super, "wbset-inst-"+store.NewULID().String())
	h.setWritebackColumn(t, f.AccountID.String(), false)

	req := map[string]any{
		"update": map[string]any{
			f.AccountID.String(): map[string]any{"writebackEnabled": true},
		},
	}

	h.store.SetWritebackDisabled(true)
	res := call(t, api.accountSet, f.UserID, req)

	if mapHasKey(res, "updated", f.AccountID.String()) {
		t.Fatalf("the update was accepted while MAILROOM_WRITEBACK_DISABLED holds: %v", res)
	}
	e := setError(t, res, "notUpdated", f.AccountID.String())
	if e["type"] != setInvalidProps {
		t.Errorf("SetError type = %v, want %q", e["type"], setInvalidProps)
	}
	if !propsName(e, "writebackEnabled") {
		t.Errorf("SetError properties = %v, want it to name writebackEnabled — a client cannot "+
			"show the user WHICH field was refused otherwise", e["properties"])
	}
	if h.writebackColumn(t, f.AccountID.String()) {
		t.Error("the refused update wrote writeback_enabled=true anyway")
	}

	// NEGATIVE CONTROL: lift the instance flag, send the identical request.
	h.store.SetWritebackDisabled(false)
	res = call(t, api.accountSet, f.UserID, req)
	if !mapHasKey(res, "updated", f.AccountID.String()) {
		t.Fatalf("the identical update was refused with the instance flag UNSET — the refusal is "+
			"not keyed on the flag at all: %v", res)
	}
	if !h.writebackColumn(t, f.AccountID.String()) {
		t.Error("writeback_enabled was not set to true with the instance flag unset")
	}
}

// TestAccountSetCreateRefusesWritebackTrueUnderTheInstanceFlag is the same
// refusal on the CREATE door, which is a genuinely separate code path
// (prepareAccountCreate, not prepareAccountUpdate) and was added to the ADR
// precisely because refusing one door and not the other leaves the hole open.
func TestAccountSetCreateRefusesWritebackTrueUnderTheInstanceFlag(t *testing.T) {
	h := newAPIHarness(t)
	api := newAccountAPI(t, h.store)
	f := seedAPIUser(t, h.super, "wbcreate-inst-"+store.NewULID().String())

	create := func(name string, writeback *bool) map[string]any {
		obj := map[string]any{
			"name":     name,
			"imapHost": "imap.example.com",
			"smtpHost": "smtp.example.com",
			"username": "you@example.com",
			"password": "s3cret-app-password",
		}
		if writeback != nil {
			obj["writebackEnabled"] = *writeback
		}
		return map[string]any{"create": map[string]any{"new": obj}}
	}
	yes := true

	h.store.SetWritebackDisabled(true)
	res := call(t, api.accountSet, f.UserID, create("refused-account", &yes))

	if created, _ := res["created"].(map[string]any); len(created) != 0 {
		t.Fatalf("an account was created with writebackEnabled:true under the instance flag: %v", created)
	}
	e := setError(t, res, "notCreated", "new")
	if e["type"] != setInvalidProps {
		t.Errorf("SetError type = %v, want %q", e["type"], setInvalidProps)
	}
	if !propsName(e, "writebackEnabled") {
		t.Errorf("SetError properties = %v, want it to name writebackEnabled", e["properties"])
	}
	var rows int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.accounts WHERE user_id = $1 AND name = 'refused-account'`,
		f.UserID.String()).Scan(&rows); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d account row(s) exist for a refused create", rows)
	}

	// CONTROL 1, with the flag STILL SET: the refusal is about the property, not
	// about creating accounts. A create that says nothing about writeback must
	// still succeed — otherwise onboarding is impossible during a cutover window.
	//
	// The stored column is deliberately NOT asserted here. ADR-028 §2 decides
	// what happens when a client SETS writebackEnabled true, and says nothing
	// about the default under the flag; the value that matters to a reader is the
	// EFFECTIVE one, which TestAccountGetHealthReportsEffectiveWriteback pins.
	res = call(t, api.accountSet, f.UserID, create("absent-property", nil))
	if !mapHasKey(res, "created", "new") {
		t.Fatalf("a create that omits writebackEnabled was refused under the instance flag: %v", res)
	}

	// CONTROL 2: with the flag unset, writebackEnabled:true is accepted and stored.
	h.store.SetWritebackDisabled(false)
	res = call(t, api.accountSet, f.UserID, create("allowed-account", &yes))
	obj, ok := res["created"].(map[string]any)["new"].(map[string]any)
	if !ok {
		t.Fatalf("writebackEnabled:true was refused with the instance flag UNSET: %v", res)
	}
	if obj["writebackEnabled"] != true {
		t.Errorf("created object reports writebackEnabled = %v, want true", obj["writebackEnabled"])
	}
	if !h.writebackColumn(t, obj["id"].(string)) {
		t.Error("writeback_enabled was not stored true")
	}
}

// TestAccountSetCreateWritebackEnabledIsAPointerNotABool pins the reason
// accountCreate.WritebackEnabled is a *bool.
//
// ⚠ A PLAIN bool HERE WOULD HAVE SHADOWED EVERY ACCOUNT EVER CREATED, silently:
// an absent property unmarshals to the zero value, false, and false is
// ingest-only. The account would sync, look completely healthy, and never write
// a single flag back. There is no error to observe — which is exactly the class
// of defect this project keeps paying for — so the default is pinned here
// instead.
//
// Both halves in one call, because the difference between them IS the test.
func TestAccountSetCreateWritebackEnabledIsAPointerNotABool(t *testing.T) {
	h := newAPIHarness(t)
	api := newAccountAPI(t, h.store)
	f := seedAPIUser(t, h.super, "wbcreate-ptr-"+store.NewULID().String())

	base := func(name string) map[string]any {
		return map[string]any{
			"name":     name,
			"imapHost": "imap.example.com",
			"smtpHost": "smtp.example.com",
			"username": "you@example.com",
			"password": "s3cret-app-password",
		}
	}
	shadowed := base("shadowed-on-create")
	shadowed["writebackEnabled"] = false
	defaulted := base("defaulted-on-create") // property ABSENT

	res := call(t, api.accountSet, f.UserID, map[string]any{
		"create": map[string]any{"shadow": shadowed, "default": defaulted},
	})
	created, _ := res["created"].(map[string]any)
	if len(created) != 2 {
		t.Fatalf("created = %v, notCreated = %v, want both accounts", created, res["notCreated"])
	}

	for _, tc := range []struct {
		cid  string
		want bool
	}{
		{"shadow", false},
		{"default", true},
	} {
		obj := created[tc.cid].(map[string]any)
		if obj["writebackEnabled"] != tc.want {
			t.Errorf("%s: created object writebackEnabled = %v, want %v",
				tc.cid, obj["writebackEnabled"], tc.want)
		}
		if got := h.writebackColumn(t, obj["id"].(string)); got != tc.want {
			t.Errorf("%s: stored writeback_enabled = %v, want %v — an absent property must take "+
				"the schema default, not the Go zero value", tc.cid, got, tc.want)
		}
	}
}

// propsName reports whether a /set error's `properties` array names prop.
func propsName(e map[string]any, prop string) bool {
	list, _ := e["properties"].([]any)
	for _, p := range list {
		if p == prop {
			return true
		}
	}
	return false
}

// ============================================================================
// B. Account/getHealth
// ============================================================================

// TestAccountGetHealthReportsEffectiveWriteback covers the two things the
// health panel has to get right about this mode, and they fail in opposite
// directions.
//
//  1. `writeback.enabled` is the EFFECTIVE value (column AND NOT instance
//     flag). Reporting the column verbatim tells the operator a backlog is
//     draining when the instance flag has it parked.
//  2. `writeback.suppressedIntents` is a LEDGER, not a failure count, and must
//     not be folded into deadLetters anywhere. Conflating them would report a
//     healthy shadow run — every mutation withheld exactly as designed — as
//     thousands of writeback errors, which is the one reading that would make
//     an operator abort a cutover that was going perfectly.
//
// The dead intent is the positive control on (2): deadLetters.count must be
// exactly 1, not 0 (which would mean the panel counts nothing) and not 4 (which
// would mean it counts the ledger as failures).
func TestAccountGetHealthReportsEffectiveWriteback(t *testing.T) {
	h := newAPIHarness(t)
	f := seedAPIUser(t, h.super, "health-wb-"+store.NewULID().String())

	for _, op := range []string{"+flag", "move", "expunge"} {
		h.seedIntent(t, f, op, "suppressed", "")
	}
	h.seedIntent(t, f, "move", "dead", "the archive role is missing")

	read := func(t *testing.T) map[string]any {
		t.Helper()
		res := call(t, h.api.accountGetHealth, f.UserID, map[string]any{})
		list := res["list"].([]any)
		if len(list) != 1 {
			t.Fatalf("health list len = %d, want 1", len(list))
		}
		return list[0].(map[string]any)
	}

	for _, tc := range []struct {
		name     string
		column   bool
		instance bool
		want     bool
	}{
		{"column true, no instance flag (control)", true, false, true},
		{"column true, INSTANCE FLAG SET", true, true, false},
		{"column false, no instance flag", false, false, false},
		{"both off", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.setWritebackColumn(t, f.AccountID.String(), tc.column)
			h.store.SetWritebackDisabled(tc.instance)
			t.Cleanup(func() { h.store.SetWritebackDisabled(false) })

			entry := read(t)
			wb, ok := entry["writeback"].(map[string]any)
			if !ok {
				t.Fatalf("no writeback block in the health entry: %v", entry)
			}
			if wb["enabled"] != tc.want {
				t.Errorf("writeback.enabled = %v, want %v (column=%v instanceFlag=%v) — this is "+
					"the EFFECTIVE value, not the column", wb["enabled"], tc.want, tc.column, tc.instance)
			}
			// The ledger is reported in every combination: it is what the go-live
			// decision rests on, and it does not disappear when the mode is lifted.
			if wb["suppressedIntents"] != float64(3) {
				t.Errorf("writeback.suppressedIntents = %v, want 3", wb["suppressedIntents"])
			}

			// ⚠ AND THE LEDGER IS NOT A FAILURE COUNT, anywhere in the payload.
			dl := entry["deadLetters"].(map[string]any)
			if dl["count"] != float64(1) {
				t.Errorf("deadLetters.count = %v, want exactly 1: 4 would mean the suppressed "+
					"ledger is being reported as writeback failures, 0 would mean the panel "+
					"counts no failures at all and this assertion proves nothing", dl["count"])
			}
			reasons := dl["reasons"].([]any)
			if len(reasons) != 1 || reasons[0] != "the archive role is missing" {
				t.Errorf("deadLetters.reasons = %v, want only the genuinely dead intent's reason", reasons)
			}
			df := entry["deliveryFailures"].(map[string]any)
			if df["count"] != float64(0) {
				t.Errorf("deliveryFailures.count = %v, want 0 — suppressed intents are not "+
					"delivery failures either", df["count"])
			}
		})
	}
}

// ============================================================================
// C. EmailSubmission/set
// ============================================================================

// bindSigningKey is bindKeyToIdentity with the reason it is load-bearing spelled
// out, because without a key row these tests are vacuous.
//
// ⚠ THE FIXTURE REQUIREMENT. compose.prepareOutbound is what rewrites a
// message's stored bytes at submit, and it returns EARLY — touching nothing —
// unless SigningKeyForIdentity finds a row. Only if it finds one with a
// NON-EMPTY public_key does it prepend the Autocrypt header, and only then does
// `changed` become true and ReplaceMessageBytes run. A live crypto process is
// NOT needed: signing is separately conditional on c.signer, and this harness
// has none, so the Autocrypt advertisement alone is what makes the rewrite path
// execute. bindKeyToIdentity (compose_signing_integration_test.go) seeds
// public_key '\x99010203' and a non-null private_key_ct, which is exactly the
// shape signingKeyUsable requires.
//
// Without this, "the stored bytes were not touched" would be true for every
// build, including one where the gate had been deleted outright.
func bindSigningKey(t *testing.T, h *composeHarness, userID ulid.ULID, address string) {
	t.Helper()
	keyID := bindKeyToIdentity(t, h, userID, address)
	var pub []byte
	if err := h.super.QueryRow(context.Background(),
		`SELECT public_key FROM app.user_keys WHERE id = $1`, keyID).Scan(&pub); err != nil {
		t.Fatalf("read seeded public key: %v", err)
	}
	if len(pub) == 0 {
		t.Fatal("the seeded signing key has an EMPTY public_key: prepareOutbound would return " +
			"before rewriting anything and every byte assertion in this file would be vacuous")
	}
}

// blobHashes lists the user's blob rows by hex hash, ordered — a multiset that
// can be compared for "nothing was written and nothing was reclaimed".
func (h *apiHarness) blobHashes(t *testing.T, userID ulid.ULID) string {
	t.Helper()
	rows, err := h.super.Query(context.Background(),
		`SELECT encode(content_hash, 'hex') FROM app.blobs WHERE user_id = $1 ORDER BY 1`,
		userID.String())
	if err != nil {
		t.Fatalf("list blob hashes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan blob hash: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list blob hashes: %v", err)
	}
	return strings.Join(out, ",")
}

// createDraft composes one draft through Email/set and returns its id.
func composeDraft(t *testing.T, h *composeHarness, userID ulid.ULID, from, subject string) string {
	t.Helper()
	res := call(t, h.api.emailSet, userID, map[string]any{
		"create": map[string]any{
			"c1": map[string]any{
				"from":       []map[string]any{{"email": from}},
				"to":         []map[string]any{{"email": "friend@example.net"}},
				"subject":    subject,
				"bodyValues": map[string]any{"1": map[string]any{"value": "body of " + subject}},
				"textBody":   []map[string]any{{"partId": "1"}},
			},
		},
	})
	created, _ := res["created"].(map[string]any)
	obj, ok := created["c1"].(map[string]any)
	if !ok {
		t.Fatalf("draft %q not created: %#v", subject, res)
	}
	return obj["id"].(string)
}

func contentHashOf(t *testing.T, h *composeHarness, userID ulid.ULID, emailID string) []byte {
	t.Helper()
	var hash []byte
	if err := h.super.QueryRow(context.Background(),
		`SELECT content_hash FROM app.messages WHERE user_id = $1 AND id = $2`,
		userID.String(), emailID).Scan(&hash); err != nil {
		t.Fatalf("read content hash: %v", err)
	}
	return hash
}

// emailCursor takes an Email /changes cursor: the state string a client would
// hold at this instant.
func emailCursor(t *testing.T, h *composeHarness, userID ulid.ULID, emailID string) string {
	t.Helper()
	state, _ := call(t, h.api.emailGet, userID, map[string]any{
		"ids": []string{emailID}, "properties": []string{"id"},
	})["state"].(string)
	if state == "" {
		t.Fatal("Email/get returned no state to use as a /changes cursor")
	}
	return state
}

// submissionRowStates lists the states of every submission row for a message,
// so a failure message can say WHICH row exists rather than just "1".
func submissionRowStates(t *testing.T, h *composeHarness, userID ulid.ULID, emailID string) []string {
	t.Helper()
	rows, err := h.super.Query(context.Background(),
		`SELECT state FROM app.submissions WHERE user_id = $1 AND message_id = $2 ORDER BY id`,
		userID.String(), emailID)
	if err != nil {
		t.Fatalf("list submissions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan submission state: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list submissions: %v", err)
	}
	return out
}

func submitJobCount(t *testing.T, h *composeHarness, userID ulid.ULID) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.jobs WHERE user_id = $1 AND kind = 'submit'`,
		userID.String()).Scan(&n); err != nil {
		t.Fatalf("count submit jobs: %v", err)
	}
	return n
}

// TestSubmissionOnAShadowedAccountLeavesNothingBehind is the sharp test for
// ADR-028 §6, and specifically for the gate having been MOVED in front of the
// first durable side effect (PlanSubmit → gate → SubmitPlanned).
//
// ⚠ "THE SUBMISSION WAS NOT SCHEDULED" IS THE WRONG ASSERTION, and would pass
// against the bug. The previous implementation ran Submit and then cancelled
// it, so its artifact was a row in app.submissions with state='canceled' — a
// send that was never permitted, recorded in the user's history — plus a
// message whose stored bytes had been rewritten and a change_log row telling
// every client the draft had changed. So this asserts ZERO ROWS, not a state,
// and it asserts on the bytes and on /changes.
//
// The two subtests share one body, so the control is genuinely identical: same
// draft, same key, same call, one flag different.
func TestSubmissionOnAShadowedAccountLeavesNothingBehind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shadowed bool
	}{
		{"shadowed: refused, and nothing at all is left behind", true},
		{"NEGATIVE CONTROL (writeback enabled): recorded, and the bytes ARE rewritten", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newComposeHarness(t)
			a, _ := seedTwo(t, h.super)
			seedSendable(t, h.super, a, "me@example.com", "")
			bindSigningKey(t, h, a.UserID, "me@example.com")
			h.setWritebackColumn(t, a.AccountID.String(), !tc.shadowed)

			emailID := composeDraft(t, h, a.UserID, "me@example.com", "shadow-send")
			hashBefore := contentHashOf(t, h, a.UserID, emailID)
			blobsBefore := h.blobHashes(t, a.UserID)
			cursor := emailCursor(t, h, a.UserID, emailID)

			res := call(t, h.api.submissionSet, a.UserID, map[string]any{
				"create": map[string]any{"s1": map[string]any{"emailId": emailID}},
			})

			hashAfter := contentHashOf(t, h, a.UserID, emailID)
			var supersededRows int
			if err := h.super.QueryRow(context.Background(),
				`SELECT count(*) FROM app.superseded_content WHERE user_id = $1 AND message_id = $2`,
				a.UserID.String(), emailID).Scan(&supersededRows); err != nil {
				t.Fatalf("count superseded_content: %v", err)
			}
			changes := call(t, h.api.emailChanges, a.UserID, map[string]any{"sinceState": cursor})
			nChanges := len(changes["created"].([]any)) + len(changes["updated"].([]any)) +
				len(changes["destroyed"].([]any))
			subs := submissionRowStates(t, h, a.UserID, emailID)
			jobs := submitJobCount(t, h, a.UserID)

			if tc.shadowed {
				// 7. The refusal itself.
				if created, _ := res["created"].(map[string]any); len(created) != 0 {
					t.Fatalf("a shadowed account created a submission: %v", created)
				}
				e := setError(t, res, "notCreated", "s1")
				if e["type"] != setForbidden {
					t.Errorf("SetError type = %v, want %q", e["type"], setForbidden)
				}
				// 8. ZERO rows — not "one row in state canceled".
				if len(subs) != 0 {
					t.Errorf("app.submissions holds %d row(s) %v for a refused send, want NONE. "+
						"A canceled row is exactly what the pre-fix implementation left behind, "+
						"so an assertion of \"not scheduled\" would have passed against the bug",
						len(subs), subs)
				}
				// 9. No job.
				if jobs != 0 {
					t.Errorf("%d submit job(s) exist for a refused send, want 0", jobs)
				}
				// 10. The bytes are untouched.
				if string(hashAfter) != string(hashBefore) {
					t.Errorf("the message's content_hash moved on a REFUSED send (%x -> %x): the "+
						"user is left holding a rewritten message they never sent",
						hashBefore, hashAfter)
				}
				if got := h.blobHashes(t, a.UserID); got != blobsBefore {
					t.Errorf("app.blobs changed on a refused send:\n before: %s\n  after: %s", blobsBefore, got)
				}
				if supersededRows != 0 {
					t.Errorf("%d superseded_content row(s) for a refused send, want 0 — the bytes "+
						"were replaced", supersededRows)
				}
				// 11. The sharpest one: no client ever hears about it.
				if nChanges != 0 {
					t.Errorf("Email/changes since the pre-request cursor is non-empty (%v) — a send "+
						"that never happened moved every client's sync cursor", changes)
				}
			} else {
				if !mapHasKey(res, "created", "s1") {
					t.Fatalf("the control send was refused: %v", res)
				}
				if len(subs) != 1 || subs[0] != "scheduled" {
					t.Errorf("app.submissions = %v, want exactly one scheduled row", subs)
				}
				if jobs != 1 {
					t.Errorf("submit jobs = %d, want 1", jobs)
				}
				// ⚠ THE CONTROL THAT MAKES THE NEGATIVES ABOVE MEAN ANYTHING. If the
				// fixture could not provoke a byte rewrite, every "unchanged" assertion
				// in the shadowed case would be passing for the wrong reason.
				if string(hashAfter) == string(hashBefore) {
					t.Errorf("content_hash did NOT move on a permitted send (%x): prepareOutbound "+
						"rewrote nothing, so the shadowed case's byte assertions are vacuous", hashBefore)
				}
				if supersededRows != 1 {
					t.Errorf("superseded_content rows = %d, want 1 on a permitted send — same problem",
						supersededRows)
				}
				if got := h.blobHashes(t, a.UserID); got == blobsBefore {
					t.Errorf("app.blobs is unchanged after a permitted send (%s): no new bytes were stored", got)
				}
				if nChanges == 0 {
					t.Error("Email/changes is empty after a permitted send that rewrote the message's " +
						"bytes — so the shadowed case's empty /changes proves nothing")
				}
			}
		})
	}
}

// TestSubmissionRefusedWhenOnlyTheInstanceFlagIsSet covers the OTHER half of
// the gate's condition. `!acct.WritebackEnabled || a.store.WritebackDisabled()`
// has two clauses and the account column exercises only the first; deleting the
// second would leave every test above green while the instance-level safety
// catch — the one that survives a misclick in the settings UI — did nothing.
func TestSubmissionRefusedWhenOnlyTheInstanceFlagIsSet(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	bindSigningKey(t, h, a.UserID, "me@example.com")
	// The per-account column stays TRUE: only the instance flag shadows this.
	if !h.writebackColumn(t, a.AccountID.String()) {
		t.Fatal("setup: the account column must be true, or this tests the wrong clause")
	}

	emailID := composeDraft(t, h, a.UserID, "me@example.com", "instance-flag-send")
	hashBefore := contentHashOf(t, h, a.UserID, emailID)

	h.store.SetWritebackDisabled(true)
	res := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{"s1": map[string]any{"emailId": emailID}},
	})
	if e := setError(t, res, "notCreated", "s1"); e["type"] != setForbidden {
		t.Errorf("SetError type = %v, want %q", e["type"], setForbidden)
	}
	if got := submissionRowStates(t, h, a.UserID, emailID); len(got) != 0 {
		t.Errorf("app.submissions holds %v for a send refused by the instance flag, want none", got)
	}
	if string(contentHashOf(t, h, a.UserID, emailID)) != string(hashBefore) {
		t.Error("the message's bytes were rewritten by a send the instance flag refused")
	}

	// NEGATIVE CONTROL: clear the flag; the identical send goes through. Without
	// it, everything above would pass against a build that refuses every send.
	h.store.SetWritebackDisabled(false)
	res = call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{"s1": map[string]any{"emailId": emailID}},
	})
	if !mapHasKey(res, "created", "s1") {
		t.Fatalf("the identical send was refused with the instance flag cleared: %v", res)
	}
}

// TestSubmissionRefusalDoesNotPoisonTheBatchTransaction is the ordering test,
// and it drives createSubmission DIRECTLY rather than through the /set method
// on purpose.
//
// ⚠ THE METHOD CANNOT BE MADE TO RUN THESE IN A CHOSEN ORDER. args.Create is a
// map[string]json.RawMessage and Go randomises map iteration, so a test that
// went through submissionSet would exercise one of the two orders at random.
// The order that matters is A-FIRST: if the refusal ever became a statement
// that ERRORS (rather than the pure pre-flight SELECT it is today), the
// transaction would be aborted with SQLSTATE 25P02 and the good object B —
// already accepted by the API — would vanish at commit with no error anywhere.
// That is the savepoint trap this project has paid for before, and the only way
// to pin it is to fix the order.
//
// TestSubmissionBatchRefusesOnlyTheShadowedAccount below covers the same shape
// through the real method, which is what proves the wiring.
func TestSubmissionRefusalDoesNotPoisonTheBatchTransaction(t *testing.T) {
	for _, aFirst := range []bool{true, false} {
		name := "B first (control)"
		if aFirst {
			name = "A first — the 25P02 case"
		}
		t.Run(name, func(t *testing.T) {
			h := newComposeHarness(t)
			ctx := context.Background()
			a, _ := seedTwo(t, h.super)
			seedSendable(t, h.super, a, "shadow@example.com", "")
			bindSigningKey(t, h, a.UserID, "shadow@example.com")
			live := seedSecondAccount(t, h.super, a, "secondary")
			seedSendable(t, h.super, live, "live@example.com", "")
			bindSigningKey(t, h, a.UserID, "live@example.com")
			h.setWritebackColumn(t, a.AccountID.String(), false)

			draftA := composeDraft(t, h, a.UserID, "shadow@example.com", "on the shadowed account")
			draftB := composeDraft(t, h, a.UserID, "live@example.com", "on the live account")

			type outcome struct {
				id  string
				err *SetError
			}
			var got []outcome
			if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
				order := []string{draftA, draftB}
				if !aFirst {
					order = []string{draftB, draftA}
				}
				for _, id := range order {
					_, _, _, serr := h.api.createSubmission(ctx, tx, a.UserID,
						json.RawMessage(`{"emailId":"`+id+`"}`))
					got = append(got, outcome{id: id, err: serr})
				}
				return nil // COMMIT: the point is what survives the commit
			}); err != nil {
				t.Fatalf("batch transaction failed outright: %v — a refused object must not "+
					"abort the transaction its siblings commit in", err)
			}

			for _, o := range got {
				switch o.id {
				case draftA:
					if o.err == nil {
						t.Error("the shadowed account's submission was accepted")
					} else if o.err.Type != setForbidden {
						t.Errorf("shadowed SetError type = %q, want %q", o.err.Type, setForbidden)
					}
				case draftB:
					if o.err != nil {
						t.Errorf("the LIVE account's submission was refused: %+v", o.err)
					}
				}
			}

			// And after COMMIT, B's durable work exists and A's does not.
			if s := submissionRowStates(t, h, a.UserID, draftA); len(s) != 0 {
				t.Errorf("the refused submission left %v in app.submissions", s)
			}
			if s := submissionRowStates(t, h, a.UserID, draftB); len(s) != 1 {
				t.Errorf("the LIVE submission is %v after commit, want exactly one row — the "+
					"refusal took its sibling down with it", s)
			}
			if n := submitJobCount(t, h, a.UserID); n != 1 {
				t.Errorf("submit jobs after commit = %d, want exactly 1 (B's)", n)
			}
			var jobLinked int
			if err := h.super.QueryRow(ctx,
				`SELECT count(*) FROM app.submissions
				  WHERE user_id = $1 AND message_id = $2 AND job_id IS NOT NULL`,
				a.UserID.String(), draftB).Scan(&jobLinked); err != nil {
				t.Fatalf("count linked jobs: %v", err)
			}
			if jobLinked != 1 {
				t.Errorf("B's submission has no job_id after commit (%d linked) — SetSubmissionJob "+
					"ran in a transaction that did not survive", jobLinked)
			}
		})
	}
}

// TestSubmissionBatchRefusesOnlyTheShadowedAccount is the same scenario through
// the real EmailSubmission/set method: one call, two creates, two accounts.
//
// ⚠ WHAT THIS DOES AND DOES NOT SHOW. It proves the WIRING — that a refusal on
// one account does not stop the other being created and committed by the JMAP
// method a client actually calls. It does NOT cover both orderings: `create` is
// a JSON object, the handler ranges a Go map, and the runtime randomises that.
// It is run several times so an order-dependent break has a fair chance of
// showing up here as well, but that is a bonus with no bound worth quoting —
// the ordering guarantee is TestSubmissionRefusalDoesNotPoisonTheBatch-
// Transaction above, which fixes the order by construction.
func TestSubmissionBatchRefusesOnlyTheShadowedAccount(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "shadow@example.com", "")
	bindSigningKey(t, h, a.UserID, "shadow@example.com")
	live := seedSecondAccount(t, h.super, a, "secondary")
	seedSendable(t, h.super, live, "live@example.com", "")
	bindSigningKey(t, h, a.UserID, "live@example.com")
	h.setWritebackColumn(t, a.AccountID.String(), false)

	const rounds = 6
	for i := 0; i < rounds; i++ {
		// Distinct subjects per round: a draft is identified by its content hash
		// (ADR-001), and two byte-identical drafts would resolve to ONE message
		// row, quietly turning round N into a re-submission of round N-1's draft.
		round := store.NewULID().String()
		draftA := composeDraft(t, h, a.UserID, "shadow@example.com", "batch shadow "+round)
		draftB := composeDraft(t, h, a.UserID, "live@example.com", "batch live "+round)

		res := call(t, h.api.submissionSet, a.UserID, map[string]any{
			"create": map[string]any{
				"shadowed": map[string]any{"emailId": draftA},
				"livesend": map[string]any{"emailId": draftB},
			},
		})
		if e := setError(t, res, "notCreated", "shadowed"); e["type"] != setForbidden {
			t.Fatalf("round %d: shadowed SetError type = %v, want %q", i, e["type"], setForbidden)
		}
		if !mapHasKey(res, "created", "livesend") {
			t.Fatalf("round %d: the live account's send was not created alongside a refused one: %v", i, res)
		}
		if s := submissionRowStates(t, h, a.UserID, draftA); len(s) != 0 {
			t.Fatalf("round %d: refused send left %v in app.submissions", i, s)
		}
		if s := submissionRowStates(t, h, a.UserID, draftB); len(s) != 1 {
			t.Fatalf("round %d: the live send is %v after commit, want one row", i, s)
		}
	}
	if n := submitJobCount(t, h, a.UserID); n != rounds {
		t.Errorf("submit jobs = %d, want %d — one per live send and none for the refused ones", n, rounds)
	}
}
