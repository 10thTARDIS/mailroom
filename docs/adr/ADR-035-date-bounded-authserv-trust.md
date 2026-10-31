# ADR-035: An authserv-id may be trusted only for mail received before a date

**Status:** Accepted (Forgejo #766; amends the `MAILROOM_AUTHSERV_IDS` rule of
#414 and #625)

## Context

`MAILROOM_AUTHSERV_IDS` names the RFC 8601 authserv-ids whose
`Authentication-Results` headers Mailroom believes. An instance trusted one id,
the provider's current receiving host. `mailroom-admin auth-report --authserv`
(v1.90.1, read-only) groups messages by topmost authserv-id:

| Topmost authserv-id, or none | Note |
|---|---|
| the provider's old receiving host | the bulk of older mail |
| the provider's current receiving host | the only id after the changeover |
| no Authentication-Results at all | likely Sent copies other clients appended |
| the provider's sending relays | not trusted |

The provider changed the id it stamps. The consequences:

- The reading pane said "unauthenticated" on most older messages. "Unauthenticated" was the reading of "no header bearing a trusted
  id", which is true of every message the provider authenticated under its
  old id. A notice on most mail teaches the reader to ignore it.
- Learned placement and calendar verification read only a trusted header, so
  they failed closed on all of those messages.

## Decision

### 1. Date-bounded trust

An entry of `MAILROOM_AUTHSERV_IDS` is `id` or `id<YYYY-MM-DD`. The bounded
form is trusted only for a message **received strictly before 00:00 UTC on
that date**.

- One resolver, `ml.TrustedAt(entries, received)`, turns the entries into the
  plain ids trusted for one message. Every consumer calls it immediately
  before its trust check: the `authResults` property, the sender warnings
  (through the same parse), calendar verification (`Email/replyToInvite` and
  ingest), learned placement (ingest and the backfill), and the admin
  reports.
- `received` is `messages.received_at`, the INTERNALDATE the provider sets at
  delivery. It is **never the Date header**, which the sender writes: a
  forger would back-date the message to fall inside the bound.
- A malformed bound (bad date, empty id, more than one `<`) is a startup
  error in the server and in every admin command that reads the setting. A
  silent drop would narrow the trusted set without telling the operator.

### 2. The notice is decided by trusted headers and where the message sits

The `authResults` property is, in order:

1. no id configured: `null` (unchanged);
2. a header bearing a trusted id exists: today's object (unchanged);
3. locally authored, or live in a Sent or Drafts mailbox: `null`;
4. otherwise `present: false` ("unauthenticated").

**Untrusted headers are not an input.** Mail whose only headers come from
untrusted ids reads "unauthenticated", exactly like mail with none. The
mailbox roles are read once per `Email/get`, only when `authResults` is
requested on a deployment that names an id. `selfSpoof` is unchanged.

### 3. Report first

`auth-report --authserv` prints, per authserv-id, the first and last UTC date
it was topmost, and the notice counts under the configured rule in
mutually exclusive categories (trusted, unauthenticated, self, no id
configured), a report-only count of messages that carry untrusted
headers but no trusted one, and a split of the header-less mail (locally
authored, Sent or Drafts, From one of the user's identities, other inbound). The bound is chosen from those dates, not from the monthly
table. Category definitions are shared code (`ml.AuthNoticeFor`), so the
report is computed by the rule it reports on.

## The security argument

RFC 8601 §5 has a receiving MTA remove only headers bearing **its own** current
authserv-id. Before the provider's change, headers naming the old id were
stripped on arrival, so any such header was the provider's. After the change
they are **not** stripped: a sender can write
`Authentication-Results: <old id>; dmarc=pass header.from=bank.example` and it
reaches Mailroom untouched.

Trusting the old id without a bound would therefore let a forger pass DMARC
for learned placement, verify a forged calendar cancellation, and suppress the
`selfSpoof` warning, for every message received from now on. The bound closes
that: the old id counts only for mail received while it was still the id the
provider stripped.

- **Received time, not Date.** The bound is checked against INTERNALDATE,
  which the sender cannot set.
- **Fail closed, twice.** A zero received time trusts no bounded entry. And a
  raw bounded entry that reaches a trust check without going through
  `TrustedAt` can never match a header: `trustedSet` drops any entry
  containing `<`. A caller that forgets to resolve trusts less, never more.
  A test feeds a raw bounded entry and a header bearing the bare id and
  asserts the result is untrusted.
- **From is not a self signal.** An earlier design also treated mail From one
  of the user's identity addresses as the user's own. A spoofer writes the
  user's address in From, so that rule would have switched off the
  "unauthenticated" notice for exactly the forgery it exists to flag. Self
  uses only facts the sender cannot set: where Mailroom or the provider put the
  message. `auth-report --authserv` still counts mail From one of the user's
  identities that is not in Sent or Drafts, report-only, so the cost of this
  choice is visible.
- **Residual window.** Mail the provider delivered under the old id but
  Mailroom received later keeps its INTERNALDATE, so it is covered. Mail
  received under the old id on the bound day itself is outside the bound
  (the date is exclusive); the bound is picked as the day after the
  last topmost appearance, which the report prints.
- **Archive rows are never judged against a bound.** `IngestArchive` takes
  `received_at` from the archive, or from the sender's Date header, or from
  now, so for such a row the stored value can be sender-controlled. Every
  consumer therefore passes the zero time (which trusts no bounded entry)
  unless the message is *provider-dated*: `local_mailbox_id IS NULL` **and** a
  provenance row exists (`store.providerDatedExpr`, used by
  `UserTx.ProviderDated`, the report refs and the placement backfill, so they
  cannot disagree; ingest passes zero for any archive row). Archive rows are
  created with `local_mailbox_id` set, and nothing clears it when IMAP
  provenance attaches later, so an archive-first message that is subsequently
  synced (keeping its archive-era `received_at`, since the earliest date wins)
  stays untrusted. A provenance row alone would have dated it. There is no
  column recording where `received_at` came from, so this is the closest marker
  that needs no migration. It is conservative: an archive row with a genuine
  delivery date gets no bounded trust, and neither does a locally authored
  message (also `local_mailbox_id` set), which is harmless because the notice
  treats it as self. A test imports a forged message from an archive, then
  ingests the same bytes over IMAP, and asserts it is not provider-dated.

### Two trusted ids on one message (v1.90.4)

Bounded trust makes it routine for two ids to be trusted at once, and real mail
from the changeover carries both: the new id is topmost from the changeover
and the old one topmost until it. On a message the new
id stamped, a header with the OLD id below it is not the provider's word on
that delivery. The provider no longer strips it (§5 strips only its current
id), so a sender can write one. `ml.TrustedAuthResults`, which feeds the notice
and the `selfSpoof` suppression, therefore keeps only the headers bearing the
SAME id as the topmost trusted header (a provider may split its verdicts over
several headers with its own id). Learned placement and calendar verification
already read only the single topmost trusted header. Without this, the bound
would have had to end at the new id's first appearance, giving up the old id's
genuine verdicts on the overlap.

## Consequences

- The value becomes `current.example,old.example<DATE`. The date
  is set in the deployment configuration from the report, not in
  this repository.
- Older messages regain a verdict, so learned placement and
  calendar verification work on them. Learned placement's backfill reports
  what it would change before `--apply`.
- Mail from an untrusted id keeps reading "unauthenticated". With the bound
  set, that is mostly relay mail and the unbounded gap after the bound date.
- `LearnedSender`, `indexCalendarInvite`, `bodyArgs` and the admin
  aggregates take the received time. A new consumer of the trust list must
  too.

## Alternatives considered

- **Trust the old id without a bound.** Simplest, and rejected for the
  forgery above. It would also make the setting's meaning depend on what a
  provider happens to strip.
- **Silence the notice when headers exist but none is trusted.** This was
  half of an earlier decision, and it was built. It was
  withdrawn after the security review: an Authentication-Results
  header is attacker-writable (RFC 8601 §5), so a header the sender adds could
  turn the "unauthenticated" warning off for their own mail. A warning that an
  attacker can suppress with one line is not a warning. The date-bounded trust
  of decision 1 carries the load instead: it gives the old-provider mail its
  verdict back, which is the case the silence was for. The remaining
  untrusted-only mail (relays, an unconfigured provider) reads
  "unauthenticated", which is accurate, since nothing we believe vouched for it.
- **Treat a From of the user's own address as self.** See the security
  section: the sender writes From.
- **Show a softer "unverified" notice for untrusted-only mail.** Still a
  different word for a state the reader cannot act on, and it would need the
  same untrusted-header input.
- **A bound in the database, set per account.** The id and its change date are
  facts about a provider and a deployment, not about a user; the environment
  variable already holds the id.
