// SPDX-License-Identifier: AGPL-3.0-only

// ROUTES (#373): every surface this client can be on, as a value.
//
// WHY THIS EXISTS. Navigation was a hand-rolled `setView({kind})` machine;
// "settings" was a menu plus four modal sheets; nothing had a URL except the
// list views. So there was no back, no deep link, no refresh-lands-where-you-
// were — and a sheet was the cheapest thing to add, so sheets got added. The
// structure was choosing the design. The desk (#375) needs this inventory to
// exist first.
//
// HASH ROUTES, NOT PATHS. The Go server serves the SPA with a plain
// http.FileServer (cmd/mailroom webHandler), so `/settings/areas` would 404 on
// reload. `#/…` keeps deep links, refresh and the PWA working with no server
// change, and it is what core/views.ts already does.
//
// DOM-FREE, like the rest of core/ (spec §12): this module knows about strings
// and shapes, never about `location`. app/routing.ts is the half that touches
// the browser.
//
// THIS LAYER SITS ON TOP OF views.ts, IT DOES NOT REPLACE IT. `View` is still
// the query-planning vocabulary (`planFor` turns one into JMAP filters);
// `viewForRoute` is the one table that maps a route onto it. Every hash
// `viewFromHash` accepts still parses — as `{page:"view"}` — and still produces
// the same View, which is what routes.test.ts asserts differentially rather
// than by re-listing the vocabulary here.

import type { Id } from "./jmap/types";
import type { View } from "./views";
import { accountOf, viewFromHash, viewToHash, withAccount } from "./views";
import { isContactsFilter } from "./contacts";
import type { ContactsFilter, ContactsSort } from "./contacts";

/**
 * The settings sections: THE TWELVE ROWS OF THE SETTINGS PAGE, plus
 * `shortcuts` (#374 increment 2).
 *
 * Settings is one searchable page now, not an index of sheets, so a "section"
 * is a ROW on that page and `#/settings/<row>` deep-links to it expanded. The
 * order below is the order the rows are rendered in, which is also the order
 * the design argues for — the accounts your mail arrives through first, the
 * shape of the desk next, then the things that act on mail, then the things
 * that only change how it looks, then the door marked out.
 *
 * `shortcuts` IS NOT A ROW. It is the keyboard sheet the `?` key and the settings
 * page's own "Keyboard shortcuts" button open, and it is a sheet rather than a
 * row because it holds no settings at all — nothing on it can be changed,
 * searched for by value, or scoped. (It is spelled out rather than shortened to
 * "Keys" because the KEYS row on the same page is about OpenPGP.) Keeping it in
 * this union is what lets `#/settings/shortcuts` go on meaning what it always
 * meant.
 */
export type SettingsSection =
  | "accounts"
  | "areas"
  | "line"
  | "tray"
  | "rules"
  | "identities"
  | "notifications"
  | "senders"
  | "out-of-office"
  | "cleanup"
  | "reading"
  | "keys"
  | "api-keys"
  | "devices"
  | "saved-searches"
  | "appearance"
  | "data"
  | "shortcuts";

/** A section that IS a row — everything except the shortcuts sheet.
 *
 * Named so the page can hold a `Record<SettingsRowId, …>` of its rows, which
 * makes "every row has an entry" a TYPE error rather than a missing row nobody
 * notices. A row absent from the page would otherwise have a URL that expands
 * nothing, and a row absent from the list below would have no URL at all —
 * neither fails loudly on its own. */
export type SettingsRowId = Exclude<SettingsSection, "shortcuts">;

/** The twelve rows, in page order — which is the order the page renders them in,
 * because the page maps over THIS list rather than keeping a second one. */
export const SETTINGS_ROWS: readonly SettingsRowId[] = [
  "accounts",
  "areas",
  "line",
  "tray",
  "rules",
  "identities",
  "notifications",
  "senders",
  "out-of-office",
  "cleanup",
  "reading",
  "keys",
  "api-keys",
  "devices",
  "saved-searches",
  "appearance",
  "data",
];

const SETTINGS_SECTIONS: readonly SettingsSection[] = [...SETTINGS_ROWS, "shortcuts"];

