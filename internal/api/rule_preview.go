// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mime"
	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// Rule/preview — the blast radius of a rule BEFORE it is saved (#155).
//
// "This would have matched 37 of your messages, for example these" is the
// difference between a rule system people use and one they are afraid of. The
// alternative is saving a guess and discovering next week that it labelled six
// months of mail.
//
// It is deliberately NOT a Rule/set flag: the rule being previewed usually does
// not exist yet, and asking the user to save something in order to find out what
// it does is precisely the problem. The method takes a rule LITERAL and writes
// nothing — no rule row, no labels, no change_log entry.
//
// It evaluates through rules.Condition.Matches, the same matcher ingest uses. A
// preview with its own matching logic would eventually disagree with the engine,
// and a confidently wrong blast radius is worse than none at all.

// The scan bounds. A rule answerable from row facts scans the whole previewable
// mailbox (previewMaxScanRows, #720); one that must open message bytes reads a
// recent window. Either way the result always reports what it actually looked at.
//
// The blob path is bounded an order of magnitude tighter because it costs one
// decrypt + MIME parse per message: `to` and arbitrary headers are not on the
// row (there is no recipients table), so a rule testing them can only be
// answered by opening the bytes.
const (
	previewDefaultScan = 500
	previewMaxScan     = 5000

	previewDefaultScanBlob = 200
	previewMaxScanBlob     = 1000

	// previewMaxScanRows is both the default and the ceiling for a rule that
	// needs only facts already on the message row (#720). Those rules cost no
	// decrypt and no MIME parse, so Rule/preview and Rule/apply look at the
	// whole previewable mailbox up to this many messages instead of the newest
	// previewDefaultScan: a rule's matches older than the newest 500 were
	// otherwise never previewed and never touched. A caller-supplied limit
	// still lowers it. Complete stays Scanned >= Total, so a mailbox larger than
	// this reports itself as bounded.
	previewMaxScanRows = 100000

	previewMaxSamples = 20
)

type rulePreviewArgs struct {
	AccountID  string           `json:"accountId"`
	Conditions *json.RawMessage `json:"conditions"`
	// TargetLabelID is the label this rule would apply. Optional, and only used
	// to report CONFLICTS: matched messages the user has already filed under a
	// DIFFERENT label by hand. Without it a preview still reports its blast
	// radius, it just cannot say whether the radius disagrees with the user.
	TargetLabelID string `json:"targetLabelId"`
	// Limit caps the scan. Omitted or <= 0 takes the default for whichever path
	// the conditions require; anything larger than the maximum is clamped, and
	// the response's scanned count reports the truth either way.
	Limit int `json:"limit"`
}

type rulePreviewSample struct {
	ID         string `json:"id"`
	Subject    string `json:"subject"`
	From       string `json:"from"`
	ReceivedAt string `json:"receivedAt"`
}

type rulePreviewResult struct {
	AccountID string `json:"accountId"`
	// Matched is how many of the scanned messages the conditions match.
	Matched int `json:"matched"`
	// Scanned is how many messages were actually examined, and Total how many
	// could have been. Scanned < Total means the answer is a lower bound over the
	// most recent Scanned messages — the client must say so rather than present
	// Matched as a whole-mailbox count.
	Scanned int `json:"scanned"`
	Total   int `json:"total"`
	// Complete is Scanned >= Total: the scan saw everything, so Matched is exact.
	Complete bool `json:"complete"`
	// ReadBodies reports that the scan had to open message blobs (the conditions
	// test `to` or a named header), which is why its window is smaller.
	ReadBodies bool `json:"readBodies"`
	// Conflicts is how many matched messages the user has already labelled
	// something else by hand, and ConflictLabels names those labels (#156). Zero
	// unless targetLabelId was given. This is the number that stops a rule being
	// accepted, so it is reported separately from the raw match count.
	Conflicts      int                 `json:"conflicts"`
	ConflictLabels []string            `json:"conflictLabels"`
	Samples        []rulePreviewSample `json:"samples"`
	// AreaFiling is set when targetLabelId is an area's own label (#708): what
	// Rule/apply would do to the matched mail, per outcome — will move, already
	// there, and left where it is because the user, another rule or something
	// else filed it elsewhere. The counts sum to Matched. Null otherwise.
	AreaFiling *ruleAreaFiling `json:"areaFiling"`
}

