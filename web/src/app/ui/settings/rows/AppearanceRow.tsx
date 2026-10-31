// SPDX-License-Identifier: AGPL-3.0-only

// APPEARANCE (#374 increment 2) — the one row whose scope is `web`, and the row
// the "defaults visible" rule was written for.
//
// It holds what #373's AppearanceSheet held, with one change of emphasis: every
// control now says whether the user has MOVED it, and offers to put it back. A
// page that annotated all four equally would tell you nothing about your own
// configuration; a default says nothing, so only a change is marked. What "a
// change" means is decided by the pure `changedControls` next door, and the reset
// writes the default through THE SAME PATH the control uses — a reset with its own
// write path would be a second way to set a preference and eventually a second
// answer about what the preference is.
//
// FIVE OF THE SIX ARE SYNCED, ONE IS NOT, and the row says so rather than
// leaving the user to discover it. Theme, density, text size, rows and
// beside/focused are `ClientPreferences` for the `web` family; the ORIENTATION
// (beside-left vs beside-below) is device-only and deliberately so — a width that reads well on a
// 27-inch screen is not a height on a laptop — which is why it lives behind
// `more` rather than beside the three that follow you between browsers.
//
// THE SPLIT IS A READOUT WITH A RESET, not a slider. The divider on the desk is
// the control (dragging it is how it has always been set, and it is the only
// affordance that shows you the result as you choose it); duplicating it here as a
// number field would be a second control for one value. What this row adds is the
// two things the divider cannot say: the number, and that it is synced.
//
// ⚠ THE STATE IS OWNED BY `useAppearance` AND READ ONCE PER PAGE. The row's state
// LINE and the row's CONTROLS describe the same four values, so the page calls the
// hook and hands the result to the body (SettingsRoute.tsx). Calling it in both
// places would give the line and the radios independent copies of the theme — and
// they would disagree for the length of a server round trip, which is exactly the
// "two answers on one screen" failure #373's sheet already had to fix once.

import { useEffect, useState } from "react";
import type { JSX } from "react";

import { t } from "../../../i18n";
import type { MessageKey } from "../../../i18n";
import {
  TEXT_SIZES,
  applyDensity,
  applyRowLayout,
  applyTextSize,
  applyTheme,
  contentSizePx,
  densityToServer,
  loadBesideLayout,
  loadDensity,
  loadRowLayout,
  loadTextSize,
  loadTheme,
  pctToPx,
  stepFromTextSize,
  textSizeFromStep,
} from "../../../prefs";
import type { Density, PaneLayout, RowLayout, TextSize, Theme } from "../../../prefs";
import { useApp, useMailState } from "../../../state";
import { useShell } from "../../shellContext";
import { APPEARANCE_DEFAULTS, changedControls } from "../changed";
import type { AppearanceControl, AppearanceState } from "../changed";

/**
 * The four appearance values as they stand, plus whether the synced split has
 * actually been read.
 *
 * ⚠ `splitKnown` IS NOT COSMETIC. Before `ClientPreferences/get` lands, this app
 * has NO percentage — the store's map is `{}`, and that is not "the default"
 * (core/store.ts says so at the field). So the row prints "not synced yet"
 * instead of `58 / 42`, and `changedControls` is fed the default so it reports
 * the split as unchanged: "unknown" is not "changed", and a `changed` badge on a
 * number nobody has read is the #159 shape — a claim that reads perfectly and is
 * about nothing.
 */
export interface AppearanceReading extends AppearanceState {
  splitKnown: boolean;
}

export interface AppearanceApi {
  reading: AppearanceReading;
  changed: AppearanceControl[];
  pickTheme: (v: Theme) => void;
  pickDensity: (v: Density) => void;
  pickTextSize: (v: TextSize) => void;
  /** Move the text size by whole steps — what the slider's arrow keys do.
   * CLAMPED at both ends by prefs.ts's textSizeFromStep, so → at Largest stays
   * at Largest rather than wrapping round to Smallest. */
  stepTextSize: (delta: number) => void;
  pickRowLayout: (v: RowLayout) => void;
  pickOpens: (v: "beside" | "focused") => void;
  resetSplit: () => void;
}

