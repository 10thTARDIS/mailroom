// SPDX-License-Identifier: AGPL-3.0-only

// Command mailroom is the Mailroom server.
//
// It wires the whole 0.5 instance together and runs it:
//
//   - config.Load — the environment is the contract; it refuses to start unless
//     exactly one auth path is usable (spec §16, internal/config).
//   - store.Open + store.ProvisionUser — BOOT-time get-or-create for the dev-auth
//     operator and the optional declared-accounts operator. Under OIDC, users are
//     provisioned at LOGIN instead (app-binding is the access gate, mailroom#98):
//     any identity that completes the flow is auto-provisioned from its token, and
//     the mailroom-admin group grants the admin role, re-evaluated each login.
//   - the accounts file — declare IMAP/SMTP accounts in JSON and reconcile them
//     idempotently at boot for the declared-accounts operator (only genuinely new
//     names are onboarded), together with the IDENTITIES each account may send as
//     (spec §5's server-side From validation).
//   - internal/api — the JMAP service, mounted BEHIND the auth middleware, which
//     is the only thing that ever injects a user id (spec §10).
//   - internal/compose — the compose/send path: it builds the message bytes,
//     validates the From, and schedules the submission as a durable job.
//   - internal/jobs — the durable-job dispatcher, a single global worker over the
//     whole job queue. It is what makes a send survive a restart (and what fires
//     an Undo-Send hold when it expires).
//   - internal/syncrunner — the per-user fan-out (startUserFanout): one supervisor
//     off the main context drives EVERY provisioned, enabled user's IMAP sync loop
//     and search indexer, and seeds each user's Trash purge. It picks up users
//     provisioned at runtime (a fresh OIDC login) without a restart, isolates one
//     user's failures from the others, and stops with the server on SIGINT/SIGTERM.
//     Each sync pass DRAINS THE WRITEBACK QUEUE for an account before syncing it,
//     which is what turns a local Email/set into an actual change on the IMAP server.
//   - internal/server — /healthz, /readyz, /metrics (always unauthenticated) in
//     front of the application handler at "/".
//
// Auth path selection mirrors internal/config exactly:
//
//	OIDC configured   -> auth.New + the sign-in page at /auth/login, the flow at
//	                     /auth/start, the provider's callback path, /auth/logout;
//	                     everything else runs behind Authenticator.Middleware.
//	otherwise         -> the INSECURE dev-auth escape hatch (auth.DevAuth): no
//	                     authentication at all, and a banner warning at startup.
//
// The login gate — what an UNAUTHENTICATED request may reach — is the route
// inventory in routes(): every route declares its side of the gate and the
// probes that prove it, and cmd/mailroom/gate_test.go asserts all of it against
// the real mux, for both auth paths. An anonymous visitor reaches the sign-in
// page and the health probes. Nothing else.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	stdsync "sync"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/blob"
	"mailroom/internal/changelog"
	"mailroom/internal/clamd"
	"mailroom/internal/compose"
	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/drawer"
	"mailroom/internal/egress"
	"mailroom/internal/ingest"
	"mailroom/internal/jobs"
	"mailroom/internal/keypub"
	"mailroom/internal/mailsync"
	"mailroom/internal/metrics"
	"mailroom/internal/ml"
	"mailroom/internal/oauthmail"
	"mailroom/internal/onboarding"
	"mailroom/internal/outofoffice"
	"mailroom/internal/pgpkeys"
	"mailroom/internal/provider"
	"mailroom/internal/purge"
	"mailroom/internal/rescan"
	"mailroom/internal/search"
	"mailroom/internal/search/tantivy"
	"mailroom/internal/server"
	"mailroom/internal/setup"
	"mailroom/internal/snooze"
	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"
	"mailroom/internal/syncrunner"
	"mailroom/internal/webpush"
	"mailroom/internal/wkd"
	"mailroom/internal/writeback"
	"mailroom/internal/xoauth2"
)

// version is stamped via -ldflags "-X main.version=..." at release time; the
// Dockerfile threads it in from the MAILROOM_VERSION build arg. The default is
// deliberately NOT a version number: an unstamped build (CI's image job, a
// local `docker build`) must be honestly labelled as not-a-release rather than
// claiming to be one. Go's own VCS stamping can't serve here — .dockerignore
// excludes .git, and a commit hash isn't the tag anyway (#134).
var version = "0.0.0-dev"

// Routes this command owns. The OIDC callback path is NOT fixed here — it is
// taken from MAILROOM_OIDC_REDIRECT_URL, so the route we serve is by
// construction the one registered with the provider.
//
//   - loginPath is the SIGN-IN PAGE (auth.Page): a real page, served to
//     anonymous visitors. It is where the gate sends an unauthenticated browser
//     and where a refused login lands, and it is the only HTML an anonymous
//     visitor can get out of this server.
//   - startPath is what the page's "Sign in" button hits: it, not loginPath,
//     bounces to the identity provider. Splitting them is what stops a refused
//     user from ping-ponging between Mailroom and a provider that is perfectly
//     happy to keep handing back the same rejected identity.
const (
	loginPath  = "/auth/login"
	startPath  = "/auth/start"
	logoutPath = "/auth/logout"
)

// blobPathInfo is the HKDF label for the blob store's per-user path-derivation
// key (an HMAC key, not an encryption key). Derived from the master key so the
// server needs no second secret. It MUST match cmd/mailroom-admin's constant:
// the two commands have to derive the same blob paths for the same user.
const blobPathInfo = "mailroom-blob-path-v1"

// syncStopTimeout bounds how long shutdown waits for an in-flight sync pass to
// notice the cancelled context and unwind.
const syncStopTimeout = 15 * time.Second

func main() {
	// The server is configured entirely from the environment and takes no
	// arguments, so --version is the one thing worth parsing: it lets an
	// operator read a built image's version without booting it, reaching a
	// database, or grepping logs — `docker run --rm <image> --version` (the
	// image's ENTRYPOINT is already /mailroom, so the binary must not be named
	// again) (#134). Anything else is still rejected by falling through to run(),
	// which ignores argv, so this stays additive.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version", "version":
			fmt.Println(version)
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mailroom: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := server.NewLogger(cfg.LogFormat, cfg.LogLevel)
	logger.Info("starting mailroom", "version", version, "addr", cfg.ListenAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("main: open store: %w", err)
	}
	defer st.Close()
	// The Trash dwell time before a soft-deleted message is hard-purged (ADR-009)
	// is configurable (MAILROOM_TRASH_RETENTION); thread it into the store, which
	// stamps it onto purge_after in the trash and reaper-vanish paths.
	st.SetTrashRetention(cfg.TrashRetention)
	// The instance-level ingest-only kill switch (ADR-028 §2, MAILROOM_WRITEBACK_
	// DISABLED); thread it into the store, which ANDs it with every account's own
	// writeback_enabled column before ever constructing an IMAPWriter.
	st.SetWritebackDisabled(cfg.WritebackDisabled)
	if cfg.WritebackDisabled {
		// A silent global kill switch is indistinguishable from a broken writeback
		// subsystem, and this project's rule is that a suppressed capability must
		// be observable — so this is loud rather than merely true.
		logger.Warn("writeback is DISABLED instance-wide ("+config.EnvWritebackDisabled+"); "+
			"no account can mutate its remote mailbox until this is unset", "instance_writeback_disabled", true)
	}

	keys := crypto.NewFileKeyProvider(cfg.MasterKeyFile)
	m := metrics.New()

	// --- Mail-provider OAuth (#52, ADR-046) ----------------------------------
	// One token Manager for the whole process (sync, send, connect). Empty
	// wiring when no provider is configured.
	oa, err := buildOAuth(st, keys, cfg, logger)
	if err != nil {
		return err
	}

	// --- Operator ------------------------------------------------------------
	// Under OIDC, users are provisioned at LOGIN now (provisioningResolver), not
	// here — this boot step exists for the dev-auth escape hatch, which needs a
	// fixed identity to inject and therefore a user that already exists, and for
	// the optional declared-accounts operator (MAILROOM_OPERATOR_SUBJECT) whose
	// accounts file is reconciled below. A zero id means no boot operator is
	// declared, which is the normal OIDC case: the sync/index/purge fan-out then
	// drives whichever users log in and get provisioned.
	operatorID, err := provisionOperator(ctx, st, cfg, logger)
	if err != nil {
		return err
	}

	// --- Declared accounts ---------------------------------------------------
	if operatorID != (ulid.ULID{}) {
		if err := reconcileAccounts(ctx, st, keys, cfg, operatorID, logger); err != nil {
			return err
		}
	}

	// --- Compose/send --------------------------------------------------------
	// The blob store is what compose needs to author message bytes; without it
	// the instance still serves every read method, it just cannot compose or
	// send, and Email/set create / EmailSubmission/set say so plainly.
	blobs, blobErr := openBlobStore(ctx, keys, cfg.BlobRoot)
	if blobErr != nil {
		logger.Error("compose/send and sync are disabled: blob store unavailable", "err", blobErr)
	}
	var composer *compose.Composer
	if blobErr == nil {
		composer = compose.NewComposer(blobs, &keyResolver{keys: keys}, cfg.UndoSendWindow)
		// Outgoing signing (#47 increment 5, ADR-024). A nil signer — no socket
		// configured — means mail goes out unsigned, which is the default and
		// not a degraded mode. It is logged either way so that "is this instance
		// signing?" is answerable from the boot log rather than by sending a
		// message and inspecting it.
		if signer := newCryptoSigner(cfg.PGPSocket, logger); signer != nil {
			composer = composer.WithSigner(signer).WithEncrypter(signer)
			logger.Info("outgoing mail will be signed for identities with a bound PGP key",
				"crypto_socket", cfg.PGPSocket)
		} else {
			logger.Info("outgoing mail will NOT be signed: no crypto socket configured")
			reportUnsignedDowngrade(ctx, st, logger)
		}
	}

	// The per-user priority-model registry (#35) backs both the ingest Scorer and
	// the Triage control methods (#34). ONE instance is built here and shared, so
	// the controls and the scorer never diverge on which model is live (and share
	// the one derived model-DEK path). It needs the blob store; a degraded instance
	// with none has a nil registry — the Triage controls then fail cleanly.
	var registry *ml.Registry
	if blobErr == nil {
		registry = ml.NewRegistry(st, blobs, keys)
	}

	// --- Search --------------------------------------------------------------
	// The index is a disposable CACHE (spec §7): it is rebuilt from Postgres, is
	// never a source of truth, and its absence is never fatal. A Mailroom whose
	// index is off or broken still serves all of a user's mail — Email/query just
	// rejects text filters with unsupportedFilter rather than quietly answering a
	// superset of what was asked for.
	searchSvc, indexer := openSearch(ctx, st, blobs, keys, cfg, blobErr == nil, m, logger)

	// ⚠ ONE scanner client, shared by ingest, re-scan AND the health panel.
	// Constructing it more than once also logs "attachment scanning is ON" more
	// than once, which reads like a contradiction in a boot log rather than a
	// duplicate. Built here because the HTTP handler below is the first thing
	// that needs it.
	scanner := newScanner(cfg, logger)
	if scanner != nil {
		go watchScannerVersion(ctx, scanner, scannerVersionSink(m), scannerVersionInterval, logger)
	}

	// Out of office (#50): ONE manager, shared by OutOfOffice/set and the
	// timer job, so its per-account lock serialises a save against a timer
	// firing for the same account. The ManageSieve dial obeys the same
	// private-host switch as the account's IMAP and SMTP dials.
	oof := outofoffice.NewManager(st, outofoffice.KeyCredentials{Keys: keys},
		outofoffice.WithAllowPrivateHosts(cfg.AllowPrivateAccountHosts), outofoffice.WithLogger(logger))

	// --- HTTP ----------------------------------------------------------------
	appHandler, apiSvc, err := buildAppHandler(ctx, st, cfg, operatorID, composer, searchSvc, keys, registry, scanner, oof, oa, m, logger)
	if err == nil && apiSvc != nil {
		// Unanswered API key approval requests expire after 24 hours (#49,
		// ADR-042 §7). They also expire on read and on decision; this makes the
		// audit and every banner say so without waiting for a read.
		go apiSvc.RunPendingSweep(ctx, pendingSweepInterval, logger)
	}
	if err != nil {
		return err
	}
	// Expired pending OAuth connects are deleted hourly (no-op without OAuth).
	runOAuthFlowSweep(ctx, st, oa)
	// Revoked sessions older than 7 days and sessions unseen for 45 are deleted
	// daily (#937, ADR-048 section 7), with the count per reason logged.
	go runSessionSweep(ctx, st, sessionSweepInterval, logger)

	// Readiness is "can we reach the database": SystemTx opens a real
	// transaction on the pool with no user identity attached (the
	// identity-less door, CLAUDE.md database-access rules), so a
	// successful no-op round-trip is a valid connectivity probe without
	// touching any user's rows.
	ready := func(ctx context.Context) error {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return st.SystemTx(pingCtx, func(tx *store.SystemTx) error { return nil })
	}

	srv := server.New(cfg.ListenAddr, logger, m.Handler(), ready, server.WithAppHandler(appHandler))

	// --- Durable jobs --------------------------------------------------------
	// The dispatcher is what makes a scheduled send actually happen — including
	// one whose Undo-Send window expired while the process was down (it runs a
	// catch-up pass on boot, spec §16). Without it, EmailSubmission/set would
	// queue mail nobody ever sends. It is a SINGLE global worker over the whole
	// job queue (app.dequeue_due_jobs is cross-user), so it is NOT fanned out;
	// only the per-user SEEDING of the recurring Trash purge is (startUserFanout).
	//
	// sentNudge links a COMPLETED send to the writeback drain (#691): the send
	// job queues the Sent APPEND and the Drafts EXPUNGE, and without a nudge
	// they wait for the account's next pass. The runner it nudges is built by
	// startUserFanout below, after the dispatcher is already running.
	sentNudge := &writebackNudgeHook{}
	jobsDone := startDispatcher(ctx, st, blobs, keys, composer, submissionSender(cfg), scanner, blobErr == nil, cfg.PurgeInterval, cfg.ChangeLogRetention, sentNudge, oof, oa, apiSvc, m, logger)

	// --- Per-user fan-out: sync + indexer + Trash-purge seeding --------------
	// One supervisor off the main context drives EVERY provisioned, enabled user
	// (mailroom#98): it lists active users, starts each user's sync loop, search
	// indexer, and first purge seed, and picks up users provisioned at runtime
	// (a fresh OIDC login) without a restart. SIGINT/SIGTERM stops it and the
	// HTTP server together; per-user and per-account failures are isolated, so a
	// bad account or a slow user degrades only that user, never the process.
	fanoutDone := startUserFanout(ctx, st, blobs, keys, registry, cfg, indexer, composer, scanner, apiSvc, sentNudge, oa, m, logger)

	srvErr := srv.Run(ctx)

	// The server has drained; give the background loops a bounded moment to
	// unwind whatever they are in the middle of.
	select {
	case <-fanoutDone:
	case <-time.After(syncStopTimeout):
		logger.Warn("per-user fan-out did not stop within the shutdown timeout; exiting anyway",
			"timeout", syncStopTimeout.String())
	}
	select {
	case <-jobsDone:
	case <-time.After(syncStopTimeout):
		logger.Warn("job dispatcher did not stop within the shutdown timeout; exiting anyway",
			"timeout", syncStopTimeout.String())
	}

	if srvErr != nil {
		return fmt.Errorf("main: server: %w", srvErr)
	}
	logger.Info("mailroom shut down cleanly")
	return nil
}

// --- Operator provisioning ---------------------------------------------------

