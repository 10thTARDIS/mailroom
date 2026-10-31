// SPDX-License-Identifier: AGPL-3.0-only

// THE WIZARD'S STATE (#446): one pure module, one reducer, no React and no
// `window`.
//
// WHY IT IS NOT FOUR COMPONENTS' WORTH OF useState. Day-one's four steps are not
// four screens that happen to follow each other — they are one decision assembled
// in four passes, and every step reads what an earlier one chose:
//
//   step 1  the feed decision, and per folder its action, label and area
//   step 2  the window, per-folder overrides, and the run those produce
//   step 3  rules induced from the folders step 1 named and step 2 kept
//   step 4  the line and the drawers, described in terms of what step 2 wrote
//
// Holding that in component state would mean either lifting it all to the route
// anyway or losing it on every Back. So it lives here, it is persisted per login
// (setupStorage.ts) so each step is genuinely RESUMABLE, and — the part that
// earns the module — every derivation from it is a pure function with a test:
// `defaultFolderChoice` (what the wizard proposes before anybody touches it) and
// `planFor` (the wire plan) are the two places a wrong default would quietly
// label four thousand messages.
//
// ⚠ THE STATE IS NOT THE PLAN. The plan (`SetupPlan`) is derived from the state
// AGAINST THE CENSUS, every time, by `planFor`. Keeping a plan in state would let
// it name a folder the census no longer has — a folder deleted on the provider
// between step 1 and step 2 is refused whole by `Setup/preview`
// (`invalidArguments`, "not a folder day-one setup can act on"), which would
// present as the preview mysteriously failing rather than as one row going away.

import type {
  FolderCensus,
  Id,
  SetupAction,
  SetupFolderChoice,
  SetupPlan,
} from "../../../core/jmap/types";
import type { KeyValueStorage } from "../../../core/storage";
import { setupStateKey } from "./setupStorage";

/** The window day-one defaults to, and the four the wizard offers.
 *
 * The DEFAULT is the server's own (internal/api/setup.go setupDefaultWindowDays),
 * repeated here because a client rendering radios has to name the values it
 * offers. The SERVER accepts any positive number — the closed set is this
 * wizard's presentation choice, not a rule. */
export const SETUP_WINDOW_DEFAULT = 30;
export const SETUP_WINDOWS: readonly number[] = [7, 14, 30, 90];

/** The drawer periods step 4 offers, plus "never". `null` IS never — and it is
 * `drawerSweepEnabled: false` with the days left alone, never a null period,
 * which has never meant off (jmap/types.ts Area.autoArchiveDays). */
export const SETUP_DRAWER_DAYS: readonly number[] = [14, 30, 90];

/** The drawer period a fresh login already has (`db/schema.sql`,
 * `drawer_default_days int NOT NULL DEFAULT 30`), so pressing nothing changes
 * nothing here either. */
export const SETUP_DRAWER_DEFAULT = 30;

/** The four lines step 4 offers. The default is the line a fresh login already
 * has (`db/schema.sql`, `line_threshold real NOT NULL DEFAULT 0.5`), so pressing
 * nothing changes nothing. */
export const SETUP_LINES: readonly number[] = [0.4, 0.5, 0.6, 0.7];
export const SETUP_LINE_DEFAULT = 0.5;

/** The tray nag default (`db/schema.sql`, `tray_nag_days int NOT NULL DEFAULT 7`). */
export const SETUP_NAG_DEFAULT = 7;

/**
 * The one big decision of step 1.
 *
 * `folders` — mail arriving in the plain folders counts as unactioned and lands
 * in its area, exactly like INBOX. The provider's rules keep running and nothing
 * changes for other clients. This is what the step recommends.
 *
 * `mailroom` — new mail should arrive in INBOX and Mailroom places it. The user
 * turns the provider's rules off THEMSELVES (Mailroom cannot edit them), which is
 * why this writes nothing at all: a client that flipped `feedsDesk` off here
 * would be claiming to have done the half it cannot do.
 */
export type FeedDecision = "folders" | "mailroom";

