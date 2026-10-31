// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// This file is the store surface for T1 priority scoring (spec §8, mailroom#33):
// the per-message triage score (app.triage_scores) plus the two read helpers the
// scorer's feature extractor needs (the sender's contact stats and the raw
// message signals).
//
// ⚠ THE EXCEPTION THIS FILE USED TO CLAIM IS NO LONGER TRUE, AND SAYING SO IS THE
// POINT (#299). It said triage scores were "OPERATIONAL data … Nothing here is
// ever synced", and that list_id / list_unsubscribe were "not JMAP-visible Email
// state". All four are served on the JMAP Email object today —
// `priorityScore`, `topFeatures` (jmap_email.go, internal/api/email.go's
// capTriage properties), `listId` and `listUnsubscribe` (#58's unsubscribe
// surface) — added AFTER those comments were written. The comments decayed; the
// code did not change under them.
//
// WHAT IS STILL TRUE: app.triage_scores is not itself a JMAP object and is not a
// store ObjectType. Its rows are reachable only as PROPERTIES OF AN Email, which
// is why the invariant lands on ObjectEmail rather than on a type of its own.
//
// WHAT THAT MEANS FOR EACH WRITER, now that it is spelled out rather than
// assumed:
//
//   - INGEST is still sound with no row of its own. The first score is written
//     in the same transaction as the message, so its values ride the Email
//     `created` row the ingest path already appends — a client that fetches the
//     new Email gets the score with it. Emitting an `updated` beside a `created`
//     for the same object in the same window would also be wrong per RFC 8620
//     §5.2, which says an object created and updated in one window belongs in
//     `created` alone.
//   - RE-SCORING IS NOT, and that is the gap the audit found: `mailroom-admin
//     rescore` re-scores up to every message a user has, changing
//     `Email.priorityScore` and `Email.isImportant` with nothing telling any
//     client, so they hold stale priorities until something unrelated happens to
//     touch each message.
//
// So the two writers below now append an Email `updated` row EXACTLY WHEN THEY
// OVERWRITE STATE A CLIENT MAY ALREADY HOLD — never on the first write. That
// keeps ingest's hot path free of a redundant second row while making the
// re-score path correct without the caller having to remember, which is the
// failure mode this whole issue is about (contrast PutRuleSuggestion, which
// re-argues its backfill caller explicitly; UpsertTriageScore never addressed
// its re-score caller at all).
//
// Every method runs inside a UserTx, so RLS (app.triage_scores /
// app.contact_addresses / app.messages all FORCE ROW LEVEL SECURITY) scopes each
// statement to the caller — no identity, no rows (fail closed).

// UpsertTriageScore writes (or overwrites) the priority score for a message on
// the (user_id, message_id) primary key — a re-score of an existing message
// replaces the prior row rather than duplicating it. modelVersion records which
// model produced the score (encodeable back to an ml_models version for audit);
// topFeatures is the explainability payload (#34) — pass []byte("[]") for now,
// which this method also substitutes for an empty/nil argument so the column
// keeps its default-shaped empty JSON array.
//
// It appends an Email `updated` change_log row when — and only when — it
// OVERWRITES an existing score with different values (#299). See the file header
// for the full argument; the short version is that priorityScore and topFeatures
// are served on the JMAP Email object, so a re-score mutates JMAP-visible state
// and owes the invariant its row, while the first score of a brand-new message
// rides the Email `created` row ingest appends in the same transaction.
//
// ⚠ THE `prior` CTE READS THE PRE-UPSERT STATE, and that is load-bearing rather
// than stylistic. Both CTEs see the same snapshot, so `prior` is the row as it
// was BEFORE the upsert regardless of execution order — which is the only way to
// answer "was this a re-score, and did anything actually change?" in one round
// trip. Doing it as a separate SELECT first would be a second statement that
// could not see a concurrent writer any better, for the same answer.
//
// Comparing the VALUES, not merely "a row existed": re-running `rescore` after a
// no-op change produces byte-identical scores for most messages, and telling
// every client that N Emails changed when none did is its own kind of lie.
//
// No ::app.ulid casts are needed here: unlike InsertModelVersion (where $2 was
// reused in a subquery WHERE compare and therefore had to be type-annotated),
// each id param is used exactly ONCE, as a VALUES target for its column, so pgx
// infers the column type directly (same as message_provenance's upsert).
func (tx *UserTx) UpsertTriageScore(ctx context.Context, msgID ulid.ULID, score float64, modelVersion string, topFeatures []byte) error {
	_, err := tx.RescoreTriage(ctx, msgID, score, modelVersion, topFeatures)
	return err
}

