// SPDX-License-Identifier: AGPL-3.0-only

// THE RAIL (#375): where you can go, in the order the desk thinks about mail.
//
//   ┌ All accounts ▾ ────────────┐   the scope everything below is measured in
//   │ ✎ Compose                  │
//   │ Inbox            42        │   everything unactioned: tray + review + drawers
//   │ In-tray          12        │
//   │ Outbox            1        │   only while something is queued
//   │ Drafts            3        │   only while there are drafts
//   │ Sent                       │
//   │ All mail      3 480        │
//   │ AREAS · review / drawer    │
//   │   Bills          4 / 19    │
//   │   Needs sorting     12     │
//   │ LABELS · SAVED SEARCHES    │
//   │ Settings                   │
//   │ ▸ Folders                  │
//
// WHAT LEFT, AND WHERE IT WENT. Priority, Starred, Snoozed, Unread, Needs a
// category and the per-account list are no longer rail entries. NONE OF THEM
// WAS REMOVED — every one is still a view with a URL, still reachable by `g p`,
// `g s`, `g b`, `g n`, the command palette and a pasted link. What went is the
// claim that they are places you navigate BETWEEN: the desk answers "what is
// waiting on me" and those are filters over the answer, so a rail that listed
// them alongside the inbox was offering nine doors into one room. Starred in
// particular is gone because the TRAY IS the flag (#444) — two rail entries for
// one keyword is the same list twice under different names.
//
// COUNTS ARE BLANK UNTIL THEY ARE KNOWN. The desk's numbers come from
// `Area/getSummary` and the tray's own query, both taken at the CURRENT SCOPE
// (core/desk.ts railCounts, and the summary carries the scope it was measured
// at). Before the first summary lands there are no badges at all, rather than
// zeroes — a `0` that means "not loaded" is a claim about the user's mail that
// nobody made (CLAUDE.md), and on the Inbox entry it is the most reassuring
// possible wrong answer.
//
// One server fact still shapes this (see core/views.ts): every account has
// its own INBOX/Sent/…, so a role view is the union of every mailbox with
// that role — the switcher's account scope narrows it via the `account:`
// search operator, not via Mailbox.accountId, because Email/query has no
// filter term for the Mailbox's own field. The Folders disclosure below is a
// DIFFERENT question — which account each raw IMAP folder belongs to for
// DISPLAY grouping — and that one Mailbox.accountId answers directly (#488).

import { Fragment, useMemo, useState } from "react";
import type { ReactNode } from "react";
import type { JSX } from "react";

import { areaCounts, railCounts, sectionKey } from "../../core/desk";
import { moveToArea } from "./moveToArea";
import { offerRuleSuggestion } from "./ruleSuggestion";
import { canDrop, decodeDrag, DRAG_MIME, inboxDropPlan } from "../../core/dragDrop";
import { isInJunk } from "../../core/junk";
import type { DropContext, DropTarget } from "../../core/dragDrop";
import { groupFoldersByAccount } from "../../core/folderGroups";
import { effectiveEmail } from "../../core/store";
import { viewRoute } from "../../core/routes";
import type { Route } from "../../core/routes";
import { viewKey, withAccount } from "../../core/views";
import type { View } from "../../core/views";
import type { Area, Id, Mailbox, MailboxRole } from "../../core/jmap/types";
import { useListRoute, useRoute } from "../routing";
import { useApp, useMailState } from "../state";
import { t } from "../i18n";
import type { MessageKey } from "../i18n";
import { AccountSwitcher } from "./AccountSwitcher";
import { outboxCount } from "./outbox";
import { useAccounts } from "./useAccounts";

/** What a role is called wherever the rail names one. Sent and Drafts are
 * entries near the top; Archive, Junk and Trash moved into the folder
 * disclosure at the bottom, where they are still unified role views. */
const ROLE_LABEL: Record<string, MessageKey> = {
  inbox: "nav.inbox",
  sent: "nav.sent",
  drafts: "nav.drafts",
  archive: "nav.archive",
  junk: "nav.junk",
  trash: "nav.trash",
};

