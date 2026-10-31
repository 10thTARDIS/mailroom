// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"mailroom/internal/sieve"
	"mailroom/internal/sieve/sievetest"
)

// The active script carries markers wherever a personal value would sit; the
// default report must print none of them.
const sieveReportActive = "require [\"fileinto\", \"vacation\"];\r\n" +
	"if address :is \"from\" \"MARKER-ADDR@example.com\" { fileinto \"MARKER-FOLDER\"; stop; }\r\n" +
	"vacation :days 5 :subject \"MARKER-SUBJECT\" text:\r\nMARKER-BODY\r\n.\r\n;\r\n"

const sieveReportInactive = "redirect \"MARKER-INACTIVE@example.com\";\r\n"

func sieveFake(t *testing.T, opts sievetest.Options) *sievetest.Server {
	t.Helper()
	opts.Username, opts.Password = "op@example.com", "pw"
	if opts.Scripts == nil {
		opts.Scripts = []sievetest.Script{
			{Name: "webmail", Body: sieveReportActive, Active: true},
			{Name: "forwarding", Body: sieveReportInactive},
		}
	}
	return sievetest.New(t, opts)
}

func runSieveReportAgainst(t *testing.T, f *sievetest.Server, targets []sieveTarget, mutate func(*sieveReportOptions)) (string, int) {
	t.Helper()
	o := sieveReportOptions{
		Host: f.Host(), Port: f.Port(), AllowPrivateHosts: true, TLSConfig: f.ClientTLSConfig(),
	}
	if mutate != nil {
		mutate(&o)
	}
	var buf bytes.Buffer
	failed := reportSieve(context.Background(), &buf, targets, o)
	return buf.String(), failed
}

func opTarget(name string) sieveTarget {
	return sieveTarget{Name: name, Profile: "migadu", IMAPHost: "imap.example.com", SyncEnabled: true,
		Username: "op@example.com", Password: "pw"}
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("report lacks %q\n---\n%s", w, out)
		}
	}
}

func TestSieveReportDefaultIsStructuralAndReadOnly(t *testing.T) {
	f := sieveFake(t, sievetest.Options{})
	out, failed := runSieveReportAgainst(t, f, []sieveTarget{opTarget("personal")}, nil)
	if failed != 0 {
		t.Fatalf("failed = %d\n%s", failed, out)
	}
	mustContain(t, out,
		`account "personal" (provider profile "migadu", sync on)`,
		`server: "Dovecot Pigeonhole", ManageSieve 1.0, TLS 1.3`,
		"vacation: yes   vacation-seconds: no   include: yes",
		"scripts: 2 (1 active)",
		`* "webmail"  `+strconv.Itoa(len(sieveReportActive))+` bytes  ACTIVE  [contains vacation]`,
		`  "forwarding"  `+strconv.Itoa(len(sieveReportInactive))+` bytes`,
		"parsed: completely",
		"require (2): fileinto vacation",
		"commands: fileinto×1 if×1 require×1 stop×1 vacation×1",
		"tests: address×1",
		"vacation action: PRESENT (tags: :days :subject)",
		"coexistence: a script IS active; the server supports include",
		"1 account(s): 1 read, 0 failed",
	)
	if strings.Contains(out, "MARKER") {
		t.Fatalf("the default report printed a script value:\n%s", out)
	}
	if n := f.WriteAttempts(); n != 0 {
		t.Fatalf("the report sent %d state-changing verb(s)", n)
	}
	for _, v := range f.Verbs() {
		switch v {
		case "CAPABILITY", "STARTTLS", "AUTHENTICATE", "LISTSCRIPTS", "GETSCRIPT", "LOGOUT":
		default:
			t.Errorf("the report sent %q", v)
		}
	}
}

