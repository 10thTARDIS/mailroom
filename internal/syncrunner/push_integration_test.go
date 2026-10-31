// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package syncrunner

// TRIAGE-AWARE PUSH, end to end through the store (#59): per-device modes,
// quiet hours on a fake clock, the VIP exception, and the per-account switch.
// The sender is a fake that records what each endpoint was told; everything
// between the batch and the send is the production StorePushNotifier.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/webpush"
)

type recordingSender struct {
	mu   sync.Mutex
	sent map[string]string // endpoint -> body text
}

func (r *recordingSender) Send(_ context.Context, sub webpush.Subscription, payload []byte) (webpush.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var p pushPayload
	_ = json.Unmarshal(payload, &p)
	r.sent[sub.Endpoint] = p.Body
	return webpush.Delivered, nil
}

type pushFixture struct {
	super   *pgxpool.Pool
	st      *store.Store
	user    ulid.ULID
	account store.Account
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	superURL, appURL := os.Getenv("MAILROOM_TEST_SUPER_URL"), os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured")
	}
	ctx := context.Background()
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	st, err := store.Open(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	f := &pushFixture{super: super, st: st, user: ulid.Make()}
	acct := ulid.Make()
	f.exec(t, `INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.user.String(), "sub-"+f.user.String(), f.user.String()+"@example.com")
	f.exec(t, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1, $2, 'Work', 'imap.example.com', 'smtp.example.com', '\x00', $3)`,
		acct.String(), f.user.String(), ulid.Make().String())
	if err := st.UserTx(ctx, f.user, func(tx *store.UserTx) error {
		var e error
		f.account, e = tx.GetAccount(ctx, acct)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *pushFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func (f *pushFixture) device(t *testing.T, name, mode string) string {
	t.Helper()
	endpoint := "https://push.example.com/" + name
	f.exec(t, `INSERT INTO app.push_subscriptions (id, user_id, endpoint, keys_p256dh, keys_auth, device_name, notify_mode)
		VALUES ($1, $2, $3, 'k', 'a', $4, $5)`, ulid.Make().String(), f.user.String(), endpoint, name, mode)
	return endpoint
}

func (f *pushFixture) message(t *testing.T, from string, score *float64) ulid.ULID {
	t.Helper()
	id := ulid.Make()
	h := sha256.Sum256([]byte(id.String()))
	f.exec(t, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1, $2, 1, 1, $3)`,
		f.user.String(), h[:], ulid.Make().String())
	f.exec(t, `INSERT INTO app.messages (id, user_id, content_hash, from_address, received_at, size_bytes)
		VALUES ($1, $2, $3, $4, now(), 1)`, id.String(), f.user.String(), h[:], from)
	if score != nil {
		if err := f.st.UserTx(context.Background(), f.user, func(tx *store.UserTx) error {
			return tx.UpsertTriageScore(context.Background(), id, *score, "m", nil)
		}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (f *pushFixture) vip(t *testing.T, addr string) {
	t.Helper()
	c := ulid.Make()
	f.exec(t, `INSERT INTO app.contacts (id, user_id, vip) VALUES ($1, $2, true)`, c.String(), f.user.String())
	f.exec(t, `INSERT INTO app.contact_addresses (id, user_id, contact_id, address) VALUES ($1, $2, $3, $4)`,
		ulid.Make().String(), f.user.String(), c.String(), addr)
}

func (f *pushFixture) quiet(t *testing.T, q store.QuietHours) {
	t.Helper()
	if err := f.st.UserTx(context.Background(), f.user, func(tx *store.UserTx) error {
		return tx.UpdateDeskPreferences(context.Background(), store.DeskPreferencesPatch{QuietHours: &q})
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *pushFixture) notify(t *testing.T, at time.Time, ids []ulid.ULID) map[string]string {
	t.Helper()
	rec := &recordingSender{sent: map[string]string{}}
	n := NewStorePushNotifier(f.st, rec, discardLogger()).WithClock(func() time.Time { return at })
	if err := n.NotifyNewMail(context.Background(), f.user, f.account, len(ids), ids); err != nil {
		t.Fatalf("NotifyNewMail: %v", err)
	}
	return rec.sent
}

func f64(v float64) *float64 { return &v }

// One batch of four: one above the line, one from a VIP (below the line), two
// ordinary. Four devices, one per mode, each told exactly its own count.
func TestPushModesPerDevice(t *testing.T) {
	f := newPushFixture(t)
	f.vip(t, "boss@example.com")
	all := f.device(t, "all", "all")
	above := f.device(t, "above", "above_line")
	vip := f.device(t, "vip", "vip")
	either := f.device(t, "either", "above_line_or_vip")
	ids := []ulid.ULID{
		f.message(t, "news@example.com", f64(0.9)),
		f.message(t, "boss@example.com", f64(0.1)),
		f.message(t, "a@example.com", f64(0.1)),
		f.message(t, "b@example.com", nil),
	}

	sent := f.notify(t, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC), ids)
	want := map[string]string{
		all:    "4 new messages in Work",
		above:  "1 new message in Work",
		vip:    "1 new message in Work",
		either: "2 new messages in Work",
	}
	for ep, body := range want {
		if sent[ep] != body {
			t.Errorf("%s: told %q, want %q", ep, sent[ep], body)
		}
	}

	// A batch with nothing above the line and no VIP: the filtered devices are
	// sent NOTHING (not "0 new messages"); the every-message device is told.
	sent = f.notify(t, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC), ids[2:])
	if _, ok := sent[above]; ok {
		t.Errorf("above-the-line device was pushed for a batch with nothing above the line: %q", sent[above])
	}
	if _, ok := sent[vip]; ok {
		t.Error("VIP device was pushed for a batch with no VIP")
	}
	if sent[all] != "2 new messages in Work" {
		t.Errorf("every-message device: %q", sent[all])
	}
}

// Quiet hours, on a fake clock in the user's zone: only VIP mail goes through,
// whatever the device's mode; with the VIP exception off, nothing does; outside
// the period, the modes apply again.
func TestPushQuietHours(t *testing.T) {
	f := newPushFixture(t)
	f.vip(t, "boss@example.com")
	all := f.device(t, "all", "all")
	ids := []ulid.ULID{
		f.message(t, "boss@example.com", f64(0.1)),
		f.message(t, "news@example.com", f64(0.9)),
	}
	f.quiet(t, store.QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: 127, Timezone: "America/New_York", AllowVIP: true})

	night := time.Date(2026, 10, 15, 3, 30, 0, 0, time.UTC) // 23:30 EDT
	if got := f.notify(t, night, ids)[all]; got != "1 new message in Work" {
		t.Errorf("quiet, VIPs allowed: told %q, want the one VIP message", got)
	}
	noon := time.Date(2026, 10, 15, 16, 0, 0, 0, time.UTC) // 12:00 EDT
	if got := f.notify(t, noon, ids)[all]; got != "2 new messages in Work" {
		t.Errorf("outside quiet hours: told %q, want both", got)
	}
	f.quiet(t, store.QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: 127, Timezone: "America/New_York", AllowVIP: false})
	if got, ok := f.notify(t, night, ids)[all]; ok {
		t.Errorf("quiet, VIPs NOT allowed: told %q, want nothing", got)
	}
}

// The per-account switch silences every device for that account.
func TestPushAccountSwitchedOff(t *testing.T) {
	f := newPushFixture(t)
	all := f.device(t, "all", "all")
	ids := []ulid.ULID{f.message(t, "a@example.com", nil)}
	f.account.NotifyPush = false
	if got, ok := f.notify(t, time.Now(), ids)[all]; ok {
		t.Errorf("account with push off: told %q, want nothing", got)
	}
	f.account.NotifyPush = true
	if got := f.notify(t, time.Now(), ids)[all]; got != "1 new message in Work" {
		t.Errorf("control, push on: told %q", got)
	}
}
