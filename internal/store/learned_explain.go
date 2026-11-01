// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/senderwarn"
)

// WHY LEARNED PLACEMENT HELD BACK (#840).
//
// learnedArrival is the arrival decision, steps 1 to 4b of
// LearnedPlacementForNewMessage, as a READ-ONLY function that returns a
// structured trace. The live path (learnedPlacement) runs it and then does the
// gate and the write; `mailroom-admin placement-explain` runs the same
// function inside a read-only transaction and prints the trace. There is no
// second copy of the logic to drift: TestPlacementExplainMatchesLive holds the
// two to the same label.
//
// Two options differ between them, and only to ADD information:
//
//   - Full keeps going after the first exit, so the trace carries the
//     evidence and every sender check even when an earlier step already
//     decided. The Result is still the FIRST exit's.
//   - Reconstruct reads the message's area as it was AT ARRIVAL (labels a
//     rule put on it, else the tab verdict), because by the time an operator
//     asks, the message carries the label learned placement or the desk put
//     on it, and a plain ResolveAreas would answer "decided by a label".

// arrivalOpts selects the explain flavour; the zero value is the live path.
type arrivalOpts struct {
	Full        bool
	Reconstruct bool
	// AsOf counts only filings and corrections created before it (zero = now).
	AsOf time.Time
}

// LearnedTrace is everything the arrival decision looked at, in the order it
// looked.
type LearnedTrace struct {
	// Enabled: the user's learned-placement switch (step 1).
	Enabled bool
	// KeysOK is false when the From has no usable address (NoKey).
	KeysOK          bool
	Address, Domain placement.Key
	// Ambiguous: not one mailbox in one From header (step 2).
	Ambiguous bool
	// Current is the area the message resolves to (step 3): live, as ingest
	// sees it; explained, as it was at arrival. ByLabel is a label deciding it.
	Current AreaResolution
	// Evidence and Decision are step 4. Decided is false when step 4 did not
	// run (the live path stops earlier).
	Decided  bool
	Evidence LearnedEvidence
	Decision placement.Decision
	// Checks is step 4b, nil when it did not run: the live path asks only when
	// the decision acts, the explain path always.
	Checks *SenderChecks
	// AgreesWithDesk: the learned area is the area the message already
	// resolves to.
	AgreesWithDesk bool
	// Result is the live path's result up to the gate; Continue says the live
	// path goes on to the gate and the write. After ExplainLearnedPlacement,
	// Result is the final outcome (gated, or Placed meaning "would place").
	Result   LearnedPlacementResult
	Continue bool
	// FirstRefusal is the first step that stopped the placement, "" if none.
	FirstRefusal string

	done bool
}

