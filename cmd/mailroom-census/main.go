// SPDX-License-Identifier: AGPL-3.0-only

// Command mailroom-census measures an IMAP mailbox — mailbox names,
// per-mailbox message/byte/attachment/draft counts, a by-year breakdown,
// SPECIAL-USE role coverage, and Message-ID duplication — WITHOUT changing
// anything on the server. It exists to answer "what am I about to sync?"
// before a real account is ever created, and "did the server say what I
// expect?" for a live one, with a number to point at rather than a guess.
//
// # Preferred: run it next to an instance, with no password copied anywhere
//
//	mailroom-census -accounts-file /path/to/mailroom-accounts.json -account personal
//
// This reads the SAME accounts document the server itself boots from (the
// file MAILROOM_ACCOUNTS_FILE points the SERVER at —
// internal/config.LoadAccountsFile) and takes the named account's
// host/port/username/password straight out of it. Nothing is copied into a
// second file or a shell history: point -accounts-file at that same file —
// this command does NOT read MAILROOM_ACCOUNTS_FILE itself, the path must
// be given explicitly.
//
// # Standalone: point it at a server directly
//
//	mailroom-census -host imap.example.com -user you@example.com \
//	    -password-file /run/secrets/census-password
//
// There is deliberately no -password flag: a password given on the command
// line ends up in the process list and shell history, both readable by
// other processes/users on the same machine. Use -password-file (its
// contents, with at most one trailing newline trimmed) or the
// MAILROOM_CENSUS_PASSWORD environment variable instead.
//
// Both modes always dial implicit TLS (IMAPS) — there is no -tls flag, on
// purpose: a census run against a real account must never be talked into
// sending a real password in plaintext. Pointing this package at a
// non-TLS/self-signed server (the Dovecot test harness) is done through
// internal/census's own package API, which is what
// internal/census/live_integration_test.go uses — not through this CLI.
//
// # It is read-only, by construction, in two independent layers
//
// internal/census.Reader — the only IMAP surface this command touches —
// has no method that can change server state at all (no STORE, EXPUNGE,
// APPEND, COPY, MOVE, CREATE, DELETE, RENAME, SUBSCRIBE); and every mailbox
// it opens is opened with EXAMINE, never SELECT, so the server itself
// refuses flag/message changes on the connection regardless of what this
// client asked for. See internal/census/reader.go for the exact guarantee
// and its one documented limitation.
//
// # Output
//
// Plain text by default (a human-readable report); -json emits the same
// Report structure as JSON, for scripting or archival. Either way, the
// output is about to describe somebody's real mailbox contents — treat it
// as OPERATOR-PRIVATE. It belongs in a private note (or nowhere at
// all), never committed to this tree (CLAUDE.md's portability rule, and
// plain good sense about mailbox contents generally).
//
// # Running it
//
// It ships in the Mailroom image as /mailroom-census (since #529's production
// run needed it somewhere the runbook could point at). The image's ENTRYPOINT
// is /mailroom and docker APPENDS arguments, so --entrypoint is not optional:
// without it, the command below starts a second SERVER instead.
//
// Before Mailroom has ever touched the account — the case a cutover census is
// for — there is no accounts file yet, so give it the password in a file:
//
//	docker run --rm --entrypoint /mailroom-census \
//	    -v "$PWD/imap-password:/run/census-password:ro" \
//	    <image> -host imap.example.com -user you@example.com \
//	    -password-file /run/census-password
//
// On a host that already runs the stack, reuse its own accounts secret
// through compose (--no-deps: postgres and the bootstrap one-shots are not
// needed and must not be restarted):
//
//	docker compose run --rm --no-deps --entrypoint /mailroom-census mailroom \
//	    -accounts-file /run/secrets/mailroom_accounts -account personal
//
// (placeholders only — substitute your own host, user, image and account).
// ⚠ The file must be readable by the image's non-root user; `chmod 0444` a
// throwaway copy and delete it afterwards, rather than loosening the real one.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mailroom/internal/census"
	"mailroom/internal/config"
)

// probeMaxHardCap bounds -probe-connections: a census run must never be the
// thing that finds out the hard way how many connections a shared mail
// server can take.
const probeMaxHardCap = 50

// envCensusPassword is the standalone mode's password source (besides
// -password-file). Deliberately NOT MAILROOM_ACCOUNTS_FILE's password
// field — that comes through -accounts-file/-account instead.
const envCensusPassword = "MAILROOM_CENSUS_PASSWORD"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mailroom-census: "+err.Error())
		os.Exit(1)
	}
}

