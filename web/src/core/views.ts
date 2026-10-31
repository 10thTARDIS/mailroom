// SPDX-License-Identifier: AGPL-3.0-only

// The view model: the named things a user navigates to, and how each one turns
// into JMAP Email/query filters. DOM-free, framework-agnostic (spec §12).
//
// This is where the SERVER'S filter limits are absorbed once, so no UI code has
// to know about them (internal/api/email.go, internal/api/search.go):
//
//   - `filter` is a FLAT condition object. There is no FilterOperator (AND/OR/
//     NOT group) — the server rejects one with `unsupportedFilter`. So a view
//     that is inherently a disjunction (the unified inbox = "in ANY account's
//     INBOX") cannot be one query. It becomes N queries whose results the
//     client merges. `QueryPlan.filters` is that list.
//   - a Mailbox now carries its owning account id (`accountId`, #488) and the
//     rail uses it to group the folder disclosure, but per-account VIEWS still
//     ride the search `account:<id>` operator: Email/query filters over the
//     EMAIL's properties, not the Mailbox object's, so scoping a query to an
//     account is a filter term, not a Mailbox field lookup. Account ids for
//     that operator come from Identity.accountId, the same source used before
//     Mailbox exposed one.
//   - snoozed mail is excluded HERE, per view, rather than by the server
//     quietly rewriting filters (#51). A JMAP filter must mean what it says: a
//     query for `inMailbox: X` that silently dropped some of that mailbox's
//     messages would be a lie to every other client. So the INBOX view asks for
//     `isSnoozed: false` explicitly and the Snoozed view asks for the
//     complement; see hidesSnoozed for why it is the inbox and nothing else.
//     Starred, Unread, label and search views do not filter either: those are
//     questions the user asked outright ("what did I star?"), and answering them
//     with a silent omission is worse than showing a message that is sleeping.
//   - a view can be narrowed to ONE MAIL ACCOUNT, and since #375 that is a
//     structured `inAccount` condition rather than the search operator
//     `account:<id>`. It ANDs with everything else in the flat filter, so
//     "this account's starred mail" is one query with two conditions — which is
//     what lets the account switcher scope every list rather than only the ones
//     that happened to have no other condition. The legacy `{kind:"account"}`
//     view (and `#/account/<id>`) still resolves through the search operator
//     and still works; nothing has to be rewritten to keep a bookmark alive.
//   - `hasKeyword` on a LABEL ATOM works, and the ⚠ that used to sit here saying
//     it does not is stale. `keywordPredicate` (internal/store/jmap_email.go)
//     resolves a keyword through `label_keyword_map` and, for a label, ORs in an
//     EXISTS over `message_labels` — so `hasKeyword: "MailroomLabel-work"`
//     matches mail carrying the label whether or not the atom is in the raw
//     `messages.keywords` array. All mail's `?label=` therefore filters
//     STRUCTURALLY, with no dependency on the search index. The LABEL VIEW still
//     goes through `label:"Name"`: that is a separate, deliberate change (the
//     operator also resolves a label by NAME, which is what the search box
//     produces), and it is not this issue's to make.

import type {
  CategoryTab,
  Comparator,
  EmailFilter,
  Id,
  Label,
  Mailbox,
  MailboxRole,
} from "./jmap/types";
import { KEYWORD_FLAGGED, KEYWORD_SEEN } from "./jmap/types";

/**
 * The account scope a structured view can carry (#375).
 *
 * OPTIONAL EVERYWHERE, AND ABSENT MEANS EVERY ACCOUNT — not "the primary one".
 * It is spelled as a mixin rather than repeated inline so that adding a kind to
 * the scoped set is one union member changing shape, and so the ⚠ in
 * routes.ts's table can point at one name.
 *
 * `mailbox` and `search` deliberately have NO scope: a folder already belongs
 * to exactly one account (`inMailbox` + `inAccount` is either redundant or
 * empty), and a search carries its own `account:<id>` operator, which is the
 * syntax the user typed and must not be silently intersected with a second one.
 */
export interface Scoped {
  accountId?: Id;
}

