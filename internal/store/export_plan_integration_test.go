// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The export plan is checked against the LIVE schema, not against a list (#54).
//
// These live in internal/store rather than internal/exportplan because the
// property under test is agreement between a Go map and a real database, and
// only this package has a database. A unit test in exportplan could compare the
// map against a constant, which is two copies of the same typing.

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/exportplan"
)

// TestEveryLiveUserTableHasAnExportDisposition is the check the whole design
// rests on: no table is silently omitted from a user's export.
//
// ⚠ IT ASSERTS AGAINST THE CATALOG, so it goes red when someone adds a table and
// does not decide what an export does with it — which is the moment the decision
// is cheap. A test comparing the plan against a hardcoded table list would pass
// forever and prove only that the list and the map were typed to match once.
func TestEveryLiveUserTableHasAnExportDisposition(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)

	live := liveExportTables(ctx, t, h, a.ID)
	if len(live) < 20 {
		t.Fatalf("only %d live tables discovered — that is a broken census, not a "+
			"small schema, and this test would pass vacuously", len(live))
	}

	missing, stale := exportplan.Unhandled(live)
	for _, m := range missing {
		t.Errorf("app.%s exists and internal/exportplan has no disposition for it — "+
			"an export would omit it silently. Decide, then add it.", m)
	}
	for _, s := range stale {
		t.Errorf("internal/exportplan decides app.%s, which is not in the schema — "+
			"the plan has drifted", s)
	}
}

