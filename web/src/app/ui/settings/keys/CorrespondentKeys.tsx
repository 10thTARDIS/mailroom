// SPDX-License-Identifier: AGPL-3.0-only

// THE KEYS YOU HOLD FOR OTHER PEOPLE (#796): who each belongs to, where it came
// from, whether it is the one encrypted mail to them uses, and when it stops
// (each one is a CorrespondentKeyCard). Remove asks first and says what removing
// does; Import uses the same dialog compose and the signature line use
// (ImportKeyDialog), after asking whose key it is.
//
// REMOVING IS NOT FOREVER (#815, ADR-040 §11). A removed key is kept, with its
// history, under History, and comes back if they send or publish it again. The
// Remove confirm says so, and points at "Don't use this key", which is what
// stops a key for good (a refused key stays refused when it arrives again).
//
// Read when the row opens (ContactKey/get with includeRemoved), and again after
// a change. A server that cannot answer is said so, never shown as "no keys"
// (#176).
//
// THE SAME COMPONENT ON A CONTACT'S PAGE (#816). With `contact`, it shows only
// that contact's keys (held, and removed under History), says "Key" rather than
// "Correspondents' keys", and imports for the contact's own address. One
// component, so the card, its refresh, "Don't use this key", Remove and the
// history cannot differ between the two places. In Settings each card links to
// its contact's page instead (`onOpenContact`).

import { useCallback, useEffect, useId, useState } from "react";
import type { JSX } from "react";

import { decideAddress, splitRemoved } from "../../../../core/keyLifecycle";
import { correspondentRows } from "../../../../core/userKeys";
import type { CorrespondentKeyRow, KeyActionError } from "../../../../core/userKeys";
import type { Contact, Id } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import { ImportKeyDialog } from "../../ImportKeyDialog";
import type { KeysApi } from "./api";
import { CorrespondentKeyCard, correspondentKeyName } from "./CorrespondentKeyCard";
import { HistoryDisclosure } from "./HistoryDisclosure";
import { KeyConfirmDialog, KeyPortal } from "./KeyDialog";
import { keyErrorText } from "./text";

type Reading =
  | { kind: "asking" }
  | { kind: "unavailable" }
  | { kind: "loaded"; held: CorrespondentKeyRow[]; removed: CorrespondentKeyRow[]; contacts: Contact[] };

function toReading(r: Awaited<ReturnType<KeysApi["readCorrespondents"]>>, contactId: Id | null): Reading {
  if (r === null) return { kind: "unavailable" };
  const keys = contactId === null ? r.keys : r.keys.filter((k) => k.contactId === contactId);
  const { held, removed } = splitRemoved(correspondentRows(keys, r.contacts));
  return { kind: "loaded", held, removed, contacts: r.contacts };
}

