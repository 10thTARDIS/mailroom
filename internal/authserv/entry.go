// SPDX-License-Identifier: AGPL-3.0-only

// Package authserv parses the entries of MAILROOM_AUTHSERV_IDS (#766).
//
// An entry is `id` (trusted for all mail) or `id<YYYY-MM-DD` (trusted only
// for mail received strictly before 00:00 UTC on that date). The package is
// separate from internal/ml so that internal/config can validate the setting
// at startup without importing the mail-parsing code, and so that ONE parser
// decides what an entry means for both the validator and the resolver
// (ml.TrustedAt): they cannot disagree about which entries are well formed.
package authserv

import (
	"errors"
	"strings"
	"time"
)

// BoundSep separates an authserv-id from its received-time bound.
//
// ⚠ '<' CANNOT APPEAR IN AN authserv-id THAT MATCHES A HEADER USEFULLY, and the
// fail-closed design leans on it: a bounded entry that reaches a trust check
// without being resolved by ml.TrustedAt never equals a plain id.
const BoundSep = "<"

// Entry is one parsed MAILROOM_AUTHSERV_IDS entry.
type Entry struct {
	// ID is the lowercased, trimmed authserv-id (never empty, never contains '<').
	ID string
	// Bound is the instant (00:00 UTC of the entry's date) before which the id
	// is trusted. Meaningful only when Bounded.
	Bound   time.Time
	Bounded bool
}

// Parse reads one entry. Lowercasing and trimming match the unbounded form's
// historical behaviour. A blank entry is not an error (the caller skips it):
// it returns ok=false, err=nil.
func Parse(raw string) (e Entry, ok bool, err error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return Entry{}, false, nil
	}
	if strings.Count(s, BoundSep) > 1 {
		return Entry{}, false, errors.New("more than one '<'")
	}
	id, date, bounded := strings.Cut(s, BoundSep)
	id = strings.TrimSpace(id)
	if id == "" {
		return Entry{}, false, errors.New("empty authserv-id")
	}
	if !bounded {
		return Entry{ID: id}, true, nil
	}
	d, err := time.Parse("2006-01-02", strings.TrimSpace(date))
	if err != nil {
		return Entry{}, false, errors.New("bound is not a YYYY-MM-DD date")
	}
	return Entry{ID: id, Bound: d.UTC(), Bounded: true}, true, nil
}
