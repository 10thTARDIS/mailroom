// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"
)

// The read side of the rules blast-radius preview (#155): "this rule would have
// matched N of your existing messages".
//
// A rule you cannot preview is a rule you find out about later — after it has
// labelled six months of mail. But the preview has to be evaluated by the SAME
// matcher the ingest path uses (rules.Condition.Matches), or it becomes a
// second implementation that can disagree with the engine, which is worse than
// no preview: it is a confident wrong answer.
//
// So this file returns FACTS, not verdicts. It projects each candidate message
// into the fields a rule can test, and the caller runs the real evaluator over
// them.
//
// WHERE THE FACTS LIVE, and why the preview has two speeds:
//
//	from, fromDomain, subject, alias, listId, listUnsubscribe  ->  columns
//	to, header                                                 ->  the BLOB only
//
// There is no recipients table — To/Cc are in the raw RFC 5322 bytes, and so is
// every arbitrary header. A rule touching those needs the message opened and
// parsed, one decrypt per message, which is far too expensive to do for a whole
// mailbox on every keystroke of an editor. Hence the split: the column path
// scans wide and cheap, and the caller falls back to opening blobs only for the
// rules that genuinely need it, over a smaller window.
//
// Either way the scan is BOUNDED and the bound is reported, so "matched 37"
// is never silently "37 of the most recent few hundred".

// RulePreviewCandidate is one message projected into the facts a rule can test
// from stored columns, plus what the caller needs to open the blob (ContentHash)
// and to show a sample row (Subject, From, ReceivedAt).
type RulePreviewCandidate struct {
	ID          ulid.ULID
	ContentHash []byte

	From      string
	Subject   string
	Alias     string
	ListID    string
	ListUnsub string

	ReceivedAt time.Time

	// UserLabelIDs are the labels the USER applied to this message — not ones a
	// rule or the model applied (#156).
	//
	// Only the user's own labels count as evidence. A label a rule put there is
	// the automation's opinion, and treating it as ground truth would let a rule
	// that is already wrong confirm itself: its output becomes training data,
	// which induces a rule agreeing with it, forever. Provenance
	// (applied_by, migration 00017) is what makes the distinction available.
	UserLabelIDs []ulid.ULID

	// HasAnyLabel is whether this message carries ANY label at all, whoever put it
	// there — user, rule, model or agent.
	//
	// ⚠ IT IS A DIFFERENT QUESTION FROM len(UserLabelIDs) > 0, AND CONFLATING THE
	// TWO SHIPPED A DEFECT (#242 follow-up). The two fields look interchangeable
	// and are not:
	//
	//	UserLabelIDs  answers "is this EVIDENCE?"  — whose opinion is it
	//	HasAnyLabel   answers "is this FILED?"     — is it categorised at all
	//
	// Induction must ask the first, for the reason spelled out above. Anything
	// deciding whether a message still NEEDS a category must ask the second,
	// because that is literally the question the surface asks: the needs-a-category
	// predicate (EmailQuery.NeedsCategory, jmap_email.go) is built on
	// `NOT EXISTS (SELECT 1 FROM app.message_labels …)` with no applied_by
	// predicate anywhere in it.
	//
	// suggest-rules used the FIRST definition for both jobs, and so counted a
	// message a RULE had already filed as uncategorised. MEASURED on a real
	// mailbox: a fair share of the suggestions it would have written landed on mail a rule
	// had already filed — and because a suggestion row satisfies the OTHER arm of
	// that same predicate, writing them would have dragged those back INTO the
	// needs-a-category queue, asking the user to categorise mail that
	// was already categorised.
	//
	// It is a bool and not a label list on purpose. Handing back the rule-applied
	// ids would invite the next caller to feed them to the inducer as evidence,
	// which is the failure the comment above exists to prevent; "is it filed" is
	// the only thing anything outside the store needs from them.
	HasAnyLabel bool

	// MailboxIDs are the mailboxes this message has LIVE provenance in — where
	// the provider itself filed it.
	//
	// FOLDER MEMBERSHIP IS EVIDENCE OF WHAT THE USER WANTS, in the same way a
	// hand-applied label is (#446 step 3, "rules from folders"). A person who
	// has been dragging mail into a "Receipts" folder for four years has stated
	// a preference just as clearly as one who clicked a label, and on a mailbox
	// arriving from another client it is the ONLY statement available — there
	// are no hand-applied Mailroom labels yet, so Rule/induce over labels has
	// nothing to learn from on day one, which is the day a rule is worth the
	// most.
	//
	// ⚠ IT IS RAW LIVE PROVENANCE, not effective membership (inMailboxSQL): the
	// question is where the PROVIDER's sorting put the message, and a message
	// the user has since moved inside Mailroom was still sorted into that folder
	// by whatever rule the induction is trying to recover.
	MailboxIDs []ulid.ULID
}

