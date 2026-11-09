// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The Activity view (ADR-053, #45), through ListActivity as mailroom_app. What is
// proven, each against real rows:
//
//  1. Every kind appears for its owner (the positive control) and NONE for
//     another user, per kind: a branch that lost its user predicate would be
//     caught by the kind that leaked, not hidden by a total.
//  2. Keyset paging over entries that SHARE a timestamp neither repeats nor
//     skips one, and a page walk equals the one-shot list.
//  3. Source and date filters select exactly their rows; a routine read is not
//     listed.
//  4. A deleted object keeps its entry and loses its link.
//  5. Bounds: limit, cursor and source are validated; the page says where it
//     stops and how far back each source goes.

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

type activitySeed struct {
	keyID, purgeMsg, batchID, ruleID, sessionID string
	base                                        time.Time
}

// activityKinds are every branch, so a new one cannot be left out of the
// isolation check without a red test.
var activityKinds = []string{
	ActivityKindAITool, ActivityKindAIKey, ActivityKindDecryption, ActivityKindPurge,
	ActivityKindSignIn, ActivityKindSignOut, ActivityKindPlacement, ActivityKindRuleApply,
}

// seedActivity writes one record for every kind, one minute apart from base
// going back, plus rows the view must NOT list.
func seedActivity(t *testing.T, super *pgxpool.Pool, u fixtureUser, base time.Time) activitySeed {
	t.Helper()
	ctx := context.Background()
	s := activitySeed{keyID: NewULID().String(), purgeMsg: NewULID().String(), batchID: NewULID().String(),
		ruleID: NewULID().String(), sessionID: NewULID().String(), base: base}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed activity: %v\n%s", err, sql)
		}
	}
	at := func(min int) time.Time { return base.Add(-time.Duration(min) * time.Minute) }
	th := sha256.Sum256([]byte("act-key-" + s.keyID))
	exec(`INSERT INTO app.api_keys (id, user_id, name, token_hash, scopes) VALUES ($1,$2,$3,$4,ARRAY['mail:read'])`,
		s.keyID, u.ID, "tool-"+u.Tag, th[:])
	// ai_tool: a refusal (listed), a write (listed), a routine read (NOT listed).
	exec(`INSERT INTO app.api_key_events (id, user_id, key_id, at, method, outcome, reason) VALUES ($1,$2,$3,$4,'Email/set','denied','scope')`,
		NewULID().String(), u.ID, s.keyID, at(1))
	exec(`INSERT INTO app.api_key_events (id, user_id, key_id, at, method, outcome) VALUES ($1,$2,$3,$4,'Email/set','ok')`,
		NewULID().String(), u.ID, s.keyID, at(2))
	exec(`INSERT INTO app.api_key_events (id, user_id, key_id, at, method, outcome) VALUES ($1,$2,$3,$4,'Email/get','ok')`,
		NewULID().String(), u.ID, s.keyID, at(3))
	audit := func(action, objType, objID string, detail string, m int) {
		exec(`INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail, created_at)
		      VALUES ($1,'user',$2,$3,$4,$5::jsonb,$6)`, u.ID, action, objType, objID, detail, at(m))
	}
	audit("api_key.create", "ApiKey", s.keyID, `{}`, 4)
	audit(PurgeAction, "Email", s.purgeMsg, `{"content_hash_prefix":"abcd"}`, 5)
	audit(backfillActionMove, backfillObjectType, s.batchID, `{}`, 6)
	audit(backfillActionMove, backfillObjectType, s.batchID, `{}`, 7)
	audit(RuleApplyAction, RuleApplyObjectType, s.ruleID, `{"changed":12}`, 8)
	// An audit row of an unrelated kind: never listed.
	audit("conflict.resolved", "Email", u.MessageID, `{}`, 9)
	exec(`INSERT INTO app.rules (id, user_id, name, conditions, actions) VALUES ($1,$2,$3,'{}','[]')`,
		s.ruleID, u.ID, "rule-"+u.Tag)
	exec(`INSERT INTO app.pgp_decrypt_audit (id, user_id, message_id, purpose, outcome, key_fingerprint, decrypted_at)
	      VALUES ($1,$2,$3,'body','decrypted','FFEEDDCC',$4)`, NewULID().String(), u.ID, u.MessageID, at(10))
	sess := func(id string, created time.Time, revokedMin int, reason string) {
		h := sha256.Sum256([]byte("act-sess-" + id))
		if reason == "" {
			exec(`INSERT INTO app.sessions (id, user_id, token_hash, created_at, user_agent) VALUES ($1,$2,$3,$4,'Mozilla/5.0 Firefox/130.0')`,
				id, u.ID, h[:], created)
			return
		}
		exec(`INSERT INTO app.sessions (id, user_id, token_hash, created_at, revoked_at, revoked_reason, user_agent)
		      VALUES ($1,$2,$3,$4,$5,$6,'Mozilla/5.0 Firefox/130.0')`, id, u.ID, h[:], created, at(revokedMin), reason)
	}
	sess(s.sessionID, at(11), 12, "signed_out")      // a sign_in at 11 and a sign_out at 12
	sess(NewULID().String(), at(13), 14, "replaced") // sign_in at 13 only: 'replaced' is no sign-out
	return s
}

