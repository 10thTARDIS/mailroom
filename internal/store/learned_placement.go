// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/senderwarn"
)

// LEARNED PLACEMENT (#714, ADR-032, migration 00060): Mailroom files a new
// message into the area the user has consistently filed its sender into.
//
// The decision itself is internal/placement's (pure, unit-tested). This file
// gathers its evidence, applies its verdict through the one area-label write
// path (insertAreaOwnLabel, shared with the desk), logs every placement, and
// records what happened to it afterwards:
//
//	evidence     app.message_labels rows for an area's OWN label with
//	             applied_by = 'user' (Re-file, the label menu, the sender card,
//	             drag and drop all write those). Nothing else, ever.
//	placement    the area's own label with applied_by = 'learned', plus an
//	             app.learned_placements row, in one savepoint at ingest
//	correction   the user takes a learned label off (ApplyEmailUpdate):
//	             outcome 'corrected', which blocks the key (placement.Judge)
//	kept         KeptAfter without a correction, or archived where it was put
//	withdrawn    an API KEY took the learned label off (#49): counted as
//	             neither kept nor corrected, because a key's move is not the
//	             user's verdict; a key's archive does not keep it either
//	gate         per area, Wilson lower bound on kept/(kept+corrected) since
//	             the area's last reset (placement.Gate)
//
// NO MATERIALISED MODEL. The evidence is read at each decision: on the
// a real mailbox (#714) the user's own area filings are a small set of rows, which
// the partial index message_labels_user_filings reaches directly, and the
// query narrows them to one domain before they leave Postgres. A table of
// learned keys would be a second copy of those rows to keep in step with
// every Re-file and every undo, for a read that is already small.

// Learned-placement outcomes beyond placement.Reason, for the ingest result.
const (
	// LearnedOff: the user switched learned placement off.
	LearnedOff placement.Reason = "off"
	// LearnedAlreadyPlaced: a label already decides the message's area (a
	// rule's at ingest), and learned placement never overrides one.
	LearnedAlreadyPlaced placement.Reason = "placed"
	// LearnedAgreesWithDesk: the tab verdict already puts the message in the
	// learned area. Nothing is placed or logged; the desk's own label goes on
	// as it always did, and the gate is not padded with placements that could
	// not have been wrong in a way learning caused.
	LearnedAgreesWithDesk placement.Reason = "agrees"
	// LearnedGated: the key is confident, but the area's correction gate has
	// stopped it.
	LearnedGated placement.Reason = "gated"
)

// Placement log outcomes (app.learned_placements.outcome).
const (
	PlacementOpen      = "open"
	PlacementKept      = "kept"
	PlacementCorrected = "corrected"
	// PlacementWithdrawn: an API KEY took the learned label off before anyone
	// judged the placement (#49, migration 00076). Neither kept nor
	// corrected — a key's move is not the user's verdict either way — so the
	// gate and the corrections ignore it, and it never ages into kept.
	PlacementWithdrawn = "withdrawn"
)

// LearnedPlacementResult says what learned placement did with one message.
type LearnedPlacementResult struct {
	Placed bool
	// Reason is placement.Confident when Placed, otherwise why not.
	Reason placement.Reason
	Key    placement.Key
	// AreaID is the learned area when a key was confident (placed, gated or
	// agreeing with the desk).
	AreaID *ulid.ULID
	// Gate is the area's gate as evaluated, when it was evaluated.
	Gate *placement.GateStatus
}

