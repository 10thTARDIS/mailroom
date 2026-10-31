// SPDX-License-Identifier: AGPL-3.0-only

// The reading pane.
//
// It renders what the server actually returns for a reading-pane Email/get
// (sync.ts asks for EMAIL_BODY_PROPERTIES + fetch*BodyValues): the body parts,
// the recipient lists, and the attachments — each one a link to the JMAP
// download URL (#85; before that endpoint existed there was nothing to link to).
//
// RENDERING HOSTILE HTML — the one genuinely dangerous thing this file does.
// The threat model and the full defence live in docs/hostile-content.md; the
// render sandbox itself lives in ./messageDocument.ts. In short:
//
//   * A plain-text body is rendered as TEXT. Nothing can escape from it.
//   * An HTML body has ALREADY been sanitized BY THE SERVER (parse → allowlist
//     filter → re-serialize; internal/api/sanitize.go): no <script>, no event
//     handlers, no <svg>/<math>, no <meta http-equiv=refresh>, no <link
//     rel=preconnect>, no <form>, and every remote image URL parked in a data-
//     attribute rather than a src.
//   * ...and it is STILL rendered inside a SANDBOXED IFRAME with an OPAQUE
//     origin and its own CSP (MESSAGE_SANDBOX / messageCSP), because the server
//     sanitizer can be wrong about one element and the browser's origin
//     isolation is what we are actually betting on.
//
// The alternative — sanitizing HTML and injecting it into our own DOM — is a bet
// that a sanitizer beats every mutation-XSS trick ever written. There is no
// dangerouslySetInnerHTML anywhere in this application, and there must not be.
//
// REMOTE CONTENT is blocked by default (spec §11). When the server reports that
// it blocked some, the user gets a banner and an explicit, per-message,
// non-persistent "Show remote content". The SSRF-guarded image proxy (ADR-026)
// has SHIPPED and is on by default (config.DefaultImageProxy = true) — with it
// on, that click routes through the server (`client.imageProxyEnabled()` below
// picks the copy that says so); with it off, the click fetches directly from the
// sender and the banner says that instead.

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import { effectiveEmail } from "../../core/store";
import type { EffectiveEmail } from "../../core/store";
import { isInJunk } from "../../core/junk";
import { formatWake } from "../../core/snooze";
import { senderIdentity, stripBidiControls } from "../../core/hostileText";
import { mailroomVerified, senderAuthNotice } from "../../core/senderAuth";
import { encryptionNotice } from "../../core/encryption";
import { SignatureLine } from "./pane/SignatureLine";
import { senderWarningsList } from "../../core/senderWarnings";
import type {
  Email,
  EmailBodyPart,
  AttachmentScan,
  AuthResults,
  Id,
  SenderWarningRaw,
} from "../../core/jmap/types";
import { useApp, useMailState } from "../state";
import { t } from "../i18n";
import {
  HEIGHT_MESSAGE_TYPE,
  MESSAGE_SANDBOX,
  blockedPixelCount,
  hasBlockedRemoteContent,
} from "./messageDocument";
import { blobToDataUrl, fetchableInlineRefs, rewriteCidImages } from "./inlineImages";
import { listedParts } from "./messageParts";
import { htmlToRender } from "./bodyChoice";
import {
  PROXIED_IMAGE_MAX_BYTES,
  failedImageCount,
  fetchAllowedImages,
  messageFrameDocument,
  proxiedImageCount,
} from "./remoteImages";
import { addressDomain, matchingRemoteAllow, normalizeAddress } from "../../core/remoteAllows";
import { previewableParts } from "./attachmentPreviewPolicy";
import { isWithheld, scanStateFor, withheldReasonKey } from "./attachmentScanPolicy";
import { releasedNote } from "./viewer/kinds";
import { useShell } from "./shellContext";
import { focusMessageListSoon } from "./focusTrap";
import { UnsubscribePanel } from "./UnsubscribePanel";
import { bytesText, rowTime } from "./desk/format";
import { MessageLabels } from "./pane/MessageLabels";
import { BlobNotice, GuardedBlobLink, useBlobGuard } from "./blobGuard";
import { InviteCard } from "./pane/InviteCard";
import { PaneToolbar } from "./pane/PaneToolbar";
import type { PaneMenuItem } from "./pane/PaneToolbar";
import { PHONE_QUERY, useMediaQuery } from "./useMediaQuery";
import { addressLine } from "./addressLabel";
import { SenderPopover } from "./pane/SenderPopover";
import { conversationIds, foldPlan } from "./pane/fold";
import { lineSideSentence } from "./pane/lineSide";

/** Hard cap on the auto-height frame (#100). A message taller than this keeps its
 * own scrollbar past the cap, so a pathological body cannot produce a mile-tall
 * frame. Kept in sync with the `max-height` backstop on `.message-html`. */
const MAX_FRAME_HEIGHT = 20000;
/** The widest natural content width the parent will lay a frame out at (#801). */
const MAX_FRAME_WIDTH = 4000;
/** Below this the text would be too small to read even pinch-zoomed in; the
 * frame keeps its own sideways scroll instead (#801). */
const MIN_FIT_SCALE = 0.3;


interface Props {
  email: EffectiveEmail;
  onClose: () => void;
  onReply: () => void;
  /** Reply all (#646) — see ui/compose/recipients.ts for who it addresses and
   * why that differs from Reply. */
  onReplyAll: () => void;
  onArchive: () => void;
  /** Forward this message (#582) — from the More menu, because Reply is the row's
   * one primary action and forwarding is the weekly verb, not the constant one. */
  onForward: () => void;
  /** "Not junk" (#528). Required of every route that opens a message, not just the
   * Junk list's: a message in Junk can be opened from a search or from the tray,
   * and the pane decides whether to offer it (core/junk.ts isInJunk), not the
   * route that happened to open it. */
  onNotJunk: () => void;
  onTrash: () => void;
  /** The IN-TRAY toggle (#375/#444): the `$flagged` keyword, which is what the
   * tray is. It used to be the star, and the star was a glyph with no word on
   * it — the desk settled that argument (desk/SelectionToolbar.tsx) and this
   * row now says what it does. */
  onTray: () => void;
  /** Pin (#51), from the More menu. */
  onPin: () => void;
  onPrint: () => void;
  onMute: () => void;
  onSnooze: () => void;
  onImportant: () => void;
  /** Open the rules editor pre-filled from this message (#155). */
  onCreateRule: () => void;
  /** Answer this message's open questions (#176): open the label menu on it, or
   * say "none of these". Both are the ordinary shell actions — the panel is a
   * place to see the question, not a second way to act on it. */
  onLabel: () => void;
  /** Open the area picker and re-file THIS message (#921), without leaving the
   * pane. Offered only when the route actually has areas to file into — a
   * permanently empty picker on a route with none would be a control that never
   * does anything, the same reason Dismiss is conditional below. */
  onRefile?: () => void;
  onDismiss: () => void;
  /**
   * Where this message sits among the rows the list is showing (#375 increment
   * 7) — ui/desk/flatten.ts `positionOf`, one-based, or null when the list does
   * not contain it.
   *
   * ONLY DRAWN ON THE PHONE, where the pane REPLACES the list (styles.css's
   * `data-pane="thread"` at 759px), so "3 of 6" is the only thing on screen
   * saying how much is left. Beside a visible list it would be a number
   * restating what the reader can see.
   */
  position?: { index: number; count: number } | null;
  /** What the back link goes back TO, in that surface's word: "Inbox" from the
   * desk, "In-tray" from the tray. */
  backLabel?: string;
}

/** One message's HTML body: the sandboxed frame, plus the remote-content banner
 * when — and only when — the server actually blocked something in it.
 *
 * `htmlParts`/`attachments` are the message's body-part objects, used ONLY to
 * resolve inline `cid:` images (Forgejo #99): each carries the server's `cid` and
 * `blobId`, so the cid→part map comes from the server's own part-id walk and is
 * never re-derived by parsing the HTML. Inline images are NOT remote content and
 * render without the gate; see ./inlineImages.ts. */
