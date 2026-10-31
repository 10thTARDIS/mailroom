// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/senderwarn"
)

// THE PLACEMENT BACKFILL (#714 increment 2a, ADR-032 addendum): apply learned
// placement to mail that is ALREADY on the desk, report first, with an exact
// undo. `mailroom-admin placement-backfill` is its only caller.
//
// ONE PLANNER. PlanPlacementBackfill is the backfill's report AND its apply:
// the command prints the plan, and --apply hands the SAME plan's moves to
// ApplyPlacementBackfill. placement-report's BACKFILL SIMULATION is the same
// planner run without a sender source (upper-bound mode), so the three
// cannot describe different decisions. Every decision is placement.Decide
// over the evidence query ingest reads (placementFilings,
// aggregatePlacementEvidence, placementCorrections).
//
// THE SAME SAFETY AS ARRIVAL. In exact mode (a PlacementSenderFunc is given)
// every message the planner would move has its raw headers read and judged by
// THE arrival-time functions: ingest.LearnedSender builds its
// placement.Sender (the From checked against the raw From headers, a trusted
// DMARC pass bound to the keyed From domain from the topmost trusted
// Authentication-Results header, ml.AuthResultsFail as the unconfigured
// floor), and senderRefusal (shared with learnedPlacement) refuses an
// ambiguous From, an unauthenticated sender and a sender warning. A message
// whose bytes cannot be read is skipped and counted. The area's correction
// gate is asked as at ingest.
//
// WHAT A MOVE IS. Re-file's swap, through the area-filing plan Rule/apply
// uses (PlanAreaFiling): the other areas' own labels an AUTOMATION applied
// (desk, setup) come off; the learned area's own label goes on, attributed
// 'learned'; a message carrying any user, rule, learned or other routing
// label is never touched (it is HELD before the planner decides, and
// PlanAreaFiling would report it contested if that changed). It is one
// ApplyEmailUpdate per message, so one Email change_log row, and no
// writeback: an area's own label is local-only (ADR-031), and the apply
// refuses outright if one ever carried a keyword atom.
//
// THE RECORD. Each move writes an app.learned_placements row (open, like an
// arrival placement), so the user's corrections of it contradict its key and
// the report shows its outcome, and an app.audit_log row: object_type
// 'PlacementBackfill', object_id the BATCH id, detail the message, the label
// added and every label removed WITH its attribution and timestamp. That row
// is the undo journal and the mark that tells a backfill placement from an
// arrival one (backfilledMessagesSQL), which keeps backfilled placements OUT
// of the correction gate. No migration: audit_log is already user-scoped,
// FORCE RLS and append-only by grant. See ADR-032's addendum.

// Backfill reasons beyond placement.Reason and the ingest-side Learned*.
const (
	// BackfillExcluded: the operator's --exclude names the message's address
	// or domain.
	BackfillExcluded placement.Reason = "excluded"
	// BackfillUnreadable: the message's stored bytes could not be read or
	// parsed, so the arrival-time checks cannot be run. Never moved.
	BackfillUnreadable placement.Reason = "unreadable"
	// BackfillPlacedBefore: learned placement already placed this message
	// once (an app.learned_placements row exists). If its label is gone, the
	// user took it off, and moving it again would overrule them.
	BackfillPlacedBefore placement.Reason = "placedbefore"
	// BackfillContested: the area-filing plan found a label the backfill may
	// not remove (PlanAreaFiling: contestedByUser/Rule/Other).
	BackfillContested placement.Reason = "contested"
	// BackfillChanged: at apply time the message, its labels or its key's
	// decision no longer match the plan (the user acted in between). Skipped:
	// the plan is a promise about the state it was made on.
	BackfillChanged placement.Reason = "changed"
	// BackfillGone: the message no longer exists.
	BackfillGone placement.Reason = "gone"
)

// Undo outcomes.
const (
	BackfillUndone        = "undone"
	BackfillAlreadyUndone = "alreadyUndone"
	// BackfillRefiled: the user (or a rule) has filed the message since the
	// batch moved it: the learned label is gone, or another routing label is
	// on. Their later decision stands; undo leaves the message alone.
	BackfillRefiled = "refiled"
)

// The audit_log vocabulary of the backfill journal.
const (
	backfillObjectType = "PlacementBackfill"
	backfillActionMove = "placement.backfill.move"
	backfillActionUndo = "placement.backfill.undo"
	backfillActor      = "admin:placement-backfill"
)

// ErrLearnedPlacementOff: the user has switched learned placement off, and the
// backfill refuses to write.
var ErrLearnedPlacementOff = errors.New("store: learned placement is switched off for this user")

// PlacementCandidate is what a sender source needs to read one message.
type PlacementCandidate struct {
	MessageID   ulid.ULID
	ContentHash []byte
	// From is the STORED From address (messages.from_address): the value the
	// key was derived from and the one placement.NewSender binds to.
	From string
	// ReceivedAt is the TRUST time (#766): messages.received_at (INTERNALDATE)
	// when the provider dated the message, the zero time when it did not
	// (archive rows, whose received_at may be the sender's Date header). What a
	// date-bounded MAILROOM_AUTHSERV_IDS entry is judged against.
	ReceivedAt time.Time
}

