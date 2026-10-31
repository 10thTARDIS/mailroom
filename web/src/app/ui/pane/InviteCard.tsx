// SPDX-License-Identifier: AGPL-3.0-only

// THE INVITATION CARD (#725): what a calendar invitation, a cancellation or an
// attendee's reply says, above the message body — and Accept / Maybe / Decline.
//
// WHAT THIS DOES NOT DO, said first because each is a hostile-content rule
// (docs/hostile-content.md §2.11):
//
//   * It never sees an .ics. The SERVER parsed the calendar (internal/ical) and
//     sends structured fields; every one is a sender-authored string and is
//     rendered as React text, bidi-stripped (§2.3b), never as markup.
//   * It never answers on its own. The only path to a REPLY is a click here,
//     and the answer goes through the ordinary submission path, so it can be
//     undone inside the Undo Send window like any other send.
//   * It never opens a URL the calendar names. There is no link on this card.
//
// ACCESSIBILITY (web/ACCESSIBILITY.md): the card is a `region` named for what it
// is and the event's title. The three answers are a `group` named "Respond to
// this invitation" of buttons whose `aria-pressed` IS the recorded answer — the
// server's, after a reload, not a local guess. The answer is announced by the
// toast (the app's one live region for outcomes), so the card adds none.

import { useState } from "react";
import type { JSX } from "react";

import type { Email } from "../../../core/jmap/types";
import { stripBidiControls } from "../../../core/hostileText";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import type { MessageKey } from "../../i18n";
import {
  formatWhen,
  inviteState,
  partstatLabel,
  personLabel,
  repeatsText,
  replySummary,
  responseText,
  unverifiedText,
} from "./invite";

type Answer = "accepted" | "tentative" | "declined";

const ANSWERS: { answer: Answer; partstat: "ACCEPTED" | "TENTATIVE" | "DECLINED"; label: MessageKey }[] = [
  { answer: "accepted", partstat: "ACCEPTED", label: "invite.accept" },
  { answer: "tentative", partstat: "TENTATIVE", label: "invite.tentative" },
  { answer: "declined", partstat: "DECLINED", label: "invite.decline" },
];