/** A navigable view. */
export type View =
  | ({ kind: "role"; role: Exclude<MailboxRole, null> } & Scoped)
  | { kind: "mailbox"; mailboxId: Id }
  | ({ kind: "starred" } & Scoped)
  | ({ kind: "priority" } & Scoped)
  | ({ kind: "needsCategory" } & Scoped)
  | ({ kind: "snoozed" } & Scoped)
  | ({ kind: "unread" } & Scoped)
  | ({ kind: "label"; labelId: Id } & Scoped)
  /**
   * ALL MAIL (#375): everything that is not in the trash, in one query.
   *
   * It is not a folder and not the archive — the archive is where mail GOES,
   * all mail is where mail IS. So it carries no `inMailbox` at all (the server
   * excludes trash unless a query names the trash mailbox itself:
   * internal/store/jmap_email.go QueryEmails), and it does not filter the
   * snooze: sleeping mail is still mail, and this is the list that promises to
   * hold all of it.
   *
   * The three optional narrowings are the rail's: an account, an AREA — by
   * `inArea`, the area's own claim, not by its label, so mail a feed resolved
   * into the area is in it — and a label.
   */
  | ({ kind: "all"; areaId?: Id; labelId?: Id } & Scoped)
  /**
   * THE OUTBOX (#448, design pass-2 §8): what has not gone yet.
   *
   * ⚠ IT IS NOT AN Email/query AT ALL, and that is the one thing to know about
   * it. Every other kind here becomes a filter over messages; this one is a list
   * of `EmailSubmission` objects, which the client already holds (the changes
   * loop tracks them — core/sync.ts) and which no Email filter can express: a
   * submission is a JOB, and "queued to go at 09:00" is a property of the job
   * rather than of the draft it will send. So `planFor` gives it NO filters, the
   * list layer loads nothing for it, and ui/routes/OutboxRoute.tsx renders it
   * from `state.submissions` instead (its arithmetic is ui/outbox.ts).
   *
   * It is in this union anyway because it is a PLACE with a URL — `#/outbox`, the
   * rail entry that until now pointed at Sent with a TODO beside it — and every
   * place this client can be on is a View so that the rail, the route table and
   * the account switcher have one vocabulary rather than two.
   *
   * NO ACCOUNT SCOPE, deliberately: `EmailSubmission` carries an `identityId` and
   * no account, so narrowing it would mean resolving every submission's identity
   * to its account and hiding rows on the result — i.e. a scoped Outbox would
   * silently under-report what is waiting whenever an identity had not loaded.
   * "What of mine has not gone" is a question about the login.
   */
  | { kind: "outbox" }
  /**
   * THE LEGACY PER-ACCOUNT VIEW (`#/account/<id>`). Kept working, deliberately:
   * it is a hash people have bookmarked, and `Scoped` above is what replaces it
   * for everything new. It is a search (`account:<id>`), so it needs the index;
   * the scoped forms above do not.
   */
  | { kind: "account"; accountId: Id }
  /**
   * One Gmail-style category tab, scoped to the inbox (#228).
   *
   * A TAB IS NOT A FOLDER AND NOT A LABEL. It is a second axis over the same
   * inbox: labels say what mail is ABOUT, tabs say what it DOES. So this view is
   * the inbox narrowed by `inTab`, never a standalone list — a "Promotions" that
   * also showed archived and sent mail would be a third thing that is neither.
   */
  | ({ kind: "tab"; tab: CategoryTab } & Scoped)
  /**
   * A search (#724 added the two optional parts).
   *
   * `prefix` is a search the user is STILL TYPING: the live results the box
   * shows between keystrokes, which treat the final word as a prefix
   * (`queryPrefix`). Pressing Enter runs the same text without it — a whole word
   * keeps typo tolerance, which a prefix match does not have.
   *
   * `sort: "relevance"` is the best-match order (the default, absent, is newest
   * first). Both are part of the view's identity and of its URL, so a reload or
   * a shared link shows the same list.
   */
  | { kind: "search"; query: string; prefix?: true; sort?: "relevance" };

/** The category-tab vocabulary, for narrowing a string off a URL. It mirrors the
 * server's closed enum; the session capability advertises the same list, and a
 * client that wanted to render the strip generically should read it from there
 * rather than from this constant. */
