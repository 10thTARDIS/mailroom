// TypeScript 7.0's new default checking of side-effect imports (TS2882) can no
// longer resolve `import "./foo.css"` without an ambient module declaration.
declare module "*.css";

// `?raw` gives a stylesheet to a TEST as text (#514): prefs.readingComfort.test.ts
// reads tokens.css and asserts the five `html[data-text-size]` font sizes match
// prefs.ts's TEXT_SIZE_PX, because two lists of five numbers in two languages is
// the shape that drifts. Vite's own suffix, so vitest resolves it through the
// same transform the build uses — no filesystem access and no @types/node.
declare module "*.css?raw" {
  const css: string;
  export default css;
}

// The same trick for a COMPONENT's source (#602): outbox.test.ts asserts that
// Sidebar.tsx computes the Outbox count with `outboxCount` rather than carrying
// its own copy of the rule. It had a copy, the copy drifted, and the Outbox
// became unreachable at exactly the moment a send failed. With no jsdom in this
// repo the component cannot be rendered and asked, so the invariant is asserted
// over its text — the same reason internal/config/envdoc_test.go parses Go
// source rather than trusting a convention.
declare module "*.tsx?raw" {
  const source: string;
  export default source;
}

// And for a DOCUMENT (#49): settings/apikeys/setupDocs.test.ts reads
// docs/mcp.md and asserts the setup snippets and the tool table the Settings
// page shows are the document's, so the two cannot drift apart.
declare module "*.md?raw" {
  const text: string;
  export default text;
}

// And for GO SOURCE (#801): messageDocument.reporter.test.ts reads
// cmd/mailroom/main.go to assert the app shell policy pins the same reporter
// hash the client computes, the one pin that is duplicated by hand.
declare module "*.go?raw" {
  const source: string;
  export default source;
}

// And for a PAGE THE SERVER RENDERS (ADR-050): settings/apikeys/consentPage.test.ts
// runs axe over the OAuth consent and error pages exactly as Go renders them
// (internal/oauthserver/testdata, which the Go tests keep byte-identical to the
// handler's output), because those pages are not part of this app.
declare module "*.html?raw" {
  const html: string;
  export default html;
}

// And for the LOCKFILE and package.json (ADR-051 §2): the editor's supply-chain
// test (core/supplyChain/checkLock.test.ts) checks what the build would actually
// install, which is only knowable from those files.
declare module "*.json?raw" {
  const text: string;
  export default text;
}

// And for EVERY component's source at once (#23): ui/modalEscape.source.test.ts
// scans each .tsx with a modal backdrop and asserts its Escape handler stops the
// keystroke, so a modal added later is checked without being listed. Vite's own
// glob, resolved by vitest through the same transform; no @types/node.
interface ImportMeta {
  glob(
    patterns: string | string[],
    options: { query: "?raw"; import: "default"; eager: true },
  ): Record<string, string>;
}
