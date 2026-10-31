// SPDX-License-Identifier: AGPL-3.0-only

// "CONNECT AN AI TOOL" — the setup text, kept in step with docs/mcp.md.
//
// The snippets and the tool table are docs/mcp.md's. They cannot be imported
// from it at build time (the image's web stage copies only web/), so they are
// restated here and setupDocs.test.ts reads the document and fails when the
// two differ: a snippet rendered with docs/mcp.md's placeholders must equal
// the document's code block character for character, and every tool row's
// required scopes must equal this table's.
//
// Generic first (ADR-044, "no lock-in"): the URL and the header, which any
// MCP client that speaks Streamable HTTP takes. The bridge and the two named
// clients are EXAMPLES, labelled as such.

/** The placeholders docs/mcp.md uses, so the test can render the doc's form. */
export const DOC_ORIGIN = "https://mail.example.com";
export const DOC_TOKEN = "mr_your_key_here";

/** The MCP endpoint for this deployment: the page's origin plus `/mcp`. */
export function mcpEndpoint(origin: string): string {
  return `${origin.replace(/\/+$/, "")}/mcp`;
}

/** The header line a client sends. */
export function authorizationHeader(token: string): string {
  return `Authorization: Bearer ${token}`;
}

/** True for an `http:` endpoint on a non-loopback host: the case where
 * mcp-remote refuses to connect without `--allow-http`, and where the key
 * crosses the network unencrypted. */
export function needsAllowHttp(endpoint: string): boolean {
  let u: URL;
  try {
    u = new URL(endpoint);
  } catch {
    return false;
  }
  if (u.protocol !== "http:") return false;
  const host = u.hostname.replace(/^\[|\]$/g, "").toLowerCase();
  if (host === "localhost" || host === "::1") return false;
  if (/^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(host)) return false;
  return true;
}

/** The stdio bridge entry (docs/mcp.md, "Clients that only run local (stdio)
 * servers"): `mcp-remote` with the header split so the space lives in the
 * environment variable. */
export function bridgeSnippet(endpoint: string, token: string): string {
  const allowHttp = needsAllowHttp(endpoint) ? `\n        "--allow-http",` : "";
  return `{
  "mcpServers": {
    "mailroom": {
      "command": "npx",
      "args": [
        "-y",
        "mcp-remote",
        "${endpoint}",${allowHttp}
        "--header",
        "Authorization:\${MAILROOM_AUTH}"
      ],
      "env": {
        "MAILROOM_AUTH": "Bearer ${token}"
      }
    }
  }
}`;
}

/** The local LLM tool example (docs/mcp.md, "Worked examples"). */
export function remoteYamlSnippet(endpoint: string, token: string): string {
  return `mcp_servers:
  mailroom:
    url: ${endpoint}
    headers:
      Authorization: Bearer ${token}`;
}

/**
 * The tools, as docs/mcp.md's table lists them: the scopes each one NEEDS
 * (the parenthesised "+ search:read for text" extras are optional and not
 * listed here). `restrictedOut`: absent from an account-restricted key.
 */
export const MCP_TOOLS: readonly { names: readonly string[]; scopes: readonly string[]; restrictedOut?: true }[] = [
  { names: ["search_mail"], scopes: ["mail:read"] },
  { names: ["read_message"], scopes: ["mail:read"] },
  { names: ["list_mailboxes"], scopes: ["mail:read"] },
  { names: ["list_labels"], scopes: ["labels:read"] },
  { names: ["apply_label", "remove_label"], scopes: ["labels:read", "mail:move"] },
  { names: ["mark_read", "mark_unread"], scopes: ["mail:move"] },
  { names: ["archive"], scopes: ["mail:read", "mail:move"] },
  { names: ["move_to_mailbox"], scopes: ["mail:read", "mail:move"] },
  { names: ["list_areas"], scopes: ["mail:read"] },
  { names: ["move_to_area"], scopes: ["mail:read", "mail:move"] },
  { names: ["draft_new"], scopes: ["mail:draft"] },
  { names: ["draft_reply"], scopes: ["mail:read", "mail:draft"] },
  { names: ["draft_update"], scopes: ["mail:draft"] },
  { names: ["draft_discard"], scopes: ["mail:draft"] },
  { names: ["send_draft"], scopes: ["mail:send"] },
  { names: ["delete_message"], scopes: ["mail:delete"] },
  { names: ["find_contacts"], scopes: ["contacts:read"], restrictedOut: true },
  { names: ["get_contact"], scopes: ["contacts:read"], restrictedOut: true },
  { names: ["get_pending"], scopes: [] },
];

/** The tools a key with these scopes gets (advisory, like tools/list: the
 * server decides every call). */
export function toolsFor(scopes: readonly string[], restricted: boolean): string[] {
  const have = new Set(scopes);
  const out: string[] = [];
  for (const row of MCP_TOOLS) {
    if (restricted && row.restrictedOut) continue;
    if (row.scopes.every((s) => have.has(s))) out.push(...row.names);
  }
  return out;
}
