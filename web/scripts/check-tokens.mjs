#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only

// The build-time gate for #376: every design token lives in web/src/app/tokens.css
// and nowhere else, every var(--x) use resolves, both themes carry the same set,
// and the text/background pairs the file declares meet WCAG AA (4.5:1). Plain
// Node, no dependencies — `npm run build` runs this before `tsc`/`vite build`
// (package.json), so CI's existing "Typecheck + build" step in
// .forgejo/workflows/ci.yml is the gate; no new job.
//
// All the actual logic is in ../src/core/tokens/checkTokens.ts (pure functions,
// unit-tested in checkTokens.test.ts). This file only walks the tree, reads the
// files, and prints the result. The import below points straight at the .ts file:
// Node 24 strips TypeScript syntax natively for a file this simple (interfaces and
// plain functions only — no enums, no decorators, nothing that needs real codegen),
// so this needs no build step and no ts-node.

import { readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

import { checkTokens } from "../src/core/tokens/checkTokens.ts";

const webRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const srcDir = join(webRoot, "src");
const tokensPath = join(srcDir, "app", "tokens.css");

function walk(dir, out) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    const stat = statSync(full);
    if (stat.isDirectory()) {
      walk(full, out);
    } else if (entry.endsWith(".css") || entry.endsWith(".tsx")) {
      out.push(full);
    }
  }
  return out;
}

function toFileText(absolutePath) {
  return {
    // Displayed in every finding as "file:line: message" — relative to web/, with
    // forward slashes regardless of host OS, so a finding reads the same on every
    // contributor's machine and in CI.
    path: relative(webRoot, absolutePath).split("\\").join("/"),
    text: readFileSync(absolutePath, "utf8"),
  };
}

const tokensFile = toFileText(tokensPath);
const otherFiles = walk(srcDir, [])
  .filter((p) => p !== tokensPath)
  .map(toFileText);

const result = checkTokens(tokensFile, otherFiles);

for (const finding of result.findings) console.error(finding);
for (const note of result.notes) console.log(`note: ${note}`);
console.log(result.summaryLine);

process.exit(result.findings.length > 0 ? 1 : 0);