// LearnedPlacementForNewMessage is the INGEST entry: decide, and when the
// decision is to act, file msgID into the learned area.
//
// It must run AFTER rules and BEFORE the desk's own area label
// (internal/ingest/ingest.go): a rule's area label is already on the message
// and wins, and once the learned label is on, the message resolves by label,
// so applyAreaLabel adds no desk label, the same way a rule's label already
// beats the desk's.
//
// ALL OR NOTHING, AND NEVER FATAL TO THE INGEST: everything, the reads
// included, runs inside one savepoint, so a failure anywhere leaves neither
// a label without its log row nor an aborted ingest transaction; the caller
// logs and swallows the error.
//
// CHANGE_LOG: no row of its own. The label rides the Email `created` row the
// ingest transaction appends right after, exactly as
// ApplyAreaLabelForNewMessage's does, and the placement log is not a JMAP
// object. ⚠ Calling this outside an ingest transaction that will append that
// row would break the load-bearing invariant (CLAUDE.md).
//
// AUTHENTICATION (#714 security review): the key is the From address, which
// is forgeable, and a placement is shown as "learned from your moves". So a
// confident key acts only when auth.Authenticated() holds (a trusted DMARC
// pass when the operator has named trusted authserv-ids, else "did not
// clearly fail") AND the sender warnings would not flag the message. A
// message that fails either gets the desk's normal placement. The EVIDENCE is
// unaffected: the user's own filings are their decisions whatever the mail
// they filed claimed to be.
//
// THE SENDER IS ONE VALUE (#714 security review): placement.Sender, built
// once at ingest. Its keys decide, its domain is the one DMARC was bound to
// and the one the warnings check, and its key is what the log records. The
// message's stored from_address is deliberately not re-read here.
func (tx *UserTx) LearnedPlacementForNewMessage(ctx context.Context, msgID ulid.ULID, sender placement.Sender) (LearnedPlacementResult, error) {
	var out LearnedPlacementResult
	err := tx.inSavepoint(ctx, func() error {
		var e error
		out, e = tx.learnedPlacement(ctx, msgID, sender)
		return e
	})
	if err != nil {
		return LearnedPlacementResult{}, err
	}
	return out, nil
}

