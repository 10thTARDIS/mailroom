// SPDX-License-Identifier: AGPL-3.0-only

package outofoffice

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/onboarding"
	"mailroom/internal/provider"
	"mailroom/internal/sieve"
	"mailroom/internal/store"
)

// JobKind is the out-of-office timer: it fires at the start and the end of the
// window and, for a contacts-only reply, once a day to refresh the list. Its
// payload is the account id and nothing else (store.jobPayloadKeys).
const JobKind = "out_of_office_sync"

// refreshInterval is how often a contacts-only reply rebuilds its list.
const refreshInterval = 24 * time.Hour

// Credentials opens an account's sealed credentials.
type Credentials interface {
	OpenAccountCredentials(ctx context.Context, userID ulid.ULID, ct []byte) (onboarding.Credentials, error)
}

// KeyCredentials opens credentials with the instance master key, the same way
// mailroom-admin's reports do.
type KeyCredentials struct{ Keys crypto.KeyProvider }

// OpenAccountCredentials implements Credentials.
func (k KeyCredentials) OpenAccountCredentials(ctx context.Context, userID ulid.ULID, ct []byte) (onboarding.Credentials, error) {
	master, err := k.Keys.MasterKey(ctx)
	if err != nil {
		return onboarding.Credentials{}, err
	}
	defer crypto.Zero(master)
	return onboarding.OpenCredentials(master, userID, ct)
}

// Manager runs the out-of-office reply for every account: it validates the
// form, talks to the provider's ManageSieve server through Install / Remove /
// Inspect, and keeps app.out_of_office in step with what the server holds.
type Manager struct {
	store        *store.Store
	creds        Credentials
	allowPrivate bool
	log          *slog.Logger
	now          func() time.Time
	open         func(context.Context, sieve.Config) (Writer, error)
	tlsConfig    *tls.Config
	endpoint     func(profile, imapHost string) (provider.SieveEndpoint, bool, string)

	mu    sync.Mutex
	locks map[ulid.ULID]*sync.Mutex
}

// Option configures a Manager.
type Option func(*Manager)

// WithAllowPrivateHosts is MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS (#527).
func WithAllowPrivateHosts(v bool) Option { return func(m *Manager) { m.allowPrivate = v } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(m *Manager) { m.log = l } }

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(m *Manager) { m.now = now } }

// WithOpener replaces the ManageSieve dial (tests).
func WithOpener(f func(context.Context, sieve.Config) (Writer, error)) Option {
	return func(m *Manager) { m.open = f }
}

// WithTLSConfig sets the TLS configuration for the ManageSieve dial (tests
// trust their fake server's CA; production leaves it nil).
func WithTLSConfig(c *tls.Config) Option { return func(m *Manager) { m.tlsConfig = c } }

// WithEndpoint replaces the provider-profile endpoint lookup (tests point it
// at a fake server).
func WithEndpoint(f func(profile, imapHost string) (provider.SieveEndpoint, bool, string)) Option {
	return func(m *Manager) { m.endpoint = f }
}

