// SPDX-License-Identifier: AGPL-3.0-only

// CREATE AN API KEY, THEN SHOW ITS TOKEN ONCE (#49, ADR-042 §1).
//
// THE FORM. A name; a preset (Read only, Organise, Assistant) or Custom, whose
// checkboxes are the scope set the SERVER lists in its session object; "ask me
// first" for the destructive scopes, on by default; an optional account
// restriction; the rate and budget with the server's defaults and bounds; and
// suspend-on-anomaly, which starts at the server's default and is only sent if
// the person changes it. Sending and deleting are never in a preset.
//
// SEND AND DELETE TOGETHER needs the person to type a phrase. The server
// refuses such a key without `acknowledgeSendAndDelete`; the dialog makes that
// a deliberate act and buildCreate (model.ts) will not produce the flag — or
// the create — without the phrase. The Create button is aria-disabled (still a
// tab stop, described) until the form is complete, and a click on it then
// sends nothing.
//
// ⚠ THE TOKEN LIVES ONLY IN THIS COMPONENT'S STATE, AND ONLY IN THE "token"
// PHASE. It arrives in ApiKey/set's answer, is rendered in the dialog, and is
// gone when the dialog unmounts. `onCreated` receives the key WITHOUT it; it is
// never written to storage, the URL, a log or any state above this component.
// Closing takes the Done button: no ✕, no Escape, no click outside
// (KeyDialog dismissable={false}).

import { useId, useRef, useState } from "react";
import type { JSX } from "react";

import type { ApiKey, ApiKeysCapability, Id } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import type { MessageKey } from "../../../i18n";
import { KeyDialog } from "../keys/KeyDialog";
import type { ApiKeysApi } from "./api";
import {
  PRESETS,
  SEND_AND_DELETE_PHRASE,
  buildCreate,
  confirmableScopes,
  defaultConfirmScopes,
  grantsSendAndDelete,
  keyRefusalText,
  matchPreset,
  offeredScopes,
  presetName,
  presetScopes,
  scopeLabel,
} from "./model";
import type { CreateForm, CreateProblem, PresetId } from "./model";
import { CopyButton, SetupGuide } from "./SetupGuide";

type Choice = PresetId | "custom";

export function CreateKeyDialog({
  api,
  accounts,
  origin,
  onCreated,
  onClose,
}: {
  api: ApiKeysApi;
  accounts: readonly { id: Id; name: string }[];
  /** The page's origin, for the setup snippet. */
  origin: string;
  /** The key as created, WITHOUT its token. */
  onCreated: (key: ApiKey) => void;
  onClose: () => void;
}): JSX.Element {
  const [shown, setShown] = useState<{ name: string; token: string; scopes: string[]; restricted: boolean } | null>(
    null,
  );
  if (shown) {
    return (
      <TokenDialog
        name={shown.name}
        token={shown.token}
        scopes={shown.scopes}
        restricted={shown.restricted}
        origin={origin}
        onDone={onClose}
      />
    );
  }
  return (
    <CreateForm
      api={api}
      accounts={accounts}
      onClose={onClose}
      onCreated={(key, token) => {
        onCreated(key);
        setShown({ name: key.name, token, scopes: key.scopes, restricted: key.accountIds !== null });
      }}
    />
  );
}

const PROBLEM_TEXT: Record<CreateProblem, MessageKey> = {
  name: "apiKeys.create.problem.name",
  scopes: "apiKeys.create.problem.scopes",
  accounts: "apiKeys.create.problem.accounts",
  rate: "apiKeys.create.problem.rate",
  budget: "apiKeys.create.problem.budget",
  sendAndDelete: "apiKeys.create.problem.sendAndDelete",
};