func activityPage(t *testing.T, st *Store, uid string, f ActivityFilter) ActivityPage {
	t.Helper()
	var p ActivityPage
	if err := st.UserTx(context.Background(), ulid.MustParse(uid), func(tx *UserTx) error {
		var err error
		p, err = tx.ListActivity(context.Background(), f)
		return err
	}); err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	return p
}

func kindsOf(es []ActivityEntry) map[string]int {
	m := map[string]int{}
	for _, e := range es {
		m[e.Kind]++
	}
	return m
}

func TestActivityIsolationPerKind(t *testing.T) {
	h := newHarness(t)
	st := NewFromPool(h.app)
	a := seedUser(t, h.super, "acta-"+NewULID().String())
	b := seedUser(t, h.super, "actb-"+NewULID().String())
	base := time.Now().UTC().Truncate(time.Second)
	seedActivity(t, h.super, a, base)

	// Positive control: A sees every kind, with the counts the seed implies. The
	// routine read, the unrelated audit row and the 'replaced' sign-out are absent.
	got := kindsOf(activityPage(t, st, a.ID, ActivityFilter{}).Entries)
	want := map[string]int{ActivityKindAITool: 2, ActivityKindAIKey: 1, ActivityKindDecryption: 2, ActivityKindPurge: 1,
		ActivityKindSignIn: 2, ActivityKindSignOut: 1, ActivityKindPlacement: 1, ActivityKindRuleApply: 1}
	// the seedUser fixture adds its own decrypt row for A's message (one more)
	want[ActivityKindDecryption] = 2
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("A's kinds = %v, want %v", got, want)
	}
	for _, k := range activityKinds {
		if got[k] == 0 {
			t.Errorf("positive control: kind %s never listed for its owner", k)
		}
	}

	// B, who has only seedUser's own rows (a decrypt row), sees none of A's: per
	// kind, so the leaking branch is named.
	bSeen := kindsOf(activityPage(t, st, b.ID, ActivityFilter{}).Entries)
	for _, k := range activityKinds {
		if k == ActivityKindDecryption {
			if bSeen[k] != 1 {
				t.Errorf("B's decryptions = %d, want only B's own 1", bSeen[k])
			}
			continue
		}
		if bSeen[k] != 0 {
			t.Errorf("B sees %d %s entries of A's", bSeen[k], k)
		}
	}
	// And through each single-source filter: a branch asked for alone.
	for _, src := range ActivitySources {
		p := activityPage(t, st, b.ID, ActivityFilter{Sources: []string{src}})
		for _, e := range p.Entries {
			if e.Kind == ActivityKindDecryption {
				continue
			}
			t.Errorf("source %s: B sees A's %s entry %s", src, e.Kind, e.ID)
		}
	}
}

