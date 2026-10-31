# ADR-026: The image proxy is a *cache in front of a shared egress guard*, and the guard ships first

**Status:** **Accepted** (Forgejo #55 increment 3). The four questions this ADR left open
were settled at the time; the outcomes are §3e, and the appendix keeps
the analysis that preceded them — including the two places the decision went against this
ADR's own recommendation, and what each of those requires in exchange. Nothing here is
built yet. §2 records a pre-existing unguarded egress path found while writing this, which
changes the shape of the work.

## Context

Spec §11 requires *"remote content blocked by default (opt-in image proxy that strips
tracking params)"* and *"SSRF guard on image proxy / WKD / outbound fetches (allowlist,
no internal ranges, DNS-rebind protection)"*. #55 increment 3 is the last item on that
issue.

Three increments have already landed and they change what this one is for:

- **v1.39.0** surfaces what SPF/DKIM/DMARC said.
- **v1.41.0** measured the exposed population: **every** message from a sender whose
  remote content is allowed carried a 1×1 beacon.
- **v1.42.0** suppressed those beacons structurally. **They no longer load at all.**

So the proxy is no longer the thing that stops open-tracking — that is done. What is left
is the **ordinary images** in mail from senders the user has allowed: a few
thousand remote images in the measured corpus, roughly nine per message, from several
hundred distinct hosts corpus-wide.

## 1. ⚠ What a proxy actually buys, stated without inflation

This is first because it is the part most likely to be overstated, and because an ADR
whose premise is inflated produces a feature nobody can later evaluate. One paragraph of
it was overstated in the opposite direction on the first pass; that correction is below
and is left visible rather than tidied away.

**A proxy hides the reader's browser.** User-Agent, `Accept-Language`, TLS fingerprint,
and the exact request pattern stop reaching the sender. That is real and it is the main
thing.

**A proxy replaces the reader's address and location with the server's, wherever the
reader happens to be.** This is the part an earlier draft of this ADR got wrong, and the
correction matters enough to record rather than quietly fix.

That draft argued the IP benefit was "possibly zero" because a home-hosted server and a
reader at home share one public address. That reasoning collapses *self-hosted* into
*read from home*, and Mailroom is a **server application**: the web client, and the Tauri
thick client contemplated in spec §12, both talk to it over the network rather than
processing mail themselves. The reader is routinely somewhere else — travelling, at work,
on a phone, at an event.

So the honest statement is the stronger one:

- **At home**, the fetch already egresses from the same address, so the IP benefit is
  nil — but the browser-fingerprint benefit above still holds.
- **Anywhere else**, the sender would otherwise learn the reader's *current* address:
  which city, which hotel, which employer's network.

⚠ **And the cumulative form is worse than any single reading.** A sender whose mail is
opened from home on Monday, an office on Tuesday and a conference hotel on Thursday
learns a **movement history**, keyed to a person, for free, from a newsletter. The proxy
collapses all of that to one fixed address — which is exactly the property that does not
degrade as the instance moves, and does not depend on where it is hosted.

The benefit therefore ranges from "browser fingerprint only" (reader at home) to
"conceals travel and workplace" (reader anywhere else), and the second case is ordinary
rather than exotic.

**A proxy does NOT hide WHEN you opened the message.** The fetch still happens at open. A
sender who wants read-tracking learns the same thing from an ordinary image as from a
beacon, just less precisely. **Only caching changes that**, and only for the second and
later opens.

**A proxy can strip tracking parameters**, per spec §11. What that is worth is unmeasured
and should be measured (see §6) rather than assumed.

**What it does not need to do any more:** stop beacons. v1.42.0 did that, and did it more
completely than a proxy could — a suppressed beacon is never fetched by anyone, whereas a
proxied beacon is still fetched, just by the server.

### The honest summary

The proxy is worth building. Its benefit is **browser-fingerprint hiding, location and
movement hiding whenever the reader is not on the same network as the server, and
caching** — and the middle one is the largest, because it is the one that accumulates.
What it is NOT is a defence against open-tracking; v1.42.0 did that, and better.

## 2. ⚠ The finding that reorders the work: there is already an unguarded egress path

While establishing that the proxy would be Mailroom's first server-side outbound fetch —
a claim made repeatedly in #55's own comments, **including by me** — it turned out to be
false.

`internal/webpush/send.go` POSTs to `sub.Endpoint`, a URL supplied by the client when it
registers a device. `internal/api/push.go`'s `PushSubscription/set` accepts **any
non-empty string** as that endpoint: no scheme check, no host check, no address check. It
is stored and later POSTed to.

That is a blind SSRF: the response is not returned to the caller, but the server can be
made to send an authenticated, encrypted POST to an arbitrary address — including
`169.254.169.254`, a loopback admin port, or an internal service. The attacker must be an
authenticated user registering their own device, which is a real limit and not a defence.

**Consequence for this ADR:** the guard is not a component of the image proxy. It is
**shared infrastructure with two callers on the day it lands**, and the second one is a
bug fix. That is a better first increment than the proxy, and it can ship alone.

## 3. Decision

### 3a. `internal/egress` — one guarded outbound HTTP client, and the only way out

A single package that every outbound network fetch goes through, in the shape
`internal/store` has for the database: not a helper anyone may bypass, but the only door.

