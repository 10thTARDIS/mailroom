// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE LIST AS A ROUTE MOUNTS IT (#666).
//
// `virtualWindow`'s arithmetic is tested without a DOM in core/desk.test.ts, and
// it was right the whole time this was broken. What was wrong was the WIRING: the
// scroller is the PARENT's element, React attaches a parent's ref after its
// children's layout effects, and the effect that adds the scroll listener read
// `scrollRef.current`, found null, and returned — once, for good. So the window
// stayed wherever the pane was scrolled the last time the item count changed, and
// everything past it was spacer with nothing in it.
//
// That is a fact about effect ordering, which no DOM-free test can see. Hence
// jsdom, for this file only.
//
// ⚠ jsdom HAS NO LAYOUT. Every measurement the component takes is stubbed below,
// so this proves which rows are in the document and what is listening — not a
// pixel of CSS.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { useRef } from "react";
import type { JSX } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { VirtualList } from "./VirtualList";

const ROW_H = 68;
const CHROME_H = 160;
const VIEWPORT = 640;

interface Item {
  kind: string;
  key: string;
}

const rows = (n: number): Item[] =>
  Array.from({ length: n }, (_, i) => ({ kind: "row", key: `k${i}` }));

let scrollTop = 0;
/** What the row-height probe measures. Mutable, because a re-measured row height
 * is exactly what the scroll pull-back was made of. */
let probeH = ROW_H;
/** The pane is `display: none` (a message is open below 1100px, styles.css): a
 * browser then measures every box in it as zero and reads its scrollTop as 0. */
let hidden = false;
let observed: Element[] = [];
let restore: (() => void)[] = [];

function stubGeometry(): void {
  const proto = HTMLElement.prototype;
  const saved = {
    clientHeight: Object.getOwnPropertyDescriptor(proto, "clientHeight"),
    scrollTop: Object.getOwnPropertyDescriptor(proto, "scrollTop"),
    rect: proto.getBoundingClientRect,
    observer: globalThis.ResizeObserver,
  };
  const isPane = (el: HTMLElement): boolean => el.classList.contains("list-pane");
  Object.defineProperty(proto, "clientHeight", {
    configurable: true,
    get(this: HTMLElement) {
      return isPane(this) && !hidden ? VIEWPORT : 0;
    },
  });
  Object.defineProperty(proto, "scrollTop", {
    configurable: true,
    get(this: HTMLElement) {
      return isPane(this) && !hidden ? scrollTop : 0;
    },
    set(this: HTMLElement, value: number) {
      if (!isPane(this)) return;
      scrollTop = Math.max(0, value);
      // What a browser does and jsdom does not: tell whoever is listening.
      this.dispatchEvent(new Event("scroll"));
    },
  });
  proto.getBoundingClientRect = function (this: HTMLElement): DOMRect {
    const rect = (top: number, height: number): DOMRect =>
      ({ top, height, bottom: top + height, left: 0, right: 0, width: 0, x: 0, y: top }) as DOMRect;
    if (hidden) return rect(0, 0);
    if (isPane(this)) return rect(0, VIEWPORT);
    if (this.classList.contains("desk-probe")) return rect(0, probeH);
    // The list sits under the sticky chrome and moves up as the pane scrolls.
    if (this.classList.contains("desk-list")) return rect(CHROME_H - scrollTop, 0);
    return rect(0, 0);
  };
  globalThis.ResizeObserver = class {
    observe(el: Element): void {
      observed.push(el);
    }
    unobserve(): void {}
    disconnect(): void {}
  };
  restore.push(() => {
    if (saved.clientHeight) Object.defineProperty(proto, "clientHeight", saved.clientHeight);
    if (saved.scrollTop) Object.defineProperty(proto, "scrollTop", saved.scrollTop);
    proto.getBoundingClientRect = saved.rect;
    globalThis.ResizeObserver = saved.observer;
  });
}

/** The shape DeskRoute and TrayRoute both have, and the whole point of the test:
 * the scroller's ref is on the list's PARENT, mounted in the same commit. */
function Pane({ items, cursor = null }: { items: Item[]; cursor?: string | null }): JSX.Element {
  const paneRef = useRef<HTMLElement | null>(null);
  const chromeRef = useRef<HTMLDivElement | null>(null);
  return (
    <section className="list-pane" ref={paneRef}>
      <div className="desk-chrome" ref={chromeRef} />
      <VirtualList
        items={items}
        scrollRef={paneRef}
        chromeRef={chromeRef}
        cursorIndex={cursor === null ? -1 : items.findIndex((i) => i.key === cursor)}
        renderItem={(item) => item.key}
        itemKey={(item) => item.key}
        rowAria={() => ({})}
        keys={{ move() {}, edge() {}, open() {}, select() {} }}
        gridId="grid"
        label="list"
      />
    </section>
  );
}

/** The indices of the rows that are actually in the document. */
function drawn(): number[] {
  return [...document.querySelectorAll("li[role=row]")].map(
    (li) => Number(li.getAttribute("aria-rowindex")) - 1,
  );
}

