// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// THE SCOPE TABLE (#49, spec §13a, ADR-042 §3).
//
// Every JMAP method an API key can reach is reachable because a line below says
// so, with the scopes it needs. Everything else is refused to a key — a method
// missing from this table is refused exactly like one marked never — and
// TestEveryMethodHasAKeyPolicy walks the dispatcher's full method list and
// fails if any method has no line here at all. That test is the guard against a
// new method silently becoming key-reachable: adding a method means deciding,
// in this file, what a key may do with it.
//
// Two kinds of "no":
//   - sessionOnly: refused to keys BY DESIGN — key management (a key must never
//     mint or widen a key), private-key operations, credentials, identities,
//     anything that sends without a submission, account and admin surfaces.
//   - notForKeys: not opened to keys in this release. Nothing about it is
//     dangerous in principle; opening one is a reviewed change to this table.

// The closed scope set (spec §13a). The store's CHECK constraint names the same
// ten strings.
const (
	scopeMailRead      = "mail:read"
	scopeMailSend      = "mail:send"
	scopeMailDraft     = "mail:draft"
	scopeMailMove      = "mail:move"
	scopeMailDelete    = "mail:delete"
	scopeLabelsRead    = "labels:read"
	scopeLabelsWrite   = "labels:write"
	scopeSearchRead    = "search:read"
	scopeContactsRead  = "contacts:read"
	scopeContactsWrite = "contacts:write"
)

// allScopes is the closed set, in the order the session advertises it.
var allScopes = []string{scopeMailRead, scopeMailSend, scopeMailDraft, scopeMailMove, scopeMailDelete,
	scopeLabelsRead, scopeLabelsWrite, scopeSearchRead, scopeContactsRead, scopeContactsWrite}

// destructiveScopes may wait for a human (confirm mode). Both default ON.
var destructiveScopes = []string{scopeMailSend, scopeMailDelete}

func isKnownScope(s string) bool {
	for _, k := range allScopes {
		if k == s {
			return true
		}
	}
	return false
}

func isDestructiveScope(s string) bool { return s == scopeMailSend || s == scopeMailDelete }

// keyNeeds is what one call by a key requires and costs.
type keyNeeds struct {
	scopes []string
	// cost is what the call draws from the key's rate and daily budget: 1 for a
	// read, the number of objects for a /set (ADR-042 §5).
	cost    int
	deletes int
	sends   int
	// hold is "send" or "delete" when the call is a destructive one that a key
	// in confirm mode must not execute without a human.
	hold string
	// summary is the audit's scope-level summary: counts and ids, never content.
	summary map[string]any
}

// keyPolicy is one method's line in the table.
type keyPolicy struct {
	// scopes, all required. Ignored when needs is set.
	scopes []string
	// needs computes what a call requires from its arguments, for methods
	// where one call can need several scopes (Email/set, EmailSubmission/set,
	// Email/query).
	needs func(ctx context.Context, a *API, userID ulid.ULID, p *keyPrincipal, args json.RawMessage) (keyNeeds, *MethodError)
	// isSet costs the call by its object count rather than 1.
	isSet bool
	// accountRestricted: the method honours a key's account restriction inside
	// its own query/get/set path. A restricted key is refused every method
	// without it.
	accountRestricted bool
	// ownPending: PendingAction/get, readable by a key for its OWN requests.
	ownPending bool

	sessionOnly bool
	notForKeys  bool
	// why is said in the refusal and in the docs.
	why string
}

func read(scopes ...string) keyPolicy { return keyPolicy{scopes: scopes} }

func readRestricted(scopes ...string) keyPolicy {
	return keyPolicy{scopes: scopes, accountRestricted: true}
}

func sessionOnly(why string) keyPolicy { return keyPolicy{sessionOnly: true, why: why} }

func notForKeys(why string) keyPolicy { return keyPolicy{notForKeys: true, why: why} }

