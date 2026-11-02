// SPDX-License-Identifier: AGPL-3.0-only

package api

import "net/http"

// The capability set, in ONE place (RFC 8620 §2 / §3.3).
//
// Three things have to agree or the server is broken in a way clients cannot
// work around:
//
//  1. the top-level session `capabilities` map — a client may only `using` a URN
//     that appears HERE (a URN advertised per-account alone leaves its methods
//     unreachable: the bug this project shipped once, see service.go);
//  2. the per-account `accountCapabilities` map;
//  3. the `using` validator below, which REJECTS a request naming a URN we do
//     not advertise (urn:ietf:params:jmap:error:unknownCapability) and refuses a
//     method whose capability the client did not declare.
//
// They agree because they are all derived from capabilityObjects().

// capabilityObjects is the advertised capability set: URN -> the object the
// session publishes for it. This is the single source of truth for what this
// server supports.
func capabilityObjects() map[string]any {
	return map[string]any{
		capCore:            coreCapability(),
		capMail:            mailCapability(),
		capSubmission:      submissionCapability(),
		capLabels:          map[string]any{},
		capRules:           map[string]any{},
		capCompose:         map[string]any{},
		capAccounts:        map[string]any{},
		capContacts:        map[string]any{},
		capTriage:          map[string]any{},
		capOrganize:        map[string]any{},
		capRemoteContent:   map[string]any{},
		capComposeSnippets: map[string]any{},
		capUnsubscribe:     map[string]any{},
		capSenderAuth:      map[string]any{},
		capSavedSearches:   map[string]any{},
		capCategoryTabs:    categoryTabsCapability(),
		capAttachmentScan:  map[string]any{},
		capSearch:          searchCapability(),
		capDesk:            deskCapability(),
		capCalendar:        calendarCapability(),
		capOutOfOffice:     map[string]any{},
		capAPIKeys:         apiKeysCapability(),
		capDevices:         map[string]any{},
		// Whether this server can sign outbound mail at all is INSTANCE state (is
		// a crypto process wired?), not something the code knows, so the session
		// builder overwrites this entry — in BOTH the top-level and the
		// per-account map — with the live value (session.go).
		//
		// ⚠ FALSE IS THE FAIL-SAFE PLACEHOLDER, not a description of this server.
		// An instance that CAN sign but failed to thread the fact here
		// under-promises, and a client reading it merely omits a "Will sign"
		// badge; the other default would have every server promise a signature it
		// might be unable to produce.
		capPGP: pgpCapability(false, false, false),
		// The VAPID applicationServerKey is instance state, so the session builder
		// overwrites this entry with the live value (session.go). The empty object
		// here keeps the `using` validator and per-account map in agreement.
		capPush: map[string]any{},
	}
}

// accountCapabilityObjects is what one account advertises: everything except
// urn:ietf:params:jmap:core, which is a property of the SERVER, not of an
// account (RFC 8620 §2).
func accountCapabilityObjects() map[string]any {
	caps := capabilityObjects()
	delete(caps, capCore)
	return caps
}

