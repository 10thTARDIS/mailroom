// SPDX-License-Identifier: AGPL-3.0-only

// THE COMPOSER AS A ROUTE (#373, reshaped in #448): `?compose=new`,
// `reply:<id>`, `forward:<id>`, `draft:<id>`.
//
// It rides on whatever list route is underneath, so composing does not lose the
// list behind it and closing the composer returns to exactly the message you were
// reading. The composer is the one overlay that genuinely belongs in the URL: it
// is a place you can still be in after a reload, and "reply to this" is a thing
// worth linking to.
//
// WHAT IS NOT IN THE URL, and why: the TEXT. `?compose=draft:<id>` names an object
// the SERVER holds, so resuming it is a fetch like opening a message — the id
// travels, the prose does not. A composer whose text exists only locally still has
// no route, and still must not have one: it would either put someone's writing in
// their browser history or lie about restoring it.
//
// WHERE IT RENDERS IS THIS FILE'S OTHER JOB (#448). It used to be a fixed panel in
// the bottom-right corner over whatever you were reading. It now takes the
// reading-pane column, or the main area, or the whole phone screen —
// ui/compose/placement.ts decides which from the device's pane setting and the
// phone breakpoint, and Shell.tsx puts `data-compose="<placement>"` on the app so
// the grid can give the composer a track (app/compose.css). The composer is
// rendered as a CHILD OF THE WORKSPACE GRID for exactly that reason; it is not an
// overlay any more, which is why Shell takes it as its own prop rather than in
// `overlays`.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import { latestRevision } from "../../../core/draftLineage";
import type { Id } from "../../../core/jmap/types";
import { effectiveEmail } from "../../../core/store";
import type { EffectiveEmail } from "../../../core/store";
import type { ComposeRef } from "../../../core/routes";
import { withCompose } from "../../../core/routes";
import { currentRoute, leaveTo, navigate, useListRoute, useRoute } from "../../routing";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import { Compose } from "../Compose";
import type { ComposeIntent } from "../Compose";
import { composerKey } from "../compose/composerKey";
import { shouldFetchSubject } from "../compose/subjectFetch";
import type { SubjectFetchPhase } from "../compose/subjectFetch";
import { composePlacement } from "../compose/placement";
import { useShell } from "../shellContext";
import { PHONE_QUERY, useMediaQuery } from "../useMediaQuery";

/**
 * Every draft a MOUNTED composer has saved, → the key that composer is mounted
 * under (#612; ../compose/composerKey.ts has the why). Module state rather than
 * React state because it must be written BEFORE the navigation that reads it:
 * the render that the URL change triggers has to find the entry already there,
 * or it computes a new key and remounts the composer it was meant to preserve.
 */
const adoptedDrafts = new Map<Id, string>();

export function ComposeRoute(): JSX.Element | null {
  const route = useRoute();
  if (!route.compose) return null;
  // Keyed by the composer the URL names, so replying to a different message is a
  // different composer rather than the previous draft with a new title — except
  // that a draft this composer saved ITSELF keeps the composer's own key.
  const key = composerKey(route.compose, adoptedDrafts);
  return <ComposeForRef key={key} ownKey={key} composeRef={route.compose} />;
}

