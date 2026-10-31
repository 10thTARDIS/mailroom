// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/tabs"
)

// This file holds the canonical Label layer (spec §6): the hybrid label model
// whose labels live in Postgres and mirror to IMAP keywords by default. A Label
// IS a JMAP-visible object (changelog.go ObjectLabel), so every create/rename/
// destroy appends exactly one Label change_log row in the SAME UserTx as the
// mutation (the load-bearing invariant — CLAUDE.md).
//
// The label↔keyword mapping (app.label_keyword_map) is DETERMINISTIC on first
// assignment and then PERSISTED and STABLE: it is never re-derived from the
// label name, so a later rename changes only the display name and can neither
// change nor orphan the keyword atom (spec §6). The reverse lookup
// (LabelIDByKeywordAtom) and the mirror read (labelKeywordAtom) both live in
// jmap_email.go and read this same persisted mapping.

// keywordAtomPrefix is the fixed prefix every mirrored label keyword carries so
// third-party IMAP clients recognize a Mailroom label atom (spec §6).
const keywordAtomPrefix = "MailroomLabel-"

// maxAtomCollisionAttempts bounds the collision-suffix search when assigning a
// keyword atom. A user would need thousands of same-slug labels to approach it;
// exhausting it is treated as an error rather than looping forever.
const maxAtomCollisionAttempts = 10000

// ErrLabelNotFound is returned when a label id does not resolve for the caller
// (RLS makes another user's id indistinguishable from a missing one).
var ErrLabelNotFound = errors.New("store: label not found")

// ErrLabelNameTaken is returned when a create/rename would collide with the
// caller's UNIQUE (user_id, name) constraint on app.labels.
var ErrLabelNameTaken = errors.New("store: label name already exists")

// LabelView is the canonical Label projection for the JMAP Label extension.
// Color is "" when unset; ParentID is nil at top level (nesting is local-only
// metadata, spec §6). KeywordAtom is the persisted IMAP mirror atom, or "" for
// a label with no mapping yet (a local-only label, mirror_to_imap = false).
type LabelView struct {
	ID           ulid.ULID
	Name         string
	Color        string
	ParentID     *ulid.ULID
	MirrorToIMAP bool
	KeywordAtom  string
	// Tab is the category tab this label's mail belongs to (#219), or "" when the
	// label is not mapped — which falls through to the tab classifier exactly as
	// an unlabelled message does. Local-only metadata like Color and ParentID:
	// there is no IMAP concept to mirror it onto.
	Tab tabs.Tab
}

// LabelRow is the input to CreateLabel. MirrorToIMAP defaults to true at the
// spec level (spec §6); the caller passes the resolved value. Tab is "" for an
// unmapped label.
type LabelRow struct {
	Name         string
	Color        string
	ParentID     *ulid.ULID
	MirrorToIMAP bool
	Tab          tabs.Tab
}

// LabelPatch is a partial update for UpdateLabel. A nil pointer / false Set
// flag means "leave unchanged"; SetColor with an empty Color clears the color,
// SetParent with a nil ParentID clears the parent (detach nesting), and SetTab
// with an empty Tab unmaps the label from every tab.
type LabelPatch struct {
	Name         *string
	SetColor     bool
	Color        string
	SetParent    bool
	ParentID     *ulid.ULID
	MirrorToIMAP *bool
	SetTab       bool
	Tab          tabs.Tab
}

// ErrLabelTabInvalid is returned for a tab outside the closed vocabulary. It is
// refused rather than coerced: a mapping the operator set and the server quietly
// changed is worse than one the server rejected.
var ErrLabelTabInvalid = errors.New("store: not a category tab")

// validateLabelTab accepts "" (unmapped) or one of the five.
func validateLabelTab(t tabs.Tab) error {
	if t == "" || t.Valid() {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrLabelTabInvalid, string(t))
}