function CreateForm({
  api,
  accounts,
  onCreated,
  onClose,
}: {
  api: ApiKeysApi;
  accounts: readonly { id: Id; name: string }[];
  onCreated: (key: ApiKey, token: string) => void;
  onClose: () => void;
}): JSX.Element {
  const cap: ApiKeysCapability | null = api.capability();
  const offered = offeredScopes(cap);
  const confirmable = confirmableScopes(cap);
  const [name, setName] = useState("");
  const [choice, setChoice] = useState<Choice>("read");
  const [scopes, setScopes] = useState<string[]>(() => presetScopes("read", offered));
  const [confirm, setConfirm] = useState<string[]>(() => defaultConfirmScopes(cap));
  const [restrict, setRestrict] = useState(false);
  const [accountIds, setAccountIds] = useState<Id[]>([]);
  const [rate, setRate] = useState("");
  const [budget, setBudget] = useState("");
  const [suspend, setSuspend] = useState<boolean | null>(null);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const nameRef = useRef<HTMLInputElement | null>(null);
  const ids = {
    name: useId(),
    preset: useId(),
    rate: useId(),
    budget: useId(),
    typed: useId(),
    waiting: useId(),
    rateHint: useId(),
    budgetHint: useId(),
  };

  const form: CreateForm = {
    name,
    scopes,
    accountIds: restrict ? accountIds : null,
    confirmScopes: confirm,
    ratePerMinute: rate,
    dailyBudget: budget,
    suspendOnAnomaly: suspend,
    sendAndDeleteTyped: typed,
  };
  const built = buildCreate(form, cap);
  const ready = built.ok && !busy;
  const both = grantsSendAndDelete(scopes);
  const suspendDefault = cap?.defaultSuspendOnAnomaly ?? true;

  const choose = (c: Choice): void => {
    setChoice(c);
    if (c !== "custom") setScopes(presetScopes(c, offered));
  };
  const toggleScope = (s: string, on: boolean): void => {
    const next = on ? [...scopes, s] : scopes.filter((x) => x !== s);
    setScopes(offered.filter((x) => next.includes(x)));
  };
  const toggleConfirm = (s: string, on: boolean): void => {
    setConfirm(on ? [...confirm, s] : confirm.filter((x) => x !== s));
  };

  const submit = async (): Promise<void> => {
    if (!built.ok || busy) return;
    setBusy(true);
    setError(null);
    const out = await api.create(built.settings);
    setBusy(false);
    if (out.ok) onCreated(out.key, out.token);
    else setError(keyRefusalText(out.error));
  };

  const grantedConfirmable = confirmable.filter((s) => scopes.includes(s));

  return (
    <KeyDialog title={t("apiKeys.create.title")} className="apikey-create" busy={busy} onClose={onClose} initialFocus={nameRef}>
      <label htmlFor={ids.name} className="field-label">
        {t("apiKeys.create.name")}
      </label>
      <input
        id={ids.name}
        ref={nameRef}
        className="text-input"
        type="text"
        maxLength={100}
        autoComplete="off"
        value={name}
        disabled={busy}
        onChange={(e) => setName(e.target.value)}
      />
      <p className="field-hint">{t("apiKeys.create.nameHint")}</p>

      <fieldset className="key-fieldset">
        <legend>{t("apiKeys.create.access")}</legend>
        {PRESETS.map((p) => (
          <label key={p.id} className="key-radio">
            <input
              type="radio"
              name={ids.preset}
              value={p.id}
              checked={choice === p.id}
              disabled={busy}
              onChange={() => choose(p.id)}
            />{" "}
            {presetName(p.id)} <span className="field-hint">— {t(`apiKeys.preset.${p.id}.explain` as MessageKey)}</span>
          </label>
        ))}
        <label className="key-radio">
          <input
            type="radio"
            name={ids.preset}
            value="custom"
            checked={choice === "custom"}
            disabled={busy}
            onChange={() => choose("custom")}
          />{" "}
          {t("apiKeys.preset.custom")} <span className="field-hint">— {t("apiKeys.preset.custom.explain")}</span>
        </label>
        {choice === "custom" ? (
          <fieldset className="key-fieldset apikey-scopes">
            <legend>{t("apiKeys.create.scopes")}</legend>
            {offered.map((s) => (
              <label key={s} className="key-radio">
                <input
                  type="checkbox"
                  value={s}
                  checked={scopes.includes(s)}
                  disabled={busy}
                  onChange={(e) => toggleScope(s, e.target.checked)}
                />{" "}
                {scopeLabel(s)} <code className="apikey-scope-id">{s}</code>
              </label>
            ))}
            {matchPreset(scopes) ? (
              <p className="field-hint">{t("apiKeys.create.matchesPreset", { preset: presetName(matchPreset(scopes)!) })}</p>
            ) : null}
          </fieldset>
        ) : null}
      </fieldset>

      {grantedConfirmable.length > 0 ? (
        <fieldset className="key-fieldset">
          <legend>{t("apiKeys.create.askFirst")}</legend>
          {grantedConfirmable.map((s) => (
            <label key={s} className="key-radio">
              <input
                type="checkbox"
                value={s}
                checked={confirm.includes(s)}
                disabled={busy}
                onChange={(e) => toggleConfirm(s, e.target.checked)}
              />{" "}
              {t("apiKeys.create.askFirstFor", { scope: scopeLabel(s) })}
            </label>
          ))}
          <p className="field-hint">{t("apiKeys.create.askFirstHint")}</p>
        </fieldset>
      ) : null}

      {both ? (
        <div className="apikey-both banner banner-warn">
          <p>{t("apiKeys.create.bothWarning")}</p>
          <label htmlFor={ids.typed} className="field-label">
            {t("apiKeys.create.bothLabel", { phrase: SEND_AND_DELETE_PHRASE })}
          </label>
          <input
            id={ids.typed}
            className="text-input apikey-both-confirm"
            type="text"
            autoComplete="off"
            spellCheck={false}
            value={typed}
            disabled={busy}
            onChange={(e) => setTyped(e.target.value)}
          />
        </div>
      ) : null}

      <fieldset className="key-fieldset">
        <legend>{t("apiKeys.create.accounts")}</legend>
        <label className="key-radio">
          <input type="radio" name={`${ids.preset}-acct`} checked={!restrict} disabled={busy} onChange={() => setRestrict(false)} />{" "}
          {t("apiKeys.restrict.all")}
        </label>
        <label className="key-radio">
          <input
            type="radio"
            name={`${ids.preset}-acct`}
            checked={restrict}
            disabled={busy || accounts.length === 0}
            onChange={() => setRestrict(true)}
          />{" "}
          {t("apiKeys.create.onlyThese")}
        </label>
        {restrict
          ? accounts.map((a) => (
              <label key={a.id} className="key-radio apikey-account">
                <input
                  type="checkbox"
                  checked={accountIds.includes(a.id)}
                  disabled={busy}
                  onChange={(e) =>
                    setAccountIds(e.target.checked ? [...accountIds, a.id] : accountIds.filter((x) => x !== a.id))
                  }
                />{" "}
                {a.name}
              </label>
            ))
          : null}
        {restrict ? <p className="field-hint">{t("apiKeys.create.restrictHint")}</p> : null}
      </fieldset>

      <fieldset className="key-fieldset apikey-limits">
        <legend>{t("apiKeys.create.limits")}</legend>
        <label htmlFor={ids.rate} className="field-label">
          {t("apiKeys.create.rate")}
        </label>
        <input
          id={ids.rate}
          className="text-input setting-days-input"
          type="text"
          inputMode="numeric"
          placeholder={String(cap?.defaultRatePerMinute ?? 60)}
          aria-describedby={ids.rateHint}
          value={rate}
          disabled={busy}
          onChange={(e) => setRate(e.target.value)}
        />
        <p className="field-hint" id={ids.rateHint}>
          {t("apiKeys.create.rateHint", { def: cap?.defaultRatePerMinute ?? 60, max: cap?.maxRatePerMinute ?? 600 })}
        </p>
        <label htmlFor={ids.budget} className="field-label">
          {t("apiKeys.create.budget")}
        </label>
        <input
          id={ids.budget}
          className="text-input setting-days-input"
          type="text"
          inputMode="numeric"
          placeholder={String(cap?.defaultDailyBudget ?? 500)}
          aria-describedby={ids.budgetHint}
          value={budget}
          disabled={busy}
          onChange={(e) => setBudget(e.target.value)}
        />
        <p className="field-hint" id={ids.budgetHint}>
          {t("apiKeys.create.budgetHint", { def: cap?.defaultDailyBudget ?? 500, max: cap?.maxDailyBudget ?? 20000 })}
        </p>
        <label className="key-checkbox">
          <input
            type="checkbox"
            checked={suspend ?? suspendDefault}
            disabled={busy}
            onChange={(e) => setSuspend(e.target.checked)}
          />{" "}
          {t("apiKeys.create.suspend")}
        </label>
        <p className="field-hint">
          {t("apiKeys.create.suspendHint", { n: cap?.massDeleteThreshold ?? 50, minutes: Math.round((cap?.massDeleteWindowSeconds ?? 600) / 60) })}
        </p>
      </fieldset>

      <p id={ids.waiting} className="field-hint apikey-create-waiting">
        {built.ok ? t("apiKeys.create.ready") : t(PROBLEM_TEXT[built.problem])}
      </p>
      {error ? (
        <p className="field-error" role="alert">
          {error}
        </p>
      ) : null}
      {busy ? (
        <p className="field-hint" role="status">
          {t("apiKeys.create.busy")}
        </p>
      ) : null}
      <div className="sheet-actions">
        <button type="button" className="button" disabled={busy} onClick={onClose}>
          {t("action.cancel")}
        </button>
        <button
          type="button"
          className="button button-primary key-submit apikey-create-submit"
          aria-disabled={!ready}
          aria-describedby={ids.waiting}
          onClick={() => void submit()}
        >
          {t("apiKeys.create.submit")}
        </button>
      </div>
    </KeyDialog>
  );
}

