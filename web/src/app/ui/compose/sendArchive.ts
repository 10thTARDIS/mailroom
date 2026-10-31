// SPDX-License-Identifier: AGPL-3.0-only

// SEND & ARCHIVE (#648): decide WHICH intents offer it, what the send asks the
// server to archive, and what the toast may honestly say — kept pure and out of
// Compose.tsx, per this project's no-DOM-harness rule.
//
// WHICH INTENTS OFFER IT: a reply, reply-all or forward has a PARENT — a
// conversation to close out. A brand-new message does not, and neither does a
// resumed draft: Compose.tsx's `{kind: "draft"; draft: EffectiveEmail}` never
// carries the parent the draft may once have been a reply to (nothing writes
// one when a reply is autosaved — internal/api/draft.go's Email/set create has
// no field for it), so there is no conversation this composer knows to
// archive. Treating a draft like `new` is therefore not a shortcut; it is the
// only conversation this code can name.
//
// ⚠ THE ARCHIVE IS THE SERVER'S, AND IT HAPPENS ONLY ONCE THE MESSAGE IS SENT.
// v1.80.0 archived from here, the moment `EmailSubmission/set` ACCEPTED the
// send, and undid it on Undo Send. Accepting is not sending: the submission is
// queued behind the undo window and can still fail. In practice a
// reply the provider then refused permanently (550 5.7.1) left its conversation
// in Archive with the reply never sent. So the send now carries
// `archiveOnSuccess: [parent]` and the SEND WORKER archives, in the transaction
// that records the submission as sent (internal/compose/send.go). A failure, an
// Undo Send or exhausted retries never reach that point, so there is nothing to
// put back — which is why the old "restore on undo" half is gone.
//
// The cost is honesty in the toast: the conversation stays visible until the
// message has actually gone, so the toast says it WILL be archived.

/** The subset of `ComposeIntent["kind"]` this decision needs — not imported
 * from Compose.tsx to avoid a value/type import back into the component this
 * module is extracted from. */
export type ComposeIntentKind = "new" | "reply" | "replyAll" | "forward" | "draft";

/**
 * Whether the message being answered is IN THE INBOX, as the client sees it
 * now: a local move the server has not confirmed yet decides (the user just
 * archived or restored it), otherwise the server's own `mailboxIds` must include
 * an inbox-role folder. Mailboxes not loaded yet read as "not in the Inbox",
 * which only ever withholds the button.
 */
export function parentInInbox(
  parent: { mailboxIds?: Record<string, boolean> | null; localMove?: "archive" | "trash" | "inbox" | "junk" | null } | null,
  mailboxes: Readonly<Record<string, { role?: string | null } | undefined>>,
): boolean {
  if (!parent) return false;
  if (parent.localMove) return parent.localMove === "inbox";
  return Object.keys(parent.mailboxIds ?? {}).some((id) => mailboxes[id]?.role === "inbox");
}

/**
 * Whether Send & archive is offered (#648): a reply, reply-all or forward
 * WHOSE PARENT IS IN THE INBOX.
 *
 * ⚠ THE INBOX CONDITION IS THE POINT. "Send & archive" means "answered, clear
 * it from my Inbox"; with nothing in the Inbox to clear, the plain Send is the
 * whole action — and replying to your own message from Sent must never move the
 * sent original out of Sent. This gate is ADVISORY: the server re-checks when
 * the message is actually sent and skips a parent that has left the Inbox in
 * the meantime (internal/store/send_archive.go), so a stale answer here can
 * only ever offer a button whose archive then does nothing, never move mail
 * the user has already filed.
 */
export function offersSendAndArchive(kind: ComposeIntentKind, parentInInbox: boolean): boolean {
  return parentInInbox && (kind === "reply" || kind === "replyAll" || kind === "forward");
}

/** What the send asks the server to archive once it has gone: the parent, when
 * Send & archive was used and there is one; otherwise nothing. */
export function archiveOnSuccessFor(archive: boolean, parentId: string | null): string[] | undefined {
  return archive && parentId ? [parentId] : undefined;
}

/**
 * The toast after a send (#648). "Archived" is never said: at this point the
 * message has not gone, and the archive happens only once it has. Promising
 * even the FUTURE archive needs the server's own word — `archivePending`, its
 * echo of `archiveOnSuccess` — because a server that ignored the request would
 * otherwise have this client announce an archive nobody will perform.
 */