const (
	whyKeyManagement = "key management is session-only: a key can never create, widen or approve keys"
	whyPrivateKeys   = "operations with your private keys are session-only"
	whyNotOpened     = "not available to API keys in this release"
	whyCredentials   = "accounts and their credentials are session-only"
	whySessions      = "signed-in devices are a person's browser sessions; a key is not one and must never end one"
	whySendsOutside  = "it sends mail outside EmailSubmission/set, where the send limits and approvals live"
)

// keyPolicies is the table. Keep it sorted by object, then method.
var keyPolicies = map[string]keyPolicy{
	"Core/echo": read(),

	// Mail: read. Email/get and Email/query honour the account restriction in
	// the store (MessagesInAccounts, EmailQuery.RestrictAccounts); the /changes
	// methods would enumerate ids across every account, so a restricted key is
	// refused them.
	"Email/get":          readRestricted(scopeMailRead),
	"Email/query":        {needs: emailQueryNeeds, accountRestricted: true},
	"Email/changes":      read(scopeMailRead),
	"Email/queryChanges": notForKeys(whyNotOpened),
	"SearchSnippet/get":  read(scopeMailRead, scopeSearchRead),
	"Thread/get":         read(scopeMailRead),
	"Thread/changes":     read(scopeMailRead),
	"Mailbox/get":        readRestricted(scopeMailRead),
	"Mailbox/query":      readRestricted(scopeMailRead),
	"Mailbox/changes":    read(scopeMailRead),
	"Identity/get":       readRestricted(scopeMailRead),
	"Identity/changes":   read(scopeMailRead),

	// Mail: write. One Email/set can draft, organise and delete at once, so it
	// needs every scope its contents need (emailSetNeeds), and a key's destroy is
	// always the soft delete to Trash (ADR-009; emailSet's destroy loop).
	"Email/set":           {needs: emailSetNeeds, accountRestricted: true},
	"EmailSubmission/set": {needs: submissionSetNeeds, accountRestricted: true},

	"EmailSubmission/get":          read(scopeMailRead),
	"EmailSubmission/query":        read(scopeMailRead),
	"EmailSubmission/changes":      read(scopeMailRead),
	"EmailSubmission/getSendQuota": read(scopeMailSend),

	// Labels are the user's own vocabulary, not mail of any account, so reading
	// them is open to a restricted key; changing them would change every
	// account's organisation, so writing them is not.
	"Label/get":     readRestricted(scopeLabelsRead),
	"Label/query":   readRestricted(scopeLabelsRead),
	"Label/changes": readRestricted(scopeLabelsRead),
	"Label/set":     {scopes: []string{scopeLabelsWrite}, isSet: true},

	// Areas (#49) are the user's own arrangement of the desk, like labels:
	// a name, a position and the label that IS the area, nothing of any
	// account's mail, so reading them is open to a restricted key. A key files
	// mail into an area with Email/set (`labelIds/<the area's labelId>`, under
	// mail:move, in emailUpdateFieldScopes), whose own path keeps to the
	// restriction; and what a key files is never the user's placement evidence
	// (store.LabelByAgent). Creating, renaming and deleting areas (Area/set),
	// the summary, the desk and Area/changes stay closed.
	"Area/get": readRestricted(scopeMailRead),

	// Contacts are derived from every account's mail, so a restricted key is
	// refused them all.
	"Contact/get":     read(scopeContactsRead),
	"Contact/query":   read(scopeContactsRead),
	"Contact/changes": read(scopeContactsRead),
	"Contact/set":     {scopes: []string{scopeContactsWrite}, isSet: true},

	// The approval queue: a key may poll its OWN requests; deciding is a human's.
	"PendingAction/get": {ownPending: true, accountRestricted: true},
	"PendingAction/set": sessionOnly("approving or denying a request is for a person, never a key"),

	"ApiKey/get":       sessionOnly(whyKeyManagement),
	"ApiKey/set":       sessionOnly(whyKeyManagement),
	"ApiKey/getEvents": sessionOnly(whyKeyManagement),

	"UserKey/get":         sessionOnly(whyPrivateKeys),
	"UserKey/generate":    sessionOnly(whyPrivateKeys),
	"UserKey/import":      sessionOnly(whyPrivateKeys),
	"UserKey/set":         sessionOnly(whyPrivateKeys),
	"UserKey/extend":      sessionOnly(whyPrivateKeys),
	"UserKey/revoke":      sessionOnly(whyPrivateKeys),
	"UserKey/export":      sessionOnly(whyPrivateKeys),
	"UserKey/replace":     sessionOnly(whyPrivateKeys),
	"ContactKey/decide":   sessionOnly("trusting or refusing a correspondent's key is a person's decision"),
	"ContactKey/import":   sessionOnly("trusting or refusing a correspondent's key is a person's decision"),
	"ContactKey/set":      sessionOnly("trusting or refusing a correspondent's key is a person's decision"),
	"ContactKey/get":      notForKeys(whyNotOpened),
	"ContactKey/history":  notForKeys(whyNotOpened),
	"ContactKey/refresh":  notForKeys(whyNotOpened),
	"RecipientKey/lookup": notForKeys(whyNotOpened),

	"Identity/set":      sessionOnly("identities decide which addresses you send as"),
	"Account/get":       sessionOnly(whyCredentials),
	"Account/query":     sessionOnly(whyCredentials),
	"Account/changes":   sessionOnly(whyCredentials),
	"Account/set":       sessionOnly(whyCredentials),
	"Account/getHealth": sessionOnly(whyCredentials),
	"Account/oauthStart": sessionOnly("connecting a mailbox grants this server full control of it; " +
		"a person signs in at the provider"),
	"Account/oauthDisconnect": sessionOnly(whyCredentials),

	"Email/replyToInvite":     sessionOnly(whySendsOutside),
	"OutOfOffice/set":         sessionOnly(whySendsOutside),
	"OutOfOffice/get":         notForKeys(whyNotOpened),
	"OutOfOffice/check":       notForKeys(whyNotOpened),
	"Email/releaseAttachment": sessionOnly("releasing a quarantined attachment is a person's decision"),
	"Mailbox/set":             sessionOnly("folders are created and deleted by a person; a folder delete can take its mail with it"),
	"WebPushSubscription/get": sessionOnly("push devices are registered by a browser"),
	"WebPushSubscription/set": sessionOnly("push devices are registered by a browser"),
	"SignedInDevice/get":      sessionOnly(whySessions),
	"SignedInDevice/set":      sessionOnly(whySessions),
	"Rule/set":                sessionOnly("a rule acts on future mail without asking; a person writes it"),
	"Rule/apply":              sessionOnly("a rule acts on mail without asking; a person runs it"),
	"Triage/set":              sessionOnly("resetting or rolling back the priority model is a person's decision"),
	"Triage/open":             sessionOnly("it records that a PERSON read a message; an agent's reads must not train the priority model"),
	"Setup/apply":             sessionOnly("day-one setup reorganises the whole mailbox"),
	"Setup/undo":              sessionOnly("day-one setup reorganises the whole mailbox"),

	"Email/dismissCategory":     notForKeys(whyNotOpened),
	"Thread/set":                notForKeys(whyNotOpened),
	"Contact/merge":             notForKeys(whyNotOpened),
	"Contact/separate":          notForKeys(whyNotOpened),
	"SenderStats/get":           notForKeys(whyNotOpened),
	"SenderStats/emailIds":      notForKeys(whyNotOpened),
	"FollowUp/get":              notForKeys(whyNotOpened),
	"FollowUp/set":              notForKeys(whyNotOpened),
	"ComposeSnippet/get":        notForKeys(whyNotOpened),
	"ComposeSnippet/changes":    notForKeys(whyNotOpened),
	"ComposeSnippet/set":        notForKeys(whyNotOpened),
	"SavedSearch/get":           notForKeys(whyNotOpened),
	"SavedSearch/changes":       notForKeys(whyNotOpened),
	"SavedSearch/set":           notForKeys(whyNotOpened),
	"CategoryTab/get":           notForKeys(whyNotOpened),
	"CategoryTab/changes":       notForKeys(whyNotOpened),
	"CategoryTab/set":           notForKeys(whyNotOpened),
	"RemoteAllow/get":           notForKeys(whyNotOpened),
	"RemoteAllow/changes":       notForKeys(whyNotOpened),
	"RemoteAllow/set":           notForKeys(whyNotOpened),
	"RemoteContent/request":     notForKeys(whyNotOpened),
	"Rule/get":                  notForKeys(whyNotOpened),
	"Rule/query":                notForKeys(whyNotOpened),
	"Rule/changes":              notForKeys(whyNotOpened),
	"Rule/preview":              notForKeys(whyNotOpened),
	"Rule/induce":               notForKeys(whyNotOpened),
	"Rule/tighten":              notForKeys(whyNotOpened),
	"Triage/get":                notForKeys(whyNotOpened),
	"Triage/correct":            notForKeys(whyNotOpened),
	"Area/changes":              notForKeys(whyNotOpened),
	"Area/set":                  notForKeys(whyNotOpened),
	"Area/getSummary":           notForKeys(whyNotOpened),
	"Area/getDesk":              notForKeys(whyNotOpened),
	"DeskPreferences/get":       notForKeys(whyNotOpened),
	"DeskPreferences/changes":   notForKeys(whyNotOpened),
	"DeskPreferences/set":       notForKeys(whyNotOpened),
	"ClientPreferences/get":     notForKeys(whyNotOpened),
	"ClientPreferences/changes": notForKeys(whyNotOpened),
	"ClientPreferences/set":     notForKeys(whyNotOpened),
	"Setup/census":              notForKeys(whyNotOpened),
	"Setup/preview":             notForKeys(whyNotOpened),
	"Setup/get":                 notForKeys(whyNotOpened),
	"Setup/changes":             notForKeys(whyNotOpened),
}

