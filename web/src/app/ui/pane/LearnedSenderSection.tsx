// SPDX-License-Identifier: AGPL-3.0-only

// THE "LEARNED SORTING" SECTION OF THE SENDER CARD (#167): what Mailroom has
// learned about this sender from the user's own moves, a way to make it forget,
// and (only for mail that cannot be verified) a switch to trust the sender for
// sorting. The existing "Change area" menu stays the one place to set the area.
//
// FAILURE IS SILENCE for the read: a server without LearnedSender/get, or a
// failed call, draws no section at all, never a heading with nothing under it.
// Forget asks first (the confirm is inline, as the Block form is) and moves
// focus onto "Keep it", the safe choice; cancelling returns it to Forget.

import { useCallback, useEffect, useRef, useState } from "react";
import type { JSX } from "react";

import type { LearnedSender } from "../../../core/jmap/types";
import { t } from "../../i18n";
import { useApp, useMailState } from "../../state";
import { forgetScope, learnedSenderSentence, showTrust, trustUseless } from "./learnedSender";

export function LearnedSenderSection({ address, messageId }: { address: string; messageId: string }): JSX.Element | null {
  const { actions, pushToast } = useApp();
  const areas = useMailState().areas;
  const [state, setState] = useState<LearnedSender | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const forgetRef = useRef<HTMLButtonElement | null>(null);
  const keepRef = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    let live = true;
    if (!address) return;
    void actions.learnedSender(address, messageId).then((s) => {
      if (live) setState(s);
    });
    return () => {
      live = false;
    };
  }, [actions, address, messageId]);

  useEffect(() => {
    if (confirming) keepRef.current?.focus();
  }, [confirming]);

  const cancel = useCallback((): void => {
    setConfirming(false);
    // After the confirm unmounts, focus goes back to the button that opened it.
    queueMicrotask(() => forgetRef.current?.focus());
  }, []);

  if (!address || !state) return null;
  const scope = forgetScope(state);
  const areaName = (id: string): string | null => areas[id]?.name ?? null;

  // forget and trust answer without a message, so the card reads again WITH its
  // message: the switch must not vanish the moment it is turned off.
  const reread = async (fresh: LearnedSender): Promise<LearnedSender> => (await actions.learnedSender(address, messageId)) ?? fresh;

  const forget = (): void => {
    if (scope === null) return;
    setBusy(true);
    void actions
      .forgetLearnedSender(address, scope)
      .then(async (fresh) => {
        if (fresh) {
          setState(await reread(fresh));
          setConfirming(false);
        } else {
          pushToast({ kind: "error", message: t("learnedSender.forget.failed") });
          cancel();
        }
      })
      .finally(() => setBusy(false));
  };

  const trust = (value: boolean): void => {
    setBusy(true);
    void actions
      .trustLearnedSender(address, value)
      .then(async (fresh) => {
        if (fresh) setState(await reread(fresh));
        else pushToast({ kind: "error", message: t("learnedSender.trust.failed") });
      })
      .finally(() => setBusy(false));
  };

  const headingId = `learned-sender-${messageId}`;
  return (
    <section className="sender-popover-section learned-sender" aria-labelledby={headingId}>
      <h3 id={headingId}>{t("learnedSender.heading")}</h3>
      <p aria-live="polite">{learnedSenderSentence(state, areaName)}</p>

      {trustUseless(state) && !state.trusted ? (
        <p className="hint learned-sender-trust-own">{t("learnedSender.trust.ownDomain")}</p>
      ) : showTrust(state) ? (
        <div className="learned-sender-trust">
          <label className="checkbox">
            <input
              type="checkbox"
              role="switch"
              checked={state.trusted}
              disabled={busy}
              onChange={(ev) => trust(ev.target.checked)}
            />
            {t("learnedSender.trust")}
          </label>
          <p className="hint">{t("learnedSender.trust.hint")}</p>
        </div>
      ) : null}

      {scope !== null && !confirming ? (
        <button
          ref={forgetRef}
          type="button"
          className="button button-quiet"
          disabled={busy}
          onClick={() => setConfirming(true)}
        >
          {t("learnedSender.forget")}
        </button>
      ) : null}

      {confirming ? (
        <div
          role="group"
          className="learned-sender-confirm"
          aria-labelledby={`${headingId}-q`}
          onKeyDown={(ev) => {
            if (ev.key === "Escape") {
              // Cancels the question only, not the card behind it.
              ev.stopPropagation();
              cancel();
            }
          }}
        >
          <p id={`${headingId}-q`}>{t("learnedSender.forget.confirm")}</p>
          <div className="sheet-actions">
            <button ref={keepRef} type="button" className="button button-quiet" disabled={busy} onClick={cancel}>
              {t("learnedSender.forget.keep")}
            </button>
            <button type="button" className="button button-primary" disabled={busy} onClick={forget}>
              {t("learnedSender.forget.go")}
            </button>
          </div>
        </div>
      ) : null}
    </section>
  );
}
