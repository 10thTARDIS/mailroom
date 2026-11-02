// SPDX-License-Identifier: AGPL-3.0-only

// Shared JMAP wire types for the Mailroom client-core (spec
// docs/mailroom-spec.md §13). This module is intentionally
// framework-agnostic and DOM-free: it holds only type/interface
// declarations so it can be consumed unmodified from the web app, a future
// Tauri desktop shell, or a React Native / Capacitor mobile app (spec §12).
//
// These shapes are transcribed from the SERVER, not from the RFC: every
// object below matches what internal/api actually emits (email.go,
// mailbox.go, thread.go, label.go, identity.go, submission.go, search.go).
// Where Mailroom's server is narrower than RFC 8621 the type is narrower
// too, and the gap is called out — a type that promises a field the server
// never sends is worse than no type at all.

/** A JMAP account id (opaque string per RFC 8620; ULID in Mailroom, ADR-007). */
export type AccountId = string;

/** A JMAP object id (opaque string per RFC 8620; ULID in Mailroom). */
export type Id = string;

/** An opaque per-type state string used for `/changes` cursors (spec §13/§14). */
export type StateString = string;

/** An opaque blob id (Mailroom: the message content hash, ADR-001). */
export type BlobId = string;

/** Capability URNs the server advertises (internal/api/service.go, search.go). */
export const CAP_CORE = "urn:ietf:params:jmap:core";
export const CAP_MAIL = "urn:ietf:params:jmap:mail";
export const CAP_SUBMISSION = "urn:ietf:params:jmap:submission";
export const CAP_LABELS = "urn:mailroom:labels";
export const CAP_COMPOSE = "urn:mailroom:compose";
export const CAP_SEARCH = "urn:mailroom:search";
export const CAP_ACCOUNTS = "urn:mailroom:accounts";
export const CAP_CONTACTS = "urn:mailroom:contacts";
/** Web Push (Forgejo #42, ADR-016). Advertised only when the server offers Web
 * Push; the whole client push feature is gated on the session carrying it. */
export const CAP_PUSH = "urn:mailroom:push";
/** Local-first organization (spec §6, #51): isPinned/pinnedAt on Email, isMuted
 * on Thread, and the Thread/set that makes the mute settable. Its own capability
 * because RFC 8621 defines none of them — see internal/api/service.go. */
export const CAP_ORGANIZE = "urn:mailroom:organize";
/** The T1 priority extension (spec §8): priorityScore / isImportant /
 * priorityReasons on Email, the isImportant filter and `priority` sort, and the
 * Triage/* control methods. */
export const CAP_TRIAGE = "urn:mailroom:triage";
/** The T0 rules extension (spec §8): Rule/get|query|changes|set, Rule/preview
 * (#155), and Email.labelProvenance. */
export const CAP_RULES = "urn:mailroom:rules";
/** Remembered per-sender "show remote content" decisions (#202, spec §11):
 * RemoteAllow/get|changes|set. Its own capability because remote-content policy is
 * not mail data — and a client that does not declare it simply cannot read or write
 * the list, which fails safe: it then blocks remote content and asks every time. */
export const CAP_REMOTE_CONTENT = "urn:mailroom:remotecontent";
/** Saved signatures and templates (#56): ComposeSnippet/get|changes|set. */
export const CAP_COMPOSE_SNIPPETS = "urn:mailroom:composesnippets";
/** Unsubscribe detection (#58): the read-only `unsubscribe` property on Email. Declared
 * because RFC 8620 §3.3 requires it, not because it withholds anything — the `using`
 * gate is per-method. What makes the value safe is that the server refused every scheme
 * it could not vouch for before sending it. */
export const CAP_UNSUBSCRIBE = "urn:mailroom:unsubscribe";
/**
 * What a PGP signature on a message means (`pgpSignature`, #47, #626 section E,
 * ADR-039) - on the message itself, or inside its decrypted entity.
 *
 * - `verified`: it checks out, by a key held for the From address that names that
 *   address and that the user may rely on.
 * - `bad`: it does not check out.
 * - `unknownKey`: well-formed, but not attributable to this sender; `fingerprint`
 *   is what the signature says made it (may be a 16-character key id).
 * - `keyChanged`: a good signature by a key that DIFFERS from the one already
 *   pinned or trusted for this address; `previousFingerprint` is that one, and
 *   `refused` says the user already chose to keep the old key.
 */
export interface PgpSignature {
  state: "verified" | "bad" | "unknownKey" | "keyChanged";
  /** The From address it was judged against. */
  address?: string;
  fingerprint?: string;
  previousFingerprint?: string;
  refused?: boolean;
  /** Refines `bad` (#795/#796). `keyExpired`: the signature checked out and was
   * refused only because the key that made it (`fingerprint`) has expired. The
   * server gives it only for a key it would otherwise have trusted for the From
   * address. The state stays `bad`; this is wording, never an upgrade. */
  reason?: "keyExpired";
  /** reason keyExpired only: when the signature says it was made (UTCDate),
   * the signer's own claim. */
  signedAt?: string;
  /** reason keyExpired only: when the signing key expired (UTCDate). */
  keyExpiredAt?: string;
  /** reason keyExpired only: signedAt is before keyExpiredAt. */
  validWhenSigned?: boolean;
}

/** The answer of `ContactKey/decide` (#47, ADR-039). */
export interface ContactKeyDecision {
  address: string;
  fingerprint: string;
  decision: "trust" | "keep";
  /** The fingerprint pinned for the address after the decision. */
  pinnedFingerprint: string | null;
}

/** The answer of `ContactKey/import` (#47, ADR-038). */
export interface ContactKeyImportResult {
  address: string;
  /** The imported key's primary fingerprint, raw. */
  fingerprint: string;
  /** The user ID that names the address, as written in the key. */
  userId: string;
  /** The fingerprint pinned for the address; null on a dry run. */
  pinnedFingerprint: string | null;
  dryRun?: boolean;
}

/** Sender-authentication display (#55): the read-only `authResults` property on Email —
 * what SPF, DKIM and DMARC each said, as written down by the servers that handled the
 * message. Declared for the same RFC 8620 §3.3 reason as CAP_UNSUBSCRIBE, and buying the
 * same nothing: the `using` gate is per-method and there is no SenderAuth/* method. The
 * value is safe because the server reports the header's facts and synthesises no verdict,
 * not because of this URN.
 *
 * Also covers `senderWarnings` (#397): a different question about the same From header —
 * not "did this authenticate" but "is the domain a lookalike, a punycode label, or a
 * stranger claiming one of your own domains". See core/senderWarnings.ts. */
export const CAP_SENDER_AUTH = "urn:mailroom:senderauth";
/** Saved searches and the sidebar smart folders (#57): SavedSearch/get|changes|set. */
export const CAP_SAVED_SEARCHES = "urn:mailroom:savedsearches";
/**
 * Gmail-style category tabs (#228): CategoryTab/get|changes|set, the `tab`
 * property on Label, `Email.categoryTab`, and the `inTab` Email/query filter.
 *
 * DECLARING THIS IS NOT OPTIONAL DECORATION. The server's `using` gate is
 * per-method and fails closed, so a capability the client does not list makes
 * every one of its methods answer `unknownMethod` — and, because the tab
 * properties are opt-in per property, the read path can look perfectly healthy
 * while only the write path is dead. That is exactly #159, which shipped once.
 */
export const CAP_CATEGORY_TABS = "urn:mailroom:categorytabs";
/**
 * The attachment malware quarantine (#396/#412, ADR-027 §8): the `attachmentScan`
 * property on Email and the `Email/releaseAttachment` method.
 *
 * ⚠ DECLARING THIS IS NOT OPTIONAL, and specifically for the method, not the
 * property — internal/api/service.go's own comment says the property is
 * readable either way, but `methodCapabilities["Email/releaseAttachment"]` gates
 * on this exact URN, and capability.go fails a request naming an undeclared
 * capability's method with `unknownMethod`. Omitting it here would make the
 * Release control silently unusable while attachmentScan itself looked fine —
 * the same shape of bug CAP_CATEGORY_TABS's comment warns about (#159).
 */
export const CAP_ATTACHMENT_SCAN = "urn:mailroom:attachmentscan";
/**
 * The desk (#375/#442): the user's own PARTITION of mail — a THIRD axis
 * alongside category tabs (message function) and labels (topic). It gates the
 * Area object and its methods (Area/get|changes|set and the Area/getSummary
 * counts), the DeskPreferences singleton that holds the LINE, and the
 * per-client-family ClientPreferences. It also names the `inArea`, `unsorted`,
 * `onDesk` and `onSurface` Email/query filters and Email's `area` /
 * `drawerArchivedAt` properties.
 *
 * ⚠ DECLARING IT IS NOT OPTIONAL, for exactly the reason CAP_CATEGORY_TABS
 * spells out: internal/api/capability.go gates every Area/* and
 * DeskPreferences/* METHOD on this URN and answers `unknownMethod` when a
 * request does not name it. Without it the desk loads no areas and no line at
 * all — while the plain mailbox views keep working perfectly, which is the
 * failure shape of #159.
 */
export const CAP_DESK = "urn:mailroom:desk";
/** Calendar invitations (#725, ADR-033): the read-only `calendarInvite`
 * property on Email — parsed on the SERVER, so this client never handles a raw
 * .ics — and `Email/replyToInvite`, which answers one through the ordinary
 * submission path (Undo Send applies). The method is genuinely gated by this
 * URN; the property is opt-in like every other Mailroom property. */
export const CAP_CALENDAR = "urn:mailroom:calendar";

/**
 * OpenPGP keys (#374): the user's OWN key (`UserKey/get`), the keys held for
 * correspondents (`Contact.keys`, and the `hasKey` filter on `Contact/query`).
 *
 * ⚠ THIS ONE IS DELIBERATELY ABSENT FROM `MAILROOM_USING`, AND THAT IS THE
 * WHOLE DESIGN OF THE KEYS ROW.
 *
 * RFC 8620 §3.3 makes a `using` list naming a capability the server does not
 * advertise a REQUEST-level failure, and internal/api/service.go implements
 * exactly that (`validateUsing` → HTTP 400, the whole batch refused before a
 * single method runs). So putting this URN in the standing `using` set would
 * not degrade the Keys row on a server that predates the keys API — it would
 * break EVERY request the app makes, on every screen, including the ones that
 * have nothing to do with keys.
 *
 * The two helpers in jmap/client.ts (`userKeysGet`, `contactKeyCount`) are
 * therefore the only callers, they append this URN to their own `using` list,
 * and they only send anything at all when the SESSION advertises the capability
 * (`hasCapability`). A server without it is answered with `null` — "not
 * available on this server" — never with a guess and never with a count of
 * zero, because zero keys and an unaskable question are different facts.
 */
export const CAP_PGP = "urn:mailroom:pgp";

/**
 * Out of office (#50): one OutOfOffice object per MAIL account, written to the
 * provider over ManageSieve. Absent from `MAILROOM_USING` for CAP_PGP's
 * reason: a server that predates it would refuse every request naming it, so
 * the out-of-office helpers in jmap/client.ts add it themselves, and only when
 * the session advertises it.
 */
export const CAP_OUT_OF_OFFICE = "urn:mailroom:outofoffice";

/**
 * Agent access (#49, ADR-042): API keys, their activity, and the approval
 * requests a confirm-mode key leaves for a person. Absent from
 * `MAILROOM_USING` for CAP_PGP's reason: a server that predates v1.95 would
 * refuse every request naming it, so the helpers in jmap/client.ts add it
 * themselves, and only when the session advertises it.
 */
export const CAP_APIKEYS = "urn:mailroom:apikeys";

/**
 * Signed-in devices (#937, ADR-048 §6): the user's live sessions, and signing
 * them out. Absent from `MAILROOM_USING` for CAP_APIKEYS's reason; the helpers
 * in jmap/client.ts add it themselves, only when the session advertises it.
 */
export const CAP_DEVICES = "urn:mailroom:devices";

/** One live session (`SignedInDevice/get`). `lastAddr` is display data and may
 * be empty. `isCurrent` is the session making the request. */
export interface SignedInDevice {
  id: Id;
  /** Parsed from the user agent: "Firefox on Linux". */
  description: string;
  createdAt: string;
  lastSeenAt: string;
  lastAddr: string;
  isCurrent: boolean;
}

/** The `urn:mailroom:apikeys` session object (internal/api/agent_keys.go
 * apiKeysCapability): what a client building a key needs to know, stated by
 * the server rather than assumed. Every field is optional on purpose — a
 * client reads what is there and falls back to asking the server (by leaving
 * the setting out of the create) for what is not. */
export interface ApiKeysCapability {
  scopes?: string[];
  confirmableScopes?: string[];
  defaultRatePerMinute?: number;
  maxRatePerMinute?: number;
  defaultDailyBudget?: number;
  maxDailyBudget?: number;
  /** Not yet advertised by every server: when absent the create leaves
   * `suspendOnAnomaly` out, so the server's own default applies. */
  defaultSuspendOnAnomaly?: boolean;
  approvalWindowSeconds?: number;
  labelRestrictionSupported?: boolean;
  massDeleteThreshold?: number;
  massDeleteWindowSeconds?: number;
  massSendThreshold?: number;
  massSendWindowSeconds?: number;
  readUnitObjects?: number;
}

/** One API key as ApiKey/get returns it. Never carries the token. */
export interface ApiKey {
  id: Id;
  name: string;
  /** `mr_` plus the first characters: enough to tell keys apart. */
  tokenHint: string;
  scopes: string[];
  /** null: every account. */
  accountIds: Id[] | null;
  confirmScopes: string[];
  ratePerMinute: number;
  dailyBudget: number;
  suspendOnAnomaly: boolean;
  createdAt: string;
  lastUsedAt: string | null;
  revokedAt: string | null;
  suspendedAt: string | null;
  suspendedReason: string | null;
  /** Cost charged in the last minute / 24 hours (ApiKey/get only). */
  usedLastMinute?: number;
  usedLast24h?: number;
}

/** The writable part of a key, for ApiKey/set create and update. */
export interface ApiKeySettings {
  name?: string;
  scopes?: string[];
  accountIds?: Id[] | null;
  confirmScopes?: string[];
  ratePerMinute?: number;
  dailyBudget?: number;
  suspendOnAnomaly?: boolean;
  suspended?: boolean;
  acknowledgeSendAndDelete?: boolean;
}

/** One row of ApiKey/getEvents. `summary` holds counts and ids, never content. */
export interface ApiKeyEvent {
  id: Id;
  keyId: Id;
  at: string;
  method: string;
  outcome: string;
  cost: number;
  deletes: number;
  sends: number;
  summary: Record<string, unknown> | null;
  clientIp: string | null;
  pendingActionId: Id | null;
  errorType: string | null;
  anomaly: string | null;
}

/** Refusals past a key's per-minute cap, counted per (minute, outcome, method,
 * reason) rather than listed one by one. */
export interface SuppressedRefusal {
  keyId: Id;
  minute: string;
  outcome: string;
  method: string;
  reason: string;
  count: number;
}

/** A held send's attachments, as the server summarised them. */
export interface PendingAttachments {
  count: number;
  names: string[];
  totalSize: number;
}

/** What approving a held send also archives. */
export interface PendingArchive {
  count: number;
  subjects: string[];
}

/** One approval request (PendingAction/get, ADR-042 section 7). The UI renders
 * the structured fields; `summary` is a notification's prose and is never
 * parsed or shown in place of them. */
export interface PendingAction {
  id: Id;
  keyId: Id;
  /** The JMAP account the request acts in. Not sent by every server yet; when
   * absent, nothing is looked up on the request's behalf. */
  accountId?: Id;
  keyName?: string;
  kind: "send" | "delete" | string;
  method: string;
  summary: string;
  state: "pending" | "approved" | "denied" | "expired" | "failed" | string;
  createdAt: string;
  expiresAt: string;
  decidedAt: string | null;
  detail: string | null;
  emailIds?: Id[];
  recipients?: string[];
  to?: string[];
  cc?: string[];
  bcc?: string[];
  subject?: string | null;
  sendAt?: string | null;
  attachments?: PendingAttachments | null;
  archive?: PendingArchive | null;
}

