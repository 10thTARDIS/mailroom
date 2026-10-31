// SPDX-License-Identifier: AGPL-3.0-only

// JmapClient — the real JMAP transport for Mailroom (spec §13).
//
// Framework-agnostic / DOM-free by construction: no `document`/`window` here.
// `fetch` is a runtime Web API (browsers, Node 18+, Deno, React Native,
// Tauri's webview), not a UI framework, so a native shell can import this
// module unmodified (spec §12).
//
// Design notes that matter:
//
//   - ONE batched POST per logical operation. RFC 8620 §3.2's request object
//     takes a list of method calls and §3.7's result references let a later
//     call consume an earlier one's output, so "list the inbox" is a SINGLE
//     round trip: Email/query then Email/get with `#ids` pointing at it. The
//     client exposes `request()` (raw batch) and thin typed helpers over it.
//   - Errors are typed, not thrown as strings. `JmapAuthError` (401/403) is
//     what the shell turns into a login redirect; `JmapMethodError` carries the
//     RFC 8620 §3.6.2 method error; `JmapTransportError` is a network/HTTP
//     failure, which is what makes an action queue retry instead of roll back.
//   - The session cookie does the authenticating (same-origin deployment), so
//     every request goes out with `credentials: "include"` and no bearer token
//     is threaded anywhere.

import type {
  Account,
  ApiKey,
  ApiKeyEvent,
  ApiKeySettings,
  ApiKeysCapability,
  SignedInDevice,
  PendingAction,
  SuppressedRefusal,
  Area,
  BlobId,
  ChangesArgs,
  ChangesResult,
  ComposeSnippet,
  CategoryTabState,
  SavedSearch,
  Contact,
  SenderGroupBy,
  SenderSortBy,
  SenderStatsResult,
  FollowUpItem,
  FollowUpResult,
  Email,
  EmailSubmission,
  EmailSubmissionFilter,
  GetArgs,
  GetResult,
  Id,
  Identity,
  Invocation,
  JmapRequest,
  JmapResponse,
  Label,
  Mailbox,
  MethodErrorArgs,
  PgpCapability,
  PushCapability,
  QueryArgs,
  ContactKeyDecision,
  ContactKeyImportResult,
  RecipientKeyInfo,
  QueryResult,
  RemoteAllow,
  Rule,
  RemovedAreaLabel,
  RuleApplyResult,
  RuleCondition,
  RuleInduceResult,
  RulePreview,
  RuleTightenResult,
  SearchSnippet,
  SendQuotaResult,
  AccountHealthResult,
  SetArgs,
  SetResult,
  SetupApplyResult,
  SetupCensusResult,
  SetupGetResult,
  SetupPlan,
  SetupPreviewResult,
  Thread,
  UserKey,
  WebPushSubscription,
  WebPushSubscriptionGetResult,
} from "./types";
import { CAP_ACCOUNTS, CAP_APIKEYS, CAP_DEVICES, CAP_OUT_OF_OFFICE, CAP_PGP, CAP_PUSH, CAP_REMOTE_CONTENT, MAILROOM_USING } from "./types";
import type { OutOfOffice, OutOfOfficeUpdate } from "./types";
import type {
  ContactMergeResult,
  ContactSeparateResult,
  ContactKeyEvent,
  ContactKeyRefreshResult,
  HeldContactKey,
  KeyExpiryArg,
  SetError,
  UserKeyReplaceResult,
  UserKeysResult,
  UserKeyUpdated,
} from "./types";
import type { UploadResult } from "./blobs";
import {
  blockedFromResponse,
  downloadUrl,
  eventSourceUrl,
  previewUrl,
  uploadBlob,
  uploadUrl,
} from "./blobs";
import type { BlobBlocked, BlobFetch, BlobProbe } from "./blobs";
import type { JmapSession } from "../session";

/** The session was rejected: the caller must re-authenticate (401/403). */
export class JmapAuthError extends Error {
  readonly status: number;
  constructor(status: number) {
    super(`jmap: unauthenticated (HTTP ${status})`);
    this.name = "JmapAuthError";
    this.status = status;
  }
}

/** The request never produced a JMAP response (network down, 5xx, bad JSON).
 * Retryable by definition — the mutation may or may not have been applied, so
 * every mutation this client sends must be idempotent. */
export class JmapTransportError extends Error {
  readonly status: number | null;
  constructor(message: string, status: number | null = null) {
    super(`jmap: ${message}`);
    this.name = "JmapTransportError";
    this.status = status;
  }
}

/** A method-level error (RFC 8620 §3.6.2): the server understood us and said no. */
export class JmapMethodError extends Error {
  readonly type: string;
  readonly description: string | undefined;
  readonly callId: string;
  constructor(callId: string, args: MethodErrorArgs) {
    super(`jmap: ${args.type}${args.description ? `: ${args.description}` : ""}`);
    this.name = "JmapMethodError";
    this.type = args.type;
    this.description = args.description;
    this.callId = callId;
  }
}

export interface JmapClientConfig {
  /** The JMAP session resource. Same-origin: `/jmap/session` (the server also
   * serves the RFC 8620 well-known alias `/.well-known/jmap`). */
  sessionUrl?: string;
  /** Injectable fetch (tests, non-browser hosts). */
  fetch?: typeof fetch;
}

/** A method call the caller wants dispatched, before it is turned into an
 * RFC 8620 Invocation triple. */
export interface Call {
  name: string;
  args: Record<string, unknown>;
  id: string;
}

/** One method response, already sorted into "worked" / "the server said no". */
export interface CallResult<T = Record<string, unknown>> {
  id: string;
  name: string;
  value: T | null;
  error: JmapMethodError | null;
}

export const DEFAULT_SESSION_URL = "/jmap/session";

/**
 * One page of the OUTBOX, as the pair of calls that fetches it (#653):
 * `EmailSubmission/query {filter: {inOutbox: true}}` and the
 * `EmailSubmission/get` that hydrates its ids through an RFC 8620 §3.7 `#ids`
 * back-reference — one round trip, the shape every list in this client uses.
 *
 * WHY THE OUTBOX NEEDS ITS OWN BOOTSTRAP: submissions used to reach the store
 * only from this tab's own sends and from `/changes`, and a reload has neither
 * — so the Outbox was empty after every reload, and a failed send or a Send
 * Later vanished without the user doing anything. `EmailSubmission/get` with
 * `ids: null` is no substitute: it answers the 100 most recent and does not
 * say so.
 *
 * Built here rather than in sync.ts so the method names sit in the one file
 * internal/api/client_contract_test.go scans for "every method the client can
 * put on the wire has its capability in MAILROOM_USING".
 *
 * `calculateTotal` is always on: the caller pages while `position + ids` is
 * short of `total`, which is what makes the server's cap (it reports `limit`)
 * impossible to hit silently.
 */
export function outboxPageCalls(accountId: string, position: number, tag: string): Call[] {
  return [
    {
      name: "EmailSubmission/query",
      args: {
        accountId,
        filter: { inOutbox: true } satisfies EmailSubmissionFilter,
        position,
        calculateTotal: true,
      },
      id: `${tag}q`,
    },
    {
      name: "EmailSubmission/get",
      args: {
        accountId,
        "#ids": { resultOf: `${tag}q`, name: "EmailSubmission/query", path: "/ids" },
      },
      id: `${tag}g`,
    },
  ];
}

/** The `using` set for WebPushSubscription calls (ADR-016): the base Mailroom
 * set plus `urn:mailroom:push`. Scoped to the push helpers so ordinary mail
 * requests never declare a capability the server might not advertise — the push
 * methods are only ever invoked when the session advertises push in the first
 * place (JmapClient.vapidPublicKey() gates it). */
const PUSH_USING: readonly string[] = [...MAILROOM_USING, CAP_PUSH];

/** The shape of an image-proxy path (internal/api/proxy.go): two ULIDs and a
 * 32-hex token. */
const PROXY_PATH = /^\/jmap\/proxy\/[0-9A-Za-z]{26}\/[0-9A-Za-z]{26}\/[0-9a-f]{32}$/;

/** The raster types the image proxy re-serves (proxy.go's
 * proxyImageContentType) and so the only ones a proxied image is accepted as.
 * No SVG, for the reason given there. */
const PROXIED_IMAGE_TYPES = new Set([
  "image/png",
  "image/jpeg",
  "image/gif",
  "image/webp",
  "image/avif",
  "image/bmp",
  "image/x-icon",
  "image/vnd.microsoft.icon",
]);

/** The media types a `cid:` part may be rendered inline as (Forgejo #99). It is
 * the raster subset of blob.go's own download allowlist, and it deliberately
 * OMITS image/svg+xml (a scripting context) — the download endpoint already
 * refuses to serve SVG as image/svg+xml (it becomes application/octet-stream),
 * and this refuses to render anything that is not a real raster image. */
const INLINE_IMAGE_TYPES = new Set([
  "image/png",
  "image/jpeg",
  "image/gif",
  "image/webp",
  "image/bmp",
  "image/avif",
  "image/tiff",
  "image/heic",
]);

export class JmapClient {
  private readonly sessionUrl: string;
  private readonly doFetch: typeof fetch;
  private session: JmapSession | null = null;

  constructor(config: JmapClientConfig = {}) {
    this.sessionUrl = config.sessionUrl ?? DEFAULT_SESSION_URL;
    // Bind: an unbound `fetch` throws "Illegal invocation" in browsers.
    this.doFetch = (config.fetch ?? globalThis.fetch).bind(globalThis);
  }

  /** The session fetched by `connect()`, or null before it. */
  get currentSession(): JmapSession | null {
    return this.session;
  }

  /** The primary account id (Mailroom has exactly one account per user, whose
   * id IS the user's ULID — internal/api/service.go `resolveAccount`). */
  get accountId(): string {
    const s = this.session;
    if (!s) throw new JmapTransportError("not connected: call connect() first");
    const primary = s.primaryAccounts[
      "urn:ietf:params:jmap:mail"
    ] as string | undefined;
    const id = primary ?? Object.keys(s.accounts)[0];
    if (!id) throw new JmapTransportError("session has no accounts");
    return id;
  }

  /** Fetches (and caches) the RFC 8620 §2 Session resource. */
  async connect(): Promise<JmapSession> {
    let res: Response;
    try {
      res = await this.doFetch(this.sessionUrl, {
        method: "GET",
        credentials: "include",
        headers: { Accept: "application/json" },
      });
    } catch (err) {
      throw new JmapTransportError(`session fetch failed: ${String(err)}`);
    }
    if (res.status === 401 || res.status === 403) throw new JmapAuthError(res.status);
    if (!res.ok) throw new JmapTransportError(`session: HTTP ${res.status}`, res.status);
    let session: JmapSession;
    try {
      session = (await res.json()) as JmapSession;
    } catch (err) {
      throw new JmapTransportError(`session: malformed JSON: ${String(err)}`);
    }
    this.session = session;
    return session;
  }

