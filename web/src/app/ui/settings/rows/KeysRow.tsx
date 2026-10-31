// SPDX-License-Identifier: AGPL-3.0-only

// KEYS (#374 increment 2; the management page since #796, the warnings #795).
//
// THE ROW STILL HAS TO BE ABLE TO SAY "I CANNOT ANSWER THAT". `urn:mailroom:pgp`
// may be absent, or present without the v1.93 management surface, so the row
// has four states and only three of them are about keys:
//
//   capability absent  → "keys are not available on this server". Never "no
//                        key": a server that cannot be asked has not said you
//                        have none.
//   still asking       → no facts on the state line (a number nobody has read
//                        is not a zero, #176).
//   asked, none held   → "no key", and (with key management) Make / Import.
//   asked, held        → each key with its facts and the verbs its state allows
//                        (settings/keys/OwnKeyCard.tsx).
//
// The read helpers (core/jmap/client.ts userKeysRead / contactKeyCount) fold
// EVERY failure into null and never throw, so a server that answers a 500 lands
// in the first state rather than taking the settings page down.
//
// WHAT NEEDS A CRYPTO PROCESS. Generate, import, extend, back up and revoke
// touch the private half, so they exist only when the capability says
// `keyManagement: true`. Choosing an address, publishing and retiring are store
// writes and are offered either way (the server documents that).
//
// HISTORY (#815). Retired, revoked and unbound-expired keys are not keys in
// use: they sit in a collapsed History section under the list (each still a
// full card, because backing up or revoking a retired key is still possible),
// and "Replaced by" on one of them moves to the newer key, opening History
// first when that key is in it too.
//
// THE WARNING THRESHOLD IS THE SERVER'S (`keyExpiryWarningDays`), read from the
// capability and only ever used to word what the server's own `expiringSoon`
// already decided (core/userKeys.ts isExpiringSoon).

import { useCallback, useEffect, useState } from "react";
import type { JSX } from "react";

import { isPastKey } from "../../../../core/keyLifecycle";
import { fixedIdentities, isExpiringSoon, keyDateShort } from "../../../../core/userKeys";
import type { Id, Identity, UserKey } from "../../../../core/jmap/types";
import { t, tCount } from "../../../i18n";
import { useApp, useMailState } from "../../../state";
import type { StateFact } from "../SettingsRow";
import type { KeysApi } from "../keys/api";
import { CorrespondentKeys } from "../keys/CorrespondentKeys";
import { formatRoute } from "../../../../core/routes";
import { navigate } from "../../../routing";
import { GenerateKeyDialog } from "../keys/GenerateKeyDialog";
import { HistoryDisclosure } from "../keys/HistoryDisclosure";
import { ImportOwnKeyDialog } from "../keys/ImportOwnKeyDialog";
import { OwnKeyCard, keyHeadingId } from "../keys/OwnKeyCard";

export type KeysReading =
  /** The session does not advertise urn:mailroom:pgp, or a call failed. */
  | { kind: "unavailable" }
  /** The request is in flight. States nothing. */
  | { kind: "asking" }
  | {
      kind: "loaded";
      keys: UserKey[];
      correspondents: number | null;
      wkdConfigured: boolean | null;
      wkdDomain: string | null;
    };

/**
 * Reads the user's own keys and the correspondent-key count, once per mount of
 * the settings page, and again on `reload()` after a change. No retry on a
 * timer: a server that said no is not hammered.
 *
 * ⚠ GATED ON THE SESSION HAVING LANDED: `hasCapability` reads the session, which
 * is null on a cold boot, and a sticky "not available" from asking too early is
 * worse than a slow right answer (the same call ui/useAccounts.ts makes).
 */
