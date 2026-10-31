// SPDX-License-Identifier: AGPL-3.0-only

// User preferences that live only in the browser: theme and density (spec §12
// — "light + dark modes (system-follow + manual)" and "density setting").
//
// Applied as data-* attributes on <html>, which is what styles.css keys off.
// No transparency anywhere in the palettes (spec §12): every surface is a solid
// colour, so text never sits on a blend of two layers.

export type Theme = "system" | "light" | "dark";
export type Density = "comfortable" | "cozy" | "compact";

/** How large the whole app draws (#514). Five steps, and the WORDS ARE THE
 *  SERVER'S — unlike density, whose two vocabularies differ (see below), these
 *  are spelled the same on both sides (`client_preferences.text_size`,
 *  migration 00052), so there is no mapping to get wrong and deliberately no
 *  mapping function to invite one. */
export type TextSize = "xs" | "s" | "m" | "l" | "xl";

/** How a row of mail is drawn (#514): the redrawn single line, or three lines
 *  (sender + marks + time / subject / snippet). Server-exact, like TextSize. */
export type RowLayout = "oneLine" | "multiLine";

/** Where the reading pane sits relative to the message list (spec §12, #61):
 *  vertical   — list left, message right (the default)
 *  horizontal — list on top, message below; more line length for wide screens
 *  off        — no reading pane; opening a message replaces the list */
export type PaneLayout = "vertical" | "horizontal" | "off";

const THEME_KEY = "mailroom.theme";
const DENSITY_KEY = "mailroom.density";
const TEXT_SIZE_KEY = "mailroom.textSize";
const ROW_LAYOUT_KEY = "mailroom.rowLayout";
const PANE_LAYOUT_KEY = "mailroom.paneLayout";
const PANE_SPLIT_KEY = "mailroom.paneSplit";
const TRAY_OPEN_KEY = "mailroom.desk.trayOpen";
/** Which desk sections are folded to their headings (#375 increment 7). */
const COLLAPSED_KEY = "mailroom.desk.collapsed";
/** The last layout the reading pane was BESIDE the list in (#375 increment 5).
 *  `messageOpens: "focused"` is `PaneLayout "off"`, and going back to "beside"
 *  has to restore the orientation the user chose rather than silently picking
 *  vertical — the horizontal layout exists for people with wide screens, and
 *  losing it on every focused/beside round trip is how a preference stops being
 *  believed. */
const PANE_BESIDE_KEY = "mailroom.paneLayout.beside";

/** Splitter bounds, in px, per orientation. The minimums are what keeps a drag
 *  from collapsing either pane to an unusable sliver — a pane you cannot see is
 *  a pane you cannot drag back. */
const SPLIT_BOUNDS: Record<"vertical" | "horizontal", { min: number; max: number; def: number }> = {
  vertical: { min: 280, max: 720, def: 380 },
  horizontal: { min: 160, max: 800, def: 320 },
};

export function splitBounds(layout: PaneLayout): { min: number; max: number; def: number } {
  return SPLIT_BOUNDS[layout === "horizontal" ? "horizontal" : "vertical"];
}

/** The percent range the SERVER will store (`client_preferences.pane_split_pct`
 *  CHECK, migration 00046; internal/api/desk_preferences.go refuses anything
 *  else). Mirrored here so a device never spends a round trip to be told no. */
export const SPLIT_PCT_BOUNDS = { min: 20, max: 80 } as const;

export function clampSplitPct(pct: number): number {
  return Math.round(Math.min(SPLIT_PCT_BOUNDS.max, Math.max(SPLIT_PCT_BOUNDS.min, pct)));
}

/**
 * The synced percent, in this device's pixels — and back.
 *
 * ⚠ THE TWO UNITS ARE NOT INTERCHANGEABLE, WHICH IS THE WHOLE POINT.
 * `ClientPreferences.paneSplitPct` is a PERCENT because it is shared between
 * devices, and a laptop's 380px list is a phone's whole screen; `--pane-split`
 * is PIXELS because that is what a CSS grid track and a drag gesture are. The
 * percent is the LIST pane's share of the content area — the first grid track
 * in styles.css — because that is the track `--pane-split` sizes.
 *
 * Both answer `null` for a container they cannot measure (a content area that
 * is not on screen yet, a zero-height pane behind a modal). Null is not zero
 * and it is not a default: converting from a container of unknown size would
 * either write a percent nobody chose to the server or apply a size nobody
 * chose to the device, and both are the "it ran and changed nothing visible,
 * but wrongly" failure. The caller skips instead.
 */
