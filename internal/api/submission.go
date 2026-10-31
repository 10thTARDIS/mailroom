// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/compose"
	"mailroom/internal/store"
)

// EmailSubmission methods (RFC 8621 §7) — the send surface (spec §5).
//
// `EmailSubmission/set create` is a SEND. It does not accept a message body: it
// references an existing Email (a draft) and an Identity, and the server does
// the rest. That indirection is what makes the security boundary enforceable —
// the From address the server validates is the one in the bytes it will
// actually transmit, not a field the client repeated in the request.
//
// Undo Send and Send Later are the SAME mechanism (spec §5): a submission is a
// durable job with a fire-at time. `sendAt` in the future is Send Later;
// `sendAt = now + the undo window` is the default, and setting
// `undoStatus: "canceled"` inside that window cancels the job. There is no
// separate "unsend" path and no separate scheduler.
//
// RFC 8621 §7.5's per-object error types are used as the RFC defines them:
// `forbiddenFrom` when the message's From is not one the user may send as (the
// spec §5 check), `noRecipients` for an empty envelope, `invalidEmail` when the
// referenced Email cannot be sent.
const (
	setForbiddenFrom = "forbiddenFrom"
	setInvalidEmail  = "invalidEmail"
	setNoRecipients  = "noRecipients"
	// setCannotEncrypt is the Mailroom-specific refusal of `encrypt: true` (#47).
	setCannotEncrypt = "cannotEncrypt"
	setCannotUnsend  = "cannotUnsend"
)

// submissionObject renders a store.Submission as an RFC 8621 EmailSubmission.
// dsn is the recorded bounce/delivery events for this submission (nil for a
// just-created one, or any submission no DSN has arrived for).
func submissionObject(s store.Submission, dsn []store.DSNEvent, props map[string]bool) map[string]any {
	rcpt := make([]map[string]any, 0, len(s.EnvelopeTo))
	for _, r := range s.EnvelopeTo {
		rcpt = append(rcpt, map[string]any{"email": r, "parameters": nil})
	}
	full := map[string]any{
		"id":         s.ID.String(),
		"identityId": s.IdentityID.String(),
		"emailId":    s.MessageID.String(),
		"threadId":   nilIfZero(s.ThreadID),
		"envelope": map[string]any{
			"mailFrom": map[string]any{"email": s.FromAddress, "parameters": nil},
			"rcptTo":   rcpt,
		},
		"sendAt":         jmapDate(s.SendAt),
		"undoStatus":     s.UndoStatus(),
		"deliveryStatus": deliveryStatusFor(s, dsn),
		"dsnBlobIds":     []string{},
		"mdnBlobIds":     []string{},
		// Mailroom extension (urn:mailroom:compose): the real submission state,
		// which carries more than RFC 8621's three undoStatus values.
		"mailroomState": s.State,
		// Mailroom extension (urn:mailroom:compose, #648): what this submission
		// will archive once it is SENT, or null. Echoed so a client can tell a
		// server that understood Send & archive from one that ignored the key.
		"archiveOnSuccess": archiveOnSuccessJSON(s.ArchiveOnSuccess),
		// Mailroom extension (urn:mailroom:compose, migration 00059): when the
		// user dismissed this FAILED send from the Outbox, or null. The Outbox
		// stops listing a dismissed failure; nothing else about it changes.
		"dismissedAt": jmapDatePtr(s.DismissedAt),
		// Mailroom extension (urn:mailroom:compose, #52): why a still-scheduled
		// submission is WAITING rather than sending — "reconnect" (its OAuth
		// account needs the user to sign in again) or "provider" (the provider
		// refuses this server's sign-in; the operator's to fix) — or null. The
		// Outbox shows "waiting" instead of "Sending…" from it. Derived from
		// the hold reason only the send worker writes (store.SubmissionHeldKind),
		// which changes with its own change row.
		"mailroomHeld": strOrNull(store.SubmissionHeldKind(s)),
	}
	if props == nil {
		return full
	}
	out := map[string]any{"id": s.ID.String()}
	for k, v := range full {
		if props[k] {
			out[k] = v
		}
	}
	return out
}

// deliveryStatusFor builds RFC 8621 §7.5's deliveryStatus map. Once the DSN
// pipeline (#30) has recorded delivery/bounce events for this submission, it
// reports one entry PER RECIPIENT from them — a real per-recipient verdict, which
// is what deliveryStatus is for. Before any DSN has arrived it falls back to the
// submission's own single state under the "*" wildcard, which is the honest "we
// only know what we did, not what the far end did" answer.
func deliveryStatusFor(s store.Submission, dsn []store.DSNEvent) map[string]any {
	if len(dsn) == 0 {
		return map[string]any{
			"*": map[string]any{
				"smtpReply": s.SMTPResponse,
				"delivered": deliveredState(s.State),
				"displayed": "unknown",
			},
		}
	}
	// DSNEventsForSubmission returns newest-first; keep the first (latest) event
	// per recipient so a "delayed then failed" sequence reports "failed".
	out := make(map[string]any, len(dsn))
	for _, e := range dsn {
		if _, seen := out[e.Recipient]; seen {
			continue
		}
		reply := e.Status
		if e.Diagnostic != "" {
			if reply != "" {
				reply += " "
			}
			reply += e.Diagnostic
		}
		out[e.Recipient] = map[string]any{
			"smtpReply": reply,
			"delivered": dsnDelivered(e.Action),
			"displayed": "unknown",
		}
	}
	return out
}