// RescoreTriage is UpsertTriageScore reporting whether it OVERWROTE a prior
// score with different values (and so appended the Email `updated` row). It is
// what `priority-rescore --apply` counts, so the apply's tally is the same
// comparison TriageScoreWouldChange made in the dry run.
//
// The comparison casts the new score to the column's own type, `real`. That
// is explicit rather than necessary: $3's first use is the `real` VALUES
// target, so Postgres already infers it as real, and a #842 probe that
// reverted the cast to double precision stayed green. It is written out so the
// comparison does not depend on parameter-inference order, and so it matches
// TriageScoreWouldChange, whose query has no VALUES position to infer from.
func (tx *UserTx) RescoreTriage(ctx context.Context, msgID ulid.ULID, score float64, modelVersion string, topFeatures []byte) (bool, error) {
	if len(topFeatures) == 0 {
		topFeatures = []byte("[]")
	}
	var rescored bool
	err := tx.tx.QueryRow(ctx, `
		WITH prior AS (
			SELECT score, model_version, top_features
			  FROM app.triage_scores
			 WHERE user_id = $1 AND message_id = $2
		), upserted AS (
			INSERT INTO app.triage_scores
				(user_id, message_id, score, model_version, top_features, scored_at)
			VALUES ($1, $2, $3, $4, $5::jsonb, now())
			ON CONFLICT (user_id, message_id) DO UPDATE
			   SET score         = EXCLUDED.score,
			       model_version = EXCLUDED.model_version,
			       top_features  = EXCLUDED.top_features,
			       scored_at     = now()
			RETURNING 1
		)
		SELECT EXISTS (
			SELECT 1 FROM prior p
			 WHERE (p.score, p.model_version, p.top_features)
			       IS DISTINCT FROM ($3::real, $4::text, $5::jsonb))`,
		tx.userID.String(), msgID.String(), score, modelVersion, string(topFeatures)).Scan(&rescored)
	if err != nil {
		return false, fmt.Errorf("store: upsert triage score: %w", err)
	}
	if !rescored {
		return false, nil // first score (rides the Email `created` row), or nothing changed
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, msgID, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// ContactByAddress reports whether the given address is a known correspondent in
// the auto-derived address book (#37) and, if so, how many times it has been
// seen — the is-known-sender / frequency signal the priority model reads. The
// address is normalized the same way the contacts layer stores it (citext,
// NormalizeAddress), so the lookup is case/whitespace insensitive. found is false
// for an address the user has never corresponded with (and, by RLS, for another
// user's contact — indistinguishable by design).
func (tx *UserTx) ContactByAddress(ctx context.Context, addr string) (found bool, seenCount int, err error) {
	norm := NormalizeAddress(addr)
	if norm == "" {
		return false, 0, nil
	}
	err = tx.tx.QueryRow(ctx, `
		SELECT seen_count
		  FROM app.contact_addresses
		 WHERE user_id = $1 AND address = $2`,
		tx.userID.String(), norm).Scan(&seenCount)
	if isNoRows(err) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("store: contact by address: %w", err)
	}
	return true, seenCount, nil
}

// MessageMLFacts is the subset of the canonical message row the ml feature
// extractor reads on the REPLAY / training path (ml.StoreExtractor). The signals
// that are NOT projected onto app.messages — List-Id / List-Unsubscribe and the
// To+Cc recipient count all live in the message blob — are deliberately absent:
// the serve path fills those from the parsed headers it already holds, while
// replay works from the row alone (see ml.MessageFacts).
type MessageMLFacts struct {
	FromAddress      string
	Subject          string
	HasAttachments   bool
	Flagged          bool
	Junk             bool
	InReplyTo        string
	ReceivedAt       time.Time
	AddressedAddress string

	// The bulk signals, projected onto the row by migration 00016 (#157). Before
	// that they were readable only from the blob, so this replay path left them
	// false and the model could never learn from them — the serve path saw a
	// message as bulk and the training path saw the same message as ordinary.
	// NULL (never backfilled by `mailroom-admin rescore`) reads as absent.
	ListID          string
	ListUnsubscribe string
	RecipientCount  int
	AuthFail        bool
}

// UpdateMessageSignals backfills the bulk signals migration 00016 added, from
// headers re-parsed out of the stored bytes (mailroom#157). It reports whether
// anything actually changed, so a repeat run is a cheap no-op.
//
// ⚠ IT APPENDS AN Email `updated` ROW WHEN IT CHANGES ANYTHING, and the comment
// it replaces was wrong to say otherwise (#299). Two of the four columns it
// writes — list_id and list_unsubscribe — ARE JMAP-visible Email state:
// `Email.listId` and `Email.listUnsubscribe` under urn:mailroom:rules and #58's
// unsubscribe surface. recipient_count and auth_fail are genuinely operational,
// but they travel in the same statement, and the guarded UPDATE only fires when
// something differs, so the row is owed whenever this returns true.
//
// Its only caller is `mailroom-admin rescore`, never ingest — so unlike
// UpsertTriageScore there is no first-write case that rides an Email `created`
// row, and no condition is needed beyond "did anything change".
func (tx *UserTx) UpdateMessageSignals(ctx context.Context, id ulid.ULID, listID, listUnsub string, recipients int, authFail bool) (bool, error) {
	var touched string
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.messages
		   SET list_id = NULLIF($3, ''), list_unsubscribe = NULLIF($4, ''),
		       recipient_count = $5, auth_fail = $6
		 WHERE user_id = $1 AND id = $2
		   AND (COALESCE(list_id, ''), COALESCE(list_unsubscribe, ''),
		        COALESCE(recipient_count, -1), auth_fail)
		       IS DISTINCT FROM ($3, $4, $5, $6)
		RETURNING id`,
		tx.userID.String(), id.String(), listID, listUnsub, recipients, authFail).Scan(&touched)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: update message signals: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// OutboundCountForAddress reports how many times the user has WRITTEN to an
// address — the correspondence signal that replaced "is in the address book"
// (mailroom#157). 0 for an address never written to, which is where every bulk
// sender lives however often it mails you.
// It also returns how many times that address has written to the USER, which is
// meaningless alone — treating it as intimacy is what broke scoring in the first
// place — but is evidence of BROADCAST when paired with an outbound count of 0.
func (tx *UserTx) OutboundCountForAddress(ctx context.Context, addr string) (outbound, inbound int, err error) {
	norm := NormalizeAddress(addr)
	if norm == "" {
		return 0, 0, nil
	}
	err = tx.tx.QueryRow(ctx, `
		SELECT outbound_count, seen_count FROM app.contact_addresses
		 WHERE user_id = $1 AND address = $2`,
		tx.userID.String(), norm).Scan(&outbound, &inbound)
	if isNoRows(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("store: outbound count for address: %w", err)
	}
	return outbound, inbound, nil
}

// MessageMLFacts reads the ml feature signals for one message (RLS-scoped).
// found is false when the message does not exist or belongs to another user.
func (tx *UserTx) MessageMLFacts(ctx context.Context, msgID ulid.ULID) (MessageMLFacts, bool, error) {
	var f MessageMLFacts
	err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(from_address, ''), subject, has_attachments,
		       is_flagged, is_junk, COALESCE(in_reply_to, ''),
		       received_at, COALESCE(addressed_address, ''),
		       COALESCE(list_id, ''), COALESCE(list_unsubscribe, ''),
		       COALESCE(recipient_count, 0), auth_fail
		  FROM app.messages
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), msgID.String()).
		Scan(&f.FromAddress, &f.Subject, &f.HasAttachments,
			&f.Flagged, &f.Junk, &f.InReplyTo, &f.ReceivedAt, &f.AddressedAddress,
			&f.ListID, &f.ListUnsubscribe, &f.RecipientCount, &f.AuthFail)
	if isNoRows(err) {
		return MessageMLFacts{}, false, nil
	}
	if err != nil {
		return MessageMLFacts{}, false, fmt.Errorf("store: message ml facts: %w", err)
	}
	return f, true, nil
}
