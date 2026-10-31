// SPDX-License-Identifier: AGPL-3.0-only
//
// THE SHIPPED SECURITY POLICY, READ OUT OF THE REPO AT RUN TIME (Forgejo #308).
//
// scripts/hostile-content-e2e.sh and scripts/inline-images-e2e.sh drive a real
// headless Chromium at the documents this app renders, and every assertion they
// make is about a POLICY: this CSP, this sandbox token list, this response
// header set. Until now each script carried its own hand-typed copy of those
// artifacts, and NOTHING COMPARED THE COPY TO THE SOURCE.
//
// That is the failure #308 reported. Each artifact does have a unilateral anchor
// on the source side (TestAppCSPDirectives, TestMessageSandboxIsNotRelaxed,
// TestPreviewResponseHeaders), but a coordinated edit — "change
// writePreviewHeaders, update its test" — left the e2e proving the OLD header
// set safe against Chromium while the server served the new one, and the suite
// printed CLEAN. The one self-check that existed was self-referential: the
// script recomputed the sha256 of ITS OWN copy of the height reporter and
// compared it to ITS OWN constant, which cannot detect anything.
// inline-images-e2e.sh had already drifted: its message CSP omitted
// `script-src '<reporter hash>'` entirely — the pre-#100 policy.
//
// So there are no copies any more. This module PARSES the three authoritative
// sources and hands back what they actually say:
//
//   web/src/app/ui/messageDocument.ts   REPORTER_JS, REPORTER_SHA256,
//                                       MESSAGE_SANDBOX, messageCSP()
//   cmd/mailroom/main.go                appCSP (+ messageHeightReporterHash)
//   internal/api/blob.go                writePreviewHeaders' constant headers
//
// ⚠ IT FAILS CLOSED, AND THAT IS THE WHOLE DESIGN. Every extraction throws if it
// cannot find what it is looking for, and the shape checks below throw if what
// it found is not a policy. An extractor that quietly returned "" would hand the
// browser stage an EMPTY CSP — under which nothing is blocked, every negative
// assertion still passes, and the suite would report the strongest possible
// result while testing no policy at all. That is the #131 shape, so a rename in
// main.go must stop this suite, not silently weaken it.
//
// Usage:
//   const p = require("./e2e-policy.cjs").extract("/tmp/policy-src");
//   node scripts/e2e-policy.cjs <dir>     # print what it extracted, as JSON
//
// <dir> holds the three sources under fixed names (messageDocument.ts, main.go,
// blob.go); the shell scripts copy them there, because the browser container has
// no checkout. The default is a repo root, so it also runs straight from a clone.

const fs = require("fs");
const path = require("path");
const crypto = require("crypto");

const SOURCES = {
  messageDocument: ["web/src/app/ui/messageDocument.ts", "messageDocument.ts"],
  main: ["cmd/mailroom/main.go", "main.go"],
  blob: ["internal/api/blob.go", "blob.go"],
};

function read(dir, key) {
  for (const rel of SOURCES[key]) {
    const p = path.join(dir, rel);
    if (fs.existsSync(p)) return fs.readFileSync(p, "utf8");
  }
  throw new Error(
    "e2e-policy: cannot find " + key + " under " + dir + " (tried " +
    SOURCES[key].join(", ") + "). The e2e suites test the policy this file " +
    "reads; with the source missing there is nothing to test, so this is a " +
    "failure and not a skip.");
}

// --- primitives ---------------------------------------------------------------

// A double-quoted literal is the same shape in Go, TypeScript and JSON for
// everything these files contain, so JSON.parse is the decoder for all three.
const STRING_LITERAL = /"(?:[^"\\]|\\.)*"/;

/** The single string literal assigned to `name`, in Go or TS. */
function stringConst(src, name, what) {
  const re = new RegExp(
    "(?:export\\s+)?const\\s+" + name + "\\s*(?::[^=]*)?=\\s*(" + STRING_LITERAL.source + ")");
  const m = src.match(re);
  if (!m) throw new Error("e2e-policy: cannot extract " + name + " (" + what + ")");
  return JSON.parse(m[1]);
}

