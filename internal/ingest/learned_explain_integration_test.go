// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// WHY LEARNED PLACEMENT HELD BACK (#840): the arrival counter and
// `placement-explain`. Both are checked against the REAL ingest path: the
// counter against the reason a live arrival produced, and the explanation
// against the reason the live path returned for the same message, so the
// explain path cannot drift from the decision it describes.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/placement"
	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// lpRecorder records every learned-placement decision ingest reports.
type lpRecorder struct {
	mu  sync.Mutex
	got []string
}

func (r *lpRecorder) observe(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, reason)
}

func (r *lpRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = nil
}

func (r *lpRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

// lpObserve rebuilds the fixture's Ingestor with the counter sink and the
// trusted authserv-ids (lpWithTrustedAuthserv would drop the sink).
func (f *fixture) lpObserve(t *testing.T, rec *lpRecorder, ids ...string) {
	t.Helper()
	blobs, err := blob.NewFSBlobStore(t.TempDir(), randomKey(t))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	f.ing = New(f.st, blobs, WithTrustedAuthservIDs(ids), WithLearnedPlacement(rec.observe))
}

// lpIngestRaw is lpIngest that also returns the raw bytes it ingested, which
// is what placement-explain reads back from the blob store.
func (f *fixture) lpIngestRaw(t *testing.T, from string, extra ...string) (ulid.ULID, []byte) {
	t.Helper()
	uid := lpUID.Add(1)
	raw := rawFrom(from, fmt.Sprintf("Note %d", uid), fmt.Sprintf("lp-%d-%s@example.com", uid, f.userID), extra...)
	res, err := f.ing.Ingest(context.Background(), f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, uid, nil))
	if err != nil {
		t.Fatalf("ingest from %q: %v", from, err)
	}
	if !res.Created {
		t.Fatalf("ingest from %q: not a new message", from)
	}
	return res.MessageID, raw
}

// lpExplainRaw re-runs the arrival decision for msg through the read-only
// path placement-explain uses.
func (f *fixture) lpExplainRaw(t *testing.T, msg ulid.ULID, raw []byte, trusted []string, asOf bool) LearnedExplanation {
	t.Helper()
	ctx := context.Background()
	var out LearnedExplanation
	if err := f.st.UserReadTx(ctx, f.userID, func(tx *store.UserTx) error {
		cands, err := tx.ExplainCandidates(ctx, &msg, "", 0)
		if err != nil {
			return err
		}
		if len(cands) != 1 {
			return fmt.Errorf("explain candidates = %d, want 1", len(cands))
		}
		c := cands[0]
		var at time.Time
		if asOf {
			at = c.CreatedAt
		}
		out, err = ExplainLearnedRaw(ctx, tx, msg, raw, c.From, trusted, c.ReceivedAt, at)
		return err
	}); err != nil {
		t.Fatalf("explain: %v", err)
	}
	return out
}

// lpSeedStoppedGate leaves the Updates area's correction gate stopped (19 kept,
// 1 corrected, the same seeding TestLearnedPlacementGateStopsAnArea uses).
func (f *fixture) lpSeedStoppedGate(t *testing.T, upd store.AreaView) {
	t.Helper()
	for i := 0; i < placement.GateMinResolved; i++ {
		who := fmt.Sprintf("seed%d@other.example", i)
		m := f.lpIngest(t, who)
		outcome, keptAt, keptBy, correctedAt := "kept", "now()", "'age'", "NULL"
		if i == 0 {
			outcome, keptAt, keptBy, correctedAt = "corrected", "NULL", "NULL", "now()"
		}
		if _, err := f.super.Exec(context.Background(), `
			INSERT INTO app.learned_placements (user_id, message_id, key_kind, key_value, area_id, decided_at, outcome, kept_at, kept_by, corrected_at)
			VALUES ($1, $2, 'address', $3, $4, now() - interval '9 days', $5, `+keptAt+`, `+keptBy+`, `+correctedAt+`)`,
			f.userID.String(), m.String(), who, upd.ID.String(), outcome); err != nil {
			t.Fatal(err)
		}
	}
}

