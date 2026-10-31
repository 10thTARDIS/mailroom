// SPDX-License-Identifier: AGPL-3.0-only

// WHERE THE WIZARD'S MEMORY LIVES (#446) — the browser half, so that
// setupState.ts stays a pure module with a test and this file holds the only
// `window` in the feature.
//
// TWO MEMORIES, TWO LIFETIMES, AND THE DIFFERENCE IS THE WHOLE POINT:
//
//   localStorage    the wizard's own state, per login. It makes each step
//                   RESUMABLE — a reload, a closed tab, a phone that went to
//                   sleep mid-import all come back to the plan the user had
//                   built rather than to step 1 with everything re-chosen.
//   sessionStorage  "this tab has already offered the wizard once". Without it,
//                   somebody who skipped by navigating away is dragged back on
//                   the next render, forever, because the gate that opened it
//                   (openSetup.ts) is still true until the write lands.
//
// FAIL SOFT, ALWAYS. A private window, storage disabled, or a full quota must
// lose the memory and not the app — the same promise app/routing.ts and
// searchHistory.ts make, for the same reason. A wizard that threw because it
// could not remember a draft plan would be worse than one that forgot it.

import { MemoryStorage } from "../../../core/storage";
import type { KeyValueStorage } from "../../../core/storage";

/** The one fallback, shared: with no host storage the memory still works for the
 * life of the page, which is exactly long enough for one pass through a wizard. */
const fallback = new MemoryStorage();

function host(kind: "local" | "session"): Storage | null {
  try {
    if (typeof window === "undefined") return null;
    return kind === "local" ? window.localStorage : window.sessionStorage;
  } catch {
    return null;
  }
}

/** A KeyValueStorage over one of the browser's two stores, which never throws.
 * Exported as the interface from core/storage.ts, so every consumer below can be
 * handed a Map in a test instead. */
export function browserStorage(kind: "local" | "session"): KeyValueStorage {
  const store = host(kind);
  if (!store) return fallback;
  return {
    getItem(key: string): string | null {
      try {
        return store.getItem(key);
      } catch {
        return null;
      }
    },
    setItem(key: string, value: string): void {
      try {
        store.setItem(key, value);
      } catch {
        /* quota or a disabled store: the memory is a convenience, never a gate */
      }
    },
    removeItem(key: string): void {
      try {
        store.removeItem(key);
      } catch {
        /* see setItem */
      }
    },
  };
}

/** Per LOGIN, because the state describes that login's folders: two accounts in
 * two tabs of one browser must not resume each other's plan. */
export function setupStateKey(loginId: string): string {
  return `mailroom.setup.v1.${loginId}`;
}

const OFFERED_KEY = "mailroom.setup.offered";

/** Whether this tab has already navigated to the wizard on its own. */
export function alreadyOffered(storage: KeyValueStorage = browserStorage("session")): boolean {
  return storage.getItem(OFFERED_KEY) === "1";
}

/** Records that it has. Written BEFORE the navigation, so a re-render between the
 * two cannot offer it twice. */
export function markOffered(storage: KeyValueStorage = browserStorage("session")): void {
  storage.setItem(OFFERED_KEY, "1");
}
