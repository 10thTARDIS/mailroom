// SPDX-License-Identifier: AGPL-3.0-only

// The editor's supply-chain rules (ADR-051 §2), run against the REAL lockfile and
// then against synthetic violations. The second half is what makes the first
// half mean something: a checker that reported nothing for every input would
// pass the real lockfile too.

import { describe, expect, it } from "vitest";
import { checkLock, MIN_TIPTAP } from "./checkLock";
import type { Lockfile, PackageJson } from "./checkLock";
import lockText from "../../../package-lock.json?raw";
import pkgText from "../../../package.json?raw";

const lock = JSON.parse(lockText) as Lockfile;
const pkg = JSON.parse(pkgText) as PackageJson;

const good: Lockfile = {
  packages: {
    "": {},
    "node_modules/@tiptap/core": { version: MIN_TIPTAP, resolved: "https://registry.npmjs.org/@tiptap/core/-/core-3.30.4.tgz" },
    "node_modules/prosemirror-model": { version: "1.25.0", resolved: "https://registry.npmjs.org/prosemirror-model/-/prosemirror-model-1.25.0.tgz" },
  },
};
const goodPkg: PackageJson = { dependencies: { "@tiptap/core": MIN_TIPTAP } };

function withPackage(path: string, p: object): Lockfile {
  return { packages: { ...good.packages, [path]: p } };
}

describe("the real lockfile", () => {
  it("has no finding", () => {
    expect(checkLock(lock, pkg)).toEqual([]);
  });

  it("actually contains the editor (the check is looking at something)", () => {
    const names = Object.keys(lock.packages ?? {});
    expect(names).toContain("node_modules/@tiptap/core");
    expect(names).toContain("node_modules/prosemirror-model");
    expect(Object.keys(lock.packages ?? {}).length).toBeGreaterThan(100);
  });

  it("holds the editor packages at exact versions in package.json", () => {
    for (const n of ["@tiptap/core", "@tiptap/pm", "@tiptap/react", "@tiptap/starter-kit"]) {
      expect(pkg.dependencies?.[n]).toMatch(/^\d+\.\d+\.\d+$/);
    }
  });
});

describe("violations are found (negative controls)", () => {
  it("accepts the synthetic good fixture", () => {
    expect(checkLock(good, goodPkg)).toEqual([]);
  });

  it("an @tiptap package not on the allowlist", () => {
    const f = checkLock(withPackage("node_modules/@tiptap/extension-collaboration", { version: "3.33.0", resolved: "https://registry.npmjs.org/x.tgz" }), goodPkg);
    expect(f.join("\n")).toContain("not on the editor allowlist");
  });

  it("a prosemirror package not on the allowlist", () => {
    const f = checkLock(withPackage("node_modules/prosemirror-evil", { version: "1.0.0", resolved: "https://registry.npmjs.org/x.tgz" }), goodPkg);
    expect(f.join("\n")).toContain("not on the editor allowlist");
  });

  it("a @tiptap-pro package", () => {
    const f = checkLock(withPackage("node_modules/@tiptap-pro/extension-ai", { version: "3.0.0", resolved: "https://registry.npmjs.org/x.tgz" }), goodPkg);
    expect(f.join("\n")).toContain("@tiptap-pro");
  });

  it("a nested copy is checked too", () => {
    const f = checkLock(withPackage("node_modules/a/node_modules/@tiptap-pro/x", { version: "1.0.0", resolved: "https://registry.npmjs.org/x.tgz" }), goodPkg);
    expect(f.join("\n")).toContain("@tiptap-pro");
  });

  it("a package resolved from another registry", () => {
    const f = checkLock(withPackage("node_modules/left-pad", { version: "1.0.0", resolved: "https://registry.example.com/left-pad.tgz" }), goodPkg);
    expect(f.join("\n")).toContain("not https://registry.npmjs.org/");
  });

  it("a package resolved from a git URL, or from nowhere", () => {
    expect(checkLock(withPackage("node_modules/a", { version: "1.0.0", resolved: "git+https://example.com/a.git" }), goodPkg)).toHaveLength(1);
    expect(checkLock(withPackage("node_modules/b", { version: "1.0.0" }), goodPkg)).toHaveLength(1);
  });

  it("a link dependency", () => {
    expect(checkLock(withPackage("node_modules/c", { link: true, resolved: "../c" }), goodPkg).join("\n")).toContain("link");
  });

  it("a registry URL that merely starts the same (a look-alike host)", () => {
    const f = checkLock(withPackage("node_modules/d", { version: "1.0.0", resolved: "https://registry.npmjs.org.example.com/d.tgz" }), goodPkg);
    expect(f.join("\n")).toContain("not https://registry.npmjs.org/");
  });

  it("a Tiptap release older than the advisory fixes", () => {
    const f = checkLock(withPackage("node_modules/@tiptap/pm", { version: "3.30.3", resolved: "https://registry.npmjs.org/x.tgz" }), goodPkg);
    expect(f.join("\n")).toContain("older than");
  });

  it("a range instead of an exact version in package.json", () => {
    expect(checkLock(good, { dependencies: { "@tiptap/core": "^3.33.0" } }).join("\n")).toContain("not an exact version");
  });

  it("a Pro package in package.json", () => {
    expect(checkLock(good, { dependencies: { "@tiptap-pro/x": "1.0.0" } }).join("\n")).toContain("Pro package");
  });
});