export function InviteCard({ email }: { email: Email }): JSX.Element | null {
  const { actions, offerUndo, pushToast } = useApp();
  const state = useMailState();
  const [busy, setBusy] = useState<Answer | null>(null);

  const inv = email.calendarInvite;
  if (!inv) return null;
  if (inv.problem) {
    // Present but unreadable is said, never shown as "no invitation" (a
    // bounded result reports its bound).
    return <p className="invite-problem">{t(`invite.problem.${inv.problem}` as MessageKey)}</p>;
  }

  const title = stripBidiControls(inv.summary ?? "").trim() || t("invite.noTitle");
  const variant = inv.method === "REPLY" ? "reply" : inv.cancelled ? "cancelled" : "request";
  const regionKey: MessageKey =
    variant === "reply" ? "invite.replyRegion" : variant === "cancelled" ? "invite.cancelledRegion" : "invite.region";
  const stateLine = inviteState(inv);
  const when = formatWhen(inv.start, inv.end);
  const repeats = repeatsText(inv.recurrence);
  const location = stripBidiControls(inv.location ?? "").trim();
  const organizer = personLabel(inv.organizer);
  const r = inv.response;
  // PRESSED IS THE SERVER'S RECORD FOR THIS VERSION. An answer to an earlier
  // version is reported in words (responseText) but presses nothing: the
  // organiser changed something, and the honest state is "not answered yet".
  const current = r && r.sequence >= (inv.sequence ?? 0) ? r.partstat : null;
  const notSent =
    r?.submissionId != null && state.submissions[r.submissionId]?.mailroomState === "failed";

  const respond = async (a: (typeof ANSWERS)[number]): Promise<void> => {
    setBusy(a.answer);
    const res = await actions.replyToInvite(email.threadId, email.id, a.answer);
    setBusy(null);
    if (!res) return;
    const message = t(`invite.sent.${a.partstat}` as MessageKey, { organizer });
    const holdUntil = res.sendAt ? Date.parse(res.sendAt) : Number.NaN;
    const submissionId = res.submissionId;
    if (submissionId && Number.isFinite(holdUntil) && holdUntil > Date.now()) {
      // The same undo as a sent message (Compose.tsx): the toast counts down
      // the server's own hold window, and Ctrl/⌘+Z reaches it too.
      offerUndo(message, () => actions.withdrawInviteReply(email.threadId, email.id, submissionId), holdUntil - Date.now());
    } else {
      pushToast({ kind: "info", message });
    }
  };

  const attendees = inv.attendees ?? [];
  const omitted = inv.attendeesOmitted ?? 0;

  return (
    <section className={`invite-card invite-${variant}`} aria-label={`${t(regionKey)}: ${title}`}>
      <h2 className="invite-title">{title}</h2>
      {stateLine ? (
        <p className={`invite-state invite-state-${stateLine}`}>{t(`invite.state.${stateLine}` as MessageKey)}</p>
      ) : null}
      {unverifiedText(inv) ? <p className="invite-state invite-unverified">{unverifiedText(inv)}</p> : null}

      {variant === "reply" ? (
        <ul className="invite-replied">
          {replySummary(attendees).map((line, i) => (
            <li key={i}>{line}</li>
          ))}
        </ul>
      ) : null}

      <dl className="invite-facts">
        {when ? (
          <>
            <dt>{t("invite.when")}</dt>
            <dd>{when}</dd>
          </>
        ) : null}
        {repeats ? (
          <>
            <dt>{t("invite.repeatsLabel")}</dt>
            <dd>{repeats}</dd>
          </>
        ) : null}
        {location ? (
          <>
            <dt>{t("invite.where")}</dt>
            <dd className="invite-location">{location}</dd>
          </>
        ) : null}
        {organizer ? (
          <>
            <dt>{t("invite.organizer")}</dt>
            <dd className="invite-person">{organizer}</dd>
          </>
        ) : null}
        {variant !== "reply" && attendees.length > 0 ? (
          <>
            <dt>{t("invite.guests")}</dt>
            <dd>
              <ul className="invite-guests">
                {attendees.map((a) => (
                  <li key={a.email}>
                    <span className="invite-person">{personLabel(a)}</span>{" "}
                    <span className="invite-partstat">— {partstatLabel(a.partstat)}</span>
                  </li>
                ))}
                {omitted > 0 ? <li>{t("invite.guestsOmitted", { count: omitted })}</li> : null}
              </ul>
            </dd>
          </>
        ) : null}
      </dl>

      {variant === "request" ? (
        <>
          {responseText(inv) ? <p className="invite-response">{responseText(inv)}</p> : null}
          {notSent ? <p className="invite-response invite-not-sent">{t("invite.replyNotSent")}</p> : null}
          {inv.canReply ? (
            <>
              <div className="invite-actions" role="group" aria-label={t("invite.respond")}>
                {ANSWERS.map((a) => (
                  <button
                    key={a.answer}
                    type="button"
                    className={`button ${current === a.partstat ? "button-primary" : "button-quiet"}`}
                    aria-pressed={current === a.partstat}
                    disabled={busy !== null}
                    onClick={() => void respond(a)}
                  >
                    {t(a.label)}
                  </button>
                ))}
              </div>
              <p className="field-hint">
                {busy !== null
                  ? t("invite.sending")
                  : t("invite.explainSend", { organizer, from: stripBidiControls(email.deliveredTo ?? "") })}
              </p>
            </>
          ) : inv.cannotReplyReason && inv.cannotReplyReason !== "cancelled" && inv.cannotReplyReason !== "notAnInvitation" ? (
            <p className="field-hint">{t(`invite.cannot.${inv.cannotReplyReason}` as MessageKey)}</p>
          ) : null}
        </>
      ) : null}
    </section>
  );
}
