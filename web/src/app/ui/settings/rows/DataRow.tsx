// SPDX-License-Identifier: AGPL-3.0-only

// DATA (#374 increment 2) — the row that says what this app cannot do, and the
// door out.
//
// EXPORT AND DELETION ARE THE OPERATOR'S, and the row says so instead of offering
// buttons that would fail. They are `mailroom-admin` operations: they run against
// the whole login's mail, they can take a long time, and they are not something a
// JMAP session is scoped to do. A client that offered "Export my mail" and then
// reported an error would be worse than one that names who to ask — and a page
// with no mention of export at all leaves the user to conclude their mail cannot
// be got out, which is false.
//
// SIGN OUT LIVES HERE rather than floating at the bottom of the page as it did in
// #373's index. It is the same kind of thing as the two sentences above it — an
// operation on your whole login rather than a preference — and putting it in a row
// means it has a URL, a heading, and no chance of being clicked by someone aiming
// at the control above it.

import type { JSX } from "react";

import { logout } from "../../../auth";
import { t } from "../../../i18n";

export function DataBody(): JSX.Element {
  return (
    <>
      <p className="field-hint">{t("settings.data.operator")}</p>
      <button type="button" className="button button-quiet menu-action" onClick={logout}>
        {t("settings.signOut")}
      </button>
    </>
  );
}