/**
 * The section ids #373 shipped, and the row each one is now part of.
 *
 * THESE ARE READ BUT NEVER WRITTEN. `parseRoute` rewrites them to the row that
 * absorbed them, so a bookmark, a pasted link or a browser history entry from
 * v1.65 still lands on the controls it named — and `formatRoute` only ever
 * emits the new id, so the old ones fade out of history rather than being
 * carried forward forever.
 *
 * `category-tabs` is the one that moved furthest: the strip's enable/order
 * controls are the `more` of Areas and drawers, so the old URL opens the Areas
 * row rather than a sheet of its own.
 */
const LEGACY_SECTIONS: Readonly<Record<string, SettingsSection>> = {
  "remote-content": "reading",
  snippets: "identities",
  "category-tabs": "areas",
};

function isSettingsSection(s: string): s is SettingsSection {
  return (SETTINGS_SECTIONS as readonly string[]).includes(s);
}

/** The section a path segment names — its own id, or the row a retired id was
 * folded into. Null for anything else, which is the settings page itself. */
function settingsSectionOf(raw: string | undefined): SettingsSection | null {
  if (raw === undefined) return null;
  if (isSettingsSection(raw)) return raw;
  return LEGACY_SECTIONS[raw] ?? null;
}

/** The two halves of the inbox the desk splits it into (#375): what wants a
 * decision, and what does not. Neither is a category tab — see views.ts. */
export type InboxTab = "review" | "rest";

/**
 * A composer that can be named by a URL.
 *
 * ONLY THE INTENTS AN ID CAN CARRY. `ComposeIntent` in ui/Compose.tsx is
 * `{kind:"new"}` or `{kind:"reply", parent: EffectiveEmail}` — the second is
 * reconstructible from the parent's id, so it is a route. An intent that
 * carried MORE than an id (a draft body, a template already applied, a
 * half-typed reply) is NOT a route and stays exactly where it is today: local
 * component state. A URL that claimed to hold a draft would either have to
 * carry the draft — putting the user's prose in their history — or lie about
 * restoring it.
 *
 * `forward` WAS declared-but-dead — the route parsed and rendered nothing,
 * because `ComposeIntent` had no forward kind. #448 gives it one, so
 * `?compose=forward:<id>` now opens a real composer (subject `Fwd: …`, the parent
 * quoted, its attachments carried by reference). The URL shape did not have to
 * change, which is exactly why it was declared ahead of the feature.
 *
 * `draft` is #448's addition and it is the one ref that names something the user
 * has already written. That does NOT contradict the paragraph above: the URL
 * carries the DRAFT'S SERVER ID, not its text — the draft is an object the server
 * holds, so resuming it is a fetch like opening a message, and nothing of the
 * user's prose goes into their browser history. A composer whose text existed
 * only locally still has no route, and still must not have one.
 *
 * `replyAll` is #646's addition, spelled `replyall:<id>` in the query string
 * (URLs are case-normalised by some clients, so the param stays lowercase
 * while the TS key stays camelCase). It reconstructs the same way `reply`
 * does — from the parent's id — and differs only in which recipients
 * `ui/compose/recipients.ts` computes from it.
 */
export type ComposeRef =
  | "new"
  | { reply: Id }
  | { replyAll: Id }
  | { forward: Id }
  | { draft: Id };

/**
 * Which of day-one setup's four steps is on screen (#446).
 *
 * A NUMBER RATHER THAN FOUR NAMED PAGES, because the steps are ordered and the
 * order IS the content: the surface says "Step 2 of 4" about itself, Back and
 * Next are ±1, and Escape peels one step. Named pages would move that arithmetic
 * into a lookup table for nothing.
 *
 * ⚠ IMPORT MODE IS ALWAYS STEP 2. `#/setup/import/<mailboxId>` is one folder put
 * through step 2 and nothing else — there is no census to read and no line to
 * set for a single folder someone chose in Settings — so `importMailboxId`
 * pins the step, and `formatRoute` emits the import path whatever `step` says.
 */
export type SetupStep = 1 | 2 | 3 | 4;

/** The step a `#/setup/<n>` segment names, or null for anything else. Only 2..4
 * are spellable: step 1 IS `#/setup`, so `#/setup/1` is not a second URL for it
 * but an unrecognised tail — which lands on step 1 anyway, like every other
 * unrecognised tail in this table. */