// dsnDelivered maps an RFC 3464 DSN action onto RFC 8621's delivered vocabulary.
func dsnDelivered(action string) string {
	switch action {
	case "delivered", "relayed", "expanded":
		return "yes"
	case "failed":
		return "no"
	default: // delayed (and anything unexpected) is still in flight
		return "queued"
	}
}

// deliveredState maps a submission state onto RFC 8621's delivery vocabulary.
func deliveredState(state string) string {
	switch state {
	case store.SubmissionSent:
		return "yes"
	case store.SubmissionFailed:
		return "no"
	default:
		return "queued"
	}
}

// archiveOnSuccessJSON renders Submission.ArchiveOnSuccess: null when empty.
func archiveOnSuccessJSON(ids []ulid.ULID) any {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func nilIfZero(id ulid.ULID) any {
	if id == (ulid.ULID{}) {
		return nil
	}
	return id.String()
}

// ---- EmailSubmission/get --------------------------------------------------

func (a *API) submissionGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties []string  `json:"properties"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	var props map[string]bool
	if args.Properties != nil {
		props = make(map[string]bool, len(args.Properties))
		for _, p := range args.Properties {
			props[p] = true
		}
	}

	var (
		state     int64
		byID      = make(map[ulid.ULID]store.Submission)
		dsnByID   = make(map[ulid.ULID][]store.DSNEvent)
		requested []requestedID
		notFound  = []string{}
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var list []store.Submission
		var err error
		if args.IDs == nil {
			// ⚠ BOUNDED: ListSubmissions answers the 100 most recent only, and
			// this response does not say so. A client that needs a complete set
			// (the Outbox after a reload, #653) asks EmailSubmission/query, which
			// reports its total and its limit, and feeds the ids in here.
			list, err = tx.ListSubmissions(ctx, 0)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			for _, v := range list {
				requested = append(requested, requestedID{raw: v.ID.String(), id: v.ID, ok: true})
			}
		} else {
			var valid []ulid.ULID
			valid, notFound, requested = parseIDs(*args.IDs)
			list, err = tx.GetSubmissions(ctx, valid)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
		}
		for _, v := range list {
			byID[v.ID] = v
			// The per-recipient delivery verdict (#30). RLS scopes this read to the
			// caller, so it can only ever return the caller's own submission's DSNs.
			ev, err := tx.DSNEventsForSubmission(ctx, v.ID)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			if len(ev) > 0 {
				dsnByID[v.ID] = ev
			}
		}
		state, err = tx.ChangeState(ctx, store.ObjectEmailSubmission)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	list := make([]map[string]any, 0, len(requested))
	for _, req := range requested {
		if !req.ok {
			continue
		}
		sv, ok := byID[req.id]
		if !ok {
			notFound = append(notFound, req.raw)
			continue
		}
		list = append(list, submissionObject(sv, dsnByID[req.id], props))
	}
	return mustMarshal(getResult{
		AccountID: userID.String(),
		State:     encodeState(state),
		List:      list,
		NotFound:  notFound,
	}), nil
}

// ---- EmailSubmission/changes ----------------------------------------------

func (a *API) submissionChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectEmailSubmission)
}

// ---- EmailSubmission/query (#653) -----------------------------------------
//
// WHY IT EXISTS: a client that reloads has no way to learn which submissions
// still need it. EmailSubmission/get with `ids: null` answers only the 100 most
// recent (store.ListSubmissions' cap, silently), and nothing else lists them, so
// the web client's Outbox was empty after every reload — a failed send vanished
// without the user acting on it, and a Send Later could no longer be cancelled
// from the Outbox. The client now bootstraps with
// `EmailSubmission/query {filter: {inOutbox: true}}` + `EmailSubmission/get`
// through an RFC 8620 §3.7 `#ids` back-reference.
//
// RFC 8621 §7.3 with a deliberately small surface, and everything outside it is
// REFUSED rather than ignored — an ignored condition returns a superset the
// client cannot detect:
//
//   - filter: a single FilterCondition of `undoStatus`, `emailIds` and the
//     Mailroom extension `inOutbox` (urn:mailroom:compose). Anything else —
//     another RFC condition, a FilterOperator — is `unsupportedFilter`.
//   - sort: none accepted; the order is fixed, newest first (created_at, then
//     id, descending). A non-empty `sort` is `unsupportedSort`.
//   - anchor: not supported, `invalidArguments`.
//   - limit: clamped to maxSubmissionQueryLimit, and when it is clamped (or was
//     omitted) the limit actually applied is RETURNED, as RFC 8620 §5.5
//     requires, so a client can never mistake a page for the whole result.
//   - total: always the real count (calculateTotal is accepted and costs
//     nothing extra — the count is needed to resolve a negative position).
//   - canCalculateChanges: false; there is no EmailSubmission/queryChanges.

// maxSubmissionQueryLimit is the most ids one EmailSubmission/query returns. It
// equals the session's advertised maxObjectsInGet, so a page fed straight into
// EmailSubmission/get through `#ids` is always a legal /get.
const maxSubmissionQueryLimit = 500

// submissionFilterConditions is every FilterCondition EmailSubmission/query
// honours (see emailFilterConditions for why the list exists at all).
var submissionFilterConditions = map[string]bool{
	"undoStatus": true,
	"emailIds":   true,
	// Mailroom extension (urn:mailroom:compose, #653): exactly the Outbox —
	// store.outboxPredicate, the server copy of web/src/app/ui/outbox.ts
	// outboxSubmissions.
	"inOutbox": true,
}

type submissionQueryArgs struct {
	AccountID      string          `json:"accountId"`
	Filter         json.RawMessage `json:"filter"`
	Sort           json.RawMessage `json:"sort"`
	Position       int             `json:"position"`
	Anchor         *string         `json:"anchor"`
	Limit          *int            `json:"limit"`
	CalculateTotal bool            `json:"calculateTotal"`
}

// submissionQueryResult is queryResult plus RFC 8620 §5.5's `limit`, present
// only when the server applied a limit other than the one requested.
type submissionQueryResult struct {
	queryResult
	Limit *int `json:"limit,omitempty"`
}

// parseSubmissionFilter decodes the filter into the store query. Pure, so the
// refusals are testable without a database.
func parseSubmissionFilter(raw json.RawMessage) (store.SubmissionQuery, *MethodError) {
	var q store.SubmissionQuery
	if len(raw) == 0 || string(raw) == "null" {
		return q, nil
	}
	var cond map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cond); err != nil {
		return q, &MethodError{Type: errInvalidArguments, Description: "filter must be a FilterCondition object"}
	}
	for k := range cond {
		if !submissionFilterConditions[k] {
			return q, &MethodError{Type: errUnsupportedFilter,
				Description: fmt.Sprintf("EmailSubmission/query does not support the filter property %q"+
					" (supported: undoStatus, emailIds, inOutbox; no FilterOperator)", k)}
		}
	}
	if v, ok := cond["undoStatus"]; ok && string(v) != "null" {
		var s string
		if err := json.Unmarshal(v, &s); err != nil || (s != "pending" && s != "final" && s != "canceled") {
			return q, &MethodError{Type: errInvalidArguments,
				Description: `undoStatus must be "pending", "final" or "canceled"`}
		}
		q.UndoStatus = s
	}
	if v, ok := cond["emailIds"]; ok && string(v) != "null" {
		var raws []string
		if err := json.Unmarshal(v, &raws); err != nil {
			return q, &MethodError{Type: errInvalidArguments, Description: "emailIds must be a list of Email ids"}
		}
		q.EmailIDsSet = true
		q.EmailIDs = make([]ulid.ULID, 0, len(raws))
		for _, r := range raws {
			id, err := ulid.Parse(r)
			if err != nil {
				return q, &MethodError{Type: errInvalidArguments, Description: "emailIds must be a list of Email ids"}
			}
			q.EmailIDs = append(q.EmailIDs, id)
		}
	}
	if v, ok := cond["inOutbox"]; ok && string(v) != "null" {
		var b bool
		// Only `true`. `false` has no Outbox meaning worth a second predicate,
		// and accepting it as a no-op would be the silent superset this parser
		// exists to refuse.
		if err := json.Unmarshal(v, &b); err != nil || !b {
			return q, &MethodError{Type: errInvalidArguments, Description: "inOutbox may only be true"}
		}
		q.InOutbox = true
	}
	return q, nil
}