// operatorIdentity resolves the (subject, email) of the single 0.5 operator from
// config. Under OIDC that is MAILROOM_OPERATOR_SUBJECT/_EMAIL; under the
// dev-auth escape hatch the dev-auth subject/email IS the operator (config.go).
//
// An empty subject is legal ONLY under OIDC and means "no operator declared":
// nobody is provisioned at boot and the declared accounts file is not applied,
// but every user and account ALREADY in the database is served and synced as
// usual — the sync fan-out lists users from the database, not from this. A
// subject WITHOUT an email is a hard error rather than a
// synthesized placeholder — app.users.email is NOT NULL UNIQUE and surfaces in
// the JMAP session, so guessing one would bake a fiction into the user's
// identity. It is a pure function so the selection logic is unit-testable
// without a database, a network, or a provider.
func operatorIdentity(cfg *config.Config) (subject, email string, err error) {
	switch {
	case cfg.DevAuthConfigured():
		subject, email = cfg.DevAuthSubject, cfg.DevAuthEmail
		if email == "" {
			return "", "", fmt.Errorf(
				"main: %s is set, so %s is required to provision the operator",
				config.EnvDevAuthSubject, config.EnvDevAuthEmail)
		}
		return subject, email, nil
	case cfg.OIDCConfigured():
		subject, email = cfg.OperatorSubject, cfg.OperatorEmail
		if subject == "" {
			return "", "", nil // legal: no operator declared under OIDC
		}
		if email == "" {
			return "", "", fmt.Errorf(
				"main: %s is set, so %s is required to provision the operator",
				config.EnvOperatorSubject, config.EnvOperatorEmail)
		}
		return subject, email, nil
	default:
		// config.Load's validateAuth guarantees one of the two paths above.
		return "", "", errors.New("main: no auth path configured (config validation should have caught this)")
	}
}

// provisionOperator get-or-creates the configured operator and returns its id
// (zero if no operator is declared). Idempotent: the same subject always
// resolves to the same id, boot after boot.
func provisionOperator(ctx context.Context, st *store.Store, cfg *config.Config, logger *slog.Logger) (ulid.ULID, error) {
	subject, email, err := operatorIdentity(cfg)
	if err != nil {
		return ulid.ULID{}, err
	}
	if subject == "" {
		// ⚠ SAY WHAT IS SKIPPED, NOT MORE. This line used to end "and no accounts
		// will be synced", which was false: a running instance logged it on
		// every boot while syncing its one account normally, and a WARN that is
		// visibly untrue teaches the operator to skim the WARNs.
		logger.Warn("no operator configured: no user is provisioned at boot and the declared accounts file is not applied; "+
			"users and accounts already in the database are served and synced as usual",
			"hint", config.EnvOperatorSubject+" selects the operator the declared accounts belong to")
		return ulid.ULID{}, nil
	}

	id, err := st.ProvisionUser(ctx, subject, email)
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("main: provision operator: %w", err)
	}
	logger.Info("operator ready", "user_id", id.String(), "subject", subject, "email", email)
	return id, nil
}

// --- Declared accounts -------------------------------------------------------