// PlacementSenderFunc builds a candidate's arrival-time placement.Sender from
// its stored bytes (ingest.BackfillSenders). An error means the bytes could
// not be read or parsed; the planner counts it as BackfillUnreadable.
type PlacementSenderFunc func(ctx context.Context, c PlacementCandidate) (placement.Sender, error)

// PlacementBackfillOptions configures the planner.
type PlacementBackfillOptions struct {
	// Exclude names addresses (with '@') or domains whose mail is skipped,
	// case-insensitively; a domain matches the From address's own domain
	// exactly, not its subdomains.
	Exclude []string
	// Senders turns on EXACT mode. Nil is placement-report's upper bound:
	// the stored auth_fail and the warnings that need no verdict.
	Senders PlacementSenderFunc
}

// PlacementBackfillMove is one message the plan moves.
type PlacementBackfillMove struct {
	MessageID ulid.ULID
	Key       placement.Key
	AreaID    ulid.ULID
	LabelID   ulid.ULID
	// NowIn is the area the message resolves to now; zero is Needs sorting.
	NowIn ulid.ULID
	// Remove is the automation-applied area labels the swap takes off.
	Remove []RemovedAreaLabel
}

// PlacementBackfillKey is one key's line of the plan: exactly what --apply
// does for it.
type PlacementBackfillKey struct {
	Key    placement.Key
	AreaID ulid.ULID
	// Moves is the number of messages the plan moves on this key, and NowIn
	// their breakdown by current area (zero ULID = Needs sorting).
	Moves int
	NowIn map[ulid.ULID]int
	// Skipped counts messages the key decided but that do not move, by
	// reason (the per-message checks, exclusion, the gate, contested).
	Skipped map[placement.Reason]int
}

// PlacementBackfillPlan is the planner's whole answer.
type PlacementBackfillPlan struct {
	Enabled bool
	// Exact: every would-move message passed the arrival-time checks on its
	// own headers. False is the upper bound (placement-report).
	Exact   bool
	Filings int

	// THE PARTITION of the scanned population (non-trashed, non-vanished,
	// non-draft mail): Scanned = Held + OffDesk + NoOp + sum(Abstain) +
	// len(Moves). Accounted() checks it.
	Scanned int
	Held    int
	OffDesk int
	NoOp    int
	Abstain map[placement.Reason]int
	Moves   []PlacementBackfillMove

	// Pairs is Moves by (now in -> learned).
	Pairs map[PlacementMove]int
	Keys  []PlacementBackfillKey
	// Excluded is the number of messages each --exclude entry removed, every
	// entry present (an entry that matched nothing shows 0, so a typo is
	// visible rather than silent).
	Excluded map[string]int
	// AreaNames names every area, for printing.
	AreaNames map[ulid.ULID]string
}

// Accounted is the sum of the partition, which must equal Scanned.
func (p PlacementBackfillPlan) Accounted() int {
	n := p.Held + p.OffDesk + p.NoOp + len(p.Moves)
	for _, c := range p.Abstain {
		n += c
	}
	return n
}

// normalizeExcludes lowercases and trims the --exclude entries, keeping
// order and dropping duplicates.
func normalizeExcludes(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range in {
		e = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(e)), "@")
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

// excludedBy returns the first exclude entry naming address or domain.
func excludedBy(excludes []string, address, domain placement.Key) (string, bool) {
	for _, e := range excludes {
		if (strings.Contains(e, "@") && e == address.Value) || (!strings.Contains(e, "@") && e == domain.Value) {
			return e, true
		}
	}
	return "", false
}