// lpClaim re-attributes every label of area on mail from `from` to the user:
// the user's own filing, for an area the desk already puts that mail in (a
// Re-file into the area a message is already in writes no row of its own).
func (f *fixture) lpClaim(t *testing.T, from string, area store.AreaView) {
	t.Helper()
	tag, err := f.super.Exec(context.Background(), `
		UPDATE app.message_labels ml SET applied_by = 'user'
		  FROM app.messages m
		 WHERE m.id = ml.message_id AND m.user_id = $1 AND m.from_address = $2 AND ml.label_id = $3`,
		f.userID.String(), from, area.LabelID.String())
	if err != nil {
		t.Fatal(err)
	}
	// #848: claiming nothing, or claiming rows the evidence query does not
	// count, leaves the test with no filings at all.
	n := int(tag.RowsAffected())
	if n == 0 {
		t.Fatalf("fixture: claiming %s labels on mail from %s matched no rows", area.Name, from)
	}
	f.lpWantFilings(t, from, area, n)
}

// lpFilingsOf reads the user's recorded filings on the ADDRESS key `from` per
// area, through the evidence query the decision itself uses (the explain
// trace's Evidence, #848): the count the placement decision would see, not a
// count of rows the test believes it wrote. The sender is built from `from`
// alone (the evidence does not depend on the authentication inputs).
func (f *fixture) lpFilingsOf(t *testing.T, from string) map[ulid.ULID]int {
	t.Helper()
	ctx := context.Background()
	var out map[ulid.ULID]int
	if err := f.st.UserReadTx(ctx, f.userID, func(tx *store.UserTx) error {
		cands, err := tx.ExplainCandidates(ctx, nil, from, 1)
		if err != nil {
			return err
		}
		if len(cands) != 1 {
			return fmt.Errorf("no message from %q to read the evidence through", from)
		}
		sender := placement.NewSender(from,
			placement.FromHeaders{Fields: 1, Raw: []string{from}, Parsed: []string{from}},
			func(string) placement.SenderAuth { return placement.SenderAuth{} })
		tr, err := tx.ExplainLearnedPlacement(ctx, cands[0].MessageID, sender, time.Time{})
		if err != nil {
			return err
		}
		if !tr.KeysOK {
			return fmt.Errorf("%q has no key", from)
		}
		out = tr.Evidence.Address.Filings
		return nil
	}); err != nil {
		t.Fatalf("evidence for %q: %v", from, err)
	}
	return out
}

// lpWantFilings asserts the evidence query reports exactly n of the user's own
// filings from `from` in area. A test that depends on filings calls this
// BEFORE its behavioural assertion: a fixture whose filings recorded nothing
// (a Re-file into the area the desk already holds writes no row of its own)
// would otherwise pass for the wrong reason (#848).
func (f *fixture) lpWantFilings(t *testing.T, from string, area store.AreaView, n int) {
	t.Helper()
	if got := f.lpFilingsOf(t, from)[area.ID]; got != n {
		t.Fatalf("fixture: evidence for %s in %s = %d filings, want %d (the filings were not recorded)", from, area.Name, got, n)
	}
}

// lpCase is one arrival: its setup, the arrival itself, and the label the
// counter and the explanation must both report for it.
type lpCase struct {
	name    string
	trusted []string
	want    string
	// setup runs before the arrival (its own ingests are not asserted on).
	setup func(t *testing.T, f *fixture, areas map[string]store.AreaView)
	// arrive ingests THE message under test.
	arrive func(t *testing.T, f *fixture) (ulid.ULID, []byte)
	// filed is the evidence the case relies on, as the evidence query reports
	// it per address key once setup has run (#848). lpRun asserts it BEFORE
	// the arrival, so a setup whose filings recorded nothing fails as a broken
	// fixture rather than passing on the held-back outcome it was meant to
	// reach another way.
	filed []lpFiled
}