  /**
   * Dispatches a batch of method calls in one POST and returns one CallResult
   * per call, in request order. A method-level error does NOT throw: the batch
   * is partially successful by design (RFC 8620 §3.2), and the caller decides
   * which failures matter. Transport and auth failures DO throw — nothing in
   * the batch ran.
   */
  async request(
    calls: Call[],
    using: readonly string[] = MAILROOM_USING,
    signal?: AbortSignal,
  ): Promise<CallResult[]> {
    const s = this.session;
    if (!s) throw new JmapTransportError("not connected: call connect() first");

    const body: JmapRequest = {
      using,
      methodCalls: calls.map((c): Invocation => [c.name, c.args, c.id]),
    };

    let res: Response;
    try {
      res = await this.doFetch(s.apiUrl, {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify(body),
        ...(signal ? { signal } : {}),
      });
    } catch (err) {
      // A caller that aborted asked for this and is not owed a "transport
      // failure": hand back the abort itself so it can be told from a dropped
      // connection.
      if (signal?.aborted) throw err;
      throw new JmapTransportError(`request failed: ${String(err)}`);
    }
    if (res.status === 401 || res.status === 403) throw new JmapAuthError(res.status);
    if (!res.ok) throw new JmapTransportError(`request: HTTP ${res.status}`, res.status);

    let parsed: JmapResponse;
    try {
      parsed = (await res.json()) as JmapResponse;
    } catch (err) {
      throw new JmapTransportError(`response: malformed JSON: ${String(err)}`);
    }

    const byCallId = new Map<string, Invocation>();
    for (const inv of parsed.methodResponses ?? []) {
      // A callId is unique per batch (we generate them), so first-wins is safe.
      if (!byCallId.has(inv[2])) byCallId.set(inv[2], inv);
    }

    return calls.map((c): CallResult => {
      const inv = byCallId.get(c.id);
      if (!inv) {
        return {
          id: c.id,
          name: c.name,
          value: null,
          error: new JmapMethodError(c.id, {
            type: "serverFail",
            description: "no response for this method call",
          }),
        };
      }
      if (inv[0] === "error") {
        return {
          id: c.id,
          name: c.name,
          value: null,
          error: new JmapMethodError(c.id, inv[1] as unknown as MethodErrorArgs),
        };
      }
      return { id: c.id, name: c.name, value: inv[1], error: null };
    });
  }

  /** Dispatches a single call and throws on a method error — the ergonomic form
   * for the many places where a failure has no partial-success story. */
  async call<T>(
    name: string,
    args: Record<string, unknown>,
    using: readonly string[] = MAILROOM_USING,
  ): Promise<T> {
    const [result] = await this.request([{ name, args, id: "c0" }], using);
    if (!result) throw new JmapTransportError(`no result for ${name}`);
    if (result.error) throw result.error;
    return result.value as T;
  }

  // -- Typed helpers ---------------------------------------------------------
  // Thin sugar over call(); anything that wants batching uses request() with
  // result references directly (see core/sync.ts).

  emailGet(args: GetArgs): Promise<GetResult<Email>> {
    return this.call("Email/get", args as unknown as Record<string, unknown>);
  }
  emailQuery(args: QueryArgs): Promise<QueryResult> {
    return this.call("Email/query", args as unknown as Record<string, unknown>);
  }
  emailChanges(args: ChangesArgs): Promise<ChangesResult> {
    return this.call("Email/changes", args as unknown as Record<string, unknown>);
  }
  emailSet(args: SetArgs): Promise<SetResult<Email>> {
    return this.call("Email/set", args as unknown as Record<string, unknown>);
  }
  mailboxGet(args: GetArgs): Promise<GetResult<Mailbox>> {
    return this.call("Mailbox/get", args as unknown as Record<string, unknown>);
  }
  mailboxChanges(args: ChangesArgs): Promise<ChangesResult> {
    return this.call("Mailbox/changes", args as unknown as Record<string, unknown>);
  }
  /**
   * `Mailbox/set` accepts exactly ONE property: `syncScope` (#62). Creating,
   * renaming and destroying mailboxes are refused server-side, and a patch that
   * mentions any other property is refused WHOLE — so this is not a general
   * mailbox editor and must not be used as one.
   */
  mailboxSet(args: SetArgs): Promise<SetResult<Mailbox>> {
    return this.call("Mailbox/set", args as unknown as Record<string, unknown>);
  }
  threadGet(args: GetArgs): Promise<GetResult<Thread>> {
    return this.call("Thread/get", args as unknown as Record<string, unknown>);
  }
  labelGet(args: GetArgs): Promise<GetResult<Label>> {
    return this.call("Label/get", args as unknown as Record<string, unknown>);
  }
  labelSet(args: SetArgs): Promise<SetResult<Label>> {
    return this.call("Label/set", args as unknown as Record<string, unknown>);
  }
  /** urn:mailroom:rules (#155). Rule/query returns ids in EVALUATION order
   * (priority, lower first) — not an arbitrary order the client may re-sort,
   * because the order IS the semantics: `stop` halts everything after it. */
  ruleQuery(args: { accountId: string }): Promise<QueryResult> {
    return this.call("Rule/query", args as unknown as Record<string, unknown>);
  }
  ruleGet(args: GetArgs): Promise<GetResult<Rule>> {
    return this.call("Rule/get", args as unknown as Record<string, unknown>);
  }
  ruleSet(args: SetArgs): Promise<SetResult<Rule>> {
    return this.call("Rule/set", args as unknown as Record<string, unknown>);
  }
  /** urn:mailroom:remotecontent (#202): the remembered per-sender "load this
   * sender's images" decisions.
   *
   * Reading the whole list is the ordinary call — there is no /query, because the
   * client has to match every sender it renders against every grant, so it wants
   * all of them anyway. Writing goes through remoteAllowSet: creates take
   * `{kind, value}` and there is deliberately no update form, since changing a
   * grant is revoking one decision and making another. */
  /** urn:mailroom:composesnippets (#56): saved signatures and templates. Read whole
   * — a compose window needs the signatures for the identity it is sending as AND
   * the templates, and both are a handful of rows. */
  composeSnippetGet(args: GetArgs): Promise<GetResult<ComposeSnippet>> {
    return this.call("ComposeSnippet/get", args as unknown as Record<string, unknown>);
  }
  composeSnippetSet(args: SetArgs): Promise<SetResult<ComposeSnippet>> {
    return this.call("ComposeSnippet/set", args as unknown as Record<string, unknown>);
  }

  /** urn:mailroom:categorytabs (#228): the five Gmail-style tabs and their
   * per-user state — which are switched on, and in what order.
   *
   * Read whole. There are five rows, the strip needs all the enabled ones to
   * render, and a settings panel needs the disabled ones too, so filtering
   * server-side would be two calls to save nothing. Sort on each row's own
   * `position`, NOT on the capability's advertised order — that order is only
   * the default a fresh account starts from, and the user rearranges it. */
  categoryTabGet(args: GetArgs): Promise<GetResult<CategoryTabState>> {
    return this.call("CategoryTab/get", args as unknown as Record<string, unknown>);
  }
  /** Enable, disable or reorder a tab. There is no create or destroy: the
   * vocabulary is fixed, and Primary cannot be disabled — the server refuses it
   * with `invalidProperties`, so a UI should not offer the control at all
   * rather than offer one that always fails. */
  categoryTabSet(args: SetArgs): Promise<SetResult<CategoryTabState>> {
    return this.call("CategoryTab/set", args as unknown as Record<string, unknown>);
  }

  savedSearchGet(args: GetArgs): Promise<GetResult<SavedSearch>> {
    return this.call("SavedSearch/get", args as unknown as Record<string, unknown>);
  }
  savedSearchSet(args: SetArgs): Promise<SetResult<SavedSearch>> {
    return this.call("SavedSearch/set", args as unknown as Record<string, unknown>);
  }

  remoteAllowGet(args: GetArgs): Promise<GetResult<RemoteAllow>> {
    return this.call("RemoteAllow/get", args as unknown as Record<string, unknown>);
  }
  remoteAllowSet(args: SetArgs): Promise<SetResult<RemoteAllow>> {
    return this.call("RemoteAllow/set", args as unknown as Record<string, unknown>);
  }

