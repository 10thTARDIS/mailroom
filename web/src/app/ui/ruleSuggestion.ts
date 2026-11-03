// SPDX-License-Identifier: AGPL-3.0-only

// "ALWAYS PUT MAIL FROM {ADDRESS} IN {AREA}?" -- a rule offered after the user
// files mail from one sender into an area BY HAND.
//
// THE SERVER DECIDES (Area/ruleSuggestion, internal/store/area_rule_suggestion.go):
// whether the user has done this often enough, whether learned placement is
// about to do it for them anyway, whether a rule already does, whether they said
// "Not now" recently. This file only asks, at the right moment, and carries out
// the two answers.
//
// WHICH MOVES ASK. Only a manual move of messages that all come from ONE
// well-formed sender. A rule-applied or learned move never reaches here (they are
// not the user's gesture), and the sender card's "Move all from" is already a bulk
// sender action, so it does not call this.
//
// FAILURE IS SILENCE. The user asked to move mail, not for a suggestion: any
// error, an unsettled write queue, or an answer of "no" shows nothing at all.
//
// NOTHING MOVES WITHOUT A SECOND TAP. "Make a rule" makes the rule, which acts on
// FUTURE mail; the earlier messages move only on "Move them", through moveToArea
// and its own Undo.

import type { MailActions } from "../../core/actions";
import type { AreaRuleSuggestion } from "../../core/jmap/types";
import type { Id } from "../../core/jmap/types";
import { senderIdentity } from "../../core/hostileText";
import { effectiveEmail } from "../../core/store";
import type { MoveDeps } from "./moveToArea";
import { moveToArea } from "./moveToArea";
import type { SyncEngine } from "../../core/sync";
import { t } from "../i18n";
import { looksLikeAddress } from "./compose/chips";
import { UNDO_TTL_MS } from "../../core/undo";

/**
 * An address the `from:` operator can carry unquoted. A From with no local part
 * ("example.com", "@example.com") would be read by `from:` as a whole DOMAIN
 * (#712), so only a well-formed address is movable (#53 security review). The
 * sender card uses this same test for its own buttons.
 */
