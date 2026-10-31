// SPDX-License-Identifier: AGPL-3.0-only

// NAVIGATING FROM INSIDE SETTINGS WITHOUT LOSING THE SCOPE (#374 increment 3).
//
// ⚠ THE PROBLEM THIS SOLVES IS SILENT. The settings page is an OVERLAY over a list,
// and `?acct=<id>` on the settings route is that list's ACCOUNT SCOPE — not a
// setting, not a filter on the page, just the scope the user will be back in when
// they close it (core/routes.ts withAcct, listRouteOf). Every navigation inside the
// page that built a route from scratch would drop it, so opening an account and
// pressing Escape would land the user on "all accounts" when they had been scoped
// to one. Nothing would look broken; the list would simply be wider than they left
// it, and `rememberedFrom` would then write that widening into the per-device
// memory (app/routing.ts).
//
// So every in-page navigation goes through this, and a target that names its OWN
// `acct` is left alone — an explicit scope in the destination always wins over the
// one being carried.

import { useCallback } from "react";

import { navigate, useRoute } from "../../routing";
import type { Route } from "../../../core/routes";
import { withAcct } from "../../../core/routes";

/** `navigate`, with the current route's account scope carried onto the target. */
export function useKeepScope(): (to: Route) => void {
  const route = useRoute();
  const acct = route.acct ?? null;
  return useCallback(
    (to: Route) => {
      navigate(to.acct ? to : withAcct(to, acct));
    },
    [acct],
  );
}