// RulePreviewCandidates returns the caller's most recent messages, newest
// first, for a rules preview.
//
// NEWEST FIRST is the deliberate ordering: a bounded scan has to choose which
// slice of the mailbox it looks at, and recent mail is what a rule being written
// today is about — the sender you just saw, the newsletter that just arrived.
// It also matches what the sample rows will show, so the count and the examples
// describe the same window.
//
// Deleted messages are excluded. A rule is a statement about mail you keep, and
// counting matches among things already thrown away inflates the blast radius
// with mail the user has dealt with. "Deleted" is Email/query's ONE definition
// (#631, previewableWhere): trashed by Mailroom, OR in a Trash folder whoever
// put it there — a message deleted from a phone is as thrown away as one
// deleted here, and the preview's "N of M" must describe the same mail the
// list views show.
func (tx *UserTx) RulePreviewCandidates(ctx context.Context, limit int) ([]RulePreviewCandidate, error) {
	if limit <= 0 {
		limit = 500
	}
	args := []any{tx.userID.String(), limit}
	where, err := tx.previewableWhere(ctx, &args)
	if err != nil {
		return nil, err
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id, m.content_hash,
		       COALESCE(m.from_address::text, ''), m.subject,
		       COALESCE(m.addressed_address::text, ''),
		       COALESCE(m.list_id, ''), COALESCE(m.list_unsubscribe, ''),
		       m.received_at
		  FROM app.messages m
		 WHERE `+where+`
		 ORDER BY m.received_at DESC, m.id DESC
		 LIMIT $2`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: rule preview candidates: %w", err)
	}
	defer rows.Close()

	var out []RulePreviewCandidate
	for rows.Next() {
		var (
			id string
			c  RulePreviewCandidate
		)
		if err := rows.Scan(&id, &c.ContentHash, &c.From, &c.Subject, &c.Alias,
			&c.ListID, &c.ListUnsub, &c.ReceivedAt); err != nil {
			return nil, fmt.Errorf("store: scan rule preview candidate: %w", err)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: rule preview candidate id %q: %w", id, err)
		}
		c.ID = parsed
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: rule preview candidates: %w", err)
	}
	if err := tx.attachLabelFacts(ctx, out); err != nil {
		return nil, err
	}
	if err := tx.attachMailboxFacts(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachMailboxFacts fills in each candidate's live-provenance mailbox ids in
// ONE query, the same shape attachLabelFacts uses for the label facts and for
// the same reason: one round trip for the whole window rather than one per
// message, on a path a rules editor calls on every keystroke.
func (tx *UserTx) attachMailboxFacts(ctx context.Context, cands []RulePreviewCandidate) error {
	if len(cands) == 0 {
		return nil
	}
	ids := make([]string, len(cands))
	at := make(map[string]int, len(cands))
	for i, c := range cands {
		ids[i] = c.ID.String()
		at[ids[i]] = i
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT message_id, mailbox_id
		  FROM app.message_provenance
		 WHERE user_id = $1 AND message_id = ANY($2) AND state = 'live'`,
		tx.userID.String(), ids)
	if err != nil {
		return fmt.Errorf("store: rule preview candidate mailboxes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID, mailboxID string
		if err := rows.Scan(&messageID, &mailboxID); err != nil {
			return fmt.Errorf("store: scan candidate mailbox: %w", err)
		}
		i, ok := at[messageID]
		if !ok {
			continue
		}
		mid, err := ulid.Parse(mailboxID)
		if err != nil {
			return fmt.Errorf("store: candidate mailbox id %q: %w", mailboxID, err)
		}
		cands[i].MailboxIDs = append(cands[i].MailboxIDs, mid)
	}
	return rows.Err()
}

// attachLabelFacts fills in each candidate's TWO label facts — the user-applied
// label ids (evidence) and whether the message carries any label at all (filed) —
// in one query rather than one per message.
//
// ONE QUERY ANSWERS BOTH because they read the same rows: the applied_by test
// moved out of the WHERE clause and into the loop below. That is the entire
// mechanical change behind the #242 follow-up. While it sat in the WHERE clause,
// a message filed ONLY by a rule did not come back at all, and every caller that
// asked "is this filed?" by looking at UserLabelIDs got "no" — see
// RulePreviewCandidate.HasAnyLabel for what that cost.
func (tx *UserTx) attachLabelFacts(ctx context.Context, cands []RulePreviewCandidate) error {
	if len(cands) == 0 {
		return nil
	}
	ids := make([]string, len(cands))
	at := make(map[string]int, len(cands))
	for i, c := range cands {
		ids[i] = c.ID.String()
		at[ids[i]] = i
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, label_id, applied_by
		  FROM app.message_labels
		 WHERE user_id = $1 AND message_id = ANY($2)`,
		tx.userID.String(), ids)
	if err != nil {
		return fmt.Errorf("store: rule preview candidate labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID, labelID, appliedBy string
		if err := rows.Scan(&messageID, &labelID, &appliedBy); err != nil {
			return fmt.Errorf("store: scan candidate label: %w", err)
		}
		i, ok := at[messageID]
		if !ok {
			continue
		}
		// FILED IS FILED, whoever filed it — this is the flag the suggestion
		// population is chosen by, and it is set for every row regardless of
		// provenance.
		cands[i].HasAnyLabel = true
		// EVIDENCE IS THE USER'S ALONE. Everything else is the automation's own
		// opinion and must never come back as training data.
		if appliedBy != LabelByUser {
			continue
		}
		lid, err := ulid.Parse(labelID)
		if err != nil {
			return fmt.Errorf("store: candidate label id %q: %w", labelID, err)
		}
		cands[i].UserLabelIDs = append(cands[i].UserLabelIDs, lid)
	}
	return rows.Err()
}

// CountPreviewableMessages is how many messages the preview COULD have scanned:
// the denominator that makes a bounded scan honest ("37 of the 500 most recent,
// out of 12,904"). Same predicate as RulePreviewCandidates, minus the bound.
func (tx *UserTx) CountPreviewableMessages(ctx context.Context) (int, error) {
	args := []any{tx.userID.String()}
	where, err := tx.previewableWhere(ctx, &args)
	if err != nil {
		return 0, err
	}
	var n int
	if err := tx.tx.QueryRow(ctx, `SELECT count(*) FROM app.messages m WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count previewable messages: %w", err)
	}
	return n, nil
}

// previewableWhere is the ONE predicate RulePreviewCandidates and
// CountPreviewableMessages share, so the numerator and the denominator of a
// preview can never disagree about which mail counts. It expects args to hold
// the user id at $1 and appends the trash-role mailbox ids after whatever the
// caller has already bound. The table must be aliased `m`.
func (tx *UserTx) previewableWhere(ctx context.Context, args *[]any) (string, error) {
	where := "m.user_id = $1 AND m.trashed_at IS NULL"
	trash, err := tx.trashRoleMailboxIDs(ctx)
	if err != nil {
		return "", err
	}
	if len(trash) == 0 {
		return where, nil
	}
	bind := func(v any) string {
		*args = append(*args, v)
		return "$" + strconv.Itoa(len(*args))
	}
	return where + " AND NOT " + inAnyMailboxSQL(bind, tx.userID, trash), nil
}
