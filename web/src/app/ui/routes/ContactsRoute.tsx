// SPDX-License-Identifier: AGPL-3.0-only

// #/contacts AND #/contacts/<id> (#816): the list, and one contact beside it.
//
// The shape is the mail list's and the reading pane's, on purpose: on a wide
// screen the page opens BESIDE the list; below 1100 px it REPLACES the list (as
// opening a message does), with a back control; on a phone one pane at a time.
// The list stays mounted under the page in every case, so Back returns to its
// rows, its "Show more" pages and its scroll (ui/contacts/ContactsList.tsx).
//
// It is a main-area route like the Outbox and day-one setup, not an overlay
// like Settings, and not a MESSAGE list: the mail list underneath is left as it
// was (core/routes.ts isListRoute), so leaving Contacts goes back to it.

import { useCallback, useState } from "react";
import type { JSX } from "react";

import { useRoute } from "../../routing";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import { useContactsApi } from "../contacts/api";
import { ContactPage } from "../contacts/ContactPage";
import { ContactsList } from "../contacts/ContactsList";

export function ContactsRoute(): JSX.Element | null {
  const route = useRoute();
  const api = useContactsApi();
  const { pushToast } = useApp();
  const [version, setVersion] = useState(0);
  const changed = useCallback(() => setVersion((n) => n + 1), []);
  const say = useCallback((message: string) => void pushToast({ kind: "info", message }), [pushToast]);
  // Nothing is asked before the session is: a reload straight onto
  // #/contacts renders before the client has connected, and a query sent then
  // fails rather than waits (found by the a11y lane's cold load).
  const accountId = useMailState().accountId;
  if (route.page !== "contacts") return null;
  if (!accountId) {
    return (
      <main className="content contacts-content" id="messages" tabIndex={-1}>
        <p className="field-hint contacts-state" role="status">
          {t("contacts.loading")}
        </p>
      </main>
    );
  }
  return (
    <main className="content contacts-content" id="messages" tabIndex={-1} data-open={route.id ? "page" : "list"}>
      <ContactsList api={api} route={route} version={version} onChanged={changed} onMessage={say} />
      {route.id ? (
        <ContactPage key={route.id} api={api} id={route.id} route={route} onChanged={changed} onMessage={say} />
      ) : null}
    </main>
  );
}
