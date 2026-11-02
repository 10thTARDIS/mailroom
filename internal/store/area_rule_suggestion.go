// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/rules"
	"mailroom/internal/senderwarn"
)

// "ALWAYS PUT MAIL FROM X IN <AREA>?" (Area/ruleSuggestion).
//
// After the user files mail from one sender into an area by hand, again and
// again, and learned placement is not going to do it for them, the move
// confirmation offers to make a rule. The CLIENT asks after a manual move; the
// SERVER decides, here, so the thresholds live in one place and the evidence is
// the persisted state, not what the browser remembers.
//
// IT SUGGESTS ONLY WHEN ALL OF THESE HOLD:
//
//   - the user's own filings (message_labels.applied_by = 'user', the same
//     evidence learned placement reads) of mail from the address into the
//     area's own label number at least RuleSuggestionMinFilings, created in the
//     last RuleSuggestionWindowDays (the move just made is counted: the client
//     settles its write queue first);
//   - learned placement would NOT file the address's next message into the area,
//     decided by the SAME code ingest runs (decideLearnedEvidence for the
//     evidence, learnedGate for the area's correction gate, senderChecks for the
//     sender, learnedEnabled for the switch), so the two cannot drift: a user
//     whom learned placement is about to serve is not also nagged for a rule;
//   - no enabled rule already files mail from the address into the area;
//   - the user has not said "Not now" to this (address, area) in the last
//     RuleSuggestionWindowDays.
//
// THE REASON is always reported. On a suggestion it says why learned placement
// is not going to do this (RuleReasonMixed and the other placement reasons,
// RuleReasonOff, RuleReasonGated, RuleReasonElsewhere, or the sender check's
// refusal). On no suggestion it says which condition stopped it. A bare
// boolean cannot tell "nothing to offer" from "something is broken".

// Thresholds. Named so the tests and the changelog quote the same numbers.
const (
	// RuleSuggestionMinFilings is how many of the user's own filings of one
	// sender into one area earn the offer.
	RuleSuggestionMinFilings = 3
	// RuleSuggestionWindowDays bounds both the filings that count and how long
	// a "Not now" is remembered.
	RuleSuggestionWindowDays = 30
)

// Reasons Area/ruleSuggestion reports beyond placement.Reason's own strings
// (mixed, thin, contradicted, unauthenticated, warned, ambiguous, ...).
const (
	// Reasons that suggest.
	//
	// RuleReasonElsewhere: learned placement would file this sender's next
	// message, but into a different area from the one the user keeps choosing.
	RuleReasonElsewhere = "elsewhere"
	// RuleReasonOff: the user switched learned placement off.
	RuleReasonOff = "off"
	// RuleReasonGated: the area's correction gate has stopped learned placement.
	RuleReasonGated = "gated"

	// Reasons that do not suggest.
	//
	// RuleReasonFewFilings: fewer than RuleSuggestionMinFilings recent filings.
	RuleReasonFewFilings = "fewFilings"
	// RuleReasonLearned: learned placement will do it; no rule is needed.
	RuleReasonLearned = "learned"
	// RuleReasonCovered: an enabled rule already files this sender there.
	RuleReasonCovered = "covered"
	// RuleReasonDismissed: the user said "Not now" within the window.
	RuleReasonDismissed = "dismissed"
	// RuleReasonUnverified: learned placement might act, but the sender's most
	// recent message could not be read to check it, so nothing is offered.
	RuleReasonUnverified = "unverified"
	// RuleReasonNoArea: the area does not exist for this user.
	RuleReasonNoArea = "noArea"
	// RuleReasonNoAddress: the address has no usable form.
	RuleReasonNoAddress = "noAddress"
)

// RuleSuggestion is Area/ruleSuggestion's answer.
type RuleSuggestion struct {
	Suggest bool
	Reason  string
	Address string
	AreaID  ulid.ULID
	// Filings is the user's own recent filings of this sender into the area.
	Filings int
	// Earlier is how many live messages from the sender are NOT in the area.
	Earlier int
}

// RuleSuggestionSenderFunc builds the placement.Sender for a stored message,
// from its bytes: the caller owns the blob store and the trusted authserv-ids.
// It is the same function shape the explain path uses (ingest.LearnedSender).
type RuleSuggestionSenderFunc func(ctx context.Context, c ExplainCandidate) (placement.Sender, error)

