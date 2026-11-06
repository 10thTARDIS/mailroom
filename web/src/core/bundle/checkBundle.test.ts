// SPDX-License-Identifier: AGPL-3.0-only

// The editor bundle check (ADR-051 §2). Synthetic chunks, because the real build
// is what `npm run check:bundle` scans; these prove the rules can fail.

import { describe, expect, it } from "vitest";
import { checkBundle } from "./checkBundle";

const editorOk = { name: "RichTextEditor-AbC.js", text: "class ProseMirror { run(){ return 1 } }" };
const main = { name: "index-Xyz.js", text: "console.log('app')" };

describe("checkBundle", () => {
  it("passes a split editor chunk with no dynamic code", () => {
    expect(checkBundle([main, editorOk])).toEqual([]);
  });

  for (const [what, code] of [
    ["eval(", "var x = eval('1')"],
    ["eval( with a space", "var x = eval ('1')"],
    ["new Function", "var f = new Function('return 1')"],
    ["importScripts", "importScripts('x.js')"],
  ] as const) {
    it(`fails on ${what} in the editor chunk`, () => {
      const f = checkBundle([main, { ...editorOk, text: `${editorOk.text};${code}` }]);
      expect(f.join("\n")).toContain("contains");
    });
  }

  it("does not mistake a method called eval or a longer name for the global", () => {
    expect(checkBundle([main, { ...editorOk, text: `${editorOk.text}; a.eval(1); medieval(2); retrieval(3)` }])).toEqual([]);
  });

  it("fails when there is no editor chunk (not code-split)", () => {
    expect(checkBundle([main]).join("\n")).toContain("not code-split");
  });

  it("fails when editor code has been pulled into the main bundle", () => {
    expect(checkBundle([{ ...main, text: "ProseMirror" }, editorOk]).join("\n")).toContain("outside the editor chunk");
  });

  it("fails when the editor chunk does not hold the editor (wrong chunk scanned)", () => {
    expect(checkBundle([main, { ...editorOk, text: "nothing" }]).join("\n")).toContain("wrong chunk");
  });
});
