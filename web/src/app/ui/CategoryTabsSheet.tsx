// SPDX-License-Identifier: AGPL-3.0-only

// ARRANGING THE CATEGORY TABS (#228) — the settings surface for the strip and
// for the label→tab mapping.
//
// BOTH HALVES LIVE IN ONE SHEET BECAUSE THEY ARE ONE QUESTION. "Which tabs do I
// want?" and "what goes in them?" are answered together or not at all: turning
// Forums on is pointless without a way to send anything there, and pointing a
// label at a tab that is switched off puts its mail in Primary. Splitting them
// across two panels would let a user do half of it and see nothing change.
//
// Until this shipped, `mailroom-admin map-label-tabs` was the ONLY way to change
// the mapping — an operator command for a per-user setting, which is a feature
// only its author can use.
//
// IT IS A ROW BODY NOW, NOT A SHEET (#374 increment 2), and specifically it is
// the `more` of the settings page's "Areas and drawers" row. Categories are one
// of the three things that feed an area (Area.tabs), so this is where they
// belong; it sits behind `more` because arranging the strip is a once-a-year job
// and the row's own controls are not. `#/settings/category-tabs` still opens that
// row (core/routes.ts LEGACY_SECTIONS), so no link to this surface was lost. The
// dialog furniture (backdrop, ✕, its own Escape handler) is gone.
//
// WHAT THIS SURFACE WILL NOT DO, deliberately: claim a message count per tab. The
// client holds labels, not a tab histogram, so it says "3 labels feed this",
// which is a fact about the mapping. "3 messages" would be a claim about the
// mailbox nothing here has measured — and it would be wrong anyway, because the
// classifier puts unlabelled mail in tabs no label feeds.

import { useMemo, useState } from "react";
import type { JSX } from "react";

import { canDisable, labelsPerTab, moveTab, stripOrder } from "../../core/categoryTabs";
import type { CategoryTab, Id } from "../../core/jmap/types";
import { useApp, useMailState } from "../state";
import { t } from "../i18n";