/** What the last look at the provider found (internal/outofoffice). */
export type OutOfOfficeServerState = "off" | "active" | "displaced" | "missing" | "modified" | "error";

/** One mail account's out-of-office reply (internal/api/outofoffice.go). */
export interface OutOfOffice {
  /** The MAIL account's id. */
  id: Id;
  accountName: string;
  isEnabled: boolean;
  /** UTC instants; null is open-ended. */
  fromDate: string | null;
  toDate: string | null;
  subject: string;
  textBody: string;
  /** :days — at most one reply per sender in this many days (1–30). */
  replyDays: number;
  fromAddress: string;
  contactsOnly: boolean;
  /** The account's own addresses: the From choices, and the addresses a
   * message must be sent to for a reply to go out. */
  addresses: string[];
  isSupported: boolean;
  unsupportedReason?: string;
  serverState: OutOfOfficeServerState;
  serverDetail: string;
  checkedAt: string | null;
  contactsIncluded: number | null;
  contactsTotal: number | null;
  maxContacts: number;
}

/** The whole form, as OutOfOffice/set takes it (every field required). */
export interface OutOfOfficeUpdate {
  isEnabled: boolean;
  fromDate: string | null;
  toDate: string | null;
  subject: string;
  textBody: string;
  replyDays: number;
  fromAddress: string;
  contactsOnly: boolean;
}

/**
 * The `urn:mailroom:pgp` session-capability payload (#492,
 * internal/api/session.go `pgpCapability` — spelled exactly as the server
 * names it: `signing`). Whether THIS SERVER can sign outbound mail at ALL —
 * i.e. whether a crypto process is wired to it, since the private half of a
 * key only ever exists behind that boundary (ADR-024). Server-WIDE, not
 * per-identity: the other half of "Will sign" is `Identity.signingKeyId`
 * above, and a client needs BOTH to be true.
 */
export interface PgpCapability {
  signing: boolean;
  /** Whether THIS SERVER can encrypt outgoing mail at all (#47, ADR-038): a crypto
   * process wired for signing AND encrypting. Absent on a server that predates
   * encryption — read it with `client.pgpEncrypt()`, which maps absent to null. */
  encrypt?: boolean;
  /** Whether UserKey/generate, /import, /extend, /revoke and /export can be
   * called (#796): a crypto process is wired for key custody. UserKey/set
   * (bind, publish, retire) and the reads work either way. Absent on a server
   * before #796. */
  keyManagement?: boolean;
  /** The ONE expiry-warning threshold in days (#795), the one every
   * `expiringSoon` flag the server returns uses. Absent before #795. */
  keyExpiryWarningDays?: number;
}

/**
 * One answer of `RecipientKey/lookup` (#47, ADR-038): can this address be
 * encrypted to, and with which key. NEVER the key itself — the bytes stay on the
 * server. `usable` is false when `keyChanged` (the address is held back until the
 * user confirms the new key; the decision is ContactKey/decide).
 */
export interface RecipientKeyInfo {
  address: string;
  usable: boolean;
  /** The key that would be used (or, when `keyChanged`, the NEW key). */
  fingerprint: string | null;
  /** "manual" (imported), "wkd" or "autocrypt"; null when no usable key. */
  source: string | null;
  /** What the WKD side did: notAsked | unavailable | skipped | found | none | unusable | failed. */
  wkd: string;
  keyChanged: boolean;
  /** The fingerprint last encrypted to, when `keyChanged`. */
  pinnedFingerprint: string | null;
  /** #795: when the chosen key stops working (UTCDate; null = never). Absent on
   * a server before #795. */
  expiresAt?: string | null;
  /** #795: usable, and expiresAt is inside the server's warning window. */
  expiringSoon?: boolean;
  /** #795: why the address is not usable; null when it is. `expired` comes with
   * `expiredAt`. */
  reason?: "expired" | "revoked" | "keyChanged" | "noKey" | null;
  /** reason expired only: the latest moment an eligible key stopped working. */
  expiredAt?: string | null;
}

/**
 * The `using` set Mailroom's own client sends. RFC 8620 §3.3 requires a client
 * to declare every capability the methods in its request depend on; we declare
 * the full Mailroom set once rather than computing a minimal set per batch,
 * because every one of them is advertised by the session for every account.
 */
export const MAILROOM_USING: readonly string[] = [
  CAP_CORE,
  CAP_MAIL,
  CAP_SUBMISSION,
  CAP_LABELS,
  CAP_COMPOSE,
  CAP_SEARCH,
  CAP_ACCOUNTS,
  CAP_CONTACTS,
  CAP_ORGANIZE,
  CAP_TRIAGE,
  CAP_RULES,
  CAP_REMOTE_CONTENT,
  CAP_COMPOSE_SNIPPETS,
  CAP_UNSUBSCRIBE,
  CAP_SENDER_AUTH,
  CAP_SAVED_SEARCHES,
  CAP_CATEGORY_TABS,
  CAP_ATTACHMENT_SCAN,
  CAP_DESK,
  CAP_CALENDAR,
];

/**
 * A Mailroom mail Account (urn:mailroom:accounts, internal/api/account.go): the
 * manageable, NON-secret surface of an IMAP/SMTP login. The password and
 * username are write-only — sent on create/update, sealed server-side, and NEVER
 * returned here — so they are deliberately absent from this type.
 */
export interface Account {
  id: Id;
  name: string;
  providerProfile: string;
  imapHost: string;
  imapPort: number;
  smtpHost: string;
  smtpPort: number;
  syncEnabled: boolean;
  /**
   * Ingest-only mode (ADR-028 §1): the account's OWN column, which is one half
   * of whether Mailroom actually writes to it right now — an instance-level
   * switch ANDs in on top of this (ADR-028 §2), so the true, effective answer
   * is `AccountHealth.writeback.enabled`, not this field. This is the value
   * the settings toggle reads and writes; a client that rendered a mail-view
   * banner from THIS field rather than from health could say "writing" while
   * the instance switch has silently refused every write.
   */
  writebackEnabled: boolean;
  /**
   * The user-chosen tint for this account's dot (#374), as `#RRGGBB`, or null for
   * "no colour — use the ordinal tone".
   *
   * ⚠ ONE STRING FOR BOTH THEMES. Every other colour in this app is a token that
   * resolves per theme (tokens.css); this one is stored, so whatever is picked is
   * the same hex on light and on dark. The picker's presets are chosen with that
   * in mind (ui/settings/accountColor.ts).
   *
   * The server accepts nothing but `#RRGGBB` or null (internal/api/account.go
   * validHexColor), so a value here that is neither did not come from a write
   * this client made — and it is headed for a `style` attribute, which is the
   * wrong place to discover that. accountColorOrToken refuses it.
   */
  color: string | null;
  /** Web Push for new mail in this account (#59), on every device. Optional for
   * a server before #59; absent reads as "cannot say". */
  notifyPush?: boolean;
  /**
   * How the account signs in (#52, internal/api/account.go accountAuthProps),
   * all four READ-ONLY. Optional for a server before v1.96; absent reads as "a
   * password account", which is all an older server had.
   *
   * `authState` is null for a password account. For an OAuth one: "active";
   * "needs_reauth" (the user must sign in at the provider again — the banner
   * and Reconnect); "operator_error" (the provider refused THIS SERVER, which
   * only its operator can fix — never "reconnect"); or "disconnected" (no
   * grant; Reconnect restores it).
   */
  authKind?: "password" | "oauth2";
  authState?: AccountAuthState | null;
  /** "google" | "microsoft" for an OAuth account, else null. */
  authProvider?: string | null;
  /** The address the provider vouched for; null without a grant. */
  authAddress?: string | null;
}

/** An OAuth account's sign-in state (Account.authState). */
export type AccountAuthState = "active" | "needs_reauth" | "operator_error" | "disconnected";

/** The write-only fields a create/connection-change carries (never read back). */
export interface AccountCreate {
  name: string;
  imapHost: string;
  imapPort?: number;
  smtpHost: string;
  smtpPort?: number;
  username: string;
  password: string;
  providerProfile?: string;
  syncEnabled?: boolean;
  color?: string | null;
}

/**
 * JMAP's "patch object" shape used by `/set` update maps: a sparse set of
 * property paths (`keywords/$seen`, `mailboxIds/<id>`) to new values, or `null`
 * to remove the property (RFC 8620 §5.3).
 */
export type PatchObject = Record<string, unknown>;

/** A JMAP `SetError` (RFC 8620 §5.3; internal/api/errors.go). */
export interface SetError {
  type: string;
  description?: string;
  properties?: string[];
}

/** A JMAP method-level error (RFC 8620 §3.6.2; internal/api/errors.go). */
export interface MethodErrorArgs {
  type: string;
  description?: string;
}

// ---------------------------------------------------------------------------
// Request / response envelope (RFC 8620 §3.2–3.4; internal/api/request.go)
// ---------------------------------------------------------------------------

/** One [name, arguments, callId] triple. */
export type Invocation = [name: string, args: Record<string, unknown>, callId: string];

export interface JmapRequest {
  using: readonly string[];
  methodCalls: Invocation[];
}

export interface JmapResponse {
  methodResponses: Invocation[];
  sessionState: StateString;
}

/** A "#"-prefixed result reference argument (RFC 8620 §3.7). */
export interface ResultReference {
  resultOf: string;
  name: string;
  path: string;
}

// ---------------------------------------------------------------------------
// Generic method shapes
// ---------------------------------------------------------------------------

export interface GetArgs {
  accountId: AccountId;
  /** null/omitted = "all" for Mailbox/Label/Identity/EmailSubmission; Email and
   * Thread REQUIRE ids (internal/api/email.go, thread.go). */
  ids?: Id[] | null;
  properties?: string[] | null;
  /**
   * RFC 8621 §4.2's body-value arguments, honoured by `Email/get` only.
   *
   * They are on the shared shape rather than an Email-specific one because
   * `Email/get` is the only `/get` in this API with body parts at all, and the
   * server ignores an argument a method does not know. Asking for them is what
   * makes the server decrypt and parse the blob, so nothing that only needs
   * headers should name them (core/sync.ts is the one caller that does, for the
   * reading pane and for resuming a draft).
   */
  fetchTextBodyValues?: boolean;
  fetchHTMLBodyValues?: boolean;
  maxBodyValueBytes?: number;
}

export interface GetResult<T> {
  accountId: AccountId;
  state: StateString;
  list: T[];
  notFound: Id[];
}

export interface ChangesArgs {
  accountId: AccountId;
  sinceState: StateString;
  maxChanges?: number | null;
}

export interface ChangesResult {
  accountId: AccountId;
  oldState: StateString;
  newState: StateString;
  hasMoreChanges: boolean;
  created: Id[];
  updated: Id[];
  destroyed: Id[];
}

export interface SetArgs {
  accountId: AccountId;
  ifInState?: StateString | null;
  create?: Record<string, unknown> | null;
  update?: Record<Id, PatchObject> | null;
  destroy?: Id[] | null;
}

export interface SetResult<T = Record<string, unknown>> {
  accountId: AccountId;
  oldState: StateString;
  newState: StateString;
  created: Record<string, T> | null;
  updated: Record<Id, T | null> | null;
  destroyed: Id[] | null;
  notCreated: Record<string, SetError> | null;
  notUpdated: Record<Id, SetError> | null;
  notDestroyed: Record<Id, SetError> | null;
}

/**
 * The Email/query filter Mailroom actually honors (internal/api/email.go's
 * `emailFilterConditions`). NOTE: the server rejects any condition it does not
 * know with `unsupportedFilter`, and it does NOT implement FilterOperator
 * (AND/OR/NOT groups) — a filter is a single flat condition object. Views that
 * need a disjunction (e.g. the unified inbox across accounts) are composed
 * client-side out of several queries.
 */
export interface EmailFilter {
  inMailbox?: Id;
  inThread?: Id;
  hasKeyword?: string;
  notKeyword?: string;
  /** UTCDate, inclusive lower bound on receivedAt. */
  after?: string;
  /** UTCDate, exclusive upper bound on receivedAt. */
  before?: string;
  // Text conditions — answered by the search index (internal/api/search.go).
  text?: string;
  from?: string;
  to?: string;
  cc?: string;
  subject?: string;
  body?: string;
  /** Mailroom extension (urn:mailroom:search): the operator syntax a search box
   * produces, e.g. `from:alice invoice is:unread`. */
  query?: string;
  /** Mailroom extension (urn:mailroom:search, #724): treat the FINAL bare word of
   * `query` as a prefix — as-you-type. A modifier, not a condition. */
  queryPrefix?: boolean;
  /** Mailroom extension (urn:mailroom:organize, #51): true selects only snoozed
   * mail (the Snoozed view), false only mail that is not snoozed (what a mailbox
   * view wants). Evaluated server-side against the current time. */
  isSnoozed?: boolean;
  /** Mailroom extension (urn:mailroom:triage, #154): mail the T1 model scored at
   * or above the importance threshold. An unscored message is never important —
   * a model that has not spoken does not get to promote mail. */
  isImportant?: boolean;
  /** Mailroom extension (urn:mailroom:rules, #169): mail the classifier declined
   * to judge — the needs-a-category queue. It is a filter on the QUESTION, not
   * the message: mail the model was sure about, mail it had no model for, and
   * mail whose question a human already answered all read as false, because none
   * of them is waiting on anybody. */
  needsCategory?: boolean;
  /**
   * Mailroom extension (urn:mailroom:categorytabs, #228): mail whose resolved
   * category tab is this one.
   *
   * PASS THE TAB THE USER CLICKED. The server applies the enabled-fold, so
   * asking for `primary` also returns mail from every tab the user switched
   * off — the client never has to know which those are. Asking for a tab that
   * IS switched off is refused with `invalidArguments` rather than answered
   * with an empty list, so a UI must not offer a disabled tab.
   *
   * ⚠ Not delta-stable: `Email/queryChanges` refuses a filter containing it,
   * because a message's tab can move with no Email change behind it (a
   * re-pointed label, a classifier version bump). Re-query rather than expecting
   * a delta.
   */
  inTab?: CategoryTab;
  /**
   * Mailroom extension (urn:mailroom:accounts): mail that LIVES in one mail
   * account — a live provenance row in that account (internal/store/jmap_email.go
   * QueryEmails). It is what scopes the desk to a single account without going
   * through the search index, which spec §7 says may legitimately be absent.
   *
   * A foreign or other-user account id matches zero rows rather than erroring
   * (RLS, fail-closed), so an unknown scope reads as an empty desk, never as
   * somebody else's mail.
   */
  inAccount?: Id;
  /**
   * Mailroom extension (urn:mailroom:desk, #442): mail this AREA claims — the
   * desk's own partition. Unlike `inTab` there is no enabled-fold: an area
   * either claims a message or it does not.
   *
   * ⚠ Not delta-stable, like `inTab`: `Email/queryChanges` refuses a filter
   * containing it, because an area's claim can move with no Email change behind
   * it (a re-pointed label, a new feed). The desk re-runs its queries when the
   * Email state moves — see SyncEngine.refreshDesk.
   */
  inArea?: Id;
  /** Mailroom extension (urn:mailroom:desk, #442): mail NO area claims — the
   * desk's "Needs sorting" pile. Mutually exclusive with `inArea`; `false` is a
   * no-op server-side rather than "claimed by some area", so a filter never has
   * to be negated to mean "no restriction". Not delta-stable. */
  unsorted?: boolean;
  /** Mailroom extension (urn:mailroom:desk, #375): mail that is (`true`) or is
   * not (`false`) ON THE DESK — live in INBOX or a folder that feeds the desk,
   * and not junk, trashed, vanished or a draft. It is the EXACT predicate
   * `Area/getSummary` counts over, which is what makes a section's list and its
   * count incapable of disagreeing. Not delta-stable. */
  onDesk?: boolean;
  /** Mailroom extension (urn:mailroom:desk, #375): which side of the LINE
   * (`DeskPreferences.lineThreshold`) a message falls on. `true` keeps a score
   * at or above the line OR NO SCORE AT ALL — unjudged mail is never hidden in a
   * drawer — and `false` keeps a score below it. Not delta-stable. */
  onSurface?: boolean;
  /** Mailroom extension (urn:mailroom:desk, #446): mail day-one setup filed away
   * (`true`) or did not (`false`) — the `deskArchivedAt` mark, which is off the
   * desk with NOTHING moved on the server. `false` is a real restriction here,
   * unlike `unsorted`, so pass the property only when the question is actually
   * about the mark. Not delta-stable, like the desk's other filters. */
  deskArchived?: boolean;
}

