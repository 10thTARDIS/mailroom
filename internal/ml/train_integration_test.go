// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ml

// Integration coverage for the LEARNING loop (spec §8, mailroom#33 PR2, #842):
// TrainAndMaybePromote fits a candidate on per-message engagement labels and
// activates it ONLY when it passes the gate against the cold start
// (priority_gate.go):
//
//   - a generalising candidate that catches what the cold start misses PASSES;
//   - too few positives, or no recall gain, is REFUSED and the cold start serves;
//   - a trained model in service that the gate no longer supports is REVERTED;
//   - every decision is written to the audit log.
//
// Runs against the throwaway Postgres from scripts/integration-test.sh; skips when
// the env is absent. fakeKeys/randomKey are shared with registry_integration_test.go.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/store"
)

// fixedHour is the received-at hour every seeded message shares, so the hour:
// feature is constant and carries no signal between senders.
const fixedHour = 10

type trainFixture struct {
	super   *pgxpool.Pool
	st      *store.Store
	reg     *Registry
	trainer *Trainer
	userID  ulid.ULID
	now     time.Time
}

func newTrainFixture(t *testing.T) *trainFixture {
	t.Helper()
	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()

	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)
	st, err := store.Open(ctx, appURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	blobs, err := blob.NewFSBlobStore(t.TempDir(), randomKey(t))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	keys := fakeKeys{master: randomKey(t)}
	reg := NewRegistry(st, blobs, keys)

	userID := ulid.Make()
	if _, err := super.Exec(ctx,
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		userID.String(), "sub-"+userID.String(), userID.String()+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	f := &trainFixture{super: super, st: st, reg: reg, userID: userID,
		now: time.Date(2026, 6, 1, fixedHour, 0, 0, 0, time.UTC)}
	f.trainer = NewTrainer(st, reg)
	// Pin the pass's clock just after the seeded feedback so the whole set falls in
	// the replay window deterministically.
	f.trainer.now = func() time.Time { return f.now.Add(2 * time.Hour) }
	return f
}

// seedLabelled seeds one labelled message: a blob, a message from `sender`
// (optionally carrying List-Unsubscribe), and one feedback_events row with
// `action` at createdAt. received_at is fixed so hour: is constant. It returns
// the message id.
func (f *trainFixture) seedLabelled(t *testing.T, sender string, unsub bool, action string, createdAt time.Time) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	msgID := ulid.Make()
	hash := sha256.Sum256([]byte("m-" + f.userID.String() + "-" + msgID.String()))
	if _, err := f.super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		 VALUES ($1, $2, 10, 20, $3)`,
		f.userID.String(), hash[:], ulid.Make().String()); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	var lu any
	if unsub {
		lu = "<https://list.example.com/u>"
	}
	if _, err := f.super.Exec(ctx,
		`INSERT INTO app.messages (id, user_id, content_hash, subject, from_address,
		        received_at, size_bytes, list_unsubscribe)
		 VALUES ($1, $2, $3, 'msg', $4, $5, 10, $6)`,
		msgID.String(), f.userID.String(), hash[:], sender,
		f.now.Add(-24*time.Hour), lu); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if _, err := f.super.Exec(ctx,
		`INSERT INTO app.feedback_events (id, user_id, message_id, action, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		ulid.Make().String(), f.userID.String(), msgID.String(), action, createdAt.UTC()); err != nil {
		t.Fatalf("seed feedback: %v", err)
	}
	return msgID
}

// seedCorpus seeds `pos` engaged and `neg` dismissed messages, spread over
// twelve sender domains each (so the sender-grouped folds have senders to hold
// out). posUnsub / negUnsub set List-Unsubscribe on that class.
//
// With posUnsub && !negUnsub the corpus is the cold start's blind spot: it sinks
// every List-Unsubscribe message to 0.14 and lifts every direct stranger to
// 0.67, so it misses all the engaged mail and marks all the dismissed mail
// important. A trained model can learn the reverse, and should be promoted.
// With posUnsub == negUnsub the classes are indistinguishable: the trained model
// can learn nothing, and must not be.
func (f *trainFixture) seedCorpus(t *testing.T, pos, neg int, posUnsub, negUnsub bool) {
	t.Helper()
	base := f.now.Add(-20 * time.Hour)
	for i := 0; i < pos; i++ {
		f.seedLabelled(t, fmt.Sprintf("p%d@pos%d.test", i, i%12), posUnsub, "open", base.Add(time.Duration(i)*time.Second))
	}
	for i := 0; i < neg; i++ {
		f.seedLabelled(t, fmt.Sprintf("n%d@neg%d.test", i, i%12), negUnsub, "archive_unread", base.Add(time.Duration(pos+i)*time.Second))
	}
}

