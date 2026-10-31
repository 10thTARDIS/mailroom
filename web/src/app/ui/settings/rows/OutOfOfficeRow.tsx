// SPDX-License-Identifier: AGPL-3.0-only

// OUT OF OFFICE (#50): an automatic reply per mail account, written to the
// provider as a ManageSieve script, so the PROVIDER sends the replies.
//
// WHAT THE FORM PROMISES, AND WHAT IT CANNOT:
//
//   * Saving with the reply on asks the provider FIRST. If another filter
//     script is active there, the provider would run only one of them, so the
//     server refuses and the form shows its sentence, which names that script.
//     Nothing is saved, and the form keeps what was typed.
//   * Replies come from the provider, so they are not counted by Mailroom's
//     sending quota meter (ADR-034). The form says so.
//   * A reply goes only to mail sent to one of the account's own addresses
//     (`addresses`). Mail that reached a per-site catch-all alias gets none,
//     and the form says that too.
//   * Contacts-only is bounded (2,000, most recent first, and less if the
//     provider's script limit is smaller); the line under it states how many
//     were included of how many.
//   * Drift: opening the row asks the provider about every account whose reply
//     is on, and a script changed or switched outside Mailroom is shown as a
//     warning. Nothing repairs it behind the user's back.

import { useCallback, useEffect, useId, useRef, useState } from "react";
import type { JSX } from "react";

import type { OutOfOffice } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import { useApp, useMailState } from "../../../state";
import { contactsLine, formFrom, statusLine, updateFrom } from "../outOfOffice";
import type { OutOfOfficeForm } from "../outOfOffice";

export interface OutOfOfficeState {
  /** null until read, or when this server has no out-of-office API. */
  list: OutOfOffice[] | null;
  loaded: boolean;
  replace: (o: OutOfOffice) => void;
}

export function useOutOfOffice(): OutOfOfficeState {
  const { client } = useApp();
  const accountId = useMailState().accountId;
  const [list, setList] = useState<OutOfOffice[] | null>(null);
  const [loaded, setLoaded] = useState(false);
  useEffect(() => {
    if (!accountId) return;
    let cancelled = false;
    void client.outOfOfficeGet().then((l) => {
      if (cancelled) return;
      setList(l);
      setLoaded(true);
    });
    return () => {
      cancelled = true;
    };
  }, [client, accountId]);
  const replace = useCallback((o: OutOfOffice) => {
    setList((prev) => (prev === null ? prev : prev.map((x) => (x.id === o.id ? o : x))));
  }, []);
  return { list, loaded, replace };
}

export function OutOfOfficeBody({ oof }: { oof: OutOfOfficeState }): JSX.Element {
  if (!oof.loaded) return <p className="menu-note">{t("oof.loading")}</p>;
  if (oof.list === null) return <p className="menu-note">{t("oof.unavailable")}</p>;
  if (oof.list.length === 0) return <p className="menu-note">{t("oof.noAccounts")}</p>;
  return (
    <>
      <p className="menu-note">{t("oof.hint.provider")}</p>
      {oof.list.map((o) => (
        <AccountReply key={o.id} initial={o} onSaved={oof.replace} />
      ))}
    </>
  );
}

