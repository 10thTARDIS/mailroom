// SPDX-License-Identifier: AGPL-3.0-only

// The Encrypt switch (#47, ADR-038). A checkbox when it can be offered; a
// sentence saying WHY NOT when it cannot — never a silently disabled control.
//
// The decision is ./encryptOffer.ts (tested per branch). This renders it. When
// the server does not support encryption, or the sender has no key of their own,
// nothing is drawn at all: the send line above already says what will happen to
// the message, and a second line repeating "no key" would be noise.
//
// `role="status"` for the sentence so a change (a recipient added who has no key)
// is announced, for the reason SendStatusLine gives.
//
// "Can't encrypt: X's key expired on …" OFFERS "Check for an updated key"
// (#818): they may have renewed it and published the renewal. The answer is a
// sentence under the line; when they published a different key, the lookup
// that follows shows it as a changed key, and the decision below asks.

import { useState } from "react";
import type { JSX } from "react";

import { recipientKeyNotices } from "../../../core/encryption";
import type { RecipientKeyNotice } from "../../../core/encryption";
import type { RecipientKeyInfo } from "../../../core/jmap/types";
import type { ComposeRefreshResult } from "../../../core/keyLifecycle";
import { keyDateShort } from "../../../core/userKeys";
import { t } from "../../i18n";
import { KeyChangeDecision } from "../KeyChangeDecision";
import { refreshText } from "../settings/keys/text";
import { joinNames } from "./sendLine";
import type { EncryptOffer } from "./encryptOffer";

