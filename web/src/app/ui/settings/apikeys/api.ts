// SPDX-License-Identifier: AGPL-3.0-only

// What Settings → API keys asks of the server (#49), as one small interface,
// so the screens render against a fake in their tests and against the JMAP
// client here (the same shape as settings/keys/api.ts). Nothing here throws.
//
// ⚠ THE TOKEN passes through `create` exactly once, into the dialog that shows
// it. Nothing on this interface stores it, and `onCreated`-style callbacks
// elsewhere receive the key WITHOUT it (CreateKeyDialog.tsx).

import { useMemo } from "react";

import type {
  ApiKey,
  ApiKeyEvent,
  ApiKeySettings,
  ApiKeysCapability,
  Id,
  MethodErrorArgs,
  PendingAction,
  SetError,
  SuppressedRefusal,
} from "../../../../core/jmap/types";
import { useApp } from "../../../state";

export type Decision =
  | { ok: true; state: string; error: MethodErrorArgs | null }
  | { ok: false; error: SetError };

export interface ApiKeysApi {
  /** null: this server has no API keys (the capability is absent). */
  capability(): ApiKeysCapability | null;
  list(): Promise<ApiKey[] | null>;
  create(settings: ApiKeySettings): Promise<{ ok: true; key: ApiKey; token: string } | { ok: false; error: SetError }>;
  update(id: Id, patch: ApiKeySettings): Promise<{ ok: true } | { ok: false; error: SetError }>;
  revoke(id: Id): Promise<{ ok: true } | { ok: false; error: SetError }>;
  events(keyId: Id): Promise<{ list: ApiKeyEvent[]; suppressedRefusals: SuppressedRefusal[] } | null>;
  pending(): Promise<PendingAction[] | null>;
  decide(id: Id, state: "approved" | "denied"): Promise<Decision>;
  /** Subjects of messages a held delete would move, by id, read IN THE
   * REQUEST'S OWN ACCOUNT (null: could not ask). */
  subjects(ids: readonly Id[], accountId: Id): Promise<Map<Id, string> | null>;
}

/** The most message subjects one held delete's card asks for. */
export const MAX_DELETE_SUBJECTS = 50;

export function useApiKeysApi(): ApiKeysApi {
  const { client } = useApp();
  return useMemo<ApiKeysApi>(
    () => ({
      capability: () => client.apiKeysCapability(),
      list: () => client.apiKeysGet(),
      create: (settings) => client.apiKeyCreate(settings),
      update: (id, patch) => client.apiKeyUpdate(id, patch),
      revoke: (id) => client.apiKeyRevoke(id),
      events: (keyId) => client.apiKeyEvents(keyId),
      pending: () => client.pendingActionsGet(),
      decide: (id, state) => client.pendingActionDecide(id, state),
      subjects: async (ids, accountId) => {
        if (ids.length === 0) return new Map();
        try {
          const r = await client.emailGet({
            accountId,
            ids: ids.slice(0, MAX_DELETE_SUBJECTS) as Id[],
            properties: ["id", "subject"],
          });
          const out = new Map<Id, string>();
          for (const e of r.list) out.set(e.id, e.subject ?? "");
          return out;
        } catch {
          return null;
        }
      },
    }),
    [client],
  );
}
