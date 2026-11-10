# ADR-056: Old attachments are re-scanned when opened; risky file types are neutralised and warned about

**Status:** Accepted (v1.103.0 beta 3; operator decisions Q80 and Q81, 2026-11-10).
Amends ADR-027 §5c (the `> 6 months` band) and records a deviation from spec §11
("dangerous types blocked").

## Context

Two loose ends from the attachment work (ADR-027) are closed here together,
because both are about what happens at the moment a person touches an attachment.

**1. Old mail is never looked at again.** ADR-027 §5c stops re-scanning once a
message is older than six months, and says so in as many words: the stop is "safe
ONLY because the verdict's age is surfaced" (§1a). The detection gap for new
malware is measured in days, so an attachment that was `clean` when it arrived in
2024 was judged by signatures that did not know what is known today. Nothing
revisits it, ever. That is acceptable for mail nobody reads; it is a poor answer
for the one old attachment a person is about to open.

**2. The spec says "blocked", and the code does something else.** Spec §11 listed
"dangerous types blocked". What the code does is neutralise: `safeContentType`
(internal/api/blob.go) serves any type not on a small allowlist as
`application/octet-stream` with `Content-Disposition: attachment` and `nosniff`,
and the preview route refuses it with 415. The file is still handed over on
request, because refusing a user their own mail is not a control (the spec's
current wording already says "neutralised and never rendered"). What was missing
is any signal to the person: a program file and a photograph are downloaded with
the same single click. The project rule is that the spec wins unless an ADR says
otherwise, so the deviation is recorded here instead of being left implicit.

## Decision A: re-scan when opened

### What "opened" means

Three events, and only these:

| Event | Where | What happens |
|---|---|---|
| the reading pane fetches a body | `Email/get` that fetches body VALUES (`fetchTextBodyValues`, `fetchHTMLBodyValues` or `fetchAllBodyValues`), for the messages returned | a **queued** re-scan job, if any attachment is stale and old |
| an attachment is **downloaded** | `GET /jmap/download/.../{blobId}` for a part | a **synchronous** re-scan before serving |
| an attachment is **previewed** | `GET /jmap/preview/.../{blobId}` | a **synchronous** re-scan before serving |

A list or search request that does not fetch body values triggers nothing, and
neither does an `Email/get` that asks only for the `attachments` property: the
trigger is the body-value fetch, which is what the reading pane does. Mail nobody
opens costs nothing.

### When it applies

Only when **both** hold:

1. the message is in the `> 6 months` band (the band above the last re-scanned
   tier in `store.RescanTiers`; the boundary is derived from that table, not
   restated), and
2. the attachment's verdict is behind the scanner's current signature database
   (`scanned_db_version` is NULL or lower than the current version), and the part
   has bytes (`size_bytes > 0`).

A fresh verdict, or a message in any newer band, triggers nothing: the existing
tiers own those, and a test pins that no clamd call is made for them. The check is
two cheap Postgres reads first (is anything old, then is it stale), so the common
case costs no scanner round trip.

The current version comes from the scanner (`VERSION`), cached for 30 s and
bounded to 2 s, so a hung scanner cannot slow a body fetch by more than that.

### Download and preview: synchronous, with a short budget

The message is re-scanned **before** the quarantine gate runs, so the gate reads
the fresh verdict:

- budget **10 s** for the scan (`rescan.SyncBudget`), enforced by a context
  deadline that clamd's client honours;
- at most **2** such scans in flight across the process; a third request does not
  wait, it falls to the queued path;
- on success the verdict, its stamp and (if the state moved) the Email change row
  are written by the same `SetScanVerdicts` ingest and the re-scan job use, so
  the invariant "a mutation appends a change row in its transaction" holds
  unchanged. The gate then serves or refuses by the fresh verdict. A part that was
  `clean` and is now `flagged` is refused, and a flag propagates to other copies
  of the same bytes exactly as it does today.

### Timeout or scanner unavailable: fail OPEN, and enqueue

If the scan times out, the scanner is unreachable, or the answer is inconclusive,
the request is served exactly as it is served today, under the existing verdict
(blocked states still block), and a re-scan job is enqueued so the next open
finds a fresh verdict.

**Why fail-open is the right choice here, and fail-closed is not.**

- The choice is between "serve under a verdict that may be stale" and "refuse a
  file that has a verdict". The first is the **current behaviour for every
  attachment in this band**, and §5c's justification for it is that the verdict's
  age is surfaced (§1a). Fail-open leaves that argument intact and adds a chance to
  do better; it never does worse than today.