export interface Comparator {
  /** `returnedAt` is Mailroom's (urn:mailroom:organize, #152): order by
   * effective arrival, so a message returning from a snooze sorts by when it
   * came back rather than by how old it is. */
  /** `priority` is Mailroom's (urn:mailroom:triage, #154): spec §6's Priority
   * Inbox order — the important bucket first, then everything else, each
   * newest-first. Grouped server-side so the view pages correctly. */
  /** `suggestionConfidence` is Mailroom's (urn:mailroom:rules, #176): the
   * needs-a-category queue's order — the message's highest open-suggestion
   * confidence first. It MUST be asked of the server: a client can only reorder
   * the page it holds, and a page selected by arrival is the newest N questions,
   * not the most confident ones. */
  /** `relevance` is Mailroom's (urn:mailroom:search, #724): the search index's
   * best-match order, paged by position like the others. Only meaningful with a
   * text filter; with none there is nothing to rank and the server falls back to
   * newest first. */
  property: "receivedAt" | "sentAt" | "returnedAt" | "priority" | "suggestionConfidence" | "relevance";
  isAscending?: boolean;
}

export interface QueryArgs {
  accountId: AccountId;
  filter?: EmailFilter | null;
  sort?: Comparator[] | null;
  position?: number;
  limit?: number | null;
  calculateTotal?: boolean;
}

export interface QueryResult {
  accountId: AccountId;
  queryState: StateString;
  canCalculateChanges: boolean;
  position: number;
  total: number;
  ids: Id[];
  /** RFC 8620 §5.5: present when the server applied a limit other than the
   * one requested (its maximum, or none was sent) — the page is then at most
   * this long, and `total` says how much more there is. */
  limit?: number;
  /** Mailroom extension (urn:mailroom:search, #722): present when the search
   * index answered with as many hits as it returns — its limit, which is the
   * value. `total` and `ids` then count only those, so `total` is a LOWER BOUND
   * on the matches, never the number of them. */
  searchCapped?: number;
}

/**
 * The EmailSubmission/query FilterCondition (RFC 8621 §7.3, #653). The server
 * implements exactly these three and REFUSES any other (`unsupportedFilter`).
 *
 * `inOutbox` is the Mailroom extension (urn:mailroom:compose): exactly the
 * submissions ui/outbox.ts `outboxSubmissions` shows — the latest per message,
 * if it is scheduled, or failed and not dismissed. The server's copy of that
 * predicate is internal/store/submissions.go `outboxPredicate`.
 */
export interface EmailSubmissionFilter {
  undoStatus?: "pending" | "final" | "canceled";
  emailIds?: Id[];
  inOutbox?: true;
}

// ---------------------------------------------------------------------------
// Entities (exactly what internal/api renders)
// ---------------------------------------------------------------------------

export type MailboxRole =
  | "inbox"
  | "archive"
  | "drafts"
  | "sent"
  | "trash"
  | "junk"
  | "all"
  | "flagged"
  | null;

/** RFC 8621 §2 Mailbox (internal/api/mailbox.go).
 *
 * Mailroom's mailboxes are FLAT (parentId is always null). They DO carry an
 * account id (`accountId` below, #488) — folder NAMES collide across
 * accounts, so this is what lets a client tell one account's Inbox from
 * another's, or group a folder list by account, instead of guessing from the
 * name. Per-account VIEWS (the search bar's account scope) still go through
 * the search `account:` operator (see core/views.ts); accountId is what a
 * client reads to know which account a given folder is already in. */
export interface Mailbox {
  id: Id;
  /**
   * The owning account (`urn:mailroom:accounts` extension property, #488),
   * read-only. Folder NAMES collide across accounts — every account has an
   * INBOX, a Sent, an Archive — so a client cannot tell one account's folder
   * from another's by name alone; this is what lets it filter or group
   * mailboxes by account instead of guessing.
   */
  accountId: Id;
  name: string;
  parentId: null;
  role: MailboxRole;
  sortOrder: number;
  totalEmails: number;
  unreadEmails: number;
  totalThreads: number;
  unreadThreads: number;
  isSubscribed: boolean;
  /**
   * SELECTIVE SYNC (#62, urn:mailroom:accounts extension property).
   *
   * `"off"` means the server does not look at this folder at all: it is skipped
   * before SELECT, so it gains no new mail AND is never reaped. Everything it
   * already held is still here and still readable — a FROZEN SNAPSHOT, not a
   * deletion.
   *
   * Which is exactly why the UI must show it. A folder that quietly stopped
   * updating while looking identical to a live one is a lie about mail: it would
   * still list messages the sender has since deleted, and miss everything that
   * arrived. Optional because an older server does not send it; treat a missing
   * value as `"full"`.
   */
  syncScope?: "full" | "off";
  /**
   * FOLDERS FEED THE DESK (#442/#446, urn:mailroom:desk extension property).
   *
   * True when live mail in this folder counts toward the desk exactly as INBOX
   * does — unactioned, landing in whatever area claims it, counted by
   * `Area/getSummary`. It is the server side of day-one's first decision
   * ("Folders feed the desk" against "Let Mailroom sort"), and it is on the
   * OBJECT as well as settable so a client configuring the desk can read its own
   * last write back rather than trust it (internal/api/mailbox.go).
   *
   * Optional because an older server does not send it — and absent is NOT
   * `false`: a folder whose setting could not be read has not said no.
   */
  feedsDesk?: boolean;
}

export interface EmailAddress {
  name: string | null;
  email: string;
}

/** One known address of an auto-derived Contact (internal/api/contact.go).
 * `reachedMeAt` is the alias the correspondent reached the user at — the
 * reply-from default for a new message to them (spec §5), or null when none is
 * known. */
export interface ContactAddress {
  /** The address row's id (#816): what `Contact/separate` names. Optional
   * because a server before #816 does not send it. */
  id?: Id;
  address: string;
  reachedMeAt: string | null;
}

/** One OpenPGP key held for a correspondent (`urn:mailroom:pgp`, #374).
 *
 * The FINGERPRINT is the identity of a key — not the key id, which is a
 * truncation of it and collides. Printed in groups of four so it can be read
 * aloud against another copy, which is the only thing a fingerprint is for. */
export interface ContactKey {
  fingerprint: string;
  /** The `app.key_kind` value as the server names it (e.g. `pgp`). */
  kind: string;
  /** Where key discovery found it: `wkd` | `autocrypt` | `manual` | `keyserver`. */
  source: string;
  /** When the server fetched or last refreshed it (JMAP UTCDate). The server
   * names this `fetchedAt`, not `createdAt` — a misnamed field here would read
   * as `undefined` forever (#155), which is why it is spelled exactly. */
  fetchedAt: string;
  /** The user chose to keep an older key instead of this one (served since
   * v1.93). */
  refused?: boolean;
}

/** RFC-8620-style Contact object for the `urn:mailroom:contacts` extension
 * (internal/api/contact.go). Auto-derived from ingest. `Contact/set` (#59)
 * changes only the user's three markers — `isVip`, `isBlocked` and
 * `blockDestination`; everything else is the server's.
 *
 * `keys` is OPTIONAL, and the optionality is the honest part: it is served only
 * by a server that advertises `urn:mailroom:pgp` and only when the property is
 * asked for, so `undefined` means "this server was not asked, or cannot answer"
 * while `[]` means "asked, and this correspondent has no key". A client that
 * collapsed the two would report "no key" for a server that has never been able
 * to say. */
export interface Contact {
  id: Id;
  displayName: string;
  addresses: ContactAddress[];
  keys?: ContactKey[];
  /** The user's VIP marker (#59): `is:vip` in search, the VIP push modes and
   * the quiet-hours exception. Optional because a server before #59 does not
   * send it; absent reads as "cannot say", never as "not a VIP". */
  isVip?: boolean;
  /** Blocked (#59): NEW mail from this contact's addresses goes to
   * `blockDestination` at ingest, before rules and learned placement. */
  isBlocked?: boolean;
  /** Where a block sends new mail. Reported whether or not blocked. */
  blockDestination?: BlockDestination;
  /** When it was blocked (UTCDate), null while not blocked. */
  blockedAt?: string | null;
  // ---- the saved-contact surface (#816, ADR-041). Every one is OPTIONAL
  // because a server before #816 sends none of them; absent reads as "cannot
  // say", never as "not saved" or "no notes".
  /** The displayed name: `userName` when set, else `displayName`. May be "":
   * show the primary address then. */
  name?: string;
  /** The user's own name for them (null when unset). Ingest never writes it. */
  userName?: string | null;
  /** The user's notes ("" when none). At most 4,000 characters. */
  notes?: string;
  /** Listed on the Contacts page. Sending to someone saves them; VIP saves;
   * blocking does not; un-saving deletes nothing. */
  isSaved?: boolean;
  savedAt?: string | null;
  /** STATISTICS, NOT SYNCED (ADR-041 §3): ingest moves them without a change
   * row, so a page that shows them re-reads the contact when it opens.
   * `inboundCount` is sightings other than the user's own sent mail, so it
   * also counts mail the person was only copied on. */
  inboundCount?: number;
  outboundCount?: number;
  firstMailAt?: string | null;
  lastMailAt?: string | null;
}

/** `Contact/merge` (#816, ADR-041 §4): `from` becomes a tombstone and is
 * reported destroyed; `id` is `into`. */
export interface ContactMergeResult {
  id: Id;
  destroyedId: Id;
  addressesMoved: number;
  keysMoved: number;
  keysCombined: number;
  oldState?: string;
  newState?: string;
}

/** `Contact/separate` (#816): one NEW contact per home among the named
 * addresses. */
export interface ContactSeparateResult {
  id: Id;
  created: Id[];
  addressContacts: Record<Id, Id>;
  keysMoved?: number;
  /** #834: every address of each merge the separated addresses came in by
   * was taken back, so those merges were undone on `id`. False for a partial
   * separate (nothing on `id` changed back). Absent from an older server. */
  restored?: boolean;
  /** #834: `id`'s fields a merge wrote that were changed after it, kept
   * rather than restored: displayName, userName, notes, isVip, isSaved,
   * isBlocked. */
  keptFields?: string[];
  oldState?: string;
  newState?: string;
}

/** Where a blocked sender's new mail goes (#59). */
export type BlockDestination = "junk" | "trash";

/** One row of the clean-up ranking (`SenderStats/get`, #53): a sender, a
 * domain or a mailing list. `neglected` counts mail left unread or archived
 * unread (internal/store/sender_stats.go defines it); `neglectShare` is
 * neglected / total. `addresses` lists at most 20 of `addressCount`, newest
 * first, each with its contact's markers. `unsubscribeEmailId` is a recent
 * message carrying List-Unsubscribe, or null. */
export interface SenderStat {
  key: string;
  displayName: string;
  total: number;
  unread: number;
  neglected: number;
  neglectShare: number;
  lastReceivedAt: string;
  addressCount: number;
  /** `name` is the contact's display name; `correspondent` is true when the
   * user has written to the address (sent mail or a reply). */
  addresses: Array<{
    address: string;
    contactId: Id | null;
    name?: string;
    isVip: boolean;
    isBlocked: boolean;
    correspondent?: boolean;
  }>;
  unsubscribeEmailId: Id | null;
  /** Whether the key searches for exactly this row (store.SenderKeyActionable);
   * a row the server marks false is never acted on. Absent from a server
   * before the flag existed. */
  actionable?: boolean;
}

export type SenderGroupBy = "address" | "domain" | "list";
export type SenderSortBy = "volume" | "neglect";

export interface SenderStatsResult {
  accountId: AccountId;
  groupBy: SenderGroupBy;
  sort: SenderSortBy;
  sinceDays: number | null;
  position: number;
  total: number;
  /** The window's start the ranking used (UTCDate), or null for all time —
   * passed back to SenderStats/emailIds so an action reads the same window. */
  since?: string | null;
  list: SenderStat[];
}

/**
 * The user's OWN OpenPGP key (`UserKey/get`, `urn:mailroom:pgp`, #374).
 *
 * ⚠ THE SHAPE HERE IS THE CLIENT SIDE OF A CONTRACT THE SERVER IS STILL
 * GROWING. The keys API is being built alongside this row; these four
 * properties are the ones the Keys row actually prints, and they are what the
 * design specifies. When the server half lands, this is the type to check
 * against it — a property named differently there would not fail loudly here,
 * it would read as `undefined` forever (the #155 shape), which is why the row
 * treats a missing fingerprint as "no key" rather than rendering a blank.
 */
export interface UserKey {
  id: Id;
  /** The identity this key signs for, or null for a login-wide key. */
  identityId: Id | null;
  kind: string;
  fingerprint: string;
  /** Published through WKD. The server's name is `wkdPublished` — spelled
   * exactly so the Keys row cannot read `undefined` as "not published" (#155). */
  wkdPublished: boolean;
  /** A retired key kept for decryption only (spec §9). */
  decryptOnly: boolean;
  createdAt: string;
  revokedAt: string | null;
  // ---- #795/#796: read from the key's packets at the call. OPTIONAL because a
  // server before v1.93 does not send them, and an absent flag must read as
  // "not said", never as false-and-therefore-fine.
  /** When the key stops being usable (UTCDate). Null = never, or unreadable. */
  expiresAt?: string | null;
  expired?: boolean;
  /** Not retired or revoked, and can still be encrypted to. */
  usable?: boolean;
  /** usable, and expiresAt is inside keyExpiryWarningDays. */
  expiringSoon?: boolean;
  /** Not retired or revoked, the private half held, primary not expired. */
  canSign?: boolean;
  /** "generated": Mailroom holds the only copy, so it is worth a backup. */
  origin?: "generated" | "imported" | null;
  /** The direct WKD URL for the bound fixed address; null when unbound. */
  wkdUrl?: string | null;
  /** This server answers WKD for that address's domain. */
  wkdServable?: boolean;
  // ---- #815/#817/#812 (v1.93 C1). OPTIONAL for the same reason as above.
  /** When the key stopped being used (retired or revoked, whichever came
   * first). Null while in use, AND for a key retired before v1.93: then it
   * was not recorded, and must never be shown as a date. */
  retiredAt?: string | null;
  /** The key UserKey/replace put in this one's place. */
  replacedBy?: Id | null;
  /** The key's self-signed user IDs, primary first. Untrusted text. */
  userIds?: KeyUserId[];
  /** Whether a valid user ID names the bound fixed address; null when
   * unbound or bound to a wildcard. */
  boundAddressListed?: boolean | null;
}

/** One OpenPGP user ID on a key (#812), as the server read it from packets
 * with a verifying self-signature. Untrusted text: render it as text only. */
export interface KeyUserId {
  name: string;
  email: string;
  revoked: boolean;
  primary: boolean;
}

/** What a successful UserKey/set update said back (#812). */
export interface UserKeyUpdated {
  boundAddressListed?: boolean | null;
  /** "addressNotInKey": the bind was made, but the key does not name it. */
  warning?: string;
}

/** `UserKey/replace` (#817). */
export interface UserKeyReplaceResult {
  id: Id;
  replacedId: Id;
  fingerprint: string;
  published: boolean;
  boundTo: Id | null;
  key: UserKey;
  oldKey: UserKey;
}

/** An exact or relative expiry for generate, extend and replace (#813).
 * `expiresAt` is a calendar date `YYYY-MM-DD`, which the server reads as the
 * END of that day in UTC. An empty object is the server's default. */
export type KeyExpiryArg = { expiresDays: number } | { expiresAt: string } | Record<string, never>;

/** One event in a correspondent key's history (#815), oldest first. */
export interface ContactKeyEvent {
  kind: string;
  source: string;
  at: string;
  /** The key's end of usability after the event; null = never or unreadable. */
  expiresAt: string | null;
}

