// SPDX-License-Identifier: AGPL-3.0-only

// The JMAP binary + push URLs (RFC 8620 §2, §6, §7.3).
//
// The Session resource does not hand a client fixed URLs: it hands it TEMPLATES
// (`/jmap/download/{accountId}/{blobId}/{name}?type={type}`). Expanding them —
// and URI-encoding every value that goes into one — is this module's whole job,
// so that no other module has to know the shape of a Mailroom URL. If the server
// ever moves an endpoint, the session says so and nothing here changes.
//
// DOM-free, like the rest of core/: `fetch` and `EventSource` are runtime Web
// APIs (browsers, Node 18+, a Tauri webview), not UI frameworks.

import type { JmapSession } from "../session";
import type { AccountId, BlobId } from "./types";

/** Expands an RFC 6570-style level-1 template: every `{name}` is replaced with
 * the URI-encoded value, and any variable the caller did not supply is dropped
 * (so `?type={type}` with no type yields `?type=`, which the server ignores). */
export function expandTemplate(template: string, vars: Record<string, string>): string {
  return template.replace(/\{(\w+)\}/g, (_match, name: string) =>
    encodeURIComponent(vars[name] ?? ""),
  );
}

/** The URL that serves one blob: a whole message (`blobId`), or one MIME part of
 * it (`<blobId>.<partId>`, which is what Email.attachments[].blobId carries).
 *
 * The `name` is the filename the browser will save it as; the server sanitizes
 * it before it goes anywhere near a header, and serves EVERY blob as
 * `Content-Disposition: attachment` with a type from its own allowlist — so a
 * hostile attachment cannot render as HTML on our origin no matter what this
 * client asks for. */
export function downloadUrl(
  session: JmapSession,
  accountId: AccountId,
  blobId: BlobId,
  name: string,
  type?: string | null,
): string {
  return expandTemplate(session.downloadUrl, {
    accountId,
    blobId,
    name: name || "attachment",
    type: type ?? "",
  });
}

/** The URL that serves one MIME part for INLINE rendering (#131).
 *
 * A MAILROOM EXTENSION — `previewUrl` is not an RFC 8620 session field. It is
 * advertised separately from `downloadUrl` because the two routes answer different
 * questions: that one serves anything on its allowlist as an inert download, this
 * one serves the much shorter list of things that are safe to RENDER, and refuses
 * the rest with 415.
 *
 * There is no `{type}` to pass. The server picks the type from its own allowlist
 * and will not be told otherwise, which is the same rule downloadUrl's ignored
 * `?type=` follows — stated here by the template simply not having the variable. */
export function previewUrl(
  session: JmapSession,
  accountId: AccountId,
  blobId: BlobId,
  name: string,
): string | null {
  // A server that does not advertise the route has no preview door; the caller
  // shows the download link instead of a frame pointed at nothing.
  if (!session.previewUrl) return null;
  return expandTemplate(session.previewUrl, {
    accountId,
    blobId,
    name: name || "preview",
  });
}

/** The URL that accepts a blob upload (RFC 8620 §6.1). */
export function uploadUrl(session: JmapSession, accountId: AccountId): string {
  return expandTemplate(session.uploadUrl, { accountId });
}

/** The EventSource URL (RFC 8620 §7.3). `types` is the set of JMAP types to be
 * pushed (`*` for all); `ping` keeps the connection warm through proxies that
 * kill idle streams; `closeafter=state` makes it a one-shot. */
export function eventSourceUrl(
  session: JmapSession,
  opts: { types?: readonly string[]; ping?: number; closeAfterState?: boolean } = {},
): string {
  return expandTemplate(session.eventSourceUrl, {
    types: opts.types && opts.types.length > 0 ? opts.types.join(",") : "*",
    closeafter: opts.closeAfterState ? "state" : "no",
    ping: String(opts.ping ?? 0),
  });
}

/** The RFC 8620 §6.1 upload response. */
export interface UploadResult {
  accountId: AccountId;
  blobId: BlobId;
  type: string;
  size: number;
}

/** Uploads bytes and returns the blobId that names them. The blob belongs to the
 * authenticated user; nothing else can reference it. */
export async function uploadBlob(
  doFetch: typeof fetch,
  url: string,
  body: Blob,
): Promise<UploadResult> {
  const res = await doFetch(url, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": body.type || "application/octet-stream" },
    body,
  });
  if (!res.ok) {
    throw new Error(`upload failed: HTTP ${res.status}`);
  }
  return (await res.json()) as UploadResult;
}

/** Why the server refused to serve a part, in the vocabulary of ADR-027: the
 * stored or on-demand scan verdict that blocked it. `flagged` is a claim (and may
 * carry the scanner's own detection name); `unknown` is the admission that no
 * claim could be made. Both are refused by the same door (§5b, §8). */
export type BlobBlocked = {
  verdict: "flagged" | "unknown";
  /** clamd's detection name, for `flagged` on a decrypted part; else null. */
  signature: string | null;
};

/** What a pre-flight check of a download/preview URL found. */
export type BlobProbe =
  | { kind: "ok" }
  | { kind: "blocked"; blocked: BlobBlocked }
  /** Refused or failed for some other reason (not found, not decryptable, the
   * network): not a scan verdict, and still not something to navigate into. */
  | { kind: "failed" };

/** What fetching a download URL in one request found (#804). */
export type BlobFetch =
  | { kind: "ok"; blob: Blob }
  | { kind: "blocked"; blocked: BlobBlocked }
  | { kind: "failed" };

/** True for the URL of a part of a DECRYPTED message (`<64 hex>.e.<path>`, the
 * server's decryptedPartPrefix). The verdict for such a part does not exist until
 * the server has decrypted and scanned it, and it does that on every request: a
 * check followed by the real request would cost two decryptions, two scans and
 * two audit rows (#804). So these are fetched once, or navigated to directly. */
export function isDecryptedPartUrl(url: string): boolean {
  // The FIRST path segment shaped like a blobId (64 hex, then an optional part
  // suffix) is the blobId; a later segment (the file name) never counts.
  const m = /\/[0-9a-f]{64}((?:\.[0-9a-z]+)*)(?=[/?#]|$)/i.exec(url);
  return m !== null && /^\.e\.\d+(?:\.\d+)*$/.test(m[1] ?? "");
}

/** Reads the scan refusal off a response, or null when it is not one. The server
 * answers 403 with `X-Mailroom-Attachment-Scan: flagged|unknown` (and, for a
 * flagged decrypted part, `X-Mailroom-Attachment-Signature`); a 403 with some
 * other (or no) scan header is not a verdict and is not reported as one. */
export function blockedFromResponse(res: Pick<Response, "status" | "headers">): BlobBlocked | null {
  if (res.status !== 403) return null;
  const verdict = res.headers.get("X-Mailroom-Attachment-Scan");
  if (verdict !== "flagged" && verdict !== "unknown") return null;
  const sig = res.headers.get("X-Mailroom-Attachment-Signature");
  return { verdict, signature: verdict === "flagged" && sig ? sig : null };
}
