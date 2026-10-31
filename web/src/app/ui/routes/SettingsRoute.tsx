// SPDX-License-Identifier: AGPL-3.0-only

// SETTINGS AS ONE SEARCHABLE PAGE (#374 increment 2).
//
// WHERE THIS CAME FROM. Settings was a popover menu that opened four modal sheets
// with no URLs; #373 gave every one of them a route and turned the menu into an
// INDEX of nine links to nine sheets. That solved the linking problem and left the
// real one untouched: a list of nine headings tells you nothing about what your
// mail client is currently doing, so finding a setting still meant opening things
// until you recognised one — and adding a tenth sheet was still cheaper than
// deciding where a setting belonged.
//
// WHAT REPLACES IT. One page. A field at the top that matches on what a row SAYS
// and on the values it holds, and twelve rows below it, each stating its own facts
// in one line before you open anything:
//
//   Accounts · Areas and drawers · The line · In-tray · Rules ·
//   Identities and signatures · Notifications · Reading and safety · Keys ·
//   Saved searches · Appearance · Data
//
// THREE RULES RUN THROUGH ALL OF IT.
//
//   1. EVERY ROW SAYS WHO IT APPLIES TO. `login`, `account`, `area`, `web` — the
//      question a settings page almost never answers, and here there are four
//      different answers on one screen (ui/settings/Scope.tsx).
//   2. A FACT THAT IS NOT MEASURED IS OMITTED, NEVER PRINTED AS ZERO. This is why
//      the state lines can be trusted at all: `0` is a claim, and a claim about a
//      number nobody has counted is the #159/#176 failure — a line that reads
//      perfectly and is about nothing. Every builder below either has the number
//      or leaves the clause out, and each place where that decision is subtle
//      carries a comment naming the missing fact and why it is missing.
//   3. NOTHING APPEARS IN TWO ROWS. Every control from #373's nine sheets has
//      exactly one home here; LEGACY_SECTIONS in core/routes.ts records where the
//      three retired section ids went.
//
// THE PAGE IS STILL AN OVERLAY OVER THE LIST, unchanged: viewForRoute keeps the
// previous list's view underneath (core/routes.ts), the list stays mounted, and
// Escape peels back to the list route you came from (app/routing.ts). Closing
// settings costs no refetch and loses no selection.
//
// ALL TWELVE BODIES ARE HERE SINCE INCREMENT 3. Accounts, Areas and drawers, The
// line and In-tray replaced the one quiet sentence increment 2 left in their place
// (rows/AccountsRow.tsx, AreasRow.tsx, LineRow.tsx, TrayRow.tsx). Two of them are
// two screens rather than one, and both use a detail segment increment 2 had
// already taught the router to parse: `#/settings/accounts/<id>` is one account's
// six controls, `#/settings/areas/<id>` marks one card.
//
// EACH OF THOSE FOUR ROWS ALSO NAMES SOMETHING THE SERVER CANNOT ANSWER, rather
// than faking it — a per-account folder list, a per-account rule subset, a username
// that is write-only, a per-account notification override. The files say which and
// why at the point the control would have gone.
//
// Keys is built against a server API that may not exist on the server this client
// is talking to, and degrades to "not available on this server" rather than
// guessing (ui/settings/rows/KeysRow.tsx).

import { useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import type { JSX } from "react";

import { t, tCount } from "../../i18n";
import { leaveTo, navigate, useListRoute, useRoute } from "../../routing";
import type { Route, SettingsRowId } from "../../../core/routes";
import { SETTINGS_ROWS, listRouteOf } from "../../../core/routes";
import { useApp, useMailState } from "../../state";
import type { MailState } from "../../../core/store";
import type { Rule } from "../../../core/jmap/types";
import { useShell } from "../shellContext";
import { useAccounts } from "../useAccounts";
import type { AccountsSnapshot } from "../useAccounts";
import { thresholdText } from "../desk/format";
import { DESK_GRID_ID } from "../desk/DeskTabs";
import { closeFocusTarget, focusablesIn, trapTab } from "../focusTrap";
import { ShortcutsHelp } from "../ShortcutsHelp";
import { CategoryTabsBody } from "../CategoryTabsSheet";
import { RemoteAllowsBody } from "../RemoteAllowsSheet";
import { RulesBody } from "../RuleEditor";
import { SavedSearchesBody } from "../SavedSearchesSheet";

import { SettingsRow, factsText } from "../settings/SettingsRow";
import type { StateFact } from "../settings/SettingsRow";
import type { SettingsScope } from "../settings/Scope";
import { filterSettings } from "../settings/search";
import { identityPrecedence } from "../settings/precedence";
import type { SearchableRow } from "../settings/search";
import {
  AppearanceBody,
  AppearanceMore,
  appearanceStateText,
  useAppearance,
} from "../settings/rows/AppearanceRow";
import { DataBody } from "../settings/rows/DataRow";
import { IdentitiesBody, identitiesStateText } from "../settings/rows/IdentitiesRow";
import { KeysBody, groupFingerprint, keysStateText, useUserKeys } from "../settings/rows/KeysRow";
import { useKeysApi } from "../settings/keys/api";
import { ApiKeysBody, apiKeysFacts, useApiKeys } from "../settings/rows/ApiKeysRow";
import { useApiKeysApi } from "../settings/apikeys/api";
import { DevicesBody, devicesFacts, useDevices } from "../settings/rows/DevicesRow";
import { useDevicesApi } from "../settings/devices/api";
import { usePendingApprovals } from "../pendingApprovals";
import { useKeepScope } from "../settings/nav";
import { NotificationsBody, pushStateKey, usePushStatus } from "../settings/rows/NotificationsRow";
import { quietHoursFact } from "../settings/notifyPrefs";
import { SendersBody, sendersFacts, useMarkedSenders } from "../settings/rows/SendersRow";
import { CleanupBody } from "../settings/rows/CleanupRow";
import { OutOfOfficeBody, useOutOfOffice } from "../settings/rows/OutOfOfficeRow";
import { outOfOfficeFacts } from "../settings/outOfOffice";
import { AccountsBody, AccountsMore } from "../settings/rows/AccountsRow";
import { AreasBody } from "../settings/rows/AreasRow";
import { LineBody } from "../settings/rows/LineRow";
import { TrayBody } from "../settings/rows/TrayRow";

/** The settings half of the route union, so `route.section` narrows without a
 * check at every use. */
type SettingsRouteValue = Extract<Route, { page: "settings" }>;

/** One row, fully built: what it is called, who it applies to, what it states,
 * what it can be found by, and what it opens into. `stateText` is DERIVED from
 * `facts`, so the line the search matches is the line the reader sees. */
interface RowSpec extends SearchableRow {
  id: SettingsRowId;
  scopes: SettingsScope[];
  facts: StateFact[];
  body: ReactNode;
  more?: ReactNode;
}

function row(spec: Omit<RowSpec, "stateText">): RowSpec {
  return { ...spec, stateText: factsText(spec.facts) };
}

const fact = (text: string): StateFact => ({ text });

// ---------------------------------------------------------------------------
// The state lines whose facts come straight from the store
// ---------------------------------------------------------------------------

/**
 * `3 · Main ok · Other domain paused` — the count, then each account with its
 * status word.
 *
 * ⚠ NO STATUS IS CLAIMED BEFORE THE TELEMETRY LANDS. `AccountEntry.status` is
 * "ok" until `Account/get` answers, which is a sensible default for a dot and a
 * LIE in a sentence: "ok" states that the user's mail is arriving. So before
 * `loaded` the names are printed bare — the account list comes from the synced
 * identities and is trustworthy from the first render, the status does not
 * (ui/useAccounts.ts draws exactly this distinction and says why).
 *
 * A FAILING ACCOUNT CARRIES ITS REASON, A PAUSED ONE DOES NOT, and the asymmetry
 * is in the data rather than in a preference: `syncStatus.lastError` is why sync
 * is failing, while "paused" is `syncEnabled: false` and the Account object
 * carries no reason for it at all. Printing the last error beside "paused" would
 * describe a state the user themselves ended (useAccounts.ts, "PAUSED WINS OVER
 * FAILING").
 */
function accountFacts(snapshot: AccountsSnapshot): StateFact[] {
  const { accounts, loaded } = snapshot;
  // No entries means the identities have not synced — not "you have no accounts".
  if (accounts.length === 0) return [];
  const facts: StateFact[] = [fact(String(accounts.length))];
  for (const account of accounts) {
    if (!loaded) {
      facts.push(fact(account.name));
      continue;
    }
    const word = t(
      account.status === "paused"
        ? "settings.state.acctPaused"
        : account.status === "failing"
          ? "settings.state.acctFailing"
          : "settings.state.acctOk",
    );
    const reason =
      account.status === "failing" ? (account.health?.syncStatus.lastError ?? null) : null;
    const text = reason === null ? `${account.name} ${word}` : `${account.name} ${word}: ${reason}`;
    facts.push(account.status === "ok" ? fact(text) : { text, tone: "warn" });
  }
  return facts;
}

/**
 * `6 areas · 3 labels in no area · 225 need sorting · 3 drawers auto-archive`.
 *
 * Every clause is omitted rather than zeroed, and three of them for reasons worth
 * naming:
 *
 *   * LABELS IN NO AREA is computed only when there is at least one area. With no
 *     areas loaded, EVERY label is "in no area" — arithmetically true and
 *     completely misleading to print while the areas are still arriving.
 *   * NEEDS SORTING is the `areaId: null` row of the desk summary, and the summary
 *     is null until the desk has been drawn once. Absent summary, absent clause:
 *     "0 need sorting" would be an inbox report this client has not taken.
 *   * DRAWERS AUTO-ARCHIVE is gated on `drawerSweepEnabled`, the login-level
 *     MASTER SWITCH. While it is off nothing is swept whatever any area says
 *     (types.ts DeskPreferences spells this out), so "3 drawers auto-archive"
 *     would describe something that does not happen. With the switch off the row
 *     says THAT instead, in the warning tone, because an area configured to sweep
 *     under a switch that is off is a setting quietly doing nothing.
 */
function areaFacts(state: MailState): StateFact[] {
  const areas = Object.values(state.areas);
  if (areas.length === 0) return [];
  const facts: StateFact[] = [fact(tCount("settings.state.areas", areas.length))];

  const labels = Object.values(state.labels);
  if (labels.length > 0) {
    const claimed = new Set<string>();
    for (const area of areas) {
      claimed.add(area.labelId);
      for (const id of area.labelIds) claimed.add(id);
    }
    const orphans = labels.filter((l) => !claimed.has(l.id)).length;
    if (orphans > 0) facts.push(fact(tCount("settings.state.labelsNoArea", orphans)));
  }

  const unsorted = state.deskSummary?.rows.find((r) => r.areaId === null);
  if (unsorted) {
    // The three counts are DISJOINT (types.ts AreaSummaryRow), so this is the
    // whole of the mail no area claims rather than a slice of it.
    const total = unsorted.surface + unsorted.drawer + unsorted.unscored;
    if (total > 0) facts.push(fact(tCount("settings.state.needSorting", total)));
  }

  const sweeping = areas.filter((a) => a.autoArchive).length;
  const prefs = state.deskPreferences;
  if (prefs !== null && sweeping > 0) {
    facts.push(
      prefs.drawerSweepEnabled
        ? fact(tCount("settings.state.drawersSweep", sweeping))
        : { text: tCount("settings.state.sweepOff", sweeping), tone: "warn" },
    );
  }
  return facts;
}

/**
 * `threshold 0.60` — and nothing else, deliberately.
 *
 * THE 7-DAY SPLIT AND THE ONE-STEP-STRICTER DELTA ARE IN THE BODY, not here, and
 * that is not because they arrived later: each costs a round trip
 * (sync.lineWeekCounts, sync.previewLine), and a state line that fetched would put
 * two requests behind merely LOOKING at this page. A state line is what is already
 * known; the body is what is worth asking for (rows/LineRow.tsx).
 *
 * The NUMBER is formatted by the desk's own `thresholdText`, deliberately: the
 * desk prints this value on the line itself, and two renderings of one number read
 * as two different answers to the same question.
 */
function lineFacts(state: MailState): StateFact[] {
  const prefs = state.deskPreferences;
  if (prefs === null) return [];
  return [fact(t("settings.state.threshold", { value: thresholdText(prefs.lineThreshold) }))];
}

/**
 * `nag after 7 days · returned snoozes and bounces are not yet routed here`.
 *
 * The second clause is TRUE TODAY and is the honest half of this row: the in-tray
 * rides the star flag (core/routes.ts — `#/tray` IS the starred view), so a
 * returned snooze or a bounce does not arrive in it. A user who expected it to
 * would otherwise conclude the tray was losing things.
 */
function trayFacts(state: MailState): StateFact[] {
  const facts: StateFact[] = [];
  const prefs = state.deskPreferences;
  if (prefs !== null) facts.push(fact(tCount("settings.state.trayNag", prefs.trayNagDays)));
  facts.push(fact(t("settings.state.trayNotRouted")));
  // Follow-up reminders (#58) live on this row; say which are on.
  const f = prefs?.followUp;
  if (f) {
    facts.push(
      fact(
        f.outbound && f.inbound
          ? t("settings.state.followUp.both")
          : f.outbound
            ? t("settings.state.followUp.outbound")
            : f.inbound
              ? t("settings.state.followUp.inbound")
              : t("settings.state.followUp.off"),
      ),
    );
  }
  return facts;
}

/**
 * `14 · 2 suggested`.
 *
 * NO "LAST FIRED", AND THAT IS A FINDING RATHER THAN AN OMISSION: `Rule`
 * (core/jmap/types.ts) carries id, name, priority, conditions, actions, enabled,
 * systemSuggested and corrections — there is no last-fired timestamp anywhere on
 * the object, so nothing here could print one without inventing it. `suggested` IS
 * derivable (`systemSuggested`) and is printed; "unreviewed" is not — there is no
 * reviewed flag — so it is not.
 *
 * `null` is THE LOAD FAILING and states nothing. An empty array is "you have no
 * rules", and the two must not collapse (core/actions.ts listRules says the same
 * from the other side).
 */
function ruleFacts(rules: Rule[] | null): StateFact[] {
  if (rules === null) return [];
  const facts: StateFact[] = [fact(String(rules.length))];
  const suggested = rules.filter((r) => r.systemSuggested).length;
  if (suggested > 0) facts.push(fact(t("settings.state.rulesSuggested", { count: suggested })));
  return facts;
}

/**
 * `remote images: ask each time · 12 senders always allowed`.
 *
 * The first clause is the app's FIXED behaviour, not a setting — remote images are
 * blocked until asked for, every time, and there is deliberately no control to
 * change that wholesale (ui/RemoteAllowsSheet.tsx says why). It is stated so a
 * reader who cannot find the control knows the answer is not hidden from them.
 */
function readingFacts(allowCount: number): StateFact[] {
  const facts: StateFact[] = [fact(t("settings.state.remoteImages"))];
  if (allowCount > 0) facts.push(fact(tCount("settings.state.sendersAllowed", allowCount)));
  return facts;
}

// ---------------------------------------------------------------------------
// The route
// ---------------------------------------------------------------------------

export function SettingsRoute(): JSX.Element | null {
  const route = useRoute();
  if (route.page !== "settings") return null;
  // The keyboard sheet is not a row: nothing on it can be changed, searched for by
  // value, or scoped, so it stays the sheet the `?` key has always opened.
  if (route.section === "shortcuts") return <ShortcutsSheet route={route} />;
  return <SettingsPage route={route} />;
}

/** `#/settings/shortcuts`. Backing out of it lands on the settings page, exactly
 * as backing out of a section did before — escapeTarget peels one layer
 * (core/routes.ts). */
function ShortcutsSheet({ route }: { route: SettingsRouteValue }): JSX.Element {
  return (
    <ShortcutsHelp
      onClose={() =>
        leaveTo({ page: "settings", section: null, ...(route.acct ? { acct: route.acct } : {}) })
      }
    />
  );
}

function SettingsPage({ route }: { route: SettingsRouteValue }): JSX.Element {
  const listRoute = useListRoute();
  const shell = useShell();
  const { actions, client, pushToast } = useApp();
  const state = useMailState();
  const accounts = useAccounts();
  const appearance = useAppearance();
  const push = usePushStatus();
  const senders = useMarkedSenders();
  const oof = useOutOfOffice();
  const keys = useUserKeys();
  const keysApi = useKeysApi();
  const apiKeysApi = useApiKeysApi();
  const devicesApi = useDevicesApi();
  // The banner (Shell) polls; this page reads the same snapshot and refreshes
  // it after a decision, so the two never disagree.
  const pending = usePendingApprovals(false);
  const keepScope = useKeepScope();
  const [query, setQuery] = useState("");

  // `shortcuts` never reaches here — SettingsRoute renders the sheet for it and
  // returns — but the route type cannot say so, and narrowing it HERE rather than
  // asserting it means a future section that is not a row lands on the page with
  // nothing expanded instead of on a lookup that returns undefined.
  const section: SettingsRowId | null = route.section === "shortcuts" ? null : route.section;
  const rulesOpen = section === "rules";

  /**
   * THE RULES, FETCHED ONCE AND CACHED IN COMPONENT STATE.
   *
   * The count is a state-line fact and the names are search VALUES, so the search
   * needs them — and a fetch per keystroke would put a JMAP round trip behind
   * every character typed into the field.
   *
   * Read on mount, and read again on the transition OUT of the Rules row: the body
   * there creates, edits and deletes rules, and a count that still said 14 after
   * the user deleted one would be exactly the sort of stale number that makes
   * every other number on this page suspect. Not on every section change — that
   * would be a round trip per row anyone opens.
   */
  const [rules, setRules] = useState<Rule[] | null>(null);
  const rulesWereOpen = useRef(false);
  // Gated on the session having landed for the reason the Keys row is
  // (rows/KeysRow.tsx): asking before there is an account id fails, `null` is the
  // load FAILING, and this effect would then not run again until the Rules row
  // was opened and closed — a sticky "no facts" on a row that has fourteen.
  const accountId = state.accountId;
  const apiKeys = useApiKeys(apiKeysApi, Boolean(accountId));
  const devices = useDevices(devicesApi, Boolean(accountId));
  useEffect(() => {
    const justClosed = rulesWereOpen.current && !rulesOpen;
    rulesWereOpen.current = rulesOpen;
    if (!accountId) return;
    if (rules !== null && !justClosed) return;
    let cancelled = false;
    void actions.listRules().then((rs) => {
      if (!cancelled) setRules(rs);
    });
    return () => {
      cancelled = true;
    };
    // `rules` is read but deliberately not a dependency: it is what this effect
    // SETS, and depending on it would re-run the effect with its own answer. The
    // effect re-runs on `rulesOpen` and on the session arriving, which is when the
    // value read here matters.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [actions, accountId, rulesOpen]);

  const snippets = Object.values(state.composeSnippets);
  const identities = Object.values(state.identities);
  // The COUNTS come from the precedence, not from the store's map, so the number
  // on the state line and the list inside the row cannot disagree: an identity
  // with no account is left out of both (settings/precedence.ts says why).
  const precedence = identityPrecedence(state.identities);
  const savedSearches = Object.values(state.savedSearches);
  const remoteAllows = Object.values(state.remoteAllows);

  /**
   * THE TWELVE ROWS, keyed by id.
   *
   * A `Record<SettingsRowId, …>` rather than an array, so a row missing from this
   * page is a TYPE error rather than a URL that expands nothing; the render order
   * comes from SETTINGS_ROWS (core/routes.ts) rather than from the order they
   * happen to be written in here, so the route vocabulary and the page cannot
   * disagree about what comes first.
   *
   * Rebuilt on every render, and that is what render is: the bodies are React
   * elements, so reconciliation keeps each body's own state (a half-written rule,
   * an open editor) across a keystroke in the search field.
   */
  const specs: Record<SettingsRowId, RowSpec> = {
    accounts: row({
      id: "accounts",
      name: t("settings.accounts"),
      scopes: ["account"],
      facts: accountFacts(accounts),
      // The account names AND the folder names, because the per-folder sync switch
      // lives in this row since increment 3 — someone hunting for "why is Archive
      // frozen" types the folder's name, not the account's.
      values: [
        ...accounts.accounts.map((a) => a.name),
        ...Object.values(state.mailboxes).map((m) => m.name),
      ],
      body: <AccountsBody accountId={route.accountId ?? null} />,
      // ⚠ THE ROW'S `more` IS THE ACCOUNT'S `more`, and it exists only inside an
      // account. SettingsRow renders one `more` per row, so the two screens this
      // row has cannot each declare their own — and the LIST has nothing rare to
      // hold, while an account has its provider profile, its rules and the
      // remove-account door. Absent on the list, present on the detail.
      ...(route.accountId
        ? { more: <AccountsMore accountId={route.accountId} rules={rules} /> }
        : {}),
    }),
    areas: row({
      id: "areas",
      name: t("settings.areas"),
      scopes: ["login", "area"],
      facts: areaFacts(state),
      // Areas AND labels: a label is one of the things that feeds an area, so
      // "where does Receipts go?" is a question about this row.
      values: [
        ...Object.values(state.areas).map((a) => a.name),
        ...Object.values(state.labels).map((l) => l.name),
      ],
      // The rules come from the fetch above rather than from a second one: the
      // cards list the rules that place mail in each area, which is a JOIN over the
      // same array the state line counts (settings/areaFeeds.ts rulesForArea).
      body: <AreasBody rules={rules} areaId={route.areaId ?? null} />,
      // The category strip's enable/order controls, which are another of the three
      // things that feed an area. Behind `more` because arranging the strip is a
      // once-a-year job; reachable from here so #373's `#/settings/category-tabs`
      // lost nothing.
      more: <CategoryTabsBody />,
    }),
    line: row({
      id: "line",
      name: t("settings.line"),
      scopes: ["login"],
      facts: lineFacts(state),
      values: [],
      body: <LineBody />,
    }),
    tray: row({
      id: "tray",
      name: t("settings.tray"),
      scopes: ["login"],
      facts: trayFacts(state),
      // Findable by the words a user would type for the reminders on it (#58).
      values: [t("settings.followUp.title"), t("followUp.title")],
      body: <TrayBody />,
    }),
    rules: row({
      id: "rules",
      name: t("rules.title"),
      scopes: ["login", "account"],
      facts: ruleFacts(rules),
      values: (rules ?? []).map((r) => r.name),
      body: <RulesBody initialDraft={shell.ruleDraft ?? undefined} />,
    }),
    identities: row({
      id: "identities",
      name: t("settings.identities"),
      scopes: ["account"],
      facts: identitiesStateText(
        precedence.reduce((n, g) => n + g.entries.length, 0),
        precedence.length,
        snippets.filter((s) => s.kind === "signature").length,
        snippets.filter((s) => s.kind === "template").length,
      ).map(fact),
      values: [
        ...identities.map((i) => i.email),
        ...identities.map((i) => i.name).filter((n) => n !== ""),
        ...snippets.map((s) => s.name),
      ],
      body: <IdentitiesBody />,
    }),
    notifications: row({
      id: "notifications",
      name: t("settings.notifications"),
      scopes: ["login", "account"],
      // Nothing at all until the read resolves: "off" states that the user's
      // notifications are not arriving, and saying it before asking is how a page
      // invents an outage.
      facts: [
        ...(push.status === null ? [] : [fact(t(pushStateKey(push.status)))]),
        // Omitted, not guessed, until DeskPreferences has landed or against a
        // server before #59 (rule 2 of this page).
        ...(quietHoursFact(state.deskPreferences?.quietHours) === null
          ? []
          : [fact(quietHoursFact(state.deskPreferences?.quietHours) as string)]),
      ],
      // The controls' names, so "quiet hours" or "VIP" finds this row.
      values: [t("settings.quiet.legend"), t("settings.notify.mode.legend"), t("settings.notify.account.legend")],
      body: <NotificationsBody api={push} />,
    }),
    senders: row({
      id: "senders",
      name: t("settings.senders"),
      scopes: ["login"],
      facts: sendersFacts(senders).map(fact),
      // The marked addresses themselves, so typing one finds the row.
      values: [...(senders.vips?.list ?? []), ...(senders.blocked?.list ?? [])].flatMap((c) =>
        [c.displayName, ...c.addresses.map((a) => a.address)].filter((s) => s !== ""),
      ),
      body: <SendersBody senders={senders} />,
    }),
    // Out of office (#50): one reply per mail account, sent by the provider.
    // Nothing on the line until the server answers, and against a server
    // without the API the row says it cannot (rule 2 of this page).
    "out-of-office": row({
      id: "out-of-office",
      name: t("settings.outOfOffice"),
      scopes: ["account"],
      facts: outOfOfficeFacts(oof.list).map(fact),
      // The control names, so "vacation" or "automatic reply" finds the row.
      values: [t("oof.enable"), "vacation", "auto reply", "away", ...(oof.list ?? []).map((o) => o.subject)].filter(
        (s) => s !== "",
      ),
      body: <OutOfOfficeBody oof={oof} />,
    }),
    cleanup: row({
      id: "cleanup",
      name: t("settings.cleanup"),
      scopes: ["login"],
      // No count on the line: the ranking is a query over the whole mailbox,
      // asked for when the row opens rather than behind merely looking at
      // this page (the Line row's reason).
      facts: [fact(t("settings.state.cleanup"))],
      values: [t("cleanup.unsubscribe"), t("cleanup.archiveAll"), t("cleanup.block")],
      body: <CleanupBody />,
    }),
    reading: row({
      id: "reading",
      name: t("settings.reading"),
      scopes: ["login"],
      facts: readingFacts(remoteAllows.length),
      values: remoteAllows.map((a) =>
        a.kind === "domain" ? t("remoteAllows.anyoneAt", { domain: a.value }) : a.value,
      ),
      body: <RemoteAllowsBody />,
    }),
    keys: row({
      id: "keys",
      name: t("settings.keys"),
      scopes: ["login"],
      // An expired or expiring key is a warn-toned fact on this line (#795).
      facts: keysStateText(keys.reading, client.pgpKeyExpiryWarningDays()),
      // A fingerprint is searched for in whichever form the user has it in — the
      // grouped one they read off this page, or the unspaced one they copied out of
      // another tool. The verbs are named too, so "revoke" or "back up" finds it.
      values: [
        ...(keys.reading.kind === "loaded"
          ? keys.reading.keys.flatMap((k) => [k.fingerprint, groupFingerprint(k.fingerprint)])
          : []),
        "PGP",
        "OpenPGP",
        t("keys.action.revoke"),
        t("keys.action.backup"),
        t("keys.action.extend"),
        t("keys.theirs.heading"),
      ],
      body: (
        <KeysBody
          reading={keys.reading}
          reload={keys.reload}
          identities={Object.values(state.identities)}
          api={keysApi}
          keyManagement={client.pgpKeyManagement()}
          warnDays={client.pgpKeyExpiryWarningDays()}
        />
      ),
    }),
    // API keys, approvals and AI-tool setup (#49). The waiting requests are on
    // the state line in the warn tone: they expire, so they are the one thing
    // on this page with a deadline.
    "api-keys": row({
      id: "api-keys",
      name: t("settings.apiKeys"),
      scopes: ["login"],
      facts: apiKeysFacts(apiKeys.reading, pending.waiting),
      values: [
        ...(apiKeys.reading.kind === "loaded" ? apiKeys.reading.keys.map((k) => k.name) : []),
        "MCP",
        "API",
        "agent",
        "AI",
        "token",
        t("apiKeys.pending.heading"),
        t("apiKeys.setup.heading"),
      ],
      body: (
        <ApiKeysBody
          reading={apiKeys.reading}
          reload={apiKeys.reload}
          api={apiKeysApi}
          waiting={pending.waiting}
          refreshPending={() => void pending.refresh()}
          accounts={accounts.accounts.map((a) => ({ id: a.id, name: a.name }))}
          apiKeyId={route.apiKeyId ?? null}
          openKey={(id) =>
            keepScope(id ? { page: "settings", section: "api-keys", apiKeyId: id } : { page: "settings", section: "api-keys" })
          }
          origin={window.location.origin}
        />
      ),
    }),
    // Signed-in devices (#937, ADR-048 section 6): the login's live sessions.
    devices: row({
      id: "devices",
      name: t("settings.devices"),
      scopes: ["login"],
      facts: devicesFacts(devices.reading),
      values: [
        ...(devices.reading.kind === "loaded" ? devices.reading.devices.map((d) => d.description) : []),
        "session",
        "sign out",
        "security",
      ],
      body: <DevicesBody reading={devices.reading} reload={devices.reload} api={devicesApi} notify={pushToast} />,
    }),
    "saved-searches": row({
      id: "saved-searches",
      name: t("savedSearch.manage"),
      scopes: ["login"],
      // A count, and no clause when there are none: the body says the useful thing
      // about an empty list, and "0" on the line says nothing.
      facts: savedSearches.length > 0 ? [fact(String(savedSearches.length))] : [],
      // The QUERY as well as the name: a smart folder is findable by what it looks
      // for, which is often the only part its owner remembers.
      values: [...savedSearches.map((s) => s.name), ...savedSearches.map((s) => s.query)],
      body: <SavedSearchesBody />,
    }),
    appearance: row({
      id: "appearance",
      name: t("settings.appearance"),
      scopes: ["web"],
      facts: appearanceStateText(appearance.reading).map(fact),
      // The two controls added by #514 are NAMED here so the search finds this
      // row for "text size" or "rows". The other four controls are not, and
      // that is not an inconsistency: theme, density and the pane are already
      // in the state line, which the search reads. These two are only in the
      // line when they are NOT the default (AppearanceRow.tsx says why), so
      // without this a user at the default could not find the setting they were
      // looking for by typing its name — which is the one moment they need to.
      values: [t("settings.textSize"), t("settings.rows")],
      body: <AppearanceBody api={appearance} />,
      more: <AppearanceMore />,
    }),
    data: row({
      id: "data",
      name: t("settings.data"),
      scopes: ["login"],
      facts: [fact(t("settings.state.dataOperator"))],
      values: [],
      body: <DataBody />,
    }),
  };

  const matches = filterSettings(
    SETTINGS_ROWS.map((id) => specs[id]),
    query,
  );

  /**
   * WHICH ROW IS OPEN. The route's own section — except that a search narrowed to
   * exactly ONE row opens it, which is the point of typing a value into the field:
   * you get the row and its controls, not a row you then have to click.
   *
   * One at a time by construction: this is a single id, not a set.
   */
  const expanded: SettingsRowId | null = matches.length === 1 ? (matches[0]?.id ?? null) : section;

  const acct = route.acct ? { acct: route.acct } : {};

  /**
   * Clicking a heading writes the ROUTE's section, not the derived expansion.
   *
   * The two differ exactly when a search has narrowed the page to one row: that
   * row is open because it is the only match, and clicking its heading should
   * put it in the URL rather than fight the search that opened it. Deriving the
   * next section from `expanded` instead would make the click a no-op you can
   * see (collapse, then immediately re-expand as the only match).
   */
  const toggle = (id: SettingsRowId): void => {
    // A rule drafted from a message (shellContext openRuleDraft) is consumed by
    // opening the row; folding the row away is the user declining it. Without this,
    // re-opening Rules would hand them back a form they had already dismissed.
    if (id === "rules" && section === "rules") shell.clearRuleDraft();
    navigate({ page: "settings", section: section === id ? null : id, ...acct });
  };

  const close = (): void => {
    shell.clearRuleDraft();
    // ONE CLICK, WHATEVER IS OPEN (reported against the v1.80.0 beta:
    // "I need to click it multiple times"). `leaveTo` undoes exactly ONE pushed
    // history entry — right for a single-layer sheet's ✕ (ShortcutsSheet below),
    // wrong here: entering settings pushes an entry, and so does opening a
    // section or a detail (`toggle` above, settings/nav.ts useKeepScope), so from
    // a detail `leaveTo(listRoute)` walked back one layer per click instead of
    // leaving in one. `listRouteOf` already computes "the list underneath" for
    // ANY depth in a single step — the same route `escapeTarget` peels back one
    // press at a time, which is what Escape and the browser Back button still do
    // on purpose.
    navigate(listRouteOf(route, listRoute), { replace: true });
  };

  const dialogRef = useRef<HTMLDivElement | null>(null);

  /**
   * THE TRAP THIS PAGE HAS BEEN CLAIMING WITHOUT DELIVERING (#447): it has said
   * `aria-modal="true"` since increment 2 while Tab walked straight out into the
   * desk behind it. Open puts focus on the search field — the one control every
   * visit to this page starts at, so a keyboard user does not have to walk in
   * from the top — or the first focusable if the field is somehow absent;
   * Tab/Shift+Tab wrap at the two edges (ui/focusTrap.ts, shared with the viewer
   * and the confirm dialog). Close hands focus back to whatever opened this page
   * if it is still in the document, or to the desk's grid if not — never
   * `<body>`, which is where a screen reader loses its place entirely and the
   * grid is the one thing that remembers which row it was on.
   */
  const openerRef = useRef<HTMLElement | null>(null);
  useEffect(() => {
    openerRef.current = document.activeElement as HTMLElement | null;
    const root = dialogRef.current;
    const search = root?.querySelector<HTMLElement>(".settings-search input");
    (search ?? (root ? focusablesIn(root)[0] : undefined))?.focus();
    return () => {
      const opener = openerRef.current;
      if (closeFocusTarget(opener) === "opener") {
        opener?.focus();
        return;
      }
      document.querySelector<HTMLElement>(`#${DESK_GRID_ID}`)?.focus();
    };
  }, []);

  return (
    // ⚠ NOT A `<main>` LANDMARK, DELIBERATELY (#447). Settings sits ON TOP of the
    // list route rather than replacing it (ui/App.tsx renders it as `overlays`
    // while `children` stays mounted), so the desk's own `<main id="messages">`
    // is still in the document: a second one here would be two `main` landmarks
    // on one page — a real violation, and the first thing an accessibility audit
    // reports. What the landmark was wanted FOR is the heading outline, and that
    // is already here without it: an `<h2>` naming the page and an `<h3>` per row
    // (ui/settings/SettingsRow.tsx), each one a jump target, inside a dialog that
    // says it is modal.
    <div
      className="sheet settings-page"
      role="dialog"
      aria-modal="true"
      aria-label={t("settings.title")}
      ref={dialogRef}
      // THE TRAP. Only Tab is acted on here — every other key on this page is
      // ordinary input, unlike the viewer, which owns the whole keyboard while it
      // is open.
      onKeyDown={(ev) => trapTab(dialogRef.current, ev)}
    >
      <header className="sheet-header">
        <h2>{t("settings.title")}</h2>
        <div className="settings-header-actions">
          {/* The keyboard sheet — the one settings surface that is not a row. A
              words-only button, like the rest of this page. */}
          <button
            type="button"
            className="link-button"
            onClick={() => navigate({ page: "settings", section: "shortcuts", ...acct })}
          >
            {t("settings.shortcuts")}
          </button>
          <button
            type="button"
            className="icon-button"
            aria-label={t("action.close")}
            onClick={close}
          >
            ✕
          </button>
        </div>
      </header>

      <div className="settings-search">
        <label className="field">
          {/* The label is visible, not a placeholder: a placeholder disappears the
              moment anyone types, taking the only explanation of what the field
              searches with it. */}
          <span className="field-label">{t("settings.search.label")}</span>
          <input
            type="search"
            className="text-input"
            value={query}
            placeholder={t("settings.search.placeholder")}
            onChange={(e) => setQuery(e.target.value)}
            // ESCAPE PEELS THE NARROWED SEARCH FIRST, and the propagation stop is
            // what makes that one layer rather than two. A `type="search"` input
            // clears itself on Escape, AND the shell's window listener would peel
            // the settings route on the same keystroke — one press, two layers,
            // which is exactly what app/routing.ts's consumeEscape exists to
            // prevent for sheets. React's stopPropagation stops the NATIVE event
            // at its root container, so the window listener never sees it.
            onKeyDown={(e) => {
              if (e.key !== "Escape" || query === "") return;
              e.preventDefault();
              e.stopPropagation();
              setQuery("");
            }}
          />
        </label>
      </div>

      {matches.length === 0 ? (
        <p className="empty">{t("settings.search.none", { query })}</p>
      ) : (
        <div className="settings-rows">
          {matches.map((r) => (
            <SettingsRow
              key={r.id}
              id={r.id}
              name={r.name}
              scopes={r.scopes}
              facts={r.facts}
              expanded={expanded === r.id}
              onToggle={() => toggle(r.id)}
              {...(r.more ? { more: r.more } : {})}
            >
              {r.body}
            </SettingsRow>
          ))}
        </div>
      )}
    </div>
  );
}