func (tx *UserTx) learnedPlacement(ctx context.Context, msgID ulid.ULID, sender placement.Sender) (LearnedPlacementResult, error) {
	// STEPS 1-4b are learnedArrival, the one read-only decision that
	// `mailroom-admin placement-explain` re-runs (ExplainLearnedPlacement), so
	// what the operator is shown is what this path decided.
	tr, err := tx.learnedArrival(ctx, msgID, sender, arrivalOpts{})
	if err != nil {
		return LearnedPlacementResult{}, err
	}
	if !tr.Continue {
		return tr.Result, nil
	}
	out := tr.Result
	area := *out.AreaID

	// 5. THE GATE. Age open placements first so the stored outcomes say what
	// the gate is about to count (the gate's own query would count them as
	// kept either way: effectiveOutcomeSQL).
	g, err := tx.learnedGate(ctx, area, true)
	if err != nil {
		return LearnedPlacementResult{}, err
	}
	out.Gate = &g
	if g.Stopped {
		out.Reason = LearnedGated
		return out, nil
	}

	// 6. ACT: the area's own label, attributed 'learned', through the desk's
	// own write, and its log row.
	applied, err := tx.insertAreaOwnLabel(ctx, msgID, area, LearnedLabelSource())
	if err != nil {
		return LearnedPlacementResult{}, err
	}
	if !applied {
		// The own label was already on the message, which step 3 rules out for
		// a label that feeds an area. Refuse loudly rather than log a placement
		// that wrote nothing (CLAUDE.md: "ran without erroring while doing
		// nothing").
		return LearnedPlacementResult{}, fmt.Errorf("store: learned placement: area %s label already on message %s", area, msgID)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.learned_placements (user_id, message_id, key_kind, key_value, area_id)
		VALUES ($1, $2, $3, $4, $5)`,
		tx.userID.String(), msgID.String(), string(out.Key.Kind), out.Key.Value, area.String()); err != nil {
		return LearnedPlacementResult{}, fmt.Errorf("store: learned placement (log): %w", err)
	}
	out.Placed = true
	return out, nil
}

// learnedGate evaluates the correction gate for area. resolveAged writes
// 'kept' onto aged open placements first (the live path); the explain path,
// whose transaction is read-only, skips that write, which changes nothing the
// gate counts because effectiveOutcomeSQL already reads an aged open
// placement as kept.
func (tx *UserTx) learnedGate(ctx context.Context, area ulid.ULID, resolveAged bool) (placement.GateStatus, error) {
	if resolveAged {
		if err := tx.resolveAgedPlacements(ctx); err != nil {
			return placement.GateStatus{}, err
		}
	}
	counts, err := tx.placementAreaCounts(ctx, &area)
	if err != nil {
		return placement.GateStatus{}, err
	}
	c := counts[area]
	return placement.Gate(placement.GateCounts{Kept: c.GateKept, Corrected: c.GateCorrected}), nil
}

// decideLearned is placement.Decide on one message's keys, over the evidence
// query narrowed to the key's domain: THE decision ingest makes, and the one
// the backfill re-asks at apply time (placement_backfill.go), so the two
// cannot drift.
func (tx *UserTx) decideLearned(ctx context.Context, addrKey, domainKey placement.Key) (placement.Decision, error) {
	d, _, err := tx.decideLearnedEvidence(ctx, addrKey, domainKey, time.Time{})
	return d, err
}

// LearnedEvidence is what decideLearned read, kept so the explain path can
// show it.
type LearnedEvidence struct {
	Address, Domain placement.Evidence
	// AddressVerdict and DomainVerdict are placement.Judge on each key alone
	// (display only; the decision is Decide's).
	AddressVerdict, DomainVerdict placement.Verdict
	// DomainFallbackBlocked: the domain is freemail or one of the user's own.
	DomainFallbackBlocked bool
}

// decideLearnedEvidence is decideLearned's body. asOf, when not zero, counts
// only filings and corrections created before it (explain's --as-of); ingest
// and the backfill pass the zero time.
func (tx *UserTx) decideLearnedEvidence(ctx context.Context, addrKey, domainKey placement.Key, asOf time.Time) (placement.Decision, LearnedEvidence, error) {
	filings, err := tx.placementFilings(ctx, domainKey.Value, asOf)
	if err != nil {
		return placement.Decision{}, LearnedEvidence{}, err
	}
	byAddr, byDomain := aggregatePlacementEvidence(filings)
	corrections, err := tx.placementCorrections(ctx, []placement.Key{addrKey, domainKey}, asOf)
	if err != nil {
		return placement.Decision{}, LearnedEvidence{}, err
	}
	addrEv, domainEv := byAddr[addrKey.Value], byDomain[domainKey.Value]
	addrEv.LatestCorrection = corrections[addrKey]
	domainEv.LatestCorrection = corrections[domainKey]
	own, err := tx.ownIdentityDomains(ctx)
	if err != nil {
		return placement.Decision{}, LearnedEvidence{}, err
	}
	ev := LearnedEvidence{
		Address: addrEv, Domain: domainEv,
		AddressVerdict:        placement.Judge(placement.KeyAddress, addrEv),
		DomainVerdict:         placement.Judge(placement.KeyDomain, domainEv),
		DomainFallbackBlocked: placement.DomainFallbackBlocked(domainKey.Value, own),
	}
	return placement.Decide(addrKey, domainKey, addrEv, domainEv, own), ev, nil
}

// SenderChecks is every check senderRefusal makes, with its inputs. Refusal is
// the FIRST that refuses ("" = none), in the order below.
type SenderChecks struct {
	// Ambiguous: not exactly one mailbox in exactly one From header.
	Ambiguous bool
	// Auth is the sender's authentication as ingest evaluated it, and
	// Authenticated is Auth.Authenticated(): the condition acted on.
	Auth          placement.SenderAuth
	Authenticated bool
	// Warnings are the sender spoofing / look-alike warnings.
	Warnings []senderwarn.Warning
	Refusal  placement.Reason
}

// senderChecks evaluates every check (no short circuit, so the explain path
// can show them all); Refusal is the first that refuses, in senderRefusal's
// documented order.
func senderChecks(sender placement.Sender, warn *senderwarn.Index) SenderChecks {
	auth := sender.Auth()
	c := SenderChecks{Ambiguous: sender.Ambiguous(), Auth: auth, Authenticated: auth.Authenticated()}
	_, domainKey, _ := sender.Keys()
	c.Warnings = warn.Check(senderwarn.Sender{Domain: domainKey.Value, AuthPassed: auth.AffirmativePass})
	switch {
	case c.Ambiguous:
		c.Refusal = placement.AmbiguousFrom
	case !c.Authenticated:
		c.Refusal = placement.Unauthenticated
	case len(c.Warnings) > 0:
		c.Refusal = placement.SenderWarned
	}
	return c
}

// senderRefusal is THE arrival-time check on the sender itself (#714
// security review), asked by ingest (learnedArrival) and by the backfill
// (placement_backfill.go) through this one function. It returns "" when the
// sender may be acted on, else the reason it may not:
//
//   - AmbiguousFrom: not exactly one mailbox in exactly one From header, or a
//     parse that disagrees with the stored From (placement.NewSender).
//   - Unauthenticated: SenderAuth.Authenticated() is false (a trusted DMARC
//     pass bound to the From domain when authserv-ids are configured, else
//     "did not clearly fail").
//   - SenderWarned: the spoofing / look-alike warnings would flag it, asked
//     exactly as the reading pane asks them (internal/api/sender_warnings.go):
//     the same index, the same trusted affirmative pass suppressing selfSpoof.
func senderRefusal(sender placement.Sender, warn *senderwarn.Index) placement.Reason {
	return senderChecks(sender, warn).Refusal
}

// placementFiling is one user filing into an area: the evidence unit.
type placementFiling struct {
	AreaID ulid.ULID
	From   string
	At     time.Time
}

// placementFilings reads the user's own area filings: app.message_labels rows
// for an area's OWN label with applied_by = 'user', on mail that is not in
// Trash. domain, when not empty, narrows them to From addresses ending in
// "@domain" (a superset filter; aggregatePlacementEvidence applies
// placement.KeysFor, the exact rule, to whatever comes back).
//
// ⚠ applied_by = 'user' IS THE WHOLE SAFETY OF THIS FEATURE. A 'learned' row
// counted here would make every placement its own evidence (#169), and a
// 'desk', 'setup' or 'rule' row is an automation's verdict, not the user's.
// TestLearnedPlacementNeverLearnsFromItsOwnOutput holds it.
//
// Trashed mail is left out so evidence does not change on the day Trash is
// purged; #714's measurement counted the same population.
//
// A message with no From is stored with from_address NULL (ingest's
// nullStr): read as "", it has no key and aggregatePlacementEvidence drops
// it. Scanned bare, one such filing made the unnarrowed read (the report's)
// fail outright (found by #49's tests).
func (tx *UserTx) placementFilings(ctx context.Context, domain string, asOf time.Time) ([]placementFiling, error) {
	args := []any{tx.userID.String(), LabelByUser}
	narrow := ""
	if domain != "" {
		suffix := "@" + domain
		args = append(args, suffix, len(suffix))
		narrow = ` AND right(lower(btrim(m.from_address)), $4) = $3`
	}
	if !asOf.IsZero() {
		args = append(args, asOf)
		narrow += ` AND ml.created_at < $` + strconv.Itoa(len(args))
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT a.id, COALESCE(m.from_address::text, ''), ml.created_at
		  FROM app.message_labels ml
		  JOIN app.areas a ON a.user_id = ml.user_id AND a.label_id = ml.label_id
		  JOIN app.messages m ON m.user_id = ml.user_id AND m.id = ml.message_id
		 WHERE ml.user_id = $1 AND ml.applied_by = $2
		   AND m.trashed_at IS NULL`+narrow, args...)
	if err != nil {
		return nil, fmt.Errorf("store: learned placement (evidence): %w", err)
	}
	defer rows.Close()
	var out []placementFiling
	for rows.Next() {
		var areaStr string
		var f placementFiling
		if err := rows.Scan(&areaStr, &f.From, &f.At); err != nil {
			return nil, fmt.Errorf("store: learned placement (scan evidence): %w", err)
		}
		id, err := ulid.Parse(areaStr)
		if err != nil {
			return nil, fmt.Errorf("store: learned placement: parse area id %q: %w", areaStr, err)
		}
		f.AreaID = id
		out = append(out, f)
	}
	return out, rows.Err()
}

