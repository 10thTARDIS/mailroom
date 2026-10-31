// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/areas"
	"mailroom/internal/tabs"
)

// THE DESK'S AREAS — the persistent object half of #442 increment 1 (migration
// 00046). Increment 0 shipped the resolver (internal/areas) and a read-only
// report over it; this file is the store layer over the object that report was
// rehearsing for: every message resolves into exactly one AREA, fed by tabs
// and/or labels, and an area OWNS a label of its own ("areas are labels" —
// placing a message in an area is applying that label).
//
// A CHANGE ROW PER MUTATION, the load-bearing invariant (CLAUDE.md): every
// Create/Update/Delete here appends exactly one Area change_log row when
// something actually changed, in the same transaction as the write. A FEED
// change is a mutation of the OWNING AREA for JMAP purposes — area_feeds
// carries no object type of its own, so a feed add/remove folds into the same
// single Area row an Update already appends, never a row of its own.

// AreaFeed is one thing that routes mail into an area: exactly one of Tab or
// LabelID is set, mirroring the database's own area_feeds_exactly_one CHECK.
// A zero value (neither set) is never valid input and is rejected before any
// statement runs (validateAreaFeed).
type AreaFeed struct {
	Tab     tabs.Tab
	LabelID *ulid.ULID
}

// AreaView is the canonical Area projection. Feeds INCLUDES the area's own
// label feed (the row areaJoinSQL's label-fed arm actually matches on) — a
// caller does not need to special-case LabelID against Feeds to see the whole
// picture.
type AreaView struct {
	ID       ulid.ULID
	Name     string
	Position int
	LabelID  ulid.ULID
	// AutoArchive is whether this drawer sweeps at all (#443, migration 00048;
	// default true). AutoArchiveDays is only the PERIOD, not the on/off switch.
	AutoArchive bool
	// AutoArchiveDays is the drawer's dwell period in days; NULL = inherit
	// desk_preferences.drawer_default_days rather than "never" — "never" is
	// AutoArchive=false.
	AutoArchiveDays *int
	HiddenFromRail  bool
	Feeds           []AreaFeed
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AreaRow is the input to CreateArea. LabelID nil means "mint a new label
// named Name, MirrorToIMAP false, unmapped from any tab, and let the area own
// it" — the ordinary case for an area a person creates from the UI, where
// there is no pre-existing label to reuse. Feeds are ADDITIONAL feeds beyond
// the own label, which CreateArea always adds itself.
type AreaRow struct {
	Name     string
	Position int
	// AutoArchive is nil -> true (#443: a new drawer sweeps by default). Set to
	// a non-nil false to create an area whose drawer never auto-archives.
	AutoArchive     *bool
	AutoArchiveDays *int
	HiddenFromRail  bool
	LabelID         *ulid.ULID
	Feeds           []AreaFeed
}

// AreaPatch is a partial update for UpdateArea. A nil pointer means "leave
// unchanged". SetAutoArchiveDays distinguishes "leave it" (false) from "set
// it to AutoArchiveDays, including clearing it to nil" (true) — the same
// shape LabelPatch's SetColor/SetParent use for a field whose valid values
// include nil itself; SetAutoArchive is the analogous guard for the #443
// on/off switch. SetFeeds replaces the WHOLE feed set (beyond the own label,
// which UpdateArea always keeps regardless of what Feeds contains).
type AreaPatch struct {
	Name     *string
	Position *int
	// SetAutoArchive distinguishes "leave it" (false) from "set it to
	// AutoArchive" (true) — #443's on/off switch, independent of the period
	// below.
	SetAutoArchive     bool
	AutoArchive        bool
	SetAutoArchiveDays bool
	AutoArchiveDays    *int
	HiddenFromRail     *bool
	SetFeeds           bool
	Feeds              []AreaFeed
}

// ErrFeedTaken is returned when a feed (a tab or a label) already belongs to
// a DIFFERENT area than the one being written — the "at most one area" half of
// #442's decision, enforced by the database's two partial unique indexes
// (area_feeds_tab_once, area_feeds_label_once) and surfaced here with which
// feed and which area, rather than a bare constraint name. A feed already on
// the SAME area is a no-op, not an error — re-asserting your own state is not
// a conflict.
var ErrFeedTaken = errors.New("store: feed already belongs to another area")

// ErrAreaNotFound is returned when an area id does not resolve for the caller
// (RLS makes another user's area indistinguishable from a missing one).
var ErrAreaNotFound = errors.New("store: area not found")

// ErrAreaNameTaken is returned when a create/rename would collide with the
// caller's case-insensitive unique index on area names (areas_user_lower_name)
// — #442 increment 3's follow-up to increment 2, which returned this as a
// plain wrapped error with no sentinel to test against.
var ErrAreaNameTaken = errors.New("store: area name already exists")

// ErrAreaLabelTaken is returned when a create would reuse a label another area
// already owns as ITS OWN label (the UNIQUE (user_id, label_id) constraint on
// app.areas, named areas_user_id_label_id_key by Postgres) — distinct from
// ErrFeedTaken, which is about an ADDITIONAL feed rather than the own-label
// column, and unreachable from UpdateArea, which never writes label_id (it is
// immutable at the API layer, internal/api/area.go's updateArea).
var ErrAreaLabelTaken = errors.New("store: label already owned by another area")

// ErrAreaFeedInvalid is returned for an AreaFeed that sets neither or both of
// Tab/LabelID, or a Tab outside the closed vocabulary — checked in Go before
// any statement runs so the caller gets a sentence rather than the raw
// area_feeds_exactly_one constraint name.
var ErrAreaFeedInvalid = errors.New("store: an area feed must name exactly one tab or label")

// validateAreaFeed checks the Go-side half of area_feeds_exactly_one plus the
// closed tab vocabulary, before any round trip.
func validateAreaFeed(f AreaFeed) error {
	hasTab := f.Tab != ""
	hasLabel := f.LabelID != nil
	if hasTab == hasLabel {
		return fmt.Errorf("%w: %+v", ErrAreaFeedInvalid, f)
	}
	if hasTab && !f.Tab.Valid() {
		return fmt.Errorf("%w: %q is not a category tab", ErrAreaFeedInvalid, f.Tab)
	}
	return nil
}

// areaFeedSQLArgs renders f as the (tab, label) bind pair insertAreaFeed and
// areaFeedOwner both use: exactly one of the two is non-nil, matching
// area_feeds' own CHECK.
func areaFeedSQLArgs(f AreaFeed) (tabArg, labelArg any) {
	if f.Tab != "" {
		tabArg = string(f.Tab)
	}
	if f.LabelID != nil {
		labelArg = f.LabelID.String()
	}
	return tabArg, labelArg
}

// feedDescription renders f for ErrFeedTaken's detail.
func feedDescription(f AreaFeed) string {
	if f.Tab != "" {
		return fmt.Sprintf("tab %q", string(f.Tab))
	}
	return fmt.Sprintf("label %s", f.LabelID)
}

// feedKey is the map key CreateArea/replaceAreaFeeds use to dedupe/diff a set
// of feeds — a tab and a label can never collide with each other, so a simple
// prefix is enough.
func feedKey(f AreaFeed) string {
	if f.Tab != "" {
		return "tab:" + string(f.Tab)
	}
	return "label:" + f.LabelID.String()
}

// ListAreas returns every area for the caller in position order, MATERIALISING
// the default plan (one area per category tab) on first call — see
// materialiseDefaultAreas.
func (tx *UserTx) ListAreas(ctx context.Context) ([]AreaView, error) {
	if err := tx.materialiseDefaultAreas(ctx); err != nil {
		return nil, err
	}
	rows, err := tx.tx.Query(ctx, areaListSQL+` WHERE a.user_id = $1 GROUP BY a.id ORDER BY a.position, a.id`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list areas: %w", err)
	}
	defer rows.Close()
	return scanAreas(rows)
}

// GetAreas returns the area projections for the requested ids (RLS-scoped),
// also materialising first so a client that syncs by id before ever listing
// does not see an empty account (the same reasoning GetCategoryTabs gives).
// Ids not returned were not found.
func (tx *UserTx) GetAreas(ctx context.Context, ids []ulid.ULID) ([]AreaView, error) {
	if err := tx.materialiseDefaultAreas(ctx); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx,
		areaListSQL+` WHERE a.user_id = $1 AND a.id = ANY($2) GROUP BY a.id ORDER BY a.position, a.id`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: get areas: %w", err)
	}
	defer rows.Close()
	return scanAreas(rows)
}

