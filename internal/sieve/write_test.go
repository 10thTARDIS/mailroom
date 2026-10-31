// SPDX-License-Identifier: AGPL-3.0-only

package sieve

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"mailroom/internal/sieve/sievetest"
)

// TestWriteSessionSurfaceIsExactlyThis pins the writing client: its exported
// methods and the verbs it may send. A new write verb is a change to this
// list, reviewed as such.
func TestWriteSessionSurfaceIsExactlyThis(t *testing.T) {
	typ := reflect.TypeOf(&WriteSession{})
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	want := []string{"Capabilities", "CheckScript", "Close", "DeleteScript", "GetScript", "ListScripts",
		"Logout", "PutScript", "SetActive"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WriteSession methods = %v, want exactly %v", got, want)
	}
	var verbs []string
	for v := range writeSessionVerbs {
		verbs = append(verbs, v)
	}
	sort.Strings(verbs)
	wantVerbs := []string{"AUTHENTICATE", "CAPABILITY", "CHECKSCRIPT", "DELETESCRIPT", "GETSCRIPT",
		"LISTSCRIPTS", "LOGOUT", "PUTSCRIPT", "SETACTIVE", "STARTTLS"}
	if !reflect.DeepEqual(verbs, wantVerbs) {
		t.Fatalf("writeSessionVerbs = %v, want exactly %v", verbs, wantVerbs)
	}
}

// TestWriteSessionRoundTrip: put, activate, deactivate, delete against the
// fake, and the verbs it does not have are refused before the wire.
func TestWriteSessionRoundTrip(t *testing.T) {
	f := sievetest.New(t, sievetest.Options{Username: "u@example.com", Password: "pw", AllowWrites: true})
	w, err := OpenWrite(context.Background(), Config{
		Host: f.Host(), Port: f.Port(), Username: "u@example.com", Password: "pw",
		AllowPrivateHosts: true, TLSConfig: f.ClientTLSConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	ctx := context.Background()
	body := "require \"vacation\";\r\nvacation \"away\";\r\n"
	if err := w.CheckScript(ctx, body); err != nil {
		t.Fatal(err)
	}
	if err := w.PutScript(ctx, "mine", body); err != nil {
		t.Fatal(err)
	}
	if err := w.SetActive(ctx, "mine"); err != nil {
		t.Fatal(err)
	}
	got, err := w.GetScript(ctx, "mine")
	if err != nil || got != body {
		t.Fatalf("GetScript = %q %v", got, err)
	}
	var se *ServerError
	if err := w.DeleteScript(ctx, "mine"); !errors.As(err, &se) || se.Code != "ACTIVE" {
		t.Fatalf("deleting the active script: %v, want NO (ACTIVE)", err)
	}
	if err := w.SetActive(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := w.DeleteScript(ctx, "mine"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"RENAMESCRIPT", "HAVESPACE", "UNAUTHENTICATE"} {
		if _, err := w.c.do(v, "x"); !errors.Is(err, ErrVerbNotPermitted) {
			t.Errorf("%s: %v, want ErrVerbNotPermitted", v, err)
		}
		if strings.Contains(string(f.Raw()), v) {
			t.Errorf("%s reached the wire", v)
		}
	}
	if len(f.Scripts()) != 0 {
		t.Fatalf("scripts left: %+v", f.Scripts())
	}
}

// TestReadSessionStillCannotWrite: the shared conn did not widen Session.
func TestReadSessionStillCannotWrite(t *testing.T) {
	f := sievetest.New(t, sievetest.Options{Username: "u@example.com", Password: "pw", AllowWrites: true})
	s, err := Open(context.Background(), Config{
		Host: f.Host(), Port: f.Port(), Username: "u@example.com", Password: "pw",
		AllowPrivateHosts: true, TLSConfig: f.ClientTLSConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, v := range []string{"PUTSCRIPT", "SETACTIVE", "DELETESCRIPT", "CHECKSCRIPT"} {
		if _, err := s.do(v, "x", "y"); !errors.Is(err, ErrVerbNotPermitted) {
			t.Errorf("read session %s: %v", v, err)
		}
	}
	if f.WriteAttempts() != 0 {
		t.Fatal("a read session reached a server that accepts writes")
	}
}

func TestMaxScriptSize(t *testing.T) {
	for v, want := range map[string]int{"49152": 49152, " 10 ": 10, "x": 0, "-1": 0, "": 0} {
		c := Capabilities{Other: map[string]string{"MAXSCRIPTSIZE": v}}
		if got := c.MaxScriptSize(); got != want {
			t.Errorf("%q: %d, want %d", v, got, want)
		}
	}
	if (Capabilities{}).MaxScriptSize() != 0 {
		t.Error("no capability should mean no limit")
	}
}
