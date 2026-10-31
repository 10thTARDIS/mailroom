// SPDX-License-Identifier: AGPL-3.0-only

// The message list: multi-select (checkbox + shift-range + select-all-matching),
// the bulk-action toolbar, and the rows themselves.
//
// Rows render THROUGH the overlay (core/store.ts effectiveEmail), so a star or a
// read/unread flip shows the instant it is clicked and keeps showing while the
// action queue catches up. A row whose change the server has not confirmed gets
// a quiet "pending" cue rather than a spinner: the UI is never blocked on a
// round trip.

import type { ReactNode, Ref } from "react";
import { Fragment, useMemo, useRef } from "react";
import type { JSX } from "react";

import { effectiveEmail } from "../../core/store";
import { formatWake } from "../../core/snooze";
import { senderIdentity, stripBidiControls } from "../../core/hostileText";
import type { QueueGroup, QueueLayout } from "../../core/needsCategory";
import type { Id } from "../../core/jmap/types";
import { useMailState } from "../state";
import { t } from "../i18n";
import { listCountExplain, listCountText } from "./listCount";
import { DRAG_MIME, dragSet, encodeDrag } from "../../core/dragDrop";
import { QueueEmptyState, QueueGroupHeader, QueueOutcomes, SuggestionStrip } from "./NeedsCategory";
import type { QueueGroupOutcome } from "./NeedsCategory";
import { AreaMenu } from "./AreaMenu";
import type { RefileTarget } from "./desk/SelectionToolbar";

interface Props {
  ids: Id[];
  selected: Set<Id>;
  cursor: Id | null;
  openId: Id | null;
  inTrash: boolean;
  /** True in the Junk view (#528): the selection's first verb is "Not junk", in the
   * slot Archive has everywhere else — archiving spam is not a rescue. */
  inJunk: boolean;
  allMatchingCount: number;
  /** A truncated "select all matching" has been acted on (#769): what is done, of
   * how many, and how many the next batch takes. Null when there is no such run. */
  nextBatch?: { done: number; total: number; next: number } | null;
  onSelectNextBatch?: () => void;
  onToggleSelect: (id: Id, shift: boolean) => void;
  /** A drag started (or ended, with []) carrying these ids (#62). The shell holds it
   * so the sidebar can light up the targets that would actually do something. */
  onDragIds?: (ids: Id[]) => void;
  onSelectAllOnPage: () => void;
  onSelectAllMatching: () => void;
  onClearSelection: () => void;
  onOpen: (id: Id) => void;
  onPin: (id?: Id) => void;
  onSnooze: (id?: Id) => void;
  /** True in the Snoozed view, where the row action means "bring it back". */
  inSnoozed: boolean;
  /** Rendered at the very top of `.list-pane`, above the toolbar — the category-tab
   * strip (#228).
   *
   * ⚠ IT MUST LIVE HERE, NOT AS A SIBLING OF THE LIST. `.content` is a CSS grid
   * whose columns ARE the reading-pane layout (list | splitter | thread), so an
   * extra direct child silently becomes grid column 1 and shifts every pane one
   * column across — which is exactly what the first version did. A ReactNode
   * rather than tab-specific props keeps this component ignorant of tabs. */
  header?: ReactNode;
  onImportant: (id?: Id) => void;
  /** Every row the selection bar's button would act on is already important
   * (#921): the button then offers "Not important" instead of "Mark as
   * important". Optional so a surface that does not compute it keeps the old
   * label. */
  importantAll?: boolean;
  /** True in the Priority view: draw the divider between the important bucket
   * and everything else (#154). */
  showPriorityBuckets: boolean;
  /** True in the needs-a-category view (#176): rows carry their open questions
   * and a dismiss action, alias/domain group headers are drawn, and the empty
   * state reads as success rather than as an empty folder. */
  inNeedsCategory: boolean;
  /** The queue's grouping, computed by the shell so that `ids` arrives in the
   * SAME order it describes (#183). Null outside the queue. */
  queueLayout: QueueLayout | null;
  /** What each answered batch did, newest first. Rendered ABOVE the list, not in
   * the group's old position: answering empties the group, so a header anchored
   * to its first row would disappear along with the blast radius it was there to
   * show. */
  groupOutcomes: QueueGroupOutcome[];
  /** Label / dismiss a whole group at once — the supervision multiplier. */
  onLabelGroup: (group: QueueGroup) => void;
  onDismissGroup: (group: QueueGroup) => void;
  /** "None of these" for one message (#176). */
  onDismiss: (id?: Id) => void;
  onArchive: (id?: Id) => void;
  onTrash: (id?: Id) => void;
  onUntrash: (id?: Id) => void;
  onNotJunk: (id?: Id) => void;
  onStar: (id?: Id) => void;
  onToggleRead: (id: Id) => void;
  onMarkRead: () => void;
  onMarkUnread: () => void;
  onLabel: () => void;
  /** The areas "Move to area" offers, in the desk's order (#708). */
  areaTargets: RefileTarget[];
  /** Re-file the whole selection into this area (#708). */
  onMoveToArea: (areaId: Id) => void;
  onLoadMore: () => void;
  /** The scrolling element of the list (`.list-pane` is the one with
   * `overflow-y`), handed up so the route can put the scroll position back where
   * it was when you return to this list (#373). Optional: nothing else needs it,
   * and a list without it just always opens at the top. */
  paneRef?: Ref<HTMLElement>;
}