// CreateLabel inserts a canonical label, appends the Label change_log row (the
// invariant), and — when the label mirrors to IMAP — assigns its stable keyword
// atom up front so the Email/set mirror path (labelKeywordAtom) has a mapping to
// enqueue. Returns ErrLabelNameTaken on a name collision.
func (tx *UserTx) CreateLabel(ctx context.Context, row LabelRow) (ulid.ULID, error) {
	if err := validateLabelTab(row.Tab); err != nil {
		return ulid.ULID{}, err
	}
	id := NewULID()
	var parent any
	if row.ParentID != nil {
		parent = row.ParentID.String()
	}
	// The INSERT runs under a SAVEPOINT: a UNIQUE (user_id, name) violation is a
	// RECOVERABLE, caller-visible outcome (ErrLabelNameTaken), and without the
	// savepoint the failed statement would abort the whole surrounding UserTx
	// (SQLSTATE 25P02) — so a Label/set that reports one duplicate name in
	// notCreated and continues would lose every other create in the same call.
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create label (savepoint): %w", err)
	}
	if _, err := sp.Exec(ctx, `
		INSERT INTO app.labels (id, user_id, name, color, parent_id, mirror_to_imap, tab)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5::app.ulid, $6,
		        NULLIF($7, '')::app.category_tab)`,
		id.String(), tx.userID.String(), row.Name, row.Color, parent, row.MirrorToIMAP,
		string(row.Tab)); err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if isUniqueViolation(err) {
			return ulid.ULID{}, ErrLabelNameTaken
		}
		return ulid.ULID{}, fmt.Errorf("store: create label: %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return ulid.ULID{}, fmt.Errorf("store: create label (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectLabel, id, OpCreated); err != nil {
		return ulid.ULID{}, err
	}
	if row.MirrorToIMAP {
		if _, err := tx.KeywordAtomForLabel(ctx, id); err != nil {
			return ulid.ULID{}, err
		}
	}
	return id, nil
}

// RenameLabel changes only a label's display name — never its keyword atom,
// which is stable once assigned (spec §6). It is a thin wrapper over UpdateLabel.
func (tx *UserTx) RenameLabel(ctx context.Context, id ulid.ULID, name string) (found bool, err error) {
	return tx.UpdateLabel(ctx, id, LabelPatch{Name: &name})
}

