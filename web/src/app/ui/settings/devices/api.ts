// SPDX-License-Identifier: AGPL-3.0-only

// What Settings → Signed-in devices asks of the server (#937), as one small
// interface, so the row renders against a fake in its tests and against the
// JMAP client here (the same shape as apikeys/api.ts). Nothing here throws.

import { useMemo } from "react";

import type { SignedInDevice } from "../../../../core/jmap/types";
import { logout } from "../../../auth";
import { useApp } from "../../../state";

export interface DevicesApi {
  /** null: this server has no signed-in devices page (the capability is absent). */
  capability(): Record<string, unknown> | null;
  list(): Promise<SignedInDevice[] | null>;
  /** Signs out other sessions through SignedInDevice/set destroy. */
  signOut(ids: string[] | "others"): Promise<{ destroyed: string[]; notDestroyed: string[] } | null>;
  /** Signs THIS device out through the ordinary logout route (provider logout
   * and cookie clearing happen there), never through the JMAP destroy. */
  signOutHere(): void;
}

export function useDevicesApi(): DevicesApi {
  const { client } = useApp();
  return useMemo<DevicesApi>(
    () => ({
      capability: () => client.devicesCapability(),
      list: () => client.signedInDevices(),
      signOut: (ids) => client.signOutDevices(ids),
      signOutHere: () => logout(),
    }),
    [client],
  );
}
