// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/senderwarn"
)

// WHAT LEARNED SORTING KNOWS ABOUT A SENDER (#167): LearnedSender/get, forget
// and trust, and LearnedSorting/status. READ-ONLY views over the decision and the
// evidence the ingest path uses, never a second definition of them:
//
//   - LearnedSenderView asks decideLearnedEvidence (the one decision ingest, the
//     backfill and placement-explain ask) and learnedGate (the area's correction
//     gate), so "acting" is exactly "would place this sender's next
//     authenticated message".
//   - LearnedSortingStatus reads placementKeyTally (placement-report's key
//     verdicts), learningCounts (LearnedPlacement/getStatus's counts), the
//     abstention tally (placement-measure section 1) and placementAreaCounts
//     (placement-report's per-area gate).
//
// CHANGE_LOG: none; nothing here writes except the audit rows (see
// learned_sender_trust.go for why the settings it reads carry none).

// Reasons LearnedSenderView adds to placement.Reason's and LearnedGated/LearnedOff.
const (
	// SenderReasonAddressDecides: the sender's address has evidence of its own, so
	// its domain is not consulted.
	SenderReasonAddressDecides = "address_decides"
	// SenderReasonFailed: with a message in hand, its authentication FAILED (the
	// trusted Authentication-Results header reports a failure, or one clearly
	// failed anywhere). A view-only code: trust cannot help, and the message is
	// not sorted automatically.
	SenderReasonFailed = "failed"
)

// LearnedSenderSide is one key's line of LearnedSenderView.
type LearnedSenderSide struct {
	AreaID *ulid.ULID
	// Filings is the user's filings counted as evidence on this key (after
	// Forget).
	Filings int
	// Verdict is placement.Judge on this key alone.
	Verdict placement.Reason
	// Acting: learned sorting would place this sender's next AUTHENTICATED
	// message from this key (the decision, the gate and the off switch).
	Acting bool
	// Reason is why not, "" when acting.
	Reason string
	// ForgottenAt is the key's Forget mark, nil when none.
	ForgottenAt *time.Time
}

// LearnedSenderView is LearnedSender/get's entry for one address.
type LearnedSenderView struct {
	Sender  string
	Address LearnedSenderSide
	Domain  LearnedSenderSide
	// DomainName is the domain key's value.
	DomainName string
	// DecidedBy is "address", "domain" or "none".
	DecidedBy string
	Trusted   bool
	// TrustUseful is false when the address is at one of the user's OWN identity
	// domains: mail claiming to be from there is always checked (the self-spoof
	// warning refuses it whatever trust says), so trusting it cannot help.
	TrustUseful bool
	// TrustWouldHelp is true only when a message was given (LearnedSenderViewOfMessage)
	// and it was refused ONLY for the missing trusted pass: no ambiguous From, no
	// TrustedFail, no AuthFail, no warning, an ASCII address not at an own
	// identity domain, and not trusted yet. That is when trusting would change
	// the outcome.
	TrustWouldHelp bool
	// ForgottenAt is the newer of the address's and the domain's marks.
	ForgottenAt *time.Time
}

// LearnedSenderEmpty is the entry for an input that cannot be keyed at all (not
// an address): nothing is known about it, and one bad entry must not fail a
// whole batch (LearnedSender/get's notFound stays empty).
func LearnedSenderEmpty(sender string) LearnedSenderView {
	return LearnedSenderView{
		Sender:    sender,
		Address:   LearnedSenderSide{Verdict: placement.NoEvidence, Reason: string(placement.NoEvidence)},
		Domain:    LearnedSenderSide{Verdict: placement.NoEvidence, Reason: string(placement.NoEvidence)},
		DecidedBy: "none", TrustUseful: false,
	}
}

// LearnedSenderViewOf computes the view of one From address, with no message
// context: authentication is a property of a message, so it reports "acting" as
// if authenticated and never reports "unauthenticated".
func (tx *UserTx) LearnedSenderViewOf(ctx context.Context, sender string) (LearnedSenderView, error) {
	return tx.learnedSenderView(ctx, sender, nil)
}