// reconcileAccounts brings the operator's stored accounts in line with the
// declared accounts file. It is a RECONCILE, not a create: an account whose name
// already exists is left exactly as it is (its stored credentials are not
// rewritten and its sync_enabled flag is not touched), so restarting the server
// never duplicates accounts and never clobbers an operator's out-of-band fixes.
// Only names that are absent are onboarded.
//
// This is also the decision for config.Account.WritebackEnabled (ADR-028):
// it is honored ONLY at creation, via accountSpec -> onboarding.AddAccount ->
// store.AccountRow.WritebackEnabled. An existing account's mode is never
// touched by a restart, same as sync_enabled above — deliberately, not an
// oversight: flipping a LIVE account to shadowed from a config-file edit,
// with no matching call to SuppressAccountIntents in the same transaction,
// would leave its already-`pending` intents to fire on the next drain exactly
// as before, reintroducing the front-edge hole this mode exists to close.
// Changing a live account's mode goes through mailroom-admin / Account/set,
// which do that suppression.
//
// Onboarding an account dials IMAP to validate it. A validation failure is NOT
// fatal to boot: onboarding.AddAccount keeps the row and we log a warning. What
// it does to sync depends on who failed (mailroom#435): a rejection by the
// server leaves sync DISABLED — fix the credentials, then `mailroom-admin
// enable-account`, because this reconcile will never re-validate a name it
// already has — while any other failure leaves sync ENABLED for the sync loop's
// backoff to retry, the same posture the loop takes toward a broken account at
// runtime. A malformed accounts FILE, by contrast, is fatal: the operator wrote
// something we cannot honor.
func reconcileAccounts(ctx context.Context, st *store.Store, keys crypto.KeyProvider, cfg *config.Config, operatorID ulid.ULID, logger *slog.Logger) error {
	declared, err := config.LoadAccountsFile(cfg.AccountsFile)
	if err != nil {
		return err
	}
	if len(declared) == 0 {
		return nil
	}

	var existing []store.Account
	if err := st.UserTx(ctx, operatorID, func(tx *store.UserTx) error {
		var e error
		existing, e = tx.ListAccounts(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("main: list accounts: %w", err)
	}
	have := make(map[string]struct{}, len(existing))
	for _, a := range existing {
		have[a.Name] = struct{}{}
	}

	adapter := onboarding.NewStoreAdapter(st)
	for _, a := range declared {
		if _, ok := have[a.Name]; ok {
			logger.Info("account already configured; leaving it alone", "name", a.Name)
			continue
		}
		logger.Info("onboarding declared account", "name", a.Name,
			"imap", fmt.Sprintf("%s:%d", a.IMAPHost, a.IMAPPort),
			"smtp", fmt.Sprintf("%s:%d", a.SMTPHost, a.SMTPPort))

		res, err := onboarding.AddAccount(ctx, adapter, keys, operatorID, accountSpec(a),
			onboarding.WithLogger(logger),
			// Declared accounts get the same egress policy as Account/set (#527):
			// one guard, one switch, no path that is quietly exempt.
			onboarding.WithAllowPrivateHosts(cfg.AllowPrivateAccountHosts))
		if err != nil {
			// A boot must not die on a bad password. AddAccount stored the row
			// whenever res.AccountID is set; whether it left sync on depends on
			// who failed (mailroom#435), and the log has to say which, because
			// the two need different things from the operator: one fixes
			// itself, the other never will until enable-account is run.
			switch {
			case res.AccountID == (ulid.ULID{}):
				logger.Error("declared account could not be stored", "name", a.Name, "err", err)
			case res.SyncEnabled:
				logger.Warn("declared account could not be validated; stored with sync ENABLED, the sync loop will retry",
					"name", a.Name, "account_id", res.AccountID.String(), "err", err)
			default:
				logger.Warn("declared account was rejected by the server; stored with sync DISABLED — fix the credentials, then run mailroom-admin enable-account",
					"name", a.Name, "account_id", res.AccountID.String(), "err", err)
			}
			continue
		}
		logger.Info("declared account onboarded", "name", a.Name,
			"account_id", res.AccountID.String(), "mailboxes", res.MailboxesSeen)
	}

	// Identities are reconciled for EVERY declared account, including ones that
	// already existed: an operator who adds a wildcard domain to the file and
	// restarts must get that send-as right, without having to delete and re-add
	// the account.
	return reconcileIdentities(ctx, st, declared, operatorID, logger)
}

// reconcileIdentities brings each declared account's send-as rights in line with
// the accounts file (spec §5). It is additive and idempotent: an identity that
// already exists is left alone, and one that is no longer declared is NOT
// removed — revoking a send-as right is a deliberate, destructive act (it can
// break references from past submissions), so it stays with the operator CLI
// rather than being a side effect of editing a file.
//
// This is where server-side From validation gets its ground truth. Every address
// the user may send as is here, or it is nowhere:
//
//	identities:       exact addresses (kind = fixed)
//	wildcard_domains: every address at a domain (kind = domain_wildcard) — the
//	                  catchall model, where there is no finite alias list to
//	                  enumerate and the reply-from address is DERIVED from the
//	                  message that arrived (spec §5).
//
// An account that declares neither gets one fixed identity for its username,
// which is the right default for a plain mailbox and means a fresh instance can
// send without any extra configuration.
func reconcileIdentities(ctx context.Context, st *store.Store, declared []config.Account, operatorID ulid.ULID, logger *slog.Logger) error {
	accounts := map[string]store.Account{}
	if err := st.UserTx(ctx, operatorID, func(tx *store.UserTx) error {
		existing, err := tx.ListAccounts(ctx)
		if err != nil {
			return err
		}
		for _, a := range existing {
			accounts[a.Name] = a
		}
		return nil
	}); err != nil {
		return fmt.Errorf("main: list accounts: %w", err)
	}

	for _, d := range declared {
		acct, ok := accounts[d.Name]
		if !ok {
			continue // onboarding failed for this account; nothing to attach to
		}
		fixed := d.Identities
		if len(fixed) == 0 && len(d.WildcardDomains) == 0 && strings.Contains(d.Username, "@") {
			fixed = []string{strings.ToLower(d.Username)}
		}

		err := st.UserTx(ctx, operatorID, func(tx *store.UserTx) error {
			for _, addr := range fixed {
				if err := ensureIdentity(ctx, tx, store.IdentityRow{
					AccountID: acct.ID, Kind: store.IdentityFixed,
					Address: addr, Enabled: true,
				}, logger, "address", addr); err != nil {
					return err
				}
			}
			for _, domain := range d.WildcardDomains {
				if err := ensureIdentity(ctx, tx, store.IdentityRow{
					AccountID: acct.ID, Kind: store.IdentityWildcard,
					Domain: domain, Enabled: true,
				}, logger, "wildcard_domain", domain); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("main: reconcile identities for account %q: %w", d.Name, err)
		}
	}
	return nil
}

// ensureIdentity creates one identity, treating "already exists" as success.
func ensureIdentity(ctx context.Context, tx *store.UserTx, row store.IdentityRow, logger *slog.Logger, kind, value string) error {
	id, err := tx.CreateIdentity(ctx, row)
	switch {
	case errors.Is(err, store.ErrIdentityExists):
		return nil
	case err != nil:
		return err
	}
	logger.Info("send-as identity provisioned", kind, value, "identity_id", id.String())
	return nil
}

// accountSpec converts a declared account into an onboarding spec. Real accounts
// always use implicit TLS (993/465); the plaintext/STARTTLS modes exist only for
// the test harness and are deliberately not reachable from the accounts file.
//
// WritebackEnabled is forwarded as-is (nil stays nil): this is what lets the
// real account be declared already shadowed (ADR-028) so it never syncs
// live even for the seconds between creation and an operator flipping a flag.
// It is honored ONLY at create — reconcileAccounts never revisits an account
// that already exists (same as every other field here, sync_enabled included),
// so editing writeback_enabled in the file after onboarding has no effect;
// changing a live account's mode goes through mailroom-admin / Account/set.
func accountSpec(a config.Account) onboarding.AccountSpec {
	return onboarding.AccountSpec{
		Name:             a.Name,
		ProviderProfile:  a.ProviderProfile,
		IMAPHost:         a.IMAPHost,
		IMAPPort:         a.IMAPPort,
		SMTPHost:         a.SMTPHost,
		SMTPPort:         a.SMTPPort,
		Username:         a.Username,
		Password:         a.Password.Reveal(),
		SMTPUsername:     a.SMTPUsername,
		SMTPPassword:     a.SMTPPassword.Reveal(),
		WritebackEnabled: a.WritebackEnabled,
		TLS:              imapsync.TLSImplicit,
	}
}

// --- HTTP wiring: the gate ---------------------------------------------------

// wkdLookup adapts the store to keypub.Lookup.
//
// It exists so internal/keypub never imports internal/store: the package that
// serves anonymous requests should not be able to reach a general database
// handle, only the one narrow question it is allowed to ask.
type wkdLookup struct{ st *store.Store }

func (l wkdLookup) LookupWKDKey(ctx context.Context, domain, hash string) ([]byte, string, bool, error) {
	k, found, err := l.st.LookupWKDKey(ctx, domain, hash)
	if err != nil || !found {
		return nil, "", false, err
	}
	return k.PublicKey, k.Fingerprint, true, nil
}

// LookupWKDKeyPage satisfies keypub.PageLookup. Same query, same published
// rows; the key page needs the address to show WHOSE key this is, and the
// fingerprint to show in a form a human can read aloud.
//
// An address whose only published keys are revoked has no page (ADR-040): the
// WKD endpoint keeps serving the revocations so clients learn of them, but a
// page inviting a human to copy a fingerprint would be offering a dead key.
func (l wkdLookup) LookupWKDKeyPage(ctx context.Context, domain, hash string) (string, string, bool, error) {
	k, found, err := l.st.LookupWKDKey(ctx, domain, hash)
	if err != nil || !found || k.RevocationsOnly {
		return "", "", false, err
	}
	return k.Address, k.Fingerprint, true, nil
}

// gate is which side of the login gate a route sits on. EVERY route on the app
// mux carries one, and routes() is the only place a route can be born — so
// adding a route to Mailroom is, by construction, choosing one of these three
// answers out loud. A route that "just works" without picking a side is the way
// an open surface gets shipped by accident; here it does not compile.
type gate int

const (
	// gateOpen: served to anyone, with no session. This is the sign-in page and
	// the OIDC flow endpoints — and NOTHING else. Every one of them must be
	// safe to hand to a hostile anonymous caller.
	gateOpen gate = iota

	// gateAPI: 401 Unauthorized without a session. A 302 to a login page is
	// meaningless to a programmatic client — and to an <img> pointing at an
	// attachment — and the user id must come from a verified session, never
	// from anything request-driven (spec §10). Every JMAP route, including
	// /jmap/download and /jmap/upload, which carry mail content.
	gateAPI

	// gateBrowser: 302 to the sign-in page. The web client — a human at a
	// browser gets a door, not a status code. The app shell is NEVER served to
	// an anonymous visitor.
	gateBrowser

	// gatePublic: served to anyone, WITH the hardening headers. This is the
	// public key surface (WKD and the shareable key page, #48) — content that
	// is public BY DESIGN, unlike everything behind the gate.
	//
	// It exists because gateOpen was the wrong tool for it. gateOpen applies NO
	// wrapper at all, which is right for the sign-in page — that page pins its
	// own stricter CSP by hash, and wrapping it would WEAKEN it. But a key page
	// is HTML rendered from user data on the same origin as the mail client, so
	// it wants frame-ancestors, nosniff and a cache policy. Reusing gateOpen
	// would have shipped it with none of them, and no test would have noticed,
	// because the header test only probes the browser paths.
	gatePublic
)

// probe is a concrete unauthenticated request the gate test drives at a route.
// A route with no probes fails the test: "I added a route" and "I said what an
// anonymous request gets from it" are the same act.
type probe struct {
	method string
	url    string
}

func get(u string) probe  { return probe{http.MethodGet, u} }
func post(u string) probe { return probe{http.MethodPost, u} }

// route is one entry in the app mux's inventory.
type route struct {
	// pattern is the http.ServeMux pattern (it may carry a method).
	pattern string
	gate    gate
	handler http.Handler
	// probes are concrete URLs an anonymous client can hit that land on this
	// route. At least one is required (gate_test.go enforces it).
	probes []probe
}

// routes is the app mux's complete inventory: pattern, gate, handler, probes.
// buildAppHandler registers exactly this and nothing else, and gate_test.go
// asserts every probe's UNAUTHENTICATED behavior matches its declared gate — on
// the real mux, for both auth paths.
//
// openRoutes differ per auth path (OIDC has a login flow; dev-auth has no auth
// at all), so they are supplied by the caller. Everything else is common: the
// JMAP surface, and the catch-all web route that swallows every path no earlier
// pattern claimed — which is what makes "no route falls through to the SPA
// unauthenticated" a property of the mux rather than a hope.
func routes(jmap, web, oauthCB http.Handler, open []route, keys *keypub.Service) []route {
	rs := make([]route, 0, len(open)+len(api.JMAPPaths())+4)
	rs = append(rs, open...)

	// The mail-provider OAuth callback (#52, ADR-046 §3). A BROWSER route:
	// the provider sends the user's browser here with a top-level GET, the
	// SameSite=Lax session cookie rides along, and the handler needs the
	// session user to bind the pending flow to — so it sits behind the
	// browser gate, never the open one. Registered in every build (the
	// handler itself answers "not configured"), so the gate test always
	// covers it. An anonymous hit is bounced to sign-in WITHOUT its query
	// (auth.WithUnauthenticatedRedirect drops a query carrying code/state),
	// so an authorization code never travels on into return_to.
	if oauthCB != nil {
		rs = append(rs, route{
			pattern: "GET " + oauthmail.CallbackPathPrefix + "{provider}",
			gate:    gateBrowser,
			handler: oauthCB,
			probes: []probe{
				get(oauthmail.CallbackPathPrefix + "google?code=4%2F0sentinel-code&state=sentinel-state"),
				get(oauthmail.CallbackPathPrefix + "microsoft?error=access_denied&state=sentinel-state"),
				get(oauthmail.CallbackPathPrefix + "google"),
			},
		})
	}

	// The PUBLIC key surface (#48, spec §9). These are the only routes in
	// Mailroom that answer a stranger with data derived from a user's rows, so
	// they are the ones to read carefully.
	//
	// They are registered whenever the service exists, INDEPENDENTLY OF
	// CONFIGURATION — a service with no domain answers 404 itself. If the
	// routes came and went with configuration, the gate tests would have to
	// become conditional too, and a test that only runs in some configurations
	// is a test that stops running.
	if keys != nil {
		rs = append(rs,
			route{
				pattern: "GET /.well-known/openpgpkey/hu/{hash}",
				gate:    gatePublic,
				handler: keys.HUHandler(),
				probes: []probe{
					// A PUBLISHED key. This one must actually answer, or every
					// assertion below passes for the uninteresting reason that
					// the route serves nobody anything.
					get("/.well-known/openpgpkey/hu/" + wkd.HashLocalPart("joe.doe")),
					// A well-formed lookup for a key nobody published. 404, and
					// indistinguishable from an address that does not exist —
					// that indistinguishability is the anti-enumeration property.
					// ⚠ Must NOT hash to the published one; these were once the
					// same URL written two ways.
					get("/.well-known/openpgpkey/hu/" + wkd.HashLocalPart("nobody-here")),
					// Malformed names: refused on shape, before any query runs.
					get("/.well-known/openpgpkey/hu/short"),
					get("/.well-known/openpgpkey/hu/" + strings.Repeat("y", 200)),
				},
			},
			route{
				// The shareable key page. Same hash as WKD, so one identifier
				// addresses both the machine-readable key and the page a person
				// can send someone to check a fingerprint against.
				pattern: "GET /key/{hash}",
				gate:    gatePublic,
				handler: keys.PageHandler(),
				probes: []probe{
					get("/key/" + wkd.HashLocalPart("joe.doe")),  // published: renders
					get("/key/iy9q119eutrkn8s1mk4r39qejnbu3n5q"), // unpublished: 404
					get("/key/short"),
					get("/key/" + strings.Repeat("y", 200)),
				},
			},
			route{
				pattern: "GET /.well-known/openpgpkey/policy",
				gate:    gatePublic,
				handler: keys.PolicyHandler(),
				probes:  []probe{get("/.well-known/openpgpkey/policy")},
			},
		)
	}

	// Straight from internal/api rather than restated here: a route the API
	// serves but this list forgets is a 404 that looks exactly like a missing
	// feature — and a route the API ADDS that this list does not gate would be a
	// hole. jmapProbes has no default case, so growing api.JMAPPaths() without
	// saying what an anonymous request gets from the new route is a loud failure,
	// not a quiet default.
	for _, p := range api.JMAPPaths() {
		rs = append(rs, route{pattern: p, gate: gateAPI, handler: jmap, probes: jmapProbes(p)})
	}

	rs = append(rs, route{
		pattern: "/",
		gate:    gateBrowser,
		handler: web,
		probes: []probe{
			get("/"),                        // the app shell
			get("/index.html"),              // ...and its file
			get("/assets/index-abc123.js"),  // every hashed bundle asset
			get("/assets/index-abc123.css"), // ...
			get("/manifest.webmanifest"),    // the PWA manifest
			get("/favicon.ico"),
			get("/inbox/01J0000000000000000000000"), // a client-side route
			get("/auth/"),                           // NOT the login page: no prefix match
			get("/auth/nope"),
			get("/jmapster"),        // NOT /jmap: an exact pattern, not a prefix
			get("/.well-known/foo"), // NOT the JMAP well-known
			// A well-known path that is not a REAL WKD name: {hash} does not
			// match an empty segment, so this is just another unmatched path
			// and gets the sign-in redirect like any other. Probed here rather
			// than on the WKD route because here is where it actually lands —
			// the gate test caught it being claimed by the wrong one.
			get("/.well-known/openpgpkey/hu/"),
			get("/.well-known/openpgpkey/"),
			post("/anything"),
		},
	})
	return rs
}

// jmapProbes is the concrete unauthenticated request(s) for each JMAP route.
// There is deliberately NO default case: add a path to api.JMAPPaths() and this
// panics in the test with instructions, rather than the new route quietly
// inheriting someone else's expectations.
func jmapProbes(path string) []probe {
	switch path {
	case "/jmap":
		return []probe{post("/jmap"), get("/jmap")}
	case "/jmap/session":
		return []probe{get("/jmap/session")}
	case "/.well-known/jmap":
		return []probe{get("/.well-known/jmap")}
	case "/jmap/download/":
		// Mail content and attachments. An <img src> or a shared link is the
		// realistic anonymous request here, so probe the shape a browser sends.
		return []probe{get("/jmap/download/01J0000000000000000000000/blob-1/invoice.pdf")}
	case "/jmap/preview/":
		// The #131 attachment preview. Same anonymous shape as download — a stale
		// frame src is the realistic hit — and the same side of the gate.
		return []probe{get("/jmap/preview/01J0000000000000000000000/blob-1/invoice.pdf")}
	case "/jmap/proxy/":
		// The image proxy (ADR-026). PROTECTED, like download and preview: it
		// serves bytes derived from one user's mail, and the realistic anonymous
		// request is a stale <img src> in a cached page. An unauthenticated hit
		// must not reach the handler at all — reaching it would mean an
		// unauthenticated request could make this server fetch a URL.
		return []probe{get("/jmap/proxy/01J0000000000000000000000/01J0000000000000000000001/" +
			"0123456789abcdef0123456789abcdef")}
	case "/jmap/upload/":
		return []probe{post("/jmap/upload/01J0000000000000000000000/")}
	case "/jmap/eventsource/":
		return []probe{get("/jmap/eventsource/?types=*&closeafter=no&ping=0")}
	case api.MCPPath:
		// MCP tools (#49, ADR-044): a key-only surface, so an anonymous POST
		// (the transport's only method) and GET (an SSE probe) never reach it.
		return []probe{post(api.MCPPath), get(api.MCPPath)}
	default:
		panic("main: api.JMAPPaths() grew " + path + " — add its unauthenticated probe " +
			"to jmapProbes and decide, explicitly, which side of the login gate it is on")
	}
}

// apiRateLimitConfig converts the operator's MAILROOM_RATE_LIMIT_RPS /
// MAILROOM_RATE_LIMIT_BURST settings (config.Load) into the api package's
// RateLimitConfig. It is a pure function so the wiring in buildAppHandler is
// testable without booting the API: main_test.go asserts a configured value
// reaches this struct unchanged, including the all-zero "operator disabled
// limiting" case.
func apiRateLimitConfig(cfg *config.Config) api.RateLimitConfig {
	return api.RateLimitConfig{PerSecond: cfg.RateLimitPerSecond, Burst: cfg.RateLimitBurst}
}

// buildAppHandler assembles everything served at "/": the JMAP API, the optional
// static web client, the sign-in page, and — in front of the first two — the
// selected auth path.
//
// It is a thin lid on mountApp, which is where the gate actually lives. The
// split is so the gate test can drive the REAL mux, the REAL middleware and the
// REAL route inventory while substituting the two PROTECTED leaf handlers for
// sentinels that fail the test if they are ever reached — which is a stronger
// statement than "the status code was 401", and is the only way to exercise the
// dev-auth stack (where nothing rejects the request, so the leaf really does
// run) without a database.
func buildAppHandler(ctx context.Context, st *store.Store, cfg *config.Config, operatorID ulid.ULID, composer *compose.Composer, searchSvc *search.Service, keys crypto.KeyProvider, registry *ml.Registry, scanner *clamd.Client, oof *outofoffice.Manager, oa oauthWiring, m *metrics.Metrics, logger *slog.Logger) (http.Handler, *api.API, error) {
	// The run context bounds the long-lived handlers: an EventSource stream holds
	// its HTTP request open indefinitely, so without this the graceful shutdown
	// would wait out its whole timeout on every connected client. Cancelling ctx
	// (SIGINT/SIGTERM) closes them all.
	opts := []api.Option{api.WithBaseContext(ctx)}
	if composer != nil {
		opts = append(opts, api.WithComposer(composer))
	}
	// The in-UI account surface (Account/set, #98/#45) seals mail-account
	// credentials server-side through the SAME key path onboarding uses; the
	// production IMAP dialer defaults to sync.Dial inside the API.
	opts = append(opts, api.WithAccountSecrets(&keyResolver{keys: keys}))
	opts = append(opts, api.WithAllowPrivateAccountHosts(cfg.AllowPrivateAccountHosts))
	// Connect by signing in at the provider (#52, ADR-046): the two methods,
	// the oauthProviders capability field, and Forget on account deletion.
	// Left unwired (not a typed nil) when no provider is configured.
	if oa.connector != nil {
		opts = append(opts, api.WithOAuthConnect(oa.connector))
	}
	if oa.ops != nil {
		opts = append(opts, api.WithOAuthOperatorStatus(oa.ops))
	}
	// The budget Account/getHealth's Gmail import progress estimates against
	// (#52): the same one the sync resolver enforces.
	opts = append(opts, api.WithGmailDailyBudget(int64(cfg.GmailDailyDownloadMB)*1000*1000))
	if oof != nil {
		opts = append(opts, api.WithOutOfOffice(oof))
	}
	// The per-user request-rate limit (spec §10, MAILROOM_RATE_LIMIT_RPS /
	// MAILROOM_RATE_LIMIT_BURST). Without this call api.New's compiled default
	// (DefaultRateLimit*) applies no matter what the operator configured (#624
	// §3) — config.Load seeds cfg.RateLimit* with that SAME default, so wiring it
	// here changes behaviour only when the operator actually sets the env var,
	// including setting either to 0: RateLimitConfig.Enabled() treats a
	// non-positive rate OR burst as "disable limiting entirely" (NewRateLimiter
	// then returns a nil *RateLimiter, and Allow on a nil limiter admits every
	// request), which is the operator's deliberate opt-out, not a bug.
	opts = append(opts, api.WithRateLimit(apiRateLimitConfig(cfg)))
	// The Triage control methods (#34) reset/rollback the priority model and read its
	// state through this registry (the SAME instance the ingest Scorer uses). Nil on
	// a degraded no-blob-store instance: the controls then return a clean error.
	if registry != nil {
		opts = append(opts, api.WithTriageRegistry(registry))
	}
	// Advertise the VAPID applicationServerKey in the session so a browser can
	// subscribe for Web Push (#42, ADR-016). The keypair is derived from the master
	// key, so it is stable and needs no separate secret; a derivation failure only
	// costs the push capability its key, never the whole surface — degrade, don't die.
	if pub, err := vapidPublicKey(ctx, keys); err != nil {
		logger.Warn("web push disabled in session: could not derive the VAPID key", "err", err)
	} else {
		opts = append(opts, api.WithPushKey(pub))
	}
	// API key alerts (#49, ADR-042 §6): anomalies and approval requests go to
	// the user's devices through the same push path and egress guard as new
	// mail. Without a key or a store they are audited and not pushed.
	if st != nil {
		if vk, err := deriveVAPID(ctx, keys); err == nil {
			sender := webpush.NewSender(vk, egress.New(egress.Policy{Timeout: 30 * time.Second}), "")
			opts = append(opts, api.WithAlertNotifier(syncrunner.NewStorePushNotifier(st, sender, logger)))
		}
	}
	// The Authentication-Results identities this deployment believes (#414,
	// RFC 8601 §5). Gates ONE thing: whether a DKIM/DMARC `pass` may suppress
	// the selfSpoof sender warning.
	//
	// ⚠ SAID OUT LOUD EITHER WAY, because "no trusted id" is a real security
	// posture and not a default worth hiding. Unset is FAIL-SAFE — no header is
	// believed, so the warning fires rather than being suppressed — but an
	// operator should know that is what they have, since the cure for the noise
	// is to name their provider's id rather than to disable the warning.
	if ids := trustedAuthservIDs(); len(ids) > 0 {
		opts = append(opts, api.WithTrustedAuthservIDs(ids))
		logger.Info("sender spoofing warnings will believe Authentication-Results from these identities only",
			"authserv_ids", strings.Join(ids, ","), "env", config.EnvAuthservIDs)
	} else {
		logger.Info("no trusted Authentication-Results identity is configured, so no authentication result "+
			"is believed for spoofing warnings: a message claiming one of your own domains is warned "+
			"about even if it carries a passing DKIM header, because that header is attacker-writable",
			"env", config.EnvAuthservIDs)
		// Learned placement (#714) reads the same setting: with it, a known
		// sender's mail is filed only on a trusted DMARC pass; without it, only
		// on mail whose authentication did not clearly fail. Said once, here.
		logger.Warn("learned placement is running without trusted authentication results: it files a known "+
			"sender's new mail whenever its authentication did not clearly fail, which a forged From with no "+
			"Authentication-Results also satisfies; name your provider's authserv-id to require a DMARC pass",
			"env", config.EnvAuthservIDs)
	}
	// The image proxy (ADR-026, #55 increment 3). ON BY DEFAULT (decision C):
	// §3c limits it to fetching what the client would have fetched anyway, so
	// enabling it MOVES an existing fetch from the reader's browser to the
	// server rather than adding one. Its rate limit and per-message cap ship
	// with it, not behind it. It fetches through internal/egress with http
	// permitted (decision A) — a widening confined to this caller.
	if cfg.ImageProxy {
		opts = append(opts, api.WithImageProxy(api.ProxyConfig{Enabled: true,
			Prefetch: cfg.ImageProxyPrefetch, PrefetchMaxAge: cfg.ImageProxyPrefetchMaxAge,
			ReferencedCacheBytes: cfg.ImageProxyPrefetchMaxBytes}))
		logger.Info("image proxy enabled: remote images are fetched by this server, not by your browser",
			"env", config.EnvImageProxy)
		// The arrival prefetch (#922): allowed senders' images are fetched
		// when the mail arrives and kept while the message is kept. Said at
		// boot either way, so "is this instance prefetching?" is answerable
		// from the log rather than by waiting for mail.
		if cfg.ImageProxyPrefetch {
			logger.Info("image prefetch enabled: images from senders you allow are fetched when mail arrives and kept while the message is kept",
				"env", config.EnvImageProxyPrefetch, "max_age", cfg.ImageProxyPrefetchMaxAge.String(),
				"max_bytes", cfg.ImageProxyPrefetchMaxBytes, "max_bytes_env", config.EnvImageProxyPrefetchMaxBytes)
			// The prefetch acts on a From before anyone has read the mail, so it
			// requires the sender authenticated by learned placement's verdict.
			// Without trusted authserv-ids that verdict is only "did not clearly
			// fail"; said once, so the weaker mode is visible.
			if len(config.TrustedAuthservIDs()) == 0 {
				logger.Warn("image prefetch is running without trusted authentication results: it fetches for an allowed "+
					"sender's mail whenever its authentication did not clearly fail, which a forged From with no "+
					"Authentication-Results also satisfies; name your provider's authserv-id to require a DMARC pass",
					"env", config.EnvAuthservIDs)
			}
		} else {
			logger.Info("image prefetch disabled: images are fetched only when a message is opened",
				"env", config.EnvImageProxyPrefetch)
		}
	} else {
		logger.Info("image proxy disabled: your browser fetches remote images directly when you allow them",
			"env", config.EnvImageProxy)
	}
	// Per-method timing (#665): the histograms at /metrics and one log line for a
	// slow request. Read-only; it cannot change a response. m is nil in tests
	// that build the handler without a registry.
	// PGP/MIME decrypt on open (#47 6b, ADR-037): the crypto process opens the
	// message on the request path; nothing is stored. Parts of a decrypted
	// message are scanned on demand by the same clamd ingest uses.
	if d := newCryptoDecrypter(cfg.PGPSocket, logger); d != nil {
		opts = append(opts, api.WithDecrypter(d))
	}
	if scanner != nil {
		opts = append(opts, api.WithOnDemandScanner(scanner))
	}
	// Own-key management over JMAP (#796): the crypto process's key verbs, and
	// the WKD domain UserKey/get reports and UserKey/set publishes for. A nil
	// custody is left unwired (not passed as a typed nil), so the API answers
	// keyManagementUnavailable instead of calling a client that cannot dial.
	if c := newCryptoCustody(cfg.PGPSocket); c != nil {
		opts = append(opts, api.WithKeyCustody(c))
	}
	opts = append(opts, api.WithWKDDomain(cfg.WKDDomain))
	// The download-time verdict of an attachment of a decrypted message (#47):
	// one INFO line (always) and mailroom_ondemand_scan_total{verdict}.
	opts = append(opts, api.WithOnDemandObserver(onDemandScanObserver(m)))
	// Recipient key lookup for encrypted sends (#47, ADR-038): WKD, through the
	// same SSRF-guarded egress client key discovery uses (https only, short
	// timeout, key-sized byte cap). The host it fetches from is derived from a
	// recipient address the user typed, i.e. attacker-influenced, which is
	// exactly what internal/egress exists for.
	opts = append(opts, api.WithKeyFetcher(wkd.NewFetcher(egress.New(egress.Policy{
		Timeout:  10 * time.Second,
		MaxBytes: pgpkeys.MaxKeyBytes,
	}), logger)))
	opts = append(opts, api.WithRequestTiming(jmapTimingObserver(m, logger)))
	opts = append(opts, api.WithVersion(version))
	// Image-proxy refusals by reason (#674): counters at /metrics and one INFO
	// line per refusal. Report-only; it changes no response.
	opts = append(opts, api.WithProxyObserver(logger, proxyObserver(m)))
	opts = append(opts, api.WithProxyCacheObserver(proxyCacheObserver(m)))
	// Signed-in devices (#937, ADR-048 section 6): a device sign-out revokes the
	// provider's refresh token (the Authenticator is installed into the revoker
	// by oidcGate), and an open event stream re-checks its session.
	revoker := newProviderRevoker(ctx, newSessionStore(st, keys), logger)
	opts = append(opts,
		api.WithSessionRevoker(revoker.revoke),
		api.WithSessionLive(sessionLiveCheck(st, auth.DefaultOutageAllowance)))
	svc := api.New(st, opts...)
	// Registers the search-aware Email/query and SearchSnippet/get. A nil service
	// is a no-op: the API then answers every structured filter and rejects a text
	// filter outright, which is the honest response for a server with no index.
	svc.SetSearch(searchSvc)
	// Scanner FRESHNESS for the health panel (#395, ADR-027 §5c). Health-only:
	// scanning at ingest and re-scanning through the dispatcher are wired
	// elsewhere. A nil scanner omits the reading entirely rather than reporting
	// on a daemon that does not exist.
	if scanner != nil {
		svc.SetScanner(scanner)
	}

	h, err := mountApp(ctx, st, cfg, operatorID, logger, svc.Handler(), webHandler(cfg.WebRoot, logger),
		oauthCallbackHandler(oa.connector), nil, revoker)
	if err != nil {
		return nil, nil, err
	}
	// svc travels back so cmd can install the writeback nudge once the sync
	// runner exists (#581) — the handler alone cannot reach it.
	return h, svc, nil
}

// mountApp builds the app mux: the auth path's open routes, then every route in
// the inventory, each wrapped according to the side of the gate it declared.
func mountApp(ctx context.Context, st *store.Store, cfg *config.Config, operatorID ulid.ULID, logger *slog.Logger, jmap, web, oauthCB http.Handler, keypubLookup keypub.Lookup, revoker *providerRevoker) (http.Handler, error) {
	var (
		open    []route
		wrapAPI func(http.Handler) http.Handler
		wrapWeb func(http.Handler) http.Handler
		err     error
	)

	switch {
	case cfg.OIDCConfigured():
		open, wrapAPI, wrapWeb, err = oidcGate(ctx, st, cfg, logger, revoker)
	case cfg.DevAuthConfigured():
		open, wrapAPI, wrapWeb, err = devAuthGate(cfg, operatorID, logger)
	default:
		// Unreachable: config.Load rejects a Config with no usable auth path. A
		// partially configured OIDC block lands there too — it is a config error,
		// never a silent fall-through to open access (config.validateAuth).
		err = errors.New("main: no auth path configured")
	}
	if err != nil {
		return nil, err
	}

	// API KEYS (#49, ADR-042): a request with an Authorization header is decided
	// by the bearer gate and never reaches the session gate; one without it goes
	// to the session gate exactly as before. In front of BOTH auth paths, so a
	// dev-auth instance still refuses a bad key rather than ignoring it. Only the
	// API routes are wrapped: a browser route never accepts a key.
	bearerGate := &auth.BearerGate{
		Resolve:       apiKeyResolver(st),
		SessionCookie: auth.DefaultSessionCookieName,
		Limiter:       auth.NewFailureLimiter(auth.DefaultBearerFailureBurst, auth.DefaultBearerFailureRefill),
		Log:           logger,
	}
	// CSRF defence in depth (#860) for the session path only: the guard sits
	// INSIDE the session gate (an anonymous request still gets its 401) and
	// BEHIND the bearer gate (an API-key request never reaches it). It covers
	// every state-changing route the API serves, /jmap and /jmap/upload/ and
	// /mcp included; the browser routes are read-only static files, and logout
	// is a deliberate GET navigation that must work cross-site.
	sessionGate := wrapAPI
	sessionAPI := func(h http.Handler) http.Handler {
		return sessionGate(auth.CSRFGuard(h, "/jmap", logger))
	}
	wrapAPI = func(h http.Handler) http.Handler { return bearerGate.Wrap(h, sessionAPI(h)) }

	// The public key surface (#48). It is built here rather than passed in
	// because it needs only the store and one config value, and because the
	// route inventory below must be able to see it — a route that exists only
	// in some builds is a route the gate tests stop covering.
	// The lookup is injectable so the gate test can drive a service that
	// actually SERVES something. It used to build its own with an empty domain,
	// which meant every probe on the public routes got a 404 and the whole
	// gatePublic assertion set passed without reaching a handler — the exact
	// "passes for the wrong reason" failure the probes exist to prevent.
	lookup := keypubLookup
	if lookup == nil {
		lookup = wkdLookup{st}
	}
	keyPub, err := keypub.New(lookup, cfg.WKDDomain, logger)
	if err != nil {
		return nil, fmt.Errorf("public key surface: %w", err)
	}
	// The anonymous surface's budget (#264). keypub.New already installs the
	// default — this only applies an operator override, so a missing or
	// mis-typed variable leaves limiting ON rather than silently off.
	keyPub.WithRateLimit(keypub.RateLimitConfig{
		PerSecond: cfg.WKDRateLimitPerSecond,
		Burst:     cfg.WKDRateLimitBurst,
	}, nil)
	if keyPub.Enabled() {
		logger.Info("WKD publication enabled", "domain", cfg.WKDDomain,
			"draft_revision", wkd.DraftRevision,
			"rate_limit_rps", cfg.WKDRateLimitPerSecond,
			"rate_limit_burst", cfg.WKDRateLimitBurst)
	} else {
		logger.Info("WKD publication disabled", "reason",
			config.EnvWKDDomain+" is unset; key lookups will answer 404")
	}

	// HSTS only when the app is actually reachable over TLS — the same signal that
	// decides Secure on the session cookie (oidcGate), so the two cannot disagree.
	hsts := strings.HasPrefix(cfg.OIDCRedirectURL, "https://")
	// The shell CSP for THIS deployment: appCSP, plus the app's own origin in
	// img-src when it is served over cleartext (#460, appCSPFor) — derived from
	// the same redirect URL as hsts, so the two agree about the public origin.
	csp := appCSPFor(cfg.OIDCRedirectURL)

	mux := http.NewServeMux()
	for _, rt := range routes(jmap, web, oauthCB, open, keyPub) {
		h := rt.handler
		switch rt.gate {
		case gateOpen:
			// No wrapper, by design. These handlers ARE the gate, and each one sets
			// its OWN, stricter policy: the sign-in page pins its inline CSS and JS
			// by CSP sha256 hash and permits no external fetch at all (auth/page.go).
			// Wrapping it in appCSP would REPLACE that with a weaker one.
		case gateAPI:
			h = wrapAPI(h)
			if rt.pattern == api.MCPPath && cfg.OIDCConfigured() {
				// An MCP request with no credential at all gets the bearer
				// gate's 401 and its WWW-Authenticate: Bearer challenge rather
				// than the session gate's bare 401: that header is how an MCP
				// client learns it needs a key (ADR-044). It sits OUTSIDE the
				// auth chain and only short-circuits a request that carries
				// neither an Authorization header nor a session cookie. Under
				// dev-auth every request already IS the operator's session, so
				// a keyless one reaches /mcp's keys-only 403 instead.
				h = api.MCPChallenge(h, auth.DefaultSessionCookieName)
			}
		case gatePublic:
			// Hardening headers, no auth. The handler is reached by strangers on
			// purpose, so it gets MORE care than a gated route, not less: the
			// content is public, the origin is still ours, and a key page that
			// could be framed by an attacker's site would let them dress up a
			// fingerprint as their own.
			//
			// The handler may still override individual headers it has a reason
			// to (WKD sets its own Content-Type and a cacheable Cache-Control);
			// it just does not start from nothing.
			h = secureHeaders(h, hsts, csp)
		case gateBrowser:
			// secureHeaders is OUTSIDE the auth middleware on purpose: the 302 that
			// bounces an anonymous browser to the sign-in page is a response from
			// this origin too, and it must carry the same headers (and must not be
			// cached). A control that only holds on the happy path is not a control.
			h = secureHeaders(wrapWeb(h), hsts, csp)
		}
		mux.Handle(rt.pattern, h)
	}
	return mux, nil
}

// oidcGate builds the OIDC auth path: the open sign-in routes, and the two
// middleware flavors everything else hides behind.
func oidcGate(ctx context.Context, st *store.Store, cfg *config.Config, logger *slog.Logger, revoker *providerRevoker) ([]route, func(http.Handler) http.Handler, func(http.Handler) http.Handler, error) {
	cbPath, err := callbackPath(cfg.OIDCRedirectURL)
	if err != nil {
		return nil, nil, nil, err
	}
	authn, err := auth.New(ctx, auth.Config{
		IssuerURL:    cfg.OIDCIssuer,
		ClientID:     cfg.OIDCClientID,
		ClientSecret: cfg.OIDCClientSecret,
		RedirectURL:  cfg.OIDCRedirectURL,
		SigningKey:   cfg.SessionKey,
		// Server-side sessions (#937, ADR-048). The adapter seals the provider
		// tokens under the master key; the store never sees one.
		SessionStore: newSessionStore(st, crypto.NewFileKeyProvider(cfg.MasterKeyFile)),
		// The proxies whose X-Forwarded-For names a device's address in the
		// signed-in devices list. Display only.
		TrustedProxies: cfg.TrustedProxies,
		// Cookies must carry Secure whenever the app is reached over HTTPS. The
		// redirect URL the provider will send the browser back to is exactly
		// that origin, so its scheme is the honest answer.
		CookieSecure: strings.HasPrefix(cfg.OIDCRedirectURL, "https://"),
		// LogoutPath must be where LogoutHandler is actually mounted below: it
		// is the Path used to clear the retired ID-token cookie (#314, ADR-048),
		// and a mismatch leaves that leftover cookie in the browser.
		LogoutPath: logoutPath,
		// Where the provider returns the browser after ending its session. It
		// must be an ABSOLUTE url and is usually required to be registered
		// against the OIDC client, so it is derived from the redirect URL's own
		// origin — the one origin already registered with the provider — and is
		// omitted entirely if that cannot be determined.
		PostLogoutRedirectURI: postLogoutURI(cfg.OIDCRedirectURL),
	}, provisioningResolver(st, cfg.OperatorSubject, logger),
		// A refused login lands on the sign-in page saying WHY, with no session
		// cookie — instead of a bare 403, and instead of being thrown back at a
		// provider that will cheerfully re-issue the same rejected identity
		// until the user gives up.
		auth.WithFailureRedirect(loginPath),
		auth.WithLogger(logger),
		auth.WithPostLogoutRedirect(loginPath+"?notice="+auth.NoticeSignedOut),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("main: auth: %w", err)
	}
	// A device sign-out from the devices list revokes the refresh token at the
	// provider through this same Authenticator.
	revoker.set(authn)

	page, err := auth.NewPage(auth.PageConfig{StartPath: startPath})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("main: sign-in page: %w", err)
	}

	logger.Info("auth: OIDC", "issuer", cfg.OIDCIssuer,
		"sign_in", loginPath, "start", startPath, "callback", cbPath)

	open := []route{{
		pattern: "GET " + loginPath,
		gate:    gateOpen,
		handler: http.HandlerFunc(page.Handler),
		probes: []probe{
			get(loginPath),
			get(loginPath + "?error=" + auth.ErrorDenied),
			get(loginPath + "?error=" + auth.ErrorExpired),
			get(loginPath + "?error=" + auth.ErrorFailed),
			get(loginPath + "?notice=" + auth.NoticeSignedOut),
			get(loginPath + "?return_to=%2Finbox"),
		},
	}, {
		pattern: "GET " + startPath,
		gate:    gateOpen,
		handler: http.HandlerFunc(authn.LoginHandler),
		probes:  []probe{get(startPath)},
	}, {
		pattern: "GET " + cbPath,
		gate:    gateOpen,
		handler: http.HandlerFunc(authn.CallbackHandler),
		// No login state, no code: the callback must refuse it. It is gateOpen
		// because it MUST be reachable without a session — that is the whole
		// point of a callback — not because it is permissive.
		probes: []probe{get(cbPath)},
	}, {
		pattern: logoutPath,
		gate:    gateOpen,
		handler: http.HandlerFunc(authn.LogoutHandler),
		// Logout is open on purpose: clearing a cookie you may or may not have
		// must never itself require a valid session (an expired session that
		// cannot be signed out of is a trap).
		probes: []probe{get(logoutPath), post(logoutPath)},
	}}

	return open,
		func(h http.Handler) http.Handler { return authn.Middleware(h) },
		func(h http.Handler) http.Handler {
			return authn.Middleware(h, auth.WithUnauthenticatedRedirect(loginPath))
		},
		nil
}

// devAuthGate builds the INSECURE dev-auth escape hatch: there is no gate. Every
// route is open and every request is the operator.
//
// It still serves the sign-in page at loginPath — in its dev-auth variant, which
// says in as many words that this instance authenticates nobody. That is not
// decoration: without it, /auth/login is a 404 and the web client's own "Sign
// out" button leads nowhere, so the one configuration that is genuinely unsafe
// is also the one that looks the most normal.
func devAuthGate(cfg *config.Config, operatorID ulid.ULID, logger *slog.Logger) ([]route, func(http.Handler) http.Handler, func(http.Handler) http.Handler, error) {
	if operatorID == (ulid.ULID{}) {
		return nil, nil, nil, errors.New("main: dev-auth is configured but no operator was provisioned")
	}
	// Loud, repeated, and greppable: this instance authenticates nobody.
	logger.Warn(auth.DevAuthWarning)
	logger.Warn("auth: DEV-AUTH (INSECURE)", "operator_subject", cfg.DevAuthSubject,
		"user_id", operatorID.String())

	page, err := auth.NewPage(auth.PageConfig{DevAuth: true})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("main: sign-in page: %w", err)
	}
	open := []route{{
		pattern: "GET " + loginPath,
		gate:    gateOpen,
		handler: http.HandlerFunc(page.Handler),
		probes:  []probe{get(loginPath)},
	}, {
		pattern: logoutPath,
		gate:    gateOpen,
		handler: http.HandlerFunc(page.Handler),
		probes:  []probe{get(logoutPath)},
	}}

	devAuth := func(h http.Handler) http.Handler { return auth.DevAuth(h, operatorID) }
	return open, devAuth, devAuth, nil
}

// callbackPath extracts the path the OIDC provider will redirect back to, so the
// route we serve is by construction the one registered with the provider (rather
// than a hardcoded path that can silently disagree with it).
func callbackPath(redirectURL string) (string, error) {
	u, err := url.Parse(redirectURL)
	if err != nil {
		return "", fmt.Errorf("main: %s is not a valid URL: %w", config.EnvOIDCRedirectURL, err)
	}
	switch u.Path {
	case "", "/":
		return "", fmt.Errorf("main: %s must have a path (e.g. https://mail.example.com/auth/callback), got %q",
			config.EnvOIDCRedirectURL, redirectURL)
	case loginPath, logoutPath:
		return "", fmt.Errorf("main: %s must not collide with %s or %s",
			config.EnvOIDCRedirectURL, loginPath, logoutPath)
	}
	return u.Path, nil
}

// adminGroup is the group whose members get the admin role. App-binding is the
// access gate — Authentik decides who reaches the application at all — and within
// that, membership of this group is what distinguishes an admin from an ordinary
// user. It is re-evaluated on every login (provisioningResolver), so adding or
// removing a user from it in the identity provider takes effect on their next
// sign-in.
const adminGroup = "mailroom-admin"

// provisioningResolver is the auth.ResolveUser the OIDC callback uses under the
// multi-user model (mailroom#98). App-binding is the access gate: ANY identity
// that successfully completes the OIDC flow for this application is authorized,
// and is AUTO-PROVISIONED on first login from the VERIFIED token's claims — this
// replaces the old allowlist-of-one. Authentik decides who can reach the app;
// Mailroom trusts a successfully-authenticated identity.
//
// Every login (not just the first):
//   - get-or-creates the user keyed by the stable `sub` claim, populating
//     email and display name from the token (email from `email`, name from
//     `name` then `preferred_username`);
//   - re-applies the role from group membership — a member of adminGroup is an
//     admin, everyone else a user — so a group change takes effect next login.
//
// It fails CLOSED: nothing request-supplied is ever trusted (only the validated
// token reaches here), a token with no usable email cannot provision and is
// refused, and a subject that resolves to a disabled/deleted user is refused
// (store.ResolveLogin returns ok=false). Most refusals abort the login with no
// session cookie on the generic ErrorDenied path the sign-in page explains —
// EXCEPT store.ErrLoginEmailTaken (Forgejo #574), which wraps auth.ErrCannotProvision
// so the callback can give it its own ErrorUnprovisionable code and sentence:
// the identity WAS verified, and the refusal comes from this server's own
// records (most often an operatorSubject that is not the provider's sub), not
// from anything the user can fix by signing in again.
// postLogoutURI derives the absolute url the OIDC provider should return the
// browser to after RP-initiated logout (#314), from the callback url's origin
// plus the sign-in path.
//
// The callback url's ORIGIN is used deliberately: it is the one origin already
// registered with the provider for this client, so a deployment that works for
// login has the best chance of this being accepted too. Providers commonly
// reject an unregistered post_logout_redirect_uri outright.
//
// Returns "" when redirectURL is unusable, which makes auth omit the parameter
// and let the provider fall back to its own configured destination — logout
// still ends the provider session, it just lands wherever the provider says.
func postLogoutURI(redirectURL string) string {
	u, err := url.Parse(redirectURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + loginPath + "?notice=" + auth.NoticeSignedOut
}

// pendingSweepInterval is how often expired approval requests are swept.
const pendingSweepInterval = 5 * time.Minute

// apiKeyResolver adapts the store's token lookup to the bearer gate. With no
// store (the gate tests, which mount sentinel handlers) every token is unknown:
// fail closed, never "no store means no check".
func apiKeyResolver(st *store.Store) auth.ResolveAPIKey {
	return func(ctx context.Context, hash [32]byte) (ulid.ULID, ulid.ULID, bool, error) {
		if st == nil {
			return ulid.ULID{}, ulid.ULID{}, false, nil
		}
		return st.ResolveAPIKeyToken(ctx, hash)
	}
}

// operatorSubject is the configured MAILROOM_OPERATOR_SUBJECT (cfg.OperatorSubject,
// the subject operatorIdentity provisions at boot under OIDC), if any — passed through so a
// login refusal or a surprising new user can say, specifically, whether it is
// consistent with a mismatched operator config rather than an unrelated collision.
func provisioningResolver(st *store.Store, operatorSubject string, logger *slog.Logger) auth.ResolveUser {
	return func(ctx context.Context, claims auth.Claims) (ulid.ULID, error) {
		email := strings.TrimSpace(claims.Email)
		if email == "" {
			// app.users.email is NOT NULL UNIQUE and surfaces as the user's JMAP
			// identity; without it we cannot provision an honest account. Refuse
			// rather than synthesize a placeholder.
			logger.Warn("login denied: token carried no email claim to provision from", "subject", claims.Subject)
			return ulid.ULID{}, fmt.Errorf("%w: token has no email claim", auth.ErrRefused)
		}
		admin := hasGroup(claims.Groups, adminGroup)
		id, created, ok, err := st.ResolveLogin(ctx, claims.Subject, email, displayNameFromClaims(claims), admin)
		if err != nil {
			if errors.Is(err, store.ErrLoginEmailTaken) {
				// Another user here already holds this email — most often the
				// operator, provisioned at boot under a subject that is not the one
				// the provider issues. Say so with enough to act on, never logging
				// the email itself (subject and role only, as everywhere here).
				//
				// The hint must NOT say "set the variable to token_subject". Doing
				// only that makes the next boot provision a second user under it,
				// collide on this same email and refuse to start: the user that owns
				// the mail has to be moved first (deploy/rebind-operator-subject.sql).
				// And anyone who can put the operator's email on an account at the
				// provider can produce this line, so the first step is to confirm
				// who token_subject is.
				hint := "another user here already holds this email: the provider issued a new " +
					"subject for someone who already has an account, or changed this subject's " +
					"email to another user's; nothing was changed"
				if operatorSubjectMismatch(operatorSubject, claims.Subject) {
					hint = "if this is the operator, the operator was provisioned at boot under " +
						"operator_subject, which is not the subject the provider issues. Do NOT just change " +
						config.EnvOperatorSubject + ": the next boot would collide on this email and refuse " +
						"to start. Confirm token_subject is your account in the provider's admin view, then " +
						"follow deploy/README.md section 4, \"A wrong operator subject\""
				}
				logger.Error("login refused: this email already belongs to another user",
					"token_subject", claims.Subject,
					"operator_subject", operatorSubject,
					"hint", hint)
				return ulid.ULID{}, fmt.Errorf("%w: %w: %w", auth.ErrRefused, auth.ErrCannotProvision, err)
			}
			return ulid.ULID{}, err
		}
		if !ok {
			// A disabled or deleted user: authenticated by the provider, but not
			// allowed back in here.
			logger.Warn("login denied: subject resolves to a disabled or deleted user", "subject", claims.Subject)
			return ulid.ULID{}, fmt.Errorf("%w: this account is disabled", auth.ErrRefused)
		}
		role := "user"
		if admin {
			role = "admin"
		}
		// Subject and role only — never the token, the email, or any body.
		logger.Info("login provisioned", "subject", claims.Subject, "user_id", id.String(), "role", role)
		if created && operatorSubjectMismatch(operatorSubject, claims.Subject) {
			// Allowed — multi-user is legitimate, and this may simply be a second,
			// genuine user — but never SILENT: if it was actually meant to be the
			// operator logging in, a mismatched operatorSubject just split one
			// person into two accounts, and this is the only place that can say so.
			logger.Warn("login created a NEW user while "+config.EnvOperatorSubject+" names a different subject",
				"token_subject", claims.Subject,
				"operator_subject", operatorSubject,
				"user_id", id.String(),
				"hint", "if this is the operator, the declared accounts are syncing into the user under "+
					"operator_subject, not this one; see deploy/README.md section 4, \"A wrong operator subject\"")
		}
		return id, nil
	}
}

// operatorSubjectMismatch reports whether a configured MAILROOM_OPERATOR_SUBJECT
// exists AND disagrees with the subject a login just resolved — the single
// condition that turns an email collision, or a login that created a new user,
// into a "your operator config is probably wrong" signal rather than an
// ordinary multi-user event (no operator declared, or the subjects agree).
// Pure and DB-free on purpose, unlike the rest of provisioningResolver, so the
// actual decision is unit-testable without a store.Store (mirrors
// operatorIdentity's rationale above).
func operatorSubjectMismatch(operatorSubject, tokenSubject string) bool {
	return operatorSubject != "" && operatorSubject != tokenSubject
}

// hasGroup reports whether groups contains want. The provider's group claim is
// the sole input to the role decision, and an absent/empty claim is simply "no
// privileged membership" — never an error.
func hasGroup(groups []string, want string) bool {
	for _, g := range groups {
		if g == want {
			return true
		}
	}
	return false
}

// displayNameFromClaims picks the human-facing name for app.users.display_name:
// the `name` claim, else `preferred_username`, else "" (the column's default —
// the JMAP session then shows the email, which is never empty). It never
// synthesizes a name from the email local-part: a blank display name is honest,
// a guessed one is a fiction baked into the user's identity.
func displayNameFromClaims(claims auth.Claims) string {
	if n := strings.TrimSpace(claims.Name); n != "" {
		return n
	}
	return strings.TrimSpace(claims.PreferredUsername)
}

// --- HTTP wiring: the app shell's security headers ---------------------------

// appCSP is the Content-Security-Policy on the WEB CLIENT — the app shell, the
// one document on this origin that runs JavaScript (spec §11: "strict CSP, no
// inline scripts"; Forgejo #39; docs/hostile-content.md).
//
// Read it as three separate jobs:
//
//  1. NOTHING RUNS THAT WE DID NOT SHIP. `default-src 'none'` denies every fetch
//     type by default and `script-src 'self'` then allows exactly one thing: a
//     same-origin script FILE. There is no 'unsafe-inline', no 'unsafe-eval', and
//     no nonce — so a script tag injected into our DOM cannot execute, and Vite's
//     output is held to that bar (its index.html emits an external module script
//     and nothing inline; security_test.go asserts the shipped index.html has no
//     inline <script> body, so the day a build tool starts inlining one, the
//     build fails rather than the policy silently blocking the app).
//
//  2. THE APP CANNOT BE TURNED INTO A WEAPON. `connect-src 'self'` means a
//     hypothetical injected script cannot exfiltrate to another origin;
//     `form-action 'none'` means nothing on this page can POST anywhere;
//     `base-uri 'none'` closes the <base> hijack that turns every relative URL on
//     the page into an attacker's URL; `frame-ancestors 'none'` (+ the legacy
//     X-Frame-Options) means nobody frames our mail.
//
//  3. IT IS THE OUTER HALF OF THE MESSAGE SANDBOX. This is the part that is easy
//     to get wrong, so it is stated here rather than discovered later: a
//     `srcdoc` iframe INHERITS its embedder's CSP (CSP3 §7.4 — policy-container
//     inheritance for local-scheme documents). So THIS policy is also enforced
//     inside the rendered message, on top of the message document's own
//     `<meta>` CSP, and the two INTERSECT. In an OPAQUE origin `script-src 'self'`
//     matches nothing, so the message can run NOTHING of its own — the ONE
//     exception is the hash-pinned height reporter (messageHeightReporterHash,
//     for content-fit auto-height #100): it is named here AND in the message's
//     own CSP, so the intersection admits exactly that one inert script and
//     nothing else. It is why `img-src` carries `https:` here:
//     the message document's own policy is `img-src data:` and blocks every
//     remote image, and the user's explicit "Show remote content" click relaxes
//     THAT policy to `data: https:`. If this outer policy did not permit https:
//     images, the intersection would still be empty and the affordance would be
//     a button that does nothing.
//
//     The cost of that headroom is stated plainly: an attacker who achieved
//     script execution in the SHELL could exfiltrate over an <img> URL. They
//     cannot achieve it — script-src 'self', no inline scripts, no eval, and the
//     shell never injects message-derived HTML into its own DOM (there is no
//     dangerouslySetInnerHTML in web/, asserted in the web build) — but the
//     trade is real and it is made deliberately, in exchange for remote-content
//     blocking that a user can actually see and act on.
//
// http: is deliberately NOT in img-src. A remote image is opt-in, but it is not
// opt-in over cleartext.
//
// `frame-src` is EXACTLY 'self', and the ATTACHMENT PREVIEW (#131,
// docs/hostile-content.md §2.4) is deliberately built so that it stays that way:
// a previewed PDF is framed from OUR OWN path, not from a blob: URL.
//
// blob: was tried first and does not work — a sandboxed iframe cannot load a
// blob: URL at all (Chromium refuses it with net::ERR_BLOCKED_BY_CLIENT). The only
// way to frame a blob would have been to drop the frame's sandbox, which trades an
// origin boundary for a CSP source. Serving the preview from our own origin keeps
// both: 'self' already covers it, and the opaque origin comes from the response's
// own `Content-Security-Policy: default-src 'none'; sandbox` (internal/api/blob.go).
//
// object-src stays 'none': <embed>/<object> are a plugin surface and a preview
// has no use for one.

// messageHeightReporterHash pins web/src/app/ui/messageDocument.ts's REPORTER_JS
// by sha256. The message srcdoc frame inherits appCSP and the two policies
// intersect, so the shell must ALSO name this hash or the height reporter (and
// content-fit auto-height, #100) is blocked. It must stay byte-identical to
// REPORTER_SHA256 in messageDocument.ts — cmd/mailroom/security_test.go asserts
// both files carry it. A hash is not 'unsafe-inline': only this one exact,
// scriptless-otherwise reporter matches, so the shell is not weakened.
const messageHeightReporterHash = "sha256-1XkLCQZRSIgsRrQKKNvaJTcjTinrA6u/CFBnA4fTy+4="

const appCSP = "default-src 'none'; " +
	"script-src 'self' '" + messageHeightReporterHash + "'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"manifest-src 'self'; " +
	"worker-src 'self'; " +
	"frame-src 'self'; " +
	"object-src 'none'; " +
	"form-action 'none'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'"

// appCSPFor is appCSP with ONE addition, made only for a deployment that is
// served over cleartext (#460): the app's OWN origin in img-src.
//
// WHY. Since the image proxy (ADR-026) an allowed remote image is no longer the
// sender's https:// URL but a same-origin path, /jmap/proxy/..., and inside the
// srcdoc message frame that path resolves against the app's base URL. The frame
// inherits this policy and intersects it with its own (see the appCSP comment,
// job 3). On an https deployment `https:` covers the proxied URL; on an http one
// NOTHING did — `'self'` matches nothing in the frame's opaque origin, and bare
// `http:` is deliberately absent — so every allowed image was refused by the
// browser before a request was made, silently: the server never saw it, the
// proxy cache stayed at one row for a month, and the banner kept saying images
// were loading. It was found by eye.
//
// THE ADDITION IS THE ONE EXPLICIT ORIGIN, NEVER THE http: SCHEME. An operator
// running the app itself over cleartext has already accepted cleartext for
// every byte of their mail; admitting that same origin for images adds nothing
// to what an attacker on the path already has, while bare `http:` would let a
// remote image be fetched from any cleartext host, which is the thing the
// appCSP comment refuses. The origin comes from the OIDC redirect URL — the
// same single source of truth that decides HSTS and the cookie's Secure flag —
// so the three can never disagree about what the public origin is.
func appCSPFor(publicURL string) string {
	u, err := url.Parse(strings.TrimSpace(publicURL))
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return appCSP
	}
	origin := "http://" + u.Host
	return strings.Replace(appCSP, "img-src 'self' data: https:;", "img-src 'self' data: https: "+origin+";", 1)
}

// appPermissionsPolicy turns off the browser capabilities a mail client has no
// use for. A feature that is never used is a feature that must never be
// reachable from a message.
const appPermissionsPolicy = "accelerometer=(), autoplay=(), camera=(), " +
	"display-capture=(), encrypted-media=(), fullscreen=(self), geolocation=(), " +
	"gyroscope=(), magnetometer=(), microphone=(), midi=(), payment=(), " +
	"picture-in-picture=(), publickey-credentials-get=(), screen-wake-lock=(), " +
	"usb=(), xr-spatial-tracking=()"

// hstsMaxAge is one year, the value below which HSTS preload lists refuse an
// origin. Emitted only when the instance is actually reachable over HTTPS.
const hstsMaxAge = "max-age=31536000; includeSubDomains"

// secureHeaders puts appCSP and its companions on every response from the web
// client — including its 404s and its redirects, because a header that is only
// present on the happy path is not a control.
//
// It is a WRAPPER rather than a line inside webHandler so that it holds for the
// "no web client installed" case too, and so a future second static surface
// cannot be added without going through it.
//
// hsts is on only when the instance is served over TLS. The signal is the same
// one that decides whether the session cookie carries Secure (the OIDC redirect
// URL the provider will send a browser back to IS the app's public origin) —
// one source of truth, so the two can never disagree. Sending HSTS from a
// plain-http dev instance would pin `http://localhost` to https and brick it.
func secureHeaders(next http.Handler, hsts bool, csp string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", appPermissionsPolicy)
		if hsts {
			h.Set("Strict-Transport-Security", hstsMaxAge)
		}
		// The shell is per-user content behind a login gate: it must never land in
		// a shared cache. The content-hashed bundle assets are immutable by
		// construction, so they may be cached — privately.
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			h.Set("Cache-Control", "private, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// webHandler serves the built web client from root when that directory exists.
// Its absence is fine and expected before the web client ships: the route still
// exists (so an unauthenticated browser still gets bounced through login — a
// useful end-to-end check of the auth path) and simply 404s afterwards.
//
// The files are served through noDirFS, so a directory with no index.html is a
// 404 rather than an auto-generated listing of the build output.
func webHandler(root string, logger *slog.Logger) http.Handler {
	if info, err := os.Stat(root); err == nil && info.IsDir() {
		logger.Info("serving web client", "root", root)
		return http.FileServer(noDirFS{http.Dir(root)})
	}
	logger.Info("no web client installed; serving the API only", "web_root", root)
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "mailroom: no web client installed", http.StatusNotFound)
	})
}

// noDirFS is an http.FileSystem that refuses to open a directory that has no
// index.html. http.FileServer's default is to render an autoindex — a listing of
// every file in the build output, served to anyone through the gate. It is not a
// vulnerability, but it is a free map of the application for no benefit, and
// "the static server does exactly one thing: serve the files we named" is a
// cheaper invariant to keep than "the listing is harmless".
type noDirFS struct{ fs http.FileSystem }

func (n noDirFS) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.IsDir() {
		// http.FileServer asks for the directory's index.html itself when it
		// exists; reaching here with a directory means it does not.
		if index, err := n.fs.Open(strings.TrimSuffix(name, "/") + "/index.html"); err == nil {
			_ = index.Close()
			return f, nil
		}
		_ = f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}

// --- Per-user fan-out: sync + indexer + purge seeding ------------------------

// startUserFanout drives ALL of a Mailroom's background per-user work over EVERY
// provisioned, enabled user (mailroom#98), and returns a channel closed once every
// per-user loop has stopped. It replaces the single-operator sync loop and search
// indexer: a supervisor (internal/syncrunner) lists active users, starts each
// user's work, and re-lists periodically so a user provisioned at runtime (a fresh
// OIDC login) starts syncing without a restart — and a disabled/deleted user's
// loops are torn down.
//
// The per-user work is three tasks:
//   - SYNC: the IMAP sync pass, which first drains the account's WRITEBACK queue
//     (so a local Email/set reaches the server, and a sent message's Sent APPEND
//     is filed) and then reconciles, including the expunge reaper (#86). Writeback
//     and sync share the task's goroutine on purpose — each dials its own IMAP
//     session, and running them concurrently would double an account's concurrent
//     connections (spec §4);
//   - IDLE: the near-instant delivery watcher (mailroom#103 Stage 2), which holds a
//     SEPARATE IMAP IDLE connection per account and triggers the SAME gated sync
//     pass within seconds of new mail; it shares the Runner's per-account gate with
//     SYNC so the two never overlap, and returns at once when IDLE is disabled;
//   - INDEX: draining the change log into the user's search index (skipped when
//     search is off, i.e. indexer is nil);
//   - PURGE SEED: a one-shot that seeds the user's first recurring Trash-purge
//     job so a fresh user starts reclaiming Trash without waiting a full interval;
//     the purge handler then reschedules itself (the global dispatcher runs it).
//
// A nil composer means the blob store failed to open: nothing can be ingested,
// nothing composed, and writeback cannot read an APPEND's bytes back — so there is
// no per-user work to do and the channel is returned already closed. The API still
// serves everything already synced: degrade, don't die.
//
// The single search.Indexer is shared across all users' index tasks (its per-user
// index map is internally locked) and is Closed exactly ONCE here, after the
// supervisor has stopped and every index task has unwound — so its native memory
// is released without racing a still-running task.
func startUserFanout(ctx context.Context, st *store.Store, blobs blob.BlobStore, keys crypto.KeyProvider, registry *ml.Registry, cfg *config.Config, indexer *search.Indexer, composer *compose.Composer, scanner *clamd.Client, apiSvc *api.API, sentNudge *writebackNudgeHook, oa oauthWiring, m *metrics.Metrics, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	haveBlobs := composer != nil
	if !haveBlobs {
		close(done)
		return done
	}

	// T1 priority scoring (spec §8, #33): the registry serves each user's active
	// model out of the encrypted blob store, and the Scorer caches it per user so
	// scoring a delivered message is, in steady state, a pure in-memory Score.
	// Until PR2 wires training there is no active model, so scores are the
	// baseline's ~0.5 — the correct behavior, not a bug. Scoring is best-effort in
	// ingest (a failure never blocks delivery), so no extra error path here. The
	// registry is the SAME instance the API's Triage controls use (built in run and
	// passed through); it is non-nil here because this function early-returned when
	// the blob store was unavailable.
	scorer := ml.NewScorer(registry)
	// The per-label classifier shares the ONE registry with the scorer and the
	// trainer, so a model promoted by the daily pass is the model that serves —
	// a second registry would be a second cache to go stale (#169).
	classifier := ml.NewClassifier(registry)
	// MALWARE SCANNING (#394, ADR-027). Options, in the order they are applied,
	// so the boot log answers "is this instance scanning?" without sending mail.
	// The trusted Authentication-Results identities gate learned placement
	// (#714) exactly as they gate the sender warnings: one setting, one parse.
	ingestOpts := []ingest.Option{ingest.WithScorer(scorer), ingest.WithClassifier(classifier),
		ingest.WithTrustedAuthservIDs(config.TrustedAuthservIDs())}
	if scanner != nil {
		ingestOpts = append(ingestOpts, ingest.WithScanner(scanner))
	}
	// Why learned placement did or did not place each arrival (#840).
	ingestOpts = append(ingestOpts, ingest.WithLearnedPlacement(learnedPlacementObserver(m)))
	// The image prefetch at arrival (#922): ingest only QUEUES a job, for an
	// allowed sender's new mail inside the window; the dispatcher fetches.
	// The window comes from the API so ingest and the job read one setting.
	if apiSvc != nil && apiSvc.ProxyPrefetchEnabled() {
		ingestOpts = append(ingestOpts, ingest.WithImagePrefetch(apiSvc.ProxyPrefetchMaxAge()))
	}
	// Mailroom's own DKIM check for mail with no trusted Authentication-Results
	// (#776, ADR-036): default on, MAILROOM_LOCAL_DKIM=off disables it.
	if cfg.LocalDKIM {
		ingestOpts = append(ingestOpts, ingest.WithLocalDKIM(localDKIMObserver(m)))
		logger.Info("local DKIM check enabled", "env", config.EnvLocalDKIM)
	} else {
		logger.Info("local DKIM check DISABLED", "env", config.EnvLocalDKIM)
	}
	engine := mailsync.New(mailsync.NewStoreAdapter(st),
		ingest.New(st, blobs, ingestOpts...), nil,
		mailsync.WithReapInterval(cfg.ReapInterval),
		// #908/#909 counters and the flag sweep's work, at /metrics.
		mailsync.WithObserver(newSyncObserver(m)),
		// Flag changes made elsewhere, for servers without CONDSTORE
		// (ADR-046 §12). Servers with it never run the sweep.
		mailsync.WithFlagSweep(mailsync.FlagSweepConfig{
			Interval: cfg.FlagSweepInterval, Window: cfg.FlagSweepWindow,
			FullInterval: cfg.FlagSweepFullInterval, FullChunk: config.DefaultFlagSweepFullChunk,
		}))
	if cfg.FlagSweepInterval <= 0 {
		logger.Warn("flag sweep DISABLED: on servers without CONDSTORE (Outlook/Microsoft 365), read and star changes made in other clients will not reach Mailroom",
			"env", config.EnvFlagSweepInterval)
	} else {
		logger.Info("flag sweep enabled for servers without CONDSTORE", "interval", cfg.FlagSweepInterval.String(),
			"window", cfg.FlagSweepWindow, "full_interval", cfg.FlagSweepFullInterval.String())
	}
	if cfg.ReapInterval <= 0 {
		logger.Warn("expunge reaper DISABLED: mail deleted or moved in another IMAP client will never leave its Mailroom folder",
			"env", config.EnvReapInterval)
	} else {
		logger.Info("expunge reaper enabled", "interval", cfg.ReapInterval.String())
	}
	// ⚠ THE LIMITER REGISTRY LIVES ON THIS RESOLVER, so there must be exactly
	// one of it for the process (#638): a second resolver would hand out a
	// second limiter per account and the shared per-account budget would be
	// spent twice over.
	// WithAllowPrivateHosts carries MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS onto
	// every IMAP dial this resolver's Configs produce — sync, the pool, IDLE and
	// the writeback drain all dial with them — so the rule that admitted the
	// account at validation is the rule every later connection is held to
	// (#625 §3).
	resolverOpts := []syncrunner.ResolverOption{
		syncrunner.WithSearchRate(cfg.IMAPSearchRate),
		syncrunner.WithAllowPrivateHosts(cfg.AllowPrivateAccountHosts),
		// The Gmail sync model's daily download budget and label sweep (#52,
		// ADR-047). Inert while the Gmail sync hold stands (GmailSyncReady).
		syncrunner.WithGmailSync(int64(cfg.GmailDailyDownloadMB)*1000*1000, cfg.GmailLabelSweepInterval),
	}
	// OAuth accounts (#52, ADR-046) get XOAUTH2 with tokens from the shared
	// Manager. Without a configured provider the option is not passed, and
	// an OAuth account fails Prepare with ErrOAuthNotConfigured.
	if oa.tokens != nil {
		resolverOpts = append(resolverOpts, syncrunner.WithAccessTokens(oa.tokens))
	}
	resolver := syncrunner.NewCredentialResolver(keys, resolverOpts...)
	if cfg.IMAPSearchRate <= 0 {
		logger.Warn("IMAP SEARCH pacing DISABLED: an account with many mailboxes may have its UID SEARCHes refused by the provider",
			"env", config.EnvIMAPSearchRate)
	} else {
		logger.Info("IMAP SEARCH pacing enabled", "searches_per_minute", cfg.IMAPSearchRate)
	}

	// The writeback runner reads an APPEND intent's bytes back out of the blob
	// store (mailroom#89). It is given the COMPOSER, not a second reader built
	// here: the composer already holds the blob store and the derived mail-blob
	// DEK, and a second key-derivation path for "the writeback side" is exactly
	// the drift that would silently APPEND garbage into the user's Sent folder.
	wb := writeback.NewRunner(st, imapsync.Dial, writebackBlobs{composer}, logger)
	drain := func(ctx context.Context, userID ulid.ULID, acct store.Account, c imapsync.Config) error {
		res, err := wb.DrainAccount(ctx, userID, acct.ID, c)
		// The pass tally was already being computed and thrown away — `_, err :=`
		// until #524. Retried and DeadLettered are both attempts that FAILED;
		// the difference is only whether the engine will try again, and an
		// account whose writeback keeps bouncing is worth seeing before it
		// exhausts its retries and dead-letters.
		acctLabel := acct.ID.String()
		if n := res.Retried + res.DeadLettered; n > 0 {
			m.WritebackFailuresTotal.WithLabelValues(acctLabel).Add(float64(n))
		}
		// Depth is read AFTER the drain, so it reports what is still owed rather
		// than what this pass was about to do. A failure here is logged and
		// dropped: a stale gauge must never fail a sync pass.
		if derr := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			n, e := tx.CountOutstandingIntents(ctx, acct.ID)
			if e != nil {
				return e
			}
			m.ActionQueueDepth.WithLabelValues(acctLabel).Set(float64(n))
			// Sampled here rather than set at send time so it resets at UTC
			// midnight without waiting for the day's first send — see
			// store.OutboundSentToday.
			sent, e := tx.OutboundSentToday(ctx, acct.ID)
			if e != nil {
				return e
			}
			m.OutboundLedgerLevel.WithLabelValues(acctLabel).Set(float64(sent))
			return nil
		}); derr != nil && ctx.Err() == nil {
			logger.Warn("per-account writeback/outbound gauges not updated; they are stale",
				"account", acctLabel, "err", derr)
		}
		return err
	}

	// One Runner serves every user: its lister/syncer/prepare are all keyed by the
	// user id passed to Run, so it is safe to drive concurrently for many users.
	runnerOpts := []syncrunner.Option{
		syncrunner.WithInterval(cfg.SyncInterval),
		syncrunner.WithMaxBackoff(cfg.SyncMaxBackoff),
		syncrunner.WithWriteback(drain),
		syncrunner.WithHealthRecorder(syncrunner.NewStoreHealthRecorder(st)),
		// mailroom#435: a sync-disabled account is an operator condition, and
		// a gauge is what makes it alertable rather than a log line that
		// scrolled past at boot.
		syncrunner.WithDisabledAccountsSink(func(userID ulid.ULID, n int) {
			m.SyncDisabledAccounts.WithLabelValues(userID.String()).Set(float64(n))
		}),
		syncrunner.WithLogger(logger),
		// IMAP IDLE (mailroom#103 Stage 2): near-instant delivery. Purely
		// additive — the scheduled poll above stays the fallback and safety net,
		// so a server without IDLE (or IDLE turned off) only loses the speedup.
		// The state sink drives the reserved per-account idle_connection_state
		// gauge (0 down / 1 connecting / 2 idling).
		syncrunner.WithIdle(cfg.IMAPIdle, cfg.IMAPIdleTimeout),
		syncrunner.WithIdleStateSink(func(accountID ulid.ULID, state float64) {
			m.IdleConnState.WithLabelValues(accountID.String()).Set(state)
		}),
		// How stale each account had become when its pass began (#524). For a
		// healthy account this sits at roughly the poll interval; for a stalled
		// one it climbs without bound, which is what an alert can key on.
		syncrunner.WithSyncLagSink(func(accountID ulid.ULID, seconds float64) {
			m.SyncLagSeconds.WithLabelValues(accountID.String()).Set(seconds)
		}),
		// Warm per-account sync connection pool (mailroom#103 Stage 3): reuse one
		// authenticated connection per account across passes instead of dialing
		// TCP+TLS+LOGIN every time. The per-account sync gate serializes passes, so
		// a single warm connection is safe; each reuse is NOOP-probed first, so a
		// dropped connection re-dials rather than skipping a sync.
		syncrunner.WithPool(cfg.IMAPPool, cfg.IMAPPoolIdleTimeout),
	}
	if cfg.IMAPIdle {
		logger.Info("IMAP IDLE enabled: new mail triggers a sync within seconds; polling remains the fallback",
			"reissue", cfg.IMAPIdleTimeout.String())
	} else {
		logger.Info("IMAP IDLE disabled: sync is poll-only", "env", config.EnvIMAPIdle)
	}
	if cfg.IMAPPool {
		logger.Info("IMAP sync connection pool enabled: one warm connection per account is reused across passes",
			"idle_timeout", cfg.IMAPPoolIdleTimeout.String())
	} else {
		logger.Info("IMAP sync connection pool disabled: each pass dials a fresh connection", "env", config.EnvIMAPPool)
	}
	// New-mail Web Push (#42, ADR-016): after a pass ingests mail, notify the user's
	// devices. The sender signs with the master-key-derived VAPID keypair; a
	// derivation failure only disables push, never sync — degrade, don't die.
	if vk, err := deriveVAPID(ctx, keys); err != nil {
		logger.Warn("new-mail Web Push disabled: could not derive the VAPID key", "err", err)
	} else {
		// ⚠ The push endpoint is a client-supplied URL, so this POST goes out
		// through the shared egress guard (ADR-026 §3a/§4 step 1), never a bare
		// http.Client. The policy is the default one — https only — because a Web
		// Push endpoint is always https and this caller must not inherit the
		// image proxy's http widening (decision A).
		sender := webpush.NewSender(vk, egress.New(egress.Policy{Timeout: 30 * time.Second}), "")
		runnerOpts = append(runnerOpts,
			syncrunner.WithPushNotifier(syncrunner.NewStorePushNotifier(st, sender, logger)))
	}
	runner := syncrunner.New(syncrunner.NewStoreLister(st), engine, resolver.Prepare, runnerOpts...)

	// A USER'S ACTION REACHES THE PROVIDER IN SECONDS, NOT AT THE NEXT POLL
	// (#581). Email/set enqueues an intent and returns; without this, nothing
	// carries it up until the account's next scheduled pass, because writeback
	// runs only as that pass's pre-sync hook. Measured before this existed: a
	// rescue from Junk reached Migadu 3m51s after the click.
	//
	// Installed HERE rather than at api.New because the handler is built before
	// the runner exists. The closure captures the PROCESS context on purpose:
	// a drain must not be cancelled when the HTTP response is written.
	if apiSvc != nil {
		apiSvc.SetWritebackNudge(func(userID ulid.ULID) { runner.NudgeWriteback(ctx, userID) })
	}
	// The same drain for a send that has just completed (#691) — see
	// compose.WithOnSent. Same process context, for the same reason.
	sentNudge.set(func(userID ulid.ULID) { runner.NudgeWriteback(ctx, userID) })

	tasks := []syncrunner.UserTask{
		// SYNC — only ever returns on context cancellation.
		func(ctx context.Context, userID ulid.ULID) {
			if err := runner.Run(ctx, userID); err != nil && ctx.Err() == nil {
				logger.Error("sync loop exited unexpectedly", "user_id", userID.String(), "err", err)
			}
		},
		// IDLE — the near-instant delivery watcher (mailroom#103 Stage 2). Runs
		// alongside SYNC and shares the Runner's per-account gate, so a triggered
		// pass never overlaps a scheduled one. Returns immediately when IDLE is
		// disabled (poll-only); otherwise only on context cancellation.
		func(ctx context.Context, userID ulid.ULID) {
			if err := runner.RunIdle(ctx, userID); err != nil && ctx.Err() == nil {
				logger.Error("idle watcher exited unexpectedly; sync falls back to polling",
					"user_id", userID.String(), "err", err)
			}
		},
		// IMAGE CACHE GAUGES (#922) — one-shot per user appearance, so the
		// referenced/unreferenced split has a real value from boot. Prefetched
		// images are bounded by no eviction; the gauge is how their growth is
		// seen. A no-op with the proxy off.
		func(ctx context.Context, userID ulid.ULID) {
			if apiSvc == nil {
				return
			}
			if err := apiSvc.ReportProxyCache(ctx, userID); err != nil && ctx.Err() == nil {
				logger.Warn("could not read the image cache size; its gauges stay unset until the next fetch",
					"user_id", userID.String(), "err", err)
			}
		},
		// PURGE SEED — one-shot per user appearance; idempotent across restarts.
		func(ctx context.Context, userID ulid.ULID) {
			if scheduled, err := purge.EnsureScheduled(ctx, st, userID, time.Now()); err != nil {
				if ctx.Err() == nil {
					logger.Error("could not seed the Trash purge job; Trash will not be reclaimed until the next successful schedule",
						"user_id", userID.String(), "err", err)
				}
			} else if scheduled {
				logger.Info("Trash purge scheduled", "user_id", userID.String(), "interval", cfg.PurgeInterval.String())
			}
		},
		// DRAWER SEED — one-shot per user appearance; idempotent across
		// restarts (#443). Seeds the user's first recurring drawer auto-archive
		// sweep so a fresh user's drawer starts ageing out from day one; the
		// handler then reschedules itself (registered in startDispatcher).
		func(ctx context.Context, userID ulid.ULID) {
			if scheduled, err := drawer.EnsureScheduled(ctx, st, userID, time.Now()); err != nil {
				if ctx.Err() == nil {
					logger.Error("could not seed the drawer sweep job; the drawer will not auto-archive until the next successful schedule",
						"user_id", userID.String(), "err", err)
				}
			} else if scheduled {
				logger.Info("drawer sweep scheduled", "user_id", userID.String())
			}
		},
		// CHANGE LOG COMPACTION SEED — one-shot per user appearance; idempotent
		// across restarts (mailroom#526, ADR-003). Seeds the user's first
		// recurring weekly compaction sweep so change_log does not grow
		// forever; the handler then reschedules itself (registered in
		// startDispatcher).
		func(ctx context.Context, userID ulid.ULID) {
			if scheduled, err := changelog.EnsureScheduled(ctx, st, userID, time.Now()); err != nil {
				if ctx.Err() == nil {
					logger.Error("could not seed the change log compaction job; change_log will grow unbounded until the next successful schedule",
						"user_id", userID.String(), "err", err)
				}
			} else if scheduled {
				logger.Info("change log compaction scheduled", "user_id", userID.String())
			}
		},
		// SNOOZE WAKE REPAIR — one-shot per user appearance. Normally the wake
		// chain is self-sustaining (each fired wake schedules the next), so this
		// schedules nothing. It exists for the case where the chain was BROKEN:
		// a dropped job, or a restore from a backup taken before the job row was
		// written. Without it, mail snoozed at that moment would sleep forever.
		// Idempotent — with no pending snooze it is a no-op.
		func(ctx context.Context, userID ulid.ULID) {
			if scheduled, err := snooze.EnsureScheduled(ctx, st, userID); err != nil {
				if ctx.Err() == nil {
					logger.Error("could not seed the snooze wake job; snoozed mail may not return on time",
						"user_id", userID.String(), "err", err)
				}
			} else if scheduled {
				logger.Info("snooze wake scheduled", "user_id", userID.String())
			}
		},
		// ML TRAINING SEED — one-shot per user appearance; idempotent across
		// restarts. Seeds the user's first recurring priority-model training pass
		// (spec §8, #33) so a fresh user starts learning without waiting a full
		// interval; the handler then reschedules itself (the global dispatcher runs
		// it, registered in startDispatcher).
		func(ctx context.Context, userID ulid.ULID) {
			if scheduled, err := ml.EnsureTrainingScheduled(ctx, st, userID, time.Now()); err != nil {
				if ctx.Err() == nil {
					logger.Error("could not seed the ML training job; the priority model will not learn until the next successful schedule",
						"user_id", userID.String(), "err", err)
				}
			} else if scheduled {
				logger.Info("ML priority training scheduled", "user_id", userID.String())
			}
		},
	}
	if scanner != nil {
		// ATTACHMENT RE-SCAN SEED (#395, ADR-027 §5c) — one-shot per user
		// appearance, idempotent across restarts, seeded only when a scanner
		// exists. The handler reschedules itself; the dispatcher runs it.
		//
		// ⚠ Seeded at now(), not now+interval, so the FIRST pass clears the
		// backlog of never-scanned attachments rather than waiting a day to
		// start. On an instance that had mail before the scanner did, that
		// backlog is the entire corpus.
		tasks = append(tasks, func(ctx context.Context, userID ulid.ULID) {
			if scheduled, err := rescan.EnsureScheduled(ctx, st, userID, time.Now()); err != nil {
				if ctx.Err() == nil {
					logger.Error("could not seed the attachment re-scan job; stored verdicts will age without being refreshed",
						"user_id", userID.String(), "err", err)
				}
			} else if scheduled {
				logger.Info("attachment re-scan scheduled", "user_id", userID.String())
			}
		})
	}
	if indexer != nil {
		// INDEX — only ever returns on context cancellation.
		tasks = append(tasks, func(ctx context.Context, userID ulid.ULID) {
			if err := indexer.Run(ctx, userID); err != nil && ctx.Err() == nil {
				logger.Error("search indexer exited unexpectedly; mail is unaffected, search is now stale",
					"user_id", userID.String(), "err", err)
			}
		})
	}

	sup := syncrunner.NewSupervisor(syncrunner.NewStoreUserLister(st), tasks,
		syncrunner.WithSupervisorLogger(logger))

	go func() {
		defer close(done)
		// Close the index once, AFTER the supervisor has stopped and every index
		// task has unwound (Run returned): the native backend is then quiescent.
		if indexer != nil {
			defer func() {
				if err := indexer.Close(); err != nil {
					logger.Warn("search: closing the index", "err", err)
				}
			}()
		}
		if err := sup.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("per-user fan-out exited unexpectedly", "err", err)
		}
	}()
	return done
}