// lpFiled is one expectation of lpCase.filed: n of the user's own filings from
// `from` in the area named `area`.
type lpFiled struct {
	from, area string
	n          int
}

const (
	lpTrusted = "mx.trusted.example"
	lpBank    = "alerts@bank.example"
)

func lpPass(domain string) string {
	return "Authentication-Results: " + lpTrusted + "; dkim=pass header.d=" + domain + "; dmarc=pass header.from=" + domain
}

// lpCases is one case per reason the counter can report from a real arrival
// ("error" and "other" have their own tests: no fixture makes ingest fail, and
// "other" is a label no decision produces).
func lpCases() []lpCase {
	from := func(addr string, extra ...string) func(*testing.T, *fixture) (ulid.ULID, []byte) {
		return func(t *testing.T, f *fixture) (ulid.ULID, []byte) { return f.lpIngestRaw(t, addr, extra...) }
	}
	file := func(addr string, n int, to string) func(*testing.T, *fixture, map[string]store.AreaView) {
		return func(t *testing.T, f *fixture, areas map[string]store.AreaView) { f.lpFile(t, addr, n, areas, to) }
	}
	return []lpCase{
		{name: "placed", want: "placed", setup: file("alice@example.com", 2, "Updates"), arrive: from("alice@example.com"),
			filed: []lpFiled{{"alice@example.com", "Updates", 2}}},
		{name: "none", want: "none", arrive: from("stranger@example.com")},
		{name: "thin", want: "thin", setup: file("alice@example.com", 1, "Updates"), arrive: from("alice@example.com"),
			filed: []lpFiled{{"alice@example.com", "Updates", 1}}},
		{name: "agrees", want: "agrees", setup: func(t *testing.T, f *fixture, areas map[string]store.AreaView) {
			f.lpSwitch(t, false)
			f.lpFile(t, "alice@example.com", 2, areas, "Primary")
			f.lpClaim(t, "alice@example.com", areas["Primary"])
			f.lpSwitch(t, true)
		}, arrive: from("alice@example.com"), filed: []lpFiled{{"alice@example.com", "Primary", 2}}},
		{name: "mixed", want: "mixed", setup: func(t *testing.T, f *fixture, areas map[string]store.AreaView) {
			f.lpSwitch(t, false)
			f.lpFile(t, "alice@example.com", 2, areas, "Updates")
			f.lpFile(t, "alice@example.com", 1, areas, "Promotions")
			f.lpSwitch(t, true)
		}, arrive: from("alice@example.com"),
			filed: []lpFiled{{"alice@example.com", "Updates", 2}, {"alice@example.com", "Promotions", 1}}},
		{name: "contradicted", want: "contradicted", setup: func(t *testing.T, f *fixture, areas map[string]store.AreaView) {
			f.lpFile(t, "bob@example.com", 2, areas, "Updates")
			learned := f.lpIngest(t, "bob@example.com")
			f.lpUpdate(t, learned, store.EmailUpdate{RemoveLabelIDs: []ulid.ULID{areas["Updates"].LabelID}})
		}, arrive: from("bob@example.com"), filed: []lpFiled{{"bob@example.com", "Updates", 2}}},
		{name: "off", want: "off", setup: func(t *testing.T, f *fixture, areas map[string]store.AreaView) {
			f.lpFile(t, "alice@example.com", 2, areas, "Updates")
			f.lpSwitch(t, false)
		}, arrive: from("alice@example.com"), filed: []lpFiled{{"alice@example.com", "Updates", 2}}},
		{name: "blocked", want: "blocked", setup: func(t *testing.T, f *fixture, areas map[string]store.AreaView) {
			f.lpSwitch(t, false)
			f.lpFile(t, "ann@gmail.com", 1, areas, "Updates")
			f.lpFile(t, "ben@gmail.com", 1, areas, "Updates")
			f.lpSwitch(t, true)
		}, arrive: from("new.friend@gmail.com"),
			filed: []lpFiled{{"ann@gmail.com", "Updates", 1}, {"ben@gmail.com", "Updates", 1}}},
		{name: "nokey", want: "nokey", arrive: from("")},
		{name: "ambiguous", want: "ambiguous", setup: file(lpBank, 2, "Updates"),
			arrive: from(lpBank, "From: x@attacker.example"), filed: []lpFiled{{lpBank, "Updates", 2}}},
		{name: "unauthenticated", want: "unauthenticated", trusted: []string{lpTrusted}, setup: file(lpBank, 2, "Updates"),
			arrive: from(lpBank, "Authentication-Results: "+lpTrusted+"; dmarc=fail header.from=bank.example"),
			filed:  []lpFiled{{lpBank, "Updates", 2}}},
		{name: "unauthenticated, a pass for another domain", want: "unauthenticated", trusted: []string{lpTrusted},
			setup:  file(lpBank, 2, "Updates"),
			arrive: from(lpBank, lpPass("attacker.example")), filed: []lpFiled{{lpBank, "Updates", 2}}},
		// #846: relaxed alignment. This case was "unauthenticated" until the
		// organisational-domain pass counted; the sibling below still is.
		{name: "placed, a pass for the organisational domain of a subdomain sender", want: "placed",
			trusted: []string{lpTrusted}, setup: func(t *testing.T, f *fixture, areas map[string]store.AreaView) {
				f.lpFile(t, "alerts@mail.bank.example", 2, areas, "Forums")
				f.lpClaim(t, "alerts@mail.bank.example", areas["Forums"])
			},
			arrive: from("alerts@mail.bank.example", lpPass("bank.example")),
			filed:  []lpFiled{{"alerts@mail.bank.example", "Forums", 2}}},
		{name: "unauthenticated, a pass for a sibling of a subdomain sender", want: "unauthenticated",
			trusted: []string{lpTrusted}, setup: func(t *testing.T, f *fixture, areas map[string]store.AreaView) {
				f.lpFile(t, "alerts@mail.bank.example", 2, areas, "Forums")
				f.lpClaim(t, "alerts@mail.bank.example", areas["Forums"])
			},
			arrive: from("alerts@mail.bank.example", lpPass("shop.bank.example")),
			filed:  []lpFiled{{"alerts@mail.bank.example", "Forums", 2}}},
		{name: "warned", want: "warned", trusted: []string{lpTrusted}, setup: file("shop@xn--pple-43d.com", 2, "Updates"),
			arrive: from("shop@xn--pple-43d.com", lpPass("xn--pple-43d.com")),
			filed:  []lpFiled{{"shop@xn--pple-43d.com", "Updates", 2}}},
		{name: "already_placed", want: "already_placed", setup: func(t *testing.T, f *fixture, areas map[string]store.AreaView) {
			ctx := context.Background()
			f.lpFile(t, "alice@example.com", 2, areas, "Updates")
			if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
				_, err := tx.CreateRule(ctx, store.RuleRow{
					Name: "alice-to-forums", Enabled: true,
					Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "alice@example.com"},
					Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: areas["Forums"].LabelID.String()}},
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}, arrive: from("alice@example.com"), filed: []lpFiled{{"alice@example.com", "Updates", 2}}},
		{name: "gated", want: "gated", setup: func(t *testing.T, f *fixture, areas map[string]store.AreaView) {
			f.lpSeedStoppedGate(t, areas["Updates"])
			f.lpFile(t, "alice@example.com", 2, areas, "Updates")
		}, arrive: from("alice@example.com"), filed: []lpFiled{{"alice@example.com", "Updates", 2}}},
	}
}

