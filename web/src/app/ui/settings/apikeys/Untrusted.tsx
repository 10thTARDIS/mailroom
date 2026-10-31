// SPDX-License-Identifier: AGPL-3.0-only

// TEXT AN AGENT OR A STRANGER WROTE, SHOWN IN OUR OWN SENTENCES (#49).
//
// An approval card shows a subject the KEY wrote, attachment names, and the
// subjects of mail strangers sent. React escapes all of it, so none of it can
// be markup. What it can still do is reorder: a right-to-left override in a
// subject would rewrite the words around it on screen ("to 3 recipients"
// read backwards into the subject, core/hostileText.ts explains the
// mechanism). Both of that file's defences are applied:
//
//   * <bdi dir="auto"> plus `unicode-bidi: isolate` (settings.css): the text
//     is its own bidi paragraph, so nothing in it can move its neighbours;
//   * the scoping bidi controls are removed, so it cannot scramble itself
//     either, and the card says that it removed some.
//
// LRM/RLM stay: they are legitimate in Arabic and Hebrew text and scope
// nothing (hostileText.ts BIDI_RANGES says why).

import type { JSX } from "react";

import { hasBidiControls, stripBidiControls } from "../../../../core/hostileText";
import { t } from "../../../i18n";

export function Untrusted({ text, className }: { text: string; className?: string }): JSX.Element {
  const stripped = hasBidiControls(text);
  return (
    <>
      <bdi dir="auto" className={`untrusted-text${className ? ` ${className}` : ""}`}>
        {stripBidiControls(text)}
      </bdi>
      {stripped ? <span className="field-hint untrusted-stripped"> {t("apiKeys.untrusted.stripped")}</span> : null}
    </>
  );
}