const CATEGORY_TABS: readonly CategoryTab[] = [
  "primary",
  "social",
  "promotions",
  "updates",
  "forums",
];

function isCategoryTab(s: string): s is CategoryTab {
  return (CATEGORY_TABS as readonly string[]).includes(s);
}

/**
 * The view a category-tab button navigates to.
 *
 * ⚠ PRIMARY IS A REAL TAB, not the plain inbox. The first version of the strip
 * routed Primary to `{kind:"role", role:"inbox"}` on the reasoning that Primary
 * "already collects everything no other tab claimed". THAT WAS WRONG, and
 * dogfooding caught it in minutes: the inbox view has no `inTab` filter at all,
 * so it lists EVERY message — every mail therefore appeared under Primary *and*
 * under its real tab, which makes the whole strip look broken.
 *
 * The inbox and Primary are different questions. "Inbox" is all of it; "Primary"
 * is the mail no other enabled tab claimed, plus whatever folded in from a tab
 * that is switched off. Only the server can answer the second, which is exactly
 * what `inTab` asks it.
 */
export function tabToView(tab: CategoryTab): View {
  return { kind: "tab", tab };
}

/**
 * Which tab the strip should show as current, or null.
 *
 * NULL ON THE PLAIN INBOX, deliberately: that view is not a tab, and
 * highlighting Primary there would claim the list is filtered when it is not —
 * the same lie the routing bug above told.
 */
export function currentTab(view: View): CategoryTab | null {
  return view.kind === "tab" ? view.tab : null;
}

/** The account a view is scoped to, or null — the one place that knows which
 * kinds carry a scope, so nothing else has to narrow the union by hand. */
export function accountOf(v: View): Id | null {
  return v.kind === "mailbox" ||
    v.kind === "search" ||
    v.kind === "account" ||
    v.kind === "outbox"
    ? null
    : v.accountId ?? null;
}

/** The same view with its account scope removed (or replaced). Written as a
 * helper for the same reason routes.ts's `withM` is: dropping an optional key
 * from a discriminated union needs the cast to live in exactly one place. */
export function withAccount(v: View, accountId: Id | null): View {
  if (
    v.kind === "mailbox" ||
    v.kind === "search" ||
    v.kind === "account" ||
    v.kind === "outbox"
  ) {
    return v;
  }
  const { accountId: _drop, ...rest } = v;
  return (accountId ? { ...rest, accountId } : rest) as View;
}

/**
 * A stable key for a view (cache/URL identity).
 *
 * ⚠ THE SCOPE IS PART OF THE IDENTITY. `state.tsx` keeps the View object stable
 * while this key is unchanged and reloads the list when it moves, so a key that
 * ignored `accountId` would leave the previous account's mail on screen after
 * an account switch — with no request made and nothing to say so.
 */
export function viewKey(v: View): string {
  const acct = accountOf(v);
  const scope = acct ? `@${acct}` : "";
  switch (v.kind) {
    case "role":
      return `role:${v.role}${scope}`;
    case "mailbox":
      return `mailbox:${v.mailboxId}`;
    case "starred":
      return `starred${scope}`;
    case "priority":
      return `priority${scope}`;
    case "needsCategory":
      return `needsCategory${scope}`;
    case "snoozed":
      return `snoozed${scope}`;
    case "unread":
      return `unread${scope}`;
    case "label":
      return `label:${v.labelId}${scope}`;
    case "all":
      return `all:${v.areaId ?? ""}:${v.labelId ?? ""}${scope}`;
    case "account":
      return `account:${v.accountId}`;
    case "outbox":
      return "outbox";
    case "tab":
      return `tab:${v.tab}${scope}`;
    case "search":
      return `search:${v.prefix ? "prefix:" : ""}${v.sort === "relevance" ? "relevance:" : ""}${v.query}`;
  }
}

/** Round-trips a view through the URL hash (hash routing is deliberate: the Go
 * server serves the SPA with a plain http.FileServer, so a deep path like
 * /thread/abc would 404 on reload — cmd/mailroom/main.go webHandler). */