It must:

- **Take its permitted schemes from the caller**, and refuse every other scheme outright.
  ⚠ This is a **parameter, not a constant**, because the guard's two callers must not get
  the same policy: `webpush` permits **`https` only** — a Web Push endpoint is always
  `https`, and widening it would leave §2's fix as a smaller copy of the same bug — while
  the proxy permits `https` **and `http`** (decision A, §3e). A caller that states nothing
  gets `https` only, so the unsafe direction is never the default.
- **Resolve first, then decide, then connect to the address it decided about.** The check
  and the connection must use the **same** IP. Resolving, validating, and then handing the
  hostname to `http.Client` re-resolves and is the textbook DNS-rebind hole. In Go this is
  a custom `DialContext` that validates in the dialer, on the address it is about to dial.
- **Refuse every non-public address**, on both families: loopback, private (RFC 1918),
  link-local **including `169.254.169.254`**, unique-local IPv6, CGNAT `100.64/10`,
  unspecified, multicast, and IPv4-mapped IPv6 forms of all of them. ⚠ The
  IPv4-mapped-IPv6 encoding is how this check is usually bypassed.
- **Re-validate every redirect hop** against all of the above, and cap the hops.
- **Bound the response**: total bytes, wall-clock timeout, and a `Content-Type` allowlist —
  with the type **sniffed, not believed**, matching how the download endpoint already
  refuses to trust a declared type (`internal/api/blob.go`).
- **Send nothing of the user's**: no cookies, no `Referer`, no `Authorization`, a fixed
  neutral `User-Agent`. Hiding the browser is the point; leaking it in a header would be
  the whole feature failing quietly.
- **Never report a failure as an empty success.** A refused, timed-out or over-size fetch
  is a distinct outcome the caller must handle. This is the shape that has cost this
  project repeatedly — most recently a security check that reported *"the tracker was
  contacted"* because it could not read its own evidence (#332).

**`webpush` adopts it in the same change**, which fixes §2 and gives the guard a second
caller immediately — a component with one caller is a component whose interface nobody has
tested.

### 3b. The proxy takes a message reference, not a URL

The endpoint is **not** `GET /proxy?url=…`. It is a reference to an image *in a message
this user already has* — message id plus the image's index in the server's own parked
list — and the server resolves the URL from the stored message.

The URL is still attacker-chosen (a spammer picks what is in their mail), so §3a is still
mandatory. What this removes is the endpoint being a **general-purpose SSRF gadget**:
there is no way to ask it for an address that does not already appear in mail delivered to
the asking user, and RLS scopes that to the asking user.

### 3c. It fetches only what the client would have loaded anyway

The proxy refuses unless the message's sender is allowed, or the user has asked for this
message. Otherwise the proxy would become a way to make the server fetch content the
blocking policy exists to prevent — a bypass wearing the costume of a privacy feature.

⚠ **This is where the fail-closed decision lands.** `store.RemoteAllowsMatch` mirrors the
*client's* rule, deliberately, because the question a report asks is *"what would the
client do"*. The proxy asks *"should I fetch this"*, and those differ on malformed
addresses — an address with two `@`, or one in angle brackets. **The proxy must not reuse
that predicate.** It needs its own, stricter, fail-closed match, and the two must be
tested against each other so the divergence stays deliberate.

### 3d. Beacons stay suppressed, and the proxy never sees them

A parked beacon (v1.42.0) has no proxy reference and cannot acquire one. Proxying a
tracking pixel would *reintroduce* the tracking the previous increment removed — the
sender still learns the message was opened, only from a different address. The proxy is
for pictures.

### 3e. The four settled decisions, and what each one costs

The appendix holds the analysis. These are the outcomes, with the obligations they create.
Two went against this ADR's recommendation; both are recorded with what has to be true for
them to be sound, because a decision whose price is not written down is a decision nobody
can hold to later.

**A. `http` is permitted, alongside `https` — and this is a stronger choice than the
recommendation it overrode.** The argument for refusing `http` was that a broken image is
the cheaper failure. Checking the render path after the decision showed that reasoning had
the situation backwards:

> `internal/api/sanitize.go` classifies `http://`, `https://` and protocol-relative `//`
> alike as `srcRemote` and parks them; `web/src/app/ui/messageDocument.ts` restores a
> parked URL by rewriting `data-mailroom-remote-src="` straight back to `src="`, and **the
> browser** performs the fetch. Mailroom is served over `https`, so a restored `http://`
> image is plain-http passive content on an https page — which modern browsers block or
> fail to upgrade. Protocol-relative `//host/path` resolves to `https` and was never part
> of the question.

So `http`-only images **do not render today at all**, and `https`-only proxying would have
preserved that while appearing to be the cautious option. Proxying them is the only way
they are ever seen: the server fetches over `http` and serves the bytes to the browser
over the app's own `https` origin, which converts a blocked subresource into a working
one. The cleartext hop moves from browser→sender, where it is blocked, to server→sender,
where it works.

The cost is real and accepted: cleartext egress from the server, on-path substitution of
those images (a swapped picture in a sandboxed CSP frame, not code execution), and a guard
that reasons about two URL classes rather than one. §3a confines the widening to the proxy
so `webpush` does not inherit it. `proxy-report` still measures the split — no longer to
decide this, but to size what is being enabled.

