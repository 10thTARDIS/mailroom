// SPDX-License-Identifier: AGPL-3.0-only

package api

// The FORCING FUNCTION for the API-layer RLS cross-user suite (data-model §5,
// "test 4"). This file carries no database dependency, so the tripwire fires in
// the plain `go test` lane, not only under -tags integration.
//
// The idea is the one cmd/mailroom/gate_test.go uses for the login gate: the
// server's own method/route table is the source of truth, and a second, hand-
// maintained list must track it exactly. Adding a JMAP method (or a data-bearing
// HTTP route) without also declaring — and, in rls_integration_test.go, running —
// its cross-user probe fails the build. A cross-user suite that silently skips a
// new surface is exactly the suite that lets the next isolation bug ship.

import (
	"slices"
	"testing"

	"mailroom/internal/search"
)

// crossUserProbedMethods is every JMAP method the RLS suite proves isolates
// users. It MUST equal the server's live method table (TestEveryMethodHasA
// CrossUserProbe). rls_integration_test.go's crossUserProbes then supplies a
// runnable probe for each — checked against this same set — so the unit tripwire
// and the integration suite cannot drift.
var crossUserProbedMethods = map[string]bool{
	// Stateless: no store, no per-user data. Its probe asserts exactly that.
	"Core/echo": true,

	"Email/get":          true,
	"Email/query":        true,
	"Email/queryChanges": true,
	"Email/changes":      true,
	"Email/set":          true,
	// #176: the sharp case is a foreign id — dismissing must not clear another
	// user's open questions, and must not report having done so.
	"Email/dismissCategory":   true,
	"Email/releaseAttachment": true,
	// #725: A naming B's invitation must not send a reply as anyone.
	"Email/replyToInvite": true,

	"Mailbox/get":     true,
	"Mailbox/query":   true,
	"Mailbox/changes": true,
	"Mailbox/set":     true,

	"Thread/get":     true,
	"Thread/changes": true,
	"Thread/set":     true,

	"Label/get":     true,
	"Label/query":   true,
	"Label/changes": true,
	"Label/set":     true,

	"Rule/get":     true,
	"Rule/query":   true,
	"Rule/changes": true,
	"Rule/set":     true,
	"Rule/preview": true,
	"Rule/induce":  true,
	"Rule/tighten": true,
	"Rule/apply":   true,

	// #202: remembered "load this sender's images" grants. The leak that matters is
	// A learning WHICH senders B trusts — a list of the correspondents someone has
	// decided to relax a privacy default for is itself sensitive, over and above the
	// row ids.
	"RemoteAllow/get":     true,
	"RemoteAllow/changes": true,
	"RemoteAllow/set":     true,
	// ADR-026 §3c: a one-off "show remote content" click. It is not a stored
	// object, but it MUST verify the message is the caller's — otherwise one user
	// could note a request against another's message id and, through the proxy,
	// have the server fetch from mail they cannot see.
	"RemoteContent/request": true,

	// #56: saved signatures and templates. Their bodies are text the user WROTE, so
	// a leak here discloses their own words — closer to reading a draft than to
	// reading a setting.
	"ComposeSnippet/get":     true,
	"ComposeSnippet/changes": true,
	"ComposeSnippet/set":     true,
	"SavedSearch/get":        true,
	"SavedSearch/changes":    true,
	"SavedSearch/set":        true,

	// #219: the category-tab strip. The disclosure here is thinner than a saved
	// query or a signature — a tab row carries no free text, only whether a tab is
	// on and where it sits — but the WRITE side is not thinner at all: switching
	// off another user's Promotions tab would move a whole class of their mail into
	// Primary without touching a single message, and nothing in their client would
	// say why.
	"CategoryTab/get":     true,
	"CategoryTab/changes": true,
	"CategoryTab/set":     true,

	// Triage controls (#34): Triage/get must not describe another user's model,
	// Triage/set must not touch it, and — the sharp one — Triage/correct must emit
	// NO feedback for a message the caller cannot see.
	"Triage/get":     true,
	"Triage/set":     true,
	"Triage/correct": true,
	// #842: Triage/open must record NO open for a message the caller cannot see.
	"Triage/open": true,

	"Account/get":       true,
	"Account/query":     true,
	"Account/changes":   true,
	"Account/set":       true,
	"Account/getHealth": true,
	// #52: A can neither start a re-auth on, nor disconnect, B's account.
	"Account/oauthStart":      true,
	"Account/oauthDisconnect": true,

	"Identity/get":     true,
	"Identity/changes": true,
	"Identity/set":     true,

	"Contact/get": true,
	"Contact/set": true,
	// #816: A naming B's contact (either side of a merge) is notFound.
	"Contact/merge":        true,
	"Contact/separate":     true,
	"SenderStats/get":      true,
	"SenderStats/emailIds": true,
	// #58: follow-up nudges — computed from the caller's mail only; a Done
	// on another user's message is notFound.
	"FollowUp/get":    true,
	"FollowUp/set":    true,
	"Contact/query":   true,
	"Contact/changes": true,

	// #448/#374 increment 1: the login's own keys, public facts only.
	"UserKey/get":         true,
	"RecipientKey/lookup": true,
	"ContactKey/decide":   true,
	"ContactKey/import":   true,
	// #796: correspondent keys (read, remove) and own-key management.
	"ContactKey/get":   true,
	"ContactKey/set":   true,
	"UserKey/generate": true,
	"UserKey/import":   true,
	"UserKey/set":      true,
	"UserKey/extend":   true,
	"UserKey/revoke":   true,
	"UserKey/export":   true,
	// v1.93 (#815): a held key's history.
	"ContactKey/history": true,
	// v1.93 (#817): replace a bound key in one step.
	"UserKey/replace": true,
	// v1.93 (#818): re-fetch a held correspondent key from WKD.
	"ContactKey/refresh": true,

	"EmailSubmission/get": true,
	// #653: the Outbox bootstrap. B's scheduled submission IS in B's Outbox, so
	// an `inOutbox` query that ignored RLS would return it.
	"EmailSubmission/query":        true,
	"EmailSubmission/changes":      true,
	"EmailSubmission/set":          true,
	"EmailSubmission/getSendQuota": true,

	// Web Push device registration (#42, ADR-016): a leak exposes another user's
	// device push endpoints and keys.
	"WebPushSubscription/get": true,
	"WebPushSubscription/set": true,

	// Signed-in devices (#937): another user's sessions, their user agents and
	// addresses, and — the sharp one — signing them out.
	"SignedInDevice/get": true,
	"SignedInDevice/set": true,

	// Agent access (#49): another user's keys, their audit and their approval
	// queue.
	"ApiKey/get":        true,
	"ApiKey/set":        true,
	"ApiKey/getEvents":  true,
	"PendingAction/get": true,
	"PendingAction/set": true,

	// Registered only when a search service is installed (API.SetSearch); the
	// full-method fixture below installs one so this is always in scope.
	"SearchSnippet/get": true,

	// The desk (#442): the user's own arrangement of Areas, the desk-wide
	// Surface/Drawer threshold, and per-client-family display preferences.
	//
	// Area/set is the sharpest of the ten: MOVING A TAB OR LABEL FEED from
	// one area to another must not be able to steal it from another user's
	// area. area_feeds' uniqueness (area_feeds_tab_once/area_feeds_label_once)
	// is scoped BY user_id, so RLS makes another user's conflicting feed
	// invisible rather than merely forbidden — a caller cannot even learn
	// that B's area already holds the tab/label it named. Area/getSummary
	// must count NONE of another user's mail into either the account-scoped
	// or the unscoped totals.
	"Area/get":        true,
	"Area/changes":    true,
	"Area/set":        true,
	"Area/getSummary": true,
	// #665: the whole desk in one call. Its sharp cases are Area/getSummary's
	// plus a section naming B's area, which must come back notFound and never
	// as a page of B's mail.
	"Area/getDesk": true,

	"DeskPreferences/get":     true,
	"DeskPreferences/changes": true,
	// DeskPreferences/set's sharp case: the only valid update id IS the
	// caller's own user id, so naming B's user id must not silently no-op —
	// it must refuse EXACTLY as an id that never existed would (RLS makes
	// the two indistinguishable), and it must not touch B's threshold.
	"DeskPreferences/set": true,

	// Out of office (#50): one object per MAIL account, id = the account id.
	// Naming B's account must look exactly like naming an account that does
	// not exist — for /get, for /check (which would otherwise DIAL B's
	// provider with B's credentials), and for /set (which would install a
	// script on B's server and overwrite B's reply text).
	"OutOfOffice/get":   true,
	"OutOfOffice/set":   true,
	"OutOfOffice/check": true,

	"ClientPreferences/get":     true,
	"ClientPreferences/changes": true,
	// ClientPreferences/set's sharp case: naming B's row id (not B's user id)
	// must not resolve to any family at all, so a rewrite of B's theme/
	// density/pane split can never land under A's call.
	"ClientPreferences/set": true,

	// Day-one setup (#446). Setup/census is the widest READ on this list — it
	// describes a login's whole filing system, folder by folder, with counts and
	// sender concentration — and Setup/undo is the sharpest WRITE: it deletes
	// labels, clears the archive mark and moves mail from a run's journal, so a
	// run id that resolved across users would strip B's filing and enqueue IMAP
	// moves on B's account. Setup/preview and Setup/apply are probed by naming
	// B's MAILBOX in a plan: it must be refused with the same answer a mailbox
	// that never existed gets, so the error cannot be used to learn B's folders
	// exist.
	"Setup/census":  true,
	"Setup/preview": true,
	"Setup/apply":   true,
	"Setup/get":     true,
	"Setup/changes": true,
	"Setup/undo":    true,
}

