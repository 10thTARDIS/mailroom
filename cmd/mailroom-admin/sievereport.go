// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/onboarding"
	"mailroom/internal/provider"
	"mailroom/internal/sieve"
	"mailroom/internal/store"
)

// `mailroom-admin sieve-report` — what each account's ManageSieve server holds
// BEFORE Mailroom writes anything to it (#50, report first).
//
// The provider allows one active Sieve script, and the operator may already
// have filters or a vacation message set in the provider's webmail. A vacation
// responder that activated its own script would silently switch those off. So
// this reports, per account: the server's capabilities (is vacation, vacation-
// seconds, include supported?), every script with its size and whether it is
// active, and for the active script a STRUCTURAL summary — the extensions it
// requires, the commands and tests it uses, whether it already sends a vacation
// reply. It never prints a script body unless --show-active asks for the
// active one: bodies hold personal addresses and folder names.
//
// It is read-only by construction, not by care: internal/sieve's client can
// only send CAPABILITY, STARTTLS, AUTHENTICATE, LISTSCRIPTS, GETSCRIPT and
// LOGOUT, and this command touches the database only through UserTx reads.
func runSieveReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sieve-report", flag.ContinueOnError)
	var (
		userIDStr   = fs.String("user-id", "", "operator user id (ULID)")
		account     = fs.String("account", "", "report only the account with this name (default: every account)")
		showActive  = fs.Bool("show-active", false, "also print the ACTIVE script's body (it may contain personal addresses)")
		host        = fs.String("host", "", "ManageSieve host override (default: derived from the provider profile — the account's IMAP host)")
		port        = fs.Int("port", 0, "ManageSieve port override (default: 4190)")
		implicitTLS = fs.Bool("implicit-tls", false, "dial TLS immediately instead of STARTTLS")
		timeout     = fs.Duration("timeout", sieve.DefaultTimeout, "bound on each account's whole session")
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

	targets, err := loadSieveTargets(ctx, d.store, d.keys, userID, *account)
	if err != nil {
		return fmt.Errorf("sieve-report: %w", err)
	}
	failed := reportSieve(ctx, os.Stdout, targets, sieveReportOptions{
		Host:              *host,
		Port:              *port,
		ImplicitTLS:       *implicitTLS,
		ShowActive:        *showActive,
		AllowPrivateHosts: d.allowPrivateAccountHosts,
		Timeout:           *timeout,
	})
	if failed > 0 {
		return fmt.Errorf("sieve-report: %d of %d account(s) could not be read — see above", failed, len(targets))
	}
	return nil
}

// sieveTarget is one account to report, with its credential already opened.
type sieveTarget struct {
	Name        string
	Profile     string
	IMAPHost    string
	SyncEnabled bool
	Username    string
	Password    string
}

// loadSieveTargets reads the user's accounts under RLS (UserTx: another user's
// accounts are invisible, so naming one is "no such account") and opens their
// sealed IMAP credentials, which are the mail login ManageSieve accepts.
func loadSieveTargets(ctx context.Context, st *store.Store, keys crypto.KeyProvider, userID ulid.ULID, only string) ([]sieveTarget, error) {
	var accounts []store.Account
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		accounts, e = tx.ListAccounts(ctx)
		return e
	}); err != nil {
		return nil, err
	}
	if only != "" {
		var names []string
		var keep []store.Account
		for _, a := range accounts {
			names = append(names, a.Name)
			if a.Name == only {
				keep = append(keep, a)
			}
		}
		if len(keep) == 0 {
			return nil, fmt.Errorf("no account named %q for this user (have: %s)", only, strings.Join(names, ", "))
		}
		accounts = keep
	}
	if len(accounts) == 0 {
		return nil, errors.New("this user has no accounts")
	}
	master, err := keys.MasterKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	defer crypto.Zero(master)
	out := make([]sieveTarget, 0, len(accounts))
	for _, a := range accounts {
		creds, err := onboarding.OpenCredentials(master, userID, a.CredentialsCT)
		if err != nil {
			return nil, fmt.Errorf("open credentials for account %q: %w", a.Name, err)
		}
		out = append(out, sieveTarget{
			Name: a.Name, Profile: a.ProviderProfile, IMAPHost: a.IMAPHost, SyncEnabled: a.SyncEnabled,
			Username: creds.IMAPUsername, Password: creds.IMAPPassword,
		})
	}
	return out, nil
}

// sieveReportOptions carries the flags plus the test seams.
type sieveReportOptions struct {
	Host              string
	Port              int
	ImplicitTLS       bool
	ShowActive        bool
	AllowPrivateHosts bool
	Timeout           time.Duration
	// TLSConfig is nil in production; tests trust the fake server's CA.
	TLSConfig *tls.Config
	// open is the session seam; nil means sieve.Open.
	open func(context.Context, sieve.Config) (sieve.Reader, error)
}

