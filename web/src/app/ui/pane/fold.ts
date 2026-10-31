// SPDX-License-Identifier: AGPL-3.0-only

// WHICH MESSAGES OF A CONVERSATION ARE OPEN (#375 increment 5).
//
// A thread with fourteen messages in it is unreadable as fourteen full
// messages, and it is equally unreadable as fourteen folded lines — the one you
// came to read would be folded too. So the rule is small and it is here, pure,
// because it is the kind of rule that is easy to get subtly wrong and
// impossible to see wrong:
//
//   * the message you OPENED is always expanded — it is the one the URL names;
//   * every UNREAD message is expanded — unread mail in a thread is exactly the
//     part that has not been dealt with, and folding it would hide the reason
//     the thread is on screen;
//   * everything else — read, and not the one you opened — folds to one line.
//
// A SINGLE-MESSAGE THREAD IS ALWAYS EXPANDED, even if the caller passes an
// openId that is not in it (a thread loaded a moment before the pane's own
// message id caught up). One folded line and nothing else is a pane that looks
// broken, and "there is only one thing here" is never a case worth folding.
//
// EXPANDING IS NOT READING. This returns the INITIAL plan; the pane keeps its
// own set of messages the user has since clicked open, and clicking a folded
// line marks nothing read — reading state stays whatever the existing behaviour
// makes it.

import type { Id } from "../../../core/jmap/types";

/** The little a fold decision needs to know about a message. Structural, so
 * `EffectiveEmail` satisfies it and a test can write `{id, isSeen}`. */
export interface FoldableMessage {
  id: Id;
  isSeen: boolean;
}

/** The ids to render EXPANDED. Everything else in `messages` folds. */
export function foldPlan(messages: readonly FoldableMessage[], openId: Id | null): Set<Id> {
  const expanded = new Set<Id>();
  if (messages.length <= 1) {
    for (const m of messages) expanded.add(m.id);
    return expanded;
  }
  for (const m of messages) {
    if (m.id === openId || !m.isSeen) expanded.add(m.id);
  }
  return expanded;
}

/**
 * WHICH MESSAGES THE PANE SHOWS: the conversation's members, plus the message
 * that was opened if the conversation left it out.
 *
 * Thread/get leaves trashed messages out of a conversation (internal/store
 * GetThreads), because a client that was told a message is destroyed must not
 * keep finding it there. That is right for everything opened from a folder and
 * wrong for the one message opened FROM Trash or from an `is:trashed` search:
 * the pane then showed that message's live siblings and not the message itself
 * (#630). Only the opened message is added back; trashed siblings stay out.
 */
export function conversationIds(threadEmailIds: readonly Id[] | undefined, openedId: Id): Id[] {
  const ids = threadEmailIds ? [...threadEmailIds] : [];
  if (!ids.includes(openedId)) ids.push(openedId);
  return ids;
}