func TestActivityKeysetStableAcrossTies(t *testing.T) {
	h := newHarness(t)
	st := NewFromPool(h.app)
	a := seedUser(t, h.super, "actk-"+NewULID().String())
	ctx := context.Background()
	tie := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	// Eleven entries across four kinds, ALL at one instant, plus neighbours either side.
	key := NewULID().String()
	th := sha256.Sum256([]byte("tie-" + key))
	ins := func(sql string, args ...any) {
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	ins(`INSERT INTO app.api_keys (id, user_id, name, token_hash, scopes) VALUES ($1,$2,'k',$3,ARRAY['mail:read'])`, key, a.ID, th[:])
	for i := 0; i < 4; i++ {
		ins(`INSERT INTO app.api_key_events (id, user_id, key_id, at, method, outcome) VALUES ($1,$2,$3,$4,'Email/set','denied')`,
			NewULID().String(), a.ID, key, tie)
		ins(`INSERT INTO app.pgp_decrypt_audit (id, user_id, message_id, purpose, outcome, key_fingerprint, decrypted_at) VALUES ($1,$2,$3,'body','decrypted','AA',$4)`,
			NewULID().String(), a.ID, a.MessageID, tie)
	}
	for i := 0; i < 3; i++ {
		ins(`INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail, created_at) VALUES ($1,'system',$2,'Email',$3,'{}',$4)`,
			a.ID, PurgeAction, NewULID().String(), tie)
	}
	ins(`INSERT INTO app.pgp_decrypt_audit (id, user_id, message_id, purpose, outcome, key_fingerprint, decrypted_at) VALUES ($1,$2,$3,'body','decrypted','AA',$4)`,
		NewULID().String(), a.ID, a.MessageID, tie.Add(time.Minute))
	ins(`INSERT INTO app.pgp_decrypt_audit (id, user_id, message_id, purpose, outcome, key_fingerprint, decrypted_at) VALUES ($1,$2,$3,'body','decrypted','AA',$4)`,
		NewULID().String(), a.ID, a.MessageID, tie.Add(-time.Minute))

	whole := activityPage(t, st, a.ID, ActivityFilter{Limit: ActivityMaxLimit})
	if whole.Next != "" {
		t.Fatalf("one-shot list should be complete, next = %q", whole.Next)
	}
	id := func(e ActivityEntry) string { return e.Kind + ":" + e.ID }
	for _, size := range []int{1, 2, 3, 4, 7} {
		var walked []string
		var after *ActivityCursor
		for pages := 0; ; pages++ {
			if pages > 100 {
				t.Fatalf("size %d: pagination never ends", size)
			}
			p := activityPage(t, st, a.ID, ActivityFilter{Limit: size, After: after})
			for _, e := range p.Entries {
				walked = append(walked, id(e))
			}
			if p.Next == "" {
				break
			}
			c, err := ParseActivityCursor(p.Next)
			if err != nil {
				t.Fatal(err)
			}
			after = &c
		}
		var one []string
		for _, e := range whole.Entries {
			one = append(one, id(e))
		}
		if !reflect.DeepEqual(walked, one) {
			t.Errorf("size %d: page walk differs from the one-shot list (duplicate or gap)\nwalk: %v\nwhole: %v", size, walked, one)
		}
		seen := map[string]bool{}
		for _, w := range walked {
			if seen[w] {
				t.Errorf("size %d: %s listed twice", size, w)
			}
			seen[w] = true
		}
	}
	// The ties really are there: the fixture would prove nothing otherwise.
	atTie := 0
	for _, e := range whole.Entries {
		if e.At.Equal(tie) {
			atTie++
		}
	}
	if atTie != 11 {
		t.Fatalf("fixture: %d entries share the tie timestamp, want 11", atTie)
	}
}

func TestActivityFilters(t *testing.T) {
	h := newHarness(t)
	st := NewFromPool(h.app)
	a := seedUser(t, h.super, "actf-"+NewULID().String())
	base := time.Now().UTC().Truncate(time.Second)
	seedActivity(t, h.super, a, base)

	for src, kinds := range map[string][]string{
		ActivitySourceAITool:     {ActivityKindAITool, ActivityKindAIKey},
		ActivitySourceDecryption: {ActivityKindDecryption},
		ActivitySourcePurge:      {ActivityKindPurge},
		ActivitySourceSession:    {ActivityKindSignIn, ActivityKindSignOut},
		ActivitySourceBulk:       {ActivityKindPlacement, ActivityKindRuleApply},
	} {
		p := activityPage(t, st, a.ID, ActivityFilter{Sources: []string{src}})
		if len(p.Entries) == 0 {
			t.Errorf("source %s lists nothing", src)
		}
		for _, e := range p.Entries {
			ok := false
			for _, k := range kinds {
				ok = ok || e.Kind == k
			}
			if !ok {
				t.Errorf("source %s lists a %s entry", src, e.Kind)
			}
		}
	}
	// Two sources at once; the routine read is never listed under any filter.
	p := activityPage(t, st, a.ID, ActivityFilter{Sources: []string{ActivitySourcePurge, ActivitySourceDecryption}})
	for _, e := range p.Entries {
		if e.Kind != ActivityKindPurge && e.Kind != ActivityKindDecryption {
			t.Errorf("two-source filter lists %s", e.Kind)
		}
	}
	for _, e := range activityPage(t, st, a.ID, ActivityFilter{Sources: []string{ActivitySourceAITool}}).Entries {
		if e.Kind == ActivityKindAITool && e.Method == "Email/get" {
			t.Errorf("a routine read is listed: %+v", e)
		}
	}

	// Dates: since is inclusive, until exclusive, on the entry time. The purge is
	// at base-5m, the placement batch at base-6m.
	in := activityPage(t, st, a.ID, ActivityFilter{
		Since: base.Add(-6 * time.Minute), Until: base.Add(-5 * time.Minute)})
	if k := kindsOf(in.Entries); len(in.Entries) != 1 || k[ActivityKindPlacement] != 1 {
		t.Errorf("[-6m,-5m) = %v, want only the placement batch (until is exclusive)", k)
	}
	in = activityPage(t, st, a.ID, ActivityFilter{Since: base.Add(-6 * time.Minute), Until: base.Add(-4 * time.Minute)})
	if k := kindsOf(in.Entries); len(in.Entries) != 2 || k[ActivityKindPurge] != 1 || k[ActivityKindPlacement] != 1 {
		t.Errorf("[-6m,-4m) = %v, want the purge and the placement batch (since is inclusive)", k)
	}
	// Date filter composes with a cursor.
	first := activityPage(t, st, a.ID, ActivityFilter{Since: base.Add(-6 * time.Minute), Limit: 1})
	if len(first.Entries) != 1 || first.Next == "" {
		t.Fatalf("first page = %+v", first)
	}
	c, _ := ParseActivityCursor(first.Next)
	second := activityPage(t, st, a.ID, ActivityFilter{Since: base.Add(-6 * time.Minute), After: &c, Limit: 50})
	for _, e := range second.Entries {
		if e.At.After(first.Entries[0].At) {
			t.Errorf("the second page repeats something newer than the first: %+v", e)
		}
	}
}

func TestActivityDeletedObjectKeepsEntryLosesLink(t *testing.T) {
	h := newHarness(t)
	st := NewFromPool(h.app)
	ctx := context.Background()
	a := seedUser(t, h.super, "actd-"+NewULID().String())
	s := seedActivity(t, h.super, a, time.Now().UTC().Truncate(time.Second))
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	// A message of its own, decrypted: deleting it must not take the entry along.
	msg := NewULID().String()
	hash := sha256.Sum256([]byte("blob-" + a.Tag))
	exec(`INSERT INTO app.messages (id, user_id, content_hash, subject, from_address, received_at, size_bytes)
	      VALUES ($1,$2,$3,'gone soon','friend@example.com',now(),100)`, msg, a.ID, hash[:])
	exec(`INSERT INTO app.pgp_decrypt_audit (id, user_id, message_id, purpose, outcome, key_fingerprint, decrypted_at)
	      VALUES ($1,$2,$3,'attachment','decrypted','FFEEDDCC',now() + interval '1 hour')`, NewULID().String(), a.ID, msg)

	find := func(kind, object string) *ActivityEntry {
		for _, e := range activityPage(t, st, a.ID, ActivityFilter{Limit: ActivityMaxLimit}).Entries {
			if e.Kind == kind && e.ObjectID == object {
				e := e
				return &e
			}
		}
		return nil
	}
	for _, c := range []struct{ kind, obj string }{
		{ActivityKindDecryption, msg}, {ActivityKindAIKey, s.keyID}, {ActivityKindRuleApply, s.ruleID}, {ActivityKindAITool, s.keyID},
	} {
		if e := find(c.kind, c.obj); e == nil || !e.ObjectExists {
			t.Fatalf("%s: before deleting, entry = %+v, want a live link", c.kind, e)
		}
	}
	if e := find(ActivityKindPurge, s.purgeMsg); e == nil || e.ObjectExists {
		t.Fatalf("a purged message must never link: %+v", e)
	}

	exec(`DELETE FROM app.messages WHERE id = $1`, msg)
	exec(`DELETE FROM app.rules WHERE id = $1`, s.ruleID)
	if e := find(ActivityKindDecryption, msg); e == nil || e.ObjectExists {
		t.Errorf("decryption after its message is gone: %+v, want the entry kept and no link", e)
	}
	if e := find(ActivityKindRuleApply, s.ruleID); e == nil || e.ObjectExists || e.KeyName != "" {
		t.Errorf("rule apply after its rule is gone: %+v, want the entry kept and no link", e)
	}
}

func TestActivityBoundsAndCoverage(t *testing.T) {
	h := newHarness(t)
	st := NewFromPool(h.app)
	ctx := context.Background()
	a := seedUser(t, h.super, "actc-"+NewULID().String())
	base := time.Now().UTC().Truncate(time.Second)
	seedActivity(t, h.super, a, base)

	err := st.UserTx(ctx, ulid.MustParse(a.ID), func(tx *UserTx) error {
		for _, f := range []ActivityFilter{
			{Limit: ActivityMaxLimit + 1}, {Limit: -1}, {Sources: []string{"nope"}},
			{Since: base, Until: base}, {After: &ActivityCursor{Kind: "nope", ID: "1"}},
		} {
			if _, e := tx.ListActivity(ctx, f); !errors.Is(e, ErrActivityArgs) {
				t.Errorf("filter %+v: err = %v, want ErrActivityArgs", f, e)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p := activityPage(t, st, a.ID, ActivityFilter{Limit: 2})
	if len(p.Entries) != 2 || p.Next == "" || p.Limit != 2 {
		t.Errorf("limit 2: %d entries, next %q, limit %d", len(p.Entries), p.Next, p.Limit)
	}
	// Coverage: sessions go back to the 13-minute sign-in; purge to 5 minutes.
	if c := p.Covers[ActivitySourceSession]; c == nil || !c.Equal(base.Add(-13*time.Minute)) {
		t.Errorf("session coverage = %v, want base-13m", c)
	}
	if c := p.Covers[ActivitySourcePurge]; c == nil || !c.Equal(base.Add(-5*time.Minute)) {
		t.Errorf("purge coverage = %v, want base-5m", c)
	}
	// The cursor round-trips and refuses garbage.
	c := ActivityCursor{At: base, Kind: ActivityKindPurge, ID: "42"}
	if back, e := ParseActivityCursor(c.Encode()); e != nil || !back.At.Equal(base) || back.Kind != c.Kind || back.ID != "42" {
		t.Errorf("cursor round trip = %+v, %v", back, e)
	}
	for _, bad := range []string{"", "!!", "bm9wZQ", ActivityCursor{At: base, Kind: "x", ID: "1"}.Encode()} {
		if _, e := ParseActivityCursor(bad); !errors.Is(e, ErrActivityArgs) {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}

func TestAuditRuleApplyAppearsInActivity(t *testing.T) {
	h := newHarness(t)
	st := NewFromPool(h.app)
	ctx := context.Background()
	a := seedUser(t, h.super, "actr-"+NewULID().String())
	rule := NewULID().String()
	if err := st.UserTx(ctx, ulid.MustParse(a.ID), func(tx *UserTx) error {
		if err := tx.AuditRuleApply(ctx, rule, false, 7); err != nil {
			return err
		}
		return tx.AuditRuleApply(ctx, rule, true, 3)
	}); err != nil {
		t.Fatal(err)
	}
	p := activityPage(t, st, a.ID, ActivityFilter{Sources: []string{ActivitySourceBulk}})
	if len(p.Entries) != 2 || p.Entries[0].Outcome != RuleApplyUndoAction || p.Entries[0].Count != 3 ||
		p.Entries[1].Outcome != RuleApplyAction || p.Entries[1].Count != 7 {
		t.Errorf("rule apply entries = %+v", p.Entries)
	}
}
