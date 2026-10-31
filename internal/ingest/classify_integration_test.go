// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// Integration coverage for the per-label classifier's SUGGEST-ONLY invariant
// (spec §8, #169, classifyMessage's own "THE MODEL SUGGESTS; IT DOES NOT
// APPLY" comment): a verdict from the model — confident or abstaining — must
// become an app.label_suggestions row, and must NEVER write app.message_labels
// directly. That guarantee had NO test until this file: classifyMessage
// changed from applying the label outright to only suggesting it, and the
// entire suite stayed green, because a concrete *ml.Classifier cannot be made
// to return a chosen verdict without training a real model first. The
// LabelClassifier interface (ingest.go) exists precisely so a fake can hand
// back a fixed verdict and this can be asserted directly. Runs against the
// throwaway Postgres from scripts/integration-test.sh; skips when absent.

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// fakeClassifier hands back a fixed, caller-supplied verdict set for every
// message it is asked about. The point is not to model a real classifier — it
// is to make a chosen LabelVerdict (Apply/Abstain/Confidence) reproducible in
// a test, which no real *ml.Classifier can offer without a trained model.
type fakeClassifier struct {
	verdicts map[ulid.ULID]ml.LabelVerdict
}

func (f fakeClassifier) Classify(_ context.Context, _ ulid.ULID, _ []ulid.ULID, _ ml.MessageFacts) map[ulid.ULID]ml.LabelVerdict {
	return f.verdicts
}

// giveLabelAModel makes labelID have an ACTIVE classifier, which is the gate
// classifyMessage checks (tx.LabelsWithModels — "SELECT model_kind FROM
// app.ml_models WHERE active") before it ever consults the classifier at all.
// Without this, ingesting through the fake classifier below would exercise
// nothing: classifyMessage returns before Classify is called.
//
// This goes through the SAME registry writes a real TrainLabeler + promotion
// would make — InsertModelVersion then ActivateModelVersion — rather than a
// raw INSERT into app.ml_models via f.super. Those are exported UserTx methods
// built for exactly this (see ml_models.go), so there is no need to reach
// around the store layer; the artifact hash and bytes behind it are never
// read by classifyMessage (that is the fake classifier's whole job), so a
// throwaway hash is enough to make the row legitimate.
func giveLabelAModel(t *testing.T, f *fixture, labelID ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	hash := sha256.Sum256([]byte("classify_integration_test-" + labelID.String()))
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		version, err := tx.InsertModelVersion(ctx, ml.KindLabel(labelID), hash[:], false)
		if err != nil {
			return err
		}
		return tx.ActivateModelVersion(ctx, ml.KindLabel(labelID), version)
	}); err != nil {
		t.Fatalf("give label a model: %v", err)
	}
}

// TestClassifierSuggestsButNeverApplies pins classifyMessage's central
// invariant across both verdict shapes that produce a write: a confident
// Apply becomes a suggestion (never a label), and a confident rejection
// (neither Apply nor Abstain) writes nothing at all — no label, no question.
func TestClassifierSuggestsButNeverApplies(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var labelID ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var err error
		labelID, err = tx.CreateLabel(ctx, store.LabelRow{Name: "Promotions-" + store.NewULID().String()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	giveLabelAModel(t, f, labelID)

	t.Run("confident apply becomes a suggestion, never a label", func(t *testing.T) {
		fc := fakeClassifier{verdicts: map[ulid.ULID]ml.LabelVerdict{
			labelID: {Confidence: 0.97, Apply: true},
		}}
		// A second Ingestor sharing the fixture's store and blob store, differing
		// only in carrying the fake classifier — WithClassifier is the one thing
		// that turns categorisation on (ingest.go), so f.ing itself (built by
		// newFixture with no options) is deliberately left uninvolved here.
		ing := New(f.st, f.ing.blobs, WithClassifier(fc))

		raw := rawMessage("Big Sale", "classify-apply-1@example.com", nil)
		res, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 901, nil))
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}

		// THE LOAD-BEARING ASSERTION. A 0.97-confidence Apply verdict is exactly
		// the case a pre-#169 classifyMessage would have written straight onto
		// the message with LabelByML attribution — and exactly the regression
		// that shipped once with the whole suite green.
		if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
			res.MessageID.String(), labelID.String()); n != 0 {
			t.Errorf("message_labels rows for (msg,label) = %d, want 0 — the classifier applied the label directly", n)
		}
		// The confident verdict must still surface as a question, not vanish:
		// exactly one suggestion row.
		if n := f.scalar(t, `SELECT count(*) FROM app.label_suggestions WHERE message_id=$1 AND label_id=$2`,
			res.MessageID.String(), labelID.String()); n != 1 {
			t.Fatalf("label_suggestions rows for (msg,label) = %d, want 1", n)
		}
		// The stored confidence is the model's own — the review surface ranks
		// suggestions by it, so losing precision here would silently reorder
		// the needs-a-category queue.
		var got float64
		if err := f.super.QueryRow(ctx,
			`SELECT confidence FROM app.label_suggestions WHERE message_id=$1 AND label_id=$2`,
			res.MessageID.String(), labelID.String()).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != 0.97 {
			t.Errorf("stored confidence = %v, want 0.97 (the verdict's own value)", got)
		}
	})

	t.Run("confident rejection writes nothing at all", func(t *testing.T) {
		fc := fakeClassifier{verdicts: map[ulid.ULID]ml.LabelVerdict{
			// Apply: false, Abstain: false is the third verdict shape — "I am
			// sure this does NOT belong" — which classifyMessage's own comment
			// says must produce neither a label nor a question: queuing "I am
			// sure this is not Promotions" would bury real uncertainty under
			// everything the model got right.
			labelID: {Confidence: 0.03, Apply: false, Abstain: false},
		}}
		ing := New(f.st, f.ing.blobs, WithClassifier(fc))

		raw := rawMessage("Not a sale", "classify-reject-1@example.com", nil)
		res, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 902, nil))
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}

		if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
			res.MessageID.String(), labelID.String()); n != 0 {
			t.Errorf("message_labels rows for (msg,label) = %d, want 0", n)
		}
		if n := f.scalar(t, `SELECT count(*) FROM app.label_suggestions WHERE message_id=$1 AND label_id=$2`,
			res.MessageID.String(), labelID.String()); n != 0 {
			t.Errorf("label_suggestions rows for (msg,label) = %d, want 0 — a confident rejection must not queue a question", n)
		}
	})
}