// keyReachable reports whether the table lets a key call the method at all.
func (p keyPolicy) keyReachable() bool { return !p.sessionOnly && !p.notForKeys }

// staticNeeds is the needs of a method with fixed scopes.
func (p keyPolicy) staticNeeds(name string, args json.RawMessage) keyNeeds {
	n := keyNeeds{scopes: p.scopes, cost: readCost(name, args)}
	if p.isSet {
		n.cost = setObjectCount(args)
	}
	return n
}

// ReadUnitObjects is how many objects one unit of a key's budget reads (#49
// review, ADR-042 §5): a /get costs ceil(ids/50), a /query ceil(limit/50), a
// /changes ceil(maxChanges/50), so the daily budget bounds the data a key
// moves, not only its call count. A read whose size the arguments do not
// state (ids: null) is charged 1 up front and topped up from the size of the
// answer (keyReadTopUp). A key's /query and /changes are never unbounded: see
// keyQueryLimit and keyChangesLimit.
const ReadUnitObjects = 50

func unitsFor(n int) int { return max(1, (n+ReadUnitObjects-1)/ReadUnitObjects) }

// readCost is the up-front cost of a read, from its arguments. A /query or
// /changes is charged for the bound it will RUN with (keyQueryLimit,
// keyChangesLimit), whatever it asked for, and before any other argument is
// looked at: an "ids" argument on a query cannot make it cheaper.
func readCost(name string, args json.RawMessage) int {
	var a struct {
		IDs        *[]string `json:"ids"`
		EmailIDs   *[]string `json:"emailIds"`
		Limit      *int      `json:"limit"`
		MaxChanges *int      `json:"maxChanges"`
	}
	_ = json.Unmarshal(args, &a)
	switch {
	case strings.HasSuffix(name, "/query"):
		return unitsFor(keyQueryLimitFor(name, a.Limit))
	case strings.HasSuffix(name, "/changes"):
		return unitsFor(keyChangesLimit(a.MaxChanges))
	case a.IDs != nil:
		return unitsFor(len(*a.IDs))
	case a.EmailIDs != nil:
		return unitsFor(len(*a.EmailIDs))
	}
	return 1
}

