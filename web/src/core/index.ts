// SPDX-License-Identifier: AGPL-3.0-only

// Barrel export for the Mailroom TS client-core (spec docs/mailroom-spec.md
// §12: "shared TS client-core (JMAP client, offline cache, sync/action-queue,
// local search)"). Framework-agnostic and DOM-free by construction — a future
// native shell (React Native / Capacitor / Tauri) imports THIS, unmodified, and
// supplies its own storage + UI.
//
//   jmap/types  — the wire shapes internal/api actually emits
//   jmap/client — the JMAP transport (batched calls + result references)
//   session     — the Session object and the per-type /changes cursors
//   storage     — the one host primitive core needs (KeyValueStorage)
//   store       — the normalized cache + the optimistic overlay
//   queue       — the durable offline action queue (intent deltas)
//   sync        — session / initial load / view queries / the changes loop
//   actions     — every mutation, with instant local apply + reconcile/rollback
//   views       — the view model and its translation to JMAP filters

export * from "./jmap/types";
export * from "./jmap/client";
export * from "./session";
export * from "./storage";
export * from "./store";
export * from "./queue";
export * from "./sync";
export * from "./actions";
export * from "./views";
