// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration tests for EventSource push (RFC 8620 §7.3) against a real Postgres:
// the stream reports the CURRENT state on connect, pushes a StateChange when the
// change log actually moves, and unwinds cleanly on client disconnect and on
// server shutdown (the two ways a long-lived connection has to be able to die).

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// sseEvent is one parsed text/event-stream frame.
type sseEvent struct {
	Name string
	Data string
}

// readEvent reads frames until a complete one arrives (or the stream ends).
func readEvent(t *testing.T, br *bufio.Reader) (sseEvent, bool) {
	t.Helper()
	var ev sseEvent
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return ev, false
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ev.Name != "" {
				return ev, true
			}
		case strings.HasPrefix(line, "event: "):
			ev.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.Data += strings.TrimPrefix(line, "data: ")
		}
	}
}

// stateChangeFor pulls the per-type states out of a StateChange payload.
func stateChangeFor(t *testing.T, data, accountID string) map[string]string {
	t.Helper()
	var sc struct {
		Type    string                       `json:"@type"`
		Changed map[string]map[string]string `json:"changed"`
	}
	if err := json.Unmarshal([]byte(data), &sc); err != nil {
		t.Fatalf("StateChange: %v (%s)", err, data)
	}
	if sc.Type != "StateChange" {
		t.Errorf("@type = %q, want StateChange", sc.Type)
	}
	return sc.Changed[accountID]
}

// TestEventSourcePushesStateChanges: the client connects, gets the current state,
// and is TOLD when the Email state moves — no polling.
func TestEventSourcePushesStateChanges(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)

	srv := httptest.NewServer(auth.DevAuth(h.api.Handler(), a.UserID))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/jmap/eventsource/?types=Email,Mailbox&ping=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("eventsource: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("eventsource: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	br := bufio.NewReader(resp.Body)
	first, ok := readEvent(t, br)
	if !ok || first.Name != "state" {
		t.Fatalf("first event = %+v, want a state event", first)
	}
	before := stateChangeFor(t, first.Data, a.UserID.String())
	if _, ok := before["Email"]; !ok {
		t.Fatalf("the opening StateChange has no Email state: %v", before)
	}
	if _, ok := before["Label"]; ok {
		t.Errorf("Label was pushed although the client subscribed to Email,Mailbox only: %v", before)
	}

	// Move the Email state the ONLY legitimate way: a mutation that appends to
	// the change log in its own transaction (the load-bearing invariant).
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		err := h.store.UserTx(context.Background(), a.UserID, func(tx *store.UserTx) error {
			_, _, err := tx.ApplyEmailUpdate(context.Background(), a.MessageID,
				store.EmailUpdate{Seen: boolPtr(true)})
			return err
		})
		if err != nil {
			t.Errorf("mutate: %v", err)
		}
	}()
	<-done

	next, ok := readEvent(t, br)
	if !ok || next.Name != "state" {
		t.Fatalf("no StateChange arrived after the mutation (%+v)", next)
	}
	after := stateChangeFor(t, next.Data, a.UserID.String())
	if after["Email"] == "" || after["Email"] == before["Email"] {
		t.Errorf("Email state did not move: before=%v after=%v", before, after)
	}
}

// TestEventSourceCloseAfterState: `closeafter=state` is a one-shot — the server
// sends the current state and closes, which is exactly what a client that just
// wants to check for movement (or a curl in an operator's terminal) wants.
func TestEventSourceCloseAfterState(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)

	srv := httptest.NewServer(auth.DevAuth(h.api.Handler(), a.UserID))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/jmap/eventsource/?types=*&closeafter=state", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("eventsource: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	br := bufio.NewReader(resp.Body)
	ev, ok := readEvent(t, br)
	if !ok || ev.Name != "state" {
		t.Fatalf("event = %+v", ev)
	}
	states := stateChangeFor(t, ev.Data, a.UserID.String())
	for _, typ := range eventSourceTypes {
		if _, ok := states[string(typ)]; !ok {
			t.Errorf(`types=* did not include %q: %v`, typ, states)
		}
	}
	// Area, DeskPreferences and ClientPreferences (#375, #469) explicitly, not
	// just via the generic loop above: each has its own /changes method and
	// change_log rows, and the desk client is about to subscribe to all
	// three — a regression that dropped one from eventSourceTypes must fail
	// HERE by name, not just as an unlabelled iteration miss.
	if _, ok := states["Area"]; !ok {
		t.Errorf(`types=* did not include "Area": %v`, states)
	}
	if _, ok := states["DeskPreferences"]; !ok {
		t.Errorf(`types=* did not include "DeskPreferences": %v`, states)
	}
	if _, ok := states["ClientPreferences"]; !ok {
		t.Errorf(`types=* did not include "ClientPreferences": %v`, states)
	}
	// The stream must END, not hang.
	if _, more := readEvent(t, br); more {
		t.Error("closeafter=state did not close the stream")
	}
}

// TestEventSourceStopsOnShutdown: a push stream must not outlive the server. The
// base context (cancelled by main on SIGINT/SIGTERM) is what lets an open
// connection unwind instead of holding the graceful-shutdown deadline open.
func TestEventSourceStopsOnShutdown(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)

	shutdown, stop := context.WithCancel(context.Background())
	defer stop()
	h.api = New(h.store, WithBaseContext(shutdown))

	srv := httptest.NewServer(auth.DevAuth(h.api.Handler(), a.UserID))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/jmap/eventsource/", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("eventsource: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	br := bufio.NewReader(resp.Body)
	if ev, ok := readEvent(t, br); !ok || ev.Name != "state" {
		t.Fatalf("event = %+v", ev)
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_, _ = readEvent(t, br) // returns when the body ends
	}()
	stop() // the server is shutting down

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the event stream did not close when the server shut down")
	}
}

// TestEventSourceRejectsUnknownType: a client that subscribed to a type this
// server does not push must be told, not left waiting for an event that will
// never come.
func TestEventSourceRejectsUnknownType(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)

	srv := httptest.NewServer(auth.DevAuth(h.api.Handler(), a.UserID))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/jmap/eventsource/?types=Email,VacationResponse", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
