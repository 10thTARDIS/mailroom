# ADR-036: Mailroom verifies DKIM itself, at ingest, for mail with no trusted authentication

**Status:** Accepted (extends ADR-035's trust model, which it
leaves untouched)

## Context

The reading pane says "unauthenticated" on mail whose headers carry no
`Authentication-Results` from a trusted authserv-id (ADR-035). In a measured corpus
(read-only reports `auth-report --authserv --no-ar --dkim`), a large set of
messages were in that state, most of them provider-internal delivery of mail the
user's own services submitted through the provider's SMTP relays. That mail
never passes the inbound MX that stamps the header, so nobody checked its
signature on the way in. It is signed (`DKIM-Signature` on essentially all of them).

Verifying those signatures locally, as measured:

| Measurement | Result |
|---|---|
| Messages with a passing signature | a minority, nearly all aligned with From |
| Signatures rejected as expired (`x=`) | the large majority of the rest |
| Pass by month | recent months pass; older months almost never, because of expiry |
| Calibration against the provider's own `dkim=` on control messages | near-total agreement where both have a verdict |
| Provider fail, local pass | 0 (no false pass) |
| Provider pass, local fail | a few, **unexplained** |

The library checks `x=` against the current clock first and offers no option to
change it. Verifying later fails most of the mail.

## Decision

1. **Mailroom verifies DKIM in-process at IMAP ingest** for a NEW message that
   has no header bearing a trusted authserv-id (`ml.TrustedAt` then
   `ml.TrustedAuthResults(...).Present == false`). The verifier is
   `internal/dkimverify`, the anti-corruption layer around `go-msgauth`.
2. **The verdict is stored**, one row per message, in `app.message_local_auth`
   (migration 00066), written in the transaction that creates the message.
3. **Only an aligned pass is displayed.** The `authResults` property of such a
   message becomes `{present: true, dkim: ["pass"], spf: null, dmarc: null,
   source: "mailroom", dkimDomain: "<d=>"}` and the reading pane says "Verified
   by Mailroom (DKIM, <domain>)", visibly distinct from a provider verdict. An
   aligned pass also counts as an affirmative pass for the `selfSpoof` warning.
   `ml.AuthNoticeFor` stays the single definition of the notice category and
   gains the category `local_dkim`; `auth-report --authserv` counts it from the
   table.
4. **A local fail is never displayed**, and neither is an unaligned pass or any
   other result. They are stored (as evidence, and for measurement).
5. **A provider's verdict always wins.** With a trusted header present the
   check is skipped at ingest, and a local row is ignored at read.
6. `MAILROOM_LOCAL_DKIM`, default on; `off` disables the check entirely.
   `mailroom_local_dkim_total{result}` counts outcomes.

### Why at ingest, not at read time

`x=` expiry. The signers that matter use short windows; a signature verified
minutes after receipt has not expired, and the same signature verified a month
later has. Ingest is the only moment the check is both cheap and meaningful.
There is no backfill: old mail stays "unauthenticated", which is honest, since
nobody checked it when it counted.

### Why only an aligned pass, and never a fail

- **A pass is cryptographic proof** that the holder of the key for `d=` signed
  these headers and this body. It means something about the message only if
  `d=` speaks for the From domain, hence alignment.
- **A fail cannot be shown yet.** The 12 provider-pass, local-fail
  disagreements in the calibration are unexplained. Until they are understood,
  a local "fail" may be our bug, and a warning that is wrong teaches the reader
  to ignore warnings. A pass has no such risk: 0 of the provider's failures
  verified locally.
- Unaligned passes are stored, not displayed: an ESP signing as itself says
  nothing about the From domain.

### Alignment

`d=` aligns with the From domain when it **equals** it or the From domain is a
**subdomain** of it, and `d=` is **not itself a public suffix**
(`golang.org/x/net/publicsuffix`, `EffectiveTLDPlusOne` errors for a suffix).
Both names go through `ml.NormalizeAuthDomain` (lowercase, no trailing dot,
punycode). The Public Suffix List (ICANN and private sections) replaces the
report's last-two-labels approximation: `d=co.uk` or `d=com` cannot speak for
everything beneath them, and `d=blogspot.com` cannot speak for one tenant's
mail. Tests cover exact, subdomain, a TLD, `co.uk`, a private-section suffix,
IDN in both directions, and an unrelated domain. The suffix test
reads the list through `ml.OrgDomain`, the one Public Suffix List helper,
which DMARC relaxed alignment for learned placement (ADR-032 amendment) also
uses. The alignment rule here is unchanged.

### DNS bounds