**B. The cache reuses the blob store.** What that buys is genuine: encryption at rest
under the user's own DEK for free, content-addressed dedup that matters at this shape
(thousands of images from hundreds of hosts, where logos and spacers repeat heavily), and **no second
store for #54's hard-delete sweep to learn about** — the blob store is already in it.

⚠ **What it costs is three prerequisites, none of which exist today.** Reusing the store
is not the same as reusing it unchanged:

1. **A blob class, and the migration to add it.** `app.blobs` is keyed
   `(user_id, content_hash)` with no kind or class column; nothing can currently tell a
   cached third-party image from the user's own mail. Forward-only and
   expand-and-contract, per ADR-008.
2. **`StorageQuota` must exclude that class.** It is
   `SELECT COALESCE(sum(size_stored), 0) FROM app.blobs`, unconditional
   (`internal/store/quota.go`). Left alone, a stranger's marketing images consume the
   quota that bounds the user's own mail — and a user pushed over it stops being able to
   receive or send. This is not a tidiness point; it is the cache breaking mail delivery.
3. **The reclaim guard needs a fourth referent.** `reclaimUnreferencedBlobs`
   (`internal/store/blobreclaim.go`) allowlists exactly three: messages, attachments and
   `ml_models.artifact_hash`. A cache entry is referenced by none of them, so a hash the
   cache shares with a purged attachment would have its row reclaimed and its file deleted
   out from under the cache — and per that file's own comment, deleting the row destroys
   the `dek_id` the ciphertext is opened with, so the bytes do not leak, they become
   permanently unreadable. **This is #300's `ml_models` hazard exactly, and finding it a
   second time by accident would be the more expensive way to learn it.**

Two further things are accepted rather than fixed: cached third-party bytes now enter the
**restore unit** (ADR-005) and are backed up; and a **cap plus eviction still has to be
built**, because the blob store reference-counts and never ages anything out. Decision B
supplies the store, not the expiry.

**C. On by default in its first release.** The recommendation was a release off first, to
shake out failures on a real mailbox. That reasoning does not survive contact with the
deployment: the instance had exactly **one user**, so a quiet release shakes
out nothing that dogfooding it switched on does not surface sooner and more honestly.

⚠ **The obligation this transfers is that the first release must be complete.** Rate
limiting and the per-message cap ship *with* the endpoint — Consequences already said they
belong in the design rather than a follow-up, and on-by-default removes the release in
which that could have slipped. **Fail-open remains forbidden**: a proxy that cannot fetch
yields a broken image, never a direct browser load, because a silent fallback would undo
the feature at the exact moment the user is told it is protecting them.

**D. Nothing is stripped in the first version.** As recommended, and for the reason in the
appendix: for images the recipient is usually encoded in the path or a required token, so
an over-eager rule 404s the picture and **fails silently**, which is the failure mode
increment 2 was built to avoid. The proxy already defeats what those parameters are for.
To be revisited on the §6 measurement.

**⚠ C and D are both deviations from spec §11, and this ADR is where they are recorded.**
The spec asks for an *"opt-in image proxy that strips tracking params"*; C makes it on by
default and D strips nothing yet. Neither is a reinterpretation of the spec's intent — the
spec wants remote content controlled and senders denied a signal, and both decisions serve
that — but they are departures from its literal text, and per `CLAUDE.md` the departure
belongs in an ADR rather than in a commit message. D is explicitly temporary. C is a
strengthening: the spec's "opt-in" guards against the server fetching things it otherwise
would not, and §3c already forbids that, so the protection the word was carrying is
supplied by a stricter mechanism than the default was.

## 4. What ships in what order

1. **`internal/egress` + `webpush` adopts it.** Fixes a live unguarded path, and the guard
   gets exercised before anything depends on it. **Shippable alone**, and it is the piece
   with real security value regardless of whether the proxy is ever built.
2. **Measure** what the proxy would do (§6), because the case for it in §1 is narrower
   than the feature's name and the numbers should decide the shape.
3. **The proxy endpoint**, honouring §3b–3d, permitting `http` and `https` (A), **on by
   default (C)** — and therefore shipping complete, with its rate limit and per-message cap
   in the same change rather than behind them.
4. **Caching in the blob store** (B), which is what buys the only timing protection
   available. It carries its own prerequisites — the class column and migration, the
   `StorageQuota` exclusion, and the fourth reclaim referent (§3e) — and those land with
   it, not after it. §6 sizes the cap; it no longer decides whether.

## 5. What remains open

The four questions this section used to hold are settled in §3e. What genuinely remains
open is narrower, and all of it is sizing rather than shape:

- **The numbers**: the byte cap per fetch, the wall-clock timeout, the redirect-hop limit,
  the cache's size cap and TTL, and the rate limit and per-message cap. §6's size
  distribution is what sets them; each has a safe conservative starting value in the
  meantime.
- **Which tracking parameters, if any, turn out to be strippable** — deferred with a
  documented reason (D), reopened only by the §6 measurement that fetches both forms.
- **Whether `http` stays permitted.** Decided yes and justified in §3e, but if
  `proxy-report` shows the `http` population is negligible, the widening buys little and
  can be withdrawn. Reversible in either direction.