// readAnswerCost is what a read actually moved: the objects in its list, or
// the ids a query returned. A Thread/get is charged by the email ids its
// threads list (#49 review): one thread can name thousands, and a charge per
// thread would let a key enumerate a mailbox for the price of a few calls.
func readAnswerCost(name string, result json.RawMessage) int {
	if name == "Thread/get" {
		var t struct {
			List []struct {
				EmailIDs []json.RawMessage `json:"emailIds"`
			} `json:"list"`
		}
		_ = json.Unmarshal(result, &t)
		n := 0
		for _, th := range t.List {
			n += len(th.EmailIDs)
		}
		return unitsFor(max(n, len(t.List)))
	}
	if strings.HasSuffix(name, "/changes") {
		// A /changes answer moves ids: charged by every id it names (#49
		// review), not as one object.
		var c struct {
			Created   []json.RawMessage `json:"created"`
			Updated   []json.RawMessage `json:"updated"`
			Destroyed []json.RawMessage `json:"destroyed"`
		}
		_ = json.Unmarshal(result, &c)
		return unitsFor(len(c.Created) + len(c.Updated) + len(c.Destroyed))
	}
	var r struct {
		List []json.RawMessage `json:"list"`
		IDs  []json.RawMessage `json:"ids"`
	}
	_ = json.Unmarshal(result, &r)
	return unitsFor(max(len(r.List), len(r.IDs)))
}

