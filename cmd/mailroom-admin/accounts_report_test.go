// SPDX-License-Identifier: AGPL-3.0-only

package main

// accounts-report's pure core (mailroom#538): compareAccounts/accountsReport
// against hand-built []config.Account / []storedAccount fixtures, no database
// and no master key. The integration test (accounts_report_integration_test.go)
// covers the one thing that can't be faked here — opening a REAL sealed
// credentials row with a real master key.

import (
	"bytes"
	"strings"
	"testing"

	"mailroom/internal/config"
	"mailroom/internal/store"
)

// baseDeclared returns a converged, fully-specified declared account: every
// test starts from this and overrides only what it is testing, so an
// unrelated field can never be the reason a test passes or fails.
func baseDeclared(name string) config.Account {
	return config.Account{
		Name:     name,
		IMAPHost: "imap.example.com",
		IMAPPort: 993, // config.DefaultIMAPPort — ParseAccounts already applies this
		SMTPHost: "smtp.example.com",
		SMTPPort: 465, // config.DefaultSMTPPort
		Username: "you@example.com",
		Password: config.Secret("S3cret-Marker-xyz"),
	}
}

// baseStored returns the storedAccount that baseDeclared's account would have
// produced through onboarding.AddAccount, converged in every field.
func baseStored(name string) storedAccount {
	return storedAccount{
		ID:               store.NewULID(),
		Name:             name,
		IMAPHost:         "imap.example.com",
		IMAPPort:         993,
		SMTPHost:         "smtp.example.com",
		SMTPPort:         465,
		ProviderProfile:  "generic",
		SyncEnabled:      true,
		WritebackEnabled: true,
		IMAPUsername:     "you@example.com",
		IMAPPassword:     "S3cret-Marker-xyz",
		// onboarding.AddAccount seals SMTP creds via firstNonEmpty(spec.SMTP*,
		// spec.*): an account created from baseDeclared (empty SMTP fields)
		// stores the IMAP creds copied over.
		SMTPUsername: "you@example.com",
		SMTPPassword: "S3cret-Marker-xyz",
		// reconcileIdentities' username-default rule: baseDeclared has no
		// Identities and no WildcardDomains, so it gets exactly one fixed
		// identity for its (lower-cased) username.
		FixedIdentities: []string{"you@example.com"},
	}
}

// notesFor returns the message strings compareAccounts produced for one
// account name, for assertions that don't care about ordering/Drift.
func notesFor(t *testing.T, r accountsReport, name string) []string {
	t.Helper()
	for _, a := range r.Accounts {
		if a.Name == name {
			out := make([]string, len(a.Notes))
			for i, n := range a.Notes {
				out[i] = n.Message
			}
			return out
		}
	}
	t.Fatalf("no account report for %q; report had: %+v", name, r.Accounts)
	return nil
}