  /** urn:mailroom:remotecontent (ADR-026 §3c): "the user clicked Show remote
   * content on THIS message". Not a stored object and not a grant — a
   * short-lived, in-memory note the image proxy honours, so a one-off click
   * loads images without remembering the sender. */
  remoteContentRequest(args: {
    accountId: string;
    messageId: string;
  }): Promise<{ requested?: boolean; expiresIn?: number }> {
    return this.call("RemoteContent/request", args as unknown as Record<string, unknown>);
  }
  /** urn:mailroom:rules (#155): what a rule WOULD have matched, before it
   * exists. Takes a bare condition tree — no rule id, nothing saved — and
   * writes nothing. `limit` is clamped server-side; the result reports the scan
   * it actually performed, which the caller must surface rather than presenting
   * a bounded count as a total. */
  rulePreview(args: {
    accountId: string;
    conditions: RuleCondition;
    /** The label this rule would apply. Supplying it is what turns the blast
     * radius into a conflict report ("…and 3 of them you called Shopping"). */
    targetLabelId?: string;
    limit?: number;
  }): Promise<RulePreview> {
    return this.call(
      "Rule/preview",
      args as unknown as Record<string, unknown>,
    ) as Promise<RulePreview>;
  }
  /**
   * urn:mailroom:rules (#168): propose a rule from what the user has labelled —
   * or, since #446, from what the PROVIDER sorted into a folder.
   *
   * Writes nothing; the client saves an accepted candidate with Rule/set.
   *
   * ⚠ EXACTLY ONE OF `labelId` AND `mailboxId`, which is why the argument is a
   * union rather than two optional fields. Both set means "positive" would mean
   * two different things in one call, and the server answers that with
   * `invalidArguments` rather than a silent precedence rule the caller cannot
   * see; neither set is the same error. The union makes the server's rule a
   * compile error here instead of a runtime one.
   */
  ruleInduce(args: {
    accountId: string;
    limit?: number;
  } & ({ labelId: string } | { mailboxId: string })): Promise<RuleInduceResult> {
    return this.call(
      "Rule/induce",
      args as unknown as Record<string, unknown>,
    ) as Promise<RuleInduceResult>;
  }
  /** urn:mailroom:rules (#191): run a saved rule over the mail already in the
   * mailbox. THE ONE Rule/* METHOD THAT WRITES — every other one proposes.
   *
   * Additive actions only, never re-applying a label the user corrected, and
   * idempotent. `undo` with the message ids the apply returned reverses it
   * without recording corrections against the rule. */
  ruleApply(args: {
    accountId: string;
    ruleId: string;
    limit?: number;
    undo?: boolean;
    messageIds?: string[];
    /** An area-filing apply's `removedAreaLabels`, handed back on undo (#708). */
    restoreAreaLabels?: Record<string, RemovedAreaLabel[]>;
  }): Promise<RuleApplyResult> {
    return this.call(
      "Rule/apply",
      args as unknown as Record<string, unknown>,
    ) as Promise<RuleApplyResult>;
  }
  /** urn:mailroom:rules (#170): a narrower form of an EXISTING rule, built from
   * the messages the user took its label back off.
   *
   * Writes nothing, and does not consume the corrections — saving the fix is
   * what retires them, because until then the rule is still misfiling. */
  ruleTighten(args: {
    accountId: string;
    ruleId: string;
    limit?: number;
  }): Promise<RuleTightenResult> {
    return this.call(
      "Rule/tighten",
      args as unknown as Record<string, unknown>,
    ) as Promise<RuleTightenResult>;
  }
  /** urn:mailroom:rules (#176): close a message's open questions without filing
   * it — the needs-a-category queue's "none of these".
   *
   * It records NOTHING about the label being wrong. A user clearing a first-run
   * backlog is saying "not now" at least as often as "not this", and training on
   * a guess about which is how a corpus acquires labels nobody meant. */
  dismissCategory(args: {
    accountId: string;
    ids: string[];
    /** Reverse a dismissal instead of making one: the messages go back in the
     * queue. It cannot restore suggestions the dismissal deleted. */
    undo?: boolean;
  }): Promise<{ dismissed?: string[]; notFound?: string[] }> {
    return this.call("Email/dismissCategory", args as unknown as Record<string, unknown>) as Promise<{
      dismissed?: string[];
      notFound?: string[];
    }>;
  }
  /** urn:mailroom:attachmentscan (#396/#412, ADR-027 §8): lift — or restore —
   * the malware scanner's quarantine on ONE projected part.
   *
   * ⚠ KEYED ON THE PART'S CONTENT DIGEST SERVER-SIDE, not on this one message,
   * so `affectedEmailIds` can name more than `id`: every copy of those exact
   * bytes anywhere in the mailbox is covered by the one decision
   * (internal/api/attachment_scan.go's own comment). A caller that only
   * refreshes `id` after this call is refreshing an undercount. */
  /** Answer a calendar invitation (urn:mailroom:calendar, #725). The server
   * builds the iTIP REPLY, saves it as a draft and submits it through the same
   * path EmailSubmission/set create runs, so the returned submission is an
   * ordinary one — `sendAt` is the Undo Send deadline and
   * `EmailSubmission/set {undoStatus: "canceled"}` takes the answer back. */
  emailReplyToInvite(args: {
    accountId: string;
    emailId: string;
    partstat: "accepted" | "tentative" | "declined";
    sendAt?: string;
  }): Promise<{
    accountId: string;
    emailId: string;
    partstat: string;
    submission: EmailSubmission | null;
    newState: string;
  }> {
    return this.call("Email/replyToInvite", args as unknown as Record<string, unknown>) as Promise<{
      accountId: string;
      emailId: string;
      partstat: string;
      submission: EmailSubmission | null;
      newState: string;
    }>;
  }

  emailReleaseAttachment(args: {
    accountId: string;
    id: string;
    partId: string;
    /** Undo a release, re-arming the gate, instead of making one. */
    revoke?: boolean;
  }): Promise<{
    accountId: string;
    newState: string;
    released: boolean;
    affectedEmailIds: string[];
  }> {
    return this.call(
      "Email/releaseAttachment",
      args as unknown as Record<string, unknown>,
    ) as Promise<{
      accountId: string;
      newState: string;
      released: boolean;
      affectedEmailIds: string[];
    }>;
  }
  /** urn:mailroom:organize (#51). Update-only — threads are derived from message
   * membership, so the server refuses create/destroy. */
  threadSet(args: SetArgs): Promise<SetResult<Thread>> {
    return this.call("Thread/set", args as unknown as Record<string, unknown>);
  }
  /** urn:mailroom:triage (#154). One-click importance correction: a map of
   * message id → "important" | "notImportant". It records a feedback event the
   * trainer consumes, so the answer is not a patched property but a re-scored
   * model on the next pass. */
  triageCorrect(args: {
    accountId: string;
    corrections: Record<string, "important" | "notImportant">;
  }): Promise<{ corrected?: string[]; notCorrected?: Record<string, string> }> {
    return this.call("Triage/correct", args as unknown as Record<string, unknown>) as Promise<{
      corrected?: string[];
      notCorrected?: Record<string, string>;
    }>;
  }
  /** urn:mailroom:triage (#842). The explicit open signal: these messages were
   * shown in the reading pane for the open dwell (ui/useOpenDwell.ts). The
   * server records an `open` engagement event once per message. Session-only:
   * an API key has no reading pane. */
  triageOpen(args: { accountId: string; emailIds: string[] }): Promise<{
    opened?: string[];
    notOpened?: Record<string, string>;
  }> {
    return this.call("Triage/open", args as unknown as Record<string, unknown>) as Promise<{
      opened?: string[];
      notOpened?: Record<string, string>;
    }>;
  }
  accountGet(args: GetArgs): Promise<GetResult<Account>> {
    return this.call("Account/get", args as unknown as Record<string, unknown>);
  }
  accountSet(args: SetArgs): Promise<SetResult<Account>> {
    return this.call("Account/set", args as unknown as Record<string, unknown>);
  }
  /**
   * The sign-in providers this server can connect mail accounts through (#52):
   * `oauthProviders` of the urn:mailroom:accounts capability. Empty when none
   * is configured — or the server predates them — and then no Connect button
   * is offered at all.
   */
  oauthProviders(): string[] {
    const cap = this.session?.capabilities?.[CAP_ACCOUNTS] as { oauthProviders?: unknown } | undefined;
    const list = cap?.oauthProviders;
    return Array.isArray(list) ? list.filter((p): p is string => typeof p === "string") : [];
  }
  /** `Account/oauthStart` (#52): records a pending sign-in and returns the
   * provider's authorization URL. `id` names an existing account to reconnect. */
  accountOAuthStart(provider: string, id?: Id): Promise<{ authorizationUrl: string; expiresAt: string }> {
    const args: Record<string, unknown> = { accountId: this.accountId, provider };
    if (id) args["id"] = id;
    return this.call("Account/oauthStart", args);
  }
  /** `Account/oauthDisconnect` (#52): ends the grant and keeps the mail.
   * `revocation` other than "revoked" means the provider may still list the
   * access, and the user should remove it there. */
  accountOAuthDisconnect(id: Id): Promise<{ id: Id; provider: string; revocation: string }> {
    return this.call("Account/oauthDisconnect", { accountId: this.accountId, id });
  }
  /**
   * urn:mailroom:desk (#442): the desk's areas.
   *
   * ⚠ `tabs` AND `labelIds` ARE REPLACE-ALL when either appears in an update
   * patch (internal/api/area.go), so an editor that means to add one feed must
   * send the whole array — see ui/settings/areaFeeds.ts, which is the only place
   * that builds them. `id` and `labelId` are immutable: an area's own label IS its
   * identity, and a destroy LEAVES that label on every message that carries it.
   */
  areaSet(args: SetArgs): Promise<SetResult<Area>> {
    return this.call("Area/set", args as unknown as Record<string, unknown>);
  }
  areaGet(args: GetArgs): Promise<GetResult<Area>> {
    return this.call("Area/get", args as unknown as Record<string, unknown>);
  }

  // -- day-one setup (urn:mailroom:desk, #446) --------------------------------
  //
  // THE ORDER OF THESE FIVE IS THE FEATURE. census → preview → apply, with the
  // first two writing nothing, is "build the report before the fix" (CLAUDE.md)
  // expressed as an API: the wizard shows the folders and their counts, then the
  // exact per-folder consequence of the plan, and only then offers the write.
  //
  // `accountId` is the LOGIN on every one of them, as everywhere else in this
  // client. The MAIL account is `mailAccountId`, and only the census takes one.

  /** Every folder day-one can act on, with the numbers a decision needs. Writes
   * nothing — not even the default areas, which is why it does not go through
   * Area/getSummary (internal/store/setup_census.go says more).
   *
   * `windowDays` is cheap to vary: step 2's window radios re-ask rather than
   * re-deriving `inWindow` client-side, because the server's counts are the
   * truth and a client-side split of a count it did not compute is a guess. */
  setupCensus(args: {
    accountId: string;
    /** Narrow to ONE mail account — the same shape Area/getSummary takes. */
    mailAccountId?: string;
    windowDays?: number;
  }): Promise<SetupCensusResult> {
    return this.call(
      "Setup/census",
      args as unknown as Record<string, unknown>,
    ) as Promise<SetupCensusResult>;
  }

  /** What applying this plan WOULD write, per folder and in total. Writes
   * nothing. */
  setupPreview(args: { accountId: string; plan: SetupPlan }): Promise<SetupPreviewResult> {
    return this.call(
      "Setup/preview",
      args as unknown as Record<string, unknown>,
    ) as Promise<SetupPreviewResult>;
  }

  /** THE ONE DOOR THAT WRITES: creates the run, mints the labels and areas the
   * plan needs, and enqueues the chunked import job.
   *
   * Throws `JmapMethodError` with type `setupRunBusy` while a run is queued,
   * running or undoing — a well-formed request that would be valid later, which
   * is why it is not `invalidArguments`. The caller shows the run that is already
   * going. */
  setupApply(args: { accountId: string; plan: SetupPlan }): Promise<SetupApplyResult> {
    return this.call(
      "Setup/apply",
      args as unknown as Record<string, unknown>,
    ) as Promise<SetupApplyResult>;
  }

  /** The runs and their progress. `ids: null` (or omitted) is every run, newest
   * decided by the server's own order. */
  setupGet(args: { accountId: string; ids?: string[] | null }): Promise<SetupGetResult> {
    return this.call(
      "Setup/get",
      args as unknown as Record<string, unknown>,
    ) as Promise<SetupGetResult>;
  }

