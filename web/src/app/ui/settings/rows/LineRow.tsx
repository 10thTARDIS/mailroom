// SPDX-License-Identifier: AGPL-3.0-only

// THE LINE (#374 increment 3) — the same setting the desk carries, with the two
// things the desk has no room to say.
//
// The desk's own control (ui/desk/TheLine.tsx) is drawn where it acts, between the
// surface and the drawers, and it answers "what would this line show?". It cannot
// answer either of the questions a user actually has when they open Settings:
//
//   last 7 days: 28 to review · 289 to drawers      what this line HAS done
//   one step stricter would move 12 to drawers      what one click would cost
//
// Both are MEASURED, and both are omitted when they are not. There is no
// zero-shaped fallback anywhere in this file: "last 7 days: 0 to review" is a
// claim about a week nobody counted and it reads exactly like a quiet week
// (#159/#176). settings/lineWindow.ts holds the arithmetic and is unit-tested,
// because the SIGN of the second sentence is its entire content.
//
// ⚠ NOTHING MOVES UNTIL APPLY, and that rule outranks the page's own `reset`
// convention. Everywhere else on this page a reset writes immediately — that is
// what the Appearance row does, and it is right there, because putting the theme
// back is free. Putting the LINE back moves a screenful of mail across it, so
// `reset` here PROPOSES the default and the preview appears under it; Apply is
// still the only thing that writes. A reset that silently re-split the inbox would
// be exactly the gesture TheLine.tsx exists to prevent.

import { useEffect, useState } from "react";
import type { JSX } from "react";

import { t, tCount } from "../../../i18n";
import { useApp, useMailState } from "../../../state";
import { headerCounts } from "../../../../core/desk";
import { thresholdText } from "../../desk/format";
import { LINE_DEFAULT_THRESHOLD, sevenDayWindow, stricterDelta } from "../lineWindow";

/** How far one click moves the line — the desk's own step, imported in spirit
 * rather than in code so this file does not depend on a desk component. It is
 * ±0.05 in both places and the two must not drift; ui/desk/TheLine.tsx LINE_STEP
 * is the other copy. */
const STEP = 0.05;

/** How long a keystroke or a click waits before asking the server what that line
 * would show. The desk's number (DeskRoute.tsx PREVIEW_DEBOUNCE_MS): long enough
 * that four clicks are one question, short enough that one click feels answered. */
const PREVIEW_DEBOUNCE_MS = 250;

/** Two decimals, clamped into [0,1] — the server refuses anything outside it, and
 * the stored value is a float32 so more precision than this is noise the server
 * would round away anyway. */
function clampLine(v: number): number {
  return Math.min(1, Math.max(0, Math.round(v * 100) / 100));
}

