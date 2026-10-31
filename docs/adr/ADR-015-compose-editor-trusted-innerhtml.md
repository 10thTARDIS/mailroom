# ADR-015: The rich-text compose editor is the one allowed `innerHTML` site

**Status:** Accepted (narrow, enforced exception to the no-`innerHTML` web
invariant; raised by rich-text compose)

## Context

A load-bearing web-security invariant (`docs/hostile-content.md`, enforced by
`TestWebNeverInjectsHTMLIntoOurOwnDOM` in `cmd/mailroom/security_test.go`) is:

> No web source may use `innerHTML` / `outerHTML` / `insertAdjacentHTML` /
> `dangerouslySetInnerHTML` / `document.write`. A message body is
> attacker-controlled and is rendered **only** in the sandboxed, opaque-origin
> message iframe — never injected into our own DOM.

The rule exists because injecting attacker HTML into our live (non-sandboxed)
DOM executes it: a `<script>` inserted via `innerHTML` does not run, but an
`<img onerror=…>` or `<svg onload=…>` **does**.

Rich-text compose (feature-matrix §6 "Rich-text (WYSIWYG) compose", v1.0)
is a `contentEditable` editor. Reading and seeding a `contentEditable` surface
is done through `innerHTML` — there is no equivalent API. So rich-text compose cannot satisfy
the blunt, grep-based ban as written, even though what it handles is **not**
attacker-controlled message HTML.

## Decision

`innerHTML` is permitted in **exactly one file** — `web/src/app/ui/Compose.tsx`
— on **exactly the lines that carry a `trusted-html:` marker comment**, each
justified below. The message-**rendering** path is untouched and the ban stands
everywhere else. The forcing test enforces both the file restriction and the
**exact count** (currently 3): a new use fails the build until it is marked here
and this ADR is extended; a removed one must drop the count.

The three permitted uses, and why each is safe:

1. **`tpl.innerHTML = html`** (`htmlToText`) — writes into a **detached
   `<template>`**. `<template>` content is inert: parsed into a document
   fragment but never rendered, so no resource loads, no `onerror`/`onload`, no
   script execution. Used only to walk the fragment and extract plain text. The
   `html` is the user's own composed content in any case.
2. **`ref.current.innerHTML = initialHTML`** (editor mount seed) — `initialHTML`
   is either empty or `quotedHtml(parent)`, which is built from the parent
   message's **plain-text** body, **HTML-escaped** (`escapeHtml`) and wrapped in
   a `<blockquote>`. It never contains the parent's HTML, so no attacker markup
   reaches the DOM.
3. **`onChange(ref.current.innerHTML)`** (read-back) — **reads** the user's own
   authored content out of the editor. It injects nothing, and the value is
   sanitized server-side (`SanitizeOutboundHTML`, `internal/api`) before it is
   ever built into a message.

**Paste is forced to plain text.** The one way attacker HTML could otherwise
reach the editor is a paste from a malicious page. The editor's `onPaste`
handler cancels the default and inserts `text/plain` only (`insertText`), so
pasted markup never enters the DOM. (Rich paste would require a client-side HTML
sanitizer we deliberately do not ship; it is a possible future enhancement.)

**Defence in depth.** Independently of all the above, every outgoing HTML body
is run through `SanitizeOutboundHTML` at the API boundary (`internal/api/
draft.go`), so even a hypothetical bypass cannot send unsafe HTML to a recipient.

## Consequences

- The invariant is now "no `innerHTML` **except** the compose editor's marked,
  counted, individually-justified uses, and never in message rendering" — a
  smaller, explicit surface rather than an absolute the editor silently breaks.
- Adding an `innerHTML` use anywhere else, or an unmarked/uncounted one in
  Compose.tsx, is still a red build.
- Paste loses source formatting (plain text only). Acceptable for v1.0; rich
  paste is deferred behind a client sanitizer.
- If a future change makes the editor seed or handle real remote/HTML content,
  this ADR and the marker justifications must be revisited — the safety rests on
  the specific facts above (escaped plain-text quotes, inert template, read-only
  self-content, plain-text paste), not on the editor being "trusted" in general.

## Alternatives considered

- **Re-implement the editor without `innerHTML`.** Would mean hand-serialising
  the `contentEditable` DOM — reinventing `innerHTML` less safely — or shipping a
  much more limited editor. No security gain over the marked-and-proven approach.
- **Weaken the tripwire to a plain file allowlist.** Rejected: a whole-file
  allowlist would let an unsafe `innerHTML` slip into Compose.tsx unreviewed. The
  per-line marker + pinned count keeps every use individually gated.