interface Props {
  view: View;
  /** Navigate. It takes a ROUTE rather than a View since #375: half of what the
   * rail points at — the tray, all mail, an area, settings — is a page and not
   * a view, and `viewRoute` is the bridge for the half that still is one. */
  onNavigate: (route: Route) => void;
  onCompose: () => void;
  /** The ids a drag in the message list is carrying (#62), or [] when none. Held by the
   * shell so the rail can light up only the targets that would actually do something. */
  dragIds: Id[];
  /**
   * Whether the rail carries the account switcher (#375 increment 7).
   *
   * False on the phone — and false means NOT RENDERED, not hidden. The desk's
   * own header is the switcher's trigger there, and the panel is a bottom
   * sheet the shell mounts instead; two mounted copies would both publish
   * `shell.railKeys` and `g g` would open whichever rendered last, which on a
   * `display: none` rail is one nobody can see.
   */
  showSwitcher?: boolean;
}

export function Sidebar({
  view,
  onNavigate,
  onCompose,
  dragIds,
  showSwitcher = true,
}: Props): JSX.Element {
  const state = useMailState();
  const { actions, sync, store, pushToast, offerUndo } = useApp();
  const route = useRoute();
  const listRoute = useListRoute();
  const [creating, setCreating] = useState(false);
  const [newLabel, setNewLabel] = useState("");
  const [dropOver, setDropOver] = useState<string | null>(null);
  const [foldersOpen, setFoldersOpen] = useState(false);

  /** The account everything on this rail is counted in — the URL's, which is
   * what the switcher writes. */
  const scope: Id | null = listRoute.acct ?? null;

  const mailboxes = useMemo(() => Object.values(state.mailboxes), [state.mailboxes]);

  /** The areas, in the order the desk draws them — the same total order
   * `deskSections` uses, so the rail, the sections and the `1`–`9` re-file keys
   * cannot disagree about which area is third. */
  const areas: Area[] = useMemo(
    () =>
      Object.values(state.areas).sort((a, b) =>
        a.position !== b.position ? a.position - b.position : a.id.localeCompare(b.id),
      ),
    [state.areas],
  );

  /** Every area's OWN label, which is not listed under Labels: those labels ARE
   * the areas, and listing both would offer two doors into one list — one of
   * which (the label) shows strictly less, since an area also holds whatever
   * its other feeds resolved into it. */
  const areaLabelIds = useMemo(() => new Set(areas.map((a) => a.labelId)), [areas]);

  const labels = useMemo(
    () =>
      Object.values(state.labels)
        .filter((l) => !areaLabelIds.has(l.id))
        .sort((a, b) => a.name.localeCompare(b.name)),
    [state.labels, areaLabelIds],
  );

  // Only the PINNED saved searches appear here (#57) — an unpinned one is saved but
  // reachable from the search box, which is the deliberate variant. Ordered by
  // position then name, matching the server's own ordering so the rail does not
  // reshuffle relative to what /get returned.
  const smartFolders = useMemo(
    () =>
      Object.values(state.savedSearches)
        .filter((s) => s.inSidebar)
        .sort((a, b) => a.position - b.position || a.name.localeCompare(b.name)),
    [state.savedSearches],
  );

  // -- the numbers ------------------------------------------------------------

  /**
   * The summary, but ONLY if it was taken at this scope — the same check
   * DeskRoute makes, and for the same reason: after an account switch the
   * previous scope's counts are still in the store until the new ones land, and
   * a badge from another account's desk is a wrong number rather than an old
   * one.
   */
  const summary =
    state.deskSummary && state.deskSummary.scope === scope ? state.deskSummary : null;
  const trayTotal = state.sections[sectionKey("tray", null, scope)]?.total ?? null;
  const counts = useMemo(() => railCounts(summary, trayTotal), [summary, trayTotal]);

  /** Queued or scheduled sends, AND sends that were refused (#62 / spec §5). The
   * client knows about the submissions it has seen, so this appears when there is
   * something to say and is absent otherwise — never a permanent "Outbox 0",
   * which would be a container the user has to check.
   *
   * ⚠ COUNTED BY THE VIEW'S OWN FUNCTION, NOT BY A COPY OF ITS RULE (#602). This
   * used to filter `undoStatus === "pending"` here. A failed submission is
   * `final` in RFC 8621's three-value vocabulary, so it counted zero, so this
   * entry vanished — at precisely the moment a send failed. The Outbox was
   * unreachable for the one event it exists to report, while `outbox.ts` had
   * been listing failures correctly all along. One rule, one function, both
   * callers. */
  const outbox = useMemo(() => outboxCount(state.submissions), [state.submissions]);

  /** Drafts across every account, from the mailboxes' own totals. */
  const drafts = useMemo(
    () => mailboxes.filter((m) => m.role === "drafts").reduce((n, m) => n + m.totalEmails, 0),
    [mailboxes],
  );

  const unreadFor = (role: Exclude<MailboxRole, null>): number =>
    mailboxes.filter((m: Mailbox) => m.role === role).reduce((n, m) => n + m.unreadEmails, 0);

  /** The roles that lost their own rail entry and moved into the folder
   * disclosure. They are still UNIFIED role views — one entry across every
   * account — and still the drop targets they were. */
  const rolesBelow = (["archive", "junk", "trash"] as Exclude<MailboxRole, null>[]).filter((r) =>
    mailboxes.some((m) => m.role === r),
  );
  const otherFolders = useMemo(
    () => mailboxes.filter((m) => !m.role).sort((a, b) => a.name.localeCompare(b.name)),
    [mailboxes],
  );
  /** otherFolders, grouped by account when the login has more than one
   * (#488) — see core/folderGroups.ts. With one account this is a single
   * group with no heading, so the rendered rail is byte-for-byte what it drew
   * before grouping existed. */
  const { accounts } = useAccounts();
  const folderGroups = useMemo(
    () => groupFoldersByAccount(otherFolders, accounts),
    [otherFolders, accounts],
  );
  /** Open when the user opened it — or while a drag is in flight, so the drop
   * targets inside are reachable without having opened it first. */
  const foldersShown = foldersOpen || dragIds.length > 0;

  // -- drag and drop ----------------------------------------------------------
  //
  // DRAG-AND-DROP FILING (#62). The rules live in core/dragDrop.ts, tested without a DOM;
  // this is the event plumbing.
  //
  // A TARGET THAT WOULD DO NOTHING REFUSES THE DROP — dropping mail on the folder it is
  // already in, or a label it already has, is declined so the cursor says so before the
  // user commits rather than the drop landing and changing nothing.
  //
  // NONE OF THIS IS THE ONLY WAY TO FILE. The label menu, `e` and `#` all still work; a
  // pointer drag is an accelerator, and treating it as the only route would exclude
  // anyone who cannot make the gesture.
  const dropCtx = (ids: Id[]): DropContext => ({
    ids,
    currentRole: view.kind === "role" ? view.role : null,
    areaLabelIds: areas.map((a) => a.labelId),
    // Which labels each dragged message already carries, read through the overlay so a
    // label applied a moment ago counts.
    labelsOn: Object.fromEntries(
      ids.map((id) => {
        const e = effectiveEmail(state, id);
        // By id (#704): local-only labels count too, and effectiveEmail folds a
        // mirrored label's pending keyword change into labelIds already.
        const on = e ? Object.keys(e.labelIds) : [];
        return [id, on];
      }),
    ),
  });

  const onDragOverTarget = (ev: React.DragEvent, target: DropTarget, key: string): void => {
    if (dragIds.length === 0) return;
    if (!canDrop(target, dropCtx(dragIds))) return;
    // preventDefault is what MAKES it a drop target — without it the browser refuses.
    ev.preventDefault();
    ev.dataTransfer.dropEffect = target.kind === "label" ? "copy" : "move";
    setDropOver(key);
  };

  const onDropOn = async (ev: React.DragEvent, target: DropTarget): Promise<void> => {
    ev.preventDefault();
    setDropOver(null);
    // Read the ids from the TRANSFER, not from component state: the transfer is what the
    // platform actually delivered, and it is the only thing guaranteed to describe the
    // drag that is being completed.
    const ids = decodeDrag(ev.dataTransfer.getData(DRAG_MIME));
    if (ids.length === 0) return;
    if (!canDrop(target, dropCtx(ids))) return;
    if (target.kind === "area") {
      // A MOVE (#728): the other areas' labels come off and the target's goes on,
      // through the one path Re-file and Move to area use, with one Undo.
      const deps = { store, sync, actions, offerUndo, pushToast };
      const outcome = await moveToArea(deps, ids, target.areaId);
      if (outcome) offerRuleSuggestion(deps, ids, target.areaId, outcome.moved);
      return;
    }
    if (target.kind === "label") {
      // An area's own label is never a plain add: the earliest area wins, so the
      // message would not move (#728). Route it through the area move.
      const owner = areas.find((a) => a.labelId === target.labelId);
      if (owner) {
        const deps = { store, sync, actions, offerUndo, pushToast };
        const outcome = await moveToArea(deps, ids, owner.id);
        if (outcome) offerRuleSuggestion(deps, ids, owner.id, outcome.moved);
        return;
      }
      // Whichever path the label needs — its atom, or its id when it is
      // local-only (#704) — exactly as the label menu applies it.
      const label = state.labels[target.labelId];
      if (!label) return;
      await actions.applyLabel(ids, label, true);
      return;
    }
    switch (target.role) {
      case "archive":
        await actions.archive(ids);
        return;
      case "trash":
        await actions.trash(ids);
        return;
      case "inbox": {
        // NOT `untrash` for everything (#528): that archived any message that was not in
        // Trash. core/dragDrop.ts inboxDropPlan says which write each message gets.
        const plan = inboxDropPlan(ids, dropCtx(ids).currentRole, (id) => isInJunk(state, id));
        if (plan.untrash.length > 0) await actions.untrash(plan.untrash);
        if (plan.notJunk.length > 0) await actions.notJunk(plan.notJunk);
        if (plan.restoreToInbox.length > 0) await actions.restoreToInbox(plan.restoreToInbox);
        return;
      }
    }
  };

  // -- entries ----------------------------------------------------------------

  /** One rail entry.
   *
   * `badge` is a ReactNode rather than a number because an area's is two
   * numbers ("4 / 19"), and `null`/`undefined` means NOT KNOWN — the entry then
   * carries no badge at all rather than a zero. */
  const entry = (opts: {
    key: string;
    label: string;
    to: Route;
    current: boolean;
    badge?: ReactNode;
    target?: DropTarget | null;
    icon?: string;
  }): JSX.Element => {
    const { key, label, to, current, badge, target, icon } = opts;
    return (
      <li key={key}>
        <button
          type="button"
          className={`nav-item${dropOver === key ? " is-drop-target" : ""}`}
          aria-current={current ? "page" : undefined}
          {...(target
            ? {
                onDragOver: (ev: React.DragEvent) => onDragOverTarget(ev, target, key),
                onDragLeave: () => setDropOver(null),
                onDrop: (ev: React.DragEvent) => void onDropOn(ev, target),
              }
            : {})}
          onClick={() => onNavigate(to)}
        >
          {icon ? (
            <span className="nav-item-icon" aria-hidden="true">
              {icon}
            </span>
          ) : null}
          <span className="nav-item-label">{label}</span>
          {badge !== null && badge !== undefined ? (
            <span className="nav-badge">{badge}</span>
          ) : null}
        </button>
      </li>
    );
  };

  /** The route for a legacy view, carrying the current scope — `viewRoute`
   * lifts a view's account into the route's `?acct=`, so every list the rail
   * points at is the same account's. */
  // `withAccount` is the one place that knows which View kinds carry a scope
  // (core/views.ts), so the rail does not keep a second list of the kinds that do
  // not — which is what this was, and what broke when the Outbox became a View
  // (#448: a submission carries an identity and no account).
  const toView = (v: View): Route => viewRoute(withAccount(v, scope));

  // On Contacts (#816) no mail view is where you are, though the mail list
  // underneath is still the last one you were on: no entry for it is current.
  const active: string | null = route.page === "contacts" ? null : viewKey(view);
  const onInbox = route.page === "inbox" && !route.sort;
  const onAllMail = route.page === "all" && !route.area && !route.label;

  /** One row of the raw IMAP folder list, shared by the ungrouped (one
   * account) and grouped (several accounts) renders below so the two paths
   * cannot draw a folder differently. */
  const folderRow = (m: Mailbox): JSX.Element => {
    // SELECTIVE SYNC (#62). A folder with syncing off is a FROZEN SNAPSHOT: it
    // keeps everything it had and gains nothing, and the server does not reap
    // it either. Drawing it like a live folder would be a lie about mail — it
    // can list messages the sender has since deleted and miss everything that
    // arrived — so the state is on the row itself even now that the CONTROL
    // is in Settings → Accounts → "Folders synced" (#374).
    const off = m.syncScope === "off";
    const key = viewKey({ kind: "mailbox", mailboxId: m.id });
    return (
      <li key={key} className={off ? "nav-row is-not-syncing" : "nav-row"}>
        <button
          type="button"
          className="nav-item"
          aria-current={key === active ? "page" : undefined}
          onClick={() => onNavigate(viewRoute({ kind: "mailbox", mailboxId: m.id }))}
        >
          <span className="nav-item-label">{m.name}</span>
          {off ? (
            <span className="nav-frozen" title={t("sync.frozenHint")}>
              {t("sync.frozenTag")}
            </span>
          ) : null}
          {m.unreadEmails ? <span className="nav-badge">{m.unreadEmails}</span> : null}
        </button>
      </li>
    );
  };

  return (
    <nav className="sidebar" aria-label={t("nav.mailboxes")}>
      {showSwitcher ? <AccountSwitcher /> : null}

      <button type="button" className="compose-button" onClick={onCompose}>
        ✎ {t("action.compose")}
      </button>

      <ul className="nav-list">
        {/* THE INBOX IS THE DESK. It navigates to the bare `#/inbox`, not to a
            tab: which half you land on is remembered per device (routing.ts
            applyRemembered), so the rail entry means "my desk" rather than
            "the review half", and pressing it from the other tab does not
            throw away the tab you chose. */}
        {entry({
          key: "inbox",
          label: t("nav.inbox"),
          to: { page: "inbox", tab: null },
          current: onInbox,
          badge: counts.inbox,
          // What the entry NAVIGATES to and what a drop on it MEANS are two
          // different things: a drop is a move back to the inbox, decided per
          // message by core/dragDrop.ts inboxDropPlan (#528).
          target: { kind: "role", role: "inbox" },
        })}
        {entry({
          key: "tray",
          label: t("rail.tray"),
          to: { page: "tray" },
          current: route.page === "tray",
          badge: counts.tray,
        })}
        {/* Outbox and Drafts APPEAR AND DISAPPEAR WITH THEIR CONTENTS. Both are
            waiting rooms: an empty one is not a place, and a permanent entry
            reading "0" is a thing to check rather than a thing to see. */}
        {outbox > 0
          ? entry({
              key: "outbox",
              label: t("rail.outbox"),
              // #448: there IS a screen now. It used to land on Sent — "where the
              // mail ends up moments later" — because nothing listed
              // EmailSubmission as a list. `#/outbox` does, and it is deliberately
              // NOT scoped by `toView`: a submission carries an identityId and no
              // account, so a scoped Outbox would hide rows whenever an identity
              // had not loaded, and under-reporting what is waiting is the one
              // thing this list must not do (core/views.ts `outbox`).
              to: viewRoute({ kind: "outbox" }),
              current: active === viewKey({ kind: "outbox" }),
              badge: outbox,
            })
          : null}
        {drafts > 0
          ? entry({
              key: "drafts",
              label: t("nav.drafts"),
              to: toView({ kind: "role", role: "drafts" }),
              current: active === viewKey({ kind: "role", role: "drafts" }),
              badge: drafts,
            })
          : null}
        {entry({
          key: "sent",
          label: t("nav.sent"),
          to: toView({ kind: "role", role: "sent" }),
          current: active === viewKey({ kind: "role", role: "sent" }),
        })}
        {/* ALL MAIL is not the archive. The archive is where mail GOES; this is
            where mail IS — everything except the trash, snoozed and archived
            mail included (core/views.ts `all`). */}
        {entry({
          key: "all",
          label: t("nav.all"),
          to: { page: "all" },
          current: onAllMail,
          badge: counts.allMail,
        })}
      </ul>

      {/* AREAS. The badge is "review / drawer" — two numbers, because they are
          two different questions ("what is this area asking of me" and "what
          has it kept for later") and a sum would answer neither. A hidden area
          keeps its mail and its sections; only this entry goes. */}
      {areas.length > 0 ? (
        <>
          <h2 className="nav-heading">{t("rail.areas")}</h2>
          <ul className="nav-list">
            {areas
              .filter((a) => !a.hiddenFromRail)
              .map((area) => {
                const c = summary ? areaCounts(summary.rows, area.id) : null;
                return entry({
                  key: `area:${area.id}`,
                  label: area.name,
                  to: { page: "all", area: area.id },
                  current: route.page === "all" && route.area === area.id,
                  target: { kind: "area", areaId: area.id, labelId: area.labelId },

                  badge: c ? (
                    <span className="nav-badge-split">
                      {c.review}
                      <span className="nav-badge-sep"> / </span>
                      {c.drawer}
                    </span>
                  ) : null,
                });
              })}
            {/* NEEDS SORTING is the pile the areas were made out of, so it sits
                with them and last. Its count is ALL THREE buckets of the
                unsorted row — nothing claims that mail, so no drawer will ever
                sweep it and all of it is on the surface (core/desk.ts). */}
            {entry({
              key: "needs-sorting",
              label: t("rail.needsSorting"),
              to: { page: "inbox", tab: null, sort: true },
              current: route.page === "inbox" && !!route.sort,
              badge: counts.needsSorting,
            })}
          </ul>
        </>
      ) : null}

      {/* SMART FOLDERS (#57). Only the pinned ones, and only when there are any — an
          empty heading teaches nothing. A smart folder navigates to the ORDINARY search
          view with its stored query, so it runs through exactly the path typing that
          query takes; there is no second execution path that could disagree. */}
      {smartFolders.length > 0 ? (
        <>
          <h2 className="nav-heading">{t("nav.smartFolders")}</h2>
          <ul className="nav-list">
            {smartFolders.map((s) => (
              <li key={s.id}>
                <button
                  type="button"
                  className="nav-item"
                  aria-current={
                    active === viewKey({ kind: "search", query: s.query }) ? "page" : undefined
                  }
                  title={s.query}
                  onClick={() => onNavigate(viewRoute({ kind: "search", query: s.query }))}
                >
                  <span className="nav-item-icon" aria-hidden="true">
                    ⌕
                  </span>
                  <span className="nav-item-label">{s.name}</span>
                </button>
              </li>
            ))}
          </ul>
        </>
      ) : null}

      <h2 className="nav-heading">{t("nav.labels")}</h2>
      <ul className="nav-list">
        {labels.map((l) => (
          <li key={l.id}>
            <button
              type="button"
              className={`nav-item${dropOver === `label:${l.id}` ? " is-drop-target" : ""}`}
              aria-current={active === viewKey({ kind: "label", labelId: l.id }) ? "page" : undefined}
              onDragOver={(ev) =>
                onDragOverTarget(
                  ev,
                  { kind: "label", labelId: l.id, keyword: l.keyword },
                  `label:${l.id}`,
                )
              }
              onDragLeave={() => setDropOver(null)}
              onDrop={(ev) =>
                void onDropOn(ev, { kind: "label", labelId: l.id, keyword: l.keyword })
              }
              onClick={() => onNavigate(toView({ kind: "label", labelId: l.id }))}
            >
              <span
                className="label-dot"
                style={l.color ? { backgroundColor: l.color } : undefined}
                aria-hidden="true"
              />
              <span className="nav-item-label">{l.name}</span>
            </button>
          </li>
        ))}
        <li>
          {creating ? (
            <form
              className="inline-form"
              onSubmit={(ev) => {
                ev.preventDefault();
                const name = newLabel.trim();
                if (!name) return;
                void actions.createLabel(name);
                setNewLabel("");
                setCreating(false);
              }}
            >
              <input
                className="text-input"
                aria-label={t("labels.name")}
                value={newLabel}
                autoFocus
                onChange={(ev) => setNewLabel(ev.target.value)}
                onBlur={() => setCreating(false)}
              />
            </form>
          ) : (
            <button
              type="button"
              className="nav-item nav-item-muted"
              onClick={() => setCreating(true)}
            >
              {t("nav.newLabel")}
            </button>
          )}
        </li>
      </ul>

      {/* SETTINGS IS A RAIL ENTRY. The gear in the header stays — it is where
          people's hands already are — but the rail is where a destination
          belongs, and settings has been a place with a URL since #373. */}
      <ul className="nav-list">
        {/* CONTACTS (#816): the people you correspond with — a place with its
            own URL, beside Settings rather than inside it (#814 is shrinking
            Settings, not growing it). */}
        {entry({
          key: "contacts",
          label: t("rail.contacts"),
          to: { page: "contacts" },
          current: route.page === "contacts",
          icon: "@",
        })}
        {entry({
          key: "settings",
          label: t("rail.settings"),
          to: { page: "settings", section: null },
          current: route.page === "settings",
          icon: "⚙",
        })}
      </ul>

      {/* FOLDERS — the roled folders that no longer have a rail entry of their
          own, then the raw IMAP folder list.
          ⚠ THE PAUSE/RESUME CONTROL LEFT THIS BLOCK IN #374 and now lives in
          Settings → Accounts → "Folders synced", which is where folder
          management belongs. What did NOT leave is the STATE: a folder with
          syncing off is a frozen snapshot, and the `frozen` tag below still says
          so on the row, because that state has to be visible where the folder is
          rather than only where it is configured. Moving the tag as well would
          have made a rail full of live-looking folders out of a rail that had
          been honest.
          IT OPENS ITSELF DURING A DRAG. Archive and Trash are drop targets
          (#62) and they are inside this disclosure now, so leaving it shut
          during a drag would not have MOVED drag-to-archive, it would have
          deleted it.
          GROUPED BY ACCOUNT WHEN THERE IS MORE THAN ONE (#488, folderGroups
          above). With one account folderGroups is a single ungrouped list and
          this renders exactly as it did before grouping existed — rolesBelow
          and the folder rows in one <ul>, no heading. With several, each
          account's folders get their own <ul> under an <h2 className=
          "nav-heading"> naming it — the same element the rail already uses
          for "Smart folders" — so a screen reader gets a heading per
          account, not one flat list pretending the folders are all one
          account's. */}
      {rolesBelow.length > 0 || otherFolders.length > 0 ? (
        <>
          <button
            type="button"
            className="nav-heading nav-disclosure"
            aria-expanded={foldersShown}
            onClick={() => setFoldersOpen((v) => !v)}
          >
            <span className="nav-item-icon" aria-hidden="true">
              {foldersShown ? "▾" : "▸"}
            </span>
            {t("nav.folders")}
          </button>
          {foldersShown ? (
            <>
              <ul className="nav-list">
                {rolesBelow.map((role) => {
                  // Archive, Junk and Trash are UNIFIED role views, exactly as
                  // they were when they had rail entries of their own — one entry
                  // over every account's folder, and the same drop target.
                  const to = toView({ kind: "role", role });
                  const dropRole = role === "archive" || role === "trash" ? role : null;
                  return entry({
                    key: `role:${role}`,
                    label: t(ROLE_LABEL[role] ?? "nav.folders"),
                    to,
                    current: active === viewKey({ kind: "role", role }),
                    badge: unreadFor(role) || null,
                    target: dropRole ? { kind: "role", role: dropRole } : null,
                  });
                })}
                {/* UNGROUPED: one account (or nothing left to group). Rendered
                    inline with rolesBelow, exactly as before grouping existed. */}
                {folderGroups.length <= 1
                  ? (folderGroups[0]?.folders ?? []).map((m) => folderRow(m))
                  : null}
              </ul>
              {/* GROUPED: more than one account. */}
              {folderGroups.length > 1
                ? folderGroups.map((group) => (
                    <Fragment key={group.accountId ?? "unknown-account"}>
                      <h2 className="nav-heading">{group.name}</h2>
                      <ul className="nav-list">{group.folders.map((m) => folderRow(m))}</ul>
                    </Fragment>
                  ))
                : null}
            </>
          ) : null}
        </>
      ) : null}

      {state.session ? (
        <p className="sidebar-footer">{t("app.signedInAs", { email: state.session.username })}</p>
      ) : null}
    </nav>
  );
}
