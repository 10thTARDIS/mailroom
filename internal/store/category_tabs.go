// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/kind"
	"mailroom/internal/tabs"
)

// CATEGORY TABS — the per-user state half of #219 (migration 00028).
//
// Two things are editable data here, because "I can rearrange label mapping
// later if I need to" is a requirement rather than an aside:
// WHICH TABS ARE ENABLED (app.category_tabs, this file) and WHERE EACH LABEL
// GOES (app.labels.tab, labels.go's LabelPatch). Neither is a Go map — a
// compiled mapping would need a release to change, which defeats the point.
//
// A CategoryTab is a JMAP-visible object: enabling one on the laptop has to
// reach the phone or it is a setting the user makes again on every device. So
// every mutation here appends its change_log row in the same transaction (the
// load-bearing invariant — CLAUDE.md).
//
// A MESSAGE'S TAB IS NOT STORED. It is derived at read time — labelled mail
// through the mapping (MessageTabMappingsAfter below), the rest from the
// function signals migration 00027 persisted — so an edit to the mapping takes
// effect on the next read with no backfill. Storing it would mean re-deriving
// the whole mailbox every time one label moved.

// CategoryTabView is one tab's state for one user.
type CategoryTabView struct {
	ID       ulid.ULID
	Tab      tabs.Tab
	Enabled  bool
	Position int
}

// CategoryTabPatch is a partial update. A nil pointer means "leave unchanged".
// There is no patch field for `tab`: the vocabulary is closed and a row's tab is
// its identity, so changing it would silently repoint whatever a client had
// already synced under that id.
type CategoryTabPatch struct {
	Enabled  *bool
	Position *int
}

// ErrPrimaryTabRequired is returned for an attempt to disable Primary.
//
// Primary is the residue — everything undecided lands there, and a message whose
// own tab is switched off FOLDS there (tabs.Fold, which is what unchecking a
// category does in Gmail). A disabled Primary would therefore be a tab that is
// off and still holding mail. app.category_tabs enforces this with a CHECK; this
// error exists so a client gets a sentence instead of a constraint name.
var ErrPrimaryTabRequired = errors.New("store: the Primary tab cannot be disabled")