export function useUserKeys(): { reading: KeysReading; reload: () => void } {
  const { client } = useApp();
  const state = useMailState();
  const [reading, setReading] = useState<KeysReading>({ kind: "asking" });
  const [generation, setGeneration] = useState(0);
  const accountId = state.accountId;
  useEffect(() => {
    if (!accountId) return;
    let cancelled = false;
    void (async () => {
      const read = await client.userKeysRead();
      if (cancelled) return;
      if (read === null) {
        setReading({ kind: "unavailable" });
        return;
      }
      const correspondents = await client.contactKeyCount();
      if (cancelled) return;
      setReading({
        kind: "loaded",
        keys: read.keys,
        correspondents,
        wkdConfigured: read.wkdConfigured,
        wkdDomain: read.wkdDomain,
      });
    })();
    return () => {
      cancelled = true;
    };
  }, [client, accountId, generation]);
  const reload = useCallback(() => setGeneration((g) => g + 1), []);
  return { reading, reload };
}

/** A fingerprint in groups of four (kept here for the page's search values). */
export function groupFingerprint(fingerprint: string): string {
  const clean = fingerprint.replace(/\s+/g, "").toUpperCase();
  return (clean.match(/.{1,4}/g) ?? []).join(" ");
}

/** The row's state-line facts. Empty while the request is in flight. A key
 * that has expired or is about to is a warn-toned fact (#795). */
export function keysStateText(
  reading: KeysReading,
  warnDays: number | null = null,
  now: Date = new Date(),
): StateFact[] {
  if (reading.kind === "unavailable") return [{ text: t("settings.keys.unavailable") }];
  if (reading.kind === "asking") return [];
  const { keys, correspondents } = reading;
  const facts: StateFact[] = [];
  if (keys.length === 0) facts.push({ text: t("settings.keys.none") });
  else if (keys.length === 1) {
    facts.push({
      text: keys[0]?.wkdPublished ? t("settings.keys.onePublished") : t("settings.keys.oneUnpublished"),
    });
  } else {
    facts.push({
      text: t("settings.keys.many", {
        count: keys.length,
        published: keys.filter((k) => k.wkdPublished).length,
      }),
    });
  }
  for (const k of keys) {
    if (k.decryptOnly || k.revokedAt !== null) continue;
    if (k.expired === true && k.identityId !== null) {
      facts.push({ text: t("settings.keys.factExpired"), tone: "warn" });
    } else if (k.expired !== true && k.expiresAt && isExpiringSoon(k, warnDays, now)) {
      facts.push({ text: t("settings.keys.factExpires", { date: keyDateShort(k.expiresAt, now) }), tone: "warn" });
    }
  }
  // Omitted at zero AND at null: null was never measured, and zero is already
  // implied by there being nothing to list.
  if (correspondents !== null && correspondents > 0) {
    facts.push({ text: tCount("settings.keys.correspondents", correspondents) });
  }
  return facts;
}