// A key's /query always runs with a limit (#49 review for Email/query, #850
// for every other): KeyQueryDefaultLimit when it sends none, and never more
// than KeyQueryMaxLimit; the server's limit is echoed in the answer whenever
// it differs from the request (RFC 8620 §5.5). Its cost is then known, and
// charged, before it runs. Charging a limit-less query 1 and topping it up
// from the answer let concurrent queries all be admitted before any top-up
// landed. TestEveryKeyQueryIsBounded holds every key-reachable /query and
// /changes in the table to this.
const (
	KeyQueryDefaultLimit = ReadUnitObjects
	KeyQueryMaxLimit     = 10 * ReadUnitObjects
)

// keyQueryLimit is the limit a key's /query runs with.
func keyQueryLimit(requested *int) int {
	if requested == nil {
		return KeyQueryDefaultLimit
	}
	return min(max(*requested, 0), KeyQueryMaxLimit)
}

// keyQueryMax names the /query methods whose own largest page is smaller
// than KeyQueryMaxLimit, so a key is charged for the page it can get.
var keyQueryMax = map[string]int{"Contact/query": store.MaxContactQueryLimit}

// keyQueryLimitFor is the limit a key's call of method runs with.
func keyQueryLimitFor(method string, requested *int) int {
	lim := keyQueryLimit(requested)
	if m, ok := keyQueryMax[method]; ok {
		lim = min(lim, m)
	}
	return lim
}

// keyServerLimit is RFC 8620 §5.5's "limit" for a key's /query answer: the
// limit it ran with when that is not what was asked (none sent, or more than
// the maximum), nil otherwise.
func keyServerLimit(method string, requested *int) *int {
	lim := keyQueryLimitFor(method, requested)
	if requested != nil && *requested == lim {
		return nil
	}
	return &lim
}

// keyChangesLimit is the maxChanges a key's /changes runs with (#852): the
// default page when it sends none, and never more than KeyQueryMaxLimit. A
// /changes answer is otherwise every change since the state it names, of
// unbounded size and charged only after it is built. RFC 8620 §5.2 lets the
// server return fewer changes than asked; hasMoreChanges and the intermediate
// newState (store.Changes' maxChanges path) carry the rest. A maxChanges that
// is not positive is the method's invalidArguments, not clamped here.
func keyChangesLimit(requested *int) int {
	if requested == nil {
		return KeyQueryDefaultLimit
	}
	return min(max(*requested, 0), KeyQueryMaxLimit)
}

