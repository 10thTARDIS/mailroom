// SPDX-License-Identifier: AGPL-3.0-only

// THE ONE SENTENCE, AND WHAT IT SAYS WHEN SOMETHING IS WRONG (#375 increment 6).
//
// DeskHeader and TrayHeader each state a different set of numbers — that is why
// they are two components (TrayHeader's own header comment says so). What they
// share is everything AFTER those numbers: the live region, the warning tone,
// and the clause that explains why the numbers might be stale. That half is
// here, once, because it is the half that must not differ: the desk saying
// "Offline" while the tray one click away says "cannot reach the server" about
// the same failed request is a client arguing with itself.
//
// THE NUMBERS STAY. This appends to the surface's own clauses rather than
// replacing them — the rule DeskHeader has carried since increment 3: rendering
// an empty list with no sentence above it leaves the reader unable to tell
// "there is no mail" from "we could not ask". Offline does not mean the counts
// are worthless, it means they are as of a moment, and the sentence says which.
//
// THE BUTTONS ARE WORDS, NOT KEYS. `Retry` and `Details` are things you do
// rarely and under stress; a printed key legend on them would be teaching a
// shortcut nobody will practise (ui/desk/SelectionToolbar.tsx makes the same
// argument the other way round for the verbs you use forty times a day).

import type { JSX } from "react";

import type { HeaderState } from "../../../core/headerState";
import { t } from "../../i18n";
import { headerStateParts } from "./format";

export function HeaderSentence({
  parts,
  state,
  now,
  onRetry,
  onDetails,
}: {
  /** The surface's own clauses, in order — "Inbox · all accounts · 12 in tray…".
   * Already words: this component does not know what a tray is. */
  parts: string[];
  state: HeaderState;
  /** The instant the whole sentence is about. Passed in, not read here, so the
   * "synced 40 s ago" in `parts` and the "last synced 14 min ago" this adds are
   * two readings of ONE clock rather than two clocks a few milliseconds apart. */
  now: number;
  /** Offline only: ask again now. */
  onRetry: () => void;
  /** Stale only: open the account health the sentence is summarising. */
  onDetails: () => void;
}): JSX.Element {
  const all = [...parts, ...headerStateParts(state, now)];
  const warn = state.kind !== "ok";

  return (
    <div className="desk-header-block">
      <p
        className={warn ? "desk-header is-warn" : "desk-header"}
        role="status"
        aria-live="polite"
      >
        {all.join(" · ")}
        {state.kind === "offline" ? (
          <>
            {" "}
            <button type="button" className="link-button" onClick={onRetry}>
              {t("header.offline.retry")}
            </button>
          </>
        ) : null}
        {state.kind === "stale" ? (
          <>
            {" "}
            <button type="button" className="link-button" onClick={onDetails}>
              {t("header.stale.details")}
            </button>
          </>
        ) : null}
      </p>
      {/* STALE IS THE ONE STATE THAT NEEDS REASSURING, and it is the one most
          likely to be misread as "do not touch anything". Mailroom is answering
          perfectly; it is the PROVIDER it cannot reach, so every action still
          lands here and goes out when the provider comes back. Without this
          line a user who is behind on their mail waits instead of working. */}
      {state.kind === "stale" ? (
        <p className="desk-header-note">{t("header.stale.note")}</p>
      ) : null}
    </div>
  );
}
