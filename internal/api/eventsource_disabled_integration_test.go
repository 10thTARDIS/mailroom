// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
)

// An event stream opened with an API key or an OAuth token carries no session, so
// the session re-check does not cover it. Disabling the user must end it too, not
// only refuse the next request: that is the userEnabled re-check.
func TestEventSourceWithoutASessionEndsWhenTheUserIsDisabled(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	h.api.streamRecheck = 100 * time.Millisecond
	h.api.sessionLive = func(context.Context, ulid.ULID, ulid.ULID) (bool, error) {
		t.Error("the session re-check ran for a stream that has no session")
		return true, nil
	}
	srv := httptest.NewServer(auth.DevAuth(h.api.Handler(), a.UserID))
	t.Cleanup(srv.Close)

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+"/jmap/eventsource/?types=Email&ping=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("eventsource: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	br := bufio.NewReader(resp.Body)
	if ev, ok := readEvent(t, br); !ok || ev.Name != "state" {
		t.Fatalf("first event = %+v", ev)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for {
			if _, err := br.ReadString('\n'); err != nil {
				return
			}
		}
	}()

	// Positive control: the stream stays open across several re-checks while the
	// user is enabled.
	select {
	case <-ended:
		t.Fatal("the stream ended although the user is enabled")
	case <-time.After(600 * time.Millisecond):
	}

	if _, err := h.api.store.SetUserDisabled(ctx, a.UserID.String(), true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = h.api.store.SetUserDisabled(context.Background(), a.UserID.String(), false) })
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end within a few re-check intervals of the user being disabled")
	}
}
