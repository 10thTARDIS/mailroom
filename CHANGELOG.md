# Changelog

All notable changes to Mailroom are recorded here. The format loosely follows
[Keep a Changelog](https://keepachangelog.com/). Versioning is semantic (spec
§16): `1.0.0` marked the MVP milestone. From there on, MINOR bumps carry
additive features, PATCH bumps carry fixes, and a MAJOR bump is reserved for a
breaking change. (Before 1.0.0, each delivered feature **wave** bumped the MINOR
version.) `[Unreleased]` accumulates changes since the last tag.

## [Unreleased]

### Faster

- **Checking for new mail uses far less database time.** Mailroom asks the database "has anything changed?" every few seconds, for every open window and for its search index. On a server with one user, the database had settled on a way of answering that read the whole change history each time. It now works out the answer from the index, so this constant background check costs a small fraction of what it did. The same fix applies to the sender lookup used when scoring new mail.

## [1.99.0-beta.2] — 2026-11-01

**The second beta of 1.99.0: when you keep moving one sender's mail by hand, Mailroom offers to make a rule.** Learned placement can't help with every sender: some have been filed in more than one place, and some send mail that can't be confirmed as genuine. Until now you just kept moving their mail yourself. This beta notices, and offers a one-tap rule right under the move.

### Sorting

- **Mailroom now offers to make a rule when you keep moving the same sender's mail by hand.** After you file mail from one sender into an area for the third time in 30 days (by dragging it to the sidebar, using Move to area, the Labels menu or Re-file), a message appears under the move: "Always put mail from alice@example.com in Updates?" with "Make a rule" and "Not now". The Undo for the move stays where it was.
- **It only asks when it will not already be handled.** If learned placement is going to file that sender's next message there itself, or a rule you have already turned on does, you are not asked. You are asked when learned placement is switched off or paused for that area, when your moves for that sender are not consistent, or when the sender's latest message could not be confirmed as genuine.
- **"Not now" is remembered for 30 days** for that sender and area. It does not stop the offer for other senders or other areas. If you ignore the offer, it goes away when the move's Undo does, and you may be asked again next time.
- **"Make a rule" makes an ordinary rule you can see and edit** under Rules, named "Mail from alice@example.com → Updates". It applies to future mail only. If older mail from that sender is somewhere else, Mailroom then asks "Also move the 12 earlier messages?" and moves nothing until you tap "Move them" (with its own Undo).

### Upgrading

- One database migration (00085). It adds a small table that remembers "Not now" answers. It is quick and needs no downtime.

## [1.99.0-beta.1] — 2026-10-31

**The first beta of 1.99.0: Mailroom now tidies up after its own background work.** Every finished background job used to leave a record behind forever. This beta deletes old ones once a day and adds a report for the person who runs Mailroom. Nothing about reading or sorting mail changes. Later betas add learned placement improvements, and the final 1.99.0 adds OAuth sign-in for AI tools.

### Housekeeping

- **Old background-job records are cleaned up automatically.** Every snooze, scheduled send, attachment scan, Trash purge and drawer sweep leaves a small record behind when it finishes, and until now nothing ever removed them, so the table only grew. Mailroom now deletes finished records after 30 days, and records of jobs that failed for good after 90 days, once a day. Jobs that are still waiting or running are never touched.
- **You can change how long they are kept** with `MAILROOM_JOB_RETENTION` and `MAILROOM_DEAD_JOB_RETENTION` (at least 24 hours each).
- **The person who runs your Mailroom can see what is there** with `mailroom-admin jobs-report`, which lists the records by kind and state and how many the daily clean-up would remove. It changes nothing.
- **A job that fails for good now records when it failed**, so the clean-up can age it.

### Upgrading

- One database migration (00084). It changes a foreign key so a sent message's record no longer blocks clean-up, and adds the clean-up functions. It is quick and needs no downtime.

### Under the hood

- **Dependency updates come from Dependabot**, weekly, with security fixes as soon as an advisory is published. Code is also scanned with CodeQL on every change.

## [1.98.1] — 2026-10-31

**Mailroom is now developed in the open on GitHub.** This release adds no new features and changes nothing in how Mailroom behaves. It is the first release published from the public repository: the server image now comes from GitHub's container registry, release notes appear on the GitHub releases page, and the README has been rebuilt to show what Mailroom is and where it is going. Every earlier release, back to the first, is described in the project's CHANGELOG.md.

### Where to get it

- **The image is `ghcr.io/10thtardis/mailroom:1.98.1`.** Pin it by digest, as before; `:latest` is published only as a convenience and is never a pin.
- **No registry login is needed** to pull it.
- **Each release has a page on GitHub** whose notes are the matching section of CHANGELOG.md. Releases up to 1.98.0 were published before the move and are recorded only there.

### Upgrading

- Point `MAILROOM_IMAGE` at `ghcr.io/10thtardis/mailroom:1.98.1` (by digest) and recreate the stack.
- There are no database migrations and no configuration changes. Going back to 1.98.0 is safe.

### Documentation

- **A new front page.** The README now shows what Mailroom is and does at a glance, with screenshots, a picture of how it fits together, the roadmap with links to each planned feature, and an invitation to contribute.
- **New guides:** a contributing guide, a developer guide, a documentation index and a page on the licence. Running and development instructions moved out of the README into these.
- **Documentation brought up to date.** The feature list reflects what has actually shipped (Send & archive, the MCP server and API keys, Outlook sign-in and more were still listed as planned), every planned feature points at its issue on the public tracker, and references to the old private tracker are gone.

### Under the hood

- **Issues and pull requests are public.** Planned work is tracked as GitHub issues under the 1.x, 2.x and 3.x milestones, and contributions are welcome (see CONTRIBUTING.md).
- **Every pull request runs the full test suite on GitHub Actions** before it can merge, including the database isolation tests, the end-to-end browser checks and the accessibility checks.
- **A release is published only from a commit whose tests passed on `main`.** The image must report the tagged version before it is pushed.
- **Privacy checks before anything is published.** Pull request titles and commit messages are checked, along with the code, so nothing about a particular installation reaches the public repository.
- **More reliable accessibility checks.** A timing problem could make the contrast check report a false failure while a pop-up was still drawing.

## [1.98.0] — 2026-10-30

**Stay signed in, and see your signed-in devices.** You sign in once on each device and stay signed in. Mailroom checks with your sign-in provider in the background while you use it, so you're no longer sent back to the sign-in page several times a day. A new Settings page lists every device you're signed in on and lets you sign any of them out.

### Stay signed in

- **One sign-in per device.** You stay signed in for as long as your sign-in provider allows, and closing the browser no longer matters.
- **No more bounces.** Mailroom renews your session itself, server to server, at most every 15 minutes while you use it. An open tab, a half-written email or the installed app is never sent back to a sign-in page mid-task.
- **How long a device can sit unused depends on your sign-in provider.**
  - If the provider issues a fresh refresh token on every renewal, the limit (for example 30 days) counts from your last use.
  - If it doesn't, the limit counts from when you signed in, and the device signs in again when it runs out.
  - Either way, if you're still signed in to the provider, that is usually just a quick redirect.
- **Changes at your provider reach Mailroom within about 15 minutes of use:**
  - revoking Mailroom's access for you;
  - disabling your account, if your provider ends Mailroom's access when it does;
  - adding or removing you from the admin group.
  
  You don't need to sign in again for any of these.
- **A provider outage doesn't sign anyone out early.** If the provider can't be reached, sessions keep working for up to 12 hours after their last check, the same as a whole session lasted before this release.
- **Disabling someone in Mailroom signs them out on their next click,** not just at their next sign-in.

### Signed-in devices

- **Settings › Signed-in devices** lists the browsers you're signed in on, with this device first. Each one shows the device (for example "Firefox on Linux"), when it was last active, when it signed in, and where from.
- **Sign out any device, or all the others at once.** A signed-out device:
  - is refused on its very next action;
  - stops receiving updates within a minute;
  - stops getting notifications.
  
  Mailroom also asks your sign-in provider to cancel that device's access, when the provider supports it.
- **The page tells you what else to do about a device you don't recognise.** Signing it out ends that sign-in, but whoever used it may still know your password, so change your password at your sign-in provider too.
- **Notifications come back by themselves** when you sign in again on a browser where you'd already allowed them.

### Safety

- **Signing out really ends the session on the server,** so a copied cookie stops working.
- **A database hiccup no longer looks like being signed out.** Open tabs wait for it to recover instead of jumping to the sign-in page.
- **A crafted sign-in or sign-out link** could send you to another website after signing in or out. That is closed.

### Upgrading

- **Everyone signs in once more after this update,** because the old sign-in cookies aren't accepted.
- **For quiet renewal, your sign-in provider must issue refresh tokens.** In Authentik:
  - add the `offline_access` scope mapping to the Mailroom provider;
  - set its Refresh Token validity to the idle limit you want (e.g. `days=30`).

  Without refresh tokens, everything still works and sessions last 12 hours, as before.
- **Optional:** behind a reverse proxy, set `MAILROOM_TRUSTED_PROXIES` to the proxy's address. The devices page then shows each device's real address rather than the proxy's.
- **Migrations 00082 and 00083** add the session records and link notifications to them. Both are additive. `deploy/compose.yml` gains the optional `MAILROOM_TRUSTED_PROXIES` setting.

### Under the hood

- **Sessions are rows in `app.sessions`.** The cookie is a random secret, stored only as a hash and looked up through one narrow database function. The provider's refresh token is sealed per user and per session.
- **Renewal runs in three steps:**
  1. under a per-session lock, the provider call, then storing any new token straight away;
  2. re-reading your identity, with no lock held;
  3. a final confirmation.

  A hiccup at any step can neither sign you out nor lose a token.
- **New JMAP capability `urn:mailroom:devices`,** with `SignedInDevice/get` and `SignedInDevice/set`. API keys can't use either.
- **Old session records are cleaned up daily:** sessions signed out more than 7 days ago, or unused for 45 days. The counts are logged by kind.
- **Tested against a real provider before release** (ADR-048 §9). Silent renewal, the role refresh, revoking access at the provider and remote sign-out all worked. Ending your provider's own sessions did not sign a Mailroom device out, and the devices page's advice was corrected to match. Every step of the work had an independent security review before merging.

## [1.98.0-rc.1] — 2026-10-30

**Test pre-release: stay signed in, and see your signed-in devices.** You sign in once on each device and stay signed in. Mailroom quietly checks with your sign-in provider every 15 minutes while you use it, so you no longer get bounced to the sign-in page several times a day. A new Settings page lists every device you're signed in on and lets you sign any of them out. Not for production: this release candidate is for testing against a real sign-in provider before the final release.

### Stay signed in

- **One sign-in per device.** You stay signed in for as long as your sign-in provider agrees, provided you use the device at least once in the provider's idle period (30 days in the reference setup). Closing the browser no longer matters.
- **No more bounces.** Mailroom renews your session server-to-server, so an open tab, a half-written email or the installed app never gets sent back to a sign-in page mid-task.
- **Changes at your provider reach Mailroom within about 15 minutes of use.** That covers being disabled, having your sessions ended, or being added to or removed from the admin group, with no new sign-in needed.
- **A provider outage doesn't sign anyone out early.** If the provider can't be reached, sessions keep working for up to 12 hours since their last check, the same as a whole session lasted before this release.
- **Disabling someone in Mailroom signs them out on their next click,** not just at their next sign-in.

### Signed-in devices

- **Settings › Signed-in devices** lists the browsers you're signed in on, with this device first. Each shows the device ("Firefox on Linux"), when it was last active, when it signed in, and where from.
- **Sign out any device, or all the others at once.** A signed-out device is refused on its very next action, stops receiving updates within a minute, and its notifications stop. Mailroom also asks your provider to cancel that device's sign-in token.
- **The page is honest about what that achieves.** If you don't recognise a device, also change your password or end your sessions at your sign-in provider, or it can simply sign in again.
- **Notifications come back by themselves** when you sign in again on a browser where you'd already allowed them.

### Safety

- **Signing out really ends the session on the server,** so a copied cookie stops working.
- **A database hiccup no longer looks like being signed out:** open tabs wait for it to recover instead of jumping to the sign-in page.
- A crafted sign-in or sign-out link could send you to another website after signing in or out. That is closed.

### Upgrading

- **Everyone signs in once more after this update,** because the old sign-in cookies aren't accepted.
- **For quiet renewal, the sign-in provider must issue refresh tokens.** In Authentik: add the `offline_access` scope mapping to the Mailroom provider, and set its Refresh Token validity to your idle limit (e.g. `days=30`). Without that, everything works and sessions last 12 hours, as before.
- **Optional:** behind a reverse proxy, set `MAILROOM_TRUSTED_PROXIES` to the proxy's address so the devices page shows real addresses rather than the proxy's.
- Migrations 00082 and 00083 add the session records and link notifications to them. Both are additive.

### Under the hood

- Sessions are rows in `app.sessions`. The cookie is a random secret stored only as a hash, looked up through one narrow function. The provider's refresh token is sealed per user and session.
- Renewal runs in three steps:
  1. under a per-session lock, the provider call, then storing the new single-use token straight away;
  2. re-reading your identity, with no lock held;
  3. a final confirmation.

  A hiccup at any step can't sign you out or lose a token.
- New JMAP capability `urn:mailroom:devices`, with `SignedInDevice/get` and `SignedInDevice/set`. API keys can't use either.
- Old session records are cleaned up daily (signed out over 7 days ago, or unused for 45 days), with counts logged per kind.
- The design is ADR-048. Every step had an independent security review before merging.

## [1.97.0] — 2026-10-28

**Your other mail apps can now catch up with day-one setup, plus quicker sorting from the reading pane.** When you first set up your desk, Mailroom filed your older mail away in Mailroom only, so your phone and your provider's webmail still show it all sitting in the inbox. This release adds a careful, report-first command that moves that mail into your Archive folder on the server, so every app agrees. You can also re-file a message straight from the reading pane, and mark several messages important at once.

### Day-one archiving, now on the server too

- **What it fixes.** Day-one setup takes mail older than its window off your desk without touching the server. That was deliberate, but it leaves thousands of messages in the inbox your phone and webmail show.
- **The person who runs your Mailroom runs one command:** `mailroom-admin day-one-server-archive --user-id …`. On its own it changes nothing. It reports:
  - how many day-one archives are still in the server's inbox;
  - how many it would move, and to which folder;
  - how many it would leave alone, and why, with a separate count for each reason.
- **It only moves mail that is plainly safe to move.** It never touches a message you've starred, pinned, snoozed, put in Trash or moved yourself since setup. It skips one that also has a copy in another folder, because moving it would pull that copy into Archive too. It also skips any account with no Archive folder or with writeback switched off.
- **`--apply` does the moves** exactly as if you had archived each message yourself, and Mailroom then moves them on the server over the next few sync passes. **`--limit N`** moves only the oldest N first, so you can check the result in webmail before doing the rest.
- **Afterwards, the report shows how the moves went** (done, still in progress, or failed), so nothing gets stuck unnoticed.
- **One thing to know:** once mail has moved, undoing day-one setup no longer puts it back in your other apps' inboxes.

### Sorting

- **Re-file a message while reading it.** The reading pane's More menu now has Re-file…, which opens your areas and moves the message there, with Undo, without going back to the list.
- **Mark several messages important at once from the desk.** The desk's and the tray's selection bar now have Mark as important.

### Fixes

- **Marking a mixed selection as important no longer un-marks it.** If the first message you'd selected was already important, Mark as important used to mark the whole selection not important. It now marks them all important unless every one already is, says how many it changed, and offers Undo.

### Under the hood

- `Triage/correct` (marking messages important or not) accepts at most 1,000 messages per call, the most a "select all matching" batch sends, so one call can no longer walk an unbounded list through one transaction.
- A test that checks the encryption-key service never leaks library error text failed now and then for no real reason: it looked for the letters "MDC" anywhere, and they turn up by chance inside message ids. It now looks for the word as the library writes it.

### Upgrading

- Nothing to do: no database changes and no new settings. The new command is in the same image as the server.

## [1.96.0] — 2026-10-28

**Outlook and Microsoft 365 by signing in, and images that stay put.** People who use Outlook or Microsoft 365 can now connect their account from Settings by signing in with Microsoft: no passwords, no admin commands. Gmail accounts can be connected the same way, but Gmail itself stays paused in this release; it is switched on in an update after a live test with real accounts. Separately, newsletters now keep their pictures: images from senders you allow are saved when the mail arrives, so they still show weeks later even when the sender's links have expired. Area drawers open faster, plain-text email reflows to your screen, and there is a batch of phone, layout and reliability fixes.

### Connect Outlook or Gmail by signing in

- **The person who runs your Mailroom sets it up once.** `docs/oauth-providers.md` walks through registering a Google and a Microsoft app (about 15 minutes each) and the `MAILROOM_OAUTH_*` settings. Until that's done, nothing new appears.
- **Then it's one button.** Settings › Accounts shows **Connect Outlook** and **Connect Gmail**. A short page first explains what you're agreeing to:
  - Mailroom will be able to read, send, organise and delete the mail in that account, and so, technically, could whoever runs the server.
  - Google will call the app "unverified". That's expected for a privately run server, and the page says how to continue.
  - Outlook.com users need to switch IMAP on in Outlook's settings first. Work or school accounts may need an administrator's approval.
- **You're told plainly how it went**: connected, cancelled, wrong account, mail access not granted, and so on.
- **A connected account shows how it signs in** (provider, address, whether sign-in is working) instead of server settings, with **Reconnect** and **Disconnect…**. Disconnecting keeps your mail.
- **Outlook accounts start syncing straight away.** A connected Gmail account says "Gmail sync arrives in the next update; your account is connected and ready."

### When an account needs you to sign in again, nothing is lost

- If you change your password or remove Mailroom's access, a **banner** names the account with a **Reconnect** button, and you get **one** notification on your devices, not one per sync.
- While you wait, Mailroom stops trying to sync that account, and your changes (read, archive, labels) are held, along with mail already queued to send. Everything carries on after you reconnect.
- New mail can't be sent from that account until then. Mailroom tells you when you press Send and keeps your draft. The Outbox shows "Waiting for you to reconnect…" instead of "Sending…".
- **A broken server setup is never blamed on you.** If Google or Microsoft rejects the server's own setup (an expired secret, say), you're told it's a server problem, admins see a notice, and outgoing mail waits in the Outbox until it's fixed.

### Outlook and Microsoft 365 work properly

- **Folders are recognised** (Sent Items, Drafts, Deleted Items, Junk Email, Archive) even when Microsoft's server doesn't label them. Outlook's empty Calendar, Contacts, Notes, Tasks and Outbox folders aren't synced.
- **Read, unread and star changes made on your phone now reach Mailroom.** Microsoft's servers don't report these changes the way others do, so Mailroom now re-checks each folder's newest 500 messages every 5 minutes and the whole folder every 6 hours. Only real changes are applied, and a change you just made here is never flipped back.
- **If your organisation blocks sending from apps**, the account becomes **receive-only** and says why, with what to ask your administrator, instead of failing quietly.
- **"IMAP access is off" is explained**, with where to turn it on, instead of a bare "authentication failed".
- A new Outlook account can send up to 100 messages a day through Mailroom, a deliberately cautious limit.

### Images that still show weeks later

- **Some senders sign their image links, and those links expire.** An older newsletter opened for the first time used to show broken pictures while newer mail from the same sender looked fine.
- **Mailroom now saves the images of mail from senders you allow when it arrives**, while the links still work, and **keeps them as long as you keep the message**. Moving a message to Trash keeps its images, because you can still restore it. Deleting it for good releases them.
- **Only for mail you'd trust:** senders you've allowed remote content from, mail that passes Mailroom's sender check, never Junk, and never tracking pixels.
- **The trade:** the sender learns when the mail was delivered, which their mail server knows anyway, and no longer learns when you open it.
- Saved images are limited to 16 GB per person by default. At the limit, new mail's images simply load when you open it, as before, and nothing already saved is deleted.
- This starts with mail that arrives after the update. Images whose links have already expired can't be recovered.

### Also fixed

- **Area drawers open faster** on big mailboxes: in measurements against a copy of the real query, up to about 3–4 times faster on the busiest areas.
- **Plain-text email reflows to your screen** when the sending program marked its line wrapping as soft (`format=flowed`), so on a phone it reads as paragraphs, not alternating long and short lines.
- **Wide newsletters fit a phone's screen** instead of sitting in a box you swipe sideways; pinch to zoom.
- **The search box has room for your query** on a very narrow phone, and **the reading pane no longer scrolls sideways** on a laptop screen. The folder list on a wide screen no longer scrolls sideways either.
- **A cut-off server reply can no longer mark your mail as unread** or clear its stars and labels. **Mail recovered after a dropped download** now notifies you and respects blocked senders.
- **A server that rejects your password now says "sign-in failed"** instead of a generic error, and a refused new account isn't retried until it's locked.
- **Encryption keys whose name contains angle brackets are read correctly**.
- **Attachments of encrypted mail are decrypted and scanned once per download**, not twice. A blocked attachment opened in a new tab now explains itself instead of showing a bare "403 Forbidden".
- **Merging duplicate copies of a message keeps what Mailroom learned about them**.

### Security

- The HTML parsing library is updated (golang.org/x/net 0.62.0) for advisory GO-2026-6860, where a specially built message could make reading it very slow.

### Not in this release yet

- **Gmail sync and write-back are built and tested but switched off** until a live test with real accounts confirms how Gmail handles labels, its daily download limit and moves out of Trash. They are switched on in a follow-up update.
- Renaming or deleting a label isn't sent to Gmail yet.
- Sign-in for AI tools comes in v1.97.

### Under the hood

- Database migrations **00078–00081** add the sign-in state and sign-ins in progress, the Gmail import bookkeeping, the receive-only mark, and which messages keep a saved image. All are additive and run automatically on start.
- New optional settings, each taking its built-in value when left empty:
  - sign-in: `MAILROOM_OAUTH_GOOGLE_CLIENT_ID` / `_SECRET`, `MAILROOM_OAUTH_MICROSOFT_CLIENT_ID` / `_SECRET` / `_TENANT`, `MAILROOM_PUBLIC_URL`;
  - Gmail: `MAILROOM_GMAIL_DAILY_DOWNLOAD_MB`, `MAILROOM_GMAIL_LABEL_SWEEP_INTERVAL`;
  - read/unread re-check: `MAILROOM_FLAG_SWEEP_INTERVAL`, `_WINDOW`, `_FULL_INTERVAL`;
  - saved images: `MAILROOM_IMAGE_PROXY_PREFETCH`, `_MAX_AGE`, `_MAX_BYTES`. The cache for images saved by opening alone rises from 256 MiB to 2 GiB per person.
- New metrics for the flag re-check, recovered arrivals, image prefetch and saved-image storage. A failed image fetch now logs the sender's main domain, never the full link.
- The IMAP library is carried in the repository with small, documented patches for Gmail (`third_party/go-imap/MAILROOM-PATCHES.md`).
- `mailroom-admin gmail-census` previews a Gmail import without downloading mail. `proxy-report` now shows what the image prefetch would fetch and the images the proxy never sees.
- The learned-placement tests now prove the filings they rely on were recorded.
- Design records: ADR-046 (sign-in and tokens), ADR-047 (Gmail All Mail and labels), and an ADR-026 amendment (image prefetch and retention).

### Upgrading

- **Self-hosted with Docker Compose:** pull the new image and copy the updated `deploy/compose.yml`, which has the new optional settings. Migrations run on start.
- **To offer Outlook or Gmail sign-in:** follow `docs/oauth-providers.md`. Without it, nothing changes.

Tested across two release candidates. Each piece of the sign-in work, and the image change, had an independent security review, with every finding fixed before release.

## [1.96.0-rc.2] — 2026-10-27

**Second test pre-release: images that stay put, faster area drawers.** Images from senders you allow are now fetched when the mail arrives and kept for as long as you keep the message, so a newsletter opened weeks later still shows its pictures even when the sender's links have expired. Opening an area's drawer is faster on big mailboxes, and a security update to the HTML parser is included. Everything in rc.1 (Gmail and Outlook sign-in) is still here, and Gmail sync is still off until the live test. Not for production.

Migration 00081 (additive). `deploy/compose.yml` gains three optional image-prefetch settings; each takes the built-in value when left empty.

### Added
- **Images from senders you allow now still show weeks later.** Some senders sign the links to their pictures, and those links stop working after a while, so an older message opened for the first time showed broken images while newer mail from the same sender looked fine. Mailroom now fetches the images of mail from senders you allow as soon as the mail arrives, while the links still work, and keeps them for as long as you keep the message. Moving a message to Trash keeps its images (you can still restore it); deleting it for good releases them. Tracking pixels are still never fetched, and nothing changes for senders you have not allowed. It only does this for mail that passes the same sender check Mailroom uses before filing mail for you, never for mail in Junk, and only from the standard web ports. Kept images are limited to 16 GiB per person by default (`MAILROOM_IMAGE_PROXY_PREFETCH_MAX_BYTES`); when that is reached, new mail's images load when you open it, as before, and nothing already kept is deleted. The trade: the sender learns when the mail was delivered, which their own mail server knows anyway, and no longer learns when you opened it. Mail already in your mailbox before this update, and mail more than 7 days old when it is first synced, is not fetched in advance: its images load when you open it, as before, and images whose links have already expired cannot be recovered. It can be switched off with `MAILROOM_IMAGE_PROXY_PREFETCH=false`.

### Fixed
- Opening an area's drawer is faster on large mailboxes: Mailroom now works out which messages belong to the area only among the mail on your desk instead of across the whole area.

### Security
- Updated the HTML parsing library (golang.org/x/net 0.62.0) for advisory GO-2026-6860: a message built to make its text pieces merge over and over could make reading it very slow. Mailroom's sanitizer and digest code use that parser.

### Under the hood
- The tests for learned placement now check that the filings they teach it from were actually recorded, and each was probed by removing its filings; one test whose sender mail the desk already put in the same area had been recording none.
- `mailroom-admin proxy-report` now also counts remote images that are dropped before they could be proxied (background= attributes, `<picture>` sources, percent-encoded `cid:` references), so the decision on how much further to go can be made from real mail.
- When the image proxy fails to fetch an image, its log line now names the sender's domain (for example `tracker.example`, never the full server name or the rest of the link, either of which can identify you), so one sender's expiring links can be told apart from a broken proxy. Refusals about who may see an image still name nothing.
- `mailroom-admin proxy-report` has a new section that counts, before anything runs, how many images the arrival fetch would download for recent mail and how many are already cached, and shows how much of the image cache is kept for messages versus held by ordinary opens (`--prefetch-window`, default 7 days). It contacts no sender.
- Migration 00081 adds `app.proxy_image_refs`, which records which messages keep a cached image. It is additive. `deploy/compose.yml` gains three optional settings, `MAILROOM_IMAGE_PROXY_PREFETCH` (default on), `MAILROOM_IMAGE_PROXY_PREFETCH_MAX_AGE` (default 168h) and `MAILROOM_IMAGE_PROXY_PREFETCH_MAX_BYTES` (default 16 GiB per user, negative for no limit); each takes the built-in value when left empty. The cap on images cached by ordinary opens rises from 256 MiB to 2 GiB per user, and images kept for a message no longer count toward it. New metrics: `mailroom_proxy_prefetch_total{result}` (including `cap_reached`), and `mailroom_proxy_cache_bytes` / `mailroom_proxy_cache_entries` by `kind` (referenced or unreferenced), so the kept images, which nothing evicts, stay visible.

## [1.96.0-rc.1] — 2026-10-27

**Gmail and Outlook sign-in: first test pre-release.** People who use Mailroom can now connect a Gmail or Outlook / Microsoft 365 account by signing in with Google or Microsoft, from Settings, with no passwords and no admin commands. Outlook accounts sync straight away. Gmail accounts connect but stay paused: the Gmail sync and write-back are built and tested against a stand-in, and are switched on only after a live test with real accounts. This pre-release also carries the day's fixes: reflowing plain-text email, phone and desktop layout, encryption-key names, attachments and sign-in errors. Not for production.

### How to try it

- Register a Google and a Microsoft app for your Mailroom, following `docs/oauth-providers.md` (about 15 minutes each). Then set the `MAILROOM_OAUTH_*` settings and restart.
- In Settings → mail accounts, use **Connect Outlook** with a throwaway Outlook.com account (switch IMAP on in Outlook.com first) and **Connect Gmail** with a throwaway Gmail account.
- Outlook mail should arrive. Read something on your phone and check it shows as read in Mailroom within a few minutes. Gmail should show "connected, sync arrives in the next update".
- `mailroom-admin gmail-census` previews what a Gmail import would fetch, without downloading any mail.

### Not in this pre-release yet

- Gmail sync and write-back stay off until the live test confirms how Gmail behaves with labels, its daily download limit, and moves out of Trash.
- Renaming or deleting a label in Mailroom isn't sent to Gmail yet.
- OAuth sign-in for AI tools comes in v1.97.

Migrations 00078–00080 (all additive). `deploy/compose.yml` gains three optional settings for the read/unread sweep. Each defaults to the built-in value when left empty.

### Added

- **An operator guide for letting people connect Gmail and Outlook accounts.** `docs/oauth-providers.md` walks through registering the Google and Microsoft apps, the settings Mailroom needs, what people will see, and what to do when a connection fails.
- **Connect a Gmail or Outlook account by signing in, from Settings.** Once the person who runs your Mailroom has set up Google or Microsoft sign-in, Settings › Accounts shows **Connect Gmail** and/or **Connect Outlook**. If neither is set up, nothing new appears. Before you leave for Google or Microsoft, a short page explains what you're agreeing to:
  - Mailroom will be able to read, send, organise and delete all the mail in that account, and whoever runs the server could technically do the same.
  - Google will warn that the app is "unverified". That is expected for a privately run server, and the page says how to continue.
  - Outlook.com users have to switch IMAP on in Outlook's settings first, and work or school accounts may need their administrator's approval.

  When you come back, a message says plainly what happened: connected, cancelled, the wrong account, mail access not granted, and so on. It shows once and doesn't come back if you reload. **Gmail sync itself arrives in the next update.** A connected Gmail account says so and has no sync switch until then. Outlook accounts start syncing straight away.
- **A connected account shows how it signs in, instead of server settings.** You see the provider, the address and whether sign-in is working, with **Reconnect** and **Disconnect…** buttons. Disconnecting keeps your mail. If Mailroom couldn't remove its own access at Google or Microsoft, it tells you where in your Google or Microsoft account to remove it.
- **When an account needs you to sign in again, Mailroom says so and nothing is lost.** This happens if you change your Google password or remove Mailroom's access, for example. A banner across the app names the account and has a **Reconnect** button. It can be dismissed until your next visit, and you get **one** notification on your devices, not one per sync. While you wait:
  - Mailroom stops trying to sync that account.
  - Your changes (read, archive, labels) are held, and so is mail already queued to send. All of it carries on after you reconnect.
  - New mail can't be sent from that account until you reconnect, and Mailroom tells you so when you press Send. Your draft is kept. The same applies to an account you disconnected.
  - The Outbox shows a waiting message as "Waiting for you to reconnect …", with a **Reconnect** button, instead of "Sending…". You can still cancel it.
- **A broken server setup is never blamed on you.** If Google or Microsoft rejects the server's own sign-in setup (an expired client secret, say), you're not told to reconnect. The account says it's a server problem and to contact the person who runs it. Admins see a notice in Settings › Accounts naming the provider, and the server logs it as an error. Your outgoing mail isn't lost while it's broken: it waits in the Outbox ("Sign-in to Microsoft is temporarily unavailable on this server") and goes out once the problem is fixed. You can still press Send in the meantime.

- **Groundwork for Gmail sync, and a way to see what it would do first.** Connected Gmail accounts stay paused: nothing syncs from Gmail until the next step. What is ready:
  - **A read-only Gmail census for the person who runs the server** (`mailroom-admin gmail-census`). Before a Gmail account's first import it reports how many messages and how much mail there is, how it would be sorted into Inbox, Sent, Drafts, Archive, Spam and Trash, which Gmail labels already exist in Mailroom and which would be new, how many messages per year, how many days the import would take, and how many messages Mailroom already has from another account. It never downloads a message, and it says so when it only looked at a sample.
  - **How Gmail will sync once switched on.** Mailroom reads Gmail's All Mail, Spam and Trash once each, never the label folders, so each message is downloaded only once. Your Gmail labels become Mailroom labels with the same names (joining any label you already have of that name), and Gmail's Inbox, Sent and Drafts decide the folder. Gmail's own "Important" marks and category tabs are not imported. A label you change in Gmail shows up in Mailroom.
  - **Large mailboxes import newest first, a day at a time.** New mail arrives from the first minute; older mail follows within a download limit of 2,000 MB in any 24 hours by default. That is below the 2,500 MB a day Google documents for Workspace accounts, to keep Gmail from pausing the account's mail on your phone and other apps; Google's exact figure and counting window for ordinary Gmail accounts are not published, so this is a safety margin, not a guarantee. Account health shows how far the import has got and roughly how many days are left. Switching the account's sync off stops the import, and switching it on again carries on where it stopped.
  - Changes you make in Mailroom are not sent back to Gmail yet; that is a later step.
- **Changes you make in Mailroom will be written back to Gmail, the Gmail way, once Gmail sync is switched on.** Until the person who runs your Mailroom switches it on, nothing is sent to Gmail. After that:
  - **Archive** takes the message out of Gmail's Inbox and leaves it in All Mail, exactly as Gmail's own Archive button does. **Move to Inbox** puts it back.
  - **Labels** you add or remove in Mailroom are added or removed as Gmail labels with the same name, including names with accents or other non-English letters, so you see them in Gmail's own apps. A label Mailroom doesn't find in Gmail is created there. Removing a label never deletes the message. Labels Mailroom keeps for its own sorting areas stay in Mailroom.
  - **Delete** moves the message to Gmail's Trash, where Gmail keeps it for 30 days. **Restore** brings it back. **Spam** and **Not spam** move it to and from Gmail's Spam.
  - **Read, unread and star** are the same in Gmail (Gmail's star is Mailroom's flag).
  - Mailroom only ever erases a message for good inside Gmail's Trash or Spam, never anywhere else, so what happens doesn't depend on your Gmail settings. A message is always written to in the Gmail account it belongs to, even when the same label exists in another account. A change Mailroom made isn't mistaken for a change made in Gmail the next time Mailroom checks your labels, so the two never undo each other.
- **Outlook and Microsoft 365 accounts get their folders right.** Sent Items, Drafts, Deleted Items, Junk Email and Archive are recognised as your Sent, Drafts, Trash, Junk and Archive folders even when Microsoft's server does not label them, so sending, deleting and "Not junk" use the right places. Outlook's own Calendar, Contacts, Notes, Tasks and Outbox folders are not synced when they show up empty the first time Mailroom sees them. A folder with one of those names that holds mail, such as your own "Notes" folder, keeps syncing, and you can switch any of them back on and it stays on. This works for English folder names; a mailbox set up in another language relies on the server labelling its folders. A new Outlook account can send up to 100 messages a day through Mailroom, a deliberately cautious limit because Microsoft's real limits vary and going over them locks sending everywhere.
- **When your organisation blocks sending from apps, the Outlook account becomes receive-only and says why.** Some workplaces and schools turn off sending mail from apps (Microsoft's "SMTP AUTH", code 5.7.139). Mailroom now notices, at connection or on the first send. The account keeps receiving mail. Account health shows "can receive mail but not send it" with what to ask your administrator, and pressing Send explains the same thing and keeps your draft. Mail already waiting to go out from that account fails with the same explanation rather than sitting at "Sending…" for days, because only an administrator can change this. Once they have, reconnect the account in Settings and Mailroom checks again.
- **"IMAP access is off" is now explained.** If Microsoft signs you in but its mail server still refuses Mailroom, the account's health now says IMAP access is off for the mailbox and where to turn it on (Outlook settings › Mail › Forwarding and IMAP, or your administrator for a work account), instead of a bare "authentication failed". You don't need to reconnect.
- **The Gmail account's Settings page shows how far its import is.** A Gmail account that is syncing shows, in its State section, how many messages are imported so far (and how many are on the server, once that is known), and roughly how many days the import will take at the pace that stays within Google's daily download limit. When the whole history is imported it simply says so. Pausing the account's sync stops the import, as before.
- **Read, unread and star changes made on your phone now reach Mailroom for mail servers that don't report them (Outlook/Microsoft 365 and some others).** These servers don't support CONDSTORE, the IMAP feature Mailroom used to learn about flag changes, so a message read on your phone stayed unread here. Mailroom now re-reads the read/star state of each folder's newest 500 messages every 5 minutes, and of the whole folder every 6 hours, a few bytes per message. Only real changes are applied, and a message you've just changed here is left alone until that change has reached the server, so it is never flipped back by an old reading. On a server that can't store Mailroom's labels, the labels are kept rather than wiped. If the server refuses this check, new mail still arrives. Servers that support CONDSTORE are unaffected. The timings can be changed (`MAILROOM_FLAG_SWEEP_INTERVAL`, `MAILROOM_FLAG_SWEEP_WINDOW`, `MAILROOM_FLAG_SWEEP_FULL_INTERVAL`).

### Fixed

- **A cut-off or split server reply can no longer mark your mail as unread.** If a mail server's reply was cut off partway, or sent a message's read/star state in a second piece, Mailroom could read "no flags" and clear read, starred and labels on that message until something changed it again. A reply without the flags is now treated as "no answer" and the message is left as it was. A brand-new message that arrives this way is still delivered, and its flags are fetched again straight after.
- **Mail recovered after a dropped download now notifies you and respects blocked senders.** When Mailroom finds a message that an earlier, interrupted download skipped, it fetches it again. Such a message never sent a notification to your devices, and one from a blocked sender, though still filed away as blocked, was left out of the blocked count. It now behaves like any other new mail.

- **Tidying up duplicate copies of a message no longer forgets what Mailroom knew about them.** When two copies of the same message are merged into one, the copy that is removed used to take two records with it: the note that Mailroom had filed it by learning where your mail goes, and the note that the first-day tidy-up had labelled it. So if you then took a learned label off the surviving message, Mailroom could not count it as a correction, and undoing the first-day tidy-up left that label behind. Both notes now move to the surviving message. If both copies had their own note, the one recording a correction of yours is kept, otherwise the surviving message's own. Where two copies were given different labels by the same first-day run, the merge is skipped for that message and reported, because one note cannot hold both. The merge's preview (`collapse-duplicates`, which changes nothing unless you add `-apply`) now counts each kind separately. Merges made before this fix cannot be recovered.
- **On a wide screen the folder list on the left no longer scrolls sideways.** The "Folders" heading was a little wider than the list it sat in, so the whole list could be dragged about 16 pixels to the left. It now fits. The automatic accessibility check treats this as a failure instead of a note, and proves on every run that it would catch it.

- **Plain-text email from programs that mark their line wrapping now reflows to your screen.** Many mail programs wrap plain text at a fixed width but mark those wraps as "soft" (`format=flowed`). Mailroom now rejoins them into paragraphs, so on a phone such a message reads as normal paragraphs instead of alternating long and short lines. Real line breaks, quoted text and signatures stay as sent. Messages that are hard-wrapped *without* that marker are unchanged for now.
- **A mail account whose server rejects the sign-in now says so.** Some servers, Microsoft's among them, refuse a wrong or expired password with a bare "no" and no reason attached. Mailroom used to file that as a generic protocol error; it now shows "sign-in failed", and a new account that is refused this way is no longer retried over and over, which could get it locked.
- **Encryption keys whose name contains angle brackets are read correctly.** A key owner named something like "Alice <b>Bold</b> <alice@example.org>" used to be read as having the address "b", so Mailroom would not encrypt to the key or recognise its signatures, and showed the wrong address. The address is now the last bracketed part, and only when it is a plain address at the very end of the name. Anything unclear is treated as having no address, so a key can never claim one it doesn't contain. The list of keys and the check used for encrypting and signature matching now read names the same way, and the order of a key's names and its "primary" name no longer varies between runs.
- **Downloading an attachment from encrypted mail now decrypts and scans it once.** It used to be checked first and then fetched, so each click decrypted and scanned it twice, wrote two audit entries and used up the decryption allowance twice as fast. Downloads of such attachments are now one request, and PDF previews and "open in a tab" no longer make a check first. Attachments of ordinary mail are still checked in advance.
- **A blocked attachment opened in a new tab now explains itself.** Right-click "Open link in new tab" (or a preview in a frame) on an attachment the scanner blocked used to land on a bare "403 Forbidden". It now shows a short page saying the attachment was blocked and why, with the scanner's detection name when there is one. The file is still never served, and the response is still a refusal.
- **Wide newsletters fit a phone's screen.** A newsletter designed for a 600 to 700 pixel page used to sit in a box you had to swipe sideways to read. On a phone it is now shrunk to the width of the screen, the way most mail apps show it, and you can pinch to zoom in on the detail. Mail that already fits, plain-text mail and responsive newsletters are left alone, and remote images, links and the protections around message content work exactly as before.
- **The search box has room for what you type on a very narrow phone.** On a 320 pixel screen the box's own buttons used to leave no room at all for the query (about six characters at 375 pixels). Below 400 pixels the options and save buttons now stay out of the way until you tap into the box, and when you do, the menu, refresh and settings buttons step aside so the whole bar is the search. At least a dozen characters of your query are visible in both states.
- **The reading pane no longer scrolls sideways on a laptop screen.** At 1440 pixels wide the row of action buttons above a message was wider than the pane, so the whole pane could be dragged sideways. The row now notices when it does not fit and moves Reply all and Mute into the More menu, and brings them back when the pane is wide enough, whatever split you have chosen. On a wide screen you still see every button.

### Under the hood

- **Microsoft profile, flag sweep and the two sync fixes (ADR-046 §12).**
  - Migration 00080 (expand-only) adds `send_blocked_code` and `send_blocked_at` to `app.account_sync_status`: operational state, no change rows. `Account/getHealth` gains `sendBlocked {code, message, since}`; `EmailSubmission/set` refuses with the new SetError type `accountCannotSend`. A connection change on a password account clears the mark; a connect or reconnect of a Microsoft account probes SMTP AUTH and marks or clears it. `535 5.7.139` no longer hands the token back for a pointless refresh. Exchange's `5.2.252 SendAsDenied` gets its own permanent cause. Both are recognised only from the server's own SMTP reply code and enhanced status, never from an error's text, so a dropped connection to an address that happens to contain those digits stays an ordinary retryable failure.
  - The folder-name map, the non-mail folder list, the Microsoft send cap and the IMAP-off hint live in `internal/provider/profile.go`. The hint applies when IMAP refuses the token the provider issued in place of a refused one (`sync.ErrIMAPAccessRefused`).
  - The flag sweep runs only when the server advertises neither CONDSTORE nor QRESYNC, reads FLAGS only, compares each reading with what is recorded and applies only differences (through the same flag refresh as the CONDSTORE pass), and applies nothing from a command that failed. Its schedule is kept in memory: a restart only makes it run sooner.
  - The vendored go-imap gets a second small patch: `FLAGS ()` is an empty list and a reply without FLAGS stays "absent" (`third_party/go-imap/MAILROOM-PATCHES.md`). The engine skips flag-less readings on every refresh path.
  - **Measurements first.** Nothing the server recorded could answer either issue's question. A failed flag pass logged the mailbox error but not the refreshes it had already applied, and nothing told gap-backfill arrivals apart from the rest. So there are four new counters at `/metrics`: `mailroom_flag_refresh_aborted_total{path}` (flag readings applied by a FETCH command that then failed), `mailroom_fetch_flags_absent_total{path}` (replies missing the FLAGS that was asked for), `mailroom_sync_recovered_arrivals_total{path,outcome}` (new mail from the gap backfill and the flag-pass fallback, by notify/muted/blocked) and `mailroom_flag_sweep_messages_total{result}`. The per-account "account synced" log line also carries `gaps_backfilled`, `flags_absent`, `flag_refresh_aborted`, `flag_sweep_read` and `flag_sweep_applied`.
- **Gmail read path (ADR-047), inert until Gmail sync is switched on.**
  - Mailroom's IMAP library (go-imap v2.0.0-beta.8) is now carried in the repository under `third_party/go-imap`, with one small change: it can read Gmail's message id, conversation id and labels from a server response. Without it the whole response failed. The change and its upstream status are written down beside it (`MAILROOM-PATCHES.md`), and the build uses it through a `replace` line in `go.mod`.
  - Migration 00079 (expand-only, no rewrite) adds the bookkeeping: each mailbox's newest-first import floor and label-sweep time and progress, each Gmail message copy's last-seen labels and conversation id (with an index for threading by it), and each account's download meter (the last 24 hours, in hourly buckets).
  - New settings `MAILROOM_GMAIL_DAILY_DOWNLOAD_MB` (default 2000) and `MAILROOM_GMAIL_LABEL_SWEEP_INTERVAL` (default 6h; how often label changes are re-read while it is unconfirmed that Gmail reports them another way).
  - Gmail accounts are forced to ingest-only: the store suppresses every writeback for them whatever their setting says, and turning writeback on for one is refused. Gmail accounts do not hold an IDLE connection (they poll every five minutes), because IDLE would sit on Gmail's Inbox label folder.
- **Gmail writeback (ADR-047 §3a), inert until it is switched on.**
  - The vendored IMAP library's patch is extended (patch 3 in `MAILROOM-PATCHES.md`): it can add and remove Gmail labels (`STORE ±X-GM-LABELS`, system labels as atoms, other labels quoted in modified UTF-7) and search by Gmail's message id (`SEARCH X-GM-MSGID`).
  - The writeback engine has a Gmail mode, chosen from the account: archive and labels become label changes, delete and spam become moves to Trash and Spam, and a permanent delete moves to Trash first and erases only there. Before writing, it finds the message's current position by Gmail's message id, and it reads every label change back before counting it as done. No new queue operations and no migration: a label change in the queue now also carries the label's name.
  - The switch is `GmailWritebackReady` (in `oauthmail` and `store`; a test checks the two agree). It stays off in this release: a Gmail change is only recorded in the ingest-only ledger, never queued to run, and the writeback runner never connects to a Gmail account, until it is flipped after a live test.
  - A Gmail account's Archive is now its All Mail, so archiving a Gmail message in Mailroom files it there.
- The phone-width accessibility check now accepts a code block that scrolls sideways on purpose, as long as it is marked as intentional, keyboard-focusable and labelled, so the AI-tool setup snippets are checked at 320 pixels too. An in-run control proves an unmarked or unfocusable scroller is still caught.
- Docs, comments and a few screen texts corrected to match what Mailroom actually does. The remaining items had already been fixed by earlier releases; this pass re-checked every one against the code and corrected the last stale comments (a test-count note in the CI workflow, the drill's `--query` help, the ingest re-scan comment, the outbox failure causes).
- A test that checks the encryption-key service never hands out private key material failed now and then for no real reason: one of its search patterns overlapped a public user name that responses are allowed to contain. It now builds its patterns only from the secret part of the key, and a new test forces the old overlap to prove it is gone.

- Reflowing `format=flowed` text now takes linear time even for a hostile message built from hundreds of thousands of soft-wrapped lines (it could freeze the reading pane for nearly a minute; found before release). The web build's ignore rules now also cover a `node_modules` symlink, which had been committed by mistake and is removed.
- **Groundwork for connecting Gmail and Outlook accounts with "Sign in with Google / Microsoft".** Nothing is visible yet: you can't connect such an account until a later step adds the button and the sign-in page. What landed:
  - Mailroom can now sign in to a mail server with a sign-in token instead of a password, for both receiving and sending mail. Accounts that use a password work exactly as before, and tests check that they do.
  - Tokens are renewed automatically before they run out. When several parts of Mailroom need a fresh one at the same moment, only one renewal happens, and two copies of Mailroom renewing at once can't overwrite each other's newer token.
  - When renewal fails, Mailroom works out who has to act: you (the account must be reconnected), the person who runs the server (their setup with Google or Microsoft is broken, so you won't be told to reconnect for nothing), or nobody (a passing network problem, retried later).
  - Tokens never appear in logs or error messages. A test proves this, and it was shown to catch a token that was logged on purpose.
  - Two design records describe the plan: ADR-046 (how sign-in, tokens and their safety work) and ADR-047 (live Gmail will sync All Mail once and keep Gmail's labels as Mailroom labels, rather than downloading every label folder separately).
- One database migration, `00078`: two new tables, for the sign-in state of each connected account and for sign-ins in progress. It only adds; nothing existing changes. No changes to `deploy/` or configuration.
- The accessibility check now also runs at 320 pixels and at 1440 pixels, measures the newsletter and the search box, and is proven able to fail on each of these problems.
- **The sign-in flow for connecting Gmail and Outlook now exists on the server.** The buttons in the web app come next, so nothing changes on screen yet. What landed:
  - New optional settings for the server's own Google and Microsoft sign-in setup: `MAILROOM_OAUTH_GOOGLE_CLIENT_ID` / `_SECRET`, `MAILROOM_OAUTH_MICROSOFT_CLIENT_ID` / `_SECRET`, `MAILROOM_OAUTH_MICROSOFT_TENANT` (default `common`) and `MAILROOM_PUBLIC_URL` (defaults to the address in the sign-in redirect setting). All empty means the feature stays off. `deploy/README.md` explains how to register with each provider, and the server logs the exact return address to register when it starts.
  - Connecting checks, in order, that the sign-in was started by you, for this provider, recently, and only once; that the provider granted mail access; and that the mailbox actually accepts the new sign-in. A sign-in started in one account cannot be finished in another person's.
  - Reconnecting an account must come back as the same Google or Microsoft account, so one mailbox can never be quietly swapped for another. Connecting the same account twice says it is already connected.
  - Disconnecting withdraws the server's access at Google (Microsoft has no way to do this from outside; you remove it in your Microsoft account), stops syncing and keeps the mail. Deleting a connected account withdraws the server's access at Google too, so restoring an old backup can't bring that access back. Deleting or disconnecting an account also drops any sign-in token held in memory.
  - If Google or Microsoft rejects a saved token when syncing or sending, Mailroom fetches a fresh one and tries once more before deciding the account needs reconnecting.
  - The server and sign-in settings of a connected account can't be edited; only its name and preferences can.
  - A connected Gmail account is connected and ready but does not sync yet, and syncing can't be switched on, until the Gmail sync work arrives in the next update. Syncing it today would download every label folder with no daily limit, which can get Gmail to suspend the account's mail access for a day, on your phone too. The account's health shows "Gmail sync arrives in the next update; your account is connected and ready." Outlook accounts sync straight away.
- **Sign-in health for connected accounts.** No database migration; nothing in `deploy/` or the configuration changes. What landed:
  - Accounts carry four new read-only properties: `authKind`, `authState`, `authProvider` and `authAddress`. Each change of sign-in state records an account change in the same transaction, so every client hears about it. Routine token renewal records nothing.
  - `Account/getHealth` gains a per-account `auth` section (state, a short reason code, when it last changed, when a token was last obtained) and, for admins only, `operatorNotices`.
  - Sending on an account waiting to be reconnected (or disconnected) is refused with the new error type `accountNeedsReconnect`. A send that is waiting carries the new EmailSubmission property `mailroomHeld` (`reconnect` or `provider`).
  - The one notification per state change is a new job kind, `oauth_reauth_notify`. It is queued in the same transaction as the state change, so two servers hearing the news at once, or a restart, never queue a second one. During quiet hours it waits instead of being dropped.

## [1.95.1] — 2026-10-27

**Fixes and phone menus.** Pop-up menus now stay on screen on a phone, including Re-file when several messages are selected. An admin tool that could quietly teach Mailroom habits you never had is fixed. There is also a second layer of protection against other websites acting as you.

### Menus that stay on screen on a phone

- **Re-file works with several messages selected.** On a phone, selecting messages puts the action bar at the bottom of the screen, and the Re-file menu used to open *downward*, entirely off the screen. It now opens upward when there is no room below. A long list of areas scrolls inside the menu instead of running off the edge.
- **The same fix covers every pop-up menu.** Move to area, the reading pane's More menu, a sender's "Move all", search suggestions and recipient suggestions all flip, shrink or shift to stay inside the screen. That holds on small phones and large ones, upright or sideways, and they never hide under the bottom bar.
- **Dialogs fit the screen as it is right now,** so one no longer ends up underneath the browser's address bar when that bar is showing.

### Duplicate tidy-ups no longer fake your filings

- The `collapse-duplicates` admin command folds duplicate copies of a message into one. It used to make every label on a removed copy reappear on the kept copy as if **you** had applied it by hand, even when a rule, learned placement, the desk or an AI tool had put it there. Mailroom learns how you sort from your own filings, so this could teach it habits you never had.
- Labels now keep who applied them. When both copies carry the same label, your own filing wins.
- **Check what earlier tidy-ups did:** `mailroom-admin collapse-provenance --user-id <id>` reports, without changing anything, which labels earlier runs mislabelled. It is honest about its limits:
  - It can identify labels that a rule applied, because they still name the rule.
  - A label from learned placement, the desk or an AI tool that was mislabelled this way can't be told apart from a real filing. The report says how many labels fall in that blind spot.
  - Run it again with `--apply` to put the rule labels back under their rule. Running it a second time changes nothing.

### Safety

- **A second lock against other websites acting as you.** Mailroom already kept your sign-in cookie from travelling with requests started by other sites. It now also refuses any change request that your browser says came from a different site, and any malformed request to the mail endpoint. You'll notice nothing in normal use. Connected AI tools and other API-key clients aren't affected.

### Under the hood

- A new automated check opens every pop-up menu on a very small phone and a larger one, upright and sideways, and fails if any part of a menu is off screen, hidden under a bar, or can't be scrolled. It is proven able to fail: it went red on the old menus.
- The accessibility check's "forward a message" scenario no longer leaves a draft behind that could change later screens, so that check gives the same answer every run.
- The database tests now fail within 30 seconds when one step is stuck waiting on another, instead of hanging for ten minutes, and they report what was holding things up. The whole run has an explicit 20-minute limit.
- No database migrations, and no changes to `deploy/` or configuration.

## [1.95.0] — 2026-10-26

**AI-tool access.** You can now let an AI tool, such as a desktop chat app or a model running on your own computer, work with your mail: search it, read it, sort it and write drafts. You decide exactly what each tool may do, anything risky waits for your approval, and nothing a tool does is permanent.

### Connect an AI tool to your mail

- Mailroom now has a standard **MCP endpoint** at `/mcp` on your Mailroom address (for example `https://mail.example.com/mcp`). Any AI tool that speaks MCP can connect; nothing is tied to one vendor.
- Each tool gets its own **API key**. Create one in **Settings → API keys and AI tools**, and the page shows the endpoint and ready-to-paste setup snippets for common kinds of client.
- A key is shown **once**, when you create it. Copy it then; Mailroom keeps only a fingerprint of it.

### Choose exactly what each tool may do

- **Presets** cover the common cases:
  - **Read only:** search and read mail, labels and contacts.
  - **Organise:** also archive, move, label and mark read or unread.
  - **Assistant:** also write drafts.
- **Sending** and **deleting** are separate, deliberate choices. A key with both needs you to type a confirmation.
- A key can be limited to **some of your mail accounts**.
- Every key has a **rate limit** (60 calls a minute by default) and a **daily budget** (500 calls by default).
- Each key's **activity list** shows what it did and when: every call, including refused ones. It records ids and counts, never the content of your mail.
- You can **revoke** a key at any time.

### "Ask me first" for sending and deleting

- When a tool wants to send an email or delete something, it can be made to **wait for you**. This is on by default.
- A **banner** appears across Mailroom, and you get a push notification. Each request shows who the email goes to, its subject and its attachments, or what would be deleted.
- **Approve** and it happens; **Deny** and the tool is told no. A request you don't answer is denied after **24 hours**.
- If a tool changes or discards a draft after asking, the request can't be approved, and you're told why.

### What a tool can do

- **Find and read:** search your mail, read a message (as text, never its attachments), list folders, labels, desk areas and contacts.
- **Organise:** archive, move to a folder or desk area, add or remove labels, mark read or unread.
- **Write:** save a new draft or a reply, edit or discard a draft it wrote itself, and, if allowed, send a draft it wrote.
- **Delete:** move mail to Trash, if allowed.
- **Check on approvals:** see whether its own requests were approved.

### Drafts written by AI tools

- **Formatting comes through.** Tools write drafts in Markdown, the style most AI models write in naturally, and Mailroom turns that into a properly formatted email: headings, bold, lists, tables and links. A plain-text copy goes with it.
- **No duplicates.** If a tool saves the same draft again within 10 minutes, Mailroom keeps the first and tells the tool it's already saved.
- **Edit instead of starting over.** A tool can update or discard a draft it wrote. It can never touch your drafts or another tool's.
- **A tool that can't send says so.** A key without permission to send is told plainly that it can't send, and that you send the draft from Drafts. Tools no longer try again and again.

### Built-in safety

- **Nothing is permanently deleted.** Deletes go to Trash, where mail stays restorable for 30 days. No tool can empty it.
- **A tool only sends what it wrote.** It can't send your drafts, or mail it merely read.
- **Runaway tools are stopped.** More than 50 deletes or 10 sends in 10 minutes are refused, you're alerted, and the key is **suspended** until you turn it back on in Settings. The same happens if a key keeps trying things it isn't allowed to do.
- **Your habits stay yours.** What a tool does never teaches Mailroom's learned sorting or what it marks "important".
- **Your own filing wins.** A tool can't move a message out of an area you filed it into yourself.
- **Mail is treated as untrusted.** Everything taken from a message reaches the tool clearly marked as data, not instructions, with hidden and direction-changing characters removed. This is a defence against emails written to trick an AI tool. It doesn't make a tool immune, which is why the limits above exist.

### Also fixed

- **Plain-text emails keep their line breaks.** Messages that are plain text only (no HTML version) showed as one run-on paragraph in the reading pane. They now show as written.
- **Copy buttons work on a plain-HTTP address.** The Copy buttons in Settings now work when Mailroom is reached over `http://` on a home network.
- The setup snippet for local tools adds the `--allow-http` option on a plain-HTTP address, which the bridge needs.

### Under the hood

- Database migrations 00074–00077 add the key, activity, approval and draft tables. All are additive, and they run automatically on start.
- The server and web images move to the latest rebuilds of the same base versions (Go 1.27, Node 24, Alpine 3.24), and the bundled ClamAV moves to its latest 1.5.4 rebuild.
- The web test runner moves to vitest 5 (development only).
- Design records: ADR-042 (keys, limits and approvals), ADR-044 (the MCP endpoint) and ADR-045 (AI-tool drafts and Markdown). The full tool reference is in `docs/mcp.md`.

### Upgrading

- **Self-hosted with Docker Compose:** pull the new image and copy the updated `deploy/compose.yml` (the ClamAV image changed). Migrations run on start; nothing else to do.
- **Behind a reverse proxy:** make sure `/mcp` is passed through to Mailroom like the rest of the site.

Tested across three release candidates, with read-only, assistant and send-with-approval keys from a real AI client.

## [1.95.0-rc.3] — 2026-10-26

The third test pre-release of AI-tool access, with the changes from the
rc.2 test: plain-text messages keep their line breaks in the reading pane (a
display bug that affected all plain-text-only mail, not only AI-tool drafts); a
key's draft bodies are Markdown, sent as a formatted email with the text
alongside; a repeated identical draft is not saved twice; keys can replace or
discard the drafts they wrote; and a key that cannot send is told so in plain
words. Migration 00077 (expand-only). Not for production.

### Added

- **`draft_update` and `draft_discard` AI tools.** A key with
  `mail:draft` can replace a draft it wrote (new content, a new id; a reply
  keeps its threading) or discard one, never the owner's or another key's
  drafts, and not one that was sent or whose send waits for approval. The old
  version goes to Trash, like every agent delete; a replaced or discarded
  version cannot be sent while it is in Trash (moved back into Drafts, it is
  the key's draft again), and approving a send of it is refused with that
  reason. Migration 00077 (expand-only) adds
  `app.api_key_drafts.content_fingerprint`.
- **Markdown draft bodies.** The draft tools' body is Markdown: the
  message carries it exactly as written as plain text plus an HTML rendering
  (goldmark; single line breaks kept, raw HTML shown as text, links only to
  http, https and mailto, images left out). ADR-045.

### Changed

- **A key's repeated draft is not saved twice.** A draft identical to one
  the same key saved in the last 10 minutes, and still unsent in Drafts, writes
  nothing: the tool answers `already_saved` with the first draft's id. Matched
  on a keyed fingerprint, so a database copy alone reveals nothing of a draft.
- **A key that cannot send is told so.** Without `mail:send`, the draft
  tools say the draft is in Drafts and the account owner sends it from
  Mailroom, and never mention `send_draft`; a hidden tool called anyway answers
  with the permission the key lacks ("send_draft is not available: this key
  does not have permission to send email (mail:send)").

### Fixed

- Plain-text messages keep their line breaks. The server lists a lone
  text/plain part in both `textBody` and `htmlBody`, and the reading pane took
  that for HTML and collapsed every break. HTML is now used only when a text/html
  part exists (a plain part inside a mixed message is escaped and keeps its
  breaks), and a resumed plain draft opens in plain mode instead of rich.

## [1.95.0-rc.2] — 2026-10-26

The second test pre-release of AI-tool access, with the changes from the
rc.1 test: an AI tool's actions no longer teach "important" (as they already
did not train learned placement), the setup guide's Copy buttons work on an
instance served over plain HTTP, and the bridge config adds `--allow-http`
there. No new migrations. Not for production.

### Changed

- **An AI tool's actions no longer teach "important".** What an API key
  archives, trashes, flags or replies to still happens, but it is never recorded
  as feedback for the priority model: the Email/set and send emitters skip a
  key's requests, and a reply written or sent by a key (including its Sent copy,
  recognised by Message-ID) records no reply signal through the submission,
  ingest or backfill paths. `priority-report` counts such replies on their own
  line ("sent by an AI tool key, not counted") instead of leaving them
  "Unrecorded", and the reply backfill skips them.

### Fixed

- **Copy buttons work on plain-HTTP instances.** Browsers expose the
  Clipboard API only on secure origins, so the setup guide's Copy buttons always
  said "Couldn't copy" on an instance served over plain HTTP. They now fall back
  to selecting a hidden field inside their own wrapper and copying that (also
  when the Clipboard API refuses), and report "Copied." only when the browser
  says it copied.
- AI-tool setup: on an instance served over plain HTTP on a private network, the bridge config now includes `--allow-http` (which `mcp-remote` requires) and a warning that the key is unencrypted.

## [1.95.0-rc.1] — 2026-10-24

The first test pre-release of AI-tool access: per-user API keys with
scopes, an optional account restriction, rate and budget limits, an audit log,
"ask me first" approvals for sending and deleting, and automatic suspension of
a key that misbehaves; a standard MCP endpoint at `/mcp` with tools to search,
read, label, archive, file into areas, draft, send and delete (deletes are
always to Trash); and Settings → "API keys and AI tools" with the keys, a
pending-approvals banner and list, and setup for any MCP client. An AI tool's
actions never train learned placement. Migrations 00074–00076. Not for
production.

### Added

- **The server states a new API key's auto-suspend default.** The
  `urn:mailroom:apikeys` session capability carries
  `defaultSuspendOnAnomaly: true`, from the same constant a create uses.

- **A held API key request names its account.** `PendingAction/get`
  objects carry `accountId`, the JMAP account the held send or delete acts
  on, so a client can look up the held messages there.

- **API keys for agents and other clients (server, ADR-042).** A key is
  created in a browser session (`ApiKey/set`, capability
  `urn:mailroom:apikeys`), shown once, and used with a standard
  `Authorization: Bearer` header on the JMAP endpoint by any client. Only a
  hash is stored. Keys have scopes from spec §13a's closed set, an optional
  restriction to some accounts, a mandatory rate per minute and 24-hour budget,
  and can be suspended or revoked. A key that can both send and delete needs an
  explicit acknowledgement. Every JMAP method has a line in one scope table and
  anything without one is refused to keys; key management, private-key
  operations, identities and accounts are never available to a key. A key's
  delete is always a move to Trash. Every call is audited (`ApiKey/getEvents`)
  with ids and counts, never content. A key that tries to delete more than 50
  messages in 10 minutes is stopped and suspended, more than 10 sends in 10
  minutes are refused, and activity in your off hours is recorded; all three
  are pushed to your devices. Sends and deletes start in "ask me first" mode:
  the call is held as a pending action (`PendingAction/get`, `/set`) that you
  approve or deny, re-checked when you approve, and denied after 24 hours. A
  key may send only drafts it wrote, cannot change or cancel a send, and pays
  for reads by volume. The MCP tools and the Settings page come next.
- **MCP tools for agents (ADR-044, `docs/mcp.md`).** Any MCP-capable
  client, with any model, can connect to `/mcp` (the standard Streamable HTTP
  transport) with an API key in `Authorization: Bearer`. The tools:
  `search_mail`, `read_message`, `list_mailboxes`, `list_labels`,
  `apply_label`, `remove_label`, `mark_read`, `mark_unread`, `archive`,
  `move_to_mailbox`, `draft_new`, `draft_reply`, `send_draft`,
  `delete_message`, `find_contacts`, `get_contact` and `get_pending`. A key
  sees only the tools its scopes allow, and every tool call goes through the
  same checks as the key's JMAP calls: limits, approvals, the audit, soft
  deletes. A send or delete in "ask me first" mode returns
  `pending_approval` and waits for you. Text taken from mail is returned only
  in fields marked as untrusted content, with invisible and
  direction-changing characters removed. The endpoint is stateless (no
  session to keep) and accepts keys only: a browser session is refused.
  Clients that only run local servers connect through a generic bridge
  such as `mcp-remote`.
- **Areas for API keys, without teaching learned sorting (ADR-042 §3a).**
  A key with `mail:read` can list your desk's areas (`Area/get`), and with
  `mail:move` file mail into one; the MCP tools gain `list_areas`,
  `move_to_area` and an area filter on `search_mail`. Creating, renaming and
  deleting areas stays yours. A key's moves are recorded as the key's, never
  as yours: they are not filings learned placement learns from, taking a
  learned placement's label off is not a correction (the placement is
  "withdrawn", counted as neither kept nor corrected, and `placement-report`
  shows how many), archiving one does not keep it, and taking a rule's label
  off is not a correction of the rule. A key cannot undo a filing you made by
  hand. `move_to_area` reports a message another of its labels keeps in a
  different area instead of counting it as moved.
- **API keys, approvals and AI-tool setup in Settings.** A new
  Settings row, *API keys and AI tools*, lists your keys (what each can do,
  with the Read only, Organise and Assistant presets named when they match,
  which accounts, whether it is active, suspended and why, or revoked, when it
  was last used and its use against the 24-hour budget) and creates them: a
  preset or your own choice of permissions from the server's list, "ask me
  first" on by default for sending and deleting, an optional account
  restriction, the limits and automatic suspension. A key that can both send
  and delete needs a typed confirmation. The new key is shown once, with a copy
  button and the connection settings filled in, and the dialog closes only when
  you say you have saved it; the key is not kept anywhere in the browser. Each
  key has a page to change its name, limits and "ask me first", suspend or
  reactivate it (reactivating says what tripped it), revoke it, and read its
  activity in plain words, including counted "+N more refused calls" and
  highlighted unusual activity. Requests waiting for your approval are listed
  first, built from the request's own fields (recipients first, To, Cc and Bcc
  apart, anyone the message would reach without being named in it, the
  agent-written subject set apart as quoted text, attachments, what else it
  archives, the messages a delete would move, and when it expires); Approve
  states exactly what will happen. A banner over the mail views says how many
  are waiting and links there; it checks every minute while the tab is visible
  and when you come back to it. *Connect an AI tool* gives the endpoint for
  this server, the header, the generic stdio bridge and two worked examples,
  kept identical to `docs/mcp.md` by a test. Tapping an API-key push
  notification now opens this row instead of a missing page. A message an API key filed into an
  area says so on its area chip ("filed by an AI tool (API key)") rather
  than "automatically".

### Changed

- **A misbehaving API key is suspended, for every anomaly rule.** Too
  many sends, too many deletes, or a burst of refused attempts that matter for
  security (outside its accounts, at approvals or at actions only you can
  take) now suspend the key by default, not only a mass delete. The alert says
  the key was suspended, the key's next call is refused, every suspension and
  every reactivation (in Settings, by you) is in the audit log, and a key that
  trips again after you reactivate it is reported again at once. A key's
  "suspend on anomaly" switch still turns this into alerts only. Activity in
  your off hours is still only recorded and pushed. The switch is now on for
  every existing key; none was in real use yet.

- **An API key's approved request is not charged a second time.** A
  held send or delete is paid for when it is held. Running it after you
  approve no longer counts again, so approving a large delete no longer fails
  with "rate spent".

- **API key reads of `/changes` are charged by the ids they return**,
  like other reads, instead of as a single object.

- **Deleting a draft erases it for good only when it is in a Drafts folder.** A draft is erased permanently, including from the mail server,
  only when it actually sits in a Drafts-role folder. Otherwise, including
  the older copy that saving a draft replaces, it goes to Trash and can be
  restored.

- **An API key can have at most 10 requests waiting for your approval.** The next one is refused (and recorded with the reason
  `pending_cap`) until one is approved, denied or expires. An approval you
  give is no longer failed because the key used up its per-minute rate since
  it asked; the daily budget still applies. A request that could not be held
  at all (nothing valid to delete, a send that would fail) is recorded as
  refused for its arguments, like other refusals.

- **Every query an API key makes is paged, and so is its change list.** `Mailbox/query`, `Label/query`, `Contact/query` and
  `EmailSubmission/query` now work for a key as `Email/query` already did: at
  most 50 ids when the call sets no `limit`, at most 500 when it asks for
  more, with the limit used returned in the answer's `limit` whenever it
  differs from the request; `Mailbox/query` and `Label/query` honour
  `position` and `limit` for keys. A key's `/changes` returns at most 50
  changes when it sends no `maxChanges` and never more than 500, with
  `hasMoreChanges` and an intermediate `newState` for the rest. Both are
  charged for that page before they run. Browser sessions are unchanged.

### Fixed

- **An API key can no longer hide your corrections from learned placement.** If a key took a learned area label off a message and then put the
  same label back, your own later removal of that label was not recorded as a
  correction, so the sender kept being placed there. It now counts exactly as
  removing the learned label does.

- **API key notifications open the right page.** Tapping an "Approval
  needed" notification opened a not-found page; it now opens Settings, API
  keys, where the request waits, and a notification about one key (stopped,
  suspended, active in your off hours) opens that key.

- **`placement-report` no longer fails on a filed message with no From.** A message without a From address that you had filed into an area
  made the report's evidence read fail; it is now left out of the evidence,
  as a new message with no From already was.

- **No message can be turned into a draft.** `keywords/$draft: true` on a
  received or sent message is refused for every client, and deleting a
  draft-flagged message outside a Drafts folder moves it to Trash instead of
  erasing it.

### Security

- **MCP tools drop text a person cannot see.** When a
  message's HTML hides text (the `hidden` attribute, `aria-hidden`, or inline
  `display:none`, `visibility:hidden`, `opacity:0`, `font-size:0`,
  `color:transparent`, or zero height/width with overflow hidden), the MCP
  `read_message` tool now leaves it out, so instructions hidden from you are
  not read to an agent. All text returned to an agent, and contact names
  learned from mail, also lose invisible fillers such as U+3164 and variation
  selectors. Hiding by stylesheet, matching colours or off-screen placement
  is not detected.
- **An IMAP system flag can no longer be set as a keyword.** `Email/set`
  accepted `keywords/\Draft`, wrote it to the mail server as the IMAP draft
  flag, and the next sync then treated the message as a draft, so an ordinary
  delete afterwards erased it everywhere instead of moving it to Trash. This
  was open to the web app's own session as well as to API keys. Keywords are
  now checked against the RFC 8621 syntax and anything else is refused as
  invalid, and the store and the writeback engine both refuse a keyword
  containing a backslash.
  System keywords are now also matched in any casing, as RFC 8621 requires:
  `$JUNK`, `junk` or `$Draft` used to become plain keywords that the next
  sync read back as junk or draft state, getting around the rules for those
  two; they now reach the same checks as `$junk` and `$draft`.

- **An API key restricted to some accounts can no longer archive mail in
  others by sending.** "Send and archive" (`archiveOnSuccess`) was not
  checked against the key's accounts, so such a key could archive up to 100
  messages elsewhere when it sent something. The whole send is now refused,
  both when it is made and again when a held send is approved, and the
  approval request lists the messages it would archive.

- **An API key cannot block, unblock or VIP a sender.** Blocking a
  sender to Trash deletes all their future mail. Through `Contact/set` a key
  with `contacts:write` could do that without the delete permission or an
  approval, could undo your own blocks, and could let a sender through quiet
  hours. Keys can still save contacts, name them and add notes.

- **An approval request cannot be made to misdescribe its send.** The
  summary repeated the key's own subject unescaped, so a crafted subject could
  invent recipients or attachments in the text you read. `PendingAction/get`
  now returns the recipients (To, Cc and Bcc), the subject and the send time
  as separate fields. The summary lists recipients first, and quote marks in
  text written by the key are neutralised.

- **A busy API key no longer rate-limits your own browser.** Keys
  shared your HTTP request allowance, so a looping agent could lock you out
  with "too many requests", including the request to revoke it. Each key now
  has its own allowance, all of your keys together share one more, so several
  keys get no more than one would, and you can have at most 20 active keys.

- **Refused API key calls now count against the key's per-minute rate.**
  A key that keeps making calls it is not allowed to make slows itself down.
  Beyond one minute's rate of refusals, routine refusals are counted per
  minute in the key's activity (`suppressedRefusals`) rather than listed one
  by one. Refusals that matter for security (attempts outside the key's
  accounts, calls to session-only methods, misuse of approvals, the anomaly
  rules) are listed in full up to five times that rate. A key that goes past
  that sends you an alert, at most once an hour.

- Web dependencies: `undici` 8.12.3 (via jsdom; three advisories against
  8.0.0–8.12.1, one high) and `vitest` 4.1.12 (an `@vitest/mocker` advisory).
  Both are test-time only; neither ships in the built client.

- **An API key looping past the send or delete limit no longer floods your
  devices.** Every such call is still refused and written in the
  key's activity, but the anomaly is recorded and pushed (urgently, through
  quiet hours) once per key, rule and hour. A key suspended for a mass delete
  is still reported every time it trips, since it can only trip again after
  you reactivate it.

- **An approval request's summary is harder to make misleading.**
  The line now reads recipients, then the number and size of attachments and
  the messages it would archive, then the subject last. Every Unicode
  quotation mark in the subject is neutralised as a class, along with
  look-alikes such as ❝ ❞ ‶ ⹂ and the ornamental quotes, which the previous
  list missed. Attachment names and archived subjects are no longer in the
  line. The recipients are chosen by the key by design, so the line is still
  prose for a notification: `PendingAction/get`'s structured fields
  (recipients, subject, attachments, archive) are what an approval screen
  shows.

- **Envelope addresses must be plain ASCII email addresses.** An
  `EmailSubmission/set` envelope `rcptTo` or `mailFrom` must be a bare
  `local@domain`: letters, digits and the usual address symbols in the
  local part (no quotes, no doubled dots), ordinary domain labels (punycode
  `xn--` names are fine), no IP literal, at most 254 characters. Anything
  else, including display names, comments, Unicode spaces and non-ASCII
  characters, is refused as `invalidProperties`, for every client. Accepted
  addresses are stored lowercased. Such text used to reach the send path as
  given, and an API key's envelope recipients lead the approval summary,
  where it could pose as part of the line. The web app does not send an
  envelope, and non-ASCII addresses were already refused when sending.

- **An API key's `/changes` answer is bounded.** It used to return
  every change since the state it named, however many, and was charged only
  after it was built. It is now a page of at most 500, charged up front (see
  Changed).

### Database

- Migration 00074: new columns on `app.api_keys` (confirm scopes, token hint,
  suspension), `app.api_key_events`, `app.api_key_pending` and `app.api_key_drafts` (all
  FORCE RLS),
  and two narrow definers for bearer lookup and the approval-expiry sweep.

- Migration 00075: the refusal-reason CHECKs on `app.api_key_events` and
  `app.api_key_suppressed_refusals` accept `pending_cap`. Expand-only.

- Migration 00076: `app.learned_placements` gains the outcome `withdrawn` and
  `withdrawn_at`; `app.submissions` gains `api_key_id` (the key that made a
  send); `suspend_on_anomaly` is turned on for every existing API key.
  Expand-only.

## [1.94.2] — 2026-10-23

Mail is marked important again, and learned placement now files mail from
senders who send from a subdomain. After deploy, recent mail is re-scored
(report first) and the priority model is held to a gate it must pass to
replace the built-in heuristic. No migration.

### Added

- **Engagement signals the model can learn from**. The web app
  reports a message as read once it has stayed in the reading pane for four
  seconds (once per message; a quick flick past it does not count), through
  the new session-only `Triage/open` method. Replies are recognised from any
  mail client, by matching your sent mail to the message it answers, once
  per original.
- **`mailroom-admin priority-report`, `priority-reply-backfill`, `priority-train`,
  `priority-rescore`**. The report is read-only: how engaged mail ranks
  under the heuristic and the trained model at the 0.6 line, counts per
  signal source (including replies that were never recorded), what each
  stored model version was trained on, and the verdict the gate would reach
  today. The backfill records past replies, `priority-train` runs the daily
  pass now (so the gate applies before a re-score), and the rescore re-scores recent
  mail with the model in service. All three are report-first, with `--apply`
  opt-in.

### Fixed

- **Mail is marked important again: the priority model is promoted only when
  it beats the built-in heuristic** (ADR-043). Since the first trained
  model went live, nothing scored at or above the 0.6 line. The model had
  learned from about 6,900 "did not matter" signals and 13 "mattered" ones,
  and a promotion check that could not fail on that data had let it replace
  the heuristic. A trained model now has to catch more of the mail you
  engage with than the heuristic does, on senders it never trained on, from
  at least 100 examples, without losing much precision. Otherwise the
  heuristic serves, and a trained model already in service is switched back.
  Every decision is logged with its numbers. Reading a message is no longer
  recorded as "did not matter".
- **Learned placement now files mail from senders on a subdomain** (ADR-032 amendment). Many senders write from a subdomain such as
  `news.example.com`, and their provider reports the DMARC pass for
  `example.com`. Learned placement refused all of them as unauthenticated.
  A trusted pass now counts when it names the sender's own domain or exactly
  its organisational domain under the Public Suffix List. It never counts for
  a public suffix such as `co.uk`, a sibling subdomain, any other parent, or
  a parent covered by a subdomain's pass, and every other check is unchanged.
  `placement-explain` says whether the match was exact or an org-domain
  match. Calendar "Verified" and Autocrypt key trust still need an exact
  match. `placement-backfill` may now find existing mail to move. It still
  reports first and moves nothing without `--apply`.

## [1.94.1] — 2026-10-23

Diagnostics and a clean-up: see why learned placement held a message back, and
strip hidden characters from contact names learned from mail. No migration.

### Added

- **Why learned placement held a message back can now be seen**. Every
  arrival decision is counted in `mailroom_learned_placement_total{reason}`
  (placed, or thin, mixed, unauthenticated, ambiguous, warned, gated, off,
  error and the rest; a closed set), where before only a gated area was ever
  logged. `mailroom-admin placement-explain --user-id U (--message-id M |
  --sender ADDRESS [--limit N])` re-runs the decision read-only, with the very
  code ingest runs, and prints the sender parse, the keys, the evidence per
  key, the decision, every sender check with its inputs (the trusted
  Authentication-Results clauses and their `header.from` against the key
  domain, warnings, the tab verdict) and the first check that refused.
  Evidence is as of now unless `--as-of` (only filings made before the message
  arrived). Nothing about what is placed changes.

### Fixed

- **Contact names from incoming mail are stored without hidden characters**. A sender's name could carry direction-changing, control or
  zero-width characters that reordered or hid text for any client reading it
  from the API. Names are now cleaned when learned (a name that is nothing but
  such characters is stored empty, so the address shows), and
  `mailroom-admin contact-names-clean` repairs older ones (report first, then
  `--apply`).

## [1.94.0] — 2026-10-22

Contacts: a Contacts entry in the sidebar and a page per person. Only the
people you save are listed:
everyone you send mail to is saved automatically, anyone else with "Add to
contacts" beside their address, and marking someone VIP saves them. A contact's
page has your own name for them (kept when their mail says otherwise), notes,
VIP and block, their key with its history, your recent mail with them and how
often each of you writes. A person's addresses can be merged by hand and
separated again, and separating gives back exactly what the merge borrowed.
VIPs and blocked senders moved from Settings to Contacts filters. Contact
dates now come from the mail itself instead of from when it was imported, and
`contact-dates-backfill` repairs older rows once (report first, then
`--apply`). Contact names from incoming mail are shown with direction-changing
characters removed and a name that looks like an address is shown beside the
real one. Tested across two release candidates.
Migrations 00072 (saved contacts, names, notes, merge tombstones) and 00073
(merge records), both expand-only.

## [1.94.0-rc.2] — 2026-10-22

The second test pre-release of the contacts page: separating merged contacts
now gives back what the merge borrowed. Not for production.

### Fixed

- **Separating merged contacts gives back what the merge borrowed**.
  After merging one contact into another and separating them again, the
  original kept the other's name (when its own mail carried none), the joined
  notes, VIP, saved and block. Each merge now records the original's state
  before the merge (migration 00073), and a separate that takes back all of the
  merged-in contact's addresses puts each of those back, unless you changed it
  after the merge: then your change is kept, and the page says which. The block
  goes back with the address that had it, so the separated contact stays
  blocked and the original returns to unblocked; a block you set after merging
  stays on both. Separating only some of the merged-in addresses changes
  nothing on the original, and says so. Merges of merges undo from each
  merge's own record. A merge made on 1.94.0-rc.1 has no record, so separating
  it still restores nothing on the original (ADR-041 §4, amended).

## [1.94.0-rc.1] — 2026-10-22

The first test pre-release of the contacts page: a Contacts entry in the
sidebar listing only the people you save (everyone you write to is saved
automatically; anyone else via "Add to contacts"), a page per contact with your
own name for them, notes, VIP and block, their key and its history, and recent
mail; merging a person's addresses by hand, and separating them again. Contact
dates now come from the mail itself. Not for production.

The contacts page (ADR-041), server and web. Migration 00072.

### Added

- **Contacts, in the rail.** A list of the people you correspond with: the
  contacts you have written to, marked VIP or added by hand, not every address
  in your mail. Search by name or address; show All saved, VIP, Has a key or
  Blocked senders (blocked senders are listed whether or not you saved them);
  sort by name or by last mail. A long list says how much of it is showing
  ("Showing 50 of 53") and loads more on request. "Add a contact by address…"
  adds anyone you have mail with. Going back from a contact returns to the
  same list, filter and place.
- **A page for each contact.** Their name (Edit name sets your own name for
  them; with one set, the name their mail gives is shown beside it with "Use
  theirs"), every address and the address of yours it writes to, how often you
  have written to each other and when, VIP, Block, notes (up to 4,000
  characters), their key with Check for an updated key, Don't use this key,
  Remove and its history, and their ten latest messages with "See all mail
  with …". Remove from contacts deletes nothing; a VIP you remove stays a VIP.
- **Merge with… and Separate.** Merge two contacts that are one person; the
  dialog says what will be combined, including that a block on either one
  carries over. Separate splits addresses back out.
- **Ways in:** the sender card's name links to the contact (or "Add to
  contacts"); a recipient chip's name in the composer opens "Open contact" or
  "Add to contacts"; each key in Settings → Keys links to its contact.
- Names and addresses on the Contacts page come from other people's mail and
  are shown with direction-changing characters removed; a name that looks
  like an address is shown with the real address beside it.
- **Saved contacts.** A contact is saved when you send mail to it, when you
  mark it VIP, or by hand (`Contact/set isSaved`). Blocking does not save.
  Removing a contact from your contacts deletes nothing: its keys, key history,
  VIP and block stay. Only saved contacts are meant for the Contacts page;
  autocomplete still covers everyone.
- **Your own name for a contact, and notes.** `Contact/set userName` (at most
  200 characters, cleaned of control and direction-changing characters; mail
  never overwrites it) and `notes` (at most 4,000 characters).
- **Contact properties:** `isSaved`, `savedAt`, `name` (your name for them, else
  the one from mail), `userName`, `notes`, `inboundCount`, `outboundCount`,
  `firstMailAt`, `lastMailAt`, and an `id` on each address.
- **`Contact/query`:** filters `isSaved` and `onlyCoRecipient` (addresses with
  no mail from them and never written to), sort by `name` or `lastMailAt`, and
  paging by `position`. With none of these it answers exactly as before.
- **`Contact/merge` and `Contact/separate`:** merge two contacts that are one
  person (addresses and keys move; a block wins, with the earlier date and the
  stricter destination; notes are joined), and separate addresses back out
  into a contact restored from the one they came from.
- **`mailroom-admin contact-dates-backfill --user-id U [--apply] [--list]`:**
  sets contacts' first/last mail dates to the real message dates that
  `contact-dates-report` measures, and saves the contacts you have written to.
  Report-only unless `--apply`; a rerun changes nothing.

### Changed

- **Settings › Senders** now links to the Contacts page's VIP and Blocked
  senders filters instead of listing them; marking an address moved to
  Contacts as "Add a contact by address…".

### Fixed

- **A contact's first and last mail dates now come from the message, not from
  when it was imported.** Importing an old archive no longer makes every
  correspondent look like they wrote today, and mail that arrives out of order
  no longer moves the dates the wrong way. Contacts written before this fix are
  repaired once with `mailroom-admin contact-dates-backfill` (dry run, then
  `--apply`); `contact-dates-report` shows how far off they are.

## [1.93.1] — 2026-10-22

A report-only patch: measures how far contacts' stored dates are from the
real mail dates, ahead of the contacts page. No behaviour change, no
migration.

### Added

- `mailroom-admin contact-dates-report --user-id U [--sample N]`: a read-only
  report of how far each contact address's stored first/last-seen dates (which
  record when mail was ingested) are from the real first/last mail dates (from
  message received times, both mail from the address and mail you sent to it),
  with per-signal buckets, the addresses with no mail found, and the age of the
  latest mail. Measurement ahead of the contacts page's first/last mail dates.

## [1.93.0] — 2026-10-21

PGP keys: a key-management page in Settings → Keys, keys that expire, and a
lasting key history. You can make a key (it expires after two years by default,
or on a date you choose), import one, choose which address it signs for, publish
it to your domain's key directory (WKD), extend it, back it up as a
passphrase-protected file, replace it in one step, retire it (it still opens old
mail) or revoke it. Keys show the names and addresses they carry, with a note
when a key does not list the address it is used for. Correspondents' keys are
listed with their source and expiry, can be checked for a renewed version in
their key directory, set aside ("Don't use this key") or removed; a removed key
is kept in its history rather than deleted. When a correspondent renews a key,
Mailroom now merges the renewal instead of keeping the expiring copy forever,
and a replayed old copy can never undo a revocation (ADR-040). Expiry warnings
appear on the desk 30 days ahead, in compose when a recipient's key is expiring
or has lapsed, and on signatures made with a key that has since expired, which
no longer read like forgeries. New admin verbs `pgp-retire`, `pgp-extend` and
`pgp-revoke`. Tested across two release candidates (details
below), including a backup restored into Kleopatra. Migrations 00070 (WKD serves
revocations) and 00071 (key history), both expand-only.

## [1.93.0-rc.2] — 2026-10-21

The second test pre-release of PGP keys: everything in rc.1, plus a lasting key
history, an exact expiry date, the names on keys, "Replace this key" and "Check
for an updated key". Not for production.

### Added

- **Key history.** Removing a correspondent's key no longer deletes it:
  it is kept, out of use, with a record of what happened to it (first seen,
  renewed, trusted, refused, removed). Nothing encrypts to, verifies with or
  shows a removed key as current; if the same key turns up again it comes back
  with its history. Your own keys now record when they were retired and which
  key replaced them. Keys retired before this release show that date as not
  recorded. Server side: `ContactKey/history`, `ContactKey/get`
  `includeRemoved`, and `retiredAt` / `replacedBy` on your keys.
- **Choose an exact expiry date for a key.** Generating or extending a
  key accepts a date (it then expires at the end of that day, UTC) or an exact
  time instead of a number of days, from one day to ten years out. The key's
  own packets carry exactly that moment, for the main key and the encryption
  subkey alike.
- **Keys show their names and addresses.** Your keys and your
  correspondents' keys now list the user IDs they carry (name, address,
  revoked, primary), so keys that share an address can be told apart. Only user
  IDs the key's owner actually signed are listed, and their text is cleaned of
  control and direction-changing characters. Binding a key to an address it
  does not name still works, with a warning that correspondents' apps may
  refuse or flag it.
- **Replace a key in one step.** "Replace" makes a new key for the same
  address, puts it in place (signing, and in WKD if the old key was published
  there) and retires the old key, all at once: if anything fails, the old key
  stays exactly as it was. The old key still opens past mail. A revoked key
  cannot be replaced; generate a new one instead.
- **Check for an updated correspondent key.** A correspondent's key can
  be re-fetched from their WKD on request. A renewal is merged in; a revocation
  is picked up; a *different* key is never used silently but goes to the usual
  "key changed" decision. The result says which happened, or that nothing was
  found or the server could not be reached. Checks are rate-limited per user,
  per domain and per key.
- **Settings → Keys shows all of the above.**
  - Each key is titled by its main name and address ("Name <address>"), with
    its other names and addresses beneath and revoked ones marked. A key
    that does not name the address it is used for says so, and so does
    choosing such an address for it.
  - Making, extending and replacing a key offer "On a date…" with a date
    picker. The page says the result in words ("Expires on 4 Nov 2026, in 14
    days"), that the key stops at the end of that day (UTC), and when the date
    is close enough that the key will show "Expires soon" straight away.
  - "Replace…" on a key in use says what happens before it does: a new key
    for the address, published if the old one was; the old key stops signing
    but still opens past mail. If something goes wrong after the new key was
    made, the page says so and lists it, unused.
  - A collapsed **History** section under your keys lists retired, revoked
    and expired keys with their dates ("date not recorded" for keys retired
    before this release), with a link from a replaced key to the key that
    replaced it. Under correspondents' keys it lists the keys you removed.
    "Show history" on a correspondent's key lists what happened to it, in
    words.
  - Removing a correspondent's key now says that it is kept in History and
    comes back if they send it again. The new "Don't use this key" is the way
    to stop using a key for good.
  - "Check for an updated key" on each correspondent's key (the main action
    when it expires soon or has expired) says what it found. If they have
    published a different key, the usual "key changed" choice appears. The
    same check is offered from compose's "Can't encrypt: … key expired" line.

### Fixed

- **Compose no longer says "key held" for a recipient whose key has expired or
  been revoked.** The recipient chip and the send line
  ("keys held for ...") now follow the same key check as the Encrypt line:
  "key held" only when a usable key exists, "key expired" or "key revoked"
  otherwise, and "no key" when there is none.

## [1.93.0-rc.1] — 2026-10-21

The first test pre-release of PGP key management: keys that expire and can be
extended, retired or revoked, renewed correspondent keys that are merged
instead of ignored, expiry warnings, and a key-management page in Settings.
Not for production.

### Added

- **PGP keys now have a lifecycle (ADR-040).** New `mailroom-admin`
  verbs for your own keys:
  - `pgp-retire --key-id [--unbind]` makes a key decrypt-only and takes it out
    of WKD in one step. It never signs or is published again, and it still
    opens old mail. A key that is still an identity's default is refused unless
    `--unbind` is given, so mail never silently goes out unsigned.
  - `pgp-extend --key-id --days N` re-signs a key with a later expiry (at most
    ten years) in the crypto process. The fingerprint does not change, so
    correspondents see a renewal, not a new key. It also works on a key that
    never expired, and it refuses a revoked key.
  - `pgp-revoke --key-id --reason compromised|superseded|retired --confirm
    LAST8` makes a real OpenPGP revocation in the crypto process. It cannot be
    undone, which is why the key is named twice. The key still opens old mail.
- **New keys expire after two years** (the primary key and its encryption
  subkey). `pgp-generate --expires-days N` sets another lifetime, and 0 means
  never. Existing keys are unchanged. `pgp-generate` and `pgp-list` show each
  key's expiry.
- **`pgp-report` counts keys by expiry** (expired, under 30 days, under 90
  days, later, never, revoked), your own and your correspondents' separately,
  with the number examined. `--keys-only` skips the message scan. Read-only.
- **WKD keeps serving a revoked key** after your current one, so a
  correspondent's client that refreshes learns that the key is revoked
  (migration 00070, expand-only).
- **Key management over JMAP (server side).**
  Under `urn:mailroom:pgp`: `UserKey/generate` (for a fixed identity; the name
  and address come from the identity, two-year default expiry),
  `UserKey/import` (an armored or base64 secret key and its passphrase),
  `UserKey/set` (bind or unbind, publish or unpublish over WKD, retire),
  `UserKey/extend`, `UserKey/revoke` (name the last 8 characters of the
  fingerprint) and `UserKey/export` (the key wrapped under a passphrase you
  choose, returned once in the response and stored nowhere). Every operation on
  a private key runs in the crypto process. Generate, import and export are
  rate-limited per user; revoke never is. Passphrases are never logged, echoed
  or stored.
- **Expiry facts for your keys and your correspondents'**. `UserKey/get`
  reports `expiresAt`, `expired`, `usable`, `expiringSoon`, `canSign`,
  `origin`, the WKD lookup URL and whether this server publishes over WKD.
  `ContactKey/get` lists the keys held for each contact with source,
  authentication, expiry, revocation and the pinned key, and `ContactKey/set`
  removes one (and its pin). The session capability carries
  `keyExpiryWarningDays` (30) and `keyManagement`.
- **Why a recipient cannot be encrypted to.** `RecipientKey/lookup` now says
  `reason: "expired"` with `expiredAt` when a recipient's only key has lapsed
  (or `revoked`, `keyChanged`, `noKey`), and gives `expiresAt` and
  `expiringSoon` for the key it chose.
- **A signature by a key that has since expired says so.** `pgpSignature` keeps
  the state `bad` and adds `reason: "keyExpired"`, `signedAt`, `keyExpiredAt`
  and `validWhenSigned`, for both signed and signed-and-encrypted mail. Only a
  key Mailroom would otherwise trust for the sender earns the reason; any other
  expired key reads as a plain bad signature.
- **Manage your keys in Settings → Keys.** Each of your keys shows its
  fingerprint in groups of four, the address it is for, when it was made and
  when it expires, and whether it signs, is published, is retired or is
  revoked. From there you can:
  - choose which address a key is for;
  - publish it in your server's key directory (WKD), with the address
    correspondents' apps look it up at, or see why it can't be published;
  - extend it by one or two years or a number of days;
  - back it up to a file protected by a passphrase you choose (Mailroom keeps
    neither the file nor the passphrase);
  - retire it (it stops signing and leaves the directory, and still opens old
    mail);
  - revoke it (a deliberate step: choose a reason and type the last 8
    characters of the fingerprint; it can't be undone);
  - make a new key for one of your addresses (two years by default), or import
    one from another mail app.

  Making, importing, extending, backing up and revoking need the server's key
  service; without it the page says so and offers the rest.
- **Correspondents' keys in Settings → Keys.** The keys Mailroom holds for the
  people you write to, with where each came from (imported, their key
  directory, or their mail, and whether the sender was verified), when it
  expires, whether it is the one encrypted mail uses, and whether you refused
  it. Remove one (after a confirmation), or import a key for an address.
- **Warnings before a key stops working.** The desk shows a banner when
  one of your keys is about to expire, or a key you use has expired, with a
  link to extend it. Dismiss it and it stays away until the key's state
  changes. The warning period is the server's (30 days).
- **Compose names a recipient whose key is expiring or has lapsed.** "Alice's
  key expires on 31 Oct" beside Encrypt, and "Can't encrypt: Carol's key
  expired on 21 Oct" (or "was revoked") instead of a generic "no key".
- **A signature by a key that has since expired** reads "Signed with a key
  that expired on …", with "it was valid when this was signed" when it was, in
  neutral styling: not the warning a signature that doesn't check out gets,
  and not the plain note of a verified one.

### Fixed

- **A correspondent's renewed key no longer stays expired in Mailroom
  forever.** When a key Mailroom already held arrived again with a later expiry
  (by Autocrypt, WKD or import), the stored copy was never updated. The copies
  are now merged, the way GnuPG does it. A renewal takes effect. A replayed
  older copy cannot shorten a key or undo a revocation, and a forged signature
  is ignored. The remembered fingerprint is untouched, so a renewal never shows
  as "key changed". Importing a key you already hold used to replace it, which
  could undo a revocation. It now merges too.
- **An expired, retired or revoked key of yours is no longer advertised.** It
  is left out of the outgoing Autocrypt header and is never served as your
  current WKD key. Mail is not signed with a key whose primary key has expired,
  and the mail still goes out.
- Withdrawing an old key from WKD after publishing its replacement no longer
  takes the replacement out of WKD too.
- **"Will sign" no longer promises a signature from an expired key.**
  `Identity.signingKeyId` now reads the bound key's packets, the same check
  compose uses, so a key whose primary has expired stops reading as signing.
- Binding, publishing and unpublishing a key that is already in that state no
  longer writes change rows.
- `UserKey/get` reports an id that is not a key id in `notFound` instead of
  dropping it.

## [1.92.1] — 2026-10-21

### Fixed

- **On a phone the interface no longer pans sideways, and Safari's toolbar can
  collapse while you read.** Measured on iPhone-sized screens (375 and
  390 px) in WebKit and Chromium, with a 640 px marketing newsletter as the
  test message. Every view was wider than the screen: 422 px, or 498 px with a
  search in the box. The search box could not shrink, so it pushed the top
  bar's buttons off the edge. This predates v1.79 and got worse in v1.88.0,
  when the search-options button was added. When reading, the whole message
  also scrolled sideways under its action row. That row has needed 416 px
  since v1.80.0, when Reply all was added to it.
  - The search box now shrinks to fit, with tighter top-bar spacing on a phone.
  - On a phone the reading pane's row is Archive, In-tray, Snooze, Reply and
    More again. Reply all is the first item in More. More now opens upwards,
    above the row.
  - A long label, or the largest text size, now truncates instead of wrapping.
  - The desk's Follow up line no longer runs past the screen in older Chromium.
  - Reading a message on a phone now scrolls the page itself, so Safari can
    hide its address bar. The action row and bottom bar stay docked at the
    bottom. The desk and lists keep their own scrolling (the virtualised list
    depends on it) and stay sized to the visible screen (`100dvh`).
  - Wide newsletters were never the cause: they already scroll inside their
    own frame.
  - The desktop layout is unchanged.
  - The accessibility lane gained a phone-width step (exit 28). At 375 and
    390 px, on every main view including the wide newsletter, it fails if the
    page overflows, anything lies off screen or scrolls sideways, or reading
    does not scroll the page. It checks its own probe against two injected
    faults first.

## [1.92.0] — 2026-10-20

PGP: read and send encrypted mail. PGP/MIME encrypted mail is decrypted when you
open it, inside the separate crypto process, and the decrypted text is never
stored, indexed or cached (ADR-037); every decryption is audited, and old retired
keys still open old mail. Attachments inside encrypted mail are scanned at
download, and a blocked one says so in the app. You can encrypt outgoing mail to
recipients whose keys Mailroom trusts (imported, published by their domain over
WKD, or sent as Autocrypt on authenticated mail), encrypted to you as well so the
Sent copy stays readable; Bcc is refused when encrypting, and the first key used
for an address is pinned (ADR-038). Signatures are shown honestly for both PGP/MIME
layouts, including the one Thunderbird and GnuPG use, with a "key changed" decision
when a sender's key differs from the one you had (ADR-039); SHA-1 signatures no
longer verify. You can import a correspondent's public key. Encrypted parts of
unencrypted mail (S/MIME, forwarded encrypted messages) no longer lock the outer
message's own attachments, and a key backup made with `pgp-export-wrapped` can be
restored with `pgp-import --passphrase-file`. Tested end to end with Thunderbird
across three release candidates (details below). Migrations
00067 (decryption audit), 00068 (contact key trust and pins), 00069 (refused
keys), all expand-only. The crypto container's default memory limit rises from
128m to 384m for decryption.

## [1.92.0-rc.3] — 2026-10-20

The third test pre-release of PGP: everything in rc.2 plus verifying the
signatures Thunderbird and GnuPG embed in the encryption, and importing a
contact's public key. Not for production.

### Added

- **Import a correspondent's public key (ADR-038).** Until now a contact's
  key could only arrive by WKD or authenticated Autocrypt, so you could not
  encrypt to someone who uses neither. Compose ("Import their key...", beside
  the note that a recipient has no key) and the reading pane's key line
  ("Import a key for this sender...") open a small dialog: paste an armored key
  or choose a .asc/.pgp file, check the fingerprint (in groups of four) and the
  user ID it shows, then confirm. The key is kept as an imported key, ranks above
  WKD and Autocrypt, and replaces the key remembered for that address. Mailroom
  refuses, and stores nothing from, a secret key, a key that does not name that
  address, a key with no encryption subkey, an expired or revoked key, a file
  with several keys, anything over 256 KiB, and one of your own addresses, each
  with its own plain-words message. New JMAP method `ContactKey/import`
  (urn:mailroom:pgp); it writes one Contact change row.

### Fixed

- **Signed and encrypted mail from Thunderbird and GnuPG now shows its signature
  (ADR-039 section 6).** Most clients put the signature inside the
  encryption (RFC 3156 section 6.2) rather than in a nested signed part, and
  1.92.0-rc.2 showed no signature line for it. The crypto process now checks
  that signature while it decrypts, against the keys held for the sender and
  your own, and the reading pane says the same things it says for other signed
  mail: checks out, doesn't check out, a key you haven't seen from this sender,
  or a new key different from the one you had, with the same rules (the key
  must name the sender's address and be one Mailroom would also encrypt to) and
  the same remembering of the first key that checks out. A signature that
  doesn't check out never hides the text. If a decrypted message also contains a
  signed part, that part's signature is the one shown. The decrypt request
  carries up to 16 public keys and its answer a signature status; no new stored
  data.
- **A SHA-1 signature no longer counts as checking out**, on any signed mail. The OpenPGP library accepted SHA-1 signatures when verifying;
  Mailroom now refuses them, as it already refused to make them.

## [1.92.0-rc.2] — 2026-10-20

The second test pre-release of PGP: everything in rc.1 plus encrypting to
recipients with known keys, the signature display with the key-changed
decision, and the fixes from the rc.1 test. Not for production.

### Added

- **PGP signatures are shown honestly, and a changed key is your decision
  (ADR-039).** The reading pane now says what a signature means, on
  ordinary signed mail and on a signature inside a decrypted message alike:
  "checks out against key ...", "doesn't check out", "signed with a key you
  haven't seen from this sender", or "signed with a NEW key, different from the
  one you had for ..." with both fingerprints in groups of four, what a change
  can mean (the person changed keys, or someone is pretending to be them),
  "Trust this key from now on", "Keep the old key" and a way to compare. A
  signature counts as checked only when the key names the sender's address and is
  one Mailroom would also encrypt to; a key that arrived in the very message it
  signs is never enough on its own. The first time a key checks out for an
  address it is remembered, so a later different key is flagged even for someone
  you have never encrypted to. Compose shows the same choice when it refuses to
  encrypt because a recipient's key changed; trusting the key lets the encrypted
  send go to it, keeping the old key leaves sends going to the old one. New
  Email property `pgpSignature` (replaces `encryptedSignature`, which only
  appeared in the 1.92.0-rc.1 test pre-release) and JMAP method
  `ContactKey/decide` (urn:mailroom:pgp), which writes a Contact change row;
  `Contact.keys` entries gain `refused` (migration 00069,
  `contact_keys.refused`).
- **Encrypt outgoing mail to recipients whose keys you hold (ADR-038).**
  Compose shows an "Encrypt" switch when every To and Cc recipient has a usable
  key, and says which recipients lack one when it does not. With it on, the
  message is signed and then encrypted (RFC 3156) to the recipients and to your
  own key, so the copy in Sent stays readable; the stored message is rewritten at
  send, like signing. Drafts are never encrypted. **A message with a Bcc
  recipient cannot be encrypted** (one encrypted message would reveal every
  recipient's key, including the Bcc ones), and a send that cannot be encrypted
  is refused rather than sent in the clear. The subject and the recipients are
  not encrypted. New JMAP method `RecipientKey/lookup` and the
  `EmailSubmission/set` create property `encrypt` (urn:mailroom:pgp, which now
  also advertises `encrypt`); a key published over WKD is looked up (bounded) and
  stored when compose asks. GnuPG decrypts what Mailroom encrypts (interop test).
- **Which key an address is encrypted to is a trust decision, not "the newest
  key" (ADR-038).** A key is used only if it was imported by you, published by
  the recipient's own domain over WKD, or sent as an Autocrypt header on a
  message that was authenticated for its From domain (a DMARC pass under a
  trusted Authentication-Results, or an aligned local DKIM pass, recorded at
  ingest in the new `contact_keys.authenticated`, migration 00068; existing keys
  count as unauthenticated); it must be unexpired and unrevoked and carry a user
  ID naming the address; imported beats WKD beats authenticated Autocrypt
  whatever the dates. The fingerprint encrypted to is remembered per address
  (`app.contact_key_pins`, migration 00068), and if a recipient's key later
  differs the send is refused and both fingerprints are shown, until you import
  the new key (the confirm step is the next increment). Without these a stranger
  could plant a key with one forged-From message and read your next encrypted
  mail to that address.
- **The crypto process's `encrypt` method.** Uses no private key; encrypts to the
  recipients' public keys plus the sender's own; refuses a recipient without a
  valid unexpired unrevoked encryption subkey by naming the address; writes only
  integrity-protected (SEIPD) packets and checks its own output; bounded at 24
  MiB, below the decrypt cap so the sender can always open what they sent.
- **Download-time scan verdicts for decrypted attachments are logged and counted.** One INFO line per scanned attachment (message id, part path, verdict,
  detection name when flagged, size; never a filename or content) and a new
  `mailroom_ondemand_scan_total{verdict}` counter (clean, flagged, unknown,
  unscanned), so the on-demand scan can be confirmed without sending malware.

### Changed

- **A blocked download is explained in the app, not as the browser's bare "403
  Forbidden" page.** Download, Open in a tab and the PDF preview now ask
  the server first. If the scanner flagged the attachment the app says "Blocked:
  the virus scanner flagged this attachment (<detection name>)."; if the scanner
  could not check it, "Blocked: this attachment was not scanned because the
  scanner could not check it."; nothing is navigated to. The refusal carries the
  verdict in `X-Mailroom-Attachment-Scan` and, for a part of a decrypted message,
  the detection name in `X-Mailroom-Attachment-Signature`. This applies to every
  blocked attachment, not only decrypted ones. For an encrypted message the check
  costs one more decrypt and scan per Download.
- **Opening a message decrypts it once.** Opening mounted both the list and
  the reading pane, and each asked for the thread's body, so an encrypted message
  was decrypted twice a few milliseconds apart (two `body` audit rows). An open
  now joins a load already in flight for the same thread. Nothing is kept after
  the request finishes, and a reload after a change (release, key decision) still
  asks the server again.

## [1.92.0-rc.1] — 2026-10-20

A test pre-release of the PGP work so far, for trying real
encrypted mail before encrypt and the signature display
are built. Not for production.

### Added

- **PGP/MIME encrypted mail is decrypted when you open it, and the decrypted
  text is never stored (ADR-037).** When a message is RFC 3156 encrypted
  and the reading pane asks for its body, the crypto process decrypts it with
  your keys (including retired and revoked ones, which keep opening old mail)
  and the body is rendered through the same sanitising pipeline as any other
  mail. The plaintext lives only in that request: it is not written to the
  database, the blob store, the search index, the snippet, the classifier or
  any cache, so encrypted mail is not searchable. The reading pane says
  "Encrypted message" or "could not be decrypted", with no further detail by
  design. A PGP/MIME signature inside the decrypted message is verified and
  shown. Attachments inside a decrypted message are decrypted again and scanned
  by the malware scanner when downloaded or previewed, under the usual
  quarantine rules, which lifts the blanket quarantine on encrypted mail that
  can be opened. New Email properties `encrypted`, `decrypted` and
  `encryptedSignature` (urn:mailroom:pgp).
- **The crypto process's `decrypt` method.** One opaque error for every
  cryptographic failure; legacy non-integrity-protected (SED) messages refused;
  plaintext capped at 32 MiB after decompression; a per-user rate limit; at most
  two decryptions at once; and a persistent, append-only audit row for every
  decryption attempt (`app.pgp_decrypt_audit`, migration 00067), written by the
  crypto process itself before any plaintext is returned. Headers inside the
  ciphertext are ignored except the MIME content fields, so an encrypted message
  cannot change its visible sender. A GnuPG interop test now decrypts a message
  gpg encrypted to a key the crypto process generated.

### Changed

- **A `pgp-export-wrapped` backup can now be restored in Mailroom.**
  `mailroom-admin pgp-import` takes an optional `--passphrase-file`; with it, the
  crypto process unlocks a passphrase-protected key (the file `pgp-export-wrapped`
  writes) with go-crypto and stores it like any other import, sealed under the
  instance's PGP root. A wrong passphrase gives one opaque error and the
  passphrase is never logged. Without the flag a protected key is still refused,
  now with a message naming the flag. `deploy/README.md` §6b documents the full
  export, storage and restore procedure and what is lost without a backup.

- **The crypto container's default memory limit is 384m (was 128m).** A
  decryption holds a whole message in memory several times over. An existing
  deployment must raise `MAILROOM_CRYPTO_MEM_LIMIT` (or update its compose file)
  itself: a release ships images only.

### Fixed

- **A forwarded encrypted message no longer quarantines the outer message's own
  attachments.** Attachment scanning treated "an encrypted message appears
  anywhere in the tree" as "every part is ciphertext", so an ordinary message with
  its own PDF plus an encrypted message forwarded as an attachment had the PDF
  recorded `unknown` and refused at download. The rule is now scoped to the
  encrypted subtree: parts inside a PGP/MIME or S/MIME encrypted node, and the
  forwarded message/rfc822 part that itself contains encrypted content (at any
  depth), stay `unknown` and are never sent to the scanner; siblings are scanned
  normally. `pgpmail.Detect` now reports a message's own tree only; the
  `pgp-report` census uses the new `DetectWithNested`, so its counts are unchanged.
  Opaque-signed S/MIME (`smime-type=signed-data`) stays held as encrypted on
  purpose: that label is written by the sender, so honouring it would let a
  sender get ciphertext scanned and recorded `clean`. Telling the two apart safely
  needs the CMS content type, which is not worth building while S/MIME is deferred.

## [1.91.0] — 2026-10-20

### Added

- **Mailroom verifies DKIM itself, at ingest, for mail with no trusted
  Authentication-Results (ADR-036).** Mail that reaches Mailroom without a header
  from a trusted authserv-id, chiefly the operator's own services submitting
  through the provider, used to read "unauthenticated" although it is signed. Now
  its signatures are verified in-process before the message is stored (DNS,
  bounded to 3 seconds a lookup, 4 lookups and 8 seconds a message; a DNS failure
  stores nothing). A signature that passes with a signing domain aligned to the
  From domain (equal, or a parent, never a public suffix) makes the reading pane
  say "Verified by Mailroom (DKIM, example.com)", distinct from a provider
  verdict, and counts as an affirmative pass for the own-domain spoofing warning.
  A failure is never shown (12 provider-pass, local-fail disagreements in the
  calibration are unexplained), and a provider's verdict always wins. A
  header-prepend guard stops a forged `From`, or an added `Reply-To`, from
  riding a genuine signature: every displayed identity header must be covered by
  it. Mail already stored is not re-checked.
- New table `app.message_local_auth` (migration 00066), under forced row-level
  security; new setting `MAILROOM_LOCAL_DKIM` (default on; `off` disables it);
  new metric `mailroom_local_dkim_total{result}`, including the outcomes that
  store nothing (`skipped_trusted_header`, `not_checked`, `no_signature`).
- `auth-report --authserv` counts messages shown as verified by Mailroom as their
  own row, and prints the passes the header-prepend guard withheld.

## [1.90.7] — 2026-10-20

### Added

- **`auth-report --dkim` names why a signature did not pass.** Two new tables
  break non-passing signatures down by reason (`status: detail`, for example
  `permerror: signature expired`) and by signing domain, so an expiry judged
  against today can be told apart from a signature that is actually wrong.

## [1.90.6] — 2026-10-20

### Added

- **`mailroom-admin auth-report --authserv --no-ar --dkim`: would verifying DKIM
  locally authenticate the mail that carries no Authentication-Results?**
  Read-only; it changes no behaviour. In the same corpus walk it verifies the
  DKIM signatures of every such message in-process (new `internal/dkimverify`,
  which hides `go-msgauth` behind our own `Result`/`Status` types), and of
  every Nth control message (`--dkim-sample`, default 20) as a calibration
  sample whose local outcome is set beside the provider's own `dkim=` verdict
  from its trusted header, with an agreement matrix and rate. A message passes
  if any signature passes; passes are split into d= aligned with the From
  domain (organisational domain approximated as the last two labels) and not.
  Tables by status, From domain, topmost `Received: from` route and received
  month. DNS lookups are cached for the run, capped at 5,000, and each has a
  5 s timeout; the report states the budget used and counts the signatures
  that came out `temperror` because it ran out. Keys are fetched at report
  time, so mail whose key has rotated reads as `key_not_found`.

## [1.90.5] — 2026-10-20

### Added

- **`mailroom-admin auth-report --authserv --no-ar`: what is the inbound mail with
  no Authentication-Results?** Read-only. Lays that set (the "other inbound" row)
  beside a control group (inbound mail with a header from a trusted id, same
  months) and counts both per month and account, live mailbox, From domain,
  `Received:` hop count, topmost `Received:` by/from host, and the presence of
  `DKIM-Signature`, ARC, `List-Id`, `Delivered-To`/`X-Original-To`,
  `X-Forwarded-*`, `Resent-*` and an empty `Return-Path`. Every top-N table
  states its N and what it cut; example message ids (ids only) are listed for
  the no-header side. Same single walk, no behaviour change.

## [1.90.4] — 2026-10-20

### Fixed

- **Only the topmost trusted authserv-id speaks for a message.** With two ids
  trusted at once (the provider's current id, and its old one bounded by date),
  a header bearing the old id BELOW the current one was believed alongside it.
  A sender can write that header (RFC 8601 §5 has the provider strip only its
  current id), so on mail received during the provider's changeover it could
  add a `pass` to the reading-pane verdict and switch off the `selfSpoof`
  warning. The notice and the warning now read only headers with the same id as
  the topmost trusted one; a provider's split verdicts under its own id are
  still all read. Learned placement and calendar verification already read only
  the topmost trusted header. Also: the auth-report's "self" row described a
  From-address rule that v1.90.3 no longer applies.

## [1.90.3] — 2026-10-20

### Added

- **`MAILROOM_AUTHSERV_IDS` entries can be date-bounded: `id<YYYY-MM-DD`.** The
  id is trusted only for mail whose received time (the provider's INTERNALDATE,
  never the sender's Date header) is strictly before 00:00 UTC on that date.
  Mail imported from an archive is never judged against a bound, since its
  received time can come from the Date header. It
  is for a provider that changed its id: RFC 8601 section 5 strips only the
  current id, so a forged header bearing the old one must stay untrusted for new
  mail, while older mail regains its verdicts for the notice, learned placement
  and calendar verification. One resolver (`ml.TrustedAt`) is applied at every
  consumer; a raw bounded entry that is not resolved matches nothing, and a
  malformed bound stops the server and `mailroom-admin` at startup.
- **`mailroom-admin auth-report --authserv` shows when each id was topmost and
  what the notice would do.** Per authserv-id, the first and last topmost
  received date (UTC), and notice counts under the configured rule in separate
  rows: trusted verdict, silenced (untrusted only), unauthenticated, none for the
  user's own mail, and none because no id is configured; mail with no header at
  all is split into locally authored, Sent or Drafts, From one of your
  identities (report-only), and other inbound. A report-only count shows how
  many messages carry only untrusted headers; they still read "unauthenticated",
  because a header the sender can write must not turn a warning off (ADR-035).
  The notice is unchanged otherwise, and is now null for mail in a Sent or
  Drafts mailbox as well as for locally authored mail. `from-field-report`
  counts its notices by the same rule.

## [1.90.2] — 2026-10-19

### Fixed

- **Bulk actions on a search reach every match, not the first 500.** Selecting
  all the mail from one sender and archiving it moved only 500 when there were more:
  `Email/query` asked the search index for its default 500 hits, then filtered,
  sorted and paged only those, so no text or `from:` search could enumerate more
  than 500 and any `position` past 500 returned nothing. That also stopped the
  sender card's "Move all" at 500. `Email/query` now takes an optional
  `searchLimit` (clamped to 500 to 5,000; a negative value is `invalidArguments`;
  absent means 500, so ordinary list views and the desk are unchanged), and
  `searchCapped` reports the limit that was actually applied. The web client
  sends the maximum when it enumerates every match. When "select all matching"
  is still truncated (for example the first 1,000 of a larger set), a verb applied to it now
  leaves an offer, "Done 1000 of N - select the next N−1000", which waits for
  the server to settle and then selects only what has not been processed yet,
  whether the verb removed the mail from the view or left it there.
  Trade-off, measured on 20,000 matching messages with 3 KB bodies (Tantivy):
  5,000 hits take about 0.4 s. Normal search is about twice
  as fast as before, because a hit that is not being highlighted now serialises
  only its id and date instead of its subject and body:

  | hits | before (all fields) | after (id and date only) |
  |---|---|---|
  | 500 | 146 ms | 70 ms |
  | 5,000 | 1,536 ms | 407 ms |

## [1.90.1] — 2026-10-18

### Added

- **`mailroom-admin auth-report --authserv`**, read-only. It shows which
  receiving servers wrote each message's Authentication-Results headers.
  For each authserv-id it gives how many messages carry it, how many carry it
  as the topmost header, and whether it is trusted. By month received, it
  gives how much mail had no header at all, a trusted one, a trusted one on
  top, or only untrusted ones. It tells imported mail from another provider
  apart from a receiving host missing from `MAILROOM_AUTHSERV_IDS`. It prints
  server hostnames and counts only.

### Changed

- **A message with more than one From field is now treated as suspect.** RFC
  5322 allows one, and two can make the sender you see differ from the one
  the receiving server checked. Learned placement no longer files such a
  message, including one with an empty From beside the real one, which it used
  to count as a single From. The reading pane now warns that the message names
  more than one sender. In practice none were found when this was checked.

### Fixed

- **A folded message in a conversation is now named by the text it shows**,
  the sender, preview and time, instead of "Show the message from …". Speech
  control and screen readers now match what is on screen (WCAG 2.5.3). The
  accessibility checks now cover a conversation with a folded message on
  purpose.

### Corrected

- The 1.90.0 notes describe the opt-in "You haven't replied to this" reminder
  as covering mail "that is either a reply or from someone you have written to
  before". The rule is: from a person, sent to you directly, and either judged
  personal by the mail-kind classifier or a reply. Having written to the
  sender before is not part of it.
- `internal/sieve`'s package comment still called the package read-only after
  out of office added its writing session. It now describes both halves.

## [1.90.0] — 2026-10-18

**Replies & time away**. Calendar
invitations show as a card with Accept, Maybe and Decline, sent as a real reply from the
address the invite reached; updates and cancellations change the card only when they verifiably
come from the organiser. Follow-up nudges: "no one answered your mail" after 7 days (on), and an
opt-in "you haven't replied", both measured on real mail before they were built. An
out-of-office responder installs its own Sieve script at the provider, refuses to displace a
script it did not write, and removes itself cleanly. Two read-only admin reports
(`sieve-report`, `nudge-report`) shipped first as `1.90.0-rc.1` below, and their measurements decided the design. Migrations 00063 (calendar), 00064 (nudges) and 00065
(out of office), all expand-only.

### Added

- **`mailroom-admin from-field-report`**, read-only: how many of a user's
  messages carry no From, several From fields, an empty From beside a real
  one, a group, or several mailboxes, and for each shape how often learned
  placement acted on such a message and the reading pane showed an
  authentication notice or a trusted DMARC pass. Counts and message ids only.
  It measures before learned placement and the sender warnings start requiring
  exactly one From field.

- **Calendar invitations.** A message carrying an invitation now shows a card
  above the body: the title, when it is (in your own time zone, with the zone
  named), where, who organised it, who is invited and what each has answered,
  and how it repeats. **Accept**, **Maybe** and **Decline** send your answer to
  the organiser from the address the invitation reached you at, through the
  ordinary send — so **Undo** in the toast takes it back. The card then says
  "You accepted", on every copy of the invitation, after a reload too.
- A **cancelled** event says so on its card, and so do earlier invitations to
  it; an **updated** invitation says it updates an earlier one, and the earlier
  one says there is a newer version. A cancellation or update changes other
  copies only if it is authenticated as coming from the event's own organiser;
  one that is not says it could not be verified, and changes nothing else.
- An attendee's **reply** shows who accepted, said maybe or declined.
- An invitation too large to read safely (over 256 KiB) says so rather than
  showing nothing. Mailroom never answers an invitation on its own, never opens
  a link inside one, and never expands a repeating event.
- **Follow up, on the desk.** When no one has answered mail you sent to
  people for a week, the desk says so under the in-tray strip: "No one
  answered your mail to … from Tue, Oct 7". Open the message, mark it
  **Done** (it does not come back), or **Snooze** it. Replies count wherever
  they arrive, including in a different conversation; automatic replies such
  as out-of-office messages do not. A reminder stops by itself a month after
  the message.
- **"You haven't replied to this", if you want it.** An opt-in reminder for
  mail from a person, sent to you directly, that is either a reply or from
  someone you have written to before, and that you have not answered after a
  few days (3 by default; choose 1 to 14). It is off by default because in
  practice only a minority of the messages it picks get a reply. Turn
  either reminder on or off in **Settings › In-tray**.
- `mailroom-admin nudge-report` now also measures the exact rule the opt-in
  reminder uses (`person+to-me+(kind-personal|is-reply)`), and marks the two
  rules that ship, so what is shown can be checked against what was measured.

- **Out of office.** Settings › Out of office sets up an automatic reply for
  each mail account. You choose:
  - the first and last day away (or leave them open);
  - a subject and a message;
  - how often the same sender can get a reply (once every 1 to 30 days);
  - which of the account's addresses the reply comes from;
  - whether to reply only to people you have written to or marked as VIP.

  Your mail provider sends the replies, so they keep working while Mailroom
  is closed. Mailroom installs one filter script of its own on the provider.

  If another filter script is already active there, Mailroom will not switch
  it off. It says which script is active and saves nothing. Turning the reply
  off puts things back exactly as they were. If you have since turned on your
  own filters in your provider's webmail, they stay on.

  If the reply is changed or switched off in the provider's webmail, Settings
  says so. Mail your provider marks as spam gets no reply, nor does mail sent
  to a per-site catch-all address. These replies are not counted by the
  sending quota meter, because Mailroom never sees them.

## [1.90.0-rc.1] — 2026-10-17

A measurement pre-release. It adds two read-only admin reports, run on a real
instance before the features they inform are built.

### Added

- **`mailroom-admin sieve-report`**, the read-only first step toward the
  out-of-office responder. Before Mailroom writes any server-side filter, this
  shows what each account's mail server already holds over ManageSieve: what
  the server supports (vacation replies, `include`, and the rest), every
  filter script with its size and which one is active, and the active
  script's outline: the commands it uses and whether it already sends a
  vacation reply. It never prints a script's contents unless you pass
  `--show-active`, because filters hold addresses and folder names. It cannot
  change anything on the server.
- **Follow-up nudges, measured first.** A new read-only admin report,
  `mailroom-admin nudge-report --user-id … [--since 180d] [--examples]`,
  measures how accurate "you haven't replied to this in N days" and "no one
  answered the mail you sent" nudges would be on your own mail, using what you
  actually did next as the answer: per candidate rule, the share of flagged
  messages you did reply to (with the bottom of its 95% confidence interval),
  how many of your replies it would have caught, and how many nudges a week it
  would have sent. It also shows how your sent mail is recognised (Sent folder,
  sends from Mailroom, your addresses) and whether replies land in the right
  conversation. Counts and message ids only — no subjects or addresses. Nothing
  is nudged yet; the rule is chosen from these numbers.

### Fixed

- **Turning notifications off and on keeps this device's setting.** It used to
  reset to "every message"; the choice is now remembered on the device and sent
  with the new registration.
- **The quiet-hours note in Settings now says what happens:** every device,
  whatever its setting, notifies only about VIPs during quiet hours, and about
  nobody if "Allow VIPs" is off.
- **On a phone, closing a message returns the desk to where you were.** Opening
  a message hid the list, and the list forgot how long it was; closing it landed
  you far above where you had been. The same list is used by the in-tray page.
- **The desk's cursor starts at the top of the desk.** If the desk loaded before
  your areas were known, the cursor started in Needs sorting at the bottom; the
  desk now waits for the areas.

## [1.89.0] — 2026-10-17

### Added

- **Block a sender.** From the sender card, block an address: new mail from it
  goes straight to Junk (or Trash, if you choose), before rules or learned
  placement can file it, and it never notifies you. The move is a real one —
  your other mail apps see it in Junk too. Mail already here moves only if you
  tick "Also move the mail already here", after the count and a confirm, with
  "Move the rest" past 1,000. Unblock from the card or from **Settings ›
  Senders**, which lists everyone you have blocked. Blocking matches the exact
  address.
- **VIPs.** Mark a sender as a VIP from their card or in Settings › Senders.
  `is:vip` finds their mail.
- **Choose what each device notifies you about:** every message (still the
  default), mail above the line, mail from VIPs, or either — in Settings ›
  Notifications. A "Mark important" rule now counts as above the line for
  notifications; until now that rule action did nothing.
- **Quiet hours.** Pick a start, an end, the days and your time zone; during
  quiet hours only VIPs can notify you, and only while "Allow VIPs during
  quiet hours" is on. Off until you turn it on.
- **Notifications per mail account.** Switch one account's notifications off
  in Settings › Notifications, or in the account's own settings.
- **Clean up.** Settings › Clean up ranks who sends you the most mail, or
  whose mail you ignore most (still unread, or archived without reading), over
  the last 30, 90 or 365 days or all time — by sender, by domain or by mailing
  list, since newsletters change addresses. For each one: unsubscribe (asked
  first, never automatic), archive all, move all to an area, or block, each
  with its count stated before anything moves and one Undo.

### Changed

- The README describes 1.88.0's search: the new operators, live results, the
  options form and best-match order.
- A push subscription's never-read `importantOnly` flag is replaced by its
  notification mode; a client that still sends `importantOnly` is ignored
  rather than switched to important-only.

### Fixed

- **Mailroom now learns which folders accept keywords.** The check that
  decides whether a folder can store a keyword such as `$NotJunk` was read
  while the folder was opened read-only, and a mail server always answers "no"
  in that mode, so every folder was recorded as refusing keywords. The answer
  is now taken when Mailroom opens a folder to change something in it, and a
  later read-only sync no longer overwrites it. In practice, after Mailroom
  first changes something in your Junk folder, rescuing a message with "Not
  junk" also tells the provider it is not spam (`$NotJunk`).
- **`from:@com` is refused, like `from:*.com`**. A bare top-level domain
  behind the `@` marker used to run and silently match nothing; now it is
  rejected as an invalid search for `from:`, `to:`, `cc:`, `alias:` and
  `deliveredto:`.
- **A full email address typed as plain text no longer matches near misses**. `alice@example.com` on its own used to be fuzzed, so it also found
  `alice@example.org` and `alices@example.com`. It now matches that address
  exactly as sender, recipient or delivered-to. To find mail that only mentions
  an address in its text, put it in quotes.
- **Adding an area's label from the label menu, or dropping mail on an area in
  the rail, now moves the message**, as Re-file and Move to area do: the
  other areas' labels come off and the new area's goes on, with one Undo that
  restores the previous labels exactly. Adding only the label used to leave the
  message in its earlier area. Area entries in the rail are now drop targets.
- **`mailroom-admin apply-rules` swaps area labels**, as the rule
  editor's Apply does. A rule that files into an area moves matched mail out of
  its earlier area; mail you or another rule filed elsewhere stays and is
  counted in the report. `--undo` removes the rule's label but does not put the
  earlier area's label back.
- **"Not junk" now lasts past the next sync** on providers that mark spam with
  a `$Junk` keyword as well as by folder (Thunderbird-style filters, Apple
  Mail, some server spam filters). Rescuing a message takes the keyword off
  before moving it to the inbox and adds `$NotJunk`, the usual "this is not
  spam" signal, where the folder accepts keywords. Before, the move carried
  `$Junk` into the inbox, so the next sync marked the message junk again,
  which also hid it from search. Moving a message into Junk removes a
  `$NotJunk` it carries, but does not add `$Junk`, because the spam verdict
  stays the provider's.
- **Changes made together now reach the mail server in the order you made
  them.** When one action changes a message's flags and also moves it, the
  flag changes are applied first. Before, the order was not guaranteed, so a
  flag change could land on the old copy after the move and do nothing. The
  flags Mailroom has just written are also remembered on the old copy, so a
  move can no longer bring back a flag you removed.
- **A held inline image is no longer invisible.** When the attachment scanner
  holds an image that sits inside the message body (a logo or banner), the
  message now lists it with the same "withheld" chip and Release action as a
  held attachment, and never asks for its bytes. An image whose name the
  message body never mentions is listed as a file instead of appearing
  nowhere.
- The Release button on a "withheld" chip is readable in the dark theme. Its
  red text sat on the browser's grey button face and was below the AA
  contrast minimum.

### Corrected

- The 1.83.0 notes and ADR-031 name "Notes" among the default areas. The default
  set is five areas, one per category tab: Primary, Social, Promotions, Updates
  and Forums. "Notes" exists only if you create it.

## [1.88.0] — 2026-10-17

### Added

- **`OR` and brackets in search.** `invoice OR receipt`, `(from:alice OR
  from:bob) -is:read`, and `-( … )` to exclude a whole group. `OR` must be
  upper case (a lower-case `or` is just a word), and it binds looser than the
  spaces between words: `a b OR c` means "(a and b) or c". A bracket that
  closes nothing, or is never closed, is forgiven rather than turning a pasted
  search into an error or an empty list.
- **Relative dates:** `newer_than:7d` and `older_than:1y`, in days (`d`), weeks
  (`w`), calendar months (`m`) or years (`y`), counted in whole UTC days. A
  value Mailroom cannot read, such as `older_than:3x`, is refused with a
  message.
- **Search by area:** `area:Work` finds the mail that shows in that area on
  your desk, and `-area:Work` leaves it out. An area name that does not exist
  finds nothing.
- **Lists and attachments:** `list:` finds a mailing list by its List-Id, or
  every list under a domain (`list:lists.example.org`); `has:unsubscribe`
  finds mail with an unsubscribe link; `filename:` matches an attachment's
  name or extension (`filename:invoice`, `filename:pdf`,
  `filename:report.pdf`); `larger:` and `smaller:` take a size such as `500K`
  or `10M`.
- **Attachment types:** `has:pdf`, `has:image`, `has:document`,
  `has:spreadsheet` and `has:presentation`, decided by the attachment's type
  and, when that says nothing, its file extension.
- **Search by the address mail was delivered to:** `alias:` (or
  `deliveredto:`) finds everything sent to one of your addresses, even when it
  is not in To or Cc; like `from:`, it takes a domain too.
- **Desk state in search:** `is:snoozed`, `is:muted`, `is:pinned`, and
  `is:important` (also `is:above-line`) for mail above your desk's line.
- **`in:trash`, `in:junk` and `in:anywhere`.** Search leaves deleted mail and
  junk out unless you ask; `in:anywhere` includes both. `in:junk` and `is:junk`
  now also find mail another app filed in your Junk folder.
- Every new operator is in the search suggestions and the search help.
- **Mailroom rebuilds each search index once after this update** so that mail
  already in the mailbox can be found by list, size, attachment and delivery
  address. It runs in the background (a few minutes for a mailbox of 25,000
  messages) and searches keep working meanwhile; only the new kinds of search
  answer "the search index is being upgraded" until it finishes.
- **Results as you type.** The list updates after a short pause in typing
  (250 ms), treating the word you are still typing as the start of a word, so
  `invo` already finds "invoice". The previous results stay on screen until the
  new ones arrive, and a slow answer for an earlier keystroke can no longer
  replace a newer one. Pressing Enter runs the search as typed and is what
  goes into recent searches.
- **Search suggestions from your contacts.** With the cursor in a `from:`,
  `to:`, `cc:`, `alias:` or `deliveredto:` value, the suggestions include your
  contacts (name and address), not only the senders on screen; choosing one
  inserts the exact address. `area:` suggests your area names. Suggestions now
  follow the cursor to whichever word it is in, not only the last one.
- **Search options.** A button in the search box opens a form — From, To,
  Subject, Has the words, Doesn't have, Date within, Larger/Smaller than, Area,
  Has attachment (any, PDF, image, document, spreadsheet, presentation) and
  Include Trash and Junk — that writes the search for you. Opening it reads the
  current search back into the fields; anything it has no field for stays in
  "Has the words", so nothing you typed is lost.
- **Newest first or Best match.** Search results can be ordered by relevance.
  Paging works the same in both orders.
- **A capped search says so.** When a search has more matches than it can
  return, the count reads "50 of at least 500" instead of presenting 500 as the
  total, and Email/query reports the bound as `searchCapped`.
- For operators: Email/query takes `{"property": "relevance"}` as a sort and
  `queryPrefix: true` beside `query` in a filter (advertised as
  `relevanceSort` in the search capability), and the server records
  `mailroom_search_duration_seconds{kind="search"|"snippet"}`, the time each
  search-index read takes — the number to read before changing the typing
  pause.

### Security

- **A search query can no longer crash the server.** Brackets nested
  thousands deep could exhaust the parser's stack; nesting beyond 32 levels is
  now read as plain text, and a search query longer than 4 KiB is refused
  with a message.

### Changed

- **Search leaves junk out by default**, as it already did deleted mail: a
  search typed in the search box no longer returns mail marked as junk or in a
  Junk folder unless it asks (`in:junk`, `is:junk`, `in:anywhere`). Searching
  inside the Junk folder itself still searches its mail.
- **The README is rewritten** as a short account of what Mailroom is, what it
  does now and what is planned, with links into the wiki for depth. The wiki
  was checked page by page against the code at 1.87.0 and gained pages for
  learned placement, moving mail between areas and day-one setup.
- The project instructions now record that the licence is AGPL-3.0-only
  (ADR-011). They had still described the decision as deferred.

## [1.87.0] — 2026-10-16

### Added

- Move mail to an area in bulk. The message list's selection bar has a
  "Move to area" menu that files every selected message (including "select all
  matching") into the area you pick, with one Undo; messages already there are
  left alone and counted. The sender card has "Move all from <address> to…",
  which counts that address's messages, asks you to confirm, then moves them all.
  A selection too big to move in one go (over 1,000) says how many it moved and
  how many it did not touch, and the sender card offers to move the rest.
- **Search by domain.** `from:shop.example` now finds every message from that
  domain and its subdomains (`news@shop.example`, `orders@mail.shop.example`);
  `from:*.shop.example` and `from:@shop.example` mean the same, and `to:` and
  `cc:` take a domain too. A full address such as `from:alice@shop.example` is
  still an exact match, and look-alike domains (`notshop.example`,
  `shop.example.evil`) are not matched.
- **A search clause that can match nothing is now an error, not an empty
  list.** `from:*foo`, `from:@` or `from:alice@` are refused with a message
  saying what is wrong.
- **Mailroom rebuilds each search index once after this update** so that mail
  already in the mailbox can be found by domain. It runs in the background and
  searches keep working meanwhile; only domain searches (`from:shop.example`)
  answer "the search index is being upgraded" until it finishes.

### Fixed

- **Typing a bare domain such as `shop.example` no longer returns most of your
  mailbox.** It was split into its parts (`shop`, `example`) and typo
  tolerance then matched the pieces of `.com`/`.example` almost everywhere. A
  word that looks like a domain is now searched as an exact phrase.
- **"Move all from <address> to…" no longer says a sender has no mail when the
  count could not be made.** On a server with search turned off it now says
  so; "select all matching" reports a failed query instead of selecting part
  of the set.
- **Search keeps indexing new mail after a rebuild.** The rebuilt index was
  left open at the temporary directory it was built in, which the swap had
  renamed away, so every message that arrived afterwards failed to index
  ("No such file or directory") until the server restarted. Every rebuild
  hit this: a new user's first index, `reindex-user`, and this release's
  one-time rebuild for search by domain.

## [1.86.2] — 2026-10-16

### Fixed

- "Apply to existing mail" (and its preview) now looks at your whole mailbox for
  any rule that only tests the sender, subject, list or similar message facts.
  It used to read only the newest 500 messages, so a rule's older matches were
  never found or moved, even though the rule was saved and enabled. Rules
  that must open message bodies (recipients or named headers) keep their smaller
  scan and still say when they did not see everything.

## [1.86.1] — 2026-10-16

### Fixed

- **The desk no longer pulls you back to the highlighted message while you
  scroll.** The list brought its cursor row back into view whenever it
  re-measured its row height, not only when the cursor moved. It re-measures
  on every scroll, so on a display where that height wobbles by a fraction of
  a pixel you could not scroll the highlighted message out of view at all. A
  density or text-size change while scrolled away also jumped back to it. The
  list now follows the cursor only when the cursor moves (`j`/`k`, the
  arrows, archive-and-advance, opening a message), never because you
  scrolled. The accessibility lane now scrolls away from the cursor and checks
  that the list stays put.
- **A plain click on a message clears the ticked selection**, on the desk and
  the tray page. Rows you had Ctrl- or Shift-clicked stayed selected when you
  clicked another message to open it, so the next action could apply to mail
  you thought you had left behind. A plain click on the subject now opens that
  message and clears the selection, as a file manager does. A Ctrl- or
  Shift-click still adds to the selection. A plain click on the sender still
  ticks or unticks that one row and keeps the rest, because it is the one way
  to select without a modifier. Selecting from the keyboard (`x`, Space,
  Shift+Space) is unchanged.
- **Escape and the reading pane's ← close the pane, in one step.** Each
  message you opened added a browser history entry, and closing went back one
  entry, so after opening three messages Escape showed you the second, then
  the first, before it reached the list. Opening a message while another is
  open now replaces the entry instead of adding one. Escape, the ← button and
  the browser's Back button all return to the list in one step, and Forward
  reopens the last message you were reading. This applies to every message
  list, not only the desk. A second Escape clears a ticked selection, as
  before. The highlighted row stays: it is where `j`, `k` and Enter act, and
  the row a screen reader is told is current.

## [1.86.0] — 2026-10-16

### Added

- **Learned placement can sort the mail you already have.** A new
  operator command, `mailroom-admin placement-backfill`, files existing desk
  mail from senders Mailroom has learned into the area you file them in. By
  default it only reports: how many messages would move, between which
  areas, a line per sender with where its mail is now, and why everything
  else stays put. `--apply` makes exactly those moves, and `--exclude` leaves
  a sender or a whole domain alone.
- **The same checks as new mail.** Every message it would move has its own
  original headers read, and it must pass the checks new mail passes: one
  sender in one From header, an authenticated sender, and no spoofing or
  look-alike warning. A message that fails any of them, or whose stored copy
  cannot be read, stays where it is and is counted by reason. So the count it
  reports is exact, not an estimate. If you have switched learned placement
  off, it still reports but will not apply.
- **Nothing you or your rules chose is touched.** Mail you have filed
  yourself, or that a rule or an earlier learned placement put somewhere, is
  left alone. Each move behaves like a Re-file: the automatic placement comes
  off and the learned one goes on, and nothing changes on your mail server.
- **Every batch can be undone exactly.** `--undo BATCH_ID` puts each message
  back where it was, with the same attribution as before. A message you have
  moved since the batch stays where you put it.
- **Moves you correct still teach it.** Moving a backfilled message somewhere
  else counts as a correction of that sender, exactly as for new mail: the
  sender stops being sorted automatically. Each area's safety gate still
  judges only how well new mail was sorted, because old mail that nobody
  revisits would otherwise look like a success it is not.
  `placement-report` shows backfill outcomes on their own row per area, and
  lists the batches.

### Changed

- **`placement-report`'s backfill estimate** now comes from the same planner
  as `placement-backfill`. It still reads no message headers, so it remains an
  upper bound and says so. Mail that is off the desk is now counted as "off
  the desk" whether or not it is already in its learned area.

## [1.85.0] — 2026-10-16

### Added

- **Mailroom learns where your mail goes.** Once you have moved a
  sender's mail into the same area at least twice, and never anywhere else,
  new mail from that sender is filed there as it arrives, with no rule to
  write. A sender you have not filed yet can follow its domain, but only when
  you have filed two or more addresses there into one area and never filed
  that sender anywhere yourself. That never happens for email providers
  shared by unrelated people (Gmail, Outlook, iCloud and the like) or for your
  own domains: two friends filed into Updates do not send every Gmail user
  there. Your own rules always come first.
- **Only mail that really is from that sender.** A From address can be
  forged, so a message is sorted this way only when its sender is
  authenticated. With your provider's Authentication-Results identity
  configured (`MAILROOM_AUTHSERV_IDS`), it needs a DMARC pass from that
  identity's own header, for exactly the sender's domain. Without it, it needs
  authentication that did not clearly fail, and the server logs a warning at
  startup that it is running that way. A message with more than one From
  header, more than one sender in From, or a group is never sorted by
  learning, and neither is a message the spoofing or look-alike warnings would
  flag. Anything held back is placed the usual way.
- **Authentication-Results are read more strictly everywhere.** Results
  inside comments, or inside another result's details, are no longer counted.
  A provider identity followed by anything other than a version number is no
  longer trusted. This also closes a way for a sender to switch off the
  "claims to be from your own domain" warning.
- **It learns from your corrections.** Move a learned message somewhere else
  and Mailroom stops sorting that sender until your filings agree again. Every
  learned placement is recorded, and an area whose placements you keep
  correcting stops being sorted automatically. The check is strict: with 20
  or more outcomes, it stops once the bottom of the 95% confidence interval on
  "kept" falls below 80%.
- **You can see why a message is where it is.** The area chip in the reading
  pane, and the sender card, now say who put it there, including "learned
  from your moves". Until now every area chip said "automatically", because
  an area's own label stays in Mailroom and had no way to report who applied
  it.
- **An off switch.** Settings → Areas → "Sort new mail into areas by learning
  from your moves". It is on by default, and the server enforces it.
- **`mailroom-admin placement-report --user-id`**: a read-only report for the
  operator. It shows confident senders per area, every placement's outcome,
  precision with its Wilson lower bound, whether each area's gate has stopped
  it, why senders are skipped (including a blocked domain fallback and an
  unauthenticated sender), and what applying this to existing mail *would*
  move, as counts only. `--reset-gate AREA_ID` re-enables an area its gate
  stopped. Existing mail is not changed. That backfill is a later step that
  needs approval.

### Changed

- **Migration 00060** adds the placement log (`app.learned_placements`), the
  gate's reset record (`app.learned_placement_gate_resets`), both with row
  level security, plus the off switch (`desk_preferences.learned_placement_enabled`,
  default on) and a partial index for the evidence query. It only adds; no
  existing row changes.
- ADR-032 amends ADR-023. ADR-023's numbers came from a mailbox without
  history.

## [1.84.3] — 2026-10-16

### Fixed

- **A rule that files mail into an area now moves the mail you already
  have**. Applying such a rule to existing mail used to add the area's
  label and leave most messages where the desk had put them, because the
  desk's own placement in an earlier area (Primary, usually) still won. It now
  takes off the placement Mailroom or day-one setup added, so the messages
  actually move. It never overrides your own filing: mail you put in another
  area yourself stays there, and so does mail another rule filed. Before you
  apply, the rule editor shows how many messages will move, how many are
  already there, and how many will stay put and why. Undo puts every message
  back where it was. New mail matching the rule was already filed correctly,
  and still is.

## [1.84.2] — 2026-10-16

### Fixed

- **Labelled messages no longer show as "not yet confirmed".** After you
  labelled a message, every list except the needs-a-category queue kept
  showing the pending marker: the blue dot in lists, ◌ on the desk. It stayed
  until you reloaded, even though the server had already applied the change.
  A label hides the message from that queue, and that queue-only note was
  wrongly counted as an unconfirmed change. The marker now appears only while
  a change is actually waiting for the server.

## [1.84.1] — 2026-10-16

### Changed

- **Large batches of changes reach your mail server sooner**. Mailroom
  sends your moves, archives and flags to the server in passes of 64. It used
  to send one pass per sync cycle, connecting and logging in again each time.
  When you moved a large batch of messages at once, they took many minutes to
  reach the server. Now, while each pass is full, the next one runs straight
  away on the same connection, for up to 16 passes (1,024 changes) or
  30 seconds before the account's sync takes its turn. Mailroom itself still
  shows every change immediately. Each `writeback pass` log line now records
  its round, how many changes it took, and how long it ran (`took_ms`).

## [1.84.0] — 2026-10-15

### Added

- **Select messages by Shift- or Ctrl-clicking anywhere on a desk row**.
  Shift+click selects a range from the last selected message. Ctrl+click (Cmd+click
  on a Mac) adds or removes one message, so a selection can skip messages in
  between. This works on any part of the row, not only the sender's name, on the
  desk and in the in-tray. A plain click still opens a message, or selects it
  when you click the sender. The keyboard shortcuts sheet lists the new gesture.

## [1.83.2] — 2026-10-15

### Changed

- **Opening an area or a drawer is faster**. To list one area's
  mail, Mailroom used to work out which area every one of your messages
  belongs to, then keep that area's. It now does that only for the
  messages that could belong there: those carrying one of the area's
  labels, or whose tab feeds the area. The list is exactly the same. A test
  compares it, for every area, with the full calculation. On a
  20,000-message test mailbox the work behind an area's list fell to about
  a third of the messages, and the query takes about 0.1 s instead of
  0.2 s. "Needs sorting" still checks every message, because it lists mail
  that no area claims.

## [1.83.1] — 2026-10-15

### Added

- **Statement timing is recorded by the database**. The deploy compose
  file now loads `pg_stat_statements`, with `track_io_timing` on. The
  `role-bootstrap-owner` step creates the extension, so a slow screen can be
  traced to the statement behind it with one read-only query
  (deploy/README.md §6d). It records statement text with the values
  replaced by placeholders, never the values themselves. Utility statements
  are not tracked (`track_utility=off`). Those are stored verbatim, and the
  role bootstrap's `ALTER ROLE … PASSWORD` would otherwise put database
  passwords in the view. On an existing installation Postgres restarts once,
  on the next `docker compose up -d`.

## [1.83.0] — 2026-10-15

### Fixed

- **Mail can be moved between areas**. Each area (Primary, Social,
  Promotions, Updates, Forums, Notes, and any you create) owns a label that
  exists only in Mailroom. Those labels were invisible to the API, so the
  label menu showed them disabled ("local only") and Re-file refused them.
  You could not move mail from Updates to Promotions. They can now be
  applied and removed like any other label, from the label menu, Re-file,
  the `1`–`9` keys, the sender card's "change area", and drag-and-drop. They
  still stay in Mailroom: nothing is written to your mail server, and the
  menu marks them "stays in Mailroom".
- **Re-file moves the message to the area you picked.** Re-file used to add
  only the target area's label. A message keeps the area that sorts first
  among its labels, so re-filing from Promotions to Updates left it in
  Promotions. Re-file now removes the other areas' labels and adds the
  target's. Undo puts back exactly the labels each message had before.

### Added

- **`Email.labelIds`** (urn:mailroom:labels, opt-in): the ids of every label
  on a message, including labels that exist only in Mailroom.
  **`Email/set` accepts `labelIds/<labelId>`**: `true` applies a label, `null`
  or `false` removes it. An unknown id, or another user's, is refused with
  `invalidPatch`. A label that is mirrored to the mail server writes back
  exactly as it does through `keywords/<atom>`. A label that exists only in
  Mailroom writes nothing to the server. ADR-031 records why areas' labels
  stay local-only.

## [1.82.3] — 2026-10-15

### Fixed

- **Drawers and per-section desk queries are always planned for the user who asked**. The query a drawer sends filters by desk area. After five runs on a
  connection, Postgres may switch it to a generic plan that doesn't know whose
  mail it is. With other users' mail in the database, that plan stalled for
  20–60 s instead of about 0.3 s. These queries now force a custom plan, as
  the one-pass desk already did. A test fails if one ever runs on a generic
  plan: with the fix removed, 3 of 6 runs were generic.

### Changed

- **Web dependencies**: React 19.3, Vite 8.3, jsdom 30.1.2 and
  @vitejs/plugin-react 6.1.2.

## [1.82.2] — 2026-10-15

### Fixed

- **Controls are named the way they look** (WCAG 2.5.3 Label in Name). axe-core 4.14,
  pinned here, enforces this rule, and it found seven kinds of control whose
  accessible name did not contain their visible text. A voice-control user
  says what they can see, so the name has to contain it. The fixes are:
  - Desk tabs and attachment thumbnails: their text read as one run-on word
    ("Review first6"); they now have real spaces.
  - Desk row buttons: the name now includes the snippet the button shows.
  - Drawer buttons: the name now starts with the visible word ("Open",
    "Archive all").
  - Classic message rows: named from their own content.
  - The composer's numbered-list button: its "1." glyph is now drawn by CSS.
  Nothing on screen changes. What a screen reader hears changes on the classic
  message rows (the whole row instead of "sender: subject"), desk rows (the
  snippet too) and drawers ("Archive all in …", "Open … drawer").

### CI

- **The end-to-end lanes no longer break each other**. Six browser lanes
  used fixed Docker names: accessibility, hostile content, tabs, inline images,
  proxied images and gate. A post-merge run on `main` overlaps PR runs by
  design, and when two overlapped, one failed with "network … already exists"
  and its cleanup then deleted the network the other run was still using. Each
  run now names its networks, volumes, containers and built image uniquely, and
  removes only its own. The proxied-images lane also picks its own private
  subnet. Two concurrent runs each of the a11y, hostile-content and
  proxied-images lanes passed when started together on one host.

## [1.82.1] — 2026-10-14

### Fixed

- **A writeback pass no longer reports outcomes for intents that no longer
  exist**. Deleting a message removes its queued writeback intents. If
  that happened while a pass was working on one, the pass still counted it as
  applied, retried or dead-lettered. It could log
  `dead_lettered=1` with no dead intent anywhere. Recording an outcome now
  reports whether a row actually changed. One that changed nothing is counted
  in a new `vanished` field of the writeback pass log line, and never in the
  other counts.

## [1.82.0] — 2026-10-14

### Changed

- **The desk loads several times faster**. On a large mailbox a desk load took many seconds, and about nine tenths of that went into working out which area every message
  belongs to, which each of the desk's ten queries did again for the whole mailbox. The desk is
  now worked out in one pass: the areas are resolved once, and every section's first page, its
  count and the area summary all come from that pass. On a 20,000-message test mailbox a desk
  load went from about 1.5–2.5 s to about 0.55 s. What the desk shows is unchanged; a test checks
  the new path against the old queries section by section. Drawers still load as before.

### Added

- **JMAP: `Area/getDesk`**, under the `urn:mailroom:desk` capability, which now advertises
  `getDesk: true`. One call returns the area summary (as `Area/getSummary` does), the Email query
  state, and, for each requested section, its collapsed `total` and a page of `ids`. A section's
  `limit` is clamped to the server's `maxObjectsInGet` and the response says so. Area ids that no
  longer exist come back in `notFound` instead of failing the call. Documented in
  `docs/openapi.yaml`. The web client uses it when the server offers it and falls back to the
  previous requests otherwise.

## [1.81.11] — 2026-10-14

### Fixed

- **Tidying Drafts after sending no longer gives up on a copy that never existed**. If an
  autosaved draft was replaced while its upload to the provider was still in progress, Mailroom
  deleted the saved text before the upload had read it, so the upload failed. The later step that
  removes the old copy from the provider's Drafts then searched for a message that was never
  uploaded, retried five times and gave up, leaving a failure in the log for nothing. The text is
  now kept until the upload has finished, so the copy is uploaded and then removed as intended.
- **Drafts and Sent are tidied on the provider straight after a send**. Filing the sent copy
  in Sent and removing the draft from Drafts waited for the next scheduled sync, which could take a
  few minutes, so another mail client kept showing the draft in the meantime. A completed send now
  starts that work at once. If a sync is already running, it still waits for that sync to finish.

## [1.81.10] — 2026-10-14

### Security

- **Remote images can no longer be fetched from the old 6to4 relay range**. The guard
  that keeps Mailroom from reaching private networks when it fetches a remote image let
  `192.88.99.0/24` through. That range is deprecated (RFC 7526) and nothing legitimate lives
  there on the public internet, but a local network can route it. It is now refused like any
  private address. The image Mailroom ships has no setting that loosens this. The one CI check
  that needs a reachable fake image server builds its own test-only server for it.

### Fixed

- **A send the server refuses no longer looks as if it went**. When the server refused a
  message (for example because it had no recipient it could read), the compose window closed,
  a "sent" notice appeared beside the error, and the autosaved draft was thrown away. The
  window now stays open with what you wrote, only the error is shown, and your draft is kept,
  so you can fix the problem and send again.
- **The test scripts no longer leave a Docker volume behind on every run**. They removed
  their containers without the volumes Postgres and other images create, so each run of the
  integration suite or an end-to-end check left one behind. On the shared dev host this had built
  up to about 25 GB and helped fill its disk. Volumes already left behind are not removed by this
  change.

## [1.81.9] — 2026-10-14

### Fixed

- **Sending to a contact whose name contains a comma failed with "the message has no
  recipients"**. Picking a recipient from the suggestions brings the contact's name
  with it, and names from work directories often read "Last, First". Mailroom wrote such a name
  into the To line without the quotation marks the format requires, so when it read the message
  back to send it, it found no address at all. Names are now quoted or encoded whenever they
  contain a comma or another character that needs it, in To, Cc, Bcc and From alike; ordinary
  names are written exactly as before. A draft saved before this fix keeps its unreadable
  address line, so discard it and write the message again.

## [1.81.8] — 2026-10-14

### Fixed

- **An autosaved draft could reappear in Drafts after sending**. While you write,
  Mailroom saves your draft every few seconds and deletes the previous save from the provider.
  If a sync happened to read the provider's Drafts folder in the moment between those two
  steps, it took the old save for new mail and added it back to Drafts as a separate draft,
  where it stayed until the next hourly check of the folder. The same window could also mark a
  just-sent message as a draft again. Sync now leaves alone any Drafts copy that Mailroom has
  already queued for deletion. If that deletion ever fails, the copy is kept rather than lost.

## [1.81.7] — 2026-10-14

### Fixed

- **An archived message no longer comes back for a few seconds**. Mailroom stopped
  hiding a message you had archived as soon as the server confirmed the move, but the list on
  screen still came from before the archive, so the message reappeared until the next refresh
  arrived, and a refresh already on its way could do the same. A list now keeps a message you
  moved out of it hidden until that list has been fetched again after the move. This applies to
  the desk, its drawers and the ordinary folder lists.
- **Archiving from the reading pane no longer scrolls the desk**. The selection jumped to
  the top of the list for a moment before moving on to the next message, so the list scrolled up
  and then back down, leaving the next message at the bottom of the screen. It now moves straight
  to the next message, and the list stays where you had it. The tray page had the same problem.

### Changed

- **The restore drill records Docker network events while it runs**. Now and then a CI
  drill lost its own network partway through, and nothing showed what removed it. A failing
  iteration now prints the network events seen during the run, and every cleanup logs which
  trap ran it, so the next occurrence names the remover.

## [1.81.6] — 2026-10-14

### Fixed

- **The image proxy's counts are on `/metrics` from the moment the server starts**. Each
  refusal reason, and both delivery results, appeared only after its first occurrence, so a
  proxy with nothing to report looked the same as one that was not counting at all. They now
  start at zero.
- **The accessibility check in CI no longer fails at random on the pane-layout step**.
  The check changed a display setting and then reloaded the page before the server had saved
  it, so the server's old value came back and the step waited in vain. Every settings step
  now waits for the server to confirm the save. The check deliberately delays saves in CI so
  that a step which stops waiting fails on every run, not now and then.

## [1.81.5] — 2026-10-14

### Added

- **The image proxy counts and logs every refusal by reason**. Nobody knew how often
  remote images were refused, or why, so none of the refusals worth fixing could be told from the
  rest.
  - `/metrics` gains `mailroom_proxy_refusals_total{reason}` and
    `mailroom_proxy_fetches_total{result}` (served or cached), so a refusal rate can be read
    per reason.
  - Each refusal logs one line with its reason, for example `reason=upstream_status
    upstream_status=403`. The image's address, its host and the message are never logged,
    because image addresses carry tracking identifiers.
  - What the reader sees is unchanged.

### Fixed

- **The restore drill's self-test again proves the drill notices PGP keys that do not open**. Since 1.80.0 the drill checks a stored check value before it tries to open any key,
  so the injection meant to prove the open step was caught one step earlier, and nothing tested
  the open step. A new injection, `wrong-pgp-root-no-check`, reaches it, and the self-test goes
  red if that step is bypassed.
- **Unfolding an area above the selected message no longer scrolls the desk**. The list
  scrolled to the selected row whenever its position in the list changed, so opening an area
  above it jumped the pane down to the row you had left. It now scrolls only when you move the
  selection, on the desk and the tray page alike.

### Changed

- **`deploy/README.md`: after a deploy that recreated both scanner containers, check that clamd
  loaded the newest signatures**. An update that lands during clamd's 70-second startup
  load is otherwise missed until the next one. The section gives the two commands to compare
  them and the reload.
- **The accessibility check in CI now proves the desk list follows scrolling**. It
  scrolled a list that already fitted on screen, which is why the blank-list bug passed
  it. It now uses a short window and fails unless rows past the first screenful appear.

## [1.81.4] — 2026-10-14

### Fixed

- **Settings → Accounts was empty for an account with no sending address**. The list was
  built only from sending addresses, and an account added as ingest-only has none, so its
  controls, including the writeback switch, could not be reached. Such an account is now listed
  with the note "No sending address yet."
- **`mailroom-admin set-account-credentials --password-file` replaced a separate SMTP login**. It copied the new IMAP login and password over the account's SMTP login, even when
  that was a different login. It now keeps a separate SMTP login unchanged and says so;
  `--smtp-too` writes the new password to SMTP as well. When the two logins are the same, both
  are updated, as before.

### Changed

- **`set-account-credentials` checks an SMTP login against the server before saving it**,
  as it already did for IMAP, including one declared in an accounts file. A login that does not
  authenticate is refused, and nothing is written.

## [1.81.3] — 2026-10-14

### Fixed

- **The desk was slow to load each area, and could briefly show older mail over newer**.
  On a large mailbox a full desk load took many seconds, and nine message
  lists made up most of that. Three changes:
  - Each list is now one database pass instead of two. The server used to count the list's mail
    and then fetch the page as separate passes over the same messages; it now gets both at once.
    This applies to every message list, not only the desk.
  - Desk loads no longer pile up. A change could start a new load while one was running, and on a
    busy evening many overlapping loads piled up, each slower than the last. Now at
    most one load runs, with one more afterwards if anything changed meanwhile, and open drawers
    are reloaded once for the whole burst.
  - An older load's answer can no longer replace a newer one. Answers were applied in the order
    they arrived, so a slow, superseded load could put older lists back on screen. A load for a
    desk you have moved away from (another account or tab) is now cancelled.

## [1.81.2] — 2026-10-14

### Security

- **Alpine's `pcre2` is 10.49 in the image**, fixing the fourth of the vulnerabilities
  1.81.1 set out to fix. The 1.81.1 image still had 10.48: its release build reused an earlier
  build's cached package layer, so the fixed package the Alpine mirror was already serving never
  reached it. Release images are now always built without the build cache, so the operating
  system packages in an image are the ones current when it is released.

## [1.81.1] — 2026-10-14

A security patch. The image is rebuilt on patched components; Mailroom's own behaviour is
unchanged.

### Security

- **Four high-severity vulnerabilities in the 1.81.0 image are fixed**:
  - two in the Go standard library, by building with Go 1.27.2;
  - one in `golang.org/x/net`, now 0.61.0;
  - one in Alpine's `pcre2`. **Not fixed in the published 1.81.1 image**, which still carries
    pcre2 10.48: the release build reused a cached layer. Fixed in 1.81.2.
- **Dependency updates:** the Go modules move to their current minor releases, including
  `ProtonMail/go-crypto` 1.5.2, `pgx` 5.11.0 and `goose` 3.29.0. The ClamAV image digest is
  refreshed. In the web client's build tooling, `source-map-js` moves to 1.2.2 (a development
  dependency, GHSA-68fv-2mgg-jv7q), so `npm audit` passes again.

### Changed

- **ADR-030 records how conversations are threaded**. They are grouped by their
  Message-ID and References headers, with the first match winning. There is no subject
  fallback and no merging of two existing threads, which deviates from spec §4. The tested JWZ
  threader stays in the tree, unused, until a real mailbox shows a measurable gap.
- **CI's linter is golangci-lint 2.14.0.** 2.13.2 could not read packages built by Go 1.27.2,
  and failed every package.
- **The secret scanner's allowlist covers three test-only false positives**: two fake
  passwords used by the admin tests, and a test name in a comment.

## [1.81.0] — 2026-10-03

### Added

- **The server now measures how long each JMAP method takes**. One desk load is a single
  request carrying about thirty method calls, and until now nothing recorded which of them was
  slow. Each call's duration, and each request's total, are now exposed at `/metrics` as
  histograms, and any request slower than one second logs a one-line breakdown by method (counts
  and times only, never arguments or mail). Responses are unchanged.

### Fixed

- **Remote images did not load for a sender you had allowed**. The banner said images
  were loading while each one showed only its description. Mailroom fetches remote images
  through its own server, and the request for each image was being made from inside the
  locked-down frame a message is shown in, where the browser withholds your sign-in. The server
  refused every one. This is confirmed in Chromium-based browsers and in Firefox, on any
  installation that signs in through an identity provider. The app now fetches the images itself
  and hands them to the message, as it already did for images attached to the mail. Your browser still never
  contacts the sender, and tracking pixels are still blocked.
- **The first images could stay broken after "Always allow"**. The images were asked for
  before the server had been told about the click. The app now waits for that.

### Changed

- **The banner says when images could not be loaded**, as "2 of 13 images could not be
  loaded", and no longer says images "are loading" as a standing claim.
- **One message's remote images are limited to 16 MB in total**. Images past that are
  reported as not loaded.
- **ADR-026 and `docs/hostile-content.md` are amended** to describe the new loading path
  and the alternatives that were rejected.

## [1.80.3] — 2026-10-02

### Fixed

- **A long desk or tray list went blank below its first screenful**. The list draws only
  the rows near where you have scrolled to, and it was never told when you scrolled: it redrew
  only when the number of rows changed. Past the first screenful there was empty space where the
  mail and the next areas should have been, most visibly on a phone after unfolding an area. The
  list now follows the scroll position, and also notices a change of density, row layout, text
  size or screen rotation while it is open.

### Changed

- **The web client's tests can now mount a component in a simulated browser**. `jsdom` is
  a new development-only dependency, used by the one test file that asks for it. The bug above
  was in how a component was wired to the page, which the existing tests had no way to see.

## [1.80.2] — 2026-09-27

### Fixed

- **The desk and its area counts were very slow on a large Inbox**. With tens of thousands
  of messages in the Inbox, each desk list took several seconds and the area counts more than
  ten seconds, long enough for a phone to give up on the request. The server was re-checking
  every message one at a time against its folders, its labels and its score; it now works these
  out once for the whole mailbox. In a 20,000-message test mailbox a desk list went from about
  6 s to under 0.2 s (roughly 30 times faster) and the area counts from about 1 s to under 0.1 s.
  What the desk shows and counts is unchanged.

## [1.80.1] — 2026-09-26

### Fixed

- **A folder with a very large backlog stopped syncing**. Sync fetched all of a folder's
  new mail in one request, so a big backlog, for example after moving thousands of messages into
  a folder on the provider, became one enormous download. When it broke off part-way, the provider
  began refusing the whole request on every pass, the folder stopped syncing, and mail moved into
  it could show as deleted until it did. Sync now fetches new mail in batches of at most 500
  messages, and a folder that was only partly fetched resumes where it stopped instead of starting
  again from the beginning.

### Changed

- **The cutover runbook's folder-consolidation section (section 6) records what real runs
  taught**: run a large consolidation only on this version or later, and judge sync
  "settled" by passes with no mailbox errors, not by passes that fetched nothing.

## [1.80.0] — 2026-09-25

The first wave for a mailbox in daily use. Replies go where they should, and Reply all, Send &
archive and every saved signature are there before sending is switched on. Sync passes over a
many-folder account get much shorter, because folders that did not change are no longer asked
about. Every connection to a mail account is now checked, not only the first. There are two new
operator commands, one to flatten a provider's old folder tree and one to rotate a password. And
two bugs that looked like test flakes turned out to be real.

**Migration 00058** (seven nullable columns on `app.mailboxes`, no backfill). Nothing to do: the
first pass after upgrading polls and observes every folder as before, and records what later
passes compare against.

**Migration 00059** (two nullable columns on `app.submissions`, no backfill: `archive_on_success`
for Send & archive and `dismissed_at` for dismissing a failed send). Nothing to do: every existing
submission asked for no archive and has not been dismissed.

### Security

- **Every connection to a mail account is checked, not only the one made when the account is
  added**. Mailroom checked an account's IMAP and SMTP hosts when the account was added, then
  looked the name up again, unchecked, on every sync, IDLE, writeback and send. A host that
  pointed at a public address could later point at this server's own network (DNS rebinding) and
  be handed the account's password. Now each connection looks the host up once, refuses it unless
  every address is public, and connects to the address it checked; TLS is still verified against
  the hostname. `MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS` now covers all of these connections.
  - ⚠ **An account whose mail server is on a private network stops syncing and sending** unless
    that setting is on; the error names it. Public providers are unaffected.
  - An account refused when it was added no longer goes on syncing to the refused host.
  - Addresses are now tried one after another instead of raced, so a host whose first address
    silently drops packets costs up to half the connection timeout before the next is tried.

### Fixed

- **The Outbox was empty after a reload**. The web client only learned about sends it had
  made itself since the page loaded, so reloading emptied the Outbox and its count in the rail: a
  send the mail server had refused disappeared without you doing anything, and a scheduled Send
  Later could no longer be cancelled from the Outbox. The Outbox is now loaded when the app starts,
  and keeps following sends made on other devices.
- **An archived message stayed on the desk for up to an hour**. Archiving took a message
  out of every folder view at once, but the desk and its area counts waited for the next check of
  the Inbox folder on the server, which runs about hourly. The desk now uses the message's new
  folder straight away, as every other view does; a message moved back to the Inbox appears on the
  desk straight away too.
- **A reply went to the sender's From even when the message had a Reply-To**. Mailing lists,
  ticketing systems and much automated mail name a Reply-To; replies now go there, as RFC 5322 says.
  A reply to a message you sent continues it to its recipients rather than to yourself. A reply
  started from the message list, not the reading pane, now waits for the message's full headers,
  because the list's copy of a message does not carry them and the reply would otherwise have
  fallen back to From without a sign.
- **Changing an appearance setting quickly could be undone by the echo of the earlier choice**. Choosing Focused and then Beside could put the pane back to Focused a moment later, when
  the server's answer about the first choice arrived after the second. The same applied to theme,
  density, text size, row layout and the splitter. A preference record the device can show is older
  than its own latest choice no longer overwrites it.
- **A sync asked for while a sync was running was dropped**. With live push there is no
  timer, so a change could sit unapplied until something else changed. The request now runs one
  more sync afterwards. This affected every kind of change, not only settings.
- **`apply-rules` printed the rule's id where a reader took it for the message's**, and its
  "to reverse it" hint sat under this run's count while `--undo` removes every label those rules
  have ever applied. Rows now print both ids, labelled, and the hint says what `--undo`
  really does.

### Added

- **Reply all**, beside Reply in the reading pane and on `a`. It adds the conversation's
  other recipients and leaves out every one of your own addresses, including any address at a
  catch-all domain you own, on every account.
- **Send & archive**, beside Send and on Ctrl/⌘+Shift+Enter, for a reply or forward to a
  message in your Inbox. The server archives the conversation, exactly as `e` would, **only once
  the reply has actually been sent**. A send that fails, is undone or runs out of retries leaves
  the conversation where it was, so the toast says it *will* be archived. If you file the message
  somewhere else before the reply goes, it stays where you put it. It is not offered for a message
  outside the Inbox, which it would otherwise have moved out of Sent or a folder. The
  reading pane leaves the conversation as soon as you send, exactly as Archive does: on to the
  next message (or the previous one), or back to the list. Undo Send does not reopen it.
- **JMAP: `EmailSubmission/query`**, under the submission capability, with the RFC 8621
  filters `undoStatus` and `emailIds` and a Mailroom `inOutbox` filter that selects exactly what
  the Outbox shows. Newest first; it reports its total and the limit it applied, so a client can
  page rather than silently receive part of the list. `EmailSubmission/get` with `ids: null` still
  returns only the 100 most recent submissions.
- **A failed send can be dismissed from the Outbox**. A message the mail server refused stayed in
  the Outbox, and in its count, until it was re-sent. Dismiss takes it off; the draft stays in
  Drafts.
- **Any saved signature can be used**, not only the default one. Insert signature
  replaces the one compose added if it is still untouched, and otherwise adds the chosen one
  without deleting anything you wrote. A signature picked by hand survives changing From.
- **`mailboxes_observed`, `mailboxes_unchanged`, `observations_skipped` and `duration_ms` in the
  sync log**: how many folders the provider answered, how many were skipped as
  provably unchanged, and how long the account's pass took.
- **The restore drill proves the escrowed PGP root even when the backup holds no keys**, by
  comparing it with the check value the database recorded when its crypto process first booted.
  A mismatch fails the drill before anything else is tried.
- **`mailroom-admin set-account-credentials`: rotate an account's IMAP/SMTP password from the
  command line**. Boot deliberately never re-applies a rotated password from the accounts
  file (a redeploy must never re-dial a working account and newly break it), and
  `accounts-report` could only report the drift (`password: DIFFERS`), not fix it; the only
  previous remedy was the web client's Account settings. This command validates the new credential
  against IMAP FIRST, through the exact same check JMAP `Account/set`'s connection change makes,
  and re-seals it only on success — a rejected credential leaves the stored one byte-identical and
  the command exits non-zero. Takes the new password from a file (`--password-file`) or straight
  out of the declared account (`--from-accounts-file --account NAME`, which also preserves a
  separately-declared `smtp_username`/`smtp_password`); never from the command line. A credential
  that already matches what is stored is reported and exits 0 without writing. Appends the same
  Account `change_log` row `Account/set` does, in the same transaction as the credential write.
- **`mailroom-admin consolidate-folders`: flatten a folder tree on the provider**. Moves
  every message in the folders matching `--from` into `--into`, then deletes the folders matching
  `--delete-empty` once they are empty. It is report-first: without `--apply` it prints each
  folder's server count, Mailroom's own count and the plan (move N, delete, or refuse with the
  reason), with totals per category. `--apply` works one folder at a time and checks every step
  on the server: after each move the source must be empty and the destination must hold exactly
  what it held plus what moved, or the run stops and says where. Each folder is re-counted just
  before it is deleted and deleted only when empty. INBOX, special-use folders and folders with
  children that are staying are never touched. A server without MOVE is refused rather than
  copied and expunged. Re-running continues a stopped run. The command writes nothing to
  Mailroom's database. Mailroom's sync picks up the moved mail as new mail, downloads it again and
  merges it with the messages it already has (proved live: 8 messages before and after, none
  lost). On an ingest-only account `--apply` also needs `--despite-ingest-only`, because this
  command bypasses writeback. Take a fresh provider-side backup first. The runbook for connecting a real mailbox
  has the procedure.

### Changed

- **Folders that did not change are skipped**. On a server that supports LIST-STATUS, one
  round trip now returns every folder's message count, next UID and modification sequence. A
  folder whose values match what Mailroom recorded is not opened, and its hourly "which messages
  are still here?" search is skipped when nothing changed since the last time that question was
  answered in full. On a mailbox with hundreds of folders that search was most of each pass's time.
  - It is careful about what "unchanged" means. A folder is re-read if a previous fetch may have
    missed its newest mail, and the search is skipped only against the last *complete* check,
    never the last pass, so mail a fetch silently dropped is still found.
  - A full check is forced at least once a day regardless.
  - Nothing is ever deleted because of a skip.
  - Servers without LIST-STATUS are synced exactly as before.
- **The deployment guide's at-rest check now covers swap**: run without swap, or on an
  encrypted swap device, because decrypted mail in memory can be paged out.

## [1.79.1] — 2026-09-24

### Fixed

- **The signature metrics read 0 for up to a day after every restart**. 1.79.0 fed
  `mailroom_scanner_signature_version` and `mailroom_scanner_signature_published_timestamp_seconds`
  only from the attachment re-scan job. That job is set up at boot only when none is already
  scheduled, and after a restart the existing one still fires at its old time, up to a day later.
  Found straight after deploying 1.79.0: both gauges read 0. A zero timestamp
  deliberately counts as stale, so the recommended alert would have fired after every deploy.
  Mailroom now reads the scanner's version within seconds of starting and every 10 minutes after,
  so a database the scanner reloads also shows within minutes rather than the next day. A failed
  read keeps the last value; the timestamp keeps ageing, so a real outage still alerts.

## [1.79.0] — 2026-09-23

Fixes from the first days of running against a real mailbox. The virus scanner's signature
updater had never worked on a fresh install, so a new deployment scanned mail against the
signatures the image was built with and still reported itself healthy. Mail deleted from a
phone or webmail now disappears from Mailroom's lists the way mail deleted in Mailroom does.
The sync log now states how often Mailroom asks the provider about each folder.

### Fixed

- **The signature updater could not start on a new install**. On an empty signature volume,
  `freshclam` stopped at "Can't create freshclam.dat" and never updated, and the container still
  reported *healthy* because the image keeps it alive with nothing running. Any deployment that
  started from a fresh volume scanned all its mail against the image's own signatures. Such a deployment could run for
  weeks on out-of-date signatures. Existing volumes created before the hardening were
  unaffected, which is why testing never showed it. The service now runs `freshclam` directly as
  the unprivileged `clamav` user, with **no** added capabilities (it used to need four).
- **Signatures were checked once a day, not twelve times**. The image's start script passed
  `--checks=1`, and a command-line flag overrides the config file's `Checks 12`. The cadence is now
  on the command line, where `docker top` shows it.
- **Mail another client moved to Trash no longer appears in All mail, Unread, label views or plain
  search**. Deleted now means one thing everywhere: deleted in Mailroom, *or* in a Trash
  folder, whoever put it there. The same rule applies to the messages a conversation shows, and to
  the mail a rule preview counts (so "N of M" no longer includes mail you have thrown away). A
  message you restore in Mailroom stays visible even while the provider still has it in Trash.
  Mailroom does **not** mark such mail as deleted in its own records, so the Trash retention purge
  never deletes mail that Mailroom itself did not delete.
- **A message could open at the wrong height and never resize**. The message frame reports
  its content height as soon as it loads, and the reading pane attached its listener slightly later,
  so a plain message with nothing to load could report before anyone was listening, and the report
  was lost. The listener is now attached in the same step that creates the frame. The same race in
  the security test suite's harness is what made it fail intermittently since August. It was not
  slowness, so raising the timeout from 20 to 60 seconds never helped. It is now reproduced on
  demand and pinned.
- **A boot warning claimed accounts would not sync when they would.** With
  `MAILROOM_OPERATOR_SUBJECT` unset, the warning said "no accounts will be synced", but accounts
  already in the database sync normally. It now says what is actually skipped: provisioning the
  operator and applying the declared accounts file.
- **The 1.78.0 entry below said SEARCH pacing is "off unless configured". It is on by default, at
  30 per minute.** Corrected in place. An account with many folders therefore paces its folder
  checks without any configuration. On a mailbox with hundreds of folders a sync pass now takes longer, because it waits
  as asked instead of being refused.

### Added

- **Signature freshness is visible from outside the container**.
  - `clamav-signatures` has a health check: it is unhealthy when `freshclam` isn't running or the
    daily database is more than 3 days old. It is a signal only; nothing waits on it, so an outage
    at the signature server never stops mail.
  - `/metrics` has `mailroom_scanner_signature_version` and
    `mailroom_scanner_signature_published_timestamp_seconds`. Alert on
    `time() - mailroom_scanner_signature_published_timestamp_seconds > 259200` (3 days).
  - The metric is a publication timestamp, not an age, because it is read as seldom as once a day and
    an age would stand still between reads. It reads zero, and so alerts, when the scanner reports
    no publication date.
- **`mailboxes_observed` on both sync log lines**: how many folders the provider answered a "which
  messages are here?" question for, one search each. Summed over an hour it is the number to compare
  against `MAILROOM_IMAP_SEARCH_RATE`. `mailboxes_reaped` looks similar but undercounts by about half,
  because a folder with nothing to reconcile is checked but never reaped.

### Changed

- **The ClamAV and Alpine base images move to upstream's latest rebuilds** of the same versions
  (Renovate digest updates). The signature fix above was re-verified on the new ClamAV image, on a
  fresh volume.
- **The runbook for connecting a real mailbox** now covers checking the signature updater at first boot. It
  also records the pass times to expect on a mailbox with hundreds of folders, and warns that the round-trip step
  takes minutes, not seconds, on an account that size.

## [1.78.0] — 2026-09-23

Mail accounts with a lot of folders stop being refused by the provider. Once an hour Mailroom
asks each folder which messages it still holds, so it can spot mail deleted elsewhere and mail a
fetch dropped. On a mailbox with hundreds of folders that asked far more often than the provider
would answer, and was told "no" hundreds of times a pass. It was harmless — nothing was deleted,
and each folder caught up eventually — but it filled the log with noise that would hide a real
failure, and it blocked turning on writing changes back to the provider.

### Fixed

- **A by-hand database snapshot must now be streamed.** v1.77.0 gave the database container a
  private `/tmp`, and `docker cp` cannot read a file from one — so the old two-step snapshot
  (dump into the container, copy it out) now fails at the copy, after reporting the dump itself as
  successful. `deploy/README.md` §0a and the compose file give the one-line streaming form and the
  check that tells a real dump from an empty file. Mailroom's own backups were already streamed and are unaffected.

### Added

- **`MAILROOM_IMAP_SEARCH_RATE` — how often Mailroom may ask "which messages does this folder
  still hold?", per minute, per account.** Defaults to 30/min. Each folder due for its hourly
  check costs one such request, so an account with hundreds of folders can ask for hundreds at
  once; Migadu, for
  example, answers at most 60 per minute *per account* and refuses the rest. The default is half
  that ceiling on purpose: the same allowance also pays for sending your changes back to the
  server, and for your phone and webmail. Set it to `0` to disable pacing entirely, which is the
  right setting for a server with no such limit. Documented in `deploy/.env.example`; the boot log
  now says which mode is in effect.
- **`mailboxes_deferred` on both sync log lines**, plus one `mailboxes deferred to the next pass`
  warning per pass naming the count and the wait the server asked for. It counts folders a pass
  did *not* ask about because the server had already refused; it is deliberately **not** counted
  as an error, because nothing was concluded about those folders and they are due again next pass.

### Changed

- **A refused request now ends the pass's folder reconciliation instead of being ignored.** Once
  the server says "you are asking too often", every further request in that pass would be refused
  too, so Mailroom stops asking. Previously it recorded each refusal as that folder's own failure
  and immediately sent the next request: in practice that meant hundreds of wasted commands on a
  first sync, and hundreds of identical warnings per pass among which a
  genuinely broken folder could not be seen.
- **Folder reconciliation times are spread out.** Each folder now gets a small, fixed offset (up
  to 20% of the interval, derived from the folder itself — no randomness, no new stored state), so
  folders reconciled together in one pass do not all come due again in one pass an hour later.
  Without it the hourly waves re-formed indefinitely: passes still had
  requests refused hours after the initial sync finished.
- **Pacing is ON by default, at 30 per minute** (*corrected in 1.79.0*: this line originally said
  "pacing is off unless configured", which was never true). An account with only a few folders
  never comes near the limit and sees no difference. An account with hundreds of folders paces its
  checks and has longer sync passes; set `MAILROOM_IMAP_SEARCH_RATE=0` for a server with no such
  limit. The refusal is recognised only for the requests that can actually hit the limit, and every
  existing guarantee is untouched: a folder that was not reconciled is not reaped, its cursor is
  not advanced, and it is due again next pass.

## [1.77.0] — 2026-09-20

Two hardening fixes from the first real deployment: backups seal without touching the network, and
they no longer write a plaintext copy of the dump inside a container.

### Changed

- **Backups seal with a pinned image and need no network.** `scripts/backup.sh` used to fetch
  `age` from Alpine's package mirror inside a throwaway container every run — an unpinned version,
  chosen by whatever the mirror served that night, and a dependency on egress being up at backup
  time. The Mailroom image itself now carries `age`, at the same digest a deployment already pins.
  Leave `MAILROOM_AGE_IMAGE` unset and a backup uses that image automatically (derived from the
  running blob container) with no new configuration; set it to pin something else; an unset
  variable and no reachable blob container falls back to the old, unpinned behaviour so the script
  still runs. Either way, the install step now runs only when `age` is actually missing, and says
  which happened. `scripts/restore-drill.sh` carries the same change.
- **The Postgres dump no longer touches a container's writable layer.** `scripts/backup.sh` used
  to run `pg_dump` to a file inside the Postgres container, then copy it out and delete it. It now
  streams the dump straight to a file on the host. A container's writable layer is not always
  where you'd expect it to be encrypted — with Docker's containerd image store (the default on
  fresh Docker 29 installs), it is a containerd snapshot under `/var/lib/containerd`, not under
  Docker's data root — so this closes a path where a backup could leave plaintext mail metadata on
  unencrypted storage without anything failing or logging.

### Fixed

- **`deploy/README.md` §0a corrected:** at-rest encryption needs to cover a container's writable
  layer as well as its named volumes, and with the containerd image store those are not the same
  directory tree. The runbook now gives the check for both. `docs/backup-restore-runbook.md` notes
  the same for `backup.sh`'s own plaintext staging directory.

## [1.76.0] — 2026-09-19

Muting a conversation now does what it says, and a search for deleted mail finds it. Making
that search work turned up two defects in Trash itself, both fixed here.

### Added

- **`is:trashed` finds deleted mail.** The search box accepted `is:trashed` (and `is:trash`,
  `is:deleted`) and always returned nothing, because the query that follows every search removed
  trashed mail again. It now returns what your Trash holds, across every account: mail you
  deleted in Mailroom, mail another app moved to Trash, and anything Mailroom deleted that is
  filed somewhere else. Combine it with other terms (`is:trashed invoice`) as usual. Mail that
  disappeared from the server is not included, as the Trash folder does not list it either.
  Autocomplete now offers it.

### Fixed

- ⚠ **Archiving a message from Trash no longer loses it.** Pressing `e` in the Trash view, or
  dragging a message from Trash onto Archive, filed it there while it stayed deleted. It
  vanished from both folders and was purged when its 30 days ran out. A move out of Trash now
  restores the message, as Restore does, and cancels any purge already queued for it. No message is known to have been caught this way. `is:trashed` finds any message that was.
- **A message opened from Trash shows.** The reading pane loaded only the parts of a conversation
  that were not deleted. Opening a deleted message showed the rest of its conversation instead,
  or the message with no body. It now loads the message you opened, whatever folder it is
  in.

### Changed

- **A new reply in a muted conversation goes straight to Archive.** Until now mute only showed a
  banner: replies still arrived in the inbox and still sent a push notification, although the
  banner said they would not be surfaced. Now a reply that arrives in the inbox of a muted
  conversation is archived on arrival, stays unread so you can still tell it is new, and sends no
  push notification. A sync that brings only muted replies sends no notification at all.
- **Mute works in Mailroom only.** Nothing moves on the mail server, so another mail app still
  shows these replies in its inbox. The banner on a muted conversation now says this. ADR-029
  records why, since the spec asks for writeback where the server can represent the change.
- A reply stays in the inbox if the account has no Archive folder, so muting never hides mail
  where you cannot find it. Your own replies, which arrive in Sent, are not moved.
- **Unmuting** brings the next reply back to the inbox as usual. Replies already archived while
  the conversation was muted stay in Archive.
- Imported mail is not affected: an archive import loads history, and does not count as new
  replies arriving.

## [1.75.0] — 2026-09-19

Fixes for what a line-by-line check of the new wiki against the code turned up: defects, security
gaps, and wording that promised things the code does not do.

### Notes for operators

- ⚠ **Set `MAILROOM_AUTHSERV_IDS`, or the sender-authentication notice now shows nothing.** The
  notice used to believe every `Authentication-Results` header, so a sender could add their own
  `dmarc=pass` and hide a genuine failure. It now believes only headers stamped with the
  authserv-id you name: your mail provider's, the first token of the `Authentication-Results`
  header it adds to delivered mail. Until you set it, the notice stays silent rather than trusting
  headers anyone can write.
- `MAILROOM_RATE_LIMIT_RPS` and `MAILROOM_RATE_LIMIT_BURST` now take effect. They were read and
  checked at startup but never applied, so every instance ran the built-in 50 requests a second,
  burst 100, whatever they said.
- An account whose **SMTP** server resolves to a private address is now refused when it is added,
  as an IMAP server already was. `MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS` covers both.

### Fixed

- **A send that kept failing no longer sits at "Sending…" for ever.** When a message failed with
  errors worth retrying until its retries ran out, the retry job gave up but the message was never
  marked failed, so the Outbox showed it as still sending with nothing left to move it. It is now
  marked failed on its last attempt, and the Outbox shows why.
- **The Forward key works on the desk and in the in-tray.** `f` was bound twice in those views and
  the first binding won, so it toggled the in-tray instead. `f` now forwards everywhere and `s`
  toggles the in-tray. The keyboard sheet now lists Forward and describes the other keys
  accurately.
- `mailroom-admin add-account` and `enable-account` now honour
  `MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS`, as their own error message always told you to.
- **Search suggestions offer only filters that work.** Autocomplete suggested `is:unflagged`,
  `is:snoozed` and `is:pinned`, which the search engine does not know, so choosing one returned no
  results and no error. A test now checks every suggestion against the search engine's vocabulary.
- `mailroom-admin delete-user --apply` now refuses up front if the database cannot delete the user,
  instead of deleting the search index and the stored files first and finding out afterwards.
- Wording that promised things the code does not do:
  - snoozed mail "returns to the Inbox" (it reappears; mail you moved stays put);
  - an area's label is "visible in any other mail client" (it is Mailroom-only);
  - "your operator turns on" the drawer sweep (setup turns it on too);
  - releasing an attachment can be undone (it cannot, from the app);
  - push notifications are set "for your whole login" (they are per device).

### Documentation

- **Backup retention costs `KEEP` full backups, not "one plus deltas"**. Each night's
  backup is a new, separately encrypted file; keeping seven means storing seven. The retention
  self-test now simulates two nights and checks they are two files.
- ADR-004 records what stored file names reveal to someone who can list the disk without the key,
  and why that is accepted.
- Docs and code comments that disagreed with the code are corrected, the OpenAPI description gains
  the two image routes it was missing, and the feature matrix is refreshed from the code.

## [1.74.0] — 2026-09-18

The gaps a review of the first-real-mailbox work found: a login failure that
did not say what was wrong, a tool the runbook needed but no image contained, and runbook text
that had gone stale.

### Added

- **The mailbox census ships in the image.** `mailroom-census`, the read-only tool that
  measures a mailbox before Mailroom touches it, is now built into the release image beside the
  server and the admin CLI, so the cutover runbook can run it wherever the image can be pulled.
  Like every tool in the image except the server, it needs `--entrypoint /mailroom-census`.
  Without that, Docker hands the arguments to the server and starts a second server instead.
- **A way back from a wrong operator subject: `deploy/rebind-operator-subject.sql`**. If
  `MAILROOM_OPERATOR_SUBJECT` was not the subject your identity provider issues, the operator
  was created under the wrong one and your mail synced into a user you cannot sign in as.
  Correcting the variable alone does not fix that. The server then refuses to start, because it
  tries to create a second user with an email the first one still holds. The script moves the
  user that owns the mail to the right subject, keeping everything it owns. If your sign-in had
  already created an empty second user, the script retires that one first, and it refuses to
  touch a second user that owns anything. `deploy/README.md` §4 gives the order to do it in, and
  an integration test runs the script file itself.

### Fixed

- **A sign-in refused because another user already has your email now says so.** Most
  often this is the operator signing in under a different subject from the configured one. The
  sign-in page used to say the account was not the one the instance was configured for, and that
  Mailroom never creates accounts automatically. Neither was true. The server log showed only a
  database constraint error. The page now says your sign-in was verified, that the account could
  not be set up because its email belongs to another account here, and that it has to be fixed on
  the server. The log records the refusal at ERROR with the subject from the sign-in and the
  configured operator subject, never the email. A sign-in that creates a new user while a
  different operator subject is configured is now logged at WARN. That is the other way a wrong
  subject shows up: your mail syncs into one user while you sign in to an empty one. The message
  for other refused sign-ins (a disabled user, or a provider that sent no email) is corrected too.

### Notes for operators

- **Sending as an alias is a permission at the mail provider**. A catchall address receives
  mail at every address, but sending as a derived per-correspondent address is a separate
  permission, and without it every such send is refused at the envelope sender. This is now a
  named prerequisite in `deploy/README.md` §3 and runbook §1.6, with the check that proves it: a
  message sent from an alias that arrives at an outside address.
- **Runbook corrections.** The census steps use the image. The writeback timing is the figure
  measured since then: seconds for a flag or a move, not minutes. An invalid
  boolean setting stops the server at boot (since 1.73.1). Rollback across a schema migration is
  still not rehearsed end to end, and the runbook now says which parts are proven.

## [1.73.6] — 2026-09-18

### Fixed

- **The message list keeps its width while you write, and can be resized there.** With the pane
  beside the list, opening a draft puts the composer where the reading pane was. Reloading the page
  in that state worked out the list's width from the wrong measurement — a share of the list's own
  width instead of the whole area — so it collapsed to the narrowest the list is allowed to be,
  and stayed that way until the next reload without a draft open. It is now measured the same way
  whether a message or a draft is open. And there was no divider to drag while composing, so the
  width could not be put back; there is one now, between the list and the composer (and the
  attachment viewer), and it moves the same divider the reading pane uses. This applies with the
  pane beside the list on a wide enough window; with the pane below the list the composer still
  sits beside a fixed-width list.

## [1.73.5] — 2026-09-18

Resolves the one thing 1.73.4 listed as known and not fixed.

### Fixed

- **Opening a reply or forward and closing it again no longer leaves a draft behind.** A reply or
  forward opens with something in it already — a "Re:" or "Fwd:" subject and the quoted message —
  so it counted as worth saving the moment it appeared, and was saved as a draft within a few
  seconds whether or not you wrote a word. Every reply you opened and thought better of stayed in
  Drafts. A composer is now saved only once you have changed something in it: typed, added or
  removed a recipient or an attachment, chosen a different From address, or inserted a template.
  A note typed into a forward before choosing who it goes to is still kept.

## [1.73.4] — 2026-09-17

A correction to 1.73.3, which said a reload would reopen your draft and had never been seen doing
it. The check that would have caught that now exists — a browser reloading straight onto a
composer — and on its first runs it found two more things nobody had looked at, because every
earlier check opened a blank composer by keyboard on a page that was already running.

### Known, not fixed here

- Opening a reply or forward and typing nothing still saves it as a draft within a few seconds. The 1.73.2 note that "a composer you have not typed in still saves nothing" is true only
  of a blank composer. How to fix it is a design choice, recorded on the issue.

### Fixed

- **Reloading the page while writing really does reopen your draft now.** 1.73.3 fixed which draft
  the address named, and said a reload would reopen it. It did not: on a reload the composer asks
  for the draft before the app has connected to the server, the request fails, and it never asked
  again — so it sat on "Opening…" for ever, and it always had, whatever the address said. That
  also held the rest of the screen in the composer's layout, with the message list squeezed narrow
  and no way out except Escape, which nothing mentions. The composer now waits until it is
  connected, asks again if a request fails, and the "Opening…" line has a Close button, so a wait
  that does not end can always be left.

- **A screen reader now hears a forward's attachments as a list.** Each attachment tile in the
  composer was marked as a group in a way that stopped it counting as an item of the list it sat
  in, so the list was announced with nothing in it. It had gone unnoticed because the accessibility
  checks only ever looked at a blank composer, which has no attachments; the new check that
  reopens a forwarded message by reloading found it on its first run.

### Changed

- **Quoted text in the composer is drawn in the normal text colour, not the faded one.** It is
  something you read, not a label, and the faded tone fell short of the stronger contrast level
  this project holds reading text to. The indent and the line beside it still mark it as quoted.

## [1.73.3] — 2026-09-17

All from two minutes of typing into one draft. A draft is edited by replacing it — every autosave
writes a new version and removes the old — and nearly everything that held on to a draft had been
written as if its identity never changed: the list's highlight, the composer's address, the rule
that decides whether the old copy on the mail server is really gone. None of these was visible in
a test. Each was visible within seconds of actually using the thing.

### Fixed

- **While you type into a draft, the list's selection stays on the draft.** Every autosave replaces
  a draft with a new version — that is how mail drafts work underneath — and the list briefly lost
  the old one before the new one arrived. In that gap the highlight moved to the message below, and
  stayed there. With nothing ticked, the list's keyboard shortcuts act on the highlighted message,
  so an archive or delete pressed after a save went to a message you never chose, while you were
  looking at the draft and not at the list. The list now knows which draft each save replaced, and
  the highlight, and anything you had ticked, follows the draft to its new version.

- **A draft you are writing no longer flickers in the list, or shows as unread.** The draft's row
  vanished and came back on every save, fifteen seconds apart, with a "Loading…" line flashing
  underneath the list each time. The old version now leaves in the same moment the new one arrives,
  and refreshing a list that is already on screen no longer says it is loading. Every saved version
  was also born unread — including the one you were typing into — which is why drafts counted
  toward unread; every version you save is now marked read, here and on the mail server. (Drafts
  saved before this release keep their old state until they are next saved.)

- **A replaced draft's old copy is really removed from the mail server.** Removing a message is two
  steps: mark it deleted, then ask the server to clear what is marked. Occasionally the server did the first and not the second — and Mailroom recorded the removal as
  done because nothing had gone wrong, rather than because the message was gone. The copy stayed on
  the server, marked deleted, and the next sync brought it back as a second draft of the same
  message. When the server reports that it removed nothing, Mailroom now asks whether the message
  is still there, and tries again if it is. The same check applies everywhere Mailroom removes or
  moves a message, not only to drafts.

- **Reloading the page while writing reopens your draft.** The composer's address named the draft it
  was opened on, and every autosave replaces that draft with a new one — so after the first save
  the address pointed at nothing, and a reload, or Back then Forward, showed a composer stuck on
  "Opening…" forever over text that was safe in Drafts all along. The address now moves to each new
  version as it is saved, without disturbing the composer you are typing in; and a composer opened
  on a draft that genuinely no longer exists says so, rather than waiting for it.

- **The Outbox names a recipient's address too, not just their display name.** The reading pane was
  fixed for this and the Outbox row was not — on the one screen whose job is telling you what
  happened to a message you sent, where "who is this going to" is the whole question.

## [1.73.2] — 2026-09-17

Everything here was found by using the thing. Seven fixes in one afternoon of real composing and
sending, and not one of them was visible to a test, a grep or a log line — which is the part worth
recording. The Outbox had never once been reachable at the moment it mattered: a refused send was
the single event that made its entry disappear, so the feature built to report a refusal could not
be opened when one happened. The list behind that entry had been right the whole time.

The rest are of a piece. A draft resumed from a derived alias quietly reverted to the main mailbox
address and sent from it. A forward's covering note, written before choosing a recipient, was
discarded on close without a word. A replaced draft left its old copy on the mail server, which the
next sync adopted as a new one. And a refused message named the pattern `*@your.domain` rather than
the address the server had just rejected, directly above the sentence telling you to change it.

### Fixed

- **A draft is kept even before you have chosen who to send it to.** Forwarding opens with an empty
  To field — a reply can borrow the recipients from the message it answers, a forward has nobody to
  borrow from — so the natural order is to write the covering note first and pick the recipient
  second. Everything written in that state was thrown away when the composer closed: not saved on
  the timer, not saved when the body lost focus, and not saved by the one last save on the way out
  that exists so that a few seconds of typing never goes missing. The footer said "Not saved yet",
  which sounds like a delay rather than a refusal. Drafts without recipients are now kept, and the
  rule that a message needs somebody to go to has moved to the only place it is actually true:
  sending. A composer you have not typed in still saves nothing.

- **Replacing a draft no longer leaves a copy of the old version on the mail server.** Every
  autosave writes a new version and removes the previous one — but "the previous one" was only
  removed if a sync had already noticed it arriving, and noticing takes a good deal longer than
  the fifteen seconds between saves. A version replaced in the meantime was left behind on the
  server with nothing arranged to remove it, and the next sync, finding a message it did not
  recognise sitting in Drafts, brought it back as a new draft. The symptom was two drafts of the
  same message where there should have been one, and it got likelier the faster you typed.

- **A refused send no longer hides the Outbox from itself.** The Outbox appears in the rail when it
  has something in it — and the count that decided this had its own copy of the rule, which counted
  only sends still on their way. A refusal is not on its way, so it counted nothing, so the Outbox
  vanished at the exact moment a send was refused: the one event it exists to report was the one
  that removed the only way into it. The list behind the door had been right all along and said so
  in a comment. There is now one rule, in one place, that both the list and the count use.

- **A refused message says which address the server rejected.** It named the catch-all pattern
  `*@your.domain` rather than the address actually used — printed directly above "Change what the
  server objected to", for a refusal whose reason was the sender address. On a domain with dozens
  of aliases in use, that identified nothing. It now names the address the message was sent from,
  which the server had been reporting all along.

- **A refused message can be opened from the Outbox.** The row said "It is still in Drafts. Change
  what the server objected to before sending again" and then offered no way to do it — failure was
  the only state with no action, so the interface named a problem and left you to go and find the
  message yourself.

- **Continuing a draft keeps the address it was written from.** A draft composed from a derived
  alias on a catch-all domain reverted to the main mailbox address when reopened, silently, and
  then sent from it — which is the opposite of what the alias is for: replies are supposed to come
  back to the alias, and the main address is the one the scheme exists to keep out of circulation.
  Found in delivered mail: two drafts written from two different aliases, both re-sent after a
  refusal, both arriving from the main mailbox.

- **A recipient's address is readable again.** The reading pane showed a correspondent's display
  name and nothing else, with no way to see the address behind it — so there was no way to confirm
  where a message had gone, and two different people with the same display name looked identical,
  which on a reply-all is the difference between answering the thread and answering a stranger.

## [1.73.1] — 2026-09-17

A patch release about statements that read as true and were not. A kill switch a typo turned off
without saying so. A route table that said `/metrics` gives away no user id, when it gives away
every user's id and the moment they were created. A deployment requirement that is real, is stated
in the spec, and was written down nowhere an operator would look — with no way to check it. And an
Outbox that had a message's recipients in hand and said it had none.

Nothing here is a new capability. Several of these are the difference between a guard and the
appearance of one, which is the harder failure to notice, because the appearance is what you go
looking for when you check.

### Changed

- **The deployment guide now opens with the two things the stack cannot do for itself.** A new
  §0 states at-rest volume encryption and external TLS termination as host *prerequisites* rather
  than footnotes, and gives a check for each. Both are invisible when they are wrong: nothing
  fails, nothing logs, and an instance with its search index on a bare disk looks exactly like one
  without. The encryption note names the volumes and why — `search` stores the subject and the
  first 256 KiB of every message's body verbatim so it can highlight them, and `pgdata` holds
  subjects, a body extract and every correspondent address as plain columns — and it records the
  two ways a check of this gets written wrongly: a `/dev/mapper/` path test calls a plain LVM host
  encrypted, and reading only the top device calls LVM-on-LUKS unencrypted. The discriminator is
  the `dm/uuid` prefix, walked down `slaves/` recursively. The TLS note adds the one instruction
  that was missing: `/metrics` must not be publicly reachable, verified with a negative control
  from a second machine rather than a successful HTTPS load.

- **ADR-028 now says what a scheduled send actually does when it lands in ingest-only mode.** That
  such a send fails permanently rather than waiting for the mode to lift was already recorded; what
  it looked like from the outside was not. It fails with a sentence written for a person, returns
  the daily quota it had reserved, leaves the draft alone — and, since the Outbox fix in v1.73.0,
  it is visible rather than silent, which is the half that had changed since the decision was
  written.

### Corrected

- **`/metrics` does expose user ids.** The deployment guide's route table said it *"exposes no user
  id, subject or address"*. No mail, subject or address, correct — but
  `mailroom_sync_disabled_accounts{user="…"}` and `mailroom_sync_lag_seconds{account="…"}` carry
  ULIDs, and a ULID's leading bits are its creation time. An anonymous scrape therefore yields how
  many users and accounts exist, when each was created, how far behind sync is, how much writeback
  is owed and how much each account has sent today. The endpoint is unchanged; the sentence
  describing it was wrong, and it was the sentence someone would read when deciding whether the
  endpoint needed protecting.

### Fixed

- **A mistyped safety switch no longer starts the server with the safety off.**
  `MAILROOM_WRITEBACK_DISABLED` is the instance-wide lock that stops a shadowed instance writing to
  somebody's real mailbox, and an operator sets it precisely because they do not trust the
  per-account setting. Any spelling outside `1`/`true`/`yes`/`on` was read as "off" — measured:
  `enabled`, `disabled`, `ture` and `y` each started the server with writeback live. Nothing said
  so, in either direction: the log line that announces the lock only appears when the flag reads as
  *on*, and the interface reports the *effective* setting, so it correctly said "live" and looked
  like it was working properly. The server now refuses to start on a boolean value it cannot read,
  naming the variable, and the same goes for the other five `MAILROOM_*` toggles. Accepted
  spellings are `1`/`true`/`yes`/`on` and `0`/`false`/`no`/`off`, any case, surrounding space
  ignored; unset and empty are unchanged, which is what a Compose file passes for a toggle nobody
  set.

- **The Outbox names who a message is going to, even when you sent it from a draft you already had
  open.** A message waiting to send showed its subject but said "no recipients" — not while it
  loaded, but permanently. Recipient lists are only fetched when a message's contents are needed,
  and a draft you had already looked at was considered "in hand" on the strength of what the list
  view had loaded, which does not include them. So the one fetch that would have asked for the
  recipients was skipped precisely for the messages most likely to be in the Outbox: the ones you
  opened before sending. Sending something composed fresh was unaffected, which is why it went
  unnoticed.

## [1.73.0] — 2026-09-16

The wave about mail that quietly did not happen. Three separate things could go wrong without
saying so: a message restored from Trash could still be destroyed on the server by a deletion
queued minutes earlier, a draft signed while an account was in ingest-only mode could lose the
bytes it still needed, and a send the mail server refused simply vanished — recorded, reasoned
about, and shown to nobody. The first two were found by reading; the third was found because a
real forward disappeared and its author went looking in the provider's webmail.

Underneath all three is the same missing fact: Mailroom recorded what it wanted to do to the mail
server and never recorded *what asked for it*, so it could not tell a person's decision from its
own machinery, and could not re-check a decision that had gone stale.

### Fixed

- **A message the mail server refuses no longer disappears silently.** If a send was rejected
  outright — a sender address the provider will not accept, a recipient it will not take — Mailroom
  recorded the refusal and the reason, and then showed you nothing. The message stayed in Drafts,
  which reads as "I never sent it" rather than "it was refused", so the natural next move was to
  send it again unchanged and have it refused again, just as quietly. Worse, this always happened
  *after* the Undo countdown had run out, so the last thing the interface said about the message was
  that it was on its way. A refused send now appears in the Outbox with the mail server's own words
  — that text is what tells you whether the problem was the address you sent from, the person you
  sent to, or the connection. It goes away by itself once you fix the message and send it again;
  there is nothing to dismiss. Sends that fail for passing reasons are unaffected: those are retried
  automatically and never appear here. Messages actually being sent right now are now shown too,
  where before they briefly vanished.

- **Restoring a message from Trash no longer destroys it on the mail server.** Once a message's
  30-day Trash window closed, the purge sweep queued an instruction to delete the server's copy —
  and nothing re-examined that decision afterwards. Restore the message in the minutes before that
  instruction ran and it was carried out anyway: Mailroom kept the message, the mail server did
  not. The decision is now re-checked at the moment it would be carried out, and an instruction for
  a message that has left Trash is withdrawn instead. Restoring inside Mailroom also withdraws it
  immediately, in the same step that takes the message out of Trash. The second check is the one
  that matters, because a message can leave Trash without Mailroom being involved at all — another
  client moves it, and Mailroom only finds out at the next sync.

- **A draft signed while an account is in ingest-only mode no longer loses the bytes it still
  needs.** Signing a message at send time replaces its contents, and the old contents are discarded
  once nothing needs them any more. The check for "nothing needs these any more" knew about two
  kinds of pending work and not about the third, which only exists in ingest-only mode — so during
  a shadow run it discarded bytes that a recorded-but-not-yet-sent action was still going to use.
  Replaying that ledger afterwards then reported one failure per signed message, on precisely the
  signal the run is meant to be judged by.

### Changed

- **Mailroom now records what asked for each pending server action** — you, a rule, the desk, the
  draft machinery, or the Trash purge. Nothing recorded it before, so a label you applied by hand
  and one a rule applied at ingest were indistinguishable once queued. The ingest-only ledger's
  report gains a breakdown by source, because the number an operator decides on was dominated by
  automation: in practice most queued actions are the keyword operation that
  ingest-time rules produce in bulk. Actions queued before this release are shown as **unknown**
  rather than being attributed to anyone — the record simply was not kept, and guessing would be
  the kind of confident falsehood this column exists to end.

## [1.72.0] — 2026-09-16

The wave between being ready for a real inbox and actually pointing at one. The cutover
itself gets a runbook whose rollback was walked rather than reasoned about, and the two
gaps that only show up when a person *uses* the client — a message without an attachment
could not be forwarded at all, and every action sat in a queue until the next poll
happened to come around. Plus a CI retry helper that could see a fetch fail but not a
fetch hang, which is the failure it kept meeting.

### Added

- **You can forward a message again — from the message itself, not only from an attachment.**
  Forwarding has been fully built since the composer was rewritten: it quotes the original, marks
  the subject `Fwd:` and carries the attachments. But the only control anywhere that opened it was
  the **attachment viewer's** Forward button, so a message with an attachment could be forwarded
  only by opening the attachment first, and a message without one could not be forwarded at all.
  Forward now sits in the reading pane's More menu, and `f` does it from the keyboard, next to `r`
  for reply. It acts on one message, because a forward carries one message's body and files.

- **A runbook for connecting a real mailbox** (`docs/production-cutover-runbook.md`): the order to do things
  in when pointing an instance at a primary mailbox, what to check at each step, and how
  to back out. It separates what has actually been exercised from what is read off the code, and
  says so line by line. The rollback path was walked through rather than reasoned
  about, which turned up three things worth knowing: the
  image tag must be pinned or a rollback silently runs the build you were leaving; rolling back to
  an older release recreates the credentials volume that was deliberately removed; and a job kind
  the older build does not recognise retries rather than failing, recovering when you roll forward.
  It also records the one fact most likely to be assumed wrongly: actions taken while an account is
  in ingest-only mode are recorded but never sent to the provider, even after writeback is switched
  on.

### Fixed

- **An action you take now reaches the provider in seconds, instead of waiting for the next
  poll.** Writeback only ever ran as a sync pass's pre-sync hook, so the queue had no way to be
  woken: archiving a message or hitting "Not junk" wrote the intent locally and then sat there
  until the account's next scheduled pass happened to come around. With a
  5-minute poll interval the wait from click to the change landing on the provider was minutes,
  and because the wait is uniform, half of all actions took longer than the average. Committing an
  `Email/set` now signals the runner to drain that user's writeback queue immediately. The drain
  goes through the same per-account gate every sync pass uses, so a nudge can never open a second
  IMAP session beside a pass already running, and twenty actions in a row collapse into one drain
  rather than twenty. It drains only — a nudge does not trigger a sync, which on a large mailbox
  is the expensive half. If the runner is busy, unreachable, or absent, nothing is lost: the
  intents are durable and the scheduled pass still drains them, exactly as before.

- **A CI network fetch that hangs now fails and retries, instead of stalling a required check
  until its 60-minute timeout.** The shared `retry` helper re-runs a command when it
  *exits* non-zero — but a stalled fetch never exits, so the helper was inert in exactly the case
  it looked busiest for. Observed: the restore drill sat inside `apk add` for over 18 minutes, its
  log byte-identical across three readings, without printing a single retry line. Each attempt is
  now bounded (`RETRY_TIMEOUT`, 180 s by default, raisable for a call that needs longer), which
  turns a stall into an ordinary non-zero exit the existing loop already handles. The give-up
  message reports whether the attempt timed out or failed, and it accepts both exit codes,
  **measured** rather than assumed: busybox exits 143, GNU coreutils 124. The first draft of the
  fix read `$?` after `fi` — where it is the status of the `if` statement, not of the condition —
  so every message said `rc=0` and the timeout branch was dead code that read correctly; the probe
  caught it, review would not have.

## [1.71.0] — 2026-09-15

The production-readiness wave: everything that does not need the real
mailbox in hand. Measurement before cutover (the census), the writes that a real inbox
makes unavoidable ("Not junk", the alias disclosure line), the costs that only appear on a
large mailbox (flags-only sync, change-log compaction, memory ceilings), and the
deployment surface an operator actually has to touch (password files, the env passthroughs,
container naming, the access gate).

### Added

- **Compose now says when the address you are sending from is new to someone on the message.**
  On a catchall domain each correspondent gets their own address, which is what makes a leak
  traceable — and it also means the address reveals how the others are built. Forward a thread,
  or add someone to a reply, and a line names the people who have not seen that address before.
  It stays quiet on an ordinary reply, because the person being replied to is the one who used
  that address in the first place, and a warning on every reply is one nobody reads. It is a line
  under the message, never a dialog, and it does not try to guess whether a correspondent is a
  company or a person: it only reports who is already on the thread.

- **A message wrongly filed as spam can now be rescued from inside the web client in one action.**
  Selecting mail in Junk, or opening a message that is in Junk from any list, offers **Not junk**
  where Archive normally sits. It moves the message to the Inbox, both in Mailroom and on the mail
  provider, which is the signal a spam filter learns from, and a notice says where it went. On an
  account in ingest-only mode the provider-side move is held back, like every other write. The
  server has accepted this for some time, but the client never offered it, so rescuing a false
  positive meant opening another mail client. The control looks at where a message is filed, not
  only at a spam flag, because some providers file spam into the Junk folder
  without flagging it. A control that looked for the flag would never have appeared. If the server
  refuses the rescue, the message goes back into Junk rather than disappearing from every list.
  There is deliberately no undo, because the reverse would be marking mail as spam, which Mailroom
  leaves to the provider.

- **What the declared-accounts file actually does at boot is now written down, and a new
  command shows where it and the database disagree.** The file was always reconciled
  additively — a name already in the database is left completely alone, and only identities
  and wildcard domains keep getting added on every restart — but that was easy to miss while
  editing it, and nothing said so where an operator would read it before acting. Editing an
  existing account's password, host, port or ingest-only setting in the file and redeploying
  looked like it should take effect and silently did nothing at all.

  `mailroom-admin accounts-report` now compares a given accounts file against the database and
  reports, account by account: a password that was rotated in the file but never applied (only
  ever as "matches" or "DIFFERS" — the value itself is never printed, either way, and neither is
  any other credential), an endpoint or setting that differs for the same reason, an account
  still waiting on a declared identity, and one left with sync disabled after a rejected
  onboarding. A new flag can fail a deploy check on any of that. Nothing it finds is repaired
  automatically — repairing some of it has no automated path today at all, which the report
  says plainly rather than leaving to be discovered.

- **A mailbox can now be measured before it is ever synced.** A new standalone command,
  `mailroom-census`, connects to an IMAP account and reports what is actually in it — mailbox
  names and sizes, message and byte counts, how much carries attachments, a year-by-year
  breakdown, which of Sent/Drafts/Trash/Junk/Archive the server actually declares, and how
  much mail looks like it might be filed in more than one folder — without changing anything.
  It exists so a cutover decision, or a "why does this account look different than expected"
  question, can start from a real number instead of a guess. It is read-only in two
  independent ways at once: the code it runs has no method that could change anything on the
  server at all, and every mailbox it opens is opened in the mode that tells the server itself
  to refuse any change — and the test that guards this was checked by removing both and
  watching it fail. It reports capabilities twice, as the server advertised them and as
  Mailroom's sync will actually conclude, because those can differ. It can read the account
  straight from the same accounts file the server boots from, so no password is copied
  anywhere, and it only ever connects over TLS. It can also probe how many more simultaneous
  connections the account will accept; the answer is stated as a lower bound, since clients
  already connected are not counted. It is an operator tool built from source, not part of the
  image. With `-messages` it instead lists each message's size by mailbox and UID — and nothing
  else about it — so that when a mailbox's total disagrees with what Mailroom stored, the
  difference can be traced to the exact messages rather than argued about from totals, which
  can agree while dozens of individual messages do not.

- **Backups can now keep more than one copy, and something can notice when they stop.** The
  backup itself was already sound — one encrypted file holding everything needed to rebuild,
  useless without the key kept separately offline. What was missing was everything around it.
  It kept exactly one copy, so a backup taken while the database was already damaged
  overwrote the last good one, and damage is usually noticed after that. It can now keep as
  many recent copies as you have room for, older ones dropping off the end automatically, and
  the copies share storage so keeping a week of them costs little more than keeping one.

  And there is now a check that goes red when backups stop happening. That sounds obvious, but
  the previous state was that a backup which quietly stopped running looked exactly like one
  that was working. The check deliberately looks at more than the timestamp: a backup that was
  cut off half-way through leaves a file that *looks* current, so it also confirms the file is
  a real, complete-looking encrypted artifact before calling it healthy. What it does not claim
  is that the backup would successfully restore — only actually restoring one proves that, which
  is what the separate weekly drill is for.

  Worth stating plainly: nothing in the software schedules a backup, and on the test machine no
  backup had ever been taken. Scheduling belongs to the machine's own configuration rather than
  the application, and that part is tracked separately — but the pieces it needs now exist,
  and both are tested on every change rather than merely believed.

- **The record of what changed in your mail no longer grows forever.** Every change — a flag
  flip, a label, a move — is kept so any client, and the search index, can ask "what changed
  since I last checked" and get back exactly that, nothing more and nothing missed. That record
  had no end to it: it grew by one entry per change, permanently, for as long as an account
  existed. A weekly housekeeping pass now trims entries older than 90 days (configurable, and
  refused below a week so a typo cannot make it too aggressive) once they are no longer needed to
  answer anyone's question. A client — or the search index — that has genuinely been offline
  longer than that window is told plainly to refetch instead of being handed a partial answer,
  and does so automatically, the one time it needs to. Nothing about your mail itself changes;
  only how long the bookkeeping behind it is kept. A new `mailroom-admin compact-changelog`
  command previews what a run would remove before anything is deleted.

- **Every setting the server reads can now actually be set from the deployment's `.env`.** Roughly
  twenty tunables — sync backoff and IDLE/pool behaviour, the remote-image proxy, Trash retention
  and the purge sweep, change-log retention, the malware scanner's size limit, the sender-
  authentication allowlist, search indexing, ingest-only mode, the private-account-host guard,
  Undo Send, and both API rate limits — existed in code with sane defaults but were reachable from
  neither `deploy/compose.yml` nor `deploy/.env.example`, so an operator who wanted to change one
  had no way to, short of editing the compose file directly. `MAILROOM_AUTHSERV_IDS` was worse: it
  was already read by the running server, but `.env` had nowhere to put it, so it silently had no
  effect however it was set. All of them are now passed through and documented, with the
  destructive ones — Trash retention, the purge cadence, change-log retention — called out
  explicitly rather than left to be discovered at the 30-day mark. A new test parses this
  package's own source to find every setting it defines and fails, by name, if a future one is
  added without a matching `deploy/` entry, so this gap cannot reopen silently the way it opened
  the first time.

### Changed

- **Every container in the deploy stack now has a memory ceiling**, sized from measurement
  rather than a guess (cgroup `memory.peak` read off each service on a running instance) and
  overridable per deployment with its own environment variable, so a runaway process is capped
  rather than free to starve its neighbours. The attachment scanner's ceiling is the one that
  needed real thought: its steady-state footprint is roughly half its measured peak, because a
  signature reload briefly holds two copies of the database in memory, so the limit allows
  headroom for that reload rather than only for scanning.

- **The base and runtime images are now pinned to exact digests**, not just tags, so a rebuild
  of this image or a fresh deploy on a new host gets exactly the bytes that were built and
  tested, rather than whatever the tag happens to resolve to on the day someone runs `docker
  build` or `docker compose up`. Every tag stays next to its digest, so the line is still
  readable and an upstream move still shows up as a normal update. Both database and scanner
  tags had in fact already been moved upstream since a running instance pulled them, so each
  is pinned to what that instance is actually running: deploying this changes nothing, and
  the upstream rebuilds arrive as ordinary update proposals to review.

### Fixed

- **Dragging mail onto the Inbox moved it to Archive instead, unless it came from Trash.** Every
  drop on the Inbox was sent as "restore from Trash", and the server files a restore with no
  destination into Archive. So dragging a false positive out of Junk onto the Inbox, the most
  natural rescue there is, quietly archived it, and so did dragging from a folder, a label or a
  search. A drop on the Inbox now does what it looks like: mail from Junk gets **Not junk**, mail
  from anywhere else goes back to the Inbox, and dragging out of Trash keeps its existing behaviour.

- **A narrow race could have handed a client, or the search index, an incomplete answer to
  "what changed since I last checked" with no error at all**, if the weekly history housekeeping
  (above) happened to run at the exact wrong moment — between the instant a request checked how
  far back it could still ask, and the instant it actually read the answer. Neither half had ever
  run before this change existed, so nothing has been affected by it yet, but the read is now one
  single database step instead of two, closing the window entirely rather than narrowing it.

- **A change another mail client makes to a message's flags — read, starred,
  answered — no longer re-downloads the whole message**, and neither does one
  Mailroom itself sends to the server, which the server reports back the same
  way. A bulk relabel could re-download a large number of
  messages before this. Mailroom's incremental
  sync already asked the server which messages had changed since the last
  check, but for any answer it re-fetched the full message anyway, even when
  nothing but a flag had moved: the same bytes came down the wire again, were
  parsed again, and were re-scanned for malware again, for no reason a client
  could ever see. It now asks for exactly what changed — the flags — and
  nothing else, unless the message turns out to be one Mailroom does not
  actually hold yet, in which case it still fetches the full thing as before.
  New counters record how many message bodies, and how many bytes, each sync
  pass actually downloads, so a mailbox that is mostly flag traffic (read
  receipts, another device catching up) is now visibly cheap instead of
  invisibly expensive.

- **Adding a mail account could be used to map the network around Mailroom.** When you add an
  account, Mailroom connects to the mail server you named to check the details work, and told
  you what happened. It would connect to *anything* you named — including addresses on the
  machine itself or elsewhere on your private network — and the error it reported back differed
  depending on whether something was listening. That turns a settings form into a way of
  discovering what else is running nearby, which matters much more on a machine sharing a
  network with other services than on a laptop.

  Mailroom now refuses to connect to an account server on a private, loopback or link-local
  address, and refuses *before* connecting, since the connection itself is the thing that
  answers the question. The refusal says plainly that it is a policy decision rather than
  imitating a connection failure.

  **If your mail server genuinely is on your own network**, which is a perfectly reasonable
  setup, set `MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS=1` — the refusal message tells you so rather
  than leaving you to guess. It widens only account checking; fetching images out of messages
  and sending web-push notifications keep the strict rule, because there the address comes from
  somebody else's mail rather than from you.

- **The restore rehearsal had never been run against a real backup, and could not be.** Every
  week Mailroom proves it can restore — but always from a backup it makes of a world it invents
  for the purpose, which tests the machinery rather than your mail. Pointing it at a real backup
  for the first time turned up two things that only a real machine can show. It copied the
  project's entire version-control history into its workspace, which it never reads, and a
  single unreadable file anywhere in that copy silently destroyed the whole transfer — so the
  rehearsal stopped dead with no message at all, after appearing to succeed at every earlier
  step. And it insisted every account contain mail, which is right for the mailbox you are
  trying to recover but wrong for a real system, where an account someone signed into once and
  never used is perfectly normal. Both fixed; the rehearsal can now be aimed at a specific
  mailbox.

  It then ran, on a real backup of a real mailbox, and passed: opening the
  encrypted archive, restoring the database, decrypting a message with the separately-held key,
  rebuilding the search index from scratch, running a search that found it, and
  confirming the second key could still open the stored PGP keys. Those timings are now recorded,
  because during an actual incident the question is "how long until mail is back" and nobody
  had ever measured it.

- **The disaster-recovery instructions named a file that is never created, on the secret that
  matters most.** Rebuilding from scratch means decrypting two escrowed secrets: the key that
  opens your mail, and the one that opens your stored PGP keys. The written procedure created
  the second under one name and told you to open it under another, so following it exactly
  worked for the first secret and then failed with "no such file" on the second — at the worst
  possible moment, after appearing to work. Found by actually performing the procedure rather
  than re-reading it. The instructions now also explain how to recover the public half of the
  recovery key if you did not write it down, and spell out what to do if that key is simply
  **lost** rather than stolen — a case they previously did not cover at all, and which is worse,
  because every backup already made becomes permanently unopenable and nothing on the machine
  can detect it. Your nightly backup keeps succeeding and the freshness check stays green, since
  a machine deliberately cannot open its own backups.

- **A failed backup could destroy the previous one.** The finished archive was copied directly
  onto the file the previous backup lived in, which means the old file was emptied before the
  new one arrived. Anything interrupting that copy — a full disk, a restart, a cancelled run —
  left a truncated file where the good backup used to be, and on a large mailbox that copy takes
  long enough for it to matter. The new archive is now written alongside and moved into place
  only once it is complete, so at every moment the backup on disk is either the previous
  complete one or the new complete one.

- **Database passwords were being written into a connection string on a shared volume.**
  Standing up the deployment used to glue a role's password directly into a
  `postgres://user:pass@host/db` string and write that whole string, plaintext, onto a Docker
  volume every database-touching process then read from — a second permanent copy of every DB
  credential beyond the secret it came from, and one built by unescaped string concatenation, so
  a password containing `@`, `:` or `/` produced a connection string that parsed wrong. The
  connection URL now carries no password at all; each process reads one separately, in memory,
  from the same secret file used to set that role's password in the first place, right before
  connecting. The one-shot setup steps that create the database roles no longer need to run as
  root, either — they only ever needed root to write onto that now-deleted volume. Passwords
  containing `@`, `/`, `:`, `%`, `'`, `#`, `?` or a space now work correctly, both at role
  creation and at every later connection.

- **The portability check's URL scan could not see a container image reference or a registry
  login, because neither has a URL scheme.** A workflow can name a registry host in a plain
  `image:` value or a bare `docker login <host>` argument, and both slipped past the existing
  check without a scheme prefix for it to find — exactly how an operator-hosted registry name
  once passed a local run undetected. The check now also reads `image:` keys and `image=` output
  lines in YAML, `FROM`/`COPY --from=` in Dockerfiles, the arguments of
  `docker login|push|pull|tag|run|build`/`buildx build --tag`, and `scripts/*.env` image
  defaults — including the default inside a `${VAR:-default}` expansion — and judges the host it
  finds by the same reserved/allowlist rule as the URL scan, reporting how many references it
  examined so an empty result cannot be mistaken for a clean one. A local run now also says
  plainly that everything outside those sites — prose, a Go module path, any other scheme-less
  string — is covered only by the out-of-band operator denylist, which a local run skips unless
  that secret is exported.

- **The deployment guide and the backup runbook named containers two different ways, and
  following both in sequence hit "no such container".** The deploy guide's plain `up -d` and the
  runbook's worked backup example implicitly assumed different Compose project names, so the
  container names one document showed were never the ones the other produced. Every documented
  command that can go through Compose now names a **service**, which works under any project
  name; where a raw container name is still genuinely required (`backup.sh` takes one directly),
  both documents state the same rule once — container names are `<project>-<service>-1`, the
  default project is `mailroom-deploy`, and `docker compose ... ps` shows the real names for
  whatever project you actually used — instead of each hard-coding an assumption about the other.
  The deployment guide also no longer gates "ready for primary mail" on two closed items; it
  points at the project's production-readiness checklist instead.

- **Three documents still described a single-subject login allowlist that OIDC multi-user support removed.** The top-level README, the example `.env`, and the compose file's own
  comment all said only one configured subject could log in and that Mailroom never provisions on
  login — neither has been true since app-binding became the access gate: any identity your
  provider lets reach this application is auto-provisioned, with a group claim selecting the admin
  role on every login. All three now describe that gate, briefly, and point at the deployment
  guide (which already had it right) for the details, including the real risk of the setting they
  used to call an allowlist: get the operator subject wrong and it silently provisions a different
  user who owns the declared mail accounts, not the one you meant to log in as.

- **The restore drill, a required check, failed whenever the runner's DNS blinked.** One drill
  self-test run fetches packages roughly fifty times — `apk add` and the search-engine library
  download, each in a fresh container — and none of those fetches retried, so a single transient
  resolver error anywhere failed the whole check and blocked merging. It happened twice in one
  afternoon, on two different failure injections. Every fetch in the drill, the one in
  `scripts/backup.sh`, and the two behind the required search-engine test
  (`scripts/tantivy-test.sh`) now retry up to five times over at most 30 seconds. A resolver that stays
  down still fails the run with the package manager's own error, so a real outage remains visible
  rather than being waited out, and a scheduled backup no longer dies of a momentary DNS failure
  either.

## [1.70.0] — 2026-09-12

### Added

- **An account can now be watched without ever being written to.** Turn on "ingest only" and
  Mailroom syncs the mailbox, indexes it, classifies it and lets you browse it, while being
  structurally unable to change anything on the server — not "configured not to", but never
  even opening a connection that could. It exists so a mailbox you care about can be run
  alongside your existing setup for a while and judged on real mail before anything is
  entrusted to it.

  **What is and is not kept while it is on**, because the honest answer is not uniform and
  you should not have to find out by losing something. Read/unread and stars are *provisional*:
  they mirror the server, so if you read a message on your phone, Mailroom will follow, and a
  read/unread change you make only in Mailroom is eventually replaced by whatever the server
  says. Labels, filing and trash are *yours* and stay put — which is the part that matters,
  because it means you can sort and label real mail and keep the results. Sending is turned
  off entirely for such an account: an instance you have not decided to trust yet should not
  be emitting mail to other people.

  Every change Mailroom would have made to the server is recorded rather than discarded, so at
  the end you can see exactly what it wanted to do — broken down by kind, not as one number.
  Turning the mode off does **not** then apply that backlog; nothing is sent to the server
  unless you explicitly ask for it in a separate step. A fortnight of filing arriving in one
  burst because a switch was flipped is precisely the surprise this is meant to avoid.

  Mail you delete while the mode is on is never emptied for good behind your back. Normally a
  deleted message is destroyed after its time in Trash — but while Mailroom is only watching,
  the message never actually reached the server's Trash, so emptying it would mean deleting
  something out of your inbox that the server never saw you delete. Mailroom will not do that:
  a message whose deletion has not been sent to the server stays put, however long ago you
  deleted it, and that remains true after you turn the mode off. It is emptied only once the
  deletion has actually been sent and the message is really in Trash.

  The practical consequence is worth knowing: if you decide to abandon a shadow run's filing
  rather than send it, the messages you deleted during it will sit in Mailroom's Trash without
  emptying. Sending the deletions, or restoring and re-deleting them once Mailroom is live, is
  what clears them.

  There is also a deployment-level switch that disables writing for the whole instance, which
  the settings screen cannot override — so a machine set up in this mode stays that way until
  someone changes its configuration, not until someone clicks the wrong thing.

- **Releases now publish a container image, instead of each machine building its own.** Tagging
  a release builds the image once and pushes it to the project's own registry. Until now every
  deployment compiled its own copy from source, which meant the software that was tested and
  the software that ran were, strictly speaking, different builds — and "go back to the previous
  version" rebuilt from scratch rather than restoring the thing that was working. The image is
  only published for a tag that came from reviewed, tested code, and it is checked before
  publication that it reports the version it claims to be.

### Fixed

- **Emptying the Trash could destroy a copy of the same message sitting in a folder you never
  deleted from.** A message can exist in more than one place in one account — mail the server
  delivered twice, or a copy filed in two folders — and when Mailroom finally destroys a
  message whose time in Trash is up, it removes every copy it knows about. One copy being in
  Trash was enough to license that, so the copy still sitting in your inbox or an archive
  folder went with it.

  Mailroom now destroys a message only when **every** copy it can see in that account is
  actually in that account's Trash. If one is still somewhere else, nothing is destroyed and
  the message waits in Trash until the deletion has genuinely reached the server for all of
  them. The cost of being wrong in this direction is a message that lingers rather than one
  that is gone, and the wait is now reported rather than silent: Mailroom's own logs
  distinguish "waiting because this account is only being watched" from "waiting because a
  deletion never reached the server", where both previously counted as the first — the one
  reason an operator would expect to see in bulk, and therefore the one that hid the other.

- **A rebuild given the wrong PGP secret could look perfectly healthy for months.** The
  service that signs mail keeps its own secret, separate from the one that protects your
  mailbox, and it only used that secret when somebody actually signed or decrypted something.
  So a rebuild paired with the wrong copy of it — or with a freshly made one — started,
  reported healthy, and carried on normally until the first signature failed, by which point
  every stored key was unopenable. The signing service now records in the database which
  secret it was set up with, and refuses to start with a different one. Mail keeps flowing
  either way; only signing waits, and its log says exactly what is wrong.

  Two limits, stated now rather than discovered later. A database that has never run this
  version has nothing to compare against, so its first start adopts whatever secret it is
  given. And the check proves "the same secret as before", not "a secret that opens the
  stored keys" — the restore drill still proves that.

- **One of the automated tests could fail on a fast machine for a reason that had nothing to do
  with what it tests.** Several test setups gave each simulated message its position in a
  mailbox from the clock, to the millisecond — so two messages created within the same
  millisecond got the same position, and the setup refused the second one. It showed up as a
  test failing before it had checked anything. The setups now draw positions from a counter.
  Mailroom itself is unchanged.

- **Deleting a draft could destroy a copy of that message sitting in another folder.** When you
  discard a draft, Mailroom removes it from the mail server outright — no Trash, no undo, which
  is right for a draft you are throwing away. But it removed *every* copy of that message
  anywhere in the account, not just the one in Drafts. That mattered because "is a draft" is
  something the mail server tells Mailroom, not only something Mailroom decides: a message
  filed in Archive years ago, flagged as a draft by whatever program put it there, counted. It
  now only ever removes the copy in Drafts. A copy anywhere else is left alone, which may mean
  a leftover to tidy up by hand — the safer of the two mistakes.

  This closes the gap before a mailbox with drafts spread over several folders is connected.

- **A change you made could stop on its way to the mail server, without saying so.** When you
  flag a message, move it, or delete one, Mailroom records what you did and then mirrors it to
  the mail server a moment later. If Mailroom was stopped or restarted during that moment —
  a deploy, a reboot, the machine losing power — that one change was picked up, never finished,
  and then never looked at again. Nothing was lost locally and nothing was done twice, but the
  server never heard about it, and nothing anywhere said so. Mailroom now notices work that was
  started and abandoned, and finishes it.

  In practice no change had been seen stranded before this fix. So this is
  insurance against a failure that had not yet been observed, not a repair of one that had —
  and it matters more once Mailroom is looking after a mailbox you cannot afford to have
  quietly drift out of step.

- **Background work that gives up now says so where monitoring can see it.** Mailroom retries
  a failed background task — emptying Trash, training the label model, sending a scheduled
  message — and eventually gives up on one that keeps failing. That giving-up was written to
  the log and reported to a counter that was never connected to anything, so the number an
  alert would watch sat at zero forever and read as "nothing has gone wrong". It is now
  connected, and it records which kind of task gave up.

- **Monitoring can now see a backlog of background work building up.** Mailroom reported the
  size of its background-work queue as zero, always, whatever was actually waiting — so a
  dashboard watching it showed a flat healthy line by construction. It now reports the real
  figure: work that is due and has not run yet, across the whole instance. Work scheduled for
  the future — a message snoozed until next week — is deliberately not counted, because
  otherwise the number would climb during ordinary use and mean nothing when it mattered.

- **The rest of Mailroom's monitoring signals now carry real numbers.** Most of what Mailroom
  offered a monitoring system was reported as zero no matter what was happening: how far
  behind each account's mail sync had fallen, how many of your actions were still waiting to
  reach the mail server, how many of those had failed, how much of today's sending allowance
  was used, and how far behind the search index was. All five now report what is actually
  happening. One further signal — a placeholder for a machine-learning model version — was
  removed rather than filled in, because the only numbers available to it did not mean what
  its labels claimed, and a plausible wrong number is worse than an absent one.

  A test now fails the build if a signal is added without saying where it gets its value,
  so this cannot quietly happen again.

## [1.69.0] — 2026-09-09

**Reading comfort**. Five text sizes with today's in the middle, applied to the whole app;
and a choice of row layout everywhere mail is listed — the single line redrawn so the subject is
never sacrificed to the snippet, and a new multi-line layout with sender and time, the subject,
and the first line of the message. Both are Appearance settings, synced per client family, and
both are audited by the accessibility lane.

### Added

- **Five text sizes, and the whole app scales with them.** Settings › Appearance
  gains a five-step control — Smallest, Small, Default, Large, Largest — that sets the
  root font size, and every length in the client that exists to fit text is now expressed
  relative to it: type, row heights, row padding, button and toolbar minimums, the
  composer's surfaces. Default is the size the app has always drawn at, so nothing changes
  for anyone who never opens the control. The control is a slider for the keyboard and a
  screen reader (its value is announced as the word, not "3 of 5") beside the five words as
  buttons for the pointer, and the page is its own preview — the words are drawn at the
  size they set. Synced per client family, so a phone and a browser can differ.
- **A row layout choice: one line, or three.** The single line is redrawn so the
  subject stops losing to the snippet — the sender column holds a fixed number of
  characters rather than a fixed number of pixels, the subject takes exactly the width its
  own text needs, and the snippet takes only what is left, separated by an en dash. The new
  multi-line layout gives each message three lines: sender with the marks and the time,
  then the subject, then the preview. Both apply everywhere mail is listed — the desk, its
  drawers, the in-tray strip, the tray page, All mail, Sent, folders, labels, search and the
  needs-a-category queue — and both are synced per client family. The two layouts share one
  DOM, one set of roles and one accessible name per row; only the CSS moves. The
  accessibility lane audits the desk in the multi-line layout and at the largest text size
  as surfaces of their own, in all three themes, so that claim is checked rather than
  asserted.

### Fixed

- **The subject sat about eight pixels above the sender on every desk row.** The
  row's middle cell aligned its contents to their shared text baseline inside a
  fixed-height box, which lines the subject up with the snippet and then puts both near the
  top — while the sender and the time, in cells that centre, sat in the middle. On a line
  whose whole job is to read as one line. Found by photographing a row rather than by
  reading the stylesheet, during a rendering check.

### Changed

- **A narrow message list drops the snippet a little sooner at large text sizes.**
  The threshold at which the snippet gives way to the subject — when a reading pane
  narrows the list — is now measured in text units rather than pixels, so it moves with the
  chosen size. At the default size it is exactly where it was.

## [1.68.1] — 2026-09-09

### Fixed

- **Nothing appears on hover over a row, on any surface now.** The older message list —
  All mail, Sent, folders, labels, search, the needs-a-category queue — still revealed a
  per-row toolbar (pin, snooze, important, read, archive, trash) on hover and on focus,
  which the desk's design rules out. The toolbar is gone;
  every one of its verbs is on the selection toolbar instead, resolving to the selected
  rows exactly as the desk's does, and the single-row keyboard shortcuts are unchanged.

## [1.68.0] — 2026-09-09

**Day one, accessibility, React 19**. A
folder-sorted mailbox becomes a desk without moving anything by accident: a census, a preview,
a chunked import that labels and archives mail in Mailroom while the bytes stay where the
provider has them, an undo journal, rules learned from folders, and a four-step wizard that
prints only numbers the server measured. The desk is a grid a screen reader can drive, every row
named in words, and CI proves it in a real browser — axe over twelve surfaces in three themes,
Chromium's accessibility tree over the desk's core loop, four negative controls that must trip.
`Mailbox.accountId` tells folders apart across accounts. TypeScript 7 and React 19 underneath.
Built and dogfooded as two pre-releases — `1.68.0-rc.1` and `1.68.0-rc.2` below carry the
detail, including the migration backfill that rc.1 shipped inert and rc.2 re-applied.

## [1.68.0-rc.2] — 2026-09-09

Second pre-release of "Day one, accessibility, React 19": the accessibility proof lane in CI
(axe and Chromium's accessibility tree over the desk, with negative controls), React 19, and the
corrected migration for the day-one backfill that `rc.1` shipped inert. Everything the
milestone names is now on main; `1.68.0` follows once this has been seen running.

### Added

- **The accessibility model is now checked by a browser instead of promised by a
  document.** A new required CI lane (`scripts/a11y-e2e.sh`) stands up the
  real image with a real Dovecot mailbox — thirteen messages arranged so the desk
  has two populated areas, a drawer with a count, an unread row, a read row, a
  flagged row, an attachment and a two-message thread, and which *fails* if the
  mailbox did not produce that — and then drives the shipped client in a
  digest-pinned headless Chromium. Two instruments, because one of them cannot
  see most of what `web/ACCESSIBILITY.md` claims: **axe-core** over twelve
  surfaces — including the day-one setup wizard at `#/setup`, added after the
  fixture user's first CI run rebased onto the wizard and the lane's own
  `#desk-grid` wait timed out behind it — in three themes (light, dark, and
  "system" with the OS asking for dark), where WCAG 2 A/AA violations fail the
  build and every run prints its `passes` denominator so an empty result cannot
  read as a clean one; and
  **Chromium's own accessibility tree**, read after each step of the desk's core
  loop — open, move, archive and reach the undo toast with `g u`, flag, cross the
  line, the line's slider, a drawer, and a fault replacing the header sentence —
  asserting the computed role, name, value and states of the active row and the
  text of the live region, and printing the sentence a reader would be given.
  `color-contrast-enhanced` (AAA) is measured on the same surfaces and gated
  against `scripts/a11y-contrast.allow`, in which every line carries the worst
  ratio that element was actually measured at, so an exception cannot silently
  become a regression.
  **Four negative controls run in the same job and each must go red**: a nameless
  button, a glyph button losing its label, the header sentence losing
  `role`/`aria-live`, and the grid losing `aria-activedescendant`. A control that
  stays green fails the run. The lane was itself probed by deleting
  `aria-rowindex` and rebuilding: all thirty-three axe passes stayed green and the
  lane went red — which is the argument for the second instrument, measured rather
  than asserted.
  **What it deliberately does not prove, in its own words:** no screen reader runs
  (no runner here is Windows, so NVDA is unavailable), so nothing in it is evidence
  about an AT's phrasing, its verbosity, or whether a polite region is spoken
  exactly once; `aria-current` is read from the DOM because Chromium exposes no
  computed property for it; the message body is not linted, because it is a
  stranger's HTML in a sandboxed opaque-origin frame that `hostile-content-e2e.sh`
  governs; the attachment viewer's *held* panel and the phone form are outside it,
  and both are named on the lane's final line rather than quietly skipped.

### Changed

- **React 19.2.8** (TypeScript 7 landed separately, above).
  `react` and `react-dom` move 18.3.1 → 19.2.8, `@types/react` → 19.2.18,
  `@types/react-dom` → 19.2.7 (the latest 19.2.x releases at the time). Two
  contributor-visible effects: `@types/react` no longer declares a global `JSX`
  namespace, so every `): JSX.Element` return annotation now needs
  `import type { JSX } from "react";` — added uniformly across all 71 files
  that use the annotation, the annotation text itself untouched; and
  `useRef<T>(null)` now types as `RefObject<T | null>` rather than
  `RefObject<T>`, which widened the one prop that crossed a component boundary
  with the narrower type (`SearchBox`'s `inputRef`). No other React-19-removed
  API (`ReactDOM.findDOMNode`, `unmountComponentAtNode`, `react-dom/test-utils`
  `act`, legacy context, `useFormState`) was in use. Checked against the
  upgrade guide and confirmed not applicable: no component renders `<title>` or
  `<meta>` (nothing to auto-hoist), no `useId` value is parsed, and the app
  installs no global `window` error handler for render errors that React 19's
  `onUncaughtError`/`onCaughtError` root options would need to replace.

### Fixed

- **A muted label on a selected row was below the contrast floor in dark.**
  The rail's unread count for the mailbox you are in, and the attachment viewer's
  caption for the thumbnail you are on, drew muted text on the selected-row
  background at 4.21:1 — under WCAG AA. The token check could not have caught it:
  it verifies the pairs the design *declares*, and this pair existed only because
  two rules met in the cascade. The pair is declared now, so it is a red build in
  both themes, and the dark selected background moved to clear 5.00:1. Found by
  the new accessibility lane on its first dark-theme pass.
- **`g u` no longer loses the reader's place.** Dismissing the undo toast
  with Escape removed the element that had focus, so focus fell to the document
  body: every subsequent keystroke did nothing, with nothing on screen to say why
  — in the one key added for people using a screen reader. Focus now goes back to
  wherever `g u` took it from, or to the message list, on every way out of the
  toast including one it does not control (a newer offer replacing it).
- **Two migration backfills that had silently done nothing in a deployment now
  actually run.** Migrations apply as `mailroom_owner`, which — like any role
  without `BYPASSRLS` — is subject to `FORCE ROW LEVEL SECURITY` on every user table, and
  `mailroom_owner` holds no policy of its own. Under FORCE that meant `00050`'s
  `UPDATE app.desk_preferences SET setup_completed_at = updated_at;` and `00047`'s
  `UPDATE app.messages SET flagged_at = received_at WHERE is_flagged AND flagged_at IS
  NULL;` both matched zero rows, silently: no error, goose printed `OK`. On the instance
  every established desk still read `setup_completed_at IS NULL`, which is exactly the
  state the day-one wizard treats as "never set up" — it would have opened for a login
  that already had a desk arranged. Migration `00051` re-applies both backfills
  idempotently, wrapped in a transient `NO FORCE`/`FORCE` toggle so the owner can actually
  see the rows it is writing, and a new static test parses every migration file to catch
  the same shape of defect before it ships again.

## [1.68.0-rc.1] — 2026-09-09
First pre-release of "Day one, accessibility, React 19": the day-one setup — server and the four-step wizard — over a desk that a screen
reader can drive, with `Mailbox.accountId` and TypeScript 7 underneath. Still to come before
`1.68.0`: the accessibility proof lane in CI, and React 19.
### Added

- **Mailbox carries `accountId`.** Folder names collide across accounts —
  every account has an INBOX, a Sent, an Archive — so folders can now be told
  apart by which account owns them instead of guessed at by name. Settings →
  Accounts → "Folders synced" filters the folder list to the account the row is
  for, and the rail's Folders disclosure groups folders under each account's
  name once a login has more than one.
- **Day-one setup, server half: a folder-sorted mailbox becomes a desk without moving
  anything by accident.** Four steps, each a report before a write. **CENSUS** —
  every folder day-one can act on, with the numbers a decision needs: live mail, how much
  arrived inside the window and how much is older, how many messages carry no From address
  at all (counted, never hidden), how concentrated its senders are, and a shape word
  (`oneDomain`/`fewSenders`/`bulk`/`mixed`/`empty`) that is a word for the counts beside it
  rather than an authority. Special-use folders are mapped without asking and are not rows;
  INBOX is a row, because its backlog is usually the largest single pile. **PREVIEW** — for
  a plan of per-folder choices, exactly what applying it would write, per folder as well as
  in total, so "this plan does nothing because the window covers everything" cannot be
  confused with "this plan does nothing because everything is already labelled".
  **APPLY** — a chunked background job that LABELS mail older than the window (default 30
  days; the window itself is untouched by design) and ARCHIVES IT IN MAILROOM: the message
  leaves the desk while its bytes stay exactly where the provider has them. **NOTHING MOVES
  ON THE SERVER** — no IMAP write is queued and no other client sees anything change —
  except a folder explicitly marked `bringToDesk`, which is a real move to the inbox with
  the writeback a person's own move would make. **UNDO** — the whole run, from a
  per-message journal, reversing only what the run itself did: a label the user has
  re-applied by hand since survives, a message that was already archived stays archived,
  and a moved message goes back to the folder it came out of.
- **Day-one setup, client half: the four-step wizard.** A login that has never
  been set up opens on it once, and every step shows a report before it does anything.
  **STEP 1** lists what is in the mailbox — each folder with its live count, a shape
  phrase that always carries the numbers it was derived from, the area day-one proposes
  (an existing one of the same name, or a new one) and the label, which is the folder's
  own word and is editable — then the one big decision: whether the folders FEED the
  desk, or whether new mail should arrive in the inbox and Mailroom should sort it.
  **STEP 2** is the mail already there: a window (7 / 14 / 30 / 90 days) whose counts
  are re-asked from the server rather than re-derived, a three-way override per folder,
  and a sentence saying exactly what the plan would write — including "move 0 messages",
  because nothing moving on the server is the reassurance the step exists to give. The
  import then reports planned beside actual, per verb, and can be undone whole. **STEP
  3** proposes rules learned from the folders, each showing what it would have done to
  the mail that is already there, with the bound of the scan printed when the scan was
  bounded. **STEP 4** sets the line, the drawer period and the tray nag, and closes with
  what the desk will actually open with — every number measured, and any clause whose
  number is unknown left out rather than printed as zero. On a mailbox with NO plain
  folders — a provider that does no sorting — the wizard says so in as many words
  ("Found N messages in INBOX and no other folders"), and hides the table and the
  decision, because there is nothing there to feed the desk. Settings → Areas and
  drawers can import one folder later, or run the whole thing again; and an All-mail row
  that day-one filed away says "archived in Mailroom", which is not the Archive folder
  and never was.
- **The desk agrees, in one place, that archived mail is off the desk.** The single
  SQL definition of "on the desk" gained the archive mark, so the area counts, the
  `onDesk` filter and — the one that matters — the drawer auto-archive sweep all honour it.
  The sweep can therefore never server-move mail day-one filed away, which is what keeps
  "nothing moves on the server" true a month after the setup ran rather than only on the
  day.
- **`Rule/induce` can learn from a folder, not only from a label.** Folder
  membership is the provider's own sorting, and that is evidence of what a person wants in
  the same way a hand-applied label is — and on the day a mailbox arrives from another
  client it is the only evidence there is, which is the day a rule is worth the most.
- **`mailroom-admin day-one`.** Prints the census and then the preview of the plan,
  per folder and with the totals, and writes nothing: `--apply` creates the run and drives
  its chunks inline, `--undo` reverses one, `--runs` lists them, and `--plan FILE` takes a
  one-line-per-folder plan whose refused lines are reported rather than skipped. The
  summary line always says "nothing moves on the server", including when the number of
  moves is zero.
- **JMAP: `Setup/census`, `Setup/preview`, `Setup/apply`, `Setup/get`, `Setup/changes` and
  `Setup/undo`**, all under the desk capability; `deskArchivedAt` on Email and a
  `deskArchived` filter on `Email/query`, so a client can show what day-one filed away;
  and `setupCompletedAt` on DeskPreferences, which is null for a login that has not been
  through day-one and is how a client knows to open the wizard rather than the desk.
- Migration `00050`: the archive mark on messages, `setupCompletedAt` (backfilled inline
  for every existing login, so no established desk is interrupted by a wizard), and the
  setup run and its undo journal.

### Changed

- **TypeScript 7.0.2** (the React 18→19 half of that Renovate
  PR is a separate change so a failure has a single cause; React, react-dom, and
  `@types/react*` stay at 18 here). Contributor-visible effects: side-effect
  stylesheet imports (`import "./foo.css"`) now need an ambient declaration, added
  once in `web/src/css.d.ts` (`declare module "*.css";`), because TS 7 checks them
  where 5.9 did not. No `tsconfig.json` options were removed — TS 7 reported no
  deprecated/no-op options for this config — and no other real type defects
  surfaced.
- **The message list is a grid you can actually drive from the keyboard, and every
  row says what it is.** The desk's list and the in-tray page's list are one
  tab stop now: `↑`/`↓` move between messages (`j`/`k` still work from anywhere),
  Home/End go to the ends, PageUp/PageDown move a screenful, Enter or `o` opens,
  Space selects and Shift+Space extends the range. Each row announces itself as one
  sentence instead of two buttons — unread first, then who it is from, the subject,
  the area, the account, the alias it arrived at, the time, and then its marks in
  words: in the tray, pinned, came back from a snooze, has an attachment, an
  attachment is held, a sender warning, how many messages are in the conversation,
  waiting to send. The message the reading pane is showing says so rather than only
  showing an accent bar, and the account is a word rather than only a coloured dot.
  The two halves are a proper tab strip (`←`/`→`, Home/End, one tab stop); the line
  is a slider that reads out the split it would produce — "6 to review, 37 in
  drawers; the line at 0.65 would move 2 to the drawers" — with `↑`/`→` for
  stricter, Enter to apply and Escape to cancel; a drawer's Open button is called
  "Promotions drawer, 24 messages, auto-archives after 30 days". `g u` moves focus
  to an undo offer (which pauses its countdown) for anyone who wants to hear what
  happened before undoing it. Opening a message with the reading pane switched off
  now puts focus in the message and gives it back to the list on the way out, and
  the reading pane is a named landmark.
  **`Tab` no longer switches desk halves.** It was swallowed while focus was in the
  list, which trapped the keyboard there in both directions and meant Tab could
  never reach the controls inside the list at all — a section's fold, a drawer's
  Open, the line's two words. The keys sheet lists the replacements.

## [1.67.0] — 2026-09-09

**Settings, compose, outbox, viewer**. Settings is
one searchable page of twelve state-first rows with the scope printed on each, down to a
single account seen from the inside and the areas and drawers editor; compose happens where
reading happens, with From as a sentence, recipient chips that carry the key fact, and a
send line that never silently downgrades; the Outbox exists only while something is queued;
attachments open in a viewer that explains a held or flagged file instead of showing it.
Built and dogfooded as three pre-releases — `1.67.0-rc.1` through `-rc.3` below carry the
detail — and finished by the entries in this section.

### Added

- **An attachment opens in the main area now, not in a floating box.** Clicking a
  file in the reading pane opens a viewer that takes the reading pane's own column, with
  the list still beside it — the whole content area where the reading pane is switched
  off, and the whole screen on a phone. No dimmed overlay over the app. The top bar names
  the file, its size, an image's dimensions once it has loaded and who sent it, with
  **Download**, **Forward**, **Open in a tab** and **Close** in words. Along the bottom is
  every attachment on the message — its kind (`jpg`, `pdf`, `zip`) and its name — with
  `1 of 3`, ← / →, and **Fit** / **100%** for an image or **Fit width** for a PDF. Arrow
  keys page through it, Escape closes it and puts you back on the file you came from.
  Office and ODF documents, and anything else nothing can render, get a panel saying
  `No preview for this type yet` with a Download in it rather than an empty frame. There
  is still no find-in-file and no text view for PDFs: both need the text the server
  extracts for search, which it does not yet hand to the client.
- **A held file opens to an explanation, not to itself.** A file the malware scanner is
  withholding shows why instead of a blank pane, in two weights that are deliberately not
  the same sentence. *Could not be scanned* — usually a password-protected archive or PDF
  — says so plainly, says that nothing has checked it rather than that anything is wrong
  with it, and leads with **Release and open**. *Flagged by the scanner* says that the
  scanner believes the file is harmful and that releasing it is a real risk to the device
  you open it on; **Keep held** is the emphasised answer there, and releasing it anyway
  asks once more before it happens. Both say the thing that is easy to miss: a release
  covers every copy of that same file in your mailbox, now and later. Held files are
  marked in the strip along the bottom too, so you know before you land on one.
- **`released by you · 12 Aug 2026`.** A file you released keeps saying so, with the date,
  both in the viewer and on the tile in the reading pane — a released file stops being
  withheld, so without the mark the only trace of the decision would be the absence of a
  warning.

- **A client can say, before you press send, whether the message will be signed.**
  Each identity now reports the signing key bound to it — and reports none when that key
  has been revoked, retired to decrypt-only, or has no private half left, which are
  exactly the cases where sending would not sign either. Alongside it, the server says
  whether it can sign *at all*: with no crypto process wired, mail goes out carrying your
  public key and no signature, and a client can now say that instead of promising a
  signature that never arrives. Both are read-only facts, never key material, and both are
  read from what the send path itself consults — so neither can drift from what actually
  happens when you send.

### Fixed

- **A message re-sent after Undo Send is signed once.** Sending, pressing **Undo**,
  and sending again used to sign the already-signed message a second time and attach a
  second copy of your public key. Both are worse than they sound: the real signature ends
  up buried inside an outer one, and two key headers make a conformant recipient ignore
  *both* — so re-sending a message quietly withdrew the key it was meant to publish.
  A re-send now leaves the prepared message exactly as it was.

### Changed

- **The send line now reads the server's own signing facts, instead of guessing from the
  login's key list.** "Will sign" required both `Identity.signingKeyId` (is a key
  bound to *this* address) and the `urn:mailroom:pgp` `signing` flag (can *this server* sign
  at all) — a client-side reading of `UserKey/get` could say "Will sign" over a message that
  would go out unsigned. "Will not sign" now names why: `no key bound to house@example.com`
  when the server could sign but no key is bound here, `this server cannot sign` when it
  cannot sign at all regardless of any key. The settings Keys row also states, per key,
  which identity it signs as.

## [1.67.0-rc.3] — 2026-09-09

Third pre-release of "Settings, compose, outbox, viewer": compose as the desk's sending
surface — From as a sentence, recipient chips that carry the key fact, the send line, a
words-only footer, autosave, forwarding with held attachments — and the Outbox. Still to
come before `1.67.0`: the attachment viewer.

### Added

- **Compose happens where reading happens.** The composer was a small panel docked
  to the bottom-right corner over whatever you were reading; it now takes the reading
  pane's own column, with the list still beside it. With the reading pane switched off it
  takes the main area, and below 760px it is the whole screen. Closing it puts you back on
  the row you started from.
- **The From address is a sentence with a reason on it.** `house@example.com · because you
  are replying to mail sent there`, or `personal@example.com · your primary address` — and
  a **Change** button that opens a typed, filtered list of every address you can send as,
  each row saying why it is there: *replying here*, *primary*, *catch-all*, *disabled*. The
  addresses that can send come first; a catch-all still lets you type the local part.
- **Recipients are chips, and each one says whether a key is held.** To / Cc / Bcc keep the
  contact autocomplete, and a finished address becomes a chip: paste a list and you get a
  chip each, Backspace puts the last one back as text to fix. A chip reads `⚿ key held`
  when an OpenPGP key is held for that address, `no key` when the server answered and holds
  none, and says nothing at all when nobody could be asked.
- **One line under the body says what will happen to the message.** `Will sign · keys held
  for A friend and Partner` when every recipient's key is in hand; `Will sign · no key for
  Partner · would send as plain text to all 3` when one is not — never a silent downgrade;
  `Will not sign: no key for house@example.com` when you have no key; and `Signing status
  unknown on this server` where keys cannot be asked about at all. It never promises to
  encrypt: encryption itself is still to come, and the line names who *could* be encrypted
  to. Announced when it changes.
- **Forward works.** `Fwd: …`, the message quoted, and its attachments carried along by
  reference — no re-upload. An attachment the malware scanner is withholding is shown
  hatched, saying `Held · cannot be forwarded until released`, with a *Release…* control;
  until it is released the file is left out of the send rather than failing it.
- **Drafts save themselves.** Every fifteen seconds while there is something new, and when
  you leave the body — with `Saved a moment ago` / `Saving…` / `Not saved yet` in the
  footer, and `Not saved · <reason>` there rather than as a message across the screen if the
  server refuses. *Discard* throws the draft away; closing the composer keeps it. A row in
  **Drafts** now reopens the draft in the composer and carries on saving into it, instead of
  showing it as mail to read.
- **The Outbox is a place.** `#/outbox`, which is where the rail's Outbox entry has
  always pointed at Sent with an apology. One row per message that has not gone: who it is
  to, its subject, the address it goes out as, and either `Sending in 14 s` or
  `Scheduled · Mon 09:00`. **Undo** or **Cancel** stops it; **Send now** sends a scheduled
  message immediately. Empty, it says `Nothing else waiting. Once a message has gone it is
  in Sent.`
### Removed

- **The send-quota meter is gone from the composer.** "N of M sends left today" was
  a limit restated on every screen where mail is written, about a cap the server enforces
  regardless. The number is still on the account-health panel, where a limit belongs.

## [1.67.0-rc.2] — 2026-09-09

Second pre-release of "Settings, compose, outbox, viewer": the deep settings rows — each
account from the inside, the areas and drawers editor, the line with its week and its
preview, the in-tray nag. Still to come before `1.67.0`: compose and the Outbox, and the
attachment viewer.

### Added

- **Accounts can be managed from Settings.** One line per account — colour, name,
  whether it is syncing, how long ago it last did — opening onto that account's own
  screen: pause or resume its sync, give it a colour so its dot is recognisable rather
  than merely different, read the server it connects to, change the credentials, and
  switch individual folders between *syncing* and *paused*. Behind **more**: its provider
  profile, and *Remove account…*, whose confirmation states exactly what the server does —
  the folders, identities, signatures and outbound history go, messages that live only in
  that account are deleted permanently, a message that also exists in another account is
  kept, and a PGP key bound to one of its identities survives with only its link cleared.
- **An account's colour is a colour you chose.** Four named swatches or any colour you
  like, and the dot on every desk row and in the account switcher uses it — with the name
  still beside it, and with a paused account still showing the paused tone whatever colour
  it holds.
- **Areas and drawers are configurable.** A card per area: rename it, move it up or
  down, choose which category tabs and which labels feed it, and set how long its drawer
  keeps mail — *never*, 7/14/30/90 days, a custom number, or *default*, which follows the
  login-wide default and moves when it does. Each card says `archived N so far · never
  deletes · never marks read`, and while your operator has sweeping switched off, both the
  row and every card say `sweep off` rather than promising something that does not happen.
  Plus: a login-wide default period, *Add area* (Mailroom mints its label), the labels that
  feed no area with an *Assign to* for each, and how much mail still needs sorting.
- **The line says what it has done and what one more step would cost.** The
  threshold with *stricter* / *looser* and a live preview, exactly as on the desk — and two
  facts the desk has no room for: `last 7 days: 28 to review · 289 to drawers`, and
  `one step stricter (0.65) would move 12 more messages to drawers`. Nothing moves until
  you apply, including a reset.
- **The in-tray nag is a setting.** How many days something may sit in the tray before the
  strip starts saying so, with a reset to 7.
### Changed

- **Folder syncing moved out of the rail.** The pause / resume control for an
  individual folder is now in **Settings → Accounts → Folders synced**, where folders are
  configured. The rail still *says* which folders are frozen, on the folder itself — that
  half deliberately stayed, because a folder that quietly stopped updating while looking
  like a live one is a lie about mail.
- **Escape backs out of one account, not out of Settings.** From inside an account it
  returns to the list of accounts, one layer at a time, like every other layer on the page.

## [1.67.0-rc.1] — 2026-09-09

First pre-release of "Settings, compose, outbox, viewer": the settings
page as one searchable page of twelve state-first rows, and the server facts the rest of
the milestone reads (correspondent and own keys; when an attachment was released). Still to
come before `1.67.0`: the deep settings rows (accounts, areas and drawers, the line, the
in-tray), compose and the Outbox, and the attachment viewer.

### Added

- **Key facts reach the client.** Compose can now show "key held" on a
  recipient chip and a send line that says whether a message will sign and/or encrypt;
  the settings page can show "1 key, published · 0 correspondent keys held". Two
  read-only additions, gated on a new `urn:mailroom:pgp` capability: `Contact.keys`
  lists a correspondent's known public keys (fingerprint, kind, source, when it was
  found — never the key material), `Contact/query` gains a `hasKey` filter for
  "how many correspondent keys do I hold", and a new `UserKey/get` method lists the
  login's own keys (published/decrypt-only/revoked — again, public facts only).
- **Email.attachmentScan reports when you released it.** Each part in
  `attachmentScan.parts` now carries `releasedAt`, the UTCDate of the user's own
  release decision (or null), so the attachment viewer can print "released by you ·
  <date>" instead of just a bare badge. It is read from the existing release lookup —
  no new query — and, like `released`, it is independent of the scanner's verdict and
  never churns on a re-scan.

- **Settings is one searchable page.** Twelve rows — Accounts, Areas and drawers,
  The line, In-tray, Rules, Identities and signatures, Notifications, Reading and safety,
  Keys, Saved searches, Appearance, Data — each stating what it is currently doing in one
  line before you open anything: `6 areas · 3 labels in no area · 225 need sorting`,
  `threshold 0.60`, `push on`, `12 senders always allowed`, `theme dark · density normal`.
  A field at the top finds a setting by its name, by any word of what it says, or by a
  value it holds — typing an alias finds the identity that owns it, typing a domain finds
  the row that remembers it. Clicking a heading opens that row's controls, one at a time,
  and `#/settings/<row>` links straight to it.
- **Every setting says who it applies to.** A small word on each row — `login`, `account`,
  `area`, `web`, and some rows carry two — so changing something for one account in the
  belief it applied everywhere, or hunting for a global setting inside one account, is no
  longer a thing the page lets you do quietly.
- **Appearance shows what you have changed.** Theme, density, how a message opens and the
  pane split each say `changed` and offer a reset when they differ from what Mailroom
  ships with; a control you have never touched says nothing at all. The pane split is
  stated as `58 / 42` with the reminder that the divider on the desk is what sets it and
  that it follows you between browsers.
- **Identities are shown as a precedence, not a list.** Which address a reply actually
  goes out as, in order, grouped by account, with the reason beside each: `primary`,
  `catch-all`, `disabled`. A concrete address you can send from comes before a catch-all
  that only accepts; anything switched off comes last.
- **A Keys row that is honest about a server that has none.** Where the server offers the
  keys API it states your OpenPGP key — fingerprint in groups of four, published or not,
  and when it was made — plus how many of your correspondents have a key on file. Where
  the server does not, it says *keys are not available on this server*, which is a
  different statement from *no key* and is never made in its place.
- **The page says what is missing, out loud.** Quiet hours are not a setting yet; returned
  snoozes and bounces are not routed to the in-tray yet; export and deletion are run by
  your operator. Four rows state their facts and say their controls arrive next.

### Changed

- **The settings index of nine sheets is gone.** Each of its sections is now a row
  of the one page, and links to the old ones still work: *Remembered senders* is inside
  **Reading and safety**, *Signatures and templates* is inside **Identities and
  signatures**, and *Categories* is behind **more** on **Areas and drawers**. Keyboard
  shortcuts stay a separate sheet — nothing on it is a setting — and `?` still opens it.
- **A count on the page is a count that was taken.** Nothing states a fact it has not
  measured: an account is not described as *ok* before its status has been read, drawers
  are not described as auto-archiving while the sweep is switched off, and a clause with
  no number behind it is left out rather than printed as `0`.

## [1.66.1] — 2026-09-08

One fix, found within the hour of the v1.66.0 deploy: desk rows were
showing a few letters of subject beside a screen-wide snippet.

### Fixed

- **Desk rows show the whole subject again.** A long snippet was shrinking the subject
  beside it to a few letters (`Emerge...`), because the snippet's flex basis was its own
  text width and the browser split the shortfall in proportion. The snippet now takes only
  the space the subject leaves.

## [1.66.0] — 2026-09-08

**The desk**. The inbox is a partition now: an
in-tray for what you flagged, *Review first* and *Everything else* split by a line you
can move with a preview, areas as sections, drawers that say when they auto-archive, a
rail and a folded account switcher that scope everything, a words-only reading pane with
labels on the message, the sender popover and folded conversations, appearance that
follows you between browsers, undo on every action, honest offline and behind states,
the keys that move a message across the line (with the server recording the placement), and a phone layout. Built and dogfooded as four pre-releases — `1.66.0-rc.1`
through `-rc.4` below carry the detail — and finished by the entries in this section.

### Added

- **The desk on a phone.** Below 760px the desk is one column, and it is a
  different arrangement rather than the same one squeezed: the account name with a `▾`
  IS the account switcher — tapping it opens the accounts and their health as a bottom
  sheet — with *synced 40 s ago* beneath it, replaced word for word by the offline or
  behind sentence when there is one. Under that, the in-tray is one tappable line that
  goes straight to the tray, the two halves are a full-width segmented control
  (*Review first · 6* | *Everything else · 37*), and each area's heading is a button
  that folds its section away and stays folded on that device — the count keeps its
  place on the folded heading, so nothing goes missing quietly. Rows become two lines
  (sender and time over subject), and under 520px the snippet goes.

- **Swipe a row, and a bottom bar to get anywhere.** On a phone, swiping a row right
  archives it and swiping left puts it in the in-tray — or, on the tray page, takes it
  out — with the same Undo toast the buttons and the keys raise, because they are the
  same verbs underneath. A swipe needs 35% of the row or 96px of travel and under 24px
  of vertical drift, so scrolling never archives anything; the buttons remain the way to
  do all of it without a gesture, and asking for reduced motion keeps the gesture and
  drops the animation. Long-pressing a row selects it, and the selection toolbar docks
  above a new bottom bar — *Inbox · In-tray · All mail · Search · Compose* — which sits
  under every pane, including the navigation rail, so the rail is a place one tap gets
  you out of. Opening a message replaces the list, with `← Inbox · 3 of 6` at the top and
  the actions docked at the thumb (*Archive · In-tray · Snooze · Reply*, the rest under
  **More**); tapping the sender opens the sender panel as a bottom sheet.

- **`Shift+↑` and `Shift+↓` move one message across the line.** Over the
  message under the cursor or the whole selection: *this belongs on the surface* pulls it
  above the line, *this belongs in the drawer* pushes it below, each with an Undo whose
  inverse is the opposite correction. The server records the placement rather than only
  the feedback, so the row moves to the other half of the desk instead of waiting for the
  model to be retrained. Both keys are listed in the Keys sheet, which remains the one
  place a shortcut is written down.

- **Appearance changed on another device now arrives at once.** `ClientPreferences`
  joined the event stream server-side, so the web client subscribes to it with everything
  else instead of waiting for the fallback poll.

- The reading pane says when you placed a message above the line or in the drawer
  yourself.

## [1.66.0-rc.4] — 2026-09-08

Fourth pre-release of the desk: undo on every desk action, the offline and behind
header states, and — on the server — the line override, so a correction moves a
message across the line at once and the drawer sweep respects the user's placement. Also
carries the `Email/query` `limit: 0` fix. Still to come before `1.66.0`: the phone
layout and the keys that move a row across the line.

### Added

- **Undo, on everything the desk does.** Archive, Trash, re-file, in-tray /
  star, snooze, mark read / unread and archive-a-whole-drawer each show one toast with
  an **Undo** button and the seconds left beside it — and `Ctrl/⌘+Z` runs the last one
  from wherever you are, so the offer is not a hunt across the screen. The countdown
  PAUSES while the toast has focus or the pointer is over it, because a deadline that
  runs out while you are still reading is a deadline only fast readers get. At most three
  offers are remembered and one toast is on screen at a time; sending gains the same
  countdown, ticking down the server's own undo-send window rather than a guess at it.
  Each undo says what it actually did: taking back a Trash reports *Restored to Archive*,
  because that is where the server puts it, and an undo that only partly landed says how
  much did not.

- **Offline and behind are two different sentences now.** The desk's and the tray's
  header line says which: *Offline. Showing what was loaded · last synced 14 min ago ·
  3 changes waiting to send*, with a **Retry** button — or, when Mailroom is answering
  fine but cannot reach your mail provider, *Sync is behind. Work last synced 3 h 12 min
  ago · provider not reachable since 09:02*, with **Details** and a line stating the
  thing that actually matters: everything you do is saved here and reaches the provider
  when it is reachable again. Offline beats behind beats a failed request, so one cause
  is never reported as three problems, and each clause is printed only when the account
  health really carries it. Offline, rows whose change has not gone yet are tagged
  *waiting to send*, and a row the desk listed but has no message for says *not loaded*
  instead of leaving a silent gap.

- **Marking a message important now actually moves it across the line.** Until
  now a correction was a note to the model and nothing else: the row stayed exactly
  where its score had put it until the model was next retrained, the counts beside the
  area did not budge, and a message you had just pulled above the line could still be
  archived out of a drawer by the sweep. Now the correction *is* the placement. It takes
  effect immediately everywhere the line is read — the list, the counts, and what the
  drawer sweep is allowed to touch — and your placement outranks the model: re-scoring
  never undoes it, and starting your model over or rolling it back leaves it alone,
  because rolling the model back is not you taking back what you said. Mail nothing has
  scored still sits above the line by default; if you put it in a drawer yourself it
  stays there, and it will eventually be archived like anything else in one. A
  correction now shows up in `Email/changes` like any other change to a message, so
  other devices follow, and a client can ask for the new read-only `lineSide` property
  to say "you placed this above the line".

### Fixed

- `Email/query` honours an explicit `limit: 0` (zero ids, `total` and
  `queryState` still computed) instead of treating it as no limit and
  returning every matching id.

## [1.66.0-rc.3] — 2026-09-08

Third pre-release of the desk: the reading pane joins the desk, the rail and the
tray shipped in rc.1 and rc.2 — a words-only action row, labels on the message, the sender
popover, folded conversations, rows as threads, and appearance that follows you between
browsers. Still to come before `1.66.0`: undo, the offline and stale states, the phone
layout, and the keys that move a row across the line.

### Added

- **The reading pane says where a message is filed.** Under the from/to line, one
  row: the AREA the message is in as a raised chip — whose tooltip names who put it there
  ("Area: Personal — by rule") — then every other label it carries as an outlined chip in
  that label's colour, with the name always printed beside it because colour is never the
  only signal. Clicking a chip opens all mail narrowed to that area or label, carrying
  whatever account scope you were in. `+ label` at the end opens the label menu on the
  message.

- **A sender popover, on the sender's name.** Rest the pointer on it (or press Enter)
  and it opens: who they are, which of your addresses they write to, why this message
  ranked where it did, what SPF/DKIM/DMARC said about it, and which area it landed in
  and who filed it there — plus *Always show images from them*, *Change area* and *Create
  a rule*. Clicking pins it; Escape closes it and puts focus back on the name. Every line
  is omitted when the fact behind it is unknown, so it is never a heading with nothing
  after it.

- **Conversations read as conversations.** With more than one message in a thread, every
  message that is already read and is not the one you opened folds to a single line —
  sender, snippet, time — and clicking it opens it (and marks nothing read by itself).
  The message you opened and everything still unread stay expanded.

- **Appearance follows you between browsers.** Theme, density and whether a message opens
  beside the list or replaces it are stored per client family (`ClientPreferences`), so a
  second browser opens in the appearance you chose in the first. The reading-pane divider
  syncs too, as a percentage of the content area rather than a pixel width, because the
  two machines do not have the same window; a change arriving while you are dragging it is
  held until you let go. Everything still applies locally first and keeps working with the
  server unreachable — a refused write leaves the setting where you put it and says so.
  Settings ▸ Appearance gains a *Message opens* choice (*Beside the list* / *Focused*);
  choosing Beside restores the orientation this device last used.

- **The reading pane's action row is words, and only words.** It is now Archive ·
  In-tray · Snooze · Mute · **Reply** · More ▾, with the back control where it was. The
  bare ★ is gone — it was the in-tray flag wearing a glyph nobody could read — and Trash,
  Pin, Mark important, Label, Create rule, Print and (only when the classifier still has
  a question about the message) Dismiss moved into the More menu, which is a real menu
  with arrow keys and Escape. Reply is the one primary button, so the row has a shape
  rather than seven identical controls. No keystroke is printed on any of them; the Keys
  sheet remains the one place shortcuts are listed. "Mark as important" also stopped
  appearing twice — the why-here strip explains a ranking, the action row acts.

- **`#/inbox` rows are conversations, not messages.** Every desk section and every drawer
  collapses each thread to its newest message, and the counts beside them count the same
  things the rows do. The in-tray deliberately does not: a tray item is the flag, and the
  flag is per message, so collapsing would hide the message you actually picked up behind
  a newer reply.

## [1.66.0-rc.2] — 2026-09-08

Second pre-release of the desk: the rail, the folded account switcher, the whole
tray and a real All mail join the desk shipped in rc.1. Still to come before `1.66.0`: the
reading-pane changes, undo, the offline and stale states, and the phone layout.

### Added

- **The rail is the desk's own map now, and every list can be scoped to one account.** The navigation rail leads with a folded account switcher — `All accounts ▾`,
  with a muted `· 1 paused` / `· 1 failing` when an account has stopped working — that
  opens to the list of accounts and, beneath it, the account-health panel. Choosing a
  scope does not navigate anywhere: it re-scopes the screen you are on, and the whole
  client follows, because an account is now a structured condition that intersects
  whatever the list already asked for rather than a search that could not be combined
  with one. Below the switcher the rail states what is waiting on you: Inbox (the tray,
  what is to review and what is in drawers, added up), In-tray, Outbox and Drafts *only
  while they hold something*, Sent, All mail, then your areas with `review / drawer`
  beside each and Needs sorting last, then labels, saved searches and Settings. Counts
  are blank until they are actually known — a `0` that means "not loaded" is the most
  reassuring possible wrong answer.

- **The in-tray has a page of its own: `#/tray`.** Everything you said you would come
  back to, grouped by how long it has been waiting — Today, This week, Over a week, Over
  a month — with a sentence above it that states how many there are and how long the
  oldest has waited, and says outright when the list is only the newest page of a longer
  one. `f`, or *Remove from tray*, takes a message out and the row leaves at the
  keystroke. It costs a single request however many areas you have.

- **All mail is a real list rather than the archive wearing its name.** `#/all` is now
  everything that is not in the trash — archived, filed, snoozed and all — in one query,
  and it narrows to an account, to an area (by the area's own claim, so mail any of its
  feeds resolved into it is included) or to a label without needing the search index.

- **Keys: `g g` opens the account switcher, `g 0` shows all accounts and `g 1`–`g 9`
  the Nth.** Asking for an account you do not have does nothing rather than switching to
  a different one. The keys sheet now also lists the desk shortcuts that landed with it —
  `f` for the in-tray, `1`–`9` to re-file into an area, and Tab to swap the desk's halves.

### Changed

- **The account scope narrows every list, not only the ones with nothing else in them.**
  It used to be honoured only on a bare inbox or all-mail list, because a per-account view
  was a text search and the server has no filter operators to combine one with a role, tab
  or label. It is now a structured condition on the same filter, so `?acct=` means the same
  thing on the desk, the tray, all mail, a label, a tab, the priority inbox and the
  needs-a-category queue. A folder and a search are deliberately left alone: a folder is
  already in exactly one account, and a search carries its own `account:` term.

- **`#/all?area=<id>` shows the area itself rather than the area's own label**, which was
  a stand-in that missed every message another of the area's feeds had resolved into it.

- The desk's account dot now means what it says: an account whose sync is switched off.
  It previously stood in "no identity for this account is enabled", a different fact that
  merely correlated with it.

### Removed

- **Rail entries for Priority, Starred, Snoozed, Unread, Needs a category and the
  per-account list.** None of the views went anywhere — `g p`, `g s`, `g b`, `g n`, the
  command palette and every URL still reach them. What went is the claim that they are
  places you navigate between rather than filters over the one question the desk answers.
  Starred in particular has no entry because the in-tray *is* the flag, and two rail rows
  for one keyword is the same list twice under different names. Archive, Junk and Trash
  moved into the Folders disclosure at the bottom, which is collapsed by default and opens
  itself while a drag is in flight so dragging mail onto them still works.

## [1.66.0-rc.1] — 2026-09-08

First pre-release of the desk. It is cut mid-milestone on purpose: the desk is
usable and worth dogfooding now, while the rail, the account switcher, the full tray, the
reading-pane changes, undo and the offline states are still to come. Pre-release tags
carry features because features cannot ride patch releases; `1.66.0` follows when the
desk is whole.

### Added

- `Email/query` filters `onDesk` and `onSurface` (`urn:mailroom:desk`): select the mail
  the desk shows — live in INBOX or a folder that feeds the desk, and above or below the
  line (`DeskPreferences.lineThreshold`; unscored mail counts as above). Both are refused
  by `Email/queryChanges`, like `inArea`. Groundwork for the desk.

- The web client's data layer for the desk (`urn:mailroom:desk`). Areas and the line load
  with the rest of the reference data and stay current through the change stream; opening a
  desk tab fills every section it has — the in-tray, each area, Needs sorting — in one
  batched request, each section with its own window, count and error, so a section that
  could not be queried says so instead of showing an empty list. Moving the line and
  archiving a whole drawer are written from the server's own answer rather than
  optimistically, and archive-all is capped and reports when the drawer still holds more.
  No screen yet — this is the layer the desk itself is built on.

- **The desk: `#/inbox` is now a partition of your mail rather than one list.** A
  header sentence states what the screen holds and how current it is — how much is in the
  in-tray, how much is waiting to be reviewed, how much is in drawers, and how long ago the
  last sync was — and it is the only thing that speaks up, so a screen reader is not read a
  running commentary. Above the list, the in-tray states its own backlog in one line (how
  many, how long the oldest has waited, how many are over a week) and opens to five rows.
  Two tabs split the inbox: *Review first*, one section per area with Needs sorting last and
  every area present even when it is empty, and *Everything else*, one closed drawer per area
  that says when it auto-archives — and says "sweep off" when the sweep is switched off, so
  the row never promises an archive nothing performs. Between them sits the line, with
  *stricter* / *looser* that PREVIEW what a different line would show and change nothing
  until you apply it. Rows carry the sender, subject, snippet, the alias the mail arrived at,
  the conversation size, and an account dot when several accounts are on screen — and no
  hover buttons at all: the keyboard and the selection toolbar are how you act. The whole
  tab is one virtualised list, so a drawer holding months of mail scrolls like a short one.

## [1.65.1] — 2026-09-08

One fix, found by eye an hour after the v1.65.0 deploy and invisible to
every server-side check by design: allowed remote images never loaded on an instance
served over plain `http://`.

### Fixed

- **Allowed remote images now load on an instance served over plain `http://`.** Since the image
  proxy arrived, an allowed image is fetched through Mailroom's own address rather than the sender's,
  and on an instance reached over `http://` the browser refused every one of those fetches before
  they left the page: the message frame and the app only permitted `https:` images, and a frame's
  own origin does not count as itself inside the sandbox. Nothing reached the server, so nothing
  was logged, and the banner kept saying images were loading. Both policies now name the instance's
  one exact address when you have allowed images for a message; the bare `http:` scheme stays
  refused, and a message whose images you have not allowed still permits none.

## [1.65.0] — 2026-09-08

The desk's foundations (the first increment of the client rebuild settled in the design
work of 7 September). Everything the desk needs before a single row can be drawn: areas
on the server with a report-first path from the current category verdicts, the drawer
auto-archive job shipped armed but off, a timestamp on the flag for the in-tray, a token
inventory the web build checks, and a route for every screen. Nothing here moves any mail
on its own: the two area reports and the drawer report are read first, and each write is
an explicit `--apply` or `--enable`.

### Added

- **Every screen in the web client has an address.** Opening a message, opening settings or a
  settings section, and starting a reply now change the URL, so a link can be pasted, a refresh lands
  where you were with the same message open and the list scrolled to the same place, and Escape does
  exactly what the browser back button does, one layer at a time: composer, then message, then
  settings section, then settings. Settings is a page of its own rather than a menu of pop-up sheets;
  every old bookmark still works. The routes the coming desk needs (`#/inbox/review`, `#/tray`,
  `#/all`, `#/settings/areas`) already resolve to the nearest thing the client can show today.
  Two things behave differently on purpose: clicking the folder you are already in no longer clears
  your selection or closes the message, and keyboard actions no longer reach the list while a settings
  sheet is open (before, an archive keystroke behind the open rules editor archived whatever was
  selected).

- **The web client checks its own colours at build time.** Every design token now lives in one
  inventory file, and the build fails if a stylesheet uses a token that is not defined, defines a
  theme token anywhere else, gives a token to one theme but not the other, or lets any declared
  text-on-background pair fall below the WCAG AA contrast ratio in either theme. The check found two
  live defects on its first run: a panel background token used twelve times and defined nowhere, so
  those panels had been rendering with no background in both themes, and the reading-pane split width
  that had no value until the first drag. Both are fixed. The settled semantic tones from the desk
  design (held, sealed, returned, and the account and label colours) are now defined in both themes,
  ready for the desk; three of them sit just under the ratio as plain text in light mode and are
  listed as known debt in the file rather than quietly waved through.

- **Drawers archive themselves, on a schedule you can see first.** Below the line, each area's
  drawer now automatically archives mail that has sat in it longer than the area's period: an hourly
  job moves those messages to the account's Archive folder on the server, so every other client
  agrees about what is in the inbox. It never deletes and never marks anything read; only an explicit
  rule of yours may do either. Flagged, pinned and snoozed mail, anything above the line, anything the
  desk has not scored, and anything no area claims are never touched, whatever their age. Each area
  can switch its drawer off or set its own period; an area with no period of its own uses the login
  default (30 days). A message the job archived says so, and each area reports how many it has
  archived so far. The sweep is **off for every login until it is switched on**: the hourly job runs
  but archives nothing, so upgrading never moves mail before anyone has looked. Before switching it
  on, `mailroom-admin drawer-report` shows per area what would be archived right now, the oldest and
  newest of them, and how many old messages are held back by each reason; `--enable` switches the
  sweep on for the login, and `--apply` performs the archive immediately, then re-checks that nothing
  remains and that the unread count and the message count are unchanged.

- **The flag remembers when it was set.** A flagged message now carries the moment its flag went on,
  cleared again when the flag comes off, whichever side did it: you in a client, a rule at arrival, or
  another mail client setting the flag on the server. Clients can read it as `flaggedAt` and show how
  long something has waited in the in-tray. Mail that was already flagged before this release is
  stamped with its arrival time, which is the earliest the flag could have been set, so a wait is
  never overstated. The property is read-only; the flag itself is still the ordinary `$flagged`
  keyword.

- **Areas are labels, written down.** A message that lands in an area only because of its category
  verdict now carries that area's own label, so the placement is something you can see, search for,
  and keep when the classifier changes its mind. New mail gets the label as it arrives, in the same
  transaction as the message; it is recorded as applied by the desk, never as your own filing, so it
  cannot teach the label model anything and a later re-sort can move it without touching labels you
  applied yourself. A message a label already routes into an area is left exactly as it is. For mail
  that was already there, `mailroom-admin materialize-area-labels` reports first: per area, how many
  messages would gain the label (in the inbox and overall), how many already carry it, how many a
  different label decided, how many no area claims and why, and which of the labels would mirror to
  IMAP; nothing is written until `--apply`, and no IMAP writeback is queued either way. A companion
  command, `mailroom-admin area-plan`, loads a plan file into the stored areas the same way: it shows
  the difference against what is stored (areas to create, feeds to add or move, positions to change,
  areas it will leave alone) and refuses up front if the plan asks for something that cannot be done
  without deleting an area.

- **The desk is reachable over the wire.** A client that declares the new desk capability can now
  read, rearrange, create and remove areas, including which tabs and which labels feed each one, and
  see every change through the ordinary change stream. One call returns the desk's counts: for each
  area, how many messages sit on the surface, how many in the drawer, and how many have no score
  yet, plus the pile no area claims. The desk's own settings and the per-client-family appearance
  record are readable and settable the same way; a folder can be marked as feeding the desk; a
  message list can be filtered to one area or to the unsorted pile, and a message can say which area
  it resolved into. Every refusal names the property at fault: a tab or label already feeding another
  area says which area has it, and an area id that does not exist is an error rather than an empty
  list. A user's export now includes the areas (by label name, not id), the desk settings and the
  appearance records, so none of this arrangement is lost on migration.

- **Areas exist on the server.** The desk's areas — the groups every message falls into, fed by
  category tabs and by labels, with an area owning a label of its own so that placing a message in
  an area is the same act as applying that label — are now stored objects rather than a plan file
  handed to a report. Each area carries its order on the rail, whether it is hidden there, and an
  optional auto-archive period. The database itself refuses a tab or label that would feed two
  areas at once, and refuses to delete a label an area still owns. A login with no areas gets one
  per tab on first read, reusing a label you already have of the same name rather than making a
  second one. Alongside them: the desk's own settings (the score threshold that divides surface
  from drawer, the default drawer period, how long an in-tray item waits before a reminder), a
  per-client-family appearance record so a phone and a browser can differ while two browsers agree,
  and a per-folder switch for whether a folder's mail counts as unactioned like the inbox. Every one
  of these writes appends to the change log like any other synced object; no client can see or set
  any of it yet — that is the next increment. The query-side resolver that will drive counts and the
  area filter is built and proven to agree with the report's resolver on every message shape.

- **`mailroom-admin area-report`: the report before the areas feature.** The client rebuild groups
  mail into *areas* fed by category tabs and labels. Before anything is built on that idea, this
  read-only command resolves every live message into an area under a plan you supply (or one area
  per tab by default) and prints, for the inbox and for all mail separately, how many land in each
  area, how many of those would sit on the surface versus in a drawer at a chosen score threshold,
  how many the plan cannot place and for which of three different reasons, how often two labels
  pulled a message toward two areas, and which labels and tabs the plan leaves without a home. It
  writes nothing. A plan line looks like `Notices = label:Shopping, label:Information`; a mistyped
  label or a label claimed by two areas is refused line by line rather than silently ignored.

## [1.64.4] — 2026-09-07

One defect, found by watching a CI harness fail for the wrong reason (v1.64.3), and
the visibility and recovery it turned out to be missing. Plus one deployment pin.

### Fixed

- **A momentary connection failure while an account was first set up switched its sync off for
  good, silently.** When Mailroom validates a newly declared account by connecting to the mail
  server, any failure at all — including the server being briefly unreachable — stored the account
  with sync switched off. Nothing ever retried it and nothing ever mentioned it again: every later
  restart or deploy left the account exactly as it found it, so the only way back was to edit the
  database by hand, while the instance looked perfectly healthy and mail simply stopped arriving.
  Now only an actual rejection by the server — a wrong password, or a configuration it refuses —
  switches sync off, because retrying those cannot help and retrying a wrong password can lock the
  account at the provider. Anything else leaves the account switched on, and the ordinary sync loop
  retries it with backoff, so a blip costs one attempt rather than the account.

### Added

- **A switched-off account is now visible for as long as it stays that way.** Every sync pass names
  any disabled accounts in its log as a warning, instead of reporting that it had nothing to do, and
  a new `mailroom_sync_disabled_accounts` gauge on `/metrics` can be alerted on.
- **`mailroom-admin enable-account` and `disable-account`.** The supported way to switch an
  account's sync back on, or pause it, without touching the database. Enabling checks the stored
  credentials against the mail server first and refuses, saying why, if they still fail — unless
  told to enable anyway and let the sync loop retry.

### Changed

- **The bundled virus scanner image is pinned to ClamAV 1.5.4** rather than the floating `1.5`
  line. Recorded because it changes the image a deployment pulls; the attachment report's
  size-limit figure was already measured against this version.

## [1.64.3] — 2026-09-06

Nothing here changes what Mailroom does with your mail. Both items are about
the project's own machinery telling the truth: a test that could fail for a
reason unrelated to the code, and a package that looked like the implementation
and was not.

### Fixed

- **A release could be marked failed by a test that never ran.** One of the end-to-end suites starts
  a throwaway mail server with a self-signed certificate and then starts Mailroom, which has to
  trust it. Mailroom was being started a fraction of a second before it was given the certificate,
  so on a slow machine it could try to connect first, fail, and — because a rejected account stays
  switched off — never recover for the rest of the run. Everything downstream then had no mail to
  work with. The suite noticed and said so rather than passing, which is the behaviour we want; the
  setup is now ordered so the window cannot exist. The same fault was found and fixed in a second
  suite.

### Changed

- **Documentation stopped pointing at code that does not run.** Mailroom contains a full
  implementation of the standard email-threading algorithm which nothing calls — the conversation
  grouping you actually see comes from a much simpler database query. A comment describing the
  unused package as the real one had already sent a previous bug fix into code that could not affect
  anything. Both are now labelled for what they are, and the question of which should be running is
  recorded as an open decision rather than left implied.

## [1.64.2] — 2026-09-05

More housekeeping, and the end of a review list. Two of the four items recorded
during the v1.61 reviews were fixed in v1.63.0 and v1.64.1; the last two are
here, together with a fifth that had been written down as "real, but we could
not reproduce it" — it reproduces.

### Fixed

- **A cleared file could keep its quarantine warning.** When a scan re-checked an attachment and
  cleared it at the same moment another part of Mailroom flagged it, the "quarantined" marker on
  your message could stay put even though the file was fine. The stored verdict was always right;
  what went missing was the notification that told your mail client to update, so the warning sat
  there until something else about the message changed. A narrow timing window, and it now has a
  test that deliberately creates it.

- **Deleting a message could leave an instruction to upload it.** A message deleted for good while
  Mailroom still had a pending "copy this to the mail server" task left that task behind pointing at
  bytes that no longer exist. It could never complete. The task now belongs to its message and goes
  when the message goes — while the separate task that *removes* a message from the server
  deliberately still outlives it, because that one has work left to do.

### Changed

- **The export now says plainly what it is.** The archive `mailroom-export` writes has always been
  the answer to "my mail is mine and I can leave" — and the mail in it genuinely does read back into
  Mailroom, byte for byte, with read and flagged states intact. But the extra files beside it, which
  carry labels, rules, saved searches and the rest, have never had anything that reads them: they are
  a record for a person, not an import. That is now stated in the archive's own `manifest.json`,
  where somebody opening the directory in three years will actually see it, along with the pointer
  that moving a whole installation is a database restore rather than an export.

- **Cutting a release no longer runs the tests twice.** Publishing a version pushed two things, and
  both of them started the full test suite, so every release ran roughly 19 jobs' worth of work twice
  over on identical code — and one of those duplicates recently failed on an unrelated flake and
  reported a red result against an already-published version. The duplicate is gone; the one thing it
  was quietly checking, that a version is only ever cut from code that passed, is now its own small
  check that takes seconds.

## [1.64.1] — 2026-09-05

Housekeeping. Nothing here changes what Mailroom does with your mail — these are
all cases of a check, a count or a document quietly saying something untrue.

### Fixed

- **The storage report called correctly-kept files leaks.** The reconciliation report counts stored
  files nothing refers to any more, so an operator can tell real waste from ordinary use. It was not
  counting *cached remote images* as a reason to keep a file, so every cached image was listed as
  wasted space. The deletion logic always knew better, so nothing was ever wrongly deleted — the
  report just sent you looking for a problem that was not there.

- **The attachment report named the wrong size limit.** It said the virus scanner refuses anything
  over 25 MB. The real limit on the deployed scanner is 100 MB, and the report's own count had been
  using the right figure for a release — only the label beside it was stale. It now takes the number
  from the same place the count does, so the two cannot disagree again.

- **`mailroom-admin --help` was missing 18 of its own commands.** The command list, the dispatcher
  and the documentation had drifted into three separate lists. Among the undocumented was
  `pgp-export-wrapped`, which the export archive itself tells you to run to retrieve the private key
  your encrypted mail depends on. The list is now generated from the dispatcher, and a test fails if
  the documentation and the dispatcher ever disagree in either direction.

- **The API reference omitted five capabilities the server advertises.** Anyone reading the document
  rather than the source would not have learned that unsubscribe detection, remote-content controls,
  compose snippets, saved searches and category tabs exist. Added, along with a test that fails if a
  capability is ever advertised without being documented — or documented after being withdrawn.

- **The backup drill's self-test could pass without testing anything.** It deliberately breaks a
  backup nine different ways and checks that the drill notices each one. But it only checked that
  the drill *failed*, not that it failed for the right reason — so when the build machine's
  container service died mid-run, three broken backups in a row were reported as correctly detected.
  Each check now has to fail for its own specific reason, and the self-test deliberately breaks its
  own environment to confirm it can still tell the difference.

### Added

- **Mailroom now exchanges encryption keys with the people you write to, without either of you doing
  anything.** Two halves of the same idea. Outgoing mail carries your public key, so a
  correspondent's mail app can pick it up and encrypt to you later. Incoming mail is read for the
  sender's key, which is filed against them in your address book. This is the groundwork for
  encrypted mail: Mailroom cannot yet read an encrypted message, and collecting keys now is what
  gives that work something real to be built against.

  Nothing changes for you day to day. The key is public by design — it is what people use to write
  *to* you, never to read your mail.

  ⚠ Only your **designated** addresses advertise a key. If you use a catch-all, its made-up
  aliases carry nothing, because there is no key belonging to them.

  ⚠ Mailroom does **not** yet ask correspondents to encrypt by default. It could — the standard has
  a way of saying "I would rather you encrypted" — but until Mailroom can open an encrypted
  message, saying so would fill your mailbox with mail you cannot read and attachments it withholds.
  Turning that on belongs with the decryption work.

- **`key-discover` looks up your correspondents' published keys.** A new command that asks each
  contact's own mail domain whether it publishes a key for them, and files what it finds. It reports
  what it *would* store and changes nothing until you add `--apply`. Expect it to find very little
  at first: hardly anyone publishes a key, and the report says how many addresses it tried so a
  result of zero can be read properly.

### Fixed

- **A correspondent's mail could not teach Mailroom their key at all.** The place to keep other
  people's keys has existed since the first version of the database and nothing had ever written to
  it — the only code that touched it was the exporter, faithfully exporting an empty table.

### Added

- **A conversation can now be one row in the message list.** Mailroom has always grouped mail into
  conversations, but the list still showed every message separately, so a five-message exchange took
  five rows. Clients can now ask for one row per conversation. This is the server side of it; the
  built-in web client does not use it yet, so nothing looks different in the browser until the
  client work lands.

- **`thread-report` finds conversations that were wrongly split apart, and can repair them.** A
  read-only listing of every message that shares its identity with another while sitting in a
  separate conversation, with `--apply` to merge them back. Needed because the fix below only prevents *new* splits.

### Fixed

- **The same message arriving twice became two conversations.** Every email carries a globally
  unique identifier, so two copies bearing the same one are the same message — but Mailroom only
  looked at which messages *replied to* which, and a copy that replies to nothing matched nothing.
  Most bulk mail replies to nothing. The common way to get two copies is ordinary: mail addressed
  to two of your own aliases arrives twice, differing only in delivery headers.

  So a newsletter delivered to two addresses showed up as two separate conversations. Now it is
  one.

  A sender cannot use this to interfere with your mail. Naming someone else's message identifier
  only ever *adds* the new message to that conversation — it can never remove, move, or displace a
  message already there — and a sender could already join a conversation by replying to it, which
  is how replies work.

- **Signing out of Mailroom now signs you out of your login provider too.** Previously "sign out"
  cleared Mailroom's own session and stopped there, leaving the provider still logged in — so on a
  shared machine the next person could click "Sign in" and walk straight back into your mail with no
  password prompt.

  ⚠ **This needs one setting in your login provider**, or it stops short: the post-logout address
  has to be registered against the Mailroom client, as most providers reject an unregistered one.
  `deploy/README.md` says exactly which address. Where a provider does not support this at all —
  it is an optional feature — signing out stays local, as before.

  The reverse direction is still not covered and is a bigger change: signing out *at the provider*,
  or an administrator disabling an account there, does not end a Mailroom session already in
  progress. The session lifetime (12 hours by default) remains the real limit on that, and an
  operator who cares should shorten it.

## [1.62.0] — 2026-09-04

### Added

- **Documented what a large mailbox costs on upgrade.** `deploy/README.md` now says that the first
  attachment re-check sweep on a big mailbox takes hours, that outbound mail shares the same queue
  and can wait a couple of minutes behind it, and how to tell when it has finished. It also spells
  out the choice available at cutover: importing with the scanner switched off leaves old mail
  visible and unchecked, while importing with it on can withhold a file that hit a momentary
  failure until virus signatures next update.

- **You can now see when an attachment has been withheld, and let it through.** Mailroom has been
  withholding attachments its scanner flagged — or could not check — since v1.61.0, and the only
  way to see why or to override it was a command-line tool. In the reading pane a withheld file now
  says so in place of its usual chip, explains which of the two happened (flagged as malware, or
  *could not be checked* — commonly an encrypted or password-protected file), and offers a Release
  control. Releasing asks first, and says plainly what it does rather than "are you sure?".

### Fixed

- **An attachment that could not be checked was withheld forever on older mail.** Mailroom re-checks
  attachments as virus signatures update, but deliberately stops for mail over six months old — the
  reasoning being that a *clean* answer ages slowly. It was applying that to "we could not check
  this" as well, which is not an ageing answer but a missing one. So a password-protected payslip,
  or anything that hit a momentary scanner failure during a first import, was withheld for the life
  of the mailbox with no way back. Those are now re-checked whatever their age.

- **The refusal message named a control that did not exist.** Trying to download a withheld
  attachment said to "release it in the reading pane" — where there was no such thing. It now points
  at the control that this release actually adds.

- **A sender could switch off the warning about their own spoofing.** Mailroom warns you when a
  message claims one of your own domains but did not come from you. That warning was skipped when
  the message said its authentication had passed — and a message can simply say so: the line that
  reports it is one an ordinary sender can write, and nothing along the way removes it. So the
  warning was optional, for the person it was warning about.

  Mailroom now believes that line only from a mail server you name, via the new
  `MAILROOM_AUTHSERV_IDS` setting (see `deploy/README.md`). Leaving it unset is safe: nothing is
  believed, so the warning appears more often rather than less. There is no way to configure it
  into silence.

- **The "this domain imitates one you know" warning is switched off for now.** It could point at the
  wrong party. Mailroom learns the domains you know from the mail you receive — so a domain
  imitating your bank becomes "known" as soon as it sends you one message, and the warning could
  then land on your *real* bank, describing it as an imitation of the impersonator. A security
  notice that can accuse the innocent is worse than none, so it is off until "a domain you know"
  means more than "one message arrived". The other two sender warnings are unaffected.

- **One unreadable message no longer hides the rest.** Mailroom fetches messages in batches, and a
  single message whose stored copy could not be read failed the whole request — so opening a folder
  could show you nothing at all, with no indication which message was at fault. That is unlikely on
  a mailbox this software created from scratch and close to certain on a decades-old imported one,
  where mail has passed through a dozen programs. The readable messages now arrive as usual and the
  unreadable one is marked as such, rather than vanishing silently.

- **A withheld attachment could still be opened from the browser's cache.** When Mailroom checks an
  attachment and finds nothing wrong, the browser was told it could keep that answer for a year and
  never ask again. But virus signatures update daily, and an attachment that looked fine on Monday
  can be recognised on Thursday — that is the entire reason Mailroom re-checks. From then on the
  browser was serving its own saved copy and never asking whether the answer had changed. Inline
  images in a message are fetched this way the moment you open it, with no clicks at all. Mailroom
  now asks the server each time, cheaply, so a later answer actually arrives.

- **Forwarding a whole message could carry a withheld attachment out with it.** Attaching a single
  withheld file to a new message was refused; forwarding the entire original message, which contains
  the same file, was not — and that puts more on the wire to someone else, not less. Both are now
  refused.

- **A withheld text attachment could still be read through a different request.** Clients can ask for
  the text of every part of a message at once. That path handed back the contents of a text file
  Mailroom was refusing to download. The message body itself is unaffected and still readable.

- **`attachment-report` now counts the parts Mailroom and JMAP disagree about.** A text file attached
  without the usual marker is listed to your mail client as an attachment but was never checked by
  the scanner. The report says how many exist so the fix can be decided on a number.

## [1.61.2] — 2026-09-04

### Fixed

- **A single unreadable attachment could quietly stop attachment re-checking for the whole
  mailbox.** Mailroom re-checks stored attachments as virus signatures are updated, working
  through the oldest messages first, fifty at a time. Some attachments can never be finished —
  an attachment that arrived empty, one whose stored copy will not read, one the scanner chokes
  on — and those were being put straight back at the front of the queue. Fifty of them and the
  queue never moves past them: **the same fifty messages are re-checked every thirty seconds,
  forever, and nothing else is ever re-checked again** — including mail that arrived this week,
  which is where nearly all the value of re-checking is.

  Nothing looked wrong from outside. The job reported a full batch of work done on every pass.
  Empty attachments are common in old mail (the `ATT00001.txt` files that decades-old archives
  are full of), so this was waiting for a real mailbox rather than a rare one.

  Attachments Mailroom has already looked at and cannot finish are now left alone until the
  virus signatures change, so the queue always moves forward.

- **A file that crashes the virus scanner can no longer hold up incoming mail.** The same loop
  meant such a file was handed back to the scanner every thirty seconds; each crash paused
  delivery for the account until the scanner restarted, and then it happened again. Mail was
  never lost — it stays with the mail provider until Mailroom can accept it — but it could stop
  arriving for as long as someone kept sending the file. It is now retried at most once per
  signature update, and Mailroom remembers across restarts.

- **Freshly delivered mail was reported as never scanned, and scanned twice.** A message scanned
  the moment it arrived was not recording *which* signature version had checked it, so the
  health report counted it in the "never scanned" backlog — the one number whose whole job is to
  show a queue that has stopped moving — and the re-check job picked it up again within the day
  to do work that had just been done.

- **The health report could not see the problem it exists to show.** `attachment-report` worked
  out its "due for re-check" figure separately from the job itself, and had never been taught
  about one of the reasons an attachment can be due. It could print zero while the queue was
  stuck forever. It now asks the job, and separates attachments that have no file fingerprint
  *yet* from those that can never have one.

## [1.61.1] — 2026-09-04

### Fixed

- **An attachment held back on older mail could not be released for up to a week.** Releasing
  an attachment is a decision about the *file*, so Mailroom has to know which file — and it
  learns that the next time it re-checks the message. Re-checks are deliberately rationed by how
  old the mail is (recent mail often, older mail weekly), and the identification was riding along
  with them. So an attachment held back on a four-month-old message would stay held with no way
  to let it through until its turn came around.

  ⚠ **Found by deploying it and looking**, not by a test: the first pass after the upgrade
  identified only some of the attachments and stopped, because only the most recent band was due. The
  release path was fine on the mailbox tested — its one held-back attachment was recent — but it would
  not have been on a mailbox with older mail.

  Identification no longer waits its turn. It is a fact about the file that never changes, so
  rationing it by age was answering the wrong question; it now happens once, everywhere, and then
  never again.

## [1.61.0] — 2026-09-04

### Added

- **A flagged attachment is now actually withheld.** For three releases Mailroom has been
  scanning attachments and recording what it found, while handing every one of them over
  unchanged — the answers were written down and nothing acted on them. Now a part the scanner
  flagged, **or could not check**, is refused when something tries to fetch it, and the reason is
  given rather than a broken link.

  It is one check in one place, and that is deliberate: the same route serves the images a
  message displays the moment you open it, the previews the reading pane renders, and anything
  a third-party mail app asks for. Guarding the download button would have missed all three.

  ⚠ **"Could not check" is treated exactly like "found something."** An encrypted archive with
  the password in the message body is an ordinary trick, not an exotic one — if unchecked files
  came through freely, nobody would ever need to defeat the scanner, only sidestep it. The
  wording differs; the outcome does not.

  **The message itself stays readable, and nothing is ever deleted.** The text, the other
  attachments, forwarding, and downloading the raw message all still work — a scanner is the
  part of this system most likely to be wrong about your mail, so the failure mode is "hard to
  reach", never "gone".

- **You can release an attachment you are sure about — and it stays released.** The decision is
  about the *file*, not about the one message you happened to open: the same attachment often
  arrives on a dozen different messages, so releasing it once covers every copy you have, and
  every copy that arrives later. Re-checks keep running and keep reporting what they find; they
  no longer re-lock what you unlocked.

- **Your mail apps are told which attachments are held**, so a client can show it beside the
  attachment instead of offering a download that fails. What syncs is the state only — not how
  old the check is, which would otherwise churn several times a day for answers that had not
  changed.

- **Exports now carry the scan results and your release decisions.** Previously an export copied
  the attachment bytes and nothing about them, so a flagged file arrived on the next system as
  an ordinary attachment with a clean slate. The verdict is not recoverable from the bytes, and
  the release is a decision you made that no re-scan could reproduce.

- **A quarantined attachment cannot be attached to an outgoing message.** Downloading one is a
  choice about your own machine; forwarding it is a choice about somebody else's.

- `mailroom-admin quarantine` lists everything currently held, and can release or revoke by
  hand — read-only unless you ask for a change. `attachment-report` now also shows how much of
  your mail shares attachment content, which is what makes "release the file, not the row"
  measurable rather than asserted.

- **Mailroom now says something when a sender's domain is imitating one you know.** Beside the
  existing note about SPF/DKIM/DMARC — which tells you whether mail really came from the domain
  it claims — the reading pane can now tell you something that question cannot answer: whether
  that domain is one you have ever heard of. A lookalike domain passes authentication
  perfectly. It belongs to the attacker, who set it up like anyone else.

  Three things get a word, and nothing else does:
  - a sending domain that **reads** like one you correspond with (`rnailbox.example` where the
    `rn` is doing the work of an `m`) — and the notice names the real one, because "this is not
    mailbox.example" is something you can check in a second;
  - mail claiming to be **from one of your own domains** that did not prove it;
  - a sending domain written in **an internationalized form**, which is ordinary and legitimate
    and is mentioned only because it is not what it renders as elsewhere.

  ⚠ **Two obvious features were measured against your actual mail and then not built.** Marking
  mail "external" would fire on nearly every message in a catch-all mailbox. And flagging a display name that
  matches a contact whose address it is not — the intuitive impersonation check — fires on a few
  percent of messages and was **legitimate every single time**: it is almost entirely mailing
  services sending on a brand's behalf. A warning that is wrong every time it appears is worse
  than no warning, because it teaches you to ignore the one that matters.

  `mailroom-admin sender-report` prints the firing rate on your own mail, so that stays a number
  rather than a claim.

## [1.60.0] — 2026-09-01

### Added

- **Attachment check results now expire, and get re-checked as the scanner learns.** "Nothing
  found" was never a fact about a file — it was a statement about what the scanner knew at the
  moment it looked, and for genuinely new malware that knowledge is often days behind. So every
  result now records *when* it was made and *what the scanner knew* at the time, and gets remade
  on a schedule: recent mail whenever the definitions change, then daily, then weekly, and after
  six months it stops and shows you how old the answer is instead.

  This is also what finally checks the mail you already had. Attachments that arrived before the
  scanner existed have never been looked at; the first pass works through them.

  ⚠ **Still nothing is blocked.** This release keeps the answers current; the release that acts
  on them is next.

- **Mailroom now tells you if its virus definitions have gone stale**, in the account health
  panel: what the scanner is running, how old its definitions are, how old the oldest unrefreshed
  answer is, and how many attachments sit in each state.

  ⚠ **This is the one that stops a silent failure.** Re-checking is triggered by the definitions
  *changing*. So if updates quietly stop — a rate limit, a full disk, an expired certificate —
  nothing changes, nothing gets re-checked, and **every scan still succeeds**. The system would
  suspend itself and look perfectly healthy while doing it. A scanner that has stopped learning
  is now reported as a fault rather than as silence, and "we cannot tell how old this is" counts
  as a fault too, because it is not the same as "it is fine".

### Fixed

- **The attachment report can now answer the questions it previously had to decline.** It used to
  print that per-tier counts, the oldest result, and the definitions version were *not reportable*
  — honest, because nothing recorded them. They are recorded now, and it prints them, including a
  count of attachments that have never been checked at all.

  ⚠ It also flags a re-check tier that is **empty**, because an empty band cannot confirm that its
  boundary is right — and a row of zeroes reads far too easily as agreement.

## [1.59.1] — 2026-09-01

### Fixed

- **The signature-update container could not start, so virus definitions would never have
  updated.** It was given the same locked-down permissions as the scanner itself, but it does a
  different job — it has to hand its work to an unprivileged user — and it crash-looped instead.
  The scanner would have kept running on the definitions baked into its image and quietly gone
  stale, which is the worst failure this feature has: a scanner with old definitions answers
  "nothing found" and looks perfectly healthy.

  Found by deploying it, not by testing it. The exact permissions were established one at a time
  against a real container until the updater actually fetched an update.

- **Corrected a claim about how long the first start takes.** The notes said the scanner
  downloads ~300 MB before mail can arrive, so the first sync would pause for minutes. Measured
  on a real deployment, it is about seventy seconds: the scanner image already contains a
  signature database, and the updater only fetches the difference afterwards.

## [1.59.0] — 2026-09-01

### Added

- **Attachments are now scanned for malware as mail arrives**, when an operator has set up a
  scanner. Every attachment gets one of four answers recorded against it: checked and nothing
  found, something found, **could not be checked**, or not scanned at all. The scan happens
  before the message is stored, so a message never sits in your mailbox with nothing known
  about the files in it.

  ⚠ **Nothing is blocked yet, and nothing looks different.** This release *records* the
  answer; the release after it acts on the answer by putting a flagged file behind a
  confirmation. Until then you can still download everything exactly as before. Saying so
  plainly matters more than the feature: a half-built guard you believe in is worse than none.

  ⚠ **"Could not be checked" is a real answer, not a shrug**, and it will carry the same
  restriction as "something found" once that restriction exists. A password-protected archive,
  a file too large for the scanner, an enormously nested one, and a PGP-encrypted message all
  land here — because in every one of those cases the scanner would otherwise report "nothing
  found" about bytes it never actually looked inside, and that is exactly what someone sending
  you a malicious file would arrange for. An unchecked file must never wear the same badge as
  a checked one.

  ⚠ **If the scanner stops answering, mail stops arriving until it is back.** That is
  deliberate. Your mail is untouched at your provider, nothing is deleted, and the next
  attempt after the scanner recovers collects all of it — but you will notice the gap, and the
  server tells the operator exactly that rather than blaming storage. The alternative was to
  let mail through carrying a verdict nobody produced.

  One message can never stop your mail on its own. A file that repeatedly crashes the scanner
  is given up on after three attempts and recorded as "could not be checked" — otherwise
  anyone who could send you such a file could switch your mailbox off from a distance.

  Sending is never blocked by the scanner, in any circumstance.

### Fixed

- **The attachment report would have called normal operation a defect.** It flagged any
  attachment state outside "not scanned / clean / found something" as a finding — so the day
  the scanner shipped, every legitimate "could not be checked" would have been reported as a
  fault. It now knows all five states, and says plainly how many attachments are still
  unscanned, because "nothing flagged" and "nothing flagged because nothing was checked" are
  the same number and opposite answers.

- **The attachment report's size column was measuring against the wrong limit.** It assumed the
  scanner refuses anything over 25 MB — the figure in the scanner's own documentation — while
  the version we actually run accepts up to 100 MB. The column was counting perfectly
  scannable files as too big. It is not a number anyone would have questioned, which is why it
  needed measuring rather than reading.

## [1.58.0]
 — 2026-09-01

### Added

- **Groundwork for attachment scanning: Mailroom now keeps a record of each attachment.**
  Before it can tell you whether an attachment is safe, it needs somewhere to write that
  answer down — and until now there was no such record. The database table for it has
  existed since the very first commit and nothing had ever written a single row to it.

  ⚠ **Nothing about your mail looks or behaves differently.** No attachment is scanned yet,
  nothing is blocked, and the record is not visible anywhere in the client. This is the
  shelf, not the thing that goes on it. The scanner itself is the next release.

  Existing mail is not recorded automatically — an operator command fills it in, and
  reports separately on any message that claims to have attachments the parser cannot
  find, because those two things look identical in a total and mean opposite things.

## [1.57.0] — 2026-09-01

### Added

- **A check on whether Mailroom is keeping track of your attachments individually yet.**
  Before it can tell you an attachment is safe, it needs a record of each one — and that
  record has never actually been written. The report now says so plainly instead of
  reporting a reassuring zero, because "nothing to check" and "nothing has been recorded
  yet" are the same number and opposite meanings, and only one of them is good news.

### Security

- **The process that holds your encryption keys can no longer reach the internet.** Mailroom
  keeps PGP private keys in a separate, deliberately minimal process, so that someone who
  breaks into the mail server still cannot get at them. That process was already locked down
  in every other way — it cannot write to its own filesystem, has no privileges, and is
  reachable only through a local socket — but nothing stopped it from opening a connection
  out to the internet, which is the one thing an attacker who did get in would want.

  It, and the database, are now on a network with no route off the machine. They can still
  talk to each other, which is all they need. This is a containment measure rather than a
  fix for a known break-in: nothing is known to have gone wrong, and the point is to bound
  what could. The mail server itself still reaches out, because it has to — that is how your
  mail arrives.

## [1.56.0] — 2026-08-31

### Fixed
- **Mail you sent while signing was enabled was recorded as having no attachments.** Signing
  rewrites the message as it goes out, adding a signature part — but the "has attachments"
  marker had already been decided from what you attached in the composer, and nothing
  updated it afterwards. Nothing was lost and the message itself was correct; the marker
  beside it was not. It is now recalculated from the message as actually sent.

  Messages already stored with the wrong marker are not corrected automatically — an
  operator command repairs them, and tells other devices to redraw.

- **An attachment could disguise what kind of file it was.** A filename is allowed to carry
  invisible characters that reverse the direction the rest of it is read in. One of them
  turns a name like `photo<RLO>gnp.exe` into something your file manager draws as
  `photoexe.png` — a program wearing a picture's extension, chosen by whoever sent you the
  mail. Those characters are now removed from attachment names in all three places they
  mattered: the chip in the reading pane, the name a download is saved under, and the name
  the server puts on the file itself. Your existing mail is unchanged; the names simply
  stop lying about themselves.

- **The attachment report could present a partial count as a complete one.** It tracks three
  kinds of mail it was unable to fully examine, but checked only two of them before
  declaring its totals covered the whole mailbox. The third is mail carrying a forwarded
  message inside it, whose attachments the report genuinely cannot see — so a mailbox of
  forwards would have reported zero attachments and offered that as a finding. It now
  requires all three to be clear before making that claim, and names the one that is not.

### Added
- **A way to see what is actually attached to your mail**, ahead of deciding whether virus
  scanning is worth adding. It reports what attachments claim to be against what their
  bytes look like, how big they are, and how much of the mail it could *not* read — that
  last one because a report which quietly skips what it cannot open would understate
  exactly the thing a scanner exists for.

  It found something on the first run: **messages you sent while signing was enabled are
  recorded as having no attachments, when they do.** Nothing is lost, but the paperclip is
  wrong on those messages, and anything built on that flag would skip them.

- **A way to check a mail provider before trusting it with your mail.** Mailroom works out
  which folder is Drafts, which is Sent and so on from what the server tells it, once, and
  refuses to guess when the server says nothing — because a guessed folder is how mail
  ends up somewhere you never look. The cost of that caution is that a provider which
  doesn't label its folders produces no error when you set it up, and no error while it
  syncs: it produces one failed action, later, the first time you try to save a draft.

  `mailroom-admin mailbox-roles` asks the question in advance. It lists what each account's
  provider actually labels, what stops working for anything missing, and stops with an
  error when something essential is absent — so it can be run as a check before switching
  providers, not just read.

## [1.55.0] — 2026-08-31

### Added
- **A build check that the project stays publishable.** Mailroom is meant to be
  open-sourced, which means nothing in it may name the person running it or the
  machines they run it on. That was a rule people had to remember, and it was
  broken once during the very review that was looking for it. It is now checked on
  every build, two ways: against a private list of identifiers kept
  outside the repository, and against a short list of the external web addresses
  this project has a reason to mention — so a new one has to be added on purpose
  rather than slipping in unnoticed.

### Security
- **Known-vulnerable JavaScript packages can no longer ship.** The Go side of the
  project has failed its build on known vulnerabilities for a long time; the web client
  — which is built into the released image — had no equivalent, so a flagged package
  could be merged and shipped with nothing objecting. Every build now checks, and stops
  on anything rated high or critical. Getting there needed the build toolchain brought
  up three major versions, which cleared all seven outstanding advisories.

- **The key that signs your login is no longer an environment variable.** The session
  key and the OIDC client secret used to sit in `.env`, which becomes the container's
  environment — readable by anyone who can inspect the container or by any process that
  gets inside it. The session key alone is enough to forge a login as any user, without
  a password and without touching the database. Both now live in files alongside the
  other secrets, which the container reads directly.

  **Upgrading requires one step:** move those two values out of `.env` and into
  `deploy/secrets/mailroom_session_key.txt` and
  `deploy/secrets/mailroom_oidc_client_secret.txt`. The old variables are no longer
  read. This cannot go wrong quietly — a configured login provider with either value
  missing refuses to start.

### Fixed
- **The security scanner had been reporting the same false alarm since July.** It was
  flagging a set of numbers printed in the published web-push standard, which the test
  suite quotes on purpose so the implementation is checked against the standard's own
  worked example rather than against itself. They are readable by anyone and protect
  nothing. The report is clean again — which matters less for the one entry than for the
  habit: a warning that is always there teaches people to stop reading the report, and
  the next one might be real.

- **Snoozed mail could be lost permanently, and the recovery built for exactly that case
  could never run.** When a wake-up ran out of retries, the message it was meant to bring
  back stayed hidden for good. Mailroom already had a repair step that runs at every
  start-up and looks for deferred mail with no wake-up scheduled — but scheduling one
  required a name derived from the message's own wake time, and the abandoned wake-up held
  that name forever. So the repair ran on every start-up, found the wake it could not
  schedule, and quietly gave up.

  Nothing reported it. The message still looked snoozed, which reads as working.

  Wake-up names are now reserved only while a wake-up is still outstanding, so an
  abandoned one releases its name and the repair can do its job. The grouping this name
  provides is unchanged: deferring fifty messages to the same time still costs one
  wake-up, not fifty.

- **A forgotten secret file failed in a way the deployment guide got wrong.** The guide
  said a missing secret file stops the whole stack before anything starts. Measured: it
  does not. Docker creates the missing path as a *directory* and everything comes up
  around it, and the one service that needed the secret then fails with "is a directory"
  — safe, but several steps removed from the actual problem, and leaving a directory
  where the file was supposed to go. The guide now says what really happens.

- **Documentation that would have misled someone deploying or recovering.** A sweep of
  every document an operator actually follows:

  - The backup guide's central warning was **wrong**, and taught the wrong lesson. It said
    backing up with the wrong database role silently produces an empty file. Measured on
    PostgreSQL 18: it **fails loudly and stops**. The silent-empty case is real but needs a
    specific flag the backup script never passes. Believing the old version means
    distrusting the wrong thing.
  - The backup and deployment guides assumed **different container names**, so following
    one and then the other failed at the backup step. The guide now tells you to check.
  - Setting the operator's account identifier requires a value only your login provider
    can tell you, and **guessing it wrong locks you out while your mail syncs into an
    account you cannot reach**. There is now a procedure for obtaining it.
  - The project README advertised features as "in progress" for roughly fifty releases
    after they shipped, and the web client's README declared message bodies, attachments,
    reply-all, push notifications and the account-health panel **unimplemented** — all of
    which work. Both now point at the issue tracker instead of a hand-maintained list,
    because a list like that is a claim with no test behind it.

- **Deleting a label left it showing on your other devices, and on the mail server.**
  Removing a label took it off every message it was attached to — but told nobody. Other
  devices were never informed the messages had changed, so they kept displaying the label
  indefinitely, and the copy Mailroom writes to your mail server was never removed either,
  so it stayed there permanently.

  Both now happen: every affected message is reported as changed, and the server-side copy
  is scheduled for removal.

  ⚠ Deleting a label is also **not** treated as disagreeing with the rule that applied it.
  Doing that would have recorded one complaint per message and made a working rule look
  badly broken.

- **The recovery instructions would have destroyed your encryption keys.** Mailroom has
  two root secrets. The setup guide offers to generate both — correct when you are
  installing for the first time, catastrophic when you are rebuilding after a disaster,
  and it said nothing about the difference.

  Getting the first one wrong announces itself immediately: your mail is unreadable. The
  second one says nothing at all. It is only used the first time somebody signs or
  decrypts a message, so a rebuilt server starts up healthy, serves mail normally, and
  reveals the mistake weeks or months later — at which point every stored key is
  permanently unopenable.

  The disaster-recovery procedure only ever told you to install the first one. It now
  installs both, the setup guide warns you not to generate them on a restore, and both
  documents point at the check that actually proves the second one is right — because
  starting up successfully does not.

- **The recovery instructions were also missing the key that opens the backup.** The
  escrow procedure carefully stored two secrets and left out the third: the identity that
  decrypts the backup file itself. Losing it is worse than losing the encryption keys —
  without those you still hold your data and a hard decision, without this you cannot
  open the archive at all. It is now part of what you escrow, with the warning that it
  must not live beside the thing it opens.

- **The restore drill proved that one message came back, not that your mail did.** It
  decrypted a single message directly and otherwise relied on rebuilding the search index
  — which reads every message. But a message whose content could not be read was quietly
  indexed from its headers alone and still counted as done, so a restore that had lost
  almost everything printed a document count and declared the restore alive.

  The rebuild now reports how many messages had no readable content, and the drill fails
  if that number is anything but zero. A new failure injection deletes part of the
  restored content and requires the drill to catch it — deliberately keeping the one
  message the older check reads, so it can only be caught by the new one.

- **The hostile-content browser check no longer fails on release commits.** It had started
  failing specifically on merges to the main branch and on release tags, while passing on
  every pull request — the worst possible pattern for a check that is supposed to block
  bad merges, because it was red exactly when it mattered and green the rest of the time.

  It was not finding a real problem: every security assertion passed each time, including
  the one that proves the server fetches nothing from a hostile message. The browser used
  to render the test page was running out of a small pool of shared memory that Docker
  allocates by default, and a stalled renderer simply never reports back — so the check
  waited, gave up, and reported a height of zero.

  Three things changed: the browser gets a proper allocation, it is told to fall back to
  ordinary disk if that runs short anyway, and pop-up windows the hostile test deliberately
  opens are now closed instead of being left running for the rest of the suite.

  The failure message also now distinguishes "the page never reported" from "the page
  reported the wrong size" — previously both printed the same line, so the log could not
  tell you which had happened.

## [1.54.0] — 2026-08-29

### Fixed
- **Restarting Mailroom while it was working could permanently stop background work.** If
  the process stopped mid-task — a deploy, a reboot, running out of memory — that task was
  left marked "in progress" by a worker that no longer existed, and nothing ever picked it
  up again.

  The damage was not the one lost task. Each of these runs on a chain, where finishing
  schedules the next one, and every restart afterwards looked at the abandoned task and
  concluded the work was already scheduled. So **emptying the Trash stopped happening at
  all** for that account, and with it the reclaiming of disk space — quietly, with nothing
  in the logs and nothing visible in the app. Model training stopped the same way.

  Abandoned work is now picked up again after fifteen minutes.

  ⚠ The code has claimed this behaviour since the beginning — the comment describing it
  was written years before anything implemented it. The column it depended on was being
  written on every task and read by nothing.

- **A first sync of a large mailbox ran the machine out of memory, and could never recover
  from it.** Mailroom asked the mail server for every message at once and held them all in
  memory before saving a single one — roughly twice the mailbox's size in RAM. The design
  documents sizing for archives of tens of gigabytes; those two things could not both be
  true.

  Worse than the crash was what followed it: because nothing had been saved when the crash
  came, the next start began again from the beginning and failed in exactly the same place.
  A mailbox too big to fit could never be synced, however many times you retried.

  Messages are now saved as they arrive. Memory no longer depends on how much mail there
  is, and a sync that is interrupted keeps everything it had already stored.

- **"Not junk" did nothing at all.** Marking a message as not-spam reported success and
  left it in Junk. Nothing errored, nothing was logged, and the message came back spam.

  This is the one spam action the design makes mandatory — it is how you rescue a real
  message the filter got wrong — and it has never worked. Junk state is a column on the
  message, like read and flagged, but the request was being applied to a different list
  where it had never lived: removing something that was not there, successfully.

  Now it clears the verdict **and moves the message to your inbox**, which is also the
  signal your mail provider needs in order to learn.

- **Marking a message *as* spam is now refused rather than silently accepted.** The spam
  verdict belongs to your mail provider and is mirrored here; a local "this is spam" would
  simply be overwritten at the next sync. The refusal says so, and points at the operation
  that does work.

- **The deployment shipped with authentication turned off.** `deploy/.env.example` — the
  file the setup guide tells you to copy — had the OIDC block commented out and the
  no-authentication escape hatch **uncommented**, directly beneath a sentence saying it was
  commented. Copying it and starting the stack, exactly as documented, gave you a mailbox
  anyone who could reach the port could read and send from.

  Two things changed. The example file no longer does that. And the server **refuses to
  start** on the escape hatch alone: turning authentication off now takes a second setting
  that spells out what it does. Being unable to configure a login provider is not the same
  as deciding you want no login, and only one of those should be possible by accident.

- **The health check could not fail.** Docker asked the app whether it was healthy and the
  app always said yes — without checking anything. A real check existed and nothing used
  it. The containers now report unhealthy when the database is unreachable, which is the
  only condition under which the answer was ever going to matter.

- **The web port was published on every network interface.** Putting a TLS terminator in
  front of Mailroom did not stop people reaching it directly beside the terminator, so
  anything the terminator enforced could be walked around. It now publishes to the local
  machine only. **If you reach your instance from another computer, set
  `MAILROOM_HTTP_BIND=0.0.0.0`** — a proxy on the same host needs no change.

- **A slow client could hold a connection open forever.** The public web server set no
  timeouts at all — while the internal crypto process, which listens on a socket nothing
  can reach over a network, set all of them. Header and idle timeouts now bound the
  exposed one. (Streaming push connections are deliberately left unbounded; they are
  supposed to stay open.)

- **Logs had no size limit**, on the same disk as the mail. An idle instance still writes a
  couple of thousand lines a day, and a full disk stops the database. Logs now rotate.

- **A first-ever start could fail for a reason nobody would guess.** While PostgreSQL
  builds its data directory it runs a temporary server that answers on a local socket, and
  the readiness probe accepted that as "ready" — so on a brand-new machine the rest of the
  stack could start against a database that was not actually there yet. Every one of this
  project's own scripts already avoided that trap; the deployment file did not.

### Changed
- `MAILROOM_COOKIE_SECURE` never existed. The deployment guide told operators to set it
  for TLS deployments, and setting it did nothing. Secure cookies are decided by whether
  `MAILROOM_OIDC_REDIRECT_URL` starts with `https://` — one signal, shared with HSTS. The
  guide now says so.
- The deployment guide said PostgreSQL 16; the stack has run 18 since v1.40.1.

## [1.53.0] — 2026-08-29

### Added
- **You can take your encryption key with you.** Until now nothing could get a private
  key out of Mailroom — deliberately, because a way out is also a way for someone else to
  take one. `mailroom-admin pgp-export-wrapped` writes the key to a file, encrypted under
  **a passphrase you choose**. Mailroom does not store that passphrase and cannot work it
  out; nobody here can open the result, which is the whole point.

  Without this, "export my data" hands you your encrypted mail as unreadable ciphertext,
  and deleting your account destroys the only thing that could ever open it.

  The passphrase is read from a **file**, never a command-line option — anything on a
  command line is readable by every other program on the machine for as long as the
  command runs, and this is the one secret protecting the file being written.

- **Accounts can now actually be deleted.** `mailroom-admin delete-user --apply` removes
  a person's mail, files, search index and every row belonging to them.

  **It refuses in more situations than it accepts**, and each refusal is cheaper than
  what it prevents: the account id has to be typed twice, an actor has to be named for
  the record, and — the important one — **it will not destroy a private key that exists
  nowhere else** unless you say so explicitly, having been shown the fingerprint and told
  how to export it first.

- **Files are deleted before database rows, on purpose.** If it dies part-way, what is
  left is rows pointing at missing files — which any later run reports. The other order
  leaves orphaned files that nothing can find.

- **It checks afterwards, with a fresh look.** The delete's own opinion of what it did is
  not the report: a separate pass re-reads the database, the file store and the search
  index, and **exits non-zero if anything of the account remains**. It also says plainly
  that backups are outside all of this — the account comes back from any restore taken
  before the deletion.

- **The export now tells you how to get the key, not just that you haven't got it.** The
  archive has always stated that PGP-encrypted mail inside it is unreadable without the
  private key — but it said so by citing a design document, because when that text was
  written no command existed to produce one. It now names `pgp-export-wrapped`, and says
  plainly that the loss is permanent otherwise. The reason the key is not simply bundled
  in is also stated: the ciphertext and the thing that opens it, in one directory, are
  protected by nothing.

### Fixed
- **A hard delete would have silently done nothing.** The database was arranged so that
  deleting an account removed zero rows *and reported success* — no error, nothing to
  catch it. This was measured on a running system and has been the gate on the
  feature ever since; the missing permission is now in place, and the test that proved
  the old behaviour has been deliberately flipped to prove the new one.

- **The same trap, one table over.** Writing the deletion's audit record was refused for
  the same underlying reason. That one failed loudly rather than silently — but it would
  have failed *after* the files were already gone.

### Verification
- **The record of a deletion no longer erases itself.** The audit entry's link to the
  account is automatically blanked when the account goes, so the one fact it exists to
  preserve — *whose* data was destroyed — was being lost microseconds after being
  written. The identifier is now stored where nothing can null it.

- **The cascade is checked table by table, not by a total.** One number cannot say
  *which* table survived. A second untouched account is checked in the same test and must
  be completely unaffected.

- **Deleting an account that does not exist is an error, not a quiet success** — that
  being precisely what the broken arrangement above used to produce.

- **The key export is checked by breaking it two ways.** Skipping the encryption is
  caught by the export's own artefact check; bypassing *both* that check and the
  encryption is caught by the tests, which then find the plaintext key in the response.

### Changed
- **Pushing a fix no longer leaves the previous test run burning a machine to
  completion**. Every push to a branch started a full matrix while the superseded
  one kept going; on shared runners that is other people's queue wait. Superseded runs are now cancelled. The weekly restore
  drill is deliberately exempt — killing a restore half-way is worse than letting it
  finish.

- **The cleanup that makes that safe was missing, and had to be fixed first.** Every
  script cleaned up on `EXIT` only, and a shell killed with `SIGTERM` — which is exactly
  how a cancelled job dies — never runs an `EXIT` trap. Abandoned database
  containers were found left behind on the runners. Cancelling runs before
  fixing this would have made the leak worse rather than better; twelve scripts now trap
  `INT` and `TERM` as well.

## [1.52.0] — 2026-08-29

### Added
- **Your settings leave with your mail.** The export now also writes your account
  record, contacts, sending identities, filing rules, signatures and templates, saved
  searches, category tabs, remote-image permissions, and your record of what you sent.
  Together with the mail and labels already shipped, the export now writes **every file
  it promises**.

- **Counts are reported for every file, including the empty ones.** A file can come out
  empty, and the export says so rather than omitting the line —
  because "you have no saved searches" and "the query that looks for them is broken"
  produce the same silence otherwise.

- **Everything in the archive is written to be readable by another system.** Nothing
  refers to an internal identifier. A signature attached to one of your addresses names
  the address. A send record names the message by content, so it points at an actual file
  in the archive. A rule that files mail into a label names the label.

### Verification
- **Running against a real mailbox found dangling references the tests could
  not.** Filing rules store the label they apply as an internal identifier, so every
  labelling rule exported an unusable pointer — a rule that says "file this into
  01KYJ2CC0H…" cannot be reconstructed anywhere, because the label it means is
  unfindable. Rules now name the label: `"label": "Shopping"`.

  **The tests missed it because the sample rule had no actions at all** — an empty action
  list cannot leak a reference. The sample now uses a real one, and deliberately
  reverting the fix makes the test fail. A check is only as good as the data it runs
  against.

- **Re-verified on a real mailbox: none of its real identifiers appear anywhere in the
  archive.**

- **A table nothing could read.** Correspondents' public keys were promised in the export
  plan, but no code anywhere read that table — a promise about data nothing could fetch.
  It is often empty, so the gap had never shown. Keys are now carried by
  fingerprint and source, which is what a person actually verifies.

## [1.51.0] — 2026-08-29

### Added
- **Your labels leave with your mail, and they still say who filed them.** The export now
  writes a companion file recording, for every message, which labels it carries — and
  whether *you* filed it or a rule did, naming the rule.

  That distinction is easy to lose and hard to notice missing. Once it is gone, mail you
  sorted by hand looks exactly like mail sorted automatically, and there is no way back.

- **The things an email cannot hold, and a filename has no room for**, now travel too:
  your own custom flags, whether a message was marked as junk, and its priority score
  with the model version that produced it. A message with no score and a message scored
  zero stay distinguishable — they are different facts.

- **The archive is written so a stranger could read it.** Nothing in it refers to
  Mailroom's internal identifiers, which mean nothing outside the installation that made
  them. Labels travel by name, and so does the rule that applied one. Message files are
  named by their own content, so anyone can check a file against its name with a standard
  checksum tool and no other information.

### Verification
- **A check over the whole archive looks for internal identifiers and found a real one.**
  Message files had been named using an internal id, so it appeared in the companion file
  on every line. They are now named by content instead — better in every respect, and the
  archive is now genuinely free of them: **none of the real identifiers taken from a
  live database appear anywhere in it.**

- **The two halves of the archive agree**, checked against a real mailbox: every message has its companion entry, and the export refuses to finish if they ever do not.

- Deliberately breaking the code so that hand-filed and rule-filed labels look the same
  makes the test fail, with the message that the two are no longer distinguishable.

## [1.50.0] — 2026-08-29

### Added
- **You can take your mail with you.** `mailroom-export` writes one account's messages
  to a directory as a maildir — one file per message, byte for byte what arrived.

  **The format was chosen for a reason, and it does more than hold the bytes.** A message's
  identity in Mailroom is a hash of its exact bytes, so an export is only correct if
  nothing is altered in passing. The older `mbox` format cannot promise that: it rewrites
  lines in the body that happen to begin with the word "From", and reversing that depends
  on both ends agreeing about a convention. One changed byte is a different message.

  Maildir also solves the problem that would otherwise need a second file nobody reads:
  **which messages you had already read.** There is no "read" mark in an email itself — it
  is something your mail program remembers. Maildir puts it in the filename, and Mailroom's
  import already understands exactly that notation. So read, replied, flagged and draft
  come back correctly, using machinery that already existed on both sides.

  Verified on a real mailbox: **every message came out identical
  to the copy Mailroom holds**, and the messages marked read were exactly the ones the
  database says are read.

- **A message whose stored copy cannot be read is named, and the export says it failed.**
  It is not skipped quietly and it does not count as exported. The archive carries a
  `manifest.json` recording what is in it, what could not be included, and what this
  format does not carry at all — because the person opening that folder in three years is
  the one who needs to know, and a changelog will not be there.

- **What this first version does not include, stated in the archive itself**: your labels,
  scores, contacts, rules and identities, which come next; and the key that would make
  encrypted mail readable, which does not exist yet by deliberate design.

### Verification
- **The test exports, then reads the archive back with the real import code** and requires
  every message's identity to be unchanged. It does not re-implement a reader — the point
  is that the two halves agree. The sample messages are deliberately awkward: a body line
  beginning "From ", mixed line endings, a last line with no ending at all, and raw
  non-text bytes.

- **Three deliberate breakages, each confirmed to fail the right test**: silently altering
  bytes on the way to disk in a way the exporter's own check could not see (caught by the
  round trip); dropping the read-state letters from filenames; and counting an unreadable
  message as exported.

- **Running it against a real mailbox found a defect no test could.** The first version
  read its database address from a plain environment variable. The real deployment supplies
  it as a mounted secret file, because it contains a password — so the tool could not start
  at all on the only configuration that exists, while every test passed.

## [1.49.0] — 2026-08-28

### Changed
- **Your record of what you sent now leaves with you.** The three tables behind send
  history were grouped together and dropped from the export; reviewing what they actually
  hold showed they are not alike, and they have been split.

  **Send attempts are now carried** — who a message went to, when it was accepted, and
  what the mail server said. Two things live there and nowhere else:

  - **A send that failed or was cancelled leaves no other trace.** The message exports
    looking like an ordinary unsent draft, with nothing recording that it was sent at all,
    or why it did not arrive.
  - **Who a message was addressed to is not always in the message.** A client can name
    recipients directly to the mail server without writing them into any header, and when
    that happens this is the only record of who was written to.

  They are carried as **addresses and outcome, not as internal identifiers** — the ids
  Mailroom uses to stitch its own tables together mean nothing anywhere else, and the
  retry token is a client's private bookkeeping rather than your data.

- **The daily send counter and parsed bounce notices stay behind**, for different reasons
  from each other. The counter is a per-day tally used for rate limiting and says nothing
  about any individual message. A bounce notice **is itself a message** and exports
  normally; the standard puts the original message's identifier inside it, so the link
  back to what bounced can be read from the exported mail rather than being lost.

### Verification
- **An assumption behind the original grouping was checked and turned out to be wrong.**
  The argument for keeping send attempts was that blind-copied recipients live only in the
  send record. They do not — Mailroom deliberately keeps the Bcc line in the stored copy
  of your message and removes it only from the copy that goes out over the wire, which is
  what every mail client does. The export carries the stored copy, so blind recipients
  travel with the mail. The reason for carrying send attempts is the other two, not this.

- **The report now says how many sends ended in anything other than success**, not just
  how many there were. A successful send is already fully described by the message itself;
  only a failed one needs this record to be legible. On a mailbox where no send has failed that number
  is zero, and the report says so plainly — a fact about that mailbox, not about the
  format.

## [1.48.0] — 2026-08-28

### Added
- **A report of exactly what "export my data" would hand you, and what it would leave
  behind — written before the exporter, so the choices are reviewable while they are
  still cheap to change.** `mailroom-admin export-report --user-id <id>` writes no files.

  The specification describes the export in one sentence naming four things: mail, plus
  sidecars for labels, scores and contacts. The database has forty-three tables. That gap
  is the whole problem — an export that carries four tables' worth of your data while
  reporting success is the portability promise failing in the way that is hardest to
  notice, because nothing goes wrong.

  So every table now has an explicit decision attached to it, in one of three kinds:

  - **Carried** — it leaves with you, and the report names the file it lands in.
  - **Not carried** — a deliberate choice, with the reason stated. This covers
    change-feed rows, IMAP coordinates, conversation
    groupings and queued jobs: bookkeeping that describes *this* installation
    rather than your mail.
  - **Withheld** — must never appear in an export at all. Your mail provider password is
    not "something we chose not to include"; it is something that must not be in a file
    that leaves the machine. Keeping that separate from ordinary omissions is the point.

- **A table nobody has decided about is reported as unhandled, not quietly dropped.** The
  table list is read from the live database each run, so a table added next year is
  compared against the plan the day it exists. **This caught a real omission immediately**
  — the account record itself, which has no owner column because it *is* the owner, and
  which the first version of the plan therefore never mentioned. Your name and address
  would have been missing from your own export.

### Verification
- **A measurement in the first run was true and misleading, and auditing it changed the
  report.** It said many messages carry custom mail-client keywords, under a
  heading about state an export would lose. Every one of those keywords turned out to be
  a label Mailroom itself had written to the mail server and read back — the same
  information as your labels, in another form. Exporting labels loses none of it.

  The report now separates the two, and says which it found: in practice, **zero**
  keywords that no label explains. On another mailbox that number could be large, which
  is why both are printed rather than the reassuring one.

- **The report totals what the export would weigh**, counts the messages that
  carry read state, which is genuinely not in the message bytes, and the label
  applications that record *which rule* filed them, checking that none point at a rule that no
  longer exists.

## [1.47.0] — 2026-08-28

### Added
- **Mailroom now records where each of your PGP keys came from, because the answer
  decides whether deleting your account destroys something you can never get back.**

  Two keys can look identical and mean opposite things:

  - You **imported** a key — you still have it. Mailroom's copy is a convenience, and
    losing it costs you nothing.
  - Mailroom **generated** it — the private half has never existed anywhere else.
    Deleting it is permanent, and it reaches backwards: any encrypted mail you had
    already exported becomes unreadable, because an export contains the encrypted
    message and not the key that opens it.

  Nothing recorded which of those was true. `mailroom-admin delete-user` now reports
  them separately, names the fingerprints, and says plainly when a delete would destroy
  the only copy of a key.

- **"We don't know" is reported as its own answer, not folded into the safe one.** Keys
  created before this release have no recorded origin and it cannot be recovered — the
  audit log is empty, and the two paths wrote identical rows. Those are counted and named
  separately, because treating an unknown key as "you have it elsewhere" would be the
  system quietly deciding a permanent deletion is harmless on no evidence.

  This is also why the column had to land now rather than alongside the delete: it only
  works forwards. Every key created without it would have been permanently unclassifiable.

### Changed
- **ADR-024's deferral of `export-wrapped` now has a deadline instead of an open end.**
  The specification always included a way to get an encrypted copy of your own private key
  out; the decision was to not build it until something needed it. Something does. It is
  now a prerequisite for the destructive half of account deletion — and, recorded
  explicitly, it has to be wrapped under **a passphrase you choose**, since a key wrapped
  under Mailroom's own secret would be exportable and still useless to you.

### Verification
- **Measured before deciding, on a real mailbox:** **zero
  PGP-encrypted messages**. Some were *signed*, which is unaffected — signed
  mail is ordinary readable text. So nothing was harmed, which is what makes this a
  precaution taken early rather than a fix shipped late.

- **The wiring is tested by making the two paths disagree.** Generating a key and then
  importing that *same key* produces two rows differing in origin and nothing else — same
  fingerprint, same public key, same private half. Wiring both paths to one value collapses
  them and the test fails. Two separate tests, each checking its own path, would both have
  passed.

- **Three deliberate breakages, each confirmed to fail the right test**: making both paths
  record the same origin; making an unrecorded origin read as "imported" (the exact hazard
  — caught); and removing the database constraint so a misspelled value would be stored.

## [1.46.0] — 2026-08-28

### Added
- **A report of everything "delete my account" would have to remove — before anything
  can remove it.** `mailroom-admin delete-user --user-id <id>` writes nothing and has no
  `--apply`. Deleting an account is the one operation where "it ran and changed nothing"
  is worst, so the measurement ships first and on its own.

  On a real mailbox it accounts for rows across dozens of tables, blob files,
  and a search index of many files. (Those totals drift between
  runs, because mail keeps arriving.)

  Three of those findings were worth having before writing any delete:

  - **The search index is real, and nothing removes it.** It holds body text —
    including the readable text of mail that is encrypted at rest — sitting in a
    directory no code path deletes. Deleting an account through the database alone would
    leave it behind, and the change feed can never clean it up, because deleting the
    account destroys the feed first.
  - **The blob count agrees from both directions:** the files found by walking the
    directory match the rows in the database. The report walks the *directory*, deliberately:
    a sweep driven by database rows is only ever as complete as every writer's
    discipline, and one writer in this project forgot for its entire life.
  - **Only one table in the whole schema is outside a user-scoped delete, and it is not
    a gap** — it is `app.users` itself, the row the delete actually targets.

- **A mistyped account id is refused, not answered.** Asking about an id that does not
  exist reads zero rows from every table, which is indistinguishable from a real account
  that owns nothing. The report says `NO SUCH USER` and exits non-zero rather than
  printing a page of zeroes that reads as "there is nothing here to delete".

### Verification
- **A hard delete written today would report success and delete nothing, and this is
  measured rather than predicted.** `app.users` is owned by `mailroom_owner`, FORCE row
  level security is on, and no policy grants that owner DELETE — so the statement is
  filtered to zero rows and PostgreSQL reports success. A table owner holds DELETE
  implicitly, so there is no permission error to catch it. The report re-derives this
  from the live database on every run and refuses to describe itself as ready while it
  holds.

- **That state is now reproduced in the test suite, on the real table.** The prerequisite
  had been recorded as untestable in CI. It is not: the integration suite applies the
  schema as `mailroom_owner`, exactly as a deployment does, so the tests measure the
  silent no-op — and the one-line fix for it — on `app.users` itself, inside transactions
  that are always rolled back. Adding the missing policy turns **exactly one test of
  1,425 red**, which is the tripwire the destructive half will be judged against.

- **Two defects in this feature were found by deliberately breaking it**, both in code
  that compiled and ran:
  - The first version named **the wrong root table**. Asking which table the account rows
    hang off matched a composite key `(user_id, content_hash)` pointing at `app.blobs`,
    giving two answers where the code assumed one — and it took whichever the database
    returned first. It now requires the key to be the account column alone, and an
    ambiguous answer is *reported* rather than silently resolved.
  - An isolation test could not fail. It compared a number against itself plus a
    quantity already known to be positive. It now gives one account rows the other does
    not have and requires the other's total not to move.

## [1.45.0] — 2026-08-27

### Added
- **Your server fetches remote images now, not your browser. On by default.**
  When you let a message show its images, the request no longer goes from your
  computer to the sender. It goes from Mailroom, and Mailroom passes the picture back.

  What that changes: the sender no longer learns your address, which city or network you
  are reading from, or which browser and language you use. What it does **not** change:
  they still learn the message was opened, because the fetch still happens when you open
  it. Only caching would change that, and caching is not built yet.

  The reading pane says the accurate version of this, and it is deliberately not the
  flattering one -- "the sender still learns you opened it but not where you are or what
  you are reading with." A privacy notice that claimed more than it delivers is worse
  than none.

  ⚠ **Your browser is never given the sender's address at all.** Mailroom does not hand
  over the real link and a proxied one beside it; it hands over only the proxied one. A
  future mistake in the client therefore cannot leak the original, because the original
  is not there to leak.

  It can be turned off with `MAILROOM_IMAGE_PROXY=false`, which restores the previous
  behaviour exactly.

- **Images you have already seen are not fetched again, so re-reading a message tells the
  sender nothing.** This is the one part that hides *when* you opened something -- and
  only from the second open onward, because the first open still has to fetch. A message
  re-read a week later now costs the sender no new information at all.

  Cached images are kept with your mail, encrypted the same way, and an image that
  appears in twenty newsletters is stored once. They are **not** counted against your
  mailbox's storage allowance -- a stranger's pictures must not consume the room your own
  mail needs -- and they are dropped automatically once the cache is full or an image has
  gone unread for a month. Losing one costs a single re-fetch.

### Changed
- **Tracking pixels are still never fetched -- not even by the server.** Proxying a
  beacon would hand the sender the same read receipt from a different address, so
  beacons keep the treatment v1.45's predecessor gave them: an address the reading pane
  has no rule for restoring.
- **Responsive image variants are dropped while the proxy is on.** An image that offers
  several resolutions now loads at its default one. The alternative was to pass the
  sender's own addresses to your browser for the variants, which would have quietly
  defeated the feature for exactly the images it covers. The picture still appears; only
  the sharper variant is lost, and that is visible rather than silent.

### Fixed
- **⚠ A one-off "Show remote content" click loaded nothing, and CI is what found it.**
  Choosing to see one message's images without trusting the sender forever is deliberately
  not remembered -- which was fine while your browser did the fetching, because no server
  involvement was needed. With the proxy, the browser asks the server, and the server had
  no way to know you had clicked, so it refused and the images silently stayed blank.
  The click now tells the server, in a note that expires and is never written down: still
  not a grant, still nothing durable, and still gone when the server restarts.

### Verification
- **Turning the feature off is proved to change nothing.** A test compares the output
  with the proxy disabled against the previous code path, character for character, on
  every shape of message body -- because this is the one piece of code every message
  every reader opens passes through.
- **The three things caching costs are each tested, and each was verified by breaking
  it.** A cached image must not count against your mail allowance (otherwise a stranger's
  pictures could stop you receiving mail); bytes that happen to be *both* a cached image
  and your own mail must be counted as yours; and deleting a message must never take a
  cached image's storage with it. Removing any one of those turns a test red.
- **An end-to-end browser test now checks the stronger claim.** It used to assert that
  allowing images made *the browser* fetch from the sender. That is exactly what this
  release stops, so the assertion was rewritten to check both halves separately: the
  picture still loads, **and** the browser never contacts the sender.
- **A refusal is always a broken image, never a fallback.** There is no path where a
  failed proxy fetch causes your browser to go to the sender instead. That was checked by
  deliberately reintroducing it: the test goes red.
- **Nine deliberate regressions, each confirmed to turn a test red** -- including
  proxying a beacon, keeping the sender's address alongside the proxied one, passing
  responsive variants through, letting a disabled proxy serve anyway, and permitting the
  image type that can carry scripts.
- **Two tests were found unable to see what they claimed to test, and rewritten.** One
  drove malformed requests through a server with no database, so every request was
  refused earlier for an unrelated reason and deleting the rule under test changed
  nothing. The other could not distinguish "switched off" from "never configured",
  because those had been the same state -- the redundant check was deleted rather than
  left as decoration.
- **One user cannot cause a fetch for another user's mail**, proved against a real
  database with a fetcher that fails the test if it is ever reached -- so the refusal is
  the authorization decision and not an incidental network failure.
- **The server refuses to relay a scriptable image type**, and every request it makes is
  bounded in size and time, capped per message, and rate-limited per person.

## [1.44.0] — 2026-08-27

### Added
- **A report that says what an image proxy would actually do here, before one exists.**
  `mailroom-admin proxy-report` measures the images a proxy would fetch -- pictures in
  mail from senders you have already trusted, with tracking beacons excluded because
  those no longer load at all. It writes nothing.

  On a real mailbox the number of distinct hosts a proxy would ever touch was small, and it
  was a surprise -- the design note had been reasoning from the host count of the whole
  mailbox rather than the part a proxy would ever touch.

  Four things the measurement settled, each of which had been an assumption:

  - **Not one image was served over plain `http`.** All were `https`. The design had
    allowed `http` on the grounds that such images are blocked by the browser today and
    only a proxy can render them -- which remains true, and was demonstrably unused in
    the mailbox measured.
  - **Not one URL carries a familiar tracking parameter.** The well-known list
    (`utm_*`, `fbclid`, `gclid`, `mc_eid`, `mkt_tok` and the rest) matches **zero** of
    them. What is actually there is `token-hash` and `token-time` --
    signatures with an expiry, not trackers. Removing those would not clean a URL, it
    would break the picture. That is the reasoning for leaving them alone, now measured
    instead of predicted.
  - **Not one host resolves anywhere private.** The report is explicit that this means
    the new safety check has not been *observed* to refuse real mail, not that it is
    unnecessary -- a sender chooses both the hostname and the address behind it, and can
    change either after any measurement.
  - **Sizes**: most are tiny, almost all are small, and the largest are around a
    megabyte. Those are the numbers a size limit and a cache
    budget should come from.

- **The proxy gets its own, stricter rule for whose images it will fetch.** The existing
  rule deliberately copies the mail client's, because it answers "what would the client
  do". A server deciding whether to make a request is a different question, and on one
  input the copied answer is wrong: a sender whose address is written
  `attacker@evil.example@trusted.example` reads, under the client's rule, as
  *trusted.example*. The proxy's rule refuses anything it cannot read as exactly one
  ordinary address. **On the mailbox measured, the stricter rule refused nothing the client would
  have loaded** -- the report prints that gap precisely so that stays true.

### Verification
- **Fetching is off by default, and the report says why.** Contacting every sender to
  measure image sizes tells all of them this mailbox is live, for the whole archive at
  once, including mail never opened -- the harm this work exists to reduce. Both
  optional measurements print **NOT MEASURED** rather than a zero when they are skipped,
  because an absent row that looks like a finding is worse than no row.
- **Some fetches failed, and the report says which host.** All were the one host
  whose URLs carry an expiry parameter, which is what a signed, time-limited URL looks
  like from the outside. An unattributed count of failures would have read as a fault in
  the fetching; attributed, it is a fact about what the eventual endpoint must do when a
  fetch legitimately fails.
- **Probing rewrote the matching rule twice.** A redundant guard was deleted after
  probing showed no test could tell it from the check beside it, and two functions that
  each located the `@` independently were merged into one -- breaking either alone left
  every test green, because the two then disagreed and the disagreement happened to fail
  safe. It was right by accident. Three further tests turned out unable to observe the
  thing they named and were rewritten until they could.
- **A test asserts a tracking token never reaches the report.** Parameter *names* are
  counted and printed; a parameter *value* identifies the reader, so the test plants one
  and fails if it appears in any table -- while also checking the name beside it was
  still counted, or it would pass for a report that quietly discarded the URL entirely.

## [1.43.0] — 2026-08-27

### Fixed
- **A device could be registered at an address inside the server, and the server would
  send to it.** Turning on new-mail notifications hands Mailroom a web address to deliver
  them to, which your browser supplies. Mailroom accepted whatever it was given, checking
  only that it was not empty -- not that it was on the public internet, not that it was a
  web address at all. So an account holder could register a "device" at the server's own
  loopback interface, at another machine on the same private network, or at the address
  cloud hosts use to hand out credentials, and every arriving message would make the
  server send a request there.

  Nothing came back: the reply is thrown away, so this could not be used to read anything
  directly. What it could do is **reach things only the server can reach** -- which is the
  part of a private network that is usually defended by being unreachable.

  Registering a device now refuses an address that is not `https`, is not a web address,
  or points somewhere that is not publicly routable, and says which. Ordinary
  registrations are unaffected. This was found while designing the image proxy, not
  reported from outside, and it had been there since notifications shipped.

### Added
- **Every outbound request the server makes now leaves through one guarded door.** New-mail
  notifications are the first thing to use it and the image proxy will be the second, so
  the guard is shared infrastructure rather than a part of either.

  It refuses anything but the schemes its caller permits; it looks up the address, decides
  about **that** address, and then connects to the one it decided about, so a name cannot
  resolve to something harmless during the check and something internal a moment later; it
  refuses a name that offers even one non-public address rather than picking a different
  one; it re-checks every redirect and caps how many it will follow; and it bounds how long
  a response may take and how large it may be. Nothing about the reader travels with the
  request -- no cookies, no referrer, a fixed neutral identifier.

  Notifications and the future image proxy deliberately get **different** permissions: a
  notification address must be `https`, while the proxy will also fetch plain `http`,
  because an `http` image in a message is content your browser refuses to load on a secure
  page and the proxy is the only way it ever appears. See ADR-026.

### Verification
- **Every test here was checked by breaking the thing it tests.** Fifteen deliberate
  regressions were introduced one at a time -- dialling the name instead of the checked
  address, checking only the first address a name offers, skipping the redirect re-check,
  turning an oversized response into a silent truncation, restoring the request's
  referrer, allowing a proxy, removing the validation entirely -- and each one turned a
  test red. A suite that passes on the first run has not yet been shown to be capable of
  failing.
- **The refusals are proved individually and the permissions are proved too.** Every
  refused range is named and tested, including the two encodings that let an internal
  address wear an external costume and the addresses carrying a network zone, which
  matched nothing at all until that was handled. Public addresses on both sides of each
  boundary are tested as *allowed*, because a guard that refuses everything and a guard
  that works are indistinguishable if only refusals are checked.
- **The old behaviour was reproduced.** With the new validation removed, all nine hostile
  registrations in the test -- the metadata service, loopback on both address families,
  a private network address, plain `http`, `file:`, `gopher:`, and a string that is not a
  web address -- were accepted, which is what the server did before this release.

## [1.42.0] — 2026-08-26

### Added
- **Tracking pixels no longer load, even for senders you have chosen to trust.** A
  tracking pixel is an invisible image -- one pixel by one pixel -- whose only purpose is
  to tell the sender you opened their message. Mailroom already refused to load any
  remote image unless asked; what v1.41.0 measured is what happens once you do ask.
  On a real mailbox, **every single message from an allowed sender carried one**.
  Saying "always show images from this sender" was also, silently,
  agreeing to be counted every time they wrote to you.

  Now the pictures load and the beacons do not. The reading pane says so, beside the
  line that already explains why images are loading, because withholding part of what
  someone allowed without telling them is the thing that line exists to avoid.

  **The rule is deliberately narrow: exactly one pixel by one, and nothing else.** The
  measurement compared four candidate definitions, and this one accounted for every
  affected message on its own -- so a wider rule could only have added mistakes. The
  near misses matter: a one-pixel-wide, six-hundred-pixel-tall image is a divider line,
  and an image sized in percentages is a responsive picture. Both are ordinary mail, and
  suppressing either would quietly delete something a reader was meant to see.

### Verification
- **The suppression is structural rather than a decision.** The server files a beacon's
  address under a label the reading pane simply has no rule for restoring, so there is no
  branch that can be written the wrong way and no state it can be in where a beacon
  loads. A test asserts the two families of label cannot be confused for one another.
- **Proved in a real browser, and the proof needed two halves.** The test message now
  carries a normal image beside the beacon: opening it with images allowed fetches the
  picture and does not fetch the beacon. Either half alone would be worthless -- "the
  beacon was not fetched" is also true when nothing works at all.
- **The near misses are pinned by test**, including one that had to be corrected: the
  case meant to prove a percentage size is not a pixel count had been written with
  numbers that could not tell the two behaviours apart, and passed either way. Found by
  deliberately breaking the code and noticing the test did not object.

## [1.41.0] — 2026-08-26

### Added
- **`mailroom-admin pixel-report` — how much of this mailbox can actually be tracked,
  and by whom.** Mail is full of tracking pixels: invisible images whose only purpose is
  to tell the sender you opened the message. Mailroom already refuses to load any remote
  image unless asked, so almost all of them never fire. The question nobody had measured
  is what happens for the senders a person has chosen to trust.

  ⚠ **On a real mailbox the answer was unanimous: every single message from an
  allowed sender carried a beacon.** Remembered "always show images
  from this sender" decisions had opened a channel that fires on every message those
  senders send, about two beacons per message. Some of the hosts being contacted exist
  only to count opens, not to serve content.

  The report counts each candidate definition of "tracking pixel" **separately** — exactly
  1×1, merely small, hidden by styling, or declaring no size at all — because they select
  very different sets of mail and picking one first would produce a number the rule had
  been fitted to. Read side by side, they disagree usefully: the strict 1×1 rule accounts
  for the entire exposed population on its own, while "merely small" catches spacers and
  dividers and found exactly one relevant message.

  **Read-only.** It changes nothing and proposes nothing; what to do about the finding is
  a separate decision, taken after reading it.

### Verification
- **It measures the filter that actually runs**, not a second copy of it: the counts come
  from the real sanitizer as it processes each message, and a test pins that instrumented
  path to produce byte-identical output to the shipped one. A separate walk of the HTML
  would have been code written by the same hand to agree with the same assumptions.
- **The report states its own bounds.** Only images can be restored at all — every other
  way a message might fetch something is discarded outright rather than held back, so the
  count is complete over what can fetch rather than a sample of something larger. It also
  reports messages it could not read, and forwarded mail it did not look inside, so a
  clean result cannot come from having quietly skipped something.
- **Thirteen new tests, each one probed by breaking the thing it covers and confirming it
  goes red.** ⚠ That probing found a genuine hole in one of them: a case meant to prove
  an unrecognised permission type is ignored had been written so that it passed whether
  or not the type was ever checked. It now uses a value that would match if the check
  were missing, so a future permission type cannot quietly come to mean "everything".

## [1.40.1] — 2026-08-26

### Fixed
- **PostgreSQL 18 could not actually start on the deployed configuration**, and v1.40.0
  shipped without anyone noticing. The official image changed where 18 keeps its data:
  it now uses a directory named after the major version, so that future upgrades can be
  done in place. A stored-data mount pointing at the old location is therefore no longer
  the data directory at all, and the database refuses to start rather than quietly
  writing somewhere that will not survive a restart. The mount now points one level up,
  which is what the image expects.

  ⚠ **Nothing in the test suite could have caught this, and that is the useful part.**
  Every database check — schema, migrations, the integration suite, all four end-to-end
  harnesses, and the restore drill — builds a temporary database with no persistent
  storage attached. All of them passed on 18. The single configuration that has
  persistent storage is the deployed one, and it is the only one that could fail. It did,
  on the first boot after the upgrade, which is exactly where the upgrade was expected to
  be risky and exactly why it was done as a deliberate operation with a verified backup
  rather than as a merged one-line version bump.

  The fix was proved before shipping, in both directions: the old mount reproduces the
  refusal, the new one starts, and data written to it survives the container being
  destroyed and recreated.

## [1.40.0] — 2026-08-26

### Changed
- **Build and CI toolchain moved to Go 1.27**, and the checkout step CI uses moved to
  its v7 line. No behaviour change and nothing user-visible — recorded because
  dependency bumps arrive without changelog entries of their own, and a release that
  silently changed the compiler it was built with would be worse than one that says so.
  The module's declared minimum stays **Go 1.26**: a newer toolchain builds it
  forward-compatibly, and raising the floor would be a separate decision with nothing
  behind it.
- **The database moved to PostgreSQL 18.** A one-line bump proposed by the dependency
  bot was declined first, and it is worth saying why: it would have changed the database
  image the running instance uses, and a major version cannot start on the previous
  version's data directory. Every automated check would have passed anyway — they all
  build an empty, throwaway database, where the new version is perfectly happy — so the
  failure would have appeared only at the next deploy, to whoever was doing it. A version
  bump that is one line in a diff is, here, a data migration.

  So it was done as one instead. Every place a version is named moved together, because
  a partial bump means the automated checks prove a different database than the one
  actually running. The reasoning, and what it deliberately does *not* claim, is recorded
  in ADR-025 — briefly: nothing was failing, PostgreSQL 16 is supported until late 2028,
  and the argument is simply that this is the cheapest this upgrade will ever be. The
  upgrade is cheap on a small database with a couple of users. The same move against a large
  database with real people using it is a maintenance window.

  The stated *minimum* stays PostgreSQL 16 — it is a floor, not a pin, and the schema
  still applies cleanly to it. Comments in already-applied migrations that record what
  was measured on 16 were left alone rather than rewritten, because they are a record of
  what was observed and not a search-and-replace target.

### Verification
- **The upgrade was proved on a copy of the real database before anything was touched.**
  A dump of the live PostgreSQL 16 was restored into 18 and compared: message, blob,
  user, label and change-history counts all identical, along with the schema version,
  all 51 row-level-security policies, and all 42 tables that force it.
- ⚠ **That the *data* survives says nothing about whether the *protections* did**, so
  that was checked separately and with a control: on the restored copy, a connection
  with no user identity set saw **zero** messages, the same connection **with** an
  identity saw all of them, and an administrative connection saw all of them. Without the
  middle number, "zero" would read the same whether the protection held or the table was
  simply empty.
- **The restore drill could not have caught a problem here, and now can.** It used one
  setting for both the database it backs up from and the one it restores into, so it
  could only ever rehearse a same-version restore — never the cross-version one that a
  real disaster, or this upgrade, actually performs. It now takes the two separately, was
  run 16 → 18 end to end, and was then deliberately sabotaged to confirm it still fails
  when it should.

## [1.39.0] — 2026-08-26

### Added
- **Mail that failed a sender check now says so.** Every message carries a record of
  what the receiving servers concluded about whether it really came from the address
  it names — SPF, DKIM and DMARC. Mailroom has always read that record, but kept only
  a single conservative yes/no from it, and v1.38.0's measurement showed what that
  costs: the flag read "nothing wrong" across an entire real mailbox while
  some messages failed a signature check outright, some could not be evaluated at all,
  and some carried an explicit "no policy". The reading pane now shows a short
  warning on exactly those messages, saying which check and what the reader can do.

  **There is deliberately no "verified" badge, and its absence is the design.** A green
  mark would appear on nearly all real mail. A mark that common stops being read — which
  makes it least visible at the one moment it would have mattered, the moment it
  disappears. So good mail says nothing at all, and the notice is reserved for the
  small fraction with something worth saying.

  **Your own mail is never flagged, and that is measured rather than assumed.** A draft
  or a copy of something you sent never passes through an authenticating server, so it
  has no such record by construction — most headerless messages on a typical
  mailbox are exactly this. Telling someone their own outbox looks unverified
  would be worse than saying nothing, so those are distinguished from mail that genuinely
  arrived unauthenticated, which still is reported.

  Three things are separated that a single flag cannot tell apart, because they license
  different claims: a check that **ran and failed**, a check that **could not complete**,
  and **no authentication at all**. A message carrying two signatures where one is valid
  and one is not is ordinary rather than suspicious, and is not reported.

  What is *not* reported: a sending domain that publishes no such policy. That is a fact
  about the sender's configuration rather than evidence about the message, nothing can be
  done about it, and it would more than double the notice rate for nothing.

### Changed
- **The per-method results are read from the message itself rather than stored.** They
  cost no extra work: the reading pane is the only place they appear, and it already
  reads the full message. Keeping a separate copy would have bought filtering that
  nothing asks for yet, in exchange for a record that can drift from the message it
  describes — a drift the v1.38.0 report exists partly to watch for.

### Verification
- **Every new test was probed by breaking the thing it covers and confirming it goes
  red** — eleven of them, each recorded next to the assertion it justifies. Two are
  worth naming because they cover the failure this project keeps paying for, a read
  path that compiles, renders, and is silently empty:
  - the six tests of the rendering logic would **all** have passed if the message had
    never actually been read for this record, so a further test asserts it against real
    message bytes, and fails alone when that wiring is removed;
  - and nothing above can tell whether a message was written here or arrived, so that
    is asserted in both directions against a real database — a value that is always
    "ours" and one that is never "ours" are equally broken, and a one-sided test cannot
    distinguish either from correct.
- **The rule for what counts as a failed check is written once and matched exactly on
  both sides.** A check that reports both a pass and a failure — ordinary for mail
  carrying two signatures — is not a failure. No such message turned up in the mailbox
  measured, so this guards a case the live numbers could not demonstrate,
  which is precisely why it is pinned by a test rather than left to be noticed later.

## [1.38.0] — 2026-08-26

### Added
- **`mailroom-admin auth-report` — what SPF, DKIM and DMARC actually say about a
  mailbox.** Mailroom has always parsed the `Authentication-Results` header that
  receiving servers stamp on mail, but only ever kept one conservative yes/no bit
  from it, and that bit read "no" on every message in the mailbox first measured. A
  zero like that cannot be read on its own — *"nothing here failed"* and *"the
  check never runs"* produce exactly the same number. The report exists to tell
  those apart before anything is built on top of the signal.

  It counts **per method and per result**, never one total, and separates the three
  populations a single bit cannot represent: mail carrying **no such header at
  all**, mail where one method **both passes and fails** (ordinary for a message
  carrying two DKIM signatures), and the in-between results — `none`, `neutral`,
  `softfail`, `permerror`, `temperror` — which are neither a pass nor a failure and
  must not be filed as either.

  Messages with no header are split by **whether Mailroom wrote them**. A draft or
  a Sent copy never crosses an authenticating hop, so it has no such header by
  construction; it is not unverified mail, and anything that later flags it as such
  would be wrong. The report says how many rather than leaving it to be inferred,
  and draws its example senders only from mail that genuinely arrived from outside.

  It reads the **stored** bit alongside its own fresh parse of the same message and
  reports any message where the two disagree, because that is a defect in one of
  them rather than a statistic.

  **Read-only.** There is no `--apply`; it writes nothing and proposes nothing.

### Changed
- **One parser, one answer.** The stored yes/no signal is now *defined in terms of*
  the per-method parse rather than being a second reading of the same header, so a
  report and the classifier can no longer disagree about what a message says. The
  documented rule it implements is unchanged.

### Verification
- **Positive controls first, because the whole value of this report is whether its
  zeros mean anything.** The parser is shown to fire on every method paired with
  every result keyword the standard puts on the wire, and to separate the absent,
  disagreeing and in-between populations — then probed: removing DMARC from the
  parser reddens 77 assertions, and forcing the "header present" answer reddens the
  absent case. A report whose expected output is zero has shipped here before from
  a filter that made zero the only possible answer; this one has been shown capable
  of producing something else.
- **Behaviour preservation is asserted, not assumed.** The refactor above touches a
  signal that feeds the message classifier, so beyond the existing cases there is
  now an exhaustive check that the verdict matches the documented rule across all
  125 combinations of three methods × five results, through both entry points.
- **The tests read the report's output, not its counters.** ⚠ Twice in one day a
  report in this package computed a figure correctly, added it up correctly, and
  printed it nowhere — nothing errored, and both runs looked complete. A test on
  the internal counts would have passed through both. The report now prints through
  a writer a test can capture, and four tests assert what it actually *says*:
  that the absent split appears and its examples exclude locally authored senders;
  that a DKIM-only failure is visible even when the overall verdict is clean; that a
  stored-versus-live disagreement is announced rather than left for a reader to
  spot; and that a corpus with no such headers at all reports that bound instead of
  letting a column of zeros read as passes. Probed by deleting the two split lines
  and confirming the test names the missing strings.

## [1.37.0] — 2026-08-25

### Changed
- **Style rules written with `>` now render.** The child combinator — `td > p`, how a
  stylesheet says "only the paragraphs directly inside a cell" — was being thrown away,
  taking its rule with it. On a real mailbox that was thousands of rules, the single
  largest structural loss left in the styling filter.

  It was never really about `>`. A `<style>` block ends at `</style` and nothing else, so
  a lone `>` inside one is ordinary text — but the check that guarded against a message
  breaking out of that block refused *both* angle brackets, which forced everything
  upstream to refuse `>` as well. The guard now refuses only the character that can
  actually end the block, which is both narrower and exactly sufficient.

  **Thousands of rules that were being discarded now survive.** Whole style blocks are barely
  affected, and that is worth stating rather than rounding up:
  most of the recovered rules live inside blocks that were surviving anyway. The win is
  rules, not blocks.

### Verification
- **The guarantee is now four independent mechanisms, and that was measured rather than
  assumed.** For a message to break out of a style block, all four would have to fail
  together: an escaped `<` always becomes part of a name and is refused there; a literal
  one never reaches the styling filter, because the page parser stops at it first;
  separate pieces cannot be fused together, since a removed comment leaves a space behind;
  and the final guard drops any block that still contains one.

  ⚠ Two things were wrong before probing found them, and both are recorded because the
  second is the more important:

  The new test could not detect the failure it existed to detect. Deliberately
  reintroducing the defect left it green, because none of its inputs had the one shape
  that actually escapes. Fixed, and now red under that defect.

  The end-to-end browser check **claims less than it first appeared to**. Reintroducing
  the defect leaves it passing too — because the other three mechanisms still hold. It is
  honest regression cover for all four failing at once, and its own comment now says so
  instead of implying a proof it does not provide. The checks that genuinely fail when
  this breaks are the unit tests.

## [1.36.0] — 2026-08-25

### Changed
- **Modern mail layout renders again.** v1.35.0's measurement found that the styling
  Mailroom recognises was written before flexbox became how email is laid out, so
  messages using it were being flattened: many lost `align-items` or
  `gap`, and so on through the whole family. Fifty-two properties are now recognised,
  every one of them measured as actually appearing in real mail, and every one of them
  incapable of naming an address — they take keywords, lengths, numbers and colours and
  nothing else. Nothing about what Mailroom refuses to fetch has changed.

  In measurement this cut the styling rules lost to unrecognised properties by a
  quarter, and cut the `<style>` blocks lost entirely for that reason
  by a third.

  Properties that *can* name an address — the ones a tracking pixel or a remote font
  would arrive through — are unchanged and stay refused, as do the ones excluded
  because absolute positioning lets a message show a person one thing and a scanner
  another. Both sets, and every other candidate the measurement turned up, are now
  recorded with the reason they were declined, so the next person does not have to
  work it out again.

### Verification
- **The rule governing what may be recognised is now partly enforced rather than
  merely written down.** It said, in a comment, that a property may be recognised only
  if it cannot name an external resource — and nothing checked that. One new test walks
  the list itself and confirms no recognised property can emit an address in any of
  thirteen spellings, including the escaped form that caused the original defect; a
  second confirms the specific properties documented as forbidden are absent.

  ⚠ Recorded because it nearly shipped as a false comfort: the **first** version of
  that test claimed to enforce the rule and did not. It passed with two forbidden
  properties deliberately added, because it was really testing a different layer of the
  filter — one that refuses addresses for every property, recognised or not. It was
  rewritten into two tests that each state exactly what they establish, and both say
  plainly that neither can decide whether a *newly proposed* property is safe. That
  judgement remains human, and a green suite is not permission.

## [1.35.0] — 2026-08-25

### Added
- **`mailroom-admin css-report` — what the styling filter actually removes from your
  mail.** v1.34.4 changed how Mailroom handles the styling inside a message: instead of
  looking for dangerous patterns, it now keeps only styling it recognises by name. That
  is safe however short the list of recognised names is — but whether the list is *long
  enough* is a completely different question, and it had never been asked. A list that
  is too short does not fail; it quietly renders newsletters wrong.

  The report answers it against your own mail. It counts, for every styling property,
  how many declarations were removed and — separately — how many distinct messages were
  affected, because one template received two hundred times and two hundred different
  senders are the same total and mean opposite things. It also separates two outcomes
  that a single "removed" figure would merge: a property that is not recognised at all,
  and a recognised property whose *value* was refused. Those have different fixes.

  When a message's whole `<style>` block is removed, the report says which of the two
  causes did it — counted per block rather than inferred from totals, which cannot
  attribute anything.

  It is read-only and has no apply mode; it changes nothing and proposes nothing.
  Deciding whether the list of recognised properties should grow is a separate
  judgement made after reading it, deliberately in that order.

  **The numbers come from the real filter, not a copy of it.** The counters are threaded
  through the same code that renders your mail, and a test requires the instrumented
  path to produce byte-identical output to the ordinary one. A report that re-did the
  work its own way would be measuring a second implementation written to agree with the
  same conclusion.

### Fixed
- **Two defects in that report, found by running it and reading the output.** It first
  claimed 390,552 malformed declarations — more than every other category combined, and
  the first figure anyone would look at. They were not declarations at all: an ordinary
  `style="color:red;"` ends with a semicolon, and the empty space after it was being
  counted as a broken rule. The real figure is 295. It also labelled one category of
  refused text with an internal number instead of a name.

  Neither affected rendering. Both would have made the report's most prominent rows
  describe something other than the mail.

- **A third: the per-block attribution was calculated and never displayed.** The figures
  the report exists to produce were computed correctly, summed correctly, and printed
  nowhere. Nothing errored, and the run looked complete.

## [1.34.8] — 2026-08-25

### Fixed
- **The new storage report attributed its findings to the wrong person.** Running
  v1.34.7's `backfill-model-blobs` against a real instance for the first time showed
  the problem immediately: with the detailed listing switched on, each person's items
  were printed *before* their own summary line, so on an instance with more than one
  account every item appeared directly beneath the previous person's row. Five stored
  models belonging to one account rendered underneath an account that has none.

  None of the numbers were wrong, and the fix is small. It is recorded as a fix rather
  than a tidy-up because a report whose entire purpose is to say which stored data
  belongs to whom, and which is read precisely when someone is deciding whether to
  delete something, does not get to be ambiguous about that. The sibling report already
  had the order right; both are now checked by a test, including one that pins each
  number to the column its heading names.

## [1.34.7] — 2026-08-25

The storage reconciliation added in v1.34.6 was run against real data for the first
time and immediately found something nobody had looked for: a few files on disk that
no record pointed at. They turned out not to be leftovers at all. They are where
Mailroom keeps what it has learned about your mail, and it has never recorded them
properly — which meant a protection added one release earlier, for exactly these
files, had never been able to do anything.

### Fixed
- **What Mailroom learns about your mail was being stored in a way nothing could
  account for.** Each time it updates its model of what matters to you it writes a
  small file and notes which version is current — but it never added that file to the
  index of stored data. Nothing broke, which is why this lasted from the very first
  version: reading a model back finds it by its content, and never consults the index.

  Three things were quietly untrue in the meantime. The files accumulated, one per
  distinct version, with nothing able to see them or clean them up. The safeguard
  added in v1.34.5 to stop a mailbox cleanup from deleting a model's file could never
  have fired, because it protects an index entry that was never created — a check that
  could not fail, added in the release whose whole subject was checks that cannot
  fail. And a future "delete my account and everything in it" would have walked
  straight past every model a person owns.

  New models are now recorded properly, and `mailroom-admin backfill-model-blobs`
  records the ones already stored. Like every other maintenance command it reports by
  default and changes nothing without `--apply`. It counts each outcome separately —
  files it can record, files that have gone missing, files it cannot read — because
  those are three different problems and only the first is the one being fixed.

- **Trimming old models could have destroyed a message.** Mailroom keeps a handful of
  recent versions and deletes the rest. Deciding what to delete asked only whether
  another model still needed those bytes, and never whether a *message* did. Because
  stored items are identified by their content, a message whose bytes happened to be
  identical to a stored model would have had its content deleted while every record
  still said it was there — the message would list normally and fail to open.

  This is the exact hazard v1.34.5 fixed in the opposite direction, where a mailbox
  cleanup could delete a model. That release rejected "it has never happened" as a
  guarantee; the same standard applies here. Both directions now ask one shared
  question, and the deletion happens in the same step that removes the record, so a
  failure means nothing was deleted and it is retried rather than leaving a record
  pointing at bytes that are gone.

### Changed
- **Stored models now count toward your storage usage.** They are real disk and always
  were; they were simply invisible to the number. It is a few kilobytes per version.
  Learning is deliberately *not* blocked when you are near the limit — failing to
  record a model that has already been written would be a worse outcome than being
  slightly over.

### Verification
- **Two test fixtures were describing a situation the software could not produce.**
  The v1.34.5 safeguard's test set up a stored model *and* an index entry for it, and
  passed. Both halves were real; the combination was not, because nothing ever created
  that index entry for a model. The test could observe its own mechanism breaking — it
  was not vacuous — but passing it said nothing about whether any actual model was
  protected. It, and the model-trimming test beside it, are now built the way the
  software genuinely stores things.

  Each new and reseeded test was checked by removing the mechanism it covers and
  confirming it goes red. That pass also showed the previous trimming test staying
  green with the defect fully present, which is why none of this was noticed.

### Corrected
- Comments in three files described the arrangement that was replaced, and one of them
  was the reason this went unnoticed for so long: the model registry stated as a
  deliberate design that models are recorded *only* alongside their version and not in
  the storage index, while the cleanup safeguard added later assumed the opposite. Two
  decisions in two files, each coherent on its own, describing incompatible worlds.
  Both now describe the same one, with the disagreement written down rather than
  resolved silently.

## [1.34.6] — 2026-08-22

The v1.34.5 audit wave fixed every headline finding it named and quietly dropped
seven trailing ones. This is those seven, found by re-reading each audit issue
against the code rather than against the commit messages. Nothing here was
user-visibly broken; most of it is the difference between a protection that
holds and a protection that can be *shown* to hold.

### Fixed
- **A test that claimed to guard the suggestion report could never fail.** It
  compared two constants — `1 <= 0` — so it read no data and exercised nothing,
  while being cited elsewhere as evidence the command was safe. The bar it was
  supposed to protect had no test at all, and re-introducing the original defect
  left the whole suite green. The report now uses the same filter as its sibling
  command, which does have real tests, and two new tests fail if either the
  filter starts reading the field that made the report vacuous or the report
  grows a second copy of the bar. Both were confirmed to go red before being
  kept.

### Changed
- **Job records now state that they carry references, never message content.**
  The dispatcher reads due work through a mechanism that necessarily crosses
  user boundaries, so what a job is allowed to carry is a security boundary. It
  was true in practice and written down nowhere; a future job type that embedded
  a subject line or a body would have turned that mechanism into a way for one
  user's mail to reach another. There is now a stated contract, a list of what
  each job type may carry, and two checks against it: one that proves the rule
  itself rejects content, and one that holds the real table to it.

- **The disaster-recovery report can now find storage that leaks.** v1.34.5
  stopped three delete paths from abandoning stored mail, but nothing could
  report the leftovers already there — which is why four of them sat unnoticed.
  `mailroom-admin blob-report` reconciles records against files in both
  directions and says which kind of problem each one is: a record with no file
  is a message that will fail to open, a file with no record is disk that never
  comes back. It also attributes leftovers to the operation that produced them.
  It is read-only by design and has no apply mode.

### Changed
- **The code linter is now a fixed version rather than whatever is newest.** It
  was installed fresh on every run, so a new release could turn the build red
  without anyone changing a line — which happened, on a change that had nothing
  to do with it, while the main branch had already been failing for days with no
  way to notice. Upgrades now arrive as their own reviewable change.

### Verification
- **The rule that a draft revision gets its own identifier is now enforced.**
  Re-using one across revisions is a natural-looking change that would let the
  cleanup of an old revision delete the draft just saved. Nothing prevented it;
  now a test does, and the reason is written where someone would make that
  change.

- **Foreign keys are documented as revealing whether an id exists.** The check
  the database performs runs with privileges that ignore per-user isolation, so
  it can confirm that an id belongs to *somebody*. It exposes no content and the
  ids cannot be guessed, so it is accepted rather than fixed — but it is now
  written next to the schema so the next person adding one is choosing it.

### Security
- **Push notification keys are no longer assembled by hand.** The signing key was
  built by writing raw curve coordinates into a key structure — an approach Go now
  deprecates because it can produce invalid keys. It is now parsed by the standard
  library, which derives the public half itself and rejects a bad value outright.
  The key material and every derived value are unchanged, so existing browser
  subscriptions keep working.

  A test was added for the property that had none: that the signature verifies
  under the key Mailroom *advertises*, rather than under the signer's own copy of
  it. The previous test compared that value against itself, so the two could have
  diverged and every push would have been rejected as a bad signature while every
  test stayed green.

### Corrected
- **The deployment notes described secret file permissions that do not exist,
  and recommended a change that would have taken the instance down.** The
  containers read those files through the very permission an obvious hardening
  pass would remove. The correct control is the directory, and that is now what
  the notes say — with the measurement that settles it.

## [1.34.5] — 2026-08-17

A security and design audit went through ten areas of Mailroom looking for two
things: defects, and *checks that report success without checking*. It found more of
the second than the first. This release is the whole result — the two live defects it
found shipped in v1.34.4; everything else is here.

### Fixed
- **A sender name that is itself an email address now shows the real address next to it.**
  A message can put anything in the name it displays, including something that looks
  like an address. So `From: "security@yourbank.example" <attacker@evil.example>` showed
  up in the message list as the bank, with nothing to indicate otherwise; the reading
  pane was already honest. The list now shows the true address alongside any name that
  is itself an address, or that had invisible characters removed from it. A name that
  merely *resembles* a company is not caught by this and cannot be without verifying who
  actually sent the mail — that is a separate piece of work, and there is now a test
  asserting the gap so it cannot be forgotten.

- **Invisible characters that reorder text are stripped from sender names and subjects.**
  A single character can reverse how the rest of a line reads, so `report.pdf` can be
  made to display as `report.exe` — or a name can visually swallow the address printed
  after it. Those characters are now removed, and the parts of the interface that show
  names and subjects are told to lay out independently of one another so a crafted name
  cannot rearrange its neighbours. Marks that legitimately appear in Arabic and Hebrew
  text are deliberately kept, since they cannot reorder anything.

- **Storage no longer accumulates unreachable files.** Retiring a draft, deleting an
  account, and signing a message on its way out each removed a message's record while
  leaving its stored body behind, unreferenced and unreadable. Such leftovers could
  accumulate. All three paths now release the body, and
  one shared check decides it — the previous two copies of that check disagreed about
  what counts as still-in-use, and both missed one case.

- **Deleting an account now tells your other devices its folders and identities are
  gone.** They were removed correctly but silently, so a second client kept offering the
  dead account's folders and send-as addresses until it was restarted. Emptying the trash
  had a smaller version of the same problem.

- **Re-calculating message priorities now reaches your other devices.** The maintenance
  command that recomputes them changed what every client should display and told none of
  them; a stale priority persisted until something unrelated touched the message.

- **A signing key created with an unusually-written account identifier is no longer
  permanently unusable.** Two spellings of the same identifier are both valid, and the
  key was locked with one and stored under the other — after which nothing could use it
  again, while the failure reported itself as a serious security alarm. Identifiers are
  now normalised before anything is derived from them.

- **A signature can no longer be produced with an obsolete algorithm.** An imported key
  can ask for one; Mailroom now refuses rather than putting your name on a weak
  signature. Worth recording precisely: the library we use turns out not to honour that
  request in the first place, so this was already safe — the refusal is there so it
  *stays* safe, with a test that fails if the library's behaviour ever changes.

- **Turning off message signing is now announced at startup** when identities that have
  published a key would be affected. Previously mail from those identities was simply
  sent unsigned, indistinguishable in the log from a user who has no key — while their
  correspondents had been told to expect a signature.

### Changed
- **The disaster-recovery drill now runs whenever a change could affect a restore.**
  It decided that by consulting a short list of file paths, and that list had fallen
  behind what the drill actually exercises: it named neither the encryption code nor the
  database layer, both of which the drill's own liveness check depends on. So changes to
  them reported a passing drill without running one — including this project's own
  previous release. The list is now derived from what the drill genuinely depends on, so
  it cannot fall behind again, and a skip prints exactly what it checked.

- **The automated checks now examine the tests that need a database.** Roughly a hundred
  files were excluded from static analysis entirely — including every test that verifies
  users cannot see each other's mail. Enabling it surfaced four real defects immediately,
  which is the evidence the gap mattered.

### Verification
Not user-visible, and the largest part of this release. Several protections were
correct while the tests that claimed to verify them could not have failed:

- The message frame's isolation — the single most important protection against a
  malicious email — was verified as a *value written in one file*, not as something
  actually applied to the frame. Removing it entirely, or weakening it in the way that
  would matter, left every test passing. It is now checked where it is used.
- The browser test for hostile mail rebuilt the page from hand-copied policy settings
  rather than reading what ships. It has been reading a stale copy: one setting was more
  permissive than what the server sends and another was missing outright, so it had been
  proving a policy safe that Mailroom does not use. The settings are now read from source.
- The test that a previewed PDF cannot contact the sender was passing because the test
  PDF contained nothing that could try. A PDF that genuinely tries now proves it — and in
  the process established that **the browser, not Mailroom's own headers, is what stops
  it.** The documentation credited the header; that has been corrected.
- The rule that strips scripts from a message's HTML had no test that could fail, because
  a second, broader rule was quietly doing the work. It is now verified on its own.
- Two isolation guarantees could not observe their own failure: the check that the
  key-publishing role sees only published keys passed through a function that re-applied
  the same filter, and the check that administrators cannot read mail listed four tables
  by name. Both now test the thing itself.
- Logging in now provably replaces any session it was handed rather than adopting it.

### Corrected
- Several comments described mechanisms that no longer, or never did, work as written:
  the claim that one deletion path was the only one; a reclaim step deferred to a
  background process that cannot reach those records; the description of how the
  key-publishing role is restricted, which had been inverted by a later change; and the
  suggestion that declaring an API capability withholds individual fields, which it has
  never done — the protection there is per-account isolation, and that is now what the
  comments say.

## [1.34.4] — 2026-08-17

### Fixed
- **Marking a message unread no longer undoes itself on the next sync.**
  When the same message has been delivered to your mailbox more than once, Mailroom
  records each delivery so it can show you one message instead of four. Changes you made
  were only ever sent back to the newest of those copies, while the ones left untouched
  kept the old state — and the next sync merged them all back together. So on those
  messages, marking one unread, un-starring it, or removing a label reverted within a
  poll, over and over, with nothing anywhere reporting a failure. On a real
  mailbox a number of messages had two to four copies, and some were in exactly the
  state where "mark unread" silently reverted.

  Every copy in an account now gets the change. If one of them cannot be reached, the
  change is retried in full rather than recorded as done — a half-mirrored change is not
  a finished one — and the failure says how many copies of how many got through, so what
  is stuck is legible rather than a bare error.

  Emptying the trash had the same shape with a worse ending: it destroyed one copy per
  sweep, and a second sweep was impossible because the request was considered already
  answered. A message you deleted could keep copies on the server indefinitely, its trash
  entry never clearing and nothing ever reporting a problem. Both halves are fixed: one
  sweep now covers every copy, and a sweep that fails can be retried by the next one.

- **"Messages destroyed" now counts messages that were actually destroyed.**
  A request to delete a message the server no longer has succeeds — it simply removes
  nothing — so an expunge that destroyed mail and one that found nothing to destroy
  reported identically. Mailroom now records what the server says it removed, which is
  the difference between a delete that worked and a delete that quietly did not. This was
  the same failure that once let a stale draft survive every send (v1.33.3); the counter
  added then was honest on the path it was written for and optimistic on the other.

- **A tracking pixel hidden in a message's stylesheet could load without Mailroom
  ever telling you.** Mailroom strips the parts of a message's CSS that reach out
  to the sender's server — a background image, an imported stylesheet — because in
  mail those exist to report that you opened it. It did that by looking for the
  text `url(` in the message's CSS. CSS lets the same instruction be spelled
  several other ways, and a message that spelled it one of those ways had its
  tracker passed straight through. Confirmed against a real browser: the image was
  fetched, and the sender learned the mail had been read.

  What made this worse than an ordinary leak is that it was **silent**. Mailroom
  normally *keeps* a blocked image, hidden, so it can tell you "this message tried
  to load remote content" and offer to show it. A tracker hidden in CSS is never
  kept that way, so there was nothing to raise the banner: you were not told
  anything had been blocked, and you were not told anything had been loaded. For a
  sender whose images you had chosen to trust, it was a beacon nobody could see.

  Message CSS is now read the way a browser reads it, and only styling that can be
  named on a known-safe list survives — colours, fonts, spacing, borders, sizes,
  table layout, media queries. Anything that could name an address on the internet
  is dropped whatever it is spelled like, so this cannot come back through a
  different spelling. Ordinary newsletter styling is unaffected; two rarely-used
  pieces of styling go with it (absolutely positioned elements, and rules written
  with a `>` between two names).

  The test that was supposed to catch this looked for exactly the same text the
  code did, so it could never have found the gap. It now checks the *structure* of
  what survives, against a second, independent reading of the same bytes — and
  each of those checks is itself verified to be capable of failing.

### Corrected
- The v1.18.0 note said that when mail is delivered twice, "anything Mailroom does back to
  your mail server still knows about every copy". That was not true when it was written:
  the repeat delivery was recorded correctly, but the write-back path was never widened to
  match, and it reached exactly one copy. It is true as of this release. The original entry
  is left as it was written.

## [1.34.3] — 2026-08-17

### Fixed
- **The suggestion tool no longer asks you about mail your own filing already answers.**
  Two operator commands turn what Mailroom has learned from your labelling into
  automation: one *files* mail under a rule, the other *asks* you to confirm a category.
  Nothing said the order mattered, and it did — run the asking one first and every
  candidate safe enough to file simply became a question instead. On a real mailbox that
  was a large share of them: rules that would have quietly sorted mail became entries in the
  "needs a category" queue, and both commands' output looked perfectly reasonable either
  way.

  Candidates that match nothing beyond mail you have already filed under that label are
  now left to the command that files them, both commands state the order they belong in,
  and the report counts what it handed over instead of silently proposing it.

  That count is only ever what is genuinely left to do: anything one of your existing
  rules already covers is reported as *already covered*, not as work waiting for you.
  Counted the other way round — as it was in a first cut of this fix — a mailbox that had
  just been through the filing command was told 48 candidates were waiting for it while
  the filing command itself said there was nothing to do.

- **The report about already-sorted mail stopped overstating itself fourfold.**
  When it declines to suggest a category for a message that already has one, it now says
  *why* it declined, because there are two very different reasons and they were being
  added together. Most of that mail is the mail you filed by hand in the first place —
  the evidence the suggestion was learned from, and never something Mailroom would have
  asked about at any version. Only the rest is mail a *rule* had filed, which is the case
  the v1.34.1 fix exists to prevent. Reported as a single number, "165 skipped" read as
  the size of that fix when the real figure was 42. The two populations are now separate
  everywhere they appear, and the merged total is deliberately not printed at all.

  The same principle, applied once more: every candidate the tool considers now lands in
  exactly one reported outcome, including the near-duplicate rules it folds into a broader
  one rather than offering twice. Three of those were previously counted nowhere at all —
  harmless, since nothing the report printed contradicted them, but a population without a
  number is the thing this release is about.

## [1.34.2] — 2026-08-16

### Fixed
- **"Create a rule from this message" no longer fails because you already have a rule for
  that sender.**
  It named every new rule after the sender and nothing else — a rule made from a Venmo
  message was called "Venmo" — while rule names have to be unique. So the moment you had
  two rules for one sender, the second one simply refused to save with "a rule with that
  name already exists", and there was nothing to do about it but rename by hand. Working
  the "needs a category" queue is precisely the activity that keeps putting you in front
  of senders you already have rules for, so on a mailbox with dozens of rules this was not
  an edge case; it was most attempts.

  A rule you do not name now takes its name from both halves of what it does — the sender
  *and* the label, "Venmo → Promotions" — which is what every other part of Mailroom that
  generates a rule name already did. A name you type yourself is never touched.

  And if the name is taken anyway, the save no longer fails: Mailroom retries under
  "… (2)" and tells you it did. That is deliberately separate from the better naming,
  because a second browser tab or a command-line run can take a name in the moment between
  checking and saving, and only the database can settle it.

### Added
- **Mailroom now offers to edit the rule you already have, instead of quietly making a
  second one.**
  Before saving a new rule it compares what the rule *matches* — not what it is called —
  against your existing rules, and if one already matches the same mail it says so and
  offers to open that rule for editing, with "create a second rule anyway" as the other
  choice. Editing the one you have is the default because at any real number of rules,
  two rules keyed on the same sender is usually a mistake in the making.

  A rule matching the same mail but filing it under a *different* label is surfaced too,
  worded as the different thing it is: not "you already have this rule" but "you already
  have a rule for this mail" — because creating this one means that mail starts arriving
  with both labels on it.

## [1.34.1] — 2026-08-16

### Fixed
- **A suggested category no longer un-files mail that was already filed.**
  v1.34.0 could offer a category for a message one of your own rules had already sorted.
  That is worse than a redundant suggestion: the "needs a category" queue holds anything
  with an open suggestion *or* no category at all, so asking the question put an
  already-sorted message back into the queue. On a real mailbox this would have affected
  a sizeable share of the suggestions the first run proposed, growing the queue instead of shrinking
  it — the opposite of what the surface is for.

  The cause was one word meaning two things. "Unlabelled" is the right test in two
  different places for two different reasons: when Mailroom works out a rule from your
  filing it must count only categories *you* applied by hand, because learning from its
  own output would let it confirm its own mistakes; but when deciding whether to ask you a
  question, what matters is whether the message has a category at all, whoever gave it
  one. The first test was being used for both jobs.

  Mailroom now declines to ask about any message that already has a category, and the
  report says how many it declined for that reason rather than folding them into a total.

- **The suggestion report no longer claims two numbers should match when they cannot.**
  `suggest-rules` told you its count should agree with `suggest-report`'s, and that a
  difference meant something was wrong. It never can agree: the two deliberately exclude
  different things, and on a real mailbox they read different numbers. The report now states the
  relationship that does hold — one is a subset of the other — and what a violation of
  *that* would mean. A stated rule that is not true is worse than no stated rule, because
  the next person to read it goes looking for a fault that was never there.

### Changed
- **Suggestions are now applied after every other rule action, regardless of rule order.**
  The protection above works by checking whether a message already has a category, so it
  only holds if the categories have been applied first. That happened to be true for
  suggestions Mailroom writes itself, which run at a lower precedence than ordinary rules
  — but a suggestion rule written by hand gets the same default precedence as any other,
  and would then be settled by which rule was created first. Suggestion actions now always
  run last within a delivery, so the guarantee no longer depends on the order your rules
  happen to be in.

## [1.34.0] — 2026-08-13

### Added
- **The "needs a category" queue now actually suggests categories, and explains itself in
  words you can check.**
  The queue has been able to show suggested labels since it was built. It never showed any.
  Suggestions came from a per-message machine-learning model, and no such model is switched
  on — measurement found the ones that scored well were the ones a one-line rule already
  solved, while the labels that would genuinely need a model scored near chance, so none was
  ever promoted. The result was a feature that was written, wired up, tested, and produced
  nothing at all on real mail.

  Suggestions now come from the same place Mailroom's rules do: your own filing. When you have
  put a handful of messages under a label, Mailroom works out what they have in common and
  proposes that as a rule. Rules narrow enough to apply on their own already did so; the ones
  that reach further than that were previously discarded, and are now the source of
  suggestions. In practice that covers most unfiled
  mail, with no message receiving competing suggestions from two different labels.

  Because a suggestion is a rule, it can say plainly why it fired — "alias is
  you+shop@example.com", "has a List-Unsubscribe header" — instead of showing feature weights.
  It is a sentence you can check against the message in front of you, and if it is wrong you
  can edit the rule rather than wait for something to be retrained.

  Nothing is applied. A suggestion writes a question, never a label, and that is structural
  rather than a promise: the code path has no way to attach a label at all.

  The remaining share is the honest limit, and it is reported rather than hidden. Working
  out a rule takes at least two messages you have filed from the same sender, so a sender you
  have never filed anything from produces no suggestion. Those messages still reach the queue —
  they are simply presented without a guess, which is the correct answer rather than a gap.

- **`mailroom-admin suggest-rules`** — builds the suggestions above from an existing mailbox.
  Like `accept-rules` and `apply-rules` it reports by default and changes nothing without
  `--apply`, and `--backfill` fills the queue for mail that arrived before the rules existed.
  It reports per label rather than as one total, including how many messages it deliberately
  declined to suggest for, so "there was nothing to do" cannot be mistaken for "the thing being
  measured is always empty". Its totals are directly comparable with `suggest-report`'s, and
  the output says so — a divergence between the two means one of them is wrong.

### Changed
- **A suggestion now says what it is based on instead of quoting a percentage.**
  Suggestions used to be shown with a confidence figure, which made sense when they came from
  a model that had been calibrated to produce one. A suggestion drawn from your own filing has
  no such figure — what it has is evidence, so that is what it shows ("backed by 4 messages you
  filed under this label"). A count is something you can weigh; a percentage nobody measured
  would only look like one. Suggestions from a model, if one is ever switched on, still show
  their percentage, and the two are now told apart wherever they are displayed.

- **A rule you wrote always wins over one Mailroom proposed.** Suggestion rules are evaluated
  after your own, and a rule of yours that ends with `stop` prevents them from running at all.
  Where both have something to say about the same message and label, the suggestion drawn from
  your filing is kept in preference to a model's — it is the better-measured of the two, and it
  is the one that can be corrected.

## [1.33.6] — 2026-08-10

### Changed
- **The disaster-recovery drill can now block a merge, instead of just reporting to nobody.**
  Mailroom keeps a weekly drill that takes a real backup, destroys the original, and restores it
  — because a backup that has never been restored is a hypothesis, not a backup. The drill also
  runs on any commit that could affect a restore.

  It was not, however, one of the checks that has to pass before a change can merge, and that
  turned out to matter. A recent database change shipped with every one of its sixteen required
  checks green while the drill was failing on it twice over: the change worked when applied to
  an existing installation and failed when applied to a *restored* one. That is a defect that
  surfaces on the day you are recovering from a disaster, or never.

  It could not simply be added to the required list, because it only ran on commits touching
  certain files — and a required check that stays silent on everything else blocks every
  unrelated change forever. The filtering has therefore moved inside the job: the drill now
  starts on every commit, decides in a couple of seconds whether that commit could affect a
  restore, and either runs in full or reports success with its reasoning. Where it cannot tell
  what changed, it drills. The result costs a few seconds per unrelated change and makes the
  drill eligible to be a required check, which it now is.

### Fixed
- **When a folder fails to sync, the log now says which folder and why.**
  If a single folder failed during a sync, Mailroom counted it and threw the reason away. The
  log line reported a number — "2 folder errors" — with no indication of which folders or what
  went wrong, and it was filed at the same severity as a completely healthy sync. The folder
  count in that same line quietly dropped, and the pass still read as a success.

  A single failing folder is deliberately not fatal: its position is left untouched so the next
  pass simply tries again, which is the right behaviour. But it also means nothing else ever
  reports the problem, so a folder that has been failing every pass for a week looked exactly
  like one that was briefly busy.

  Each failure is now logged with the folder's name and the underlying cause, and a sync that
  had any is no longer recorded as routine. This is what an operator needs to tell a transient
  hiccup from a folder that has silently stopped syncing.

### Security
- **The tracking-pixel test now says only what it has actually checked.**
  The test that proves a tracking pixel never phones home worked by confirming the pixel's
  server received nothing. But "nothing arrived" has two meanings, and it could not tell them
  apart: either the email was displayed and the pixel was correctly refused, or the email was
  never displayed at all, so there was nothing to refuse. The second case reported success
  just as confidently as the first — and it was observed happening, in a run that was
  otherwise failing.

  The check now reports success only when a display actually took place. If it did not, the
  test says so in those words instead of claiming a result it never established.

- **The hostile-mail test can no longer pass by quietly giving up.**
  Mailroom's most important test opens a deliberately malicious email in a real browser and
  checks that nothing ran, nothing was fetched, and the isolation held. If the browser stage
  hit any unexpected problem, though — a moved interface, a changed response, a timeout — the
  test reported that it had "skipped" that part and declared the whole run a success. It then
  printed, in as many words, that hostile content had been neutralized end to end.

  So the checks most worth trusting were the ones that could vanish without anybody being
  told. The only failures that could turn the build red were a fixed list of ones already
  known about, which meant a *new* way of breaking could not be caught even in principle.

  Now the browser stage fails when it cannot do its job. The single exception — the browser
  image genuinely being unavailable, as on a machine with no network — has to be asked for
  explicitly, so a passing run is now evidence the check actually ran rather than evidence
  nobody looked.

  Demonstrated both ways before shipping: with a simulated browser fault the old code printed
  "skipped", declared success and exited 0, while the new code fails the build on the same
  fault.

- **The test that proves tracking pixels stay blocked is now actually run.**
  Mailroom blocks remote content in received mail by default, which is what stops a one-pixel
  image in a marketing email reporting back that you opened it — while still displaying images
  that travel *inside* the message. Holding both of those at once is delicate: the natural way
  to make embedded images work is also the way to let tracking pixels through.

  A test suite was written to guard exactly that boundary. It delivers a real message containing
  both an embedded image and a tracking pixel, opens it in a real browser, and checks that the
  embedded image appears while the pixel's server receives nothing at all. It has existed for a
  month, and in that month it never ran once — the change that added the other end-to-end suites
  to the build simply did not include it. Nothing was watching this guarantee.

  It now runs on every change, and it has to pass before anything can merge. Checked first that
  it still works against today's code, and then that it still *fails* when it should: with remote
  blocking deliberately disabled, the suite goes red rather than quietly passing.

- **The browser that proves hostile mail is harmless can no longer change without anyone noticing.**
  Two of Mailroom's test suites open a deliberately malicious message in a real headless browser
  and check that nothing ran, nothing was fetched, and the sandbox held. Every one of those
  checks is a statement that something did *not* happen — and that kind of check is uniquely
  fragile, because it also passes when the thing it was watching stopped being possible to
  observe at all.

  Both suites were pulling that browser from a tag that moves. A new Chromium could have arrived
  with no commit, no review and no changelog entry, and if it had quietly changed how a sandbox
  attribute or a content-security rule behaved, the suites would have gone on passing while
  testing less than they used to. This is the same shape as an earlier bug in which three
  assertions about a sandbox all passed against a page that had never loaded.

  The browser is now pinned to an exact image, with the Chromium and Puppeteer versions recorded
  beside the pin so the next person can see what they are upgrading from. Upgrading is still a
  single-line change, and it now has to be a deliberate one — with the suites' own
  self-sabotage checks re-run afterwards, since a browser change is exactly when "would this
  test still fail if it should?" stops being a formality.

## [1.33.5] — 2026-08-08

### Changed
- **Signed mail is now checked against an independent implementation, in both directions.**
  Mailroom's signing and its signature-checking were only ever tested against each other. That
  is a real gap rather than a formality: the cryptography comes from a third-party library, but
  the decision about *exactly which bytes get signed* is ours on both sides — so a single
  mistake made consistently in both places would look perfectly correct in every test we had,
  and would produce mail that no other program in the world accepts.

  Every build now checks that GnuPG — which shares no code with Mailroom — accepts a message
  Mailroom signed, and that Mailroom accepts a message GnuPG signed. Both checks are paired
  with deliberately corrupted variants that must be rejected, including one that differs by a
  single line ending, so a check that could never fail cannot pass for the wrong reason.

  Nothing was found to be wrong: the existing behaviour was already correct. What changes is
  that it is now demonstrated rather than assumed, and a future change that broke it would be
  caught before release instead of by a recipient.

## [1.33.4] — 2026-08-08

### Security
- **The public key-lookup service can no longer reach your private keys — this time for real.**
  Mailroom can publish a public key for an address so that anyone can find it. That lookup is
  the only place where a complete stranger, with no login, reaches into the database, so it is
  supposed to run with permission to read the *public* half of a key and nothing else.

  It never actually did. Three previous attempts to restrict it all failed silently, for the
  same underlying reason: the lookup ran as the database account that **owns** the tables, and
  an owner cannot be restricted — the database refuses to function without giving an owner full
  access, so every restriction placed on it either did nothing or broke something else. The
  third attempt did take effect and immediately broke deleting accounts, users, addresses and
  keys, which is what the previous release had to undo.

  The lookup now runs as a dedicated account that owns nothing at all, holding only the handful
  of columns it needs. Reading private key material from that path is now refused by the
  database itself rather than merely avoided by the code — so it stays refused even if the
  lookup were later rewritten to try.

  To be precise about what this does and does not change: no key material was ever exposed, and
  no code path ever read it. What was missing was the second line of defence that the design
  claimed to have.

### Changed
- Deleting accounts, users, addresses and keys is now covered by tests that run against the
  same ownership setup as the real database, alongside the restriction above. The two have
  broken each other twice; they are now checked together, so neither can be fixed at the
  other's expense without a test going red.

## [1.33.3] — 2026-08-08

### Fixed
- **⚠ Sending a message could leave a stale draft behind that you could not delete.** Sending
  works by saving the draft, filing a copy in your Drafts folder, sending, and then removing
  that copy. The removal step could quietly fail: it looked for the copy, did not find it, and
  recorded itself as having succeeded. Nothing retried, nothing was logged as wrong, and the
  draft stayed on the server.

  Worse, deleting the leftover from inside Mailroom did not help. The deletion was swallowed
  because a removal for that same message had already been recorded as done, so nothing was
  ever asked of the server — and the next sync found the copy still sitting there and brought
  it straight back. Deleting it again just repeated the cycle.

  Three separate things were wrong, and all three are fixed:

  - A removal that removes nothing is no longer treated as success. It is retried, and if it
    still cannot find the message it is reported as a failure you can see, naming the message
    and the folder, instead of disappearing.
  - The leftover is no longer created in the first place. When a message is signed on its way
    out, its contents change — and the step that was supposed to cancel the queued filing of
    the *unsigned* draft was looking for the signed one, so it cancelled nothing. It now
    recognises both.
  - Deleting a message you want gone is always acted on, even if an identical removal ran
    earlier and finished. Repeating a request that is still in progress is still collapsed, as
    it should be; repeating one that already completed is a new request and is treated as one.

  If you have a stale draft from before this release, deleting it in Mailroom now works.

### Changed
- When Mailroom files a message on the server, it now remembers where the server put it. That
  removes a race in which it filed a message and then, a fraction of a second later, could not
  find the message it had just filed.
- The writeback log line now reports how many messages were actually removed from the server,
  separately from how many operations completed. The two are not the same thing, and the gap
  between them was invisible.

## [1.33.2] — 2026-08-08

### Fixed
- **⚠ Deleting an account, a user, an address or a key was broken, and this release fixes it.**
  A change in the previous release tightened who is allowed to read the stored private half of
  a PGP key. It tightened one permission too far: the database uses that same permission
  internally when it tidies up after a deletion, so removing an account — which has to clear
  the addresses belonging to it — failed outright. The same applied to removing a user, an
  address, or a key.

  Nothing was lost or corrupted: the deletions refused rather than half-completing. But they
  refused, and the tightening that caused it has been undone.

  The protection it was meant to add is gone again as a result. That is a deliberate choice —
  a deletion path that does not work is a worse problem than a missing second layer of defence
  on a path whose first layer is correct — and the underlying design error is now understood
  and written down properly rather than patched a third time.

### Changed
- **The test suite now sets the database up the way the real one is set up.** Tests were
  building the database as an administrator, while the real thing builds it as a more limited
  account. Anything about *who is allowed to do what* therefore looked the same in testing no
  matter what it was, which is how the permission bug above reached you — and how two earlier
  attempts at the same protection were both merged while doing nothing at all.

  With this change the tests found the bug within minutes of the change existing.

## [1.33.1] — 2026-08-07

### Added
- **The public key lookup can no longer be asked unlimited questions.** The page that serves
  your public key to correspondents is the one part of Mailroom a stranger can reach without
  logging in, and until now nothing capped how often. Each request costs a database lookup, so
  a machine hammering it could have made the whole instance slow — including your own mail.

  There is now a ceiling, on by default and generous enough that ordinary use will never meet
  it: a mail program looks up one key per recipient, and the allowance covers far more than any
  real message needs. It can be tuned or switched off deliberately if you need to.

  Requests that are obviously not key lookups still cost nothing and are not counted, so junk
  traffic cannot use up the allowance and crowd out real lookups. And a refusal looks exactly
  the same whether the address exists or not — the limit cannot be used to work out which of
  your addresses are real, which is a property the rest of this feature is carefully built
  around.

## [1.33.0] — 2026-08-07

### Added
- **A read-only report that checks whether the PGP signatures in your mailbox actually hold up**
  (`mailroom-admin pgp-verify`). The existing report could tell you a message *carried* a
  signature; it could not tell you whether that signature was still good. Those are different
  questions, and only the second one catches a message that was altered after it was signed.

  Each signed message gets one of four answers, kept apart on purpose: the signature **checks
  out**, the signature is **bad**, it was made by a **key you do not have**, or the message
  claims to be signed and is **malformed**. Only the second is a problem. "Key you do not have"
  is the ordinary case for anyone writing to you — Mailroom cannot yet look up a stranger's key
  — and lumping it in with real failures would bury them.

  ⚠ A signature that checks out proves the message was not changed since it was signed. It does
  **not** prove the key belongs to whoever the message says sent it — that needs key lookup,
  which does not exist yet, and the report says so rather than letting you assume otherwise.

  It writes nothing and changes nothing about how mail is displayed. It exists to establish
  the facts first: the previous release's claim — that Mailroom's own signed mail survives the
  trip through a mail server intact — had until now been checked only in tests.

## [1.32.1] — 2026-08-07

### Fixed
- **Sending a signed message could leave a stray, unsigned copy of it sitting in your Drafts.**
  Signing rewrites the message as you send it, but the copy already saved to your mail
  provider's Drafts folder still held the older, unsigned version — and Mailroom recognises a
  message by its exact contents, which had just changed. If a sync happened to read that folder
  in the seconds before the old copy was deleted, it came back as a *new* message: a draft of
  something you had already sent, which you could then edit and send again.

  Mailroom now remembers what a message used to look like when it rewrites one, so a late copy
  of the older version is recognised as the same message instead of becoming a second one.
  Nothing is thrown away — the copy is still tracked, which is what lets the stale one be
  cleaned out of the folder.

  It was a race, so it did not happen every time: of the first two signed messages sent,
  one grew the extra draft and one did not.

## [1.32.0] — 2026-08-07

### Added
- **Mailroom can now sign the mail you send, so a correspondent can prove it really came from
  you.** If you have given one of your addresses a PGP key, messages sent from that address
  carry a standard OpenPGP signature. Addresses without a key are completely unaffected and
  their mail goes out exactly as before — signing is per-address, and nothing turns on by
  itself.

  The signature is applied when you press send, and the copy kept in your Sent folder is the
  *same* signed message the recipient got. That matters more than it sounds: the alternative —
  signing on the way out — would have left your own sent mail appearing **twice** in your
  mailbox, because the copy your provider files would no longer match the copy Mailroom kept.
  It also means your archive can show exactly what you signed, rather than something close to it.

  ⚠ **While the key-holding part of Mailroom is stopped, an address with a key cannot send.**
  This is deliberate and there is no fallback: the whole reason your keys live in a separate,
  sealed-off program is that the rest of Mailroom cannot use them, and quietly signing somewhere
  else under load would undo that. It fails at the moment you press send, which is the point at
  which you can do something about it, rather than producing mail carrying a signature nobody
  can check. Reading and receiving mail are unaffected, and so is sending from any address
  without a key.

### Fixed
- **Accented letters, dashes and non-Latin scripts are now encoded properly in outgoing mail.**
  Message bodies were being sent in a form that mail servers are not obliged to carry, so
  anything beyond plain English letters — an em-dash, a name like "Renée", Japanese, an emoji —
  depended on every server along the way choosing to handle it. Bodies that need it are now
  encoded in a form that has been safe to send since the 1990s, and plain-ASCII messages are
  left byte-for-byte as they were.

  This also silently broke signatures: a single accented character was enough for a server in
  the middle to rewrite the body, after which the signature no longer matched and the recipient
  saw a warning — with nothing on this side showing a problem. Very long lines had the same
  effect and are handled the same way.

## [1.31.1] — 2026-08-07

### Fixed
- **A safeguard around your stored PGP keys was doing nothing, and now does something.** The
  part of Mailroom that answers a stranger's "what is the public key for this address?" was
  supposed to be *unable* to read your sealed private keys — not merely written so that it
  doesn't, but denied the ability by the database itself, so that a future mistake in that code
  could not quietly turn it into a leak. Checking on the running server rather than in the test
  environment showed the permission had never actually been taken away: it was removed at the
  level of the single column, which does not override the blanket access the database's owning
  account already holds. The block is now applied at the level that works.

  Nothing was exposed. The lookup only ever asked for public information, and there is no sign
  or possibility of anything else having been read — what was missing is the second line of
  defence behind it, for the case where the first one is wrong one day.

  The table holding your addresses had the same decorative limit, and gets the same repair.
  Nothing secret lives there — the fields it leaves out are your display name and signature —
  but a limit that reads as enforced and isn't is worse than no limit, because the next person
  to read it will believe it.

  ⚠ The previous release's notes described this protection as already in place. It was not, and
  that claim was wrong rather than merely optimistic. The reasoning is recorded in the migration
  and in the test, including the trap that makes the obvious fix break every key lookup if the
  two steps are done in the wrong order.

## [1.31.0] — 2026-08-07

### Added
- **⚠ A second secret to keep somewhere safe, and a backup rehearsal that now checks for it.**
  The part of Mailroom that will hold your PGP keys has been split out into its own separate
  program with its own secret, so that someone who steals the server's main key still cannot
  use your keys to sign or read mail as you. The price is that there are now **two** things to
  keep a copy of, and they fail very differently. Losing the main key is loud — nothing works.
  Losing the new one is **silent**: your mailbox restores, opens, searches and reads perfectly,
  and only the mail that arrived encrypted is gone forever, with nothing on screen saying so.
  So the weekly restore rehearsal now refuses to run unless you hand it both, tells you plainly
  what it did *not* prove if you skip one, and fails outright if you have quietly pointed both
  at the same file — which would look completely normal while undoing the whole point.
  `deploy/README.md` and the escrow procedure explain how to generate and store both.

  Nothing in your mail uses this yet. It is the foundation the encrypted-mail work is built on,
  not the feature itself.
- **Your PGP keys are now stored, and can be brought with you.** Keys survive a restart instead
  of living only in memory, an existing key you already use elsewhere can be handed over rather
  than replaced, and a key can be attached to one of your addresses so it is clear which key
  signs mail from which identity. Everything is held per-account and isolated by the database
  in the same way your mail is: another account cannot read your keys even by naming them
  exactly.

  Still nothing a person can *see* — no mail is signed or decrypted yet, and no key is published
  anywhere. That is the next step.
- **Your PGP key can now be published, so people can actually write to you privately.** Mailroom
  serves your public key at the standard location that mail programs already look in, which means
  a correspondent using Thunderbird or GnuPG finds it from your address alone, with nothing to
  install and nothing to ask you for. There is also a plain page you can send someone, showing the
  fingerprint in the usual groups so the two of you can check over the phone that the key really is
  yours — which is the part that makes encryption mean anything.

  Only addresses you deliberately publish are findable, one at a time. A catch-all's endless
  made-up addresses are not publishable and never will be, and neither is a retired key — that
  would invite mail to a key you have stopped using. Withdrawing a key stops new lookups, but
  cannot recall a copy someone already fetched.

  ⚠ **Publishing needs one piece of DNS you have to get exactly right.** Mail programs check an
  `openpgpkey.` subdomain of your domain *first*, so a subdomain that half-works breaks every
  lookup — including ones that would otherwise have succeeded. It must be either absent or
  correct. `deploy/README.md` has the setting, the check, and the one command that tells you the
  truth: asking a real mail program to find your key, rather than fetching the page yourself.
- **The weekly backup rehearsal now opens a real key instead of taking your word for it.** Since
  keys are stored, the rehearsal unseals every one of them with the escrowed second secret and
  checks each is genuinely the key it claims to be. If a mailbox has no keys yet it says so and
  reports the check as **unproven** rather than passing — and if a stored key does not open, that
  is a loud failure, because it means the copy you kept and the mail you have quietly stopped
  matching, and no other check in the rehearsal would notice.
- **A read-only report on how much of your mail is signed or encrypted** (`mailroom-admin
  pgp-report`), the first step of the encrypted-mail work. It counts each kind of signing and
  encryption apparatus separately — PGP/MIME, the older inline armor, Autocrypt headers, and
  S/MIME — because the answers lead to different next steps: mail you cannot read without
  decryption is a different problem from a correspondent quietly advertising that they *could*
  receive encrypted replies. Messages that could not be read or parsed are reported on their
  own line rather than counted as clean, since not being able to look at something is not the
  same as having looked and found nothing.

### Documentation
- **ADR-024** records how encrypted mail will be handled before any of it is built: the keys
  that prove who you are get their own separate process with its own secret, so that even
  someone who has stolen the server's main key cannot use them. It also writes down the price
  of that choice in plain terms — there is now a **second** thing you must keep a copy of
  somewhere safe, and losing it would cost you every message ever encrypted to you, quietly,
  while everything else restores and works normally. The backup documentation and the weekly
  restore rehearsal change at the same time as the feature, not after it.
- **ADR-023** records why automatic categorisation is rules plus abstention rather than a
  learned per-label model, and why the model is kept but cannot serve without clearing a
  measured bar. The conclusion had been re-derived three times across five weeks of
  measurement; it is now written down once, with the argument, the numbers, and the tests that
  hold it.

## [1.30.1] — 2026-08-05

### Fixed
- **The new suggestion report answered its own question wrongly on its first real run, and the
  answer was a property of the report rather than of your mail.** It reused a setting from the
  rule-accepting command — "only count rules that would touch nothing you have not already
  filed" — which is the right caution when creating a rule and is self-defeating when
  measuring which unfiled mail a rule would reach: those are the same messages, so the count
  was forced to zero for any mailbox whatsoever. It reported that no unfiled mail could be
  suggested for; in practice most unfiled mail could be suggested for, none of
  it ambiguous between two categories. The setting is gone, the reasoning is written where
  the filter used to be, and a run that legitimately reports zero now tells you to check this
  first.

## [1.30.0] — 2026-08-05

### Added
- **A new read-only report answers whether Mailroom could suggest categories from your own
  rules** (`mailroom-admin suggest-report`). The place where suggested categories appear has
  been built and connected for some time and has never shown anything, because the only thing
  wired to fill it was a model that has not earned the right to speak. The proposal is to fill
  it from the rule suggestions instead — the mechanism that demonstrably works — and this
  report measures, before anything is built, how much of your unfiled mail that would actually
  reach: per category, how many messages, how many would get conflicting suggestions, and how
  many would get nothing at all, with a sample of those so the remainder can be looked at
  rather than taken on trust. It writes nothing and has no apply mode, because there is
  nothing to apply yet.
- **The promise this whole feature was built to keep is now a test that runs.** "A shop's
  advertisement and that shop's order receipt must not end up in the same place" has been the
  stated bar for automatic categorisation since the work began, and it lived as a paragraph in
  a planning note — checked by hand, for shops Mailroom had already seen. It is now three
  executable cases covering the three ways it could break, for a shop Mailroom has **never**
  seen: the receipt is recognised as a receipt rather than merely landing beside one; a model
  trained on nothing but that shop's advertisements does not pull in its receipt; and a
  category learned from a single sender cannot be promoted at all.

### Changed
- **A category model can no longer earn the right to suggest by ranking well — it has to be
  right.** The bar used to be a ranking score over a single sample of held-out senders. A
  ranking score answers "does this model sort mail sensibly?", which is not the question a
  suggestion surface asks: what matters is that the suggestions you are actually shown are
  correct, and the two came apart badly on real mail — respectable ranking alongside
  suggestions that were wrong much of the time. The bar is now **precision at the point
  where a suggestion is shown, measured across five rotations of held-out senders, read at the
  pessimistic end of its confidence interval**, and a label must have mail from at least ten
  distinct senders before it can be judged at all — enough that no single sender carries a
  rotation. Reading the bottom of the interval rather than the middle is what stops a flawless
  9-out-of-10 from promoting: ten suggestions cannot support that claim, and the interval says
  so.
- **The diagnostic and the decision are now the same calculation.** `train-labels --folds N`
  and `train-labels --apply` used to measure different things on different samples, so the
  preview could disagree with what actually happened. The report now prints, per label, the
  verdict the promotion path would reach, because it is that verdict.
- **On the current mailbox this refuses every label, which is the expected result**, and the
  report says so as a sentence rather than leaving a column of "false" to be interpreted.
  Nothing was being promoted before either — but for want of anyone running the command, not
  because the bar held.

### Fixed
- **A model's score depended on the order Go happened to walk a map**, so two runs over
  identical evidence could disagree in the last few digits. Harmless as a score, and not
  harmless as a decision: the gate that decides whether a newly trained category model
  replaces the one in service compares those numbers, so a genuinely tied comparison could be
  settled by iteration order rather than by anything about your mail. Scores are now summed in
  a fixed order, which makes a training pass reproducible from its evidence — the property the
  seeded shuffle and the cold restart already existed to give it. Nothing is being promoted
  today; this closes the defect *before* the promotion path is revisited rather than after.
- **Saved model snapshots now serialize to identical bytes for an identical model.** Model
  artifacts are stored content-addressed, so a map-ordered payload gave the same model a
  different content hash on every save — two saves of one model could not be recognised as the
  same object.

### Notes for operators
- Existing stored models are unaffected: their weights were saved and are read back unchanged.
  Only the order in which those weights are summed has changed, so existing priority scores may
  move in the last few decimal places and nowhere else. No re-training and no backfill.

## [1.29.1] — 2026-08-04

### Fixed
- **The label names in Settings → Categories were unreadable** — "Cybersec…", "Ga…", "Ne…" —
  because each category dropdown was taking the whole row. The name now keeps a readable
  minimum width and the dropdown a bounded one, and a name long enough to still be shortened
  carries its full text as a tooltip. A label picker whose labels cannot be read is not a
  picker.

### Fixed
- **The weekly restore drill's "restore an older backup, then migrate forward" rehearsal has
  been failing since 2026-08-03** and nothing said so: it is a separate workflow from CI, so it
  never appears in a pull request's checks. The drill built its older fixture by seeding an
  old schema with *today's* code — which stopped working the moment a migration added a column
  the ingest path writes. It now builds that fixture from the revision that predates the
  migration under test, which is both the honest simulation and what the drill's own error
  message already tells a human to do. The other six phases, including the full clean restore,
  were passing throughout — what was unexercised was specifically the disaster case where your
  backup is older than your code.

### Notes for operators
- **The category-tab JMAP methods are now exercised over real HTTP in CI** (`scripts/tabs-e2e.sh`,
  a new `tabs-e2e` job). The in-process tests call handlers directly, which bypasses the
  dispatcher — and the capability gate lives in the dispatcher, so nothing covered the seam
  where a client that fails to declare `urn:mailroom:categorytabs` gets `unknownMethod` from
  every method while the read path still renders perfectly. The suite drives a real container
  under dev-auth: it reads the tabs, disables one, proves it persisted, proves Primary is
  refused *per object* inside a batch whose other update still applies, and patches a label's
  tab and clears it again. Its load-bearing assertion is a negative control — the same call
  with the capability omitted must be refused.

## [1.29.0] — 2026-08-04

### Added
- **Categories are now yours to arrange.** Settings → Categories lists the five tabs: switch
  one off, or move it earlier or later in the strip. Until now the strip was fixed in Gmail's
  order and every tab was on.
- **Primary cannot be switched off**, and the panel says why rather than offering a control
  that always fails: mail from a category you turn off appears in Primary, so a Primary that
  was off would be a tab that is switched off and still holding mail.
- **A label can be pointed at a category**, in the same panel. A label decides its mail's
  category outright — the automatic sorting only ever answers for mail no label speaks for —
  and the change takes effect immediately, with no reprocessing of anything. The list you are
  looking at re-runs itself, since a message can change category without changing in any other
  way and nothing else would have noticed.
- **Switching off the category you are reading returns you to the inbox** rather than leaving
  you on a list the server will not answer. The same applies to a link to a category this
  account has switched off.
- Each tab says how many labels point at it, and a tab you have switched off says that its
  mail is appearing in Primary meanwhile. Both are facts about your *mapping*, not counts of
  mail: nothing here has measured your mailbox.
- The panel says plainly that automatic sorting is a guess — dependable for list mail and
  newsletters, weaker between offers and notifications. That is the measured shape of it, not modesty.

### Notes for operators
- `mailroom-admin map-label-tabs` is no longer the only way to change the mapping. It stays
  useful for applying a whole mapping at once.
- Turning off all but Primary hides the strip (it needs two tabs to be a choice), and the
  panel says so where it happens — otherwise a setting whose effect is invisible reads as
  broken.

## [1.28.1] — 2026-08-04

### Fixed
- **Every message appeared in Primary as well as its own tab.** Primary was routed to the plain
  inbox view, which has no tab filter at all and therefore lists everything. Primary is now a
  real tab like the others, so it shows the mail no other enabled tab claimed — plus anything
  folded in from a tab you have switched off. Expect it to be *thin* on a mailbox that is mostly
  bulk, which is a property of the mail rather than a fault in the tab.
- **The tab strip stretched to fill the pane while a list was loading**, then snapped back once
  mail arrived.
- **Opening a message rendered it inside the list column** instead of the reading pane.

  Those last two were one mistake: the strip was added as a direct child of the pane container,
  which is a CSS grid whose columns *are* the reading-pane layout. An extra child silently
  became a grid cell and pushed every pane across. The strip now renders inside the list itself,
  where it belongs.
- The strip stays put when you scroll the list, instead of sliding under the toolbar.

## [1.28.0] — 2026-08-04

### Added
- **Category tabs are visible.** A strip appears above the inbox — Primary, Social,
  Promotions, Updates, Forums — and clicking one narrows the list to that kind of mail.
  Deep-linkable (`#/tab/promotions`), so a tab is something you can bookmark or send someone.
- **A tab narrows your inbox rather than replacing it.** Tabs are a second axis over the same
  mail: labels say what a message is *about*, tabs say what it *does*. So a tab never shows
  archived or sent mail, and snoozed mail stays hidden exactly as it is in the inbox.
- **Only tabs you have switched on appear**, and Primary is the inbox itself. Turn a tab off
  and its mail folds back into Primary rather than disappearing.

### Notes for operators
- The web client now declares `urn:mailroom:categorytabs`. Without that declaration the
  server's per-method gate fails closed and every tab call answers `unknownMethod` — the
  shape where the read path renders fine and only the write path is dead.
- The strip hides itself when fewer than two tabs are enabled: one tab holds everything, so
  the control would be furniture.

## [1.27.0] — 2026-08-04

### Notes for operators
The JMAP surface for category tabs. A client can now ask which tab a message is in and
list one tab's mail — but there is still no tab strip, so nothing changes in the app yet.

- **`Email.categoryTab`** — read-only and **opt-in** (not in the default property set, so a
  list view that never names it pays nothing). It returns the tab plus `fromMapping`, because
  a tab decided by your label mapping is your own decision while a classified one is a guess.
  **It is `null` when nothing has decided** — no mapped label and no stored verdict — rather
  than `"primary"`: such a message is also absent from an `inTab` query, so calling it Primary
  would put it in a tab whose own list excludes it.
- **`Email/query` gains an `inTab` filter.** The server applies the enabled-fold, so a client
  asks for the tab the user clicked and never has to know which tabs are switched off. Asking
  for Primary returns Primary plus any disabled tab's mail; asking for a *disabled* tab is
  refused rather than answered with an empty list, since an empty list is a false statement
  about the mailbox.
- **`Email/queryChanges` refuses a tab filter** (`cannotCalculateChanges`). A message's tab can
  change with no Email change-log row behind it — you re-point a label, or the classifier
  version bumps — so a delta computed from that log would silently omit mail that moved tabs.

## [1.26.0] — 2026-08-04

### Notes for operators
Still no tab strip — this is the server half of the client surface.

- **Migration 00029** stores the tab classifier's *verdict* on each message
  (`classified_tab`, `classifier_version`); goose goes to **29**. It is deliberately not a
  `tab` column: a message's tab stays `COALESCE(mapping, verdict)` with the mapping half read
  live, so re-pointing a label still moves its mail with no backfill. Storing the verdict is
  what lets `Email/query` filter by tab in SQL without re-writing the Go classifier in SQL —
  two implementations of one decision list would drift, and the drift would show as the tab
  strip's count disagreeing with its own list. **ADR-022** records the reasoning and amends
  ADR-021.
- **`mailroom-admin classify-tabs`** fills in the verdicts for mail that predates the column
  (dry-run by default, reporting per tab and per rule). Until it has run, mail with no verdict
  and no mapped label is *invisible* to a tab filter rather than misplaced — so the command
  also reports what is still missing afterwards.

## [1.25.0] — 2026-08-03

### Notes for operators
Groundwork for automatic category tabs. Nothing user-visible changes yet; every
item exists so the feature can be *measured* before it is built.

- **Migration 00028** adds the category tabs themselves as editable per-user state; goose
  goes to **28**. Two things become data rather than code: `app.category_tabs` (which of
  the five Gmail-style tabs are switched on, and their order) and `app.labels.tab` (which
  tab each label's mail feeds). Both are JMAP-visible — a new `urn:mailroom:categorytabs`
  capability with `CategoryTab/get`, `/changes` and `/set`, plus a `tab` property on
  `Label` — so a rearrangement made on one device reaches the others.

  All five tabs ship enabled, Forums included, even though a typical label mapping
  routes nothing to it. Primary cannot be disabled: mail from a switched-off tab folds
  into Primary, so a disabled Primary would be a tab that is off and still holding mail.
  **A message's tab is derived, never stored** — labelled mail through the mapping (a
  join), so re-pointing one label moves its mail on the next read, with no backfill and
  nothing left stale.
- **The tab classifier** decides a tab for mail no label maps, from public RFC facts alone —
  `List-Id` (2919), `Auto-Submitted` (3834), one-click (8058), `Precedence`, `Feedback-ID`,
  DKIM `d=`, `Return-Path` — plus internal/kind's ESP table and subject lexicons. A
  first-match decision list, not a score: no weights, and every verdict explains itself in one
  sentence ("carries List-Id, so: Forums"). It uses no per-user behaviour, deliberately, so
  the tab is right on a stranger's first message. Nothing is wired to a client yet; it is
  reachable only through the report below.
- **`mailroom-admin tab-eval`** replays the classifier over the whole corpus and scores it
  against the existing label mapping. It reports per-tab precision and recall **separately**
  (never an aggregate), repeats every metric macro-averaged over **senders** so one
  high-volume sender cannot carry a tab, decomposes precision **per rule**, and hands back
  the `List-Id` population as a reconciliation list. A Forums recall of 0 against today's
  mapping is expected and the report says so. Read-only.
- **`mailroom-admin map-label-tabs`** applies a label→tab mapping from a plan file the
  operator supplies (dry-run by default). It exists because the mapping is per-user data
  that no migration seeds, and because setting `app.labels.tab` by hand-written SQL would
  skip the `change_log` row every JMAP-visible mutation owes — this routes through the
  store instead. The report is per label, and a run that changes nothing says so.
- **`mailroom-admin tabs`** reports what the mapping alone decides: per-tab message counts
  before and after the enabled fold, and the two undecided populations reported
  *separately* — mail with no labels (which needs the classifier that is not built yet)
  and mail whose labels simply have no tab (which needs one mapping edit). They look
  identical from outside and have opposite fixes, so a single "unresolved" number would
  send the next increment after the wrong problem. It also counts how often the
  tie-break for labels spanning two tabs actually fires, which is currently zero.
- **Migration 00027** stores the message signals a category is decided from — RFC 8058
  one-click, `Auto-Submitted`, `Precedence`, `Feedback-ID`, the DKIM `d=` domains and
  `Return-Path` — as raw header values on `app.messages`; goose goes to **27**. Migration
  00016 stored four of these already for the same stated reason; this completes the set.
  Existing rows keep NULL until `mailroom-admin backfill-function-signals` runs (dry-run
  by default, and it reports per-signal presence rather than one total).
- **The per-label classifier's evaluation was measuring its own training procedure.** Four
  defects, none of them a threshold: the trainer took 24 unshuffled, unweighted passes with
  positives loaded before negatives, so sender memorisation was a *fixed point of the
  procedure*; cross-validation scored only negatives sharing a domain with the held-out
  positives; the negative pool was the 400 most recent messages and full of unlabelled
  would-be positives; and the delivery alias leaked past the sender grouping. All four are
  fixed, and precision-at-threshold — which existed nowhere, and is the only metric that
  matters for a suggest-only surface — is now reported over pooled out-of-fold scores.
  No threshold, weight or epoch count changed, and the classifier still only ever suggests.

  Re-measured on a real corpus, this **raised** grouped AUC where the defects had
  been suppressing signal (some labels had scored below chance and no longer do),
  **lowered** it where they had been inflating it,
  and roughly halved the fold spreads. It also showed the classifier is still not usable at
  its operating point: precision stayed low, and most true positives were confidently
  rejected on several labels. That is now a sound negative rather than an artifact, and it is
  why category tabs are built from deterministic public facts instead.

## [1.24.0] — 2026-08-03

### Added
- **You can stop syncing a folder you don't want.** Each folder in the sidebar has a
  pause control; a paused folder stops being polled entirely. Useful for a huge
  archive, a shared folder you don't read, or anything whose mail you'd rather not
  pull down.
- **Nothing is deleted when you pause.** The mail already there stays and stays
  readable. The server simply stops looking at that folder — which also means it
  never concludes that anything in it has gone away.
- **A paused folder says so**, marked `paused` in the sidebar and greyed, with a
  tooltip explaining what you're looking at: a snapshot from when you paused it. A
  folder that quietly stopped updating while looking normal would be a lie about
  mail — it would still list messages the sender has since deleted, and miss
  everything that arrived.
- **Un-pausing catches up.** New mail arrives, mail deleted elsewhere finally
  disappears, and even a folder that was deleted and recreated on the server while
  paused is reconciled rather than left broken.

### Notes for operators
- **Migration 00026** constrains `app.mailboxes.sync_scope` to `full` / `off`; goose
  goes to **26**. It adds no column — the column has been in the schema since the
  beginning, and the sync engine has always honoured `off`. What was missing was any
  way to set it other than direct SQL.
- **A third value `headers` was advertised in the schema comment and never
  implemented.** Because the engine tests only for `off`, a folder set to `headers`
  was synced *fully* — the opposite of what the name promises. It is now refused by
  the database, the store and the API alike, and migration 00026 rewrites any such
  row to `full` (behaviour-preserving: that is what it was already doing). See
  **ADR-020** for why implementing it is an architecture change rather than an enum
  value — a headers-only message has no raw bytes, so it cannot have a content hash,
  which is message identity (ADR-001).
- `Mailbox/set` now accepts exactly one property, `syncScope`. Creating, renaming and
  destroying mailboxes are still refused, and a patch mentioning any other property
  is refused **whole** — including a `syncScope` next to it, so a client can never be
  left believing a rename landed.
- Per-**account** `syncEnabled` already existed; this is the per-**folder** half.
  Per-**device** sync scope is a different feature — it needs change-log cursor work
  (spec §13/§14) and its own ADR — and is not included.

## [1.23.0] — 2026-08-02

### Added
- **You can send from your own plus addresses.** If `you@example.com` is a verified
  address, `you+shopping@example.com` now counts as yours: you can put it in From,
  and a reply to mail that arrived at it comes from it by default. Previously only a
  catchall (whole-domain) account could do this — an ordinary single-address account
  was refused.
- Mail delivered to a plus address already recorded the exact address it arrived at,
  so rules can file on it (the `alias` condition) and it shows as `deliveredTo`.
  Authorising the address is what makes the reply-From follow suit.
- **You can give a plus address its own name and signature.** Creating an identity for
  `you+shopping@example.com` used to be refused as a duplicate; it is a real identity
  of its own now, and it grants nothing your base address did not already allow.

### Notes for operators
- **No migration**; goose stays at **25**.
- **A catchall account behaves exactly as before.** The new rule is consulted only
  after the existing exact-address and whole-domain matches both miss, so which
  identity — and therefore which display name and signature — a subaddressed send
  uses is unchanged on a wildcard account. There is a test pinning this specifically.
- **The rule reads no message data.** It is a database fact (a verified base address)
  plus RFC 5233's `+` delimiter. The tempting alternative — "treat an address as
  yours once mail has arrived at it" — was rejected because `Delivered-To` is written
  by the sender, not by us: Mailroom is an IMAP client and never sees the real
  envelope recipient, so a hostile message could otherwise have added addresses to
  your own From picker. See **ADR-019**.
- Owning `you@d` still implies nothing about `boss@d`, and **disabling an identity
  disables its plus addresses too** — revoking an address revokes everything it
  covered.
- The one cost, stated plainly: on a mail provider that does not implement plus
  addressing, sending from one will bounce. That is visible and recoverable, and it
  is the price of a rule that consults nothing an attacker can write. If bounces are
  ever seen in practice, a per-provider gate would narrow this rule rather than widen
  it.

## [1.22.0] — 2026-08-01

### Added
- **Hitting Send with no connection no longer loses the message.** The send is saved,
  the compose window closes, and it goes out on its own when you reconnect — a
  reload, a closed tab or a restarted browser in between makes no difference. You are
  told which of the two happened: "saved and will send when you reconnect" rather than
  "Message sent."
- **Saving a draft offline works the same way** and stays a draft: nothing is submitted
  when the connection comes back.
- **A send interrupted mid-flight is not sent twice.** The case that used to be
  unfixable from the client: the send request reaches the server, the server schedules
  the message, and the *reply* is lost — a dropped connection, a closed laptop, a proxy
  timeout. The client cannot tell that apart from "it never arrived", so it has to try
  again. Every send now carries a key the server remembers, and a repeat gets back the
  send it already made instead of putting a second copy of your message on the wire.
- The pending-changes indicator counts a waiting send along with everything else, and
  names it (`Send "the invoice"`) on hover.
- **The unsubscribe button reports honestly too** — offline it says the request will be
  sent when you reconnect, rather than claiming it was sent.

### Notes for operators
- **Migration 00025** adds `submissions.idempotency_key` (nullable) with a partial
  unique index per user; goose goes to **25**. Nothing backfills, and existing rows
  keep a null key — the index is partial for exactly that reason.
- `EmailSubmission/set create` accepts an `idempotencyKey` (8–200 characters,
  `urn:mailroom:compose`, write-only). A key outside the bound is **refused**
  (`invalidProperties`) rather than trimmed: a truncated key could collide with
  another send's and silently suppress it. Keys are scoped per user, so two clients
  that mint the same one cannot cancel each other's mail.
- A send the server *answers and refuses* — an unverified From, over quota — is still
  reported immediately and is **not** queued. Only "unreachable" and "the session
  lapsed" are held for replay; a refusal replayed on a timer would be a loop that
  never sends.
- The narrow case that remains, stated plainly: if the browser dies in the moment
  between the draft being created and the client recording its id, the replay creates
  one **extra draft**. It does not send twice — the key still collapses that — so the
  worst outcome is an orphaned draft in the Drafts folder.

## [1.21.0] — 2026-08-01

### Added
- **Drag mail onto a label or a folder to file it.** Drag a row onto any label in the
  sidebar to apply it, or onto Inbox, Archive or Trash to move it there.
- **Dragging a selected row takes the whole selection with it**, which is what you meant if
  five rows are highlighted. Dragging an unselected row moves just that one and leaves your
  selection alone.
- **A drop that would change nothing is refused** — the folder the mail is already in, or a
  label every dragged message already has. The cursor tells you before you let go, rather
  than the drop landing and appearing to do nothing.
- Dragging is an extra, never the only way: the label menu, `e` to archive and `#` to trash
  all still work exactly as before.

### Notes for operators
- **No migration**; goose stays at **24**. Client-only.
- Drop targets are labels plus the Inbox / Archive / Trash roles. Sent and Drafts are
  deliberately not targets ("move a received message into Sent" has no meaning), and Junk is
  not one yet — the client's optimistic-move overlay has no junk state, so such a drop would
  appear to do nothing until the next sync.
- The payload travels under a private MIME type, so a list of message ids is never offered
  to a text field elsewhere on the page or to another application, and a drop payload the
  client does not recognise does nothing rather than guessing.

## [1.20.0] — 2026-08-01

### Added
- **Saved searches, and smart folders.** A search you run often can be given a name and
  kept. Tick "show in the sidebar" and it becomes a folder that fills itself — the same
  search, run fresh every time you click it, so it is never out of date.
- Save one straight from the search box, with the ☆ beside it, while you are looking at
  the results. The name is prefilled from the query and can be replaced in a keystroke.
- Settings → "Saved searches" lists them all, **shows each one's query rather than hiding
  it**, and lets you rename, re-word, unpin or delete. A folder whose contents you cannot
  explain is one you stop trusting.
- They follow you between devices.

### Fixed
- **Two signatures or templates with the same name no longer break the whole save.** Since
  1.15.0, saving one whose name was already used failed the entire request instead of
  telling you which one clashed — so if you saved several at once, the others were lost
  too. It now refuses just the clashing one and says the name is the problem.

### Notes for operators
- Migration **00024** (`app.saved_searches`, plus a `SavedSearch` value on
  `app.object_type`); goose moves to **24**. Applied automatically by the `migrate` one-shot.
- New capability URN: `urn:mailroom:savedsearches`, with `SavedSearch/get|changes|set`.
- The query is stored as the **operator string** exactly as typed, not as a parsed filter,
  so a saved search runs through the same path as typing it and cannot match different mail
  than the same text typed by hand. There is deliberately no `SavedSearch/query` method.
- The 1.15.0 fix above was a missing `SAVEPOINT`: a `UNIQUE` violation aborts the whole
  surrounding transaction (SQLSTATE 25P02), so per-object error reporting was decoration.
  `app.saved_searches` has the same shape and got it right from the start.

## [1.19.0] — 2026-08-01

### Fixed
- **The duplicates already in your mailbox can now be cleaned up.** 1.18.0 stopped new
  repeat deliveries becoming extra messages, but the ones already there stayed. Running
  `mailroom-admin collapse-duplicates` folds each set of copies back into one message, and
  the copy that is kept is the one that arrived first.
- **Everything attached to a copy is kept**, not dropped: every folder position it was
  found at, every label — including a label you only ever put on one of the copies — and
  any "important"/"not important" correction you made. The copies also stop taking up
  space; their stored contents are released.
- Anything Mailroom had **not yet finished doing** to a copy on your mail server stops it
  being folded away, so no pending action is silently cancelled. Those are reported instead
  of being skipped quietly.

### Notes for operators
- `mailroom-admin collapse-duplicates --user-id …` — **dry run by default**, `-apply` to
  act. The dry run lists every group and what would move.
- **No migration.** Requires `backfill-dedup-hash` to have run first: a `NULL` hash does
  not group, so there would be nothing to collapse.
- **Nothing is done to the mail server.** The redundant copies exist there too; removing
  them from there is a separate and much larger decision, deliberately not taken here.
- One transaction per group, and a group that cannot be fully accounted for is skipped
  whole rather than half-merged. Blob files are deleted inside the transaction, which is
  the same ordering the Trash purge uses and for the same reason.

## [1.18.0] — 2026-08-01

### Fixed
- **The same message no longer shows up several times.** Some mail is delivered more than
  once — a forwarder retrying, a provider queue doing its job — and each delivery arrives
  stamped with a few bytes of its own routing information. Mailroom identified a message by
  its exact bytes, so those few bytes made it a different message, and the inbox listed one
  newsletter three or four times. It now recognises a repeat delivery of something it
  already has and records it as another copy of the same message instead of a new one.
- **Nothing is thrown away** to achieve that. The repeat delivery is still recorded as
  having happened, in the folder and position it arrived at, so anything Mailroom does back
  to your mail server still knows about every copy.
- A **mailing-list copy is still its own message.** If the same message reaches you twice
  with different content — a list that appends a footer, for instance — the two stay
  separate, because they genuinely are.

### Notes for operators
- Migration **00023** (`app.messages.dedup_hash` + a partial index); goose moves to **23**.
  Applied automatically by the `migrate` one-shot.
- **Mail that arrived before this release is not protected until it has been backfilled.**
  The new check matches on a column existing rows do not have yet. Run
  `mailroom-admin backfill-dedup-hash --user-id … ` (dry run by default; `-apply` to write).
  It reports how many rows still lack a hash — `0` means every message participates — and
  how many rows in the mailbox are already redundant.
- **The backfill does not delete anything.** Collapsing rows that are already duplicated is
  a separate step, still to come, because it moves provenance and removes message rows.
- `mailroom-admin diff-duplicates` is a read-only diagnostic that explains *why* any
  remaining duplicates differ.
- `content_hash` is unchanged and is still a message's identity. See **ADR-018** for why
  this is a second hash rather than a change to that one.

## [1.17.0] — 2026-08-01

### Added
- **Search now tells you what it can do.** Mailroom has understood `from:`, `label:`,
  `is:unread`, `before:` and the rest since 1.0, but the only way to find out was a
  tooltip. Start typing and it offers the operators, each with a plain-English note about
  what it matches; type `label:` and your labels are listed; type `from:al` and it offers
  the people you have mail from.
- **Recent searches** are offered when the box is empty, and can be forgotten in one
  click from the same list.
- Arrow keys move through the suggestions, Enter runs the highlighted one, and Tab fills
  it in without running it — so operators can be stacked up before you search. Nothing is
  ever highlighted to begin with, so pressing Enter always runs exactly what you typed.

### Privacy
- **Recent searches stay in this browser.** They are not stored on the server, are not in
  any backup or export, and do not follow you to another device. A list of what someone
  went looking for in their own mail is more revealing than most of the mail it searches,
  so it is kept the same way the theme setting is, and the cost — the phone does not know
  what the laptop searched — is accepted deliberately rather than by omission.

## [1.16.0] — 2026-08-01

### Added
- **Unsubscribe, offered where you read the message.** Much real mail says how
  to unsubscribe in a header nobody sees; Mailroom now reads it and offers the two ways
  out it found — open the sender's own page, or send them the unsubscribe email they
  asked for, from the address they wrote to (which is the address a list will actually
  recognise).
- It **tells you what unsubscribing costs** before you do it: an unsubscribe is a reply,
  and a reply confirms your address is real and read. For a sender you signed up with
  that is fine. For one you did not, it is worth more to them than the unsubscribe. That
  is your call to make, so it is stated rather than buried.

### Security
- **Mailroom never unsubscribes for you, and never opens the link itself.** The one-click
  unsubscribe some senders advertise would mean this server fetching a web address chosen
  by whoever sent the mail — the one thing it has never done — so it is deliberately not
  built. The link is opened by your own browser, in a new tab that cannot reach back into
  Mailroom or tell the far end which message it came from.
- The address is checked before you are ever shown it: only secure `https` links and real
  `mailto:` addresses are offered. Anything else — a script link, an embedded document, a
  plain unencrypted link, or a link written to look like one site and go to another — is
  treated as no link at all. Where a message offers only a rejected link, its unsubscribe
  *email*, if it has one, is still offered.
- All of that checking happens on the server, so the browser is never handed a raw
  address from a stranger to make sense of.

## [1.15.0] — 2026-08-01

### Added
- **Send later.** Pick a time beside the Send button and the message goes out then.
  It rides the same durable scheduling the send-undo window already used, so a
  scheduled message survives closing the browser, signing out, and a server restart.
  Every option shows the actual moment it resolves to, because "tomorrow morning"
  is exactly the phrase that quietly means 3am.
- **Signatures, one per address if you want.** Save as many as you like, bind one to
  an address, and mark one as the one to use automatically — it is already in the
  message when compose opens, chosen for the address you are sending from. A
  signature written for one address is never put on mail from another.
- **Templates.** Save a message you send often and start from it. A template is
  offered only before you have started writing, and it never replaces or deletes
  text you have already typed.
- Both live under Settings → "Signatures and templates", where they can be edited
  and deleted, and both are per-account and follow you between devices.
- **Markdown compose.** A tick beside the plain-text toggle. What you type is still
  sent as the plain-text part exactly as written, with a formatted version alongside
  it — so nothing is lost for a recipient whose mail client shows plain text, which
  is the whole point of writing Markdown. Supported: headings, bold, italic, code,
  code blocks, links, quotes, lists and rules. Anything else is left as the literal
  characters you typed rather than half-formatted.

### Fixed
- The server now **enforces** the scheduling limit it advertises. It has always told
  clients it would hold a message for at most 30 days; it accepted longer anyway,
  which meant a message could be scheduled for a date nothing would honour.

### Security
- The Markdown formatter is ours and deliberately small, and it treats itself as if
  it were the security boundary even though the server sanitizes every outgoing
  message anyway: all text is escaped before any formatting is added, so HTML typed
  into a message stays visible text; the only link addresses it will produce are
  `http`, `https` and `mailto`; and it emits no other attributes at all.
- Nothing it produces is ever displayed inside Mailroom itself — it is built when the
  message is sent and handed straight to the server.

## [1.14.0] — 2026-08-01

### Added
- **You can tell Mailroom to always load a sender's images.** Remote images are
  still blocked by default for everyone else — this only stops it asking again
  about senders you have already decided about. The banner now offers two choices
  and says how wide each one is: *this sender*, or *anyone at their domain*.
- **Everything you have allowed is listed in one place, and can be taken back in
  one click** — Settings → "Senders whose images load", and from the message itself.
  When images load because of a remembered decision, the message says so and offers
  to stop, so it is never a mystery why one sender's pictures appear and another's
  do not.
- Your choices follow you between browsers and devices.
- A single message can still be opened without loading images without forgetting
  the sender, and loading images once still remembers nothing.

### Security
- There is deliberately **no "always load all images"**, and there is no way to add
  one: the stored form can only ever name one sender or one domain. It also refuses
  anything wider that looks like it might work — wildcards, "everything under this
  domain", a domain that would match by suffix — rather than storing a rule that
  quietly means more than you meant.
- Allowing a domain does **not** allow its subdomains, and allowing `example.com`
  does not allow `notexample.com`.
- Nothing about how mail is cleaned up before display changed. Blocking still
  happens the same way for every message; this release only decides which senders
  you are asked about.

## [1.13.0] — 2026-07-31

### Changed
- **Clicking an attachment now previews it instead of downloading it.** Preview
  arrived in 1.12.0 as a small extra button beside the attachment, which left
  downloading as the thing a click actually did — so the safer option was also the
  one you had to go looking for. Downloading is still one click away: a ⤓ beside
  the attachment, and again inside the preview once you have looked. An attachment
  with no preview keeps its download link, so a click never does something other
  than what it says.
- **The preview opens as a full overlay rather than a panel under the message.** It
  was squeezed into the reading-pane column, which is the wrong amount of room for
  a PDF or a scan; it now gets the window. Escape or a click outside closes it, and
  it behaves like the other overlays in the app.
- **You can move between a message's attachments without closing the preview** —
  arrows on screen, `←`/`→` on the keyboard, and a "3 of 7" counter so you know
  where you are.

### Security
- Unchanged. Same preview route, same single previewable media type, same
  no-origin response, same checks on what may be rendered. This release changes
  where the preview appears and what opens it, not what is allowed through.

## [1.12.0] — 2026-07-31

### Added
- **You can look at an attachment without downloading it.** PDFs, images and plain
  text now open in the reading pane, beside the download link rather than instead
  of it. Anything else says so plainly and offers the download.
- **This is a safety feature more than a convenience one.** Until now the only way
  to find out what an attachment contained was to save it and open it in a desktop
  application — handing a stranger's document to a full-featured program running as
  you. Looking first, in something that cannot act, is the safer order.
- Previewing is per-attachment and deliberate: opening a message renders no
  attachment and fetches nothing. Oversized files are refused with the reason
  shown, and long text says when it has been shortened.

### Security
- A previewed document is served with **no origin of its own**, so it cannot reach
  your session, your mail or the page around it — and it cannot make network
  requests, so previewing something cannot tell a sender you read it. The one
  media type served this way is PDF; everything the browser would treat as a
  document or a script is refused outright rather than served differently.
- Previewing runs the same per-user authorization as downloading, so a preview
  cannot reach another account's attachment.

## [1.11.1] — 2026-07-30

### Fixed
- **Accepting suggested rules from the command line no longer re-creates rules you
  already have.** It only ever checked whether a rule of the same *name* existed,
  which held as long as it was the only thing making rules — and quietly stopped
  holding once you could also accept a suggestion in the rules editor, because the
  two name the same rule differently. Accepting one in the browser and then running
  the command-line tool gave you two rules doing one job. It now compares what a
  rule *does* rather than what it is called, so this cannot happen from either
  direction.
- It also declines to re-create a rule you have **turned off**, and says so. An
  enabled copy of a rule you switched off is the automation overruling a decision
  you made on purpose.
- It can now tell you apart the two cases that used to look identical: "nothing met
  the bar" and "everything that met the bar, you already have".

### Added
- **It points out duplicate rules you already have**, naming both and which one is
  safe to delete — or saying they are identical and either will do. Duplicates do
  no harm to your mail, but a rules list longer than the number of decisions in it
  is one nobody can audit.

## [1.11.0] — 2026-07-30

### Added
- **All of your rules can now be run over your existing mail in one go, from the
  command line.** Applying a rule to old mail arrived in 1.10.0, but one rule at
  a time from inside the rule editor — which is the right shape for a rule you
  just wrote and the wrong one for a mailbox that has accumulated dozens of them.
  Doing it by hand meant one trip through the editor per rule, so in practice it
  did not get done and the rules sat there doing nothing to the mail they were
  written about.
- **It reports before it writes.** By default it changes nothing and instead
  prints what each rule matched, what it would newly label, what already had the
  label, and what it is leaving alone because you corrected it. The part worth
  reading is the list of disagreements: every message a rule wants to label one
  thing that you filed as another, with the date, sender and subject, so a
  seven-hundred-message change can actually be reviewed rather than just
  approved. The whole run can be reversed afterwards.
- It refuses to guess. A rule whose conditions look at recipients or at an
  arbitrary header cannot be answered without opening the message itself, which
  this tool does not do — so it names those rules and declines them instead of
  quietly reporting a number that would be too low.

## [1.10.0] — 2026-07-29

### Added
- **A rule can now be applied to the mail you already have.** Until now rules
  only ever ran on mail as it arrived, so the editor would tell you "this
  matches 47 of your messages", you would save it, and none of those 47 were
  touched. The count was a promise about the past that only paid out on the
  future. Rules written from a suggestion suffered most, since the whole reason
  they were suggested is that they explain mail already in the mailbox.
- It shows you what it will do before it does it, tells you afterwards what it
  **did not** do — what already had the label, and which parts of the rule only
  make sense for new mail — and offers to undo the whole thing.
- **It will not put back a label you took off.** If you have ever removed this
  rule's label from a message, applying the rule leaves that message alone and
  says how many it left alone. Undoing an apply is likewise treated as undoing
  an operation, not as you disagreeing with the rule several hundred times.

### Fixed
- A stray invisible character in one of the client source files had, since
  1.6.0, made the project's version-control tooling treat that file as binary —
  no readable history, no line-by-line review of changes to it. Behaviour is
  unchanged; the file is text again.

## [1.9.0] — 2026-07-29

### Added
- **Disagreeing with a rule now goes somewhere.** Taking a label back off a
  message a rule filed used to leave no trace: the label went, the record of
  which rule put it there went with it, and the rule carried on doing the same
  thing forever. The rules list now shows how many messages each rule has
  misfiled, and offers a narrower version of it — one built from the messages
  you kept, checked against the ones you rejected, and offered only if it
  actually excludes them. It shows what the narrower rule would stop labelling
  before you accept it, because that is the price of tightening and it is not
  always zero.
- Nothing is applied automatically. The narrower rule opens in the ordinary
  editor for you to look at and save, and saving it is what clears the record —
  a rule that keeps being corrected keeps saying so until it is actually fixed.
- When no narrower rule exists — because the mail you rejected looks identical
  to the mail you kept — it says that, rather than inventing a condition that
  merely looks discriminating.

### Fixed
- Two words that should have been recognised in a subject line were not
  (`ordered`, `deliveries` and their variants), and a check meant to match a
  sending domain could match a *different* domain that merely ended with the
  same letters. Re-measured across the whole mailbox: no category's score moved
  outside its normal variation, so this is recorded as a correctness fix and not
  as an improvement.

## [1.8.0] — 2026-07-29

Mailroom is now honest with itself about whether it has learned anything.

### Changed
- **A category model is now judged on senders it has never seen.** It used to be
  tested on a sample that included the very senders it learned from, so a model
  that had memorised a list of addresses looked expert. Measured against a real
  mailbox, that flattered every category — one scored near-perfectly on the old test and
  worse than a coin toss on the honest one. Nothing is trusted now unless it can
  categorise mail from someone it has not seen before, which is the only
  situation where it is any use: mail from a familiar sender is already handled
  by a rule, and more legibly.
- **What a subject *reads like* is now a signal in its own right** — receipt or
  confirmation language, sales-campaign language, alert or digest language —
  rather than the model relying on individual words, which are partly just the
  sender's name in disguise. It uses the same vocabulary the deterministic layer
  uses, so there is one definition rather than two that can drift apart. This
  measurably helped the one category it should have helped (Shopping) and left
  the rest unchanged, which is reported honestly rather than dressed up.

### Added
- **Operators can now ask how much a model's score is worth**, not just what it
  is: `mailroom-admin train-labels --folds N` re-runs the evaluation several
  times over different senders and reports the spread. A category whose score
  swings wildly depending on who was held back has a number that cannot carry a
  decision, however good its average looks.

## [1.7.3] — 2026-07-28

### Fixed
- **An address that forwards mail in is no longer treated as an address you gave
  to one company.** If you have an old address that everyone had for years and
  now forwards into your mailbox, everything arriving through it shared one thing
  — the forwarding — and "a hundred messages arrived at this address, where do these
  go?" is a question with no answer. Mailroom now checks whether most of an
  address's mail comes from a single sender: if it does, the address is the
  group, as before; if it does not, the senders are. On the mailbox this was
  found in, that turned one unanswerable pile into a few groups covering most
  of it.
- The "from N different senders" note now appears only when a group spans more
  than two. Two is the ordinary shape of a company that mails you from its own
  domain and its newsletter subdomain, and remarking on it taught you to ignore
  the note in the case that matters.

## [1.7.2] — 2026-07-28

### Fixed
- **"Needs a category" now separates the addresses you can answer in bulk from
  the mail that arrived on its own.** Groups appear first, then a **One at a
  time** heading, then everything else. Previously a two-message group could be
  followed by sixteen unrelated messages before the next heading, and with
  nothing announcing what those sixteen were, they read as part of the group
  above them. (v1.7.1 tried to fix this by outlining each group; an outline does
  not stop sixteen rows looking like a continuation. The rows needed a heading of
  their own.)
- **The keyboard cursor follows the list you can see.** `j`/`k`, shift-range and
  select-all walked the order the server returned rather than the grouped order
  on screen, so moving the cursor could jump somewhere other than the next row.

## [1.7.1] — 2026-07-28

Two dogfooding fixes in "Needs a category": the one thing you could not take
back, and a group that did not look like it ended.

### Fixed
- **Unrelated messages no longer look like part of an address group.** A group of
  one gets no heading — a queue of unrelated mail should not become a wall of
  headings — but with nothing marking where a group *ended*, the plain rows after
  it read as more of it: "2 messages arrived at `shop@`" sat above four
  rows, two of them a library notice and a tax advert. The count was right and
  the batch always acted on the right two; the list was drawing a boundary nobody
  could see. Group members now carry a rail and the last one closes it.
- **A batch answer can now be undone.** Filing a whole address group is one
  keystroke, and picking the wrong label was one keystroke you could not reverse:
  answering clears the selection and empties the group, so at the moment you
  realise the mistake you have neither the messages nor the view. The card that
  reports what the batch did now offers **Undo** — it is the only thing left that
  still knows which messages and which label — and it puts them straight back in
  the list to be answered again. (It cannot bring back suggestions the filing
  cleared; the messages return as simply uncategorised, which is enough for them
  to be asked about.)

## [1.7.0] — 2026-07-28

Mailroom starts having an opinion about what your mail *is* — and, more
importantly, a place to tell it when it is wrong or when it has no idea.

The through-line is that none of it decides anything for you. A category the
system is confident about is still only a suggestion; a category it is unsure
about becomes a question in a list you can clear in batches; and the rule that
would automate either is shown with how much other mail it would touch before
you accept it.

### Added
- **Mailroom can now learn a category from the messages you have filed under it,
  and it only ever suggests.** Once a label has enough hand-filed examples, new
  mail is scored against it, and the answer has three outcomes rather than two:
  yes, no, and *I do not know*. The third is the one that makes the feature
  trustworthy — a system that asks 10% of the time is more useful than one
  confidently wrong 3% of the time, because a wrong label is silent. Mail ends up
  somewhere you do not look and you find out weeks later, or never.
- **"Needs a category" — a place to answer the questions Mailroom cannot answer
  itself.** A view listing every message nothing has categorised: the ones the
  classifier looked at and was not sure about, and the ones no rule, no model and
  no label has ever touched. The uncertain ones come first, because those are
  where an answer teaches the most and costs the least.
- **Answer for a whole address at once.** Mail is grouped by the address it
  arrived at — "24 messages arrived at `bank@` — where do these go?" — so one
  keystroke files all of them instead of one. Mail with no distinguishing address
  is grouped by who sent it, so this works whether or not you run a catchall.
  Each group says how many different senders it spans, because an address you
  forward everything through is not the same kind of group as an address you gave
  to one company, and one label does not fit both.
- **What a batch implies, shown where you did it.** After filing a group,
  Mailroom says what a rule for it would keep doing and how much *other* mail
  that rule would newly label — the same number, in the same terms, as the rule
  editor's preview. It stays on screen rather than passing as a notification, and
  it is a blast radius to look at, not a score.
- **Why a category was suggested.** Suggestions now carry their reasoning and
  show it inline, in the same form as the priority explanations. Where the
  reasoning was never recorded, it says that rather than implying there was none.
- **"This does not need a category".** Some mail belongs in no category you keep,
  and until now the only ways to clear it were to file it somewhere false or to
  leave it in the list forever. Dismissing records nothing about any label being
  wrong — clearing a backlog means "not now" at least as often as "not this", and
  guessing which is how a system learns something you never said.
- An empty list now reads as what it is — everything has a category — rather than
  as an empty folder.
- **Dismissing is undoable.** Every other way of clearing a message can be taken
  back, and this one removes it from the only list that would ever ask about it
  again, so it now offers an undo. (It cannot bring back suggestions it cleared —
  the message returns as simply uncategorised.)

### Changed
- **A category is a category, whoever gave it.** The list of mail needing one now
  counts a message as sorted if it carries any label at all, not only if a model
  happened to have an opinion about it. Before this, the list was empty on any
  mailbox that had never trained a model — which is every mailbox on its first
  day, and the day the list is worth the most.
- **The classifier suggests; it never files.** It briefly did both — a confident
  score applied the label outright. The measurement that ended that: the test a
  model must pass to be trusted does not group its holdout by sender, so a label
  with eight messages from four senders scores near-perfect by recognising
  senders it trained on and gets promoted, while a label with twenty-two messages
  from seventeen senders scores near chance and never does. The labels that earn
  an automatic model are exactly the ones a one-line rule already handles, and
  the label that would genuinely need one cannot get there. Being wrong about
  where a message sits in a list is cheap; being wrong about where it was filed
  is not.

### Fixed
- The client-side core (view planning, the optimistic overlay, the queue's
  grouping) now has unit tests and runs them in CI. It was written to be
  testable without a browser and nothing had ever tested it, which is where every
  count this feature shows the user is computed.

### Internal
- **A deterministic "what kind of message is this?" layer** — promotional,
  transactional, notification, list mail, personal — read from envelope and
  header evidence rather than from a model, so it works on the first message a
  new mailbox ever receives. It is wired to nothing user-visible yet, and
  deliberately: measured against real mail, no binding from a kind to a label was
  good enough to offer. Reported here because it ships in the binary.
- **Operator tooling for measuring all of the above against real mail**, rather
  than against fixtures or intuition: `mailroom-admin classify` (what the kind
  layer decides, per message, with the evidence behind it), `kind-sweep` (how
  each label's mail concentrates, and how much *other* mail a binding would
  sweep up), `induce` (what rule every label implies, all at once), and
  `train-labels`. Three earlier rounds of weight tuning were reverted after these
  showed the numbers behind them were wrong; the tools exist so the next change
  is measured before it is believed.

## [1.6.1] — 2026-07-28

An operator-facing fix: a deploy that quietly did nothing now says so.

### Fixed
- **A deploy could silently leave the database ahead of the code.** Applying
  migrations against a database that a newer build had already migrated found
  nothing to do and said so cheerfully — indistinguishable from a correct
  deploy. It now says plainly that the database is ahead, and why that matters:
  migrations are recorded by version number, not by content, so a version
  already recorded is SKIPPED when a later release ships it. It warns rather
  than refusing, because deploying an older release on purpose is legitimate.

## [1.6.0] — 2026-07-28

Mailroom starts learning your categories from the way you already sort your mail
— by watching what you label, and by finally understanding the per-service
addresses a catchall user has been categorising with all along.

Both halves are about using what you have already told it rather than guessing.
A rule suggested from two messages you labelled yourself is not a prediction; an
address you handed to one company is not an inference. Nothing is applied for
you: suggestions are proposals, shown with what they would catch.

### Added
- **Catchall (wildcard) identities can now be set up.** If you run a catchall on
  your own domain and hand out a different address to every service —
  `shop@`, `bank@`, `post@` — Mailroom can now be told so. Because a wildcard
  identity is a claim to own an entire domain, it is set up by the operator at
  the server rather than requested from a mail client, which is the only place
  that claim can actually be checked.

  Until now Mailroom knew only your one fixed mailbox address, so every message
  recorded the same address and the per-service addresses you had already sorted
  your own mail with were invisible to it. Your own addresses were also filed in
  your address book as though they were people you correspond with.
- **Suggested rules can use the address a message came in on.** Handing out
  `crafts@` to a craft shop already says what that mail is; that is a category you
  stated yourself, with no guessing involved. Rule suggestions now use it — and
  it holds up when a company mails you from several different systems, which is
  where suggestions based on the sender's domain give up entirely.

  A retailer's advertisements and its receipts arrive at the same address, so
  the address alone is deliberately not enough to build a rule from: the bulk
  marker still has to separate them.
- **Rule suggestions, tightest first.** Two suggestions backed by equally many
  examples are now ordered by how much *other* mail they would catch.
- **Rule suggestions work on categories, not just single senders.** A label like
  "Promotions" spans many senders, and asking what *all* of them have in common
  has no useful answer. Suggestions are now made per group of related senders —
  four newsletters from one shop imply a rule; fourteen one-off senders imply
  nothing and are left alone.
- **Mailroom now suggests rules from your labelling.** Label a couple of
  messages into a category by hand and it offers to keep doing it — showing what
  else the rule would catch before you accept. It proposes; you accept, edit or
  ignore. Nothing is created for you.

  The hard part is not proposing a rule, it is proposing the *right* one: two
  labelled advertisements from a retailer imply `sender's domain`, and that rule
  eats the receipts. It instead finds what the examples share that the receipts
  do not — marketing carries `List-Unsubscribe`, order confirmations do not —
  and stops generalising at the point where the blast radius jumps. Verified
  against real mail: a bank sends both promotions and account notices
  from one domain, and the suggested rule takes the promotions only.
- **Rule previews warn about collisions.** "This would match 47 messages — 3 of
  which you filed under Shopping yourself." Only your own filing counts; a label
  another rule applied is not a conflict.
- **Create a label while building a rule.** The `addLabel` action can now make a
  new label without leaving the editor — which is exactly when you need one,
  since setting up categories is the moment none of them exist yet.

### Fixed
- **Mail to a catchall recorded the mailbox it landed in, not the address it was
  sent to.** Providers stamp both, and for a catchall they disagree: one header
  says which of your addresses the sender actually used, another says which
  mailbox the catchall dropped it into. Mailroom preferred the second, so every
  message looked identical and replies came from the wrong address. It now
  prefers the address the sender used. On a real catchall mailbox this changed
  nearly every message from one indistinguishable address to many distinct ones.

  If you do not run a catchall this changes nothing at all: an address you are
  not verified to send as is skipped whatever order it is considered in.
- **Selection checkboxes did not appear ticked until you clicked another row.**
  The selection was always correct — the count and every bulk action used the
  right messages — but the checkbox itself rendered a step behind, so ticking one
  looked like it had ticked the previous one.
- **A rule could be saved with no label to apply, and would then do nothing —
  permanently and invisibly.** The server deliberately skips an unresolvable
  label rather than failing an ingest, so such a rule saved cleanly, listed as
  enabled, evaluated on every message and had no effect. Saving one is now
  refused, and a rule whose label was deleted *after* the fact is flagged in the
  rules list.

## [1.5.0] — 2026-07-26

The rules engine gets an interface. It has been filtering mail since 1.0 with no
client at all — this release is the editor, the "make a rule out of this message"
flow, and the preview that tells you what a rule would do before it does it.

### Added
- **`Rule/preview` — a rule's blast radius before you save it.** Give it a rule
  that does not exist yet and it reports how many of your existing messages it
  would have matched, with examples. It evaluates through the same matcher the
  ingest path uses, so the preview cannot disagree with the engine, and it writes
  nothing. The scan is bounded and reports its own bound: a count over the most
  recent messages is never presented as a whole-mailbox answer. Rules testing
  recipients or arbitrary headers have to open message bodies, so they scan a
  smaller window and say so.
- **Labels now record who applied them.** A label a rule added reports that rule,
  by name, on the message (`labelProvenance`) — so a surprising label is
  traceable to its cause instead of being indistinguishable from one you applied
  and forgot. Deleting a rule keeps its labels and drops only the attribution.

- **A rules editor.** The T0 rules engine has been filtering mail since 1.0 with
  no interface at all; this is the interface. A rules list in evaluation order, a
  condition editor covering the full nestable `all`/`any` tree — including
  `List-Id` and arbitrary headers, which are what separate a retailer's promotion
  from that retailer's receipt — and the actions the server will actually accept.
  Actions it rejects are not offered.
- **"Create a rule from this message."** From an open message, one click starts a
  rule already filled in from what is in front of you — the mailing list if there
  is one, otherwise the exact sender, with the broader options (the whole domain,
  a subject match) offered next to a blast radius so you can see what each would
  catch before saving. Typing conditions from scratch is why rule systems go
  unused.
- **One-keystroke labelling.** The label menu opens focused with a filter; `1`–`9`
  apply the first nine labels, and typing narrows the list. `l` then `1` labels a
  message without your hands leaving the keyboard.
- **`Email.listId`** — the List-Id value, null for mail that is not from a list.

### Fixed
- **Rule-applied labels claimed the user had applied them.** The `applied_by`
  column has existed since the first schema with the vocabulary
  `user|rule|ml|agent`, and nothing ever wrote to it, so every automatically
  applied label took the `'user'` default. Existing rows are left as they are —
  nothing recorded the truth at the time, and inventing it retroactively would be
  a guess wearing a fact's clothes.

## [1.4.1] — 2026-07-26

### Fixed
- **Marking a message important failed with `unknownMethod`.** The web client
  called `Triage/correct` without declaring `urn:mailroom:triage` in its `using`
  set, and the server correctly refuses a method whose capability the caller did
  not declare. Only that one write path was affected — the Priority view, the
  importance divider and the "why is this ranked here?" panel read properties
  that are gated per-property rather than per-method, so they worked throughout.

### Added
- A contract test asserting that every JMAP method the web client can call has
  its capability declared, and that every capability it declares is one the
  server advertises. The existing API tests invoke handlers directly, which
  bypasses the gate this class of bug lives behind, so nothing could have caught
  it before it reached a user.

## [1.4.0] — 2026-07-26

The priority inbox. The engine for it has been scoring mail since 1.0 and had no
interface at all; this release builds the interface — and fixes the scoring
underneath it, which turned out to rank newsletters above everything else.

### Added
- **A Priority view.** Your inbox, re-ordered so what the model thinks matters
  leads, with a divider between the two. It is the whole inbox, not a filter —
  a view you have to leave to see the rest is a worse inbox, not a better one.
  Reachable from the sidebar, the command palette, or `g p`.
- **An importance marker on every row, and one click to disagree.** Marking
  something important or not is not just a correction to the list: it teaches
  the model, which re-scores from it.
- **"Why is this ranked here?"** in the reading pane — the actual reasons behind
  the score, in plain language ("You have written to this sender", "Bulk mail
  (has unsubscribe)", "Mails you often, never answered"). A ranking you cannot
  interrogate is one you cannot trust, and one you cannot correct sensibly.

### Fixed
- **Priority scoring ranked bulk mail highest.** On a real mailbox, nearly
  everything came out "important" and the top of the ranking was a daily automated
  digest. The cause was that "known contact" meant "is in your address book",
  and the address book is built from everyone who has ever mailed you — so the
  more something mailed you, the more important it looked. A newsletter was
  therefore the closest correspondent you had.
  What counts now is whether **you have written to them**. Frequency without a
  reply means the opposite of intimacy, and is treated that way: mail from
  someone you have never answered is demoted once it becomes a pattern, as is
  mail from an address that does not accept replies. Bulk markers a sender
  volunteers (`List-Unsubscribe`, `List-Id`) are weighted properly, and no
  longer let a promotion collect the "addressed to you personally" bonus.
  After the change far fewer messages came out important, the digest fell well down
  the ranking, and the newsletters went to the bottom.
  Correspondence is read from your **Sent** mail rather than from sends made
  through Mailroom, so importing an existing mailbox teaches it everyone you
  have ever written to, in one pass.
- **The model can now learn from bulk signals at all.** `List-Id`,
  `List-Unsubscribe`, the recipient count and the sender-authentication verdict
  lived only inside the message file, so the training path could not see them:
  at training time no message had ever been bulk. They are now recorded
  alongside the message (migration 00016), so what the scorer sees live and what
  the trainer learns from are the same facts.

### Added (operator)
- **`mailroom-admin rescore`** re-derives those signals for existing mail and
  re-scores it, printing a distribution histogram and how much would be
  important at several thresholds. Dry-run by default. Scores are computed once
  at delivery, so without it a scoring change never reaches mail you already
  have — and the histogram is how a change gets checked against real mail
  instead of intuition.

## [1.3.0] — 2026-07-25

### Changed
- **Snoozed mail comes back at the top of the inbox.** It used to return to the
  position its age gave it — a message snoozed on Monday and woken on Friday
  reappeared in the middle of the week's mail, which is exactly the invisibility
  snoozing is meant to prevent. A returning message now sorts by *when it came
  back*, so it lands where you will see it, and carries a "Came back …" note for
  a day so an older message at the top of the list explains itself rather than
  looking like a broken sort. Bringing a message back by hand does the same
  thing, because "bring it back" should mean back into view.
  The date on the message is **not** rewritten to achieve this: that is when the
  mail arrived, it is what your mail server says, and it still drives date
  searches, threading and any other client's sorting. Mailroom records the
  return separately and orders by whichever is later.

## [1.2.0] — 2026-07-25

### Added
- **Ctrl+Enter sends** (⌘+Enter on a Mac), from anywhere in the compose window —
  the body, the subject, the recipient fields, the rich editor. Plain Enter
  still does what it always did in each of them, including inserting a newline
  in the body; and with the recipient autocomplete open, Ctrl+Enter sends rather
  than picking a suggestion, because an explicit send should beat a completion.
  The Send button carries the shortcut in its tooltip and it is listed in `?`.

### Fixed
- **Sent mail really does leave the Drafts folder now.** 1.1.2 only removed the
  draft copy if it had already reached the server *and* been synced — but a send
  normally beats its own draft upload, so in the ordinary case there was nothing
  to find and nothing was done; the upload then landed a minute later and put the
  message back in Drafts. The cleanup no longer waits to be told where the copy
  is: it cancels the pending upload, so the copy is never created, and queues the
  removal of one that did land, whether or not Mailroom has seen it yet.
- **Deleting a sent message no longer fails with a database error** — and, more
  seriously, can no longer delete the copy in Sent. Destroying a draft removes
  every copy of it from the server, which is right for a draft and catastrophic
  for a message that has been sent: it would take the Sent copy with it. A
  message that has been handed to the send path is therefore never treated as a
  disposable draft; it is moved to Trash like any other message. A message still
  queued to send says so instead, rather than being silently trashed while the
  send goes out anyway.
  The raw `SQLSTATE 23503` this surfaced as came from the same root: the
  submissions table was the only thing referencing a message without saying what
  should happen when that message is deleted, which quietly made any message
  with a send record undeletable — including, in 30 days' time, by the Trash
  purge, and by account deletion. It now cascades (migration 00014).
- **A repair for mail already stranded in Drafts by the previous release's bug.**
  `mailroom-admin repair-sent-drafts --user-id <id> --apply` expunges the leftover
  Drafts copy of each already-sent message and clears its stale Draft tag,
  leaving the Sent copy and the message itself untouched. Dry-run by default.
  This is the supported way to clean those up: deleting them by hand would have
  removed the Sent copy too.

## [1.1.2] — 2026-07-25

### Fixed
- **Sent mail leaves the Drafts folder.** A message you sent kept a copy in
  Drafts and went back to being tagged Draft shortly after sending. Drafts are
  mirrored to IMAP on purpose, but sending never removed that mirrored copy, so
  the message legitimately lived in two folders at once — and because the copy
  still carried `\Draft` on the server, the next sync read that flag back over
  the "this has been sent" the send had just written. Sending now expunges the
  draft copy from Drafts as part of filing the sent copy in Sent.
  The expunge names its target by folder and Message-ID rather than by position,
  so it cannot fire at a message that has since been renumbered, and if the
  message has no Message-ID it is left alone rather than guessed at —
  duplication over data loss. Mail sent *before* this release keeps its stale
  Drafts copy: nothing distinguishes it from a genuine draft with the same
  content, so it is not repaired automatically. Delete those copies by hand.

## [1.1.1] — 2026-07-25

### Fixed
- **The reading pane moves on with you.** Snoozing the message you were reading
  left it on screen after it had gone from the list, and archiving or deleting
  closed the pane outright and dropped you back to a bare list. Both are now one
  behaviour: when an action takes the open message out of the current view, the
  pane advances to the next message down — the previous one when it was the
  last, and closes only when there is nothing left. It advances only when the
  message really leaves: archiving a selection that does not include what you
  are reading leaves the pane alone, and snoozing from Starred (which does not
  hide snoozed mail) keeps the message open.

## [1.1.0] — 2026-07-25

The first additive release of the v1 line: local-first organization (spec §6)
lands complete — pin, mute and snooze — alongside the reading-pane layouts and
the licence decision. Nothing here changes an existing behaviour, and no
migration rewrites existing data: 00012 and 00013 each add one nullable column.

### Added
- **Pin a message, mute a conversation.** Pinned messages sort to the top of the
  list and carry an accent edge so their position reads as chosen rather than
  merely recent; they order by when you pinned them, not when they arrived.
  Muting is per-conversation — the unit people actually mean — and the reading
  pane says so, because muting is easy to forget you did. Both are available
  from the list, the reading pane, the command palette, and the keyboard (`p`
  and `m`). Neither writes back to IMAP: there is no `\Pinned` flag and no
  thread-level mute in the protocol, so both are local-only by design, while
  still syncing across your own devices.
- **Snooze a message and it comes back.** Pick a time — later today, tomorrow,
  this weekend, next week, or an exact date and time — and the message leaves
  your inbox until then, waiting in a new Snoozed view ordered by when each one
  returns. Only the inbox hides snoozed mail, deliberately: a folder answers
  "what is in here", and a message you snoozed and then deleted must still be
  in Trash where you go looking for it. `b` snoozes from the list or the reading pane (and, in the
  Snoozed view, brings a message back immediately); the command palette and a
  row action do the same. Every choice shows the instant it resolves to, because
  a menu that only says "next week" is asking to be misread, and a wake time in
  the past is refused rather than quietly accepted.
  The wake rides the durable job model, so it survives a restart: a wake whose
  time passed while the server was down fires on the next start, and each wake
  schedules the next one from the data — a month-long snooze costs one timer,
  not a poll every minute, and a timer that is somehow lost is repaired at boot.
  Waking is idempotent by construction (it asks the database what is due now,
  rather than trusting a list of ids recorded when the timer was set), so a job
  that fires twice wakes nothing the second time. Being snoozed is a question
  about the clock, not a flag: the moment a wake time passes, the message is
  back in the inbox view whether or not the timer has run yet.
  Local-only, like pin and mute: there is no IMAP move, so another mail client
  still shows a snoozed message in its folder. That is deliberate — a move is
  the one operation that touches canonical placement, the expunge reaper and
  provenance generations at once, and a snooze is a statement about your view,
  not a reason to relocate your mail on the server.
- **The reading pane can sit beside the list, below it, or not at all** — and
  the divider is draggable. Beside-the-list stays the default; below-the-list
  gives a wide screen the full width for line length; off turns the list
  full-width and opens messages as a page. The divider is a real splitter: drag
  it, or focus it and use the arrow keys (Shift for a coarser step, Home/End for
  the limits, Enter or a double-click to reset). Each orientation remembers its
  own size, because a width that reads well as a list column makes a poor
  height. The setting lives beside theme and density.
- **Print, and therefore save to PDF.** A Print action in the reading pane and
  the command palette, plus a print stylesheet: the sidebar, list, toolbars and
  buttons drop away and the message prints on its own, with link targets written
  out beside their text since a printed hyperlink is otherwise invisible. It
  goes through the browser's own print dialog, so Save-as-PDF works everywhere
  with no new dependency. Printing deliberately drives the app window rather
  than the message frame: the frame's sandbox is pinned by a security test and
  carries no `allow-modals`, and widening it for a print button would be a poor
  trade.
- **Mailroom is licensed under the GNU AGPL-3.0.** The licence decision was
  deliberately deferred until pre-publication and is now made and recorded
  (ADR-011): `LICENSE` at the repo root, and a one-line
  `SPDX-License-Identifier: AGPL-3.0-only` on every source file rather than a
  header banner on each. The deciding argument was reversibility — a sole
  copyright holder can relicense AGPL → permissive later, but not the reverse
  once outside contributions land — reinforced by the AGPL matching the
  project's reason for existing: if you run a modified Mailroom as a network
  service, your users get your source; self-hosting it for yourself carries no
  such obligation. Some organizations ban the AGPL outright, and that cost was
  accepted knowingly.

## [1.0.1] — 2026-07-25

### Added
- **The server reports its own version.** Release images are stamped via a
  `MAILROOM_VERSION` build arg (`docker build --build-arg MAILROOM_VERSION=…`),
  which threads into the binary as `main.version`, and a `--version` flag reads
  it back without booting the server or reaching a database
  (`docker run --rm <image> --version`). Previously the ldflag the code was
  written for was never actually passed, so every image — including the one
  deployed for 1.0.0 — reported `0.0.0-dev` and there was no way to confirm
  which release a running instance was. An unstamped build still reports
  `0.0.0-dev`, on purpose: a local or CI build must not claim to be a release.

### Changed
- **Runtime base image moved to Alpine 3.24** (from 3.20). The server links the
  search engine through CGO against musl, so the base image is load-bearing
  rather than incidental; the end-to-end suites that actually boot the built
  image both pass on it, which is the only check that exercises the linked
  binary on the real runtime filesystem.

### Fixed
- **e2e harnesses fail at the point of failure.** The readiness loops in the
  gate and inline-image suites ended silently when a service never came up, so
  the script carried on and died several commands later on something unrelated
  — a real CI run reported `container … is not running` from the next command,
  which said nothing about the database having failed to start. All three
  suites now report which service timed out, its final state and its last log
  lines, and stop there. The inline-image suite's database check also raced
  first-time initialization: it asked over the Unix socket, which answers
  "ready" from the temporary init server before the real database exists.
- **The web client builds against current TypeScript.** From TypeScript 5.7 the
  typed arrays became generic over their backing buffer and the DOM narrowed
  `BufferSource` to reject shared buffers, so the Web Push VAPID key no longer
  satisfied `pushManager.subscribe`. Its buffer is now allocated explicitly,
  which is what the code always meant. No runtime change; it unblocks the
  pending web dependency update.

## [1.0.0] — 2026-07-24

The MVP milestone (spec §17). Everything the 1.0 scope called for is in:
multi-account IMAP sync with unified inbox and threading, bulk archive import,
hybrid labels, full-text search, alias-aware compose, delayed submission with
Undo Send, the outbound ledger and DSN pipeline, OIDC multi-user with full
row-level isolation, the T0 rules engine and T1 priority inbox with
explainability and an eval harness, internal contacts, the security baseline
including a restore drill, the web app with PWA and push, and a documented
RFC 8620/8621-conformant API subset.

This release closes it out with the two bugs that dogfooding real mail turned
up — one of which was writing to the user's mailbox.

### Fixed
- **Syncing no longer marks every message read.** The IMAP body fetch used
  `BODY[]` where it must use `BODY.PEEK[]`. The non-peek form *implicitly sets
  `\Seen`* (RFC 9051 §6.4.5), so the act of syncing a message marked it read —
  on the server, for every client on the account, not just in Mailroom — and the
  next flag reconcile then read that back as the truth. Unread was consequently
  unreachable: new mail arrived already-read and nothing could stay unread.
  Syncing observes; it must not mutate the mailbox it is observing. A live
  Dovecot regression test now appends an unseen message, issues the exact fetch
  the sync engine issues, and asserts the message is still unseen afterwards —
  a property only a real server can verify, which is why the original defect
  (one zero-valued struct field) survived the stubbed tests.
  **Existing mail is not repaired by this fix:** messages already marked `\Seen`
  on the server stay that way, because nothing distinguishes them from mail that
  was genuinely read. Mark a folder unread from any client to reset it.
- **Reading pane fills its width.** The message body sat in a second, narrower
  box inset inside the reading pane, with the pane's dead space stacked on one
  side. The body's measure cap is now centred rather than left-pinned, is wide
  enough for the ~600px tables newsletters ship, and the HTML body is flush with
  its message card instead of framed twice. The auto-height contract that fixed
  the earlier double-scroll is untouched.

## [0.25.0] — 2026-07-14

### Added
- **Warm IMAP sync connection pool.** Each account now keeps ONE authenticated
  sync connection and reuses it across passes instead of dialing TCP+TLS+LOGIN
  every time — worthwhile now that IDLE triggers frequent syncs. Before every
  reuse the connection is validated with a NOOP probe; if it fails (a server that
  dropped the idle connection, or an eviction past the idle timeout) the pool
  discards it and re-dials, so a stale connection can never make a pass silently
  do nothing. If a reused connection passes its probe but then dies mid-pass, the
  pass is retried once over a fresh dial, so pooling never turns a
  live-connection race into a skipped sync (mail latency). The per-account sync
  gate already serializes passes, which is what makes a single warm connection
  safe, and the connection cap is unchanged (one IDLE connection + one sync
  connection per account). On by default; set `MAILROOM_IMAP_POOL=0` to dial a
  fresh connection per pass, and `MAILROOM_IMAP_POOL_IDLE_TIMEOUT` tunes how long
  a warm connection may sit unused before eviction (default 5m).

## [0.24.1] — 2026-07-14

### Fixed
- **Preview backfill for already-synced mail.** A new `mailroom-admin resnippet`
  command re-derives message previews from stored blobs using the fixed text
  extractor, so mail ingested before the preview fix (v0.23.1) gets clean previews
  without a re-sync. It defaults to a dry run, only rewrites previews that actually
  change, and — because the preview is a JMAP-visible property — records an Email
  update so clients pick up the new text.

## [0.24.0] — 2026-07-14

### Added
- **Near-instant mail delivery (IMAP IDLE).** New mail now triggers a sync within
  seconds instead of waiting for the next poll tick. A per-account IDLE watcher holds a
  separate IMAP connection, and when the server pushes an EXISTS hint it fires the SAME
  cursor-driven `SyncAccount` pass the scheduler runs — IDLE is purely additive, never a
  source of truth. Polling remains the fallback and safety net: while a watcher is down,
  backing off, or on a server without IDLE, the account stays poll-only and no mail is
  lost. A new per-account gate serializes and coalesces passes so a scheduled sync and an
  IDLE-triggered sync for one account never overlap, and a hint arriving mid-pass yields
  exactly one guaranteed follow-up. IDLE-triggered syncs are debounced per account
  (honoring provider poll floors) so a chatty mailbox cannot hammer a throttled provider.
  On by default; set `MAILROOM_IMAP_IDLE=0` for poll-only, and `MAILROOM_IMAP_IDLE_TIMEOUT`
  tunes the RFC 2177 re-issue period (default 29m). The reserved per-account
  `idle_connection_state` metric is now driven (0 down / 1 connecting / 2 idling).

## [0.23.1] — 2026-07-14

### Fixed
- **Message previews no longer show raw HTML.** The list preview is now extracted
  as clean text: `<style>`/`<script>` blocks are dropped entirely (no more
  `.Roll2:hover{…}` leaking into the preview), HTML entities are decoded, and
  HTML-only messages — which previously showed a blank preview — now derive their
  preview from the HTML body. (Existing messages refresh their preview on re-sync.)
- **Read vs unread is now unmistakable.** Unread messages get an accent dot before
  the sender and bolder text; read messages recede (muted), with a stronger unread
  background tint in dark mode. The cues are shape- and weight-based, not colour
  alone. (The read/unread state itself was always correct — it reflects the
  server's `\Seen` flag.)
- **The reading pane no longer double-scrolls.** A box-model bug made each message
  frame render 2px shorter than its content, forcing a redundant inner scrollbar on
  top of the pane scroll; the frame now fits its content exactly and the pane is the
  single scroll region.

## [0.23.0] — 2026-07-13

### Added
- **Take control of triage (model controls + one-click correction).** Three new JMAP
  methods under `urn:mailroom:triage` turn the priority model from a black box into
  something you steer. `Triage/get` shows your model's state — the active version and
  its full lineage of snapshots. `Triage/set` is the safety valve: `reset` starts your
  model over from a fresh baseline (breaking lineage), and `rollbackTo` re-activates an
  earlier version without ever deleting one, so you can always roll forward again.
  `Triage/correct` is the explicit training signal that pairs with the explanation
  display: mark a message `important` or `notImportant` and Mailroom records a feedback
  event the model learns from over time. Corrections for a message you cannot see are a
  silent no-op. The controls and the delivery scorer share one model registry, so they
  never disagree on which model is live.

## [0.22.0] — 2026-07-13

### Added
- **See *why* a message got its priority (triage explainability — display).** When a
  message is scored at delivery, Mailroom now records the top per-feature reasons
  behind that score (e.g. "From a known contact", "Mailing-list mail", "Failed sender
  authentication") and exposes them on the JMAP Email object as a new opt-in
  `priorityReasons` property under `urn:mailroom:triage` — each reason a short
  human-readable label with its signed weight and an up/down polarity. Both the
  trained model and the cold-start heuristic explain themselves, and the reasons sum
  consistently with the score. No new API method and no correction surface yet (that
  follows); existing clients are unaffected (the property is opt-in).

## [0.21.0] — 2026-07-13

### Added
- **Sensible priority from the very first message (cold-start scoring).** Before the
  triage model has learned anything from you, incoming mail is now prioritized by a
  deterministic heuristic instead of sitting at a flat neutral score: mail from
  people you know or have replied to, and mail addressed directly to you, ranks up;
  mailing-list mail, one-click-unsubscribe bulk, and messages whose sender
  authentication (SPF/DKIM/DMARC) clearly failed rank down. The moment enough of
  your own feedback accrues, your trained model takes over automatically. Sender
  authentication is now also a scoring signal, evaluated conservatively so ordinary
  mail that simply lacks auth headers is never penalized.

## [0.20.1] — 2026-07-13

### Fixed
- **The unified inbox now includes your account's INBOX.** A mailbox's role was
  detected only from its IMAP SPECIAL-USE attribute, but INBOX carries none (it is
  the inbox by its reserved name, not a flag), so it was left role-less and dropped
  out of the unified inbox. The inbox is now recognized by name. The same root
  cause also silently broke "skip inbox" rules and moving a message back to the
  inbox by role; both are fixed.

## [0.20.0] — 2026-07-13

### Added
- **Priority scoring now learns from your actions (T1 triage — learning loop).**
  The triage model no longer sits at a neutral baseline: it trains on how you
  actually treat your mail. Everyday actions are captured as feedback — marking
  read, starring/flagging, archiving unread, trashing, and replying/forwarding
  (the reply is credited to the message it answers) — recorded as a side effect of
  the actions you already take, with no new API and never able to fail or slow the
  action itself. A daily per-user background pass trains a fresh model on recent
  feedback and promotes it **only if it ranks a held-out slice at least as well as
  the model currently in service**, so a noisy stretch can never degrade a good
  model; every prior version is retained for rollback. Cold-start is respected: too
  little feedback (or only one kind of action) simply skips training. Known limit
  for now: three signals that live only in the raw message (mailing-list headers and
  the recipient count) are not yet learned from replay and will follow a later data
  change.

## [0.19.0] — 2026-07-13

### Added
- **Priority scoring on incoming mail (T1 triage — serving path).** Every newly
  delivered message is now scored for priority (0–1) at delivery and the score is
  recorded, exposed on the JMAP Email object as `priorityScore` and a thresholded
  `isImportant` under a new `urn:mailroom:triage` capability (opt-in properties, so
  existing clients are unaffected). Scoring runs a lightweight in-process model with
  no external service, and is strictly best-effort: a scoring failure can never
  delay or lose a message. A single feature function serves both live scoring and
  offline replay, so the model the next wave *trains* sees exactly the features it
  *serves*. Until the learning loop lands (next wave), scores sit at the model's
  neutral baseline — the pipeline and read surface are in place first.

## [0.18.0] — 2026-07-13

### Added
- **Model lifecycle foundation (the ML registry + evaluation harness).** Internal
  plumbing the learned triage sits on: per-user model snapshots stored as encrypted
  blobs with version lineage, exactly-one-active-per-kind enforced in the database,
  and rollback / **reset** ("start my model over from a clean slate") — every prior
  version is retained for audit and manual rollback, nothing is ever silently
  discarded. Alongside it, a model-agnostic evaluation harness replays recorded
  feedback (opens, replies, archives) against a candidate model and reports ranking
  quality (AUC), so a new model can be judged before it ever goes live. This release
  defines the `Model` and feature interfaces the scorer will implement; the scoring
  itself and its user-facing controls land in the next waves. No API surface changes.

## [0.17.0] — 2026-07-13

### Added
- **Filtering rules (the deterministic T0 triage tier).** You can now define rules
  — conditions on sender, domain, the address you were reached at, List-Id /
  List-Unsubscribe, any header, or a regex, combined with all/any — that apply
  actions to incoming mail: add a label, mark it priority, flag it, or skip the
  inbox (the message is filed away, never deleted). Rules are managed over a new
  `Rule/*` JMAP surface (`urn:mailroom:rules`) and evaluated at delivery. A broken
  rule can never lose or misfile mail (each action runs isolated; a failure is
  skipped and the message delivers normally). This is the transparent, instant
  layer the upcoming learned triage will sit on top of. (Move-to-mailbox and junk
  actions are deferred to a later tier that owns server-side writeback.)

### Added
- **Machine-readable API docs (OpenAPI 3.1 / JSON-Schema).** `docs/openapi.yaml`
  formally describes the JMAP surface — all six HTTP routes, the Session object,
  every advertised capability, and the core method arg/response shapes — validated
  in CI. Extension URNs that depend on not-yet-built features (triage, keys, rules)
  are documented as deferred rather than advertised.

### Changed
- **`Email/queryChanges` now computes real deltas for stable queries** (an
  unfiltered inbox, or filters over mailbox/keyword/date) instead of always asking
  the client to re-query. Volatile queries — full-text search, cross-thread or
  cross-account, or windowed — still honestly return "can't calculate changes."
  Correctness leans on the invariant that every entry to or exit from a result set
  already appends a change-log row.

## [0.15.0] — 2026-07-13

### Added
- **IMAP connection hardening (Stage 1): each account now syncs on its own
  schedule.** Instead of every account re-syncing in a single global lockstep,
  each account is scheduled independently around the configured base interval:
  a failing account backs off on its own timeline (exponential, jittered, capped
  at a new `MAILROOM_SYNC_MAX_BACKOFF`, default 30m) without ever delaying a
  healthy one, and per-provider minimum-interval floors keep aggressive providers
  (e.g. Gmail) from being polled too often no matter how tight the base interval.
  `MAILROOM_SYNC_INTERVAL` is now the per-account **base** interval (compatible
  meaning). Mail is never dropped and per-account error isolation is unchanged.

### Changed
- **Disaster-recovery drill now exercises restore-then-migrate-forward.** The
  backup/restore drill can build a fixture one schema version behind head, back
  it up, restore it, and migrate it forward to head before proving the data — the
  case a real restore always hits (your backup is older than your code). Wired
  into the drill's self-test. (Internal DR hardening; spec §16.)

## [0.14.0] — 2026-07-13

### Added
- **Search hardening: index staleness is now surfaced, not just tracked
  internally.** Account health reports how many minutes behind the change log
  your search index is (the age of the oldest change it hasn't indexed yet),
  omitted when this server has no search backend installed. The account-health
  panel shows a plain "search up to date" / "search may be up to N minutes
  stale" line underneath the per-account cards.

## [0.13.0] — 2026-07-13

### Added
- **Storage-quota enforcement now covers composing and syncing.** Creating a
  message/draft that would exceed your storage quota is refused up front (nothing
  is stored), and — the important one — if your mailbox provider still has mail
  you have no room for, sync **pauses and flags it in account health rather than
  dropping anything**: the message is left on the server and picked up on the next
  pass once there's room. (Uploads were already enforced.)

## [0.12.0] — 2026-07-13

### Added
- **Installable PWA with Web Push notifications.** Mailroom can be installed to
  the home screen / desktop, and (where the browser supports it) you can enable
  push notifications for new mail from Settings. Notifications are triage-aware by
  design (the "important-only" preference is stored now; it starts filtering once
  priority scoring lands). The app also caches its shell for offline loading. The
  push signing key is derived from the server's master key (no new secret to
  manage), and notifications are sent per-user under the same row-level isolation
  as everything else (see `docs/adr/ADR-016`).

## [0.11.0] — 2026-07-13

### Added
- **Account health panel.** Each account now shows its sync state ("synced 3h
  ago" or a clear error with the consecutive-failure count), recent delivery
  failures, writeback dead-letters, and the send-quota meter — so a stalled or
  misconfigured account is visible without reading server logs. Per-account sync
  outcomes are now persisted (previously only logged).
- **Rich-text compose.** The compose body is now a WYSIWYG editor (bold, italic,
  underline, lists, links) with a plain-text toggle, inline images, drag-and-drop
  attachments, and an alias hint that defaults your From address to the one a
  known correspondent reached you at. Outgoing HTML is sanitized server-side, and
  the editor pastes as plain text; the one place the client touches `innerHTML`
  is the editor, narrowly and provably safe (see `docs/adr/ADR-015`).

### Fixed
- **CI: the two end-to-end security jobs now run in the Forgejo runner.** The
  login-gate and hostile-content e2e scripts had never passed in CI (a
  `set -o pipefail` bashism the runner's dash rejects, then host bind mounts that
  don't resolve when the job is itself a container). Both are now dash-safe and
  use `docker cp`; the hostile-content real-browser proof runs rather than
  silently skipping.

## [0.10.0] — 2026-07-13

### Added
- **Unified inbox, first-class per-account views.** `Email/query` gained a native
  `inAccount` filter, so viewing one account's mail (or the unified all-accounts
  view) no longer depends on the search index being present — it reads straight
  from the message store. Byte-identical mail arriving in two of your accounts
  already collapses to one message that acts on both; this makes the per-account
  slice of that unified view structural rather than index-only.
- **Per-account color.** An account can carry a display color (settable from the
  UI), and its outbound daily send-cap is now visible on the account. Each of a
  user's accounts also syncs concurrently instead of one after another.
- **Internal contacts, auto-derived from your mail.** Correspondents are built
  automatically as mail flows in — display name, every address seen, and the
  alias each person reached you at. Recipient fields in compose now autocomplete
  from these contacts (by name or address, most-corresponded-with first). The
  "reached-me-at" alias is captured so a future reply can default its From to the
  address a correspondent actually wrote to. VIP/block and stored keys remain
  deferred.

### Notes
- Contact re-sightings update internal "seen" counters without emitting a sync
  change (see `docs/adr/ADR-014`), so `/changes` stays proportional to real
  contact changes, not to mail volume.
- IMAP IDLE push, per-account connection pooling, and per-account rate limiting
  are tracked separately as connection hardening and deferred behind this wave.

## [0.9.0] — 2026-07-13

### Added
- **Multi-user.** Any user who can complete the OIDC login for the Mailroom
  application is now auto-provisioned on first login from their token claims, and
  membership in the `mailroom-admin` group grants the admin role (re-evaluated
  every login). Each user's mail syncs, indexes, and purges independently (a
  per-user fan-out that picks up a new login within a minute, with no restart and
  no cross-user interference). Previously a single operator was configured in the
  environment.
- **Add your mail accounts from the UI.** A new add-account wizard and JMAP
  `Account/*` surface let a signed-in user add, edit, and remove their own
  IMAP/SMTP accounts — the server validates the connection, detects the folders,
  and seals the credentials (the password is write-only and never returned).
  Declaring accounts in a file still works as a bootstrap.

### Fixed
- The signed-in user's own email and name now come from their login, not from a
  server environment variable (this fixes the wrong address shown in the UI).

### Added
- **Content-fit auto-height for the message reading pane.** The HTML body now
  sizes exactly to its content — no cramped box, no nested scrollbar. It uses one
  hash-pinned height reporter inside the (still opaque-origin) sandbox; a hostile
  `<script>` still executes nothing and remote content stays blocked, proven in
  headless Chromium.
- **Send-quota meter.** `EmailSubmission/getSendQuota` reports "N of M sends left
  today, resets at midnight UTC" per account, shown in the compose panel. (The
  daily-cap enforcement itself already shipped in 0.6.0.)

### Fixed
- **Mail moved or deleted in another client is reconciled.** A mailbox deleted on
  the server now leaves its messages'' folder membership and emits the right
  `/changes` — fail-closed (a partial/failed folder list destroys nothing; a list
  missing INBOX is refused), and without silently nulling canonical placements.
- **A UIDVALIDITY roll now announces the membership change** it causes (an
  `Email updated` per affected message), instead of silently dropping messages
  from a folder with nothing in the change stream.
- **Drafts are findable by mailbox and account search** (the index missed
  messages that had only a local placement, not IMAP provenance).
- **A "mailbox recreated on the server" writeback failure now says so**, instead
  of mislabelling it "moved or expunged remotely."

## [0.7.2] — 2026-07-13

### Fixed
- Message-list subject and preview overlapped in the wide (full-width list)
  layout — the preview was assigned the subject's grid cell, so the two texts
  rendered on top of each other. The preview now has its own column.

## [0.7.1] — 2026-07-13

### Fixed
- **Message list row was crushing the subject.** With the reading pane open the
  list narrows, and the fixed-width sender column left the subject only a few
  characters. Rows now lay out from the *list's* width (a container query): a
  readable two-line row (sender + time, then the subject) when the list is
  narrow, and the single-line sender/subject/preview/time layout when it's wide.
- **The message body was clipped into a small ~240px box.** The reading pane now
  fills most of the available height. (A sandboxed message frame can't be sized
  exactly to its content without a script inside it, which the sandbox and CSP
  deny — true content-fit height is a tracked follow-up.)

## [0.7.0] — 2026-07-13

### Fixed
- **HTML mail now renders as HTML.** The reading pane was showing the plain-text
  alternative of every `multipart/alternative` message (bracketed link URLs, lost
  layout, no images, no "Show remote content" banner); it now renders the HTML
  body through the existing sandboxed-iframe path and falls back to plain text
  only for text-only mail.

### Added
- **Inline (`cid:`) images render.** Images embedded in a message (signature
  logos, `multipart/related` content) now display. They are resolved to their
  attached bytes and inlined as `data:` URLs — no new content-security-policy
  source is opened, so remote content stays blocked by default and this cannot
  become a tracking vector. Inline images never trip the remote-content gate.
- **Bounce / DSN pipeline.** Delivery Status Notifications (RFC 3464) are
  detected at ingest, parsed leniently, and linked back to the message and
  submission they bounced. A failed or delayed send now surfaces as a real
  per-recipient `deliveryStatus` on the `EmailSubmission` (status code +
  diagnostic) instead of an opaque robot email, and delivery failures are
  recorded for the account-health view.

### Changed
- The repository now keeps this changelog and follows a tag-per-wave release
  process (see `CLAUDE.md` → "Releases & changelog").

### Notes
- The outbound send-quota **enforcement** already shipped in 0.6.0; the
  user-visible send-quota **meter** (a read-only "N of M sends today" surface) is
  deferred to a later tag.

## [0.6.0] — 2026-07-13

First tagged release. The v0.5 "daily driver" milestone is code-complete, and the
first wave of 1.0 features has landed on top of it. Mailroom boots, authenticates
via OIDC, syncs real IMAP accounts, sends mail, searches, imports archives, and
ships a web UI — as a single container with per-user isolation from day one.

### Added — daily-driver baseline (v0.5)
- **JMAP API** — RFC 8620 core semantics + an RFC 8621 mail subset with Mailroom
  extensions: `Email`, `Mailbox`, `Thread`, `Label`, `Identity`,
  `EmailSubmission`, `SearchSnippet`; exact per-type `/changes`; blob
  upload/download; `EventSource` push; a validated `using` set.
- **IMAP sync** — incremental CONDSTORE sync with reconciliation for the cases a
  naive poll misses: messages expunged/moved in another client, UIDVALIDITY
  rolls, and silently-truncated FETCHes. Mail is never dropped or silently lost.
- **Compose / reply / send** — SMTP submission with server-side From validation,
  envelope-derived reply identities, correct threading headers, Sent-copy
  content-hash dedup, drafts, and Undo-Send.
- **Search** — embedded Tantivy with a typed operator query syntax
  (`from:`/`to:`/`subject:`/`label:`/`is:`/`has:`/`account:`, dates, phrases,
  typo tolerance), per-user indexes driven off the change log and rebuildable
  from Postgres.
- **Labels** — canonical labels in Postgres mirrored to IMAP keywords.
- **Web UI** — a Gmail-familiar client: message list, thread view, compose,
  instant-apply actions, multi-select, search, command palette, keyboard
  shortcuts, light/dark.
- **Storage & isolation** — an encrypted, content-addressed blob store; every
  table under forced row-level security; the per-user/per-type `change_log` that
  drives `/changes`, the indexer, and client sync.

### Added — first 1.0 wave
- **OIDC login gate + sign-in page** — Authorization-Code + PKCE, with every
  unauthenticated surface closed and asserted route-by-route in CI; only an
  allowlisted operator subject may log in (single-operator; multi-user
  provisioning is planned).
- **Backup + scripted restore drill** — an encrypted, off-box restore unit
  (Postgres + blobs + key store, master key escrowed separately) and a drill
  that restores into a scratch container, decrypts a message, rebuilds an index,
  and runs a search — and fails loudly on a corrupt/incomplete backup.
- **Bulk archive import** — mbox, maildir, and Google Takeout; streaming
  (bounded memory regardless of archive size), resumable, and idempotent.
  Imported mail is never written back to IMAP.
- **Hostile-content security baseline** — server-side HTML sanitization, message
  bodies rendered in a sandboxed iframe, remote content blocked by default, a
  strict app CSP, and attachments served non-executable.
- **Deletion lifecycle** — Trash and restore, plus a purge job that hard-deletes
  after a retention window with IMAP EXPUNGE propagation, an audit tombstone, and
  safe (reference-counted) blob reclaim.
- **Multi-user hardening** — the API-layer cross-user RLS negative-test suite
  (proving one user can never reach another's data), per-user API rate limiting,
  and storage-quota enforcement on uploads.

### Fixed
- The search index destroyed itself on first boot (cursor file collided with the
  engine's own metadata filename); newly indexed mail was invisible until a
  restart (the reader was never reloaded).
- The production image could not start at all (a required runtime library for the
  CGO search engine was missing from the base image).
- Batched `Label/set` silently discarded work when one label name collided (a
  transaction-abort not isolated by a savepoint).