// rulePreview implements Rule/preview.
func (a *API) rulePreview(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args rulePreviewArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if args.Conditions == nil {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: "conditions is required"}
	}
	conds, err := rules.ParseConditions(*args.Conditions)
	if err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	// Validated exactly as Rule/set would: previewing something that cannot be
	// saved wastes a scan and, worse, reports "matched 0" for what is really a
	// malformed rule — the engine treats a bad condition as non-matching, so the
	// honest answer here is the validation error, not a zero.
	if err := rules.ValidateConditions(conds); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}

	// Which facts does this rule actually need? Only `to` and `header` require
	// the blob; everything else is on the row.
	headerNames := rules.HeaderNames([]rules.Rule{{Conditions: conds}})
	needBodies := len(headerNames) > 0 || rules.UsesField(conds, rules.FieldTo)

	limit := ruleScanLimit(args.Limit, needBodies)
	// Reading bodies needs the blob seam. Without one this instance holds no
	// message bytes, so such a rule cannot be previewed here — say so rather than
	// silently reporting a zero that means "could not look".
	if needBodies && a.raw == nil {
		return nil, &MethodError{Type: errServerFail,
			Description: "this server cannot read message bodies, so a rule testing " +
				"recipients or headers cannot be previewed"}
	}

	target := strings.TrimSpace(args.TargetLabelID)
	var (
		candidates []store.RulePreviewCandidate
		total      int
	)
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var e error
		if candidates, e = tx.RulePreviewCandidates(ctx, limit); e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		if total, e = tx.CountPreviewableMessages(ctx); e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}

	res := rulePreviewResult{
		AccountID:      userID.String(),
		Scanned:        len(candidates),
		Total:          total,
		ReadBodies:     needBodies,
		Samples:        []rulePreviewSample{},
		ConflictLabels: []string{},
	}
	conflictIDs := map[string]bool{}
	var matchedIDs []ulid.ULID
	for _, c := range candidates {
		facts := a.factsFor(ctx, userID, c, needBodies, headerNames)
		if !conds.Matches(facts) {
			continue
		}
		res.Matched++
		matchedIDs = append(matchedIDs, c.ID)
		// A CONFLICT is a matched message the user has already filed somewhere
		// else by hand (#156). It is the difference between "this rule would
		// label 47 messages" and "…and 3 of them you called Shopping", which is
		// the sentence that stops a bad rule being accepted.
		//
		// Only the user's own labels count. A label some other rule applied is an
		// automation's opinion, and treating it as a conflict would have rules
		// arguing with each other on the user's behalf.
		if target != "" {
			for _, lid := range c.UserLabelIDs {
				if lid.String() != target {
					res.Conflicts++
					conflictIDs[lid.String()] = true
					break
				}
			}
		}
		if len(res.Samples) < previewMaxSamples {
			res.Samples = append(res.Samples, rulePreviewSample{
				ID:         c.ID.String(),
				Subject:    c.Subject,
				From:       c.From,
				ReceivedAt: jmapDate(c.ReceivedAt),
			})
		}
	}
	res.Complete = res.Scanned >= res.Total
	if target != "" {
		af, mErr := a.previewAreaFiling(ctx, userID, target, matchedIDs)
		if mErr != nil {
			return nil, mErr
		}
		res.AreaFiling = af
	}
	if len(conflictIDs) > 0 {
		res.ConflictLabels = a.labelNames(ctx, userID, conflictIDs)
	}

	return mustMarshal(res), nil
}

