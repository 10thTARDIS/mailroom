// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/subtle"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/onboarding"
	"mailroom/internal/store"
)

// `mailroom-admin accounts-report` — where the DATABASE actually is relative to
// what MAILROOM_ACCOUNTS_FILE declares (mailroom#538).
//
// The accounts file is reconciled at boot ADDITIVELY, not declaratively
// (internal/config/accounts.go, cmd/mailroom's reconcileAccounts/
// reconcileIdentities): a name already in the database is never touched again —
// not its endpoints, not its credentials, not sync_enabled — and only genuinely
// new names get onboarded. Identities/wildcard domains are the one exception:
// they are reconciled for every declared account, existing or not, but only
// ADDED, never removed. This is deliberate (a redeploy must never re-dial a
// working account, or silently rotate a live password out from under a running
// sync loop), but it means an operator editing the file after day one is
// editing a document that describes intent, not present state — and nothing
// before this command said so out loud, or told them WHERE the two disagree.
//
// This is a REPORT, never a mutation: it changes nothing in the database or the
// file. --fail-on-drift turns "the file and the database disagree in a way that
// matters" into a non-zero exit, for a deploy pipeline or a pre-flight check —
// see deploy/README.md.
func runAccountsReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("accounts-report", flag.ContinueOnError)
	var (
		userIDStr    = fs.String("user-id", "", "operator user id (ULID)")
		accountsFile = fs.String("accounts-file", "", "path to the accounts file (default: MAILROOM_ACCOUNTS_FILE)")
		failOnDrift  = fs.Bool("fail-on-drift", false, "exit non-zero when the database differs from the file")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	// openDeps does not return the config it loaded (its own callers only ever
	// needed the DB/master-key/logger pieces), and duplicating the _FILE secret
	// indirection here by hand would be a second, divergeable copy of that
	// logic. config.Load reads small, already-required env/files a second
	// time — no new secret handling, no extra I/O that matters.
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("accounts-report: %w", err)
	}
	path := strings.TrimSpace(*accountsFile)
	if path == "" {
		path = cfg.AccountsFile
	}

	declared, err := config.LoadAccountsFile(path)
	if err != nil {
		return fmt.Errorf("accounts-report: %w", err)
	}
	switch {
	case path == "":
		fmt.Printf("no --accounts-file given and %s is unset — every stored account will be "+
			"reported as undeclared\n\n", config.EnvAccountsFile)
	case len(declared) == 0:
		fmt.Printf("accounts file %s declares zero accounts — every stored account will be "+
			"reported as undeclared\n\n", path)
	}

	var (
		accounts   []store.Account
		identities []store.Identity
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if accounts, e = tx.ListAccounts(ctx); e != nil {
			return e
		}
		identities, e = tx.ListIdentities(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("accounts-report: %w", err)
	}

	stored, err := openStoredAccounts(ctx, d.keys, userID, accounts, identities)
	if err != nil {
		return fmt.Errorf("accounts-report: %w", err)
	}

	report := compareAccounts(declared, stored)
	report.render(os.Stdout)

	if *failOnDrift && report.drift() {
		return fmt.Errorf("accounts-report: drift found between the accounts file and the "+
			"database (%d difference(s) need action) — see the report above", report.driftCount())
	}
	return nil
}