/**
 * The current appearance and the four ways to change it.
 *
 * EVERY WRITE APPLIES LOCALLY FIRST AND WRITES UPWARD SECOND — the click has to
 * feel immediate, the device has to keep working offline, and a refused write
 * leaves the device on the value the user chose rather than snapping back
 * (core/actions.ts saveClientPreferences says the same from the other side).
 *
 * Theme and density are re-read whenever the synced `web` record moves, for the
 * reason #373's sheet already did it: without that, the radios keep showing what
 * was chosen HERE while the app around them is in the state that arrived from
 * another device.
 */
export function useAppearance(): AppearanceApi {
  const shell = useShell();
  const { actions } = useApp();
  const state = useMailState();
  const web = state.clientPreferences["web"];
  const [theme, setTheme] = useState<Theme>(() => loadTheme());
  const [density, setDensity] = useState<Density>(() => loadDensity());
  const [textSize, setTextSize] = useState<TextSize>(() => loadTextSize());
  const [rowLayout, setRowLayout] = useState<RowLayout>(() => loadRowLayout());

  useEffect(() => {
    setTheme(loadTheme());
    setDensity(loadDensity());
    setTextSize(loadTextSize());
    setRowLayout(loadRowLayout());
  }, [web]);

  const reading: AppearanceReading = {
    theme,
    density,
    textSize,
    rowLayout,
    messageOpens: shell.paneLayout === "off" ? "focused" : "beside",
    splitPct: web?.paneSplitPct ?? APPEARANCE_DEFAULTS.splitPct,
    splitKnown: web !== undefined,
  };

  const pickTheme = (v: Theme): void => {
    setTheme(v);
    applyTheme(v);
    void actions.saveClientPreferences({ theme: v });
  };

  const pickDensity = (v: Density): void => {
    setDensity(v);
    applyDensity(v);
    // The two vocabularies differ (prefs.ts): sending "cozy" would be refused.
    void actions.saveClientPreferences({ density: densityToServer(v) });
  };

  /**
   * The text size and the row layout (#514).
   *
   * THE PAGE IS ITS OWN PREVIEW, which is why these apply locally FIRST like
   * every other write here and why no preview mechanism was built: the control
   * that changes the text is drawn in that text, inside an app drawn in that
   * text. There is nothing a swatch could show that the screen is not already
   * showing.
   *
   * ⚠ THE WORD GOES STRAIGHT ACROSS, and that is not an oversight of the
   * density line above. `textSize`/`rowLayout` are spelled identically on both
   * sides (prefs.ts, jmap/types.ts); density is the one that needs a mapping.
   */
  const pickTextSize = (v: TextSize): void => {
    setTextSize(v);
    applyTextSize(v);
    void actions.saveClientPreferences({ textSize: v });
  };

  const stepTextSize = (delta: number): void => {
    const next = textSizeFromStep(stepFromTextSize(textSize) + delta);
    // A key press that lands on the step you are already on writes nothing:
    // arrowing into the end of the range must not fire a request per keystroke.
    if (next !== textSize) pickTextSize(next);
  };

  const pickRowLayout = (v: RowLayout): void => {
    setRowLayout(v);
    applyRowLayout(v);
    void actions.saveClientPreferences({ rowLayout: v });
  };

  // Choosing Beside restores the orientation this device last had one in, rather
  // than defaulting to vertical and quietly discarding a horizontal pane the user
  // chose. setPaneLayout does the local apply and the upward write together.
  const pickOpens = (v: "beside" | "focused"): void => {
    shell.setPaneLayout(v === "focused" ? "off" : loadBesideLayout());
  };

  /**
   * Puts the divider back to 58 %.
   *
   * TWO PATHS, and the line between them is the one prefs.ts draws. When the pane
   * is on screen the percent is converted against THIS device's content area and
   * applied through `resizePane` — the identical path a drag takes, so the divider
   * moves and the write happens exactly once. When it is not (the pane is focused,
   * or the container cannot be measured), only the PERCENT is written: converting
   * against an unknown width would put the divider somewhere nobody chose, and the
   * shell converts it properly the moment the pane exists (applyServerPrefs).
   */
  const resetSplit = (): void => {
    if (shell.paneLayout !== "off") {
      const container = contentSizePx(shell.paneLayout);
      const px =
        container === null
          ? null
          : pctToPx(APPEARANCE_DEFAULTS.splitPct, container, shell.paneLayout);
      if (px !== null) {
        shell.resizePane(px);
        return;
      }
    }
    void actions.saveClientPreferences({ paneSplitPct: APPEARANCE_DEFAULTS.splitPct });
  };

  return {
    reading,
    changed: changedControls(reading),
    pickTheme,
    pickDensity,
    pickTextSize,
    stepTextSize,
    pickRowLayout,
    pickOpens,
    resetSplit,
  };
}

