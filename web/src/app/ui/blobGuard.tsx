// SPDX-License-Identifier: AGPL-3.0-only

// A DOWNLOAD THAT MAY BE REFUSED, EXPLAINED IN THE APP (#47).
//
// A plain `<a href download>` to a part the server refuses (the scanner flagged
// it, or could not check it) navigates the browser to a bare "403 Forbidden"
// page: no reason, no way back into the app. That happens for any attachment,
// but it is the ordinary case for an attachment of a DECRYPTED message, whose
// verdict does not exist until the moment of download (the ciphertext is opaque
// at ingest), so the chip cannot know in advance that it is withheld.
//
// So the link asks first (JmapClient.probeBlob, a HEAD with the same cookie) and
// navigates only when the server would serve it. (Not for a part of a DECRYPTED
// message: there a probe is a second decryption and scan, so it is fetched once
// instead, #804; a tab to one gets the server's own explaining page, #805.) A refusal becomes a message in
// the app, with the reason the server sent. The link keeps its real href, so a
// right-click still copies the address, but a plain click never navigates to a
// refusal.
//
// NOT COLOUR-ONLY: the message begins with the word "Blocked" and is a live
// region (role="alert"), so it is read out when it appears.

import { useCallback, useEffect, useRef, useState } from "react";
import type { JSX, MouseEvent, ReactNode } from "react";

import { isDecryptedPartUrl } from "../../core/jmap/blobs";
import type { BlobBlocked } from "../../core/jmap/blobs";
import { stripBidiControls } from "../../core/hostileText";
import { t } from "../i18n";
import { useApp } from "../state";

/** The sentence for a refusal. The detection name is scanner text (a signature
 * database entry such as "Eicar-Test-Signature"), shown only when present and
 * stripped like any other text we did not write. */
export function blockedMessage(b: BlobBlocked): string {
  if (b.verdict === "flagged") {
    const detection = b.signature ? stripBidiControls(b.signature) : "";
    return detection
      ? t("attachment.blocked.flaggedNamed", { detection })
      : t("attachment.blocked.flagged");
  }
  return t("attachment.blocked.unknown");
}

/** The file name in a download URL's last path segment, for a save with no
 * explicit name. */
