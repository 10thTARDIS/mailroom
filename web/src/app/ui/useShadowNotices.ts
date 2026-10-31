// SPDX-License-Identifier: AGPL-3.0-only

// WHAT THE MAIL VIEW SAYS ABOUT INGEST-ONLY MODE, ASSEMBLED ONCE (ADR-028 §5,
// #521) — the `useHeaderState.ts` shape applied to a different, orthogonal
// fact: the decision is pure and lives in `core/writebackBanner.ts`; this is
// only the wiring, shared by DeskRoute and TrayRoute so the two surfaces
// cannot name a different set of shadowed accounts for the same session.

import { useMemo } from "react";

import type { Id } from "../../core/jmap/types";
import { shadowNotices } from "../../core/writebackBanner";
import type { ShadowAccount, ShadowNotice } from "../../core/writebackBanner";
import { useAccounts } from "./useAccounts";

export function useShadowNotices(scope: Id | null): ShadowNotice[] {
  const { accounts } = useAccounts();

  const withWriteback: ShadowAccount[] = useMemo(
    () =>
      accounts.map((a) => ({
        id: a.id,
        name: a.name,
        // `undefined` (no health card yet) and `null` both mean "not read" —
        // core/writebackBanner.ts claims nothing for either, the same rule
        // useAccounts.ts states for `status` before its own fetch lands.
        writebackEnabled: a.health?.writeback.enabled ?? null,
        suppressedIntents: a.health?.writeback.suppressedIntents ?? 0,
      })),
    [accounts],
  );

  return useMemo(() => shadowNotices(withWriteback, scope), [withWriteback, scope]);
}
