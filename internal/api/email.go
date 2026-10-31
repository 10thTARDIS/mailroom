// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ml"
	"mailroom/internal/pgpmime"
	"mailroom/internal/search"
	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// Email method handlers (RFC 8621 §4). Reads (get/query) and the reviewed
// Email/set mutation path. Labels are keywords (spec §13); the set path routes a
// keyword patch to the right canonical store (system flag, label, or raw
// keyword) and the store appends the single Email change_log row per update.

// systemKeywordFlags maps the JMAP system keyword tokens Email/set recognizes to
// the EmailUpdate flag pointer it drives.
var systemKeywords = map[string]bool{
	store.KeywordSeen:     true,
	store.KeywordFlagged:  true,
	store.KeywordAnswered: true,
	store.KeywordDraft:    true,
	store.KeywordJunk:     true,
}

// maxKeywordLen is RFC 8621 §4.1.1's bound on a keyword.
const maxKeywordLen = 255

// validJMAPKeyword is RFC 8621 §4.1.1's keyword syntax: 1–255 characters, each
// in %x21–%x7E (no space, no control characters, ASCII only), none of
// `( ) { ] % * " \` (the IMAP atom-specials). Email/set refuses anything else,
// and the refusal is load-bearing (#49 review): a keyword is written back as
// `UID STORE +FLAGS (<keyword>)`, and `\Draft` there is the IMAP SYSTEM flag,
// which the next sync reads back as is_draft — a draft made by a route the
// $draft checks never see, whose next ordinary delete erases it everywhere.
func validJMAPKeyword(k string) bool {
	if k == "" || len(k) > maxKeywordLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c < 0x21 || c > 0x7e {
			return false
		}
		switch c {
		case '(', ')', '{', ']', '%', '*', '"', '\\':
			return false
		}
	}
	return true
}

// invalidKeyword is the SetError for a keyword validJMAPKeyword refuses.
func invalidKeyword(name string) *SetError {
	return &SetError{Type: setInvalidProps, Properties: []string{"keywords/" + name},
		Description: "not a valid keyword (RFC 8621 §4.1.1): 1–255 printable ASCII characters, " +
			"none of ( ) { ] % * \" \\ — and IMAP system flags such as \\Draft are not keywords"}
}

// jmapDate renders a time as a JMAP UTCDate (RFC 8620 §1.4, "...Z", no fraction).
func jmapDate(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// jmapDateOrNil renders an optional timestamp as a UTCDate or JSON null. Used
// for properties where "never happened" is meaningful state rather than an
// error — pinnedAt on an unpinned message, say.
func jmapDateOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return jmapDate(*t)
}

// getResult is the RFC 8620 §5.1 Foo/get response shape.
type getResult struct {
	AccountID string           `json:"accountId"`
	State     string           `json:"state"`
	List      []map[string]any `json:"list"`
	NotFound  []string         `json:"notFound"`
}

type emailGetArgs struct {
	AccountID  string    `json:"accountId"`
	IDs        *[]string `json:"ids"`
	Properties []string  `json:"properties"`
	bodyArgs
}

func (a *API) emailGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if err := json.Unmarshal(rawArgs, &args.bodyArgs); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if args.IDs == nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "Email/get requires ids"}
	}

	valid, notFound, requested := parseIDs(*args.IDs)
	props := defaultEmailProperties
	if args.Properties != nil {
		props = make(map[string]bool, len(args.Properties)+1)
		for _, p := range args.Properties {
			props[p] = true
		}
		props["id"] = true // always present (RFC 8620 §5.1)
	}

	var (
		state int64
		byID  = make(map[ulid.ULID]store.EmailView)
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		views, err := tx.GetEmails(ctx, valid)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		// An account-restricted API key sees only its accounts' mail: the rest
		// is notFound, exactly like another user's (#49, ADR-042 §5).
		visible, err := keyVisibleMessages(ctx, tx, valid)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		for _, v := range views {
			if visible != nil && !visible[v.ID] {
				continue
			}
			byID[v.ID] = v
		}
		state, err = tx.ChangeState(ctx, store.ObjectEmail)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	// The blob reads happen AFTER the transaction has committed, and only for
	// the properties that need them: decrypting and MIME-parsing a message is
	// filesystem + crypto work that has no business holding a Postgres
	// transaction (and its row locks) open.
	// ⚠ THE SCAN STATES ARE RESOLVED BEFORE THE BODIES, because bodyValues needs
	// them (#413). fetchAllBodyValues emits every text/* leaf INCLUDING one with
	// Content-Disposition: attachment — which IS projected and scannable — so a
	// flagged .txt came back verbatim as a JSON string through a door the
	// download route refuses. One batch read, shared by the gate below and by the
	// attachmentScan property, so the two cannot disagree about what is blocked.
	scanIDs := make([]ulid.ULID, 0, len(byID))
	for id := range byID {
		scanIDs = append(scanIDs, id)
	}
	scans, mErr := a.resolveAttachmentScans(ctx, userID, scanIDs, props, args.bodyArgs)
	if mErr != nil {
		return nil, mErr
	}
	bodies, mErr := a.loadBodies(ctx, userID, props, args.bodyArgs, byID, scans)
	if mErr != nil {
		return nil, mErr
	}

	list := make([]map[string]any, 0, len(requested))
	// The category tab (#228) is derived rather than stored, so it is resolved in
	// one batch here — and only when the client actually asked for it.
	resolvedIDs := make([]ulid.ULID, 0, len(byID))
	for id := range byID {
		resolvedIDs = append(resolvedIDs, id)
	}
	catTabs, mErr := a.resolveCategoryTabs(ctx, userID, resolvedIDs, props)
	if mErr != nil {
		return nil, mErr
	}
	// The desk's area (#442) is likewise derived rather than stored, and
	// likewise opt-in — see area_email.go's resolveAreas.
	areaRes, areaProvs, mErr := a.resolveAreas(ctx, userID, resolvedIDs, props)
	if mErr != nil {
		return nil, mErr
	}
	// The drawer sweep's archive mark (#443) is likewise derived and opt-in —
	// see drawer_email.go's resolveDrawerArchivedAt.
	drawerAt, mErr := a.resolveDrawerArchivedAt(ctx, userID, resolvedIDs, props)
	if mErr != nil {
		return nil, mErr
	}
	// Day-one setup's archive mark (#446): the same derived, opt-in shape — see
	// setup_email.go's resolveDeskArchivedAt, and its ⚠ on why this is a SECOND
	// property beside drawerArchivedAt rather than a widening of it.
	deskArchivedAt, mErr := a.resolveDeskArchivedAt(ctx, userID, resolvedIDs, props)
	if mErr != nil {
		return nil, mErr
	}
	// The user's own placement across the desk's line (#464): read-only and
	// opt-in in the same shape — see line_side_email.go's resolveLineSides.
	lineSides, mErr := a.resolveLineSides(ctx, userID, resolvedIDs, props)
	if mErr != nil {
		return nil, mErr
	}
	// The sender's known-domain index (#397): one query for the whole request,
	// never one per message.
	senderIdx, mErr := a.resolveSenderIndex(ctx, userID, props)
	if mErr != nil {
		return nil, mErr
	}
	// Calendar invitations (#725): the other versions of, and the user's own
	// answers to, every invitation in the batch — one transaction, two
	// queries, and nothing at all unless calendarInvite was asked for.
	calCtx, mErr := a.resolveCalendarContext(ctx, userID, props, bodies)
	if mErr != nil {
		return nil, mErr
	}

	for _, req := range requested {
		id, ok := req.parsed()
		if !ok {
			continue
		}
		ev, ok := byID[id]
		if !ok {
			notFound = append(notFound, req.raw)
			continue
		}
		obj := emailObject(ev, props, bodies[id])
		if catTabs != nil {
			r, found := catTabs[id]
			obj["categoryTab"] = emailCategoryTab(r, found)
		}
		if areaRes != nil {
			r, found := areaRes[id]
			if props["area"] {
				obj["area"] = emailAreaProperty(r, found)
			}
			if props["areaProvenance"] {
				obj["areaProvenance"] = emailAreaProvenanceProperty(r, found, areaProvs[id])
			}
		}
		if drawerAt != nil {
			at, found := drawerAt[id]
			obj["drawerArchivedAt"] = emailDrawerArchivedAtProperty(at, found)
		}
		if deskArchivedAt != nil {
			at, found := deskArchivedAt[id]
			obj["deskArchivedAt"] = emailDeskArchivedAtProperty(at, found)
		}
		if lineSides != nil {
			side, found := lineSides[id]
			obj["lineSide"] = emailLineSideProperty(side, found)
		}
		if scans != nil {
			// Present as null for a message with no projected parts, rather
			// than absent: a client that asked for the property gets an answer
			// for every message it asked about.
			obj["attachmentScan"] = attachmentScanObject(scans[id])
		}
		if senderIdx != nil {
			obj["senderWarnings"] = senderWarningsObject(ev, bodies[id], senderIdx)
		}
		if calCtx != nil {
			// Present as null for a message with no calendar, so a client that
			// asked gets an answer for every message.
			obj["calendarInvite"] = calendarInviteObject(ev, bodies[id], calCtx)
		}
		list = append(list, obj)
	}
	return mustMarshal(getResult{
		AccountID: userID.String(),
		State:     encodeState(state),
		List:      list,
		NotFound:  notFound,
	}), nil
}