// startDispatcher runs the durable-job dispatcher and returns a channel closed
// when it stops. It registers the job kinds 0.5 has:
//
//   - `submit`: the delayed send behind both Undo Send and Send Later
//     (spec §5/§16);
//   - `purge_trash`: the recurring per-user sweep that hard-deletes messages past
//     their Trash window, reclaims their blobs, and leaves an audit tombstone
//     (ADR-009). It is the ONLY path that ever hard-deletes a message, reachable
//     from no request surface — the seam that keeps agent deletes soft.
//   - `ml_train`: the recurring per-user priority-model training pass (spec §8,
//     #33) that learns from recent feedback and promotes a better model.
//
// It runs a catch-up pass immediately on boot, which is the whole point of a
// durable job: a send whose hold window expired — or a Trash purge that came
// due — while the process was restarting still happens.
//
// It is a SINGLE global worker: app.dequeue_due_jobs hands out due jobs across all
// users (the job carries its own user id), so the dispatcher is NOT fanned out per
// user. Only the per-user SEEDING of each user's first recurring purge is, and that
// lives in startUserFanout alongside the sync and index loops.
// dispatcherMetrics connects the job dispatcher's observability seam to the
// Prometheus registry (#524).
//
// ⚠ THE SEAM ALREADY EXISTED AND NOTHING EVER FILLED IT. jobs.Metrics and
// jobs.NopMetrics have been there since the dispatcher was written, jobs.Config
// defaults Metrics to NopMetrics{}, and no caller in cmd/ ever passed anything
// else — so every dispatcher signal was discarded before reaching the registry
// that exists to serve it. That is the shape #351 named: the mechanism is
// present, and absent exactly where it ships.
type dispatcherMetrics struct{ m *metrics.Metrics }