// reportSieve prints the report for every target and returns how many could
// not be read. One account's failure never hides another's report.
func reportSieve(ctx context.Context, w io.Writer, targets []sieveTarget, o sieveReportOptions) int {
	open := o.open
	if open == nil {
		open = func(ctx context.Context, c sieve.Config) (sieve.Reader, error) { return sieve.Open(ctx, c) }
	}
	failed := 0
	for i, t := range targets {
		if i > 0 {
			wln(w)
		}
		if err := reportOneSieve(ctx, w, t, o, open); err != nil {
			failed++
			wf(w, "  ERROR: %v\n", err)
		}
	}
	wf(w, "\n%d account(s): %d read, %d failed\n", len(targets), len(targets)-failed, failed)
	return failed
}

func reportOneSieve(ctx context.Context, w io.Writer, t sieveTarget, o sieveReportOptions,
	open func(context.Context, sieve.Config) (sieve.Reader, error)) error {
	syncState := "sync on"
	if !t.SyncEnabled {
		syncState = "sync off"
	}
	wf(w, "account %q (provider profile %q, %s)\n", t.Name, t.Profile, syncState)

	ep, ok, why := provider.ManageSieveEndpoint(t.Profile, t.IMAPHost)
	if !ok && o.Host == "" {
		wf(w, "  skipped: %s (pass --host to try anyway)\n", why)
		return nil
	}
	if o.Host != "" {
		ep.Host, ep.Source = o.Host, "--host"
	}
	if o.Port != 0 {
		ep.Port, ep.Source = o.Port, ep.Source+", --port"
	}
	if ep.Port == 0 {
		ep.Port = sieve.DefaultPort
	}
	if o.ImplicitTLS {
		ep.ImplicitTLS, ep.Source = true, ep.Source+", --implicit-tls"
	}
	mode := "STARTTLS"
	if ep.ImplicitTLS {
		mode = "implicit TLS"
	}
	wf(w, "  endpoint: %s:%d, %s (from %s)\n", ep.Host, ep.Port, mode, ep.Source)
	if t.Username == "" {
		return errors.New("the account has no stored IMAP username to log in with")
	}

	ctx, cancel := context.WithTimeout(ctx, timeoutOr(o.Timeout))
	defer cancel()
	sess, err := open(ctx, sieve.Config{
		Host: ep.Host, Port: ep.Port, ImplicitTLS: ep.ImplicitTLS,
		Username: t.Username, Password: t.Password,
		AllowPrivateHosts: o.AllowPrivateHosts, Timeout: timeoutOr(o.Timeout),
		TLSConfig: o.TLSConfig,
	})
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	renderSieveCaps(w, sess)

	scripts, err := sess.ListScripts(ctx)
	if err != nil {
		return err
	}
	// Only the ACTIVE script's body is held. It is fetched first, so a broken
	// fetch of some other script cannot cost the one that matters. Every other
	// script is fetched one at a time for its size and vacation flag and its
	// body dropped at once: ManageSieve's LISTSCRIPTS carries no sizes, and
	// holding N bodies of up to sieve.MaxScriptBytes each is what the bound
	// per response exists to prevent.
	active := -1
	for i, sc := range scripts {
		if sc.Active {
			active = i
			break
		}
	}
	type scriptFacts struct {
		size     int
		vacation bool
		err      error
	}
	facts := make([]scriptFacts, len(scripts))
	var activeBody string
	// usable turns false after an error that is not a clean NO: the response
	// was abandoned mid-stream, so the session is out of step with the server.
	usable := true
	fetch := func(i int) string {
		if !usable {
			facts[i].err = errors.New("not fetched: the session was abandoned after an earlier error")
			return ""
		}
		body, err := sess.GetScript(ctx, scripts[i].Name)
		if err != nil {
			var se *sieve.ServerError
			if !errors.As(err, &se) {
				usable = false
			}
			facts[i].err = err
			return ""
		}
		facts[i].size, facts[i].vacation = len(body), sieve.Summarize(body).Vacation
		return body
	}
	if active >= 0 {
		activeBody = fetch(active)
	}
	for i := range scripts {
		if i != active {
			_ = fetch(i)
		}
	}
	nActive := 0
	for _, sc := range scripts {
		if sc.Active {
			nActive++
		}
	}
	wf(w, "  scripts: %d (%d active)\n", len(scripts), nActive)
	for i, sc := range scripts {
		mark, state := " ", ""
		if sc.Active {
			mark, state = "*", "  ACTIVE"
		}
		if facts[i].err != nil {
			wf(w, "    %s %q  size unknown (%v)%s\n", mark, sieve.Printable(sc.Name), facts[i].err, state)
			continue
		}
		vac := ""
		if facts[i].vacation {
			vac = "  [contains vacation]"
		}
		wf(w, "    %s %q  %d bytes%s%s\n", mark, sieve.Printable(sc.Name), facts[i].size, state, vac)
	}

	caps := sess.Capabilities()
	if active < 0 {
		wln(w, "  active script: none — no user Sieve script runs for this account (provider-wide filters, if any, are not visible here)")
		wln(w, "  coexistence: nothing would be displaced by activating a Mailroom script")
	} else {
		name := sieve.Printable(scripts[active].Name)
		if facts[active].err != nil {
			wf(w, "  active script %q: could not be read: %v\n", name, facts[active].err)
		} else {
			renderSieveSummary(w, name, sieve.Summarize(activeBody))
		}
		if caps.HasSieve("include") {
			wln(w, "  coexistence: a script IS active; the server supports include, so a Mailroom script could include it")
		} else {
			wln(w, "  coexistence: a script IS active and the server lacks include — activating another would switch it off")
		}
		if o.ShowActive && facts[active].err == nil {
			// Escaped, not raw: the body is server-supplied bytes, and a
			// terminal executes escape sequences. Tab, CR and LF are kept.
			wf(w, "  --- active script %q body (--show-active; control characters shown as escapes) ---\n", name)
			wbody(w, activeBody)
			if !strings.HasSuffix(activeBody, "\n") {
				wln(w)
			}
			wln(w, "  --- end of body ---")
		}
	}
	if !usable {
		_ = sess.Close()
		return errors.New("the session was abandoned mid-response; the report above is incomplete")
	}
	if err := sess.Logout(ctx); err != nil {
		wf(w, "  note: LOGOUT: %v\n", err)
	}
	return nil
}