/** The token, once. Closing is the Done button and nothing else. */
export function TokenDialog({
  name,
  token,
  scopes,
  restricted,
  origin,
  onDone,
}: {
  name: string;
  token: string;
  scopes: readonly string[];
  restricted: boolean;
  origin: string;
  onDone: () => void;
}): JSX.Element {
  const tokenId = useId();
  // Focus starts on the token itself (selected on focus, ready to copy), not on
  // Done: an Enter pressed out of habit must not be what throws the key away.
  const tokenRef = useRef<HTMLInputElement | null>(null);
  return (
    <KeyDialog
      title={t("apiKeys.token.title", { name })}
      className="apikey-token"
      onClose={onDone}
      dismissable={false}
      initialFocus={tokenRef}
    >
      <p>
        <strong>{t("apiKeys.token.once")}</strong>
      </p>
      <label htmlFor={tokenId} className="field-label">
        {t("apiKeys.token.label")}
      </label>
      <input
        id={tokenId}
        ref={tokenRef}
        className="text-input apikey-token-value"
        type="text"
        readOnly
        autoComplete="off"
        spellCheck={false}
        value={token}
        onFocus={(e) => e.currentTarget.select()}
      />
      <CopyButton text={token} label={t("apiKeys.token.copy")} />
      <h3 className="apikey-token-setup">{t("apiKeys.setup.heading")}</h3>
      <SetupGuide origin={origin} token={token} scopes={scopes} restricted={restricted} subheading="h4" />
      <div className="sheet-actions">
        <button type="button" className="button button-primary apikey-token-done" onClick={onDone}>
          {t("apiKeys.token.done")}
        </button>
      </div>
    </KeyDialog>
  );
}
