// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"
)

// THE DRAWER AUTO-ARCHIVE JOB (mailroom#443, Direction 7 "Desk and Drawers",
// #372, migration 00048): below the line, each area's DRAWER holds mail the
// desk judged unimportant. A drawer AUTOMATICALLY ARCHIVES what has sat in it
// longer than the area's period (default 30 days, per-area configurable).
// Archive is a MOVE ON THE SERVER to the account's Archive folder, so every
// other client agrees about what is in the inbox. NEVER DELETE. NEVER MARK AS
// READ — only an explicit user rule may do either. The in-tray (flagged) and
// everything above the line are never touched, whatever their age.
//
// The rules engine has no notion of age, so this is a durable job in the
// shape of the purge sweep (internal/purge), not a rule — see
// internal/drawer for the worker that calls this file's functions on a
// schedule, and `mailroom-admin drawer-report` for the report-first preview
// (CLAUDE.md: "build the report before the fix" — the first run on a real
// mailbox is a large, visible writeback event and must never be discovered).

// DrawerJobKind is the durable-job kind the drawer sweep worker registers
// for.
const DrawerJobKind = "drawer_sweep"

// DrawerCandidate is one message DrawerCandidates found ready to archive: old
// enough, on the desk, unflagged, unpinned, unsnoozed, BELOW THE LINE (the
// user's own placement if they made one, else its score — lineSideSQL, #464),
// in an auto-archiving area, and not already swept.
type DrawerCandidate struct {
	ID         ulid.ULID
	AreaID     ulid.ULID
	AreaName   string
	PeriodDays int
	ReceivedAt time.Time
}

// drawerCandidateWhereSQL returns the predicate DrawerCandidates and
// ArchiveByDrawer's re-check both share — live, canonical, on the desk,
// unflagged, unpinned, unsnoozed, IN THE DRAWER, and not already swept — so
// the two callers (a full scan, and a single-id recheck) build the IDENTICAL
// predicate rather than two hand-copies that could drift (the same
// one-implementation reasoning areaJoinSQL and deskLiveSQL already give).
// `m` is the message alias and `t` the LEFT-JOINed app.triage_scores alias;
// bind registers a parameter and returns its placeholder.
//
// ⚠ "IN THE DRAWER" IS lineSideSQL'S ANSWER, NOT `t.score < threshold` (#464).
// The sweep must obey the user's own placement, and in BOTH directions: a
// message they pulled above the line is never swept however low it scores —
// archiving what somebody just said was important is the worst thing this job
// could do — and one they pushed into the drawer by hand IS swept once it is
// old enough, whatever its score, including a message nothing ever scored.
// That second half is the deliberate exception to "unscored mail is not a
// candidate" (see DrawerCandidates): with no override the desk has no verdict
// about unscored mail, but a placement IS a verdict, and it is the user's.
//
// Age is deliberately NOT part of this predicate: DrawerCandidates adds its
// own "old enough" clause (the period varies per area), and ArchiveByDrawer's
// re-check omits it on purpose — the message was old enough a moment ago,
// and age can only increase between the scan and the write.
func drawerCandidateWhereSQL(bind func(any) string, threshold float32) string {
	return `NOT m.is_junk AND m.trashed_at IS NULL AND m.vanished_at IS NULL AND NOT m.is_draft
	          AND m.moved_mailbox_id IS NULL
	          AND NOT m.is_flagged
	          AND m.pinned_at IS NULL
	          AND m.snoozed_until IS NULL
	          AND ` + lineSideSQL("t.score", bind(threshold)) + ` = 'drawer'
	          AND NOT EXISTS (SELECT 1 FROM app.drawer_archives da
	                           WHERE da.user_id = m.user_id AND da.message_id = m.id)`
}