// aggregatePlacementEvidence folds filings into per-address and per-domain
// evidence, keyed by placement.Key.Value. Filings whose From has no usable key
// are dropped, exactly as a new message from such an address gets no key.
func aggregatePlacementEvidence(filings []placementFiling) (byAddr, byDomain map[string]placement.Evidence) {
	byAddr = map[string]placement.Evidence{}
	byDomain = map[string]placement.Evidence{}
	domainAddrs := map[string]map[string]bool{}
	add := func(m map[string]placement.Evidence, key string, f placementFiling) {
		e := m[key]
		if e.Filings == nil {
			e.Filings = map[ulid.ULID]int{}
		}
		e.Filings[f.AreaID]++
		if f.At.After(e.LatestFiling) {
			e.LatestFiling = f.At
		}
		m[key] = e
	}
	for _, f := range filings {
		a, d, ok := placement.KeysFor(f.From)
		if !ok {
			continue
		}
		add(byAddr, a.Value, f)
		add(byDomain, d.Value, f)
		if domainAddrs[d.Value] == nil {
			domainAddrs[d.Value] = map[string]bool{}
		}
		domainAddrs[d.Value][a.Value] = true
	}
	for k, e := range byAddr {
		e.Addresses = 1
		byAddr[k] = e
	}
	for k, e := range byDomain {
		e.Addresses = len(domainAddrs[k])
		byDomain[k] = e
	}
	return byAddr, byDomain
}