// PlanPlacementBackfill computes the plan. READ-ONLY: it writes nothing (it
// reads outcomes through effectiveOutcomeSQL rather than ageing them).
func (tx *UserTx) PlanPlacementBackfill(ctx context.Context, opts PlacementBackfillOptions) (PlacementBackfillPlan, error) {
	plan := PlacementBackfillPlan{
		Exact:    opts.Senders != nil,
		Abstain:  map[placement.Reason]int{},
		Pairs:    map[PlacementMove]int{},
		Excluded: map[string]int{},
	}
	excludes := normalizeExcludes(opts.Exclude)
	for _, e := range excludes {
		plan.Excluded[e] = 0
	}
	enabled, err := tx.learnedPlacementEnabled(ctx)
	if err != nil {
		return plan, err
	}
	plan.Enabled = enabled

	// The evidence and the context every decision reads, once.
	filings, err := tx.placementFilings(ctx, "", time.Time{})
	if err != nil {
		return plan, err
	}
	plan.Filings = len(filings)
	byAddr, byDomain := aggregatePlacementEvidence(filings)
	corrections, err := tx.placementCorrections(ctx, nil, time.Time{})
	if err != nil {
		return plan, err
	}
	own, err := tx.ownIdentityDomains(ctx)
	if err != nil {
		return plan, err
	}
	sd, err := tx.SenderDomainsFor(ctx)
	if err != nil {
		return plan, err
	}
	warn := senderwarn.NewIndex(sd.Own, sd.Corresponded)
	counts, err := tx.placementAreaCounts(ctx, nil)
	if err != nil {
		return plan, err
	}
	areaLabel, names, err := tx.areaOwnLabels(ctx)
	if err != nil {
		return plan, err
	}
	plan.AreaNames = names

	type pending struct {
		mv   PlacementBackfillMove
		cand PlacementCandidate
	}
	keyLine := map[placement.Key]*PlacementBackfillKey{}
	line := func(k placement.Key, area ulid.ULID) *PlacementBackfillKey {
		l := keyLine[k]
		if l == nil {
			l = &PlacementBackfillKey{Key: k, AreaID: area, NowIn: map[ulid.ULID]int{}, Skipped: map[placement.Reason]int{}}
			keyLine[k] = l
		}
		return l
	}
	skip := func(k placement.Key, area ulid.ULID, r placement.Reason) {
		plan.Abstain[r]++
		line(k, area).Skipped[r]++
	}

	// 1. THE SCAN, with ingest's rules: a message whose area a user, rule or
	// learned label (anything but the desk or day-one setup) already decides
	// is HELD and never moved; the rest go through placement.Decide on the
	// message's own key.
	args := []any{tx.userID.String()}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	joins, _, _ := areaJoinSQL(bind, nil)
	onDesk := deskMessageSQL(bind, tx.userID, nil)
	decided := bind([]string{LabelByDesk, LabelBySetup})
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id, COALESCE(m.from_address::text, ''), m.auth_fail, m.content_hash, `+areaResolvedSQL+`, `+onDesk+`,
		       (m.id IN (SELECT hml.message_id
		                   FROM app.message_labels hml
		                   JOIN app.area_feeds hf ON hf.user_id = hml.user_id AND hf.label_id = hml.label_id
		                  WHERE hml.user_id = $1 AND NOT (hml.applied_by = ANY(`+decided+`)))) IS TRUE,
		       (m.id IN (SELECT lp.message_id FROM app.learned_placements lp WHERE lp.user_id = $1)) IS TRUE,
		       m.received_at,
		       `+providerDatedExpr("m")+`
		  FROM app.messages m`+joins+`
		 WHERE m.user_id = $1 AND m.trashed_at IS NULL AND m.vanished_at IS NULL AND NOT m.is_draft
		 ORDER BY m.id`, args...)
	if err != nil {
		return plan, fmt.Errorf("store: placement backfill (scan): %w", err)
	}
	var queue []pending
	for rows.Next() {
		var (
			idStr, from          string
			authFail             bool
			hash                 []byte
			areaStr              *string
			desk, held, placedBy bool
			receivedAt           time.Time
			providerDated        bool
		)
		if err := rows.Scan(&idStr, &from, &authFail, &hash, &areaStr, &desk, &held, &placedBy, &receivedAt, &providerDated); err != nil {
			rows.Close()
			return plan, fmt.Errorf("store: placement backfill (scan row): %w", err)
		}
		plan.Scanned++
		if held {
			plan.Held++
			continue
		}
		addrKey, domainKey, ok := placement.KeysFor(from)
		if !ok {
			plan.Abstain[placement.NoKey]++
			continue
		}
		addrEv, domainEv := byAddr[addrKey.Value], byDomain[domainKey.Value]
		addrEv.LatestCorrection = corrections[addrKey]
		domainEv.LatestCorrection = corrections[domainKey]
		d := placement.Decide(addrKey, domainKey, addrEv, domainEv, own)
		if !d.Act {
			plan.Abstain[d.Reason]++
			continue
		}
		// THE BACKFILL IS DESK-ONLY: archived mail and other folders are not
		// where a misplacement costs the user anything, and the operator
		// approved the desk's moves.
		if !desk {
			plan.OffDesk++
			continue
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			rows.Close()
			return plan, fmt.Errorf("store: placement backfill: parse message id %q: %w", idStr, err)
		}
		var now ulid.ULID // zero: Needs sorting
		if areaStr != nil {
			if now, err = ulid.Parse(*areaStr); err != nil {
				rows.Close()
				return plan, fmt.Errorf("store: placement backfill: parse area id %q: %w", *areaStr, err)
			}
		}
		if now == d.Area {
			plan.NoOp++
			continue
		}
		if e, ex := excludedBy(excludes, addrKey, domainKey); ex {
			plan.Excluded[e]++
			skip(d.Key, d.Area, BackfillExcluded)
			continue
		}
		if placedBy {
			skip(d.Key, d.Area, BackfillPlacedBefore)
			continue
		}
		if g := counts[d.Area]; placement.Gate(placement.GateCounts{Kept: g.GateKept, Corrected: g.GateCorrected}).Stopped {
			skip(d.Key, d.Area, LearnedGated)
			continue
		}
		label, ok := areaLabel[d.Area]
		if !ok {
			rows.Close()
			return plan, fmt.Errorf("store: placement backfill: area %s has no own label", d.Area)
		}
		if !plan.Exact {
			// ⚠ placement-report's UPPER BOUND: the stored auth_fail (the
			// unconfigured floor) and the warnings that need no verdict
			// (selfSpoof suppressed, AuthPassed true). Exact mode replaces
			// both with the arrival-time checks on the real headers, below.
			if !(placement.SenderAuth{AuthFail: authFail}).Authenticated() {
				skip(d.Key, d.Area, placement.Unauthenticated)
				continue
			}
			if ws := warn.Check(senderwarn.Sender{Domain: domainKey.Value, AuthPassed: true}); len(ws) > 0 {
				skip(d.Key, d.Area, placement.SenderWarned)
				continue
			}
		}
		queue = append(queue, pending{
			mv:   PlacementBackfillMove{MessageID: id, Key: d.Key, AreaID: d.Area, LabelID: label, NowIn: now},
			cand: PlacementCandidate{MessageID: id, ContentHash: hash, From: from, ReceivedAt: TrustTime(receivedAt, providerDated)},
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return plan, fmt.Errorf("store: placement backfill (scan): %w", err)
	}

	// 2. EXACT MODE: the arrival-time checks on each candidate's own headers.
	if plan.Exact {
		kept := queue[:0]
		for _, p := range queue {
			s, err := opts.Senders(ctx, p.cand)
			if err != nil {
				if ctx.Err() != nil {
					return plan, ctx.Err()
				}
				skip(p.mv.Key, p.mv.AreaID, BackfillUnreadable)
				continue
			}
			r := senderRefusal(s, warn)
			if r == "" {
				// Tripwire: the sender was built from the stored From, so its
				// key IS the planned one. A difference means the identity that
				// was authenticated is not the one being acted on.
				if a, dom, ok := s.Keys(); !ok || (p.mv.Key.Kind == placement.KeyAddress && a != p.mv.Key) ||
					(p.mv.Key.Kind == placement.KeyDomain && dom != p.mv.Key) {
					r = placement.AmbiguousFrom
				}
			}
			if r != "" {
				skip(p.mv.Key, p.mv.AreaID, r)
				continue
			}
			kept = append(kept, p)
		}
		queue = kept
	}

	// 3. THE SWAP, from the area-filing plan Rule/apply uses, per learned area.
	byArea := map[ulid.ULID][]ulid.ULID{}
	for _, p := range queue {
		byArea[p.mv.AreaID] = append(byArea[p.mv.AreaID], p.mv.MessageID)
	}
	steps := map[ulid.ULID]AreaFilingStep{}
	for area, ids := range byArea {
		s, err := tx.PlanAreaFiling(ctx, AreaFilingTarget{AreaID: area, LabelID: areaLabel[area]}, ids)
		if err != nil {
			return plan, err
		}
		for id, st := range s {
			steps[id] = st
		}
	}
	for _, p := range queue {
		st, ok := steps[p.mv.MessageID]
		switch {
		case !ok:
			skip(p.mv.Key, p.mv.AreaID, BackfillGone)
			continue
		case st.Outcome == AreaFilingAlreadyThere:
			plan.NoOp++
			continue
		case st.Outcome != AreaFilingMove:
			skip(p.mv.Key, p.mv.AreaID, BackfillContested)
			continue
		}
		mv := p.mv
		mv.Remove = st.Remove
		plan.Moves = append(plan.Moves, mv)
		plan.Pairs[PlacementMove{From: mv.NowIn, To: mv.AreaID}]++
		l := line(mv.Key, mv.AreaID)
		l.Moves++
		l.NowIn[mv.NowIn]++
	}

	for _, l := range keyLine {
		plan.Keys = append(plan.Keys, *l)
	}
	sort.Slice(plan.Keys, func(i, j int) bool {
		a, b := plan.Keys[i], plan.Keys[j]
		if a.Moves != b.Moves {
			return a.Moves > b.Moves
		}
		if a.Key.Kind != b.Key.Kind {
			return a.Key.Kind < b.Key.Kind
		}
		return a.Key.Value < b.Key.Value
	})
	return plan, nil
}

// learnedPlacementEnabled reads the off switch. No desk_preferences row means
// the column default, which is ON (#714).
func (tx *UserTx) learnedPlacementEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT learned_placement_enabled FROM app.desk_preferences WHERE user_id = $1), true)`,
		tx.userID.String()).Scan(&enabled); err != nil {
		return false, fmt.Errorf("store: learned placement (switch): %w", err)
	}
	return enabled, nil
}

