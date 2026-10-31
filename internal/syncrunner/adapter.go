// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/notify"
	"mailroom/internal/store"
	"mailroom/internal/webpush"
)

// StoreLister satisfies AccountLister over a *store.Store, running the list
// inside a Store.UserTx so RLS scopes it to the owning user (CLAUDE.md).
type StoreLister struct {
	store *store.Store
}

// NewStoreLister wraps a *store.Store as an AccountLister.
func NewStoreLister(s *store.Store) *StoreLister { return &StoreLister{store: s} }

var _ AccountLister = (*StoreLister)(nil)

var _ ReconnectLister = (*StoreLister)(nil)
var _ AccountReader = (*StoreLister)(nil)

// GetAccount runs UserTx.GetAccount.
func (l *StoreLister) GetAccount(ctx context.Context, userID, accountID ulid.ULID) (store.Account, error) {
	var out store.Account
	err := l.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		out, e = tx.GetAccount(ctx, accountID)
		return e
	})
	return out, err
}

// ListAwaitingReconnect runs UserTx.ListAwaitingReconnect.
func (l *StoreLister) ListAwaitingReconnect(ctx context.Context, userID ulid.ULID) ([]store.Account, error) {
	var out []store.Account
	err := l.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		out, e = tx.ListAwaitingReconnect(ctx)
		return e
	})
	return out, err
}

// ListSyncableAccounts runs UserTx.ListSyncableAccounts.
func (l *StoreLister) ListSyncableAccounts(ctx context.Context, userID ulid.ULID) ([]store.Account, error) {
	var out []store.Account
	err := l.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		out, e = tx.ListSyncableAccounts(ctx)
		return e
	})
	return out, err
}

// ListDisabledAccounts runs UserTx.ListDisabledAccounts.
func (l *StoreLister) ListDisabledAccounts(ctx context.Context, userID ulid.ULID) ([]store.Account, error) {
	var out []store.Account
	err := l.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		out, e = tx.ListDisabledAccounts(ctx)
		return e
	})
	return out, err
}

// StoreUserLister satisfies UserLister over a *store.Store, listing every
// provisioned, enabled user through store.ListActiveUserIDs (the narrow SECURITY
// DEFINER app.active_user_ids — the only way to enumerate users under RLS).
type StoreUserLister struct {
	store *store.Store
}

// NewStoreUserLister wraps a *store.Store as a UserLister.
func NewStoreUserLister(s *store.Store) *StoreUserLister { return &StoreUserLister{store: s} }

var _ UserLister = (*StoreUserLister)(nil)

// ListActiveUserIDs runs Store.ListActiveUserIDs.
func (l *StoreUserLister) ListActiveUserIDs(ctx context.Context) ([]ulid.ULID, error) {
	return l.store.ListActiveUserIDs(ctx)
}

// StoreHealthRecorder satisfies HealthRecorder over a *store.Store, writing each
// account's sync-health telemetry inside a Store.UserTx so RLS scopes the upsert
// to the owning user (CLAUDE.md, spec §12 / mailroom#43).
type StoreHealthRecorder struct {
	store *store.Store
}

// NewStoreHealthRecorder wraps a *store.Store as a HealthRecorder.
func NewStoreHealthRecorder(s *store.Store) *StoreHealthRecorder {
	return &StoreHealthRecorder{store: s}
}

var _ HealthRecorder = (*StoreHealthRecorder)(nil)

// RecordSyncSuccess runs UserTx.RecordSyncSuccess under userID's RLS scope.
func (h *StoreHealthRecorder) RecordSyncSuccess(ctx context.Context, userID, accountID ulid.ULID, at time.Time) error {
	return h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.RecordSyncSuccess(ctx, accountID, at)
	})
}

// RecordSyncFailure runs UserTx.RecordSyncFailure under userID's RLS scope.
func (h *StoreHealthRecorder) RecordSyncFailure(ctx context.Context, userID, accountID ulid.ULID, errMsg string, at time.Time) error {
	return h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.RecordSyncFailure(ctx, accountID, errMsg, at)
	})
}

// PushSender is the seam StorePushNotifier uses to deliver one encrypted push.
// *webpush.Sender satisfies it. It is an interface so the notifier unit-tests
// with a fake and net/http never enters this package.
type PushSender interface {
	Send(ctx context.Context, sub webpush.Subscription, payload []byte) (webpush.Result, error)
}

