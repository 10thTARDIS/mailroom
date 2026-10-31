// SPDX-License-Identifier: AGPL-3.0-only

// "2 actions waiting for your approval" — the banner over every mail view
// (#49, ADR-042 §7). It sits with the shell's other banners (offline, error),
// so the desk, the lists and the tray all carry it without each route
// remembering to, and links to Settings → API keys, where the requests are.
//
// A LIVE REGION THAT EXISTS BEFORE IT HAS ANYTHING TO SAY. `role="status"`
// (polite) is on a wrapper that is always rendered, empty while nothing is
// waiting: a region inserted together with its text is not reliably
// announced (web/ACCESSIBILITY.md's note on the selection count), and this
// one's whole job is to be heard when a request arrives.
//
// It says nothing at all when the server cannot be asked, or has not been
// yet: "0 waiting" would be a claim about a list nobody read.

import type { JSX } from "react";

import { t, tCount } from "../i18n";
import { navigate } from "../routing";
import { usePendingApprovals } from "./pendingApprovals";

export function PendingApprovalsBanner(): JSX.Element {
  const { waiting } = usePendingApprovals();
  const n = waiting?.length ?? 0;
  return (
    <div className="pending-banner-region" role="status">
      {n > 0 ? (
        <div className="banner banner-warn pending-banner">
          <p>{tCount("pendingBanner.text", n)}</p>
          <button
            type="button"
            className="link-button"
            onClick={() => navigate({ page: "settings", section: "api-keys" })}
          >
            {t("pendingBanner.open")}
          </button>
        </div>
      ) : null}
    </div>
  );
}