// areaOwnLabels maps each of the user's areas to its own label and its name.
// It reads app.areas directly rather than through ListAreas, which would
// materialise a fresh account's default plan (and its change rows): the
// planner writes nothing.
func (tx *UserTx) areaOwnLabels(ctx context.Context) (map[ulid.ULID]ulid.ULID, map[ulid.ULID]string, error) {
	rows, err := tx.tx.Query(ctx, `SELECT id, label_id, name FROM app.areas WHERE user_id = $1`, tx.userID.String())
	if err != nil {
		return nil, nil, fmt.Errorf("store: area own labels: %w", err)
	}
	defer rows.Close()
	labels, names := map[ulid.ULID]ulid.ULID{}, map[ulid.ULID]string{}
	for rows.Next() {
		var a, l, name string
		if err := rows.Scan(&a, &l, &name); err != nil {
			return nil, nil, fmt.Errorf("store: scan area own label: %w", err)
		}
		aid, err := ulid.Parse(a)
		if err != nil {
			return nil, nil, fmt.Errorf("store: area id %q: %w", a, err)
		}
		lid, err := ulid.Parse(l)
		if err != nil {
			return nil, nil, fmt.Errorf("store: label id %q: %w", l, err)
		}
		labels[aid], names[aid] = lid, name
	}
	return labels, names, rows.Err()
}