// StorePushNotifier satisfies PushNotifier over a *store.Store and a PushSender.
// It opens a Store.UserTx for the user whose sync pass fired it — so RLS scopes
// the push_subscriptions read to exactly that user (no bypass, ADR-016) — lists
// the active subscriptions, sends each an encrypted "new mail" push, and retires
// any endpoint the push service reports GONE (404/410) in the SAME transaction.
//
// TRIAGE-AWARE (#59). Each device has a mode (every message, above the line,
// VIPs only, either), the user may have quiet hours, and an account may have
// push switched off; internal/notify decides what each device is told and
// store.PushCounts measures the batch. A device whose count comes out zero is
// sent nothing.
type StorePushNotifier struct {
	store  *store.Store
	sender PushSender
	log    *slog.Logger
	// now is the clock quiet hours are judged by — time.Now in production, a
	// fixed instant in tests.
	now func() time.Time
}

// NewStorePushNotifier wraps a store + sender as a PushNotifier.
func NewStorePushNotifier(s *store.Store, sender PushSender, log *slog.Logger) *StorePushNotifier {
	if log == nil {
		log = slog.Default()
	}
	return &StorePushNotifier{store: s, sender: sender, log: log, now: time.Now}
}

// WithClock replaces the notifier's clock (tests: quiet hours on a fake clock).
func (p *StorePushNotifier) WithClock(now func() time.Time) *StorePushNotifier {
	p.now = now
	return p
}

var _ PushNotifier = (*StorePushNotifier)(nil)

// NotifyNewMail delivers the new-mail push to userID's registered devices. A send
// failure to one device is logged and skipped (the subscription is kept for a
// later retry); a GONE result retires that subscription. The only errors it
// returns are store-level failures (listing/marking) — the runner treats even
// those as fail-soft telemetry (notifyPush).
//
// newMessages is the plain count (every-message mode sends exactly what it
// always did); ids are the same messages, counted by category only when some
// device's mode or quiet hours need it (notify.NeedsBreakdown).
func (p *StorePushNotifier) NotifyNewMail(ctx context.Context, userID ulid.ULID, acct store.Account, newMessages int, ids []ulid.ULID) error {
	// Push switched off for this account (#59): nothing to any device. The
	// account is the one the runner loaded for this pass.
	if !acct.NotifyPush {
		return nil
	}
	return p.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		subs, err := tx.ListActivePushSubscriptions(ctx)
		if err != nil {
			return err
		}
		if len(subs) == 0 {
			return nil
		}
		prefs, err := tx.GetDeskPreferences(ctx)
		if err != nil {
			return err
		}
		quiet := quietHoursOf(prefs.QuietHours)
		quietNow := quiet.Active(p.now())
		modes := make([]notify.Mode, len(subs))
		for i, s := range subs {
			modes[i] = notify.Mode(s.NotifyMode)
		}
		counts := notify.Counts{All: newMessages}
		if notify.NeedsBreakdown(modes, quietNow) {
			pc, err := tx.PushCounts(ctx, ids, prefs.LineThreshold)
			if err != nil {
				return err
			}
			counts.AboveLine, counts.VIP, counts.AboveLineOrVIP = pc.AboveLine, pc.VIP, pc.AboveLineOrVIP
		}
		payloads := map[int][]byte{}
		for i, sub := range subs {
			n := notify.Decide(modes[i], counts, quietNow, quiet.AllowVIP)
			if n == 0 {
				continue
			}
			payload, ok := payloads[n]
			if !ok {
				if payload, err = newMailPayload(acct.Name, n); err != nil {
					return err
				}
				payloads[n] = payload
			}
			res, serr := p.sender.Send(ctx, webpush.Subscription{
				Endpoint: sub.Endpoint,
				P256dh:   sub.KeysP256dh,
				Auth:     sub.KeysAuth,
			}, payload)
			switch {
			case serr != nil:
				p.log.Warn("web push send failed; keeping subscription for retry",
					"account_id", acct.ID.String(), "err", serr)
			case res == webpush.Gone:
				if merr := tx.MarkPushSubscriptionExpired(ctx, sub.ID); merr != nil {
					return merr
				}
			}
		}
		return nil
	})
}

// quietHoursOf converts the stored setting into internal/notify's terms.
func quietHoursOf(q store.QuietHours) notify.QuietHours {
	return notify.QuietHours{
		Enabled:  q.Enabled,
		Start:    q.Start,
		End:      q.End,
		Days:     uint8(q.Days & int(notify.AllDays)),
		Zone:     q.Timezone,
		AllowVIP: q.AllowVIP,
	}
}

