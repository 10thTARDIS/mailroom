// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// PENDING APPROVALS: confirm mode's backend (#49, ADR-042 §7).
//
// A destructive call by a key whose scope is in confirm_scopes is not run. It is
// stored — the canonical request (ids and the draft's content hash, never a
// body) plus a summary written here, server-side, for the person deciding — and
// the caller is told it is pending:
//
//	Email/set           notDestroyed[id] = {type: "pendingApproval", pendingActionId}
//	EmailSubmission/set notCreated[cid]  = {type: "pendingApproval", pendingActionId}
//
// A person decides with PendingAction/set (session only). Approval RE-VALIDATES
// everything at that moment — the request is still pending and in time, the
// key is active, the draft is byte-for-byte the one requested (content hash),
// and then the scope check, the restriction, the rate and the budget, by
// running the call through the same gate as the key's own calls — and only then
// executes it, attributed to the key (outcome "approved") and to the session
// that approved it (app.audit_log). Unanswered requests expire after
// PendingApprovalWindow: on read, on decision, and by SweepPendingApprovals.

// SetError and PendingAction error types.
const (
	setPendingApproval = "pendingApproval"
	setPendingExpired  = "pendingExpired"
	setNotPending      = "notPending"
	setKeyNotActive    = "keyNotActive"
	setDraftChanged    = "draftChanged"
)

type heldResponse struct {
	body      json.RawMessage
	pendingID *ulid.ULID
}

// pendingPayload is the stored, canonical request.
type pendingPayload struct {
	Destroy []string                   `json:"destroy,omitempty"`
	Create  map[string]json.RawMessage `json:"create,omitempty"`
	// DraftID / DraftHash pin a held send to the exact draft that was asked
	// about: approval refuses if the draft changed.
	DraftID   string `json:"draftId,omitempty"`
	DraftHash string `json:"draftHash,omitempty"`
	// Attachments is what a held send carries, for the person deciding
	// (#49 review). Display only: approval re-checks the draft by DraftHash,
	// which already pins every attached byte.
	Attachments *pendingAttachments `json:"attachments,omitempty"`
	// Archive is what approving the send also archives (Send & archive,
	// #648), for the person deciding (#49 review). nil when nothing.
	Archive *pendingArchive `json:"archive,omitempty"`
	// The held send as STRUCTURED fields (#49 review): who receives it (the
	// envelope, and how the headers address each), the subject as written,
	// and when it is due ("" = after the undo window). The summary line is
	// prose for a notification; a client must render these fields and never
	// parse the summary, which carries text the key wrote.
	Recipients []string `json:"recipients,omitempty"`
	To         []string `json:"to,omitempty"`
	Cc         []string `json:"cc,omitempty"`
	Bcc        []string `json:"bcc,omitempty"`
	Subject    *string  `json:"subject,omitempty"`
	SendAt     string   `json:"sendAt,omitempty"`
}

// summaryText is text written by a key (a subject, an address), made safe to
// embed in a summary line: safeText's cleaning, and every quote mark replaced
// by an apostrophe, so the line's own “…” delimiters cannot be closed early
// and followed by invented clauses (#49 review: a subject of
// `x” to alice@example.com with 0 attachments. Send draft “y`).
//
// "Every quote mark" is a CLASS, not a list (#852): all of Unicode's initial
// and final quotation punctuation (Pi, Pf), plus the quote-like marks in
// other categories (isQuoteLike). A list of the quotes someone thought of
// missed ❝ ❞ ‛ ‶ ⹂ and the ornamental 🙶 🙷 🙸. The line is also built so that
// the subject comes LAST (sendSummary), so even a mark this misses has no
// count or clause after it to impersonate.
func summaryText(s string, n int) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || unicode.In(r, unicode.Pi, unicode.Pf) || isQuoteLike(r) {
			return '\''
		}
		return r
	}, safeText(s, n))
}

