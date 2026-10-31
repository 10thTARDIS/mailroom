// SPDX-License-Identifier: AGPL-3.0-only

// ACCOUNTS (#374 increment 3) — the row that is really twelve rows deep, so it is
// TWO SCREENS with a URL each.
//
//   #/settings/accounts            one line per account: dot, name, state, age
//   #/settings/accounts/<id>       that account's six controls
//
// The list is a list of LINKS, not a list of disclosures, and the difference
// matters: an account holds six controls and a folder list, which is more than a
// row can hold without hiding the eleven rows below it. The detail route was
// already parsed and formatted by increment 2 (core/routes.ts), so this only had
// to render it.
//
// EVERY CONTROL STATES ITS CURRENT VALUE FIRST. That is the page's rule and it is
// what makes the controls readable without operating them: "paused" above a
// Resume button, `#3B6FB6` above the swatches, the hosts above "Change
// credentials…". A control that only showed its options would make the user click
// one to find out where they were.
//
// ⚠ NOTHING HERE CLAIMS A STATE THE SERVER HAS NOT CONFIRMED. Every write is
// followed by `useAccounts().reload()` — the same `Account/get` + `Account/getHealth`
// pair the rail reads — and the button stays busy until that lands. There is no
// optimistic "paused": "this account is not syncing" is a statement about the
// user's mail not arriving, and core/actions.ts setMailboxSyncScope draws the same
// line for the same reason.
//
// TWO THINGS THE DESIGN ASKED FOR THAT THE SERVER CANNOT ANSWER, each stated
// where it would have gone rather than faked:
//
//   1. THE USERNAME IS NOT SHOWN. `Account/get` never returns it — accountObject:
//      "It NEVER carries the password or username … username lives only inside
//      the sealed secret". So the Server control prints the endpoints it has and
//      the credentials form asks for the username again, which is exactly what
//      the server's own comment says a connection change must do.
//   2. RULES ARE NOT ACCOUNT-SCOPED. `RuleField` has no account selector
//      (core/jmap/types.ts), so "rules limited to this account" is not a set that
//      exists. The `more` block says `rules are login-wide` and links to the row
//      that owns them.
//
// A per-account NOTIFICATION override is likewise absent server-side. It gets a
// sentence in `more` and no control — the design said not to render a disabled
// one, and this page's rule is that an absent feature is stated rather than
// implied by its absence (rows/NotificationsRow.tsx does the same for quiet
// hours).

import { useEffect, useRef, useState } from "react";
import type { JSX } from "react";

import { t, tCount } from "../../../i18n";
import type { MessageKey } from "../../../i18n";
import { useApp, useMailState } from "../../../state";
import type { Account, AccountGmailImport, AccountHealth, Id, Rule } from "../../../../core/jmap/types";
import { ConfirmDialog } from "../../desk/ConfirmDialog";
import { syncedAgoText } from "../../desk/format";
import { forgetAccounts, useAccounts } from "../../useAccounts";
import type { AccountEntry } from "../../useAccounts";
import {
  ACCOUNT_CONNECTION_NOTE,
  AccountFields,
  AddAccountWizard,
  accountFormError,
  accountFormPayload,
  emptyAccountForm,
} from "../../AddAccountWizard";
import type { AccountFormValues } from "../../AddAccountWizard";
import { ACCOUNT_COLOR_PRESETS } from "../accountColor";
import { foldersForAccount } from "../foldersForAccount";
import { useKeepScope } from "../nav";
import { AccountPushToggle } from "./NotificationControls";
import { OAuthConnectButtons } from "./OAuthConnect";
import { KeyConfirmDialog } from "../keys/KeyDialog";
import { canDisconnect, canReconnect, isOAuthAccount, revocationKind } from "../../../../core/oauth";
import { startOAuth } from "../../oauthReturn";
import { providerName, startErrorText } from "../../oauthWords";

/** The status word beside a name — the same three the state line uses, so the row
 * heading and the row body cannot describe an account differently. An OAuth
 * account whose sign-in is not working says WHICH way first (#52): "paused"
 * for a disconnected account, or "failing" for one waiting to be reconnected,
 * would leave the user guessing what to do. */
function statusKey(entry: AccountEntry): MessageKey {
  const auth = entry.record?.authState;
  if (auth === "needs_reauth") return "settings.state.acctReconnect";
  if (auth === "operator_error") return "settings.state.acctOperator";
  if (auth === "disconnected") return "settings.state.acctDisconnected";
  if (entry.status === "paused") return "settings.state.acctPaused";
  if (entry.status === "failing") return "settings.state.acctFailing";
  return "settings.state.acctOk";
}

/**
 * How long ago this account last succeeded, or nothing at all.
 *
 * ⚠ THREE STATES, NOT TWO. No health card means the telemetry has not been read
 * and NOTHING is said; a card with `lagSeconds: null` means it has been read and
 * this account has never synced, which is a fact worth printing. Collapsing them
 * would put "never synced" under every account for the length of a round trip on
 * every page load — an outage this client invented.
 */
