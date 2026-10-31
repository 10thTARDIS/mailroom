// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// THE DESK'S TWO SETTINGS SURFACES (#442 increment 1, migration 00046): the
// per-user desk singleton (the Surface/Drawer score threshold, drawer and tray
// dwell periods) and per-client-family display preferences (theme, density,
// pane split). Both materialise a default row lazily on first read — the same
// posture ListCategoryTabs takes and for the same reason: there is no
// Go-side user-provisioning step to hang a seed off, so the row and its
// change_log row spring into being together the first time anything asks
// (category_tabs.go's doc comment explains why in full).

// DeskPreferences is the desk's per-user settings singleton.
type DeskPreferences struct {
	LineThreshold     float32
	DrawerDefaultDays int
	TrayNagDays       int
	// DrawerSweepEnabled is the drawer sweep's login-level MASTER SWITCH
	// (migration 00048, #443), default false. internal/drawer's Sweep reads
	// this FIRST and archives nothing while it is off, regardless of any
	// area's own auto_archive — a per-area switch was not enough, because the
	// default plan materialises five areas, all on (CLAUDE.md "build the
	// report before the fix").
	DrawerSweepEnabled bool
	// SetupCompletedAt is when this login finished DAY-ONE SETUP (#446,
	// migration 00050). NIL MEANS "NEVER" — the desk opens to the setup wizard
	// rather than to the desk. Migration 00050 backfilled every row that
	// existed before it to that row's updated_at, so no established desk is
	// interrupted by a wizard; only a login materialising its preferences AFTER
	// the migration starts out nil.
	//
	// It is not a boolean because "when" is the useful fact: a support question
	// about a mailbox that came out wrong is answered by the date the import
	// ran, and Setup/get's runs are ordered against it.
	SetupCompletedAt *time.Time
	// LearnedPlacementEnabled is LEARNED PLACEMENT's off switch (#714,
	// migration 00060), default true: it was decided that it acts
	// immediately. The store enforces it at ingest
	// (LearnedPlacementForNewMessage reads the column itself), so turning it
	// off is not a client-side nicety a second client could bypass.
	LearnedPlacementEnabled bool
	// QuietHours is the user's quiet-hours setting (#59, migration 00061),
	// off by default. It lives on this singleton for learned placement's
	// reason: it is per USER and server-enforced (the push sender reads it), and
	// this is the one per-user settings record the server has.
	QuietHours QuietHours
	// FollowUp is the follow-up nudge settings (#58, migration 00064).
	FollowUp  FollowUpSettings
	UpdatedAt time.Time
}

// FollowUpSettings mirrors the desk_preferences nudge_* columns: "no one
// answered your mail" (on by default), "you haven't replied to this" (off by
// default — opt-in, because its measured precision is low) and the inbound
// delay in days (1-14, default 3). internal/nudge owns what they mean.
type FollowUpSettings struct {
	Outbound    bool
	Inbound     bool
	InboundDays int
}

// QuietHours mirrors the desk_preferences quiet_hours_* columns. Start and End
// are minutes after local midnight in Timezone (an IANA name); Days is a
// bitmask of the weekday a quiet period starts on, bit 0 = Sunday.
// internal/notify owns what they mean.
type QuietHours struct {
	Enabled  bool
	Start    int
	End      int
	Days     int
	Timezone string
	AllowVIP bool
}

// DeskPreferencesPatch is a partial update. A nil pointer means "leave
// unchanged" — every SCALAR field here has a valid zero-adjacent default, so
// there is no "clear it" case to distinguish for them.
//
// SetupCompletedAt is the exception and needs the THREE-STATE shape
// AreaPatch.AutoArchiveDays already uses: nil is itself a meaningful value
// ("this login has not been through day-one"), so "leave it" and "set it to
// null" cannot both be expressed by a nil pointer. SetSetupCompletedAt is the
// guard — false leaves the column alone, true writes SetupCompletedAt
// including nil.
type DeskPreferencesPatch struct {
	LineThreshold       *float32
	DrawerDefaultDays   *int
	TrayNagDays         *int
	DrawerSweepEnabled  *bool
	SetSetupCompletedAt bool
	SetupCompletedAt    *time.Time
	// LearnedPlacementEnabled switches learned placement on or off (#714).
	LearnedPlacementEnabled *bool
	// QuietHours replaces the whole quiet-hours setting when non-nil (#59).
	// One object rather than six pointers: the fields only make sense
	// together (an end without its start, a time without its zone), and the
	// client edits them as one form.
	QuietHours *QuietHours
	// FollowUp replaces the follow-up nudge settings when non-nil (#58).
	FollowUp *FollowUpSettings
}