// TestANewTableIsReportedUnhandledRatherThanSilentlyDropped proves the previous
// test can actually fail, by CREATING a table and requiring it to be named.
//
// ⚠ WITHOUT THIS, "every table has a disposition" is satisfied by a schema that
// happens to match today and says nothing about the mechanism. This is the same
// technique as TestUserCensusIsDerivedFromTheCatalogNotAList, and for the same
// reason: the failure being guarded against is a decision nobody made, which
// only shows up when something new appears.
func TestANewTableIsReportedUnhandledRatherThanSilentlyDropped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)

	const probe = "the_44th_table"
	if _, ok := exportplan.Lookup(probe); ok {
		t.Fatalf("the export plan already decides %q, so this probe cannot "+
			"demonstrate anything", probe)
	}

	for _, stmt := range []string{
		`CREATE TABLE app.` + probe + ` (
		   user_id app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
		   note    text NOT NULL,
		   PRIMARY KEY (user_id, note))`,
		`ALTER TABLE app.` + probe + ` ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE app.` + probe + ` FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY t44 ON app.` + probe + ` FOR ALL TO mailroom_app
		   USING (user_id = app.current_user_id())
		   WITH CHECK (user_id = app.current_user_id())`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON app.` + probe + ` TO mailroom_app`,
	} {
		if _, err := h.super.Exec(ctx, stmt); err != nil {
			t.Fatalf("create probe table: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = h.super.Exec(context.Background(), `DROP TABLE IF EXISTS app.`+probe)
	})

	live := liveExportTables(ctx, t, h, a.ID)
	missing, _ := exportplan.Unhandled(live)

	var named bool
	for _, m := range missing {
		if m == probe {
			named = true
		}
	}
	if !named {
		t.Fatalf("a newly created user-scoped table was NOT reported as unhandled "+
			"(unhandled = %v). The export plan is not being checked against the "+
			"catalog, so a future table would be dropped from every user's export "+
			"with nothing saying so", missing)
	}
}

// TestWithheldTablesAreNeverAlsoExported. Withheld exists to be distinguishable
// from Dropped; a table in both states would defeat the point, and the map's
// shape makes that impossible to express — so this asserts the INVARIANT the
// reader relies on rather than re-typing the map.
func TestWithheldTablesAreNeverAlsoExported(t *testing.T) {
	for _, r := range exportplan.Rows() {
		switch r.Entry.Disposition {
		case exportplan.Exported:
			if r.Entry.Detail == "" {
				t.Errorf("app.%s is exported but names no output file — a reader cannot "+
					"tell where the data lands", r.Table)
			}
		case exportplan.Dropped, exportplan.Withheld:
			if r.Entry.Reason == "" {
				t.Errorf("app.%s is %s with no reason — a disposition without a reason "+
					"is a decision nobody has to defend", r.Table, r.Entry.Disposition)
			}
			if r.Entry.Detail != "" {
				t.Errorf("app.%s is %s but names an output file %q", r.Table,
					r.Entry.Disposition, r.Entry.Detail)
			}
		default:
			t.Errorf("app.%s has an unknown disposition %q", r.Table, r.Entry.Disposition)
		}
	}
}

// TestCredentialTablesAreWithheldNotMerelyDropped pins the distinction that
// matters most, by name.
//
// ⚠ These are the tables whose accidental inclusion in an export file is not
// recoverable by re-running the export: the credentials are already in a file
// that left the machine. Dropped would be the wrong disposition even though the
// bytes-on-disk outcome is identical today — the exporter is allowed to widen
// what it carries, and Withheld is what tells it where it must not.
func TestCredentialTablesAreWithheldNotMerelyDropped(t *testing.T) {
	for _, table := range []string{"accounts", "api_keys", "ai_providers", "user_keys"} {
		e, ok := exportplan.Lookup(table)
		if !ok {
			t.Errorf("app.%s has no disposition at all", table)
			continue
		}
		if e.Disposition != exportplan.Withheld {
			t.Errorf("app.%s is %q, want %q — it holds credential material, and "+
				"\"we chose not to\" must not be confusable with \"it would be unsafe to\"",
				table, e.Disposition, exportplan.Withheld)
		}
	}
}

// liveExportTables is the catalog-derived table set the plan is judged against:
// every user-scoped table plus the root.
func liveExportTables(ctx context.Context, t *testing.T, h *harness, userID string) []string {
	t.Helper()
	c, err := censusFor(ctx, h.appStore(), mustULID(t, userID))
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	live := make([]string, 0, len(c.Tables)+1)
	for _, tc := range c.Tables {
		live = append(live, tc.Table)
	}
	if c.Root != "" {
		live = append(live, c.Root)
	}
	return live
}

// TestKeywordSurveySeparatesLabelBackedFromIndependentKeywords.
//
// ⚠ THIS TEST EXISTS BECAUSE THE UNSPLIT MEASUREMENT WAS MISLEADING ON REAL
// DATA. A real mailbox reported nearly half its messages as "carrying custom
// IMAP keywords" under a heading about state an export would lose — and every
// one of those keywords turned out to be a `MailroomLabel-*` atom Mailroom
// itself wrote to IMAP and read back, i.e. app.message_labels in another
// encoding. True sentence, false impression.
//
// It asserts BOTH cases from one fixture, because a test with only the
// label-backed case would pass against a survey that counted nothing at all,
// and a test with only the independent case would pass against one that counted
// everything.
func TestKeywordSurveySeparatesLabelBackedFromIndependentKeywords(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	before, err := surveyFor(ctx, s, aID)
	if err != nil {
		t.Fatalf("survey before: %v", err)
	}

	// A label with its IMAP keyword atom, exactly as writeback creates one.
	labelID := NewULID().String()
	atom := "MailroomLabel-probe-" + NewULID().String()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO app.labels (id, user_id, name) VALUES ($1, $2, $3)`,
			[]any{labelID, a.ID, "probe-" + labelID}},
		{`INSERT INTO app.label_keyword_map (user_id, label_id, keyword_atom)
		  VALUES ($1, $2, $3)`, []any{a.ID, labelID, atom}},
	} {
		if _, err := h.super.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed label/keyword map: %v", err)
		}
	}

	// One message whose keyword IS the label's atom, one whose keyword is not.
	labelBacked := seedKeywordedMessage(t, h, a, []string{atom})
	independent := seedKeywordedMessage(t, h, a, []string{"$Forwarded"})
	_ = labelBacked

	after, err := surveyFor(ctx, s, aID)
	if err != nil {
		t.Fatalf("survey after: %v", err)
	}

	if got := after.Keyworded - before.Keyworded; got != 2 {
		t.Errorf("keyworded messages rose by %d, want 2 — the survey is not counting "+
			"the fixture it was given", got)
	}
	if got := after.KeywordedIndependently - before.KeywordedIndependently; got != 1 {
		t.Errorf("independently-keyworded rose by %d, want 1: the $Forwarded message "+
			"counts and the label-backed one must not", got)
	}
	if independent == "" {
		t.Fatal("fixture did not create the independent message")
	}
}

func surveyFor(ctx context.Context, s *Store, id ulid.ULID) (ExportSurvey, error) {
	var out ExportSurvey
	err := s.UserTx(ctx, id, func(tx *UserTx) error {
		var e error
		out, e = tx.SurveyExportable(ctx)
		return e
	})
	return out, err
}

// seedKeywordedMessage inserts one message carrying the given IMAP keywords and
// returns its id. It reuses the seeded user's thread and blob so the fixture
// stays a message, not a whole second object graph.
func seedKeywordedMessage(t *testing.T, h *harness, u fixtureUser, keywords []string) string {
	t.Helper()
	ctx := context.Background()
	id := NewULID().String()
	sum := sha256.Sum256([]byte("kwprobe-" + id))

	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		 VALUES ($1, $2, 10, 20, $3)`,
		u.ID, sum[:], NewULID().String()); err != nil {
		t.Fatalf("seed keyword probe blob: %v", err)
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.messages (id, user_id, content_hash, subject, from_address,
		    received_at, size_bytes, message_id_hint, keywords)
		 VALUES ($1, $2, $3, 'keyword probe', $4, now(), 10, $5, $6)`,
		id, u.ID, sum[:], "kw@example.com", "<kw-"+id+"@example.com>", keywords); err != nil {
		t.Fatalf("seed keyword probe message: %v", err)
	}
	return id
}