/** Renders a SearchSnippet safely. The server escapes everything except the
 * <mark> tags it adds (internal/search/backend.go Snippet.Mark), so the string
 * is parsed here into text + <mark> React nodes — never injected as HTML.
 * dangerouslySetInnerHTML on message-derived content is exactly the hostile-HTML
 * hole spec §11 tells us not to open.
 *
 * The text runs are bidi-stripped on the way out (#307), like every other piece
 * of message-derived chrome text in this file. A snippet is a fragment of a
 * SUBJECT or a BODY, so it carries whatever the sender put there — and a snippet
 * is spliced between <mark> elements in one line box, which is precisely the
 * shape a directional override reorders. */
function renderSnippet(html: string): ReactNode {
  const parts = html.split(/(<mark>|<\/mark>)/);
  const out: ReactNode[] = [];
  let marking = false;
  parts.forEach((part, i) => {
    if (part === "<mark>") {
      marking = true;
      return;
    }
    if (part === "</mark>") {
      marking = false;
      return;
    }
    if (!part) return;
    const text = stripBidiControls(unescapeEntities(part));
    out.push(
      marking ? (
        <mark key={i}>{text}</mark>
      ) : (
        <Fragment key={i}>{text}</Fragment>
      ),
    );
  });
  return out;
}

function unescapeEntities(s: string): string {
  return s
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&#34;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&amp;/g, "&");
}

/** Whether a snooze return is recent enough to still be worth explaining (#152).
 * A day: long enough to cover "it came back this morning and I got to it after
 * lunch", short enough that a month-old return is not still shouting about
 * itself. */
function isRecentReturn(iso: string, now: number = Date.now()): boolean {
  const at = Date.parse(iso);
  return !Number.isNaN(at) && now - at < 24 * 60 * 60 * 1000;
}

function formatDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const now = new Date();
  const sameDay =
    d.getFullYear() === now.getFullYear() &&
    d.getMonth() === now.getMonth() &&
    d.getDate() === now.getDate();
  return sameDay
    ? d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" })
    : d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

