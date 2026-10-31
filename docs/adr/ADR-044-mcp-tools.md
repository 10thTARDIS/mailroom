# ADR-044: MCP tools — standard Streamable HTTP, stateless, keys only, a thin client of the JMAP gate

**Status:** Accepted (v1.95 increment "M"; spec §13a). Builds on
ADR-042, which it does not widen.

## Context

ADR-042 gave agents API keys and put every rule a key obeys in one place: the
JMAP dispatcher's key gate (scope table, account restriction, rate and budget,
confirm-mode holds, audit, anomaly rules). Its consequences section binds the MCP
tools to be a thin client of that gate with no second enforcement path.

The decisions add **no lock-in**: standard MCP and
a standard `Authorization: Bearer` header, no client-specific extensions, any
MCP-capable client with any model; stdio-only clients through a generic bridge;
CI drives the endpoint with the MCP SDK's own client. Named clients are
documentation examples only.

## Decision

### 1. Transport: Streamable HTTP at `/mcp`, stateless, JSON responses

- The MCP specification's **Streamable HTTP** transport, served by the existing
  process at `/mcp`. No stdio server and no SSE-only transport.
- **Stateless mode.** Each POST is a complete exchange; the server issues and
  reads no `Mcp-Session-Id`, keeps no session table, and answers `GET` (the
  optional server-to-client SSE stream) and `DELETE` (session teardown) with
  `405`. Nothing the tools do needs a server-initiated message: no sampling, no
  elicitation, no progress or list-changed notifications. So nothing is lost,
  and the endpoint works behind any reverse proxy with no affinity and survives
  a restart mid-conversation.
- **JSON responses** (`application/json`), not an SSE stream per POST: every
  tool answers once.
- Request bodies are capped at the JMAP request limit (10 MiB). The SDK's
  DNS-rebinding guard, which refuses a loopback connection with a non-loopback
  `Host`, is **off**: it protects a credential-less local server from a browser,
  it breaks every reverse proxy on the same host, and this endpoint answers only
  a request carrying a key in `Authorization`, which a browser never attaches by
  itself and a rebound page cannot add (no CORS is granted).
- `tools/list` results are marked `cacheScope: "private"`, `ttlMs: 0`: the list
  differs per key.

### 2. Keys only

- `/mcp` is inside the API handler, behind the same chain as `/jmap`: the bearer
  gate, `nosniff`, the per-key and aggregate rate buckets, and `keyPolicyGate`,
  which loads the key's policy into the request context. `keyPathAllowed` admits
  `/mcp` for keys (every method, so the transport can answer 405 itself).
- **A browser session is refused (403).** MCP is a programmatic surface. Accepting
  the cookie would make every tool a cross-site request away from the browser's
  whole authority, and would attribute to a person what an agent did.
- **No credential at all gets 401 with `WWW-Authenticate: Bearer realm="mailroom"`**
  (`api.MCPChallenge`, mounted on `/mcp` outside the auth chain in OIDC mode),
  which is how an MCP client learns it needs a key. A bad, revoked or suspended
  key gets the bearer gate's uniform 401 as on `/jmap`. Under dev-auth every
  request already is the user's session, so a keyless request reaches the
  403.
- The bridge refuses a tool call whose context carries no key policy, a second
  check independent of the HTTP one.

### 3. One enforcement path

- Every tool call becomes **one JMAP request** run by `(*API).dispatch` with the
  request context, through `invoke` and the key gate. Back-references work as on
  `/jmap` (`search_mail` is `Email/query` plus `Email/get` with `#ids`).
- `internal/mcp` never touches the store, never reads a key's settings, and
  re-implements no check. A method error or a per-object SetError becomes an MCP
  **tool error** (`isError: true`) with the JMAP type and description; a
  confirm-mode hold (SetError `pendingApproval`) becomes a normal result
  `{status: "pending_approval", pendingId, expiresAt, …}`. Protocol errors are
  only for malformed MCP.