## 6. The measurement that should come before the endpoint

`mailroom-admin proxy-report`, read-only, over the images that would actually be proxied
(remote images in messages from allowed senders, beacons excluded):

- how many hosts, and how many are reachable over `https` versus `http`-only — which no
  longer decides A, but sizes it: this is the count of images that **render only because
  the proxy exists**, since an `http` image is mixed content the browser blocks today
  (§3e). If it is negligible, A can be withdrawn;
- the size distribution, which sets the byte cap and the cache budget;
- how many URLs carry known tracking parameters, and what stripping them would change;
- how many resolve to a **non-public address today** — the number that says whether the
  SSRF guard would ever fire on real mail, or is purely a guard against the hypothetical.

That last row is the one this project would regret skipping. A guard that has never been
observed to refuse anything is a guard nobody has evidence works.

## 7. What §6 actually measured (v1.44.0)

`proxy-report` ran on a real corpus. The findings are recorded here because
several of them contradict figures this ADR reasoned from.

⚠ **The population is a handful of hosts, not hundreds.** §1 and §6 both quote the
whole-corpus host and image counts. The population a
proxy would ever touch — remote images in mail from senders the proxy's own rule allows,
beacons excluded — is far smaller.
The gap matters most for §4 step 4: a cache for a handful of hosts with heavy repetition is a
different object from a cache for hundreds.

| §6 row | result |
|---|---|
| hosts, `https` vs `http`-only | **a handful of hosts, all `https`. Zero `http`.** |
| size distribution | median a few KiB; p99 a few hundred KiB; max about 1 MiB |
| known tracking parameters | **zero.** The actual parameters were signed-URL `token-hash` / `token-time` |
| resolves to a non-public address | **none** |
| *(added)* client-allows minus proxy-allows | **zero** — the stricter §3c rule costs nothing here |

**Decision A stands, with its benefit measured at zero on the measured corpus.** `http`
was kept permitted: the measurement covers only the *currently eligible* population —
a few grants, a handful of hosts — and allowing one more sender could surface `http` images,
which render only through the proxy. The widening is built, tested, and confined to the
proxy, so keeping it costs nothing now. §5's "can be withdrawn" remains available.

⚠ **Decision D is now evidence rather than prediction, and the evidence is stronger than
the argument was.** The appendix argued that the familiar `utm_*`/`fbclid` list is a
*link*-tracking phenomenon and would mostly 404 an image. It matches **zero** URLs here,
so it would not have stripped anything at all. What is present instead is a signed URL
with an expiry — and 9 of 120 sampled fetches returned **HTTP 403, all of them on the one
host carrying `token-time`**. Those parameters are not trackers to be removed; they are
the reason the image loads.

**Two things this hands to §4 step 3.** The byte cap has evidence behind it (max observed
1004 KiB, so a cap in the low megabytes is generous). And a proxied fetch can fail for an
ordinary, non-hostile reason — an expired signature — so "render a broken image, never
retry into a fallback" is a case that occurs in practice, not only in theory.

## Consequences