// openStoredAccounts opens the sealed credentials for every stored account,
// mirroring validateStoredAccount's shape exactly (this file's other caller of
// onboarding.OpenCredentials): derive the master key once, defer zeroing it,
// and open each account's ciphertext. It is the one place this command touches
// crypto.KeyProvider or onboarding — everything downstream (compareAccounts) is
// a pure function of already-opened data.
func openStoredAccounts(ctx context.Context, keys crypto.KeyProvider, userID ulid.ULID, accounts []store.Account, identities []store.Identity) ([]storedAccount, error) {
	master, err := keys.MasterKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	defer crypto.Zero(master)

	byAccount := make(map[ulid.ULID][]store.Identity, len(accounts))
	for _, id := range identities {
		byAccount[id.AccountID] = append(byAccount[id.AccountID], id)
	}

	out := make([]storedAccount, 0, len(accounts))
	for _, a := range accounts {
		creds, err := onboarding.OpenCredentials(master, userID, a.CredentialsCT)
		if err != nil {
			// Never wrap ct or anything derived from it — OpenCredentials' own
			// error already carries only the AEAD failure, never plaintext.
			return nil, fmt.Errorf("open credentials for account %q: %w", a.Name, err)
		}
		sa := storedAccount{
			ID:               a.ID,
			Name:             a.Name,
			IMAPHost:         a.IMAPHost,
			IMAPPort:         a.IMAPPort,
			SMTPHost:         a.SMTPHost,
			SMTPPort:         a.SMTPPort,
			ProviderProfile:  a.ProviderProfile,
			SyncEnabled:      a.SyncEnabled,
			WritebackEnabled: a.WritebackEnabled,
			IMAPUsername:     creds.IMAPUsername,
			IMAPPassword:     creds.IMAPPassword,
			SMTPUsername:     creds.SMTPUsername,
			SMTPPassword:     creds.SMTPPassword,
		}
		for _, id := range byAccount[a.ID] {
			switch id.Kind {
			case store.IdentityFixed:
				sa.FixedIdentities = append(sa.FixedIdentities, id.Address)
			case store.IdentityWildcard:
				sa.WildcardDomains = append(sa.WildcardDomains, id.Domain)
			}
		}
		out = append(out, sa)
	}
	return out, nil
}

// storedAccount is everything compareAccounts needs about one stored account,
// with its sealed credentials already opened by openStoredAccounts (or a test
// fixture). It never crosses back into ciphertext or a KeyProvider — that is
// what makes compareAccounts testable with no database and no master key.
type storedAccount struct {
	ID               ulid.ULID
	Name             string
	IMAPHost         string
	IMAPPort         int
	SMTPHost         string
	SMTPPort         int
	ProviderProfile  string
	SyncEnabled      bool
	WritebackEnabled bool
	IMAPUsername     string
	IMAPPassword     string
	SMTPUsername     string
	SMTPPassword     string
	// FixedIdentities/WildcardDomains are this account's identities, split by
	// kind (store.IdentityFixed / store.IdentityWildcard). Addresses/domains as
	// stored (already lower-cased by store.CreateIdentity).
	FixedIdentities []string
	WildcardDomains []string
}

// driftNote is one line compareAccounts has to say about a single account.
// Drift marks whether it counts toward accountsReport.drift()/driftCount() —
// i.e. whether --fail-on-drift should fail because of it. Plenty of true, even
// expected, facts about an account are NOT drift (an undeclared account still
// active, an identity present but not declared): the file is additive-only by
// design, so those are business as usual, not something to fix.
type driftNote struct {
	Message string
	Drift   bool
}

// accountReport is compareAccounts' verdict for one account name.
type accountReport struct {
	Name  string
	Notes []driftNote
}

// accountsReport is compareAccounts' pure result. Nothing in it or reachable
// from it requires a database, a master key, or an accounts file on disk — a
// unit test builds one directly from []config.Account/[]storedAccount
// fixtures.
type accountsReport struct {
	Declared int
	Stored   int
	Accounts []accountReport // ordered by name
}

// driftCount is how many individual notes count as drift across every
// account — the "K" in the summary line and what --fail-on-drift gates on.
func (r accountsReport) driftCount() int {
	n := 0
	for _, a := range r.Accounts {
		for _, note := range a.Notes {
			if note.Drift {
				n++
			}
		}
	}
	return n
}

// drift reports whether ANY note anywhere in the report counts as drift.
func (r accountsReport) drift() bool { return r.driftCount() > 0 }

