// SPDX-License-Identifier: AGPL-3.0-only

package census

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// fakeCloser records whether Close was called, so a test can assert every
// connection ProbeConnections opened was also closed — not merely that the
// count came out right.
type fakeCloser struct {
	closed bool
}

func (f *fakeCloser) Close() error {
	f.closed = true
	return nil
}

// failingDialAt returns a dial func that succeeds on every attempt except
// the k-th (1-indexed), which fails. The returned slice is appended to as
// each successful dial happens, so the test can inspect every closer it
// produced afterward.
func failingDialAt(k int) (func(ctx context.Context) (io.Closer, error), *[]*fakeCloser) {
	made := &[]*fakeCloser{}
	attempt := 0
	dial := func(context.Context) (io.Closer, error) {
		attempt++
		if attempt == k {
			return nil, errors.New("refused: too many connections")
		}
		c := &fakeCloser{}
		*made = append(*made, c)
		return c, nil
	}
	return dial, made
}

func TestProbeConnectionsRefusal(t *testing.T) {
	dial, made := failingDialAt(3) // succeeds twice, fails on the 3rd
	res := ProbeConnections(context.Background(), dial, 5, 0)

	if !res.Attempted {
		t.Error("Attempted = false, want true")
	}
	if res.Max != 5 {
		t.Errorf("Max = %d, want 5", res.Max)
	}
	if res.Held != 2 {
		t.Errorf("Held = %d, want 2 (attempts 1 and 2 succeeded)", res.Held)
	}
	if !res.Refused {
		t.Error("Refused = false, want true")
	}
	if res.RefusalError == "" {
		t.Error("RefusalError is empty, want the dial error's text")
	}

	if len(*made) != 2 {
		t.Fatalf("made %d closers, want 2", len(*made))
	}
	for i, c := range *made {
		if !c.closed {
			t.Errorf("closer %d was never closed", i)
		}
	}
}

func TestProbeConnectionsNoRefusal(t *testing.T) {
	dial, made := failingDialAt(0) // never fails (attempt never equals 0)
	res := ProbeConnections(context.Background(), dial, 3, 0)

	if res.Held != 3 {
		t.Errorf("Held = %d, want 3", res.Held)
	}
	if res.Refused {
		t.Error("Refused = true, want false")
	}
	if res.RefusalError != "" {
		t.Errorf("RefusalError = %q, want empty", res.RefusalError)
	}

	if len(*made) != 3 {
		t.Fatalf("made %d closers, want 3", len(*made))
	}
	for i, c := range *made {
		if !c.closed {
			t.Errorf("closer %d was never closed", i)
		}
	}
}

func TestProbeConnectionsZeroMax(t *testing.T) {
	called := false
	dial := func(context.Context) (io.Closer, error) {
		called = true
		return &fakeCloser{}, nil
	}
	res := ProbeConnections(context.Background(), dial, 0, 0)
	if !res.Attempted {
		t.Error("Attempted = false, want true (ProbeConnections was called)")
	}
	if res.Held != 0 {
		t.Errorf("Held = %d, want 0", res.Held)
	}
	if called {
		t.Error("dial was called with max <= 0, want it never called")
	}
}

func TestProbeConnectionsCancelClosesEverythingHeld(t *testing.T) {
	dial, made := failingDialAt(0)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after the first successful dial by wrapping dial: the probe
	// must still close what it already opened.
	wrapped := func(ctx context.Context) (io.Closer, error) {
		c, err := dial(ctx)
		if err == nil && len(*made) == 1 {
			cancel()
		}
		return c, err
	}

	res := ProbeConnections(ctx, wrapped, 5, time.Millisecond)
	if res.Held != len(*made) {
		t.Errorf("Held = %d, want %d (len(made))", res.Held, len(*made))
	}
	for i, c := range *made {
		if !c.closed {
			t.Errorf("closer %d was never closed after ctx cancellation", i)
		}
	}
}