// hasArchiveMailboxSQL returns "EXISTS(...)" — whether ANY of message m's
// LIVE accounts has a mailbox with the Archive SPECIAL-USE role. It matches
// roleForSpecialUse("archive") exactly (jmap_mailbox.go): that function strips
// a leading backslash and lowercases, so this strips and lowercases too — a
// server that advertises `Archive` rather than `\Archive` must resolve the
// same way on both sides, or the report would count "no Archive folder" for
// mail ArchiveByDrawer then happily moves. Done in bulk here (DrawerCandidates
// and DrawerReport evaluate it across many messages) rather than per message
// in Go; ArchiveByDrawer, which has ONE message loaded, re-checks through
// MailboxForRole — same definition, different caller.
func hasArchiveMailboxSQL() string {
	return `EXISTS (
	          SELECT 1
	            FROM app.message_provenance mp2
	            JOIN app.mailboxes mb2 ON mb2.user_id = mp2.user_id AND mb2.account_id = mp2.account_id
	           WHERE mp2.user_id = m.user_id AND mp2.message_id = m.id AND mp2.state = 'live'
	             AND lower(ltrim(mb2.special_use, '\')) = 'archive')`
}

// DrawerCandidates returns up to limit messages ready for the drawer sweep to
// archive, oldest received_at first, across every area whose drawer
// auto-archives. ONE query: for every area with auto_archive, period =
// COALESCE(a.auto_archive_days, the login default), and a message qualifies
// when it is live, canonical, on the desk (deskLiveSQL — the same predicate
// AreaSummary's counts use), resolves to THIS area (areaJoinSQL — the same
// resolver Email.area and the summary counts use), unflagged, unpinned,
// unsnoozed, below the line (lineSideSQL — the same expression those counts
// split on), old enough for its area's period, and not already in
// app.drawer_archives — AND reachable: at least one of its live
// accounts has an Archive mailbox (hasArchiveMailboxSQL). That last predicate
// is here, not only in ArchiveByDrawer's re-check, for two reasons: the
// report's candidate count must be THIS query's count (a message the report
// files under "no Archive folder" cannot also be a candidate here), and a
// batch of unarchivable candidates must not starve the archivable ones behind
// them — with LIMIT 200 and an account that has no Archive folder, the sweep
// would otherwise select the same 200 dead candidates every run and never
// reach the live ones.
//
// UNSCORED MAIL IS NOT A CANDIDATE: the desk has no verdict about it, so the
// sweep has no basis to say it belongs in the drawer at all (spec) — UNLESS
// the user put it there themselves (#464). A placement is a verdict, and the
// user's outranks the model's; see drawerCandidateWhereSQL.
func (tx *UserTx) DrawerCandidates(ctx context.Context, now time.Time, limit int) ([]DrawerCandidate, error) {
	prefs, err := tx.GetDeskPreferences(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: drawer candidates (desk preferences): %w", err)
	}

	args := []any{tx.userID.String()}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	areaJoins, _, _ := areaJoinSQL(bind, nil)
	deskLive := deskLiveSQL(bind, tx.userID, nil)
	cond := drawerCandidateWhereSQL(bind, prefs.LineThreshold)
	defaultPH := bind(prefs.DrawerDefaultDays)
	nowPH := bind(now.UTC())
	limitPH := bind(limit)

	query := `
		SELECT m.id, a.id, a.name, COALESCE(a.auto_archive_days, ` + defaultPH + `) AS period_days, m.received_at
		  FROM app.messages m` + areaJoins + `
		  JOIN app.areas a ON a.user_id = m.user_id AND a.id = ` + areaResolvedSQL + ` AND a.auto_archive
		  LEFT JOIN app.triage_scores t ON t.user_id = m.user_id AND t.message_id = m.id
		 WHERE m.user_id = $1
		   AND ` + deskLive + `
		   AND ` + cond + `
		   AND ` + hasArchiveMailboxSQL() + `
		   AND m.received_at < ` + nowPH + `::timestamptz - (COALESCE(a.auto_archive_days, ` + defaultPH + `) * interval '1 day')
		 ORDER BY m.received_at ASC
		 LIMIT ` + limitPH

	rows, err := tx.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: drawer candidates: %w", err)
	}
	defer rows.Close()

	var out []DrawerCandidate
	for rows.Next() {
		var (
			c                   DrawerCandidate
			msgIDStr, areaIDStr string
		)
		if err := rows.Scan(&msgIDStr, &areaIDStr, &c.AreaName, &c.PeriodDays, &c.ReceivedAt); err != nil {
			return nil, fmt.Errorf("store: scan drawer candidate: %w", err)
		}
		if c.ID, err = ulid.Parse(msgIDStr); err != nil {
			return nil, fmt.Errorf("store: parse drawer candidate message id %q: %w", msgIDStr, err)
		}
		if c.AreaID, err = ulid.Parse(areaIDStr); err != nil {
			return nil, fmt.Errorf("store: parse drawer candidate area id %q: %w", areaIDStr, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: drawer candidates: %w", err)
	}
	return out, nil
}

// drawerCandidateStillQualifies re-checks, BY ID, that a message ArchiveByDrawer
// was handed a moment ago still qualifies: the same predicate DrawerCandidates
// uses, minus age (it was old enough when it was selected, and age only grows),
// plus a check that it still resolves to areaID and that area still
// auto-archives — either could have changed between the scan and this write
// (the user moved it, relabelled it, or flipped the area's switch).
func (tx *UserTx) drawerCandidateStillQualifies(ctx context.Context, id, areaID ulid.ULID, threshold float32) (bool, error) {
	args := []any{tx.userID.String(), id.String(), areaID.String()}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	areaJoins, _, _ := areaJoinSQL(bind, nil)
	deskLive := deskLiveSQL(bind, tx.userID, nil)
	cond := drawerCandidateWhereSQL(bind, threshold)

	query := `
		SELECT EXISTS (
		  SELECT 1
		    FROM app.messages m
		    JOIN app.areas a ON a.user_id = m.user_id AND a.id = $3 AND a.auto_archive
		    LEFT JOIN app.triage_scores t ON t.user_id = m.user_id AND t.message_id = m.id` + areaJoins + `
		   WHERE m.user_id = $1 AND m.id = $2
		     AND ` + areaResolvedSQL + ` = a.id
		     AND ` + deskLive + `
		     AND ` + cond + `
		)`
	var ok bool
	if err := tx.tx.QueryRow(ctx, query, args...).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: drawer candidate recheck: %w", err)
	}
	return ok, nil
}