- **A new egress path exists, and it is a permanent security surface.** Everything in §3a
  is load-bearing, and each item wants a test that watches it refuse — a guard asserted
  only by "it fetched the good URL" is the vacuous-pass shape this project keeps paying
  for (#131, #242, #332).
- **`webpush` changes behaviour**: a subscription endpoint that resolves to a private
  address stops working. That is the fix, but it is a behaviour change and existing rows
  should be checked before it lands.
- **The proxy makes the server fetch attacker-chosen URLs on a schedule the attacker
  partly controls** (they choose when to send the mail; the user chooses when to open it).
  Rate limiting and a per-message cap belong in the design, not in a follow-up — and
  decision C removes the release in which that could have slipped, since the endpoint is
  live for the only user the day it lands.
- **The blob store stops holding only the user's own bytes.** Decision B puts third-party
  images into the store that is also the restore unit (ADR-005) and the thing
  `StorageQuota` sums, so two properties that were previously free — *everything in
  `app.blobs` is the user's mail*, and *everything in `app.blobs` is worth backing up* —
  become things a class column maintains rather than things the schema guarantees. §3e
  lists the three changes that keeps honest. The reclaim guard is the one where being
  wrong destroys bytes rather than merely miscounting them.
- **The benefit varies with where the reader is, and the UI should not pretend otherwise.**
  Reading from the same network as the server hides the browser but not the address;
  reading from anywhere else hides both. Whatever the feature says about itself has to be
  true in both cases, which argues for describing what it does ("images are fetched by
  your server") rather than what it achieves ("your location is hidden").

---

## Amendment (#667): the app fetches a proxied image, not the message frame

**What this ADR assumed.** §3b made an allowed image a same-origin `/jmap/proxy/…` path,
and the client restored that path as the image's `src` inside the message frame, for the
frame to fetch.

**Why that failed on a cookie-gated deployment.** The proxy route is authenticated
by the session cookie, which is `HttpOnly` and `SameSite=Lax`. The message frame is
sandboxed without `allow-same-origin` (and must stay so: `docs/hostile-content.md` §2.1),
so its origin is opaque, a request from it is cross-site, and the browser withholds the
cookie. Every proxied image arrived unauthenticated and was refused. That is confirmed in
Chromium and in Firefox. Production's cache shows some images did load through the proxy
on one earlier day, from a client that was not identified, so it is not claimed for every
browser. The dev-auth
harnesses have no credential to withhold, the one OIDC lane used curl, and no lane loaded
a proxied image, so nothing in CI could see it. It was found in use, as "not all images are loading".

**Decision.** With the proxy on, the *app* fetches each allowed image: a same-origin
request, with the cookie, by the trusted page. It accepts only the raster types the proxy
re-serves, converts the bytes to a `data:` URL, and builds the frame's document from
those. This is what the client already did for `cid:` images.

What follows from it:

- **The frame fetches nothing.** With the proxy on, the frame's document is always built
  with the *blocked* policy (`img-src data:`), for an allowed message too, and no parked
  URL is restored into it. A parked image that did not resolve is removed and shows its
  alt text. "The frame is never handed a proxy path" is a unit-tested property, and a
  browser lane (`scripts/proxied-images-e2e.sh`) signs in through a real OIDC flow and
  asserts the image decodes inside the frame.
- **§3c is unchanged, and its order is now enforced.** The one-off request for a message
  is awaited before the first image is asked for. Before, it was sent and not awaited, so
  the first images of an "always allow" click could be refused and stay broken until the
  message was reopened. A remembered grant still makes no request on the user's behalf:
  the proxy's own, stricter match decides.
- **§1 is unchanged.** The server still makes the only outbound fetch. The browser never
  contacts the sender, and a beacon is still parked under a name nothing restores (§3d).
- **The reader is told what did not arrive.** The app knows each image's outcome, so the
  banner reports how many could not be loaded instead of saying they are loading.

What it costs:

- The frame's document carries the images inline, at four thirds their size. One message's
  images are capped (16 MiB in total, 8 MiB each, the proxy's own ceiling); anything past
  that is reported as not loaded.
- The images of a message arrive together rather than one by one, and the frame is
  rebuilt once when they do.
- The browser's image cache is not used for them. The server cache (decision B) is.

**Rejected.**

- *`allow-same-origin` on the frame.* It is the token whose absence makes
  `allow-scripts` safe. Forbidden, and pinned by tests.
- *`SameSite=None` on the session cookie.* It gives up the cookie's CSRF defence for every
  route, and browsers that partition third-party cookies would still withhold it from an
  opaque-origin frame.
- *A signed capability URL outside the cookie gate.* It breaks the pinned property that
  nothing under `/jmap/` is reachable without a session, and it would still be stopped by
  an authenticating reverse proxy in front of the app.

**A deployment note.** An authenticating reverse proxy in front of Mailroom should not
answer a non-navigation `/jmap/*` request with a redirect into an interactive login.
Mailroom gates `/jmap/*` itself and answers 401. With the redirect, each failed image
became two extra requests against the identity provider.

**With the proxy off** (`MAILROOM_IMAGE_PROXY=false`) nothing changes: the parked URL is
the sender's own, the browser fetches it, and the message policy admits `https:`.

---

## Amendment (#922): fetch allowed senders' images at arrival, and keep them while the message is kept

**What went wrong.** §7 measured some sampled fetches answering 403, all on one host
whose image URLs carry `token-time` / `token-hash`: a signed URL with an expiry. The proxy
fetched only when a message was opened, so in practice an older message from that
sender, opened for the first time, showed broken images, while newer mail from the same
sender loaded. Nothing at open time can fix an expired signature. The only fix is to fetch
while the signature is valid, which means at arrival.

**Decision.**

- **Prefetch at arrival, for exactly the senders §3c lets the proxy fetch for.** Ingest
  queues one job per newly stored message whose sender passes `ProxyRemoteAllowsMatch`,
  the proxy's own fail-closed rule, and never the client's. The job checks the rule
  again when it runs, so a grant withdrawn in between withdraws the fetch. A one-off
  "show remote content" click is not a reason to prefetch: it happens at open, and stays
  an open-time fetch. Ingest only queues the job: one read of the grants and one INSERT,
  under a savepoint in the ingest transaction, so a failure cannot cost the mail. The fetching runs off the ingest path, in the job dispatcher.
- **The same fetch, not a second fetcher.** The job takes the image list an open would
  serve (`proxyEligibleImages`: what the shipped sanitizer parks, §3d's beacons excluded,
  the per-message cap applied) and fetches through the same `openProxyImage` and
  `storeProxyImage` as an open. That means the same egress guard, schemes, timeout,
  redirect limit, byte cap, content-type allowlist, cache key and sealed blob storage. A
  prefetch cannot fetch or keep anything an open would refuse. Opening a prefetched
  message is an ordinary cache hit and makes no upstream request.
- **Beacons are never prefetched.** §3d unchanged.
- **An arrival window, default 7 days** (`MAILROOM_IMAGE_PROXY_PREFETCH_MAX_AGE`). Mail
  received longer ago than this is not queued, so a first sync or an import of years of
  history does not contact every sender in it at once. Mail already stored when this
  shipped is not backfilled. Live mail is prefetched within seconds whatever the window,
  so the window is a bound on bursts and not a guess at how long signatures last.
- **Rate.** The prefetch shares the reader's per-user, per-minute window but is admitted
  only into its first half, so a burst of arriving mail cannot make the message being
  opened answer 429. One job run is time-boxed (10 s, plus at most one fetch in flight),
  because the dispatcher runs jobs one at a time and a scheduled send must not wait behind
  a slow sender. Whatever is left becomes a continuation job (at most 20 per message).
  Only a run that ran out of *time* moves to the next round; a run that ran out of
  *rate* re-queues the same round at the window reset, so waiting for the reader's
  budget never spends the rounds (review M3). Rate deferrals are bounded on their own
  (60 per message, about an hour of waiting), after which the message is left to open
  time like one whose rounds ran out.
- **Retention: kept as long as a message that references it is kept** (decided with the prefetch).
  `app.proxy_image_refs` (migration 00081) records "this message shows the image cached
  under this URL hash". A referenced entry is exempt from the 30-day unread TTL **and from
  the size cap**. The reference is released by the message row's deletion, through
  `ON DELETE CASCADE`, so every destroy path releases it with no sweep to remember: the
  Trash purge's hard delete, account removal, duplicate collapse (which first moves a
  copy's references onto the keeper), and a retired draft. **Trash does not release**:
  Trash is restorable, and an image released at trash time could not be fetched again
  after an undo. Once released, an entry is an ordinary one and ages out by
  `last_used_at` like any other. For a prefetched image nobody opened, that is its fetch
  time, so it usually goes on the next pass. Images cached by an ordinary open, with no
  reference, keep today's behaviour.
- **The cap now bounds unreferenced entries only, and it is 2 GiB** (decided the same
  day). `CacheBytes` keeps its meaning otherwise: zero takes the default, negative turns
  the cache off, and with it the prefetch, which would have nowhere to put what it
  fetches. Referenced bytes neither count toward the cap nor become victims of it.
- **A ceiling on referenced bytes that never evicts** (review H1, same day). Per user,
  `MAILROOM_IMAGE_PROXY_PREFETCH_MAX_BYTES`, default 16 GiB; negative means no limit,
  which an operator may opt into; 0 takes the default. Each prefetch run reads the
  user's referenced bytes first, and at or over the ceiling it fetches nothing
  (`cap_reached`, counted in `mailroom_proxy_prefetch_total`, and one WARN per user per
  hour at most). Nothing already kept is evicted: a kept image's URL may have expired.
  New mail's images then load at open, as before. The check is per run, so one run may
  overshoot by at most one message's images (200 × 8 MiB in the worst case).
- **Gates the prefetch adds and an open does not** (review M1). A grant names a sender
  by From, and From is forgeable. An open is the reader acting on what they see; a
  prefetch is the server acting on the From alone, before anyone has looked. So the
  prefetch also requires:
  - **authentication**: not stored as failing (`messages.auth_fail`), and authenticated
    by *the same verdict learned placement acts on* (`ingest.LearnedSender`: one From
    mailbox in one header, matching the stored address, `Authenticated()` for its
    domain). With trusted authserv-ids configured that is a trusted, relaxed-aligned
    DMARC pass. Without them it is only "did not clearly fail", exactly as for learned
    placement, and the server warns at boot that it is running that way. There is no
    second reading of `Authentication-Results`;
  - **not junk**: the arrival mailbox is not junk-role (checked at enqueue), and the
    message is not in a junk-role mailbox when the job runs;
  - **default ports only**: 443 for `https`, 80 for `http`, on the first URL **and on
    every redirect hop**. The rule rides on the request (`egress.WithRequestCheck`),
    which the egress client applies wherever it validates a hop, so a redirect cannot
    carry the prefetch to another port. Anything else is refused as `port_refused` and
    logged like any other fetch failure. The open path does not apply this rule.
- **The registrable domain on failed fetches, never the hostname** (refined by
  review M2). The decision was to log the host on fetch failures. It is logged as the
  registrable domain (eTLD+1, by the public suffix list), because wildcard-DNS trackers
  put recipient tokens in hostname labels (`u-8f3a91.img.tracker.example`), and the full
  hostname would copy the token into the log just as the path would. Under a
  *private*-section suffix (`github.io`, `workers.dev`, a cloud provider's bucket
  domain) eTLD+1 is a customer's own label, which can itself be the token, so the line
  names the ICANN-registrable domain of that suffix instead (`github.io`). That also
  drops the variable label of a wildcard private rule. An IP literal is logged as
  `ip-literal`, because a sender holding an IPv6 /64 can encode a token in the address
  bits. That includes the IPv4 spellings a resolver accepts and a strict parser does
  not (`0x7f.1`, `2130706433`, `127.1`). A host the list cannot reduce is logged as
  `unregistrable`. The
  line for a fetch failure (`egress_refused`, `scheme_refused`, `too_many_redirects`,
  `upstream_timeout`, `client_cancelled`, `upstream_error`, `upstream_status`,
  `content_type`, `too_large`, `body_aborted`, `bad_request`, and the prefetch's own
  `port_refused`) gains `host` with that value: never a port, userinfo, path or query.
  The path and query carry the recipient's token. Authorization
  and request-shape refusals, and `rate_limited`, carry no host, because they answer "may
  this reader have this image" and a host would say which sender's mail a refused request
  was about. Success and cache hits are still not logged. The allowlist lives in
  `noteProxy` (`hostLoggedReasons`), so a caller cannot put a host on the wrong line. The
  host is never a metric label.

**The privacy trade, stated plainly.** A fetch at arrival tells the sender *when the mail
was delivered*, which their own mail server already knows. It stops telling them *when
the reader opened it*. §1 said the proxy does not hide open time, and that only the cache
did, from the second open onward. For prefetched images that is now true from the first
open. What the sender gains is confirmation that this address accepts mail and that its
images are fetched. For an allowed sender (someone the reader chose to load images from),
an open would have given them that anyway. The fetch still comes from the server, so §1's
browser and location protection is unchanged.

**What bounds the growth instead of eviction.** Nothing evicts a referenced image, so
growth is bounded by the referenced-bytes ceiling (16 GiB per user by default, which
stops the prefetch and evicts nothing), `MaxBytes` per image (8 MiB), `PerMessage` per
message (200), the allowed-senders rule, the authentication and junk gates, and the
arrival window. `mailroom_proxy_cache_bytes` /
`mailroom_proxy_cache_entries` by `kind` keep the referenced part visible, and
`mailroom-admin proxy-report` prints the same split. It also prints a read-only count of
what the prefetch would fetch for recent mail, so the operator can read the number before
the feature runs.

**What it cannot fix.**

- Images whose URLs had already expired before this shipped, or before the mail
  first reached Mailroom (a late first sync, an import), stay broken. The prefetch only
  gets ahead of an expiry it arrives in time for.
- A sender who is allowed only after the mail arrived: the prefetch checked the rule at
  arrival and found no grant. That mail's images load at open, as before.

**change_log.** None. `app.proxy_image_cache` writes have never appended a change_log row,
and `app.proxy_image_refs` does not either. Both are operational state, not JMAP-visible
objects, and nothing a client syncs changes when an image is cached or released.

---

## Appendix: the four decisions, as they were put

**These are settled — the outcomes and their obligations are §3e.** This appendix is kept
as written, with each recommendation left standing and the outcome recorded beneath it,
because two of the four went the other way and the reasoning that lost is the part worth
being able to re-read. One of those recommendations was simply wrong on the facts, and
tidying it away would hide why.

### A. Plain `http`, or `https` only?

**What is actually at stake.** Not confidentiality of the *content*: these are marketing
images, and anyone positioned to read the fetch already sees the DNS lookup and the TLS
SNI. What `http` costs is **integrity** — an on-path party can substitute the image — and
**a second data point about the reader**, since a cleartext request advertises exactly
which resource the server pulled.

The integrity risk is smaller than it first sounds: the substituted content is a
third-party image that already goes into a sandboxed frame under a CSP, and cannot script.
The realistic harm is a swapped picture, not code execution.

Against that, `https`-only has a real cost: an image that does not load. That failure is
**visible and recoverable** — the reader sees a broken image and can still open the mail
elsewhere — which is the better failure direction.

| option | for | against |
|---|---|---|
| **`https` only** | consistent with the rest of the posture; no cleartext egress; simplest guard | breaks `http`-only senders, and nobody knows how many that is |
| **allow `http`** | nothing breaks | cleartext egress; a second class of URL for the guard to reason about |
| **`https`, plus `http` behind a setting** | recoverable if the measurement is bad | a setting nobody will find, and two code paths |

⚠ **This is unmeasured and cheap to measure.** Of the hosts in the corpus, the split
between `https` and `http`-only is one row of `proxy-report` (§6). Deciding first and
measuring after is the pattern #55 has now avoided three times.

**Recommendation: `https` only, and let the measurement reopen it.** If the report shows a
material number of `http`-only hosts carrying images a reader would miss, revisit with
evidence. Reversible in either direction; failing closed first is the cheaper mistake.

> **Outcome: `http` is permitted (§3e).** ⚠ And the recommendation above was **wrong on a
> fact it did not check**. Its whole case rests on "an image that does not load" being a
> new cost imposed by refusing `http` — but the render path shows those images do not load
> *today*: the client restores a parked URL directly into `src=` and the browser blocks
> plain-http content on the https page. `https`-only would have preserved an existing
> breakage while presenting itself as the careful option, and the table above never
> considered that proxying is what *fixes* those images. The row it needed —
> "what does the client currently do with an `http` URL" — was one file away and unread,
> which is the same shape as every measurement lesson in `CLAUDE.md`.

### B. Where does the cache live?

**What is actually at stake.** A cache is the only thing that hides *when* a message was
opened, from the second open onward. It is also a new store of third-party bytes, and —
less obviously — **a record of which images this person loaded and when**, which is itself
sensitive and interacts with #54 (export and hard delete).

| option | for | against |
|---|---|---|
| **reuse the blob store** | exists; encrypted at rest; content-addressed, so identical images dedupe | it is the **restore unit** (ADR-005: Postgres + blobs + key store). Third-party images would be backed up, and would count against `StorageQuota`, which is meant to describe the user's own mail |
| **a separate cache: size cap + TTL, not backed up** | matches an existing precedent exactly; disposable; bounded; easy to purge | new code; a second store to reason about in #54 |
| **no cache at first** | least code; no new sensitive store | gives up the only timing protection available; every re-open re-pings the sender |

⚠ **The precedent is exact and already in the spec.** The search index is described as *"a
disposable cache, rebuilt from Postgres"* that is deliberately **not** backed up (spec
§7/§11), because a restored-but-stale index is worse than none. A proxied-image cache is
the same category of thing: derived, disposable, reconstructible by re-fetching.

**Recommendation: a separate cache, capped and TTL'd, explicitly excluded from backup,**
following the search index's precedent — and shipped *after* the endpoint, so the endpoint
can be evaluated without it. Whatever is chosen, the cache must be named in #54's
hard-delete sweep: "delete my data" that leaves a directory of images the person was sent
is not hard delete.

> **Outcome: the blob store (§3e).** The "for" column understated it — dedup is worth more
> at this shape than the row suggests, and being *already* inside #54's sweep is a
> correctness benefit rather than a convenience, since the separate store would have been
> a new thing to remember. The "against" column understated it too: this needs a class
> column and a migration, a `StorageQuota` exclusion without which a stranger's images can
> stop the user receiving mail, and a fourth referent in the reclaim guard. §3e carries
> all three. The size cap and TTL are still required — the blob store never ages anything
> out — so the eviction work was never what this choice avoided.

### C. Is the proxy on by default?

**The reframing that decides this.** §3c already constrains the proxy to fetch **only what
the client would otherwise have fetched itself**. So turning it on does not increase what
leaves the machine — it **moves** an existing fetch from the reader's browser to the
server. The question is not "should the server start fetching things", it is "when a fetch
is going to happen anyway, should it come from the reader's current location or from the
server's".

Put that way, and with §1's correction that the reader is routinely not at home, the case
for on-by-default is strong.

| option | for | against |
|---|---|---|
| **on by default** | the benefit is automatic; a privacy feature nobody enables protects nobody; no new egress, only relocated egress | a proxy outage becomes broken images; server bandwidth and latency; the server is now the thing touching trackers |
| **opt-in** | conservative; nothing changes for anyone who does not ask | almost nobody will turn it on, so the feature mostly does not exist |
| **on by default after one release off** | shakes out failures on a real mailbox before it is load-bearing | one extra release of nothing happening |

⚠ **The rule that matters more than the default: never silently fall back to a direct
fetch.** A proxy that fails open — proxy unavailable, so the browser fetches directly —
would undo the entire feature at exactly the moment the user is told it is protecting
them, and would do it invisibly. A failed proxy fetch must be a **broken image**, which is
visible, not a direct load, which is not.

**Recommendation: ship off by default for one release, then on by default,** and treat
fail-open as forbidden rather than as a fallback.

> **Outcome: on by default in the first release (§3e).** The recommendation's "one extra
> release of nothing happening" was costed as free and is not: the instance has one user,
> so the shakeout release shakes out nothing that switching it on does not surface sooner.
> The fail-open prohibition carries over untouched — it was never contingent on the
> default — and the rate limit and per-message cap now have to be in the first version.

### D. Which tracking parameters to strip?

⚠ **The uncomfortable part first: for images, stripping usually breaks the image.** The
familiar strippable parameters (`utm_*`, `fbclid`, `gclid`, `mc_eid`, `mkt_tok`) are a
**link**-tracking phenomenon. An image URL more often encodes the recipient in the path or
in a required token, and removing it yields a 404 rather than a clean picture.

So the risk profile is the one increment 2 navigated with the 1×1 rule: an over-eager rule
**fails silently**, by removing content the reader was meant to see, and nobody finds out.

There is also a structural point that lowers the stakes: **the proxy already defeats the
main purpose of those parameters**, because whatever the URL says, the sender now sees the
server rather than the reader.

| option | for | against |
|---|---|---|
| **strip nothing at first** | no silent breakage; the proxy's main benefit is unaffected | spec §11 asks for stripping, so this is a deviation to record |
| **strip a known-safe list** | some benefit; familiar | unmeasured here — the list is drawn from link tracking, not image tracking |
| **strip only what is measured to be removable** | evidence-led; no silent breakage | needs a measurement that fetches both forms, so it depends on the guard existing |

**Recommendation: strip nothing in the first version, and measure.** The measurement is
"for image URLs carrying a known tracking parameter, does the image still resolve without
it" — which requires fetching both ways and therefore comes after `internal/egress`. If
that shows a safe subset, strip that subset and say which. Until then, a documented
"stripping deferred, and here is why" is more honest than a list copied from link-tracking
advice and applied to a different problem.

> **Outcome: strip nothing, and measure (§3e).** As recommended — the only one of the four
> that survived unchanged.

### What was actually decided

`https` **and `http`**, because `http` images are blocked in the client today and proxying
them is what makes them work · the cache in the **blob store**, which costs a class column,
a `StorageQuota` exclusion and a fourth reclaim referent · **on by default immediately**,
never failing open, with the rate limit and per-message cap in the same release · no
parameter stripping until measured.

Each remains reversible. What changed relative to the recommendations is that two of them
had priced a failure as free when it was not: a `https`-only guard would have preserved an
existing breakage, and a shakeout release shakes out nothing on a single-user instance.