func (a *API) submissionQuery(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args submissionQueryArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	q, mErr := parseSubmissionFilter(args.Filter)
	if mErr != nil {
		return nil, mErr
	}
	if s := strings.TrimSpace(string(args.Sort)); s != "" && s != "null" && s != "[]" {
		return nil, &MethodError{Type: errUnsupportedSort,
			Description: "EmailSubmission/query has one fixed order, newest first; omit sort"}
	}
	if args.Anchor != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "EmailSubmission/query does not support anchor"}
	}

	// The limit actually applied, reported back whenever it differs from what
	// was asked for — never a silent truncation (RFC 8620 §5.5).
	var reported *int
	limit := maxSubmissionQueryLimit
	switch {
	case args.Limit == nil:
		reported = &limit
	case *args.Limit < 0:
		return nil, &MethodError{Type: errInvalidArguments, Description: "limit must be a non-negative integer"}
	case *args.Limit > maxSubmissionQueryLimit:
		reported = &limit
	default:
		limit = *args.Limit
	}
	// A key's query defaults to the key page, not the Outbox's (#850).
	if principalFrom(ctx) != nil {
		limit = keyQueryLimitFor("EmailSubmission/query", args.Limit)
		reported = keyServerLimit("EmailSubmission/query", args.Limit)
	}
	q.Position = args.Position
	q.Limit = limit

	var (
		ids      []ulid.ULID
		total    int
		position int
		state    int64
	)
	mErr = a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		ids, total, position, err = tx.QuerySubmissions(ctx, q)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		state, err = tx.ChangeState(ctx, store.ObjectEmailSubmission)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(submissionQueryResult{
		queryResult: queryResult{
			AccountID:           userID.String(),
			QueryState:          encodeState(state),
			CanCalculateChanges: false,
			Position:            position,
			Total:               total,
			IDs:                 ulidStrings(ids),
		},
		Limit: reported,
	}), nil
}

// ---- EmailSubmission/getSendQuota (Mailroom extension) --------------------
//
// The read-only companion to the outbound daily cap (spec §5). ENFORCEMENT
// lives in the send path (store.ReserveOutbound counts one per account per UTC
// day and defers an over-cap send to midnight); this method just SURFACES the
// same ledger so a client can show "N of M sends today, resets at midnight UTC"
// before the user hits the wall. It never mutates anything.
//
// It is gated on urn:mailroom:compose (capability.go) and reads through the
// caller's RLS-scoped UserTx, so it can only ever report the caller's own
// accounts — a request naming another user's JMAP account is rejected by
// resolveAccount before any row is read (spec §10).

