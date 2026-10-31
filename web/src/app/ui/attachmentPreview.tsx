// SPDX-License-Identifier: AGPL-3.0-only

// LOOKING AT AN ATTACHMENT WITHOUT DOWNLOADING IT (Forgejo #131, presented as a
// lightbox in #200, as the viewer that takes the main area in #449). The threat
// model this lives under is written down once in docs/hostile-content.md — read
// §2.4a before changing anything here.
//
// WHAT IS LEFT IN THIS FILE, AFTER #449. The overlay, the navigation and the
// keyboard moved into ui/viewer/ — that is chrome, and the viewer replaced it with
// a top bar, a thumbnail strip and two held panels. What stayed is the part that
// actually touches a stranger's bytes: fetching ONE part and putting it on screen.
// It stayed here, with its comments, because moving the file that carries this
// threat model into a directory named after a widget would have quietly detached
// the reasoning from the code it constrains.
//
// WHY THIS IS A SAFETY FEATURE AND NOT A CONVENIENCE ONE. Before this, the only
// way to see what an attachment contained was to download it and open it in a
// desktop application: a hostile document handed to a full-featured local viewer
// running with the user's own privileges, which is the attack path the reading
// pane spends all its effort closing. Looking first, in something that cannot act,
// is the safer order — which is also why #200 made the CHIP ITSELF the preview
// control. A preview hidden behind a secondary button loses to the download, and
// then the safer path is the one nobody takes.
//
// TWO RENDER PATHS, AND ONLY ONE OF THEM NEEDED A SERVER DOOR.
//
// Images and text are rendered from BYTES THIS CLIENT ALREADY FETCHED through the
// ordinary download route, unchanged — `Content-Disposition: attachment`
// constrains navigation, not `fetch()`. An image becomes a `data:` URL (the
// inline-bytes case `img-src 'self' data:` already permits, the same move
// inlineImages.ts makes for cid: images) and text becomes TEXT NODES, never markup.
//
// A PDF is framed from OUR OWN preview route (internal/api/blob.go
// PreviewHandler), which is a THIRD DOOR and documented as one. It serves exactly
// one media type, and the opaque origin arrives in the RESPONSE
// (`Content-Security-Policy: default-src 'none'; sandbox`) rather than in an
// attribute here — so it holds however this frame is written. Framing a `blob:`
// URL was tried first and cannot work: a sandboxed iframe refuses blob: entirely
// (net::ERR_BLOCKED_BY_CLIENT), and an unsandboxed one gives up the origin
// boundary.
//
// WHAT HOLDS IT SHUT:
//
//  1. THE TYPE IS THE SERVER'S, NOT THE PART'S. For images and text this client
//     accepts only what the server named from blob.go's allowlist; for PDF the
//     preview route refuses anything but application/pdf with a 415. Either way
//     `application/octet-stream` is NEVER renderable — it is precisely the bucket
//     text/html, XHTML, SVG and the XML family are downgraded into.
//  2. THE PREVIEWED DOCUMENT HAS NO ORIGIN. Verified against Chromium: hostile
//     HTML served under that response CSP runs no script and cannot be reached
//     from the embedder. Drop the `sandbox` directive and the embedder CAN reach
//     it — which is why it is a response header and not a suggestion.
//  3. IT IS OPT-IN, PER ATTACHMENT. Opening a message renders no attachment and
//     fetches no part. A parser is only ever fed after a deliberate click.
//  4. AND A HELD PART NEVER GETS THIS FAR. The scan gate is checked before the
//     renderer is chosen (ui/viewer/kinds.ts viewerMode), because both routes
//     below answer 403 for a quarantined part — so a held file that reached this
//     component would render an empty frame where its explanation belongs.
//
// AND THE TRADE, STATED PLAINLY: previewing a PDF hands the bytes to the browser's
// own PDF engine (PDFium, or pdf.js in Firefox) — a large parser with a long
// history of vulnerabilities. It is not ours, it runs inside the browser's process
// sandbox, and it is strictly less exposed than the desktop viewer it replaces.
// That is the argument for it; it is not that PDF parsing is safe.

import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import type { JSX } from "react";

