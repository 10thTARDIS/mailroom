// SPDX-License-Identifier: AGPL-3.0-only

// THE APP: a route, and the shell it is rendered in (#373).
//
// This file was 1,179 lines — the header, the rail, the list, the reading pane,
// every modal and every sheet as a local boolean, plus the keyboard chain that
// had to know about all of them. It is now the routing table and nothing else:
//
//   ui/Shell.tsx            the chrome that is always there, and the one key map
//   ui/routes/DeskRoute     the desk — what #/inbox is (#375)
//   ui/routes/ListRoute     the message list and the reading pane
//   ui/routes/SettingsRoute #/settings and its sections
//   ui/routes/OutboxRoute   #/outbox — what has not gone yet (#448)
//   ui/routes/ComposeRoute  ?compose=new | reply | forward | draft (#448)
//   ui/setup/SetupRoute     #/setup — day-one, the four steps (#446)
//
// The two effects below are the corrections a route can need before it renders:
// the one legacy hash that has to be rewritten, and the per-device scope this
// browser was last using.

import { useEffect } from "react";
import type { JSX } from "react";

import { legacyRedirect } from "../../core/routes";
import { applyRemembered, loadScope, navigate, saveScope, useListRoute, useRoute } from "../routing";
import { Shell } from "./Shell";
import { DeskRoute } from "./routes/DeskRoute";
import { ListRoute } from "./routes/ListRoute";
import { OutboxRoute } from "./routes/OutboxRoute";
import { TrayRoute } from "./routes/TrayRoute";
import { SettingsRoute } from "./routes/SettingsRoute";
import { ComposeRoute } from "./routes/ComposeRoute";
import { ContactsRoute } from "./routes/ContactsRoute";
import { SetupRoute } from "./setup/SetupRoute";

export function App(): JSX.Element {
  const route = useRoute();
  const listRoute = useListRoute();

  // `#/m/<id>` named a message with no list around it. It becomes the list this
  // tab was last on, with that message open — replacing the entry rather than
  // pushing, so the back button does not walk back into a hash that would only
  // redirect again.
  useEffect(() => {
    const target = legacyRedirect(window.location.hash, listRoute);
    if (target) navigate(target, { replace: true });
  }, [route, listRoute]);

  // The scope and inbox tab this device was last using, applied only when the
  // URL does not say (routing.ts). A route that names its own scope always wins:
  // a pasted link has to mean what it says.
  useEffect(() => {
    const target = applyRemembered(route, loadScope());
    if (target) {
      navigate(target, { replace: true });
      return;
    }
    saveScope(route);
  }, [route]);

  // THE INBOX IS THE DESK NOW (#375). `#/inbox`, `#/inbox/review` and
  // `#/inbox/rest` are the desk's three URLs and `#/tray` is the desk's other
  // surface — the whole in-tray, grouped by how long each thing has waited.
  // `#/inbox/sort` — the needs-a-category queue — is a different surface with a
  // different question, and every other route (search, a label, a folder, all
  // mail) is still the message list. One condition, in one place, so there is
  // never a route that renders two of them or none.
  const desk = route.page === "inbox" && !route.sort;

  // THE OUTBOX IS NOT A MESSAGE LIST (#448). It is a view with a URL and no
  // `Email/query` behind it — its rows are EmailSubmission objects — so it has to
  // be branched here rather than left to ListRoute, which would render an empty
  // message list for it (core/views.ts gives the outbox view no filters).
  const outbox = route.page === "view" && route.view.kind === "outbox";

  // DAY-ONE SETUP IS THE MAIN AREA AND NOT AN OVERLAY (#446). It is branched
  // here, beside the desk, for the reason the outbox is: it has a URL and no
  // Email/query behind it, so leaving it to ListRoute would draw an empty message
  // list under the wizard. It takes the whole content area — no list, no reading
  // pane — while the RAIL stays, because the wizard is a place in the app rather
  // than a modal in front of it: "Skip setup, open the desk" has to be one click
  // away and the rail is where every other exit lives.
  const setup = route.page === "setup";

  return (
    <Shell
      overlays={<SettingsRoute />}
      // Not an overlay: the composer takes the reading-pane column, which means it
      // is a child of the workspace grid (ui/Shell.tsx says why the two are
      // different props).
      composer={<ComposeRoute />}
    >
      {setup ? (
        <SetupRoute />
      ) : route.page === "contacts" ? (
        // CONTACTS (#816): a main-area route with no Email/query behind it,
        // branched here for the reason the outbox is.
        <ContactsRoute />
      ) : desk ? (
        <DeskRoute />
      ) : route.page === "tray" ? (
        <TrayRoute />
      ) : outbox ? (
        <OutboxRoute />
      ) : (
        <ListRoute />
      )}
    </Shell>
  );
}
