// SPDX-License-Identifier: AGPL-3.0-only

package onboarding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/egress"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// AccountStore is the store surface AddAccount needs. It is satisfied by
// StoreAdapter, which runs each call inside a Store.UserTx (the only
// legitimate path to the database — CLAUDE.md).
type AccountStore interface {
	CreateAccount(ctx context.Context, userID ulid.ULID, row store.AccountRow) (ulid.ULID, error)
	SetAccountSyncEnabled(ctx context.Context, userID, accountID ulid.ULID, enabled bool) error
}

// Dialer opens an authenticated IMAP session used to validate an account's
// credentials. The production value is sync.Dial; tests inject a fake.
type Dialer func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error)

// AccountSpec is the operator-provided description of an account to add.
type AccountSpec struct {
	Name            string
	ProviderProfile string // "" -> store default ("generic")
	IMAPHost        string
	IMAPPort        int // 0 -> 993
	SMTPHost        string
	SMTPPort        int // 0 -> 465
	Username        string
	Password        string
	SMTPUsername    string // "" -> Username
	SMTPPassword    string // "" -> Password

	// WritebackEnabled declares whether the account should be created already
	// shadowed (ADR-028 ingest-only mode), forwarded verbatim to
	// store.AccountRow.WritebackEnabled — see that field's doc for why it is a
	// pointer (nil -> schema default true; a plain bool would silently shadow
	// every caller that does not know about this field).
	WritebackEnabled *bool

	// TLS selects the IMAP transport for the validation dial. Real accounts
	// use sync.TLSImplicit (993); the others exist only for the test harness.
	TLS sync.TLSMode
	// InsecureSkipVerify and ServerName are for the ephemeral test IMAP server
	// only; never set them for a real account.
	InsecureSkipVerify bool
	ServerName         string
}

// Validate checks the spec is complete, without any I/O.
func (s AccountSpec) Validate() error {
	switch {
	case s.Name == "":
		return errors.New("onboarding: spec: name is required")
	case s.IMAPHost == "":
		return errors.New("onboarding: spec: imap host is required")
	case s.SMTPHost == "":
		return errors.New("onboarding: spec: smtp host is required")
	case s.Username == "":
		return errors.New("onboarding: spec: username is required")
	case s.Password == "":
		return errors.New("onboarding: spec: password is required")
	case s.IMAPPort < 0 || s.IMAPPort > 65535:
		return fmt.Errorf("onboarding: spec: imap port %d out of range", s.IMAPPort)
	case s.SMTPPort < 0 || s.SMTPPort > 65535:
		return fmt.Errorf("onboarding: spec: smtp port %d out of range", s.SMTPPort)
	default:
		return nil
	}
}

// Result reports the outcome of AddAccount. AccountID is set whenever the row
// was created, even if validation then failed. Validated says whether the IMAP
// dial succeeded; SyncEnabled says what the account was LEFT as, which is not
// the same thing (mailroom#435): a validation failure the server did not cause
// — a network or TLS error, a timeout — keeps sync enabled so the sync loop's
// backoff retries it, and only a rejection (bad credentials, refused config)
// disables it. See latchesSync.
type Result struct {
	AccountID     ulid.ULID
	Validated     bool
	SyncEnabled   bool
	MailboxesSeen int
}

// Option configures AddAccount.
type Option func(*options)

type options struct {
	dial         Dialer
	log          *slog.Logger
	allowPrivate bool
	resolver     egress.Resolver
}

// WithDialer overrides the IMAP dialer used for validation (default sync.Dial).
func WithDialer(d Dialer) Option { return func(o *options) { o.dial = d } }

// ErrHostNotPermitted is returned when the account's IMAP host resolves to an
// address the egress policy refuses. It wraps egress.ErrBlockedAddress.
//
// ⚠ IT IS A DISTINCT SENTINEL ON PURPOSE. The whole defect (#527) was that the
// caller learned about the network from the SHAPE of a dial error — refused
// versus refused-differently versus connected. A refusal that is
// indistinguishable from "connection refused" would re-open exactly that oracle
// in a new place, so this says "policy", never "the host did something".
var ErrHostNotPermitted = errors.New("onboarding: imap host is not a permitted egress target")

