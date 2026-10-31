// SPDX-License-Identifier: AGPL-3.0-only

// ONE API KEY (`#/settings/api-keys/<id>`, #49).
//
// What it can do and where, its state, and the controls the state allows:
//
//   * EDIT the name, the limits, "ask me first" and suspend-on-anomaly (the
//     scopes are fixed: a key with different powers is a new key, made with the
//     typed confirmation where it needs one). Only the changed fields are sent.
//   * SUSPEND / REACTIVATE. Reactivating says what tripped the suspension —
//     the server's reason and time, and the most recent anomaly in the
//     activity — before the person confirms.
//   * REVOKE, with a confirmation that names the key. Permanent.
//   * ACTIVITY, below (Activity.tsx).
//
// A revoked key is shown read-only, with its activity: the audit stays.

import { useEffect, useId, useState } from "react";
import type { JSX } from "react";

import type { ApiKey, Id } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import { keyDateLong } from "../../../../core/userKeys";
import { clockText } from "../../desk/format";
import { KeyConfirmDialog } from "../keys/KeyDialog";
import { Activity } from "./Activity";
import type { ActivityReading } from "./Activity";
import type { ApiKeysApi } from "./api";
import {
  confirmableScopes,
  eventText,
  keyRefusalText,
  keyState,
  orderScopes,
  restrictionText,
  scopeLabel,
  scopeSummary,
  usageText,
} from "./model";
import { SetupGuide } from "./SetupGuide";

export const ACTIVITY_LIMIT = 50;

function when(iso: string): string {
  return `${keyDateLong(iso)} ${clockText(iso)}`;
}

