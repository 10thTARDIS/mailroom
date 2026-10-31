# ADR-045: An API key's own drafts — clear no-send wording, repeats collapsed, replace and discard, Markdown bodies

**Status:** Accepted (v1.95.0; all four parts chosen
after testing v1.95.0-rc.2). Amends ADR-042 §3 (what a key's Email/set needs)
and ADR-044 §4 (the tool list). Spec §13a's "no agent path to immediate hard
deletion, ever" is kept, not deviated from.

## Context

v1.95.0-rc.2 was tested with a small local model behind a chat
front end. The key had `mail:draft` and not `mail:send`. Asked to send a
message, the model saved the same draft over and over and never got further.
The cause was ours: every draft tool's description
told every key that sending is `send_draft` — a tool hidden from a key without
`mail:send`. The model was told to call a tool it did not have, and did the
only thing it could. Three further gaps showed: a key could not correct or
withdraw a draft it wrote (each attempt was another draft), and the model's
Markdown arrived at the recipient as literal asterisks.

## Decision

### 1. Say plainly when a key cannot send

- The draft tools' descriptions (`draft_new`, `draft_reply`, `draft_update`)
  and every draft result are worded for the key's **send mode**, read by the
  bridge from the key's policy (`mcpBridge.SendMode`): without `mail:send`
  they say "This key cannot send email. The draft is in Drafts; the account
  owner sends it from Mailroom." and never name `send_draft`; with it they
  point at `send_draft`, and say the send needs approval when `mail:send` is in
  the key's confirm scopes. The registered description is the no-send wording,
  so a description that somehow escapes the per-key rewording promises nothing.
  The rewording copies the listed tool (the SDK's registered `*Tool` is shared
  across requests).
- `get_pending`'s schema no longer names `send_draft` either: a no-send key's
  whole `tools/list` is free of it.
- **A hidden tool called anyway** still runs its JMAP call through the
  dispatcher, which refuses it **and audits the refusal** (cost 1 against the
  rate, as ADR-042 §5 wants for a key looping on refused calls). Its tool error
  is then reworded in `register` to name what the key lacks, from the same
  policy table (`mcpBridge.KeyLacks`): "send_draft is not available: this key
  does not have permission to send email (mail:send)"; the server's own error
  is kept as detail. A tool name that does not exist at all keeps the SDK's
  JSON-RPC "unknown tool" error, which is what the MCP specification asks for.
  Intercepting in the SDK's receiving middleware instead would have skipped
  the audit and the rate charge.

### 2. A key's repeat draft is collapsed into the first

- A key's Email/set create that matches a draft **the same key** saved in the
  last 10 minutes (`KeyDraftRepeatWindow`), which still exists, is still
  `$draft`, is not in Trash, has no submission and is still in a Drafts-role
  mailbox, writes nothing and answers with that draft's object plus
  `alreadySaved: true`; the tool returns `status: "already_saved"` and says
  nothing new was written. It applies to every key create (raw JMAP too),
  except in a call that also destroys or discards — the earlier draft might be
  the one going away — and except a replacement (§3).
- **Matching is on a stored, KEYED fingerprint** (`app.api_key_drafts.
  content_fingerprint`, migration 00077, expand-only): HMAC-SHA256, under a key
  derived from the user's mail-blob DEK (`compose.DraftFingerprint`), of the
  create normalised (`api.draftCanonical`): From as given, To/Cc/Bcc lowercased
  and sorted, subject trimmed, In-Reply-To and References, the text and HTML
  bodies with line endings unified and trailing whitespace dropped, and the
  attachments' blob ids. Comparing against the existing drafts instead was
  rejected: recipients and bodies are not in Postgres — only in the encrypted
  blob store — so each candidate would have to be read, decrypted and parsed
  inside the Email/set transaction, and the composer's Date, Message-ID and
  boundary make the bytes of two identical drafts differ. A plain hash was
  rejected too: it would let anyone holding a database copy confirm a guess
  at a draft's content. Keyed, the database alone confirms nothing, and the
  DEK's holder can read the blobs anyway.
- **Concurrency.** Every key draft write — create, replace, discard, and a
  key's submission — takes a transaction-scoped advisory lock on (user, key)
  (`store.LockAPIKeyDrafts`, as `feedbackOnceLock`), so two identical creates
  racing cannot both miss each other, and a discard cannot interleave with a
  send of the same draft. A key's Email/set that creates or discards takes it
  ONCE, as the first statement of its transaction (amended after a
  review: taken per item, it came after a destroy's message and feedback locks
  in one call and before them in another, a deadlock between two calls of the
  same key). The lock is taken before anything that locks the
  key's `app.api_keys` row (a draft insert does, through its foreign key), so
  the hold of a send for approval — which runs in the admission transaction,
  under that row lock — does not take it; it sends nothing, and the approved
  run comes back through the send path, which does.

### 3. `draft_update` and `draft_discard`, under `mail:draft`, only on the key's own drafts

- Two Mailroom extensions to Email/set, for API keys only (a session gets
  `invalidProperties` / `forbidden` and edits drafts as before):
  - a create carrying **`replacesEmailId`** is a new version of that draft;
  - **`discardDrafts: [ids]`** (answered in `discarded` / `notDiscarded`).
- Both are refused, with words an agent can act on, unless the draft is
  **this key's** (`app.api_key_drafts`) — never the owner's, never another
  key's — and still live (as in §2), with **no submission** and **no send
  pending** (an `app.api_key_pending` row of kind `send` naming it, pending and
  unexpired, or approved and not yet run). The approval pins the draft by
  content hash; approving a send of a draft the key had meanwhile replaced
  would send the old words, so the replacement waits for the decision.
- A replacement is checked, created, recorded as the key's (with its
  fingerprint), and the old version moved to Trash — in ONE transaction; a
  failure after the create aborts the whole call, so the old draft stays and
  no new one exists. The old version's In-Reply-To and References are kept
  when the create gives none. `draft_update` takes the same fields as
  `draft_new` (the whole new content) and returns the new id.
