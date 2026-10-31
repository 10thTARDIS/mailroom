// SPDX-License-Identifier: AGPL-3.0-only

// STEP 4 OF 4 — THE LINE AND THE DRAWERS, TO START (#446).
//
// Three dials and a promise. The dials are the three numbers a desk cannot be
// opened without — where the line sits, how long a drawer keeps mail, and after
// how many days something sitting in the tray is worth nagging about — and the
// promise is the closing sentence: what the desk will actually open with.
//
// EVERY NUMBER IN THAT SENTENCE IS MEASURED. `review` and `drawers` come from an
// `Area/getSummary` preview AT THE CHOSEN LINE (sync.previewLine, which stores
// nothing — it is a question about a desk that does not exist yet); `allMail` is
// what the run actually archived, or what the plan would archive when there is no
// run to read; `leave` is the preview's own count of what was deferred, with the
// folders named. A clause whose number is missing is ABSENT, never zero
// (ui/setup/sentences.ts).
//
// ⚠ "NEVER" FOR THE DRAWERS IS `drawerSweepEnabled: false`, WITH THE PERIOD LEFT
// ALONE. A null period has never meant "off" (jmap/types.ts
// Area.autoArchiveDays), and writing a period of zero would mean "sweep
// everything immediately". Leaving the number where it is also means that
// switching the sweep on later lands on the period the user chose here rather than
// on a default they never saw.

import { useEffect, useState } from "react";
import type { JSX } from "react";

import { headerCounts, sectionKey } from "../../../core/desk";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import { useRoute } from "../../routing";
import { thresholdText } from "../desk/format";
import { SetupFooter, StepBody } from "./SetupRoute";
import type { StepProps } from "./SetupRoute";
import {
  SETUP_DRAWER_DAYS,
  SETUP_LINES,
  choiceFor,
  planFor,
} from "./setupState";
import { countText, desklineSentence } from "./sentences";

/** The bulk folder the nudge suggests a shorter period for. 14 days is the
 * design's own number, and it is a SUGGESTION printed beside the folder's real
 * count — never a default applied on the user's behalf. */
const BULK_NUDGE_DAYS = 14;

