// SPDX-License-Identifier: AGPL-3.0-only

// Package rcptproj derives the stored, searchable form of a message's
// recipients and List-Id (ADR-055, migration 00095) from a parsed message.
//
// Like attachproj it exists for a boundary: internal/store does not import
// internal/mime, and mime must not import store. Ingest, compose and the
// `backfill-recipients` command all call Project, so the three cannot disagree
// about what a recipient column holds.
package rcptproj

import (
	"strings"
	"unicode/utf8"

	"mailroom/internal/mime"
	"mailroom/internal/store"
)

// Bounds on what is stored. A To list is sender-controlled and unbounded; the
// columns are not. A recipient past MaxRecipients is not stored (and Project
// reports the truncation), so a substring that is only in the dropped tail does
// not match: a documented bound, never a silent one (ADR-055).
const (
	MaxRecipients = 200
	maxNameRunes  = 128
	maxAddrRunes  = 254
	maxKeyRunes   = 255
)

// Project derives the five columns of m. A nil message yields filled, empty
// columns: the bytes could not be parsed into anything, and "filled, nothing
// there" is the truthful state (the backfill reports unreadable bytes separately
// and never reaches here for them).
func Project(m *mime.Message) store.RecipientCols {
	c := store.RecipientCols{}
	if m == nil {
		return c
	}
	var trunc bool
	to := m.Header.To()
	c.To, trunc = text(to)
	c.Truncated = c.Truncated || trunc
	c.Cc, trunc = text(m.Header.Cc())
	c.Truncated = c.Truncated || trunc
	c.Bcc, trunc = text(m.Header.Bcc())
	c.Truncated = c.Truncated || trunc
	c.ToSort = sortKey(to)
	c.ListIDKey = ListIDKey(m.Header.Get("List-Id"))
	return c
}

// text renders addresses as lowercased lines: the display name (when there is
// one) and the address (when there is one), each on its own line.
func text(as []mime.Address) (string, bool) {
	var lines []string
	truncated := false
	n := 0
	for _, a := range as {
		name := clip(fold(a.Name), maxNameRunes)
		addr := clip(strings.ToLower(strings.TrimSpace(a.Address)), maxAddrRunes)
		if name == "" && addr == "" {
			continue
		}
		if n == MaxRecipients {
			truncated = true
			break
		}
		n++
		if name != "" {
			lines = append(lines, name)
		}
		if addr != "" {
			lines = append(lines, addr)
		}
	}
	return strings.Join(lines, "\n"), truncated
}

// sortKey is the first recipient's display name, else its address, with the
// case kept (the query lowercases, as the From sort does).
func sortKey(as []mime.Address) string {
	for _, a := range as {
		if name := clip(strings.Join(strings.Fields(a.Name), " "), maxNameRunes); name != "" {
			return name
		}
		if addr := clip(strings.TrimSpace(a.Address), maxAddrRunes); addr != "" {
			return addr
		}
	}
	return ""
}

// ListIDKey is the id inside the angle brackets of a List-Id value
// ("Plans <plans.example.net>" gives "plans.example.net"), lowercased; a value
// with no brackets is used whole. Empty for an absent or blank header.
func ListIDKey(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if open := strings.LastIndexByte(v, '<'); open >= 0 {
		if end := strings.IndexByte(v[open:], '>'); end > 1 {
			v = v[open+1 : open+end]
		}
	}
	return clip(strings.ToLower(strings.Join(strings.Fields(v), "")), maxKeyRunes)
}

// fold lowercases and collapses whitespace runs (newlines included) to a space.
func fold(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// clip bounds s to runes characters and makes it storable: PostgreSQL text
// rejects NUL and invalid UTF-8, and the input is sender-controlled.
func clip(s string, runes int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "\uFFFD")
	if utf8.RuneCountInString(s) <= runes {
		return s
	}
	r := []rune(s)
	return string(r[:runes])
}