// loadBodies decrypts and parses the message bytes for the requested views —
// but ONLY when the client asked for a property that can only come from them
// (rawProperties). This is the whole performance story of Email/get: a list view
// naming its properties never gets here, so scrolling a mailbox costs zero blob
// reads and zero MIME parses, while a reading pane pays for exactly the messages
// it opened.
//
// ⚠ A MESSAGE WHOSE BLOB CANNOT BE READ COSTS ONE MESSAGE, NOT THE BATCH (#419).
//
// This used to fail the whole METHOD, and the reasoning behind it was sound as
// far as it went: blob.Get verifies the AEAD tag and the content hash as it
// drains, so a read error means the bytes are corrupt or tampered with, and
// "answering with a body-less Email would be quietly serving a lie". The word
// carrying that argument is QUIETLY. The remedy for a quiet lie is to say
// something, not to refuse fifty messages.
//
// ⚠ AND THE COST WAS NOT PROPORTIONAL TO THE DAMAGE. One unreadable message made
// an entire Email/get fail, so the reading pane lost the other forty-nine and
// nothing said which one was at fault. On a young corpus — all
// ingested by the same code in the same few weeks — the probability of any
// message being unreadable is near zero. On a decades-old imported mailbox, mail
// from a dozen clients with truncated fetches and pre-format-change blobs, it is
// near one.
//
// So the failure degrades per message: the ones that parsed come back with
// bodies, and the one that did not comes back MARKED. `bodyUnavailable` is what
// keeps this from being the silent version — the same "an absence is not an
// answer" rule this codebase applies to scan verdicts and tier counts. The
// server logs the underlying error; the client is told there is one.
//
// ⚠ IT DOES NOT DEGRADE INTO SILENCE ON A TOTAL OUTAGE EITHER. If the blob store
// is entirely down every message comes back marked, which is honest and visible,
// rather than an empty reading pane. What must never happen is a message whose
// body simply vanishes with no property saying so.
func (a *API) loadBodies(ctx context.Context, userID ulid.ULID, props map[string]bool, args bodyArgs,
	views map[ulid.ULID]store.EmailView, scans map[ulid.ULID][]store.PartScanState) (map[ulid.ULID]*emailBody, *MethodError) {

	need := false
	for p := range props {
		if rawProperties[p] {
			need = true
			break
		}
	}
	if !need || len(views) == 0 {
		return nil, nil
	}
	if a.raw == nil {
		// No blob store is configured, so this instance holds no message bytes
		// at all (nothing can have been ingested). Serve what Postgres knows and
		// leave the body properties out rather than failing every Email/get.
		return nil, nil
	}

	// Who is "self" for the authResults notice (#766): ONE read for the whole
	// request, and only when the notice can be shown at all.
	var self *authSelfFacts
	if props["authResults"] && len(a.trustedAuthservIDs) > 0 {
		var mErr *MethodError
		if self, mErr = a.resolveAuthSelf(ctx, userID); mErr != nil {
			return nil, mErr
		}
	}

	// Which messages have a PROVIDER-set received_at (#766): only those may be
	// judged against a date-bounded authserv-id entry. Archive-imported rows
	// have none, and their received_at can be the sender's Date header.
	var dated map[ulid.ULID]bool
	if hasBoundedAuthserv(a.trustedAuthservIDs) {
		ids := make([]ulid.ULID, 0, len(views))
		for id := range views {
			ids = append(ids, id)
		}
		if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
			var err error
			if dated, err = tx.ProviderDated(ctx, ids); err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			return nil
		}); mErr != nil {
			return nil, mErr
		}
	}

	// Mailroom's own DKIM verdicts (#776): ONE batched read for the request, and
	// only when the notice or the sender warnings can use them.
	var local map[ulid.ULID]store.LocalDKIM
	if props["authResults"] || props["senderWarnings"] {
		ids := make([]ulid.ULID, 0, len(views))
		for id := range views {
			ids = append(ids, id)
		}
		if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
			var err error
			if local, err = tx.LocalDKIMFor(ctx, ids); err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			return nil
		}); mErr != nil {
			return nil, mErr
		}
	}

	out := make(map[ulid.ULID]*emailBody, len(views))
	for id, ev := range views {
		ba := args
		if v, ok := local[id]; ok {
			v := v
			ba.localDKIM = &v
		}
		// Received time and self-ness are facts about THIS stored message.
		ba.received = store.TrustTime(ev.ReceivedAt, dated[id])
		ba.authSelf = self.isSelf(ev)
		// The trusted authserv-ids are deployment configuration, never request
		// input: set here rather than decoded, so no client can name its own.
		ba.trustedAuthservIDs = a.trustedAuthservIDs
		ba.wantCalendar = props["calendarInvite"]
		if a.proxy != nil && a.proxy.cfg.Enabled {
			// Per MESSAGE: the reference has to name the message it came from,
			// because that is what the endpoint resolves the URL out of.
			ba.proxyRewrite = proxyRewriterFor(userID, id)
		}
		// Per MESSAGE again: which parts are refused is a fact about this
		// message, and bodyValues consults it by partId.
		if ba.gatesBodyValues() {
			ba.blockedParts = blockedPartSet(scans[id])
		}
		raw, err := a.raw.ReadRaw(ctx, userID, ev.ContentHash)
		if err != nil {
			// ⚠ THE ID IS IN THE LOG, NOT IN THE RESPONSE. Which message is
			// corrupt is an operator question — the answer is a blob path and
			// an AEAD failure — and the client already learns the actionable
			// half from bodyUnavailable on that message.
			slog.ErrorContext(ctx, "cannot read a message body; serving the rest of the batch and "+
				"marking this one unavailable",
				"user_id", userID.String(), "message_id", id.String(), "err", err)
			out[id] = &emailBody{unreadable: true}
			continue
		}
		// RFC 3156 encrypted mail (#47 6b, ADR-037): decrypted on this request
		// path only, only when a body property was asked for, and never stored.
		if pgpmime.IsEncrypted(raw) {
			out[id] = a.buildEncryptedBody(ctx, userID, id, ev, raw, ba, props)
			continue
		}
		b := buildEmailBody(raw, ev.ContentHash, ba)
		if props["pgpSignature"] {
			// A PGP/MIME signed plaintext message, verified here and not stored.
			b.pgpSignature = a.pgpSignatureFor(ctx, userID, raw, raw)
		}
		out[id] = b
	}
	return out, nil
}

// ImportantThreshold is the priority score at or above which a message is
// reported as isImportant on the JMAP Email object (urn:mailroom:triage). It is
// a presentation cutoff over the model's [0,1] score, deliberately tunable and
// kept in ONE place: 0.6 is a mild lean toward "important" (a fresh/untrained
// model scores ~0.5, so nothing is important until a trained model actually
// pushes a message above the line). A client that wants the raw number reads
// priorityScore instead.
const ImportantThreshold = 0.6

// isSnoozed reports whether a wake time is still in the future — the definition
// of "snoozed" (spec §6, #51). One function so the JMAP property and any future
// caller cannot drift apart on the boundary case.
func isSnoozed(until *time.Time) bool {
	return until != nil && until.After(time.Now())
}

// emailObject renders an EmailView as a JMAP Email object, honoring a requested
// property subset (id is always present, RFC 8620 §5.1). body carries everything
// that could only be read from the message bytes (nil when the caller asked for
// no such property, or when this instance has no blob store).
func emailObject(ev store.EmailView, props map[string]bool, body *emailBody) map[string]any {
	full := map[string]any{
		"id":            ev.ID.String(),
		"blobId":        hex.EncodeToString(ev.ContentHash),
		"threadId":      ev.ThreadID.String(),
		"mailboxIds":    idSetTrue(ev.MailboxIDs),
		"keywords":      keywordSet(ev.Keywords),
		"size":          ev.Size,
		"receivedAt":    jmapDate(ev.ReceivedAt),
		"subject":       ev.Subject,
		"preview":       ev.Preview,
		"hasAttachment": ev.HasAttachment,
		// Threading headers (RFC 8621 §4.1.3). A client composing a reply reads
		// these off the parent and hands them back on the draft it creates,
		// which is how In-Reply-To/References come out right (spec §5).
		"messageId":  msgIDList(ev.MessageIDHint),
		"inReplyTo":  msgIDList(ev.InReplyTo),
		"references": nonNilStrings(ev.References),
		// Mailroom extension (urn:mailroom:compose), spec §5's alias story:
		// deliveredTo is the exact address this message was delivered to — what
		// a reply should come FROM — and identityId is the verified identity it
		// resolved to (null when none did, e.g. an alias at a domain that is not
		// verified for this user).
		"deliveredTo": nilIfEmpty(ev.DeliveredTo),
		"identityId":  nilIfZero(ev.AddressedIdentityID),
		// Mailroom extension (urn:mailroom:triage), spec §8's T1 priority scoring:
		// priorityScore is the model's [0,1] priority (null when the message has
		// not been scored), and isImportant is the same score thresholded at
		// ImportantThreshold for a client that just wants the boolean.
		//
		// "OPT-IN" HERE MEANS ONE THING ONLY: they are not in
		// defaultEmailProperties, so a client sees them only when it NAMES them in
		// `properties`, and they cost a client that never asks exactly nothing.
		// It does NOT mean the triage capability withholds them — this comment said
		// so until Forgejo #310 and it was never true. emailGet builds its property
		// set from the client's `properties` array alone; methodFn is not handed
		// `using` at all (request.go), so a client that names priorityScore having
		// declared only urn:ietf:params:jmap:mail receives priorityScore. Such a
		// client is non-conformant (RFC 8620 §3.3), not privileged. What scopes the
		// value is the authenticated user id and RLS, which is a real boundary; the
		// capability list is a declaration of intent, which is not.
		"priorityScore": floatOrNil(ev.PriorityScore),
		"isImportant":   isImportant(ev.PriorityScore),
		// priorityReasons (#34) is the human-readable WHY behind the score: the
		// stored per-feature contributions, each rendered { label, weight, polarity }.
		// null when the message is unscored or its score carries no reasons. Opt-in
		// in the same sense as priorityScore above — absent from
		// defaultEmailProperties, and not withheld by anything else.
		"priorityReasons": priorityReasons(ev.TopFeatures),
		// labelProvenance (urn:mailroom:rules, #155) answers "why is this label
		// here?" — for each label on the message, who put it there and, if a rule
		// did, which one by name. Without it an automatically-applied label is
		// indistinguishable from one the user applied and forgot, and the rule
		// that did it is unfindable except by reading every rule.
		//
		// Opt-in, like the triage and organize properties: not in
		// defaultEmailProperties, so it costs a client nothing until it asks —
		// which is the ONLY thing "opt-in" means for any property in this map
		// (Forgejo #310; see the note on priorityScore above).
		"labelProvenance": labelProvenance(ev.LabelProvenance),
		// labelIds (urn:mailroom:labels, #704) is the id-set of EVERY label on
		// the message — mirrored and local-only alike — in the same
		// { id: true } shape as mailboxIds, so `labelIds/<id>` is a JSON
		// Pointer into it and Email/set can patch it (parseEmailPatch).
		//
		// It exists because `keywords` cannot carry a local-only label: such a
		// label has no atom by design (ADR-031 — every area's own label is
		// one), so it was on the message, deciding its area, and invisible to
		// every client. A mirrored label appears in BOTH places; a local one only
		// here. Opt-in like the rest of this map — absent from
		// defaultEmailProperties, and not withheld by the capability (#310).
		"labelIds": idSetTrue(ev.LabelIDs),
		// suggestedLabels (urn:mailroom:rules, #169) are the classifier's OPEN
		// QUESTIONS: labels it considered and was not confident enough to apply.
		//
		// They are not a weaker form of label. Nothing here affects keywords,
		// mailboxes or what any other client sees — a suggestion is a question
		// waiting for a person, and answering it goes through Email/set like any
		// other labelling. Null when the model decided, had nothing to say, or a
		// human already answered.
		"suggestedLabels": suggestedLabels(ev.SuggestedLabels),
		// listId (urn:mailroom:rules, #155) is the List-Id value, or null when
		// this is not list mail. It is what "create a rule from this mailing list"
		// keys on: the list itself, not the domain that sends it — the difference
		// between a rule that catches a retailer's newsletter and one that also
		// catches their receipts. Opt-in, like the rest of the extension set.
		"listId": nilIfEmpty(ev.ListID),
		// unsubscribe (urn:mailroom:unsubscribe, #58) is List-Unsubscribe parsed
		// into the two things a client can offer: an https link for the user's own
		// browser to open, and a mailto split into the parts a compose needs.
		// Null when the message carries no header or nothing usable in it.
		//
		// SURFACING ONLY — Mailroom never sends the unsubscribe itself and does not
		// implement RFC 8058 one-click. See unsubscribe.go for why that boundary is
		// deliberate, and what real mail actually looks like.
		"unsubscribe": parseListUnsubscribe(ev.ListUnsubscribe),
		// Mailroom extension (urn:mailroom:organize), spec §6's local-first
		// organization: isPinned is the boolean most clients want, pinnedAt the
		// timestamp that makes a pinned-first ordering stable. Opt-in like the
		// triage pair — not in defaultEmailProperties, and, like the triage pair,
		// not gated by its capability either: urn:mailroom:organize gates the
		// Thread/set METHOD and nothing else (Forgejo #310, capability.go mayCall).
		"isPinned": ev.PinnedAt != nil,
		"pinnedAt": jmapDateOrNil(ev.PinnedAt),
		// Snooze, same capability (#51). snoozedUntil is WHEN the message comes
		// back; isSnoozed is that compared with now, which is the comparison every
		// client would otherwise have to make (and get subtly wrong around the wake
		// instant). A message whose wake time has passed but whose wake job has not
		// run yet reads as NOT snoozed — the clock is what the user asked for; the
		// job is only what makes it durable.
		"isSnoozed":    isSnoozed(ev.SnoozedUntil),
		"snoozedUntil": jmapDateOrNil(ev.SnoozedUntil),
		// When a snooze last brought this message back (#152). Read-only, and the
		// reason a client can explain why a week-old message is sitting at the top
		// of the inbox — which without it reads as a bug.
		"returnedAt": jmapDateOrNil(ev.SnoozeWokeAt),
		// flaggedAt (#444): the in-tray's "waiting since" — null when the message
		// is not flagged. Read-only: the flag itself is set through the $flagged
		// keyword, and the SERVER stamps the time, so two clients flagging the
		// same message a minute apart cannot disagree about when it happened.
		// Opt-in, same posture as the organize pair above — not in
		// defaultEmailProperties, and not gated by any capability either
		// (Forgejo #310; see the note on priorityScore above).
		"flaggedAt": jmapDateOrNil(ev.FlaggedAt),
	}
	if ev.SentAt != nil {
		full["sentAt"] = jmapDate(*ev.SentAt)
	} else {
		full["sentAt"] = nil
	}
	if ev.FromAddress != "" {
		full["from"] = []map[string]any{{"name": nilIfEmpty(ev.FromName), "email": ev.FromAddress}}
	} else {
		full["from"] = nil
	}
	// The RFC 8621 §4.1.2/§4.1.4 properties that live in the message BYTES:
	// the full recipient lists (without which a client cannot reply-all) and the
	// body itself (without which it cannot show the message at all). A nil body
	// means they were not asked for — they are then absent, not empty, because
	// "we did not fetch this" and "this message has no recipients" are different
	// claims.
	if body != nil {
		full["sender"] = nilIfNoAddrs(body.sender)
		full["to"] = nilIfNoAddrs(body.to)
		full["cc"] = nilIfNoAddrs(body.cc)
		full["bcc"] = nilIfNoAddrs(body.bcc)
		full["replyTo"] = nilIfNoAddrs(body.replyTo)
		full["bodyStructure"] = body.structure
		full["textBody"] = body.textBody
		full["htmlBody"] = body.htmlBody
		full["attachments"] = body.attachment
		full["bodyValues"] = body.values
		// ⚠ authTrusted, NOT the permissive parse (#625). An Authentication-
		// Results header is attacker-writable unless its authserv-id is
		// checked (RFC 8601 §5), so a sender could otherwise add their own
		// `dmarc=pass` header and cancel the provider's genuine `dmarc=fail`
		// in what this notice shows. See body.go's authTrusted doc and
		// buildEmailBody. With MAILROOM_AUTHSERV_IDS unset there is nothing to
		// trust, so the property is null: the client shows no notice at all.
		// NOT authResultsObject over an empty parse — that renders
		// `present: false`, which the client shows as "unauthenticated" on
		// every remote message (see body.go's authConfigured).
		// #766: untrusted-only mail is silent too, and "unauthenticated" is
		// kept for inbound non-self mail with no header at all; see
		// authNoticeObject.
		full["authResults"] = authNoticeObject(body.authTrusted,
			body.authConfigured, ev.LocallyAuthored, body.authSelf, body.localDKIM)
		// Mailroom extension (#419): the message exists and its bytes could not
		// be read, so every property above is empty for a REASON rather than
		// because the message is empty. Present only when body properties were
		// asked for at all — saying nothing about bodies nobody requested.
		full["bodyUnavailable"] = body.unreadable
		// urn:mailroom:pgp extensions (#47 6b, ADR-037). decrypted is null when
		// no decryption was attempted, false for "could not be decrypted" (no
		// detail, by design), true when the body above is the plaintext.
		full["encrypted"] = body.encrypted
		if body.decrypted != nil {
			full["decrypted"] = *body.decrypted
		} else {
			full["decrypted"] = nil
		}
		if body.pgpSignature != nil {
			full["pgpSignature"] = body.pgpSignature
		} else {
			full["pgpSignature"] = nil
		}
	}

	// A nil property set means "every property this object has" — what Email/set
	// returns for an object it just created (RFC 8620 §5.3). Email/get never
	// passes nil: an omitted `properties` argument is the RFC 8621 §4.2 DEFAULT
	// set, which is a different, narrower thing.
	if props == nil {
		return full
	}
	out := map[string]any{"id": ev.ID.String()}
	for k, v := range full {
		if props[k] {
			out[k] = v
		}
	}
	return out
}