func (d dispatcherMetrics) DeadLettered(kind string) {
	d.m.JobDeadLettersTotal.WithLabelValues(kind).Inc()
}

func (d dispatcherMetrics) ObserveQueueDepth(n int64) {
	d.m.JobQueueDepth.Set(float64(n))
}

// ObserveClaimed is deliberately NOT the queue-depth signal, and the reason
// belongs here rather than in a commit message someone would have to find.
//
// The dispatcher claims at most BatchSize jobs per pass, so len(batch) is a
// SATURATION measure: it cannot exceed the batch size however deep the queue is.
// Feeding it to JobQueueDepth, whose help promises a count of due jobs, would be
// a metric fed by the wrong thing — worse than one that is absent, because it
// reads as authoritative and silently flattens at the batch size exactly when a
// backlog is worst.
//
// Real depth comes from app.count_due_jobs (migration 00054), reported through
// ObserveQueueDepth above. There is no separate saturation gauge yet; if one is
// wanted, it is a new metric rather than a second meaning for this one.
func (d dispatcherMetrics) ObserveClaimed(int) {}

// scannerVersionSink builds the rescan.VersionSink that feeds
// mailroom_scanner_signature_{version,published_timestamp_seconds} (#641).
// Two readers feed it: watchScannerVersion (at boot, then every
// scannerVersionInterval), which is what guarantees the gauges, and the
// re-scan worker's own read, which is the same answer at its own cadence.
func scannerVersionSink(m *metrics.Metrics) rescan.VersionSink {
	return func(v clamd.Version) {
		m.ScannerSignatureVersion.Set(float64(v.Database))
		// A zero Published means clamd did not say, and the timestamp goes to
		// zero WITH it rather than keeping an older database's date: the gauge
		// describes the database loaded now, and "unknown" must read as stale
		// (see ScannerSignaturePublishedTimestamp).
		var ts float64
		if !v.Published.IsZero() {
			ts = float64(v.Published.Unix())
		}
		m.ScannerSignaturePublishedTimestamp.Set(ts)
	}
}