// render writes the human-readable report. It never receives, and therefore
// can never print, a secret: every driftNote's Message was already built
// without one (see compareOneAccount).
func (r accountsReport) render(w io.Writer) {
	_, _ = fmt.Fprintln(w, "=== accounts-report (#538): the accounts file vs the database ===")
	for _, a := range r.Accounts {
		_, _ = fmt.Fprintf(w, "\n%s:\n", a.Name)
		// "Converged" means nothing here needs action, not that there is nothing
		// to say: a declared, matching account still carries its informational
		// `password: matches` note, so the test is "no drift note", never
		// "no notes" (which could only ever be true for an empty account).
		hasDrift := false
		for _, note := range a.Notes {
			hasDrift = hasDrift || note.Drift
		}
		if !hasDrift {
			_, _ = fmt.Fprintln(w, "  converged — nothing here needs action")
		}
		for _, note := range a.Notes {
			_, _ = fmt.Fprintf(w, "  %s\n", note.Message)
		}
	}
	n := r.driftCount()
	_, _ = fmt.Fprintf(w, "\n%d declared, %d stored, %d differences need action\n", r.Declared, r.Stored, n)
	if n == 0 {
		_, _ = fmt.Fprintln(w, "the database reflects everything the file declares")
	}
}

// compareAccounts is the pure core: everything accounts-report has to say,
// computed with no I/O at all. declared is LoadAccountsFile's output (or an
// empty/nil slice for "no file" — the caller has already said so plainly, per
// runAccountsReport); stored is openStoredAccounts' output (or a hand-built
// fixture in a unit test).
func compareAccounts(declared []config.Account, stored []storedAccount) accountsReport {
	declaredByName := make(map[string]config.Account, len(declared))
	for _, d := range declared {
		declaredByName[d.Name] = d
	}
	storedByName := make(map[string]storedAccount, len(stored))
	for _, s := range stored {
		storedByName[s.Name] = s
	}

	names := make(map[string]struct{}, len(declared)+len(stored))
	for _, d := range declared {
		names[d.Name] = struct{}{}
	}
	for _, s := range stored {
		names[s.Name] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)

	report := accountsReport{Declared: len(declared), Stored: len(stored)}
	for _, name := range ordered {
		d, hasDeclared := declaredByName[name]
		s, hasStored := storedByName[name]
		ar := accountReport{Name: name}

		switch {
		case hasDeclared && !hasStored:
			ar.Notes = append(ar.Notes, driftNote{
				Drift: true,
				Message: "NOT PRESENT — will be onboarded at the next boot (IMAP-validated; " +
					"a server rejection stores it with sync disabled)",
			})
		case !hasDeclared && hasStored:
			ar.Notes = append(ar.Notes, driftNote{
				Drift: false,
				Message: "not declared in the file (created through Account/set or add-account, " +
					"or removed from the file) — still active; the file never deletes accounts",
			})
		default:
			ar.Notes = append(ar.Notes, compareOneAccount(d, s)...)
		}

		report.Accounts = append(report.Accounts, ar)
	}
	return report
}

