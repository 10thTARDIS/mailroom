// SPDX-License-Identifier: AGPL-3.0-only

// AREAS AND DRAWERS (#374 increment 3) — the row that configures the desk, and the
// only row on the page whose scope is genuinely two words.
//
//   login   the default drawer period every new area starts on
//   area    everything on every card below it
//
// An area is a region of the desk with a LABEL of its own, a set of FEEDS that
// route mail into it, and a DRAWER that keeps what falls below the line. This row
// is where all three are set, and the ordering top-to-bottom is deliberate: the
// login-wide default first (it is the thing the cards inherit), then the cards,
// then the two things that are ABOUT the areas without being one of them (labels
// no area claims, mail no area claims).
//
// ⚠ THE SWEEP HAS A MASTER SWITCH AND IT IS NOT SHOWN HERE. `drawerSweepEnabled`
// (#443) is the OPERATOR's — `mailroom-admin drawer-report --enable` — and it
// defaults to FALSE, so on an instance nobody has enabled it every card can be
// configured to auto-archive and nothing is ever archived. Offering it as a client
// control would be offering a switch the client is not the authority on; hiding the
// FACT would be worse. So while it is off, the row says so once and every card
// repeats `sweep off` beside its period, exactly as the desk's drawer rows do
// (ui/desk/DrawerRow.tsx, i18n `desk.drawer.sweepOff`).
//
// ⚠ EVERY FEED WRITE SENDS BOTH ARRAYS. `tabs` and `labelIds` are REPLACE-ALL
// server-side, so a tab toggle that sent only the tab would quietly unsubscribe the
// area from all of its label feeds. `feedPatch` (settings/areaFeeds.ts) is the only
// thing that builds them and it is unit-tested for exactly that.
//
// REFUSALS LAND ON THE CARD, not in a toast. The two the server actually produces
// name the thing that is in the way — "the promotions tab already feeds Shopping",
// "an area with that name already exists" — and they are answers about the control
// still on screen (core/actions.ts AreaWrite says the same from the other side).

import { useEffect, useRef, useState } from "react";
import type { JSX } from "react";

import { t, tCount } from "../../../i18n";
import type { MessageKey } from "../../../i18n";
import { useApp, useMailState } from "../../../state";
import { LearnedPlacementToggle } from "./LearnedPlacementToggle";
import type { Area, CategoryTab, DeskPreferences, Id, Label, Rule } from "../../../../core/jmap/types";
import { ConfirmDialog } from "../../desk/ConfirmDialog";
import { feedPatch, labelsInNoArea, movedByOne, rulesForArea } from "../areaFeeds";
import { useKeepScope } from "../nav";
import { useAccounts } from "../../useAccounts";
import {
  DRAWER_DEFAULT_DAYS,
  DRAWER_PERIOD_PRESETS,
  drawerPeriodChoice,
  drawerPeriodPatch,
} from "../drawerPeriod";
import type { DrawerPeriod } from "../drawerPeriod";

/** The five tabs, in the order the strip declares them. Hard-coded because a
 * toggle control has to name the values it offers, which is the same reason
 * core/jmap/types.ts spells out `CategoryTab`. */
const TABS: readonly CategoryTab[] = ["primary", "social", "promotions", "updates", "forums"];

function tabLabel(tab: CategoryTab): string {
  return t(`tabs.${tab}` as MessageKey);
}

/** Which OTHER area already holds a tab as a feed, or null. A feed is exclusive
 * server-side; this is a hint from the client's own copy, not a substitute for the
 * refusal (see the comment at the checkbox). */
function tabHolder(tab: CategoryTab, others: readonly Area[]): Area | null {
  return others.find((a) => a.tabs.includes(tab)) ?? null;
}

/** The same for a label — which counts an area's OWN label as held, because it is
 * a feed row for that area and adding it here is refused just the same. */
function labelHolder(labelId: Id, others: readonly Area[]): Area | null {
  return others.find((a) => a.labelId === labelId || a.labelIds.includes(labelId)) ?? null;
}

// ---------------------------------------------------------------------------
// The login-wide default
// ---------------------------------------------------------------------------

