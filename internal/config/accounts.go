// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// The accounts file (MAILROOM_ACCOUNTS_FILE) is how a 0.5 operator DECLARES the
// IMAP/SMTP accounts their instance syncs. It is a JSON document rather than
// more environment variables for two reasons: an account is a record with ~9
// correlated fields (env flattening gets ugly fast at more than one account),
// and it carries passwords — a file can be a Docker/Compose secret mounted at
// /run/secrets/..., mode 0400, never appearing in `docker inspect`, `ps`, or a
// process's environment.
//
// Shape (see docs/ or the README for a fuller example):
//
//	{
//	  "accounts": [
//	    {
//	      "name": "personal",
//	      "provider_profile": "generic",
//	      "imap_host": "imap.example.com",
//	      "imap_port": 993,
//	      "smtp_host": "smtp.example.com",
//	      "smtp_port": 465,
//	      "username": "you@example.com",
//	      "password": "…",
//	      "smtp_username": "",
//	      "smtp_password": "",
//	      "writeback_enabled": true
//	    }
//	  ]
//	}
//
// The document is an object, not a bare array, so later keys (a schema version,
// global sync defaults) can be added without breaking existing files. Parsing is
// STRICT — an unknown key is an error, not a silent no-op — because a typo in an
// ops file that quietly disables a setting is worse than a loud failure at boot.
//
// It is declarative in SHAPE but ADDITIVE in effect (mailroom#538): cmd/mailroom
// reconciles it idempotently at boot, and an account whose name already exists
// is left alone — its endpoints, credentials and sync_enabled are never
// rewritten, only genuinely new names are onboarded. Removing an entry does NOT
// delete the account — deletion is a destructive operation that stays with the
// operator CLI, not a side effect of editing a file.
//
// Identities and wildcard domains are the one exception: they are reconciled
// for EVERY declared account, existing or not, but still only ADDED, never
// removed (cmd/mailroom's reconcileIdentities). So an operator can add a
// wildcard domain or a fixed identity to an existing account's entry and a
// restart picks it up — but rotating its password, or changing its host/port,
// silently does nothing until the operator acts some other way. `mailroom-admin
// accounts-report` is how to see, for a given file and a given database,
// exactly where the two have drifted and what (if anything) applies each kind
// of change.

// Defaults applied to an account entry that omits a port.
const (
	DefaultIMAPPort = 993 // implicit TLS
	DefaultSMTPPort = 465 // implicit TLS
)

// Secret is a password read from the accounts file. It is a distinct type so
// that the obvious ways of leaking it — printing it, %v-ing the struct that
// holds it, logging that struct with slog, or marshaling it back to JSON — all
// redact instead. The plaintext comes out only through Reveal, which is
// deliberately conspicuous at the call site.
type Secret string

// redacted is what a Secret renders as everywhere except Reveal.
const redacted = "[REDACTED]"

// String implements fmt.Stringer, so %v/%s/Print of a Secret (or of any struct
// containing one) prints the placeholder.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, so even %#v redacts.
func (Secret) GoString() string { return `"` + redacted + `"` }

// LogValue implements slog.LogValuer, so slog attributes redact.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON redacts, so a re-serialized Account never round-trips the
// password into a log line or an error message.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Reveal returns the plaintext. Every call site is a place a password can
// leak, so keep the set small and name each one here rather than letting it
// drift out of date:
//
//   - cmd/mailroom's accountSpec, sealing a declared account's credentials
//     at onboarding.
//   - cmd/mailroom-census's connectionFromAccountsFile, passing the password
//     straight into a read-only IMAP LOGIN (internal/census.Dial) and
//     nowhere else — no logging, no storage, no third field it flows into.
func (s Secret) Reveal() string { return string(s) }

// Account is one declared IMAP/SMTP account. The fields map 1:1 onto
// onboarding.AccountSpec, which cmd/mailroom builds from this; internal/config
// stays a leaf package and does not import it.
type Account struct {
	// Name is the account's stable, per-user-unique identifier, and the key
	// boot reconciliation matches on. Renaming it in the file creates a SECOND
	// account rather than renaming the first.
	Name string `json:"name"`

	// ProviderProfile selects provider quirks (e.g. "generic", "migadu",
	// "gmail"). Empty means the store default ("generic").
	ProviderProfile string `json:"provider_profile"`

	IMAPHost string `json:"imap_host"`
	IMAPPort int    `json:"imap_port"` // 0 -> DefaultIMAPPort
	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"` // 0 -> DefaultSMTPPort

	// Username/Password authenticate to IMAP, and to SMTP too unless the
	// SMTP-specific fields override them.
	Username string `json:"username"`
	Password Secret `json:"password"`

	// SMTPUsername/SMTPPassword are for providers that issue separate
	// submission credentials. Empty means "reuse the IMAP ones".
	SMTPUsername string `json:"smtp_username"`
	SMTPPassword Secret `json:"smtp_password"`

	// Identities are the exact addresses this account may send as (spec §5's
	// server-side From validation: the server refuses any From that is not one
	// of these). Empty means "just the username", which is the right default
	// for a plain mailbox.
	Identities []string `json:"identities"`

	// WildcardDomains are domains for which this account may send as ANY
	// address — the catchall / wildcard-send model (spec §5: "the primary domain
	// is a catchall … there is no finite alias list to enumerate … wildcard
	// send-as is already configured, so Mailroom can send as any derived address
	// without per-alias setup").
	//
	// This is a SECURITY-RELEVANT declaration: every address at a listed domain
	// becomes sendable by this user. It belongs in the operator's accounts file,
	// where the operator states what the provider has actually granted — never in
	// something a client can write.
	WildcardDomains []string `json:"wildcard_domains"`

	// WritebackEnabled declares whether a NEWLY CREATED account should start
	// shadowed (ADR-028 ingest-only mode): the account syncs normally but
	// Mailroom never constructs an IMAPWriter for it, so it cannot mutate the
	// remote mailbox. Mirrors store.AccountRow.WritebackEnabled and the JMAP
	// Account/set create property of the same name.
	//
	// ⚠ A POINTER ON PURPOSE, exactly like those two. Absent (nil) takes the
	// schema default (true, normal writeback); a plain bool field would make
	// every accounts file written before this field existed construct the zero
	// value and silently shadow every declared account — the failure this mode
	// exists to make impossible, introduced by the mode itself.
	//
	// Only honored when the account is first created: reconcileAccounts
	// (cmd/mailroom) never touches an account that already exists — same as
	// sync_enabled and every other field here — so editing this in the file
	// after the account has been onboarded does nothing. Use
	// `mailroom-admin` / Account/set to change a live account's mode.
	WritebackEnabled *bool `json:"writeback_enabled"`
}