// RuleSuggestion decides whether to offer a rule for address -> area. It writes
// nothing. senderFor is asked at most once, and only when learned placement
// would otherwise act into this very area.
func (tx *UserTx) RuleSuggestion(ctx context.Context, areaID ulid.ULID, address string, senderFor RuleSuggestionSenderFunc) (RuleSuggestion, error) {
	addr := strings.ToLower(strings.TrimSpace(address))
	out := RuleSuggestion{Address: addr, AreaID: areaID}

	labelID, found, err := tx.areaOwnLabel(ctx, areaID)
	if err != nil {
		return out, err
	}
	if !found {
		out.Reason = RuleReasonNoArea
		return out, nil
	}
	addrKey, domainKey, ok := placement.KeysFor(addr)
	if !ok {
		out.Reason = RuleReasonNoAddress
		return out, nil
	}

	if out.Filings, err = tx.recentUserFilings(ctx, areaID, addr); err != nil {
		return out, err
	}
	if out.Earlier, err = tx.messagesFromNotInArea(ctx, labelID, addr); err != nil {
		return out, err
	}
	if out.Filings < RuleSuggestionMinFilings {
		out.Reason = RuleReasonFewFilings
		return out, nil
	}

	reason, suggest, err := tx.suggestionVerdict(ctx, areaID, labelID, addr, addrKey, domainKey, senderFor)
	if err != nil {
		return out, err
	}
	out.Suggest, out.Reason = suggest, reason
	return out, nil
}

// suggestionVerdict is the part of the decision after the filings threshold:
// dismissed, covered by an enabled rule, then whether learned placement would
// file the next message. Shared with the learned-placement status list
// (LearningStatus) so the two cannot disagree about which senders are
// "not being learned".
func (tx *UserTx) suggestionVerdict(ctx context.Context, areaID, labelID ulid.ULID, addr string,
	addrKey, domainKey placement.Key, senderFor RuleSuggestionSenderFunc) (reason string, suggest bool, err error) {
	dismissed, err := tx.ruleSuggestionDismissed(ctx, areaID, addr)
	if err != nil {
		return "", false, err
	}
	if dismissed {
		return RuleReasonDismissed, false, nil
	}
	covered, err := tx.enabledRuleFiles(ctx, addr, labelID)
	if err != nil {
		return "", false, err
	}
	if covered {
		return RuleReasonCovered, false, nil
	}
	// Would learned placement file the next message from this address?
	return tx.learnedWouldNotFile(ctx, areaID, addr, addrKey, domainKey, senderFor)
}

// learnedWouldNotFile answers: when this address's next message arrives, will
// learned placement put it in areaID? suggest is true when it will NOT, and
// reason says why; when it will (or cannot be told), suggest is false.
func (tx *UserTx) learnedWouldNotFile(ctx context.Context, areaID ulid.ULID, addr string,
	addrKey, domainKey placement.Key, senderFor RuleSuggestionSenderFunc) (reason string, suggest bool, err error) {
	// 1. The switch (learnedArrival step 1).
	on, err := tx.learnedEnabled(ctx)
	if err != nil {
		return "", false, err
	}
	if !on {
		return RuleReasonOff, true, nil
	}
	// 4. The evidence and the decision (learnedArrival step 4): THE function.
	d, _, err := tx.decideLearnedEvidence(ctx, addrKey, domainKey, time.Time{})
	if err != nil {
		return "", false, err
	}
	if !d.Act {
		return string(d.Reason), true, nil
	}
	if d.Area != areaID {
		return RuleReasonElsewhere, true, nil
	}
	// 5. The gate (LearnedPlacementForNewMessage step 5), read without the
	// aged-placement write the live path does first: effectiveOutcomeSQL
	// already reads an aged open placement as kept.
	g, err := tx.learnedGate(ctx, areaID, false)
	if err != nil {
		return "", false, err
	}
	if g.Stopped {
		return RuleReasonGated, true, nil
	}
	// 4b. The sender checks, on the sender's most recent message: it is the
	// best stand-in for the next one. Authentication is per message, so a
	// sender whose latest mail fails it is not served by learned placement.
	cands, err := tx.ExplainCandidates(ctx, nil, addr, 1)
	if err != nil {
		return "", false, err
	}
	if len(cands) == 0 || senderFor == nil {
		return RuleReasonUnverified, false, nil
	}
	sender, err := senderFor(ctx, cands[0])
	if err != nil {
		// Unreadable bytes: we cannot show learned placement would refuse, so
		// say nothing rather than offer on a guess.
		return RuleReasonUnverified, false, nil
	}
	sd, err := tx.SenderDomainsFor(ctx)
	if err != nil {
		return "", false, err
	}
	c := senderChecks(sender, senderwarn.NewIndex(sd.Own, sd.Corresponded))
	if c.Refusal != "" {
		return string(c.Refusal), true, nil
	}
	return RuleReasonLearned, false, nil
}

// areaOwnLabel is the area's own label, and whether the area exists for this
// user.
func (tx *UserTx) areaOwnLabel(ctx context.Context, areaID ulid.ULID) (ulid.ULID, bool, error) {
	var s string
	err := tx.tx.QueryRow(ctx, `SELECT label_id FROM app.areas WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), areaID.String()).Scan(&s)
	if err != nil {
		if isNoRows(err) {
			return ulid.ULID{}, false, nil
		}
		return ulid.ULID{}, false, fmt.Errorf("store: rule suggestion (area): %w", err)
	}
	id, err := ulid.Parse(s)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: rule suggestion: parse label id %q: %w", s, err)
	}
	return id, true, nil
}

// recentUserFilings counts the user's own filings of mail from addr into the
// area's own label created within the window. Trashed mail is left out, as the
// learned-placement evidence leaves it out (placementFilings).
func (tx *UserTx) recentUserFilings(ctx context.Context, areaID ulid.ULID, addr string) (int, error) {
	var n int
	err := tx.tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM app.message_labels ml
		  JOIN app.areas a ON a.user_id = ml.user_id AND a.label_id = ml.label_id
		  JOIN app.messages m ON m.user_id = ml.user_id AND m.id = ml.message_id
		 WHERE ml.user_id = $1 AND a.id = $2 AND ml.applied_by = $3
		   AND lower(btrim(m.from_address::text)) = $4
		   AND m.trashed_at IS NULL
		   AND ml.created_at > now() - make_interval(days => $5)`,
		tx.userID.String(), areaID.String(), LabelByUser, addr, RuleSuggestionWindowDays).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: rule suggestion (filings): %w", err)
	}
	return n, nil
}