export function viewToHash(v: View): string {
  switch (v.kind) {
    case "search":
      return `#/search/${encodeURIComponent(v.query)}`;
    case "role":
      return `#/${v.role}`;
    case "mailbox":
      return `#/mailbox/${v.mailboxId}`;
    case "label":
      return `#/label/${v.labelId}`;
    case "account":
      return `#/account/${v.accountId}`;
    case "tab":
      return `#/tab/${v.tab}`;
    case "needsCategory":
      // Spelled out rather than falling through to `#/needsCategory`: this is a
      // link people paste to each other ("start here"), and a hash is read by
      // humans before it is parsed by anything.
      return "#/needs-category";
    case "outbox":
      // Spelled out rather than left to the default below, for the reason
      // needs-category is: this is a hash a person reads before anything parses
      // it, and the default would happen to be right today and silently wrong
      // the moment a kind's name and its URL diverge.
      return "#/outbox";
    case "all":
      // The PATH only. All mail's narrowings (`?area=`, `?label=`, `?acct=`)
      // are query parameters, which this function does not emit for any view —
      // `formatRoute` owns them, and `viewRoute` turns an all-mail view back
      // into the all-mail PAGE so none of them is dropped on the way.
      return "#/all";
    default:
      return `#/${v.kind}`;
  }
}

