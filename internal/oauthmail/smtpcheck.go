// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"context"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/provider"
	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

// SMTPCheck authenticates to the provider's SMTP server with a just-issued
// access token and sends nothing (ADR-046 §12): the connect-time probe that
// tells a Microsoft account whose organisation has turned SMTP AUTH off
// (5.7.139) apart from one that can send. The default (nil) runs no check.
type SMTPCheck func(ctx context.Context, p Provider, address string, token xoauth2.Secret) error

// SMTPAuthCheck is the production SMTPCheck: provider.SMTPSender.CheckAuth
// against the provider's fixed SMTP host, through the same egress guard as
// every send.
func SMTPAuthCheck(allowPrivate bool) SMTPCheck {
	return func(ctx context.Context, p Provider, address string, token xoauth2.Secret) error {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return provider.SMTPSender{AllowPrivateHosts: allowPrivate}.CheckAuth(ctx, provider.SendRequest{
			Host: p.SMTPHost, Port: p.SMTPPort, ImplicitTLS: p.SMTPImplicitTLS,
			Auth: provider.XOAuth2Auth(address, xoauth2.StaticToken(token)),
		})
	}
}

// checkSending runs the SMTP probe for a just-connected or reconnected
// Microsoft account and records the verdict: 5.7.139 marks it RECEIVE-ONLY
// (the connect still succeeds — the design's "keep it receive-only with a
// warning instead of failing the connect"), a successful AUTH clears any
// earlier mark (this is how a person rechecks after their administrator
// turned SMTP back on: reconnect), and any other failure changes nothing —
// a network blip must neither block nor unblock sending. Best-effort: it
// never fails the callback.
func (c *Connector) checkSending(ctx context.Context, userID ulid.ULID, p Provider, accountID ulid.ULID, address string, token xoauth2.Secret) {
	if c.smtpCheck == nil || p.Profile != provider.ProfileMicrosoft || accountID == (ulid.ULID{}) {
		return
	}
	err := c.smtpCheck(ctx, p, address, token)
	log := c.log.With("account_id", accountID.String(), "provider", p.Name)
	switch {
	case err == nil:
		var cleared bool
		if serr := c.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			cleared, e = tx.ClearSendBlocked(ctx, accountID)
			return e
		}); serr != nil {
			log.Error("oauth connect: could not record that the account can send", "err", serr)
		} else if cleared {
			log.Info("oauth connect: SMTP accepted the account again; it can send")
		}
	case errors.Is(err, provider.ErrSMTPAuthDisabled):
		if serr := c.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			return tx.MarkSendBlocked(ctx, accountID, store.SendBlockedSMTPAuthDisabled, c.now())
		}); serr != nil {
			log.Error("oauth connect: could not mark the account receive-only", "err", serr)
		} else {
			log.Warn("oauth connect: the organisation has SMTP AUTH turned off (5.7.139); the account is receive-only")
		}
	default:
		log.Warn("oauth connect: SMTP check failed; sending state unchanged",
			"err", xoauth2.Scrub(err.Error(), token))
	}
}
