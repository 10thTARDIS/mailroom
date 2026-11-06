// SPDX-License-Identifier: AGPL-3.0-only

// VIEW SOURCE (#43): the raw message as TEXT, and its header block, in a dialog.
//
// THE SOURCE IS ATTACKER-CONTROLLED, and this is the one place the app prints a
// message's bytes unfiltered. So every character reaches the DOM as a React TEXT
// CHILD of a <pre> or a <dd>: no dangerouslySetInnerHTML, no innerHTML, no
// srcdoc, no element is ever built from the message. A `<script>` in the source
// is the six characters "<script>" on screen. (ViewSourceDialog.test.tsx feeds it
// a hostile message and checks no element is created and no handler runs; the
// probe that proves that test can fail is in the PR.)
//
// It fetches the message through the ordinary authenticated blob download
// (JmapClient.fetchBlobOnce) — no new route, no new door — decodes as UTF-8 with
// replacement, shows at most MAX_SOURCE_DISPLAY_BYTES of it, and says so when it
// cut the display short. Download goes through the same guarded blob link every
// attachment uses, and always has the whole message.
//
// The text goes through stripBidiControls for DISPLAY only (a right-to-left
// override in a header would reorder the line around it, #307); Copy and Download
// carry the bytes exactly as received.
//
// Modal: focus trapped, Escape and the backdrop close, labelled, focus goes to
// the dialog on open and back to the opener on close.

import { useEffect, useId, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import { stripBidiControls } from "../../core/hostileText";
import type { BlobFetch } from "../../core/jmap/blobs";
import { decodeAll, decodeSource, parseHeaderRowsCapped } from "../../core/messageSource";
import { t } from "../i18n";
import { closeFocusTarget, trapTab } from "./focusTrap";

type Tab = "source" | "headers";

type Loaded = { bytes: Uint8Array };

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`;
}

export function ViewSourceDialog({
  load,
  onDownload,
  downloadNotice,
  onClose,
}: {
  /** Fetches the message's raw bytes through the authenticated blob download. */
  load: () => Promise<BlobFetch>;
  /** Starts the .eml download (the guarded blob link). */
  onDownload: () => void;
  /** A refusal from that download, to show where the reader is looking. */
  downloadNotice?: string | null;
  onClose: () => void;
}): JSX.Element {
  const [state, setState] = useState<"loading" | "failed" | Loaded>("loading");
  const [tab, setTab] = useState<Tab>("source");
  const [copy, setCopy] = useState<"idle" | "copied" | "failed">("idle");
  const dialogRef = useRef<HTMLDivElement | null>(null);
  const openerRef = useRef<HTMLElement | null>(
    typeof document === "undefined" ? null : (document.activeElement as HTMLElement | null),
  );
  const titleId = useId();
  const loadRef = useRef(load);

  useEffect(() => {
    dialogRef.current?.focus();
    const opener = openerRef.current;
    return () => {
      if (closeFocusTarget(opener) === "opener") opener?.focus();
    };
  }, []);

  useEffect(() => {
    let live = true;
    void loadRef.current().then(async (got) => {
      if (!live) return;
      if (got.kind !== "ok") {
        setState("failed");
        return;
      }
      try {
        const buf = await got.blob.arrayBuffer();
        if (live) setState({ bytes: new Uint8Array(buf) });
      } catch {
        if (live) setState("failed");
      }
    }, () => {
      if (live) setState("failed");
    });
    return () => {
      live = false;
    };
  }, []);

  const shown = useMemo(() => (typeof state === "object" ? decodeSource(state.bytes) : null), [state]);
  const headers = useMemo(() => (shown ? parseHeaderRowsCapped(shown.text) : { rows: [], omitted: 0 }), [shown]);
  const rows = headers.rows;

  const doCopy = async (): Promise<void> => {
    if (typeof state !== "object") return;
    try {
      await navigator.clipboard.writeText(decodeAll(state.bytes));
      setCopy("copied");
    } catch {
      setCopy("failed");
    }
  };

  return (
    <div
      className="modal-backdrop view-source-backdrop"
      onMouseDown={(ev) => {
        if (ev.target === ev.currentTarget) onClose();
      }}
      onKeyDown={(ev) => {
        if (ev.key === "Escape") {
          ev.stopPropagation();
          onClose();
          return;
        }
        trapTab(dialogRef.current, ev);
      }}
    >
      <div
        className="sheet view-source"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        ref={dialogRef}
        tabIndex={-1}
      >
        <header className="sheet-header">
          <h2 id={titleId}>{t("source.title")}</h2>
          <button type="button" className="icon-button" aria-label={t("action.close")} onClick={onClose}>
            ✕
          </button>
        </header>
        <div className="sheet-body">
          <div className="view-source-bar">
            <div role="group" aria-label={t("source.tabs")} className="view-source-tabs">
              <button
                type="button"
                className="button"
                aria-pressed={tab === "source"}
                onClick={() => setTab("source")}
              >
                {t("source.tab.source")}
              </button>
              <button
                type="button"
                className="button"
                aria-pressed={tab === "headers"}
                onClick={() => setTab("headers")}
              >
                {t("source.tab.headers")}
              </button>
            </div>
            <div className="view-source-actions">
              <button type="button" className="button" disabled={typeof state !== "object"} onClick={() => void doCopy()}>
                {t("source.copy")}
              </button>
              <button type="button" className="button" onClick={onDownload}>
                {t("source.download")}
              </button>
            </div>
          </div>
          <p className="field-hint" role="status">
            {copy === "failed" ? t("source.copyFailed") : copy === "copied" ? t("source.copied") : ""}
          </p>
          {downloadNotice ? (
            <p className="banner banner-error" role="alert">
              {downloadNotice}
            </p>
          ) : null}
          {state === "loading" ? (
            <p className="field-hint" role="status">
              {t("source.loading")}
            </p>
          ) : state === "failed" ? (
            <p className="field-error" role="alert">
              {t("source.failed")}
            </p>
          ) : shown ? (
            <>
              {shown.truncated ? (
                <p className="field-hint view-source-truncated" role="status">
                  {t("source.truncated", {
                    shown: formatBytes(new TextEncoder().encode(shown.text).byteLength),
                    total: formatBytes(shown.totalBytes),
                  })}
                </p>
              ) : null}
              {tab === "source" ? (
                // TEXT CHILD ONLY. `tabIndex={0}` so a keyboard user can scroll it.
                <pre className="view-source-text" dir="ltr" tabIndex={0} aria-label={t("source.tab.source")}>
                  {stripBidiControls(shown.text)}
                </pre>
              ) : rows.length === 0 ? (
                <p className="field-hint">{t("source.headers.none")}</p>
              ) : (
                <dl className="view-source-headers" tabIndex={0} aria-label={t("source.tab.headers")}>
                  {rows.map((r, i) => (
                    <div key={i} className="view-source-header">
                      <dt>{r.name ? stripBidiControls(r.name) : t("source.noName")}</dt>
                      <dd>{stripBidiControls(r.value)}</dd>
                    </div>
                  ))}
                  {headers.omitted > 0 ? (
                    <div className="view-source-header view-source-header-more">
                      <dd>{t("source.headers.more", { count: headers.omitted })}</dd>
                    </div>
                  ) : null}
                </dl>
              )}
            </>
          ) : null}
        </div>
      </div>
    </div>
  );
}
