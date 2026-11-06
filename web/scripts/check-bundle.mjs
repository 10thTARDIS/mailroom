#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only

// Runs after `vite build` (package.json "build"): fails the build if the editor
// chunk contains eval/new Function/importScripts, or is not split out. The logic
// is ../src/core/bundle/checkBundle.ts (unit-tested); Node 24 strips the types.

import { readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { checkBundle } from "../src/core/bundle/checkBundle.ts";

const assets = join(dirname(fileURLToPath(import.meta.url)), "..", "dist", "assets");
const chunks = readdirSync(assets)
  .filter((n) => n.endsWith(".js"))
  .map((name) => ({ name, text: readFileSync(join(assets, name), "utf8") }));

const findings = checkBundle(chunks);
for (const f of findings) console.error(`check:bundle: ${f}`);
console.log(
  findings.length === 0
    ? `check:bundle: ok (${chunks.length} chunks scanned; editor chunk is free of eval, new Function and importScripts)`
    : `check:bundle: ${findings.length} finding(s)`,
);
process.exit(findings.length > 0 ? 1 : 0);
