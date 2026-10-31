// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
)

// The store surface for the per-label classifier (spec §8, #169): the training
// evidence it learns from, and the queue its abstentions land in.

// LabelTrainingRow is one message in a label's training set: the message to
// build features from, and whether the USER put this label on it.
type LabelTrainingRow struct {
	MessageID ulid.ULID
	Positive  bool
	// SenderDomain is the From domain, lowercased, or "" when the message has
	// none. It is NOT a feature — the model never sees it from here — it is the
	// GROUPING KEY the holdout split needs (#169).
	//
	// Without it the only split available is by class, which lets the same sender
	// sit on both sides: a model that has memorised `deals@shop.example → Promotions`
	// then scores near-perfectly on a holdout full of the senders it trained on,
	// and the promotion gate reads that as generalisation. It is not, and the
	// difference decides whether the model is allowed to act.
	SenderDomain string
}

// LabelTrainingRows returns the evidence for one label's classifier: every
// message the user hand-labelled with it (positive), plus a bounded sample of
// their other recent mail (negative).
//
// POSITIVES ARE HAND-APPLIED ONLY. A label a rule or this very model applied is
// an automation's own output; training on it closes a loop where the model's
// mistakes become its evidence, and confidence grows while accuracy does not.
// The applied_by provenance (migration 00017) is what makes this expressible —
// before it, every label claimed to be the user's.
//
// NEGATIVES COME FROM TWO POOLS, interleaved half-and-half up to the bound
// (either pool's shortfall is filled from the other):
//
//   - HARD negatives: mail the user hand-labelled with some OTHER label. These
//     are uncontaminated by construction — the user looked at the message,
//     made a filing decision, and did not choose this label. (Labels are not
//     mutually exclusive, so this is strong evidence rather than proof, but it
//     is the cleanest negative signal the corpus has.)
//   - UNLABELLED mail, sampled uniformly across the WHOLE corpus. This pool is
//     the approximation worth naming: some of it is mail the user WOULD label
//     this way and simply has not got to. It stays in the mix anyway, because
//     hand-labelled mail alone would train the model on a world where every
//     message has a category, which is not the world; the abstention band is
//     what absorbs the resulting noise.
//
// The unlabelled sample used to be the N MOST RECENT messages while positives
// span all time, which was two defects in one: a time skew (a sender active
// only in the past could never be a negative, so "old" itself became weak
// positive evidence), and maximal contamination (in a mostly-bulk mailbox the
// recent unlabelled window is dense with would-be positives). Sampling on
// md5(id) instead is uniform across the corpus AND deterministic — the same
// evidence yields the same sample, so a training pass stays reproducible,
// which random() would break.
//
// Negatives EXCLUDE anything carrying the label by any provenance, so a message
// this model already labelled is never fed back as a counter-example to itself.
func (tx *UserTx) LabelTrainingRows(ctx context.Context, labelID ulid.ULID, negativeLimit int) ([]LabelTrainingRow, error) {
	if negativeLimit <= 0 {
		negativeLimit = 500
	}
	// The window function interleaves the two negative pools: rows are ranked
	// WITHIN their pool by md5(id), then consumed in rank order across pools —
	// rank 1 hard, rank 1 unlabelled, rank 2 hard, ... — so the LIMIT takes
	// half from each while either pool lasts, with no tuning knob to pick. The
	// outer ORDER BY makes the returned row order itself deterministic (a bare
	// UNION ALL guarantees nothing): the trainer shuffles per epoch, so order
	// carries no training bias, but fold assignment and the naive-split
	// diagnostic both read this order and must be reproducible from the
	// evidence alone.
	rows, err := tx.tx.Query(ctx, `
		  SELECT m.id, true, COALESCE(m.from_address, '')
		    FROM app.message_labels ml
		    JOIN app.messages m ON m.id = ml.message_id AND m.user_id = ml.user_id
		   WHERE ml.user_id = $1 AND ml.label_id = $2 AND ml.applied_by = $3
		UNION ALL
		  SELECT id, false, from_address FROM (
		    SELECT m.id,
		           COALESCE(m.from_address, '') AS from_address,
		           row_number() OVER (
		             PARTITION BY EXISTS (
		               SELECT 1 FROM app.message_labels other
		                WHERE other.user_id = m.user_id
		                  AND other.message_id = m.id
		                  AND other.applied_by = $3)
		             ORDER BY md5(m.id)
		           ) AS pick
		      FROM app.messages m
		     WHERE m.user_id = $1
		       AND m.trashed_at IS NULL
		       AND NOT EXISTS (
		             SELECT 1 FROM app.message_labels ml2
		              WHERE ml2.user_id = m.user_id
						AND ml2.message_id = m.id
						AND ml2.label_id = $2)
		     ORDER BY pick, md5(m.id)
		     LIMIT $4
		  ) sample
		ORDER BY 2 DESC, 1`,
		tx.userID.String(), labelID.String(), LabelByUser, negativeLimit)
	if err != nil {
		return nil, fmt.Errorf("store: label training rows: %w", err)
	}
	defer rows.Close()

	var out []LabelTrainingRow
	for rows.Next() {
		var (
			id   string
			pos  bool
			from string
		)
		if err := rows.Scan(&id, &pos, &from); err != nil {
			return nil, fmt.Errorf("store: scan label training row: %w", err)
		}
		mid, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: label training row id %q: %w", id, err)
		}
		out = append(out, LabelTrainingRow{
			MessageID:    mid,
			Positive:     pos,
			SenderDomain: AddressDomain(NormalizeAddress(from)),
		})
	}
	return out, rows.Err()
}

