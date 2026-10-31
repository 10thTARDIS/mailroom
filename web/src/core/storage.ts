// SPDX-License-Identifier: AGPL-3.0-only

// The one storage primitive the client-core depends on. Keeping it an
// interface (rather than reaching for `localStorage` / IndexedDB directly) is
// what keeps core/ DOM-free and portable: the web app passes
// `window.localStorage`, a React Native shell passes AsyncStorage behind the
// same three methods, and tests pass a Map (spec §12: "a future native app
// imports the client-core unmodified").

export interface KeyValueStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

/** An in-memory KeyValueStorage — the default when no host storage is supplied
 * (and the one tests use). */
export class MemoryStorage implements KeyValueStorage {
  private readonly map = new Map<string, string>();
  getItem(key: string): string | null {
    return this.map.get(key) ?? null;
  }
  setItem(key: string, value: string): void {
    this.map.set(key, value);
  }
  removeItem(key: string): void {
    this.map.delete(key);
  }
}
