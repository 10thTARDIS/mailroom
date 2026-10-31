// SPDX-License-Identifier: AGPL-3.0-only

package outofoffice

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"mailroom/internal/sieve"
	"mailroom/internal/sieve/sievetest"
)

// imported is the production picture the report found: one INACTIVE script
// the user made elsewhere. It must come out of every sequence byte-identical,
// still inactive, and never read.
const importedBody = "require \"fileinto\";\r\nif header :contains \"subject\" \"x\" { fileinto \"X\"; }\r\n"

func fake(t *testing.T, scripts ...sievetest.Script) *sievetest.Server {
	t.Helper()
	return sievetest.New(t, sievetest.Options{
		Username: "me@example.com", Password: "pw", AllowWrites: true,
		Scripts: append([]sievetest.Script{{Name: "imported", Body: importedBody}}, scripts...),
	})
}

func open(t *testing.T, f *sievetest.Server) *sieve.WriteSession {
	t.Helper()
	w, err := sieve.OpenWrite(context.Background(), sieve.Config{
		Host: f.Host(), Port: f.Port(), Username: "me@example.com", Password: "pw",
		AllowPrivateHosts: true, TLSConfig: f.ClientTLSConfig(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func script(t *testing.T) string {
	t.Helper()
	return mustBuild(t, baseSettings(), 0).Script
}

func get(f *sievetest.Server, name string) (sievetest.Script, bool) {
	for _, sc := range f.Scripts() {
		if sc.Name == name {
			return sc, true
		}
	}
	return sievetest.Script{}, false
}

// assertImportedUntouched: present, inactive, body unchanged, and never named
// in any command Mailroom sent.
func assertImportedUntouched(t *testing.T, f *sievetest.Server) {
	t.Helper()
	sc, ok := get(f, "imported")
	if !ok || sc.Active || sc.Body != importedBody {
		t.Fatalf("the inactive foreign script was touched: %+v (present %v)", sc, ok)
	}
	if strings.Contains(string(f.Raw()), `"imported"`) {
		t.Fatal("a command named the foreign script")
	}
}

func TestInstallOnAServerWithNothingActive(t *testing.T) {
	f := fake(t)
	body := script(t)
	if err := Install(context.Background(), open(t, f), body); err != nil {
		t.Fatalf("Install: %v", err)
	}
	ours, ok := get(f, ScriptName)
	if !ok || !ours.Active || ours.Body != body {
		t.Fatalf("our script after Install: %+v (present %v)", ours, ok)
	}
	assertImportedUntouched(t, f)
	if n := f.VerbCount("GETSCRIPT"); n != 0 {
		t.Errorf("Install read %d script(s); it reads none", n)
	}
	// The sequence, in order: list, check, put, list again, activate.
	var writes []string
	for _, v := range f.Verbs() {
		switch v {
		case "LISTSCRIPTS", "CHECKSCRIPT", "PUTSCRIPT", "SETACTIVE", "DELETESCRIPT":
			writes = append(writes, v)
		}
	}
	if strings.Join(writes, ",") != "LISTSCRIPTS,CHECKSCRIPT,PUTSCRIPT,LISTSCRIPTS,SETACTIVE" {
		t.Errorf("sequence = %v", writes)
	}
}

// TestInstallRefusesWhileAForeignScriptIsActive: the refusal names the script
// and happens BEFORE any state-changing verb is sent.
func TestInstallRefusesWhileAForeignScriptIsActive(t *testing.T) {
	f := fake(t, sievetest.Script{Name: "webmail-filters", Body: "keep;\r\n", Active: true})
	err := Install(context.Background(), open(t, f), script(t))
	var fe *ForeignActiveError
	if !errors.As(err, &fe) || fe.Script != "webmail-filters" {
		t.Fatalf("err = %v, want a refusal naming webmail-filters", err)
	}
	if !strings.Contains(err.Error(), `"webmail-filters" is active`) {
		t.Errorf("the refusal does not explain itself: %s", err)
	}
	if n := f.WriteAttempts(); n != 0 {
		t.Fatalf("the refusal sent %d state-changing verb(s)", n)
	}
	if sc, _ := get(f, "webmail-filters"); !sc.Active {
		t.Fatal("the foreign script is no longer active")
	}
	if _, ok := get(f, ScriptName); ok {
		t.Fatal("our script was stored despite the refusal")
	}
	assertImportedUntouched(t, f)
}

// TestInstallRefusesARaceWithWebmail: a foreign script becomes active between
// PUTSCRIPT and SETACTIVE. Ours is taken back out and nothing is activated.
func TestInstallRefusesARaceWithWebmail(t *testing.T) {
	f := sievetest.New(t, sievetest.Options{
		Username: "me@example.com", Password: "pw", AllowWrites: true, ActivateOnPut: "imported",
		Scripts: []sievetest.Script{{Name: "imported", Body: importedBody}},
	})
	err := Install(context.Background(), open(t, f), script(t))
	var fe *ForeignActiveError
	if !errors.As(err, &fe) || fe.Script != "imported" {
		t.Fatalf("err = %v, want a refusal naming imported", err)
	}
	if f.VerbCount("SETACTIVE") != 0 {
		t.Fatal("SETACTIVE was sent after the second LISTSCRIPTS saw a foreign active script")
	}
	if _, ok := get(f, ScriptName); ok {
		t.Fatal("our just-stored script was left behind")
	}
	if sc, _ := get(f, "imported"); !sc.Active || sc.Body != importedBody {
		t.Fatalf("the script webmail activated was changed: %+v", sc)
	}
}

func TestInstallStopsOnACompileError(t *testing.T) {
	f := sievetest.New(t, sievetest.Options{
		Username: "me@example.com", Password: "pw", AllowWrites: true, InvalidMarker: "vacation",
	})
	err := Install(context.Background(), open(t, f), script(t))
	if err == nil || f.VerbCount("PUTSCRIPT") != 0 || f.VerbCount("SETACTIVE") != 0 {
		t.Fatalf("err = %v; PUTSCRIPT %d SETACTIVE %d after CHECKSCRIPT failed",
			err, f.VerbCount("PUTSCRIPT"), f.VerbCount("SETACTIVE"))
	}
}

// TestInstallUpdatesOurOwnActiveScript: saving new text while on replaces
// ours in place; ours being active is not a "foreign" script.
func TestInstallUpdatesOurOwnActiveScript(t *testing.T) {
	f := fake(t, sievetest.Script{Name: ScriptName, Body: "old", Active: true})
	body := script(t)
	if err := Install(context.Background(), open(t, f), body); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if sc, _ := get(f, ScriptName); !sc.Active || sc.Body != body {
		t.Fatalf("ours = %+v", sc)
	}
}

// TestRemoveRestoresNothingActive: ours was the active script; afterwards
// nothing is active and ours is gone — the state before Install.
func TestRemoveRestoresNothingActive(t *testing.T) {
	f := fake(t, sievetest.Script{Name: ScriptName, Body: "x", Active: true})
	out, other, err := Remove(context.Background(), open(t, f))
	if err != nil || out != RemovedActive || other != "" {
		t.Fatalf("Remove = %v %q %v", out, other, err)
	}
	if _, ok := get(f, ScriptName); ok {
		t.Fatal("our script is still there")
	}
	for _, sc := range f.Scripts() {
		if sc.Active {
			t.Fatalf("%q is active after Remove", sc.Name)
		}
	}
	assertImportedUntouched(t, f)
	if n := f.VerbCount("GETSCRIPT"); n != 0 {
		t.Errorf("Remove read %d script(s); it reads none", n)
	}
}

// TestRemoveLeavesAScriptTheUserActivatedSince: the user switched to their
// own filters in webmail; turning the reply off must not switch those off.
func TestRemoveLeavesAScriptTheUserActivatedSince(t *testing.T) {
	f := fake(t, sievetest.Script{Name: ScriptName, Body: "x"})
	f.Activate("imported")
	out, other, err := Remove(context.Background(), open(t, f))
	if err != nil || out != RemovedLeftOther || other != "imported" {
		t.Fatalf("Remove = %v %q %v", out, other, err)
	}
	if f.VerbCount("SETACTIVE") != 0 {
		t.Fatal("Remove sent SETACTIVE while another script was active")
	}
	if sc, _ := get(f, "imported"); !sc.Active || sc.Body != importedBody {
		t.Fatalf("the user's active script was changed: %+v", sc)
	}
	if _, ok := get(f, ScriptName); ok {
		t.Fatal("our inactive script was not deleted")
	}
}

func TestRemoveWithNothingOfOursIsANoOp(t *testing.T) {
	f := fake(t)
	out, _, err := Remove(context.Background(), open(t, f))
	if err != nil || out != RemovedNothing || f.WriteAttempts() != 0 {
		t.Fatalf("Remove = %v %v, writes %d", out, err, f.WriteAttempts())
	}
}

func TestInspectSeesDrift(t *testing.T) {
	body := script(t)
	sum := sha256.Sum256([]byte(body))
	want := sum[:]
	ctx := context.Background()

	f := fake(t, sievetest.Script{Name: ScriptName, Body: body, Active: true})
	cases := []struct {
		name  string
		setup func()
		want  ServerState
		other string
	}{
		{"exactly ours", func() {}, StateActive, ""},
		{"edited in webmail", func() { f.Replace(ScriptName, "keep;") }, StateModified, ""},
		{"user activated another", func() { f.Replace(ScriptName, body); f.Activate("imported") }, StateDisplaced, "imported"},
		{"deactivated", func() { f.Activate("") }, StateMissing, ""},
	}
	for _, c := range cases {
		c.setup()
		ins, err := Inspect(ctx, open(t, f), want)
		if err != nil || ins.State != c.want || ins.Other != c.other {
			t.Errorf("%s: %+v %v, want %s %q", c.name, ins, err, c.want, c.other)
		}
	}
	empty := fake(t)
	if ins, err := Inspect(ctx, open(t, empty), nil); err != nil || ins.State != StateOff {
		t.Errorf("nothing installed: %+v %v", ins, err)
	}
	if ins, err := Inspect(ctx, open(t, empty), want); err != nil || ins.State != StateMissing {
		t.Errorf("installed but gone: %+v %v", ins, err)
	}
}

// TestInspectNeverReadsAForeignScript: whatever the state, the only body
// Inspect asks for is ours.
func TestInspectNeverReadsAForeignScript(t *testing.T) {
	body := script(t)
	sum := sha256.Sum256([]byte(body))
	f := fake(t, sievetest.Script{Name: ScriptName, Body: body, Active: true})
	if _, err := Inspect(context.Background(), open(t, f), sum[:]); err != nil {
		t.Fatal(err)
	}
	raw := string(f.Raw())
	if strings.Contains(raw, `GETSCRIPT "imported"`) || strings.Count(raw, "GETSCRIPT") != 1 {
		t.Fatalf("Inspect fetched something other than its own script:\n%s", raw)
	}
}

// TestServerSizeLimitIsAdvertisedAndEnforced: MAXSCRIPTSIZE reaches Build,
// and a script over it never reaches PUTSCRIPT.
func TestServerSizeLimitIsAdvertisedAndEnforced(t *testing.T) {
	f := sievetest.New(t, sievetest.Options{
		Username: "me@example.com", Password: "pw", AllowWrites: true, MaxScriptSize: 600,
	})
	w := open(t, f)
	if got := w.Capabilities().MaxScriptSize(); got != 600 {
		t.Fatalf("MaxScriptSize = %d", got)
	}
	s := baseSettings()
	s.Body = strings.Repeat("long line of text ", 40)
	if _, err := Build(s, w.Capabilities().MaxScriptSize()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Build over the advertised limit: %v", err)
	}
}
