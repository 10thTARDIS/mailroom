// SPDX-License-Identifier: AGPL-3.0-only

// ONE CONTACT (#816): who they are to you, how to reach them, what you hold for
// them, and what you have said to each other.
//
//   * THE NAME is the server's `name` (your name for them, else the one their
//     mail gives). "Edit name" sets YOUR name, which their next email never
//     overwrites; with one set, the learned name is shown small beside it with
//     "Use theirs", which clears yours.
//   * ADDRESSES, each with the alias of yours it writes to.
//   * COUNTS AND DATES are statistics the server does not sync (ADR-041 §3), so
//     the contact is READ FRESH when the page opens rather than taken from the
//     list. A date is shown only when there is one.
//   * VIP, BLOCK, REMOVE. Remove un-saves and deletes nothing. The server KEEPS
//     VIP when a VIP is un-saved (captured: store.SetContactMarkers), so the
//     confirm says that — they stay a VIP, under the VIP filter — rather than
//     promising a removal the server does not make.
//   * NOTES, capped at the server's 4,000 characters, counted as it counts them.
//   * THE KEY: Settings → Keys' own component, narrowed to this contact.
//   * RECENT MAIL from or to ANY of the addresses, through the search path.
//   * MERGE WITH… and SEPARATE (the second only with two or more addresses).
//
// The page works for an UNSAVED contact too (a deep link from Settings → Keys,
// the sender card): it says so and offers "Add to contacts".

import { useCallback, useEffect, useId, useRef, useState } from "react";
import type { JSX } from "react";

import {
  MAX_NOTES,
  MAX_USER_NAME,
  cleanContactText,
  contactIdentity,
  contactLabel,
  learnedNameBeside,
  notesLength,
  separateOutcome,
  userNameLength,
} from "../../../core/contacts";
import type { ContactError } from "../../../core/contacts";
import type { BlockDestination, Contact, Id } from "../../../core/jmap/types";
import { contactsListOf, formatRoute, viewRoute, withM } from "../../../core/routes";
import type { ContactsRoute } from "../../../core/routes";
import { senderIdentity } from "../../../core/hostileText";
import { keyDateLong } from "../../../core/userKeys";
import { leaveTo, navigate } from "../../routing";
import { useMailState } from "../../state";
import { t, tCount } from "../../i18n";
import { useKeysApi } from "../settings/keys/api";
import { CorrespondentKeys } from "../settings/keys/CorrespondentKeys";
import { KeyConfirmDialog } from "../settings/keys/KeyDialog";
import type { ContactsApi, RecentMail } from "./api";
import { MergeDialog } from "./MergeDialog";
import { SeparateDialog } from "./SeparateDialog";
import { contactErrorText, separateOutcomeText } from "./text";

type Reading =
  | { kind: "asking" }
  | { kind: "missing" }
  | { kind: "failed"; error: ContactError }
  | { kind: "loaded"; contact: Contact };

type Recent = { kind: "asking" } | { kind: "failed"; error: ContactError } | { kind: "loaded"; mail: RecentMail };

function plainClick(ev: React.MouseEvent): boolean {
  return ev.button === 0 && !ev.metaKey && !ev.ctrlKey && !ev.shiftKey && !ev.altKey;
}