// AccountsDoc is the top-level accounts-file document.
type AccountsDoc struct {
	Accounts []Account `json:"accounts"`
}

// LoadAccountsFile reads and validates the accounts file at path.
//
// An empty path, or a path that does not exist, yields (nil, nil): declaring no
// accounts is a legitimate configuration (a fresh instance, or one whose
// accounts were added with mailroom-admin), not an error. Anything else that
// goes wrong — unreadable file, malformed JSON, unknown key, missing required
// field, duplicate name — IS an error: the operator wrote a file and meant
// something by it, so a file we cannot honor must stop the boot rather than
// silently sync nothing.
//
// Errors never quote the file's contents (they could contain a password); they
// name the offending account by index/name only.
func LoadAccountsFile(path string) ([]Account, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: read %s (%s): %w", EnvAccountsFile, path, err)
	}
	return ParseAccounts(data)
}

// ParseAccounts parses and validates an accounts document. It is the testable
// core of LoadAccountsFile.
func ParseAccounts(data []byte) ([]Account, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var doc AccountsDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("config: %s: invalid JSON: %w", EnvAccountsFile, err)
	}
	// Reject trailing garbage after the document (e.g. two concatenated objects),
	// which encoding/json would otherwise ignore.
	if dec.More() {
		return nil, fmt.Errorf("config: %s: unexpected trailing content after the JSON document", EnvAccountsFile)
	}

	seen := make(map[string]struct{}, len(doc.Accounts))
	out := make([]Account, 0, len(doc.Accounts))
	for i, a := range doc.Accounts {
		a.Name = strings.TrimSpace(a.Name)
		a.ProviderProfile = strings.TrimSpace(a.ProviderProfile)
		a.IMAPHost = strings.TrimSpace(a.IMAPHost)
		a.SMTPHost = strings.TrimSpace(a.SMTPHost)
		a.Username = strings.TrimSpace(a.Username)
		a.SMTPUsername = strings.TrimSpace(a.SMTPUsername)
		a.Identities = normalizeList(a.Identities)
		a.WildcardDomains = normalizeList(a.WildcardDomains)
		// Passwords are NOT trimmed: whitespace can be significant in one.

		if err := a.validate(i); err != nil {
			return nil, err
		}
		if _, dup := seen[a.Name]; dup {
			return nil, fmt.Errorf("config: %s: duplicate account name %q", EnvAccountsFile, a.Name)
		}
		seen[a.Name] = struct{}{}

		if a.IMAPPort == 0 {
			a.IMAPPort = DefaultIMAPPort
		}
		if a.SMTPPort == 0 {
			a.SMTPPort = DefaultSMTPPort
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// normalizeList trims, lowercases and de-duplicates a declared string list,
// dropping empties. Identities and wildcard domains are compared
// case-insensitively (they are addresses and domains).
func normalizeList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		v := strings.ToLower(strings.TrimSpace(s))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// validate checks one account entry. index identifies it when the name itself is
// the thing that's missing.
func (a Account) validate(index int) error {
	where := fmt.Sprintf("accounts[%d]", index)
	if a.Name != "" {
		where = fmt.Sprintf("accounts[%d] (%q)", index, a.Name)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("config: %s: %s: %s", EnvAccountsFile, where, fmt.Sprintf(format, args...))
	}
	switch {
	case a.Name == "":
		return fail("name is required")
	case a.IMAPHost == "":
		return fail("imap_host is required")
	case a.SMTPHost == "":
		return fail("smtp_host is required")
	case a.Username == "":
		return fail("username is required")
	case a.Password == "":
		return fail("password is required")
	case a.IMAPPort < 0 || a.IMAPPort > 65535:
		return fail("imap_port %d is out of range", a.IMAPPort)
	case a.SMTPPort < 0 || a.SMTPPort > 65535:
		return fail("smtp_port %d is out of range", a.SMTPPort)
	}
	// A malformed identity or wildcard domain is a hard error: these declare
	// what the server will let this user SEND AS (spec §5), and a typo that
	// silently widened — or silently dropped — a send-as right is not something
	// to discover at 2am.
	for _, id := range a.Identities {
		if strings.Count(id, "@") != 1 || strings.HasPrefix(id, "@") || strings.HasSuffix(id, "@") {
			return fail("identity %q is not a valid email address", id)
		}
	}
	for _, d := range a.WildcardDomains {
		if d == "" || strings.ContainsAny(d, "@ \t") || !strings.Contains(d, ".") {
			return fail("wildcard_domain %q is not a valid domain", d)
		}
	}
	return nil
}