// floatOrNil renders a *float64 as its value, or JSON null when the pointer is
// nil — so an UNSCORED message reports priorityScore: null (distinct from a real
// score of 0).
func floatOrNil(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// isImportant thresholds a message's priority score at ImportantThreshold. An
// unscored message (nil) is never important.
func isImportant(score *float64) bool {
	return score != nil && *score >= ImportantThreshold
}

// priorityReasons renders the stored top_features JSON (a machine-readable
// []ml.FeatureContribution — feature KEYS and signed weights) as the JMAP
// priorityReasons property (#34): a JSON array of {label, weight, polarity},
// where label is ml.FeatureLabel(key) and polarity is "up" when the weight
// pushed the message TOWARD important, "down" when it pushed it away.
//
// It returns nil (JSON null) when the message is unscored (nil payload) or the
// score carries no reasons (an empty / `[]` payload, or malformed JSON) — so
// "no reasons" and "reasons" are distinguishable, never an empty array. Keeping
// KEYS in storage and applying LABELS here is deliberate: label wording can
// change without rewriting any stored triage row.
func priorityReasons(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	contribs, ok := decodeContributions(raw)
	if !ok || len(contribs) == 0 {
		return nil
	}
	return contribs
}

// suggestionReasons renders a stored label suggestion's reasons (#176). Same
// {label, weight, polarity} shape as priorityReasons — one explanation contract
// for both models — but it PRESERVES THE EMPTY ARRAY, and that is the whole
// reason it is not the same function.
//
// The two columns store different things. triage_scores.top_features is NOT NULL
// DEFAULT '[]', so for it "empty" and "absent" are the same state and collapsing
// them to null is right. label_suggestions.reasons is nullable precisely so they
// are DIFFERENT states (migration 00019):
//
//	null → no record of why: written before the column existed, or by a model
//	       that cannot explain itself at all.
//	[]   → the model explained, and nothing cleared the noise floor.
//
// Rendering both as null would make the client say "no record of why" about a
// model that gave a full account of itself and had nothing to report — a claim
// the server never made, and exactly what the migration spends three paragraphs
// forbidding. Malformed JSON is the one case that still reads as null: we do not
// know what it said, which IS "no record".
func suggestionReasons(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	contribs, ok := decodeContributions(raw)
	if !ok {
		return nil
	}
	return contribs
}

// decodeContributions maps a stored []ml.FeatureContribution payload onto the
// wire shape, or reports !ok when it cannot be read. Keeping KEYS in storage and
// applying LABELS here is deliberate: wording can change without rewriting any
// stored row.
//
// Text is the one exception, and it exists for a reason the key vocabulary
// cannot cover (#242). ml.FeatureLabel(key) works at all because the T1
// priority model's features are a FIXED, small vocabulary — "a reply in a
// thread you're part of" is the same sentence every time "reply:1" shows up, so
// the label can live in code and only the key needs to travel. A rule conjunct
// has no such vocabulary: "alias is sub@example.com" is DATA the user typed
// into a rule editor, not a member of a closed set FeatureLabel could ever be
// made to enumerate. There is no key to map it through, so the rule inducer
// writes the literal phrase into Text and this function renders that verbatim
// instead of asking FeatureLabel to recognize a key it was never given. A
// contribution with no Text — every T1 priority contribution, today — still
// goes through FeatureLabel exactly as before: this is an additional path, not
// a replacement of the keys-in-storage design.
func decodeContributions(raw []byte) ([]map[string]any, bool) {
	var contribs []ml.FeatureContribution
	if err := json.Unmarshal(raw, &contribs); err != nil {
		return nil, false
	}
	out := make([]map[string]any, 0, len(contribs))
	for _, c := range contribs {
		polarity := "up"
		if c.Weight < 0 {
			polarity = "down"
		}
		label := c.Text
		if label == "" {
			label = ml.FeatureLabel(c.Key)
		}
		out = append(out, map[string]any{
			"label":    label,
			"weight":   c.Weight,
			"polarity": polarity,
		})
	}
	return out, true
}

// labelProvenance renders who applied each of a message's labels (#155), or
// null when it has none.
//
// The rule is reported as an OBJECT rather than a bare id so a client can say
// "labelled by Wayfair promos" from this property alone, without fetching every
// rule to resolve a name — and can still say something useful ("a rule that no
// longer exists") when the rule was deleted and the id went NULL with it.
func labelProvenance(ps []store.LabelProvenance) any {
	if len(ps) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		entry := map[string]any{
			"keyword":   p.KeywordAtom,
			"appliedBy": p.AppliedBy,
			"rule":      nil,
		}
		if p.RuleID != "" {
			entry["rule"] = map[string]any{"id": p.RuleID, "name": p.RuleName}
		}
		out = append(out, entry)
	}
	return out
}

// suggestedLabels renders the classifier's open questions (#169), or null when
// there are none.
//
// Only the label ID travels, not its name: unlike rules, labels are part of the
// client's ordinary sync, so it already holds every name and colour. Sending
// them again would be a second copy to disagree with the first the moment a
// label is renamed.
//
// REASONS ride each suggestion (#176), through the SAME renderer as
// priorityReasons and therefore in the same {label, weight, polarity} shape. Two
// models, two panels, one payload contract — a client that can explain a
// priority score can explain a category with no new code, and the two cannot
// drift into disagreeing about what an explanation looks like.
//
// A null `reasons` means the suggestion carries no record of why: written before
// migration 00019, or by a model that could not explain itself. It is NOT an
// empty array, and a client must not render it as "no reasons found" — that
// would be a claim the server never made.
//
// `source` is what #242 adds, and it exists because `confidence` stopped
// meaning one single thing the moment a suggestion could come from the rule
// inducer instead of the T1 classifier. A rule has no calibrated probability —
// there is no training procedure behind it to calibrate — so the rule path
// writes an EVIDENCE RANK into the same column instead: a monotone function of
// how many of the user's own filings back the candidate. It has to be the same
// column, because label_suggestions.confidence is what the needs-a-category
// query orders on (max(ls.confidence) DESC NULLS LAST, jmap_email.go), and a
// second ranking column would mean two queues instead of one merged by "most
// worth a person's attention first". The number this produces is real and
// correctly ordered; it is simply not a probability, and rendering it as one —
// a bare "67%" — would claim a precision the rule path never earned. `source`
// is what lets a client tell the two apart and render each honestly, which is
// exactly what web/src/app/ui/NeedsCategory.tsx's SuggestionStrip does: a
// percentage for "model", and for "rule" the evidence phrase the rule path
// already wrote as the last entry of that suggestion's own `reasons`.
func suggestedLabels(ss []store.LabelSuggestion) any {
	if len(ss) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, map[string]any{
			"labelId":    s.LabelID.String(),
			"confidence": s.Confidence,
			"reasons":    suggestionReasons(s.Reasons),
			"source":     s.Source,
		})
	}
	return out
}

// nilIfNoAddrs renders an empty address list as JSON null (an absent header),
// never as [].
func nilIfNoAddrs(in []map[string]any) any {
	if len(in) == 0 {
		return nil
	}
	return in
}

// idSetTrue renders a ULID slice as a JMAP {id: true} set.
func idSetTrue(ids []ulid.ULID) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id.String()] = true
	}
	return out
}

// keywordSet renders keyword tokens as a JMAP {keyword: true} set, de-duped.
func keywordSet(keywords []string) map[string]bool {
	out := make(map[string]bool, len(keywords))
	for _, k := range keywords {
		out[k] = true
	}
	return out
}

// queryResult is the RFC 8620 §5.5 Foo/query response shape.
type queryResult struct {
	AccountID           string   `json:"accountId"`
	QueryState          string   `json:"queryState"`
	CanCalculateChanges bool     `json:"canCalculateChanges"`
	Position            int      `json:"position"`
	Total               int      `json:"total"`
	IDs                 []string `json:"ids"`
	// Limit is RFC 8620 §5.5's "limit enforced by the server", present only
	// when the server set one other than the request's (an API key's query,
	// keyQueryLimit).
	Limit *int `json:"limit,omitempty"`

	// SearchCapped (urn:mailroom:search) is present only when the search index
	// answered with as many hits as it returns — its hit limit, which is the
	// value — so the matches it handed Postgres may be incomplete: `total` and
	// the ids then count only those. A client shows "at least" or "first N"
	// rather than presenting the number as every match (#722).
	SearchCapped int `json:"searchCapped,omitempty"`
}