/** The value of a Go const whose initializer is a multi-line `"a" + b + "c"`
 * concatenation. `idents` supplies the value of every identifier that may
 * appear.
 *
 * The residue check at the end is the fail-closed part: if the expression grows
 * a function call, a raw backtick string or an identifier nobody told us about,
 * this throws rather than silently returning the literals it happened to
 * recognise — which would be a policy with directives missing. */
function goConcatConst(src, name, idents, what) {
  const lines = src.split(/\r?\n/);
  let i = lines.findIndex((l) => new RegExp("^const\\s+" + name + "\\s*=").test(l));
  if (i < 0) throw new Error("e2e-policy: cannot find `const " + name + "` (" + what + ")");
  let expr = lines[i].replace(new RegExp("^const\\s+" + name + "\\s*=\\s*"), "");
  while (/\+\s*$/.test(expr) && i + 1 < lines.length) {
    i += 1;
    expr += "\n" + lines[i];
  }

  let work = expr;
  for (const [ident, value] of Object.entries(idents)) {
    work = work.replace(new RegExp("\\b" + ident + "\\b", "g"), JSON.stringify(value));
  }
  const parts = work.match(new RegExp(STRING_LITERAL.source, "g")) || [];
  if (!parts.length) throw new Error("e2e-policy: " + name + " has no string parts (" + what + ")");
  const residue = work
    .replace(new RegExp(STRING_LITERAL.source, "g"), "")
    .replace(/[+\s]/g, "");
  if (residue !== "") {
    throw new Error(
      "e2e-policy: " + name + " contains something this extractor cannot evaluate: " +
      JSON.stringify(residue) + ". Teach goConcatConst about it — do NOT let the " +
      "e2e run against a partially-read policy.");
  }
  return parts.map((p) => JSON.parse(p)).join("");
}

/** Evaluates the real `messageCSP` from messageDocument.ts.
 *
 * The function is EVALUATED rather than reconstructed, so there is no second
 * implementation of the policy to drift: whatever directives the shipped
 * function returns are the directives the browser stage renders under. The only
 * transformation is removing the two TypeScript type annotations, and if the
 * signature ever changes shape the strip fails and `new Function` throws.
 *
 * On `new Function`: the input is a FIRST-PARTY source file out of this
 * repository's own checkout, which the same run is about to compile and ship —
 * it is not user input and there is no trust boundary between it and this
 * process. Anyone who can edit web/src/app/ui/messageDocument.ts can already run
 * whatever they like in CI. Nothing that arrives over the network, out of a
 * message, or from the container under test reaches this call. */
function messageCSPFn(src, reporterSha) {
  const start = src.indexOf("export function messageCSP");
  if (start < 0) throw new Error("e2e-policy: cannot find `export function messageCSP` in messageDocument.ts");
  const lines = src.slice(start).split(/\r?\n/);
  const end = lines.findIndex((l) => l === "}");
  if (end < 0) throw new Error("e2e-policy: messageCSP has no closing brace at column 0");
  // The strip keeps every PARAMETER and removes only the type annotations
  // (`allowRemote: boolean, origin = ""` -> `allowRemote, origin = ""`), so a
  // parameter added to the real function (#460 added `origin`) is still bound
  // when the body runs — dropping it would make the body read an undeclared
  // name and throw, which is how the first #460 attempt failed the suites.
  const body = lines
    .slice(0, end + 1)
    .join("\n")
    .replace(/^export\s+/, "")
    .replace(/function\s+messageCSP\s*\(([^)]*)\)\s*:\s*string/, (_m, params) =>
      "function messageCSP(" + params.replace(/:\s*\w+/g, "") + ")");
  if (/:\s*(string|boolean)\b/.test(body.split("\n")[0])) {
    throw new Error("e2e-policy: messageCSP still carries a type annotation after the strip: " + body.split("\n")[0]);
  }
  // eslint-disable-next-line no-new-func
  return new Function("REPORTER_SHA256", body + "\nreturn messageCSP;")(reporterSha);
}

