// SPDX-License-Identifier: AGPL-3.0-only

// THE SETUP TEXT IS docs/mcp.md's (#49). The Settings page restates the
// snippets and the tool table (the image's web stage cannot read docs/), so
// this reads the document and fails when the two drift: rendered with the
// document's placeholders, each snippet must equal the document's code block
// exactly, and each tool's required scopes must equal the table's.

import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import mcpDoc from "../../../../../../docs/mcp.md?raw";
import { DOC_ORIGIN, DOC_TOKEN, MCP_TOOLS, bridgeSnippet, mcpEndpoint, needsAllowHttp, remoteYamlSnippet, toolsFor } from "./setup";
import { PRESETS } from "./model";
import { SetupGuide } from "./SetupGuide";

function codeBlocks(lang: string): string[] {
  const re = new RegExp("```" + lang + "\\n([\\s\\S]*?)\\n```", "g");
  return [...mcpDoc.matchAll(re)].map((m) => m[1] ?? "");
}

/** The tool table: name(s) → the backticked scopes outside any parentheses. */
function docTools(): Map<string, string[]> {
  const out = new Map<string, string[]>();
  for (const line of mcpDoc.split("\n")) {
    const cells = line.split("|").map((c) => c.trim());
    // | tool | what | methods | scopes |  → ["", tool, what, methods, scopes, ""]
    if (cells.length !== 6 || !cells[1]?.startsWith("`")) continue;
    const names = [...(cells[1] ?? "").matchAll(/`([a-z_]+)`/g)].map((m) => m[1] ?? "");
    const scopeCell = (cells[4] ?? "").replace(/\([^)]*\)/g, "");
    const scopes = [...scopeCell.matchAll(/`([a-z]+:[a-z]+)`/g)].map((m) => m[1] ?? "").sort();
    for (const n of names) out.set(n, scopes);
  }
  return out;
}

describe("setup snippets match docs/mcp.md", () => {
  it("the parse finds the document's blocks (else every assertion below is vacuous)", () => {
    expect(codeBlocks("json")).toHaveLength(1);
    expect(codeBlocks("yaml")).toHaveLength(1);
    expect(docTools().size).toBeGreaterThanOrEqual(17);
  });

  it("the endpoint is the origin plus /mcp, as the document's URL is", () => {
    expect(mcpEndpoint(DOC_ORIGIN)).toBe("https://mail.example.com/mcp");
    expect(mcpEndpoint("https://mail.example.com/")).toBe("https://mail.example.com/mcp");
    expect(mcpDoc).toContain("`https://mail.example.com/mcp`");
    expect(mcpDoc).toContain(`Authorization: Bearer ${DOC_TOKEN}`);
  });

  it("the stdio bridge block is the document's, character for character", () => {
    expect(bridgeSnippet(mcpEndpoint(DOC_ORIGIN), DOC_TOKEN)).toBe(codeBlocks("json")[0]);
  });

  it("the local LLM tool block is the document's", () => {
    expect(remoteYamlSnippet(mcpEndpoint(DOC_ORIGIN), DOC_TOKEN)).toBe(codeBlocks("yaml")[0]);
  });

  it("fills in a real key without touching the shape", () => {
    const token = "mr_" + "a".repeat(52);
    const snippet = bridgeSnippet("https://mail.example.org/mcp", token);
    expect(JSON.parse(snippet)).toEqual({
      mcpServers: {
        mailroom: {
          command: "npx",
          args: ["-y", "mcp-remote", "https://mail.example.org/mcp", "--header", "Authorization:${MAILROOM_AUTH}"],
          env: { MAILROOM_AUTH: `Bearer ${token}` },
        },
      },
    });
  });
});

describe("the tool table matches docs/mcp.md", () => {
  it("lists exactly the document's tools, each with the document's required scopes", () => {
    const doc = docTools();
    const ours = new Map<string, string[]>();
    for (const row of MCP_TOOLS) for (const n of row.names) ours.set(n, [...row.scopes].sort());
    expect([...ours.keys()].sort()).toEqual([...doc.keys()].sort());
    for (const [name, scopes] of doc) expect({ name, scopes: ours.get(name) }).toEqual({ name, scopes });
  });

  it("gives each preset the tools its scopes allow, and a restricted key no contacts", () => {
    const read = PRESETS.find((p) => p.id === "read")!;
    expect(toolsFor(read.scopes, false)).toEqual([
      "search_mail",
      "read_message",
      "list_mailboxes",
      "list_labels",
      "list_areas",
      "find_contacts",
      "get_contact",
      "get_pending",
    ]);
    expect(toolsFor(read.scopes, true)).not.toContain("find_contacts");
    const assistant = PRESETS.find((p) => p.id === "assistant")!;
    const tools = toolsFor(assistant.scopes, false);
    expect(tools).toContain("draft_reply");
    // #872: a key's own drafts can be replaced and discarded under mail:draft.
    expect(tools).toContain("draft_update");
    expect(tools).toContain("draft_discard");
    expect(toolsFor(read.scopes, false)).not.toContain("draft_update");
    // Areas (K2): listed with mail:read, filled with mail:move as well.
    expect(tools).toContain("move_to_area");
    expect(toolsFor(read.scopes, false)).not.toContain("move_to_area");
    expect(tools).not.toContain("send_draft");
    expect(tools).not.toContain("delete_message");
  });
});

describe("plain HTTP on a private network (#863)", () => {
  const token = "mr_" + "a".repeat(52);
  const argsOf = (origin: string): string[] =>
    (JSON.parse(bridgeSnippet(mcpEndpoint(origin), token)) as { mcpServers: { mailroom: { args: string[] } } }).mcpServers.mailroom.args;

  it("adds --allow-http right after the URL for non-loopback http", () => {
    expect(argsOf("http://mail.example.test:8090")).toEqual([
      "-y",
      "mcp-remote",
      "http://mail.example.test:8090/mcp",
      "--allow-http",
      "--header",
      "Authorization:${MAILROOM_AUTH}",
    ]);
    expect(needsAllowHttp("http://10.0.0.5/mcp")).toBe(true);
  });

  it("adds nothing for https or loopback http", () => {
    expect(argsOf("https://mail.example.com")).not.toContain("--allow-http");
    for (const o of ["http://localhost:8090", "http://127.0.0.1:8090", "http://127.1.2.3", "http://[::1]:8090"]) {
      expect(argsOf(o)).not.toContain("--allow-http");
      expect(needsAllowHttp(mcpEndpoint(o))).toBe(false);
    }
  });

  it("shows the warning in the guide only for non-loopback http", () => {
    const html = (origin: string): string =>
      renderToStaticMarkup(createElement(SetupGuide, { origin, token: null, scopes: null }));
    expect(html("http://mail.example.test")).toContain("plain HTTP");
    expect(html("http://mail.example.test")).toContain("--allow-http");
    expect(html("https://mail.example.com")).not.toContain("plain HTTP");
    expect(html("https://mail.example.com")).not.toContain("--allow-http");
    expect(html("http://localhost:8090")).not.toContain("plain HTTP");
  });
});
