// SPDX-License-Identifier: AGPL-3.0-only

package oauthserver

import (
	"context"
	"net/http"
	"strconv"

	"mailroom/internal/store"
)

// Revoke is POST /oauth/revoke (RFC 7009): a refresh token revokes its whole
// family, an access token only itself. It answers 200 whether or not the token
// was known, so it is no oracle; an address that keeps presenting unknown
// tokens is slowed by the shared failure limiter like any other guesser.
func (s *Server) Revoke() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := formBody(w, r); e != nil {
			s.failed(w, r, e)
			return
		}
		token := r.PostForm.Get("token")
		if token == "" {
			s.failed(w, r, badRequest("token is required"))
			return
		}
		rev, found, err := s.st.RevokeOAuthToken(r.Context(), hashOf(token))
		if err != nil {
			s.log.ErrorContext(r.Context(), "oauth: revoke", "err", err)
			writeOAuthError(w, serverFault())
			return
		}
		if !found {
			addr := clientIP(r)
			if blocked, retry := s.limiter.Blocked(addr); blocked {
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "slow_down"})
				return
			}
			s.limiter.Fail(addr)
		} else if rev.Revoked > 0 {
			s.auditRevocation(r.Context(), rev)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	})
}

func (s *Server) auditRevocation(ctx context.Context, rev store.OAuthRevocation) {
	ctx = context.WithoutCancel(ctx)
	action := "oauth.token_revoke"
	if rev.Kind == store.OAuthTokenRefresh {
		action = "oauth.family_revoke"
	}
	if err := s.st.UserTx(ctx, rev.UserID, func(tx *store.UserTx) error {
		return tx.AuditAPIKeySystem(ctx, action, rev.APIKeyID,
			map[string]any{"familyId": rev.FamilyID.String(), "tokensRevoked": rev.Revoked, "via": "revocation endpoint"})
	}); err != nil {
		s.log.ErrorContext(ctx, "oauth: could not audit a revocation", "err", err)
	}
}
