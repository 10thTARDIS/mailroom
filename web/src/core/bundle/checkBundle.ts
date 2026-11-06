// SPDX-License-Identifier: AGPL-3.0-only

// THE EDITOR BUNDLE CHECK (ADR-051 §2, §6). Two facts about the built output are
// worth failing the build over:
//
//   1. NO eval, NO dynamic code. The page's CSP is `script-src 'self'` with no
//      'unsafe-eval' (cmd/mailroom appCSP), so code that needs `eval(`,
//      `new Function` or `importScripts` would break in production and, worse,
//      would be a sign the editor library had started doing something its
//      adopters did not review. This looks at the editor's chunk.
//   2. The editor is in its OWN chunk, and the entry chunk holds none of it. The
//      composer loads it on demand; if a static import ever pulled it into the
//      main bundle, every reading view would pay for it, and this check would
//      have nothing to scan. A missing editor chunk is therefore a failure, not a
//      pass.
//
// Pure functions over chunk names and text; scripts/check-bundle.mjs reads dist/.

/** The tokens that must not appear in the editor's code. */
export const FORBIDDEN: ReadonlyArray<{ name: string; re: RegExp }> = [
  { name: "eval(", re: /(?<![\w$.])eval\s*\(/ },
  { name: "new Function", re: /new\s+Function\b/ },
  { name: "importScripts", re: /\bimportScripts\b/ },
];

/** The marker a ProseMirror-based chunk always carries (class names the library
 * writes into the DOM). */
const EDITOR_MARKER = "ProseMirror";

export interface Chunk {
  /** File name under dist/assets, e.g. "RichTextEditor-AbC123.js". */
  name: string;
  text: string;
}

export function checkBundle(chunks: Chunk[]): string[] {
  const findings: string[] = [];
  const editor = chunks.filter((c) => /^RichTextEditor-.*\.js$/.test(c.name));
  if (editor.length === 0) {
    findings.push("no RichTextEditor-*.js chunk: the editor is not code-split (or the chunk was renamed)");
  }
  for (const c of editor) {
    if (!c.text.includes(EDITOR_MARKER)) {
      findings.push(`${c.name}: does not contain the editor (no "${EDITOR_MARKER}"); the check is looking at the wrong chunk`);
    }
    for (const f of FORBIDDEN) {
      if (f.re.test(c.text)) findings.push(`${c.name}: contains ${f.name}`);
    }
  }
  for (const c of chunks) {
    if (/^RichTextEditor-/.test(c.name) || !c.name.endsWith(".js")) continue;
    if (c.text.includes(EDITOR_MARKER)) {
      findings.push(`${c.name}: holds editor code outside the editor chunk (a static import pulls it into the main bundle)`);
    }
  }
  return findings;
}