/** Where a folder's label points on the desk. Three shapes rather than a
 * nullable id, because "create an area called Receipts" and "feed the existing
 * Receipts area" are different wire fields (`areaName` / `areaId`) and different
 * consequences: the first mints an area whose OWN label is this folder's, the
 * second adds a feed to an arrangement the user already has. */
export type AreaChoice =
  | { kind: "none" }
  | { kind: "existing"; areaId: Id }
  | { kind: "new"; name: string };

/** One folder's line of the wizard, as the user has it. */
export interface FolderChoice {
  action: SetupAction;
  /** The label this folder's older mail gets. Blank is legal on the wire (the
   * server defaults it to the folder name) but the wizard always carries the
   * name, because the field is an editable text input the user can read. */
  labelName: string;
  area: AreaChoice;
}

export interface SetupState {
  windowDays: number;
  feed: FeedDecision;
  /** Per mailbox id. A folder with no entry has never been touched and uses
   * `defaultFolderChoice`; the map is the OVERRIDES, so a census that gains a
   * folder proposes for it rather than leaving it out. */
  folders: Record<Id, FolderChoice>;
  /** Accepted rule candidates, by `candidateKey` (ui/setup/rules.ts). Ids rather
   * than the candidates themselves: the candidates come back from `Rule/induce`
   * on every visit to step 3, and remembering a stale copy of one would let the
   * step show numbers measured against a mailbox that has since changed. */
  acceptedRules: string[];
  /** The line step 4 has selected. */
  line: number;
  /** The drawer period step 4 has selected, or null for "never". */
  drawerDays: number | null;
  nagDays: number;
  /** The run step 2 started, so a reload lands back on its progress panel rather
   * than on a button that would start a second run (and be refused). */
  runId: Id | null;
}

export function initialSetupState(): SetupState {
  return {
    windowDays: SETUP_WINDOW_DEFAULT,
    // RECOMMENDED TO START, and the default because it is the only one of the two
    // that is true the moment it is chosen: the folders already have the mail in
    // them.
    feed: "folders",
    folders: {},
    acceptedRules: [],
    line: SETUP_LINE_DEFAULT,
    drawerDays: SETUP_DRAWER_DEFAULT,
    nagDays: SETUP_NAG_DEFAULT,
    runId: null,
  };
}

/**
 * What the wizard PROPOSES for one folder before anybody touches it.
 *
 * `labelArchive` for everything, INBOX included: the mail gets its label and
 * leaves the desk, and NOTHING moves on the server. `bringToDesk` is never a
 * default — it is the one action with a provider-visible consequence, and a
 * default that moved mail would be exactly the accident this wizard exists to
 * prevent.
 *
 * The AREA is the folder's own name, either as an existing area to feed (when the
 * census found one, so two areas called Receipts cannot appear) or as a new area
 * to mint. The LABEL is the folder's name, because the user already chose that
 * word.
 *
 * INBOX gets NEITHER: its mail is labelled with each message's own resolved area
 * label, per message, and the server ignores `labelName` and mints nothing for it
 * (internal/store/setup.go resolveSetupPlan). Proposing an area called "INBOX"
 * would be this client inventing a decision the server does not offer.
 */
export function defaultFolderChoice(folder: FolderCensus): FolderChoice {
  if (folder.role === "inbox") {
    return { action: "labelArchive", labelName: "", area: { kind: "none" } };
  }
  return {
    action: "labelArchive",
    labelName: folder.name,
    area: folder.existingAreaId
      ? { kind: "existing", areaId: folder.existingAreaId }
      : { kind: "new", name: folder.name },
  };
}

/** One folder's line as it stands: the override if there is one, else the
 * proposal. The single reader, so no surface can accidentally render a default
 * different from the one `planFor` sends. */
export function choiceFor(state: SetupState, folder: FolderCensus): FolderChoice {
  return state.folders[folder.mailboxId] ?? defaultFolderChoice(folder);
}

