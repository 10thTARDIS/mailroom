// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"mailroom/internal/store"
)

// The web client declares its capabilities ONCE, as a static list, and the
// server refuses any method whose capability the caller did not declare
// (mayCall -> unknownMethod). Nothing in either language checks that the list
// actually covers the methods the client calls, so adding a gated method to the
// client and forgetting the URN produces a feature that builds, type-checks,
// passes every Go test, and then fails at the user's fingertips.
//
// That is not hypothetical: it shipped. v1.4.0 wired Triage/correct into the
// client without adding urn:mailroom:triage to MAILROOM_USING, so marking a
// message important returned `unknownMethod` in production (#159). The API
// integration tests could not have caught it — they invoke handlers directly
// (call(t, h.api.emailQuery, ...)), which bypasses the dispatcher where the
// `using` gate lives.
//
// So the contract is checked here, across the language boundary: every JMAP
// method the client can put on the wire must have its capability in the set the
// client declares. Only client.ts is scanned because it is the single transport
// — every method literal elsewhere in web/src is prose in a comment.
const (
	webTypesTS  = "../../web/src/core/jmap/types.ts"
	webClientTS = "../../web/src/core/jmap/client.ts"
)

var (
	// export const CAP_TRIAGE = "urn:mailroom:triage";
	reCapConst = regexp.MustCompile(`export const (CAP_[A-Z_]+)\s*=\s*"([^"]+)"`)
	// export const MAILROOM_USING: readonly string[] = [ CAP_CORE, ... ];
	reUsingList = regexp.MustCompile(`(?s)export const MAILROOM_USING[^=]*=\s*\[(.*?)\]`)
	reCapRef    = regexp.MustCompile(`CAP_[A-Z_]+`)
	// this.call("Triage/correct", ...) — the method names on the wire.
	reMethod = regexp.MustCompile(`"([A-Z][A-Za-z]*/[a-zA-Z]+)"`)
)

func readWebSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (this test asserts a contract with the web client; "+
			"if the client moved, update the path)", path, err)
	}
	return string(b)
}

// declaredUsing resolves MAILROOM_USING to the URNs it names.
func declaredUsing(t *testing.T) map[string]bool {
	t.Helper()
	types := readWebSource(t, webTypesTS)

	urnFor := map[string]string{}
	for _, m := range reCapConst.FindAllStringSubmatch(types, -1) {
		urnFor[m[1]] = m[2]
	}
	if len(urnFor) == 0 {
		t.Fatalf("no CAP_* constants found in %s — the parse is stale", webTypesTS)
	}

	list := reUsingList.FindStringSubmatch(types)
	if list == nil {
		t.Fatalf("MAILROOM_USING not found in %s — the parse is stale", webTypesTS)
	}

	using := map[string]bool{}
	for _, ref := range reCapRef.FindAllString(list[1], -1) {
		urn, ok := urnFor[ref]
		if !ok {
			t.Fatalf("MAILROOM_USING names %s, which is not declared in %s", ref, webTypesTS)
		}
		using[urn] = true
	}
	return using
}

