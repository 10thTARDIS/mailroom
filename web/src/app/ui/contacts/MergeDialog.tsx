// SPDX-License-Identifier: AGPL-3.0-only

// MERGE WITH… (#816, ADR-041 §4): the same person, two addresses, one contact.
//
// The search finds SAVED contacts by name or address, and — when what is typed
// is an address — ANY contact with exactly that address, saved or not, because
// the address being merged in is often one the user never saved (an old work
// address that still writes now and then).
//
// BEFORE ANYTHING HAPPENS IT SAYS WHAT WILL BE COMBINED, by the server's own
// rules (core/contacts.ts mergePreview): every address, the keys that move, the
// notes joined, the name kept, VIP and saved if either was — and, the one that
// surprises, that A BLOCK ON EITHER ONE CARRIES OVER to the merged contact.
//
// The search is a combobox over a listbox (WAI-ARIA 1.2): focus stays in the
// field, ArrowUp/ArrowDown move the active option (aria-activedescendant), Enter
// chooses it. The dialog is Settings → Keys' own modal shell (KeyDialog): a
// portal, a focus trap, Escape closes this and nothing behind it.

import { useEffect, useId, useRef, useState } from "react";
import type { JSX } from "react";

import { contactIdentity, contactLabel, mergePreview } from "../../../core/contacts";
import type { Contact, ContactMergeResult } from "../../../core/jmap/types";
import { t, tCount } from "../../i18n";
import { looksLikeAddress } from "../compose/chips";
import { KeyDialog } from "../settings/keys/KeyDialog";
import type { ContactsApi } from "./api";
import { contactErrorText } from "./text";

const SEARCH_DELAY_MS = 200;

