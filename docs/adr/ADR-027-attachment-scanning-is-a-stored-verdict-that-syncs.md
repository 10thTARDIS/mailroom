# ADR-027: Attachment scanning is a *stored, syncing verdict* — not a check at the click

**Status:** **Accepted** (Forgejo #333). Increments 0–5 built and deployed (v1.57–v1.61); the
client *interaction* for releasing a quarantine remains with #371–#376. Amendments made after
measurement are marked inline as `AMENDED, increment N` rather than rewritten over, because in
three of five increments what this document specified turned out not to match what the database
or the daemon actually did.

⚠ **This is a rewrite, not a revision.** The first draft was reviewed from four angles —
threat model, operations, privacy, evidence quality — and did not survive intact. It
contained a factual error about this codebase, a measurement whose method invalidated its own
conclusion, three internal contradictions, and a blind spot an attacker can select. Each is
recorded where it occurred rather than quietly corrected, because each is the reason a
section now says what it says.

Where a number appears, its provenance is named. Where a claim is judgment rather than
measurement, it is labelled.

## Context

Spec §11 requires *"Attachments never executed; safe `Content-Disposition`; dangerous types
blocked; **optional ClamAV**; sandboxed viewer."* Four of those five exist in some form — the
"blocked" wording is itself wrong (§12). This is the fifth.

`docs/hostile-content.md` lists roughly ten residual risks. **One of them is the one a scanner
addresses**, and it bounds this work:

> **Malware in an attachment the user downloads and runs.** We never execute it, never serve
> it inline, and never name it as a dangerous type — but the file lands on their disk if they
> ask for it. *ClamAV scanning is v1.x.*

⚠ The first draft said that document "names exactly one residual path". It names about ten, of
which this is one. #333 phrased it correctly and the compression lost the qualifier — the
difference between *"this closes the residual risk"* and *"this closes one of ten"*.

## 1. The requirement, and what it does not deliver

The requirement, as stated:

> I want to know that my mailbox is clean. I want to know if I download or preview an
> attachment that it won't trigger something malicious, and I want it to be able to work
> **offline** — if I'm using the thick client or apps and I don't have a connection back to
> the server, I want to know I don't have an untrustworthy attachment silently waiting for me
> to click on it.

**Offline eliminates scan-at-download.** A client with no connection cannot ask the server to
scan. The verdict must be *stored, synced and carried with the message*, which forces scanning
before the message is available, verdicts in `app.attachments` (§3), a JMAP-visible property,
and a backfill. The requirement decided the architecture, not a preference.

### 1a. ⚠ What this does NOT give you, stated before the design that gives it

The first draft let §4 read as though closing the visible-but-unscanned window closed the
*risk* window. It does not, and the honest position belongs first:

- **Scan-at-ingest is the moment of MINIMUM signature coverage.** §5c's own premise is that
  malware ships before its signature. The ingest scan is a **floor** against commodity and
  older malware; the **re-scan cadence is the actual detection mechanism**. The message that
  arrives an hour before you board is the one most likely to carry a false `clean`.
- **A later `flagged` never reaches a disconnected client.** If a re-scan flips a verdict
  while you are offline, you hold the old one until you sync.
- **Offline quarantine is advisory by construction.** The bytes are already on the device.
  Enforcement (§8) is server-side; a disconnected client can decline to help, not prevent.
- **Therefore a bare `clean` must never sync.** For a careful user a stale green badge is
  *worse than no badge*: it inverts the caution that the present absence of any claim
  preserves. What syncs is "scanned, nothing found, as of DB *N*, *T* ago" — never "clean".

⚠ **The offline consumers do not exist yet.** Native apps are 2.x (#64), the Tauri client 3.x
(#72). This ADR specifies the server side so a verdict *can* sync; it does not design a UI for
a client that does not exist.

## 2. What increment 0 measured

`mailroom-admin attachment-report` (PR #378), against a real corpus.

⚠ **Define the unit first, because it is not "files the sender attached".** `internal/mime`
classifies **every non-text leaf part** as an attachment (`walk.go:226-249`) — including
inline `cid:` images and `application/pgp-signature` parts. The report deliberately uses the
system's own definition so its numbers compare with `has_attachments` and JMAP. The
consequence: "233 attachments" counts leaf parts, and an unknown but probably large share of
the 194 JPEGs are inline newsletter images rather than files anyone attached.

| | |
|---|---|
| messages / attachment parts | 2041 / 233 |
| unreadable, truncated, nested | 0 / 0 / 0 |
| largest part | 5.4 MB |
| over clamd's `StreamMaxLength` (see the correction below) | 0 |
| zip-shaped | 0 |
| declared types | 194 jpeg, 17 pdf, 8 pgp-signature, 6 png, 3 ics, 3 octet-stream, 2 csv |
| declared ≠ sniffed | 16 — **13 shown benign**, **3 unexplained** |

⚠ The first draft said "16, all benign". Benignity was demonstrated for 13 (pgp-signature, ics
and csv sniffing as `text/plain`). The three unexplained are the octet-stream-declared parts —
precisely the rows the report's own output flags as worth reading.

**Corrected in increment 3 — `StreamMaxLength` is 100 MB, not the 25 MB this table and §4b
originally said.** 25 MB is upstream's *documented* default; the official `clamav/clamav`
image reports `104857600` (probed with `clamconf`). Nothing about the corpus
changes — the largest part is well under the limit and the count over it is 0 either way — but the
constant in `cmd/mailroom-admin/attachmentreport.go` was wrong, so its "over clamd's limit"
column had been counting files clamd scans perfectly well. Corrected there, and it must now
stay equal to `internal/clamd.MaxStreamBytes` and `deploy/clamd.conf`'s `StreamMaxLength`.
**A documented default is not a measurement of a deployed daemon** — the same lesson as §4a's
timing and §5a's alert settings, arrived at for a third time.

**Falsified premise 1 — "most of what a scanner would catch is already blocked."** That
population is **empty**. `safeContentType` refuses nothing; it downgrades to
`application/octet-stream` and serves with `Content-Disposition: attachment` and `nosniff`.
The download path returns no 4xx on type; only *preview* returns 415. Resolved in §12.

**Falsified premise 2 — "archives and size limits are where a scanner's limits bite."** Not on
this corpus. ⚠ True of *this* mailbox at *this* size only — and the absent formats are exactly
where the limits live. The day a `.zip` or `.docx` arrives, `unknown` (§5) stops being rare.
**Engine choice must be format-agnostic**; the corpus shape orders the *supplements* (§7d),
nothing else.

## 3. The projection a verdict lives in has never been written

`app.attachments` has existed since the scaffold commit, carrying `filename`, `content_type`,
`size_bytes`, `content_hash` and a `scan_status` column already reading
`'unscanned'|'clean'|'flagged'`. **No production code writes it.** Measured: 0 rows, against
messages that did carry `has_attachments` (#383 repaired a few).

Storing a per-attachment verdict therefore means **building that projection first** — a write
path at ingest plus a backfill. Unavoidable under §1.

⚠ **The first draft's claim about the reclaim guard was wrong on three counts, and the error
was copied from #333's evidence comment instead of checked against the code.** It said the
guard exists in "both copies", is "structurally incapable of firing", and is "untested code".
In fact:

- There is **one** guard. `internal/store/blobreclaim.go` opens with *"This file holds the ONE
  blob-reclaim guard the store shares (#300)"*. The other reference in that file is
  `BlobRowCensus`, a read-only report.
- It **runs constantly** — on every draft retire, purge, collapse and account delete. What has
  never happened in production is its *attachments clause matching*, because the table is
  empty.
- It is **tested in both directions**: `TestRetireDraftKeepsABlobAnAttachmentShares` inserts an
  `app.attachments` row and asserts reclaim is withheld; `purge_integration_test.go` does the
  same; the suite carries the positive control. Those tests predate the claim by two weeks.

This is the #169/#220 pattern the project names — a claim from a measurement thread
propagating into a decision document unaudited. It was repeated in #333 and #378 and needs
correcting there too. **What is true:** the clause becomes production-live the moment the
projection populates `content_hash`, and that transition deserves attention even though the
code is covered.

## 4. Decision: scan synchronously at ingest, before the message is visible

⚠ **This reverses an earlier position** that proposed a durable job *after* ingest and accepted
a visible-but-unscanned window. The question was whether messages could be held until
scanned. They can, and it is better: the window is small enough that explaining it costs more
than closing it.

### 4a. The timing, measured three times

⚠ **The first two measurements were both invalid, and the second looked convincing.**

1. `date +%N` is unsupported in the container's busybox, so the first run reported "0 ms" for
   every file — arithmetic on an empty string.
2. The second timed **the same file 50 times**. clamd caches clean results by content hash, so
   iterations 2–50 were cache hits. It measured a cache lookup and called it a scan.

Re-measured with **distinct files**, which a content-hash cache cannot hit (ClamAV 1.5.4, DB
28108, warm daemon, `clamdscan --fdpass`):

| workload | per scan |
|---|---|
| same 5.4 MB file, 20× (**cache hits**) | 53 ms |
| 20 **distinct** 5.4 MB files | **366 ms** |
| 50 **distinct** 50 KB files | **32 ms** |

**The cache made it look ~7× cheaper than it is.** That mattered exactly where the old figure
was used: a §5c re-scan is *triggered by a signature update*, which **flushes that cache** — so
the estimate used the one number its own trigger invalidates.

**Full-corpus cold re-scan: ~9 seconds**, not the 5 the first draft claimed.

⚠ **Still unmeasured, and stated rather than hidden:** the files were random bytes, which
engage no format parser. Real PDFs and archives cost more, possibly much more. The figures also
include per-invocation `clamdscan` spawn overhead a Go socket client would not pay.

⚠ **A detection control ran alongside** — EICAR returned `Eicar-Test-Signature FOUND` in the
same batch. It validates *detection*, not timing method; the timing method needed its own
control, and lacking one is what let error 2 through.

**The decision survives all of it.** Tens to hundreds of milliseconds against a five-minute
sync interval; even a 50× error changes nothing. What did not survive were two sentences
asserting a marginal cost and a total.

### 4b. ⚠ Failure taxonomy — the part that makes §4 safe or lethal

The first draft said "take the quota path exactly". **That is wrong, and three reviewers
independently found the same defect.**

The analogy fails on its own terms. The engine's comment ends — in a clause the first draft
silently trimmed — *"re-fetched on the next pass **once the user frees space or the operator
raises the limit**"*. Quota is **account-global and user-clearable**. A scan failure can be
**per-message and sender-chosen**, and nothing the user does clears it.

⚠ Mechanically: cursors are a single high-water UID, so a message failing at the head of a pass
is re-fetched first on every subsequent pass, forever. A file that reliably times out or
crashes clamd (CVE-2023-20032 is the existence proof) **halts all ingest for the account, at a
stranger's choosing** — a remote kill switch, strictly worse than the malware it would catch.

**The boundary is not observable from the wire without care.** Exceeding `StreamMaxLength`
mid-`INSTREAM` makes clamd error *and close the connection*, which from Go looks exactly like
an outage. (Judgment, ~85% confidence — settle with one oversized-INSTREAM probe before
coding.)

**Required:**

1. **Size pre-check before contacting clamd.** Over `StreamMaxLength` → `unknown`, no scan
   attempted. **Done** (increment 3). ⚠ The constant that already existed in
   `cmd/mailroom-admin/attachmentreport.go` was **wrong** — 25 MB documented against 100 MB
   deployed (§2) — so the live one is `internal/clamd.MaxStreamBytes`.
2. **`PING` after any scan error.** Daemon answers → per-file `unknown`, **ingest proceeds**.
   Daemon silent → genuine outage, refuse the pass. Refusal is then provably daemon-scoped
   without weakening fail-closed. **Done** (`internal/clamd.Scan`).

   ⚠ **The PING must run on the CALLER's context, not the scan's** — a defect found by
   reading during increment 3, before it shipped. `Scan` derived its 90-second deadline into
   the context it then pinged on, so a file that stalls the daemon past `ScanTimeout` left
   that context Done and the PING failed *instantly* with "context deadline exceeded" against
   a daemon answering PONG on the next socket. A per-file timeout would have been reported as
   an outage, which is step 4's failure exactly. `internal/clamd/timeout_test.go` covers it,
   with a control proving a genuinely dead daemon still produces the sentinel.

3. **Circuit breaker keyed on `content_hash`.** N consecutive failures attributable to one
   message → `unknown`, move on. The only defence against the crash case, where scanning the
   file kills the daemon and the resulting error *is* connection-level. **Done** (N = 3,
   `internal/ingest/scan.go`).

   ⚠ **"Consecutive failures" alone is not implementable as written, and implementing it
   literally would leak mail during an outage.** Cursors do not advance across a refusal, so
   the same message is retried first on every pass — during a *genuine* outage a naive counter
   therefore reaches N and waves that message through as `unknown`, then the next, and the
   next: a slow drip of admitted mail with the scanner still dead. The fix is to count only
   failures the daemon can be *excluded* from: PING **before** the first scan of a message, and
   count a connection-level failure only when that PING succeeded. Alive-then-dead is the crash
   case; dead-then-dead is an outage and counts nothing. Once open, the message is never sent
   to the daemon again in that process's lifetime — re-sending a file that has already killed
   clamd three times is how a breaker becomes a loop.

4. **A single message must never pause the account.** **Done**, and it is what 2 and 3 are for.

> **AMENDED, increment 6 (#415): the breaker was built into ONE of the two paths that send
> bytes to clamd, and the other one undid it.** Step 3 above is written as a property of
> ingest, and that is where it lives. But the row ingest writes when the breaker opens is due
> on the next re-scan pass, and `internal/rescan` sent the same bytes to the same daemon with
> no memory of its own: clamd dies, the chunk skips the message *without recording anything*,
> `remaining > 0` reschedules in **thirty seconds**, and meanwhile ingest's pre-scan PING fails
> so the whole account pauses — mail on the provider, cursors withheld, reaper skipped. With
> `restart: unless-stopped` the daemon comes back and the loop repeats. Mail is not lost; it
> simply stops arriving, for as long as a stranger keeps sending the file. **The remote kill
> switch this section exists to prevent, reached by the other door.**
>
> The fix is the same witness §5c's amendment describes: a give-up stamps
> `attempted_db_version`, so the row leaves the predicate and the bytes are re-offered **at most
> once per signature-database change** rather than every thirty seconds. That memory is strictly
> better than step 3's, which is a process-lifetime map — ⚠ **a restart resets ingest's breaker,
> so a crash-file at the head of the cursor can still cost up to ~1.5 h of account-wide refusal
> after every restart.** The durable one does not forget.
>
> ⚠ **Re-scan must NOT adopt ingest's other half.** It is best-effort by design: everything it
> looks at already has a verdict, so recording `unknown` because the daemon hiccuped would
> quarantine ordinary mail (`unknown` blocks exactly as `flagged` does, §5b). The give-up
> touches the attempt columns and nothing else — the standing verdict keeps its real age.
>
> ⚠ **Testing it honestly needs a file that genuinely crashes a daemon**, which a fixture cannot
> supply. A stub returning a fatal error is the substitute, and **the assertion is on the
> reschedule interval, not the verdict** — the stored rows look identical whether the job comes
> back in thirty seconds or in a day, so the schedule is the only witness that separates a drain
> from a spin.

### 4c. On genuine outage: the quota path, including the half that is easy to miss

When the daemon is genuinely unreachable (established by 4b), refuse and leave the mail on the
server — fail-closed, with precedent.

⚠ **The reaper skip is the non-obvious half.** From `internal/mailsync/engine.go`:

> *"Nothing was ingested for this message, `syncMailbox` withheld the cursors …, and returning
> here **SKIPS the reaper entirely** — so 'no room' leaves the mail on the server, exactly
> once, and can never become 'mailbox emptied'."*

Withholding cursors makes the refusal a **retry**. Skipping the reaper is what stops *"we could
not scan this"* becoming *"the user deleted this"*. A refusal doing the first and forgetting
the second would be worse than no scanner.

⚠ **It needs its own sentinel, not `ErrQuotaExhausted`.** The engine matches on the sentinel and
the health string comes from `err.Error()` — reusing quota's would make account health report
*"over storage quota"* during a scanner outage. The new sentinel must name the scanner and say
the mail is safe on the provider.

⚠ **Timeouts must be introduced; none exist.** There is no `WithTimeout` or `WithDeadline` in
`syncrunner`, `mailsync` or `ingest`. A clamd that accepts the connection then stalls hangs the
sync goroutine indefinitely — the pass never returns, so `recordHealth` never runs and **no
error is ever recorded**. A hung scanner is strictly worse than a dead one. The client needs
generous dial and read deadlines (~60–90 s, to survive a database reload) plus one in-process
retry before declaring an outage.

**Scan before the ingest `UserTx` opens** — the MIME parse already happens pre-transaction — so
clamd latency never holds a Postgres transaction open. Write verdict rows inside the same
transaction as the message rows; the change-log invariant requires it anyway.

### 4d. First sync, honestly

Synchronous scanning does **not** reintroduce #351 B2's memory problem: that fix was streaming
ingest, and scans happen per message inside the stream.

⚠ But cursors advance **once per mailbox**, so a clamd blip part-way through a very large
first sync aborts with the cursor unadvanced. Ingested rows survive via dedup, but the next pass
**re-downloads the entire fetch range from the wire** — a transient error during the longest
fetch in the instance's life costs a full re-download. 4b's retry-before-refusing is the
mitigation. Steady-state the amplification is about one message.

## 5. Five states, and the configuration that makes them real

| state | meaning | user-visible |
|---|---|---|
| `unscanned` | no scanner configured, or not yet reached | **in the aggregate** (§5d) |
| `clean` | scanned, nothing found — **always with DB version and age** | yes, qualified (§1a) |
| `flagged` | scanned, something found | **yes** |
| `unknown` | attempted, could not determine | **yes** |
| `released` | flagged, and a human overrode it | **yes** |

⚠ **`released` is a fifth fact the first draft missed.** §7c's stability table says `malicious`
essentially never reverts, so a re-scan will re-flag a released attachment and **resurrect the
quarantine on a cadence** — turning every false positive into a recurring fight. Release must be
persisted state that re-scan respects, and it is a mutation of a JMAP-visible object, so it owes
a `change_log` row.

> **AMENDED, increment 5 (#396): `released` is NOT a value of `scan_status`.** The concern above
> is exactly right and the modelling was wrong. It now lives in `app.attachment_releases`, keyed
> on the part's **content digest** (migration 00044), for three reasons the column could not
> satisfy:
>
> 1. **§8 requires a release to reach every copy of the content.** As a column value that is an
>    N-row write which a copy arriving *later* never receives. As a row keyed on the digest it is
>    one write, and every copy past and future is covered by construction.
> 2. **It destroyed the verdict it overrode.** Writing `released` over `flagged` loses what the
>    scanner said, and this section's own stability table means it would never come back. The
>    verdict now stays `flagged` and keeps being refreshed, so a client can say "we flagged this,
>    you released it".
> 3. **The guard was remembered, not structural.** Every writer of `scan_status` had to carry
>    `AND scan_status IS DISTINCT FROM 'released'`; one that forgot would silently revoke a user's
>    decision. Nothing a scanner writes can reach the release table.
>
> No data migration was needed: no code ever wrote the value (`SetScanVerdicts` refused it by
> construction, and nothing else writes the column), checked against the live database — 267
> `clean`, 1 `unknown`, 0 `released`. The vocabulary is now four states plus a release flag.

### 5a. ⚠ Stock ClamAV collapses `unknown` into `clean`, so the model does not exist by default

By default, clamd hitting `MaxScanSize`, `MaxFileSize`, `MaxRecursion` or `MaxFiles` inside an
archive **stops quietly and returns OK**; encrypted archives return OK. "Over a size limit,
archive too deep → `unknown`" is *not* what a stock daemon reports.

Producing `unknown` requires `AlertExceedsMax` and `AlertEncrypted` — and those then arrive as
**detections** (`Heuristics.Limits.Exceeded.*`, `Heuristics.Encrypted.*`), which a naive
integration maps to `flagged`, quarantining every password-protected zip as malware.

**Both the configuration and the response mapping are mandatory:** heuristic limit/encrypted
families → `unknown`; real signatures → `flagged`. That mapping *is* the difference between
four states and three. (Judgment on the exact family names — verify against clamd.conf
documentation before coding.)

Related: `internal/mime` truncates at 64 MiB, and **a truncated walk must force `unknown`** —
otherwise a message over that limit gets a clean-on-a-prefix verdict while the full payload
remains downloadable as raw `.eml`. **Done** (increment 3, `ingest.forcedUnknownReason`): the
parts are marked `unknown` and **never sent to the daemon at all**, so a design that scanned
and then discarded the answer cannot creep back in.

### 5b. ⚠ `unknown` must have teeth, or the attacker simply opts out

If `unknown` is a badge with a free download, the adversary never needs to beat the scanner —
only to avoid it. Pad past the size limit, nest deeply, or send an encrypted zip with the
password in the body, which is **standard commodity technique, not exotic**.

**Decision: `unknown` gets the same enforcement as `flagged` (§8), with different wording.** "We
could not check this" and "we checked and it is bad" warrant the same friction and different
explanations. Anything less makes §5a's state modelling decorative.

### 5c. "Clean" is a claim with a timestamp, and its freshness trigger can fail silently

**Re-scan cadence** (the design as proposed; the reasoning is right — the detection gap is short,
so re-scanning recent mail is where nearly all the value is):

| age | re-scan |
|---|---|
| ≤ 7 days | on every signature-DB change |
| ≤ 1 month | daily, if the DB moved |
| > 1 month | weekly, if the DB moved |
| > 6 months | stop — and show the age |

⚠ **The version-change trigger fails OPEN — an optimisation that introduced the failure it was
meant to prevent.** Gating every tier on "only if the DB moved" means that when freshclam
silently stops updating — CDN rate-limit, full disk, expired certificate — the version never
changes and **the entire re-scan system suspends itself with no signal**. clamd keeps answering
against a stale DB, and the age shown to the user keeps *advancing*, because scans keep running.

**Required:** store the DB version **and its publication timestamp**; surface **DB age** as
account health beyond ~3 days; treat "DB older than the active tier's period" as a fault, not a
no-op. Also re-scan on engine **major** version change, not only signatures.

⚠ **Cost is not the clamd call.** Attachments are not blobs (§3), so every re-scan loads the
message blob, decrypts with the user's DEK and re-walks MIME. The ~9 s in §4a counts clamd time
only.

⚠ **"Daily if the DB moved" is close to decorative** — the DB moves most days. The tiers are
upper bounds on frequency, not a saving.

⚠ **Stopping at six months is safe ONLY because the age is surfaced** (§1a). Judgment, and the
thinnest-supported decision in this document: the boundaries derive from no measurement of this
corpus's age distribution, and **nothing in the design would reveal them being wrong.** Revisit
once verdict coverage (§13) has run.

**Coverage has now run, and the revisit still cannot happen — which is itself the finding.**
Measured on a small instance, attachment-bearing messages by tier: the `<= 7 days`,
`<= 1 month` and `1-6 months` tiers held mail, and **`> 6 months` held none**.

The oldest attachment-bearing message was only weeks old. The `> 6 months` boundary is not merely
unvalidated, it is **unreachable there** — and a mailbox of several years would be dominated by
that same tier, the opposite shape. Nothing here justifies moving a boundary.

**What it justifies is observability instead of a better guess.** The census emits every tier
*including the empty ones* — a `GROUP BY` drops a band with no mail in it, and a vanished row
reads as "not applicable" rather than "zero", which is exactly how the unvalidated boundary would
have disappeared from its own audit. The day that last row stops being empty is now something
seen rather than assumed.

⚠ Second-order, and worth stating because it inverts this section's premise: **on this corpus the
tiering saves nothing.** Every attachment sits in an actively re-scanned band, so the whole corpus
re-scans at least weekly. The tiers are upper bounds on frequency, not yet a reduction.

> **AMENDED, increment 6 (#409/#407/#415): the queue had no termination argument, and the
> missing one cost more than any tier boundary could.** This section specifies when a row
> becomes *due*. It never says when a row *leaves* — and the predicate that implements it
> answered only the first question, so three ordinary populations were due forever:
>
> 1. **A part that decoded to zero bytes.** `attachscan.PartHash` returns nil for it by design
>    (hashing nothing yields one identity every empty part in the mailbox would share, so one
>    release against it would open the gate on all of them), so `part_hash` stayed NULL on every
>    pass — and increment 5's digest arm deliberately bypasses the tier gate.
> 2. **A row whose message blob would not read.**
> 3. **A row whose message the parser now finds no attachment parts in.**
>
> ⚠ **And the damage was not the wasted work.** The chunk query is `ORDER BY m.id LIMIT 50` over
> ingest-ordered ULIDs, so once fifty such messages precede a genuinely due one, **the same fifty
> ran forever and nothing else was ever re-scanned** — including the `≤ 7 days` tier, which is
> the entire value of re-scanning — while the job logged a full chunk every thirty seconds.
> Latent on a small instance (no empty parts); ordinary on a decades-old mailbox, which
> is full of empty `ATT00001.txt` artefacts.
>
> **The rule this section should have carried from the start: a row leaves the queue when a pass
> has VISITED it, whatever came of the visit.** Implemented as `app.attachments.attempted_at` /
> `attempted_db_version` (migration 00045), stamped by every pass — the ones that write a verdict
> and the ones that give up alike — and read as the predicate's leading factor, *"we have not
> already tried this row at this signature version"*. Termination is then structural rather than
> argued: **the due set strictly shrinks within a signature version**, and a version change is
> what legitimately re-opens it.
>
> ⚠ **The attempt is deliberately NOT `scanned_at`.** §1a makes the age of a verdict the whole
> justification for the `> 6 months` tier never being re-scanned, and `attachment-report` prints
> it. A give-up that refreshed it would claim freshness in the one surface that exists to stop
> that claim being made. Two columns, because they answer two questions.
>
> ⚠ **A zero-byte part is excluded permanently, not retried**, via `size_bytes > 0` in the digest
> arm: a part with no bytes has no content identity to compute, which is the definition rather
> than a workaround. That equivalence spans three packages (`mime` sets `OctetSize = len(data)`,
> `attachproj` copies it to `size_bytes`, `attachscan` calls the same emptiness `HasNoBytes`), so
> it is pinned by a test rather than left as a reading of two files.
>
> ⚠ **The audit could not see any of this**, which is the part worth carrying forward.
> `attachment-report` computed "due for re-scan" from stamped versions alone and never learned
> about the digest arm, so it printed `due for re-scan: 0` while the job had work forever.
> v1.61.1 had extracted a shared SQL constant precisely so the SELECT and the COUNT could not
> drift, **and missed the third consumer.** The report now calls the job's own
> `CountMessagesDueForRescan`. A check that re-derives what it audits is not a check.

### 5d. ⚠ "Is my mailbox clean" needs an aggregate, or it is unanswerable

If `clean` and `unscanned` are both silent, they are the same pixels: mid-backfill silence means
"not yet scanned", afterwards it means "clean". The mailbox-level claim requires an explicit
aggregate — *"233/233 scanned, 0 flagged, 2 unknown, oldest verdict 4 days"* — in which any
`unscanned` row **degrades the answer** rather than vanishing into it.

## 6. ⚠ Encrypted mail is an attacker-selectable blind spot, and the first draft never mentioned it

WKD publishes the user's public key, so **any stranger can encrypt to this mailbox**. At
ingest, an attachment inside a PGP/MIME message is an opaque octet-stream part: clamd scans
random-looking bytes, returns OK, and the row records `clean`. The user decrypts later and opens
a file wearing a green badge **that was never scanned in plaintext**.

This is worse than no verdict, because the attacker *chooses* the path and the design
affirmatively endorses the file. The first draft contained zero occurrences of "PGP" or
"encrypt" despite this project having an entire PGP subsystem.

**Required:** `pgpmail.Signals` already detects `PGPMIMEEncrypted` with no key material. An
encrypted part maps to **`unknown`, never `clean`**, and the scan moves to wherever plaintext
exists once #47's decryption lands. Same rule for encrypted archives (§5a).

**Done** (increment 3, `ingest.forcedUnknownReason`), covering `PGPMIMEEncrypted` **and**
`SMIMEEncrypted`. ⚠ Inline PGP (`PGPInlineMessage`) is deliberately **excluded**: its
ciphertext sits in a text body rather than an attachment part, so no attachment bytes are being
blessed and the parts alongside it are genuinely scannable. Folding it in would mark ordinary
attachments `unknown` because the sender happened to paste an armored block into the body.

> **AMENDED (#47 increment 6b, ADR-037): the scan has moved to where plaintext
> exists, which is the request.** Decrypted plaintext is never stored, so there is nothing
> to scan at ingest and nowhere to keep a verdict. Parts of a decrypted message have their
> own partIds (`e.<path>`) and are scanned by clamd ON DEMAND when downloaded or previewed,
> under this ADR's rules unchanged (§5a forced `unknown`, §5b `unknown` blocks, §8 the same
> 403, a release by content digest honoured). The ciphertext's stored parts keep their
> forced `unknown`. Nothing is cached. See ADR-037 §5.

## 7. Engine choice

### 7a. ClamAV is the right shape, and its limits belong in the document

Runs locally, standard mail-gateway scanner, clean daemon protocol, licence-compatible. Nothing
else open-source fits a self-hosted private mail server as well.

⚠ Its detection rate against fresh or targeted malware is **generally considered** weaker than
commercial engines. This ADR does not benchmark that, and published comparisons are contested
and often vendor-funded — so *"ClamAV alone therefore my mailbox is clean"* overstates what it
delivers, and no UI wording may imply it.

### 7b. Submitting content to a third party is ruled out

Uploading attachments to VirusTotal or similar sends private mail to a service that retains and
shares samples. Incompatible with a self-hosted encrypted mailbox, and it would undo the posture
ADR-026 and `internal/egress` exist to establish.

### 7c. Hash checking: mirror a dump, do not query per file

⚠ **This section was rewritten twice before arriving here.** The first draft ruled out
VirusTotal wholesale; it was correctly objected that hash lookup is not upload. The second
adopted per-hash queries with a decaying re-query window — and had the re-query rule backwards,
which was also corrected. Review then found that **the query paradigm is the wrong
shape**, which dissolves both arguments.

**Decision: mirror a hash corpus locally and check locally.** abuse.ch's MalwareBazaar publishes
full dumps plus deltas. Fetching them is one more **scheduled, content-independent** egress — the
same privacy shape as signature updates — and it is *better on detection*: every hash can be
re-checked on every delta, forever, at zero marginal privacy cost. No rate limits, no per-file
network path, no cost question, and the decaying-window mechanism evaporates.

**Per-file network lookup is NOT adopted.** Recorded because it took three attempts to get here —
what was wrong with it:

- **A hash disclosure appreciates retroactively.** If a confidential document leaks later and
  someone submits it, retained query logs become dated proof you held it before publication.
- **The sequence is the leak.** Per-ingest queries timestamp every attachment arrival; the
  multiset over months profiles the corpus.
- **It is attributable to a person**, not an API key — a single-user instance's stable egress IP
  is itself an identifier.
- **It leaks to on-path observers**, not only the service: DNS and TLS SNI per ingest reveal
  mail-arrival timing to the ISP without the service's cooperation.
- **The re-query rule was backwards.** Stability runs `malicious` > `clean` > `not seen`, so 404s
  are the *highest*-value re-query — the opposite of the second draft. The correction is
  preserved because it is right; it simply no longer matters.

⚠ Two earlier claims are withdrawn: that lookup does not expose files to the service's customers
(**asserted, not verifiable** — say "not uploaded" and stop), and that a spreading file appears
publicly "within hours" (**received wisdom**; per-recipient polymorphism makes targeted mail a
permanent miss regardless).

**The load-bearing rule survives and applies to a local corpus identically: a miss is `unknown`,
never `clean`.** ⚠ But a miss must not *set* `unknown` — most personal attachments are unique and
will miss forever, which would make `unknown` the common state and flood the one warning that
matters. **A hash-corpus miss contributes nothing**; only a hit sets `flagged`. The local engine
owns the `clean`/`unknown` distinction.

### 7d. Complements

⚠ Verify current terms before adopting any of these; licensing changes, and this document should
not be trusted as current on it.

- **Extra ClamAV signature databases** — SaneSecurity is the best known. ⚠ **Its justification in
  the first draft was wrong**: phishing signatures target bodies and URLs, and with per-part
  scanning **the scanner never sees a body** (`walk.go` routes inline text to
  `msg.Text`/`msg.HTML`, not `msg.Attachments`). Re-justify against attachment content, or drop.
- **Structural analysis by capability** — for PDFs: embedded JavaScript, launch actions, embedded
  files. Catches what no signature has seen, needs no network. Extend per format as the corpus
  changes; Office macro analysis the day the first `.docx` arrives.
- **The declared-vs-sniffed signal** the report already computes.
- **YARA** as a ClamAV extension point, not a separate product.

⚠ Third-party sets, the aggressive ones especially, are **reported to carry higher false-positive
rates** (judgment, not measured here). That interacts directly with §8 and `released`.

## 8. ⚠ Where quarantine is enforced — the first draft had no enforcement point

It specified "preview refuses, download behind a release" as though those were the two doors.
They are not:

- **CID inline images render on message open**, fetched through the **download** route. A flagged
  image is parsed by the browser's codecs with **zero clicks** — so "the text is not the threat"
  is false as stated: an attachment can *be* part of the rendering.
- **Image and text previews never touch `PreviewHandler`** — they render client-side from
  download-route bytes. A 415 there gates PDFs only.
- **`serveWholeBlob` streams the raw message** with the attachment base64'd inside.
- **The exporter does the same.**
- **Third-party JMAP clients ignore a property** entirely.

**Decision: one server-side gate on the part-download route, keyed on state plus a persisted
release** — which then covers CID rendering, client-side previews, foreign clients and raw fetch
automatically. Raw-message and export paths need their own decision (§11).

This is architecture, not the UI detail deferred to #371–#376. What is legitimately deferred is
the *interaction*: a confirmation dialog clickable on muscle memory is theatre; a release so
obstructive that a false positive becomes a deletion is the opposite failure.

**Unchanged from the first draft, and still right:** the message stays readable, and nothing is
ever deleted automatically. The scanner is the component most likely to be wrong.

⚠ **Quarantine acts on content, not on one row.** Re-delivered mail means the same attachment
exists as N rows sharing a `content_hash` (#206). A quarantine or a release must hit **all** of
them — the writeback-hit-1-of-N-copies bug's exact shape.

> **AMENDED, increment 5 (#396): the requirement stands, the join key did not exist, and the
> reason it duplicates is not the one named above.** Two measurements taken before any of this
> was built:
>
> - **`app.attachments.content_hash` is NULL on all 268 rows.** That column is a **blob
>   reference** — `blobreclaim.go` reads it to decide whether a blob is still in use — and
>   `internal/attachproj` leaves it NULL deliberately, because a part's bytes are a slice of the
>   message blob and there is no per-attachment blob to name. Writing a part digest into it would
>   arm the reclaim guard against blobs that do not exist. Migration 00044 therefore adds a
>   separate `part_hash`, computed in `internal/attachscan` where both scanning paths already
>   hold the decoded bytes, so ingest and re-scan cannot derive it differently.
> - **Zero messages share a `dedup_hash`.** There are no redeliveries on this corpus at all, so
>   #206 is the wrong citation. What duplicates is the case `blobreclaim.go` already names — the
>   same attachment on several **different** messages: by the only proxy available before a
>   digest existed, `(filename, size, content_type)` equality, a large share of rows,
>   one newsletter image on a dozen messages. ⚠ That figure is an upper bound and provably loose
>   (eight `signature.asc` at 307 bytes are eight *different* signatures), which is why
>   `attachment-report` now prints the true digest-based census.
>
> **What enforcement does, as shipped:** `flagged` and `unknown` are refused at the part-download
> route AND at the preview route (which renders bytes inline into a browser's PDF engine — the
> more dangerous of the two doors), and cannot be attached to an outgoing draft (§11's
> forward-malware path, gated for a stronger reason than the download: attaching puts the bytes
> on the wire to someone else). A new `flagged` verdict propagates to every row sharing the
> digest; `clean` deliberately does **not** propagate, so a retracted signature clears each copy
> on its own tier rather than letting one message's re-scan unblock content another still flags.
> `unscanned` does **not** block — an instance running no scanner has every row in that state.

## 9. What syncs, and what does not

⚠ **The first draft required two things that conflict**: that verdict age and DB version be
user-visible, and that verdicts be JMAP-visible for offline clients. If age and version sync as
properties, every re-scan in the 7-day tier appends `change_log` rows and forces every client to
refetch — several times a day, pure noise. If they do not sync, §1a's "clean as of *T*" display
is impossible offline.

**Decision: sync state transitions only** (`unscanned`/`clean`/`flagged`/`unknown`/`released`)
through `change_log`. Serve scan age and DB version through a **computed read** on the
account-health envelope — the `searchStalenessSeconds` pattern: operational telemetry rather than
a synced object, and demonstrably wired end to end.

A fully offline client then shows age as of its last sync. That is honest — it *is* the freshest
claim it can make.

⚠ The **backfill** is the same problem in one burst: full-corpus verdict writes mean change rows
for every attachment-bearing message, plus search-index replay. Survivable for a small corpus; a full
client resync of a very large one is not.

> **AMENDED, increment 5 (#396): "append a change row on every mutation" is the obvious reading
> of the load-bearing invariant and it is the WRONG one here.** Re-scan rewrites every row of
> every due message on every pass, and §5c's tiers keep this entire corpus in an actively
> re-scanned band — so a row per `UPDATE` is ~257 `Email` change rows a week, forever, for
> verdicts that did not move, forcing a full client refetch each time. That is precisely the
> noise this section refuses for verdict age and DB version, arriving through the state instead.
>
> The invariant is about **transitions**. `store.writeVerdicts` reports the row's PREVIOUS status
> (a CTE, because `UPDATE … RETURNING` yields the new value and cannot tell a move from a
> rewrite) and appends a change row only when the state actually moved. A release, and the
> propagation of a `flagged` verdict to other copies, append one row per affected message —
> including messages the caller did not name, which is the point.
>
> **The property is keyed by `partId`, not hung off the `attachments` array**, and that is forced
> rather than stylistic: `mime.IsAttachmentLeaf` counts a `Content-Disposition: inline` image as
> an attachment (so it carries a verdict), while RFC 8621 puts a CID-referenced image in
> `htmlBody` and never in `attachments`. A property on that array would therefore describe
> everything **except** the parts that render with zero clicks — the exact case §8 exists for.
> `Email.attachmentScan` is a Postgres read, so it costs no blob parse and a list view can carry
> the badge.

## 10. Deployment

### 10a. ⚠ Memory: clamd becomes the largest thing in the stack by an order of magnitude

**Measured** (`docker stats`, signatures loaded, idle):

| | RSS |
|---|---|
| **clamd** | **1.12 GiB** |
| mailroom | 17.9 MiB |
| postgres | 64.7 MiB |
| crypto | 4.5 MiB |
| **entire existing stack** | **~87 MiB** |

**The scanner is ~13× everything else combined.** `ConcurrentDatabaseReload` (the default)
roughly doubles it transiently on every signature update.

⚠ **`deploy/compose.yml` sets no memory limits on any service.** The OOM killer therefore chooses
by RSS and may shoot **Postgres**, converting a scanner problem into a whole-app outage — or put
clamd in a restart loop where each restart is a 30–60 s CPU spike plus an ingest-refusal window.

**Required:** a stated host floor (≈ +1.5 GiB steady, +3 GiB during reload; or non-concurrent
reload with §4c's patient timeouts), and an explicit memory limit **on clamd specifically** — it
is the one service whose death is designed for, so make it the process the kernel picks. **A 2 GB
host does not fit this design.**

### 10b. ⚠ Network isolation does not exist, so the egress constraint would be unenforced

`deploy/compose.yml` contains **zero** `networks:` configuration — every service sits on the
default bridge with unrestricted egress. "It reaches only the signature endpoints" would be an
assertion with no mechanism, in the container that parses hostile input.

**Required, using the repo's own pattern:** clamd with **`network_mode: none`**, reached over a
unix socket in a shared volume — exactly the crypto-process arrangement. Signature and hash dumps
fetched by a Mailroom-side job through `internal/egress` (host-pinned policy), or by a
fetch-only container holding no mail data, writing into the shared volume.

That makes the constraint true by construction, and downgrades "compromised clamd exfiltrates
every attachment" to "compromised clamd lies about verdicts".

⚠ **Noticed while verifying, and out of scope here:** the same absence means the **crypto**
container — which holds PGP private keys — also has unrestricted egress today. Pre-existing;
worth its own issue.

### 10c. Egress, cold start, and data at rest

The remaining outbound paths are **scheduled and content-independent**: signature updates and
hash-corpus dumps. Same shape ADR-026 accepts, and far weaker than per-message egress.

⚠ freshclam carries a **persistent per-install identifier** and a ~2-hourly heartbeat — a durable
install identity and an uptime oracle. §10b's Mailroom-side fetch removes both for free;
`internal/egress` already fixes a neutral User-Agent.

⚠ **The signature DB must live in a named volume.** Otherwise every container recreate
re-downloads ~300 MB, and the CDN rate-limits repeated full downloads per IP — which can lock
freshclam out for hours, feeding §5c's fails-open failure directly.

⚠ **Do not add `depends_on: service_healthy`** from mailroom to clamd: it would block the whole
app — reads, sends, the web client — on a minutes-long download, and make a permanently broken
clamd take the server down. The per-ingest refusal is the better failure.

⚠ **clamd writes plaintext outside the encrypted store.** Scan temp files (decompressed archive
members) are deleted, not shredded; detection log lines name malware family and time in shared
logs. Required: `read_only` container with tmpfs on the temp directory, `LeaveTemporaryFiles`
off, **INSTREAM only so no filenames ever reach clamd or its logs**, and a decision on log
retention. Quarantine is a policy state over the existing encrypted blob — **never a plaintext
copy-out** — and clamd must never quarantine anything itself.

⚠ Attachment-granularity `content_hash` in DB backups is a **confirm-possession oracle** for
widely-distributed files, which message-level hashes are not (unique headers make whole messages
unguessable). Inherent to ADR-004's content addressing; acknowledged, not fixed.

## 11. ⚠ What is not scanned, and must be said out loud

- **Compose and the importer write no `app.attachments` rows.** Outbound and imported mail would
  be unscanned forever — the design as drafted lets a user **forward malware with no verdict**.
  Both must at minimum write projection rows with an explicit state.
- **Whether a scanner outage blocks the sent-copy ingest** is undefined. Under ADR-024's
  no-fallback posture it plausibly should, with the consequence "cannot send while the scanner is
  down". That must be a decision, not an accident.
- **The importer's error handling is wrong for this.** It burns 50 consecutive failures and then
  blames "the database or blob store" — misleading during a scanner outage. It should fail fast on
  §4b's daemon-level check with a scanner-named error.
- **Raw message fetch and export bypass §8's gate.** Export especially: `exportplan` disposes
  `attachments` as *"MIME parts inside the raw bytes"* — true for content, false for verdicts. **An
  export would ship a flagged file's bytes with no verdict**, resurrecting the silently-waiting
  attachment on the next instance. Verdicts and release state must join the per-message sidecar,
  amended in the same increment that first writes the table.

  > **CHECKED IN INCREMENT 5, AND IT HAD NOT HAPPENED.** "Amended in the same increment that
  > first writes the table" was increment 2; it was not done, and nothing noticed for three
  > increments. Verified rather than assumed: `sidecarMessage` carried
  > `content_hash, file, labels, keywords, junk, score, score_model` and nothing else, and the
  > strings `scan`, `verdict`, `quarantine` and `attachment` appeared nowhere in
  > `internal/exporter/`. So the predicted consequence was live the whole time. Done now: each
  > part's `state`, `released` and `filename` ride in `messages.jsonl`, and the `exportplan`
  > entry that made it easy to miss is reworded — "attachments are MIME parts inside the raw
  > bytes" reads as a complete disposition and is only half of one.
  >
  > **Decision on the raw-message download: it is NOT gated, and says so in a header.** A
  > quarantined part sits inside those bytes base64-encoded — inert until something deliberately
  > extracts it, which is a different act from a browser decoding an inline image on message
  > open. Blocking it would make a message with one bad attachment un-fetchable, un-forwardable
  > and un-exportable in its entirety, which fails this ADR's own "the message text stays
  > readable" rule and §12's posture. What it must not be is silent: the response carries
  > `X-Mailroom-Attachment-Scan` naming the worst unreleased state, and the export sidecar is
  > the durable record.
- **Filename deception is untouched.** Double extensions and bidi overrides achieve "user downloads
  and runs" with no bytes for any scanner to flag. A live defect exists here and is being fixed
  separately.

## 12. Decision: the spec wording moves, not the code

Spec §11 claims dangerous types are **blocked**; §2 establishes the code *neutralises*. Refusing
to hand a user their own file is user-hostile, the browser-side risk genuinely is neutralised, and
the residual — the file on disk — is what this ADR addresses. **Spec §11 should read *neutralised
and never rendered*.** `hostile-content.md`'s residual entry must be **reworded, not removed**,
since §1a establishes the risk is narrowed rather than closed.

**Done in increment 5 (#396).** Spec §11 now reads "neutralised and never rendered", with the
mechanism named (the server chooses the Content-Type from a small allowlist) and the one place
the scanner DOES withhold bytes stated beside it, so the sentence is not read as "we block
nothing". `hostile-content.md`'s entry is reworded rather than removed and grew the four
residuals that survive a working scanner: a `clean` verdict is a claim with a timestamp and
ingest is the moment of minimum coverage (§1a); a release is a user decision the design
deliberately allows; the raw message is never blocked; and an encrypted or over-limit part is
`unknown` rather than examined.

## 13. Increments

1. **Verdict coverage in `attachment-report`, before any scanner exists.** Counts per state, per
   tier, oldest verdict, current-vs-stamped DB version. The project's own report-first rule applied
   to the scanner itself; the tool exists and is already run. It is the single audit that catches
   every silent failure above — freshclam rot, dead re-scan chunks, version-stamp races.
2. The `app.attachments` projection at ingest, plus backfill (§3).
3. clamd sealed per §10b, with §4b's taxonomy and §5a's configuration. **Done** (v1.59.0):
   the client, the sealed sidecar, ingest wiring, and the account-wide pause — cursors withheld
   **and the reaper skipped** — on `ingest.ErrScannerUnavailable`.
4. Re-scan tiers (§5c), chunked and **version-predicated** (`WHERE scanned_db_version < current
   AND tier due`) so interruption and mid-pass signature updates self-heal; chunks well under the
   15-minute lock lease, which has no heartbeat; read the DB version *before* each chunk and stamp
   what was read.
   ⚠ The job dispatcher is **sequential** — one goroutine — so a long re-scan **blocks `submit` and
   outbound mail stops**. Chunking is what prevents that, not an optimisation.

   **Done** (v1.60.0). Bounded twice in the end: 50 messages **and** a two-minute wall clock, the
   second being the guard that actually holds — a count cannot bound time when part sizes span
   three orders of magnitude.

   ⚠ **THE PREDICATE SPELLED OUT ABOVE WOULD HAVE DONE NOTHING, and it stood in this document and
   in #395 unchallenged.** `scanned_db_version < current` matches NOTHING for a never-scanned row,
   because `NULL < n` is NULL rather than true. Checked against the live database before any of it
   was written: it would have skipped all 255 unscanned attachments — the entire backlog the
   increment exists to clear — while the job logged a successful pass. The live predicate is
   `(scanned_db_version IS NULL OR scanned_db_version < $1)`, and the census prints the
   never-scanned count on its own line so a backlog that is not moving is a number rather than
   silence. A specification is not a measurement either.

   ⚠ **AND THE SAME PREDICATE WAS WRONG A SECOND TIME, FOR THE SAME REASON, ONE INCREMENT
   LATER.** Increment 5's `part_hash` is written by the pass that writes a verdict, so every row
   predating migration 00044 has a verdict and **no digest** — and a release is keyed on the
   digest. Those rows can be quarantined and **cannot be released**: a gate with no key. The
   version arm does not reach them either, because they are stamped at the *current* version and
   are therefore behind nothing; the backfill would have arrived whenever freshclam next moved
   the number, which is luck about an external timer rather than a design. Caught by asking what
   the deploy would actually do before running it, not by a test — there were none for this
   query at all. The live predicate carries a third arm, `OR part_hash IS NULL`, it is now a
   shared constant so the SELECT and the COUNT cannot disagree about "remaining", and it finally
   has tests.

   Three defects in one predicate, all the same shape: **the row that needed work did not match,
   the job ran, and it logged a successful pass.** None of them could produce an error. That is
   the failure mode this whole document keeps circling, arriving in the same four lines of SQL
   each time.
5. Enforcement (§8) and the client surface (#371–#376).

   **Done** (v1.61.0, #396) — the server half. One gate on the part-download route, plus the
   preview route and the draft-attach path; `Email.attachmentScan` keyed by `partId`;
   `Email/releaseAttachment` and `app.attachment_releases`; the export sidecar §11 had been owed
   since increment 2; the digest census in `attachment-report`.

   ⚠ **THE JOIN KEY §8 NAMES DID NOT EXIST, AND THE DUPLICATION IT CITES IS NOT WHAT DUPLICATES.**
   `content_hash` is NULL on all 268 rows on purpose (it is a blob reference the reclaim guard
   reads), and zero messages share a `dedup_hash`, so the re-delivery framing was wrong too.
   Both measured before writing anything; see the amendment in §8. Third increment running in
   which a specification turned out not to be a measurement.

   ⚠ **AND THE INVARIANT, APPLIED LITERALLY, WOULD HAVE MEANT A WEEKLY FULL RESYNC** — a change
   row per verdict write, over a corpus every tier re-scans. See §9's amendment.

   What remains is the client *interaction* (#371–#376): the property, the method and the data
   model are here for it to build against.
6. **Termination: a row the scanner could not process must still leave the queue** (#409, #407,
   #415). Not a feature — the correction to the same four lines of SQL, for the fourth time, and
   the first one where the shape is *different*.

   The three earlier defects were all "the row that needed work did not match". This one is its
   mirror: **the row that could never be finished always matched.** A part that decoded to zero
   bytes can never acquire a `part_hash` (that is deliberate — hashing nothing yields one
   identity every empty part would share), so increment 5's tier-bypassing digest arm selected it
   on every pass forever. Same for a blob that would not read and a message the parser now finds
   no parts in.

   ⚠ **And the consequence was starvation, not waste.** The chunk is `ORDER BY m.id LIMIT 50`
   over ingest-ordered ULIDs, so fifty stuck messages at the head meant **nothing else was ever
   re-scanned** — while the log said `messages=50` every thirty seconds. Old mail is full of
   empty `ATT00001.txt` artefacts; fifty is ordinary on a real mailbox and there were none on the
   test instance, which is why it shipped.

   Delivered: `attempted_at` / `attempted_db_version` (migration 00045), stamped by every pass
   including the ones that give up, as the predicate's leading factor — see §5c's amendment for
   the termination argument and §4b's for how the same witness closes the re-scan path's missing
   circuit breaker. Plus `SetScanVerdicts` and `StampScanVerdicts` collapsing into one method
   with a required stamp, because **two write paths where only one stamped** is what #407 was.

   ⚠ **The audit was blind to all of it**, and that is the finding to carry. `attachment-report`
   re-derived "due for re-scan" from stamped versions and never learned about the digest arm, so
   it could print `0` while the job had work forever. Increment 5 had extracted a shared SQL
   constant *specifically* so the SELECT and the COUNT could not drift — and missed the third
   consumer. **A check that re-derives what it audits is not a check**; it now calls the job's own
   count.

## 14. Still open

- Which §7d supplements to adopt — needs current terms checked.
- MalwareBazaar dump availability and terms (§7c).
- The §5c tier boundaries: judgment, currently unfalsifiable.
- Quarantine-release *interaction* (§8), deferred to the client milestone. The **data model** for
  release is not deferred — **landed in increment 5**: `app.attachment_releases`,
  `Email/releaseAttachment` (with `revoke`), and `mailroom-admin quarantine` as the
  recovery path while the client surface is still to be built. What is still open is the
  human question §8 poses: a confirmation clickable on muscle memory is theatre, and a release so
  obstructive that a false positive becomes a deletion is the opposite failure.
- **Whether outbound attachments should be scanned at submit.** Increment 3 decided a scanner
  outage must not block sending, and named this as the consequence: `internal/compose` writes
  projection rows that stay `unscanned`, and `unscanned` does not block. Increment 5 changes the
  stakes rather than the answer — now that the gate exists, a verdict on outbound mail would
  finally do something observable, and the draft-attach gate already stops a user forwarding a
  part *we* flagged. What it does not cover is a file the user attaches from their own disk.

### Answered in increment 3

**Log retention for clamd detection lines (§10c): clamd writes no detection log at all, and
Mailroom's structured log is the only record.** `deploy/clamd.conf` sets no `LogFile` and no
`LogSyslog`, so the container's own output is startup and errors; a detection is recorded by
`internal/ingest` instead, at `WARN`, and inherits the app's existing rotation — the
`json-file` driver's `10m x 5` in `deploy/compose.yml` (#351). No separate retention policy is
needed, which is the point: a second log with its own lifetime is a second thing to get wrong,
in the one container that should hold least.

⚠ **The line deliberately omits the filename.** clamd never learns it — we speak `INSTREAM`
precisely so no filename reaches the scanner or its logs — and it is attacker-controlled text
whose purpose in this threat model is deception (§11's double extensions and bidi overrides).
Logging it would re-introduce on our side the exposure `INSTREAM` removes on clamd's, and put
an attacker's string in an operator's console. `content_hash` plus `part_path` names the part
unambiguously for anyone who needs to look it up.

**Whether a scanner outage blocks sending (§11): NO.** ADR-024's no-fallback posture does not
transfer, and the reason it does not is worth stating because the two look alike. Signing in
the app would be *the security boundary deleting itself* — there is no safe degraded mode. A
sent copy filed with `unknown` verdicts is not a degraded mode at all: `unknown` carries the
same enforcement as `flagged` (§5b), so the user's own copy is gated exactly as an
unscannable inbound attachment would be. And the mail leaves over SMTP regardless of what our
projection says, so refusing to file our copy protects nobody and locks the user out of their
own mail server because a sidecar is restarting.

⚠ **What that leaves undone, said out loud rather than implied.** Outbound attachments are
therefore **not scanned at all** in this increment: `internal/compose` writes projection rows
(#393) that stay `unscanned`, and §5d's aggregate reports them as such rather than folding them
into "clean". Scanning at submit belongs with increment 5, because until §8's download gate
exists a verdict on outbound mail changes nothing anyone can observe. The decision recorded
here is only that an outage must not *block* the send — which the current code already
satisfies, and now on purpose.

**Also settled by measurement rather than left open:** `StreamMaxLength` (§2), and the
heuristic family names in §5a — `Heuristics.Encrypted.*` and `Heuristics.Limits.Exceeded.*`,
matched as prefixes so a future family member is `unknown` without a code change, while a real
signature merely *containing* the word "Encrypted" still flags.

## Consequences

- `app.attachments` becomes a written table for the first time, with a backfill; its clause in the
  reclaim guard becomes production-live (§3).
- Ingest gains a synchronous dependency on clamd **when configured**, with a failure taxonomy that
  must distinguish daemon outage from per-file failure, or a single message wedges the account
  (§4b).
- Timeouts must be introduced into a sync path that currently has none (§4c).
- A five-state model, of which two states did not exist in the first draft and one does not exist
  in stock ClamAV's default output (§5, §5a).
- A new JMAP-visible property, therefore a `change_log` row on every state transition — with age
  and version deliberately *not* synced (§9).
- Two scheduled egress paths, both content-independent, with the sidecar sealed by
  `network_mode: none` (§10b).
- A host memory floor the current deployment does not state and a 2 GB host cannot meet (§10a).
- Spec §11's wording changes; `hostile-content.md`'s residual entry is reworded (§12).
- A claim shown to the user that the project is then responsible for keeping true — and which, per
  §1a, is never the word "clean" on its own.