/**
 * `New areas auto-archive after 30 days`.
 *
 * ⚠ IT DOES NOT MOVE EXISTING AREAS — it moves every area that FOLLOWS the
 * default, which is every area whose `autoArchiveDays` is null, and that is a
 * different and larger set than "new areas". The sentence says both halves,
 * because a user who read only the first would be surprised twice: once when an
 * old area moved, once when a pinned one did not.
 */
function LoginDefault({ prefs }: { prefs: DeskPreferences | null }): JSX.Element {
  const { actions } = useApp();
  const [busy, setBusy] = useState(false);
  const [custom, setCustom] = useState("");

  if (prefs === null) {
    // Nothing is claimed: `30` here would be this client stating a login default it
    // has not read, and the whole page rests on not doing that.
    return (
      <fieldset className="menu-group">
        <legend>{t("settings.areas.default")}</legend>
        <p className="field-hint">{t("settings.areas.default.unread")}</p>
      </fieldset>
    );
  }

  const days = prefs.drawerDefaultDays;
  const changed = days !== DRAWER_DEFAULT_DAYS;
  const isPreset = DRAWER_PERIOD_PRESETS.includes(days);

  const write = (n: number): void => {
    if (!Number.isFinite(n) || n <= 0) return;
    setBusy(true);
    void (async () => {
      await actions.updateDeskPreferences(
        { drawerDefaultDays: Math.floor(n) },
        t("settings.areas.default.failed"),
      );
      setBusy(false);
    })();
  };

  return (
    <fieldset className="menu-group">
      <legend>
        {t("settings.areas.default")}
        {changed ? (
          <span className="setting-changed">
            <span className="setting-changed-word">{t("settings.changed")}</span>
            <button
              type="button"
              className="link-button"
              disabled={busy}
              onClick={() => write(DRAWER_DEFAULT_DAYS)}
            >
              {t("settings.areas.default.reset", { n: DRAWER_DEFAULT_DAYS })}
            </button>
          </span>
        ) : null}
      </legend>
      <p className="menu-note">{tCount("settings.areas.default.value", days)}</p>
      <div className="field-inline">
        <select
          className="select"
          value={isPreset ? String(days) : "custom"}
          disabled={busy}
          aria-label={t("settings.areas.default")}
          onChange={(e) => {
            if (e.target.value === "custom") {
              setCustom(String(days));
              return;
            }
            write(Number(e.target.value));
          }}
        >
          {DRAWER_PERIOD_PRESETS.map((n) => (
            <option key={n} value={String(n)}>
              {tCount("settings.areas.days", n)}
            </option>
          ))}
          <option value="custom">{t("settings.areas.custom")}</option>
        </select>
        {isPreset && custom === "" ? null : (
          <>
            <input
              type="number"
              className="text-input setting-days-input"
              min={1}
              value={custom === "" ? String(days) : custom}
              disabled={busy}
              aria-label={t("settings.areas.custom")}
              onChange={(e) => setCustom(e.target.value)}
            />
            <button
              type="button"
              className="button button-quiet"
              disabled={busy}
              onClick={() => write(Number(custom === "" ? days : custom))}
            >
              {t("settings.areas.apply")}
            </button>
          </>
        )}
      </div>
      <p className="field-hint">{t("settings.areas.default.explain")}</p>
    </fieldset>
  );
}

// ---------------------------------------------------------------------------
// One area's card
// ---------------------------------------------------------------------------

/** The option a select shows for one drawer period, as a plain string key. The
 * union is flattened for the `<select value>` and expanded again on change, which
 * is the smallest amount of stringly-typed code that a native select forces. */
function periodKey(period: DrawerPeriod): string {
  if (period.kind === "never") return "never";
  if (period.kind === "default") return "default";
  return DRAWER_PERIOD_PRESETS.includes(period.days) ? String(period.days) : "custom";
}