// keyQueryWindow applies a key's position and limit to a query whose full id
// list is built in memory (Mailbox/query, Label/query: small sets the session
// windows itself), RFC 8620 §5.5: a negative position counts from the end,
// one past the end is an empty page. It returns the page and the position of
// its first id.
func keyQueryWindow(method string, ids []ulid.ULID, position int, requested *int) ([]ulid.ULID, int) {
	if position < 0 {
		position = max(len(ids)+position, 0)
	}
	position = min(position, len(ids))
	end := min(position+keyQueryLimitFor(method, requested), len(ids))
	return ids[position:end], position
}

// setObjectCount is the number of objects a standard /set call touches (at
// least 1, so an empty /set still costs a call).
func setObjectCount(args json.RawMessage) int {
	var s struct {
		Create  map[string]json.RawMessage `json:"create"`
		Update  map[string]json.RawMessage `json:"update"`
		Destroy []string                   `json:"destroy"`
	}
	_ = json.Unmarshal(args, &s)
	return max(1, len(s.Create)+len(s.Update)+len(s.Destroy))
}

// emailQueryNeeds: reading is mail:read; a full-text condition also needs
// search:read. The cost is the limit the query will run with (keyQueryLimit).
func emailQueryNeeds(_ context.Context, _ *API, _ ulid.ULID, _ *keyPrincipal, raw json.RawMessage) (keyNeeds, *MethodError) {
	var args emailQueryArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return keyNeeds{}, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	plan, mErr := parseEmailQuery(args)
	if mErr != nil {
		return keyNeeds{}, mErr
	}
	n := keyNeeds{scopes: []string{scopeMailRead}, cost: unitsFor(keyQueryLimit(args.Limit))}
	if !plan.text.empty() {
		n.scopes = append(n.scopes, scopeSearchRead)
	}
	return n, nil
}

// emailUpdateFieldScopes classifies EVERY field of store.EmailUpdate. A key's
// Email/set update needs the scope of each field its patch sets. A field that
// is not in this map is refused (emailUpdateScopes), and
// TestEveryEmailUpdateFieldIsClassified fails when store.EmailUpdate grows a
// field this map does not name — so a new kind of change cannot ride in under
// a scope that never anticipated it.
var emailUpdateFieldScopes = map[string]string{
	"Seen":     scopeMailMove,
	"Flagged":  scopeMailMove,
	"Answered": scopeMailMove,
	// "Draft" ($draft) is deliberately ABSENT (#49 review): no key may change
	// whether a message is a draft. Making one a draft arms a permanent delete
	// and a re-send; emailUpdateScopes refuses an unclassified field, and
	// TestEveryEmailUpdateFieldIsClassified holds that absence in place.
	// Only "not junk" is settable, and it moves the message to the inbox.
	"Junk":            scopeMailMove,
	"Pinned":          scopeMailMove,
	"SnoozeUntil":     scopeMailMove,
	"Unsnooze":        scopeMailMove,
	"AddKeywords":     scopeMailMove,
	"RemoveKeywords":  scopeMailMove,
	"AddLabelIDs":     scopeMailMove,
	"RemoveLabelIDs":  scopeMailMove,
	"LabelSource":     scopeMailMove,
	"AddLabelSources": scopeMailMove,
	"RemovalIsUndo":   scopeMailMove,
	// Into Trash by patch (mailboxIds/<trash>: true) is a delete, whatever the
	// call calls it.
	"Trash": scopeMailDelete,
	// Out of Trash is a move: it undoes a delete, it cannot destroy anything.
	"Restore":         scopeMailMove,
	"MoveToRole":      scopeMailMove,
	"MoveToMailboxID": scopeMailMove,
}

