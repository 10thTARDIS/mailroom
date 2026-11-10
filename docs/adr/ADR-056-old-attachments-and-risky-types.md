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
| the reading pane fetches a body | `Email/get` with `bodyValues`, `textBody`, `htmlBody` or `attachments` fetched, for the messages returned | a **queued** re-scan job, if any attachment is stale and old |
| an attachment is **downloaded** | `GET /jmap/download/.../{blobId}` for a part | a **synchronous** re-scan before serving |
| an attachment is **previewed** | `GET /jmap/preview/.../{blobId}` | a **synchronous** re-scan before serving |

A list or search request that does not fetch bodies triggers nothing. Mail nobody
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

**An inconclusive answer never downgrades a verdict.** clamd reports a per-file
failure as `unknown` rather than as an error. Writing that over an existing
`clean` would block ordinary old mail because the daemon had a bad second. So a
fresh `unknown` that would replace a stored `clean` is treated as inconclusive:
nothing is written, the request is served under the existing verdict, and a job is
enqueued. (A part that was already `unknown`, or that clamd now flags, is written
normally.) This guard is shared by the synchronous and queued paths.

After a synchronous failure the process skips the synchronous path for 60 s and
goes straight to the queue, so a hung scanner costs one request 10 s, not every
download.

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
| per user per minute | at most **2** such jobs created in any 60 s | a burst of opens (scrolling a list of old mail with previews on) cannot flood the queue |
| per job | **20 s** budget, one message | the handler gives up and returns normally (no dispatcher retry) |

Worst case, with a hung scanner, the open jobs occupy 40 s of a minute, and a
`submit` that falls due waits for the job running plus at most one queued ahead of
it; with a healthy scanner a job is milliseconds. The dispatcher orders by
`fire_at`, so anything due earlier than an open job runs first. This is a bound,
not a guarantee of zero delay, and it is stated as one. A body fetch inspects at
most 10 messages and a request that finds the cap or the per-message cooldown
reached simply does not enqueue.

The check-then-insert for the cap is one SQL statement, but concurrent requests can
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
| `throttled` | the per-minute cap or per-message cooldown refused an enqueue |

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
| `program` | .exe .com .scr .dll .cpl .jar .app .apk |
| `script` | .bat .cmd .ps1 .psm1 .vbs .vbe .js .jse .wsf .wsh .hta .sh .reg |
| `installer` | .msi .msp .dmg .pkg .deb .rpm |
| `macro_document` | .docm .dotm .xlsm .xltm .xlam .pptm .potm .ppam .ppsm .sldm |
| `shortcut` | .lnk .url |
| `disk_image` | .iso .img |

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
`macroenabled` Office types) count too. Declared types are attacker-chosen, so
they can only add a warning, never remove one.

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
  `macro_document`, `shortcut`, `disk_image`, `disguised`) or `null`. It is in the
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
