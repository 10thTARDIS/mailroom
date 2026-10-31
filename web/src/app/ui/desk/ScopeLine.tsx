// SPDX-License-Identifier: AGPL-3.0-only

// THE PHONE'S HEADER (#375 increment 7).
//
//   Personal ▾
//   synced 40 s ago
//
// TWO LINES INSTEAD OF SIX CLAUSES, and shared by the desk and the tray for the
// same reason HeaderSentence is: whatever else those two pages state, they must
// state the SCOPE and the FRESHNESS identically, or the app is arguing with
// itself about whose mail this is and how old it is.
//
// THE SCOPE IS THE SWITCHER. On a desktop the account switcher lives at the top
// of the rail (ui/AccountSwitcher.tsx) and the header merely names the scope;
// on a phone the rail is a whole pane away behind ☰, so the name that says
// which mail you are looking at is also the control that changes it. That is
// one affordance doing one job — the `▾` is the promise that tapping does
// something, and `aria-haspopup="dialog"` is the same promise to a reader.
//
// THE WARNING REPLACES THE SECOND LINE, UNCHANGED IN WORDING. Offline and stale
// are decided once (core/headerState.ts) and worded once (ui/desk/format.ts
// headerStateParts); this reuses HeaderSentence with NO clauses of its own, so
// the phone gets exactly the sentence the desktop gets — including its Retry /
// Details button and the stale reassurance underneath. Rewording it shorter for
// a small screen is how two surfaces end up describing one failure two ways.

import type { JSX } from "react";

import type { HeaderState } from "../../../core/headerState";
import { HeaderSentence } from "./HeaderSentence";

export function ScopeLine({
  scope,
  note,
  state,
  now,
  onScope,
  onRetry,
  onDetails,
}: {
  /** The account name, or "all accounts" — already resolved by the caller. */
  scope: string;
  /** The SECOND line, in the surface's own words — "synced 40 s ago" on the
   * desk, "12 · oldest waiting 9 d · synced 40 s ago" on the tray, where that
   * total has nowhere else to go. Shown only while nothing is wrong; the
   * warning sentence replaces it whole. */
  note: string;
  state: HeaderState;
  now: number;
  onScope: () => void;
  onRetry: () => void;
  onDetails: () => void;
}): JSX.Element {
  return (
    <div className="desk-scope">
      <button
        type="button"
        className="desk-scope-button"
        aria-haspopup="dialog"
        onClick={onScope}
      >
        <span className="desk-scope-name">{scope}</span>
        {/* Decorative: the button's accessible name is the account, and
            aria-haspopup already says a chooser follows. */}
        <span className="desk-scope-caret" aria-hidden="true">
          ▾
        </span>
      </button>
      {state.kind === "ok" ? (
        // THE PAGE'S ONE LIVE REGION when nothing is wrong, exactly as the full
        // sentence is on a wider screen — and it is the same element class, so
        // "synced 40 s ago" is muted here for the same reason it is muted
        // there.
        <p className="desk-header desk-header-synced" role="status" aria-live="polite">
          {note}
        </p>
      ) : (
        <HeaderSentence
          parts={[]}
          state={state}
          now={now}
          onRetry={onRetry}
          onDetails={onDetails}
        />
      )}
    </div>
  );
}
