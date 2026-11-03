// SPDX-License-Identifier: AGPL-3.0-only

// WHO THIS IS FROM, AND WHY IT IS HERE (#375 increment 5).
//
// Hovering the sender's name for 400ms opens a popover; clicking pins it. It
// gathers, in one place, the four things a reader actually asks about a sender
// and which the pane previously scattered down its whole length (or did not
// answer at all):
//
//   1. who they are, and which of YOUR addresses they write to;
//   2. why this message ranked where it did (the priority reasons);
//   3. the SEAL — what authentication said about it;
//   4. which area it lands in, and who filed it there;
//   ...and the three things you would then want to do about them.
//
// EVERY LINE IS OMITTED WHEN ITS FACT IS UNKNOWN. Never a label with nothing
// after it: a popover of empty headings is how a reader learns that the
// popover is not worth opening. That is also why there is no "Seal: fine" —
// senderAuthNotice is silent for the 99.4% of mail with nothing wrong, and a
// reassuring badge that common stops being read (core/senderAuth.ts).
//
// TWO THINGS ARE DELIBERATELY NOT HERE:
//
//   * KEY STATUS (PGP/S-MIME). The slot is in the markup below with nothing in
//     it, because this client has no key data at all yet (#47 is unshipped as
//     of v1.69.0). Rendering "no key" would be a claim about the sender rather
//     than about us.
//   * MUTE SENDER. Only THREAD mute exists server-side (Thread.isMuted,
//     urn:mailroom:organize); there is no per-correspondent mute to call, and a
//     button that silently muted one thread while promising to mute a person
//     would be worse than its absence. BLOCK (#59) is the per-person control
//     that does exist, and it is here: new mail from the address goes to Junk
//     or Trash, and the mail already here moves only if the reader ticks the
//     box (then through ../SenderBulk.tsx — the same count, confirm and "Move
//     the rest" as "Move all from … to…").
//
// VIP (#59) is the sender's other marker: `is:vip`, the VIP push modes and the
// quiet-hours exception read it. Both markers live on the CONTACT, which this
// card reads when it opens (client.contactForAddress) — and only then, because
// a thread renders one card per message.
//
// THE CONTACT, BY NAME (#816). When the sender is a SAVED contact, the name at
// the top of the card links to their contact page. When they are not, "Add to
// contacts" saves them, and on success becomes "In your contacts" — the same
// link — so the result of the press is where the button was. Nothing is shown
// while the contact is unread, or from a server without saved contacts.
//
// AND ONE FOLLOW-UP, NAMED RATHER THAN FAKED: the design retires the why-here
// line after a reader has seen a correspondent twice, which needs a
// per-correspondent READ COUNT. `Contact` carries addresses, keys and the
// VIP and block markers (#59, internal/api/contact.go), but no read count,
// so that count does not exist server-side. The pane's own why-here strip
// therefore stays where it is and this shows the same reasons; counting reads
// client-side would be a number about this browser, not about the person.

import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import type { BlockDestination, Contact } from "../../../core/jmap/types";
import { SenderBulkPanel, useSenderBulk } from "../SenderBulk";

import { senderIdentity } from "../../../core/hostileText";
import { senderAuthNotice } from "../../../core/senderAuth";
import { matchingRemoteAllow } from "../../../core/remoteAllows";
import type { EffectiveEmail } from "../../../core/store";
import { areasInDeskOrder, refileChanges } from "../../../core/refile";
import type { Id } from "../../../core/jmap/types";
import { JmapMethodError } from "../../../core/jmap/client";
import { formatRoute } from "../../../core/routes";
import { navigate } from "../../routing";
import { AreaMenu } from "../AreaMenu";
import { moveText, moveToArea } from "../moveToArea";
import type { MoveOutcome } from "../moveToArea";
import { movableSenderAddress } from "../ruleSuggestion";
import { aliasOf } from "../desk/format";
import { draftRuleFrom } from "../ruleDraft";
import { useShell } from "../shellContext";
import { PHONE_QUERY, useMediaQuery } from "../useMediaQuery";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import { areaProvenanceKey, messageLabels } from "./labels";
import { lineSideSentence } from "./lineSide";
import { placePopover } from "./popover";
import type { PopoverPlacement } from "./popover";