// LearnedSenderViewOfMessage is LearnedSenderViewOf with a message's sender
// (built as ingest builds it, from the message's raw headers): the SAME sender
// checks the arrival path runs (senderChecks) refine "acting" and "reason", and
// TrustWouldHelp is set. msg is ignored when its From address is not sender.
func (tx *UserTx) LearnedSenderViewOfMessage(ctx context.Context, sender string, msg placement.Sender) (LearnedSenderView, error) {
	return tx.learnedSenderView(ctx, sender, &msg)
}

func (tx *UserTx) learnedSenderView(ctx context.Context, sender string, msg *placement.Sender) (LearnedSenderView, error) {
	addrKey, domainKey, ok := placement.KeysFor(sender)
	if !ok {
		return LearnedSenderView{}, ErrSenderInvalid
	}
	enabled, err := tx.learnedEnabled(ctx)
	if err != nil {
		return LearnedSenderView{}, err
	}
	d, ev, err := tx.decideLearnedEvidence(ctx, addrKey, domainKey, time.Time{})
	if err != nil {
		return LearnedSenderView{}, err
	}
	out := LearnedSenderView{Sender: addrKey.Value, DomainName: domainKey.Value, DecidedBy: "none"}
	if out.Trusted, err = tx.SenderTrusted(ctx, addrKey.Value); err != nil {
		return out, err
	}
	own, err := tx.ownIdentityDomains(ctx)
	if err != nil {
		return out, err
	}
	out.TrustUseful = !own[domainKey.Value]
	// The gate matters only for a decision that acts.
	gated := false
	if d.Act {
		g, err := tx.learnedGate(ctx, d.Area, false)
		if err != nil {
			return out, err
		}
		gated = g.Stopped
	}
	acting := enabled && d.Act && !gated
	// With a message: the arrival path's own sender checks.
	refusal := ""
	if msg != nil {
		if a, _, ok := msg.Keys(); ok && a == addrKey {
			sd, err := tx.SenderDomainsFor(ctx)
			if err != nil {
				return out, err
			}
			trusted, err := tx.trustedSenderSet(ctx)
			if err != nil {
				return out, err
			}
			c := senderChecks(*msg, senderwarn.NewIndex(sd.Own, sd.Corresponded), trusted)
			// Would trusting change the outcome? Ask the SAME check again with this
			// address trusted, so the answer cannot drift from the ingest rule: the
			// waiver refuses a trusted failure and every warning still refuses.
			// (LearnedSender/trust needs no such refusal of its own: trust is per
			// address and ingest enforces these rules on every message, whatever
			// the UI offered.)
			if k, asciiOK := msg.TrustKey(); asciiOK && out.TrustUseful && !c.Trusted && c.Refusal == placement.Unauthenticated {
				with := make(map[string]bool, len(trusted)+1)
				for t := range trusted {
					with[t] = true
				}
				with[k] = true
				out.TrustWouldHelp = senderChecks(*msg, senderwarn.NewIndex(sd.Own, sd.Corresponded), with).Refusal == ""
			}
			if c.Refusal != "" {
				refusal = string(c.Refusal)
				if c.Refusal == placement.Unauthenticated {
					// The arrival path reports the first refusal (unauthenticated);
					// the card says the stronger thing when trust cannot help.
					switch {
					case c.Auth.AuthFail || c.Auth.TrustedFail:
						refusal = SenderReasonFailed
					case len(c.Warnings) > 0:
						refusal = string(placement.SenderWarned)
					}
				}
			}
		}
	}
	if acting && refusal != "" {
		acting = false
	}
	why := func() string { // why the DECIDING key is not acting
		switch {
		case !enabled:
			return string(LearnedOff)
		case d.Act && gated:
			return string(LearnedGated)
		case d.Act && refusal != "":
			return refusal
		}
		return string(d.Reason)
	}

	out.Address = LearnedSenderSide{AreaID: ev.AddressVerdict.Area, Filings: ev.Address.Total(), Verdict: ev.AddressVerdict.Reason}
	out.Domain = LearnedSenderSide{AreaID: ev.DomainVerdict.Area, Filings: ev.Domain.Total(), Verdict: ev.DomainVerdict.Reason}
	switch {
	case ev.Address.Total() > 0:
		out.DecidedBy = "address"
		out.Address.Acting = acting
		if !acting {
			out.Address.Reason = why()
		}
		out.Domain.Reason = SenderReasonAddressDecides
	case !ev.DomainFallbackBlocked && ev.Domain.Total() > 0:
		out.DecidedBy = "domain"
		out.Address.Reason = string(placement.NoEvidence)
		out.Domain.Acting = acting
		if !acting {
			out.Domain.Reason = why()
		}
	default:
		out.Address.Reason = string(placement.NoEvidence)
		out.Domain.Reason = string(placement.NoEvidence)
		if ev.DomainFallbackBlocked && ev.Domain.Total() > 0 {
			out.Domain.Reason = string(placement.DomainBlocked)
		}
	}
	if ev.DomainFallbackBlocked && ev.Domain.Total() > 0 && out.DecidedBy != "domain" {
		out.Domain.Reason = string(placement.DomainBlocked)
	}
	if !ev.AddressForgotten.IsZero() {
		t := ev.AddressForgotten
		out.Address.ForgottenAt, out.ForgottenAt = &t, &t
	}
	if !ev.DomainForgotten.IsZero() {
		t := ev.DomainForgotten
		out.Domain.ForgottenAt = &t
		if out.ForgottenAt == nil || t.After(*out.ForgottenAt) {
			out.ForgottenAt = &t
		}
	}
	return out, nil
}