/**
 * THE WIRE PLAN, derived from the state against the census.
 *
 * Only census folders appear, and every one of them does — including the ones
 * left alone, spelled out as `leave` rather than omitted. Omission means the same
 * thing to the server, but saying it costs nothing and makes `Setup/preview`'s
 * `perFolder` cover every row the wizard is showing: a folder that vanished from
 * the preview because the client left it out reads exactly like a folder the
 * server declined to count (#176).
 *
 * `restrictTo` is import mode: one folder, chosen in Settings, and the rest of
 * the mailbox untouched — so the plan carries that folder alone rather than
 * `leave` lines for everything else, because a `leave` line still reports its
 * `older` count and the import panel's sentence would then be about the whole
 * mailbox.
 */
export function planFor(
  state: SetupState,
  census: readonly FolderCensus[],
  restrictTo?: Id,
): SetupPlan {
  const folders: SetupFolderChoice[] = [];
  for (const f of census) {
    if (restrictTo !== undefined && f.mailboxId !== restrictTo) continue;
    const choice = choiceFor(state, f);
    const line: SetupFolderChoice = { mailboxId: f.mailboxId, action: choice.action };
    // A `leave` folder is not labelled and joins no area, so sending either would
    // be describing something the server will not do.
    if (choice.action !== "leave" && f.role !== "inbox") {
      line.labelName = choice.labelName.trim() === "" ? f.name : choice.labelName.trim();
      if (choice.area.kind === "existing") line.areaId = choice.area.areaId;
      if (choice.area.kind === "new" && choice.area.name.trim() !== "") {
        line.areaName = choice.area.name.trim();
      }
    }
    folders.push(line);
  }
  return { windowDays: state.windowDays, folders };
}

/** The folders "Folders feed the desk" points at the desk: the plain ones the
 * plan does not leave alone. INBOX is excluded because it already feeds the desk
 * by definition, and a `leave` folder is excluded because the user said "later" —
 * pointing it at the desk would put its whole backlog on the surface, which is
 * the opposite of leaving it alone. */
export function foldersToFeed(state: SetupState, census: readonly FolderCensus[]): Id[] {
  return census
    .filter((f) => f.role !== "inbox" && choiceFor(state, f).action !== "leave")
    .map((f) => f.mailboxId);
}

// ---------------------------------------------------------------------------
// The reducer
// ---------------------------------------------------------------------------

export type SetupEdit =
  | { type: "window"; days: number }
  | { type: "feed"; feed: FeedDecision }
  | { type: "folderAction"; mailboxId: Id; action: SetupAction; folder: FolderCensus }
  | { type: "folderLabel"; mailboxId: Id; labelName: string; folder: FolderCensus }
  | { type: "folderArea"; mailboxId: Id; area: AreaChoice; folder: FolderCensus }
  | { type: "acceptRule"; key: string }
  | { type: "line"; line: number }
  | { type: "drawerDays"; days: number | null }
  | { type: "nagDays"; days: number }
  | { type: "run"; runId: Id | null };

/**
 * Every change the wizard can make, in one place.
 *
 * ⚠ A PER-FOLDER EDIT CARRIES ITS CENSUS ROW, and that is not redundancy. The
 * override map starts empty, so the first edit to a folder has to write the OTHER
 * two fields as well — and the only correct values for them are the proposal
 * (`defaultFolderChoice`), which needs the row. Without it, changing a folder's
 * action would blank its label, and the plan would then send `labelName: ""` and
 * silently fall back to the folder name — right by luck today, wrong the moment
 * the user has edited the label and then changed the action.
 */
export function setupReducer(state: SetupState, action: SetupEdit): SetupState {
  switch (action.type) {
    case "window":
      return { ...state, windowDays: action.days };
    case "feed":
      return { ...state, feed: action.feed };
    case "folderAction":
      return withFolder(state, action.folder, (c) => ({ ...c, action: action.action }));
    case "folderLabel":
      return withFolder(state, action.folder, (c) => ({ ...c, labelName: action.labelName }));
    case "folderArea":
      return withFolder(state, action.folder, (c) => ({ ...c, area: action.area }));
    case "acceptRule":
      // Set semantics: accepting the same candidate twice must not create a
      // second rule, and the step renders "Accepted" off this list.
      return state.acceptedRules.includes(action.key)
        ? state
        : { ...state, acceptedRules: [...state.acceptedRules, action.key] };
    case "line":
      return { ...state, line: action.line };
    case "drawerDays":
      return { ...state, drawerDays: action.days };
    case "nagDays":
      return { ...state, nagDays: action.days };
    case "run":
      return { ...state, runId: action.runId };
  }
}