/** Long enough for the passive effects and the one-per-frame measurement. */
const settle = (): Promise<void> =>
  new Promise((resolve) => requestAnimationFrame(() => setTimeout(resolve, 0)));

describe("VirtualList inside a parent-owned scroller", () => {
  let root: Root;

  beforeEach(() => {
    scrollTop = 0;
    probeH = ROW_H;
    hidden = false;
    observed = [];
    restore = [];
    // Built with DOM calls, not an HTML string: cmd/mailroom's security test
    // refuses every HTML-string sink under web/src, tests included.
    const app = document.createElement("div");
    document.body.replaceChildren(app);
    stubGeometry();
    root = createRoot(app);
  });

  afterEach(() => {
    flushSync(() => root.unmount());
    for (const undo of restore) undo();
  });

  it("draws the rows the pane was scrolled to", async () => {
    flushSync(() => root.render(<Pane items={rows(600)} />));
    await settle();
    // The first window: the viewport's rows plus the overscan, from the top.
    expect(drawn()[0]).toBe(0);
    expect(drawn().length).toBeLessThan(30);

    const pane = document.querySelector(".list-pane") as HTMLElement;
    pane.scrollTop = 3000;
    await settle();

    // (3000 - 160) / 68 is row 41: it must be in the document, and the rows the
    // pane has left behind must not be. On the unfixed component this window is
    // still 0..16 and row 41 is 3,000 pixels of empty spacer.
    const after = drawn();
    expect(after).toContain(41);
    expect(after).not.toContain(0);
  });

  // A MESSAGE OPENED AND CLOSED BELOW 1100px (#673). The reading pane replaces
  // the list there: `.list-pane` goes `display: none`, every box in it measures
  // zero, and the list used to take that for "not measured yet" and fall back to
  // ordinary flow — forty rows and no spacer. The pane then had nothing to scroll
  // to, so when it came back the browser clamped the scroll offset it had kept to
  // that short height: scrolled to row 70, closed a message, landed on row 23.
  // (Seen in Chromium at 360px, scrolled to 5000px: back at 1930px.)
  it("keeps its window and its full height while the pane is hidden", async () => {
    flushSync(() => root.render(<Pane items={rows(600)} />));
    await settle();
    const pane = document.querySelector(".list-pane") as HTMLElement;
    pane.scrollTop = 3000;
    await settle();
    const grid = document.querySelector("ul[role=grid]") as HTMLElement;
    const height = grid.style.height;
    expect(height).toBe(`${600 * ROW_H}px`);
    expect(grid.className).toContain("is-virtual");

    // The message opens: the pane is hidden and the zero measurements arrive.
    hidden = true;
    pane.dispatchEvent(new Event("scroll"));
    await settle();
    await settle();
    expect(grid.className).toContain("is-virtual");
    expect(grid.style.height).toBe(height);

    // It closes again, the browser puts the offset back, and the window is
    // where the user left it.
    hidden = false;
    scrollTop = 3000;
    pane.dispatchEvent(new Event("scroll"));
    await settle();
    expect(drawn()).toContain(41);
    expect(grid.style.height).toBe(height);
  });

  it("watches the scroller, the row-height probe and the chrome for a resize", async () => {
    flushSync(() => root.render(<Pane items={rows(40)} />));
    await settle();
    const classes = observed.map((el) => el.className);
    expect(classes).toContain("list-pane");
    expect(classes).toContain("desk-probe");
    expect(classes).toContain("desk-chrome");
  });

  // THE CURSOR'S IDENTITY, NOT ITS INDEX (#673). The route keeps the cursor as an
  // id and derives the index each render; the scroll-into-view effect used to key
  // on that index, so rows inserted ABOVE the cursor moved it and scrolled the
  // pane to a row nobody had asked to go to.
  describe("scrolling the cursor into view", () => {
    const withSection = (above: number): Item[] => [
      ...Array.from({ length: above }, (_, i) => ({ kind: "row", key: `a${i}` })),
      ...rows(100),
    ];

    it("does not scroll when rows are inserted above the cursor", async () => {
      flushSync(() => root.render(<Pane items={withSection(0)} cursor="k50" />));
      await settle();
      const pane = document.querySelector(".list-pane") as HTMLElement;
      // Put the pane where the user left it, with the cursor row on screen.
      pane.scrollTop = CHROME_H + 50 * ROW_H - 100;
      await settle();
      const before = scrollTop;

      // Unfold a section above: the cursor is still k50, now at index 70.
      flushSync(() => root.render(<Pane items={withSection(20)} cursor="k50" />));
      await settle();

      expect(scrollTop).toBe(before);
    });

    it("scrolls when the cursor moves to a row outside the view", async () => {
      flushSync(() => root.render(<Pane items={rows(100)} cursor="k0" />));
      await settle();
      expect(scrollTop).toBe(0);

      flushSync(() => root.render(<Pane items={rows(100)} cursor="k50" />));
      await settle();

      // Bottom of row 50 at the bottom edge of the viewport.
      expect(scrollTop).toBe(CHROME_H + 51 * ROW_H - VIEWPORT);
      expect(drawn()).toContain(50);
    });

    // #685. The desk's jump on archive-and-advance was the route moving the
    // cursor twice, not this effect's arithmetic — these pin the arithmetic
    // down so that stays true: a cursor that lands on a row already on screen
    // scrolls nothing, and one that lands off screen scrolls the least it can.
    it("does not scroll when the cursor's row leaves and the next one, on screen, takes it", async () => {
      flushSync(() => root.render(<Pane items={rows(100)} cursor="k40" />));
      await settle();
      const pane = document.querySelector(".list-pane") as HTMLElement;
      // Rows 38.. on screen under the chrome; the cursor row and the one after
      // it well inside.
      pane.scrollTop = CHROME_H + 38 * ROW_H - CHROME_H;
      await settle();
      const before = scrollTop;

      // k40 is archived: gone from the items, and the cursor is on k41 — which
      // now sits at index 40, exactly where k40 was.
      const without = rows(100).filter((r) => r.key !== "k40");
      flushSync(() => root.render(<Pane items={without} cursor="k41" />));
      await settle();

      expect(scrollTop).toBe(before);
    });

    // THE SCROLL PULL-BACK. The report: "I can't scroll up or down on the list
    // past the point where that email wouldn't be visible anymore." The effect
    // depended on the measured row height as well as the cursor, and the list is
    // measured on every scroll frame — so any change in that number, without the
    // cursor moving, put the cursor back on screen. The browser half of this is
    // scripts/a11y-check.cjs's "scroll away from the cursor" step.
    describe("never because the user scrolled", () => {
      const scrollAway = async (to: number): Promise<HTMLElement> => {
        const pane = document.querySelector(".list-pane") as HTMLElement;
        pane.scrollTop = to;
        await settle();
        return pane;
      };

      it("leaves the pane where the user scrolled it when the row height wobbles", async () => {
        flushSync(() => root.render(<Pane items={rows(200)} cursor="k1" />));
        await settle();
        const pane = await scrollAway(3000);
        expect(scrollTop).toBe(3000);

        // A sub-pixel difference in the measured height from one scroll position
        // to the next — the last bits of a converted, scrolled rect. Each scroll
        // re-measures; the cursor never moved.
        for (const [i, wobble] of [2e-5, 0, 1e-5, 0].entries()) {
          probeH = ROW_H + wobble;
          pane.scrollTop = 3000 + (i + 1) * 40;
          await settle();
          expect(scrollTop).toBe(3000 + (i + 1) * 40);
        }
      });

      it("leaves the pane where it is when the density changes the row height", async () => {
        flushSync(() => root.render(<Pane items={rows(200)} cursor="k1" />));
        await settle();
        const pane = await scrollAway(3000);

        probeH = 46;
        pane.dispatchEvent(new Event("scroll"));
        // Two: the measurement lands on a frame, and the effect it re-runs after
        // the render that frame scheduled. One settle observed nothing either way.
        await settle();
        await settle();

        expect(scrollTop).toBe(3000);
        // ...and the window is the new height's window, so the measurement was
        // actually taken: (3000 - 160) / 46 is row 61.
        expect(drawn()).toContain(61);
      });

      it("does not scroll when the cursor leaves the list for a moment and comes back", async () => {
        flushSync(() => root.render(<Pane items={rows(200)} cursor="k1" />));
        await settle();
        await scrollAway(3000);

        flushSync(() => root.render(<Pane items={rows(200)} cursor={null} />));
        await settle();
        flushSync(() => root.render(<Pane items={rows(200)} cursor="k1" />));
        await settle();

        expect(scrollTop).toBe(3000);
      });

      it("still follows a cursor that moved after the user scrolled away", async () => {
        flushSync(() => root.render(<Pane items={rows(200)} cursor="k1" />));
        await settle();
        await scrollAway(3000);

        flushSync(() => root.render(<Pane items={rows(200)} cursor="k2" />));
        await settle();

        // k2's top just under the chrome.
        expect(scrollTop + CHROME_H).toBe(CHROME_H + 2 * ROW_H);
      });

      it("follows a cursor that moved before the first measurement, once it is measured", async () => {
        // Nothing measured: the probe has no height yet.
        probeH = 0;
        flushSync(() => root.render(<Pane items={rows(200)} cursor="k50" />));
        await settle();
        expect(scrollTop).toBe(0);

        probeH = ROW_H;
        const pane = document.querySelector(".list-pane") as HTMLElement;
        pane.dispatchEvent(new Event("scroll"));
        await settle();
        await settle();

        expect(scrollTop).toBe(CHROME_H + 51 * ROW_H - VIEWPORT);
      });
    });

    it("scrolls up only as far as the row, its top just under the chrome", async () => {
      flushSync(() => root.render(<Pane items={rows(100)} cursor="k50" />));
      await settle();
      const pane = document.querySelector(".list-pane") as HTMLElement;
      pane.scrollTop = CHROME_H + 50 * ROW_H - CHROME_H;
      await settle();

      flushSync(() => root.render(<Pane items={rows(100)} cursor="k45" />));
      await settle();

      // Row 45's top at the bottom of the sticky chrome: no further.
      expect(scrollTop + CHROME_H).toBe(CHROME_H + 45 * ROW_H);
    });
  });
});
