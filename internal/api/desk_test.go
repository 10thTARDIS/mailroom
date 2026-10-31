// SPDX-License-Identifier: AGPL-3.0-only

package api

import "testing"

// TestInVocabulary pins the desk's closed-vocabulary validator (#442) against
// EXACTLY what deskCapability advertises — the whole point of defining these
// slices once (desk.go) rather than re-deriving them from the store's CHECK
// constraints.
func TestInVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		vocab []string
		in    []string
		out   []string
	}{
		{"clientFamilies", deskClientFamilies, []string{"web", "phone", "desktop"}, []string{"", "tablet", "WEB"}},
		{"themes", deskThemes, []string{"system", "light", "dark"}, []string{"sepia", "Dark", ""}},
		{"densities", deskDensities, []string{"compact", "normal", "relaxed"}, []string{"cozy", ""}},
		{"messageOpens", deskMessageOpens, []string{"beside", "focused"}, []string{"modal", ""}},
		{"textSizes", deskTextSizes, []string{"xs", "s", "m", "l", "xl"}, []string{"huge", "M", "medium", ""}},
		{"rowLayouts", deskRowLayouts, []string{"oneLine", "multiLine"}, []string{"twoLine", "oneline", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, v := range tc.in {
				if !inVocabulary(tc.vocab, v) {
					t.Errorf("inVocabulary(%v, %q) = false, want true", tc.vocab, v)
				}
			}
			for _, v := range tc.out {
				if inVocabulary(tc.vocab, v) {
					t.Errorf("inVocabulary(%v, %q) = true, want false", tc.vocab, v)
				}
			}
		})
	}
}

// TestDeskCapabilityAdvertisesTheVocabularies keeps the session honest, the
// same way TestCategoryTabsAreAdvertised does for tabs: a client that cannot
// see the vocabulary cannot render a settings UI without hard-coding it.
func TestDeskCapabilityAdvertisesTheVocabularies(t *testing.T) {
	caps := capabilityObjects()
	obj, ok := caps[capDesk].(map[string]any)
	if !ok {
		t.Fatalf("%s is not advertised in the session capabilities", capDesk)
	}
	checks := map[string][]string{
		"clientFamilies": deskClientFamilies,
		"themes":         deskThemes,
		"densities":      deskDensities,
		"messageOpens":   deskMessageOpens,
		// #514. Order matters for textSizes and the check below compares
		// element by element, which is what pins "smallest first" — a client
		// renders the five-step control straight off this slice.
		"textSizes":  deskTextSizes,
		"rowLayouts": deskRowLayouts,
	}
	for key, want := range checks {
		got, ok := obj[key].([]string)
		if !ok || len(got) != len(want) {
			t.Errorf("capDesk[%q] = %#v, want %v", key, obj[key], want)
			continue
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("capDesk[%q][%d] = %q, want %q", key, i, got[i], w)
			}
		}
	}
	for _, m := range []string{
		"Area/get", "Area/changes", "Area/set", "Area/getSummary", "Area/getDesk",
		"DeskPreferences/get", "DeskPreferences/changes", "DeskPreferences/set",
		"ClientPreferences/get", "ClientPreferences/changes", "ClientPreferences/set",
	} {
		if methodCapabilities[m] != capDesk {
			t.Errorf("%s is gated on %q, want %q", m, methodCapabilities[m], capDesk)
		}
	}
	// #665: the flag a client reads to choose Area/getDesk over the
	// per-section path. Advertised only alongside the method itself.
	if obj["getDesk"] != true {
		t.Errorf("capDesk[getDesk] = %#v, want true", obj["getDesk"])
	}
}