- **Discarded and replaced versions go to Trash, not destroyed.** A draft
  retired by its author is normally destroyed (a session's edit destroys the
  old version), and that was the alternative weighed. It is not taken because
  spec §13a allows "no agent path to immediate hard deletion, ever", and
  ADR-042 §3 makes every key destroy soft; this is a key path. Trash keeps the
  owner able to see what the agent replaced or withdrew for the retention
  window, and every other delete in the MCP tools already goes there.
- The cost of Trash is a draft-flagged copy the key still "owns". So
  **a key may send only a draft that is still in Drafts** (`keyMaySubmitDraft`,
  which also runs when a held send is approved): a replaced or discarded
  version is refused — "this draft was replaced or discarded and is no longer
  in Drafts; send the current version" — and a confused agent holding the old
  id cannot send words it took back. The rule is about where the draft IS, read
  at send time: a replaced or discarded version cannot be sent while it is in
  Trash; moved back into Drafts (by the owner, or by a key with `mail:move`
  restoring its own version), it is the key's draft again. Approval applies
  the same rule before anything runs: a held send of a draft the key has since
  withdrawn is refused as `draftChanged` ("the key discarded or replaced this
  draft"), not approved and then failed.
- **Scope.** Both need `mail:draft`, never `mail:delete`, count as no delete
  for the mass-delete rule and are never held for approval: they touch only
  what the key itself wrote and never sent, and the result is recoverable.
  `emailSetNeeds` classifies them; `KeyMayCall` reads the same line, so the
  tools are listed exactly for keys with `mail:draft`. Every write goes
  through the ordinary store paths (`SaveDraft`, `ApplyEmailUpdate`), which
  append the change_log rows.

### 4. Markdown bodies, rendered by goldmark, raw HTML never passed through

- `body` in `draft_new`, `draft_reply` and `draft_update` is documented as
  Markdown. The MCP layer sends the Email/set create with **both** `textBody`
  (the body exactly as written) and `htmlBody` (its rendering), which the
  composer builds as `multipart/alternative`. A body that renders to nothing
  stays plain text.
- **Library: `github.com/yuin/goldmark` v1.8.6** (MIT; compatible with
  AGPL-3.0-only), pure Go, no transitive dependencies, CommonMark-compliant,
  the renderer Hugo and others ship. Extensions: GFM tables, strikethrough,
  linkify. `govulncheck` is a required job.
- **Configuration** (`internal/mcp/markdown.go`):
  - not `html.WithUnsafe`, and raw HTML — inline and block — is rendered as
    **escaped text** by a node renderer of our own, so "Hi <Bob>" survives and
    `<script>` arrives as visible characters (goldmark's default would drop it
    silently);
  - links keep only `http`/`https` destinations with a host and `mailto`;
    anything else (javascript:, data:, vbscript:, file:, relative, an
    obfuscated scheme) loses the link and keeps its text — an allowlist, so an
    encoding trick fails closed;
  - images are dropped and rendered as their alt text: a key cannot attach
    files, and a remote image is a tracking pixel the owner never chose;
  - hard wraps: a single newline is a `<br>`, because a model's line breaks
    are intentional and a body with no Markdown must read as written.
- The rendering is the first of two layers: the API runs every `htmlBody`
  through `SanitizeOutboundHTML`, the send path's trust boundary, before it is
  stored.
- The approval summary and the pending view are unchanged: they are derived
  from the canonical request and the planned send, not from the body.

## Consequences

- Tool list (amends ADR-044 §4): `draft_update` and `draft_discard` are added;
  the names remain a public contract.
- A key's draft creates are serialised per key (the advisory lock); a key's
  rate already bounds how many it makes.
- `content_fingerprint` is NULL for drafts written before 00077; they are never
  collapsed into, which is the safe reading.
- Whether a key may send its own draft is read at send time from where the
  draft is: one the owner moves back into Drafts is the key's to send again.

## References

spec §13a · ADR-009 · ADR-042 §3, §5, §7 · ADR-044 ·
`internal/api/key_drafts.go`, `internal/store/api_key_drafts.go`,
`internal/compose/fingerprint.go`, `internal/mcp/markdown.go`,
`db/migrations/00077_api_key_draft_fingerprint.sql`, `docs/mcp.md`
