// SPDX-License-Identifier: AGPL-3.0-only

// THE VIEWER'S BOTTOM BAR (#449): every attachment on the message, which one you
// are on, and the controls that belong to the type you are looking at.
//
// ⚠ NAMED StripBar RATHER THAN Strip, and the reason is not taste: `./strip.ts`
// (the pure half, next door) and a `./Strip.tsx` differ only in casing, and
// TypeScript refuses to resolve two modules that collide that way on a
// case-insensitive filesystem — `forceConsistentCasingInFileNames` fails the build on
// Windows and macOS while passing on Linux. The pure module keeps the plain name,
// because that is where the tested decisions live.
//
// WHAT the strip lists and WHY a held one is marked is decided in ./strip.ts,
// under test; this file only prints it. The two things worth knowing here:
//
//   * `role="list"` IS WRITTEN OUT even though a <ul> has it implicitly. Removing
//     the bullets with `list-style: none` also removes list semantics in WebKit,
//     which is a real and long-standing behaviour rather than a theory — so the
//     count a screen reader announces ("list, 3 items") depends on this attribute
//     being here.
//   * A THUMBNAIL STRIP WITH NO THUMBNAILS IN IT. Rendering one would mean
//     fetching and decoding every attachment on the message the moment the viewer
//     opens — feeding a parser bytes nobody asked to look at, which is exactly the
//     opt-in rule docs/hostile-content.md §2.4a states. The kind word (`jpg`,
//     `pdf`, `zip`) is derived from metadata alone and costs no fetch.

import type { ReactNode } from "react";
import type { JSX } from "react";

import { t } from "../../i18n";
import type { StripModel } from "./strip";

export function ViewerStrip({
  model,
  onSelect,
  onStep,
  controls,
}: {
  model: StripModel;
  onSelect: (index: number) => void;
  /** −1 / +1, wrapping (./strip.ts stepIndex). */
  onStep: (delta: number) => void;
  /** The controls for the type on screen — Fit/100%, Fit width — or nothing for
   *  a type that has none. */
  controls?: ReactNode;
}): JSX.Element {
  return (
    <footer className="viewer-bottombar">
      <ul className="viewer-strip" role="list" aria-label={t("viewer.strip")}>
        {model.items.map((item) => (
          <li key={item.key}>
            <button
              type="button"
              className={`viewer-thumb${item.held ? " is-held" : ""}${item.current ? " is-current" : ""}`}
              // `aria-current` rather than `aria-selected`: this is "the one you
              // are on" in a list, not a selection in a listbox — and the
              // stylesheet's outline is not a cue anyone can hear.
              aria-current={item.current ? "true" : undefined}
              aria-label={item.label}
              onClick={() => onSelect(item.index)}
            >
              <span className="viewer-thumb-kind" aria-hidden="true">
                {item.glyph}
              </span>
              {/* Renders nothing between the two boxes, but keeps the DOM text
                  "png photo.png" rather than "pngphoto.png", which is what the
                  name above has to contain (WCAG 2.5.3, axe 4.14). */}
              {" "}
              <span className="viewer-thumb-name">{item.name}</span>
            </button>
          </li>
        ))}
      </ul>
      <div className="viewer-strip-controls">
        {controls}
        {model.navigable ? (
          <>
            {/* The position is text, not a title on a glyph: "2 of 5" is the answer
                to a question the reader actually has. */}
            <span className="viewer-position">{model.position}</span>
            {/* ARROWS, and this is the one place in the viewer that uses a glyph
                instead of a word: they sit against a horizontally scrolling strip
                where the direction IS the meaning, and each carries the accessible
                name the lightbox already used. The top bar's verbs stay words. */}
            <button
              type="button"
              className="icon-button"
              aria-label={t("attachment.previous")}
              onClick={() => onStep(-1)}
            >
              <span aria-hidden="true">←</span>
            </button>
            <button
              type="button"
              className="icon-button"
              aria-label={t("attachment.next")}
              onClick={() => onStep(1)}
            >
              <span aria-hidden="true">→</span>
            </button>
          </>
        ) : null}
      </div>
    </footer>
  );
}