// run is main's testable core: it takes stdout as an io.Writer so a test
// can capture output without a real file descriptor, and does everything
// through census's public API — no test here reaches a live server, since
// that is what internal/census/live_integration_test.go is for.

// options collects the raw flag values exactly as parsed, plus whether
// -port was explicitly set (fs.Visit, in run) — connectionConfig resolves
// them into a census.Config purely from this struct, doing no I/O beyond
// reading a declared accounts file, so it needs no flag.FlagSet to unit
// test. There is no TLS mode field: both connection modes always dial
// implicit TLS — see the package doc.
type options struct {
	host         string
	port         int
	portSet      bool
	user         string
	passwordFile string
	accountsFile string
	account      string
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mailroom-census", flag.ContinueOnError)
	host := fs.String("host", "", "IMAP host (standalone mode; mutually exclusive with -accounts-file/-account)")
	port := fs.Int("port", 993, "IMAP port")
	user := fs.String("user", "", "IMAP username (standalone mode)")
	passwordFile := fs.String("password-file", "", "file holding the IMAP password (one trailing newline trimmed); "+
		"or set "+envCensusPassword+". There is deliberately no -password flag — see the package doc")
	accountsFile := fs.String("accounts-file", "", "read connection details from this accounts document "+
		"(same format/file the server boots from, MAILROOM_ACCOUNTS_FILE) instead of -host/-user/-password-file; "+
		"requires -account")
	account := fs.String("account", "", "the account name to use from -accounts-file")
	jsonOut := fs.Bool("json", false, "emit the report as JSON instead of plain text")
	probeConnections := fs.Int("probe-connections", 0, fmt.Sprintf(
		"dial+hold up to N connections to find the server's connection cap (0 = off, max %d)", probeMaxHardCap))
	probeSpacing := fs.Duration("probe-spacing", 500*time.Millisecond, "pause between probe connection attempts")
	messagesOut := fs.Bool("messages", false, "instead of the report, print one tab-separated line per message: "+
		"mailbox, uidvalidity, uid, size (RFC822.SIZE) — for joining against a store's provenance rows. "+
		"Cannot be combined with -json or -probe-connections")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: mailroom-census (-accounts-file PATH -account NAME) | "+
			"(-host H -user U [-password-file F | "+envCensusPassword+"=...]) [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	var portSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portSet = true
		}
	})

	if *probeConnections < 0 {
		return errors.New("-probe-connections must not be negative")
	}
	if *probeConnections > probeMaxHardCap {
		return fmt.Errorf("-probe-connections %d exceeds the hard cap of %d",
			*probeConnections, probeMaxHardCap)
	}
	if *messagesOut && (*jsonOut || *probeConnections > 0) {
		return errors.New("-messages cannot be combined with -json or -probe-connections")
	}

	opts := options{
		host: *host, port: *port, portSet: portSet,
		user: *user, passwordFile: *passwordFile,
		accountsFile: *accountsFile, account: *account,
	}
	cfg, err := connectionConfig(opts, os.Getenv(envCensusPassword))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r, err := census.Dial(ctx, cfg)
	if err != nil {
		return err
	}

	if *messagesOut {
		err := writeMessages(ctx, r, stdout)
		_ = r.Close()
		return err
	}

	rep, err := census.Run(ctx, r, time.Now())
	// Close exactly once, and BEFORE probing: the probe below dials its own
	// fresh connections against the same account, and this one must not
	// still be held open counting against that same cap while it does —
	// that would measure "the cap minus one" and under-report by exactly
	// the connection this process itself is still sitting on.
	_ = r.Close()
	if err != nil {
		return err
	}

	if *probeConnections > 0 {
		result := census.ProbeConnections(ctx, census.DialProbe(cfg), *probeConnections, *probeSpacing)
		rep.Probe = &result
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return fmt.Errorf("encode report: %w", err)
		}
		return nil
	}
	return census.WriteText(stdout, rep)
}