type emailQueryArgs struct {
	AccountID      string           `json:"accountId"`
	Filter         json.RawMessage  `json:"filter"`
	Sort           []sortComparator `json:"sort"`
	Position       int              `json:"position"`
	Limit          *int             `json:"limit"`
	CalculateTotal bool             `json:"calculateTotal"`
	// CollapseThreads is RFC 8621 §4.4.4: keep only the first Email of each
	// Thread in the query's own sort order, for both the page and the total.
	// It is a top-level Email/query argument, not a FilterCondition, so it is
	// decoded here rather than through emailFilter/emailFilterConditions.
	CollapseThreads bool `json:"collapseThreads"`
	// SearchLimit is a Mailroom extension (urn:mailroom:search, #769): how many
	// hits the search index may return as candidates for a text filter. Absent
	// means search.DefaultLimit. A client enumerating every match for a bulk
	// action (select all matching, "move all from this sender") sends
	// search.MaxLimit; ordinary list views leave it off, because a larger limit
	// costs time on the index side. Clamped to [DefaultLimit, MaxLimit]; negative
	// is invalidArguments.
	SearchLimit *int `json:"searchLimit"`
}

type sortComparator struct {
	Property    string `json:"property"`
	IsAscending *bool  `json:"isAscending"`
}

type emailFilter struct {
	InMailbox   *string `json:"inMailbox"`
	InThread    *string `json:"inThread"`
	InAccount   *string `json:"inAccount"`
	HasKeyword  string  `json:"hasKeyword"`
	NotKeyword  string  `json:"notKeyword"`
	Before      string  `json:"before"`
	After       string  `json:"after"`
	IsSnoozed   *bool   `json:"isSnoozed"`
	IsImportant *bool   `json:"isImportant"`
	// NeedsCategory selects mail the classifier declined to judge (#169).
	NeedsCategory *bool `json:"needsCategory"`
	// InTab selects mail whose resolved category tab is this one (#228). The
	// server does the enabled-fold, so a client asks for the tab the user
	// clicked and never has to know which tabs are switched off.
	InTab *string `json:"inTab"`
	// InArea selects mail whose resolved area is this one (#442). Unlike
	// InTab there is no enabled-fold to apply — an area either claims a
	// message or it does not — so the id is validated but not expanded.
	InArea *string `json:"inArea"`
	// Unsorted selects mail no area claims — the desk's "Needs sorting" pile
	// (#442). Mutually exclusive with InArea; `false` is a no-op rather than
	// "claimed by some area", since a bare filter should not have to be
	// negated to mean "no restriction".
	Unsorted *bool `json:"unsorted"`
	// OnDesk selects mail that is (true) or is not (false) "on the desk"
	// (#375, urn:mailroom:desk): live in INBOX or a feeds_desk mailbox, and
	// not junk, trashed, vanished or a draft — the SAME predicate
	// Area/getSummary counts over, so a tab's query and its count can never
	// disagree.
	OnDesk *bool `json:"onDesk"`
	// OnSurface splits scored mail at the desk's line (#375, urn:mailroom:desk,
	// DeskPreferences.lineThreshold): true keeps a score at or above the line
	// OR no score at all (unjudged mail is never hidden in a drawer); false
	// keeps a score below it.
	OnSurface *bool `json:"onSurface"`
	// DeskArchived selects mail day-one setup took off the desk without moving
	// anything (#446): true keeps exactly that mail, false keeps everything
	// else. It is what makes "here is what day-one filed away" a view a client
	// can render — that mail is in no folder anybody moved it to and carries no
	// keyword, so nothing else can name the set.
	DeskArchived *bool `json:"deskArchived"`
}

// emailFilterConditions is every FilterCondition property Email/query honors —
// the structured ones here plus the text ones searchFilter owns (search.go).
//
// It exists because an unknown condition MUST be answered with unsupportedFilter
// (RFC 8620 §5.5): silently ignoring one returns a SUPERSET of what the client
// asked for, and the client cannot tell. (Before this list existed, a filter of
// {"before": "..."} was decoded into a struct that had no such field and simply
// dropped — the query then returned the whole mailbox. See the report on #20.)
var emailFilterConditions = map[string]bool{
	// structured (Postgres)
	"inMailbox": true, "inThread": true, "inAccount": true,
	"hasKeyword": true, "notKeyword": true,
	"before": true, "after": true,
	// Mailroom extension (urn:mailroom:organize, #51): the Snoozed view, and the
	// condition that keeps snoozed mail out of the inbox view until it is due.
	"isSnoozed": true,
	// Mailroom extension (urn:mailroom:triage, #154): the priority inbox — mail
	// the T1 model scored at or above ImportantThreshold.
	"isImportant": true,
	// Mailroom extension (urn:mailroom:rules, #169): the needs-a-category queue —
	// mail the per-label classifier declined to judge.
	"needsCategory": true,
	// Mailroom extension (urn:mailroom:categorytabs, #228): the category tab
	// strip. The server applies the enabled-fold, so this is the tab the user
	// clicked rather than an expanded set.
	"inTab": true,
	// Mailroom extension (urn:mailroom:desk, #442): the desk's own partition —
	// mail resolved into a specific Area, or into none at all ("Needs
	// sorting").
	"inArea": true, "unsorted": true,
	// Mailroom extension (urn:mailroom:desk, #375): the desk's own tabs — mail
	// that is (or is not) "on the desk", and mail above (or below) the desk's
	// line.
	"onDesk": true, "onSurface": true,
	// Mailroom extension (urn:mailroom:desk, #446): mail day-one setup archived
	// IN MAILROOM — filed away without anything moving on the server.
	"deskArchived": true,
	// text (the search index)
	"text": true, "from": true, "to": true, "cc": true, "subject": true, "body": true,
	// Mailroom extension: the operator syntax (capSearch), and its as-you-type
	// modifier (#724)
	"query": true, "queryPrefix": true,
}

// emailQuery is the registration in service.go's method table: Email/query with
// NO search service. Installing one (API.SetSearch) replaces this entry with the
// same implementation bound to it.
func (a *API) emailQuery(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.emailQueryWith(ctx, nil, userID, rawArgs)
}

// emailQueryWith implements Email/query. When the filter carries a text condition
// (text/from/to/cc/subject/body, or Mailroom's `query` operator syntax) the search
// index answers ONE question — "which messages match this text?" — and hands the
// candidate ids to Postgres, which remains the authority on the structured
// predicates, the ORDER BY, the total, and the window. A text-filtered
// Email/query therefore has exactly the JMAP semantics of an unfiltered one
// (spec §7: the index is a cache; the store is the truth).
func (a *API) emailQueryWith(ctx context.Context, svc *search.Service, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailQueryArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	plan, mErr := parseEmailQuery(args)
	if mErr != nil {
		return nil, mErr
	}
	q := plan.query
	// An account-restricted API key queries only its accounts, in the store's
	// WHERE rather than by filtering the page (#49, ADR-042 §5).
	q.RestrictAccounts = keyAccounts(ctx)
	// A key's query always has a limit, the one its admission charged for
	// (keyQueryLimit, #49 review).
	var serverLimit *int
	if principalFrom(ctx) != nil {
		q.Limit, q.LimitSet = keyQueryLimitFor("Email/query", args.Limit), true
		serverLimit = keyServerLimit("Email/query", args.Limit)
	}

	// The category-tab filter is expanded HERE rather than in parseEmailQuery,
	// because expanding it needs the user's enabled-tab set and that parse is
	// deliberately pure. See emailQueryPlan.requestedTab and expandTabFilter.
	if plan.requestedTab != "" {
		wanted, mErr := a.expandTabFilter(ctx, userID, string(plan.requestedTab))
		if mErr != nil {
			return nil, mErr
		}
		q.InTab = wanted
	}

	// `inArea` is validated HERE, not in parseEmailQuery, for the same purity
	// reason as inTab's expansion above: confirming the area exists needs
	// store access. See area_email.go's checkAreaExists.
	if q.InArea != nil {
		if mErr := a.checkAreaExists(ctx, userID, *q.InArea); mErr != nil {
			return nil, mErr
		}
	}

	// The text half runs against the index and produces the candidate set. A
	// non-nil, empty set means "the text matched nothing" and yields no results —
	// never "no restriction".
	found, mErr := searchCandidates(ctx, svc, userID, plan.text, plan.searchLimit)
	if mErr != nil {
		return nil, mErr
	}
	q.RestrictIDs = found.candidates

	var (
		ids   []ulid.ULID
		total int
		state int64
	)
	mErr = a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		// The store half of a search (area:, is:snoozed, …, #722/#723), with
		// its area names resolved in this transaction.
		if found.store != nil {
			pred, perr := storePredicate(ctx, tx, found.store)
			if perr != nil {
				return &MethodError{Type: errServerFail, Description: perr.Error()}
			}
			q.Search = &pred
		}
		// onSurface — and a search for is:important — compare against the
		// desk's CURRENT line, read in this SAME transaction so the threshold
		// QueryEmails binds cannot straddle a concurrent DeskPreferences/set
		// (#375).
		if q.OnSurface != nil || (q.Search != nil && q.Search.Uses(store.SearchAboveLine)) {
			prefs, perr := tx.GetDeskPreferences(ctx)
			if perr != nil {
				return &MethodError{Type: errServerFail, Description: perr.Error()}
			}
			q.LineThreshold = prefs.LineThreshold
		}
		ids, total, err = tx.QueryEmails(ctx, q)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		state, err = tx.ChangeState(ctx, store.ObjectEmail)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	res := queryResult{
		AccountID:           userID.String(),
		QueryState:          encodeState(state),
		CanCalculateChanges: plan.stable, // Email/queryChanges returns a real delta only for delta-stable filters
		Position:            args.Position,
		Total:               total,
		IDs:                 ulidStrings(ids),
		Limit:               serverLimit,
	}
	if found.capped {
		res.SearchCapped = plan.searchLimit
	}
	return mustMarshal(res), nil
}

// emailQueryPlan is a parsed Email/query: the store query, the text/search half,
// and whether Email/queryChanges can compute a REAL delta for this filter+sort
// (the value Email/query reports as RFC 8620 §5.5 canCalculateChanges).
//
// A query is DELTA-STABLE only when its result set can be reconstructed
// deterministically from the Email change log. That holds for a filter built
// exclusively from the structured conditions whose EVERY membership change
// appends an Email change_log row — inMailbox, hasKeyword, notKeyword, before,
// after — plus the empty filter, sorted on an immutable field (receivedAt or
// sentAt). It does NOT hold for:
//
//   - a text/search condition (text/from/to/cc/subject/body, or Mailroom's
//     `query` operator): the result is index-ranked and its membership is not
//     driven by the change log at all;
//   - inThread / inAccount: a thread merge or a provenance retraction is not
//     something this first delta version proves is change-logged 1:1, so it is
//     excluded conservatively (correctness over completeness — CLAUDE.md);
//   - a `returnedAt` sort (#152): the key is COALESCE(snooze_woke_at,
//     received_at), and a wake rewrites it, so the ORDER of an unchanged result
//     set turns over — something a delta expressed as added/removed ids cannot
//     describe;
//   - isSnoozed (#51), which looks delta-stable and is NOT. Snoozing and waking
//     both append an Email change_log row, but membership of this condition also
//     turns over with the CLOCK: a message whose wake time passes leaves the
//     snoozed set at that instant, while the row that records it is appended
//     later, when the wake job runs. A delta computed from the log alone would
//     therefore be briefly wrong, so it falls back to a full re-query.
//
// Both non-stable cases fall back to cannotCalculateChanges in queryChanges.
type emailQueryPlan struct {
	query  store.EmailQuery
	text   searchFilter
	stable bool
	// requestedTab is the category tab the client asked for (#228), "" for none.
	//
	// It is carried rather than resolved because expanding it needs the user's
	// enabled-tab set, and parseEmailQuery is deliberately PURE — it takes no
	// context and touches no store, which is what lets Email/query and
	// Email/queryChanges share it and be guaranteed to parse identically. The
	// caller, which has both, does the expansion.
	requestedTab tabs.Tab
	// searchLimit is the clamped candidate limit for the index (#769).
	searchLimit int
}