// NewManager builds a Manager.
func NewManager(st *store.Store, creds Credentials, opts ...Option) *Manager {
	m := &Manager{
		store:    st,
		creds:    creds,
		log:      slog.Default(),
		now:      time.Now,
		open:     func(ctx context.Context, c sieve.Config) (Writer, error) { return sieve.OpenWrite(ctx, c) },
		endpoint: provider.ManageSieveEndpoint,
		locks:    map[ulid.ULID]*sync.Mutex{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Manager) lock(account ulid.ULID) func() {
	m.mu.Lock()
	l, ok := m.locks[account]
	if !ok {
		l = &sync.Mutex{}
		m.locks[account] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Input is a save from the form.
type Input struct {
	Enabled      bool
	From, To     *time.Time
	Subject      string
	Body         string
	ReplyDays    int
	FromAddress  string
	ContactsOnly bool
}

// View is one account's out-of-office object as the API shows it.
type View struct {
	store.OutOfOffice
	AccountName string
	// Addresses are the account's own fixed addresses: the reply's From must be
	// one of them, and the reply goes only to mail that names one in To or Cc.
	Addresses []string
	// Supported is false when the provider has no ManageSieve service.
	Supported   bool
	Unsupported string
}

// ErrUnsupported is returned when the provider has no ManageSieve service.
var ErrUnsupported = errors.New("this provider has no ManageSieve service, so Mailroom cannot set an out-of-office reply there")

// account is everything a sequence needs about one account.
type account struct {
	acct      store.Account
	row       store.OutOfOffice
	addresses []string
}

func (m *Manager) load(ctx context.Context, userID, accountID ulid.ULID) (account, error) {
	var a account
	err := m.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if a.acct, e = tx.GetAccount(ctx, accountID); e != nil {
			return e
		}
		if a.row, e = tx.GetOutOfOffice(ctx, accountID); e != nil {
			return e
		}
		ids, e := tx.ListIdentities(ctx)
		if e != nil {
			return e
		}
		a.addresses = accountAddresses(accountID, ids)
		return nil
	})
	return a, err
}

func accountAddresses(accountID ulid.ULID, ids []store.Identity) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if id.AccountID != accountID || id.Kind != store.IdentityFixed || !id.Enabled {
			continue
		}
		a := strings.ToLower(strings.TrimSpace(id.Address))
		if a != "" && !seen[a] && checkAddress(a) == nil {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

func (m *Manager) view(a account) View {
	_, ok, why := m.endpoint(a.acct.ProviderProfile, a.acct.IMAPHost)
	return View{OutOfOffice: a.row, AccountName: a.acct.Name, Addresses: a.addresses, Supported: ok, Unsupported: why}
}

// List returns every account's out-of-office object, stored or default.
func (m *Manager) List(ctx context.Context, userID ulid.ULID) ([]View, error) {
	var (
		accts []store.Account
		rows  []store.OutOfOffice
		ids   []store.Identity
	)
	if err := m.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if accts, e = tx.ListAccounts(ctx); e != nil {
			return e
		}
		if rows, e = tx.ListOutOfOffice(ctx); e != nil {
			return e
		}
		ids, e = tx.ListIdentities(ctx)
		return e
	}); err != nil {
		return nil, err
	}
	byAcct := map[ulid.ULID]store.OutOfOffice{}
	for _, r := range rows {
		byAcct[r.AccountID] = r
	}
	out := make([]View, 0, len(accts))
	for _, ac := range accts {
		row, ok := byAcct[ac.ID]
		if !ok {
			row = store.DefaultOutOfOffice(ac.ID)
		}
		out = append(out, m.view(account{acct: ac, row: row, addresses: accountAddresses(ac.ID, ids)}))
	}
	return out, nil
}

// Get returns one account's object.
func (m *Manager) Get(ctx context.Context, userID, accountID ulid.ULID) (View, error) {
	a, err := m.load(ctx, userID, accountID)
	if err != nil {
		return View{}, err
	}
	return m.view(a), nil
}

func (m *Manager) dial(ctx context.Context, userID ulid.ULID, acct store.Account) (Writer, error) {
	ep, ok, why := m.endpoint(acct.ProviderProfile, acct.IMAPHost)
	if !ok {
		return nil, fmt.Errorf("%w (%s)", ErrUnsupported, why)
	}
	creds, err := m.creds.OpenAccountCredentials(ctx, userID, acct.CredentialsCT)
	if err != nil {
		return nil, fmt.Errorf("outofoffice: open credentials: %w", err)
	}
	return m.open(ctx, sieve.Config{
		Host: ep.Host, Port: ep.Port, ImplicitTLS: ep.ImplicitTLS,
		Username: creds.IMAPUsername, Password: creds.IMAPPassword,
		AllowPrivateHosts: m.allowPrivate, TLSConfig: m.tlsConfig,
	})
}

func closeWriter(ctx context.Context, w Writer) {
	if err := w.Logout(ctx); err != nil {
		_ = w.Close()
	}
}

// validate checks a save and fills the defaults; it returns the cleaned input.
func (m *Manager) validate(in Input, a account) (Input, error) {
	if in.ReplyDays == 0 {
		in.ReplyDays = 7
	}
	if in.ReplyDays < MinReplyDays || in.ReplyDays > MaxReplyDays {
		return in, fmt.Errorf("%w: reply interval must be %d to %d days", ErrInvalid, MinReplyDays, MaxReplyDays)
	}
	if in.From != nil && in.To != nil && !in.From.Before(*in.To) {
		return in, fmt.Errorf("%w: the last day must be after the first day", ErrInvalid)
	}
	in.Subject = oneLine(in.Subject)
	if len(in.Subject) > 998 {
		return in, fmt.Errorf("%w: the subject is too long", ErrInvalid)
	}
	in.FromAddress = strings.ToLower(strings.TrimSpace(in.FromAddress))
	if in.FromAddress == "" && len(a.addresses) > 0 {
		in.FromAddress = a.addresses[0]
	}
	if in.FromAddress != "" {
		found := false
		for _, ad := range a.addresses {
			if ad == in.FromAddress {
				found = true
			}
		}
		if !found {
			return in, fmt.Errorf("%w: the reply can only come from one of this account's addresses", ErrInvalid)
		}
	}
	if in.Enabled {
		if in.To != nil && !m.now().Before(*in.To) {
			return in, fmt.Errorf("%w: the last day has already passed", ErrInvalid)
		}
		if strings.TrimSpace(in.Body) == "" {
			return in, fmt.Errorf("%w: write the reply before turning it on", ErrInvalid)
		}
	}
	return in, nil
}

// Set saves the form. Turning the reply ON installs and activates the script
// first and saves only if the server accepted it; a refusal (another script is
// active) is returned as *ForeignActiveError and nothing is saved. Turning it
// OFF removes the script first and saves only once the server agreed.
func (m *Manager) Set(ctx context.Context, userID, accountID ulid.ULID, in Input) (View, error) {
	defer m.lock(accountID)()
	a, err := m.load(ctx, userID, accountID)
	if err != nil {
		return View{}, err
	}
	in, err = m.validate(in, a)
	if err != nil {
		return View{}, err
	}
	now := m.now().UTC()
	next := a.row
	next.Enabled, next.From, next.To = in.Enabled, in.From, in.To
	next.Subject, next.Body, next.ReplyDays = in.Subject, in.Body, in.ReplyDays
	next.FromAddress, next.ContactsOnly = in.FromAddress, in.ContactsOnly

	var jobs []time.Time
	switch {
	case in.Enabled:
		w, err := m.dial(ctx, userID, a.acct)
		if err != nil {
			return View{}, err
		}
		built, err := m.build(ctx, userID, w, a, in)
		if err != nil {
			closeWriter(ctx, w)
			return View{}, err
		}
		if err := Install(ctx, w, built.Script); err != nil {
			closeWriter(ctx, w)
			return View{}, err
		}
		closeWriter(ctx, w)
		next.InstalledSHA256 = built.SHA256[:]
		next.ServerState, next.ServerDetail, next.CheckedAt = string(StateActive), "", &now
		next.ContactsIncluded, next.ContactsTotal = contactCounts(in.ContactsOnly, built)
		if in.From != nil && in.From.After(now) {
			jobs = append(jobs, *in.From)
		}
		if in.To != nil {
			jobs = append(jobs, *in.To)
		}
		if in.ContactsOnly {
			jobs = append(jobs, now.Add(refreshInterval))
		}
	case a.row.Enabled || a.row.InstalledSHA256 != nil || drifted(a.row.ServerState):
		w, err := m.dial(ctx, userID, a.acct)
		if err != nil {
			return View{}, err
		}
		outcome, other, err := Remove(ctx, w)
		closeWriter(ctx, w)
		if err != nil {
			return View{}, err
		}
		next.InstalledSHA256 = nil
		next.ServerState, next.ServerDetail, next.CheckedAt = string(StateOff), removeDetail(outcome, other), &now
		next.ContactsIncluded, next.ContactsTotal = nil, nil
	default:
		// Off and nothing installed: a settings-only save, no server contact.
		next.InstalledSHA256 = nil
	}
	if err := m.save(ctx, userID, next, jobs); err != nil {
		return View{}, err
	}
	a.row = next
	a.row.Stored = true
	return m.view(a), nil
}

func drifted(state string) bool {
	switch ServerState(state) {
	case StateDisplaced, StateMissing, StateModified:
		return true
	}
	return false
}

func contactCounts(contactsOnly bool, b Built) (*int, *int) {
	if !contactsOnly {
		return nil, nil
	}
	inc, tot := b.ContactsIncluded, b.ContactsTotal
	return &inc, &tot
}

func removeDetail(o RemoveOutcome, other string) string {
	if o == RemovedLeftOther {
		return fmt.Sprintf("The filter script %q had been made active on the server since; Mailroom left it active and deleted only its own inactive reply script.",
			sieve.Printable(other))
	}
	return ""
}

// build gathers the contacts (if any) and generates the script against the
// server's size limit.
func (m *Manager) build(ctx context.Context, userID ulid.ULID, w Writer, a account, in Input) (Built, error) {
	if len(a.addresses) == 0 {
		return Built{}, fmt.Errorf("%w: this account has no address of its own to reply from", ErrInvalid)
	}
	var contacts []string
	if in.ContactsOnly {
		var total int
		if err := m.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			contacts, total, e = tx.OutOfOfficeContacts(ctx, MaxContacts)
			return e
		}); err != nil {
			return Built{}, err
		}
		b, err := Build(m.settings(in, a, contacts), w.Capabilities().MaxScriptSize())
		if err != nil {
			return Built{}, err
		}
		b.ContactsTotal = total // the whole set, not only the first MaxContacts
		return b, nil
	}
	return Build(m.settings(in, a, nil), w.Capabilities().MaxScriptSize())
}

