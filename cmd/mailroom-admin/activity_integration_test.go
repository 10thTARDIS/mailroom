// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// `mailroom-admin activity` (ADR-053) against a real Postgres: per-kind counts
// for the instance, one user's entries with --user, and neither crossing into
// another user's records.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

type adminActivitySeed struct{ user, keyName, purged, ruleID string }

func seedAdminActivity(t *testing.T, super *pgxpool.Pool, tag string) adminActivitySeed {
	t.Helper()
	ctx := context.Background()
	s := adminActivitySeed{user: seedUntangleUser(t, super, tag).String(), keyName: "tool-" + tag,
		purged: store.NewULID().String(), ruleID: store.NewULID().String()}
	key := store.NewULID().String()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	th := sha256.Sum256([]byte("k-" + key))
	exec(`INSERT INTO app.api_keys (id, user_id, name, token_hash, scopes) VALUES ($1,$2,$3,$4,ARRAY['mail:read'])`, key, s.user, s.keyName, th[:])
	exec(`INSERT INTO app.api_key_events (id, user_id, key_id, method, outcome, reason) VALUES ($1,$2,$3,'Email/set','denied','scope')`, store.NewULID().String(), s.user, key)
	for _, a := range [][3]string{
		{"api_key.create", "ApiKey", key}, {store.PurgeAction, "Email", s.purged},
		{"placement.backfill.move", "PlacementBackfill", store.NewULID().String()},
		{store.RuleApplyAction, store.RuleApplyObjectType, s.ruleID},
	} {
		exec(`INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail) VALUES ($1,'user',$2,$3,$4,'{"changed":3}')`, s.user, a[0], a[1], a[2])
	}
	exec(`INSERT INTO app.rules (id, user_id, name, conditions, actions) VALUES ($1,$2,$3,'{}','[]')`, s.ruleID, s.user, "rule-"+tag)
	exec(`INSERT INTO app.pgp_decrypt_audit (id, user_id, message_id, purpose, outcome, key_fingerprint) VALUES ($1,$2,$3,'body','decrypted','AABBCCDD11223344')`,
		store.NewULID().String(), s.user, store.NewULID().String())
	sid := store.NewULID().String()
	sh := sha256.Sum256([]byte("s-" + sid))
	exec(`INSERT INTO app.sessions (id, user_id, token_hash, user_agent, revoked_at, revoked_reason) VALUES ($1,$2,$3,'Mozilla/5.0 Firefox/130.0 raw-ua-'||$4,now(),'signed_out')`,
		sid, s.user, sh[:], tag)
	return s
}

func TestActivityCommand(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	run := store.NewULID().String()
	a := seedAdminActivity(t, h.super, "adma-"+run)
	b := seedAdminActivity(t, h.super, "admb-"+run)
	since := time.Now().Add(-time.Hour)

	// --user: A's entries, none of B's, and no raw user agent or message content.
	uid := ulid.MustParse(a.user)
	var buf bytes.Buffer
	if err := activityCore(ctx, h.store, activityOptions{Since: since, User: &uid, Limit: 50, Cap: 100}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{a.keyName, a.purged, "sign_in", "sign_out", "placement", "rule_apply", "decryption", "ai_key", "REPORT ONLY"} {
		if !strings.Contains(out, want) {
			t.Errorf("--user output lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{b.keyName, b.purged, b.ruleID, "raw-ua-", "Mozilla"} {
		if strings.Contains(out, leak) {
			t.Errorf("--user output carries %q:\n%s", leak, out)
		}
	}
	// A source filter narrows it.
	buf.Reset()
	if err := activityCore(ctx, h.store, activityOptions{Since: since, User: &uid, Limit: 50, Cap: 100, Sources: []string{"purge"}}, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "sign_in") || !strings.Contains(buf.String(), a.purged) {
		t.Errorf("--source purge output = %s", buf.String())
	}
	// A bounded page says so.
	buf.Reset()
	if err := activityCore(ctx, h.store, activityOptions{Since: since, User: &uid, Limit: 2, Cap: 100}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "showing the 2 most recent") {
		t.Errorf("a bounded page does not say it is bounded:\n%s", buf.String())
	}

	// Instance-wide: every kind has a row and at least both users' entries; no
	// per-user detail (no key names, no ids); a cap of 1 says it is a lower bound.
	buf.Reset()
	if err := activityCore(ctx, h.store, activityOptions{Since: since, Limit: 50, Cap: 1000}, &buf); err != nil {
		t.Fatal(err)
	}
	out = buf.String()
	for _, k := range activityKindsInOrder {
		line := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, k+" ") {
				line = l
			}
		}
		if line == "" {
			t.Errorf("no row for kind %s:\n%s", k, out)
		}
		if strings.HasSuffix(line, " 0  -") {
			t.Errorf("kind %s counts 0 though two users have one each: %q", k, line)
		}
	}
	for _, leak := range []string{a.keyName, b.keyName, a.purged, b.purged, "raw-ua-"} {
		if strings.Contains(out, leak) {
			t.Errorf("instance-wide output carries per-user detail %q", leak)
		}
	}
	buf.Reset()
	if err := activityCore(ctx, h.store, activityOptions{Since: since, Limit: 50, Cap: 1}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "LOWER BOUND") {
		t.Errorf("a capped count does not say it is a lower bound:\n%s", buf.String())
	}
	// Exact counts for one user through the same counter.
	counts, oldest := map[string]int{}, map[string]*time.Time{}
	n, capped, err := countUserActivity(ctx, h.store, uid, activityOptions{Since: since, Cap: 1000}, counts, oldest)
	if err != nil || capped || n != 8 {
		t.Errorf("countUserActivity = %d, capped %v, %v; want 8 uncapped", n, capped, err)
	}
	for _, k := range activityKindsInOrder {
		if counts[k] != 1 {
			t.Errorf("kind %s counted %d for A, want 1", k, counts[k])
		}
	}
}
