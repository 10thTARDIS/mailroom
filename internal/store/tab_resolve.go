// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/tabs"
)

// RESOLVING A MESSAGE'S TAB, in the two places that have to agree (#228,
// ADR-022).
//
//	resolved_tab := COALESCE(mapping_tab, classified_tab)   [then the enabled fold]
//
// The mapping half is a LIVE JOIN, so re-pointing a label moves its mail on the
// next read with no backfill — ADR-021's guarantee, intact. The classifier half
// is the stored verdict (migration 00029), because Email/query filters in SQL
// and the classifier is a Go decision list; re-expressing those rules in SQL
// would be two implementations of one thing (#155's Rule/preview lesson).
//
// THIS FILE HOLDS BOTH IMPLEMENTATIONS ON PURPOSE — the Go resolver below and
// the SQL predicate it is paired with — so a reader changing one is looking at
// the other. They cannot share code across the language boundary, so
// TestTabResolverAgreesWithTheQueryPredicate makes disagreement a red build
// instead.

// ResolvedTab is one message's tab and where the answer came from. The source
// matters to the caller: a tab from the mapping is the user's own decision, and a
// tab from the classifier is a guess with a measured error rate (#219).
type ResolvedTab struct {
	Tab tabs.Tab
	// FromMapping is true when a mapped label decided it, false when the stored
	// classifier verdict did.
	FromMapping bool
	// Classified is false when NEITHER decided — no mapped label and no stored
	// verdict. The tab is then Primary by fold, but nothing actually chose it, and
	// such a message is INVISIBLE to a tab filter until the backfill reaches it.
	// Reported rather than smoothed over.
	Classified bool
}

// ResolveTabs answers the tab for a set of messages in one round trip.
//
// enabled is the caller's fold map (UserTx.EnabledTabs). Passing it in rather
// than reading it here keeps the five-row lookup out of a per-message path and
// makes the fold testable without a database.
func (tx *UserTx) ResolveTabs(ctx context.Context, ids []ulid.ULID, enabled map[tabs.Tab]bool) (map[ulid.ULID]ResolvedTab, error) {
	out := make(map[ulid.ULID]ResolvedTab, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id,
		       (SELECT l.tab::text
		          FROM app.message_labels ml
		          JOIN app.labels l
		            ON l.user_id = ml.user_id AND l.id = ml.label_id
		         WHERE ml.user_id = m.user_id AND ml.message_id = m.id
		           AND l.tab IS NOT NULL
		         ORDER BY array_position($3::text[], l.tab::text)
		         LIMIT 1) AS mapping_tab,
		       m.classified_tab::text
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids), tabs.PrecedenceNames())
	if err != nil {
		return nil, fmt.Errorf("store: resolve tabs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id         string
			mapped     *string
			classified *string
		)
		if err := rows.Scan(&id, &mapped, &classified); err != nil {
			return nil, fmt.Errorf("store: scan resolved tab: %w", err)
		}
		mid, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, err)
		}
		r, err := resolveOne(mapped, classified, enabled)
		if err != nil {
			return nil, fmt.Errorf("store: message %s: %w", id, err)
		}
		out[mid] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: resolve tabs: %w", err)
	}
	return out, nil
}

// resolveOne is the decision itself, split out so it is testable without a
// database and so the SQL above stays a pure projection.
func resolveOne(mapped, classified *string, enabled map[tabs.Tab]bool) (ResolvedTab, error) {
	parse := func(s *string) (tabs.Tab, bool, error) {
		if s == nil || *s == "" {
			return "", false, nil
		}
		t, ok := tabs.Parse(*s)
		if !ok {
			return "", false, fmt.Errorf("unknown category tab %q", *s)
		}
		return t, true, nil
	}
	if t, ok, err := parse(mapped); err != nil {
		return ResolvedTab{}, err
	} else if ok {
		return ResolvedTab{Tab: tabs.Fold(t, enabled), FromMapping: true, Classified: true}, nil
	}
	if t, ok, err := parse(classified); err != nil {
		return ResolvedTab{}, err
	} else if ok {
		return ResolvedTab{Tab: tabs.Fold(t, enabled), Classified: true}, nil
	}
	// Neither. Primary is where it goes, because a strip has to put every message
	// somewhere — but Classified stays false so a caller can tell the difference
	// between "decided Primary" and "nothing has looked at this yet".
	return ResolvedTab{Tab: tabs.Primary}, nil
}

