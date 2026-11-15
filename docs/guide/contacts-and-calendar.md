<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Contacts, calendar and out of office

How to manage the people you write to, answer meeting invitations, and set an automatic reply.

**On this page:** [Contacts](#contacts) · [VIPs and blocking](#vips-and-blocking) · [Merge contacts](#merge-two-addresses-into-one-contact) · [Meeting invitations](#meeting-invitations) · [Out of office](#out-of-office)

## Contacts

Mailroom builds your contacts from your mail: people you send to appear automatically. Open **Contacts** in the sidebar to search them, filter by **All saved**, **VIP**, **Has a key** or **Blocked senders**, and sort by **Name** or **Last mail (newest first)**.

To add someone you have mail from but have not written to, choose **Add a contact by address…**. You can add only addresses that already appear in your mail.

Open a contact to see their addresses, how often you have written, recent mail (**See all mail with …** runs a search), and the key you hold for them.

- **Edit name** sets *your* name for them. **Use theirs** goes back to the name their mail uses.
- **Notes** holds up to 4,000 characters, for you only.
- **Remove from contacts…** hides them from the list. Nothing is deleted: mail, keys, notes and any block stay.

## VIPs and blocking

- **VIP** marks someone important. Their mail can notify you during quiet hours, and `is:vip` finds it. Toggle it on their contact page, from the sender card, or in Settings → **Senders**.
- **Block…** sends their new mail to Junk or Trash (you choose, under **New mail from them goes to**). Mail already here stays where it is. A block matches the exact address. **Unblock** reverses it.

## Merge two addresses into one contact

If someone writes from two addresses:

1. Open one contact and choose **Merge with…**.
2. Search for the other and review **What will be combined**.
3. Choose **Merge**.

Keys stay tied to each address. **Separate…** splits them apart again.

## Meeting invitations

An email with a calendar invitation shows a card above the message with **When**, **Where**, **Organiser** and **Guests**. Choose **Accept**, **Maybe** or **Decline** and Mailroom sends your answer to the organiser from the address it was sent to. As with any message, Undo Send applies, so the answer waits briefly in the Outbox.

- If the event was cancelled or updated, the card says so.
- If the invitation was not sent to an address you can send from, it says it cannot be answered here.
- An update that cannot be verified as from the organiser changes nothing else.

Mailroom does not have a calendar view or sync with calendar apps. Add events to your calendar yourself.

## Out of office

An automatic reply for when you are away.

1. Go to Settings → **Out of office**.
2. Switch on **Send an automatic reply** for the account.
3. Optionally set **First day away** and **Last day away**. With no first day it starts when you save; with no last day it runs until you turn it off.
4. Write the **Subject** and **Message**.
5. Choose how often one sender gets a reply (**Reply to the same sender at most once every this many days**, 1 to 30).
6. To reply only to people you know, tick **Only reply to people I have written to or marked as VIP**.
7. Choose **Save the reply for** your account.

Not every account supports this; an account that cannot says "Not available for this account". Your mail provider sends the replies, not Mailroom, so they keep going while Mailroom is down. Spam and mail sent to a catch-all address get no reply. If another filter on your mail server takes over, Settings says "Another filter script is now active on your mail server, so the reply is not being sent."

## Related

- [Search](search.md)
- [Encryption (PGP)](encryption.md)
- [Settings map](settings.md)
