// SPDX-License-Identifier: AGPL-3.0-only

import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { PendingChanges } from "./AccountHealthPanel";

const html = (paused: boolean, count: number): string =>
  renderToStaticMarkup(<PendingChanges paused={paused} count={count} />);

describe("PendingChanges", () => {
  it("shows the waiting sentence when sync is paused and changes are queued", () => {
    expect(html(true, 3)).toContain(
      "3 changes are waiting. They will be applied to this mailbox when you turn sync back on.",
    );
  });

  it("uses the singular for one change", () => {
    expect(html(true, 1)).toContain(
      "1 change is waiting. It will be applied to this mailbox when you turn sync back on.",
    );
  });

  it("shows nothing while sync is on, however many are queued", () => {
    expect(html(false, 5)).toBe("");
  });

  it("shows nothing at zero", () => {
    expect(html(true, 0)).toBe("");
  });
});