// ArchiveByDrawer archives one drawer candidate, IN THE CALLER'S TRANSACTION:
// it re-checks the message still qualifies (drawerCandidateStillQualifies)
// and that at least one of its live accounts has an Archive mailbox
// (MailboxForRole); if either is false it returns (false, nil) — nothing is
// written, and nothing is enqueued. Only then does it call ApplyEmailUpdate
// with MoveToRole "archive" — which sets moved_mailbox_id, appends the ONE
// Email `updated` change row, and enqueues the per-account IMAP move intent
// (this IS the accepted writeback event, spec) — and records the sweep in
// app.drawer_archives so the same message is never offered again.
//
// ⚠ IT MUST NOT TOUCH is_seen. ApplyEmailUpdate's EmailUpdate here sets only
// MoveToRole; Seen stays nil, so is_seen is left exactly as it was — asserted
// directly in TestArchiveByDrawerMovesAndRecordsAndNothingElse.
//
// ⚠ IT NEVER DELETES ANYTHING, by construction: there is no DELETE statement
// anywhere on this path, only an UPDATE (the move) and an INSERT (the audit
// row).
func (tx *UserTx) ArchiveByDrawer(ctx context.Context, c DrawerCandidate, now time.Time) (archived bool, err error) {
	prefs, err := tx.GetDeskPreferences(ctx)
	if err != nil {
		return false, err
	}
	ok, err := tx.drawerCandidateStillQualifies(ctx, c.ID, c.AreaID, prefs.LineThreshold)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	accounts, err := tx.liveAccountIDs(ctx, c.ID)
	if err != nil {
		return false, err
	}
	hasArchive := false
	for _, acct := range accounts {
		_, found, ferr := tx.MailboxForRole(ctx, acct, "archive")
		if ferr != nil {
			return false, ferr
		}
		if found {
			hasArchive = true
			break
		}
	}
	if !hasArchive {
		return false, nil
	}

	found, _, err := tx.ApplyEmailUpdate(ctx, c.ID, EmailUpdate{MoveToRole: "archive"})
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.drawer_archives (user_id, message_id, area_id, area_name, period_days, archived_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, message_id) DO NOTHING`,
		tx.userID.String(), c.ID.String(), c.AreaID.String(), c.AreaName, c.PeriodDays, now.UTC(),
	); err != nil {
		return false, fmt.Errorf("store: archive by drawer (record): %w", err)
	}
	return true, nil
}

// DrawerArchivedAt returns, for the given message ids, WHEN a drawer sweep
// archived them. Ids never swept are simply absent from the map — this is
// how Email.drawerArchivedAt renders null. One round trip, matching
// resolveAreas/ResolveAreas' own shape.
func (tx *UserTx) DrawerArchivedAt(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID]time.Time, error) {
	out := make(map[ulid.ULID]time.Time, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, archived_at FROM app.drawer_archives
		 WHERE user_id = $1 AND message_id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: drawer archived at: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var midStr string
		var at time.Time
		if err := rows.Scan(&midStr, &at); err != nil {
			return nil, fmt.Errorf("store: scan drawer archived at: %w", err)
		}
		mid, err := ulid.Parse(midStr)
		if err != nil {
			return nil, fmt.Errorf("store: parse drawer archived message id %q: %w", midStr, err)
		}
		out[mid] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: drawer archived at: %w", err)
	}
	return out, nil
}

// DrawerArchiveCounts returns, per area id, how many messages a drawer sweep
// has EVER archived (all time) — Area/getSummary's archivedByDrawer and
// Settings' "archived N so far". The area_name-but-NULL-area_id rows of a
// since-deleted area (ON DELETE SET NULL, migration 00048) are simply not
// counted per area here: there is no area left to attribute them to, and
// app.drawer_archives itself keeps their history legible by name.
func (tx *UserTx) DrawerArchiveCounts(ctx context.Context) (map[ulid.ULID]int, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT area_id, count(*) FROM app.drawer_archives
		 WHERE user_id = $1 AND area_id IS NOT NULL
		 GROUP BY area_id`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: drawer archive counts: %w", err)
	}
	defer rows.Close()
	out := map[ulid.ULID]int{}
	for rows.Next() {
		var areaIDStr string
		var n int
		if err := rows.Scan(&areaIDStr, &n); err != nil {
			return nil, fmt.Errorf("store: scan drawer archive count: %w", err)
		}
		areaID, err := ulid.Parse(areaIDStr)
		if err != nil {
			return nil, fmt.Errorf("store: parse drawer archive area id %q: %w", areaIDStr, err)
		}
		out[areaID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: drawer archive counts: %w", err)
	}
	return out, nil
}