// UpdateLabel applies a partial update to a label and appends exactly one Label
// change_log row when something actually changes (a no-op patch is a true no-op,
// like ApplyEmailUpdate). Renaming NEVER touches label_keyword_map; flipping
// mirror_to_imap on assigns a keyword atom if the label lacks one, while
// flipping it off leaves any existing atom in place (never orphan it — spec §6).
// found is false when the label does not exist for the caller.
func (tx *UserTx) UpdateLabel(ctx context.Context, id ulid.ULID, patch LabelPatch) (found bool, err error) {
	if patch.SetTab {
		if err := validateLabelTab(patch.Tab); err != nil {
			return false, err
		}
	}
	// Lock and read current state so unspecified fields are preserved and the
	// guarded UPDATE can detect a genuine no-op.
	var (
		name   string
		color  string
		parent *string
		mirror bool
		tab    string
	)
	err = tx.tx.QueryRow(ctx, `
		SELECT name, COALESCE(color, ''), parent_id, mirror_to_imap,
		       COALESCE(tab::text, '')
		  FROM app.labels
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`,
		tx.userID.String(), id.String()).Scan(&name, &color, &parent, &mirror, &tab)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: update label (read): %w", err)
	}

	nName, nColor, nMirror, nTab := name, color, mirror, tab
	nParent := parent
	if patch.Name != nil {
		nName = *patch.Name
	}
	if patch.SetColor {
		nColor = patch.Color
	}
	if patch.SetTab {
		nTab = string(patch.Tab)
	}
	if patch.SetParent {
		if patch.ParentID != nil {
			s := patch.ParentID.String()
			nParent = &s
		} else {
			nParent = nil
		}
	}
	if patch.MirrorToIMAP != nil {
		nMirror = *patch.MirrorToIMAP
	}

	var nParentArg any
	if nParent != nil {
		nParentArg = *nParent
	}
	// As in CreateLabel, the guarded UPDATE runs under a SAVEPOINT so a rename
	// that collides on UNIQUE (user_id, name) surfaces as ErrLabelNameTaken
	// without aborting the surrounding UserTx (SQLSTATE 25P02).
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return false, fmt.Errorf("store: update label (savepoint): %w", err)
	}
	var touched string
	err = sp.QueryRow(ctx, `
		UPDATE app.labels
		   SET name = $3, color = NULLIF($4, ''), parent_id = $5::app.ulid,
		       mirror_to_imap = $6, tab = NULLIF($7, '')::app.category_tab
		 WHERE user_id = $1 AND id = $2
		   AND (name, COALESCE(color, ''), parent_id, mirror_to_imap,
		        COALESCE(tab::text, ''))
		       IS DISTINCT FROM ($3, $4, $5::app.ulid, $6, $7)
		RETURNING id`,
		tx.userID.String(), id.String(), nName, nColor, nParentArg, nMirror, nTab).Scan(&touched)
	if err != nil {
		if isNoRows(err) {
			// The row exists (we locked it) but nothing changed: a true no-op, so
			// no change_log row and no atom work. No SQL error occurred, so the
			// savepoint is released rather than rolled back.
			if cerr := sp.Commit(ctx); cerr != nil {
				return false, fmt.Errorf("store: update label (release): %w", cerr)
			}
			return true, nil
		}
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if isUniqueViolation(err) {
			return true, ErrLabelNameTaken
		}
		return false, fmt.Errorf("store: update label (write): %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return false, fmt.Errorf("store: update label (release): %w", err)
	}

	// A label that now mirrors but has no atom gets one assigned (stable from
	// here on). Turning mirroring off never removes an existing atom.
	if nMirror {
		if _, err := tx.KeywordAtomForLabel(ctx, id); err != nil {
			return false, err
		}
	}

	if _, err := tx.AppendChange(ctx, ObjectLabel, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteLabel destroys a label: it detaches the label from every message
// (app.message_labels), removes the label row (which cascades the keyword
// mapping via FK), and appends the single Label change_log row. found is false
// when the label does not exist for the caller.
//
// Note (documented limitation): detaching the label alters the JMAP keyword set
// of each affected Email, but 0.5 does NOT fan out per-Email change_log rows on
// label delete (a label may sit on 10^6 messages). Only the Label destroyed row
// is emitted; per-Email keyword re-sync on label delete is a later refinement.
// CountLabelUsage reports how many messages carry a label. RLS-scoped, so it
// counts only the caller's own mail.
//
// It exists for the one question worth asking before a delete: DeleteLabel
// detaches the label from every message first and there is no undo — the
// change_log records the Label as destroyed, not what it was attached to. That
// count is the difference between housekeeping and data loss, and an operator
// deserves to see it before deciding, not after.
func (tx *UserTx) CountLabelUsage(ctx context.Context, id ulid.ULID) (int, error) {
	var n int
	if err := tx.tx.QueryRow(ctx,
		`SELECT count(*) FROM app.message_labels WHERE user_id = $1 AND label_id = $2`,
		tx.userID.String(), id.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count label usage: %w", err)
	}
	return n, nil
}

// DeleteLabel removes a label and detaches it from every message that carried
// it, appending an Email change row per detached message and enqueuing the
// keyword-removal writeback for each.
//
// ⚠ IT DID NEITHER UNTIL #351 B4, AND THAT BROKE THE LOAD-BEARING INVARIANT.
// The old implementation issued a bare `DELETE FROM app.message_labels` and
// appended exactly one row — Label/destroyed. But a label's keyword atom IS a
// message_labels row (jmap_email.go's keyword derivation), so that DELETE
// mutated JMAP-visible Email state for every attached message with no Email
// change row to show for it.
//
// What that cost a client: `Email/changes` reported nothing, so a conforming
// third-party JMAP client kept rendering the keyword on every cached message
// indefinitely. `Label` is a Mailroom extension type, so such a client got no
// signal at all. And no writeback intent was enqueued, so the mirrored
// `MailroomLabel-<slug>` atoms stayed on the IMAP server forever — the orphaned
// keywords spec §6 guards against for RENAMES, arriving by the other door.
//
// The detach now routes through ApplyEmailUpdate, which is the one write path
// that already gets all of this right, rather than a second implementation that
// would have to keep getting it right.
//
// RemovalIsUndo is set deliberately. Deleting a label is not the user
// disagreeing with a rule about N messages — it is the user disposing of the
// label — and routing it through the correction path would manufacture one
// correction per message and make a perfectly good rule look catastrophic.
// That is the case RemovalIsUndo's own doc describes.
//
// ⚠ COST: this is O(messages carrying the label), each its own change row. That
// is the honest price of the invariant, and CountLabelUsage exists so the count
// is visible BEFORE the delete rather than discovered after it.
func (tx *UserTx) DeleteLabel(ctx context.Context, id ulid.ULID) (found bool, err error) {
	// The attached set, read before anything mutates it.
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id FROM app.message_labels
		 WHERE user_id = $1 AND label_id = $2
		 ORDER BY message_id`,
		tx.userID.String(), id.String())
	if err != nil {
		return false, fmt.Errorf("store: delete label (attached set): %w", err)
	}
	var attached []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return false, fmt.Errorf("store: delete label (scan attached): %w", err)
		}
		mid, perr := ulid.Parse(s)
		if perr != nil {
			rows.Close()
			return false, fmt.Errorf("store: delete label: message id %q: %w", s, perr)
		}
		attached = append(attached, mid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: delete label (attached set): %w", err)
	}

	for _, mid := range attached {
		if _, _, err := tx.ApplyEmailUpdate(ctx, mid, EmailUpdate{
			RemoveLabelIDs: []ulid.ULID{id},
			RemovalIsUndo:  true,
		}); err != nil {
			return false, fmt.Errorf("store: delete label (detach %s): %w", mid, err)
		}
	}

	var deleted string
	err = tx.tx.QueryRow(ctx, `
		DELETE FROM app.labels WHERE user_id = $1 AND id = $2 RETURNING id`,
		tx.userID.String(), id.String()).Scan(&deleted)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: delete label: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectLabel, id, OpDestroyed); err != nil {
		return false, err
	}
	return true, nil
}

// ListLabels returns every label for the caller, ordered by name then id — the
// stable total order Label/query needs. Each view carries its persisted keyword
// atom (or "" when unmapped).
func (tx *UserTx) ListLabels(ctx context.Context) ([]LabelView, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT l.id, l.name, COALESCE(l.color, ''), l.parent_id, l.mirror_to_imap,
		       COALESCE(k.keyword_atom, ''), COALESCE(l.tab::text, '')
		  FROM app.labels l
		  LEFT JOIN app.label_keyword_map k
		    ON k.user_id = l.user_id AND k.label_id = l.id
		 WHERE l.user_id = $1
		 ORDER BY l.name, l.id`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list labels: %w", err)
	}
	defer rows.Close()
	return scanLabels(rows)
}

// GetLabels returns the label projections for the requested ids (RLS-scoped).
// Ids not returned were not found or belong to another user — indistinguishable
// by design. Result order is unspecified; the caller re-associates by id.
func (tx *UserTx) GetLabels(ctx context.Context, ids []ulid.ULID) ([]LabelView, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT l.id, l.name, COALESCE(l.color, ''), l.parent_id, l.mirror_to_imap,
		       COALESCE(k.keyword_atom, ''), COALESCE(l.tab::text, '')
		  FROM app.labels l
		  LEFT JOIN app.label_keyword_map k
		    ON k.user_id = l.user_id AND k.label_id = l.id
		 WHERE l.user_id = $1 AND l.id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: get labels: %w", err)
	}
	defer rows.Close()
	return scanLabels(rows)
}