// ErrDeskPreferencesInvalid wraps a CHECK-constraint failure on desk
// preferences (line_threshold outside [0,1], a dwell period <= 0) as a typed
// validation error rather than a bare Postgres message.
var ErrDeskPreferencesInvalid = errors.New("store: invalid desk preferences")

const deskPreferencesColumns = `line_threshold, drawer_default_days, tray_nag_days, drawer_sweep_enabled, setup_completed_at, learned_placement_enabled, ` +
	`quiet_hours_enabled, quiet_hours_start, quiet_hours_end, quiet_hours_days, quiet_hours_timezone, quiet_hours_allow_vip, ` +
	`nudge_outbound_enabled, nudge_inbound_enabled, nudge_inbound_days, updated_at`

// deskPreferencesDest is the Scan destination list matching
// deskPreferencesColumns, in one place so the two reads cannot drift.
func deskPreferencesDest(p *DeskPreferences) []any {
	return []any{&p.LineThreshold, &p.DrawerDefaultDays, &p.TrayNagDays, &p.DrawerSweepEnabled, &p.SetupCompletedAt,
		&p.LearnedPlacementEnabled,
		&p.QuietHours.Enabled, &p.QuietHours.Start, &p.QuietHours.End, &p.QuietHours.Days,
		&p.QuietHours.Timezone, &p.QuietHours.AllowVIP,
		&p.FollowUp.Outbound, &p.FollowUp.Inbound, &p.FollowUp.InboundDays,
		&p.UpdatedAt}
}

// GetDeskPreferences returns the caller's desk preferences, materialising the
// default row (and appending its DeskPreferences created change row, with
// object_id = the user's own id — there is exactly one desk per user in 0.5)
// on first call.
func (tx *UserTx) GetDeskPreferences(ctx context.Context) (DeskPreferences, error) {
	p, ok, err := tx.readDeskPreferences(ctx)
	if err != nil {
		return DeskPreferences{}, err
	}
	if ok {
		return p, nil
	}

	err = tx.tx.QueryRow(ctx, `
		INSERT INTO app.desk_preferences (user_id)
		VALUES ($1)
		ON CONFLICT (user_id) DO NOTHING
		RETURNING `+deskPreferencesColumns,
		tx.userID.String()).Scan(deskPreferencesDest(&p)...)
	if err == nil {
		if _, aerr := tx.AppendChange(ctx, ObjectDeskPreferences, tx.userID, OpCreated); aerr != nil {
			return DeskPreferences{}, aerr
		}
		return p, nil
	}
	if !isNoRows(err) {
		return DeskPreferences{}, fmt.Errorf("store: materialise desk preferences: %w", err)
	}
	// Lost the race: another caller materialised it first.
	p, ok, err = tx.readDeskPreferences(ctx)
	if err != nil {
		return DeskPreferences{}, err
	}
	if !ok {
		return DeskPreferences{}, fmt.Errorf("store: desk preferences: materialised row vanished")
	}
	return p, nil
}

func (tx *UserTx) readDeskPreferences(ctx context.Context) (DeskPreferences, bool, error) {
	var p DeskPreferences
	err := tx.tx.QueryRow(ctx,
		`SELECT `+deskPreferencesColumns+` FROM app.desk_preferences WHERE user_id = $1`,
		tx.userID.String()).Scan(deskPreferencesDest(&p)...)
	if err == nil {
		return p, true, nil
	}
	if isNoRows(err) {
		return DeskPreferences{}, false, nil
	}
	return DeskPreferences{}, false, fmt.Errorf("store: get desk preferences: %w", err)
}