  /** Reverse a whole run from its per-message journal: only what the run itself
   * did, so a label the user has re-applied by hand survives.
   *
   * Refused with `setupRunBusy` until the run has FINISHED — the server making
   * "Undo is disabled until it is done" a rule rather than a convention. */
  setupUndo(args: { accountId: string; runId: string }): Promise<{
    accountId: string;
    runId: string;
  }> {
    return this.call("Setup/undo", args as unknown as Record<string, unknown>) as Promise<{
      accountId: string;
      runId: string;
    }>;
  }
  identityGet(args: GetArgs): Promise<GetResult<Identity>> {
    return this.call("Identity/get", args as unknown as Record<string, unknown>);
  }
  submissionGet(args: GetArgs): Promise<GetResult<EmailSubmission>> {
    return this.call(
      "EmailSubmission/get",
      args as unknown as Record<string, unknown>,
    );
  }
  submissionSet(args: SetArgs): Promise<SetResult<EmailSubmission>> {
    return this.call(
      "EmailSubmission/set",
      args as unknown as Record<string, unknown>,
    );
  }
  /** The user-visible send-quota meter (Mailroom extension, urn:mailroom:compose):
   * "N of M sends today, resets at midnight UTC" per mail account. Read-only —
   * enforcement lives server-side in the send path (internal/store ReserveOutbound);
   * this only surfaces the same ledger. RLS scopes it to the caller's own accounts. */
  sendQuotaGet(accountId: string): Promise<SendQuotaResult> {
    return this.call("EmailSubmission/getSendQuota", { accountId });
  }
  /** The account-health panel read (Mailroom extension, urn:mailroom:accounts):
   * per mail account, its sync status (with lag), recent delivery failures,
   * dead-lettered writeback intents, and today's send quota. Read-only and
   * computed on demand — account health is operational telemetry, not a synced
   * object. RLS scopes it to the caller's own accounts. Passing no ids returns
   * every account. */
  accountHealthGet(accountId: string): Promise<AccountHealthResult> {
    return this.call("Account/getHealth", { accountId });
  }
  /** Recipient autocomplete over the auto-derived contacts (urn:mailroom:contacts).
   * One batched round trip (RFC 8620 §3.7): Contact/query matches the typed text
   * (address or display name, most-corresponded-with first), and a back-referenced
   * Contact/get materializes the objects. The query's id order is authoritative —
   * Contact/get's list order is unspecified — so we re-sort the objects by it. A
   * server that does not advertise the capability, or any method error, yields an
   * empty list: autocomplete is a convenience, never a blocker on typing. */
  async contactAutocomplete(text: string, limit = 8): Promise<Contact[]> {
    const accountId = this.accountId;
    const results = await this.request([
      {
        name: "Contact/query",
        args: { accountId, filter: { text }, limit },
        id: "q",
      },
      {
        name: "Contact/get",
        args: {
          accountId,
          "#ids": { resultOf: "q", name: "Contact/query", path: "/ids" },
        },
        id: "g",
      },
    ]);
    const query = results.find((r) => r.id === "q");
    const get = results.find((r) => r.id === "g");
    if (!get || get.error || !get.value) return [];
    const ids = (query?.value as unknown as QueryResult | undefined)?.ids ?? [];
    const list = (get.value as unknown as GetResult<Contact>).list ?? [];
    const byId = new Map(list.map((c) => [c.id, c]));
    return ids
      .map((id) => byId.get(id))
      .filter((c): c is Contact => Boolean(c));
  }

  // -- OpenPGP keys (urn:mailroom:pgp, #374) ---------------------------------
  //
  // BOTH OF THESE FAIL SOFT, AND THAT IS THEIR POINT. The Keys row states three
  // facts about a feature this server may not have at all, and the row has to be
  // able to say "keys are not available on this server" rather than a guess — so
  // every failure here funnels into ONE value, `null`, which the row renders as
  // that sentence. A thrown error would take the whole settings page down over a
  // capability that was never promised.
  //
  // ⚠ NULL IS NOT ZERO AND NOT EMPTY, in both directions:
  //   * `userKeysGet()` → null means "could not ask"; `[]` means "asked, and you
  //     have no key" — which the row prints as "no key".
  //   * `contactKeyCount()` → null means "could not ask"; `0` means "asked, and
  //     nobody you correspond with has published one".
  // Collapsing either pair is the #176 mistake: a zero row is a finding, an
  // unaskable question is not a zero.

  /** True when the SESSION advertises a capability. The only safe pre-flight for
   * an optional URN: naming an unadvertised one in `using` is a request-level
   * 400 that refuses the entire batch (see CAP_PGP in types.ts). */
  hasCapability(urn: string): boolean {
    const caps = this.session?.capabilities;
    return caps !== undefined && caps !== null && Object.hasOwn(caps, urn);
  }

  /**
   * Whether THIS SERVER can sign outbound mail AT ALL (#492), read from the
   * `signing` property of the session's `urn:mailroom:pgp` capability object
   * (internal/api/session.go `pgpCapability`).
   *
   * `null` when the capability is ABSENT altogether — an older server that
   * predates #492 — which is a different fact from "present and false"
   * (#176: a server that cannot be asked has not said no). The compose send
   * line's `willSign` combines this INSTANCE-wide fact with the per-identity
   * `Identity.signingKeyId`; neither one alone is enough for a "Will sign"
   * claim.
   */
  pgpSigning(): boolean | null {
    if (!this.hasCapability(CAP_PGP)) return null;
    const cap = this.session?.capabilities[CAP_PGP] as PgpCapability | undefined;
    return typeof cap?.signing === "boolean" ? cap.signing : null;
  }

  /**
   * Whether THIS SERVER can encrypt outbound mail at all (#47, ADR-038): the
   * `encrypt` property of the `urn:mailroom:pgp` capability. `null` when the
   * capability or the property is absent (an older server), which is "cannot
   * ask", not "no" (#176).
   */
  pgpEncrypt(): boolean | null {
    if (!this.hasCapability(CAP_PGP)) return null;
    const cap = this.session?.capabilities[CAP_PGP] as PgpCapability | undefined;
    return typeof cap?.encrypt === "boolean" ? cap.encrypt : null;
  }

  /**
   * Whether this server can generate, import, extend, revoke and back up keys
   * (#796): the `keyManagement` property of `urn:mailroom:pgp`. `null` when the
   * capability or the property is absent (a server before #796), which is
   * "cannot ask", not "no" (#176).
   */
  pgpKeyManagement(): boolean | null {
    if (!this.hasCapability(CAP_PGP)) return null;
    const cap = this.session?.capabilities[CAP_PGP] as PgpCapability | undefined;
    return typeof cap?.keyManagement === "boolean" ? cap.keyManagement : null;
  }

  /**
   * The server's expiry-warning threshold in days (#795), `null` when it does
   * not say. Read from the session, never assumed: the server's `expiringSoon`
   * flags use this one number, and a client that hard-coded its own would
   * disagree with them the day the server's changed.
   */
  pgpKeyExpiryWarningDays(): number | null {
    if (!this.hasCapability(CAP_PGP)) return null;
    const cap = this.session?.capabilities[CAP_PGP] as PgpCapability | undefined;
    const d = cap?.keyExpiryWarningDays;
    return typeof d === "number" && Number.isFinite(d) && d >= 0 ? d : null;
  }

  /**
   * `RecipientKey/lookup`: which of these addresses can be encrypted to. May make
   * the server fetch a key over WKD (bounded), so callers debounce it. `null`
   * when it cannot be asked (no capability, or the call failed) — never an empty
   * list standing in for "nobody has a key".
   */
  async recipientKeyLookup(addresses: string[]): Promise<RecipientKeyInfo[] | null> {
    if (!this.hasCapability(CAP_PGP) || addresses.length === 0) return null;
    try {
      const [r] = await this.request(
        [
          {
            name: "RecipientKey/lookup",
            args: { accountId: this.accountId, addresses },
            id: "rk",
          },
        ],
        [...MAILROOM_USING, CAP_PGP],
      );
      if (!r || r.error || !r.value) return null;
      const list = (r.value as unknown as { list?: RecipientKeyInfo[] }).list;
      return Array.isArray(list) ? list : null;
    } catch {
      return null;
    }
  }

  /**
   * `ContactKey/decide` (#47, ADR-039): the user's answer to "this sender's key is
   * not the one I had". `trust` makes the key the pin and an eligible imported
   * key (and lets encryption to that address proceed); `keep` records it as
   * refused. Throws the method error, so a caller can say what went wrong.
   */
  contactKeyDecide(
    address: string,
    fingerprint: string,
    decision: "trust" | "keep",
  ): Promise<ContactKeyDecision> {
    // Like the key helpers: naming an unadvertised URN is a request-level 400, and
    // a server without the keys API has no decision to take.
    if (!this.hasCapability(CAP_PGP)) {
      return Promise.reject(new JmapTransportError("this server does not support key decisions"));
    }
    return this.call(
      "ContactKey/decide",
      { accountId: this.accountId, address, fingerprint, decision },
      [...MAILROOM_USING, CAP_PGP],
    );
  }

  /**
   * `ContactKey/import` (#47, ADR-038): the user hands over a correspondent's
   * public key (armored text, or the base64 of a binary key). `dryRun` runs every
   * check and returns the fingerprint and user ID without storing anything, so
   * the dialog can show the key BEFORE the user confirms. Throws the method
   * error (its `type` says which rule refused the key).
   */
  contactKeyImport(
    address: string,
    publicKey: string,
    opts: { dryRun?: boolean } = {},
  ): Promise<ContactKeyImportResult> {
    if (!this.hasCapability(CAP_PGP)) {
      return Promise.reject(new JmapTransportError("this server does not support key import"));
    }
    return this.call(
      "ContactKey/import",
      {
        accountId: this.accountId,
        address,
        publicKey,
        ...(opts.dryRun ? { dryRun: true } : {}),
      },
      [...MAILROOM_USING, CAP_PGP],
    );
  }

  // -- Out of office (#50) ---------------------------------------------------

  /** Every account's out-of-office object, or null when this server has no
   * out-of-office API or the call failed ("not available", never "off"). */
  async outOfOfficeGet(): Promise<OutOfOffice[] | null> {
    if (!this.hasCapability(CAP_OUT_OF_OFFICE)) return null;
    try {
      const [r] = await this.request(
        [{ name: "OutOfOffice/get", args: { accountId: this.accountId, ids: null }, id: "o" }],
        [...MAILROOM_USING, CAP_OUT_OF_OFFICE],
      );
      if (!r || r.error || !r.value) return null;
      return ((r.value as unknown as GetResult<OutOfOffice>).list ?? []) as OutOfOffice[];
    } catch {
      return null;
    }
  }

  /** Ask the provider what it holds for this account now (drift), or null. */
  async outOfOfficeCheck(id: Id): Promise<OutOfOffice | null> {
    if (!this.hasCapability(CAP_OUT_OF_OFFICE)) return null;
    try {
      const [r] = await this.request(
        [{ name: "OutOfOffice/check", args: { accountId: this.accountId, ids: [id] }, id: "c" }],
        [...MAILROOM_USING, CAP_OUT_OF_OFFICE],
      );
      if (!r || r.error || !r.value) return null;
      return (((r.value as unknown as GetResult<OutOfOffice>).list ?? [])[0] as OutOfOffice | undefined) ?? null;
    } catch {
      return null;
    }
  }