// compareOneAccount compares a declared account against its stored row,
// reporting every field reconcileAccounts (cmd/mailroom) would have left alone
// because the account already existed — this is the drift ITS additive
// semantics make possible — plus the identities reconcileIdentities WOULD still
// add today. Every difference names what applies the fix, because "differs" on
// its own tells an operator nothing about what to run.
func compareOneAccount(d config.Account, s storedAccount) []driftNote {
	var notes []driftNote
	diff := func(field, fileVal, storedVal string) {
		notes = append(notes, driftNote{
			Drift: true,
			Message: fmt.Sprintf("%s: DIFFERS (file: %s, stored: %s) — the file only applies "+
				"this when the account is created", field, fileVal, storedVal),
		})
	}

	if d.IMAPHost != s.IMAPHost {
		diff("imap_host", d.IMAPHost, s.IMAPHost)
	}
	// The file's ports are already defaulted by config.ParseAccounts (0 ->
	// DefaultIMAPPort/DefaultSMTPPort), so a caller that did not go through
	// LoadAccountsFile/ParseAccounts must supply the same defaults itself or
	// this reports phantom drift on every account.
	if d.IMAPPort != s.IMAPPort {
		diff("imap_port", strconv.Itoa(d.IMAPPort), strconv.Itoa(s.IMAPPort))
	}
	if d.SMTPHost != s.SMTPHost {
		diff("smtp_host", d.SMTPHost, s.SMTPHost)
	}
	if d.SMTPPort != s.SMTPPort {
		diff("smtp_port", strconv.Itoa(d.SMTPPort), strconv.Itoa(s.SMTPPort))
	}

	// provider_profile: empty in the file means "the store default", which is
	// "generic" — mirrored from store.CreateAccount's own default
	// (internal/store/accounts.go: `profile := a.ProviderProfile; if profile ==
	// "" { profile = "generic" }`), so a file that never set it does not read as
	// drift against the row that default produced.
	declaredProfile := d.ProviderProfile
	if declaredProfile == "" {
		declaredProfile = "generic"
	}
	if declaredProfile != s.ProviderProfile {
		diff("provider_profile", declaredProfile, s.ProviderProfile)
	}

	if d.Username != s.IMAPUsername {
		diff("username", d.Username, s.IMAPUsername)
	}

	// password: the one field whose VALUE must never be rendered either way —
	// only ever "matches" or "DIFFERS", compared in constant time.
	if subtle.ConstantTimeCompare([]byte(d.Password.Reveal()), []byte(s.IMAPPassword)) == 1 {
		notes = append(notes, driftNote{Drift: false, Message: "password: matches"})
	} else {
		notes = append(notes, driftNote{
			Drift: true,
			Message: "password: DIFFERS — a rotation in the file has NOT been applied; apply it " +
				"through Account/set (the web client's account settings) or `mailroom-admin " +
				"set-account-credentials --from-accounts-file --account " + d.Name + "` (#571)",
		})
	}

	// SMTP username/password: mirror onboarding.AddAccount's sealing EXACTLY
	// (internal/onboarding/onboarding.go, the SealCredentials call inside
	// AddAccount, and its firstNonEmpty helper): an empty SMTP field in the file
	// means "reuse the IMAP one", and that is what got sealed into the stored
	// row — so the declared side of this comparison must apply the identical
	// fallback, or a file that legitimately leaves smtp_username/smtp_password
	// blank would show phantom drift against every account that has ever been
	// created.
	declaredSMTPUser := d.SMTPUsername
	if declaredSMTPUser == "" {
		declaredSMTPUser = d.Username
	}
	if declaredSMTPUser != s.SMTPUsername {
		diff("smtp_username", declaredSMTPUser, s.SMTPUsername)
	}
	declaredSMTPPass := d.SMTPPassword.Reveal()
	if declaredSMTPPass == "" {
		declaredSMTPPass = d.Password.Reveal()
	}
	// Never printed either way — same never-print rule as password — but only
	// reported when it actually differs, matching every other non-secret field
	// above (a "matches" line for something that always matches in the common
	// case, where smtp_password is left blank and mirrors the IMAP password,
	// would be noise on every converged account).
	if subtle.ConstantTimeCompare([]byte(declaredSMTPPass), []byte(s.SMTPPassword)) != 1 {
		notes = append(notes, driftNote{
			Drift: true,
			Message: "smtp_password: DIFFERS — a rotation in the file has NOT been applied; apply it " +
				"through Account/set (the web client's account settings) or `mailroom-admin " +
				"set-account-credentials --from-accounts-file --account " + d.Name + "` (#571)",
		})
	}

	if d.WritebackEnabled != nil && *d.WritebackEnabled != s.WritebackEnabled {
		notes = append(notes, driftNote{
			Drift: true,
			Message: fmt.Sprintf("writeback_enabled: DIFFERS (file: %t, stored: %t) — the file only "+
				"applies this when the account is created; change a live account with Account/set "+
				"(writebackEnabled)", *d.WritebackEnabled, s.WritebackEnabled),
		})
	}

	if !s.SyncEnabled {
		notes = append(notes, driftNote{
			Drift: true,
			Message: fmt.Sprintf("SYNC DISABLED — fix the credentials, then run mailroom-admin "+
				"enable-account --user-id <user> --account-id %s", s.ID),
		})
	}

	notes = append(notes, compareIdentities(d, s)...)
	return notes
}

