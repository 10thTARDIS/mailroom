// SPDX-License-Identifier: AGPL-3.0-only

// axe-core over a jsdom rendering, for the API keys dialogs and cards (#49).
//
// The project's accessibility lane (scripts/a11y-e2e.sh) runs axe in a real
// browser over the built image; this runs the same engine (the exact-pinned
// devDependency) over a component in the unit suite, so a nameless control or
// a broken label reference in these screens fails `npm test` rather than
// waiting for that lane. jsdom has no layout, so the rules that need one
// (colour contrast, target size) are off here and left to the browser lane.

import axe from "axe-core";

export async function axeViolations(root: Element = document.body): Promise<string[]> {
  const result = await axe.run(root, {
    rules: {
      "color-contrast": { enabled: false },
      "color-contrast-enhanced": { enabled: false },
      "target-size": { enabled: false },
      // A fragment rendered into a bare test document has no landmarks or
      // page heading of its own; those belong to the app shell.
      region: { enabled: false },
      "page-has-heading-one": { enabled: false },
      "landmark-one-main": { enabled: false },
    },
  });
  return result.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`);
}
