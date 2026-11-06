// SPDX-License-Identifier: AGPL-3.0-only

// Rich paste (ADR-051 §4). ProseMirror has already parsed the clipboard's HTML
// into the editor's schema, in an inert document, before this runs: elements and
// attributes the schema does not define are gone, and so are event handlers and
// disallowed link schemes. What is left to decide here is the one thing the
// schema allows that we do not: a picture that lives somewhere else.

import { Fragment, Slice } from "@tiptap/pm/model";
import type { Node as PMNode } from "@tiptap/pm/model";
import { isInlineImageSrc } from "./uri";

function stripFragment(fragment: Fragment, counter: { dropped: number }): Fragment {
  const kept: PMNode[] = [];
  fragment.forEach((node) => {
    if (node.type.name === "image" && !isInlineImageSrc(node.attrs["src"] as string | null)) {
      counter.dropped += 1;
      return;
    }
    kept.push(node.isLeaf ? node : node.copy(stripFragment(node.content, counter)));
  });
  return Fragment.fromArray(kept);
}

/** The pasted slice without any `<img>` whose source is not cid: or data:image/*,
 * and how many were dropped (so the user can be told once). */
export function dropRemoteImages(slice: Slice): { slice: Slice; dropped: number } {
  const counter = { dropped: 0 };
  const content = stripFragment(slice.content, counter);
  if (counter.dropped === 0) return { slice, dropped: 0 };
  if (content.size === 0) return { slice: Slice.empty, dropped: counter.dropped };
  // Dropping a node can leave fewer levels to be "open" at an edge than before.
  const open = (side: "first" | "last", wanted: number): number => {
    let depth = 0;
    let node = side === "first" ? content.firstChild : content.lastChild;
    while (depth < wanted && node && !node.isLeaf) {
      depth += 1;
      node = side === "first" ? node.firstChild : node.lastChild;
    }
    return depth;
  };
  return {
    slice: new Slice(content, open("first", slice.openStart), open("last", slice.openEnd)),
    dropped: counter.dropped,
  };
}