function syncedClause(health: AccountHealth | null): string | null {
  if (health === null) return null;
  const { lagSeconds } = health.syncStatus;
  if (lagSeconds === null) return t("settings.account.neverSynced");
  return t("settings.account.lastSynced", { ago: syncedAgoText(lagSeconds) });
}

// ---------------------------------------------------------------------------
// The list
// ---------------------------------------------------------------------------

/** Admins only (#52): providers refusing this server's sign-in client. The
 * server sends these to admins alone; for anyone else the list is absent. */
function OperatorNotices(): JSX.Element | null {
  const { operatorNotices } = useAccounts();
  if (!operatorNotices || operatorNotices.length === 0) return null;
  return (
    <div className="banner banner-error setting-operator-notices" role="status">
      <p className="setting-operator-title">{t("settings.operator.title")}</p>
      {operatorNotices.map((n) => (
        <p key={n.provider}>
          {t("settings.operator.notice", {
            provider: providerName(n.provider),
            code: n.code,
            since: new Date(n.since).toLocaleString(),
            accounts: n.accounts,
          })}
        </p>
      ))}
    </div>
  );
}

function AccountList(): JSX.Element {
  const { accounts, loaded } = useAccounts();
  const go = useKeepScope();

  if (accounts.length === 0) {
    // NOT "you have no accounts". The list is derived from synced Identities
    // (useAccounts.ts), so an empty one before they arrive means the sync has not
    // got there yet — and telling a user with three accounts that they have none
    // is the worst available answer.
    return (
      <>
        <OperatorNotices />
        <p className="field-hint">{t("settings.account.none")}</p>
        <OAuthConnectButtons />
        <AddAccountWizard trigger="words" />
      </>
    );
  }

  return (
    <>
      <OperatorNotices />
      <ul className="setting-account-list">
        {accounts.map((entry) => {
          const synced = syncedClause(entry.health);
          const reason =
            entry.status === "failing" ? (entry.health?.syncStatus.lastError ?? null) : null;
          return (
            <li key={entry.id} className="setting-account">
              <button
                type="button"
                className="link-button setting-account-open"
                onClick={() =>
                  go({ page: "settings", section: "accounts", accountId: entry.id })
                }
              >
                <span
                  className={`desk-acct-dot is-acct-${entry.tone}`}
                  aria-hidden="true"
                  style={entry.color === null ? undefined : { background: entry.color }}
                />
                <span className="setting-account-name">{entry.name}</span>
              </button>
              {/* THE STATUS IS OMITTED UNTIL THE FETCH LANDS. "ok" states that
                  this account's mail is arriving, and saying it before asking is
                  how a settings page invents good news (SettingsRoute.tsx
                  accountFacts says the same about the state line). */}
              {loaded ? (
                <span
                  className={
                    entry.status === "ok" ? "remote-allow-kind" : "remote-allow-kind is-warn"
                  }
                >
                  {reason === null
                    ? t(statusKey(entry))
                    : `${t(statusKey(entry))}: ${reason}`}
                </span>
              ) : null}
              {synced === null ? null : <span className="remote-allow-kind">{synced}</span>}
              {entry.hasIdentity ? null : (
                <span className="remote-allow-kind">{t("settings.account.noAddress")}</span>
              )}
            </li>
          );
        })}
      </ul>
      <OAuthConnectButtons />
      <AddAccountWizard trigger="words" />
    </>
  );
}

// ---------------------------------------------------------------------------
// 1. State
// ---------------------------------------------------------------------------

/**
 * The status SENTENCE — the long form of the list's one word, built from the three
 * things the health card actually carries.
 *
 * `null` health is its own sentence and not the healthy one: an account whose
 * telemetry has not been read is not "syncing normally", it is unread.
 */
function stateSentence(entry: AccountEntry): string {
  if (entry.status === "paused") return t("settings.account.state.paused");
  const health = entry.health;
  if (health === null) return t("settings.account.state.unknown");
  const { lastError, lastSuccessAt, consecutiveFailures } = health.syncStatus;
  if (lastError !== null) {
    const failures =
      consecutiveFailures > 0
        ? ` ${tCount("settings.account.state.failures", consecutiveFailures)}`
        : "";
    return `${t("settings.account.state.failing", { error: lastError })}${failures}`;
  }
  if (lastSuccessAt === null) return t("settings.account.state.okNever");
  return t("settings.account.state.ok", { ago: syncedAgoText(health.syncStatus.lagSeconds) });
}

/**
 * The Gmail import progress sentence (#914). The server paces the history
 * import against Google's daily download limit, so the ETA it sends is a pace,
 * not a promise. A `null` field is the server saying it cannot say yet — no
 * poll of the server's count yet, or no average size yet — which reads as the
 * sentence WITHOUT the clause; the client never fills one in.
 */