export function pctToPx(pct: number, containerPx: number, layout: PaneLayout): number | null {
  if (!Number.isFinite(pct) || !Number.isFinite(containerPx) || containerPx <= 0) return null;
  return clampSplit(layout, (containerPx * pct) / 100);
}

export function pxToPct(px: number, containerPx: number): number | null {
  if (!Number.isFinite(px) || !Number.isFinite(containerPx) || containerPx <= 0) return null;
  return clampSplitPct((px / containerPx) * 100);
}

/**
 * The content area's size along the splitter's axis, or null when it cannot be
 * measured.
 *
 * `#messages` is the route's `<main class="content">` — the element whose grid
 * `--pane-split` sizes the first track of, and the same id the shell's skip
 * link already reaches for. Measuring the WINDOW instead would overstate it by
 * the width of the navigation rail, and a percent taken against the wrong
 * container is a number that looks fine and means something else.
 */
export function contentSizePx(layout: PaneLayout): number | null {
  const el = typeof document === "undefined" ? null : document.getElementById("messages");
  if (!el) return null;
  const box = el.getBoundingClientRect();
  if (layout === "horizontal") return box.height > 0 ? box.height : null;
  // ⚠ A DOCKED COMPOSER OR VIEWER IS PART OF THE CONTENT AREA (#617). It takes
  // the reading pane's column as its own WORKSPACE track (compose.css,
  // viewer.css), which leaves `.content` holding the list alone — so measured
  // on its own, the percent was taken of the list's own width. A force refresh
  // with a draft open then turned the synced 31% into 31% of the list: pinned
  // at the 280px minimum, and written to localStorage so it stayed. With nothing
  // docked this is exactly `box.width`, so no stored split changes meaning. (The
  // horizontal split is a HEIGHT, and `.content` keeps the full height when
  // something is docked, so that axis never had the problem.)
  const docked = document.querySelector(".compose-pane, .viewer-pane");
  const right = docked ? Math.max(box.right, docked.getBoundingClientRect().right) : box.right;
  const px = right - box.left;
  return px > 0 ? px : null;
}

export function clampSplit(layout: PaneLayout, px: number): number {
  const { min, max } = splitBounds(layout);
  return Math.round(Math.min(max, Math.max(min, px)));
}

export function loadTheme(): Theme {
  const v = localStorage.getItem(THEME_KEY);
  return v === "light" || v === "dark" || v === "system" ? v : "system";
}

/**
 * ⚠ THE TWO DENSITY VOCABULARIES ARE DIFFERENT WORDS FOR THE SAME THREE STEPS.
 * This app has always said comfortable / cozy / compact (spec §12's "density
 * setting"); the server's `client_preferences.density` CHECK says relaxed /
 * normal / compact (migration 00046, internal/api/desk.go deskDensities). Only
 * "compact" is spelled the same, so a value handed straight across would be
 * refused for two of the three — a settings control that works for one option
 * and silently fails for the others.
 *
 * It is a 1:1 mapping in both directions and it is written down HERE, once,
 * rather than at each call site. Renaming either vocabulary is a migration on
 * one side or a locale change on the other; this is the seam between them.
 */
export type ServerDensity = "compact" | "normal" | "relaxed";

export function densityFromServer(d: string): Density {
  return d === "relaxed" ? "comfortable" : d === "compact" ? "compact" : "cozy";
}

export function densityToServer(d: Density): ServerDensity {
  return d === "comfortable" ? "relaxed" : d === "compact" ? "compact" : "normal";
}

export function loadDensity(): Density {
  const v = localStorage.getItem(DENSITY_KEY);
  return v === "comfortable" || v === "cozy" || v === "compact" ? v : "cozy";
}

export function applyTheme(theme: Theme): void {
  const root = document.documentElement;
  const resolved =
    theme === "system"
      ? window.matchMedia("(prefers-color-scheme: dark)").matches
        ? "dark"
        : "light"
      : theme;
  root.dataset["theme"] = resolved;
  // Keeps native form controls / scrollbars in step with the chosen theme.
  root.style.colorScheme = resolved;
  localStorage.setItem(THEME_KEY, theme);
}

export function applyDensity(density: Density): void {
  document.documentElement.dataset["density"] = density;
  localStorage.setItem(DENSITY_KEY, density);
}

/* ---- text size and row layout (#514) ------------------------------------- */

