// SPDX-License-Identifier: AGPL-3.0-only

// THE COUNT, THE CONFIRM AND "MOVE THE REST", FOR ANY TARGET (#59, #53).
//
// The state machine the sender card's "Move all from … to…" runs (#708), as a
// hook and a panel, so blocking a sender's existing mail and every clean-up
// action ask the same question the same way: count first, state the count,
// confirm in place (never window.confirm), move through the ordinary path with
// one Undo, and say plainly when a cap left mail untouched — then offer the
// rest. The moving itself is ./senderBulk.ts.

import { useState } from "react";
import type { JSX } from "react";

import { JmapMethodError } from "../../core/jmap/client";
import type { Id } from "../../core/jmap/types";
import type { MoveOutcome } from "./moveToArea";
import { bulkText, moveSenderMail, restStart, targetName } from "./senderBulk";
import type { BulkTarget } from "./senderBulk";
import { useApp, useMailState } from "../state";
import { t } from "../i18n";

/** The cap one page of a sender's mail is read in — allMatchingIds' own. */
export const BULK_CAP = 1000;

export type BulkState =
  | { phase: "counting"; target: BulkTarget }
  | { phase: "confirm"; target: BulkTarget; ids: Id[]; total: number; position: number }
  | { phase: "moving"; target: BulkTarget }
  | { phase: "rest"; target: BulkTarget; outcome: MoveOutcome; position: number };

export interface SenderBulk {
  state: BulkState | null;
  /** Count the sender's mail for `target` and ask. */
  start: (target: BulkTarget) => void;
  cancel: () => void;
}

/** Reads one page of the set being moved: at most `cap` ids from `position`,
 * and how many there are in all. */
export type BulkFetch = (position: number, cap: number) => Promise<{ ids: Id[]; total: number }>;

/**
 * `address` names the sender in every sentence. `fetch` reads the set to
 * move: by default the sender card's exact-address `from:` search (#708); the
 * clean-up view passes its own row's enumeration (SenderStats/emailIds), so
 * what moves is exactly what the row counted and no query string is built
 * from a ranking key.
 */
export function useSenderBulk(
  address: string,
  onDone?: (target: BulkTarget, outcome: MoveOutcome) => void,
  fetch?: BulkFetch,
): SenderBulk & {
  confirm: () => void;
  rest: () => void;
} {
  const { store, sync, actions, offerUndo, pushToast } = useApp();
  const [state, setState] = useState<BulkState | null>(null);
  const read: BulkFetch =
    fetch ?? ((position, cap) => sync.allMatchingIds({ kind: "search", query: `from:${address}` }, cap, position));

  const count = (target: BulkTarget, position: number): void => {
    setState({ phase: "counting", target });
    void (async () => {
      try {
        const { ids, total } = await read(position, BULK_CAP);
        if (ids.length === 0) {
          if (position === 0) pushToast({ kind: "info", message: t("areaMove.sender.none", { address }) });
          setState(null);
          return;
        }
        setState({ phase: "confirm", target, ids, total: Math.max(total, position + ids.length), position });
      } catch (e) {
        const noSearch = e instanceof JmapMethodError && e.type === "unsupportedFilter";
        pushToast({
          kind: "error",
          message: t(noSearch ? "areaMove.sender.noSearch" : "areaMove.sender.failed", { address }),
        });
        setState(null);
      }
    })();
  };

  const confirm = (): void => {
    if (state?.phase !== "confirm") return;
    const { target, ids, total, position } = state;
    setState({ phase: "moving", target });
    void (async () => {
      const outcome = await moveSenderMail(
        { store, sync, actions, offerUndo, pushToast },
        ids,
        target,
        total - position,
      );
      if (outcome) onDone?.(target, outcome);
      // Truncated: say so and offer the rest. Never leave a partial move
      // looking finished.
      setState(outcome?.truncated ? { phase: "rest", target, outcome, position } : null);
    })();
  };

  const rest = (): void => {
    if (state?.phase !== "rest") return;
    const { target, outcome, position } = state;
    const next = restStart(target, position, outcome.looked);
    if (next === 0) {
      // Junk and Trash take the moved mail out of the search, so the next page
      // starts at 0 — once the server has applied this page, or it would come
      // back again (senderBulk.ts header).
      setState({ phase: "counting", target });
      void (async () => {
        if (!(await actions.settle())) {
          pushToast({ kind: "error", message: t("senderBulk.notSettled") });
          setState(null);
          return;
        }
        count(target, 0);
      })();
      return;
    }
    count(target, next);
  };

  return { state, start: (target) => count(target, 0), cancel: () => setState(null), confirm, rest };
}

/** The panel: what the hook is doing, in words, with its two buttons. */
export function SenderBulkPanel({
  bulk,
  address,
}: {
  bulk: ReturnType<typeof useSenderBulk>;
  address: string;
}): JSX.Element | null {
  const state = useMailState();
  const s = bulk.state;
  if (!s) return null;
  const name = targetName(s.target, state.areas);
  const fmt = (n: number): string => n.toLocaleString("en-US");
  return (
    <section className="sender-bulk" aria-live="polite">
      {s.phase === "counting" || s.phase === "moving" ? (
        <p>{t("areaMove.sender.finding", { address })}</p>
      ) : null}
      {s.phase === "confirm" ? (
        <>
          <p>
            {s.total - s.position > s.ids.length
              ? t("areaMove.sender.confirm.capped", {
                  address,
                  total: fmt(s.total - s.position),
                  n: fmt(s.ids.length),
                  area: name,
                })
              : s.ids.length === 1
                ? t("areaMove.sender.confirm.one", { address, area: name })
                : t("areaMove.sender.confirm.other", { address, n: fmt(s.ids.length), area: name })}
          </p>
          {s.target.kind === "area" ? <p className="hint">{t("areaMove.sender.learn")}</p> : null}
          <div className="sheet-actions">
            <button type="button" className="button button-quiet" onClick={bulk.cancel}>
              {t("action.cancel")}
            </button>
            <button type="button" className="button button-primary" onClick={bulk.confirm}>
              {t("areaMove.sender.go", { area: name })}
            </button>
          </div>
        </>
      ) : null}
      {s.phase === "rest" ? (
        <>
          <p>{bulkText(s.outcome, name)}</p>
          <div className="sheet-actions">
            <button type="button" className="button button-quiet" onClick={bulk.cancel}>
              {t("action.cancel")}
            </button>
            <button type="button" className="button button-primary" onClick={bulk.rest}>
              {t("areaMove.sender.rest", { n: fmt(s.outcome.total - s.outcome.looked) })}
            </button>
          </div>
        </>
      ) : null}
    </section>
  );
}
