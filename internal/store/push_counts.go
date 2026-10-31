// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
)

// THE PUSH COUNTER (#59): of the messages one sync pass brought in, how many
// are above the line, how many are from a VIP, and how many are either. The
// notifier (internal/syncrunner) reads these once per pass and per account,
// and internal/notify decides, per device, which count that device is told.
//
// "ABOVE THE LINE" IS THE DESK'S OWN RULE: lineSideSQL against the caller's
// DeskPreferences.LineThreshold — the same expression `is:important` and the
// desk's surface use, so a push can never call "important" what the desk
// files in a drawer. A message nothing has judged (no score, no placement)
// has no side and is NOT above the line: in an above-the-line mode it does not
// push. That is the mode's meaning, and the settings copy says so.
//
// ONE ADDITION, deliberately, and only here: a message carrying the
// `$important` keyword counts as above the line FOR PUSH. That keyword is what
// the "Mark important" rule action writes (rules.KeywordImportant), and until
// #59 nothing read it (#626 §E) — a rule a user wrote to say "this matters"
// that did nothing at all. Push is the one place where the rule's meaning is
// unambiguous ("tell me about this"), so it is honoured here. It is NOT folded
// into lineSideSQL: that expression is also the drawer sweep's candidate
// predicate and the desk's split, where a keyword overriding the user's own
// Shift+↑/↓ placement would be a second, silent precedence rule.
//
// "FROM A VIP" is fromVIPSQL — the same subquery `is:vip` compiles to.

// PushCounts is the breakdown of one batch of new messages.
type PushCounts struct {
	// Found is how many of the ids exist for the caller (all of them, unless a
	// message was removed between ingest and the push).
	Found          int
	AboveLine      int
	VIP            int
	AboveLineOrVIP int
}

// PushCounts counts ids by push category, against the caller's line at
// threshold. RLS-scoped and bound to the caller's own user id: another user's
// message id counts as nothing. An empty batch costs no query.
func (tx *UserTx) PushCounts(ctx context.Context, ids []ulid.ULID, threshold float32) (PushCounts, error) {
	var c PushCounts
	if len(ids) == 0 {
		return c, nil
	}
	err := tx.tx.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE s.imp),
		       count(*) FILTER (WHERE s.vip),
		       count(*) FILTER (WHERE s.imp OR s.vip)
		  FROM (
			SELECT (((`+lineSideSQL("t.score", "$3")+` = 'surface') IS TRUE)
			        OR $4 = ANY(m.keywords)) AS imp,
			       `+fromVIPSQL+` AS vip
			  FROM app.messages m
			  LEFT JOIN app.triage_scores t ON t.user_id = m.user_id AND t.message_id = m.id
			 WHERE m.user_id = $1 AND m.id = ANY($2)
		  ) s`,
		tx.userID.String(), ulidStrings(ids), threshold, rules.KeywordImportant).
		Scan(&c.Found, &c.AboveLine, &c.VIP, &c.AboveLineOrVIP)
	if err != nil {
		return PushCounts{}, fmt.Errorf("store: push counts: %w", err)
	}
	return c, nil
}