// Audit vocabulary of the Activity entries Forget and Trust write (app.audit_log).
const (
	LearnedSenderObjectType = "LearnedSender"
	LearnedSenderForget     = "learned_sender.forget"
	LearnedSenderTrust      = "learned_sender.trust"
	LearnedSenderUntrust    = "learned_sender.untrust"
)

// AuditLearnedSender writes one Activity entry for a Forget or a trust change.
//
// ⚠ THE ADDRESS NEVER GOES INTO app.audit_log. Audit rows survive a hard delete
// of the user (user_id is ON DELETE SET NULL, deliberately), so a sender address
// there would outlive the account it belongs to. The address is written to
// app.learned_sender_events (cascades with the user, RLS forced); the audit row
// carries only that event's id in object_id and the scope in detail, and
// Activity resolves the address by joining through the events table.
func (tx *UserTx) AuditLearnedSender(ctx context.Context, action, subject, scope string) error {
	kind := strings.TrimPrefix(action, "learned_sender.")
	id := NewULID().String()
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.learned_sender_events (id, user_id, address, kind, scope)
		VALUES ($1, $2, $3, $4, $5)`, id, tx.userID.String(), subject, kind, scope); err != nil {
		return fmt.Errorf("store: learned sender event: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
		VALUES ($1, 'user', $2, $3, $4, jsonb_build_object('scope', $5::text))`,
		tx.userID.String(), action, LearnedSenderObjectType, id, scope); err != nil {
		return fmt.Errorf("store: audit learned sender: %w", err)
	}
	return nil
}

// ── LearnedSorting/status ───────────────────────────────────────────────────

// LearnedSortingWindowDays is the status window (the same as LearnedPlacement/
// getStatus and the abstention tally the report reads).
const LearnedSortingWindowDays = LearningStatusWindowDays

// LearnedSortingArea is one area's line of the status.
type LearnedSortingArea struct {
	AreaID            ulid.ULID
	Placed, Corrected int
	// Gate is "acting", "stopped" or "not_judged" (too few resolved placements
	// to judge; placement.GateMinResolved).
	Gate string
}

// LearnedSortingStatus is LearnedSorting/status's answer.
type LearnedSortingStatus struct {
	Enabled bool
	// Placed and Corrected are ARRIVAL placements decided in the window (as in
	// LearnedPlacement/getStatus); Precision and LowerBound are kept/(kept+
	// corrected) and its 95% Wilson lower bound over the RESOLVED ones, NaN when
	// none are resolved.
	Placed, Corrected     int
	Precision, LowerBound float64
	// SendersLearnedAddress/Domain: keys whose verdict is confident (the
	// report's KEYS BY VERDICT).
	SendersLearnedAddress, SendersLearnedDomain int
	// HeldBack counts held-back arrivals in the window by reason, every reason
	// in the vocabulary present (zero included). HeldBackSince is the first UTC
	// day the tally covers: it is recorded going forward only, so a window
	// reaching back past it is a LOWER BOUND; zero when nothing was ever
	// recorded.
	HeldBack      map[string]int
	HeldBackSince time.Time
	Areas         []LearnedSortingArea
}