function gmailImportSentence(progress: AccountGmailImport): string {
  if (progress.historyComplete) return t("settings.account.gmailImport.done");
  const done = progress.messagesImported.toLocaleString();
  const sentence =
    progress.messagesOnServer === null
      ? t("settings.account.gmailImport.progressUnknownTotal", { done })
      : t("settings.account.gmailImport.progress", {
          done,
          total: progress.messagesOnServer.toLocaleString(),
        });
  if (progress.etaDays === null) return sentence;
  const eta =
    progress.etaDays <= 1
      ? t("settings.account.gmailImport.etaSoon")
      : t("settings.account.gmailImport.eta", { days: progress.etaDays });
  return `${sentence} ${eta}`;
}

function StateControl({ entry }: { entry: AccountEntry }): JSX.Element {
  const { actions } = useApp();
  const { reload } = useAccounts();
  const [busy, setBusy] = useState(false);
  // The Pause/Resume button is only offered once the RECORD has landed, because
  // `syncEnabled` is what it toggles: a button labelled from a guess would offer
  // "Pause" for an account that is already paused, and the write would be a no-op
  // the user reads as a broken control.
  const record = entry.record;
  const paused = record !== null && !record.syncEnabled;
  // THE SERVER HOLDS THIS ACCOUNT'S SYNC OFF (#52: a connected Gmail account
  // until Mailroom's Gmail sync model lands). Its own sentence replaces the
  // Resume button, which the server would refuse: a control that can only fail
  // is not offered.
  const hold = entry.health?.syncHold ?? null;
  if (hold !== null) {
    return (
      <fieldset className="menu-group">
        <legend>{t("settings.account.state")}</legend>
        <p className="menu-note" role="status">
          {hold.message}
        </p>
      </fieldset>
    );
  }

  // A syncing Gmail account is mid-history-import (#914): the progress is the
  // sentence ABOVE the control, because the account IS syncing — the
  // Pause/Resume button below is the one that stops the import.
  const progress = entry.health?.gmailImport ?? null;

  const toggle = (): void => {
    if (record === null) return;
    setBusy(true);
    void (async () => {
      const ok = await actions.updateAccount(
        entry.id,
        { syncEnabled: paused },
        paused ? t("settings.account.resumeFailed") : t("settings.account.pauseFailed"),
      );
      // The server's answer, not ours — and awaited, so the label does not flip
      // back and forth while the read is in flight.
      if (ok) await reload();
      setBusy(false);
    })();
  };

  return (
    <fieldset className="menu-group">
      <legend>{t("settings.account.state")}</legend>
      <p className="menu-note" role="status">
        {stateSentence(entry)}
      </p>
      {progress === null ? null : (
        <p className="menu-note" role="status">
          {gmailImportSentence(progress)}
        </p>
      )}
      {record === null ? null : (
        <button
          type="button"
          className="button button-quiet menu-action"
          disabled={busy}
          onClick={toggle}
        >
          {busy
            ? t("settings.account.working")
            : paused
              ? t("settings.account.resume")
              : t("settings.account.pause")}
        </button>
      )}
    </fieldset>
  );
}

// ---------------------------------------------------------------------------
// 1a. Writeback (ADR-028, #521)
// ---------------------------------------------------------------------------

/**
 * Ingest-only mode: whether Mailroom actually writes to this account right
 * now, and the switch that turns it off.
 *
 * ⚠ THE STATUS SENTENCE READS `entry.health.writeback`, NOT
 * `entry.record.writebackEnabled`. The checkbox below patches the account's
 * own column, but the column is only HALF of whether writes happen — an
 * instance-level switch ANDs in on top of it (ADR-028 §2) and the API cannot
 * be told to override it. Reporting the column back at the user here could
 * say "writing normally" while the instance switch is silently refusing
 * every write; health carries the EFFECTIVE value because it is computed
 * from both.
 *
 * The checkbox itself still targets the column, because that is the one
 * thing this control can change — and a refusal (the instance switch is set
 * while this tries to turn writeback back on) surfaces exactly the way every
 * other `invalidProperties` response does here: `actions.updateAccount`
 * already reads the server's own per-property description into the toast
 * (`describeSetError`), so no special case is needed for this property.
 */