// scanLabels drains a label result set into LabelViews.
func scanLabels(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]LabelView, error) {
	var out []LabelView
	for rows.Next() {
		var (
			lv     LabelView
			id     string
			parent *string
			tab    string
		)
		if err := rows.Scan(&id, &lv.Name, &lv.Color, &parent, &lv.MirrorToIMAP,
			&lv.KeywordAtom, &tab); err != nil {
			return nil, fmt.Errorf("store: scan label: %w", err)
		}
		var err error
		if lv.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse label id %q: %w", id, err)
		}
		if tab != "" {
			t, ok := tabs.Parse(tab)
			if !ok {
				// Only reachable if the database carries a tab this binary does not
				// know — a downgrade past the migration that added it. Refused rather
				// than dropped, since silently reading a mapped label as unmapped
				// would move the user's mail without saying so.
				return nil, fmt.Errorf("store: unknown category tab %q on label %s", tab, id)
			}
			lv.Tab = t
		}
		if parent != nil {
			p, err := ulid.Parse(*parent)
			if err != nil {
				return nil, fmt.Errorf("store: parse label parent id %q: %w", *parent, err)
			}
			lv.ParentID = &p
		}
		out = append(out, lv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan labels: %w", err)
	}
	return out, nil
}

// KeywordAtomForLabel returns the label's stable IMAP keyword atom, creating and
// persisting it on first call. The mapping is deterministic on first assignment
// (MailroomLabel-<slug>, slug derived from the current name) and then NEVER
// re-derived: a subsequent rename cannot change or orphan it (spec §6). If the
// base atom is taken by another label it is collision-suffixed (-2, -3, ...).
//
// Assignment is done under a SAVEPOINT so a UNIQUE-collision does not poison the
// surrounding UserTx, and the UNIQUE (user_id, keyword_atom) constraint is the
// race backstop: a concurrent assignment for the SAME label is detected (PK
// conflict) and its atom returned; a collision on the atom bumps the suffix.
func (tx *UserTx) KeywordAtomForLabel(ctx context.Context, labelID ulid.ULID) (string, error) {
	// Already mapped → return the persisted atom (stable, never re-derived).
	if atom, _, ok, err := tx.labelKeywordAtom(ctx, labelID); err != nil {
		return "", err
	} else if ok {
		return atom, nil
	}

	var name string
	err := tx.tx.QueryRow(ctx,
		`SELECT name FROM app.labels WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), labelID.String()).Scan(&name)
	if err != nil {
		if isNoRows(err) {
			return "", ErrLabelNotFound
		}
		return "", fmt.Errorf("store: keyword atom for label (read name): %w", err)
	}
	base := keywordAtomBase(name)

	for i := 0; i <= maxAtomCollisionAttempts; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", base, i+1) // -2, -3, ...
		}
		sp, err := tx.tx.Begin(ctx) // SAVEPOINT
		if err != nil {
			return "", fmt.Errorf("store: keyword atom for label (savepoint): %w", err)
		}
		_, err = sp.Exec(ctx, `
			INSERT INTO app.label_keyword_map (user_id, label_id, keyword_atom)
			VALUES ($1, $2, $3)`,
			tx.userID.String(), labelID.String(), candidate)
		if err == nil {
			if cerr := sp.Commit(ctx); cerr != nil {
				return "", fmt.Errorf("store: keyword atom for label (release): %w", cerr)
			}
			return candidate, nil
		}
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if !isUniqueViolation(err) {
			return "", fmt.Errorf("store: keyword atom for label (insert): %w", err)
		}
		// Either this label was mapped concurrently (PK conflict) — return that
		// atom — or the candidate atom is taken by another label — bump suffix.
		if atom, _, ok, rerr := tx.labelKeywordAtom(ctx, labelID); rerr != nil {
			return "", rerr
		} else if ok {
			return atom, nil
		}
	}
	return "", fmt.Errorf("store: keyword atom for label %s: exhausted collision suffixes", labelID)
}

// keywordAtomBase renders the deterministic base atom for a label name:
// the fixed prefix plus a slug sanitized to the IMAP atom/keyword charset.
func keywordAtomBase(name string) string {
	slug := slugForKeyword(name)
	if slug == "" {
		// A name with no atom-safe characters (e.g. all emoji) still needs a
		// base; collision-suffixing keeps distinct such labels apart.
		slug = "label"
	}
	return keywordAtomPrefix + slug
}

// slugForKeyword lowercases name and reduces it to [a-z0-9-]: any run of other
// characters collapses to a single '-', with leading/trailing '-' trimmed. The
// output is a strict subset of the IMAP atom charset the label_keyword_map CHECK
// constraint enforces, so it is always a valid keyword.
func slugForKeyword(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteByte(byte(r))
			prevDash = false
		default:
			if b.Len() > 0 && !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// MirrorKeyword reports whether a label keyword should be mirrored to a specific
// mailbox: only when the label has a keyword atom (mirror_to_imap assigned one)
// AND the mailbox advertised keyword support via its PERMANENTFLAGS `\*` probe
// (app.mailboxes.accepts_keywords). A false result means the label stays
// LOCAL-ONLY for that mailbox — surfaced in account health, not mirrored
// (spec §6). This is the pure decision function; MailboxAcceptsKeywords supplies
// the per-mailbox probe result.
func MirrorKeyword(atom string, acceptsKeywords bool) bool {
	return atom != "" && acceptsKeywords
}

// MailboxAcceptsKeywords reports the recorded PERMANENTFLAGS probe result for a
// mailbox (app.mailboxes.accepts_keywords): whether the server will store user
// keywords there. found is false when the mailbox does not exist for the caller.
// The writeback path pairs this with MirrorKeyword to skip mirroring a label
// keyword into a mailbox that rejects keywords (local-only, spec §6).
//
// ⚠ NOT CALLED OUTSIDE TESTS (checked for #740), and neither is MirrorKeyword:
// label mirroring is not gated on the probe today. The value is recorded from
// read-write SELECTs only (RecordKeywordProbes), so false also means "Mailroom
// has not written to this mailbox yet" — wire a gate to it with that in mind.
func (tx *UserTx) MailboxAcceptsKeywords(ctx context.Context, mailboxID ulid.ULID) (accepts, found bool, err error) {
	e := tx.tx.QueryRow(ctx,
		`SELECT accepts_keywords FROM app.mailboxes WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), mailboxID.String()).Scan(&accepts)
	if e != nil {
		if isNoRows(e) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("store: mailbox accepts keywords: %w", e)
	}
	return accepts, true, nil
}

// isUniqueViolation reports whether err is a PostgreSQL unique_violation (23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
