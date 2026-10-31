// SPDX-License-Identifier: AGPL-3.0-only

// THE OUTBOX (#448, design pass-2 §8): `#/outbox`.
//
// The rail has carried an Outbox COUNT since #375 and had nothing behind it — the
// entry pointed at Sent with a TODO saying no view listed submissions. This is
// that view.
//
// IT IS NOT A MESSAGE LIST, and that is why it is its own route rather than a
// `View` the list route renders. Its rows are `EmailSubmission` objects — jobs with
// a fire-at time — which no `Email/query` can express, so `planFor` gives the
// outbox view no filters at all and the sync layer asks the server for nothing
// (core/views.ts). What the rows need instead is the DRAFT behind each submission,
// fetched in ONE batch for the ids the store does not already hold.
//
// THE ARITHMETIC IS ui/outbox.ts, tested: which submissions belong here, in what
// order, and whether a row reads as a countdown or as a schedule. Nothing on this
// screen is computed twice.
//
// IT KEEPS ITSELF CURRENT FROM TWO CLOCKS, and they are different clocks:
//   * the SERVER's, through the EmailSubmission change loop, which is already
//     polled and pushed (core/sync.ts) — that is what makes a send disappear from
//     here when it goes;
//   * the LOCAL one, a one-second tick, because "Sending in 14 s" is a countdown
//     and a row that only re-rendered when the server said something would sit at
//     14 for the whole window.

import { useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import { formatWake } from "../../../core/snooze";
import { withCompose } from "../../../core/routes";
import type { Id } from "../../../core/jmap/types";
import { navigate, useRoute } from "../../routing";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import { outboxMissingEmailIds, outboxRows } from "../outbox";
import type { OutboxRow } from "../outbox";
import { startOAuth } from "../oauthReturn";
import { providerName, startErrorText } from "../oauthWords";
import { useAccounts } from "../useAccounts";

/** The countdown's granularity. One second, because that is what the sentence
 * says. */
const TICK_MS = 1_000;

export function OutboxRoute(): JSX.Element {
  const state = useMailState();
  const route = useRoute();
  const { actions, sync, pushToast, offerUndo, client } = useApp();
  // Waiting rows name their account and reconnect it (#52).
  const { byId: accountsById } = useAccounts();
  const [now, setNow] = useState(() => Date.now());
  const [busy, setBusy] = useState<Id | null>(null);

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), TICK_MS);
    return () => clearInterval(timer);
  }, []);

  const rows = useMemo(
    () => outboxRows(state.submissions, state.emails, state.identities, now),
    [state.submissions, state.emails, state.identities, now],
  );

  /** The drafts already asked for, so the batch below runs once per id rather than
   * once per second. */
  const asked = useRef<Set<Id>>(new Set());

  /** The sentence for a waiting row (#52). */
  const waitingText = (row: OutboxRow): string => {
    const entry = row.accountId ? accountsById.get(row.accountId) : undefined;
    if (row.state.kind === "waiting" && row.state.why === "provider") {
      return t("outbox.waitingProvider", { provider: providerName(entry?.record?.authProvider) });
    }
    return entry ? t("outbox.waitingReconnect", { account: entry.name }) : t("outbox.waitingReconnectUnnamed");
  };

  /** Starts the sign-in that releases a waiting row (#52). */
  const reconnect = (row: OutboxRow): void => {
    const entry = row.accountId ? accountsById.get(row.accountId) : undefined;
    const provider = entry?.record?.authProvider ?? "";
    setBusy(row.id);
    void startOAuth(client, provider, row.accountId ?? undefined).then((err) => {
      if (err === null) return; // leaving for the provider
      setBusy(null);
      pushToast({ kind: "error", message: startErrorText(err, provider) });
    });
  };
  useEffect(() => {
    const missing = outboxMissingEmailIds(rows).filter((id) => !asked.current.has(id));
    if (missing.length === 0) return;
    for (const id of missing) asked.current.add(id);
    void sync.loadEmails(missing);
  }, [rows, sync]);

  const cancel = async (row: OutboxRow): Promise<void> => {
    setBusy(row.id);
    await actions.cancelSubmission(row.id);
    setBusy(null);
  };

  const dismiss = async (row: OutboxRow): Promise<void> => {
    setBusy(row.id);
    const ok = await actions.dismissSubmission(row.id);
    setBusy(null);
    if (ok) pushToast({ kind: "info", message: t("outbox.dismissed") });
  };

  const sendNow = async (row: OutboxRow): Promise<void> => {
    setBusy(row.id);
    const res = await actions.sendNow(row.id);
    setBusy(null);
    if (!res) return; // sendNow has already surfaced the server's own words
    // THE NEW SUBMISSION'S OWN WINDOW, from the server's `sendAt` — the same number
    // the composer's send toast counts down, and for the same reason: the hold
    // window is the server's setting, and a client-computed deadline would run out
    // while the message was still cancellable.
    const holdUntil = res.sendAt ? Date.parse(res.sendAt) : Number.NaN;
    const submissionId = res.submissionId;
    if (submissionId && Number.isFinite(holdUntil) && holdUntil > Date.now()) {
      offerUndo(
        t("outbox.sendingNow"),
        async () => actions.cancelSubmission(submissionId),
        holdUntil - Date.now(),
      );
      return;
    }
    pushToast({ kind: "info", message: t("outbox.sendingNow") });
  };

  return (
    <main className="content" id="messages" tabIndex={-1}>
      <section className="list-pane outbox-pane" aria-label={t("outbox.pane")}>
        <header className="outbox-header">
          <h1>{t("outbox.title")}</h1>
        </header>
        {rows.length === 0 ? (
          <p className="outbox-empty">{t("outbox.empty")}</p>
        ) : (
          <ul className="outbox-list" aria-label={t("outbox.rows")}>
            {rows.map((row) => (
              <li key={row.id} className="outbox-row">
                <p className="outbox-row-who">
                  {/* NOT LOADED IS SAID, NOT HIDDEN. The submission is what has not
                      gone; waiting for its draft before drawing the row would
                      under-report what is waiting, and drawing an empty message
                      would claim it has no recipients. */}
                  {row.loaded ? (
                    <span className="outbox-row-recipients">
                      {row.recipients.length > 0
                        ? row.recipients.join(", ")
                        : t("outbox.noRecipients")}
                    </span>
                  ) : (
                    <span className="outbox-row-unloaded">{t("row.notLoaded")}</span>
                  )}
                  {row.alias ? (
                    <span className="outbox-row-alias">{t("outbox.as", { alias: row.alias })}</span>
                  ) : null}
                </p>
                <p className="outbox-row-subject">
                  {row.subject ?? t("outbox.noSubject")}
                </p>
                <p
                  className={
                    row.state.kind === "failed"
                      ? "outbox-row-state outbox-row-state-failed"
                      : "outbox-row-state"
                  }
                >
                  {row.state.kind === "window"
                    ? t("outbox.sendingIn", { seconds: row.state.secondsLeft })
                    : row.state.kind === "scheduled"
                      ? t("outbox.scheduled", { when: formatWake(row.state.sendAt) })
                      : row.state.kind === "sending"
                        ? t("outbox.sending")
                        : row.state.kind === "waiting"
                          ? waitingText(row)
                          : t("outbox.failed")}
                </p>
                {/* THE PROVIDER'S OWN WORDS, on their own line. A generic
                    "send failed" would have left the operator unable to tell
                    whether the cause was the From address, the recipient, the
                    network or Mailroom — the verbatim reply is the whole
                    diagnostic value of this row (#589). */}
                {row.state.kind === "failed" ? (
                  <>
                    <p className="outbox-row-reason">
                      {row.state.reply
                        ? t("outbox.failedReply", { reply: row.state.reply })
                        : t("outbox.failedNoReply")}
                    </p>
                    <p className="outbox-row-reason">{t("outbox.failedNext")}</p>
                  </>
                ) : null}
                <p className="outbox-row-actions">
                  {/* WORDS ONLY, and the word differs with the state: inside the
                      hold window the act is UNDO (take back something already
                      going), outside it the act is CANCEL (call off something
                      arranged). Same server call — the only legal EmailSubmission
                      update — and two different things to a reader.

                      ⚠ NEITHER IS OFFERED ON A FAILED ROW. The server accepts
                      `undoStatus: "canceled"` only while the job is still
                      pending, and a failed submission is final — so the button
                      could not do what it says, which is the same objection this
                      file already makes about listing `final` rows at all. The
                      other three states ARE pending, including `sending`: a
                      submission requeued after a transient failure is genuinely
                      still cancellable, and the server is the authority on the
                      race. */}
                  {/* A FAILED ROW GETS THE ONE ACTION ITS OWN SENTENCE ASKS FOR
                      (#604). It says "It is still in Drafts. Change what the
                      server objected to before sending again" — and then, until
                      this button, offered nothing: the row named a problem, told
                      the user to go and fix it, and made them find the message
                      themselves in another view. Every other state has an action
                      here; failure, the state that most needs one, had none.

                      It opens the DRAFT rather than showing the message here.
                      This view is not a message list (see the header), and
                      reading is not the act required — a rejected sender cannot
                      be fixed by looking at the body. Edit is the act, so the
                      row is a route into the composer. */}
                  {/* DISMISS, beside Edit (migration 00059): the act for a failure
                      the user does NOT mean to re-send. Before it, a refused send
                      could never leave — destroy is refused, and discarding the
                      draft only soft-trashes it — so the row and the rail's count
                      stayed for good. It hides the failure; it deletes nothing,
                      the draft included, so Edit stays possible from Drafts. */}
                  {row.state.kind === "failed" ? (
                    <>
                      <button
                        type="button"
                        className="button"
                        onClick={() => navigate(withCompose(route, { draft: row.emailId }))}
                      >
                        {t("outbox.failedEdit")}
                      </button>
                      <button
                        type="button"
                        className="button button-quiet"
                        disabled={busy === row.id}
                        onClick={() => void dismiss(row)}
                      >
                        {t("outbox.dismiss")}
                      </button>
                    </>
                  ) : (
                    <button
                      type="button"
                      className="button button-quiet"
                      disabled={busy === row.id}
                      onClick={() => void cancel(row)}
                    >
                      {row.state.kind === "window" ? t("outbox.undo") : t("outbox.cancel")}
                    </button>
                  )}
                  {/* SEND NOW IS OFFERED ON A SCHEDULE ONLY. Inside the hold window
                      it would mean "hurry up by a few seconds", at the cost of
                      cancelling and re-creating a submission — a real risk of
                      double-sending for no benefit. */}
                  {row.state.kind === "scheduled" ? (
                    <button
                      type="button"
                      className="button"
                      disabled={busy === row.id}
                      onClick={() => void sendNow(row)}
                    >
                      {t("outbox.sendNow")}
                    </button>
                  ) : null}
                  {/* RECONNECT, on a message waiting for its account (#52): the
                      one act that lets it go. Not on a provider wait — that is
                      the operator's to fix, and offering a sign-in would say
                      otherwise. */}
                  {row.state.kind === "waiting" && row.state.why === "reconnect" && row.accountId ? (
                    <button
                      type="button"
                      className="button"
                      disabled={busy === row.id}
                      onClick={() => reconnect(row)}
                    >
                      {t("outbox.reconnect")}
                    </button>
                  ) : null}
                </p>
              </li>
            ))}
          </ul>
        )}
      </section>
    </main>
  );
}