// scannerVersionInterval is how often watchScannerVersion re-reads clamd's
// VERSION: often enough that a database clamd reloads shows within minutes,
// and one unix-socket round trip per interval costs nothing.
const scannerVersionInterval = 10 * time.Minute

// versionReader is the one clamd.Client method watchScannerVersion needs, so a
// test can drive it with a fake.
type versionReader interface {
	Version(ctx context.Context) (clamd.Version, error)
}

// watchScannerVersion reads the scanner's VERSION once IMMEDIATELY and then
// every interval until ctx ends, handing each answer to sink.
//
// ⚠ THE IMMEDIATE READ IS THE POINT (#641). v1.79.0 fed the gauges only from
// the re-scan worker, whose job is seeded at boot ONLY when none is pending.
// After a restart the pending job still fired at its old time, up to a day
// later, so both gauges read 0 until then. Seen straight after
// the v1.79.0 deploy: version 0, timestamp 0. A zero timestamp
// deliberately alerts (an unknown date is stale), so every deploy would have
// paged for up to a day.
//
// A failed read leaves the gauges as they were. That is honest here, unlike
// an age: the published timestamp is a fact about the loaded database, and
// `time() - ts` keeps growing while clamd is unreachable, so an outage long
// enough to matter still alerts. The ingest path already reports the outage
// itself; this logs it at DEBUG only, so it is not said twice.
func watchScannerVersion(ctx context.Context, r versionReader, sink rescan.VersionSink, interval time.Duration, logger *slog.Logger) {
	read := func() {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		v, err := r.Version(rctx)
		if err != nil {
			logger.Debug("scanner version read failed; signature gauges keep their last value", "err", err)
			return
		}
		sink(v)
	}
	read()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			read()
		}
	}
}