// hasFieldNote reports whether any note begins with exactly "<field>: <verdict>".
// It exists because a plain substring check is not precise enough here: the
// "password" field's DIFFERS/matches note is itself a substring of the
// "smtp_password" field's ("smtp_password: DIFFERS" contains "password:
// DIFFERS"). Rotating only the IMAP password, with SMTP fields left blank in
// the file, ALSO changes the effective declared SMTP password (it falls back
// to the IMAP one, mirroring onboarding.AddAccount's firstNonEmpty) — so a
// bare strings.Contains(notes, "password: DIFFERS") stayed green even when the
// password comparison itself was probed to always report "matches"
// (accounts-report#538 N1 negative control): the smtp_password note's DIFFERS
// satisfied it. Field-anchored matching is what actually pins the field being
// tested.
func hasFieldNote(notes []string, field, verdict string) bool {
	prefix := field + ": " + verdict
	for _, n := range notes {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

func containsSubstr(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

func TestCompareAccountsConverged(t *testing.T) {
	d := baseDeclared("work")
	s := baseStored("work")

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	if !hasFieldNote(notes, "password", "matches") {
		t.Errorf("converged account notes = %v; want a password: matches line", notes)
	}
	for _, n := range notes {
		if strings.Contains(n, "DIFFERS") || strings.Contains(n, "NOT PRESENT") || strings.Contains(n, "SYNC DISABLED") {
			t.Errorf("converged account has an unexpected note: %q (all notes: %v)", n, notes)
		}
	}
	if r.drift() {
		t.Errorf("drift() = true for a fully converged account; driftCount=%d", r.driftCount())
	}
	if r.Declared != 1 || r.Stored != 1 {
		t.Errorf("Declared=%d Stored=%d, want 1,1", r.Declared, r.Stored)
	}
}

func TestCompareAccountsRotatedPassword(t *testing.T) {
	d := baseDeclared("work")
	d.Password = config.Secret("a-new-password")
	s := baseStored("work") // still has the OLD password

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	if !hasFieldNote(notes, "password", "DIFFERS") {
		t.Errorf("rotated-password notes = %v; want a password: DIFFERS line", notes)
	}
	if !containsSubstr(notes, "Account/set") {
		t.Errorf("rotated-password notes = %v; want the Account/set remediation hint", notes)
	}
	if !containsSubstr(notes, "#571") {
		t.Errorf("rotated-password notes = %v; want a pointer to #571", notes)
	}
	if !r.drift() {
		t.Error("drift() = false for a rotated, unapplied password; want true")
	}
}

func TestCompareAccountsRotatedUsername(t *testing.T) {
	d := baseDeclared("work")
	d.Username = "someone-else@example.com"
	s := baseStored("work")

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	if !containsSubstr(notes, "username: DIFFERS (file: someone-else@example.com, stored: you@example.com)") {
		t.Errorf("rotated-username notes = %v; want a username DIFFERS line naming both values", notes)
	}
	if !r.drift() {
		t.Error("drift() = false for a rotated username; want true")
	}
}

func TestCompareAccountsHostPortChange(t *testing.T) {
	d := baseDeclared("work")
	d.IMAPHost = "imap2.example.com"
	d.SMTPPort = 587
	s := baseStored("work")

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	if !containsSubstr(notes, "imap_host: DIFFERS (file: imap2.example.com, stored: imap.example.com)") {
		t.Errorf("notes = %v; want an imap_host DIFFERS line", notes)
	}
	if !containsSubstr(notes, "smtp_port: DIFFERS (file: 587, stored: 465)") {
		t.Errorf("notes = %v; want an smtp_port DIFFERS line", notes)
	}
	if !r.drift() {
		t.Error("drift() = false for a host/port change; want true")
	}
}

// TestCompareAccountsEmptySMTPFieldsNoPhantomDrift pins the onboarding
// firstNonEmpty mirror: a file that never set smtp_username/smtp_password
// (baseDeclared) must not read as drift against the row onboarding actually
// produced (baseStored, which copied the IMAP creds over).
func TestCompareAccountsEmptySMTPFieldsNoPhantomDrift(t *testing.T) {
	d := baseDeclared("work")
	s := baseStored("work")

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	if containsSubstr(notes, "smtp_username") || containsSubstr(notes, "smtp_password") {
		t.Errorf("empty SMTP fields produced phantom drift: %v", notes)
	}
	if r.drift() {
		t.Errorf("drift() = true for empty-SMTP-fields-mirroring-IMAP; want false (driftCount=%d, notes=%v)",
			r.driftCount(), notes)
	}
}

func TestCompareAccountsSMTPPasswordDiffersWhenDeclared(t *testing.T) {
	d := baseDeclared("work")
	d.SMTPUsername = "submission@example.com"
	d.SMTPPassword = config.Secret("a-different-smtp-password")
	s := baseStored("work") // SMTP creds still mirror the (unrelated) IMAP ones

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	if !containsSubstr(notes, "smtp_username: DIFFERS (file: submission@example.com, stored: you@example.com)") {
		t.Errorf("notes = %v; want an smtp_username DIFFERS line", notes)
	}
	if !containsSubstr(notes, "smtp_password: DIFFERS") {
		t.Errorf("notes = %v; want an smtp_password DIFFERS line", notes)
	}
	if !r.drift() {
		t.Error("drift() = false for a declared SMTP override that disagrees with storage; want true")
	}
}

func TestCompareAccountsWritebackDeclaredMismatch(t *testing.T) {
	d := baseDeclared("work")
	falseVal := false
	d.WritebackEnabled = &falseVal
	s := baseStored("work")
	s.WritebackEnabled = true // still live

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	if !containsSubstr(notes, "writeback_enabled: DIFFERS (file: false, stored: true)") {
		t.Errorf("notes = %v; want a writeback_enabled DIFFERS line", notes)
	}
	if !containsSubstr(notes, "Account/set") {
		t.Errorf("notes = %v; want the Account/set remediation", notes)
	}
	if !r.drift() {
		t.Error("drift() = false for a declared writebackEnabled mismatch; want true")
	}
}

// TestCompareAccountsWritebackUndeclaredNoReport pins the pointer contract
// (config.Account.WritebackEnabled is *bool): a file that never mentions
// writeback_enabled (nil) must produce NO note at all, regardless of what the
// account is actually storing — the file said nothing, so there is nothing to
// compare.
func TestCompareAccountsWritebackUndeclaredNoReport(t *testing.T) {
	d := baseDeclared("work") // WritebackEnabled is nil
	s := baseStored("work")
	s.WritebackEnabled = false // shadowed; the file simply never said

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	if containsSubstr(notes, "writeback_enabled") {
		t.Errorf("notes = %v; want no writeback_enabled note when the file never declares it", notes)
	}
}

func TestCompareAccountsSyncDisabled(t *testing.T) {
	d := baseDeclared("work")
	s := baseStored("work")
	s.SyncEnabled = false

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	want := "SYNC DISABLED — fix the credentials, then run mailroom-admin enable-account " +
		"--user-id <user> --account-id " + s.ID.String()
	if !containsSubstr(notes, want) {
		t.Errorf("notes = %v; want %q", notes, want)
	}
	if !r.drift() {
		t.Error("drift() = false for a sync-disabled account; want true")
	}
}

func TestCompareAccountsMissingDeclaredAccount(t *testing.T) {
	d := baseDeclared("newaccount")

	r := compareAccounts([]config.Account{d}, nil)

	notes := notesFor(t, r, "newaccount")
	if !containsSubstr(notes, "NOT PRESENT") {
		t.Errorf("notes = %v; want a NOT PRESENT line", notes)
	}
	if !r.drift() {
		t.Error("drift() = false for a declared-but-missing account; want true")
	}
	if r.Declared != 1 || r.Stored != 0 {
		t.Errorf("Declared=%d Stored=%d, want 1,0", r.Declared, r.Stored)
	}
}

func TestCompareAccountsExtraStoredAccount(t *testing.T) {
	s := baseStored("sideloaded")

	r := compareAccounts(nil, []storedAccount{s})

	notes := notesFor(t, r, "sideloaded")
	if !containsSubstr(notes, "not declared in the file") {
		t.Errorf("notes = %v; want a not-declared line", notes)
	}
	if !containsSubstr(notes, "the file never deletes accounts") {
		t.Errorf("notes = %v; want the never-deletes-accounts explanation", notes)
	}
	if r.drift() {
		t.Errorf("drift() = true for an extra stored account; want false (this is normal, additive-only behavior)")
	}
}

// --- Identity default rule (mirrors cmd/mailroom's reconcileIdentities) ----

func TestDeclaredIdentitiesUsernameDefault(t *testing.T) {
	d := baseDeclared("work") // no Identities, no WildcardDomains
	fixed, wild := declaredIdentities(d)
	if len(wild) != 0 {
		t.Errorf("wildcards = %v, want none", wild)
	}
	if len(fixed) != 1 || fixed[0] != "you@example.com" {
		t.Errorf("fixed = %v, want [you@example.com] (the username default)", fixed)
	}
}

// TestDeclaredIdentitiesWildcardOnlySuppressesUsernameDefault pins the exact
// condition reconcileIdentities checks: len(fixed)==0 AND len(wildcards)==0.
// Declaring ONLY a wildcard domain must NOT also get the bare username as a
// fixed identity — that would grant a second, undeclared send-as right.
func TestDeclaredIdentitiesWildcardOnlySuppressesUsernameDefault(t *testing.T) {
	d := baseDeclared("work")
	d.WildcardDomains = []string{"example.com"}
	fixed, wild := declaredIdentities(d)
	if len(fixed) != 0 {
		t.Errorf("fixed = %v, want none (a declared wildcard domain suppresses the username default)", fixed)
	}
	if len(wild) != 1 || wild[0] != "example.com" {
		t.Errorf("wildcards = %v, want [example.com]", wild)
	}
}

func TestDeclaredIdentitiesExplicitSuppressesUsernameDefault(t *testing.T) {
	d := baseDeclared("work")
	d.Identities = []string{"alias@example.com"}
	fixed, wild := declaredIdentities(d)
	if len(wild) != 0 {
		t.Errorf("wildcards = %v, want none", wild)
	}
	if len(fixed) != 1 || fixed[0] != "alias@example.com" {
		t.Errorf("fixed = %v, want exactly the declared identity, not the username default", fixed)
	}
}

func TestCompareAccountsDeclaredIdentityMissing(t *testing.T) {
	d := baseDeclared("work")
	d.Identities = []string{"alias@example.com"}
	s := baseStored("work")
	s.FixedIdentities = nil // nothing stored yet

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	if !containsSubstr(notes, "identity alias@example.com: declared but not present") {
		t.Errorf("notes = %v; want a declared-but-missing identity line", notes)
	}
	if !r.drift() {
		t.Error("drift() = false for a declared identity missing from storage; want true")
	}
}

func TestCompareAccountsUndeclaredStoredIdentity(t *testing.T) {
	d := baseDeclared("work")
	d.Identities = []string{"you@example.com"} // only this one declared
	s := baseStored("work")
	s.FixedIdentities = []string{"you@example.com", "extra@example.com"}

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	if !containsSubstr(notes, "identity extra@example.com: present, not declared") {
		t.Errorf("notes = %v; want an undeclared-identity line for extra@example.com", notes)
	}
	if !containsSubstr(notes, "the file never removes identities") {
		t.Errorf("notes = %v; want the never-removes-identities explanation", notes)
	}
	if r.drift() {
		t.Errorf("drift() = true for an undeclared stored identity; want false (driftCount=%d)", r.driftCount())
	}
}

func TestCompareAccountsWildcardDomainMissingAndUndeclared(t *testing.T) {
	d := baseDeclared("work")
	d.WildcardDomains = []string{"example.com", "example.org"}
	s := baseStored("work")
	s.WildcardDomains = []string{"example.com", "leftover.example.net"}

	r := compareAccounts([]config.Account{d}, []storedAccount{s})

	notes := notesFor(t, r, "work")
	if !containsSubstr(notes, "wildcard_domain example.org: declared but not present") {
		t.Errorf("notes = %v; want a missing-wildcard line for example.org", notes)
	}
	if !containsSubstr(notes, "wildcard_domain leftover.example.net: present, not declared") {
		t.Errorf("notes = %v; want an undeclared-wildcard line for leftover.example.net", notes)
	}
	if r.driftCount() != 1 {
		t.Errorf("driftCount() = %d, want 1 (only the missing domain counts)", r.driftCount())
	}
}

// --- Ordering + summary ------------------------------------------------

func TestCompareAccountsOrderedByName(t *testing.T) {
	r := compareAccounts(
		[]config.Account{baseDeclared("zebra"), baseDeclared("apple")},
		[]storedAccount{baseStored("zebra"), baseStored("apple")},
	)
	if len(r.Accounts) != 2 || r.Accounts[0].Name != "apple" || r.Accounts[1].Name != "zebra" {
		t.Errorf("Accounts = %+v; want [apple zebra]", r.Accounts)
	}
}

func TestAccountsReportRenderSummaryLine(t *testing.T) {
	r := compareAccounts(
		[]config.Account{baseDeclared("work")},
		[]storedAccount{baseStored("work")},
	)
	var buf bytes.Buffer
	r.render(&buf)
	out := buf.String()
	if !strings.Contains(out, "1 declared, 1 stored, 0 differences need action") {
		t.Errorf("render output = %q; want the zero-drift summary line", out)
	}
	if !strings.Contains(out, "the database reflects everything the file declares") {
		t.Errorf("render output = %q; want the explicit zero-drift sentence", out)
	}
}

func TestAccountsReportRenderSummaryLineWithDrift(t *testing.T) {
	// imap_host is deliberately the field changed here (rather than, say,
	// username): username feeds both smtp_username's fallback and the identity
	// default, so changing it cascades into three notes and would make this
	// summary-line count assertion actually be pinning that cascade rather than
	// the summary line's arithmetic. imap_host affects nothing else.
	d := baseDeclared("work")
	d.IMAPHost = "changed.example.com"
	r := compareAccounts([]config.Account{d}, []storedAccount{baseStored("work")})

	var buf bytes.Buffer
	r.render(&buf)
	out := buf.String()
	if strings.Contains(out, "the database reflects everything the file declares") {
		t.Errorf("render output = %q; must not claim convergence when there is drift", out)
	}
	if !strings.Contains(out, "1 declared, 1 stored, 1 differences need action") {
		t.Errorf("render output = %q; want the summary line naming exactly 1 difference", out)
	}
}

// A converged declared account still carries its informational `password:
// matches` note, so "converged" must key on the ABSENCE OF DRIFT, not on an empty
// note list — which, for a declared and stored account, can never be empty.
func TestAccountsReportRenderConvergedLineKeysOnDriftNotNotes(t *testing.T) {
	converged := compareAccounts([]config.Account{baseDeclared("work")}, []storedAccount{baseStored("work")})
	var buf bytes.Buffer
	converged.render(&buf)
	if out := buf.String(); !strings.Contains(out, "converged — nothing here needs action") {
		t.Errorf("render output = %q; a converged account must say so", out)
	}

	d := baseDeclared("work")
	d.IMAPHost = "changed.example.com"
	drifted := compareAccounts([]config.Account{d}, []storedAccount{baseStored("work")})
	buf.Reset()
	drifted.render(&buf)
	if out := buf.String(); strings.Contains(out, "converged") {
		t.Errorf("render output = %q; an account with drift must not be called converged", out)
	}
}

// --- The secret must never appear in output or in any error ------------

// TestAccountsReportNeverRendersTheSecret plants a distinctive password on
// both sides, in a MATCHING case (the common, non-drift case, where a naive
// implementation might print it to say "matches: <value>") and a DIFFERING
// case, then asserts the marker never appears in rendered output.
func TestAccountsReportNeverRendersTheSecret(t *testing.T) {
	const marker = "S3cret-Marker-xyz"

	t.Run("matching passwords", func(t *testing.T) {
		d := baseDeclared("work")
		d.Password = config.Secret(marker)
		s := baseStored("work")
		s.IMAPPassword = marker

		r := compareAccounts([]config.Account{d}, []storedAccount{s})
		var buf bytes.Buffer
		r.render(&buf)
		if strings.Contains(buf.String(), marker) {
			t.Fatalf("render output contains the plaintext password marker:\n%s", buf.String())
		}
	})

	t.Run("differing passwords", func(t *testing.T) {
		d := baseDeclared("work")
		d.Password = config.Secret(marker)
		s := baseStored("work")
		s.IMAPPassword = "a-totally-different-password"

		r := compareAccounts([]config.Account{d}, []storedAccount{s})
		var buf bytes.Buffer
		r.render(&buf)
		if strings.Contains(buf.String(), marker) {
			t.Fatalf("render output contains the plaintext password marker:\n%s", buf.String())
		}
	})

	t.Run("differing SMTP passwords", func(t *testing.T) {
		d := baseDeclared("work")
		d.SMTPUsername = "submission@example.com"
		d.SMTPPassword = config.Secret(marker)
		s := baseStored("work")
		s.SMTPPassword = "a-totally-different-smtp-password"

		r := compareAccounts([]config.Account{d}, []storedAccount{s})
		var buf bytes.Buffer
		r.render(&buf)
		if strings.Contains(buf.String(), marker) {
			t.Fatalf("render output contains the plaintext SMTP password marker:\n%s", buf.String())
		}
	})
}

// TestAccountsReportNeverRendersTheSecretEvenAsFileValue is the negative
// control for the trap above: diff() DOES print file/stored values for
// non-secret fields (imap_host etc). Confirm that mechanism actually renders
// something so the marker's absence above is not simply because nothing was
// ever printed.
func TestAccountsReportNeverRendersTheSecretEvenAsFileValueControl(t *testing.T) {
	d := baseDeclared("work")
	d.IMAPHost = "changed.example.com"
	s := baseStored("work")

	r := compareAccounts([]config.Account{d}, []storedAccount{s})
	var buf bytes.Buffer
	r.render(&buf)
	if !strings.Contains(buf.String(), "changed.example.com") {
		t.Fatalf("render output never printed a non-secret file value at all — the rendering "+
			"mechanism itself is not exercised, so the password test above proves nothing:\n%s", buf.String())
	}
}