export function HtmlBody({
  html,
  title,
  htmlParts,
  attachments,
  scan,
  senderAddress,
  messageId,
}: {
  html: string;
  title: string;
  htmlParts: EmailBodyPart[] | undefined;
  attachments: EmailBodyPart[] | undefined;
  /** The message's quarantine verdicts: a held inline part is never asked for
   * (#675). */
  scan: AttachmentScan | null | undefined;
  /** The From address, for the remembered-sender check (#202). */
  senderAddress: string;
  /** The message this body belongs to, for the one-off remote-content request
   * the image proxy needs (ADR-026 §3c). */
  messageId: string;
}): JSX.Element {
  const { client, actions } = useApp();
  const { remoteAllows } = useMailState();

  // WHY THIS IS ALLOWED TO PERSIST NOW (#202). It used to be per-message, per-open,
  // and the comment said why: "I trusted this one newsletter once" must not become
  // "this sender may track me forever". That is right about a BLANKET allow, and it
  // is exactly what is still refused — what changed is that there is now somewhere
  // to record a NARROW decision, so the choice is no longer between asking every
  // single time and never asking. Asking every time about the same newsletter is
  // how a safe default becomes something a user routes around.
  //
  // The grant is per sender or per domain, listed in Settings, and revocable from
  // the banner below. The server still parks every remote URL unconditionally —
  // this only decides what to un-park (docs/hostile-content.md §2.3).
  const allows = useMemo(() => Object.values(remoteAllows), [remoteAllows]);
  const allowedBy = useMemo(
    () => matchingRemoteAllow(senderAddress, allows),
    [senderAddress, allows],
  );
  const senderDomain = addressDomain(normalizeAddress(senderAddress));

  // A per-message override, on top of the remembered decision, in BOTH directions:
  // `null` means "no override — do what the grant says", so one message can be
  // opened without images without deleting a grant, and a one-off allow still
  // records nothing.
  const [override, setOverride] = useState<boolean | null>(null);
  const allowRemote = override ?? allowedBy !== null;
  // A click here only sets the override. With the image proxy on, the server has
  // to be told the user asked for THIS message (ADR-026 §3c), and the effect
  // that fetches the images does that and WAITS for it (#667) — it used to be
  // fired from here and not awaited, so the first images could be asked for
  // before the server knew, and stayed broken until the message was reopened.
  const setAllowRemote = (v: boolean): void => {
    setOverride(v);
  };
  const blocked = hasBlockedRemoteContent(html);

  // Tracking pixels the server withheld (#55 increment 2), and the line that says
  // so — or null when there is nothing to say.
  //
  // ⚠ ONLY WHILE IMAGES ARE ACTUALLY LOADING. With remote content still blocked,
  // the beacon is one of many things not being fetched and the banner above
  // already covers it; the count would be noise. It becomes worth saying exactly
  // when the user has allowed the rest and this part is still being held back.
  const pixelCount = blockedPixelCount(html);
  const pixelLine =
    allowRemote && pixelCount > 0
      ? pixelCount === 1
        ? t("thread.pixelsBlocked.one")
        : t("thread.pixelsBlocked.many", { count: pixelCount })
      : null;

  const onAlwaysAllow = async (kind: "address" | "domain", value: string): Promise<void> => {
    // Show them immediately, then persist: the click means "load these", and making
    // the user wait on a round trip to see it happen would make the button feel
    // broken. If the write fails, actions surfaces the error and the next render
    // falls back to the grant list, which still says blocked. The override also
    // makes the loading effect ask for this message by itself, so these images do
    // not depend on the grant having been written yet.
    setOverride(true);
    await actions.allowRemoteContent(kind, value);
  };

  const onRevoke = async (id: string): Promise<void> => {
    // Drop the override too, or the pane would keep showing images for the rest of
    // this open after the user just said stop.
    setOverride(null);
    await actions.revokeRemoteContent(id);
  };

  // Content-fit auto-height (#100). The frame is opaque-origin, so the parent
  // cannot MEASURE it — a trusted, hash-pinned reporter inside the frame
  // (messageDocument.ts) postMessages its content height out, and we size the
  // frame to it. Everything about accepting that message is a trust boundary:
  //   * event.source must be THIS frame's window — not another frame, not the
  //     opener, not a message-injected postMessage from anywhere else;
  //   * the payload shape is validated (a bounded integer only);
  //   * the value is clamped, so a pathological message cannot make a mile-tall
  //     frame — past the cap the frame keeps its own scroll.
  // The reporter has no same-origin access and no network, so height is the only
  // thing it can ever tell us.
  const frameRef = useRef<HTMLIFrameElement | null>(null);
  const [height, setHeight] = useState(0);
  // Fit a wide newsletter to a phone (#801). `contentWidth` is what the reporter
  // says the document needs; `fitWidth` is the width the frame may take (the
  // card edge to edge). On a phone, when the content needs more, the frame is
  // laid out at the content's own width and a CSS `transform: scale()` shrinks it to fit — so
  // the newsletter looks the way its author drew it, only smaller, and a
  // pinch-zoom still reads the detail. Nothing about the sandbox or CSP
  // changes: the only new thing crossing the boundary is one more bounded
  // integer from the same hash-pinned reporter.
  const [contentWidth, setContentWidth] = useState(0);
  const [fitWidth, setFitWidth] = useState(0);
  const phone = useMediaQuery(PHONE_QUERY);
  useLayoutEffect(() => {
    const frame = frameRef.current;
    const box = frame?.parentElement;
    if (!frame || !box || typeof ResizeObserver !== "function") return;
    const measure = (): void => {
      const cs = getComputedStyle(box);
      const fs = getComputedStyle(frame);
      // The frame is full bleed: the box's content width less the frame's
      // (negative) side margins. Read from the CSS rather than restated.
      const w = Math.floor(
        box.clientWidth - (parseFloat(cs.paddingLeft) || 0) - (parseFloat(cs.paddingRight) || 0) -
          (parseFloat(fs.marginLeft) || 0) - (parseFloat(fs.marginRight) || 0),
      );
      setFitWidth((prev) => {
        if (prev !== w) setContentWidth(0); // the frame returns to its own width and reports again
        return w;
      });
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(box);
    return () => ro.disconnect();
  }, []);
  const scale =
    phone && fitWidth > 0 && contentWidth > fitWidth + 1 && fitWidth / contentWidth >= MIN_FIT_SCALE
      ? fitWidth / contentWidth
      : 1;
  // ⚠ A LAYOUT EFFECT, NOT A PASSIVE ONE (#463). The reporter posts its height
  // as soon as the frame's document runs (at once, on load, and on the
  // ResizeObserver's first callback). A plain text body has nothing to fetch, so
  // all three can happen straight after the commit that inserts the iframe. A
  // passive effect runs later, after paint, and a message that arrives first has
  // no listener and is gone. The frame then stays at its fallback height forever.
  // A layout effect runs synchronously inside that same commit, and a postMessage
  // is always delivered in a LATER task, so no report can arrive before the
  // listener. The e2e harness lost exactly this race: an unchanging 600px body
  // "never posted a height", while the hostile body, whose load comes later,
  // did.
  useLayoutEffect(() => {
    function onMessage(e: MessageEvent): void {
      if (e.source !== frameRef.current?.contentWindow) return;
      const data = e.data as { type?: unknown; height?: unknown; width?: unknown } | null;
      if (!data || data.type !== HEIGHT_MESSAGE_TYPE) return;
      if (typeof data.height !== "number" || !Number.isFinite(data.height)) return;
      setHeight(Math.min(Math.max(Math.ceil(data.height), 0), MAX_FRAME_HEIGHT));
      // The natural width (#801), the same bounded-integer treatment; a message
      // that does not send one (an older reporter) simply never scales.
      if (typeof data.width === "number" && Number.isFinite(data.width)) {
        setContentWidth(Math.min(Math.max(Math.ceil(data.width), 0), MAX_FRAME_WIDTH));
      }
    }
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, []);

  // The inline (cid:) parts this body actually references, resolved against the
  // server's own body-part map. An unreferenced inline part is not fetched — the
  // file list shows it instead (messageParts.ts) — and neither is a HELD one
  // (#675): its src is stripped like any unresolved cid, and its withheld chip
  // in the file list is where the reader learns about it.
  const inlineRefs = useMemo(
    () => fetchableInlineRefs(html, scan, htmlParts, attachments),
    [html, scan, htmlParts, attachments],
  );
  // A stable key so the fetch effect re-runs only when the referenced blobs
  // change, not on every render (inlineRefs is a fresh array each render).
  const fetchKey = inlineRefs.map((r) => r.blobId).join(",");

  // The body with cid: images rehydrated to data: URLs. Starts as the raw
  // (cid-bearing) html and is replaced once the inline bytes are fetched.
  //
  // ⚠ A cid: src NEVER REACHES THE FRAME, not even before the fetch lands
  // (#675). It could not load there anyway (the frame's img-src is data:), but
  // a HELD image's reference must not survive into the document at all, and a
  // body whose only cid images are held fetches nothing — so the strip cannot
  // wait for a fetch that never happens. Every cid: src starts stripped and is
  // put back, as a data: URL, only once its bytes are in.
  const [resolvedHtml, setResolvedHtml] = useState(() => rewriteCidImages(html, new Map()));
  useEffect(() => {
    if (inlineRefs.length === 0) {
      setResolvedHtml(rewriteCidImages(html, new Map()));
      return;
    }
    let cancelled = false;
    void (async () => {
      const resolved = new Map<string, string>();
      await Promise.all(
        inlineRefs.map(async (ref) => {
          try {
            const blob = await client.fetchInlineImage(ref.blobId);
            if (blob) resolved.set(ref.cid, await blobToDataUrl(blob));
          } catch {
            // Leave it unresolved: rewriteCidImages strips the dangling src.
          }
        }),
      );
      if (!cancelled) setResolvedHtml(rewriteCidImages(html, resolved));
    })();
    return () => {
      cancelled = true;
    };
    // inlineRefs is captured; fetchKey is its stable identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [html, fetchKey, client]);

  // PROXIED REMOTE IMAGES (#667; ./remoteImages.ts has the why). With the image
  // proxy on, the app fetches each allowed image itself and inlines it as a
  // `data:` URL, as it does for cid: images above. The frame cannot fetch them:
  // its origin is opaque, so the session cookie that gates the proxy is withheld.
  //
  // Keyed on `html`, the body as the server sent it, and not on `resolvedHtml`:
  // the proxy paths are the same in both, and keying on the latter would fetch
  // every image a second time when the cid: images arrive.
  const proxied = client.imageProxyEnabled();
  const [remoteImages, setRemoteImages] = useState<{
    forHtml: string;
    resolved: Map<string, string>;
  } | null>(null);
  useEffect(() => {
    if (!proxied || !allowRemote) {
      setRemoteImages(null);
      return;
    }
    let cancelled = false;
    void (async () => {
      // A click on this message (one-off or "always allow") is authorised
      // server-side by a request that must land BEFORE the first image is asked
      // for; a remembered grant needs none. fetchAllowedImages holds the order.
      const resolved = await fetchAllowedImages(html, {
        askFirst: override === true,
        ask: () => actions.requestRemoteContent(messageId),
        fetchOne: async (path) => {
          const blob = await client.fetchProxiedImage(path, PROXIED_IMAGE_MAX_BYTES);
          return blob ? { dataUrl: await blobToDataUrl(blob), bytes: blob.size } : null;
        },
      });
      if (!cancelled) setRemoteImages({ forHtml: html, resolved });
    })();
    return () => {
      cancelled = true;
    };
  }, [html, proxied, allowRemote, override, messageId, client, actions]);

  // What the frame is given (messageFrameDocument has the rule: with the proxy
  // on, always the blocked policy, because every image is `data:` by then).
  const remoteReady = remoteImages !== null && remoteImages.forHtml === html;
  const frameDocument = useMemo(
    () =>
      messageFrameDocument(resolvedHtml, {
        proxied,
        allowRemote,
        resolved: remoteReady ? remoteImages.resolved : null,
        origin: window.location.origin,
      }),
    [proxied, resolvedHtml, allowRemote, remoteReady, remoteImages],
  );

  // What the banner says about them: nothing while they are on their way, and
  // the count that did not arrive once they are in. "Images are loading" was a
  // claim made whether or not a single one had.
  const remoteTotal = proxied && allowRemote ? proxiedImageCount(html) : 0;
  const remoteFailed =
    proxied && allowRemote && remoteReady ? failedImageCount(html, remoteImages.resolved) : 0;
  const remoteLoading = proxied && allowRemote && !remoteReady && remoteTotal > 0;
  const imagesLine = remoteLoading
    ? t("thread.imagesLoading")
    : remoteFailed > 0
      ? remoteFailed === remoteTotal
        ? t("thread.imagesFailed.all")
        : t("thread.imagesFailed.some", { failed: remoteFailed, total: remoteTotal })
      : null;

  return (
    <>
      {blocked && !allowRemote ? (
        <div className="remote-banner" role="status">
          {/* ⚠ TWO DIFFERENT BARGAINS, TWO DIFFERENT SENTENCES. Without the
              proxy, loading remote images hands the sender your address and your
              browser. With it, your own server fetches them and the sender sees
              the server. Both still tell the sender the message was OPENED —
              only a cache changes that (ADR-026 §1) — so the proxied line says
              what it does rather than claiming more. */}
          <span>
            {client.imageProxyEnabled()
              ? t("thread.remoteBlockedProxied")
              : t("thread.remoteBlocked")}
          </span>
          <button type="button" className="button-quiet" onClick={() => setAllowRemote(true)}>
            {t("thread.showRemote")}
          </button>
          {/* Remembering it (#202). Two buttons rather than one because the
              annoyance has two shapes — a person is an address, a publisher is a
              domain — and the DOMAIN one says out loud that it is broader, here,
              where the decision is made. A settings page nobody reads is not where
              that belongs. */}
          {senderAddress ? (
            <button
              type="button"
              className="button-quiet"
              onClick={() => void onAlwaysAllow("address", senderAddress)}
            >
              {t("thread.alwaysAllowSender")}
            </button>
          ) : null}
          {senderDomain ? (
            <button
              type="button"
              className="button-quiet"
              title={t("thread.alwaysAllowDomainHint", { domain: senderDomain })}
              onClick={() => void onAlwaysAllow("domain", senderDomain)}
            >
              {t("thread.alwaysAllowDomain", { domain: senderDomain })}
            </button>
          ) : null}
        </div>
      ) : null}

      {/* When images loaded because of a REMEMBERED decision, say so. A pane that
          silently behaves differently for one sender than another erodes trust in
          the default — and this line is also the fastest route to undoing it. */}
      {blocked && allowRemote && allowedBy ? (
        <div className="remote-banner remote-banner-quiet" role="status">
          <span>
            {allowedBy.kind === "domain"
              ? t("thread.remoteAllowedDomain", { domain: allowedBy.value })
              : t("thread.remoteAllowedSender", { sender: allowedBy.value })}
          </span>
          <button
            type="button"
            className="button-quiet"
            onClick={() => void onRevoke(allowedBy.id)}
          >
            {t("thread.stopAllowing")}
          </button>
          {imagesLine ? <span className="remote-banner-aside">{imagesLine}</span> : null}
          {pixelLine ? <span className="remote-banner-aside">{pixelLine}</span> : null}
        </div>
      ) : null}

      {/* The same line when images were loaded by a CLICK rather than by a
          remembered decision — there is no banner above to hang it on then, and
          the reader has just as much reason to be told. */}
      {(pixelLine || imagesLine) && !(blocked && allowRemote && allowedBy) ? (
        <div className="remote-banner remote-banner-quiet" role="status">
          {imagesLine ? <span>{imagesLine}</span> : null}
          {pixelLine ? <span>{pixelLine}</span> : null}
        </div>
      ) : null}
      {/* The frame sits in a plain box so a scaled one can be given the room it
          takes up on screen: a transform shrinks what is drawn, not the space
          the frame occupies, so the box is sized to the scaled frame (#801).
          Unscaled, the box has no style and the frame lays out exactly as it did
          before. */}
      <div
        className="message-html-box"
        style={
          scale < 1
            ? {
                width: "calc(100% + 32px)",
                margin: "10px -16px 0",
                height: `${Math.ceil((height + 2) * scale)}px`,
                overflow: "hidden",
              }
            : undefined
        }
      >
        <iframe
          ref={frameRef}
          className="message-html"
          title={title}
          sandbox={MESSAGE_SANDBOX}
          referrerPolicy="no-referrer"
          srcDoc={frameDocument}
          style={{
            ...(height > 0 ? { height: `${height}px` } : null),
            ...(scale < 1
              ? {
                  width: `${contentWidth}px`,
                  margin: 0,
                  transform: `scale(${scale})`,
                  transformOrigin: "0 0",
                }
              : null),
          }}
        />
      </div>
    </>
  );
}

/** The decoded text of the first body part that has a value. */
function bodyText(email: Email | EffectiveEmail, parts: EmailBodyPart[] | undefined): string {
  const values = (email as Email).bodyValues ?? {};
  return (parts ?? [])
    .map((p) => (p.partId ? values[p.partId]?.value ?? "" : ""))
    .filter(Boolean)
    .join("\n\n");
}

/**
 * One WITHHELD attachment: the malware scanner refused it, and the download
 * route will 403 it too (#396/#412, ADR-027 §8).
 *
 * STILL NEITHER A PREVIEW LINK NOR A DOWNLOAD LINK — both would 403, and a chip
 * that looks clickable but silently fails is worse than one that says plainly
 * what is going on. The filename and size are shown for the same reason a
 * rejected package still gets a shipping label: knowing WHAT was withheld is
 * not the same as being handed it.
 *
 * WHAT #449 ADDED: the name IS a control, and it opens the viewer — onto the
 * explanation, never onto the file (ui/viewer/kinds.ts checks the scan gate
 * before it chooses a renderer). Without it, a message whose only attachment is
 * held could not reach the two-weight held panel at all, and the panel this
 * issue exists for would be unreachable except by paging from a sibling.
 *
 * The confirmation is a SEPARATE step (`onRequestRelease`), not fired from
 * here, because the wording that step needs (ADR-027 §14: state the
 * consequence, never "are you sure?") belongs with the button that actually
 * releases something, not scattered across every chip that offers one.
 */
function WithheldAttachmentChip({
  label,
  size,
  reasonKey,
  onOpen,
  onRequestRelease,
}: {
  label: string;
  size: JSX.Element;
  reasonKey: ReturnType<typeof withheldReasonKey>;
  onOpen: (ev: React.MouseEvent<HTMLElement>) => void;
  onRequestRelease: () => void;
}): JSX.Element {
  return (
    <span className="attachment attachment-withheld" role="group" aria-label={t("attachment.withheldLabel", { name: label })}>
      <button
        type="button"
        className="attachment-withheld-open"
        aria-haspopup="dialog"
        title={t("viewer.open", { name: label })}
        onClick={onOpen}
      >
        <span className="attachment-withheld-icon" aria-hidden="true">
          🚫
        </span>{" "}
        <span className="attachment-name">{label}</span> {size}
      </button>
      <span className="attachment-withheld-state">{t("attachment.withheldState")}</span>
      <span className="attachment-withheld-reason">{t(reasonKey)}</span>
      <button type="button" className="button-quiet" onClick={onRequestRelease}>
        {t("attachment.release")}
      </button>
    </span>
  );
}

/**
 * The release confirmation (#412, ADR-027 §14).
 *
 * ⚠ THE WORDING MUST NOT REASSURE. This gates a real security control, not a
 * "delete this?" prompt someone can click through on muscle memory — so the
 * body states the consequence plainly (what releasing DOES: opens the download
 * gate for this content, on every message that carries it, permanently and with
 * no undo in this client — NOT that Mailroom stops checking it; the verdict
 * keeps being refreshed by re-scan, the release just keeps overriding it
 * regardless of what a later scan finds) instead of asking a yes/no question
 * that invites a reflexive "yes". The buttons say what they do rather than
 * "OK"/"Cancel" for the same reason.
 *
 * A bespoke dialog rather than `window.confirm` (used elsewhere in this app for
 * plain deletes): the two buttons need their own asymmetric wording, which a
 * native confirm cannot offer.
 */
function ReleaseConfirmDialog({
  name,
  busy,
  onCancel,
  onConfirm,
}: {
  name: string;
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}): JSX.Element {
  const cancelRef = useRef<HTMLButtonElement | null>(null);
  useEffect(() => {
    cancelRef.current?.focus();
  }, []);
  return (
    <div className="modal-backdrop" onKeyDown={(e) => e.key === "Escape" && onCancel()}>
      <div
        className="sheet"
        role="alertdialog"
        aria-modal="true"
        aria-label={t("attachment.releaseConfirmTitle", { name })}
      >
        <header className="sheet-header">
          <h2>{t("attachment.releaseConfirmTitle", { name })}</h2>
        </header>
        <div className="sheet-body">
          <p>{t("attachment.releaseConfirmBody")}</p>
          <div className="sheet-actions">
            <button type="button" className="button" onClick={onCancel} ref={cancelRef}>
              {t("attachment.keepWithheld")}
            </button>
            <button type="button" className="button button-danger" disabled={busy} onClick={onConfirm}>
              {busy ? t("attachment.releasing") : t("attachment.releaseAnyway")}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

/**
 * One message's attachments, and the viewer they open into (#131, reshaped in
 * #200, and again in #449 where the lightbox became a surface that takes the main
 * area; the withheld chip added in #412).
 *
 * CLICKING THE CHIP OPENS IT. It used to download, with preview demoted to a
 * smaller control beside it — which meant the primary action was still the
 * round-trip to a desktop viewer that #131 existed to avoid. A safer path that is
 * harder to reach is a safer path nobody takes, so the chip IS the viewer now and
 * downloading is the secondary control (⤓ beside it, and again in the viewer's top
 * bar).
 *
 * A part with NO preview keeps a download link as its chip, so a chip never lies
 * about what clicking it does. That is still true and it is now the SMALLER half of
 * the story: the viewer's strip lists every attachment on the message, so paging can
 * land on that .xlsx, and what it lands on is a panel with a Download in it rather
 * than a dead end. A WITHHELD part opens the viewer too, onto its explanation.
 *
 * THE OPEN VIEWER DOES NOT LIVE HERE, and that is the change from the lightbox: it
 * takes a column of the workspace grid, so it is the shell that renders it
 * (ui/shellContext.ts ViewerRequest says why). What lives here is which attachment
 * to open on, and the tile to give focus back to when it closes.
 */
export function AttachmentList({
  messageId,
  threadId,
  attachments,
  scan,
}: {
  messageId: string;
  /** For the refresh after a release (#412): the server's decision can affect
   * every message sharing these bytes, and `sync.loadThread` is what re-asks
   * `Email/get` with `attachmentScan` for the whole open thread. */
  threadId: string;
  attachments: EmailBodyPart[];
  scan: AttachmentScan | null | undefined;
}) {
  const { client, actions } = useApp();
  const shell = useShell();
  // A download the server refuses (the scanner flagged it, or could not check
  // it) is explained here rather than navigated to as a bare 403 (#47).
  const guard = useBlobGuard();
  // The part awaiting confirmation, or null. One at a time — nothing here needs
  // more than a single pending decision.
  const [confirming, setConfirming] = useState<{ partId: string; label: string } | null>(null);
  const [releasing, setReleasing] = useState(false);

  /**
   * THE TILE TO PUT FOCUS BACK ON when the viewer closes (#447), and a counter that
   * says "now".
   *
   * ⚠ THE focus() MUST NOT HAPPEN IN THE CLOSE HANDLER. In the pane placement the
   * reading pane is `display: none` while the viewer holds its column (app/viewer.css),
   * and an element that is not displayed cannot take focus — so focusing synchronously
   * from the shell's close callback would silently drop the user at the top of the
   * document. The bump schedules it instead: React commits the shell's state (the pane
   * is on screen again) and only then runs this effect.
   */
  const openerRef = useRef<HTMLElement | null>(null);
  const [returnFocus, setReturnFocus] = useState(0);
  useEffect(() => {
    if (returnFocus === 0) return;
    openerRef.current?.focus();
  }, [returnFocus]);

  /** Opens the viewer on ONE attachment. `index` is a position in this message's
   *  `attachments` — the same array the viewer reads back out of the store, never
   *  a position in the previewable subset. */
  const openViewer = (index: number, ev: React.MouseEvent<HTMLElement>): void => {
    openerRef.current = ev.currentTarget;
    shell.openViewer({
      messageId,
      threadId,
      index,
      onClosed: () => setReturnFocus((n) => n + 1),
    });
  };

  // WHICH TILES ARE OPENABLE-AS-A-PREVIEW, from the one helper, so a chip cannot
  // claim a preview the viewer would not produce. The viewer itself pages through
  // EVERYTHING (see the note above); this set is only about what a chip promises.
  const previewable = useMemo(
    () => new Set(previewableParts(attachments).map((p) => p.blobId)),
    [attachments],
  );

  const confirmRelease = async (): Promise<void> => {
    if (!confirming) return;
    setReleasing(true);
    // Closed either way: on success the refreshed thread re-renders the chip as
    // ordinary; on failure actions.releaseAttachment has already surfaced the
    // error toast, and the chip stays withheld because nothing changed.
    await actions.releaseAttachment(threadId, messageId, confirming.partId);
    setReleasing(false);
    setConfirming(null);
  };

  return (
    <>
      <ul className="attachments" aria-label={t("thread.attachments")}>
        {attachments.map((at, index) => {
          if (!at.blobId) return null;
          // An attachment filename is sender-authored, so it gets the same
          // treatment as every other piece of hostile text we render. Both
          // halves matter and they are different harms: `label` is what our own
          // chrome shows, while `dlName` is what lands on the user's DISK —
          // where "report<RLO>gnp.exe" is saved under a name the file manager
          // draws as "reportexe.png". The server sanitizes the
          // Content-Disposition independently (internal/api/blob.go); neither
          // side is sufficient alone.
          const label = stripBidiControls(at.name) || stripBidiControls(at.type);
          const dlName = stripBidiControls(at.name) || "attachment";
          const size = <span className="attachment-size">{bytesText(at.size)}</span>;

          // ⚠ CHECKED BEFORE THE PREVIEW RING, AND EXCLUSIVE OF IT. A withheld
          // part must never fall through to a preview or download link — both
          // 403 — so this branch returns before either of those is considered.
          if (at.partId && isWithheld(scan, at.partId)) {
            const partId = at.partId;
            const reasonKey = withheldReasonKey(scanStateFor(scan, partId)?.state);
            return (
              <li key={at.blobId}>
                <WithheldAttachmentChip
                  label={label}
                  size={size}
                  reasonKey={reasonKey}
                  onOpen={(ev) => openViewer(index, ev)}
                  onRequestRelease={() => setConfirming({ partId, label })}
                />
              </li>
            );
          }

          // THE MARK A RELEASE LEAVES (#449). A released part stops being withheld
          // and renders like any other file, so without this the only trace of a
          // decision about malware would be the absence of a chip. Read from the
          // same `releasedAt` the viewer prints, so the two cannot disagree about
          // the date.
          const released = releasedNote(scanStateFor(scan, at.partId));
          return (
            <li key={at.blobId}>
              {!previewable.has(at.blobId) ? (
                // Nothing here can render it: the chip is the download, and says so.
                <GuardedBlobLink
                  guard={guard}
                  className="attachment"
                  href={client.blobUrl(at.blobId, dlName, at.type)}
                  download={dlName}
                  title={t("attachment.noPreview")}
                >
                  📎 <span className="attachment-name">{label}</span> {size}
                </GuardedBlobLink>
              ) : (
                <>
                  <button
                    type="button"
                    className="attachment attachment-openable"
                    aria-haspopup="dialog"
                    title={t("viewer.open", { name: label })}
                    onClick={(ev) => openViewer(index, ev)}
                  >
                    📎 <span className="attachment-name">{label}</span> {size}
                  </button>
                  <GuardedBlobLink
                    guard={guard}
                    className="attachment-download"
                    href={client.blobUrl(at.blobId, dlName, at.type)}
                    download={dlName}
                    ariaLabel={t("attachment.downloadNamed", { name: label })}
                    title={t("attachment.download")}
                  >
                    ⤓
                  </GuardedBlobLink>
                </>
              )}
              {released ? <span className="attachment-released">{released}</span> : null}
            </li>
          );
        })}
      </ul>
      <BlobNotice notice={guard.notice} />

      {confirming ? (
        <ReleaseConfirmDialog
          name={confirming.label}
          busy={releasing}
          onCancel={() => setConfirming(null)}
          onConfirm={() => void confirmRelease()}
        />
      ) : null}

    </>
  );
}

/**
 * Why this message ranked where it did, and the way to disagree (#154).
 *
 * The score is shown with its reasons rather than alone: a bare number invites
 * either blind trust or blind dismissal, and the model is a per-user estimate
 * that will be wrong sometimes. The reasons come from the server already
 * humanised (`ml.FeatureLabel`), so this renders them rather than interpreting
 * feature keys it would have to keep in sync.
 *
 * Collapsed by default — it is diagnostic, not something to read every time.
 */
/**
 * "Labelled by the rule X" (#155).
 *
 * Only AUTOMATED labels are announced. A label the user applied needs no
 * explanation — they remember — and captioning every one of those would bury the
 * single line that actually answers "why is this here?".
 *
 * A rule that has been deleted still says a rule did it. The attribution is
 * dropped when the rule goes (ON DELETE SET NULL), and "an automation you no
 * longer have" is the truth; claiming the user did it would be the lie this
 * feature was built to stop telling.
 */
/**
 * What SPF, DKIM and DMARC said about one message — shown only when that is worth
 * saying (#55 increment 1).
 *
 * ⚠ THERE IS NO "VERIFIED" COUNTERPART, AND ITS ABSENCE IS THE DESIGN. Increment 0
 * measured the corpus this renders into: nearly all messages pass all three
 * methods, so a reassuring badge would appear on 99.4% of mail. A mark that common
 * stops being read, which makes it least visible exactly when it disappears — the
 * one moment it would have mattered. Silence is therefore the signal for good mail,
 * and this element exists only for the ~0.8% with something wrong.
 *
 * The judgement about WHICH results are worth a notice lives in core/senderAuth.ts,
 * away from the JSX and under test, because it is the part that can be wrong in a
 * way nobody would see.
 */
function SenderAuthLine({
  auth,
  sender,
}: {
  auth: AuthResults | null | undefined;
  sender: string;
}): JSX.Element | null {
  const verified = mailroomVerified(auth);
  if (verified) {
    const domain = stripBidiControls(verified.domain);
    return (
      <p
        className="auth-verified"
        role="status"
        title={t("thread.auth.verifiedByMailroomHint", { domain })}
      >
        {t("thread.auth.verifiedByMailroom", { domain })}
      </p>
    );
  }
  const notice = senderAuthNotice(auth);
  if (!notice) return null;
  const methods = notice.methods.map((m) => t(`thread.auth.method.${m}`)).join(", ");
  return (
    <p className={`auth-notice auth-notice-${notice.severity}`} role="status">
      {/* The sender is attacker-controlled text and is being quoted back inside a
          warning ABOUT that attacker, which is the worst place to let a bidi
          override loose — it could reorder the sentence around it. Stripped, as
          everywhere else this pane echoes a sender (#307). */}
      {t(`thread.auth.${notice.severity}`, { sender: stripBidiControls(sender), methods })}
    </p>
  );
}

/**
 * Whether this message's SENDING DOMAIN is trying to look like one the reader
 * already knows — a lookalike, a punycode label, or a stranger claiming one of
 * the reader's own domains (#397).
 *
 * ⚠ A DIFFERENT QUESTION FROM SenderAuthLine, ON PURPOSE, AND SHOWN ALONGSIDE
 * IT RATHER THAN INSTEAD OF IT. A lookalike domain authenticates PERFECTLY — it
 * belongs to the attacker, who configured SPF and DKIM for it like anyone else
 * — so a clean SenderAuthLine says nothing about whether this line should also
 * be silent. What counts as worth showing, and the order, lives in
 * core/senderWarnings.ts, under test, for the same reason senderAuthNotice does.
 */
function SenderWarningLines({
  warnings,
}: {
  warnings: SenderWarningRaw[] | null | undefined;
}): JSX.Element | null {
  const list = senderWarningsList(warnings);
  if (list.length === 0) return null;
  return (
    <>
      {list.map((w) => {
        // The domain (and, for a lookalike, the domain it resembles) is
        // attacker-controlled text being quoted back inside a warning ABOUT
        // that attacker — the worst place to let a bidi override loose, since
        // it could reorder the sentence around it. Stripped, as everywhere
        // else this pane echoes a sender (#307).
        const domain = stripBidiControls(w.domain);
        const text =
          w.kind === "selfSpoof"
            ? t("thread.senderWarning.selfSpoof", { domain })
            : w.kind === "multipleFrom"
              ? t("thread.senderWarning.multipleFrom")
              : w.kind === "lookalikeDomain"
              ? t("thread.senderWarning.lookalikeDomain", {
                  domain,
                  resembles: stripBidiControls(w.resembles ?? ""),
                })
              : w.unicode
                ? t("thread.senderWarning.punycodeDomain", {
                    domain,
                    unicode: stripBidiControls(w.unicode),
                  })
                : t("thread.senderWarning.punycodeDomainOpaque", { domain });
        return (
          <p key={w.kind} className={`sender-warning sender-warning-${w.kind}`} role="status">
            {text}
          </p>
        );
      })}
    </>
  );
}

/**
 * Encrypted mail (#47 6b, ADR-037): "decrypted to show it, never stored", or
 * "could not be decrypted". Silent for mail that is not encrypted. The words
 * and the decision live in core/encryption.ts, under test.
 */
function EncryptionLine({ email }: { email: EffectiveEmail }): JSX.Element | null {
  const notice = encryptionNotice(email);
  if (!notice) return null;
  if (notice.kind === "failed") {
    return (
      <p className="encryption-line encryption-line-failed" role="status">
        {t("thread.encryption.failed")}
      </p>
    );
  }
  return (
    <p className="encryption-line" role="status">
      <strong>{t("thread.encryption.decrypted")}</strong>
    </p>
  );
}

function LabelProvenanceNote({ email }: { email: EffectiveEmail }): JSX.Element | null {
  const automated = (email.labelProvenance ?? []).filter((p) => p.appliedBy !== "user");
  if (automated.length === 0) return null;
  return (
    <ul className="label-provenance">
      {automated.map((p) => (
        <li key={p.keyword}>
          {p.appliedBy === "rule"
            ? p.rule
              ? t("rules.appliedBy.rule", { name: p.rule.name })
              : t("rules.appliedBy.deletedRule")
            : p.appliedBy === "agent"
              ? t("rules.appliedBy.agent")
              : t("rules.appliedBy.ml")}
        </li>
      ))}
    </ul>
  );
}

/**
 * The why-here strip. It STAYS (#375 increment 5) — the sender popover shows the
 * same reasons, and the design retires this only after a reader has seen a
 * correspondent twice, which needs a per-correspondent read count that does not
 * exist server-side (see pane/SenderPopover.tsx for the whole note).
 *
 * What DID leave is the "Mark as important" button that used to sit here: the
 * verb now lives once, in the pane's More menu, because two controls for one
 * toggle inside one pane is two things that can disagree on screen. The strip
 * explains a ranking; the action row acts.
 */
function PriorityPanel({ email }: { email: EffectiveEmail }): JSX.Element | null {
  const [open, setOpen] = useState(false);
  const score = email.priorityScore;
  const reasons = email.priorityReasons ?? [];
  // Email.lineSide (#464): the user's own placement, independent of whether
  // the model has ever scored this message — shown whenever there is one,
  // never derived from `score` or `onSurface`. See pane/lineSide.ts.
  const lineSideKey = lineSideSentence(email.lineSide);

  // Never scored: say so plainly rather than implying a verdict of "unimportant".
  const unscored = score === null || score === undefined;

  return (
    <div className="priority-panel">
      <div className="priority-summary">
        {!unscored ? (
          <button
            type="button"
            className="button button-quiet"
            aria-expanded={open}
            onClick={() => setOpen((v) => !v)}
          >
            {t("priority.why")}
          </button>
        ) : (
          <span className="hint">{t("priority.unscored")}</span>
        )}
        {lineSideKey ? <span className="hint">{t(lineSideKey)}</span> : null}
      </div>
      {open && !unscored ? (
        <div className="priority-reasons">
          <p className="priority-score">
            {t("priority.whyTitle", { score: (score as number).toFixed(2) })}
          </p>
          {reasons.length === 0 ? (
            <p className="hint">{t("priority.learning")}</p>
          ) : (
            <ul>
              {reasons.map((r) => (
                <li key={`${r.label}:${r.weight}`} className={`priority-reason is-${r.polarity}`}>
                  <span className="priority-reason-label">{r.label}</span>
                  <span className="priority-reason-dir">
                    {r.polarity === "up" ? t("priority.reasonUp") : t("priority.reasonDown")}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
      ) : null}
    </div>
  );
}

/**
 * "We are not sure what this is" — the classifier's open questions about the
 * message on screen (#176), with the reasoning behind each one.
 *
 * Its own panel rather than a row in the label list, because a suggestion is NOT
 * a weak label: nothing here has been applied, and showing it among the applied
 * labels would make the two indistinguishable at a glance — which is the exact
 * confusion the whole suggest-don't-apply design exists to prevent.
 *
 * The reasons are OPEN by default, unlike the priority panel's. A priority score
 * explains a ranking the user can read past; this explains a question they are
 * being asked, and hiding the reasoning behind a click makes "just accept it"
 * the cheapest path.
 */
function SuggestionPanel({
  email,
  onLabel,
  onDismiss,
}: {
  email: EffectiveEmail;
  onLabel: () => void;
  onDismiss: () => void;
}): JSX.Element | null {
  const state = useMailState();
  if (email.suggestions.length === 0) return null;
  return (
    <div className="suggestion-panel">
      <p className="suggestion-question">{t("queue.thread.question")}</p>
      <ul className="suggestion-list">
        {email.suggestions.map((s) => {
          const label = state.labels[s.labelId];
          // null reasons means the model left NO RECORD of why — distinct from
          // an empty list, which means it looked and had nothing to say. Saying
          // "no reasons" for the first would be a claim the server never made.
          const reasons = s.reasons;
          return (
            <li className="suggestion-item" key={s.labelId}>
              <span className="suggestion-name">{label?.name ?? t("queue.unknownLabel")}</span>
              <span className="suggestion-confidence">
                {t("queue.confidence", { percent: Math.round(s.confidence * 100) })}
              </span>
              {reasons === null ? (
                <span className="hint">{t("queue.noReasonRecorded")}</span>
              ) : reasons.length === 0 ? (
                <span className="hint">{t("queue.noReasons")}</span>
              ) : (
                <ul className="suggestion-reason-list">
                  {reasons.map((r) => (
                    <li key={`${r.label}:${r.weight}`} className={`priority-reason is-${r.polarity}`}>
                      <span className="priority-reason-label">{r.label}</span>
                      <span className="priority-reason-dir">
                        {r.polarity === "up" ? t("queue.reasonFor") : t("queue.reasonAgainst")}
                      </span>
                    </li>
                  ))}
                </ul>
              )}
            </li>
          );
        })}
      </ul>
      <div className="suggestion-actions">
        <button type="button" className="button" onClick={onLabel}>
          {t("action.label")}
        </button>
        <button type="button" className="button button-quiet" onClick={onDismiss}>
          {t("queue.dismissOne")}
        </button>
      </div>
    </div>
  );
}

export function ThreadPane({
  email,
  onClose,
  onReply,
  onReplyAll,
  onForward,
  onArchive,
  onNotJunk,
  onTrash,
  onTray,
  onPin,
  onPrint,
  onMute,
  onSnooze,
  onImportant,
  onCreateRule,
  onLabel,
  onRefile,
  onDismiss,
  position = null,
  backLabel,
}: Props): JSX.Element {
  const state = useMailState();
  const { sync, client } = useApp();
  /** On the phone the toolbar docks to the bottom at the thumb and carries four
   * buttons; Mute MOVES INTO the More menu rather than being hidden, which is a
   * different set of children and therefore not something CSS can do
   * (ui/useMediaQuery.ts). */
  const phone = useMediaQuery(PHONE_QUERY);
  const shell = useShell();

  useEffect(() => {
    // email.id as well: when the conversation leaves this message out (it is in
    // Trash, #630), loadThread fetches it by id so its body still arrives.
    if (email.threadId) void sync.loadThread(email.threadId, email.id, { open: true });
  }, [email.threadId, email.id, sync]);

  /**
   * FOCUSED READING TAKES THE MAIN AREA, SO IT HAS TO TAKE THE FOCUS (#447).
   *
   * With `messageOpens: "focused"` (or on the phone, where there is only ever one
   * pane) the stylesheet gives the list `display: none` while a message is open.
   * That is not a cosmetic detail: the desk's grid is what had focus when the
   * reader pressed Enter, and an element that becomes `display: none` while
   * focused hands focus to `<body>` — where the grid's arrows stop answering, the
   * `aria-activedescendant` still names a row nothing is focused on, and a screen
   * reader has lost its place in the document. So focus moves into the pane, and
   * comes back to the grid when the pane goes (ui/focusTrap.ts, which also
   * explains the deferral and why it refuses to steal focus from a surface that
   * has since taken it).
   *
   * ⚠ AND IT IS NOT A FOCUS TRAP, which the issue's list asks for and which would
   * be wrong here. A trap is for a surface with something LIVE hidden behind it —
   * the attachment viewer keeps the list beside it, the composer covers it
   * opaquely without hiding it (app/compose.css says why: a hidden scroll
   * container loses its position). In focused reading the list is genuinely
   * `display: none` and therefore not in the tab order at all, so there is nothing
   * to trap focus away from; what IS still on screen is the rail, the search box
   * and the header, and trapping would cut the reader off from all three while
   * claiming a modality this pane does not have.
   */
  const takesMainArea = shell.paneLayout === "off" || phone;
  const paneRef = useRef<HTMLElement | null>(null);
  useEffect(() => {
    if (!takesMainArea) return;
    paneRef.current?.focus();
    return () => focusMessageListSoon();
  }, [takesMainArea]);

  const thread = state.threads[email.threadId];
  const muted = thread?.isMuted ?? false;
  const messages = useMemo(() => {
    const ids = conversationIds(thread?.emailIds, email.id);
    return ids
      .map((id) => effectiveEmail(state, id))
      .filter((e): e is EffectiveEmail => !!e)
      .sort((a, b) => a.receivedAt.localeCompare(b.receivedAt));
  }, [thread, state, email.id]);

  // THE CONVERSATION (#375 increment 5). `foldPlan` decides the INITIAL shape —
  // the opened message and every unread one expanded, the read remainder folded
  // to a line each — and this set holds the ones the reader has since clicked
  // open. Two pieces of state rather than one because the plan has to be
  // recomputed as messages arrive and are read, while a message the reader
  // deliberately opened must stay open across that.
  //
  // ⚠ EXPANDING IS NOT READING. Clicking a folded line marks nothing: reading
  // state is whatever the existing behaviour makes it, and a fold gesture that
  // silently cleared unread would make the fold itself destructive.
  const [alsoExpanded, setAlsoExpanded] = useState<Set<Id>>(() => new Set());
  useEffect(() => {
    // A different message (or a different thread) is a different conversation:
    // carrying the old one's manual expansions across would open messages the
    // reader never asked about.
    setAlsoExpanded(new Set());
  }, [email.id, email.threadId]);
  const plan = useMemo(() => foldPlan(messages, email.id), [messages, email.id]);
  const now = Date.now();

  const menu: PaneMenuItem[] = [
    // First: it is the verb a reader reaches for after Reply, and until #582 the
    // only way to reach the forward composer at all was the attachment viewer —
    // so a message with no attachment could not be forwarded.
    { key: "forward", label: t("action.forward"), onSelect: onForward },
    { key: "trash", label: t("action.trash"), onSelect: onTrash },
    {
      key: "pin",
      label: email.isPinned ? t("action.unpin") : t("action.pin"),
      onSelect: onPin,
      pressed: email.isPinned,
    },
    {
      key: "important",
      label: email.isImportant ? t("priority.markNotImportant") : t("priority.markImportant"),
      onSelect: onImportant,
      pressed: email.isImportant,
    },
    { key: "label", label: t("pane.menu.label"), onSelect: onLabel },
    // Re-file while reading (#921): the verb the desk offers its selection,
    // applied to the message being read. Present only when the route passed one
    // — a route with no areas has nothing to file into.
    ...(onRefile ? [{ key: "refile", label: t("pane.menu.refile"), onSelect: onRefile }] : []),
    { key: "rule", label: t("rules.fromMessage"), onSelect: onCreateRule },
    { key: "print", label: t("action.print"), onSelect: onPrint },
    // Only for a message the classifier still has an open question about
    // (#176). A permanently-disabled "Dismiss" on every other message would be
    // a control that never does anything.
    ...(email.suggestions.length > 0
      ? [{ key: "dismiss", label: t("queue.dismissOne"), onSelect: onDismiss }]
      : []),
  ];

  return (
    // A NAMED COMPLEMENTARY LANDMARK (#447). `<aside>` rather than `<section>`:
    // both are landmarks once they have an accessible name, but a reader looking
    // for the message they just opened is looking for the pane BESIDE the list,
    // and `complementary` is the only role that says that. The name is the
    // subject, so the landmark list reads "Your invoice is ready, complementary"
    // rather than "region" twelve times. Nothing in the stylesheet selects the
    // element type (every rule is `.thread-pane`), and neither element carries a
    // UA margin, so this is a rename in the accessibility tree only.
    <aside
      className="thread-pane"
      ref={paneRef}
      // Focusable but not tabbable: it is where focus LANDS when the pane takes
      // the main area (see the effect above), never a stop on the way past.
      tabIndex={-1}
      aria-label={stripBidiControls(email.subject) || t("list.noSubject")}
    >
      {/* THE TOP LINE, PHONE ONLY: back, where back goes, and where you are.
          Hidden above the breakpoint by the stylesheet, where the list is
          beside the pane and the toolbar's own ← is the back affordance. It is
          rendered rather than conditioned on `phone` so the two never disagree
          about the breakpoint — but the toolbar's ← IS hidden there, so there
          is never a second back button on screen. */}
      {backLabel ? (
        <div className="pane-topline">
          <button type="button" className="pane-topline-back" onClick={onClose}>
            <span aria-hidden="true">←</span> {backLabel}
          </button>
          {position ? (
            <span className="pane-topline-position">
              {t("pane.position", { index: position.index, count: position.count })}
            </span>
          ) : null}
        </div>
      ) : null}

      <PaneToolbar
        inTray={email.isFlagged}
        muted={muted}
        snoozed={email.isSnoozed}
        compact={phone}
        onBack={onClose}
        onArchive={onArchive}
        onNotJunk={isInJunk(state, email.id) ? onNotJunk : undefined}
        onTray={onTray}
        onSnooze={onSnooze}
        onMute={onMute}
        onReply={onReply}
        onReplyAll={onReplyAll}
        menu={menu}
      />

      <div className="thread-body">
        {/* Bidi-stripped, like every sender-authored string this pane renders in
            OUR DOM rather than in the sandboxed frame (#307, core/hostileText.ts).
            The subject heading sits directly above the panels, so a directional
            override here would reorder the line it shares. */}
        <h1 className="thread-subject">{stripBidiControls(email.subject) || t("list.noSubject")}</h1>
        {muted ? (
          <p className="thread-muted" role="status">
            {t("thread.muted")}
          </p>
        ) : null}
        <PriorityPanel email={email} />
        {/* The classifier's open questions about THIS message (#176), directly
            under the priority panel because they are the same kind of thing: what
            the system thinks, offered for correction. */}
        <SuggestionPanel email={email} onLabel={onLabel} onDismiss={onDismiss} />
        <LabelProvenanceNote email={email} />
        {/* Unsubscribe (#58). Under the label provenance rather than up in the action
            row: it is a thing you do to a SENDER, not to this message, and the action
            row is where per-message verbs live. */}
        <UnsubscribePanel email={email} />
        {email.isSnoozed && email.snoozedUntil ? (
          // Say it in the pane, like the mute banner: a message you are reading
          // that is also scheduled to come back is a state worth stating, not
          // one to infer from a list cue you are no longer looking at.
          <p className="thread-snoozed" role="status">
            {t("snooze.until", { when: formatWake(email.snoozedUntil) })}
          </p>
        ) : null}
        {messages.length > 1 ? (
          <p className="thread-count">{t("thread.messages", { count: messages.length })}</p>
        ) : null}

        {messages.map((m) => {
          // The reading pane already showed name AND address side by side, which
          // is the right answer for a surface with room for it — so #307's change
          // here is the bidi half: the two spans below share ONE line box, i.e.
          // one bidi paragraph, so a U+202E in the display name reordered the
          // ADDRESS beside it. `unicode-bidi: isolate` in styles.css now confines
          // that, and senderIdentity removes the control outright; neither alone
          // covers the aria-label, which is a plain string with no box at all.
          const from = senderIdentity(m.from?.[0]);
          const text = bodyText(m, m.textBody);
          // HTML only when a text/html part exists (#871): a lone text/plain
          // part is also listed in htmlBody, and in the frame it lost its breaks.
          const html = htmlToRender(m as Email);
          // Attachments PLUS the inline images the body will not show — held
          // by the quarantine, or referenced by nothing (#675). The viewer pages
          // through the same list.
          const attachments = listedParts(m);

          // FOLDED: one line, and a button that says so. Everything below the
          // header — the seal, the body, the attachments — is not rendered at
          // all rather than hidden with CSS, so a folded message costs no
          // sandboxed iframe and no inline-image fetch.
          if (!plan.has(m.id) && !alsoExpanded.has(m.id)) {
            return (
              <button
                type="button"
                key={m.id}
                className="message-folded"
                aria-expanded={false}
                // NAMED BY ITS CONTENT, NOT AN aria-label (#759, WCAG 2.5.3
                // Label in Name). The label used to be "Show the message from
                // {sender}", which a speech-input user saying the preview they
                // can SEE could not match. The button's own text — sender,
                // preview, time — is now its name, and aria-expanded=false is
                // what says it opens.
                onClick={() => setAlsoExpanded((prev) => new Set(prev).add(m.id))}
              >
                <span className="message-folded-from">
                  {from.name || from.address || t("list.noSender")}
                </span>
                <span className="message-folded-preview">{stripBidiControls(m.preview)}</span>
                <time className="message-folded-time" dateTime={m.receivedAt}>
                  {rowTime(m.receivedAt, now)}
                </time>
              </button>
            );
          }

          return (
            <article className="message" key={m.id} aria-label={from.address || t("list.noSender")}>
              <header className="message-header">
                <div>
                  {/* The name is a BUTTON now (#375 increment 5): hovering it
                      opens the sender popover, clicking pins it. The address
                      beside it is unchanged — it is the disambiguation #307
                      added, not an affordance. */}
                  <SenderPopover email={m} />
                  {from.name && from.address ? (
                    <span className="message-address">&lt;{from.address}&gt;</span>
                  ) : null}
                </div>
                <time dateTime={m.receivedAt}>{new Date(m.receivedAt).toLocaleString()}</time>
              </header>
              {/* Sender authentication (#55). Directly under the name it qualifies,
                  because it is a statement ABOUT that name — and rendered for the
                  ~0.8% of mail with something wrong, never as a "verified" badge on
                  the rest. senderAuthNotice holds that judgement and its reasons. */}
              <SenderAuthLine
                auth={m.authResults}
                sender={from.address || from.name || t("list.noSender")}
              />
              {/* Sender-domain warnings (#397). Directly below SenderAuthLine: a
                  reader looking at the name that just got a caution about
                  authentication is exactly who also needs to know the domain
                  itself is a lookalike, punycode, or a stranger claiming the
                  reader's own domain. */}
              <SenderWarningLines warnings={m.senderWarnings} />
              {/* Encrypted mail (#47 6b): said once, under the sender lines. */}
              <EncryptionLine email={m} />
              {/* PGP signature (#47, ADR-039): verified / bad / unknown / changed key. */}
              <SignatureLine email={m} />
              {m.to?.length ? (
                <p className="message-meta">
                  {t("thread.to")}:{" "}
                  {/* THE ADDRESS IS ALWAYS SHOWN, not just the name (#600). This
                      was `name || email`, so a recipient with a display name had
                      their address dropped — and with no title, popover or expand
                      on this line, it was unreachable rather than merely hidden.
                      A display name is chosen by whoever set it, so two different
                      people render identically; the sender's address is revealed
                      for exactly that reason (pane/SenderPopover.tsx) and the
                      argument does not stop at From. */}
                  <span className="message-address">
                    {addressLine(
                      m.to.map((a) => ({
                        name: stripBidiControls(a.name ?? ""),
                        email: stripBidiControls(a.email),
                      })),
                    )}
                  </span>
                </p>
              ) : null}
              {m.deliveredTo ? (
                <p className="message-meta">
                  {t("thread.deliveredTo")}:{" "}
                  <span className="message-address">{stripBidiControls(m.deliveredTo)}</span>
                </p>
              ) : null}

              {/* Where this message is filed, and what else it is tagged with
                  (#375 increment 5). PER MESSAGE, under the meta line, because
                  keywords and the resolved area are per-message facts — in a
                  conversation two messages can genuinely wear different
                  labels. */}
              <MessageLabels email={m} onAddLabel={onLabel} />

              {/* A calendar invitation, cancellation or reply (#725): the card
                  sits ABOVE the body, because for an invitation the card is the
                  message and the body is the organiser's covering note. */}
              <InviteCard email={m} />

              {html ? (
                // Prefer HTML whenever the message has it (Forgejo #99). Almost
                // every real message is multipart/alternative, and the text/plain
                // twin is a degraded fallback — showing it instead of the HTML
                // hid layout, inline images, and the remote-content banner. The
                // HTML renders ONLY inside the sandboxed, opaque-origin iframe
                // with the strict CSP (messageDocument.ts, #39); that sandbox is
                // the whole reason we can default to it. (A future "view plain
                // text" toggle can expose `text`; the DEFAULT is HTML-when-present.)
                <HtmlBody
                  html={html}
                  // The frame's accessible name. It is an ATTRIBUTE, so no CSS
                  // isolate reaches it — stripping is the only thing that does.
                  title={stripBidiControls(m.subject) || t("list.noSubject")}
                  htmlParts={m.htmlBody}
                  attachments={m.attachments}
                  scan={m.attachmentScan}
                  // The un-normalised address is what the remembered-allow
                  // matcher wants (#202): core/remoteAllows.ts lowercases and
                  // trims it itself, and comparing against the same value the
                  // store holds is the point. Bidi-stripped all the same — a
                  // control character in an address is not part of the address.
                  senderAddress={from.address}
                  messageId={m.id}
                />
              ) : text ? (
                <pre className="message-text">{text}</pre>
              ) : (
                // The body has not been fetched yet (the reading-pane Email/get is
                // in flight), or the message genuinely has no text: the server's
                // own preview is the honest thing to show meanwhile.
                <p className="message-preview">{stripBidiControls(m.preview)}</p>
              )}

              {attachments.length > 0 ? (
                <AttachmentList
                  messageId={m.id}
                  threadId={email.threadId}
                  attachments={attachments}
                  scan={m.attachmentScan}
                />
              ) : null}
            </article>
          );
        })}
      </div>
    </aside>
  );
}