// submissionSender is the SMTP submission client the send worker uses, built
// from config in ONE place so the account-host policy it applies is the same
// switch account validation reads (#625 §3). Its zero value would be the
// guarded one; the field is set explicitly so MAILROOM_ALLOW_PRIVATE_ACCOUNT_
// HOSTS reaches the submission dial and not only the validation check —
// without it, an operator whose mail server is on the LAN could add the
// account and then never send from it.
func submissionSender(cfg *config.Config) provider.Sender {
	return provider.SMTPSender{AllowPrivateHosts: cfg.AllowPrivateAccountHosts}
}

func startDispatcher(ctx context.Context, st *store.Store, blobs blob.BlobStore, keys crypto.KeyProvider, composer *compose.Composer, sender provider.Sender, scanner *clamd.Client, haveBlobs bool, purgeInterval, changeLogRetention time.Duration, sentNudge *writebackNudgeHook, oof *outofoffice.Manager, oa oauthWiring, apiSvc *api.API, m *metrics.Metrics, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	if !haveBlobs {
		logger.Error("job dispatcher disabled: blob store unavailable, so scheduled sends cannot read their message bytes and Trash cannot be purged")
		close(done)
		return done
	}

	// tokens: OAuth accounts send with XOAUTH2 from the shared Manager (#52);
	// nil (an untyped nil interface) when no provider is configured.
	worker := compose.NewSendWorker(st, blobs, &keyResolver{keys: keys, tokens: oa.tokens}, sender, logger,
		compose.WithOnSent(sentNudge.fire))
	purgeWorker := purge.NewWorker(st, blobs, purgeInterval, logger)
	// T1 priority scoring's LEARNING loop (spec §8, #33): a per-user batch that
	// trains a logistic model on recent feedback and promotes it only if it ranks
	// better. It reads/writes model artifacts through the SAME encrypted blob store
	// + derived DEK the serve-time registry uses; a daily cadence is set inside the
	// worker (defaultTrainInterval), so no config knob is threaded here.
	trainWorker := ml.NewTrainWorker(st, ml.NewRegistry(st, blobs, keys), 0, logger)
	// Snooze wake-ups (spec §6, #51). No config knob and no interval: unlike the
	// purge sweep this is not periodic — each wake is scheduled for its own
	// message's time, and the handler schedules the next one from the data.
	snoozeWorker := snooze.NewWorker(st, logger)
	// The drawer auto-archive sweep (#443, Direction 7): recurring per-user,
	// the same shape as the purge sweep. No config knob, like snooze — the
	// cadence is fixed inside the worker (drawer.defaultInterval) and the
	// per-area period lives in desk_preferences/areas instead.
	drawerWorker := drawer.NewWorker(st, 0, logger)
	// change_log compaction (mailroom#526, ADR-003): recurring per-user, same
	// shape as drawer and purge. The cadence is fixed inside the worker
	// (changelog.defaultInterval, weekly); retention is the one config knob
	// (MAILROOM_CHANGE_LOG_RETENTION), threaded through from cfg.
	changelogWorker := changelog.NewWorker(st, changeLogRetention, 0, logger)
	// DAY-ONE SETUP (#446): one-shot per run, NOT recurring and deliberately
	// NOT seeded at boot — a run is enqueued by Setup/apply and its reversal by
	// Setup/undo. There is no EnsureScheduled call beside drawer's above for
	// exactly that reason: a day-one wizard that rescheduled itself would run
	// again on a mailbox that had already been set up.
	setupWorker := setup.NewWorker(st, logger)
	d := jobs.New(jobs.StoreQueue{Store: st}, jobs.Config{Logger: logger, Metrics: dispatcherMetrics{m: m}})
	// ATTACHMENT RE-SCAN (#395, ADR-027 §5c). Registered only when a scanner is
	// configured — with none, there is nothing to re-scan against and a handler
	// would wake every day to do nothing.
	//
	// ⚠ It reads message bytes through the COMPOSER, not through a reader built
	// here. The composer already holds the blob store and the derived mail-blob
	// DEK; a second key path for "the re-scan side" is exactly the drift that
	// would silently decrypt mail with the wrong key (the same reasoning as
	// writebackBlobs, whose Raw method this reuses verbatim).
	if scanner != nil && composer != nil {
		d.Register(rescan.JobKind, rescan.New(st, writebackBlobs{composer}, scanner, 0, logger, scannerVersionSink(m)).Handle)
	}
	d.Register(compose.SubmitJobKind, worker.Handle)
	d.Register(purge.JobKind, purgeWorker.Handle)
	d.Register(ml.TrainJobKind, trainWorker.Handle)
	d.Register(store.SnoozeJobKind, snoozeWorker.Handle)
	d.Register(drawer.JobKind, drawerWorker.Handle)
	d.Register(setup.JobKind, setupWorker.Handle)
	d.Register(changelog.JobKind, changelogWorker.Handle)
	// Out of office (#50): one-shot timers at the window's start and end, plus
	// a daily refresh for a contacts-only reply. Enqueued by OutOfOffice/set
	// and by the handler itself; nothing is seeded at boot.
	if oof != nil {
		d.Register(outofoffice.JobKind, oof.HandleJob)
	}
	// The one Web Push an OAuth grant owes when it starts needing its user to
	// reconnect (#52): queued in the transaction that makes the transition.
	d.Register(oauthmail.ReauthNotifyJobKind, reauthNotifyHandler(ctx, st, keys, logger))
	// The image prefetch at arrival (#922): queued by ingest, one job per
	// allowed sender's new message. Registered whenever the API exists, even
	// with prefetch off: a job queued before a restart that turned it off then
	// completes as "disabled" instead of retrying into the dead-letter queue.
	if apiSvc != nil {
		d.Register(store.ProxyPrefetchJobKind, apiSvc.HandleProxyPrefetch)
	}

	go func() {
		defer close(done)
		if err := d.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("job dispatcher exited unexpectedly", "err", err)
		}
	}()
	return done
}

