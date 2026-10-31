// SPDX-License-Identifier: AGPL-3.0-only

// THE REQUESTS WAITING FOR YOUR APPROVAL (#49, ADR-042 §7).
//
// ⚠ EVERY LINE OF A CARD COMES FROM A STRUCTURED FIELD of PendingAction/get
// (model.ts pendingView), never from `summary`. The summary is a notification's
// prose and carries the key's own subject line; a card built from it could be
// rewritten by the agent it is asking about.
//
// RECIPIENTS FIRST, because "who receives this" is the decision. To, Cc and Bcc
// are separate lists, each shown in full or with a "Show all N" control — never
// cut short silently. Anyone the envelope delivers to who is not in the headers
// gets a list of their own, in the warning tone: they would receive the message
// without appearing on it.
//
// Then the subject (marked as written by the agent), attachments, what else
// approving archives, and for a delete the messages it moves to Trash (their
// subjects read by Email/get — still structured, and still strangers' text).
// Every piece of agent- or mail-written text goes through <Untrusted>.
//
// APPROVE OPENS A DIALOG that states exactly what will happen, with Cancel
// focused first; DENY acts at once (it is the safe answer and cannot be
// regretted). The server's answer is shown on the card, and an expired or
// already-decided request says so.

import { useEffect, useRef, useState } from "react";
import type { JSX } from "react";

import type { Id, PendingAction } from "../../../../core/jmap/types";
import { t, tCount } from "../../../i18n";
import { keyDateLong } from "../../../../core/userKeys";
import { clockText } from "../../desk/format";
import { KeyDialog } from "../keys/KeyDialog";
import type { ApiKeysApi, Decision } from "./api";
import { MAX_DELETE_SUBJECTS } from "./api";
import {
  approveText,
  attachmentsText,
  decisionText,
  pendingView,
  timeLeftText,
} from "./model";
import type { PendingView } from "./model";
import { Untrusted } from "./Untrusted";

/** Addresses shown before "Show all". */
export const RECIPIENTS_SHOWN = 8;
/** Subjects / names shown before "Show all". */
const ITEMS_SHOWN = 5;

/** "12 Nov 2026, 09:30" — a scheduled send's due time. */
export function sendAtText(iso: string): string {
  return `${keyDateLong(iso)}, ${clockText(iso)}`;
}

export function PendingList({
  waiting,
  api,
  onDecided,
  now = Date.now(),
}: {
  waiting: readonly PendingAction[];
  api: ApiKeysApi;
  /** After any decision: re-read the list (and the banner). */
  onDecided: () => void;
  now?: number;
}): JSX.Element {
  // Decided cards stay on screen with their answer until the next visit, so
  // the result of pressing Approve is never a card that silently vanished.
  const [decided, setDecided] = useState<Map<Id, { view: PendingView; out: Decision }>>(new Map());
  const shown = waiting.filter((p) => !decided.has(p.id));
  const record = (view: PendingView, out: Decision): void => {
    setDecided((m) => new Map(m).set(view.id, { view, out }));
    onDecided();
  };
  if (shown.length === 0 && decided.size === 0) {
    return <p className="field-hint">{t("apiKeys.pending.none")}</p>;
  }
  return (
    <ul className="pending-list">
      {[...decided.values()].map(({ view, out }) => (
        <li key={view.id} className="pending-card is-decided">
          <DecidedCard view={view} out={out} />
        </li>
      ))}
      {shown.map((p) => (
        <li key={p.id} className="pending-card">
          <PendingCard pending={p} api={api} now={now} onDecided={record} />
        </li>
      ))}
    </ul>
  );
}

function DecidedCard({ view, out }: { view: PendingView; out: Decision }): JSX.Element {
  const words = decisionText(out, view);
  return (
    <>
      <p className="pending-card-heading">
        {view.kind === "delete" ? t("apiKeys.pending.kind.delete") : t("apiKeys.pending.kind.send")}
        {" · "}
        {t("apiKeys.pending.fromKey")} <Untrusted text={view.keyName} />
      </p>
      <p className={words.tone === "warn" ? "pending-result is-warn" : "pending-result"} role="status">
        {words.text}
      </p>
    </>
  );
}