/** How long the pointer has to rest on the name before the popover opens.
 * Long enough that crossing the name on the way somewhere else does not open
 * it; short enough that resting there feels like asking. */
const OPEN_DELAY_MS = 400;
/** ...and the grace period on the way out, so the diagonal from the name to
 * the popover's first button does not close it under the cursor. */
const CLOSE_DELAY_MS = 150;

/** How far down the sheet has to be dragged before it closes (#375 increment
 * 7). Far enough that a scroll inside it is not a dismissal, short enough that
 * the gesture is one flick. */
const SHEET_DISMISS_PX = 72;

/** The why-here sentence, or null when the model recorded no reasons. Null is
 * not "no reasons found" — that is a claim the server never made (#176's rule
 * about `reasons === null`), and either way the line is simply absent. */
function whyHereLines(email: EffectiveEmail): string[] {
  const reasons = email.priorityReasons ?? [];
  if (reasons.length === 0) return [];
  const up = reasons.filter((r) => r.polarity === "up").map((r) => r.label);
  const down = reasons.filter((r) => r.polarity === "down").map((r) => r.label);
  const out: string[] = [];
  if (up.length > 0) out.push(t("popover.whyHere.up", { reasons: up.join(", ") }));
  if (down.length > 0) out.push(t("popover.whyHere.down", { reasons: down.join(", ") }));
  return out;
}

/**
 * TWO ROLES, ONE COMPONENT (#447).
 *
 * Hovered and not pinned, this is a DESCRIPTION of the sender's name: transient,
 * closed by the pointer leaving, and never something a keyboard user is inside —
 * so it is a `role="tooltip"` and the anchor points its `aria-describedby` at it,
 * which is how a screen reader gets the content at all (nobody "hovers" with a
 * screen reader). Pinned — which is what a click, Enter or a tap does — it is a
 * `role="dialog"`: it stays until dismissed, its buttons are the point, and
 * Escape closes it and puts focus back on the name.
 *
 * ⚠ THE UNPINNED FORM CONTAINS BUTTONS, which a tooltip strictly should not, and
 * that is a deliberate acceptance rather than an oversight. The buttons are there
 * for the pointer user who slides from the name into the popover (the reason
 * CLOSE_DELAY_MS exists at all); every keyboard or touch route into this thing
 * PINS it first, so the only reader who meets it as a tooltip meets it as a
 * description, which is exactly what `aria-describedby` promises.
 */
