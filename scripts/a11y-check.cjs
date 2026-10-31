// SPDX-License-Identifier: AGPL-3.0-only
//
// THE ACCESSIBILITY PROOF, INSIDE A REAL BROWSER (Forgejo #447, spec §12).
// Driven by scripts/a11y-e2e.sh, which stands up the stack this drives; run
// inside the pinned Chromium+puppeteer image (scripts/e2e-chrome-image.env).
//
// WHY IT EXISTS. web/ACCESSIBILITY.md records a model — the desk's list is a
// `role="grid"` whose current row is named by `aria-activedescendant`, a row's
// name is an assembled SENTENCE, the line is a `role="slider"` whose value text
// states the split, there is exactly ONE live region, a drawer's button says how
// much is in it. Every one of those is a claim about what a screen reader is
// GIVEN, and a linter cannot check any of them: axe can tell you a button has an
// accessible name, not that the name is the right sentence, not that the cursor
// moved, and not that anything was announced when the header changed. So this
// does both halves — axe over every surface in both themes, AND the browser's own
// accessibility tree read after each step of the desk's core loop.
//
// WHAT IT PROVES
//   * axe-core (pinned, exact) reports ZERO WCAG 2 A/AA violations on FIFTEEN
//     surfaces (the desk, its drawer, the tray, all mail, settings, its open
//     section, the composer, the composer reached by a RELOAD (#612), the reading pane, the sender popover, the
//     attachment viewer, focused reading, the day-one setup wizard, `#/setup`
//     — mailroom#447 follow-up — and, since mailroom#514, the desk in the
//     MULTI-LINE row layout and the desk at the LARGEST text size), in light,
//     dark and system-dark, and each run prints its `passes`/`incomplete`
//     denominators so an empty result cannot be mistaken for a clean one.
//
//     ⚠ THE TWO #514 SURFACES ARE NOT DECORATION. That issue's claim is that
//     the two row layouts share ONE DOM and one set of roles and names — only
//     CSS moves — and that the whole app is legible at every text size. Both of
//     those are exactly the kind of claim that is true when it is written and
//     quietly false three commits later, and neither is visible in a diff. So
//     the desk is linted in the layout and at the size a user can actually
//     choose, chosen THROUGH THE SETTINGS CONTROLS the way the theme already
//     is, and put back afterwards so the announcement script still runs on the
//     default.
//   * `color-contrast-enhanced` (AAA, 7:1) is measured separately on the same
//     surfaces; a failure is a FAILURE unless scripts/a11y-contrast.allow names
//     the element and says why.
//   * eight steps of the desk's loop — open, move, archive + the undo toast,
//     flag, cross the line, the line's slider, a drawer, and a fault replacing
//     the header sentence — each assert the computed ROLE, NAME, VALUE and STATES
//     of the active row / focused node, plus the text of the live region. Those
//     four things are the INPUTS to any announcement.
//   * three negative controls, in the same run, each of which must make one of
//     those checks go red. A green run therefore also says the checks can fail.
//
// ⚠ WHAT IS OUT OF SCOPE, IN WORDS, so the claim is exactly as big as the check:
//   * NO SCREEN READER RUNS HERE. No CI runner in this project is Windows, so
//     NVDA is not available and JAWS is not licensable in CI; VoiceOver has no
//     automation surface. What is read is CHROMIUM'S OWN ACCESSIBILITY TREE —
//     the thing a screen reader on this platform consumes — through puppeteer's
//     `page.accessibility.snapshot` and CDP `Accessibility.getPartialAXTree`.
//   * THEREFORE NOTHING HERE IS EVIDENCE ABOUT PHRASING OR VERBOSITY. A given
//     AT's wording, its punctuation settings, whether it reads a comma as a
//     pause, and how much of a row it speaks at default verbosity are all its
//     own. This asserts the STRING and the STATES it is handed.
//   * IT CANNOT SEE "ANNOUNCED ONCE". A polite live region's text is observable;
//     how many times an AT spoke it is not. Where increment A's model turns on
//     that (the `role="timer"` NOT re-announcing its container), the check is the
//     structural one — the timer's own value holds while focused — not the
//     acoustic one.
//   * `aria-current` IS ASSERTED FROM THE DOM, not the AX tree: CDP's
//     `AXPropertyName` has no `current`, so Chromium does not expose it as a
//     computed property at all. That is a limit of the observation, and it is
//     said here rather than dressed up.
//   * THE MESSAGE BODY IS NOT LINTED. It renders in a sandboxed, opaque-origin
//     srcdoc iframe (ui/messageDocument.ts), which axe cannot enter and must not
//     — that document is a stranger's HTML and scripts/hostile-content-e2e.sh is
//     what governs it. axe runs with `iframes: false` and the bound is printed.
//   * ONE SURFACE IS ABSENT FOR A FIXTURE REASON, not an oversight: the attachment
//     viewer's HELD panel needs a QUARANTINED part, which needs the scanner and a
//     real detection (scripts/clamav-test.sh's territory). It is named as a bound
//     on the final line rather than silently skipped.
//
// EXIT CODES (the shell maps them; one per failure kind, like hostile-content):
//     0  CLEAN
//     2  ERR <message>          — anything thrown; fail closed
//     6  AXE <rule> on <surface>/<theme>
//     7  NONAME                 — a node that must have a name has none, or the
//                                 wrong one
//     8  NOLIVE                 — the one live region is not a live region
//     9  NOACTIVE               — the grid names no current row, or the wrong one
//    10  CONTROLPASSED <which>  — a negative control did not go red
//    11  NOSTEP <step>          — an announcement step's own assertion failed
//    12  CONTRAST <selector>    — an AAA contrast failure the allowlist does not
//                                 name
//    13  NOTHEME <theme>        — the theme did not take, so the pass that
//                                 follows would be linting the wrong palette
//    14  NOFIXTURE <what>       — the browser cannot see the desk the fixture
//                                 built (an assertion against an empty desk is
//                                 the #131 shape)
//    15  UNTOUCHED <state>      — a composer nobody typed in was saved, or one
//                                 that WAS typed in was not (mailroom#614)
//    16  SPLIT <what>           — with a composer or viewer docked, the synced
//                                 split was measured wrong or cannot be dragged
//                                 (mailroom#617)
//    17  SCROLLPULL <what>      — scrolling the cursor row out of view was
//                                 undone without the cursor moving, or the
//                                 cursor moved and was not followed
//    18  PANECLOSE <what>       — after opening several messages, Escape or the
//                                 pane's close button did not return to the list
//    19  MOVEMENU <what>        — the classic list's "Move to area" menu (#708):
//                                 name, menu roles, or Escape returning focus
//    20  MOVESENDER <what>      — the sender card's "Move all from … to…" confirm
//                                 step (#708): its buttons by role and name
//    21  SEARCHBOX <what>       — the search box (#724): the suggestion
//                                 combobox's role/expanded/activedescendant/
//                                 Escape, or the options form's names, focus
//                                 in and out, or fit at 360px
//    22  NOTIFY <what>          — Settings › Notifications and › Senders
//                                 (#59): a control whose computed name does not
//                                 contain its visible label, a group with no
//                                 name, or the in-run negative control staying
//                                 green
//    23  CLEANUP <what>         — Settings › Clean up (#53): the controls'
//                                 names, a row's action group without the
//                                 row's name, or the in-run negative control
//                                 staying green
//    24  INVITE <what>          — the calendar invitation card (#725): its
//                                 region or group name, the three answers'
//                                 names, an aria-pressed that does not match
//                                 the server's recorded answer (before, after
//                                 Accept, after its Undo), or the in-run
//                                 negative control staying green
//    25  FOLLOWUP <what>        — the desk's Follow up strip (#58): no strip
//                                 for the fixture's due nudge, a region or a
//                                 row's action group without its name, the
//                                 in-run negative control staying green, or a
//                                 Done that did not stay done
//    26  OOF <what>             — Settings › Out of office (#50): a control
//                                 whose computed name does not contain its
//                                 visible label, an account's form not named
//                                 by its account, no status line, or the in-run
//                                 negative control staying green
//    27  FOLDED <what>          — a folded message in the reading pane (#759):
//                                 no folded button where the fixture made one,
//                                 a computed name that does not contain the
//                                 text the button shows (WCAG 2.5.3), or the
//                                 in-run negative control staying green
//    28  PHONEWIDTH <view>      — at 375 or 390 px (mobile, touch, 3x) a main
//                                 view is wider than the phone: the document
//                                 overflows, an element lies outside the
//                                 viewport, something scrolls sideways, the
//                                 phone's action row truncates a label, or
//                                 the reading view does not scroll the
//                                 document (#798); also at 320 px, and (#801)
//                                 a newsletter wider than the phone that still
//                                 scrolls inside its frame instead of being
//                                 scaled to fit, (#802) fewer than 12
//                                 characters of room in the search box, and
//                                 (#803) at 1440x900 the reading pane or its
//                                 action row wider than the pane, or Reply all
//                                 left on a row too narrow for it. Every offender is printed with its
//                                 selector path, box and computed width,
//                                 min-width, flex and white-space
//    29  KEYS <what>            — Settings › Keys and its revoke dialog (#796):
//                                 a control not named by its visible label,
//                                 focus leaving the revoke dialog, its confirm
//                                 button not announced as unavailable (and
//                                 why) until the fingerprint's tail matches,
//                                 the desk's key-expiry banner (#795) missing
//                                 or not staying dismissed, or either in-run
//                                 negative control (trap cut, unavailable
//                                 state dropped) staying green; and (v1.93
//                                 C2) a History section that is not a
//                                 button exposing its expanded state through
//                                 Enter and Space, a date input not named or
//                                 described, the replace dialog not a named
//                                 modal holding focus with its confirm
//                                 unavailable-and-why until a date is chosen,
//                                 or one of those three in-run controls
//                                 staying green
//    30  CONTACTS <what>        — the contacts page (#816): the list's search
//                                 not named, the filters not a named group of
//                                 toggles exposing which is pressed, Blocked
//                                 senders not reachable by keyboard, a
//                                 contact's notes not named and described by
//                                 their count, VIP not a toggle, the merge
//                                 dialog not a named modal holding focus, its
//                                 search not a combobox naming the active
//                                 option or not choosable by keyboard, Escape
//                                 not returning to its opener, or one of the
//                                 three in-run controls staying green
//    31  APIKEYS <what>         — Settings › API keys (#49): the fixture's
//                                 held send and delete not waiting, the send
//                                 card's To/Cc/Bcc not the request's own
//                                 fields or its subject not an isolated
//                                 <bdi dir=auto> without the override, the
//                                 approve, create or token dialog not a named
//                                 modal holding focus, Create not announced
//                                 unavailable-and-why before the phrase,
//                                 Escape closing the token dialog, the banner
//                                 not a polite live region, or the cleanup
//                                 unable to deny the requests
//    32  PHONEMENUS <menu>      — at 320 or 390 px wide, portrait or rotated, a
//                                 pop-up menu (the desk's Re-file with two
//                                 rows selected, the list's Move to area, the
//                                 reading pane's More, the sender card's Move
//                                 all, the search suggestions and options, the
//                                 recipient suggestions) is not wholly on the
//                                 screen, is cut off by a clipping ancestor,
//                                 does not scroll inside itself when taller
//                                 than its room, has a last item that cannot
//                                 be reached, or is covered; or an in-run
//                                 negative control stayed green (#880)
//    33  OAUTH <what>           — mail-provider sign-in (#52): the Connect
//                                 buttons not a named group of buttons named
//                                 by their words, the interstitial not a named
//                                 modal starting on Cancel, holding focus and
//                                 returning it on Escape, or not saying what
//                                 access means; the reconnect banner not a
//                                 polite region or not staying dismissed for
//                                 the session; an OAuth account offering a
//                                 server editor or no Sign-in group; a held
//                                 Gmail account offering a sync toggle; the
//                                 sign-in result not shown, shown twice, or
//                                 left in the URL; or one of the two in-run
//                                 controls staying green

"use strict";

const fs = require("fs");
const puppeteer = require("puppeteer");

const BASE = process.env["A11Y_BASE"] || "http://mailroom:8080";
const AXE_SOURCE = fs.readFileSync("/tmp/axe.min.js", "utf8");
const FIX = JSON.parse(fs.readFileSync("/tmp/fixture.json", "utf8"));

/** WCAG 2 A + AA, the level this project holds itself to as a gate. */
const AA_TAGS = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"];

/**
 * A named failure. Thrown, so every exit goes through one place and an
 * UNEXPECTED throw cannot be mistaken for an expected one (it lands in the
 * catch-all as `ERR`, exit 2).
 */
class Fail extends Error {
  constructor(code, line) {
    super(line);
    this.code = code;
  }
}

const tally = {
  surfaces: 0,
  axeRuns: 0,
  passes: 0,
  incomplete: 0,
  announcements: 0,
  controlsTripped: 0,
  controlsRun: 0,
  contrastAllowed: 0,
  dockedSplit: 0,
  scrollAway: 0,
  paneClose: 0,
  moveMenu: 0,
  moveSender: 0,
  searchBox: 0,
  heldChip: 0,
  notify: 0,
  cleanup: 0,
  invite: 0,
  followUp: 0,
  oof: 0,
  folded: 0,
  phoneWidth: 0,
  phoneMenus: 0,
  keys: 0,
  contacts: 0,
  apiKeys: 0,
  oauth: 0,
};
let axeVersion = "?";

// ---------------------------------------------------------------------------
// The contrast allowlist
// ---------------------------------------------------------------------------
//
// One line per exception: a CSS selector, a MINIMUM ratio, then the reason. A
// `color-contrast-enhanced` (AAA, 7:1) failure whose element matches one of these
// selectors is allowed — but only DOWN TO ITS OWN FLOOR, and the run fails if it
// slips below that.
//
// ⚠ THE NUMBER IS WHAT STOPS THIS BEING A MUTE. An allowlist of bare selectors is
// indistinguishable from `--disable-rules`: it would accept 7:1 and 2:1 equally,
// so a token that regressed inside the allowed range would pass forever. With a
// floor, each line is a checkable claim — "this element is held to AA, not AAA,
// because it is a LABEL rather than body text" — and the assertion is on the
// bottom of the range rather than on a point estimate nobody re-reads.
//
// Set `A11Y_CONTRAST_REPORT=1` to measure instead of gate: every failure is
// printed and grouped by class + colour pair at the end of the run, which is how
// this file was written in the first place (CLAUDE.md: build the report before the
// fix). CI never sets it, and a run with it set marks its own final line so a
// report can never be read as a pass.
function parseAllow(text) {
  const out = [];
  text.split(/\r?\n/).forEach((raw, i) => {
    const line = raw.trim();
    if (line === "" || line.startsWith("#")) return;
    const m = line.match(/^(\S+)\s+([0-9]+(?:\.[0-9]+)?)\s+(.+)$/);
    if (!m) {
      throw new Fail(
        12,
        "CONTRAST scripts/a11y-contrast.allow line " +
          (i + 1) +
          " is not `<selector> <min-ratio> <reason>`: " +
          line,
      );
    }
    out.push({ selector: m[1], min: Number(m[2]), reason: m[3] });
  });
  return out;
}
const ALLOW = parseAllow(fs.readFileSync("/tmp/a11y-contrast.allow", "utf8"));
const CONTRAST_REPORT = process.env["A11Y_CONTRAST_REPORT"] === "1";
const PREFS_SET_DELAY_MS = Number(process.env["A11Y_PREFS_SET_DELAY_MS"] || "0");
/** Every AAA failure seen in a reporting run, grouped by class + colour pair. */
const contrastReport = new Map();

// ---------------------------------------------------------------------------
// Small helpers over the page
// ---------------------------------------------------------------------------

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * `pageerror` and console `error` messages seen since the last top-level
 * navigation. Populated by the listeners `main()` registers and cleared on
 * `framenavigated`, so a diagnostic printed after a `load()`/`reload()` is
 * about THIS document, not one three surfaces back.
 */
let recentPageIssues = [];

/**
 * The four things that turn "selector not found" into "here is what was
 * actually on screen" (#447 follow-up, mailroom#447): where the browser
 * really is, what the document really says, and whether it already told us
 * why. A bare "Waiting for selector … 60000ms exceeded" is exactly what this
 * lane printed the day main() grew the day-one setup wizard: the fixture had
 * built the desk fine, but the client had navigated itself to `#/setup` and
 * `#desk-grid` was never going to appear. `page.url()` alone would have
 * settled that in the first log line instead of a round trip.
 */
async function pageDiagnostics(page) {
  try {
    const info = await page.evaluate(() => ({
      url: window.location.href,
      title: document.title,
      body: ((document.body && document.body.innerText) || "").slice(0, 300),
    }));
    return (
      "page.url()=" +
      info.url +
      " document.title=" +
      JSON.stringify(info.title) +
      " body[0:300]=" +
      JSON.stringify(info.body) +
      " pageerrors_since_nav=" +
      JSON.stringify(recentPageIssues)
    );
  } catch (e) {
    return "diagnostics unavailable: " + (e && e.message ? e.message : String(e));
  }
}

/**
 * `page.waitForSelector`, with `pageDiagnostics` attached to the thrown error
 * on failure. Every caller in this file goes through this instead of the bare
 * puppeteer method, so a timeout NAMES ITSELF rather than costing whoever
 * reads CI a reproduction cycle to find out where the page actually was.
 */
async function waitForSelectorD(page, selector, opts) {
  try {
    return await page.waitForSelector(selector, opts);
  } catch (e) {
    const diag = await pageDiagnostics(page);
    e.message = e.message + " -- " + diag;
    throw e;
  }
}

/**
 * Waits for `probe` to answer truthily, and FAILS LOUDLY on exhaustion with what
 * it last saw. The same rule scripts/*.sh's `wait_for` follows: a bare loop that
 * falls through leaves the failure to surface somewhere unrelated, three
 * assertions later, describing the wrong thing.
 */
// `describe`, when given, is asked ONCE on timeout for what WAS there instead
// (#562). "still false" alone is how the accessibility lane's one flake
// (`messageOpens beside`) arrived with nothing to reason from: the wait could
// have seen "off" (a stale synced preference re-applied over the click —
// Shell.tsx applyServerPrefs runs on every record that arrives), "horizontal"
// (the remembered orientation, which this harness does not expect), or no
// attribute at all, and each is a different defect.
async function until(label, ms, probe, code = 11, describe = null) {
  const started = Date.now();
  let last;
  for (;;) {
    last = await probe();
    if (last) return last;
    if (Date.now() - started >= ms) {
      let saw = "";
      if (describe) {
        try {
          saw = "; observed " + (await describe());
        } catch (e) {
          saw = "; observing failed: " + (e && e.message ? e.message : String(e));
        }
      }
      throw new Fail(
        code,
        "NOSTEP " + label + ": still false after " + (Date.now() - started) + "ms" + saw,
      );
    }
    await sleep(120);
  }
}

/**
 * Every ClientPreferences/set the page sent, and what became of it (#658).
 *
 * The pane-layout flake arrived with `checkedRadio: null`, which read as "the
 * click never landed" and was nothing of the kind: that wait runs on the DESK,
 * after a reload, where the Settings radios are not in the DOM at all. What the
 * line could not say was whether the WRITE behind the click ever reached the
 * server — and a write the next reload aborted is a server still holding the old
 * value, which "SERVER WINS AT LOAD" (Shell.tsx applyServerPrefs) then puts back
 * on screen. So the harness keeps the record itself, from the browser's own
 * network events, and every pane-layout timeout prints it.
 */
const prefsWrites = [];
function watchPrefsWrites(page) {
  const find = (req) => prefsWrites.find((w) => w.req === req);
  page.on("request", (req) => {
    const body = req.method() === "POST" ? req.postData() || "" : "";
    if (body.indexOf('"ClientPreferences/set"') < 0) return;
    let patch = "?";
    try {
      const calls = JSON.parse(body).methodCalls || [];
      const set = calls.find((c) => c[0] === "ClientPreferences/set");
      patch = JSON.stringify(Object.values((set && set[1].update) || {})[0] || {});
    } catch (e) {
      patch = "unparsed";
    }
    prefsWrites.push({ req, at: Date.now(), patch, outcome: "pending" });
    if (prefsWrites.length > 40) prefsWrites.shift();
  });
  page.on("requestfinished", (req) => {
    const w = find(req);
    if (w) w.outcome = "HTTP " + (req.response() ? req.response().status() : "?");
  });
  page.on("requestfailed", (req) => {
    const w = find(req);
    if (w) w.outcome = "FAILED " + ((req.failure() && req.failure().errorText) || "?");
  });
}
function recentPrefsWrites() {
  const now = Date.now();
  return prefsWrites.slice(-6).map((w) => ({
    agoMs: now - w.at,
    patch: w.patch,
    outcome: w.outcome,
  }));
}

/** What the pane-layout waits report on timeout: the attribute the app actually
 * set, what the device remembered, which page it is on, the Settings radios
 * (ABSENT is not the same as UNCHECKED), and the preference writes the page
 * sent and what became of each. */
function describePaneLayout(page) {
  return async () => {
    const dom = await page.evaluate(() => {
      const radios = document.querySelectorAll('input[name="messageOpens"]');
      const checked = document.querySelector('input[name="messageOpens"]:checked');
      return {
        hash: location.hash,
        dataPaneLayout: document.documentElement.dataset["paneLayout"] ?? null,
        storedLayout: localStorage.getItem("mailroom.paneLayout"),
        storedBeside: localStorage.getItem("mailroom.paneLayout.beside"),
        // "absent" — Settings is not rendered (the waits after a reload run on
        // the desk); null — the radios exist and none is checked.
        checkedRadio: radios.length === 0 ? "absent" : checked ? checked.value : null,
      };
    });
    return JSON.stringify({ ...dom, prefsWrites: recentPrefsWrites() });
  };
}

/** Navigate by HASH, the way the app's own links do — no document reload, so the
 * client-side route change is the one under test. Used inside a surface and all
 * through the announcement script, where the point is that the ROUTE moved. */
async function gotoHash(page, hash) {
  await page.evaluate((h) => {
    window.location.hash = h;
  }, hash);
}

/**
 * Start a surface from a COLD DOCUMENT at `hash`.
 *
 * ⚠ EACH SURFACE IS RELOADED, AND THAT IS NOT CAUTION — the first version of this
 * file navigated by hash between surfaces and the attachment viewer never opened,
 * because the surface before it leaves a PINNED SENDER POPOVER on screen and
 * `location.hash = <the hash it already is>` fires no hashchange, so nothing tore
 * it down. puppeteer then clicked the popover instead of the attachment. A lint
 * pass that inherits the previous surface's overlays is linting something nobody
 * can name, and the failure it produces points at the wrong surface.
 *
 * `location.hash` first and `reload()` second, rather than `page.goto(url#hash)`:
 * a navigation that differs only in the fragment is a SAME-DOCUMENT navigation
 * and does not reload, which is exactly the trap above wearing a different hat.
 */
async function load(page, hash) {
  await page.evaluate((h) => {
    window.location.hash = h;
  }, hash);
  await settled(page, "reload " + hash, () => page.reload({ waitUntil: "networkidle2", timeout: 60000 }));
}

/**
 * Run a navigation and REFUSE a Chromium error page as its result, retrying a
 * bounded number of times.
 *
 * ⚠ THIS IS NOT LENIENCY ABOUT THE APP; IT IS ABOUT THE RUNNER. On a main push
 * run (2026-09-09, #502's second data point) the fixture stage had just built the
 * whole desk against http://mailroom:8080 and the very next reload landed on
 * `chrome-error://chromewebdata/` with ERR_ADDRESS_UNREACHABLE while three lanes
 * shared the runner's Docker networking. An app that renders nothing still fails
 * here — a real page that lacks the grid is not an error page, and every wait
 * below still has its own timeout — but a transient unreachable address costs a
 * few seconds and a printed line, not a red required check. Bounded: after
 * NAV_TRIES the last error page is reported with the URL, exactly as before.
 */
const NAV_TRIES = 5;
const NAV_RETRY_MS = 3000;
async function settled(page, what, nav) {
  for (let attempt = 1; ; attempt += 1) {
    let navErr = null;
    try {
      await nav();
    } catch (e) {
      navErr = e;
    }
    const url = page.url();
    const errorPage = url.startsWith("chrome-error://");
    if (!navErr && !errorPage) return;
    if (attempt >= NAV_TRIES) {
      if (navErr) throw navErr;
      throw new Error(what + ": Chromium error page after " + attempt + " attempts, url=" + url);
    }
    console.log("retry " + what + " attempt=" + attempt + " " + (navErr ? "error=" + String(navErr.message).slice(0, 120) : "url=" + url));
    await new Promise((r) => setTimeout(r, NAV_RETRY_MS));
  }
}

/** The desk is loaded when its grid exists and claims rows. `aria-rowcount > 0`
 * rather than "the element is present": an empty grid renders perfectly and every
 * assertion below it would then be about nothing (#131). */
async function waitForDesk(page) {
  await waitForSelectorD(page, "#desk-grid[role=grid]", { timeout: 60000 });
  await until(
    "desk grid has rows",
    60000,
    () =>
      page.evaluate(() => {
        const g = document.querySelector("#desk-grid");
        return g ? Number(g.getAttribute("aria-rowcount") || "0") > 0 : false;
      }),
    14,
  );
}

/** The accessible node for one selector, via puppeteer's own snapshot: role,
 * name, value and the states it serialises (selected, expanded, focused,
 * valuetext, valuemin/max). */
async function axSnapshot(page, selector) {
  const handle = await page.$(selector);
  if (!handle) return null;
  const snap = await page.accessibility.snapshot({ root: handle, interestingOnly: false });
  await handle.dispose();
  return snap;
}

/**
 * The same node through CDP, which carries the computed properties the snapshot
 * does not — `live`, `atomic`, `relevant`, `activedescendant`. That is what makes
 * "this is a polite live region" an assertion about the ACCESSIBILITY TREE rather
 * than about two attributes in the DOM.
 */
async function axCDP(cdp, selector) {
  const doc = await cdp.send("DOM.getDocument", { depth: -1 });
  const found = await cdp.send("DOM.querySelector", {
    nodeId: doc.root.nodeId,
    selector,
  });
  if (!found.nodeId) return null;
  const described = await cdp.send("DOM.describeNode", { nodeId: found.nodeId });
  const backend = described.node.backendNodeId;
  const tree = await cdp.send("Accessibility.getPartialAXTree", {
    nodeId: found.nodeId,
    fetchRelatives: false,
  });
  const node =
    (tree.nodes || []).find((n) => n.backendDOMNodeId === backend) ||
    (tree.nodes || [])[(tree.nodes || []).length - 1];
  if (!node) return null;
  const props = {};
  for (const p of node.properties || []) props[p.name] = p.value && p.value.value;
  return {
    role: node.role && node.role.value,
    name: node.name && node.name.value,
    value: node.value && node.value.value,
    // The computed description (aria-describedby), which CDP carries beside
    // the properties rather than among them (#796's revoke button says WHY it
    // is unavailable through it).
    description: node.description && node.description.value,
    ignored: node.ignored === true,
    props,
  };
}

/**
 * Wait for the toasts container to say something containing `needle`, and on
 * exhaustion FAIL WITH WHAT IT ACTUALLY SAID. An error toast is the client
 * answering the question; a bare timeout throws that answer away.
 */
async function untilToast(page, label, needle) {
  const started = Date.now();
  for (;;) {
    const txt = await page.evaluate(() => {
      const t = document.querySelector("div.toasts");
      return t ? t.textContent.replace(/\s+/g, " ").trim() : "";
    });
    if (txt.indexOf(needle) >= 0) return txt;
    if (Date.now() - started >= 20000) {
      throw new Fail(
        11,
        "NOSTEP " +
          label +
          ": no toast containing " +
          JSON.stringify(needle) +
          " after " +
          (Date.now() - started) +
          "ms; the toasts container says " +
          JSON.stringify(txt),
      );
    }
    await sleep(120);
  }
}

/** One transcript line per announcement step. The transcript IS the deliverable:
 * a reader must be able to see the sentence a screen reader would have been
 * given, not merely that a boolean held. */
function announce(step, node, live) {
  tally.announcements += 1;
  console.log(
    "announce " +
      step +
      " role=" +
      (node && node.role ? node.role : "-") +
      ' name="' +
      ((node && node.name) || "") +
      '" live="' +
      (live || "") +
      '"',
  );
}

// ---------------------------------------------------------------------------
// axe
// ---------------------------------------------------------------------------

/**
 * Inject axe.
 *
 * ⚠ NOT `page.addScriptTag`. The app ships `script-src 'self' '<one hash>'` with
 * no `unsafe-inline` and no `unsafe-eval` (cmd/mailroom appCSP), so an injected
 * inline <script> is BLOCKED — and so is an in-page `eval` of the source. What is
 * not subject to page CSP is CDP's own `Runtime.evaluate`, which is what
 * `page.evaluate(<string>)` compiles to. So the linter arrives through the
 * debugger, and `page.setBypassCSP` is deliberately NOT used: the shipped policy
 * stays exactly as shipped while it is being linted.
 */
async function injectAxe(page) {
  await page.evaluate(AXE_SOURCE);
  const v = await page.evaluate(() => (window.axe ? window.axe.version : null));
  if (!v) throw new Fail(2, "ERR axe did not define window.axe after injection");
  axeVersion = v;
}

const AXE_SHARED = {
  // The message body is a stranger's HTML in a sandboxed, opaque-origin frame:
  // axe cannot enter it and hostile-content-e2e.sh is what governs it.
  iframes: false,
  // Every rule's result, so a run can print its denominators.
  resultTypes: ["violations", "passes", "incomplete"],
};

async function axeAA(page, surface, theme) {
  const res = await page.evaluate(
    (tags, shared) =>
      window.axe
        .run(document, Object.assign({}, shared, { runOnly: { type: "tag", values: tags } }))
        .then((r) => ({
          violations: r.violations.map((v) => ({
            id: v.id,
            count: v.nodes.length,
            help: v.help,
            targets: v.nodes.slice(0, 4).map((n) => String(n.target[0])),
          })),
          passes: r.passes.length,
          incomplete: r.incomplete.length,
        })),
    AA_TAGS,
    AXE_SHARED,
  );
  tally.axeRuns += 1;
  tally.passes += res.passes;
  tally.incomplete += res.incomplete;
  let total = 0;
  for (const v of res.violations) {
    total += v.count;
    console.log("axe " + surface + " " + theme + " " + v.id + " " + v.count);
    for (const t of v.targets) console.log("axe     ↳ " + t);
  }
  // ⚠ THE DENOMINATOR IS PRINTED ON EVERY RUN. `violations=0` with `passes=0` is
  // axe having examined nothing — a linter that did not run reads exactly like a
  // clean surface, which is the whole shape of #131.
  console.log(
    "axe " +
      surface +
      " " +
      theme +
      " violations=" +
      total +
      " passes=" +
      res.passes +
      " incomplete=" +
      res.incomplete,
  );
  if (res.passes === 0) {
    throw new Fail(
      6,
      "AXE no-rule-passed on " +
        surface +
        "/" +
        theme +
        " — axe reported zero passes, so `violations=0` is not evidence of anything",
    );
  }
  if (total > 0) {
    throw new Fail(
      6,
      "AXE " + res.violations.map((v) => v.id).join(",") + " on " + surface + "/" + theme,
    );
  }
}

/**
 * The AAA contrast pass, separately — and it HAS to be separate.
 * `color-contrast-enhanced` is tagged wcag2aaa/wcag146, so under a
 * `runOnly: {type:"tag", values:[…AA…]}` it is filtered out of the rule set
 * BEFORE `rules: {enabled:true}` is consulted: enabling it in the AA run would
 * have looked like it worked and measured nothing.
 */
async function axeContrastAAA(page, surface, theme) {
  const res = await page.evaluate(
    (shared, allow) =>
      window.axe
        .run(
          document,
          Object.assign({}, shared, {
            runOnly: { type: "rule", values: ["color-contrast-enhanced"] },
            // The node axe actually measured, not a re-query of its selector
            // afterwards. A surface that is still rendering (the sender popover
            // adds sections as its data arrives) can replace the element between
            // the run and the lookup; the selector then resolves to nothing and
            // an allowlisted element was reported as an unlisted failure.
            elementRef: true,
          }),
        )
        .then((r) => {
          const out = { passes: 0, incomplete: 0, nodes: [] };
          for (const p of r.passes) out.passes += p.nodes.length;
          for (const i of r.incomplete) out.incomplete += i.nodes.length;
          for (const v of r.violations) {
            for (const n of v.nodes) {
              const sel = String(n.target[0]);
              const data = (n.any && n.any[0] && n.any[0].data) || {};
              let el = n.element || null;
              if (!el) {
                try {
                  el = document.querySelector(sel);
                } catch (e) {
                  el = null;
                }
              }
              let hit = null;
              if (el) {
                for (const a of allow) {
                  try {
                    if (el.matches(a.selector)) {
                      hit = a;
                      break;
                    }
                  } catch (e) {
                    /* a malformed selector is caught by the parser, not here */
                  }
                }
              }
              out.nodes.push({
                selector: sel,
                // The GROUPING KEY for a reporting run: what this element IS,
                // rather than where it happens to sit in a virtualised list. The
                // nth-child paths axe emits are unusable as allowlist entries —
                // they name row 4 of section 3 on one particular desk.
                tag: el ? el.tagName.toLowerCase() : "?",
                classes: el ? String(el.className || "") : "",
                fg: data.fgColor || null,
                bg: data.bgColor || null,
                ratio: typeof data.contrastRatio === "number" ? data.contrastRatio : null,
                allowedBy: hit ? hit.selector : null,
                min: hit ? hit.min : null,
                reason: hit ? hit.reason : null,
              });
            }
          }
          return out;
        }),
    AXE_SHARED,
    ALLOW,
  );
  tally.axeRuns += 1;

  const denied = [];
  const belowFloor = [];
  for (const n of res.nodes) {
    if (CONTRAST_REPORT) {
      const key =
        n.tag + "." + n.classes.split(/\s+/).join(".") + "  " + n.fg + " on " + n.bg;
      const prev = contrastReport.get(key);
      const ratio = n.ratio === null ? 0 : n.ratio;
      if (!prev) {
        contrastReport.set(key, { min: ratio, count: 1, example: n.selector });
      } else {
        prev.count += 1;
        if (ratio < prev.min) prev.min = ratio;
      }
      continue;
    }
    if (n.allowedBy === null) {
      denied.push(n);
      continue;
    }
    if (n.ratio !== null && n.ratio < n.min) {
      belowFloor.push(n);
      continue;
    }
    tally.contrastAllowed += 1;
  }
  for (const n of denied) {
    console.log(
      "aaa " +
        surface +
        " " +
        theme +
        " DENIED " +
        n.selector +
        " ratio=" +
        n.ratio +
        " tag=" +
        n.tag +
        ' class="' +
        n.classes +
        '" ' +
        n.fg +
        " on " +
        n.bg,
    );
  }
  for (const n of belowFloor) {
    console.log(
      "aaa " +
        surface +
        " " +
        theme +
        " BELOW-FLOOR " +
        n.selector +
        " ratio=" +
        n.ratio +
        " floor=" +
        n.min +
        " (" +
        n.allowedBy +
        ")",
    );
  }
  console.log(
    "aaa " +
      surface +
      " " +
      theme +
      " enhanced_failures=" +
      res.nodes.length +
      " allowed=" +
      (res.nodes.length - denied.length - belowFloor.length) +
      " denied=" +
      denied.length +
      " below_floor=" +
      belowFloor.length +
      " passes=" +
      res.passes +
      " incomplete=" +
      res.incomplete +
      (CONTRAST_REPORT ? " (REPORT ONLY - nothing gated)" : ""),
  );
  if (denied.length > 0) {
    throw new Fail(
      12,
      "CONTRAST " +
        denied[0].selector +
        " on " +
        surface +
        "/" +
        theme +
        " at " +
        denied[0].ratio +
        ":1 — not named in scripts/a11y-contrast.allow",
    );
  }
  if (belowFloor.length > 0) {
    throw new Fail(
      12,
      "CONTRAST " +
        belowFloor[0].allowedBy +
        " on " +
        surface +
        "/" +
        theme +
        " fell to " +
        belowFloor[0].ratio +
        ":1, below its own floor of " +
        belowFloor[0].min +
        ":1 in scripts/a11y-contrast.allow",
    );
  }
}

/** What a reporting run prints at the end: one line per class + colour pair, with
 * the WORST ratio seen for it — which is the number an allowlist entry's floor has
 * to sit at or below. */
