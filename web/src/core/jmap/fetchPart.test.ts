// SPDX-License-Identifier: AGPL-3.0-only

// Tests for the download-endpoint type gate (#131, #99).
//
// fetchPart is the control that decides whether a stranger's bytes are handed to a
// renderer. Its rule is that only the type THE SERVER CHOSE counts — never the
// type the part declared — because internal/api/blob.go maps every scripting
// context onto application/octet-stream, and that downgrade is only worth anything
// if the client refuses what comes back downgraded.
//
// So the assertions here are mostly negatives, and one of them is the whole point:
// a part that CLAIMS to be a PNG and is SERVED as octet-stream must come back null.

import { describe, expect, it } from "vitest";

import { isDecryptedPartUrl } from "./blobs";
import { JmapClient } from "./client";

const SESSION = {
  apiUrl: "/jmap/api",
  downloadUrl: "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
  uploadUrl: "/jmap/upload/{accountId}",
  eventSourceUrl: "/jmap/eventsource",
  capabilities: {},
  accounts: { acct: {} },
  primaryAccounts: { "urn:ietf:params:jmap:mail": "acct" },
  state: "s1",
};

/** A client whose session is real (fetched through connect(), so the download URL
 * is expanded by the same code the app uses) and whose blob fetches are scripted. */
async function clientServing(
  blobResponse: () => Response,
): Promise<{ client: JmapClient; urls: string[] }> {
  const urls: string[] = [];
  const fakeFetch = (async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/jmap/session") || url === "/jmap/session") {
      return new Response(JSON.stringify(SESSION), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    urls.push(url);
    return blobResponse();
  }) as unknown as typeof fetch;

  const client = new JmapClient({ fetch: fakeFetch });
  await client.connect();
  return { client, urls };
}

function served(type: string, body = "bytes", extraHeaders: Record<string, string> = {}) {
  return () =>
    new Response(body, { status: 200, headers: { "Content-Type": type, ...extraHeaders } });
}

const IMAGES: ReadonlySet<string> = new Set(["image/png", "image/jpeg"]);

describe("fetchBlobOnce (#804)", () => {
  it("makes exactly one GET and hands the bytes back as an inert type, whatever the server said", async () => {
    const { client, urls } = await clientServing(served("text/html", "<script>x</script>"));
    const got = await client.fetchBlobOnce("/jmap/download/acct/B1.e.2/x.html");
    expect(urls).toEqual(["/jmap/download/acct/B1.e.2/x.html"]);
    expect(got.kind).toBe("ok");
    if (got.kind === "ok") expect(got.blob.type).toBe("application/octet-stream");
  });

  it("reads a scan refusal off that same response", async () => {
    const { client, urls } = await clientServing(
      () =>
        new Response("{}", {
          status: 403,
          headers: { "X-Mailroom-Attachment-Scan": "flagged", "X-Mailroom-Attachment-Signature": "Eicar" },
        }),
    );
    const got = await client.fetchBlobOnce("/jmap/download/acct/B1.e.2/x.zip");
    expect(urls).toHaveLength(1);
    expect(got).toEqual({ kind: "blocked", blocked: { verdict: "flagged", signature: "Eicar" } });
  });

  it("a 403 that is not a scan verdict, or a 404, is a plain failure", async () => {
    const { client } = await clientServing(() => new Response("no", { status: 404 }));
    expect((await client.fetchBlobOnce("/jmap/download/acct/B1.e.2/x")).kind).toBe("failed");
  });
});

describe("isDecryptedPartUrl (#804)", () => {
  const H = "ab".repeat(32);
  it("is true only for the decrypted-part form", () => {
    expect(isDecryptedPartUrl(`/jmap/download/A/${H}.e.2/x.pdf?type=`)).toBe(true);
    expect(isDecryptedPartUrl(`/jmap/preview/A/${H}.e.1.3/x.pdf`)).toBe(true);
    expect(isDecryptedPartUrl(`/jmap/download/A/${H}.2/x.pdf`)).toBe(false);
    expect(isDecryptedPartUrl(`/jmap/download/A/${H}/x.pdf`)).toBe(false);
    expect(isDecryptedPartUrl(`/jmap/download/A/B.e.2/x.pdf`)).toBe(false);
    // The marker in the file name does not count.
    expect(isDecryptedPartUrl(`/jmap/download/A/${H}.2/${H}.e.2`)).toBe(false);
  });
});

