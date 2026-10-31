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

import { useCallback, useState } from "react";
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

export type BlobGuard = {
  /** The message to show, or null. */
  notice: string | null;
  clear: () => void;
  /** Checks `url`, then opens it (a download, or a new tab) only when served. */
  open: (url: string, opts: { download?: string; newTab?: boolean }) => Promise<void>;
};

export function useBlobGuard(): BlobGuard {
  const { client } = useApp();
  const [notice, setNotice] = useState<string | null>(null);

  const clear = useCallback(() => setNotice(null), []);

  const open = useCallback(
    async (url: string, opts: { download?: string; newTab?: boolean }): Promise<void> => {
      setNotice(null);
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

  return { notice, clear, open };
}

/** The refusal, where the user is looking. Renders nothing without one. */
export function BlobNotice({ notice }: { notice: string | null }): JSX.Element | null {
  if (!notice) return null;
  return (
    <p className="banner banner-error blob-notice" role="alert">
      {notice}
    </p>
  );
}

/** An attachment link that checks before it navigates. */
export function GuardedBlobLink({
  guard,
  href,
  download,
  newTab = false,
  className,
  title,
  ariaLabel,
  children,
}: {
  guard: BlobGuard;
  href: string;
  download?: string;
  newTab?: boolean;
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
        void guard.open(href, { download, newTab });
      }}
    >
      {children}
    </a>
  );
}