function printContrastReport() {
  if (!CONTRAST_REPORT) return;
  console.log("--- AAA contrast candidates (worst ratio per class + colour pair)");
  const rows = [...contrastReport.entries()].sort((a, b) => a[1].min - b[1].min);
  for (const [key, v] of rows) {
    console.log(
      "aaacand min=" + v.min.toFixed(2) + " n=" + v.count + " " + key + "  e.g. " + v.example,
    );
  }
  console.log("aaacand groups=" + rows.length);
}

// ---------------------------------------------------------------------------
// The themes
// ---------------------------------------------------------------------------
//
// THROUGH THE APP'S OWN CONTROL, not by writing `data-theme` from outside, and
// that is not fussiness: app/ui/Shell.tsx's `applyServerPrefs` says "SERVER WINS
// AT LOAD" — the stored ClientPreferences theme is re-applied over whatever
// localStorage held. A lane that set localStorage (or the attribute) would be
// racing that, and a pass that silently linted the light palette while claiming
// "dark" is worse than no pass at all. So each theme is chosen the way a user
// chooses it: the radio in Settings › Appearance, which writes both.
const THEMES = [
  { name: "light", pick: "light", expect: "light", media: null },
  { name: "dark", pick: "dark", expect: "dark", media: null },
  // "system" with the OS asking for dark: the third state, which is a different
  // code path (matchMedia, plus prefs.ts's watchSystemTheme).
  { name: "system-dark", pick: "system", expect: "dark", media: "dark" },
];

/**
 * Click a synced Appearance control and wait until the SERVER holds the value
 * (#658), not merely the screen.
 *
 * ⚠ THE ATTRIBUTE IS NOT THE WRITE. Every one of these controls applies locally
 * first and writes upward second (AppearanceRow.tsx, Shell.tsx setPaneLayout),
 * so `data-pane-layout="off"` is true the instant the click lands, while the
 * `ClientPreferences/set` is still on its way. The steps that follow RELOAD the
 * page (each surface starts from a cold document), a reload takes an unanswered
 * request down with the old document, and at load the server wins
 * (Shell.tsx applyServerPrefs): the device goes back to whatever the server
 * still holds. That is #658's line — "the pane took the screen: still false",
 * pane `vertical`, after a Focused click that had shown `off` moments earlier —
 * and it is reproduced on demand by holding every set back
 * (`A11Y_PREFS_SET_DELAY_MS`, which the lane sets on purpose).
 *
 * So the response is armed BEFORE the click (a fast server can answer before a
 * listener attached afterwards exists), matched on the field AND the value so
 * an earlier write of the same field cannot stand in for it, and its body is
 * read: a set the server refused by name is not a saved preference either.
 *
 * A radio that is ALREADY checked fires no change and writes nothing; waiting
 * for a write there would hang on a request nobody sends, so it is skipped —
 * and that is safe only because every earlier write went through here, so the
 * device's checked radio is the server's value.
 */
async function clickAndSave(page, sel, field, value) {
  const writes = await page.$eval(
    sel,
    (el) => !(el instanceof HTMLInputElement && el.type === "radio" && el.checked),
  );
  const needle = JSON.stringify(field) + ":" + JSON.stringify(value);
  const saved = writes
    ? page.waitForResponse(
        (r) => {
          const body = r.request().method() === "POST" ? r.request().postData() || "" : "";
          return body.indexOf('"ClientPreferences/set"') >= 0 && body.indexOf(needle) >= 0;
        },
        { timeout: 30000 },
      )
    : null;
  // Observed even if the click below throws, so a failed step cannot leave an
  // unhandled rejection behind to kill the process with a different message.
  if (saved) saved.catch(() => {});
  await page.click(sel);
  if (!saved) return;
  let res;
  try {
    res = await saved;
  } catch (e) {
    throw new Fail(
      11,
      "NOSTEP the " + field + "=" + JSON.stringify(value) + " write was never answered (" +
        (e && e.message ? e.message : String(e)) + "); prefsWrites " +
        JSON.stringify(recentPrefsWrites()),
    );
  }
  let set = null;
  try {
    const body = await res.json();
    set = ((body && body.methodResponses) || []).find((m) => m[2] === "set") || null;
  } catch (e) {
    set = null;
  }
  const updated = set && set[0] === "ClientPreferences/set" && set[1] && set[1].updated;
  if (!res.ok() || !updated || Object.keys(updated).length === 0) {
    throw new Fail(
      11,
      "NOSTEP the server did not take " + field + "=" + JSON.stringify(value) + ": HTTP " +
        res.status() + " " + JSON.stringify(set),
    );
  }
}

async function chooseTheme(page, theme) {
  if (theme.media) {
    await page.emulateMediaFeatures([
      { name: "prefers-color-scheme", value: theme.media },
    ]);
  } else {
    await page.emulateMediaFeatures([{ name: "prefers-color-scheme", value: "light" }]);
  }
  await gotoHash(page, "#/settings/appearance");
  const sel = '#setting-body-appearance input[name="theme"][value="' + theme.pick + '"]';
  await waitForSelectorD(page, sel, { timeout: 30000 });
  await clickAndSave(page, sel, "theme", theme.pick);
  await until(
    "theme " + theme.name + " applied",
    20000,
    () =>
      page.evaluate((want) => document.documentElement.dataset["theme"] === want, theme.expect),
    13,
  );
  await gotoHash(page, "#/inbox/review");
  await waitForDesk(page);
  const applied = await page.evaluate(() => document.documentElement.dataset["theme"]);
  if (applied !== theme.expect) {
    throw new Fail(
      13,
      "NOTHEME " + theme.name + " resolved to " + applied + ", want " + theme.expect,
    );
  }
  console.log("theme " + theme.name + " data-theme=" + applied);
}

/**
 * The row layout and the text size (#514), through the SAME Appearance controls
 * a user would use, and for `chooseTheme`'s reason restated: both are synced
 * `ClientPreferences` fields, and `applyServerPrefs` re-applies the stored value
 * over whatever localStorage held. A lane that wrote `data-row-layout` from
 * outside would be racing that, and a pass that linted the single-line desk
 * while its lines said "multiline" is worse than no pass at all.
 *
 * Each one WAITS FOR THE ATTRIBUTE rather than for a timeout: the write goes
 * through a JMAP round trip, and a fixed sleep is how a lane starts linting the
 * previous state.
 */
async function chooseRowLayout(page, value) {
  await gotoHash(page, "#/settings/appearance");
  const sel = '#setting-body-appearance input[name="rowLayout"][value="' + value + '"]';
  await waitForSelectorD(page, sel, { timeout: 30000 });
  await clickAndSave(page, sel, "rowLayout", value);
  await until("row layout " + value, 20000, () =>
    page.evaluate((want) => document.documentElement.dataset["rowLayout"] === want, value),
  );
  await gotoHash(page, "#/inbox/review");
  await waitForDesk(page);
}

/** The five-step control's POINTER half — the word buttons, not the slider,
 *  because a click is what puppeteer can do without inventing a drag. The
 *  slider's own keyboard behaviour is unit-tested (web/src/app/
 *  prefs.readingComfort.test.ts); what this lane is for is the RESULT. */
async function chooseTextSize(page, size) {
  await gotoHash(page, "#/settings/appearance");
  const sel = '#setting-body-appearance button.text-size-step[data-size="' + size + '"]';
  await waitForSelectorD(page, sel, { timeout: 30000 });
  await clickAndSave(page, sel, "textSize", size);
  await until("text size " + size, 20000, () =>
    page.evaluate((want) => document.documentElement.dataset["textSize"] === want, size),
  );
  await gotoHash(page, "#/inbox/review");
  await waitForDesk(page);
}

/** Beside vs focused, through the same radio group, for the same reason. */
async function chooseMessageOpens(page, value) {
  await gotoHash(page, "#/settings/appearance");
  const sel = '#setting-body-appearance input[name="messageOpens"][value="' + value + '"]';
  await waitForSelectorD(page, sel, { timeout: 30000 });
  await clickAndSave(page, sel, "messageOpens", value);
  await until("messageOpens " + value, 20000, () =>
    page.evaluate(
      (want) =>
        document.documentElement.dataset["paneLayout"] ===
        (want === "focused" ? "off" : "vertical"),
      value,
    ),
    11,
    describePaneLayout(page),
  );
  await gotoHash(page, "#/inbox/review");
  await waitForDesk(page);
}

// ---------------------------------------------------------------------------
// The surfaces
// ---------------------------------------------------------------------------
//
// Each one is opened the way a user opens it — a hash the rail links to, or the
// key/button that reveals it — and each WAITS FOR ITS OWN LANDMARK before axe
// runs. Waiting for a fixed number of milliseconds is how a lane starts linting
// the previous surface.

const SURFACES = [
  {
    // THE DAY-ONE WIZARD (#446), audited as a surface in its own right — it is a
    // real screen a fresh login sees, not a modal over the desk (App.tsx: `setup
    // ? <SetupRoute /> : ...`). Reached by NAVIGATING TO `#/setup` explicitly
    // rather than by racing the auto-open gate: the fixture stage marks
    // `setupCompletedAt` before this script ever launches a browser (see the
    // fixture stage in scripts/a11y-e2e.sh), which is what lets every OTHER
    // surface below load straight to `#desk-grid` instead of dying behind the
    // wizard the way this lane did the day main() grew it (mailroom#447 follow-
    // up: `ERR Waiting for selector #desk-grid[role=grid] failed`, because the
    // fixture user's DeskPreferences had never been told setup was done).
    // `#/setup` renders the wizard NO MATTER what `setupCompletedAt` says — it is
    // a plain route, not gated by `shouldOpenSetup` (App.tsx: `const setup =
    // route.page === "setup"`) — which is exactly what "Run setup again" from
    // Settings relies on, so this is the same surface a user reaches by hand.
    name: "setup-wizard",
    async open(page) {
      await load(page, "#/setup");
      await waitForSelectorD(page, 'section.setup[aria-labelledby="setup-title"]', {
        timeout: 30000,
      });
      // Step 1's own report, so axe lints the loaded wizard rather than the
      // "Loading…" status placeholder every step starts on.
      await waitForSelectorD(page, "p.setup-lead", { timeout: 30000 });
    },
  },
  {
    name: "desk-review",
    async open(page) {
      await load(page, "#/inbox/review");
      await waitForDesk(page);
    },
  },
  {
    name: "desk-rest-drawer-open",
    async open(page) {
      await load(page, "#/inbox/rest");
      const sel = await focusFixtureDrawerButton(page);
      await page.click(sel);
      await until("a drawer opened", 30000, () =>
        page.evaluate(
          () => document.querySelectorAll('.desk-drawer button[aria-expanded="true"]').length > 0,
        ),
      );
      await until("the open drawer has rows", 30000, () =>
        page.evaluate(() => document.querySelectorAll("#desk-grid li.desk-item-row").length > 0),
      );
    },
  },
  {
    name: "tray",
    async open(page) {
      await load(page, "#/tray");
      await waitForSelectorD(page, "#tray-grid[role=grid]", { timeout: 30000 });
    },
  },
  {
    name: "all-mail",
    async open(page) {
      await load(page, "#/all");
      await waitForSelectorD(page, "ul.messages", { timeout: 30000 });
      await until("all-mail has rows", 30000, () =>
        page.evaluate(() => document.querySelectorAll("ul.messages > li").length > 0),
      );
    },
  },
  {
    name: "settings",
    async open(page) {
      await load(page, "#/settings");
      await waitForSelectorD(page, '.settings-page[role="dialog"]', { timeout: 30000 });
    },
  },
  {
    name: "settings-section-open",
    async open(page) {
      await load(page, "#/settings/appearance");
      await waitForSelectorD(page, "#setting-body-appearance input[name='theme']", {
        timeout: 30000,
      });
    },
  },
  {
    name: "compose",
    async open(page) {
      await load(page, "#/inbox/review");
      await waitForDesk(page);
      // `c` — the key, not the URL, so the shell's own chain is what opens it.
      await page.keyboard.press("c");
      await waitForSelectorD(page, 'section.compose[role="dialog"]', { timeout: 30000 });
    },
  },
  {
    // A COMPOSER REACHED BY RELOAD (#612). The surface above opens the composer
    // with a keypress on a page whose session is long established — the warm
    // path — and it stayed green for the whole time a RELOAD onto a composer URL
    // hung on "Opening…" for ever: the composer mounted before the session
    // existed, its one read failed "not connected", and nothing asked again.
    // A forward takes the same fetch path a resumed draft does (both need the
    // body), and the fixture has one, so this needs no draft to be created. The
    // waiting line is a plain `section.compose` with no dialog role; only the
    // real composer matches, so this wait IS the assertion.
    name: "compose-cold-forward",
    async open(page) {
      await load(page, "#/inbox/review?compose=forward:" + FIX.attachment.id);
      await waitForSelectorD(page, 'section.compose[role="dialog"]', { timeout: 30000 });
    },
    // ⚠ mailroom#614, BOTH WAYS, IN A REAL BROWSER — and the fixture put back.
    //
    // An untouched forward used to be SAVED within one autosave tick (its subject
    // and quote make it non-empty), and that draft threaded onto the attachment
    // message: every later surface then measured a two-message thread, and the
    // reading pane went red on elements this surface had created. So:
    //   1. hold the untouched forward open past TWO autosave ticks (3 s each) and
    //      require that nothing was saved — the #614 guard;
    //   2. type one character and require that it IS saved. Without this half a
    //      composer that never saves at all would pass step 1, and that is #601's
    //      bug (a forward's covering note thrown away) wearing #614's fix;
    //   3. discard that draft and wait for the destroy response itself, so no
    //      later surface sees it.
    async after(page) {
      const saveState = () =>
        page.evaluate(() => {
          const s = document.querySelector(".compose-save-state");
          return s ? s.getAttribute("data-state") : null;
        });
      await sleep(7000);
      const untouched = await saveState();
      if (untouched !== "never") {
        throw new Fail(
          15,
          "UNTOUCHED compose-cold-forward state=" + untouched +
            " after 7s with nothing typed — a forward nobody edited was saved (#614)",
        );
      }
      await page.click("section.compose .editor-surface");
      await page.keyboard.type("x");
      await until(
        "the typed forward's first autosave has landed (#614's control)",
        30000,
        async () => (await saveState()) === "saved",
        15,
      );
      // The composer closes BEFORE its destroy request returns (Compose.tsx
      // discard), and the next surface's reload would abort it in flight — so
      // the wait is on that response itself, armed before the click. (Not a
      // network-idle wait: the push stream is a request that never finishes.)
      const destroyed = page.waitForResponse(
        (r) =>
          r.request().method() === "POST" &&
          (r.request().postData() || "").indexOf('"destroy"') >= 0,
        { timeout: 30000 },
      );
      await page.click("section.compose button.compose-discard");
      const res = await destroyed;
      if (!res.ok()) throw new Error("discarding the forward's draft: HTTP " + res.status());
      await until("the composer has closed", 30000, () =>
        page.evaluate(() => !document.querySelector("section.compose")),
      );
      // #858: the discard above can lose a race with an autosave already in
      // flight, and that draft would thread onto the attachment message.
      await settleAttachmentThread(page, "compose-cold-forward");
    },
  },
  {
    name: "reading-pane",
    async open(page) {
      // #858's in-run control: this surface lints ONE message, never a thread.
      await assertAttachmentThreadSingle(page, "reading-pane");
      await openTheAttachmentMessage(page);
    },
  },
  {
    name: "sender-popover-pinned",
    async open(page) {
      await openTheAttachmentMessage(page);
      await page.click("button.message-from-button");
      await waitForSelectorD(page, '.sender-popover[role="dialog"]', { timeout: 30000 });
    },
  },
  {
    name: "attachment-viewer",
    async open(page) {
      await openTheAttachmentMessage(page);
      await waitForSelectorD(page, "button.attachment-openable", { timeout: 30000 });
      await page.click("button.attachment-openable");
      await waitForSelectorD(page, 'section.viewer[role="dialog"]', { timeout: 30000 });
    },
  },
  {
    // THE WITHHELD CHIP, AS A HELD INLINE IMAGE PRODUCES IT (#675). The chip
    // itself is older than this surface, but nothing in this lane ever drew
    // one: it needs a quarantined part, and #675 is what made a held INLINE
    // image produce one at all. Linted on every theme like the rest, and its
    // names are read out of the accessibility tree (heldChipCheck), because a
    // chip announced as the wrong thing is not something axe can see.
    name: "reading-pane-held-inline",
    async open(page) {
      await openTheHeldInlineMessage(page);
      const r = await heldChipCheck(page);
      if (!r.ok) throw new Fail(7, "NONAME held-inline chip: " + r.why);
      tally.heldChip += 1;
    },
  },
  {
    // THE DESK IN THE MULTI-LINE LAYOUT (#514). Same desk, same rows, same
    // roles — the whole point — at three lines a row.
    name: "desk-multiline",
    async open(page) {
      await chooseRowLayout(page, "multiLine");
      await until("the multi-line desk has rows", 30000, () =>
        page.evaluate(() => document.querySelectorAll("#desk-grid li.desk-item-row").length > 0),
      );
    },
    async after(page) {
      await chooseRowLayout(page, "oneLine");
    },
  },
  {
    // THE DESK AT THE LARGEST TEXT SIZE (#514), single line. 18px root: every
    // box on the page is a third larger than the one axe measured on the
    // surface above, which is where a contrast pair computed against a
    // background that has moved, or a control that has overflowed its own
    // clickable area, would show up.
    name: "desk-text-xl",
    async open(page) {
      await chooseTextSize(page, "xl");
      await until("the largest-text desk has rows", 30000, () =>
        page.evaluate(() => document.querySelectorAll("#desk-grid li.desk-item-row").length > 0),
      );
    },
    async after(page) {
      // ⚠ PUT BACK, and not only out of tidiness: the announcement script and
      // the negative controls run AFTER this loop, against the desk as it is
      // left. Leaving the app at `xl` would silently change what those steps
      // measure, and leaving it in multi-line would change which element the
      // cursor assertions are looking at.
      await chooseTextSize(page, "m");
    },
  },
  {
    name: "focused-reading",
    async open(page) {
      await chooseMessageOpens(page, "focused");
      await openTheAttachmentMessage(page);
      await until("the pane took the screen", 20000, () =>
        page.evaluate(() => document.documentElement.dataset["paneLayout"] === "off"),
        11,
        describePaneLayout(page),
      );
    },
    async after(page) {
      // Put the device back the way every other surface expects it.
      await chooseMessageOpens(page, "beside");
    },
  },
];

/**
 * Open the ONE drawer the fixture put mail in, by NAME.
 *
 * ⚠ NOT "the first closed drawer". Everything else lists one drawer per area, and
 * most of them are empty — a drawer with nothing in it opens, says
 * `aria-expanded="true"`, and produces no rows, which would make "rows appeared
 * beneath it" fail for a reason that has nothing to do with disclosure. The
 * fixture manifest names the drawer that has a count, so this asks for that one.
 */
async function openFixtureDrawer(page) {
  await waitForSelectorD(page, ".desk-drawer button[aria-expanded]", { timeout: 30000 });
  const found = await page.evaluate((name) => {
    const drawers = Array.from(document.querySelectorAll(".desk-drawer"));
    const hit = drawers.find((d) => {
      const n = d.querySelector(".desk-drawer-name");
      return !!n && n.textContent.trim() === name;
    });
    if (!hit) return { err: "no drawer row called " + name };
    const btn = hit.querySelector("button[aria-expanded]");
    if (!btn) return { err: "the " + name + " drawer has no disclosure button" };
    const count = Number((hit.querySelector(".desk-drawer-count") || {}).textContent || "0");
    return { count, expanded: btn.getAttribute("aria-expanded"), label: btn.getAttribute("aria-label") };
  }, FIX.drawerArea.name);
  if (found.err) throw new Fail(14, "NOFIXTURE " + found.err);
  if (!(found.count > 0)) {
    throw new Fail(
      14,
      "NOFIXTURE the " + FIX.drawerArea.name + " drawer shows " + found.count + " messages",
    );
  }
  return found;
}

/** Focus the fixture drawer's disclosure button, and return the node's own
 * selector so callers can read it back out of the AX tree. */
async function focusFixtureDrawerButton(page) {
  await openFixtureDrawer(page);
  const ok = await page.evaluate((name) => {
    const drawers = Array.from(document.querySelectorAll(".desk-drawer"));
    const hit = drawers.find((d) => {
      const n = d.querySelector(".desk-drawer-name");
      return !!n && n.textContent.trim() === name;
    });
    const btn = hit && hit.querySelector("button[aria-expanded]");
    if (!btn) return false;
    btn.id = "a11y-drawer-disclosure";
    btn.focus();
    return document.activeElement === btn;
  }, FIX.drawerArea.name);
  if (!ok) throw new Fail(14, "NOFIXTURE could not focus the " + FIX.drawerArea.name + " drawer button");
  return "#a11y-drawer-disclosure";
}

/** The desk with the fixture's ATTACHMENT message open — the one message that can
 * reach the reading pane, the sender popover and the viewer.
 *
 * `list` is the list route it is opened from. The desk (the default) only works
 * while the message is ON the desk: the announcement script ARCHIVES this very
 * message (its `move` lands on it before `e`), so anything that runs after that
 * script must pass "all". Until #652 the desk ignored Mailroom's own pending
 * moves, so an archived message stayed on it until the next Inbox observation —
 * and the docked-split step below passed by relying on exactly that lag. */
/**
 * THE ATTACHMENT FIXTURE'S THREAD, AS THE SERVER HOLDS IT (#858).
 *
 * A forward's autosaved draft threads with the message it forwards, so the
 * scenarios that forward FIX.attachment (compose-cold-forward, the docked-split
 * check) can leave a second message in the thread every later surface opens.
 * Timing decides whether they do, which is what made the reading pane's
 * system-dark pass flake: the pane then drew `.thread-count` and a folded
 * message, none of it part of what that surface is meant to lint.
 *
 * TWO HALVES, because the first can race and the second cannot be argued with:
 *   * settleAttachmentThread(): after a forward scenario, DESTROY any draft that
 *     joined the thread (the scenario's own discard can lose a race with an
 *     autosave already in flight), wait out two autosave ticks, and require the
 *     thread to be one message again. A scenario that cannot put it back fails
 *     there, naming itself.
 *   * assertAttachmentThreadSingle(): the in-run control, asserted when the
 *     reading pane is about to be linted. If any path leaves a second message
 *     behind, the run fails HERE with the message count, instead of passing
 *     with a different pane or failing later on a colour nobody drew on purpose.
 *
 * Chosen over forwarding a dedicated fixture message: that would be a fourteenth
 * message, and every count this lane prints and asserts (the desk's sections,
 * the drawer, the tray, the phone-width views) would shift with it. The draft is
 * the thing that leaks, so the draft is what is removed, and the control proves
 * it.
 */
async function attachmentThreadMessages(page) {
  return page.evaluate(async (id) => {
    const sess = await (await fetch("/jmap/session", { credentials: "include" })).json();
    const accountId = Object.values(sess.primaryAccounts)[0];
    const using = ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"];
    const call = async (name, args) => {
      const res = await fetch(sess.apiUrl, {
        method: "POST",
        credentials: "include",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ using, methodCalls: [[name, { accountId, ...args }, "c"]] }),
      });
      const out = (await res.json()).methodResponses[0];
      if (out[0] === "error") throw new Error(name + ": " + JSON.stringify(out[1]));
      return out[1];
    };
    const e = await call("Email/get", { ids: [id], properties: ["threadId"] });
    const threadId = e.list[0].threadId;
    const t = await call("Thread/get", { ids: [threadId] });
    const ids = t.list[0].emailIds;
    const m = await call("Email/get", { ids, properties: ["keywords", "subject"] });
    return m.list.map((x) => ({ id: x.id, draft: !!(x.keywords && x.keywords["$draft"]), subject: x.subject }));
  }, FIX.attachment.id);
}

async function assertAttachmentThreadSingle(page, where) {
  const msgs = await attachmentThreadMessages(page);
  if (msgs.length !== 1 || msgs[0].id !== FIX.attachment.id) {
    throw new Fail(
      15,
      "UNTOUCHED " + where + ": the attachment fixture's thread has " + msgs.length +
        " message(s), want exactly 1 (a forward's draft threaded onto it, #858): " + JSON.stringify(msgs),
    );
  }
}

async function settleAttachmentThread(page, where) {
  for (let pass = 0; pass < 2; pass += 1) {
    const strays = (await attachmentThreadMessages(page)).filter((m) => m.id !== FIX.attachment.id);
    const nonDrafts = strays.filter((m) => !m.draft);
    if (nonDrafts.length) {
      throw new Fail(15, "UNTOUCHED " + where + ": the attachment fixture's thread holds a message that is not a draft: " + JSON.stringify(nonDrafts));
    }
    if (strays.length) {
      await page.evaluate(async (ids) => {
        const sess = await (await fetch("/jmap/session", { credentials: "include" })).json();
        const accountId = Object.values(sess.primaryAccounts)[0];
        const res = await fetch(sess.apiUrl, {
          method: "POST",
          credentials: "include",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            using: ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"],
            methodCalls: [["Email/set", { accountId, destroy: ids }, "d"]],
          }),
        });
        await res.json();
      }, strays.map((m) => m.id));
      console.log("settled " + where + ": destroyed " + strays.length + " draft(s) that joined the attachment thread");
    }
    // Two autosave ticks (3 s each): a save that was in flight when the composer
    // closed has landed by now, and the second pass would find and remove it.
    await sleep(pass === 0 ? 7000 : 1000);
  }
  await assertAttachmentThreadSingle(page, where);
}

/** Closes an open composer by its Discard button, as a user leaving would. */
async function discardComposer(page) {
  if (!(await page.$("section.compose"))) return;
  await page.click("section.compose button.compose-discard");
  await until("the composer has closed", 30000, () => page.evaluate(() => !document.querySelector("section.compose")));
}

async function openTheAttachmentMessage(page, list = "inbox/review") {
  await load(page, "#/" + list + "?m=" + FIX.attachment.id);
  await waitForSelectorD(page, "aside.thread-pane", { timeout: 30000 });
  await until("the open message is the fixture's", 30000, () =>
    page.evaluate((subject) => {
      const h = document.querySelector("h1.thread-subject");
      return !!h && h.textContent.indexOf(subject) >= 0;
    }, FIX.attachment.subject),
  );
}

/** Opens the message whose inline image the quarantine holds (#675), and
 *  waits for its withheld chip. */
async function openTheHeldInlineMessage(page) {
  await load(page, "#/all?m=" + FIX.heldInline.id);
  await waitForSelectorD(page, "aside.thread-pane", { timeout: 30000 });
  await until("the open message is the held-inline fixture", 30000, () =>
    page.evaluate((subject) => {
      const h = document.querySelector("h1.thread-subject");
      return !!h && h.textContent.indexOf(subject) >= 0;
    }, FIX.heldInline.subject),
  );
  await waitForSelectorD(page, ".attachment-withheld", { timeout: 30000 });
}

/**
 * What the held image's chip ANNOUNCES, from the accessibility tree: a group
 * named "<file> — withheld" that holds a button named "Release". Both halves
 * are the point — the name says what was held and that it was held, and the
 * button is the only way out of it (ADR-027 §8).
 */
async function heldChipCheck(page) {
  const snap = await axSnapshot(page, ".attachment-withheld");
  if (!snap) return { ok: false, why: "no .attachment-withheld in the pane" };
  const want = FIX.heldInline.name + " — withheld";
  if (snap.role !== "group" || snap.name !== want) {
    return { ok: false, why: "announced as " + snap.role + ' "' + snap.name + '", want group "' + want + '"' };
  }
  const buttons = [];
  const walk = (n) => {
    if (!n) return;
    if (n.role === "button") buttons.push(n.name);
    for (const c of n.children || []) walk(c);
  };
  walk(snap);
  if (buttons.indexOf("Release") < 0) {
    return { ok: false, why: "no button named Release inside it: " + JSON.stringify(buttons) };
  }
  return { ok: true, why: "" };
}

// ---------------------------------------------------------------------------
// The docked split (mailroom#617)
// ---------------------------------------------------------------------------

/** The list's width, and the content area it is a percent of while something is
 *  docked: from the list's left edge to the docked panel's right edge. */
async function dockedGeometry(page) {
  return page.evaluate(() => {
    const list = document.getElementById("messages");
    const panel = document.querySelector(".compose-pane, .viewer-pane");
    if (!list || !panel) return null;
    const l = list.getBoundingClientRect();
    const p = panel.getBoundingClientRect();
    return { list: Math.round(l.width), left: l.left, area: Math.round(p.right - l.left) };
  });
}

/**
 * WITH A COMPOSER DOCKED, THE SYNCED SPLIT MEANS WHAT IT SAYS, AND CAN BE DRAGGED.
 *
 * The defect: the synced percent was converted against `#messages` alone, which
 * while a composer is docked is the LIST — so a cold load with a draft open took
 * the percent of the list's own width and pinned it at the 280px minimum. And no
 * splitter existed while composing, so it could not be put back.
 *
 * ⚠ NOT VACUOUS BY CONSTRUCTION. localStorage is planted with a width the synced
 * percent does NOT describe before the cold load, so the list starts wrong and can
 * only reach the expected width if the synced percent was APPLIED — and measured
 * against the right area. Planting the right width instead would pass before the
 * preferences had even arrived.
 */
async function dockedSplitCheck(page) {
  // 1. Set a known split the way a user does: a message open beside the list,
  //    the real splitter moved by keyboard, the app saving the percent.
  await load(page, "#/all?m=" + FIX.attachment.id);
  await waitForSelectorD(page, ".content .pane-splitter", { timeout: 30000 });
  // The SET, not any request naming the property: a ClientPreferences/get that
  // lists its properties names `paneSplitPct` too, and carries no value.
  const saved = page.waitForResponse(
    (r) => {
      const body = r.request().postData() || "";
      return (
        r.request().method() === "POST" &&
        body.indexOf("ClientPreferences/set") >= 0 &&
        body.indexOf("paneSplitPct") >= 0
      );
    },
    { timeout: 30000 },
  );
  await page.focus(".content .pane-splitter");
  await page.keyboard.press("End"); // the vertical maximum
  const pct = JSON.parse((await saved).request().postData() || "{}").methodCalls
    ?.map((c) => JSON.stringify(c))
    .join("")
    .match(/"paneSplitPct":(\d+)/);
  if (!pct) throw new Fail(16, "SPLIT the splitter saved no paneSplitPct");
  const percent = Number(pct[1]);

  // 2. Plant a WRONG local width, then cold-load straight onto a docked composer.
  await page.evaluate(() => localStorage.setItem("mailroom.paneSplit.vertical", "300"));
  await load(page, "#/all?compose=forward:" + FIX.attachment.id);
  await waitForSelectorD(page, 'section.compose[role="dialog"]', { timeout: 30000 });
  let geo = null;
  const expect = (g) => Math.min(720, Math.max(280, Math.round((g.area * percent) / 100)));
  try {
    await until("the docked list reaches the synced split", 15000, async () => {
      geo = await dockedGeometry(page);
      return !!geo && Math.abs(geo.list - expect(geo)) <= 12;
    }, 16);
  } catch (e) {
    throw new Fail(
      16,
      "SPLIT docked list=" + (geo && geo.list) + "px, want ~" + (geo && expect(geo)) +
        "px (" + percent + "% of area=" + (geo && geo.area) +
        "px) after a cold load with a composer docked (#617)",
    );
  }
  console.log("split cold-load percent=" + percent + " area=" + geo.area + " list=" + geo.list);

  // 3. The docked splitter exists while composing, and a POINTER drag moves the
  //    list to where the pointer is — measured from the LIST's left edge, which is
  //    the half a keyboard step would not test (its parent is the workspace).
  const bar = await page.evaluate(() => {
    const s = document.querySelector(".pane-splitter-docked");
    if (!s) return null;
    const r = s.getBoundingClientRect();
    return { display: getComputedStyle(s).display, x: r.left + r.width / 2, y: r.top + r.height / 2 };
  });
  if (!bar || bar.display !== "block") {
    throw new Fail(16, "SPLIT no docked splitter while composing: " + JSON.stringify(bar));
  }
  const target = Math.round(geo.left + 400);
  await page.mouse.move(bar.x, bar.y);
  await page.mouse.down();
  await page.mouse.move(target, bar.y, { steps: 8 });
  await page.mouse.up();
  const dragged = await dockedGeometry(page);
  if (!dragged || Math.abs(dragged.list - 400) > 12) {
    throw new Fail(
      16,
      "SPLIT dragging the docked splitter to list+400px left the list at " +
        (dragged && dragged.list) + "px",
    );
  }
  console.log("split docked-drag want=400 list=" + dragged.list);
  // #858: this step opened a forward of the attachment message and must not leave
  // it behind, whatever the autosave timing was.
  await discardComposer(page);
  await settleAttachmentThread(page, "docked-split");

  // 4. The docked attachment viewer takes the same column the same way, so it
  //    has the same splitter — asserted, not assumed (the two CSS files mirror
  //    each other, which is exactly how one of two gets missed). Opened from All
  //    mail: the announcement script has archived this message by now, so it is
  //    no longer on the desk (see openTheAttachmentMessage).
  await openTheAttachmentMessage(page, "all");
  await waitForSelectorD(page, "button.attachment-openable", { timeout: 30000 });
  await page.click("button.attachment-openable");
  await waitForSelectorD(page, 'section.viewer[role="dialog"]', { timeout: 30000 });
  const viewerBar = await page.evaluate(() => {
    const s = document.querySelector(".pane-splitter-docked");
    return s ? getComputedStyle(s).display : null;
  });
  if (viewerBar !== "block") {
    throw new Fail(16, "SPLIT no docked splitter beside the attachment viewer: " + viewerBar);
  }
  tally.dockedSplit = 1;
}

// ---------------------------------------------------------------------------
// The announcement script
// ---------------------------------------------------------------------------

/** The one live region's text, as the tree has it. */
async function liveText(page) {
  return page.evaluate(() => {
    const p = document.querySelector("p.desk-header");
    return p ? p.textContent.replace(/\s+/g, " ").trim() : "";
  });
}

/**
 * The live region, asserted as a live region — role and `aria-live` from the DOM
 * AND the computed `live` property from the accessibility tree. Both, because
 * they are different claims: the attributes are what we wrote, `live` is what the
 * browser decided, and negative control 2 has to be able to kill both.
 */
async function checkLiveRegion(cdp, page) {
  const dom = await page.evaluate(() => {
    const p = document.querySelector("p.desk-header");
    if (!p) return null;
    return { role: p.getAttribute("role"), live: p.getAttribute("aria-live") };
  });
  const ax = await axCDP(cdp, "p.desk-header");
  const ok =
    !!dom &&
    dom.role === "status" &&
    dom.live === "polite" &&
    !!ax &&
    ax.props["live"] === "polite";
  return {
    ok,
    detail:
      "dom=" +
      JSON.stringify(dom) +
      " ax_live=" +
      (ax ? String(ax.props["live"]) : "no-node") +
      " ax_role=" +
      (ax ? String(ax.role) : "-"),
  };
}

/**
 * The grid's current row, resolved the way a screen reader resolves it: read
 * `aria-activedescendant`, look the id UP IN THE DOCUMENT, and read that node's
 * accessible name, role and states.
 *
 * ⚠ THE LOOKUP IS THE ASSERTION. An `aria-activedescendant` naming an id that is
 * not in the document is worse than no attribute at all — the grid claims a
 * current row and the reader finds none — which is exactly the trap
 * VirtualList.tsx's `pinnedWindow` exists to avoid, and it can only be caught by
 * resolving it.
 */
async function readActiveRow(page, cdp) {
  const info = await page.evaluate(() => {
    const g = document.querySelector("#desk-grid");
    if (!g) return { err: "no grid" };
    const id = g.getAttribute("aria-activedescendant");
    if (!id) return { err: "no aria-activedescendant" };
    const row = document.getElementById(id);
    if (!row) return { err: "aria-activedescendant names " + id + ", which is not in the document" };
    return {
      id,
      rowindex: Number(row.getAttribute("aria-rowindex") || "0"),
      selected: row.getAttribute("aria-selected"),
      current: row.getAttribute("aria-current"),
      rowcount: Number(g.getAttribute("aria-rowcount") || "0"),
      gridLabelledBy: g.getAttribute("aria-labelledby"),
    };
  });
  if (info.err) return { err: info.err };
  const ax = await axCDP(cdp, '#desk-grid [id="' + info.id + '"]');
  return Object.assign({}, info, {
    role: ax ? ax.role : null,
    name: ax ? ax.name : null,
    axSelected: ax ? ax.props["selected"] : undefined,
  });
}