// deltaStableConditions are the Email/query FilterCondition keys whose result-set
// membership is reconstructable from the Email change log (see emailQueryPlan).
var deltaStableConditions = map[string]bool{
	"inMailbox": true, "hasKeyword": true, "notKeyword": true,
	"before": true, "after": true,
	// ⚠ `inTab` IS DELIBERATELY ABSENT and must stay absent (#228). A message's
	// tab can change with NO Email change_log row behind it — the operator
	// re-points a label, or a classifier version bump re-runs the verdicts — so
	// its membership is not reconstructable from the Email change log, which is
	// exactly what this list means. Adding it would make Email/queryChanges hand
	// clients a delta that silently omits mail that moved tabs.
	//
	// ⚠ `inArea`/`unsorted` ARE ABSENT FOR THE SAME REASON, one level further
	// removed (#442). An area is derived state built from a message's labels
	// and its resolved tab (areas_resolve.go), so EITHER of two things that
	// carry no Email change_log row of their own can move a message between
	// areas: a Label/set repointing which area a label feeds, or an Area/set
	// changing an area's own feeds. Email/queryChanges therefore cannot
	// compute a real delta for either filter and falls back to
	// cannotCalculateChanges, exactly as it does for `inTab`.
	//
	// ⚠ `deskArchived` IS ABSENT TOO, and for a different reason worth naming
	// precisely (#446): the mark itself DOES append an Email change row (the
	// apply chunk appends exactly one per message it touches, and so does the
	// undo), so half of it would replay. The other half would not — undoing a
	// run clears the mark on thousands of messages inside a background job, and
	// a client holding a `deskArchived:true` query would need every one of them
	// as a REMOVED id in order; the delta is technically derivable and
	// practically a whole-result-set churn. Refusing is the honest answer, and
	// the same one inTab/inArea get.
	//
	// ⚠ `onDesk`/`onSurface` ARE ABSENT FOR THE SAME REASON TOO (#375). Neither
	// membership change appends an Email change_log row of its own: a score can
	// be (re)written — or the desk's lineThreshold changed via
	// DeskPreferences/set — with no Email row behind it, and a mailbox's
	// feedsDesk can flip via Mailbox/set with no Email row either. A delta
	// computed from the Email change log alone would therefore silently miss
	// mail that crossed onDesk or onSurface membership for either reason, so
	// Email/queryChanges falls back to cannotCalculateChanges here too.
	//
	// ⚠ `collapseThreads` MUST NEVER BE ADDED HERE EITHER, and it is not even a
	// key of this map to add — it is a top-level Email/query argument (RFC 8621
	// §4.4.4), not a FilterCondition. parseEmailQuery forces stable=false
	// directly whenever args.CollapseThreads is set, for the same reason inTab
	// is excluded: a new message arriving in a thread changes WHICH message
	// represents that thread, with no added/removed pair a change-log delta
	// can express — the representative silently swaps out from under a client
	// that trusted a delta. There is no Email change_log row for "this message
	// stopped being the thread's representative", so there is nothing for
	// Email/queryChanges to replay.
}

// parseEmailQuery decodes an Email/query filter+sort+window into the store query,
// the text half, and the delta-stability flag. It is shared by Email/query and
// Email/queryChanges so the two agree on which filters are structured, which are
// search-backed, and how a filter maps to store.EmailQuery — the queryState and
// result ordering chain between them only because they parse identically.
func parseEmailQuery(args emailQueryArgs) (emailQueryPlan, *MethodError) {
	var plan emailQueryPlan
	plan.searchLimit = search.DefaultLimit
	if args.SearchLimit != nil {
		if *args.SearchLimit < 0 {
			return emailQueryPlan{}, &MethodError{Type: errInvalidArguments, Description: "searchLimit must be >= 0"}
		}
		plan.searchLimit = min(max(*args.SearchLimit, search.DefaultLimit), search.MaxLimit)
	}
	q := store.EmailQuery{Position: args.Position}
	if args.Limit != nil {
		if *args.Limit < 0 {
			return emailQueryPlan{}, &MethodError{Type: errInvalidArguments, Description: "limit must be >= 0"}
		}
		q.Limit = *args.Limit
		// LimitSet, not just Limit: an explicit limit: 0 is a legitimate RFC
		// 8620 §5.5 request for zero ids, and Limit alone cannot distinguish it
		// from "no limit sent" — see store.EmailQuery.LimitSet.
		q.LimitSet = true
	}

	stable := true
	// RFC 8621 §4.4.4. Not a FilterCondition (see the ⚠ note on
	// deltaStableConditions), so it is carried and force-destabilized here
	// rather than through the emailFilterConditions loop below.
	q.CollapseThreads = args.CollapseThreads
	if q.CollapseThreads {
		stable = false
	}
	var text searchFilter
	if len(args.Filter) > 0 && !isJSONNull(args.Filter) {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(args.Filter, &raw); err != nil {
			return emailQueryPlan{}, &MethodError{Type: errInvalidArguments, Description: "filter must be an object"}
		}
		for k := range raw {
			if !emailFilterConditions[k] {
				return emailQueryPlan{}, &MethodError{Type: errUnsupportedFilter,
					Description: "unsupported filter condition: " + k}
			}
			// A single search-backed or thread/account condition makes the whole
			// filter volatile: its delta cannot be reconstructed from the change log.
			if !deltaStableConditions[k] {
				stable = false
			}
		}

		var f emailFilter
		if err := json.Unmarshal(args.Filter, &f); err != nil {
			return emailQueryPlan{}, &MethodError{Type: errInvalidArguments, Description: "unsupported filter"}
		}
		if err := json.Unmarshal(args.Filter, &text); err != nil {
			return emailQueryPlan{}, &MethodError{Type: errInvalidArguments, Description: "unsupported filter"}
		}
		// A query clause whose syntax can match nothing (`from:*foo`) is the
		// caller's mistake, not an empty mailbox (#712).
		if mErr := text.validate(); mErr != nil {
			return emailQueryPlan{}, mErr
		}
		if f.InMailbox != nil {
			id, err := ulid.Parse(*f.InMailbox)
			if err != nil {
				return emailQueryPlan{}, &MethodError{Type: errInvalidArguments, Description: "inMailbox is not a valid id"}
			}
			q.InMailbox = &id
		}
		if f.InThread != nil {
			id, err := ulid.Parse(*f.InThread)
			if err != nil {
				return emailQueryPlan{}, &MethodError{Type: errInvalidArguments, Description: "inThread is not a valid id"}
			}
			q.InThread = &id
		}
		if f.InAccount != nil {
			id, err := ulid.Parse(*f.InAccount)
			if err != nil {
				return emailQueryPlan{}, &MethodError{Type: errInvalidArguments, Description: "inAccount is not a valid id"}
			}
			q.InAccount = &id
		}
		if f.InTab != nil {
			// Validated here (an unknown tab is a filter error, not a silently
			// empty result) but EXPANDED by the caller, which has the user's
			// enabled set. See emailQueryPlan.requestedTab.
			t, ok := tabs.Parse(*f.InTab)
			if !ok {
				return emailQueryPlan{}, &MethodError{Type: errInvalidArguments,
					Description: "inTab is not a category tab"}
			}
			plan.requestedTab = t
		}
		unsorted := f.Unsorted != nil && *f.Unsorted
		if f.InArea != nil {
			id, err := ulid.Parse(*f.InArea)
			if err != nil {
				return emailQueryPlan{}, &MethodError{Type: errInvalidArguments,
					Description: "inArea is not an area id"}
			}
			if unsorted {
				return emailQueryPlan{}, &MethodError{Type: errInvalidArguments,
					Description: "inArea and unsorted are mutually exclusive"}
			}
			// EXISTENCE is checked by the caller (emailQueryWith calling
			// checkAreaExists), which has store access; this parse stays pure.
			// An area that does not exist must be invalidArguments, not a
			// silently empty result — the same reasoning expandTabFilter gives
			// for a disabled tab.
			q.InArea = &id
		} else if unsorted {
			q.Unsorted = true
		}
		if f.Before != "" {
			t, ok := jmapTime(f.Before)
			if !ok {
				return emailQueryPlan{}, &MethodError{Type: errInvalidArguments, Description: "before is not a valid UTCDate"}
			}
			q.Before = &t
		}
		if f.After != "" {
			t, ok := jmapTime(f.After)
			if !ok {
				return emailQueryPlan{}, &MethodError{Type: errInvalidArguments, Description: "after is not a valid UTCDate"}
			}
			q.After = &t
		}
		q.Snoozed = f.IsSnoozed
		q.Important = f.IsImportant
		q.NeedsCategory = f.NeedsCategory
		q.HasKeyword = f.HasKeyword
		q.NotKeyword = f.NotKeyword
		q.OnDesk = f.OnDesk
		q.OnSurface = f.OnSurface
		q.DeskArchived = f.DeskArchived
	}
	// The cutoff travels with the query: the store does not get to decide what
	// "important" means, and spec §8 makes it per-user adjustable.
	q.ImportantThreshold = ImportantThreshold
	// receivedAt/sentAt (RFC 8621) plus Mailroom's returnedAt; default is
	// receivedAt desc. The two RFC keys are immutable, so they never introduce
	// volatility — returnedAt does, and says so below.
	for _, s := range args.Sort {
		switch s.Property {
		case "receivedAt", "sentAt":
		case "returnedAt":
			// urn:mailroom:organize (#152): order by EFFECTIVE ARRIVAL —
			// COALESCE(snooze_woke_at, received_at) — so a message that came back
			// from a snooze returns to the top of the list instead of to the
			// position its age dictates, which for anything snoozed more than a few
			// hours is far enough down that the user never sees it.
			//
			// Its own sort property rather than a redefinition of receivedAt: a
			// client that asks to sort by receivedAt is promised receivedAt, and
			// quietly ordering by something else — while still reporting the
			// original receivedAt on the object — would make the two disagree.
			//
			// MUTABLE, unlike the RFC keys: a wake changes it, so the result order
			// turns over without a client-visible cause the change log can be
			// replayed from. Never delta-stable.
			q.SortByEffectiveArrival = true
			stable = false
		case "priority":
			// urn:mailroom:triage (#154): spec §6's Priority Inbox order — the
			// important bucket first, then everything else, each newest-first.
			// Grouping in SQL rather than in the client is what makes the view page
			// correctly; a client can only regroup the window it already holds.
			//
			// Not delta-stable: the model rescores, and a message crosses the
			// threshold with no Email change_log row to replay.
			q.SortByPriority = true
			q.SortByEffectiveArrival = true
			stable = false
		case "suggestionConfidence":
			// urn:mailroom:rules (#176): the needs-a-category queue's order —
			// highest open-suggestion confidence first, then effective arrival.
			//
			// Server-side because the client cannot do it: it can only reorder the
			// page it was handed, and a page selected by received_at is the NEWEST
			// N open questions rather than the most confident ones. On a mailbox
			// with more questions than fit in a page those are different sets, and
			// the client has no way to tell.
			//
			// Not delta-stable, for a reason worth naming precisely: a suggestion
			// is written during ingest and rides the Email-CREATE row, so the
			// arrival of a new question moves the order with no Email/updated row
			// to replay. (Its removal DOES append one — store.DismissCategoryQuestions
			// — but one direction being replayable is not enough.)
			q.SortBySuggestionConfidence = true
			q.SortByEffectiveArrival = true
			stable = false
		case "relevance":
			// urn:mailroom:search (#724): best match first — the order the search
			// index ranked its candidates in (search.RankHits, a total order), with
			// Postgres still choosing the window: the candidate list is passed down
			// as RestrictIDs and the store orders by each id's position in it. So
			// a page is position/limit over one deterministic sequence, exactly as
			// for a date sort; see store.EmailQuery.SortByRank for why that holds
			// across page requests and what it falls back to when the index ranked
			// nothing (a store-only query such as `area:Work` alone).
			//
			// Not delta-stable: the index re-scores as mail arrives. (A text
			// filter never is anyway.)
			q.SortByRank = true
			stable = false
		default:
			return emailQueryPlan{}, &MethodError{Type: errInvalidArguments, Description: "unsupported sort property " + s.Property}
		}
		if s.IsAscending != nil && *s.IsAscending {
			q.SortAscending = true
		}
	}
	// A search for `is:trashed` / `is:junk` is answered here, in Postgres, not
	// by the index (#623, #723), and a search's scope (`in:anywhere`, the junk
	// default) is a Postgres predicate too: see searchFilter.plan. A text filter
	// is never delta-stable, so none of this can reach Email/queryChanges.
	if !text.empty() {
		sp := text.plan(true)
		q.OnlyTrashed, q.IncludeDeleted = sp.onlyTrashed, sp.includeDeleted
		q.OnlyJunk, q.ExcludeJunk = sp.onlyJunk, sp.excludeJunk
	}
	plan.query, plan.text, plan.stable = q, text, stable
	return plan, nil
}

