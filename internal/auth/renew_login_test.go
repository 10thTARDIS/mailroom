// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// At sign-in too: UserInfo for a different subject than the ID token fails the
// sign-in (OIDC Core 5.3.2) instead of being merged into the claims. A UserInfo
// that merely fails stays best-effort.
func TestCallback_UserInfoSubjectMismatchFailsTheLogin(t *testing.T) {
	for name, tc := range map[string]struct {
		userInfo func(context.Context, *oauth2.Token) (userInfoResult, error)
		wantOK   bool
	}{
		"mismatch": {func(context.Context, *oauth2.Token) (userInfoResult, error) {
			return userInfoResult{Subject: "someone-else", Claims: idTokenClaims{Groups: []string{"mailroom-admin"}}}, nil
		}, false},
		"failure is best effort": {func(context.Context, *oauth2.Token) (userInfoResult, error) {
			return userInfoResult{}, errors.New("userinfo is down")
		}, true},
		"match": {func(context.Context, *oauth2.Token) (userInfoResult, error) {
			return userInfoResult{Subject: "user-sub-1", Claims: idTokenClaims{Groups: []string{"g"}}}, nil
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCallbackHarness(t)
			h.a.userInfoFn = tc.userInfo
			fs := flowState{State: "state-123", Nonce: "nonce-123", Verifier: "verifier-123", Expiry: time.Now().Add(time.Minute)}
			currentTokenSpec = tokenResponseSpec{
				wantCode: "auth-code-123", wantVerifier: fs.Verifier,
				idToken: h.signIDToken(h.baseClaims("user-sub-1", fs.Nonce)),
			}
			rr := h.run(fs, nil)
			c := issuedSession(rr, h.a)
			if tc.wantOK != (c != nil) {
				t.Fatalf("status %d, session cookie issued=%v; want issued=%v", rr.Code, c != nil, tc.wantOK)
			}
			if !tc.wantOK && len(h.resolveArgs) != 0 {
				t.Errorf("ResolveUser ran %d times after a UserInfo subject mismatch", len(h.resolveArgs))
			}
		})
	}
}
