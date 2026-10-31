// SPDX-License-Identifier: AGPL-3.0-only

// A search's two #724 modifiers are part of its identity: in the URL, in the
// view key (so the list reloads when one changes), and in what is asked of the
// server — the snippets included.

import { describe, expect, it } from "vitest";

import { formatRoute, parseRoute, routeKey } from "./routes";
import type { Route } from "./routes";
import { planFor, snippetFilter, viewKey } from "./views";
import type { View } from "./views";

const cases: View[] = [
  { kind: "search", query: "from:alice invoice" },
  { kind: "search", query: "invo", prefix: true },
  { kind: "search", query: "invoice", sort: "relevance" },
  { kind: "search", query: "in vo", prefix: true, sort: "relevance" },
];

describe("search routes", () => {
  it("round-trips every combination through the URL", () => {
    for (const view of cases) {
      const r: Route = { page: "view", view };
      expect(parseRoute(formatRoute(r))).toEqual(r);
    }
    expect(formatRoute({ page: "view", view: cases[3] as View })).toBe(
      "#/search/in%20vo?prefix=1&sort=relevance",
    );
  });

  it("keys each combination differently, so changing one reloads the list", () => {
    const keys = new Set(cases.map((v) => viewKey(v)));
    expect(keys.size).toBe(cases.length);
    const routes = new Set(cases.map((v) => routeKey({ page: "view", view: v })));
    expect(routes.size).toBe(cases.length);
  });

  it("ignores a sort it does not know rather than failing the link", () => {
    expect(parseRoute("#/search/x?sort=bogus")).toEqual({ page: "view", view: { kind: "search", query: "x" } });
  });

  it("asks the server for exactly what the view says, snippets included", () => {
    const plan = planFor({ kind: "search", query: "invo", prefix: true, sort: "relevance" }, [], {});
    expect(plan.filters).toEqual([{ query: "invo", queryPrefix: true }]);
    expect(plan.sort).toEqual([{ property: "relevance" }]);
    expect(snippetFilter(plan)).toEqual({ query: "invo", queryPrefix: true });
    const plain = planFor({ kind: "search", query: "invoice" }, [], {});
    expect(plain.filters).toEqual([{ query: "invoice" }]);
    expect(plain.sort).toBeUndefined();
    expect(snippetFilter(plain)).toEqual({ query: "invoice" });
  });
});