// TestSieveReportShowActivePrintsOnlyTheActiveBody: --show-active is the one
// way to see a body, and it shows the ACTIVE one only.
func TestSieveReportShowActivePrintsOnlyTheActiveBody(t *testing.T) {
	f := sieveFake(t, sievetest.Options{})
	out, _ := runSieveReportAgainst(t, f, []sieveTarget{opTarget("personal")},
		func(o *sieveReportOptions) { o.ShowActive = true })
	mustContain(t, out, "MARKER-ADDR@example.com", "MARKER-BODY", `--- active script "webmail" body (--show-active; control characters shown as escapes) ---`)
	if strings.Contains(out, "MARKER-INACTIVE") {
		t.Fatalf("--show-active printed an inactive script's body:\n%s", out)
	}
}

func TestSieveReportNoActiveScriptAndNoInclude(t *testing.T) {
	f := sieveFake(t, sievetest.Options{
		Sieve:   "fileinto vacation",
		Scripts: []sievetest.Script{{Name: "draft", Body: "keep;\r\n"}},
	})
	out, _ := runSieveReportAgainst(t, f, []sieveTarget{opTarget("a")}, nil)
	mustContain(t, out, "scripts: 1 (0 active)", "active script: none",
		"nothing would be displaced", "include: no")

	g := sieveFake(t, sievetest.Options{Sieve: "fileinto vacation"})
	out, _ = runSieveReportAgainst(t, g, []sieveTarget{opTarget("b")}, nil)
	mustContain(t, out, "the server lacks include — activating another would switch it off")
}

// TestSieveReportOneFailureDoesNotHideTheNext: a wrong password on one account
// is reported, counted, and the next account is still read.
func TestSieveReportOneFailureDoesNotHideTheNext(t *testing.T) {
	f := sieveFake(t, sievetest.Options{})
	bad := opTarget("broken")
	bad.Password = "wrong"
	out, failed := runSieveReportAgainst(t, f, []sieveTarget{bad, opTarget("fine")}, nil)
	if failed != 1 {
		t.Fatalf("failed = %d, want 1\n%s", failed, out)
	}
	mustContain(t, out, "ERROR: sieve: AUTHENTICATE: server said NO", `account "fine"`,
		"scripts: 2 (1 active)", "2 account(s): 1 read, 1 failed")
}

// TestSieveReportSkipsAProfileWithNoManageSieve: a Gmail account is skipped
// without a dial; --host overrides the skip.
func TestSieveReportSkipsAProfileWithNoManageSieve(t *testing.T) {
	f := sieveFake(t, sievetest.Options{})
	g := opTarget("gmail")
	g.Profile = "gmail"
	out, failed := runSieveReportAgainst(t, f, []sieveTarget{g}, func(o *sieveReportOptions) { o.Host = "" })
	if failed != 0 || !strings.Contains(out, "skipped: Gmail offers no ManageSieve service") {
		t.Fatalf("failed=%d\n%s", failed, out)
	}
	if len(f.Verbs()) != 0 {
		t.Fatal("a skipped account was dialled")
	}
}

// TestSieveReportDerivesTheEndpoint: with no --host the endpoint is the
// account's IMAP host on 4190 via STARTTLS, and the open seam receives it.
func TestSieveReportDerivesTheEndpoint(t *testing.T) {
	var got sieve.Config
	open := func(_ context.Context, c sieve.Config) (sieve.Reader, error) {
		got = c
		return nil, context.Canceled
	}
	var buf bytes.Buffer
	failed := reportSieve(context.Background(), &buf, []sieveTarget{opTarget("p")}, sieveReportOptions{open: open})
	if failed != 1 {
		t.Fatalf("failed = %d", failed)
	}
	if got.Host != "imap.example.com" || got.Port != 4190 || got.ImplicitTLS || got.Username != "op@example.com" {
		t.Fatalf("derived config = %+v", got)
	}
	if !strings.Contains(buf.String(), "endpoint: imap.example.com:4190, STARTTLS (from the account's IMAP host") {
		t.Fatalf("%s", buf.String())
	}
}