export function CorrespondentKeys({
  api,
  onChanged,
  contact,
  onOpenContact,
}: {
  api: KeysApi;
  onChanged: (message: string) => void;
  /** On a contact's page: only this contact's keys. */
  contact?: { id: Id; addresses: readonly string[] };
  /** In Settings: each card links to its contact's page. */
  onOpenContact?: (contactId: Id) => { href: string; open: () => void };
}): JSX.Element {
  const contactId = contact?.id ?? null;
  const [reading, setReading] = useState<Reading>({ kind: "asking" });
  const [confirm, setConfirm] = useState<{ kind: "remove" | "refuse"; row: CorrespondentKeyRow } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<KeyActionError | null>(null);
  const [importAddress, setImportAddress] = useState("");
  const [importFor, setImportFor] = useState<string | null>(null);
  const [historyOpen, setHistoryOpen] = useState(false);
  const addressId = useId();
  const headingId = useId();

  const load = useCallback(async (): Promise<void> => {
    setReading(toReading(await api.readCorrespondents(), contactId));
  }, [api, contactId]);

  useEffect(() => {
    let cancelled = false;
    void api.readCorrespondents().then((r) => {
      if (!cancelled) setReading(toReading(r, contactId));
    });
    return () => {
      cancelled = true;
    };
  }, [api, contactId]);

  const contactAddresses = (row: CorrespondentKeyRow): string[] => {
    if (contact && row.key.contactId === contact.id) return [...contact.addresses];
    if (reading.kind !== "loaded") return [];
    const c = reading.contacts.find((x) => x.id === row.key.contactId);
    return c ? c.addresses.map((a) => a.address) : [];
  };

  const changed = (message: string): void => {
    onChanged(message);
    void load();
  };

  const act = async (kind: "remove" | "refuse", row: CorrespondentKeyRow): Promise<void> => {
    setBusy(true);
    setError(null);
    const name = correspondentKeyName(row);
    let out;
    if (kind === "remove") {
      out = await api.removeCorrespondentKey(row.key.id);
    } else {
      const address = decideAddress(row.key, contactAddresses(row));
      out = address
        ? await api.decideCorrespondentKey(address, row.key.fingerprint, "keep")
        : ({ ok: false, error: "notFound" } as const);
    }
    setBusy(false);
    if (!out.ok) {
      setError(out.error);
      return;
    }
    setConfirm(null);
    changed(kind === "remove" ? t("keys.done.removed", { name }) : t("keys.done.refused", { name }));
  };

  const startImport = (): void => {
    const a = importAddress.trim();
    if (a === "" || !a.includes("@")) return;
    setImportFor(a);
  };

  const card = (row: CorrespondentKeyRow): JSX.Element => (
    <CorrespondentKeyCard
      key={row.key.id}
      row={row}
      contactAddresses={contactAddresses(row)}
      api={api}
      onChanged={changed}
      onReload={() => void load()}
      onRemove={(r) => {
        setError(null);
        setConfirm({ kind: "remove", row: r });
      }}
      onRefuse={(r) => {
        setError(null);
        setConfirm({ kind: "refuse", row: r });
      }}
      {...(onOpenContact ? { contactLink: onOpenContact(row.key.contactId) } : {})}
    />
  );

  const confirmName = confirm ? correspondentKeyName(confirm.row) : "";

  return (
    <section className="correspondent-keys" aria-labelledby={headingId}>
      {contact ? (
        <h2 id={headingId} className="contact-section-heading">
          {t("contacts.key.heading")}
        </h2>
      ) : (
        <h4 id={headingId}>{t("keys.theirs.heading")}</h4>
      )}
      {contact ? null : <p className="field-hint">{t("keys.theirs.explain")}</p>}
      {reading.kind === "asking" ? (
        <p className="field-hint">{t("settings.keys.asking")}</p>
      ) : reading.kind === "unavailable" ? (
        <p className="field-hint">{t("keys.theirs.unavailable")}</p>
      ) : reading.held.length === 0 ? (
        <p className="field-hint">{contact ? t("contacts.key.none") : t("keys.theirs.none")}</p>
      ) : (
        <ul className="correspondent-key-list">{reading.held.map(card)}</ul>
      )}

      {contact ? (
        contact.addresses[0] ? (
          <p>
            <button
              type="button"
              className="button button-quiet"
              onClick={() => setImportFor(contact.addresses[0] as string)}
            >
              {t("contacts.key.import")}
            </button>
          </p>
        ) : null
      ) : (
      <form
        className="correspondent-key-import"
        onSubmit={(ev) => {
          ev.preventDefault();
          startImport();
        }}
      >
        <label htmlFor={addressId} className="field-label">
          {t("keys.theirs.importAddress")}
        </label>
        <div className="correspondent-key-import-row">
          <input
            id={addressId}
            className="text-input"
            type="email"
            autoComplete="off"
            value={importAddress}
            onChange={(ev) => setImportAddress(ev.target.value)}
          />
          <button type="submit" className="button" disabled={!importAddress.trim().includes("@")}>
            {t("keys.theirs.import")}
          </button>
        </div>
      </form>
      )}

      {reading.kind === "loaded" ? (
        <HistoryDisclosure
          open={historyOpen}
          onToggle={setHistoryOpen}
          count={reading.removed.length}
          label={contact ? t("contacts.key.historyLabel") : t("keys.history.theirsLabel")}
        >
          <p className="field-hint">{t("keys.history.theirsExplain")}</p>
          {reading.removed.length === 0 ? (
            <p className="field-hint">{t("keys.history.theirsNone")}</p>
          ) : (
            <ul className="correspondent-key-list">{reading.removed.map(card)}</ul>
          )}
        </HistoryDisclosure>
      ) : null}

      {confirm?.kind === "remove" ? (
        <KeyConfirmDialog
          title={t("keys.theirs.removeTitle", { name: confirmName })}
          paragraphs={[
            t("keys.theirs.removeWhat", { name: confirmName }),
            ...(confirm.row.key.pinned ? [t("keys.theirs.removePinned")] : []),
            t("keys.theirs.removeRevives"),
          ]}
          confirmLabel={t("keys.theirs.removeSubmit")}
          busyLabel={t("keys.theirs.removing")}
          busy={busy}
          error={error ? keyErrorText(error) : null}
          onClose={() => setConfirm(null)}
          onConfirm={() => void act("remove", confirm.row)}
        />
      ) : null}
      {confirm?.kind === "refuse" ? (
        <KeyConfirmDialog
          title={t("keys.refuse.title", { name: confirmName })}
          paragraphs={[t("keys.refuse.what"), t("keys.refuse.undo")]}
          confirmLabel={t("keys.refuse.submit")}
          busyLabel={t("keys.refuse.busy")}
          busy={busy}
          error={error ? keyErrorText(error) : null}
          onClose={() => setConfirm(null)}
          onConfirm={() => void act("refuse", confirm.row)}
        />
      ) : null}
      {importFor !== null ? (
        <KeyPortal>
          <ImportKeyDialog
            address={importFor}
            check={(key) => api.checkContactKey(importFor, key)}
            onImport={(key) => api.importContactKey(importFor, key)}
            onImported={() => {
              setImportAddress("");
              onChanged(t("keys.done.importedFor", { address: importFor }));
              void load();
            }}
            onClose={() => setImportFor(null)}
          />
        </KeyPortal>
      ) : null}
    </section>
  );
}