// lpRun sets one case up on a fresh fixture and returns the arrival's message,
// its raw bytes, and the labels the counter saw for it.
func lpRun(t *testing.T, c lpCase) (*fixture, ulid.ULID, []byte, []string) {
	t.Helper()
	f := newFixture(t)
	rec := &lpRecorder{}
	f.lpObserve(t, rec, c.trusted...)
	areas := f.lpAreas(t)
	if c.setup != nil {
		c.setup(t, f, areas)
	}
	for _, w := range c.filed {
		f.lpWantFilings(t, w.from, areas[w.area], w.n)
	}
	rec.reset() // only THE arrival is counted below
	msg, raw := c.arrive(t, f)
	return f, msg, raw, rec.all()
}

// TestLearnedPlacementCounterOncePerArrival: one increment per arrival, with
// the reason the live path produced, for every reason a real arrival can have.
//
// NEGATIVE CONTROL: with the `in.learnedObserve(...)` call in
// applyLearnedPlacementAtIngest removed, every case fails with "counter saw
// []" (run and recorded in the PR).
func TestLearnedPlacementCounterOncePerArrival(t *testing.T) {
	for _, c := range lpCases() {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, got := lpRun(t, c)
			if len(got) != 1 || got[0] != c.want {
				t.Errorf("counter saw %v for the arrival, want exactly [%s]", got, c.want)
			}
		})
	}
}

