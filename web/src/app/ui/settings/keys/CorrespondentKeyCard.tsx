// SPDX-License-Identifier: AGPL-3.0-only

// ONE KEY YOU HOLD FOR SOMEONE ELSE (#796, and the v1.93 final scope): whose it
// is (its primary user ID as the title, #812), where it came from, whether it is
// the one encrypted mail to them uses, when it stops, and what can be done:
//
//   * Check for an updated key (#818): ContactKey/refresh. The primary action
//     when the key expires soon or has expired, otherwise after the others.
//     The answer is one plain sentence; "they've published a different key"
//     opens the same key-changed decision the reading pane and compose show
//     (KeyChangeDecision), and is never taken as a replacement on its own.
//   * Show history (#815): ContactKey/history, read when asked, as a short
//     dated list in words.
//   * Don't use this key / Use this key again: ContactKey/decide keep / trust.
//     This, not Remove, is how a key is stopped for good (ADR-040 §11): a
//     removed key comes back when it is seen again.
//   * Remove…: the caller's confirm.
//
// A REMOVED key (under History) shows when it was removed and its history, and
// nothing else: the server uses it for nothing and refreshes nothing.
//
// User IDs are text from whoever made the key: text nodes only, never markup.

import { useId, useState } from "react";
import type { JSX } from "react";

import {
  decideAddress,
  keyHistoryLines,
  refreshIsProminent,
  refreshOutcome,
  titleUserIds,
  userIdText,
} from "../../../../core/keyLifecycle";
import type { KeyHistoryLine, RefreshOutcome } from "../../../../core/keyLifecycle";
import { contactKeySource, keyDateLong } from "../../../../core/userKeys";
import type { CorrespondentKeyRow, KeyActionError } from "../../../../core/userKeys";
import { groupFingerprint } from "../../../../core/pgpSignature";
import { stripBidiControls } from "../../../../core/hostileText";
import { t } from "../../../i18n";
import { KeyChangeDecision } from "../../KeyChangeDecision";
import type { KeysApi } from "./api";
import { SOURCE_TEXT, keyErrorText, keyEventText, refreshText } from "./text";

type History =
  | { kind: "closed" }
  | { kind: "loading" }
  | { kind: "loaded"; lines: KeyHistoryLine[] }
  | { kind: "failed"; error: KeyActionError };

type Refresh =
  | { kind: "idle" }
  | { kind: "busy" }
  | { kind: "done"; outcome: RefreshOutcome }
  | { kind: "failed"; error: KeyActionError };

/** The name a correspondent key goes by on the page: its primary user ID, or
 * (a key without one, an older server) the contact's name or address. */
export function correspondentKeyName(row: CorrespondentKeyRow): string {
  const { title } = titleUserIds(row.key.userIds);
  return stripBidiControls(title ? userIdText(title) : row.name);
}