export function viewFromHash(hash: string): View {
  const raw = hash.replace(/^#\/?/, "");
  const [head, ...rest] = raw.split("/");
  const tail = rest.join("/");
  switch (head) {
    case "search":
      return { kind: "search", query: decodeURIComponent(tail) };
    case "mailbox":
      return tail ? { kind: "mailbox", mailboxId: tail } : { kind: "role", role: "inbox" };
    case "label":
      return tail ? { kind: "label", labelId: tail } : { kind: "role", role: "inbox" };
    case "account":
      return tail ? { kind: "account", accountId: tail } : { kind: "role", role: "inbox" };
    case "tab":
      // An unknown tab in a pasted URL falls back to the inbox rather than
      // producing a view the server would refuse with invalidArguments. The
      // vocabulary is closed, so anything else is a typo or a stale link.
      return isCategoryTab(tail) ? { kind: "tab", tab: tail } : { kind: "role", role: "inbox" };
    case "outbox":
      return { kind: "outbox" };
    case "starred":
      return { kind: "starred" };
    case "priority":
      return { kind: "priority" };
    case "needs-category":
      return { kind: "needsCategory" };
    case "snoozed":
      return { kind: "snoozed" };
    case "unread":
      return { kind: "unread" };
    case "inbox":
    case "archive":
    case "sent":
    case "drafts":
    case "trash":
    case "junk":
      return { kind: "role", role: head };
    default:
      return { kind: "role", role: "inbox" };
  }
}

/**
 * A QueryPlan is what a view becomes on the wire: one or more Email/query
 * filters whose id lists are merged (by receivedAt, desc) into one list.
 *
 * `role` is the mailbox role this view represents, when it has one. The
 * optimistic overlay uses it to decide what a locally-archived/trashed message
 * should do in THIS list (leave it, or appear in it) — see core/store.ts.
 */
export interface QueryPlan {
  filters: EmailFilter[];
  role: Exclude<MailboxRole, null> | null;
  /** The sort this view asks the server for (#154). Views that do not say get
   * `returnedAt` — effective arrival, newest first. The Priority view asks for
   * `priority`, which groups the important bucket first server-side, because a
   * client can only regroup the page it already holds. */
  sort?: Comparator[];
  /** True when the plan depends on the search index (a text/operator filter). */
  needsSearch: boolean;
  /** What this view asked of the snooze (#51): false for the mailbox views that
   * exclude snoozed mail, true for the Snoozed view, null for the views that do
   * not filter on it. The optimistic overlay reads it so a just-snoozed message
   * leaves the list immediately — the local half of the server-side filter. */
  snoozed: boolean | null;
}

/**
 * Whether a view over this mailbox role hides snoozed mail (#51).
 *
 * ONLY the inbox does. Snoozing means "not in my way while I triage", and the
 * inbox is what triage happens in — that is the whole scope of the promise.
 * Applying it to every folder would be worse than useless: snooze a message and
 * then delete it and it would vanish from TRASH, which is the one place a user
 * looks for something they deleted. Same for a snoozed message in Sent or in a
 * folder they filed it into. Those folders answer "what is in here", and the
 * answer must not depend on a timer.
 *
 * A snoozed message that is archived or filed therefore still shows in that
 * folder, carrying its "returns …" cue. That is honest: it IS there, and it is
 * also coming back.
 */
function hidesSnoozed(role: MailboxRole | null): boolean {
  return role === "inbox";
}

/** Quotes a value for the search operator syntax if it contains whitespace
 * (internal/search/parse.go tokenizer: `label:"Client Work"`). */
function quoteIfNeeded(v: string): string {
  return /\s/.test(v) ? `"${v}"` : v;
}

export function planFor(
  view: View,
  mailboxes: Mailbox[],
  labels: Record<Id, Label>,
): QueryPlan {
  /**
   * THE ACCOUNT SCOPE, AS A CONDITION ON EVERY FILTER OF THE PLAN (#375).
   *
   * `inAccount` is structured and flat, so it ANDs with whatever else the view
   * asks for — which is the whole reason the account switcher can narrow a
   * label, a tab or the Priority inbox and not only the lists that had no other
   * condition. It is applied to EVERY filter in the plan, including the
   * per-account fan-out of a role view: Email/query has no way to filter by
   * the MAILBOX's accountId directly (#488 added that field to Mailbox, not to
   * the Email filter grammar), so the intersection with `inAccount` is still
   * what tells the N inbox queries apart — the other N−1 return nothing rather
   * than the wrong account's mail.
   */
  const scope = accountOf(view);
  const inAccount: EmailFilter = scope ? { inAccount: scope } : {};
  const scoped = (filters: EmailFilter[]): EmailFilter[] =>
    scope ? filters.map((f) => ({ ...f, ...inAccount })) : filters;

  switch (view.kind) {
    case "role": {
      const inRole = mailboxes.filter((m) => m.role === view.role);
      const hide = hidesSnoozed(view.role);
      return {
        // One query per mailbox with this role — one per account. The client
        // merges them; the server has no OR.
        filters: scoped(
          inRole.map((m) => ({ inMailbox: m.id, ...(hide ? { isSnoozed: false } : {}) })),
        ),
        role: view.role,
        needsSearch: false,
        snoozed: hide ? false : null,
      };
    }
    case "mailbox": {
      const mb = mailboxes.find((m) => m.id === view.mailboxId);
      const hide = hidesSnoozed(mb?.role ?? null);
      return {
        filters: [{ inMailbox: view.mailboxId, ...(hide ? { isSnoozed: false } : {}) }],
        role: (mb?.role ?? null) as Exclude<MailboxRole, null> | null,
        needsSearch: false,
        snoozed: hide ? false : null,
      };
    }
    case "starred":
      return {
        filters: scoped([{ hasKeyword: KEYWORD_FLAGGED }]),
        role: null,
        needsSearch: false,
        snoozed: null,
      };
    case "priority": {
      // THE PRIORITY INBOX (spec §6, #154). It is the inbox — same mail, same
      // snoozed-mail exclusion — re-ordered so the important bucket leads.
      //
      // Deliberately NOT `isImportant: true`: hiding the rest would make this a
      // filter, and a filter you have to leave to see the remainder is a worse
      // inbox, not a better one. Spec §6 says buckets, and the divider the list
      // draws is what separates them.
      const inInbox = mailboxes.filter((m) => m.role === "inbox");
      return {
        filters: scoped(inInbox.map((m) => ({ inMailbox: m.id, isSnoozed: false }))),
        role: "inbox",
        needsSearch: false,
        snoozed: false,
        sort: [{ property: "priority", isAscending: false }],
      };
    }
    case "tab": {
      // ONE CATEGORY TAB (#228), scoped to the inbox — one query per account,
      // exactly like a role view, because a tab narrows the inbox rather than
      // replacing it. Showing archived or sent mail under "Promotions" would
      // make the strip a third kind of container that is neither folder nor
      // label.
      //
      // `inTab` takes the tab the user CLICKED. The server applies the
      // enabled-fold, so Primary also collects mail from any tab switched off
      // and this client never has to know which those are. It also refuses a
      // disabled tab outright, which is why the strip must render only enabled
      // ones — asking for a hidden tab is a bug here, not an empty list.
      //
      // Snoozed mail is excluded, like the inbox view it narrows: a tab is where
      // triage happens, and a snooze means "not in my way while I triage".
      const inInbox = mailboxes.filter((m) => m.role === "inbox");
      return {
        filters: scoped(
          inInbox.map((m) => ({ inMailbox: m.id, inTab: view.tab, isSnoozed: false })),
        ),
        role: "inbox",
        needsSearch: false,
        snoozed: false,
      };
    }
    case "needsCategory": {
      // THE NEEDS-A-CATEGORY QUEUE (spec §8, #176).
      //
      // SCOPED TO THE INBOX, one query per account like a role view. The server
      // already refuses to queue drafts and junk, but `needsCategory` on its own
      // is still "every untrashed message nothing has categorised" — which
      // includes Sent (your own mail, unlabelled by definition) and Archive (mail
      // already dealt with). Neither belongs in a list that offers to batch-label
      // whatever is in it, and the 546-message figure this design was argued from
      // was an INBOX count.
      //
      // It also buys a second escape hatch for free: archiving removes a message
      // from the queue, because "I have dealt with this" is a perfectly good
      // answer to "what category is it?".
      //
      // Snoozed mail is NOT excluded, unlike the inbox view. A snooze says "not
      // in my way while I triage my mail"; this is not mail to triage, it is a
      // list of questions. Hiding a question until its message woke would make
      // the queue under-report what is waiting, and the empty state's whole
      // promise is that empty means nothing is.
      //
      // The sort MUST come from the server: confidence decides which messages
      // are ON the page, and a client can only reorder the page it was given.
      // core/store.ts visibleIds reproduces the same order locally so the
      // per-account merge and the optimistic overlay do not scramble it.
      const inInbox = mailboxes.filter((m) => m.role === "inbox");
      return {
        // SCOPED TOO, though #375's spec text lists only the other seven kinds.
        // `#/inbox/sort` is a list route, the account switcher puts `acct` on
        // it, and the rail prints this queue's count from the SCOPED summary —
        // so a queue that ignored the scope would sit under a badge counting
        // something else. That disagreement is the one thing the desk's numbers
        // are not allowed to have.
        filters: scoped(inInbox.map((m) => ({ inMailbox: m.id, needsCategory: true }))),
        role: "inbox",
        needsSearch: false,
        snoozed: null,
        sort: [{ property: "suggestionConfidence", isAscending: false }],
      };
    }
    case "snoozed":
      // The Snoozed view (spec §6). Nothing to merge across accounts: snooze is
      // a Mailroom-side property of the message, not a folder, so ONE query
      // answers it — unlike a role view, which is a union over per-account
      // mailboxes.
      return {
        filters: scoped([{ isSnoozed: true }]),
        role: null,
        needsSearch: false,
        snoozed: true,
      };
    case "unread":
      // Structured (`notKeyword: $seen`), NOT the search operator `is:unread`.
      // Both are correct, but the structured one needs no index — and the index
      // is the one part of this server that can legitimately be absent (spec §7:
      // "the index is a disposable cache"). `store.QueryEmails`
      // (internal/store/jmap_email.go) DOES apply a trash predicate —
      // `m.trashed_at IS NULL` — unless the query is scoped to the Trash
      // mailbox, so an unread message sitting in Trash is correctly excluded
      // here, same as the search operator would exclude it.
      return {
        filters: scoped([{ notKeyword: KEYWORD_SEEN }]),
        role: null,
        needsSearch: false,
        snoozed: null,
      };
    case "label": {
      const label = labels[view.labelId];
      if (!label) return { filters: [], role: null, needsSearch: true, snoozed: null };
      return {
        // The text half and the structured scope in ONE flat filter: the server
        // runs the operator against the index to get the candidate set and ANDs
        // `inAccount` in SQL (internal/api/email.go emailQueryWith), so this is
        // still one query and still means what it says.
        filters: scoped([{ query: `label:${quoteIfNeeded(label.name)}` }]),
        role: null,
        needsSearch: true,
        snoozed: null,
      };
    }
    case "all": {
      // ALL MAIL (#375) — ONE query, whatever the narrowings, because every one
      // of them is a condition in the same flat filter rather than a union.
      //
      // `role: null` and `snoozed: null`: this list is not a mailbox and does
      // not filter the snooze, so nothing here has a local half to apply. It
      // does inherit the client-wide overlay behaviour that a message archived
      // FROM a role-less list leaves it until the next load (store.ts
      // visibleIds) — the same as Starred, Unread and a label view. Making all
      // mail the exception would be a second overlay rule to learn; the row is
      // back the moment the list reloads.
      //
      // NO `sort`. That means the plan takes the client's default,
      // `returnedAt` desc (sync.ts DEFAULT_SORT) — effective arrival — rather
      // than raw `receivedAt`. Deliberate: `store.ts` re-sorts every list it
      // renders by effective arrival, so asking the server for a different
      // order would put the page boundary in one place and the rows in
      // another, which is how a "show more" starts skipping mail.
      const label = view.labelId === undefined ? undefined : labels[view.labelId];
      if (view.labelId !== undefined && !label) {
        // A label id we have never heard of — a stale link, or reference data
        // that has not landed. NO FILTER AT ALL would be worse than useless: it
        // would list the whole account under a heading naming one label.
        return { filters: [], role: null, needsSearch: false, snoozed: null };
      }
      // An area filters by `inArea` — the area's own CLAIM, which includes mail
      // any of its feeds resolved into it — and not by its own label, which is
      // only one of those feeds.
      const byArea: EmailFilter = view.areaId ? { inArea: view.areaId } : {};
      // A label filters STRUCTURALLY on its keyword atom: `keywordPredicate`
      // resolves the atom through `label_keyword_map` and matches
      // `message_labels`, so this needs no search index (see the header). A
      // LOCAL-ONLY label has no atom to match, so it falls back to the operator
      // the label view uses — the only case here that touches the index.
      const local = label !== undefined && !label.keyword;
      const byLabel: EmailFilter = !label
        ? {}
        : label.keyword
          ? { hasKeyword: label.keyword }
          : { query: `label:${quoteIfNeeded(label.name)}` };
      return {
        filters: scoped([{ ...byArea, ...byLabel }]),
        role: null,
        needsSearch: local,
        snoozed: null,
      };
    }
    case "account":
      // The account id is a ULID; internal/search/service.go resolveTerm takes a
      // ULID as-is (no name lookup needed).
      return {
        filters: [{ query: `account:${view.accountId}` }],
        role: null,
        needsSearch: true,
        snoozed: null,
      };
    case "outbox":
      // NO FILTERS AT ALL, and this is the honest answer rather than a gap.
      // The Outbox lists EmailSubmission objects, which no Email/query can
      // express (see the union member's own note). An empty plan is what the
      // sync layer already understands as "there is nothing to ask the server
      // for" — sync.ts loadView returns an empty list for it without a request —
      // so navigating here costs no query, and the route renders from
      // `state.submissions`.
      return { filters: [], role: null, needsSearch: false, snoozed: null };
    case "search":
      return {
        filters: [{ query: view.query, ...(view.prefix ? { queryPrefix: true } : {}) }],
        role: null,
        needsSearch: true,
        snoozed: null,
        ...(view.sort === "relevance" ? { sort: [{ property: "relevance" as const }] } : {}),
      };
  }
}

/** The search filter to hand SearchSnippet/get for this view (RFC 8621 §5.1:
 * the SAME filter the query used), or null when the view has no text part. */
export function snippetFilter(plan: QueryPlan): Record<string, unknown> | null {
  const withQuery = plan.filters.find((f) => f.query);
  if (!withQuery?.query) return null;
  // queryPrefix too: the highlights must be of what the query actually matched.
  return { query: withQuery.query, ...(withQuery.queryPrefix ? { queryPrefix: true } : {}) };
}