describe("fetchPart type gate", () => {
  it("returns the bytes when the server served an allowed type", async () => {
    const { client } = await clientServing(served("image/png"));
    const blob = await client.fetchPart("B1", IMAGES);
    expect(blob).not.toBeNull();
    expect(blob?.type).toBe("image/png");
  });

  it("ignores media-type parameters and casing on the served type", async () => {
    const { client } = await clientServing(served("IMAGE/PNG; charset=binary"));
    expect(await client.fetchPart("B1", IMAGES)).not.toBeNull();
  });

  it("refuses octet-stream — the bucket blob.go downgrades every dangerous type into", async () => {
    const { client } = await clientServing(served("application/octet-stream"));
    expect(await client.fetchPart("B1", IMAGES)).toBeNull();
  });

  it("refuses a scripting context even when the caller asked for images", async () => {
    for (const type of ["text/html", "image/svg+xml", "application/xhtml+xml"]) {
      const { client } = await clientServing(served(type));
      expect(await client.fetchPart("B1", IMAGES), `${type} passed the gate`).toBeNull();
    }
  });

  it("refuses a type the caller did not ask for, even though it is otherwise safe", async () => {
    // The gate is per-caller on purpose: adding a renderer must not widen what any
    // other renderer accepts.
    const { client } = await clientServing(served("application/pdf"));
    expect(await client.fetchPart("B1", IMAGES)).toBeNull();
  });

  it("refuses a missing Content-Type rather than guessing", async () => {
    const { client } = await clientServing(
      () => new Response("bytes", { status: 200, headers: {} }),
    );
    // Response() supplies its own default type for a string body; whatever it is,
    // it is not in the caller's set, so the gate must refuse it.
    expect(await client.fetchPart("B1", IMAGES)).toBeNull();
  });

  it("returns null on a non-2xx without reading a body", async () => {
    const { client } = await clientServing(
      () => new Response("nope", { status: 404, headers: { "Content-Type": "image/png" } }),
    );
    expect(await client.fetchPart("B1", IMAGES)).toBeNull();
  });
});

describe("fetchPart size bound", () => {
  it("refuses a part whose declared Content-Length exceeds the bound", async () => {
    const { client } = await clientServing(
      served("image/png", "bytes", { "Content-Length": "999999" }),
    );
    expect(await client.fetchPart("B1", IMAGES, "p", 10)).toBeNull();
  });

  it("refuses oversized BYTES even when no Content-Length was sent", async () => {
    // A length header is a claim, and a chunked response need not send one — so the
    // bound has to be re-checked against what actually arrived.
    const { client } = await clientServing(served("image/png", "0123456789abcdef"));
    expect(await client.fetchPart("B1", IMAGES, "p", 4)).toBeNull();
  });

  it("allows a part inside the bound", async () => {
    const { client } = await clientServing(served("image/png", "abc"));
    expect(await client.fetchPart("B1", IMAGES, "p", 1024)).not.toBeNull();
  });

  it("is unbounded only when the caller passes no bound", async () => {
    const { client } = await clientServing(served("image/png", "0123456789"));
    expect(await client.fetchPart("B1", IMAGES)).not.toBeNull();
  });
});

describe("fetchInlineImage still rides on the same gate (#99 unchanged)", () => {
  it("accepts a raster image", async () => {
    const { client } = await clientServing(served("image/gif"));
    expect(await client.fetchInlineImage("B1")).not.toBeNull();
  });

  it("still refuses SVG and HTML", async () => {
    for (const type of ["image/svg+xml", "text/html", "application/octet-stream"]) {
      const { client } = await clientServing(served(type));
      expect(await client.fetchInlineImage("B1"), `${type} passed`).toBeNull();
    }
  });
});

// A REFUSAL THAT SAYS WHY (#47). The server answers a quarantined part with 403
// and the verdict in headers (internal/api/blob.go blobQuarantined); the client
// reads them so the app can explain, rather than the browser showing a bare 403.
const refused = (verdict: string, signature?: string) => () =>
  new Response("{}", {
    status: 403,
    headers: {
      "X-Mailroom-Attachment-Scan": verdict,
      ...(signature ? { "X-Mailroom-Attachment-Signature": signature } : {}),
    },
  });

describe("probeBlob reads the refusal before the browser navigates (#47)", () => {
  it("a flagged part: blocked, with the detection name", async () => {
    const { client, urls } = await clientServing(refused("flagged", "Eicar-Test-Signature"));
    expect(await client.probeBlob("/jmap/download/acct/B1/x")).toEqual({
      kind: "blocked",
      blocked: { verdict: "flagged", signature: "Eicar-Test-Signature" },
    });
    expect(urls).toEqual(["/jmap/download/acct/B1/x"]);
  });

  it("an unknown part: blocked, no detection name even if one were sent", async () => {
    const { client } = await clientServing(refused("unknown", "ignored"));
    expect(await client.probeBlob("/u")).toEqual({
      kind: "blocked",
      blocked: { verdict: "unknown", signature: null },
    });
  });

  it("a 403 that is not a scan verdict is a failure, not a verdict", async () => {
    const { client } = await clientServing(() => new Response("no", { status: 403 }));
    expect(await client.probeBlob("/u")).toEqual({ kind: "failed" });
  });

  it("a served part is ok (positive control)", async () => {
    const { client } = await clientServing(served("image/png"));
    expect(await client.probeBlob("/u")).toEqual({ kind: "ok" });
  });

  it("uses HEAD, so no body is transferred", async () => {
    const methods: string[] = [];
    const client = new JmapClient({
      fetch: (async (_input: RequestInfo | URL, init?: RequestInit) => {
        methods.push(init?.method ?? "GET");
        return new Response(null, { status: 200 });
      }) as unknown as typeof fetch,
    });
    await client.probeBlob("/u");
    expect(methods).toEqual(["HEAD"]);
  });
});

describe("fetchPart reports a scan refusal to the caller (#47)", () => {
  it("calls onBlocked and still returns null", async () => {
    const { client } = await clientServing(refused("flagged", "Eicar-Test-Signature"));
    const seen: unknown[] = [];
    expect(await client.fetchPart("B1", IMAGES, "p", undefined, (b) => seen.push(b))).toBeNull();
    expect(seen).toEqual([{ verdict: "flagged", signature: "Eicar-Test-Signature" }]);
  });
});