function withFolder(
  state: SetupState,
  folder: FolderCensus,
  edit: (c: FolderChoice) => FolderChoice,
): SetupState {
  const current = state.folders[folder.mailboxId] ?? defaultFolderChoice(folder);
  return { ...state, folders: { ...state.folders, [folder.mailboxId]: edit(current) } };
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

/**
 * Reads the stored state back, VALIDATING every field rather than trusting it.
 *
 * localStorage is editable by hand and survives an upgrade, so a stored blob can
 * legitimately be from a different version of this wizard. Anything unrecognised
 * falls back to the initial value for THAT FIELD rather than throwing the whole
 * memory away: a hand-mangled `line` should not cost the user the folder plan
 * they spent two minutes building. The same reasoning as app/routing.ts's
 * `loadPosition`, one step further because there is more to lose.
 */
export function parseSetupState(raw: string | null): SetupState {
  const base = initialSetupState();
  if (!raw) return base;
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return base;
  }
  if (typeof value !== "object" || value === null) return base;
  const v = value as Record<string, unknown>;
  return {
    windowDays: positive(v["windowDays"]) ?? base.windowDays,
    feed: v["feed"] === "mailroom" ? "mailroom" : "folders",
    folders: parseFolders(v["folders"]),
    acceptedRules: Array.isArray(v["acceptedRules"])
      ? v["acceptedRules"].filter((k): k is string => typeof k === "string")
      : [],
    line: inRange(v["line"], 0, 1) ?? base.line,
    // null is a VALUE here ("never"), so it is distinguished from a missing field.
    drawerDays: v["drawerDays"] === null ? null : positive(v["drawerDays"]) ?? base.drawerDays,
    nagDays: positive(v["nagDays"]) ?? base.nagDays,
    runId: typeof v["runId"] === "string" ? v["runId"] : null,
  };
}

function positive(v: unknown): number | null {
  return typeof v === "number" && Number.isFinite(v) && v > 0 ? Math.floor(v) : null;
}

function inRange(v: unknown, lo: number, hi: number): number | null {
  return typeof v === "number" && Number.isFinite(v) && v >= lo && v <= hi ? v : null;
}

function parseFolders(v: unknown): Record<Id, FolderChoice> {
  const out: Record<Id, FolderChoice> = {};
  if (typeof v !== "object" || v === null) return out;
  for (const [id, entry] of Object.entries(v as Record<string, unknown>)) {
    if (typeof entry !== "object" || entry === null) continue;
    const e = entry as Record<string, unknown>;
    const action = e["action"];
    if (action !== "labelArchive" && action !== "bringToDesk" && action !== "leave") continue;
    out[id] = {
      action,
      labelName: typeof e["labelName"] === "string" ? e["labelName"] : "",
      area: parseArea(e["area"]),
    };
  }
  return out;
}

function parseArea(v: unknown): AreaChoice {
  if (typeof v !== "object" || v === null) return { kind: "none" };
  const a = v as Record<string, unknown>;
  if (a["kind"] === "existing" && typeof a["areaId"] === "string") {
    return { kind: "existing", areaId: a["areaId"] };
  }
  if (a["kind"] === "new" && typeof a["name"] === "string") {
    return { kind: "new", name: a["name"] };
  }
  return { kind: "none" };
}

export function loadSetupState(storage: KeyValueStorage, loginId: string): SetupState {
  return parseSetupState(storage.getItem(setupStateKey(loginId)));
}

export function saveSetupState(
  storage: KeyValueStorage,
  loginId: string,
  state: SetupState,
): void {
  storage.setItem(setupStateKey(loginId), JSON.stringify(state));
}

/** Forgets the draft — on "Open the desk" and on "Skip setup", because both are
 * the user saying they are done. Leaving it behind would resume a plan against a
 * mailbox day-one has already reorganised. */
export function clearSetupState(storage: KeyValueStorage, loginId: string): void {
  storage.removeItem(setupStateKey(loginId));
}