// pushPayload is the minimal notification the service worker renders.
type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

// newMailPayload builds the small JSON body for a new-mail notification.
func newMailPayload(accountName string, newMessages int) ([]byte, error) {
	body := fmt.Sprintf("%d new message", newMessages)
	if newMessages != 1 {
		body += "s"
	}
	if accountName != "" {
		body += " in " + accountName
	}
	b, err := json.Marshal(pushPayload{Title: "New mail", Body: body, URL: "/"})
	if err != nil {
		return nil, fmt.Errorf("syncrunner: marshal push payload: %w", err)
	}
	return b, nil
}

// PushReconnectNeeded tells every active device of the user that an account
// must be signed in again (#52): the one push a grant's transition into
// needs_reauth owes (internal/oauthmail/notify.go). Like an agent alert it
// goes to every device whatever the device's mail mode, and whatever the
// account's new-mail switch — it is not about new mail, and the mail it IS
// about is what stops arriving until the user acts. It respects quiet hours by
// sending nothing and reporting quiet, so the caller tries again later rather
// than dropping the only notice this transition gets. It satisfies
// oauthmail.ReconnectPusher.
func (p *StorePushNotifier) PushReconnectNeeded(ctx context.Context, userID, accountID ulid.ULID, accountName string) (quiet bool, err error) {
	body := "Sign in to " + accountName + " again so Mailroom can keep syncing it. Nothing is lost while you wait."
	if accountName == "" {
		body = "Sign in to one of your accounts again so Mailroom can keep syncing it. Nothing is lost while you wait."
	}
	payload, err := json.Marshal(pushPayload{Title: "Reconnect your account", Body: body,
		URL: "/#/settings/accounts/" + accountID.String()})
	if err != nil {
		return false, fmt.Errorf("syncrunner: marshal reconnect payload: %w", err)
	}
	err = p.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		subs, err := tx.ListActivePushSubscriptions(ctx)
		if err != nil || len(subs) == 0 {
			return err
		}
		prefs, err := tx.GetDeskPreferences(ctx)
		if err != nil {
			return err
		}
		if quietHoursOf(prefs.QuietHours).Active(p.now()) {
			quiet = true
			return nil
		}
		for _, sub := range subs {
			res, serr := p.sender.Send(ctx, webpush.Subscription{
				Endpoint: sub.Endpoint, P256dh: sub.KeysP256dh, Auth: sub.KeysAuth}, payload)
			switch {
			case serr != nil:
				p.log.Warn("reconnect push failed; keeping subscription", "account_id", accountID.String(), "err", serr)
			case res == webpush.Gone:
				if merr := tx.MarkPushSubscriptionExpired(ctx, sub.ID); merr != nil {
					return merr
				}
			}
		}
		return nil
	})
	return quiet, err
}

// NotifyAgentAlert pushes an API-key alert (#49, ADR-042 §6) to every active
// device of the user — whatever the device's mail mode, because these are not
// about mail. An urgent alert (an anomaly rule fired) is a security alert and
// is sent during quiet hours too; any other (an approval request) respects
// them. It satisfies api.AlertNotifier.
func (p *StorePushNotifier) NotifyAgentAlert(ctx context.Context, userID ulid.ULID, title, body, url string, urgent bool) error {
	payload, err := json.Marshal(pushPayload{Title: title, Body: body, URL: url})
	if err != nil {
		return fmt.Errorf("syncrunner: marshal alert payload: %w", err)
	}
	return p.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		subs, err := tx.ListActivePushSubscriptions(ctx)
		if err != nil || len(subs) == 0 {
			return err
		}
		if !urgent {
			prefs, err := tx.GetDeskPreferences(ctx)
			if err != nil {
				return err
			}
			if quietHoursOf(prefs.QuietHours).Active(p.now()) {
				return nil
			}
		}
		for _, sub := range subs {
			res, serr := p.sender.Send(ctx, webpush.Subscription{
				Endpoint: sub.Endpoint, P256dh: sub.KeysP256dh, Auth: sub.KeysAuth}, payload)
			switch {
			case serr != nil:
				p.log.Warn("agent alert push failed; keeping subscription", "err", serr)
			case res == webpush.Gone:
				if merr := tx.MarkPushSubscriptionExpired(ctx, sub.ID); merr != nil {
					return merr
				}
			}
		}
		return nil
	})
}
