// SPDX-License-Identifier: AGPL-3.0-only

// THE SEND LINE (#448): one sentence under the body, before the footer.
//
// `role="status"` so a change is ANNOUNCED (#447). That is the whole reason this
// is a live region rather than an ordinary paragraph: the sentence changes as
// recipients are added, and the change is the information — a screen-reader user
// who added a recipient with no key must hear that the message will go as plain
// text without hunting for the line.
//
// The decision is ./sendLine.ts, tested per branch. This file joins the clauses
// with " · " and picks the tone class; nothing here decides what is true.
//
// ⚠ THE FILE IS NOT CALLED SendLine.tsx, and it cannot be. `sendLine.ts` — the
// pure module beside it — collides with that name on a case-insensitive
// filesystem: TypeScript resolves `./SendLine` to `sendLine.ts` (it tries `.ts`
// before `.tsx`) and then reports the two paths as differing only in casing. So
// the component is SendStatusLine and the decision is sendLine, which also reads
// better: one is a sentence on screen, the other is the sentence's content.

import type { JSX } from "react";

import { t } from "../../i18n";
import { sendLine } from "./sendLine";
import type { SendLineChip, WillNotSignReason } from "./sendLine";

export function SendStatusLine({
  ownKey,
  ownAddress,
  chips,
  reason,
}: {
  /** true = a usable signing key; false = none; null = this server cannot be
   * asked (no `urn:mailroom:pgp`). Computed by the caller with `willSign`. */
  ownKey: boolean | null;
  ownAddress: string;
  chips: readonly SendLineChip[];
  /** Why `ownKey` is `false` — `willNotSignReason`'s answer. Ignored (and
   * optional) when `ownKey` is not `false`. */
  reason?: WillNotSignReason;
}): JSX.Element {
  const line = sendLine(ownKey, chips, ownAddress, reason);
  return (
    <p className={`compose-sendline compose-sendline-${line.tone}`} role="status">
      {line.tone === "seal" ? <span aria-hidden="true">⚿ </span> : null}
      {line.parts.map((part, i) => (
        <span key={part.key}>
          {i > 0 ? (
            <span className="compose-sendline-sep" aria-hidden="true">
              {" · "}
            </span>
          ) : null}
          {t(part.key, part.params)}
        </span>
      ))}
    </p>
  );
}