export function KeyDetail({
  keyItem,
  api,
  accountNames,
  origin,
  onChanged,
  onBack,
}: {
  keyItem: ApiKey;
  api: ApiKeysApi;
  accountNames: ReadonlyMap<Id, string>;
  origin: string;
  /** After a write: re-read the list. */
  onChanged: () => void;
  onBack: () => void;
}): JSX.Element {
  const cap = api.capability();
  const state = keyState(keyItem);
  const [activity, setActivity] = useState<ActivityReading>({ kind: "asking" });
  const [generation, setGeneration] = useState(0);
  const [dialog, setDialog] = useState<"revoke" | "suspend" | "reactivate" | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState("");

  useEffect(() => {
    let cancelled = false;
    setActivity({ kind: "asking" });
    void api.events(keyItem.id).then((r) => {
      if (cancelled) return;
      setActivity(r === null ? { kind: "unavailable" } : { kind: "loaded", list: r.list, suppressed: r.suppressedRefusals });
    });
    return () => {
      cancelled = true;
    };
  }, [api, keyItem.id, generation]);

  const run = async (label: string, op: () => Promise<{ ok: true } | { ok: false; error: { type: string; description?: string } }>): Promise<boolean> => {
    setBusy(true);
    setError(null);
    const out = await op();
    setBusy(false);
    if (!out.ok) {
      setError(keyRefusalText(out.error));
      return false;
    }
    setDone(label);
    setDialog(null);
    onChanged();
    setGeneration((g) => g + 1);
    return true;
  };

  // The anomaly that most likely tripped a suspension: the newest one listed.
  const lastAnomaly =
    activity.kind === "loaded" ? activity.list.find((e) => e.outcome === "anomaly") ?? null : null;

  return (
    <div className="apikey-detail">
      <p>
        <button type="button" className="link-button" onClick={onBack}>
          {t("apiKeys.detail.back")}
        </button>
      </p>
      <h4 className="apikey-detail-name">{keyItem.name}</h4>
      <p className="setting-done" role="status">
        {done}
      </p>
      <dl className="apikey-facts">
        <dt>{t("apiKeys.detail.state")}</dt>
        <dd className={state.kind === "active" ? undefined : "is-warn"}>
          {state.kind === "active"
            ? t("apiKeys.state.active")
            : state.kind === "revoked"
              ? t("apiKeys.state.revokedOn", { when: when(state.at) })
              : t("apiKeys.state.suspendedOn", {
                  when: state.at ? when(state.at) : "?",
                  reason: state.reason ?? t("apiKeys.state.noReason"),
                })}
        </dd>
        <dt>{t("apiKeys.detail.hint")}</dt>
        <dd>
          <code>{keyItem.tokenHint}…</code>
        </dd>
        <dt>{t("apiKeys.detail.access")}</dt>
        <dd>
          {scopeSummary(keyItem.scopes)}
          <ul className="apikey-scope-list">
            {orderScopes(keyItem.scopes).map((s) => (
              <li key={s}>
                {scopeLabel(s)} <code className="apikey-scope-id">{s}</code>
              </li>
            ))}
          </ul>
        </dd>
        <dt>{t("apiKeys.detail.accounts")}</dt>
        <dd>{restrictionText(keyItem.accountIds, accountNames)}</dd>
        <dt>{t("apiKeys.detail.created")}</dt>
        <dd>{when(keyItem.createdAt)}</dd>
        <dt>{t("apiKeys.detail.lastUsed")}</dt>
        <dd>{keyItem.lastUsedAt ? when(keyItem.lastUsedAt) : t("apiKeys.never")}</dd>
        {usageText(keyItem) ? (
          <>
            <dt>{t("apiKeys.detail.usage")}</dt>
            <dd>{usageText(keyItem)}</dd>
          </>
        ) : null}
      </dl>

      {state.kind !== "revoked" ? (
        <EditForm keyItem={keyItem} api={api} busy={busy} onSave={(patch) => run(t("apiKeys.detail.saved"), () => api.update(keyItem.id, patch))} />
      ) : null}

      {error ? (
        <p className="field-error" role="alert">
          {error}
        </p>
      ) : null}

      {state.kind !== "revoked" ? (
        <div className="own-key-actions">
          {state.kind === "active" ? (
            <button type="button" className="button" disabled={busy} onClick={() => setDialog("suspend")}>
              {t("apiKeys.detail.suspend")}
            </button>
          ) : (
            <button type="button" className="button" disabled={busy} onClick={() => setDialog("reactivate")}>
              {t("apiKeys.detail.reactivate")}
            </button>
          )}
          <button type="button" className="button button-danger" disabled={busy} onClick={() => setDialog("revoke")}>
            {t("apiKeys.detail.revoke")}
          </button>
        </div>
      ) : null}

      <h4>{t("apiKeys.activity.heading")}</h4>
      <Activity reading={activity} cap={cap} limit={ACTIVITY_LIMIT} />

      {state.kind !== "revoked" ? (
        <>
          <h4>{t("apiKeys.setup.heading")}</h4>
          <SetupGuide origin={origin} token={null} scopes={keyItem.scopes} restricted={keyItem.accountIds !== null} />
        </>
      ) : null}

      {dialog === "revoke" ? (
        <KeyConfirmDialog
          title={t("apiKeys.revoke.title", { name: keyItem.name })}
          paragraphs={[t("apiKeys.revoke.what"), t("apiKeys.revoke.permanent")]}
          confirmLabel={t("apiKeys.revoke.submit")}
          busyLabel={t("apiKeys.revoke.busy")}
          busy={busy}
          error={error}
          onConfirm={() => void run(t("apiKeys.revoke.done"), () => api.revoke(keyItem.id))}
          onClose={() => setDialog(null)}
        />
      ) : null}
      {dialog === "suspend" ? (
        <KeyConfirmDialog
          title={t("apiKeys.suspend.title", { name: keyItem.name })}
          paragraphs={[t("apiKeys.suspend.what")]}
          confirmLabel={t("apiKeys.suspend.submit")}
          busyLabel={t("apiKeys.suspend.busy")}
          busy={busy}
          error={error}
          onConfirm={() => void run(t("apiKeys.suspend.done"), () => api.update(keyItem.id, { suspended: true }))}
          onClose={() => setDialog(null)}
        />
      ) : null}
      {dialog === "reactivate" ? (
        <KeyConfirmDialog
          title={t("apiKeys.reactivate.title", { name: keyItem.name })}
          paragraphs={[
            state.kind === "suspended"
              ? t("apiKeys.reactivate.why", {
                  when: state.at ? when(state.at) : "?",
                  reason: state.reason ?? t("apiKeys.state.noReason"),
                })
              : "",
            ...(lastAnomaly
              ? [
                  t("apiKeys.reactivate.lastAnomaly", {
                    when: when(lastAnomaly.at),
                    what: eventText(lastAnomaly, cap).reason ?? "",
                  }),
                ]
              : []),
            t("apiKeys.reactivate.what"),
          ].filter((p) => p !== "")}
          confirmLabel={t("apiKeys.reactivate.submit")}
          busyLabel={t("apiKeys.reactivate.busy")}
          busy={busy}
          error={error}
          onConfirm={() => void run(t("apiKeys.reactivate.done"), () => api.update(keyItem.id, { suspended: false }))}
          onClose={() => setDialog(null)}
        />
      ) : null}
    </div>
  );
}

