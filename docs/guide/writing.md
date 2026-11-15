<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Writing and sending

How to compose, reply, send later and undo a send.

**On this page:** [Compose](#compose-a-message) · [Modes](#rich-plain-and-markdown) · [From address](#choose-the-from-address) · [Reply and forward](#reply-reply-all-and-forward) · [Drafts](#drafts) · [Sending options](#sending-options) · [Signatures and templates](#signatures-and-templates) · [Contacts](#recipients) · [Images and attachments](#images-and-attachments)

## Compose a message

Choose **Compose** (key `c`). Fill in **To**, **Subject** and **Message**. **Add Cc** and **Add Bcc** reveal those fields. Send with **Send** or **Ctrl/⌘+Enter**.

Spell checking uses your browser's own.

## Rich, plain and Markdown

Above the message box, the button **Plain text** (or **Rich text**) switches modes.

- **Rich text** has a toolbar: bold, italic, lists, links, tables, fonts and colours.
- **Plain text** sends exactly what you type.
- In plain text mode, tick **Markdown** to write with simple markup (`**bold**`, `# heading`, lists, links, quotes, code). The recipient gets a formatted version and a plain-text copy. Only web and `mailto:` links are made clickable.

## Choose the From address

The **From** line picks the address you send as. Mailroom suggests one: the address the person wrote to when you reply, otherwise your primary address. Choose **Change** to pick another, including a catch-all address at your domain (you can send as any address there, if your provider allows it).

Settings → **Identities and signatures** lists your addresses in the order Mailroom prefers them, with each marked primary, catch-all or disabled. A disabled address cannot send.

## Reply, reply all and forward

| Action | Button | Key |
|---|---|---|
| Reply | **Reply** | `r` |
| Reply to everyone | **Reply all** | `a` |
| Forward | **Forward** | `f` |

The reply is sent from the address the original was sent to, so replies keep coming back to the same place.

## Drafts

Drafts save by themselves; the footer shows "Saving…" then "Saved a moment ago". Find them in **Drafts**. **Discard** deletes one. If you are offline, "this draft is saved and will sync when you reconnect".

## Sending options

- **Undo Send.** After you send, a message offers **Undo send** for a few seconds (10 by default; your operator can change it). Until it expires the message waits in the **Outbox**, where you can also choose **Send now** or **Cancel**.
- **Send later…** schedules a message: pick a time such as **Tomorrow**, or **Pick a date and time**. It can be at most 30 days ahead. It waits in the Outbox as "Scheduled".
- **Send & archive** appears when you reply or forward. It sends, then archives the conversation once the message has gone (`Ctrl/⌘+Shift+Enter`).
- If a message cannot be delivered the Outbox says **Not sent — the mail server refused it** and shows the reason. The draft is still in **Drafts**; fix it and send again.

Offline, a message is saved and sent when you reconnect.

## Signatures and templates

Create them in Settings → **Identities and signatures**.

- A **signature** is added below your text. Mark one **Use this one automatically** to have it added to every message, and bind it to an address so it is used only when you send from that address.
- A **template** is a whole message to start from. **Insert template** appears only before you start writing.
- Use **Insert signature** in the composer to add a different signature.

## Recipients

As you type in **To**, Mailroom suggests people from your contacts and the people you write to. A chip shows whether you hold an encryption key for them ("key held" or "no key"). Choosing a chip lets you open the contact or add them to your contacts.

## Images and attachments

- **Attach files**, or drag files onto the message ("Drop files here to attach").
- In rich text, **Insert image** puts a picture in the text. Pictures copied from the web are left out ("attach them instead").
- A file Mailroom withheld cannot be forwarded until you release it.

## Related

- [Encryption (PGP)](encryption.md): the **Encrypt** switch in the composer.
- [Contacts and calendar](contacts-and-calendar.md)
- [Keyboard shortcuts](keyboard-shortcuts.md)
