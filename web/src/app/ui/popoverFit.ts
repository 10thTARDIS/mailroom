// SPDX-License-Identifier: AGPL-3.0-only

// ONE RULE FOR EVERY ANCHORED POP-UP: STAY ON THE SCREEN (#880).
//
// A menu was seen cut off on a phone: the desk's Re-file menu with
// several messages selected. On a phone the selection toolbar is docked to the
// BOTTOM of the screen (desk.css), and the menu hangs from `top: 100% + 4px` of
// its button, so it opened entirely below the visible screen. The reading pane's
// More menu had been fixed for exactly that by a one-off CSS rule (#798); the
// Re-file menu, the search suggestions, the recipient suggestions and the
// key-chip panel each had the same latent fault and no rule.
//
// Rather than a fourth patch, every anchored pop-up is fitted by this one
// function: if it does not fit where its stylesheet put it, it FLIPS to the other
// side of its anchor when there is more room there, is CAPPED to the room that
// exists (and scrolls inside itself, so its last item stays reachable), and is
// SHIFTED sideways to stay inside the screen. The stylesheet stays the source of
// the default position; this only ever overrides it, with inline styles it
// removes again before it measures.
//
// It touches STYLE ONLY: no DOM is added, moved or re-ordered, focus is never
// moved, and no attribute a screen reader reads is written, so keyboard and aria
// behaviour are exactly the component's own.
//
// WHY A MutationObserver AND A SELECTOR LIST, NOT A HOOK IN EACH MENU: the next
// menu someone adds is covered by adding one selector here (and the PHONEMENUS
// step in scripts/a11y-check.cjs, which fails on any menu it opens that is not on
// the screen), instead of remembering to call a hook. The fit runs in the
// observer's microtask, i.e. before the browser paints the new menu and before a
// component's passive effect focuses its first item (a focus() on an off-screen
// item scrolls the page).

/** The anchored pop-ups this rule covers. Dialogs (`.sheet`, `.palette`) are
 * centred and capped by their own CSS and are not anchored to anything. */
export const FITTED_POPOVERS: readonly string[] = [
  ".pane-menu",
  ".desk-refile-menu",
  ".search-suggestions",
  ".search-save",
  ".recipient-suggestions",
  ".compose-chip-panel",
];

/** Space kept between a pop-up and the screen's edge, in px. */
export const EDGE_MARGIN = 8;
/** Space kept between a pop-up and the control it hangs from, in px. */
export const ANCHOR_GAP = 4;

export interface Box {
  top: number;
  bottom: number;
  left: number;
  right: number;
}

export interface Fit {
  /** Hang from the anchor's top edge, going up, instead of its bottom edge. */
  flip: boolean;
  /** Cap the height at this many px (null: leave the stylesheet's own cap). */
  maxHeight: number | null;
  /** Cap the width at this many px (null: leave the stylesheet's own cap). */
  maxWidth: number | null;
  /** Sideways shift, px. */
  dx: number;
}

/**
 * The arithmetic, pure. `pop` is the pop-up where its stylesheet put it, `anchor`
 * the control it hangs from, `view` the usable screen (already shrunk by the
 * margin and the safe-area insets). `below` says the stylesheet hangs it below
 * the anchor (the usual case) rather than above.
 */
export function computeFit(pop: Box, anchor: Box, view: Box, below: boolean, gap = ANCHOR_GAP): Fit {
  const h = pop.bottom - pop.top;
  const w = pop.right - pop.left;
  const viewH = view.bottom - view.top;
  const viewW = view.right - view.left;

  let flip = false;
  let maxHeight: number | null = null;
  if (pop.top < view.top || pop.bottom > view.bottom) {
    const roomBelow = view.bottom - (anchor.bottom + gap);
    const roomAbove = anchor.top - gap - view.top;
    // Keep the stylesheet's side if the whole thing fits there; otherwise take
    // the other side if the whole thing fits THERE; otherwise the bigger side,
    // capped, with the menu scrolling inside itself.
    const room = (b: boolean): number => (b ? roomBelow : roomAbove);
    let side = below;
    if (h > room(side) && h <= room(!side)) side = !side;
    else if (h > room(side) && room(!side) > room(side)) side = !side;
    flip = side !== below;
    const r = Math.max(0, Math.min(room(side), viewH));
    if (h > r) maxHeight = Math.floor(r);
  }

  let maxWidth: number | null = null;
  let width = w;
  if (w > viewW) {
    maxWidth = Math.floor(viewW);
    width = viewW;
  }
  // Shift from where the popover's left edge started; a width cap keeps the left
  // edge where it was, so measure the right edge it will have.
  const right = pop.left + width;
  let dx = 0;
  if (right > view.right) dx = view.right - right;
  if (pop.left + dx < view.left) dx = view.left - pop.left;
  return { flip, maxHeight, maxWidth, dx: Math.round(dx) };
}

