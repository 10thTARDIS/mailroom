<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Encryption (PGP)

How to sign and encrypt mail with PGP, and what to do when a correspondent's key changes.

**On this page:** [What PGP gives you](#what-pgp-gives-you) · [Your keys](#your-keys) · [Send encrypted or signed](#send-encrypted-or-signed) · [Read signed and encrypted mail](#read-signed-and-encrypted-mail) · [When a key changes](#when-a-key-changes)

## What PGP gives you

PGP uses a pair of keys. Your **public key** is shared so people can encrypt mail to you and check your signatures. Your **private key** stays on the server and does that work for you.

- **Encryption** means only you and the recipient can read the message body. The subject and the recipients stay readable.
- **A signature** proves the message came from you and was not changed.

Both people need PGP for encryption. Mailroom does not store decrypted text: an encrypted message is decrypted when you open it, so it does not appear in search.

> **Important:** If your server cannot manage keys, Settings → **Keys** says "no key service is connected". Ask your operator.

## Your keys

Go to Settings → **Keys**. Under **Your keys** you can:

- **Make a new key…**: choose the **Address** (a key names one address), when it **Expires**, and tick **Sign my mail from this address with it**.
- **Import your own key…**: paste a private key or choose a file, with its passphrase if it has one.
- **Publish in the key directory**: lets other people's mail apps find your key automatically, once your operator has turned this on.
- **Extend…** an expiry (the fingerprint does not change), **Back up…** to a passphrase-protected file, **Replace…** with a new key, **Retire…** a key you no longer use, or **Revoke…** one that may be stolen.

You are warned before a key expires, with an **Extend it** button.

> **Important:** Back up a key the server made, and keep the file and passphrase safe. If the server is lost, so is the key.

Retiring tells nobody else. Revoking tells everyone who fetches your key to stop using it, and cannot be undone. Both still let you open old mail.

## Send encrypted or signed

**Signing.** If a key is set for your From address, the composer shows "Will sign". If not, it says "Will not sign: no key bound to…".

**Encrypting.**

1. In the composer, switch on **Encrypt**.
2. It says "Encrypted to … and to you". Each recipient chip shows "key held" or "no key".
3. Send as usual.

Mailroom fetches correspondents' keys from their published directories and from their signed mail. If it cannot encrypt, it tells you why:

| Message | What to do |
|---|---|
| "Can't encrypt: no key for …" | Choose **Import their key…** and paste the key they gave you. Compare the fingerprint with them first. |
| "Can't encrypt with a Bcc recipient" | Move them to To or Cc, or send unencrypted. |
| "…key has expired" or "was revoked" | Choose **Check for an updated key**, or ask them for a new one. |
| "the key … has changed since you last wrote to them" | Confirm the new key first: see [When a key changes](#when-a-key-changes). |

Settings → **Keys** → **Correspondents' keys** lists every key held, with where it came from, and lets you **Remove…** one or **Don't use this key…**.

## Read signed and encrypted mail

An encrypted message shows "Encrypted message. Mailroom decrypted it to show it to you". If it cannot, it says "This message is encrypted and could not be decrypted."

A signed message shows one of:

| You see | Meaning |
|---|---|
| "Signed, and the signature checks out against key …" | Good. It came from the key you hold for that sender. |
| "Signed, but the signature doesn't check out." | The message may have been changed after signing. Do not trust it. |
| "Signed with a key you haven't seen from this sender" | It cannot be checked. Import their key to check it. |
| "Signed with a key that has expired" | The key is out of date. If it says "valid when this was signed", the signature was fine at the time. |

## When a key changes

If a signature arrives with a different key from the one you hold for that person, Mailroom shows the old key and the new key side by side, and changes nothing until you choose.

| You see | Why | What to do |
|---|---|---|
| "Signed with a NEW key, different from the one you had for …" | They changed keys, or someone is pretending to be them. | Check with them another way, then **Trust this key from now on** or **Keep the old key**. |
| "Signed with a new key for …" and "Their previous key, the one you had, vouches for it." | The old key signed off the new one, which is a good sign. | **Trust this key from now on**, or **Keep the old key**. |
| "The key you had for … was reported stolen or compromised" | The old key cannot vouch for the new one. | Confirm the fingerprint by phone or in person first. |
| "…You chose to keep the old key, so this one is not trusted." | You rejected it earlier. | **Trust this key after all** if you have since confirmed it. |

Choose **Compare the keys** for advice. In short: ask them to read their fingerprint to you by phone or in person, and do not use the message to check itself.

When you try to send them an encrypted message, you are asked the same question ("… has a new key.") before it can go.

To change a decision later, open Settings → **Keys** → **Correspondents' keys** and use **Use this key again** or **Don't use this key…**.

## Related

- [Writing and sending](writing.md)
- [Reading mail safely](reading.md)
- [Contacts](contacts-and-calendar.md): a contact's page shows the key you hold for them.