/** `ContactKey/refresh` (#818). */
export interface ContactKeyRefreshResult {
  id: Id;
  result: string;
  checkedAt: string;
  key: HeldContactKey;
  addresses: { address: string; outcome: string }[];
  newKeyId?: Id;
  newFingerprint?: string;
}

/** `UserKey/get` in full (#796): the keys and the instance's WKD facts. */
export interface UserKeysResult {
  keys: UserKey[];
  /** This server publishes keys over WKD at all (MAILROOM_WKD_DOMAIN set).
   * Null when the server did not say (before #796). */
  wkdConfigured: boolean | null;
  wkdDomain: string | null;
}

/**
 * One correspondent key the server holds (`ContactKey/get`, #796). Never key
 * material. `createdAt` is when the key was MADE (from its packets; null when
 * unreadable) and `fetchedAt` when Mailroom last saw it.
 */
export interface HeldContactKey {
  id: Id;
  contactId: Id;
  fingerprint: string;
  kind: string;
  /** "manual" (imported), "wkd", "autocrypt" or "keyserver". */
  source: string;
  /** Autocrypt only means much when authenticated (DKIM-aligned). */
  authenticated: boolean;
  /** The user chose to keep an older key instead of this one. */
  refused: boolean;
  /** The encryption pin for at least one of the contact's addresses. */
  pinned: boolean;
  pinnedFor: string[];
  /** The addresses the key's own user IDs name. */
  addresses: string[];
  createdAt: string | null;
  fetchedAt: string;
  expiresAt: string | null;
  expired: boolean;
  revoked: boolean;
  usable: boolean;
  expiringSoon: boolean;
  hasEncryptionKey: boolean;
  /** #812: the key's self-signed user IDs, primary first. Untrusted text. */
  userIds?: KeyUserId[];
  /** #815: when the user removed it (only with includeRemoved). A removed key
   * is used for nothing, and comes back if it is seen again. */
  removedAt?: string | null;
}

/** RFC 8621 §4.1.4 EmailBodyPart. `partId`/`blobId` are null for (and only for)
 * a multipart/* part; `subParts` is present only inside `bodyStructure`. */
export interface EmailBodyPart {
  partId: string | null;
  blobId: BlobId | null;
  size: number;
  name: string | null;
  type: string;
  charset: string | null;
  /** RFC 3676 parameter: "flowed" marks a soft-wrapped text/plain body. */
  format?: string | null;
  /** RFC 3676 parameter: true when DelSp=yes drops soft-break spaces. */
  delSp?: boolean;
  disposition: string | null;
  cid: string | null;
  subParts?: EmailBodyPart[] | null;
}

/** RFC 8621 §4.1.4 EmailBodyValue: the decoded text of one body part. */
export interface EmailBodyValue {
  value: string;
  isEncodingProblem: boolean;
  isTruncated: boolean;
}

/**
 * RFC 8621 §4 Email, as Mailroom renders it (internal/api/email.go
 * `emailObject` + `body.go`).
 *
 * The body/recipient properties are OPTIONAL here because they are optional on
 * the wire: they can only be answered by decrypting and parsing the message's
 * blob, so `Email/get` returns them only when they are asked for. A list view
 * names the cheap properties (EMAIL_LIST_PROPERTIES) and gets none of them; the
 * reading pane asks for the body (EMAIL_BODY_PROPERTIES + fetch*BodyValues) and
 * pays for exactly the messages it opened.
 */
/** One reason behind a priority score (urn:mailroom:triage). `weight` is the
 * feature's signed contribution; `polarity` is its direction, so a UI never has
 * to infer "up" from a sign it might get backwards. */
export interface PriorityReason {
  label: string;
  weight: number;
  polarity: "up" | "down";
}

/**
 * One of the classifier's OPEN QUESTIONS about a message (urn:mailroom:rules,
 * #169/#176): a label it considered and was not confident enough to apply.
 *
 * Not a weak label. Nothing here affects keywords, mailboxes, or what any other
 * client sees — it is a question waiting for a person, and answering it goes
 * through Email/set like any other labelling.
 *
 * Only the id travels: labels are part of the ordinary sync, so the client
 * already holds every name and colour, and a second copy would disagree with the
 * first the moment one is renamed.
 */
/**
 * How to unsubscribe from a message's sender (urn:mailroom:unsubscribe, #58),
 * parsed from List-Unsubscribe (RFC 2369) by the SERVER.
 *
 * The parsing is deliberately not done here. The header is written by a stranger, and
 * deciding which scheme is safe to put in an `href` is a security decision that must be
 * made once, not twice in two languages — so the client receives a `url` already known
 * to be `https` with a host and no userinfo, or an empty string, and never sees the raw
 * header at all (internal/api/unsubscribe.go).
 *
 * At least one of the two is usable whenever this object is present.
 */
/**
 * One method's result keywords, as they appeared in Authentication-Results
 * (RFC 8601 §2.2) — lowercased, de-duplicated and sorted by the server.
 *
 * ⚠ AN ARRAY BECAUSE A METHOD CAN SAY MORE THAN ONE THING. A message carrying two
 * DKIM signatures, one valid and one not, genuinely holds both `pass` and `fail`,
 * and that is ordinary rather than contradictory. Collapsing the pair to either
 * member would invent a verdict the header does not contain.
 *
 * `null` means the method did not appear at all — which is not the same as
 * appearing and reporting nothing.
 */
export type AuthMethodResults = readonly string[] | null;

/**
 * What SPF, DKIM and DMARC said about one message (urn:mailroom:senderauth, #55).
 *
 * These are FACTS RELAYED, not checks Mailroom performed: the receiving servers ran
 * them and wrote the answers into the message. Mailroom parses that header and passes
 * on what it says.
 */
export interface AuthResults {
  /** False when the message carried no spf/dkim/dmarc token anywhere — it arrived
   * with no authentication at all. Every method below is then null. This is the
   * state a green "verified" badge would most dangerously paper over. */
  present: boolean;
  spf: AuthMethodResults;
  dkim: AuthMethodResults;
  dmarc: AuthMethodResults;
  /** Mailroom extension (#776, ADR-036): `"mailroom"` when THIS verdict is
   * Mailroom's own DKIM check at ingest rather than the provider's. Absent for
   * a provider verdict. Only ever sent for an aligned pass. */
  source?: "mailroom";
  /** The signing domain (d=) of the passing signature; sent with
   * `source: "mailroom"`. */
  dkimDomain?: string;
}

/**
 * One entry of `Email.senderWarnings` (urn:mailroom:senderauth, #397), exactly as
 * the wire sends it.
 *
 * `kind` is a plain `string`, not the closed union the client actually renders —
 * that union lives in core/senderWarnings.ts, which is the boundary that decides
 * what an unrecognized kind means (nothing, rather than a raw string rendered
 * inside a security notice). A type here that already narrowed `kind` would let a
 * server-added kind through the type checker and onto the page untranslated.
 */
export interface SenderWarningRaw {
  kind: string;
  /** The sending domain, exactly as the message presented it. */
  domain: string;
  /** lookalikeDomain only: the known domain it collides with. */
  resembles?: string;
  /** punycodeDomain only: the decoded form, when it decodes. */
  unicode?: string;
}

/**
 * One projected part's malware-scan state (urn:mailroom:attachmentscan,
 * #396/#412), exactly as `internal/api/attachment_scan.go`'s
 * `attachmentScanObject` renders it — a MAP keyed by partId, not a list. Every
 * part carries a `state`; `released` is reported even when `state` is
 * `"clean"`, because it is a fact about the user's OWN decision and must not
 * disappear the next time a re-scan happens to come back clean.
 *
 * ⚠ `state`, NOT `status` — this is a transcription of the server's actual JSON
 * key, not of RFC wording or a guess at naming. Getting this wrong reads
 * `undefined` for every part and fails silently (no property is missing, no
 * request errors — the reason text just never resolves), exactly the trap the
 * module header warns every type in this file to transcribe from the server.
 */
export interface AttachmentScanPart {
  state: "clean" | "flagged" | "unknown" | "unscanned";
  released: boolean;
  /**
   * When the user released it — a JMAP UTCDate — or null if they never did
   * (#449, server side #484).
   *
   * ⚠ NOT A SCANNER FACT, and that is why this one timestamp exists while the
   * verdict's own age does not: `internal/api/attachment_scan.go` withholds scan
   * age and signature-DB version deliberately (they churn on every re-scan pass),
   * whereas this records when the USER made a decision, so it changes only on a
   * release or a revoke. It is what the viewer and the tile print as
   * "released by you · <date>".
   *
   * OPTIONAL, not `string | null`, because a server that predates #484 sends no
   * such key at all and the fallback is a real rendering path ("released by you",
   * with no date) rather than a theoretical one. Reading a property that does not
   * exist on the wire is `undefined` forever and errors nowhere — the #155 trap
   * this file's header warns about — so the type states the absence instead of
   * assuming the field.
   */
  releasedAt?: string | null;
}

/**
 * `Email.attachmentScan` (urn:mailroom:attachmentscan, #396/#412): the scan
 * verdict for every projected part of one message, and which of them the
 * server will actually refuse to serve.
 *
 * `quarantined` is DERIVED SERVER-SIDE from the same `attachscan.Blocked` rule
 * the download gate itself calls — never re-derive "is this blocked" from
 * `state`/`released` on the client, or a future change to that rule (e.g. a new
 * scan state) silently disagrees between the chip and the gate.
 */
export interface AttachmentScan {
  parts: Record<string, AttachmentScanPart>;
  /** partIds the download route will answer 403 for, right now. */
  quarantined: string[];
}

export interface Unsubscribe {
  /** An https link, for the USER'S OWN BROWSER to open in a new tab. Empty when the
   * message offered no usable link. Never fetched by the server or by the app. */
  url: string;
  /** A mailto unsubscribe, already split into the parts a send needs. Sent through the
   * ordinary compose path from the user's own identity, so it opens no new door.
   *
   * `subject` is load-bearing, not decoration: senders key the unsubscribe on it, so a
   * message sent without it arrives and does nothing. */
  email: { to: string; subject: string; body: string } | null;
}

export interface SuggestedLabel {
  labelId: Id;
  /**
   * The review queue's ordering key — but NOT one uniform quantity (#242).
   *
   * For `source: "model"` this is the T1 classifier's calibrated [0,1]
   * probability: within the abstention band by construction, since a confident
   * yes became a label and a confident no was dropped.
   *
   * For `source: "rule"` it is an EVIDENCE RANK instead: a monotone function of
   * how many of the user's own filings back the candidate, written into this
   * same column purely because the ordering (`max(confidence) DESC NULLS
   * LAST`) is shared between both producers. It carries NO calibration — there
   * is no training procedure behind a rule to calibrate — and rendering it as
   * a percentage would claim a precision the rule path never earned. See
   * `confidencePercent`'s own doc comment (NeedsCategory.tsx) and check
   * `source` before formatting this as "N%".
   */
  confidence: number;
  /** Which producer wrote this suggestion (#242): the T1 classifier or the rule
   * inducer. This is what a renderer must switch on before touching
   * `confidence` — see the field's own doc comment. */
  source: "model" | "rule";
  /** WHY, in the SAME shape as priorityReasons (#176) — one explanation contract
   * for both models, so a client that can explain a ranking can explain a
   * category with no new code.
   *
   * `null` means NO RECORD of why (a suggestion older than the reasons column,
   * or a model that could not explain itself) and must not be rendered as "no
   * reasons found" — that is a claim the server never made. An empty array is
   * the different, weaker statement: it looked, and had nothing to say.
   *
   * For a rule-sourced suggestion, the LAST entry is the evidence phrase the
   * rule inducer wrote to justify the evidence rank above (e.g. "backed by 4
   * messages you filed under this label") — what a client shows in place of a
   * percentage it does not have (#242). */
  reasons: PriorityReason[] | null;
}

/**
 * A calendar time as the server resolved it (internal/api/calendar.go).
 *
 * - `utc`: an instant (`utc` set); shown in the READER's zone.
 * - `date`: an all-day date (`date` set).
 * - `floating`: a wall-clock time the server could not place in any zone
 *   (`local` set). Shown AS WRITTEN with `timeZone` named when there is one —
 *   never silently converted, because there is nothing to convert from.
 */
export interface CalendarTime {
  kind: "utc" | "date" | "floating";
  utc: string | null;
  date: string | null;
  local: string | null;
  timeZone: string | null;
}

export type CalendarPartStat =
  | "NEEDS-ACTION"
  | "ACCEPTED"
  | "TENTATIVE"
  | "DECLINED"
  | "DELEGATED";

export interface CalendarAttendee {
  name: string | null;
  email: string;
  partstat: CalendarPartStat;
  role: string;
}

/** Why an invitation offers no buttons — a closed set the client translates. */
export type CannotReplyReason =
  | "notAnInvitation"
  | "cancelled"
  | "noOrganizer"
  | "noUid"
  | "notAddressedToYou";

/** Email.calendarInvite (urn:mailroom:calendar, #725). Either a `problem`
 * (the calendar was there and could not be read — never confused with "no
 * calendar", which is null) or the invitation. */
export interface CalendarInvite {
  problem: "tooLarge" | "unreadable" | null;
  method?: "REQUEST" | "CANCEL" | "REPLY";
  uid?: string | null;
  sequence?: number;
  status?: string | null;
  summary?: string;
  location?: string | null;
  start?: CalendarTime | null;
  end?: CalendarTime | null;
  organizer?: { name: string | null; email: string } | null;
  attendees?: CalendarAttendee[];
  attendeesOmitted?: number;
  recurrence?: {
    freq: string | null;
    interval: number;
    count: number | null;
    until: CalendarTime | null;
    byDay: string[];
    text: string;
  } | null;
  recurrenceId?: CalendarTime | null;
  cancelled?: boolean;
  /** Whether THIS message is authenticated as coming from the person it speaks
   * for (the organiser; for a REPLY, the attendee) — From matches and a
   * trusted, aligned DMARC pass. An unverified cancellation, update or reply
   * still shows its own card, marked, and changes no other card. */
  verified?: boolean;
  cancelledByEmailId?: Id | null;
  newerEmailId?: Id | null;
  updatesEmailId?: Id | null;
  /** The user's own current answer to this event, or null. Undo Send takes it
   * back (the server excludes a cancelled submission). */
  response?: {
    partstat: "ACCEPTED" | "TENTATIVE" | "DECLINED";
    sequence: number;
    respondedAt: string;
    submissionId: Id | null;
    emailId: Id | null;
  } | null;
  canReply?: boolean;
  cannotReplyReason?: CannotReplyReason | null;
}