export function SenderPopover({ email }: { email: EffectiveEmail }): JSX.Element {
  const state = useMailState();
  const { actions, sync, store, offerUndo, pushToast } = useApp();
  const shell = useShell();

  /**
   * The phone form (#375 increment 7): the same content, as a BOTTOM SHEET.
   *
   * ⚠ THE PLACEMENT ARITHMETIC MUST NOT RUN HERE. `placePopover` writes inline
   * `top`/`left` (ui/pane/popover.ts), and inline styles beat the stylesheet —
   * so a sheet whose position CSS owns would be dragged back to a coordinate
   * computed for a floating popover, which is the "renders perfectly and is
   * simply unreadable" failure that module exists to prevent, reintroduced from
   * the other side.
   *
   * There is no hover on a phone either, so the whole open-on-rest machinery is
   * bypassed: a tap opens it, pinned, and only the ✕, Escape or a drag down
   * closes it.
   */
  const sheet = useMediaQuery(PHONE_QUERY);

  const [open, setOpen] = useState(false);
  const [pinned, setPinned] = useState(false);
  const [areaMenu, setAreaMenu] = useState(false);
  const [at, setAt] = useState<PopoverPlacement | null>(null);

  const anchorRef = useRef<HTMLButtonElement | null>(null);
  const popRef = useRef<HTMLDivElement | null>(null);
  const openTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const closeTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const from = senderIdentity(email.from?.[0]);
  const sender = from.name || from.address || t("list.noSender");

  /** Per MESSAGE, because the reading pane renders one of these per message in
   * the thread and two elements sharing an id would make `aria-describedby`
   * resolve to whichever came first. */
  const popId = `sender-popover-${email.id}`;

  const clearTimers = useCallback(() => {
    if (openTimer.current !== null) clearTimeout(openTimer.current);
    if (closeTimer.current !== null) clearTimeout(closeTimer.current);
    openTimer.current = null;
    closeTimer.current = null;
  }, []);

  const close = useCallback(
    (restoreFocus: boolean) => {
      clearTimers();
      setOpen(false);
      setPinned(false);
      setAreaMenu(false);
      if (restoreFocus) anchorRef.current?.focus();
    },
    [clearTimers],
  );

  useEffect(() => clearTimers, [clearTimers]);

  // Placed AFTER it has rendered, because placing it needs its measured size —
  // a popover positioned from a guessed height is one that flips to the wrong
  // side exactly when the window is short. Layout effect, so the first painted
  // frame is already in the right place rather than jumping.
  useLayoutEffect(() => {
    if (!open || sheet) return;
    const anchor = anchorRef.current?.getBoundingClientRect();
    const pop = popRef.current?.getBoundingClientRect();
    if (!anchor || !pop) return;
    setAt(
      placePopover(
        { top: anchor.top, left: anchor.left, width: anchor.width, height: anchor.height },
        { width: pop.width, height: pop.height },
        { width: window.innerWidth, height: window.innerHeight },
      ),
    );
  }, [open, pinned, areaMenu, sheet]);

  // ESCAPE, IN THE CAPTURE PHASE, AND THAT IS THE POINT. The shell owns one
  // window-level keydown chain (ui/Shell.tsx) whose Escape peels a ROUTE —
  // closing the open message. A bubble-phase listener here would run after it,
  // so Escape with the popover open would close the message and leave the
  // popover floating over the next one. Capture on `window` runs first, and
  // stopping the event there is what keeps one keystroke to one layer.
  useEffect(() => {
    if (!open) return;
    const onKey = (ev: KeyboardEvent): void => {
      if (ev.key !== "Escape") return;
      ev.stopPropagation();
      close(true);
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [open, close]);

  // Click outside closes a PINNED popover. An unpinned one is closed by the
  // pointer leaving, so this would only ever fight with that.
  useEffect(() => {
    if (!pinned) return;
    const onDown = (ev: PointerEvent): void => {
      const target = ev.target as Node;
      if (popRef.current?.contains(target) || anchorRef.current?.contains(target)) return;
      close(false);
    };
    document.addEventListener("pointerdown", onDown);
    return () => document.removeEventListener("pointerdown", onDown);
  }, [pinned, close]);

  const onEnter = (): void => {
    // A phone's "pointerenter" arrives with the tap that is about to fire the
    // click below; opening on it would make the 400ms rest timer race the tap.
    if (sheet) return;
    if (closeTimer.current !== null) clearTimeout(closeTimer.current);
    closeTimer.current = null;
    if (open) return;
    openTimer.current = setTimeout(() => setOpen(true), OPEN_DELAY_MS);
  };

  const onLeave = (): void => {
    if (sheet) return;
    if (openTimer.current !== null) clearTimeout(openTimer.current);
    openTimer.current = null;
    if (pinned) return;
    closeTimer.current = setTimeout(() => setOpen(false), CLOSE_DELAY_MS);
  };

  /** DRAG DOWN TO DISMISS. Pointer events, as the row's swipe is
   * (ui/desk/desk swipe.ts) — one press, one release, and the distance between
   * them. It is a SECOND way out, never the only one: the ✕ and Escape are
   * both still there, because a gesture is not an accessible path. */
  const dragFrom = useRef<number | null>(null);

  /**
   * Pin it — which is what a click, Enter or a tap does, and what turns it from a
   * description into a dialog.
   *
   * FOCUS IS DELIBERATELY NOT MOVED INTO IT. This is a NON-MODAL dialog: the
   * anchor is the thing the user just activated and it keeps focus, the popover is
   * rendered immediately after the anchor in DOM order so one Tab reaches its first
   * control, and Escape closes it and returns focus (`close(true)`). Pulling focus
   * inside on open would mean a reader who pinned it by accident has to Escape to
   * get back to the message they were reading, and it would trap nothing — nothing
   * here is modal.
   */
  const pin = (): void => {
    clearTimers();
    setOpen(true);
    setPinned(true);
  };

  const alias = aliasOf(email.deliveredTo);
  const why = whyHereLines(email);
  const lineSideKey = lineSideSentence(email.lineSide);
  const notice = senderAuthNotice(email.authResults);
  const { area } = useMemo(
    () => messageLabels(email, state.labels, state.areas),
    [email, state.labels, state.areas],
  );
  const areaName = area?.area?.name ?? null;
  const allows = useMemo(() => Object.values(state.remoteAllows), [state.remoteAllows]);
  const allowedBy = matchingRemoteAllow(from.address, allows);

  /** The areas this message can be re-filed into — the same rule the selection
   * toolbar follows: an area OWNS a label, so filing IS applying that label,
   * local-only or not (#704). Only an unloaded label blocks a target. */
  const areaTargets = useMemo(
    () =>
      Object.values(state.areas)
        .sort((a, b) => a.position - b.position)
        .map((a) => ({
          id: a.id,
          name: a.name,
          loaded: state.labels[a.labelId] !== undefined,
        })),
    [state.areas, state.labels],
  );

  /** Placing the message in an area is the same label SWAP the selection
   * toolbar makes (core/refile.ts), on one message: the other areas' own
   * labels off, this one's on — or the earlier-positioned area keeps it. */
  const refile = (areaId: string): void => {
    void actions.relabel(refileChanges(state, [email.id], areaId), "Area changed");
    setAreaMenu(false);
  };

  // -- MOVE EVERYTHING FROM THIS ADDRESS (#708) ---------------------------------
  //
  // Ask the server for every non-trashed message from this EXACT address (the
  // `from:` search operator is an exact-address term, internal/search/parse.go,
  // and a search never returns trashed mail), show the count and confirm in the
  // popover itself -- never window.confirm -- then re-file all of them through
  // the same path as every other re-file, with one exact Undo.
  type SenderMove =
    | { phase: "counting"; areaId: Id }
    | { phase: "confirm"; areaId: Id; ids: Id[]; total: number }
    | { phase: "moving"; areaId: Id }
    | { phase: "rest"; areaId: Id; outcome: MoveOutcome; position: number };
  const [senderMove, setSenderMove] = useState<SenderMove | null>(null);
  /** An address the operator syntax can carry unquoted. Anything else gets no
   * button rather than a query that means something other than "this address". */
  // A From with no local part ("example.com", "@example.com") would be read by
  // `from:` as a whole DOMAIN (#712); only a well-formed address is movable
  // (#53 security review).
  const movable = movableSenderAddress(from.address);
  const senderView = useMemo(
    () => ({ kind: "search" as const, query: `from:${from.address}` }),
    [from.address],
  );

  const startSenderMove = (areaId: Id): void => {
    setSenderMove({ phase: "counting", areaId });
    void (async () => {
      try {
        const { ids, total } = await sync.allMatchingIds(senderView);
        if (ids.length === 0) {
          pushToast({ kind: "info", message: t("areaMove.sender.none", { address: from.address }) });
          setSenderMove(null);
          return;
        }
        setSenderMove({ phase: "confirm", areaId, ids, total });
      } catch (e) {
        const noSearch = e instanceof JmapMethodError && e.type === "unsupportedFilter";
        pushToast({
          kind: "error",
          message: t(noSearch ? "areaMove.sender.noSearch" : "areaMove.sender.failed", { address: from.address }),
        });
        setSenderMove(null);
      }
    })();
  };

  const runSenderMove = (areaId: Id, ids: Id[], total: number, position: number): void => {
    setSenderMove({ phase: "moving", areaId });
    void (async () => {
      const outcome = await moveToArea(
        { store, sync, actions, offerUndo, pushToast },
        ids,
        areaId,
        total - position,
      );
      // Truncated: say so and offer the rest, from where this stopped. Never
      // leave a partial move looking finished.
      setSenderMove(
        outcome?.truncated
          ? { phase: "rest", areaId, outcome, position: position + outcome.looked }
          : null,
      );
    })();
  };

  const moveTheRest = (m: Extract<SenderMove, { phase: "rest" }>): void => {
    setSenderMove({ phase: "counting", areaId: m.areaId });
    void (async () => {
      try {
        const { ids, total } = await sync.allMatchingIds(senderView, 1000, m.position);
        if (ids.length === 0) {
          setSenderMove(null);
          return;
        }
        runSenderMove(m.areaId, ids, Math.max(total, m.position + ids.length), m.position);
      } catch {
        pushToast({ kind: "error", message: t("areaMove.sender.failed", { address: from.address }) });
        setSenderMove(null);
      }
    })();
  };

  const senderAreaName = (id: Id): string => state.areas[id]?.name ?? "";

  // -- VIP AND BLOCK (#59) ------------------------------------------------------
  const { client } = useApp();
  /** undefined = not asked yet or could not ask; null = no contact for this
   * address; otherwise the contact and its markers. */
  const [contact, setContact] = useState<Contact | null | undefined>(undefined);
  const [blockForm, setBlockForm] = useState<{ dest: BlockDestination; moveExisting: boolean } | null>(null);
  const [markBusy, setMarkBusy] = useState(false);
  const bulk = useSenderBulk(from.address);
  useEffect(() => {
    if (!open || !from.address || !client) return;
    let cancelled = false;
    void client.contactForAddress(from.address).then((c) => {
      if (!cancelled) setContact(c);
    });
    return () => {
      cancelled = true;
    };
  }, [open, from.address, client]);
  const markers = contact && contact.isVip !== undefined ? contact : null;
  /** Saved-contact state (#816): null until read, or from a server without it. */
  const saved = contact && typeof contact.isSaved === "boolean" ? contact : null;
  const [saving, setSaving] = useState(false);
  const contactRoute = saved ? { page: "contacts" as const, id: saved.id } : null;
  const openContact = (ev: React.MouseEvent): void => {
    if (!contactRoute || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    ev.preventDefault();
    close(false);
    navigate(contactRoute);
  };
  const addToContacts = (): void => {
    if (!saved) return;
    setSaving(true);
    void actions.setContactMarkers(saved.id, { isSaved: true }, t("contacts.popover.failed")).then((ok) => {
      setSaving(false);
      if (ok) setContact({ ...saved, isSaved: true });
    });
  };

  const setMarkers = (
    patch: { isVip?: boolean; isBlocked?: boolean; blockDestination?: BlockDestination },
    failed: string,
  ): Promise<boolean> => {
    if (!markers) return Promise.resolve(false);
    setMarkBusy(true);
    return actions.setContactMarkers(markers.id, patch, failed).then((ok) => {
      setMarkBusy(false);
      if (ok) setContact({ ...markers, ...patch, ...(patch.isBlocked === false ? { blockedAt: null } : {}) });
      return ok;
    });
  };

  const toggleVip = (): void => {
    if (!markers) return;
    const next = !markers.isVip;
    void setMarkers({ isVip: next }, t("senders.vip.failed"));
  };

  const block = (): void => {
    if (!markers || !blockForm) return;
    const { dest, moveExisting } = blockForm;
    void (async () => {
      const ok = await setMarkers({ isBlocked: true, blockDestination: dest }, t("senders.block.failed"));
      if (!ok) return;
      setBlockForm(null);
      const message = t(dest === "junk" ? "senders.blocked.junk" : "senders.blocked.trash", { address: from.address });
      offerUndo(message, () => setMarkers({ isBlocked: false }, t("senders.unblock.failed")));
      // The mail already here moves ONLY when asked, through the count and
      // the confirm — never as a side effect of the block itself.
      // Only for a well-formed address: the move is a `from:` search, which
      // reads a domain-shaped From as a whole domain (`movable`).
      if (moveExisting && movable) bulk.start({ kind: dest });
    })();
  };

  const unblock = (): void => {
    void setMarkers({ isBlocked: false }, t("senders.unblock.failed")).then((ok) => {
      if (ok) pushToast({ kind: "info", message: t("senders.unblocked", { address: from.address }) });
    });
  };

  // A closed popover forgets a half-finished move: a confirm left behind would
  // be answered later, out of context, by whoever reopens it.
  useEffect(() => {
    if (!open) {
      setSenderMove(null);
      setBlockForm(null);
      bulk.cancel();
    }
    // bulk.cancel is a fresh closure each render; the effect is about `open`.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  return (
    <>
      <button
        ref={anchorRef}
        type="button"
        className="message-from message-from-button"
        aria-haspopup="dialog"
        aria-expanded={open}
        // Only while it is a TOOLTIP: a description is what an unpinned popover
        // is, and once it is pinned it is a dialog the reader is expected to move
        // INTO — describing the anchor with it as well would read the whole panel
        // out again every time focus returned to the name.
        {...(open && !pinned ? { "aria-describedby": popId } : {})}
        title={t("popover.aboutSender", { sender })}
        onPointerEnter={onEnter}
        onPointerLeave={onLeave}
        // No onFocus. Escape closes the popover and puts focus BACK on this
        // button, and a focus-opens rule would then re-open it 400ms later —
        // an Escape that undoes itself. The keyboard route in is Enter, which
        // is this button's own click.
        onClick={pin}
      >
        {sender}
      </button>

      {open ? (
        <div
          ref={popRef}
          className={
            sheet ? "sender-popover is-sheet" : `sender-popover${at ? ` is-${at.side}` : ""}`
          }
          id={popId}
          // See the component's header: a description while it is hovered, a
          // dialog once it is pinned.
          role={pinned ? "dialog" : "tooltip"}
          // A dialog needs a name; a tooltip must not have one that competes with
          // the text it consists of (the anchor is what is being described, and it
          // is already called the sender's name).
          {...(pinned ? { "aria-label": sender } : {})}
          // The sheet is placed by the stylesheet; writing coordinates here
          // would override it (see `sheet`'s comment above). Hidden until the
          // arithmetic has run only in the floating form, where there IS
          // arithmetic to wait for.
          {...(sheet
            ? {}
            : { style: at ? { top: `${at.top}px`, left: `${at.left}px` } : { visibility: "hidden" } })}
          onPointerEnter={onEnter}
          onPointerLeave={onLeave}
          {...(sheet
            ? {
                onPointerDown: (ev: React.PointerEvent<HTMLDivElement>) => {
                  dragFrom.current = ev.clientY;
                },
                onPointerUp: (ev: React.PointerEvent<HTMLDivElement>) => {
                  const from = dragFrom.current;
                  dragFrom.current = null;
                  if (from !== null && ev.clientY - from >= SHEET_DISMISS_PX) close(false);
                },
                onPointerCancel: () => {
                  dragFrom.current = null;
                },
              }
            : {})}
        >
          {/* The drag handle: a phone convention that says "this sheet can be
              pulled down", and decorative because the gesture is not the
              accessible path — the ✕ beside it and Escape are. */}
          {sheet ? <span className="sender-popover-grip" aria-hidden="true" /> : null}
          <div className="sender-popover-head">
            {saved?.isSaved && contactRoute ? (
              <a className="sender-popover-name sender-popover-contact" href={formatRoute(contactRoute)} onClick={openContact}>
                {sender}
              </a>
            ) : (
              <span className="sender-popover-name">{sender}</span>
            )}
            {from.address && from.address !== sender ? (
              <span className="sender-popover-address">{from.address}</span>
            ) : null}
            {alias ? (
              <span className="sender-popover-alias">{t("popover.writesTo", { alias })}</span>
            ) : null}
            <button
              type="button"
              className="icon-button sender-popover-pin"
              aria-pressed={pinned}
              aria-label={pinned ? t("popover.unpin") : t("popover.pin")}
              onClick={() => (pinned ? close(true) : pin())}
            >
              {pinned ? "✕" : "📌"}
            </button>
          </div>

          {saved && contactRoute ? (
            <p className="sender-popover-saved">
              {saved.isSaved ? (
                <a href={formatRoute(contactRoute)} onClick={openContact}>
                  {t("contacts.popover.in")}
                </a>
              ) : (
                <button
                  type="button"
                  className="button button-quiet sender-popover-add"
                  disabled={saving}
                  onClick={addToContacts}
                >
                  {saving ? t("contacts.popover.adding") : t("contacts.popover.add")}
                </button>
              )}
            </p>
          ) : null}

          {why.length > 0 || lineSideKey ? (
            <section className="sender-popover-section">
              <h3>{t("popover.whyHere")}</h3>
              {why.map((line) => (
                <p key={line}>{line}</p>
              ))}
              {lineSideKey ? <p>{t(lineSideKey)}</p> : null}
            </section>
          ) : null}

          {notice ? (
            <section className="sender-popover-section">
              <h3>{t("popover.seal")}</h3>
              <p className={`auth-notice auth-notice-${notice.severity}`}>
                {t(`thread.auth.${notice.severity}`, {
                  sender,
                  methods: notice.methods.map((m) => t(`thread.auth.method.${m}`)).join(", "),
                })}
              </p>
              {/* KEY STATUS GOES HERE (#47). Left empty on purpose — this
                  client holds no key data at all as of v1.69.0, and an
                  "unsigned" line would be a claim about the sender that we have
                  not checked. */}
            </section>
          ) : null}

          {markers?.isBlocked ? (
            <section className="sender-popover-section">
              <p>
                {t(markers.blockDestination === "trash" ? "senders.isBlocked.trash" : "senders.isBlocked.junk")}
              </p>
            </section>
          ) : null}

          {areaName ? (
            <section className="sender-popover-section">
              <h3>{t("popover.area")}</h3>
              <p>
                {t("popover.areaLine", {
                  area: areaName,
                  how: t(areaProvenanceKey(area?.provenance ?? null)),
                })}
              </p>
            </section>
          ) : null}

          <div className="sender-popover-actions">
            {from.address ? (
              allowedBy ? (
                <button
                  type="button"
                  className="button button-quiet"
                  onClick={() => void actions.revokeRemoteContent(allowedBy.id)}
                >
                  {t("popover.stopShowingImages")}
                </button>
              ) : (
                <button
                  type="button"
                  className="button button-quiet"
                  onClick={() => void actions.allowRemoteContent("address", from.address)}
                >
                  {t("popover.alwaysShowImages", { address: from.address })}
                </button>
              )
            ) : null}

            <span className="sender-popover-refile">
              <button
                type="button"
                className="button button-quiet"
                aria-expanded={areaMenu}
                aria-haspopup="menu"
                onClick={() => setAreaMenu((v) => !v)}
              >
                {t("popover.changeArea")} ▾
              </button>
              {areaMenu ? (
                <div className="desk-refile-menu" role="menu" aria-label={t("desk.refile.label")}>
                  {areaTargets.length === 0 ? (
                    <p className="desk-refile-none">{t("desk.refile.noAreas")}</p>
                  ) : null}
                  {areaTargets.map((a) => (
                    <button
                      key={a.id}
                      type="button"
                      role="menuitem"
                      className="desk-refile-item"
                      disabled={!a.loaded}
                      title={
                        !a.loaded ? t("desk.refile.labelMissing", { area: a.name }) : undefined
                      }
                      onClick={() => refile(a.id)}
                    >
                      {a.name}
                    </button>
                  ))}
                </div>
              ) : null}
            </span>

            {movable ? (
              <AreaMenu
                buttonText={t("areaMove.sender.button", { address: from.address })}
                menuLabel={t("areaMove.sender.menuLabel", { address: from.address })}
                areas={areasInDeskOrder(state.areas).map((a) => ({
                  id: a.id,
                  name: a.name,
                  blocked: state.labels[a.labelId]
                    ? null
                    : t("desk.refile.labelMissing", { area: a.name }),
                }))}
                onPick={startSenderMove}
                disabled={senderMove !== null}
              />
            ) : null}

            <button
              type="button"
              className="button button-quiet"
              onClick={() => {
                close(false);
                shell.openRuleDraft(draftRuleFrom(email));
              }}
            >
              {t("rules.fromMessage")}
            </button>

            {/* VIP and block (#59). Absent until the contact has been read —
                a toggle that guessed "not a VIP" would be a claim nobody made. */}
            {markers ? (
              <>
                <button
                  type="button"
                  className="button button-quiet"
                  aria-pressed={markers.isVip === true}
                  disabled={markBusy}
                  onClick={toggleVip}
                >
                  {t("senders.vip.toggle")}
                </button>
                {markers.isBlocked ? (
                  <button type="button" className="button button-quiet" disabled={markBusy} onClick={unblock}>
                    {t("senders.unblock", { address: from.address })}
                  </button>
                ) : (
                  <button
                    type="button"
                    className="button button-quiet"
                    aria-expanded={blockForm !== null}
                    disabled={markBusy || bulk.state !== null}
                    onClick={() =>
                      setBlockForm((f) =>
                        f ? null : { dest: markers.blockDestination ?? "junk", moveExisting: false },
                      )
                    }
                  >
                    {t("senders.block.open", { address: from.address })}
                  </button>
                )}
              </>
            ) : null}
          </div>

          {/* THE BLOCK FORM, IN THE POPOVER: where new mail goes (Junk unless
              changed), and whether the mail already here moves too — off
              unless ticked. */}
          {blockForm && markers ? (
            <form
              className="sender-popover-section sender-block-form"
              aria-label={t("senders.block.formLabel", { address: from.address })}
              onSubmit={(ev) => {
                ev.preventDefault();
                block();
              }}
            >
              <fieldset>
                <legend>{t("senders.block.where", { address: from.address })}</legend>
                {(["junk", "trash"] as const).map((d) => (
                  <label key={d} className="radio">
                    <input
                      type="radio"
                      name={`block-dest-${email.id}`}
                      value={d}
                      checked={blockForm.dest === d}
                      onChange={() => setBlockForm({ ...blockForm, dest: d })}
                    />
                    {t(d === "junk" ? "senders.block.toJunk" : "senders.block.toTrash")}
                  </label>
                ))}
              </fieldset>
              {movable ? (
                <label className="checkbox">
                  <input
                    type="checkbox"
                    checked={blockForm.moveExisting}
                    onChange={(ev) => setBlockForm({ ...blockForm, moveExisting: ev.target.checked })}
                  />
                  {t("senders.block.moveExisting", { address: from.address })}
                </label>
              ) : null}
              <p className="hint">{t("senders.block.reversible")}</p>
              <div className="sheet-actions">
                <button type="button" className="button button-quiet" onClick={() => setBlockForm(null)}>
                  {t("action.cancel")}
                </button>
                <button type="submit" className="button button-primary" disabled={markBusy}>
                  {t("senders.block.go")}
                </button>
              </div>
            </form>
          ) : null}

          <SenderBulkPanel bulk={bulk} address={from.address} />

          {/* THE CONFIRM STEP, IN THE POPOVER (never window.confirm): the count
              is the consequence, so it is stated before anything moves. */}
          {senderMove ? (
            <section className="sender-popover-section" aria-live="polite">
              {senderMove.phase === "counting" || senderMove.phase === "moving" ? (
                <p>{t("areaMove.sender.finding", { address: from.address })}</p>
              ) : null}
              {senderMove.phase === "confirm" ? (
                <>
                  <p>
                    {senderMove.total > senderMove.ids.length
                      ? t("areaMove.sender.confirm.capped", {
                          address: from.address,
                          total: senderMove.total.toLocaleString("en-US"),
                          n: senderMove.ids.length.toLocaleString("en-US"),
                          area: senderAreaName(senderMove.areaId),
                        })
                      : senderMove.ids.length === 1
                        ? t("areaMove.sender.confirm.one", {
                            address: from.address,
                            area: senderAreaName(senderMove.areaId),
                          })
                        : t("areaMove.sender.confirm.other", {
                            address: from.address,
                            n: senderMove.ids.length.toLocaleString("en-US"),
                            area: senderAreaName(senderMove.areaId),
                          })}
                  </p>
                  <p className="hint">{t("areaMove.sender.learn")}</p>
                  <div className="sheet-actions">
                    <button
                      type="button"
                      className="button button-quiet"
                      onClick={() => setSenderMove(null)}
                    >
                      {t("action.cancel")}
                    </button>
                    <button
                      type="button"
                      className="button button-primary"
                      onClick={() =>
                        runSenderMove(senderMove.areaId, senderMove.ids, senderMove.total, 0)
                      }
                    >
                      {t("areaMove.sender.go", { area: senderAreaName(senderMove.areaId) })}
                    </button>
                  </div>
                </>
              ) : null}
              {senderMove.phase === "rest" ? (
                <>
                  <p>{moveText(senderMove.outcome, senderAreaName(senderMove.areaId))}</p>
                  <div className="sheet-actions">
                    <button
                      type="button"
                      className="button button-quiet"
                      onClick={() => setSenderMove(null)}
                    >
                      {t("action.cancel")}
                    </button>
                    <button
                      type="button"
                      className="button button-primary"
                      onClick={() => moveTheRest(senderMove)}
                    >
                      {t("areaMove.sender.rest", {
                        n: (senderMove.outcome.total - senderMove.outcome.looked).toLocaleString("en-US"),
                      })}
                    </button>
                  </div>
                </>
              ) : null}
            </section>
          ) : null}
        </div>
      ) : null}
    </>
  );
}