function ComposeForRef({
  composeRef: routeRef,
  ownKey,
}: {
  composeRef: ComposeRef;
  ownKey: string;
}): JSX.Element | null {
  const state = useMailState();
  const { sync } = useApp();
  const route = useRoute();
  const listRoute = useListRoute();
  const shell = useShell();
  const phone = useMediaQuery(PHONE_QUERY);

  /**
   * The composer's ref, FROZEN at mount (#612).
   *
   * Under one key the route's ref changes for exactly one reason: this composer
   * moved its own URL to the draft it just saved. Everything below must go on
   * treating that as the composer it opened as — a reply stays a reply, whose
   * In-Reply-To and References the NEXT save still has to carry. Read live, the
   * ref would turn a reply into `{ draft: <parent> }` on the first save. Any
   * other ref change comes with a different key, and so with a new composer.
   */
  const [composeRef] = useState(routeRef);

  /** The message this composer is ABOUT: the parent of a reply/forward, or the
   * draft being resumed. One id, whichever it is, so the load below is one path.
   *
   * A resumed draft is followed to its newest revision FIRST: a URL can name a
   * revision an autosave in this tab has since replaced (a link held from before,
   * a history entry), and the draft the user means is the one they last saved.
   * Settled at mount like the ref itself. */
  const [wantId] = useState<Id | null>(() =>
    typeof composeRef === "object"
      ? "reply" in composeRef
        ? composeRef.reply
        : "replyAll" in composeRef
          ? composeRef.replyAll
          : "forward" in composeRef
            ? composeRef.forward
            : latestRevision(state.draftSuccessors, composeRef.draft)
      : null,
  );

  const resolved = wantId ? effectiveEmail(state, wantId) : null;

  /**
   * WHETHER THE MESSAGE IS READY, which is not the same as "is it in the store".
   *
   * A forward quotes the parent and re-attaches its parts; a resumed draft needs
   * its body and its attachments. A REPLY (and reply all) NOW NEEDS IT TOO
   * (#646): `replyRecipients` (ui/compose/recipients.ts) reads the parent's
   * Reply-To, To and Cc to decide who a reply goes to, and none of those travel
   * on the LIST's copy of a message — `EMAIL_LIST_PROPERTIES` (core/jmap/types.ts)
   * carries `from` alone. A reply opened straight from a list row, before this,
   * silently fell back to addressing From only, which is indistinguishable from
   * "there was no Reply-To" even when there was one. All four kinds now come only
   * from an `Email/get` that asked for the body properties, so a message the LIST
   * loaded (no `to`, no `bodyValues`, no `attachments`) is present and not
   * usable — and opening a composer on it would quote nothing, forward nothing,
   * address a reply from a partial guess, and for a draft, autosave an empty body
   * over what the user wrote. `bodyValues` is the marker because it is the one
   * property that is only ever present when the body was fetched.
   */
  const needsBody = typeof composeRef === "object";
  const ready = resolved !== null && (!needsBody || resolved.bodyValues !== undefined);

  // Fetched here rather than left to the list: a deep link to
  // `?compose=draft:<id>` may arrive with nothing loaded at all, and the composer
  // must open on the draft rather than never.
  //
  // ⚠ ON A RELOAD THIS COMPOSER MOUNTS BEFORE THERE IS A SESSION (#612). React
  // runs a child's effects before its parent's, and the session is opened by
  // AppProvider's, so a read issued at mount failed with "not connected" and —
  // asked exactly once — was never repeated: "Opening…" for ever. When to ask is
  // decided by ../compose/subjectFetch.ts, which waits for the session and asks a
  // FAILED read again after the next successful sync.
  const [phase, setPhase] = useState<SubjectFetchPhase>("idle");
  const [failedAtSync, setFailedAtSync] = useState<number | null>(null);
  /** The server said the message does not exist — as opposed to "not here yet",
   * which is what a null `resolved` otherwise means for as long as it lasts. */
  const [missing, setMissing] = useState(false);
  const sessionReady = state.accountId !== "";
  useEffect(() => {
    const go = shouldFetchSubject({
      wantId,
      needsBody,
      hasBody: resolved?.bodyValues !== undefined,
      sessionReady,
      phase,
      failedAtSync,
      lastSyncAt: state.lastSyncAt,
    });
    if (!go || !wantId) return;
    setPhase("inflight");
    const askedAt = state.lastSyncAt;
    void sync.loadEmails([wantId], { body: true }).then((r) => {
      if (r.notFound.includes(wantId)) {
        setMissing(true);
        setPhase("done");
      } else if (r.failed) {
        setFailedAtSync(askedAt);
        setPhase("failed");
      } else {
        setPhase("done");
      }
    });
  }, [wantId, needsBody, resolved, sessionReady, phase, failedAtSync, state.lastSyncAt, sync]);

  /**
   * MOVE THE URL TO THE DRAFT JUST SAVED, WITHOUT REMOUNTING (#612).
   *
   * The adoption is written BEFORE the navigation, which is the whole mechanism:
   * the render the URL change causes computes this composer's key from
   * `adoptedDrafts`, finds it unchanged, and keeps this instance — focus, caret,
   * undo and all.
   *
   * It moves the URL only while this composer is still the one the URL names.
   * The closing flush (Compose's "last save on the way out") lands after the
   * user has left, and must not drag them back into a composer they closed; a
   * save that lands after they opened a different draft must not take its URL.
   * The route is therefore read NOW, not captured when the save began.
   */
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      // ⚠ ADOPTIONS DIE WITH THE COMPOSER THAT MADE THEM. Keys are reused — every
      // blank composer is "new" — so an entry left behind would make the NEXT
      // blank composer keep its instance when the user then opens that draft
      // from the list: the URL would name the draft and the composer would go
      // on showing something else.
      for (const [id, key] of adoptedDrafts) if (key === ownKey) adoptedDrafts.delete(id);
    };
  }, [ownKey]);
  const onDraftSaved = useCallback(
    (draftId: Id) => {
      if (!mounted.current) return;
      const cur = currentRoute();
      if (!cur.compose || composerKey(cur.compose, adoptedDrafts) !== ownKey) return;
      adoptedDrafts.set(draftId, ownKey);
      navigate(withCompose(cur, { draft: draftId }), { replace: true });
    },
    [ownKey],
  );

  /**
   * The message, FROZEN at the moment the composer became able to open.
   *
   * The message keeps changing underneath (read state, labels, a scan verdict), and
   * a composer whose quoted parent silently changed mid-draft would be a different
   * reply than the one that was started. Frozen lazily so a deep link works: on a
   * cold load the message is not in the store yet, and the composer opens as soon
   * as it arrives rather than never.
   */
  const [subject, setSubject] = useState<EffectiveEmail | null>(() => (ready ? resolved : null));
  useEffect(() => {
    if (subject || !ready) return;
    setSubject(resolved);
  }, [subject, ready, resolved]);

  const intent = useMemo<ComposeIntent | null>(() => {
    if (composeRef === "new") return { kind: "new" };
    if (!subject) return null;
    if ("reply" in composeRef) return { kind: "reply", parent: subject };
    if ("replyAll" in composeRef) return { kind: "replyAll", parent: subject };
    if ("forward" in composeRef) return { kind: "forward", parent: subject };
    return { kind: "draft", draft: subject };
  }, [composeRef, subject]);

  const placement = composePlacement({ phone, paneLayout: shell.paneLayout });

  // WHILE THE MESSAGE IS ON ITS WAY, THE COLUMN IS STILL THE COMPOSER'S.
  //
  // Shell puts `data-compose` on the app from the ROUTE, so the grid has already
  // given up a track and hidden the reading pane. Returning null here would leave
  // that track empty for as long as the fetch takes — a blank column beside the
  // list, which reads as a rendering failure. A line saying what is happening is
  // the honest occupant, and it is deliberately NOT a composer: an empty one would
  // invite typing into a draft that is about to be replaced by the loaded one.
  if (!intent) {
    // NOT "Opening…" forever (#612): the server has answered, and a line that
    // still promises the draft is coming would be a lie with no end.
    if (missing) {
      return (
        <section className={`compose compose-${placement}`}>
          <p className="compose-loading">
            {t(
              typeof composeRef === "object" && "forward" in composeRef
                ? "compose.missingForward"
                : "compose.missing",
            )}
          </p>
          <button
            type="button"
            className="button"
            onClick={() => leaveTo(withCompose(route, null))}
          >
            {t("compose.close")}
          </button>
        </section>
      );
    }
    // The waiting line gets a way out too. A wait that never ended once held the
    // user in the composer's layout — the list squeezed to its 320–420px track,
    // the splitter hidden — with Escape the only exit and nothing saying so.
    return (
      <section className={`compose compose-${placement}`} aria-busy="true">
        <p className="compose-loading">{t("compose.loading")}</p>
        <button type="button" className="button" onClick={() => leaveTo(withCompose(route, null))}>
          {t("compose.close")}
        </button>
      </section>
    );
  }
  return (
    <Compose
      intent={intent}
      placement={placement}
      // The account the list beneath is scoped to — what "your primary address"
      // is measured in (ui/compose/from.ts). The LIST route's, not the current
      // one's, for the same reason everything else here reads it: settings sits on
      // top of the list without changing it.
      scope={listRoute.acct ?? null}
      onClose={() => leaveTo(withCompose(route, null))}
      onDraftSaved={onDraftSaved}
    />
  );
}