export function CorrespondentKeyCard({
  row,
  contactAddresses,
  api,
  onChanged,
  onReload,
  onRemove,
  onRefuse,
  contactLink,
}: {
  /** Settings (#816): a link to the contact this key is held under. */
  contactLink?: { href: string; open: () => void };
  row: CorrespondentKeyRow;
  /** The addresses of the contact the key is held under. */
  contactAddresses: readonly string[];
  api: KeysApi;
  /** A change landed: reload the list, and say this. */
  onChanged: (message: string) => void;
  /** Reload the list without a page-level message (the card says it). */
  onReload: () => void;
  /** Ask to remove it (the caller confirms). Not offered for a removed key. */
  onRemove: (row: CorrespondentKeyRow) => void;
  /** Ask to refuse it (the caller confirms). */
  onRefuse: (row: CorrespondentKeyRow) => void;
}): JSX.Element {
  const k = row.key;
  const removed = !!k.removedAt;
  const [history, setHistory] = useState<History>({ kind: "closed" });
  const [refresh, setRefresh] = useState<Refresh>({ kind: "idle" });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<KeyActionError | null>(null);
  const historyId = useId();
  const { title, others } = titleUserIds(k.userIds);
  const titleText = title ? stripBidiControls(userIdText(title)) : null;
  const name = correspondentKeyName(row);
  const prominent = !removed && refreshIsProminent(k);
  const address = decideAddress(k, contactAddresses);

  const toggleHistory = async (): Promise<void> => {
    if (history.kind !== "closed") {
      setHistory({ kind: "closed" });
      return;
    }
    setHistory({ kind: "loading" });
    const out = await api.correspondentKeyHistory(k.id);
    setHistory(out.ok ? { kind: "loaded", lines: keyHistoryLines(out.value) } : { kind: "failed", error: out.error });
  };

  const check = async (): Promise<void> => {
    if (refresh.kind === "busy") return;
    setRefresh({ kind: "busy" });
    const out = await api.refreshCorrespondentKey(k.id);
    if (!out.ok) {
      setRefresh({ kind: "failed", error: out.error });
      return;
    }
    const outcome = refreshOutcome(out.value);
    setRefresh({ kind: "done", outcome });
    // A merge changed what the page shows (the expiry, the chips); a different
    // key stays a question, answered below, so the list is not reloaded under it.
    if (outcome.kind === "renewed" || outcome.kind === "revoked" || outcome.kind === "updated") {
      onReload();
    }
  };

  const trustAgain = async (): Promise<void> => {
    if (address === null) return;
    setBusy(true);
    setError(null);
    const out = await api.decideCorrespondentKey(address, k.fingerprint, "trust");
    setBusy(false);
    if (!out.ok) setError(out.error);
    else onChanged(t("keys.done.trusted", { name }));
  };

  const refreshButton = removed ? null : (
    <button
      type="button"
      className={prominent ? "button button-primary key-refresh" : "button key-refresh"}
      aria-label={t("keys.refresh.checkFor", { name })}
      disabled={refresh.kind === "busy"}
      onClick={() => void check()}
    >
      {refresh.kind === "busy" ? t("keys.refresh.busy") : t("keys.refresh.check")}
    </button>
  );

  const refreshLine =
    refresh.kind === "done"
      ? refreshText(refresh.outcome)
      : refresh.kind === "failed"
        ? refreshText({ kind: "error", error: refresh.error })
        : refresh.kind === "busy"
          ? t("keys.refresh.busy")
          : "";

  return (
    <li className={removed ? "correspondent-key is-removed" : "correspondent-key"}>
      <p className="correspondent-key-name">
        <strong className="key-uid" title={titleText ?? undefined}>
          {titleText ?? stripBidiControls(row.name)}
        </strong>
        {titleText === null && row.addresses.length > 0 && row.addresses[0] !== row.name ? (
          <span className="field-hint"> {row.addresses.map(stripBidiControls).join(", ")}</span>
        ) : null}
      </p>
      {contactLink ? (
        <p className="correspondent-key-contact">
          <a
            href={contactLink.href}
            aria-label={t("contacts.openFor", { name })}
            onClick={(ev) => {
              if (ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
              ev.preventDefault();
              contactLink.open();
            }}
          >
            {t("contacts.open")}
          </a>
        </p>
      ) : null}
      {others.length > 0 ? (
        <ul className="key-uids" aria-label={t("keys.userIds.others")}>
          {others.map((u, i) => {
            const text = stripBidiControls(userIdText(u));
            return (
              <li key={`${i}:${text}`} className={u.revoked ? "key-uid is-revoked" : "key-uid"} title={text}>
                {text}
                {u.revoked ? ` ${t("keys.userIds.revoked")}` : null}
              </li>
            );
          })}
        </ul>
      ) : null}
      <p>
        <code className="setting-fingerprint">{groupFingerprint(k.fingerprint)}</code>
      </p>
      <p className="own-key-facts">
        <span>{t(SOURCE_TEXT[contactKeySource(k)])}</span>
        <span className="setting-sep"> · </span>
        <span className={k.expired ? "is-warn" : undefined}>
          {k.expiresAt === null
            ? t("keys.neverExpires")
            : k.expired
              ? t("keys.expiredOn", { date: keyDateLong(k.expiresAt) })
              : t("keys.expires", { date: keyDateLong(k.expiresAt) })}
        </span>
        {k.removedAt ? (
          <>
            <span className="setting-sep"> · </span>
            <span>{t("keys.history.removedOn", { date: keyDateLong(k.removedAt) })}</span>
          </>
        ) : null}
      </p>
      <ul className="key-chips" aria-label={t("keys.chips")}>
        {k.pinned ? (
          <li className="remote-allow-kind">
            {t("keys.chip.pinned", { addresses: k.pinnedFor.map(stripBidiControls).join(", ") })}
          </li>
        ) : null}
        {k.refused ? <li className="remote-allow-kind is-warn">{t("keys.chip.refused")}</li> : null}
        {k.revoked ? <li className="remote-allow-kind is-warn">{t("keys.chip.revoked")}</li> : null}
        {k.expired && !k.revoked ? <li className="remote-allow-kind is-warn">{t("keys.chip.expired")}</li> : null}
        {!k.expired && !k.revoked && k.expiringSoon ? (
          <li className="remote-allow-kind is-warn">{t("keys.chip.expiringSoon")}</li>
        ) : null}
        {!k.hasEncryptionKey ? <li className="remote-allow-kind is-warn">{t("keys.chip.noEncryption")}</li> : null}
      </ul>

      <div className="own-key-actions" role="group" aria-label={t("keys.actionsFor", { key: name })}>
        {prominent ? refreshButton : null}
        <button
          type="button"
          className="button key-history-show"
          aria-expanded={history.kind !== "closed"}
          aria-controls={historyId}
          aria-label={
            history.kind === "closed" ? t("keys.history.showFor", { name }) : t("keys.history.hideFor", { name })
          }
          onClick={() => void toggleHistory()}
        >
          {history.kind === "closed" ? t("keys.history.show") : t("keys.history.hide")}
        </button>
        {!removed && !k.refused ? (
          <button
            type="button"
            className="button"
            aria-label={t("keys.refuse.actionFor", { name })}
            disabled={busy}
            onClick={() => onRefuse(row)}
          >
            {t("keys.refuse.action")}
          </button>
        ) : null}
        {!removed && k.refused && address !== null ? (
          <button
            type="button"
            className="button"
            aria-label={t("keys.trust.actionFor", { name })}
            disabled={busy}
            onClick={() => void trustAgain()}
          >
            {t("keys.trust.action")}
          </button>
        ) : null}
        {!removed ? (
          <button
            type="button"
            className="button"
            aria-label={t("keys.theirs.removeFor", { name })}
            disabled={busy}
            onClick={() => onRemove(row)}
          >
            {t("keys.theirs.remove")}
          </button>
        ) : null}
        {!prominent ? refreshButton : null}
      </div>

      {error ? (
        <p className="field-error" role="alert">
          {keyErrorText(error)}
        </p>
      ) : null}
      {!removed ? (
        <p className="field-hint key-refresh-result" role="status">
          {refreshLine}
        </p>
      ) : null}
      {refresh.kind === "done" && refresh.outcome.kind === "differentKey" && refresh.outcome.newFingerprint && address ? (
        <KeyChangeDecision
          address={address}
          fingerprint={refresh.outcome.newFingerprint}
          previousFingerprint={k.fingerprint}
          heading={t("keys.refresh.decisionHeading", { name })}
          onDecide={async (decision) => {
            const fpr = (refresh.outcome as { newFingerprint: string }).newFingerprint;
            const out = await api.decideCorrespondentKey(address, fpr, decision);
            if (!out.ok) {
              setError(out.error);
              return false;
            }
            setRefresh({ kind: "idle" });
            onChanged(decision === "trust" ? t("keys.done.trusted", { name }) : t("keys.done.refused", { name }));
            return true;
          }}
        />
      ) : null}

      <div id={historyId} className="key-events-wrap" hidden={history.kind === "closed"}>
        {history.kind === "loading" ? (
          <p className="field-hint" role="status">
            {t("keys.history.loading")}
          </p>
        ) : history.kind === "failed" ? (
          <p className="field-error" role="alert">
            {keyErrorText(history.error)}
          </p>
        ) : history.kind === "loaded" ? (
          history.lines.length === 0 ? (
            <p className="field-hint">{t("keys.history.empty")}</p>
          ) : (
            <ol className="key-events" aria-label={t("keys.history.list", { name })}>
              {history.lines.map((line, i) => (
                <li key={`${i}:${line.at}`}>
                  <time dateTime={line.at}>{keyDateLong(line.at)}</time> {keyEventText(line)}
                </li>
              ))}
            </ol>
          )
        ) : null}
      </div>
    </li>
  );
}