import { isDecryptedPartUrl } from "../../core/jmap/blobs";
import type { BlobBlocked } from "../../core/jmap/blobs";
import type { EmailBodyPart } from "../../core/jmap/types";
import { useApp } from "../state";
import { t } from "../i18n";
import { blockedMessage } from "./blobGuard";
import { blobToDataUrl } from "./inlineImages";
import {
  PREVIEW_TEXT_MAX_CHARS,
  previewKind,
  rulesFor,
} from "./attachmentPreviewPolicy";

type State =
  | { phase: "loading" }
  | { phase: "image"; src: string }
  | { phase: "text"; text: string; truncated: boolean }
  | { phase: "pdf"; src: string }
  | { phase: "unavailable"; reason: "tooBig" | "noPreview" }
  // The server refused the part (scanner verdict); say so instead of an empty frame.
  | { phase: "blocked"; blocked: BlobBlocked };

/**
 * A panel where a file would be: a sentence, and the controls that still work.
 *
 * The viewer's dead ends all take this shape — too large to render, nothing to
 * render it with, and (in ui/viewer/HeldPanel.tsx, which has its own two-weight
 * version) held. A sentence with a Download beside it, rather than a bare note:
 * every one of these cases is a file the user can still HAVE, and a panel that
 * only says "no" makes the reader close the viewer to find the way to their own
 * attachment.
 */
export function FilePanel({
  title,
  note,
  actions,
}: {
  title: string;
  note?: string;
  actions?: ReactNode;
}): JSX.Element {
  return (
    <div className="viewer-panel">
      {/* The SENTENCE is the live region, not the panel: paging onto a file nothing
          can render should announce why, and a live region that wrapped the Download
          button too would re-announce a control every time it changed. */}
      <p className="viewer-panel-title" role="status">
        {title}
      </p>
      {note ? <p className="viewer-panel-note">{note}</p> : null}
      {actions ? <div className="viewer-panel-actions">{actions}</div> : null}
    </div>
  );
}

/** Loads and renders ONE part. Owns nothing but that: the dialog, the navigation,
 * the keyboard and the held decision live in ui/viewer/, so this can be dropped
 * anywhere a single attachment needs showing.
 *
 * Remounted per attachment by the viewer (a `key` on the part's blobId), which is
 * what resets this state on navigation — cheaper and less error-prone than
 * diffing the previous part's phase against the new one. */