/**
 * The five steps, SMALLEST FIRST — which is also the order the server
 * advertises them in (`urn:mailroom:desk`'s `textSizes`) and the order the
 * settings slider walks. The index in this array IS the slider's position, so
 * the array is the single definition of "which way is bigger" and nothing else
 * gets to have an opinion about it.
 */
export const TEXT_SIZES: readonly TextSize[] = ["xs", "s", "m", "l", "xl"];

/**
 * What each step sets the ROOT font size to, in pixels.
 *
 * ⚠ DECLARED HERE FOR THE TESTS AND FOR READERS; THE VALUES THAT ACTUALLY RUN
 * ARE IN tokens.css (`html[data-text-size="…"] { font-size: …px }`), because a
 * root size applied from JavaScript would arrive after first paint and the app
 * would visibly resize itself on every load. The two must agree, so the unit
 * test reads tokens.css and compares — a comment promising they match is
 * exactly the thing that stops being true.
 *
 * `m` is 14px: the size this app has always drawn at, which is why it is the
 * default and why the middle of the range is where an existing user starts.
 */
export const TEXT_SIZE_PX: Readonly<Record<TextSize, number>> = {
  xs: 12,
  s: 13,
  m: 14,
  l: 16,
  xl: 18,
};

export const DEFAULT_TEXT_SIZE: TextSize = "m";
export const DEFAULT_ROW_LAYOUT: RowLayout = "oneLine";

/** The guard every value coming from OUTSIDE this module goes through — the
 *  server, localStorage, a hand-edited URL. An unknown word falls back to the
 *  default rather than reaching `<html>`, for `densityFromServer`'s reason: a
 *  `data-text-size="huge"` matches no rule in tokens.css and would silently
 *  leave the app at whatever size it happened to be, which looks like the
 *  setting not working rather than like a bad value. */
export function textSizeFromServer(v: string): TextSize {
  return (TEXT_SIZES as readonly string[]).includes(v) ? (v as TextSize) : DEFAULT_TEXT_SIZE;
}

export function rowLayoutFromServer(v: string): RowLayout {
  return v === "multiLine" ? "multiLine" : DEFAULT_ROW_LAYOUT;
}

/** 1-based, because that is what the slider's `aria-valuenow` says
 *  (`aria-valuemin=1`, `aria-valuemax=5`). A 0-based index would put the
 *  smallest step at the minimum in the DOM and at "one below the minimum" in
 *  the announcement, which is the kind of off-by-one only a screen-reader user
 *  ever hears. */
export function stepFromTextSize(size: TextSize): number {
  return TEXT_SIZES.indexOf(size) + 1;
}

/** The inverse, CLAMPED rather than wrapped: pressing → at the largest step
 *  must stay at the largest step. A wrap would take the biggest text to the
 *  smallest on one keypress. */
export function textSizeFromStep(step: number): TextSize {
  if (!Number.isFinite(step)) return DEFAULT_TEXT_SIZE;
  const i = Math.min(TEXT_SIZES.length, Math.max(1, Math.round(step))) - 1;
  return TEXT_SIZES[i] as TextSize;
}

export function loadTextSize(): TextSize {
  try {
    return textSizeFromServer(localStorage.getItem(TEXT_SIZE_KEY) ?? "");
  } catch {
    return DEFAULT_TEXT_SIZE;
  }
}

export function loadRowLayout(): RowLayout {
  try {
    return rowLayoutFromServer(localStorage.getItem(ROW_LAYOUT_KEY) ?? "");
  } catch {
    return DEFAULT_ROW_LAYOUT;
  }
}

/** Applied exactly as density is: an attribute on `<html>`, set from main.tsx
 *  BEFORE the first render, so the app never paints at one size and then jumps
 *  to another. */
export function applyTextSize(size: TextSize): void {
  document.documentElement.dataset["textSize"] = size;
  try {
    localStorage.setItem(TEXT_SIZE_KEY, size);
  } catch {
    /* a browser with storage disabled loses the memory, never the app */
  }
}

export function applyRowLayout(layout: RowLayout): void {
  document.documentElement.dataset["rowLayout"] = layout;
  try {
    localStorage.setItem(ROW_LAYOUT_KEY, layout);
  } catch {
    /* see applyTextSize */
  }
}

export function loadPaneLayout(): PaneLayout {
  const v = localStorage.getItem(PANE_LAYOUT_KEY);
  return v === "vertical" || v === "horizontal" || v === "off" ? v : "vertical";
}

/** The split size for the CURRENT layout. Vertical and horizontal are stored
 *  separately: a width that reads well as a list column is a poor height, so
 *  switching orientation must not carry the other one's number across. */