function WritebackControl({ entry }: { entry: AccountEntry }): JSX.Element {
  const { actions } = useApp();
  const { reload } = useAccounts();
  const [busy, setBusy] = useState(false);
  const record = entry.record;
  const columnOn = record !== null && record.writebackEnabled;
  const effective = entry.health?.writeback ?? null;

  const toggle = (): void => {
    if (record === null) return;
    setBusy(true);
    void (async () => {
      const ok = await actions.updateAccount(
        entry.id,
        { writebackEnabled: !columnOn },
        columnOn
          ? t("settings.account.writeback.disableFailed")
          : t("settings.account.writeback.enableFailed"),
      );
      if (ok) await reload();
      setBusy(false);
    })();
  };

  return (
    <fieldset className="menu-group">
      <legend>{t("settings.account.writeback")}</legend>
      <p className="menu-note" role="status">
        {effective === null
          ? t("settings.account.writeback.unknown")
          : effective.enabled
            ? t("settings.account.writeback.writing")
            : t("settings.account.writeback.shadowed")}
      </p>
      {/* The ledger, stated as its own line rather than folded into the
          sentence above — ADR-028 §4 is explicit that it does not fall to
          zero merely because the mode is lifted, so it is a fact worth
          printing on its own rather than a clause that reads like a queue
          depth. */}
      {effective !== null && !effective.enabled ? (
        <p className="menu-note">
          {tCount("settings.account.writeback.suppressed", effective.suppressedIntents)}
        </p>
      ) : null}
      {record === null ? (
        <p className="field-hint">{t("settings.account.writeback.unread")}</p>
      ) : (
        <>
          <label className="checkbox">
            <input
              type="checkbox"
              checked={!columnOn}
              disabled={busy}
              onChange={toggle}
            />
            {t("settings.account.writeback.label")}
          </label>
          <p className="field-hint">{t("settings.account.writeback.explain")}</p>
        </>
      )}
    </fieldset>
  );
}

// ---------------------------------------------------------------------------
// 2. Colour
// ---------------------------------------------------------------------------

/** How long the native colour input waits after the last movement before writing.
 * It fires on every step of a drag through the picker, so without this one choice
 * would be dozens of `Account/set` calls. */
const COLOR_COMMIT_MS = 400;

function ColourControl({ entry }: { entry: AccountEntry }): JSX.Element {
  const { actions } = useApp();
  const { reload } = useAccounts();
  const [busy, setBusy] = useState(false);
  const record = entry.record;
  // The STORED value, not `entry.color` — a paused account reports null there
  // (accountColor.ts: paused beats a chosen colour), and a picker that showed no
  // colour for a paused account would look like the choice had been lost.
  const stored = record?.color ?? null;
  /** What the native input currently shows, while it is being dragged. */
  const [picked, setPicked] = useState<string | null>(null);

  const write = (color: string | null): void => {
    setBusy(true);
    void (async () => {
      const ok = await actions.updateAccount(
        entry.id,
        { color },
        t("settings.account.colorFailed"),
      );
      if (ok) await reload();
      setBusy(false);
      setPicked(null);
    })();
  };

  /**
   * ⚠ A TIMER RATHER THAN `onBlur`, and the difference is whether the write happens
   * at all. A native colour picker is an OS surface: the user opens it, chooses, and
   * closes it — focus comes back to the input and never leaves, so a blur-committed
   * write does not land until they click something else. Someone who picked a colour
   * and then reloaded would find it gone, which is the worst outcome available. So
   * the last value wins after a short pause, whether or not focus moves.
   */
  const commitTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(
    () => () => {
      if (commitTimer.current !== null) clearTimeout(commitTimer.current);
    },
    [],
  );
  const commitLater = (color: string): void => {
    setPicked(color);
    if (commitTimer.current !== null) clearTimeout(commitTimer.current);
    commitTimer.current = setTimeout(() => {
      commitTimer.current = null;
      if (color !== stored) write(color);
      else setPicked(null);
    }, COLOR_COMMIT_MS);
  };

  return (
    <fieldset className="menu-group">
      <legend>{t("settings.account.color")}</legend>
      <p className="menu-note">
        {stored === null ? (
          t("settings.account.color.none")
        ) : (
          <>
            <span
              className="setting-swatch-current"
              aria-hidden="true"
              style={{ background: stored }}
            />
            {stored}
          </>
        )}
      </p>
      {record === null ? (
        // Nothing to write to yet: the patch needs the record's id, and the id is
        // the account's — but a picker offered before `Account/get` answered would
        // show "none" for an account that has a colour.
        <p className="field-hint">{t("settings.account.color.unread")}</p>
      ) : (
        <>
          <div className="setting-swatches">
            {ACCOUNT_COLOR_PRESETS.map((preset) => (
              <button
                key={preset.hex}
                type="button"
                className={
                  stored !== null && stored.toUpperCase() === preset.hex.toUpperCase()
                    ? "setting-swatch is-current"
                    : "setting-swatch"
                }
                // NAMED, not just coloured: a row of unlabelled squares is a
                // control only a sighted user can operate, and `aria-pressed` is
                // what says which one is on without relying on the ring around it.
                aria-label={t("settings.account.color.pick", {
                  name: t(`settings.account.color.${preset.name}` as MessageKey),
                })}
                aria-pressed={stored !== null && stored.toUpperCase() === preset.hex.toUpperCase()}
                title={t(`settings.account.color.${preset.name}` as MessageKey)}
                disabled={busy}
                style={{ background: preset.hex }}
                onClick={() => write(preset.hex)}
              />
            ))}
          </div>
          <label className="field field-inline">
            <span className="field-label">{t("settings.account.color.custom")}</span>
            <input
              type="color"
              className="setting-color-input"
              // The native control needs SOME value; the first preset is the
              // starting point when nothing is stored, chosen rather than left to
              // the browser's default black.
              value={picked ?? stored ?? (ACCOUNT_COLOR_PRESETS[0]?.hex ?? "#3B6FB6")}
              disabled={busy}
              onChange={(e) => commitLater(e.target.value)}
            />
          </label>
          {stored === null ? null : (
            <button
              type="button"
              className="link-button"
              disabled={busy}
              onClick={() => write(null)}
            >
              {t("settings.account.color.clear")}
            </button>
          )}
          <p className="field-hint">{t("settings.account.color.explain")}</p>
        </>
      )}
    </fieldset>
  );
}