// methodCapabilities maps every method this server implements to the capability
// URN a client MUST declare in `using` to call it (RFC 8620 §3.3: "The client
// MUST NOT use methods from a capability it did not list"). A method that is
// absent from this map is unreachable — fail closed: adding a method without
// naming its capability makes it un-callable rather than silently un-gated.
//
// Identity/* is deliberately urn:ietf:params:jmap:submission: RFC 8621 §6
// defines the Identity object under the submission capability, not under mail.
var methodCapabilities = map[string]string{
	"Core/echo": capCore,

	"Email/get":          capMail,
	"Email/query":        capMail,
	"Email/queryChanges": capMail,
	"Email/changes":      capMail,
	"Email/set":          capMail,
	"Mailbox/get":        capMail,
	"Mailbox/query":      capMail,
	"Mailbox/changes":    capMail,
	"Mailbox/set":        capMail,
	"Thread/get":         capMail,
	"Thread/changes":     capMail,
	"SearchSnippet/get":  capMail,

	"Identity/get":     capSubmission,
	"Identity/changes": capSubmission,
	"Identity/set":     capSubmission,

	"EmailSubmission/get": capSubmission,
	// #653: the reload bootstrap of the Outbox (`inOutbox`, a urn:mailroom:compose
	// filter extension on an RFC 8621 method). Gated like the RFC method it is.
	"EmailSubmission/query":   capSubmission,
	"EmailSubmission/changes": capSubmission,
	"EmailSubmission/set":     capSubmission,
	// Mailroom extension: the read-only send-quota meter (spec §5), gated on the
	// compose capability that already advertises the outbound daily cap.
	"EmailSubmission/getSendQuota": capCompose,

	"Label/get":     capLabels,
	"Label/query":   capLabels,
	"Label/changes": capLabels,
	"Label/set":     capLabels,

	"Rule/get":     capRules,
	"Rule/query":   capRules,
	"Rule/changes": capRules,
	"Rule/set":     capRules,
	"Rule/preview": capRules,
	"Rule/induce":  capRules,
	"Rule/tighten": capRules,
	"Rule/apply":   capRules,
	// The needs-a-category queue's other verb (#176): close a message's open
	// questions without filing it. Gated on capRules, NOT capMail, because
	// suggestions live under urn:mailroom:rules with Email.suggestedLabels —
	// a mail-only client keeps exactly RFC 8621's Email surface.
	"Email/dismissCategory": capRules,
	// Lifting an attachment quarantine (#396, ADR-027 §8). Gated on the
	// quarantine's own URN rather than capMail: a client that has never heard of
	// the quarantine has no business overriding one.
	"Email/releaseAttachment": capAttachmentScan,
	// Answering a calendar invitation (#725). Gated on the calendar URN, not
	// capMail: it SENDS mail, and a client that has never heard of invitations
	// has no business producing an iTIP REPLY.
	"Email/replyToInvite": capCalendar,

	// Remembered per-sender remote-content decisions (#202). No /query: the list is
	// a handful of rows a client reads whole, and a filterable query over "senders
	// whose images I load" would be answering a question nobody asks.
	"RemoteAllow/get":       capRemoteContent,
	"RemoteAllow/changes":   capRemoteContent,
	"RemoteAllow/set":       capRemoteContent,
	"RemoteContent/request": capRemoteContent,

	// Saved signatures and templates (#56). No /query: a compose window reads the
	// whole handful and picks locally.
	"ComposeSnippet/get":     capComposeSnippets,
	"ComposeSnippet/changes": capComposeSnippets,
	"ComposeSnippet/set":     capComposeSnippets,

	// Saved searches / smart folders (#57).
	"SavedSearch/get":     capSavedSearches,
	"SavedSearch/changes": capSavedSearches,
	"SavedSearch/set":     capSavedSearches,

	// Gmail-style category tabs (#219). No /query and no create/destroy: the
	// vocabulary is closed and five rows are read whole.
	"CategoryTab/get":     capCategoryTabs,
	"CategoryTab/changes": capCategoryTabs,
	"CategoryTab/set":     capCategoryTabs,

	// T1 triage controls (spec §8, #34): read the priority model's state, the
	// reset/rollback safety valve, and one-click importance correction. All gated
	// on the same triage capability that advertises the Email scoring properties.
	"Triage/get":     capTriage,
	"Triage/set":     capTriage,
	"Triage/correct": capTriage,
	"Triage/open":    capTriage,

	// Thread/set is a Mailroom extension — RFC 8621 defines no such method — so
	// it is gated by capOrganize, NOT by capMail alongside Thread/get and
	// Thread/changes. A mail-only client keeps exactly the RFC's Thread surface.
	"Thread/set": capOrganize,

	"Account/get":     capAccounts,
	"Account/query":   capAccounts,
	"Account/changes": capAccounts,
	"Account/set":     capAccounts,
	// Mailroom extension: the read-only account-health panel (spec §12), gated on
	// the same accounts capability the Account object lives under.
	"Account/getHealth": capAccounts,
	// Connecting a mailbox by signing in at its provider (#52, ADR-046).
	"Account/oauthStart":      capAccounts,
	"Account/oauthDisconnect": capAccounts,

	"Contact/get": capContacts,
	"Contact/set": capContacts,
	// #816 (ADR-041): merge two contacts by hand, and separate them again.
	"Contact/merge":    capContacts,
	"Contact/separate": capContacts,
	// #53: the clean-up ranking. Read-only and computed on demand, gated with
	// the contacts it is about.
	"SenderStats/get":      capContacts,
	"SenderStats/emailIds": capContacts,
	"Contact/query":        capContacts,
	"Contact/changes":      capContacts,

	// #448/#374 increment 1: the login's own keys, public facts only.
	"UserKey/get": capPGP,
	// #47 (ADR-038): which recipients have a usable encryption key. A computed
	// read; may fetch a key over WKD and store it.
	"RecipientKey/lookup": capPGP,
	// #47 (ADR-039): the user's trust/keep answer for a sender's changed key.
	"ContactKey/decide": capPGP,
	// #47 (ADR-038): the user imports a correspondent's public key.
	"ContactKey/import": capPGP,
	// #796: the held correspondent keys with their packet facts, and removal.
	"ContactKey/get": capPGP,
	"ContactKey/set": capPGP,
	// #815: a held key's history.
	"ContactKey/history": capPGP,
	// #818: re-fetch a held key from WKD and merge a renewal.
	"ContactKey/refresh": capPGP,
	// #796: own-key management (user_key_manage.go).
	"UserKey/generate": capPGP,
	"UserKey/import":   capPGP,
	"UserKey/set":      capPGP,
	"UserKey/extend":   capPGP,
	"UserKey/revoke":   capPGP,
	"UserKey/export":   capPGP,
	// v1.93 (#817): replace a bound key in one step.
	"UserKey/replace": capPGP,

	// Mailroom extension: Web Push device registration (spec §12, ADR-016).
	"WebPushSubscription/get": capPush,
	"WebPushSubscription/set": capPush,

	// Signed-in devices (#937, ADR-048 section 6): the user's own browser sessions.
	"SignedInDevice/get": capDevices,
	"SignedInDevice/set": capDevices,

	// The desk (#442): the user's own arrangement of Areas (fed by tabs and/or
	// labels), the desk-wide Surface/Drawer threshold and dwell periods, and
	// per-client-family display preferences. All gated on capDesk — a third
	// axis alongside tabs and labels, not a corner of either.
	"Area/get":        capDesk,
	"Area/changes":    capDesk,
	"Area/set":        capDesk,
	"Area/getSummary": capDesk,
	// #665: the whole desk — getSummary's counts plus every section's page —
	// from one resolution of every message's area.
	"Area/getDesk": capDesk,
	// "Always put mail from X in <area>?" after a manual move, and its "Not now".
	"Area/ruleSuggestion":        capDesk,
	"Area/dismissRuleSuggestion": capDesk,
	"LearnedPlacement/getStatus": capDesk,

	"DeskPreferences/get":     capDesk,
	"OutOfOffice/get":         capOutOfOffice,
	"OutOfOffice/set":         capOutOfOffice,
	"OutOfOffice/check":       capOutOfOffice,
	"DeskPreferences/changes": capDesk,
	"DeskPreferences/set":     capDesk,

	// Follow-up nudges (#58): computed on read for the desk's Follow up strip.
	"FollowUp/get": capDesk,
	"FollowUp/set": capDesk,

	"ClientPreferences/get":     capDesk,
	"ClientPreferences/changes": capDesk,
	"ClientPreferences/set":     capDesk,

	// Day-one setup (#446) — the desk being set up, not a separate capability.
	// Three of the six write nothing (census, preview, get): the report comes
	// before the apply, and the client cannot reach `apply` without having been
	// able to reach the two that show what it would do.
	"Setup/census":  capDesk,
	"Setup/preview": capDesk,
	"Setup/apply":   capDesk,
	"Setup/get":     capDesk,
	"Setup/changes": capDesk,
	"Setup/undo":    capDesk,

	"ApiKey/get":        capAPIKeys,
	"ApiKey/set":        capAPIKeys,
	"ApiKey/getEvents":  capAPIKeys,
	"PendingAction/get": capAPIKeys,
	"PendingAction/set": capAPIKeys,
}