// ruleScanLimit is how many messages Rule/preview and Rule/apply scan (#720).
// A rule answerable from row facts defaults to, and is capped at, the whole
// mailbox up to previewMaxScanRows; a rule that must open message bytes keeps
// the tighter blob bounds. A requested limit <= 0 means "the default".
func ruleScanLimit(requested int, needBodies bool) int {
	maxScan, defScan := previewMaxScanRows, previewMaxScanRows
	if needBodies {
		maxScan, defScan = previewMaxScanBlob, previewDefaultScanBlob
	}
	if requested <= 0 {
		return defScan
	}
	if requested > maxScan {
		return maxScan
	}
	return requested
}

// factsFor projects one candidate into the facts a condition tests. Bodies are
// opened only when the rule needs `to` or a named header, and a message whose
// bytes cannot be read is evaluated on the facts we do have — best-effort, the
// same way ingest's own header projection is. It still counts toward the scan,
// so the denominator stays truthful.
func (a *API) factsFor(
	ctx context.Context,
	userID ulid.ULID,
	c store.RulePreviewCandidate,
	needBodies bool,
	headerNames []string,
) rules.MessageFacts {
	facts := rules.MessageFacts{
		From:            c.From,
		FromDomain:      addressDomain(c.From),
		Subject:         c.Subject,
		Alias:           c.Alias,
		ListID:          c.ListID,
		ListUnsubscribe: c.ListUnsub,
		Headers:         map[string]string{},
	}
	if !needBodies || a.raw == nil {
		return facts
	}
	raw, err := a.raw.ReadRaw(ctx, userID, c.ContentHash)
	if err != nil {
		return facts
	}
	parsed, _ := mime.ParseBytes(raw)
	if parsed == nil {
		return facts
	}
	for _, addr := range append(parsed.Header.To(), parsed.Header.Cc()...) {
		if addr.Address != "" {
			facts.To = append(facts.To, addr.Address)
		}
	}
	for _, name := range headerNames {
		facts.Headers[strings.ToLower(name)] = parsed.Header.Get(name)
	}
	return facts
}

// labelNames resolves label ids to names, for the sentence a conflict produces
// ("3 of them you labelled Shopping"). An id that no longer resolves is dropped
// rather than shown raw — a ULID in that sentence is noise.
func (a *API) labelNames(ctx context.Context, userID ulid.ULID, ids map[string]bool) []string {
	want := make([]ulid.ULID, 0, len(ids))
	for s := range ids {
		if id, err := ulid.Parse(s); err == nil {
			want = append(want, id)
		}
	}
	out := []string{}
	_ = a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		views, err := tx.GetLabels(ctx, want)
		if err != nil {
			return nil
		}
		for _, v := range views {
			out = append(out, v.Name)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// addressDomain is the lowercased domain of an address, matching the ingest
// projection's domainOf so preview and evaluation agree on `fromDomain`.
func addressDomain(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return strings.ToLower(addr[at+1:])
}

// previewAreaFiling reports what Rule/apply would do to the matched mail when
// the target is an area's own label (#708), from the same store.PlanAreaFiling
// the apply performs. Nil, not zeroes, for a target that is not an area label:
// "this rule files nothing into an area" and "it would move none" differ.
//
// The target is resolved the way a rule's addLabel is (an id, an atom or a
// name), because that is what a client previewing a rule's action passes.
func (a *API) previewAreaFiling(
	ctx context.Context,
	userID ulid.ULID,
	target string,
	matched []ulid.ULID,
) (*ruleAreaFiling, *MethodError) {
	var out *ruleAreaFiling
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		lid, ok, e := tx.ResolveRuleLabel(ctx, target)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		if !ok {
			return nil
		}
		t, isArea, e := tx.AreaFilingTargetFor(ctx, []ulid.ULID{lid})
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		if !isArea {
			return nil
		}
		plan, e := tx.PlanAreaFiling(ctx, t, matched)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		out = newRuleAreaFiling(t)
		for _, id := range matched {
			if step, ok := plan[id]; ok {
				out.count(step.Outcome)
			}
		}
		return nil
	})
	return out, mErr
}
