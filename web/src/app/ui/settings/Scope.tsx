// SPDX-License-Identifier: AGPL-3.0-only

// SCOPE ON EVERY ROW (#374 increment 2).
//
// The question a settings page almost never answers is "who does this apply
// to?", and in Mailroom that question has four different answers on one screen:
//
//   login    your whole login, every device, every account — the line, the tray
//            nag, your rules, your keys
//   account  one mail account: the addresses it sends as, the credentials it
//            syncs with, whether it is paused
//   area     one area of the desk: its drawer, what feeds it
//   web      this app family, synced between browsers but not to a phone
//
// Without the word printed, the two failures are equally bad and both happen:
// changing a per-account setting believing it was global, and hunting for a
// global setting inside one account. Several rows carry TWO words (`login ·
// area`) because they genuinely hold both kinds of control — Areas and drawers
// has a login-wide default sweep period and a per-area override — and the honest
// rendering of that is two words, not the broader of the two.
//
// It is a WORD, not a colour or a badge: colour is a second channel here at best
// (tokens.css's "never colour alone"), and there is no icon that distinguishes a
// login from an account without being learned first.

import type { JSX } from "react";

import { t } from "../../i18n";
import type { MessageKey } from "../../i18n";

/** The four things a setting can apply to. Closed on purpose: a fifth scope
 * would be a fifth thing a user has to learn, so adding one is a design
 * decision rather than a string. */
export type SettingsScope = "login" | "account" | "area" | "web";

/** The rows in the page's own order, so `SCOPES` can be read as documentation of
 * the vocabulary rather than being scattered across twelve row files. */
export const SCOPE_ORDER: readonly SettingsScope[] = ["login", "account", "area", "web"];

function label(scope: SettingsScope): string {
  return t(`settings.scope.${scope}` as MessageKey);
}

function explain(scope: SettingsScope): string {
  return t(`settings.scope.${scope}.explain` as MessageKey);
}

/**
 * The scope words for one row.
 *
 * The `title` carries the sentence-long meaning of each word so the short label
 * is not the only explanation anywhere — a four-word vocabulary a reader has to
 * infer is a vocabulary half of them will infer wrongly. It is on the individual
 * word rather than the group, because in `login · area` the two words mean
 * different things and one tooltip covering both would have to say so in a
 * sentence nobody reads.
 */
export function Scope({ scopes }: { scopes: readonly SettingsScope[] }): JSX.Element {
  return (
    <p className="setting-scope">
      {scopes.map((scope, i) => (
        <span key={scope}>
          {i > 0 ? <span className="setting-sep"> · </span> : null}
          <span className="setting-scope-word" title={explain(scope)}>
            {label(scope)}
          </span>
        </span>
      ))}
    </p>
  );
}