// placementCorrections returns the newest correction time per key, for the
// keys named (nil = every key with a correction).
func (tx *UserTx) placementCorrections(ctx context.Context, keys []placement.Key, asOf time.Time) (map[placement.Key]time.Time, error) {
	args := []any{tx.userID.String()}
	filter := ""
	if keys != nil {
		var ors []string
		for _, k := range keys {
			args = append(args, string(k.Kind), k.Value)
			ors = append(ors, `(key_kind = $`+strconv.Itoa(len(args)-1)+` AND key_value = $`+strconv.Itoa(len(args))+`)`)
		}
		if len(ors) == 0 {
			return map[placement.Key]time.Time{}, nil
		}
		filter = ` AND (` + strings.Join(ors, " OR ") + `)`
	}
	if !asOf.IsZero() {
		args = append(args, asOf)
		filter += ` AND corrected_at < $` + strconv.Itoa(len(args))
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT key_kind, key_value, max(corrected_at)
		  FROM app.learned_placements
		 WHERE user_id = $1 AND outcome = 'corrected'`+filter+`
		 GROUP BY key_kind, key_value`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: learned placement (corrections): %w", err)
	}
	defer rows.Close()
	out := map[placement.Key]time.Time{}
	for rows.Next() {
		var kind, value string
		var at time.Time
		if err := rows.Scan(&kind, &value, &at); err != nil {
			return nil, fmt.Errorf("store: learned placement (scan corrections): %w", err)
		}
		out[placement.Key{Kind: placement.KeyKind(kind), Value: value}] = at
	}
	return out, rows.Err()
}

// keptAfterSeconds is placement.KeptAfter as the make_interval argument.
func keptAfterSeconds() float64 { return placement.KeptAfter.Seconds() }

// effectiveOutcomeSQL is a placement's outcome as of now, for alias p: an open
// placement older than placement.KeptAfter is kept, whether or not
// resolveAgedPlacements has written that down yet. agePH binds
// keptAfterSeconds(). THE ONE DEFINITION, read by the gate at ingest and by
// the report, so the two cannot disagree about what has been kept.
func effectiveOutcomeSQL(agePH string) string {
	return `(CASE WHEN p.outcome = 'open' AND p.decided_at <= now() - make_interval(secs => ` + agePH + `)
	              THEN 'kept' ELSE p.outcome END)`
}

// resolveAgedPlacements writes 'kept' (by age) onto open placements older than
// placement.KeptAfter. A placement log row is not a JMAP object, so this
// appends no change_log row.
func (tx *UserTx) resolveAgedPlacements(ctx context.Context) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.learned_placements
		   SET outcome = 'kept', kept_at = now(), kept_by = 'age'
		 WHERE user_id = $1 AND outcome = 'open'
		   AND decided_at <= now() - make_interval(secs => $2)`,
		tx.userID.String(), keptAfterSeconds()); err != nil {
		return fmt.Errorf("store: learned placement (age): %w", err)
	}
	return nil
}