function setupStepOf(raw: string | undefined): SetupStep | null {
  switch (raw) {
    case "2":
      return 2;
    case "3":
      return 3;
    case "4":
      return 4;
    default:
      return null;
  }
}

/** The query parameters every route shares. `m` is the open message, `acct` the
 * account scope, `compose` the composer on top. */
interface Shared {
  m?: Id;
  acct?: Id;
  compose?: ComposeRef;
}

export type Route =
  // #/inbox, #/inbox/review, #/inbox/rest, #/inbox/sort
  | ({ page: "inbox"; tab: InboxTab | null; sort?: boolean } & Shared)
  // #/tray
  | ({ page: "tray" } & Shared)
  // #/all?area=&label=&archived=
  | ({ page: "all"; area?: Id; label?: Id; archived?: boolean } & Shared)
  // Every hash views.ts already accepts: #/starred, #/priority, #/snoozed,
  // #/unread, #/label/<id>, #/tab/<t>, #/search/<q>, #/mailbox/<id>,
  // #/account/<id>, #/archive|sent|drafts|trash|junk|needs-category.
  | ({ page: "view"; view: View } & Shared)
  // #/settings, #/settings/<row>, #/settings/accounts/<id>, #/settings/areas/<id>
  | ({ page: "settings"; section: SettingsSection | null; accountId?: Id; areaId?: Id; apiKeyId?: Id } & Shared)
  // #/setup, #/setup/2..4, #/setup/import/<mailboxId>
  | ({ page: "setup"; step: SetupStep; importMailboxId?: Id } & Shared)
  // THE CONTACTS PAGE (#816): `#/contacts` is the list, `#/contacts/<id>` one
  // contact. The list's filter, sort and search text are IN THE URL, on the
  // page's route as well as the list's, so opening a contact and going back
  // returns to exactly the list it was opened from (and a pasted link means
  // what it says). Absent = the default: saved contacts, by name, no text.
  | ({ page: "contacts"; id?: Id; filter?: ContactsFilter; sort?: ContactsSort; q?: string } & Shared);

export type ContactsRoute = Extract<Route, { page: "contacts" }>;

/** The list a contacts route is (or was opened from): the same filter, sort and
 * text, no contact. */
export function contactsListOf(r: ContactsRoute): ContactsRoute {
  const { id: _drop, ...rest } = r;
  return rest;
}

/** Where an unknown hash lands, and what the app boots on. */
export const INBOX_ROUTE: Route = { page: "inbox", tab: null };

/**
 * What this module will carry as an id.
 *
 * DELIBERATELY A SHAPE CHECK, NOT A ULID CHECK. Ids are ULIDs app-side
 * (ADR-007), but a route id is an opaque server-supplied string, and dropping a
 * legitimate one would break navigation for mail we can see — whereas carrying
 * an id the store has never heard of just fails closed at the lookup (the pane
 * renders nothing, exactly as it does for a deleted message). So this rejects
 * the things that would corrupt a URL — whitespace, `/`, `?`, `&`, `=`, `%`,
 * `:` — and nothing else. `:` in particular MUST be excluded, because
 * `compose=reply:<id>` splits on it.
 */
const ID_RE = /^[A-Za-z0-9_.-]{1,64}$/;

function asId(v: string | undefined): Id | undefined {
  return v !== undefined && ID_RE.test(v) ? v : undefined;
}

/** decodeURIComponent that cannot throw on a hand-mangled `%` sequence. */
function decode(v: string): string {
  try {
    return decodeURIComponent(v);
  } catch {
    return v;
  }
}

// -- compose serialisation ---------------------------------------------------

/** `new` | `reply:<id>` | `replyall:<id>` | `forward:<id>` | `draft:<id>`. */
export function composeParam(ref: ComposeRef): string {
  if (ref === "new") return "new";
  if ("reply" in ref) return `reply:${encodeURIComponent(ref.reply)}`;
  if ("replyAll" in ref) return `replyall:${encodeURIComponent(ref.replyAll)}`;
  if ("forward" in ref) return `forward:${encodeURIComponent(ref.forward)}`;
  return `draft:${encodeURIComponent(ref.draft)}`;
}