// compareIdentities reports where an account's send-as rights (spec §5) differ
// from what reconcileIdentities (cmd/mailroom) would still add, and what is
// stored but not declared. Only "declared but missing" counts as drift: an
// operator who is about to redeploy needs to know a wildcard domain they just
// added has not taken effect yet. "Stored but undeclared" is normal — the file
// never removes an identity — so it is reported but never counted.
func compareIdentities(d config.Account, s storedAccount) []driftNote {
	declaredFixed, declaredWildcards := declaredIdentities(d)

	var notes []driftNote
	storedFixed := toLowerSet(s.FixedIdentities)
	for _, addr := range declaredFixed {
		if !storedFixed[strings.ToLower(addr)] {
			notes = append(notes, driftNote{
				Drift:   true,
				Message: fmt.Sprintf("identity %s: declared but not present — will be added at the next boot", addr),
			})
		}
	}
	storedWildcards := toLowerSet(s.WildcardDomains)
	for _, dom := range declaredWildcards {
		if !storedWildcards[strings.ToLower(dom)] {
			notes = append(notes, driftNote{
				Drift:   true,
				Message: fmt.Sprintf("wildcard_domain %s: declared but not present — will be added at the next boot", dom),
			})
		}
	}

	declaredFixedSet := toLowerSet(declaredFixed)
	for _, addr := range s.FixedIdentities {
		if !declaredFixedSet[strings.ToLower(addr)] {
			notes = append(notes, driftNote{
				Drift:   false,
				Message: fmt.Sprintf("identity %s: present, not declared — the file never removes identities", addr),
			})
		}
	}
	declaredWildcardSet := toLowerSet(declaredWildcards)
	for _, dom := range s.WildcardDomains {
		if !declaredWildcardSet[strings.ToLower(dom)] {
			notes = append(notes, driftNote{
				Drift:   false,
				Message: fmt.Sprintf("wildcard_domain %s: present, not declared — the file never removes identities", dom),
			})
		}
	}
	return notes
}

// declaredIdentities computes the fixed-address and wildcard-domain sets a
// declared account will end up with, mirroring reconcileIdentities
// (cmd/mailroom/main.go) EXACTLY:
//
//	fixed := d.Identities
//	if len(fixed) == 0 && len(d.WildcardDomains) == 0 && strings.Contains(d.Username, "@") {
//		fixed = []string{strings.ToLower(d.Username)}
//	}
//
// i.e. an account that declares neither identities nor wildcard domains, and
// whose username contains "@", gets ONE fixed identity for its (lower-cased)
// username — the default that lets a fresh instance send with zero extra
// configuration. Declaring a wildcard domain alone suppresses it, same as
// reconcileIdentities.
func declaredIdentities(d config.Account) (fixed, wildcards []string) {
	fixed = d.Identities
	wildcards = d.WildcardDomains
	if len(fixed) == 0 && len(wildcards) == 0 && strings.Contains(d.Username, "@") {
		fixed = []string{strings.ToLower(d.Username)}
	}
	return fixed, wildcards
}

// toLowerSet builds a lower-cased membership set for a case-insensitive
// comparison (addresses and domains are compared case-insensitively
// throughout — config.normalizeList and store.NormalizeAddress both agree).
func toLowerSet(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, s := range in {
		out[strings.ToLower(s)] = true
	}
	return out
}