export interface Email {
  id: Id;
  blobId: BlobId;
  threadId: Id;
  mailboxIds: Record<Id, true>;
  keywords: Record<string, true>;
  /** Mailroom extension (urn:mailroom:labels, #704, ADR-031): the id-set of
   * EVERY label on the message, mirrored and local-only alike. A mirrored label
   * is also in `keywords` as its atom; a LOCAL-ONLY label — every area's own
   * label — has no atom and is visible only here. Settable with the patch form
   * `labelIds/<id>`. Optional so an older server simply reads as "unknown". */
  labelIds?: Record<Id, true>;
  size: number;
  receivedAt: string;
  sentAt: string | null;
  subject: string;
  preview: string;
  hasAttachment: boolean;
  from: EmailAddress[] | null;
  messageId: string[] | null;
  inReplyTo: string[] | null;
  references: string[];
  /** Mailroom extension (urn:mailroom:compose): the address this message was
   * delivered to — what a reply should come FROM (spec §5's catchall story). */
  deliveredTo: string | null;
  /** The verified identity `deliveredTo` resolved to, or null. */
  identityId: Id | null;
  /** Mailroom extension (urn:mailroom:organize, spec §6): pinned state, and
   * WHEN it was pinned. The timestamp is what lets a pinned-first ordering be
   * stable rather than arbitrary; a client that only wants the boolean can
   * ignore it. Optional so an older server (or a response that did not request
   * them) simply reads as unpinned. */
  isPinned?: boolean;
  pinnedAt?: string | null;
  /** Mailroom extension (urn:mailroom:organize, spec §6): when this message
   * comes back, and whether it is still away. `snoozedUntil` is the whole state
   * — the server derives `isSnoozed` from it against the current time, so a
   * message whose wake time has passed reads as NOT snoozed even before the
   * server's wake job has run. Clients should trust `isSnoozed` rather than
   * re-deriving it, so everyone agrees on the boundary. */
  isSnoozed?: boolean;
  snoozedUntil?: string | null;
  /** Mailroom extension (urn:mailroom:triage, spec §8): the T1 model's [0,1]
   * priority, null when this message has never been scored; `isImportant` is
   * that thresholded server-side. Read-only — the way to change the model's mind
   * is `Triage/correct`, not a patch. */
  priorityScore?: number | null;
  isImportant?: boolean;
  /** Why it ranked where it did: the top per-feature contributions, already
   * humanised. A ranking you cannot interrogate is one you cannot trust, so the
   * reading pane shows these rather than a bare score. */
  priorityReasons?: PriorityReason[] | null;
  /** Mailroom extension (urn:mailroom:organize, #152): when a snooze last
   * brought this message back, or null. Read-only. It is why a week-old message
   * can be sitting at the top of the inbox, and the list says so rather than
   * leaving the reader to conclude the sort is broken. */
  returnedAt?: string | null;
  /** Mailroom extension (urn:mailroom:rules, #155): who applied each of this
   * message's labels. Read-only, and body-only — it answers a question asked of
   * an open message. */
  labelProvenance?: LabelProvenance[] | null;
  /** Mailroom extension (urn:mailroom:rules, #155): the List-Id value, or null
   * when this is not list mail. It names the LIST rather than the sending
   * domain, which is what "create a rule from this mailing list" needs to
   * distinguish a retailer's newsletter from that retailer's receipts. */
  listId?: string | null;
  /** Mailroom extension (urn:mailroom:unsubscribe, #58): how to unsubscribe from
   * this sender, already parsed and validated server-side, or null when the
   * message offers no usable way.
   *
   * SURFACING ONLY. Mailroom never sends the unsubscribe itself: the `url` is for
   * the user's own browser to open, and `email` is sent through the ordinary
   * compose path from the user's own identity. Nothing here is fetched by the
   * server or by the app — see internal/api/unsubscribe.go for why that boundary
   * exists and what it refuses.
   *
   * Body-only: absent from the list-view property set. */
  unsubscribe?: Unsubscribe | null;
  /** Mailroom extension (urn:mailroom:senderauth, #55): what SPF, DKIM and DMARC each
   * said about this message, per method, as written down by the servers that handled it.
   *
   * ⚠ THREE STATES, AND THEY MEAN DIFFERENT THINGS.
   *  - `null` — the question does not apply: Mailroom wrote this message and it never
   *    crossed an authenticating hop, so there was never anything to report. Do NOT
   *    render this as unverified; it is the user's own outbox.
   *  - `{present: false, ...}` — it arrived from outside carrying no authentication at
   *    all. Nobody vouched for it. This is a real finding, not a missing value.
   *  - `{present: true, ...}` — the methods ran; read them.
   *
   * REPORTING ONLY, and deliberately NOT a verdict. The server already persists a
   * conservative pass/fail boolean and increment 0 measured what it hides: it reads
   * "no failure" on the operator's whole mailbox while DKIM has failed outright on 3
   * messages and DMARC evaluation errored on 6. Deciding what is worth showing is this
   * client's job — see senderAuthNotice. */
  authResults?: AuthResults | null;
  /** Mailroom extension (urn:mailroom:senderauth, #397): whether this message's
   * sending domain is trying to look like one the user already knows — a
   * lookalike, a punycode label, or a stranger claiming one of the user's own
   * domains. `null` means nothing to say, which is the overwhelming majority of
   * mail by measurement (see internal/senderwarn).
   *
   * A different question from `authResults`, on purpose: a lookalike domain
   * authenticates perfectly, because it belongs to the attacker. Deciding which
   * of these findings are worth showing, and in what order, lives in
   * core/senderWarnings.ts — away from the JSX, under test, for the same reason
   * senderAuthNotice does. Body-only: absent from the list-view property set. */
  senderWarnings?: SenderWarningRaw[] | null;
  /** urn:mailroom:pgp (#47 6b, ADR-037): the stored message is RFC 3156
   * PGP/MIME encrypted. Body-only. */
  encrypted?: boolean;
  /** `true`: the body properties are the decrypted plaintext (decrypted on the
   * server, on this request, never stored). `false`: it could not be decrypted
   * — no reason is given, by design. `null`: no decryption was attempted. */
  decrypted?: boolean | null;
  /** urn:mailroom:pgp (#47, ADR-039): the verdict on a PGP/MIME signature - on the
   * message, or inside its decrypted entity - or null when unsigned. Body-only. */
  pgpSignature?: PgpSignature | null;
  /** Mailroom extension (urn:mailroom:attachmentscan, #396/#412): the malware
   * scanner's verdict on every projected part of this message, or `null` for a
   * message with no projected parts (never a signal that everything is clean —
   * see AttachmentScan's own doc comment on why there is no all-clear summary).
   * Body-only, like senderWarnings: the reading pane is the only surface that
   * shows it, and loadBodies already parses the blob once for everything else
   * on this list. */
  attachmentScan?: AttachmentScan | null;
  /** Mailroom extension (urn:mailroom:calendar, #725): the invitation,
   * cancellation or reply this message carries, parsed on the server; null when
   * it carries no calendar. Body-only: the card is in the reading pane. */
  calendarInvite?: CalendarInvite | null;
  /** Mailroom extension (urn:mailroom:rules, #169): the classifier's open
   * questions about this message, or null when it has none — which is the
   * overwhelming majority of mail. Read-only; the way to answer is to apply a
   * label (Email/set) or to say "none of these"
   * (Email/dismissCategory). */
  suggestedLabels?: SuggestedLabel[] | null;
  /** Mailroom extension (urn:mailroom:desk, #444): when the user put this
   * message IN THE TRAY — i.e. when `$flagged` was set — or null when it is not
   * flagged.
   *
   * READ-ONLY and SERVER-STAMPED: the flag itself is set through the keyword,
   * and an `Email/set` patch naming this property is refused, so two clients
   * flagging the same message a minute apart cannot disagree about when it
   * happened. It is what the in-tray strip ages its rows by ("waiting 3 days"),
   * which is why it is in the LIST property set rather than the body one. */
  flaggedAt?: string | null;
  /** Mailroom extension (urn:mailroom:desk, #442): the area that claims this
   * message, or null when none does ("Needs sorting"). OPT-IN and body-only —
   * the desk's lists learn a message's area from the QUERY that returned it
   * (`inArea`), so paying for a per-message area resolution on every row
   * scrolled past would answer a question the list never asks. */
  area?: Id | null;
  /** Mailroom extension (urn:mailroom:desk, #714): who put this message in its
   * area — the `applied_by` of the resolved area's OWN label ("user", "rule",
   * "desk", "setup", "learned", ...), or null when it is in no area or is there
   * without its own label. OPT-IN and body-only, like `area`.
   *
   * It exists because an area's own label is LOCAL-ONLY (ADR-031) and so has
   * no keyword atom, which means `labelProvenance` (keyed by atom) never
   * described it: every area chip read "automatically". A string rather than a
   * union for `MessageAreaChip.provenance`'s reason. */
  areaProvenance?: string | null;
  /** Mailroom extension (urn:mailroom:desk, #443): when the drawer sweep
   * archived this message out of its area, or null when the sweep never touched
   * it. Read-only and opt-in, like `area`: it explains why a message is in
   * Archive without the user having filed it, which is a question asked of ONE
   * open message. */
  drawerArchivedAt?: string | null;
  /**
   * Mailroom extension (urn:mailroom:desk, #446): when DAY-ONE SETUP took this
   * message off the desk, or null when it never did.
   *
   * ⚠ NOT THE SAME FACT AS `drawerArchivedAt` ABOVE, and a client must not
   * conflate them (internal/api/setup_email.go says the same from the other
   * side). `drawerArchivedAt` is a real server-side MOVE to the Archive folder
   * that every other client sees; this one is "filed away in Mailroom with
   * NOTHING moved" — the bytes are exactly where the provider has them, the
   * message is still in its folder, and no other client saw anything happen.
   * Two properties because "your mail was moved" and "your mail was filed
   * without being moved" are different sentences to show a person.
   *
   * Read-only and opt-in server-side; requested in EMAIL_LIST_PROPERTIES,
   * because the surface that shows it is All mail — a LIST — see the note there.
   */
  deskArchivedAt?: string | null;
  /** Mailroom extension (urn:mailroom:desk, #464): the user's own placement of
   * this message relative to the desk's LINE — `"surface"`, `"drawer"`, or
   * `null` when they have not placed it and the score still decides.
   * Read-only and opt-in, like `area` and `drawerArchivedAt`: the way to
   * change it is `Triage/correct`, not a patch — `Email/set` refuses a patch
   * naming it. ⚠ It is the OVERRIDE, not the effective side: `null` does NOT
   * mean "surface". A client that wants the side a message actually renders
   * on asks `Email/query` with `onSurface`. */
  lineSide?: "surface" | "drawer" | null;

  // --- Fetched only when requested (see above). ---
  sender?: EmailAddress[] | null;
  to?: EmailAddress[] | null;
  cc?: EmailAddress[] | null;
  bcc?: EmailAddress[] | null;
  replyTo?: EmailAddress[] | null;
  bodyStructure?: EmailBodyPart;
  /** The parts to render as the text version of the body, in order. */
  textBody?: EmailBodyPart[];
  /** The parts to render as the HTML version of the body, in order. */
  htmlBody?: EmailBodyPart[];
  attachments?: EmailBodyPart[];
  /** Keyed by EmailBodyPart.partId. Empty unless a fetch*BodyValues argument
   * asked for values. */
  bodyValues?: Record<string, EmailBodyValue>;
}

/** The Email properties a LIST view needs. Naming them is what keeps scrolling
 * cheap: omitting `properties` selects RFC 8621 §4.2's default set, which
 * includes the body — and the body costs a blob decrypt + MIME parse per
 * message (internal/api/body.go). */
export const EMAIL_LIST_PROPERTIES = [
  "id",
  "blobId",
  "threadId",
  "mailboxIds",
  "keywords",
  // urn:mailroom:labels (#704). In the LIST set, beside `keywords`, because the
  // label menu and the desk's re-file act on whatever is SELECTED in a list,
  // and a local-only label (every area's own) is visible nowhere else. It also
  // has to be TRUE rather than merely present: putEmails merges projections, so
  // a property fetched only by the reading pane would go stale on every row
  // re-filed from the list. Cheap: it rides the label query Email/get already
  // runs for `keywords` (internal/store attachLabelKeywords).
  "labelIds",
  "size",
  "receivedAt",
  "sentAt",
  "subject",
  "preview",
  "hasAttachment",
  "from",
  "messageId",
  "inReplyTo",
  "references",
  "deliveredTo",
  "identityId",
  // urn:mailroom:organize (#51). Requested in the LIST properties, not just the
  // body ones: a pinned message has to be identifiable in the list itself —
  // that is the whole point of pinning — and pinnedAt is what makes the
  // pinned-first ordering stable rather than arbitrary.
  "isPinned",
  "pinnedAt",
  // Snooze, same extension (#51). The list needs both: `isSnoozed` decides
  // whether a row carries the "comes back later" cue, and `snoozedUntil` is
  // what that cue says.
  "isSnoozed",
  "snoozedUntil",
  "returnedAt",
  // urn:mailroom:triage (#154). isImportant drives the list cue and the Priority
  // view's bucket divider; priorityScore is what a threshold is applied to.
  // priorityReasons is NOT here — it is per-message detail the reading pane asks
  // for, and putting it in the list set would pull explanation payloads for every
  // row scrolled past.
  "isImportant",
  "priorityScore",
  // urn:mailroom:rules (#176). In the LIST set, unlike priorityReasons, for two
  // reasons. The needs-a-category view is a list view whose every row is about
  // its suggestions — the label being offered and how sure the model is are the
  // row's content, not detail behind it. And putting it here is what keeps it
  // TRUE: putEmails merges projections, so a property fetched only sometimes
  // goes stale rather than absent, and a message whose questions were answered
  // elsewhere would keep showing them until something reopened it.
  //
  // It is cheap: one batched query per Email/get (store.SuggestionsForMessages),
  // returning nothing at all for the mail that has no open question.
  "suggestedLabels",
  // urn:mailroom:desk (#444). In the LIST set because the IN-TRAY STRIP orders
  // and ages its rows by it: "waiting since" is the strip's whole content, and
  // it groups by how long each item has waited. Falling back to receivedAt would
  // age a message by when it arrived rather than by when the user picked it up,
  // which for anything flagged out of an old thread is simply a different
  // number. Cheap: one nullable timestamp column on the message row.
  "flaggedAt",
  // urn:mailroom:desk (#446): "archived in Mailroom" — day-one setup took this
  // message off the desk and nothing moved on the server.
  //
  // IN THE LIST SET, unlike `area` and `drawerArchivedAt`, because the surface
  // that shows it IS a list: All mail is where day-one's filed mail lives, and
  // the row has to be able to say why it is there without the reader opening it.
  // Putting it here is also what keeps it TRUE — putEmails merges projections, so
  // a property fetched only sometimes goes stale rather than absent, and a row
  // would keep claiming "archived in Mailroom" after an undo until something
  // re-opened it (the same argument suggestedLabels makes above).
  //
  // Cheap: one indexed `desk_archived_at IS NOT NULL` read per Email/get batch
  // (internal/store/setup.go DeskArchivedAt), which returns nothing at all for a
  // login that has never run setup.
  "deskArchivedAt",
];

/** The list properties plus everything the reading pane needs to display a
 * message and reply to it. */
export const EMAIL_BODY_PROPERTIES = [
  ...EMAIL_LIST_PROPERTIES,
  // The explanation is fetched with the message the user actually opened.
  "priorityReasons",
  // urn:mailroom:rules (#155): who applied each label. Body-only for the same
  // reason as priorityReasons — "why is this label here?" is a question asked of
  // ONE open message, and carrying an attribution array for every row scrolled
  // past would pay for an answer nobody asked for.
  "labelProvenance",
  // Same extension: what "create a rule from this mailing list" keys on. Only
  // needed for the message someone is actually looking at.
  "listId",
  // urn:mailroom:unsubscribe (#58): body-only for the same reason. "How do I get
  // off this list?" is asked of the message in front of you, and most real mail
  // carries the header — putting a URL on every row scrolled past would ship a
  // sender-controlled string into the list view to answer a question nobody asked.
  "unsubscribe",
  "authResults",
  // urn:mailroom:senderauth (#397): same body-only reasoning as authResults —
  // the reading pane is the only surface that shows it, and loadBodies already
  // parses the blob once for everything else on this list.
  "senderWarnings",
  // urn:mailroom:pgp (#47 6b): asked for WITH the body, so the server decrypts
  // only the message the user opened (it never decrypts for a list).
  "encrypted",
  "decrypted",
  "pgpSignature",
  // urn:mailroom:attachmentscan (#396/#412): opt-in, same body-only reasoning —
  // the reading pane is the only surface that shows it, so asking for it on
  // every row scrolled past would cost a batched quarantine-state read nobody
  // asked for.
  "attachmentScan",
  // urn:mailroom:desk (#442/#375 increment 5): which AREA claims this message.
  // Opt-in and body-only server-side (internal/api/area_email.go resolveAreas
  // returns nothing unless `properties` names it), and body-only here for the
  // same reason: the desk's lists learn a message's area from the QUERY that
  // returned it (`inArea`), so the only surface that has to ask per message is
  // the reading pane, where the area is printed as a chip on the message.
  "area",
  // urn:mailroom:desk (#714): who put it in that area, for the same chip.
  "areaProvenance",
  // urn:mailroom:desk (#464): the user's own placement relative to the line,
  // when they made one. Body-only for the same reason as `area` — the only
  // surface that says "you placed this yourself" is the reading pane, and
  // the desk's lists already know a message's effective side from the query
  // that returned it (`onSurface`) without asking per message.
  "lineSide",
  // urn:mailroom:calendar (#725): the invitation card. Body-only — it costs a
  // calendar-part search of the blob loadBodies already parsed, and only the
  // reading pane shows it.
  "calendarInvite",
  "sender",
  "to",
  "cc",
  "bcc",
  "replyTo",
  "textBody",
  "htmlBody",
  "attachments",
  "bodyValues",
];