// TestSieveReportShowActiveEscapesControlCharacters: a body carrying terminal
// escape sequences is printed with them visible, never raw; tabs and line
// breaks survive so the script still reads as one.
func TestSieveReportShowActiveEscapesControlCharacters(t *testing.T) {
	body := "require \"vacation\";\r\n\tvacation \"\x1b[2J\x1b]0;pwned\x07gone\u009b\";\r\n"
	f := sieveFake(t, sievetest.Options{Scripts: []sievetest.Script{{Name: "evil\x1b[31m", Body: body, Active: true}}})
	out, failed := runSieveReportAgainst(t, f, []sieveTarget{opTarget("p")},
		func(o *sieveReportOptions) { o.ShowActive = true })
	if failed != 0 {
		t.Fatalf("failed=%d\n%s", failed, out)
	}
	for _, r := range out {
		if (r < 0x20 && r != '\t' && r != '\r' && r != '\n') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			t.Fatalf("raw control character %U reached the output:\n%q", r, out)
		}
	}
	mustContain(t, out, `\x1b[2J\x1b]0;pwned\x07gone\u009b`, "\tvacation")
}

// TestSieveReportOversizedInactiveScript: a non-active script the server will
// not serve within the bound does not cost the active one its summary, and
// the report says the session was abandoned rather than reporting success.
func TestSieveReportOversizedInactiveScript(t *testing.T) {
	huge := strings.Repeat("#", sieve.MaxScriptBytes+1)
	f := sieveFake(t, sievetest.Options{Scripts: []sievetest.Script{
		{Name: "big", Body: huge},
		{Name: "webmail", Body: sieveReportActive, Active: true},
		{Name: "after", Body: "keep;"},
	}})
	out, failed := runSieveReportAgainst(t, f, []sieveTarget{opTarget("p")}, nil)
	if failed != 1 {
		t.Fatalf("failed=%d, want 1 (the session is out of step after the oversized literal)\n%s", failed, out)
	}
	mustContain(t, out, "vacation action: PRESENT", `"big"  size unknown`, "exceeds", `"after"  size unknown (not fetched`)
}

// hostileText carries ESC sequences, a bare CR (which would return the cursor
// and overwrite the line already printed), BEL and a C1 CSI.
const hostileText = "\x1b[2J\x1b]0;t\x07\rOVERWRITE\u009b[31m"

// TestSieveReportSanitisesEveryServerString: hostile bytes in the
// IMPLEMENTATION capability, in script names, and in the text of three error
// responses (a failed AUTHENTICATE -> the ERROR line, a refused GETSCRIPT ->
// "size unknown", a refused LOGOUT -> the note) never reach the output raw.
// Only LF may appear: the report itself prints no tab and no CR.
func TestSieveReportSanitisesEveryServerString(t *testing.T) {
	f := sieveFake(t, sievetest.Options{
		Implementation: "Pigeon" + hostileText,
		ErrorText:      "nope " + hostileText,
		Scripts: []sievetest.Script{
			{Name: "act" + hostileText, Body: sieveReportActive, Active: true},
			{Name: "denied" + hostileText, Body: "keep;", Deny: true},
		},
	})
	bad := opTarget("bad")
	bad.Password = "wrong"
	out, failed := runSieveReportAgainst(t, f, []sieveTarget{bad, opTarget("good")}, nil)
	if failed != 1 {
		t.Fatalf("failed = %d, want 1 (the wrong password)\n%q", failed, out)
	}
	for i, r := range out {
		if (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			t.Fatalf("raw control character %U at byte %d reached the output:\n%q", r, i, out)
		}
	}
	// The text survives with its control characters removed, on every path.
	mustContain(t, out,
		"ERROR: sieve: AUTHENTICATE: server said NO: nope [2J]0;tOVERWRITE[31m",
		`server: "Pigeon[2J]0;tOVERWRITE[31m"`,
		`"act[2J]0;tOVERWRITE[31m"`,
		`"denied[2J]0;tOVERWRITE[31m"  size unknown (sieve: GETSCRIPT: server said NO: nope [2J]0;tOVERWRITE[31m)`,
		"note: LOGOUT: sieve: LOGOUT: server said NO: nope [2J]0;tOVERWRITE[31m",
	)
}