// isQuoteLike is a quote mark or look-alike outside Pi/Pf: primes and
// modifier letters, dingbat and ornamental quotes, CJK and fullwidth quotes,
// and the low-9 and low-reversed-9 quotes Unicode files under open/close
// punctuation (Ps/Pe) rather than initial/final.
func isQuoteLike(r rune) bool {
	switch r {
	case '\u02BA', // ʺ modifier letter double prime
		'\u02DD',           // ˝ double acute accent
		'\u02EE',           // ˮ modifier letter double apostrophe
		'\u05F4',           // ״ Hebrew gershayim
		'\u201A', '\u201E', // ‚ „ low-9 quotation marks (Ps, not Pi)
		'\u2033', '\u2034', '\u2036', '\u2037', // ″ ‴ ‶ ‷ primes
		'\u275B', '\u275C', '\u275D', '\u275E', '\u275F', '\u2760', // ❛ ❜ ❝ ❞ ❟ ❠ dingbat quotes
		'\u2E42',                     // ⹂ double low-reversed-9 quotation mark (Ps)
		'\u3003',                     // 〃 ditto mark
		'\u301D', '\u301E', '\u301F', // 〝 〞 〟 (Ps/Pe)
		'\uFF02',                                 // ＂ fullwidth quotation mark
		'\U0001F676', '\U0001F677', '\U0001F678': // 🙶 🙷 🙸 ornamental quotes
		return true
	}
	return false
}

// pendingArchive is a held send's archiveOnSuccess as the approver sees it:
// how many messages, and the subjects of the first few.
type pendingArchive struct {
	Count    int      `json:"count"`
	Subjects []string `json:"subjects"`
}

// maxPendingArchiveSubjects bounds the subjects one held send stores.
const maxPendingArchiveSubjects = 5

func summarizeArchive(ctx context.Context, tx *store.UserTx, ids []ulid.ULID) (*pendingArchive, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := &pendingArchive{Count: len(ids), Subjects: []string{}}
	first := ids[:min(len(ids), maxPendingArchiveSubjects)]
	views, err := tx.GetEmails(ctx, first)
	if err != nil {
		return nil, err
	}
	for _, v := range views {
		subj := safeText(v.Subject, 60)
		if subj == "" {
			subj = "(no subject)"
		}
		out.Subjects = append(out.Subjects, subj)
	}
	return out, nil
}

// clause is the archive's part of the summary line, "" for none: a count.
// The subjects are in the structured payload (Archive.Subjects); they are mail
// text an outsider wrote, so they are not put in the line (#852).
func (p *pendingArchive) clause() string {
	if p == nil || p.Count == 0 {
		return ""
	}
	return fmt.Sprintf("archive %d message%s", p.Count, plural(p.Count))
}

// pendingAttachments is a held send's attachments as the approver sees them:
// how many, their names (at most maxPendingAttachmentNames, cleaned with
// safeText) and their total size in octets.
type pendingAttachments struct {
	Count     int      `json:"count"`
	Names     []string `json:"names"`
	TotalSize int64    `json:"totalSize"`
}

// maxPendingAttachmentNames bounds the names one held send stores.
const maxPendingAttachmentNames = 20

func summarizeAttachments(rows []store.AttachmentRow) *pendingAttachments {
	out := &pendingAttachments{Count: len(rows), Names: []string{}}
	for _, r := range rows {
		out.TotalSize += r.SizeBytes
		if len(out.Names) < maxPendingAttachmentNames {
			name := safeText(r.Filename, 80)
			if name == "" {
				name = "(unnamed " + safeText(r.ContentType, 60) + ")"
			}
			out.Names = append(out.Names, name)
		}
	}
	return out
}

// maxHeldCreateBytes bounds one held EmailSubmission create (metadata only).
const maxHeldCreateBytes = 16 << 10

