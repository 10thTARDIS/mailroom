// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/authserv"
	"mailroom/internal/store"
)

// authSelfFacts decides, for the `authResults` notice, whether a message is the
// user's own rather than inbound (#766). A message with no
// Authentication-Results header at all is shown as "unauthenticated" only when
// it is NOT self: mail the user wrote or filed themselves never crossed an
// authenticating hop (other clients append ~600 Sent copies a month on the
// corpus this was measured on).
//
// Self is locally authored (checked by the caller from the row) or live in a
// Sent or Drafts mailbox (EmailView.MailboxIDs, already loaded).
//
// ⚠ NOT "From is one of the user's addresses". The sender writes From, and a
// spoofer would put the user's address there to switch the header-less notice
// off. Only facts the sender cannot set count.
//
// The mailbox roles are resolved by ONE store read per Email/get
// (store.AuthSelfFacts), and only when the request asks for authResults on a
// deployment that names an authserv-id, so a list-view fetch pays nothing and a
// reading-pane fetch pays one small query, not one per message.
type authSelfFacts struct{ f store.AuthSelfFacts }

func (a *API) resolveAuthSelf(ctx context.Context, userID ulid.ULID) (*authSelfFacts, *MethodError) {
	var f store.AuthSelfFacts
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		f, err = tx.AuthSelfFacts(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	return &authSelfFacts{f: f}, nil
}

// isSelf is nil-safe: with no facts loaded nothing is self, which only matters
// to a header-less message, and for one the notice then reads as before.
func (s *authSelfFacts) isSelf(ev store.EmailView) bool {
	if s == nil {
		return false
	}
	return s.f.IsSelf(ev.MailboxIDs)
}

// hasBoundedAuthserv reports whether any configured entry is date-bounded
// (`id<YYYY-MM-DD`), the only case where a message's received time matters.
func hasBoundedAuthserv(entries []string) bool {
	for _, e := range entries {
		if strings.Contains(e, authserv.BoundSep) {
			return true
		}
	}
	return false
}
