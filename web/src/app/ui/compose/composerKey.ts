// SPDX-License-Identifier: AGPL-3.0-only

// WHICH COMPOSER A COMPOSE URL BELONGS TO (#612).
//
// ComposeRoute keys the composer by its URL, so that replying to a different
// message is a different composer rather than the last draft with a new title.
// Autosave broke the other half of that: every save mints a new draft id, and a
// composer that never moved its URL was left naming a draft that no longer
// exists — so a reload, or back and forward, opened a composer that sat on
// "Opening…" forever over text that was safe on the server all along.
//
// Moving the URL to the new draft is the fix, and a naive move is WORSE than the
// bug: a new URL is a new key, a new key REMOUNTS the composer, and a remount
// throws away focus, the caret, undo history and anything typed since the save —
// every fifteen seconds. So the key is not the URL's own spelling when the URL
// names a draft this composer saved itself: it stays the key the composer was
// opened under. `adopted` is that record, written by the composer as each save
// lands (ComposeRoute.tsx).
//
// A draft this composer did not save — one clicked in the list, one from another
// device, a pasted link — is never in `adopted`, keeps its own key, and so still
// opens a composer of its own.

import type { ComposeRef } from "../../../core/routes";
import { composeParam } from "../../../core/routes";
import type { Id } from "../../../core/jmap/types";

export function composerKey(ref: ComposeRef, adopted: ReadonlyMap<Id, string>): string {
  if (typeof ref === "object" && "draft" in ref) {
    return adopted.get(ref.draft) ?? composeParam(ref);
  }
  return composeParam(ref);
}
