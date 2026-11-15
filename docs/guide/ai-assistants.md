<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# AI assistants

How to let an AI tool help with your mail while you stay in control. Nothing leaves your server unless you connect a tool.

**On this page:** [What an assistant can do](#what-an-assistant-can-do) · [Create a key](#create-a-key-for-a-tool) · [Approve before it sends or deletes](#approve-before-it-sends-or-deletes) · [Connected apps](#connected-apps) · [Activity](#see-what-happened) · [Revoke access](#suspend-or-revoke-access)

## What an assistant can do

An assistant connects to Mailroom through a key or an approved app, and can only do what you allowed. Depending on the permissions you give, it can:

- search and read mail, labels and contacts;
- move, mark and label mail;
- write drafts;
- send mail and delete mail (to Trash only), if you allow it.

Whatever it files does not teach Mailroom's own sorting; your habits stay yours.

## Create a key for a tool

1. Go to Settings → **API keys and AI tools**.
2. Choose **Create a key…**.
3. Give it a **Name** that says which tool uses it.
4. Under **What it can do**, pick a preset:

| Preset | Allows |
|---|---|
| **Read only** | Search and read mail, labels and contacts |
| **Organise** | Read only, plus moving, marking and labelling mail |
| **Assistant** | Organise, plus writing drafts |
| **Custom** | Choose each permission, including **Send mail** and **Delete mail (to Trash)** |

5. Optionally limit it to **Only these accounts**, and set **Calls per minute** and **Calls per 24 hours**.
6. Leave **Suspend the key automatically if it misbehaves** ticked (it is on by default) as a safety net.
7. Choose **Create key**, then **Copy key**. It is shown only once; choose **Done, I've saved the key** afterwards.

Give the key to your tool, which also needs your Mailroom's address. The same page has a **Connect an AI tool** guide with the address and the exact settings to copy. Technical setup is in the [MCP reference](../mcp.md).

> **Important:** Treat a key like a password. A key that can both send and delete needs you to type a phrase to confirm, because it could mail anyone as you and then remove the trace.

## Approve before it sends or deletes

**Ask me first** is ticked for sending and deleting by default; keep it that way. The tool's request then waits for you:

- A banner appears: "1 action waiting for your approval", with **Review**, and you also get a notification.
- The request lists what would happen: recipients, subject and attachments for a send, or the messages for a delete.
- Choose **Approve…** or **Deny**.
- An unanswered request is denied automatically after 24 hours.

Deleted messages go to Trash and can be restored for 30 days.

## Connected apps

Some tools sign in through your browser instead of using a key. When one asks, you choose what it may do and approve it. It then appears under **Connected apps** in the same Settings page. You can end it at any time, and there is no key to copy.

## See what happened

Settings → **Activity** lists what happened to your mail and access, newest first: what **AI tools** did or were refused, **Decryptions**, permanent **Deletes**, **Sign-ins** and **Bulk changes**. Tick the kinds you want under **Show**, and set **From** and **To** dates. A refused request gives the reason, such as "it needs your approval first" or "it has used its daily allowance". A key's own page has its activity, too.

## Suspend or revoke access

Open the key (**Details**) or the app, then:

- **Suspend…** stops all its calls until you choose **Reactivate…**. Nothing else changes.
- **Revoke…** ends it at once and for good. Its history stays in Activity. To use the tool again, create a new key.

## Related

- [MCP reference (technical setup)](../mcp.md)
- [Settings map](settings.md)
- [Frequently asked questions](faq.md)