func (f *trainFixture) gateAudit(t *testing.T) []string {
	t.Helper()
	rows, err := f.super.Query(context.Background(),
		`SELECT action FROM app.audit_log WHERE user_id = $1 AND object_type = 'ml_model.priority' ORDER BY id`,
		f.userID.String())
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// A candidate that generalises across senders and catches the engaged mail the
// cold start misses PASSES the gate, is activated, and the decision is logged.
// It is the negative control for the two refusal tests below: they differ from
// it by the number of positives, or by the one feature that separates the
// classes.
func TestTrainGatePromotesAGeneralisingCandidate(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()
	f.seedCorpus(t, MinPositives+20, 200, true, false)

	res, err := f.trainer.TrainAndMaybePromote(ctx, f.userID)
	if err != nil {
		t.Fatalf("TrainAndMaybePromote: %v", err)
	}
	if !res.Promoted || res.Gate == nil || !res.Gate.Pass {
		t.Fatalf("a generalising candidate was not promoted: %s", res.Reason)
	}
	if res.Gate.Cold.AppliedPositives != 0 {
		t.Errorf("fixture check: the cold start caught %d engaged messages, want 0", res.Gate.Cold.AppliedPositives)
	}
	if v, err := f.reg.ActiveVersion(ctx, f.userID, KindPriority); err != nil || v != res.Version || v == 0 {
		t.Errorf("active version = %d (err %v), want the promoted %d", v, err, res.Version)
	}
	if got := f.gateAudit(t); len(got) != 1 || got[0] != gateActionPromote {
		t.Errorf("audit = %v, want [%s]", got, gateActionPromote)
	}
}

// The v1 shape: a candidate fitted on too few positives is refused however it
// scores, and the cold start stays in service.
func TestTrainGateRefusesFewPositives(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()
	f.seedCorpus(t, 13, 200, true, false)

	res, err := f.trainer.TrainAndMaybePromote(ctx, f.userID)
	if err != nil {
		t.Fatalf("TrainAndMaybePromote: %v", err)
	}
	if res.Promoted || res.Gate == nil || res.Gate.Pass {
		t.Fatalf("13 positives promoted: %s", res.Reason)
	}
	if !strings.Contains(res.Reason, "too few positives") {
		t.Errorf("refused for the wrong reason: %s", res.Reason)
	}
	if v, _ := f.reg.ActiveVersion(ctx, f.userID, KindPriority); v != 0 {
		t.Errorf("active version = %d, want 0 (cold start)", v)
	}
	if got := f.gateAudit(t); len(got) != 1 || got[0] != gateActionRefuse {
		t.Errorf("audit = %v, want [%s]", got, gateActionRefuse)
	}
}

// Enough positives, but nothing separates them from the negatives: the
// candidate cannot beat the cold start's recall (which, on direct mail, catches
// every engaged message), and is refused.
func TestTrainGateRefusesWorseRecall(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()
	f.seedCorpus(t, MinPositives+20, 200, false, false)

	res, err := f.trainer.TrainAndMaybePromote(ctx, f.userID)
	if err != nil {
		t.Fatalf("TrainAndMaybePromote: %v", err)
	}
	if res.Promoted || res.Gate == nil || res.Gate.Pass {
		t.Fatalf("an uninformative candidate was promoted: %s", res.Reason)
	}
	if res.Gate.Cold.AppliedPositives <= res.Gate.Cand.AppliedPositives {
		t.Errorf("fixture check: cold-start recall %d should exceed the candidate's %d",
			res.Gate.Cold.AppliedPositives, res.Gate.Cand.AppliedPositives)
	}
	if v, _ := f.reg.ActiveVersion(ctx, f.userID, KindPriority); v != 0 {
		t.Errorf("active version = %d, want 0", v)
	}
}

// A trained model in service that the gate no longer supports is REVERTED to
// the cold start, and the revert is logged with the version it replaced.
// Precondition checked: the model really was active before the pass.
func TestTrainGateRevertsAnActiveModelThatFails(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()

	stale := NewLogistic(KindPriority)
	ver, err := f.reg.Snapshot(ctx, f.userID, stale)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := f.reg.Activate(ctx, f.userID, KindPriority, ver); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if v, _ := f.reg.ActiveVersion(ctx, f.userID, KindPriority); v != ver {
		t.Fatalf("precondition: active version %d, want %d", v, ver)
	}
	f.seedCorpus(t, 13, 200, true, false)

	res, err := f.trainer.TrainAndMaybePromote(ctx, f.userID)
	if err != nil {
		t.Fatalf("TrainAndMaybePromote: %v", err)
	}
	if !res.Reverted || res.Version != 0 {
		t.Fatalf("not reverted: %+v", res)
	}
	m, v, err := f.reg.ActiveModel(ctx, f.userID, KindPriority)
	if err != nil || v != 0 {
		t.Fatalf("ActiveModel after revert = v%d (err %v), want the cold start at v0", v, err)
	}
	if _, ok := m.(coldStartModel); !ok {
		t.Errorf("ActiveModel after revert is %T, want the cold start", m)
	}
	// Nothing deleted: the reverted version is still in the lineage.
	vs, err := f.reg.ListVersions(ctx, f.userID, KindPriority)
	if err != nil || len(vs) != 1 || vs[0].Active {
		t.Errorf("lineage after revert = %+v (err %v), want the one inactive version", vs, err)
	}
	if got := f.gateAudit(t); len(got) != 1 || got[0] != gateActionRevert {
		t.Errorf("audit = %v, want [%s]", got, gateActionRevert)
	}
}

// The data floor still skips the fit entirely, and is logged as a refusal.
func TestTrainSkipsBelowMinSamples(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()
	f.seedCorpus(t, 5, 5, true, false)

	res, err := f.trainer.TrainAndMaybePromote(ctx, f.userID)
	if err != nil {
		t.Fatalf("TrainAndMaybePromote: %v", err)
	}
	if res.Trained || res.Promoted || res.Samples != 10 {
		t.Errorf("expected a skip over 10 samples: %+v", res)
	}
}

// mark_read_no_open rows from before #842 are not labels any more: a corpus of
// nothing else is "no data", not 300 negatives.
func TestTrainIgnoresRetiredMarkReadNoOpen(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()
	base := f.now.Add(-10 * time.Hour)
	for i := 0; i < 300; i++ {
		f.seedLabelled(t, fmt.Sprintf("r%d@read%d.test", i, i%12), false, "mark_read_no_open", base.Add(time.Duration(i)*time.Second))
	}
	res, err := f.trainer.TrainAndMaybePromote(ctx, f.userID)
	if err != nil {
		t.Fatalf("TrainAndMaybePromote: %v", err)
	}
	if res.Samples != 0 || res.Negatives != 0 {
		t.Errorf("retired events became samples: %+v", res)
	}
}

// ── The label-model promotion gate (#169, #220) ──────────────────────────────
//
// The unit tests in promotion_gate_test.go prove labelPromotionGate decides
// correctly. They prove NOTHING about whether trainOneLabel consults it — which
// is the failure this project keeps writing rules about, a mechanism that runs
// without erroring while deciding nothing. These two tests cover the wiring from
// both sides, and BOTH directions are needed: a gate that only ever refuses is
// indistinguishable from `return false`.

// seedLabelledMessage inserts one message from `sender` with `subject`, and
// hand-applies `labelID` when it is non-empty (applied_by='user', the only
// provenance the trainer accepts as evidence).
func (f *trainFixture) seedLabelledMessage(t *testing.T, salt, sender, subject string, labelID ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	hash := sha256.Sum256([]byte("lm-" + f.userID.String() + "-" + salt))
	msgID := ulid.Make()
	if _, err := f.super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		 VALUES ($1, $2, 10, 20, $3)`,
		f.userID.String(), hash[:], ulid.Make().String()); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := f.super.Exec(ctx,
		`INSERT INTO app.messages (id, user_id, content_hash, subject, from_address,
		        received_at, size_bytes)
		 VALUES ($1, $2, $3, $4, $5, $6, 10)`,
		msgID.String(), f.userID.String(), hash[:], subject, sender,
		f.now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if labelID == (ulid.ULID{}) {
		return
	}
	if _, err := f.super.Exec(ctx,
		`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
		 VALUES ($1, $2, $3, 'user')`,
		f.userID.String(), msgID.String(), labelID.String()); err != nil {
		t.Fatalf("seed message_label: %v", err)
	}
}

func (f *trainFixture) seedLabel(t *testing.T, name string) ulid.ULID {
	t.Helper()
	id := ulid.Make()
	if _, err := f.super.Exec(context.Background(),
		`INSERT INTO app.labels (id, user_id, name) VALUES ($1, $2, $3)`,
		id.String(), f.userID.String(), name); err != nil {
		t.Fatalf("seed label: %v", err)
	}
	return id
}

// A label whose positives are separable by SUBJECT VOCABULARY — signal that
// survives holding the sender out, since identity features are stripped — over
// enough senders to satisfy the floor, promotes. This is the test that proves
// the gate is a gate rather than a refusal.
func TestLabelGatePromotesSeparableEvidence(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()
	labelID := f.seedLabel(t, "Receipts")

	// 20 positive senders x 3 messages, and 20 unrelated senders x 3, with
	// disjoint subject vocabularies. Sender identity is useless to the model by
	// construction (labelModelFeatures strips it), so the only thing left to
	// learn is the vocabulary — which is exactly the question the holdout asks.
	for s := 0; s < 20; s++ {
		for m := 0; m < 3; m++ {
			f.seedLabelledMessage(t,
				fmt.Sprintf("pos-%d-%d", s, m),
				fmt.Sprintf("billing@shop%d.test", s),
				"Your order confirmation and receipt", labelID)
			f.seedLabelledMessage(t,
				fmt.Sprintf("neg-%d-%d", s, m),
				fmt.Sprintf("news@zine%d.test", s),
				"Weekend sale discount inside", ulid.ULID{})
		}
	}

	results, err := f.trainer.TrainLabelModels(ctx, f.userID, false)
	if err != nil {
		t.Fatalf("TrainLabelModels: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d label results, want 1", len(results))
	}
	r := results[0]
	if !r.Promoted {
		t.Fatalf("separable evidence over 20 senders was refused: gate=%+v reason=%q", r.Gate, r.Reason)
	}
	if !r.Gate.Pass {
		t.Errorf("Promoted=true with Gate.Pass=false — the promotion did not come from the gate")
	}
	if r.Gate.PrecisionLCB < r.Gate.Target {
		t.Errorf("promoted with PrecisionLCB %.3f below target %.2f", r.Gate.PrecisionLCB, r.Gate.Target)
	}
	if _, version, err := f.reg.ActiveModel(ctx, f.userID, KindLabel(labelID)); err != nil || version == 0 {
		t.Errorf("no active model after promotion (version=%d, err=%v)", version, err)
	}
}

// The same wiring from the other side: evidence that is NOT separable once the
// sender is held out must be refused, and refused BY THE GATE — with its reason,
// not a generic one — and must leave no active model behind.
func TestLabelGateRefusesUnseparableEvidence(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()
	labelID := f.seedLabel(t, "Miscellany")

	// Identical vocabulary on both sides: the ONLY thing distinguishing a
	// positive from a negative is which sender it came from, and that is the
	// feature the label model is denied. This is the shape #169 measured on a
	// real corpus, reproduced in miniature.
	for s := 0; s < 20; s++ {
		for m := 0; m < 3; m++ {
			f.seedLabelledMessage(t,
				fmt.Sprintf("mpos-%d-%d", s, m),
				fmt.Sprintf("a@src%d.test", s),
				"Weekly update for you", labelID)
			f.seedLabelledMessage(t,
				fmt.Sprintf("mneg-%d-%d", s, m),
				fmt.Sprintf("b@other%d.test", s),
				"Weekly update for you", ulid.ULID{})
		}
	}

	results, err := f.trainer.TrainLabelModels(ctx, f.userID, false)
	if err != nil {
		t.Fatalf("TrainLabelModels: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d label results, want 1", len(results))
	}
	r := results[0]
	if r.Promoted {
		t.Fatalf("promoted a model with no sender-independent signal: gate=%+v", r.Gate)
	}
	if r.Gate.Pass {
		t.Errorf("Gate.Pass=true while Promoted=false — the refusal did not come from the gate")
	}
	if r.Reason != r.Gate.Reason {
		t.Errorf("reported reason %q is not the gate's %q — the report and the decision have drifted apart",
			r.Reason, r.Gate.Reason)
	}
	if _, version, _ := f.reg.ActiveModel(ctx, f.userID, KindLabel(labelID)); version != 0 {
		t.Errorf("a model became active despite refusal (version=%d)", version)
	}
}
