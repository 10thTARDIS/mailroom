# Hostile content: the threat model, and what is (and is not) defended

**Status:** implemented, Forgejo #39. Authoritative spec: `docs/mailroom-spec.md`
§11; feature matrix §2 and §8.

If you are about to touch the message render path, the attachment download path,
or the app shell's headers — **read this first**. It is the one place that says
*why* the iframe is there, and every control below is pinned by a test that will
fail loudly if you relax it.

---

## 1. The threat model

> **Every message body is attacker-controlled input, authored by a stranger, and
> rendered inside the operator's own origin, next to their mail.**

That sentence is the whole problem. A phishing email is not a document we
received — it is *code somebody else wrote*, which we have agreed to execute in a
browser tab that also holds a live session cookie for the user's entire mailbox.
A single mistake turns "you got a spam" into stored XSS with full read/write
access to years of mail, the ability to send as the user, and the ability to
exfiltrate everything quietly.

The specific adversaries, in the order they matter:

| # | Attacker goal | The move |
|---|---|---|
| 1 | **Stored XSS on our origin** | `<script>`, `on*=` handlers, `<svg><script>`, `javascript:` URLs, mutation-XSS against a sanitizer, an HTML/SVG *attachment* served inline |
| 2 | **Tracking / read receipts** | A 1×1 remote `<img>`, a CSS `background:url()`, `@font-face`, `srcset`, `<link rel=preconnect>`, `<meta http-equiv=refresh>` |
| 3 | **Credential phishing** | A `<form>` that posts offsite; a link that looks like ours |
| 3a | **Lying about *who sent it*** | A display name that is itself an address (`"security@bank.example" <attacker@evil.example>`); a `U+202E` in a name that reorders the address rendered beside it — §2.3b. The `From` header is unauthenticated, so this is bounded rather than closed until #55 |
| 4 | **Clickjacking / UI redress** | Framing our app from an attacker's page |
| 5 | **Header injection / response splitting** | A filename or a subject reflected into a response header |
| 6 | **Network interception** | Cleartext IMAP/SMTP; a stripped `STARTTLS`; an unverified certificate |

## 2. The defence, in layers

### 2.1 The render path (threats 1, 2, 3)

There are **two independent layers**, and they are deliberately different *kinds*
of thing, so that a mistake in one is not a mistake in both.

**Layer 1 — the browser's origin isolation (primary).**
`web/src/app/ui/messageDocument.ts`. An HTML body is **never** injected into our
DOM. It goes into a **sandboxed iframe**:

```
sandbox="allow-popups allow-popups-to-escape-sandbox allow-scripts"
```

`allow-same-origin` is absent — and it must **never** be added: the document has
an **opaque origin**, so it cannot read our cookies, our `localStorage`, our DOM
or our session. `allow-scripts` **is** present (Forgejo #100, for content-fit
auto-height, below), and it is safe only because `allow-same-origin` is not:
`allow-scripts` + `allow-same-origin` *together* would additionally let the frame
delete its own `sandbox` attribute. We take one precisely because we never take
the other — a script in this frame has no origin to abuse.

> **One narrow exception to "never injected into our DOM": the rich-text
> compose editor** (`web/src/app/ui/Compose.tsx`). A `contentEditable` editor is
> read/seeded via `innerHTML`, but what it touches is not attacker message HTML:
> reply quotes are the parent's *plain text*, HTML-escaped; the html→text step
> uses an inert `<template>`; reads return the user's own content (re-sanitized
> server-side on send); and paste is forced to plain text. Each use carries a
> `trusted-html:` marker and the forcing test pins their file and count. See
> `docs/adr/ADR-015`. The message-**rendering** path above is unaffected.