export function EncryptSwitch({
  offer,
  checked,
  onChange,
  names,
  keys,
  onDecideKey,
  onImportKey,
  recipients = [],
  labels = {},
  warnDays = null,
  now,
  onRefreshKey,
}: {
  offer: EncryptOffer;
  checked: boolean;
  onChange: (on: boolean) => void;
  /** Display names of the To/Cc recipients, for the confirmation sentence. */
  names: readonly string[];
  /** The lookup answers, keyed by lower-cased address: where a changed key's two
   * fingerprints come from. */
  keys: Readonly<Record<string, RecipientKeyInfo | undefined>>;
  /** Record the user's answer for a changed key (ContactKey/decide). */
  onDecideKey: (address: string, fingerprint: string, decision: "trust" | "keep") => Promise<boolean>;
  /** Open the import-a-key dialog for a recipient who has no key (#47). */
  onImportKey?: (address: string) => void;
  /** The To/Cc addresses (lower-cased) the dates are said about (#795). */
  recipients?: readonly string[];
  /** Lower-cased address → the name the composer shows for it. */
  labels?: Readonly<Record<string, string>>;
  /** The server's warning threshold (client.pgpKeyExpiryWarningDays()). */
  warnDays?: number | null;
  now?: Date;
  /** Check for an updated key for a recipient whose key expired (#818). */
  onRefreshKey?: (address: string) => Promise<ComposeRefreshResult>;
}): JSX.Element | null {
  const [refreshing, setRefreshing] = useState<Readonly<Record<string, "busy" | ComposeRefreshResult>>>({});
  if (offer.why === "unsupported" || offer.why === "noRecipients") return null;

  const clock = now ?? new Date();
  const notices = recipientKeyNotices(offer, recipients, keys, warnDays, clock);
  const nameOf = (address: string): string => labels[address] ?? address;
  const noticeText = (n: RecipientKeyNotice): string => {
    switch (n.kind) {
      case "expiresSoon":
        return t("compose.keyExpiresOn", { name: nameOf(n.address), date: keyDateShort(n.at, clock) });
      case "expired":
        return n.at
          ? t("compose.keyExpiredOn", { name: nameOf(n.address), date: keyDateShort(n.at, clock) })
          : t("compose.keyExpired", { name: nameOf(n.address) });
      case "revoked":
        return t("compose.keyRevoked", { name: nameOf(n.address) });
    }
  };

  const refreshKey = async (address: string): Promise<void> => {
    if (!onRefreshKey || refreshing[address] === "busy") return;
    setRefreshing((cur) => ({ ...cur, [address]: "busy" }));
    const result = await onRefreshKey(address);
    setRefreshing((cur) => ({ ...cur, [address]: result }));
  };
  const refreshResultText = (r: ComposeRefreshResult, address: string): string =>
    r.kind === "noKey" ? t("compose.refreshNoKey", { name: nameOf(address) }) : refreshText(r);
  // What a check found, said under the line whatever the line now says (a
  // renewal turns "can't encrypt" into an offered switch).
  const refreshNotes = Object.entries(refreshing).flatMap(([address, r]) =>
    r === "busy"
      ? []
      : [
          <p key={`refreshed-${address}`} className="hint compose-encrypt-note compose-key-refreshed" role="status">
            {nameOf(address)}: {refreshResultText(r, address)}
          </p>,
        ],
  );

  if (offer.offered) {
    return (
      <div className="compose-encrypt">
        <label>
          <input type="checkbox" checked={checked} onChange={(ev) => onChange(ev.target.checked)} />{" "}
          {t("compose.encrypt")}
        </label>
        {checked ? (
          <p className="hint compose-encrypt-note" role="status">
            {t("compose.encryptOn", { names: joinNames(names) })}
          </p>
        ) : null}
        {notices.expiring.length > 0 ? (
          <p className="hint compose-encrypt-note compose-key-expiry" role="status">
            {notices.expiring.map(noticeText).join(" ")}
          </p>
        ) : null}
        {refreshNotes}
      </div>
    );
  }

  let message: string | null = null;
  switch (offer.why) {
    case "bcc":
      message = t("compose.encryptBcc");
      break;
    case "missing":
      // A lapsed key is said by name and date (#795); the generic sentence
      // covers only the addresses that have no key at all.
      message = [
        ...notices.lapsed.map(noticeText),
        ...(notices.otherMissing.length > 0
          ? [t("compose.encryptMissing", { names: joinNames(notices.otherMissing.map(nameOf)) })]
          : []),
      ].join(" ");
      break;
    case "changed":
      message = t("compose.encryptChanged", { names: joinNames(offer.changed) });
      break;
    case "pending":
      message = t("compose.encryptPending");
      break;
    case "noOwnKey":
      // The send line already says "Will not sign: …"; saying it twice is noise.
      return null;
    default:
      return null;
  }
  // A changed key is not a dead end: the same decision the reading pane offers,
  // here, for each recipient it applies to.
  const decisions =
    offer.why === "changed"
      ? offer.changed.flatMap((address) => {
          const info = keys[address];
          if (!info || !info.keyChanged || !info.fingerprint || !info.pinnedFingerprint) return [];
          return [
            <KeyChangeDecision
              key={address}
              address={address}
              fingerprint={info.fingerprint}
              previousFingerprint={info.pinnedFingerprint}
              heading={t("compose.encryptChangedHeading", { address })}
              onDecide={(decision) => onDecideKey(address, info.fingerprint as string, decision)}
            />,
          ];
        })
      : [];
  // A missing key is not a dead end either: the user may have been given one.
  const imports =
    offer.why === "missing" && onImportKey
      ? offer.missing.map((address) => (
          <p key={address} className="key-import-action">
            <button
              type="button"
              aria-label={t("compose.importKeyFor", { address })}
              onClick={() => onImportKey(address)}
            >
              {t("compose.importKey")}
            </button>
          </p>
        ))
      : [];
  // An expired key may have been renewed: offer to look.
  const refreshes =
    offer.why === "missing" && onRefreshKey
      ? notices.lapsed
          .filter((n) => n.kind === "expired")
          .map((n) => (
            <p key={`refresh-${n.address}`} className="key-import-action">
              <button
                type="button"
                className="compose-key-refresh"
                aria-label={t("compose.refreshKeyFor", { name: nameOf(n.address) })}
                disabled={refreshing[n.address] === "busy"}
                onClick={() => void refreshKey(n.address)}
              >
                {refreshing[n.address] === "busy" ? t("keys.refresh.busy") : t("compose.refreshKey")}
              </button>
            </p>
          ))
      : [];
  return (
    <>
      <p className="hint compose-encrypt-note" role="status">
        {message}
      </p>
      {refreshes}
      {refreshNotes}
      {imports}
      {decisions}
    </>
  );
}
