# ADR-033: iCalendar invitations are read by a small in-house parser, not a library

**Status:** Accepted (Forgejo #725, v1.90.0).

## Context

#725 shows a calendar invitation (iTIP, RFC 5546, carried in mail by iMIP, RFC 6047)
as a card above the message body and lets the user answer it with Accept / Maybe /
Decline. That needs two things from iCalendar (RFC 5545):

- **read** a `text/calendar` part or an `.ics` attachment well enough to show the
  summary, the start and end (with their time zone), the location, the organiser, the
  attendees and their PARTSTAT, the SEQUENCE and UID, the METHOD, and whether and how
  the event repeats;
- **write** one iTIP REPLY: a VCALENDAR with METHOD:REPLY and one VEVENT naming one
  attendee and their answer.

An `.ics` is attacker-controlled input exactly as a message body is
(docs/hostile-content.md §1). Whatever reads it must bound its own work — bytes,
components, properties, nesting — and must never fetch anything named in it (URL,
ATTACH, TZURL, ALTREP). Recurrence rules must not be expanded: `RRULE:FREQ=SECONDLY;
COUNT=2000000000` is a valid line.

Spec §3 requires third-party protocol libraries to sit behind our own interface. #74
(3.x) will later want CalDAV, which is where a full iCalendar library genuinely earns
its keep.

### ⚠ This moves part of a 3.x roadmap item into 1.x — a stated deviation

Spec §17 lists "CalDAV/calendar hooks (iTIP accept/decline)" under **3.x**, and §1
calls calendar "a v3 hook". #725 (a feature request) splits the iTIP half
out of #74 and ships it now. This ADR records that deviation, and its limit: what
moves forward is the *hook* §1 describes — an invitation answered from the message it
arrived in — not groupware. There is no calendar view, no event store and no sync;
those stay with #74 in 3.x, and §1's "full groupware is out" is unchanged.

## Options

Facts below were read from the downloaded modules on 2026-10-17 (go-ical at
`v0.0.0-20250609112844-439c63cef608`, golang-ical at `v0.3.7`), not recalled.

| | `github.com/emersion/go-ical` | `github.com/arran4/golang-ical` | In-house subset |
|---|---|---|---|
| Licence | MIT | Apache-2.0 | AGPL-3.0-only (ours) |
| AGPL-3.0 compatible | yes | yes (Apache-2.0 is GPLv3-compatible) | — |
| Size / deps | 1,426 non-test lines; requires `teambition/rrule-go` | 4,507 non-test lines; test-only deps | 1,662 lines including its (long) comments; no deps |
| Maintenance | same author as go-imap/go-message, which we already depend on; untagged, last commit June 2025 | tagged releases | ours |
| Size bound | none: decodes the whole stream | none | `MaxBytes`, enforced first |
| Component / property / nesting bound | none (bounded only by input size) | none | enforced while parsing |
| Parameter count bound | none | none | enforced per line |
| Recurrence | expands via rrule-go if asked | expands if asked | **cannot** expand: summary only |
| Time zones | `time.LoadLocation(TZID)` only (ical.go) — a Windows name such as `Pacific Standard Time` (every Outlook invitation) is unresolvable | VTIMEZONE support partial | IANA name, else the file's own VTIMEZONE rules, else floating with the zone named |
| REPLY writer | yes | yes | yes (~120 lines) |

## Decision

**A small in-house parser, `internal/ical`.** It reads the RFC 5545 subset an
invitation needs and nothing more, and its exported surface — `Parse`,
`ExtractInvite`, `BuildReply`, `FindInMessage` and the `Invite` type — is the
anti-corruption interface. Nothing outside the package sees a content line.

Why, weighed:

1. **Hostile-input robustness is the deciding axis, and the libraries leave it to the
   caller.** Neither library bounds components, properties, nesting or parameters. A
   256 KiB cap applied *before* calling one bounds memory, but not the shape the
   library then walks, and it is the shape (a parameter list of 30,000 values on one
   line, 10,000 nested BEGINs) that hostile files use. Here every bound is enforced at
   the point the parser would otherwise allocate, and each bound has a test that goes
   red when it is removed.
2. **Not expanding recurrence is a property of the code, not a promise of the
   caller.** With go-ical, "do not call the expander" is a rule someone has to keep;
   here there is no expander to call. The card says "Repeats weekly on Monday, 10
   times", which is what a person needs from a mail client.
3. **Time zones work for Outlook.** Outlook writes Windows zone names and ships the
   rules as a VTIMEZONE. Resolving from the file's own STANDARD/DAYLIGHT observances
   (yearly BYMONTH + BYDAY or BYMONTHDAY rules, which is the only shape real
   VTIMEZONEs use) needs no mapping table and no network. go-ical would have needed
   the same code written on top of it.
4. **"Never fetch a URL" is structural.** The package imports no network package, and
   `TestPackageNeverFetches` fails if one is added.
5. **The subset is small and stable.** Content lines, folding, parameter quoting, TEXT
   escaping, DATE/DATE-TIME/DURATION/UTC-OFFSET values. RFC 5545 has not changed since
   2009 and RFC 7986's additions are properties this card does not show.

**Licence:** ours (AGPL-3.0-only). No new module dependency, so no `govulncheck`
surface. The compiled-in zone database is the Go standard library's `time/tzdata`.

## Consequences

- **What this does not do**, and must not grow into: CalDAV, free/busy, VTODO or
  VJOURNAL invitations, COUNTER/DECLINECOUNTER, delegation, RDATE/EXDATE, or any
  occurrence expansion. When #74 (3.x) brings CalDAV it should **revisit this ADR**:
  two-way sync needs a full, round-tripping object model, and that is the point at
  which a library (go-ical is the likely candidate, being the go-webdav author's) is
  worth its unbounded parse — behind this same package's interface, with these limits
  applied in front of it.
- **Only an authenticated message speaks for the event.** A CANCEL or update
  changes another message's card only when `ical.Verified` holds (exactly one
  From field, equal to ORGANIZER, plus a trusted DMARC pass aligned to its
  domain; a Sender is never honoured, so a delegate's update reads unverified); an unverified one shows its own card, marked, and changes nothing
  else. With no trusted authserv-id configured nothing verifies (fail closed).
  Added after a security review of the first push; see docs/hostile-content.md
  §2.11.
- A REPLY carries only what RFC 5546 §3.2.3 requires plus SUMMARY: UID, SEQUENCE,
  DTSTAMP, ORGANIZER, one ATTENDEE, and RECURRENCE-ID for an instance. Every echoed
  value is re-encoded, so a CR/LF in a SUMMARY or UID cannot become a content line.
- A TZID that is neither an IANA name nor defined by the file leaves the time
  **floating**, reported with the zone's name, rather than silently shown in the
  reader's zone. The card says so.
- The limits are constants in `internal/ical/parse.go` and `invite.go`; a real
  invitation that hits one is reported as such (`calendarInvite.problem`), never as
  "no invitation".

## References

RFC 5545 (iCalendar), RFC 5546 (iTIP), RFC 6047 (iMIP); spec §3 (anti-corruption
layers), §11 (security); docs/hostile-content.md §2.11; Forgejo #725, #74.
