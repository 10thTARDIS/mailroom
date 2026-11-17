<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Frequently asked questions

Short answers to the questions new users ask most.

**Questions:** [Where is my mail?](#where-is-my-mail-stored) · [Can I use my phone's mail app?](#can-i-still-use-my-phones-mail-app) · [If Mailroom is down?](#what-happens-if-mailroom-is-down) · [Images blocked](#why-are-images-blocked) · [Attachment withheld](#why-was-an-attachment-withheld) · [Why this area?](#why-did-a-message-land-in-this-area) · [Undo](#how-do-i-undo-something) · [Learned sorting](#how-do-i-stop-mailroom-learning-where-i-file-things) · [Operator](#can-the-person-who-runs-mailroom-read-my-mail) · [Missing mail](#why-is-mail-missing-or-late) · [Says offline](#why-does-mailroom-say-offline-when-my-connection-is-fine) · [Notifications](#why-am-i-not-getting-notifications) · [Delete my data](#how-do-i-export-or-delete-my-data)

## Where is my mail stored?

With your mail provider, as always. Mailroom keeps a synchronised copy on the server it runs on, stored encrypted, and writes your changes back to the provider. It is never a mail server itself.

## Can I still use my phone's mail app?

Yes. Your provider still holds your mail, so any mail app works alongside Mailroom. Read, unread, star and archive changes sync both ways. Some things stay inside Mailroom: areas, and labels marked "stays in Mailroom". Muting is also Mailroom-only.

## What happens if Mailroom is down?

Your mail is safe at your provider and your other mail apps keep working. An out-of-office reply is sent by your provider, so it keeps going too. In Mailroom, changes you make while it cannot reach the server are queued ("Offline — changes are queued") and sent when it is back.

## Why are images blocked?

Pictures in email can tell the sender you opened it. Choose **Show remote content** for one message, or **Always allow this sender**. See [Reading mail safely](reading.md#remote-images).

## Why was an attachment withheld?

The virus scanner flagged it, or could not check it (often because it is password-protected). Withheld files cannot be opened or forwarded unless you **Release** them. See [Attachments](reading.md#attachments).

## Why did a message land in this area?

Every placement explains itself. Open the sender's card and look at **Why it is here** and **Area**: it says whether you placed it, a rule did, or Mailroom learned it from your moves. See [The desk and areas](desk-and-areas.md#areas).

## How do I undo something?

Most actions show a message with **Undo** for a few seconds; `Ctrl/⌘+Z` does the same. Sends have **Undo send**. A bulk rule application has **Undo**, and so does a day-one setup step.

## How do I stop Mailroom learning where I file things?

Move a message back to the right area to correct it. To switch it off, untick **Sort new mail into areas by learning from your moves** in Settings → **Areas and drawers**. See [Learned sorting](sorting.md#learned-sorting).

## Does it send my mail to an AI?

No, unless you connect an assistant yourself. See [AI assistants](ai-assistants.md).

## Can the person who runs Mailroom read my mail?

Technically, yes: whoever runs the server holds the stored copy and the keys that protect it. Mailroom limits and records what its software does, but this is a matter of trust. Connect an account only if you trust them.

## Why is mail missing or late?

Open **Account health** from the account switcher. It shows whether each account is syncing, and any stuck changes. A paused account stops receiving new mail until you choose **Resume sync**. If an account says it **needs reconnecting**, choose **Reconnect**: nothing is lost in the meantime.

## Why does Mailroom say "Offline" when my connection is fine?

It could not reach the server. If **Retry** does not help, reload the page. If you have to sign
in again, your sign-in had expired; Mailroom takes you there by itself when it can tell.

## Why am I not getting notifications?

Notifications are per device: turn them on in Settings → **Notifications** on each one. Check the choice under **This device notifies you about**, any **Quiet hours**, and that the account is ticked under **Accounts that notify**. On iPhone, install Mailroom to the Home Screen first.

## How do I export or delete my data?

Your operator does this for you. See Settings → **Data**.

## Related

- [Getting started](getting-started.md)
- [Settings map](settings.md)
- [Guide home](README.md)