export function ContactPage({
  api,
  id,
  route,
  onChanged,
  onMessage,
}: {
  api: ContactsApi;
  id: Id;
  route: ContactsRoute;
  /** A change landed: the list re-reads. */
  onChanged: () => void;
  /** Say this (a toast). */
  onMessage: (message: string) => void;
}): JSX.Element {
  const [reading, setReading] = useState<Reading>({ kind: "asking" });
  const [tick, setTick] = useState(0);
  const reload = useCallback(() => setTick((n) => n + 1), []);
  const listRoute = contactsListOf(route);

  useEffect(() => {
    let cancelled = false;
    void api.get(id).then((out) => {
      if (cancelled) return;
      if (!out.ok) setReading({ kind: "failed", error: out.error });
      else setReading(out.value ? { kind: "loaded", contact: out.value } : { kind: "missing" });
    });
    return () => {
      cancelled = true;
    };
  }, [api, id, tick]);

  const back = (
    <button type="button" className="button button-quiet contact-back" onClick={() => leaveTo(listRoute)}>
      <span aria-hidden="true">← </span>
      {t("contacts.back")}
    </button>
  );

  return (
    <section className="contact-page" aria-label={reading.kind === "loaded" ? contactLabel(reading.contact) : t("contacts.page")}>
      {back}
      {reading.kind === "asking" ? (
        <p className="field-hint" role="status">
          {t("contacts.page.loading")}
        </p>
      ) : reading.kind === "missing" ? (
        <p className="field-hint">{t("contacts.page.missing")}</p>
      ) : reading.kind === "failed" ? (
        <p className="field-error" role="alert">
          {contactErrorText(reading.error)}
        </p>
      ) : (
        <ContactBody
          key={reading.contact.id}
          api={api}
          contact={reading.contact}
          listRoute={listRoute}
          reload={reload}
          onChanged={() => {
            reload();
            onChanged();
          }}
          onMessage={onMessage}
        />
      )}
    </section>
  );
}

