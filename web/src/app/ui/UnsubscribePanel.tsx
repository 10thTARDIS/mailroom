// SPDX-License-Identifier: AGPL-3.0-only

// UNSUBSCRIBE DETECTION, SURFACED (#58, matrix §6).
//
// Most of the mail in a typical mailbox carries List-Unsubscribe,
// so this is not a corner case — it is a control that belongs on two thirds of what
// arrives, which is exactly why it must be honest about what it does.
//
// WHAT THIS DOES NOT DO. It never unsubscribes on its own. RFC 8058 one-click is
// deliberately unimplemented: a POST issued by the server to a URL out of a message
// would be Mailroom's first outbound fetch on behalf of a message
// (docs/hostile-content.md §2.3), pointed at an address a stranger chose. So there are
// exactly two routes, and both are ones the product already has:
//
//   * The LINK is opened by the user's own browser in a new tab. Not fetched by the app,
//     not fetched by the server; `noopener noreferrer` so the opened page can neither
//     reach back into this window nor be told where it was opened from.
//   * The MAILTO is sent through the ordinary compose path from the user's own identity,
//     which is the same door every other outgoing message uses.
//
// IT ASKS FIRST, and the confirmation is not ceremony. Clicking unsubscribe confirms to
// the sender that the address is live and being read, which for a sender who did not have
// consent in the first place is worth more than the unsubscribe. A user is entitled to
// know that before they press it, in the same spirit as the remote-content banner
// (§2.3) saying what loading an image reveals.

import { useMemo, useState } from "react";
import type { JSX } from "react";

import type { EffectiveEmail } from "../../core/store";
import { useApp, useMailState } from "../state";
import { t } from "../i18n";

export function UnsubscribePanel({ email }: { email: EffectiveEmail }): JSX.Element | null {
  const { actions, pushToast } = useApp();
  const state = useMailState();
  const [confirming, setConfirming] = useState(false);
  const [sending, setSending] = useState(false);

  // WHICH ADDRESS THE UNSUBSCRIBE COMES FROM decides whether it works at all: a list
  // keyed on the alias it mails will not recognise a request from a different address
  // (spec §5's alias model). So it is the address this message was DELIVERED TO where
  // we know that, and a concrete identity otherwise.
  const from = useMemo(() => {
    if (email.deliveredTo) return email.deliveredTo;
    const concrete = Object.values(state.identities).find((i) => i.enabled && !i.isWildcard);
    return concrete?.email ?? "";
  }, [email.deliveredTo, state.identities]);

  const unsub = email.unsubscribe;
  // Absent, or present with neither route usable: no control. The server already
  // refused everything it could not vouch for, so there is nothing to fall back to
  // and nothing to apologise for — a message with no unsubscribe simply has none.
  if (!unsub || (unsub.url === "" && !unsub.email)) return null;

  const sendUnsubscribeMail = async (): Promise<void> => {
    if (!unsub.email || from === "") return;
    setSending(true);
    const ok = await actions.sendMessage({
      from: { name: null, email: from },
      to: [{ name: null, email: unsub.email.to }],
      // The subject is the sender's token, carried verbatim. Editing it would
      // produce a message that arrives and does nothing.
      subject: unsub.email.subject,
      text: unsub.email.body || t("unsubscribe.defaultBody"),
    });
    setSending(false);
    setConfirming(false);
    // A queued request is neither sent nor lost (#62), and this surface in
    // particular must not overstate: the user is deciding whether they are now
    // unsubscribed.
    if (ok) {
      pushToast({
        kind: "info",
        message: t(ok.queued ? "unsubscribe.queued" : "unsubscribe.sent"),
      });
    }
  };

  return (
    <div className="unsubscribe-panel">
      {!confirming ? (
        <button
          type="button"
          className="button button-quiet"
          aria-expanded={false}
          onClick={() => setConfirming(true)}
        >
          {t("unsubscribe.open")}
        </button>
      ) : (
        <div className="unsubscribe-confirm" role="group" aria-label={t("unsubscribe.open")}>
          {/* Said before the click, not after: an unsubscribe is a reply, and a
              reply tells the sender the address is real. */}
          <p className="field-hint">{t("unsubscribe.explain")}</p>

          {unsub.url ? (
            <>
              {/* A real anchor, not a scripted window.open: the user can see where it
                  goes, middle-click it, or copy it — and the app never navigates on
                  their behalf. The host is shown separately because the URL itself is
                  usually an opaque token nobody can read. */}
              <a
                className="button button-quiet"
                href={unsub.url}
                target="_blank"
                rel="noopener noreferrer"
                onClick={() => setConfirming(false)}
              >
                {t("unsubscribe.openLink", { host: hostOf(unsub.url) })}
              </a>
            </>
          ) : null}

          {/* The mail route needs an address to send FROM. With no usable identity
              there is nothing to offer rather than a button that would fail — and the
              link route, if there is one, still works. */}
          {unsub.email && from !== "" ? (
            <button
              type="button"
              className="button button-quiet"
              disabled={sending}
              onClick={() => void sendUnsubscribeMail()}
            >
              {sending
                ? t("unsubscribe.sending")
                : t("unsubscribe.sendMail", { to: unsub.email.to })}
            </button>
          ) : null}

          <button type="button" className="button-quiet" onClick={() => setConfirming(false)}>
            {t("action.cancel")}
          </button>
        </div>
      )}
    </div>
  );
}

/** The host of an already-validated https URL, for display.
 *
 * The server guaranteed a parseable https URL with a host and no userinfo, so this cannot
 * mislead — but it still cannot throw, because a display helper that throws would take
 * the whole reading pane down over a label. */
function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}