// sendQuotaAccount is one mail account's outbound send meter. remaining follows
// ReserveOutbound's 0 = blocked semantics: a limit of 0 means "cannot send", so
// remaining is max(0, limit-sent), which is 0 when limit is 0.
type sendQuotaAccount struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Sent      int    `json:"sent"`
	Limit     int    `json:"limit"`
	Remaining int    `json:"remaining"`
	ResetAt   string `json:"resetAt"`
}

// sendQuotaResult is the EmailSubmission/getSendQuota response. accountId echoes
// the caller's single JMAP account (which IS the mailroom user); list carries one
// entry per configured mail account. resetAt is the shared next-UTC-midnight the
// per-account meters roll over at, repeated on each entry for a client that only
// reads the list.
type sendQuotaResult struct {
	AccountID string             `json:"accountId"`
	List      []sendQuotaAccount `json:"list"`
	ResetAt   string             `json:"resetAt"`
}

// nextUTCMidnight is the instant the per-UTC-day outbound ledger next resets: the
// start of tomorrow in UTC. It matches store.ReserveOutbound / OutboundUsage,
// which key the ledger on CURRENT_DATE (UTC).
func nextUTCMidnight(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

func (a *API) submissionSendQuota(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	// Reject a request naming another user's account BEFORE opening a tx: identity
	// comes from the auth context, never this parameter (spec §10).
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	resetAt := jmapDate(nextUTCMidnight(time.Now()))
	var list []sendQuotaAccount
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		accts, err := tx.ListAccounts(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		list = make([]sendQuotaAccount, 0, len(accts))
		for _, acct := range accts {
			sent, limit, err := tx.OutboundUsage(ctx, acct.ID)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			remaining := limit - sent
			if remaining < 0 {
				remaining = 0
			}
			list = append(list, sendQuotaAccount{
				ID:        acct.ID.String(),
				Name:      acct.Name,
				Sent:      sent,
				Limit:     limit,
				Remaining: remaining,
				ResetAt:   resetAt,
			})
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(sendQuotaResult{
		AccountID: userID.String(),
		List:      list,
		ResetAt:   resetAt,
	}), nil
}

// ---- EmailSubmission/set --------------------------------------------------

// submissionSetArgs extends the shared /set shape with RFC 8621 §7.5's two
// on-success hooks, which are how a client atomically clears $draft / files the
// message in Sent as part of sending it.
type submissionSetArgs struct {
	AccountID             string                     `json:"accountId"`
	IfInState             *string                    `json:"ifInState"`
	Create                map[string]json.RawMessage `json:"create"`
	Update                map[string]json.RawMessage `json:"update"`
	Destroy               []string                   `json:"destroy"`
	OnSuccessUpdateEmail  map[string]json.RawMessage `json:"onSuccessUpdateEmail"`
	OnSuccessDestroyEmail []string                   `json:"onSuccessDestroyEmail"`
}

type submissionCreate struct {
	IdentityID string        `json:"identityId"`
	EmailID    string        `json:"emailId"`
	Envelope   *jmapEnvelope `json:"envelope"`
	SendAt     *string       `json:"sendAt"`
	// IdempotencyKey (Mailroom extension, #62) makes this create SAFE TO RETRY: a repeat
	// with the same key returns the submission that already exists instead of sending the
	// message a second time. The offline send queue generates one per queued action.
	//
	// Absent means "not retry-safe", which is how every client behaved before this and is
	// still correct for a one-shot send nothing will replay.
	IdempotencyKey *string `json:"idempotencyKey"`
	UndoStatus     *string `json:"undoStatus"`
	// ArchiveOnSuccess (Mailroom extension, urn:mailroom:compose, #648) is Send &
	// archive: the Emails to archive when, and ONLY when, this submission is
	// actually sent. The send worker performs it in the transaction that marks
	// the submission 'sent', so a send that fails, is undone or runs out of
	// retries archives nothing. Echoed back on the EmailSubmission, which is how
	// a client knows this server understood it (an older one ignores the key).
	ArchiveOnSuccess []string `json:"archiveOnSuccess"`
	// Encrypt (Mailroom extension, urn:mailroom:pgp, #47 ADR-038) asks for the
	// message to be signed and encrypted to every To/Cc recipient and to the
	// sender. A request that cannot be honoured (a recipient with no key, a Bcc,
	// no crypto process, no key bound) is REFUSED with `cannotEncrypt`; it is never
	// downgraded to a plaintext send.
	Encrypt *bool           `json:"encrypt"`
	Extra   json.RawMessage `json:"-"`
}

type jmapEnvelope struct {
	MailFrom *jmapAddr  `json:"mailFrom"`
	RcptTo   []jmapAddr `json:"rcptTo"`
}

type jmapAddr struct {
	Email string `json:"email"`
}

func (a *API) submissionSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	if a.composer == nil {
		return nil, &MethodError{Type: errServerFail,
			Description: "this server is not configured to send mail (no blob store or SMTP)"}
	}
	var args submissionSetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	res := newSetResult(userID)
	// createdEmails maps a submission creation id to the Email it sent, so the
	// onSuccessUpdateEmail patches (keyed by "#creationId") can find it.
	createdEmails := map[string]ulid.ULID{}

	var stateErr *MethodError
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		oldState, err := tx.ChangeState(ctx, store.ObjectEmailSubmission)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		created := map[string]ulid.ULID{}
		for cid, raw := range args.Create {
			obj, id, emailID, serr := a.createSubmission(ctx, tx, userID, raw)
			if serr != nil {
				res.NotCreated[cid] = *serr
				continue
			}
			created[cid] = id
			createdEmails[cid] = emailID
			res.Created[cid] = obj
		}

		// Update: RFC 8621 §7.5 allows exactly one mutation — undoStatus to
		// "canceled" — and Mailroom adds one more, dismissing a failed send
		// (updateSubmission). Everything else about a submission is server-owned.
		for id, patch := range args.Update {
			sid, ok := resolveSetID(id, created)
			if !ok {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			if serr := updateSubmission(ctx, tx, sid, patch); serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			res.Updated[id] = nil
		}

		// Destroy: a submission is a record of a send. RFC 8621 lets a server
		// refuse to destroy one, and this one does: deleting the evidence that
		// mail went out as a given identity is not a client's call.
		for _, id := range args.Destroy {
			res.NotDestroyed[id] = SetError{Type: setForbidden,
				Description: "a submission is a permanent record of a send and cannot be destroyed"}
		}

		// onSuccessUpdateEmail / onSuccessDestroyEmail (RFC 8621 §7.5): applied
		// only for submissions that were actually created, in THIS transaction,
		// so "the draft is no longer a draft" commits with the send or not at all.
		if serr := a.applyOnSuccess(ctx, tx, args, createdEmails, &res); serr != nil {
			return serr
		}

		newState, err := tx.ChangeState(ctx, store.ObjectEmailSubmission)
		if err != nil {
			return err
		}
		res.NewState = encodeState(newState)
		return nil
	})
	if stateErr != nil {
		return nil, stateErr
	}
	if err != nil && err != errMethodRollback {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	return mustMarshal(res), nil
}

// parseSubmitRequest reads one EmailSubmission create into the compose
// request. Shared by the method and by the approval queue, which plans a held
// send with exactly the request the method would make (#49, ADR-042 §7).
func parseSubmitRequest(raw json.RawMessage) (compose.SubmitRequest, *SetError) {
	var c submissionCreate
	if err := json.Unmarshal(raw, &c); err != nil {
		return compose.SubmitRequest{}, &SetError{Type: setInvalidProps, Description: err.Error()}
	}
	emailID, err := ulid.Parse(c.EmailID)
	if err != nil {
		return compose.SubmitRequest{}, &SetError{Type: setInvalidEmail,
			Description: "emailId is required and must be a valid id", Properties: []string{"emailId"}}
	}
	req := compose.SubmitRequest{EmailID: emailID}
	if c.IdempotencyKey != nil {
		key := strings.TrimSpace(*c.IdempotencyKey)
		// Bounded to match the column's CHECK, and REFUSED rather than silently
		// truncated: a truncated key could collide with another action's key, and a
		// collision here suppresses a send the user really did make.
		if key != "" && (len(key) < 8 || len(key) > 200) {
			return compose.SubmitRequest{}, &SetError{Type: setInvalidProps,
				Description: "idempotencyKey must be between 8 and 200 characters",
				Properties:  []string{"idempotencyKey"}}
		}
		req.IdempotencyKey = key
	}
	if c.ArchiveOnSuccess != nil {
		ids, serr := parseArchiveOnSuccess(c.ArchiveOnSuccess)
		if serr != nil {
			return compose.SubmitRequest{}, serr
		}
		req.ArchiveOnSuccess = ids
	}
	if c.Encrypt != nil && *c.Encrypt {
		req.Encrypt = true
	}
	if c.IdentityID != "" {
		iid, err := ulid.Parse(c.IdentityID)
		if err != nil {
			return compose.SubmitRequest{}, &SetError{Type: setInvalidProps,
				Description: "identityId must be a valid id", Properties: []string{"identityId"}}
		}
		req.IdentityID = iid
	}
	if c.Envelope != nil {
		if c.Envelope.MailFrom != nil && c.Envelope.MailFrom.Email != "" {
			addr, ok := store.CanonicalEnvelopeAddress(c.Envelope.MailFrom.Email)
			if !ok {
				return compose.SubmitRequest{}, envelopeAddressError("envelope/mailFrom/email")
			}
			req.EnvelopeFrom = addr
		}
		for i, r := range c.Envelope.RcptTo {
			addr, ok := store.CanonicalEnvelopeAddress(r.Email)
			if !ok {
				return compose.SubmitRequest{}, envelopeAddressError(fmt.Sprintf("envelope/rcptTo/%d/email", i))
			}
			req.EnvelopeTo = append(req.EnvelopeTo, addr)
		}
	}
	if c.SendAt != nil && *c.SendAt != "" {
		t, err := time.Parse(time.RFC3339, *c.SendAt)
		if err != nil {
			return compose.SubmitRequest{}, &SetError{Type: setInvalidProps,
				Description: "sendAt must be a UTC date", Properties: []string{"sendAt"}}
		}
		// THE ADVERTISED HORIZON IS NOW ENFORCED (#56). The session publishes
		// `maxDelayedSend` (session.go) and until send-later had a UI nothing checked
		// it — so a client could schedule a send for the year 3000 and the server
		// would dutifully write a durable job for it. A limit a client is TOLD about
		// and a limit the server ENFORCES that disagree is worse than either, which is
		// the same reasoning maxSizeUpload already carries.
		if horizon := time.Now().Add(maxDelayedSend); t.After(horizon) {
			return compose.SubmitRequest{}, &SetError{Type: setInvalidProps,
				Description: "sendAt is further ahead than this server will hold a message" +
					" (see maxDelayedSend in the session)",
				Properties: []string{"sendAt"}}
		}
		req.SendAt = t
	}

	return req, nil
}

// envelopeAddressError refuses an envelope address that is not a strict
// ASCII addr-spec (store.CanonicalEnvelopeAddress). The envelope is what SMTP
// is handed, and a key's envelope recipients lead the approval summary (#852
// review), so free text there would reach both. Sessions get the same rule;
// the web client never sends an envelope.
func envelopeAddressError(prop string) *SetError {
	return &SetError{Type: setInvalidProps, Properties: []string{prop},
		Description: "each envelope address must be a bare ASCII email address (local@domain), with no name, quotes or other text"}
}

// createSubmission validates and schedules one send. The From validation lives
// in compose.Composer.Submit (which reads it out of the message bytes); this
// function's job is to translate the JMAP shape and map the failures onto
// RFC 8621 §7.5's error types.
func (a *API) createSubmission(ctx context.Context, tx *store.UserTx, userID ulid.ULID, raw json.RawMessage) (map[string]any, ulid.ULID, ulid.ULID, *SetError) {
	req, serr := parseSubmitRequest(raw)
	if serr != nil {
		return nil, ulid.ULID{}, ulid.ULID{}, serr
	}
	emailID := req.EmailID
	// A key's send is recorded as the key's (#49, migration 00076), so
	// the archive the worker performs after it is the key's too and never
	// keeps a learned placement on the user's behalf. Also the run of an
	// approved held send, which carries the key's principal.
	if p := principalFrom(ctx); p != nil {
		kid := p.key.ID
		req.APIKeyID = &kid
	}
	if serr := keyMaySubmitDraft(ctx, tx, emailID, true); serr != nil {
		return nil, ulid.ULID{}, ulid.ULID{}, serr
	}
	// Also the run of an approved held send: the approval runs through here.
	if serr := keyMayArchive(ctx, tx, req.ArchiveOnSuccess); serr != nil {
		return nil, ulid.ULID{}, ulid.ULID{}, serr
	}

	// THE SEND IS PLANNED BEFORE IT IS RECORDED, so the ingest-only refusal below
	// can happen before anything durable exists. PlanSubmit runs every check
	// Submit makes — the draft check, the From-to-verified-identity resolution
	// that IS the security boundary, the identity and envelope cross-checks, the
	// recipient check — and writes nothing: SELECTs and one blob read.
	plan, err := a.composer.PlanSubmit(ctx, tx, userID, req)
	if err != nil {
		return nil, ulid.ULID{}, ulid.ULID{}, submitSetError(err)
	}

	// An account-restricted API key sends only from its accounts (#49,
	// ADR-042 §5). plan.AccountID is the account resolved from the verified
	// From in the bytes, the same one the ingest-only gate below reads.
	if serr := a.keyMaySendFrom(ctx, plan.AccountID); serr != nil {
		return nil, ulid.ULID{}, ulid.ULID{}, serr
	}

	// ADR-028 §6: a shadowed account cannot submit AT ALL, and the refusal must
	// land HERE — after the account is known, before the first side effect.
	//
	// plan.AccountID is the account the send would go out on, resolved from the
	// message's verified From address by the same compose-layer call the send
	// itself uses (store.ResolveSendIdentity); this reuses that resolution rather
	// than repeating it, because a second copy of From→identity→account could
	// disagree with the one that actually authorizes the send. It is not derived
	// from identityId, which is optional and is only ever cross-checked against
	// what the bytes resolve to.
	//
	// ⚠ REFUSING AFTER Submit RETURNED WAS TOO LATE, and cancelling the
	// submission did not undo it. Submit signs the message for a PGP-keyed
	// identity and REWRITES the Email's stored bytes in place (ADR-001's
	// documented exception, compose.prepareOutbound), so a refused send left the
	// user with a signed message they never sent — plus a canceled submission in
	// their history for a send that was never permitted in the first place.
	//
	// This is deliberately a SEPARATE, stricter gate than the writeback switch's
	// IMAP-side enforcement, not an inherited one. Suppressing writeback stops the
	// Sent copy being FILED (it is an APPEND intent) while leaving the SMTP send
	// itself working, because submission is a different egress behind a different
	// seam (provider.Sender) that the writeback engine never touches. Accepting
	// the create here and letting the writeback gate silently swallow the Sent
	// copy would be worse than either half alone: a not-yet-trusted shadow
	// instance would emit real mail to third parties and not even record it in
	// Sent. So the create is refused outright.
	//
	// It is ALSO not the only gate: compose.SendWorker.Send refuses again at the
	// SMTP seam, because this one cannot speak for a submission created before
	// the mode was switched on (ADR-028 §6, send.go's errIngestOnlyCannotSend).
	//
	// NO SAVEPOINT, because nothing above has written for this object: this is a
	// pure pre-flight refusal, no statement has errored, and a batch containing
	// one refused submission and one good one still creates the good one.
	acct, aerr := tx.GetAccount(ctx, plan.AccountID)
	if aerr != nil {
		return nil, ulid.ULID{}, ulid.ULID{}, &SetError{Type: setForbidden, Description: aerr.Error()}
	}
	if !acct.WritebackEnabled || a.store.WritebackDisabled() {
		return nil, ulid.ULID{}, ulid.ULID{}, &SetError{Type: setForbidden,
			Description: "this account is in ingest-only mode and cannot send"}
	}
	// #52, ADR-046 §5/§11: an OAuth account whose user must sign in again
	// (its grant is needs_reauth, or it was disconnected) cannot send, and saying so NOW — while the person is looking at
	// the composer, with the draft kept — is the honest moment. Accepting it
	// would queue mail behind a reconnect that may never come. A submission
	// created BEFORE the grant died is held by the send worker instead, never
	// failed (compose.SendWorker.holdIfAccountCannotSend). operator_error is
	// NOT refused: the server's broken sign-in is not the user's to fix, so
	// the send is accepted and held until the operator fixes it.
	if acct.AwaitingReconnect() {
		return nil, ulid.ULID{}, ulid.ULID{}, &SetError{Type: setAccountNeedsReconnect,
			Description: "this account needs you to sign in to it again before it can send; " +
				"reconnect it in Settings, Accounts (your draft is kept)"}
	}
	// ADR-046 §12: a RECEIVE-ONLY account (its provider refuses SMTP AUTH as
	// a policy, 5.7.139) is refused now, with the reason, rather than queued
	// to fail at the provider a few seconds later. Pre-flight, like the two
	// refusals above: nothing has been written for this object.
	if code, err := tx.SendBlocked(ctx, acct.ID); err != nil {
		return nil, ulid.ULID{}, ulid.ULID{}, &SetError{Type: setForbidden, Description: err.Error()}
	} else if code != "" {
		return nil, ulid.ULID{}, ulid.ULID{}, &SetError{Type: setAccountCannotSend,
			Description: store.SendBlockedReason(code) + " (your draft is kept)"}
	}

	out, err := a.composer.SubmitPlanned(ctx, tx, userID, plan)
	if err != nil {
		return nil, ulid.ULID{}, ulid.ULID{}, submitSetError(err)
	}

	// The send is a positive triage signal for the message it answers (a reply or
	// forward). Capture it best-effort and SAVEPOINT-guarded, keyed to the ORIGINAL
	// message — never able to abort the send it rides along with. mailroom#33 PR2.
	a.emitSendFeedback(ctx, tx, emailID)

	subs, err := tx.GetSubmissions(ctx, []ulid.ULID{out.SubmissionID})
	if err != nil || len(subs) == 0 {
		return map[string]any{"id": out.SubmissionID.String()}, out.SubmissionID, emailID, nil
	}
	// A just-scheduled send has no delivery events yet.
	return submissionObject(subs[0], nil, nil), out.SubmissionID, emailID, nil
}

// maxArchiveOnSuccess bounds archiveOnSuccess, matching the column's CHECK
// (migration 00059). Send & archive names one message; the bound only stops a
// hostile client parking an unbounded array on a row the send worker walks.
const maxArchiveOnSuccess = 100

// parseArchiveOnSuccess validates the Send & archive list (#648): every entry a
// valid id, at most maxArchiveOnSuccess distinct ones. Duplicates collapse.
// Existence, ownership and "still in the Inbox" are deliberately NOT checked
// here: the send may fire hours later (Send Later), so the only answer that
// matters is the one the send worker gets when it fires, under RLS.
func parseArchiveOnSuccess(raw []string) ([]ulid.ULID, *SetError) {
	seen := make(map[ulid.ULID]bool, len(raw))
	var out []ulid.ULID
	for _, s := range raw {
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, &SetError{Type: setInvalidProps,
				Description: "archiveOnSuccess must be a list of Email ids",
				Properties:  []string{"archiveOnSuccess"}}
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) > maxArchiveOnSuccess {
		return nil, &SetError{Type: setInvalidProps,
			Description: "archiveOnSuccess names too many Emails",
			Properties:  []string{"archiveOnSuccess"}}
	}
	return out, nil
}

// submitSetError maps a compose failure onto RFC 8621 §7.5's per-object errors.
// A From that is not verified is `forbiddenFrom` — the client learns it may not
// send as that address, and learns nothing else about why.
func submitSetError(err error) *SetError {
	var missing *compose.MissingKeysError
	var changed *compose.KeyChangedError
	switch {
	// Encryption refusals (#47, ADR-038). One type, with a plain description, so a
	// client has a single branch; the per-recipient detail it needs to render is
	// already available beforehand from RecipientKey/lookup.
	case errors.As(err, &missing):
		return &SetError{Type: setCannotEncrypt, Properties: []string{"encrypt"},
			Description: "these recipients have no usable encryption key: " + strings.Join(missing.Addresses, ", ")}
	case errors.As(err, &changed):
		parts := make([]string, 0, len(changed.Changes))
		for _, c := range changed.Changes {
			parts = append(parts, c.Address+" (was "+c.Pinned+", now "+c.Now+")")
		}
		return &SetError{Type: setCannotEncrypt, Properties: []string{"encrypt"},
			Description: "the encryption key changed and must be confirmed before you can encrypt to: " +
				strings.Join(parts, "; ")}
	case errors.Is(err, compose.ErrEncryptBcc):
		return &SetError{Type: setCannotEncrypt, Properties: []string{"encrypt"},
			Description: "a message with Bcc recipients cannot be encrypted: one encrypted message would reveal every recipient's key, including the Bcc ones"}
	case errors.Is(err, compose.ErrEncryptNoSenderKey):
		return &SetError{Type: setCannotEncrypt, Properties: []string{"encrypt"},
			Description: "the sending identity has no PGP key bound, so its mail cannot be encrypted"}
	case errors.Is(err, compose.ErrEncryptUnavailable):
		return &SetError{Type: setCannotEncrypt, Properties: []string{"encrypt"},
			Description: "this server cannot encrypt outgoing mail"}
	case errors.Is(err, store.ErrFromNotVerified):
		return &SetError{Type: setForbiddenFrom,
			Description: "the From address of this message is not a verified identity for your account"}
	case errors.Is(err, compose.ErrNoRecipients):
		return &SetError{Type: setNoRecipients,
			Description: "the message has no recipients"}
	case errors.Is(err, store.ErrNotADraft):
		return &SetError{Type: setInvalidEmail,
			Description: "only a draft can be submitted"}
	case errors.Is(err, store.ErrMessageNotFound):
		return &SetError{Type: setInvalidEmail, Description: "no such email"}
	default:
		return &SetError{Type: setForbidden, Description: err.Error()}
	}
}

// updateSubmission applies one EmailSubmission/set update. Exactly two patches
// are legal, each ALONE:
//
//   - `undoStatus: "canceled"` — RFC 8621 §7.5's one mutation (Undo Send);
//   - `dismissed: true` — the Mailroom extension (urn:mailroom:compose,
//     migration 00059) that takes a permanently FAILED submission off the
//     Outbox. Refused for any other state: a pending send is cancelled, not
//     dismissed, and a sent or cancelled one is not in the Outbox to dismiss.
//
// Everything else about a submission is server-owned.
func updateSubmission(ctx context.Context, tx *store.UserTx, id ulid.ULID, patch json.RawMessage) *SetError {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(patch, &raw); err != nil {
		return &SetError{Type: setInvalidPatch, Description: err.Error()}
	}
	const onlyUpdatable = `the only updatable properties of an EmailSubmission are undoStatus ` +
		`(to "canceled") and dismissed (to true), one per update`
	if len(raw) != 1 {
		return &SetError{Type: setInvalidProps, Description: onlyUpdatable}
	}
	if val, ok := raw["dismissed"]; ok {
		return dismissSubmission(ctx, tx, id, val)
	}
	val, ok := raw["undoStatus"]
	if !ok {
		return &SetError{Type: setInvalidProps, Description: onlyUpdatable}
	}
	var status string
	if err := json.Unmarshal(val, &status); err != nil || status != "canceled" {
		return &SetError{Type: setInvalidProps,
			Description: `undoStatus may only be set to "canceled"`, Properties: []string{"undoStatus"}}
	}

	err := tx.CancelSubmission(ctx, id)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrSubmissionNotFound):
		return &SetError{Type: setNotFound}
	case errors.Is(err, store.ErrSubmissionNotCancelable):
		return &SetError{Type: setCannotUnsend,
			Description: "this message is already on its way; the undo window has closed"}
	default:
		return &SetError{Type: setForbidden, Description: err.Error()}
	}
}