/** Does this row name look like the desk's assembled sentence, and is it a
 * sentence about a message THIS FIXTURE delivered? */
function nameIsAFixtureRow(name) {
  if (!name || !/^(Unread, )?.+, .+/.test(name)) return null;
  return (
    FIX.messages.find(
      (m) => name.indexOf(m.sender) >= 0 && name.indexOf(m.subject) >= 0,
    ) || null
  );
}

/**
 * Where the pane is scrolled to, and where the cursor row sits in it.
 *
 * `inView` is "some of the row shows BELOW the sticky chrome": the chrome covers
 * the top of the pane, so a row under it is not on screen even though its box is
 * inside the scroller's.
 */
async function cursorView(page) {
  return page.evaluate(() => {
    const pane = document.querySelector(".desk-pane");
    const grid = document.querySelector("#desk-grid");
    const chrome = document.querySelector(".desk-chrome");
    const id = grid ? grid.getAttribute("aria-activedescendant") : null;
    const row = id ? document.getElementById(id) : null;
    if (!pane || !row) return { err: "no pane or no cursor row (" + id + ")" };
    const p = pane.getBoundingClientRect();
    const c = chrome ? chrome.getBoundingClientRect().bottom : p.top;
    const r = row.getBoundingClientRect();
    return {
      id,
      rowindex: Number(row.getAttribute("aria-rowindex") || "0"),
      scrollTop: pane.scrollTop,
      max: pane.scrollHeight - pane.clientHeight,
      inView: r.bottom > c + 1 && r.top < p.bottom - 1,
    };
  });
}

/**
 * SCROLLING AWAY FROM THE CURSOR ROW, AND THE LIST STAYING WHERE IT WAS PUT.
 *
 * Reported on v1.86.0: "I can't scroll up or down on the list past the
 * point where that email wouldn't be visible anymore." VirtualList's follow
 * effect depended on the measured row height as well as the cursor, the list is
 * measured on every scroll frame, and the effect put the cursor back in view each
 * time that number changed — whether or not the cursor had moved. Measured in
 * this browser before the fix: switching the density with the pane scrolled 900px
 * away pulled it back to 45px, and a 2e-5px wobble in the measured height (the
 * shape a converted, scrolled rect has on some platforms; headless Chromium at
 * these sizes reports a steady height, so it is injected here) clamped every wheel
 * step at the cursor's edge: 100 -> 56, 156 -> 56, 156 -> 56.
 *
 * Three ways of re-measuring without moving the cursor, each of which must leave
 * the pane alone, and then the positive control: the cursor MOVING still brings
 * its row into view. Exit 17 on any of them.
 */
async function scrollAwayFromCursor(page) {
  // The cursor on the first row, the pane at the top. Focus first and the scroll
  // after it: focusing the grid scrolls the pane to the grid's own top.
  await page.evaluate(() => {
    const g = document.querySelector("#desk-grid");
    if (g) g.focus({ preventScroll: true });
  });
  await page.keyboard.press("Home");
  const start = await until("scroll-away: the cursor is on the first row, in view", 15000, async () => {
    await page.evaluate(() => {
      const pane = document.querySelector(".desk-pane");
      if (pane) pane.scrollTop = 0;
    });
    await sleep(150);
    const v = await cursorView(page);
    return !v.err && v.inView && v.scrollTop <= 1 ? v : false;
  }, 17, async () => JSON.stringify(await cursorView(page)));
  if (start.max < 300) {
    throw new Fail(
      17,
      "SCROLLPULL precondition: the pane scrolls only " +
        start.max +
        "px at this viewport, so scrolling the cursor away would prove nothing",
    );
  }
  const box = await page.evaluate(() => {
    const p = document.querySelector(".desk-pane").getBoundingClientRect();
    const c = document.querySelector(".desk-chrome").getBoundingClientRect();
    return { x: p.left + p.width / 2, y: (c.bottom + p.bottom) / 2 };
  });
  await page.mouse.move(box.x, box.y);

  /** Wheel by `dy` and wait for the scroll position to settle; returns it. */
  const wheel = async (dy) => {
    await page.mouse.wheel({ deltaY: dy });
    let last = -1;
    for (let i = 0; i < 20; i += 1) {
      await sleep(60);
      const now = await page.evaluate(() => document.querySelector(".desk-pane").scrollTop);
      if (now === last) return now;
      last = now;
    }
    return last;
  };
  /** One frame for the measurement, one for the render and its effects. */
  const frames = () =>
    page.evaluate(
      () =>
        new Promise((r) =>
          requestAnimationFrame(() => requestAnimationFrame(() => setTimeout(r, 150))),
        ),
    );

  // ---- a. the wheel, plainly ----------------------------------------------
  const steps = [];
  let at = start.scrollTop;
  for (let i = 0; i < 4; i += 1) {
    const want = Math.min(start.max, at + 100);
    const got = await wheel(100);
    steps.push(at + "->" + got);
    if (Math.abs(got - want) > 1) {
      throw new Fail(17, "SCROLLPULL wheel: scrolled " + steps.join(" ") + ", want " + want);
    }
    at = got;
  }
  const away = await cursorView(page);
  if (away.inView) {
    throw new Fail(17, "SCROLLPULL wheel: after 400px of wheel the cursor row is still on screen");
  }

  // ---- b. a re-measured row height: the density changes -------------------
  const density = await page.evaluate(() => document.documentElement.dataset["density"] || "");
  const other = density === "compact" ? "comfortable" : "compact";
  await page.evaluate((d) => {
    document.documentElement.dataset["density"] = d;
  }, other);
  await frames();
  const afterDensity = await cursorView(page);
  await page.evaluate((d) => {
    if (d) document.documentElement.dataset["density"] = d;
    else delete document.documentElement.dataset["density"];
  }, density);
  await frames();
  // Judged on the CURSOR, not on the pixel: new row heights move every row, so
  // the browser's own scroll anchoring may legitimately shift the offset to keep
  // what was on screen on screen. What it must not do is land on the cursor row
  // (the pull-back put it just under the chrome, at 45px of 900).
  if (afterDensity.err || afterDensity.inView) {
    throw new Fail(
      17,
      "SCROLLPULL density: " +
        density +
        " -> " +
        other +
        " moved the pane " +
        at +
        " -> " +
        afterDensity.scrollTop +
        " (cursor row on screen: " +
        afterDensity.inView +
        "); the cursor did not move",
    );
  }
  at = await page.evaluate(() => document.querySelector(".desk-pane").scrollTop);

  // ---- c. a row height that wobbles with the scroll position --------------
  await page.evaluate(() => {
    const orig = Element.prototype.getBoundingClientRect;
    window.__wobbleRestore = () => {
      Element.prototype.getBoundingClientRect = orig;
    };
    Element.prototype.getBoundingClientRect = function () {
      const r = orig.call(this);
      if (!this.classList || !this.classList.contains("desk-probe")) return r;
      const pane = document.querySelector(".desk-pane");
      const st = pane ? Math.round(pane.scrollTop) : 0;
      return new DOMRect(r.x, r.y, r.width, r.height + (st % 3) * 1e-5);
    };
  });
  const wobbleSteps = [];
  try {
    for (const dy of [-100, -50, 100, 70]) {
      const want = Math.max(0, Math.min(start.max, at + dy));
      const got = await wheel(dy);
      await frames();
      const settledAt = await page.evaluate(() => document.querySelector(".desk-pane").scrollTop);
      wobbleSteps.push(at + "->" + settledAt);
      if (Math.abs(settledAt - want) > 1 || Math.abs(got - want) > 1) {
        throw new Fail(
          17,
          "SCROLLPULL wobble: with a sub-pixel row-height wobble the wheel went " +
            wobbleSteps.join(" ") +
            ", want " +
            want +
            " — the list is being pulled back to the cursor",
        );
      }
      at = settledAt;
    }
  } finally {
    await page.evaluate(() => window.__wobbleRestore && window.__wobbleRestore());
  }

  // ---- d. the positive control: the cursor MOVES, and is followed ---------
  // `j` from the first row, with the pane scrolled away from it: the cursor's new
  // row is off screen, and following it is the whole job of the effect.
  const before = await cursorView(page);
  await page.keyboard.press("j");
  const moved = await until("scroll-away: j moved the cursor and brought it into view", 15000, async () => {
    const v = await cursorView(page);
    return !v.err && v.id !== before.id && v.inView ? v : false;
  }, 17, async () => JSON.stringify(await cursorView(page)));

  tally.scrollAway = 1;
  console.log(
    "scroll-away       wheel " +
      steps.join(" ") +
      " | density " +
      density +
      "->" +
      other +
      " kept " +
      afterDensity.scrollTop +
      " | wobble " +
      wobbleSteps.join(" ") +
      " | j rowindex " +
      before.rowindex +
      "->" +
      moved.rowindex +
      " scrolled " +
      before.scrollTop +
      "->" +
      moved.scrollTop,
  );
}

/**
 * THE READING PANE CLOSES IN ONE STEP, HOWEVER MANY MESSAGES IT SHOWED.
 *
 * Reported on v1.86.0: Escape and the pane's ← "skip me between emails I've
 * previously opened, in the reverse order to my opening them". Every opening
 * pushed a history entry and closing is one step back, so closing C showed B.
 * That is a claim about the browser's session history, which is why it is
 * checked here and not only in jsdom (web/src/app/ui/routes/
 * DeskRoute.clickAndClose.test.tsx has the route-level half). Exit 18.
 */
async function paneClosesInOneStep(page) {
  const results = [];
  for (const how of ["Escape", "close button"]) {
    await load(page, "#/inbox/review");
    await waitForDesk(page);
    const rows = await page.evaluate(() =>
      [...document.querySelectorAll("#desk-grid li.desk-item-row")].slice(0, 3).map((li) => li.id),
    );
    if (rows.length < 3) {
      throw new Fail(14, "NOFIXTURE pane-close: " + rows.length + " message rows on the desk, need 3");
    }
    const before = await page.evaluate(() => window.history.length);
    for (const rid of rows) {
      await page.click('[id="' + rid + '"] .desk-main');
      await until("pane-close: " + rid + " is open", 15000, () =>
        page.evaluate((r) => {
          const m = /[?&]m=([^&]+)/.exec(window.location.hash);
          return !!m && r.endsWith(m[1]) && !!document.querySelector(".thread-pane");
        }, rid),
      18);
    }
    const after = await page.evaluate(() => window.history.length);
    // ⚠ BOUNDED: Chromium keeps at most 50 entries per tab, and by this point in
    // the run the tab is usually at that cap, where a push no longer changes the
    // length. So this check can only fire below the cap, and the line says which
    // it was; the close below is the assertion that always runs (and went red,
    // exit 18, on the image without the fix).
    const capped = before >= 49;
    if (!capped && after > before + 1) {
      throw new Fail(
        18,
        "PANECLOSE history: opening three messages one after another added " +
          (after - before) +
          " history entries, want 1 — each would be a step Escape and Back walk through",
      );
    }
    if (how === "Escape") await page.keyboard.press("Escape");
    else await page.click(".thread-pane .pane-back");
    const describe = () =>
      page.evaluate(
        () => window.location.hash + " pane=" + !!document.querySelector(".thread-pane"),
      );
    await until(
      "pane-close: " + how + " closed the pane",
      15000,
      () =>
        page.evaluate(
          () => !/[?&]m=/.test(window.location.hash) && !document.querySelector(".thread-pane"),
        ),
      18,
      describe,
    );
    // And it STAYS closed: the old shape showed the previous message a moment
    // after the hash changed, on the same back() the close was.
    await sleep(800);
    const still = await page.evaluate(
      () => !/[?&]m=/.test(window.location.hash) && !document.querySelector(".thread-pane"),
    );
    if (!still) {
      throw new Fail(18, "PANECLOSE " + how + ": the pane closed and came back: " + (await describe()));
    }
    results.push(
      how + " ok (history " + (capped ? "at the cap, not measured" : "+" + (after - before)) + ")",
    );
  }
  tally.paneClose = 1;
  console.log("pane-close        opened 3, then " + results.join("; "));
}


/**
 * MOVE TO AREA, AXE-CLEAN AND KEYBOARD-OPERABLE (#708).
 *
 * Two surfaces that only exist after an interaction, so they are not in the
 * surface matrix: the classic list's "Move to area ▾" menu with a row selected,
 * and the pinned sender popover's confirm step. Each is run through axe (AA) in
 * the light theme with the thing OPEN, and each asserts the computed facts a
 * linter cannot: the accessible name carries the visible words (WCAG 2.5.3), the
 * menu is a `menu` of `menuitem`s, Escape returns focus to the button, and the
 * confirm's buttons are findable by role and name.
 *
 * ⚠ THE SENDER STEP CANCELS. Confirming would re-file mail that every later step
 * of this lane (and the shared mailbox) relies on being where the fixture put
 * it. The move itself is covered by the vitest suites; this proves the surface.
 * Exits 19 and 20.
 */
async function moveToAreaSurfaces(page) {
  // ---- 1. the classic list -------------------------------------------------
  await load(page, "#/all");
  await page.evaluate(() => {
    window.__moveBtn = () =>
      [...document.querySelectorAll(".list-toolbar button")].find((n) =>
        /^Move to area/.test((n.textContent || "").trim()),
      );
  });
  await waitForSelectorD(page, "ul.messages", { timeout: 30000 });
  await until(
    "move-menu: the list has rows",
    30000,
    () => page.evaluate(() => document.querySelectorAll("ul.messages > li").length > 0),
    14,
  );
  await injectAxe(page);
  await page.click("ul.messages > li input[type=checkbox]");
  await until(
    "move-menu: the selection toolbar offers Move to area",
    15000,
    () => page.evaluate(() => !!window.__moveBtn()),
    19,
  );
  const info = await page.evaluate(() => {
    const b = window.__moveBtn();
    return { label: b.getAttribute("aria-label"), text: (b.textContent || "").trim(), haspopup: b.getAttribute("aria-haspopup") };
  });
  // WCAG 2.5.3: the accessible name is the content unless an aria-label replaces
  // it, and a replacement must still contain the visible words.
  const name = info.label || info.text;
  if (name.indexOf("Move to area") < 0) {
    throw new Fail(19, "MOVEMENU name: accessible name " + JSON.stringify(name) + " does not contain 'Move to area'");
  }
  if (info.haspopup !== "menu") {
    throw new Fail(19, "MOVEMENU popup: aria-haspopup is " + JSON.stringify(info.haspopup) + ", want menu");
  }
  await page.evaluate(() => window.__moveBtn().click());
  await until(
    "move-menu: the menu opened",
    15000,
    () => page.evaluate(() => document.querySelectorAll('.list-toolbar [role="menu"] [role="menuitem"]').length > 0),
    19,
  );
  await axeAA(page, "move-to-area-menu-open", "light");
  const roles = await page.evaluate(() => ({
    menus: document.querySelectorAll('[role="menu"]').length,
    items: document.querySelectorAll('[role="menu"] [role="menuitem"]').length,
    expanded: window.__moveBtn().getAttribute("aria-expanded"),
  }));
  if (roles.menus < 1 || roles.items < 1 || roles.expanded !== "true") {
    throw new Fail(19, "MOVEMENU roles: " + JSON.stringify(roles) + ", want a menu with >=1 menuitem and aria-expanded=true");
  }
  await page.keyboard.press("Escape");
  await until(
    "move-menu: Escape closed the menu and focus is back on the button",
    15000,
    () =>
      page.evaluate(() => {
        const a = document.activeElement;
        return (
          !document.querySelector('[role="menu"]') &&
          !!a &&
          a.tagName === "BUTTON" &&
          /^Move to area/.test((a.textContent || "").trim())
        );
      }),
    19,
    () =>
      page.evaluate(
        () =>
          "active=" +
          (document.activeElement
            ? document.activeElement.tagName + ":" + (document.activeElement.textContent || "").slice(0, 30)
            : "none"),
      ),
  );
  tally.moveMenu = 1;

  // ---- 2. the sender card's confirm step ----------------------------------
  // Opened from All mail: the announcement script has archived this message by
  // now, so it is no longer on the desk (see openTheAttachmentMessage).
  await openTheAttachmentMessage(page, "all");
  await injectAxe(page);
  await page.click("button.message-from-button");
  await waitForSelectorD(page, '.sender-popover[role="dialog"]', { timeout: 30000 });
  // The count is a `from:` search, so the index must have caught up with the
  // fixture (MAILROOM_INDEX_INTERVAL): until it has, the card answers "no
  // messages" and closes the menu. Re-open and re-pick until the confirm is up.
  const popoverButtons = () =>
    page.evaluate(() =>
      [...document.querySelectorAll('.sender-popover[role="dialog"] button')].map((n) => (n.textContent || "").trim()),
    );
  let area = null;
  await until(
    "move-sender: the confirm step is up",
    60000,
    async () => {
      const names = await popoverButtons();
      // The confirm step: Cancel and "Move to <area>" by role and NAME, not by class.
      if (area && names.indexOf("Cancel") >= 0 && names.indexOf("Move to " + area) >= 0) return true;
      const picked = await page.evaluate(() => {
        const pop = '.sender-popover[role="dialog"]';
        const items = [...document.querySelectorAll(pop + ' [role="menuitem"]')];
        if (items.length === 0) {
          const b = [...document.querySelectorAll(pop + " button")].find(
            (n) => /^Move all from .* to…/.test((n.textContent || "").trim()) && !n.disabled,
          );
          if (b) b.click();
          return null;
        }
        const it = items.find((n) => !n.disabled);
        if (!it) return { blocked: true };
        it.click();
        return { name: (it.textContent || "").trim() };
      });
      if (picked && picked.blocked) throw new Fail(20, "MOVESENDER area: every area in the menu is disabled");
      if (picked) area = picked.name;
      return false;
    },
    20,
    async () =>
      (await popoverButtons()).join(" | ") +
      " | toasts: " +
      (await page.evaluate(() => [...document.querySelectorAll(".toast-message")].map((n) => (n.textContent || "").trim()).join(" / "))),
  );
  await axeAA(page, "sender-move-confirm", "light");
  // CANCEL, never confirm (see the header): the mailbox is shared with later steps.
  await page.evaluate(() => {
    [...document.querySelectorAll('.sender-popover[role="dialog"] button')]
      .find((n) => (n.textContent || "").trim() === "Cancel")
      .click();
  });
  await until(
    "move-sender: Cancel closed the confirm step",
    15000,
    async () => (await popoverButtons()).indexOf("Cancel") < 0,
    20,
  );
  tally.moveSender = 1;
  console.log("move-to-area      list menu ok (name, menu roles, Escape focus); sender confirm ok via '" + area + "', cancelled");
}

/**
 * THE SEARCH BOX: THE SUGGESTION COMBOBOX AND THE OPTIONS FORM (#724).
 *
 * Both exist only after an interaction, so like the move-to-area surfaces they
 * are not in the surface matrix: each is opened, run through axe (AA, light),
 * and asserted on the facts a linter cannot see.
 *
 *   * the combobox: the input's COMPUTED role is combobox and it is expanded
 *     while the list shows; aria-controls names the listbox; nothing is active
 *     until ArrowDown, which makes the first option the active descendant and
 *     selected; Escape collapses it, keeps focus, and leaves the text alone;
 *   * the form: in the page (no dialog), focus lands on its first field, every
 *     control's COMPUTED name is its visible label (WCAG 2.5.3), Escape closes
 *     it and focus returns to the button that opened it;
 *   * the form at PHONE WIDTH (360 px): reopened there, it lies wholly on
 *     screen and so does every control in it — the issue's "works at phone
 *     width", measured rather than assumed — and it is axe-clean there too.
 *
 * It types `from:` and nothing more: an operator with no value is not a
 * complete term, so live search does not run (core/searchQuery.ts liveQueryOf)
 * and the list under the box stays the one the fixture built. Exit 21.
 */
async function searchBoxSurfaces(page, cdp) {
  await load(page, "#/all");
  await waitForSelectorD(page, "ul.messages", { timeout: 30000 });
  await until(
    "search-box: the list has rows (the senders the suggestions draw on)",
    30000,
    () => page.evaluate(() => document.querySelectorAll("ul.messages > li").length > 0),
    14,
  );
  await injectAxe(page);

  // ---- 1. the suggestion combobox -----------------------------------------
  await page.click("input.search-input");
  // ONE input event, not five keystrokes: on a slow runner a pause after "from"
  // longer than SEARCH_DEBOUNCE_MS would run a live search for the bare word and
  // replace the list the suggestions are drawn from.
  await page.keyboard.sendCharacter("from:");
  const comboState = () =>
    page.evaluate(() => {
      const input = document.querySelector("input.search-input");
      const listId = input.getAttribute("aria-controls");
      const list = listId ? document.getElementById(listId) : null;
      return {
        expanded: input.getAttribute("aria-expanded"),
        listRole: list ? list.getAttribute("role") : null,
        options: list ? list.querySelectorAll('[role="option"]').length : 0,
        active: input.getAttribute("aria-activedescendant"),
        value: input.value,
        focused: document.activeElement === input,
      };
    });
  await until(
    "search-box: from: lists senders",
    15000,
    async () => (await comboState()).options > 0,
    21,
    async () => JSON.stringify(await comboState()),
  );
  const combo = await axCDP(cdp, "input.search-input");
  const open1 = await comboState();
  if (!combo || combo.role !== "combobox" || combo.props.expanded !== true) {
    throw new Fail(21, "SEARCHBOX combobox: computed " + JSON.stringify(combo) + ", want role combobox, expanded");
  }
  if (open1.listRole !== "listbox" || open1.active) {
    throw new Fail(21, "SEARCHBOX listbox: " + JSON.stringify(open1) + ", want aria-controls -> listbox and NO active option yet");
  }
  await page.keyboard.press("ArrowDown");
  const moved = await page.evaluate(() => {
    const input = document.querySelector("input.search-input");
    const id = input.getAttribute("aria-activedescendant");
    const opt = id ? document.getElementById(id) : null;
    return { id, role: opt && opt.getAttribute("role"), selected: opt && opt.getAttribute("aria-selected") };
  });
  if (!moved.id || moved.role !== "option" || moved.selected !== "true") {
    throw new Fail(21, "SEARCHBOX activedescendant: after ArrowDown " + JSON.stringify(moved) + ", want a selected option");
  }
  await axeAA(page, "search-suggestions-open", "light");
  await page.keyboard.press("Escape");
  await until(
    "search-box: Escape collapsed the list, kept focus and the text",
    15000,
    async () => {
      const s = await comboState();
      return s.expanded === "false" && s.focused && s.value === "from:";
    },
    21,
    async () => JSON.stringify(await comboState()),
  );

  // ---- 2. the options form -------------------------------------------------
  const BTN = 'button[aria-controls="search-options"]';
  const formState = () =>
    page.evaluate((btnSel) => {
      const form = document.getElementById("search-options");
      const btn = document.querySelector(btnSel);
      const a = document.activeElement;
      return {
        open: !!form,
        tag: form ? form.tagName : null,
        dialogs: document.querySelectorAll('dialog, [role="dialog"], [role="alertdialog"]').length,
        expanded: btn ? btn.getAttribute("aria-expanded") : null,
        active: a ? a.tagName + "#" + (a.id || "") + ":" + (a.getAttribute("aria-label") || "") : "none",
        onButton: a === btn,
      };
    }, BTN);
  await page.click(BTN);
  await until(
    "search-box: the options form opened with focus on its first field",
    15000,
    async () => {
      const s = await formState();
      return s.open && s.active === "INPUT#search-options-from:";
    },
    21,
    async () => JSON.stringify(await formState()),
  );
  const f1 = await formState();
  if (f1.tag !== "FORM" || f1.expanded !== "true") {
    throw new Fail(21, "SEARCHBOX form: " + JSON.stringify(f1) + ", want an in-page FORM and aria-expanded=true");
  }
  // Only the app's own dialogs could be open here, and none should be.
  if (f1.dialogs !== 0) throw new Fail(21, "SEARCHBOX form: a dialog is open beside it: " + JSON.stringify(f1));
  const controls = await page.evaluate(() =>
    [...document.querySelectorAll("#search-options input, #search-options select")].map((c) => ({
      id: c.id,
      label: c.labels && c.labels[0] ? (c.labels[0].textContent || "").trim() : "",
    })),
  );
  if (controls.length < 11) throw new Fail(21, "SEARCHBOX form: " + controls.length + " controls, want 11");
  for (const c of controls) {
    const ax = await axCDP(cdp, "#" + c.id);
    const name = (ax && ax.name) || "";
    if (!c.label || name.indexOf(c.label) < 0) {
      throw new Fail(21, "SEARCHBOX name: #" + c.id + " computed name " + JSON.stringify(name) + ", visible label " + JSON.stringify(c.label));
    }
  }
  await axeAA(page, "search-options-open", "light");
  await page.keyboard.press("Escape");
  await until(
    "search-box: Escape closed the form and focus is back on its button",
    15000,
    async () => {
      const s = await formState();
      return !s.open && s.onButton && s.expanded === "false";
    },
    21,
    async () => JSON.stringify(await formState()),
  );

  // ---- 3. the form at phone width -----------------------------------------
  await page.setViewport({ width: 360, height: 740 });
  try {
    // Below 400 px the box's options button is clipped until the box has focus
    // (#802), so a phone user focuses the box first, exactly as here.
    await page.focus("input.search-input");
    await page.click(BTN);
    await until("search-box: the form opened at 360px", 15000, async () => (await formState()).open, 21, async () =>
      JSON.stringify(await formState()),
    );
    const fit = await page.evaluate(() => {
      const w = window.innerWidth;
      const form = document.getElementById("search-options").getBoundingClientRect();
      const off = [...document.querySelectorAll("#search-options input, #search-options select, #search-options button")]
        .map((c) => ({ id: c.id || c.textContent, r: c.getBoundingClientRect() }))
        .filter((c) => c.r.width <= 0 || c.r.left < 0 || c.r.right > w + 0.5)
        .map((c) => c.id + "@" + Math.round(c.r.left) + ".." + Math.round(c.r.right));
      return { w, left: form.left, right: form.right, top: form.top, off };
    });
    if (fit.left < 0 || fit.right > fit.w + 0.5 || fit.top < 0 || fit.off.length > 0) {
      throw new Fail(21, "SEARCHBOX phone: at " + fit.w + "px the form or a control is off screen: " + JSON.stringify(fit));
    }
    await axeAA(page, "search-options-phone", "light");
    await page.keyboard.press("Escape");
  } finally {
    await page.setViewport({ width: 1680, height: 1100 });
  }
  tally.searchBox = 1;
  console.log(
    "search-box        combobox ok (role, expanded, activedescendant, Escape); options form ok (" +
      controls.length +
      " named controls, focus in/out, fits 360px)",
  );
}

/**
 * SETTINGS › NOTIFICATIONS AND › SENDERS (#59).
 *
 * Both rows are opened by URL, run through axe (AA, light), and then asked the
 * one thing axe's AA rules do not check: that every control's COMPUTED name
 * contains the words printed beside it (WCAG 2.5.3, label in name). axe's rule
 * for that, `label-content-name-mismatch`, is experimental and off in the
 * wcag2a/aa set this lane gates on, so a control whose aria-label replaced its
 * label would pass axe here while a speech user saying "allow VIPs" missed it.
 * The quiet-hours days are also asserted to be a NAMED group, the structure a
 * screen reader announces before the first day.
 *
 * ⚠ THE NEGATIVE CONTROL RUNS IN THE SAME STEP: an aria-label that does not
 * contain the visible words is put on "Allow VIPs during quiet hours", and
 * the same check must go red on exactly that control. A defect axe's AA set
 * cannot see, by construction — which is the point of this check existing.
 * Exit 22.
 */
async function labelInNameProblems(page, cdp, scope) {
  const controls = await page.evaluate((sel) => {
    let n = 0;
    return [...document.querySelectorAll(sel + " input, " + sel + " select")].map((c) => {
      if (!c.id) c.id = "a11y-notify-" + n++;
      const label = c.closest("label");
      const words = label
        ? [...label.childNodes]
            .filter((k) => k !== c)
            .map((k) => (k.textContent || "").trim())
            .join(" ")
            .trim()
        : "";
      return { id: c.id, label: words };
    });
  }, scope);
  const bad = [];
  for (const c of controls) {
    const ax = await axCDP(cdp, "#" + c.id);
    const name = ((ax && ax.name) || "").trim();
    if (!c.label || name.indexOf(c.label) < 0) bad.push({ id: c.id, label: c.label, name });
  }
  return { count: controls.length, bad };
}

async function notifySurfaces(page, cdp) {
  await load(page, "#/settings/notifications");
  await waitForSelectorD(page, "form.quiet-hours", { timeout: 30000 });
  await injectAxe(page);
  await axeAA(page, "settings-notifications", "light");
  const quiet = await labelInNameProblems(page, cdp, "form.quiet-hours");
  if (quiet.count < 12) throw new Fail(22, "NOTIFY quiet-hours: " + quiet.count + " controls, want >= 12 (on, from, until, 7 days, zone, VIPs)");
  if (quiet.bad.length > 0) throw new Fail(22, "NOTIFY name: " + JSON.stringify(quiet.bad));
  const days = await axCDP(cdp, "fieldset.quiet-hours-days");
  if (!days || days.role !== "group" || !days.name) {
    throw new Fail(22, "NOTIFY days: computed " + JSON.stringify(days) + ", want a named group");
  }

  // The in-run negative control (see the header).
  await page.evaluate(() => {
    const box = [...document.querySelectorAll("form.quiet-hours label")]
      .find((l) => (l.textContent || "").trim() === "Allow VIPs during quiet hours")
      .querySelector("input");
    box.setAttribute("aria-label", "Toggle");
  });
  const broken = await labelInNameProblems(page, cdp, "form.quiet-hours");
  tally.controlsRun += 1;
  if (!broken.bad.some((b) => b.label === "Allow VIPs during quiet hours")) {
    throw new Fail(22, "NOTIFY CONTROLPASSED label-in-name — an aria-label replacing the visible words went unseen: " + JSON.stringify(broken));
  }
  tally.controlsTripped += 1;
  console.log("control notify-label-in-name tripped=yes");

  // Per-account switches, when the fixture's accounts have answered.
  await load(page, "#/settings/notifications");
  await waitForSelectorD(page, "form.quiet-hours", { timeout: 30000 });
  const accounts = await labelInNameProblems(page, cdp, ".settings-page fieldset.menu-group");
  if (accounts.bad.length > 0) throw new Fail(22, "NOTIFY name (row): " + JSON.stringify(accounts.bad));

  // Settings › Senders is two links into Contacts since #816 (its "mark an
  // address" form moved there, and CONTACTS asserts that page).
  await load(page, "#/settings/senders");
  await waitForSelectorD(page, ".senders-links a", { timeout: 30000 });
  await injectAxe(page);
  await axeAA(page, "settings-senders", "light");
  const senders = await page.evaluate(() =>
    [...document.querySelectorAll(".senders-links a")].map((a) => [a.textContent.trim(), a.getAttribute("href")]),
  );
  if (senders.length !== 2 || senders[0][1] !== "#/contacts?filter=vip" || senders[1][1] !== "#/contacts?filter=blocked") {
    throw new Fail(22, "NOTIFY senders: the links into Contacts are " + JSON.stringify(senders));
  }
  // Settings › Signed-in devices (#937). This fixture signs in with dev-auth,
  // which has no sessions, so the row shows its development-mode sentence: the
  // page is still opened by URL and put through axe in a real browser, where
  // jsdom's vitest pass cannot check contrast or target size. The loaded list
  // needs a real OIDC session and is covered by vitest (DevicesRow.test.tsx).
  await load(page, "#/settings/devices");
  await waitForSelectorD(page, ".devices-none", { timeout: 30000 });
  await injectAxe(page);
  await axeAA(page, "settings-devices", "light");

  tally.notify = 1;
  console.log(
    "notify            quiet hours " + quiet.count + " controls named by their labels, days a named group; row " +
      accounts.count + " controls; senders ok; label-in-name control tripped",
  );
}

/**
 * SETTINGS › CLEAN UP (#53).
 *
 * Opened by URL and run through axe (AA, light). Then two facts axe does not
 * check: every control of the ranking form is named by its visible label
 * (labelInNameProblems, as NOTIFY), and every row's ACTION GROUP is named by
 * its row — the buttons say only "Archive all", "Block…", and it is the group's
 * name that tells a screen-reader user whose mail they are about to move. axe
 * does not require a role=group to have a name, so a group that lost its
 * aria-labelledby passes axe and leaves every row's buttons indistinguishable.
 * The in-run negative control strips exactly that from the first row and
 * requires the check to go red. Needs the fixture's mail to have produced at
 * least one ranked row; a ranking with none is NOFIXTURE, not a pass. Exit 23.
 */
async function cleanupGroupProblems(page, cdp) {
  const ids = await page.evaluate(() => {
    let n = 0;
    return [...document.querySelectorAll("li.cleanup-item .cleanup-actions")].map((g) => {
      if (!g.id) g.id = "a11y-cleanup-" + n++;
      const nameEl = g.closest("li").querySelector(".senders-name");
      return { id: g.id, row: nameEl ? (nameEl.textContent || "").trim() : "" };
    });
  });
  const bad = [];
  for (const g of ids) {
    const ax = await axCDP(cdp, "#" + g.id);
    if (!ax || ax.role !== "group" || !ax.name || ax.name.indexOf(g.row) < 0) bad.push({ id: g.id, row: g.row, ax });
  }
  return { count: ids.length, bad };
}

async function cleanupSurfaces(page, cdp) {
  await load(page, "#/settings/cleanup");
  await waitForSelectorD(page, "form.cleanup-controls", { timeout: 30000 });
  // All time, so the fixture's mail ranks whatever dates it was built with.
  await page.select("form.cleanup-controls select", "all");
  await until(
    "cleanup: the ranking has rows",
    30000,
    () => page.evaluate(() => document.querySelectorAll("li.cleanup-item .cleanup-actions").length > 0),
    14,
    () => page.evaluate(() => (document.querySelector(".settings-page [role=status]") || {}).textContent || "no status"),
  );
  await injectAxe(page);
  await axeAA(page, "settings-cleanup", "light");
  const controls = await labelInNameProblems(page, cdp, "form.cleanup-controls");
  if (controls.count < 6 || controls.bad.length > 0) {
    throw new Fail(23, "CLEANUP controls: " + JSON.stringify(controls) + ", want >= 6 named by their labels");
  }
  const groups = await cleanupGroupProblems(page, cdp);
  if (groups.bad.length > 0) throw new Fail(23, "CLEANUP group: " + JSON.stringify(groups.bad));

  // The in-run negative control (see the header).
  await page.evaluate(() => {
    document.querySelector("li.cleanup-item .cleanup-actions").removeAttribute("aria-labelledby");
  });
  const broken = await cleanupGroupProblems(page, cdp);
  tally.controlsRun += 1;
  if (broken.bad.length === 0) {
    throw new Fail(23, "CLEANUP CONTROLPASSED group-name — an action group with no name went unseen");
  }
  tally.controlsTripped += 1;
  console.log("control cleanup-group-name tripped=yes");
  tally.cleanup = 1;
  console.log(
    "cleanup           " + controls.count + " controls named by their labels; " + groups.count +
      " row action groups named by their rows; group-name control tripped",
  );
}

/**
 * THE CALENDAR INVITATION CARD (#725).
 *
 * Opened by id (the fixture filed it in Archive, off the desk) and run through
 * axe (AA, light). Then what axe cannot judge: that the card is a region named
 * for the event, that the answers are a NAMED group of three buttons named by
 * their words, and — the one that matters — that each button's `aria-pressed`
 * IS the answer the server has recorded. A pressed state that lies is valid
 * ARIA, so axe passes it; a screen-reader user is then told they accepted a
 * meeting they did not, or not told they did.
 *
 * The sequence: nothing pressed; the in-run NEGATIVE CONTROL (Accept's
 * aria-pressed forced to "true" while the server has no answer — the check must
 * go red); reload; press Accept and wait for the SERVER's answer to press it
 * (the card reloads the thread, so this is the recorded state, not the click);
 * then the toast's Undo, after which nothing is pressed again. The Undo lands
 * inside the server's Undo Send window, so nothing reaches SMTP. Exit 24.
 */