const MARK = "data-popover-fit";

function insets(): Box {
  // `env(safe-area-inset-*)` is CSS-only; a probe reads it back as padding.
  const probe = document.createElement("div");
  probe.style.cssText =
    "position:fixed;visibility:hidden;pointer-events:none;top:0;left:0;width:0;height:0;" +
    "padding:env(safe-area-inset-top,0px) env(safe-area-inset-right,0px) env(safe-area-inset-bottom,0px) env(safe-area-inset-left,0px)";
  document.body.appendChild(probe);
  const cs = getComputedStyle(probe);
  const out = {
    top: parseFloat(cs.paddingTop) || 0,
    right: parseFloat(cs.paddingRight) || 0,
    bottom: parseFloat(cs.paddingBottom) || 0,
    left: parseFloat(cs.paddingLeft) || 0,
  };
  probe.remove();
  return out;
}

/** The screen the user can see, inside the safe area and the edge margin. */
export function usableScreen(): Box {
  const i = insets();
  const w = document.documentElement.clientWidth || window.innerWidth;
  const h = window.innerHeight;
  let bottom = h - i.bottom - EDGE_MARGIN;
  // THE PHONE'S BOTTOM BAR IS ON SCREEN AND ABOVE ANY MENU: a menu that "fits" in
  // the pixels the bar covers has its last items under it (measured in the lane's
  // 568x320 landscape run, #880: the Re-file menu ended 8px inside the screen and
  // its last item could not be tapped). Room ends at the bar's top edge.
  const bar = document.querySelector<HTMLElement>(".bottom-bar");
  if (bar && bar.getClientRects().length > 0) {
    const r = bar.getBoundingClientRect();
    if (r.top > h / 2) bottom = Math.min(bottom, r.top - EDGE_MARGIN);
  }
  return {
    top: i.top + EDGE_MARGIN,
    left: i.left + EDGE_MARGIN,
    right: w - i.right - EDGE_MARGIN,
    bottom,
  };
}

/**
 * Shrink `view` to what an ancestor that CLIPS (overflow other than visible) lets
 * show. An absolutely positioned menu is clipped by every clipping ancestor from
 * its containing block upward — the sender card's bottom sheet clips its own
 * "Move all to…" menu, the list pane clips the list toolbar's — so fitting to the
 * screen alone leaves a menu "on screen" and still cut off. The document itself
 * is not one of them (it scrolls).
 */
function clipTo(el: HTMLElement, view: Box): Box {
  const out = { ...view };
  for (let a = el.offsetParent as HTMLElement | null; a && a !== document.body && a !== document.documentElement; a = a.parentElement) {
    const cs = getComputedStyle(a);
    if (cs.overflowX === "visible" && cs.overflowY === "visible") {
      if (cs.position === "fixed") break;
      continue;
    }
    const r = a.getBoundingClientRect();
    // The padding box: inside the border, and inside a scrollbar if it has one.
    const left = r.left + a.clientLeft;
    const top = r.top + a.clientTop;
    if (cs.overflowX !== "visible") {
      out.left = Math.max(out.left, left);
      out.right = Math.min(out.right, left + a.clientWidth);
    }
    if (cs.overflowY !== "visible") {
      out.top = Math.max(out.top, top);
      out.bottom = Math.min(out.bottom, top + a.clientHeight);
    }
    // Nothing above a fixed box clips it (the desk's docked selection toolbar).
    if (cs.position === "fixed") break;
  }
  // A clipping ancestor too small to hold a menu (a toolbar with overflow set)
  // is not one a menu can be fitted into; the screen is the best answer left.
  return out.bottom - out.top < 80 || out.right - out.left < 80 ? view : out;
}