export function parseComposeParam(raw: string | undefined): ComposeRef | undefined {
  if (raw === undefined) return undefined;
  if (raw === "new") return "new";
  const i = raw.indexOf(":");
  if (i === -1) return undefined;
  const kind = raw.slice(0, i);
  const id = asId(decode(raw.slice(i + 1)));
  if (!id) return undefined;
  if (kind === "reply") return { reply: id };
  if (kind === "replyall") return { replyAll: id };
  if (kind === "forward") return { forward: id };
  if (kind === "draft") return { draft: id };
  return undefined;
}

// -- parse -------------------------------------------------------------------

function parseQuery(raw: string): Record<string, string> {
  const out: Record<string, string> = {};
  if (!raw) return out;
  for (const pair of raw.split("&")) {
    if (!pair) continue;
    const eq = pair.indexOf("=");
    const key = decode(eq === -1 ? pair : pair.slice(0, eq));
    const value = eq === -1 ? "" : decode(pair.slice(eq + 1));
    if (key && !(key in out)) out[key] = value;
  }
  return out;
}

/** `1`/`true` are true; everything else, including the absent case, is false —
 * a flag in a URL is either asserted or it is not there. */
function parseFlag(v: string | undefined): boolean {
  return v === "1" || v === "true";
}

function sharedFrom(q: Record<string, string>): Shared {
  const m = asId(q["m"]);
  const acct = asId(q["acct"]);
  const compose = parseComposeParam(q["compose"]);
  return {
    ...(m ? { m } : {}),
    ...(acct ? { acct } : {}),
    ...(compose ? { compose } : {}),
  };
}

/**
 * The legacy view a path names.
 *
 * ⚠ THE TRY/CATCH IS LOAD-BEARING, and it is guarding a bug older than this
 * module: `viewFromHash` decodes `#/search/<q>` with a bare
 * `decodeURIComponent`, which THROWS on a malformed escape (`#/search/%E0%A4%A`
 * — a truncated sequence, which is what a URL loses when it is copied out of a
 * chat client that ate the last character). Before routes existed that threw
 * inside a useState initialiser and the app rendered nothing at all. This is
 * now the only caller, so catching it here closes it everywhere; the fallback
 * is the same inbox an unrecognised hash gets, because a mangled link is a
 * link that no longer says where it was going.
 */
function viewForPath(path: string): View {
  try {
    return viewFromHash(`#${path}`);
  } catch {
    return { kind: "role", role: "inbox" };
  }
}

/**
 * TOTAL, by construction: an unknown hash is the inbox, a malformed id is
 * dropped, nothing throws. A route parser that can throw turns a pasted URL
 * into a white screen.
 */