// WithAllowPrivateHosts permits account hosts on private, loopback and
// link-local addresses. Default FALSE.
//
// ⚠ THIS IS THE DECISION #527 SAID NOT TO MAKE SILENTLY, so it is made here in
// the open. The guard's policy is the image proxy's, which refuses RFC1918,
// loopback, link-local (including 169.254.169.254), IPv4-mapped, NAT64 and
// friends. For THIS deployment that costs nothing — Migadu is a public host —
// but a self-hosted IMAP server on the same LAN is a perfectly legitimate
// account target for someone else, and shipping the proxy's policy verbatim
// with no way out would make Mailroom unusable for them behind a confusing
// error.
//
// So: refuse by default, because the default should be the safe one and because
// an operator who needs a LAN server knows they need it; and name this option in
// the refusal, so the way out is discoverable from the error rather than from
// reading this file.
func WithAllowPrivateHosts(allow bool) Option {
	return func(o *options) { o.allowPrivate = allow }
}

// WithResolver overrides DNS for the egress check. Tests inject a fake; nil
// means the system resolver.
func WithResolver(r egress.Resolver) Option { return func(o *options) { o.resolver = r } }

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.log = l
		}
	}
}

// AddAccount seals the account's credentials, records the account, and
// validates it by dialing IMAP. On validation failure the account is kept and a
// non-nil error is returned (Result.AccountID is still set) so the caller can
// report it; whether sync is left ENABLED or DISABLED depends on the failure —
// see latchesSync — and Result.SyncEnabled says which happened.
func AddAccount(ctx context.Context, st AccountStore, keys crypto.KeyProvider, userID ulid.ULID, spec AccountSpec, opts ...Option) (Result, error) {
	if userID == (ulid.ULID{}) {
		return Result{}, errors.New("onboarding: add account: user id is required")
	}
	if err := spec.Validate(); err != nil {
		return Result{}, err
	}
	o := options{dial: sync.Dial, log: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}

	master, err := keys.MasterKey(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("onboarding: master key: %w", err)
	}
	defer crypto.Zero(master)

	ct, err := SealCredentials(master, userID, Credentials{
		IMAPUsername: spec.Username,
		IMAPPassword: spec.Password,
		SMTPUsername: firstNonEmpty(spec.SMTPUsername, spec.Username),
		SMTPPassword: firstNonEmpty(spec.SMTPPassword, spec.Password),
	})
	if err != nil {
		return Result{}, err
	}

	accountID, err := st.CreateAccount(ctx, userID, store.AccountRow{
		Name:             spec.Name,
		ProviderProfile:  spec.ProviderProfile,
		IMAPHost:         spec.IMAPHost,
		IMAPPort:         spec.IMAPPort,
		SMTPHost:         spec.SMTPHost,
		SMTPPort:         spec.SMTPPort,
		AuthKind:         "password",
		CredentialsCT:    ct,
		CredentialsDEK:   CredentialsSchemeV1,
		EnvelopeVer:      crypto.Version1,
		SyncEnabled:      true,
		WritebackEnabled: spec.WritebackEnabled,
	})
	if err != nil {
		return Result{}, fmt.Errorf("onboarding: add account %q: %w", spec.Name, err)
	}

	res := Result{AccountID: accountID, SyncEnabled: true}
	n, verr := validateIMAP(ctx, o, spec)
	if verr != nil {
		if !latchesSync(verr) {
			// Not the server saying no: a connection, DNS, TLS or timeout
			// failure, or a protocol surprise. Retrying is safe and is exactly
			// what the sync loop does, with per-account backoff, so the account
			// stays enabled and the blip costs one pass, not the account
			// (mailroom#435).
			o.log.Warn("account stored; IMAP validation failed but sync stays enabled — the sync loop will retry with backoff",
				"account_id", accountID.String(), "name", spec.Name, "kind", FailureKind(verr), "err", verr)
			return res, fmt.Errorf("onboarding: add account %q: IMAP validation failed (account stored, sync enabled, will be retried): %w", spec.Name, verr)
		}
		if derr := st.SetAccountSyncEnabled(ctx, userID, accountID, false); derr != nil {
			return res, fmt.Errorf("onboarding: add account %q: validation failed (%v); disabling sync also failed: %w", spec.Name, verr, derr)
		}
		res.SyncEnabled = false
		o.log.Warn("account stored but sync disabled: the server rejected the account",
			"account_id", accountID.String(), "name", spec.Name, "kind", FailureKind(verr), "err", verr)
		return res, fmt.Errorf("onboarding: add account %q: IMAP validation failed (account stored, sync disabled): %w", spec.Name, verr)
	}

	res.Validated = true
	res.MailboxesSeen = n
	o.log.Info("account added", "account_id", accountID.String(), "name", spec.Name, "mailboxes", n)
	return res, nil
}

