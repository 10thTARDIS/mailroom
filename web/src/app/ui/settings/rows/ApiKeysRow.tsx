// SPDX-License-Identifier: AGPL-3.0-only

// API KEYS AND AI TOOLS (#49, ADR-042, ADR-044).
//
// The row has the Keys row's four states (rows/KeysRow.tsx), for the same
// reason: `urn:mailroom:apikeys` may be absent, and a server that cannot be
// asked has not said you have no keys.
//
//   capability absent → "not available on this server"
//   still asking      → no facts (a number nobody has read is not a zero)
//   asked             → the requests waiting for approval FIRST (they are
//                       the only thing here with a deadline), then the keys,
//                       then Create, then "Connect an AI tool"
//   a key's detail    → `#/settings/api-keys/<id>` (apikeys/KeyDetail.tsx)
//
// Keys are not JMAP-synced objects (ADR-042 §1): the list is read when the row
// opens and again after every change made here.

import { useCallback, useEffect, useState } from "react";
import type { JSX } from "react";

import type { ApiKey, Id, PendingAction } from "../../../../core/jmap/types";
import { t, tCount } from "../../../i18n";
import { keyDateLong } from "../../../../core/userKeys";
import type { StateFact } from "../SettingsRow";
import type { ApiKeysApi } from "../apikeys/api";
import { CreateKeyDialog } from "../apikeys/CreateKeyDialog";
import { KeyDetail } from "../apikeys/KeyDetail";
import { keyState, restrictionText, scopeSummary, usageText } from "../apikeys/model";
import { PendingList } from "../apikeys/PendingList";
import { SetupGuide } from "../apikeys/SetupGuide";

export type ApiKeysReading = { kind: "unavailable" } | { kind: "asking" } | { kind: "loaded"; keys: ApiKey[] };

/** Reads the keys once the session has landed (KeysRow's gate), and on reload(). */
export function useApiKeys(api: ApiKeysApi, sessionReady: boolean): { reading: ApiKeysReading; reload: () => void } {
  const [reading, setReading] = useState<ApiKeysReading>({ kind: "asking" });
  const [generation, setGeneration] = useState(0);
  useEffect(() => {
    if (!sessionReady) return;
    if (api.capability() === null) {
      setReading({ kind: "unavailable" });
      return;
    }
    let cancelled = false;
    void api.list().then((keys) => {
      if (!cancelled) setReading(keys === null ? { kind: "unavailable" } : { kind: "loaded", keys });
    });
    return () => {
      cancelled = true;
    };
  }, [api, sessionReady, generation]);
  const reload = useCallback(() => setGeneration((g) => g + 1), []);
  return { reading, reload };
}

/** The state line: `2 active · 1 suspended · 3 waiting for approval`. */
export function apiKeysFacts(reading: ApiKeysReading, waiting: readonly PendingAction[] | null): StateFact[] {
  if (reading.kind === "unavailable") return [{ text: t("apiKeys.unavailable") }];
  if (reading.kind === "asking") return [];
  const live = reading.keys.filter((k) => !k.revokedAt);
  const facts: StateFact[] = [];
  if (live.length === 0) facts.push({ text: t("apiKeys.state.none") });
  const active = live.filter((k) => !k.suspendedAt).length;
  const suspended = live.length - active;
  if (active > 0) facts.push({ text: tCount("apiKeys.state.activeCount", active) });
  if (suspended > 0) facts.push({ text: tCount("apiKeys.state.suspendedCount", suspended), tone: "warn" });
  if (waiting && waiting.length > 0) facts.push({ text: tCount("apiKeys.state.waiting", waiting.length), tone: "warn" });
  return facts;
}