func timeoutOr(d time.Duration) time.Duration {
	if d <= 0 {
		return sieve.DefaultTimeout
	}
	return d
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func renderSieveCaps(w io.Writer, sess sieve.Reader) {
	c := sess.Capabilities()
	tlsName := sess.TLS()
	if tlsName == "" {
		tlsName = "NONE (cleartext)"
	}
	wf(w, "  server: %q, ManageSieve %s, %s\n", c.Implementation, c.Version, tlsName)
	wf(w, "  sasl (before login): %s\n", strings.Join(sess.PreAuthCapabilities().SASL, " "))
	wf(w, "  vacation: %s   vacation-seconds: %s   include: %s   variables: %s   date: %s   relational: %s\n",
		yesNo(c.HasSieve("vacation")), yesNo(c.HasSieve("vacation-seconds")), yesNo(c.HasSieve("include")),
		yesNo(c.HasSieve("variables")), yesNo(c.HasSieve("date")), yesNo(c.HasSieve("relational")))
	wf(w, "  sieve extensions (%d): %s\n", len(c.Sieve), strings.Join(c.Sieve, " "))
	if len(c.Other) > 0 {
		keys := make([]string, 0, len(c.Other))
		for k := range c.Other {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%s=%q", sieve.Printable(k), sieve.Printable(c.Other[k]))
		}
		wf(w, "  other capabilities: %s\n", strings.Join(parts, " "))
	}
}

func renderSieveSummary(w io.Writer, name string, s sieve.Summary) {
	wf(w, "  active script %q: %d bytes, %d lines\n", name, s.Bytes, s.Lines)
	if s.Complete {
		wln(w, "    parsed: completely")
	} else {
		wf(w, "    parsed: ONLY THE FIRST %d of %d bytes (%s) — every count below is a lower bound\n",
			s.StoppedAt, s.Bytes, s.Problem)
	}
	wf(w, "    require (%d): %s\n", len(s.Require), orNone(strings.Join(s.Require, " ")))
	wf(w, "    commands: %s\n", orNone(strings.Join(sieve.SortedCounts(s.Commands), " ")))
	wf(w, "    tests: %s\n", orNone(strings.Join(sieve.SortedCounts(s.Tests), " ")))
	if s.Vacation {
		wf(w, "    vacation action: PRESENT (tags: %s)\n", orNone(strings.Join(s.VacationTags, " ")))
	} else {
		wln(w, "    vacation action: none")
	}
	wf(w, "    include commands: %d\n", s.Includes)
}

// wf, wln and wbody are the ONLY ways this report writes, and each one
// sanitises what it is given, so a new line cannot forget to (#50).
//
// Almost everything the report prints can carry server-controlled bytes: the
// IMPLEMENTATION and SASL capabilities, extension and script names, and every
// error — a NO's text, a protocol error quoting a fragment, a dial or TLS
// error naming what the server sent. wf and wln therefore run EVERY string
// and error argument through sieve.Printable, which drops control characters
// (ESC, a bare CR that would overwrite the line, C1 CSI). Only the format
// string, which is ours, is printed as written. A write error is discarded:
// a report on a terminal or a pipe has nothing useful to do with one.
func wf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, cleanArgs(a)...) }

func wln(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, cleanArgs(a)...) }

// wbody prints a script body for --show-active. It keeps the line structure
// (tab, CRLF, LF) and shows every other control character as an escape.
func wbody(w io.Writer, body string) { _, _ = fmt.Fprint(w, sieve.EscapeControls(body)) }

func cleanArgs(a []any) []any {
	out := make([]any, len(a))
	for i, v := range a {
		switch x := v.(type) {
		case string:
			out[i] = sieve.Printable(x)
		case error:
			out[i] = sieve.Printable(x.Error())
		case fmt.Stringer:
			out[i] = sieve.Printable(x.String())
		case []string:
			c := make([]string, len(x))
			for j, e := range x {
				c[j] = sieve.Printable(e)
			}
			out[i] = c
		default:
			out[i] = v
		}
	}
	return out
}