export function CategoryTabsBody(): JSX.Element {
  const { actions } = useApp();
  const { categoryTabs, labels } = useMailState();
  // One in-flight write at a time. Two reorder patches computed from the same
  // starting arrangement and sent together would fight: each renumbers from a
  // strip the other has already moved.
  const [busy, setBusy] = useState(false);

  const ordered = useMemo(() => stripOrder(categoryTabs), [categoryTabs]);
  const labelList = useMemo(
    () => Object.values(labels).sort((a, b) => a.name.localeCompare(b.name)),
    [labels],
  );
  const perTab = useMemo(() => labelsPerTab(labelList), [labelList]);
  const enabledCount = ordered.filter((tb) => tb.enabled).length;

  const tabName = (tab: CategoryTab): string => t(`tabs.${tab}` as Parameters<typeof t>[0]);

  async function run(work: () => Promise<unknown>): Promise<void> {
    if (busy) return;
    setBusy(true);
    try {
      await work();
    } finally {
      setBusy(false);
    }
  }

  function move(id: Id, delta: -1 | 1): void {
    const patch = moveTab(categoryTabs, id, delta);
    if (Object.keys(patch).length === 0) return;
    void run(() => actions.updateCategoryTabs(patch));
  }

  /** What to say beside one tab: why Primary has no switch, or how much of the
   * mapping points at this tab. */
  function noteFor(tab: CategoryTab, enabled: boolean, count: number): string {
    if (!canDisable(tab)) return t("tabs.primaryAlwaysOn");
    const feed =
      count === 0
        ? t("tabs.feed.none")
        : count === 1
          ? t("tabs.feed.one")
          : t("tabs.feed.many", { count });
    // A mapped label pointing at a switched-off tab is not an error — its mail
    // folds into Primary — but it is the kind of thing a user would otherwise
    // have to deduce from a mailbox that looks like it lost something.
    return !enabled && count > 0 ? `${feed} ${t("tabs.feed.whileOff")}` : feed;
  }

  return (
    <>
      <h4 className="sheet-section">{t("tabs.settings")}</h4>
      <p className="field-hint">{t("tabs.explain")}</p>

      {ordered.length === 0 ? (
        <p className="field-hint">{t("tabs.notLoaded")}</p>
      ) : (
        <>
          {/* `.sheet-section` IS the heading style here (SnippetsSheet uses it
              the same way), not a container — small, uppercase, muted. */}
          <h3 className="sheet-section">{t("tabs.strip.heading")}</h3>
          <ul className="tab-settings-list">
            {ordered.map((tb, i) => (
              <li key={tb.id} className="tab-setting">
                <label className="field-inline tab-setting-label">
                  <input
                    type="checkbox"
                    checked={tb.enabled}
                    // Primary's is SHOWN BUT INERT, with the sentence beside
                    // it saying why. The state ("it is on") is worth
                    // rendering; the affordance is not. A live control here
                    // would invite a click, spend a round trip and come back
                    // with an `invalidProperties` the user could not have
                    // avoided (canDisable, and the CHECK behind it).
                    disabled={busy || !canDisable(tb.tab)}
                    onChange={(e) => {
                      // Read the value NOW, not inside the async work. The
                      // input is controlled, so by the time the deferred
                      // closure ran, `e.target.checked` would be whatever
                      // React last rendered — which is the state we are
                      // trying to change.
                      const enabled = e.target.checked;
                      void run(() => actions.updateCategoryTabs({ [tb.id]: { enabled } }));
                    }}
                  />
                  <span className="field-label">{tabName(tb.tab)}</span>
                </label>

                <span className="tab-setting-note">
                  {noteFor(tb.tab, tb.enabled, perTab[tb.tab] ?? 0)}
                </span>

                <span className="tab-setting-move">
                  <button
                    type="button"
                    className="button-quiet"
                    aria-label={t("tabs.moveUp", { tab: tabName(tb.tab) })}
                    disabled={busy || i === 0}
                    onClick={() => move(tb.id, -1)}
                  >
                    ↑
                  </button>
                  <button
                    type="button"
                    className="button-quiet"
                    aria-label={t("tabs.moveDown", { tab: tabName(tb.tab) })}
                    disabled={busy || i === ordered.length - 1}
                    onClick={() => move(tb.id, 1)}
                  >
                    ↓
                  </button>
                </span>
              </li>
            ))}
          </ul>
          {/* Derived from the state rather than warned about in advance: the
              strip needs two tabs to be a choice, so below that it does not
              render at all — and a setting whose effect is invisible reads as
              broken unless something says why. */}
          {enabledCount < 2 ? <p className="field-hint">{t("tabs.stripHidden")}</p> : null}

          <h3 className="sheet-section">{t("tabs.mapping.heading")}</h3>
          <p className="field-hint">{t("tabs.mapping.explain")}</p>
          {labelList.length === 0 ? (
            <p className="field-hint">{t("tabs.mapping.noLabels")}</p>
          ) : (
            <ul className="tab-settings-list">
              {labelList.map((l) => (
                <li key={l.id} className="tab-setting">
                  {/* title as well as the visible text: a name long enough to
                      ellipsise is exactly the one you need to read in full
                      before choosing its category. */}
                  <span className="tab-setting-label" title={l.name}>
                    {l.name}
                  </span>
                  <select
                    className="select"
                    value={l.tab ?? ""}
                    disabled={busy}
                    aria-label={t("tabs.mapping.forLabel", { label: l.name })}
                    onChange={(e) => {
                      const picked = e.target.value === "" ? null : e.target.value;
                      void run(() => actions.setLabelTab(l.id, picked));
                    }}
                  >
                    <option value="">{t("tabs.mapping.auto")}</option>
                    {/* The user's own strip order, and only the tabs the
                        server actually has — a sixth tab would appear here
                        without this list being edited. */}
                    {ordered.map((tb) => (
                      <option key={tb.id} value={tb.tab}>
                        {tb.enabled
                          ? tabName(tb.tab)
                          : t("tabs.mapping.offOption", { tab: tabName(tb.tab) })}
                      </option>
                    ))}
                  </select>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </>
  );
}