// holdForApproval stores a held call inside the admission transaction and
// builds the caller's answer. A request that cannot be held (nothing valid to
// delete, a send that would fail anyway) gets that failure as its answer and
// heldResponse.pendingID nil: nothing is stored and nothing runs.
func (a *API) holdForApproval(ctx context.Context, tx *store.UserTx, userID ulid.ULID, p *keyPrincipal, k store.APIKey, method, kind string, args json.RawMessage, now time.Time) (heldResponse, string, error) {
	switch kind {
	case holdDelete:
		return a.holdDelete(ctx, tx, userID, k, method, args, now)
	case holdSend:
		return a.holdSend(ctx, tx, userID, k, method, args, now)
	}
	return heldResponse{}, "", fmt.Errorf("api: unknown hold kind %q", kind)
}

func (a *API) holdDelete(ctx context.Context, tx *store.UserTx, userID ulid.ULID, k store.APIKey, method string, raw json.RawMessage, now time.Time) (heldResponse, string, error) {
	var args emailSetArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return heldResponse{}, "", err
	}
	res := newSetResult(userID)
	state, err := tx.ChangeState(ctx, store.ObjectEmail)
	if err != nil {
		return heldResponse{}, "", err
	}
	res.OldState, res.NewState = encodeState(state), encodeState(state)

	var valid []ulid.ULID
	var validRaw []string
	for _, id := range args.Destroy {
		mid, perr := ulid.Parse(id)
		if perr != nil {
			res.NotDestroyed[id] = SetError{Type: setNotFound}
			continue
		}
		valid, validRaw = append(valid, mid), append(validRaw, id)
	}
	found, err := tx.GetEmails(ctx, valid)
	if err != nil {
		return heldResponse{}, "", err
	}
	exists := map[ulid.ULID]bool{}
	for _, v := range found {
		exists[v.ID] = true
	}
	visible, err := keyVisibleMessages(ctx, tx, valid)
	if err != nil {
		return heldResponse{}, "", err
	}
	var held []string
	for i, mid := range valid {
		if !exists[mid] || (visible != nil && !visible[mid]) {
			res.NotDestroyed[validRaw[i]] = SetError{Type: setNotFound}
			continue
		}
		held = append(held, validRaw[i])
	}
	if len(held) == 0 {
		return heldResponse{body: mustMarshal(res)}, "", nil
	}
	summary := fmt.Sprintf("Move %d message%s to Trash", len(held), plural(len(held)))
	payload, err := json.Marshal(pendingPayload{Destroy: held})
	if err != nil {
		return heldResponse{}, "", err
	}
	pid, err := tx.CreateAPIKeyPending(ctx, store.APIKeyPending{KeyID: k.ID, Kind: holdDelete, Method: method,
		Payload: payload, Summary: summary, ExpiresAt: now.Add(PendingApprovalWindow)})
	if err != nil {
		return heldResponse{}, "", err
	}
	for _, id := range held {
		res.NotDestroyed[id] = SetError{Type: setPendingApproval, PendingActionID: pid.String(),
			Description: "this key's deletes wait for your approval; nothing has been deleted yet"}
	}
	return heldResponse{body: mustMarshal(res), pendingID: &pid}, summary, nil
}