// keyRefusedEmailUpdateFields are the store.EmailUpdate fields NO key may set,
// by decision rather than by omission; TestEveryEmailUpdateFieldIsClassified
// requires every field to be in exactly one of the two maps.
var keyRefusedEmailUpdateFields = map[string]string{
	"Draft": "whether a message is a draft decides what a destroy erases for good and what a send sends",
}

// emailUpdateScopes is the set of scopes an EmailUpdate needs, or ok=false when
// it sets a field the table does not classify (deny by default).
func emailUpdateScopes(up store.EmailUpdate) (scopes []string, ok bool) {
	set := map[string]bool{}
	v := reflect.ValueOf(up)
	t := v.Type()
	for i := range t.NumField() {
		if v.Field(i).IsZero() {
			continue
		}
		scope, known := emailUpdateFieldScopes[t.Field(i).Name]
		if !known {
			return nil, false
		}
		set[scope] = true
	}
	for s := range set {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	return scopes, true
}

// emailSetNeeds reads one Email/set and returns every scope its contents need.
// Patches are parsed by parseEmailPatch — the SAME function the method itself
// runs — inside a read-only transaction, so what is classified is exactly what
// would execute rather than a second reading of the JSON.
func emailSetNeeds(ctx context.Context, a *API, userID ulid.ULID, p *keyPrincipal, raw json.RawMessage) (keyNeeds, *MethodError) {
	var args emailSetArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return keyNeeds{}, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	scopes := map[string]bool{}
	n := keyNeeds{cost: max(1, len(args.Create)+len(args.Update)+len(args.Destroy)+len(args.DiscardDrafts))}
	// A create writes a draft — a new one, a repeat collapsed into the key's
	// earlier one, or a replacement of the key's own draft (replacesEmailId).
	// discardDrafts moves the key's OWN unsent drafts to Trash. All of it is
	// the key's own unsent work, so all of it is mail:draft, never mail:delete,
	// and none of it counts as a delete or is held for approval (#872,
	// ADR-045 §3); the method itself refuses anything that is not the key's.
	if len(args.Create) > 0 || len(args.DiscardDrafts) > 0 {
		scopes[scopeMailDraft] = true
	}
	trashedByPatch, refusedKeywords := 0, 0
	if len(args.Update) > 0 {
		mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
			for _, patch := range args.Update {
				up, serr := parseEmailPatch(ctx, tx, patch)
				if serr != nil {
					// The method reports this object's error itself; it executes
					// nothing for it, so it needs nothing. A refused keyword (a
					// system flag, a reserved token) is counted on the call's
					// audit row, which is always written (store.CriticalRefusalReasons).
					for _, prop := range serr.Properties {
						if strings.HasPrefix(prop, "keywords") {
							refusedKeywords++
							break
						}
					}
					continue
				}
				need, ok := emailUpdateScopes(up)
				if !ok {
					return &MethodError{Type: errForbidden,
						Description: "this change is not available to API keys"}
				}
				if len(need) == 0 {
					need = []string{scopeMailMove}
				}
				for _, s := range need {
					scopes[s] = true
				}
				if up.Trash {
					trashedByPatch++
				}
			}
			return errMethodRollbackErr
		})
		if mErr != nil && mErr != errMethodRollbackErr {
			return keyNeeds{}, mErr
		}
	}
	if len(args.Destroy) > 0 {
		scopes[scopeMailDelete] = true
	}
	n.deletes = len(args.Destroy) + trashedByPatch
	for s := range scopes {
		n.scopes = append(n.scopes, s)
	}
	sort.Strings(n.scopes)
	n.summary = map[string]any{"create": len(args.Create), "update": len(args.Update),
		"destroy": len(args.Destroy)}
	if len(args.DiscardDrafts) > 0 {
		n.summary["discard"] = len(args.DiscardDrafts)
		n.summary["discardIds"] = idSample(args.DiscardDrafts)
	}
	if refusedKeywords > 0 {
		n.summary["refusedKeywords"] = refusedKeywords
	}
	if ids := idSample(args.Destroy); len(ids) > 0 {
		n.summary["destroyIds"] = ids
	}
	if ids := idSample(mapKeys(args.Update)); len(ids) > 0 {
		n.summary["updateIds"] = ids
	}
	if p != nil && p.confirms(scopeMailDelete) && n.deletes > 0 && p.approval == nil {
		// Confirm mode holds the delete, so the held request must be exactly
		// that: a destroy and nothing else. A delete written as a patch, or one
		// mixed with other changes, is refused rather than half-executed.
		if len(args.Create) > 0 || len(args.Update) > 0 || len(args.DiscardDrafts) > 0 {
			return keyNeeds{}, &MethodError{Type: errInvalidArguments,
				Description: "this key's deletes wait for approval: send them as destroy, in an Email/set of their own"}
		}
		n.hold = holdDelete
	}
	return n, nil
}

