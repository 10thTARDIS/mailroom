// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

// EffectiveWriteback is the one evaluation Account/get and Account/getHealth
// share. The table covers every switch alone and together, and the precedence
// when several apply (account, then instance, then provider: the order of who can
// change it).
func TestEffectiveWriteback(t *testing.T) {
	plain := Account{WritebackEnabled: true}
	gmail := Account{WritebackEnabled: true, AuthKind: "oauth2", ProviderProfile: "gmail"}
	for _, tc := range []struct {
		name     string
		acct     Account
		instance bool
		want     bool
		heldBy   string
	}{
		{"nothing holds it", plain, false, true, ""},
		{"the account's own switch", Account{WritebackEnabled: false}, false, false, WritebackHeldByAccount},
		{"the instance flag alone", plain, true, false, WritebackHeldByInstance},
		{"the provider hold alone", gmail, false, false, WritebackHeldByProvider},
		{"account and instance: the account is named", Account{WritebackEnabled: false}, true, false, WritebackHeldByAccount},
		{"instance and provider: the instance is named", gmail, true, false, WritebackHeldByInstance},
		{"a Gmail account on an app password is not held", Account{WritebackEnabled: true, AuthKind: "password", ProviderProfile: "gmail"}, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, heldBy := EffectiveWriteback(tc.acct, tc.instance)
			if got != tc.want || heldBy != tc.heldBy {
				t.Errorf("EffectiveWriteback = (%v, %q), want (%v, %q)", got, heldBy, tc.want, tc.heldBy)
			}
			// effective is true exactly when there is no reason.
			if got != (heldBy == "") {
				t.Errorf("effective=%v with reason %q: they must agree", got, heldBy)
			}
		})
	}
}
