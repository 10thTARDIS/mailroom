// SPDX-License-Identifier: AGPL-3.0-only

// WHAT A HELD FILE SHOWS INSTEAD OF ITSELF (#449, ADR-027 §8/§14).
//
// TWO CASES, TWO WEIGHTS, ONE VOCABULARY. "The scanner believes this file is
// harmful" and "nothing has managed to look at this file" are different findings,
// and a single "withheld" panel would tell a user the scanner caught something it
// never opened. So the two get different tones, different sentences, and — the
// part that is behaviour rather than wording — DIFFERENT PRIMARY BUTTONS:
//
//   could not be scanned   Release and open is the primary. The usual cause is a
//                          password-protected archive or PDF; making someone hunt
//                          for the way into their own file would be the app
//                          treating "unchecked" as "guilty".
//   flagged                Keep held is the primary, and releasing asks again.
//                          Releasing a flagged file is a real risk to the device
//                          it gets opened on, so the emphasised answer is the safe
//                          one and the release is deliberately two decisions.
//
// Which of those applies is decided in ./kinds.ts heldPanelSpec, where a test can
// reach it — this file prints the spec and calls the release.
//
// AND THE SENTENCE THAT IS EASY TO GET WRONG: a release is not scoped to this
// message. The server keys `attachment_releases` by the part's content hash, so
// releasing covers every copy of the same bytes in the mailbox, now and later
// (internal/store/attachments.go). Both bodies say so, because a user who thinks
// they are unblocking one message is making a bigger decision than they know.
//
// WHAT IS NOT HERE: the scanner's named reason. The server exposes the state and
// nothing else, on purpose (internal/api/attachment_scan.go: verdict age and
// signature-DB version churn on every re-scan pass, so they would be stale claims
// on a panel). A placeholder like "Reason: unavailable" would read as a failure
// rather than as a deliberate silence, so there is none.

import { useState } from "react";
import type { JSX } from "react";

import { useApp } from "../../state";
import { t } from "../../i18n";
import { ConfirmDialog } from "../desk/ConfirmDialog";
import { heldPanelSpec } from "./kinds";

export function HeldPanel({
  state,
  name,
  threadId,
  messageId,
  partId,
  confirming,
  onConfirming,
  onKeepHeld,
}: {
  /** The part's scan state (`flagged`, `unknown`, `unscanned`, …). */
  state: string | undefined;
  /** Bidi-stripped by the caller — it goes into a confirmation title. */
  name: string;
  threadId: string;
  messageId: string;
  partId: string;
  /** Whether the second confirmation is showing. OWNED BY THE VIEWER, not here:
   *  the viewer holds the keyboard while it is open (its capture listener stops
   *  every key from reaching this subtree), so it is the only component that can
   *  make Escape peel one layer at a time instead of closing both. */
  confirming: boolean;
  onConfirming: (open: boolean) => void;
  /** Keep it held: the decision to walk away, which closes the viewer. */
  onKeepHeld: () => void;
}): JSX.Element {
  const { actions } = useApp();
  const spec = heldPanelSpec(state);
  const [busy, setBusy] = useState(false);

  /** Releases, for real. `actions.releaseAttachment` reloads the whole thread —
   * the server's decision can affect every message sharing these bytes — so the
   * file starts rendering on its own the moment the store comes back with the part
   * no longer quarantined. Nothing here tries to predict that locally: a release
   * the server refused must leave the panel exactly where it is, and it surfaces
   * its own error toast. */
  const release = async (): Promise<void> => {
    setBusy(true);
    await actions.releaseAttachment(threadId, messageId, partId);
    setBusy(false);
    onConfirming(false);
  };

  const releaseButton = (
    <button
      type="button"
      className={
        spec.primary === "release"
          ? "button button-primary"
          : "button button-danger"
      }
      disabled={busy}
      onClick={() => {
        // The flagged case asks once more; the unchecked case does not. That
        // difference is heldPanelSpec's, not this component's.
        if (spec.asksAgain) onConfirming(true);
        else void release();
      }}
      // The label comes from the spec, and its WEIGHT is the point: one says what
      // you get ("Release and open"), the other says what you are accepting
      // ("I understand the risk — release").
      key="release"
    >
      {busy ? t("attachment.releasing") : t(spec.releaseKey)}
    </button>
  );

  const keepButton = (
    <button
      type="button"
      className={spec.primary === "keep" ? "button button-primary" : "button"}
      disabled={busy}
      onClick={onKeepHeld}
      key="keep"
    >
      {t("viewer.held.keep")}
    </button>
  );

  // The PRIMARY comes first in the DOM, so it is also first in the tab order —
  // the emphasis and the keyboard cannot disagree about which answer is the one
  // being offered.
  const buttons: JSX.Element[] =
    spec.primary === "keep" ? [keepButton, releaseButton] : [releaseButton, keepButton];

  return (
    <div className={`viewer-held viewer-held-${spec.tone}`} role="group" aria-label={t(spec.titleKey)}>
      {/* A heading, not a paragraph: this panel replaces the file, so it is the
          content of the area and its title is a real heading in the document. */}
      <h2 className="viewer-held-title">{t(spec.titleKey)}</h2>
      <p className="viewer-held-body">{t(spec.bodyKey)}</p>
      <div className="viewer-held-actions">{buttons}</div>
      {confirming ? (
        <ConfirmDialog
          title={t("viewer.held.confirmTitle", { name })}
          body={t("viewer.held.confirmBody")}
          confirmLabel={busy ? t("attachment.releasing") : t("viewer.held.confirmRelease")}
          busy={busy}
          onConfirm={() => void release()}
          onCancel={() => onConfirming(false)}
        />
      ) : null}
    </div>
  );
}