// Every method the web client calls must be reachable with the capabilities the
// web client declares. A failure here means a user-facing method returns
// unknownMethod.
func TestWebClientDeclaresCapabilitiesForEveryMethodItCalls(t *testing.T) {
	using := declaredUsing(t)
	// WebPushSubscription/* rides a wider `using` (PUSH_USING = MAILROOM_USING +
	// CAP_PUSH) because push is opt-in per ADR-016, so it is exempt from the
	// base set — but still checked, against that wider set.
	push := map[string]bool{capPush: true}
	for urn := range using {
		push[urn] = true
	}
	// UserKey/*, RecipientKey/* and ContactKey/* (#47) ride a wider set the same way (MAILROOM_USING + CAP_PGP), and
	// only when the session advertises urn:mailroom:pgp: the key helpers in
	// client.ts check hasCapability first, because naming an unadvertised URN
	// in the STANDING set is a request-level 400 that would break every screen
	// against a server that predates the keys API (#374 increment 2). So it is
	// exempt from the base set, but still checked against that wider set.
	pgp := map[string]bool{capPGP: true}
	for urn := range using {
		pgp[urn] = true
	}
	// OutOfOffice/* rides a wider set the same way (MAILROOM_USING +
	// CAP_OUT_OF_OFFICE), for the keys' reason: the helpers check
	// hasCapability first, so a server that predates #50 is never sent the URN.
	oof := map[string]bool{capOutOfOffice: true}
	for urn := range using {
		oof[urn] = true
	}
	// ApiKey/* and PendingAction/* (#49) ride a wider set the same way
	// (MAILROOM_USING + CAP_APIKEYS), for the keys' reason: the helpers check
	// hasCapability first, so a server that predates v1.95 is never sent the URN.
	apikeys := map[string]bool{capAPIKeys: true}
	for urn := range using {
		apikeys[urn] = true
	}
	// SignedInDevice/* (#937) rides a wider set the same way (MAILROOM_USING +
	// CAP_DEVICES): the helpers check hasCapability first, so a server that
	// predates v1.98 is never sent the URN.
	devices := map[string]bool{capDevices: true}
	for urn := range using {
		devices[urn] = true
	}

	client := readWebSource(t, webClientTS)
	seen := map[string]bool{}
	for _, m := range reMethod.FindAllStringSubmatch(client, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true

		declared := using
		if strings.HasPrefix(name, "WebPushSubscription/") {
			declared = push
		}
		if strings.HasPrefix(name, "UserKey/") || strings.HasPrefix(name, "RecipientKey/") ||
			strings.HasPrefix(name, "ContactKey/") {
			declared = pgp
		}
		if strings.HasPrefix(name, "OutOfOffice/") {
			declared = oof
		}
		if strings.HasPrefix(name, "ApiKey/") || strings.HasPrefix(name, "PendingAction/") {
			declared = apikeys
		}
		if strings.HasPrefix(name, "SignedInDevice/") {
			declared = devices
		}

		urn, known := methodCapabilities[name]
		if !known {
			t.Errorf("client calls %s, which this server does not implement: "+
				"it will always return unknownMethod", name)
			continue
		}
		if !declared[urn] {
			t.Errorf("client calls %s, which requires %q, but the client does not "+
				"declare it in MAILROOM_USING: this returns unknownMethod at runtime",
				name, urn)
		}
	}
	if len(seen) == 0 {
		t.Fatalf("no JMAP method literals found in %s — the parse is stale", webClientTS)
	}
}

// The declared capabilities must all be real. A typo'd or retired URN in
// MAILROOM_USING is rejected by the `using` validator, which fails the WHOLE
// request batch — so this breaks every method, not one.
func TestWebClientDeclaresOnlyAdvertisedCapabilities(t *testing.T) {
	advertised := capabilityObjects()
	for urn := range declaredUsing(t) {
		if _, ok := advertised[urn]; !ok {
			t.Errorf("client declares %q in MAILROOM_USING, which this server does "+
				"not advertise: every request would be rejected", urn)
		}
	}
}

// reKeywordConst matches the web client's system-keyword constants:
// export const KEYWORD_JUNK = "$junk";
var reKeywordConst = regexp.MustCompile(`export const (KEYWORD_[A-Z]+)\s*=\s*"([^"]+)"`)

// The system keywords the web client writes must be spelled exactly as the
// server's parseEmailPatch switches on them (#528).
//
// A drifted atom does not fail loudly. setKeyword's `default:` arm treats any
// name it does not recognise as a label atom or a RAW keyword, so a client that
// sent `keywords/$Junk: null` would get success, a change_log row and a
// writeback intent — and the message would stay in Junk. That is #351's exact
// shape, where "Not junk" reported success and changed nothing because $junk had
// no case at all. The API tests call handlers with the server's own constants,
// so only a check across the language boundary sees this.
func TestWebClientSystemKeywordsMatchTheServer(t *testing.T) {
	want := map[string]string{
		"KEYWORD_SEEN":     store.KeywordSeen,
		"KEYWORD_FLAGGED":  store.KeywordFlagged,
		"KEYWORD_ANSWERED": store.KeywordAnswered,
		"KEYWORD_DRAFT":    store.KeywordDraft,
		"KEYWORD_JUNK":     store.KeywordJunk,
	}
	got := map[string]string{}
	for _, m := range reKeywordConst.FindAllStringSubmatch(readWebSource(t, webTypesTS), -1) {
		got[m[1]] = m[2]
	}
	for name, server := range want {
		client, ok := got[name]
		if !ok {
			t.Errorf("%s not found in %s — the parse is stale, or the constant moved", name, webTypesTS)
			continue
		}
		if client != server {
			t.Errorf("client %s = %q but the server switches on %q: Email/set would treat the "+
				"client's atom as a raw keyword, report success, and change nothing", name, client, server)
		}
	}
}
