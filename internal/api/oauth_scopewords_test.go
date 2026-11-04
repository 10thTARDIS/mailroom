// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"testing"

	"mailroom/internal/oauthserver"
)

// The consent screen describes each scope with oauthserver.ScopeWords. That
// table must cover ADR-042's closed scope set exactly (allScopes, the table the
// gate enforces), or a scope would reach the screen without words, or the
// screen would describe one the gate no longer has.
func TestConsentScopeWordsMatchTheClosedSet(t *testing.T) {
	var words []string
	for sc, w := range oauthserver.ScopeWords {
		if w == "" {
			t.Errorf("scope %s has empty words", sc)
		}
		words = append(words, sc)
	}
	sort.Strings(words)
	closed := slices.Clone(allScopes)
	sort.Strings(closed)
	if !slices.Equal(words, closed) {
		t.Errorf("consent scope words %v differ from the closed scope set %v", words, closed)
	}
	// The policy the consent screen is built from is that same set.
	pol := OAuthPolicy()
	if !slices.Equal(pol.Scopes, allScopes) {
		t.Errorf("OAuthPolicy scopes %v differ from the closed set %v", pol.Scopes, allScopes)
	}
}

// The words are the web key dialog's: web/src/app/i18n.ts "apiKeys.scope.*".
// Both are read here so editing one without the other fails.
func TestConsentScopeWordsMatchTheKeyDialog(t *testing.T) {
	src, err := os.ReadFile("../../web/src/app/i18n.ts")
	if err != nil {
		t.Fatalf("cannot read the web strings to compare against: %v", err)
	}
	re := regexp.MustCompile(`"apiKeys\.scope\.([a-z]+:[a-z]+)":\s*"([^"]*)"`)
	web := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		web[m[1]] = m[2]
	}
	if len(web) == 0 {
		t.Fatal("found no apiKeys.scope.* strings in i18n.ts; the pattern or the file moved")
	}
	for sc, w := range oauthserver.ScopeWords {
		if web[sc] != w {
			t.Errorf("scope %s: consent says %q, the key dialog says %q", sc, w, web[sc])
		}
	}
	for sc := range web {
		if _, ok := oauthserver.ScopeWords[sc]; !ok {
			t.Errorf("the key dialog words scope %s but the consent screen does not", sc)
		}
	}
}
