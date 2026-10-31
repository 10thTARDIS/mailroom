# MCP tools for agents

Mailroom serves its mail tools over the **Model Context Protocol** (MCP), so any
MCP-capable client, with any model, can search, read, organise, draft and (if you
allow it) send and delete mail on your behalf. It uses the standard transport and
standard authentication, with no client-specific extensions (ADR-044).

- **Endpoint:** `https://mail.example.com/mcp`, the MCP *Streamable HTTP* transport.
- **Authentication:** an API key in a standard header,
  `Authorization: Bearer mr_…`. Create the key in Mailroom (Settings → API keys,
  or `ApiKey/set`; see ADR-042) and give it only the scopes the agent needs.
- **Stateless:** each request stands alone. The server issues no
  `Mcp-Session-Id` and answers `GET` and `DELETE` with `405`, so it works behind
  any reverse proxy with no session affinity.

The endpoint accepts **API keys only**. A browser session is refused (`403`), and
a request with no credential gets `401` with `WWW-Authenticate: Bearer`.

## Connecting a client

### Clients that take a URL and headers

Most MCP clients that support remote servers accept a URL and a set of HTTP
headers. Give them:

| setting | value |
|---|---|
| URL | `https://mail.example.com/mcp` |
| transport | Streamable HTTP (sometimes called "HTTP" or "streamable-http") |
| header | `Authorization: Bearer mr_your_key_here` |

Keep the key out of shared config files where your client allows an environment
variable or a secrets store instead.

### Clients that only run local (stdio) servers

A client that can only start a local process can reach the endpoint through a
generic stdio-to-HTTP bridge. `mcp-remote`
(an npm package, run with `npx`) is one; these flags are from its README for
version 0.15.2. It takes the URL and one `--header` per header, and expands
`${VAR}` from the `env` block, so the key can live in an environment variable:

```json
{
  "mcpServers": {
    "mailroom": {
      "command": "npx",
      "args": [
        "-y",
        "mcp-remote",
        "https://mail.example.com/mcp",
        "--header",
        "Authorization:${MAILROOM_AUTH}"
      ],
      "env": {
        "MAILROOM_AUTH": "Bearer mr_your_key_here"
      }
    }
  }
}
```

There is no space after `Authorization:` on purpose. Some clients do not escape
spaces inside `args` when they start `npx`, which breaks `"Authorization: Bearer
…"`, so the space lives in the environment variable instead.

Values in `args` are visible in the process list to other users of the machine.
To keep the key out of it, put the header in a file (`Authorization: Bearer
mr_…`, one `Name: value` per line) and pass `--header-file /path/to/headers.txt`
instead of `--header`.

### Plain HTTP on a private network

`mcp-remote` refuses a non-loopback `http://` URL unless it is also given
`--allow-http`. When Mailroom is served over plain HTTP on a private network,
Settings adds `"--allow-http"` to the bridge `args`, right after the URL (the
block above shows the HTTPS form, which needs no flag; `http://localhost` and
other loopback addresses need none either), and shows a warning: the key then
travels unencrypted, so use this only on a trusted private network and serve
Mailroom over HTTPS otherwise.

### Worked examples

These two are **examples, not requirements**: any MCP client works the same way.