func (a *API) holdSend(ctx context.Context, tx *store.UserTx, userID ulid.ULID, k store.APIKey, method string, raw json.RawMessage, now time.Time) (heldResponse, string, error) {
	var args submissionSetArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return heldResponse{}, "", err
	}
	res := newSetResult(userID)
	state, err := tx.ChangeState(ctx, store.ObjectEmailSubmission)
	if err != nil {
		return heldResponse{}, "", err
	}
	res.OldState, res.NewState = encodeState(state), encodeState(state)

	var cid string
	var create json.RawMessage
	for c, r := range args.Create { // exactly one: submissionSetNeeds insists
		cid, create = c, r
	}
	answer := func(serr SetError) (heldResponse, string, error) {
		res.NotCreated[cid] = serr
		return heldResponse{body: mustMarshal(res)}, "", nil
	}
	if len(create) > maxHeldCreateBytes {
		return answer(SetError{Type: setInvalidProps, Description: "this submission is too large to hold for approval"})
	}
	if a.composer == nil {
		return answer(SetError{Type: setNotImplemented, Description: "this server is not configured to send mail"})
	}
	req, serr := parseSubmitRequest(create)
	if serr != nil {
		return answer(*serr)
	}
	if serr := keyMaySubmitDraft(ctx, tx, req.EmailID, false); serr != nil {
		return answer(*serr)
	}
	if serr := keyMayArchive(ctx, tx, req.ArchiveOnSuccess); serr != nil {
		return answer(*serr)
	}
	// The send is PLANNED now — every check the send makes, writing nothing —
	// so an agent learns at once that a send is impossible, and so the person
	// is shown who it really goes to: the recipients from the bytes.
	plan, err := a.composer.PlanSubmit(ctx, tx, userID, req)
	if err != nil {
		return answer(*submitSetError(err))
	}
	if serr := a.keyMaySendFrom(ctx, plan.AccountID); serr != nil {
		return answer(*serr)
	}
	views, err := tx.GetEmails(ctx, []ulid.ULID{req.EmailID})
	if err != nil {
		return heldResponse{}, "", err
	}
	if len(views) != 1 {
		return answer(SetError{Type: setInvalidEmail, Description: "no such draft"})
	}
	draft := views[0]
	rows, aerr := plan.Attachments()
	if aerr != nil {
		// Fail closed: a send whose attachments cannot be listed is not put in
		// front of a person as if it had none.
		return answer(SetError{Type: setInvalidEmail,
			Description: "the draft's attachments could not be read, so it cannot be held for approval"})
	}
	atts := summarizeAttachments(rows)
	archive, err := summarizeArchive(ctx, tx, req.ArchiveOnSuccess)
	if err != nil {
		return heldResponse{}, "", err
	}
	summary := sendSummary(draft.Subject, plan.Recipients(), atts, archive)
	to, cc, bcc := plan.HeaderRecipients()
	subject := draft.Subject
	var sendAt string
	if at := plan.SendAt(); !at.IsZero() {
		sendAt = jmapDate(at)
	}
	payload, err := json.Marshal(pendingPayload{Create: map[string]json.RawMessage{cid: create},
		DraftID: req.EmailID.String(), DraftHash: hex.EncodeToString(draft.ContentHash), Attachments: atts,
		Archive: archive, Recipients: plan.Recipients(), To: to, Cc: cc, Bcc: bcc, Subject: &subject, SendAt: sendAt})
	if err != nil {
		return heldResponse{}, "", err
	}
	pid, err := tx.CreateAPIKeyPending(ctx, store.APIKeyPending{KeyID: k.ID, Kind: holdSend, Method: method,
		Payload: payload, Summary: summary, ExpiresAt: now.Add(PendingApprovalWindow)})
	if err != nil {
		return heldResponse{}, "", err
	}
	res.NotCreated[cid] = SetError{Type: setPendingApproval, PendingActionID: pid.String(),
		Description: "this key's sends wait for your approval; nothing has been sent yet"}
	return heldResponse{body: mustMarshal(res), pendingID: &pid}, summary, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// sendSummary is the prose line a notification shows. Who it goes to comes
// FIRST, then the counts (attachments and their size, messages archived),
// and the subject comes LAST (#852), so no count follows the subject and the
// subject cannot impersonate one. The recipients are key-written too, BY
// DESIGN (the envelope a key asked for): parseSubmitRequest admits only bare
// addresses there (store.CanonicalEnvelopeAddress, strict ASCII), and they
// are cleaned the same way.
// Attachment names and archived subjects are not in the line at all. The
// line is prose: PendingAction/get carries every one of these as structured
// fields, and those fields, not this line, are what an approval UI renders.
func sendSummary(subject string, rcpt []string, atts *pendingAttachments, archive *pendingArchive) string {
	subj := summaryText(subject, 80)
	if subj == "" {
		subj = "(no subject)"
	}
	shown := rcpt
	if len(shown) > 3 {
		shown = shown[:3]
	}
	names := make([]string, 0, len(shown))
	for _, r := range shown {
		names = append(names, summaryText(r, 80))
	}
	s := "Send to " + strings.Join(names, ", ")
	if extra := len(rcpt) - len(shown); extra > 0 {
		s += fmt.Sprintf(" (+%d more)", extra)
	}
	var parts []string
	if atts != nil && atts.Count > 0 {
		parts = append(parts, fmt.Sprintf("with %d attachment%s (%s)", atts.Count, plural(atts.Count),
			formatOctets(atts.TotalSize)))
	}
	if c := archive.clause(); c != "" {
		parts = append(parts, "and "+c)
	}
	if len(parts) > 0 {
		s += " " + strings.Join(parts, ", ")
	}
	return s + ": draft “" + subj + "”"
}

