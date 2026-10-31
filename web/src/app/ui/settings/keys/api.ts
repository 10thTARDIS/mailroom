// SPDX-License-Identifier: AGPL-3.0-only

// What the Keys page asks of the server (#796), as one small interface, so the
// page renders against a fake in its tests and against the JMAP client here.
// Every write resolves to an outcome in core/userKeys.ts's closed set, never a
// raw error, and never throws.

import { useMemo } from "react";

import type { ImportKeyOutcome } from "../../../../core/keyImport";
import { replaceLeftNewKey } from "../../../../core/keyLifecycle";
import { keyErrorOf, keyOutcome, keySetErrorOf } from "../../../../core/userKeys";
import type { KeyActionError, KeyOutcome } from "../../../../core/userKeys";
import type {
  Contact,
  ContactKeyEvent,
  ContactKeyRefreshResult,
  HeldContactKey,
  Id,
  KeyExpiryArg,
  UserKeyReplaceResult,
  UserKeyUpdated,
} from "../../../../core/jmap/types";
import { useApp } from "../../../state";

export type RevokeReason = "compromised" | "superseded" | "retired";

/** A replace that failed says, besides why, whether it left a new key behind
 * (made, but not put in place: the old key is unchanged, #817). */
export type ReplaceOutcome =
  | { ok: true; value: UserKeyReplaceResult }
  | { ok: false; error: KeyActionError; leftNewKey: Id | null };

export interface KeysApi {
  generate(identityId: Id, expiry: KeyExpiryArg, bind: boolean): Promise<KeyOutcome<{ fingerprint: string }>>;
  importOwn(privateKey: string, passphrase: string): Promise<KeyOutcome<{ alreadyStored: boolean }>>;
  update(
    id: Id,
    patch: { identityId?: Id | null; wkdPublished?: boolean; decryptOnly?: true },
  ): Promise<KeyOutcome<UserKeyUpdated>>;
  extend(id: Id, to: number | { expiresAt: string }): Promise<KeyOutcome<{ expiresAt: string | null }>>;
  replace(id: Id, expiry: KeyExpiryArg): Promise<ReplaceOutcome>;
  revoke(id: Id, reason: RevokeReason, confirm: string): Promise<KeyOutcome>;
  exportKey(id: Id, passphrase: string): Promise<KeyOutcome<{ armoredKey: string; fileName: string }>>;
  /** Held AND removed keys (#815: removed ones carry removedAt). */
  readCorrespondents(): Promise<{ keys: HeldContactKey[]; contacts: Contact[] } | null>;
  removeCorrespondentKey(id: Id): Promise<KeyOutcome>;
  correspondentKeyHistory(id: Id): Promise<KeyOutcome<ContactKeyEvent[]>>;
  refreshCorrespondentKey(id: Id): Promise<KeyOutcome<ContactKeyRefreshResult>>;
  /** ContactKey/decide: "keep" refuses the key, "trust" makes it the pin. */
  decideCorrespondentKey(address: string, fingerprint: string, decision: "trust" | "keep"): Promise<KeyOutcome>;
  checkContactKey(address: string, publicKey: string): Promise<ImportKeyOutcome>;
  importContactKey(address: string, publicKey: string): Promise<ImportKeyOutcome>;
  /** After a change that moves Identity.signingKeyId: sync identities now. */
  afterChange(): void;
}

export function useKeysApi(): KeysApi {
  const { client, actions } = useApp();
  return useMemo<KeysApi>(
    () => ({
      generate: (identityId, expiry, bind) =>
        keyOutcome(async () => {
          const r = await client.userKeyGenerate(identityId, { ...expiry, bind });
          return { fingerprint: r.fingerprint };
        }),
      importOwn: (privateKey, passphrase) =>
        keyOutcome(async () => {
          const r = await client.userKeyImport(privateKey, passphrase);
          return { alreadyStored: r.alreadyStored === true };
        }),
      update: async (id, patch) => {
        try {
          const r = await client.userKeySet(id, patch);
          return r.ok ? { ok: true, value: r.updated } : { ok: false, error: keySetErrorOf(r.refused) };
        } catch (err) {
          return { ok: false, error: keyErrorOf(err) };
        }
      },
      extend: (id, to) =>
        keyOutcome(async () => {
          const r = await client.userKeyExtend(id, to);
          return { expiresAt: r.expiresAt ?? null };
        }),
      replace: async (id, expiry) => {
        try {
          return { ok: true, value: await client.userKeyReplace(id, expiry) };
        } catch (err) {
          return { ok: false, error: keyErrorOf(err), leftNewKey: replaceLeftNewKey(err) };
        }
      },
      revoke: (id, reason, confirm) =>
        keyOutcome(async () => {
          await client.userKeyRevoke(id, reason, confirm);
          return undefined;
        }),
      exportKey: (id, passphrase) =>
        keyOutcome(async () => {
          const r = await client.userKeyExport(id, passphrase);
          return { armoredKey: r.armoredKey, fileName: r.fileName };
        }),
      readCorrespondents: () => client.contactKeysRead({ includeRemoved: true }),
      removeCorrespondentKey: async (id) => {
        try {
          const refused = await client.contactKeyDestroy(id);
          return refused ? { ok: false, error: keySetErrorOf(refused) } : { ok: true, value: undefined };
        } catch (err) {
          return { ok: false, error: keyErrorOf(err) };
        }
      },
      correspondentKeyHistory: (id) => keyOutcome(() => client.contactKeyHistory(id)),
      refreshCorrespondentKey: (id) => keyOutcome(() => client.contactKeyRefresh(id)),
      decideCorrespondentKey: (address, fingerprint, decision) =>
        keyOutcome(async () => {
          await client.contactKeyDecide(address, fingerprint, decision);
          return undefined;
        }),
      checkContactKey: (address, key) => actions.importContactKey(address, key, { dryRun: true }),
      importContactKey: (address, key) => actions.importContactKey(address, key),
      afterChange: () => actions.refreshAfterKeyChange(),
    }),
    [client, actions],
  );
}