// dismissSubmission applies `dismissed: true` (see updateSubmission). There is
// no un-dismiss: `false` is refused rather than silently ignored, because a
// client that sends it believes it can bring the row back and cannot.
func dismissSubmission(ctx context.Context, tx *store.UserTx, id ulid.ULID, val json.RawMessage) *SetError {
	var dismissed bool
	if err := json.Unmarshal(val, &dismissed); err != nil || !dismissed {
		return &SetError{Type: setInvalidProps,
			Description: "dismissed may only be set to true", Properties: []string{"dismissed"}}
	}
	err := tx.DismissSubmission(ctx, id)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrSubmissionNotFound):
		return &SetError{Type: setNotFound}
	case errors.Is(err, store.ErrSubmissionNotDismissable):
		return &SetError{Type: setInvalidProps, Properties: []string{"dismissed"},
			Description: "only a send that failed can be dismissed; a pending one can be cancelled instead"}
	default:
		return &SetError{Type: setForbidden, Description: err.Error()}
	}
}

// applyOnSuccess runs the RFC 8621 §7.5 hooks: after a successful create, patch
// (or destroy) the Email that was sent. This is how "$draft goes away and the
// message appears in Sent" happens atomically with the send being scheduled —
// the client does not have to make a second, racy Email/set call.
func (a *API) applyOnSuccess(ctx context.Context, tx *store.UserTx, args submissionSetArgs, createdEmails map[string]ulid.ULID, res *setResult) error {
	for ref, patch := range args.OnSuccessUpdateEmail {
		emailID, ok := onSuccessTarget(ref, createdEmails)
		if !ok {
			continue // the submission was not created; the hook does not fire
		}
		up, serr := parseEmailPatch(ctx, tx, patch)
		if serr != nil {
			res.NotUpdated[emailID.String()] = *serr
			continue
		}
		found, _, err := tx.ApplyEmailUpdate(ctx, emailID, up)
		if err != nil {
			return err
		}
		if !found {
			res.NotUpdated[emailID.String()] = SetError{Type: setNotFound}
		}
	}
	for _, ref := range args.OnSuccessDestroyEmail {
		emailID, ok := onSuccessTarget(ref, createdEmails)
		if !ok {
			continue
		}
		reclaim, err := tx.RetireDraft(ctx, emailID)
		if err != nil {
			if errors.Is(err, store.ErrMessageNotFound) || errors.Is(err, store.ErrNotADraft) {
				res.NotDestroyed[emailID.String()] = SetError{Type: setNotFound}
				continue
			}
			return err
		}
		// The blob FILES of the retired draft, inside this transaction (#300) —
		// see reclaimBlobFiles for why the ordering matters.
		if err := a.reclaimBlobFiles(ctx, tx.UserID(), reclaim); err != nil {
			return err
		}
	}
	return nil
}

// onSuccessTarget resolves a "#creationId" hook key to the Email a submission
// sent. A key naming a submission that was NOT created resolves to nothing, so
// a failed send never mutates the draft it failed to send.
func onSuccessTarget(ref string, createdEmails map[string]ulid.ULID) (ulid.ULID, bool) {
	id, ok := createdEmails[trimHash(ref)]
	return id, ok
}

func trimHash(s string) string {
	if len(s) > 0 && s[0] == '#' {
		return s[1:]
	}
	return s
}
