// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// ADR-049 increment 1: the stored resolved area (messages.area_id) is a cache
// kept correct by triggers. These tests are the proof it never drifts.
//
//   - TestAreaIndexAgreesWithTheResolverUnderRandomChanges drives random
//     sequences of EVERY input change and asserts, after each step, that the
//     stored column equals the resolver for every message of the user, and that
//     a second user's rows are never written.
//   - TestAreaIndexAgreementDetectsADroppedTrigger is its negative control: it
//     drops each trigger in turn and requires the same run to go red. A run
//     that stays green with a trigger missing proves nothing about that trigger.
//   - TestAreaTabPrecedenceSQLMatchesGo holds the SQL copy of the tab
//     precedence to tabs.PrecedenceNames().

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/tabs"
)

func TestAreaTabPrecedenceSQLMatchesGo(t *testing.T) {
	h := newHarness(t)
	var sqlList []string
	if err := h.super.QueryRow(context.Background(), `SELECT app.area_tab_precedence()`).Scan(&sqlList); err != nil {
		t.Fatalf("read SQL precedence: %v", err)
	}
	if !slices.Equal(sqlList, tabs.PrecedenceNames()) {
		t.Fatalf("app.area_tab_precedence() = %v, tabs.PrecedenceNames() = %v: the SQL copy in the "+
			"stored-area triggers has drifted from the Go order (change migration + schema.sql together)",
			sqlList, tabs.PrecedenceNames())
	}
}

// aiWorld is one user's seeded mailbox plus the means to change it.
type aiWorld struct {
	h      *harness
	s      *Store
	userID ulid.ULID
	tag    string
	rng    *rand.Rand
	n      int // unique-name counter
	kinds  map[string]int
	skips  int
}