/**
 * A FOLDED MESSAGE, ON PURPOSE (#759).
 *
 * The fixture filed a two-message conversation in Archive and marked the first
 * one read, so opening the second folds the first to a one-line button. axe
 * (AA, light) runs over that pane — the surface that used to reach the lane
 * only by accident, behind another step's overlay, where axe flagged
 * label-content-name-mismatch on it. Then the fact itself, read from the
 * accessibility tree: each folded button's computed NAME contains the text it
 * SHOWS (WCAG 2.5.3 Label in Name — a speech-input user says what they see).
 * Whitespace is ignored on both sides, because the name is computed from
 * inline spans and the point is the words, not their spacing.
 *
 * The in-run negative control puts back the defect: an aria-label that names
 * the sender but not the preview. The check must go red. Exit 27.
 */
async function foldedNameProblems(page, cdp) {
  const ids = await page.evaluate(() => {
    let n = 0;
    return [...document.querySelectorAll("button.message-folded")].map((b) => {
      if (!b.id) b.id = "a11y-folded-" + n++;
      return { id: b.id, visible: (b.innerText || b.textContent || "").trim() };
    });
  });
  const squash = (s) => (s || "").replace(/\s+/g, "").toLowerCase();
  const bad = [];
  for (const b of ids) {
    const ax = await axCDP(cdp, "#" + b.id);
    if (!ax || ax.role !== "button" || !b.visible || squash(ax.name).indexOf(squash(b.visible)) < 0) {
      bad.push({ id: b.id, visible: b.visible, name: ax && ax.name, role: ax && ax.role });
    }
  }
  return { count: ids.length, bad };
}

async function foldedSurfaces(page, cdp) {
  await load(page, "#/all?m=" + FIX.folded.openId);
  await waitForSelectorD(page, "aside.thread-pane", { timeout: 30000 });
  await until(
    "folded: the pane folds the read message",
    30000,
    () => page.evaluate(() => document.querySelectorAll("button.message-folded").length > 0),
    27,
    () => page.evaluate(() => (document.querySelector("aside.thread-pane") || {}).innerText || "no pane"),
  );
  await injectAxe(page);
  await axeAA(page, "reading-pane-folded", "light");
  const ok = await foldedNameProblems(page, cdp);
  if (ok.count < 1 || ok.bad.length > 0) {
    throw new Fail(27, "FOLDED name: " + JSON.stringify(ok));
  }

  // The in-run negative control (see above).
  await page.evaluate(() => {
    document.querySelector("button.message-folded").setAttribute("aria-label", "Show the message from Grace Hopper");
  });
  const broken = await foldedNameProblems(page, cdp);
  tally.controlsRun += 1;
  if (broken.bad.length === 0) {
    throw new Fail(27, "FOLDED CONTROLPASSED label-in-name — a folded button named without its visible preview went unseen");
  }
  tally.controlsTripped += 1;
  console.log("control folded-label-in-name tripped=yes");
  tally.folded = 1;
  console.log("folded            " + ok.count + " folded button(s) named by their visible text; label-in-name control tripped");
}

async function invitePressedProblems(cdp, want) {
  const bad = [];
  const names = ["Accept", "Maybe", "Decline"];
  for (let i = 0; i < names.length; i++) {
    const ax = await axCDP(cdp, "section.invite-card .invite-actions button:nth-of-type(" + (i + 1) + ")");
    if (!ax || ax.role !== "button" || ax.name !== names[i]) {
      bad.push({ want: names[i], got: ax && { role: ax.role, name: ax.name } });
      continue;
    }
    const pressed = String(ax.props.pressed);
    const expect = want === names[i] ? "true" : "false";
    if (pressed !== expect) bad.push({ button: names[i], pressed, want: expect });
  }
  return bad;
}

async function openTheInvitation(page) {
  await load(page, "#/all?m=" + FIX.invite.id);
  await waitForSelectorD(page, "aside.thread-pane", { timeout: 30000 });
  await waitForSelectorD(page, "section.invite-card .invite-actions button", { timeout: 30000 });
}

async function inviteSurfaces(page, cdp) {
  await openTheInvitation(page);
  await injectAxe(page);
  await axeAA(page, "reading-pane-invitation", "light");

  const region = await axCDP(cdp, "section.invite-card");
  const wantRegion = "Calendar invitation: " + FIX.invite.title;
  if (!region || region.role !== "region" || region.name !== wantRegion) {
    throw new Fail(24, "INVITE region: computed " + JSON.stringify(region) + ', want region "' + wantRegion + '"');
  }
  const group = await axCDP(cdp, "section.invite-card .invite-actions");
  if (!group || group.role !== "group" || group.name !== "Respond to this invitation") {
    throw new Fail(24, "INVITE group: computed " + JSON.stringify(group) + ', want group "Respond to this invitation"');
  }
  let bad = await invitePressedProblems(cdp, null);
  if (bad.length > 0) throw new Fail(24, "INVITE before answering: " + JSON.stringify(bad));

  // The in-run negative control (see above).
  await page.evaluate(() => {
    document.querySelector("section.invite-card .invite-actions button").setAttribute("aria-pressed", "true");
  });
  const broken = await invitePressedProblems(cdp, null);
  tally.controlsRun += 1;
  if (!broken.some((b) => b.button === "Accept")) {
    throw new Fail(24, "INVITE CONTROLPASSED pressed — an Accept claiming to be pressed with no answer went unseen");
  }
  tally.controlsTripped += 1;
  console.log("control invite-pressed tripped=yes");

  await openTheInvitation(page);
  await page.click("section.invite-card .invite-actions button:nth-of-type(1)");
  await untilToast(page, "invite: the toast says the answer is on its way", "your answer is on its way");
  await until(
    "invite: the SERVER's answer presses Accept",
    30000,
    async () => (await invitePressedProblems(cdp, "Accept")).length === 0,
    24,
    async () => JSON.stringify(await invitePressedProblems(cdp, "Accept")),
  );

  // Undo, through the toast's own button, inside the hold window.
  await page.click(".toast-undo .toast-undo-action");
  await until(
    "invite: after Undo nothing is pressed",
    30000,
    async () => (await invitePressedProblems(cdp, null)).length === 0,
    24,
    async () => JSON.stringify(await invitePressedProblems(cdp, null)),
  );
  tally.invite = 1;
  console.log(
    "invite            region and group named; Accept/Maybe/Decline pressed = the server's answer " +
      "(none, Accept, none after Undo); pressed control tripped",
  );
}

/**
 * THE DESK'S FOLLOW UP STRIP (#58).
 *
 * The fixture's one sent message (scripts/a11y-e2e.sh, backdated ten days and
 * unanswered) makes "no one answered your mail" due, so the strip is drawn.
 * It is waited for — FollowUp/get answers after the desk's own load — and then
 * run through axe (AA, and the AAA contrast pass) in the light theme. Then the
 * fact axe does not check: each row's action GROUP is named by its row, because
 * "Done" alone does not say which conversation it closes, and axe does not
 * require a role=group to have a name. The in-run negative control strips the
 * name from the first group and requires the check to go red. Last, Done is
 * pressed and must take the row away AND keep it away across a reload (the
 * dismissal is stored server-side). A strip that never appears is NOFIXTURE,
 * not a pass. Exit 25.
 */
async function followUpGroupProblems(page, cdp) {
  const ids = await page.evaluate(() =>
    [...document.querySelectorAll("li.desk-followup-item")].map((li, n) => {
      const g = li.querySelector(".desk-followup-actions");
      if (!g.id) g.id = "a11y-followup-" + n;
      const row = li.querySelector(".desk-followup-open");
      return { id: g.id, row: row ? (row.textContent || "").trim() : "" };
    }),
  );
  const bad = [];
  for (const g of ids) {
    const ax = await axCDP(cdp, "#" + g.id);
    // The group's name must BE its row's words (the row's subject is part of
    // them), not merely exist.
    // Whitespace-blind: Chromium may join the row's two lines with a space
    // that textContent does not have.
    const flat = (s) => String(s || "").replace(/\s+/g, "");
    if (!ax || ax.role !== "group" || flat(ax.name).length < 10 || flat(g.row).indexOf(flat(ax.name)) < 0) {
      bad.push({ id: g.id, row: g.row, ax });
    }
  }
  return { count: ids.length, bad };
}

async function followUpSurfaces(page, cdp) {
  await load(page, "#/inbox/review");
  await waitForDesk(page);
  await until(
    "follow-up: the strip is drawn",
    45000,
    () => page.evaluate(() => document.querySelectorAll("section.desk-followup li.desk-followup-item").length > 0),
    25,
    () => page.evaluate(() => "NOFIXTURE no follow-up row; strip=" + !!document.querySelector("section.desk-followup")),
  );
  await injectAxe(page);
  await axeAA(page, "desk-followup", "light");
  await axeContrastAAA(page, "desk-followup", "light");

  const region = await axCDP(cdp, "section.desk-followup");
  if (!region || !region.name || region.name.indexOf("Follow up") < 0) {
    throw new Fail(25, "FOLLOWUP region: computed " + JSON.stringify(region) + ", want a region named Follow up");
  }
  const groups = await followUpGroupProblems(page, cdp);
  if (groups.count < 1 || groups.bad.length > 0) throw new Fail(25, "FOLLOWUP group: " + JSON.stringify(groups));

  // The in-run negative control (see the header).
  await page.evaluate(() => {
    document.querySelector("li.desk-followup-item .desk-followup-actions").removeAttribute("aria-labelledby");
  });
  const broken = await followUpGroupProblems(page, cdp);
  tally.controlsRun += 1;
  if (broken.bad.length === 0) {
    throw new Fail(25, "FOLLOWUP CONTROLPASSED group-name — an action group with no name went unseen");
  }
  tally.controlsTripped += 1;
  console.log("control followup-group-name tripped=yes");

  // Done: gone now, and still gone after a reload (stored, not hidden).
  await load(page, "#/inbox/review");
  await waitForDesk(page);
  await until(
    "follow-up: the strip is back after the control's reload",
    45000,
    () => page.evaluate(() => document.querySelectorAll("li.desk-followup-item").length > 0),
    25,
  );
  const before = await page.evaluate(() => document.querySelectorAll("li.desk-followup-item").length);
  await page.evaluate(() => {
    const done = [...document.querySelectorAll("li.desk-followup-item .desk-followup-actions button")].find(
      (b) => (b.textContent || "").trim() === "Done",
    );
    done.click();
  });
  await until(
    "follow-up: Done took the row away",
    30000,
    () => page.evaluate((n) => document.querySelectorAll("li.desk-followup-item").length === n - 1, before),
    25,
  );
  await load(page, "#/inbox/review");
  await waitForDesk(page);
  // Give FollowUp/get its turn before counting: the strip is drawn after it.
  await new Promise((r) => setTimeout(r, 3000));
  const after = await page.evaluate(() => document.querySelectorAll("li.desk-followup-item").length);
  if (after !== before - 1) {
    throw new Fail(25, "FOLLOWUP done: " + before + " rows before, " + after + " after a reload, want " + (before - 1));
  }
  tally.followUp = 1;
  console.log(
    "followup          strip axe-clean; " + groups.count + " row action group(s) named by their rows; " +
      "group-name control tripped; Done stayed done across a reload",
  );
}

/**
 * SETTINGS › OUT OF OFFICE (#50).
 *
 * Opened by URL and run through axe (AA, light). Then the facts axe does not
 * check. Each account's reply is a FORM NAMED BY ITS ACCOUNT (aria-labelledby
 * its heading): a user with two accounts hears "Main, form" and "Work, form",
 * and with the name gone every one of them is just "form" holding the same
 * eight controls. axe does not require a form to have a name, so losing it
 * passes axe. Every control is named by its VISIBLE label, whether the label
 * wraps it or points at it with for=. Each form carries a role=status line
 * (where drift is announced). The in-run negative control strips the first
 * form's name and requires the check to go red. Needs the fixture to have at
 * least one mail account; an empty row is NOFIXTURE, not a pass. Exit 26.
 */
async function oofProblems(page, cdp) {
  const found = await page.evaluate(() => {
    let n = 0;
    const forms = [...document.querySelectorAll("form.oof-account")].map((f) => {
      if (!f.id) f.id = "a11y-oof-form-" + n++;
      const h = f.querySelector("h3");
      const controls = [...f.querySelectorAll("input, select, textarea")].map((c) => {
        if (!c.id) c.id = "a11y-oof-c-" + n++;
        const wrap = c.closest("label");
        const pointed = document.querySelector('label[for="' + c.id + '"]');
        const label = pointed
          ? (pointed.textContent || "").trim()
          : wrap
            ? [...wrap.childNodes].filter((k) => k !== c).map((k) => (k.textContent || "").trim()).join(" ").trim()
            : "";
        return { id: c.id, label };
      });
      return {
        id: f.id,
        account: h ? (h.textContent || "").trim() : "",
        controls,
        status: !!f.querySelector('[role="status"]'),
      };
    });
    return forms;
  });
  const bad = [];
  let controls = 0;
  for (const f of found) {
    const ax = await axCDP(cdp, "#" + f.id);
    if (!ax || ax.role !== "form" || !ax.name || ax.name.indexOf(f.account) < 0 || !f.account) {
      bad.push({ form: f.id, account: f.account, ax });
    }
    if (!f.status) bad.push({ form: f.id, status: "missing" });
    for (const c of f.controls) {
      controls += 1;
      const cax = await axCDP(cdp, "#" + c.id);
      const name = ((cax && cax.name) || "").trim();
      if (!c.label || name.indexOf(c.label) < 0) bad.push({ id: c.id, label: c.label, name });
    }
  }
  return { forms: found.length, controls, bad };
}

async function outOfOfficeSurfaces(page, cdp) {
  await load(page, "#/settings/out-of-office");
  await until(
    "out of office: an account's form is drawn",
    30000,
    () => page.evaluate(() => document.querySelectorAll("form.oof-account").length > 0),
    14,
    () => page.evaluate(() => (document.querySelector(".settings-page") || {}).textContent || "no settings page"),
  );
  await injectAxe(page);
  await axeAA(page, "settings-out-of-office", "light");
  const ok = await oofProblems(page, cdp);
  if (ok.forms < 1 || ok.controls < 8) {
    throw new Fail(26, "OOF " + ok.forms + " form(s), " + ok.controls + " controls, want >= 1 and >= 8");
  }
  if (ok.bad.length > 0) throw new Fail(26, "OOF name: " + JSON.stringify(ok.bad));

  // The in-run negative control (see the header).
  await page.evaluate(() => {
    document.querySelector("form.oof-account").removeAttribute("aria-labelledby");
  });
  const broken = await oofProblems(page, cdp);
  tally.controlsRun += 1;
  if (!broken.bad.some((b) => b.form)) {
    throw new Fail(26, "OOF CONTROLPASSED form-name — an account's form with no name went unseen: " + JSON.stringify(broken));
  }
  tally.controlsTripped += 1;
  console.log("control oof-form-name tripped=yes");
  tally.oof = 1;
  console.log(
    "out-of-office     " + ok.forms + " account form(s) named by their accounts, each with a status line; " +
      ok.controls + " controls named by their labels; form-name control tripped",
  );
}

// ---------------------------------------------------------------------------
// SETTINGS › KEYS AND THE REVOKE DIALOG (#796, #795) — exit 29 KEYS
// ---------------------------------------------------------------------------
//
// The keys page manages things that cannot be taken back (revoking a key), so
// the parts a keyboard or screen-reader user depends on are asserted here
// rather than assumed: every control is named by its visible label, the revoke
// dialog holds focus, and its confirm button says it is unavailable (and why)
// until the fingerprint's last 8 characters are typed. Against the REAL crypto
// process (scripts/a11y-e2e.sh runs one): the key is made through the page's own
// Make a new key dialog, and a second, ten-day key over JMAP, so the desk's
// expiry banner has something true to say.
//
// TWO IN-RUN NEGATIVE CONTROLS, both of a kind axe cannot see on its own:
//   * the trap is cut (Tab never reaches the dialog's handler) and the trap
//     check must go red;
//   * the confirm button's aria-disabled is removed and the "unavailable until
//     it matches" check must go red.

async function keyControlNameProblems(page, cdp, scope) {
  const controls = await page.evaluate((sel) => {
    let n = 0;
    return [...document.querySelectorAll(sel + " input:not([type=hidden]), " + sel + " select, " + sel + " textarea")].map(
      (c) => {
        if (!c.id) c.id = "a11y-keys-" + n++;
        const pointed = document.querySelector('label[for="' + c.id + '"]');
        const wrap = c.closest("label");
        const label = pointed
          ? (pointed.textContent || "").trim()
          : wrap
            ? [...wrap.childNodes].filter((k) => k !== c).map((k) => (k.textContent || "").trim()).join(" ").trim()
            : "";
        return { id: c.id, label };
      },
    );
  }, scope);
  const bad = [];
  for (const c of controls) {
    const ax = await axCDP(cdp, "#" + c.id);
    // Whitespace collapsed on both sides, as the accessible-name computation
    // does: a label rebuilt from its text nodes joins them with spaces of its
    // own (the API keys dialogs' "Read only — …" labels, #49).
    const name = ((ax && ax.name) || "").replace(/\s+/g, " ").trim();
    const label = c.label.replace(/\s+/g, " ").trim();
    if (!label || name.indexOf(label) < 0) bad.push({ id: c.id, label: c.label, name });
  }
  return { count: controls.length, bad };
}

/** Tab forward and back through the dialog, more times than it has controls:
 * every focus must stay inside it. Returns the escapes it saw. */
async function revokeTrapProblems(page) {
  const count = await page.evaluate(
    () =>
      [...document.querySelectorAll('.key-revoke[role="dialog"] button, .key-revoke[role="dialog"] input')].filter(
        (e) => !e.disabled && e.offsetParent !== null,
      ).length,
  );
  const escapes = [];
  const where = () =>
    page.evaluate(() => {
      const a = document.activeElement;
      const d = document.querySelector('.key-revoke[role="dialog"]');
      return {
        inside: !!(a && d && d.contains(a)),
        what: a ? a.tagName.toLowerCase() + (a.className ? "." + String(a.className).split(" ")[0] : "") : "none",
      };
    });
  for (let i = 0; i < count + 2; i++) {
    await page.keyboard.press("Tab");
    const w = await where();
    if (!w.inside) escapes.push("Tab " + (i + 1) + " -> " + w.what);
  }
  for (let i = 0; i < count + 2; i++) {
    await page.keyboard.down("Shift");
    await page.keyboard.press("Tab");
    await page.keyboard.up("Shift");
    const w = await where();
    if (!w.inside) escapes.push("Shift+Tab " + (i + 1) + " -> " + w.what);
  }
  return { count, escapes };
}

async function revokeButtonState(cdp) {
  const ax = await axCDP(cdp, ".key-revoke-submit");
  return {
    found: !!ax,
    role: ax && ax.role,
    name: ax && ax.name,
    disabled: !!(ax && ax.props && ax.props["disabled"] === true),
    focusable: !!(ax && ax.props && ax.props["focusable"] === true),
    description: ax && ax.description,
  };
}

async function openRevokeDialog(page) {
  await load(page, "#/settings/keys");
  await waitForSelectorD(page, ".keys-page .own-key", { timeout: 30000 });
  await page.evaluate(() => {
    const b = [...document.querySelectorAll(".keys-page .own-key .own-key-actions button")].find(
      (x) => (x.textContent || "").trim() === "Revoke…",
    );
    b.setAttribute("data-a11y-opener", "1");
    // Focused first, as a pointer or keyboard press on it would leave it: a
    // bare element.click() moves no focus, and the dialog returns focus to
    // whatever had it when it opened.
    b.focus();
    b.click();
  });
  await waitForSelectorD(page, '.key-revoke[role="dialog"]', { timeout: 15000 });
}

// ---------------------------------------------------------------------------
// API keys, approvals and AI-tool setup (#49)
// ---------------------------------------------------------------------------
//
// Two fixture keys made over JMAP in the session, then used AS KEYS (bearer,
// no cookie) to leave two requests waiting: a held send of a draft whose
// subject is hostile (quotes, a right-to-left override and a fake
// "(+0 more)"), addressed To two, Cc one, Bcc one; and a held delete of two
// fixture messages. Nothing is ever approved here — every request is denied
// at the end (apiKeysCleanup) — so no later stage sees a moved message.
//
// Then: the row with both cards, the approve dialog (named, modal, focus in,
// Tab trapped), the create dialog with send+delete ticked (Create announced
// unavailable-and-why until the phrase is typed), the token dialog (named,
// Escape does NOT close it), a key's detail, and the desk with the banner (a
// polite live region in the AX tree). axe AA on each in light and dark; two
// negative controls. With A11Y_SHOTS=1 it also saves screenshots to
// /tmp/shots (desktop and phone, light and dark), which a11y-e2e.sh copies out
// when A11Y_SHOT_DIR is set.

const APIKEY_USING = [
  "urn:ietf:params:jmap:core",
  "urn:ietf:params:jmap:mail",
  "urn:ietf:params:jmap:submission",
  "urn:mailroom:apikeys",
];
const SHOTS = process.env.A11Y_SHOTS === "1";
if (SHOTS) fs.mkdirSync("/tmp/shots", { recursive: true });
const apiKeyState = { pendingIds: [] };

async function apiKeysFixture(page) {
  const delIds = FIX.messages.slice(0, 2).map((m) => m.id);
  const out = await page.evaluate(
    async (using, delIds) => {
      const sess = await (await fetch("/jmap/session", { credentials: "include" })).json();
      const accountId = Object.values(sess.primaryAccounts)[0];
      const asSession = async (calls) =>
        (await (await fetch(sess.apiUrl, { method: "POST", credentials: "include", headers: { "content-type": "application/json" }, body: JSON.stringify({ using, methodCalls: calls }) })).json()).methodResponses;
      const asKey = async (token, calls) => {
        const r = await fetch(sess.apiUrl, {
          method: "POST",
          credentials: "omit",
          headers: { "content-type": "application/json", authorization: "Bearer " + token },
          body: JSON.stringify({ using: using.slice(0, 3), methodCalls: calls }),
        });
        return r.status === 200 ? (await r.json()).methodResponses : [["http", { status: r.status, text: await r.text() }]];
      };
      const [c] = await asSession([
        ["ApiKey/set", { accountId, create: {
          s: { name: "Fixture sender", scopes: ["mail:read", "mail:draft", "mail:send"] },
          d: { name: "Fixture cleaner", scopes: ["mail:read", "mail:delete"] },
        } }, "c"],
      ]);
      const created = (c && c[1] && c[1].created) || {};
      if (!created.s || !created.d) return { error: "ApiKey/set create: " + JSON.stringify(c) };
      const RLO = String.fromCodePoint(0x202e);
      const subject = 'Quarterly numbers" to boss@example.com with 0 attachments (+0 more) ' + RLO + "fdp.exe";
      const addr = (email) => ({ name: "", email });
      const [d] = await asKey(created.s.token, [
        ["Email/set", { accountId, create: { d: {
          subject,
          to: [addr("alice@example.com"), addr("bob@example.com")],
          cc: [addr("carol@example.com")],
          bcc: [addr("dave@example.com")],
          textBody: [{ partId: "1", type: "text/plain" }],
          bodyValues: { "1": { value: "Hello from the fixture." } },
        } } }, "d"],
      ]);
      const draft = d && d[1] && d[1].created && d[1].created.d;
      if (!draft) return { error: "draft as key: " + JSON.stringify(d) };
      const [s] = await asKey(created.s.token, [
        ["EmailSubmission/set", { accountId, create: { s: { emailId: draft.id } } }, "s"],
      ]);
      const held = s && s[1] && s[1].notCreated && s[1].notCreated.s;
      if (!held || held.type !== "pendingApproval") return { error: "send as key was not held: " + JSON.stringify(s) };
      const [x] = await asKey(created.d.token, [["Email/set", { accountId, destroy: delIds }, "x"]]);
      const nd = (x && x[1] && x[1].notDestroyed) || {};
      const first = nd[delIds[0]];
      if (!first || first.type !== "pendingApproval") return { error: "delete as key was not held: " + JSON.stringify(x) };
      return { pending: [held.pendingActionId, first.pendingActionId] };
    },
    APIKEY_USING,
    delIds,
  );
  if (!out || out.error) throw new Fail(31, "APIKEYS fixture: " + (out ? out.error : "no answer"));
  apiKeyState.pendingIds = out.pending;
  console.log("apikeys fixture pending=" + out.pending.join(","));
}

async function openApiKeysRow(page) {
  await load(page, "#/settings/api-keys");
  await until(
    "apikeys: both requests and the keys are drawn",
    30000,
    () => page.evaluate(() => document.querySelectorAll(".pending-card").length >= 2 && document.querySelectorAll(".apikey-list > li").length >= 2),
    31,
    () => page.evaluate(() => "APIKEYS row: " + ((document.querySelector("#setting-body-api-keys") || {}).textContent || "").slice(0, 400)),
  );
}

async function openApproveDialog(page) {
  await openApiKeysRow(page);
  await page.evaluate(() => {
    const card = [...document.querySelectorAll(".pending-card")].find((c) => /Send a message/.test(c.textContent || ""));
    const b = [...card.querySelectorAll("button")].find((x) => (x.textContent || "").trim() === "Approve…");
    b.focus();
    b.click();
  });
  await waitForSelectorD(page, '.approve-dialog[role="dialog"]', { timeout: 15000 });
}

async function openCreateDialog(page, both) {
  await openApiKeysRow(page);
  await page.evaluate(() => {
    const b = [...document.querySelectorAll("#setting-body-api-keys button")].find((x) => (x.textContent || "").trim() === "Create a key…");
    b.focus();
    b.click();
  });
  await waitForSelectorD(page, '.apikey-create[role="dialog"]', { timeout: 15000 });
  if (both) {
    await page.click('.apikey-create input[type="radio"][value="custom"]');
    await waitForSelectorD(page, '.apikey-scopes input[value="mail:send"]', { timeout: 5000 });
    await page.click('.apikey-scopes input[value="mail:send"]');
    await page.click('.apikey-scopes input[value="mail:delete"]');
    await waitForSelectorD(page, ".apikey-both-confirm", { timeout: 5000 });
  }
}

async function openTokenDialog(page, name) {
  await openCreateDialog(page, false);
  await page.focus('.apikey-create input[type="text"]');
  await page.keyboard.type(name);
  await page.click(".apikey-create-submit");
  await waitForSelectorD(page, '.apikey-token[role="dialog"]', { timeout: 15000 });
}

async function dialogTrapProblems(page, sel) {
  const count = await page.evaluate(
    (s) => [...document.querySelectorAll(s + " button, " + s + " input, " + s + " [tabindex='0']")].filter((e) => !e.disabled && e.offsetParent !== null).length,
    sel,
  );
  const escapes = [];
  const where = () =>
    page.evaluate((s) => {
      const a = document.activeElement;
      const d = document.querySelector(s);
      return { inside: !!(a && d && d.contains(a)), what: a ? a.tagName.toLowerCase() + "." + String(a.className).split(" ")[0] : "none" };
    }, sel);
  for (let i = 0; i < count + 2; i++) {
    await page.keyboard.press("Tab");
    const w = await where();
    if (!w.inside) escapes.push("Tab " + (i + 1) + " -> " + w.what);
  }
  await page.keyboard.down("Shift");
  for (let i = 0; i < count + 2; i++) {
    await page.keyboard.press("Tab");
    const w = await where();
    if (!w.inside) escapes.push("Shift+Tab " + (i + 1) + " -> " + w.what);
  }
  await page.keyboard.up("Shift");
  return { count, escapes };
}

async function namedModal(cdp, sel, want) {
  const dlg = await axCDP(cdp, sel);
  if (!dlg || dlg.role !== "dialog" || !dlg.name || dlg.name.indexOf(want) < 0 || dlg.props["modal"] !== true) {
    throw new Fail(31, "APIKEYS " + sel + " is not a modal dialog named " + JSON.stringify(want) + ": " + JSON.stringify(dlg));
  }
}

async function shoot(page, name) {
  if (!SHOTS) return;
  // The settings page scrolls inside itself: bring the open row's body (or the
  // dialog) into view so the picture is of what this stage is about.
  await page.evaluate(() => {
    const d = document.querySelector('.key-portal [role="dialog"]');
    if (d) return;
    const body = document.querySelector("#setting-body-api-keys");
    if (body) body.scrollIntoView({ block: "start" });
  });
  await sleep(300);
  await page.screenshot({ path: "/tmp/shots/" + name + ".png" });
  console.log("shot " + name);
}

async function apiKeysSurfaces(page, cdp) {
  await apiKeysFixture(page);

  for (const theme of [THEMES[0], THEMES[1]]) {
    await chooseTheme(page, theme);

    // ---- the row: both cards, the keys, the setup guide ------------------
    await openApiKeysRow(page);
    await injectAxe(page);
    await axeAA(page, "settings-api-keys", theme.name);
    if (theme === THEMES[0]) {
      const facts = await page.evaluate(() => {
        const send = [...document.querySelectorAll(".pending-card")].find((c) => /Send a message/.test(c.textContent || ""));
        const field = (label) => {
          const dt = [...send.querySelectorAll(".pending-fields dt")].find((d) => (d.textContent || "").startsWith(label));
          return dt ? [...dt.nextElementSibling.querySelectorAll(".pending-addresses bdi")].map((b) => b.textContent) : null;
        };
        const subj = send.querySelector(".pending-subject");
        return {
          to: field("To"), cc: field("Cc"), bcc: field("Bcc"),
          subjectTag: subj && subj.tagName, subjectDir: subj && subj.getAttribute("dir"),
          rlo: send.innerHTML.indexOf(String.fromCodePoint(0x202e)) >= 0,
          summaryLeak: send.textContent.indexOf("Send to ") >= 0,
        };
      });
      if (JSON.stringify(facts.to) !== JSON.stringify(["alice@example.com", "bob@example.com"]) ||
          JSON.stringify(facts.cc) !== JSON.stringify(["carol@example.com"]) ||
          JSON.stringify(facts.bcc) !== JSON.stringify(["dave@example.com"]) ||
          facts.subjectTag !== "BDI" || facts.subjectDir !== "auto" || facts.rlo || facts.summaryLeak) {
        throw new Fail(31, "APIKEYS the held send's card: " + JSON.stringify(facts));
      }
      const names = await keyControlNameProblems(page, cdp, "#setting-body-api-keys");
      if (names.bad.length) throw new Fail(31, "APIKEYS row control names: " + JSON.stringify(names));
    }
    await shoot(page, "desktop-" + theme.name + "-list");

    // ---- the approve dialog -----------------------------------------------
    await openApproveDialog(page);
    await injectAxe(page);
    await axeAA(page, "settings-api-keys-approve", theme.name);
    if (theme === THEMES[0]) {
      await namedModal(cdp, '.approve-dialog[role="dialog"]', "Approve");
      const first = await page.evaluate(() => (document.activeElement || {}).textContent);
      if (first !== "Cancel") throw new Fail(31, "APIKEYS focus did not start on Cancel in the approve dialog: " + first);
      const trap = await dialogTrapProblems(page, '.approve-dialog[role="dialog"]');
      if (trap.count < 3 || trap.escapes.length) throw new Fail(31, "APIKEYS focus left the approve dialog: " + JSON.stringify(trap));
      const said = await page.evaluate(() => document.querySelector(".approve-what").textContent);
      if (said !== "The message will be sent to 4 recipients.") throw new Fail(31, "APIKEYS approve sentence: " + said);
    }
    await shoot(page, "desktop-" + theme.name + "-approve");
    await page.keyboard.press("Escape");

    // ---- the create dialog with send + delete ticked ----------------------
    await openCreateDialog(page, true);
    await injectAxe(page);
    await axeAA(page, "settings-api-keys-create", theme.name);
    if (theme === THEMES[0]) {
      await namedModal(cdp, '.apikey-create[role="dialog"]', "Create an API key");
      const names = await keyControlNameProblems(page, cdp, '.apikey-create[role="dialog"]');
      if (names.count < 8 || names.bad.length) throw new Fail(31, "APIKEYS create dialog names: " + JSON.stringify(names));
      const ax = await axCDP(cdp, ".apikey-create-submit");
      if (!ax || ax.props["disabled"] !== true || ax.props["focusable"] !== true || !ax.description) {
        throw new Fail(31, "APIKEYS Create is not announced unavailable-and-why before the phrase: " + JSON.stringify(ax));
      }
    }
    await shoot(page, "desktop-" + theme.name + "-create");
    await page.keyboard.press("Escape");

    // ---- the token dialog --------------------------------------------------
    await openTokenDialog(page, "A11y token " + theme.name);
    await injectAxe(page);
    await axeAA(page, "settings-api-keys-token", theme.name);
    if (theme === THEMES[0]) {
      await namedModal(cdp, '.apikey-token[role="dialog"]', "Your new key");
      await page.keyboard.press("Escape");
      await sleep(400);
      if (!(await page.$('.apikey-token[role="dialog"]'))) throw new Fail(31, "APIKEYS Escape closed the token dialog");
      const trap = await dialogTrapProblems(page, '.apikey-token[role="dialog"]');
      if (trap.count < 3 || trap.escapes.length) throw new Fail(31, "APIKEYS focus left the token dialog: " + JSON.stringify(trap));
    }
    await shoot(page, "desktop-" + theme.name + "-token");
    await page.click(".apikey-token-done");
    await until("apikeys: Done closed the token dialog", 10000, () => page.evaluate(() => !document.querySelector(".apikey-token")), 31);

    // ---- a key's detail ----------------------------------------------------
    await page.evaluate(() => {
      const li = [...document.querySelectorAll(".apikey-list > li")].find((x) => /Fixture cleaner/.test(x.textContent || ""));
      [...li.querySelectorAll("button")].find((b) => (b.textContent || "").trim() === "Details").click();
    });
    await waitForSelectorD(page, ".apikey-detail .apikey-activity, .apikey-detail .field-hint", { timeout: 15000 });
    await until("apikeys: the detail's activity is read", 15000, () => page.evaluate(() => !!document.querySelector(".apikey-activity")), 31);
    await injectAxe(page);
    await axeAA(page, "settings-api-key-detail", theme.name);
    await shoot(page, "desktop-" + theme.name + "-detail");

    // ---- the desk with the banner -----------------------------------------
    await load(page, "#/inbox/review");
    await waitForDesk(page);
    await until("apikeys: the banner says two are waiting", 30000, () =>
      page.evaluate(() => /2 actions waiting for your approval/.test((document.querySelector(".pending-banner") || {}).textContent || "")),
      31,
    );
    await injectAxe(page);
    await axeAA(page, "desk-pending-banner", theme.name);
    if (theme === THEMES[0]) {
      const live = await axCDP(cdp, ".pending-banner-region");
      if (!live || live.role !== "status" || live.props["live"] !== "polite") {
        throw new Fail(31, "APIKEYS the banner is not a polite live region: " + JSON.stringify(live));
      }
    }
    if (SHOTS) {
      await page.screenshot({ path: "/tmp/shots/desktop-" + theme.name + "-banner.png" });
      console.log("shot desktop-" + theme.name + "-banner");
    }
  }

  // ---- negative controls ----------------------------------------------------
  await chooseTheme(page, THEMES[0]);
  await openApproveDialog(page);
  // axe must SEE inside the portalled dialog: the Approve button made nameless
  // has to be reported (button-name is WCAG 4.1.2, in the AA run).
  await control("apikeys-approve-nameless", async () => {
    await page.evaluate(() => {
      document.querySelector(".approve-submit").textContent = "";
    });
    try {
      await injectAxe(page);
      await axeAA(page, "settings-api-keys-approve-CONTROL", "light");
      return false;
    } catch (e) {
      if (e instanceof Fail && /button-name/.test(e.message)) return true;
      throw e;
    }
  });
  await openApproveDialog(page);
  await control("apikeys-approve-trap", async () => {
    await page.evaluate(() => {
      document.querySelector('.approve-dialog[role="dialog"]').addEventListener("keydown", (e) => {
        if (e.key === "Tab") e.stopPropagation();
      });
    });
    const broken = await dialogTrapProblems(page, '.approve-dialog[role="dialog"]');
    return broken.escapes.length > 0;
  });

  // ---- phone screenshots (the width itself is measured by phoneWidthSurfaces)
  if (SHOTS) {
    for (const theme of [THEMES[0], THEMES[1]]) {
      await chooseTheme(page, theme);
      await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 2, isMobile: true, hasTouch: true });
      await openApiKeysRow(page);
      await shoot(page, "phone-" + theme.name + "-list");
      await openApproveDialog(page);
      await shoot(page, "phone-" + theme.name + "-approve");
      await page.keyboard.press("Escape");
      await openTokenDialog(page, "Phone token " + theme.name);
      await shoot(page, "phone-" + theme.name + "-token");
      await page.click(".apikey-token-done");
      await load(page, "#/inbox/review");
      await waitForDesk(page);
      await page.screenshot({ path: "/tmp/shots/phone-" + theme.name + "-banner.png" });
      console.log("shot phone-" + theme.name + "-banner");
      await page.setViewport({ width: 1680, height: 1100 });
    }
    await chooseTheme(page, THEMES[0]);
  }
  tally.apiKeys = 1;
}