// areaListSQL is the shared projection ListAreas/GetAreas each append a WHERE
// onto: the area's own columns plus its feeds, aggregated as two parallel
// arrays (tabs and label ids) rather than a second round trip per area.
const areaListSQL = `
	SELECT a.id, a.name, a.position, a.label_id, a.auto_archive, a.auto_archive_days,
	       a.hidden_from_rail, a.created_at, a.updated_at,
	       COALESCE(array_agg(f.tab::text) FILTER (WHERE f.tab IS NOT NULL), '{}') AS tabs,
	       COALESCE(array_agg(f.label_id::text) FILTER (WHERE f.label_id IS NOT NULL), '{}') AS label_ids
	  FROM app.areas a
	  LEFT JOIN app.area_feeds f ON f.user_id = a.user_id AND f.area_id = a.id`

// scanAreas drains an areaListSQL result set into AreaViews.
func scanAreas(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]AreaView, error) {
	var out []AreaView
	for rows.Next() {
		var (
			v                  AreaView
			id, labelID        string
			tabsRaw, labelsRaw []string
		)
		if err := rows.Scan(&id, &v.Name, &v.Position, &labelID, &v.AutoArchive, &v.AutoArchiveDays,
			&v.HiddenFromRail, &v.CreatedAt, &v.UpdatedAt, &tabsRaw, &labelsRaw); err != nil {
			return nil, fmt.Errorf("store: scan area: %w", err)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse area id %q: %w", id, err)
		}
		v.ID = parsed
		lid, err := ulid.Parse(labelID)
		if err != nil {
			return nil, fmt.Errorf("store: parse area label id %q: %w", labelID, err)
		}
		v.LabelID = lid
		for _, s := range tabsRaw {
			t, ok := tabs.Parse(s)
			if !ok {
				// Only reachable if the database carries a tab this binary does not
				// know — a downgrade past the migration that added it. Refused rather
				// than dropped, matching scanLabels' own reasoning.
				return nil, fmt.Errorf("store: unknown category tab %q on area %s", s, id)
			}
			v.Feeds = append(v.Feeds, AreaFeed{Tab: t})
		}
		for _, s := range labelsRaw {
			flid, err := ulid.Parse(s)
			if err != nil {
				return nil, fmt.Errorf("store: parse area feed label id %q: %w", s, err)
			}
			v.Feeds = append(v.Feeds, AreaFeed{LabelID: &flid})
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan areas: %w", err)
	}
	return out, nil
}

// CreateArea inserts an area, its own label (minted or reused per row.LabelID),
// and its feeds, appending exactly one Area created change row. A created own
// label goes through CreateLabel first, which appends its own Label created
// row — two objects, two rows, both required by the load-bearing invariant.
func (tx *UserTx) CreateArea(ctx context.Context, row AreaRow) (ulid.ULID, error) {
	for _, f := range row.Feeds {
		if err := validateAreaFeed(f); err != nil {
			return ulid.ULID{}, err
		}
	}

	areaID := NewULID()
	var autoArchiveDaysArg any
	if row.AutoArchiveDays != nil {
		autoArchiveDaysArg = *row.AutoArchiveDays
	}
	// #443: nil -> true, a new drawer sweeps by default.
	autoArchive := true
	if row.AutoArchive != nil {
		autoArchive = *row.AutoArchive
	}

	// THE OWN LABEL, THE AREA ROW AND ALL ITS FEEDS ARE ONE ATOMIC UNIT, under
	// a SINGLE outer SAVEPOINT — not one savepoint per statement. A name/label
	// collision on the area row, or ErrFeedTaken on the SECOND or later feed,
	// is a RECOVERABLE, caller-visible outcome (#442's own test recovers from
	// ErrFeedTaken and keeps using the SAME transaction), and without one
	// savepoint around the WHOLE sequence a caller doing exactly that would
	// commit a half-built area — a row, or a row plus one feed but not the
	// next — with no Area change_log row to show for it, silently breaking the
	// load-bearing invariant. The minted own label sits INSIDE the savepoint
	// for the same reason: a label that exists only because an area was about
	// to own it has no business surviving the area's failure to be created —
	// the user would find a stray "Receipts" label (and its Label change row)
	// after being told the area could not be made. CreateLabel's own internal
	// savepoint nests inside this one; PostgreSQL nests savepoints by issue
	// order regardless of which Go handle issues them.
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create area (savepoint): %w", err)
	}

	var labelID ulid.ULID
	if row.LabelID != nil {
		labelID = *row.LabelID
	} else {
		id, err := tx.CreateLabel(ctx, LabelRow{Name: row.Name, MirrorToIMAP: false})
		if err != nil {
			_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
			return ulid.ULID{}, fmt.Errorf("store: create area (own label): %w", err)
		}
		labelID = id
	}

	if _, err := sp.Exec(ctx, `
		INSERT INTO app.areas (id, user_id, name, position, label_id, auto_archive, auto_archive_days, hidden_from_rail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		areaID.String(), tx.userID.String(), row.Name, row.Position, labelID.String(),
		autoArchive, autoArchiveDaysArg, row.HiddenFromRail); err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		// #442 increment 3: the constraint NAME tells the two collisions apart,
		// where the old isUniqueViolation(err) check could only say "one of them
		// fired" — see ErrAreaNameTaken/ErrAreaLabelTaken and
		// internal/api/area.go's areaStoreSetError, which needed that distinction
		// to name the right JMAP property instead of both.
		if cname, ok := uniqueViolationConstraint(err); ok {
			switch cname {
			case "areas_user_lower_name":
				return ulid.ULID{}, fmt.Errorf("%w: %q", ErrAreaNameTaken, row.Name)
			case "areas_user_id_label_id_key":
				return ulid.ULID{}, fmt.Errorf("%w: %s", ErrAreaLabelTaken, labelID)
			}
		}
		return ulid.ULID{}, fmt.Errorf("store: create area: %w", err)
	}

	// The own label is always a feed — "areas are labels" (#442): placing a
	// message in the area IS applying this label, so areaJoinSQL's label-fed
	// arm must find the area through the exact same row a hand-added label
	// feed would use, not through areas.label_id alone.
	feeds := append([]AreaFeed{{LabelID: &labelID}}, row.Feeds...)
	for _, f := range feeds {
		if err := tx.insertAreaFeed(ctx, sp, areaID, f); err != nil {
			_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — undoes the area row too
			return ulid.ULID{}, err
		}
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return ulid.ULID{}, fmt.Errorf("store: create area (release): %w", err)
	}

	if _, err := tx.AppendChange(ctx, ObjectArea, areaID, OpCreated); err != nil {
		return ulid.ULID{}, err
	}
	return areaID, nil
}

// insertAreaFeed adds one feed to areaID, running every statement against
// exec — the caller's currently-open SAVEPOINT (or tx.tx itself when there is
// none yet to nest under) — so a multi-feed caller can roll back the WHOLE
// sequence atomically on a conflict rather than leaving earlier feeds
// committed. Postgres savepoints nest correctly by issue order regardless of
// which Go handle issues them, so insertAreaFeed's own inner SAVEPOINT here
// nests inside whatever the caller already has open.
//
// A feed already on areaID itself is a no-op (re-asserting your own state); a
// feed already on a DIFFERENT area is ErrFeedTaken.
func (tx *UserTx) insertAreaFeed(ctx context.Context, exec pgx.Tx, areaID ulid.ULID, f AreaFeed) error {
	tabArg, labelArg := areaFeedSQLArgs(f)
	sp, err := exec.Begin(ctx) // SAVEPOINT
	if err != nil {
		return fmt.Errorf("store: add area feed (savepoint): %w", err)
	}
	_, err = sp.Exec(ctx, `
		INSERT INTO app.area_feeds (id, user_id, area_id, tab, label_id)
		VALUES ($1, $2, $3, $4::app.category_tab, $5::app.ulid)`,
		NewULID().String(), tx.userID.String(), areaID.String(), tabArg, labelArg)
	if err == nil {
		if cerr := sp.Commit(ctx); cerr != nil { // RELEASE SAVEPOINT
			return fmt.Errorf("store: add area feed (release): %w", cerr)
		}
		return nil
	}
	_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the caller's scope stays usable
	if !isUniqueViolation(err) {
		return fmt.Errorf("store: add area feed: %w", err)
	}
	ownerID, ownerName, oerr := tx.areaFeedOwner(ctx, f)
	if oerr != nil {
		return oerr
	}
	if ownerID == areaID {
		return nil // re-asserting the area's own feed: a no-op, not a conflict
	}
	return fmt.Errorf("%w: %s already feeds %q", ErrFeedTaken, feedDescription(f), ownerName)
}

// areaFeedOwner finds which area currently holds feed f, for ErrFeedTaken's
// detail and for insertAreaFeed's same-area no-op check. A plain read: which
// handle it runs through does not matter, since every statement on one
// PostgreSQL transaction sees every earlier one's effects regardless of
// savepoint nesting.
func (tx *UserTx) areaFeedOwner(ctx context.Context, f AreaFeed) (ulid.ULID, string, error) {
	var (
		idStr, name string
		err         error
	)
	if f.Tab != "" {
		err = tx.tx.QueryRow(ctx, `
			SELECT a.id, a.name
			  FROM app.area_feeds f
			  JOIN app.areas a ON a.user_id = f.user_id AND a.id = f.area_id
			 WHERE f.user_id = $1 AND f.tab = $2::app.category_tab`,
			tx.userID.String(), string(f.Tab)).Scan(&idStr, &name)
	} else {
		err = tx.tx.QueryRow(ctx, `
			SELECT a.id, a.name
			  FROM app.area_feeds f
			  JOIN app.areas a ON a.user_id = f.user_id AND a.id = f.area_id
			 WHERE f.user_id = $1 AND f.label_id = $2::app.ulid`,
			tx.userID.String(), f.LabelID.String()).Scan(&idStr, &name)
	}
	if err != nil {
		return ulid.ULID{}, "", fmt.Errorf("store: find area feed owner: %w", err)
	}
	id, err := ulid.Parse(idStr)
	if err != nil {
		return ulid.ULID{}, "", fmt.Errorf("store: parse area id %q: %w", idStr, err)
	}
	return id, name, nil
}

// UpdateArea applies a partial update, appending exactly one Area updated
// change row when EITHER the scalar columns or the feed set actually changed
// (a no-op patch on both fronts appends none). found is false when the area
// does not exist for the caller; a feed conflict returns (true, ErrFeedTaken)
// since the area itself was found and locked before the conflict occurred —
// and, because the scalar update and the feed replacement share ONE outer
// SAVEPOINT, a feed conflict rolls back a scalar change already applied in the
// SAME call too. Without that a caller that recovers from ErrFeedTaken
// (#442's own test does exactly this, to prove the surrounding transaction
// survives) could commit half of one AreaPatch — a rename with no feed
// change, say — with no change_log row to show for it.
func (tx *UserTx) UpdateArea(ctx context.Context, id ulid.ULID, patch AreaPatch) (found bool, err error) {
	if patch.SetFeeds {
		for _, f := range patch.Feeds {
			if err := validateAreaFeed(f); err != nil {
				return false, err
			}
		}
	}

	var (
		name            string
		position        int
		labelID         string
		autoArchive     bool
		autoArchiveDays *int
		hidden          bool
	)
	err = tx.tx.QueryRow(ctx, `
		SELECT name, position, label_id, auto_archive, auto_archive_days, hidden_from_rail
		  FROM app.areas
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`,
		tx.userID.String(), id.String()).Scan(&name, &position, &labelID, &autoArchive, &autoArchiveDays, &hidden)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: update area (read): %w", err)
	}
	ownLabelID, perr := ulid.Parse(labelID)
	if perr != nil {
		return false, fmt.Errorf("store: parse area label id %q: %w", labelID, perr)
	}

	nName, nPosition, nHidden := name, position, hidden
	nAutoArchive := autoArchive
	nAutoArchiveDays := autoArchiveDays
	if patch.Name != nil {
		nName = *patch.Name
	}
	if patch.Position != nil {
		nPosition = *patch.Position
	}
	if patch.HiddenFromRail != nil {
		nHidden = *patch.HiddenFromRail
	}
	if patch.SetAutoArchive {
		nAutoArchive = patch.AutoArchive
	}
	if patch.SetAutoArchiveDays {
		nAutoArchiveDays = patch.AutoArchiveDays
	}
	var autoArchiveDaysArg any
	if nAutoArchiveDays != nil {
		autoArchiveDaysArg = *nAutoArchiveDays
	}

	// ONE outer SAVEPOINT for the whole mutation — the scalar UPDATE and (if
	// requested) the feed replacement both run against it, so either can be
	// undone by the other's failure without touching the surrounding UserTx.
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return false, fmt.Errorf("store: update area (savepoint): %w", err)
	}

	var touched string
	err = sp.QueryRow(ctx, `
		UPDATE app.areas
		   SET name = $3, position = $4, auto_archive = $5, auto_archive_days = $6,
		       hidden_from_rail = $7, updated_at = now()
		 WHERE user_id = $1 AND id = $2
		   AND (name, position, auto_archive, auto_archive_days, hidden_from_rail)
		       IS DISTINCT FROM ($3, $4, $5, $6, $7)
		RETURNING id`,
		tx.userID.String(), id.String(), nName, nPosition, nAutoArchive, autoArchiveDaysArg, nHidden).Scan(&touched)
	scalarChanged := true
	if err != nil {
		if isNoRows(err) {
			// The row exists (it is locked) but nothing changed: a true no-op. No
			// SQL error occurred, so sp stays open for the feed step below.
			scalarChanged = false
		} else {
			_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
			// Only areas_user_lower_name is reachable here: the UPDATE above never
			// touches label_id (it is immutable, see ErrAreaLabelTaken's doc), so
			// the own-label constraint cannot fire from this statement.
			if cname, ok := uniqueViolationConstraint(err); ok && cname == "areas_user_lower_name" {
				return true, fmt.Errorf("%w: %q", ErrAreaNameTaken, nName)
			}
			return false, fmt.Errorf("store: update area (write): %w", err)
		}
	}

	feedsChanged := false
	if patch.SetFeeds {
		feedsChanged, err = tx.replaceAreaFeeds(ctx, sp, id, ownLabelID, patch.Feeds)
		if err != nil {
			_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — undoes the scalar change too
			return true, err
		}
	}

	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return false, fmt.Errorf("store: update area (release): %w", err)
	}

	if scalarChanged || feedsChanged {
		if _, err := tx.AppendChange(ctx, ObjectArea, id, OpUpdated); err != nil {
			return false, err
		}
	}
	return true, nil
}

// replaceAreaFeeds implements AreaPatch.SetFeeds: a replace-all of areaID's
// feeds, with the own label (ownLabelID) ALWAYS kept regardless of whether
// desired names it explicitly — "areas are labels" means giving up the own
// label is deleting the area, never a feed edit. Every statement runs against
// exec — the caller's open SAVEPOINT — so UpdateArea can roll back the WHOLE
// replace (and any scalar change made in the same call) atomically on a
// conflict.
func (tx *UserTx) replaceAreaFeeds(ctx context.Context, exec pgx.Tx, areaID, ownLabelID ulid.ULID, desired []AreaFeed) (changed bool, err error) {
	own := AreaFeed{LabelID: &ownLabelID}
	want := map[string]AreaFeed{feedKey(own): own}
	for _, f := range desired {
		want[feedKey(f)] = f
	}

	rows, err := exec.Query(ctx, `
		SELECT tab::text, label_id::text FROM app.area_feeds
		 WHERE user_id = $1 AND area_id = $2`,
		tx.userID.String(), areaID.String())
	if err != nil {
		return false, fmt.Errorf("store: read area feeds: %w", err)
	}
	have := map[string]AreaFeed{}
	for rows.Next() {
		var tab, label *string
		if err := rows.Scan(&tab, &label); err != nil {
			rows.Close()
			return false, fmt.Errorf("store: scan area feed: %w", err)
		}
		var f AreaFeed
		if tab != nil {
			f.Tab = tabs.Tab(*tab)
		}
		if label != nil {
			lid, perr := ulid.Parse(*label)
			if perr != nil {
				rows.Close()
				return false, fmt.Errorf("store: parse area feed label id %q: %w", *label, perr)
			}
			f.LabelID = &lid
		}
		have[feedKey(f)] = f
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: read area feeds: %w", err)
	}

	for k, f := range have {
		if _, keep := want[k]; keep {
			continue
		}
		tabArg, labelArg := areaFeedSQLArgs(f)
		if _, err := exec.Exec(ctx, `
			DELETE FROM app.area_feeds
			 WHERE user_id = $1 AND area_id = $2
			   AND tab IS NOT DISTINCT FROM $3::app.category_tab
			   AND label_id IS NOT DISTINCT FROM $4::app.ulid`,
			tx.userID.String(), areaID.String(), tabArg, labelArg); err != nil {
			return false, fmt.Errorf("store: remove area feed: %w", err)
		}
		changed = true
	}
	for k, f := range want {
		if _, keep := have[k]; keep {
			continue
		}
		if err := tx.insertAreaFeed(ctx, exec, areaID, f); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// DeleteArea removes an area. Its feeds cascade (area_feeds.area_id ON DELETE
// CASCADE); the area's OWN LABEL is never touched — deleting the area is how
// you give up the label, and every message that carried it keeps the label and
// everything else about it. found is false when the area does not exist for
// the caller.
func (tx *UserTx) DeleteArea(ctx context.Context, id ulid.ULID) (found bool, err error) {
	var deleted string
	err = tx.tx.QueryRow(ctx,
		`DELETE FROM app.areas WHERE user_id = $1 AND id = $2 RETURNING id`,
		tx.userID.String(), id.String()).Scan(&deleted)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: delete area: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectArea, id, OpDestroyed); err != nil {
		return false, err
	}
	return true, nil
}

// materialiseDefaultAreas creates the DEFAULT PLAN — one area per category
// tab, in tabs.All order, named and fed exactly as internal/areas.Default()
// already specifies — the first time the caller has zero areas. Reusing
// areas.Default() rather than restating "one area per tab, name capitalised"
// keeps the naming in the one place AreaConfig (areas_resolve.go) and the
// agreement test both already depend on.
//
// LABEL REUSE, NOT A SECOND "PROMOTIONS": an operator who labelled mail by
// hand before areas ever existed plausibly already has a label named
// Promotions or Social. Minting a fresh label of the same name would either
// collide with labels' UNIQUE (user_id, name) outright, or — worse, where the
// case differs — leave the user with two labels meaning the same thing: the
// one they already use, and a new one the area silently created underneath
// them. So an existing label is reused whenever its name matches
// case-insensitively; only a genuinely new name mints one.
//
// IDEMPOTENT UNDER CONCURRENCY WITH NO SAVEPOINT GAMES, the same posture
// materialiseCategoryTabs takes: every INSERT here is guarded by ON CONFLICT
// DO NOTHING against a real unique index, so a second caller racing the first
// either finds the row already there (label reuse) or has its own INSERT
// silently do nothing (area/feed rows) — never an aborted transaction to
// retry around.
func (tx *UserTx) materialiseDefaultAreas(ctx context.Context) error {
	var n int
	if err := tx.tx.QueryRow(ctx,
		`SELECT count(*) FROM app.areas WHERE user_id = $1`,
		tx.userID.String()).Scan(&n); err != nil {
		return fmt.Errorf("store: materialise areas (count): %w", err)
	}
	if n > 0 {
		return nil
	}

	for i, a := range areas.Default().Areas {
		labelID, err := tx.labelForAreaName(ctx, a.Name, a.Tabs[0])
		if err != nil {
			return err
		}

		// ON CONFLICT (user_id, label_id) is the whole concurrency story: two
		// callers materialising the SAME user resolve the SAME label id
		// (labelForAreaName is itself insert-or-get), so they collide HERE
		// rather than on the area name, and the loser's INSERT inserts
		// nothing — no rollback, nothing to retry.
		var areaIDStr string
		err = tx.tx.QueryRow(ctx, `
			INSERT INTO app.areas (id, user_id, name, position, label_id)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_id, label_id) DO NOTHING
			RETURNING id`,
			NewULID().String(), tx.userID.String(), a.Name, i, labelID.String()).Scan(&areaIDStr)
		if err != nil {
			if isNoRows(err) {
				continue // another caller already materialised this one
			}
			return fmt.Errorf("store: materialise area %q: %w", a.Name, err)
		}
		areaID, perr := ulid.Parse(areaIDStr)
		if perr != nil {
			return fmt.Errorf("store: materialise area %q: parse id: %w", a.Name, perr)
		}

		// Only the winner of the area INSERT adds feeds: a loser inserted
		// nothing above, so its feeds would either duplicate the winner's or
		// (for the tab) outright conflict with them.
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO app.area_feeds (id, user_id, area_id, tab)
			VALUES ($1, $2, $3, $4::app.category_tab)`,
			NewULID().String(), tx.userID.String(), areaID.String(), string(a.Tabs[0])); err != nil {
			return fmt.Errorf("store: materialise area %q (tab feed): %w", a.Name, err)
		}
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO app.area_feeds (id, user_id, area_id, label_id)
			VALUES ($1, $2, $3, $4)`,
			NewULID().String(), tx.userID.String(), areaID.String(), labelID.String()); err != nil {
			return fmt.Errorf("store: materialise area %q (label feed): %w", a.Name, err)
		}
		if _, err := tx.AppendChange(ctx, ObjectArea, areaID, OpCreated); err != nil {
			return err
		}
	}
	return nil
}

// labelForAreaName resolves the label a default area should own: an existing
// label of the caller's whose name matches case-insensitively, or a freshly
// minted one (MirrorToIMAP false, mapped to t so the label participates in
// the tab layer the same way its area does).
func (tx *UserTx) labelForAreaName(ctx context.Context, name string, t tabs.Tab) (ulid.ULID, error) {
	var existing string
	err := tx.tx.QueryRow(ctx,
		`SELECT id FROM app.labels WHERE user_id = $1 AND lower(name) = lower($2)`,
		tx.userID.String(), name).Scan(&existing)
	if err == nil {
		return ulid.Parse(existing)
	}
	if !isNoRows(err) {
		return ulid.ULID{}, fmt.Errorf("store: find label for area %q: %w", name, err)
	}

	id, err := tx.CreateLabel(ctx, LabelRow{Name: name, MirrorToIMAP: false, Tab: t})
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, ErrLabelNameTaken) {
		return ulid.ULID{}, fmt.Errorf("store: create label for area %q: %w", name, err)
	}
	// Raced with a concurrent materialisation (or an exact-case label created
	// between the case-insensitive lookup above and this INSERT): the name now
	// exists, so read it back instead of failing the whole materialisation.
	var raced string
	if rerr := tx.tx.QueryRow(ctx,
		`SELECT id FROM app.labels WHERE user_id = $1 AND name = $2`,
		tx.userID.String(), name).Scan(&raced); rerr != nil {
		return ulid.ULID{}, fmt.Errorf("store: find label for area %q (post-race): %w", name, rerr)
	}
	return ulid.Parse(raced)
}