function ContactBody({
  api,
  contact: c,
  listRoute,
  reload,
  onChanged,
  onMessage,
}: {
  api: ContactsApi;
  contact: Contact;
  listRoute: ContactsRoute;
  reload: () => void;
  onChanged: () => void;
  onMessage: (message: string) => void;
}): JSX.Element {
  const keysApi = useKeysApi();
  const state = useMailState();
  // Every string of theirs through the one formatter (core/contacts.ts):
  // `name` is the one-line form for sentences, `identity` the heading's parts.
  const identity = contactIdentity(c);
  const name = contactLabel(c);
  const learned = learnedNameBeside(c);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [nameDraft, setNameDraft] = useState("");
  const [nameError, setNameError] = useState<string | null>(null);
  const [notes, setNotes] = useState(c.notes ?? "");
  const [notesError, setNotesError] = useState<string | null>(null);
  const [blockForm, setBlockForm] = useState<BlockDestination | null>(null);
  const [dialog, setDialog] = useState<"remove" | "merge" | "separate" | null>(null);
  const [separated, setSeparated] = useState<{ id: Id; address: string }[]>([]);
  // What the separate did to this contact (#834): restored, with the edits
  // kept, or left as it was.
  const [separatedNote, setSeparatedNote] = useState<string | null>(null);
  const [recent, setRecent] = useState<Recent>({ kind: "asking" });
  const nameInput = useRef<HTMLInputElement | null>(null);
  const editButton = useRef<HTMLButtonElement | null>(null);
  const nameId = useId();
  const notesId = useId();
  const notesCountId = useId();
  const headingId = useId();
  const savedNotes = c.notes ?? "";
  const notesCount = notesLength(notes);
  const notesOver = notesCount > MAX_NOTES;
  const notesDirty = notes !== savedNotes;
  const addressesKey = c.addresses.map((a) => a.address).join("\n");

  useEffect(() => {
    let cancelled = false;
    setRecent({ kind: "asking" });
    void api.recentMail(c.addresses).then((out) => {
      if (!cancelled) setRecent(out.ok ? { kind: "loaded", mail: out.value } : { kind: "failed", error: out.error });
    });
    return () => {
      cancelled = true;
    };
    // The addresses' text is the dependency: a re-read that changes nothing
    // must not run the search again.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api, addressesKey]);

  useEffect(() => {
    if (editing) nameInput.current?.focus();
  }, [editing]);

  // A fresh read replaces the field with what the server STORED (trimmed, line
  // breaks normalised), so a saved note is not left looking unsaved.
  useEffect(() => setNotes(savedNotes), [savedNotes]);

  /** One Contact/set, then a fresh read. Resolves to whether it landed. */
  const write = async (patch: Parameters<ContactsApi["update"]>[1], done: string | null): Promise<boolean> => {
    setBusy(true);
    setError(null);
    const out = await api.update(c.id, patch);
    setBusy(false);
    if (!out.ok) {
      setError(contactErrorText(out.error));
      return false;
    }
    if (done) onMessage(done);
    onChanged();
    return true;
  };

  const saveName = async (): Promise<void> => {
    const draft = nameDraft.trim();
    if (userNameLength(draft) > MAX_USER_NAME) {
      setNameError(t("contacts.error.userNameTooLong"));
      return;
    }
    setNameError(null);
    setBusy(true);
    const out = await api.update(c.id, { userName: draft === "" ? null : draft });
    setBusy(false);
    if (!out.ok) {
      setNameError(contactErrorText(out.error));
      return;
    }
    setEditing(false);
    onMessage(draft === "" ? t("contacts.name.cleared") : t("contacts.name.saved", { name: draft }));
    onChanged();
  };

  const saveNotes = async (): Promise<void> => {
    if (notesOver) return;
    setNotesError(null);
    setBusy(true);
    const out = await api.update(c.id, { notes });
    setBusy(false);
    if (!out.ok) {
      setNotesError(contactErrorText(out.error));
      return;
    }
    onMessage(t("contacts.notes.saved"));
    onChanged();
  };

  // Escape inside a field leaves the field, not the page: a half-written note
  // must not be thrown away by the key that closes things (the shell's Escape
  // would otherwise peel this route).
  const onKeyDown = (ev: React.KeyboardEvent): void => {
    if (ev.key !== "Escape") return;
    const el = ev.target as HTMLElement;
    if (el.tagName === "TEXTAREA" || el.tagName === "INPUT") {
      ev.stopPropagation();
      if (editing && el === nameInput.current) {
        setEditing(false);
        editButton.current?.focus();
        return;
      }
      el.blur();
    }
  };

  const saved = c.isSaved === true;
  const recentLink = recent.kind === "loaded" && recent.mail.query !== "" ? viewRoute({ kind: "search", query: recent.mail.query }) : null;

  return (
    <div className="contact-body" onKeyDown={onKeyDown}>
      <header className="contact-head">
        <h1 id={headingId} className="contact-name">
          {identity.name}
        </h1>
        {identity.showAddress ? <p className="contact-real-address">{identity.address}</p> : null}
        {learned ? (
          <p className="contact-learned">
            {t("contacts.name.theirs", { name: learned })}{" "}
            <button
              type="button"
              className="link-button"
              disabled={busy}
              onClick={() => void write({ userName: null }, t("contacts.name.cleared"))}
            >
              {t("contacts.name.useTheirs")}
            </button>
          </p>
        ) : null}
        {!saved ? <p className="field-hint">{t("contacts.notSaved")}</p> : null}
        {editing ? (
          <form
            className="contact-name-form"
            onSubmit={(ev) => {
              ev.preventDefault();
              void saveName();
            }}
          >
            <label className="field-label" htmlFor={nameId}>
              {t("contacts.name.label")}
            </label>
            <input
              id={nameId}
              ref={nameInput}
              className="text-input"
              autoComplete="off"
              value={nameDraft}
              aria-describedby={`${nameId}-hint`}
              onChange={(ev) => setNameDraft(ev.target.value)}
            />
            <p id={`${nameId}-hint`} className="field-hint">
              {t("contacts.name.hint")}
            </p>
            {nameError ? (
              <p className="field-error" role="alert">
                {nameError}
              </p>
            ) : null}
            <div className="sheet-actions">
              <button
                type="button"
                className="button"
                onClick={() => {
                  setEditing(false);
                  setNameError(null);
                }}
              >
                {t("action.cancel")}
              </button>
              <button type="submit" className="button button-primary" disabled={busy}>
                {t("contacts.name.save")}
              </button>
            </div>
          </form>
        ) : (
          <p>
            <button
              type="button"
              ref={editButton}
              className="button button-quiet contact-edit-name"
              onClick={() => {
                setNameDraft(c.userName ?? "");
                setNameError(null);
                setEditing(true);
              }}
            >
              {t("contacts.name.edit")}
            </button>
          </p>
        )}
      </header>

      <section className="contact-section" aria-labelledby={`${headingId}-addresses`}>
        <h2 id={`${headingId}-addresses`} className="contact-section-heading">
          {t("contacts.addresses")}
        </h2>
        <ul className="contact-addresses">
          {c.addresses.map((a) => (
            <li key={a.id ?? a.address}>
              <span className="contact-address">{cleanContactText(a.address)}</span>
              {a.reachedMeAt ? (
                <span className="field-hint"> {t("contacts.writesTo", { alias: cleanContactText(a.reachedMeAt) })}</span>
              ) : null}
            </li>
          ))}
        </ul>
        {separated.length > 0 ? (
          <div className="contact-separated" role="status">
            <p>{t("contacts.separate.done")}</p>
            {separatedNote ? <p className="contact-separated-outcome">{separatedNote}</p> : null}
            <ul>
              {separated.map((s) => (
                <li key={s.id}>
                  <a
                    href={formatRoute({ ...listRoute, id: s.id })}
                    onClick={(ev) => {
                      if (!plainClick(ev)) return;
                      ev.preventDefault();
                      navigate({ ...listRoute, id: s.id });
                    }}
                  >
                    {t("contacts.separate.open", { address: s.address })}
                  </a>
                </li>
              ))}
            </ul>
          </div>
        ) : null}
      </section>

      <section className="contact-section" aria-labelledby={`${headingId}-activity`}>
        <h2 id={`${headingId}-activity`} className="contact-section-heading">
          {t("contacts.activity")}
        </h2>
        {typeof c.outboundCount === "number" && typeof c.inboundCount === "number" ? (
          <p className="contact-counts">
            {c.outboundCount === 0 ? t("contacts.counts.out.none") : tCount("contacts.counts.out", c.outboundCount)}
            <span aria-hidden="true"> · </span>
            {tCount("contacts.counts.in", c.inboundCount)}
          </p>
        ) : null}
        {c.firstMailAt || c.lastMailAt ? (
          <p className="contact-dates">
            {c.firstMailAt ? <span>{t("contacts.firstMail", { date: keyDateLong(c.firstMailAt) })}</span> : null}
            {c.firstMailAt && c.lastMailAt ? <span aria-hidden="true"> · </span> : null}
            {c.lastMailAt ? <span>{t("contacts.lastMail", { date: keyDateLong(c.lastMailAt) })}</span> : null}
          </p>
        ) : null}
      </section>

      <div className="contact-actions" role="group" aria-label={t("contacts.actionsFor", { name })}>
        <button
          type="button"
          className="button contact-vip"
          aria-pressed={c.isVip === true}
          disabled={busy}
          onClick={() =>
            void write(
              { isVip: c.isVip !== true },
              c.isVip ? t("contacts.vip.off", { name }) : t("contacts.vip.on", { name }),
            )
          }
        >
          {t("contacts.vip")}
        </button>
        {c.isBlocked ? (
          <button
            type="button"
            className="button"
            disabled={busy}
            onClick={() => void write({ isBlocked: false }, t("contacts.unblocked", { name }))}
          >
            {t("contacts.unblock")}
          </button>
        ) : (
          <button
            type="button"
            className="button"
            aria-expanded={blockForm !== null}
            disabled={busy}
            onClick={() => setBlockForm((f) => (f ? null : (c.blockDestination ?? "junk")))}
          >
            {t("contacts.block.open")}
          </button>
        )}
        {saved ? (
          <button type="button" className="button contact-remove" disabled={busy} onClick={() => setDialog("remove")}>
            {t("contacts.remove")}
          </button>
        ) : (
          <button
            type="button"
            className="button button-primary contact-save"
            disabled={busy}
            onClick={() => void write({ isSaved: true }, t("contacts.added", { name }))}
          >
            {t("contacts.addTo")}
          </button>
        )}
        <button type="button" className="button contact-merge-open" disabled={busy} onClick={() => setDialog("merge")}>
          {t("contacts.merge.open")}
        </button>
        {c.addresses.length >= 2 ? (
          <button
            type="button"
            className="button contact-separate-open"
            disabled={busy}
            onClick={() => setDialog("separate")}
          >
            {t("contacts.separate.open.button")}
          </button>
        ) : null}
      </div>
      {c.isBlocked ? (
        <p className="contact-blocked">
          {t(c.blockDestination === "trash" ? "contacts.isBlocked.trash" : "contacts.isBlocked.junk")}
        </p>
      ) : null}
      {blockForm && !c.isBlocked ? (
        <form
          className="contact-block-form"
          aria-label={t("contacts.block.formLabel", { name })}
          onSubmit={(ev) => {
            ev.preventDefault();
            void write({ isBlocked: true, blockDestination: blockForm }, t("contacts.blocked", { name })).then((ok) => {
              if (ok) setBlockForm(null);
            });
          }}
        >
          <fieldset>
            <legend>{t("contacts.block.where")}</legend>
            {(["junk", "trash"] as const).map((d) => (
              <label key={d} className="radio">
                <input
                  type="radio"
                  name={`contact-block-${c.id}`}
                  value={d}
                  checked={blockForm === d}
                  onChange={() => setBlockForm(d)}
                />
                {t(d === "junk" ? "senders.block.toJunk" : "senders.block.toTrash")}
              </label>
            ))}
          </fieldset>
          <p className="field-hint">{t("contacts.block.explain")}</p>
          <div className="sheet-actions">
            <button type="button" className="button" onClick={() => setBlockForm(null)}>
              {t("action.cancel")}
            </button>
            <button type="submit" className="button button-primary" disabled={busy}>
              {t("senders.block.go")}
            </button>
          </div>
        </form>
      ) : null}
      {error ? (
        <p className="field-error" role="alert">
          {error}
        </p>
      ) : null}

      <section className="contact-section" aria-labelledby={`${headingId}-notes`}>
        <h2 id={`${headingId}-notes`} className="contact-section-heading">
          <label htmlFor={notesId}>{t("contacts.notes")}</label>
        </h2>
        <textarea
          id={notesId}
          className="textarea contact-notes"
          rows={4}
          value={notes}
          aria-describedby={notesCountId}
          aria-invalid={notesOver || undefined}
          onChange={(ev) => setNotes(ev.target.value)}
        />
        <p id={notesCountId} className={notesOver ? "field-error contact-notes-count" : "field-hint contact-notes-count"}>
          {notesOver
            ? t("contacts.notes.over", { count: notesCount.toLocaleString("en-US"), max: MAX_NOTES.toLocaleString("en-US") })
            : t("contacts.notes.count", { count: notesCount.toLocaleString("en-US"), max: MAX_NOTES.toLocaleString("en-US") })}
        </p>
        {notesError ? (
          <p className="field-error" role="alert">
            {notesError}
          </p>
        ) : null}
        <div className="sheet-actions">
          <button
            type="button"
            className="button"
            disabled={!notesDirty || busy}
            onClick={() => {
              setNotes(savedNotes);
              setNotesError(null);
            }}
          >
            {t("action.cancel")}
          </button>
          <button
            type="button"
            className="button button-primary contact-notes-save"
            disabled={!notesDirty || notesOver || busy}
            onClick={() => void saveNotes()}
          >
            {t("contacts.notes.save")}
          </button>
        </div>
      </section>

      <div className="contact-section contact-keys">
        <CorrespondentKeys
          // Keyed on the addresses: a merge or separate moves keys between
          // contacts, and the list is read again then.
          key={`${c.id}:${addressesKey}`}
          api={keysApi}
          onChanged={(m) => {
            onMessage(m);
            reload();
          }}
          contact={{ id: c.id, addresses: c.addresses.map((a) => a.address) }}
        />
      </div>

      <section className="contact-section" aria-labelledby={`${headingId}-recent`}>
        <h2 id={`${headingId}-recent`} className="contact-section-heading">
          {t("contacts.recent")}
        </h2>
        {recent.kind === "asking" ? (
          <p className="field-hint" role="status">
            {t("contacts.recent.loading")}
          </p>
        ) : recent.kind === "failed" ? (
          <p className="field-hint">{contactErrorText(recent.error)}</p>
        ) : recent.mail.ids.length === 0 ? (
          <p className="field-hint">{recent.mail.query === "" ? t("contacts.recent.cannot") : t("contacts.recent.none")}</p>
        ) : (
          <>
            <ul className="contact-recent">
              {recent.mail.ids.map((mid) => {
                const e = state.emails[mid];
                const from = senderIdentity(e?.from?.[0]);
                const route = recentLink ? withM(recentLink, mid) : null;
                return (
                  <li key={mid}>
                    {route ? (
                      <a
                        href={formatRoute(route)}
                        onClick={(ev) => {
                          if (!plainClick(ev)) return;
                          ev.preventDefault();
                          navigate(route);
                        }}
                      >
                        <span className="contact-recent-subject">{e?.subject || t("contacts.recent.noSubject")}</span>
                      </a>
                    ) : null}
                    <span className="field-hint contact-recent-meta">
                      {" "}
                      {from.name || from.address}
                      {e?.receivedAt ? ` · ${keyDateLong(e.receivedAt)}` : ""}
                    </span>
                  </li>
                );
              })}
            </ul>
            {recent.mail.total > recent.mail.ids.length ? (
              <p className="field-hint">
                {t("contacts.recent.bound", { shown: recent.mail.ids.length, total: recent.mail.total })}
              </p>
            ) : null}
          </>
        )}
        {recent.kind === "loaded" && recent.mail.skipped.length > 0 ? (
          <p className="field-hint">{t("contacts.recent.skipped", { addresses: recent.mail.skipped.join(", ") })}</p>
        ) : null}
        {recentLink ? (
          <p>
            <a
              href={formatRoute(recentLink)}
              className="contact-recent-all"
              onClick={(ev) => {
                if (!plainClick(ev)) return;
                ev.preventDefault();
                navigate(recentLink);
              }}
            >
              {t("contacts.recent.all", { name })}
            </a>
          </p>
        ) : null}
      </section>

      {dialog === "remove" ? (
        <RemoveDialog
          contact={c}
          busy={busy}
          error={error}
          onClose={() => setDialog(null)}
          onConfirm={() =>
            void write({ isSaved: false }, t("contacts.removed", { name })).then((ok) => {
              if (ok) setDialog(null);
            })
          }
        />
      ) : null}
      {dialog === "merge" ? (
        <MergeDialog
          api={api}
          into={c}
          onClose={() => setDialog(null)}
          onMerged={(result, fromName) => {
            setDialog(null);
            onMessage(t("contacts.merge.done", { from: fromName, name }));
            onChanged();
            // The merged contact is `into`, this one: re-read it where it is.
            navigate({ ...listRoute, id: result.id }, { replace: true });
          }}
        />
      ) : null}
      {dialog === "separate" ? (
        <SeparateDialog
          api={api}
          contact={c}
          onClose={() => setDialog(null)}
          onSeparated={(result) => {
            setDialog(null);
            const byContact = Object.entries(result.addressContacts).map(([addressId, contactId]) => ({
              id: contactId,
              address: cleanContactText(c.addresses.find((a) => a.id === addressId)?.address),
            }));
            setSeparated(byContact);
            setSeparatedNote(separateOutcomeText(separateOutcome(result)));
            onMessage(tCount("contacts.separate.toast", byContact.length));
            onChanged();
          }}
        />
      ) : null}
    </div>
  );
}

/** Remove from contacts: un-save, which deletes nothing. What the server does
 * with a VIP is said, not guessed: it stays a VIP. */
function RemoveDialog({
  contact,
  busy,
  error,
  onClose,
  onConfirm,
}: {
  contact: Contact;
  busy: boolean;
  error: string | null;
  onClose: () => void;
  onConfirm: () => void;
}): JSX.Element {
  const name = contactLabel(contact);
  return (
    <KeyConfirmDialog
      title={t("contacts.remove.title", { name })}
      paragraphs={[
        t("contacts.remove.nothingDeleted"),
        ...(contact.isVip ? [t("contacts.remove.staysVip", { name })] : []),
        t("contacts.remove.again"),
      ]}
      confirmLabel={t("contacts.remove.submit")}
      busyLabel={t("contacts.remove.busy")}
      busy={busy}
      error={error}
      onClose={onClose}
      onConfirm={onConfirm}
    />
  );
}