function AccountReply({ initial, onSaved }: { initial: OutOfOffice; onSaved: (o: OutOfOffice) => void }): JSX.Element {
  const { client } = useApp();
  const id = useId();
  const [obj, setObj] = useState(initial);
  const [form, setForm] = useState<OutOfOfficeForm>(() => formFrom(initial));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);

  // Ask the provider about a reply that is on (or was found drifting), ONCE,
  // when the row opens: that is how a change made in webmail becomes visible.
  const checked = useRef(false);
  useEffect(() => {
    if (checked.current) return;
    checked.current = true;
    if (!initial.isSupported || !(initial.isEnabled || initial.serverState !== "off")) return;
    void client.outOfOfficeCheck(initial.id).then((fresh) => {
      if (fresh) {
        setObj(fresh);
        onSaved(fresh);
      }
    });
  }, [client, initial, onSaved]);

  const set = <K extends keyof OutOfOfficeForm>(k: K, v: OutOfOfficeForm[K]): void => {
    setForm((f) => ({ ...f, [k]: v }));
    setSaved(null);
  };

  const save = (): void => {
    const update = updateFrom(form);
    if ("error" in update) {
      setError(update.error);
      return;
    }
    setBusy(true);
    setError(null);
    setSaved(null);
    void client
      .outOfOfficeSet(obj.id, update)
      .then((r) => {
        if (r.ok) {
          setObj(r.value);
          setForm(formFrom(r.value));
          onSaved(r.value);
          setSaved(t(r.value.isEnabled ? "oof.saved.on" : "oof.saved.off"));
        } else {
          setError(r.error || t("oof.err.failed"));
        }
      })
      .catch(() => setError(t("oof.err.failed")))
      .finally(() => setBusy(false));
  };

  const status = statusLine(obj);
  const contacts = contactsLine(obj);
  const headingId = `${id}-h`;

  if (!obj.isSupported) {
    return (
      <section className="menu-group oof-account" aria-labelledby={headingId}>
        <h3 id={headingId}>{obj.accountName}</h3>
        <p className="menu-note">{status.text}</p>
      </section>
    );
  }

  return (
    <form
      className="menu-group oof-account"
      aria-labelledby={headingId}
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <h3 id={headingId}>{obj.accountName}</h3>
      <p className={status.warn ? "menu-note oof-status warn" : "menu-note oof-status"} role="status">
        {status.text}
      </p>
      <label className="toggle-row">
        <input type="checkbox" checked={form.isEnabled} onChange={(e) => set("isEnabled", e.target.checked)} />
        {t("oof.enable")}
      </label>
      <div className="oof-dates">
        <label htmlFor={`${id}-first`}>{t("oof.firstDay")}</label>
        <input id={`${id}-first`} type="date" value={form.firstDay} aria-describedby={`${id}-dates`}
          onChange={(e) => set("firstDay", e.target.value)} />
        <label htmlFor={`${id}-last`}>{t("oof.lastDay")}</label>
        <input id={`${id}-last`} type="date" value={form.lastDay} aria-describedby={`${id}-dates`}
          onChange={(e) => set("lastDay", e.target.value)} />
        <p id={`${id}-dates`} className="menu-note">{t("oof.hint.dates")}</p>
      </div>
      <label htmlFor={`${id}-subject`}>{t("oof.subject")}</label>
      <input id={`${id}-subject`} type="text" value={form.subject} maxLength={200}
        onChange={(e) => set("subject", e.target.value)} />
      <label htmlFor={`${id}-body`}>{t("oof.body")}</label>
      <textarea id={`${id}-body`} rows={6} value={form.textBody} onChange={(e) => set("textBody", e.target.value)} />
      <label htmlFor={`${id}-days`}>{t("oof.days")}</label>
      <input id={`${id}-days`} type="number" min={1} max={30} step={1} value={form.replyDays}
        aria-describedby={`${id}-days-hint`}
        onChange={(e) => set("replyDays", Number(e.target.value))} />
      <p id={`${id}-days-hint`} className="menu-note">{t("oof.hint.days")}</p>
      {obj.addresses.length > 0 && (
        <>
          <label htmlFor={`${id}-from`}>{t("oof.from")}</label>
          <select id={`${id}-from`} value={form.fromAddress} onChange={(e) => set("fromAddress", e.target.value)}>
            {obj.addresses.map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>
        </>
      )}
      <p className="menu-note">{t("oof.hint.addresses", { addresses: obj.addresses.join(", ") })}</p>
      <label className="toggle-row">
        <input type="checkbox" checked={form.contactsOnly} aria-describedby={`${id}-contacts`}
          onChange={(e) => set("contactsOnly", e.target.checked)} />
        {t("oof.contactsOnly")}
      </label>
      <p id={`${id}-contacts`} className="menu-note">
        {t("oof.hint.contacts", { max: obj.maxContacts.toLocaleString("en-US") })}
        {contacts ? ` ${contacts}` : ""}
      </p>
      {error && (
        <p className="menu-note oof-error" role="alert">
          {error}
        </p>
      )}
      {saved && (
        <p className="menu-note" role="status">
          {saved}
        </p>
      )}
      <button type="submit" disabled={busy}>
        {busy ? t("oof.saving") : t("oof.save", { name: obj.accountName })}
      </button>
    </form>
  );
}