export function movableSenderAddress(address: string): boolean {
  return address !== "" && !/[\s"]/.test(address) && looksLikeAddress(address) && !address.startsWith("*");
}

export interface RuleSuggestionDeps extends MoveDeps {
  sync: Pick<SyncEngine, "loadEmails" | "allMatchingIds">;
  actions: Pick<MailActions, "relabel" | "ruleSuggestion" | "dismissRuleSuggestion" | "createRule">;
}

/** The one sender every message in `ids` is from, or null: none readable, a
 * message whose From is not exactly one mailbox, two different senders, or an
 * address that is not movable. Compared case-insensitively. */
export function commonMovableSender(state: Parameters<typeof effectiveEmail>[0], ids: Id[]): string | null {
  let found: string | null = null;
  for (const id of ids) {
    const e = effectiveEmail(state, id);
    if (!e || (e.from?.length ?? 0) !== 1) return null;
    const address = senderIdentity(e.from?.[0]).address;
    if (!movableSenderAddress(address)) return null;
    if (found === null) found = address;
    else if (found.toLowerCase() !== address.toLowerCase()) return null;
  }
  return found;
}

/**
 * Call after a successful MANUAL move of `ids` into `areaId` (`moved` is how many
 * messages actually changed). Fire-and-forget: it returns at once and never
 * throws.
 *
 * It leaves the move's own Undo toast alone: the offer is a second, separate
 * toast, and both may be on screen.
 */
/** How long an offer stays up unanswered: as long as the move's own Undo. */
export const RULE_OFFER_TTL_MS = UNDO_TTL_MS;

/**
 * Resolves true when the offer was shown, so a caller with an older prompt of
 * its own (LabelMenu's rule induction) can stand down instead of showing both.
 */
export async function offerRuleSuggestion(deps: RuleSuggestionDeps, ids: Id[], areaId: Id, moved: number): Promise<boolean> {
  if (moved <= 0 || ids.length === 0) return false;
  const address = commonMovableSender(deps.store.getState(), ids);
  if (address === null) return false;
  try {
    const answer = await deps.actions.ruleSuggestion(areaId, address);
    if (!answer || !answer.suggest) return false;
    const area = deps.store.getState().areas[areaId];
    if (!area) return false;
    deps.pushToast({
      ttlMs: RULE_OFFER_TTL_MS,
      kind: "info",
      message: t("ruleSuggestion.offer", { address, area: area.name }),
      action: { label: t("ruleSuggestion.make"), run: () => void makeRule(deps, areaId, address, answer) },
      secondary: {
        label: t("ruleSuggestion.notNow"),
        run: () => void deps.actions.dismissRuleSuggestion(areaId, address),
      },
    });
    return true;
  } catch {
    // No suggestion is the safe failure: this is an offer, never an error.
    return false;
  }
}

/** "Make a rule": exactly the rule the rule editor produces for "move to area" --
 * from equals the address, the area's own label applied -- then, when older mail
 * from the sender is elsewhere, the offer to move it too. */
async function makeRule(deps: RuleSuggestionDeps, areaId: Id, address: string, answer: AreaRuleSuggestion): Promise<boolean> {
  const area = deps.store.getState().areas[areaId];
  if (!area) return false;
  const rule = await deps.actions.createRule({
    name: t("ruleSuggestion.ruleName", { address, area: area.name }),
    priority: 100,
    conditions: { all: [{ field: "from", op: "equals", value: address }] },
    actions: [{ type: "addLabel", label: area.labelId }],
    enabled: true,
  });
  // createRule has already said why, in an error toast.
  if (!rule) return false;
  if (answer.earlier > 0) {
    deps.pushToast({
      kind: "info",
      ttlMs: RULE_OFFER_TTL_MS,
      message: t("ruleSuggestion.madeEarlier", { earlier: String(answer.earlier), address, area: area.name }),
      action: { label: t("ruleSuggestion.moveThem"), run: () => void moveEarlier(deps, areaId, address) },
    });
  } else {
    deps.pushToast({ kind: "info", message: t("ruleSuggestion.made") });
  }
  return true;
}

/**
 * "Make a rule" from Settings' list of senders learned placement is not
 * learning: the SAME flow as the offer's toast (the rule, then "Move them" for
 * earlier mail). The count of earlier mail comes from a fresh Area/ruleSuggestion
 * answer; if that cannot be had the rule is still made, without the offer to
 * move earlier mail. Resolves true when the rule was made.
 */
export async function makeRuleForSender(deps: RuleSuggestionDeps, areaId: Id, address: string): Promise<boolean> {
  let answer: AreaRuleSuggestion | null = null;
  try {
    answer = await deps.actions.ruleSuggestion(areaId, address);
  } catch {
    answer = null;
  }
  return makeRule(deps, areaId, address, answer ?? { suggest: true, reason: "", address, areaId, filings: 0, earlier: 0 });
}

/** "Move them": every message from the address, minus those already carrying the
 * area's label, through the one move path (one Undo, per-case counts). */
async function moveEarlier(deps: RuleSuggestionDeps, areaId: Id, address: string): Promise<void> {
  const area = deps.store.getState().areas[areaId];
  if (!area) return;
  let found: { ids: Id[]; total: number };
  try {
    found = await deps.sync.allMatchingIds({ kind: "search", query: `from:${address}` });
  } catch {
    deps.pushToast({ kind: "error", message: t("ruleSuggestion.earlierFailed", { address }) });
    return;
  }
  // Drop what is cheaply known to be there already; anything not loaded is left
  // for moveToArea to read and report as "already there".
  const emails = deps.store.getState().emails;
  const todo = found.ids.filter((id) => emails[id]?.labelIds?.[area.labelId] !== true);
  const dropped = found.ids.length - todo.length;
  if (todo.length === 0) {
    await moveToArea(deps, found.ids, areaId, found.total);
    return;
  }
  await moveToArea(deps, todo, areaId, Math.max(todo.length, found.total - dropped));
}