// emailQueryChangesArgs is the RFC 8620 §5.6 Email/queryChanges request. It has
// no position/limit — the client tracks the whole result list (or, with upToId,
// a prefix of it).
type emailQueryChangesArgs struct {
	AccountID       string           `json:"accountId"`
	Filter          json.RawMessage  `json:"filter"`
	Sort            []sortComparator `json:"sort"`
	SinceQueryState string           `json:"sinceQueryState"`
	MaxChanges      *int             `json:"maxChanges"`
	UpToID          *string          `json:"upToId"`
	CalculateTotal  bool             `json:"calculateTotal"`
	// CollapseThreads mirrors Email/query's argument of the same name (RFC
	// 8621 §4.4.4). Accepted here too so a client that sends it gets a
	// coherent answer: parseEmailQuery forces plan.stable=false whenever it is
	// set, which the !plan.stable check below turns into
	// cannotCalculateChanges rather than a delta that silently omits a
	// representative swap — see the ⚠ note on deltaStableConditions.
	CollapseThreads bool `json:"collapseThreads"`
}

// queryChangesAdded is one RFC 8620 §5.6 AddedItem: an id and the index at which
// the client must splice it into its (post-removal) cached result list.
type queryChangesAdded struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
}

// queryChangesResult is the RFC 8620 §5.6 Foo/queryChanges response.
type queryChangesResult struct {
	AccountID     string              `json:"accountId"`
	OldQueryState string              `json:"oldQueryState"`
	NewQueryState string              `json:"newQueryState"`
	Total         *int                `json:"total,omitempty"`
	Removed       []string            `json:"removed"`
	Added         []queryChangesAdded `json:"added"`
}

// emailQueryChanges computes a real RFC 8620 §5.6 delta for a DELTA-STABLE query
// (see emailQueryPlan), and falls back to cannotCalculateChanges for a volatile
// (text/search or inThread/inAccount) filter or a windowed request (upToId).
//
// THE ALGORITHM, and why it is exact. The queryState IS the Email change state
// (Email/query returns encodeState(ChangeState(Email))), so sinceQueryState
// decodes to a change sequence and the delta comes straight from the change log:
//
//   - `touched` = the ids the change log reports created-or-updated since the
//     since-state (tx.Changes' created ∪ updated buckets); `removed` = updated ∪
//     destroyed.
//   - Run the query NOW to get the full ordered result. `added` = every touched
//     id still in that result, at its absolute index, in ascending index order.
//
// A client that removes `removed` from its cached list and then splices `added`
// reconstructs the new result EXACTLY, because for a delta-stable filter every
// entry to / exit from the result set appends an Email change_log row for that
// message (the load-bearing invariant): so an UNTOUCHED id has identical
// membership before and after, and — the sort key being immutable — identical
// relative order. The untouched ids therefore fall through unchanged and the
// touched ids are re-placed at their true indices. If we cannot guarantee that
// (a volatile filter), we return cannotCalculateChanges rather than a wrong
// delta, which is strictly worse (the client would silently desync).
func (a *API) emailQueryChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailQueryChangesArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	// Parse the filter/sort exactly as Email/query does. queryChanges carries no
	// position/limit, so this is always the unwindowed query.
	plan, mErr := parseEmailQuery(emailQueryArgs{
		Filter: args.Filter, Sort: args.Sort, CollapseThreads: args.CollapseThreads,
	})
	if mErr != nil {
		return nil, mErr
	}
	// A volatile filter, or a windowed (upToId) request we do not compute deltas
	// for, is the honest fallback — exactly what canCalculateChanges:false on
	// Email/query already told the client to expect (RFC 8620 §5.5/§5.6).
	//
	// onDesk/onSurface never reach tx.QueryEmails from here: they are absent
	// from deltaStableConditions (see its ⚠ note, #375), so plan.stable is
	// already false and the check below returns cannotCalculateChanges before
	// LineThreshold would ever need to be looked up — unlike emailQueryWith,
	// this path has no GetDeskPreferences call to make.
	if !plan.stable || args.UpToID != nil {
		return nil, &MethodError{Type: errCannotCalculateChanges}
	}
	since, err := decodeState(args.SinceQueryState)
	if err != nil {
		return nil, &MethodError{Type: errCannotCalculateChanges}
	}
	if args.MaxChanges != nil && *args.MaxChanges <= 0 {
		return nil, &MethodError{Type: errInvalidArguments, Description: "maxChanges must be a positive integer"}
	}

	var (
		page  store.ChangesPage
		ids   []ulid.ULID
		total int
	)
	mErr = a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		// tx.Changes gives the change-log buckets since `since` AND enforces the
		// compaction floor / impossible-future check (→ cannotCalculateChanges),
		// with page.OldState == since and page.NewState == the current Email state.
		p, cErr := tx.Changes(ctx, store.ObjectEmail, since, 0)
		if cErr != nil {
			if errors.Is(cErr, store.ErrCannotCalculateChanges) {
				return &MethodError{Type: errCannotCalculateChanges}
			}
			return &MethodError{Type: errServerFail, Description: cErr.Error()}
		}
		page = p
		// The full current result, in query order, in the SAME transaction so it is
		// consistent with page.NewState (data-model §3.1).
		ids, total, cErr = tx.QueryEmails(ctx, plan.query)
		if cErr != nil {
			return &MethodError{Type: errServerFail, Description: cErr.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	// touched = created ∪ updated: the ids that may re-enter the result at a new
	// position. removed = updated ∪ destroyed: every id that may have been in the
	// client's list and either left or moved (a purely-created id was never in the
	// client's list, so it is only ever added).
	touched := make(map[ulid.ULID]bool, len(page.Created)+len(page.Updated))
	for _, id := range page.Created {
		touched[id] = true
	}
	for _, id := range page.Updated {
		touched[id] = true
	}
	removed := make([]string, 0, len(page.Updated)+len(page.Destroyed))
	for _, id := range page.Updated {
		removed = append(removed, id.String())
	}
	for _, id := range page.Destroyed {
		removed = append(removed, id.String())
	}
	added := make([]queryChangesAdded, 0, len(touched))
	for i, id := range ids {
		if touched[id] {
			added = append(added, queryChangesAdded{ID: id.String(), Index: i})
		}
	}
	// maxChanges bounds the size of the delta (RFC 8620 §5.6): if honoring it would
	// require returning more, refetching via /query is the answer.
	if args.MaxChanges != nil && len(added)+len(removed) > *args.MaxChanges {
		return nil, &MethodError{Type: errCannotCalculateChanges}
	}

	res := queryChangesResult{
		AccountID:     userID.String(),
		OldQueryState: encodeState(page.OldState),
		NewQueryState: encodeState(page.NewState),
		Removed:       removed,
		Added:         added,
	}
	if args.CalculateTotal {
		res.Total = &total
	}
	return mustMarshal(res), nil
}

func (a *API) emailChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectEmail)
}

// ---- Email/set ------------------------------------------------------------

type emailSetArgs struct {
	AccountID string                     `json:"accountId"`
	IfInState *string                    `json:"ifInState"`
	Create    map[string]json.RawMessage `json:"create"`
	Update    map[string]json.RawMessage `json:"update"`
	Destroy   []string                   `json:"destroy"`
	// DiscardDrafts is a Mailroom extension for API keys (#872, ADR-045): the
	// key's OWN unsent drafts to move to Trash, under mail:draft. A session
	// destroys a draft instead.
	DiscardDrafts []string `json:"discardDrafts"`
}

type setResult struct {
	AccountID    string              `json:"accountId"`
	OldState     string              `json:"oldState"`
	NewState     string              `json:"newState"`
	Created      map[string]any      `json:"created"`
	Updated      map[string]any      `json:"updated"`
	Destroyed    []string            `json:"destroyed"`
	NotCreated   map[string]SetError `json:"notCreated"`
	NotUpdated   map[string]SetError `json:"notUpdated"`
	NotDestroyed map[string]SetError `json:"notDestroyed"`
	// Discarded / NotDiscarded answer Email/set's discardDrafts (#872); absent
	// from every other answer.
	Discarded    []string            `json:"discarded,omitempty"`
	NotDiscarded map[string]SetError `json:"notDiscarded,omitempty"`
}

