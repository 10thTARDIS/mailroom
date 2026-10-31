// SPDX-License-Identifier: AGPL-3.0-only

// Package ical is Mailroom's anti-corruption layer for iCalendar (RFC 5545) as it
// arrives in mail: iTIP invitations (RFC 5546) carried by iMIP (RFC 6047). See
// ADR-033 for why this is a small in-house parser rather than a library.
//
// # What it does
//
//   - Parse reads a bounded subset of RFC 5545 — content lines, parameters,
//     components — into a generic tree, and refuses anything past its limits.
//   - ExtractInvite turns that tree into an Invite: the fields a mail client shows
//     on a card (summary, start and end, location, organiser, attendees and their
//     PARTSTAT, sequence, UID, method, a recurrence SUMMARY) and nothing else.
//   - BuildReply writes the iTIP REPLY that carries one attendee's answer.
//   - FindInPart locates the calendar inside a parsed MIME message.
//
// # The hostile-input contract
//
// A .ics file is attacker-controlled input, exactly like a message body
// (docs/hostile-content.md). So:
//
//   - every limit is enforced WHILE parsing, not afterwards: bytes (MaxBytes),
//     components (MaxComponents), properties (MaxProperties), nesting (MaxDepth),
//     parameters per property (MaxParams), attendees kept (MaxAttendees, and the
//     number dropped is REPORTED, never silently lost);
//   - a recurrence rule is SUMMARISED, never expanded — "repeats weekly, 10 times"
//     costs the same whether COUNT is 10 or ten million;
//   - nothing here fetches anything. URL, ATTACH, a TZURL, an ALTREP parameter —
//     none of them is read, followed or returned. A VTIMEZONE is resolved from
//     the zone database or from the file's own definition, never from a network;
//   - every string that leaves this package has had control characters removed
//     and is length-capped, and BuildReply re-escapes everything it echoes, so a
//     CR/LF smuggled into a SUMMARY or a UID cannot become a new content line in
//     the reply we send.
//
// It never panics on any input: the fuzz-style tests feed it truncations and
// mutations of every sample.
package ical
