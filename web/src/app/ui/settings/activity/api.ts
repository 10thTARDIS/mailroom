// SPDX-License-Identifier: AGPL-3.0-only

// What Settings → Activity asks of the server (ADR-053), as one small interface,
// so the row renders against a fake in its tests and against the JMAP client
// here (the same shape as devices/api.ts). Nothing here throws.

import { useMemo } from "react";

import type { ActivityPage, ActivitySource } from "../../../../core/jmap/types";
import { useApp } from "../../../state";

export interface ActivityQuery {
  sources?: ActivitySource[];
  since?: string;
  until?: string;
  before?: string;
  limit?: number;
}

export interface ActivityApi {
  /** null: this server has no Activity list (the capability is absent). */
  capability(): Record<string, unknown> | null;
  /** One page, or null when it could not be asked. */
  list(q: ActivityQuery): Promise<ActivityPage | null>;
}

export function useActivityApi(): ActivityApi {
  const { client } = useApp();
  return useMemo<ActivityApi>(
    () => ({
      capability: () => client.activityCapability(),
      list: (q) => client.activityList(q),
    }),
    [client],
  );
}