// errMethodRollbackErr makes a read-only classification transaction roll back.
var errMethodRollbackErr = &MethodError{Type: "rollback"}

// submissionSetNeeds: a create is a send; a create that archives on success is
// also a move. onSuccessUpdateEmail and onSuccessDestroyEmail are refused to
// keys: the server files the sent copy itself, and a destroy chained to a send
// would be a delete no scope check saw.
func submissionSetNeeds(_ context.Context, _ *API, _ ulid.ULID, p *keyPrincipal, raw json.RawMessage) (keyNeeds, *MethodError) {
	var args submissionSetArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return keyNeeds{}, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	// Changing or cancelling a submission — the user's scheduled sends and
	// Undo-Send windows included — is not available to keys (#49 review); a key
	// can only create a submission, and only from a draft it wrote itself
	// (createSubmission, app.api_key_drafts).
	if len(args.Update) > 0 || len(args.Destroy) > 0 {
		return keyNeeds{}, &MethodError{Type: errForbidden,
			Description: "changing or cancelling a submission is not available to API keys"}
	}
	if len(args.OnSuccessUpdateEmail) > 0 || len(args.OnSuccessDestroyEmail) > 0 {
		return keyNeeds{}, &MethodError{Type: errForbidden,
			Description: "onSuccessUpdateEmail and onSuccessDestroyEmail are not available to API keys; " +
				"the server files the sent copy itself"}
	}
	scopes := map[string]bool{scopeMailSend: true}
	var emailIDs []string
	for _, c := range args.Create {
		var sc submissionCreate
		if err := json.Unmarshal(c, &sc); err == nil {
			if len(sc.ArchiveOnSuccess) > 0 {
				scopes[scopeMailMove] = true
			}
			emailIDs = append(emailIDs, sc.EmailID)
		}
	}
	n := keyNeeds{cost: max(1, len(args.Create)+len(args.Update)+len(args.Destroy)), sends: len(args.Create)}
	for s := range scopes {
		n.scopes = append(n.scopes, s)
	}
	sort.Strings(n.scopes)
	n.summary = map[string]any{"create": len(args.Create), "update": len(args.Update)}
	if ids := idSample(emailIDs); len(ids) > 0 {
		n.summary["emailIds"] = ids
	}
	if p != nil && p.confirms(scopeMailSend) && len(args.Create) > 0 && p.approval == nil {
		if len(args.Create) != 1 || len(args.Update) > 0 || len(args.Destroy) > 0 {
			return keyNeeds{}, &MethodError{Type: errInvalidArguments,
				Description: "this key's sends wait for approval: send one EmailSubmission create per call"}
		}
		n.hold = holdSend
	}
	return n, nil
}

const (
	holdSend   = "send"
	holdDelete = "delete"
)

// maxSummaryIDs bounds how many ids one audit row lists (the count is exact).
const maxSummaryIDs = 50

func idSample(ids []string) []string {
	if len(ids) > maxSummaryIDs {
		return ids[:maxSummaryIDs]
	}
	return ids
}

func mapKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