/** The state line's three facts, in the design's words: `theme dark · density
 * normal · message opens beside the list`.
 *
 * The DENSITY WORD is the server's (relaxed / normal / compact), not this app's
 * (comfortable / cozy / compact), because the design prints the vocabulary the
 * value is SYNCED in — that is the word another device shows for the same
 * setting. prefs.ts owns the mapping; this is the one place the page consults it
 * for display rather than for a write. */
export function appearanceStateText(a: AppearanceReading): string[] {
  return [
    t("settings.state.theme", { value: t(`settings.theme.${a.theme}` as MessageKey) }),
    t("settings.state.density", {
      value: t(`settings.serverDensity.${densityToServer(a.density)}` as MessageKey),
    }),
    // ⚠ THE TWO NEW FACTS APPEAR ONLY WHEN THEY ARE NOT THE DEFAULT (#514),
    // which is this page own rule and not an inconsistency with the three
    // around them. Theme, density and beside/focused are the three questions
    // the row has always been ABOUT, and the line is where you read them; text
    // size and rows are settings you either changed or have never thought
    // about, and "text Default · rows One line" on every login Appearance line
    // would be two-thirds of a sentence saying nothing. The `changed` badges
    // beside the controls say the same thing from the other direction.
    ...(a.textSize === APPEARANCE_DEFAULTS.textSize
      ? []
      : [
          t("settings.state.textSize", {
            value: t(`settings.textSize.${a.textSize}` as MessageKey),
          }),
        ]),
    ...(a.rowLayout === APPEARANCE_DEFAULTS.rowLayout
      ? []
      : [t("settings.state.rows", { value: t(`settings.rows.${a.rowLayout}` as MessageKey) })]),
    t(a.messageOpens === "beside" ? "settings.state.opensBeside" : "settings.state.opensFocused"),
  ];
}

/** `changed` plus the per-control reset, or nothing at all. Rendered inside the
 * control's own legend so the two cannot be read apart. */
function Changed({
  control,
  changed,
  onReset,
}: {
  control: AppearanceControl;
  changed: readonly AppearanceControl[];
  onReset: () => void;
}): JSX.Element | null {
  if (!changed.includes(control)) return null;
  return (
    <span className="setting-changed">
      <span className="setting-changed-word">{t("settings.changed")}</span>
      <button type="button" className="link-button" onClick={onReset}>
        {t("settings.reset")}
      </button>
    </span>
  );
}