function AreaCard({
  area,
  prefs,
  labels,
  rules,
  otherAreas,
  archived,
  first,
  last,
  current,
  onMove,
}: {
  area: Area;
  prefs: DeskPreferences | null;
  labels: Label[];
  rules: Rule[] | null;
  /** Every OTHER area, for the "already feeds X" hint beside a claimed feed. */
  otherAreas: Area[];
  /** `archivedByDrawer` from the desk summary, or null when no summary is loaded —
   * in which case the count is OMITTED rather than printed as 0. */
  archived: number | null;
  first: boolean;
  last: boolean;
  /** This is the card `#/settings/areas/<id>` named — scrolled to, and marked. */
  current: boolean;
  /** Move this card by one place. The ROW owns it, because a swap is a fact about
   * the whole list and a card cannot see its neighbour. */
  onMove: (delta: -1 | 1) => void;
}): JSX.Element {
  const { actions } = useApp();
  const go = useKeepScope();
  const cardRef = useRef<HTMLDivElement | null>(null);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [renaming, setRenaming] = useState<string | null>(null);
  const [custom, setCustom] = useState("");
  const [moreOpen, setMoreOpen] = useState(false);
  const [confirming, setConfirming] = useState(false);

  // Only when it BECOMES current, so a card the user has scrolled away from is not
  // dragged back under them on every re-render of the row.
  useEffect(() => {
    if (current) cardRef.current?.scrollIntoView({ block: "nearest" });
  }, [current]);

  const period = drawerPeriodChoice(area);
  const sweepOn = prefs?.drawerSweepEnabled ?? false;
  const ownLabel = labels.find((l) => l.id === area.labelId);
  const feedLabels = labels.filter((l) => l.id !== area.labelId);

  /** One write, with the refusal kept on the card. Every control on the card goes
   * through this, so there is exactly one place that decides what a refusal does. */
  const write = (patch: Record<string, unknown>): void => {
    setBusy(true);
    setRefusal(null);
    void (async () => {
      const res = await actions.updateArea(area.id, patch);
      setBusy(false);
      setRefusal(res.ok ? null : res.refusal);
    })();
  };

  const setPeriod = (next: DrawerPeriod): void => write({ ...drawerPeriodPatch(next) });

  return (
    <div
      className={current ? "setting-area-card is-current" : "setting-area-card"}
      ref={cardRef}
      aria-current={current ? "true" : undefined}
    >
      {/* The refusal, at the top of the card it belongs to. `role="alert"` because
          it appears in response to something the user just did and they may not be
          looking at this corner of the card. */}
      {refusal === null ? null : (
        <p className="banner banner-warn" role="alert">
          {refusal}
        </p>
      )}

      <div className="setting-area-head">
        {renaming === null ? (
          <>
            <span className="setting-area-name">{area.name}</span>
            <button
              type="button"
              className="link-button"
              disabled={busy}
              onClick={() => setRenaming(area.name)}
            >
              {t("settings.areas.rename")}
            </button>
          </>
        ) : (
          <form
            className="field-inline"
            onSubmit={(e) => {
              e.preventDefault();
              const next = renaming.trim();
              // An unchanged name is not a write: it would spend a round trip to be
              // told nothing changed, and on a name-taken refusal it would report a
              // collision with itself.
              if (next === "" || next === area.name) {
                setRenaming(null);
                return;
              }
              setRenaming(null);
              write({ name: next });
            }}
          >
            <input
              className="text-input"
              type="text"
              value={renaming}
              disabled={busy}
              aria-label={t("settings.areas.rename")}
              onChange={(e) => setRenaming(e.target.value)}
              autoFocus
            />
            <button type="submit" className="button button-quiet" disabled={busy}>
              {t("action.save")}
            </button>
            <button
              type="button"
              className="link-button"
              disabled={busy}
              onClick={() => setRenaming(null)}
            >
              {t("action.cancel")}
            </button>
          </form>
        )}
        {/* ↑ ↓ rather than a drag handle: this is a list of at most a handful of
            rows, the keyboard gets the same control as the mouse for free, and
            `position` is an integer the server orders on. The ends are disabled
            rather than absent, so the two buttons do not move between cards.
            ⚠ THEY SWAP, THEY DO NOT DECREMENT. `position` has no uniqueness
            constraint and the store orders by `position, id`, so writing one area's
            position down to match its neighbour's leaves them TIED and the tie
            breaks on creation order — the arrow would move the card sometimes and
            not others. core/actions.ts reorderAreas renumbers the list instead. */}
        <button
          type="button"
          className="icon-button"
          disabled={busy || first}
          aria-label={t("settings.areas.moveUp", { name: area.name })}
          onClick={() => onMove(-1)}
        >
          ↑
        </button>
        <button
          type="button"
          className="icon-button"
          disabled={busy || last}
          aria-label={t("settings.areas.moveDown", { name: area.name })}
          onClick={() => onMove(1)}
        >
          ↓
        </button>
      </div>

      {/* ---- fed by ---- */}
      <fieldset className="menu-group">
        <legend>{t("settings.areas.fedBy")}</legend>
        {/* The OWN label first, and not as a control: it is immutable and placing a
            message in this area IS applying it, so a removable-looking checkbox
            beside it would offer a write the server refuses. */}
        <p className="menu-note">
          {ownLabel
            ? t("settings.areas.ownLabel", { name: ownLabel.name })
            : t("settings.areas.ownLabelUnread")}
        </p>
        {/* ⚠ A FEED ANOTHER AREA HOLDS IS ANNOTATED, NOT DISABLED, and the split is
            deliberate. The annotation is a READ of this client's own areas — the same
            data the server's refusal would name — so it is a hint rather than a
            second opinion, and it saves clicking through a refusal per label on a
            desk with six areas. Disabling would be different: it would make the
            client's copy load-bearing, and a client one poll behind would then
            forbid a feed the server would have accepted. So the box stays clickable
            and the server stays the authority. */}
        <div className="setting-feed-tabs">
          {TABS.map((tab) => {
            const held = area.tabs.includes(tab) ? null : tabHolder(tab, otherAreas);
            return (
              <label className="checkbox" key={tab}>
                <input
                  type="checkbox"
                  checked={area.tabs.includes(tab)}
                  disabled={busy}
                  onChange={() => write({ ...feedPatch(area, { kind: "toggleTab", tab }) })}
                />
                {tabLabel(tab)}
                {held === null ? null : (
                  <span className="remote-allow-kind">
                    {t("settings.areas.feedsOther", { name: held.name })}
                  </span>
                )}
              </label>
            );
          })}
        </div>
        {feedLabels.length === 0 ? (
          <p className="field-hint">{t("settings.areas.noOtherLabels")}</p>
        ) : (
          <div className="setting-feed-labels">
            {feedLabels.map((label) => {
              const on = area.labelIds.includes(label.id);
              const held = on ? null : labelHolder(label.id, otherAreas);
              return (
                <label className="checkbox" key={label.id}>
                  <input
                    type="checkbox"
                    checked={on}
                    disabled={busy}
                    onChange={() =>
                      write({
                        ...feedPatch(area, {
                          kind: on ? "removeLabel" : "addLabel",
                          labelId: label.id,
                        }),
                      })
                    }
                  />
                  {label.name}
                  {held === null ? null : (
                    <span className="remote-allow-kind">
                      {t("settings.areas.feedsOther", { name: held.name })}
                    </span>
                  )}
                </label>
              );
            })}
          </div>
        )}
        <p className="field-hint">{t("settings.areas.feedExclusive")}</p>
      </fieldset>

      {/* ---- auto-archives after ---- */}
      <fieldset className="menu-group">
        <legend>{t("settings.areas.drawer")}</legend>
        <div className="field-inline">
          <select
            className="select"
            value={periodKey(period)}
            disabled={busy}
            aria-label={t("settings.areas.drawer")}
            onChange={(e) => {
              const v = e.target.value;
              if (v === "never") return setPeriod({ kind: "never" });
              if (v === "default") return setPeriod({ kind: "default" });
              if (v === "custom") {
                setCustom(String(period.kind === "days" ? period.days : DRAWER_DEFAULT_DAYS));
                return;
              }
              return setPeriod({ kind: "days", days: Number(v) });
            }}
          >
            <option value="never">{t("settings.areas.never")}</option>
            {DRAWER_PERIOD_PRESETS.map((n) => (
              <option key={n} value={String(n)}>
                {tCount("settings.areas.days", n)}
              </option>
            ))}
            {/* The DEFAULT option is not the same as picking its number: this one
                follows the login default and moves when it moves, and choosing "30"
                pins 30 forever. Labelled with the number when it is known, without
                one when it is not. */}
            <option value="default">
              {prefs === null
                ? t("settings.areas.defaultOptionUnread")
                : t("settings.areas.defaultOption", { n: prefs.drawerDefaultDays })}
            </option>
            <option value="custom">{t("settings.areas.custom")}</option>
          </select>
          {/* ⚠ THE NUMBER IS SHOWN WHENEVER THE PERIOD IS NOT A PRESET, not only
              while the user is editing it. An area pinned at 21 days selects
              "Custom…" in the list above, and without this it would be the only
              state on this page where the current value cannot be READ — the whole
              point of stating the value before the options. */}
          {periodKey(period) === "custom" || custom !== "" ? (
            <>
              <input
                type="number"
                className="text-input setting-days-input"
                min={1}
                value={custom === "" ? String(period.kind === "days" ? period.days : "") : custom}
                disabled={busy}
                aria-label={t("settings.areas.custom")}
                onChange={(e) => setCustom(e.target.value)}
              />
              <button
                type="button"
                className="button button-quiet"
                // Nothing to apply until the number differs from the stored one, so
                // the button cannot be a click that spends a round trip to be told
                // nothing changed.
                disabled={
                  busy ||
                  !Number.isInteger(Number(custom)) ||
                  Number(custom) <= 0 ||
                  (period.kind === "days" && Number(custom) === period.days)
                }
                onClick={() => {
                  const n = Number(custom);
                  if (!Number.isFinite(n) || n <= 0) return;
                  setCustom("");
                  setPeriod({ kind: "days", days: Math.floor(n) });
                }}
              >
                {t("settings.areas.apply")}
              </button>
            </>
          ) : null}
          {period.kind === "default" ? null : (
            <button
              type="button"
              className="link-button"
              disabled={busy}
              onClick={() => setPeriod({ kind: "default" })}
            >
              {t("settings.areas.resetToDefault")}
            </button>
          )}
        </div>
        {/* ⚠ THE SWEEP-OFF CLAUSE IS FIRST when it applies, because it makes every
            other word in this fieldset conditional: an area set to auto-archive
            after 30 days under a switch that is off archives nothing, ever. */}
        {sweepOn ? null : (
          <p className="menu-note setting-sweep-off">{t("desk.drawer.sweepOff")}</p>
        )}
        <p className="field-hint">
          {archived === null
            ? // Omitted, not zeroed: no desk summary means nobody has counted, and
              // "archived 0 so far" is the #176 shape — a claim about an unread
              // number that reads exactly like a real finding.
              t("settings.areas.drawerPromise")
            : t("settings.areas.drawerPromiseCounted", {
                counted: tCount("settings.areas.archivedSoFar", archived),
              })}
        </p>
      </fieldset>

      <label className="checkbox">
        <input
          type="checkbox"
          checked={area.hiddenFromRail}
          disabled={busy}
          onChange={() => write({ hiddenFromRail: !area.hiddenFromRail })}
        />
        {t("settings.areas.hideFromRail")}
      </label>
      <p className="field-hint">{t("settings.areas.hideFromRail.explain")}</p>

      <div className="setting-more">
        <button
          type="button"
          className="link-button setting-more-button"
          aria-expanded={moreOpen}
          onClick={() => setMoreOpen((v) => !v)}
        >
          {moreOpen ? t("settings.less") : t("settings.more")}
        </button>
        {moreOpen ? (
          <div className="setting-more-body">
            <fieldset className="menu-group">
              <legend>{t("settings.areas.rulesHere")}</legend>
              {/* DERIVED, not stored: nothing on a Rule says "feeds this area". The
                  area's own label IS the placement, so a rule whose addLabel names
                  that label puts mail here — settings/areaFeeds.ts rulesForArea
                  matches all three forms the target can take. */}
              {rules === null ? (
                <p className="field-hint">{t("settings.areas.rulesUnread")}</p>
              ) : (
                (() => {
                  const here = rulesForArea(rules, area, ownLabel);
                  if (here.length === 0) {
                    return <p className="field-hint">{t("settings.areas.rulesNone")}</p>;
                  }
                  return (
                    <ul className="remote-allow-list">
                      {here.map((rule) => (
                        <li key={rule.id} className="remote-allow">
                          <span>{rule.name}</span>
                          {rule.enabled ? null : (
                            <span className="remote-allow-kind is-warn">
                              {t("settings.areas.ruleDisabled")}
                            </span>
                          )}
                        </li>
                      ))}
                    </ul>
                  );
                })()
              )}
              <button
                type="button"
                className="link-button"
                onClick={() => go({ page: "settings", section: "rules" })}
              >
                {t("settings.account.rules.open")}
              </button>
            </fieldset>

            <button
              type="button"
              className="button button-danger menu-action"
              onClick={() => setConfirming(true)}
            >
              {t("settings.areas.delete")}
            </button>
          </div>
        ) : null}
      </div>

      {confirming ? (
        <ConfirmDialog
          title={t("settings.areas.delete.title", { name: area.name })}
          // The SERVER's semantics: the area's own label is immutable and survives
          // the destroy on every message that carries it (core/jmap/types.ts Area.
          // labelId). So this deletes a region of the desk and no mail and no label.
          body={t("settings.areas.delete.body")}
          confirmLabel={t("settings.areas.delete.confirm")}
          busy={busy}
          onConfirm={() => {
            setBusy(true);
            void (async () => {
              const res = await actions.destroyArea(area.id);
              setBusy(false);
              if (res.ok) setConfirming(false);
              else setRefusal(res.refusal);
            })();
          }}
          onCancel={() => setConfirming(false)}
        />
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// New area, homeless labels, needs sorting
// ---------------------------------------------------------------------------

function NewArea(): JSX.Element {
  const { actions } = useApp();
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  return (
    <form
      className="setting-new-area"
      onSubmit={(e) => {
        e.preventDefault();
        if (name.trim() === "") return;
        setBusy(true);
        setRefusal(null);
        void (async () => {
          const res = await actions.createArea(name);
          setBusy(false);
          if (res.ok) setName("");
          setRefusal(res.ok ? null : res.refusal);
        })();
      }}
    >
      {refusal === null ? null : (
        <p className="banner banner-warn" role="alert">
          {refusal}
        </p>
      )}
      <div className="field-inline">
        <input
          className="text-input"
          type="text"
          value={name}
          disabled={busy}
          placeholder={t("settings.areas.newPlaceholder")}
          aria-label={t("settings.areas.new")}
          onChange={(e) => setName(e.target.value)}
        />
        <button
          type="submit"
          className="button button-quiet"
          disabled={busy || name.trim() === ""}
        >
          {t("settings.areas.new")}
        </button>
      </div>
      <p className="field-hint">{t("settings.areas.new.explain")}</p>
    </form>
  );
}

function HomelessLabels({ labels, areas }: { labels: Label[]; areas: Area[] }): JSX.Element | null {
  const { actions } = useApp();
  const [busy, setBusy] = useState<Id | null>(null);
  const [refusal, setRefusal] = useState<string | null>(null);

  // ⚠ NOT RENDERED WITHOUT AREAS. With none loaded, EVERY label is "in no area" —
  // arithmetically true and completely misleading, and it would offer to assign
  // them to a list of areas that is empty. SettingsRoute.tsx draws the same line
  // for the state-line clause and says so there too.
  if (areas.length === 0) return null;
  const homeless = labelsInNoArea(labels, areas);
  if (homeless.length === 0) return null;

  const assign = (labelId: Id, area: Area): void => {
    setBusy(labelId);
    setRefusal(null);
    void (async () => {
      // Full arrays, always — the label is APPENDED to whatever the area already
      // had, which is what makes this an assignment rather than a replacement of
      // the target area's whole feed set.
      const res = await actions.updateArea(area.id, {
        ...feedPatch(area, { kind: "addLabel", labelId }),
      });
      setBusy(null);
      setRefusal(res.ok ? null : res.refusal);
    })();
  };

  return (
    <fieldset className="menu-group">
      <legend>{t("settings.areas.homeless")}</legend>
      {refusal === null ? null : (
        <p className="banner banner-warn" role="alert">
          {refusal}
        </p>
      )}
      <p className="field-hint">{t("settings.areas.homeless.explain")}</p>
      <ul className="remote-allow-list">
        {homeless.map((label) => (
          <li key={label.id} className="remote-allow">
            <span>{label.name}</span>
            <select
              className="select"
              value=""
              disabled={busy === label.id}
              aria-label={t("settings.areas.assignTo", { name: label.name })}
              onChange={(e) => {
                const area = areas.find((a) => a.id === e.target.value);
                if (area) assign(label.id, area);
              }}
            >
              <option value="">{t("settings.areas.assignTo.prompt")}</option>
              {areas.map((area) => (
                <option key={area.id} value={area.id}>
                  {area.name}
                </option>
              ))}
            </select>
          </li>
        ))}
      </ul>
    </fieldset>
  );
}

/**
 * DAY-ONE SETUP, FROM THE OTHER END (#446): import one folder now, or run the
 * whole wizard again.
 *
 * These two are HERE rather than on the Data row because they are about the desk:
 * "Leave alone (import later)" in step 2 is only a real answer if "later" has a
 * door, and this is it. The row's own state line is unchanged — neither button is
 * a setting, and a line that counted folders left to import would be a number this
 * row would then have to keep true.
 *
 * ⚠ THE SELECT LISTS EVERY PLAIN FOLDER, NOT ONLY THE UNIMPORTED ONES. #446
 * offers either; this takes the simpler half deliberately, because the alternative
 * is a guess. "Already imported" is not a fact the client holds — the census
 * derives it server-side by matching a label of the same name — so filtering here
 * would mean re-deriving that rule from the store's labels and being confidently
 * wrong about a folder whose label the user renamed. Importing a folder twice is
 * SAFE and nearly free: the label is reused, and `Setup/preview` counts as
 * `archived` only the messages that are not archived already, so the second run
 * writes what the first one missed and nothing else. Special-use folders are
 * excluded because day-one cannot act on them at all (the census has no row for
 * them, and a plan naming one is refused).
 *
 * The folder's ACCOUNT is named beside it when the login has more than one: folder
 * names collide across accounts, and "Receipts" twice in a select is a choice
 * nobody can make (jmap/types.ts Mailbox.accountId says the same).
 */
function DayOneEntry(): JSX.Element {
  const go = useKeepScope();
  const state = useMailState();
  const { accounts } = useAccounts();
  const [chosen, setChosen] = useState("");

  const folders = Object.values(state.mailboxes)
    .filter((m) => !m.role)
    .sort((a, b) => a.name.localeCompare(b.name));
  const several = accounts.length > 1;

  return (
    <fieldset className="menu-group">
      <legend>{t("settings.areas.dayOne")}</legend>
      <p className="field-hint">{t("settings.areas.dayOne.explain")}</p>
      {folders.length === 0 ? (
        // Not "you have no folders": the mailboxes are reference data loaded with
        // the session, so an empty list before that lands is a load in progress —
        // and a login whose provider does no folder sorting genuinely has none,
        // which is the same sentence either way.
        <p className="field-hint">{t("settings.areas.import.none")}</p>
      ) : (
        <div className="field-inline">
          <select
            className="select"
            value={chosen}
            aria-label={t("settings.areas.import.pick")}
            onChange={(e) => setChosen(e.target.value)}
          >
            <option value="">{t("settings.areas.import.prompt")}</option>
            {folders.map((folder) => (
              <option key={folder.id} value={folder.id}>
                {several
                  ? t("settings.areas.import.inAccount", {
                      folder: folder.name,
                      account:
                        accounts.find((a) => a.id === folder.accountId)?.name ?? folder.accountId,
                    })
                  : folder.name}
              </option>
            ))}
          </select>
          <button
            type="button"
            className="button button-quiet"
            disabled={chosen === ""}
            onClick={() => go({ page: "setup", step: 2, importMailboxId: chosen })}
          >
            {t("settings.areas.import")}
          </button>
        </div>
      )}
      <button
        type="button"
        className="link-button"
        onClick={() => go({ page: "setup", step: 1 })}
      >
        {t("settings.areas.runSetup")}
      </button>
    </fieldset>
  );
}

// ---------------------------------------------------------------------------
// The row
// ---------------------------------------------------------------------------

export function AreasBody({
  rules,
  areaId,
}: {
  rules: Rule[] | null;
  /**
   * The area `#/settings/areas/<id>` names, or null.
   *
   * ⚠ IT MARKS A CARD, IT DOES NOT FILTER TO ONE. Increment 2 taught the router to
   * parse and format this segment and nothing consumed it, so the URL existed and
   * meant nothing. Filtering would have been the other reading, and it is the wrong
   * one here: the cards are where feeds are moved BETWEEN areas, and a screen
   * showing one area cannot show the area a refusal names. So a deep link scrolls
   * its card into view and marks it as current; everything else stays on screen.
   */
  areaId: Id | null;
}): JSX.Element {
  const { actions } = useApp();
  const go = useKeepScope();
  const state = useMailState();
  const prefs = state.deskPreferences;
  // The same total order the desk and the rail use (core/desk.ts deskSections,
  // Sidebar.tsx) and the same one the store applies (`ORDER BY a.position, a.id`).
  // `position` has no uniqueness constraint, so the id tiebreak is what keeps this
  // row from listing two areas in a different order than the desk draws them.
  const areas = Object.values(state.areas).sort((a, b) =>
    a.position !== b.position ? a.position - b.position : a.id.localeCompare(b.id),
  );
  const labels = Object.values(state.labels).sort((a, b) => a.name.localeCompare(b.name));
  const summary = state.deskSummary;
  const unsorted = summary?.rows.find((r) => r.areaId === null) ?? null;

  return (
    <>
      <LoginDefault prefs={prefs} />
      <LearnedPlacementToggle />

      {/* The row's own sweep-off line, said ONCE at the top as well as on each
          card: a reader who never opens a card still has to know that none of the
          periods below them do anything yet. */}
      {prefs !== null && !prefs.drawerSweepEnabled ? (
        <p className="menu-note setting-sweep-off">{t("settings.areas.sweepOffRow")}</p>
      ) : null}

      {areas.length === 0 ? (
        // NOT "you have no areas": the areas are reference data loaded with the
        // session (core/sync.ts), so an empty map before that lands is a load in
        // progress. A fresh login genuinely has none, and the New area form below
        // is the answer to both.
        <p className="field-hint">{t("settings.areas.noneYet")}</p>
      ) : (
        areas.map((area, i) => (
          <AreaCard
            key={area.id}
            area={area}
            prefs={prefs}
            labels={labels}
            rules={rules}
            otherAreas={areas.filter((a) => a.id !== area.id)}
            archived={summary?.rows.find((r) => r.areaId === area.id)?.archivedByDrawer ?? null}
            first={i === 0}
            last={i === areas.length - 1}
            current={area.id === areaId}
            onMove={(delta) =>
              void actions.reorderAreas(movedByOne(areas, i, delta).map((a) => a.id))
            }
          />
        ))
      )}

      <NewArea />
      <HomelessLabels labels={labels} areas={areas} />

      {/* NEEDS SORTING — the summary's null row, which is mail no area claims. The
          three counts are disjoint (types.ts AreaSummaryRow) so they add up; absent
          summary, absent clause. */}
      {unsorted === null ? null : (
        <fieldset className="menu-group">
          <legend>{t("settings.areas.needsSorting")}</legend>
          <p className="menu-note">
            {tCount(
              "settings.state.needSorting",
              unsorted.surface + unsorted.drawer + unsorted.unscored,
            )}
          </p>
          <button
            type="button"
            className="link-button"
            onClick={() => go({ page: "inbox", tab: null, sort: true })}
          >
            {t("settings.areas.sortNow")}
          </button>
        </fieldset>
      )}

      {/* LAST on the row, because it is the only thing here that leaves it. */}
      <DayOneEntry />
    </>
  );
}