// PlacementAreaCounts is one area's placement log, by outcome as of now.
//
// ARRIVAL AND BACKFILL APART (#714 increment 2a). Open/Kept/Corrected and the
// gate's counts are ARRIVAL placements only: the ones learned placement made
// on new mail at ingest. Backfilled placements (placement_backfill.go) are on
// old mail the user mostly never revisits, so they age into "kept" without
// anyone having looked; counted by the gate they would lift every area's
// Wilson bound and mute the corrections of new mail, a metric that does not
// match the harm (CLAUDE.md). They are reported in Backfill*, and NOT gated.
// A correction of a backfilled message still contradicts its KEY
// (placementCorrections reads every origin).
type PlacementAreaCounts struct {
	AreaID ulid.ULID
	// Arrival placements, all time.
	Open, Kept, Corrected int
	// Withdrawn: placements an API key took the label off (#49), arrival
	// and backfill together. Never gated; reported so that a key re-filing
	// learned mail is visible rather than a silent shrink of the other counts.
	Withdrawn int
	// Arrival placements since the area's gate reset (equal to the above with
	// no reset): what the correction gate counts.
	GateKept, GateCorrected int
	ResetAt                 *time.Time
	// Backfilled placements, all time: informational, never gated.
	BackfillOpen, BackfillKept, BackfillCorrected int
}

// backfilledMessagesSQL is the set of message ids any placement backfill
// batch moved, as a derived table with one column, message_id: THE
// definition of "a backfill placement", read by the gate's counts and the
// report's origin split. userPH binds the caller's user id. It reads the
// backfill journal in app.audit_log (placement_backfill.go): one DISTINCT
// pass the planner hashes and anti-joins against the placement log, never a
// per-row lookup.
//
// ⚠ INDEX: app.audit_log has no index leading with user_id (only its
// identity primary key), so this pass is a scan of the table filtered by the
// RLS predicate. That is the audit log's size, not the mailbox's, and it is
// read only where the gate is evaluated (a confident, authenticated arrival
// that would move) and by the report. If it ever shows, the index is
// (user_id, object_type, action) — a migration, deliberately not added here.
func backfilledMessagesSQL(userPH string) string {
	return `(SELECT DISTINCT bfa.detail->>'message' AS message_id
	           FROM app.audit_log bfa
	          WHERE bfa.user_id = ` + userPH + `
	            AND bfa.object_type = '` + backfillObjectType + `'
	            AND bfa.action = '` + backfillActionMove + `')`
}