/** Deny every request the fixture left, so no later stage sees a banner. */
async function apiKeysCleanup(page) {
  const out = await page.evaluate(
    async (using, ids) => {
      const sess = await (await fetch("/jmap/session", { credentials: "include" })).json();
      const accountId = Object.values(sess.primaryAccounts)[0];
      const update = {};
      for (const id of ids) update[id] = { state: "denied" };
      const r = await fetch(sess.apiUrl, { method: "POST", credentials: "include", headers: { "content-type": "application/json" }, body: JSON.stringify({ using, methodCalls: [["PendingAction/set", { accountId, update }, "p"]] }) });
      return (await r.json()).methodResponses;
    },
    APIKEY_USING,
    apiKeyState.pendingIds,
  );
  const updated = (out && out[0] && out[0][1] && out[0][1].updated) || {};
  for (const id of apiKeyState.pendingIds) {
    if (!updated[id] || updated[id].state !== "denied") throw new Fail(31, "APIKEYS cleanup could not deny " + id + ": " + JSON.stringify(out));
  }
  console.log("apikeys cleanup denied=" + apiKeyState.pendingIds.length);
}

async function keysSurfaces(page, cdp) {
  // ---- the page, with key management on ---------------------------------
  await load(page, "#/settings/keys");
  await until(
    "keys: the management page is drawn with Make a new key",
    30000,
    () =>
      page.evaluate(() =>
        [...document.querySelectorAll(".keys-page button")].some((b) => (b.textContent || "").trim() === "Make a new key…"),
      ),
    29,
    () => page.evaluate(() => "KEYS no Make a new key button: " + ((document.querySelector(".settings-page") || {}).textContent || "").slice(0, 400)),
  );

  // ---- make a key through the page's own dialog --------------------------
  await page.evaluate(() =>
    [...document.querySelectorAll(".keys-page button")].find((b) => (b.textContent || "").trim() === "Make a new key…").click(),
  );
  await waitForSelectorD(page, '.key-dialog[role="dialog"]', { timeout: 15000 });
  await injectAxe(page);
  await axeAA(page, "settings-keys-generate", "light");
  const gen = await keyControlNameProblems(page, cdp, '.key-dialog[role="dialog"]');
  if (gen.count < 3 || gen.bad.length) throw new Fail(29, "KEYS generate dialog names: " + JSON.stringify(gen));
  await page.evaluate(() =>
    [...document.querySelectorAll('.key-dialog[role="dialog"] button')].find((b) => (b.textContent || "").trim() === "Make key").click(),
  );
  await until("keys: the generated key is listed", 60000, () =>
    page.evaluate(() => document.querySelectorAll(".keys-page .own-key").length >= 1 && !document.querySelector('.key-dialog[role="dialog"]')),
    29,
    () => page.evaluate(() => "KEYS generate: " + ((document.querySelector(".keys-page") || {}).textContent || "").slice(0, 400)),
  );

  // ---- a second key that expires in ten days, over JMAP -------------------
  const made = await page.evaluate(async () => {
    const sess = await (await fetch("/jmap/session", { credentials: "include" })).json();
    const accountId = Object.values(sess.primaryAccounts)[0];
    const using = ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:submission", "urn:mailroom:pgp"];
    const call = async (calls) =>
      (await (await fetch(sess.apiUrl, { method: "POST", credentials: "include", headers: { "content-type": "application/json" }, body: JSON.stringify({ using, methodCalls: calls }) })).json()).methodResponses;
    const [ids] = await call([["Identity/get", { accountId, ids: null }, "i"]]);
    const ident = ids[1].list.find((i) => !i.isWildcard);
    const [g] = await call([["UserKey/generate", { accountId, identityId: ident.id, expiresDays: 10 }, "g"]]);
    return g;
  });
  if (!made || made[0] !== "UserKey/generate") throw new Fail(29, "KEYS could not make the ten-day key: " + JSON.stringify(made));

  await load(page, "#/settings/keys");
  await until("keys: both keys are listed", 30000, () =>
    page.evaluate(() => document.querySelectorAll(".keys-page .own-key").length >= 2),
  29);
  await injectAxe(page);
  await axeAA(page, "settings-keys", "light");
  const pageNames = await keyControlNameProblems(page, cdp, ".keys-page");
  if (pageNames.count < 3 || pageNames.bad.length) throw new Fail(29, "KEYS page control names: " + JSON.stringify(pageNames));

  // ---- the revoke dialog ---------------------------------------------------
  await openRevokeDialog(page);
  await injectAxe(page);
  await axeAA(page, "settings-keys-revoke", "light");
  const names = await keyControlNameProblems(page, cdp, '.key-revoke[role="dialog"]');
  if (names.count < 4 || names.bad.length) throw new Fail(29, "KEYS revoke dialog names: " + JSON.stringify(names));
  const dlg = await axCDP(cdp, '.key-revoke[role="dialog"]');
  if (!dlg || dlg.role !== "dialog" || !dlg.name || dlg.name.indexOf("Revoke") < 0 || dlg.props["modal"] !== true) {
    throw new Fail(29, "KEYS the revoke dialog is not a named modal dialog: " + JSON.stringify(dlg));
  }
  const focusedIn = await page.evaluate(() => document.querySelector('.key-revoke[role="dialog"]').contains(document.activeElement));
  if (!focusedIn) throw new Fail(29, "KEYS focus did not move into the revoke dialog");

  // The confirm button: unavailable, still focusable, and saying why.
  const before = await revokeButtonState(cdp);
  if (!before.found || !before.disabled || !before.focusable || !before.description) {
    throw new Fail(29, "KEYS the revoke button is not announced as unavailable-and-why before the tail matches: " + JSON.stringify(before));
  }
  // A click while unavailable does nothing.
  await page.click(".key-revoke-submit");
  await sleep(500);
  if (!(await page.$('.key-revoke[role="dialog"]'))) throw new Fail(29, "KEYS an unavailable revoke button closed the dialog");

  const trap = await revokeTrapProblems(page);
  if (trap.count < 5 || trap.escapes.length) throw new Fail(29, "KEYS focus left the revoke dialog: " + JSON.stringify(trap));

  // Choose a reason and type the tail: the button becomes available.
  const tail = await page.evaluate(() => {
    const fpr = document.querySelector(".key-revoke-fingerprint code").textContent.replace(/\s+/g, "");
    return fpr.slice(-8);
  });
  await page.click('.key-revoke input[type="radio"][value="superseded"]');
  await page.focus(".key-revoke-confirm");
  await page.keyboard.type(tail.toLowerCase());
  await until("keys: the revoke button becomes available once the tail matches", 10000, async () => !(await revokeButtonState(cdp)).disabled, 29, async () =>
    "KEYS after typing the tail: " + JSON.stringify(await revokeButtonState(cdp)),
  );

  // Escape closes it and focus goes back to Revoke….
  await page.keyboard.press("Escape");
  await until("keys: Escape closed the revoke dialog, back on its opener", 10000, () =>
    page.evaluate(
      () =>
        !document.querySelector('.key-revoke[role="dialog"]') &&
        document.activeElement &&
        document.activeElement.getAttribute("data-a11y-opener") === "1" &&
        !!document.querySelector('.settings-page[role="dialog"]'),
    ),
    29,
    () =>
      page.evaluate(() => {
        const a = document.activeElement;
        return (
          "dialog open=" + !!document.querySelector('.key-revoke[role="dialog"]') +
          " active=" + (a ? a.tagName + "." + a.className + " '" + (a.textContent || "").trim().slice(0, 30) + "'" : "none")
        );
      }),
  );

  // ---- negative control 1: the trap cut -----------------------------------
  await openRevokeDialog(page);
  await control("keys-revoke-trap", async () => {
    await page.evaluate(() => {
      // A native listener below React's root: Tab never reaches the dialog's
      // handler, so the trap is gone while every attribute axe reads is intact.
      document.querySelector('.key-revoke[role="dialog"]').addEventListener("keydown", (e) => {
        if (e.key === "Tab") e.stopPropagation();
      });
    });
    const broken = await revokeTrapProblems(page);
    return broken.escapes.length > 0;
  });

  // ---- negative control 2: the unavailable state dropped ------------------
  await openRevokeDialog(page);
  await control("keys-revoke-disabled", async () => {
    await page.evaluate(() => document.querySelector(".key-revoke-submit").removeAttribute("aria-disabled"));
    const s = await revokeButtonState(cdp);
    return !(s.found && s.disabled && s.focusable && s.description);
  });

  // ---- the desk's expiry banner -------------------------------------------
  await page.evaluate(() => localStorage.removeItem("mailroom.keyWarnings.dismissed"));
  await load(page, "#/inbox/review");
  await waitForDesk(page);
  await until("keys: the desk warns about the ten-day key", 30000, () =>
    page.evaluate(() => {
      const b = document.querySelector(".key-expiry-banner [role=status]");
      return !!b && /expires on/.test(b.textContent || "");
    }),
    29,
    () => page.evaluate(() => "KEYS no banner: " + ((document.querySelector(".desk-chrome") || {}).textContent || "").slice(0, 300)),
  );
  await injectAxe(page);
  await axeAA(page, "desk-key-banner", "light");
  const bannerButtons = await page.evaluate(() =>
    [...document.querySelectorAll(".key-expiry-banner button")].map((b) => ({ text: (b.textContent || "").trim(), label: b.getAttribute("aria-label") || "" })),
  );
  if (!bannerButtons.some((b) => b.text === "Extend it") || !bannerButtons.some((b) => b.text === "Dismiss" && b.label.indexOf("Dismiss") === 0)) {
    throw new Fail(29, "KEYS the banner's two buttons: " + JSON.stringify(bannerButtons));
  }
  // Dismissed stays dismissed across a reload (localStorage).
  await page.evaluate(() => [...document.querySelectorAll(".key-expiry-banner button")].find((b) => (b.textContent || "").trim() === "Dismiss").click());
  await until("keys: Dismiss took the banner away", 10000, () => page.evaluate(() => !document.querySelector(".key-expiry-banner")), 29);
  await load(page, "#/inbox/review");
  await waitForDesk(page);
  await sleep(1500);
  if (await page.$(".key-expiry-banner")) throw new Fail(29, "KEYS a dismissed warning came back on reload");

  const later = await keyLifecycleSurfaces(page, cdp);

  tally.keys = 1;
  console.log(
    "keys              page + generate + revoke dialogs named (" + pageNames.count + "/" + gen.count + "/" + names.count +
      " controls); revoke focus trapped over " + trap.count + " controls; confirm unavailable-and-described until the tail matched; " +
      "banner shown, dismissed, stayed dismissed; 2 controls tripped",
  );
  console.log(
    "keys (v1.93 C2)   History disclosures expanded/collapsed by keyboard (yours + correspondents'); a key's history listed (" +
      later.events + " events); refresh said \"" + later.refreshed + "\"; date input named and described in generate (" +
      later.genDate + " controls); replace dialog named, modal, focus trapped over " + later.replaceTrap +
      " controls, confirm unavailable-and-described until a date is chosen; 3 controls tripped",
  );
}

// ---------------------------------------------------------------------------
// SETTINGS › KEYS, THE v1.93 FINAL SCOPE (#815 #813 #812 #817 #818) — exit 29
// ---------------------------------------------------------------------------
//
// What axe cannot see on its own, asserted from the accessibility tree:
//   * each History section is a BUTTON whose expanded state is exposed and
//     follows Enter and Space, and whose region appears and goes;
//   * "On a date…" puts a date input in the dialog that is NAMED by its visible
//     label and DESCRIBED by the result and the end-of-day-UTC note;
//   * the replace dialog is a named modal that holds focus, and its confirm is
//     announced as unavailable, and why, while a date is still to be chosen.
//
// THREE IN-RUN NEGATIVE CONTROLS, each a defect axe passes:
//   * aria-expanded removed from a History button → the expanded check goes red;
//   * aria-describedby removed from the date input → the description check red;
//   * aria-disabled removed from the replace confirm → the unavailable check red.
//
// Two public keys (gpg, never expiring, so the lane does not move with the
// calendar) are imported for correspondents; bob's is then removed, so both
// lists have a History.

const A11Y_ALICE_PUB = `-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEatkI2BYJKwYBBAHaRw8BAQdAoCTfbb/W5tIYClpFNP9hv4fkQ2smZFrohukV
4SlpsR+0IUFsaWNlIEV4YW1wbGUgPGFsaWNlQGV4YW1wbGUub3JnPoiTBBMWCgA7
FiEEBnaDWr//OqoGxjd6Ocx6WlxAzIAFAmrZCNgCGwMFCwkIBwICIgIGFQoJCAsC
BBYCAwECHgcCF4AACgkQOcx6WlxAzID4xwD/QQaA90LYbbMEfBOsBDS7rMY3pgbx
N+vKpWAfD15D9JYA/jPu1bl7YurU04HcKbDfRin2IpRZmungIwcVJ/X8GgwOuDgE
atkI2BIKKwYBBAGXVQEFAQEHQOw1IGNGJxHMQotItp6ZmIH3W/2f4Qc207lGeVe7
WEIcAwEIB4h4BBgWCgAgFiEEBnaDWr//OqoGxjd6Ocx6WlxAzIAFAmrZCNgCGwwA
CgkQOcx6WlxAzIDYeAD/XN+gGiwZb236cx4tQpl9BHY38P5dyFF12G2ARKWkG6MB
AIB6vs2cZKRL1csNHKWwVzUAi18sNudoqXzCx3ojK2EH
=RGw6
-----END PGP PUBLIC KEY BLOCK-----
`;
const A11Y_BOB_PUB = `-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEatkI2BYJKwYBBAHaRw8BAQdA1cLBlfxNpwp6aBPdWw8IBAtEHn27FJ7n1/N7
YU48yn20HUJvYiBFeGFtcGxlIDxib2JAZXhhbXBsZS5vcmc+iJMEExYKADsWIQRk
kaHyx0D+rezMjQoypk7Ev+bzKAUCatkI2AIbAwULCQgHAgIiAgYVCgkICwIEFgID
AQIeBwIXgAAKCRAypk7Ev+bzKFrgAP9yC4z5qDsh482edLNpxm31eW9tCD4sQbHU
o9d81ZWBNQD+PQ40BLyRu94Re+x1MNkBqITlaBqqGx3WGai4mXk9Vg64OARq2QjY
EgorBgEEAZdVAQUBAQdA0UcYLSqR9VtzaZXdZoEUZ8nw7MMoXppnwmD4FVtM7TMD
AQgHiHgEGBYKACAWIQRkkaHyx0D+rezMjQoypk7Ev+bzKAUCatkI2AIbDAAKCRAy
pk7Ev+bzKOBMAQCQt40TE/DziOdvHG0UeaLmUl5gKnyChNcGBd7iEQPGbwD+Jcxp
F2PBL4muidB+GEz59Ys3xrkN7d0LNxyLEYySiQU=
=Y1gY
-----END PGP PUBLIC KEY BLOCK-----
`;

async function keysJmap(page, calls) {
  return page.evaluate(async (calls) => {
    const sess = await (await fetch("/jmap/session", { credentials: "include" })).json();
    const accountId = Object.values(sess.primaryAccounts)[0];
    const using = ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", "urn:mailroom:contacts", "urn:mailroom:pgp"];
    const methodCalls = calls.map(([name, args, id]) => [name, { accountId, ...args }, id]);
    const res = await fetch(sess.apiUrl, {
      method: "POST",
      credentials: "include",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ using, methodCalls }),
    });
    return (await res.json()).methodResponses;
  }, calls);
}

/** The expanded state a History button exposes, by CDP. */
async function disclosureState(cdp, sel) {
  const ax = await axCDP(cdp, sel);
  return {
    found: !!ax,
    role: ax && ax.role,
    name: ax && ax.name,
    expanded: ax && ax.props ? ax.props["expanded"] : undefined,
  };
}

/** Tab forward and back through a dialog more times than it has controls;
 * returns the escapes. */
async function dialogTrapProblems(page, sel) {
  const count = await page.evaluate(
    (sel) =>
      [...document.querySelectorAll(sel + " button, " + sel + " input, " + sel + " select")].filter(
        (e) => !e.disabled && e.offsetParent !== null,
      ).length,
    sel,
  );
  const escapes = [];
  const inside = () => page.evaluate((sel) => !!document.querySelector(sel)?.contains(document.activeElement), sel);
  for (let i = 0; i < count + 2; i++) {
    await page.keyboard.press("Tab");
    if (!(await inside())) escapes.push("Tab " + (i + 1));
  }
  for (let i = 0; i < count + 2; i++) {
    await page.keyboard.down("Shift");
    await page.keyboard.press("Tab");
    await page.keyboard.up("Shift");
    if (!(await inside())) escapes.push("Shift+Tab " + (i + 1));
  }
  return { count, escapes };
}

async function openKeysPage(page) {
  await load(page, "#/settings/keys");
  await waitForSelectorD(page, ".keys-page .own-key", { timeout: 30000 });
  await waitForSelectorD(page, ".correspondent-keys .key-history-toggle", { timeout: 30000 });
}

/** Both History sections open, and the first correspondent key's history
 * shown: the state the phone-width check and the screenshots measure. */
async function openKeyHistories(page) {
  await openKeysPage(page);
  await page.evaluate(() => {
    for (const b of document.querySelectorAll(".keys-page .key-history-toggle")) {
      if (b.getAttribute("aria-expanded") === "false") b.click();
    }
  });
  await page.evaluate(() => {
    const b = document.querySelector(".correspondent-keys .correspondent-key .key-history-show");
    if (b && b.getAttribute("aria-expanded") === "false") b.click();
  });
  await waitForSelectorD(page, ".correspondent-keys .key-events li", { timeout: 15000 });
}

async function openReplaceDialog(page, withDate) {
  await openKeysPage(page);
  await page.evaluate(() => {
    const b = [...document.querySelectorAll(".keys-page .own-key .own-key-actions button")].find(
      (x) => (x.textContent || "").trim() === "Replace…",
    );
    b.setAttribute("data-a11y-opener", "replace");
    b.focus();
    b.click();
  });
  await waitForSelectorD(page, '.key-replace[role="dialog"]', { timeout: 15000 });
  if (withDate) await page.select('.key-replace[role="dialog"] select', "date");
}

async function openGenerateDateDialog(page) {
  await openKeysPage(page);
  await page.evaluate(() =>
    [...document.querySelectorAll(".keys-page button")].find((b) => (b.textContent || "").trim() === "Make a new key…").click(),
  );
  await waitForSelectorD(page, '.key-dialog[role="dialog"]', { timeout: 15000 });
  const selects = await page.$$('.key-dialog[role="dialog"] select');
  await selects[1].select("date");
  await waitForSelectorD(page, '.key-dialog[role="dialog"] input[type="date"]', { timeout: 5000 });
}

async function keyLifecycleSurfaces(page, cdp) {
  // ---- correspondents: alice held, bob removed ---------------------------
  const imported = await keysJmap(page, [
    ["ContactKey/import", { address: "alice@example.org", publicKey: A11Y_ALICE_PUB }, "a"],
    ["ContactKey/import", { address: "bob@example.org", publicKey: A11Y_BOB_PUB }, "b"],
  ]);
  if (!imported || imported.some((r) => r[0] !== "ContactKey/import")) {
    throw new Fail(29, "KEYS could not import the correspondents' keys: " + JSON.stringify(imported));
  }
  const listed = await keysJmap(page, [["ContactKey/get", {}, "g"]]);
  const bob = listed[0][1].list.find((k) => k.addresses.indexOf("bob@example.org") >= 0);
  const removed = await keysJmap(page, [["ContactKey/set", { destroy: [bob.id] }, "d"]]);
  if (!removed[0][1].destroyed || removed[0][1].destroyed[0] !== bob.id) {
    throw new Fail(29, "KEYS could not remove bob's key: " + JSON.stringify(removed));
  }

  // ---- the History disclosures, by keyboard --------------------------------
  await openKeysPage(page);
  for (const [which, sel] of [
    ["yours", ".own-keys .key-history-toggle"],
    ["correspondents'", ".correspondent-keys .key-history-toggle"],
  ]) {
    const closed = await disclosureState(cdp, sel);
    if (!closed.found || closed.role !== "button" || !/History/.test(closed.name || "") || closed.expanded !== false) {
      throw new Fail(29, "KEYS the " + which + " History is not a collapsed, named button: " + JSON.stringify(closed));
    }
    await page.focus(sel);
    await page.keyboard.press("Enter");
    await until("keys: Enter opened the " + which + " History", 5000, async () => (await disclosureState(cdp, sel)).expanded === true, 29);
    const region = await page.evaluate((sel) => {
      const r = document.getElementById(document.querySelector(sel).getAttribute("aria-controls"));
      return !!r && !r.hidden && r.getAttribute("role") === "region" && r.offsetParent !== null;
    }, sel);
    if (!region) throw new Fail(29, "KEYS the " + which + " History opened no visible region");
    await page.keyboard.press("Space");
    await until("keys: Space closed the " + which + " History", 5000, async () => (await disclosureState(cdp, sel)).expanded === false, 29);
    await page.keyboard.press("Enter");
    await until("keys: Enter opened the " + which + " History again", 5000, async () => (await disclosureState(cdp, sel)).expanded === true, 29);
  }
  await control("keys-history-expanded", async () => {
    await page.evaluate(() => document.querySelector(".own-keys .key-history-toggle").removeAttribute("aria-expanded"));
    const s = await disclosureState(cdp, ".own-keys .key-history-toggle");
    return !(s.expanded === true || s.expanded === false);
  });

  // ---- a correspondent key's history, and a refresh ------------------------
  await openKeyHistories(page);
  const events = await page.evaluate(() => document.querySelectorAll(".correspondent-keys .key-events li").length);
  const showState = await disclosureState(cdp, ".correspondent-keys .correspondent-key .key-history-show");
  if (showState.expanded !== true || !/Show history of|Hide history of/.test(showState.name || "")) {
    throw new Fail(29, "KEYS Show history is not an expanded, named button once open: " + JSON.stringify(showState));
  }
  await page.click(".correspondent-keys .correspondent-key .key-refresh");
  await until("keys: the refresh said what it found", 30000, () =>
    page.evaluate(() => {
      const s = document.querySelector(".correspondent-keys .key-refresh-result");
      return !!s && s.textContent.length > 0 && s.textContent.indexOf("Checking") < 0;
    }),
  29);
  const refreshed = await page.evaluate(() => document.querySelector(".correspondent-keys .key-refresh-result").textContent);
  await injectAxe(page);
  await axeAA(page, "settings-keys-history", "light");

  // ---- "On a date…" in generate --------------------------------------------
  await openGenerateDateDialog(page);
  await injectAxe(page);
  await axeAA(page, "settings-keys-generate-date", "light");
  const genDate = await keyControlNameProblems(page, cdp, '.key-dialog[role="dialog"]');
  if (genDate.count < 4 || genDate.bad.length) throw new Fail(29, "KEYS generate-on-a-date names: " + JSON.stringify(genDate));
  const dateAx = await axCDP(cdp, '.key-dialog[role="dialog"] input[type="date"]');
  if (!dateAx || dateAx.name !== "Expiry date" || !/end of that day \(UTC\)/.test(dateAx.description || "")) {
    throw new Fail(29, "KEYS the date input is not named and described: " + JSON.stringify(dateAx));
  }
  await control("keys-date-described", async () => {
    await page.evaluate(() => document.querySelector('.key-dialog[role="dialog"] input[type="date"]').removeAttribute("aria-describedby"));
    const ax = await axCDP(cdp, '.key-dialog[role="dialog"] input[type="date"]');
    return !(ax && /end of that day \(UTC\)/.test(ax.description || ""));
  });
  await page.keyboard.press("Escape");

  // ---- the replace dialog ----------------------------------------------------
  await openReplaceDialog(page, false);
  await injectAxe(page);
  await axeAA(page, "settings-keys-replace", "light");
  const rep = await axCDP(cdp, '.key-replace[role="dialog"]');
  if (!rep || rep.role !== "dialog" || !/Replace/.test(rep.name || "") || rep.props["modal"] !== true) {
    throw new Fail(29, "KEYS the replace dialog is not a named modal dialog: " + JSON.stringify(rep));
  }
  if (!(await page.evaluate(() => document.querySelector('.key-replace[role="dialog"]').contains(document.activeElement)))) {
    throw new Fail(29, "KEYS focus did not move into the replace dialog");
  }
  const repNames = await keyControlNameProblems(page, cdp, '.key-replace[role="dialog"]');
  if (repNames.count < 1 || repNames.bad.length) throw new Fail(29, "KEYS replace dialog names: " + JSON.stringify(repNames));
  const replaceTrap = await dialogTrapProblems(page, '.key-replace[role="dialog"]');
  if (replaceTrap.count < 4 || replaceTrap.escapes.length) {
    throw new Fail(29, "KEYS focus left the replace dialog: " + JSON.stringify(replaceTrap));
  }
  await page.select('.key-replace[role="dialog"] select', "date");
  await injectAxe(page);
  await axeAA(page, "settings-keys-replace-date", "light");
  const confirm = async () => {
    const ax = await axCDP(cdp, ".key-replace-submit");
    return {
      found: !!ax,
      disabled: !!(ax && ax.props && ax.props["disabled"] === true),
      focusable: !!(ax && ax.props && ax.props["focusable"] === true),
      description: ax && ax.description,
    };
  };
  const waiting = await confirm();
  if (!waiting.found || !waiting.disabled || !waiting.focusable || !waiting.description) {
    throw new Fail(29, "KEYS the replace confirm is not announced as unavailable-and-why with no date: " + JSON.stringify(waiting));
  }
  await page.click(".key-replace-submit");
  await sleep(500);
  if (!(await page.$('.key-replace[role="dialog"]'))) throw new Fail(29, "KEYS an unavailable replace confirm closed the dialog");
  await control("keys-replace-disabled", async () => {
    await page.evaluate(() => document.querySelector(".key-replace-submit").removeAttribute("aria-disabled"));
    const s = await confirm();
    return !(s.found && s.disabled && s.focusable && s.description);
  });
  await page.keyboard.press("Escape");
  await until("keys: Escape closed the replace dialog, back on Replace…", 10000, () =>
    page.evaluate(
      () =>
        !document.querySelector('.key-replace[role="dialog"]') &&
        document.activeElement &&
        document.activeElement.getAttribute("data-a11y-opener") === "replace",
    ),
  29);
  return { events, refreshed, genDate: genDate.count, replaceTrap: replaceTrap.count };
}

// ---------------------------------------------------------------------------
// THE CONTACTS PAGE (#816) — exit 30 CONTACTS
// ---------------------------------------------------------------------------
//
// The list, one contact's page and the merge dialog, each through axe (AA,
// light), and then what axe cannot see on its own, from the accessibility tree:
//   * the list's search field is NAMED by its visible label, and the filter
//     chips are a named group of TOGGLE buttons whose pressed state is exposed
//     (axe passes a button that lost aria-pressed: it still has a name);
//   * on a contact's page the notes field is named "Notes" and DESCRIBED by its
//     live character count, and VIP exposes its pressed state;
//   * the merge dialog is a named modal that takes focus into its search, holds
//     it (Tab and Shift+Tab never leave), the search is a combobox naming the
//     ACTIVE option through aria-activedescendant, ArrowDown/Enter choose by
//     keyboard (the preview appears), and Escape closes it with focus back on
//     "Merge with…".
//
// THREE IN-RUN NEGATIVE CONTROLS, each a defect axe passes:
//   * aria-pressed removed from the filter chips → the pressed check goes red;
//   * aria-describedby removed from the notes field → the description check red;
//   * aria-activedescendant removed from the merge search → the active check red.
//
// The fixture: Ada saved by hand (the lane's own sender), the Grand Theatre's
// list address blocked to Trash and never saved — the merge candidate, so the
// preview has a block to carry over. The merge is CANCELLED: later steps (the
// phone-width pass) measure these same views.

async function contactsJmap(page, calls) {
  return keysJmap(page, calls);
}

async function contactsFixture(page) {
  const got = await contactsJmap(page, [["Contact/get", { ids: null }, "g"]]);
  const list = (got[0] && got[0][1] && got[0][1].list) || [];
  const by = (a) => list.find((c) => (c.addresses || []).some((x) => x.address === a));
  const ada = by("ada@example.com");
  const news = by("updates@news.theatre.example.com");
  if (!ada || !news || typeof ada.isSaved !== "boolean") {
    throw new Fail(30, "CONTACTS the lane's contacts are not there (or the server has no saved contacts): " + JSON.stringify(list.map((c) => [c.displayName, c.isSaved])));
  }
  const set = await contactsJmap(page, [
    ["Contact/set", { update: { [ada.id]: { isSaved: true, notes: "Met at the analytical engine talk." }, [news.id]: { isBlocked: true, blockDestination: "trash" } } }, "s"],
  ]);
  const upd = (set[0] && set[0][1] && set[0][1].updated) || {};
  if (!(ada.id in upd) || !(news.id in upd)) throw new Fail(30, "CONTACTS could not prepare the fixture: " + JSON.stringify(set));
  return { ada: ada.id, news: news.id };
}

async function openContactsList(page) {
  await load(page, "#/contacts");
  await until("contacts: the list has rows", 30000, () => page.evaluate(() => document.querySelectorAll(".contacts-row").length > 0), 30,
    () => page.evaluate(() => ((document.querySelector(".contacts-list-pane") || {}).textContent || "").slice(0, 300)));
}

async function openContactPage(page, id) {
  await load(page, "#/contacts/" + id);
  await waitForSelectorD(page, "h1.contact-name", { timeout: 30000 });
  await waitForSelectorD(page, "textarea.contact-notes", { timeout: 30000 });
}

async function openMergeDialog(page, id) {
  await openContactPage(page, id);
  await page.evaluate(() => {
    const b = document.querySelector("button.contact-merge-open");
    b.setAttribute("data-a11y-opener", "merge");
    b.focus();
    b.click();
  });
  await waitForSelectorD(page, '.contact-merge[role="dialog"]', { timeout: 15000 });
}

async function chooseTheatreInMerge(page) {
  await page.type('.contact-merge[role="dialog"] input[role="combobox"]', "updates@news.theatre.example.com");
  await until("contacts: the merge search lists the theatre", 15000, () =>
    page.evaluate(() => document.querySelectorAll('.contact-merge [role="option"]').length > 0), 30);
}

async function contactsSurfaces(page, cdp) {
  const ids = await contactsFixture(page);

  // ---- the list ------------------------------------------------------------
  await openContactsList(page);
  await injectAxe(page);
  await axeAA(page, "contacts-list", "light");
  const search = await axCDP(cdp, ".contacts-search");
  if (!search || search.name !== "Search contacts") throw new Fail(30, "CONTACTS the list's search is not named by its label: " + JSON.stringify(search));
  const group = await axCDP(cdp, ".contacts-filters");
  if (!group || group.role !== "group" || group.name !== "Show") throw new Fail(30, "CONTACTS the filters are not a named group: " + JSON.stringify(group));
  const chips = async () => {
    const n = await page.evaluate(() => {
      const bs = [...document.querySelectorAll(".contacts-filters button")];
      bs.forEach((b, i) => (b.id = "a11y-contacts-chip-" + i));
      return bs.length;
    });
    const out = [];
    for (let i = 0; i < n; i++) {
      const ax = await axCDP(cdp, "#a11y-contacts-chip-" + i);
      out.push({ name: ax && ax.name, pressed: ax && ax.props ? ax.props["pressed"] : undefined });
    }
    return out;
  };
  const before = await chips();
  const pressedOk = (cs) =>
    cs.length === 4 && cs.every((c) => c.pressed === "true" || c.pressed === "false" || c.pressed === true || c.pressed === false) &&
    cs.filter((c) => c.pressed === "true" || c.pressed === true).length === 1;
  if (!pressedOk(before) || before[0].name !== "All saved") throw new Fail(30, "CONTACTS the filter chips do not expose one pressed toggle: " + JSON.stringify(before));
  // Blocked senders, by keyboard: the URL changes and the theatre (never saved) is listed.
  await page.focus("#a11y-contacts-chip-3");
  await page.keyboard.press("Enter");
  await until("contacts: Blocked senders lists the theatre", 15000, () =>
    page.evaluate(() => location.hash === "#/contacts?filter=blocked" &&
      [...document.querySelectorAll(".contacts-row-name")].some((n) => n.textContent === "The Grand Theatre")), 30);
  const blocked = await chips();
  if (!(blocked[3].pressed === "true" || blocked[3].pressed === true)) throw new Fail(30, "CONTACTS Blocked senders is not the pressed chip: " + JSON.stringify(blocked));
  await injectAxe(page);
  await axeAA(page, "contacts-list-blocked", "light");
  await control("contacts-chip-pressed", async () => {
    await page.evaluate(() => document.querySelectorAll(".contacts-filters button").forEach((b) => b.removeAttribute("aria-pressed")));
    return !pressedOk(await chips());
  });

  // ---- a contact's page -----------------------------------------------------
  await openContactPage(page, ids.ada);
  await injectAxe(page);
  await axeAA(page, "contacts-page", "light");
  const notes = async () => axCDP(cdp, "textarea.contact-notes");
  const n1 = await notes();
  if (!n1 || n1.name !== "Notes" || !/of 4,000 characters/.test(n1.description || "")) {
    throw new Fail(30, "CONTACTS the notes field is not named and described by its count: " + JSON.stringify(n1));
  }
  const vip = await axCDP(cdp, "button.contact-vip");
  if (!vip || vip.name !== "VIP" || !(vip.props["pressed"] === "false" || vip.props["pressed"] === false)) {
    throw new Fail(30, "CONTACTS VIP is not a toggle exposing its state: " + JSON.stringify(vip));
  }
  const actions = await axCDP(cdp, ".contact-actions");
  if (!actions || actions.role !== "group" || !/Ada/.test(actions.name || "")) throw new Fail(30, "CONTACTS the page's actions are not a group named by the contact: " + JSON.stringify(actions));
  await control("contacts-notes-described", async () => {
    await page.evaluate(() => document.querySelector("textarea.contact-notes").removeAttribute("aria-describedby"));
    const n = await notes();
    return !(n && /of 4,000 characters/.test(n.description || ""));
  });

  // ---- the merge dialog -----------------------------------------------------
  await openMergeDialog(page, ids.ada);
  await injectAxe(page);
  await axeAA(page, "contacts-merge", "light");
  const dlg = await axCDP(cdp, '.contact-merge[role="dialog"]');
  if (!dlg || dlg.role !== "dialog" || !/^Merge with /.test(dlg.name || "") || dlg.props["modal"] !== true) {
    throw new Fail(30, "CONTACTS the merge dialog is not a named modal dialog: " + JSON.stringify(dlg));
  }
  const focusInSearch = await page.evaluate(() => document.activeElement && document.activeElement.matches('.contact-merge input[role="combobox"]'));
  if (!focusInSearch) throw new Fail(30, "CONTACTS focus did not move into the merge search");
  const combo = await axCDP(cdp, '.contact-merge input[role="combobox"]');
  if (!combo || combo.role !== "combobox" || combo.name !== "Find a contact or an address") {
    throw new Fail(30, "CONTACTS the merge search is not a combobox named by its label: " + JSON.stringify(combo));
  }
  await chooseTheatreInMerge(page);
  await page.keyboard.press("ArrowDown");
  const activeOk = () =>
    page.evaluate(() => {
      const input = document.querySelector('.contact-merge input[role="combobox"]');
      const id = input.getAttribute("aria-activedescendant");
      const opt = id ? document.getElementById(id) : null;
      return !!opt && opt.getAttribute("role") === "option" && opt.getAttribute("aria-selected") === "true" &&
        input.getAttribute("aria-expanded") === "true";
    });
  if (!(await activeOk())) throw new Fail(30, "CONTACTS the merge search does not name its active option");
  await page.keyboard.press("Enter");
  await until("contacts: Enter chose the theatre and the preview says the block carries over", 10000, () =>
    page.evaluate(() => /carries over/.test(((document.querySelector(".contact-merge-block") || {}).textContent) || "")), 30);
  await injectAxe(page);
  await axeAA(page, "contacts-merge-preview", "light");
  const trap = await dialogTrapProblems(page, '.contact-merge[role="dialog"]');
  if (trap.count < 3 || trap.escapes.length) throw new Fail(30, "CONTACTS focus left the merge dialog: " + JSON.stringify(trap));
  await control("contacts-merge-activedescendant", async () => {
    await page.evaluate(() => document.querySelector('.contact-merge input[role="combobox"]').removeAttribute("aria-activedescendant"));
    return !(await activeOk());
  });
  await page.keyboard.press("Escape");
  await until("contacts: Escape closed the merge dialog, back on Merge with…", 10000, () =>
    page.evaluate(() => !document.querySelector('.contact-merge[role="dialog"]') &&
      document.activeElement && document.activeElement.getAttribute("data-a11y-opener") === "merge" &&
      location.hash.indexOf("#/contacts/") === 0), 30);

  tally.contacts = 1;
  console.log(
    "contacts          list (search named, 4 filter toggles, Blocked senders by keyboard), page (notes named+described, VIP toggle), " +
      "merge dialog (named modal, combobox with active option, keyboard choice, trap over " + trap.count + " controls, Escape back to its opener); 3 controls tripped",
  );
  return ids;
}