// backfillRemoved is one removed label in the journal, with everything an
// exact restore needs.
type backfillRemoved struct {
	LabelID   string    `json:"label"`
	AppliedBy string    `json:"applied_by"`
	CreatedAt time.Time `json:"created_at"`
}

// backfillJournal is the audit_log detail of one move: ids and attributions
// only, never a sender, subject or byte of content.
type backfillJournal struct {
	V       int               `json:"v"`
	Message string            `json:"message"`
	Area    string            `json:"area"`
	Label   string            `json:"label"`
	NowIn   string            `json:"now_in,omitempty"`
	KeyKind string            `json:"key_kind"`
	Removed []backfillRemoved `json:"removed"`
}

// PlacementBackfillApplied is what ApplyPlacementBackfill did.
type PlacementBackfillApplied struct {
	BatchID ulid.ULID
	Moved   int
	// Skipped counts planned moves not made, by reason (BackfillChanged,
	// BackfillGone, BackfillPlacedBefore).
	Skipped map[placement.Reason]int
	// PerKey is the moves actually written per key, for the report-equals-
	// apply comparison.
	PerKey map[placement.Key]int
}

// ApplyPlacementBackfill performs the plan's moves under a new batch id, ONE
// TRANSACTION PER MESSAGE: a failure leaves every earlier move committed and
// journalled (so --undo can reverse it) and no move half-made. It refuses
// outright when learned placement is switched off.
func (s *Store) ApplyPlacementBackfill(ctx context.Context, userID ulid.ULID, moves []PlacementBackfillMove) (PlacementBackfillApplied, error) {
	out := PlacementBackfillApplied{BatchID: NewULID(), Skipped: map[placement.Reason]int{}, PerKey: map[placement.Key]int{}}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		on, err := tx.learnedPlacementEnabled(ctx)
		if err == nil && !on {
			err = ErrLearnedPlacementOff
		}
		return err
	}); err != nil {
		return out, err
	}
	for _, mv := range moves {
		var reason placement.Reason
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			var e error
			reason, e = tx.applyPlacementBackfillMove(ctx, out.BatchID, mv)
			return e
		}); err != nil {
			return out, fmt.Errorf("store: placement backfill: message %s: %w", mv.MessageID, err)
		}
		if reason != "" {
			out.Skipped[reason]++
			continue
		}
		out.Moved++
		out.PerKey[mv.Key]++
	}
	return out, nil
}

