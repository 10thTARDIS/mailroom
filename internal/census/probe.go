// SPDX-License-Identifier: AGPL-3.0-only

package census

import (
	"context"
	"io"
	"time"
)

// ProbeResult is what ProbeConnections found. It always states its own
// bound (CLAUDE.md: a bounded or empty result must report its bound): Held
// is a LOWER bound on the server's concurrent-connection cap unless Refused
// is also true, in which case Held is the exact number the server allowed
// before refusing the next one.
type ProbeResult struct {
	// Attempted is false only when the probe was never run at all (the
	// caller's -probe-connections was 0/off) — WriteText renders that as
	// "not run", distinct from a probe that ran and held 0.
	Attempted bool `json:"attempted"`
	// Max is the ceiling the probe was asked to try for.
	Max int `json:"max"`
	// Held is how many connections were successfully dialed AND
	// authenticated, and were still open when the probe finished (before it
	// closed them all).
	Held int `json:"held"`
	// Refused is true when a dial or LOGIN failed before Max was reached —
	// the ordinary way a real server signals its connection cap.
	Refused bool `json:"refused"`
	// RefusalError is the error text from the failing dial/login, set only
	// when Refused is true.
	RefusalError string `json:"refusalError,omitempty"`
}

// ProbeConnections dials+authenticates sequentially via dial, HOLDING every
// successful session open (never closing one to make room for the next),
// until either max are held or a dial/login fails. It then closes every
// session it opened — ALWAYS, including when ctx is canceled mid-probe —
// because a probe that leaked held connections would itself be consuming
// the resource it exists to measure.
//
// spacing is a small pause between successive dial attempts, so the probe
// does not present as a connection-flood to whatever it's measuring; it is
// skipped after the last attempt (nothing follows it) and skipped entirely
// while ctx is already done.
//
// A nil dial (max <= 0) is never called; ProbeConnections returns
// Attempted: true, Held: 0 immediately. Held == 0 with Refused == false is
// therefore ambiguous between "max was 0" and "the very first dial failed
// before any distinction was possible" — RefusalError disambiguates the
// latter.
func ProbeConnections(ctx context.Context, dial func(ctx context.Context) (io.Closer, error), max int, spacing time.Duration) ProbeResult {
	result := ProbeResult{Attempted: true, Max: max}
	if max <= 0 {
		return result
	}

	var held []io.Closer
	// ALWAYS close every session this probe opened, on every return path —
	// including a canceled ctx, which is exactly when leaving connections
	// open would be easiest to miss.
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()

	for i := 0; i < max; i++ {
		if err := ctx.Err(); err != nil {
			result.Held = len(held)
			return result
		}

		c, err := dial(ctx)
		if err != nil {
			result.Held = len(held)
			result.Refused = true
			result.RefusalError = err.Error()
			return result
		}
		held = append(held, c)

		if spacing > 0 && i < max-1 {
			select {
			case <-time.After(spacing):
			case <-ctx.Done():
				result.Held = len(held)
				return result
			}
		}
	}

	result.Held = len(held)
	return result
}
