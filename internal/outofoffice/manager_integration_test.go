// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package outofoffice

// The manager against a real Postgres (RLS, change_log, jobs) and the fake
// ManageSieve server. These are the operator's rules end to end: install only
// over an empty "active" slot, refuse and save nothing otherwise, restore
// exactly on the way out, leave every other script alone, and record drift
// rather than repair it.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/onboarding"
	"mailroom/internal/provider"
	"mailroom/internal/sieve/sievetest"
	"mailroom/internal/store"
)

type fixedCreds struct{}

func (fixedCreds) OpenAccountCredentials(context.Context, ulid.ULID, []byte) (onboarding.Credentials, error) {
	return onboarding.Credentials{IMAPUsername: "me@example.com", IMAPPassword: "pw"}, nil
}

type harness struct {
	super *pgxpool.Pool
	store *store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	superURL, appURL := os.Getenv("MAILROOM_TEST_SUPER_URL"), os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return &harness{super: super, store: store.NewFromPool(app)}
}

func (h *harness) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// seed creates a user with one account (profile) and one fixed identity.
func (h *harness) seed(t *testing.T, profile string) (userID, accountID ulid.ULID) {
	t.Helper()
	userID, accountID = store.NewULID(), store.NewULID()
	tag := userID.String()
	h.exec(t, `INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		userID.String(), "oidc-"+tag, tag+"@example.org")
	h.exec(t, `INSERT INTO app.accounts (id, user_id, name, provider_profile, imap_host, smtp_host, credentials_ct, credentials_dek)
	           VALUES ($1, $2, 'main', $3, 'imap.example.com', 'smtp.example.com', $4, $5)`,
		accountID.String(), userID.String(), profile, []byte{1}, store.NewULID().String())
	h.exec(t, `INSERT INTO app.identities (id, user_id, account_id, kind, address, enabled)
	           VALUES ($1, $2, $3, 'fixed', 'me@example.com', true)`,
		store.NewULID().String(), userID.String(), accountID.String())
	return userID, accountID
}

func (h *harness) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) changeRows(t *testing.T, userID ulid.ULID) int {
	return h.count(t, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'OutOfOffice'`, userID.String())
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newFakeServer(t *testing.T, scripts ...sievetest.Script) *sievetest.Server {
	return sievetest.New(t, sievetest.Options{
		Username: "me@example.com", Password: "pw", AllowWrites: true, MaxScriptSize: 49152,
		Scripts: append([]sievetest.Script{{Name: "imported", Body: importedBody}}, scripts...),
	})
}

func (h *harness) manager(f *sievetest.Server, c *clock) *Manager {
	return NewManager(h.store, fixedCreds{},
		WithAllowPrivateHosts(true), WithClock(c.now), WithTLSConfig(f.ClientTLSConfig()),
		WithEndpoint(func(profile, imapHost string) (provider.SieveEndpoint, bool, string) {
			if profile == "gmail" {
				return provider.ManageSieveEndpoint(profile, imapHost)
			}
			return provider.SieveEndpoint{Host: f.Host(), Port: f.Port(), Source: "test"}, true, ""
		}))
}

func ours(f *sievetest.Server) (sievetest.Script, bool) { return get(f, ScriptName) }

func onInput(c *clock) Input {
	to := c.t.Add(72 * time.Hour)
	return Input{Enabled: true, To: &to, Subject: "Away", Body: "I am away.", ReplyDays: 3}
}

func TestManagerEnableSavesOnlyWhatTheServerAccepted(t *testing.T) {
	h := newHarness(t)
	c := &clock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f := newFakeServer(t)
	m := h.manager(f, c)
	userID, accountID := h.seed(t, "migadu")
	ctx := context.Background()

	v, err := m.Set(ctx, userID, accountID, onInput(c))
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	sc, ok := ours(f)
	if !ok || !sc.Active {
		t.Fatalf("our script is not active on the server: %+v", sc)
	}
	sum := sha256.Sum256([]byte(sc.Body))
	if !v.Enabled || string(v.InstalledSHA256) != string(sum[:]) || v.ServerState != "active" || v.FromAddress != "me@example.com" {
		t.Fatalf("view = %+v", v.OutOfOffice)
	}
	assertImportedUntouched(t, f)
	if n := h.changeRows(t, userID); n != 1 {
		t.Fatalf("change rows after enable = %d, want 1", n)
	}
	// The end-of-window timer, carrying the account id and nothing else.
	var payload []byte
	var fire time.Time
	if err := h.super.QueryRow(ctx, `SELECT payload, fire_at FROM app.jobs WHERE user_id = $1 AND kind = $2`,
		userID.String(), JobKind).Scan(&payload, &fire); err != nil {
		t.Fatalf("end timer: %v", err)
	}
	var keys map[string]any
	_ = json.Unmarshal(payload, &keys)
	if len(keys) != 1 || keys["account_id"] != accountID.String() || !fire.Equal(*onInput(c).To) {
		t.Fatalf("timer payload %s at %v", payload, fire)
	}

	// An identical save is a no-op for the change log (the clock is fixed).
	if _, err := m.Set(ctx, userID, accountID, onInput(c)); err != nil {
		t.Fatal(err)
	}
	if n := h.changeRows(t, userID); n != 1 {
		t.Fatalf("an identical save appended change rows: %d", n)
	}
}

// TestManagerRefusesAndSavesNothing: with a foreign script active, turning on
// (or changing) the reply is refused, the database is untouched, and so is
// every script on the server.
func TestManagerRefusesAndSavesNothing(t *testing.T) {
	h := newHarness(t)
	c := &clock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f := newFakeServer(t, sievetest.Script{Name: "webmail", Body: "keep;\r\n", Active: true})
	m := h.manager(f, c)
	userID, accountID := h.seed(t, "migadu")

	_, err := m.Set(context.Background(), userID, accountID, onInput(c))
	var fe *ForeignActiveError
	if !errors.As(err, &fe) || fe.Script != "webmail" {
		t.Fatalf("err = %v, want a refusal naming webmail", err)
	}
	if n := h.count(t, `SELECT count(*) FROM app.out_of_office WHERE account_id = $1`, accountID.String()); n != 0 {
		t.Fatal("a refused enable saved a row")
	}
	if h.changeRows(t, userID) != 0 || f.WriteAttempts() != 0 {
		t.Fatalf("refusal changed something: change rows %d, server writes %d", h.changeRows(t, userID), f.WriteAttempts())
	}
	if h.count(t, `SELECT count(*) FROM app.jobs WHERE user_id = $1`, userID.String()) != 0 {
		t.Fatal("a refused enable scheduled timers")
	}
}

// TestManagerDisableRestoresExactly: on, then off: nothing active, our script
// gone, "imported" untouched — the state before.
func TestManagerDisableRestoresExactly(t *testing.T) {
	h := newHarness(t)
	c := &clock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f := newFakeServer(t)
	m := h.manager(f, c)
	userID, accountID := h.seed(t, "migadu")
	ctx := context.Background()
	before := f.Scripts()

	if _, err := m.Set(ctx, userID, accountID, onInput(c)); err != nil {
		t.Fatal(err)
	}
	off := onInput(c)
	off.Enabled = false
	v, err := m.Set(ctx, userID, accountID, off)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if v.Enabled || v.InstalledSHA256 != nil || v.ServerState != "off" || v.Body != "I am away." {
		t.Fatalf("after disable: %+v", v.OutOfOffice)
	}
	after := f.Scripts()
	if len(after) != len(before) || after[0] != before[0] {
		t.Fatalf("server not restored:\nbefore %+v\nafter  %+v", before, after)
	}
}

// TestManagerDisableLeavesTheUsersNewScript: the user activated their own
// filters in webmail while the reply was on. Turning it off keeps theirs and
// says so.
func TestManagerDisableLeavesTheUsersNewScript(t *testing.T) {
	h := newHarness(t)
	c := &clock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f := newFakeServer(t)
	m := h.manager(f, c)
	userID, accountID := h.seed(t, "migadu")
	ctx := context.Background()
	if _, err := m.Set(ctx, userID, accountID, onInput(c)); err != nil {
		t.Fatal(err)
	}
	f.Activate("imported")
	off := onInput(c)
	off.Enabled = false
	v, err := m.Set(ctx, userID, accountID, off)
	if err != nil {
		t.Fatal(err)
	}
	if sc, _ := get(f, "imported"); !sc.Active || sc.Body != importedBody {
		t.Fatalf("the user's script was changed: %+v", sc)
	}
	if _, ok := ours(f); ok {
		t.Fatal("our inactive script was left behind")
	}
	if !strings.Contains(v.ServerDetail, `"imported"`) {
		t.Fatalf("the user was not told: %q", v.ServerDetail)
	}
}

// TestManagerTimerEndsTheReply: at the end of the window the timer removes the
// script and turns the reply off.
func TestManagerTimerEndsTheReply(t *testing.T) {
	h := newHarness(t)
	c := &clock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f := newFakeServer(t)
	m := h.manager(f, c)
	userID, accountID := h.seed(t, "migadu")
	ctx := context.Background()
	in := onInput(c)
	if _, err := m.Set(ctx, userID, accountID, in); err != nil {
		t.Fatal(err)
	}
	// Before the end: the timer looks, finds it intact, changes nothing.
	c.t = in.To.Add(-time.Hour)
	if err := m.Reconcile(ctx, userID, accountID); err != nil {
		t.Fatal(err)
	}
	if sc, ok := ours(f); !ok || !sc.Active {
		t.Fatal("the reply was removed before its window ended")
	}
	c.t = *in.To
	if err := m.Reconcile(ctx, userID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, ok := ours(f); ok {
		t.Fatal("the reply outlived its window")
	}
	v, _ := m.Get(ctx, userID, accountID)
	if v.Enabled || v.ServerState != "off" {
		t.Fatalf("after the end: %+v", v.OutOfOffice)
	}
	assertImportedUntouched(t, f)
}

// TestManagerRecordsDriftAndNeverRepairsIt: a webmail edit or switch is
// recorded with a sentence, and no timer re-activates anything.
func TestManagerRecordsDriftAndNeverRepairsIt(t *testing.T) {
	h := newHarness(t)
	c := &clock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f := newFakeServer(t)
	m := h.manager(f, c)
	userID, accountID := h.seed(t, "migadu")
	ctx := context.Background()
	if _, err := m.Set(ctx, userID, accountID, onInput(c)); err != nil {
		t.Fatal(err)
	}
	f.Replace(ScriptName, "keep;\r\n")
	v, err := m.Check(ctx, userID, accountID)
	if err != nil || v.ServerState != "modified" || v.ServerDetail == "" {
		t.Fatalf("after a webmail edit: %+v %v", v.OutOfOffice, err)
	}
	f.Activate("imported")
	writes := f.WriteAttempts()
	if err := m.Reconcile(ctx, userID, accountID); err != nil {
		t.Fatal(err)
	}
	v, _ = m.Get(ctx, userID, accountID)
	if v.ServerState != "displaced" || !strings.Contains(v.ServerDetail, `"imported"`) {
		t.Fatalf("after a webmail switch: %+v", v.OutOfOffice)
	}
	if f.WriteAttempts() != writes {
		t.Fatal("the timer wrote to the server while another script was active")
	}
	if sc, _ := get(f, "imported"); !sc.Active {
		t.Fatal("the timer switched the user's script off")
	}
}

// TestManagerContactsOnly: the list is exactly the people written to or
// marked VIP, never a blocked one, and its bound is recorded.
func TestManagerContactsOnly(t *testing.T) {
	h := newHarness(t)
	c := &clock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f := newFakeServer(t)
	m := h.manager(f, c)
	userID, accountID := h.seed(t, "migadu")
	add := func(addr string, outbound int, vip, blocked bool) {
		cid := store.NewULID().String()
		blockedAt := any(nil)
		if blocked {
			blockedAt = time.Now()
		}
		h.exec(t, `INSERT INTO app.contacts (id, user_id, vip, blocked, blocked_at) VALUES ($1, $2, $3, $4, $5)`,
			cid, userID.String(), vip, blocked, blockedAt)
		h.exec(t, `INSERT INTO app.contact_addresses (id, user_id, contact_id, address, outbound_count) VALUES ($1, $2, $3, $4, $5)`,
			store.NewULID().String(), userID.String(), cid, addr, outbound)
	}
	add("friend@example.net", 2, false, false)
	add("boss@example.net", 0, true, false)
	add("newsletter@example.net", 0, false, false)
	add("pest@example.net", 5, false, true)

	in := onInput(c)
	in.ContactsOnly = true
	v, err := m.Set(context.Background(), userID, accountID, in)
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := ours(f)
	for addr, want := range map[string]bool{
		"friend@example.net": true, "boss@example.net": true,
		"newsletter@example.net": false, "pest@example.net": false,
	} {
		if strings.Contains(sc.Body, `"`+addr+`"`) != want {
			t.Errorf("%s in the script = %v, want %v", addr, !want, want)
		}
	}
	if v.ContactsIncluded == nil || *v.ContactsIncluded != 2 || *v.ContactsTotal != 2 {
		t.Fatalf("contacts bound not recorded: %v of %v", v.ContactsIncluded, v.ContactsTotal)
	}
	// And the daily refresh is scheduled.
	if h.count(t, `SELECT count(*) FROM app.jobs WHERE user_id = $1 AND kind = $2`, userID.String(), JobKind) != 2 {
		t.Fatal("want the end timer and the daily refresh")
	}
}

// TestManagerIsolation: another user naming this account is told it does not
// exist, and nothing reaches the server.
func TestManagerIsolation(t *testing.T) {
	h := newHarness(t)
	c := &clock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f := newFakeServer(t)
	m := h.manager(f, c)
	_, accountA := h.seed(t, "migadu")
	userB, _ := h.seed(t, "migadu")
	ctx := context.Background()
	if _, err := m.Set(ctx, userB, accountA, onInput(c)); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("Set on another user's account: %v", err)
	}
	if _, err := m.Check(ctx, userB, accountA); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("Check on another user's account: %v", err)
	}
	if err := m.Reconcile(ctx, userB, accountA); err != nil {
		t.Fatalf("Reconcile on another user's account: %v", err)
	}
	if len(f.Verbs()) != 0 {
		t.Fatal("another user's request reached the provider")
	}
	list, err := m.List(ctx, userB)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range list {
		if v.AccountID == accountA {
			t.Fatal("List showed another user's account")
		}
	}
}

func TestManagerUnsupportedProvider(t *testing.T) {
	h := newHarness(t)
	c := &clock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f := newFakeServer(t)
	m := h.manager(f, c)
	userID, accountID := h.seed(t, "gmail")
	if _, err := m.Set(context.Background(), userID, accountID, onInput(c)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("gmail: %v", err)
	}
	v, _ := m.Get(context.Background(), userID, accountID)
	if v.Supported || v.Unsupported == "" {
		t.Fatalf("gmail view: %+v", v)
	}
}