// ---------------------------------------------------------------------------
// 3. Server
// ---------------------------------------------------------------------------

function CredentialsForm({
  account,
  onDone,
}: {
  account: Account;
  onDone: () => void;
}): JSX.Element {
  const { actions } = useApp();
  const { reload } = useAccounts();
  const [form, setForm] = useState<AccountFormValues>({
    ...emptyAccountForm,
    // Pre-filled from the record, which carries everything EXCEPT the two
    // credential fields. The user re-types those, which is what the server
    // requires (ACCOUNT_CONNECTION_NOTE says why in full).
    name: account.name,
    imapHost: account.imapHost,
    imapPort: String(account.imapPort),
    smtpHost: account.smtpHost,
    smtpPort: String(account.smtpPort),
  });
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = (e: React.FormEvent): void => {
    e.preventDefault();
    const bad = accountFormError(form);
    if (bad) {
      setError(t(bad));
      return;
    }
    setBusy(true);
    setError(null);
    void (async () => {
      // The FULL block, exactly as a create sends it: a connection change is
      // classified by the presence of any endpoint/credential field and then
      // validated as a whole, so a partial patch is refused rather than merged.
      const ok = await actions.updateAccount(
        account.id,
        accountFormPayload(form),
        t("settings.account.credentialsFailed"),
      );
      setBusy(false);
      if (!ok) {
        // The refusal itself has already gone to a toast with the server's own
        // per-field description (actions.updateAccount surfaces it verbatim); this
        // keeps the form open and says so, rather than closing over the failure.
        setError(t("settings.account.credentialsRefused"));
        return;
      }
      await reload();
      onDone();
    })();
  };

  return (
    <form className="setting-subform" onSubmit={submit}>
      {error ? (
        <p className="banner banner-warn" role="alert">
          {error}
        </p>
      ) : null}
      <p className="field-hint">{t(ACCOUNT_CONNECTION_NOTE)}</p>
      <AccountFields
        form={form}
        onChange={setForm}
        disabled={busy}
        passwordHint="settings.account.password.hint"
      />
      {busy ? (
        <p className="field-hint" role="status">
          {t("account.validating")}
        </p>
      ) : null}
      <div className="sheet-actions">
        <button type="button" className="button button-quiet" onClick={onDone} disabled={busy}>
          {t("action.cancel")}
        </button>
        <button type="submit" className="button button-primary" disabled={busy}>
          {t("settings.account.credentialsSave")}
        </button>
      </div>
    </form>
  );
}

// ---------------------------------------------------------------------------
// 3b. Sign-in, for an account that signs in at its provider (#52)
// ---------------------------------------------------------------------------

/** The sentence for an OAuth account's sign-in state. */
function signInSentence(state: string | null | undefined, provider: string): string {
  switch (state) {
    case "needs_reauth":
      return t("settings.account.signIn.needsReauth", { provider });
    case "operator_error":
      return t("settings.account.signIn.operatorError", { provider });
    case "disconnected":
      return t("settings.account.signIn.disconnected");
    default:
      return t("settings.account.signIn.active");
  }
}

/** What a disconnect left behind at the provider, in words. */
function disconnectedText(provider: string | null, revocation: string): string {
  if (revocationKind(revocation) === "revoked") {
    return t("settings.account.disconnect.revoked", { provider: providerName(provider) });
  }
  if (provider === "google") return t("settings.account.disconnect.manual.google");
  if (provider === "microsoft") return t("settings.account.disconnect.manual.microsoft");
  return t("settings.account.disconnect.manual.unknown");
}

/**
 * Who this account signs in as, whether that is working, and the two things a
 * person can do about it: Reconnect (when the provider wants them back, or
 * after a disconnect) and Disconnect (which revokes where the provider allows
 * it and KEEPS the mail).
 *
 * ⚠ NO HOST, PORT OR PASSWORD EDITOR. An OAuth account's servers are its
 * provider's, fixed by the server's registry, and Account/set refuses any
 * change to them (ADR-046 §9 (e)); a form that could only be refused is not
 * offered. operator_error offers no Reconnect either: it is the server's
 * registration that is broken, and signing in again cannot fix it.
 */
