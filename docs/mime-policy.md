# MIME parsing policy

One page, per spec §17 (0.x roadmap: "MIME golden corpus") and the design
review (§6 item 6, "Charset/MIME/EAI policy"). Implemented by
`internal/mime` (`docs/mime-policy.md` and the package must stay in sync —
if they disagree, fix the code or fix this doc, don't let them drift).

## Why this exists

Mail arriving at Mailroom is adversarial by construction: it comes from the
open Internet, unauthenticated senders, years of accumulated implementation
bugs in other people's mail clients, and — eventually — from people
actively trying to break a self-hosted mail server. `internal/mime` is the
one place raw bytes off the wire turn into structured data, so it is the
one place that must never crash, hang, or exhaust memory no matter what
those bytes contain. This document is the explicit contract for what
"never crash" means in practice: every fallback below is a deliberate
choice, not an accident of whatever the underlying library happened to do.

`internal/mime` wraps `github.com/emersion/go-message` (plus its `mail` and
`charset` subpackages) behind Mailroom-owned types (CLAUDE.md:
anti-corruption layer). Callers outside the package never see an emersion
type; every accessor on `mime.Message`/`mime.Header` is safe to call on
hostile input and never panics.

## The rules

### 1. Malformed RFC 2047 encoded-words

**Best-effort decode; on failure, fall back to the raw token text; never
crash.**

`Header.Subject()` and address-header decoding use RFC 2047 (`=?charset?
{Q,B}?text?=`) decoding. A single malformed encoded-word (bad base64
padding, an unrecognized encoding letter, unbalanced `?`s) does not fail
the whole header: only that token falls back to its raw, undecoded text,
and decoding continues for the rest of the header. See
`testdata/corpus/malformed_rfc2047_subject.eml`.

### 2. Unknown/unsupported charset

**Substitute the Unicode replacement character (U+FFFD) for undecodable
bytes; never crash.**

`go-message/charset` (imported for its side effect in `parse.go`) resolves
most declared charsets — including all common legacy ones (Windows-125x,
ISO-8859-*, Shift-JIS, GBK, KOI8-*, ...) — to UTF-8 automatically. When a
part declares a charset that library doesn't recognize, the bytes are left
undecoded; `internal/mime` then passes ASCII bytes through unchanged and
replaces every non-ASCII byte with U+FFFD. The text is never dropped and
the parser never errors out — see `testdata/corpus/unknown_charset.eml`.

### 3. Unencoded 8-bit bytes in headers or body

**Decode as UTF-8 if valid, else fall back to a permissive Windows-1252
interpretation; never error out.**

This is a different scenario from #2: no charset was necessarily declared
wrong — the content simply isn't valid under the charset it claims (or
under the RFC 5322 default of 7-bit ASCII). For any text we extract
(header values via `Header.Get`/`Header.Subject`/address display names,
and `text/plain`/`text/html` bodies declared `us-ascii`/`utf-8`/undeclared),
Mailroom checks `utf8.Valid` first — mislabeled UTF-8 is common — and only
if that fails re-interprets the raw bytes as Windows-1252 (a full 256-byte
charmap, so this step can never itself fail). This is deliberately more
forgiving than rule #2's outright replacement, because there was no
charset label actively lying to us. See
`testdata/corpus/unencoded_8bit.eml` for the case that most concretely
shows the difference from rule #2: the same non-ASCII byte becomes a real
character here, but U+FFFD in the unknown-charset fixture.

### 4. Nested `message/rfc822`

**Recurse, but bound recursion depth and total part count / cumulative
size; on limit, stop and mark truncated rather than crash or OOM.**

`internal/mime` recurses into `message/rfc822` (and `message/global`)
parts, producing a `Message.Nested` tree. Three limits (`internal/mime/
limits.go`) apply across the whole tree, shared with ordinary multipart
nesting since either can be used to build a nesting bomb:

| Limit | Value | Guards against |
|---|---|---|
| `MaxDepth` | 20 | A `message/rfc822`-in-`message/rfc822` (or multipart-in-multipart) chain deep enough to blow the stack or never finish. |
| `MaxParts` | 2000 | A message with an enormous number of sibling parts/nested messages. |
| `MaxTotalSize` | 64 MiB | Cumulative decoded bytes across every part in the tree, so no single message can force unbounded memory use regardless of how the bytes are spread across parts. |

Hitting any limit stops descending further, sets `Message.Truncated = true`
on every ancestor in the affected branch (the flag propagates up through
`Nested`, so the top-level `Message.Truncated` is true if *anything* in
its tree was truncated), and appends an explanatory string to
`Message.Warnings`. This is never a fatal error — the caller always gets a
usable, if incomplete, `Message`. See
`testdata/corpus/oversized_nesting_bomb.eml` (25 levels of
`message/rfc822`, deliberately a small file that exceeds `MaxDepth`) and
`testdata/corpus/nested_rfc822.eml` (3 levels, well inside the cap, to
prove ordinary forwarded-mail nesting round-trips cleanly).

### 5. Transfer-encoding and charset decoding for extraction

Quoted-printable and base64 `Content-Transfer-Encoding` are always decoded
before a part's bytes are exposed in `Part.Data`/`Message.Text`/
`Message.HTML`/`Attachment.Data` — callers never see wire-format bytes.
Charset decoding (rules #2/#3) is applied on top, for `text/*` parts only,
per RFC 2046 §4.1.2. See `testdata/corpus/quoted_printable_and_base64.eml`.

## What "never panics" means in practice

`Parse`/`ParseBytes` recover from any panic in the underlying library (none
are currently known, but the library was not written against an
adversarial threat model, so this is not assumed) and convert it to
`Message.Truncated = true` plus a `Warnings` entry — or, if the panic
happens before any `Message` exists yet, to a non-nil error. `internal/mime/
safe.go` documents every point where control passes into the underlying
parser and is wrapped this way.

On fatal structural failure — currently only when the header block itself
cannot be split into `Key: Value` lines at all (e.g. empty input, or a
line with no colon before the first blank line) — `ParseBytes` still
returns a non-nil, best-effort `Message` (`Truncated = true`, a `Warnings`
entry explaining why) **alongside** a non-nil `error`. Callers that only
want best-effort extraction can ignore the error; callers that want strict
validation should check it. See `testdata/corpus/truncated_garbage.eml`
and `testdata/corpus/missing_headers.eml` (the latter is *not* fatal —
an empty-but-well-formed header block is valid).

## EAI / SMTPUTF8 — PENDING (spec §18.1)

`internal/mime.AddressRequiresEAI` is a **detection-only** stub: it flags
an address (`mime.Address.EAI`) whose local-part or domain contains
non-ASCII bytes, meaning it would need SMTPUTF8 (RFC 6531) to send. It does
not implement EAI (no SMTPUTF8 negotiation, no A-label/U-label
conversion). Whether Migadu supports SMTPUTF8 is an open question (spec
§18.1); until it's answered, compose-side behavior for `EAI == true`
addresses is undecided — spec §18.1 says the fallback is "compose must
produce a clear error", but that enforcement belongs to the compose/
outbound code path, not to parsing, and is not implemented here.

## Golden corpus

`internal/mime/testdata/corpus/*.eml` are self-contained, synthetic
fixtures authored for this project (not a downloaded external corpus), all
using `example.com` addresses and no real personal data. `corpus_test.go`
runs every fixture through two checks: a blanket no-panic/non-nil-Message
smoke test (`TestCorpusNoPanic`), and a table of specific expected
extractions per file (`TestCorpusExtractions`), which also fails if a new
fixture is added without a matching table entry. Add new hostile-input
regressions here first.