export function PendingCard({
  pending,
  api,
  now,
  onDecided,
}: {
  pending: PendingAction;
  api: ApiKeysApi;
  now: number;
  onDecided: (view: PendingView, out: Decision) => void;
}): JSX.Element {
  const view = pendingView(pending);
  const [approving, setApproving] = useState(false);
  const [busy, setBusy] = useState(false);
  const headingId = `pending-h-${view.id}`;

  const decide = async (state: "approved" | "denied"): Promise<void> => {
    setBusy(true);
    const out = await api.decide(view.id, state);
    setBusy(false);
    setApproving(false);
    onDecided(view, out);
  };

  return (
    <section aria-labelledby={headingId}>
      <h5 className="pending-card-heading" id={headingId}>
        {view.kind === "delete"
          ? t("apiKeys.pending.kind.delete")
          : view.kind === "send"
            ? t("apiKeys.pending.kind.send")
            : t("apiKeys.pending.kind.other")}
        {" · "}
        {t("apiKeys.pending.fromKey")} <Untrusted text={view.keyName} />
      </h5>
      <PendingDetails view={view} api={api} />
      <p className="field-hint pending-expiry">{timeLeftText(view.expiresAt, now)}</p>
      <div className="pending-actions">
        <button type="button" className="button button-primary" disabled={busy} onClick={() => setApproving(true)}>
          {t("apiKeys.pending.approve")}
        </button>
        <button type="button" className="button" disabled={busy} onClick={() => void decide("denied")}>
          {t("apiKeys.pending.deny")}
        </button>
        {busy ? (
          <span className="field-hint" role="status">
            {t("apiKeys.pending.deciding")}
          </span>
        ) : null}
      </div>
      {approving ? (
        <ApproveDialog view={view} api={api} busy={busy} onApprove={() => void decide("approved")} onClose={() => setApproving(false)} />
      ) : null}
    </section>
  );
}

/** The structured fields, in decision order. Shared by the card and the dialog. */
export function PendingDetails({ view, api }: { view: PendingView; api: ApiKeysApi }): JSX.Element {
  if (view.kind === "delete") return <DeleteDetails view={view} api={api} />;
  return (
    <dl className="pending-fields">
      {view.to.length > 0 ? <AddressRow label={t("apiKeys.pending.to")} list={view.to} /> : null}
      {view.cc.length > 0 ? <AddressRow label={t("apiKeys.pending.cc")} list={view.cc} /> : null}
      {view.bcc.length > 0 ? <AddressRow label={t("apiKeys.pending.bcc")} list={view.bcc} /> : null}
      {view.envelopeOnly.length > 0 ? (
        <AddressRow label={t("apiKeys.pending.envelopeOnly")} list={view.envelopeOnly} warn />
      ) : null}
      {view.to.length + view.cc.length + view.bcc.length + view.envelopeOnly.length === 0 ? (
        <>
          <dt>{t("apiKeys.pending.recipients")}</dt>
          <dd>{t("apiKeys.pending.noRecipients")}</dd>
        </>
      ) : null}
      <dt>{t("apiKeys.pending.subject")}</dt>
      <dd>
        {view.subject === null || view.subject === "" ? (
          <span className="field-hint">{t("apiKeys.pending.noSubject")}</span>
        ) : (
          <Untrusted text={view.subject} className="pending-subject" />
        )}
        <span className="field-hint pending-provenance"> {t("apiKeys.pending.writtenByAgent")}</span>
      </dd>
      <dt>{t("apiKeys.pending.attachmentsLabel")}</dt>
      <dd>
        {view.attachments === null || view.attachments.count === 0 ? (
          t("apiKeys.pending.noAttachments")
        ) : (
          <>
            {attachmentsText(view.attachments)}
            <ItemList items={view.attachments.names} total={view.attachments.count} />
          </>
        )}
      </dd>
      {view.archive ? (
        <>
          <dt>{t("apiKeys.pending.alsoArchives")}</dt>
          <dd>
            {tCount("apiKeys.pending.archiveCount", view.archive.count)}
            <ItemList items={view.archive.subjects} total={view.archive.count} />
            <span className="field-hint"> {t("apiKeys.pending.writtenBySenders")}</span>
          </dd>
        </>
      ) : null}
      <dt>{t("apiKeys.pending.when")}</dt>
      <dd>{view.sendAt ? t("apiKeys.pending.sendAt", { when: sendAtText(view.sendAt) }) : t("apiKeys.pending.sendNow")}</dd>
    </dl>
  );
}

function AddressRow({ label, list, warn = false }: { label: string; list: readonly string[]; warn?: boolean }): JSX.Element {
  const [all, setAll] = useState(false);
  const shown = all ? list : list.slice(0, RECIPIENTS_SHOWN);
  return (
    <>
      <dt className={warn ? "is-warn" : undefined}>
        {label} <span className="pending-count">({list.length})</span>
      </dt>
      <dd className={warn ? "is-warn" : undefined}>
        <ul className="pending-addresses">
          {shown.map((a, i) => (
            <li key={`${i}-${a}`}>
              <Untrusted text={a} />
            </li>
          ))}
        </ul>
        {list.length > RECIPIENTS_SHOWN ? (
          <button type="button" className="link-button" aria-expanded={all} onClick={() => setAll((v) => !v)}>
            {all ? t("apiKeys.pending.showFewer") : t("apiKeys.pending.showAll", { count: list.length })}
          </button>
        ) : null}
      </dd>
    </>
  );
}

/** Names or subjects, each its own isolated item; "Show all" past five. When
 * the server stored fewer than the total (it keeps at most 20 names and 5
 * subjects), the list says how many it holds. */
