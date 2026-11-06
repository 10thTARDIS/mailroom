// SPDX-License-Identifier: AGPL-3.0-only

// jsdom does no layout, so it has no geometry methods on Range. ProseMirror asks
// for them whenever a transaction scrolls the selection into view (any insert that
// calls focus()), and without them it throws from inside an async continuation:
// an uncaught error that lands in whichever test happens to be running. A real
// browser has them; this gives jsdom the answers "no boxes", which is what a
// page with no layout would say.

function noRects(): DOMRectList {
  const list = { length: 0, item: () => null, [Symbol.iterator]: function* () {} };
  return list as unknown as DOMRectList;
}
function zeroRect(): DOMRect {
  return { x: 0, y: 0, width: 0, height: 0, top: 0, right: 0, bottom: 0, left: 0, toJSON: () => ({}) } as DOMRect;
}

const proto = (globalThis as { Range?: { prototype: Range } }).Range?.prototype;
if (proto) {
  if (typeof proto.getClientRects !== "function") proto.getClientRects = noRects;
  if (typeof proto.getBoundingClientRect !== "function") proto.getBoundingClientRect = zeroRect;
}

export {};
