// SPDX-License-Identifier: AGPL-3.0-only

// IDENTITIES AND SIGNATURES (#374 increment 2).
//
// THE IDENTITIES ARE A PRECEDENCE, NOT AN INVENTORY. Under spec §5's alias model
// an account can hold several concrete addresses AND a `*@domain` catch-all, and
// the question a user actually has is not "which addresses do I have?" — they know
// — it is "which one is a reply going to come from?". That is an order, so this
// renders an order, with the reason word beside each entry. The ordering itself is
// pure and tested next door (settings/precedence.ts); this file only draws it and
// supplies the account NAMES, which come from useAccounts and cannot live in a
// pure module.
//
// SIGNATURES AND TEMPLATES ARE IN THIS ROW AND NOT THEIR OWN, because a signature
// is BOUND to an identity (that is the point of it under the alias model — the
// sign-off for mail from shop@ is not the one for work@), and a row that listed
// the identities without the sign-offs attached to them would be answering half of
// one question. `#/settings/snippets` still lands here (core/routes.ts).
//
// NOTHING IS SHOWN TWICE. The precedence names the addresses; the snippets list
// names the sign-offs and the identity each is bound to. The overlap is the
// address, and it is deliberate: it is the join between the two halves.

import type { JSX } from "react";

import { t, tCount } from "../../../i18n";
import { useMailState } from "../../../state";
import type { MessageKey } from "../../../i18n";
import { useAccounts } from "../../useAccounts";
import { SnippetsBody } from "../../SnippetsSheet";
import { identityPrecedence } from "../precedence";
import type { PrecedenceReason } from "../precedence";

function reasonLabel(reason: PrecedenceReason): string {
  return t(`settings.identities.reason.${reason}` as MessageKey);
}

/** The state line: `3 identities across 2 accounts · 2 signatures · 1 template`.
 *
 * The identity count is taken from the PRECEDENCE, not from the store's map, so
 * the number and the list below it cannot disagree — an identity with no account
 * is left out of both (precedence.ts says why).
 *
 * The two snippet kinds are counted separately because they are different things
 * that happen to share a table: a signature is appended to what you write, a
 * template replaces an empty message. "3 snippets" would be a number about the
 * schema rather than about the user's setup. */
export function identitiesStateText(
  identityCount: number,
  accountCount: number,
  signatures: number,
  templates: number,
): string[] {
  // Composed from two independently-pluralised pieces rather than one string with
  // two counts in it: "1 identities across 1 accounts" is the shape a single key
  // produces, and it is wrong in two places at once.
  const facts: string[] = [
    t("settings.state.identitiesAcross", {
      identities: tCount("settings.state.identityCount", identityCount),
      accounts: tCount("settings.state.accountCount", accountCount),
    }),
  ];
  // Omitted at zero: "0 signatures" is not a fact about a setup, it is the absence
  // of one, and the body's own empty state says it where it belongs.
  if (signatures > 0) facts.push(tCount("settings.state.signatures", signatures));
  if (templates > 0) facts.push(tCount("settings.state.templates", templates));
  return facts;
}

export function IdentitiesBody(): JSX.Element {
  const state = useMailState();
  const { byId } = useAccounts();
  const groups = identityPrecedence(state.identities);

  return (
    <>
      <h4 className="sheet-section">{t("settings.identities.precedence")}</h4>
      <p className="field-hint">{t("settings.identities.explain")}</p>
      {groups.length === 0 ? (
        <p className="field-hint">{t("settings.identities.none")}</p>
      ) : (
        groups.map((group) => (
          <div className="setting-group" key={group.accountId}>
            {/* The account's own name when its record has loaded, else the id —
                never a guess. useAccounts falls back to a representative address
                and then to the id, and an account with no identity at all has no
                entry there, which is exactly the case this fallback covers. */}
            <p className="setting-group-name">
              {byId.get(group.accountId)?.name ?? group.accountId}
            </p>
            <ol className="setting-precedence">
              {group.entries.map((entry) => (
                <li key={entry.id} className="setting-precedence-item">
                  <span className="setting-precedence-address">{entry.email}</span>
                  {entry.name !== "" ? (
                    <span className="remote-allow-kind">{entry.name}</span>
                  ) : null}
                  <span
                    className={
                      entry.reason === "disabled"
                        ? "remote-allow-kind is-warn"
                        : "remote-allow-kind"
                    }
                  >
                    {reasonLabel(entry.reason)}
                  </span>
                </li>
              ))}
            </ol>
          </div>
        ))
      )}

      <h4 className="sheet-section">{t("snippets.title")}</h4>
      <SnippetsBody />
    </>
  );
}
