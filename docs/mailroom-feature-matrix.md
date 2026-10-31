# Mailroom — Feature Matrix (v0.4)
### Every mail-client feature, mapped to a version target

Companion to **mailroom-spec.md** (v0.4). Goal: an exhaustive inventory of features found across mail clients (Gmail, Outlook, Apple Mail, Thunderbird, Superhuman, Spark, Proton, Fastmail, mutt/aerc, etc.), each assigned a Mailroom target.

**v0.4 changelog:** dedup basis corrected to content hash; Bounce/DSN handling added (v1.0); Undo Send + Send later unified on one delayed-submission engine; search-index at-rest encryption and restore drill added as v1.0 requirements; export format defined at v1.0; third-party JMAP clients row now tied to the RFC 8620 conformance decision; 0.5 daily-driver milestone added to the rollup.

**Legend (semver-aligned):**
- **v1.0** — ships in the MVP + intelligence release
- **v1.x** — later feature release within the v1 major
- **v2** — second major
- **v3** — third major
- **—** — not planned (reason noted)

> Targets are intentions, not contracts, and move as we learn. Open work is tracked as GitHub issues and milestones (1.x, 2.x, 3.x); a row's issue number is given where one exists.

---

## 1. Accounts & connectivity

| Feature | Notes | Target |
|---|---|---|
| Multiple accounts | Several IMAP/SMTP logins per user | shipped (v1.0.0) |
| Unified inbox across accounts | Deduped by **content hash** (per user) w/ provenance — spec §4 | shipped (v1.0.0) |
| Per-account settings (colors, signature, sync scope) | | shipped (v1.0.0) |
| IMAP (IMAP4rev1/rev2) | Primary ingest | shipped (v1.0.0) |
| SMTP submission | Primary send. Non-ASCII (EAI/SMTPUTF8, RFC 6531) envelope addresses are refused, not sent — detection-only stub (`internal/mime/eai.go`) | shipped (v1.0.0) |
| CONDSTORE/QRESYNC incremental sync | Efficiency. CONDSTORE only: go-imap v2 has no QRESYNC/VANISHED support, so vanished messages are found by diffing UIDs instead (`internal/sync/doc.go`) | shipped (v1.0.0) |
| IMAP IDLE push | Near-real-time. Watches `INBOX` only — other folders get no push wake-up | shipped (v1.0.0) |
| **Bounce/DSN handling** | **[v0.4 new]** RFC 3464 parse, thread association, delivery-health view — spec §4 | **shipped (v1.0.0)** |
| OAuth2 login to providers | Gmail and Outlook/Microsoft 365 over XOAUTH2 with operator-owned OAuth clients (ADR-046): Connect buttons where the operator configured a provider, an explanation page first, a reconnect banner and one Web Push per transition, writeback and outgoing mail held while a grant needs reconnecting, sign-in health in `Account/getHealth`, an admin notice when a provider refuses the server. Outlook syncs on connect; Gmail sync waits on the row below | **shipped (v1.96.0)** |
| App-password / basic IMAP auth | Migadu path | shipped (v1.0.0) |
| JMAP *upstream* (to a JMAP provider) | We speak JMAP to clients regardless; upstream JMAP is a bonus | v3 — #20 |
| POP3 ingest | Rarely needed; legacy | — |
| Exchange / EWS / MAPI / ActiveSync | Proprietary; out of scope | — |
| Provider profiles (Migadu preset) | Partial: the profile's only effect in code is a poll-interval floor (5 min for `gmail`, none for `migadu`/`generic` — `internal/provider/imap.go`); no Migadu preset of limits, folders, or send-as exists | v1.0 (partial) — #46 |
| Gmail IMAP account (All Mail + X-GM-LABELS, budgeted) | One-time Takeout import ships (`internal/importer/gmail.go`). Live sync over OAuth (ADR-047: All Mail, Spam and Trash downloaded once, Gmail labels as Mailroom labels, a daily download budget, newest-first history) is built and tested but switched off until it is verified against live accounts; label rename and delete are not written back yet (#37) | v1.96.0 (built, switched off) — #3 |
| Outlook.com / Microsoft 365 account | Over OAuth (ADR-046), receive-only with an explanation where the tenant has SMTP AUTH turned off; a FLAGS-only sweep for servers without CONDSTORE or QRESYNC | **shipped (v1.96.0)** |
| Bulk archive import (mbox / maildir / Google Takeout) | One-time, local, resumable — day-one Gmail back-catalog | shipped (v1.0.0) |

## 2. Reading messages

| Feature | Notes | Target |
|---|---|---|
| Conversation threading | Partial: ships as first-match Message-ID/References hint matching, **not** JWZ, with no subject-fallback. A complete, tested JWZ implementation exists (`internal/thread/`) but is deliberately unwired (ADR-030) | v1.0 (partial) — ADR-030 |
| Flat/message list view | | shipped (v1.0.0) |
| HTML rendering (sandboxed) | CSP + iframe | shipped (v1.0.0) |
| Plaintext rendering | | shipped (v1.0.0) |
| Remote-image blocking by default | Privacy | shipped (v1.0.0) |
| Image proxy (opt-in, strips trackers) | On **by default** for the instance (`MAILROOM_IMAGE_PROXY`, default `true`) — not opt-in; remote images still load only after "Show remote content" is chosen | shipped (v1.43.0) |
| Tracking-pixel detection/blocking | | shipped |
| Inline (CID) images | | shipped (v1.0.0) |
| Preview text / snippets | | shipped (v1.0.0) |
| Reading-pane layouts (vertical/horizontal/none) | | shipped |
| View source / raw headers | Partial: raw bytes are reachable via the JMAP blob download (`internal/api/blob.go`), so an API client can fetch source; the web client has no "view source" menu item or raw-headers view | v1.0 (partial) — #43 |
| Print / export message to PDF | Browser print-to-PDF, deliberately, rather than a bespoke renderer | shipped (v1.1.0) |
| Message translation | Needs T2 or ext. service | v2 — #4 |
| Read receipts — request/send | Privacy-sensitive, optional | v2 — #11 |
| Sender authentication display (SPF/DKIM/DMARC) | Trust badges | shipped |
| External-sender / spoofing warnings | ⚠ Ships as SENDER-DOMAIN warnings only. Measured on a real corpus: external-sender marking fires on nearly every message of a catch-all mailbox, and display-name impersonation fires on a few percent with no true positives (it measures ESP delegation), so neither was built. What ships is a domain that visually collides with one you correspond with, a punycode sending domain, and mail claiming your own domain unauthenticated. | shipped |

## 3. Composition

| Feature | Notes | Target |
|---|---|---|
| Rich-text (WYSIWYG) compose | | shipped (v1.0.0) |
| Plaintext compose | | shipped (v1.0.0) |
| Reply / reply-all / forward | | shipped (v1.0.0) |
| Alias-aware From selection | Core differentiator; server-side From validation (spec §5) | shipped (v1.0.0) |
| Recipient autocomplete (from contacts/history) | | shipped (v1.0.0) |
| CC / BCC | | shipped (v1.0.0) |
| Attachments (drag-drop, multiple) | | shipped (v1.0.0) |
| Inline images | | shipped (v1.0.0) |
| Draft autosave (local + optional IMAP) | Conflict rule: LWW, loser preserved (spec §5) | shipped (v1.0.0) |
| **Delayed-submission engine** | **[v0.4]** One mechanism behind Undo Send *and* send-later — durable job (spec §5/§16) | **shipped (v1.0.0)** |
| Undo Send (hold window) | UI over the delayed-submission engine | shipped (v1.0.0) |
| Send later / scheduled send | UI over the same engine | shipped |
| Outbound quota meter | **[v0.4]** Ledger + backpressure vs. provider daily caps (spec §5). Not yet exercised against a real provider's daily caps at production scale | **shipped (v1.0.0)** |
| Per-identity signatures | Bound to an identity; one may be marked default and is inserted automatically (ADR-017) | shipped |
| Templates / canned responses / snippets | Same object as signatures, one table with a kind (ADR-017) | shipped |
| Spell check | Browser-native to start | shipped (v1.0.0) |
| Grammar / style assist | T2 optional | v2 — #4 |
| Smart-reply / smart-compose | T2 optional | v2 — #4 |
| Large-attachment handling (link instead of attach) | | v3 — #20 |
| Markdown compose | Own renderer, documented subset; source stays the text part (hostile-content §2.9) | shipped (v1.15.0) |
| Dictation | Platform-native (native apps) | v2 — #5 |

## 4. Organization

| Feature | Notes | Target |
|---|---|---|
| Labels/tags (multi-label, color, nested) | Hybrid, IMAP-sync default; persisted keyword map + PERMANENTFLAGS probe (spec §6) | shipped (v1.0.0) |
| Folders (IMAP) | | shipped (v1.0.0) |
| Archive / "done" | | shipped (v1.0.0) |
| Star / flag | | shipped (v1.0.0) |
| Star with importance markers | The matrix lists this as its own thing; the code doesn't separate it — one `is_flagged` column, plus a separate learned priority score (§6) | shipped (v1.0.0) |
| Client-side filters/rules (T0) | | shipped (v1.0.0) |
| Server-side Sieve filter editing | `internal/rules/rules.go` only borrows Sieve's *evaluation model* ("Sieve-style" priority order). `internal/sieve` is a **read-only** ManageSieve client (no write verbs) behind `mailroom-admin sieve-report`; nothing writes a script yet | v1.x — #29 |
| Auto-suggested rules ("you always archive X") | | shipped (v1.5.0–v1.11.1) |
| Snooze | Durable jobs; missed-timer catch-up (spec §16) | shipped |
| Mute / ignore thread | | shipped |
| Pin message/thread | | shipped |
| Categories (Primary/Social/Promotions-style) | T0 rules + per-label classification. NOT from T1: that model scores *importance*, which has no notion of promotional vs social. Function-based (`List-Id`, DKIM `d=`, etc.), not sender/topic-based — ADR-021 | shipped (v1.28.0) |
| Bulk / multi-select actions | Requirement | shipped (v1.0.0) |
| Select-all-matching-search | | shipped (v1.0.0) |
| Drag-drop to labels/folders | | shipped |

## 5. Search

| Feature | Notes | Target |
|---|---|---|
| Full-text search (typo-tolerant) | Tantivy via `tantivy-go`, with an in-memory fallback (not the originally-considered Bleve). Typo tolerance covers subject/participants only (n-gram field), deliberately not bodies (spec §3/§7) | shipped (v1.0.0) |
| Field operators (from/to/subject/label/has/is/account/date) | Mailroom-owned query AST | shipped (v1.0.0) |
| Cross-account search | | shipped (v1.0.0) |
| Search highlighting | | shipped (v1.0.0) |
| As-you-type / prefix search | Results update after a short pause in typing, with the word being typed treated as a prefix; an explicit trailing `*` also works | **shipped (v1.88.0)** |
| Reindex-from-store | **[v0.4]** First-class op; index = disposable cache (spec §7) | shipped (v1.0.0) |
| Attachment-content search | Opt-in extraction. Schema column exists (`app.attachments.extracted_text_hash`) but nothing writes it yet | v1.x — #21 |
| Saved searches / smart folders | | shipped (v1.20.0) |
| Semantic ("about X") search | Static embeddings | v2 — #8 |
| Natural-language search | T2 optional | v2 — #4 |
| Search suggestions/history | ⚠ Known bug: suggestions offer `is:unflagged`/`is:snoozed`/`is:pinned`, which the server parser doesn't recognise, so picking one returns no results and no error | shipped |

## 6. Triage & intelligence

| Feature | Notes | Target |
|---|---|---|
| Priority Inbox (learned) | Core; explicit cold-start heuristics (spec §8) | shipped (v1.0.0) |
| Importance scoring w/ online learning | T1. Learning is not per-action: a per-user recurring job retrains (default every 24h) and a new model replaces the active one only if it passes a promotion check | shipped (v1.0.0) |
| Explainable "why important" + correct | Trust | shipped (v1.0.0) |
| Model snapshots / rollback / reset | **[v0.4]** Poisoning + drift recovery (spec §8) | shipped (v1.0.0) |
| Eval harness (replay held-out actions) | **[v0.4]** Built before tuning (spec §8). Its own defects were found and fixed | shipped (v1.0.0) |
| Multi-label auto-suggestion | T1. Two sources: induced-rule suggestions and per-label models, each gated by a promotion check; a model suggestion never overwrites a rule one | shipped (v1.34.0) |
| Adjustable priority threshold | "The line" — a per-user threshold (0–1) on the priority score, adjustable in 0.05 steps with a preview and explicit Apply | shipped |
| Deterministic rules (T0) | | shipped (v1.0.0) |
| Needs-reply / follow-up nudges | The desk's Follow up strip. "No one answered your mail" (to people, no human answer in 7 days) is on by default; "you haven't replied to this" (from a person, to you, personal or a reply, after 1–14 days) is opt-in because its measured precision is low. Computed on read with the rules `mailroom-admin nudge-report` measured; Done is stored, Snooze reuses snooze, both expire at 30 days. No push | v1.90.0 |
| Unsubscribe detection (List-Unsubscribe) | Surfaced, never auto-executed; RFC 8058 one-click deliberately not built (hostile-content §2.10) | shipped (v1.16.0) |
| One-click + bulk unsubscribe | Bulk half: the clean-up view surfaces each sender's unsubscribe through the existing affordance — asked first, never executed on its own. One-click (RFC 8058) stays deliberately unbuilt | partial (v1.89.0); one-click v2 |
| Newsletter bundling / digest | | v2 — #7 |
| Archive cleanup assistant (triage-clustered bulk actions) | Settings › Clean up: senders, domains or mailing lists ranked by volume or by neglect (unread, or archived unread from Mailroom) over 30/90/365 days or all time; per row unsubscribe (existing affordance), archive all, move all to an area (with a count, confirm and "Move the rest"), block. Measured at ~0.2 s all-time on a seeded 25k mailbox. Not clustered by the T1 model — ranked by counts | shipped (v1.89.0) |
| Thread summarization | T2 optional | v2 — #4 |
| Task / date extraction | T2 optional | v2 — #4 |
| Spam filtering | Provider-side (for example rspamd); we mirror Junk; "Not junk" writes back (spec §6), clearing `$Junk`/`Junk` and setting `$NotJunk` where the provider accepts keywords, so a rescue survives the next sync | — (provider's job; mirroring shipped, v1.89.0) |

## 7. Contacts

| Feature | Notes | Target |
|---|---|---|
| Internal contacts (auto-derived from mail) | | shipped (v1.0.0) |
| Reached-me-at-alias mapping | Powers reply-from default | shipped (v1.0.0) |
| Store contacts' public keys | Encryption | shipped (v1.64.0) |
| Autocomplete from contacts | | shipped (v1.0.0) |
| VIP / block sender | Contacts-level markers, set from the sender card or Settings › Senders. Block matches the exact From address and sends new mail to Junk (default) or Trash at ingest, before rules and learned placement, through the ordinary move with writeback; existing mail moves only when asked. VIP drives `is:vip`, the VIP push modes and the quiet-hours exception | shipped (v1.89.0) |
| Contact groups | | v2 — #6 |
| CardDAV sync (Nextcloud/Apple) | | v2 — #6 |
| Google contacts sync (CardDAV/People API) | | v2 — #6 |
| Contact photos / details | | v2 — #6 |

## 8. Security & privacy

| Feature | Notes | Target |
|---|---|---|
| At-rest encryption (mail/attachments/creds/keys) | Envelope encryption, per-user DEKs (spec §9/§11) | shipped (v1.0.0) |
| **Search-index + ML-model at-rest encryption** | **[v0.4]** Volume/FS-level (LUKS etc.) — stated deployment requirement; index contains decrypted-mail plaintext (spec §7/§11). Documented as a host prerequisite with manual verification commands; there is no code check, so the app never detects an unencrypted volume | **shipped (ops)** |
| **Restore drill (scripted, scheduled)** | **[v0.4]** Restore unit incl. master-key escrow; index excluded (rebuildable) (spec §11). CI runs it weekly and on every branch push; also run once against a real test-instance artifact. Step 5's PGP-root check has a known blind spot on key-less backups | **shipped (ops)** |
| TLS everywhere | App terminates no TLS by design; derives cookie `Secure`/HSTS from the OIDC redirect URL and expects a reverse proxy. Proxy requirements are now documented; verifying them on a deployed host is still owed | shipped (v1.0.0) |
| HTML sanitization + sandboxing | | shipped (v1.0.0) |
| Remote-content blocking default | | shipped (v1.0.0) |
| SSRF-guarded image proxy | | shipped (v1.43.0) |
| Attachment scanning (ClamAV, optional) | | shipped (v1.59.0) |
| PGP encrypt/decrypt | Server-side keys behind the separate crypto process (ProtonMail go-crypto/gopenpgp). PGP/MIME mail is decrypted on open and the plaintext is never stored, indexed or cached (ADR-037); outgoing mail is encrypted to keys Mailroom trusts (ADR-038) | **shipped (v1.92.0)** |
| PGP sign / verify | | shipped (v1.32.0) |
| S/MIME encrypt/sign/verify | **[v0.4]** Priced 2–3× PGP (thin Go ecosystem). Decided: deferred to 2.x (ADR-024 §3, 2026-08-05) — no longer an open question | v2 — #10 |
| WKD publishing (key authority) | Internet-Draft (pin revision); real identities only — catchall aliases unpublishable (spec §9) | shipped (v1.31.0) |
| Autocrypt headers | Outbound header deliberately sent without `prefer-encrypt=mutual` until decrypt exists | shipped |
| Shareable key pages (fingerprint/QR) | Partial: fingerprint page implemented (`internal/keypub/page.go`); no QR-code generation found anywhere in the tree | v1.x (partial) — #47 |
| DNS OPENPGPKEY publishing | | v3 — #16 |
| Phishing / homograph-URL warnings | ⚠ LINKS, not senders — the boundary with the row above, stated so this is not built twice or never. That one judges the `From` domain; this one judges URLs in message bodies. They share `internal/senderwarn.Skeleton`, which is exported for exactly that. | v2 — #11 |
| Biometric unlock (native apps) | | v2 — #12 |
| Hardware key / smartcard (YubiKey) for PGP | | v3 — #16 |
| 2FA | Via Authentik. No app-side 2FA code exists, by design — entirely the OIDC provider's concern | shipped (v1.0.0) |
| Audit log (incl. agent-key actions, conflict resolutions) | Partial: every AI-tool key has an activity list (v1.95.0), every decryption is audited (v1.92.0), and the purge job records what it removed. There is no single audit view across them | v1.0 (partial) — #45 |
| Per-user data export + hard delete | **[v0.4]** Format defined at v1.0 (mbox/maildir + JSON sidecars); implemented v1.x (spec §10). **Narrower than planned: export is maildir + JSON sidecars only** — mbox is deliberately rejected because an export must be byte-exact and "mbox cannot promise that" | shipped (v1.53.0) |

## 9. Notifications

| Feature | Notes | Target |
|---|---|---|
| Web Push (VAPID) | iOS: installed PWA only, OS-throttled (spec §12) | shipped (v0.12.0) |
| Triage-aware push | Per device: every message (the default — a deliberate deviation from spec §12's important-only default, ADR-016 amendment 2026-10-17), above the line, VIPs only, or either. Checked against the code before this change: push did fire for every ingested message, the `importantOnly` flag was never read. Above the line is the desk's own line plus the "Mark important" rule keyword; unjudged mail does not count | shipped (v1.89.0) |
| Per-account notification settings | `Account.notifyPush`: an account switched off notifies no device | shipped (v1.89.0) |
| VIP notifications | The VIP push mode and the quiet-hours exception | shipped (v1.89.0) |
| Quiet hours | Per user: start, end, days and an IANA time zone, wall-clock across daylight saving; only VIP mail notifies, and only while "Allow VIPs during quiet hours" is on (default on). Off by default | shipped (v1.89.0) |
| Actionable notifications (archive/reply) | | v2 — #9 |
| Badge / unread counts | In-app sidebar badge only; no OS-level `navigator.setAppBadge` integration | shipped (v1.0.0) |

## 10. Multi-device & sync

| Feature | Notes | Target |
|---|---|---|
| Cross-device sync (server-authoritative) | Change-log driven (spec §13/§14) | shipped (v1.0.0) |
| Offline read (web/PWA cache) | Partial: the service worker caches the app shell only, never `/jmap`; the client's persistent store (`localStorage`, not IndexedDB) holds only the offline action queue, optimistic overlay, prefs and search history — no mail. The app opens offline and mail already loaded in an open tab stays readable, but nothing of the mailbox survives a reload while offline | v1.0 (partial) — #44 |
| Offline compose + queued send | Ships already, ahead of its v1.x matrix target | shipped |
| Offline mail/search/label sync (native) | | v2 — #5 |
| Offline (desktop thick client) | | v3 — #13 |
| Selective sync / sync scope | Per-FOLDER `syncScope` (`full`/`off`) via `Mailbox/set`; per-account `syncEnabled` already existed. An `off` folder is skipped before SELECT so it is never reaped — its mail is frozen, not deleted (ADR-020). Per-DEVICE scope is not this: it needs change-log cursor work (spec §13/§14) and its own ADR. | shipped |
| Conflict resolution | Intent-delta model (spec §4a) | shipped (v1.0.0) |
| Backup / export (mbox / eml / maildir) | Export format defined v1.0; ships v1.x. **Narrower than planned: maildir + JSON sidecars only** — mbox deliberately rejected (see §8's export row) | shipped (v1.53.0) |
| Bulk archive import (mbox/maildir/Takeout) | See §1 — day-one archive ingest | shipped (v1.0.0) |
| Import from other clients (migration assistants) | | v3 — #20 |

## 11. Clients & platforms

| Feature | Notes | Target |
|---|---|---|
| Web app (Gmail-like) | | shipped (v1.0.0) |
| Light/dark mode | | shipped (v1.0.0) |
| Responsive (any screen size) | | shipped (v1.0.0) |
| No-transparency, clean layout | Requirement (unverified as a single code artifact — a design convention throughout the stylesheet, not one component) | shipped (v1.0.0) |
| Multi-select bulk-action UX | Requirement | shipped (v1.0.0) |
| Account health panel | **[v0.4]** Sync lag, dead-letters, outbound quota, delivery failures (spec §12). Distinct from the operator-facing Prometheus `/metrics` endpoint, whose metrics are all wired to running code | shipped (v0.11.0) |
| Keyboard shortcuts | | shipped (v1.0.0) |
| Command palette | Ships ahead of its v1.x matrix target | shipped |
| Custom shortcuts | | v2 — #9 |
| Accessibility (screen reader/keyboard/contrast) | Exercised by an `axe-core` e2e job in CI | shipped (v1.0.0) |
| Density setting | Ships ahead of its v1.x matrix target | shipped |
| i18n / localization | Framework ready v1; locales later. Framework part shipped (~1,270 keys, one `en` catalog); no second locale exists yet | v2 (partial — framework shipped, locales pending) — #12 |
| PWA (installable) | | shipped (v1.0.0) |
| Native iOS app | | v2 — #5 |
| Native Android app | | v2 — #5 |
| Mobile swipe actions | Ships ahead of its v2 target, but in the responsive **web** app (phone-width layout, archive/flag swipe), not a native app — native-app gestures remain part of #5 | shipped |
| Desktop thick client (Tauri, all OSes) | | v3 — #13 |
| CLI client | Possible via API | — |

## 12. Power-user / productivity

| Feature | Notes | Target |
|---|---|---|
| Send-and-archive | Beside Send, and on Ctrl/⌘+Shift+Enter, for a reply or forward | **shipped (v1.80.0)** |
| Multiple signatures | Same row as per-identity signatures (§3) — see ADR-017 for why not an identities column | shipped |
| Plus-addressing / catch-all awareness | Ties to alias logic. Sending *from* a derived catchall alias works only if the mail provider permits sending as any address on the domain, otherwise it refuses at `MAIL FROM` — now a documented deployment prerequisite; refusing such a From at compose time is still an undecided option | shipped |
| Disposable-alias generation | Requires provider API; Migadu-specific | v3 — #17 |
| Split inbox / multiple inboxes | The desk covers most of it: it already splits the inbox (Review first / Everything else, areas, category tabs). Saved searches as desk sections remain | v2 — #41 |
| Message pinning | | shipped |
| Vacation / out-of-office responder | One Mailroom-owned ManageSieve script per account (`internal/outofoffice`), refused while another script is active, drift shown in Settings; the replies are sent by the provider and are outside the outbound ledger (ADR-034) | **shipped (v1.90.0)** |

## 13. Collaboration / team

| Feature | Notes | Target |
|---|---|---|
| Shared mailboxes | | v3 — #14 |
| Delegation / send-on-behalf | | v3 — #14 |
| Internal notes / comments on threads | | v3 — #14 |
| Assignment / shared drafts / collision detection | | — (revisit if demand) |

## 14. Admin / multi-user

| Feature | Notes | Target |
|---|---|---|
| Multi-user from day one | Requirement — data model + isolation day one; admin *UI* may lag (spec §10) | shipped (v1.0.0) |
| OIDC provisioning (Authentik) | | shipped (v0.6.0) |
| Roles (admin can't read mail — software-enforced + audited, not cryptographic) | Honest wording per spec §9/§10 | shipped (v1.0.0) |
| Per-user quotas / rate limits | Enforced defaults v1.0; management UI may follow in v1.x. No admin UI for changing another user's limits was found | shipped (v1.0.0) |
| Per-user isolation (index/models/keys/blobs) | Layered design + CI negative tests (spec §10) | shipped (v1.0.0) |
| SCIM auto-provisioning | | v3 — #18 |

## 15. Integrations & extensibility

| Feature | Notes | Target |
|---|---|---|
| **RFC 8620/8621-conformant API subset** + OpenAPI/JSON-Schema | **[v0.4]** Upgraded from "JMAP-subset"; extensions as capability URNs (spec §13) | **shipped (v0.6.0)** |
| MCP server | Hardened defaults: no send+delete keys by default, agent deletes always soft, mandatory rate/budget (spec §13a, ADR-042, ADR-044). Reference: `docs/mcp.md` | **shipped (v1.95.0)** |
| Scoped API keys (read/send/organize/delete etc.) | Presets plus separate send and delete grants, optional account restriction, rate limit and daily budget, revocable | **shipped (v1.95.0)** |
| Per-key human-in-the-loop confirm mode | "Ask me first" for sending and deleting, on by default: a banner and a push notification, unanswered requests denied after 24 hours | **shipped (v1.95.0)** |
| Webhooks | | v2 — #9 |
| CalDAV / calendar hooks | iTIP accept/decline in mail shipped in v1.90.0; CalDAV sync stays here | v3 — #15 |
| Third-party JMAP clients can connect | **[v0.4]** Falls out of the RFC 8620 core conformance committed in spec §13. The internal conformance suite exists; exercising a real third-party client (aerc) in CI (`.github/workflows/`) is not wired yet | v1.x (partial) — #33 |
| Plugin/extension system | | — (revisit) |

## 16. Calendar (adjacent — mostly out of scope)

| Feature | Notes | Target |
|---|---|---|
| .ics / iTIP invitation parsing + accept/decline | Card above the body for REQUEST/CANCEL/REPLY, parsed server-side by a bounded in-house parser (ADR-033); Accept/Maybe/Decline send an iTIP REPLY through the ordinary submission path (Undo Send applies). Split out of CalDAV (#15) | **v1.90.0** |
| Calendar view | | — |
| CalDAV two-way | | v3 — #15 |
| Scheduling / availability | | — |

---

## Rollup by version

- **[v0.4] 0.5 (daily-driver, single-operator):** single user, real accounts; read/search/label/reply/archive; no ML, no crypto, no multi-user UI — but multi-user data model, isolation, content-hash identity, and change log from day one. Dogfooding gate before everything below.
- **v1.0 (MVP + intelligence):** multi-account sync + unified inbox + threading; **bulk archive import (mbox/maildir/Takeout)**; hybrid labels (IMAP-sync default, persisted keyword map); typo-tolerant full-text search (+ reindex op); alias-aware compose/reply + server-side From validation; delayed-submission engine (Undo Send); **outbound ledger + DSN pipeline**; T0 rules + T1 explainable priority inbox + label suggestions + online learning + **model snapshots/eval harness**; internal contacts; multi-user + isolation (with CI negative tests); security baseline incl. **index-volume encryption + restore drill**; Gmail-like web + PWA + web push (the triage filter is not built; see §9) + account health panel; **RFC 8620/8621-conformant documented API**.
- **v1.x:** PGP (+S/MIME if cost accepted) + WKD key authority + Autocrypt + key pages; MCP server + scoped keys (hardened defaults); ManageSieve filter/vacation editing; **archive cleanup assistant**; live Gmail IMAP (operator OAuth); snooze/mute/pin; attachment-text search; saved searches; send-later UI; signatures/templates; unsubscribe detection; sender-auth display; image proxy; data export.
- **v2:** optional LLM features; native iOS/Android (offline); CardDAV contacts sync; bulk unsubscribe; semantic/NL search; split inbox; actionable notifications; S/MIME if deferred.
- **v3:** desktop thick client; shared mailboxes/delegation; calendar/CalDAV hooks; hardware-key PGP; DNS OPENPGPKEY; disposable aliases; SCIM; optional Python trainer.
- **Not planned:** POP3, Exchange/EWS/ActiveSync, being a spam filter, being a mail server, CLI client, full team-collaboration suite, full calendar app (all revisitable if demand appears).