function box(r: DOMRect): Box {
  return { top: r.top, bottom: r.bottom, left: r.left, right: r.right };
}

function undo(el: HTMLElement): void {
  if (!el.hasAttribute(MARK)) return;
  const s = el.style;
  s.removeProperty("translate");
  s.removeProperty("max-height");
  s.removeProperty("max-width");
  s.removeProperty("overflow-y");
  s.removeProperty("top");
  s.removeProperty("bottom");
  el.removeAttribute(MARK);
}

/** Fit one pop-up now. Safe to call repeatedly; it measures from the stylesheet's position. */
export function fitPopover(el: HTMLElement): void {
  if (el.hidden) {
    undo(el);
    return;
  }
  const scroll = el.scrollTop;
  undo(el);
  const cs = getComputedStyle(el);
  if (cs.display === "none") return;
  const anchorEl = (el.offsetParent as HTMLElement | null) ?? el.parentElement;
  if (!anchorEl) return;
  const fixed = cs.position === "fixed";
  const view = clipTo(el, usableScreen());
  const pop = box(el.getBoundingClientRect());
  const anchor = box(anchorEl.getBoundingClientRect());
  // "Below" = the stylesheet hangs it under the anchor's bottom edge.
  const below = pop.top >= anchor.bottom - 1 || fixed;

  const fit = computeFit(pop, anchor, view, below);
  if (!fit.flip && fit.maxHeight === null && fit.maxWidth === null && fit.dx === 0) return;

  el.setAttribute(MARK, "");
  if (fit.maxWidth !== null) el.style.setProperty("max-width", fit.maxWidth + "px");
  if (fit.flip && !fixed) {
    // The stylesheet's offset from the anchor, mirrored to the other side.
    const hang = `calc(100% + ${ANCHOR_GAP}px)`;
    el.style.setProperty(below ? "top" : "bottom", "auto");
    el.style.setProperty(below ? "bottom" : "top", hang);
  }
  if (fit.maxHeight !== null) {
    el.style.setProperty("max-height", fit.maxHeight + "px");
    el.style.setProperty("overflow-y", "auto");
  }
  if (fit.dx !== 0) el.style.setProperty("translate", `${fit.dx}px 0`);
  el.scrollTop = scroll;
}

function open(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>(FITTED_POPOVERS.join(",")));
}

/**
 * Start fitting every pop-up that appears, and refit on resize and rotation.
 * Returns the function that stops it (tests; the app never stops it).
 */
export function installPopoverFit(): () => void {
  const selector = FITTED_POPOVERS.join(",");
  const observer = new MutationObserver((records) => {
    const todo = new Set<HTMLElement>();
    for (const rec of records) {
      if (rec.type === "childList") {
        for (const n of rec.addedNodes) {
          if (!(n instanceof HTMLElement)) continue;
          if (n.matches(selector)) todo.add(n);
          n.querySelectorAll<HTMLElement>(selector).forEach((e) => todo.add(e));
        }
      }
      // Content changed inside an open pop-up (a filtered suggestion list), or
      // `hidden` was toggled on one.
      const inside = rec.target instanceof Element ? rec.target.closest<HTMLElement>(selector) : null;
      if (inside) todo.add(inside);
    }
    todo.forEach(fitPopover);
  });
  observer.observe(document.body, { childList: true, subtree: true, attributes: true, attributeFilter: ["hidden"] });
  const refit = (): void => open().forEach(fitPopover);
  window.addEventListener("resize", refit);
  window.addEventListener("orientationchange", refit);
  window.visualViewport?.addEventListener("resize", refit);
  refit();
  return () => {
    observer.disconnect();
    window.removeEventListener("resize", refit);
    window.removeEventListener("orientationchange", refit);
    window.visualViewport?.removeEventListener("resize", refit);
  };
}
