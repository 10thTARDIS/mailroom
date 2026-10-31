// SPDX-License-Identifier: AGPL-3.0-only

// THE PERSISTENT INGEST-ONLY BANNER (ADR-028 §5, #521).
//
// core/writebackBanner.ts decides WHICH accounts to name; this only renders
// what it is handed, the same split HeaderSentence/headerState.ts keeps. It is
// deliberately its own element rather than a clause appended to HeaderSentence
// — that sentence is ONE live region that states offline/stale/error/ok, and
// this fact is neither: a shadowed account can be perfectly in sync, so this
// must render whatever that sentence says, not compete with it for the one
// slot HeaderSentence has room for.
//
// EVERY CLAUSE STATES THE ACTUAL CONTRACT, MEASURED FROM THE CODE (ADR-028
// §5), and getting any one of them wrong is worse than showing nothing:
//
//   * Mailroom is not writing to this server — the plain fact, first.
//   * flags are PROVISIONAL, not safe — the server will overwrite them the
//     next time it speaks about a message (ADR-028's CONDSTORE measurement).
//   * labels/filing/trash are LOCAL AND DURABLE — the one thing that survives.
//   * N changes recorded but not sent — the ledger, which does NOT drain when
//     the mode is lifted (ADR-028 §4: replay is a separate, explicit action).
//
// Nothing here says "will be applied later" or anything a reader could take as
// a promise the queue drains on its own — it does not, and this repo's own
// measurement discipline treats an inaccurate reassurance as worse than none.

import type { JSX } from "react";

import type { ShadowNotice } from "../../../core/writebackBanner";
import { t, tCount } from "../../i18n";

function ShadowNoticeBanner({ notice }: { notice: ShadowNotice }): JSX.Element {
  return (
    <div className="banner banner-warn shadow-banner" role="status">
      <p className="shadow-banner-title">{t("shadow.title", { name: notice.name })}</p>
      <p>{t("shadow.notWriting")}</p>
      <p>{t("shadow.flagsProvisional")}</p>
      <p>{t("shadow.localDurable")}</p>
      <p>{tCount("shadow.suppressed", notice.suppressedIntents)}</p>
    </div>
  );
}

/**
 * One banner per shadowed account in view — not one banner naming several,
 * because `core/writebackBanner.ts` already refused to collapse them into a
 * single worst-of-many, and a component that then joined their names back
 * into one sentence would undo that on the way to the screen.
 */
export function ShadowBanners({ notices }: { notices: ShadowNotice[] }): JSX.Element | null {
  if (notices.length === 0) return null;
  return (
    <>
      {notices.map((notice) => (
        <ShadowNoticeBanner key={notice.id} notice={notice} />
      ))}
    </>
  );
}
