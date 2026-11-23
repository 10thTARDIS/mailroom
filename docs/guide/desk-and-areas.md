<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# The desk and areas

How Mailroom arranges your inbox so that what needs you comes first.

**On this page:** [The desk](#the-desk) · [Areas](#areas) · [The line](#move-the-line) · [Drawers](#drawers) · [The in-tray](#the-in-tray) · [Follow-ups](#follow-ups) · [Day-one setup](#day-one-setup)

## The desk

The desk is your inbox, split into two tabs under **Inbox halves**:

- **Review first**: mail that needs a decision from you, grouped by area.
- **Everything else**: mail that can wait, kept in a drawer for each area.

The header counts what is waiting, for example "3 in tray", "12 to review" and "40 in drawers". Use the arrow keys to switch tabs.

Mail that belongs to no area yet appears under **Needs sorting**. Place a message once and Mailroom adds the label and offers a rule. The sidebar's **Needs sorting** entry opens the whole queue.

## Areas

An area is a group of mail, such as **Primary**, **Updates** or **Promotions**. Each area is tied to a label: putting a message in the area means applying that label. The label exists in Mailroom only; other mail apps do not see it.

To see why a message is in its area, open the sender's name for the sender card, which has **Why it is here** and the **Area**. The area can have been set by:

| Shown as | Meaning |
|---|---|
| you placed it | You moved it |
| by rule | One of your rules |
| Learned sorting | [Learned sorting](sorting.md#learned-sorting) |
| by the desk's plan | [Day-one setup](#day-one-setup) |
| filed by an AI tool (API key) | An [assistant](ai-assistants.md) |
| automatically | The built-in sorting of mail by what it does |

Manage areas in Settings → **Areas and drawers**: **Add area**, **Rename**, reorder, choose which labels feed an area, or **Delete area…** (the label stays on its mail, and the mail moves to **Needs sorting**). **Hide from the rail** removes an area from the sidebar but not from the desk.

## Move the line

The line is a score. Mail at or above it is on the surface waiting for you; mail below it goes to the area's drawer. The score is learned from what you do, and **Why is this ranked here?** explains each message.

1. On the desk, find **The line**.
2. Choose **stricter** (fewer messages to review) or **looser**.
3. Read the preview, for example "at 0.65: 12 to review, 40 in drawers".
4. Choose **Apply**.

Nothing moves until you apply. You can also set it in Settings → **The line**, which shows what the last 7 days would have looked like.

To correct a single message, press `!` to mark it important or not important, or use `Shift+↑` and `Shift+↓` to move it above or below the line.

## Drawers

A drawer holds an area's mail that sits below the line. Each shows how long it waits ("auto-archives after 7 days") or "never auto-archives".

- **Open** shows the drawer; **Archive all** archives everything in it after a confirmation.
- A drawer only archives. It never deletes and never marks mail read, and archived mail stays in **All mail** and in search.
- Set how long each area waits in Settings → **Areas and drawers** (**Auto-archives after**, or the **Default drawer period**).

If you see "Sweeping is switched off for this login", drawers will not archive by themselves until you pick a drawer period in day-one setup.

## The in-tray

The in-tray is for mail you must come back to. Press `s` on a message to put it there or take it out; the in-tray uses the same flag as **Star**. Open **In-tray** in the sidebar to see everything in it, grouped as **Today**, **This week**, **Over a week** and **Over a month**.

Set when the strip above the desk starts nagging in Settings → **In-tray** (**Nag after**). It never moves or deletes anything.

## Follow-ups

Under **Follow up** on the desk, Mailroom reminds you of conversations that went quiet. Each shows **Done** or **Snooze…**. Turn them on in Settings → **In-tray**:

- **Remind me when no one has answered my mail for a week**
- **Remind me about mail I haven't replied to** (off until you turn it on), with **Remind me after** a number of days

A reminder stops by itself a month after the message.

## Day-one setup

The first time you sign in with mail, a four-step wizard turns your folders into a desk. Every step shows a preview and writes nothing until you choose to.

1. **What is in your mailbox**: your folders, and how new mail should arrive.
2. **The mail already in those folders**: how far back counts as new, and what to do with older mail. You can undo this step.
3. **Rules, learned from your folders**: proposed rules, each showing what it would have done.
4. **The line and the drawers, to start**.

Choose **Skip setup, open the desk** at any time. To run it again or import one folder later, go to Settings → **Areas and drawers** → **Day-one setup**.

## Related

- [Sorting your mail](sorting.md)
- [Settings map](settings.md)
- [Keyboard shortcuts](keyboard-shortcuts.md)