// TestLearnedPlacementCounterReportsAnError: a decision that fails is counted
// as "error", and the ingest transaction is not what carries the failure.
func TestLearnedPlacementCounterReportsAnError(t *testing.T) {
	f := newFixture(t)
	rec := &lpRecorder{}
	f.lpObserve(t, rec)
	msg := f.lpIngest(t, "alice@example.com")
	rec.reset()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		// A cancelled context makes every read inside the decision fail.
		f.ing.applyLearnedPlacementAtIngest(ctx, tx, msg, placement.Sender{})
		return nil
	})
	if got := rec.all(); len(got) != 1 || got[0] != "error" {
		t.Errorf("counter saw %v for a failed decision, want exactly [error]", got)
	}
}

// TestPlacementExplainMatchesLive: for each kind of arrival, the explanation
// re-run after the fact reaches the SAME reason as the live path did, and
// shows the check that refused. Every case also asserts the live label
// itself, so a case cannot pass by both sides being the same wrong thing.
//
// NEGATIVE CONTROL: with ExplainLearnedPlacement made to diverge (its Result
// taken as Continue's opposite, see the PR) the unauthenticated, ambiguous,
// agrees, thin and placed cases go red.
func TestPlacementExplainMatchesLive(t *testing.T) {
	for _, c := range lpCases() {
		t.Run(c.name, func(t *testing.T) {
			f, msg, raw, live := lpRun(t, c)
			if len(live) != 1 || live[0] != c.want {
				t.Fatalf("fixture: the live arrival reported %v, want [%s]", live, c.want)
			}
			ex := f.lpExplainRaw(t, msg, raw, c.trusted, false)
			if got := ex.Trace.Result.Label(); got != c.want {
				t.Errorf("explain outcome = %s (%q), want the live path's %s", got, ex.Trace.Result.Reason, c.want)
			}
			if c.want != "placed" && c.want != "gated" && ex.Trace.Continue {
				t.Errorf("explain says the placement goes ahead; the live path held it back (%s)", c.want)
			}
			if c.want != "placed" && ex.Trace.FirstRefusal == "" {
				t.Errorf("explain names no first refusal for a held-back message (%s)", c.want)
			}
			if c.want == "placed" && ex.Trace.FirstRefusal != "" {
				t.Errorf("explain names a refusal %q for a placed message", ex.Trace.FirstRefusal)
			}
		})
	}
}