func newAIWorld(t *testing.T, h *harness, tag string, rng *rand.Rand) *aiWorld {
	t.Helper()
	u := seedUser(t, h.super, tag)
	w := &aiWorld{h: h, s: h.appStore(), userID: mustULID(t, u.ID), tag: tag, rng: rng, kinds: map[string]int{}}
	// seedUser's own mail would muddy the count; the world below is self-contained
	// but the user's fixture rows stay (they are not areas or labels).
	ctx := context.Background()
	if err := w.s.UserTx(ctx, w.userID, func(tx *UserTx) error {
		var labels []ulid.ULID
		for i := 0; i < 6; i++ {
			row := LabelRow{Name: fmt.Sprintf("L%d-%s", i, tag), MirrorToIMAP: false}
			if i%3 == 0 {
				row.Tab = tabs.All[w.rng.Intn(len(tabs.All))]
			}
			id, e := tx.CreateLabel(ctx, row)
			if e != nil {
				return e
			}
			labels = append(labels, id)
		}
		if _, e := tx.CreateArea(ctx, AreaRow{Name: "A0-" + tag, Position: 0,
			Feeds: []AreaFeed{{Tab: tabs.Updates}, {LabelID: &labels[1]}}}); e != nil {
			return e
		}
		if _, e := tx.CreateArea(ctx, AreaRow{Name: "A1-" + tag, Position: 1,
			Feeds: []AreaFeed{{Tab: tabs.Forums}}}); e != nil {
			return e
		}
		for i := 0; i < 14; i++ {
			if e := w.insertMessage(ctx, tx); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed world %s: %v", tag, err)
	}
	return w
}

func (w *aiWorld) randTab() any {
	if w.rng.Intn(4) == 0 {
		return nil
	}
	return string(tabs.All[w.rng.Intn(len(tabs.All))])
}

func (w *aiWorld) insertMessage(ctx context.Context, tx *UserTx) error {
	w.n++
	hash := sha256.Sum256([]byte(fmt.Sprintf("areaindex-%s-%d", w.tag, w.n)))
	if _, err := tx.tx.Exec(ctx, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		VALUES ($1,$2,10,20,$3)`, w.userID.String(), hash[:], NewULID().String()); err != nil {
		return err
	}
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes,
		                          classified_tab, classifier_version)
		VALUES ($1,$2,$3,$4,now(),10,$5::app.category_tab,$6)`,
		NewULID().String(), w.userID.String(), hash[:], fmt.Sprintf("m%d", w.n), w.randTab(), tabs.ClassifierVersion)
	return err
}

func (w *aiWorld) col(ctx context.Context, tx *UserTx, q string, args ...any) ([]string, error) {
	rows, err := tx.tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func pick(rng *rand.Rand, xs []string) (string, bool) {
	if len(xs) == 0 {
		return "", false
	}
	return xs[rng.Intn(len(xs))], true
}

var aiKinds = []string{
	"label-add", "label-remove", "verdict", "map-tab", "feed-add", "feed-remove", "feed-retarget",
	"area-create", "area-delete", "area-reorder", "label-delete-store", "label-delete-cascade",
	"msg-insert", "msg-delete",
}

// step applies one random input change as the user, through the application
// role, and returns the kind it ran. A step that hits a legitimate refusal
// (a feed already taken, an area's own label) is rolled back and counted as
// skipped, not failed.
func (w *aiWorld) step(ctx context.Context) (string, error) {
	kind := aiKinds[w.rng.Intn(len(aiKinds))]
	uid := w.userID.String()
	err := w.s.UserTx(ctx, w.userID, func(tx *UserTx) error {
		msgs, err := w.col(ctx, tx, `SELECT id FROM app.messages WHERE user_id=$1 ORDER BY id`, uid)
		if err != nil {
			return err
		}
		labels, err := w.col(ctx, tx, `SELECT id FROM app.labels WHERE user_id=$1 ORDER BY id`, uid)
		if err != nil {
			return err
		}
		areaIDs, err := w.col(ctx, tx, `SELECT id FROM app.areas WHERE user_id=$1 ORDER BY id`, uid)
		if err != nil {
			return err
		}
		switch kind {
		case "label-add":
			m, ok1 := pick(w.rng, msgs)
			l, ok2 := pick(w.rng, labels)
			if !ok1 || !ok2 {
				return errSkip
			}
			_, err := tx.tx.Exec(ctx, `INSERT INTO app.message_labels (user_id, message_id, label_id)
				VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, uid, m, l)
			return err
		case "label-remove":
			rows, err := w.col(ctx, tx, `SELECT message_id||'/'||label_id FROM app.message_labels WHERE user_id=$1`, uid)
			if err != nil {
				return err
			}
			r, ok := pick(w.rng, rows)
			if !ok {
				return errSkip
			}
			parts := strings.Split(r, "/")
			_, err = tx.tx.Exec(ctx, `DELETE FROM app.message_labels WHERE user_id=$1 AND message_id=$2 AND label_id=$3`,
				uid, parts[0], parts[1])
			return err
		case "verdict":
			m, ok := pick(w.rng, msgs)
			if !ok {
				return errSkip
			}
			if t := w.randTab(); t != nil {
				_, err := tx.StoreClassifiedTab(ctx, mustULIDNoT(m), tabs.Tab(t.(string)), tabs.ClassifierVersion+w.n)
				w.n++
				return err
			}
			_, err := tx.tx.Exec(ctx, `UPDATE app.messages SET classified_tab = NULL WHERE user_id=$1 AND id=$2`, uid, m)
			return err
		case "map-tab":
			l, ok := pick(w.rng, labels)
			if !ok {
				return errSkip
			}
			var tab any
			if t := w.randTab(); t != nil {
				tab = t
			}
			_, err := tx.tx.Exec(ctx, `UPDATE app.labels SET tab = $3::app.category_tab WHERE user_id=$1 AND id=$2`, uid, l, tab)
			return err
		case "feed-add", "feed-remove":
			a, ok := pick(w.rng, areaIDs)
			if !ok {
				return errSkip
			}
			views, err := tx.GetAreas(ctx, []ulid.ULID{mustULIDNoT(a)})
			if err != nil || len(views) == 0 {
				return errSkip
			}
			feeds := append([]AreaFeed(nil), views[0].Feeds...)
			if kind == "feed-remove" {
				if len(feeds) < 2 {
					return errSkip
				}
				feeds = slices.Delete(feeds, 0, 1)
			} else if w.rng.Intn(2) == 0 {
				feeds = append(feeds, AreaFeed{Tab: tabs.All[w.rng.Intn(len(tabs.All))]})
			} else {
				l, ok := pick(w.rng, labels)
				if !ok {
					return errSkip
				}
				lid := mustULIDNoT(l)
				feeds = append(feeds, AreaFeed{LabelID: &lid})
			}
			_, err = tx.UpdateArea(ctx, views[0].ID, AreaPatch{SetFeeds: true, Feeds: feeds})
			return err
		case "feed-retarget":
			// A raw UPDATE of a tab feed's tab: the one input no application path
			// uses (feeds are deleted and re-inserted) but the schema allows.
			a, ok := pick(w.rng, areaIDs)
			if !ok {
				return errSkip
			}
			_, err := tx.tx.Exec(ctx, `UPDATE app.area_feeds SET tab = $3::app.category_tab
				WHERE user_id=$1 AND area_id=$2 AND tab IS NOT NULL`, uid, a, string(tabs.All[w.rng.Intn(len(tabs.All))]))
			return err
		case "area-create":
			w.n++
			row := AreaRow{Name: fmt.Sprintf("N%d-%s", w.n, w.tag), Position: w.rng.Intn(5)}
			if w.rng.Intn(2) == 0 {
				row.Feeds = []AreaFeed{{Tab: tabs.All[w.rng.Intn(len(tabs.All))]}}
			}
			_, err := tx.CreateArea(ctx, row)
			return err
		case "area-delete":
			a, ok := pick(w.rng, areaIDs)
			if !ok {
				return errSkip
			}
			_, err := tx.DeleteArea(ctx, mustULIDNoT(a))
			return err
		case "area-reorder":
			a, ok := pick(w.rng, areaIDs)
			if !ok {
				return errSkip
			}
			p := w.rng.Intn(6)
			_, err := tx.UpdateArea(ctx, mustULIDNoT(a), AreaPatch{Position: &p})
			return err
		case "label-delete-store", "label-delete-cascade":
			free, err := w.col(ctx, tx, `SELECT id FROM app.labels l WHERE user_id=$1
				AND NOT EXISTS (SELECT 1 FROM app.areas a WHERE a.user_id=l.user_id AND a.label_id=l.id)`, uid)
			if err != nil {
				return err
			}
			l, ok := pick(w.rng, free)
			if !ok {
				return errSkip
			}
			if kind == "label-delete-store" {
				_, err = tx.DeleteLabel(ctx, mustULIDNoT(l))
				return err
			}
			// The cascade path: the label row goes with its message_labels and feeds.
			_, err = tx.tx.Exec(ctx, `DELETE FROM app.labels WHERE user_id=$1 AND id=$2`, uid, l)
			return err
		case "msg-insert":
			return w.insertMessage(ctx, tx)
		case "msg-delete":
			m, ok := pick(w.rng, msgs)
			if !ok {
				return errSkip
			}
			_, err := tx.tx.Exec(ctx, `DELETE FROM app.messages WHERE user_id=$1 AND id=$2`, uid, m)
			return err
		}
		return fmt.Errorf("unknown kind %q", kind)
	})
	if errors.Is(err, errSkip) || errors.Is(err, ErrFeedTaken) || errors.Is(err, ErrLabelNameTaken) {
		w.skips++
		return kind + " (skipped)", nil
	}
	if err != nil && isUniqueViolationErr(err) {
		w.skips++
		return kind + " (skipped)", nil
	}
	if err == nil {
		w.kinds[kind]++
	}
	return kind, err
}

var errSkip = errors.New("skip")

func isUniqueViolationErr(err error) bool { return err != nil && isUniqueViolation(err) }

func mustULIDNoT(s string) ulid.ULID {
	id, err := ulid.Parse(s)
	if err != nil {
		panic(err)
	}
	return id
}

// agrees returns an error describing every place the stored column differs
// from the resolver (areaJoinSQL) for the user, or nil.
func (w *aiWorld) agrees(ctx context.Context) error {
	var rep AreaIndexReport
	if err := w.s.UserTx(ctx, w.userID, func(tx *UserTx) error {
		var e error
		rep, e = tx.AreaIndexReport(ctx)
		return e
	}); err != nil {
		return err
	}
	if rep.Disagreeing() != 0 {
		return fmt.Errorf("%d of %d message(s) hold a stored area that differs from the resolver: %+v",
			rep.Disagreeing(), rep.Messages, rep.Disagreements)
	}
	return nil
}

// snapshot is every message's (id, area_id, xmin): any write at all to the
// user's rows changes it.
func (w *aiWorld) snapshot(ctx context.Context) (string, error) {
	var s string
	err := w.h.super.QueryRow(ctx, `SELECT COALESCE(string_agg(id||':'||COALESCE(area_id,'')||':'||xmin::text, ',' ORDER BY id), '')
		FROM app.messages WHERE user_id=$1`, w.userID.String()).Scan(&s)
	return s, err
}

// runAreaIndexAgreement is the whole randomised run. It returns the first
// failure (rather than failing the test) so the probe can require one.
func runAreaIndexAgreement(t *testing.T, h *harness, seed int64, steps int) (*aiWorld, error) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(seed))
	a := newAIWorld(t, h, fmt.Sprintf("aia-%d-%s", seed, NewULID().String()), rng)
	b := newAIWorld(t, h, fmt.Sprintf("aib-%d-%s", seed, NewULID().String()), rand.New(rand.NewSource(seed+1)))
	// Both start from triggers' own population; make the column exact first so
	// the loop starts from agreement and every later disagreement is a step's.
	if err := a.agrees(ctx); err != nil {
		return a, fmt.Errorf("seed %d, initial state, user A: %w", seed, err)
	}
	if err := b.agrees(ctx); err != nil {
		return a, fmt.Errorf("seed %d, initial state, user B: %w", seed, err)
	}
	before, err := b.snapshot(ctx)
	if err != nil {
		return a, err
	}
	for i := 0; i < steps; i++ {
		kind, err := a.step(ctx)
		if err != nil {
			return a, fmt.Errorf("seed %d, step %d (%s): %w", seed, i, kind, err)
		}
		if err := a.agrees(ctx); err != nil {
			return a, fmt.Errorf("seed %d, step %d (%s): %w", seed, i, kind, err)
		}
		after, err := b.snapshot(ctx)
		if err != nil {
			return a, err
		}
		if after != before {
			return a, fmt.Errorf("seed %d, step %d (%s): the other user's messages were written", seed, i, kind)
		}
	}
	if err := b.agrees(ctx); err != nil {
		return a, fmt.Errorf("seed %d, user B at the end: %w", seed, err)
	}
	// The trigger-side forms agree with the stored column too: the single-
	// message function says nothing differs, and the set-based recompute writes 0.
	var diff int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.messages
		WHERE user_id=$1 AND app.resolve_message_area(user_id, id) IS DISTINCT FROM area_id`,
		a.userID.String()).Scan(&diff); err != nil {
		return a, err
	}
	if diff != 0 {
		return a, fmt.Errorf("seed %d: resolve_message_area disagrees with the stored column on %d message(s)", seed, diff)
	}
	var changed int64
	if err := a.s.UserTx(ctx, a.userID, func(tx *UserTx) error {
		return tx.tx.QueryRow(ctx, `SELECT app.recompute_message_areas($1)`, a.userID.String()).Scan(&changed)
	}); err != nil {
		return a, err
	}
	if changed != 0 {
		return a, fmt.Errorf("seed %d: recompute_message_areas rewrote %d message(s) the stored column already had right", seed, changed)
	}
	return a, nil
}

func TestAreaIndexAgreesWithTheResolverUnderRandomChanges(t *testing.T) {
	h := newHarness(t)
	for _, seed := range []int64{1, 2, 3} {
		w, err := runAreaIndexAgreement(t, h, seed, 200)
		if err != nil {
			t.Fatalf("stored area drifted (re-run with this seed): %v", err)
		}
		// Not vacuous: every kind of input change really ran, and the column
		// held real areas (not NULL throughout).
		for _, k := range aiKinds {
			if w.kinds[k] == 0 {
				t.Errorf("seed %d: step kind %q never succeeded in 200 steps (skips: %d); the run does not cover it", seed, k, w.skips)
			}
		}
		var nonNull int
		if err := h.super.QueryRow(context.Background(),
			`SELECT count(area_id) FROM app.messages WHERE user_id=$1`, w.userID.String()).Scan(&nonNull); err != nil {
			t.Fatal(err)
		}
		if nonNull == 0 {
			t.Errorf("seed %d: no message ended with a stored area; the run exercised only the NULL case", seed)
		}
		t.Logf("seed %d: kinds=%v skips=%d stored-non-null=%d", seed, w.kinds, w.skips, nonNull)
	}
}

// TestAreaIndexAgreementDetectsADroppedTrigger drops each trigger in turn
// (restoring it from its own pg_get_triggerdef afterwards) and requires the
// randomised run to go red. required lists the triggers whose absence MUST be
// seen; areas_area_ins is listed separately because a new area cannot change
// any resolution (nothing feeds it yet), so its absence is unobservable.
func TestAreaIndexAgreementDetectsADroppedTrigger(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	type trig struct{ name, table string }
	required := []trig{
		{"message_labels_area", "app.message_labels"},
		{"messages_area_insert", "app.messages"},
		{"messages_area_verdict", "app.messages"},
		{"areas_area_del", "app.areas"},
		{"areas_area_pos", "app.areas"},
		{"area_feeds_area_ins", "app.area_feeds"},
		{"area_feeds_area_del", "app.area_feeds"},
		{"area_feeds_area_upd", "app.area_feeds"},
		{"labels_area_tab", "app.labels"},
	}
	for _, tr := range required {
		t.Run(tr.name, func(t *testing.T) {
			var def string
			if err := h.super.QueryRow(ctx, `SELECT pg_get_triggerdef(oid) FROM pg_trigger
				WHERE tgname=$1 AND tgrelid=$2::regclass`, tr.name, tr.table).Scan(&def); err != nil {
				t.Fatalf("read trigger %s: %v", tr.name, err)
			}
			if _, err := h.super.Exec(ctx, `DROP TRIGGER `+tr.name+` ON `+tr.table); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := h.super.Exec(ctx, def); err != nil {
					t.Errorf("restore trigger %s: %v", tr.name, err)
				}
			})
			var lastErr error
			for _, seed := range []int64{1, 2, 3} {
				if _, lastErr = runAreaIndexAgreement(t, h, seed, 200); lastErr != nil {
					t.Logf("RED as required without %s: %v", tr.name, trunc(lastErr.Error(), 300))
					return
				}
			}
			t.Fatalf("the randomised agreement run stayed green without trigger %s on 3 seeds: "+
				"the test cannot see that trigger", tr.name)
		})
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// TestAreaIndexBulkRelabelCost times a bulk relabel with the triggers in place
// and without them, and logs the ratio. A measurement, not an assertion, so it
// runs only when asked for: MAILROOM_AREA_INDEX_BULK_COST=1.
func TestAreaIndexBulkRelabelCost(t *testing.T) {
	if os.Getenv("MAILROOM_AREA_INDEX_BULK_COST") == "" {
		t.Skip("set MAILROOM_AREA_INDEX_BULK_COST=1 to run the bulk relabel timing")
	}
	h := newHarness(t)
	ctx := context.Background()
	const n = 3000
	build := func() *aiWorld {
		w := newAIWorld(t, h, "cost-"+NewULID().String(), rand.New(rand.NewSource(7)))
		if err := w.s.UserTx(ctx, w.userID, func(tx *UserTx) error {
			for i := 0; i < n; i++ {
				if err := w.insertMessage(ctx, tx); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return w
	}
	// The bulk relabel: attach a label that feeds an area to every message, as a
	// rule or a move-all does, in one statement; and the desk's per-message
	// materialise path over the same mail.
	timeIt := func(w *aiWorld) (bulk, perMsg time.Duration) {
		var label string
		if err := h.super.QueryRow(ctx, `SELECT f.label_id FROM app.area_feeds f
			WHERE f.user_id=$1 AND f.label_id IS NOT NULL ORDER BY f.id LIMIT 1`, w.userID.String()).Scan(&label); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := w.s.UserTx(ctx, w.userID, func(tx *UserTx) error {
			_, e := tx.tx.Exec(ctx, `INSERT INTO app.message_labels (user_id, message_id, label_id)
				SELECT user_id, id, $2 FROM app.messages WHERE user_id=$1 ON CONFLICT DO NOTHING`, w.userID.String(), label)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		bulk = time.Since(start)
		// per-message: remove then re-add one row at a time, the shape of a rule applying per message.
		ids, _ := func() ([]string, error) {
			var out []string
			err := w.s.UserTx(ctx, w.userID, func(tx *UserTx) error {
				var e error
				out, e = w.col(ctx, tx, `SELECT id FROM app.messages WHERE user_id=$1`, w.userID.String())
				return e
			})
			return out, err
		}()
		start = time.Now()
		if err := w.s.UserTx(ctx, w.userID, func(tx *UserTx) error {
			for _, id := range ids {
				if _, e := tx.tx.Exec(ctx, `DELETE FROM app.message_labels WHERE user_id=$1 AND message_id=$2 AND label_id=$3`,
					w.userID.String(), id, label); e != nil {
					return e
				}
				if _, e := tx.tx.Exec(ctx, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
					w.userID.String(), id, label); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return bulk, time.Since(start)
	}
	with := build()
	withBulk, withPer := timeIt(with)

	var def string
	if err := h.super.QueryRow(ctx, `SELECT pg_get_triggerdef(oid) FROM pg_trigger
		WHERE tgname='message_labels_area' AND tgrelid='app.message_labels'::regclass`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `DROP TRIGGER message_labels_area ON app.message_labels`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = h.super.Exec(ctx, def) })
	without := build()
	woBulk, woPer := timeIt(without)
	t.Logf("MEASURED (%d messages): single-statement bulk relabel with=%v without=%v ratio=%.2f; "+
		"per-message relabel with=%v without=%v ratio=%.2f",
		n, withBulk, woBulk, float64(withBulk)/float64(woBulk), withPer, woPer, float64(withPer)/float64(woPer))
}