// errUnknownCapability is the RFC 8620 §3.6.1 request-level error for a `using`
// list naming a capability this server does not support.
const errUnknownCapability = "unknownCapability"

// validateUsing checks the request's `using` list against what we advertise. An
// unadvertised URN is a REQUEST-level failure (the whole request is rejected
// with a 400), because the client is asking for semantics this server does not
// have and every method call in the batch is therefore suspect.
func validateUsing(using []string) *requestError {
	advertised := capabilityObjects()
	for _, urn := range using {
		if _, ok := advertised[urn]; !ok {
			e := newRequestError(errUnknownCapability, http.StatusBadRequest,
				"this server does not support the capability "+urn)
			return &e
		}
	}
	return nil
}

// usingSet indexes a validated `using` list for the per-method gate in dispatch.
func usingSet(using []string) map[string]bool {
	out := make(map[string]bool, len(using))
	for _, urn := range using {
		out[urn] = true
	}
	return out
}

// mayCall reports whether a client that declared `using` may call method name.
// An unknown method, and a known method whose capability was not declared, are
// the SAME answer to the client (unknownMethod, RFC 8620 §3.2): "this method
// does not exist for you". Undeclared capabilities therefore cannot be probed.
//
// ── WHAT THE `using` GATE IS, AND WHAT IT IS NOT (Forgejo #310) ──────────────
//
// THIS FUNCTION IS THE WHOLE MECHANISM. There is exactly one DECIDING call site — the
// dispatch loop in request.go (timingName repeats the question only to label a
// timing) — and it decides one thing: whether a METHOD runs.
// Per-PROPERTY gating does not exist here and cannot exist here without a
// signature change: methodFn is
//
//	func(ctx, userID ulid.ULID, args json.RawMessage) (json.RawMessage, *MethodError)
//
// and does not receive `using` at all. A handler therefore cannot know what the
// client declared, and emailGet builds its property set purely from the client's
// own `properties` array. So a client that asks for `priorityScore` WITHOUT
// listing urn:mailroom:triage gets `priorityScore` — it needed
// urn:ietf:params:jmap:mail to reach Email/get, and nothing after that consults
// the list again.
//
// Declaring a Mailroom capability therefore buys exactly two things:
//
//  1. For a URN that gates methods (see methodCapabilities), it is a HARD gate on
//     those methods. Not listing urn:mailroom:rules really does make Rule/set
//     unreachable, and this is enforced, tested, and fails closed.
//  2. For every URN, it is what RFC 8620 §3.3 REQUIRES a conformant client to
//     send before using the semantics it names. It is a declaration of intent
//     that lets a server evolve; it is not an access-control decision.
//
// It buys nothing else, and in particular it does not withhold a property from a
// client that under-declares. That is a conformance gap in the CLIENT, not a hole
// in this server: the security boundary on every one of these properties is the
// authenticated user id taken from the request context (never a parameter, spec
// §10) plus row-level security in the database. `using` is negotiation, and RFC
// 8620 does not make it authorization either.
//
// ⚠ SEVERAL COMMENTS IN THIS PACKAGE USED TO DESCRIBE IT AS IF IT DID. They said
// a property was "opt-in under capTriage" or gated "exactly like the compose
// extension", which reads as "declaring the URN is what releases the property".
// It never was. capUnsubscribe's comment in service.go was the one that got this
// right; #310 made the others match it rather than making the code match them —
// real per-property gating is a signature change across every handler AND a
// behaviour change for existing under-declaring clients (they would start losing
// properties they receive today), bought for no security gain, since RLS already
// provides the property this would be pretending to provide.
func mayCall(using map[string]bool, name string) bool {
	urn, ok := methodCapabilities[name]
	return ok && using[urn]
}

