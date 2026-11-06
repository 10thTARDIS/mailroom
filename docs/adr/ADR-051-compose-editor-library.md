# ADR-051: The compose editor moves to Tiptap (ProseMirror), with rich paste parsed into its schema

**Status:** Accepted (v1.101.0; #82, #83). Amends ADR-015, which it partly
supersedes (§4). The operator's decisions (2026-11-06): Tiptap 3 (§1), and paste
keeps formatting, with Mod-Shift-V for plain text (§4).

## Context

The composer's rich-text editor is a dependency-free `contentEditable` with an
`execCommand` toolbar (`web/src/app/ui/Compose.tsx`). It offers bold, italic,
underline, two kinds of list, links and inline pictures. #82 asks for a full
mail-editor toolset:

- fonts, font size, text colour, highlight colour;
- strike-through, subscript, superscript;
- nested lists with indent and outdent, paragraph indentation;
- block quotes, inline code, code blocks;
- tables;
- undo and redo.

`execCommand` is deprecated, has no table support, and its undo stack breaks as
soon as the page changes the DOM itself. The operator chose (2026-11-04, on #82)
to move to a maintained editor library, provided security is kept. The conditions
are recorded on #82:

- the server stays the trust boundary;
- paste is sanitised in the editor;
- no CSP relaxation;
- no remote loading or telemetry;
- an AGPL-compatible supply chain;
- links and pictures follow today's rules;
- the editor is accessible;
- the output is mail-client HTML.

Two existing facts frame the choice:

1. **The shell's CSP** (`cmd/mailroom/main.go`, `appCSP`) is strict for scripts
   (`script-src 'self'` plus one pinned hash: no inline script and no `eval`) but
   already permits inline styles (`style-src 'self' 'unsafe-inline'`), because
   rendered mail needs them. An editor that injects a `<style>` element or sets
   `style` attributes therefore needs no policy change. One that needs `eval`,
   `new Function` or an inline script is excluded.
2. **ADR-015** makes the compose editor the one allowed `innerHTML` site. It holds
   only because of three facts:
   - the editor is seeded with an escaped plain-text reply quote, never the
     parent's HTML;
   - it reads back only the user's own content;
   - paste is plain text only, "deferred behind a client sanitizer".

## Decision

### 1. Tiptap 3, open-source packages only

The editor is **Tiptap 3** (`@tiptap/*`, MIT), on ProseMirror (MIT). We use only
its open-source packages:

- `@tiptap/core`, `@tiptap/pm`, `@tiptap/react` and `@tiptap/starter-kit`;
- `@tiptap/extension-table` (TableKit);
- `@tiptap/extension-text-style` (text style, colour, font family, font size and
  background colour);
- `@tiptap/extension-highlight`, `@tiptap/extension-subscript`,
  `@tiptap/extension-superscript`, `@tiptap/extension-image`,
  `@tiptap/extension-text-align` and `@tiptap/extension-file-handler`.

Paragraph indentation has no official extension, so it is a small extension of
our own in `web/src/app/ui/editor/`.

Why it wins (survey of 2026-11-06, against the #82 conditions):

| | Tiptap 3 | Lexical | ProseMirror directly | TinyMCE 8 / CKEditor 5 | Quill 2 |
|---|---|---|---|---|---|
| Licence of everything used | MIT | MIT | MIT | GPL-2.0-or-later (compatible) | BSD-3 |
| Script CSP (no eval/inline) | yes | yes | yes | yes | yes (ESM build) |
| Network calls / telemetry | none found | none | none | local licence check; CKEditor shows a mandatory logo | none |
| Pasted HTML parsed into a schema | yes, in an inert document | yes | yes | yes | yes |
| Every #82 feature, open source | all but paragraph indent | no image node; reading HTML back drops colour, size and font | all, but each written by us | yes | tables experimental |
| Maintenance | very active, SECURITY.md | active, still 0.x, no SECURITY.md found | active, no SECURITY.md | active; many XSS advisories in 2026 | last release 2024-11 |
| Size (gzip, our feature set) | ~150 KB | ~164 KB without images | ≤ Tiptap, not measured | ~330–450 KB | ~60 KB |

- **Lexical:** the second choice. It would cost us an image node and custom
  importers so that replies, drafts and forwards keep their colours and fonts.
- **ProseMirror directly:** the same security base as Tiptap, with every mark,
  node and command written by us.
- **TinyMCE and CKEditor:** heavier, with more advisories. CKEditor's free licence
  also shows a "Powered by" logo.
- **Quill:** unmaintained.

### 2. The security conditions, as enforced rules

- **The server is still the boundary.** Every body the editor produces is sent
  through `SanitizeOutboundHTML` before it is saved or transmitted, exactly as
  today. The editor's schema is a convenience and never the control.
- **No Pro or private-registry package, and a pinned supply chain.**
  - Dependencies are exact versions in `package-lock.json`.
  - A web test lists the `@tiptap/*` and `prosemirror-*` packages the build may
    contain. It fails on any other `@tiptap` package, on any `@tiptap-pro` scope,
    and on a registry other than `registry.npmjs.org` in the lockfile.
  - The minimum version is the first one with every 2026 advisory fixed (3.30.4
    at the time of writing). Renovate or Dependabot keeps it current, and
    `npm audit` is part of the existing dependency checks.
- **No runtime style injection that we don't ship.** `injectCSS: false`. The
  editor's few required rules (ProseMirror's base CSS) come in our own static
  stylesheet. (The policy would allow injection; we turn it off so the styles are
  ours and reviewable.)
- **No eval, no network.** A build check fails if the bundled editor code contains
  `eval(`, `new Function` or `importScripts`. The existing CSP and the
  `connect-src 'self'` policy are the run-time backstop.
- **Links:** the Link extension's `isAllowedUri` permits only `http`, `https` and
  `mailto`. Today's link rules for `rel` and target are applied when sent.
- **Pictures stay attachments.** The image button and pasted or dropped image
  files go through today's upload path and are inserted as `cid:` references.
  Pasted HTML that names a remote `<img src="https://…">` is not hot-linked: the
  image is dropped on paste and the user is told why.
- **Accessibility:**
  - the toolbar is ours: a `role="toolbar"` with named buttons, roving focus and
    keyboard shortcuts listed in their titles;
  - the editing surface carries `aria-label`, `aria-multiline` and the composer's
    label;
  - the existing axe checks run over the composer in light and dark, at desktop
    and phone widths.

### 3. What is sent: mail-client HTML

The editor's `getHTML()` is the source. A small serializer we own turns it into
mail-client HTML before the draft is saved:

- inline styles only, no classes;
- tables with explicit borders and cell padding;
- lists with standard nesting;
- `<span style>` for colour, size and font, with highlight as a
  `background-color`;
- indentation as `margin-left` on block elements.

Its tests, together with the server sanitiser's, run every #82 format through the
full round trip: composer → `SanitizeOutboundHTML` → MIME → the reader's sanitiser
→ the rendered message. Each format must survive each step. Drafts reopen with
their formatting intact, because Tiptap parses inline `color`, `font-size` and
`font-family` back into its marks.

The plain-text toggle keeps working. Rich to plain degrades each format sensibly:
tables become aligned text, quotes get `>`, code is kept verbatim, and lists keep
their markers.

### 4. ADR-015 amended: rich paste, parsed in an inert document

- **Paste becomes rich, safely.** Tiptap parses pasted HTML with ProseMirror's
  `DOMParser` in a document created by `document.implementation.createHTMLDocument`.
  That document is inert: scripts don't run, event handlers don't fire, nothing
  loads. The parser keeps only what the editor's schema defines, so unknown
  elements and attributes, event handlers and disallowed URL schemes are dropped
  before anything reaches the live page.
  - We additionally strip remote images on paste (§2).
  - We keep a "Paste as plain text" shortcut (Mod-Shift-V).
  - A hostile-paste test suite pastes the cases ADR-015 feared (`<img onerror>`,
    `<svg onload>`, `javascript:` and `data:` links, `<iframe>`, `<style>`, form
    controls, CSS `url()` in style attributes) and asserts that nothing executes
    or loads, and that the resulting document holds none of them.
- **Reply quotes stay as today: escaped plain text, never the parent's HTML.**
  Quoting a message with its formatting would be a separate decision.
- **Compose.tsx no longer uses `innerHTML`.** The editor is seeded through
  `setContent` (a schema parse) and read through `getHTML()` (a serializer).
  ADR-015's exception is retired, and the web build's no-`innerHTML` check applies
  to Compose.tsx like every other file. The check scans our source, not
  `node_modules`; the editor's own internal DOM handling is covered by the
  hostile-paste suite and the server sanitiser, not by that check.

### 5. The composer layout (#83)

The body fills the composer between the fields and the attachment and action bar,
at a fixed size, and scrolls inside itself. The toolbar stays at the top of the
body. The attachment area and Send/Discard stay visible. This holds in rich and
plain mode and at phone width. It lands with the new editor, because both change
the same layout.

### 6. Delivery (operator's choice: betas per step)

1. **Beta 1:** Tiptap with exactly today's features and #83. Nothing is gained or
   lost, so any regression from the switch shows on its own. It includes:
   - the supply-chain, no-eval and hostile-paste tests;
   - the browser CSP check: the composer loaded with a draft holding `style`
     spans raises no CSP violation, verified with a real browser;
   - rich paste, which is part of the change.
2. **Beta 2:** the new formatting: fonts, size, text and highlight colour,
   strike-through, subscript and superscript, quotes, code and code blocks,
   paragraph and list indentation, and undo/redo in the toolbar.
3. **Beta 3:** tables, and the full round-trip suite for every format.

#43, #47 and #35 ride in whichever beta has room.

**As released:** the steps shipped as v1.101.0-beta.1 (step 1), beta.3 (step 2,
alongside format=flowed sending, #93) and beta.4 (step 3), with #43, #47 and the
#35 report in beta.2. #35's display change was measured and not built.

## Consequences

- **Cost:** about 150 KB gzip of editor code. It is loaded only when the composer
  opens (a separate chunk), so the reading views are unaffected.
- **Maintenance:** a fast-moving dependency with occasional core advisories.
  Keeping it current is part of routine dependency maintenance.
- **Vendor risk:** a commercial company could move a feature we use into its paid
  tier. Everything we use is MIT on npm today. If that happened, we would pin the
  last MIT version and could fall back to ProseMirror directly, which Tiptap is
  built on, without changing the stored format, which is HTML.
- **ADR-015** is partly superseded (§4). Its plain-text reply quoting remains in
  force.

## Alternatives considered

The table in §1, and keeping and extending the `execCommand` editor, which was
rejected on #82: no tables, a deprecated API, and an undo stack that breaks as
soon as the page changes the DOM itself.

## References

#82, #83 · ADR-015, ADR-042 · `cmd/mailroom/main.go` (`appCSP`),
`internal/api/sanitize.go`, `internal/api/sanitize_css.go` · Tiptap docs and
security advisories (GHSA-cp6q-959q-f8rh and others, fixed by 3.30.4+)