export function LineBody(): JSX.Element {
  const { actions, sync } = useApp();
  const state = useMailState();
  const prefs = state.deskPreferences;
  const summary = state.deskSummary;
  const accountId = state.accountId;

  const [pending, setPending] = useState<number | null>(null);
  /**
   * What is literally in the box, when the user is typing in it.
   *
   * ⚠ IT IS A SEPARATE PIECE OF STATE FROM `pending`, and it has to be. `pending`
   * is a clamped, two-decimal NUMBER — that is what the preview and the write need
   * — and feeding it back into the input would rewrite the field on every
   * keystroke: typing `0.` becomes `0`, the point disappears, and `0.65` is
   * unreachable by hand. So the text is what the user typed and `pending` is what it
   * parsed to, and the two are only re-synchronised by the ± buttons and by
   * cancelling.
   */
  const [text, setText] = useState<string | null>(null);
  const [preview, setPreview] = useState<{ review: number; drawers: number } | null>(null);
  const [previewFailed, setPreviewFailed] = useState(false);
  const [busy, setBusy] = useState(false);

  /** The seven-day split. `undefined` is "not asked yet", `null` is "asked and the
   * server could not answer" — two states, because only the second is worth a
   * sentence saying so. */
  const [week, setWeek] = useState<{ review: number; drawers: number } | null | undefined>(
    undefined,
  );
  /** The rows one step stricter would produce, for the delta sentence. */
  const [stricterRows, setStricterRows] = useState<
    Awaited<ReturnType<typeof sync.previewLine>> | undefined
  >(undefined);

  const threshold = prefs?.lineThreshold ?? null;

  // -- the seven-day split ---------------------------------------------------
  //
  // Once per mount of the row, gated on the session having landed for the reason
  // rows/KeysRow.tsx is: asking before there is an account id fails, and this
  // effect would then not run again — a sticky "could not be measured" on a row
  // that can be.
  useEffect(() => {
    if (!accountId) return;
    let cancelled = false;
    // The window is computed ONCE, here, rather than inside the sync engine: the
    // engine would have to read a clock, and a bound that is a function of `now`
    // is the part worth testing without one (settings/lineWindow.ts).
    const { after } = sevenDayWindow(Date.now());
    void sync.lineWeekCounts(after).then((counts) => {
      if (!cancelled) setWeek(counts);
    });
    return () => {
      cancelled = true;
    };
  }, [accountId, sync]);

  // -- what one step stricter would do ---------------------------------------
  //
  // Re-asked when the STORED line moves, because the sentence is about the line
  // the desk is actually split at — not about whatever is in the input box.
  useEffect(() => {
    if (!accountId || threshold === null) return;
    let cancelled = false;
    void sync.previewLine(clampLine(threshold + STEP)).then((rows) => {
      if (!cancelled) setStricterRows(rows);
    });
    return () => {
      cancelled = true;
    };
  }, [accountId, sync, threshold]);

  // -- the live preview of the pending line ----------------------------------
  useEffect(() => {
    if (pending === null) {
      setPreview(null);
      setPreviewFailed(false);
      return;
    }
    setPreview(null);
    setPreviewFailed(false);
    let cancelled = false;
    const handle = setTimeout(() => {
      void (async () => {
        const rows = await sync.previewLine(pending);
        if (cancelled) return;
        if (!rows) {
          // Said out loud rather than left as a spinner that never resolves, and
          // Apply stays reachable: "the preview failed" is still a state a user can
          // legitimately decide from (TheLine.tsx makes the same call).
          setPreviewFailed(true);
          return;
        }
        setPreview(headerCounts(rows));
      })();
    }, PREVIEW_DEBOUNCE_MS);
    return () => {
      cancelled = true;
      clearTimeout(handle);
    };
  }, [pending, sync]);

  if (threshold === null) {
    // The line cannot be moved from a number this client has not read: "±0.05"
    // would be relative to a value it invented (DeskRoute.tsx lineUnknown).
    return <p className="field-hint">{t("settings.line.unread")}</p>;
  }

  const shown = pending ?? threshold;
  const changed = threshold !== LINE_DEFAULT_THRESHOLD;

  /** Sets both halves at once — for the ± buttons and for the reset, where the
   * number IS the text. */
  const propose = (value: number): void => {
    const next = clampLine(value);
    setPending(next);
    setText(thresholdText(next));
  };
  const cancel = (): void => {
    setPending(null);
    setText(null);
  };

  const apply = (): void => {
    if (pending === null) return;
    setBusy(true);
    void (async () => {
      const ok = await actions.setLineThreshold(pending);
      setBusy(false);
      // Cleared only on success: a refused write must leave the number the user
      // chose in the box, not snap it back to a line they did not pick.
      if (ok) cancel();
    })();
  };

  const delta = stricterDelta(summary?.rows ?? null, stricterRows ?? null);

  return (
    <>
      <fieldset className="menu-group">
        <legend>
          {t("settings.line.threshold")}
          {changed ? (
            <span className="setting-changed">
              <span className="setting-changed-word">{t("settings.changed")}</span>
              <button
                type="button"
                className="link-button"
                disabled={busy}
                // PROPOSES the default; Apply is still what writes. See the header.
                onClick={() => propose(LINE_DEFAULT_THRESHOLD)}
              >
                {t("settings.line.reset", { value: thresholdText(LINE_DEFAULT_THRESHOLD) })}
              </button>
            </span>
          ) : null}
        </legend>

        <div className="field-inline">
          <input
            type="number"
            className="text-input setting-line-input"
            min={0}
            max={1}
            step={STEP}
            value={text ?? thresholdText(threshold)}
            disabled={busy}
            aria-label={t("settings.line.threshold")}
            onChange={(e) => {
              const raw = e.target.value;
              setText(raw);
              const n = Number(raw);
              // A half-typed number (`0.`, ``, `-`) updates the TEXT and leaves the
              // proposal alone: it is not a line, so it must not become a preview
              // request or an Apply that writes a value nobody meant.
              if (raw.trim() === "" || !Number.isFinite(n)) return;
              setPending(clampLine(n));
            }}
          />
          <button
            type="button"
            className="button button-quiet"
            disabled={busy || shown >= 1}
            onClick={() => propose(shown + STEP)}
          >
            {t("desk.line.stricter")}
          </button>
          <button
            type="button"
            className="button button-quiet"
            disabled={busy || shown <= 0}
            onClick={() => propose(shown - STEP)}
          >
            {t("desk.line.looser")}
          </button>
        </div>

        {pending === null ? (
          <p className="field-hint">{t("settings.line.explain")}</p>
        ) : (
          <div className="field-inline">
            <span className="menu-note" role="status">
              {preview
                ? t("desk.line.preview", {
                    threshold: thresholdText(pending),
                    review: preview.review,
                    drawers: preview.drawers,
                  })
                : previewFailed
                  ? t("desk.line.previewFailed")
                  : t("desk.line.previewing")}
            </span>
            <button
              type="button"
              className="button button-primary"
              // Applying a line nobody has seen the consequences of is the thing
              // this control exists to prevent, so Apply waits for the preview —
              // or for it to have failed, which is still a state to decide from.
              disabled={busy || (preview === null && !previewFailed)}
              onClick={apply}
            >
              {t("desk.line.apply")}
            </button>
            <button type="button" className="link-button" disabled={busy} onClick={cancel}>
              {t("action.cancel")}
            </button>
          </div>
        )}
      </fieldset>

      <fieldset className="menu-group">
        <legend>{t("settings.line.facts")}</legend>
        {/* THE WEEK. Three states and three sentences: not asked, asked and
            unanswerable, answered. The middle one exists because a row that went
            quiet would be indistinguishable from a week with no mail in it. */}
        {week === undefined ? (
          <p className="menu-note">{t("settings.line.week.asking")}</p>
        ) : week === null ? (
          <p className="menu-note">{t("settings.line.week.failed")}</p>
        ) : (
          <p className="menu-note">
            {t("settings.line.week", { review: week.review, drawers: week.drawers })}
          </p>
        )}

        {/* ONE STEP STRICTER. Omitted entirely while either side is missing —
            `stricterDelta` returns null for exactly that — and the two ends of the
            range get their own words: a step that moves nothing is a measured fact
            worth stating, and "would move 0" is not the way to state it. */}
        {delta === null ? null : delta > 0 ? (
          <p className="menu-note">
            {tCount("settings.line.stricter", delta, {
              threshold: thresholdText(clampLine(threshold + STEP)),
            })}
          </p>
        ) : (
          <p className="menu-note">
            {t("settings.line.stricterNothing", {
              threshold: thresholdText(clampLine(threshold + STEP)),
            })}
          </p>
        )}
      </fieldset>
    </>
  );
}
