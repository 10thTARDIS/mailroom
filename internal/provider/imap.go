// SPDX-License-Identifier: AGPL-3.0-only

package provider

import "time"

// IMAP sync-cadence floors (spec §16 provider profiles, mailroom#103 IMAP
// connection hardening — Stage 1).
//
// Some providers throttle or penalize aggressive IMAP polling. A per-provider
// MINIMUM sync interval lets the sync loop honor a provider's tolerance
// independently of the operator's globally-configured base interval: the base
// interval governs whenever it is already slower than the floor, and the floor
// only ever SLOWS an account down toward a safer cadence — it never speeds one
// up. This keeps a single conservative default from having to be pessimal for
// every provider at once.
//
// The table is deliberately small, documented, and extensible: add a row when a
// provider's polling tolerance is actually known, and leave everything else on
// the "generic" default (no floor). It is a pure lookup with no I/O, so the sync
// loop can consult it on every scheduling decision.
var imapMinSyncInterval = map[string]time.Duration{
	// generic: no floor — the operator's configured base interval governs.
	"generic": 0,
	// migadu: no known polling penalty; base interval governs.
	"migadu": 0,
	// gmail: Gmail's IMAP throttles clients that reconnect/poll too aggressively,
	// so never poll a Gmail account faster than every 5 minutes regardless of a
	// tighter global base interval. Conservative on purpose.
	"gmail": 5 * time.Minute,
}

// IMAPMinSyncInterval returns the minimum re-sync interval to honor for a
// provider profile, or 0 when the profile is unknown or has no floor (in which
// case the caller's base interval governs). It never returns a negative value.
func IMAPMinSyncInterval(profile string) time.Duration {
	if d := imapMinSyncInterval[profile]; d > 0 {
		return d
	}
	return 0
}