// learnedEnabled is the user's learned-placement switch, read server-side. No
// desk_preferences row yet means the column default, which is ON (#714).
// Shared by the arrival decision and by Area/ruleSuggestion, so the two cannot
// disagree about whether learned placement is on.
func (tx *UserTx) learnedEnabled(ctx context.Context) (bool, error) {
	var on bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT learned_placement_enabled FROM app.desk_preferences WHERE user_id = $1), true)`,
		tx.userID.String()).Scan(&on); err != nil {
		return false, fmt.Errorf("store: learned placement (switch): %w", err)
	}
	return on, nil
}

// learnedArrival runs steps 1 to 4b. It writes nothing.
func (tx *UserTx) learnedArrival(ctx context.Context, msgID ulid.ULID, sender placement.Sender, opt arrivalOpts) (LearnedTrace, error) {
	var tr LearnedTrace
	// stop records the FIRST exit and reports whether the caller returns now
	// (always, on the live path).
	stop := func(step string, r LearnedPlacementResult) bool {
		if !tr.done {
			tr.done = true
			tr.Result = r
			tr.FirstRefusal = step
		}
		return !opt.Full
	}

	// 1. THE OFF SWITCH, server-side. No desk_preferences row yet means the
	// column default, which is ON (#714: a design decision).
	var err error
	if tr.Enabled, err = tx.learnedEnabled(ctx); err != nil {
		return LearnedTrace{}, err
	}
	if !tr.Enabled && stop("learned placement is switched off", LearnedPlacementResult{Reason: LearnedOff}) {
		return tr, nil
	}

	// 2. THE KEY, from the one sender value, and refuse an ambiguous From
	// before anything is decided on it.
	addrKey, domainKey, ok := sender.Keys()
	tr.KeysOK, tr.Address, tr.Domain = ok, addrKey, domainKey
	if !ok {
		stop("the From has no usable address", LearnedPlacementResult{Reason: placement.NoKey})
		return tr, nil // nothing below can run without keys
	}
	tr.Ambiguous = sender.Ambiguous()
	if tr.Ambiguous && stop("the From is not one mailbox in one header", LearnedPlacementResult{Reason: placement.AmbiguousFrom, Key: addrKey}) {
		return tr, nil
	}

	// 3. NEVER OVER A PLACEMENT ALREADY MADE. At ingest the only labels that
	// can be on the message are a rule's; a label-decided area is final.
	var current AreaResolution
	if opt.Reconstruct {
		var err error
		if current, err = tx.arrivalAreaState(ctx, msgID); err != nil {
			return LearnedTrace{}, err
		}
	} else {
		res, err := tx.ResolveAreas(ctx, []ulid.ULID{msgID})
		if err != nil {
			return LearnedTrace{}, err
		}
		current = res[msgID]
	}
	tr.Current = current
	if current.ByLabel && stop("a label already decides the message's area", LearnedPlacementResult{Reason: LearnedAlreadyPlaced}) {
		return tr, nil
	}

	// 4. THE EVIDENCE AND THE DECISION.
	d, ev, err := tx.decideLearnedEvidence(ctx, addrKey, domainKey, opt.AsOf)
	if err != nil {
		return LearnedTrace{}, err
	}
	tr.Decided, tr.Decision, tr.Evidence = true, d, ev
	out := LearnedPlacementResult{Reason: d.Reason, Key: d.Key}
	if !d.Act {
		if stop("the evidence does not support acting ("+string(d.Reason)+")", out) {
			return tr, nil
		}
	} else {
		area := d.Area
		out.AreaID = &area
	}

	// 4b. THE SENDER MUST BE WHO IT CLAIMS: senderRefusal, the one function
	// the backfill (placement_backfill.go) asks too. The live path asks only
	// when the decision acts; the explain path always, to show the inputs.
	if d.Act || opt.Full {
		sd, err := tx.SenderDomainsFor(ctx)
		if err != nil {
			return LearnedTrace{}, err
		}
		c := senderChecks(sender, senderwarn.NewIndex(sd.Own, sd.Corresponded))
		tr.Checks = &c
	}
	if d.Act {
		area := d.Area
		tr.AgreesWithDesk = current.AreaID != nil && *current.AreaID == area
		if r := tr.Checks.Refusal; r != "" {
			refused := out
			refused.Reason = r
			if stop("sender check: "+string(r), refused) {
				return tr, nil
			}
		}
		if tr.AgreesWithDesk {
			agrees := out
			agrees.Reason = LearnedAgreesWithDesk
			if stop("the tab verdict already puts the message in the learned area", agrees) {
				return tr, nil
			}
		}
	}
	if !tr.done {
		tr.Continue = true
		tr.Result = out
	}
	return tr, nil
}

// postArrivalSources are the label sources that act AFTER a message arrived
// (or, for the desk and day-one setup, independently of learned placement),
// so the explain's view of the arrival leaves their labels out.
var postArrivalSources = []string{LabelByUser, LabelByAgent, LabelByLearned, LabelByDesk, LabelBySetup}

// arrivalAreaState is the message's area as it was when it arrived: a label
// that feeds an area and was NOT put on by the user, an API key, learned
// placement, the desk or day-one setup (what is left is a rule's, applied
// before learned placement runs), else the tab verdict. Read now, so a tab
// label the user has changed since shows the changed verdict.
//
// An API key's label (#49) is applied after arrival, like the user's: read
// as a rule's, a key's area move would make the explain report "a label
// already placed it" for a message that arrived with none.
func (tx *UserTx) arrivalAreaState(ctx context.Context, msgID ulid.ULID) (AreaResolution, error) {
	var label *string
	err := tx.tx.QueryRow(ctx, `
		SELECT f.area_id
		  FROM app.message_labels ml
		  JOIN app.area_feeds f ON f.user_id = ml.user_id AND f.label_id = ml.label_id
		  JOIN app.areas a ON a.user_id = f.user_id AND a.id = f.area_id
		 WHERE ml.user_id = $1 AND ml.message_id = $2 AND NOT (ml.applied_by = ANY($3))
		 ORDER BY a.position, a.id
		 LIMIT 1`, tx.userID.String(), msgID.String(),
		postArrivalSources).Scan(&label)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AreaResolution{}, fmt.Errorf("store: learned placement explain (arrival label): %w", err)
	}
	if label != nil {
		id, perr := ulid.Parse(*label)
		if perr != nil {
			return AreaResolution{}, fmt.Errorf("store: parse area id %q: %w", *label, perr)
		}
		return AreaResolution{AreaID: &id, ByLabel: true}, nil
	}
	// The tab verdict must not read the labels learned placement, the desk or
	// the user put on the message since: an area's own label carries a tab, so
	// the label learned placement wrote would otherwise make the message
	// "already in the learned area". The restriction rides areaJoinSQL's
	// restrict hook (a condition over the `ml` alias), so the tab layer itself
	// is the desk's own.
	args := []any{tx.userID.String(), msgID.String()}
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	joins, _, tabArea := areaJoinSQL(bind, func(col string) string {
		return col + " = $2 AND NOT (ml.applied_by = ANY(" + bind(postArrivalSources) + "))"
	})
	var tab *string
	if err := tx.tx.QueryRow(ctx, `SELECT `+tabArea+` FROM app.messages m`+joins+`
		 WHERE m.user_id = $1 AND m.id = $2`, args...).Scan(&tab); err != nil {
		return AreaResolution{}, fmt.Errorf("store: learned placement explain (tab verdict): %w", err)
	}
	if tab == nil {
		return AreaResolution{}, nil
	}
	id, perr := ulid.Parse(*tab)
	if perr != nil {
		return AreaResolution{}, fmt.Errorf("store: parse area id %q: %w", *tab, perr)
	}
	return AreaResolution{AreaID: &id}, nil
}

// ExplainLearnedPlacement re-runs the arrival decision for msgID read-only and
// returns its trace. It never writes: it needs a UserReadTx, whose transaction
// refuses writes. asOf, when not zero, counts only filings and corrections
// created before it; the gate, the user's own domains, the sender-warning
// index and the tab verdict are always read as of NOW.
//
// The Result is the outcome the live path would reach now: Placed means
// "would place" (nothing was written), Reason LearnedGated means the gate
// stops the area.
func (tx *UserTx) ExplainLearnedPlacement(ctx context.Context, msgID ulid.ULID, sender placement.Sender, asOf time.Time) (LearnedTrace, error) {
	tr, err := tx.learnedArrival(ctx, msgID, sender, arrivalOpts{Full: true, Reconstruct: true, AsOf: asOf})
	if err != nil {
		return LearnedTrace{}, err
	}
	if !tr.Continue {
		return tr, nil
	}
	area := *tr.Result.AreaID
	g, err := tx.learnedGate(ctx, area, false)
	if err != nil {
		return LearnedTrace{}, err
	}
	tr.Result.Gate = &g
	if g.Stopped {
		tr.Result.Reason = LearnedGated
		tr.FirstRefusal = "the area's correction gate is stopped"
		return tr, nil
	}
	tr.Result.Placed = true
	return tr, nil
}

// UserReadTx is UserTx for a read that must not write: the transaction is
// READ ONLY (Postgres refuses any write in it) and is always rolled back,
// never committed.
func (s *Store) UserReadTx(ctx context.Context, userID ulid.ULID, fn func(tx *UserTx) error) error {
	if userID == (ulid.ULID{}) {
		return ErrNoIdentity
	}
	ptx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("store: begin read-only tx: %w", err)
	}
	defer func() { _ = ptx.Rollback(ctx) }()
	if _, err := ptx.Exec(ctx, `SELECT set_config('mailroom.user_id', $1, true)`, userID.String()); err != nil {
		return fmt.Errorf("store: set user identity: %w", err)
	}
	retention := s.trashRetention
	if retention <= 0 {
		retention = DefaultTrashRetention
	}
	return fn(&UserTx{tx: ptx, userID: userID, trashRetention: retention, writebackDisabled: s.writebackDisabled})
}

// ── the closed label set of mailroom_learned_placement_total ────────────────

// LearnedLabelPlaced and friends are the counter's reasons that are not
// placement.Reason strings.
const (
	// LearnedLabelPlaced: learned placement filed the message.
	LearnedLabelPlaced = "placed"
	// LearnedLabelAlreadyPlaced: a rule's label had already decided the area.
	// Not "placed": that word is the success.
	LearnedLabelAlreadyPlaced = "already_placed"
	// LearnedLabelError: the decision failed (the desk places the message).
	LearnedLabelError = "error"
	// LearnedLabelOther: a reason outside the closed set.
	LearnedLabelOther = "other"
)

// LearnedPlacementLabels is every value the counter's reason label can take,
// so a server can create them all at zero.
func LearnedPlacementLabels() []string {
	return []string{
		LearnedLabelPlaced, LearnedLabelAlreadyPlaced, string(LearnedAgreesWithDesk), string(LearnedGated),
		string(LearnedOff), string(placement.NoEvidence), string(placement.Thin), string(placement.Mixed),
		string(placement.Contradicted), string(placement.NoKey), string(placement.DomainBlocked),
		string(placement.Unauthenticated), string(placement.SenderWarned), string(placement.AmbiguousFrom),
		LearnedLabelError, LearnedLabelOther,
	}
}

// Label is the result's counter reason: bounded, anything unknown "other".
func (r LearnedPlacementResult) Label() string {
	switch {
	case r.Placed:
		return LearnedLabelPlaced
	case r.Reason == LearnedAlreadyPlaced:
		return LearnedLabelAlreadyPlaced
	}
	for _, l := range LearnedPlacementLabels() {
		if string(r.Reason) == l && l != LearnedLabelPlaced {
			return l
		}
	}
	return LearnedLabelOther
}

// ── explain's reads ─────────────────────────────────────────────────────────

// ExplainCandidate is a message to explain: what the sender source needs to
// read its bytes, and when it was ingested.
type ExplainCandidate struct {
	PlacementCandidate
	// CreatedAt is when Mailroom ingested the message (messages.created_at):
	// explain's --as-of.
	CreatedAt time.Time
}

// ExplainCandidates reads one message by id (messageID) or the newest limit
// non-draft messages whose stored From is address (case-insensitive), newest
// first by ingest time. Read-only.
func (tx *UserTx) ExplainCandidates(ctx context.Context, messageID *ulid.ULID, address string, limit int) ([]ExplainCandidate, error) {
	q := `SELECT m.id, m.content_hash, COALESCE(m.from_address::text, ''), m.received_at, ` + providerDatedExpr("m") + `, m.created_at
	        FROM app.messages m
	       WHERE m.user_id = $1 `
	args := []any{tx.userID.String()}
	if messageID != nil {
		q += `AND m.id = $2`
		args = append(args, messageID.String())
	} else {
		q += `AND NOT m.is_draft AND lower(btrim(m.from_address::text)) = $2 ORDER BY m.created_at DESC, m.id DESC LIMIT $3`
		args = append(args, strings.ToLower(strings.TrimSpace(address)), limit)
	}
	rows, err := tx.tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: placement explain (messages): %w", err)
	}
	defer rows.Close()
	var out []ExplainCandidate
	for rows.Next() {
		var idStr string
		var c ExplainCandidate
		var received time.Time
		var dated bool
		if err := rows.Scan(&idStr, &c.ContentHash, &c.From, &received, &dated, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: placement explain (scan): %w", err)
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", idStr, err)
		}
		c.MessageID = id
		c.ReceivedAt = TrustTime(received, dated)
		out = append(out, c)
	}
	return out, rows.Err()
}

// AreaNames maps every area id to its name, read-only (ListAreas would
// materialise a fresh account's default plan).
func (tx *UserTx) AreaNames(ctx context.Context) (map[ulid.ULID]string, error) {
	rows, err := tx.tx.Query(ctx, `SELECT id, name FROM app.areas WHERE user_id = $1`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: area names: %w", err)
	}
	defer rows.Close()
	out := map[ulid.ULID]string{}
	for rows.Next() {
		var idStr, name string
		if err := rows.Scan(&idStr, &name); err != nil {
			return nil, fmt.Errorf("store: scan area name: %w", err)
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			return nil, fmt.Errorf("store: parse area id %q: %w", idStr, err)
		}
		out[id] = name
	}
	return out, rows.Err()
}