The check runs **before** the ingest transaction opens, like the malware scan:
DNS latency is never held inside a Postgres transaction. Per lookup 3 seconds,
at most 4 lookups per message, 8 seconds for the whole message. A **fresh
`Verifier` per message** makes the 4-lookup bound structural, and avoids a
shared verifier caching one transient DNS failure as a permanent answer. A
DNS failure, a spent budget or the deadline writes **nothing**: "not checked"
is the honest state, and there is no retry. The metric carries
`skipped_trusted_header` and `not_checked` (and `no_signature`) so the bound is
visible and a total never hides them.

### Storage and the change log

The row is part of the message it describes. It is written in the same
transaction as the message, which appends the Email `created` change row, and
is never updated, so it appends **no change row of its own**: there is no
mutation for one to announce. The write is guarded by the same RLS the message
has (`WHERE EXISTS` over `app.messages`, because a foreign-key check runs
outside RLS), and the table is `FORCE ROW LEVEL SECURITY`. It is in the RLS
inventory suite. A dedup hit onto an existing row, an archive import and a
locally authored message get no row.

## Security: header prepend and header add (RFC 6376 §8.15)

A DKIM signature covers, for each name in `h=`, the **bottom-most** instance of
that header, and nothing the signature does not name. So with ONE genuinely
signed message from a domain, an attacker can:

- **prepend** a second `From:` (or `Subject:`, `Reply-To:`) above the signed
  ones: the signature still verifies, the reader sees the TOP `From`;
- **add** a header the signature never covered, such as a `Reply-To` on a
  message that had none: the signature still verifies and replies go to the
  attacker.

An unguarded check would label either forgery "Verified by Mailroom" and, worse,
use it to silence `selfSpoof`. The guard is **"every displayed identity header
is signed"**, not "no duplicates". A pass is displayable only if, for the
passing signature, with *m* = instances in the message (the raw field count,
`mime.Header.FieldCount`, as for the one-From rule) and *n* = times `h=` lists the name:

- `From`: *m* = 1 and *n* >= 1;
- each of Sender, Reply-To, To, Cc, Subject, Date, Message-ID: *m* = 0, or
  *m* = 1 and *n* >= 1, or *m* >= 2 and *n* > *m*. The last is an OVERSIGNED
  header: `h=` lists it more times than it is present, so one more instance
  would fill the empty slot and break the signature. A header oversigned while
  absent is likewise safe.

When it fails, the row is still stored (result `pass`) with `aligned = false`
and `display_blocked` naming the reason (`duplicate_header:from`,
`unsigned_header:reply-to`, `from_not_signed`). The metric counts them as
`pass_blocked_duplicate` and `pass_blocked_unsigned`, and `auth-report
--authserv` prints each as a report-only row, so what legitimate mail the guard
costs (mail whose signer does not sign `Cc`, say) is visible rather than
guessed. Tests sign a real message and prepend `From`, duplicate `Subject`, and
add `Reply-To` and `Cc`; each is stored, not displayed, and does not suppress
`selfSpoof`. Each was confirmed red with the corresponding rule removed.

Other consequences of the trust model: a forged `Authentication-Results` header
is irrelevant (the check runs only when no *trusted* header exists, and
untrusted ones are not an input, ADR-035); `d=` is the signer's claim, so
alignment, not the signature alone, binds it to the From domain; `l=` signatures
are declined by the verifier.

## Out of scope

- **Learned placement** (ADR-032) needs a DMARC pass; a DKIM pass is not that
  and is not wired in.
- **Calendar "Verified"** keeps reading the trusted parse only.
- **Backfill.** Old mail is not re-checked: keys rotate and `x=` expires.

## Rejected alternatives

- **Verify at read time.** Fails every signature carrying `x=` expiry (most of
  the set), puts DNS on the read path of every message open, and gives a verdict
  that changes day to day.
- **Trust the provider's outbound relays** (a configured `Received:` host).
  Cheaper, but any customer of the provider submits through the same relays, so
  it trusts provider policy for any From domain.
- **Vendor or fork the DKIM library to verify "as of the received time"** so old
  mail can be backfilled. Possible with one added option, but it earns a verdict
  on a key that may have rotated and a fork to maintain, for mail whose
  authenticity nobody is acting on. Revisit if a backfill is wanted.

## Consequences

- One migration (00066), one dependency promoted to a direct import
  (`golang.org/x/net/publicsuffix`), outbound DNS from the app.
- New mail from the user's own services reads "Verified by Mailroom" instead
  of "unauthenticated".
- The few unexplained disagreements remain an open item; showing a fail waits on
  them.

## References

ADR-035; RFC 6376 §8.15; `internal/dkimverify`;
`internal/ingest/localdkim.go`.
