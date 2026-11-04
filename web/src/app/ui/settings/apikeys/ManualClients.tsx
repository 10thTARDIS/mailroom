// SPDX-License-Identifier: AGPL-3.0-only

// APPS YOU REGISTER YOURSELF (ADR-050 section 4, manual registration).
//
// The fallback for a tool that cannot identify itself: the person gives the app
// a name and the addresses it returns to, and pastes the client ID this shows
// into the tool. The ID is NOT a secret (a client ID names an app, and PKCE, not
// the ID, proves who is finishing a sign-in), so unlike a key's token it is
// shown again whenever the list is open.
//
// Return addresses are checked here by the server's own rule (model.ts
// checkReturnAddress) so a mistake is named before anything is sent; the server
// checks again. The list reads when the section opens and after each change.

import { useEffect, useId, useState } from "react";
import type { JSX } from "react";

import type { OAuthClient } from "../../../../core/jmap/types";
import { keyDateLong } from "../../../../core/userKeys";
import { t } from "../../../i18n";
import { KeyConfirmDialog } from "../keys/KeyDialog";
import type { ApiKeysApi } from "./api";
import { buildClient, clientProblemText, keyRefusalText } from "./model";
import { CopyButton } from "./SetupGuide";
import { Untrusted } from "./Untrusted";

type Reading = { kind: "asking" } | { kind: "unavailable" } | { kind: "loaded"; list: OAuthClient[] };

export function ManualClients({ api }: { api: ApiKeysApi }): JSX.Element {
  const [reading, setReading] = useState<Reading>({ kind: "asking" });
  const [generation, setGeneration] = useState(0);
  const [name, setName] = useState("");
  const [addresses, setAddresses] = useState("");
  const [problem, setProblem] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<OAuthClient | null>(null);
  const [deleting, setDeleting] = useState<OAuthClient | null>(null);
  const [done, setDone] = useState("");
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const ids = { name: useId(), uris: useId(), hint: useId() };

  useEffect(() => {
    let cancelled = false;
    void api.clients().then((list) => {
      if (!cancelled) setReading(list === null ? { kind: "unavailable" } : { kind: "loaded", list });
    });
    return () => {
      cancelled = true;
    };
  }, [api, generation]);

  const submit = async (): Promise<void> => {
    const built = buildClient(name, addresses);
    if (!built.ok) {
      setProblem(clientProblemText(built));
      return;
    }
    setProblem(null);
    setBusy(true);
    const out = await api.createClient(built.name, built.redirectUris);
    setBusy(false);
    if (!out.ok) {
      setProblem(keyRefusalText(out.error));
      return;
    }
    setCreated(out.client);
    setDone("");
    setName("");
    setAddresses("");
    setGeneration((g) => g + 1);
  };

  const remove = async (c: OAuthClient): Promise<void> => {
    setBusy(true);
    setDeleteError(null);
    const out = await api.deleteClient(c.id);
    setBusy(false);
    if (!out.ok) {
      setDeleteError(keyRefusalText(out.error));
      return;
    }
    setDeleting(null);
    if (created?.id === c.id) setCreated(null);
    setDone(t("apiKeys.clients.deleted"));
    setGeneration((g) => g + 1);
  };

  return (
    <div className="apikey-clients">
      <h4>{t("apiKeys.clients.heading")}</h4>
      <p className="field-hint">{t("apiKeys.clients.intro")}</p>

      <fieldset className="key-fieldset apikey-client-form">
        <legend>{t("apiKeys.clients.submit")}</legend>
        <label htmlFor={ids.name} className="field-label">
          {t("apiKeys.clients.name")}
        </label>
        <input
          id={ids.name}
          className="text-input"
          type="text"
          maxLength={100}
          value={name}
          disabled={busy}
          onChange={(e) => setName(e.target.value)}
        />
        <label htmlFor={ids.uris} className="field-label">
          {t("apiKeys.clients.uris")}
        </label>
        <textarea
          id={ids.uris}
          className="text-input apikey-client-uris"
          rows={3}
          spellCheck={false}
          autoComplete="off"
          aria-describedby={ids.hint}
          value={addresses}
          disabled={busy}
          onChange={(e) => setAddresses(e.target.value)}
        />
        <p className="field-hint" id={ids.hint}>
          {t("apiKeys.clients.urisHint")}
        </p>
        {problem ? (
          <p className="field-error" role="alert">
            {problem}
          </p>
        ) : null}
        <div className="own-key-actions">
          <button type="button" className="button button-primary" disabled={busy} onClick={() => void submit()}>
            {busy ? t("apiKeys.clients.busy") : t("apiKeys.clients.submit")}
          </button>
        </div>
      </fieldset>

      {created ? (
        <div className="banner apikey-client-created" role="status">
          <p>{t("apiKeys.clients.created", { name: created.name })}</p>
          <p>
            <code className="setup-inline">{created.id}</code>
          </p>
          <CopyButton text={created.id} label={t("apiKeys.clients.copy")} />
          <p className="field-hint">{t("apiKeys.clients.idHint")}</p>
        </div>
      ) : null}

      <p className="setting-done" role="status">
        {done}
      </p>
      {reading.kind === "asking" ? <p className="field-hint">{t("apiKeys.clients.asking")}</p> : null}
      {reading.kind === "unavailable" ? <p className="field-hint">{t("apiKeys.clients.unavailable")}</p> : null}
      {reading.kind === "loaded" && reading.list.length === 0 ? <p className="field-hint">{t("apiKeys.clients.none")}</p> : null}
      {reading.kind === "loaded" ? (
        <ul className="own-key-list apikey-client-list">
          {reading.list.map((c) => (
            <li key={c.id} className="own-key apikey-client">
              <p className="own-key-heading">
                <Untrusted text={c.name} />
              </p>
              <p className="own-key-facts">
                <span>{t("apiKeys.clients.id")}:</span> <code className="setup-inline">{c.id}</code>
              </p>
              <p className="own-key-facts">
                <span>{t("apiKeys.clients.returnsTo")}:</span> {c.redirectUris.join(", ")}
              </p>
              <p className="own-key-facts field-hint">{t("apiKeys.clients.registeredOn", { when: keyDateLong(c.createdAt) })}</p>
              <div className="own-key-actions">
                <CopyButton text={c.id} label={t("apiKeys.clients.copy")} />
                <button
                  type="button"
                  className="button button-danger"
                  disabled={busy}
                  aria-label={t("apiKeys.clients.deleteFor", { name: c.name })}
                  onClick={() => {
                    setDeleteError(null);
                    setDeleting(c);
                  }}
                >
                  {t("apiKeys.clients.delete")}
                </button>
              </div>
            </li>
          ))}
        </ul>
      ) : null}

      {deleting ? (
        <KeyConfirmDialog
          title={t("apiKeys.clients.deleteTitle", { name: deleting.name })}
          paragraphs={[t("apiKeys.clients.deleteWhat")]}
          confirmLabel={t("apiKeys.clients.deleteSubmit")}
          busyLabel={t("apiKeys.clients.deleteBusy")}
          busy={busy}
          error={deleteError}
          onConfirm={() => void remove(deleting)}
          onClose={() => setDeleting(null)}
        />
      ) : null}
    </div>
  );
}
