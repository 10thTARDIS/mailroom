// SPDX-License-Identifier: AGPL-3.0-only

// ATTACHMENT TILES (#448): what is going with this message.
//
// TWO KINDS OF TILE, and the difference is where the bytes are:
//
//   * an UPLOAD — chosen here, already POSTed to /jmap/upload, so it has a blobId
//     of its own and can be removed;
//   * a PART OF THE MESSAGE BEING FORWARDED — the bytes are already on the server
//     inside the parent message, and the draft create references them by their
//     part blobId. The server resolves either form through the SAME ownership gate
//     the download route uses (internal/api/draft.go resolveAttachments), so
//     forwarding by reference costs no upload and no round trip.
//
// A HELD PART CANNOT BE FORWARDED, and this is the one place in the composer where
// the server would refuse the whole send rather than the part: `resolveAttachments`
// answers a quarantined blobId with `setInvalidProps` ("release it before attaching
// it to a message"), and it refuses the CREATE, i.e. the entire draft. So a held
// tile is excluded from the send until it is released, and it says so on itself —
// rather than being silently dropped (the user would believe they forwarded it) or
// silently included (the send fails with a server error about a file, on Send,
// after the message was written).
//
// ADR-027 §11 names "a user forwards malware" as the hole this gate closes, which
// is why the tile offers RELEASE rather than an override: releasing is the user
// saying these bytes are fine, recorded server-side, and it is the same decision
// the reading pane offers on the same part.

import { useState } from "react";
import type { JSX } from "react";

import { useApp } from "../../state";
import { t } from "../../i18n";
import type { MessageKey } from "../../i18n";
import { ConfirmDialog } from "../desk/ConfirmDialog";

/** A file the user attached here. */
export interface UploadTile {
  kind: "upload";
  blobId: string;
  name: string;
  type: string;
  size: number;
}

/** A part of the message being forwarded, carried by reference. */
export interface ForwardTile {
  kind: "forward";
  /** The part's blobId — what travels in the draft create. Null for a part the
   * server did not give one (a multipart container), which cannot be forwarded. */
  blobId: string | null;
  partId: string | null;
  name: string;
  type: string;
  size: number;
  /** True when the scan gate would refuse it right now (attachmentScanPolicy
   * isWithheld). Such a tile is EXCLUDED from the send. */
  held: boolean;
  /** Why, when held. */
  reasonKey: MessageKey;
}