export function loadPaneSplit(layout: PaneLayout): number {
  const raw = localStorage.getItem(`${PANE_SPLIT_KEY}.${layout}`);
  const n = raw === null ? NaN : Number(raw);
  return Number.isFinite(n) ? clampSplit(layout, n) : splitBounds(layout).def;
}

export function applyPaneLayout(layout: PaneLayout): void {
  document.documentElement.dataset["paneLayout"] = layout;
  localStorage.setItem(PANE_LAYOUT_KEY, layout);
  // Re-assert this layout's own size, so a switch never inherits the other
  // orientation's number for a frame.
  applyPaneSplit(layout, loadPaneSplit(layout));
}

export function applyPaneSplit(layout: PaneLayout, px: number): number {
  const clamped = clampSplit(layout, px);
  document.documentElement.style.setProperty("--pane-split", `${clamped}px`);
  localStorage.setItem(`${PANE_SPLIT_KEY}.${layout}`, String(clamped));
  return clamped;
}

/** The orientation to go back to when the reading pane returns from "focused"
 *  (`messageOpens: "beside"`). Vertical is the default a device that has never
 *  had a horizontal pane falls back to — the same default loadPaneLayout has. */
export function loadBesideLayout(): "vertical" | "horizontal" {
  const v = localStorage.getItem(PANE_BESIDE_KEY);
  return v === "horizontal" ? "horizontal" : "vertical";
}

/** Records an orientation as the one to restore. "off" is deliberately NOT
 *  recorded: it is the state we are remembering our way OUT of. */
export function saveBesideLayout(layout: PaneLayout): void {
  if (layout === "off") return;
  localStorage.setItem(PANE_BESIDE_KEY, layout);
}

/** Re-applies the theme when the OS flips, but only while "system" is selected. */
export function watchSystemTheme(getTheme: () => Theme): () => void {
  const mq = window.matchMedia("(prefers-color-scheme: dark)");
  const onChange = (): void => {
    if (getTheme() === "system") applyTheme("system");
  };
  mq.addEventListener("change", onChange);
  return () => mq.removeEventListener("change", onChange);
}

/** Whether the desk's in-tray strip is expanded (#375).
 *
 * A device preference, like theme and density, and stored the same way: how
 * much room the tray deserves depends on the screen you are looking at, not on
 * the account. It defaults to CLOSED — the strip states the backlog in one line
 * and the desk below it is what the screen is for.
 */
export function loadTrayOpen(): boolean {
  try {
    return localStorage.getItem(TRAY_OPEN_KEY) === "1";
  } catch {
    // A browser with storage disabled loses the memory, never the app.
    return false;
  }
}

export function saveTrayOpen(open: boolean): void {
  try {
    localStorage.setItem(TRAY_OPEN_KEY, open ? "1" : "0");
  } catch {
    /* see loadTrayOpen */
  }
}

/**
 * The desk sections folded to their headings (#375 increment 7).
 *
 * A DEVICE PREFERENCE, like the tray strip above and for a stronger version of
 * the same reason: folding is how the phone fits a dozen areas on a screen four
 * inches tall, and syncing that to the desktop would fold sections on a monitor
 * that had room for all of them. There is no server field for it and there
 * should not be.
 *
 * ⚠ THE ENCODING IS DELIBERATELY NOT JSON. The members are ULIDs (ADR-007:
 * Crockford base32, so no commas, no braces, no quotes) plus the literal
 * `unsorted`, which makes a comma-separated list both sufficient and immune to
 * the one failure JSON has here — a half-written or hand-edited value throwing
 * inside a render. Sorted on the way out so the same set is always the same
 * string, which is what makes the round trip testable at all.
 */
export function encodeCollapsed(ids: ReadonlySet<string>): string {
  return [...ids].filter((id) => id !== "").sort().join(",");
}

export function decodeCollapsed(raw: string | null): Set<string> {
  if (!raw) return new Set();
  return new Set(raw.split(",").filter((id) => id !== ""));
}

export function loadCollapsedSections(): Set<string> {
  try {
    return decodeCollapsed(localStorage.getItem(COLLAPSED_KEY));
  } catch {
    // A browser with storage disabled loses the memory, never the app — and an
    // empty set is the safe direction to fail in: nothing hidden.
    return new Set();
  }
}

export function saveCollapsedSections(ids: ReadonlySet<string>): void {
  try {
    localStorage.setItem(COLLAPSED_KEY, encodeCollapsed(ids));
  } catch {
    /* see loadCollapsedSections */
  }
}