// applyPlacementBackfillMove makes one planned move, after re-asking every
// question the plan answered against the state inside this transaction. ""
// means moved; otherwise the reason it was skipped.
func (tx *UserTx) applyPlacementBackfillMove(ctx context.Context, batchID ulid.ULID, mv PlacementBackfillMove) (placement.Reason, error) {
	on, err := tx.learnedPlacementEnabled(ctx)
	if err != nil {
		return "", err
	}
	if !on {
		return "", ErrLearnedPlacementOff
	}
	// Lock the message so nothing changes it between the checks and the write.
	var from string
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(from_address::text, '') FROM app.messages
		 WHERE user_id = $1 AND id = $2 AND trashed_at IS NULL AND vanished_at IS NULL
		 FOR UPDATE`, tx.userID.String(), mv.MessageID.String()).Scan(&from); err != nil {
		if isNoRows(err) {
			return BackfillGone, nil
		}
		return "", fmt.Errorf("store: placement backfill (lock): %w", err)
	}
	var placed bool
	if err := tx.tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM app.learned_placements WHERE user_id = $1 AND message_id = $2)`,
		tx.userID.String(), mv.MessageID.String()).Scan(&placed); err != nil {
		return "", fmt.Errorf("store: placement backfill (placed): %w", err)
	}
	if placed {
		return BackfillPlacedBefore, nil
	}
	// The key's decision, re-asked: a filing or correction since the plan
	// could have made it mixed or contradicted.
	addrKey, domainKey, ok := placement.KeysFor(from)
	if !ok {
		return BackfillChanged, nil
	}
	d, err := tx.decideLearned(ctx, addrKey, domainKey)
	if err != nil {
		return "", err
	}
	if !d.Act || d.Area != mv.AreaID || d.Key != mv.Key {
		return BackfillChanged, nil
	}
	// The resolved area and the swap, re-asked.
	res, err := tx.ResolveAreas(ctx, []ulid.ULID{mv.MessageID})
	if err != nil {
		return "", err
	}
	var now ulid.ULID
	if r := res[mv.MessageID]; r.AreaID != nil {
		now = *r.AreaID
	}
	if now != mv.NowIn {
		return BackfillChanged, nil
	}
	steps, err := tx.PlanAreaFiling(ctx, AreaFilingTarget{AreaID: mv.AreaID, LabelID: mv.LabelID}, []ulid.ULID{mv.MessageID})
	if err != nil {
		return "", err
	}
	st, ok := steps[mv.MessageID]
	if !ok || st.Outcome != AreaFilingMove || !sameRemoved(st.Remove, mv.Remove) {
		return BackfillChanged, nil
	}

	// NO WRITEBACK, by construction rather than by hope: an area's own label
	// is local-only (ADR-031). If one ever carried a keyword atom,
	// ApplyEmailUpdate would mirror the change to IMAP, so refuse instead.
	ids := []ulid.ULID{mv.LabelID}
	for _, r := range mv.Remove {
		ids = append(ids, r.LabelID)
	}
	var atoms int
	if err := tx.tx.QueryRow(ctx,
		`SELECT count(*) FROM app.label_keyword_map WHERE user_id = $1 AND label_id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids)).Scan(&atoms); err != nil {
		return "", fmt.Errorf("store: placement backfill (keyword atoms): %w", err)
	}
	if atoms > 0 {
		return "", fmt.Errorf("store: placement backfill: an area label of message %s maps to an IMAP keyword; refusing a move that would write back", mv.MessageID)
	}

	// The journal entry: every removed label with its attribution and
	// timestamp, read before the DELETE takes them.
	j := backfillJournal{V: 1, Message: mv.MessageID.String(), Area: mv.AreaID.String(),
		Label: mv.LabelID.String(), KeyKind: string(mv.Key.Kind), Removed: []backfillRemoved{}}
	if mv.NowIn != (ulid.ULID{}) {
		j.NowIn = mv.NowIn.String()
	}
	remove := make([]ulid.ULID, 0, len(mv.Remove))
	for _, r := range mv.Remove {
		var at time.Time
		if err := tx.tx.QueryRow(ctx, `
			SELECT created_at FROM app.message_labels
			 WHERE user_id = $1 AND message_id = $2 AND label_id = $3 AND applied_by = $4`,
			tx.userID.String(), mv.MessageID.String(), r.LabelID.String(), r.AppliedBy).Scan(&at); err != nil {
			if isNoRows(err) {
				return BackfillChanged, nil
			}
			return "", fmt.Errorf("store: placement backfill (read removed label): %w", err)
		}
		j.Removed = append(j.Removed, backfillRemoved{LabelID: r.LabelID.String(), AppliedBy: r.AppliedBy, CreatedAt: at})
		remove = append(remove, r.LabelID)
	}

	// THE MOVE: the one Email/set write, so one Email change_log row.
	found, _, err := tx.ApplyEmailUpdate(ctx, mv.MessageID, EmailUpdate{
		AddLabelIDs:    []ulid.ULID{mv.LabelID},
		LabelSource:    LearnedLabelSource(),
		RemoveLabelIDs: remove,
	})
	if err != nil {
		return "", err
	}
	if !found {
		return BackfillGone, nil
	}
	var by string
	if err := tx.tx.QueryRow(ctx, `
		SELECT applied_by FROM app.message_labels WHERE user_id = $1 AND message_id = $2 AND label_id = $3`,
		tx.userID.String(), mv.MessageID.String(), mv.LabelID.String()).Scan(&by); err != nil || by != LabelByLearned {
		// The plan said the target's own label was not on the message. A
		// placement logged over a label someone else wrote would be a lie.
		return "", fmt.Errorf("store: placement backfill: message %s: learned label not written (applied_by %q, %v)", mv.MessageID, by, err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.learned_placements (user_id, message_id, key_kind, key_value, area_id)
		VALUES ($1, $2, $3, $4, $5)`,
		tx.userID.String(), mv.MessageID.String(), string(mv.Key.Kind), mv.Key.Value, mv.AreaID.String()); err != nil {
		return "", fmt.Errorf("store: placement backfill (log): %w", err)
	}
	if err := tx.writeBackfillAudit(ctx, batchID, backfillActionMove, j); err != nil {
		return "", err
	}
	return "", nil
}

func sameRemoved(a, b []RemovedAreaLabel) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(r RemovedAreaLabel) string { return r.LabelID.String() + "/" + r.AppliedBy }
	set := map[string]int{}
	for _, r := range a {
		set[key(r)]++
	}
	for _, r := range b {
		set[key(r)]--
	}
	for _, n := range set {
		if n != 0 {
			return false
		}
	}
	return true
}

func (tx *UserTx) writeBackfillAudit(ctx context.Context, batchID ulid.ULID, action string, detail any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("store: placement backfill (journal): %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		tx.userID.String(), backfillActor, action, backfillObjectType, batchID.String(), raw); err != nil {
		return fmt.Errorf("store: placement backfill (journal): %w", err)
	}
	return nil
}

// backfillJournalRow is one journal row as read back.
type backfillJournalRow struct {
	Action string
	At     time.Time
	Detail backfillJournal
}