export function PreviewBody({
  part,
  fit = true,
  fitWidth = false,
  onImageSize,
  actions,
}: {
  part: EmailBodyPart;
  /** Images: fit the area (true) or draw at 100% (false). */
  fit?: boolean;
  /** PDF: ask the browser's own viewer to fit the page width. */
  fitWidth?: boolean;
  /** An image's intrinsic size, once the browser knows it — which is the only
   *  place it is knowable, so the top bar is told rather than asking. */
  onImageSize?: (width: number, height: number) => void;
  /** What a dead end offers instead of the file (a Download link). */
  actions?: ReactNode;
}): JSX.Element {
  const { client } = useApp();
  const [state, setState] = useState<State>({ phase: "loading" });

  // The load is an effect, not a click handler: the viewer opens already showing
  // this part, so fetching is what mounting MEANS here. `cancelled` guards the
  // async tail — page quickly through five attachments and four responses arrive
  // after their component is gone.
  useEffect(() => {
    let cancelled = false;
    const kind = previewKind(part.type);
    const blobId = part.blobId;
    if (!kind || !blobId) {
      setState({ phase: "unavailable", reason: "noPreview" });
      return;
    }
    const { allowed, maxBytes } = rulesFor(kind);
    if (part.size > maxBytes) {
      setState({ phase: "unavailable", reason: "tooBig" });
      return;
    }

    // A PDF IS NOT FETCHED. It is framed straight from the server's preview route
    // — the only route that serves anything with an inline disposition, and it
    // serves nothing but PDF. See the header comment for why the frame carries no
    // `sandbox` attribute.
    if (kind === "pdf") {
      const url = client.previewUrl(blobId, part.name ?? "preview");
      if (!url) {
        setState({ phase: "unavailable", reason: "noPreview" });
        return;
      }
      // ASKED BEFORE IT IS FRAMED (#47): a refused preview would otherwise render
      // the server's 403 inside the frame, or nothing at all. The HEAD carries the
      // reason, so the panel can say what happened.
      // A decrypted part is NOT probed (#804): the probe and the frame would each
      // decrypt and scan it. The frame goes straight to the preview route, and a
      // refusal renders there as the server's own explaining page (#805), which
      // that route makes framable by this origin.
      if (isDecryptedPartUrl(url)) {
        setState({ phase: "pdf", src: url });
        return;
      }
      void (async () => {
        const probe = await client.probeBlob(url);
        if (cancelled) return;
        if (probe.kind === "blocked") setState({ phase: "blocked", blocked: probe.blocked });
        else if (probe.kind === "failed") setState({ phase: "unavailable", reason: "noPreview" });
        else setState({ phase: "pdf", src: url });
      })();
      return () => {
        cancelled = true;
      };
    }

    void (async () => {
      let blob: Blob | null = null;
      let blocked: BlobBlocked | null = null;
      try {
        blob = await client.fetchPart(blobId, allowed, part.name ?? "preview", maxBytes, (b) => {
          blocked = b;
        });
      } catch {
        // A failed fetch is an unavailable preview, never an error thrown into the
        // reading pane: the download link in the top bar still works.
        blob = null;
      }
      if (cancelled) return;
      if (blocked) {
        setState({ phase: "blocked", blocked });
        return;
      }
      if (!blob) {
        setState({ phase: "unavailable", reason: "noPreview" });
        return;
      }
      try {
        if (kind === "text") {
          const full = await blob.text();
          if (cancelled) return;
          const truncated = full.length > PREVIEW_TEXT_MAX_CHARS;
          setState({
            phase: "text",
            text: truncated ? full.slice(0, PREVIEW_TEXT_MAX_CHARS) : full,
            truncated,
          });
          return;
        }
        // Images: a `data:` URL, NOT an object URL. The app CSP's img-src is
        // `'self' data:`, and rehydrating to data: is what lets this render without
        // adding a source to it. It also means there is no object URL to leak.
        const src = await blobToDataUrl(blob);
        if (!cancelled) setState({ phase: "image", src });
      } catch {
        if (!cancelled) setState({ phase: "unavailable", reason: "noPreview" });
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [client, part]);

  const label = part.name || part.type;

  switch (state.phase) {
    case "loading":
      return (
        <p className="viewer-note" role="status">
          {t("attachment.loading")}
        </p>
      );
    case "unavailable":
      return (
        <FilePanel
          title={state.reason === "tooBig" ? t("attachment.tooBig") : t("attachment.noPreview")}
          actions={actions}
        />
      );
    case "blocked":
      // No Download here: the same door is shut. The sentence is the live region
      // (FilePanel) and starts with the word "Blocked", so it does not rely on colour.
      return <FilePanel title={blockedMessage(state.blocked)} />;
    case "image":
      return (
        <img
          className={`viewer-image ${fit ? "is-fit" : "is-actual"}`}
          src={state.src}
          alt={label}
          // The intrinsic size, reported once the bytes are decoded. The top bar
          // prints "1600 × 1200"; nothing else can know it, because the size on
          // the part is the encoded byte count.
          onLoad={(ev) => {
            const img = ev.currentTarget;
            onImageSize?.(img.naturalWidth, img.naturalHeight);
          }}
        />
      );
    case "text":
      return (
        <>
          <pre className="viewer-text">{state.text}</pre>
          {state.truncated ? <p className="viewer-note">{t("attachment.truncated")}</p> : null}
        </>
      );
    case "pdf":
      // NO `sandbox` ATTRIBUTE, DELIBERATELY. A sandboxed frame is an opaque origin
      // to the server too, and the preview response's
      // `Cross-Origin-Resource-Policy: same-origin` then refuses the load
      // (net::ERR_BLOCKED_BY_RESPONSE). The opaque origin comes from that
      // response's own CSP `sandbox` directive, which is the stronger place for it:
      // it cannot be dropped by editing this element.
      //
      // `#view=FitH` IS A REQUEST, NOT A SETTING. PDF open parameters are a
      // convention of the embedded viewer (PDFium honours view/zoom/page; other
      // engines honour some or none), and there is no way to ask whether it took —
      // the framed document has an opaque origin, so nothing here can read into
      // it. That is also why the top bar prints no page count: the number exists
      // only inside a document this side of the boundary cannot see. Both are
      // "where the browser allows" rather than promises.
      return (
        <iframe
          className="viewer-pdf"
          title={t("attachment.previewTitle", { name: label })}
          referrerPolicy="no-referrer"
          src={fitWidth ? `${state.src}#view=FitH` : state.src}
        />
      );
  }
}
