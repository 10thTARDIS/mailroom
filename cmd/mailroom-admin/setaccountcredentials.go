// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/onboarding"
	"mailroom/internal/provider"
	"mailroom/internal/store"
)

// mailroom-admin set-account-credentials (mailroom#571): the automation path
// for a password rotation that `accounts-report` can SEE (`password: DIFFERS`)
// but that boot deliberately never applies (#538 — a redeploy must never re-dial
// a working account and newly break it). Until this existed the ONLY way to
// apply a rotation was the web client's Account settings, i.e. JMAP
// `Account/set` as a connection change (internal/api/account.go
// prepareAccountUpdate): re-enter the username/password, validate them by
// dialing IMAP, and re-seal only on success. This command is that same
// operation from the command line, for a deploy pipeline or an operator who
// only has this CLI — it calls the EXACT SAME two primitives
// (onboarding.ValidateAccount, onboarding.SealCredentials) in the same order,
// rather than a second implementation of either.
//
// A stored credential is never touched on a rejection: the dial happens FIRST,
// against the CANDIDATE secret, entirely before anything is sealed or written.

// runSetAccountCredentials parses flags, resolves the one permitted credential
// source, and delegates to setAccountCredentials for everything that touches
// the database or the network.
func runSetAccountCredentials(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("set-account-credentials", flag.ContinueOnError)
	var (
		userIDStr        = fs.String("user-id", "", "operator user id (ULID)")
		accountIDStr     = fs.String("account-id", "", "account id (ULID; see list-accounts)")
		passwordFile     = fs.String("password-file", "", "file holding the new password (Docker-secret style)")
		fromAccountsFile = fs.Bool("from-accounts-file", false, "read the new username/password from the accounts file (with --account)")
		accountName      = fs.String("account", "", "account name to look up in the accounts file (with --from-accounts-file)")
		accountsFile     = fs.String("accounts-file", "", "path to the accounts file (default: MAILROOM_ACCOUNTS_FILE; with --from-accounts-file)")
		smtpToo          = fs.Bool("smtp-too", false, "with --password-file: write the new password to the SMTP login as well, even when the stored SMTP login differs from the IMAP one (default: keep a separate SMTP login unchanged)")
		username         = fs.String("username", "", "override the IMAP/SMTP username (default: keep the stored one, or the file's declared username with --from-accounts-file)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	accountID, err := ulid.Parse(*accountIDStr)
	if err != nil {
		return fmt.Errorf("set-account-credentials: --account-id: %w", err)
	}
	source, err := parseCredentialSource(*passwordFile, *fromAccountsFile, *accountName)
	if err != nil {
		return err
	}

	if *smtpToo && source.kind != credentialSourcePasswordFile {
		return errors.New("set-account-credentials: --smtp-too is only used with --password-file (--from-accounts-file already writes the file's own SMTP login)")
	}

	pending, err := resolvePendingCredential(source, *username, *accountsFile)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	return setAccountCredentials(ctx, os.Stdout, d.store, d.keys, nil, d.allowPrivateAccountHosts, userID, accountID, pending, setCredentialsOptions{SMTPToo: *smtpToo})
}

// credentialSourceKind is which of the two mutually exclusive ways
// set-account-credentials was told to get the new secret.
type credentialSourceKind int

const (
	credentialSourceNone credentialSourceKind = iota
	credentialSourcePasswordFile
	credentialSourceAccountsFile
)

// credentialSource is the parsed (but not yet read) description of where the
// new credential comes from — kept separate from file I/O so the flag-shape
// validation is a pure function a unit test can drive with no disk access.
type credentialSource struct {
	kind         credentialSourceKind
	passwordFile string
	accountName  string
}

// parseCredentialSource enforces "exactly one password source" (mailroom#571):
// either --password-file alone, or --from-accounts-file paired with --account
// NAME. It performs no I/O, so a flag-shape mistake is reported before this
// command ever opens a file, touches the database, or dials anything.
func parseCredentialSource(passwordFile string, fromAccountsFile bool, accountName string) (credentialSource, error) {
	passwordFile = strings.TrimSpace(passwordFile)
	accountName = strings.TrimSpace(accountName)
	havePasswordFile := passwordFile != ""
	switch {
	case havePasswordFile && fromAccountsFile:
		return credentialSource{}, errors.New("set-account-credentials: --password-file and --from-accounts-file are mutually exclusive")
	case havePasswordFile:
		if accountName != "" {
			return credentialSource{}, errors.New("set-account-credentials: --account is only used with --from-accounts-file")
		}
		return credentialSource{kind: credentialSourcePasswordFile, passwordFile: passwordFile}, nil
	case fromAccountsFile:
		if accountName == "" {
			return credentialSource{}, errors.New("set-account-credentials: --from-accounts-file requires --account NAME")
		}
		return credentialSource{kind: credentialSourceAccountsFile, accountName: accountName}, nil
	default:
		return credentialSource{}, errors.New("set-account-credentials: exactly one of --password-file or --from-accounts-file is required")
	}
}

// pendingCredential is the new secret, resolved from its source but not yet
// compared against what is stored or validated against IMAP. Username empty
// means "keep the account's current IMAP username" (only reachable from
// --password-file: --from-accounts-file always supplies one).
// SMTPUsername/SMTPPassword empty mean "mirror Username/Password" — resolved
// in setAccountCredentials, once Username's own default (which needs the
// stored row) is known — except that, for a stored account whose SMTP login
// differs from its IMAP login, they mean "keep the stored SMTP pair" unless
// setCredentialsOptions.SMTPToo (#655).
type pendingCredential struct {
	Username     string
	Password     string
	SMTPUsername string
	SMTPPassword string
}

// resolvePendingCredential does the source's file I/O — reading the password
// file, or loading and searching the accounts file — never touching the
// database. accountsFileFlag is the --accounts-file override; empty means
// MAILROOM_ACCOUNTS_FILE via config.Load, mirroring accounts-report's own
// fallback (cmd/mailroom-admin/accounts_report.go).
func resolvePendingCredential(source credentialSource, usernameOverride, accountsFileFlag string) (pendingCredential, error) {
	switch source.kind {
	case credentialSourcePasswordFile:
		pass, err := resolveSecretFlag("", source.passwordFile)
		if err != nil {
			return pendingCredential{}, fmt.Errorf("set-account-credentials: password: %w", err)
		}
		if pass == "" {
			return pendingCredential{}, errors.New("set-account-credentials: --password-file is empty")
		}
		// SMTPUsername/SMTPPassword are left empty here. In
		// setAccountCredentials they mean: when the stored SMTP login equals
		// the stored IMAP login (the common one-login case, and all that
		// Account/set can express — internal/api/account.go
		// credentialsFromSpec), mirror Username/Password; when it DIFFERS,
		// keep the stored SMTP pair unless --smtp-too (#655).
		return pendingCredential{Username: strings.TrimSpace(usernameOverride), Password: pass}, nil

	case credentialSourceAccountsFile:
		path := strings.TrimSpace(accountsFileFlag)
		if path == "" {
			cfg, err := config.Load()
			if err != nil {
				return pendingCredential{}, fmt.Errorf("set-account-credentials: %w", err)
			}
			path = cfg.AccountsFile
		}
		declared, err := config.LoadAccountsFile(path)
		if err != nil {
			return pendingCredential{}, fmt.Errorf("set-account-credentials: %w", err)
		}
		da, ok := findDeclaredAccount(declared, source.accountName)
		if !ok {
			return pendingCredential{}, fmt.Errorf("set-account-credentials: account %q not found in %s", source.accountName, path)
		}
		user := strings.TrimSpace(usernameOverride)
		if user == "" {
			user = da.Username
		}
		if user == "" {
			return pendingCredential{}, fmt.Errorf("set-account-credentials: account %q in %s declares no username", source.accountName, path)
		}
		pass := da.Password.Reveal()
		if pass == "" {
			return pendingCredential{}, fmt.Errorf("set-account-credentials: account %q in %s declares no password", source.accountName, path)
		}
		// smtp_username/smtp_password: empty in the file means "reuse the IMAP
		// ones" — the SAME fallback compareOneAccount applies when comparing a
		// declared account against a stored row (accounts_report.go), so a file
		// that legitimately leaves them blank rotates to exactly what
		// accounts-report already expects a converged account to hold.
		smtpUser := da.SMTPUsername
		if smtpUser == "" {
			smtpUser = user
		}
		smtpPass := da.SMTPPassword.Reveal()
		if smtpPass == "" {
			smtpPass = pass
		}
		return pendingCredential{Username: user, Password: pass, SMTPUsername: smtpUser, SMTPPassword: smtpPass}, nil

	default:
		return pendingCredential{}, errors.New("set-account-credentials: no credential source resolved")
	}
}

// findDeclaredAccount looks an account up by name in a loaded accounts file
// (config.LoadAccountsFile's output already rejects duplicate names).
func findDeclaredAccount(declared []config.Account, name string) (config.Account, bool) {
	for _, a := range declared {
		if a.Name == name {
			return a, true
		}
	}
	return config.Account{}, false
}

// setAccountCredentials is the testable core: validate the candidate secret
// against IMAP FIRST, seal it only if the server accepts it, and write it
// through store.UserTx.UpdateAccount — the SAME store method Account/set's own
// connection change calls — so the change_log invariant (CLAUDE.md) is
// whatever that method already guarantees, not a second implementation of it.
//
// dial is threaded through unchanged to onboarding.ValidateAccount: nil in
// production (which then uses the real sync.Dial, exactly like
// cmd/mailroom-admin's enable-account/validateStoredAccount), or a fake IMAP
// dialer in a test — mirroring internal/api/account_integration_test.go's
// WithAccountDialer seam, the established way this codebase proves an
// account-credential path without a live IMAP server.
func setAccountCredentials(ctx context.Context, out io.Writer, st *store.Store, keys crypto.KeyProvider, dial onboarding.Dialer, allowPrivate bool, userID, accountID ulid.ULID, pending pendingCredential, opts setCredentialsOptions) error {
	var acct store.Account
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		acct, e = tx.GetAccount(ctx, accountID)
		return e
	}); err != nil {
		return fmt.Errorf("set-account-credentials: %w", err)
	}

	master, err := keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("set-account-credentials: master key: %w", err)
	}
	defer crypto.Zero(master)
	stored, err := onboarding.OpenCredentials(master, userID, acct.CredentialsCT)
	if err != nil {
		return fmt.Errorf("set-account-credentials: open stored credentials: %w", err)
	}

	user := pending.Username
	if user == "" {
		user = stored.IMAPUsername
	}

	// ⚠ A SEPARATE STORED SMTP LOGIN IS KEPT (#655). --password-file carries one
	// password and no SMTP fields (pending.SMTP* empty). Mirroring it into SMTP
	// is right when the stored SMTP login IS the IMAP login, but for an account
	// whose SMTP login differs it would silently replace a login this command
	// was never told about — and the password it wrote there is the IMAP one.
	// So that case keeps the stored SMTP pair unless --smtp-too.
	fromPasswordOnly := pending.SMTPUsername == "" && pending.SMTPPassword == ""
	smtpKept := fromPasswordOnly && !opts.SMTPToo && stored.SMTPUsername != stored.IMAPUsername

	smtpUser := pending.SMTPUsername
	smtpPass := pending.SMTPPassword
	if smtpKept {
		smtpUser, smtpPass = stored.SMTPUsername, stored.SMTPPassword
	} else {
		if smtpUser == "" {
			smtpUser = user
			// --smtp-too on a separate login: the new PASSWORD goes to the SMTP
			// login, but its USERNAME stays the stored one.
			if fromPasswordOnly && stored.SMTPUsername != stored.IMAPUsername {
				smtpUser = stored.SMTPUsername
			}
		}
		if smtpPass == "" {
			smtpPass = pending.Password
		}
	}
	next := onboarding.Credentials{
		IMAPUsername: user, IMAPPassword: pending.Password,
		SMTPUsername: smtpUser, SMTPPassword: smtpPass,
	}

	// The report comes BEFORE any dial or write: which of the two logins this
	// run will change. Never a username or password, only the verdict.
	if smtpKept {
		_, _ = fmt.Fprintf(out, "account %s (%s): will change IMAP; SMTP login KEPT unchanged (it differs from the IMAP login; pass --smtp-too to write the new password there as well)\n", acct.ID, acct.Name)
	} else {
		_, _ = fmt.Fprintf(out, "account %s (%s): will change IMAP and SMTP\n", acct.ID, acct.Name)
	}

	// ⚠ VALIDATE BEFORE ANYTHING IS SEALED OR WRITTEN (mailroom#571). A
	// rejected credential must leave the stored one byte-identical — the
	// negative control this command exists to satisfy — so the dial happens
	// against the CANDIDATE secret first, and sealing/UpdateAccount are only
	// reached past a `return` on failure.
	spec := onboarding.AccountSpec{
		Name: acct.Name, ProviderProfile: acct.ProviderProfile,
		IMAPHost: acct.IMAPHost, IMAPPort: acct.IMAPPort,
		SMTPHost: acct.SMTPHost, SMTPPort: acct.SMTPPort,
		Username: next.IMAPUsername, Password: next.IMAPPassword,
		TLS: onboarding.TLSImplicit,
	}
	if _, err := onboarding.ValidateAccount(ctx, dial, spec, onboarding.WithAllowPrivateHosts(allowPrivate)); err != nil {
		return accountValidationError(acct.IMAPHost, err)
	}

	// An SMTP credential that is about to be sealed is checked too, by an
	// AUTH-only dial against the account's own SMTP host, before anything is
	// sealed — a bad SMTP password would otherwise be stored and only found by
	// the first send (#655). Nothing is sent. A kept SMTP pair is not re-checked:
	// it is not being written.
	if !smtpKept {
		if err := checkSMTPCredential(ctx, opts.CheckSMTP, allowPrivate, acct, next); err != nil {
			return err
		}
	}

	if credentialsEqual(stored, next) {
		_, _ = fmt.Fprintf(out, "account %s (%s): the validated credential already matches what is stored — nothing to write\n", acct.ID, acct.Name)
		return nil
	}

	ct, err := onboarding.SealCredentials(master, userID, next)
	if err != nil {
		return fmt.Errorf("set-account-credentials: seal: %w", err)
	}

	patch := store.AccountPatch{Connection: &store.AccountConnection{
		ProviderProfile: acct.ProviderProfile,
		IMAPHost:        acct.IMAPHost, IMAPPort: acct.IMAPPort,
		SMTPHost: acct.SMTPHost, SMTPPort: acct.SMTPPort,
		CredentialsCT:  ct,
		CredentialsDEK: onboarding.CredentialsSchemeV1,
	}}
	var found bool
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		found, e = tx.UpdateAccount(ctx, accountID, patch)
		return e
	}); err != nil {
		return fmt.Errorf("set-account-credentials: %w", err)
	}
	if !found {
		return fmt.Errorf("set-account-credentials: account %s not found", accountID)
	}
	if smtpKept {
		_, _ = fmt.Fprintf(out, "account %s (%s): IMAP credentials validated against %s and rotated; SMTP login kept\n", acct.ID, acct.Name, acct.IMAPHost)
	} else {
		_, _ = fmt.Fprintf(out, "account %s (%s): IMAP credentials validated against %s and SMTP credentials validated against %s; both rotated\n", acct.ID, acct.Name, acct.IMAPHost, acct.SMTPHost)
	}
	return nil
}