// writeMessages prints the -messages listing: a header line, then one
// tab-separated row per message. A mailbox name containing a tab or newline
// is refused rather than written, because it would silently shift that row's
// columns in whatever joins against this output.
func writeMessages(ctx context.Context, r census.Reader, w io.Writer) error {
	if _, err := fmt.Fprintln(w, "mailbox\tuidvalidity\tuid\tsize"); err != nil {
		return err
	}
	return census.ListMessages(ctx, r, func(row census.MessageRow) error {
		if strings.ContainsAny(row.Mailbox, "\t\r\n") {
			return fmt.Errorf("mailbox name %q contains a tab or newline; refusing to write an ambiguous row", row.Mailbox)
		}
		_, err := fmt.Fprintf(w, "%s\t%d\t%d\t%d\n", row.Mailbox, row.UIDValidity, row.UID, row.Size)
		return err
	})
}

// connectionConfig resolves opts (plus the standalone mode's env-var
// password source) into a census.Config. It performs no network I/O; the
// only I/O is LoadAccountsFile in accounts-file mode. Kept separate from
// run so it can be unit-tested without flag parsing or a live server.
func connectionConfig(o options, envPassword string) (census.Config, error) {
	accountsMode := o.accountsFile != "" || o.account != ""
	hostMode := o.host != "" || o.user != "" || o.passwordFile != "" || envPassword != ""
	if accountsMode && hostMode {
		return census.Config{}, errors.New("-accounts-file/-account cannot be combined with " +
			"-host/-user/-password-file/" + envCensusPassword + " — pick one way to specify the connection")
	}
	if accountsMode {
		return connectionFromAccountsFile(o)
	}
	return connectionFromFlags(o, envPassword)
}

// connectionFromAccountsFile resolves -accounts-file/-account. Always
// implicit TLS: accountSpec (cmd/mailroom/main.go) hardcodes
// imapsync.TLSImplicit for every account it builds from the accounts file,
// and this mirrors that default — there is no override, see the package
// doc for why.
func connectionFromAccountsFile(o options) (census.Config, error) {
	if o.accountsFile == "" || o.account == "" {
		return census.Config{}, errors.New("-accounts-file and -account must both be set")
	}

	accounts, err := config.LoadAccountsFile(o.accountsFile)
	if err != nil {
		return census.Config{}, fmt.Errorf("%w", err)
	}
	if len(accounts) == 0 {
		return census.Config{}, fmt.Errorf(
			"no accounts found in %s (missing file, empty document, or no accounts declared)",
			o.accountsFile)
	}

	names := make([]string, 0, len(accounts))
	for _, a := range accounts {
		names = append(names, a.Name)
		if a.Name != o.account {
			continue
		}
		cfg := census.Config{
			Host: a.IMAPHost,
			Port: a.IMAPPort,
			TLS:  census.TLSImplicit,
			// Second call site for config.Secret.Reveal (the doc comment on
			// Reveal names this one): the plaintext goes straight into the
			// read-only IMAP LOGIN below and nowhere else — not logged, not
			// stored, not part of the report.
			Username: a.Username,
			Password: a.Password.Reveal(),
		}
		if o.portSet {
			cfg.Port = o.port
		}
		return cfg, nil
	}
	return census.Config{}, fmt.Errorf("no account named %q in %s; names present: %s",
		o.account, o.accountsFile, strings.Join(names, ", "))
}

func connectionFromFlags(o options, envPassword string) (census.Config, error) {
	if o.host == "" {
		return census.Config{}, errors.New("-host is required (or use -accounts-file/-account)")
	}
	if o.user == "" {
		return census.Config{}, errors.New("-user is required (or use -accounts-file/-account)")
	}
	password, err := resolvePassword(o.passwordFile, envPassword)
	if err != nil {
		return census.Config{}, err
	}
	return census.Config{
		Host:     o.host,
		Port:     o.port,
		TLS:      census.TLSImplicit,
		Username: o.user,
		Password: password,
	}, nil
}

// resolvePassword returns the password from -password-file (trimming
// exactly one trailing newline, then one trailing carriage return, so both
// Unix and Windows-authored secret files work) or MAILROOM_CENSUS_PASSWORD.
func resolvePassword(passwordFile, envPassword string) (string, error) {
	if passwordFile != "" {
		b, err := os.ReadFile(passwordFile)
		if err != nil {
			return "", fmt.Errorf("read -password-file: %w", err)
		}
		s := strings.TrimSuffix(string(b), "\n")
		s = strings.TrimSuffix(s, "\r")
		if s == "" {
			return "", errors.New("-password-file is empty")
		}
		return s, nil
	}
	if envPassword != "" {
		return envPassword, nil
	}
	return "", fmt.Errorf("a password is required: -password-file or %s "+
		"(there is deliberately no -password flag — see the package doc)", envCensusPassword)
}