// latchesSync reports whether a validation failure should switch the account's
// sync OFF. Only a rejection qualifies: the server refused the credentials
// (ErrorKindAuth), or the dialer refused the configuration (ErrorKindConfig).
// Retrying either cannot succeed, and retrying a wrong password can get the
// account locked at the provider. Everything else — a connection, DNS or TLS
// failure, a timeout, a protocol surprise, or an error the classifier could not
// place — keeps sync ENABLED and hands the account to the sync loop's
// per-account backoff.
//
// ⚠ Before mailroom#435 every failure latched, and the latch was permanent:
// boot reconciles declared accounts by name and never re-validates one it
// already has, so a certificate arriving a few hundred milliseconds late cost
// the account its sync until someone edited sync_enabled in SQL. The trigger
// was narrow (first boot of a given account name) but the failure mode was
// silence, which is the worst shape: accounts=0 at INFO, /readyz 200, and no
// mail.
func latchesSync(err error) bool {
	var se *sync.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Kind == sync.ErrorKindAuth || se.Kind == sync.ErrorKindConfig
}

// FailureKind names the classified kind of a validation failure ("auth",
// "transient", ...) for a log line or a CLI message, or "unclassified" when the
// error did not come from the sync package at all.
func FailureKind(err error) string {
	var se *sync.Error
	if errors.As(err, &se) {
		return se.Kind.String()
	}
	return "unclassified"
}

// TLSImplicit is the production IMAP transport (implicit TLS, 993/465), re-
// exported so callers that build an AccountSpec — like the JMAP Account/set
// path — can select it without importing internal/sync directly.
var TLSImplicit = sync.TLSImplicit