function SignInControl({ entry, record }: { entry: AccountEntry; record: Account }): JSX.Element {
  const { client } = useApp();
  const { reload } = useAccounts();
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const provider = record.authProvider ?? entry.health?.auth?.provider ?? null;
  const name = providerName(provider);
  const reason = entry.health?.auth?.reason ?? null;

  const reconnect = (): void => {
    setBusy(true);
    setError(null);
    void startOAuth(client, provider ?? "", record.id).then((err) => {
      if (err === null) return; // leaving for the provider
      setBusy(false);
      setError(startErrorText(err, provider));
    });
  };

  const disconnect = (): void => {
    setBusy(true);
    setError(null);
    void (async () => {
      try {
        const res = await client.accountOAuthDisconnect(record.id);
        setConfirming(false);
        setDone(disconnectedText(provider, res.revocation));
        await reload();
      } catch (err) {
        setError(t("settings.account.disconnect.failed", { error: err instanceof Error ? err.message : String(err) }));
      } finally {
        setBusy(false);
      }
    })();
  };

  return (
    <fieldset className="menu-group">
      <legend>{t("settings.account.signIn")}</legend>
      <p className="menu-note">
        {record.authAddress
          ? t("settings.account.signIn.via", { provider: name, address: record.authAddress })
          : t("settings.account.signIn.viaNoAddress", { provider: name })}
      </p>
      <p className="menu-note" role="status">
        {done ?? signInSentence(record.authState, name)}
      </p>
      {reason !== null && record.authState !== "active" && done === null ? (
        <p className="field-hint">{t("settings.account.signIn.reason", { reason })}</p>
      ) : null}
      {error === null || confirming ? null : (
        <p className="banner banner-error" role="alert">
          {error}
        </p>
      )}
      <div className="setting-signin-actions">
        {canReconnect(record) ? (
          <button type="button" className="button button-primary menu-action" disabled={busy} onClick={reconnect}>
            {t("settings.account.signIn.reconnect")}
          </button>
        ) : null}
        {canDisconnect(record) ? (
          <button type="button" className="button button-quiet menu-action" disabled={busy} onClick={() => setConfirming(true)}>
            {t("settings.account.signIn.disconnect")}
          </button>
        ) : null}
      </div>
      <p className="field-hint">{t("settings.account.signIn.noServer", { provider: name })}</p>
      {confirming ? (
        <KeyConfirmDialog
          title={t("settings.account.disconnect.title", { name: entry.name })}
          paragraphs={[t("settings.account.disconnect.body")]}
          confirmLabel={t("settings.account.disconnect.confirm")}
          busyLabel={t("settings.account.working")}
          busy={busy}
          error={error}
          onConfirm={disconnect}
          onClose={() => {
            setConfirming(false);
            setError(null);
          }}
        />
      ) : null}
    </fieldset>
  );
}

function ServerControl({ entry }: { entry: AccountEntry }): JSX.Element {
  const [editing, setEditing] = useState(false);
  const record = entry.record;
  if (record !== null && isOAuthAccount(record)) return <SignInControl entry={entry} record={record} />;

  return (
    <fieldset className="menu-group">
      <legend>{t("settings.account.server")}</legend>
      {record === null ? (
        <p className="field-hint">{t("settings.account.server.unread")}</p>
      ) : (
        <>
          <p className="menu-note">
            {t("settings.account.server.imap", {
              host: record.imapHost,
              port: record.imapPort,
            })}
          </p>
          <p className="menu-note">
            {t("settings.account.server.smtp", {
              host: record.smtpHost,
              port: record.smtpPort,
            })}
          </p>
          {/* THE USERNAME IS NOT HERE AND THAT IS SAID OUT LOUD. Account/get never
              returns it (accountObject: it "lives only inside the sealed secret"),
              so the alternative to this sentence is a reader concluding the field
              is empty server-side. */}
          <p className="field-hint">{t("settings.account.server.noUsername")}</p>
          {editing ? (
            <CredentialsForm account={record} onDone={() => setEditing(false)} />
          ) : (
            <button
              type="button"
              className="button button-quiet menu-action"
              onClick={() => setEditing(true)}
            >
              {t("settings.account.credentials")}
            </button>
          )}
        </>
      )}
    </fieldset>
  );
}

// ---------------------------------------------------------------------------
// 4. Folders synced
// ---------------------------------------------------------------------------