// writebackBlobs adapts the composer to writeback.BlobReader: same blob store,
// same derived mail-blob DEK, one key path. The method rename is the only thing
// this type does — deliberately, so there is nowhere for a second, drifting
// implementation to grow.
type writebackBlobs struct{ c *compose.Composer }

var _ writeback.BlobReader = writebackBlobs{}

func (w writebackBlobs) Raw(ctx context.Context, userID ulid.ULID, contentHash []byte) ([]byte, error) {
	return w.c.ReadRaw(ctx, userID, contentHash)
}

// keyResolver supplies compose with the per-user secrets it needs, without that
// package having to know anything about the key hierarchy or the credential
// envelope format (compose.Keys).
//
// It derives the SAME mail-blob DEK the sync path writes with
// (syncrunner.MailBlobInfoV1 / MailBlobSchemeV1) — deliberately reusing those
// constants rather than restating them, because a drift between "the key ingest
// encrypts a blob with" and "the key compose reads it back with" would be a
// silent, unrecoverable corruption of the user's mail.
type keyResolver struct {
	keys crypto.KeyProvider
	// tokens issues OAuth access tokens (ADR-046); nil until the connect flow
	// configures providers, which is the state in which no OAuth account exists.
	tokens syncrunner.AccessTokens
}

var _ compose.Keys = (*keyResolver)(nil)

func (k *keyResolver) MailDEK(ctx context.Context, userID ulid.ULID) ([]byte, ulid.ULID, error) {
	master, err := k.keys.MasterKey(ctx)
	if err != nil {
		return nil, ulid.ULID{}, fmt.Errorf("main: master key: %w", err)
	}
	defer crypto.Zero(master)

	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	if err != nil {
		return nil, ulid.ULID{}, fmt.Errorf("main: derive mail dek: %w", err)
	}
	return dek, syncrunner.MailBlobSchemeV1, nil
}

// SealAccountCredentials seals a mail-account secret server-side for the in-UI
// account surface (api.AccountSecrets, #98/#45). It is the SAME derivation the
// boot onboarding uses — onboarding.SealCredentials, which SMTPAuth/sync open
// with onboarding.OpenCredentials — so there is exactly one credentials-key path,
// not a second one that could drift.
func (k *keyResolver) SealAccountCredentials(ctx context.Context, userID ulid.ULID, creds onboarding.Credentials) ([]byte, ulid.ULID, error) {
	master, err := k.keys.MasterKey(ctx)
	if err != nil {
		return nil, ulid.ULID{}, fmt.Errorf("main: master key: %w", err)
	}
	defer crypto.Zero(master)
	ct, err := onboarding.SealCredentials(master, userID, creds)
	if err != nil {
		return nil, ulid.ULID{}, fmt.Errorf("main: seal account credentials: %w", err)
	}
	return ct, onboarding.CredentialsSchemeV1, nil
}

// SMTPAuth opens the account's sealed credentials for one submission. The
// plaintext lives only for the duration of the send and is never logged.
//
// An OAuth account (ADR-046) authenticates with XOAUTH2 and a current access
// token from k.tokens. k.tokens is nil when no provider is configured; an
// OAuth account then fails its send here with a cause the user can read,
// never falling back to a password it does not have.
func (k *keyResolver) SMTPAuth(ctx context.Context, userID ulid.ULID, acct store.Account) (compose.SMTPLogin, error) {
	master, err := k.keys.MasterKey(ctx)
	if err != nil {
		return compose.SMTPLogin{}, fmt.Errorf("main: master key: %w", err)
	}
	defer crypto.Zero(master)

	creds, err := onboarding.OpenCredentials(master, userID, acct.CredentialsCT)
	if err != nil {
		return compose.SMTPLogin{}, fmt.Errorf("main: open credentials for account %s: %w", acct.ID, err)
	}
	if creds.OAuth != nil {
		if k.tokens == nil {
			return compose.SMTPLogin{}, fmt.Errorf("main: account %s signs in with OAuth, which is not configured on this server", acct.ID)
		}
		// The server is the PROVIDER's, never the row's (ADR-046 §1), and a
		// row that disagrees is refused, not corrected.
		hosts, ok := k.tokens.HostsFor(creds.OAuth.Provider)
		if !ok {
			return compose.SMTPLogin{}, fmt.Errorf("main: account %s: OAuth provider %q is not configured on this server",
				acct.ID, creds.OAuth.Provider)
		}
		if err := syncrunner.PinOAuthHost(userID, acct.ID, creds.OAuth.Provider, "smtp", acct.SMTPHost, acct.SMTPPort,
			hosts.SMTPHost, hosts.SMTPPort); err != nil {
			return compose.SMTPLogin{}, fmt.Errorf("main: %w", err)
		}
		// The token is asked for when the conversation authenticates, and a
		// 535 to it goes back through the source's Reject before the one
		// retry (ADR-046 §9 obligation (a), provider.SMTPSender).
		return compose.SMTPLogin{Auth: provider.XOAuth2Auth(creds.IMAPUsername, k.tokens.TokenSource(userID, acct.ID)),
			Host: hosts.SMTPHost, Port: hosts.SMTPPort, ImplicitTLS: hosts.SMTPImplicitTLS}, nil
	}
	user, pass := creds.SMTPUsername, creds.SMTPPassword
	if user == "" {
		// One login for both (the common case): the IMAP credentials are the
		// SMTP ones (onboarding.Credentials).
		user, pass = creds.IMAPUsername, creds.IMAPPassword
	}
	return compose.PasswordLogin(acct, user, pass), nil
}

// ProbeToken obtains a current access token for an OAuth account without
// sending anything (#52): the send worker's operator_error hold asks it, so a
// held submission notices the operator's fix even on an account no sync pass
// refreshes. The token itself is dropped here; the Manager caches it.
func (k *keyResolver) ProbeToken(ctx context.Context, userID ulid.ULID, acct store.Account) error {
	if k.tokens == nil {
		return fmt.Errorf("main: account %s: OAuth is not configured on this server: %w", acct.ID, xoauth2.ErrProviderMisconfigured)
	}
	_, err := k.tokens.TokenSource(userID, acct.ID).Token(ctx)
	return err
}

// --- Search ------------------------------------------------------------------

// openSearch builds the search service and its indexer, or (nil, nil) when
// search is off. EVERY failure here degrades rather than aborts: the index is a
// disposable cache (spec §7), so a Mailroom that cannot open one still serves
// mail. It returns nil rather than an error for exactly that reason.
//
// Backend selection is honest about what it got. The Tantivy engine is behind a
// build tag (CGO_ENABLED=1 -tags tantivy, ADR-006); a binary built without it
// falls back to an in-memory index, which is correct but is rebuilt from
// Postgres on every start — fine for a dev instance, wrong for a real one, so it
// says so loudly rather than quietly performing badly at scale.
//
// The index holds the plaintext of every message, so its content key MUST be the
// same derived mail-blob DEK the sync path writes blobs under — hence
// keyResolver.MailDEK rather than a second derivation that could silently drift
// and decrypt garbage.
// indexLagTracker publishes the WORST per-user search-index lag as the single
// global mailroom_index_cursor_lag_seconds gauge (#524). Indexing runs per user
// on its own goroutine, so reports arrive concurrently and out of order; the map
// keeps the latest figure for each and the gauge always shows the maximum.
//
// A user whose index has caught up reports 0 and stays in the map at 0, which is
// what lets the gauge come back down. Dropping caught-up users instead would
// make the maximum jump around as users drained.
type indexLagTracker struct {
	mu     stdsync.Mutex
	m      *metrics.Metrics
	byUser map[ulid.ULID]float64
}

func (t *indexLagTracker) set(userID ulid.ULID, seconds float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byUser[userID] = seconds
	worst := 0.0
	for _, v := range t.byUser {
		if v > worst {
			worst = v
		}
	}
	t.m.IndexCursorLagSeconds.Set(worst)
}

func openSearch(ctx context.Context, st *store.Store, blobs blob.BlobStore, keys crypto.KeyProvider, cfg *config.Config, haveBlobs bool, m *metrics.Metrics, logger *slog.Logger) (*search.Service, *search.Indexer) {
	if cfg.SearchDisabled {
		logger.Warn("search is disabled (" + config.EnvSearchDisabled + "); Email/query will reject text filters")
		return nil, nil
	}
	if !haveBlobs {
		logger.Error("search disabled: the blob store is unavailable, so message bodies cannot be read")
		return nil, nil
	}

	open := tantivy.Open
	if tantivy.Available() {
		logger.Info("search backend: tantivy", "root", cfg.SearchRoot)
	} else {
		open = search.MemoryFactory
		logger.Warn("search backend: IN-MEMORY — this binary was built without the Tantivy engine, "+
			"so the index is held in RAM and rebuilt from Postgres on every start",
			"hint", "build with CGO_ENABLED=1 -tags tantivy for the persistent engine")
	}

	kr := &keyResolver{keys: keys}
	content := search.NewBlobContent(blobs, func(ctx context.Context, userID ulid.ULID) ([]byte, error) {
		dek, _, err := kr.MailDEK(ctx, userID)
		return dek, err
	})

	// mailroom_index_cursor_lag_seconds is a single global gauge while indexing
	// is per-user, so the tracker keeps each user's lag and publishes the WORST
	// of them — "is the search index keeping up" is answered by the user it is
	// furthest behind for, not by an average that a healthy user would flatter.
	lag := &indexLagTracker{m: m, byUser: map[ulid.ULID]float64{}}
	ix, err := search.NewIndexer(cfg.SearchRoot, open, search.NewStoreSource(st), content,
		search.WithLogger(logger), search.WithInterval(cfg.IndexInterval),
		search.WithSyncSink(func(userID ulid.ULID, p search.Progress) {
			var seconds float64
			if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
				at, behind, e := tx.OldestUnconsumedEmailChangeAt(ctx, p.Cursor)
				if e != nil {
					return e
				}
				if behind {
					seconds = time.Since(at).Seconds()
				}
				return nil
			}); err != nil {
				if ctx.Err() == nil {
					logger.Warn("search: index-lag read failed; the gauge is stale",
						"user_id", userID.String(), "err", err)
				}
				return
			}
			lag.set(userID, seconds)
		}))
	if err != nil {
		logger.Error("search disabled: cannot open the index root", "root", cfg.SearchRoot, "err", err)
		return nil, nil
	}
	return search.NewService(ix, search.NewStoreResolver(st),
		search.WithSearchTiming(searchTimingObserver(m))), ix
}

// deriveVAPID reconstructs the Web Push application keypair from the master key
// (ADR-016). It is deterministic, so the session advertiser and the sync-loop
// sender derive the same keypair independently without sharing state.
func deriveVAPID(ctx context.Context, keys crypto.KeyProvider) (*webpush.VAPIDKey, error) {
	if keys == nil {
		return nil, errors.New("no key provider")
	}
	master, err := keys.MasterKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	defer crypto.Zero(master)
	return webpush.DeriveVAPIDKey(master)
}

// vapidPublicKey is the base64url VAPID applicationServerKey advertised in the
// JMAP session so a browser can subscribe against this server.
func vapidPublicKey(ctx context.Context, keys crypto.KeyProvider) (string, error) {
	key, err := deriveVAPID(ctx, keys)
	if err != nil {
		return "", err
	}
	return key.PublicKeyBase64URL(), nil
}

// openBlobStore builds the FSBlobStore the sync path ingests into. Its per-user
// path key is derived from the master key (blobPathInfo), so no second secret is
// required — and it must derive identically in cmd/mailroom-admin, which shares
// the constant.
func openBlobStore(ctx context.Context, keys crypto.KeyProvider, root string) (blob.BlobStore, error) {
	master, err := keys.MasterKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	defer crypto.Zero(master)

	pathKey, err := crypto.DeriveKey(master, nil, blobPathInfo)
	if err != nil {
		return nil, fmt.Errorf("derive blob path key: %w", err)
	}
	defer crypto.Zero(pathKey)

	blobs, err := blob.NewFSBlobStore(root, pathKey)
	if err != nil {
		return nil, fmt.Errorf("open blob store: %w", err)
	}
	return blobs, nil
}

// trustedAuthservIDs reads config.EnvAuthservIDs as a comma-separated list.
//
// ⚠ EMPTY IS A MEANINGFUL VALUE, NOT A MISSING ONE (#414). It says "believe no
// Authentication-Results header for a security decision", which makes selfSpoof
// fire on own-domain mail rather than trust a header the sender could have
// written. There is deliberately no way to configure this into silence.
func trustedAuthservIDs() []string {
	return config.TrustedAuthservIDs()
}
