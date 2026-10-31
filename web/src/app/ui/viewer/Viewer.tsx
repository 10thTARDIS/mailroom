// SPDX-License-Identifier: AGPL-3.0-only

// THE ATTACHMENT VIEWER (#449, design pass-2 §9): one file, in the main area,
// with every other file on the message along the bottom.
//
// IT TAKES THE MAIN AREA THE WAY FOCUSED READING DOES, and that is the whole
// difference from the lightbox it replaces (#200). That was a floating box
// centred on a translucent `.modal-backdrop` — the same dimmed `rgba(0,0,0,0.45)`
// scrim this app's brief yes/no dialogs still use (AddAccountWizard, ConfirmDialog,
// ReleaseConfirmDialog…), so this is not a blanket "no surface is ever
// translucent" rule. The difference is DURATION and WHAT SITS BEHIND IT: a
// confirmation dims the app for one click; reading a file is not a click, it is
// however long you look, and floating that over a dimmed reading pane is the
// shape this rewrite avoids. So the viewer is a GRID OCCUPANT, not an overlay: it takes the
// reading pane's own column with the list still beside it, or the whole content
// area where there is no second column, or the phone's single screen. The three
// cases and the CSS are ui/Shell.tsx + app/viewer.css, from the same placement
// function the composer uses (ui/compose/placement.ts) so that the attribute on
// `.app` and the class on this element cannot disagree about which track exists.
//
// IT IS STILL A DIALOG. `role="dialog"`, `aria-modal`, focus trapped, Escape
// closes and focus goes back to the tile that opened it (#447). Trapped in ALL
// three placements, unlike the composer, and the difference is what the surface IS:
// a composer is somewhere you are working, so Tab reaching a visible list beside it
// is a feature; a viewer is somewhere you are LOOKING, one Escape from being gone,
// and a Tab that walked into the list behind it would leave a modal open over mail
// the next keystroke could archive.
//
// WHERE THE DATA COMES FROM, AND WHY NOT FROM PROPS. The parts and the scan are
// read LIVE out of the store by message id, not frozen into the request that
// opened this. Releasing a held file reloads the whole thread (the server's
// decision is keyed on the part's content hash and can affect every message
// sharing those bytes), and that reload is exactly what makes the held panel turn
// into the file. A frozen copy would leave the explanation on screen after the
// release the user just made — the file would be released and still look held.
//
// NOT BUILT HERE, deliberately, and named so nobody looks for it: FIND IN FILE and
// a TEXT VIEW FOR PDFs. Both need the text the server extracts for the search index,
// and the API exposes none of it to this client. The follow-up is that exposure;
// until then there is no search box, because a search box that searched nothing
// would be a worse answer than its absence.

import { useCallback, useEffect, useRef, useState } from "react";
import type { JSX } from "react";

import { effectiveEmail } from "../../../core/store";
import { senderIdentity, senderText, stripBidiControls } from "../../../core/hostileText";
import { withCompose } from "../../../core/routes";
import { navigate, useRoute } from "../../routing";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import { bytesText } from "../desk/format";
import { FilePanel, PreviewBody } from "../attachmentPreview";
import { BlobNotice, GuardedBlobLink, useBlobGuard } from "../blobGuard";
import { scanStateFor } from "../attachmentScanPolicy";
import { listedParts } from "../messageParts";
import type { ViewerRequest } from "../shellContext";
import { HeldPanel } from "./HeldPanel";
import { ViewerStrip } from "./StripBar";
import { ViewerTopBar } from "./TopBar";
import { trapTab } from "../focusTrap";
import { isHeldMode, viewerMode } from "./kinds";
import { stepIndex, stripItems } from "./strip";

export type ViewerPlacement = "pane" | "main" | "full";

