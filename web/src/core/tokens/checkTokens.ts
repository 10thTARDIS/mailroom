// SPDX-License-Identifier: AGPL-3.0-only

// The pure logic behind the design-token check (#376). `web/scripts/check-tokens.mjs`
// is the thin runner that actually reads the tree and calls this; everything that can
// go wrong lives here instead, where it can be unit-tested with synthetic fixtures
// rather than only ever being probed by editing the real tree by hand.
//
// WHAT THIS CATCHES, AND WHY IT HAS TO BE A BUILD STEP RATHER THAN A COMMENT: CSS
// fails silently. v1.28.0 shipped a stylesheet using `--surface`, `--surface-hover`,
// `--text`, `--text-muted` — four of six undefined — and nothing errored; the tab
// strip just rendered wrong, in both themes, until someone noticed by eye. It is
// STILL happening: before this issue, `var(--surface)` was used 12 times in
// styles.css (9 with no fallback) while `--surface` was defined nowhere, and
// `--pane-split` was read in CSS but only ever given a value at runtime by
// app/prefs.ts's `style.setProperty` — so a page that loaded before that line ran
// (or a print stylesheet, or a future consumer of the same custom property) saw
// `initial`, not a real size. #372 pass 2 adds semantic tones (held, sealed,
// returned) and per-account/per-label colours that all have to survive both themes
// at equal contrast, which is exactly the kind of fact a comment promises and a
// build step actually keeps.
//
// THE ONE INVENTORY: every custom property tokens.css defines lives in exactly one
// of four shapes — `:root`, `html[data-density="…"]`, or `html[data-theme="light"
// | "dark"]`. A declaration in any OTHER file, in one of THOSE SAME shapes, is a
// second, silently-shadowing definition of a token and is an error. A declaration
// in some other selector (e.g. styles.css's `.list-pane { --tab-strip-h: 2.4rem; }`,
// which inherits down to `.tab-strip`/`.list-toolbar` on purpose — see the comment
// beside it) is not a design token at all: it is an ordinary CSS custom property
// scoped to one component subtree, the same pattern any plain CSS file uses for a
// value only its own children need, and promoting it into the global inventory
// would turn a one-component implementation detail into a promise it never made.
// So the "defined outside tokens.css" rule below only fires for the four
// recognised shapes; declarations elsewhere are still tracked (so a var(--tab-
// strip-h) USE is never wrongly reported as undefined) but are not part of the
// token inventory the notes/contrast checks reason about.
//
// CONTRAST: the pairs the file promises are declared as machine-readable comments
// (`@contrast fg/bg`, one per line, parsed from the RAW text before comments are
// stripped for everything else) and checked against WCAG 2.x relative luminance at
// the AA text threshold, 4.5:1, in both themes. A pair already in the tree that
// fails is not silently changed here — #376 is the inventory and the check, not
// the restyle (the palette itself is #372 pass 2 territory) — it is named on an
// `@contrast-known-low` line instead, with its measured ratio, so the check is
// green when this lands (CLAUDE.md: a gate introduced already-red teaches people
// to ignore it) and the debt stays visible in the one file that would otherwise
// claim everything passes. If a known-low pair is later found to have improved
// past 4.5, THAT is a finding too — the debt line must never go stale in the
// direction of overstating a problem that no longer exists.

export interface FileText {
  path: string;
  text: string;
}

export interface TokenLocation {
  value: string;
  line: number;
}

export interface Inventory {
  root: Map<string, TokenLocation>;
  density: {
    comfortable: Map<string, TokenLocation>;
    cozy: Map<string, TokenLocation>;
    compact: Map<string, TokenLocation>;
  };
  light: Map<string, TokenLocation>;
  dark: Map<string, TokenLocation>;
}

export interface Declaration {
  name: string;
  value: string;
  line: number;
  selector: string;
  /** Brace nesting depth at the declaration (1 = directly inside the selector
   *  named above, with nothing else enclosing it; 2+ = that selector is itself
   *  nested in something else, e.g. an `@media` block). Only depth-1 root/
   *  density/theme declarations count as "the inventory" — see
   *  isTopLevelTokenBlock below for why. */
  depth: number;
}

export interface Use {
  file: string;
  line: number;
  name: string;
}

export interface ContrastPairDirective {
  pair: string;
  line: number;
}

export interface KnownLowDirective {
  theme: "light" | "dark";
  pair: string;
  ratio: number;
  line: number;
}