**Content-fit auto-height, and why `allow-scripts` is not a hole (Forgejo #100).**
A parent cannot measure an opaque-origin iframe's content, so sizing the frame to
its content needs a script *inside* the frame to `postMessage` its height out. The
frame therefore runs **exactly one script — ours** — and nothing else can, held
shut by three independent facts that all hold at once:

1. **No same-origin.** The frame is opaque (above). The reporter can `postMessage`
   a number and do nothing else: no cookies, no parent DOM, no network.
2. **CSP hash-pins the one script.** `script-src 'sha256-<reporter>'` names the
   reporter's exact bytes — in the message document's own CSP **and** in the
   app-shell CSP it inherits (they intersect; see §2.5). The message's own
   `<script>`, every inline `on*=` handler, and every `javascript:` URL are a hash
   mismatch and are blocked by *both* policies. (The app-shell hash is not
   optional: without it the intersection with the shell bans the reporter too and
   auto-height silently dies — proven in `scripts/hostile-content-e2e.sh`.)
3. **The sanitizer already stripped the body's scripts** (Layer 2). So even the
   hash check is a backstop, not the first line.

Remove any one of the three and `allow-scripts` becomes a hole. The reporter is
the last node in `<body>`, posts `{type:"mailroom-height", height, width}` (its content height and natural width; the width lets a phone scale a wide newsletter to fit, #801) on load and on
a `ResizeObserver`, and the parent (`ThreadPane`'s `HtmlBody`) accepts it **only**
when `event.source === iframe.contentWindow`, validates the shape, and **clamps**
the values — so a pathological message cannot make a mile-tall frame; past the cap
it keeps its own scrollbar.

The message document also carries its own CSP:

```
default-src 'none'; script-src 'sha256-<reporter>'; img-src data:;
style-src 'unsafe-inline'; font-src data:; form-action 'none'; base-uri 'none'
```

`script-src` is stated (not left to `default-src`) so the single reporter can run
and nothing else; `form-action` and `base-uri` are stated because **they do not
fall back to `default-src`** — a policy that assumes they do has holes in it.

**Layer 2 — the server sanitizer (`internal/api/sanitize.go`).**
Every `text/html` body value leaves the server having been **parsed** (with
`golang.org/x/net/html`, the browser's tree-construction algorithm),
**filtered** against an element/attribute allowlist, and **re-serialized**. What
we emit is a rendering of a tree we fully inspected, not a substring of the
attacker's bytes — which is what closes the mutation-XSS gap that string-matching
sanitizers live in.

**CSS gets the same treatment, and did not always** (`internal/api/sanitize_css.go`,
Forgejo #297). A `style=""` value and a `<style>` body are **tokenized** with a
CSS Syntax Level 3 tokenizer, **filtered** against an allowlist of *properties*
(plus an allowlist of value functions, selector functions and at-rules), and
**re-serialized canonically** — every name emitted comes from our own tables and
every identifier is re-escaped. Until #297 this half was a regular expression, and
§2.3 records exactly what that cost.

It hooks in at exactly one place: `bodyValues` in `internal/api/body.go`, which is
the one door an HTML body leaves through (`Email/get`).

### 2.2 Why *both* — and the two holes a CSP cannot close

Layer 1 is strong, and it is the one to trust. But it is **not sufficient**, and
this is the part that is easy to get wrong:

1. **`<meta http-equiv="refresh" content="0;url=https://tracker/…">`**
   A sandboxed iframe may navigate **itself** — the sandbox flags gate only *top*
   navigation. And **no CSP directive governs a document navigating itself**
   (`navigate-to` was dropped from CSP3 and never shipped). So a meta refresh in a
   message body is a *working* tracking beacon and a *working* phishing redirect
   through the strictest CSP that can be written.

2. **`<link rel="dns-prefetch">` / `rel="preconnect">`**
   These perform DNS resolution and a TCP+TLS connect. They fetch no *resource*,
   so **no CSP directive applies to them at all**. A per-recipient hostname
   (`<token>.tracker.example`) makes DNS itself the read receipt.

Neither is hypothetical. Neither is stopped by `default-src 'none'`. **The server
sanitizer is the only place they can be removed** — which is the answer to
"is a server-side sanitizer redundant given the iframe?". It is not.

And separately: layer 1 is a property of *one client*. A JMAP client that is not
a browser (aerc, the future MCP surface, an export) has no iframe to hide behind.
Spec §11 says "server **+** client" for exactly this reason.

### 2.3 Remote content (threat 2)

**Blocked by default. This is the 1.0 behaviour** (spec §11, feature matrix §2/§8).
The SSRF-guarded image proxy shipped in 1.x (ADR-026; `internal/api/proxy.go`)
and is **on by default** (`MAILROOM_IMAGE_PROXY`, `config.DefaultImageProxy`).
When a user unblocks remote content, the fetch is made by the server rather than
the reader's browser, so it is `internal/egress`'s SSRF guard that matters on
that path — see `internal/api/proxy.go` for what it fetches and why.
For senders the proxy's own rule allows, the same fetch also runs when mail
ARRIVES (`MAILROOM_IMAGE_PROXY_PREFETCH`, ADR-026 amendment,
`internal/api/proxy_prefetch.go`): the same image list, beacons excluded, through
the same guard, so it cannot fetch anything an open would refuse.

The server strips every fetching attribute. A remote `<img>` is not deleted — it
is *parked*:

```html
<img data-mailroom-remote-src="https://tracker.example/p.gif?id=…" data-mailroom-blocked="">
```

The client sees the `data-mailroom-blocked` marker, shows a banner, and offers
**"Show remote content"** — off by default. What the click does depends on the
image proxy:

- **Proxy on (the default).** The parked value is a same-origin `/jmap/proxy/…`
  path, not the sender's URL. The *app* fetches each one, with the session
  cookie, and gives the frame a `data:` URL (ADR-026, amendment).
  The frame fetches nothing: its policy stays `img-src data:` and no parked URL
  is restored into it. It could not fetch them anyway. Its origin is opaque, so
  the browser withholds the `SameSite=Lax` session cookie from any request it
  makes, and the proxy refuses an unauthenticated request (Forgejo #667). The
  banner says the fetch goes through the server, so the sender learns you opened
  the mail but not the reader's browser or location, and it reports any image
  that could not be loaded.
- **Proxy off.** The click restores `src` and relaxes the message CSP's `img-src`
  to `data: https:`. The banner says the fetch goes straight to the sender's
  server.

#### 2.3a Remembering that decision, per sender (Forgejo #202)

Until 1.14.0 the allow was **per message, per open, and never persisted**, on the
reasoning that *"I trusted this one newsletter once" must not become "this sender
may track me forever"*. That reasoning is right about a **blanket** allow and it is
still enforced. What it left missing was anywhere to record a **narrow** decision,
so the only two answers available were "ask every single time" and "stop asking" —
and asking every time about the same newsletter is how a safe default becomes
something a user routes around. A default that gets worked around protects nobody.

So a decision can now be remembered, and the whole design is about keeping it
narrow and visible:

- **Two matchers, no third.** `app.remote_allows.kind` is `address` (exactly this
  From) or `domain` (anyone at example.com), CHECKed in the schema. There is **no
  wildcard value and no `kind='all'`** — the schema cannot express a blanket allow,
  so no future UI can offer one. The store additionally refuses `*`,
  `*.example.com`, a leading or trailing dot, a domain with no dot, an address in a
  domain grant and vice versa. Domain is the broader grant and the UI says so **at
  the point of granting**, not only in a settings page.
- **Every grant is listed and revocable**, in Settings → "Senders whose images
  load", and from the banner on any message it affects. This is not a convenience:
  an "always allow" that cannot be seen or withdrawn *is* the permanent tracking
  consent the original decision guarded against, merely moved out of a click and
  into a database. **If that surface ever regresses, the feature is broken, not
  merely less nice.**
- **The per-message override still works both ways.** One message can be opened
  without images without deleting a grant, and a one-off allow still records
  nothing.
- **It syncs** (`RemoteAllow` is a JMAP object with a `change_log` stream), because
  a privacy decision that had to be remade in every browser would be remade
  carelessly.

> **⚠ THE GRANT IS KEYED ON AN UNAUTHENTICATED `From` (Forgejo #307).** Everything
> above is about keeping the grant *narrow*, and that half is solid — the schema
> cannot express a wildcard, the store refuses anything wider than one address or
> one domain, and every grant is listed and revocable. What none of it establishes
> is that the sender **is who the `From` header says**.
>
> `From:` is unauthenticated by construction. Mailroom does not verify DKIM, SPF or
> DMARC on the read path today (#55), so `matchingRemoteAllow` compares a remembered
> value against a string the sender chose. The consequence is exact and bounded:
> **anyone who can forge `From: news@publisher.example` inherits a grant the user
> made for that publisher, and their remote images load without a banner.** They
> gain the read receipt and the IP; they gain no script, no origin and no access to
> anything else, because the server still parks every remote URL unconditionally
> and the frame is still opaque.
>
> This is a **stated boundary, not an oversight**, and it is the reason the grant is
> deliberately not a wider mechanism: it decides only what to *un-park*, so a forged
> `From` buys exactly the thing the user already chose to give the real sender.
> Closing it properly means an authenticated `From` — DMARC/ARC verification, #55 —
> at which point `RemoteAllow` should match on the *verified* identity rather than
> the header. Hardening the store further would not help: **the scope is not the
> weak part, the identity is.**

**THE SANITIZER DID NOT CHANGE, AND MUST NOT.** `internal/api/sanitize.go` still
parks every remote URL unconditionally; it takes no branch on any preference. A
sanitizer with a preference branch stops being the only path, and everything else
in this document rests on it having exactly one. The allow list is read **on the
client**, and all it decides is what to un-park from output the server already made
inert — so the worst a bug in it can do is load images the user did not ask for,
never execute anything. That is why this feature could be added without touching a
single line of the render path.

**Not restorable, ever:** CSS `url()` and `@import`, `<link>`, `background=`,
`poster=`, `<video>`/`<audio>`/`<source>`, `<object>`/`<embed>`. Their only use in
a mail body is tracking. A decorative CSS background is not worth a channel.
Unlike a blocked `<img>`, these are **not parked** in a `data-mailroom-*`
attribute — the URL is *gone from the bytes*, so there is nothing for a client to
restore and nothing for a future `img-src` widening to turn back on.

> **⚠ For CSS that sentence was false until Forgejo #297, and the way it was false
> is worth keeping.** The CSS filter was a regular expression looking for the
> literal bytes `url(`, `@import`, `expression(`. CSS Syntax §4.3.1 decodes an
> identifier's escapes **before** anything compares it, so
> `\75 rl(https://tracker.example/x.png)` is an ordinary `<url-token>` to every
> browser and contains none of those bytes; `image-set()` was simply not on the
> list. Measured in the pinned Chromium image, in a real `srcdoc` frame with the
> real sandbox tokens and the exact `messageCSP()`, four such payloads reached the
> tracker once `img-src` was widened by a §2.3a grant.
>
> The consequence was worse than a surviving tracker, and it is the part to
> remember: an escaped-CSS-only body carries **no `data-mailroom-blocked`
> marker**, because nothing was parked. So `hasBlockedRemoteContent()` is false
> and **neither banner renders** — not "remote content blocked", not "images
> loaded because of a remembered decision". For a sender the user had ever
> allowed, that is a silent per-recipient beacon through *both* layers.
>
> The CSS filter is now an **allowlist of properties** over a real CSS tokenizer
> (`internal/api/sanitize_css.go`): escapes are decoded once, by the browser's
> rules, and a declaration survives only if its *property* is on the list and
> every token of its *value* is. `background-image`, `list-style-image`,
> `border-image-*`, `cursor`, `content`, `filter`, `mask*`, `src`, `behavior`,
> `-moz-binding` and every custom property are absent, so it does not matter how
> a value spells `url(`. Re-measured after the fix in the same harness: every
> payload `FETCHED=false`, with the unsanitized form of each still
> `FETCHED=true` as the positive control.
>
> A message whose *only* remote content was in CSS therefore still shows no
> banner — and that is now **correct**, because there is genuinely nothing left
> to load. Before the fix the same silence meant the opposite.

> **Note on `img-src https:` in the app shell's CSP.** A `srcdoc` iframe
> **inherits its embedder's CSP** (CSP3 §7.4, policy-container inheritance for
> local-scheme documents) and the two policies **intersect**. So the shell's
> policy is *also* enforced inside the message — which is why the message can run
> no script *except* the one hash-pinned height reporter that **both** policies
> name (`script-src 'self'` matches nothing in an opaque origin; the shared
> `'sha256-<reporter>'` is the only source that fits both — §2.1/§2.5) — and it
> is why the shell's `img-src` must carry `https:`: without that headroom the
> intersection would still be empty and "Show remote content" would be a button
> that does nothing. The cost is stated plainly in `cmd/mailroom/main.go`.
>
> **⚠ Since Forgejo #667 the frame no longer loads proxied images at all**, so
> with the proxy on the message policy stays `img-src data:` and the paragraph
> below describes headroom that only the proxy-off path still uses. The two
> policies are unchanged, and are kept as written so that path keeps working.
>
> **And since the image proxy (ADR-026), the app's own origin.** An allowed image
> is no longer the sender's `https://` URL but a same-origin `/jmap/proxy/...`
> path, which resolves against the app's base URL inside the frame. On an
> `https://` deployment `https:` still covers it; on a plain-`http://` one nothing
> did — `'self'` matches nothing in the opaque origin and bare `http:` is refused
> on purpose — so every allowed image was silently blocked by the browser, with
> no server-side trace (Forgejo #460). Both policies now name the deployment's
> *one* explicit origin when remote content is allowed: the message CSP gets
> `window.location.origin`, and the shell CSP gets the origin of the configured
> OIDC redirect URL when that is `http://` (`appCSPFor` in `main.go`). Neither
> ever admits the `http:` scheme itself.

#### 2.3b Sender-authored text in *our* chrome (Forgejo #307)

Everything above §2.3 is about the message **body**, and the body is defended by
being put somewhere it cannot reach us. Three strings are **not** in the frame and
cannot be: the **sender name**, the **subject** and the **preview**. They are React
text nodes in the app's own DOM — the list row, the reading pane's header, the
rule-preview sample list — because that is what a mail client is.

React escapes them, so they cannot inject markup. What they can still do is **lie**,
and two of the ways are mechanical rather than rhetorical:

- **A display name that is itself an address.** `From: "security@bank.example"
  <attacker@evil.example>`. A list that shows the display name alone renders
  `security@bank.example`, which is a true-looking statement of an identity this
  message does not have. Nothing is escaped wrongly.
- **A bidirectional control character.** `U+202E` RIGHT-TO-LEFT OVERRIDE and its
  family (`U+202A`–`U+202E`, `U+2066`–`U+2069`) do not reorder *themselves* — they
  reorder everything **after** them in the same bidi paragraph, until a matching pop
  or the end of the paragraph. In the reading pane `.message-from` and
  `.message-address` are two spans in **one line box**, i.e. one paragraph: a
  `U+202E` in the display name **rewrites the address beside it on screen**. The
  user reads the right bytes in the wrong order.

**The defence is two halves, and neither is sufficient alone.**

1. **Strip, before render.** `web/src/core/hostileText.ts` removes the scoping bidi
   controls from every sender-authored string this client renders in its own DOM.
   `U+200E`/`U+200F` (LRM/RLM) are deliberately **kept**: they are single-character
   hints with no scope, they cannot reorder a following run, and they appear in
   legitimate Arabic and Hebrew text. Stripping is what covers the places CSS
   cannot reach at all — an `aria-label`, a `title`, an iframe's accessible name.
2. **Isolate, in CSS.** `unicode-bidi: isolate` on `.sender`, `.subject`,
   `.preview`, `.message-from`, `.message-address`, `.message-meta`,
   `.thread-subject` and the rule-preview pairs makes each its own bidi paragraph,
   so nothing inside one can ever reach outside it. Several of those boxes are grid
   or flex items today and are therefore *already* separate paragraphs — that is a
   property of the current **layout**, not of the markup, and one refactor to an
   inline row would silently take it away.

For the confusable-name case the list row **shows the address too**, but only when
the display name contains `@` or carried a bidi control. That is the narrow rule on
purpose: the sender cell is a single ellipsised column, and appending `<addr>` to
every row would double the text in the cell people triage in — while, in the narrow
layout, usually clipping the very address it was added to show. The reading pane
shows both, always.

**What this does not catch** is in §3.

### 2.4 Attachments (threat 1)

`internal/api/blob.go`. An attachment is bytes from a stranger served from our own
origin.

- `Content-Type` is **chosen by us** from a short **allowlist**. `text/html`,
  `application/xhtml+xml`, `image/svg+xml` and the XML family are **not on it and
  never will be** — each is a scripting context. Anything unlisted becomes
  `application/octet-stream`.
- The `?type=` query parameter of RFC 8620's `downloadUrl` template is
  **ignored outright**. It is client-supplied, and the "client" may be a link in
  the phishing mail itself.
- `Content-Disposition: attachment`, **always** — never conditioned on the type,
  because that would make the disposition a decision the attacker gets to make.
- `X-Content-Type-Options: nosniff`, so the browser cannot recover the dangerous
  type by sniffing.
- `Content-Security-Policy: default-src 'none'; sandbox` and
  `Cross-Origin-Resource-Policy: same-origin` as defence in depth.

The **raw** message bytes stay reachable (a part's `blobId`) because "view source"
is a real feature. That path never renders: it is `application/octet-stream` +
`attachment` + `nosniff`. **Rendering is sanitized; downloading is inert.**

### 2.4a The third door: attachment preview (Forgejo #131)

For two releases this section could end "those are the only two doors." It cannot
any more, and pretending otherwise would be worse than the door itself.
`internal/api/blob.go`'s `PreviewHandler` serves a stranger's bytes with
`Content-Disposition: inline`. Here is why that is a net improvement and what
holds it shut.

**Why it exists.** The alternative to previewing an attachment is *downloading*
it and opening it in a desktop application — handing a hostile document to a
full-featured local viewer running with the user's own privileges. That is the
attack path this entire document exists to close, walked voluntarily, once per
attachment. A door that renders under our own headers is narrower than a door
that hands the bytes to Acrobat. Spec §11 anticipated this: "sandboxed viewer".

**It serves almost nothing.** `inlinePreviewTypes` has **one** entry,
`application/pdf`. Everything else — including every type the *download* door
happily names — gets a `415`, not a different disposition. The two allowlists
answer different questions: `downloadableTypes` is "safe to save",
`inlinePreviewTypes` is "safe to **render**", and the second is a subset by
construction (a test asserts it). `safeContentType` still runs first, so a part
claiming `application/pdf` while declaring `text/html` is already
`application/octet-stream` by the time the inline check sees it — and
octet-stream is not previewable.

**The opaque origin is in the response, not in the caller's iframe.**

```
Content-Security-Policy: default-src 'none'; sandbox; frame-ancestors 'self'
X-Frame-Options: SAMEORIGIN
X-Content-Type-Options: nosniff
Cross-Origin-Resource-Policy: same-origin
```

The `sandbox` directive gives the served document an opaque origin **however it is
embedded**, so the guarantee cannot be lost by editing a JSX attribute. Measured
in real Chromium: with it, hostile HTML served by this route runs no script and is
unreachable from the embedder; **without it, the embedder can reach into the
document** — which is why the e2e asserts it and goes red when it is removed.

`frame-ancestors 'self'` / `SAMEORIGIN` is the one header here that is *looser*
than the download response's `'none'`/`DENY`, and it has to be: a browser enforces
`frame-ancestors 'none'` by refusing the load outright
(`net::ERR_BLOCKED_BY_RESPONSE`), which is the whole feature. It is relaxed to our
own origin and no further.

**Images and text do not use this door at all.** They are rendered from bytes the
client already fetched through the ordinary download route — `Content-Disposition:
attachment` constrains *navigation*, not `fetch()`. An image becomes a `data:` URL
(the case `img-src 'self' data:` already permits, so no CSP source is added — the
same move as §2.8's cid: images) and text becomes **text nodes**, never markup.
That is why the inline allowlist is one entry long rather than nine.

**Preview is opt-in, per attachment.** Opening a message renders no attachment and
fetches no part. A parser is only fed after a deliberate click.

> **Two dead ends worth not repeating.** The obvious build is to fetch the PDF,
> mint a `blob:` URL and frame it with `sandbox="allow-scripts"`. **A sandboxed
> iframe cannot load a `blob:` URL at all** (`net::ERR_BLOCKED_BY_CLIENT`) — the
> frame shows an error page, which looks exactly like a working feature until
> someone opens a PDF. And adding a `sandbox` attribute to the frame pointed at
> *this* route breaks it too: a sandboxed frame is an opaque origin to the server,
> so `Cross-Origin-Resource-Policy: same-origin` refuses the load. `frame-src`
> stays exactly `'self'`; no CSP source was added for this feature.

**And the trade, plainly:** previewing a PDF hands the bytes to the browser's own
PDF engine (PDFium, or pdf.js in Firefox) — a large parser with a long history of
vulnerabilities. It is not ours, it runs in the browser's process sandbox, and it
is strictly less exposed than the desktop viewer it replaces. That is the argument
for it. The argument is *not* that PDF parsing is safe.

### 2.5 The app shell (threats 1, 4)

`cmd/mailroom/main.go` (`appCSP`, `secureHeaders`). Served on **every** response
from the web route, including its 404s and the 302 that bounces an anonymous
browser to the sign-in page — *a header that is only present on the happy path is
not a control.*

```
default-src 'none'; script-src 'self' 'sha256-<reporter>';
style-src 'self' 'unsafe-inline'; img-src 'self' data: https:;
font-src 'self' data:; connect-src 'self'; manifest-src 'self';
frame-src 'self'; object-src 'none'; form-action 'none'; base-uri 'none';
frame-ancestors 'none'
```

`script-src` allows `'self'` (the app's own external module script) **and** the
one hash-pinned message height reporter (Forgejo #100). The reporter hash is here
because the `srcdoc` message frame **inherits this policy** and the two intersect
(§2.1 / §2.2): without the hash in *this* policy the intersection would ban the
reporter and auto-height would break. It adds **no** general inline-script
capability: a hash is not `'unsafe-inline'` (in fact its presence makes a browser
ignore `'unsafe-inline'` in `script-src`), and only one exact, inert script — ours
— matches it. An attacker cannot inject a *different* inline script and have it run.

plus `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy: no-referrer`,
`Cross-Origin-Opener-Policy`, `Cross-Origin-Resource-Policy`,
`Permissions-Policy`, `Cache-Control`, and `Strict-Transport-Security` **when and
only when** the instance is served over HTTPS (same signal that decides `Secure`
on the session cookie).

**No inline scripts** (spec §11). Vite emits an external module script; the source
`index.html` is asserted to contain no inline `<script>` body, and the *built*
one is asserted against the real image in the e2e.

The **sign-in page** (`internal/auth/page.go`) keeps its own, *stricter* policy —
`default-src 'none'` with its one inline style and one inline script pinned by
**sha256 hash**, and zero external fetches. It is deliberately not wrapped in
`appCSP`, which would replace it with a weaker one.

### 2.6 Header injection (threat 5)

The **only** user-controlled value that ever reaches a response header is an
attachment **filename** (`Content-Disposition`) — from two hostile sources: the
`{name}` the client puts in the download URL, and the filename the **sender** put
in the MIME part. `sanitizeFilename` strips CR/LF, control characters, quotes,
backslashes and path separators; `contentDisposition` emits RFC 6266 + RFC 5987.
Go's `net/http` would also refuse to write a CR/LF into a header value — but that
is a *backstop*, not the control: a control we do not own is one that can be taken
away.

Subjects, display names and error strings go into **JSON bodies**, never headers,
and every JSON response carries `nosniff` + `default-src 'none'; sandbox`.

### 2.7 TLS everywhere (threat 6)

- **IMAP.** `internal/sync` supports `TLSImplicit` / `TLSStartTLS` / `TLSInsecure`
  and an `InsecureSkipVerify` flag. **None of the unsafe ones is reachable from an
  operator's configuration.** `config.Account` — the accounts file's schema — has
  *no field* for a TLS mode or a skip-verify, `LoadAccountsFile` uses
  `DisallowUnknownFields` so writing one anyway is a **hard boot failure** rather
  than a silently-ignored key, and both construction sites (`cmd/mailroom`'s
  `accountSpec`, `internal/syncrunner`'s resolver) hardcode `TLSImplicit` with
  verification on. The zero value is the safe value.
- **SMTP submission.** `internal/provider/sender.go` **refuses to authenticate**
  over a connection that is neither implicit-TLS nor successfully STARTTLS-
  upgraded. A STARTTLS-stripping MITM therefore gets a *failed send*, not a
  password. Certificate verification is on (`MinVersion: TLS1.2`, no
  `InsecureSkipVerify`).
- A repository-wide `go/ast` test (`TestNoProductionCodeDisablesTLS`) fails the
  build if **any** non-`_test.go` file sets `InsecureSkipVerify: true`,
  `AllowInsecureNoTLS: true`, or names `TLSInsecure`/`TLSStartTLS`.

### 2.8 Inline (CID) images — rendering them without a remote fetch (Forgejo #99)

A `multipart/related` mail embeds images (a signature logo, a header banner) as
MIME parts and references them from the HTML with `cid:` URLs (RFC 2392). Those
bytes are **attached to the message**, so displaying one fetches nothing external
— it is *not* remote content, and it must render **without** the "Show remote
content" gate (§2.3). Two facts made the earlier build show them as broken
images: the render CSP is `img-src data:` (which does not resolve `cid:`), and
nothing mapped a `cid:` reference to the part that carries it. Closing that gap
must not re-open §2.3.

It is closed on the **client**, and the key move is that an inline image is
rehydrated to a **`data:` URL** — the exact inline-bytes case the message CSP
*already* permits — so **no new CSP source is added**. `img-src` stays `data:`
(and `data: https:` only when the user opts into remote content); there is no
`img-src *`, no blanket `https:`, and no scoped self origin. A source that is
never widened cannot re-enable tracking. Concretely:

- **The map comes from the server's one part-id walk, not from re-parsing HTML.**
  `internal/api/body.go` already emits, on every `EmailBodyPart`, the part's `cid`
  (its Content-ID) and `blobId` (`<messageBlobId>.<partId>`, minted by
  `assignPartIDs`). The client (`web/src/app/ui/inlineImages.ts`) builds the
  cid→blobId map straight off those objects. So "the cid the message references"
  and "the part the client fetches" are the same walk and cannot drift.
- **The sanitizer still owns what a `cid:` may be.** `sanitize.go` keeps a `cid:`
  src verbatim (classified `srcInline`), strips every other scheme, and never
  marks a `cid:` image as blocked remote content — so a message with *only* inline
  images shows no banner.
- **The fetch is the app's own, same-origin, authenticated request.**
  `JmapClient.fetchInlineImage` GETs the part from `/jmap/download/…` with the
  session cookie and accepts the bytes **only** if the endpoint served them as a
  real raster image type (the client half of §2.4's allowlist — `image/svg+xml`
  and `text/html` come back as `application/octet-stream` and are refused). It
  then becomes a `data:` URL and the client rewrites `src="cid:x"` to it.
- **Unresolved `cid:` is stripped.** A `cid:` that names no part — or a part the
  endpoint would not serve as a raster image — has its `src` removed, so it
  renders as nothing rather than as a dangling reference.
- **Only referenced parts are inlined.** An image part the HTML does not reference
  stays an ordinary attachment (a download); nothing auto-inlines every image.

The string rewrite is sound for the same reason `restoreRemoteContent` is: it runs
only on the server's own sanitized output, where every `"` in text and attribute
values is `&#34;`, so ` src="cid:` occurs only in an `<img>` src the server wrote.

> **Render preference (Forgejo #99).** The reading pane now **prefers the HTML
> body** whenever a message has one, falling back to the `text/plain` twin only
> for text-only mail. Almost every real message is `multipart/alternative`;
> rendering the plain-text alternative hid layout, inline images and the
> remote-content banner. HTML still renders **only** inside the sandboxed,
> opaque-origin iframe with the strict CSP — that sandbox is exactly what makes
> defaulting to HTML safe.

### 2.9 HTML we *generate*: Markdown compose (Forgejo #56)

Everything above is about HTML arriving from outside. Markdown compose adds the one
place where **we** build an HTML document from user input
(`web/src/core/markdown.ts`), so it needs its own paragraph — not because it is
exposed to an attacker, but because the next person will reasonably ask.

Three facts bound it:

1. **The output never enters our own DOM.** It is computed at send time and handed
   to `Email/set` as the `html` body. Nothing renders it in the app shell, so it is
   not a route to the shell's origin, and `TestWebNeverInjectsHTMLIntoOurOwnDOM`
   still holds unchanged — this file adds no `innerHTML` call. (Contrast §2.1's
   narrow `contentEditable` exception, which this is *not* an extension of.)
2. **The server re-derives what is transmitted.** `Email/set` sanitizes every
   outbound HTML body (`internal/api/draft.go` → `SanitizeOutboundHTML`) exactly as
   it did for the rich editor. The renderer is therefore a convenience, not a
   control: it cannot put bytes on the wire the sanitizer would not have passed.
3. **It is still written as if it were the control**, because "something else will
   catch it" is how holes get built. Every character of user text is escaped
   *before* any markup is added, so inline HTML comes out as visible text rather
   than being "unsupported"; the emitted tag set is fixed and closed; the only
   attribute it ever emits is `href`; and hrefs are an **allowlist** (http, https,
   mailto), so no amount of `java&#115;cript:` obfuscation reaches one.

The renderer is deliberately ours and deliberately incomplete. A general-purpose
Markdown library is a parser fed by a human with a CVE history in its HTML output
path; the subset people actually type in mail is small enough to own outright, and
unsupported syntax degrades to the literal characters typed — which for mail is the
correct failure, since the recipient still reads what was written.

**The Markdown source, not the rendering, is the `text/plain` part.** That is the
point of Markdown, and it means a recipient with HTML disabled sees exactly the
bytes the sender reviewed before pressing Send.

### 2.10 A URL out of a message: unsubscribe detection (Forgejo #58)

`List-Unsubscribe` (RFC 2369) is a sender-controlled URL that the product now puts in
front of the user, so it gets a paragraph. **Most real mail carries it** (a majority of messages in a
real test mailbox), so this is not a corner case.

**The door that stays shut.** RFC 8058 defines a one-click unsubscribe: the client
`POST`s to the https URL. Mailroom does not implement it, and the reason is §2.3's:
a server-side POST to an address a stranger chose would be this project's **first
outbound fetch on behalf of a message**, i.e. an SSRF surface aimed at whatever the
sender wrote — internal hosts, cloud metadata endpoints, the operator's own network.
Forgejo #58 scopes unsubscribe to *surfacing* the affordance, and that scope is what
keeps the door shut. If one-click is ever wanted it needs its own ADR, an
egress-restricted fetcher, and a re-reading of §2.3's claim that Mailroom makes no
outbound fetch on a message's behalf.

**So there are exactly two routes, and both already existed:**

- The **link** is an `<a target="_blank" rel="noopener noreferrer">` the *user's own
  browser* opens. Nothing in the app or the server fetches it. `noopener` so the
  opened page cannot reach back through `window.opener`; `noreferrer` so it is not
  told which message it came from.
- The **mailto** is sent through the ordinary submission path from the user's own
  identity — the same door every outgoing message uses, with no new capability.

**Parsing happens on the SERVER** (`internal/api/unsubscribe.go`), and that placement
is the point: deciding which scheme may enter an `href` is a security decision, and
making it twice — once in Go, once in TypeScript — means making it differently. The
client receives a URL already known to parse as `https` with a host and no userinfo,
or nothing at all. It never sees the raw header.

The scheme list is an **allowlist**: `https` and `mailto`, nothing else. Notably
**plain `http` is refused**, and the cost of that is measured rather than assumed —
in a real mailbox no message used it, while an unsubscribe over cleartext both leaks the
subscription to the network and can be rewritten in flight into something else. A
message offering only an `http` link is reported as having no link; its mailto, if
any, is still offered. `userinfo` is refused too (`https://real.example@evil.example/`
is the classic read-as-one-host-resolve-to-another trick).

The parser is **lenient about what real mail actually does** and strict only about
schemes: a few messages carry a bare URL with no angle brackets (RFC 2369 requires them),
and a few arrive as undecoded RFC 2047 encoded-words. A strict reading drops those and
tells the user there is no unsubscribe while one is visible in the message body.

**It asks before acting**, and the confirmation is not ceremony: an unsubscribe is a
reply, and a reply confirms to the sender that the address is live and read — which for
a sender who never had consent is worth more than the unsubscribe. The panel says so,
in the same spirit as §2.3's banner saying what loading an image reveals.

### 2.11 A calendar out of a message: invitations (Forgejo #725, ADR-033)

An `.ics` — a `text/calendar` part or attachment — is attacker-controlled input with
two features a body does not have: it carries **addresses Mailroom will send to**
(the ORGANIZER), and it carries **instructions** (METHOD:CANCEL, a new SEQUENCE)
that change what the user is shown about other messages. Each rule below closes one
door, and each has a test that goes red without it.

- **Parsed on the server, bounded while parsing.** `internal/ical` reads at most
  256 KiB, 256 components, 4,096 properties, nesting depth 4 and 32 parameters per
  line; 100 attendees are listed and the rest are *counted*. A calendar past a bound
  is reported as `calendarInvite.problem = "tooLarge"`, never as "no invitation". The
  client receives structured fields only and never sees a raw `.ics` — the same
  placement argument as §2.10's unsubscribe parser.
- **Recurrence is summarised, never expanded.** There is no expander to call
  (ADR-033), so `RRULE:FREQ=SECONDLY;COUNT=2000000000` costs what any line costs.
- **Nothing a calendar names is fetched.** URL, ATTACH, TZURL, ALTREP: none is read,
  returned or followed, and `internal/ical` imports no network package
  (`TestPackageNeverFetches`). A time zone resolves from the compiled-in zone
  database or the file's own VTIMEZONE; a TZID that is neither — including a path
  (`../../etc/passwd`), `Local` (which would mean the *server's* zone) or a 1,000-byte
  name — leaves the time **floating**, shown as written with the zone named.
- **Sender-authored strings are text, cleaned twice.** Control characters are
  removed and lengths capped on the server; the card renders them as React text and
  strips bidi controls (§2.3b), with `unicode-bidi: isolate` on the title, people and
  location.
- **The REPLY re-encodes everything it echoes.** UID and SUMMARY are TEXT-escaped,
  the CN is DQUOTEd with control characters removed, addresses are single plain
  addr-specs, and a RECURRENCE-ID is echoed only if its value and TZID are well
  formed. A CR/LF smuggled into any field cannot start a content line of the reply,
  and the Subject goes through compose's header sanitizer like any other.
- **Never auto-respond.** Ingest indexes an invitation and Email/get reads one;
  neither writes a draft, a submission or an answer
  (`TestCalendarInviteOnEmailGet`). The only path to a REPLY is
  `Email/replyToInvite`, which a click reaches.
- **The reply goes through the ordinary send.** It is a draft FROM the address the
  invitation was delivered to — which ingest resolved to a verified identity — TO the
  organiser, submitted by the same `createSubmission` EmailSubmission/set runs: the
  From check, ingest-only mode and Undo Send apply unchanged. An organiser that is
  missing or not one plain address is refused with a sentence, and nothing is written.
- **Only an AUTHENTICATED organiser changes another message's card** (security
  review of #725). A UID is in every copy of an invitation, and ORGANIZER is a string
  the sender typed, so matching on them alone let anyone who had seen an invitation
  cancel or supersede it on the card. A CANCEL or a higher-SEQUENCE REQUEST now affects
  another message only when `ical.Verified` holds: the message has exactly one From
  field and its address equals ORGANIZER, **and** the topmost Authentication-Results
  bearing a trusted authserv-id (`MAILROOM_AUTHSERV_IDS`, the parse learned placement
  uses; an entry `id<YYYY-MM-DD` is trusted only for mail received before that date,
  judged on `received_at`, never the Date header, ADR-035) carries a DMARC pass aligned to the From domain. Anything else still shows its
  own card, marked "could not be verified as coming from the organiser", and cancels,
  supersedes and appends change rows for nothing. An attendee's REPLY is verified the
  same way against the attendee it names. **A Sender is never honoured**, although
  RFC 6047 §3 lets an organiser's delegate send: DMARC vouches for the From domain,
  not the Sender mailbox, so "Sender = ORGANIZER" would let anyone at that domain
  cancel a colleague's meeting. A delegate's cancellation therefore shows as
  unverified. **Fail closed:** with no trusted authserv-id
  configured nothing is verified — learned placement's "did not clearly fail" fallback
  is not used, because a spoof carrying no Authentication-Results at all passes it.
  `TestSpoofedCancellationsAndUpdatesChangeNoOtherCard` sends the four spoofs (no
  auth, a DMARC fail, an authenticated stranger, an untrusted authserv-id) and an
  unverified update, then the genuine CANCEL, and each check was probed red.
- **The window cannot be flooded.** Versions are read per (UID, instance, organiser)
  in SQL with the cap applied per key, and the key's highest verified CANCEL and
  REQUEST are returned whatever the cap, so newer rows sharing the UID cannot push
  the decisive one out (`TestCalendarVersionsCapIsPerKeyAndKeepsTheDecisiveRows`).

## 3. What is NOT defended

Stated plainly, because a security note that only lists wins is a marketing
document.

- **The search index is not a hostile-content surface, but it IS a plaintext-at-rest
  one.** Every user's per-user search index holds a readable copy of message
  subject/body/participants — including mail that arrived PGP/S-MIME encrypted and
  was decrypted for indexing — so it carries the same at-rest exposure as the raw
  mail store and MUST live on a volume/filesystem-level-encrypted disk (LUKS/dm-crypt
  or equivalent), the same deployment requirement stated for the blob store; see
  spec §7 and §11 (mailroom#41).
- **A browser bug in iframe sandboxing or CSP.** We bet on origin isolation. If
  the browser's isolation is broken, so are we — as is every other webmail.
- **The user clicking a phishing link.** Links work (that is the point). We force
  `rel="noopener noreferrer nofollow"` and `target="_blank"`, strip `javascript:`
  and friends, and send no referrer — but a user who clicks through to a
  convincing fake and types their password is not something the renderer can stop.
  *Phishing / homograph-URL warnings are v2* (feature matrix §8).
- **Malware in an attachment the user downloads and runs — narrowed, not closed.**
  ClamAV scanning shipped in v1.59–v1.61 (ADR-027). A part whose verdict is
  `flagged` or `unknown` is now **refused at the download route**, which is the
  one door CID inline images, client-side previews, foreign JMAP clients and raw
  `fetch` all go through; it cannot be attached to an outgoing draft either. So
  the commodity case is no longer in this list.
  What remains here, and why this entry is reworded rather than removed:
  - **A `clean` verdict is a claim with a timestamp, not a property of the file**
    (ADR-027 §1a). Scanning at ingest is the moment of MINIMUM signature
    coverage — malware ships before its signature — so the re-scan cadence, not
    the arrival scan, is the actual detection mechanism.
  - **The user can release a quarantine**, deliberately: a scanner that cannot be
    overridden turns a false positive into destroyed mail, which is the worse
    failure. Once released, the bytes are handed over.
  - **The raw message is never blocked.** A quarantined part sits inside it
    base64-encoded, and a determined extraction gets it. The message must stay
    readable, forwardable and exportable; the export carries the verdict beside
    the bytes so the next instance is not told a lie by omission (§11).
  - **And no scanner sees everything.** An encrypted archive or a message past the
    parser's limits is `unknown` rather than examined — which is why `unknown`
    carries the same enforcement as `flagged` (§5b).
- **Remote content the user explicitly unblocks.** They are told exactly what it
  means. With the image proxy on (the default since 1.x, ADR-026), the fetch is
  made by the server, which hides the reader's browser and address but not the
  fact or timing of the open; with it turned off, the fetch is direct from the
  reader's browser.
- **Tracking by DNS/preconnect once remote content is unblocked** — the sanitizer
  removes `<link>` unconditionally, so this stays closed even then; but a remote
  image URL is itself a beacon, which is the trade the user just made.
- **Content sniffing by a client that ignores `nosniff`.** Not our browser.
- **A display name that merely *looks* like someone.** §2.3b catches a name that is
  itself an address and a name built with a direction override, because both are
  mechanically detectable. `From: "Bank Security" <attacker@evil.example>` is not
  detectable by any heuristic over a string the sender chose — the reading pane
  shows the real address, as it does for every message, and that is the whole of
  the answer until the `From` is **authenticated**. *DMARC/ARC verification is
  #55.* The same gap is what §2.3a's remembered remote-content grant is keyed on.

  ⚠ **MEASURED IN v1.61.0 (#397), AND THE SENTENCE ABOVE HELD.** The obvious rule
  — "the display name matches a contact but the address is not theirs" — was run
  against a real mailbox: it fired on **a few percent** of messages and was
  legitimate **every single time**. Almost all of it is an ESP sending on a
  brand's behalf, which authenticates correctly because the ESP signs as itself.
  The rule measures *delegation*, not deception, so it was not built.
- **Homograph display names and lookalike domains** (`раypal.example` in Cyrillic).
  ⚠ **HALF OF THIS MOVED OUT OF THIS LIST IN v1.61.0** (#397), and the half that
  stayed is the more interesting one.
  - **Lookalike SENDING DOMAINS are now flagged.** `Email.senderWarnings` reduces
    the `From` domain to what it *renders* as and compares it against the domains
    the user actually corresponds with — so `rnailbox.example` is reported as
    imitating `mailbox.example`, and the warning names the real domain. Measured
    before shipping: 0 collisions among a real mailbox's correspondent
    domains, against crafted lookalikes of those same domains that do collide.
  - **Homograph DISPLAY NAMES are still not flagged**, and a lookalike domain
    the user has never corresponded with is not either — the index is built from
    their own mail, so it warns about imitations of relationships they *have*.
  - **Neither is a domain that merely CONTAINS a brand** (`paypal-security.example`).
    That is not a confusable; it is a different domain that reads convincingly,
    and catching it needs a brand list with a much worse false-positive profile.
  - *Phishing / homograph warnings about URLs in bodies remain v2* (#70). Same
    machinery, different object: that one judges links, this one judges senders.
- **Inline (CID) images now render** (Forgejo #99), and they do it *without*
  opening the remote hole — see §2.8. The residual non-defence is only the general
  one: an inline image is attacker-supplied bytes, decoded by the browser's image
  codecs; we never serve it as `image/svg+xml` or any active type (§2.4), so it is
  a picture, not a document.

## 4. Where the proofs live

| Claim | Test |
|---|---|
| The sanitizer removes every construct in a hostile corpus, and the output's **DOM** contains nothing that executes or fetches | `internal/api/sanitize_test.go` (`assertSafeDOM`) |
| Surviving CSS declares only allowlisted **properties**, contains no `<url-token>` / unlisted function / unlisted at-rule, and — decoded by a **second, independent** escape decoder — contains no dangerous literal | `sanitize_test.go` `assertInertCSS` (three checks) |
| A CSS-escaped `url()` / `@import` / `expression()`, `image-set()` and the rest of the image-function family are removed — the #297 payloads, measured fetching in real Chromium before the fix | `sanitize_test.go` `TestSanitizeCSS`, and the `#297:` cases in `TestSanitizeHTML` |
| The CSS allowlist has not been tightened into uselessness: a real newsletter's media query, class rules and cell styling survive | `TestSanitizerKeepsLegitimateCSS` (the positive control for every `#297:` case) |
| Each of the three CSS assertions **can actually fail**, and does not fire on inert CSS | `TestTheCSSInertnessCheckerCanActuallyFail` |
| The sanitizer is a **fixed point** (no mutation-XSS gap) and never panics | `TestSanitizeHTMLIsIdempotent`, `TestSanitizeHTMLIsTotal` |
| `Email/get` actually **calls** it, on real RFC 5322 bytes | `internal/api/security_test.go` `TestEmailBodyValuesAreSanitized` |
| Legitimate newsletter markup survives | `sanitize_test.go` "a real newsletter's layout survives" |
| `nosniff` on **every** JMAP response incl. 404/405 | `TestNosniffOnEveryJMAPResponse` |
| Attachments are never a scripting context; `?type=` is ignored | `TestDownloadTypeParameterIsIgnored`, `TestBlobResponseHeaderSet` |
| The **preview** door renders one media type and nothing that executes; its allowlist is a subset of the download one | `internal/api/blob_test.go` `TestInlinePreviewTypesRenderNothingThatExecutes` |
| The preview response carries the `sandbox` directive, `frame-ancestors 'self'`, `nosniff` and an inline disposition — and grants the sandbox no escape tokens | `TestPreviewResponseHeaders` |
| An inline disposition cannot be used to forge a header either | `TestInlineDispositionIsInlineAndCannotForgeHeaders` |
| The preview door runs the **same RLS ownership check** — another user's part is a 404, not a 415 | `internal/api/rls_integration_test.go` `TestRLSCrossUserPreview` |
| The client renders only types the **server** named, refuses `application/octet-stream`, and bounds every renderer | `web/src/app/ui/attachmentPreviewPolicy.test.ts`, `web/src/core/jmap/fetchPart.test.ts` |
| A remembered remote-content allow **cannot be widened** into a wildcard, a suffix match, or an unknown kind — and a refused value is not stored | `internal/store/remote_allows_integration_test.go` `TestRemoteAllowRefusesAnythingWiderThanOneSenderOrDomain` |
| Every grant is listable and **revocable**, and both writes append a `change_log` row (re-granting the same thing does not) | `TestRemoteAllowRoundTripAndRevoke`, `TestRemoteAllowAppendsChangeRows` |
| The client's matcher **fails closed** — no grants, no sender, a malformed address, an unknown kind, a domain suffix or subdomain all resolve to "keep blocking" | `web/src/core/remoteAllows.test.ts` |
| A grant is **per user**: naming another user's allow neither reads nor revokes it | `internal/api/rls_integration_test.go` `RemoteAllow/*` probes |
| A **real PDF actually reaches the browser's viewer**, and the same route serving hostile HTML executes nothing, fetches nothing, and is opaque to the embedder | `scripts/hostile-content-e2e.sh` §6 (preview cases) |
| The Markdown renderer escapes inline HTML rather than passing it, emits no attribute but `href`, and **refuses every scheme outside http/https/mailto** — including case- and entity-obfuscated ones — while still linking the three it allows | `web/src/core/markdown.test.ts` "what it refuses to emit" |
| A `List-Unsubscribe` target is refused unless it is `https` with a host and no userinfo, or a `mailto` with an address — `javascript:`, `data:`, `file:`, `ftp:`, plain `http:` and schemeless all yield no affordance, and a refused link does not cost the message its mailto | `internal/api/unsubscribe_test.go` `TestParseListUnsubscribeRefusesUnsafeTargets` (each refusal negative-controlled) |
| The `unsubscribe` property survives the whole chain to `Email/get` — and an unsafe target yields `null` **there**, on a real row, not just in the parser | `internal/api/unsubscribe_integration_test.go` |
| `unsubscribe` is **not** a default Email property, so a sender-controlled string never enters the list view | `TestUnsubscribeIsNotADefaultEmailProperty` (with a positive control) |
| A calendar part is bounded while it is parsed (size, components, properties, depth, parameters, attendees), a RRULE is never expanded, a hostile TZID floats, and a CR/LF in any field cannot become a content line of the REPLY | `internal/ical/ical_test.go` `TestHostile*`, `reply_test.go` `TestReplyEscapesTextExactly` (each mechanism probed red, #725) |
| `internal/ical` cannot fetch: it imports no network package | `internal/ical/reply_test.go` `TestPackageNeverFetches` |
| Receiving and reading an invitation never answers it; a refused answer writes nothing; a stranger's CANCEL does not cancel the card | `internal/api/calendar_integration_test.go` |
| A filename cannot split a response (asserted over the **serialized wire bytes**) | `TestFilenameCannotInjectAHeaderThroughTheRealHandler` |
| The app shell's CSP, on the **real mux**, on both auth paths | `cmd/mailroom/security_test.go` `TestAppShellSecurityHeaders`, `TestAppCSPDirectives` |
| The sign-in page keeps its stricter hash-pinned CSP | `TestSignInPageKeepsItsOwnStricterCSP` |
| No inline script; no `innerHTML`/`dangerouslySetInnerHTML` anywhere in `web/` | `TestWebIndexHasNoInlineScript`, `TestWebNeverInjectsHTMLIntoOurOwnDOM` |
| The message sandbox is `allow-popups allow-popups-to-escape-sandbox allow-scripts` and never `allow-same-origin`; the message CSP `script-src` is the reporter hash only | `TestMessageSandboxIsNotRelaxed` |
| …and the `<iframe>` **actually receives it**: the only frame in `web/src` outside the #131 preview carries `sandbox={MESSAGE_SANDBOX}` **verbatim** — no concatenation, no template literal, no overriding spread — and the frame count is pinned so the assertion cannot go vacuous | `cmd/mailroom/security_test.go` `TestEveryIframeCarriesTheMessageSandbox` (probed against deletion **and** against `+ " allow-same-origin"`, both of which leave the constant test green — Forgejo #298) |
| Event handlers are stripped by their **own** rule, not only by the attribute allowlist — asserted with `onclick` temporarily *on* the allowlist, so the backstop cannot be what removes it | `internal/api/sanitize_test.go` `TestEventHandlersAreStrippedByTheirOwnRule` |
| No key on the attribute allowlist begins with `on` | `TestNoEventHandlerIsOnTheAttributeAllowlist` |
| Every scoping bidi control (`U+202A`–`U+202E`, `U+2066`–`U+2069`) is removed from sender-authored chrome text, LRM/RLM are **kept**, and a confusable display name makes the list row show the real address | `web/src/core/hostileText.test.ts` (including the assertion that a no-`@` lookalike is **not** caught, so widening the rule is a decision) |
| The one message script that runs is the height reporter; a hostile body still executes nothing and fetches nothing remote **with `allow-scripts` on**, and a normal message auto-sizes to its content | `scripts/hostile-content-e2e.sh` §6 |
| The accounts file cannot disable TLS verification | `TestAccountsFileCannotDisableTLSVerification` |
| **No production file anywhere** turns off TLS | `TestNoProductionCodeDisablesTLS` |
| SMTP never sends `AUTH` over cleartext — *asserted by what the server received* | `internal/provider/sender_tls_test.go` |
| SMTPS really verifies the certificate | `TestImplicitTLSVerifiesTheCertificate` |
| All of the above **against the real image**, with a real hostile message, a real Postgres, a real IMAP server over real TLS, and a real remote-content tracker that must stay silent | `scripts/hostile-content-e2e.sh` |

## 5. Rules for the next person

1. **Never render a message body outside the sandboxed iframe.** There is no
   "safe" `innerHTML`. A test fails if you add one.
2. **Never add `allow-same-origin`** to the message sandbox. `allow-scripts` is
   present, and is safe *only* under the three-part lock in §2.1 (opaque origin +
   CSP hash-pin of the single reporter in **both** policies + server sanitizer).
   If you cannot keep all three, do not ship the change. Never add any script
   source to the message CSP beyond the reporter's own sha256.
   *Note the shape of the rule:* the sandbox is asserted **twice**, as the value
   of the constant and as the attribute the `<iframe>` receives, because until
   #298 only the first was — and deleting the attribute, or writing
   `MESSAGE_SANDBOX + " allow-same-origin"`, left Go, `tsc` and every vitest green
   while the message frame ran same-origin. **A constant nobody checks the use of
   is a comment.**
3. **Never add `'unsafe-inline'` to the app shell's `script-src`.** A single
   hash-pinned source (the message reporter) is fine — it is one exact, inert
   script; `'unsafe-inline'` is a blank cheque. If the app
   stops booting under CSP, the build tool started inlining a script — fix the
   build tool.
4. **Never add `text/html`, `image/svg+xml` or the XML family** to
   `downloadableTypes` — nor, more urgently, to `inlinePreviewTypes` (§2.4a),
   where an entry would be served *inline* from the operator's own origin. That
   list stays a strict subset of the download one, and every addition to it needs
   the same argument §2.4a makes for PDF: which browser component parses it, in
   which process, and why that is safer than the download it replaces.
5. **Never let the sanitizer branch on a user preference.** The per-sender
   remote-content allow (§2.3a) was built entirely client-side for this reason: a
   sanitizer with one branch has two paths, and the argument in §2.1 only holds for
   one. If a feature seems to need "sanitize differently for this sender", the
   answer is to decide on the client what to un-park from unconditionally-parked
   output. And never add a wildcard, a suffix matcher or a global toggle to
   `app.remote_allows` — the schema is deliberately unable to express one.
6. **Never move the preview's `sandbox` into the client's iframe attribute.** It
   lives in the response so it cannot be dropped by a UI edit, and a `sandbox`
   attribute on that frame actively breaks the load (§2.4a). Never widen
   `frame-ancestors` beyond `'self'` there.
7. **Never weaken the sanitizer to make a message render.** If real mail is being
   destroyed, widen the *allowlist* of inert elements/attributes — not the policy.
   For CSS that means adding to `cssAllowedProperties`, and only for a property
   that **cannot name an external resource**: no `<url>`, no `<image>`, no script
   hook. Never re-introduce a *denylist* of dangerous CSS constructs, and never
   assert a CSS property with `strings.Contains` — #297 is the whole story of why
   (§2.3), and its test was green for two releases because it shared the code's
   string model.
8. When you add a route that returns bytes, decide its `Content-Type`, set
   `nosniff`, and say which side of the login gate it is on (`cmd/mailroom`'s
   `routes()` will not compile otherwise). If it serves per-user data by a
   caller-supplied id, it also needs a cross-user probe — `internal/api`'s
   `TestEveryDataRouteHasACrossUserProbe` will not let you forget.
9. **Never render our own generated HTML in the app shell** (§2.9). The Markdown
   renderer's output goes to `Email/set` and nowhere else. If a live preview is ever
   wanted, it belongs in the same sandboxed opaque-origin frame the message body
   uses — not in a `div` — and the argument in §2.1 has to be made again for it.
   Never widen that renderer's href allowlist, and never add an attribute to its
   output: `href` is the only one, and adding a second means auditing every emitted
   tag again.
10. **Never make Mailroom fetch a URL that came out of a message** (§2.10). Not for
    one-click unsubscribe, not for link previews, not for "checking whether the
    unsubscribe worked". §2.3's claim that Mailroom makes *no* outbound fetch on a
    message's behalf is load-bearing, and the first exception to it is an SSRF surface
    pointed at an address a stranger chose. A user's own browser opening a link they
    clicked is a different thing, and is the only shape allowed here.
11. **Assert that a security control can FAIL.** Three assertions written for the
   preview (#131) passed while proving nothing: `contentDocument` is null for any
   PDF-typed resource whether or not it is sandboxed, `alert()` is silently dropped
   in a sandboxed document so a dialog counter never fires, and a frame that failed
   to load executes nothing for the trivial reason that it loaded nothing. Each was
   only caught by deliberately breaking the control and checking the test went red.
   A green security test that cannot go red is worse than no test, because it is
   read as evidence.