export function Viewer({
  request,
  placement,
  onClose,
}: {
  request: ViewerRequest;
  placement: ViewerPlacement;
  onClose: () => void;
}): JSX.Element | null {
  const state = useMailState();
  const { client } = useApp();
  const route = useRoute();
  const dialogRef = useRef<HTMLDivElement | null>(null);

  const [index, setIndex] = useState(request.index);
  /** An image's intrinsic size, for the top bar. Only the decoded image knows it. */
  const [imageSize, setImageSize] = useState<{ w: number; h: number } | null>(null);
  /** Images: fit the area, or draw at 100%. */
  const [fit, setFit] = useState(true);
  /** PDF: ask the browser's viewer to fit the page width. */
  const [fitWidth, setFitWidth] = useState(false);
  /**
   * The second confirmation for releasing a FLAGGED file, held here rather than
   * inside HeldPanel — because Escape has to peel one layer at a time and this
   * component owns the keyboard while it is open (see the capture listener below).
   * A dialog whose own Escape handler never fires is exactly the bug that would
   * produce: one press would close the confirmation AND the viewer under it.
   */
  const [confirming, setConfirming] = useState(false);
  // A download or new-tab open the server refuses is explained in the viewer, not
  // navigated to as a bare 403 (#47).
  const guard = useBlobGuard();
  const clearNotice = guard.clear;

  const email = effectiveEmail(state, request.messageId);
  // The SAME list the reading pane's chips index into (#675): attachments plus
  // the inline images the body does not show, a held one included.
  const parts = email ? listedParts(email) : [];
  const part = parts[index];

  /** Moving between attachments resets what belonged to the last one. */
  const goTo = useCallback((next: number) => {
    setIndex(next);
    setImageSize(null);
    setConfirming(false);
    clearNotice();
  }, [clearNotice]);

  const step = useCallback(
    (delta: number) => {
      if (parts.length < 2) return;
      goTo(stepIndex(index, delta, parts.length));
    },
    [goTo, index, parts.length],
  );

  // THE KEYBOARD IS TAKEN, NOT SHARED, WHILE THIS IS OPEN.
  //
  // Shell.tsx's global keydown decides whether to act on the message list, and it
  // guards on the route (a composer, the settings page) — it cannot see a dialog a
  // route opened for itself. Without this, `j`/`k` would keep moving the selection
  // underneath and `←`/`→` would page attachments AND do whatever the list does
  // with arrows.
  //
  // Rather than teach the shell about a component it does not own, this listens in
  // the CAPTURE phase on window and stops propagation: a capture listener on window
  // runs before the event reaches the target, so the bubble-phase listener the
  // shell installed never sees it. The viewer owns the keyboard for as long as it
  // is open and gives it back on unmount with no bookkeeping anywhere else.
  //
  // TAB IS LET THROUGH, and that is load-bearing twice over: it is what lets the
  // focus trap below (a React handler, i.e. bubble phase) see it at all, and taking
  // it would trap a keyboard user in a dialog with no way out but the mouse.
  useEffect(() => {
    const onKey = (ev: KeyboardEvent): void => {
      switch (ev.key) {
        case "Escape":
          ev.preventDefault();
          ev.stopPropagation();
          // One layer per press: the release confirmation first, then the viewer.
          if (confirming) setConfirming(false);
          else onClose();
          return;
        case "ArrowLeft":
          ev.preventDefault();
          ev.stopPropagation();
          if (!confirming) step(-1);
          return;
        case "ArrowRight":
          ev.preventDefault();
          ev.stopPropagation();
          if (!confirming) step(1);
          return;
        default:
          // Everything else is swallowed too, so a stray `e` does not archive the
          // message whose attachment is on screen.
          if (ev.key !== "Tab") ev.stopPropagation();
      }
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [confirming, onClose, step]);

  // FOCUS LANDS ON THE DIALOG ITSELF, not on a button in it (WAI-ARIA APG allows
  // either). The consequence is deliberate: when a held panel is showing, NOTHING
  // is under the return key — least of all the release of a flagged file — and the
  // reader Tabs to the answer they want.
  useEffect(() => {
    dialogRef.current?.focus();
  }, []);

  /**
   * A message that went away under us, or an index that names nothing.
   *
   * Closing rather than rendering an empty frame: the shell has given this a grid
   * track, and a track with nothing in it reads as a rendering failure rather than
   * as a closed viewer. Done in an effect because it is a state change, and returns
   * null for the one render in between.
   */
  const missing = !email || !part;
  useEffect(() => {
    if (missing) onClose();
  }, [missing, onClose]);
  if (!email || !part) return null;

  const mode = viewerMode(part, email.attachmentScan);
  const held = isHeldMode(mode);
  const scanPart = scanStateFor(email.attachmentScan, part.partId);
  const strip = stripItems(parts, email.attachmentScan, index);

  // Sender-authored text in our own chrome, stripped on the way in — the label the
  // top bar prints and the `download` name that lands on the user's DISK are
  // different harms and both are covered (ThreadPane.tsx says it at length).
  const name = stripBidiControls(part.name) || stripBidiControls(part.type);
  const dlName = stripBidiControls(part.name) || "attachment";
  const sender = senderText(senderIdentity(email.from?.[0]), t("list.noSender"));

  const downloadHref =
    !held && part.blobId ? client.blobUrl(part.blobId, dlName, part.type) : null;
  // "Open in a tab" POINTS AT THE PREVIEW ROUTE, and only where that route serves
  // anything: the download route is `Content-Disposition: attachment` ALWAYS
  // (docs/hostile-content.md §2.4), so a new tab pointed at it would download the
  // file rather than open it — a control that did not do what its words say. The
  // preview route serves exactly one type inline (application/pdf) and is the same
  // door the frame beside it already uses, so this opens NO new one; for every
  // other type there is nothing a tab could show and the button is absent.
  const tabHref = !held && mode === "pdf" && part.blobId ? client.previewUrl(part.blobId, name) : null;

  const downloadAction = downloadHref ? (
    <GuardedBlobLink
      guard={guard}
      className="button"
      href={downloadHref}
      download={dlName}
      ariaLabel={t("attachment.downloadNamed", { name })}
    >
      {t("attachment.download")}
    </GuardedBlobLink>
  ) : null;

  /** Forward: the composer, on the message this attachment belongs to.
   *
   * ⚠ IT FORWARDS THE MESSAGE, NOT THIS ONE FILE, and the spec allows saying so
   * here rather than pretending otherwise. #448's composer carries EVERY part of
   * the forwarded message by reference (ui/compose/AttachmentTiles.tsx builds one
   * tile per attachment from the frozen parent) and has no notion of a subset —
   * `?compose=forward:<id>` names a message and nothing finer. Preselecting one
   * attachment is a composer change (a forward whose tiles start deselected), not
   * a viewer one.
   *
   * The viewer closes on the way: the composer wants the same column the viewer is
   * holding, and two occupants of one grid track would overlap. */
  const onForward = (): void => {
    navigate(withCompose(route, { forward: request.messageId }));
    onClose();
  };

  const controls =
    mode === "image" ? (
      <div className="viewer-zoom" role="group" aria-label={t("viewer.zoom")}>
        <button
          type="button"
          className="button"
          aria-pressed={fit}
          onClick={() => setFit(true)}
        >
          {t("viewer.fit")}
        </button>
        <button
          type="button"
          className="button"
          aria-pressed={!fit}
          onClick={() => setFit(false)}
        >
          {t("viewer.actualSize")}
        </button>
      </div>
    ) : mode === "pdf" ? (
      <button
        type="button"
        className="button"
        aria-pressed={fitWidth}
        onClick={() => setFitWidth((v) => !v)}
      >
        {t("viewer.fitWidth")}
      </button>
    ) : null;

  return (
    <section
      className={`viewer viewer-${placement}`}
      role="dialog"
      aria-modal="true"
      aria-label={t("viewer.title", { name })}
      ref={dialogRef}
      tabIndex={-1}
      // THE TRAP. Only Tab reaches here (the capture listener above swallows the
      // rest), and the two edges wrap onto each other so focus can never reach the
      // list behind this. Shared with the confirm dialog and the composer since
      // #447 — including the case this copy used to get wrong: Shift+Tab from the
      // dialog CONTAINER, which is what has focus on the first keystroke, and which
      // walked straight out of the back of the dialog.
      onKeyDown={(ev) => trapTab(dialogRef.current, ev)}
    >
      <ViewerTopBar
        name={name}
        sizeText={bytesText(part.size)}
        dimensions={
          imageSize ? t("viewer.dimensions", { w: imageSize.w, h: imageSize.h }) : null
        }
        sender={sender}
        scanPart={scanPart}
        held={held}
        downloadHref={downloadHref}
        downloadName={dlName}
        tabHref={tabHref}
        guard={guard}
        onForward={onForward}
        onClose={onClose}
      />

      <div className="viewer-file">
        <BlobNotice notice={guard.notice} />
        {held ? (
          part.partId ? (
            <HeldPanel
              // Remounted per part, so a release in flight cannot land on the next
              // attachment's panel.
              key={part.partId}
              state={scanPart?.state}
              name={name}
              threadId={request.threadId}
              messageId={request.messageId}
              partId={part.partId}
              confirming={confirming}
              onConfirming={setConfirming}
              onKeepHeld={onClose}
            />
          ) : null
        ) : mode === "nopreview" ? (
          // Office and ODF documents land here, and so does everything else with
          // no renderer — including every type blob.go downgrades to
          // application/octet-stream because it is a scripting context. Rendering
          // those is #199 (a document viewer), which is why this says "yet".
          <FilePanel
            title={t("viewer.noPreview")}
            note={t("viewer.noPreviewNote")}
            actions={downloadAction}
          />
        ) : (
          // Keyed on the part, so navigating remounts the loader and its state
          // starts clean rather than briefly showing the previous attachment.
          <PreviewBody
            key={part.blobId ?? String(index)}
            part={part}
            fit={fit}
            fitWidth={fitWidth}
            onImageSize={(w, h) => setImageSize({ w, h })}
            actions={downloadAction}
          />
        )}
      </div>

      <ViewerStrip model={strip} onSelect={goTo} onStep={step} controls={controls} />
    </section>
  );
}
