// SPDX-License-Identifier: AGPL-3.0-only

// CLEAN UP (#53): who sends you the most mail, and whose mail you leave alone —
// ranked, with the four things you might do about each.
//
// THE RANKING IS THE SERVER'S (`SenderStats/get`), and so is the word
// "ignored": a message counts as ignored when it is still unread, wherever it
// sits, or when it was archived from Mailroom before it was read
// (internal/store/sender_stats.go says exactly how). A row's count is the count
// its action's search finds — the ranking excludes Trash and Junk by the
// search's own rules — so "Archive all 212" moves 212 (or says why not).
//
// EVERY ACTION IS ONE THE PRODUCT ALREADY HAS:
//
//   * UNSUBSCRIBE opens the existing affordance (../../UnsubscribePanel.tsx) on
//     a recent message from the sender that carries List-Unsubscribe. It asks
//     first and states what clicking costs; nothing is ever sent on its own.
//   * ARCHIVE ALL and MOVE ALL TO… are the #708 sender-wide move
//     (../../SenderBulk.tsx): count, confirm here, one Undo, "Move the rest"
//     past 1,000.
//   * BLOCK is #59's contact marker. On a domain or list row it blocks every
//     address that row has used (up to the 20 it lists, and it says when there
//     are more); a list that starts using a new address is not blocked, and the
//     form says that too.
//
// GROUPED THREE WAYS because newsletters rotate sender addresses: by address,
// by domain, and by mailing list (List-Id). A domain row's actions go
// through SenderStats/emailIds, which matches the row's EXACT domain key (the
// same predicate that ranked it); subdomains are separate rows and are not
// swept in. The count in the confirm is that call's, so the confirm is the
// truth about what will move.

import { useCallback, useEffect, useId, useState } from "react";
import type { JSX } from "react";

import type { BlockDestination, Id, SenderGroupBy, SenderSortBy, SenderStat, SenderStatsResult } from "../../../../core/jmap/types";
import { effectiveEmail } from "../../../../core/store";
import { areasInDeskOrder } from "../../../../core/refile";
import { t } from "../../../i18n";
import { useApp, useMailState } from "../../../state";
import { AreaMenu } from "../../AreaMenu";
import { SenderBulkPanel, useSenderBulk } from "../../SenderBulk";
import { UnsubscribePanel } from "../../UnsubscribePanel";
import { looksLikeAddress } from "../../compose/chips";

/** The windows offered, in days; null is all time. */
export const CLEANUP_WINDOWS: ReadonlyArray<number | null> = [30, 90, 365, null];

/**
 * The set a row's actions move: the row's OWN messages, enumerated by the
 * server with the ranking's predicate (SenderStats/emailIds) in the window the
 * ranking was computed for. Not a `from:`/`list:` search rebuilt from the key
 * — `list:` takes sub-lists and `from:` reads a domain-shaped key as a domain,
 * either of which would move mail the row never showed.
 */
export function rowFetch(
  client: { senderStatsEmailIds: (args: { groupBy: SenderGroupBy; key: string; since: string | null; position: number; limit: number }) => Promise<{ ids: Id[]; total: number }> },
  groupBy: SenderGroupBy,
  key: string,
  since: string | null,
) {
  return (position: number, cap: number) => client.senderStatsEmailIds({ groupBy, key, since, position, limit: cap });
}

/**
 * Whether a row may be acted on: its key must search for EXACTLY this row.
 *
 * The server decides first (`actionable`, store.SenderKeyActionable) and a
 * row it refuses is never acted on. The client re-checks the same rule with
 * the helpers it already has, so a server that predates the flag cannot hand
 * it a key that means something else:
 *
 *   * address rows must be a well-formed local@domain (compose's
 *     looksLikeAddress). A From with no local part — `example.com`,
 *     `@example.com`, `*.example.com`, `.example.com` — would be read by
 *     `from:` as a DOMAIN (#712), so "archive all" on one "address" would
 *     sweep a whole domain;
 *   * domain rows carry no '@' or '*' and no empty label — the row
 *     is that exact domain (subdomains are their own rows), and says so;
 *   * every row: nothing the query syntax splits or groups on.
 */