// DrawerPeriodSource says where an area's effective drawer period came from —
// the report's "30 d" / "30 d (default)" / "off" column.
type DrawerPeriodSource string

const (
	// DrawerPeriodOwn is an area's own auto_archive_days override.
	DrawerPeriodOwn DrawerPeriodSource = "own"
	// DrawerPeriodDefault is desk_preferences.drawer_default_days, inherited
	// because the area names no override of its own.
	DrawerPeriodDefault DrawerPeriodSource = "default"
	// DrawerPeriodOff means the area's drawer does not auto-archive at all
	// (auto_archive = false) — the period shown is what WOULD apply if it did.
	DrawerPeriodOff DrawerPeriodSource = "off"
)

// DrawerExclusions counts, among an area's messages that are old enough and
// on the desk, how many are excluded by EACH signal — never one total
// (CLAUDE.md): a single number cannot tell "everything here is flagged" from
// "everything here already scored above the line" apart, and an operator
// would act on those two findings completely differently. A message can be
// counted under more than one signal (e.g. flagged AND pinned); these are
// independent tallies, not a partition.
type DrawerExclusions struct {
	Flagged int
	Pinned  int
	Snoozed int
	// AboveLine and Unscored are lineSideSQL's 'surface' and NULL (#464) — the
	// SAME expression Candidates' own "in the drawer" test uses, so the three
	// answer one question about one message. Reading them off the raw score
	// instead would let a message the user pushed into the drawer count as a
	// candidate AND as "excluded: above the line" in the same row, which is a
	// report contradicting itself about what the sweep is going to do.
	//
	// So AboveLine reads "the desk or the user put it above the line" and
	// Unscored reads "nothing has judged it and nobody has placed it".
	AboveLine       int
	Unscored        int
	AlreadySwept    int
	NoArchiveFolder int
}

