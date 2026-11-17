#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
//
// The web job's dependency gate.
//
//   node scripts/npm-audit-gate.cjs              (run in web/; calls npm audit twice)
//   node scripts/npm-audit-gate.cjs --self-test  (no npm; exercises the rules)
//
// Rules:
//   1. What ships (production dependencies) may carry NO high or critical
//      advisory. Nothing can be allowlisted here.
//   2. The full tree (build and test tools included) may carry a high or
//      critical advisory only if web/audit-allow.json lists its GHSA id with a
//      reason and an expiry date no more than 90 days away. Past the date the
//      gate fails again, so an exception is always visible and temporary.
//   3. An entry that matches nothing current is reported so it can be removed;
//      it does not fail the gate (an upstream fix landing is good news).
//
// Why a gate of our own rather than `npm audit --audit-level=high`: an advisory
// with no fixed version in a build-only tool turned every pull request red with
// nothing to do but wait, while a production-only audit would have silently
// stopped watching the build tools altogether. This keeps both, and makes the
// one judgement in between (a dated, reasoned exception) explicit.

"use strict";

const { execFileSync } = require("node:child_process");
const { readFileSync } = require("node:fs");
const path = require("node:path");

const BLOCKING = new Set(["high", "critical"]);
const MAX_DAYS = 90;

/**
 * Fail closed on a report that is not a report. `npm audit` answers a registry
 * failure with {"error": ...} and no vulnerabilities at all, which would read as
 * a clean tree. A report whose own totals count blocking advisories that the
 * parser below cannot find means the format moved under us: also a failure.
 */
function checkReport(report, label) {
  if (!report || typeof report !== "object") return `${label}: npm audit returned no JSON report`;
  if (report.error) return `${label}: npm audit failed: ${JSON.stringify(report.error).slice(0, 300)}`;
  if (!report.vulnerabilities || typeof report.vulnerabilities !== "object") return `${label}: report has no "vulnerabilities" object`;
  const totals = report.metadata && report.metadata.vulnerabilities;
  if (!totals || typeof totals !== "object") return `${label}: report has no "metadata.vulnerabilities" totals`;
  const counted = (totals.high || 0) + (totals.critical || 0);
  if (counted > 0 && advisories(report).size === 0) {
    return `${label}: report counts ${counted} high/critical but none could be read; the format may have changed`;
  }
  return null;
}

/** Every high/critical advisory in an `npm audit --json` report, by GHSA id. */
function advisories(report) {
  const out = new Map();
  for (const [pkg, v] of Object.entries((report && report.vulnerabilities) || {})) {
    for (const via of v.via || []) {
      if (typeof via === "string" || !BLOCKING.has(via.severity)) continue;
      const m = /GHSA-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}/i.exec(via.url || "");
      const id = m ? m[0] : `npm-${via.source}`;
      if (!out.has(id)) out.set(id, { id, pkg, severity: via.severity, title: via.title || "", url: via.url || "" });
    }
  }
  return out;
}

function daysBetween(a, b) {
  return Math.round((Date.parse(b) - Date.parse(a)) / 86400000);
}

/** Pure decision: { errors: string[], notes: string[] }. `today` is YYYY-MM-DD. */
function evaluate(prodReport, fullReport, allow, today) {
  const errors = [];
  const notes = [];
  for (const [r, label] of [[prodReport, "production audit"], [fullReport, "full audit"]]) {
    const bad = checkReport(r, label);
    if (bad) errors.push(bad);
  }
  if (errors.length) return { errors, notes };
  const prod = advisories(prodReport);
  const full = advisories(fullReport);
  const entries = new Map();

  for (const e of (allow && allow.advisories) || []) {
    const where = `audit-allow.json entry ${e && e.id ? e.id : "(no id)"}`;
    if (!e || !/^GHSA-/.test(e.id || "")) { errors.push(`${where}: "id" must be a GHSA id`); continue; }
    if (!e.reason || String(e.reason).trim().length < 20) { errors.push(`${where}: "reason" must say why it is acceptable`); continue; }
    if (!/^\d{4}-\d{2}-\d{2}$/.test(e.expires || "")) { errors.push(`${where}: "expires" must be YYYY-MM-DD`); continue; }
    if (e.expires < today) { errors.push(`${where}: expired on ${e.expires}; check for a fixed version or renew it deliberately`); continue; }
    if (daysBetween(today, e.expires) > MAX_DAYS) { errors.push(`${where}: expires more than ${MAX_DAYS} days ahead`); continue; }
    entries.set(e.id, e);
  }

  for (const a of prod.values()) {
    errors.push(`${a.id} (${a.severity}) in ${a.pkg} reaches production code: ${a.title} ${a.url}`.trim());
  }
  for (const a of full.values()) {
    if (prod.has(a.id)) continue;
    if (entries.has(a.id)) {
      notes.push(`${a.id} in ${a.pkg} allowed until ${entries.get(a.id).expires} (build and test tools only)`);
      continue;
    }
    errors.push(`${a.id} (${a.severity}) in ${a.pkg}: ${a.title} ${a.url}`.trim());
  }
  for (const id of entries.keys()) {
    if (!full.has(id)) notes.push(`${id} is allowlisted but no longer reported; remove it from audit-allow.json`);
  }
  return { errors, notes };
}