/** Cap on a single body value (1 MiB). A message with a bigger body than this
 * is pathological, and the server flags what it cut with `isTruncated`. */
export const MAX_BODY_VALUE_BYTES = 1 << 20;

/** The JMAP system keywords Mailroom recognizes (internal/store keyword atoms). */
export const KEYWORD_SEEN = "$seen";
export const KEYWORD_FLAGGED = "$flagged";
export const KEYWORD_ANSWERED = "$answered";
export const KEYWORD_DRAFT = "$draft";
export const KEYWORD_JUNK = "$junk";

/** RFC 8621 §3.1 Thread (internal/api/thread.go). */
export interface Thread {
  id: Id;
  emailIds: Id[];
  /** Mailroom extension (urn:mailroom:organize, spec §6): the user has asked us
   * to stop surfacing this conversation. Thread-scoped, because that is the
   * unit people actually mean by "mute". */
  isMuted?: boolean;
}

/** `urn:mailroom:labels` Label (internal/api/label.go). */
export interface Label {
  id: Id;
  name: string;
  color: string | null;
  parentId: Id | null;
  mirrorToImap: boolean;
  /** The server-assigned IMAP mirror atom (`MailroomLabel-<slug>`), or null for
   * a local-only label. It is what appears in Email.keywords. */
  keyword: string | null;
  /** `urn:mailroom:categorytabs` (#219): which category tab this label's mail
   * feeds, or null when the label is unmapped — in which case its mail is
   * classified from the message's own function signals, exactly as unlabelled
   * mail is. Local-only, like `color` and `parentId`: there is no IMAP concept
   * to mirror it onto. Tabs are message FUNCTION and labels are TOPIC; the two
   * are orthogonal axes, so a label has a tab rather than being one. */
  tab: CategoryTab | null;
}

/** The closed category-tab vocabulary (`app.category_tab`, internal/tabs). The
 * server advertises it in the `urn:mailroom:categorytabs` session capability, so
 * a client can render the strip without hard-coding this list — but the type is
 * here because a mapping control has to name the values it offers. */
export type CategoryTab = 'primary' | 'social' | 'promotions' | 'updates' | 'forums';

/** `urn:mailroom:categorytabs` CategoryTab (internal/api/category_tab.go): one
 * tab's per-user state. The set is FIXED — a client enables, disables and
 * reorders these five, and neither creates nor destroys them. `primary` cannot
 * be disabled: mail from a switched-off tab appears there. */
export interface CategoryTabState {
  id: Id;
  tab: CategoryTab;
  enabled: boolean;
  /** Where it sits in the strip. Sort on this, not on the capability's order,
   * which is only the default a fresh account starts from. */
  position: number;
}

// ---- The desk (urn:mailroom:desk, internal/api/area.go + desk_preferences.go)
//
// The desk is the user's own PARTITION of the inbox (#375/#442): every
// unactioned message sits in exactly one AREA and on exactly one side of the
// LINE. Areas are the third axis — tabs say what mail DOES, labels say what it
// is ABOUT, an area says WHERE ON THE DESK IT GOES — and the line
// (DeskPreferences.lineThreshold) decides whether it is on the surface (asking
// for a decision) or in that area's drawer (kept, not shown).

/** `urn:mailroom:desk` Area (internal/api/area.go areaObject): one region of
 * the desk.
 *
 * AN AREA OWNS A LABEL. `labelId` is immutable and placing a message in the
 * area IS applying that label ("areas are labels"), which is what makes the
 * arrangement survive in any other mail client. `tabs` and `labelIds` are the
 * ADDITIONAL feeds that also route mail here; `labelIds` never repeats
 * `labelId`, so a client must not present the own label as a removable feed. */
export interface Area {
  id: Id;
  name: string;
  /** Where it sits on the desk, ascending. The server orders Area/get and
   * Area/getSummary by it; a client that re-sorts must sort on this. */
  position: number;
  /** The area's OWN label. Immutable, and it survives the area's destruction —
   * every message that carried it keeps it. */
  labelId: Id;
  /** Category tabs whose mail lands in this area. Always present, possibly
   * empty, never null. */
  tabs: CategoryTab[];
  /** OTHER labels that also feed this area, EXCLUDING `labelId`. Always
   * present, possibly empty, never null. */
  labelIds: Id[];
  /** Whether this area's DRAWER sweeps at all (#443); default true. "Off" is
   * this being false — never `autoArchiveDays: null`. */
  autoArchive: boolean;
  /** The sweep PERIOD in days, or null meaning "the login default"
   * (`DeskPreferences.drawerDefaultDays`). Null has never meant "off". */
  autoArchiveDays: number | null;
  /** The user has hidden this area from the RAIL. It still has its sections on
   * the desk and its mail still resolves here — only the navigation entry goes
   * away — so nothing may treat this as "the area does not exist". */
  hiddenFromRail: boolean;
}

/** One row of `Area/getSummary` (#375/#442): how much mail an area holds, split
 * by which side of the line it is on.
 *
 * The three counts are DISJOINT (internal/store AreaSummary counts them with
 * mutually exclusive FILTERs), so they add up and must not be double-counted:
 * `surface` is scored at or above the line, `drawer` is scored below it, and
 * `unscored` is mail the model has never judged — which the desk shows on the
 * surface, because unjudged mail is never hidden. */
export interface AreaSummaryRow {
  /** The area, or null for the trailing "Needs sorting" row: mail no area
   * claims. That row is always last, and ALL of it belongs on the surface —
   * there is no Needs-sorting drawer, because nothing sweeps unsorted mail. */
  areaId: Id | null;
  surface: number;
  drawer: number;
  unscored: number;
  /** How many messages the drawer sweep has archived out of this area, ALL
   * TIME (#443) — independent of the threshold, and always 0 on the
   * Needs-sorting row. */
  archivedByDrawer: number;
}

/** The `Area/getSummary` answer: the desk's counts for one scope. */
export interface AreaSummaryResult {
  accountId: AccountId;
  /** The Area change state these counts were taken at. */
  state: StateString;
  /** The line the counts were actually taken at — the stored `lineThreshold`
   * when the request omitted one, or the PREVIEW threshold it supplied. Read it
   * rather than assuming: it is the only thing that says which question these
   * numbers answer. */
  threshold: number;
  /** One row per area in position order, then the `areaId: null` row. */
  list: AreaSummaryRow[];
}

/** One section of an `Area/getDesk` answer (#665): what that section's
 * thread-collapsed `Email/query` would have returned. */
export interface DeskSectionResult {
  /** The area, or null for Needs sorting. */
  areaId: Id | null;
  position: number;
  /** Collapsed total — threads, not messages, as the rows are. */
  total: number;
  ids: Id[];
  /** Present only when the server answered with a smaller window than asked
   * (RFC 8620 §5.5). */
  limit?: number;
}

/** `Area/getDesk` (#665): `Area/getSummary`'s answer plus every requested
 * section, taken in one transaction from one area resolution. */
export interface AreaGetDeskResult extends AreaSummaryResult {
  /** The Email state the sections were taken at — Email/query's queryState. */
  queryState: StateString;
  mailAccountId?: Id;
  /** The requested sections that exist, in request order. */
  sections: DeskSectionResult[];
  /** Requested area ids that name no area (destroyed elsewhere). */
  notFound: Id[];
}

/** `urn:mailroom:desk` DeskPreferences (internal/api/desk_preferences.go): the
 * ONE record per login that says where the line is. A singleton — `id` is the
 * user id, `set` accepts only `update`, and creating or destroying it is
 * refused. */
export interface DeskPreferences {
  /** Always the user's own id (= the JMAP account id). */
  id: Id;
  /** THE LINE, in [0,1]: a triage score at or above it is on the surface, below
   * it is in a drawer. Moving it moves mail, which is why it is written from
   * the server's answer rather than optimistically. */
  lineThreshold: number;
  /** The default drawer sweep period, in days, for an area whose own
   * `autoArchiveDays` is null. */
  drawerDefaultDays: number;
  /** After how many days a message sitting in the tray is nagged about. */
  trayNagDays: number;
  /** The drawer sweep's login-level MASTER SWITCH (#443), default FALSE. While
   * it is off nothing is swept, whatever any area's `autoArchive` says — so a
   * UI that promises "auto-archives after N days" must consult this first, or
   * it is describing something that does not happen. */
  drawerSweepEnabled: boolean;
  /**
   * When this login finished DAY-ONE SETUP (#446), or null for one that never
   * has — which is how the client decides to open the WIZARD rather than the
   * desk (`ui/setup/openSetup.ts`).
   *
   * ⚠ `null` AND `undefined` ARE DIFFERENT ANSWERS HERE, and the wizard's gate
   * tests for `null` exactly. The server always sends the property (migration
   * 00050 backfilled every login that already had preferences), so `null` means
   * "this login has not been through setup". `undefined` can only come from a
   * server that predates #446, where the correct behaviour is to leave the user
   * on their desk rather than open a wizard against a server with no `Setup/*`
   * at all — fail closed.
   *
   * Settable to a UTCDate or back to null; "Skip setup, open the desk" writes
   * the date exactly as "Open the desk" does, because both are the user saying
   * they are done being asked.
   */
  setupCompletedAt: string | null;
  /** LEARNED PLACEMENT's off switch (#714), default TRUE: when on, a new
   * message from a sender the user has consistently filed into one area is
   * filed there at arrival. The SERVER enforces it at ingest, so switching it
   * off here stops it for every client. Optional because a server before #714
   * does not send it; absent reads as "not available", never as on. */
  learnedPlacementEnabled?: boolean;
  /** QUIET HOURS (#59), per login and server-enforced: while in force only VIP
   * pushes go through (and only with `allowVip`). Optional for a server before
   * #59 — absent means the setting does not exist there. */
  quietHours?: QuietHours;
  /** FOLLOW-UP NUDGES (#58). Optional for a server before #58 — absent means
   * the feature does not exist there, and nothing is drawn for it. */
  followUp?: FollowUpSettings;
}

/** The follow-up nudge settings (internal/api/desk_preferences.go
 * followUpObject). `outbound` is "no one answered your mail" (default on);
 * `inbound` is "you haven't replied to this" (default OFF — opt-in, because
 * its measured precision is low); `inboundDays` is its delay, 1-14 (default
 * 3). The server wants the whole object on every write. */
export interface FollowUpSettings {
  outbound: boolean;
  inbound: boolean;
  inboundDays: number;
}

/** One due follow-up nudge (FollowUp/get, internal/api/follow_up.go). */
export interface FollowUpItem {
  emailId: Id;
  threadId: Id | null;
  /** `outbound`: no one answered the user's mail. `inbound`: the user has
   * not replied. */
  kind: "outbound" | "inbound";
  /** When the message was sent or arrived (UTCDate) — the "<day>". */
  since: string;
  /** When the nudge became due (UTCDate). */
  dueAt: string;
  subject: string;
  /** Outbound: who it went to. Inbound: who sent it. */
  who: { name: string; email: string };
}

/** FollowUp/get's answer. `truncated` names any read bound that was hit and
 * `unreadable` counts messages whose bytes could not be read — so "nothing
 * due" is distinguishable from "nothing could be read". */
export interface FollowUpResult {
  accountId: Id;
  list: FollowUpItem[];
  settings: FollowUpSettings;
  truncated: string[];
  unreadable: number;
}

/** The quiet-hours setting (internal/api/desk_preferences.go quietHoursObject).
 * `start`/`end` are minutes after local midnight in `timeZone` (IANA); `days`
 * are the weekdays a quiet period STARTS on, 0 = Sunday. The server wants the
 * whole object on every write. */
export interface QuietHours {
  enabled: boolean;
  start: number;
  end: number;
  days: number[];
  timeZone: string;
  allowVip: boolean;
}

/** `urn:mailroom:desk` ClientPreferences (internal/api/desk_preferences.go):
 * display preferences per CLIENT FAMILY, so a phone and a desktop can disagree
 * about density without fighting over one record.
 *
 * ⚠ THE THREE ROWS ALWAYS EXIST, AND A CLIENT NEVER CREATES ONE.
 * `ClientPreferences/get` MATERIALISES all three families before it answers
 * (clientPreferencesGet calls GetClientPreferences per family, which inserts on
 * first contact and appends the `created` change row), and
 * `clientPreferencesSet` REFUSES `create` outright — "the client families are a
 * fixed set; update one instead of creating a row". So the only legal write is
 * `update` keyed on the id this app read back from `/get`, and there is no
 * first-run case where that id does not exist. */
export interface ClientPreferences {
  /** One row per family; the id is per-row, not the user id. */
  id: Id;
  family: "web" | "phone" | "desktop";
  /** Narrowed to the server's own closed vocabularies (internal/api/desk.go
   * deskThemes / deskDensities): a `set` naming anything else is refused, so a
   * wider type here would promise a value the server does not take. */
  theme: "system" | "light" | "dark";
  /** ⚠ NOT this app's own density vocabulary, which is comfortable / cozy /
   * compact (app/prefs.ts). Only "compact" is spelled the same; the mapping
   * between them lives in prefs.ts (densityFromServer/densityToServer) and is
   * tested, because a value handed straight across is refused for two of the
   * three values and accepted for one. */
  density: "compact" | "normal" | "relaxed";
  /** Whether opening a message shows it BESIDE the list or replaces it. */
  messageOpens: "beside" | "focused";
  /** The reading-pane split, as a whole percentage of the window. */
  paneSplitPct: number;
  /** The whole client's text scale (#514, migration 00052). FIVE STEPS,
   * SMALLEST FIRST — the server advertises them in that order
   * (`urn:mailroom:desk`'s `textSizes`) and app/prefs.ts's TEXT_SIZES is the
   * same array, so nothing has to decide for itself which end is bigger.
   *
   * ⚠ UNLIKE `density`, THESE ARE THE SAME WORDS ON BOTH SIDES. There is no
   * mapping and deliberately no mapping function: the client's vocabulary IS
   * the server's, so a value can be handed straight across. */
  textSize: "xs" | "s" | "m" | "l" | "xl";
  /** How a row of mail is drawn: `oneLine` (sender, subject, then the snippet
   * in whatever room the subject leaves) or `multiLine` (sender and time,
   * subject, snippet). Server-exact, like textSize. */
  rowLayout: "oneLine" | "multiLine";
}

// ---- day-one setup (urn:mailroom:desk, #446) -------------------------------
//
// THE FOUR STEPS THAT TURN A FOLDER-SORTED MAILBOX INTO A DESK, and the shape of
// the promise they make: THREE OF THE FIVE METHODS WRITE NOTHING.
// `Setup/census`, `Setup/preview` and `Setup/get` are reports; `Setup/apply` is
// the only door that writes and `Setup/undo` is the way back out. Every field
// below is spelled exactly as internal/api/setup.go spells it — a wrong name
// here compiles, reads fine, and is `undefined` forever (#155).
//
// ⚠ TWO ACCOUNT IDS, AND THEY ARE NOT THE SAME ONE. `accountId` in the ARGS is
// the LOGIN, as everywhere else in this client; `mailAccountId` narrows a census
// to one mail account, and `FolderCensus.accountId` is the MAIL account whose
// folder the row is (internal/api/setup.go folderCensusObject). Confusing them
// yields an empty census rather than an error.

/** `classifyShape`'s word for a folder (internal/store/setup_census.go).
 *
 * ⚠ A WORD FOR THE COUNTS BESIDE IT, NEVER AN AUTHORITY. Every boundary behind
 * it (one domain, five senders, 80% bulk) is a judgement, not a measurement, so
 * the row renders it WITH `distinctSenders` / `sendersFor90` / `bulkShare` and
 * nothing may branch on the word alone. */
export type FolderShape = "oneDomain" | "fewSenders" | "bulk" | "mixed" | "empty";