  /** Save one account's form. The server works on the provider FIRST and saves
   * only what it accepted, so a refusal comes back as `{ ok: false, error }`
   * with the server's own sentence (for example, naming an active script). */
  async outOfOfficeSet(
    id: Id,
    update: OutOfOfficeUpdate,
  ): Promise<{ ok: true; value: OutOfOffice } | { ok: false; error: string }> {
    if (!this.hasCapability(CAP_OUT_OF_OFFICE)) return { ok: false, error: "" };
    const [r] = await this.request(
      [{ name: "OutOfOffice/set", args: { accountId: this.accountId, update: { [id]: update } }, id: "s" }],
      [...MAILROOM_USING, CAP_OUT_OF_OFFICE],
    );
    if (!r || r.error || !r.value) return { ok: false, error: "" };
    const v = r.value as unknown as {
      updated?: Record<Id, OutOfOffice | null> | null;
      notUpdated?: Record<Id, { description?: string }> | null;
    };
    const updated = v.updated?.[id];
    if (updated) return { ok: true, value: updated };
    return { ok: false, error: v.notUpdated?.[id]?.description ?? "" };
  }

  // -- Signed-in devices (#937, ADR-048 §6) ------------------------------------
  //
  // Sessions are not JMAP-synced objects: the list is read when the row opens
  // and again after every sign-out. SESSION ONLY on the server (a key is
  // refused). Both helpers add CAP_DEVICES to their own `using`, only when the
  // session advertises it, and never throw.

  /** True when the session advertises `urn:mailroom:devices`; null otherwise. */
  devicesCapability(): Record<string, unknown> | null {
    if (!this.hasCapability(CAP_DEVICES)) return null;
    return (this.session?.capabilities[CAP_DEVICES] as Record<string, unknown> | undefined) ?? {};
  }

  /** The live sessions, or null when they cannot be asked. An empty list is a
   * real answer (a server with no sessions: dev-auth). Never throws. */
  async signedInDevices(): Promise<SignedInDevice[] | null> {
    if (!this.hasCapability(CAP_DEVICES)) return null;
    try {
      const [r] = await this.request(
        [{ name: "SignedInDevice/get", args: { accountId: this.accountId, ids: null }, id: "d" }],
        [...MAILROOM_USING, CAP_DEVICES],
      );
      if (!r || r.error || !r.value) return null;
      return ((r.value as unknown as GetResult<SignedInDevice>).list ?? []) as SignedInDevice[];
    } catch {
      return null;
    }
  }

  /** Signs out the given sessions, or every session but this one (`"others"`
   * sends `["*others*"]`). null when the call could not be made or failed as a
   * whole. Never throws. */
  async signOutDevices(ids: string[] | "others"): Promise<{ destroyed: string[]; notDestroyed: string[] } | null> {
    if (!this.hasCapability(CAP_DEVICES)) return null;
    try {
      const [r] = await this.request(
        [
          {
            name: "SignedInDevice/set",
            args: { accountId: this.accountId, destroy: ids === "others" ? ["*others*"] : ids },
            id: "d",
          },
        ],
        [...MAILROOM_USING, CAP_DEVICES],
      );
      if (!r || r.error || !r.value) return null;
      const v = r.value as unknown as { destroyed?: string[] | null; notDestroyed?: Record<string, unknown> | null };
      return { destroyed: v.destroyed ?? [], notDestroyed: Object.keys(v.notDestroyed ?? {}) };
    } catch {
      return null;
    }
  }

  // -- API keys and approvals (#49, ADR-042) ----------------------------------
  //
  // SESSION ONLY on the server, and every helper here adds CAP_APIKEYS to its
  // own `using` and asks only when the session advertises it (CAP_PGP's reason).
  //
  // ⚠ THE TOKEN. ApiKey/set's `created` entry is the one response that ever
  // carries a key's token. apiKeyCreate hands it to its caller and keeps no
  // copy: nothing here caches responses, logs them, or writes them anywhere.

  /** The `urn:mailroom:apikeys` capability object, or null when absent. */
  apiKeysCapability(): ApiKeysCapability | null {
    if (!this.hasCapability(CAP_APIKEYS)) return null;
    return (this.session?.capabilities[CAP_APIKEYS] as ApiKeysCapability | undefined) ?? {};
  }

  private async apiKeysCall(name: string, args: Record<string, unknown>): Promise<CallResult | null> {
    if (!this.hasCapability(CAP_APIKEYS)) return null;
    const [r] = await this.request(
      [{ name, args: { accountId: this.accountId, ...args }, id: "k" }],
      [...MAILROOM_USING, CAP_APIKEYS],
    );
    return r ?? null;
  }

  /** Every key (revoked ones included), or null when it cannot be asked. Never throws. */
  async apiKeysGet(): Promise<ApiKey[] | null> {
    try {
      const r = await this.apiKeysCall("ApiKey/get", { ids: null });
      if (!r || r.error || !r.value) return null;
      return ((r.value as unknown as GetResult<ApiKey>).list ?? []) as ApiKey[];
    } catch {
      return null;
    }
  }

  /** ApiKey/set create. The token is in the answer exactly once. Never throws. */
  async apiKeyCreate(
    settings: ApiKeySettings,
  ): Promise<{ ok: true; key: ApiKey; token: string } | { ok: false; error: SetError }> {
    try {
      const r = await this.apiKeysCall("ApiKey/set", { create: { k: settings } });
      if (!r) return { ok: false, error: { type: "unavailable" } };
      if (r.error) return { ok: false, error: { type: r.error.type, description: r.error.description ?? "" } };
      const v = r.value as unknown as {
        created?: Record<string, (ApiKey & { token?: string }) | null> | null;
        notCreated?: Record<string, SetError> | null;
      };
      const created = v.created?.["k"];
      if (created && typeof created.token === "string") {
        const { token, ...key } = created;
        return { ok: true, key: key as ApiKey, token };
      }
      return { ok: false, error: v.notCreated?.["k"] ?? { type: "serverFail" } };
    } catch (err) {
      return { ok: false, error: { type: "transport", description: String(err) } };
    }
  }

  /** ApiKey/set update (name, limits, confirm scopes, `suspended`, ...). Never throws. */
  async apiKeyUpdate(id: Id, patch: ApiKeySettings): Promise<{ ok: true } | { ok: false; error: SetError }> {
    return this.apiKeySetOne({ update: { [id]: patch } }, id, "updated", "notUpdated");
  }

  /** ApiKey/set destroy: REVOKES the key (permanent). Never throws. */
  async apiKeyRevoke(id: Id): Promise<{ ok: true } | { ok: false; error: SetError }> {
    return this.apiKeySetOne({ destroy: [id] }, id, "destroyed", "notDestroyed");
  }

  private async apiKeySetOne(
    args: Record<string, unknown>,
    id: Id,
    done: "updated" | "destroyed",
    refused: "notUpdated" | "notDestroyed",
  ): Promise<{ ok: true } | { ok: false; error: SetError }> {
    try {
      const r = await this.apiKeysCall("ApiKey/set", args);
      if (!r) return { ok: false, error: { type: "unavailable" } };
      if (r.error) return { ok: false, error: { type: r.error.type, description: r.error.description ?? "" } };
      const v = r.value as unknown as Record<string, unknown>;
      const ok =
        done === "destroyed"
          ? Array.isArray(v["destroyed"]) && (v["destroyed"] as string[]).includes(id)
          : typeof v["updated"] === "object" && v["updated"] !== null && id in (v["updated"] as object);
      if (ok) return { ok: true };
      const refusals = v[refused] as Record<string, SetError> | null | undefined;
      return { ok: false, error: refusals?.[id] ?? { type: "serverFail" } };
    } catch (err) {
      return { ok: false, error: { type: "transport", description: String(err) } };
    }
  }

  /** ApiKey/getEvents for one key, newest first, or null. Never throws. */
  async apiKeyEvents(
    keyId: Id,
    limit = 50,
  ): Promise<{ list: ApiKeyEvent[]; suppressedRefusals: SuppressedRefusal[] } | null> {
    try {
      const r = await this.apiKeysCall("ApiKey/getEvents", { keyId, limit });
      if (!r || r.error || !r.value) return null;
      const v = r.value as unknown as { list?: ApiKeyEvent[]; suppressedRefusals?: SuppressedRefusal[] };
      return { list: v.list ?? [], suppressedRefusals: v.suppressedRefusals ?? [] };
    } catch {
      return null;
    }
  }

  /** PendingAction/get, optionally by state, or null when it cannot be asked. Never throws. */
  async pendingActionsGet(states?: readonly string[]): Promise<PendingAction[] | null> {
    try {
      const r = await this.apiKeysCall("PendingAction/get", { ids: null, ...(states ? { states } : {}) });
      if (!r || r.error || !r.value) return null;
      return ((r.value as unknown as GetResult<PendingAction>).list ?? []) as PendingAction[];
    } catch {
      return null;
    }
  }

  /**
   * PendingAction/set: approve or deny one request. `decided` is the server's
   * answer for an accepted decision — for an approval that was refused when it
   * ran, `state: "failed"` and the method error. A refused decision (expired,
   * already decided, key no longer active, draft changed) is `{ok: false}`
   * with the SetError. Never throws.
   */
  async pendingActionDecide(
    id: Id,
    state: "approved" | "denied",
  ): Promise<
    { ok: true; state: string; error: MethodErrorArgs | null } | { ok: false; error: SetError }
  > {
    try {
      const r = await this.apiKeysCall("PendingAction/set", { update: { [id]: { state } } });
      if (!r) return { ok: false, error: { type: "unavailable" } };
      if (r.error) return { ok: false, error: { type: r.error.type, description: r.error.description ?? "" } };
      const v = r.value as unknown as {
        updated?: Record<string, { state?: string; error?: MethodErrorArgs } | null> | null;
        notUpdated?: Record<string, SetError> | null;
      };
      const u = v.updated?.[id];
      if (u) return { ok: true, state: u.state ?? state, error: u.error ?? null };
      return { ok: false, error: v.notUpdated?.[id] ?? { type: "serverFail" } };
    } catch (err) {
      return { ok: false, error: { type: "transport", description: String(err) } };
    }
  }

  /** The user's own OpenPGP key(s), or null when this server has no keys API /
   * the call failed. Never throws. */
  async userKeysGet(): Promise<UserKey[] | null> {
    if (!this.hasCapability(CAP_PGP)) return null;
    try {
      const [r] = await this.request(
        [{ name: "UserKey/get", args: { accountId: this.accountId, ids: null }, id: "k" }],
        [...MAILROOM_USING, CAP_PGP],
      );
      if (!r || r.error || !r.value) return null;
      return (r.value as unknown as GetResult<UserKey>).list ?? [];
    } catch {
      return null;
    }
  }

  // -- Own-key management (#796, urn:mailroom:pgp) ---------------------------
  //
  // The reads fail soft into null, like userKeysGet. The WRITES THROW the
  // method error (JmapMethodError, whose `type` the Keys page maps to a plain
  // sentence in core/userKeys.ts) or a transport error, so the dialog that
  // asked can say what went wrong where the user is looking.
  //
  // ⚠ PASSPHRASES AND PRIVATE KEYS pass through these as arguments and are
  // never kept, logged or put in an error message here.