// placementAreaCounts reads the placement log's per-area counts (one area when
// areaID is set).
func (tx *UserTx) placementAreaCounts(ctx context.Context, areaID *ulid.ULID) (map[ulid.ULID]PlacementAreaCounts, error) {
	args := []any{tx.userID.String(), keptAfterSeconds()}
	filter := ""
	if areaID != nil {
		args = append(args, areaID.String())
		filter = ` AND p.area_id = $3`
	}
	eff := effectiveOutcomeSQL("$2")
	since := `(r.reset_at IS NULL OR p.decided_at > r.reset_at)`
	arrival := `bf.message_id IS NULL`
	backfill := `bf.message_id IS NOT NULL`
	rows, err := tx.tx.Query(ctx, `
		SELECT p.area_id,
		       count(*) FILTER (WHERE `+arrival+` AND `+eff+` = 'open'),
		       count(*) FILTER (WHERE `+arrival+` AND `+eff+` = 'kept'),
		       count(*) FILTER (WHERE `+arrival+` AND `+eff+` = 'corrected'),
		       count(*) FILTER (WHERE `+arrival+` AND `+eff+` = 'kept' AND `+since+`),
		       count(*) FILTER (WHERE `+arrival+` AND `+eff+` = 'corrected' AND `+since+`),
		       max(r.reset_at),
		       count(*) FILTER (WHERE `+backfill+` AND `+eff+` = 'open'),
		       count(*) FILTER (WHERE `+backfill+` AND `+eff+` = 'kept'),
		       count(*) FILTER (WHERE `+backfill+` AND `+eff+` = 'corrected'),
		       count(*) FILTER (WHERE p.outcome = 'withdrawn')
		  FROM app.learned_placements p
		  LEFT JOIN app.learned_placement_gate_resets r
		         ON r.user_id = p.user_id AND r.area_id = p.area_id
		  LEFT JOIN `+backfilledMessagesSQL("$1")+` bf
		         ON bf.message_id = p.message_id
		 WHERE p.user_id = $1`+filter+`
		 GROUP BY p.area_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: learned placement (counts): %w", err)
	}
	defer rows.Close()
	out := map[ulid.ULID]PlacementAreaCounts{}
	for rows.Next() {
		var areaStr string
		var c PlacementAreaCounts
		if err := rows.Scan(&areaStr, &c.Open, &c.Kept, &c.Corrected, &c.GateKept, &c.GateCorrected, &c.ResetAt,
			&c.BackfillOpen, &c.BackfillKept, &c.BackfillCorrected, &c.Withdrawn); err != nil {
			return nil, fmt.Errorf("store: learned placement (scan counts): %w", err)
		}
		id, err := ulid.Parse(areaStr)
		if err != nil {
			return nil, fmt.Errorf("store: learned placement: parse area id %q: %w", areaStr, err)
		}
		c.AreaID = id
		out[id] = c
	}
	return out, rows.Err()
}

// recordLearnedCorrection marks msgID's learned placement CORRECTED: the user
// took the learned label off (ApplyEmailUpdate). Only an OPEN placement can
// be corrected; one already kept (aged, or archived where it was put) stays
// kept, because that outcome has already been counted. The placement log is
// not a JMAP object, so no change_log row: the Email update row the caller
// appends covers the label change itself.
func (tx *UserTx) recordLearnedCorrection(ctx context.Context, msgID ulid.ULID) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.learned_placements
		   SET outcome = 'corrected', corrected_at = now()
		 WHERE user_id = $1 AND message_id = $2 AND outcome = 'open'
		   AND decided_at > now() - make_interval(secs => $3)`,
		tx.userID.String(), msgID.String(), keptAfterSeconds()); err != nil {
		return fmt.Errorf("store: learned placement (correction): %w", err)
	}
	return nil
}

// recordLearnedWithdrawal marks msgID's open learned placement WITHDRAWN: an
// API key took the learned label off (ApplyEmailUpdate, #49). The same
// window as a correction: only an OPEN placement younger than KeptAfter, so
// one already kept (or effectively kept by age) keeps the outcome it has
// already been counted with. No change_log row, as for a correction.
func (tx *UserTx) recordLearnedWithdrawal(ctx context.Context, msgID ulid.ULID) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.learned_placements
		   SET outcome = 'withdrawn', withdrawn_at = now()
		 WHERE user_id = $1 AND message_id = $2 AND outcome = 'open'
		   AND decided_at > now() - make_interval(secs => $3)`,
		tx.userID.String(), msgID.String(), keptAfterSeconds()); err != nil {
		return fmt.Errorf("store: learned placement (withdrawal): %w", err)
	}
	return nil
}

// recordLearnedCorrectionOfAgentLabel marks msgID's learned placement
// CORRECTED when the user took off labelID, an 'agent' row that stands where
// the learned label stood: an API key removed the learned label (withdrawing
// the placement) and then re-added it (#49 K2 review). Only when labelID is
// the placement's own area's label, and only for a placement still OPEN or
// WITHDRAWN inside the same KeptAfter window recordLearnedCorrection uses, so
// the user's removal counts exactly as it would have on the learned row. A
// withdrawn placement's withdrawn_at is cleared (the stamped CHECKs: one
// outcome, one stamp). No change_log row, as for a correction.
func (tx *UserTx) recordLearnedCorrectionOfAgentLabel(ctx context.Context, msgID, labelID ulid.ULID) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.learned_placements p
		   SET outcome = 'corrected', corrected_at = now(), withdrawn_at = NULL
		  FROM app.areas a
		 WHERE p.user_id = $1 AND p.message_id = $2
		   AND p.outcome IN ('open', 'withdrawn')
		   AND p.decided_at > now() - make_interval(secs => $3)
		   AND a.user_id = p.user_id AND a.id = p.area_id AND a.label_id = $4`,
		tx.userID.String(), msgID.String(), keptAfterSeconds(), labelID.String()); err != nil {
		return fmt.Errorf("store: learned placement (correction of a key's re-add): %w", err)
	}
	return nil
}