- **tools/list filtering is advisory.** Each tool declares its JMAP uses. A tool
  is listed only when `KeyMayCall` says the key may make every use. `KeyMayCall`
  reads the same tables the gate enforces (`keyPolicies`; for an `Email/set`
  update, `emailUpdateFieldScopes` by the fields its patch sets; for the computed
  methods, the method's own needs function on representative arguments) and
  mirrors gate steps 2–4 (reachable, account restriction, scopes). It decides
  nothing: a hidden tool that is called anyway is refused by the dispatcher.
  `TestEveryToolUseIsInTheKeyPolicyTable` fails when a declared use has no
  reachable line in the table, so a typo cannot hide a tool silently.

### 4. Tools and their names

The names are a public contract (clients and prompts refer to them):
`search_mail`, `read_message`, `list_mailboxes`, `list_labels`, `apply_label`,
`remove_label`, `mark_read`, `mark_unread`, `archive`, `move_to_mailbox`,
`list_areas`, `move_to_area`, `draft_new`, `draft_reply`, `send_draft`,
`delete_message`, `find_contacts`, `get_contact`, `get_pending`, and, since
v1.95.0, `draft_update` and `draft_discard` (ADR-045). `docs/mcp.md` lists each
tool's methods and scopes.

- **Areas (amended, increment K2).** M shipped without them,
  because ADR-042's table refused every `Area/*` method to keys. K2 opened
  `Area/get` (ADR-042 §3a), and the tools follow: `list_areas` (Area/get),
  `move_to_area` (Area/get, Email/get, and an Email/set update setting
  `AddLabelIDs` and `RemoveLabelIDs`, under `mail:move`), and `search_mail`'s
  `areaId` (Email/query's `inArea`). `move_to_area` is the web client's Re-file
  swap (the area's own label on, every other area's own label off), skips
  messages already in the area and reports them as `alreadyThere`, and reads
  each moved message's `area` back after the set: one that another label
  still routes to an earlier area is a `notMoved` failure, never counted as
  moved. That a key's move is never the owner's placement evidence, and that a
  key cannot undo the owner's own filing, is the server's rule (ADR-042 §3a),
  not the tool's.
- Each tool has an input schema with bounds (lengths, list sizes ≤ 50, result
  limits), validated by the SDK before the tool runs, and returns
  `structuredContent` plus a short text rendering that carries the same JSON.
- `read_message` returns the text body (HTML converted to text, quotes kept)
  capped at 64 KiB with `truncated`, and attachments by name, type and size,
  never content. `draft_new`/`draft_reply` attach nothing; their body is
  Markdown, sent as plain text plus a rendering, and their wording follows the
  key's send mode (amended for ADR-045).
  `send_draft` needs an explicit draft id, and the gate sends only drafts the key
  wrote. `delete_message` is the soft delete.

### 5. Prompt-injection posture

- Third-party text (names, addresses, subjects, previews, bodies, attachment
  names, learned contact names, approval summaries that quote a draft) is
  returned only under fields named `untrusted_content`, never spliced into a
  tool's own prose; tool descriptions and the server instructions say it is
  data, not instructions.
- Every string a tool returns, structured or text, is cleaned of C0/C1 controls
  (except newline and tab), explicit directional formatting characters, and all
  other Unicode format characters (zero-width characters, BOM, soft hyphen,
  tags). The predicate is the one `store.cleanContactName` applies to learned
  names, restated in `internal/mcp` because the store's version is
  unexported and single-line. Both also strip the rest of Unicode's
  Default_Ignorable_Code_Point set that is not Cf (Hangul fillers such as
  U+3164, variation selectors, the combining grapheme joiner), from the
  standard library's Other_Default_Ignorable_Code_Point and Variation_Selector
  tables (amended after a review; `CleanLearnedContactName` and the
  `contact-names-clean` classifier gained the same set).
- **Hidden HTML is dropped** (amended after a review). Text a person
  never sees but a model reads is an injection channel, and the server's
  sanitiser keeps the harmless-to-a-browser markup that hides it. The
  HTML-to-text converter drops the whole subtree of an element with `hidden`,
  `aria-hidden="true"`, or an inline style of `display:none`,
  `visibility:hidden|collapse`, `opacity:0`, `font-size:0` (any unit),
  `color:transparent` (or a zero-alpha `rgba`/`hsla`), or a zero
  height/width/max-height/max-width with `overflow:hidden`. **Residual:** it
  cannot see hiding done by a `<style>` block or a class, text coloured like
  its background, text positioned off-screen, or a font merely too small to
  read. Those cannot be solved without rendering the message; scopes,
  approvals, soft deletes and send-only-own-drafts are what bound an agent
  that is fooled by them.

### 6. The SDK, behind our own interface

- `github.com/modelcontextprotocol/go-sdk` **v1.8.0** (released 2026-09-04), the
  official Go SDK. Its licence is Apache-2.0, with contributions not yet
  relicensed remaining under MIT; both are compatible with AGPL-3.0-only.
- Only `internal/mcp` (and its test helper `internal/mcp/mcptest`) imports it;
  `TestOnlyThisPackageImportsTheSDK` walks the repository to keep it so. The
  package's exported surface is `NewHandler` (an `http.Handler`), the `Backend`
  interface (JMAP invocations in and out, plus `KeyMayCall`) and `Catalog`. No
  SDK type reaches `internal/api`.
- New module dependencies through the SDK: `google/jsonschema-go`,
  `segmentio/encoding` and `segmentio/asm`, `yosida95/uritemplate`,
  `golang.org/x/time`. `govulncheck` is a required job.

## Consequences

- A tool is a few JMAP calls; adding one is adding a declared use, and if its
  method is not open to keys, opening it is ADR-042's reviewed table change.
- MCP authorization (OAuth for remote MCP, per the MCP specification) is v1.96;
  until then clients whose connectors expect OAuth use a generic stdio bridge
  with the bearer header (`docs/mcp.md`).
- Threads, rules and attachments stay out of the tools until the key table
  opens them. Areas were opened in K2 (§4); creating, renaming and deleting
  them (Area/set) stays session-only.

## References

spec §13a · ADR-042 · ADR-009 · `internal/mcp`,
`internal/api/mcp_bridge.go`, `docs/mcp.md`
