// SPDX-License-Identifier: AGPL-3.0-only

package writeback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Drain keeps applying passes on ONE writer while each pass claims a full
// batch (#706). Before it existed a large backlog drained 64 intents per sync
// pass, with a fresh dial and login each time; a bulk move took many minutes
// to reach the provider.

// seedBacklog queues n add-flag intents on one account, each with a live copy,
// and returns the account.
func seedBacklog(s *fakeStore, n int) ulid.ULID {
	acct := store.NewULID()
	for i := 0; i < n; i++ {
		msg := store.NewULID()
		in := store.Intent{
			ID: store.NewULID(), MessageID: &msg, AccountID: acct, Op: store.IntentAddFlag,
			Payload: []byte(`{"flag":"\\Seen"}`), IdempotencyKey: "drain-" + msg.String(),
			State: store.IntentPending,
		}
		s.add(in)
		s.prov[provKey(msg, acct)] = []store.ProvenanceTarget{liveTargetAt("INBOX", int64(100+i))}
	}
	return acct
}

func countState(s *fakeStore, st store.IntentState) int {
	n := 0
	for _, in := range s.intents {
		if in.State == st {
			n++
		}
	}
	return n
}

func TestDrainEmptiesABacklogInOneCall(t *testing.T) {
	s := newFakeStore()
	acct := seedBacklog(s, 25)
	var rounds []int
	e := NewEngine(s, WithBatchSize(10))
	res, err := e.Drain(context.Background(), acct, &fakeWriter{}, DrainLimits{MaxRounds: 16, Budget: time.Minute},
		func(round int, r Result, _ time.Duration) { rounds = append(rounds, r.Claimed) })
	if err != nil {
		t.Fatal(err)
	}
	// 10 + 10 + 5: the short third batch is what ends it.
	if want := []int{10, 10, 5}; !equalInts(rounds, want) {
		t.Errorf("claimed per round = %v, want %v", rounds, want)
	}
	if res.Applied != 25 || res.Claimed != 25 {
		t.Errorf("total = %+v, want 25 applied / 25 claimed", res)
	}
	if n := countState(s, store.IntentPending); n != 0 {
		t.Errorf("%d intents still pending after a drain with headroom, want 0", n)
	}
}

// An exact multiple of the batch ends with one empty round, which is the cheap
// way to learn there is nothing left; it must not loop.
func TestDrainStopsOnAnEmptyRound(t *testing.T) {
	s := newFakeStore()
	acct := seedBacklog(s, 20)
	var rounds []int
	_, err := NewEngine(s, WithBatchSize(10)).Drain(context.Background(), acct, &fakeWriter{},
		DrainLimits{MaxRounds: 16, Budget: time.Minute},
		func(_ int, r Result, _ time.Duration) { rounds = append(rounds, r.Claimed) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{10, 10, 0}; !equalInts(rounds, want) {
		t.Errorf("claimed per round = %v, want %v", rounds, want)
	}
}

// MaxRounds bounds the drain, so one account's backlog cannot hold its sync
// pass (which runs after the drain on the same sequence) indefinitely.
func TestDrainHonoursMaxRounds(t *testing.T) {
	s := newFakeStore()
	acct := seedBacklog(s, 100)
	res, err := NewEngine(s, WithBatchSize(10)).Drain(context.Background(), acct, &fakeWriter{},
		DrainLimits{MaxRounds: 3, Budget: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Claimed != 30 {
		t.Errorf("claimed = %d, want 30 (3 rounds of 10)", res.Claimed)
	}
	if n := countState(s, store.IntentPending); n != 70 {
		t.Errorf("pending after a capped drain = %d, want 70 left for the next one", n)
	}
}

// A zero budget means "one pass, then stop": the check runs after each pass.
func TestDrainHonoursTheBudget(t *testing.T) {
	s := newFakeStore()
	acct := seedBacklog(s, 100)
	res, err := NewEngine(s, WithBatchSize(10)).Drain(context.Background(), acct, &fakeWriter{},
		DrainLimits{MaxRounds: 16, Budget: 0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Claimed != 10 {
		t.Errorf("claimed = %d, want 10 (one round, budget already spent)", res.Claimed)
	}
}

// An infrastructure error ends the drain at that pass: what that pass did is
// already recorded per intent, and carrying on would repeat the failure.
func TestDrainStopsAtTheFirstError(t *testing.T) {
	s := newFakeStore()
	acct := seedBacklog(s, 30)
	s.markErr = errors.New("database went away")
	var rounds int
	_, err := NewEngine(s, WithBatchSize(10)).Drain(context.Background(), acct, &fakeWriter{},
		DrainLimits{MaxRounds: 16, Budget: time.Minute},
		func(int, Result, time.Duration) { rounds++ })
	if err == nil {
		t.Fatal("Drain returned no error with every mark failing")
	}
	if rounds != 1 {
		t.Errorf("rounds = %d after an error, want 1", rounds)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