func (a *API) emailSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	res := setResult{
		AccountID:    userID.String(),
		Created:      map[string]any{},
		Updated:      map[string]any{},
		Destroyed:    []string{},
		NotCreated:   map[string]SetError{},
		NotUpdated:   map[string]SetError{},
		NotDestroyed: map[string]SetError{},
	}

	// The whole set runs in ONE UserTx: oldState/newState bracket every mutation
	// and per-object failures still commit the successful ones. A stateMismatch
	// aborts the whole method (RFC 8620 §5.3).
	var stateErr *MethodError
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// A key's draft lock (#872), taken ONCE and FIRST, before any lock a
		// loop below takes (a message's row lock in a destroy or an update, a
		// feedback lock, the key's api_keys row through a draft insert's
		// foreign key). Taken per item instead, it came after those in one
		// call and before them in another: A = {create, destroy: [M]} against
		// B = {destroy: [M], discardDrafts} could deadlock. Every key call
		// that creates or discards now orders the same way. (The hold of a
		// send for approval never takes it: see keyMaySubmitDraft.)
		if p := principalFrom(ctx); p != nil && len(args.Create)+len(args.DiscardDrafts) > 0 {
			if err := tx.LockAPIKeyDrafts(ctx, p.key.ID); err != nil {
				return err
			}
		}
		oldState, err := tx.ChangeState(ctx, store.ObjectEmail)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)

		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		// Create = compose a DRAFT (spec §5). The server builds the RFC 5322
		// bytes, validates the From against the user's verified identities, and
		// files the draft locally + queues its APPEND to the IMAP Drafts folder.
		created := map[string]ulid.ULID{}
		// A key's repeat collapses into its earlier draft (#872), except in a
		// call that also destroys or discards: the earlier draft might be the
		// one going away.
		collapse := len(args.Destroy) == 0 && len(args.DiscardDrafts) == 0
		for cid, raw := range args.Create {
			if a.composer == nil {
				res.NotCreated[cid] = SetError{Type: setNotImplemented,
					Description: "this server is not configured to compose mail (no blob store)"}
				continue
			}
			// A key's create: collapsed, a replacement, or a new draft, and
			// remembered as the key's either way (key_drafts.go).
			if p := principalFrom(ctx); p != nil {
				obj, id, serr, err := a.keyCreateDraft(ctx, tx, userID, p, raw, collapse)
				if err != nil {
					return err
				}
				if serr != nil {
					res.NotCreated[cid] = *serr
					continue
				}
				created[cid] = id
				res.Created[cid] = obj
				continue
			}
			if replacesDraft(raw) {
				res.NotCreated[cid] = SetError{Type: setInvalidProps, Properties: []string{"replacesEmailId"},
					Description: "replacesEmailId is for API keys; a session edits a draft by creating the new " +
						"version and destroying the old one in the same call"}
				continue
			}
			obj, id, serr := a.createDraft(ctx, tx, userID, raw)
			if serr != nil {
				res.NotCreated[cid] = *serr
				continue
			}
			created[cid] = id
			res.Created[cid] = obj
		}

		for id, patch := range args.Update {
			mid, perr := ulid.Parse(id)
			if perr != nil {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			up, serr := parseEmailPatch(ctx, tx, patch)
			if serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			// An account-restricted API key changes only its accounts' mail,
			// and moves it only within them (#49, ADR-042 §5).
			if visible, verr := keyVisibleMessages(ctx, tx, []ulid.ULID{mid}); verr != nil {
				return verr
			} else if visible != nil && !visible[mid] {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			if kerr, verr := keyMayApplyEmailUpdate(ctx, tx, up); verr != nil {
				return verr
			} else if kerr != nil {
				res.NotUpdated[id] = *kerr
				continue
			}
			// AN API KEY'S CHANGE IS THE KEY'S, NEVER THE USER'S EVIDENCE (#49,
			// ADR-042 §3a): its labels are applied_by 'agent', and the store
			// records no correction, no learned-placement verdict and no "kept on
			// archive" for it (store.LabelByAgent).
			if principalFrom(ctx) != nil {
				up.LabelSource = store.AgentLabelSource()
			}
			found, _, aerr := tx.ApplyEmailUpdate(ctx, mid, up)
			var protected *store.UserFilingProtectedError
			if errors.As(aerr, &protected) {
				props := make([]string, 0, len(protected.LabelIDs))
				for _, l := range protected.LabelIDs {
					props = append(props, "labelIds/"+l.String())
				}
				res.NotUpdated[id] = SetError{Type: setForbidden, Properties: props,
					Description: "the mailbox owner filed this message into that area by hand, and an API key cannot undo their own filing; they can move it in Mailroom"}
				continue
			}
			if errors.Is(aerr, store.ErrCannotMakeDraft) {
				res.NotUpdated[id] = SetError{Type: setForbidden, Properties: []string{"keywords/$draft"},
					Description: "a message that is not a draft cannot be made one"}
				continue
			}
			if errors.Is(aerr, store.ErrInvalidKeyword) {
				// The store's backstop; parseEmailPatch refuses these first.
				res.NotUpdated[id] = SetError{Type: setInvalidProps, Properties: []string{"keywords"},
					Description: "that keyword has a meaning of its own (an IMAP system flag or a junk marker) and cannot be set as a plain keyword"}
				continue
			}
			if aerr != nil {
				return aerr
			}
			if !found {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			res.Updated[id] = nil
			// Capture the triage signal this action carries (mark-read, star,
			// archive-unread), best-effort and SAVEPOINT-guarded — never able to
			// undo the update just applied. mailroom#33 PR2 (see feedback.go).
			a.emitEmailSetFeedback(ctx, tx, mid, up)
		}

		for _, id := range args.Destroy {
			mid, ok := resolveSetID(id, created)
			if !ok {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			// A DRAFT is destroyed, not trashed: retiring the superseded version
			// (and expunging exactly its copy from the IMAP Drafts folder) is how
			// an edited draft is replaced — the client creates the new version and
			// destroys the old one in the same call. See store.RetireDraft for the
			// conflict rule this implements (spec §5: LWW, loser preserved).
			ref, rerr := tx.MessageRef(ctx, mid)
			switch {
			case errors.Is(rerr, store.ErrMessageNotFound):
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			case rerr != nil:
				return rerr
			}
			if visible, verr := keyVisibleMessages(ctx, tx, []ulid.ULID{mid}); verr != nil {
				return verr
			} else if visible != nil && !visible[mid] {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			// ...unless it has been SUBMITTED (mailroom#148). A draft-flagged
			// message with a submission is either mid-Undo-Send or left
			// draft-flagged by #146, and retiring it would expunge EVERY live copy
			// — including the one in Sent, deleting the user's record of a message
			// they actually sent. It falls through to the Trash path instead.
			submitted, pending, serr := tx.MessageSubmissionStatus(ctx, mid)
			if serr != nil {
				return serr
			}
			if pending {
				// Still going out. Silently trashing it would leave the user
				// believing they had stopped a send they had not.
				res.NotDestroyed[id] = SetError{Type: setForbidden,
					Description: "this message is queued to send; cancel the send before deleting it"}
				continue
			}
			// AN API KEY NEVER HARD-DELETES (spec §13a, ADR-009): retiring an
			// unsent draft removes it for good, so a key's destroy of one takes
			// the Trash path below like every other message it deletes.
			if ref.IsDraft && !submitted && principalFrom(ctx) == nil {
				reclaim, derr := tx.RetireDraft(ctx, mid)
				// A draft-flagged message outside every Drafts folder is not a
				// draft RetireDraft will erase (#49 review): it takes the soft
				// delete below like any other message.
				if !errors.Is(derr, store.ErrNotADraft) {
					if derr != nil {
						return derr
					}
					// The blob FILES of the retired revision, inside this
					// transaction (#300) — see reclaimBlobFiles for the ordering.
					if derr := a.reclaimBlobFiles(ctx, userID, reclaim); derr != nil {
						return derr
					}
					res.Destroyed = append(res.Destroyed, id)
					continue
				}
			}
			// Everything else: destroy = move to Trash (soft-delete, ADR-009).
			// The message keeps existing in Trash and the change log records an
			// Email update (not destroyed — only a hard purge emits destroyed,
			// data-model §6).
			trash := store.EmailUpdate{Trash: true}
			if principalFrom(ctx) != nil {
				trash.LabelSource = store.AgentLabelSource() // the key's delete (#49)
			}
			found, _, aerr := tx.ApplyEmailUpdate(ctx, mid, trash)
			if aerr != nil {
				return aerr
			}
			if !found {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			res.Destroyed = append(res.Destroyed, id)
			// A non-draft destroy is a move to Trash — a dismissal signal for the
			// triage model (best-effort, SAVEPOINT-guarded). A retired DRAFT above
			// is not a triage signal and emits nothing.
			a.emitEmailSetFeedback(ctx, tx, mid, store.EmailUpdate{Trash: true})
		}

		// discardDrafts (#872): a key's own unsent drafts, to Trash. Not a
		// triage signal — the key's own words are no evidence about the
		// owner's mail — so no feedback is emitted.
		if len(args.DiscardDrafts) > 0 {
			res.NotDiscarded = map[string]SetError{}
			res.Discarded = []string{}
		}
		for _, id := range args.DiscardDrafts {
			p := principalFrom(ctx)
			if p == nil {
				res.NotDiscarded[id] = SetError{Type: setForbidden,
					Description: "discardDrafts is for API keys; a session destroys a draft"}
				continue
			}
			serr, err := keyDiscardDraft(ctx, tx, p, id)
			if err != nil {
				return err
			}
			if serr != nil {
				res.NotDiscarded[id] = *serr
				continue
			}
			res.Discarded = append(res.Discarded, id)
		}

		newState, err := tx.ChangeState(ctx, store.ObjectEmail)
		if err != nil {
			return err
		}
		res.NewState = encodeState(newState)
		return nil
	})
	if stateErr != nil {
		return nil, stateErr
	}
	if err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	// The transaction has COMMITTED, so any writeback intents it enqueued are
	// visible to the drain (#581). Before this, a move or a flag waited for the
	// account's next scheduled pass — up to the whole sync interval — because
	// writeback only ever ran as that pass's pre-sync hook.
	//
	// AFTER the commit and AFTER the error checks, deliberately: nudging a
	// rolled-back transaction would dial IMAP to discover there is nothing to
	// send. It returns immediately; the drain happens elsewhere.
	a.nudgeWriteback(userID)
	return mustMarshal(res), nil
}

// parseEmailPatch translates a JMAP Email/set update patch into the canonical
// EmailUpdate, routing keyword changes to system flags, labels, or raw keywords,
// labelIds changes to labels by id (#704 — the only route to a local-only
// label), and mailboxIds changes to trash/restore/move. It supports both the
// "prop/key" patch form and full-object replacement of keywords/mailboxIds;
// labelIds is patch-form only.
func parseEmailPatch(ctx context.Context, tx *store.UserTx, patch json.RawMessage) (store.EmailUpdate, *SetError) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(patch, &raw); err != nil {
		return store.EmailUpdate{}, &SetError{Type: setInvalidPatch, Description: err.Error()}
	}
	var up store.EmailUpdate

	setKeyword := func(name string, present bool) *SetError {
		if !validJMAPKeyword(name) {
			return invalidKeyword(name)
		}
		// System keywords are matched CASE-INSENSITIVELY (RFC 8621 §4.1.1;
		// #49 review). An exact match let `$JUNK`, `junk` or `$Draft` fall to
		// default:, become raw keywords, and come back from the next sync as
		// is_junk (ingest folds case) — around the refusals below. Only the
		// COMPARISON is folded: label atoms are stored mixed-case
		// ("MailroomLabel-…") and looked up exactly, so default: keeps the name
		// as sent.
		lname := strings.ToLower(name)
		if slices.Contains(store.JunkKeywords, lname) {
			lname = store.KeywordJunk // bare `junk` is the provider's spelling of $junk
		}
		if slices.Contains(store.NotJunkKeywords, lname) {
			return &SetError{Type: setInvalidProps, Properties: []string{"keywords/" + name},
				Description: "keywords/" + name + " is the mail provider's not-junk marker, not a keyword; " +
					"remove keywords/" + store.KeywordJunk + " to mark a message not junk"}
		}
		switch lname {
		case store.KeywordSeen:
			up.Seen = boolPtr(present)
		case store.KeywordFlagged:
			up.Flagged = boolPtr(present)
		case store.KeywordAnswered:
			up.Answered = boolPtr(present)
		case store.KeywordDraft:
			up.Draft = boolPtr(present)
		case store.KeywordJunk:
			// Spec §6: "Junk is read-only with one exception: 'Not junk' must
			// write back (move to INBOX) so users can rescue false positives;
			// where the provider offers a ham-feedback mechanism we use it,
			// otherwise the move itself is the signal."
			//
			// ⚠ Until #351 $junk had no case here at all. It fell through to
			// default:, failed the label lookup, and landed in RemoveKeywords —
			// which edits the keywords ARRAY, while $junk is derived from the
			// is_junk COLUMN. So "Not junk" returned success and changed
			// nothing, and the one spam action the spec makes mandatory was the
			// one that did not work.
			if present {
				// Setting junk is not ours to do: the verdict is the provider's
				// and arrives through ingest. Refusing is the honest answer —
				// accepting it would put a local flag out of step with the
				// provider on the next sync, which is how it would have behaved.
				return &SetError{Type: setInvalidPatch, Description: "keywords/" + store.KeywordJunk +
					" cannot be set: the junk verdict is the mail provider's, mirrored here. " +
					"Removing it (\"not junk\") is supported and moves the message to your inbox."}
			}
			up.Junk = boolPtr(false)
			// The move IS the ham signal (spec §6). Only supply it if the client
			// has not already named a destination — an explicit mailbox change in
			// the same patch is the user being more specific, not less.
			if up.MoveToRole == "" && up.MoveToMailboxID == nil {
				up.MoveToRole = "inbox"
			}
		default:
			// A label (a known keyword atom) or a raw keyword.
			lid, ok, err := tx.LabelIDByKeywordAtom(ctx, name)
			if err != nil {
				return &SetError{Type: setInvalidPatch, Description: err.Error()}
			}
			if ok {
				if present {
					up.AddLabelIDs = append(up.AddLabelIDs, lid)
				} else {
					up.RemoveLabelIDs = append(up.RemoveLabelIDs, lid)
				}
			} else if present {
				up.AddKeywords = append(up.AddKeywords, name)
			} else {
				up.RemoveKeywords = append(up.RemoveKeywords, name)
			}
		}
		return nil
	}

	// labelIDPatches are the labels this patch names BY ID (`labelIds/<id>`,
	// #704), checked against the user's labels in one read after the loop.
	labelIDPatches := map[ulid.ULID]bool{}
	labelIDKeys := map[ulid.ULID]string{}

	setMailbox := func(id string, present bool) *SetError {
		mid, err := ulid.Parse(id)
		if err != nil {
			return &SetError{Type: setInvalidPatch, Description: "mailboxIds key is not a valid id"}
		}
		role, found, rerr := tx.MailboxRole(ctx, mid)
		if rerr != nil {
			return &SetError{Type: setInvalidPatch, Description: rerr.Error()}
		}
		if !found {
			return &SetError{Type: setNotFound, Description: "mailbox " + id + " not found"}
		}
		switch {
		case role == "trash" && present:
			up.Trash = true
		case role == "trash" && !present:
			up.Restore = true
		case present:
			// A move into a non-trash folder (archive et al.). BOTH the role and
			// the mailbox the client actually named are carried: the role drives
			// the per-account writeback (mailbox names differ per account), while
			// the named mailbox is the canonical placement — which is also what
			// makes a move into a folder with no SPECIAL-USE role land in THAT
			// folder rather than being rounded off to Archive.
			target := mid
			up.MoveToRole = role
			up.MoveToMailboxID = &target
		}
		return nil
	}

	for key, val := range raw {
		switch {
		case key == "keywords":
			// Full replacement: apply system flags by presence; add non-system
			// tokens present in the new set (removal of raw keywords on full
			// replace is not diffed in 0.5 — clients use the patch form).
			var kw map[string]bool
			if err := json.Unmarshal(val, &kw); err != nil {
				return store.EmailUpdate{}, &SetError{Type: setInvalidPatch, Description: "keywords must be an object"}
			}
			// Every name is checked, present or not: the loops below skip the
			// absent ones, and an object naming `\Draft` is refused whole
			// rather than half-applied (#49 review).
			for name := range kw {
				if !validJMAPKeyword(name) {
					return store.EmailUpdate{}, invalidKeyword(name)
				}
			}
			// A key's whole-object replacement leaves draft state alone (#49
			// review): no key may change it (keyRefusedEmailUpdateFields), and a
			// replacement that omits $draft would otherwise mean "clear it" and
			// make every such patch from a key forbidden.
			keyCaller := principalFrom(ctx) != nil
			for name := range systemKeywords {
				if name == store.KeywordJunk || (keyCaller && name == store.KeywordDraft) {
					continue
				}
				// Present in ANY casing (RFC 8621 §4.1.1).
				present := false
				for k, v := range kw {
					present = present || (v && strings.EqualFold(k, name))
				}
				if serr := setKeyword(name, present); serr != nil {
					return store.EmailUpdate{}, serr
				}
			}
			for name, present := range kw {
				if l := strings.ToLower(name); systemKeywords[l] || slices.Contains(store.JunkKeywords, l) || !present {
					continue
				}
				if serr := setKeyword(name, true); serr != nil {
					return store.EmailUpdate{}, serr
				}
			}
		case strings.HasPrefix(key, "keywords/"):
			name := strings.TrimPrefix(key, "keywords/")
			if serr := setKeyword(name, isPatchTrue(val)); serr != nil {
				return store.EmailUpdate{}, serr
			}
		case key == "mailboxIds":
			var mb map[string]bool
			if err := json.Unmarshal(val, &mb); err != nil {
				return store.EmailUpdate{}, &SetError{Type: setInvalidPatch, Description: "mailboxIds must be an object"}
			}
			for id, present := range mb {
				if !present {
					continue
				}
				if serr := setMailbox(id, true); serr != nil {
					return store.EmailUpdate{}, serr
				}
			}
		case strings.HasPrefix(key, "mailboxIds/"):
			id := strings.TrimPrefix(key, "mailboxIds/")
			if serr := setMailbox(id, isPatchTrue(val)); serr != nil {
				return store.EmailUpdate{}, serr
			}
		case key == "labelIds":
			// A whole-object replacement would mean "remove every label not
			// listed", which needs the message's current set to diff against —
			// and getting that wrong strips labels the client never saw (a
			// client that fetched without labelIds would send {} and wipe the
			// message). Refused rather than half-supported; the patch form below
			// says exactly what it changes.
			return store.EmailUpdate{}, &SetError{Type: setInvalidPatch,
				Description: "labelIds cannot be replaced wholesale; patch labelIds/<labelId> with true or null",
				Properties:  []string{"labelIds"}}
		case strings.HasPrefix(key, "labelIds/"):
			// urn:mailroom:labels (#704): apply or remove a label BY ID — the
			// only way to reach a local-only label, which has no keyword atom
			// (ADR-031). It lands in the same EmailUpdate fields the keyword path
			// fills, so ApplyEmailUpdate does everything it does for a keyword
			// patch: the change_log row, clearing open questions (#169), rule
			// corrections (#170), and a writeback intent ONLY when the label has
			// an atom (store.labelKeywordAtom). A local label therefore writes
			// nothing to the mail server, by the same code that already decides
			// that — not by a check here that could drift from it.
			lid, err := ulid.Parse(strings.TrimPrefix(key, "labelIds/"))
			if err != nil {
				return store.EmailUpdate{}, &SetError{Type: setInvalidPatch,
					Description: "labelIds key is not a valid id", Properties: []string{key}}
			}
			labelIDPatches[lid] = isPatchTrue(val)
			labelIDKeys[lid] = key
		case key == "isPinned":
			// urn:mailroom:organize. Settable as a plain boolean; pinnedAt is
			// derived server-side, so a client cannot backdate a pin (and there is
			// no reason it should want to).
			up.Pinned = boolPtr(isPatchTrue(val))
		case key == "snoozedUntil":
			// urn:mailroom:organize (#51). The WAKE TIME is what is settable —
			// there is no separate isSnoozed to patch, because the timestamp is the
			// whole state. null un-snoozes.
			if isJSONNull(val) {
				up.Unsnooze = true
				break
			}
			var s string
			if err := json.Unmarshal(val, &s); err != nil {
				return store.EmailUpdate{}, &SetError{Type: setInvalidProps,
					Description: "snoozedUntil must be a UTCDate string or null",
					Properties:  []string{"snoozedUntil"}}
			}
			at, ok := jmapTime(s)
			if !ok {
				return store.EmailUpdate{}, &SetError{Type: setInvalidProps,
					Description: "snoozedUntil is not a valid UTCDate",
					Properties:  []string{"snoozedUntil"}}
			}
			// A wake time in the past is refused rather than quietly accepted. It
			// would "work" — the next dispatcher pass would wake it — but it means
			// the client asked for something it cannot have got right, and a
			// message that is snoozed and instantly un-snoozed is a confusing
			// non-event to explain. Say so instead.
			if !at.After(time.Now()) {
				return store.EmailUpdate{}, &SetError{Type: setInvalidProps,
					Description: "snoozedUntil must be in the future",
					Properties:  []string{"snoozedUntil"}}
			}
			up.SnoozeUntil = &at
		default:
			// Unknown/unsupported property in the patch.
			return store.EmailUpdate{}, &SetError{Type: setInvalidProps,
				Description: "unsupported Email property in patch: " + key, Properties: []string{key}}
		}
	}
	if serr := applyLabelIDPatches(ctx, tx, &up, labelIDPatches, labelIDKeys); serr != nil {
		return store.EmailUpdate{}, serr
	}
	return up, nil
}

// applyLabelIDPatches validates the `labelIds/<id>` half of a patch (#704) and
// folds it into up.
//
// ONE READ FOR THE WHOLE PATCH: GetLabels is RLS-scoped, so another user's
// label id comes back exactly like one that never existed — absent — and both
// are refused the same way, naming the patch key. Refused rather than skipped,
// because a skipped id would report success for a label that was never applied.
//
// A label named both added and removed in one patch (labelIds/X: true beside
// keywords/<X's atom>: null) is refused too: ApplyEmailUpdate runs adds before
// removes, so it would "succeed" as a removal, which is a guess about intent
// rather than an answer to it.
func applyLabelIDPatches(ctx context.Context, tx *store.UserTx, up *store.EmailUpdate,
	patches map[ulid.ULID]bool, keys map[ulid.ULID]string) *SetError {
	if len(patches) == 0 {
		return nil
	}
	ids := make([]ulid.ULID, 0, len(patches))
	for id := range patches {
		ids = append(ids, id)
	}
	found, err := tx.GetLabels(ctx, ids)
	if err != nil {
		return &SetError{Type: setInvalidPatch, Description: err.Error()}
	}
	known := make(map[ulid.ULID]bool, len(found))
	for _, l := range found {
		known[l.ID] = true
	}
	// Deterministic: sorted ids, so the same bad patch names the same key.
	slices.SortFunc(ids, func(a, b ulid.ULID) int { return a.Compare(b) })
	for _, id := range ids {
		if !known[id] {
			return &SetError{Type: setInvalidPatch,
				Description: "no such label: " + id.String(), Properties: []string{keys[id]}}
		}
	}
	for _, id := range ids {
		present := patches[id]
		opposite := up.RemoveLabelIDs
		if !present {
			opposite = up.AddLabelIDs
		}
		if slices.Contains(opposite, id) {
			return &SetError{Type: setInvalidPatch,
				Description: "the patch both adds and removes label " + id.String(),
				Properties:  []string{keys[id]}}
		}
		if present {
			if !slices.Contains(up.AddLabelIDs, id) {
				up.AddLabelIDs = append(up.AddLabelIDs, id)
			}
		} else if !slices.Contains(up.RemoveLabelIDs, id) {
			up.RemoveLabelIDs = append(up.RemoveLabelIDs, id)
		}
	}
	return nil
}

// isPatchTrue reports whether a patch value denotes "present" (true) rather than
// removal (null or false).
func isPatchTrue(v json.RawMessage) bool {
	var b bool
	return json.Unmarshal(v, &b) == nil && b
}

// isJSONNull reports whether a raw message is the JSON null literal.
func isJSONNull(v json.RawMessage) bool {
	return strings.TrimSpace(string(v)) == "null"
}

func boolPtr(b bool) *bool { return &b }

// requestedID pairs a requested id string with its parse result, so a /get can
// report unparseable ids as notFound while preserving request order.
type requestedID struct {
	raw string
	id  ulid.ULID
	ok  bool
}

func (r requestedID) parsed() (ulid.ULID, bool) { return r.id, r.ok }

// parseIDs splits requested id strings into the valid ULIDs (for the store
// query), the immediately-not-found (unparseable) strings, and the ordered
// request list used to assemble the response.
func parseIDs(raw []string) (valid []ulid.ULID, notFound []string, requested []requestedID) {
	notFound = []string{}
	for _, s := range raw {
		id, err := ulid.Parse(s)
		if err != nil {
			requested = append(requested, requestedID{raw: s, ok: false})
			continue
		}
		valid = append(valid, id)
		requested = append(requested, requestedID{raw: s, id: id, ok: true})
	}
	return valid, notFound, requested
}
