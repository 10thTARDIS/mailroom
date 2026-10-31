# Mailroom's patches to go-imap

This directory is go-imap **v2.0.0-beta.8** (module `github.com/emersion/go-imap/v2`,
MIT licence, see `LICENSE`), copied unmodified from the Go module cache and then
patched as described below. Mailroom builds against it through
`replace github.com/emersion/go-imap/v2 => ./third_party/go-imap` in the
top-level `go.mod` (#3, ADR-047 §5).

**Why vendored in-repo rather than a hosted fork:** a hosted fork would be a
second repository the build depends on, at a URL that must stay reachable and
must not identify the operator (the portability rule). In-repo, the patch is
reviewed in the same pull request as the code that needs it, and the build
needs nothing beyond the module proxy it already uses.

**Upstream status:** to be offered upstream. Drop this directory and the
`replace` line once a go-imap release carries equivalent support.

**Re-applying on a go-imap bump:** copy the new module version over this
directory, keep this file, and re-apply the patch below. Every patched spot is
marked with a `Mailroom patch` comment.

## Patch 1: Gmail X-GM-EXT-1 FETCH items (2026-10-27)

**What:** FETCH can request and parse Gmail's `X-GM-MSGID`, `X-GM-THRID` and
`X-GM-LABELS` (developers.google.com/workspace/gmail/imap/imap-extensions).

**Why:** without it, a FETCH response carrying any X-GM-* item fails the whole
command with `unsupported msg-att name` (`imapclient/fetch.go`), and
`FetchOptions` is a closed struct, so the items cannot even be requested.
Mailroom's Gmail sync model (ADR-047) reads every message's labels from
`X-GM-LABELS`.

**Changes (all additive; no existing behaviour changes):**

- `capability.go`: `CapGmailExt Cap = "X-GM-EXT-1"`.
- `fetch.go`: `FetchOptions` gains `GmailMsgID`, `GmailThrID`, `GmailLabels bool`.
- `imapclient/fetch.go`:
  - `writeFetchItems` writes `X-GM-MSGID`, `X-GM-THRID`, `X-GM-LABELS` when
    requested;
  - new item types `FetchItemDataGmailMsgID`, `FetchItemDataGmailThrID`,
    `FetchItemDataGmailLabels`, and matching `FetchMessageBuffer` fields
    `GmailMsgID`, `GmailThrID`, `GmailLabels`;
  - the msg-att parser reads the two ids as unsigned 64-bit numbers and the
    labels with a new `readGmailLabels`: a backslash atom (`\Inbox`) is a
    system label and keeps its backslash; an astring (atom, quoted or
    literal) is a user label, decoded from modified UTF-7 when it is valid
    modified UTF-7 and kept verbatim otherwise; Gmail's quoted form of a
    system label (`"\\Important"`) unquotes to `\Important`. An empty list
    gives an empty, non-nil slice.
- `imapclient/gmail_test.go`: parser tests against real-shaped responses
  (spaces, escaped quotes, modified UTF-7, a literal, items in any order).

**Not in this patch:** `STORE ±X-GM-LABELS` and `SEARCH X-GM-MSGID`. Those are
patch 3 below.

Run the patch's own tests with `cd third_party/go-imap && go test ./imapclient -run Gmail`.

## Patch 2: FLAGS absent versus empty (2026-10-27)

**What:** `FetchMessageBuffer.Flags` is an empty, non-nil slice when the
server answered `FLAGS ()`, and stays nil when the response carried no FLAGS
item at all (`imapclient/fetch.go`, `populateItemData`).

**Why:** Unpatched, both read as nil, so a message whose FLAGS
never arrived — a response truncated before the item, or a server that sent
FLAGS in a second FETCH response for the same message (go-imap routes that one
to the unilateral handler) — was indistinguishable from a message with no
flags, and Mailroom's flag refresh cleared `\Seen`, `\Flagged` and every
keyword on it. The same absent-versus-empty rule patch 1 gives `GmailLabels`.

**Changes:** two lines in `populateItemData` and a doc comment on the field;
`imapclient/flags_absent_test.go`. No other behaviour changes: SELECT's
FLAGS and PERMANENTFLAGS are parsed elsewhere and are untouched.

Run with `cd third_party/go-imap && go test ./imapclient -run FlagsAbsent`.

## Patch 3: Gmail X-GM-LABELS STORE and X-GM-MSGID SEARCH (2026-10-27)

**What:** STORE can add or remove Gmail labels
(`UID STORE <set> ±X-GM-LABELS[.SILENT] (<labels>)`), and SEARCH can find a
message by its `X-GM-MSGID`.

**Why:** Mailroom's Gmail writeback (ADR-047 §3, increment 5b) archives by
taking off the `\Inbox` label, writes Mailroom labels back as Gmail labels, and
finds a message's current UID by its Gmail id before writing to it.
`StoreFlags` is a closed struct of IMAP flags (its encoder refuses a label with
a space, and it would never modified-UTF-7 encode one), and `SearchCriteria` has
no key for the Gmail id.

**Changes (all additive; no existing behaviour changes):**

- `store.go`: new `StoreGmailLabels{Op, Silent, Labels}`.
- `imapclient/store.go`: new `Client.StoreGmailLabels`, the same command shape
  as `Client.Store` with the item name `X-GM-LABELS`. Its untagged FETCH answer
  (Gmail echoes the labels after the change unless `.SILENT`) is parsed by
  patch 1's `readGmailLabels`, so a caller can confirm what Gmail applied.
- `internal/imapwire/encoder.go`: new `Encoder.GmailLabel`, the write-side
  counterpart of `readGmailLabels`:
  - a system label (`\` followed by an atom, `\Inbox`) is written as an atom;
  - any other label is a user label, encoded in modified UTF-7 (`été` →
    `&AOk-t&AOk-`, `R&D` → `R&-D`) and written as a quoted string, with `"` and
    `\` escaped. Modified UTF-7 output is printable ASCII, so it is quoted
    unless it is longer than a quoted string may be (then a literal);
  - an empty label and a user label beginning with `\` are refused (an encoder
    error, which fails the command): Gmail would read the second as a system
    label, and patch 1's decoder unquotes `"\\Important"` to `\Important`, so
    such a name could not round-trip. Mailroom checks names before sending
    (`sync.ValidGmailLabel`) so a bad name never costs the connection.
- `search.go`: `SearchCriteria` gains `GmailMsgID []uint64`, ANDed by `And`.
- `imapclient/search.go`: `writeSearchKey` writes `X-GM-MSGID <n>` (unsigned
  64-bit decimal) for each.
- `imapclient/gmail_write_test.go`: wire-form tests (op sign, system atoms,
  quoting and escaping, modified UTF-7 for non-ASCII and `&`, `.SILENT`), the
  echo parsed back through patch 1's decoder (a round trip), the refusals, and
  the search key at the full 64-bit range; plus "a command that does not ask
  for the Gmail form does not name it".

Run the Gmail patches' own tests with `cd third_party/go-imap && go test ./imapclient -run Gmail`.