// setCredentialsOptions are the knobs setAccountCredentials takes beyond the
// credential itself.
type setCredentialsOptions struct {
	// SMTPToo writes the new password to the SMTP login even when the stored
	// SMTP login differs from the IMAP one (--smtp-too).
	SMTPToo bool
	// CheckSMTP is the AUTH-only SMTP check; nil means the real, egress-guarded
	// provider.SMTPSender.CheckAuth. A test substitutes a fake, as dial does for
	// IMAP.
	CheckSMTP smtpChecker
}

// smtpChecker authenticates to an SMTP server and sends nothing.
type smtpChecker func(ctx context.Context, req provider.SendRequest) error

// checkSMTPCredential is the SMTP counterpart of onboarding.ValidateAccount in
// setAccountCredentials: an AUTH-only dial of the account's SMTP host with the
// candidate SMTP login, through the same egress guard (allowPrivate is
// MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS). It touches only the host and the
// server's own error text, never the username or password.
func checkSMTPCredential(ctx context.Context, check smtpChecker, allowPrivate bool, acct store.Account, next onboarding.Credentials) error {
	if check == nil {
		check = provider.SMTPSender{AllowPrivateHosts: allowPrivate}.CheckAuth
	}
	port := acct.SMTPPort
	if port == 0 {
		port = 465
	}
	err := check(ctx, provider.SendRequest{
		Host: acct.SMTPHost, Port: port,
		Username: next.SMTPUsername, Password: next.SMTPPassword,
		// The same rule the submission path applies (internal/compose/send.go).
		ImplicitTLS: port == 465,
	})
	if err != nil {
		return fmt.Errorf("set-account-credentials: could not authenticate the SMTP credential against %s: %w — nothing was written and the stored credential is unchanged", acct.SMTPHost, err)
	}
	return nil
}

// accountValidationError formats the failure setAccountCredentials returns
// when the candidate credential fails IMAP validation. It touches only the
// account's host and the classified onboarding.FailureKind — never Username or
// Password — so a rejected rotation can never put the secret it was trying (or
// the one still stored) into a log line or a script's captured output.
func accountValidationError(host string, err error) error {
	return fmt.Errorf("set-account-credentials: could not validate the new credential against %s (%s): %w — the stored credential is unchanged",
		host, onboarding.FailureKind(err), err)
}

// credentialsEqual reports whether the candidate credential is identical to
// what is already stored — usernames compared directly (not secret,
// accounts-report's own compareOneAccount does the same), passwords in
// constant time and never printed either way.
func credentialsEqual(stored, next onboarding.Credentials) bool {
	if stored.IMAPUsername != next.IMAPUsername || stored.SMTPUsername != next.SMTPUsername {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored.IMAPPassword), []byte(next.IMAPPassword)) == 1 &&
		subtle.ConstantTimeCompare([]byte(stored.SMTPPassword), []byte(next.SMTPPassword)) == 1
}
