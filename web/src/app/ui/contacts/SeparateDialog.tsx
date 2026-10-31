// SPDX-License-Identifier: AGPL-3.0-only

// SEPARATE (#816, ADR-041 §4): split addresses off a merged contact. Each chosen
// address goes to a NEW contact made from what it had before it was merged
// (its own name, notes, VIP and block). When the chosen addresses are all of
// a merge's, the contact they leave goes back to how it was before that merge,
// keeping the user's later edits (#834; the page says which). At least one
// address is chosen, and at least one is left — the server refuses either,
// and the dialog says so before asking it.

import { useId, useState } from "react";
import type { JSX } from "react";

import { cleanContactText, contactLabel, separateAllowed } from "../../../core/contacts";
import type { Contact, ContactSeparateResult, Id } from "../../../core/jmap/types";
import { t } from "../../i18n";
import { KeyDialog } from "../settings/keys/KeyDialog";
import type { ContactsApi } from "./api";
import { contactErrorText } from "./text";

export function SeparateDialog({
  api,
  contact,
  onClose,
  onSeparated,
}: {
  api: ContactsApi;
  contact: Contact;
  onClose: () => void;
  onSeparated: (result: ContactSeparateResult) => void;
}): JSX.Element {
  const [chosen, setChosen] = useState<Set<Id>>(new Set());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const whyId = useId();
  const name = contactLabel(contact);
  const allowed = separateAllowed(contact, chosen);

  const toggle = (id: Id, on: boolean): void => {
    setChosen((cur) => {
      const next = new Set(cur);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });
  };

  const go = async (): Promise<void> => {
    if (allowed !== "ok") return;
    setBusy(true);
    setError(null);
    const out = await api.separate(contact.id, [...chosen]);
    setBusy(false);
    if (!out.ok) {
      setError(contactErrorText(out.error));
      return;
    }
    onSeparated(out.value);
  };

  return (
    <KeyDialog title={t("contacts.separate.title", { name })} className="contact-separate" busy={busy} onClose={onClose}>
      <p>{t("contacts.separate.explain")}</p>
      <fieldset className="contact-separate-addresses">
        <legend>{t("contacts.separate.legend")}</legend>
        {contact.addresses.map((a) =>
          a.id ? (
            <label key={a.id} className="checkbox">
              <input
                type="checkbox"
                checked={chosen.has(a.id)}
                onChange={(ev) => toggle(a.id as Id, ev.target.checked)}
              />
              {cleanContactText(a.address)}
            </label>
          ) : null,
        )}
      </fieldset>
      <p id={whyId} className={allowed === "all" ? "field-error" : "field-hint"}>
        {allowed === "none"
          ? t("contacts.separate.none")
          : allowed === "all"
            ? t("contacts.separate.all", { name })
            : t("contacts.separate.ready")}
      </p>
      {error ? (
        <p className="field-error" role="alert">
          {error}
        </p>
      ) : null}
      <div className="sheet-actions">
        <button type="button" className="button" disabled={busy} onClick={onClose}>
          {t("action.cancel")}
        </button>
        <button
          type="button"
          className="button button-primary contact-separate-submit"
          disabled={busy || allowed !== "ok"}
          aria-describedby={whyId}
          onClick={() => void go()}
        >
          {busy ? t("contacts.separate.busy") : t("contacts.separate.submit")}
        </button>
      </div>
    </KeyDialog>
  );
}