// messagesFromNotInArea counts the live messages from addr that do not carry
// the area's own label: what a rule made now would leave behind, which the
// client offers to move too. "Live" is not trashed, not junk, not a draft, not
// vanished. An area also fed by a tab or another label can hold a message
// without the own label; this counts those as "not in", and the client's move
// reports them as already there.
func (tx *UserTx) messagesFromNotInArea(ctx context.Context, labelID ulid.ULID, addr string) (int, error) {
	var n int
	err := tx.tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM app.messages m
		 WHERE m.user_id = $1 AND lower(btrim(m.from_address::text)) = $2
		   AND NOT m.is_junk AND m.trashed_at IS NULL AND m.vanished_at IS NULL AND NOT m.is_draft
		   AND NOT EXISTS (SELECT 1 FROM app.message_labels ml
		                    WHERE ml.user_id = m.user_id AND ml.message_id = m.id AND ml.label_id = $3)`,
		tx.userID.String(), addr, labelID.String()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: rule suggestion (earlier): %w", err)
	}
	return n, nil
}

func (tx *UserTx) ruleSuggestionDismissed(ctx context.Context, areaID ulid.ULID, addr string) (bool, error) {
	var dismissed bool
	err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.rule_suggestion_dismissals
		                WHERE user_id = $1 AND area_id = $2 AND address = $3
		                  AND dismissed_at > now() - make_interval(days => $4))`,
		tx.userID.String(), areaID.String(), addr, RuleSuggestionWindowDays).Scan(&dismissed)
	if err != nil {
		return false, fmt.Errorf("store: rule suggestion (dismissal): %w", err)
	}
	return dismissed, nil
}

// DismissRuleSuggestion records "Not now" for (area, address); a second one
// refreshes the time. The area must be the caller's: the foreign key to
// app.areas fails for another user's area id under RLS (the row is invisible),
// which is reported as found=false rather than an error.
//
// No change_log row: a dismissal is not a JMAP-visible object (migration 00085).
func (tx *UserTx) DismissRuleSuggestion(ctx context.Context, areaID ulid.ULID, address string) (found bool, err error) {
	addr := strings.ToLower(strings.TrimSpace(address))
	if _, _, ok := placement.KeysFor(addr); !ok {
		return false, nil
	}
	if _, found, err = tx.areaOwnLabel(ctx, areaID); err != nil || !found {
		return false, err
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.rule_suggestion_dismissals (user_id, area_id, address, dismissed_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id, area_id, address) DO UPDATE SET dismissed_at = excluded.dismissed_at`,
		tx.userID.String(), areaID.String(), addr); err != nil {
		return false, fmt.Errorf("store: rule suggestion (dismiss): %w", err)
	}
	return true, nil
}

// enabledRuleFiles reports whether any ENABLED rule both names addr in its
// condition and files into the label. Deliberately conservative: a condition
// that mentions the address anywhere (any field, any operator, any nesting)
// counts as covering it, because an unneeded offer is a nuisance the user has
// to dismiss, and a rule that is NOT redundant but is treated as covered costs
// one missed offer.
func (tx *UserTx) enabledRuleFiles(ctx context.Context, addr string, labelID ulid.ULID) (bool, error) {
	rs, err := tx.ListRules(ctx, true)
	if err != nil {
		return false, err
	}
	for _, r := range rs {
		if !conditionMentions(r.Conditions, addr) {
			continue
		}
		for _, a := range r.Actions {
			if a.Type != rules.ActionAddLabel || strings.TrimSpace(a.Label) == "" {
				continue
			}
			id, ok, err := tx.ResolveRuleLabel(ctx, a.Label)
			if err != nil {
				return false, err
			}
			if ok && id == labelID {
				return true, nil
			}
		}
	}
	return false, nil
}

// conditionMentions: some leaf of the tree has a value containing addr
// (case-insensitive).
func conditionMentions(c rules.Condition, addr string) bool {
	if c.Field != "" && strings.Contains(strings.ToLower(c.Value), addr) {
		return true
	}
	for _, sub := range c.All {
		if conditionMentions(sub, addr) {
			return true
		}
	}
	for _, sub := range c.Any {
		if conditionMentions(sub, addr) {
			return true
		}
	}
	return false
}