export function parseRoute(hash: string): Route {
  const raw = hash.startsWith("#") ? hash.slice(1) : hash;
  const qi = raw.indexOf("?");
  const path = qi === -1 ? raw : raw.slice(0, qi);
  const query = parseQuery(qi === -1 ? "" : raw.slice(qi + 1));
  const shared = sharedFrom(query);
  const segs = path.split("/").filter((s) => s.length > 0);
  const head = segs[0] ?? "";

  switch (head) {
    case "":
      return { page: "inbox", tab: null, ...shared };

    case "inbox": {
      // `#/inbox/<anything else>` is the plain inbox, which is also what
      // viewFromHash does with a tail it does not recognise.
      const sub = segs[1];
      if (sub === "sort") return { page: "inbox", tab: null, sort: true, ...shared };
      if (sub === "review" || sub === "rest") return { page: "inbox", tab: sub, ...shared };
      return { page: "inbox", tab: null, ...shared };
    }

    case "tray":
      return { page: "tray", ...shared };

    case "all": {
      const area = asId(query["area"]);
      const label = asId(query["label"]);
      const archived = parseFlag(query["archived"]);
      return {
        page: "all",
        ...(area ? { area } : {}),
        ...(label ? { label } : {}),
        ...(archived ? { archived } : {}),
        ...shared,
      };
    }

    case "settings": {
      const raw1 = segs[1] === undefined ? undefined : decode(segs[1]);
      const section = settingsSectionOf(raw1);
      const detail = segs[2] === undefined ? undefined : asId(decode(segs[2]));
      return {
        page: "settings",
        section,
        ...(section === "accounts" && detail ? { accountId: detail } : {}),
        ...(section === "areas" && detail ? { areaId: detail } : {}),
        ...(section === "api-keys" && detail ? { apiKeyId: detail } : {}),
        ...shared,
      };
    }

    case "setup": {
      // DAY-ONE SETUP (#446). `#/setup/import/<id>` first, because "import" is
      // not a step number and a folder id is not a step either: the two tails are
      // different shapes rather than one with an optional extra.
      if (segs[1] === "import") {
        const id = segs[2] === undefined ? undefined : asId(decode(segs[2]));
        // A mangled folder id lands on the WIZARD, not on an import of nothing:
        // step 2 with no folder would offer to act on the whole mailbox, which is
        // the opposite of what the link asked for.
        if (id) return { page: "setup", step: 2, importMailboxId: id, ...shared };
        return { page: "setup", step: 1, ...shared };
      }
      return { page: "setup", step: setupStepOf(segs[1]) ?? 1, ...shared };
    }

    case "contacts": {
      const id = segs[1] === undefined ? undefined : asId(decode(segs[1]));
      const filter = isContactsFilter(query["filter"]) && query["filter"] !== "saved" ? query["filter"] : undefined;
      const sort = query["sort"] === "last" ? ("last" as const) : undefined;
      // Bounded: it is typed into a search box, and a URL is not where a
      // megabyte of text belongs.
      const q = (query["q"] ?? "").slice(0, 200);
      const { m: _m, ...rest } = shared;
      return {
        page: "contacts",
        ...(id ? { id } : {}),
        ...(filter ? { filter } : {}),
        ...(sort ? { sort } : {}),
        ...(q.trim() !== "" ? { q } : {}),
        ...rest,
      };
    }

    case "m": {
      // The legacy per-message hash. It parses to "the inbox with that message
      // open" so parseRoute stays total; legacyRedirect() is what turns it into
      // the list the user was actually on.
      const id = segs[1] === undefined ? undefined : asId(decode(segs[1]));
      return { page: "inbox", tab: null, ...shared, ...(id ? { m: id } : {}) };
    }

    default: {
      // EVERY legacy hash, unchanged. The path is handed to viewFromHash whole
      // (not the split segments) so its own decoding of `#/search/<q>` is the
      // one that runs.
      const view = viewForPath(path);
      // A search's two modifiers ride as parameters (#724): `?prefix=1` is the
      // as-you-type list, `?sort=relevance` the best-match order. Anything else
      // in either is ignored, so a hand-edited link still lands on the search.
      if (view.kind === "search") {
        const prefix = parseFlag(query["prefix"]);
        const relevance = query["sort"] === "relevance";
        return {
          page: "view",
          view: { ...view, ...(prefix ? { prefix: true as const } : {}), ...(relevance ? { sort: "relevance" as const } : {}) },
          ...shared,
        };
      }
      return { page: "view", view, ...shared };
    }
  }
}

// -- format ------------------------------------------------------------------

/**
 * The canonical string for a route. Query parameters are emitted in ONE fixed
 * order — area, label, archived, m, acct, compose — so two equal routes have
 * one string and that string can be used as a key.
 *
 * `parseRoute(formatRoute(r))` deep-equals `r` for every route parseRoute can
 * produce. THE ONE EXCEPTION IS DELIBERATE: `{page:"view", view:{kind:"role",
 * role:"inbox"}}` formats to `#/inbox`, which is the inbox PAGE and parses back
 * as one. They are the same URL and `viewForRoute` gives them the same View, so
 * the canonical form of that hash is the page — see the round-trip test.
 */
export function formatRoute(r: Route): string {
  return render(r, true);
}

/**
 * The route's identity WITHOUT the message it has open or the composer on top —
 * what list-position memory is keyed by (#373). Opening a message must not look
 * like arriving at a different list.
 */
export function routeKey(r: Route): string {
  return render(r, false);
}