// UserFilingProtectedError refuses an API key's removal of an area's own
// label that the user applied by hand (#49): LabelIDs are the protected
// labels the update named.
type UserFilingProtectedError struct{ LabelIDs []ulid.ULID }

func (e *UserFilingProtectedError) Error() string {
	return fmt.Sprintf("store: an API key cannot take off %d area filing(s) the user made by hand", len(e.LabelIDs))
}

// userFiledAreaLabels returns which of labelIDs are, on msgID, an area's OWN
// label applied by the user: exactly the rows placementFilings counts as
// evidence (trashed or not — the row is what a key would delete).
func (tx *UserTx) userFiledAreaLabels(ctx context.Context, msgID ulid.ULID, labelIDs []ulid.ULID) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT ml.label_id
		  FROM app.message_labels ml
		  JOIN app.areas a ON a.user_id = ml.user_id AND a.label_id = ml.label_id
		 WHERE ml.user_id = $1 AND ml.message_id = $2 AND ml.label_id = ANY($3)
		   AND ml.applied_by = $4
		 ORDER BY ml.label_id`,
		tx.userID.String(), msgID.String(), ulidStrings(labelIDs), LabelByUser)
	if err != nil {
		return nil, fmt.Errorf("store: user-filed area labels: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: scan user-filed area label: %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("store: user-filed area label id %q: %w", s, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// keepLearnedOnArchive marks msgID's open learned placement KEPT because the
// user archived the message from the area it was placed in: they acted on it
// there. Only while the learned label is still on the message.
func (tx *UserTx) keepLearnedOnArchive(ctx context.Context, msgID ulid.ULID) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.learned_placements p
		   SET outcome = 'kept', kept_at = now(), kept_by = 'archive'
		 WHERE p.user_id = $1 AND p.message_id = $2 AND p.outcome = 'open'
		   AND EXISTS (SELECT 1 FROM app.message_labels ml
		                 JOIN app.areas a ON a.user_id = ml.user_id AND a.label_id = ml.label_id
		                WHERE ml.user_id = p.user_id AND ml.message_id = p.message_id
		                  AND a.id = p.area_id AND ml.applied_by = $3)`,
		tx.userID.String(), msgID.String(), LabelByLearned); err != nil {
		return fmt.Errorf("store: learned placement (kept on archive): %w", err)
	}
	return nil
}

// ResetPlacementGate records an operator re-enable for one area's correction
// gate: from now on the gate counts only placements decided after this
// moment. Every outcome already recorded stays as it is; the report keeps
// showing them. Returns false when the area does not exist.
func (tx *UserTx) ResetPlacementGate(ctx context.Context, areaID ulid.ULID) (bool, error) {
	var found bool
	if err := tx.tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM app.areas WHERE user_id = $1 AND id = $2)`,
		tx.userID.String(), areaID.String()).Scan(&found); err != nil {
		return false, fmt.Errorf("store: reset placement gate (area): %w", err)
	}
	if !found {
		return false, nil
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.learned_placement_gate_resets (user_id, area_id, reset_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id, area_id) DO UPDATE SET reset_at = EXCLUDED.reset_at`,
		tx.userID.String(), areaID.String()); err != nil {
		return false, fmt.Errorf("store: reset placement gate: %w", err)
	}
	return true, nil
}