// TestSieveReportWritesOnlyThroughTheSanitisingHelpers keeps the guarantee
// above from depending on memory: outside wf, wln and wbody, sievereport.go
// never calls a fmt print function, so a new line has to go through a helper
// that sanitises it.
func TestSieveReportWritesOnlyThroughTheSanitisingHelpers(t *testing.T) {
	src, err := os.ReadFile("sievereport.go")
	if err != nil {
		t.Fatal(err)
	}
	helpers := map[string]bool{"wf": true, "wln": true, "wbody": true}
	current := ""
	found := 0
	for n, ln := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(ln, "func ") {
			name := strings.TrimPrefix(ln, "func ")
			if i := strings.IndexAny(name, "( "); i >= 0 {
				name = name[:i]
			}
			current = name
		}
		for _, call := range []string{"fmt.Print", "fmt.Fprint", "io.WriteString(", ".Write("} {
			if strings.Contains(ln, call) {
				if !helpers[current] {
					t.Errorf("sievereport.go:%d: %s outside the sanitising helpers (in %s)", n+1, call, current)
				}
				found++
			}
		}
	}
	if found != 3 {
		t.Fatalf("found %d print calls in the helpers, want exactly 3 — the scan no longer sees them", found)
	}
}

// hostileSieveReader is a sieve.Reader that hands the report RAW hostile strings,
// bypassing internal/sieve's own sanitising, so the test below exercises the
// report's layer on its own: whatever a future client change lets through,
// the report must still not print it raw.
type hostileSieveReader struct{}

func (hostileSieveReader) Capabilities() sieve.Capabilities {
	return sieve.Capabilities{
		Implementation: "impl" + hostileText, Version: "1.0" + hostileText,
		Sieve: []string{"vacation", "ext" + hostileText}, SASL: []string{"PLAIN" + hostileText},
		Other: map[string]string{"X" + hostileText: "v" + hostileText},
	}
}
func (r hostileSieveReader) PreAuthCapabilities() sieve.Capabilities { return r.Capabilities() }
func (hostileSieveReader) TLS() string                               { return "TLS 1.3" + hostileText }
func (hostileSieveReader) ListScripts(context.Context) ([]sieve.Script, error) {
	return []sieve.Script{{Name: "a" + hostileText, Active: true}, {Name: "b" + hostileText}}, nil
}
func (hostileSieveReader) GetScript(_ context.Context, name string) (string, error) {
	if strings.HasPrefix(name, "b") {
		// A clean NO, so the session stays usable and LOGOUT is reached.
		return "", &sieve.ServerError{Verb: "GETSCRIPT", Status: "NO", Text: "raw get error " + hostileText}
	}
	return "keep;", nil
}
func (hostileSieveReader) Logout(context.Context) error {
	return errors.New("raw logout error " + hostileText)
}
func (hostileSieveReader) Close() error { return nil }

func TestSieveReportLayerSanitisesWhatTheClientHandsIt(t *testing.T) {
	calls := 0
	open := func(context.Context, sieve.Config) (sieve.Reader, error) {
		calls++
		if calls == 1 {
			// A dial or TLS error naming what the server presented.
			return nil, errors.New("raw dial error " + hostileText)
		}
		return hostileSieveReader{}, nil
	}
	var buf bytes.Buffer
	failed := reportSieve(context.Background(), &buf, []sieveTarget{opTarget("one"), opTarget("two")},
		sieveReportOptions{open: open})
	out := buf.String()
	if failed != 1 {
		t.Fatalf("failed = %d\n%q", failed, out)
	}
	for i, r := range out {
		if (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			t.Fatalf("raw control character %U at byte %d reached the output:\n%q", r, i, out)
		}
	}
	mustContain(t, out, "ERROR: raw dial error", "impl[2J", "raw get error", "note: LOGOUT: raw logout error", "PLAIN[2J",
		"2 account(s): 1 read, 1 failed")
}