export function sentToastKey(archivePending: boolean): "compose.sentWillArchive" | "compose.sent" {
  return archivePending ? "compose.sentWillArchive" : "compose.sent";
}

/** The toast for a send held offline (#62). The archive request travels with the
 * held send, so it too happens only once the message has actually gone. */
export function queuedToastKey(archiveRequested: boolean): "compose.queuedWillArchive" | "compose.queued" {
  return archiveRequested ? "compose.queuedWillArchive" : "compose.queued";
}

// ---------------------------------------------------------------------------
// THE READING PANE AFTER SEND & ARCHIVE (#648).
//
// The report: after Send & archive the conversation left the desk
// list, and the reading pane stayed open on it. Archiving with `e` does not do
// that — it moves the pane on (ui/useListVerbs.ts `advancePastOpen`: the next
// message, else the previous, else the list). Send & archive is the same
// gesture, "this is done", so it now ends the same way, by calling that same
// helper rather than a second copy of the decision.
//
// IT HAPPENS ON SEND, NOT WHEN THE SERVER ARCHIVES. The archive runs once the
// message has actually gone, after the Undo-Send window, and the toast already
// says the conversation "will be archived". Waiting for it would leave the user
// looking at the thread for the whole window. Undo Send reopens nothing: it
// cancels the send (and with it the archive), and the user is wherever the pane
// moved to, exactly as after `e` followed by a navigation.
//
// TWO HALVES, BECAUSE TWO COMPONENTS. The composer knows WHAT was sent and
// whether the open pane is its conversation; only the list surface knows the
// list order `advancePastOpen` walks. So the composer decides (`paneToLeaveAfterSend`)
// and leaves a note; the surface acts on it once the composer has CLOSED
// (`paneLeaveVerdict`) — acting while the composer is still in the URL would
// navigate the composer along with the pane, and closing it would then go back.

/** What the reading pane is showing: the open message and its conversation. */
export interface OpenPane {
  id: string | null;
  threadId: string | null;
}

/**
 * Which open message a send should move the pane off, or null to leave the
 * pane alone.
 *
 * `archiving` is true only for a Send & archive the client ACCEPTED: sent now
 * with the server's echo of the archive (`archivePending` — the same word the
 * toast waits for), or held offline with the archive travelling with it. A
 * plain Send, a refused send, or a server that ignored the archive request
 * pass false, and the pane stays where it is.
 *
 * The pane moves only when it is showing the PARENT or another message of the
 * parent's conversation. A composer opened elsewhere — the open pane is some
 * other thread — has nothing to do with what is on screen, and leaves it be.
 */
export function paneToLeaveAfterSend(opts: {
  archiving: boolean;
  parent: { id: string; threadId?: string | null } | null;
  open: OpenPane;
}): string | null {
  const { archiving, parent, open } = opts;
  if (!archiving || !parent || !open.id) return null;
  if (open.id === parent.id) return open.id;
  if (parent.threadId && open.threadId === parent.threadId) return open.id;
  return null;
}

/** A note from the composer: move the pane off `openId` once the composer has
 * closed. `at` is when it was written. */
export interface PendingPaneLeave {
  openId: string;
  at: number;
}

/** How long a note is honoured. Closing the composer is a navigation that
 * lands in milliseconds; a note nobody acted on by now belongs to a surface
 * that was not mounted, and must not fire later on a message the user opens
 * again. */
export const PANE_LEAVE_TTL_MS = 10_000;

/**
 * What a list surface does with the composer's note, now.
 *
 *   "none"  — there is no note.
 *   "wait"  — the composer is still open; act once it has closed.
 *   "leave" — move the pane on, as Archive does.
 *   "drop"  — discard the note: it is stale, or the pane is no longer on the
 *             message it names (the user moved first), so it no longer
 *             describes what is on screen.
 */
export type PaneLeaveVerdict = "none" | "wait" | "leave" | "drop";

export function paneLeaveVerdict(
  pending: PendingPaneLeave | null,
  now: { openId: string | null; composing: boolean; at: number },
): PaneLeaveVerdict {
  if (!pending) return "none";
  if (now.at - pending.at > PANE_LEAVE_TTL_MS) return "drop";
  if (now.composing) return "wait";
  return now.openId === pending.openId ? "leave" : "drop";
}