function nameFromUrl(url: string): string {
  const path = url.split(/[?#]/)[0] ?? "";
  const last = path.slice(path.lastIndexOf("/") + 1);
  try {
    return decodeURIComponent(last) || "attachment";
  } catch {
    return "attachment";
  }
}

/** What a download is opened with. `risky` is the server's `riskyType` reason for
 * the part (ADR-056), or null/absent for an ordinary file. */
export type OpenOpts = { download?: string; newTab?: boolean; risky?: string | null };

/** The risky-type reasons the server sends (internal/attachrisk). Anything else is
 * shown with the generic sentence rather than dropped. */
const RISKY_KEYS = {
  program: "attachment.risky.program",
  script: "attachment.risky.script",
  installer: "attachment.risky.installer",
  macro_document: "attachment.risky.macro_document",
  shortcut: "attachment.risky.shortcut",
  disk_image: "attachment.risky.disk_image",
  disguised: "attachment.risky.disguised",
} as const;

/** The plain-language sentence for a risky reason. */
export function riskyMessage(reason: string): string {
  const key = (RISKY_KEYS as Record<string, (typeof RISKY_KEYS)[keyof typeof RISKY_KEYS] | undefined>)[reason];
  return key ? t(key) : t("attachment.risky.generic");
}

type PendingRisky = { url: string; opts: OpenOpts; name: string; reason: string };

export type BlobGuard = {
  /** The message to show, or null. */
  notice: string | null;
  clear: () => void;
  /** A risky download waiting for the person's decision, or null. */
  pending: { name: string; reason: string } | null;
  /** Download anyway. */
  confirmRisky: () => void;
  /** Cancel: nothing is downloaded. */
  cancelRisky: () => void;
  /** Checks `url`, then opens it (a download, or a new tab) only when served.
   * A risky part asks first (ADR-056). */
  open: (url: string, opts: OpenOpts) => Promise<void>;
};

export function useBlobGuard(): BlobGuard {
  const { client } = useApp();
  const [notice, setNotice] = useState<string | null>(null);
  const [pending, setPending] = useState<PendingRisky | null>(null);

  const clear = useCallback(() => setNotice(null), []);

  const open = useCallback(
    async (url: string, opts: OpenOpts, confirmed = false): Promise<void> => {
      setNotice(null);
      // ⚠ A RISKY TYPE ASKS BEFORE ANYTHING IS FETCHED OR NAVIGATED (ADR-056). The
      // warning is a courtesy of this client, not a control: the server already
      // neutralises the response.
      if (opts.risky && !confirmed) {
        setPending({ url, opts, name: opts.download ?? nameFromUrl(url), reason: opts.risky });
        return;
      }
      // A PART OF A DECRYPTED MESSAGE IS NEVER PROBED (#804). Its verdict exists
      // only once the server has decrypted and scanned it, and it does so on
      // every request, so "ask, then fetch" is two of each. A download is fetched
      // once and saved from memory; a tab is navigated to directly, and a refusal
      // there is the server's own explaining page (#805).
      const decrypted = isDecryptedPartUrl(url);
      if (decrypted && !opts.newTab) {
        const got = await client.fetchBlobOnce(url);
        if (got.kind === "blocked") {
          setNotice(blockedMessage(got.blocked));
          return;
        }
        if (got.kind === "failed") {
          setNotice(t("attachment.fetchFailed"));
          return;
        }
        const objectUrl = URL.createObjectURL(got.blob);
        const a = document.createElement("a");
        a.href = objectUrl;
        a.download = opts.download ?? nameFromUrl(url);
        a.rel = "noreferrer";
        a.click();
        // The browser reads the blob asynchronously; release it later, not now.
        setTimeout(() => URL.revokeObjectURL(objectUrl), 60_000);
        return;
      }
      if (!decrypted) {
        const probe = await client.probeBlob(url);
        if (probe.kind === "blocked") {
          setNotice(blockedMessage(probe.blocked));
          return;
        }
        if (probe.kind === "failed") {
          setNotice(t("attachment.fetchFailed"));
          return;
        }
      }
      // Served: hand it to the browser through a throwaway anchor, which is what
      // the user's click would have done.
      const a = document.createElement("a");
      a.href = url;
      if (opts.download) a.download = opts.download;
      if (opts.newTab) {
        a.target = "_blank";
        a.rel = "noopener noreferrer";
      } else {
        a.rel = "noreferrer";
      }
      a.click();
    },
    [client],
  );

  const confirmRisky = useCallback(() => {
    if (!pending) return;
    const p = pending;
    setPending(null);
    void open(p.url, p.opts, true);
  }, [pending, open]);
  const cancelRisky = useCallback(() => setPending(null), []);

  return {
    notice,
    clear,
    pending: pending ? { name: pending.name, reason: pending.reason } : null,
    confirmRisky,
    cancelRisky,
    open: (url, opts) => open(url, opts),
  };
}

/**
 * THE QUESTION BEFORE A RISKY DOWNLOAD (ADR-056, operator decision Q81).
 *
 * Cancel is the default: it takes focus when the dialog opens, Escape cancels, and
 * focus goes back to the control the person used. An `alertdialog`, so the title and
 * the sentence are read out on open. The sentence says WHY in plain words and does
 * not name a threat it cannot know about.
 */
export function RiskyDownloadDialog({ guard }: { guard: BlobGuard }): JSX.Element | null {
  const cancelRef = useRef<HTMLButtonElement | null>(null);
  const openerRef = useRef<Element | null>(null);
  const open = guard.pending !== null;
  useEffect(() => {
    if (!open) return;
    openerRef.current = document.activeElement;
    cancelRef.current?.focus();
    return () => {
      const el = openerRef.current;
      if (el instanceof HTMLElement && el.isConnected) el.focus();
    };
  }, [open]);
  if (!guard.pending) return null;
  const { name, reason } = guard.pending;
  const title = t("attachment.risky.title", { name: stripBidiControls(name) });
  return (
    <div className="modal-backdrop" onKeyDown={(e) => e.key === "Escape" && guard.cancelRisky()}>
      <div
        className="sheet"
        role="alertdialog"
        aria-modal="true"
        aria-label={title}
        aria-describedby="risky-download-body"
      >
        <header className="sheet-header">
          <h2>{title}</h2>
        </header>
        <div className="sheet-body">
          <p id="risky-download-body">{riskyMessage(reason)}</p>
          <div className="sheet-actions">
            <button type="button" className="button" ref={cancelRef} onClick={guard.cancelRisky}>
              {t("attachment.risky.cancel")}
            </button>
            <button type="button" className="button button-danger" onClick={guard.confirmRisky}>
              {t("attachment.risky.confirm")}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

/** The refusal, where the user is looking. Renders nothing without one. */
export function BlobNotice({
  notice,
  guard,
}: {
  notice: string | null;
  /** When given, the risky-download question is rendered here too. */
  guard?: BlobGuard;
}): JSX.Element | null {
  return (
    <>
      {notice ? (
        <p className="banner banner-error blob-notice" role="alert">
          {notice}
        </p>
      ) : null}
      {guard ? <RiskyDownloadDialog guard={guard} /> : null}
    </>
  );
}

/** An attachment link that checks before it navigates. */
export function GuardedBlobLink({
  guard,
  href,
  download,
  newTab = false,
  risky = null,
  className,
  title,
  ariaLabel,
  children,
}: {
  guard: BlobGuard;
  href: string;
  download?: string;
  newTab?: boolean;
  /** The part's `riskyType`; a download of a risky part asks first. */
  risky?: string | null;
  className?: string;
  title?: string;
  ariaLabel?: string;
  children: ReactNode;
}): JSX.Element {
  return (
    <a
      className={className}
      href={href}
      download={download}
      target={newTab ? "_blank" : undefined}
      rel={newTab ? "noopener noreferrer" : "noreferrer"}
      title={title}
      aria-label={ariaLabel}
      onClick={(ev: MouseEvent<HTMLAnchorElement>) => {
        ev.preventDefault();
        void guard.open(href, { download, newTab, risky });
      }}
    >
      {children}
    </a>
  );
}