// formatOctets renders a size for a person: bytes, KB or MB (1024-based).
func formatOctets(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// ---- PendingAction/get --------------------------------------------------------

// pendingObject renders one held request. accountId is the JMAP account the
// held call acts on, which is always the caller's own (a user has one JMAP
// account, resolveAccount): a client looks the held messages up there
// (Email/get for a delete's emailIds). A key sees only its own requests
// (pendingGet's KeyID filter), so the field tells it nothing it could not
// already address.
func pendingObject(pp store.APIKeyPending, keyName string, userID ulid.ULID) map[string]any {
	obj := map[string]any{
		"id":        pp.ID.String(),
		"accountId": userID.String(),
		"keyId":     pp.KeyID.String(),
		"kind":      pp.Kind,
		"method":    pp.Method,
		"summary":   pp.Summary,
		"state":     pp.State,
		"createdAt": jmapDate(pp.CreatedAt),
		"expiresAt": jmapDate(pp.ExpiresAt),
		"decidedAt": jmapDatePtr(pp.DecidedAt),
		"detail":    nullIfEmpty(pp.Detail),
	}
	if keyName != "" {
		obj["keyName"] = keyName
	}
	var payload pendingPayload
	if json.Unmarshal(pp.Payload, &payload) == nil {
		switch pp.Kind {
		case holdDelete:
			obj["emailIds"] = payload.Destroy
		case holdSend:
			obj["emailIds"] = []string{payload.DraftID}
			obj["attachments"] = payload.Attachments
			obj["archive"] = payload.Archive
			obj["recipients"] = nonNilStrings(payload.Recipients)
			obj["to"] = nonNilStrings(payload.To)
			obj["cc"] = nonNilStrings(payload.Cc)
			obj["bcc"] = nonNilStrings(payload.Bcc)
			obj["subject"] = payload.Subject
			obj["sendAt"] = nullIfEmpty(payload.SendAt)
		}
	}
	return obj
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// maxPendingList bounds one PendingAction/get.
const maxPendingList = 200

// pendingGet: a session sees every request of the user; a key sees only its
// OWN, read-only (how an agent polls the request it made).
func (a *API) pendingGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string    `json:"accountId"`
		IDs       *[]string `json:"ids"`
		States    []string  `json:"states"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	var f store.PendingFilter
	if p := principalFrom(ctx); p != nil {
		id := p.key.ID
		f.KeyID = &id
	}
	notFound := []string{}
	var wanted []string
	if args.IDs != nil {
		valid, nf, _ := parseIDs(*args.IDs)
		notFound = nf
		f.IDs = valid
		if f.IDs == nil {
			f.IDs = []ulid.ULID{}
		}
		wanted = *args.IDs
	}
	if args.States != nil {
		f.States = args.States
	}
	var (
		list  []store.APIKeyPending
		names = map[ulid.ULID]string{}
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		if err := a.expirePending(ctx, tx); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		var err error
		if list, err = tx.ListAPIKeyPending(ctx, f, maxPendingList); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if principalFrom(ctx) == nil {
			keys, err := tx.ListAPIKeys(ctx)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			for _, k := range keys {
				names[k.ID] = k.Name
			}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	out := make([]map[string]any, 0, len(list))
	seen := map[string]bool{}
	for _, pp := range list {
		out = append(out, pendingObject(pp, names[pp.KeyID], userID))
		seen[pp.ID.String()] = true
	}
	for _, id := range wanted {
		if _, err := ulid.Parse(id); err == nil && !seen[id] {
			notFound = append(notFound, id)
		}
	}
	return mustMarshal(map[string]any{"accountId": userID.String(), "list": out, "notFound": notFound}), nil
}

// expirePending moves the user's overdue requests to "expired" and audits
// each one. Runs inside the caller's UserTx.
func (a *API) expirePending(ctx context.Context, tx *store.UserTx) error {
	expired, err := tx.ExpireAPIKeyPending(ctx, a.clock())
	if err != nil {
		return err
	}
	for _, pp := range expired {
		id := pp.ID
		if _, err := tx.InsertAPIKeyEvent(ctx, store.APIKeyEvent{KeyID: pp.KeyID, Method: pp.Method,
			Outcome: store.KeyOutcomeExpired, PendingID: &id,
			Summary: map[string]any{"kind": pp.Kind}}); err != nil {
			return err
		}
	}
	// An APPROVED request that was never admitted to run within
	// store.StuckApprovalAge: the process stopped between the decision's
	// commit and the run. It is marked failed and audited, so neither the
	// person nor the agent waits on a send or delete that will never happen.
	stuck, err := tx.FailStuckAPIKeyPending(ctx)
	if err != nil {
		return err
	}
	for _, pp := range stuck {
		id := pp.ID
		if _, err := tx.InsertAPIKeyEvent(ctx, store.APIKeyEvent{KeyID: pp.KeyID, Method: pp.Method,
			Outcome: store.KeyOutcomeDenied, PendingID: &id,
			Summary: map[string]any{"kind": pp.Kind, "reason": "approvedButNeverRan"}}); err != nil {
			return err
		}
	}
	return nil
}

// SweepPendingApprovals expires every user's overdue requests. Cross-user
// discovery is one narrow definer returning user ids; each user's requests
// are then expired in that user's own UserTx.
func (a *API) SweepPendingApprovals(ctx context.Context) (int, error) {
	users, err := a.store.UsersWithDuePending(ctx)
	if err != nil {
		return 0, err
	}
	for _, u := range users {
		if err := a.store.UserTx(ctx, u, func(tx *store.UserTx) error { return a.expirePending(ctx, tx) }); err != nil {
			return 0, err
		}
	}
	return len(users), nil
}

// RunPendingSweep runs SweepPendingApprovals every interval until ctx ends.
func (a *API) RunPendingSweep(ctx context.Context, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := a.SweepPendingApprovals(ctx); err != nil && ctx.Err() == nil {
				log.Warn("pending-approval sweep failed", "err", err)
			} else if n > 0 {
				log.Info("expired unanswered API key approval requests", "users", n)
			}
		}
	}
}

// ---- PendingAction/set ---------------------------------------------------------

// pendingSet decides held requests: update {id: {state: "approved"|"denied"}}.
// Session only (the scope table) — and refused here too if a key ever reached
// it, because "a key approves its own request" must be impossible twice over.
func (a *API) pendingSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	if principalFrom(ctx) != nil {
		return nil, forbidden("approving or denying a request is for a person, never a key")
	}
	var args emailSetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	res := newSetResult(userID)
	for cid := range args.Create {
		res.NotCreated[cid] = SetError{Type: setForbidden, Description: "requests are made by API keys, not created"}
	}
	for _, id := range args.Destroy {
		res.NotDestroyed[id] = SetError{Type: setForbidden, Description: "a request is kept for the audit; deny it instead"}
	}
	for id, patch := range args.Update {
		var p struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(patch, &p); err != nil || (p.State != store.PendingStateApproved && p.State != store.PendingStateDenied) {
			res.NotUpdated[id] = SetError{Type: setInvalidPatch, Description: `set "state" to "approved" or "denied"`,
				Properties: []string{"state"}}
			continue
		}
		pid, err := ulid.Parse(id)
		if err != nil {
			res.NotUpdated[id] = SetError{Type: setNotFound}
			continue
		}
		obj, serr, err := a.decidePending(ctx, userID, pid, p.State == store.PendingStateApproved)
		if err != nil {
			return nil, &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if serr != nil {
			res.NotUpdated[id] = *serr
			continue
		}
		res.Updated[id] = obj
	}
	return mustMarshal(res), nil
}

var errPendingDecided = errors.New("api: pending request already decided")

// decidePending denies or approves one request. Approval re-validates, marks
// the request approved (so a second approval cannot run it again), commits,
// and only then executes it through the key gate.
func (a *API) decidePending(ctx context.Context, userID, pid ulid.ULID, approve bool) (map[string]any, *SetError, error) {
	var (
		pp      store.APIKeyPending
		key     store.APIKey
		refusal *SetError
	)
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var found bool
		var err error
		if pp, found, err = tx.LockAPIKeyPending(ctx, pid); err != nil {
			return err
		}
		if !found {
			refusal = &SetError{Type: setNotFound}
			return nil
		}
		if pp.State != store.PendingStatePending {
			refusal = &SetError{Type: setNotPending, Description: "this request was already " + pp.State}
			return nil
		}
		event := store.APIKeyEvent{KeyID: pp.KeyID, Method: pp.Method, PendingID: &pp.ID,
			Summary: map[string]any{"kind": pp.Kind, "decidedBy": "session"}}
		if !a.clock().Before(pp.ExpiresAt) {
			if err := a.expirePending(ctx, tx); err != nil {
				return err
			}
			refusal = &SetError{Type: setPendingExpired, Description: "this request was not answered in time and has expired"}
			return nil
		}
		if !approve {
			if _, err := tx.DecideAPIKeyPending(ctx, pp.ID, store.PendingStateDenied, "denied by you"); err != nil {
				return err
			}
			event.Outcome = store.KeyOutcomeDeniedByUser
			if _, err := tx.InsertAPIKeyEvent(ctx, event); err != nil {
				return err
			}
			pp.State = store.PendingStateDenied
			return tx.AuditAPIKeyLifecycle(ctx, "api_key.pending.deny", pp.KeyID, map[string]any{"pendingId": pp.ID.String()})
		}
		fail := func(detail string, serr SetError) error {
			if _, err := tx.DecideAPIKeyPending(ctx, pp.ID, store.PendingStateFailed, detail); err != nil {
				return err
			}
			event.Outcome = store.KeyOutcomeDenied
			event.Summary = mergeSummary(event.Summary, map[string]any{"reason": serr.Type})
			if _, err := tx.InsertAPIKeyEvent(ctx, event); err != nil {
				return err
			}
			refusal = &serr
			return nil
		}
		k, kfound, err := tx.GetAPIKey(ctx, pp.KeyID)
		if err != nil {
			return err
		}
		if !kfound || !k.Active() {
			return fail("the key was revoked or suspended", SetError{Type: setKeyNotActive,
				Description: "the key that asked has been revoked or suspended since; nothing was done"})
		}
		key = k
		if pp.Kind == holdSend {
			var payload pendingPayload
			if err := json.Unmarshal(pp.Payload, &payload); err != nil {
				return err
			}
			changed, err := draftChanged(ctx, tx, payload)
			if err != nil {
				return err
			}
			if changed {
				return fail("the draft changed after the request", SetError{Type: setDraftChanged,
					Description: "the draft is no longer the one the key asked to send; nothing was sent"})
			}
			// A draft the key discarded or replaced keeps its bytes (it moved
			// to Trash, still draft-flagged), so draftChanged passes it — and
			// the run would then be refused by keyMaySubmitDraft, failing as
			// "approved, but refused when run". A draft_discard can land
			// between the key's hold and this decision (it checked before the
			// hold committed), so say what happened before anything runs
			// (#872 review).
			if gone, err := keyDraftWithdrawn(ctx, tx, pp.KeyID, payload); err != nil {
				return err
			} else if gone {
				return fail("the key discarded or replaced this draft", SetError{Type: setDraftChanged,
					Description: "the key discarded or replaced this draft after asking (it is no longer in " +
						"Drafts); nothing was sent"})
			}
		}
		ok, err := tx.DecideAPIKeyPending(ctx, pp.ID, store.PendingStateApproved, "")
		if err != nil {
			return err
		}
		if !ok {
			return errPendingDecided
		}
		pp.State = store.PendingStateApproved
		return tx.AuditAPIKeyLifecycle(ctx, "api_key.pending.approve", pp.KeyID, map[string]any{"pendingId": pp.ID.String()})
	})
	if errors.Is(err, errPendingDecided) {
		return nil, &SetError{Type: setNotPending, Description: "this request was decided by someone else just now"}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if refusal != nil {
		return nil, refusal, nil
	}
	if !approve {
		return map[string]any{"state": pp.State}, nil, nil
	}

	// EXECUTE, through the same gate as the key's own calls: scope, restriction,
	// rate and budget are all checked again against the key as it is now.
	var payload pendingPayload
	if err := json.Unmarshal(pp.Payload, &payload); err != nil {
		return nil, nil, err
	}
	var args json.RawMessage
	switch pp.Kind {
	case holdDelete:
		args = mustMarshal(map[string]any{"accountId": userID.String(), "destroy": payload.Destroy})
	case holdSend:
		args = mustMarshal(map[string]any{"accountId": userID.String(), "create": payload.Create})
	default:
		return nil, nil, fmt.Errorf("api: unknown pending kind %q", pp.Kind)
	}
	fn, ok := a.methods[pp.Method]
	if !ok {
		return nil, nil, fmt.Errorf("api: pending method %q is not registered", pp.Method)
	}
	principal := newPrincipal(key, "")
	principal.approval = &keyApproval{pendingID: pp.ID}
	result, mErr := a.runKeyCall(withPrincipal(ctx, principal), userID, principal, pp.Method, args, fn)
	if mErr != nil {
		if err := a.store.UserTx(context.WithoutCancel(ctx), userID, func(tx *store.UserTx) error {
			return tx.FailApprovedAPIKeyPending(ctx, pp.ID, "approved, but refused when run: "+mErr.Type)
		}); err != nil {
			return nil, nil, err
		}
		return map[string]any{"state": store.PendingStateFailed, "error": mErr}, nil, nil
	}
	return map[string]any{"state": store.PendingStateApproved, "result": result}, nil, nil
}

// keyDraftWithdrawn reports whether a held send's draft is still a draft but
// in Trash or out of Drafts — what draft_update and draft_discard leave — the
// same rule keyMaySubmitDraft applies when the approved send runs.
func keyDraftWithdrawn(ctx context.Context, tx *store.UserTx, keyID ulid.ULID, payload pendingPayload) (bool, error) {
	id, err := ulid.Parse(payload.DraftID)
	if err != nil {
		return true, nil
	}
	st, err := tx.APIKeyDraftStatus(ctx, keyID, id)
	if err != nil {
		return false, err
	}
	return st.IsDraft && (st.Trashed || !st.InDrafts), nil
}

// draftChanged reports whether a held send's draft is gone, no longer a
// draft, or has different bytes than when the key asked.
func draftChanged(ctx context.Context, tx *store.UserTx, payload pendingPayload) (bool, error) {
	id, err := ulid.Parse(payload.DraftID)
	if err != nil {
		return true, nil
	}
	want, err := hex.DecodeString(payload.DraftHash)
	if err != nil {
		return true, nil
	}
	ref, err := tx.MessageRef(ctx, id)
	if errors.Is(err, store.ErrMessageNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !ref.IsDraft || !bytes.Equal(ref.ContentHash, want), nil
}