// LearnedSortingStatus computes the status. It writes nothing.
func (tx *UserTx) LearnedSortingStatus(ctx context.Context, now time.Time) (LearnedSortingStatus, error) {
	out := LearnedSortingStatus{HeldBack: map[string]int{}, Precision: math.NaN(), LowerBound: math.NaN()}
	var err error
	if out.Enabled, err = tx.learnedEnabled(ctx); err != nil {
		return out, err
	}
	var ls LearningStatus
	if err := tx.learningCounts(ctx, &ls); err != nil {
		return out, err
	}
	out.Placed, out.Corrected = ls.Placed, ls.Corrected
	if resolved := ls.Kept + ls.Corrected; resolved > 0 {
		g := placement.Gate(placement.GateCounts{Kept: ls.Kept, Corrected: ls.Corrected})
		out.Precision, out.LowerBound = g.Precision, g.LCB
	}
	kt, err := tx.placementKeyTally(ctx)
	if err != nil {
		return out, err
	}
	out.SendersLearnedAddress = kt.Reasons[placement.KeyAddress][placement.Confident]
	out.SendersLearnedDomain = kt.Reasons[placement.KeyDomain][placement.Confident]

	tally, err := tx.AbstentionTally(ctx, LearnedSortingWindowDays, now)
	if err != nil {
		return out, err
	}
	for _, r := range LearnedPlacementLabels() {
		if got, ok := LearnedAbstentionReason(LearnedPlacementResult{Reason: placement.Reason(r)}); ok && got == r {
			out.HeldBack[r] = tally.ByReason[r]
		}
	}
	if !tally.FirstRecorded.IsZero() {
		out.HeldBackSince = tally.Since
		if tally.FirstRecorded.After(out.HeldBackSince) {
			out.HeldBackSince = tally.FirstRecorded
		}
	}

	// Areas, in rail order: windowed placed/corrected, and the real gate.
	counts, err := tx.placementAreaCounts(ctx, nil)
	if err != nil {
		return out, err
	}
	win, err := tx.areaWindowCounts(ctx)
	if err != nil {
		return out, err
	}
	rows, err := tx.tx.Query(ctx, `SELECT id FROM app.areas WHERE user_id = $1 ORDER BY position, id`, tx.userID.String())
	if err != nil {
		return out, fmt.Errorf("store: learned sorting status (areas): %w", err)
	}
	var ids []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: learned sorting status (scan area): %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			rows.Close()
			return out, fmt.Errorf("store: learned sorting status: area id %q: %w", s, err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for _, id := range ids {
		c := counts[id]
		g := placement.Gate(placement.GateCounts{Kept: c.GateKept, Corrected: c.GateCorrected})
		word := "acting"
		switch {
		case g.Stopped:
			word = "stopped"
		case !g.Judged:
			word = "not_judged"
		}
		w := win[id]
		out.Areas = append(out.Areas, LearnedSortingArea{AreaID: id, Placed: w[0], Corrected: w[1], Gate: word})
	}
	return out, nil
}

// areaWindowCounts is, per area, the arrival placements decided in the window
// and how many of those stand corrected: the same population as learningCounts
// (backfilled placements left out through backfilledMessagesSQL).
func (tx *UserTx) areaWindowCounts(ctx context.Context) (map[ulid.ULID][2]int, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT p.area_id, count(*), count(*) FILTER (WHERE `+effectiveOutcomeSQL("$2")+` = 'corrected')
		  FROM app.learned_placements p
		  LEFT JOIN `+backfilledMessagesSQL("$1")+` bf ON bf.message_id = p.message_id
		 WHERE p.user_id = $1 AND bf.message_id IS NULL
		   AND p.decided_at > now() - make_interval(days => $3)
		 GROUP BY p.area_id`, tx.userID.String(), keptAfterSeconds(), LearnedSortingWindowDays)
	if err != nil {
		return nil, fmt.Errorf("store: learned sorting status (area counts): %w", err)
	}
	defer rows.Close()
	out := map[ulid.ULID][2]int{}
	for rows.Next() {
		var s string
		var placed, corrected int
		if err := rows.Scan(&s, &placed, &corrected); err != nil {
			return nil, fmt.Errorf("store: learned sorting status (scan area counts): %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("store: learned sorting status: area id %q: %w", s, err)
		}
		out[id] = [2]int{placed, corrected}
	}
	return out, rows.Err()
}