function render(r: Route, transient: boolean): string {
  const q: string[] = [];
  let path: string;
  switch (r.page) {
    case "inbox":
      path = `#/inbox${r.sort ? "/sort" : r.tab ? `/${r.tab}` : ""}`;
      break;
    case "tray":
      path = "#/tray";
      break;
    case "all":
      path = "#/all";
      if (r.area) q.push(`area=${encodeURIComponent(r.area)}`);
      if (r.label) q.push(`label=${encodeURIComponent(r.label)}`);
      if (r.archived) q.push("archived=1");
      break;
    case "view":
      path = viewToHash(r.view);
      // Part of the search's identity, so in the key as well as the URL (#724).
      if (r.view.kind === "search" && r.view.prefix) q.push("prefix=1");
      if (r.view.kind === "search" && r.view.sort === "relevance") q.push("sort=relevance");
      break;
    case "settings":
      path = "#/settings";
      if (r.section) path += `/${r.section}`;
      if (r.section === "accounts" && r.accountId) path += `/${encodeURIComponent(r.accountId)}`;
      if (r.section === "areas" && r.areaId) path += `/${encodeURIComponent(r.areaId)}`;
      if (r.section === "api-keys" && r.apiKeyId) path += `/${encodeURIComponent(r.apiKeyId)}`;
      break;
    case "setup":
      // The import path wins over the step, because import mode IS step 2 and
      // carrying both would be two statements of one fact that could disagree.
      path = r.importMailboxId
        ? `#/setup/import/${encodeURIComponent(r.importMailboxId)}`
        : `#/setup${r.step === 1 ? "" : `/${r.step}`}`;
      break;
    case "contacts":
      path = "#/contacts";
      if (r.id) path += `/${encodeURIComponent(r.id)}`;
      if (r.filter && r.filter !== "saved") q.push(`filter=${r.filter}`);
      if (r.sort === "last") q.push("sort=last");
      if (r.q && r.q.trim() !== "") q.push(`q=${encodeURIComponent(r.q)}`);
      break;
  }
  if (transient && r.m) q.push(`m=${encodeURIComponent(r.m)}`);
  if (r.acct) q.push(`acct=${encodeURIComponent(r.acct)}`);
  if (transient && r.compose) q.push(`compose=${composeParam(r.compose)}`);
  return q.length > 0 ? `${path}?${q.join("&")}` : path;
}

// -- the view a route is looking at ------------------------------------------

/** What `viewForRoute` needs that it cannot know by itself, supplied by the
 * caller so this stays DOM-free and store-free.
 *
 * `areaLabel` USED TO BE HERE and is gone (#375). `#/all?area=<id>` was shown
 * through the area's own LABEL because there was no view that could ask for an
 * area; now there is, and `inArea` is a strictly better question — it includes
 * the mail every OTHER feed resolved into the area, which the own label does
 * not carry. Nothing else consulted the hook, so it went with the fallback it
 * existed to justify. */
export interface RouteContext {
  /** The list the user was on before opening settings — settings does not
   * change the list beneath it. */
  previousListRoute?: Route;
}

/**
 * True for the routes that ARE a list — everything except settings and the
 * day-one wizard.
 *
 * ⚠ THE WIZARD IS NOT A LIST, and getting that wrong would be quiet rather than
 * visible. `#/setup` renders no message list at all, so if it counted as one it
 * would become the remembered "last list" (app/routing.ts): Escape out of
 * Settings would land back in the wizard, the per-device scope memory would be
 * written from a route that has no scope, and the reading pane's list-position
 * memory would key an entry for a surface with no rows. It sits OVER the list the
 * user was on, exactly as settings does, which is also why `viewForRoute` keeps
 * that list's view underneath rather than issuing a query of its own.
 */
export function isListRoute(r: Route): boolean {
  // Contacts (#816) is not a MESSAGE list: the mail list underneath stays the
  // one the user was on, so leaving Contacts returns to it unchanged.
  return r.page !== "settings" && r.page !== "setup" && r.page !== "contacts";
}

/** The list a route is showing: itself, or — for settings, which overlays the
 * list rather than replacing it — the one the caller says came before. */
export function listRouteOf(r: Route, previous?: Route): Route {
  if (isListRoute(r)) return r;
  return previous && isListRoute(previous) ? previous : INBOX_ROUTE;
}