  /** `UserKey/get` with the instance's WKD facts, or null when it cannot be
   * asked. Never throws. */
  async userKeysRead(): Promise<UserKeysResult | null> {
    if (!this.hasCapability(CAP_PGP)) return null;
    try {
      const [r] = await this.request(
        [{ name: "UserKey/get", args: { accountId: this.accountId, ids: null }, id: "k" }],
        [...MAILROOM_USING, CAP_PGP],
      );
      if (!r || r.error || !r.value) return null;
      const v = r.value as unknown as GetResult<UserKey> & { wkdConfigured?: unknown; wkdDomain?: unknown };
      return {
        keys: v.list ?? [],
        wkdConfigured: typeof v.wkdConfigured === "boolean" ? v.wkdConfigured : null,
        wkdDomain: typeof v.wkdDomain === "string" ? v.wkdDomain : null,
      };
    } catch {
      return null;
    }
  }

  private pgpCall<T>(name: string, args: Record<string, unknown>): Promise<T> {
    if (!this.hasCapability(CAP_PGP)) {
      return Promise.reject(new JmapTransportError("this server does not support key management"));
    }
    return this.call<T>(name, { accountId: this.accountId, ...args }, [...MAILROOM_USING, CAP_PGP]);
  }

  /** `UserKey/generate` for one of the user's fixed identities. `expiresDays`
   * undefined is the server's default (two years), 0 is never. `expiresAt`
   * (#813) is a calendar date the server reads as the end of that day in UTC;
   * the server refuses both at once, so at most one is sent. */
  userKeyGenerate(
    identityId: Id,
    opts: { expiresDays?: number; expiresAt?: string; bind?: boolean } = {},
  ): Promise<{ id: Id; fingerprint: string; boundTo: Id | null; key: UserKey }> {
    return this.pgpCall("UserKey/generate", {
      identityId,
      ...(opts.expiresAt !== undefined
        ? { expiresAt: opts.expiresAt }
        : opts.expiresDays !== undefined
          ? { expiresDays: opts.expiresDays }
          : {}),
      ...(opts.bind ? { bind: true } : {}),
    });
  }

  /** `UserKey/replace` (#817): a new key for the address of `id`, bound and
   * (when the old one was) published, with the old key retired, in one step. */
  userKeyReplace(id: Id, expiry: KeyExpiryArg): Promise<UserKeyReplaceResult> {
    return this.pgpCall("UserKey/replace", { id, ...expiry });
  }

  /** `UserKey/import`: an armored private key (or the base64 of a binary one)
   * and its passphrase, if protected. */
  userKeyImport(
    privateKey: string,
    passphrase: string,
  ): Promise<{ id: Id; fingerprint: string; alreadyStored: boolean; key: UserKey }> {
    return this.pgpCall("UserKey/import", {
      privateKey,
      ...(passphrase !== "" ? { passphrase } : {}),
    });
  }

  /**
   * `UserKey/set` for ONE key and ONE change (the server refuses two at once,
   * except retire-and-unbind). Resolves to the SetError when the server refused
   * that update, null when it was applied; throws a method error.
   */
  async userKeyUpdate(
    id: Id,
    patch: { identityId?: Id | null; wkdPublished?: boolean; decryptOnly?: true },
  ): Promise<SetError | null> {
    const r = await this.userKeySet(id, patch);
    return r.ok ? null : r.refused;
  }

  /**
   * The same `UserKey/set`, keeping what a successful update said back: a
   * bind answers `{boundAddressListed}` and, when the key does not name the
   * address, `warning: "addressNotInKey"` (#812). The bind is made either way.
   */
  async userKeySet(
    id: Id,
    patch: { identityId?: Id | null; wkdPublished?: boolean; decryptOnly?: true },
  ): Promise<{ ok: true; updated: UserKeyUpdated } | { ok: false; refused: SetError }> {
    const res = await this.pgpCall<{
      updated?: Record<Id, unknown> | null;
      notUpdated?: Record<Id, SetError> | null;
    }>("UserKey/set", { update: { [id]: patch } });
    const refused = res.notUpdated?.[id];
    if (refused) return { ok: false, refused };
    if (res.updated && Object.hasOwn(res.updated, id)) {
      const v = res.updated[id];
      const o = v && typeof v === "object" ? (v as Record<string, unknown>) : {};
      return {
        ok: true,
        updated: {
          ...(typeof o["boundAddressListed"] === "boolean" ? { boundAddressListed: o["boundAddressListed"] } : {}),
          ...(typeof o["warning"] === "string" ? { warning: o["warning"] } : {}),
        },
      };
    }
    // Neither: the server said nothing about this id, which is not a success.
    return { ok: false, refused: { type: "serverFail" } };
  }

  /** `UserKey/extend`: the key's expiry becomes now + days, or (#813) the end
   * of the given calendar day in UTC. Exactly one of the two is sent. */
  userKeyExtend(
    id: Id,
    to: number | { expiresAt: string },
  ): Promise<{ id: Id; expiresAt: string | null; key: UserKey }> {
    return this.pgpCall("UserKey/extend", typeof to === "number" ? { id, days: to } : { id, expiresAt: to.expiresAt });
  }

  /** `UserKey/revoke`: irreversible. `confirm` is the last 8 hex digits of
   * the fingerprint, which the crypto process checks against the key. */
  userKeyRevoke(
    id: Id,
    reason: "compromised" | "superseded" | "retired",
    confirm: string,
  ): Promise<{ id: Id; revokedAt: string | null; key: UserKey }> {
    return this.pgpCall("UserKey/revoke", { id, reason, confirm });
  }

  /** `UserKey/export`: the key wrapped under `passphrase`, returned once in
   * this response and stored nowhere. */
  userKeyExport(
    id: Id,
    passphrase: string,
  ): Promise<{ id: Id; fingerprint: string; armoredKey: string; fileName: string }> {
    return this.pgpCall("UserKey/export", { id, passphrase });
  }

  /**
   * Every correspondent key the server holds (`ContactKey/get`, #796), with the
   * contacts they belong to (a back-referenced `Contact/get` in the same
   * batch), or null when it cannot be asked. Never throws.
   */
  async contactKeysRead(
    opts: { includeRemoved?: boolean } = {},
  ): Promise<{ keys: HeldContactKey[]; contacts: Contact[] } | null> {
    if (!this.hasCapability(CAP_PGP)) return null;
    const accountId = this.accountId;
    try {
      const results = await this.request(
        [
          {
            name: "ContactKey/get",
            // #815: removed keys too, each with removedAt, for the History list.
            args: opts.includeRemoved ? { accountId, includeRemoved: true } : { accountId },
            id: "ck",
          },
          {
            name: "Contact/get",
            args: {
              accountId,
              "#ids": { resultOf: "ck", name: "ContactKey/get", path: "/list/*/contactId" },
              properties: ["displayName", "addresses"],
            },
            id: "cg",
          },
        ],
        [...MAILROOM_USING, CAP_PGP],
      );
      const ck = results.find((r) => r.id === "ck");
      if (!ck || ck.error || !ck.value) return null;
      const keys = ((ck.value as unknown as { list?: HeldContactKey[] }).list ?? []) as HeldContactKey[];
      const cg = results.find((r) => r.id === "cg");
      // The names are a nicety: without them each key still lists the
      // addresses its own user IDs name.
      const contacts =
        cg && !cg.error && cg.value ? ((cg.value as unknown as GetResult<Contact>).list ?? []) : [];
      return { keys, contacts };
    } catch {
      return null;
    }
  }

  /** `ContactKey/history` (#815): what happened to one held or removed key,
   * oldest first. Throws the method error. */
  async contactKeyHistory(id: Id): Promise<ContactKeyEvent[]> {
    const r = await this.pgpCall<{ list?: ContactKeyEvent[] | null }>("ContactKey/history", { id });
    return r.list ?? [];
  }

  /** `ContactKey/refresh` (#818): look the key up again over WKD and merge a
   * renewal. Throws the method error (rateLimited, wkdUnavailable, notFound). */
  contactKeyRefresh(id: Id): Promise<ContactKeyRefreshResult> {
    return this.pgpCall("ContactKey/refresh", { id });
  }

  /** `ContactKey/set {destroy}` for one key: removes it (a soft delete since
   * #815: it is kept in history, and comes back if it is seen again), and the
   * pin when it was the pin. Resolves to the SetError when refused, null when
   * removed. */
  async contactKeyDestroy(id: Id): Promise<SetError | null> {
    const res = await this.pgpCall<{
      destroyed?: Id[] | null;
      notDestroyed?: Record<Id, SetError> | null;
    }>("ContactKey/set", { destroy: [id] });
    const refused = res.notDestroyed?.[id];
    if (refused) return refused;
    if (res.destroyed?.includes(id)) return null;
    return { type: "serverFail" };
  }

  /**
   * How many correspondents this server holds a key for, or null when it cannot
   * be asked.
   *
   * `limit: 0` + `calculateTotal: true` is the count WITHOUT the ids: the row
   * prints a number, so fetching a page of contacts to measure its length would
   * be both a bigger answer than the question and a WRONG one — a page's length
   * is bounded by the page, which is the #155 "a lower bound reads as a total"
   * failure. The total is read from the server's own `total`, and a response
   * that omits it yields null rather than 0.
   */
  async contactKeyCount(): Promise<number | null> {
    if (!this.hasCapability(CAP_PGP)) return null;
    try {
      const [r] = await this.request(
        [
          {
            name: "Contact/query",
            args: {
              accountId: this.accountId,
              filter: { hasKey: true },
              limit: 0,
              calculateTotal: true,
            },
            id: "q",
          },
        ],
        [...MAILROOM_USING, CAP_PGP],
      );
      if (!r || r.error || !r.value) return null;
      // Read as `unknown` rather than through QueryResult: the type declares
      // `total` required, but `calculateTotal` is what makes a server send it,
      // and trusting the declaration would turn an absent field into `NaN`
      // arithmetic or a blank in the state line instead of the honest null.
      const total = (r.value as { total?: unknown }).total;
      return typeof total === "number" ? total : null;
    } catch {
      return null;
    }
  }