- Fail-closed turns a scanner outage, a reload of the signature database (which
  stalls clamd for seconds) or a 100 MB attachment into "you cannot open your own
  old mail". ADR-027 §8 names making real mail unreachable as the worse failure.
  Re-scan has no stake in being a gate: its package comment already says refusing
  because a best-effort refresh failed "would be theatre".
- A protection that fails closed on any hiccup trains people to reach for the
  release button, and that damages the real detections.
- What is lost is bounded and visible: the timeout counter below, and the verdict
  age already shown. An operator whose scanner is down sees a rising count rather
  than silence.

**A transient failure never downgrades a verdict, and it is judged per part.**
clamd reports some per-file failures as `unknown` rather than as an error (the scan
failed on a live daemon, the stream was refused, the reply was not understood).
`clamd.Verdict.Transient` marks exactly those. Writing one over a stored verdict
that did not block (`clean`, unscanned, empty, or released) would block a file
that was being served because the daemon had a bad second, so such a part keeps
its stored verdict (and its age), counts as `inconclusive`, and a job is
enqueued. A part that already blocks (`flagged`, `unknown`) takes the new answer. The rule is applied **to each part, never to the message**: every other
verdict in the same message is written, and a `flagged` verdict always wins and
quarantines immediately. A policy `unknown` (encrypted, over the size limit, a
heuristic "could not examine", a part with no bytes) is a real answer for those
bytes and is written as designed. The same function serves the synchronous and the
queued path.

### One slow part must not shield the message (amendment, security review)

The re-scan of an open path (the synchronous scan, `OnOpen`'s queued job and the
`attachment_rescan_open` handler; the tiered re-scan of ADR-027 is unchanged) works
part by part:

- it scans only the **stale** parts (`store.OpenRescanStaleParts`), the requested
  part first on a download or preview, then the others smallest first;
- each part gets its own **slice** of what is left of the overall budget: the
  remaining time divided by the parts still to scan, and the requested part takes
  half when it is not alone. A decoy that uses its whole slice leaves the others a
  share, where before the first `Scan` error abandoned the message, wrote nothing
  and put the real payload one slow file away from never being refreshed;
- a part that runs out of its slice keeps its stored verdict, is counted as a
  `timeout`, and the next part is scanned. The verdicts that finished are written
  by the same `SetScanVerdicts` call, and a job is queued for what was left;
- a scanner error that is not a deadline stops the loop (the next part would meet
  the same dead scanner) and counts as `unavailable`.

### The breaker counts scanner failures, not slow files

The synchronous path is skipped for 60 s (`BreakerPause`) after **3** failures
within 60 s (`BreakerThreshold`, `BreakerWindow`). A failure is the scanner being
unavailable: its `VERSION` call failing, or a `Scan` returning an error that is
not a deadline, at most once per refresh however many parts it had. A part that
merely ran out of its own slice is **not** a failure. One crafted attachment must
not be able to switch the synchronous path off for every user; the cost of that
choice is that a scanner that answers `VERSION` but hangs on every `Scan` costs
each request up to its budget (bounded by the two synchronous slots) rather than
tripping the breaker.

A failed `VERSION` call is remembered for 30 s (`versionFailTTL`): later calls
fail fast without dialling, so a hung scanner costs one open the 2 s version
timeout, not every open. It counts once toward the breaker, when it happened.

### Opening a message: one bounded, deduplicated job

A body fetch enqueues a re-scan job (`attachment_rescan_open`) per message that
has stale old attachments. It is a small job: one message, its payload is the
message id and nothing else (job payload rule, `store.jobPayloadKeys`), and the
handler re-reads the message under that user's identity.

The dispatcher is sequential, so a re-scan that runs long delays `submit` and
outbound mail stops. The job is therefore bounded three ways:

| Bound | Value | Why |
|---|---|---|
| per message | one job per message per 10 minutes, counting finished and failed ones | a message whose re-scan cannot complete is not retried by every open; repeated opens of the same message coalesce |
| per user per minute | at most **2** such jobs created in any 60 s |
| per process per minute | at most **6** such jobs created, across ALL users, in any 60 s (`GlobalEnqueueCap`) | a burst of opens (scrolling a list of old mail with previews on) cannot flood the queue |
| per job | **20 s** budget, one message | the handler gives up and returns normally (no dispatcher retry) |

Worst case, with a hung scanner, the open jobs occupy 40 s of a minute, and a
`submit` that falls due waits for the job running plus at most one queued ahead of
it; with a healthy scanner a job is milliseconds. The dispatcher orders by
`fire_at`, so anything due earlier than an open job runs first. This is a bound,
not a guarantee of zero delay, and it is stated as one. A body fetch inspects at
most 10 messages and a request that finds the cap or the per-message cooldown
reached simply does not enqueue.