/**
 * THE TABLE. One row per route form, each with the reason it maps where it does.
 *
 * ⚠ `acct` NARROWS EVERY LIST, and used not to. The old note here said it could
 * be honoured only on a "plain" list, because a per-account view WAS the search
 * `account:<id>` and the server has no filter operators to intersect it with a
 * role, label or tab. That constraint is gone: `inAccount` is a structured
 * condition on the same flat filter (views.ts `Scoped`/`planFor`), so the scope
 * ANDs with whatever else the route asks for and the account switcher means the
 * same thing on every screen. Two kinds still carry no scope on purpose —
 * a FOLDER already belongs to one account, and a SEARCH carries its own
 * `account:` operator — so on `#/mailbox/<id>` and `#/search/<q>` the parameter
 * rides along as scope memory without changing the question, exactly as it did
 * everywhere before.
 */
export function viewForRoute(r: Route, ctx: RouteContext = {}): View {
  const scoped = (v: View): View => (r.acct ? withAccount(v, r.acct) : v);

  switch (r.page) {
    case "inbox":
      // `sort` is the needs-a-category queue (#176) — the "what is this?" pile.
      // It wins over `tab` if both are somehow set; parseRoute never produces
      // both, and a queue that silently rendered as a mail list would be the
      // more confusing of the two answers.
      if (r.sort) return scoped({ kind: "needsCategory" });
      // `review` is the priority inbox (#154): the same mail, important first.
      if (r.tab === "review") return scoped({ kind: "priority" });
      // `rest` and the bare inbox are the inbox itself. They differ on the desk
      // (#375), which splits the list; underneath they are one query.
      return scoped({ kind: "role", role: "inbox" });

    case "tray":
      // The in-tray rides the star flag (#444) — there is no separate server
      // state for it, so the tray IS the starred view.
      return scoped({ kind: "starred" });

    case "all":
      // ALL MAIL (#375) — one view, however many narrowings, because they are
      // conditions on one filter rather than different lists. `?archived=1`
      // still parses (it was `#/all`'s original meaning, when this route was
      // the archive) and resolves HERE, to the same view: archived mail is in
      // all mail, so the parameter no longer selects anything and is kept only
      // so an old link still lands somewhere sensible.
      return scoped({
        kind: "all",
        ...(r.area ? { areaId: r.area } : {}),
        ...(r.label ? { labelId: r.label } : {}),
      });

    case "view":
      return scoped(r.view);

    case "settings":
    case "setup":
    case "contacts":
      // Neither of these changes the list beneath it (today's sheets sit on top
      // of it, and the route keeps that): the view stays whatever the previous
      // list route was, so closing settings does not refetch anything.
      //
      // The wizard is here for the same reason and one more: it must not issue a
      // query. "Skip setup, open the desk" has to land on a desk that is already
      // loaded, and a wizard that owned a view of its own would have spent the
      // whole visit fetching a list nobody could see.
      return viewForRoute(listRouteOf(r, ctx.previousListRoute), ctx);
  }
}

// -- moving between routes ---------------------------------------------------

/**
 * The route for a `View` — the compatibility bridge for every component that
 * still speaks Views (state.tsx's setView shim, the rail, the tab strip).
 *
 * IT IS LOSSLESS, which takes two special cases. A view's account scope is
 * carried by the ROUTE (`?acct=`), never by `viewToHash`, so it is lifted out
 * here rather than silently dropped at format time; and an all-mail view is
 * turned back into the all-mail PAGE, whose `?area=`/`?label=` are the only
 * place those narrowings can be written down. Without either, navigating
 * through this bridge would quietly widen the list the user was looking at.
 */
export function viewRoute(view: View): Route {
  const acct = accountOf(view);
  const scope = acct ? { acct } : {};
  if (view.kind === "all") {
    return {
      page: "all",
      ...(view.areaId ? { area: view.areaId } : {}),
      ...(view.labelId ? { label: view.labelId } : {}),
      ...scope,
    };
  }
  return { page: "view", view: withAccount(view, null), ...scope };
}

/** The same route scoped to an account (`null` widens it to all accounts) —
 * what the switcher writes. A helper for the same reason `withM` is one: the
 * cast that drops an optional key from a discriminated union belongs in exactly
 * one place. */
