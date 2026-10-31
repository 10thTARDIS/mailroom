# ADR-042: Agent access — API keys, bearer authentication, deny-by-default scopes, limits, anomalies and approvals

**Status:** Accepted (Forgejo #49, v1.95 server increment "K"; spec §13a;
builds on ADR-009 for soft deletes). The MCP tools (increment M) and the
Settings UI (increment U) build on this; neither may widen it.

## Context

Spec §13a asks for scoped API keys with hardened defaults: no send and delete
together without deliberate intent, agent deletes always soft, mandatory
per-key rate limits and budgets drawing from the outbound ledger, anomaly
alerting, and an optional human-in-the-loop confirm mode for the destructive
scopes. `app.api_keys` existed since the baseline and nothing used it.

Decisions on #49: every ability in v1.95 with the
safety defaults (send and delete "ask me first", a typed confirmation for
send+delete), a pending-approvals banner with push, unanswered requests denied
after 24 hours, and **no lock-in**: standard MCP and a standard
`Authorization: Bearer` header, nothing that assumes a particular client or
model. The two first users (a desktop chat client, a local LLM tool) are
examples, not targets.

## Decision

### 1. Keys

- **Token:** `mr_` + 52 characters of lowercase base32 (32 bytes from
  crypto/rand, 256 bits). The prefix lets secret scanners recognise a leaked
  key; base32 needs no escaping anywhere. Shown **once**, in ApiKey/set's
  `created` entry, and never stored, logged or returned again.
- **Stored:** SHA-256 of the whole token (`token_hash`, CHECK 32 bytes) and a
  7-character hint (35 of 256 bits) so a list can tell keys apart. A slow
  password hash buys nothing for a uniformly random input.