// readBackfillJournal reads the journal rows, for one batch or (zero id) all.
func (tx *UserTx) readBackfillJournal(ctx context.Context, batchID ulid.ULID) (map[string][]backfillJournalRow, error) {
	args := []any{tx.userID.String(), backfillObjectType}
	filter := ""
	if batchID != (ulid.ULID{}) {
		args = append(args, batchID.String())
		filter = ` AND object_id = $3`
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT object_id, action, created_at, detail FROM app.audit_log
		 WHERE user_id = $1 AND object_type = $2`+filter+`
		 ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: placement backfill (read journal): %w", err)
	}
	defer rows.Close()
	out := map[string][]backfillJournalRow{}
	for rows.Next() {
		var batch string
		var r backfillJournalRow
		var raw []byte
		if err := rows.Scan(&batch, &r.Action, &r.At, &raw); err != nil {
			return nil, fmt.Errorf("store: placement backfill (scan journal): %w", err)
		}
		if err := json.Unmarshal(raw, &r.Detail); err != nil {
			return nil, fmt.Errorf("store: placement backfill (decode journal of batch %s): %w", batch, err)
		}
		out[batch] = append(out[batch], r)
	}
	return out, rows.Err()
}

// PlacementBackfillBatch summarises one batch for the report.
type PlacementBackfillBatch struct {
	BatchID string
	At      time.Time
	Moved   int
	Undone  int
}

// PlacementBackfillBatches lists every batch, oldest first.
func (tx *UserTx) PlacementBackfillBatches(ctx context.Context) ([]PlacementBackfillBatch, error) {
	j, err := tx.readBackfillJournal(ctx, ulid.ULID{})
	if err != nil {
		return nil, err
	}
	var out []PlacementBackfillBatch
	for batch, rows := range j {
		b := PlacementBackfillBatch{BatchID: batch}
		for i, r := range rows {
			if i == 0 {
				b.At = r.At
			}
			switch r.Action {
			case backfillActionMove:
				b.Moved++
			case backfillActionUndo:
				b.Undone++
			}
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].BatchID < out[k].BatchID })
	return out, nil
}

// PlacementBackfillUndo is what UndoPlacementBackfill did.
type PlacementBackfillUndo struct {
	// Journal is how many moves the batch recorded; Outcomes partitions them.
	Journal  int
	Outcomes map[string]int
}

// ErrNoSuchBackfillBatch: --undo named a batch this user does not have.
var ErrNoSuchBackfillBatch = errors.New("store: no placement backfill batch with that id for this user")

// UndoPlacementBackfill reverses one batch exactly, ONE TRANSACTION PER
// MESSAGE: the learned label it added comes off, the labels it removed go
// back with their original attribution and timestamp, the placement log row
// is deleted (an undone placement was never a placement, so it neither
// counts for the gate nor blocks its key), and an undo row is appended to the
// journal. A message the user (or a rule) has filed since is SKIPPED and
// counted: undo never overrules a later decision.
func (s *Store) UndoPlacementBackfill(ctx context.Context, userID, batchID ulid.ULID) (PlacementBackfillUndo, error) {
	out := PlacementBackfillUndo{Outcomes: map[string]int{}}
	var entries []backfillJournal
	undone := map[string]bool{}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		j, err := tx.readBackfillJournal(ctx, batchID)
		if err != nil {
			return err
		}
		for _, r := range j[batchID.String()] {
			switch r.Action {
			case backfillActionMove:
				entries = append(entries, r.Detail)
			case backfillActionUndo:
				undone[r.Detail.Message] = true
			}
		}
		return nil
	}); err != nil {
		return out, err
	}
	if len(entries) == 0 {
		return out, ErrNoSuchBackfillBatch
	}
	out.Journal = len(entries)
	for _, e := range entries {
		if undone[e.Message] {
			out.Outcomes[BackfillAlreadyUndone]++
			continue
		}
		var outcome string
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			var err error
			outcome, err = tx.undoPlacementBackfillMove(ctx, batchID, e)
			return err
		}); err != nil {
			return out, fmt.Errorf("store: undo placement backfill: message %s: %w", e.Message, err)
		}
		out.Outcomes[outcome]++
	}
	return out, nil
}