export function MergeDialog({
  api,
  into,
  onClose,
  onMerged,
}: {
  api: ContactsApi;
  into: Contact;
  onClose: () => void;
  onMerged: (result: ContactMergeResult, fromName: string) => void;
}): JSX.Element {
  const [text, setText] = useState("");
  const [results, setResults] = useState<Contact[] | null>(null);
  const [active, setActive] = useState(0);
  const [chosen, setChosen] = useState<Contact | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const inputId = useId();
  const hintId = useId();
  const listId = useId();
  const intoName = contactLabel(into);

  useEffect(() => {
    const q = text.trim();
    if (q.length < 2) {
      setResults(null);
      return;
    }
    let cancelled = false;
    const timer = setTimeout(() => {
      void (async () => {
        const [saved, exact] = await Promise.all([
          api.query({ filter: { isSaved: true, text: q }, sort: [{ property: "name", isAscending: true }], position: 0, limit: 8 }),
          looksLikeAddress(q) ? api.byAddress(q) : Promise.resolve(null),
        ]);
        if (cancelled) return;
        const found: Contact[] = [];
        if (exact && exact.ok && exact.value) found.push(exact.value);
        if (saved.ok) for (const c of saved.value.list) if (!found.some((f) => f.id === c.id)) found.push(c);
        setResults(found.filter((c) => c.id !== into.id));
        setActive(0);
        setError(!saved.ok ? contactErrorText(saved.error) : null);
      })();
    }, SEARCH_DELAY_MS);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [text, api, into.id]);

  const choose = (c: Contact): void => {
    setChosen(c);
    setError(null);
  };

  const onKeyDown = (ev: React.KeyboardEvent<HTMLInputElement>): void => {
    if (!results || results.length === 0) return;
    if (ev.key === "ArrowDown") {
      ev.preventDefault();
      setActive((i) => (i + 1) % results.length);
    } else if (ev.key === "ArrowUp") {
      ev.preventDefault();
      setActive((i) => (i - 1 + results.length) % results.length);
    } else if (ev.key === "Enter") {
      ev.preventDefault();
      const c = results[active];
      if (c) choose(c);
    }
  };

  const merge = async (): Promise<void> => {
    if (!chosen) return;
    setBusy(true);
    setError(null);
    const out = await api.merge(into.id, chosen.id);
    setBusy(false);
    if (!out.ok) {
      setError(contactErrorText(out.error));
      return;
    }
    onMerged(out.value, contactLabel(chosen));
  };

  const preview = chosen ? mergePreview(into, chosen) : null;
  const open = results !== null && results.length > 0;

  return (
    <KeyDialog
      title={t("contacts.merge.title", { name: intoName })}
      className="contact-merge"
      busy={busy}
      onClose={onClose}
      initialFocus={inputRef}
    >
      <p>{t("contacts.merge.explain", { name: intoName })}</p>
      <label className="field-label" htmlFor={inputId}>
        {t("contacts.merge.search")}
      </label>
      <input
        id={inputId}
        ref={inputRef}
        className="text-input contact-merge-search"
        type="search"
        autoComplete="off"
        role="combobox"
        aria-expanded={open}
        aria-controls={listId}
        aria-autocomplete="list"
        aria-describedby={hintId}
        aria-activedescendant={open && results[active] ? `${listId}-${active}` : undefined}
        value={text}
        onChange={(ev) => setText(ev.target.value)}
        onKeyDown={onKeyDown}
      />
      <p id={hintId} className="field-hint">
        {t("contacts.merge.searchHint")}
      </p>
      <ul className="contact-merge-results" id={listId} role="listbox" aria-label={t("contacts.merge.results")} hidden={!open}>
        {(results ?? []).map((c, i) => (
          <li
            key={c.id}
            id={`${listId}-${i}`}
            role="option"
            aria-selected={i === active}
            className={`contact-merge-option${i === active ? " is-active" : ""}${chosen?.id === c.id ? " is-chosen" : ""}`}
            onMouseEnter={() => setActive(i)}
            onMouseDown={(ev) => {
              // Before the field's blur, and focus stays in the field.
              ev.preventDefault();
              choose(c);
            }}
          >
            <span className="contact-merge-option-name">{contactIdentity(c).name}</span>
            {contactIdentity(c).address !== contactIdentity(c).name ? (
              <span className="field-hint contact-merge-option-address"> {contactIdentity(c).address}</span>
            ) : null}
            {c.isSaved === false ? <span className="field-hint"> · {t("contacts.merge.unsaved")}</span> : null}
            {c.isBlocked ? <span className="field-hint"> · {t("contacts.chip.blocked")}</span> : null}
          </li>
        ))}
      </ul>
      {results !== null && results.length === 0 ? (
        <p className="field-hint" role="status">
          {t("contacts.merge.noMatch", { text: text.trim() })}
        </p>
      ) : null}

      {chosen && preview ? (
        <section className="contact-merge-preview" aria-label={t("contacts.merge.previewLabel")}>
          <h3>{t("contacts.merge.preview", { from: contactLabel(chosen), name: intoName })}</h3>
          <ul>
            <li>{t("contacts.merge.addresses", { addresses: preview.addresses.join(", ") })}</li>
            <li>{t("contacts.merge.name", { name: preview.name })}</li>
            {preview.keysFrom > 0 ? <li>{tCount("contacts.merge.keys", preview.keysFrom)}</li> : null}
            {preview.notesJoined ? <li>{t("contacts.merge.notesJoined")}</li> : null}
            {preview.vip ? <li>{t("contacts.merge.vip")}</li> : null}
            {preview.saved && chosen.isSaved !== into.isSaved ? <li>{t("contacts.merge.saved")}</li> : null}
            {preview.block ? (
              <li className="contact-merge-block">
                <strong>
                  {t(
                    preview.block.destination === "trash" ? "contacts.merge.block.trash" : "contacts.merge.block.junk",
                    {
                      who:
                        preview.block.from === "both"
                          ? t("contacts.merge.block.both")
                          : contactLabel(preview.block.from === "into" ? into : chosen),
                    },
                  )}
                </strong>
              </li>
            ) : null}
          </ul>
          <p className="field-hint">{t("contacts.merge.undo")}</p>
          {preview.notesTooLong ? (
            <p className="field-error" role="alert">
              {t("contacts.error.notesTooLong")}
            </p>
          ) : null}
        </section>
      ) : null}

      {error ? (
        <p className="field-error" role="alert">
          {error}
        </p>
      ) : null}
      {busy ? (
        <p className="field-hint" role="status">
          {t("contacts.merge.busy")}
        </p>
      ) : null}
      <div className="sheet-actions">
        <button type="button" className="button" disabled={busy} onClick={onClose}>
          {t("action.cancel")}
        </button>
        <button
          type="button"
          className="button button-primary contact-merge-submit"
          disabled={busy || !chosen || preview?.notesTooLong === true}
          onClick={() => void merge()}
        >
          {chosen ? t("contacts.merge.submitWith", { from: contactLabel(chosen) }) : t("contacts.merge.submit")}
        </button>
      </div>
    </KeyDialog>
  );
}