export function withAcct(r: Route, acct: Id | null): Route {
  const { acct: _drop, ...rest } = r;
  return (acct ? { ...rest, acct } : rest) as Route;
}

/** The same route with a different open message (`null` closes it). Written as
 * a helper rather than an object spread at each call site because dropping a
 * key from a discriminated union needs the cast to be in one place. */
export function withM(r: Route, m: Id | null): Route {
  const { m: _drop, ...rest } = r;
  return (m ? { ...rest, m } : rest) as Route;
}

/** The same route with a different composer (`null` closes it). */
export function withCompose(r: Route, compose: ComposeRef | null): Route {
  const { compose: _drop, ...rest } = r;
  return (compose ? { ...rest, compose } : rest) as Route;
}

/**
 * WHAT ESCAPE BACKS OUT TO, or null when there is nothing to back out of.
 *
 * One layer per press, outermost first, so Escape peels rather than jumping:
 * the composer, then the open message, then a settings DETAIL (one account, one
 * area), then the settings section it is inside, then settings itself, then one
 * step of the day-one wizard, then the sort queue. `previous` is the list to
 * return to from settings or the wizard; with none supplied it is the inbox,
 * because Escape must always be able to leave both.
 *
 * ⚠ THE DETAIL IS ITS OWN LAYER (#374 increment 3). `#/settings/accounts/<id>` is
 * a screen inside the Accounts row, so Escape from inside one account lands on the
 * list of accounts — not two layers out on the bare settings page, which is what
 * it did when the segment was parsed but nothing rendered it.
 */
export function escapeTarget(r: Route, previous?: Route): Route | null {
  if (r.compose) return withCompose(r, null);
  if (r.m) return withM(r, null);
  if (r.page === "settings") {
    if (r.accountId || r.areaId || r.apiKeyId) {
      return {
        page: "settings",
        section: r.section,
        ...(r.acct ? { acct: r.acct } : {}),
      };
    }
    if (r.section) return { page: "settings", section: null, ...(r.acct ? { acct: r.acct } : {}) };
    return listRouteOf(r, previous);
  }
  if (r.page === "setup") {
    // ONE STEP PER PRESS, and out of step 1 to the desk — the wizard's Back
    // button and Escape are the same gesture, so they are the same decision.
    // Nothing is discarded on the way out: the wizard's state is persisted per
    // login (ui/setup/setupState.ts), so leaving and coming back resumes.
    if (r.importMailboxId) {
      // Import mode has no step to go back to. It was opened from Settings →
      // Areas and drawers, and that is where its own "Done" returns, so Escape
      // agrees with the button rather than dropping the user on a list.
      return { page: "settings", section: "areas", ...(r.acct ? { acct: r.acct } : {}) };
    }
    if (r.step > 1) {
      return {
        page: "setup",
        step: (r.step - 1) as SetupStep,
        ...(r.acct ? { acct: r.acct } : {}),
      };
    }
    return listRouteOf(r, previous);
  }
  if (r.page === "inbox" && r.sort) {
    return { page: "inbox", tab: null, ...(r.acct ? { acct: r.acct } : {}) };
  }
  // One contact peels back to the list it was opened from (#816); the list
  // itself is a place, like the inbox, and Escape there does nothing.
  if (r.page === "contacts" && r.id) return contactsListOf(r);
  return null;
}

/**
 * The one hash shape that has to be rewritten rather than rendered: `#/m/<id>`
 * named a message with no list around it. It becomes the list the user was last
 * on, with that message open — so a link someone pasted opens the message
 * without also throwing away where they were. Null when the hash is not that.
 */
export function legacyRedirect(hash: string, lastListRoute?: Route): Route | null {
  const raw = hash.startsWith("#") ? hash.slice(1) : hash;
  const qi = raw.indexOf("?");
  const path = qi === -1 ? raw : raw.slice(0, qi);
  const segs = path.split("/").filter((s) => s.length > 0);
  if (segs[0] !== "m") return null;
  const id = segs[1] === undefined ? undefined : asId(decode(segs[1]));
  if (!id) return listRouteOf(lastListRoute ?? INBOX_ROUTE);
  return withM(listRouteOf(lastListRoute ?? INBOX_ROUTE), id);
}
