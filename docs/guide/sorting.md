<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Sorting your mail

How to file mail in areas, let Mailroom learn from you, write rules, and set mail aside until later.

**On this page:** [Move to area](#move-mail-to-an-area) · [Learned sorting](#learned-sorting) · [Rules](#rules) · [Suggested rules](#suggested-rules) · [Snooze, pin, mute](#snooze-pin-and-mute) · [Labels and folders](#labels-and-folders) · [Clean up](#clean-up-old-senders)

## Move mail to an area

1. Select one or more messages.
2. Choose **Move to area** and pick an area. On the desk the button is **Re-file**; the keys `1`–`9` file into the first nine areas.
3. A message appears: "Moved 1 message to Updates." Choose **Undo** to take it back (also `Ctrl/⌘+Z`).

To move everything from one sender, choose **Move all from … to…** on a message or the sender card. Mailroom counts the messages and asks first ("Move all 14 messages from … to Updates?").

## Learned sorting

Once you have moved a sender's mail to the same area at least twice, and never anywhere else, Mailroom files that sender's new mail there too. There is nothing to set up.

It is deliberately careful:

- **Your rules always win.**
- **Only genuine senders.** A message is sorted this way only if its sender is confirmed as genuine. Mail that cannot be authenticated, or that carries a [sender warning](reading.md#sender-warnings), is never sorted by learning.
- **Not for shared providers.** Learning never spreads from one Gmail or Outlook address to every address at that provider.
- **It backs off when you correct it.** If you move a learned message elsewhere, the sender is no longer sorted automatically, and an area where it was corrected often is paused.

To undo or correct it, simply move the message to the right area. To turn it off, go to Settings → **Areas and drawers** and switch off **Sort new mail into areas by learning from your moves**. That section also shows how many messages learned sorting filed in the last 30 days and how many you moved again, and lists **Senders you keep moving by hand** with a **Make a rule** button for each.

## Rules

A rule files, labels or flags mail as it arrives.

1. Open a message, then choose **More** → **Create a rule from this message…**, and pick the basis: **From this sender**, **From anyone at** the domain, **With this subject** or **From this mailing list**. Or go to Settings → **Rules** → **New rule**.
2. Under **Only for messages that are**, optionally limit the rule to one kind of message: **Personal mail**, **Receipts and account mail**, **Promotions**, **Notifications**, **Mailing lists and newsletters**, **Other automatic mail**, or leave **Any kind of message**.
3. Under **When a message matches**, add conditions (for example **Sender** **is** an address, or **Subject** **contains** a word) joined by **all of** or **any of**.
4. Under **Do this**, add actions: **Apply label**, **Mark important**, **Star it**, **Skip the inbox**, **Archive it**, or **Stop processing further rules**.
5. Choose **Check what this matches** to see how many of your messages the rule would catch, with examples.
6. Choose **Save rule**.

Rules run in order, lowest **Order** number first. To apply a saved rule to mail you already have, choose **Apply to existing mail**, review the count, then **Undo** is available afterwards. Rules limited to one kind of message apply to new mail only.

If you take a rule's label off messages, the rule shows "misfiled" with **Narrow it** to propose a tighter version.

## Suggested rules

After you move the same sender's mail by hand, Mailroom offers "Always put mail from … in …?" with **Make a rule** and **Not now**. Once made, it can also move the earlier messages from that sender. In Settings → **Rules**, a count of suggested rules is shown.

## Snooze, pin and mute

| Action | What it does | Key |
|---|---|---|
| **Snooze** | Hides a message until a time you choose (**Later today**, **Tomorrow**, **Next week**, or **Pick a date and time**), then brings it back. Open **Snoozed** to see them; **Bring it back now** returns one early. | `b` |
| **Pin** | Marks a message or conversation as pinned so it stands out; `is:pinned` finds them. | `p` |
| **Mute conversation** | New replies go straight to Archive, unread, with no notification. This happens in Mailroom only; other apps still show them in the inbox. | `m` |

## Labels and folders

A **label** describes what mail is about and a message can have several. Press `l` to apply one, or `l` then `1`–`9` for your first nine. Create one with **New label…** in the sidebar. A label marked "stays in Mailroom" is not visible to other mail apps.

**Folders** are your provider's own folders. Drag a message onto a folder or label in the sidebar to file it.

**Needs a category** is a separate queue where Mailroom asks about mail it could not categorise; **Label all** files a group at once.

## Clean up old senders

Settings → **Clean up** ranks senders, domains or mailing lists by **Most mail** or **Most ignored**, with **Unsubscribe…**, **Archive all**, **Move all to…** and **Block…** for each.

## Related

- [The desk and areas](desk-and-areas.md)
- [Search](search.md)
- [Contacts, VIPs and blocking](contacts-and-calendar.md)
