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

Once you have moved a sender's mail to an area, Mailroom files that sender's next mail there too. There is nothing to set up. It acts on:

- **One move.** After you move a sender's mail to an area **once**, and never anywhere else, new mail from that exact address goes there. If you then correct it (move a learned message elsewhere), that sender stays unsorted until you have moved **two more** of its messages to the same place.
- **A recent run.** A sender whose older mail you filed in more than one area is sorted by your **latest** habit: when your three most recent moves of that sender all went to the same area, with none elsewhere in between, new mail goes there. Two moves are not enough.
- **A whole company, carefully.** Mail from an address you have never moved follows its domain only when you have filed at least two different addresses at that domain the same way. This is never done for shared providers such as Gmail or Outlook, or for your own domains.

Learned mail is marked **Learned sorting** on the message and its sender card, so you can always tell it from a rule or from your own move.

It is deliberately careful:

- **Your rules always win.**
- **Only genuine senders.** A message is sorted this way only if its sender is confirmed as genuine. Mail that cannot be authenticated, or that carries a [sender warning](reading.md#sender-warnings), is never sorted by learning, unless you trust the sender (below), and even then a sender warning still stops it.
- **Not for shared providers.** Learning never spreads from one Gmail or Outlook address to every address at that provider.
- **It backs off when you correct it.** If you move a learned message elsewhere, the sender is no longer sorted automatically, and an area where it was corrected often is paused.

### See and change what it learned

Open a sender's card to see what learned sorting has learned about that sender: which area it files them in, how many of your moves it is based on, and whether it will act on their next message. From there you can:

- **Forget this sender.** Learned sorting ignores everything you did with that sender before now (it deletes nothing, and your messages stay where they are). Moves you make afterwards count again. You can forget just the address, or the whole domain.
- **Trust this sender.** Some senders can never be confirmed as genuine, for example automatic mail from a server of your own. For one exact address you can tell learned sorting to sort it without that confirmation. It does not turn off sender warnings, and it does not apply to a message whose check clearly failed. A whole domain cannot be trusted.
  - **Trust only senders you know**, such as your own servers, whose mail no one else could plausibly fake. A sender whose domain publishes no mail policy can be faked in a way that looks identical to the real one, and trust is exactly what lets such mail through.
  - **It does not help for your own addresses' domains.** Mail claiming to be from the domain of one of your own addresses is always checked, so trust can't help there; the sender card hides the switch for those. Trust is for automatic senders at a different domain.

Forget, trust and stop-trusting each leave a line in **Activity**.

To undo or correct a learned message, simply move it to the right area. To turn learned sorting off, go to Settings → **Areas and drawers** and switch off **Sort new mail into areas by learning from your moves**. That section also shows how many messages learned sorting filed in the last 30 days and how many you moved again, and lists **Senders you keep moving by hand** with a **Make a rule** button for each.

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