/** One folder of `Setup/census`: everything the wizard needs to propose an
 * action for it. Special-use folders (Sent, Drafts, Archive, Junk, Trash) are
 * NOT rows — they are mapped without asking — and INBOX IS one, because its
 * backlog is usually the largest single pile. */
export interface FolderCensus {
  mailboxId: Id;
  /** The MAIL account this folder belongs to — not the login. */
  accountId: Id;
  /** The display name, and by default the LABEL day-one proposes for its mail:
   * the user already chose that word. */
  name: string;
  /** `""` for a plain folder, `"inbox"` for INBOX. */
  role: "" | "inbox";
  feedsDesk: boolean;
  /** The population every other count here is against: live mail that is not
   * junk, trashed, vanished or a draft. `inWindow + older === live`, always. */
  live: number;
  /** Arrived inside the window — untouched by design, so this is what stays on
   * the desk. */
  inWindow: number;
  /** `live - inWindow`: the mail day-one would act on. */
  older: number;
  /** How many of `live` carry no From address at all, so nothing can be said
   * about their sender. Counted, never hidden. */
  unclassified: number;
  distinctSenders: number;
  distinctDomains: number;
  /** The fewest senders whose mail covers 90% of `live` — the concentration
   * figure that survives a long tail. `0` means "no answer" (no From addresses
   * to concentrate), never "one sender". */
  sendersFor90: number;
  /** The share of `live` carrying List-Unsubscribe, in [0,1]. */
  bulkShare: number;
  shape: FolderShape;
  /** A label that already has this folder's name, so day-one reuses it instead
   * of minting a second label meaning the same thing. Null when there is none. */
  existingLabelId: Id | null;
  /** An area that already has this folder's name — the plan feeds it rather than
   * creating a duplicate. Null when there is none. */
  existingAreaId: Id | null;
}

/** The `Setup/census` answer. `windowDays` is the window the counts were taken
 * at — read it rather than assuming, exactly as with `AreaSummaryResult.threshold`. */
export interface SetupCensusResult {
  accountId: AccountId;
  windowDays: number;
  folders: FolderCensus[];
}

/**
 * What day-one does with one folder's OLDER mail.
 *
 * `labelArchive` — label it and take it off the desk, with NOTHING moved on the
 * server. `bringToDesk` — a real move to that account's INBOX, with the
 * writeback a person's own move would make (refused on INBOX itself:
 * `invalidArguments`). `leave` — import it later.
 */
export type SetupAction = "labelArchive" | "bringToDesk" | "leave";

/** One folder's line of the plan. */
export interface SetupFolderChoice {
  mailboxId: Id;
  action: SetupAction;
  /** The label this folder's older mail gets; defaults server-side to the
   * folder's own name, and is IGNORED for INBOX (whose label is each message's
   * own resolved area label — there is no single word for "the inbox"). */
  labelName?: string;
  /** An EXISTING area to feed. */
  areaId?: Id;
  /** Create — or reuse by name — an area owning this folder's label. Both this
   * and `areaId` absent means label only, which is a legitimate choice for a
   * folder that is an archive rather than a topic. */
  areaName?: string;
}

/** Step 2's whole decision: the window, and a line per folder. A census folder
 * ABSENT from `folders` is `leave`. */
export interface SetupPlan {
  windowDays: number;
  folders: SetupFolderChoice[];
}

/** One folder's line of the preview. `older` is the folder's share of the
 * population — the denominator the other three are counted against. */
export interface SetupFolderCounts {
  mailboxId: Id;
  name: string;
  action: SetupAction;
  older: number;
  labels: number;
  archived: number;
  moves: number;
}

/**
 * What applying a plan would write — PER FOLDER as well as in total, which is
 * the whole point (CLAUDE.md): one number cannot tell "this plan does nothing
 * because the window covers everything" from "this plan does nothing because
 * everything is already labelled".
 */
export interface SetupCounts {
  labels: number;
  archived: number;
  /** The ONLY number here that changes anything on the provider, and 0 for a
   * plan with no `bringToDesk` folder. Zero is kept in the sentence rather than
   * dropped — it is the reassurance. */
  moves: number;
  /** Older messages the plan deliberately does not touch: the `leave` folders
   * plus every census folder the plan omits. */
  leave: number;
  /** How many of the messages the plan WOULD touch carry no From address. */
  unclassified: number;
  perFolder: SetupFolderCounts[];
  /** A STATED PLANNING RATE, not a measurement (internal/store/setup.go
   * setupWritesPerSecond). Rendered as "about N minutes", never as a countdown. */
  estimatedSeconds: number;
}

export interface SetupPreviewResult {
  accountId: AccountId;
  counts: SetupCounts;
}

/** `Setup/apply`: the run it created, and the counts it RECORDED AS PLANNED —
 * re-previewed inside the same transaction rather than echoed from the client,
 * so "planned 4,404, wrote 4,404" is a check rather than a tautology. */
export interface SetupApplyResult {
  accountId: AccountId;
  runId: Id;
  counts: SetupCounts;
}

export type SetupRunStatus = "queued" | "running" | "done" | "failed" | "undoing" | "undone";

/**
 * One day-one run (`Setup/get`). READ-ONLY over JMAP: a client cannot create one
 * with a `/set`, because creating a run also mints labels and areas and enqueues
 * a job — `Setup/apply` is the one door.
 *
 * PLANNED BESIDE ACTUAL, deliberately both: it is the only form in which the
 * preview's promise can be checked rather than trusted.
 */
export interface SetupRun {
  id: Id;
  kind: string;
  status: SetupRunStatus;
  windowDays: number;
  plan: SetupPlan;
  plannedLabels: number;
  labelsWritten: number;
  plannedArchived: number;
  archived: number;
  plannedMoves: number;
  moved: number;
  leftForLater: number;
  unclassified: number;
  /** The failure, or null. A run with `status: "failed"` and no message here is
   * still a failure — the status is the fact, this is the explanation. */
  error: string | null;
  createdAt: string;
  startedAt: string | null;
  finishedAt: string | null;
  undoneAt: string | null;
}

export interface SetupGetResult {
  accountId: AccountId;
  state: StateString;
  list: SetupRun[];
  notFound: Id[];
}

/** The Mailroom method error for "a run is already outstanding" and "that run
 * has not finished yet". Its own type rather than `invalidArguments` because the
 * remedy differs in kind: the request was well formed and would be valid later,
 * so the client shows the run that is already going instead of a validation
 * message. */
export const ERR_SETUP_RUN_BUSY = "setupRunBusy";

// ---- T0 rules (urn:mailroom:rules, internal/rules) -------------------------
//
// The engine has been filtering mail since 1.0 with no client at all (#155).
// These types mirror internal/rules/rules.go exactly, because they (de)serialize
// straight into the `conditions` and `actions` jsonb columns — a mismatch here
// does not fail loudly, it produces a rule the server stores and the evaluator
// silently never matches.

/** A condition field selector. `header` additionally requires `header`. */
export type RuleField =
  | "from"
  | "fromDomain"
  | "to"
  | "subject"
  | "alias"
  | "listId"
  | "listUnsubscribe"
  | "header";

/** A condition operator. `exists` ignores `value`. */
export type RuleOp = "equals" | "contains" | "matches" | "exists";

/**
 * One node of the boolean condition tree: EITHER a group (`all` or `any`,
 * nestable) OR a leaf (`field`). A node that is neither matches nothing — the
 * server's safe default is that a malformed rule does nothing rather than
 * acting on all mail.
 */
export interface RuleCondition {
  all?: RuleCondition[];
  any?: RuleCondition[];
  field?: RuleField;
  op?: RuleOp;
  value?: string;
  /** The header name, when field is "header". */
  header?: string;
}

/**
 * A rule action. `moveToMailbox` and `junk` are DEFINED by the server but
 * rejected at save (they need placement/IMAP-writeback work T0 does not do), so
 * this union deliberately omits them — the editor must not offer what cannot be
 * saved.
 */
export type RuleActionType = "addLabel" | "markPriority" | "flag" | "skipInbox" | "archive" | "stop";

export interface RuleAction {
  type: RuleActionType;
  /** The target for addLabel: a label id, a keyword atom, or a label name. */
  label?: string;
}

/** A stored T0 rule (internal/api/rule.go). */
/** A remembered per-sender remote-content decision (#202,
 * urn:mailroom:remotecontent). `kind` is "address" (this exact From) or "domain"
 * (anyone at it); there is no wildcard and no "all" — the server's schema cannot
 * express one. */
/** A saved signature or template (#56, urn:mailroom:composesnippets).
 *
 * One type for both, because they are the same thing wearing two hats: named body
 * text the user wrote once and reuses. A signature is appended and may be bound to
 * an identity (the signature for shop@ is not the one for work@); a template seeds a
 * whole body and is never bound to one. */
export interface ComposeSnippet {
  id: Id;
  kind: "signature" | "template";
  /** Only ever set for a signature: the identity it is offered for. null means any. */
  identityId: Id | null;
  name: string;
  bodyHtml: string;
  bodyText: string;
  /** The one offered without asking. At most one per kind per identity, enforced by
   * the server's schema rather than by client bookkeeping. */
  isDefault: boolean;
}

/**
 * A saved search (#57, urn:mailroom:savedsearches) — and, when `inSidebar`, what the
 * feature matrix calls a smart folder. One object and a boolean, not two things.
 *
 * `query` is the OPERATOR STRING as typed, not a parsed filter. Running one means putting
 * that string into an `Email/query` filter, which is exactly what the search box does with
 * text the user typed — so a saved search cannot match different mail than the same text
 * typed by hand. There is deliberately no `SavedSearch/query` method, because a second way
 * to run a search is a second thing that could disagree.
 */
export interface SavedSearch {
  id: Id;
  name: string;
  query: string;
  /** Pinned to the nav rail. Saved-but-unpinned is the deliberate variant, reachable
   * from the search box. */
  inSidebar: boolean;
  /** Sidebar order; ties break by name server-side so the order is total. */
  position: number;
}

export interface RemoteAllow {
  id: Id;
  kind: "address" | "domain";
  value: string;
}

export interface Rule {
  id: Id;
  name: string;
  /** Evaluation order, lower first. */
  priority: number;
  conditions: RuleCondition;
  actions: RuleAction[];
  enabled: boolean;
  systemSuggested: boolean;
  /**
   * How many messages the user has taken this rule's label back off (#170).
   *
   * Server-set and read-only — patching it is refused. A rule that keeps being
   * corrected is a rule that is wrong, and this is the only number that says so:
   * before it existed, removing a label deleted the row and its attribution
   * together, so the rule went on firing and the disagreement went nowhere.
   */
  corrections: number;
}

/**
 * What a set of conditions does to the scanned window (#170).
 *
 * The current rule and every proposal come back in this SAME shape, because the
 * only useful reading of a proposal is against the thing it would replace —
 * "matches 31" means nothing until you know the rule it replaces matches 47.
 */
export interface RuleScope {
  matched: number;
  /** Matched mail still carrying the label — what the rule gets right. */
  keeps: number;
  /** Correctly labelled mail a proposal would stop matching: the PRICE of
   * tightening. Always 0 for the current rule, by construction. */
  drops: number;
  /** Matched mail the user corrected. The current rule's is why you are here; a
   * proposal's is always 0, because one that still catches a rejected message is
   * not offered at all. */
  misfires: number;
  /** Matched mail nobody has judged. A blast radius, NOT an error count. */
  collateral: number;
}

/** One narrower form of an existing rule (#170). */
export interface TightenedRule extends RuleScope {
  conditions: RuleCondition;
  /** Corrected messages this form no longer matches. */
  fixes: number;
  conflicts: number;
  conflictLabels: string[];
  /** The messages this form would DROP. Showing what it still catches would
   * confirm only what is guaranteed; what you need before accepting a narrower
   * rule is what you LOSE. */
  samples: RulePreviewSample[];
}

/**
 * What Rule/apply reports (#191) — running a saved rule over mail that already
 * arrived.
 *
 * `changed` is the number to show, not `matched`: matched includes mail that
 * already carried everything the rule applies, and reporting that as "labelled
 * 47" would claim work that did not happen.
 */
export interface RuleApplyResult {
  ruleId: Id;
  matched: number;
  changed: number;
  /** Matched mail the rule would have changed nothing about — a previous apply,
   * or the user's own filing. Without it a second run reads as unexplained
   * silence rather than "nothing left to do". */
  alreadyDone: number;
  /** Matched mail carrying a label this rule applied and the user REMOVED
   * (#170), never re-applied. Reported loudly: it is the number that says the
   * corrections were respected. */
  skippedCorrected: number;
  /** Rule actions a backfill will not perform — `skipInbox`, `archive`, `stop`.
   * They decide where new mail LANDS, and running them over history would move
   * mail already triaged. */
  skippedActions: string[];
  /** The messages this call changed: the argument to an undo. */
  messageIds: Id[];
  /** More matched than one call will change. The client must offer to continue
   * rather than present a partial backfill as a finished one. */
  truncated: boolean;
  remaining: number;
  scanned: number;
  total: number;
  complete: boolean;
  /** Set on an undo: how many messages had labels taken back off. */
  undone: number;
  /** Set when the rule files into an area (#708): what this call did, per
   * outcome. Null for any other rule. */
  areaFiling?: RuleAreaFiling | null;
  /** The desk/setup area labels this apply took off, per message. Hand them
   * back on undo (`restoreAreaLabels`) or the undo cannot put those messages
   * back where they were. */
  removedAreaLabels?: Record<Id, RemovedAreaLabel[]>;
}

/**
 * Filing mail into an area with a rule (#708), per outcome — never one total,
 * because "40 matched" cannot tell "40 will move" from "40 you already filed
 * elsewhere". On Rule/preview the counts sum to `matched`.
 */
export interface RuleAreaFiling {
  areaId: Id;
  labelId: Id;
  /** Not in the area yet and only automatic placement in the way: moves. */
  move: number;
  alreadyThere: number;
  /** Filed in another area by the user: left there, their filing wins. */
  contestedByUser: number;
  /** Filed in another area by another rule: left there. */
  contestedByRule: number;
  /** Sent to another area by a topic label or the classifier: left there. */
  contestedOther: number;
}

/** One automatically applied area label an area-filing apply removed. */
export interface RemovedAreaLabel {
  labelId: Id;
  appliedBy: "desk" | "setup";
}

/** What Rule/tighten reports (#170). */
export interface RuleTightenResult {
  ruleId: Id;
  corrections: number;
  /** How many corrections the scan could SEE. Zero with corrections > 0 means
   * the mail this rule got wrong is older than the window — not that there is
   * nothing to fix. */
  correctionsInWindow: number;
  current: RuleScope;
  scanned: number;
  total: number;
  complete: boolean;
  /** Best first. EMPTY IS A REAL ANSWER: mail the user rejected can be
   * indistinguishable from mail they kept on the facts a rule can test, and in
   * that case there is no narrower rule to offer. */
  proposals: TightenedRule[];
}

/**
 * What Rule/preview reports (#155): the blast radius of a rule that may not
 * exist yet.
 *
 * `matched` is only a whole-mailbox answer when `complete` is true. Otherwise it
 * counts the `scanned` most recent messages out of `total`, and the UI must say
 * so — presenting a lower bound as exact is the failure this method exists to
 * prevent.
 */
export interface RulePreview {
  matched: number;
  scanned: number;
  total: number;
  complete: boolean;
  /** The scan had to open message bodies (the rule tests `to` or a header),
   * which is why its window is smaller. */
  readBodies: boolean;
  /** Matched messages the user has already filed under a DIFFERENT label BY
   * HAND, and the names of those labels (#156). Zero unless `targetLabelId` was
   * supplied — which means "not asked", NOT "safe". */
  conflicts: number;
  conflictLabels: string[];
  samples: RulePreviewSample[];
  /** Set when `targetLabelId` is an area's own label (#708): what applying the
   * rule would do, per outcome. Null otherwise. */
  areaFiling?: RuleAreaFiling | null;
}

export interface RulePreviewSample {
  id: Id;
  subject: string;
  from: string;
  receivedAt: string;
}

