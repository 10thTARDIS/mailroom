// SPDX-License-Identifier: AGPL-3.0-only

// ONE ROW OF THE SETTINGS PAGE (#374 increment 2).
//
// The shape is the same twelve times, which is the point: name, scope, a state
// line of FACTS, and — only when you ask — the controls. A user scanning the page
// reads twelve state lines and knows what their mail client is currently doing,
// without opening anything.
//
// THE HEADING IS THE CONTROL. It is an `<h3>` containing a `<button>`, so the
// page still has a real heading outline for a screen reader and the whole width
// of the heading is the target. `aria-expanded` and `aria-controls` are what make
// it a disclosure rather than a link that happens to change the page.
//
// A FACT THAT IS NOT MEASURED IS NOT A FACT, and this component cannot fix that —
// it renders what it is handed. The rule lives at each call site (SettingsRoute
// builds the facts) and it is worth restating here because it is the difference
// between this page being trustworthy and being decorative: `0` is a claim, and a
// claim about a number nobody has counted yet is the #159/#176 failure. A row
// that has not loaded its facts hands over FEWER facts, never a zero.
//
// ONE `more` PER ROW, CLOSED BY DEFAULT. The rarer controls have to be somewhere,
// and the alternative to one disclosure is either a second page (which is what
// #373's sheets were) or a row that is long enough to hide the eleven other rows.
// Its open state is deliberately LOCAL and not in the URL: `#/settings/<row>`
// names the row, and a URL that also named a disclosure would be a URL that
// changes when you fold something away.

import { useState } from "react";
import type { ReactNode } from "react";
import type { JSX } from "react";

import { t } from "../../i18n";
import { Scope } from "./Scope";
import type { SettingsScope } from "./Scope";

/**
 * One clause of a row's state line.
 *
 * `tone: "warn"` is for a fact that is a PROBLEM — an account whose credentials
 * were rejected, a sweep that is switched off while areas are configured for it.
 * It is a tone on top of words that already say so, never instead of them
 * (spec's WCAG 1.4.1 rule, and the same discipline as .health-dot).
 */
export interface StateFact {
  text: string;
  tone?: "warn";
}

/** The state line as one string — what the search matches on, and what a reader
 * would see if the tones were stripped. Built from the same facts that are
 * rendered, so the two cannot describe different states. */
export function factsText(facts: readonly StateFact[]): string {
  return facts.map((f) => f.text).join(" · ");
}

export function SettingsRow({
  id,
  name,
  scopes,
  facts,
  expanded,
  onToggle,
  children,
  more,
}: {
  /** The row id, which is also its route segment (`#/settings/<id>`). */
  id: string;
  name: string;
  scopes: readonly SettingsScope[];
  facts: readonly StateFact[];
  expanded: boolean;
  onToggle: () => void;
  /** The row's controls. Rendered only while expanded — a collapsed row must not
   * pay for the sheet inside it, and several of these bodies fetch. */
  children?: ReactNode;
  /** The rarer controls, behind this row's one `more`. */
  more?: ReactNode;
}): JSX.Element {
  const [moreOpen, setMoreOpen] = useState(false);
  const bodyId = `setting-body-${id}`;

  return (
    <section className={`setting${expanded ? " is-open" : ""}`} aria-labelledby={`setting-h-${id}`}>
      <h3 className="setting-heading" id={`setting-h-${id}`}>
        <button
          type="button"
          className="setting-heading-button"
          aria-expanded={expanded}
          aria-controls={bodyId}
          onClick={onToggle}
        >
          {/* A caret, not a key legend: the desk prints no keys on its surfaces
              either (ui/desk/SelectionToolbar.tsx says why), and the shortcuts
              sheet is the one place they are written down. */}
          <span className="setting-caret" aria-hidden="true">
            {expanded ? "▾" : "▸"}
          </span>
          <span className="setting-name">{name}</span>
        </button>
      </h3>

      <Scope scopes={scopes} />

      {/* The facts. Omitted entirely when there are none — an empty line reads as
          a row that failed rather than a row with nothing to report yet. */}
      {facts.length > 0 ? (
        <p className="setting-state">
          {facts.map((fact, i) => (
            <span key={`${i}-${fact.text}`}>
              {i > 0 ? <span className="setting-sep"> · </span> : null}
              <span className={fact.tone === "warn" ? "setting-fact is-warn" : "setting-fact"}>
                {fact.text}
              </span>
            </span>
          ))}
        </p>
      ) : null}

      <div className="setting-body" id={bodyId} hidden={!expanded}>
        {expanded ? (
          <>
            {children}
            {more ? (
              <div className="setting-more">
                <button
                  type="button"
                  className="link-button setting-more-button"
                  aria-expanded={moreOpen}
                  onClick={() => setMoreOpen((v) => !v)}
                >
                  {moreOpen ? t("settings.less") : t("settings.more")}
                </button>
                {moreOpen ? <div className="setting-more-body">{more}</div> : null}
              </div>
            ) : null}
          </>
        ) : null}
      </div>
    </section>
  );
}