// tabFilterSQL is the Email/query predicate for `inTab`, and the twin of
// resolveOne above.
//
// It returns a condition and its two bind values. `wanted` is the EXPANDED set
// the caller wants — expanded because the enabled-fold is applied in Go, not
// here: querying an enabled tab asks for that tab alone, while querying Primary
// asks for Primary plus every disabled tab, since that is where their mail
// folds. Keeping the fold in one language is worth the caller doing the
// expansion (ADR-022).
//
// ⚠ THE ORDER BY MUST MATCH tabs.Precedence(), and it does not restate it: the
// order arrives as a bind parameter. There is deliberately no lookup table or
// IMMUTABLE function in the database holding a second copy to drift from.
func tabFilterSQL(wanted []tabs.Tab, bind func(any) string) string {
	names := make([]string, len(wanted))
	for i, t := range wanted {
		names[i] = string(t)
	}
	return `COALESCE(
	           (SELECT l.tab::text
	              FROM app.message_labels ml
	              JOIN app.labels l
	                ON l.user_id = ml.user_id AND l.id = ml.label_id
	             WHERE ml.user_id = m.user_id AND ml.message_id = m.id
	               AND l.tab IS NOT NULL
	             ORDER BY array_position(` + bind(tabs.PrecedenceNames()) + `::text[], l.tab::text)
	             LIMIT 1),
	           m.classified_tab::text
	         ) = ANY(` + bind(names) + `::text[])`
}

// StoreClassifiedTab writes one message's classifier verdict and the version
// that produced it (migration 00029).
//
// NO change_log ROW, and the reason is the same as the function signals in
// 00027: this is an operational projection, not JMAP-visible Email state. No
// client reads `classified_tab`; what a client reads is `Email.categoryTab`,
// which is DERIVED from it and from the mapping. A change row here would tell
// every device a message changed when nothing about it did.
//
// ⚠ THE COROLLARY, stated because it is the sharp edge: re-classifying does NOT
// notify clients. A tab that moves because the classifier changed is invisible
// to /changes until something else touches the message. That is acceptable for a
// release-time backfill and would NOT be acceptable for a user-facing edit —
// which is exactly why the mapping half is a live join instead.
func (tx *UserTx) StoreClassifiedTab(ctx context.Context, id ulid.ULID, tab tabs.Tab, version int) (bool, error) {
	if tab != "" && !tab.Valid() {
		return false, fmt.Errorf("store: classified tab %q is not in the vocabulary", tab)
	}
	var touched string
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.messages
		   SET classified_tab = NULLIF($3, '')::app.category_tab,
		       classifier_version = $4
		 WHERE user_id = $1 AND id = $2
		   AND (COALESCE(classified_tab::text, ''), COALESCE(classifier_version, -1))
		       IS DISTINCT FROM ($3, $4)
		RETURNING id`,
		tx.userID.String(), id.String(), string(tab), version).Scan(&touched)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: store classified tab: %w", err)
	}
	return true, nil
}

// MessagesNeedingClassification counts rows whose stored verdict is stale or
// absent — the exact set a re-classify must visit.
//
// It exists so a report can state its bound. A tab filter silently omits any
// message with no verdict and no mapped label, so "how many are unclassified" is
// not housekeeping trivia: it is the difference between a tab that is empty and
// a tab that is incomplete (#155, #176).
func (tx *UserTx) MessagesNeedingClassification(ctx context.Context, version int) (stale, missing int, err error) {
	err = tx.tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE classified_tab IS NOT NULL
		                          AND classifier_version IS DISTINCT FROM $2),
		       count(*) FILTER (WHERE classified_tab IS NULL)
		  FROM app.messages
		 WHERE user_id = $1`,
		tx.userID.String(), version).Scan(&stale, &missing)
	if err != nil {
		return 0, 0, fmt.Errorf("store: count messages needing classification: %w", err)
	}
	return stale, missing, nil
}
