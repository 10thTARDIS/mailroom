// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/tabs"
)

// AreaReportRow is one live message as `mailroom-admin area-report` (#442
// increment 0) sees it: exactly what internal/areas.Resolve needs to place a
// message, plus enough provenance to split the report into "the inbox" and
// "all mail" the same way `mailroom-admin tabs` already does.
//
// ⚠ MappedTab/MappedKnown MIRROR resolveOne's mapped half (tab_resolve.go)
// EXACTLY — the identical subquery, the identical tabs.PrecedenceNames()
// bind — because an area report that disagreed with ResolveTabs about which
// tab a message resolves to would be measuring a second, drifted
// implementation rather than the resolver that actually runs (#155's
// lesson, restated for a third consumer of the same precedence). ResolvedTab
// below applies the mapped-else-classified COALESCE the same way ResolveTabs
// does, but DELIBERATELY LEAVES THE ENABLED FOLD OUT: an area plan feeds off
// tabs.All as named, not off which tabs today's strip happens to show, so
// folding a disabled tab into Primary here would hide it from an area an
// operator explicitly gave it.
type AreaReportRow struct {
	ID ulid.ULID
	// InInbox is true when the message has a LIVE provenance row in a mailbox
	// named INBOX (case-insensitive) — the population `tabs` calls "unactioned
	// mail" and the one an operator actually triages daily, as opposed to
	// everything else they have already filed.
	InInbox bool
	// Labels is every label name on the message, sorted — internal/areas'
	// label half needs the set, and sorted output means a test's expectations
	// need no second sort of their own.
	Labels []string
	// MappedTab/MappedKnown: the first tab by tabs.Precedence() among the
	// message's labels, exactly as ResolveTabs' live join computes it.
	MappedTab   tabs.Tab
	MappedKnown bool
	// ClassifiedTab/ClassifiedKnown: the stored classifier verdict
	// (messages.classified_tab) — the resolver's fallback when no label maps.
	ClassifiedTab   tabs.Tab
	ClassifiedKnown bool
	// Score/Scored: the triage score, when the model has scored this message —
	// the report's surface/drawer split at the chosen threshold.
	Score  float32
	Scored bool
}

// ResolvedTab mirrors resolveOne's mapped-else-classified precedence WITHOUT
// the enabled-tab fold — see the type doc above for why the fold is left out
// here. ok is false when NEITHER decided anything: the same "nothing has
// looked at this message yet" case resolveOne reports via its own Classified
// field.
func (r AreaReportRow) ResolvedTab() (tabs.Tab, bool) {
	if r.MappedKnown {
		return r.MappedTab, true
	}
	if r.ClassifiedKnown {
		return r.ClassifiedTab, true
	}
	return "", false
}

// AreaReportRowsAfter walks the caller's LIVE messages in ascending id order —
// the same keyset shape as MessageTabMappingsAfter, and the same "would a
// user ever see this" predicate the message list itself uses (not junk, not
// trashed, not vanished, not a draft): a report over messages nobody would
// ever see is not sizing anything real.
//
// `after` is an exclusive low bound (pass the zero ULID to start). RLS scopes
// the walk to the caller. It is READ-ONLY: no row here is ever written by
// this method, and it exists so `mailroom-admin area-report` can be built —
// this walk is the whole of #442 increment 0's read side.
//
// ⚠ THIS MUST STAY IN STEP WITH ResolveTabs (tab_resolve.go). The mapped-tab
// subquery below is copied from resolveOne's SQL rather than shared, for the
// same cross-language reason tab_resolve.go's own doc comment gives for
// holding both of ITS implementations in one file: a reader changing the tab
// resolution rule has to be looking at every place it is expressed. If
// ResolveTabs' subquery changes, this one must change with it.
func (tx *UserTx) AreaReportRowsAfter(ctx context.Context, after ulid.ULID, limit int) ([]AreaReportRow, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id,
		       EXISTS (
		         SELECT 1
		           FROM app.message_provenance mp
		           JOIN app.mailboxes mb
		             ON mb.user_id = mp.user_id AND mb.id = mp.mailbox_id
		          WHERE mp.user_id = m.user_id AND mp.message_id = m.id
		            AND mp.state = 'live' AND upper(mb.imap_name) = 'INBOX'
		       ) AS in_inbox,
		       COALESCE((
		         SELECT array_agg(l.name ORDER BY l.name)
		           FROM app.message_labels ml
		           JOIN app.labels l
		             ON l.user_id = ml.user_id AND l.id = ml.label_id
		          WHERE ml.user_id = m.user_id AND ml.message_id = m.id
		       ), '{}'::text[]) AS labels,
		       (SELECT l.tab::text
		          FROM app.message_labels ml
		          JOIN app.labels l
		            ON l.user_id = ml.user_id AND l.id = ml.label_id
		         WHERE ml.user_id = m.user_id AND ml.message_id = m.id
		           AND l.tab IS NOT NULL
		         ORDER BY array_position($4::text[], l.tab::text)
		         LIMIT 1) AS mapping_tab,
		       m.classified_tab::text,
		       (SELECT t.score FROM app.triage_scores t
		         WHERE t.user_id = m.user_id AND t.message_id = m.id) AS score
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.id > $2
		   AND NOT m.is_junk AND m.trashed_at IS NULL AND m.vanished_at IS NULL
		   AND NOT m.is_draft
		 ORDER BY m.id
		 LIMIT $3`,
		tx.userID.String(), after.String(), limit, tabs.PrecedenceNames())
	if err != nil {
		return nil, fmt.Errorf("store: walk area report rows: %w", err)
	}
	defer rows.Close()

	var out []AreaReportRow
	for rows.Next() {
		var (
			id         string
			labels     []string
			mapped     *string
			classified *string
			score      *float32
		)
		var r AreaReportRow
		if err := rows.Scan(&id, &r.InInbox, &labels, &mapped, &classified, &score); err != nil {
			return nil, fmt.Errorf("store: scan area report row: %w", err)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, err)
		}
		r.ID = parsed
		r.Labels = labels
		if mapped != nil && *mapped != "" {
			t, ok := tabs.Parse(*mapped)
			if !ok {
				return nil, fmt.Errorf("store: unknown category tab %q (mapped) on message %s", *mapped, id)
			}
			r.MappedTab, r.MappedKnown = t, true
		}
		if classified != nil && *classified != "" {
			t, ok := tabs.Parse(*classified)
			if !ok {
				return nil, fmt.Errorf("store: unknown category tab %q (classified) on message %s", *classified, id)
			}
			r.ClassifiedTab, r.ClassifiedKnown = t, true
		}
		if score != nil {
			r.Score, r.Scored = *score, true
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: walk area report rows: %w", err)
	}
	return out, nil
}