// exemptCrossUserRoutes are JMAP HTTP routes that expose no per-user object by an
// id a caller supplies, so there is no cross-user probe to write:
//   - /jmap is method dispatch; every method is probed individually.
//   - the Session resource is always the caller's OWN account and takes no id
//     (TestRLSSessionIsOwnAccountOnly asserts it never names another user).
//   - /mcp (#49, ADR-044) turns each tool call into JMAP method calls run
//     through the same dispatch as /jmap, so the methods' own probes cover it.
var exemptCrossUserRoutes = map[string]bool{
	"/jmap":             true,
	"/jmap/session":     true,
	"/.well-known/jmap": true,
	MCPPath:             true,
}

// crossUserProbedRoutes are the data-bearing HTTP endpoints whose cross-user
// isolation the integration suite drives over the REAL mux (download by another
// user's blobId, upload/eventsource under another user's account).
var crossUserProbedRoutes = map[string]bool{
	"/jmap/download/":    true,
	"/jmap/preview/":     true,
	"/jmap/upload/":      true,
	"/jmap/eventsource/": true,
	"/jmap/proxy/":       true,
}

// newFullMethodAPI builds an API with the COMPLETE method table — the base
// methods plus the search extension (SearchSnippet/get and the search-aware
// Email/query) — with no database. It mirrors exactly what the integration
// harness registers, so the two agree on the method set they must cover.
func newFullMethodAPI(t *testing.T) *API {
	t.Helper()
	a := New(nil)
	ix, err := search.NewIndexer(t.TempDir(), search.MemoryFactory, search.NewStoreSource(nil), nil)
	if err != nil {
		t.Fatalf("build search indexer: %v", err)
	}
	a.SetSearch(search.NewService(ix, search.NewStoreResolver(nil)))
	return a
}