export function ApiKeysBody({
  reading,
  reload,
  api,
  waiting,
  refreshPending,
  accounts,
  apiKeyId,
  openKey,
  origin,
}: {
  reading: ApiKeysReading;
  reload: () => void;
  api: ApiKeysApi;
  waiting: readonly PendingAction[] | null;
  refreshPending: () => void;
  accounts: readonly { id: Id; name: string }[];
  /** The key whose detail is open, from the route. */
  apiKeyId: Id | null;
  /** Navigate to a key's detail (null: back to the list). */
  openKey: (id: Id | null) => void;
  origin: string;
}): JSX.Element {
  const [creating, setCreating] = useState(false);
  const [done, setDone] = useState("");
  const names = new Map(accounts.map((a) => [a.id, a.name] as const));

  // Re-read the waiting list when the row opens, rather than waiting out the
  // banner's poll: the person came here to decide.
  useEffect(() => {
    refreshPending();
    // Once per opening of the row.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  if (reading.kind === "unavailable") return <p className="field-hint">{t("apiKeys.unavailableLong")}</p>;
  if (reading.kind === "asking") return <p className="field-hint">{t("apiKeys.asking")}</p>;

  if (apiKeyId) {
    const k = reading.keys.find((x) => x.id === apiKeyId);
    if (!k) {
      return (
        <div className="apikeys-page">
          <p>{t("apiKeys.detail.missing")}</p>
          <button type="button" className="link-button" onClick={() => openKey(null)}>
            {t("apiKeys.detail.back")}
          </button>
        </div>
      );
    }
    return (
      <div className="apikeys-page">
        <KeyDetail keyItem={k} api={api} accountNames={names} origin={origin} onChanged={reload} onBack={() => openKey(null)} />
      </div>
    );
  }

  const live = reading.keys.filter((k) => !k.revokedAt);
  const revoked = reading.keys.filter((k) => k.revokedAt);

  return (
    <div className="apikeys-page">
      <h4>{t("apiKeys.pending.heading")}</h4>
      {waiting === null ? (
        <p className="field-hint">{t("apiKeys.pending.asking")}</p>
      ) : (
        <PendingList waiting={waiting} api={api} onDecided={refreshPending} />
      )}

      <h4>{t("apiKeys.list.heading")}</h4>
      <p className="setting-done" role="status">
        {done}
      </p>
      {live.length === 0 ? <p className="field-hint">{t("apiKeys.list.none")}</p> : null}
      <ul className="own-key-list apikey-list">
        {live.map((k) => (
          <KeyItem key={k.id} k={k} names={names} onOpen={() => openKey(k.id)} />
        ))}
      </ul>
      <div className="own-key-actions">
        <button type="button" className="button button-primary" onClick={() => setCreating(true)}>
          {t("apiKeys.create.open")}
        </button>
      </div>
      {revoked.length > 0 ? (
        <details className="apikey-revoked">
          <summary>{tCount("apiKeys.list.revoked", revoked.length)}</summary>
          <ul className="own-key-list apikey-list">
            {revoked.map((k) => (
              <KeyItem key={k.id} k={k} names={names} onOpen={() => openKey(k.id)} />
            ))}
          </ul>
        </details>
      ) : null}

      <h4>{t("apiKeys.setup.heading")}</h4>
      <SetupGuide origin={origin} token={null} scopes={null} />

      {creating ? (
        <CreateKeyDialog
          api={api}
          accounts={accounts}
          origin={origin}
          onCreated={(key) => {
            setDone(t("apiKeys.create.done", { name: key.name }));
            reload();
          }}
          onClose={() => setCreating(false)}
        />
      ) : null}
    </div>
  );
}

function KeyItem({ k, names, onOpen }: { k: ApiKey; names: ReadonlyMap<Id, string>; onOpen: () => void }): JSX.Element {
  const state = keyState(k);
  const usage = usageText(k);
  return (
    <li className="own-key apikey-item">
      <p className="own-key-heading">
        {k.name} <code className="apikey-hint">{k.tokenHint}…</code>
      </p>
      <p className="own-key-facts">
        <span>{scopeSummary(k.scopes)}</span>
        {" · "}
        <span>{restrictionText(k.accountIds, names)}</span>
        {" · "}
        <span className={state.kind === "active" ? undefined : "is-warn"}>
          {state.kind === "active"
            ? t("apiKeys.state.active")
            : state.kind === "revoked"
              ? t("apiKeys.state.revokedShort", { when: keyDateLong(state.at) })
              : t("apiKeys.state.suspendedShort", { reason: state.reason ?? t("apiKeys.state.noReason") })}
        </span>
      </p>
      <p className="own-key-facts field-hint">
        {k.lastUsedAt ? t("apiKeys.lastUsed", { when: keyDateLong(k.lastUsedAt) }) : t("apiKeys.neverUsed")}
        {usage ? ` · ${usage}` : null}
      </p>
      <div className="own-key-actions">
        <button type="button" className="button" onClick={onOpen} aria-label={t("apiKeys.list.openFor", { name: k.name })}>
          {t("apiKeys.list.open")}
        </button>
      </div>
    </li>
  );
}