func (m *Manager) settings(in Input, a account, contacts []string) ScriptSettings {
	return ScriptSettings{
		From: in.From, To: in.To, Subject: in.Subject, Body: in.Body, Days: in.ReplyDays,
		FromAddress: in.FromAddress, Addresses: a.addresses, ContactsOnly: in.ContactsOnly, Contacts: contacts,
	}
}

// save writes the row (one change_log row if anything changed) and schedules
// the timers, in one transaction.
func (m *Manager) save(ctx context.Context, userID ulid.ULID, row store.OutOfOffice, jobs []time.Time) error {
	payload, err := json.Marshal(map[string]string{"account_id": row.AccountID.String()})
	if err != nil {
		return err
	}
	return m.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if _, err := tx.PutOutOfOffice(ctx, row); err != nil {
			return err
		}
		for _, at := range jobs {
			key := fmt.Sprintf("oof:%s:%d:%s", row.AccountID, at.Unix(), store.NewULID())
			if _, err := tx.EnqueueJob(ctx, JobKind, at, payload, key); err != nil {
				return err
			}
		}
		return nil
	})
}

// Check asks the server what it holds now and records it.
func (m *Manager) Check(ctx context.Context, userID, accountID ulid.ULID) (View, error) {
	defer m.lock(accountID)()
	a, err := m.load(ctx, userID, accountID)
	if err != nil {
		return View{}, err
	}
	next := m.inspect(ctx, userID, a)
	if err := m.save(ctx, userID, next, nil); err != nil {
		return View{}, err
	}
	a.row = next
	a.row.Stored = true
	return m.view(a), nil
}