// DrawerAreaReport is one area's row of DrawerReport, or the trailing
// Needs-sorting row (AreaID nil) — which is never swept at all (nothing
// resolves a Needs-sorting message to an area to sweep it against), so its
// only meaningful field is Candidates: how much desk-live mail currently has
// no area, regardless of age.
type DrawerAreaReport struct {
	AreaID       *ulid.ULID
	AreaName     string
	PeriodDays   int
	PeriodSource DrawerPeriodSource
	// Candidates is how many messages would actually be archived right now —
	// old enough, on the desk, in this area, and excluded by NONE of the
	// signals below.
	Candidates int
	Oldest     *time.Time
	Newest     *time.Time
	Excluded   DrawerExclusions
}

// DrawerReportRows is the whole drawer-report: one row per area, in position
// order, plus the trailing Needs-sorting row.
type DrawerReportRows struct {
	Areas        []DrawerAreaReport
	NeedsSorting DrawerAreaReport
}

// DrawerReport answers, per area, what a sweep run right now would do and
// why messages that look old enough are not candidates — the report
// `mailroom-admin drawer-report` prints BEFORE the first sweep ever runs
// (CLAUDE.md: "build the report before the fix"). Per area: the effective
// period and its source, the true candidate count with its oldest/newest
// received_at, and — among messages that are old enough and on the desk in
// this area — the count excluded by each signal. Per-signal, never one
// total.
func (tx *UserTx) DrawerReport(ctx context.Context, now time.Time) (DrawerReportRows, error) {
	prefs, err := tx.GetDeskPreferences(ctx)
	if err != nil {
		return DrawerReportRows{}, fmt.Errorf("store: drawer report (desk preferences): %w", err)
	}

	args := []any{tx.userID.String(), now.UTC(), prefs.DrawerDefaultDays}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	areaJoins, _, _ := areaJoinSQL(bind, nil)
	deskLive := deskLiveSQL(bind, tx.userID, nil)
	hasArchive := hasArchiveMailboxSQL()
	thresholdPH := bind(prefs.LineThreshold)

	// `base`: every live, canonical, on-the-desk message, with its resolved
	// area, its side of the line (lineSideSQL — the user's placement if they
	// made one, else the score, exactly as the sweep itself reads it) and the
	// raw facts every other signal below is computed from. `by_area`
	// joins it back to app.areas so the "old enough" cutoff can use EACH
	// area's own effective period (COALESCE(auto_archive_days, default)).
	qualifies := `NOT b.is_flagged AND b.pinned_at IS NULL AND b.snoozed_until IS NULL
	                AND b.side = 'drawer'
	                AND NOT b.already_swept AND NOT b.no_archive`
	query := `
		WITH base AS (
			SELECT m.id, ` + areaResolvedSQL + ` AS area_id, m.is_flagged, m.pinned_at,
			       m.snoozed_until, ` + lineSideSQL("t.score", thresholdPH) + ` AS side, m.received_at,
			       EXISTS (SELECT 1 FROM app.drawer_archives da
			                WHERE da.user_id = m.user_id AND da.message_id = m.id) AS already_swept,
			       NOT ` + hasArchive + ` AS no_archive
			  FROM app.messages m
			  LEFT JOIN app.triage_scores t ON t.user_id = m.user_id AND t.message_id = m.id` + areaJoins + `
			 WHERE m.user_id = $1
			   AND NOT m.is_junk AND m.trashed_at IS NULL AND m.vanished_at IS NULL AND NOT m.is_draft
			   AND m.moved_mailbox_id IS NULL
			   AND ` + deskLive + `
		)
		SELECT a.id, a.name, a.auto_archive, a.auto_archive_days,
		       count(b.id) FILTER (WHERE ` + qualifies + `) AS candidates,
		       min(b.received_at) FILTER (WHERE ` + qualifies + `) AS oldest,
		       max(b.received_at) FILTER (WHERE ` + qualifies + `) AS newest,
		       count(b.id) FILTER (WHERE b.is_flagged) AS flagged,
		       count(b.id) FILTER (WHERE b.pinned_at IS NOT NULL) AS pinned,
		       count(b.id) FILTER (WHERE b.snoozed_until IS NOT NULL) AS snoozed,
		       count(b.id) FILTER (WHERE b.side = 'surface') AS above_line,
		       count(b.id) FILTER (WHERE b.side IS NULL) AS unscored,
		       count(b.id) FILTER (WHERE b.already_swept) AS already_swept,
		       count(b.id) FILTER (WHERE b.no_archive) AS no_archive
		  FROM app.areas a
		  LEFT JOIN base b
		    ON b.area_id = a.id
		   AND b.received_at < $2::timestamptz - (COALESCE(a.auto_archive_days, $3) * interval '1 day')
		 WHERE a.user_id = $1
		 GROUP BY a.id, a.name, a.position, a.auto_archive, a.auto_archive_days
		 ORDER BY a.position, a.id`

	rows, err := tx.tx.Query(ctx, query, args...)
	if err != nil {
		return DrawerReportRows{}, fmt.Errorf("store: drawer report: %w", err)
	}
	var out DrawerReportRows
	for rows.Next() {
		var (
			r               DrawerAreaReport
			areaIDStr, name string
			autoArchive     bool
			autoArchiveDays *int
		)
		if err := rows.Scan(&areaIDStr, &name, &autoArchive, &autoArchiveDays,
			&r.Candidates, &r.Oldest, &r.Newest,
			&r.Excluded.Flagged, &r.Excluded.Pinned, &r.Excluded.Snoozed,
			&r.Excluded.AboveLine, &r.Excluded.Unscored,
			&r.Excluded.AlreadySwept, &r.Excluded.NoArchiveFolder); err != nil {
			rows.Close()
			return DrawerReportRows{}, fmt.Errorf("store: scan drawer report row: %w", err)
		}
		areaID, perr := ulid.Parse(areaIDStr)
		if perr != nil {
			rows.Close()
			return DrawerReportRows{}, fmt.Errorf("store: parse drawer report area id %q: %w", areaIDStr, perr)
		}
		r.AreaID = &areaID
		r.AreaName = name
		switch {
		case !autoArchive:
			r.PeriodSource = DrawerPeriodOff
		case autoArchiveDays != nil:
			r.PeriodSource = DrawerPeriodOwn
		default:
			r.PeriodSource = DrawerPeriodDefault
		}
		if autoArchiveDays != nil {
			r.PeriodDays = *autoArchiveDays
		} else {
			r.PeriodDays = prefs.DrawerDefaultDays
		}
		out.Areas = append(out.Areas, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DrawerReportRows{}, fmt.Errorf("store: drawer report: %w", err)
	}
	rows.Close()

	// NEEDS SORTING is never swept (nothing resolves an unsorted message to an
	// area to sweep it against) — its only figure is how much desk-live mail
	// currently has no area at all, with no age filter: it will never age into
	// eligibility regardless.
	uArgs := []any{tx.userID.String()}
	ubind := func(v any) string {
		uArgs = append(uArgs, v)
		return "$" + strconv.Itoa(len(uArgs))
	}
	uAreaJoins, _, _ := areaJoinSQL(ubind, nil)
	uDeskLive := deskLiveSQL(ubind, tx.userID, nil)
	var unsorted int
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM app.messages m`+uAreaJoins+`
		 WHERE m.user_id = $1
		   AND NOT m.is_junk AND m.trashed_at IS NULL AND m.vanished_at IS NULL AND NOT m.is_draft
		   AND m.moved_mailbox_id IS NULL
		   AND `+uDeskLive+`
		   AND `+areaResolvedSQL+` IS NULL`, uArgs...).Scan(&unsorted); err != nil {
		return DrawerReportRows{}, fmt.Errorf("store: drawer report (needs sorting): %w", err)
	}
	out.NeedsSorting = DrawerAreaReport{AreaName: "Needs sorting", Candidates: unsorted}

	return out, nil
}