export function AppearanceBody({ api }: { api: AppearanceApi }): JSX.Element {
  const {
    reading,
    changed,
    pickTheme,
    pickDensity,
    pickTextSize,
    stepTextSize,
    pickRowLayout,
    pickOpens,
    resetSplit,
  } = api;

  return (
    <>
      <fieldset className="menu-group">
        <legend>
          {t("settings.theme")}
          <Changed
            control="theme"
            changed={changed}
            onReset={() => pickTheme(APPEARANCE_DEFAULTS.theme)}
          />
        </legend>
        {(["system", "light", "dark"] as Theme[]).map((v) => (
          <label className="radio" key={v}>
            <input
              type="radio"
              name="theme"
              value={v}
              checked={reading.theme === v}
              onChange={() => pickTheme(v)}
            />
            {t(`settings.theme.${v}` as MessageKey)}
          </label>
        ))}
      </fieldset>

      <fieldset className="menu-group">
        <legend>
          {t("settings.density")}
          <Changed
            control="density"
            changed={changed}
            onReset={() => pickDensity(APPEARANCE_DEFAULTS.density)}
          />
        </legend>
        {(["comfortable", "cozy", "compact"] as Density[]).map((v) => (
          <label className="radio" key={v}>
            <input
              type="radio"
              name="density"
              value={v}
              checked={reading.density === v}
              onChange={() => pickDensity(v)}
            />
            {t(`settings.density.${v}` as MessageKey)}
          </label>
        ))}
      </fieldset>

      {/* TEXT SIZE (#514) — a five-step control that is TWO controls in the DOM,
          deliberately, and they are SIBLINGS rather than nested.

          The `role="slider"` is the keyboard's and the screen reader's: one tab
          stop, arrows to move, `aria-valuetext` saying the WORD ("Large")
          rather than the number, because "3 of 5" is not a size. The five words
          are the pointer's: a slider you can only drag is a slider a mouse user
          has to aim at, and this range has exactly five stops, all of which fit
          on a line as words.

          ⚠ THE BUTTONS ARE NOT INSIDE THE SLIDER. `slider` is one of the roles
          that takes no focusable descendants, so nesting them would be a real
          violation (axe's `nested-interactive`, WCAG 4.1.2) and the
          accessibility lane would say so. The two controls sit side by side and
          drive the same value.

          There is no preview swatch because the page IS the preview: the words
          below, this legend, the row above and every list behind this sheet are
          all drawn at the size being chosen. */}
      <fieldset className="menu-group">
        <legend>
          {t("settings.textSize")}
          <Changed
            control="textSize"
            changed={changed}
            onReset={() => pickTextSize(APPEARANCE_DEFAULTS.textSize)}
          />
        </legend>
        <div
          className="text-size-slider"
          role="slider"
          tabIndex={0}
          aria-label={t("settings.textSize")}
          aria-valuemin={1}
          aria-valuemax={TEXT_SIZES.length}
          aria-valuenow={stepFromTextSize(reading.textSize)}
          aria-valuetext={t(`settings.textSize.${reading.textSize}` as MessageKey)}
          onKeyDown={(ev) => {
            // Home/End ARE implemented here, unlike on the desk line slider
            // (web/ACCESSIBILITY.md says why they are not there): the ends of
            // this range are "smallest" and "largest", which is exactly what
            // Home and End mean. The ends of the line range are "show
            // everything" and "show nothing", which they do not.
            const delta =
              ev.key === "ArrowRight" || ev.key === "ArrowUp"
                ? 1
                : ev.key === "ArrowLeft" || ev.key === "ArrowDown"
                  ? -1
                  : 0;
            if (delta !== 0) {
              ev.preventDefault();
              stepTextSize(delta);
              return;
            }
            if (ev.key === "Home") {
              ev.preventDefault();
              pickTextSize(TEXT_SIZES[0] as TextSize);
            } else if (ev.key === "End") {
              ev.preventDefault();
              pickTextSize(TEXT_SIZES[TEXT_SIZES.length - 1] as TextSize);
            }
          }}
        >
          {/* Decorative: the slider own value text is what says where it is. A
              tick is not a control — clicking one does nothing, because the
              word under it is the target. */}
          {TEXT_SIZES.map((v) => (
            <span
              key={v}
              aria-hidden="true"
              className={`text-size-tick${v === reading.textSize ? " is-current" : ""}`}
            />
          ))}
        </div>
        <div className="text-size-steps">
          {TEXT_SIZES.map((v) => (
            <button
              key={v}
              type="button"
              className={`button text-size-step${v === reading.textSize ? " is-current" : ""}`}
              // The pointer path to the same value. `aria-pressed` rather than
              // a radio group because the slider beside it already IS the
              // group — two radiogroups for one value would be announced as two
              // separate settings.
              aria-pressed={v === reading.textSize}
              data-size={v}
              onClick={() => pickTextSize(v)}
            >
              {t(`settings.textSize.${v}` as MessageKey)}
            </button>
          ))}
        </div>
      </fieldset>

      {/* ROWS (#514). Radios, styled as a segmented control, for the reason the
          three groups around it are radios: this is a settings page, the
          fieldset+legend is already the group name, and a radio group is the
          one shape every assistive technology reads as "one of these". The
          segmented LOOK is CSS. */}
      <fieldset className="menu-group">
        <legend>
          {t("settings.rows")}
          <Changed
            control="rowLayout"
            changed={changed}
            onReset={() => pickRowLayout(APPEARANCE_DEFAULTS.rowLayout)}
          />
        </legend>
        <div className="segmented">
          {(["oneLine", "multiLine"] as RowLayout[]).map((v) => (
            <label className="radio segmented-option" key={v}>
              <input
                type="radio"
                name="rowLayout"
                value={v}
                checked={reading.rowLayout === v}
                onChange={() => pickRowLayout(v)}
              />
              {t(`settings.rows.${v}` as MessageKey)}
            </label>
          ))}
        </div>
        <p className="menu-note">{t("settings.rows.hint")}</p>
      </fieldset>

      {/* Beside vs focused — the SYNCED half. "Focused" is PaneLayout "off": one
          state, named twice, because the wire word and the CSS word are different
          vocabularies and neither is worth renaming. */}
      <fieldset className="menu-group">
        <legend>
          {t("settings.messageOpens")}
          <Changed
            control="messageOpens"
            changed={changed}
            onReset={() => pickOpens(APPEARANCE_DEFAULTS.messageOpens)}
          />
        </legend>
        {(["beside", "focused"] as const).map((v) => (
          <label className="radio" key={v}>
            <input
              type="radio"
              name="messageOpens"
              value={v}
              checked={reading.messageOpens === v}
              onChange={() => pickOpens(v)}
            />
            {t(`settings.messageOpens.${v}` as MessageKey)}
          </label>
        ))}
      </fieldset>

      <fieldset className="menu-group">
        <legend>
          {t("settings.split")}
          <Changed control="split" changed={changed} onReset={resetSplit} />
        </legend>
        <p className="menu-note">
          {reading.splitKnown
            ? t("settings.split.value", {
                list: String(Math.round(reading.splitPct)),
                message: String(100 - Math.round(reading.splitPct)),
              })
            : t("settings.split.unknown")}
        </p>
        <p className="menu-note">{t("settings.split.hint")}</p>
      </fieldset>

      <p className="setting-footer">{t("settings.appearance.scopeNote")}</p>
    </>
  );
}

/**
 * The ORIENTATION, behind `more`.
 *
 * Device-only, and the rarer of the two questions: which side the pane is on is a
 * decision you make once per screen, while beside/focused is one you revisit. "No
 * reading pane" is the same state as Focused above and is kept because it is the
 * wording this control has always had; both write through shell.setPaneLayout, so
 * the two fieldsets cannot disagree.
 */
export function AppearanceMore(): JSX.Element {
  const shell = useShell();
  return (
    <fieldset className="menu-group">
      <legend>{t("settings.paneLayout")}</legend>
      {(["vertical", "horizontal", "off"] as PaneLayout[]).map((v) => (
        <label className="radio" key={v}>
          <input
            type="radio"
            name="paneLayout"
            value={v}
            checked={shell.paneLayout === v}
            onChange={() => shell.setPaneLayout(v)}
          />
          {t(`settings.paneLayout.${v}` as MessageKey)}
        </label>
      ))}
      <p className="menu-note">{t("settings.paneLayout.deviceOnly")}</p>
    </fieldset>
  );
}