function audit(args) {
  try {
    return JSON.parse(execFileSync("npm", ["audit", "--json", ...args], { encoding: "utf8", stdio: ["ignore", "pipe", "inherit"] }));
  } catch (err) {
    // npm audit exits non-zero when it finds anything; its JSON is still on stdout.
    // Anything unparseable becomes null, which checkReport turns into a failure.
    if (err.stdout) {
      try {
        return JSON.parse(err.stdout);
      } catch {
        return null;
      }
    }
    return null;
  }
}

function selfTest() {
  const report = (pkg, id, severity = "high") => ({
    vulnerabilities: {
      [pkg]: { severity, via: [{ source: 1, url: `https://github.com/advisories/${id}`, severity, title: "t" }] },
      dependent: { severity, via: [pkg] },
    },
    metadata: { vulnerabilities: { [severity]: 2 } },
  });
  const empty = { vulnerabilities: {}, metadata: { vulnerabilities: { high: 0, critical: 0 } } };
  const ok = { id: "GHSA-aaaa-bbbb-cccc", reason: "build-time only; matches our own fixed globs", expires: "2026-12-01" };
  const today = "2026-11-17";
  const cases = [
    ["clean tree passes", empty, empty, { advisories: [] }, 0],
    ["unlisted dev advisory fails", empty, report("x", "GHSA-aaaa-bbbb-cccc"), { advisories: [] }, 1],
    ["listed dev advisory passes", empty, report("x", "GHSA-aaaa-bbbb-cccc"), { advisories: [ok] }, 0],
    ["listing cannot excuse production", report("x", "GHSA-aaaa-bbbb-cccc"), report("x", "GHSA-aaaa-bbbb-cccc"), { advisories: [ok] }, 1],
    ["expired entry fails", empty, report("x", "GHSA-aaaa-bbbb-cccc"), { advisories: [{ ...ok, expires: "2026-11-16" }] }, 2],
    ["entry too far ahead fails", empty, report("x", "GHSA-aaaa-bbbb-cccc"), { advisories: [{ ...ok, expires: "2027-06-01" }] }, 2],
    ["entry without a reason fails", empty, report("x", "GHSA-aaaa-bbbb-cccc"), { advisories: [{ ...ok, reason: "" }] }, 2],
    ["a listed id does not excuse a different one", empty, report("x", "GHSA-zzzz-zzzz-zzzz"), { advisories: [ok] }, 1],
    ["moderate is not blocking", empty, report("x", "GHSA-zzzz-zzzz-zzzz", "moderate"), { advisories: [] }, 0],
    ["stale entry is only a note", empty, empty, { advisories: [ok] }, 0],
    ["an npm error report fails closed", empty, { error: { code: "ENOTFOUND" } }, { advisories: [] }, 1],
    ["a report with no vulnerabilities object fails closed", { metadata: { vulnerabilities: {} } }, empty, { advisories: [] }, 1],
    ["a report with no totals fails closed", empty, { vulnerabilities: {} }, { advisories: [] }, 1],
    ["totals that the parser cannot find fail closed", empty, { vulnerabilities: { x: { severity: "high", via: ["y"] } }, metadata: { vulnerabilities: { high: 1 } } }, { advisories: [] }, 1],
  ];
  let failed = 0;
  for (const [name, prod, full, allow, wantErrors] of cases) {
    const got = evaluate(prod, full, allow, today).errors.length;
    const pass = got === wantErrors;
    if (!pass) failed++;
    console.log(`${pass ? "ok  " : "FAIL"} ${name} (errors: ${got}, want ${wantErrors})`);
  }
  if (failed) { console.error(`self-test: ${failed} case(s) failed`); process.exit(1); }
  console.log("self-test: all cases passed");
}

function main() {
  if (process.argv.includes("--self-test")) return selfTest();
  const allowPath = path.join(process.cwd(), "audit-allow.json");
  const allow = JSON.parse(readFileSync(allowPath, "utf8"));
  const today = new Date().toISOString().slice(0, 10);
  const { errors, notes } = evaluate(audit(["--omit=dev"]), audit([]), allow, today);
  for (const n of notes) console.log(`note: ${n}`);
  if (errors.length) {
    for (const e of errors) console.error(`error: ${e}`);
    console.error(`dependency gate: ${errors.length} problem(s)`);
    process.exit(1);
  }
  console.log("dependency gate: production clean; full tree clean or allowlisted");
}

module.exports = { evaluate, advisories };
if (require.main === module) main();