// TestEveryMethodHasACrossUserProbe is the tripwire: a method the server
// dispatches but the RLS suite does not cover, or a covered method that no longer
// exists, fails here — in the fast lane, with no database required.
func TestEveryMethodHasACrossUserProbe(t *testing.T) {
	a := newFullMethodAPI(t)
	for name := range a.methods {
		if !crossUserProbedMethods[name] {
			t.Errorf("JMAP method %q has NO cross-user RLS probe.\n"+
				"Every method must be proven to isolate users (data-model §5, test 4): add it to "+
				"crossUserProbedMethods (rls_forcing_test.go) AND write its probe in crossUserProbes "+
				"(rls_integration_test.go).", name)
		}
	}
	for name := range crossUserProbedMethods {
		if _, ok := a.methods[name]; !ok {
			t.Errorf("crossUserProbedMethods lists %q, which is not a registered method — remove it "+
				"or it hides a real gap behind a dead entry.", name)
		}
	}
}

// TestEveryDataRouteHasACrossUserProbe does the same for the HTTP surface: a new
// route in api.JMAPPaths() must be classified as either identity-free (exempt) or
// cross-user probed, so a data-bearing endpoint cannot be added without deciding
// how its isolation is proven.
func TestEveryDataRouteHasACrossUserProbe(t *testing.T) {
	for _, p := range JMAPPaths() {
		if exemptCrossUserRoutes[p] || crossUserProbedRoutes[p] {
			continue
		}
		t.Errorf("JMAP route %q is neither exempt nor cross-user probed.\n"+
			"Declare it: identity-free surfaces go in exemptCrossUserRoutes; a route that serves "+
			"per-user data by a caller-supplied id needs a probe in rls_integration_test.go and an "+
			"entry in crossUserProbedRoutes.", p)
	}
	for p := range crossUserProbedRoutes {
		if !slices.Contains(JMAPPaths(), p) {
			t.Errorf("crossUserProbedRoutes lists %q, which is not a served route.", p)
		}
	}
	for p := range exemptCrossUserRoutes {
		if !slices.Contains(JMAPPaths(), p) {
			t.Errorf("exemptCrossUserRoutes lists %q, which is not a served route.", p)
		}
	}
}
