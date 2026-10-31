// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/gmailsync"
	"mailroom/internal/mailsync"
	"mailroom/internal/oauthmail"
	"mailroom/internal/store"
	"mailroom/internal/sync"
	"mailroom/internal/syncrunner"
	"mailroom/internal/xoauth2"
)

// runGmailCensus is the Gmail read path's REPORT-FIRST tool (#52, ADR-047
// §6): before a Gmail account's first import, read what that import would
// do — messages, bytes, labels, years, days at the budget, dedup — and change
// nothing. It downloads no message body and selects no label folder
// (internal/gmailsync).
//
// A CLI rather than a JMAP method, deliberately: it is an operator's
// pre-flight before switching a person's mailbox on, not something the person
// does; on a large Gmail it runs for minutes, which no JMAP request should;
// and it must run while the account's sync is still HELD (the Gmail sync
// hold), which is exactly when the JMAP surface refuses to sync it. It reads
// through the same resolver and token Manager the server uses, so it proves
// the grant works too.
func runGmailCensus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gmail-census", flag.ContinueOnError)
	var (
		userIDStr    = fs.String("user-id", "", "user id (ULID)")
		accountIDStr = fs.String("account-id", "", "the Gmail account (ULID)")
		limit        = fs.Int("limit", 0, "read only the newest N messages of each mailbox (0 = all); a sampled report says so and labels its estimates")
		top          = fs.Int("top", 20, "how many user labels to list by count")
		skipDedup    = fs.Bool("skip-dedup", false, "skip the Message-ID dedup preview (and the ENVELOPE it fetches)")
		budgetMB     = fs.Int("budget-mb", 0, "daily budget (MB) for the days-to-import estimate (0 = the configured MAILROOM_GMAIL_DAILY_DOWNLOAD_MB)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	accountID, err := parseULIDFlag("--account-id", *accountIDStr)
	if err != nil {
		return err
	}
	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var acct store.Account
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		acct, e = tx.GetAccount(ctx, accountID)
		return e
	}); err != nil {
		return fmt.Errorf("gmail-census: %w", err)
	}
	if acct.ProviderProfile != "gmail" {
		return fmt.Errorf("gmail-census: account %s has provider profile %q, not gmail", acct.ID, acct.ProviderProfile)
	}

	opts := syncResolverOptions(d)
	// READ-ONLY: the census is run while the Gmail sync hold stands, before
	// the first import, so it must be able to dial a held account. It never
	// ingests (internal/gmailsync), which is what makes lifting the hold for
	// it, and only it, safe.
	opts = append(opts, syncrunner.WithSyncHold(syncrunner.NoSyncHold))
	if acct.AuthKind == "oauth2" {
		tokens, err := adminOAuthTokens(d)
		if err != nil {
			return err
		}
		opts = append(opts, syncrunner.WithAccessTokens(tokens))
	}
	req, err := syncrunner.NewCredentialResolver(d.keys, opts...).Prepare(ctx, userID, acct)
	if err != nil {
		return fmt.Errorf("gmail-census: %w", err)
	}
	crypto.Zero(req.DEK) // the census reads no mail and has no use for the mail key

	budget := d.gmailBudgetBytes()
	if *budgetMB > 0 {
		budget = int64(*budgetMB) * 1000 * 1000
	}
	return gmailCensus(ctx, os.Stdout, sync.Dial, req.Config, censusStore{st: d.store, userID: userID, accountID: acct.ID},
		gmailsync.CensusOptions{Limit: *limit, TopLabels: *top, SkipDedup: *skipDedup, DailyBudgetBytes: budget})
}

// gmailCensus dials and runs the census; the seam the test drives against the
// scripted fake Gmail.
func gmailCensus(ctx context.Context, w io.Writer, dial mailsync.SessionFactory, cfg sync.Config, st gmailsync.CensusStore, opt gmailsync.CensusOptions) error {
	dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	sess, err := dial(dctx, cfg)
	cancel()
	if err != nil {
		return fmt.Errorf("gmail-census: dial: %w", err)
	}
	defer func() {
		_ = sess.Logout(ctx)
		_ = sess.Close()
	}()
	r, err := gmailsync.Census(ctx, sess, st, opt)
	if err != nil {
		return fmt.Errorf("gmail-census: %w", err)
	}
	r.Write(w)
	return nil
}

// censusStore answers the census's two read-only questions through UserTx.
type censusStore struct {
	st        *store.Store
	userID    ulid.ULID
	accountID ulid.ULID
}

func (c censusStore) ExistingLabels(ctx context.Context, names []string) (map[string]bool, error) {
	out := map[string]bool{}
	err := c.st.UserTx(ctx, c.userID, func(tx *store.UserTx) error {
		got, err := tx.LabelIDsByName(ctx, names)
		for n := range got {
			out[n] = true
		}
		return err
	})
	return out, err
}

func (c censusStore) CountMessageIDsElsewhere(ctx context.Context, ids []string) (int, error) {
	var n int
	err := c.st.UserTx(ctx, c.userID, func(tx *store.UserTx) error {
		var err error
		n, err = tx.CountMessageIDsElsewhere(ctx, c.accountID, ids)
		return err
	})
	return n, err
}

// adminOAuthTokens builds the token Manager the server builds (cmd/mailroom
// buildOAuth), for this CLI's dial of an OAuth account. Only the provider
// registry is needed: the connect flow and its callback are the server's.
func adminOAuthTokens(d *deps) (syncrunner.AccessTokens, error) {
	if d.cfg == nil {
		return nil, fmt.Errorf("gmail-census: no configuration loaded")
	}
	ms, err := oauthmail.Microsoft(oauthmail.Client{ID: d.cfg.OAuthMicrosoftClientID,
		Secret: xoauth2.Secret(d.cfg.OAuthMicrosoftClientSecret)}, d.cfg.OAuthMicrosoftTenant)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvOAuthMicrosoftTenant, err)
	}
	reg, err := oauthmail.NewRegistry(
		oauthmail.Google(oauthmail.Client{ID: d.cfg.OAuthGoogleClientID, Secret: xoauth2.Secret(d.cfg.OAuthGoogleClientSecret)}),
		ms,
	)
	if err != nil {
		return nil, fmt.Errorf("oauth registry: %w", err)
	}
	if _, ok := reg.Lookup(oauthmail.ProviderGoogle); !ok {
		return nil, fmt.Errorf("gmail-census: the account signs in with Google, and Google sign-in is not configured here (%s)",
			config.EnvOAuthGoogleClientID)
	}
	return oauthmail.NewManager(reg, oauthmail.StoreGrants{Store: d.store, Keys: d.keys}, oauthmail.WithLogger(d.log)), nil
}