export function MessageList(props: Props): JSX.Element {
  const state = useMailState();
  const { ids, selected, cursor, openId, inTrash, inJunk, inSnoozed, onDragIds, header } = props;
  const list = state.list;

  const rows = useMemo(
    () => ids.map((id) => effectiveEmail(state, id)).filter((e): e is NonNullable<typeof e> => !!e),
    [ids, state],
  );

  // The alias/domain grouping (#176). Headers are rendered inline, as list rows,
  // exactly like the priority bucket divider — splitting the list per group
  // would mean N scroll regions and N "load more" buttons.
  //
  // The layout is computed by the SHELL and handed down (core/needsCategory.ts
  // layoutQueue), not recomputed here (#183). `props.ids` already arrives in
  // display order, so the rows the eye sees and the rows `j`/`k`, shift-range
  // and select-all walk are one list. Deriving the order here — as this
  // component briefly did — is what made them two.
  const layout = props.queueLayout;
  const groupHeaderFor = layout?.headerFor;
  const grouped = layout?.grouped;
  const groupLast = layout?.groupLast;
  const restStartsAt = layout?.restStartsAt ?? null;
  const ordered = rows;

  // Whether Shift was held for the interaction now in flight, captured on
  // mousedown because that fires BEFORE the change event and is the only place
  // the modifier is reliably readable. One ref for the whole list is enough —
  // only one row can be clicked at a time.
  const shiftHeld = useRef(false);

  const allOnPageSelected = rows.length > 0 && rows.every((r) => selected.has(r.id));
  const hasSelection = selected.size > 0;

  return (
    <section className="list-pane" ref={props.paneRef} aria-label={t("nav.mailboxes")}>
      {header}
      <div className="list-toolbar" role="toolbar" aria-label="Message actions">
        <label className="checkbox">
          <input
            type="checkbox"
            checked={allOnPageSelected}
            aria-label={t("list.selectAll")}
            onChange={props.onSelectAllOnPage}
          />
        </label>

        {hasSelection ? (
          <>
            <span className="selection-count">{t("list.selected", { count: selected.size })}</span>
            {inTrash ? (
              <button type="button" className="button" onClick={() => props.onUntrash()}>
                {t("action.untrash")}
              </button>
            ) : inJunk ? (
              <button type="button" className="button" onClick={() => props.onNotJunk()}>
                {t("action.notJunk")}
              </button>
            ) : (
              <button type="button" className="button" onClick={() => props.onArchive()}>
                {t("action.archive")}
              </button>
            )}
            <button type="button" className="button" onClick={() => props.onTrash()}>
              {t("action.trash")}
            </button>
            {/* NOTHING APPEARS ON HOVER OVER A ROW (#372 round 2, the operator's
                rule; #447 restated it for every width). The per-row toolbar that
                used to show on hover and focus-within is gone from every surface
                this list draws — All mail, Sent, folders, labels, search, the
                needs-a-category queue — and its verbs live HERE, on the selection,
                exactly as the desk's SelectionToolbar does. Each verb below resolves
                to the selected rows (ListRoute's `targets`), and the keyboard keeps
                its single-row shortcuts. */}
            <button type="button" className="button" onClick={() => props.onPin()}>
              {t("action.pin")}
            </button>
            <button type="button" className="button" onClick={() => props.onSnooze()}>
              {t("action.snooze")}
            </button>
            <button type="button" className="button" onClick={() => props.onImportant()}>
              {props.importantAll ? t("priority.markNotImportant") : t("priority.markImportant")}
            </button>
            {props.inNeedsCategory ? (
              <button type="button" className="button" onClick={() => props.onDismiss()}>
                {t("queue.dismissOne")}
              </button>
            ) : null}
            <button type="button" className="button" onClick={props.onMarkRead}>
              {t("action.markRead")}
            </button>
            <button type="button" className="button" onClick={props.onMarkUnread}>
              {t("action.markUnread")}
            </button>
            <button type="button" className="button" onClick={props.onLabel}>
              {t("action.label")}
            </button>
            <AreaMenu
              buttonText={t("areaMove.menu")}
              menuLabel={t("areaMove.menuLabel")}
              areas={props.areaTargets}
              onPick={props.onMoveToArea}
            />
            <button type="button" className="button button-quiet" onClick={props.onClearSelection}>
              {t("action.cancel")}
            </button>
          </>
        ) : (
          // ⚠ A CAPPED SEARCH'S TOTAL IS A FLOOR (#722, #724): "at least", and the
          // title says why and what to do about it. See listCount.ts.
          <span className="list-count" title={listCountExplain(list.total, list.searchCapped)}>
            {listCountText(rows.length, list.total, list.searchCapped)}
          </span>
        )}
      </div>

      {hasSelection && props.allMatchingCount === 0 && list.total > rows.length ? (
        <div className="select-all-banner">
          <button type="button" className="link-button" onClick={props.onSelectAllMatching}>
            {list.searchCapped !== null
              ? t("list.selectAllMatchingBound", { total: list.total })
              : t("list.selectAllMatching", { total: list.total })}
          </button>
        </div>
      ) : null}

      {props.nextBatch && props.nextBatch.next > 0 ? (
        <div className="select-all-banner">
          <button type="button" className="link-button" onClick={props.onSelectNextBatch}>
            {t("list.nextBatch", {
              done: props.nextBatch.done,
              total: props.nextBatch.total,
              next: props.nextBatch.next,
            })}
          </button>
        </div>
      ) : null}

      {list.searchUnavailable ? (
        <p className="empty" role="status">
          {t("search.unavailable")}
        </p>
      ) : null}
      {list.error ? (
        <p className="empty error" role="alert">
          {list.error}
        </p>
      ) : null}

      {props.inNeedsCategory ? <QueueOutcomes outcomes={props.groupOutcomes} /> : null}

      <ul className="messages" role="list">
        {ordered.map((e, i) => {
          const snippet = state.snippets[e.id];
          const isSelected = selected.has(e.id);
          // WHO SENT IT, AND WHETHER THE ROW MAY SAY SO IN ONE STRING (#307).
          // This used to be `from?.name || from?.email`, i.e. the display NAME
          // alone whenever there was one — so `From: "security@bank.example"
          // <attacker@evil.example>` rendered as the bank, truthfully escaped and
          // entirely wrong. senderIdentity strips bidi controls from both halves
          // and decides whether the name is confusable enough that the row must
          // show the address beside it; see core/hostileText.ts for why that rule
          // is narrow, and what it deliberately does not catch.
          const sender = senderIdentity(e.from?.[0]);
          // Every other piece of chrome text on this row comes from the sender
          // too, and gets the same treatment.
          const subject = stripBidiControls(e.subject) || t("list.noSubject");
          // The bucket divider (#154). The list is already grouped — important
          // first — so the boundary is simply where that flips, and the headers
          // are rendered inline rather than by splitting the list, which would
          // mean two scroll regions and two "load more" buttons.
          const prev = i > 0 ? ordered[i - 1] : undefined;
          const bucketHeader =
            props.showPriorityBuckets && (i === 0 || prev?.isImportant !== e.isImportant)
              ? e.isImportant
                ? t("priority.bucketImportant")
                : t("priority.bucketRest")
              : null;
          const group = groupHeaderFor?.get(e.id);
          return (
            <Fragment key={`row-${e.id}`}>
            {bucketHeader ? (
              <li className="list-bucket" role="presentation">
                {bucketHeader}
              </li>
            ) : null}
            {/* Where the batches stop and the one-offs begin. A heading of its
                own, because a run of ungrouped rows with nothing above it reads
                as a continuation of the last group. */}
            {restStartsAt === e.id ? (
              <li className="list-bucket" role="presentation">
                {t("queue.rest")}
              </li>
            ) : null}
            {group ? (
              <QueueGroupHeader
                group={group}
                // The grouping can only see the rows the client holds, so its
                // counts are totals only when the client holds the whole queue.
                complete={rows.length >= list.total}
                onLabel={props.onLabelGroup}
                onDismiss={props.onDismissGroup}
              />
            ) : null}
            <li
              key={e.id}
              className={[
                "message-row",
                e.isSeen ? "is-read" : "is-unread",
                e.isPinned ? "is-pinned" : "",
                // Belongs to the group whose header is above, and closes it —
                // so a headerless row that follows is visibly not part of it.
                grouped?.has(e.id) ? "is-grouped" : "",
                groupLast?.has(e.id) ? "is-group-last" : "",
                isSelected ? "is-selected" : "",
                cursor === e.id ? "is-cursor" : "",
                openId === e.id ? "is-open" : "",
              ]
                .filter(Boolean)
                .join(" ")}
              // DRAG TO FILE (#62). The row is the drag handle, and what it carries is
              // the SELECTION when this row is part of it — acting on one row while five
              // are highlighted is how a drag loses mail (core/dragDrop.ts).
              //
              // A custom MIME type, so a mail id list is never offered to a text field
              // elsewhere on the page or to another application.
              draggable
              onDragStart={(ev) => {
                const ids = dragSet(e.id, selected);
                ev.dataTransfer.setData(DRAG_MIME, encodeDrag(ids));
                ev.dataTransfer.effectAllowed = "copyMove";
                onDragIds?.(ids);
              }}
              onDragEnd={() => onDragIds?.([])}
            >
              <label className="checkbox">
                <input
                  type="checkbox"
                  checked={isSelected}
                  aria-label={`Select “${subject}”`}
                  // NEVER preventDefault here. This is a CONTROLLED checkbox:
                  // the browser toggles the DOM node on click, preventDefault
                  // reverts it, and React — whose value tracker recorded the
                  // toggle — then decides the DOM already matches the new
                  // `checked` and skips writing it. The row stays visibly
                  // unchecked while the state says selected, and only repaints
                  // when some LATER render touches it, which is why ticking one
                  // box appeared to tick the previous one (#172).
                  onMouseDown={(ev) => {
                    shiftHeld.current = ev.shiftKey;
                  }}
                  onKeyDown={(ev) => {
                    // Space/Enter never carries a range intent, and without this
                    // the ref would still hold the last mouse click's modifier.
                    if (ev.key === " " || ev.key === "Enter") shiftHeld.current = false;
                  }}
                  // Stop the click reaching the row (which would open the
                  // message), but let the checkbox toggle normally.
                  onClick={(ev) => ev.stopPropagation()}
                  onChange={() => props.onToggleSelect(e.id, shiftHeld.current)}
                />
              </label>

              <button
                type="button"
                className={`star ${e.isFlagged ? "is-on" : ""}`}
                aria-label={e.isFlagged ? t("action.unstar") : t("action.star")}
                aria-pressed={e.isFlagged}
                onClick={() => props.onStar(e.id)}
              >
                {e.isFlagged ? "★" : "☆"}
              </button>

              <button
                type="button"
                className="message-main"
                onClick={() => props.onOpen(e.id)}
                // NO aria-label: the name is the button's own content (WCAG 2.5.3).
                // This button holds sender, subject, snippet, tags and date, so any
                // short label ("sender: subject") leaves visible words out of the
                // name, which axe-core 4.14 reports as label-content-name-mismatch.
                // The children are grid items, so the computed name keeps them
                // apart; the row reads "sender subject snippet ... date".
              >
                <span className="sender">
                  {sender.name || sender.address || t("list.noSender")}
                  {/* The real address, shown only when the display name is a
                      confusable one (#307). A separate element rather than one
                      string so it can recede visually — it is a disambiguation,
                      not part of the name — and so `unicode-bidi: isolate` in
                      styles.css confines each half independently. */}
                  {sender.showAddress ? (
                    <span className="sender-address" title={t("list.senderAddressShown")}>
                      {" "}
                      &lt;{sender.address}&gt;
                    </span>
                  ) : null}
                </span>
                <span className="subject">
                  {e.isDraft ? <span className="tag tag-draft">{t("list.draft")}</span> : null}
                  {snippet?.subject ? renderSnippet(snippet.subject) : subject}
                </span>
                <span className="preview">
                  {snippet?.preview ? renderSnippet(snippet.preview) : stripBidiControls(e.preview)}
                </span>
                {/* What the system thinks, and why (#176). Only in the queue:
                    elsewhere a message's open questions are not what the row is
                    about, and every list would grow a band of model output. */}
                {props.inNeedsCategory ? (
                  <SuggestionStrip email={e} labels={state.labels} />
                ) : null}
                <span className="row-meta">
                  {!e.isSnoozed && e.returnedAt && isRecentReturn(e.returnedAt) ? (
                    // Why a message older than everything around it is at the
                    // top. Without this the list just looks mis-sorted. It fades
                    // out after a day, by which point the return is no longer the
                    // interesting thing about the message.
                    <span className="tag tag-returned">
                      {t("snooze.returned", { when: formatWake(e.returnedAt) })}
                    </span>
                  ) : null}
                  {e.isSnoozed && e.snoozedUntil ? (
                    // WHEN it comes back, on the row itself. In the Snoozed view
                    // that is the only thing the row is really about; anywhere
                    // else (a search, Starred) it explains why a message the
                    // user thought they had put away is on screen at all.
                    <span className="tag tag-snoozed">
                      {t("snooze.returns", { when: formatWake(e.snoozedUntil) })}
                    </span>
                  ) : null}
                  {/* ARCHIVED IN MAILROOM (#446): day-one setup took this message
                      off the desk and NOTHING MOVED on the server — the bytes are
                      still in whatever folder the provider has them in, and no
                      other client saw anything happen.

                      It is drawn wherever the message list is drawn, which is All
                      mail, search and every folder or label view — the desk has its
                      own rows and never shows desk-archived mail at all. The
                      property is in EMAIL_LIST_PROPERTIES for exactly this row
                      (jmap/types.ts says why it is there and not in the body set).

                      Deliberately NOT the same chip as a drawer sweep would earn:
                      that one is a real server-side move, this one is a filing that
                      moved nothing, and the two must never read the same. */}
                  {e.deskArchivedAt ? (
                    <span className="tag tag-desk-archived">{t("list.deskArchived")}</span>
                  ) : null}
                  {e.labelKeywords.length > 0 ? (
                    <span className="tags">
                      {e.labelKeywords.map((atom) => (
                        <span className="tag" key={atom}>
                          {labelName(state.labels, atom)}
                        </span>
                      ))}
                    </span>
                  ) : null}
                  {e.hasAttachment ? (
                    <span className="attach" title={t("list.attachment")} aria-label={t("list.attachment")}>
                      📎
                    </span>
                  ) : null}
                  {e.pending ? (
                    <span className="pending-dot" title={t("list.pending")} aria-label={t("list.pending")} />
                  ) : null}
                  <time dateTime={e.receivedAt}>{formatDate(e.receivedAt)}</time>
                </span>
              </button>

            </li>
            </Fragment>
          );
        })}
      </ul>

      {list.loading ? <p className="empty">{t("list.loading")}</p> : null}
      {!list.loading && rows.length === 0 && !list.searchUnavailable ? (
        // An empty queue is an ACHIEVEMENT — nothing is waiting on you — and
        // saying "Nothing here" would file it alongside an empty Trash.
        props.inNeedsCategory ? (
          <QueueEmptyState />
        ) : (
          <p className="empty">{t("list.empty")}</p>
        )
      ) : null}
      {rows.length > 0 && rows.length < list.total ? (
        <button type="button" className="button load-more" onClick={props.onLoadMore}>
          {t("list.loadMore")}
        </button>
      ) : null}
    </section>
  );
}

/** The label a keyword atom belongs to (Label.keyword is the mirror atom). */
function labelName(labels: Record<string, { name: string; keyword: string | null }>, atom: string): string {
  for (const l of Object.values(labels)) {
    if (l.keyword === atom) return l.name;
  }
  return atom;
}
