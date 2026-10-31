// SPDX-License-Identifier: AGPL-3.0-only

import type { JSX } from "react";
import type { CategoryTab, CategoryTabState } from "../../core/jmap/types";
import type { View } from "../../core/views";
import { currentTab, tabToView } from "../../core/views";
import { stripOrder } from "../../core/categoryTabs";

/**
 * The Gmail-style category-tab strip (#228).
 *
 * ⚠ IT RENDERS INSIDE `.list-pane`, PASSED IN AS MessageList's `header`, NOT as
 * a child of `.content`. `.content` is a CSS GRID whose columns are the reading
 * pane layout — `list | splitter | thread` — so a third direct child silently
 * becomes grid column 1 and shoves every pane one column across. The first
 * version did exactly that: the strip stretched to the full row height and an
 * opened message rendered where the list should be. Nothing errored; it just
 * laid out wrong.
 *
 * IT ONLY APPEARS OVER THE INBOX. A tab narrows the inbox — a second axis over
 * the same mail, not a folder — so showing it above Archive or Sent would
 * promise a filter that means nothing there.
 *
 * ONLY ENABLED TABS ARE RENDERED, and that is correctness rather than tidiness:
 * the server REFUSES an `inTab` query naming a disabled tab, because answering
 * with an empty list would state the tab has no mail when in fact its mail
 * folded into Primary. The way to guarantee a disabled tab is never clicked is
 * not to draw it.
 */
export function TabStrip({
  tabs,
  view,
  setView,
  label,
  tabLabel,
}: {
  tabs: Record<string, CategoryTabState>;
  view: View;
  setView: (v: View) => void;
  /** Accessible name for the strip itself. */
  label: string;
  /** Human-readable name for one tab. */
  tabLabel: (tab: CategoryTab) => string;
}): JSX.Element | null {
  const overInbox = (view.kind === "role" && view.role === "inbox") || view.kind === "tab";
  if (!overInbox) return null;

  // Ordered by the SHARED comparator, not one of its own: the settings sheet
  // rearranges this list, and a strip that sorted differently would send the
  // user back to a panel whose order it does not honour.
  const enabled = stripOrder(tabs).filter((t) => t.enabled);

  // Nothing to choose between is not a strip. One enabled tab means every
  // message is in it, so the control would be furniture that does nothing.
  if (enabled.length < 2) return null;

  // Null on the plain inbox: that view is not a tab, and highlighting Primary
  // there would claim the list is filtered when it is not.
  const current = currentTab(view);

  return (
    <div className="tab-strip" role="tablist" aria-label={label}>
      {enabled.map((t) => {
        const selected = t.tab === current;
        return (
          <button
            key={t.id}
            type="button"
            role="tab"
            aria-selected={selected}
            className={selected ? "tab-strip-tab is-selected" : "tab-strip-tab"}
            onClick={() => setView(tabToView(t.tab))}
          >
            {tabLabel(t.tab)}
          </button>
        );
      })}
    </div>
  );
}