// TestPlacementExplainShowsTheAuthenticationInputs: the facts an operator
// needs to see why a DMARC pass did not count: the trusted clause, its
// header.from, and that it is not the key domain (a different domain, a
// subdomain sender against a sibling, and against a parent above its
// organisational domain).
func TestPlacementExplainShowsTheAuthenticationInputs(t *testing.T) {
	for _, tc := range []struct{ name, sender, passFor string }{
		{"different domain", lpBank, "attacker.example"},
		{"subdomain sender, sibling pass", "alerts@mail.bank.example", "shop.bank.example"},
		{"subdomain sender, public-suffix pass", "alerts@mail.example.co.uk", "co.uk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			rec := &lpRecorder{}
			f.lpObserve(t, rec, lpTrusted)
			areas := f.lpAreas(t)
			// The desk puts mail from some of these senders in Updates itself, so
			// the filings are claimed (#848).
			f.lpFileHeld(t, tc.sender, 2, areas, "Updates")
			f.lpWantFilings(t, tc.sender, areas["Updates"], 2)
			msg, raw := f.lpIngestRaw(t, tc.sender, lpPass(tc.passFor))
			ex := f.lpExplainRaw(t, msg, raw, []string{lpTrusted}, false)

			tr := ex.Trace
			if tr.Checks == nil || tr.Checks.Refusal != placement.Unauthenticated {
				t.Fatalf("checks = %+v, want an unauthenticated refusal", tr.Checks)
			}
			if tr.Checks.Auth.DMARCPass || tr.Checks.Auth.DMARCMatch != "" || !tr.Checks.Auth.TrustConfigured {
				t.Errorf("auth = %+v, want trust configured and no bound DMARC pass", tr.Checks.Auth)
			}
			if len(ex.Facts.TrustedIDs) != 1 || ex.Facts.TrustedIDs[0] != lpTrusted {
				t.Errorf("trusted ids = %v, want [%s]", ex.Facts.TrustedIDs, lpTrusted)
			}
			if len(ex.Facts.AuthHeaders) != 1 || !ex.Facts.AuthHeaders[0].Topmost {
				t.Fatalf("auth headers = %+v, want one topmost trusted header", ex.Facts.AuthHeaders)
			}
			var hf string
			for _, cl := range ex.Facts.AuthHeaders[0].Clauses {
				if cl.Method == "dmarc" && len(cl.Props["header.from"]) == 1 {
					hf = cl.Props["header.from"][0]
				}
			}
			if hf != tc.passFor || hf == tr.Domain.Value {
				t.Errorf("dmarc header.from = %q against key domain %q, want %q and not equal", hf, tr.Domain.Value, tc.passFor)
			}
			if ex.Facts.FromFields != 1 || len(ex.Facts.Mailboxes) != 1 {
				t.Errorf("sender parse = %d From fields, %d mailboxes, want 1 and 1", ex.Facts.FromFields, len(ex.Facts.Mailboxes))
			}
		})
	}
}

// TestPlacementExplainAsOf: --as-of counts only filings created before the
// message arrived. After a message was placed, the user filed more mail the
// other way; now the evidence is mixed, but at arrival it was not. Without
// as-of the explanation is of the present, with it of the arrival.
//
// NEGATIVE CONTROL: with placementFilings ignoring asOf the as-of half
// reports "mixed" and fails.
func TestPlacementExplainAsOf(t *testing.T) {
	f := newFixture(t)
	rec := &lpRecorder{}
	f.lpObserve(t, rec)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	msg, raw := f.lpIngestRaw(t, "alice@example.com")
	if got := rec.all(); got[len(got)-1] != "placed" {
		t.Fatalf("fixture: the arrival was %v, want placed", got)
	}
	time.Sleep(20 * time.Millisecond)
	f.lpSwitch(t, false) // so the extra filings are not corrections of a learned label
	f.lpFile(t, "alice@example.com", 3, areas, "Promotions")
	f.lpSwitch(t, true)

	if got := f.lpExplainRaw(t, msg, raw, nil, false).Trace.Result.Label(); got != "mixed" {
		t.Errorf("explained as of now = %s, want mixed (the later filings count)", got)
	}
	if got := f.lpExplainRaw(t, msg, raw, nil, true).Trace.Result.Label(); got != "placed" {
		t.Errorf("explained as of the arrival = %s, want placed (later filings do not count)", got)
	}
}