/** One rule Rule/induce proposes from what the user has labelled (#168). */
export interface InducedRule {
  conditions: RuleCondition;
  /** Everything it matches in the scanned window. */
  matched: number;
  /** How many of the labelled examples that includes. */
  covers: number;
  /** The rest — mail this rule would NEWLY label. This is the number that
   * matters: it is what the user has not already agreed to. */
  collateral: number;
  /** Matched mail the user filed under a different label by hand. Induce never
   * proposes a candidate with conflicts, so this is 0 today; it is carried
   * because proposals and manual previews render through the same component. */
  conflicts: number;
  conflictLabels: string[];
  /** Examples drawn from the COLLATERAL — what else this rule catches. Showing
   * the labelled examples back would confirm only what is guaranteed. */
  samples: RulePreviewSample[];
}

/**
 * `Area/ruleSuggestion` (urn:mailroom:desk): whether to offer "Always put mail
 * from {address} in {area}?" after the user filed mail from one sender into an
 * area by hand. The SERVER decides; `reason` says why either way, and
 * `earlier` is how many live messages from the sender are not in the area.
 */
export interface AreaRuleSuggestion {
  suggest: boolean;
  reason: string;
  address: string;
  areaId: Id;
  filings: number;
  earlier: number;
}

/** One line of LearnedPlacement/getStatus's "senders you keep moving by hand". */
export interface LearningStatusSender {
  address: string;
  areaId: Id;
  /** Why learned placement is not filing this sender (a placement reason or an Area/ruleSuggestion one). */
  reason: string;
  filings: number;
}

/**
 * LearnedPlacement/getStatus: what learned placement did in the last 30 days
 * (arrival placements only) and the senders it is not learning. When `enabled`
 * is false nothing else is meaningful. `moreSenders` is a lower bound when
 * `sendersTruncated`.
 */
export interface LearningStatus {
  enabled: boolean;
  placed: number;
  corrected: number;
  kept: number;
  open: number;
  senders: LearningStatusSender[];
  moreSenders: number;
  sendersTruncated: boolean;
}

/**
 * What Rule/induce reports (#168).
 *
 * `examples < required` is the normal answer for a new label, and the UI must
 * say "label one more" rather than "nothing found" — they are different
 * situations and only one of them is a dead end.
 */
export interface RuleInduceResult {
  /**
   * The FOLDER the positives came from (#446), echoed back, and absent when the
   * positives were a label's hand-applied messages instead.
   *
   * Echoed because both answers render through the same component and it has to
   * be able to tell which one it is holding — a folder-induced proposal is
   * evidence about the provider's own sorting, a label-induced one is evidence
   * about what the user did by hand, and the sentence beside the numbers differs.
   */
  mailboxId?: Id;
  examples: number;
  required: number;
  scanned: number;
  total: number;
  complete: boolean;
  /** Best first. Empty WITH enough examples is an honest answer too: messages
   * with nothing in common do not imply a rule. */
  candidates: InducedRule[];
}

/** Who applied a label to a message (Email.labelProvenance, #155). */
export interface LabelProvenance {
  /** The label's keyword atom, as it appears in Email.keywords. */
  keyword: string;
  appliedBy: "user" | "rule" | "ml" | "agent" | "desk" | "setup" | "learned";
  /** The rule that did it, or null — including when a rule DID it but has since
   * been deleted (the attribution is dropped, the label is not). */
  rule: { id: Id; name: string } | null;
}

/** RFC 8621 §6.1 Identity + Mailroom's compose extension (internal/api/identity.go).
 *
 * A wildcard (catchall) identity renders as `*@example.com`: any local part at
 * that domain is a legal From. */
export interface Identity {
  id: Id;
  name: string;
  email: string;
  replyTo: EmailAddress[] | null;
  bcc: EmailAddress[] | null;
  textSignature: string;
  htmlSignature: string;
  mayDelete: boolean;
  /** Mailroom extension: the account whose SMTP this identity submits through. */
  accountId: AccountId;
  isWildcard: boolean;
  enabled: boolean;
  /**
   * The `urn:mailroom:pgp` extension (#492, internal/api/identity.go
   * `identityObject` — spelled exactly as the server names it: `signingKeyId`).
   * READ-ONLY: the id of the UserKey bound as this identity's signing key, and
   * only while that key can still sign; `null`/absent otherwise (the ordinary
   * case). An `Identity/set` patch naming it is refused server-side — a key is
   * bound by the crypto process, never over JMAP.
   *
   * ⚠ NOT SUFFICIENT ON ITS OWN for a "Will sign" claim — the server must ALSO
   * advertise `urn:mailroom:pgp` `{signing: true}` (see `PgpCapability`
   * above and `willSign` in ui/compose/sendLine.ts). This id is an
   * IDENTITY-level fact; `signing` is an INSTANCE-level one.
   */
  signingKeyId?: Id | null;
}

export interface EmailSubmission {
  id: Id;
  identityId: Id;
  emailId: Id;
  threadId: Id | null;
  sendAt: string;
  undoStatus: "pending" | "final" | "canceled";
  /** Mailroom extension: the real submission state, which carries more than
   * RFC 8621's three undoStatus values. */
  mailroomState?: string;
  /**
   * RFC 8621 §7.5 delivery verdicts, keyed by recipient — or by `"*"` before any
   * DSN has arrived, which is the server saying "we only know what we did, not
   * what the far end did" (internal/api/submission.go deliveryStatusFor).
   *
   * ⚠ `smtpReply` IS THE ONLY PLACE A USER CAN LEARN WHY A SEND FAILED. It
   * carries the provider's verbatim response — `550 5.7.1 sender address
   * rejected: not allowed` is what made a real failure diagnosable (#589). A
   * generic "send failed" would have left the operator with no way to know the
   * cause was the From address rather than the recipient, the network, or us.
   *
   * The server has always sent this; the client simply never declared it, which
   * is why the reason was unreachable even though it was on the wire.
   */
  deliveryStatus?: Record<
    string,
    { smtpReply?: string; delivered?: "queued" | "yes" | "no" | "unknown"; displayed?: string }
  >;
  /**
   * RFC 8621 §7 envelope: the addresses actually handed to SMTP.
   *
   * ⚠ `mailFrom.email` IS THE ONLY PLACE THE REAL SENDER APPEARS. An identity on
   * a catch-all domain has the address `*@domain`, so naming a submission by its
   * identity prints the wildcard PATTERN — and on the one failure this matters
   * for, `550 … sender address rejected`, the row then conceals the very address
   * the server refused (#604). The envelope carries `parcel@domain`, which is the
   * answer to "what do I change".
   *
   * Like `deliveryStatus` above, the server has always sent this and the client
   * simply never declared it.
   */
  envelope?: { mailFrom?: { email?: string }; rcptTo?: { email?: string }[] };
  /**
   * SEND & ARCHIVE (#648, urn:mailroom:compose): the Emails the server will
   * archive once this submission is actually SENT — never on a failure, an
   * Undo Send or exhausted retries. Echoed by the server, so its presence is how
   * the client knows the request was understood (an older server ignores it).
   */
  archiveOnSuccess?: Id[] | null;
  /**
   * When the user dismissed this FAILED send from the Outbox (urn:mailroom:compose,
   * migration 00059), or null. Set with the update patch `{dismissed: true}`, legal
   * only on a failed submission. Nothing is deleted, the draft included.
   */
  dismissedAt?: string | null;
  /**
   * Why a still-pending submission is WAITING rather than sending (#52,
   * urn:mailroom:compose): "reconnect" — its account needs the user to sign in
   * again; "provider" — the provider is refusing this server's sign-in, which
   * the operator must fix. null (or absent, on an older server) when it is not
   * held. A dedicated flag, so the Outbox never has to match reason strings.
   */
  mailroomHeld?: "reconnect" | "provider" | null;
}

/** One mail account's outbound send meter (Mailroom extension
 * `EmailSubmission/getSendQuota`, urn:mailroom:compose — internal/api/submission.go).
 * `remaining` follows the send path's 0 = blocked semantics: a `limit` of 0 means
 * "cannot send", so `remaining` is `max(0, limit - sent)`. `resetAt` is the next
 * UTC midnight the per-UTC-day ledger rolls over at. */
export interface SendQuotaAccount {
  id: Id;
  name: string;
  sent: number;
  limit: number;
  remaining: number;
  resetAt: string;
}

/** The `EmailSubmission/getSendQuota` response: one meter per configured mail
 * account, scoped by RLS to the caller's own accounts. */
export interface SendQuotaResult {
  accountId: Id;
  list: SendQuotaAccount[];
  resetAt: string;
}

/** One account's sync telemetry (Mailroom extension `Account/getHealth`,
 * urn:mailroom:accounts — internal/api/account_health.go). Nullable fields are
 * `null` when the account has never attempted/succeeded or has no recorded
 * error. `lagSeconds` is the whole seconds since the last SUCCESSFUL sync (null
 * if it never has), the panel's "synced N ago" input. */
export interface AccountSyncStatus {
  lastSuccessAt: string | null;
  lastAttemptAt: string | null;
  lagSeconds: number | null;
  lastError: string | null;
  lastErrorAt: string | null;
  consecutiveFailures: number;
}

/** One recent failed delivery (bounce) for an account. */
export interface AccountDeliveryFailure {
  recipient: string;
  statusCode: string;
  diagnostic: string;
  at: string;
}

/** Recent delivery failures: a short `recent` tail plus the fuller `count`. */
export interface AccountDeliveryFailures {
  count: number;
  recent: AccountDeliveryFailure[];
}

/** Dead-lettered writeback intents: how many gave up, and their distinct
 * reasons (deduped). */
export interface AccountDeadLetters {
  count: number;
  reasons: string[];
}

/** Today's outbound spend vs. the provider's per-account daily cap. */
export interface AccountSendQuota {
  sent: number;
  limit: number;
}

/**
 * Ingest-only mode's own signal (ADR-028 §1/§2/§4): whether Mailroom is
 * actually writing to this account right now, and how much it has NOT sent
 * because it is not.
 *
 * ⚠ `enabled` IS THE EFFECTIVE VALUE — the account's own `writebackEnabled`
 * column ANDed with an instance-level switch that the API cannot be told to
 * override. A client must read shadow-mode state from here, never by
 * recomputing it from `Account.writebackEnabled`: the two can legitimately
 * disagree (account column true, instance switch off), and this field is the
 * one that matches what actually happened to the last write.
 *
 * `suppressedIntents` is a durable ledger, not a queue depth: ADR-028 §4 is
 * explicit that lifting the mode drains none of it — replay is a separate,
 * explicit admin action — so this count does not fall to zero merely because
 * `enabled` becomes true again.
 */
export interface AccountWriteback {
  enabled: boolean;
  suppressedIntents: number;
}

/** Why the server holds an account's sync OFF and refuses to switch it on
 * (#52: a connected Gmail account until Mailroom's Gmail sync model lands).
 * `message` is the server's own sentence; `code` is stable. */
export interface AccountSyncHold {
  code: string;
  message: string;
}

/**
 * An account's sign-in health (#52, internal/api/account_health.go). `reason`
 * is a short stable code behind a non-active state (an OAuth error code such
 * as "invalid_grant"), never provider prose.
 */
export interface AccountAuthHealth {
  kind: "password" | "oauth2";
  provider: string | null;
  state: AccountAuthState | null;
  reason: string | null;
  stateChangedAt: string | null;
  lastRefreshAt: string | null;
}

/** Gmail import progress (#52, internal/api/account_health.go healthGmailImport). Present only for a syncing Gmail OAuth account. */
export interface AccountGmailImport {
  messagesOnServer: number | null;
  messagesImported: number;
  bytesImported: number;
  historyComplete: boolean;
  bytesDownloadedLast24h: number;
  dailyBudgetBytes: number;
  remainingEstimateBytes: number | null;
  etaDays: number | null;
}

/** One account's full health entry (`Account/getHealth`). */
export interface AccountHealth {
  id: Id;
  name: string;
  syncStatus: AccountSyncStatus;
  deliveryFailures: AccountDeliveryFailures;
  deadLetters: AccountDeadLetters;
  sendQuota: AccountSendQuota;
  writeback: AccountWriteback;
  /** Absent when the server is not holding this account's sync off. */
  syncHold?: AccountSyncHold;
  /** Absent from a server before v1.96. */
  auth?: AccountAuthHealth;
  /** Absent unless this is a syncing Gmail account (#914). */
  gmailImport?: AccountGmailImport;
}

/** A provider refusing this server's sign-in client (operator_error), shown
 * to admins only (`Account/getHealth` operatorNotices). */
export interface OperatorNotice {
  provider: string;
  code: string;
  since: string;
  lastSeenAt: string;
  accounts: number;
}

/** The `Account/getHealth` response: one entry per mail account, RLS-scoped to
 * the caller's own accounts.
 *
 * `searchStalenessSeconds` is envelope-level, not per-account — the search
 * index is per-USER, one index for every mail account the user has (spec §7,
 * mailroom#41). It is absent (undefined) when this server has no search
 * backend installed, distinct from 0 ("caught up"). */
export interface AccountHealthResult {
  accountId: Id;
  list: AccountHealth[];
  searchStalenessSeconds?: number;
  /** Admins only, and only when a sign-in provider is configured (#52). */
  operatorNotices?: OperatorNotice[];
}

/** RFC 8621 §5.1 SearchSnippet — `<mark>`-highlighted fragments (search.go). */
export interface SearchSnippet {
  emailId: Id;
  subject: string | null;
  preview: string | null;
}

// ---------------------------------------------------------------------------
// Draft creation (Email/set create — internal/api/draft.go)
// ---------------------------------------------------------------------------

/** The Email create shape Mailroom's draft path accepts. The server BUILDS the
 * RFC 5322 bytes; the client never supplies them (draft.go). */
export interface EmailCreate {
  from?: EmailAddress[];
  to?: EmailAddress[];
  cc?: EmailAddress[];
  bcc?: EmailAddress[];
  subject?: string;
  inReplyTo?: string[];
  references?: string[];
  bodyValues?: Record<string, { value: string }>;
  textBody?: { partId: string; type: string }[];
  htmlBody?: { partId: string; type: string }[];
}

export interface SubmissionCreate {
  identityId: Id;
  emailId: Id;
  envelope?: {
    mailFrom: { email: string };
    rcptTo: { email: string }[];
  };
  /** RFC 3339. Omitted = "now + the server's Undo-Send window". */
  sendAt?: string;
}

// ---------------------------------------------------------------------------
// Web Push (Forgejo #42, ADR-016 — internal/api WebPushSubscription)
// ---------------------------------------------------------------------------

/** The `urn:mailroom:push` session-capability payload (ADR-016): the server's
 * VAPID application public key — a base64url (unpadded) uncompressed P-256 point.
 * The browser needs it as the `applicationServerKey` for `pushManager.subscribe`
 * (src/app/push.ts decodes it to a Uint8Array). Its presence in the session is
 * the single feature gate for client-side Web Push. */
export interface PushCapability {
  vapidPublicKey: string;
}

/** A stored Web Push subscription as `WebPushSubscription/get` returns it. The
 * push keys (p256dh/auth) are WRITE-ONLY — sent on create, never read back — so
 * they are deliberately absent from this read shape. */
export interface WebPushSubscription {
  id: Id;
  deviceName: string | null;
  /** This device's push mode (#59). It replaced `importantOnly`, which the
   * server never read; a server before #59 sends no mode, read as "all". */
  notifyMode?: PushMode;
  createdAt: string;
  expiredAt: string | null;
}

/** A device's push mode (#59): every message (the default), above the line,
 * VIPs only, or either. */
export type PushMode = "all" | "above_line" | "vip" | "above_line_or_vip";

/** The write-only create shape for `WebPushSubscription/set` create. `endpoint`
 * and `keys` come straight from the browser PushSubscription: `sub.endpoint`,
 * and `sub.getKey('p256dh')` / `sub.getKey('auth')` base64url-encoded. */
export interface WebPushSubscriptionCreate {
  endpoint: string;
  keys: { p256dh: string; auth: string };
  deviceName?: string;
  notifyMode?: PushMode;
}

/** The `WebPushSubscription/get` response (no keys), RLS-scoped to the caller. */
export interface WebPushSubscriptionGetResult {
  accountId: AccountId;
  list: WebPushSubscription[];
}