// ValidateAccount validates a spec by dialing IMAP and listing its mailboxes,
// returning the mailbox count ("folders detected") on success. It is the
// standalone validation + SPECIAL-USE-detection step AddAccount runs, factored
// out so the JMAP Account/set path can reuse the EXACT same check (dial, confirm
// the credentials work, detect folders) and surface a failure as a per-object
// /set error rather than a 500. dial defaults to the production sync.Dial when
// nil. It performs I/O and no database work, so a caller runs it OUTSIDE any
// database transaction.
func ValidateAccount(ctx context.Context, dial Dialer, spec AccountSpec, opts ...Option) (int, error) {
	if err := spec.Validate(); err != nil {
		return 0, err
	}
	o := options{dial: sync.Dial, log: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	// The explicit dial argument still wins, so every existing caller behaves
	// exactly as before; opts only add the egress policy and its seams.
	if dial != nil {
		o.dial = dial
	}
	return validateIMAP(ctx, o, spec)
}

// validateIMAP dials the account and lists its mailboxes, returning the count.
// A dial or list failure means the credentials/endpoint are unusable.
func validateIMAP(ctx context.Context, o options, spec AccountSpec) (int, error) {
	cfg := sync.Config{
		Host:               spec.IMAPHost,
		Port:               spec.IMAPPort,
		Username:           spec.Username,
		Password:           spec.Password,
		TLS:                spec.TLS,
		ServerName:         spec.ServerName,
		InsecureSkipVerify: spec.InsecureSkipVerify,
		// The SAME rule the pre-dial check below applies, carried onto the dial
		// itself (#625 §3): sync.Dial resolves once, refuses unless every
		// address passes, and connects to the checked literal. Validation and
		// every later sync pass are therefore held to one rule, through one
		// resolver seam.
		AllowPrivateHosts: o.allowPrivate,
		Resolver:          o.resolver,
	}
	if cfg.Port == 0 {
		cfg.Port = 993
	}

	// ⚠ THE GUARD, AND IT MUST COME BEFORE THE DIAL (#527).
	//
	// Until v1.70.x this function dialled whatever host:port the caller named and
	// handed back the raw dial error. That is an SSRF primitive and a port-scan
	// oracle: an authenticated caller distinguishes an open port from a closed
	// one by the SHAPE of the error, over everything the container can reach.
	// Capability-gating Account/set limits who can do it; it does not stop it.
	//
	// Refusing here rather than after the dial is the whole point — a refusal
	// that happened to be issued after connecting would still have leaked the
	// answer. And the refusal is its own sentinel so it cannot be mistaken for
	// "the host refused the connection", which would put the oracle back.
	//
	// Checked for BOTH endpoints (#625 §3): this function only ever dials IMAP,
	// but the spec's SMTP host is stored and later dialled by the submission
	// path, so an SMTP host on a private network is exactly the same oracle one
	// hop later — nothing about "we don't dial it here" makes it safe to skip.
	//
	// Adopted from internal/egress rather than restated: same resolve-then-check
	// rules, same address list, one implementation. On its own this check does
	// not close DNS rebinding (see egress.CheckHost): the dial re-resolves. What
	// closes it is that every account dial is now guarded too (#625 §3) — the
	// IMAP dial below and every later sync pass through sync.Dial's
	// egress.Dialer, and the SMTP submission dial through provider.SMTPSender's
	// — each of which checks the address it is about to connect to and dials
	// that literal. This check stays because it is the only place the SMTP host
	// is examined at validation time, and because it refuses before the account
	// is ever dialled with its credentials.
	if !o.allowPrivate {
		r := o.resolver
		if r == nil {
			r = net.DefaultResolver
		}
		if err := checkAccountHost(ctx, r, "imap", spec.IMAPHost); err != nil {
			return 0, err
		}
		if err := checkAccountHost(ctx, r, "smtp", spec.SMTPHost); err != nil {
			return 0, err
		}
	}

	sess, err := o.dial(ctx, cfg)
	if err != nil {
		if errors.Is(err, egress.ErrBlockedAddress) {
			// The host passed the check above and then resolved to a refused
			// address at the dial (rebinding in the gap between the two), and
			// the dial's own guard refused it before connecting. Reported as the
			// SAME policy refusal the pre-dial check gives — ErrHostNotPermitted,
			// naming the host and the switch — so the caller cannot tell the two
			// apart and neither can an attacker. The *sync.Error is deliberately
			// NOT kept in the chain: latchesSync would read its ErrorKindConfig
			// and disable sync, while the pre-dial refusal of the same condition
			// leaves it enabled.
			var se *sync.Error
			cause := err
			if errors.As(err, &se) {
				cause = se.Err
			}
			return 0, fmt.Errorf("%w: imap host %q at connect time (%w)", ErrHostNotPermitted, spec.IMAPHost, cause)
		}
		return 0, fmt.Errorf("dial imap: %w", err)
	}
	defer func() {
		_ = sess.Logout(ctx) // best-effort: politeness, not correctness
		_ = sess.Close()
	}()
	mbs, err := sess.ListMailboxes(ctx)
	if err != nil {
		return 0, fmt.Errorf("list mailboxes: %w", err)
	}
	return len(mbs), nil
}

// checkAccountHost applies the egress guard (see validateIMAP's comment for
// what it closes and does not) to one of the account's two endpoints and names
// WHICH host was refused — IMAP and SMTP are independent hosts that can
// disagree, and a refusal that did not say which one would send an operator
// fixing the wrong DNS record.
func checkAccountHost(ctx context.Context, r egress.Resolver, label, host string) error {
	cerr := egress.CheckHost(ctx, r, host)
	if cerr == nil {
		return nil
	}
	// ⚠ TWO FAILURES LIVE HERE AND THEY ARE NOT THE SAME ANSWER.
	//
	// "resolves to a blocked address" is a POLICY refusal: the operator asked
	// for something this instance will not reach, and the fix is the escape
	// hatch. "could not resolve at all" is not policy — the host may simply not
	// exist, or DNS may be down — and reporting it as "not permitted" would send
	// an operator hunting for a security setting when their typo is the problem.
	//
	// Both still refuse before dialling, because failing closed is the right
	// default for a guard; only the story told differs.
	if errors.Is(cerr, egress.ErrBlockedAddress) {
		return fmt.Errorf("%w: %s host %q (%w). If this instance must reach a mail server on "+
			"a private network, set MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS=1",
			ErrHostNotPermitted, label, host, cerr)
	}
	return fmt.Errorf("onboarding: cannot resolve %s host %q: %w", label, host, cerr)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