export interface CheckSummary {
  definedCount: number;
  usedCount: number;
  pairsPerTheme: number;
  notesCount: number;
}

export interface CheckResult {
  findings: string[];
  notes: string[];
  summary: CheckSummary;
  summaryLine: string;
}

// ---- comment stripping (line numbers must survive) -------------------------

/** Blanks every `/* … *\/` comment to spaces, keeping newlines so every line number
 *  computed afterwards still matches the ORIGINAL file. A `{`, `}` or `;` inside a
 *  comment (styles.css has plenty, in prose) would otherwise desync the brace
 *  tracking in scanDeclarations, or `var(--x)` inside a comment would count as a
 *  real use. */
export function stripCssComments(css: string): string {
  let out = "";
  let i = 0;
  while (i < css.length) {
    if (css.charAt(i) === "/" && css.charAt(i + 1) === "*") {
      const end = css.indexOf("*/", i + 2);
      const stop = end === -1 ? css.length : end + 2;
      for (let j = i; j < stop; j++) {
        out += css.charAt(j) === "\n" ? "\n" : " ";
      }
      i = stop;
    } else {
      out += css.charAt(i);
      i++;
    }
  }
  return out;
}

/** The .tsx equivalent. Block comments are the same shape as CSS's; line comments
 *  are stripped with a textual heuristic, not a real JS/TSX parser — this only
 *  has to find `var(--name` mentions in inline style strings, and a `//` that is
 *  legitimately part of a URL in a string (protected by requiring it not follow
 *  a `:`) can only ever suppress a comment-strip, never hide a real token use,
 *  since a genuine use is `var(--…` and that could not appear before the `//` on
 *  the same line without already having been seen. */
export function stripJsComments(text: string): string {
  const withoutBlocks = stripCssComments(text);
  return withoutBlocks
    .split("\n")
    .map((line) => {
      const idx = line.indexOf("//");
      if (idx === -1) return line;
      if (idx > 0 && line.charAt(idx - 1) === ":") return line; // e.g. https://
      return line.slice(0, idx);
    })
    .join("\n");
}

// ---- declaration scanning ---------------------------------------------------

const CUSTOM_PROP_RE = /^--([a-zA-Z0-9-]+)\s*:\s*(.+)$/;

/** A minimal, single-pass CSS declaration scanner. It does not build a CSS AST —
 *  it tracks brace nesting generically (a stack, not a fixed depth) so a
 *  declaration nested inside e.g. an `@media` block is attributed to the
 *  selector that actually encloses it, rather than assuming the flat shape
 *  tokens.css happens to use. For every `--name: value;` custom-property
 *  declaration found anywhere, it records the name, value, starting line, and
 *  the innermost enclosing selector (the top of the brace stack at that point).
 *  Run stripCssComments first — a `{`/`}`/`;` inside a comment would desync the
 *  brace stack, since this scanner has no idea comments exist. */
export function scanDeclarations(codeText: string): Declaration[] {
  const declarations: Declaration[] = [];
  const stack: string[] = [];
  let pending = "";
  let line = 1;
  let pendingStartLine = 1;

  for (let i = 0; i < codeText.length; i++) {
    const ch = codeText.charAt(i);
    if (pending.trim() === "") pendingStartLine = line;

    if (ch === "\n") {
      line++;
      pending += ch;
      continue;
    }
    if (ch === "{") {
      stack.push(pending.trim().replace(/\s+/g, " "));
      pending = "";
      continue;
    }
    if (ch === "}") {
      stack.pop();
      pending = "";
      continue;
    }
    if (ch === ";") {
      const decl = pending.trim();
      const m = CUSTOM_PROP_RE.exec(decl);
      if (m) {
        declarations.push({
          name: m[1]!,
          value: m[2]!.trim(),
          line: pendingStartLine,
          selector: stack[stack.length - 1] ?? "",
          depth: stack.length,
        });
      }
      pending = "";
      continue;
    }
    pending += ch;
  }

  return declarations;
}

const ROOT_SELECTOR = ":root";