export type Tile = UploadTile | ForwardTile;

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} kB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function AttachmentTiles({
  tiles,
  uploading,
  onRemove,
  onFiles,
  onReleased,
  parent,
}: {
  tiles: readonly Tile[];
  uploading: boolean;
  /** Remove one upload. A forwarded part is not removable through this — dropping
   * a part of the message you are forwarding is a different act, and the design
   * does not ask for it. */
  onRemove: (blobId: string) => void;
  onFiles: (files: FileList | null) => void;
  /** Called with the partId AFTER a release the server accepted. The caller
   * records it: the composer holds a FROZEN copy of the parent, so nothing else
   * can make the tile stop being held. */
  onReleased: (partId: string) => void;
  /** The message being forwarded, for the release call (it needs the thread and
   * the message the part belongs to). Null when nothing is being forwarded. */
  parent: { threadId: string; emailId: string } | null;
}): JSX.Element {
  const { actions } = useApp();
  const [dragging, setDragging] = useState(false);
  const [confirming, setConfirming] = useState<{ partId: string; name: string } | null>(null);
  const [releasing, setReleasing] = useState(false);

  const confirmRelease = async (): Promise<void> => {
    if (!confirming || !parent) return;
    setReleasing(true);
    const ok = await actions.releaseAttachment(parent.threadId, parent.emailId, confirming.partId);
    setReleasing(false);
    setConfirming(null);
    // Only on success: a failed release has already surfaced its own error, and
    // reporting one would claim a change the server refused.
    if (ok) onReleased(confirming.partId);
  };

  return (
    <div
      className={`compose-attach${dragging ? " is-dragover" : ""}`}
      onDragOver={(ev) => {
        ev.preventDefault();
        setDragging(true);
      }}
      onDragLeave={(ev) => {
        ev.preventDefault();
        setDragging(false);
      }}
      onDrop={(ev) => {
        ev.preventDefault();
        setDragging(false);
        onFiles(ev.dataTransfer.files);
      }}
    >
      <label className="compose-attach-zone">
        <span className="compose-attach-label">{t("compose.attachZone")}</span>
        <input
          type="file"
          multiple
          className="file-input"
          aria-label={t("compose.attach")}
          onChange={(ev) => {
            onFiles(ev.target.files);
            ev.target.value = "";
          }}
        />
      </label>
      {uploading ? <span className="hint">{t("compose.uploading")}</span> : null}

      {tiles.length > 0 ? (
        <ul className="compose-tiles" aria-label={t("compose.attachments")}>
          {tiles.map((tile) => {
            const key = tile.blobId ?? `${tile.kind}:${tile.name}`;
            const held = tile.kind === "forward" && tile.held;
            return (
              // ⚠ THE LIST ITEM STAYS A LIST ITEM, and the group lives inside it.
              // `role="group"` was on the <li> itself, which REPLACES its implicit
              // listitem role — leaving a <ul> whose children are not list items,
              // so a reader announced a list of nothing (axe `list`). It went
              // unseen because the only composer axe ever looked at was a blank
              // one: tiles appear on a forward, and the first surface to open a
              // forward (#612's reload surface) failed on it immediately.
              <li key={key}>
                <div
                  className={`compose-tile${held ? " is-held" : ""}`}
                  // A group rather than a bare list item: a held tile is a name, a
                  // state and a reason, and a reader needs all three together.
                  role="group"
                  aria-label={
                    held ? t("attachment.withheldLabel", { name: tile.name }) : tile.name
                  }
                >
                <span className="compose-tile-icon" aria-hidden="true">
                  {held ? "⚠" : "📎"}
                </span>
                <span className="compose-tile-name">{tile.name}</span>
                <span className="compose-tile-size">{formatSize(tile.size)}</span>
                {tile.kind === "forward" && !held ? (
                  <span className="compose-tile-origin">{t("compose.attachFromMessage")}</span>
                ) : null}
                {held ? (
                  <>
                    <span className="compose-tile-held">{t("compose.attachHeld")}</span>
                    <span className="compose-tile-reason">{t(tile.reasonKey)}</span>
                    {tile.partId && parent ? (
                      <button
                        type="button"
                        className="button button-quiet compose-tile-release"
                        onClick={() =>
                          setConfirming({ partId: tile.partId as string, name: tile.name })
                        }
                      >
                        {t("compose.attachRelease")}
                      </button>
                    ) : null}
                  </>
                ) : null}
                {tile.kind === "upload" ? (
                  <button
                    type="button"
                    className="icon-button"
                    aria-label={t("compose.removeAttachment", { name: tile.name })}
                    onClick={() => onRemove(tile.blobId)}
                  >
                    ✕
                  </button>
                ) : null}
                </div>
              </li>
            );
          })}
        </ul>
      ) : null}

      {confirming ? (
        <ConfirmDialog
          title={t("attachment.releaseConfirmTitle", { name: confirming.name })}
          body={t("attachment.releaseConfirmBody")}
          confirmLabel={releasing ? t("attachment.releasing") : t("attachment.releaseAnyway")}
          busy={releasing}
          onConfirm={() => void confirmRelease()}
          onCancel={() => setConfirming(null)}
        />
      ) : null}
    </div>
  );
}

/**
 * The attachments a send should actually carry.
 *
 * ⚠ THE HELD FILTER LIVES HERE, at the boundary between the tiles and the send,
 * so there is exactly one place that decides it. A held part left in would make
 * the server refuse the whole draft create — not the part — so the message the
 * user just wrote would fail on Send with an error about a file.
 */
export function sendableAttachments(
  tiles: readonly Tile[],
): { blobId: string; type: string; name: string }[] {
  return tiles
    .filter((tile) => !(tile.kind === "forward" && tile.held))
    .filter((tile): tile is Tile & { blobId: string } => typeof tile.blobId === "string")
    .map((tile) => ({
      blobId: tile.blobId,
      type: tile.type || "application/octet-stream",
      name: tile.name,
    }));
}