export function actionable(groupBy: SenderGroupBy, stat: Pick<SenderStat, "key" | "actionable">): boolean {
  const key = stat.key;
  if (stat.actionable === false) return false;
  if (key === "" || /[\s"()]/.test(key)) return false;
  switch (groupBy) {
    case "address":
      return looksLikeAddress(key) && !key.startsWith("*");
    case "domain":
      return !/[@*]/.test(key) && key.split(".").length >= 2 && key.split(".").every((l) => l !== "");
    default:
      return true;
  }
}

/** The addresses a group block ticks by default: not already blocked, held by
 * a contact, and neither a VIP nor someone the user has written to. Those two
 * are listed and can be ticked by hand, never swept up silently. */
export function defaultBlockPicks(stat: Pick<SenderStat, "addresses">): string[] {
  return stat.addresses
    .filter((a) => a.contactId !== null && !a.isBlocked && !a.isVip && !a.correspondent)
    .map((a) => a.contactId as string);
}

/** `42%` — the share of a row's mail left unread or archived unread. */
export function sharePercent(share: number): string {
  return `${Math.round(share * 100)}%`;
}

export function CleanupBody(): JSX.Element {
  const { client } = useApp();
  const accountId = useMailState().accountId;
  const [groupBy, setGroupBy] = useState<SenderGroupBy>("address");
  const [sort, setSort] = useState<SenderSortBy>("volume");
  const [sinceDays, setSinceDays] = useState<number | null>(90);
  const [result, setResult] = useState<SenderStatsResult | null>(null);
  const [failed, setFailed] = useState(false);
  const [tick, setTick] = useState(0);
  const [pages, setPages] = useState(1);
  const reload = useCallback(() => setTick((n) => n + 1), []);

  useEffect(() => {
    if (!accountId) return;
    let cancelled = false;
    setFailed(false);
    void client
      .senderStatsGet({ groupBy, sort, sinceDays, position: 0, limit: 50 * pages })
      .then((r) => {
        if (!cancelled) setResult(r);
      })
      .catch(() => {
        if (!cancelled) {
          setResult(null);
          setFailed(true);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [client, accountId, groupBy, sort, sinceDays, pages, tick]);

  const groupWord = (n: number): string =>
    t(`cleanup.count.${groupBy}.${n === 1 ? "one" : "other"}`, { count: n.toLocaleString("en-US") });

  return (
    <>
      <form className="menu-group cleanup-controls" aria-label={t("cleanup.controls")} onSubmit={(e) => e.preventDefault()}>
        <fieldset className="segmented">
          <legend>{t("cleanup.groupBy")}</legend>
          {(["address", "domain", "list"] as const).map((g) => (
            <label key={g} className="segmented-option">
              <input
                type="radio"
                name="cleanup-group"
                checked={groupBy === g}
                onChange={() => {
                  setGroupBy(g);
                  setPages(1);
                }}
              />
              {t(`cleanup.groupBy.${g}`)}
            </label>
          ))}
        </fieldset>
        <fieldset className="segmented">
          <legend>{t("cleanup.sort")}</legend>
          {(["volume", "neglect"] as const).map((s) => (
            <label key={s} className="segmented-option">
              <input type="radio" name="cleanup-sort" checked={sort === s} onChange={() => setSort(s)} />
              {t(`cleanup.sort.${s}`)}
            </label>
          ))}
        </fieldset>
        <label className="field">
          <span className="field-label">{t("cleanup.window")}</span>
          <select
            className="text-input"
            value={sinceDays === null ? "all" : String(sinceDays)}
            onChange={(e) => {
              setSinceDays(e.target.value === "all" ? null : Number(e.target.value));
              setPages(1);
            }}
          >
            {CLEANUP_WINDOWS.map((w) => (
              <option key={w ?? "all"} value={w === null ? "all" : String(w)}>
                {w === null ? t("cleanup.window.all") : t("cleanup.window.days", { days: w })}
              </option>
            ))}
          </select>
        </label>
      </form>

      <p className="menu-note" role="status">
        {failed
          ? t("cleanup.failed")
          : result === null
            ? t("cleanup.loading")
            : result.total === 0
              ? t("cleanup.none")
              : t("cleanup.showing", { shown: result.list.length.toLocaleString("en-US"), total: groupWord(result.total) })}
      </p>
      <p className="field-hint">{t("cleanup.ignoredMeans")}</p>

      {result && result.list.length > 0 ? (
        <ul className="senders-list cleanup-list">
          {result.list.map((s) => (
            <CleanupItem
              key={`${result.groupBy}:${s.key}`}
              stat={s}
              groupBy={result.groupBy}
              since={result.since ?? null}
              onChanged={reload}
            />
          ))}
        </ul>
      ) : null}
      {result && result.total > result.list.length ? (
        <button type="button" className="button button-quiet menu-action" onClick={() => setPages((p) => p + 1)}>
          {t("cleanup.more")}
        </button>
      ) : null}
    </>
  );
}

function CleanupItem({
  stat,
  groupBy,
  since,
  onChanged,
}: {
  stat: SenderStat;
  groupBy: SenderGroupBy;
  since: string | null;
  onChanged: () => void;
}): JSX.Element {
  const { actions, sync, offerUndo, pushToast } = useApp();
  const state = useMailState();
  const nameId = useId();
  const [unsubOpen, setUnsubOpen] = useState(false);
  const [unsubLoaded, setUnsubLoaded] = useState(false);
  const [blockForm, setBlockForm] = useState<{ dest: BlockDestination; moveExisting: boolean; picked: string[] } | null>(null);
  const [busy, setBusy] = useState(false);
  const label = stat.key;
  const { client } = useApp();
  const bulk = useSenderBulk(label, () => onChanged(), rowFetch(client, groupBy, stat.key, since));
  const canAct = actionable(groupBy, stat);
  const isGroup = groupBy !== "address";

  const contacts = stat.addresses.filter((a) => a.contactId !== null);
  const allBlocked = contacts.length > 0 && contacts.every((a) => a.isBlocked);

  const openUnsub = (): void => {
    if (unsubOpen) {
      setUnsubOpen(false);
      return;
    }
    setUnsubOpen(true);
    if (!stat.unsubscribeEmailId || unsubLoaded) return;
    // The unsubscribe routes are body properties (Email.unsubscribe), so the
    // message is read with its body — the same read the reading pane makes.
    void sync.loadEmails([stat.unsubscribeEmailId], { body: true }).then(() => setUnsubLoaded(true));
  };
  const unsubEmail = stat.unsubscribeEmailId ? effectiveEmail(state, stat.unsubscribeEmailId) : null;

  const block = (): void => {
    if (!blockForm) return;
    const { dest, moveExisting, picked } = blockForm;
    // ONLY what is ticked: on an address row that is the address; on a group
    // row it is exactly the boxes the user left ticked (defaultBlockPicks).
    const ids = contacts.filter((a) => !a.isBlocked && picked.includes(a.contactId as string)).map((a) => a.contactId as string);
    if (ids.length === 0) return;
    setBusy(true);
    void (async () => {
      const changed = await actions.setContactsMarkers(ids, { isBlocked: true, blockDestination: dest }, t("senders.block.failed"));
      setBusy(false);
      if (changed.length === 0) return;
      setBlockForm(null);
      const message = t(dest === "junk" ? "cleanup.blocked.junk" : "cleanup.blocked.trash", {
        count: changed.length,
        key: label,
      });
      offerUndo(message, async () => {
        const back = await actions.setContactsMarkers(changed, { isBlocked: false }, t("senders.unblock.failed"));
        onChanged();
        return back.length === changed.length;
      });
      // Moving existing mail is offered on ADDRESS rows only: on a group row the
      // search would take every address in the group, ticked or not.
      if (moveExisting && !isGroup) bulk.start({ kind: dest });
      else onChanged();
    })();
  };

  /** How many addresses the Block button will actually block. */
  const pickedCount = (picked: string[]): number =>
    contacts.filter((a) => !a.isBlocked && picked.includes(a.contactId as string)).length;

  const unblock = (): void => {
    setBusy(true);
    void actions
      .setContactsMarkers(contacts.map((a) => a.contactId as string), { isBlocked: false }, t("senders.unblock.failed"))
      .then((changed) => {
        setBusy(false);
        if (changed.length > 0) {
          pushToast({ kind: "info", message: t("cleanup.unblocked", { key: label }) });
          onChanged();
        }
      });
  };

  const last = new Date(stat.lastReceivedAt);
  const lastText = Number.isNaN(last.getTime())
    ? ""
    : last.toLocaleDateString("en-GB", { day: "numeric", month: "short", year: "numeric" });

  return (
    <li className="cleanup-item">
      <span className="senders-name" id={nameId}>
        {stat.displayName || stat.key}
      </span>
      {stat.displayName && stat.displayName !== stat.key ? <span className="senders-address">{stat.key}</span> : null}
      <span className="senders-where cleanup-facts">
        {t("cleanup.facts", {
          total: stat.total.toLocaleString("en-US"),
          unread: stat.unread.toLocaleString("en-US"),
          ignored: sharePercent(stat.neglectShare),
          last: lastText,
        })}
        {groupBy === "domain" ? ` ${t("cleanup.everyoneAt", { domain: stat.key })}` : null}
        {isGroup ? ` ${t("cleanup.addresses", { count: stat.addressCount })}` : null}
        {allBlocked ? ` ${t("cleanup.isBlocked")}` : null}
      </span>

      {canAct ? (
        // A group named by the row, so each button is announced in its row's
        // context ("Archive all, group news@example.com") while its own name
        // stays exactly its visible words (WCAG 2.5.3).
        <div className="cleanup-actions" role="group" aria-labelledby={nameId}>
          {stat.unsubscribeEmailId ? (
            <button type="button" className="button button-quiet" aria-expanded={unsubOpen} onClick={openUnsub}>
              {t("cleanup.unsubscribe")}
            </button>
          ) : null}
          <button
            type="button"
            className="button button-quiet"
            disabled={bulk.state !== null}
            onClick={() => bulk.start({ kind: "archive" })}
          >
            {t("cleanup.archiveAll")}
          </button>
          <AreaMenu
            buttonText={t("cleanup.moveAll")}
            menuLabel={t("areaMove.sender.menuLabel", { address: label })}
            areas={areasInDeskOrder(state.areas).map((a) => ({
              id: a.id,
              name: a.name,
              blocked: state.labels[a.labelId] ? null : t("desk.refile.labelMissing", { area: a.name }),
            }))}
            onPick={(areaId) => bulk.start({ kind: "area", areaId })}
            disabled={bulk.state !== null}
          />
          {contacts.length === 0 ? null : allBlocked ? (
            <button type="button" className="button button-quiet" disabled={busy} onClick={unblock}>
              {t("cleanup.unblock")}
            </button>
          ) : (
            <button
              type="button"
              className="button button-quiet"
              aria-expanded={blockForm !== null}
              disabled={busy || bulk.state !== null}
              onClick={() =>
                setBlockForm((f) =>
                  f
                    ? null
                    : {
                        dest: "junk",
                        moveExisting: false,
                        picked: isGroup ? defaultBlockPicks(stat) : contacts.map((a) => a.contactId as string),
                      },
                )
              }
            >
              {t("cleanup.block")}
            </button>
          )}
        </div>
      ) : (
        <p className="field-hint">{t("cleanup.cannotAct")}</p>
      )}

      {unsubOpen ? (
        <div className="cleanup-unsubscribe">
          {unsubEmail ? <UnsubscribePanel email={unsubEmail} /> : <p className="menu-note">{t("cleanup.loading")}</p>}
        </div>
      ) : null}

      {blockForm ? (
        <form
          className="sender-block-form cleanup-block-form"
          aria-label={t("senders.block.formLabel", { address: label })}
          onSubmit={(ev) => {
            ev.preventDefault();
            block();
          }}
        >
          {isGroup ? (
            <fieldset className="cleanup-block-picks">
              <legend>{t("cleanup.block.pick")}</legend>
              {contacts.map((a) => (
                <label key={a.contactId} className="checkbox">
                  <input
                    type="checkbox"
                    disabled={a.isBlocked}
                    checked={a.isBlocked || blockForm.picked.includes(a.contactId as string)}
                    onChange={(ev) =>
                      setBlockForm({
                        ...blockForm,
                        picked: ev.target.checked
                          ? [...blockForm.picked, a.contactId as string]
                          : blockForm.picked.filter((id) => id !== a.contactId),
                      })
                    }
                  />
                  {a.name ? `${a.name} <${a.address}>` : a.address}
                  {a.isVip ? ` ${t("cleanup.block.vip")}` : null}
                  {a.correspondent ? ` ${t("cleanup.block.correspondent")}` : null}
                  {a.isBlocked ? ` ${t("cleanup.block.already")}` : null}
                </label>
              ))}
              <p className="hint">
                {t("cleanup.block.defaults")}
                {stat.addressCount > contacts.length ? ` ${t("cleanup.block.more", { count: stat.addressCount - contacts.length })}` : null}
              </p>
            </fieldset>
          ) : null}
          <fieldset>
            <legend>{t("senders.block.where", { address: label })}</legend>
            {(["junk", "trash"] as const).map((d) => (
              <label key={d} className="radio">
                <input
                  type="radio"
                  name={`cleanup-block-${groupBy}-${stat.key}`}
                  checked={blockForm.dest === d}
                  onChange={() => setBlockForm({ ...blockForm, dest: d })}
                />
                {t(d === "junk" ? "senders.block.toJunk" : "senders.block.toTrash")}
              </label>
            ))}
          </fieldset>
          {isGroup ? (
            <p className="hint">{t("cleanup.block.noMoveForGroups")}</p>
          ) : (
            <label className="checkbox">
              <input
                type="checkbox"
                checked={blockForm.moveExisting}
                onChange={(ev) => setBlockForm({ ...blockForm, moveExisting: ev.target.checked })}
              />
              {t("senders.block.moveExisting", { address: label })}
            </label>
          )}
          <p className="hint">{t("cleanup.block.exact")}</p>
          <div className="sheet-actions">
            <button type="button" className="button button-quiet" onClick={() => setBlockForm(null)}>
              {t("action.cancel")}
            </button>
            <button type="submit" className="button button-primary" disabled={busy || pickedCount(blockForm.picked) === 0}>
              {pickedCount(blockForm.picked) === 1
                ? t("cleanup.block.goOne")
                : t("cleanup.block.goMany", { count: pickedCount(blockForm.picked) })}
            </button>
          </div>
        </form>
      ) : null}

      <SenderBulkPanel bulk={bulk} address={label} />
    </li>
  );
}