// inspect returns the row with the server's current state recorded on it. A
// failure to reach the server is recorded as StateError, not returned.
func (m *Manager) inspect(ctx context.Context, userID ulid.ULID, a account) store.OutOfOffice {
	next := a.row
	now := m.now().UTC()
	next.CheckedAt = &now
	w, err := m.dial(ctx, userID, a.acct)
	if err != nil {
		next.ServerState, next.ServerDetail = string(StateError), sieve.Printable(err.Error())
		return next
	}
	defer closeWriter(ctx, w)
	ins, err := Inspect(ctx, w, a.row.InstalledSHA256)
	if err != nil {
		next.ServerState, next.ServerDetail = string(StateError), sieve.Printable(err.Error())
		return next
	}
	next.ServerState, next.ServerDetail = string(ins.State), inspectDetail(ins)
	return next
}

func inspectDetail(ins Inspection) string {
	switch ins.State {
	case StateDisplaced:
		return fmt.Sprintf("The filter script %q is now the active script on the server, so the reply is not being sent. Mailroom does not switch it off.",
			sieve.Printable(ins.Other))
	case StateMissing:
		return "The reply's script is no longer active on the server: it was deactivated or deleted outside Mailroom. Turn out of office off and on again to reinstall it."
	case StateModified:
		return "The reply's script on the server is not the one Mailroom installed: it was changed outside Mailroom."
	}
	return ""
}

