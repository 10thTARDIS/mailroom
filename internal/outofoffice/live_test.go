// SPDX-License-Identifier: AGPL-3.0-only

//go:build sievelive

package outofoffice

// The enable/disable sequences against a REAL Dovecot Pigeonhole
// (scripts/sieve-live-test.sh). The fake copies Pigeonhole's bytes; this is
// what proves the generated script COMPILES on a real server (CHECKSCRIPT and
// PUTSCRIPT accept it), that GETSCRIPT returns it byte-for-byte (which is what
// drift detection compares), and that the operator's rules hold there too.
// It fails rather than skips without a server.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"mailroom/internal/sieve"
)

func liveWriter(t *testing.T, user string) *sieve.WriteSession {
	t.Helper()
	host := os.Getenv("MAILROOM_TEST_SIEVE_HOST")
	port, _ := strconv.Atoi(os.Getenv("MAILROOM_TEST_SIEVE_PORT"))
	if host == "" || port == 0 {
		t.Fatal("MAILROOM_TEST_SIEVE_HOST/PORT unset: run via scripts/sieve-live-test.sh")
	}
	w, err := sieve.OpenWrite(context.Background(), sieve.Config{
		Host: host, Port: port, Username: user, Password: "pass", AllowPrivateHosts: true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, // the stock image's self-signed certificate
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func liveUser() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "oof" + hex.EncodeToString(b)
}

func liveScripts(t *testing.T, w *sieve.WriteSession) map[string]bool {
	t.Helper()
	list, err := w.ListScripts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, s := range list {
		out[s.Name] = s.Active
	}
	return out
}

// fullScript is every feature at once: the window, contacts-only, a subject
// and body that need escaping and dot-stuffing.
func fullScript(t *testing.T) string {
	from := time.Now().UTC().Add(-time.Hour)
	to := from.Add(48 * time.Hour)
	s := baseSettings()
	s.From, s.To = &from, &to
	s.Subject = `Out "of" office \ until Monday`
	s.Body = "Hello,\n.\n..I am away.\n"
	s.ContactsOnly = true
	for i := 0; i < 50; i++ {
		s.Contacts = append(s.Contacts, fmt.Sprintf("c%d@example.net", i))
	}
	return mustBuild(t, s, 49152).Script
}

func TestLiveEnableDisableAndInactiveScriptsUntouched(t *testing.T) {
	ctx := context.Background()
	user := liveUser()
	w := liveWriter(t, user)
	// The production picture: one inactive script the user made elsewhere.
	if err := w.PutScript(ctx, "imported", importedBody); err != nil {
		t.Fatal(err)
	}

	body := fullScript(t)
	if err := Install(ctx, w, body); err != nil {
		t.Fatalf("Install on real Pigeonhole: %v", err)
	}
	if got := liveScripts(t, w); !got[ScriptName] || got["imported"] {
		t.Fatalf("after Install: %v", got)
	}
	// GETSCRIPT returns exactly what was stored, so drift detection can trust
	// a SHA-256 comparison.
	stored, err := w.GetScript(ctx, ScriptName)
	if err != nil || stored != body {
		t.Fatalf("Pigeonhole did not return the script byte-for-byte (%v)", err)
	}
	sum := sha256.Sum256([]byte(body))
	if ins, err := Inspect(ctx, w, sum[:]); err != nil || ins.State != StateActive {
		t.Fatalf("Inspect: %+v %v", ins, err)
	}

	out, _, err := Remove(ctx, w)
	if err != nil || out != RemovedActive {
		t.Fatalf("Remove: %v %v", out, err)
	}
	got := liveScripts(t, w)
	if _, still := got[ScriptName]; still || got["imported"] || len(got) != 1 {
		t.Fatalf("after Remove: %v", got)
	}
	if b, err := w.GetScript(ctx, "imported"); err != nil || b != importedBody {
		t.Fatalf("the inactive script changed: %q %v", b, err)
	}
}

func TestLiveRefusalWhileAForeignScriptIsActive(t *testing.T) {
	ctx := context.Background()
	w := liveWriter(t, liveUser())
	if err := w.PutScript(ctx, "webmail", "keep;\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.SetActive(ctx, "webmail"); err != nil {
		t.Fatal(err)
	}
	err := Install(ctx, w, fullScript(t))
	var fe *ForeignActiveError
	if !errors.As(err, &fe) || fe.Script != "webmail" {
		t.Fatalf("err = %v, want a refusal naming webmail", err)
	}
	got := liveScripts(t, w)
	if _, stored := got[ScriptName]; stored || !got["webmail"] {
		t.Fatalf("after the refusal: %v", got)
	}
}

// TestLiveRealServerRejectsABrokenScript: CHECKSCRIPT is a real compile, so a
// script Mailroom got wrong would be stopped before it is stored.
func TestLiveRealServerRejectsABrokenScript(t *testing.T) {
	ctx := context.Background()
	w := liveWriter(t, liveUser())
	err := Install(ctx, w, "require \"vacation\";\r\nvacation :days \"x\";\r\n")
	if err == nil {
		t.Fatal("Pigeonhole accepted a broken script")
	}
	if got := liveScripts(t, w); len(got) != 0 {
		t.Fatalf("a rejected script was stored: %v", got)
	}
}

// TestLiveEmitScriptsForTheInterpreter writes three generated scripts (the
// window now, already ended, not yet started) to MAILROOM_SIEVE_EMIT_DIR for
// scripts/sieve-behaviour.sh, which runs them through Pigeonhole's sieve-test
// in the dovecot container and asserts who gets a reply.
func TestLiveEmitScriptsForTheInterpreter(t *testing.T) {
	dir := os.Getenv("MAILROOM_SIEVE_EMIT_DIR")
	if dir == "" {
		t.Fatal("MAILROOM_SIEVE_EMIT_DIR unset: run via scripts/sieve-live-test.sh")
	}
	now := time.Now().UTC()
	windows := map[string][2]time.Time{
		"current": {now.Add(-time.Hour), now.Add(48 * time.Hour)},
		"expired": {now.Add(-96 * time.Hour), now.Add(-time.Hour)},
		"future":  {now.Add(24 * time.Hour), now.Add(96 * time.Hour)},
	}
	for name, w := range windows {
		from, to := w[0], w[1]
		s := baseSettings()
		s.From, s.To = &from, &to
		s.ContactsOnly = true
		s.Contacts = []string{"friend@example.net", "someone@example.net"}
		b := mustBuild(t, s, 49152)
		if err := os.WriteFile(dir+"/"+name+".sieve", []byte(b.Script), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