async function announcementScript(page, cdp) {
  console.log("--- the announcement script (light theme; this is about the tree, not colour)");

  // ---- 1. open ------------------------------------------------------------
  await gotoHash(page, "#/inbox/review");
  await waitForDesk(page);
  // The skip link's target, then the grid — the two tab stops a keyboard user
  // actually crosses to get here.
  await page.evaluate(() => {
    document.getElementById("messages") && document.getElementById("messages").focus();
    const g = document.querySelector("#desk-grid");
    g && g.focus();
  });
  await until("the grid has focus", 10000, () =>
    page.evaluate(() => document.activeElement && document.activeElement.id === "desk-grid"),
  );
  let row = await until("open: the grid names a current row", 30000, async () => {
    const r = await readActiveRow(page, cdp);
    return r.err ? false : r;
  }, 9);
  let live = await liveText(page);
  const match = nameIsAFixtureRow(row.name);
  if (!match) {
    throw new Fail(
      7,
      'NONAME open: the current row is called "' +
        row.name +
        '", which is not <(Unread, )sender, subject, …> for any message this fixture delivered',
    );
  }
  if (row.role !== "row") throw new Fail(7, "NONAME open: the current row's role is " + row.role);
  if (live.indexOf("to review") < 0 || live.indexOf("in drawers") < 0) {
    throw new Fail(
      8,
      'NOLIVE open: the header sentence is "' + live + '" — no "to review"/"in drawers"',
    );
  }
  const liveOk = await checkLiveRegion(cdp, page);
  if (!liveOk.ok) throw new Fail(8, "NOLIVE open: " + liveOk.detail);
  announce("open", row, live);
  console.log(
    "announce open     rowindex=" +
      row.rowindex +
      "/" +
      row.rowcount +
      " selected=" +
      String(row.selected) +
      " aria-current=" +
      String(row.current) +
      " grid_labelled_by=" +
      String(row.gridLabelledBy),
  );

  // ---- 2. ArrowDown -------------------------------------------------------
  const beforeId = row.id;
  const beforeIndex = row.rowindex;
  await page.keyboard.press("ArrowDown");
  row = await until("move: the current row changed", 15000, async () => {
    const r = await readActiveRow(page, cdp);
    return r.err || r.id === beforeId ? false : r;
  }, 9);
  if (!(row.rowindex > beforeIndex)) {
    throw new Fail(
      9,
      "NOACTIVE move: aria-rowindex went " + beforeIndex + " -> " + row.rowindex,
    );
  }
  if (!nameIsAFixtureRow(row.name)) {
    throw new Fail(7, 'NONAME move: the next row is called "' + row.name + '"');
  }
  announce("move", row, await liveText(page));
  console.log("announce move     rowindex=" + beforeIndex + " -> " + row.rowindex);

  // ---- 3. archive, then reach the toast with `g u` -------------------------
  const archivedRowName = row.name;
  await page.keyboard.press("e");
  // ⚠ THE FAILURE MESSAGE READS THE TOAST IT DID GET. Without that this step
  // reported "still false after 20000ms" while the client was displaying "This
  // account has no Archive folder, so there is nothing to archive into" — the
  // whole answer, on screen, thrown away by the assertion that was waiting for a
  // different sentence.
  const toast = await untilToast(page, "archive: the toast says what happened", "Archived");
  const afterArchive = await readActiveRow(page, cdp);
  if (afterArchive.err) throw new Fail(9, "NOACTIVE archive: " + afterArchive.err);
  if (afterArchive.name === archivedRowName) {
    throw new Fail(
      11,
      "NOSTEP archive: the cursor is still on the row that was archived (" +
        archivedRowName +
        ")",
    );
  }
  announce("archive", afterArchive, toast);
  // `g u` — increment A's route to the toast. Focus, not a second undo.
  await page.keyboard.press("g");
  await page.keyboard.press("u");
  const undoBtn = await until("archive: `g u` reached the Undo button", 10000, async () => {
    const isUndo = await page.evaluate(() => {
      const a = document.activeElement;
      return !!a && a.classList.contains("toast-undo-action");
    });
    if (!isUndo) return false;
    return axCDP(cdp, ".toast-undo .toast-undo-action");
  });
  if (!undoBtn || undoBtn.name !== "Undo") {
    throw new Fail(
      7,
      "NONAME archive: the focused control is named " +
        JSON.stringify(undoBtn && undoBtn.name) +
        ", want \"Undo\"",
    );
  }
  announce("archive+g u", undoBtn, toast);
  // THE COUNTDOWN PAUSES WHILE IT IS FOCUSED. Read the timer's own accessible
  // name (which is where the number is spelled out), wait, read it again.
  const timerBefore = await axCDP(cdp, ".toast-undo .toast-countdown");
  await sleep(2200);
  const timerAfter = await axCDP(cdp, ".toast-undo .toast-countdown");
  if (!timerBefore || timerBefore.role !== "timer") {
    throw new Fail(
      11,
      "NOSTEP archive: the countdown's role is " +
        JSON.stringify(timerBefore && timerBefore.role) +
        ', want "timer" (an implicit aria-live=off is the whole point)',
    );
  }
  if (!timerAfter || timerAfter.name !== timerBefore.name) {
    throw new Fail(
      11,
      "NOSTEP archive: the focused countdown moved from " +
        JSON.stringify(timerBefore.name) +
        " to " +
        JSON.stringify(timerAfter && timerAfter.name) +
        " — the pause is not holding, so the offer expires under a reader",
    );
  }
  console.log(
    'announce archive+timer role=timer name="' +
      timerBefore.name +
      '" held_for_ms=2200 (paused while focused)',
  );
  // Escape dismisses the toast; focus must not be dropped on the floor.
  await page.keyboard.press("Escape");
  await until("archive: the toast went away", 10000, () =>
    page.evaluate(() => document.querySelectorAll("div.toasts .toast-undo").length === 0),
  );
  const afterEscape = await page.evaluate(() => {
    const a = document.activeElement;
    return { tag: a ? a.tagName.toLowerCase() : null, id: a ? a.id : null };
  });
  if (afterEscape.id !== "desk-grid") {
    throw new Fail(
      11,
      "NOSTEP archive: Escape out of the toast left focus on <" +
        afterEscape.tag +
        "> (id=" +
        JSON.stringify(afterEscape.id) +
        "), not on the grid — a reader loses their place entirely",
    );
  }
  console.log("announce archive+escape focus_returned_to=#desk-grid");

  // ---- 4. flag ------------------------------------------------------------
  const trayBefore = trayCountOf(await liveText(page));
  row = await until("flag: a current row to act on", 15000, async () => {
    const r = await readActiveRow(page, cdp);
    return r.err ? false : r;
  }, 9);
  // Since #685, archiving leaves the cursor on the NEXT message rather than on
  // the top row, and in this fixture the next one is already in the tray, so `s`
  // would take it OUT. Step down to the first row that is not in the tray, so the
  // step tests what it claims: flagging puts a message in the tray. Bounded, and
  // a fixture with no such row fails loudly instead of testing the wrong thing.
  for (let i = 0; row.name && row.name.indexOf("In the tray") >= 0; i++) {
    if (i >= 10) {
      throw new Fail(9, "NOSTEP flag: no row within 10 steps of the cursor is outside the tray (last: " + JSON.stringify(row.name) + ")");
    }
    const was = row.id;
    await page.keyboard.press("ArrowDown");
    row = await until("flag: the cursor moved to the next row", 5000, async () => {
      const r = await readActiveRow(page, cdp);
      return !r.err && r.id !== was ? r : false;
    }, 9);
  }
  console.log("announce flag     target " + JSON.stringify(row.name).slice(0, 80));
  // `s`, not `f`: on the desk `f` is Forward since #624 (it was bound twice,
  // tray toggle first, so Forward never ran). Pressing `f` here now opens a
  // composer, and the row never gains "In the tray".
  await page.keyboard.press("s");
  const flagged = await until("flag: the row's name gained \"In the tray\"", 20000, async () => {
    const r = await readActiveRow(page, cdp);
    return !r.err && r.name && r.name.indexOf("In the tray") >= 0 ? r : false;
  }, 7);
  // The COUNT is a server measurement, so it moves on the next desk refresh
  // rather than on the keystroke. Waited for, and the wait is reported.
  const flagStarted = Date.now();
  const trayAfter = await until(
    "flag: the header sentence's tray count rose",
    45000,
    async () => {
      const n = trayCountOf(await liveText(page));
      return n !== null && trayBefore !== null && n > trayBefore ? n : false;
    },
  );
  announce("flag", flagged, await liveText(page));
  console.log(
    "announce flag     in_tray " +
      trayBefore +
      " -> " +
      trayAfter +
      " after " +
      (Date.now() - flagStarted) +
      "ms",
  );

  // ---- 5. cross the line --------------------------------------------------
  const drawersBefore = drawerCountOf(await liveText(page));
  const crossingId = flagged.id;
  await page.keyboard.down("Shift");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.up("Shift");
  const crossStarted = Date.now();
  const drawersAfter = await until(
    'cross: the header sentence\'s "in drawers" rose',
    45000,
    async () => {
      const n = drawerCountOf(await liveText(page));
      return n !== null && drawersBefore !== null && n > drawersBefore ? n : false;
    },
  );
  const afterCross = await until("cross: the row left the surface", 45000, async () => {
    const r = await readActiveRow(page, cdp);
    return !r.err && r.id !== crossingId ? r : false;
  }, 9);
  announce("cross-the-line", afterCross, await liveText(page));
  console.log(
    "announce cross    in_drawers " +
      drawersBefore +
      " -> " +
      drawersAfter +
      " after " +
      (Date.now() - crossStarted) +
      "ms",
  );

  // ---- 6. the line --------------------------------------------------------
  // It is the LAST item of a virtualised list, so it has to be scrolled to
  // before it is in the document at all.
  //
  // ⚠ AT A SHORT VIEWPORT, SO THIS SCROLL IS LOAD-BEARING (#673). At 1100px the
  // whole list fits in the first window and the scroll below moved nothing, which
  // is why #666 (a window that never followed scroll) was invisible to this lane:
  // the line was drawn without any scrolling having to be seen. Here the window
  // (the visible rows plus six of overscan) is smaller than the list, so the line
  // — the LAST item — is in the document only if the list follows the scroll.
  const FULL_VIEWPORT = { width: 1680, height: 1100 };
  await page.setViewport({ width: 1680, height: 360 });
  const drawnRows = () =>
    page.evaluate(() => {
      const g = document.querySelector("#desk-grid");
      const idx = [...document.querySelectorAll("#desk-grid li[role=row]")].map((li) =>
        Number(li.getAttribute("aria-rowindex") || "0"),
      );
      return {
        count: g ? Number(g.getAttribute("aria-rowcount") || "0") : 0,
        max: idx.length ? Math.max(...idx) : 0,
        hasLine: !!document.querySelector("#desk-line-slider"),
      };
    });
  // The window can lag a resize by a frame, so wait for it to be the short one.
  const shortBefore = await until("scroll: the window is shorter than the list", 15000, async () => {
    const d = await drawnRows();
    return d.count > 0 && d.max < d.count && !d.hasLine ? d : false;
  }).catch(async () => {
    const d = await drawnRows();
    throw new Fail(
      7,
      "NOSCROLL precondition: at a 360px viewport the grid already draws rows up to " +
        d.max +
        " of " +
        d.count +
        " (line present: " +
        d.hasLine +
        "), so scrolling would prove nothing; the fixture needs more rows or the viewport is too tall",
    );
  });
  await page.evaluate(() => {
    const pane = document.querySelector(".desk-pane");
    if (pane) pane.scrollTop = pane.scrollHeight;
  });
  const shortAfter = await until("scroll: rows beyond the first window are drawn", 15000, async () => {
    const d = await drawnRows();
    return d.max > shortBefore.max && d.max === d.count ? d : false;
  }).catch(async () => {
    const d = await drawnRows();
    throw new Fail(
      7,
      "NOFOLLOW scroll: after scrolling to the bottom the grid still draws rows up to " +
        d.max +
        " of " +
        d.count +
        " (was " +
        shortBefore.max +
        "): the list is not following scroll (#666)",
    );
  });
  console.log(
    "scroll            drawn rows up to " +
      shortBefore.max +
      " -> " +
      shortAfter.max +
      " of " +
      shortAfter.count +
      " at a 360px viewport",
  );
  await waitForSelectorD(page, "#desk-line-slider", { timeout: 20000 });
  await page.evaluate(() => document.querySelector("#desk-line-slider").focus());
  const sliderBefore = await axSnapshot(page, "#desk-line-slider");
  if (!sliderBefore || sliderBefore.role !== "slider") {
    throw new Fail(
      7,
      "NONAME line: #desk-line-slider has role " +
        JSON.stringify(sliderBefore && sliderBefore.role) +
        ', want "slider"',
    );
  }
  if (!/\d+ to review, \d+ in drawers/.test(String(sliderBefore.valuetext))) {
    throw new Fail(
      7,
      'NONAME line: aria-valuetext is "' +
        sliderBefore.valuetext +
        '", not the split (desk.line.value)',
    );
  }
  console.log(
    'announce line     role=slider name="' +
      sliderBefore.name +
      '" valuenow=' +
      sliderBefore.valuemin +
      ".." +
      sliderBefore.valuemax +
      " now=" +
      (await page.evaluate(() =>
        document.querySelector("#desk-line-slider").getAttribute("aria-valuenow"),
      )) +
      ' valuetext="' +
      sliderBefore.valuetext +
      '"',
  );
  tally.announcements += 1;
  const nowBefore = Number(
    await page.evaluate(() =>
      document.querySelector("#desk-line-slider").getAttribute("aria-valuenow"),
    ),
  );
  await page.keyboard.press("ArrowUp");
  const nowAfter = await until("line: aria-valuenow rose", 15000, async () => {
    const n = Number(
      await page.evaluate(() =>
        document.querySelector("#desk-line-slider").getAttribute("aria-valuenow"),
      ),
    );
    return n > nowBefore ? n : false;
  });
  const pending = await until("line: the value text gained the preview clause", 45000, () =>
    page.evaluate(() => {
      const s = document.querySelector("#desk-line-slider");
      const vt = s ? s.getAttribute("aria-valuetext") : "";
      return /would move/.test(vt) ? vt : false;
    }),
  );
  console.log(
    "announce line+up  valuenow " +
      nowBefore +
      " -> " +
      nowAfter +
      ' valuetext="' +
      pending +
      '"',
  );
  tally.announcements += 1;
  await page.keyboard.press("Escape");
  await until("line: Escape cancelled the pending line", 15000, () =>
    page.evaluate(() => !/would move/.test(
      document.querySelector("#desk-line-slider").getAttribute("aria-valuetext") || "",
    )),
  );

  // At the same short viewport, where the list is taller than the pane: the
  // user scrolls the cursor row out of view, and the list must stay put.
  await scrollAwayFromCursor(page);

  // Back to the fixed viewport the rest of the run (and every axe pass) assumes.
  await page.setViewport(FULL_VIEWPORT);

  // ---- 7. the other half, and a drawer ------------------------------------
  await page.evaluate(() => document.querySelector("#desk-tab-review").focus());
  await page.keyboard.press("ArrowRight");
  await until("tabs: Everything else is selected", 20000, () =>
    page.evaluate(() => {
      const rest = document.querySelector("#desk-tab-rest");
      const grid = document.querySelector("#desk-grid");
      return (
        !!rest &&
        rest.getAttribute("aria-selected") === "true" &&
        !!grid &&
        grid.getAttribute("aria-labelledby") === "desk-tab-rest"
      );
    }),
  );
  const restTab = await axSnapshot(page, "#desk-tab-rest");
  console.log(
    'announce tabs     role=' +
      (restTab && restTab.role) +
      ' name="' +
      (restTab && restTab.name) +
      '" selected=' +
      String(restTab && restTab.selected) +
      " grid_labelled_by=desk-tab-rest",
  );
  tally.announcements += 1;
  const drawerSel = await focusFixtureDrawerButton(page);
  const drawerBtn = await axCDP(cdp, drawerSel);
  if (!drawerBtn || !/ drawer, \d+ message/.test(String(drawerBtn.name))) {
    throw new Fail(
      7,
      "NONAME drawer: the disclosure is called " +
        JSON.stringify(drawerBtn && drawerBtn.name) +
        ' — want "<area> drawer, N messages, <sweep>"',
    );
  }
  const rowsBefore = await page.evaluate(
    () => document.querySelectorAll("#desk-grid li.desk-item-row").length,
  );
  await page.keyboard.press("Enter");
  await until("drawer: it says it is expanded", 20000, () =>
    page.evaluate(
      () => document.querySelectorAll('.desk-drawer button[aria-expanded="true"]').length > 0,
    ),
  );
  const rowsAfter = await until("drawer: rows appeared beneath it", 30000, async () => {
    const n = await page.evaluate(
      () => document.querySelectorAll("#desk-grid li.desk-item-row").length,
    );
    return n > rowsBefore ? n : false;
  });
  announce("drawer", drawerBtn, await liveText(page));
  console.log(
    "announce drawer   aria-expanded=true rows " + rowsBefore + " -> " + rowsAfter,
  );

  // ---- 8. a fault replaces the header sentence ----------------------------
  await gotoHash(page, "#/inbox/review");
  await waitForDesk(page);
  const okSentence = await liveText(page);
  await page.setOfflineMode(true);
  const offlineStarted = Date.now();
  const offlineSentence = await untilOffline(page);
  const offlineLive = await checkLiveRegion(cdp, page);
  if (!offlineLive.ok) throw new Fail(8, "NOLIVE fault: " + offlineLive.detail);
  const hdr = await axCDP(cdp, "p.desk-header");
  announce("fault-offline", hdr, offlineSentence);
  console.log(
    "announce fault    ax_role=" +
      hdr.role +
      " ax_live=" +
      hdr.props["live"] +
      " after " +
      (Date.now() - offlineStarted) +
      "ms",
  );
  await page.setOfflineMode(false);
  const recovered = await until("fault: the header came back", 45000, async () => {
    // The `online` event already wakes the client (app/state.tsx), and this is
    // the same nudge as above for the same reason.
    await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
    const s = await liveText(page);
    return s.indexOf("Offline") < 0 ? s : false;
  }, 8);
  announce("fault-recovered", await axCDP(cdp, "p.desk-header"), recovered);
  console.log('announce fault    ok_sentence_before="' + okSentence + '"');
}

/**
 * Wait for the desk to say it is offline, and on exhaustion SAY WHAT THE PAGE
 * COULD STILL SEE.
 *
 * ⚠ THE DEADLINE IS THE CLIENT'S POLL, NOT FIVE SECONDS. core/sync.ts polls every
 * 10s (DEFAULT_POLL_MS) and going offline is only noticed when a round trip fails
 * to happen, so #447's suggested 5s window would be a coin toss on a shared
 * runner. Bounded generously, and the wait is REPORTED on the transcript: a
 * genuinely broken offline sentence still fails, it just takes this long to say
 * so.
 *
 * ⚠ AND THE PROBE IS PART OF THE ASSERTION, not decoration. `page.setOfflineMode`
 * is a claim about the browser, and "the header did not change" is consistent with
 * two completely different worlds: the client ignoring the fault, or the fault
 * never having happened. So on failure this reports `navigator.onLine`, whether a
 * service worker is controlling the document, and what a fetch to /jmap from the
 * page actually did — which is the difference between a client bug and a harness
 * that was never offline.
 */
async function untilOffline(page) {
  const started = Date.now();
  for (;;) {
    // ⚠ WAKE IT THE WAY THE BROWSER DOES, rather than waiting out a timer. With
    // the SSE push channel open the client STOPS POLLING altogether (core/sync.ts
    // `onOpen`: "Push is live: stop polling"), so how quickly a dropped connection
    // becomes a sentence depends on when EventSource notices — measured at ~2s in
    // one run and still nothing after 40s in another, on the same tree. A
    // wall-clock race is not an assertion.
    //
    // `visibilitychange` is the app's OWN immediate-sync path (app/state.tsx
    // `wake`: poll now and drain the queue, rather than waiting out the poll
    // interval), and dispatching it is dispatching the event a real tab switch
    // dispatches — not reaching into the client. `poll()` returns the in-flight
    // promise when one is running, so repeating it is free.
    await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
    const s = await liveText(page);
    if (s.indexOf("Offline") >= 0) return s;
    if (Date.now() - started >= 45000) {
      const probe = await page.evaluate(async () => {
        const out = {
          onLine: navigator.onLine,
          controlledBySW: !!(navigator.serviceWorker && navigator.serviceWorker.controller),
        };
        try {
          const res = await fetch("/jmap/session", { cache: "no-store" });
          out.fetch = "answered " + res.status;
        } catch (e) {
          out.fetch = "threw " + (e && e.message);
        }
        return out;
      });
      throw new Fail(
        8,
        "NOLIVE fault: the header sentence never said Offline after " +
          (Date.now() - started) +
          "ms. It says " +
          JSON.stringify(s) +
          ". The page reports " +
          JSON.stringify(probe) +
          " — if that fetch ANSWERED, the browser was never offline and this is the " +
          "harness, not the client.",
      );
    }
    await sleep(250);
  }
}

/** "12 in tray" out of the header sentence, or null. */
function trayCountOf(sentence) {
  const m = /(\d+) in tray/.exec(sentence || "");
  return m ? Number(m[1]) : null;
}
/** "37 in drawers" out of the header sentence, or null. */
function drawerCountOf(sentence) {
  const m = /(\d+) in drawers/.exec(sentence || "");
  return m ? Number(m[1]) : null;
}

/**
 * EVERY GLYPH-ONLY BUTTON IS NAMED IN WORDS — the check axe cannot make.
 *
 * ⚠ THIS EXISTS BECAUSE A NEGATIVE CONTROL SAID IT HAD TO. #447's control for the
 * lint half was "strip `aria-label` from a glyph-only button and watch
 * `button-name` go red". It did not go red, and the reason is worth knowing:
 * axe's `button-name` asks whether a button has an accessible name AT ALL, and a
 * button whose text content is "☰" HAS one — the glyph. So on this app, where
 * every icon button prints a real character rather than an SVG, `button-name`
 * cannot see the failure it was supposed to guard: the rail's ☰ losing its label
 * would be announced as "☰" and axe would call the page clean.
 *
 * The accessibility TREE can see it, which is the whole argument for reading the
 * tree as well as running a linter. So the assertion is here instead: for every
 * button whose visible text is one or two non-alphanumeric characters, the
 * COMPUTED NAME must contain a letter. The count is printed, because "0 badly
 * named glyph buttons" out of 0 examined is not a finding.
 */
async function glyphButtonsAreWords(page, cdp) {
  const marked = await page.evaluate(() => {
    const out = [];
    let i = 0;
    for (const b of Array.from(document.querySelectorAll("button"))) {
      b.removeAttribute("data-a11y-glyph");
      const text = (b.textContent || "").trim();
      if (text.length === 0 || text.length > 2 || /[0-9A-Za-z]/.test(text)) continue;
      // ⚠ RENDERED ONLY, and it is not a convenience. A button that is not in the
      // layout is not in the accessibility tree either, so CDP answers with an
      // ignored node and no name — which this check would read as "announced as
      // its glyph". The desk's ☰ is exactly that: `display: none` above the phone
      // breakpoint, perfectly well labelled, and the first version of this check
      // reported it as a defect. What is not announced cannot be misannounced.
      if (b.getClientRects().length === 0) continue;
      if (getComputedStyle(b).visibility === "hidden") continue;
      if (b.closest('[aria-hidden="true"]')) continue;
      b.setAttribute("data-a11y-glyph", String(i));
      out.push({ i, text, cls: String(b.className || "") });
      i += 1;
    }
    return out;
  });
  const bad = [];
  for (const g of marked) {
    const ax = await axCDP(cdp, '[data-a11y-glyph="' + g.i + '"]');
    const name = (ax && ax.name) || "";
    if (!/[A-Za-z]/.test(name)) bad.push({ glyph: g.text, cls: g.cls, name });
  }
  return { examined: marked.length, bad };
}

// ---------------------------------------------------------------------------
// The negative controls
// ---------------------------------------------------------------------------
//
// ⚠ READ THIS BEFORE CHANGING ANY OF THEM. Three assertions about a sandbox once
// passed against a page that could not have loaded at all (#131), and the whole
// reason this lane exists is that a check nobody has watched FAIL is a claim, not
// a control. Each of these breaks ONE mechanism and requires the check that
// covers it to go red. A control that PASSES fails the run.
//
// The page is RELOADED between them so each is isolated: a stripped attribute is
// not undone by the next control's `page.evaluate`.

async function control(name, run) {
  tally.controlsRun += 1;
  const tripped = await run();
  if (!tripped) {
    throw new Fail(
      10,
      "CONTROLPASSED " +
        name +
        " — the check it targets stayed green with the mechanism removed, so that " +
        "check is not evidence about the mechanism",
    );
  }
  tally.controlsTripped += 1;
  console.log("control " + name + " tripped=yes");
}

/**
 * A COLD DESK between controls, so each one is isolated.
 *
 * ⚠ `page.goto(<the url it is already on>)` DOES NOT RELOAD — a navigation that
 * differs only in the fragment is same-document — and this function used to do
 * exactly that. The consequence was measurable: the nameless button control 1a
 * injects survived into control 1b and was counted as a finding by the check
 * control 1b exists to falsify. `load()` sets the hash and then reloads, which is
 * the only form that is a reload whatever the current URL is.
 */
async function reloadDesk(page) {
  await load(page, "#/inbox/review");
  await waitForDesk(page);
}

async function negativeControls(page, cdp) {
  console.log("--- negative controls (each must make its own check go red)");

  // 1a. THE axe AA GATE CAN GO RED ON THIS PAGE -> `button-name`.
  //
  // A button with NO accessible name at all is injected: a glyph inside an
  // `aria-hidden` span, which is the commonest real form of this defect and the
  // one `button-name` is written for. Without this, "violations=0" on fifteen
  // surfaces is consistent with axe having been injected and never having found
  // anything it could report — the #131 shape, in the lint half.
  await reloadDesk(page);
  await injectAxe(page);
  await page.evaluate(() => {
    const b = document.createElement("button");
    b.type = "button";
    b.id = "a11y-control-nameless";
    b.innerHTML = '<span aria-hidden="true">✕</span>';
    document.querySelector("main#messages").appendChild(b);
  });
  await control("axe-button-name", async () => {
    const n = await page.evaluate(
      (shared, tags) =>
        window.axe
          .run(document, Object.assign({}, shared, { runOnly: { type: "tag", values: tags } }))
          .then((r) => {
            const v = r.violations.find((x) => x.id === "button-name");
            return v ? v.nodes.length : 0;
          }),
      AXE_SHARED,
      AA_TAGS,
    );
    console.log("control axe-button-name axe_button_name_violations=" + n);
    return n >= 1;
  });

  // 1b. A GLYPH-ONLY BUTTON LOSES ITS LABEL -> `glyphButtonsAreWords`.
  //
  // ⚠ AND THIS IS THE CONTROL THAT CHANGED THE LANE. #447 asked for exactly this
  // strip and expected axe to catch it; it does not, because the glyph itself is
  // an accessible name as far as `button-name` is concerned (see
  // glyphButtonsAreWords). Measured, not assumed: the first run of this control
  // stripped `aria-label="Mailboxes"` off the rail's ☰ and axe reported
  // `button_name_violations=0`. So the positive check moved to the accessibility
  // tree, and this control now targets THAT.
  await reloadDesk(page);
  const before = await glyphButtonsAreWords(page, cdp);
  if (before.examined === 0) {
    throw new Fail(
      10,
      "CONTROLPASSED glyph-name — no glyph-only buttons on the desk to examine, so " +
        "the positive check above proved nothing",
    );
  }
  if (before.bad.length > 0) {
    throw new Fail(
      7,
      "NONAME " +
        before.bad.length +
        " of " +
        before.examined +
        " glyph-only buttons are announced as their glyph: " +
        JSON.stringify(before.bad),
    );
  }
  console.log(
    "check glyph-buttons-are-words examined=" + before.examined + " announced_as_glyph=0",
  );
  // Stripped from a button the CHECK ABOVE ACTUALLY EXAMINED — `data-a11y-glyph`
  // is the mark it just left — rather than from whatever the document happens to
  // match. A control aimed at an element the assertion never looks at proves
  // nothing, and the hidden ☰ is precisely such an element.
  //
  // `:not([title])` matters too: a `title` is an accessible name in its own right,
  // so a button that also had one would not go red, and the control would be
  // telling us the assertion is not load-bearing for that element.
  const stripped = await page.evaluate(() => {
    const glyph = document.querySelector("[data-a11y-glyph][aria-label]:not([title])");
    if (!glyph) return null;
    const label = glyph.getAttribute("aria-label");
    glyph.removeAttribute("aria-label");
    return { label, cls: String(glyph.className || ""), text: (glyph.textContent || "").trim() };
  });
  if (!stripped) {
    throw new Fail(
      10,
      "CONTROLPASSED glyph-name — no glyph-only button[aria-label] without a title " +
        "to strip, so the control could not be run at all",
    );
  }
  console.log(
    'control glyph-name target=".' +
      String(stripped.cls).split(/\s+/).join(".") +
      '" glyph="' +
      stripped.text +
      '" removed_aria_label="' +
      stripped.label +
      '"',
  );
  await control("glyph-name", async () => {
    const after = await glyphButtonsAreWords(page, cdp);
    console.log(
      "control glyph-name examined=" +
        after.examined +
        " announced_as_glyph=" +
        after.bad.length +
        " " +
        JSON.stringify(after.bad.map((b) => b.glyph)),
    );
    return after.bad.length >= 1;
  });

  // 2. THE ONE LIVE REGION STOPS BEING ONE -> the fault step's NOLIVE.
  await reloadDesk(page);
  const killed = await page.evaluate(() => {
    const p = document.querySelector("p.desk-header");
    if (!p) return false;
    p.removeAttribute("aria-live");
    p.removeAttribute("role");
    return !p.hasAttribute("aria-live") && !p.hasAttribute("role");
  });
  if (!killed) throw new Fail(10, "CONTROLPASSED live-region — could not strip p.desk-header");
  await control("live-region", async () => {
    const check = await checkLiveRegion(cdp, page);
    console.log("control live-region check_ok=" + check.ok + " " + check.detail);
    // The control is only meaningful if React has not silently re-asserted the
    // attributes behind us; `role` and `aria-live` are constants in
    // HeaderSentence.tsx, so it does not — and this line is what would say so if
    // that ever changed.
    const back = await page.evaluate(() => {
      const p = document.querySelector("p.desk-header");
      return !!p && (p.hasAttribute("role") || p.hasAttribute("aria-live"));
    });
    if (back) {
      throw new Fail(
        10,
        "CONTROLPASSED live-region — the attributes were re-asserted by the app " +
          "between the strip and the read, so this control observed nothing",
      );
    }
    return check.ok === false;
  });

  // 2b. THE HELD CHIP ANNOUNCES THE WRONG THING -> heldChipCheck (#675).
  //
  // A DEFECT axe CANNOT SEE, on purpose: the group keeps an accessible name,
  // just not one that says what was held or that it was held — so `aria-*`
  // rules all pass. The axe count is printed beside the trip to show it.
  await openTheHeldInlineMessage(page);
  await injectAxe(page);
  await page.evaluate(() => {
    const g = document.querySelector(".attachment-withheld");
    if (g) g.setAttribute("aria-label", "Attachments");
  });
  await control("held-chip-name", async () => {
    const r = await heldChipCheck(page);
    const axeN = await page.evaluate(
      (shared, tags) =>
        window.axe
          .run(document, Object.assign({}, shared, { runOnly: { type: "tag", values: tags } }))
          .then((res) => res.violations.reduce((n, v) => n + v.nodes.length, 0)),
      AXE_SHARED,
      AA_TAGS,
    );
    console.log("control held-chip-name check=" + JSON.stringify(r.why) + " axe_violations=" + axeN);
    return !r.ok;
  });

  // 3. THE GRID STOPS NAMING A CURRENT ROW -> the move step's NOACTIVE.
  //
  // ⚠ THIS CONTROL TARGETS THE READ, NOT THE KEYSTROKE, and the distinction is
  // worth writing down: `aria-activedescendant` is a REACT-RENDERED attribute
  // driven by the cursor, so pressing ArrowDown after stripping it would make
  // React put it straight back. What the move step's assertion actually depends
  // on is `readActiveRow` — read the attribute, resolve the id in the document,
  // read that node's name — and that is what is falsified here.
  await reloadDesk(page);
  const unset = await page.evaluate(() => {
    const g = document.querySelector("#desk-grid");
    if (!g) return false;
    g.removeAttribute("aria-activedescendant");
    return !g.hasAttribute("aria-activedescendant");
  });
  if (!unset) throw new Fail(10, "CONTROLPASSED active-descendant — could not strip #desk-grid");
  await control("active-descendant", async () => {
    const r = await readActiveRow(page, cdp);
    console.log("control active-descendant read=" + JSON.stringify(r.err || r.name));
    return !!r.err;
  });
}

// ---------------------------------------------------------------------------
// MAIL-PROVIDER SIGN-IN (#52, v1.96) — exit 33 OAUTH
// ---------------------------------------------------------------------------
//
// The surfaces a person meets when an account signs in at Google or
// Microsoft: the Connect buttons and their honesty interstitial, an account
// waiting to be reconnected (its Sign-in group, and the banner across the app),
// a Gmail account the server holds with sync off, and the message the
// provider's sign-in comes back with.
//
// ⚠ THE PROVIDERS AND ACCOUNTS ARE A FIXTURE IN THE PAGE, NOT ON THE SERVER.
// This lane's server has no OAuth client configured (there is no provider to
// configure against), so `oauthProviders` is empty and no OAuth account can
// exist. installOAuthFixture wraps the page's fetch, switched on by a
// sessionStorage flag, and adds exactly three things to what the REAL server
// answered: both providers in the accounts capability, an Outlook account in
// needs_reauth and a Gmail account with sync held, to Account/get and
// Account/getHealth. Every other byte is the server's. What this step checks
// is therefore the CLIENT's rendering of those states, which is what the
// accessibility claims are about; the server half is covered by the Go suites.
// Continue (which would leave for the provider) is never pressed.
//
// The return message is real end to end: the page is loaded at
// `/?oauth=denied&provider=google#/settings/accounts`, the app reads the code,
// and the query must be gone from the address bar afterwards.
//
// TWO IN-RUN NEGATIVE CONTROLS: the banner region losing its role must make the
// live-region check go red, and the interstitial losing its name must make the
// named-modal check go red.

const OAUTH_FIXTURE_FLAG = "a11y.oauthFixture";
const OAUTH_FIX = {
  reauthId: "01JB0000000000000000RECN01",
  heldId: "01JB0000000000000000GMAH01",
};