The per-process cap is a sliding window held in memory by `OpenRescan`. It is **per
process**, not per deployment: each running instance keeps its own window, so N
instances admit up to 6N a minute. A slot is taken before the INSERT and given
back when the INSERT created nothing (deduped, throttled, error), so the cap counts
jobs created, not attempts. When it refuses, the enqueue is skipped and counted as
`global_cap`.

- **Fairness.** The per-user bound of 2 a minute, enforced in the INSERT, means one
  account can hold at most 2 of the 6 global slots. Starving everyone needs three
  or more accounts acting together, and accounts are created by the operator.
- **A refusal is not lost work.** It persists nothing and starts no cooldown, so the
  next open of that message tries again (a test pins it).
- **The synchronous path is unaffected.** Download and preview still re-scan
  synchronously under their own in-flight and breaker bounds, so the cap only delays
  the background refresh. It never decides what is served.
- **The trade-off.** The cap exists so that queued re-scans can never delay outgoing
  mail without bound across users.

The check-then-insert for the per-user cap is one SQL statement, but concurrent requests can
overshoot it by the number of requests racing in the same instant; the per-message
live-key unique index keeps that to one job per message.

A job re-reads the message, finds nothing stale (already refreshed by a download,
or the message gone, or not the user's) and returns. A message that is not the
user's own is invisible under row-level security: no job, probe or scan can reach
it.

### Counters

`mailroom_attachment_open_rescan_total{event}`, a closed set created at zero at
boot:

| event | meaning |
|---|---|
| `sync` | a download or preview was re-scanned synchronously and the verdict written |
| `queued` | a job was enqueued |
| `job` | a queued job completed a refresh |
| `timeout` | the synchronous budget (or a job's) ran out |
| `unavailable` | the scanner could not be reached or errored |
| `inconclusive` | the scan could not support a downgrade of a stored `clean` |
| `busy` | the synchronous slots were full or the breaker was open; queued instead |
| `throttled` | the per-user per-minute cap refused an enqueue |
| `global_cap` | the per-process cap across all users refused an enqueue |
| `deduped` | a job for this message already exists or was created within the cooldown |

"Synchronous versus queued" is `sync` versus `job`; "timeouts" is `timeout`.

## Decision B: risky types are neutralised, not blocked, and the person is warned

### The deviation

Spec §11's "dangerous types blocked" is **superseded by "neutralised, with a
warning"**. Neutralising is unchanged (octet-stream, attachment disposition,
`nosniff`, preview 415). The addition is a flag and a warning. A control that
withholds a file the user may need is not a control (the spec's own argument for
neutralising); a warning respects both the person and the risk. `flagged` and
`unknown` verdicts are still withheld (ADR-027 §8); this ADR does not touch that.

### The risky set

By filename extension (after lower-casing and trimming trailing dots, spaces,
NULs and format characters, since Windows ignores trailing dots and spaces and a
bidi control must not hide an extension):

| Reason code | Extensions |
|---|---|
| `program` | .exe .com .scr .dll .cpl .jar .app .apk .pif .msc .chm .gadget |
| `script` | .bat .cmd .ps1 .psm1 .ps1xml .vbs .vbe .vb .js .jse .wsf .wsh .ws .sct .hta .sh .reg .inf .command .scpt |
| `installer` | .msi .msp .dmg .pkg .deb .rpm .appx .msix .appxbundle .msixbundle .application .appref-ms |
| `macro_document` | .docm .dotm .xlsm .xltm .xlam .xlsb .pptm .potm .ppam .ppsm .sldm |
| `shortcut` | .lnk .url .library-ms .search-ms .settingcontent-ms |
| `disk_image` | .iso .img .vhd .vhdx |

By the **sniffed type** of the decoded bytes (leading bytes only):

| Signature | Reason |
|---|---|
| `MZ` with a valid `PE\0\0` header | `program` |
| `\x7fELF`; Mach-O (32/64, both byte orders, universal) | `program` |
| Windows shortcut header | `shortcut` |
| ISO 9660 `CD001` at offset 0x8001 | `disk_image` |
| `#!` with a name that has no extension | `script` |

A small set of declared media types (`application/x-msdownload`,
`application/x-dosexec`, `application/vnd.microsoft.portable-executable`,
`application/x-msdos-program`, `application/x-sh`, `application/java-archive`,
`application/x-msi`, `application/x-apple-diskimage`, and the
`macroenabled` Office types, including binary workbooks, templates, add-ins and
PowerPoint slideshows) count too. Declared types are attacker-chosen, so
they can only add a warning, never remove one.

### The classified name is the saved name

The download route saves a file as `sanitizeFilename` of the URL `{name}`, else of
the part's own filename. Two evasions made the classifier judge a different name
from the one on disk, and both are closed:

- a NUL (`invoice.pdf\x00.exe`, or its RFC 2231 `%00` and RFC 2047 `=00` forms) is
  dropped by the sanitiser, so the file is saved as `invoice.pdf.exe`. The
  classifier now skips control characters instead of stopping at the first;
- the sanitiser cut names at 100 runes whatever they ended in, so
  96 x `a` + `.exe.pdf` was judged `.pdf` and saved as `...a.exe`. It now cuts the
  stem and keeps the final extension when that extension, dot included, is at most
  24 runes (longer than the longest known one, `.settingcontent-ms`; a test walks
  the list). The total stays at most 100 runes.

A part is risky if the sanitised name **or** the raw name is; the reason is the
sanitised one's when that is risky. Extensions are matched case-folded through
upper then lower case, so a long s (`.ſcr`) is `.scr`.

### Both signals, and the disguised cases

Extension and bytes are checked independently and **either one** makes the file
risky. Two further cases get the reason `disguised`, because the sentence shown to
the person differs:

- a **double extension** where a document-like extension precedes a risky one
  (`invoice.pdf.exe`);
- a **mismatch**: the bytes sniff as risky but the name does not say so (a PE file
  named `.pdf`).

A file whose name says `.exe` and whose bytes say so too is `program`. A name of
`.exe` over plain text is still risky (the name is what an operating system
acts on).

### What is not covered

**Archives are not opened.** The scan walk does not enumerate archive members
(ADR-027 does no archive parsing, and this ADR adds none), so a `.zip` holding an
`.exe` is not flagged risky. Likewise a macro-enabled document is recognised by its
extension or declared type only, not by finding `vbaProject.bin` inside a zip, and
a legacy `.doc` with a macro is not detected. These are stated limits, not oversights:
parsing archives to classify them is a larger attack surface than the warning is
worth, and clamd already looks inside archives for known malware. Revisit if the
scan walk ever learns members.

### Server and client

- The server adds one property to every `EmailBodyPart`: **`riskyType`**, a reason
  code from the closed set above (`program`, `script`, `installer`,
  `macro_document`, `shortcut`, `disk_image`, `disguised`) or `null`. The MCP
  `read_message` tool passes it through on each attachment. It is in the
  default body-property set, so a client that names no `bodyProperties` gets it. It
  is computed from the bytes already loaded to build the body; it costs no extra
  blob read, and it is not stored (no migration, no change rows). It applies to
  parts of decrypted mail too.
- The web client shows a dialog before **any** download of a part with a non-null
  `riskyType`: the reader's attachment list, the attachment viewer's Download and
  Open-in-tab, and any other control that goes through the shared download guard.
  Compose has no download control for attachments (forwarded attachments are
  carried as blobs of the draft, not downloaded), so nothing is added there.
- The dialog is plain language ("This is a program file. Only open it if you
  expected it from someone you trust."), has **Download anyway** and **Cancel**,
  Cancel is focused when it opens, Escape cancels, and it is an `alertdialog` with
  a label, so it is read out on open and focus returns to where the person was.
- **Preview stays refused** for these types (415), unchanged: nothing about a
  warning makes a program renderable.
- A third-party JMAP client that ignores `riskyType` gets the neutralised response
  it always got; the warning is a courtesy of our client, not a control. That is the
  same position as the rest of the attachment gate: the server's controls are the
  Content-Type and disposition, not the dialog.

## Consequences

- Opening old mail with attachments can cost up to one clamd call per message; mail
  nobody opens costs nothing. A scanner outage degrades to today's behaviour, and the
  `timeout` and `unavailable` counters say it is happening.
- One new job kind and one new payload key set; no migration.
- Each stale old message produces at most one job per 10 minutes and at most two per
  minute per user.
- A verdict can now go from `clean` to `flagged` for an old attachment at the moment
  it is downloaded. That is the point; the person sees the existing quarantine page.
- The risky list is judgment and needs maintenance. It errs towards warning: a
  `.js` file the person expected costs one extra click.

## References

ADR-027 §1a, §5c, §8; spec §11; `docs/hostile-content.md` §2.4;
`internal/store/rescan.go`, `internal/rescan`, `internal/attachrisk`,
`internal/api/blob.go`.
