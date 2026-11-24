// SPDX-License-Identifier: AGPL-3.0-only

// WHO ANSWERS ENTER. The shell owns one window-level keyboard chain, and on a
// list route a plain Enter means "open the cursor row". That is right when focus
// is on the list or on nothing, and wrong when focus is on a control that
// activates itself on Enter: the chain called preventDefault, which cancels the
// browser's own click, so a keyboard user pressing Enter on a focused button in
// the reading pane or the sender card got a no-op (found by the accessibility lane
// on the sender card's "Forget what was learned", #167).
//
// Only Enter is handed back. The letter verbs stay global on purpose: `e` on a
// focused button still archives, as it always has.

const SELF_ACTIVATING_ROLES = new Set(["button", "menuitem", "menuitemcheckbox", "menuitemradio", "switch", "tab"]);

/** True when `target` is a control that clicks itself on Enter. */
export function activatesOnEnter(target: EventTarget | null): boolean {
  const node = target as HTMLElement | null;
  if (!node || typeof node.getAttribute !== "function") return false;
  switch (node.tagName) {
    case "BUTTON":
    case "SUMMARY":
      return true;
    case "A":
      return node.hasAttribute("href");
    default: {
      const role = node.getAttribute("role");
      return role !== null && SELF_ACTIVATING_ROLES.has(role);
    }
  }
}