/** Installed once, before the first load; does nothing until the flag is set. */
async function installOAuthFixture(page) {
  await page.evaluateOnNewDocument((flag, fx) => {
    const cap = "urn:mailroom:accounts";
    const status = () => ({
      lastSuccessAt: null, lastAttemptAt: null, lagSeconds: null, lastError: null, lastErrorAt: null, consecutiveFailures: 0,
    });
    const common = { imapPort: 993, writebackEnabled: true, color: null, notifyPush: true, outboundDailyQuota: 300, authKind: "oauth2" };
    const accounts = [
      Object.assign({}, common, {
        id: fx.reauthId, name: "Work (Outlook)", providerProfile: "microsoft", imapHost: "outlook.office365.com",
        smtpHost: "smtp.office365.com", smtpPort: 587, syncEnabled: true,
        authState: "needs_reauth", authProvider: "microsoft", authAddress: "work@example.com",
      }),
      Object.assign({}, common, {
        id: fx.heldId, name: "Family Gmail", providerProfile: "gmail", imapHost: "imap.gmail.com",
        smtpHost: "smtp.gmail.com", smtpPort: 465, syncEnabled: false,
        authState: "active", authProvider: "google", authAddress: "family@example.com",
      }),
    ];
    const health = [
      {
        id: fx.reauthId, name: "Work (Outlook)", syncStatus: status(), deliveryFailures: { count: 0, recent: [] },
        deadLetters: { count: 0, reasons: [] }, sendQuota: { sent: 0, limit: 300 }, writeback: { enabled: true, suppressedIntents: 0 },
        auth: { kind: "oauth2", provider: "microsoft", state: "needs_reauth", reason: "invalid_grant", stateChangedAt: "2026-10-27T08:00:00Z", lastRefreshAt: "2026-10-27T07:00:00Z" },
      },
      {
        id: fx.heldId, name: "Family Gmail", syncStatus: status(), deliveryFailures: { count: 0, recent: [] },
        deadLetters: { count: 0, reasons: [] }, sendQuota: { sent: 0, limit: 500 }, writeback: { enabled: true, suppressedIntents: 0 },
        syncHold: { code: "gmail_sync_pending", message: "Gmail sync arrives in the next update; your account is connected and ready." },
        auth: { kind: "oauth2", provider: "google", state: "active", reason: null, stateChangedAt: "2026-10-27T08:00:00Z", lastRefreshAt: null },
      },
    ];
    const withProviders = (c) => Object.assign({}, c, { oauthProviders: ["google", "microsoft"] });
    const json = (obj, res) => new Response(JSON.stringify(obj), { status: res.status, headers: { "content-type": "application/json" } });
    const orig = window.fetch;
    window.fetch = async function (input, init) {
      const res = await orig.call(this, input, init);
      let on = false;
      try {
        on = window.sessionStorage.getItem(flag) === "1";
      } catch (e) {
        on = false;
      }
      if (!on || !res.ok) return res;
      const url = typeof input === "string" ? input : (input && input.url) || "";
      try {
        if (url.indexOf("/jmap/session") >= 0) {
          const s = await res.clone().json();
          if (s.capabilities && s.capabilities[cap]) s.capabilities[cap] = withProviders(s.capabilities[cap]);
          for (const a of Object.values(s.accounts || {})) {
            if (a.accountCapabilities && a.accountCapabilities[cap]) a.accountCapabilities[cap] = withProviders(a.accountCapabilities[cap]);
          }
          return json(s, res);
        }
        const body = init && typeof init.body === "string" ? init.body : "";
        if (body.indexOf('"Account/get') < 0) return res;
        const j = await res.clone().json();
        for (const r of j.methodResponses || []) {
          const add = r[0] === "Account/get" ? accounts : r[0] === "Account/getHealth" ? health : null;
          if (!add || !r[1] || !Array.isArray(r[1].list)) continue;
          for (const x of add) if (!r[1].list.some((y) => y.id === x.id)) r[1].list.push(x);
        }
        return json(j, res);
      } catch (e) {
        return res;
      }
    };
  }, OAUTH_FIXTURE_FLAG, OAUTH_FIX);
}

async function setOAuthFixture(page, on) {
  await page.evaluate(
    (flag, v) => {
      if (v) window.sessionStorage.setItem(flag, "1");
      else window.sessionStorage.removeItem(flag);
      window.sessionStorage.removeItem("mailroom.reconnectBanner.dismissed");
    },
    OAUTH_FIXTURE_FLAG,
    on,
  );
}

async function openOAuthAccounts(page) {
  await load(page, "#/settings/accounts");
  await until(
    "oauth: the Connect buttons and the reconnect banner are drawn",
    30000,
    () => page.evaluate(() => !!document.querySelector(".setting-oauth-buttons") && !!document.querySelector(".reconnect-banner")),
    33,
    () => page.evaluate(() => (document.querySelector(".settings-page") || document.body).textContent.slice(0, 400)),
  );
}

async function openOAuthDialog(page) {
  await openOAuthAccounts(page);
  await page.evaluate(() => {
    [...document.querySelectorAll(".setting-oauth-buttons button")].find((b) => b.textContent === "Connect Gmail").focus();
  });
  await page.keyboard.press("Enter");
  await waitForSelectorD(page, '.oauth-connect[role="dialog"]', { timeout: 10000 });
}

async function openOAuthAccount(page, id) {
  await load(page, "#/settings/accounts/" + id);
  await until(
    "oauth: the account " + id + " is drawn",
    30000,
    () => page.evaluate(() => [...document.querySelectorAll(".settings-page legend")].some((l) => /Sign-in|State/.test(l.textContent))),
    33,
  );
}

/** Every button in scope is named by (at least) its visible words (WCAG 2.5.3). */
async function oauthButtonNameProblems(page, cdp, scope) {
  const ids = await page.evaluate((s) => {
    let n = 0;
    return [...document.querySelectorAll(s + " button")]
      .filter((b) => b.offsetParent !== null)
      .map((b) => {
        if (!b.id) b.id = "a11y-oauth-b-" + n++;
        return { id: b.id, text: (b.textContent || "").trim() };
      });
  }, scope);
  const bad = [];
  for (const b of ids) {
    const ax = await axCDP(cdp, "#" + b.id);
    const name = ((ax && ax.name) || "").trim();
    if (!b.text || name.indexOf(b.text) < 0) bad.push({ id: b.id, text: b.text, name });
  }
  return { count: ids.length, bad };
}

async function oauthBannerProblems(cdp) {
  const live = await axCDP(cdp, ".reconnect-banner-region");
  if (!live || live.role !== "status" || live.props["live"] !== "polite") return ["the reconnect banner is not a polite live region: " + JSON.stringify(live)];
  return [];
}

async function oauthDialogProblems(cdp) {
  const dlg = await axCDP(cdp, '.oauth-connect[role="dialog"]');
  if (!dlg || dlg.role !== "dialog" || dlg.props["modal"] !== true || !dlg.name || dlg.name.indexOf("Connect your Gmail account") < 0) {
    return ["the interstitial is not a modal dialog named by its title: " + JSON.stringify(dlg)];
  }
  return [];
}

async function oauthSurfaces(page, cdp) {
  await setOAuthFixture(page, true);
  const problems = [];
  try {
    for (const theme of [THEMES[0], THEMES[1]]) {
      await chooseTheme(page, theme);
      const light = theme === THEMES[0];

      // ---- the list: Connect buttons, the banner, the status words ---------
      await openOAuthAccounts(page);
      await injectAxe(page);
      await axeAA(page, "settings-accounts-oauth", theme.name);
      if (light) {
        problems.push(...(await oauthBannerProblems(cdp)));
        const facts = await page.evaluate(() => ({
          banner: (document.querySelector(".reconnect-banner") || {}).textContent || "",
          list: (document.querySelector(".setting-account-list") || {}).textContent || "",
        }));
        if (facts.banner.indexOf("Work (Outlook) needs you to sign in again") < 0) problems.push("banner text: " + facts.banner);
        if (facts.list.indexOf("needs reconnecting") < 0) problems.push("the list does not say the account needs reconnecting: " + facts.list);
        const group = await axCDP(cdp, ".setting-oauth-connect");
        if (!group || group.role !== "group" || group.name !== "Connect an account by signing in") problems.push("Connect group: " + JSON.stringify(group));
        const names = await oauthButtonNameProblems(page, cdp, ".setting-oauth-connect");
        if (names.count !== 2 || names.bad.length) problems.push("Connect buttons: " + JSON.stringify(names));
        const bnames = await oauthButtonNameProblems(page, cdp, ".reconnect-banner");
        if (bnames.count !== 2 || bnames.bad.length) problems.push("banner buttons: " + JSON.stringify(bnames));
      }

      // ---- the honesty interstitial -----------------------------------------
      await openOAuthDialog(page);
      await injectAxe(page);
      await axeAA(page, "settings-oauth-connect-dialog", theme.name);
      if (light) {
        problems.push(...(await oauthDialogProblems(cdp)));
        const first = await page.evaluate(() => (document.activeElement || {}).textContent);
        if (first !== "Cancel") problems.push("focus did not start on Cancel: " + first);
        const said = await page.evaluate(() => document.querySelector('.oauth-connect[role="dialog"]').textContent);
        for (const want of ["read, send, organise and delete", "Whoever runs this Mailroom server", "unverified"]) {
          if (said.indexOf(want) < 0) problems.push("the interstitial does not say " + JSON.stringify(want));
        }
        const trap = await dialogTrapProblems(page, '.oauth-connect[role="dialog"]');
        if (trap.count < 2 || trap.escapes.length) problems.push("focus left the interstitial: " + JSON.stringify(trap));
        await page.keyboard.press("Escape");
        await until("oauth: Escape closed the interstitial", 10000, () => page.evaluate(() => !document.querySelector(".oauth-connect")), 33);
        const back = await page.evaluate(() => (document.activeElement || {}).textContent);
        if (back !== "Connect Gmail") problems.push("Escape did not return focus to Connect Gmail: " + back);
      }

      // ---- an account waiting to be reconnected ------------------------------
      await openOAuthAccount(page, OAUTH_FIX.reauthId);
      await injectAxe(page);
      await axeAA(page, "settings-account-reconnect", theme.name);
      if (light) {
        const f = await page.evaluate(() => {
          const legend = [...document.querySelectorAll(".settings-page legend")].find((l) => l.textContent === "Sign-in");
          const set = legend && legend.closest("fieldset");
          if (set && !set.id) set.id = "a11y-oauth-signin";
          return {
            signIn: !!set,
            text: set ? set.textContent : "",
            password: !!document.querySelector('.settings-page input[type="password"]'),
            credentials: [...document.querySelectorAll(".settings-page button")].some((b) => /Change credentials/.test(b.textContent)),
          };
        });
        if (!f.signIn) problems.push("no Sign-in group on the account waiting to be reconnected");
        if (f.password || f.credentials) problems.push("an OAuth account offers a password or credentials editor");
        for (const want of ["Signs in with Microsoft as work@example.com", "nothing is lost", "invalid_grant"]) {
          if (f.text.indexOf(want) < 0) problems.push("the Sign-in group does not say " + JSON.stringify(want) + ": " + f.text);
        }
        if (f.signIn) {
          const g = await axCDP(cdp, "#a11y-oauth-signin");
          // Case-insensitively: the legend is drawn in capitals by CSS, and
          // Chromium's computed name follows text-transform.
          if (!g || g.role !== "group" || String(g.name || "").toLowerCase() !== "sign-in") {
            problems.push("Sign-in group name: " + JSON.stringify(g));
          }
          const names = await oauthButtonNameProblems(page, cdp, "#a11y-oauth-signin");
          const labels = await page.evaluate(() => [...document.querySelectorAll("#a11y-oauth-signin button")].map((b) => b.textContent));
          if (names.bad.length || labels.indexOf("Reconnect") < 0 || labels.indexOf("Disconnect…") < 0) {
            problems.push("Sign-in buttons: " + JSON.stringify({ names, labels }));
          }
        }
      }

      // ---- a Gmail account with sync held --------------------------------------
      await openOAuthAccount(page, OAUTH_FIX.heldId);
      await injectAxe(page);
      await axeAA(page, "settings-account-held", theme.name);
      if (light) {
        const f = await page.evaluate(() => ({
          text: (document.querySelector(".settings-page") || {}).textContent || "",
          toggle: [...document.querySelectorAll(".settings-page button")].some((b) => /^(Pause|Resume) sync$/.test(b.textContent)),
        }));
        if (f.text.indexOf("Gmail sync arrives in the next update") < 0) problems.push("the held Gmail account does not show its hold");
        if (f.toggle) problems.push("the held Gmail account offers a sync toggle the server would refuse");
      }
    }

    // ---- the result the provider came back with: real, end to end ---------------
    await chooseTheme(page, THEMES[0]);
    await settled(page, "oauth return", () =>
      page.goto(BASE + "/?oauth=denied&provider=google#/settings/accounts", { waitUntil: "networkidle2", timeout: 60000 }),
    );
    await until("oauth: the result message is shown", 30000, () =>
      page.evaluate(() => /You cancelled at Google/.test((document.querySelector(".oauth-result") || {}).textContent || "")),
      33,
    );
    await injectAxe(page);
    await axeAA(page, "oauth-return-message", "light");
    const after = await page.evaluate(() => ({ search: window.location.search, hash: window.location.hash }));
    if (after.search !== "" || after.hash !== "#/settings/accounts") problems.push("the query was not taken off the URL: " + JSON.stringify(after));
    const region = await axCDP(cdp, ".oauth-result-region");
    if (!region || region.role !== "status") problems.push("the result message is not in a status region: " + JSON.stringify(region));
    await page.reload({ waitUntil: "networkidle2", timeout: 60000 });
    await sleep(500);
    const again = await page.evaluate(() => !!document.querySelector(".oauth-result"));
    if (again) problems.push("a reload showed the sign-in result again");

    // ---- the banner stays dismissed for the session -----------------------------
    await openOAuthAccounts(page);
    await page.evaluate(() => {
      [...document.querySelectorAll(".reconnect-banner button")].find((b) => b.textContent === "Dismiss").click();
    });
    await sleep(300);
    await load(page, "#/settings/accounts");
    await sleep(800);
    if (await page.evaluate(() => !!document.querySelector(".reconnect-banner"))) problems.push("the dismissed banner came back in the same session");

    if (problems.length) throw new Fail(33, "OAUTH " + problems.join("\n  "));

    // ---- negative controls --------------------------------------------------------
    await setOAuthFixture(page, true);
    await openOAuthAccounts(page);
    await control("oauth-banner-region", async () => {
      await page.evaluate(() => document.querySelector(".reconnect-banner-region").removeAttribute("role"));
      return (await oauthBannerProblems(cdp)).length > 0;
    });
    await openOAuthDialog(page);
    await control("oauth-dialog-name", async () => {
      await page.evaluate(() => document.querySelector('.oauth-connect[role="dialog"]').removeAttribute("aria-labelledby"));
      return (await oauthDialogProblems(cdp)).length > 0;
    });
  } finally {
    await setOAuthFixture(page, false);
  }
  tally.oauth = 1;
  console.log(
    "oauth             Connect buttons and their interstitial (named modal, focus starts on Cancel, trapped, Escape back to " +
      "the opener), the reconnect banner (polite region, named buttons, dismissed for the session), an account waiting to " +
      "be reconnected (Sign-in group, no server editor), a held Gmail account (its hold, no toggle) and the sign-in result " +
      "(shown once, query cleared); axe AA in light and dark; 2 controls tripped",
  );
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

/**
 * NOTHING IS WIDER THAN A PHONE (#798).
 *
 * On iPhone Safari the WHOLE interface panned sideways while a newsletter was
 * open. Two things overflowed. The top bar's search box would not shrink, and it
 * pushed ⟳ and ⚙ past the edge: the document was 422 px wide on a 375 px phone,
 * 498 px with a query in the box. The phone's reading-pane row had six buttons
 * and needed 416 px, and `.thread-pane`'s `overflow-y: auto` makes x `auto` too,
 * so the whole pane scrolled sideways under it. Neither is visible at the 1680 px
 * this lane otherwise runs at. The 360 px step above only measures the search
 * options form.
 *
 * So at 375x667 AND 390x844 (`isMobile`, touch, 3x), on every main view, this
 * step asserts:
 *   * `innerWidth` is the phone's width. Chromium's mobile mode WIDENS the layout
 *     viewport to fit overflowing content (zoom out to fit), so a page that
 *     overflows reports a larger innerWidth. On v1.92.0 that was 389/465, and
 *     the per-element test against it found nothing;
 *   * `document.documentElement.scrollWidth <= innerWidth`;
 *   * no element lies outside the viewport (`right > innerWidth + 1` or
 *     `left < -1`), unless an in-viewport ancestor CLIPS it without scrolling
 *     (an ellipsis, a rounded card);
 *   * no element SCROLLS sideways (`overflow-x` auto/scroll with scrollWidth >
 *     clientWidth), except an element marked data-scroll-x="intentional" that is
 *     focusable and named (#896). That is the reading pane's failure, which a
 *     clip-only test would excuse, since the pane "clipped" its own overflow by
 *     scrolling;
 *   * the reading pane's phone row shows its labels whole (four buttons and
 *     More; a truncated label means a button was squeezed in);
 *   * in the reading view the DOCUMENT is what scrolls, the pane is not a
 *     scroller, and the action row and bottom bar stay on screen after
 *     scrolling. Safari collapses its toolbars only on document scroll, the
 *     second half of #798.
 *
 * THE VIEWS: desk, all mail, a search with a query in the box (the screenshot's
 * state), the reading pane with a WIDE NEWSLETTER (the fixture's 640 px ESP
 * table: it must scroll inside its frame, never widen the page) and with a plain
 * message, the same newsletter opened from the search, More opened on the
 * phone's row (its menu must be on screen), compose, and settings.
 *
 * IN-RUN NEGATIVE CONTROLS (#131's rule): before trusting a clean verdict, the
 * probe is run against injected faults and must name each one. A top bar
 * forced to 640 px (the document-level shape), and the phone's reading-pane row
 * forced to 500 px (the scroll-inside-a-pane shape). A probe that cannot see
 * those is not evidence that the views are clean. The two later checks have
 * their own: the newsletter's frame put back at the phone's width unscaled must
 * be named FRAME-SCROLLS, and the search box's clipped buttons put back in
 * flow must leave fewer than 12 characters; the desktop pass forces its row to
 * 900 px and must see the pane scroll.
 *
 * SINCE #801-#803 the step also runs at 320x568 (the row's words may truncate
 * there, by design; nothing may overflow), measures the newsletter FROM INSIDE
 * its frame (scrollWidth against clientWidth: scaled to fit, nothing to
 * scroll), measures the search input's room against a 12-character sample, and
 * repeats the reading views at 1440x900 with a mouse.
 *
 * CHROMIUM ONLY: this lane's browser is the pinned Chromium image. WebKit, which
 * is what iOS Safari is, was measured by hand for #798 and is not gated here.
 */
const PHONE_SIZES = [
  { width: 320, height: 568 },
  { width: 375, height: 667 },
  { width: 390, height: 844 },
];
// The desktop pass (#803): the width at which the default split gave the reading
// pane about 493 px and its eight-button row ran under the pane's edge.
const DESKTOP_SIZE = { width: 1440, height: 900 };
const SEARCH_MIN_CHARS = 12;

/**
 * #801: a newsletter wider than the phone is scaled to fit it, so its frame has
 * nothing to scroll. Measured from INSIDE the frame (puppeteer reaches the
 * opaque-origin frame through CDP): the document's scrollWidth against its
 * clientWidth, and the frame's own box against the phone.
 */
async function newsletterFrameFit(page, phoneWidth) {
  const handle = await page.$("iframe.message-html");
  if (!handle) return ["NO-FRAME the reading pane has no message frame"];
  const frame = await handle.contentFrame();
  const inner = await frame.evaluate(() => ({
    sw: document.documentElement.scrollWidth,
    cw: document.documentElement.clientWidth,
  }));
  const box = await page.evaluate(() => {
    const f = document.querySelector("iframe.message-html");
    const r = f.getBoundingClientRect();
    return { w: Math.round(r.width), scale: f.style.transform || "none", styleW: f.style.width || "auto" };
  });
  const bad = [];
  if (inner.sw > inner.cw + 1) {
    bad.push(
      "FRAME-SCROLLS the newsletter scrolls sideways inside its frame (scrollWidth=" + inner.sw + " > clientWidth=" + inner.cw +
        "; frame box " + box.w + "px, " + box.scale + ", width " + box.styleW + ")",
    );
  }
  if (inner.sw < 500) bad.push("FRAME-NOT-WIDE the fixture's frame is only " + inner.sw + "px wide: this check is not looking at a wide newsletter");
  if (box.w > phoneWidth + 1) bad.push("FRAME-WIDER-THAN-PHONE the frame box is " + box.w + "px on a " + phoneWidth + "px phone");
  return bad;
}

/**
 * #802: room for a query. How many characters of a typical word fit in the
 * input, from the same font the input draws with: the input's content width
 * against a 12-character sample, so the check does not depend on the length of
 * the query the view happens to have typed.
 */
function searchRoom() {
  const i = document.querySelector(".search-input");
  const cs = getComputedStyle(i);
  const c = document.createElement("canvas").getContext("2d");
  c.font = cs.font;
  const inner = i.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
  const sample = "abcdefghijkl";
  let chars = 0;
  while (chars < 40 && c.measureText(sample.repeat(4).slice(0, chars + 1)).width <= inner) chars += 1;
  return { chars, inner: Math.round(inner) };
}

function phoneWidthProbe(limit) {
  // Measured against the PHONE'S width, not innerWidth: in Chromium's mobile
  // mode an overflowing page widens innerWidth, which would hide every offender.
  const iw = limit;
  const path = (el) => {
    const parts = [];
    for (let e = el; e && e.nodeType === 1 && parts.length < 6; e = e.parentElement) {
      let s = e.tagName.toLowerCase();
      if (e.id) s += "#" + e.id;
      const cls = (typeof e.className === "string" ? e.className : "").trim().split(/\s+/).filter(Boolean).slice(0, 2);
      if (cls.length) s += "." + cls.join(".");
      parts.unshift(s);
      if (e.id) break;
    }
    return parts.join(" > ");
  };
  const scrollsX = (el) => {
    const o = getComputedStyle(el).overflowX;
    return (o === "auto" || o === "scroll") && el.scrollWidth > el.clientWidth + 1;
  };
  // #896: a scroller may scroll sideways on purpose (the API-keys setup
  // snippets: a wrapped line would look different from the one you copy). Exempt
  // it from SCROLLS-SIDEWAYS only while it stays usable for the people a mouse
  // does not serve:
  //   * data-scroll-x="intentional" marks the overflow as deliberate, so a
  //     future regression that starts scrolling something else does not inherit
  //     the exemption;
  //   * it must be keyboard-focusable (tabindex >= 0): without a wheel or a
  //     touch swipe, tabbing to the block is the only way a keyboard user gets
  //     to the off-screen text;
  //   * it must be named (aria-labelledby resolving to non-empty text, or a
  //     non-empty aria-label): a screen-reader user must know what the block is
  //     before they reach it.
  // The OUTSIDE check is NOT waived: the box itself must still fit the screen.
  const intentionalScroller = (el) => {
    if (el.getAttribute("data-scroll-x") !== "intentional") return false;
    if (!(el.tabIndex >= 0)) return false;
    const labelledby = el.getAttribute("aria-labelledby");
    if (labelledby) {
      const named = labelledby
        .split(/\s+/)
        .filter(Boolean)
        .some((id) => {
          const e = document.getElementById(id);
          return !!e && (e.textContent || "").trim().length > 0;
        });
      if (named) return true;
    }
    return (el.getAttribute("aria-label") || "").trim().length > 0;
  };
  const describe = (el, why) => {
    const r = el.getBoundingClientRect();
    const cs = getComputedStyle(el);
    return (
      why + " " + path(el) + " [" + Math.round(r.left) + ".." + Math.round(r.right) + " w=" + Math.round(r.width) +
      (why === "SCROLLS-SIDEWAYS" ? " scrollWidth=" + el.scrollWidth + " clientWidth=" + el.clientWidth : "") +
      " width:" + cs.width + " min-width:" + cs.minWidth + " flex:" + cs.flex + " white-space:" + cs.whiteSpace + "]"
    );
  };
  const out = new Set();
  const offenders = [];
  for (const el of document.querySelectorAll("body *")) {
    const cs = getComputedStyle(el);
    if (cs.display === "none" || cs.visibility === "hidden") continue;
    // Off-screen until focused, on purpose (left:-9999px). It cannot widen
    // the page: overflow to the LEFT of the origin is not scrollable.
    if (el.classList.contains("skip-link")) continue;
    if (scrollsX(el) && !intentionalScroller(el)) offenders.push(describe(el, "SCROLLS-SIDEWAYS"));
    const r = el.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) continue;
    if (!(r.right > iw + 1 || r.left < -1)) continue;
    let clipped = false;
    for (let a = el.parentElement; a && a !== document.body; a = a.parentElement) {
      if (getComputedStyle(a).overflowX === "visible") continue;
      const ar = a.getBoundingClientRect();
      // An intentional scroller (#896) contains its overflow by design: content
      // past the screen edge inside it is reached by scrolling the block, so it
      // clips like any non-scrolling ancestor. Its own box must still fit.
      if (ar.right <= iw + 1 && ar.left >= -1 && (!scrollsX(a) || intentionalScroller(a))) clipped = true;
      break;
    }
    if (clipped) continue;
    out.add(el);
    if (el.parentElement && out.has(el.parentElement)) continue;
    offenders.push(describe(el, "OUTSIDE"));
  }
  return { iw: window.innerWidth, scrollWidth: document.documentElement.scrollWidth, offenders };
}