// HandleJob is the dispatcher handler for JobKind.
func (m *Manager) HandleJob(ctx context.Context, job store.Job) error {
	if job.UserID == nil {
		return errors.New("outofoffice: job has no user")
	}
	var p struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("outofoffice: job payload: %w", err)
	}
	accountID, err := ulid.Parse(p.AccountID)
	if err != nil {
		return fmt.Errorf("outofoffice: job account id: %w", err)
	}
	return m.Reconcile(ctx, *job.UserID, accountID)
}

// Reconcile is what every timer does: end the reply once its window has
// closed; otherwise check the server and, for a contacts-only reply that is
// still exactly ours and active, refresh the list. It never activates a script
// and never touches another one: drift is recorded, not repaired.
func (m *Manager) Reconcile(ctx context.Context, userID, accountID ulid.ULID) error {
	defer m.lock(accountID)()
	a, err := m.load(ctx, userID, accountID)
	if errors.Is(err, store.ErrAccountNotFound) {
		return nil // the account was deleted; the row went with it
	}
	if err != nil {
		return err
	}
	if !a.row.Enabled {
		return nil
	}
	now := m.now().UTC()
	next := a.row

	if a.row.To != nil && !now.Before(*a.row.To) {
		w, err := m.dial(ctx, userID, a.acct)
		if err != nil {
			m.recordError(ctx, userID, a.row, err)
			return err // retried: the reply must not outlive its window
		}
		outcome, other, err := Remove(ctx, w)
		closeWriter(ctx, w)
		if err != nil {
			m.recordError(ctx, userID, a.row, err)
			return err
		}
		next.Enabled, next.InstalledSHA256 = false, nil
		next.ServerState, next.CheckedAt = string(StateOff), &now
		next.ServerDetail = removeDetail(outcome, other)
		next.ContactsIncluded, next.ContactsTotal = nil, nil
		return m.save(ctx, userID, next, nil)
	}

	next = m.inspect(ctx, userID, a)
	var jobs []time.Time
	if next.ServerState == string(StateActive) && a.row.ContactsOnly {
		refreshed, err := m.refresh(ctx, userID, a)
		if err != nil {
			next.ServerState, next.ServerDetail = string(StateError), sieve.Printable(err.Error())
		} else {
			next = refreshed
		}
	}
	if a.row.ContactsOnly {
		at := now.Add(refreshInterval)
		if a.row.To == nil || at.Before(*a.row.To) {
			jobs = append(jobs, at)
		}
	}
	return m.save(ctx, userID, next, jobs)
}

// refresh rebuilds a contacts-only script and replaces ours in place if the
// list changed. Called only while ours is active and unmodified.
func (m *Manager) refresh(ctx context.Context, userID ulid.ULID, a account) (store.OutOfOffice, error) {
	w, err := m.dial(ctx, userID, a.acct)
	if err != nil {
		return a.row, err
	}
	defer closeWriter(ctx, w)
	in := Input{Enabled: true, From: a.row.From, To: a.row.To, Subject: a.row.Subject, Body: a.row.Body,
		ReplyDays: a.row.ReplyDays, FromAddress: a.row.FromAddress, ContactsOnly: true}
	built, err := m.build(ctx, userID, w, a, in)
	if err != nil {
		return a.row, err
	}
	next := a.row
	now := m.now().UTC()
	next.CheckedAt = &now
	next.ServerState, next.ServerDetail = string(StateActive), ""
	next.ContactsIncluded, next.ContactsTotal = contactCounts(true, built)
	if string(built.SHA256[:]) == string(a.row.InstalledSHA256) {
		return next, nil
	}
	// Ours is the active script (inspect said so a moment ago): CHECKSCRIPT,
	// then PUTSCRIPT replaces it in place. Nothing is activated.
	if err := w.CheckScript(ctx, built.Script); err != nil {
		return a.row, err
	}
	if err := w.PutScript(ctx, ScriptName, built.Script); err != nil {
		return a.row, err
	}
	next.InstalledSHA256 = built.SHA256[:]
	return next, nil
}

func (m *Manager) recordError(ctx context.Context, userID ulid.ULID, row store.OutOfOffice, cause error) {
	now := m.now().UTC()
	row.ServerState, row.ServerDetail, row.CheckedAt = string(StateError), sieve.Printable(cause.Error()), &now
	if err := m.save(ctx, userID, row, nil); err != nil {
		m.log.Warn("out of office: could not record a server error", "err", err)
	}
}
