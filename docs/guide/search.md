<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Search

How to find mail across all your accounts, using plain words or operators.

**On this page:** [Search basics](#search-basics) · [Operators](#operators) · [OR and brackets](#or-and-brackets) · [Search options](#search-options) · [Saved searches](#saved-searches)

## Search basics

Press `/`, or click the search box, and type. Results update after a short pause. Every word you type must match, and the last word is treated as the start of a word, so `invo` finds "invoice".

- Put a phrase in quotes: `"quarterly revenue"`.
- End a word with `*` to match any ending: `overd*`.
- Small typos in subjects and people's names are forgiven. Message bodies are matched exactly.
- Trash and Junk are left out unless you add `in:anywhere`.

Use **Sort search results** to choose **Newest first** or **Best match**.

## Operators

Type an operator, a colon and a value, with no space after the colon.

| Operator | Finds | Example |
|---|---|---|
| `from:` | Mail from an address, or from anyone at a domain | `from:alice@example.com`, `from:shop.example` |
| `to:` | Mail sent to an address (To or Cc) | `to:bob@example.com` |
| `cc:` | The same as `to:` | `cc:bob@example.com` |
| `alias:` | Mail delivered to one of your addresses | `alias:me@example.com` |
| `subject:` | Words in the subject only | `subject:invoice` |
| `body:` | Words in the message text only | `body:refund` |
| `label:` | Mail with a label, by name | `label:Work`, `label:"Client Work"` |
| `area:` | Mail in an area, by name | `area:Updates` |
| `list:` | Mail from a mailing list | `list:lists.example.org` |
| `account:` | Mail in one account, by name | `account:Personal` |
| `is:` | A state: `unread`, `read`, `starred`, `answered`, `draft`, `junk`, `trashed`, `snoozed`, `muted`, `pinned`, `important`, `vip` | `is:unread` |
| `has:` | Has `attachment`, `pdf`, `image`, `document`, `spreadsheet`, `presentation` or `unsubscribe` | `has:pdf` |
| `in:` | Where: `trash`, `junk`, or `anywhere` to include both | `in:anywhere` |
| `filename:` | An attachment's name or extension | `filename:report.pdf` |
| `larger:` / `smaller:` | Size, with `K`, `M` or `G` | `larger:5M` |
| `after:` / `before:` | Received on or after, or before, a day | `after:2026-01-01` |
| `on:` | Received on one day | `on:2026-07-12` |
| `date:` | Received in a month or year | `date:2026-07` |
| `newer_than:` / `older_than:` | Relative age, in `d`ays, `w`eeks, `m`onths or `y`ears | `newer_than:7d` |

Put `-` in front of any operator to exclude it: `-is:read`, `-label:Noise`.

An unknown value, such as `is:bananas`, finds nothing rather than being ignored. A colon that is not an operator (a web address, say) is searched as ordinary text.

## OR and brackets

Words are combined with "and" by default. Write `OR` in capitals for either.

| Search | Means |
|---|---|
| `from:alice OR from:bob` | Mail from either |
| `invoice (from:alice OR from:bob)` | Invoices from either |
| `invoice -(from:alice from:bob)` | Invoices, except those matching both |

`a b OR c` means "`a` and `b`, or `c`". Use brackets to be sure.

## Search options

Open **Search options** beside the box for a form: **From**, **To**, **Subject**, **Has the words**, **Doesn't have**, **Date within**, **Larger than**, **Smaller than**, **Area**, **Has attachment** and **Include Trash and Junk**. It writes the operators for you.

## Saved searches

1. Run a search.
2. Choose the star button (**Save this search**), type a name under **Save as**, and tick **Show in the sidebar**.

A saved search appears under **Smart folders** and behaves like a folder that fills itself. Change the name or query, or delete one, in Settings → **Saved searches** (**Edit**, **Delete**).

## Related

- [Sorting your mail](sorting.md)
- [Keyboard shortcuts](keyboard-shortcuts.md)
- [Settings map](settings.md)
