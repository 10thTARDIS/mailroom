// SPDX-License-Identifier: AGPL-3.0-only

// The add-account wizard (spec §12, mailroom#98/#45): add-account form → the
// server validates the credentials + detects folders → initial-sync progress.
//
// It is a thin, honest client over the Account/set round trip (urn:mailroom:
// accounts). The password is WRITE-ONLY: it is sent once, sealed server-side,
// and never read back — so this form collects it, submits it, and forgets it
// (there is no "edit password" that shows the old one). Server-side validation
// is the source of truth: the "Add" button dials IMAP through the server, so a
// wrong password or unreachable host comes back as a clear per-field message,
// not a half-created account.
//
// THE FIELDS ARE SHARED WITH THE CREDENTIALS EDITOR (#374). Settings → Accounts →
// "Change credentials…" collects exactly this block, because a connection change
// is validated and sealed EXACTLY like a create server-side
// (internal/api/account.go prepareAccountUpdate: "A connection change is
// validated and sealed exactly like a create"). Two copies of the block would be
// two chances for the edit form to omit a field the server requires — see
// `AccountFields`, and see the ⚠ on `ACCOUNT_CONNECTION_NOTE`.

import { useState } from "react";
import type { JSX } from "react";

import { useApp } from "../state";
import { t } from "../i18n";
import type { MessageKey } from "../i18n";
import type { Account, SetError, SetResult } from "../../core/jmap/types";

type Phase = "form" | "validating" | "done";

/** Every field a create — or a connection change — has to carry. */
export interface AccountFormValues {
  name: string;
  imapHost: string;
  imapPort: string;
  smtpHost: string;
  smtpPort: string;
  username: string;
  password: string;
}

export const emptyAccountForm: AccountFormValues = {
  name: "",
  imapHost: "",
  imapPort: "993",
  smtpHost: "",
  smtpPort: "465",
  username: "",
  password: "",
};

/**
 * ⚠ THERE IS NO PARTIAL CONNECTION CHANGE, and this is the note the settings row
 * prints rather than discovering it as a refusal.
 *
 * internal/api/account.go classifies a patch mentioning ANY of `imapHost`,
 * `imapPort`, `smtpHost`, `smtpPort`, `username`, `password` or `providerProfile`
 * as a connection change, and then runs `accountSpecFromCreate` over the WHOLE
 * patch — which requires `name`, `imapHost`, `smtpHost`, `username` AND a non-empty
 * `password`. The server never keeps the old plaintext secret (it is sealed under
 * the user's credentials key and never opened for this path), so "leave the
 * password as it was" is not a thing it can do. A blank password field here would
 * therefore be refused with `password is required`, not honoured as "unchanged".
 */
export const ACCOUNT_CONNECTION_NOTE = "account.credentials.mustReenter" as const;

/** Maps a per-object Account/set error to a user-facing message. A
 * validationFailed (the server could not dial/sign in) gets the plain-language
 * explanation; anything with a description surfaces it; the rest fall back to a
 * generic line. Server error strings never carry the password (account.go). */
export function messageForSetError(se: SetError | undefined): string {
  if (!se) return t("account.validationFailed");
  if (se.type === "validationFailed") return t("account.validationFailed");
  if (se.description) return se.description;
  return t("account.validationFailed");
}

/**
 * The required-field checks, done client-side so the message names the field.
 *
 * The SERVER re-validates every one of them and does the real IMAP dial
 * regardless — this only saves a round trip whose answer is already known.
 */
export function accountFormError(form: AccountFormValues): MessageKey | null {
  if (!form.name.trim()) return "account.needName";
  if (!form.imapHost.trim()) return "account.needImapHost";
  if (!form.smtpHost.trim()) return "account.needSmtpHost";
  if (!form.username.trim()) return "account.needUsername";
  if (!form.password) return "account.needPassword";
  return null;
}

/** The `Account/set` create — or connection-change update — payload for a filled
 * form. One builder for both, because the server takes the same block for both
 * and a second one would eventually carry a different set of fields. */
export function accountFormPayload(form: AccountFormValues): Record<string, unknown> {
  return {
    name: form.name.trim(),
    imapHost: form.imapHost.trim(),
    imapPort: Number(form.imapPort) || undefined,
    smtpHost: form.smtpHost.trim(),
    smtpPort: Number(form.smtpPort) || undefined,
    username: form.username.trim(),
    password: form.password,
  };
}

/**
 * The seven fields, once.
 *
 * `autoFocusName` is off in the settings row: that form appears inside an already
 * open disclosure, and stealing focus there would move a reader who was on their
 * way somewhere else.
 */