  /**
   * THE CONTACT BEHIND ONE TYPED ADDRESS, with its keys (#448).
   *
   * The composer's recipient chips print `⚿ key held` / `no key` per recipient,
   * and this is the lookup behind a chip the user TYPED rather than picked (a pick
   * already has the Contact in hand). One address, one batched round trip:
   * `Contact/query {text}` then a back-referenced `Contact/get`, exactly as
   * `contactAutocomplete` does.
   *
   * ⚠ THREE RETURN VALUES, NOT TWO, and collapsing any pair is the #176 mistake:
   *   * a Contact  — this address is known, and its `keys` says the rest;
   *   * `null`     — the server ANSWERED and holds no contact for this address,
   *                  which is also the answer "no key is held for them";
   *   * `undefined` — the question could not be asked (no contacts capability, a
   *                  method error, a dead connection). The chip then prints
   *                  NOTHING, because "no key" would be a claim nobody made.
   *
   * ⚠ CAP_PGP RIDES ALONG ONLY WHEN THE SESSION ADVERTISES IT, exactly as
   * `userKeysGet` and `contactKeyCount` do. `Contact/get` itself is gated on
   * `urn:mailroom:contacts` (which IS in MAILROOM_USING), so this method works
   * without the keys capability — it simply comes back with no `keys` property,
   * which reads as "unknown" and is the truth. Naming an unadvertised URN in the
   * standing set would 400 the whole batch on every screen (see CAP_PGP in
   * types.ts), which is why it is added per call and never to MAILROOM_USING.
   *
   * ⚠ `properties` IS NOT SENT. The server serves `keys` for a null properties
   * list and filters to the named set otherwise (internal/api/contact.go
   * contactObject), so asking for a subset would be one more place that has to
   * remember to name `keys` — and forgetting it would read as "no key held" for
   * every recipient, silently.
   */
  async contactForAddress(address: string): Promise<Contact | null | undefined> {
    const text = address.trim();
    if (text === "") return undefined;
    const accountId = this.accountId;
    const using = this.hasCapability(CAP_PGP) ? [...MAILROOM_USING, CAP_PGP] : MAILROOM_USING;
    try {
      const results = await this.request(
        [
          {
            name: "Contact/query",
            // Bounded at a handful rather than 1: `filter.text` is a SUBSTRING
            // match over addresses and display names (internal/api/contact.go),
            // so the exact address wanted is not guaranteed to be the first hit —
            // `ada@example.com` also matches `ada@example.com.au`. The exact
            // match is picked out below.
            args: { accountId, filter: { text }, limit: 8 },
            id: "q",
          },
          {
            name: "Contact/get",
            args: {
              accountId,
              "#ids": { resultOf: "q", name: "Contact/query", path: "/ids" },
            },
            id: "g",
          },
        ],
        using,
      );
      const get = results.find((r) => r.id === "g");
      if (!get || get.error || !get.value) return undefined;
      const list = (get.value as unknown as GetResult<Contact>).list ?? [];
      const wanted = text.toLowerCase();
      const match = list.find((c) =>
        c.addresses.some((a) => a.address.toLowerCase() === wanted),
      );
      // A server that answered with no exact match holds no contact for this
      // address: `null`, which the chip reads as "no key". Distinct from the
      // `undefined` above, which is "could not ask".
      return match ?? null;
    } catch {
      return undefined;
    }
  }

  /** The clean-up ranking (#53, `SenderStats/get`): computed on demand, read
   * only. */
  senderStatsGet(args: {
    groupBy: SenderGroupBy;
    sort: SenderSortBy;
    sinceDays: number | null;
    position?: number;
    limit?: number;
  }): Promise<SenderStatsResult> {
    return this.call("SenderStats/get", { accountId: this.accountId, ...args });
  }

  /** Follow-up nudges due now (#58, `FollowUp/get`): computed on read. */
  followUpGet(): Promise<FollowUpResult> {
    return this.call("FollowUp/get", { accountId: this.accountId });
  }

  /** The user's Done on one nudge (#58, `FollowUp/set` dismiss). Resolves to
   * whether the server recorded it; a dismissal never resurfaces. */
  async followUpDismiss(emailId: Id, kind: FollowUpItem["kind"]): Promise<boolean> {
    const r = await this.call<{ dismissed: Id[] }>("FollowUp/set", {
      accountId: this.accountId,
      dismiss: { [emailId]: kind },
    });
    return r.dismissed.includes(emailId);
  }

  /** The messages ONE clean-up row counted (#53, `SenderStats/emailIds`):
   * at most 1,000 ids from `position`, newest first, and the row's total. */
  senderStatsEmailIds(args: {
    groupBy: SenderGroupBy;
    key: string;
    since: string | null;
    position: number;
    limit: number;
  }): Promise<{ ids: Id[]; total: number }> {
    return this.call("SenderStats/emailIds", { accountId: this.accountId, ...args });
  }

  /** Contact/set (#59): the user's markers only — isVip, isBlocked,
   * blockDestination. */
  contactSet(args: SetArgs): Promise<SetResult<Contact>> {
    return this.call("Contact/set", args as unknown as Record<string, unknown>);
  }

  /**
   * The contacts carrying a marker (#59) — Settings' VIP and blocked lists —
   * in one batched round trip, like `contactAutocomplete`. `total` is the
   * server's count; when it exceeds the list (the server pages at 100) the
   * caller says so rather than presenting the list as all of them.
   */
  async contactsMarked(
    filter: { isVip?: boolean; isBlocked?: boolean },
    limit = 100,
  ): Promise<{ list: Contact[]; total: number }> {
    const accountId = this.accountId;
    const results = await this.request([
      { name: "Contact/query", args: { accountId, filter, limit }, id: "q" },
      {
        name: "Contact/get",
        args: { accountId, "#ids": { resultOf: "q", name: "Contact/query", path: "/ids" } },
        id: "g",
      },
    ]);
    const query = results.find((r) => r.id === "q");
    const get = results.find((r) => r.id === "g");
    if (!query || query.error || !get || get.error || !get.value) {
      throw new Error("could not read contacts");
    }
    const q = query.value as unknown as QueryResult;
    const list = (get.value as unknown as GetResult<Contact>).list ?? [];
    const byId = new Map(list.map((c) => [c.id, c]));
    return {
      list: (q.ids ?? []).map((id) => byId.get(id)).filter((c): c is Contact => Boolean(c)),
      total: typeof q.total === "number" ? q.total : list.length,
    };
  }

  /**
   * THE CONTACTS PAGE'S QUERY (#816): `Contact/query` with the page's filter,
   * sort and window, and a back-referenced `Contact/get`, in one round trip. The
   * query's id order is authoritative (Contact/get's list order is not), and
   * `total` is the server's own count, never the page's length (#155).
   *
   * CAP_PGP rides along when the session advertises it, so `keys` comes back
   * for the "key" chip; without it the property is absent ("cannot say").
   * Throws the method error (`JmapMethodError`) or the transport error.
   */
  async contactsQuery(args: {
    filter: Record<string, unknown>;
    sort?: { property: string; isAscending: boolean }[];
    position?: number;
    limit?: number;
  }): Promise<{ list: Contact[]; ids: Id[]; total: number; position: number }> {
    const accountId = this.accountId;
    const using = this.hasCapability(CAP_PGP) ? [...MAILROOM_USING, CAP_PGP] : MAILROOM_USING;
    const results = await this.request(
      [
        {
          name: "Contact/query",
          args: {
            accountId,
            filter: args.filter,
            ...(args.sort ? { sort: args.sort } : {}),
            ...(args.position !== undefined ? { position: args.position } : {}),
            ...(args.limit !== undefined ? { limit: args.limit } : {}),
            calculateTotal: true,
          },
          id: "q",
        },
        {
          name: "Contact/get",
          args: { accountId, "#ids": { resultOf: "q", name: "Contact/query", path: "/ids" } },
          id: "g",
        },
      ],
      using,
    );
    const query = results.find((r) => r.id === "q");
    const get = results.find((r) => r.id === "g");
    if (!query) throw new JmapTransportError("no result for Contact/query");
    if (query.error) throw query.error;
    if (!get) throw new JmapTransportError("no result for Contact/get");
    if (get.error) throw get.error;
    const q = query.value as unknown as QueryResult;
    const list = (get.value as unknown as GetResult<Contact>).list ?? [];
    const byId = new Map(list.map((c) => [c.id, c]));
    const ids = q.ids ?? [];
    return {
      ids,
      list: ids.map((id) => byId.get(id)).filter((c): c is Contact => Boolean(c)),
      total: typeof q.total === "number" ? q.total : list.length,
      position: typeof q.position === "number" ? q.position : (args.position ?? 0),
    };
  }

  /** One contact, read fresh (#816: its statistics are not synced, so the
   * page re-reads it when it opens). Null when the server says it has no such
   * contact (merged away, or never). Throws the method error. */
  async contactGetOne(id: Id): Promise<Contact | null> {
    const using = this.hasCapability(CAP_PGP) ? [...MAILROOM_USING, CAP_PGP] : MAILROOM_USING;
    const r = await this.call<GetResult<Contact>>("Contact/get", { accountId: this.accountId, ids: [id] }, using);
    return (r.list ?? []).find((c) => c.id === id) ?? null;
  }

  /** `Contact/merge` (#816): `from` into `into`. Throws the method error
   * (invalidArguments, notFound, stateMismatch, notesTooLong). */
  contactMerge(into: Id, from: Id): Promise<ContactMergeResult> {
    return this.call("Contact/merge", { accountId: this.accountId, into, from });
  }

  /** `Contact/separate` (#816): split the named addresses off into new
   * contacts. Throws the method error (invalidArguments, notFound,
   * stateMismatch). */
  contactSeparate(id: Id, addressIds: Id[]): Promise<ContactSeparateResult> {
    return this.call("Contact/separate", { accountId: this.accountId, id, addressIds });
  }

  searchSnippetGet(args: {
    accountId: string;
    filter: Record<string, unknown>;
    emailIds: Id[];
  }): Promise<{ list: SearchSnippet[]; notFound: Id[] }> {
    return this.call(
      "SearchSnippet/get",
      args as unknown as Record<string, unknown>,
    );
  }

  // -- Blobs and push (RFC 8620 §6, §7.3) ------------------------------------
  // The URLs come from the SESSION, never from a constant here: the server owns
  // its own routes and says what they are (core/jmap/blobs.ts expands them).

  /** The URL that serves one blob — a whole message, or one MIME part of it
   * (`Email.attachments[].blobId`). Safe to use as an `<a href>` or `<img src>`:
   * the server serves every blob as an attachment with a type from its own
   * allowlist, so a hostile part cannot render as HTML on our origin. */
  blobUrl(blobId: BlobId, name: string, type?: string | null): string {
    return downloadUrl(this.requireSession(), this.accountId, blobId, name, type);
  }

  /** The URL that serves one part for INLINE rendering (#131) — a different door
   * from blobUrl, with different rules.
   *
   * Safe as an `<iframe src>` and safe ONLY there: that route serves a one-entry
   * allowlist (application/pdf) and answers 415 for everything else, and the bytes
   * come back with `Content-Security-Policy: default-src 'none'; sandbox`, which
   * makes the document's origin OPAQUE however it is embedded. The iframe needs no
   * `sandbox` attribute — and must not have one, because a sandboxed frame is an
   * opaque origin to the server too and the response's
   * `Cross-Origin-Resource-Policy: same-origin` then refuses it.
   *
   * The template comes from the session, like every other URL here — the server
   * owns its routes. */
  previewUrl(blobId: BlobId, name: string): string | null {
    return previewUrl(this.requireSession(), this.accountId, blobId, name);
  }