export function themeOfSelector(selector: string): "light" | "dark" | null {
  const m = /^html\[data-theme=["'](light|dark)["']\]$/.exec(selector);
  return m ? (m[1] as "light" | "dark") : null;
}

export function densityOfSelector(selector: string): "comfortable" | "cozy" | "compact" | null {
  const m = /^html\[data-density=["'](comfortable|cozy|compact)["']\]$/.exec(selector);
  return m ? (m[1] as "comfortable" | "cozy" | "compact") : null;
}

/** The four shapes that make a declaration a TOKEN rather than an ordinary,
 *  component-scoped CSS custom property (see the file header). */
export function isTokenBlockSelector(selector: string): boolean {
  return selector === ROOT_SELECTOR || themeOfSelector(selector) !== null || densityOfSelector(selector) !== null;
}

/** A declaration counts as a global token definition only at depth 1 — the
 *  selector named directly, nothing else enclosing it. styles.css also has
 *  `@media (prefers-contrast: more) { html[data-theme="light"] { --border: …
 *  } }`, which reuses the theme selector shape at depth 2 to STRENGTHEN three
 *  already-defined tokens for a media feature, not to define new ones — the
 *  spec's move list (the #376 issue) never asked for this to move, and folding
 *  it into the base inventory would silently let the enhanced-contrast values
 *  stand in for the ones real users see by default. So it is deliberately
 *  invisible to this checker, the same way styles.css's OTHER kind of
 *  exception (a component-scoped variable, e.g. --tab-strip-h) is: neither
 *  flagged as a rogue redefinition nor merged into the inventory it would
 *  otherwise silently rewrite. */
export function isTopLevelTokenBlock(d: Declaration): boolean {
  return d.depth === 1 && isTokenBlockSelector(d.selector);
}

/** Builds the inventory from tokens.css's own text: every TOP-LEVEL declaration
 *  in one of the four recognised block shapes, bucketed accordingly. A
 *  declaration in some other selector, or one of these four shapes nested
 *  deeper than depth 1, is ignored here — see isTopLevelTokenBlock. */
export function buildInventory(tokensCssText: string): Inventory {
  const code = stripCssComments(tokensCssText);
  const inventory: Inventory = {
    root: new Map(),
    density: { comfortable: new Map(), cozy: new Map(), compact: new Map() },
    light: new Map(),
    dark: new Map(),
  };
  for (const d of scanDeclarations(code)) {
    if (!isTopLevelTokenBlock(d)) continue;
    const loc: TokenLocation = { value: d.value, line: d.line };
    if (d.selector === ROOT_SELECTOR) {
      inventory.root.set(d.name, loc);
      continue;
    }
    const theme = themeOfSelector(d.selector);
    if (theme) {
      inventory[theme].set(d.name, loc);
      continue;
    }
    const density = densityOfSelector(d.selector);
    if (density) {
      inventory.density[density].set(d.name, loc);
    }
  }
  return inventory;
}

export function globalNamesOf(inventory: Inventory): Set<string> {
  return new Set<string>([
    ...inventory.root.keys(),
    ...inventory.density.comfortable.keys(),
    ...inventory.density.cozy.keys(),
    ...inventory.density.compact.keys(),
    ...inventory.light.keys(),
    ...inventory.dark.keys(),
  ]);
}

// ---- var(--…) use scanning ---------------------------------------------------

const VAR_USE_RE = /var\(\s*--([a-zA-Z0-9-]+)/g;

/** Finds every `var(--name` occurrence in a file (a `*.css` file, or a `*.tsx`
 *  file's inline `style` strings — per #376 this is a plain textual scan of the
 *  whole file, not a JSX/CSS-in-JS parser, which is enough for the shape this
 *  codebase actually uses). Comments are stripped first so a mention inside one
 *  is never counted as a real use. */
export function findVarUses(file: FileText): Use[] {
  const code = file.path.endsWith(".tsx") ? stripJsComments(file.text) : stripCssComments(file.text);
  const uses: Use[] = [];
  const lines = code.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i] ?? "";
    VAR_USE_RE.lastIndex = 0;
    let m: RegExpExecArray | null;
    while ((m = VAR_USE_RE.exec(line))) {
      uses.push({ file: file.path, line: i + 1, name: m[1]! });
    }
  }
  return uses;
}

// ---- @contrast / @contrast-known-low directives -------------------------------

const PAIR_SHAPE = "[a-zA-Z0-9-]+/[a-zA-Z0-9-]+";
const CONTRAST_RE = new RegExp(`^\\s*/\\*\\s*@contrast\\s+(${PAIR_SHAPE})\\s*\\*/\\s*$`);
const KNOWN_LOW_RE = new RegExp(`^\\s*/\\*\\s*@contrast-known-low\\s+(light|dark)\\s+(${PAIR_SHAPE})\\s+([0-9.]+)`);

/** Reads `@contrast` and `@contrast-known-low` directives from tokens.css's RAW
 *  text — they live inside comments, which stripCssComments would blank out, so
 *  this runs BEFORE that and does its own line-oriented match instead. */
export function parseContrastDirectives(tokensCssRawText: string): {
  pairs: ContrastPairDirective[];
  knownLow: KnownLowDirective[];
} {
  const pairs: ContrastPairDirective[] = [];
  const knownLow: KnownLowDirective[] = [];
  const lines = tokensCssRawText.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i] ?? "";
    const kl = KNOWN_LOW_RE.exec(line);
    if (kl) {
      knownLow.push({ theme: kl[1] as "light" | "dark", pair: kl[2]!, ratio: Number(kl[3]), line: i + 1 });
      continue;
    }
    const c = CONTRAST_RE.exec(line);
    if (c) pairs.push({ pair: c[1]!, line: i + 1 });
  }
  return { pairs, knownLow };
}

function splitPair(pair: string): [string, string] | null {
  const parts = pair.split("/");
  if (parts.length !== 2) return null;
  const [a, b] = parts;
  if (!a || !b) return null;
  return [a, b];
}

// ---- contrast math (WCAG 2.x relative luminance) ------------------------------

const HEX_RE = /^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$/;

/** Hex only (`#rgb` / `#rrggbb`) — the check cannot reason about `var(--x)`,
 *  `rgb(...)`, named colours, etc., so a pair whose value is not plain hex is an
 *  error (see checkTokens below), not a silently-skipped pair. */
export function hexToRgb(hex: string): [number, number, number] | null {
  const m = HEX_RE.exec(hex.trim());
  if (!m) return null;
  let h = m[1]!;
  if (h.length === 3) h = h.split("").map((c) => c + c).join("");
  const n = parseInt(h, 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

function linearize(c: number): number {
  const v = c / 255;
  return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
}

export function relativeLuminance(hex: string): number | null {
  const rgb = hexToRgb(hex);
  if (!rgb) return null;
  const [r, g, b] = rgb;
  return 0.2126 * linearize(r) + 0.7152 * linearize(g) + 0.0722 * linearize(b);
}

/** WCAG 2.x contrast ratio, (L1 + 0.05) / (L2 + 0.05) with L1 the lighter of the
 *  two — ranges from 1 (identical) to 21 (black on white). AA text threshold is
 *  4.5:1, checked below. */
export function contrastRatio(hex1: string, hex2: string): number | null {
  const l1 = relativeLuminance(hex1);
  const l2 = relativeLuminance(hex2);
  if (l1 === null || l2 === null) return null;
  const lighter = Math.max(l1, l2);
  const darker = Math.min(l1, l2);
  return (lighter + 0.05) / (darker + 0.05);
}

const AA_THRESHOLD = 4.5;

// ---- the check -----------------------------------------------------------------

/** Runs every rule described in the file header against one tokens.css plus the
 *  rest of the tree (every other `*.css` and `*.tsx` file under web/src). Pure —
 *  takes file contents, returns findings/notes/summary; `check-tokens.mjs` is the
 *  only thing that touches the filesystem. */
export function checkTokens(tokensFile: FileText, otherFiles: FileText[]): CheckResult {
  const findings: string[] = [];
  const notes: string[] = [];

  const inventory = buildInventory(tokensFile.text);
  const globalNames = globalNamesOf(inventory);
  const { pairs, knownLow } = parseContrastDirectives(tokensFile.text);

  const cssFiles = otherFiles.filter((f) => f.path.endsWith(".css"));
  const tsxFiles = otherFiles.filter((f) => f.path.endsWith(".tsx"));

  // Every declaration in every OTHER css file: a rogue token if it sits in one of
  // the four recognised shapes, and always recorded as a "local name" so a
  // legitimate component-scoped var (e.g. --tab-strip-h) never reads as an
  // undefined use below.
  const localNames = new Set<string>();
  for (const file of cssFiles) {
    const code = stripCssComments(file.text);
    for (const d of scanDeclarations(code)) {
      localNames.add(d.name);
      if (isTopLevelTokenBlock(d)) {
        findings.push(
          `${file.path}:${d.line}: --${d.name} is defined in ${d.selector}, outside tokens.css` +
            " — every token lives in exactly one place (#376)",
        );
      }
    }
  }

  // Uses: every *.css (tokens.css included, so a token referencing another token
  // still counts) plus every *.tsx.
  const usedNames = new Set<string>();
  for (const file of [tokensFile, ...cssFiles, ...tsxFiles]) {
    for (const use of findVarUses(file)) {
      usedNames.add(use.name);
      if (!globalNames.has(use.name) && !localNames.has(use.name)) {
        findings.push(`${use.file}:${use.line}: var(--${use.name}) is used but never defined in tokens.css (#376)`);
      }
    }
  }

  // Theme symmetry: every name in either theme block must be in both.
  const themeNames = new Set<string>([...inventory.light.keys(), ...inventory.dark.keys()]);
  for (const name of themeNames) {
    const inLight = inventory.light.get(name);
    const inDark = inventory.dark.get(name);
    if (inLight && !inDark) {
      findings.push(
        `${tokensFile.path}:${inLight.line}: --${name} is defined for light but missing from the dark theme block (#376)`,
      );
    }
    if (inDark && !inLight) {
      findings.push(
        `${tokensFile.path}:${inDark.line}: --${name} is defined for dark but missing from the light theme block (#376)`,
      );
    }
  }

  // Contrast pairs, both themes.
  for (const { pair, line } of pairs) {
    const split = splitPair(pair);
    if (!split) {
      findings.push(`${tokensFile.path}:${line}: @contrast pair "${pair}" is not in fg/bg form (#376)`);
      continue;
    }
    const [fgName, bgName] = split;
    for (const theme of ["light", "dark"] as const) {
      const map = theme === "light" ? inventory.light : inventory.dark;
      const fg = map.get(fgName);
      const bg = map.get(bgName);
      if (!fg || !bg) {
        findings.push(
          `${tokensFile.path}:${line}: @contrast pair ${pair} references a token undefined in the ${theme} theme (#376)`,
        );
        continue;
      }
      if (!HEX_RE.test(fg.value.trim()) || !HEX_RE.test(bg.value.trim())) {
        findings.push(
          `${tokensFile.path}:${line}: @contrast pair ${pair} in ${theme} is not a plain hex colour` +
            ` (fg="${fg.value}" bg="${bg.value}") — cannot check contrast (#376)`,
        );
        continue;
      }
      const ratio = contrastRatio(fg.value.trim(), bg.value.trim())!;
      const isKnownLow = knownLow.some((k) => k.theme === theme && k.pair === pair);
      if (ratio < AA_THRESHOLD && !isKnownLow) {
        findings.push(
          `${tokensFile.path}:${line}: ${pair} is ${ratio.toFixed(2)}:1 in ${theme}` +
            ` — below the ${AA_THRESHOLD}:1 WCAG AA threshold (#376)`,
        );
      }
    }
  }

  // Known-low staleness: a listed pair that has since improved past 4.5 has to be
  // taken off the line, or the debt it declares is no longer true.
  for (const k of knownLow) {
    const split = splitPair(k.pair);
    if (!split) continue;
    const [fgName, bgName] = split;
    const map = k.theme === "light" ? inventory.light : inventory.dark;
    const fg = map.get(fgName);
    const bg = map.get(bgName);
    if (!fg || !bg || !HEX_RE.test(fg.value.trim()) || !HEX_RE.test(bg.value.trim())) continue;
    const ratio = contrastRatio(fg.value.trim(), bg.value.trim())!;
    if (ratio >= AA_THRESHOLD) {
      findings.push(
        `${tokensFile.path}:${k.line}: @contrast-known-low ${k.theme} ${k.pair} now measures ${ratio.toFixed(2)}:1` +
          " — it has passed 4.5:1, remove it from the known-low line (#376)",
      );
    }
  }

  // Notes: tokens defined but never used anywhere.
  for (const name of [...globalNames].sort()) {
    if (!usedNames.has(name)) notes.push(`--${name} is defined but not used anywhere (#376)`);
  }

  const usedCount = [...globalNames].filter((n) => usedNames.has(n)).length;
  const summary: CheckSummary = {
    definedCount: globalNames.size,
    usedCount,
    pairsPerTheme: pairs.length,
    notesCount: notes.length,
  };
  const summaryLine =
    `tokens: ${summary.definedCount} defined, ${summary.usedCount} used, ` +
    `${summary.pairsPerTheme} contrast pairs checked per theme, ${summary.notesCount} notes`;

  return { findings, notes, summary, summaryLine };
}