function EditForm({
  keyItem,
  api,
  busy,
  onSave,
}: {
  keyItem: ApiKey;
  api: ApiKeysApi;
  busy: boolean;
  onSave: (patch: Parameters<ApiKeysApi["update"]>[1]) => Promise<boolean>;
}): JSX.Element {
  const cap = api.capability();
  const [name, setName] = useState(keyItem.name);
  const [rate, setRate] = useState(String(keyItem.ratePerMinute));
  const [budget, setBudget] = useState(String(keyItem.dailyBudget));
  const [confirm, setConfirm] = useState<string[]>(keyItem.confirmScopes);
  const [suspend, setSuspend] = useState(keyItem.suspendOnAnomaly);
  const [problem, setProblem] = useState<string | null>(null);
  const ids = { name: useId(), rate: useId(), budget: useId() };
  const confirmable = confirmableScopes(cap).filter((s) => keyItem.scopes.includes(s));

  const save = async (): Promise<void> => {
    const patch: Parameters<ApiKeysApi["update"]>[1] = {};
    const trimmed = name.trim();
    if (trimmed === "" || [...trimmed].length > 100) return setProblem(t("apiKeys.create.problem.name"));
    if (trimmed !== keyItem.name) patch.name = trimmed;
    const r = Number(rate.trim());
    const maxRate = cap?.maxRatePerMinute ?? 600;
    if (!/^\d+$/.test(rate.trim()) || r < 1 || r > maxRate) return setProblem(t("apiKeys.create.problem.rate"));
    if (r !== keyItem.ratePerMinute) patch.ratePerMinute = r;
    const b = Number(budget.trim());
    const maxBudget = cap?.maxDailyBudget ?? 20000;
    if (!/^\d+$/.test(budget.trim()) || b < 1 || b > maxBudget) return setProblem(t("apiKeys.create.problem.budget"));
    if (b !== keyItem.dailyBudget) patch.dailyBudget = b;
    const sorted = [...confirm].sort();
    if (sorted.join(",") !== [...keyItem.confirmScopes].sort().join(",")) patch.confirmScopes = sorted;
    if (suspend !== keyItem.suspendOnAnomaly) patch.suspendOnAnomaly = suspend;
    setProblem(null);
    if (Object.keys(patch).length === 0) return setProblem(t("apiKeys.detail.nothingChanged"));
    await onSave(patch);
  };

  return (
    <fieldset className="key-fieldset apikey-edit">
      <legend>{t("apiKeys.detail.edit")}</legend>
      <label htmlFor={ids.name} className="field-label">
        {t("apiKeys.create.name")}
      </label>
      <input id={ids.name} className="text-input" type="text" maxLength={100} value={name} disabled={busy} onChange={(e) => setName(e.target.value)} />
      <label htmlFor={ids.rate} className="field-label">
        {t("apiKeys.create.rate")}
      </label>
      <input
        id={ids.rate}
        className="text-input setting-days-input"
        type="text"
        inputMode="numeric"
        value={rate}
        disabled={busy}
        onChange={(e) => setRate(e.target.value)}
      />
      <label htmlFor={ids.budget} className="field-label">
        {t("apiKeys.create.budget")}
      </label>
      <input
        id={ids.budget}
        className="text-input setting-days-input"
        type="text"
        inputMode="numeric"
        value={budget}
        disabled={busy}
        onChange={(e) => setBudget(e.target.value)}
      />
      {confirmable.map((s) => (
        <label key={s} className="key-checkbox">
          <input
            type="checkbox"
            checked={confirm.includes(s)}
            disabled={busy}
            onChange={(e) => setConfirm(e.target.checked ? [...confirm, s] : confirm.filter((x) => x !== s))}
          />{" "}
          {t("apiKeys.create.askFirstFor", { scope: scopeLabel(s) })}
        </label>
      ))}
      <label className="key-checkbox">
        <input type="checkbox" checked={suspend} disabled={busy} onChange={(e) => setSuspend(e.target.checked)} />{" "}
        {t("apiKeys.create.suspend")}
      </label>
      {problem ? (
        <p className="field-error" role="alert">
          {problem}
        </p>
      ) : null}
      <div className="own-key-actions">
        <button type="button" className="button button-primary" disabled={busy} onClick={() => void save()}>
          {t("apiKeys.detail.save")}
        </button>
      </div>
    </fieldset>
  );
}
