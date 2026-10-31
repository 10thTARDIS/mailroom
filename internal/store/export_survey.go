// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
)

// What an export would have to carry, measured rather than assumed (#54).
//
// The row counts in UserRowCensus answer "how much is there". These answer the
// question the census cannot: how much of it is IN THE RAW MESSAGE BYTES, and
// how much would be lost by an export that wrote only the mail.
//
// ⚠ THAT DISTINCTION IS THE ONE #54 CALLS OUT BY NAME: "an export that writes
// only messages re-imports everything unread." Read state, flags and keywords
// are Mailroom's, not RFC 5322's — they live in columns, and a maildir of raw
// bytes carries none of them. So does label provenance: which rule applied a
// label is a fact about the user's filing, and it is gone the moment the
// sidecar omits it.

// ExportSurvey is the shape of one user's exportable mail.
type ExportSurvey struct {
	// Messages and RawBytes size the maildir an export would write.
	Messages int64
	RawBytes int64
	// Trashed messages are counted separately: they are still rows, and whether
	// an export carries a user's trash is a decision, not an oversight.
	Trashed int64

	// FlagBearing is how many messages carry at least one flag or keyword that
	// is NOT recoverable from the raw bytes. An export whose sidecar omits
	// these re-imports that many messages in the wrong state.
	FlagBearing int64
	// Keyworded is the subset carrying custom IMAP keywords (text[]), which are
	// the part most easily forgotten because the booleans look like the whole
	// story.
	Keyworded int64
	// KeywordedIndependently is the subset of those whose keywords are NOT all
	// accounted for by app.label_keyword_map.
	//
	// ⚠ THE SPLIT EXISTS BECAUSE THE UNSPLIT NUMBER OVERSTATED THE LOSS, and it
	// was caught by auditing the measurement on real data rather than by
	// reasoning. On a real mailbox nearly half the messages carry keywords
	// — a startling share — and every single one of those keywords is a
	// `MailroomLabel-*` atom that Mailroom itself wrote to IMAP and read back.
	// They are app.message_labels in a provider-specific encoding, so an export
	// carrying labels.json loses NOTHING by omitting them.
	//
	// Reporting the raw 917 under "state an export would lose" would therefore
	// have been a true sentence producing a false impression. What an export
	// genuinely risks losing is a keyword no label explains — `$Forwarded`,
	// `NonJunk`, whatever a user's other client wrote — and on the mailbox measured that
	// count was ZERO. Another mailbox will differ, which is exactly why both
	// numbers are reported rather than the reassuring one.
	//
	// The authority is app.label_keyword_map, not a `MailroomLabel-` prefix
	// test: the prefix is a convention this code does not own, and matching on
	// it would silently mis-file a user's own keyword that happened to start the
	// same way.
	KeywordedIndependently int64

	// Submissions is every send attempt, and SubmissionsUnsent is how many ended
	// in any state other than 'sent'.
	//
	// ⚠ THE SECOND NUMBER IS THE ONE THAT SAYS WHETHER EXPORTING THIS TABLE
	// MATTERS ON A GIVEN MAILBOX. A successful send is already fully described
	// by the message itself. A failed or cancelled one is not: the message
	// exports looking like an ordinary unsent draft, and only this table records
	// that a send was attempted and what the server said. Reporting the total
	// alone would answer "how much send history is there" when the question is
	// "how much of it is unrecoverable from the mail".
	Submissions       int64
	SubmissionsUnsent int64

	// Labelled is the number of (message, label) pairs.
	Labelled int64
	// RuleApplied is how many of those record WHICH rule applied them
	// (applied_by = 'rule' with a non-null applied_by_rule_id). A sidecar that
	// writes only the label name loses this, and with it the user's ability to
	// tell their own filing from a rule's.
	RuleApplied int64
	// RuleAppliedOrphan is applied_by='rule' rows whose rule id is NULL — the
	// attribution exists but names nothing, because rules delete SET NULL. They
	// are counted apart so that "provenance carried" is not overstated.
	RuleAppliedOrphan int64
}

// SurveyExportable measures the calling user's mail for export planning.
//
// Every count is RLS-scoped like the census: no WHERE user_id here, so these
// numbers cannot disagree with the policies that govern the export itself.
func (tx *UserTx) SurveyExportable(ctx context.Context) (ExportSurvey, error) {
	var s ExportSurvey

	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*),
		       COALESCE(sum(size_bytes), 0),
		       count(*) FILTER (WHERE trashed_at IS NOT NULL),
		       count(*) FILTER (WHERE is_seen OR is_flagged OR is_answered
		                           OR is_draft OR is_junk
		                           OR cardinality(keywords) > 0),
		       count(*) FILTER (WHERE cardinality(keywords) > 0),
		       count(*) FILTER (
		         WHERE cardinality(keywords) > 0
		           AND EXISTS (SELECT 1 FROM unnest(keywords) AS k
		                        WHERE k NOT IN (SELECT keyword_atom
		                                          FROM app.label_keyword_map)))
		  FROM app.messages`).Scan(
		&s.Messages, &s.RawBytes, &s.Trashed, &s.FlagBearing, &s.Keyworded,
		&s.KeywordedIndependently); err != nil {
		return s, fmt.Errorf("store: survey messages for export: %w", err)
	}

	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE applied_by = 'rule' AND applied_by_rule_id IS NOT NULL),
		       count(*) FILTER (WHERE applied_by = 'rule' AND applied_by_rule_id IS NULL)
		  FROM app.message_labels`).Scan(
		&s.Labelled, &s.RuleApplied, &s.RuleAppliedOrphan); err != nil {
		return s, fmt.Errorf("store: survey labels for export: %w", err)
	}

	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE state <> 'sent')
		  FROM app.submissions`).Scan(&s.Submissions, &s.SubmissionsUnsent); err != nil {
		return s, fmt.Errorf("store: survey submissions for export: %w", err)
	}

	return s, nil
}
