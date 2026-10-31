# internal/sync — go-imap v2 beta.8 spike notes

Findings below come from reading `github.com/emersion/go-imap/v2@v2.0.0-beta.8`
source directly out of the module cache (not vendored — vendoring is
deliberately deferred, see "Dependency pin" below), specifically every
`.go` file under `imapclient/`, `imapserver/` (including `imapmemserver/`),
and the top-level `imap` package.

## CONDSTORE (RFC 7162): confirmed, fully exposed

- `imap.SelectOptions.CondStore` sends `SELECT mailbox (CONDSTORE)`.
- `imap.SelectData.HighestModSeq` is populated from the server's
  `[HIGHESTMODSEQ n]` response code.
- `imap.FetchOptions.ModSeq` requests `FETCH ... MODSEQ`; the per-message
  value comes back as `imapclient.FetchItemDataModSeq` /
  `FetchMessageBuffer.ModSeq`.
- `imap.FetchOptions.ChangedSince` sends the `FETCH ... (CHANGEDSINCE n)`
  modifier.
- `imap.CapSet.Has(imap.CapCondStore)` also returns true when QRESYNC is
  advertised (CONDSTORE is a QRESYNC prerequisite per RFC 7162), so
  capability detection is correct either way.

`SyncSession` wraps all of the above directly: `SelectOptions.CondStore`,
`FetchItems.ModSeq`/`ChangedSince`, `MailboxStatus.HighestModSeq`,
`MessageMeta.ModSeq`.

## QRESYNC (RFC 7162): NOT usable — capability name only

`imap.CapQResync = "QRESYNC"` exists purely as a capability-name constant
for `CAPABILITY` response parsing (`capability.go`). Nothing else in the
library implements QRESYNC:

- `imap.SelectOptions` has no field for the QRESYNC SELECT parameter
  (`(QRESYNC (uidvalidity modseq [known-uids [seq-match-data]]))`,
  RFC 7162 §3.2.5) — there is no way to send it.
- There is no `VANISHED` response handling anywhere in the tree (client or
  server): searching the whole module for `VANISHED` (case-insensitive)
  returns nothing.
- `imap.FetchOptions` has no way to request `UID FETCH ... (VANISHED)`
  (RFC 7162 §3.2.10).
- Consequently `imap.CapSet.Has(imap.CapQResync)` correctly reports whether
  the *server* advertises QRESYNC, but the client cannot actually drive a
  QRESYNC exchange even when it does.

**Path forward** (per spec §4's "verify QRESYNC support in v2 at spike
time"): `SyncSession` implements CONDSTORE-only reconciliation now — the
sync engine diffs UIDs via an open-ended `UIDSetRange(uid, 0)`
against the mailbox's current `UIDNEXT` to find new/vanished messages, and
uses `FetchItems.ChangedSince` (MODSEQ-filtered fetch) to find flag
changes, rather than QRESYNC's single-round-trip vanished-message list.
Contributing QRESYNC support upstream to `emersion/go-imap` is the other
option if the CONDSTORE-only reconcile proves too expensive against real
traffic; recording that as a possible follow-up rather than doing it now.

## Dependency pin

`go.mod`/`go.sum` already pin `github.com/emersion/go-imap/v2 v2.0.0-beta.8`
via cryptographic hash (the go.sum `h1:` line) — that pin is what makes it
safe to build against a pre-stable dependency. A full `go mod vendor` is
**deliberately not done** as part of this change, per the task's
instruction; that stays an orchestrator decision (vendoring a pre-stable
dep trades "immune to upstream force-pushes/yanks" for "vendor tree needs
manual refresh on every bump" — worth deciding once, not per-package).

## Known gap: go.sum is missing an entry needed to actually build this package

`internal/sync` imports `github.com/emersion/go-imap/v2/imapclient`
(required to do any real IMAP I/O — that's the whole point of this seam).
`imapclient`'s `authenticate.go` imports `github.com/emersion/go-sasl` (used
for the `AUTHENTICATE` SASL exchange elsewhere in that package — this
package's `Dial` authenticates with plain `LOGIN` and never calls
`Authenticate`, but Go compiles the whole `imapclient` package as a unit,
so the import is unavoidable simply by using the package).

`go.sum` currently has only:

```
github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6/go.mod h1:iL2twTeMvZnrg54ZoPDNfJaJaqy0xIQFuBdrLsmspwQ=
```

— the `/go.mod` hash (needed for MVS graph resolution) but **not** the full
module content hash (an `h1:` line without the `/go.mod` suffix) needed to
actually download and compile the package.

Verified directly: a throwaway file containing only
`import _ "github.com/emersion/go-imap/v2/imapclient"`, built against this
repo's real `go.mod`/`go.sum` with `GOFLAGS=-mod=readonly`, fails with:

```
missing go.sum entry for module providing package github.com/emersion/go-sasl
(imported by github.com/emersion/go-imap/v2/imapclient); to add:
	go get github.com/emersion/go-imap/v2/imapclient@v2.0.0-beta.8
```

Per this task's constraints, that command has **not** been run and
`go.mod`/`go.sum` have **not** been touched. **Orchestrator decision
needed:** run `go get github.com/emersion/go-imap/v2/imapclient@v2.0.0-beta.8`
(adds an indirect `github.com/emersion/go-sasl` require line plus go.sum
entries only — does not change the go-imap version pin, does not require a
full `go mod tidy`, though running tidy is also fine) before `internal/sync`
will build in this repo.

To verify the code in this change is itself correct despite that gap, it
was compiled and unit-tested against a throwaway copy of `go.mod`/`go.sum`
with that `go get` applied, in a scratch directory outside the repo — the
real repo's `go.mod`/`go.sum` were never modified. See the task report for
the exact commands and results.
