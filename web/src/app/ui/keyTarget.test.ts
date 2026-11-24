// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

import { describe, expect, it } from "vitest";

import { activatesOnEnter } from "./keyTarget";

const el = (html: string): Element => {
  const d = document.createElement("div");
  d.innerHTML = html;
  return d.firstElementChild as Element;
};

describe("activatesOnEnter", () => {
  it("is true for a button, a summary, a link with an href, and the self-activating roles", () => {
    expect(activatesOnEnter(el("<button>Forget what was learned</button>"))).toBe(true);
    expect(activatesOnEnter(el("<summary>Compare the keys</summary>"))).toBe(true);
    expect(activatesOnEnter(el('<a href="#/all">x</a>'))).toBe(true);
    expect(activatesOnEnter(el('<span role="button" tabindex="0">x</span>'))).toBe(true);
    expect(activatesOnEnter(el('<li role="menuitem">x</li>'))).toBe(true);
    expect(activatesOnEnter(el('<span role="tab">x</span>'))).toBe(true);
  });

  it("is false for the list, for nothing, and for things that do not click themselves", () => {
    // The grid container is not a button: Enter on it must still open the row.
    expect(activatesOnEnter(el('<ul role="grid" tabindex="0"></ul>'))).toBe(false);
    expect(activatesOnEnter(el("<a>no href</a>"))).toBe(false);
    expect(activatesOnEnter(el("<div>text</div>"))).toBe(false);
    expect(activatesOnEnter(null)).toBe(false);
    expect(activatesOnEnter(window)).toBe(false);
  });
});
