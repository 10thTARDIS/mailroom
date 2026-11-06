// SPDX-License-Identifier: AGPL-3.0-only

// THE EDITOR'S SUPPLY CHAIN (ADR-051 §2). The compose editor is third-party code
// that parses attacker-supplied HTML, so what may enter the build is a decision
// written down here rather than whatever `npm install` happens to resolve:
//
//   * every `@tiptap/*` and `prosemirror-*` package in the lockfile is on an
//     explicit allowlist (a new one, or a transitive one a bump drags in, fails
//     until someone looks at it and adds it here);
//   * no `@tiptap-pro/*` package, and no private-registry scope, at all;
//   * every `resolved` URL is registry.npmjs.org (no git, tarball-URL or private
//     registry source), and no package is a `link`;
//   * the Tiptap packages are exact versions in package.json, and at or above the
//     first release with the 2026 advisories fixed.
//
// Pure functions over the parsed lockfile, so the test can feed them violations.

/** `@tiptap/*` packages the build may contain. Beta 1 uses core, pm, react,
 * starter-kit, extension-image and extension-file-handler directly; the rest are
 * what starter-kit and react bring with them. Betas 2 and 3 add their packages
 * (table, text-style, highlight, ...) here when they land. */
export const ALLOWED_TIPTAP: readonly string[] = [
  "@tiptap/core",
  "@tiptap/pm",
  "@tiptap/react",
  "@tiptap/starter-kit",
  "@tiptap/extensions",
  "@tiptap/extension-blockquote",
  "@tiptap/extension-bold",
  "@tiptap/extension-bubble-menu",
  "@tiptap/extension-bullet-list",
  "@tiptap/extension-code",
  "@tiptap/extension-code-block",
  "@tiptap/extension-document",
  "@tiptap/extension-dropcursor",
  "@tiptap/extension-file-handler",
  "@tiptap/extension-floating-menu",
  "@tiptap/extension-gapcursor",
  "@tiptap/extension-hard-break",
  "@tiptap/extension-heading",
  "@tiptap/extension-horizontal-rule",
  "@tiptap/extension-image",
  "@tiptap/extension-italic",
  "@tiptap/extension-link",
  "@tiptap/extension-list",
  "@tiptap/extension-list-item",
  "@tiptap/extension-list-keymap",
  "@tiptap/extension-ordered-list",
  "@tiptap/extension-paragraph",
  "@tiptap/extension-strike",
  "@tiptap/extension-text",
  "@tiptap/extension-text-style",
  "@tiptap/extension-underline",
];

/** `prosemirror-*` packages the build may contain (what @tiptap/pm re-exports). */
export const ALLOWED_PROSEMIRROR: readonly string[] = [
  "prosemirror-changeset",
  "prosemirror-commands",
  "prosemirror-dropcursor",
  "prosemirror-gapcursor",
  "prosemirror-history",
  "prosemirror-inputrules",
  "prosemirror-keymap",
  "prosemirror-model",
  "prosemirror-schema-list",
  "prosemirror-state",
  "prosemirror-tables",
  "prosemirror-transform",
  "prosemirror-view",
];

/** The first Tiptap 3 release with every 2026 advisory fixed (ADR-051 §2). */
export const MIN_TIPTAP = "3.30.4";

export const REGISTRY = "https://registry.npmjs.org/";

export interface LockPackage {
  version?: string;
  resolved?: string;
  link?: boolean;
  inBundle?: boolean;
}
export interface Lockfile {
  packages?: Record<string, LockPackage>;
}
export interface PackageJson {
  dependencies?: Record<string, string>;
  devDependencies?: Record<string, string>;
}

function versionAtLeast(version: string, min: string): boolean {
  const a = version.split(/[.-]/).slice(0, 3).map(Number);
  const b = min.split(".").map(Number);
  for (let i = 0; i < 3; i++) {
    const x = a[i] ?? 0;
    const y = b[i] ?? 0;
    if (x !== y) return x > y;
  }
  return true;
}

/** The package name of a lockfile path ("node_modules/a/node_modules/@s/b" is "@s/b"). */
function nameOf(path: string): string {
  const i = path.lastIndexOf("node_modules/");
  return i < 0 ? "" : path.slice(i + "node_modules/".length);
}

/** Every violation, as a readable line; empty when the lockfile is acceptable. */
export function checkLock(lock: Lockfile, pkg: PackageJson): string[] {
  const findings: string[] = [];
  const allowed = new Set([...ALLOWED_TIPTAP, ...ALLOWED_PROSEMIRROR]);
  for (const [path, p] of Object.entries(lock.packages ?? {})) {
    if (path === "") continue; // the root project
    const name = nameOf(path);
    if (name.startsWith("@tiptap-pro/") || name.startsWith("@tiptap-pro")) {
      findings.push(`${path}: a @tiptap-pro package (Pro packages need a private registry and are not open source)`);
    } else if ((name.startsWith("@tiptap/") || name.startsWith("prosemirror-")) && !allowed.has(name)) {
      findings.push(`${path}: ${name} is not on the editor allowlist (src/core/supplyChain/checkLock.ts)`);
    }
    if (p.link) {
      findings.push(`${path}: a link: dependency (resolves outside the registry)`);
    } else if (!p.inBundle && (p.resolved === undefined || !p.resolved.startsWith(REGISTRY))) {
      findings.push(`${path}: resolved from ${p.resolved ?? "(nowhere)"}, not ${REGISTRY}`);
    }
    if (name.startsWith("@tiptap/") && p.version && !versionAtLeast(p.version, MIN_TIPTAP)) {
      findings.push(`${path}: ${p.version} is older than ${MIN_TIPTAP}`);
    }
  }
  for (const group of [pkg.dependencies ?? {}, pkg.devDependencies ?? {}]) {
    for (const [name, range] of Object.entries(group)) {
      if (name.startsWith("@tiptap-pro")) findings.push(`package.json: ${name} is a Pro package`);
      if ((name.startsWith("@tiptap/") || name.startsWith("prosemirror-")) && !/^\d+\.\d+\.\d+$/.test(range)) {
        findings.push(`package.json: ${name} is "${range}", not an exact version`);
      }
    }
  }
  return findings;
}