- **Settings:** name; scopes from the closed set (`mail:read`, `mail:send`,
  `mail:draft`, `mail:move`, `mail:delete`, `labels:read`, `labels:write`,
  `search:read`, `contacts:read`, `contacts:write`; unknown refused, also by a
  CHECK); `accountIds` (null = every account); `confirmScopes` ⊆ {send,
  delete}, defaulting to the destructive scopes granted (an update that newly
  grants one starts it in confirm mode); `ratePerMinute` (default 60, 1–600);
  `dailyBudget` (default 500, 1–20000); `suspendOnAnomaly` (default on; since
  the suspend-on-anomaly decision (#49) it applies to every anomaly rule but off hours, §6, and migration
  00076 turned it on for every existing key).
  `confirm_scopes text[]` supersedes the never-read `confirm_mode` boolean,
  which stays until a contract step (ADR-008).
- **Send + delete together** needs `acknowledgeSendAndDelete: true` in the same
  request — on create, and on any update that newly grants both. The UI makes
  it a typed confirmation.
- **Label restriction is not implemented:** a key with `labelIds` is refused
  with a clear error, so no key holds one. Account restriction is complete
  (§4).
- Destroy **revokes** (permanent); the row and its audit stay. Suspension is
  reversible (`suspended: false`).
- Key management is session-only (§3). Lifecycle events go to `app.audit_log`
  (`api_key.create/update/revoke`, `api_key.pending.approve/deny`), with
  settings and never the token.
- None of the three tables is a JMAP-synced object (like push subscriptions):
  no `/changes`, no change rows. U polls ApiKey/get and PendingAction/get.

### 2. Bearer authentication

- `internal/auth.BearerGate` sits in front of **both** auth paths on the API
  routes only (`cmd/mailroom.mountApp`). A request without `Authorization` goes
  to the session gate unchanged; one with it is decided by the bearer gate and
  never reaches the session gate. Browser routes never accept a key.
- The key is resolved under SystemTx by `app.api_key_for_token(hash)`, a
  SECURITY DEFINER returning only `(user_id, key_id)` for a key neither revoked
  nor suspended whose user can sign in. Its owner policy repeats the same
  predicate, so the two are defence in depth. Everything else about the key is
  then read under the user's own UserTx: RLS is exactly the browser's.
- **Every refusal is the same 401** — malformed, unknown, revoked, suspended —
  same body (`unauthorized`), same `WWW-Authenticate: Bearer realm="mailroom"`,
  and the database lookup runs even for a malformed token so timing does not
  separate them.
- **Cookie + bearer is refused** (the same 401). Either reading could be the
  wrong one: taking the cookie lets a key's caller borrow a browser's whole
  authority; taking the bearer attributes to an agent what a person's browser
  sent. Refusing both cannot be wrong, and no legitimate client sends both.
- **CSRF does not apply** to bearer requests: browsers never attach an
  Authorization header by themselves and a cross-origin page cannot add one
  without a CORS preflight this server does not grant.
- **Failed attempts are limited per client address** (20 at once, one more
  every 6 s), and the limiter is consulted and charged **only after a token
  has failed to resolve** (amended after review): a live key is never refused
  by it. Checked first, as this ADR originally had it, it was an
  unauthenticated denial of service — behind a reverse proxy every request has
  the proxy's address, so twenty bogus requests locked every agent out. The
  cost is that every presented token is looked up; one indexed lookup per
  request is the cookie path's order of cost. A failed attempt from an
  exhausted address gets 429 with Retry-After instead of 401. Nothing here
  knows a trusted proxy (as `internal/keypub` documents); honouring a
  configured client-address header is a possible follow-up, deliberately not
  added in this release.
- A key reaches only `POST /jmap`, `GET /jmap/session` and
  `GET /.well-known/jmap` (and `/mcp`, added by ADR-044, which runs every tool
  call through the same dispatcher). Downloads, uploads, the image proxy and the event
  stream answer 403 to a key in this release (their account checks are not
  key-aware).
- **Fail closed twice:** the API's HTTP gate loads the key's policy into the
  request context (refusing a key that is now revoked or suspended), and the
  dispatcher refuses any call whose context names a key but carries no policy.
  The branch's history never had the bearer mount without one of them.

### 3. Scopes: one table, deny by default

`internal/api/agent_scopes.go` gives every JMAP method one line: the scopes it
needs (or a function computing them from the arguments), whether it honours the
account restriction, or a refusal — `sessionOnly` (by design: key management,
private-key operations, identities, accounts and credentials, anything that
sends outside EmailSubmission/set, folder deletion, rules, setup, quarantine
release, push devices, approvals) or `notForKeys` (not opened in this release).
**A method missing from the table is refused**, and
`TestEveryMethodHasAKeyPolicy` walks the dispatcher's whole method list and
fails on a method without a line — the guard against a new method silently
becoming key-reachable.

Reachable in v1.95: Core/echo; Email/get, /query (+`search:read` for a
full-text condition), /changes; SearchSnippet/get (`mail:read`+`search:read`);
Thread/get, /changes; Mailbox/get, /query, /changes; Identity/get, /changes;
EmailSubmission/get, /query, /changes (`mail:read`), /getSendQuota
(`mail:send`); Label/get, /query, /changes (`labels:read`), /set
(`labels:write`); Contact/get, /query, /changes (`contacts:read`), /set
(`contacts:write`: `isSaved`, `userName`, `notes` only — `isBlocked`,
`blockDestination` and `isVip` are refused to keys, third review: a block to
Trash deletes every future arrival from a sender with no `mail:delete`, no
confirmation and no anomaly, unblocking undoes the user's own blocks, and VIP
lets a sender through quiet hours; `userName` stays editable because it is
shown only on the contacts pages, beside the address when it could mislead,
and never in the list, the reader or the desk; compose autocomplete MATCHES on
it without showing it — `Contact/query`'s `text` filter and its `name` sort
use it — so a key can change which contact a typed name finds first, but not
what the person sees beside the address they pick); Email/set and
EmailSubmission/set (computed);
PendingAction/get (own requests only); Area/get (`mail:read`, open to a
restricted key: areas are the user's arrangement of the desk, like labels;
§3a).

**Email/set** needs every scope its contents need: create → `mail:draft`;
each update is parsed by `parseEmailPatch` — the method's own parser, in a
read-only transaction — and every non-zero field of the resulting
`store.EmailUpdate` maps to a scope (`emailUpdateFieldScopes`: flags, keywords,
labels, pin, snooze, moves and restore → `mail:move`; **into Trash by patch →
`mail:delete`**). A field missing from that map is refused, and
`TestEveryEmailUpdateFieldIsClassified` fails when the struct grows one.
Destroy → `mail:delete`.

**`$draft` is refused to every key, and no caller can make a message a draft**
(amended after review). A draft is what a destroy may erase for good and what a
send sends, so `keywords/$draft: true` on received mail armed a permanent
delete (the user's next ordinary Delete) and on Sent mail a re-send. Two
controls: `Draft` is in `keyRefusedEmailUpdateFields`, not in the scope map;
and `store.applyEmailUpdate` refuses the false→true transition for sessions too
(`ErrCannotMakeDraft`; nothing in the web client sets it; clearing stays
allowed). And `RetireDraft` — the only hard delete a destroy can reach — now
requires the draft to be effectively in a **Drafts-role mailbox**: Mailroom's
own drafts are placed there (SaveDraft's local mailbox, then the APPENDed
copy), and other clients' drafts sync from that folder, so that is exactly the
set it was designed for; any other draft-flagged message takes the soft
delete to Trash.

**A key's destroy is always the soft delete** (ADR-009): non-drafts already
were; an unsent draft, which a session's destroy retires for good, takes the
Trash path for a key. No key-reachable method hard-deletes: Mailbox/set and
every purge path are session-only. (Amended for #872, ADR-045 §3: a key may
also move its OWN unsent drafts to Trash under `mail:draft` — a create with
`replacesEmailId`, or `discardDrafts` — refused for anything that is not its
own live draft; still the soft path.)

**EmailSubmission/set:** create → `mail:send` (+`mail:move` with
archiveOnSuccess); `onSuccessUpdateEmail` / `onSuccessDestroyEmail` are refused
to keys (the server files the sent copy itself, and a chained destroy would be
a delete no scope check saw). Update and destroy — changing or cancelling the
user's scheduled or Undo-Send submissions — are refused to keys. **A key
submits only a draft it wrote itself:** a key's Email/set creates are recorded
in `app.api_key_drafts`, and createSubmission (and the hold) refuse any other
draft, so a send key cannot mail out what the user or another key has been
writing.

**Decryption is session-only:** Email/get by a key returns an encrypted
message as stored, `decrypted: null` (not attempted). Outbound signing is
unchanged: a key's send is signed exactly like a browser's when the identity
has a bound key, because the signature attests the account sent it, which it
did.

### 3a. Areas, and a key's moves are never the user's evidence (#49)

Decision (#49): a key may file mail into areas, through
this table, the restriction, the budget and the audit like everything else,
and **moves made by a key must not count as the user's filing evidence for
learned placement**. `Area/get` is open under `mail:read`; filing is the
existing Email/set route (`labelIds/<the area's labelId>`, `AddLabelIDs` and
`RemoveLabelIDs` in `emailUpdateFieldScopes`, `mail:move`), so no new
key-callable write exists. `Area/set` (create, rename, delete), the summary,
the desk and `Area/changes` stay closed.

**Provenance.** Email/set sets `store.AgentLabelSource()` on every update and
destroy a key makes, approved runs included; a key's Send & archive records
the key on the submission (`app.submissions.api_key_id`, migration 00076) and
the worker's archive carries the same source. Labels a key applies are
`applied_by = 'agent'`, and its writeback intents say `agent`. Every learner
reads `applied_by = 'user'` as "the user decided this", so they skip a key's
rows without a change of their own. The paths where a person's move becomes
evidence, and what a key's move does instead:

| path | a person's move | a key's move |
|---|---|---|
| learned placement's filings (`placementFilings`: ingest's decision, the backfill, the report's counts and verdicts, the explain's evidence) | an `applied_by = 'user'` row counts | `'agent'` rows are not read |
| label model positives and negatives, rule induction (`HandAppliedLabels`, `LabelTrainingRows`, `RulePreviewCandidates.UserLabelIDs`) | `'user'` rows | not read |
| open label questions (`ClearLabelSuggestions`) | answered by the user's label | left open |
| a learned label taken off (`recordLearnedCorrection`) | **corrected**: blocks the sender's key, counts against the area's gate | **withdrawn** (new outcome): neither corrected nor kept, never ages into kept (an open placement would, `effectiveOutcomeSQL`), counted apart in the report |
| the user takes off an area label a key re-added after withdrawing the learned one (`recordLearnedCorrectionOfAgentLabel`) | **corrected**, as for the learned label itself, while the placement is open or withdrawn and inside the same window | (a key's removal of its own row: nothing) |
| a learned placement archived (`keepLearnedOnArchive`) | **kept** by archive | left open (it ages like any the user has not looked at) |
| a rule's label taken off (`recordRuleCorrection`) | a correction the rule can be narrowed by | none |
| the explain's arrival state (`arrivalAreaState`) | a user label is post-arrival | an agent label is post-arrival too, never "a rule placed it" |

**A key cannot take off a filing the user made by hand.** Deleting the user's
evidence is training by subtraction: re-filing the hand-filed half of a
sender's mail would flip the majority with no key row ever counted. So an
update by a key that would remove an area's own label `applied_by = 'user'`
is refused before any write (`store.UserFilingProtectedError`; Email/set's
SetError `forbidden` names the `labelIds/<id>`), the same rule a rule's area
filing follows (`area_filing.go`: a filing on the user's behalf never removes
the user's own). A key may take off what an automation put there (desk, setup,
learned, rule, another key). **Residual:** a key with `mail:delete` can still
soft-delete a hand-filed message, and trashed mail is not evidence; that is a
delete (confirm mode by default, the mass-delete rule, restorable for 30
days), not a re-file. Topic labels (not an area's own) are not protected: a
key's `remove_label` on a label the user applied removes label-model
evidence, as it did before K2.

**A key's re-add does not hide the user's correction** (K2 review). A key
that takes a learned label off and puts it back leaves an `'agent'` row where
the `'learned'` one stood; the user's later removal of that label is matched to
the message's learned placement by the area's label and recorded as the
correction it would have been. The rule-label analogue (a key removes a rule's
label and re-adds it, then the user removes it) is **not** recorded: the
delete takes `applied_by_rule_id` with it and nothing else keeps which rule
had applied the label, so there is no rule to attribute the correction to.
Closing it would need a schema change (a tombstone of key-removed rule labels,
or carrying the rule id onto the key's re-added row); the cost of the gap is
one uncounted correction per such message, never a wrong one.

**`Area/get` may write.** `GetAreas` (and `ListAreas`) materialise the user's
default areas on first use (`materialiseDefaultAreas`, `internal/store/areas.go`),
a write that appends change_log rows, so a key's `Area/get` can be the call that
creates them. This was already reachable by a key before K2, through
Email/query's `inArea` (`checkAreaExists` reads `GetAreas`). It is harmless: the
result is the same default plan the user's first browser session would
materialise. It is recorded so nobody is surprised by a read-scoped key's
change rows.

**Not covered here:** the priority model's feedback from Email/set (archive of
unread mail, trash, flag) and from a send (reply) is recorded for a key's call
as for a person's. `Triage/open` is session-only for the same reason; extending
that to these signals is a follow-up, not part of the areas decision.

### 4. The account restriction

Enforced inside the methods' own paths, never by checking ids passed in:
`store.EmailQuery.RestrictAccounts` in the one WHERE every Email/query path
builds (search candidates included); Email/get treats other accounts' mail as
notFound; Email/set refuses updates and destroys outside the accounts and moves
into another account's mailbox, and a create must be From one of the accounts
and attach nothing (a blob id carries no account); EmailSubmission/set checks
the account the verified From resolves to; Mailbox and Identity reads list only
the accounts' objects. One predicate (`store.keyAccountsSQL`) serves both the
query and the id checks: where the message IS decides — its canonical
`moved_mailbox_id` when the user moved it (amended after review: provenance
still names the old account until writeback, so a key restricted to that
account kept seeing mail moved out of it), else a live provenance row in one
of the accounts, or a local-only draft shown in one of their mailboxes. A
restricted key's Session resource names none of the user's addresses (the
login address may belong to an account the key is not allowed); the key's
name stands in for it. A restricted key is refused
every method not marked as honouring the restriction (all /changes, threads,
contacts, label writes). A message that also lives in another account is in
the key's account and is shown; acting on it acts on every copy, as in the
browser.

### 5. Limits

Counted at **admission**, inside one transaction holding the key's row lock,
which also writes the audit row — so the check and the spend cannot be split by
a concurrent call. A `/set` costs 1 per object (create + update + destroy),
so one call cannot destroy a thousand messages for the price of one. A read
costs one unit per `ReadUnitObjects` (50) objects (amended after review, so the
budget bounds the data a key moves, spec §13a): a /get `ceil(ids/50)`, a
/query `ceil(limit/50)`, a /changes `ceil(maxChanges/50)`. **Every** /query a
key can call (Email, Mailbox, Label, Contact, EmailSubmission; second review
for Email/query, #850 for the rest) always runs with a limit: 50
(`KeyQueryDefaultLimit`) when it sends none, at most 500 (`KeyQueryMaxLimit`;
Contact/query's own 100), reported back as the response's `limit` whenever it
differs from the request (RFC 8620 §5.5), and charged for that limit before it
runs; Mailbox/query and Label/query honour `position` and `limit` for keys.
Every /changes a key can call likewise runs with `maxChanges` 50 when it sends
none and at most 500, charged up front, with `hasMoreChanges` and an
intermediate `newState` carrying the rest (RFC 8620 §5.2; #852). The charge for
a /query or /changes is taken from that bound before any other argument is
read, and `TestEveryKeyQueryIsBounded` holds every key-reachable /query and
/changes in the scope table to it. Any
other read whose arguments do not state its size (ids null) is charged 1 up
front and topped up from the size of its answer, as a separate audit row —
the call that crosses the budget completes, the next is refused. A
`Thread/get` is topped up by the email ids its threads list, not per thread. The rate is the sum over the last minute, the budget over the
last 24 hours (rolling). The budget counts admitted calls only (ok, pending,
approved); the per-minute rate also counts refusals, each at cost 1 (third
review). Each refusal carries a `reason` from a fixed server list. Refusals are
written in full to `api_key_events` up to two per-minute caps: `rate_per_min`
ROUTINE ones (scope, arguments, a method not opened, rate, budget), and
5 × `rate_per_min` SECURITY-MEANINGFUL ones (`store.CriticalRefusalReasons`:
account-restriction violations, session-only methods, approval misuse, the
mass-delete and mass-send rules). Past its cap a refusal is counted in
`app.api_key_suppressed_refusals`, one row per (minute, outcome, method,
reason), where the method is the name only if this server has it and
`unknown` otherwise, so no client text keys a row; ApiKey/getEvents returns
those counts as `suppressedRefusals`. The first critical refusal past its cap
records a `critical_refusals` anomaly and pushes an alert, at most once per key
per hour, or, for a key with `suspendOnAnomaly` (the default; #49),
suspends the key and records and pushes it every time (§6). A refused system
or reserved keyword is counted on the admitted Email/set's own row
(`refusedKeywords`). Refusals are written under the key's row lock.
Exceeding them is a method error, `rateLimit` or `overQuota`, plus an audit
row. A key's send is an ordinary submission: the send worker draws it from the
outbound ledger like every other send.

### 6. Audit and anomalies

`app.api_key_events` (user_id-led, FORCE RLS): one row per call, admitted or
refused, per anomaly and per approval decision — key, time, method, outcome
(`ok, denied, rate_limited, over_budget, pending, approved, denied_by_user,
expired, anomaly`), cost/deletes/sends as columns (the limits and rules query
them), a summary of counts and ids (never content), client address, pending id,
error type, anomaly rule. Written once; only `error_type` may be updated
(column-level grant), recording the outcome of a call admitted before it ran.

Rules (named constants in `agent_gate.go`):

- **Mass delete:** more than 50 soft deletes in 10 minutes. The call that would
  cross it is refused, an anomaly is recorded and pushed, and the key is
  suspended when `suspendOnAnomaly` (default on). The bound is on the damage.
- **Mass send:** more than 10 sends in 10 minutes: refused, recorded, pushed,
  and suspended when `suspendOnAnomaly` (#49; it was alert-only before).
- **Critical refusals:** the first security-meaningful refusal past its
  per-minute cap (§5): recorded, pushed, and suspended when `suspendOnAnomaly`
  (#49).
- The rules refuse **every** call that would cross them, but a key with
  `suspendOnAnomaly` off is recorded and pushed at most once per key, rule and
  `MassAnomalyAlertCooldown` (one hour; #852; critical refusals:
  `CriticalRefusalAlertCooldown`, also an hour), so a key looping past a
  threshold cannot flood the user's devices with urgent alerts. A key the rule
  suspends is recorded and pushed every time, since it can only recur after a
  person reactivated it.
- **Suspension** (#49) is the same for every rule: `store.SuspendAPIKey`
  sets `suspended_at` and the reason and writes `api_key.suspend` to
  `app.audit_log` (actor `system`, with the rule) in the transaction that
  refuses the call, reporting whether this call suspended it so a stale copy
  of the key cannot suspend, audit or alert twice. The anomaly row carries
  `suspended: true`; the alert's title says the key was suspended and its body
  how to reactivate it; the refusal says so; every later call in the same
  request is refused as suspended at once; and the next request gets the
  bearer gate's uniform 401 (§2 is unchanged: a suspended key's 401 does not
  differ from an unknown token's). **Reactivation** is `ApiKey/set`
  `{suspended: false}` — session-only like all of ApiKey/set — and writes
  `api_key.reactivate` (actor `user`, with the suspension it ended) besides
  the usual `api_key.update`.
- **Off hours** stays alert-only: an overnight assistant is legitimate, so it
  is not a misuse signal and does not suspend.
- **Off hours:** a write during the user's quiet hours if they are on, else
  00:00–06:00 in their quiet-hours time zone (UTC if none): recorded and pushed
  once per 6 hours, not refused — an overnight assistant is legitimate, an
  unnoticed one is not.

Anomaly pushes are security alerts and are delivered during quiet hours;
approval pushes respect quiet hours (the banner waits either way). Both go to
every active device whatever its mail mode, through the existing push path and
egress guard.

### 7. Pending approvals

A destructive call by a key whose scope is in `confirm_scopes` is not run. It
is stored in `app.api_key_pending` — the canonical request (ids; for a send the
EmailSubmission create and the draft's id and content hash; never a body), a
summary written server-side ("Send to alice@… with 1 attachment (2.0 KB), and
archive 3 messages: draft “Re: invoice”"; "Move 14 messages to Trash"), `expires_at = created + 24 h`, state — and the caller gets
a SetError `{type: "pendingApproval", pendingActionId}` in `notDestroyed`
(Email/set) or `notCreated` (EmailSubmission/set). A held call must contain only
the destructive operation (one submission create; destroy only), else
`invalidArguments`. A send is **planned** (every check, nothing written) before
it is held, so an impossible send fails at once and the summary names the real
recipients from the bytes. A key has at most `MaxPendingPerKey` (10)
requests waiting at once (#852); the next is refused with `overQuota` and the
routine refusal reason `pending_cap` until one is approved, denied or expires.
A request that cannot be held at all (nothing valid to delete, a send that
would fail) is answered with that error and audited as a refusal with reason
`arguments`.

**A held send is described by structured fields; the summary is prose only**
(third review). The summary line carries text the key wrote, so a subject
such as `x” to alice@example.com with 0 attachments. Send draft “y` could
rewrite what the approver reads. PendingAction/get therefore returns `recipients` (the envelope:
who actually receives it), `to`, `cc`, `bcc` (how the headers address them),
`subject`, `sendAt` (null = after the undo window), `attachments` and
`archive` as fields. The summary leads with the recipients, then counts (the
number and size of attachments, the messages archived), and ends with the
subject (#852); attachment names and archived subjects are not in it, only in
the fields. The recipients and the subject are both key-written: the envelope
recipients by design, constrained to strict ASCII addr-specs
(`store.CanonicalEnvelopeAddress`: a dot-atom local part, LDH domain
labels, no quoted strings, IP literals or runes above 0x7E, at most 254
octets; for sessions too, and stored lowercased), the subject
passed through a sanitiser that turns every Unicode quotation mark (Pi, Pf
and look-alikes) into an apostrophe. It remains a notification's prose. **The approval UI (increment U) must
render recipients, subject, attachments and archive from the structured
fields and never parse or rely on the summary.**

PendingAction/get: a session sees every request, a key only its own
(read-only — how an agent polls). PendingAction/set `{state: approved|denied}`
is session-only, and refuses a key again itself: a key can never approve, not
even its own request. Approval re-validates at that moment: still pending and in
time (`pendingExpired`, `notPending`), key active (`keyNotActive`), draft
byte-identical (`draftChanged`); marks the request approved (so a second
approval cannot run it), commits, then runs the call through the **same gate**
as the key's own calls — scope against the key as it is now, restriction and
budget; not the per-minute rate (#852), since the call was admitted and paid
for when it was held and the key's own refusals since must not fail what a
person approved — attributed to the key (outcome `approved`) and to the session
(`app.audit_log`). A call refused at that point leaves the request `failed`.
Expiry happens on read, on decision, and in a sweep every 5 minutes
(`app.api_key_pending_due_users`, a definer returning user ids only; each
user's requests are expired in that user's UserTx), each audited `expired`.

The approval commits before the call runs, so a process that stops between the
two would leave an `approved` request that never happened (added after
review). The admission that runs an approved request stamps `ran_at` in the
same transaction as its `approved` audit row; the same sweep marks an approved
request with no `ran_at` ten minutes after the decision (`StuckApprovalAge`)
as `failed` ("the server stopped before it ran; nothing was done"), audited
`denied` with reason `approvedButNeverRan`.

## Consequences

- MCP (M) is a thin client of this: a tool absent from a key is a method this
  table refuses; M must not add a second enforcement path.
- Opening a method to keys is a reviewed change to one table.
- A label-restricted key, key-aware downloads/uploads/event stream, and live
  pending banners via change rows are follow-ups, not regressions.
- `api_key_events` grows with every agent call; retention is a follow-up.

## References

spec §13a, §5 (outbound ledger) · ADR-008, ADR-009 · Forgejo #49 ·
`internal/auth/bearer.go`, `internal/api/agent_*.go`, `internal/store/api_keys.go`,
`internal/store/label_source.go` (LabelByAgent), migrations 00074, 00075, 00076