// UpdateDeskPreferences applies a partial update, materialising the default
// row first if needed, and appends exactly one DeskPreferences updated change
// row when something actually changed (a no-op patch appends none). A
// CHECK-violating value surfaces as ErrDeskPreferencesInvalid; the surrounding
// transaction survives either way, via the SAVEPOINT below.
func (tx *UserTx) UpdateDeskPreferences(ctx context.Context, patch DeskPreferencesPatch) error {
	current, err := tx.GetDeskPreferences(ctx)
	if err != nil {
		return err
	}
	nThreshold, nDrawer, nNag := current.LineThreshold, current.DrawerDefaultDays, current.TrayNagDays
	nSweep := current.DrawerSweepEnabled
	nLearned := current.LearnedPlacementEnabled
	if patch.LearnedPlacementEnabled != nil {
		nLearned = *patch.LearnedPlacementEnabled
	}
	nQuiet := current.QuietHours
	if patch.QuietHours != nil {
		nQuiet = *patch.QuietHours
	}
	nFollow := current.FollowUp
	if patch.FollowUp != nil {
		nFollow = *patch.FollowUp
	}
	nSetupDone := current.SetupCompletedAt
	if patch.SetSetupCompletedAt {
		nSetupDone = patch.SetupCompletedAt
	}
	if patch.LineThreshold != nil {
		nThreshold = *patch.LineThreshold
	}
	if patch.DrawerDefaultDays != nil {
		nDrawer = *patch.DrawerDefaultDays
	}
	if patch.TrayNagDays != nil {
		nNag = *patch.TrayNagDays
	}
	if patch.DrawerSweepEnabled != nil {
		nSweep = *patch.DrawerSweepEnabled
	}

	// Under a SAVEPOINT: a CHECK violation aborts a bare statement's
	// surrounding transaction exactly as a unique violation does (SQLSTATE
	// 25P02), and #442's own test list requires the tx to survive an
	// out-of-range value.
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return fmt.Errorf("store: update desk preferences (savepoint): %w", err)
	}
	var touched string
	// setup_completed_at joins the IS DISTINCT FROM row comparison rather than
	// being written unconditionally, so a patch that names it with the value it
	// already has stays a true no-op and appends no change row — the same
	// property every other field here has.
	var setupArg any
	if nSetupDone != nil {
		setupArg = nSetupDone.UTC()
	}
	err = sp.QueryRow(ctx, `
		UPDATE app.desk_preferences
		   SET line_threshold = $2, drawer_default_days = $3, tray_nag_days = $4,
		       drawer_sweep_enabled = $5, setup_completed_at = $6,
		       learned_placement_enabled = $7,
		       quiet_hours_enabled = $8, quiet_hours_start = $9, quiet_hours_end = $10,
		       quiet_hours_days = $11, quiet_hours_timezone = $12, quiet_hours_allow_vip = $13,
		       nudge_outbound_enabled = $14, nudge_inbound_enabled = $15, nudge_inbound_days = $16,
		       updated_at = now()
		 WHERE user_id = $1
		   AND (line_threshold, drawer_default_days, tray_nag_days, drawer_sweep_enabled, setup_completed_at,
		        learned_placement_enabled,
		        quiet_hours_enabled, quiet_hours_start, quiet_hours_end, quiet_hours_days,
		        quiet_hours_timezone, quiet_hours_allow_vip,
		        nudge_outbound_enabled, nudge_inbound_enabled, nudge_inbound_days)
		       IS DISTINCT FROM ($2, $3, $4, $5, $6::timestamptz, $7,
		        $8::boolean, $9::smallint, $10::smallint, $11::smallint, $12::text, $13::boolean,
		        $14::boolean, $15::boolean, $16::smallint)
		RETURNING user_id`,
		tx.userID.String(), nThreshold, nDrawer, nNag, nSweep, setupArg, nLearned,
		nQuiet.Enabled, nQuiet.Start, nQuiet.End, nQuiet.Days, nQuiet.Timezone, nQuiet.AllowVIP,
		nFollow.Outbound, nFollow.Inbound, nFollow.InboundDays).Scan(&touched)
	if err != nil {
		if isNoRows(err) {
			// A true no-op: no SQL error occurred, so RELEASE rather than ROLLBACK.
			if cerr := sp.Commit(ctx); cerr != nil {
				return fmt.Errorf("store: update desk preferences (release): %w", cerr)
			}
			return nil
		}
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if isCheckViolation(err) {
			return fmt.Errorf("%w: %v", ErrDeskPreferencesInvalid, err)
		}
		return fmt.Errorf("store: update desk preferences (write): %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return fmt.Errorf("store: update desk preferences (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectDeskPreferences, tx.userID, OpUpdated); err != nil {
		return err
	}
	return nil
}

// ClientFamily is which kind of client asked (spec §442): "web" | "phone" |
// "desktop". A closed vocabulary, aliased to app.client_family.
type ClientFamily string

const (
	ClientWeb     ClientFamily = "web"
	ClientPhone   ClientFamily = "phone"
	ClientDesktop ClientFamily = "desktop"
)

// Valid reports whether f is one of the three known families.
func (f ClientFamily) Valid() bool {
	switch f {
	case ClientWeb, ClientPhone, ClientDesktop:
		return true
	default:
		return false
	}
}

// ErrClientFamilyInvalid is returned for a family outside the closed set,
// before any statement runs.
var ErrClientFamilyInvalid = errors.New("store: not a client family")

// ErrClientPreferencesInvalid wraps a CHECK-constraint failure on client
// preferences (an unknown theme/density/message_opens/text_size/row_layout
// token, an out-of-range pane_split_pct) as a typed validation error.
var ErrClientPreferencesInvalid = errors.New("store: invalid client preferences")

// ClientPreferences is one client family's per-user display preferences.
type ClientPreferences struct {
	ID           ulid.ULID
	Family       ClientFamily
	Theme        string
	Density      string
	MessageOpens string
	PaneSplitPct int
	// TextSize is the whole app's text scale — 'xs' | 's' | 'm' | 'l' | 'xl',
	// with 'm' the size the client has always rendered at (migration 00052,
	// #514). Five steps rather than a number, because it is a closed
	// vocabulary a settings control renders as five words and because the
	// client maps each step to a root font size of its own choosing.
	TextSize string
	// RowLayout is how a row of mail is drawn — 'oneLine' | 'multiLine'
	// (migration 00052, #514). The server stores the CHOICE and knows nothing
	// about the geometry; what a line contains is the client's business.
	RowLayout string
	UpdatedAt time.Time
}

// ClientPreferencesPatch is a partial update. A nil pointer means "leave
// unchanged".
type ClientPreferencesPatch struct {
	Theme        *string
	Density      *string
	MessageOpens *string
	PaneSplitPct *int
	TextSize     *string
	RowLayout    *string
}

const clientPreferencesColumns = `theme, density, message_opens, pane_split_pct, text_size, row_layout, updated_at`

// GetClientPreferences returns the caller's preferences for family,
// materialising the default row (and appending its ClientPreferences created
// change row) on first call. Unlike DeskPreferences this is NOT a singleton —
// a phone and a browser get their own row and their own change rows.
func (tx *UserTx) GetClientPreferences(ctx context.Context, family ClientFamily) (ClientPreferences, error) {
	if !family.Valid() {
		return ClientPreferences{}, fmt.Errorf("%w: %q", ErrClientFamilyInvalid, family)
	}
	p, ok, err := tx.readClientPreferences(ctx, family)
	if err != nil {
		return ClientPreferences{}, err
	}
	if ok {
		return p, nil
	}

	newID := NewULID()
	err = tx.tx.QueryRow(ctx, `
		INSERT INTO app.client_preferences (id, user_id, family)
		VALUES ($1, $2, $3::app.client_family)
		ON CONFLICT (user_id, family) DO NOTHING
		RETURNING `+clientPreferencesColumns,
		newID.String(), tx.userID.String(), string(family)).
		Scan(&p.Theme, &p.Density, &p.MessageOpens, &p.PaneSplitPct, &p.TextSize, &p.RowLayout, &p.UpdatedAt)
	if err == nil {
		p.ID, p.Family = newID, family
		if _, aerr := tx.AppendChange(ctx, ObjectClientPreferences, newID, OpCreated); aerr != nil {
			return ClientPreferences{}, aerr
		}
		return p, nil
	}
	if !isNoRows(err) {
		return ClientPreferences{}, fmt.Errorf("store: materialise client preferences: %w", err)
	}
	// Lost the race: another caller materialised it first.
	p, ok, err = tx.readClientPreferences(ctx, family)
	if err != nil {
		return ClientPreferences{}, err
	}
	if !ok {
		return ClientPreferences{}, fmt.Errorf("store: client preferences: materialised row vanished")
	}
	return p, nil
}

func (tx *UserTx) readClientPreferences(ctx context.Context, family ClientFamily) (ClientPreferences, bool, error) {
	var (
		p  ClientPreferences
		id string
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT id, `+clientPreferencesColumns+`
		  FROM app.client_preferences WHERE user_id = $1 AND family = $2::app.client_family`,
		tx.userID.String(), string(family)).
		Scan(&id, &p.Theme, &p.Density, &p.MessageOpens, &p.PaneSplitPct, &p.TextSize, &p.RowLayout, &p.UpdatedAt)
	if isNoRows(err) {
		return ClientPreferences{}, false, nil
	}
	if err != nil {
		return ClientPreferences{}, false, fmt.Errorf("store: get client preferences: %w", err)
	}
	parsed, perr := ulid.Parse(id)
	if perr != nil {
		return ClientPreferences{}, false, fmt.Errorf("store: parse client preferences id %q: %w", id, perr)
	}
	p.ID, p.Family = parsed, family
	return p, true, nil
}

// UpdateClientPreferences applies a partial update for family, materialising
// the default row first if needed, and appends exactly one ClientPreferences
// updated change row when something actually changed. A CHECK-violating value
// surfaces as ErrClientPreferencesInvalid; the transaction survives either
// way, via the SAVEPOINT below.
func (tx *UserTx) UpdateClientPreferences(ctx context.Context, family ClientFamily, patch ClientPreferencesPatch) error {
	if !family.Valid() {
		return fmt.Errorf("%w: %q", ErrClientFamilyInvalid, family)
	}
	current, err := tx.GetClientPreferences(ctx, family)
	if err != nil {
		return err
	}
	nTheme, nDensity, nOpens, nPct := current.Theme, current.Density, current.MessageOpens, current.PaneSplitPct
	nText, nLayout := current.TextSize, current.RowLayout
	if patch.Theme != nil {
		nTheme = *patch.Theme
	}
	if patch.Density != nil {
		nDensity = *patch.Density
	}
	if patch.MessageOpens != nil {
		nOpens = *patch.MessageOpens
	}
	if patch.PaneSplitPct != nil {
		nPct = *patch.PaneSplitPct
	}
	if patch.TextSize != nil {
		nText = *patch.TextSize
	}
	if patch.RowLayout != nil {
		nLayout = *patch.RowLayout
	}

	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return fmt.Errorf("store: update client preferences (savepoint): %w", err)
	}
	var touched string
	// text_size and row_layout join the IS DISTINCT FROM row comparison rather
	// than being written unconditionally, so a patch naming a value the row
	// already holds stays a true no-op and appends no change row — the same
	// property every other field here has.
	err = sp.QueryRow(ctx, `
		UPDATE app.client_preferences
		   SET theme = $3, density = $4, message_opens = $5, pane_split_pct = $6,
		       text_size = $7, row_layout = $8, updated_at = now()
		 WHERE user_id = $1 AND family = $2::app.client_family
		   AND (theme, density, message_opens, pane_split_pct, text_size, row_layout)
		       IS DISTINCT FROM ($3, $4, $5, $6, $7, $8)
		RETURNING id`,
		tx.userID.String(), string(family), nTheme, nDensity, nOpens, nPct, nText, nLayout).Scan(&touched)
	if err != nil {
		if isNoRows(err) {
			if cerr := sp.Commit(ctx); cerr != nil {
				return fmt.Errorf("store: update client preferences (release): %w", cerr)
			}
			return nil
		}
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if isCheckViolation(err) {
			return fmt.Errorf("%w: %v", ErrClientPreferencesInvalid, err)
		}
		return fmt.Errorf("store: update client preferences (write): %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return fmt.Errorf("store: update client preferences (release): %w", err)
	}
	id, perr := ulid.Parse(touched)
	if perr != nil {
		return fmt.Errorf("store: parse client preferences id %q: %w", touched, perr)
	}
	if _, err := tx.AppendChange(ctx, ObjectClientPreferences, id, OpUpdated); err != nil {
		return err
	}
	return nil
}