// ListCategoryTabs returns every tab for the caller in strip order,
// MATERIALISING the rows on first call.
//
// The lazy materialisation is deliberate and there is no other place to put it.
// Mailroom has no Go-side user-provisioning step to hang a seed off — login runs
// through app.upsert_login, a SECURITY DEFINER — and seeding inside migration
// 00028 would create JMAP-visible rows with no change_log row, which is the one
// invariant this project does not bend. Assigning persistent state on first read
// already has precedent here: KeywordAtomForLabel does exactly this for label
// keyword atoms, for the same reason (the object must exist before anything can
// reference it, and nothing earlier in the lifecycle knows to create it).
//
// Ordering is by position then by the enum's own order, so ties break the same
// way on every read rather than by whatever the planner returns.
func (tx *UserTx) ListCategoryTabs(ctx context.Context) ([]CategoryTabView, error) {
	if err := tx.materialiseCategoryTabs(ctx); err != nil {
		return nil, err
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, tab::text, enabled, position
		  FROM app.category_tabs
		 WHERE user_id = $1
		 ORDER BY position, tab`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list category tabs: %w", err)
	}
	defer rows.Close()
	return scanCategoryTabs(rows)
}

// GetCategoryTabs returns the tabs for the requested ids (RLS-scoped), also
// materialising first so a client that syncs by id before ever listing does not
// see an empty account. Ids not returned were not found.
func (tx *UserTx) GetCategoryTabs(ctx context.Context, ids []ulid.ULID) ([]CategoryTabView, error) {
	if err := tx.materialiseCategoryTabs(ctx); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, tab::text, enabled, position
		  FROM app.category_tabs
		 WHERE user_id = $1 AND id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: get category tabs: %w", err)
	}
	defer rows.Close()
	return scanCategoryTabs(rows)
}

// EnabledTabs is the fold map for tabs.Fold: which tabs the caller can actually
// see. Primary is always present and true (the CHECK constraint guarantees it),
// so a fold always terminates somewhere visible.
func (tx *UserTx) EnabledTabs(ctx context.Context) (map[tabs.Tab]bool, error) {
	views, err := tx.ListCategoryTabs(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[tabs.Tab]bool, len(views))
	for _, v := range views {
		out[v.Tab] = v.Enabled
	}
	return out, nil
}

// materialiseCategoryTabs inserts whatever of the five vocabulary rows the
// caller does not have yet, and appends a `created` change_log row for each row
// it ACTUALLY inserted — never for one that was already there, or a repeat read
// would emit a change per call and every client would resync forever.
//
// NO SAVEPOINT, unlike the creates in labels.go and saved_searches.go: ON
// CONFLICT DO NOTHING is not an error, so nothing here can abort the surrounding
// transaction. A concurrent materialisation blocks on the unique index and then
// finds nothing to insert, which is the correct outcome rather than a race.
func (tx *UserTx) materialiseCategoryTabs(ctx context.Context) error {
	ids := make([]string, len(tabs.All))
	names := make([]string, len(tabs.All))
	positions := make([]int32, len(tabs.All))
	for i, t := range tabs.All {
		ids[i] = NewULID().String()
		names[i] = string(t)
		// tabs.All is Gmail's own strip order, which is the default arrangement;
		// the user moves them from there.
		positions[i] = int32(i)
	}
	rows, err := tx.tx.Query(ctx, `
		INSERT INTO app.category_tabs (id, user_id, tab, position)
		SELECT v.id::app.ulid, $1, v.tab::app.category_tab, v.position
		  FROM unnest($2::text[], $3::text[], $4::int[]) AS v(id, tab, position)
		ON CONFLICT (user_id, tab) DO NOTHING
		RETURNING id`,
		tx.userID.String(), ids, names, positions)
	if err != nil {
		return fmt.Errorf("store: materialise category tabs: %w", err)
	}
	var created []ulid.ULID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return fmt.Errorf("store: materialise category tabs: scan: %w", err)
		}
		id, err := ulid.Parse(raw)
		if err != nil {
			rows.Close()
			return fmt.Errorf("store: category tab id %q: %w", raw, err)
		}
		created = append(created, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: materialise category tabs: %w", err)
	}
	for _, id := range created {
		if _, err := tx.AppendChange(ctx, ObjectCategoryTab, id, OpCreated); err != nil {
			return err
		}
	}
	return nil
}

// UpdateCategoryTab applies a partial update and appends exactly one CategoryTab
// change_log row when something actually changes (a no-op patch is a true no-op,
// like UpdateLabel). found is false when the id does not resolve for the caller.
func (tx *UserTx) UpdateCategoryTab(ctx context.Context, id ulid.ULID, patch CategoryTabPatch) (found bool, err error) {
	var (
		tab      string
		enabled  bool
		position int
	)
	err = tx.tx.QueryRow(ctx, `
		SELECT tab::text, enabled, position
		  FROM app.category_tabs
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`,
		tx.userID.String(), id.String()).Scan(&tab, &enabled, &position)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: update category tab (read): %w", err)
	}

	nEnabled, nPosition := enabled, position
	if patch.Enabled != nil {
		nEnabled = *patch.Enabled
	}
	if patch.Position != nil {
		nPosition = *patch.Position
	}
	// Refused here so the caller gets ErrPrimaryTabRequired rather than a
	// constraint violation. The CHECK in migration 00028 remains the backstop —
	// this check is the message, not the enforcement.
	if tab == string(tabs.Primary) && !nEnabled {
		return true, ErrPrimaryTabRequired
	}

	// Under a SAVEPOINT, and honestly: NOTHING CURRENTLY REACHES IT. The Go check
	// above fires before any statement runs, so the CHECK constraint is
	// unreachable through this function and the savepoint protects a case that
	// does not occur. Measured, not assumed — deleting the Go check makes the
	// violation surface as a whole-method serverFail, which is what proves the Go
	// check (not this savepoint) is what makes the refusal per-object.
	//
	// It stays because the shape around it is a trap this project has already
	// fallen into: a failed statement aborts the whole surrounding UserTx
	// (SQLSTATE 25P02), so the moment this UPDATE gains a recoverable per-object
	// failure — a uniqueness rule on position, say — a bare statement here would
	// silently lose every OTHER object in the same CategoryTab/set while
	// faithfully reporting the one that failed. Cheap now, and the alternative is
	// rediscovering that from a bug report.
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return false, fmt.Errorf("store: update category tab (savepoint): %w", err)
	}
	var touched string
	err = sp.QueryRow(ctx, `
		UPDATE app.category_tabs
		   SET enabled = $3, position = $4, updated_at = now()
		 WHERE user_id = $1 AND id = $2
		   AND (enabled, position) IS DISTINCT FROM ($3, $4)
		RETURNING id`,
		tx.userID.String(), id.String(), nEnabled, nPosition).Scan(&touched)
	if err != nil {
		if isNoRows(err) {
			// The row exists (it is locked) but nothing changed: a true no-op, so no
			// change_log row. No SQL error occurred, so RELEASE rather than ROLLBACK.
			if cerr := sp.Commit(ctx); cerr != nil {
				return false, fmt.Errorf("store: update category tab (release): %w", cerr)
			}
			return true, nil
		}
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		return false, fmt.Errorf("store: update category tab (write): %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return false, fmt.Errorf("store: update category tab (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectCategoryTab, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

func scanCategoryTabs(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]CategoryTabView, error) {
	var out []CategoryTabView
	for rows.Next() {
		var (
			v   CategoryTabView
			id  string
			tab string
		)
		if err := rows.Scan(&id, &tab, &v.Enabled, &v.Position); err != nil {
			return nil, fmt.Errorf("store: scan category tab: %w", err)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: category tab id %q: %w", id, err)
		}
		t, ok := tabs.Parse(tab)
		if !ok {
			// The column is an enum, so this is only reachable if the database has
			// a value the binary does not know — i.e. a downgrade past a migration
			// that added a tab. Refused rather than coerced to Primary, which would
			// silently move the user's mail.
			return nil, fmt.Errorf("store: unknown category tab %q", tab)
		}
		v.ID, v.Tab = parsed, t
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan category tabs: %w", err)
	}
	return out, nil
}

// MessageTabFacts is one message as the tab classifier and its replay see it:
// the header facts, plus the label→tab mapping that is its ground truth, plus
// the sender the metrics group by.
//
// THE FACTS COME OUT OF THE ROW, never the blob. That is what migration 00027
// was for, and it is also why this classifier CANNOT have a train/serve gap: a
// message's tab is derived at read time (ADR-021), so replay and serve are the
// same path reading the same columns. There is no second extraction to disagree
// with the first — the failure #157 was fixed to close.
type MessageTabFacts struct {
	ID ulid.ULID
	// Facts is the classifier's input, assembled here so there is exactly one
	// row→Facts projection in the codebase.
	Facts kind.Facts
	// Labels is how many labels the message carries, and Mapped the distinct tabs
	// those labels point to. Together they are the ground truth: empty Mapped with
	// Labels > 0 is a mapping gap, empty Mapped with Labels == 0 is unlabelled
	// mail. The two have opposite fixes and are never merged.
	Labels int
	Mapped []Tab
}

// Tab is a category tab as the store hands it back. Aliased to the tabs
// package's type so callers do not convert, and so a store consumer cannot
// invent a sixth value.
type Tab = tabs.Tab

// MessageTabFactsAfter walks the caller's messages in ascending id order,
// returning each one's classifier input and mapped tabs. Keyset paging, the same
// shape as MessageBlobRefsAfter. RLS scopes it to the caller.
//
// HasCorresponded is deliberately never set — see internal/tabs/classify.go. A
// tab decided partly by the user's own behaviour would forfeit the property the
// whole feature rests on: that function is sender-independent, so the tab is
// right on a stranger's FIRST message.
func (tx *UserTx) MessageTabFactsAfter(ctx context.Context, after ulid.ULID, limit int) ([]MessageTabFacts, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id,
		       COALESCE(m.from_address, ''), COALESCE(m.subject, ''),
		       COALESCE(m.list_id, ''), COALESCE(m.list_unsubscribe, ''),
		       COALESCE(m.list_unsubscribe_post, ''), COALESCE(m.auto_submitted, ''),
		       COALESCE(m.precedence, ''), COALESCE(m.feedback_id, ''),
		       COALESCE(m.dkim_domains, '{}'::text[]), COALESCE(m.return_path, ''),
		       COALESCE(m.recipient_count, 0),
		       (m.in_reply_to IS NOT NULL AND m.in_reply_to <> '') AS is_reply,
		       count(ml.label_id)::int AS label_count,
		       COALESCE(
		         array_agg(DISTINCT l.tab::text) FILTER (WHERE l.tab IS NOT NULL),
		         '{}'::text[]) AS tabs
		  FROM app.messages m
		  LEFT JOIN app.message_labels ml
		         ON ml.user_id = m.user_id AND ml.message_id = m.id
		  LEFT JOIN app.labels l
		         ON l.user_id = ml.user_id AND l.id = ml.label_id
		 WHERE m.user_id = $1 AND m.id > $2
		 GROUP BY m.id
		 ORDER BY m.id
		 LIMIT $3`,
		tx.userID.String(), after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: walk message tab facts: %w", err)
	}
	defer rows.Close()

	var out []MessageTabFacts
	for rows.Next() {
		var (
			id      string
			m       MessageTabFacts
			rawTabs []string
		)
		if err := rows.Scan(&id,
			&m.Facts.FromAddress, &m.Facts.Subject,
			&m.Facts.ListID, &m.Facts.ListUnsubscribe,
			&m.Facts.ListUnsubscribePost, &m.Facts.AutoSubmitted,
			&m.Facts.Precedence, &m.Facts.FeedbackID,
			&m.Facts.DKIMDomains, &m.Facts.ReturnPath,
			&m.Facts.RecipientCount, &m.Facts.IsReply,
			&m.Labels, &rawTabs); err != nil {
			return nil, fmt.Errorf("store: scan message tab facts: %w", err)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, err)
		}
		m.ID = parsed
		m.Facts.FromDomain = addressDomain(m.Facts.FromAddress)
		for _, s := range rawTabs {
			t, ok := tabs.Parse(s)
			if !ok {
				return nil, fmt.Errorf("store: unknown category tab %q on message %s", s, id)
			}
			m.Mapped = append(m.Mapped, t)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: walk message tab facts: %w", err)
	}
	return out, nil
}