// LabelsWithEnoughEvidence returns the labels that have at least `min`
// hand-applied messages — the ones worth training a classifier for at all.
func (tx *UserTx) LabelsWithEnoughEvidence(ctx context.Context, min int) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT label_id FROM app.message_labels
		 WHERE user_id = $1 AND applied_by = $2
		 GROUP BY label_id
		HAVING count(*) >= $3`,
		tx.userID.String(), LabelByUser, min)
	if err != nil {
		return nil, fmt.Errorf("store: labels with evidence: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan label id: %w", err)
		}
		lid, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: label id %q: %w", id, err)
		}
		out = append(out, lid)
	}
	return out, rows.Err()
}

// LabelsWithModels returns the labels that have an ACTIVE trained classifier —
// the only ones worth asking about on the delivery path.
//
// Reading ml_models directly (rather than listing every label and probing the
// registry for each) keeps a message's arrival to one small query instead of one
// blob read per label the user has ever created.
func (tx *UserTx) LabelsWithModels(ctx context.Context) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT model_kind FROM app.ml_models
		 WHERE user_id = $1 AND active AND model_kind LIKE 'label:%'`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: labels with models: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return nil, fmt.Errorf("store: scan model kind: %w", err)
		}
		id, err := ulid.Parse(strings.TrimPrefix(kind, "label:"))
		if err != nil {
			// A malformed kind is not worth failing an ingest over; skip it.
			continue
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Suggestion sources — app.label_suggestions.source (migration 00036, #242).
// WHICH system asked the question, because `confidence` means a different thing
// for each and the client has to know which reading applies.
const (
	// SuggestionSourceModel: the per-label classifier, via ingest's
	// classifyMessage. confidence is the model's probability and may be rendered
	// as one.
	SuggestionSourceModel = "model"
	// SuggestionSourceRule: an induced rule's `suggestLabel` action, via
	// applyRules. confidence is rules.EvidenceConfidence — an ORDERING RANK over
	// the user's own filings, NOT a probability, and must never be rendered as a
	// percentage.
	SuggestionSourceRule = "rule"
)

// LabelSuggestion is one stored abstention: "this might be Promotions, and I am
// not sure enough to say so".
type LabelSuggestion struct {
	MessageID    ulid.ULID
	LabelID      ulid.ULID
	Confidence   float64
	ModelVersion int
	// Reasons is the RAW app.label_suggestions.reasons JSON (#176): a
	// machine-readable []ml.FeatureContribution, byte-identical in shape to
	// EmailView.TopFeatures, which api decodes and maps through ml.FeatureLabel.
	//
	// nil means NO RECORD of why — a suggestion written before migration 00019,
	// or by a model that could not explain itself. It is deliberately distinct
	// from []byte("[]"), which is a model that explained and had nothing above
	// the noise floor. The surface says different things about the two.
	Reasons []byte
	// Source is which system produced this suggestion — one of the
	// SuggestionSource* constants (#242). It is what makes Confidence readable:
	// see the constants, and internal/rules.EvidenceConfidence.
	//
	// An EMPTY Source on the way IN defaults to SuggestionSourceModel in
	// PutLabelSuggestion, so the pre-existing classifier caller is unchanged in
	// behaviour by this field appearing. On the way OUT it is always populated —
	// the column is NOT NULL with the same default.
	Source string
}

// PutLabelSuggestion records (or refreshes) one abstention.
//
// It appends NO change_log row, and that is deliberate: a suggestion is not a
// JMAP-visible mutation of the message. Nothing about the Email changes — not
// its keywords, not its mailboxes, not what any other client sees. It is a
// question queued for a human, and the ANSWER, when it comes, goes through the
// ordinary label write path and appends its row there.
func (tx *UserTx) PutLabelSuggestion(ctx context.Context, s LabelSuggestion) error {
	// A nil Reasons stores SQL NULL rather than the JSON literal `null`, so
	// "no record of why" reads back as nil and not as a four-byte payload that
	// decodes to an empty slice — the distinction LabelSuggestion.Reasons
	// documents. (jsonb rejects a raw []byte("") outright, which is the failure
	// this also avoids.)
	var reasons any
	if len(s.Reasons) > 0 {
		reasons = s.Reasons
	}
	// An unset Source is the CLASSIFIER (#242). The model path predates the column
	// by two milestones and every row it ever wrote was model-derived, so defaulting
	// here means adding this field changed nothing for the caller that was already
	// there — and it matches the column's own DEFAULT, rather than relying on the
	// database to fill a gap the Go value could equally have left as ''. The CHECK
	// constraint would reject '' outright, which is a failed ingest write for a
	// caller that simply did not know about a new field.
	source := s.Source
	if source == "" {
		source = SuggestionSourceModel
	}
	// ⚠ A MODEL ROW MUST NOT OVERWRITE A RULE ROW (#242). Until #242 this table had
	// exactly one writer, so the upsert could overwrite unconditionally and the
	// question never arose. It has two now, they collide on this primary key, and
	// at ingest the collision is ORDERED: applyRules runs before classifyMessage
	// (internal/ingest/ingest.go), so an unguarded upsert would mean the model
	// silently replacing the rule's row on every message where both fire.
	//
	// That is backwards on every axis #242 weighed the two on. The rule side was
	// MEASURED at zero collateral across 27 candidates and 66 rules (#168/#173);
	// the model side was measured at 0.20–0.60 precision at the apply threshold
	// and is the reason #169 refuses to promote one at all (#220). The rule
	// explains itself as a condition the user can read and correct with
	// Rule/tighten; the model explains itself as feature weights and can only be
	// retrained. Letting the second overwrite the first would discard the better
	// suggestion, its explanation and its correctability, and do it invisibly.
	//
	// So: a row may be updated by the SAME source (either side refreshing its own
	// answer), or by a RULE arriving over a model row (the backfill reaching mail
	// a model had already spoken for — the rule still wins). A model arriving over
	// a rule row is the one combination that does nothing. DO UPDATE ... WHERE
	// failing is a silent no-op rather than an error, which is what this path
	// wants: it is called from a fail-soft ingest that must never lose mail.
	//
	// HONEST LIMIT: nothing clears suggestions when the rule behind them is
	// deleted, so a stale rule row can outlive its rule and keep blocking a model
	// row for that (message, label). That gap predates this change — deleting a
	// rule has never swept its suggestions — and it is currently unreachable
	// besides, since no model is promoted. It is called out here rather than left
	// for someone to find, and belongs with rule deletion, not with this write.
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO app.label_suggestions
		       (user_id, message_id, label_id, confidence, model_version, reasons, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, message_id, label_id)
		DO UPDATE SET confidence = EXCLUDED.confidence,
		              model_version = EXCLUDED.model_version,
		              reasons = EXCLUDED.reasons,
		              source = EXCLUDED.source,
		              created_at = now()
		WHERE label_suggestions.source <> 'rule'
		   OR EXCLUDED.source = 'rule'`,
		tx.userID.String(), s.MessageID.String(), s.LabelID.String(),
		s.Confidence, s.ModelVersion, reasons, source)
	if err != nil {
		return fmt.Errorf("store: put label suggestion: %w", err)
	}
	return nil
}

// PutRuleSuggestion writes a RULE-derived label suggestion: the single funnel for
// both the ingest path (applyRuleAction's suggestLabel case) and the admin
// backfill that fills the surface for mail which arrived before the rule existed.
//
// ⚠ IT IS ONE FUNCTION BECAUSE TWO WOULD DRIFT. `cmd/mailroom-admin/applyrules.go`
// states the doctrine for exactly this shape of feature — a CLI is "a front door
// onto the SAME store functions, never a second write path" — and the drift here
// would be silent and user-visible: the backfill and ingest would each derive a
// confidence and each render a reasons payload, and the queue would show two
// different explanations, or two different sort keys, for the same rule depending
// on when the message happened to arrive. Everything variable about a rule-sourced
// suggestion (the rank, the phrasing, the source, the model_version) is decided
// HERE and nowhere else. Callers supply only what they alone know: which message,
// which label (resolved differently by each — see applyRuleAction), the live
// condition tree, and the induced evidence count.
//
// ⚠ IT WRITES NOTHING FOR A MESSAGE THAT ALREADY CARRIES A LABEL, and that guard
// is why this is not a two-line wrapper (#242 follow-up).
//
// A suggestion is a QUESTION — "which category is this?" — and asking it about
// mail that is already filed does not merely waste a row: it UN-FILES the message
// in the user's view. The needs-a-category predicate (EmailQuery.NeedsCategory,
// jmap_email.go) admits a message on either of two arms:
//
//	EXISTS (label_suggestions …)  OR  NOT EXISTS (message_labels …)
//
// so a suggestion written on top of a rule-applied label satisfies the FIRST arm
// and drags an already-categorised message into the review queue. MEASURED on the
// a real mailbox before this guard existed: of the suggestions
// `suggest-rules --backfill` would have written, a fair share landed on mail a rule had
// already filed, and the queue would have grown — a review surface
// GROWING by asking about mail it had already answered.
//
// "ALREADY CARRIES A LABEL" MEANS ANY LABEL, whoever applied it, because that is
// exactly what the predicate above asks — it carries no applied_by test. It
// deliberately does NOT mean "the user filed it by hand", which is a different
// question with a different right answer elsewhere: induction reads only
// hand-applied labels as evidence (RulePreviewCandidate.UserLabelIDs argues that
// at length), and reusing THAT definition here is precisely the conflation that
// produced the 36.
//
// THE GUARD SITS ON THIS FUNCTION AND NOT ON PutLabelSuggestion, deliberately.
// This is the single funnel for RULE-derived suggestions — ingest's suggestLabel
// arm and the admin backfill both arrive here — so one check covers both write
// paths and no third can forget it. PutLabelSuggestion is the MODEL path: it is
// outside #242, it is unreachable today (no promoted model — #169/#220), and
// whether a classifier should also stay quiet about filed mail is its own
// question with its own evidence, which nobody has gathered. Quietly changing the
// model path's population while fixing the rule path's would be an untested
// behaviour change smuggled in under a bug fix.
//
// AT INGEST THE ORDERING IS WHAT MAKES THIS WORK, and it is guaranteed by
// ApplyRuleActions rather than by priority. That function applies every
// suggestLabel action in a SECOND pass, after all other actions of the same
// delivery, so a matching addLabel has already INSERTed its row in THIS
// transaction by the time this runs and this SELECT sees it — a transaction sees
// its own writes. A message that earns both a label and a suggestion from one
// delivery keeps the label and drops the question.
//
// ⚠ PRIORITY IS NOT WHAT PROTECTS THIS, though it looks like it does. `suggest-
// rules` creates its rules at priority 200 against an ordinary rule's default of
// 100, so induced suggestions would sort after their labels anyway — but
// `--priority` is operator-settable, and a suggestLabel rule written BY HAND
// through Rule/set takes app.rules.priority's DEFAULT of 100, the same as an
// addLabel rule, at which point RulesForEval's `ORDER BY priority, id` leaves
// CREATION ORDER deciding whether this guard sees the label. That was measured,
// not imagined: reversing the two priorities in a test makes the suggestion get
// written. The two-pass split in ApplyRuleActions is what turns the guarantee
// from coincidence into structure; do not reintroduce a single-pass loop there.
//
// A PRE-CHECK READ RATHER THAN A `WHERE NOT EXISTS` ON THE INSERT: the insert
// belongs to PutLabelSuggestion, which must not learn about this rule (above), so
// folding the guard into it means either a duplicated INSERT here — a second copy
// of a wire format, the thing the funnel exists to prevent — or a boolean
// parameter threaded through the model path, whose wrong value would be silent.
// One EXISTS on the (user_id, message_id) prefix of message_labels' primary key
// is the cheaper thing to get right.
//
// TWO HONEST LIMITS, written down rather than left to be found:
//   - IT DOES NOT SWEEP. A suggestion written while the message was uncategorised
//     survives a label arriving later, unless a HUMAN applies it — that path calls
//     ClearLabelSuggestions. A rule labelling a message after the fact (apply-rules
//     over stored mail) leaves the old question standing. That repair belongs with
//     the label write, not with this one.
//   - IT IS READ COMMITTED, so a label committed by a CONCURRENT transaction
//     between this SELECT and the INSERT is invisible here. The cost is one stale
//     question on a message filed in the same instant, which any human answer
//     clears. Locking the message to close that is not worth doing on a fail-soft
//     ingest path.
//
// It appends NO change_log row, inheriting PutLabelSuggestion's reasoning: a
// suggestion is not a JMAP-visible mutation of the Email, and at ingest it rides
// the message's own OpCreated row. The BACKFILL caller is the case where that
// reasoning does not obviously carry — those messages were created long ago — and
// it is still right: the backfill is an operator action over stored mail, and the
// alternative (a change_log row per backfilled message) would push a sync storm
// of Email updates to every connected client for rows whose Email properties did
// not change except for suggestedLabels. If that becomes the wrong trade, it is
// the backfill's decision to make explicitly, not a side effect of this write.
//
// ModelVersion is 0, which is the column's default and reads as "no model
// involved". It is not a version number withheld; there is no model in this path.
//
// KNOWN INTERACTION, written down rather than designed around: the primary key is
// (user_id, message_id, label_id) and PutLabelSuggestion upserts, so LAST WRITER
// WINS when a rule and a model both propose the SAME label for the SAME message.
// At ingest applyRules runs before classifyMessage (ingest.go), so in that
// collision the model's row is what persists. Nothing is fabricated by that — the
// stored source, confidence and reasons all describe the writer that won, which is
// what `source` is for — and the collision needs a label that has BOTH a promoted
// model and a suggestLabel rule, which #169's promotion verdict makes rare. It is
// noted here because "which of two producers owns this row" is not obvious from
// either call site, and if it ever needs a policy (rules outranking models, say)
// this is where it goes.
func (tx *UserTx) PutRuleSuggestion(ctx context.Context, msgID, labelID ulid.ULID, cond rules.Condition, evidence int) error {
	// THE GUARD — see the ⚠ paragraph above for why "any label" and why here.
	var filed bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.message_labels
		                WHERE user_id = $1 AND message_id = $2)`,
		tx.userID.String(), msgID.String()).Scan(&filed); err != nil {
		return fmt.Errorf("store: rule suggestion filed check: %w", err)
	}
	if filed {
		// A SILENT SKIP, not an error. Both callers are bulk: ingest is fail-soft
		// and must never turn "this message was already filed" into a logged rule
		// failure on every delivery, and the backfill walks thousands of messages
		// for which this is the ordinary, expected answer. The population is
		// reported by the caller that can see the whole scan
		// (cmd/mailroom-admin/suggestrules.go counts it per label), which is the
		// only place a count of skips means anything.
		return nil
	}
	reasons, err := ruleSuggestionReasons(cond, evidence)
	if err != nil {
		return err
	}
	return tx.PutLabelSuggestion(ctx, LabelSuggestion{
		MessageID: msgID,
		LabelID:   labelID,
		// An ORDERING RANK, not a probability — see rules.EvidenceConfidence, and
		// the `source` column that exists so the client never renders it as one.
		Confidence:   rules.EvidenceConfidence(evidence),
		ModelVersion: 0,
		Reasons:      reasons,
		Source:       SuggestionSourceRule,
	})
}

// suggestionReason mirrors ml.FeatureContribution's JSON shape for the reasons
// payload.
//
// ⚠ IT IS A COPY BECAUSE internal/store CANNOT IMPORT internal/ml: internal/ml
// imports internal/store (features.go, registry.go, train.go), so the reverse
// edge is an import cycle the compiler refuses. This is the same reason
// UpsertTriageScore takes raw []byte rather than a typed slice — the store has
// always spoken this payload as JSON and never as an ml type.
//
// The drift risk that creates is closed by a TEST, not by this comment:
// rule_suggestion_shape_test.go (an external `store_test` package, which CAN
// import both) compares the two struct types field by field — name, type and json
// tag — so adding, renaming or retagging a field on ml.FeatureContribution turns
// it red. Without that test this struct is a second definition of a wire format,
// which is exactly the kind of thing that agrees for a year and then quietly does
// not.
//
// Field ORDER matters and matches ml.FeatureContribution: encoding/json emits in
// declaration order, and the test asserts byte-identical output.
type suggestionReason struct {
	Key    string  `json:"key"`
	Weight float64 `json:"weight"`
	Text   string  `json:"text,omitempty"`
}

// ruleSuggestionReasons renders a rule's live conditions as the stored reasons
// payload: one entry per conjunct, plus a trailing entry naming the evidence.
//
// ⚠ EVERY WEIGHT IS 0, AND THAT IS A DELIBERATE STATEMENT RATHER THAN A GAP. A
// model's contribution weights mean something specific — each is that feature's
// signed share of the logit, and they sum to the score. An AND rule has no such
// decomposition: its conjuncts are each INDIVIDUALLY NECESSARY, the rule fires
// only when all of them hold, and removing any one of them changes the rule
// rather than reducing a total. There is no "how much did `alias is sub@`
// contribute" to answer, so any number written here would be invented — and an
// invented weight is worse than none, because the surface would sort and size the
// reasons by it as though it had been measured. 0 across the board is the honest
// encoding of "these are equal, necessary conditions"; `Text` carries the whole
// meaning.
//
// Key is empty for the same reason: these are literal conditions, not feature
// keys, and Text overrides FeatureLabel(Key) at render time
// (ml.FeatureContribution.Text).
//
// The evidence entry is appended ONLY when there is evidence to name. A
// hand-written suggestLabel rule carries no count (Action.Evidence is advisory
// and unvalidated), and "backed by 0 messages you filed under this label" would
// be a sentence asserting a measurement that was never taken. Silence is the
// honest output there; the conjuncts still explain the rule.
//
// It returns a NON-NIL payload whenever the rule describes itself at all, so the
// row stores `[...]` rather than SQL NULL. NULL on this column means "no record
// of why" (migration 00019), which would be false here — a rule always knows why
// it fired.
func ruleSuggestionReasons(cond rules.Condition, evidence int) ([]byte, error) {
	phrases := rules.DescribeCondition(cond)
	out := make([]suggestionReason, 0, len(phrases)+1)
	for _, p := range phrases {
		out = append(out, suggestionReason{Text: p})
	}
	if evidence >= 1 {
		out = append(out, suggestionReason{Text: evidencePhrase(evidence)})
	}
	if len(out) == 0 {
		// A rule whose conditions render to nothing AND that carries no evidence
		// count. Only an empty/malformed condition tree reaches this, and such a
		// tree matches nothing, so in practice no suggestion can have been produced
		// by one. nil stores SQL NULL — "no record of why" — which is the accurate
		// thing to say about a rule that cannot describe itself.
		return nil, nil
	}
	payload, err := json.Marshal(out)
	if err != nil {
		// Unreachable: []suggestionReason is strings and a float64. It is returned
		// rather than swallowed because the caller (applyRuleAction) runs inside a
		// per-action SAVEPOINT and treats any error as "skip this action" — so an
		// impossible failure costs one suggestion, never the ingest.
		return nil, fmt.Errorf("store: marshal rule suggestion reasons: %w", err)
	}
	return payload, nil
}

// evidencePhrase names how much of the user's own filing backs a candidate, in
// the same plain, lowercase-led voice as rules.DescribeCondition (these sit in
// one list on the queue row).
//
// It counts MESSAGES THE USER FILED, which is the only claim the inducer can
// honestly make: Induce's positives are hand-applied labels only, never labels a
// rule or a model put there (see store.LabelTrainingRows for why that distinction
// is load-bearing everywhere it appears). "you filed" is therefore literal.
func evidencePhrase(evidence int) string {
	if evidence == 1 {
		return "backed by 1 message you filed under this label"
	}
	return "backed by " + strconv.Itoa(evidence) + " messages you filed under this label"
}

// ClearLabelSuggestions removes every open question about a message. Called when
// a human answers — by applying a label, or by dismissing the lot — because a
// question that stays queued after it has been answered is worse than one never
// asked.
//
// It reports whether it actually removed anything, and the caller MUST treat
// that as an Email change (#299). Suggestions are served as properties of the
// Email (`needsCategory`, and the suggestion list the review surface reads), so
// clearing them mutates a JMAP-visible object; ApplyEmailUpdate used to be able
// to clear them and then take its "nothing changed" early return, leaving the
// review queue on every other client showing a question the user has answered
// with no /changes call ever saying otherwise.
//
// It appends no row itself, deliberately: it is always called from inside a
// larger Email mutation that owes exactly ONE Email row for the whole set, and a
// row appended here would be a second one for the same object in the same
// transaction.
func (tx *UserTx) ClearLabelSuggestions(ctx context.Context, msgID ulid.ULID) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		DELETE FROM app.label_suggestions WHERE user_id = $1 AND message_id = $2`,
		tx.userID.String(), msgID.String())
	if err != nil {
		return false, fmt.Errorf("store: clear label suggestions: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DismissCategoryQuestions records "these do not need a category" (#176): it
// closes every open question about each message AND writes the tombstone that
// keeps them out of the queue, WITHOUT filing them anywhere.
//
// BOTH WRITES ARE REQUIRED, and it is worth being precise about why, because
// either alone silently fails. Deleting the suggestions is what stops the
// message ASKING; the tombstone is what stops it QUEUEING, because a message
// with no label is in the queue on the strength of that absence alone
// (EmailQuery.NeedsCategory). With only the delete, a dismissal on unlabelled
// mail — which is most of a first-run backlog — would appear to do nothing at
// all.
//
// It returns the ids that actually CHANGED: something was closed, or a tombstone
// is new. A message already dismissed and already quiet is not reported, because
// reporting it would tell the client something moved when nothing did.
//
// IT APPENDS A change_log ROW PER CHANGED MESSAGE, which the ingest-side write
// deliberately does not (see PutLabelSuggestion). The asymmetry is real, not an
// inconsistency: a suggestion APPEARING rides the Email-create row of the
// message it arrived with, so it needs none of its own. A suggestion
// DISAPPEARING has nothing to ride — no keyword moved, no mailbox changed — and
// Email.suggestedLabels is a JMAP-visible property, so without a row here every
// other client (and this one after a reload) keeps rendering a question the user
// already answered, until something unrelated happens to touch that message.
func (tx *UserTx) DismissCategoryQuestions(ctx context.Context, msgIDs []ulid.ULID) ([]ulid.ULID, error) {
	if len(msgIDs) == 0 {
		return nil, nil
	}
	ids := ulidStrings(msgIDs)
	changed := map[ulid.ULID]bool{}

	collect := func(sql string) error {
		rows, err := tx.tx.Query(ctx, sql, tx.userID.String(), ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			id, err := ulid.Parse(raw)
			if err != nil {
				return fmt.Errorf("message id %q: %w", raw, err)
			}
			// A message with several open questions returns once per deleted row,
			// and again for its tombstone; the SET is what "which messages changed"
			// means, and it is what stops one message appending three identical
			// change_log rows.
			changed[id] = true
		}
		return rows.Err()
	}

	if err := collect(`
		DELETE FROM app.label_suggestions
		 WHERE user_id = $1 AND message_id = ANY($2)
		 RETURNING message_id`); err != nil {
		return nil, fmt.Errorf("store: dismiss category questions (suggestions): %w", err)
	}
	// DO NOTHING, so re-dismissing is idempotent and returns no row — which is
	// exactly the "nothing changed" signal the caller reports.
	if err := collect(`
		INSERT INTO app.category_dismissals (user_id, message_id)
		SELECT $1, id FROM unnest($2::text[]) AS t(id)
		ON CONFLICT (user_id, message_id) DO NOTHING
		 RETURNING message_id`); err != nil {
		return nil, fmt.Errorf("store: dismiss category questions (tombstone): %w", err)
	}

	dismissed := make([]ulid.ULID, 0, len(changed))
	for id := range changed {
		dismissed = append(dismissed, id)
	}
	// Deterministic order: ranging a Go map is randomised, and this value is
	// rendered into a JMAP response and asserted on by tests. (The same defect
	// that flipped kind-sweep's dominant-kind tie-break between runs, #175.)
	sort.Slice(dismissed, func(i, j int) bool {
		return dismissed[i].Compare(dismissed[j]) < 0
	})
	for _, id := range dismissed {
		if _, err := tx.AppendChange(ctx, ObjectEmail, id, OpUpdated); err != nil {
			return nil, err
		}
	}
	return dismissed, nil
}

// UndoCategoryDismissal puts messages back in the needs-a-category queue by
// removing their tombstones (#176), returning the ids that actually had one.
//
// IT DOES NOT RESTORE THE SUGGESTIONS. Those were deleted, and a message that
// went back to being merely uncategorised is the honest end state: the queue
// will show it again, without the guesses the model had made. Recreating them
// would mean either keeping deleted rows around or inventing confidences, and
// the second is how a surface starts asserting things nobody measured.
//
// It exists because a dismissal is otherwise PERMANENT, SILENT and reachable by
// one keystroke. Every other destructive verb in this client is reversible —
// archive, trash, snooze, even a label — and this one removes a message from the
// only surface that would ever ask about it again. A misfire with no way back
// short of psql is not a risk worth taking for a queue whose whole purpose is to
// be cleared quickly.
//
// Appends a change_log row per restored message, for the same reason the
// dismissal does: Email.suggestedLabels and queue membership are what a client
// re-reads, and neither is derivable from anything else it holds.
func (tx *UserTx) UndoCategoryDismissal(ctx context.Context, msgIDs []ulid.ULID) ([]ulid.ULID, error) {
	if len(msgIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		DELETE FROM app.category_dismissals
		 WHERE user_id = $1 AND message_id = ANY($2)
		 RETURNING message_id`,
		tx.userID.String(), ulidStrings(msgIDs))
	if err != nil {
		return nil, fmt.Errorf("store: undo category dismissal: %w", err)
	}
	var restored []ulid.ULID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan restored dismissal: %w", err)
		}
		id, err := ulid.Parse(raw)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: restored message id %q: %w", raw, err)
		}
		restored = append(restored, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: undo category dismissal: %w", err)
	}
	sort.Slice(restored, func(i, j int) bool { return restored[i].Compare(restored[j]) < 0 })
	for _, id := range restored {
		if _, err := tx.AppendChange(ctx, ObjectEmail, id, OpUpdated); err != nil {
			return nil, err
		}
	}
	return restored, nil
}

// SuggestionsForMessages returns the open questions for the given messages,
// keyed by message id — what the reading pane and the review surface render.
func (tx *UserTx) SuggestionsForMessages(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID][]LabelSuggestion, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, label_id, confidence, model_version, reasons, source
		  FROM app.label_suggestions
		 WHERE user_id = $1 AND message_id = ANY($2)
		 ORDER BY confidence DESC, label_id`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: suggestions for messages: %w", err)
	}
	defer rows.Close()

	out := map[ulid.ULID][]LabelSuggestion{}
	for rows.Next() {
		var messageID, labelID string
		var s LabelSuggestion
		// source is selected on EVERY read, not only where a caller says it wants
		// it: Confidence cannot be interpreted without it (#242), so handing a
		// caller one without the other is handing them a number whose units are
		// unknown. The column is NOT NULL, so this never scans a zero value.
		if err := rows.Scan(&messageID, &labelID, &s.Confidence, &s.ModelVersion, &s.Reasons, &s.Source); err != nil {
			return nil, fmt.Errorf("store: scan suggestion: %w", err)
		}
		mid, err := ulid.Parse(messageID)
		if err != nil {
			return nil, fmt.Errorf("store: suggestion message id %q: %w", messageID, err)
		}
		lid, err := ulid.Parse(labelID)
		if err != nil {
			return nil, fmt.Errorf("store: suggestion label id %q: %w", labelID, err)
		}
		s.MessageID, s.LabelID = mid, lid
		out[mid] = append(out[mid], s)
	}
	return out, rows.Err()
}

// HandAppliedLabels returns, for each message the user has labelled BY HAND, the
// labels they applied. It is the evidence set for kind-bindings (#175): "20 of
// your 22 Promotions look promotional".
//
// Hand-applied only, for the same reason the classifier trains on hand-applied
// only: a label a rule or the model put there is an automation's opinion, and
// binding a kind to it would let the system draw conclusions from its own output.
func (tx *UserTx) HandAppliedLabels(ctx context.Context) (map[ulid.ULID][]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, label_id FROM app.message_labels
		 WHERE user_id = $1 AND applied_by = $2`,
		tx.userID.String(), LabelByUser)
	if err != nil {
		return nil, fmt.Errorf("store: hand-applied labels: %w", err)
	}
	defer rows.Close()
	out := map[ulid.ULID][]ulid.ULID{}
	for rows.Next() {
		var m, l string
		if err := rows.Scan(&m, &l); err != nil {
			return nil, fmt.Errorf("store: scan hand-applied label: %w", err)
		}
		mid, err := ulid.Parse(m)
		if err != nil {
			continue
		}
		lid, err := ulid.Parse(l)
		if err != nil {
			continue
		}
		out[mid] = append(out[mid], lid)
	}
	return out, rows.Err()
}