async function phoneWidthSurfaces(page, contactIds) {
  const plain = FIX.messages.find((m) => m.subject === "Lunch on Thursday?") || FIX.messages[0];
  const waitPane = async (page, subject) => {
    await waitForSelectorD(page, "aside.thread-pane", { timeout: 30000 });
    await until("phone-width: the open message is " + JSON.stringify(subject), 30000, () =>
      page.evaluate((s) => {
        const h = document.querySelector("h1.thread-subject");
        return !!h && h.textContent.indexOf(s) >= 0;
      }, subject),
    );
  };
  // The newsletter is only measurable once its frame has its height and the
  // inline hero is in. Before that the frame is a placeholder, and a narrow
  // placeholder proves nothing about a 640 px table.
  const waitNewsletter = async (page) => {
    await waitPane(page, FIX.newsletter.subject);
    await until("phone-width: the newsletter frame was sized by its content", 30000, () =>
      page.evaluate(() => {
        const f = document.querySelector("iframe.message-html");
        // The REPORTED height (the inline style), not the box: a newsletter
        // scaled to fit a phone (#801) is drawn at a fraction of it.
        return !!f && parseFloat(f.style.height) > 400;
      }),
    );
  };
  const VIEWS = [
    { name: "desk", hash: "#/inbox/review", ready: (p) => waitForDesk(p) },
    {
      name: "all-mail",
      hash: "#/all",
      ready: (p) => waitForSelectorD(p, "ul.messages > li", { timeout: 30000 }),
    },
    {
      name: "search-with-query",
      hash: "#/search/Grand",
      room: true,
      ready: (p) =>
        until("phone-width: the query is in the box", 30000, () =>
          p.evaluate(() => (document.querySelector(".search-input") || {}).value === "Grand"),
        ),
    },
    { name: "read-newsletter", hash: "#/all?m=" + FIX.newsletter.id, ready: waitNewsletter, reading: true, newsletter: true },
    { name: "read-plain", hash: "#/all?m=" + plain.id, ready: (p) => waitPane(p, plain.subject), reading: true },
    {
      name: "search-read-newsletter",
      hash: "#/search/Grand?m=" + FIX.newsletter.id,
      ready: waitNewsletter,
      reading: true,
      newsletter: true,
    },
    {
      name: "read-plain-more-open",
      hash: "#/all?m=" + plain.id,
      ready: async (p) => {
        await waitPane(p, plain.subject);
        await p.click(".thread-toolbar .pane-more > button");
        await waitForSelectorD(p, '.pane-menu[role="menu"]', { timeout: 10000 });
      },
      menu: true,
    },
    {
      name: "compose",
      hash: "#/inbox/review?compose=new",
      ready: (p) => waitForSelectorD(p, 'section.compose[role="dialog"]', { timeout: 30000 }),
    },
    {
      name: "settings",
      hash: "#/settings",
      ready: (p) => waitForSelectorD(p, '.settings-page[role="dialog"]', { timeout: 30000 }),
    },
    // Settings › Keys with the two keys keysSurfaces made, its revoke dialog,
    // and the desk carrying the key-expiry banner (#796, #795).
    {
      name: "settings-keys",
      hash: "#/settings/keys",
      ready: (p) => waitForSelectorD(p, ".keys-page .own-key", { timeout: 30000 }),
    },
    {
      name: "settings-keys-revoke",
      hash: "#/settings/keys",
      ready: (p) => openRevokeDialog(p),
    },
    // v1.93 C2: both History sections open with a key's history listed, the
    // replace dialog on a date, and generate on a date (#815, #817, #813).
    {
      name: "settings-keys-history",
      hash: "#/settings/keys",
      ready: (p) => openKeyHistories(p),
    },
    {
      name: "settings-keys-replace",
      hash: "#/settings/keys",
      ready: (p) => openReplaceDialog(p, true),
    },
    {
      name: "settings-keys-generate-date",
      hash: "#/settings/keys",
      ready: (p) => openGenerateDateDialog(p),
    },
    // The contacts page (#816): the list, one contact, and the merge dialog
    // with its preview open.
    {
      name: "contacts-list",
      hash: "#/contacts",
      ready: (p) => openContactsList(p),
    },
    {
      name: "contacts-blocked",
      hash: "#/contacts?filter=blocked",
      ready: (p) => waitForSelectorD(p, ".contacts-row", { timeout: 30000 }),
    },
    {
      name: "contact-page",
      hash: "#/contacts/" + contactIds.ada,
      ready: (p) => openContactPage(p, contactIds.ada),
    },
    {
      name: "contact-merge",
      hash: "#/contacts/" + contactIds.ada,
      ready: async (p) => {
        await openMergeDialog(p, contactIds.ada);
        await chooseTheatreInMerge(p);
        await p.keyboard.press("Enter");
        await waitForSelectorD(p, ".contact-merge-block", { timeout: 10000 });
      },
    },
    // Settings › API keys with the two waiting requests apiKeysSurfaces left,
    // and its approve dialog (#49).
    {
      name: "settings-api-keys",
      hash: "#/settings/api-keys",
      ready: (p) => openApiKeysRow(p),
    },
    {
      name: "settings-api-keys-approve",
      hash: "#/settings/api-keys",
      ready: (p) => openApproveDialog(p),
    },
    // Mail-provider sign-in (#52), with oauthSurfaces' in-page fixture: the
    // accounts list carrying the reconnect banner and the Connect buttons, the
    // interstitial, and the account waiting to be reconnected.
    { name: "settings-accounts-oauth", hash: "#/settings/accounts", oauthFixture: true, ready: (p) => openOAuthAccounts(p) },
    { name: "settings-oauth-connect-dialog", hash: "#/settings/accounts", oauthFixture: true, ready: (p) => openOAuthDialog(p) },
    {
      name: "settings-account-reconnect",
      hash: "#/settings/accounts/" + OAUTH_FIX.reauthId,
      oauthFixture: true,
      ready: (p) => openOAuthAccount(p, OAUTH_FIX.reauthId),
    },
    {
      name: "desk-key-banner",
      hash: "#/inbox/review",
      ready: async (p) => {
        await p.evaluate(() => localStorage.removeItem("mailroom.keyWarnings.dismissed"));
        await load(p, "#/inbox/review");
        await waitForDesk(p);
        await waitForSelectorD(p, ".key-expiry-banner", { timeout: 30000 });
      },
    },
  ];

  const verdict = (size, name, m) => {
    const bad = [];
    if (m.iw !== size.width) bad.push("innerWidth=" + m.iw + " (the layout viewport was widened to fit overflow)");
    if (m.scrollWidth > m.iw) bad.push("document scrollWidth=" + m.scrollWidth + " > innerWidth=" + m.iw);
    return bad.concat(m.offenders);
  };

  let checks = 0;
  let intentionalScrollControlRan = false;
  const failures = [];
  try {
    for (const size of PHONE_SIZES) {
      await page.setViewport({ ...size, deviceScaleFactor: 3, isMobile: true, hasTouch: true });

      // ---- the probe must SEE the two shapes of #798 before it is trusted ----
      const controls = [
        {
          name: "topbar-640",
          hash: "#/inbox/review",
          ready: (p) => waitForDesk(p),
          // CSSOM, not a <style> tag, so the app's CSP cannot quietly refuse it.
          fault: () => document.querySelector(".topbar").style.setProperty("min-width", "640px"),
          want: "topbar",
        },
        {
          name: "pane-row-500",
          hash: "#/all?m=" + plain.id,
          ready: (p) => waitPane(p, plain.subject),
          fault: () => {
            document.querySelector(".thread-pane").style.setProperty("overflow-x", "auto");
            document.querySelector(".thread-pane .thread-toolbar").style.setProperty("min-width", "500px");
          },
          want: "thread-pane",
        },
      ];
      for (const c of controls) {
        tally.controlsRun += 1;
        await load(page, c.hash);
        await c.ready(page);
        await page.evaluate(c.fault);
        await sleep(300);
        const bad = verdict(size, c.name, await page.evaluate(phoneWidthProbe, size.width));
        if (!bad.some((b) => b.indexOf(c.want) >= 0)) {
          throw new Fail(
            10,
            "CONTROLPASSED phone-width/" + c.name + " at " + size.width + "px: the probe did not name `" + c.want +
              "` with the fault injected (" + JSON.stringify(bad) + ") — a clean verdict from it would mean nothing",
          );
        }
        tally.controlsTripped += 1;
      }

      // ---- and the two checks added for #801 and #802 must be able to go red ----
      {
        tally.controlsRun += 1;
        await load(page, "#/all?m=" + FIX.newsletter.id);
        await waitNewsletter(page);
        await sleep(300);
        // CSSOM, so the CSP cannot refuse it: the frame back at the phone's width, unscaled.
        await page.evaluate(() => {
          const f = document.querySelector("iframe.message-html");
          f.style.removeProperty("transform");
          f.style.removeProperty("width");
        });
        await sleep(400);
        const bad = await newsletterFrameFit(page, size.width);
        if (!bad.some((b) => b.indexOf("FRAME-SCROLLS") >= 0)) {
          throw new Fail(
            10,
            "CONTROLPASSED phone-width/newsletter-unscaled at " + size.width + "px: the frame check did not name FRAME-SCROLLS with " +
              "the scale removed (" + JSON.stringify(bad) + ") — a clean verdict from it would mean nothing",
          );
        }
        tally.controlsTripped += 1;
      }
      {
        tally.controlsRun += 1;
        await load(page, "#/search/Grand");
        await until("phone-width: the query is in the box", 30000, () =>
          page.evaluate(() => (document.querySelector(".search-input") || {}).value === "Grand"),
        );
        await sleep(300);
        // The options and save buttons back in the box, wide as the crowded box was (#802).
        await page.evaluate(() => {
          // Unfocused, so the box is in its resting form whatever the page did on load.
          if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
          for (const b of document.querySelectorAll(".search-aux")) {
            for (const [k, v] of [["position", "static"], ["clip-path", "none"], ["width", "56px"], ["height", "auto"], ["min-width", "56px"], ["overflow", "visible"]]) {
              b.style.setProperty(k, v, "important");
            }
          }
        });
        await sleep(300);
        const room = await page.evaluate(searchRoom);
        if (room.chars >= SEARCH_MIN_CHARS) {
          throw new Fail(
            10,
            "CONTROLPASSED phone-width/search-crowded at " + size.width + "px: " + room.chars + " characters still fit with the box's " +
              "buttons put back — the room check would not notice them",
          );
        }
        tally.controlsTripped += 1;
      }

      for (const v of VIEWS) {
        // The API-keys setup snippets scroll inside their own box by design;
        // since #896 they carry data-scroll-x="intentional" and the probe exempts
        // them, so the views run at every phone width.
        if (v.minWidth && size.width < v.minWidth) continue;
        await setOAuthFixture(page, !!v.oauthFixture);
        await load(page, v.hash);
        await v.ready(page);
        await sleep(400);
        // #896 control, once at the first phone size the view runs at (320 px,
        // where the snippet does scroll): an intentional scroller that loses its
        // keyboard focus must STOP being exempted, or the exemption hides real
        // offenders. tabindex is removed and restored around the probe run.
        if (v.name === "settings-api-keys" && !intentionalScrollControlRan) {
          intentionalScrollControlRan = true;
          tally.controlsRun += 1;
          await page.evaluate(() => {
            document.querySelector(".setup-code").removeAttribute("tabindex");
          });
          await sleep(300);
          const cbad = verdict(size, "intentional-scroll-control", await page.evaluate(phoneWidthProbe, size.width));
          if (!cbad.some((b) => b.indexOf("SCROLLS-SIDEWAYS") >= 0 && b.indexOf("setup-code") >= 0)) {
            throw new Fail(
              10,
              "CONTROLPASSED phone-width/intentional-scroll at " + size.width + "px: the intentional-scroll control did not trip — " +
                "the probe did not name the .setup-code as SCROLLS-SIDEWAYS with its tabindex removed (" + JSON.stringify(cbad) +
                ") — a clean verdict from the exemption would mean nothing",
            );
          }
          await page.evaluate(() => {
            document.querySelector(".setup-code").setAttribute("tabindex", "0");
          });
          tally.controlsTripped += 1;
        }
        const bad = verdict(size, v.name, await page.evaluate(phoneWidthProbe, size.width));
        if (v.menu) {
          const menu = await page.evaluate(() => {
            const r = document.querySelector('.pane-menu[role="menu"]').getBoundingClientRect();
            return { top: r.top, bottom: r.bottom, left: r.left, right: r.right, ih: window.innerHeight };
          });
          if (menu.top < 0 || menu.bottom > menu.ih + 1) bad.push("MENU-OFFSCREEN More's menu " + JSON.stringify(menu));
        }
        if (v.newsletter) {
          // #801: the wide newsletter is SCALED to the screen, not scrolled in its box.
          for (const b of await newsletterFrameFit(page, size.width)) bad.push(b);
        }
        if (v.room) {
          // #802: a typed query has room. 12 characters is the issue's floor.
          const room = await page.evaluate(searchRoom);
          if (room.chars < SEARCH_MIN_CHARS) {
            bad.push("SEARCH-CROWDED only " + room.chars + " characters fit the search input (" + room.inner + "px), want " + SEARCH_MIN_CHARS);
          }
        }
        if (v.reading) {
          // THE ADDRESS-BAR HALF: the document scrolls, and the docked rows stay
          // on screen after it has.
          const s = await page.evaluate(async () => {
            const pane = getComputedStyle(document.querySelector("aside.thread-pane")).overflowY;
            const before = document.documentElement.scrollHeight > window.innerHeight;
            window.scrollTo(0, 400);
            await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
            const row = document.querySelector(".thread-toolbar").getBoundingClientRect();
            const bar = document.querySelector(".bottom-bar").getBoundingClientRect();
            const out = {
              pane,
              docTaller: before,
              scrollY: Math.round(window.scrollY),
              rowBottom: Math.round(row.bottom),
              barTop: Math.round(bar.top),
              barBottom: Math.round(bar.bottom),
              ih: window.innerHeight,
            };
            window.scrollTo(0, 0);
            return out;
          });
          if (s.pane === "auto" || s.pane === "scroll") bad.push("PANE-SCROLLS the reading pane is a scroller (overflow-y:" + s.pane + "), so the document never scrolls");
          if (!s.docTaller || s.scrollY === 0) bad.push("NODOCSCROLL " + JSON.stringify(s));
          if (s.barBottom > s.ih + 1 || s.barTop >= s.ih) bad.push("BAR-OFFSCREEN " + JSON.stringify(s));
          if (s.rowBottom > s.barTop + 1) bad.push("ROW-UNDER-BAR " + JSON.stringify(s));
          // THE ROW'S WORDS FIT (#375, #798). The row's CSS lets a button
          // truncate rather than overflow, so overflow alone cannot see a
          // sixth button squeezed in. Measured for #798: with Reply all back
          // in the phone's row, nothing overflowed, and every label was cut to
          // an ellipsis instead. At 375 and 390 px, four buttons and More
          // must show their whole words.
          //
          // The text's own laid-out width (a Range over it) against the
          // button's content box, NOT the button's scrollWidth. Measured in this
          // lane's Chromium 124, a BUTTON's scrollWidth does not report an
          // ellipsised label ("Reply all" cut to 45 px of 49: scrollWidth 61 =
          // clientWidth 61), so that test could never fail.
          const cut = await page.evaluate(() =>
            [...document.querySelectorAll(".thread-toolbar .button")]
              .filter((b) => b.offsetParent !== null)
              .map((b) => {
                const range = document.createRange();
                range.selectNodeContents(b);
                const cs = getComputedStyle(b);
                const inner = b.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
                return { label: (b.textContent || "").trim(), text: range.getBoundingClientRect().width, inner };
              })
              .filter((m) => m.text > m.inner + 0.5)
              .map((m) => JSON.stringify(m.label) + " text=" + m.text.toFixed(1) + "px in a " + m.inner.toFixed(1) + "px box"),
          );
          // 360 px is "the narrowest tight fit" (desk.css): at 320 the row's rule is to
          // truncate rather than overflow, so words cut there are the design, and the
          // overflow checks above still hold.
          if (size.width >= 360) for (const c of cut) bad.push("TRUNCATED the phone's action row cuts " + c);
        }
        // Every view and both sizes are measured before failing, so one run
        // prints the whole list rather than the first offender only.
        if (bad.length) {
          failures.push(
            "PHONEWIDTH " + v.name + " at " + size.width + "x" + size.height + ": " + bad.length + " offender(s)\n  " +
              bad.join("\n  "),
          );
        }
        checks += 1;
      }
    }

    // ---- THE DESKTOP PASS (#803): 1440x900, mouse, one pixel per CSS pixel ----
    // The reading pane sits beside the list here, about 493 px wide in the
    // default split, and its action row needs about 574 px for all seven
    // buttons. `.thread-pane` scrolls vertically, which makes x `auto`, so the
    // whole pane scrolled sideways under the row. The row now measures itself
    // and moves Reply all and Mute into More when it does not fit.
    await page.setViewport({ ...DESKTOP_SIZE, deviceScaleFactor: 1, isMobile: false, hasTouch: false });
    const desktopRow = async () =>
      page.evaluate(() => {
        const pane = document.querySelector("aside.thread-pane");
        const row = document.querySelector(".thread-toolbar");
        return {
          paneScroll: pane.scrollWidth,
          paneClient: pane.clientWidth,
          rowScroll: row.scrollWidth,
          rowClient: row.clientWidth,
          buttons: [...row.querySelectorAll(":scope > .button, :scope > .pane-more > .button")].map((b) => b.textContent.trim()),
        };
      });
    {
      tally.controlsRun += 1;
      await load(page, "#/all?m=" + plain.id);
      await waitPane(page, plain.subject);
      await sleep(400);
      // The same fault as the phone's pane-row control: a row that cannot shrink.
      await page.evaluate(() => {
        document.querySelector(".thread-pane").style.setProperty("overflow-x", "auto");
        document.querySelector(".thread-pane .thread-toolbar").style.setProperty("min-width", "900px");
      });
      await sleep(300);
      const bad = verdict(DESKTOP_SIZE, "desktop-pane-row-900", await page.evaluate(phoneWidthProbe, DESKTOP_SIZE.width));
      if (!bad.some((b) => b.indexOf("thread-pane") >= 0)) {
        throw new Fail(
          10,
          "CONTROLPASSED phone-width/desktop-pane-row-900: the probe did not name `thread-pane` with the fault injected (" +
            JSON.stringify(bad) + ") — a clean verdict from it would mean nothing",
        );
      }
      tally.controlsTripped += 1;
    }
    {
      // #896's negative control: the very fault that scrolled the sidebar (a
      // full-width child with a side margin) injected into the rail must make
      // the probe name nav.sidebar, or a clean verdict from it means nothing.
      tally.controlsRun += 1;
      await load(page, "#/all?m=" + plain.id);
      await waitPane(page, plain.subject);
      await sleep(400);
      const before = verdict(DESKTOP_SIZE, "desktop-sidebar-clean", await page.evaluate(phoneWidthProbe, DESKTOP_SIZE.width));
      await page.evaluate(() => {
        const b = document.createElement("button");
        b.id = "a11y-control-wide";
        b.textContent = "control";
        b.style.cssText = "display:block;width:100%;margin:0 8px;";
        document.querySelector("nav.sidebar").appendChild(b);
      });
      await sleep(300);
      const bad = verdict(DESKTOP_SIZE, "desktop-sidebar-16px", await page.evaluate(phoneWidthProbe, DESKTOP_SIZE.width));
      await page.evaluate(() => document.getElementById("a11y-control-wide")?.remove());
      if (before.some((x) => x.indexOf("nav.sidebar") >= 0) || !bad.some((x) => x.indexOf("nav.sidebar") >= 0)) {
        throw new Fail(
          10,
          "CONTROLPASSED phone-width/desktop-sidebar-16px: the probe must be clean on nav.sidebar before the fault and name it after (before=" +
            JSON.stringify(before) + ", after=" + JSON.stringify(bad) + ")",
        );
      }
      tally.controlsTripped += 1;
    }
    for (const v of VIEWS.filter((x) => x.reading && !x.menu)) {
      await load(page, v.hash);
      await v.ready(page);
      await sleep(500);
      const all = verdict(DESKTOP_SIZE, v.name, await page.evaluate(phoneWidthProbe, DESKTOP_SIZE.width));
      // The sidebar is held to the same standard as the pane (#896). It used to be
      // a NOTE: `.nav-disclosure` was 100% wide inside an 8px margin on each side,
      // so nav.sidebar scrolled sideways by 16 px at 1440 px.
      const bad = all;
      const r = await desktopRow();
      if (r.rowScroll > r.rowClient + 1) bad.push("ROW-OVERFLOWS the action row needs " + r.rowScroll + "px of " + r.rowClient + "px " + JSON.stringify(r.buttons));
      if (r.paneScroll > r.paneClient + 1) bad.push("PANE-SCROLLS-SIDEWAYS the reading pane is " + r.paneScroll + "px wide in " + r.paneClient + "px");
      // Reply all still on a row that overflows is the squeeze not happening; the
      // overflow check above names it, and this names the cause (measured on the
      // unfixed code: the row needed 574 px of 493, with Reply all on it).
      if (r.rowScroll > r.rowClient + 1 && r.buttons.includes("Reply all")) bad.push("ROW-NOT-SQUEEZED Reply all is on a " + r.rowClient + "px row that needs " + r.rowScroll + "px " + JSON.stringify(r.buttons));
      if (bad.length) {
        failures.push(
          "PHONEWIDTH " + v.name + " at " + DESKTOP_SIZE.width + "x" + DESKTOP_SIZE.height + ": " + bad.length + " offender(s)\n  " + bad.join("\n  "),
        );
      }
      checks += 1;
    }
  } finally {
    await setOAuthFixture(page, false);
    await page.setViewport({ width: 1680, height: 1100 });
  }
  if (failures.length) {
    throw new Fail(28, failures.join("\n") + "\nPHONEWIDTH " + failures.length + " of " + checks + " view checks failed");
  }
  tally.phoneWidth = 1;
  console.log(
    "phone-width       " + checks + " views fit 320, 375 and 390px and the reading pane fits 1440px (no page overflow, nothing off screen, " +
      "nothing scrolls sideways except the intentional setup-code scroller, action-row labels whole, a wide newsletter scaled to the " +
      "screen, 12 characters of room in the search box; reading scrolls the document); 4 probe controls per phone size, 2 on the " +
      "desktop, and the intentional-scroll exemption control tripped",
  );
}

/**
 * EVERY POP-UP MENU STAYS ON A PHONE'S SCREEN (#880). Exit 32, PHONEMENUS.
 *
 * ⚠ AXE CANNOT SEE THIS CLASS OF DEFECT, and neither can any other step in this
 * file. A menu hung 200 px below the bottom of the screen has a perfect role,
 * name, states and contrast; it is simply not there. #880 was exactly that: with
 * several messages selected, the desk's Re-file menu opened below a toolbar that
 * a phone docks to the BOTTOM of the screen, so it was entirely off screen —
 * while this lane stayed green, because phone-width (exit 28) measures what is
 * WIDER than the phone and opened one menu (More), in portrait, on a short list.
 *
 * WHAT IT DOES. For each pop-up the app anchors to a control — the Re-file menu
 * with several rows selected, the classic list's Move to area, the reading pane's
 * More, the sender card's "Move all from … to…", the search box's suggestions and
 * its options form, the compose recipient suggestions — it sets the viewport to
 * 320x568 and 390x844 AND each rotated (568x320, 844x390), loads the view cold,
 * opens the menu, and asserts:
 *
 *   * its box lies inside the visible screen (1px of slack);
 *   * no ancestor that clips (overflow other than visible) cuts it;
 *   * when it is taller than its box it scrolls INSIDE itself (overflow-y auto or
 *     scroll), and after scrolling to the end its LAST item is on screen and is
 *     the topmost element at its centre — not under the bottom bar or toolbar;
 *   * the same for its first item before scrolling.
 *
 * Extra areas are created first (and destroyed after) so the Re-file and Move
 * menus are LONGER THAN A 320 px TALL SCREEN: a short menu fitting says nothing
 * about a long one. The table of every menu x size is printed on every run, with
 * the item count, so "all fine" cannot be a menu that never opened (#131).
 *
 * IN-RUN NEGATIVE CONTROLS: the Re-file menu is opened, then (1) pushed 2000 px
 * off the screen with an inline `translate`, and (2) given `overflow: hidden`, so
 * its last item cannot be reached. The probe must name each one or the run fails
 * CONTROLPASSED.
 */
const MENU_SIZES = [
  { width: 320, height: 568 },
  { width: 568, height: 320 },
  { width: 390, height: 844 },
  { width: 844, height: 390 },
];
const EXTRA_AREAS = 14;

/** The measurement, run in the page. Returns a list of problems (empty = fine)
 * plus the numbers, for the table. */
function menuProbe(sel) {
  const el = document.querySelector(sel);
  if (!el) return { missing: true, problems: ["no element matches " + sel] };
  const iw = document.documentElement.clientWidth;
  const ih = window.innerHeight;
  const problems = [];
  const px = (n) => Math.round(n);
  const inView = (r, what) => {
    if (r.top < -1) problems.push(what + " is " + px(-r.top) + "px above the top of the screen");
    if (r.bottom > ih + 1) problems.push(what + " is " + px(r.bottom - ih) + "px below the bottom of the screen");
    if (r.left < -1) problems.push(what + " is " + px(-r.left) + "px past the left edge");
    if (r.right > iw + 1) problems.push(what + " is " + px(r.right - iw) + "px past the right edge");
  };
  const r0 = el.getBoundingClientRect();
  inView(r0, "the menu");
  // A clipping ancestor cuts a menu even when the SCREEN has room for it.
  for (let a = el.offsetParent; a && a !== document.body && a !== document.documentElement; a = a.parentElement) {
    const c = getComputedStyle(a);
    if (c.overflowX === "visible" && c.overflowY === "visible") {
      if (c.position === "fixed") break;
      continue;
    }
    const r = a.getBoundingClientRect();
    const L = r.left + a.clientLeft;
    const T = r.top + a.clientTop;
    const name = (typeof a.className === "string" ? a.className : "").split(" ")[0] || a.tagName.toLowerCase();
    if (c.overflowY !== "visible" && (r0.top < T - 1 || r0.bottom > T + a.clientHeight + 1)) {
      problems.push("the menu is cut off by ." + name + " (overflow-y:" + c.overflowY + ") by " +
        px(Math.max(T - r0.top, r0.bottom - (T + a.clientHeight))) + "px vertically");
    }
    if (c.overflowX !== "visible" && (r0.left < L - 1 || r0.right > L + a.clientWidth + 1)) {
      problems.push("the menu is cut off by ." + name + " (overflow-x:" + c.overflowX + ") sideways");
    }
    if (c.position === "fixed") break;
  }
  const kids = [...el.querySelectorAll('[role="menuitem"], [role="option"], button, input, select, a[href]')].filter(
    (k) => k.getClientRects().length > 0,
  );
  const first = kids[0];
  const last = kids[kids.length - 1];
  const topmost = (k, what) => {
    const r = k.getBoundingClientRect();
    inView(r, what);
    const x = Math.min(Math.max(r.left + r.width / 2, 0), iw - 1);
    const y = Math.min(Math.max(r.top + r.height / 2, 0), ih - 1);
    const hit = document.elementFromPoint(x, y);
    if (!hit || !(el.contains(hit) || hit === el)) {
      problems.push(what + " is covered by " + (hit ? hit.tagName.toLowerCase() + "." + String(hit.className).split(" ")[0] : "nothing"));
    }
  };
  const tall = el.scrollHeight > el.clientHeight + 1;
  const oy = getComputedStyle(el).overflowY;
  if (tall && oy !== "auto" && oy !== "scroll") {
    problems.push("the menu holds " + px(el.scrollHeight) + "px of items in " + px(el.clientHeight) + "px and does not scroll (overflow-y:" + oy + ")");
  }
  if (first) topmost(first, "its first item");
  if (tall && (oy === "auto" || oy === "scroll")) el.scrollTop = el.scrollHeight;
  if (last) topmost(last, "its last item (" + (tall ? "after scrolling to the end" : "no scrolling needed") + ")");
  el.scrollTop = 0;
  return { problems, items: kids.length, box: [px(r0.left), px(r0.top), px(r0.right), px(r0.bottom)], scrolls: tall, iw, ih };
}

async function phoneMenusJmap(page, calls) {
  return page.evaluate(async (calls) => {
    const sess = await (await fetch("/jmap/session", { credentials: "include" })).json();
    const accountId = Object.values(sess.primaryAccounts)[0];
    const using = ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", "urn:mailroom:desk"];
    const methodCalls = calls.map(([name, args, id]) => [name, { accountId, ...args }, id]);
    const res = await fetch(sess.apiUrl, {
      method: "POST",
      credentials: "include",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ using, methodCalls }),
    });
    return (await res.json()).methodResponses;
  }, calls);
}

async function phoneMenusSurfaces(page) {
  const plain = FIX.messages.find((m) => m.subject === "Lunch on Thursday?") || FIX.messages[0];

  // ---- seed: areas enough to overflow a 320 px tall screen -----------------
  await load(page, "#/inbox/review");
  const create = {};
  for (let i = 1; i <= EXTRA_AREAS; i += 1) create["pm" + i] = { name: "Phone menu area " + String(i).padStart(2, "0") };
  const made = await phoneMenusJmap(page, [["Area/set", { create }, "mk"]]);
  const created = (made[0] && made[0][1] && made[0][1].created) || {};
  const createdIds = Object.values(created).map((a) => a.id);
  if (createdIds.length !== EXTRA_AREAS) {
    throw new Fail(32, "PHONEMENUS seed: wanted " + EXTRA_AREAS + " extra areas, the server made " + createdIds.length + ": " + JSON.stringify(made).slice(0, 300));
  }

  const openRefileDesk = async (p) => {
    await waitForDesk(p);
    await until("phone-menus: two desk rows can be selected", 30000, () =>
      p.evaluate(() => document.querySelectorAll("button.desk-sender").length >= 2),
    );
    await p.evaluate(() => {
      const b = document.querySelectorAll("button.desk-sender");
      b[0].click();
      b[1].click();
    });
    await until("phone-menus: the selection toolbar shows 2 selected", 15000, () =>
      p.evaluate(() => /2 selected|^2/.test((document.querySelector(".desk-toolbar-count") || {}).textContent || "")),
    );
    await p.evaluate(() => {
      [...document.querySelectorAll(".desk-toolbar button")].find((b) => /^Re-file/.test((b.textContent || "").trim())).click();
    });
    await waitForSelectorD(p, '.desk-toolbar [role="menu"]', { timeout: 10000 });
  };

  const MENUS = [
    {
      name: "desk-refile-2-selected",
      hash: "#/inbox/review",
      sel: '.desk-toolbar [role="menu"]',
      minItems: EXTRA_AREAS,
      open: openRefileDesk,
    },
    {
      name: "list-move-to-area",
      hash: "#/all",
      sel: '.list-toolbar [role="menu"]',
      minItems: EXTRA_AREAS,
      open: async (p) => {
        await waitForSelectorD(p, "ul.messages > li input[type=checkbox]", { timeout: 30000 });
        await p.click("ul.messages > li input[type=checkbox]");
        await until("phone-menus: the list's Move to area is offered", 15000, () =>
          p.evaluate(() => [...document.querySelectorAll(".list-toolbar button")].some((n) => /^Move to area/.test((n.textContent || "").trim()))),
        );
        await p.evaluate(() => {
          [...document.querySelectorAll(".list-toolbar button")].find((n) => /^Move to area/.test((n.textContent || "").trim())).click();
        });
        await waitForSelectorD(p, '.list-toolbar [role="menu"]', { timeout: 10000 });
      },
    },
    {
      name: "reading-pane-more",
      hash: "#/all?m=" + plain.id,
      sel: '.pane-menu[role="menu"]',
      minItems: 3,
      open: async (p) => {
        await waitForSelectorD(p, ".thread-toolbar .pane-more > button", { timeout: 30000 });
        await p.click(".thread-toolbar .pane-more > button");
        await waitForSelectorD(p, '.pane-menu[role="menu"]', { timeout: 10000 });
      },
    },
    {
      name: "sender-card-move-all",
      hash: "#/all?m=" + FIX.attachment.id,
      sel: '.sender-popover [role="menu"]',
      minItems: EXTRA_AREAS,
      open: async (p) => {
        await waitForSelectorD(p, "button.message-from-button", { timeout: 30000 });
        await p.click("button.message-from-button");
        await waitForSelectorD(p, '.sender-popover[role="dialog"]', { timeout: 30000 });
        // The card's count is a `from:` search; until the index has caught up it
        // closes the menu again, so click until the menu stays (as moveToAreaSurfaces does).
        await until(
          "phone-menus: the sender card's Move all menu is open",
          60000,
          async () => {
            const open = await p.evaluate(() => !!document.querySelector('.sender-popover [role="menu"]'));
            if (open) return true;
            await p.evaluate(() => {
              const b = [...document.querySelectorAll('.sender-popover[role="dialog"] button')].find(
                (n) => /^Move all from .* to…/.test((n.textContent || "").trim()) && !n.disabled,
              );
              if (b) b.click();
            });
            return false;
          },
          32,
        );
      },
    },
    {
      name: "search-suggestions",
      hash: "#/all",
      sel: "ul.search-suggestions",
      minItems: 1,
      open: async (p) => {
        await waitForSelectorD(p, "ul.messages > li", { timeout: 30000 });
        await p.click("input.search-input");
        await p.keyboard.sendCharacter("from:");
        await waitForSelectorD(p, "ul.search-suggestions", { timeout: 10000 });
      },
    },
    {
      name: "search-options-form",
      hash: "#/all",
      sel: "#search-options",
      minItems: 11,
      open: async (p) => {
        await waitForSelectorD(p, 'button[aria-controls="search-options"]', { timeout: 30000 });
        // Clipped until the box has focus below 400 px (#802), so focus it first.
        await p.focus("input.search-input");
        await p.click('button[aria-controls="search-options"]');
        await waitForSelectorD(p, "#search-options", { timeout: 10000 });
      },
    },
    {
      name: "compose-recipient-suggestions",
      hash: "#/inbox/review?compose=new",
      sel: "ul.recipient-suggestions",
      minItems: 1,
      open: async (p) => {
        await waitForSelectorD(p, 'section.compose[role="dialog"] input.compose-chip-text', { timeout: 30000 });
        await p.click('section.compose[role="dialog"] input.compose-chip-text');
        await p.keyboard.sendCharacter("ada");
        await waitForSelectorD(p, "ul.recipient-suggestions", { timeout: 15000 });
      },
    },
  ];

  const setSize = (size) => page.setViewport({ ...size, deviceScaleFactor: 3, isMobile: true, hasTouch: true });
  const rows = [];
  const failures = [];
  try {
    // ---- the probe must SEE the two faults before it is trusted --------------
    await setSize(MENU_SIZES[0]);
    const controls = [
      {
        name: "refile-pushed-off-screen",
        fault: () => document.querySelector('.desk-toolbar [role="menu"]').style.setProperty("translate", "0 2000px"),
        want: "below the bottom",
      },
      {
        name: "refile-cannot-scroll",
        fault: () => {
          const m = document.querySelector('.desk-toolbar [role="menu"]');
          m.style.setProperty("max-height", "120px");
          m.style.setProperty("overflow-y", "hidden");
        },
        want: "does not scroll",
      },
    ];
    for (const c of controls) {
      tally.controlsRun += 1;
      await load(page, "#/inbox/review");
      await openRefileDesk(page);
      await page.evaluate(c.fault);
      await sleep(200);
      const got = await page.evaluate(menuProbe, '.desk-toolbar [role="menu"]');
      if (!got.problems.some((s) => s.indexOf(c.want) >= 0)) {
        throw new Fail(
          10,
          "CONTROLPASSED phone-menus/" + c.name + ": the probe did not report `" + c.want + "` with the fault injected (" +
            JSON.stringify(got.problems) + ") - a clean verdict from it would mean nothing",
        );
      }
      tally.controlsTripped += 1;
    }

    // ---- every menu, every size ---------------------------------------------
    for (const size of MENU_SIZES) {
      await setSize(size);
      for (const m of MENUS) {
        await load(page, m.hash);
        await m.open(page);
        await sleep(300);
        const got = await page.evaluate(menuProbe, m.sel);
        const problems = got.problems.slice();
        if (!got.missing && got.items < m.minItems) {
          problems.push("only " + got.items + " items, wanted at least " + m.minItems + " (the seed did not reach this menu, so a clean verdict would prove little)");
        }
        rows.push({ menu: m.name, size: size.width + "x" + size.height, items: got.items, box: got.box, problems });
        if (problems.length) {
          failures.push("PHONEMENUS " + m.name + " at " + size.width + "x" + size.height + ":\n  " + problems.join("\n  "));
          if (SHOTS) {
            await page.screenshot({ path: "/tmp/shots/phonemenus-" + m.name + "-" + size.width + "x" + size.height + ".png" });
          }
        }
      }
    }
  } finally {
    await page.setViewport({ width: 1680, height: 1100 });
    if (createdIds.length) {
      await load(page, "#/inbox/review");
      await phoneMenusJmap(page, [["Area/set", { destroy: createdIds }, "rm"]]);
    }
  }
  // The table, every run: the number of menus examined is a denominator.
  console.log("phone-menus       menu x size (items, box left,top,right,bottom):");
  for (const r of rows) {
    console.log("  " + (r.problems.length ? "CLIPPED " : "ok      ") + r.menu.padEnd(30) + r.size.padEnd(9) + " items=" + r.items + " box=" + JSON.stringify(r.box));
  }
  if (failures.length) {
    throw new Fail(32, failures.join("\n") + "\nPHONEMENUS " + failures.length + " of " + rows.length + " menu checks failed");
  }
  tally.phoneMenus = 1;
  console.log(
    "phone-menus       " + rows.length + " menu checks (" + MENUS.length + " menus x " + MENU_SIZES.length +
      " sizes, portrait and landscape) on screen, scrollable to the last item, not covered; 2 probe controls tripped",
  );
}

async function main() {
  console.log(
    "fixture accountId=" +
      FIX.accountId +
      " messages=" +
      FIX.messages.length +
      " areas=" +
      FIX.areas.length +
      " crossed=" +
      FIX.crossed.length,
  );
  for (const a of FIX.summary) {
    console.log(
      "fixture area " +
        (a.areaName || "(needs sorting)") +
        " surface=" +
        a.surface +
        " drawer=" +
        a.drawer +
        " unscored=" +
        a.unscored,
    );
  }

  const browser = await puppeteer.launch({
    args: ["--no-sandbox", "--disable-setuid-sandbox", "--disable-dev-shm-usage"],
  });
  try {
    const page = await browser.newPage();
    // Wide enough to be the DESKTOP layout (PHONE_QUERY is max-width 759px) and
    // tall enough that the desk's virtualised list renders the line without
    // scrolling in most themes. Fixed, because a viewport that varied would make
    // the contrast and reflow results vary with it.
    await page.setViewport({ width: 1680, height: 1100 });
    page.on("pageerror", (e) => {
      console.log("pageerror " + e.message);
      recentPageIssues.push("pageerror: " + e.message);
    });
    // Console `error` messages, collected for the same reason: a client that
    // already logged why it is stuck is a different bug from a selector that is
    // merely slow, and `waitForSelectorD` folds both into the same timeout line.
    page.on("console", (msg) => {
      if (msg.type() !== "error") return;
      console.log("consoleerror " + msg.text());
      recentPageIssues.push("console.error: " + msg.text());
    });
    // Cleared on every TOP-LEVEL navigation, so a diagnostic printed after a
    // `load()`/`reload()` reports issues from the document that is actually on
    // screen rather than accumulating across every surface the run has visited.
    page.on("framenavigated", (frame) => {
      if (frame === page.mainFrame()) recentPageIssues = [];
    });
    watchPrefsWrites(page);
    // Mail-provider sign-in (#52): inert until oauthSurfaces sets its flag.
    await installOAuthFixture(page);
    if (PREFS_SET_DELAY_MS > 0) {
      // Held in the PAGE, before the request exists, so a navigation in the
      // meantime takes it down with the document — the shape a reload racing an
      // unanswered write has (#658).
      await page.evaluateOnNewDocument((ms) => {
        const orig = window.fetch;
        window.fetch = function (input, init) {
          const body = init && typeof init.body === "string" ? init.body : "";
          if (body.indexOf('"ClientPreferences/set"') < 0) return orig.call(this, input, init);
          return new Promise((r) => setTimeout(r, ms)).then(() => orig.call(this, input, init));
        };
      }, PREFS_SET_DELAY_MS);
      console.log("prefs-set delay " + PREFS_SET_DELAY_MS + "ms");
    }
    const cdp = await page.createCDPSession();
    await cdp.send("DOM.enable");
    await cdp.send("Accessibility.enable");

    await settled(page, "first load", () => page.goto(BASE + "/#/inbox/review", { waitUntil: "networkidle2", timeout: 60000 }));
    await waitForDesk(page);

    // ---- axe, every surface, every theme --------------------------------
    for (const theme of THEMES) {
      await chooseTheme(page, theme);
      for (const surface of SURFACES) {
        await surface.open(page);
        // ⚠ RE-ASSERTED PER SURFACE, because every surface now starts from a cold
        // document and a theme that failed to survive the reload would make this
        // pass lint the light palette while its lines say "dark". One attribute
        // read is cheap; a whole theme's results being about the wrong colours is
        // not detectable afterwards.
        const shown = await page.evaluate(() => document.documentElement.dataset["theme"]);
        if (shown !== theme.expect) {
          throw new Fail(
            13,
            "NOTHEME " + surface.name + " loaded with data-theme=" + shown +
              ", want " + theme.expect,
          );
        }
        // Re-injected per surface: a hash route change does not reload the
        // document, but `page.goto`/`reload` does, and `window.axe` goes with it.
        await injectAxe(page);
        await axeAA(page, surface.name, theme.name);
        await axeContrastAAA(page, surface.name, theme.name);
        if (surface.after) await surface.after(page);
        if (theme === THEMES[0]) tally.surfaces += 1;
      }
    }

    // ---- the announcement script (light) --------------------------------
    await chooseTheme(page, THEMES[0]);
    await announcementScript(page, cdp);
    await paneClosesInOneStep(page);
    await moveToAreaSurfaces(page);
    await searchBoxSurfaces(page, cdp);
    await notifySurfaces(page, cdp);
    await cleanupSurfaces(page, cdp);
    // Out of office before the invitation card and the Follow up strip, as
    // before. The reason it was first moved — a folded message's button behind
    // the settings overlay tripping label-content-name-mismatch — is fixed
    // (#759), and the folded pane is now linted on purpose by foldedSurfaces.
    await outOfOfficeSurfaces(page, cdp);
    await foldedSurfaces(page, cdp);
    await inviteSurfaces(page, cdp);
    await followUpSurfaces(page, cdp);
    // Keys before the phone-width pass, which measures the keys page and the
    // revoke dialog with the keys this step makes (#796).
    await keysSurfaces(page, cdp);
    // Contacts (#816) before the phone-width pass, which measures its views.
    const contactIds = await contactsSurfaces(page, cdp);
    // API keys (#49) before the phone-width pass, which measures the row and
    // the approve dialog with the requests this step leaves waiting; they are
    // denied right after it, so nothing later carries the banner.
    await apiKeysSurfaces(page, cdp);
    // Mail-provider sign-in (#52) before the phone-width pass, which measures
    // the same surfaces with the same in-page fixture.
    await oauthSurfaces(page, cdp);
    await phoneWidthSurfaces(page, contactIds);
    await apiKeysCleanup(page);
    // Every pop-up menu on a phone (#880), after the API-key requests are denied
    // so no banner shifts the layout it measures.
    await phoneMenusSurfaces(page);

    // ---- the negative controls ------------------------------------------
    await negativeControls(page, cdp);

    // ---- the docked split (#617), LAST: it changes the synced split --------
    await chooseTheme(page, THEMES[0]);
    await dockedSplitCheck(page);

    // ⚠ THE FINAL LINE IS NOT ALLOWED TO BE VACUOUS. Every number on it is a
    // denominator, and a CLEAN line reporting `surfaces=0 axe_runs=0` would be a
    // pass that examined nothing — which is what a mistake in the surface or theme
    // loop looks like from the outside, and what a debugging shortcut left in by
    // accident looks like too (one was: the matrix was cut to nothing while the
    // announcement half was being iterated on, and the run still printed CLEAN).
    if (tally.axeRuns === 0 || tally.surfaces === 0 || tally.announcements === 0) {
      throw new Fail(
        11,
        "NOSTEP nothing-ran: surfaces=" +
          tally.surfaces +
          " axe_runs=" +
          tally.axeRuns +
          " announcements=" +
          tally.announcements +
          " — a CLEAN line with a zero in it would be a pass that examined nothing",
      );
    }
    printContrastReport();
    console.log(
      (CONTRAST_REPORT ? "REPORT-ONLY " : "") +
        "CLEAN a11y surfaces=" +
        tally.surfaces +
        " themes=" +
        THEMES.length +
        " axe_runs=" +
        tally.axeRuns +
        " axe_violations=0 axe_passes=" +
        tally.passes +
        " axe_incomplete=" +
        tally.incomplete +
        " contrast_aaa_allowed=" +
        tally.contrastAllowed +
        " announcements=" +
        tally.announcements +
        " negative_controls=" +
        tally.controlsTripped +
        "/" +
        tally.controlsRun +
        " docked_split=" +
        (tally.dockedSplit ? "ok" : "NOT-RUN") +
        " scroll_away=" +
        (tally.scrollAway ? "ok" : "NOT-RUN") +
        " pane_close=" +
        (tally.paneClose ? "ok" : "NOT-RUN") +
        " move_menu=" +
        (tally.moveMenu ? "ok" : "NOT-RUN") +
        " move_sender=" +
        (tally.moveSender ? "ok" : "NOT-RUN") +
        " search_box=" +
        (tally.searchBox ? "ok" : "NOT-RUN") +
        " held_chip=" +
        (tally.heldChip ? "ok" : "NOT-RUN") +
        " notify=" +
        (tally.notify ? "ok" : "NOT-RUN") +
        " cleanup=" +
        (tally.cleanup ? "ok" : "NOT-RUN") +
        " invite=" +
        (tally.invite ? "ok" : "NOT-RUN") +
        " follow_up=" +
        (tally.followUp ? "ok" : "NOT-RUN") +
        " out_of_office=" +
        (tally.oof ? "ok" : "NOT-RUN") +
        " folded=" +
        (tally.folded ? "ok" : "NOT-RUN") +
        " phone_width=" +
        (tally.phoneWidth ? "ok" : "NOT-RUN") +
        " phone_menus=" +
        (tally.phoneMenus ? "ok" : "NOT-RUN") +
        " keys=" +
        (tally.keys ? "ok" : "NOT-RUN") +
        " contacts=" +
        (tally.contacts ? "ok" : "NOT-RUN") +
        " api_keys=" +
        (tally.apiKeys ? "ok" : "NOT-RUN") +
        " oauth=" +
        (tally.oauth ? "ok" : "NOT-RUN") +
        " axe_version=" +
        axeVersion +
        " NOT_CHECKED=held-attachment-panel,message-body-iframe,at-phrasing",
    );
  } finally {
    await browser.close();
  }
}

main().then(
  () => process.exit(0),
  (e) => {
    if (e instanceof Fail) {
      console.log(e.message);
      process.exit(e.code);
    }
    console.log("ERR " + (e && e.message ? e.message : String(e)));
    process.exit(2);
  },
);