/** writePreviewHeaders' CONSTANT headers, plus the names of the ones it computes.
 *
 * Content-Type, Content-Disposition and Content-Length are per-response and are
 * supplied by the harness; every other header in that function is a fixed
 * security control and is returned verbatim. The dynamic names are returned too
 * so the caller can fail when a NEW computed header appears that its harness
 * knows nothing about. */
function previewHeaders(src) {
  const start = src.indexOf("func writePreviewHeaders(");
  if (start < 0) throw new Error("e2e-policy: cannot find `func writePreviewHeaders` in blob.go");
  const lines = src.slice(start).split(/\r?\n/);
  const end = lines.findIndex((l, i) => i > 0 && l === "}");
  if (end < 0) throw new Error("e2e-policy: writePreviewHeaders has no closing brace at column 0");
  const body = lines.slice(0, end + 1).join("\n");

  // ⚠ A NAMED CONSTANT IS STILL A CONSTANT (#410). writePreviewHeaders sets
  // Cache-Control from a package-level `const`, shared with the download route
  // so the two caching policies cannot drift. Classifying an identifier as
  // "dynamic" would make the harness declare a hole it does not have — and the
  // pressure that creates is to add the name to HARNESS_SUPPLIED, which would
  // let the harness serve its OWN value for a header the real route fixes.
  // Resolving the const keeps the reproduction faithful and keeps the guard
  // sharp for headers that genuinely vary per response.
  const consts = {};
  const constRe = new RegExp("^const\\s+(\\w+)\\s*=\\s*(" + STRING_LITERAL.source + ")\\s*$", "gm");
  let c;
  while ((c = constRe.exec(src)) !== null) consts[c[1]] = JSON.parse(c[2]);

  const constant = {};
  const dynamic = [];
  const re = new RegExp("h\\.Set\\(\\s*(" + STRING_LITERAL.source + ")\\s*,\\s*([^\\n]*?)\\)\\s*$", "gm");
  let m;
  while ((m = re.exec(body)) !== null) {
    const name = JSON.parse(m[1]);
    const value = m[2].trim();
    if (new RegExp("^" + STRING_LITERAL.source + "$").test(value)) constant[name] = JSON.parse(value);
    else if (Object.prototype.hasOwnProperty.call(consts, value)) constant[name] = consts[value];
    else dynamic.push(name);
  }
  if (!Object.keys(constant).length) {
    throw new Error("e2e-policy: writePreviewHeaders yielded no constant headers — the parse is wrong");
  }
  return { constant, dynamic };
}

// --- the extraction, plus the checks that make it worth trusting ---------------