func (tx *UserTx) undoPlacementBackfillMove(ctx context.Context, batchID ulid.ULID, e backfillJournal) (string, error) {
	msgID, err := ulid.Parse(e.Message)
	if err != nil {
		return "", fmt.Errorf("journal message id %q: %w", e.Message, err)
	}
	label, err := ulid.Parse(e.Label)
	if err != nil {
		return "", fmt.Errorf("journal label id %q: %w", e.Label, err)
	}
	var exists bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.messages WHERE user_id = $1 AND id = $2 FOR UPDATE)`,
		tx.userID.String(), msgID.String()).Scan(&exists); err != nil {
		return "", fmt.Errorf("store: undo placement backfill (lock): %w", err)
	}
	if !exists {
		return string(BackfillGone), nil
	}
	// RE-FILED SINCE? Every area-routing label on the message now, with who
	// put it there. The batch left exactly one non-automation routing label:
	// its own learned label. Anything else is a later decision.
	rows, err := tx.tx.Query(ctx, `
		SELECT ml.label_id, ml.applied_by
		  FROM app.message_labels ml
		  LEFT JOIN app.areas own ON own.user_id = ml.user_id AND own.label_id = ml.label_id
		  LEFT JOIN app.area_feeds f ON f.user_id = ml.user_id AND f.label_id = ml.label_id
		 WHERE ml.user_id = $1 AND ml.message_id = $2
		   AND (own.id IS NOT NULL OR f.area_id IS NOT NULL)`,
		tx.userID.String(), msgID.String())
	if err != nil {
		return "", fmt.Errorf("store: undo placement backfill (labels): %w", err)
	}
	learnedOn, laterDecision := false, false
	restoring := map[string]bool{}
	for _, r := range e.Removed {
		restoring[r.LabelID] = true
	}
	for rows.Next() {
		var lid, by string
		if err := rows.Scan(&lid, &by); err != nil {
			rows.Close()
			return "", fmt.Errorf("store: undo placement backfill (scan labels): %w", err)
		}
		switch {
		case lid == e.Label && by == LabelByLearned:
			learnedOn = true
		case restoring[lid]:
			// A label the undo would put back is already on again: someone
			// put it there after the batch.
			laterDecision = true
		case by != LabelByDesk && by != LabelBySetup:
			laterDecision = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("store: undo placement backfill (labels): %w", err)
	}
	var outcome string
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT outcome FROM app.learned_placements
		                  WHERE user_id = $1 AND message_id = $2 AND area_id = $3), '')`,
		tx.userID.String(), msgID.String(), e.Area).Scan(&outcome); err != nil {
		return "", fmt.Errorf("store: undo placement backfill (placement): %w", err)
	}
	if !learnedOn || laterDecision || outcome == PlacementCorrected {
		return BackfillRefiled, nil
	}

	// THE REVERSE MOVE, in one Email/set write: one Email change_log row.
	// RemovalIsUndo: reversing an operation is not a correction of it.
	up := EmailUpdate{RemoveLabelIDs: []ulid.ULID{label}, RemovalIsUndo: true}
	if len(e.Removed) > 0 {
		up.AddLabelSources = map[ulid.ULID]LabelSource{}
		for _, r := range e.Removed {
			lid, err := ulid.Parse(r.LabelID)
			if err != nil {
				return "", fmt.Errorf("journal removed label id %q: %w", r.LabelID, err)
			}
			if r.AppliedBy != LabelByDesk && r.AppliedBy != LabelBySetup {
				return "", fmt.Errorf("journal names a %q label to restore; the backfill only ever removes desk or setup labels", r.AppliedBy)
			}
			up.AddLabelIDs = append(up.AddLabelIDs, lid)
			up.AddLabelSources[lid] = LabelSource{Kind: r.AppliedBy}
		}
	}
	found, _, err := tx.ApplyEmailUpdate(ctx, msgID, up)
	if err != nil {
		return "", err
	}
	if !found {
		return string(BackfillGone), nil
	}
	// Exact: each restored row gets back the timestamp it had.
	for _, r := range e.Removed {
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.message_labels SET created_at = $4
			 WHERE user_id = $1 AND message_id = $2 AND label_id = $3`,
			tx.userID.String(), msgID.String(), r.LabelID, r.CreatedAt); err != nil {
			return "", fmt.Errorf("store: undo placement backfill (restore timestamp): %w", err)
		}
	}
	if _, err := tx.tx.Exec(ctx, `
		DELETE FROM app.learned_placements WHERE user_id = $1 AND message_id = $2 AND area_id = $3`,
		tx.userID.String(), msgID.String(), e.Area); err != nil {
		return "", fmt.Errorf("store: undo placement backfill (log): %w", err)
	}
	if err := tx.writeBackfillAudit(ctx, batchID, backfillActionUndo, map[string]any{"v": 1, "message": msgID.String()}); err != nil {
		return "", err
	}
	return BackfillUndone, nil
}

// PlacementOrigins counts placements by origin (arrival or backfill) and
// effective outcome, for placement-report.
type PlacementOrigins struct {
	Arrival, Backfill map[string]int
}

// placementOrigins splits the placement log by origin with the same
// backfilledMessagesSQL the gate's counts use, so the two cannot disagree
// about which placement is which.
func (tx *UserTx) placementOrigins(ctx context.Context) (PlacementOrigins, error) {
	out := PlacementOrigins{Arrival: map[string]int{}, Backfill: map[string]int{}}
	rows, err := tx.tx.Query(ctx, `
		SELECT bf.message_id IS NOT NULL, `+effectiveOutcomeSQL("$2")+`, count(*)
		  FROM app.learned_placements p
		  LEFT JOIN `+backfilledMessagesSQL("$1")+` bf ON bf.message_id = p.message_id
		 WHERE p.user_id = $1
		 GROUP BY 1, 2`, tx.userID.String(), keptAfterSeconds())
	if err != nil {
		return out, fmt.Errorf("store: placement origins: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var back bool
		var outcome string
		var n int
		if err := rows.Scan(&back, &outcome, &n); err != nil {
			return out, fmt.Errorf("store: scan placement origin: %w", err)
		}
		if back {
			out.Backfill[outcome] += n
		} else {
			out.Arrival[outcome] += n
		}
	}
	return out, rows.Err()
}
