<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Getting started

How to sign in, find your way around, add your mail accounts and install Mailroom as an app.

**On this page:** [Sign in](#sign-in) · [The screen](#the-screen) · [Add a mail account](#add-a-mail-account) · [Install as an app](#install-as-an-app) · [Notifications](#turn-on-notifications) · [Themes](#choose-a-theme)

> Whoever runs your Mailroom server is called the **operator** in this guide. It may be you.

## Sign in

1. Open your Mailroom address in a browser.
2. Press **Sign in**. Mailroom sends you to your identity provider (the service that holds your login), then brings you straight back.
3. To leave, use Settings → **Sign out**.

If you see "That account cannot use this Mailroom", ask your operator to enable it.

## The screen

| Region | What it is |
|---|---|
| Sidebar (on a phone, the bottom bar) | Takes you to **Inbox**, **Drafts**, **Sent** and **All mail**, then your **Smart folders** (saved searches), **Labels** and **Folders** (including **Archive**, **Junk** and **Trash**). It also has **In-tray**, **Outbox**, **Contacts** and **Settings**. |
| Account switcher | Chooses **All accounts** or one account. The key `g` then `g` opens it. |
| Search box | Searches all your mail. Press `/` to jump to it. |
| List or desk | Your messages. The default view is the desk: see [The desk and areas](desk-and-areas.md). |
| Reading pane | Shows the open message, beside the list or below it. |

**Starred**, **Snoozed**, **Priority** and **Unread** are not in the sidebar. Open them with the command palette (**Ctrl/⌘+K**). **Starred**, **Snoozed** and **Priority** also open with `g` then `s`, `b` or `p`. Press `?` anywhere for the [shortcut list](keyboard-shortcuts.md).

## Add a mail account

You add your own accounts. Mailroom connects to the mail you already have and keeps a copy; your mail stays with your provider.

1. Go to Settings → **Accounts**.
2. Choose **Add mail account…**.
3. If you use Gmail or Outlook and your operator has set them up, choose **Connect Gmail** or **Connect Outlook**. You sign in at the provider and Mailroom never sees your password.
4. Otherwise enter an **Account name**, **Username** and **Password**. Open **Server settings** to enter the **IMAP server** and **SMTP server** if they are not filled in. Use an app password if your provider offers one.
5. Choose **Add account**. Your mail appears as it downloads.

To pause an account, change its password, or remove it, open it from Settings → **Accounts**.

> **Important:** Removing an account deletes mail that exists only in it. Mail also held by your provider is not affected.

## Install as an app

Mailroom installs from your browser; there is nothing to download from an app store.

- **Phone (Android):** open the browser menu and choose **Install app** or **Add to Home screen**.
- **iPhone or iPad:** in Safari, choose Share, then **Add to Home Screen**.
- **Computer:** in Chrome or Edge, use the install icon at the right of the address bar.

## Turn on notifications

Notifications are set per device.

1. On the device, go to Settings → **Notifications**.
2. Choose **Enable notifications** and allow them in the browser prompt.
3. Under **This device notifies you about**, choose **Every new message**, **Mail above the line**, **Mail from VIPs** or **Mail above the line or from VIPs**.
4. Optionally turn on **Quiet hours**. During them only VIP mail notifies you.

On iPhone, notifications work only once Mailroom is installed to the Home Screen. If you see "This server doesn't offer push notifications", your operator has not enabled them.

## Choose a theme

Go to Settings → **Appearance**. **Theme** offers **Follow system**, **Light** and **Dark**. The same page sets **Density**, **Text size**, **Rows** and the **Reading pane** layout.

## Related

- [The desk and areas](desk-and-areas.md)
- [Settings map](settings.md)
- [Frequently asked questions](faq.md)