// addressDomain is the domain half of an address, lowercased, or "".
//
// Derived here rather than stored: app.messages has no from_domain column, and
// adding one to save a string split would be a projection with no query behind
// it. kind.Facts wants both halves and the ESP/social lookups both normalise
// again anyway.
func addressDomain(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(addr[at+1:]))
}

// MessageTabMapping is one message's input to the tab resolver: how many labels
// it carries, and the distinct tabs those labels map to.
//
// Labels and Mapped are BOTH reported because they answer different questions
// and the difference is the point. Labels == 0 means unlabelled mail, which the
// classifier must decide from the function facts. Labels > 0 with an empty
// Mapped means the mail IS labelled but the operator has not mapped those labels
// to a tab yet — a mapping gap they can close in one edit, not a classifier
// workload. Collapsing the two into "unresolved" would hide which of the two it
// is, and they have opposite fixes.
type MessageTabMapping struct {
	ID     ulid.ULID
	Labels int
	Mapped []tabs.Tab
}

// MessageTabMappingsAfter walks the caller's messages in ascending id order,
// returning each one's label→tab mapping input. `after` is an exclusive low
// bound (pass the zero ULID to start); ULIDs sort by creation time, so this is a
// keyset walk over the primary key with no offset cost — the same shape as
// MessageBlobRefsAfter. RLS scopes it to the caller.
//
// The mapping is a JOIN, which is the entire reason a message's tab is not
// stored: re-pointing one label moves all its mail on the next read, with no
// backfill and nothing left stale.
func (tx *UserTx) MessageTabMappingsAfter(ctx context.Context, after ulid.ULID, limit int) ([]MessageTabMapping, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id,
		       count(ml.label_id)::int AS label_count,
		       COALESCE(
		         array_agg(DISTINCT l.tab::text) FILTER (WHERE l.tab IS NOT NULL),
		         '{}'::text[]) AS tabs
		  FROM app.messages m
		  LEFT JOIN app.message_labels ml
		         ON ml.user_id = m.user_id AND ml.message_id = m.id
		  LEFT JOIN app.labels l
		         ON l.user_id = ml.user_id AND l.id = ml.label_id
		 WHERE m.user_id = $1 AND m.id > $2
		 GROUP BY m.id
		 ORDER BY m.id
		 LIMIT $3`,
		tx.userID.String(), after.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: walk message tab mappings: %w", err)
	}
	defer rows.Close()

	var out []MessageTabMapping
	for rows.Next() {
		var (
			id  string
			m   MessageTabMapping
			raw []string
		)
		if err := rows.Scan(&id, &m.Labels, &raw); err != nil {
			return nil, fmt.Errorf("store: scan message tab mapping: %w", err)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, err)
		}
		m.ID = parsed
		for _, s := range raw {
			t, ok := tabs.Parse(s)
			if !ok {
				return nil, fmt.Errorf("store: unknown category tab %q on message %s", s, id)
			}
			m.Mapped = append(m.Mapped, t)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: walk message tab mappings: %w", err)
	}
	return out, nil
}