**A desktop chat client that runs local servers from a JSON config** (for
example, Claude Desktop's `claude_desktop_config.json`): use the `mcp-remote`
block above as the entry under `mcpServers`, restart the client, and the
Mailroom tools appear in its tool list. Clients whose built-in "connectors"
expect OAuth cannot use a key directly; the bridge is the supported route until
Mailroom implements MCP authorization (planned for v1.96).

**A local LLM tool with native remote-MCP support** (for example, Hermes):
configure a remote server with the URL and the `Authorization` header from the
table above. The exact keys differ from tool to tool; the shape is usually:

```yaml
mcp_servers:
  mailroom:
    url: https://mail.example.com/mcp
    headers:
      Authorization: Bearer mr_your_key_here
```

Check your tool's own documentation for its spelling of `url` and `headers`. If
it only runs stdio servers, use the `mcp-remote` bridge.

## The tools

A tool appears in `tools/list` only when the key's scopes allow every JMAP
method it uses; the rest are absent. A tool that is called anyway is refused by
the server exactly as the JMAP call would be. Tool names are stable: clients and
prompts may rely on them.

| tool | what it does | JMAP methods | scopes needed |
|---|---|---|---|
| `search_mail` | Search, newest first: text, from, to, subject, mailbox, area, unread, flagged, before/after; up to 50 results | Email/query, Email/get | `mail:read` (+ `search:read` for text, from, to, subject) |
| `read_message` | One message: headers, text body (HTML as text, up to 64 KiB, `truncated` when cut), attachment names/types/sizes, never their content | Email/get | `mail:read` |
| `list_mailboxes` | Mailboxes (folders) with role, account and counts | Mailbox/get | `mail:read` |
| `list_labels` | Labels | Label/get | `labels:read` |
| `apply_label` / `remove_label` | Add or remove a label, by id or exact name, on up to 50 messages | Label/get, Email/set | `labels:read`, `mail:move` |
| `mark_read` / `mark_unread` | Set or clear "read" | Email/set | `mail:move` |
| `archive` | Move to the Archive folder of each message's account | Email/get, Mailbox/get, Email/set | `mail:read`, `mail:move` |
| `move_to_mailbox` | Move to a mailbox by id (not Trash: that is `delete_message`) | Mailbox/get, Email/set | `mail:read`, `mail:move` |
| `list_areas` | The areas of your desk, in desk order | Area/get | `mail:read` |
| `move_to_area` | File up to 50 messages into an area, by id or exact name; reports what moved, what was already there, and what another label keeps elsewhere | Area/get, Email/get, Email/set | `mail:read`, `mail:move` |
| `draft_new` | Save a new draft; the body is Markdown (see below) | Email/set | `mail:draft` |
| `draft_reply` | Save a reply draft: Re: subject, threading headers, reply-all optional | Email/get, Identity/get, Email/set | `mail:read`, `mail:draft` |
| `draft_update` | Replace a draft this key wrote with new content (new id; the old version goes to Trash; a reply keeps its threading) | Email/set | `mail:draft` |
| `draft_discard` | Discard a draft this key wrote and never sent (to Trash) | Email/set | `mail:draft` |
| `send_draft` | Send a draft this key wrote, by id | EmailSubmission/set, PendingAction/get | `mail:send` |
| `delete_message` | Soft delete: move to Trash | Email/set, PendingAction/get | `mail:delete` |
| `find_contacts` | Contacts whose name or address matches | Contact/query, Contact/get | `contacts:read` |
| `get_contact` | One contact | Contact/get | `contacts:read` |
| `get_pending` | This key's own approval requests and their state | PendingAction/get | none beyond a valid key |

A key restricted to some of your accounts sees only those accounts' mail and
mailboxes, and does not get the contact tools (contacts are built from every
account's mail).

Areas (the desk's partitions) can be listed and filled, but not created,
renamed or deleted. `move_to_area` files a message the way your own Re-file
does: the message's other area labels come off and the area's own label goes
on. Two things differ for a key:

- **A key's moves do not teach Mailroom's learned sorting.** Learned placement
  learns where a sender's mail goes from *your* filings. A move made with an
  API key is recorded as the key's, so it never counts as yours: not as a
  filing, not as a correction of a learned placement, and not as keeping one.
- **A key cannot undo your own filing.** A message you filed into an area by
  hand is refused (`forbidden`). Re-file it yourself in Mailroom.

A message can stay where it is after the move when one of its other labels
(a topic label you set up to feed an area) routes it to an earlier area. The
tool reports it as `notMoved` rather than counting it as moved.

### Drafts

- **The body is Markdown.** `draft_new`, `draft_reply` and `draft_update` send
  the body twice, as one message: the plain-text part is exactly what the agent
  wrote, and the HTML part is that text rendered (CommonMark with tables,
  strikethrough and links). Single line breaks are kept, so a body with no
  Markdown at all reads as written. Raw HTML is shown as text, never run;
  links go only to `http`, `https` and `mailto` addresses; images are left out
  (shown as their description).
- **A key that cannot send is told so.** Without `mail:send` the draft tools
  say the draft is in Drafts and that you send it from Mailroom; they never
  mention `send_draft`. A hidden tool called anyway answers with the
  permission the key lacks, such as "send_draft is not available: this key
  does not have permission to send email (mail:send)".
- **Repeats are collapsed.** Saving a draft identical to one the same key saved
  in the last 10 minutes (same From, recipients, subject, reply target and
  body) writes nothing: the tool returns the first draft's id with status
  `already_saved`.
- **A key changes only its own drafts.** `draft_update` and `draft_discard` act
  only on drafts that key wrote and never sent; your own drafts and another
  key's are refused, and so is a draft whose send is waiting for your approval.
  Both move the old version to Trash (restorable, like every agent delete). A
  replaced or discarded version cannot be sent while it is in Trash; moved back
  into Drafts, it is the key's draft again.

## Safety model

The MCP endpoint is a thin client of the JMAP API: every tool call is one JMAP
request through the same checks a key's JMAP call meets (ADR-042). There is no
second set of rules to drift from the first.

- **Scopes, deny by default.** A key can do only what its scopes allow; the
  presets are *Read only*, *Organise* (adds moving and labels), *Assistant* (adds
  drafting), and sending and deleting are separate, deliberate choices. A key
  with both needs a typed confirmation.
- **Deletes are always soft.** `delete_message` moves mail to Trash, where it
  stays restorable for 30 days. No tool can permanently delete anything.
- **Approvals.** Sends and deletes start in "ask me first" mode. The tool then
  returns `{"status": "pending_approval", "pendingId": …, "expiresAt": …}` as a
  normal result: nothing has happened yet. You approve or deny in Mailroom;
  unanswered requests are denied after 24 hours. The agent can check with
  `get_pending`.
- **Send only what the key wrote.** `send_draft` needs an explicit draft id and
  only sends drafts this key created and that are still in Drafts, so text the
  agent *read* never becomes a send by itself, and a version it replaced or
  discarded cannot be sent while it is in Trash (moved back into Drafts, it is
  the key's draft again). Attachments cannot be added through the tools.
- **Limits.** Every key has a rate per minute and a 24-hour budget, charged per
  JMAP call (a search is two calls). Past them a tool returns an error
  (`rateLimit`, `overQuota`). More than 50 deletes or 10 sends in 10 minutes are
  refused and pushed to your devices. So is a key that keeps making refused
  attempts that matter for security (reaching outside its accounts, or at
  actions only you can take). By default each of these also **suspends** the
  key: it can make no calls until you reactivate it in Settings, and the alert
  says so. Activity in your off hours is only recorded and pushed.
- **Audit.** Every call is recorded against the key (ids and counts, never
  content), including refusals.
- **Prompt injection.** Mail is written by strangers. Everything taken from a
  message (names, addresses, subjects, previews, bodies, attachment names, and
  contact names learned from mail) is returned only inside fields named
  `untrusted_content`, and every tool description tells the model it is data, not
  instructions. Invisible and direction-changing characters (bidi overrides,
  zero-width characters, control characters) are stripped from all returned
  text. None of this makes an agent immune; scopes, approvals and soft deletes
  are what bound the damage.

Errors from the server come back as tool errors (`isError: true`) carrying the
JMAP error type and description, for example `forbidden`, `notFound`,
`overQuota`, `rateLimit`, so the model can see what happened and adjust.