function ItemList({ items, total }: { items: readonly string[]; total: number }): JSX.Element | null {
  const [all, setAll] = useState(false);
  if (items.length === 0) return null;
  const shown = all ? items : items.slice(0, ITEMS_SHOWN);
  return (
    <>
      <ul className="pending-items">
        {shown.map((s, i) => (
          <li key={`${i}-${s}`}>
            <Untrusted text={s} />
          </li>
        ))}
      </ul>
      {items.length > ITEMS_SHOWN ? (
        <button type="button" className="link-button" aria-expanded={all} onClick={() => setAll((v) => !v)}>
          {all ? t("apiKeys.pending.showFewer") : t("apiKeys.pending.showAll", { count: items.length })}
        </button>
      ) : null}
      {total > items.length ? (
        <p className="field-hint">{t("apiKeys.pending.namesBound", { shown: items.length, total })}</p>
      ) : null}
    </>
  );
}

function DeleteDetails({ view, api }: { view: PendingView; api: ApiKeysApi }): JSX.Element {
  // ⚠ READ IN THE REQUEST'S OWN ACCOUNT, OR NOT AT ALL (#856 review F1). A
  // lookup in another account answers notFound for every id, and "N of them
  // are gone" would then be a false claim about what Approve will trash — one
  // the agent could steer. Without the request's account, or when nothing at
  // all was found, the card says the subjects couldn't be shown; "not found"
  // is claimed only for some ids of a lookup that found the others.
  const accountId = view.accountId;
  const [subjects, setSubjects] = useState<Map<Id, string> | null | "asking">(accountId ? "asking" : null);
  const ids = view.emailIds;
  const idsKey = ids.join(",");
  useEffect(() => {
    if (!accountId) return;
    let cancelled = false;
    void api.subjects(ids, accountId).then((m) => {
      if (!cancelled) setSubjects(m);
    });
    return () => {
      cancelled = true;
    };
    // The ids are the request; their joined form is the stable dependency.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api, idsKey, accountId]);
  const asked = ids.slice(0, MAX_DELETE_SUBJECTS);
  const found = subjects instanceof Map ? asked.filter((id) => subjects.has(id)) : [];
  // Nothing found is not evidence that everything is gone: it is what a
  // lookup in the wrong place looks like, so it reads as "couldn't be shown".
  const shown = subjects instanceof Map && found.length > 0 ? subjects : null;
  const gone = shown ? asked.length - found.length : 0;
  return (
    <dl className="pending-fields">
      <dt>{t("apiKeys.pending.moves")}</dt>
      <dd>
        {tCount("apiKeys.pending.deleteCount", ids.length)}
        {subjects === "asking" ? (
          <p className="field-hint">{t("apiKeys.pending.subjectsAsking")}</p>
        ) : shown === null ? (
          <p className="field-hint">{t("apiKeys.pending.subjectsUnavailable")}</p>
        ) : (
          <>
            <ItemList
              items={found.map((id) => shown.get(id) || t("apiKeys.pending.noSubject"))}
              total={found.length}
            />
            {gone > 0 ? <p className="field-hint">{tCount("apiKeys.pending.subjectsGone", gone)}</p> : null}
            {ids.length > asked.length ? (
              <p className="field-hint">{t("apiKeys.pending.subjectsBound", { shown: asked.length, total: ids.length })}</p>
            ) : null}
            <span className="field-hint"> {t("apiKeys.pending.writtenBySenders")}</span>
          </>
        )}
      </dd>
      <dt>{t("apiKeys.pending.restorable")}</dt>
      <dd>{t("apiKeys.pending.restorableText")}</dd>
    </dl>
  );
}

function ApproveDialog({
  view,
  api,
  busy,
  onApprove,
  onClose,
}: {
  view: PendingView;
  api: ApiKeysApi;
  busy: boolean;
  onApprove: () => void;
  onClose: () => void;
}): JSX.Element {
  const cancelRef = useRef<HTMLButtonElement | null>(null);
  const sentence = approveText(view, view.sendAt ? sendAtText(view.sendAt) : null);
  return (
    <KeyDialog
      title={view.kind === "delete" ? t("apiKeys.approve.titleDelete") : t("apiKeys.approve.titleSend")}
      className="approve-dialog"
      busy={busy}
      onClose={onClose}
      initialFocus={cancelRef}
    >
      <p className="approve-what">
        <strong>{sentence}</strong>
      </p>
      <p>
        {t("apiKeys.pending.fromKey")} <Untrusted text={view.keyName} />
      </p>
      <PendingDetails view={view} api={api} />
      <p className="field-hint">{view.kind === "delete" ? t("apiKeys.approve.deleteNote") : t("apiKeys.approve.sendNote")}</p>
      {busy ? (
        <p className="field-hint" role="status">
          {t("apiKeys.pending.deciding")}
        </p>
      ) : null}
      <div className="sheet-actions">
        <button type="button" className="button" ref={cancelRef} disabled={busy} onClick={onClose}>
          {t("action.cancel")}
        </button>
        <button type="button" className="button button-primary approve-submit" disabled={busy} onClick={onApprove}>
          {view.kind === "delete" ? t("apiKeys.approve.submitDelete") : t("apiKeys.approve.submitSend")}
        </button>
      </div>
    </KeyDialog>
  );
}