export function AccountFields({
  form,
  onChange,
  disabled,
  autoFocusName = false,
  passwordHint = "account.password.hint",
}: {
  form: AccountFormValues;
  onChange: (next: AccountFormValues) => void;
  disabled: boolean;
  autoFocusName?: boolean;
  passwordHint?: MessageKey;
}): JSX.Element {
  const field =
    (k: keyof AccountFormValues) =>
    (e: React.ChangeEvent<HTMLInputElement>): void =>
      onChange({ ...form, [k]: e.target.value });

  return (
    <>
      <label className="field">
        <span className="field-label">{t("account.name")}</span>
        <input
          className="text-input"
          type="text"
          value={form.name}
          onChange={field("name")}
          autoFocus={autoFocusName}
          autoComplete="off"
          disabled={disabled}
        />
      </label>

      <label className="field">
        <span className="field-label">{t("account.username")}</span>
        <input
          className="text-input"
          type="text"
          value={form.username}
          onChange={field("username")}
          autoComplete="username"
          inputMode="email"
          disabled={disabled}
        />
      </label>

      <label className="field">
        <span className="field-label">{t("account.password")}</span>
        <input
          className="text-input"
          type="password"
          value={form.password}
          onChange={field("password")}
          autoComplete="new-password"
          disabled={disabled}
        />
        <span className="field-hint">{t(passwordHint)}</span>
      </label>

      <fieldset className="field-set">
        <legend className="field-label">{t("account.advanced")}</legend>
        <div className="field-row">
          <label className="field field-grow">
            <span className="field-label">{t("account.imapHost")}</span>
            <input
              className="text-input"
              type="text"
              value={form.imapHost}
              onChange={field("imapHost")}
              autoComplete="off"
              disabled={disabled}
            />
          </label>
          <label className="field">
            <span className="field-label">{t("account.imapPort")}</span>
            <input
              className="text-input"
              type="number"
              value={form.imapPort}
              onChange={field("imapPort")}
              min={1}
              max={65535}
              disabled={disabled}
            />
          </label>
        </div>
        <div className="field-row">
          <label className="field field-grow">
            <span className="field-label">{t("account.smtpHost")}</span>
            <input
              className="text-input"
              type="text"
              value={form.smtpHost}
              onChange={field("smtpHost")}
              autoComplete="off"
              disabled={disabled}
            />
          </label>
          <label className="field">
            <span className="field-label">{t("account.smtpPort")}</span>
            <input
              className="text-input"
              type="number"
              value={form.smtpPort}
              onChange={field("smtpPort")}
              min={1}
              max={65535}
              disabled={disabled}
            />
          </label>
        </div>
      </fieldset>
    </>
  );
}

/**
 * The ＋ in the top bar, and the same wizard as a words button inside
 * Settings → Accounts (#374).
 *
 * TWO TRIGGERS, ONE COMPONENT, AND THEY ARE INDEPENDENT INSTANCES — each has its
 * own `open`, so there is no shared state to get out of step. The top-bar ＋ stays
 * because adding an account is a thing people do without going looking for a
 * settings page first; the settings row carries it because that row is now where
 * accounts live, and a list of accounts with no way to add one is a dead end.
 */
export function AddAccountWizard({
  trigger = "icon",
}: { trigger?: "icon" | "words" } = {}): JSX.Element {
  const { client, sync } = useApp();
  const [open, setOpen] = useState(false);
  const [phase, setPhase] = useState<Phase>("form");
  const [error, setError] = useState<string | null>(null);
  const [addedName, setAddedName] = useState("");
  const [form, setForm] = useState<AccountFormValues>({ ...emptyAccountForm });

  const reset = (): void => {
    setForm({ ...emptyAccountForm });
    setPhase("form");
    setError(null);
    setAddedName("");
  };
  const close = (): void => {
    setOpen(false);
    reset();
  };

  const submit = async (e: React.FormEvent): Promise<void> => {
    e.preventDefault();
    const ce = accountFormError(form);
    if (ce) {
      setError(t(ce));
      return;
    }
    setPhase("validating");
    setError(null);
    try {
      const res = (await client.accountSet({
        create: { new: accountFormPayload(form) },
      } as never)) as SetResult<Account>;
      if (res.created && res.created["new"]) {
        setAddedName(form.name.trim());
        setPhase("done");
        // Start pulling the newly synced mail without waiting for the poll tick.
        void sync.poll();
        return;
      }
      setPhase("form");
      setError(messageForSetError(res.notCreated?.["new"]));
    } catch (err) {
      setPhase("form");
      setError(t("account.error", { error: String(err) }));
    }
  };

  const onKeyDown = (e: React.KeyboardEvent): void => {
    if (e.key === "Escape") close();
  };

  return (
    <>
      {trigger === "words" ? (
        <button
          type="button"
          className="button button-quiet menu-action"
          aria-haspopup="dialog"
          onClick={() => setOpen(true)}
        >
          {t("account.add")}
        </button>
      ) : (
        <button
          type="button"
          className="icon-button"
          aria-haspopup="dialog"
          aria-label={t("account.add")}
          title={t("account.add")}
          onClick={() => setOpen(true)}
        >
          ＋
        </button>
      )}

      {open ? (
        <div className="modal-backdrop" onKeyDown={onKeyDown}>
          <div
            className="sheet"
            role="dialog"
            aria-modal="true"
            aria-label={t("account.add.title")}
          >
            <header className="sheet-header">
              <h2>{t("account.add.title")}</h2>
              <button
                type="button"
                className="icon-button"
                aria-label={t("action.close")}
                onClick={close}
              >
                ✕
              </button>
            </header>

            {phase === "done" ? (
              <div className="sheet-body">
                <p className="banner banner-ok" role="status">
                  {t("account.added", { name: addedName })}
                </p>
                <div className="sheet-actions">
                  <button type="button" className="button button-primary" onClick={close}>
                    {t("action.close")}
                  </button>
                </div>
              </div>
            ) : (
              <form className="sheet-body" onSubmit={submit}>
                {error ? (
                  <p className="banner banner-warn" role="alert">
                    {error}
                  </p>
                ) : null}

                <AccountFields
                  form={form}
                  onChange={setForm}
                  disabled={phase === "validating"}
                  autoFocusName
                />

                {phase === "validating" ? (
                  <p className="field-hint" role="status">
                    {t("account.validating")}
                  </p>
                ) : null}

                <div className="sheet-actions">
                  <button
                    type="button"
                    className="button button-quiet"
                    onClick={close}
                    disabled={phase === "validating"}
                  >
                    {t("action.cancel")}
                  </button>
                  <button
                    type="submit"
                    className="button button-primary"
                    disabled={phase === "validating"}
                  >
                    {t("account.submit")}
                  </button>
                </div>
              </form>
            )}
          </div>
        </div>
      ) : null}
    </>
  );
}