// capabilitiesGatingNoMethod is the set of advertised URNs that appear in NO
// methodCapabilities entry, and therefore gate nothing at all (Forgejo #310).
//
// They are not a bug and they are not dead weight — they are CONFORMANCE
// SIGNALS. Each one names a semantic extension that rides on a method some other
// capability already gates:
//
//   - urn:mailroom:search advertises the query SYNTAX (the operator vocabulary in
//     searchCapability) as a `query` FilterCondition on Email/query, which is
//     gated by urn:ietf:params:jmap:mail. There is no Search/* method to gate.
//   - urn:mailroom:unsubscribe advertises the `unsubscribe` PROPERTY on Email,
//     read through Email/get — likewise gated by the mail capability. Nothing
//     here sends an unsubscribe, so there is no method for it either.
//   - urn:mailroom:senderauth advertises the `authResults` PROPERTY on Email —
//     the same shape again. Mailroom verifies nothing itself; it relays what the
//     receiving servers already wrote into Authentication-Results, so there is no
//     method to gate and nothing for a client to call.
//
// A client that means to use either MUST list it (RFC 8620 §3.3), and one that
// forgets is non-conformant rather than blocked — see mayCall above for why that
// distinction is the honest description of this whole mechanism.
//
// The set is pinned so that a NEW capability which gates nothing is a decision
// somebody made on purpose rather than a method table someone forgot to update:
// TestCapabilitiesGatingNoMethodArePinned asserts this list against the real
// tables in both directions.
var capabilitiesGatingNoMethod = map[string]bool{
	capSearch:      true,
	capUnsubscribe: true,
	capSenderAuth:  true,
}