// TestPlacementExplainWritesNothing: explain changes no row, and its
// transaction REFUSES a write. The negative control is the ordinary UserTx,
// which accepts the very same write: the refusal is the read-only
// transaction's, not an accident of the statement.
func TestPlacementExplainWritesNothing(t *testing.T) {
	f := newFixture(t)
	rec := &lpRecorder{}
	f.lpObserve(t, rec)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	f.lpWantFilings(t, "alice@example.com", areas["Updates"], 2)
	msg, raw := f.lpIngestRaw(t, "alice@example.com")

	count := func() [3]int {
		return [3]int{
			f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE user_id=$1`, f.userID.String()),
			f.scalar(t, `SELECT count(*) FROM app.learned_placements WHERE user_id=$1`, f.userID.String()),
			f.scalar(t, `SELECT count(*) FROM app.change_log WHERE user_id=$1`, f.userID.String()),
		}
	}
	before := count()
	f.lpExplainRaw(t, msg, raw, nil, false)
	f.lpExplainRaw(t, msg, raw, nil, true)
	if after := count(); after != before {
		t.Errorf("rows (labels, placements, change_log) went %v -> %v by explaining", before, after)
	}

	ctx := context.Background()
	write := func(tx *store.UserTx) error {
		_, err := tx.ResetPlacementGate(ctx, areas["Updates"].ID)
		return err
	}
	if err := f.st.UserReadTx(ctx, f.userID, write); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("a write in UserReadTx = %v, want a read-only transaction error", err)
	}
	if err := f.st.UserTx(ctx, f.userID, write); err != nil {
		t.Errorf("negative control: the same write in an ordinary UserTx = %v, want success", err)
	}
}

// TestPlacementExplainReportsAnOrgDomainMatch (#846): a subdomain sender
// placed on its organisational domain's pass is explained as an org-domain
// match, and an exact pass as exact, from the decision's own SenderAuth.
//
// NEGATIVE CONTROL: with LearnedSender using ml.DMARCStrict, the org-domain
// half fails (no pass, unauthenticated).
func TestPlacementExplainReportsAnOrgDomainMatch(t *testing.T) {
	for _, tc := range []struct{ passFor, want string }{
		{"bank.example", "org-domain"},
		{"mail.bank.example", "exact"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			f := newFixture(t)
			rec := &lpRecorder{}
			f.lpObserve(t, rec, lpTrusted)
			areas := f.lpAreas(t)
			f.lpFile(t, "alerts@mail.bank.example", 2, areas, "Forums")
			f.lpClaim(t, "alerts@mail.bank.example", areas["Forums"])
			rec.reset()
			msg, raw := f.lpIngestRaw(t, "alerts@mail.bank.example", lpPass(tc.passFor))
			if got := rec.all(); len(got) != 1 || got[0] != "placed" {
				t.Fatalf("live arrival = %v, want [placed]", got)
			}
			ex := f.lpExplainRaw(t, msg, raw, []string{lpTrusted}, false)
			if ex.Trace.Checks == nil || !ex.Trace.Checks.Auth.DMARCPass || ex.Trace.Checks.Auth.DMARCMatch != tc.want {
				t.Fatalf("checks = %+v, want a DMARC pass matched %q", ex.Trace.Checks, tc.want)
			}
			if got := ex.Trace.Result.Label(); got != "placed" {
				t.Errorf("explain outcome = %s, want placed", got)
			}
		})
	}
}