function extract(dir) {
  dir = dir || path.join(__dirname, "..");
  const ts = read(dir, "messageDocument");
  const mainGo = read(dir, "main");
  const blobGo = read(dir, "blob");

  const REPORTER_JS = stringConst(ts, "REPORTER_JS", "the trusted height reporter");
  const REPORTER_SHA256 = stringConst(ts, "REPORTER_SHA256", "the reporter's CSP hash pin");
  const MESSAGE_SANDBOX = stringConst(ts, "MESSAGE_SANDBOX", "the message iframe sandbox tokens");
  const messageHeightReporterHash = stringConst(mainGo, "messageHeightReporterHash", "the shell's copy of the pin");
  const APP_CSP = goConcatConst(mainGo, "appCSP", { messageHeightReporterHash }, "the app shell CSP");
  const csp = messageCSPFn(ts, REPORTER_SHA256);
  const PREVIEW = previewHeaders(blobGo);

  // ⚠ THIS IS THE CHECK THE OLD SELF-CHECK ONLY LOOKED LIKE (#308 item 1).
  //
  // The hash is a THREE-WAY pin: the reporter's bytes, the constant in
  // messageDocument.ts, and the copy in main.go's appCSP, which must intersect
  // with the message document's own policy or auto-height silently dies (#100).
  // Nothing in the repo hashed the actual reporter before this line —
  // TestMessageSandboxIsNotRelaxed only checks that the hash STRING appears in
  // both files, so editing one byte of the reporter without recomputing the pin
  // passed every test and killed auto-height.
  const computed = "sha256-" + crypto.createHash("sha256").update(REPORTER_JS, "utf8").digest("base64");
  if (computed !== REPORTER_SHA256) {
    throw new Error(
      "e2e-policy: THE REPORTER HASH PIN IS STALE. messageDocument.ts's REPORTER_JS " +
      "hashes to " + computed + " but REPORTER_SHA256 says " + REPORTER_SHA256 + ". " +
      "The height reporter would be blocked by its own CSP and auto-height would " +
      "silently stop working. Recompute the pin in messageDocument.ts AND in " +
      "cmd/mailroom/main.go's messageHeightReporterHash.");
  }
  if (messageHeightReporterHash !== REPORTER_SHA256) {
    throw new Error(
      "e2e-policy: the two hash pins disagree. main.go has " + messageHeightReporterHash +
      ", messageDocument.ts has " + REPORTER_SHA256 + ". The srcdoc frame inherits " +
      "the shell policy and the two INTERSECT, so a mismatch bans the reporter.");
  }

  // Shape checks. Each names the specific way a silent extraction failure would
  // hand the browser stage a policy that forbids nothing.
  const must = (cond, msg) => { if (!cond) throw new Error("e2e-policy: " + msg); };
  must(/^default-src 'none'/.test(APP_CSP), "the extracted appCSP does not start with default-src 'none': " + APP_CSP);
  must(APP_CSP.includes(REPORTER_SHA256), "the extracted appCSP does not carry the reporter hash: " + APP_CSP);
  must(/^default-src 'none'/.test(csp(false)), "the extracted message CSP does not start with default-src 'none': " + csp(false));
  must(csp(false).includes(REPORTER_SHA256), "the extracted message CSP does not carry the reporter hash: " + csp(false));
  must(csp(false).includes("img-src data:"), "the extracted message CSP does not block remote images: " + csp(false));
  // #460: an allowed message must admit the app's own origin (the proxied image
  // path is same-origin, and 'self' matches nothing in the frame), and a
  // blocked one must not, whatever origin it is handed.
  must(csp(true, "http://mail.example.test:8080").includes(" http://mail.example.test:8080"),
    "the extracted message CSP does not admit the app origin when remote content is allowed (#460): " +
    csp(true, "http://mail.example.test:8080"));
  must(!csp(false, "http://mail.example.test:8080").includes("mail.example.test"),
    "the extracted message CSP admits the app origin for a BLOCKED message (#460): " +
    csp(false, "http://mail.example.test:8080"));
  must(MESSAGE_SANDBOX.split(/\s+/).length >= 2, "the extracted sandbox token list looks empty: " + MESSAGE_SANDBOX);
  must(REPORTER_JS.includes("postMessage"), "the extracted reporter does not postMessage: " + REPORTER_JS);
  must(typeof PREVIEW.constant["Content-Security-Policy"] === "string",
    "writePreviewHeaders no longer sets a constant Content-Security-Policy");
  must(/(^|;)\s*sandbox(\s|;|$)/.test(PREVIEW.constant["Content-Security-Policy"]),
    "the preview CSP has lost its `sandbox` directive — that directive is what makes the " +
    "served document's origin opaque: " + PREVIEW.constant["Content-Security-Policy"]);

  return {
    REPORTER_JS,
    REPORTER_SHA256,
    MESSAGE_SANDBOX,
    APP_CSP,
    messageCSP: csp,
    MSG_CSP: csp(false),
    MSG_CSP_REMOTE: csp(true),
    PREVIEW_CONSTANT_HEADERS: PREVIEW.constant,
    PREVIEW_DYNAMIC_HEADERS: PREVIEW.dynamic,
  };
}

module.exports = { extract };

if (require.main === module) {
  const p = extract(process.argv[2]);
  const { messageCSP, ...printable } = p;
  console.log(JSON.stringify(printable, null, 2));
}