export function KeysBody({
  reading,
  identities,
  reload,
  api,
  keyManagement,
  warnDays,
  now = new Date(),
}: {
  reading: KeysReading;
  /** `state.identities`: which address each key is for, and "Signs as". */
  identities: readonly Identity[];
  reload: () => void;
  api: KeysApi;
  /** `client.pgpKeyManagement()`: null/false hides the crypto-process verbs. */
  keyManagement: boolean | null;
  /** `client.pgpKeyExpiryWarningDays()`. */
  warnDays: number | null;
  now?: Date;
}): JSX.Element {
  const [open, setOpen] = useState<null | "generate" | "import">(null);
  const [done, setDone] = useState<string | null>(null);
  const [historyOpen, setHistoryOpen] = useState(false);
  // "Replaced by" moves focus to another key's card; when that card is under
  // History, History opens first and the move happens once it is drawn.
  const [showKey, setShowKey] = useState<Id | null>(null);
  useEffect(() => {
    if (showKey === null) return;
    const el = document.getElementById(keyHeadingId(showKey));
    if (!el) return;
    el.scrollIntoView?.({ block: "center" });
    el.focus();
    setShowKey(null);
  }, [showKey, historyOpen]);

  if (reading.kind === "unavailable") {
    return <p className="field-hint">{t("settings.keys.unavailableLong")}</p>;
  }
  if (reading.kind === "asking") {
    return <p className="field-hint">{t("settings.keys.asking")}</p>;
  }
  const managed = keyManagement === true;
  const changed = (message: string): void => {
    setDone(message);
    reload();
  };
  // Keys in use first (those used for an address before those that are not);
  // retired, revoked and unbound-expired keys are History (#815), newest first.
  const rank = (k: UserKey): number =>
    k.revokedAt !== null ? 3 : k.decryptOnly ? 2 : k.identityId === null ? 1 : 0;
  const sorted = [...reading.keys].sort((a, b) => rank(a) - rank(b) || b.createdAt.localeCompare(a.createdAt));
  const keys = sorted.filter((k) => !isPastKey(k));
  const past = sorted.filter((k) => isPastKey(k));
  const byId = new Map(reading.keys.map((k) => [k.id, k]));
  const goToKey = (id: Id): void => {
    const target = byId.get(id);
    if (target && isPastKey(target)) setHistoryOpen(true);
    setShowKey(id);
  };
  const card = (k: UserKey): JSX.Element => (
    <OwnKeyCard
      key={k.id}
      keyItem={k}
      identities={identities}
      wkdConfigured={reading.wkdConfigured}
      wkdDomain={reading.wkdDomain}
      keyManagement={managed}
      warnDays={warnDays}
      now={now}
      api={api}
      onChanged={changed}
      findKey={(id) => byId.get(id)}
      onShowKey={goToKey}
    />
  );

  return (
    <div className="keys-page">
      <p className="setting-done" role="status">
        {done ?? ""}
      </p>
      <section className="own-keys" aria-labelledby="keys-yours-heading">
        <h4 id="keys-yours-heading">{t("keys.yours.heading")}</h4>
        <p className="field-hint">{t("keys.yours.explain")}</p>
        {warnDays !== null ? <p className="field-hint">{t("keys.yours.warnWindow", { days: warnDays })}</p> : null}
        {!managed ? <p className="field-hint">{t("keys.yours.noCustody")}</p> : null}
        {keys.length === 0 ? (
          <p className="field-hint">{t("settings.keys.noneLong")}</p>
        ) : (
          <ul className="own-key-list">{keys.map(card)}</ul>
        )}
        {managed ? (
          <div className="sheet-actions own-keys-new">
            <button type="button" className="button" onClick={() => setOpen("generate")}>
              {t("keys.generate.open")}
            </button>
            <button type="button" className="button" onClick={() => setOpen("import")}>
              {t("keys.importOwn.open")}
            </button>
          </div>
        ) : null}
        <HistoryDisclosure
          open={historyOpen}
          onToggle={setHistoryOpen}
          count={past.length}
          label={t("keys.history.yoursLabel")}
        >
          <p className="field-hint">{t("keys.history.yoursExplain")}</p>
          {past.length === 0 ? (
            <p className="field-hint">{t("keys.history.yoursNone")}</p>
          ) : (
            <ul className="own-key-list">{past.map(card)}</ul>
          )}
        </HistoryDisclosure>
      </section>

      <CorrespondentKeys
        api={api}
        onChanged={(m) => changed(m)}
        // #816: each key links to the page of the contact it is held under.
        onOpenContact={(id) => ({
          href: formatRoute({ page: "contacts", id }),
          open: () => navigate({ page: "contacts", id }),
        })}
      />

      {open === "generate" ? (
        <GenerateKeyDialog
          identities={fixedIdentities(identities)}
          now={now}
          warnDays={warnDays}
          onClose={() => setOpen(null)}
          onGenerate={async (identityId, expiry, bind) => {
            const out = await api.generate(identityId, expiry, bind);
            if (!out.ok) return out;
            api.afterChange();
            changed(t("keys.done.generated"));
            return { ok: true };
          }}
        />
      ) : null}
      {open === "import" ? (
        <ImportOwnKeyDialog
          onClose={() => setOpen(null)}
          onImport={(privateKey, passphrase) => api.importOwn(privateKey, passphrase)}
          onDone={(already) => {
            api.afterChange();
            changed(already ? t("keys.done.alreadyHeld") : t("keys.done.imported"));
          }}
        />
      ) : null}
    </div>
  );
}