export function SetupDialsStep(props: StepProps): JSX.Element {
  const { state, dispatch, census, onBack, onSkip, onFinish } = props;
  const { actions, sync } = useApp();
  const mail = useMailState();
  const route = useRoute();

  /** The last preview's split at the chosen line. `undefined` while one is in
   * flight, `null` when the server could not answer — and only the second is worth
   * a sentence saying so. */
  const [preview, setPreview] = useState<{ review: number; drawers: number } | null | undefined>(
    undefined,
  );
  /** What day-one filed away and what it left, from the RUN when there is one and
   * from the plan when there is not. Null when neither could be read. */
  const [filed, setFiled] = useState<{ allMail: number; leave: number } | null>(null);
  const [busy, setBusy] = useState(false);

  // -- the line ---------------------------------------------------------------
  //
  // One request per press. `previewLine` stores nothing, so the desk underneath is
  // never redrawn at a threshold nobody has agreed to.
  useEffect(() => {
    let cancelled = false;
    setPreview(undefined);
    void sync.previewLine(state.line).then((rows) => {
      if (cancelled) return;
      setPreview(rows === null ? null : headerCounts(rows));
    });
    return () => {
      cancelled = true;
    };
  }, [sync, state.line]);

  // -- what the desk will hold ----------------------------------------------
  const runId = state.runId;
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      if (runId) {
        // THE RUN'S OWN COUNTS BEAT THE PLAN'S. After an import, "4,276 in All
        // mail" should be what was WRITTEN, not what was promised — the whole
        // point of the run carrying both.
        const runs = await actions.setupRuns([runId]);
        const run = runs?.[0];
        if (cancelled) return;
        if (run) {
          setFiled({ allMail: run.archived, leave: run.leftForLater });
          return;
        }
      }
      const counts = await actions.setupPreview(planFor(state, census));
      if (cancelled) return;
      setFiled(counts === null ? null : { allMail: counts.archived, leave: counts.leave });
    })();
    return () => {
      cancelled = true;
    };
    // The plan is not a dependency: this describes what step 2 did, and step 4
    // cannot change it. Re-reading on every keystroke in the nag field would be a
    // request per character for a number that cannot have moved.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [actions, runId]);

  /** How much is newly arrived — the census's own `inWindow` total, which is the
   * denominator the line's sentence is about. */
  const newly = census.reduce((sum, f) => sum + f.inWindow, 0);

  /**
   * The tray count, from the desk's own section if it has been loaded.
   *
   * NULL WHEN IT HAS NOT BEEN. A wizard reached from a pasted URL has never drawn
   * a desk, so there is no tray count anywhere in the store — and "0 in the tray"
   * would be a number nobody measured. The scope is the route's, so a wizard run
   * inside one account's scope reads that account's tray.
   */
  const tray =
    mail.sections[sectionKey("tray", null, route.acct ?? null)]?.loadedAt != null
      ? (mail.sections[sectionKey("tray", null, route.acct ?? null)]?.total ?? null)
      : null;

  const leaveFolders = census
    .filter((f) => choiceFor(state, f).action === "leave")
    .map((f) => f.name);

  const closing = desklineSentence({
    tray,
    review: preview?.review ?? null,
    drawers: preview?.drawers ?? null,
    allMail: filed?.allMail ?? null,
    leave: filed?.leave ?? null,
    leaveFolders,
  });

  /** The folder the nudge is about: the biggest bulk one, so a mailbox with three
   * newsletter folders gets one suggestion rather than three. */
  const bulk = census
    .filter((f) => f.shape === "bulk")
    .sort((a, b) => b.live - a.live)[0];

  const open = (): void => {
    setBusy(true);
    void onFinish({
      lineThreshold: state.line,
      trayNagDays: state.nagDays,
      // See the file header: "never" is the SWITCH, and the period is left where
      // the user put it.
      ...(state.drawerDays === null
        ? { drawerSweepEnabled: false }
        : { drawerSweepEnabled: true, drawerDefaultDays: state.drawerDays }),
    }).finally(() => setBusy(false));
  };

  return (
    <StepBody>
      <div className="setup-dials">
        <fieldset className="menu-group">
          <legend>{t("setup.dials.line")}</legend>
          <div className="setup-dial-row">
            {SETUP_LINES.map((line) => (
              <button
                key={line}
                type="button"
                className={`button button-quiet${state.line === line ? " is-chosen" : ""}`}
                // `aria-pressed` is the state, said in the markup rather than by the
                // class: a toggle whose only cue is a border is not readable.
                aria-pressed={state.line === line}
                onClick={() => dispatch({ type: "line", line })}
              >
                {thresholdText(line)}
              </button>
            ))}
            <span className="setup-dial-note">
              {preview === undefined
                ? t("setup.dials.line.previewing")
                : preview === null
                  ? t("setup.dials.line.previewFailed")
                  : t("setup.dials.line.preview", {
                      threshold: thresholdText(state.line),
                      review: countText(preview.review),
                      newly: countText(newly),
                    })}
            </span>
          </div>
          <p className="field-hint">{t("setup.dials.line.later")}</p>
        </fieldset>

        <fieldset className="menu-group">
          <legend>{t("setup.dials.drawers")}</legend>
          <div className="setup-dial-row">
            <label className="radio">
              <input
                type="radio"
                name="setup-drawers"
                checked={state.drawerDays === null}
                onChange={() => dispatch({ type: "drawerDays", days: null })}
              />{" "}
              {t("setup.dials.drawers.never")}
            </label>
            {SETUP_DRAWER_DAYS.map((days) => (
              <label key={days} className="radio">
                <input
                  type="radio"
                  name="setup-drawers"
                  checked={state.drawerDays === days}
                  onChange={() => dispatch({ type: "drawerDays", days })}
                />{" "}
                {t("setup.dials.drawers.days", { n: days })}
              </label>
            ))}
            <span className="setup-dial-note">{t("setup.dials.drawers.perArea")}</span>
          </div>
          {/* THE NUDGE IS A SUGGESTION WITH ITS EVIDENCE ATTACHED — the folder and
              how much is in it — and it changes nothing on its own. */}
          {bulk ? (
            <p className="menu-note">
              {t("setup.dials.drawers.nudge", {
                name: bulk.name,
                n: countText(bulk.live),
                days: BULK_NUDGE_DAYS,
              })}
            </p>
          ) : null}
        </fieldset>

        <fieldset className="menu-group">
          <legend>{t("setup.dials.tray")}</legend>
          <div className="setup-dial-row">
            <label className="field-inline">
              <span className="field-label">{t("setup.dials.tray.nagLabel")}</span>
              <input
                type="number"
                className="text-input setup-nag-input"
                min={1}
                value={state.nagDays}
                onChange={(e) => {
                  const n = Number(e.target.value);
                  // A half-typed number is not a setting: the field shows what was
                  // typed and the state keeps the last real value, so Open the desk
                  // can never write a NaN.
                  if (Number.isFinite(n) && n >= 1) dispatch({ type: "nagDays", days: Math.floor(n) });
                }}
              />
            </label>
            <span className="setup-dial-note">{t("setup.dials.tray.empty")}</span>
          </div>
        </fieldset>
      </div>

      {/* The closing promise, in the same live region idiom the "This would…"
          sentence uses: it changes as the line moves, and it is the last thing read
          before the desk opens. */}
      {closing === null ? null : (
        <div className="setup-status" role="status">
          <p className="setup-status-sentence">{closing}</p>
        </div>
      )}

      <SetupFooter
        onBack={onBack}
        onSkip={onSkip}
        next={{ label: t("setup.finish"), onClick: open, disabled: busy }}
      />
    </StepBody>
  );
}