  /** Asks the server whether a download or preview URL would be served, BEFORE
   * the browser navigates to it (a HEAD, same cookie as the real request).
   *
   * Navigating to a refused URL lands the user on the browser's bare "403
   * Forbidden" page with no way back into the app and no reason given. The
   * refusal carries its reason in headers (see blockedFromResponse), and a
   * fetch can read them where a navigation cannot. The caller shows the
   * message in-app and navigates only on `ok`.
   *
   * Use it for STORED parts only (their verdict is a database read). For a part
   * of a decrypted message a probe plus the real request is two decryptions, two
   * scans and two audit rows (#804): use fetchBlobOnce, or navigate straight to
   * the URL and let the server's own refusal page explain (#805).
   * `url` must be one of this client's own download/preview URLs. */
  async probeBlob(url: string): Promise<BlobProbe> {
    let res: Response;
    try {
      res = await this.doFetch(url, { method: "HEAD", credentials: "include" });
    } catch {
      return { kind: "failed" };
    }
    if (res.ok) return { kind: "ok" };
    const blocked = blockedFromResponse(res);
    return blocked ? { kind: "blocked", blocked } : { kind: "failed" };
  }

  /** Fetches a download URL ONCE and returns the bytes, for a part of a
   * decrypted message (#804): one request, so one decryption, one scan and one
   * audit row, where a probe followed by a navigation made two of each. A refusal
   * is read from the same response's headers, as probeBlob reads it.
   *
   * THE BLOB IS TYPED application/octet-stream, whatever the server said. It is
   * only ever saved: a blob: URL is same-origin, so giving it a renderable type
   * would let a browser display attacker bytes on our origin, which the
   * `attachment` disposition on the real route exists to prevent. Bounded by the
   * server's decrypted-output cap. `url` must be one of this client's own
   * download URLs. */
  async fetchBlobOnce(url: string): Promise<BlobFetch> {
    let res: Response;
    try {
      res = await this.doFetch(url, { method: "GET", credentials: "include" });
    } catch {
      return { kind: "failed" };
    }
    if (!res.ok) {
      const blocked = blockedFromResponse(res);
      return blocked ? { kind: "blocked", blocked } : { kind: "failed" };
    }
    try {
      const buf = await res.arrayBuffer();
      return { kind: "ok", blob: new Blob([buf], { type: "application/octet-stream" }) };
    } catch {
      return { kind: "failed" };
    }
  }

  /** Fetches one inline (Content-ID) message part for rendering INSIDE the
   * message sandbox, as a Blob whose type is the server's own allowlisted image
   * type — or null if the part is not a raster image.
   *
   * Inline images are NOT remote content: the bytes are attached to the message
   * and this fetch goes to OUR OWN download endpoint (same origin, session
   * cookie), so it reveals nothing to the sender and makes no external request.
   * The caller turns the Blob into a `data:` URL (ui/inlineImages.ts), which is
   * why the message CSP can keep `img-src data:` and never admit a remote source.
   *
   * The type gate is the client half of blob.go's allowlist: that endpoint maps
   * text/html and image/svg+xml (the scripting contexts) to
   * application/octet-stream, and we render only what comes back as a real raster
   * image — so a hostile "image" part cannot become an active document here. */
  async fetchInlineImage(blobId: BlobId): Promise<Blob | null> {
    return this.fetchPart(blobId, INLINE_IMAGE_TYPES, "inline");
  }

  /** Fetches one message part for PREVIEW (Forgejo #131), returning the bytes only
   * when the server served them as one of `allowed` — otherwise null.
   *
   * THIS OPENS NO NEW DOOR, and that is the whole design. docs/hostile-content.md
   * §2.4 says there are exactly two: rendering is sanitized, downloading is inert.
   * A preview is not a third one, because `Content-Disposition: attachment`
   * constrains NAVIGATION and not `fetch()` — so this is the ordinary, inert
   * download door, unchanged, and the decision to render what came back is made
   * here on the client. The server has no preview mode, no inline disposition and
   * no new route; if it ever needs one, the third door is being opened.
   *
   * `allowed` IS THE CONTROL, and it is deliberately the caller's to state rather
   * than a default here: a caller must name the types it is prepared to render, so
   * adding a renderer cannot silently widen what any other renderer accepts. The
   * type compared is the one the SERVER chose from blob.go's allowlist, never the
   * part's own declared type — that endpoint maps text/html, image/svg+xml and the
   * XML family to application/octet-stream, so those fail this gate and the caller
   * falls back to a download.
   *
   * Bounded by `maxBytes` when given: the Content-Length is refused before any body
   * is read, so an oversized part costs nothing. It is a bound on what we will feed
   * a renderer, not a statement about the attachment. */
  async fetchPart(
    blobId: BlobId,
    allowed: ReadonlySet<string>,
    name = "part",
    maxBytes?: number,
    onBlocked?: (blocked: BlobBlocked) => void,
  ): Promise<Blob | null> {
    const res = await this.doFetch(this.blobUrl(blobId, name), {
      method: "GET",
      credentials: "include",
    });
    if (!res.ok) {
      // A scan refusal is reported, not swallowed: the caller can say WHY there
      // is nothing to show. Still null, so a caller that passes no callback
      // behaves exactly as before.
      const blocked = blockedFromResponse(res);
      if (blocked) onBlocked?.(blocked);
      return null;
    }
    const ct = (res.headers.get("Content-Type") ?? "").split(";")[0]?.trim().toLowerCase() ?? "";
    if (!allowed.has(ct)) return null;
    if (maxBytes !== undefined) {
      const declared = Number(res.headers.get("Content-Length") ?? "");
      if (Number.isFinite(declared) && declared > maxBytes) return null;
    }
    const buf = await res.arrayBuffer();
    // Checked again on the bytes actually received: Content-Length is a claim, and
    // a chunked response need not send one at all.
    if (maxBytes !== undefined && buf.byteLength > maxBytes) return null;
    return new Blob([buf], { type: ct });
  }

  /** Fetches one image through the server's image proxy (ADR-026), returning the
   * bytes only when the server served them as one of the raster types the proxy
   * re-serves — otherwise null (#667).
   *
   * The APP makes this request, same-origin and with the session cookie. The
   * message frame cannot: it is an opaque origin, and the cookie is withheld
   * from it. `path` is the `/jmap/proxy/…` path the server parked in the body;
   * anything of another shape is refused here, so this can never be pointed at
   * a different route or another host. */
  async fetchProxiedImage(path: string, maxBytes?: number): Promise<Blob | null> {
    if (!PROXY_PATH.test(path)) return null;
    const res = await this.doFetch(path, { method: "GET", credentials: "include" });
    if (!res.ok) return null;
    const ct = (res.headers.get("Content-Type") ?? "").split(";")[0]?.trim().toLowerCase() ?? "";
    if (!PROXIED_IMAGE_TYPES.has(ct)) return null;
    if (maxBytes !== undefined) {
      const declared = Number(res.headers.get("Content-Length") ?? "");
      if (Number.isFinite(declared) && declared > maxBytes) return null;
    }
    const buf = await res.arrayBuffer();
    if (maxBytes !== undefined && buf.byteLength > maxBytes) return null;
    return new Blob([buf], { type: ct });
  }

  /** Uploads a file and returns the blobId that names it — what `Email/set
   * create` puts in `attachments[].blobId`. */
  async upload(file: Blob): Promise<UploadResult> {
    const session = this.requireSession();
    return uploadBlob(this.doFetch, uploadUrl(session, this.accountId), file);
  }

  /** The EventSource URL for the push channel (core/sync.ts subscribes). */
  pushUrl(opts: { types?: readonly string[]; ping?: number } = {}): string {
    return eventSourceUrl(this.requireSession(), opts);
  }

  // -- Web Push (Forgejo #42, ADR-016) ---------------------------------------
  // DISTINCT from pushUrl() above: that is RFC 8620 §7.3 in-app SSE for /changes
  // (live while a tab is open). These manage RFC 8030 WebPush subscriptions —
  // OS-level notifications the server can deliver while no tab is open. The
  // browser-side subscribe/permission flow lives in src/app/push.ts (it needs
  // DOM/ServiceWorker APIs this DOM-free module must not touch); these three are
  // only the JMAP wire.

  /** The server's VAPID application public key from the `urn:mailroom:push`
   * session capability (ADR-016) — a base64url (unpadded) uncompressed P-256
   * point — or null when this server does not offer Web Push. Its presence is
   * the single feature gate the UI checks before offering to enable push. */
  vapidPublicKey(): string | null {
    const cap = this.session?.capabilities[CAP_PUSH] as PushCapability | undefined;
    const key = cap?.vapidPublicKey;
    return typeof key === "string" && key.length > 0 ? key : null;
  }

  /** Whether this server fetches remote images itself instead of letting the
   * browser fetch them (ADR-026), from the `urn:mailroom:remotecontent` session
   * capability.
   *
   * ⚠ IT CHANGES WHAT "Show remote content" MEANS, which is why the reading pane
   * asks. Without the proxy, loading images hands the sender your address and
   * your browser. With it, your own server fetches them and the sender sees the
   * server instead. Both still tell the sender the message was opened — only a
   * cache changes that — so neither banner may claim more than it does.
   *
   * Defaults to FALSE when the server says nothing, which is the older, weaker
   * behaviour: assuming the stronger one would overstate the protection to the
   * user on exactly the servers that do not have it. */
  imageProxyEnabled(): boolean {
    const cap = this.session?.capabilities[CAP_REMOTE_CONTENT] as
      | { imageProxy?: unknown }
      | undefined;
    return cap?.imageProxy === true;
  }

  /** Registers/destroys a browser PushSubscription with the server. `create`'s
   * values carry `{ endpoint, keys: { p256dh, auth }, deviceName?, notifyMode? }`;
   * `update` may change one device's `notifyMode` (#59); `destroy` carries
   * stored subscription ids. Push-scoped `using` (see PUSH_USING). */
  webPushSubscriptionSet(args: SetArgs): Promise<SetResult<WebPushSubscription>> {
    return this.call(
      "WebPushSubscription/set",
      args as unknown as Record<string, unknown>,
      PUSH_USING,
    );
  }

  /** Lists this user's stored Web Push subscriptions (no keys — write-only). */
  webPushSubscriptionGet(accountId: string): Promise<WebPushSubscriptionGetResult> {
    return this.call("WebPushSubscription/get", { accountId }, PUSH_USING);
  }

  private requireSession(): JmapSession {
    const s = this.session;
    if (!s) throw new JmapTransportError("not connected: call connect() first");
    return s;
  }
}