/**
 * The per-folder sync switch, MOVED HERE from the rail's Folders disclosure
 * (ui/Sidebar.tsx, #62 → #374).
 *
 * ⚠ THE STATE STAYED IN THE RAIL. Only the control moved: a folder with syncing
 * off is a frozen snapshot, and the rail still tags it, because the fact belongs
 * where the folder is and the CONTROL belongs where folders are configured.
 *
 * FILTERED TO THIS ACCOUNT (#488): Mailbox now carries `accountId`, so
 * `foldersForAccount` (settings/foldersForAccount.ts) shows only the folders
 * that belong to the account this row sits inside — never another account's,
 * whatever its folders happen to be named.
 */
function FoldersControl({ accountId }: { accountId: Id }): JSX.Element {
  const { actions } = useApp();
  const state = useMailState();
  const [busy, setBusy] = useState<Id | null>(null);

  // The roled ones (Inbox, Sent, Archive…) are the server's own and are not
  // offered as pausable, exactly as before the move; foldersForAccount also
  // drops any mailbox belonging to a DIFFERENT account (#488).
  const folders = foldersForAccount(Object.values(state.mailboxes), accountId);

  const toggle = (id: Id, off: boolean): void => {
    setBusy(id);
    void (async () => {
      await actions.setMailboxSyncScope(id, off ? "full" : "off");
      setBusy(null);
    })();
  };

  return (
    <fieldset className="menu-group">
      <legend>{t("settings.account.folders")}</legend>
      <p className="field-hint">{t("settings.account.folders.explain")}</p>
      {folders.length === 0 ? (
        // A measured empty only in the sense that the mailbox list is synced
        // reference data: no non-roled folders is an ordinary provider layout, not
        // a failure, and the sentence says which.
        <p className="field-hint">{t("settings.account.folders.none")}</p>
      ) : (
        <ul className="remote-allow-list">
          {folders.map((m) => {
            const off = m.syncScope === "off";
            return (
              <li key={m.id} className="remote-allow">
                <span className="setting-folder-name">{m.name}</span>
                <span className={off ? "remote-allow-kind is-warn" : "remote-allow-kind"}>
                  {t(off ? "settings.account.folders.paused" : "settings.account.folders.syncing")}
                </span>
                <button
                  type="button"
                  className="link-button"
                  disabled={busy === m.id}
                  // The rail's own hover explanations, kept: the button label says
                  // what the click does and the title says what it COSTS, which is
                  // the half a one-line label cannot carry.
                  title={t(off ? "sync.resumeHint" : "sync.pauseHint")}
                  onClick={() => toggle(m.id, off)}
                >
                  {t(off ? "sync.resumeFolder" : "sync.pauseFolder", { name: m.name })}
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </fieldset>
  );
}

// ---------------------------------------------------------------------------
// 5. Identities
// ---------------------------------------------------------------------------

/**
 * How many identities this account sends as, and the way to them.
 *
 * ⚠ IT LINKS TO THE ROW, NOT TO A FILTERED ROW, and the design asked which
 * mechanism would carry the filter. Neither fits:
 *
 *   * `?acct=<id>` is not a row filter, it is THE SCOPE OF THE LIST UNDERNEATH
 *     settings (core/routes.ts withAcct), and `rememberedFrom` writes any route's
 *     `acct` into the per-device scope memory — so following such a link would
 *     silently re-scope the user's whole mail view to one account.
 *   * a settings-route detail segment exists for `accounts` and `areas` only
 *     (parseRoute), so `#/settings/identities/<id>` parses as the plain row.
 *
 * And no filter is needed: the Identities row is ALREADY GROUPED BY ACCOUNT with
 * the account's name as the group heading (settings/precedence.ts groups it, and
 * says why the grouping is the honest shape), so the link lands on a page where
 * this account's addresses are a titled block. A filter would have hidden the
 * other accounts' addresses, which is a narrowing nobody asked for.
 */
function IdentitiesControl({ accountId }: { accountId: Id }): JSX.Element {
  const state = useMailState();
  const go = useKeepScope();
  const count = Object.values(state.identities).filter((i) => i.accountId === accountId).length;

  return (
    <fieldset className="menu-group">
      <legend>{t("settings.account.identities")}</legend>
      <p className="menu-note">{tCount("settings.state.identityCount", count)}</p>
      {count === 0 ? <p className="menu-note">{t("settings.account.noAddress")}</p> : null}
      <button
        type="button"
        className="link-button"
        onClick={() => go({ page: "settings", section: "identities" })}
      >
        {t("settings.account.identities.open")}
      </button>
    </fieldset>
  );
}

// ---------------------------------------------------------------------------
// The detail, and the row's two bodies
// ---------------------------------------------------------------------------

export function AccountsBody({ accountId }: { accountId: Id | null }): JSX.Element {
  // ⚠ CALLED HERE AS WELL AS IN SettingsRoute, AND THAT IS SAFE FOR A REASON THE
  // Appearance row cannot claim: `useAccounts` reads a MODULE-level store through
  // useSyncExternalStore (ui/useAccounts.ts), so every caller gets the same
  // snapshot and one fetch. A hook with component state — `useAppearance` — must
  // be called once and passed down, and says so.
  const { byId } = useAccounts();

  if (accountId === null) return <AccountList />;

  const entry = byId.get(accountId);
  if (!entry) {
    return (
      <>
        {/* Not an error: two tabs, or an account removed in the other one. The way
            back is the same one the detail has. */}
        <p className="field-hint">{t("settings.account.notFound")}</p>
        <BackToAccounts />
      </>
    );
  }

  return (
    <>
      <BackToAccounts />
      <h4 className="sheet-section">{entry.name}</h4>
      <StateControl entry={entry} />
      <WritebackControl entry={entry} />
      <ColourControl entry={entry} />
      <ServerControl entry={entry} />
      <FoldersControl accountId={entry.id} />
      <IdentitiesControl accountId={entry.id} />
    </>
  );
}

function BackToAccounts(): JSX.Element {
  const go = useKeepScope();
  return (
    <button
      type="button"
      className="link-button setting-back"
      onClick={() => go({ page: "settings", section: "accounts" })}
    >
      {t("settings.account.back")}
    </button>
  );
}

/**
 * The row's `more`, which is the ACCOUNT's more: it only exists inside an
 * account, because the list itself has nothing rare to hold.
 *
 * Remove account is here rather than beside the pause button for the obvious
 * reason and one less obvious one: they are adjacent in meaning ("stop this
 * account") and wildly different in consequence, and a destructive button next to
 * a reversible one is a mis-click that cannot be undone.
 */
export function AccountsMore({
  accountId,
  rules,
}: {
  accountId: Id;
  rules: Rule[] | null;
}): JSX.Element | null {
  const { actions } = useApp();
  const { byId } = useAccounts();
  const go = useKeepScope();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const entry = byId.get(accountId);
  if (!entry) return null;

  const remove = (): void => {
    setBusy(true);
    void (async () => {
      const ok = await actions.destroyAccount(accountId);
      setBusy(false);
      if (!ok) return;
      setConfirming(false);
      // The cache is dropped rather than re-read: the account is gone, and
      // `forgetAccounts` also clears `everRequested` so the next render fetches
      // afresh instead of showing the removed account until something else asks.
      forgetAccounts();
      go({ page: "settings", section: "accounts" });
    })();
  };

  return (
    <>
      <fieldset className="menu-group">
        <legend>{t("settings.account.provider")}</legend>
        <p className="menu-note">
          {entry.record === null
            ? t("settings.account.provider.unread")
            : entry.record.providerProfile === ""
              ? t("settings.account.provider.none")
              : entry.record.providerProfile}
        </p>
        <p className="field-hint">{t("settings.account.provider.explain")}</p>
      </fieldset>

      <fieldset className="menu-group">
        <legend>{t("settings.account.rules")}</legend>
        {/* `rules are login-wide` is a FINDING, not a placeholder: RuleField
            (core/jmap/types.ts) is from / fromDomain / to / subject / alias /
            listId / listUnsubscribe / header — there is no account selector, so a
            per-account subset of the rules is not a set that exists. */}
        <p className="menu-note">{t("settings.account.rules.loginWide")}</p>
        {rules === null ? null : (
          <p className="field-hint">{tCount("settings.account.rules.count", rules.length)}</p>
        )}
        <button
          type="button"
          className="link-button"
          onClick={() => go({ page: "settings", section: "rules" })}
        >
          {t("settings.account.rules.open")}
        </button>
      </fieldset>

      <fieldset className="menu-group">
        <legend>{t("settings.account.notifications")}</legend>
        {/* The same switch as the Notifications row's per-account list (#59). */}
        <AccountPushToggle entry={entry} />
        <p className="field-hint">{t("settings.account.notifications.where")}</p>
      </fieldset>

      <fieldset className="menu-group">
        <legend>{t("settings.account.remove")}</legend>
        <button
          type="button"
          className="button button-danger menu-action"
          onClick={() => setConfirming(true)}
        >
          {t("settings.account.remove.open")}
        </button>
      </fieldset>

      {confirming ? (
        <ConfirmDialog
          title={t("settings.account.remove.title", { name: entry.name })}
          // ⚠ THE SERVER'S OWN LIST, NOT A PARAPHRASE. internal/store/accounts.go
          // DeleteAccount: the cascade takes the folders, identities, snippets and
          // submissions; messages that lived ONLY in this account are HARD
          // DELETED (not trashed); a message shared with another account by
          // content hash is KEPT; a PGP key bound to one of those identities
          // survives with its binding cleared. Every clause of the sentence below
          // is one of those.
          body={t("settings.account.remove.body", { name: entry.name })}
          confirmLabel={t("settings.account.remove.confirm")}
          busy={busy}
          onConfirm={remove}
          onCancel={() => setConfirming(false)}
        />
      ) : null}
    </>
  );
}
