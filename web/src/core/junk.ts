// SPDX-License-Identifier: AGPL-3.0-only

// "NOT JUNK" (#528, spec §6): which messages are in Junk, and what rescuing one
// sends.
//
// Spec §6 is unusually firm: "Junk is read-only with one exception: 'Not junk'
// must write back (move to INBOX) so users can rescue false positives." The
// server half has been right since #351 (internal/api/email.go, the
// store.KeywordJunk case of parseEmailPatch's setKeyword). This file is the DOM-free half of the
// client, so both of the things below can be tested without a browser.
//
// ⚠ JUNK IS WHERE A MESSAGE IS, NOT A KEYWORD IT CARRIES — ON THE PROVIDER WE
// ACTUALLY RUN AGAINST. `$junk` on an Email is derived from app.messages.is_junk,
// which ingest sets ONLY from a `Junk`/`$Junk` IMAP keyword (ingest.mapFlags).
// Migadu files spam into the \Junk folder and tags nothing: measured on the test
// instance 2026-09-15, 10 of 10 messages in \Junk had is_junk = false and no
// junk keyword at all. A "Not junk" control gated on the keyword would therefore
// never have rendered for a single real message. So placement is the primary
// signal, and the keyword the second one, for providers that do tag it.

import type { Id, PatchObject } from "./jmap/types";
import { KEYWORD_JUNK } from "./jmap/types";
import type { MailState } from "./store";
import { effectiveEmail } from "./store";

/**
 * Whether a message is in Junk right now, as this client sees it.
 *
 * A pending LOCAL MOVE wins over what the server last said: a message the user
 * has just rescued (or trashed) is not in Junk any more, whatever `mailboxIds`
 * still reads until the write lands. Without that, the pane would keep offering
 * "Not junk" for the message it just moved.
 *
 * ANY copy counts. A message can live in several accounts, and if one copy is
 * in Junk the rescue is meaningful: the server moves every account's copy by
 * ROLE (internal/api/email.go setMailbox).
 */
export function isInJunk(state: MailState, id: Id): boolean {
  const e = effectiveEmail(state, id);
  if (!e) return false;
  if (e.localMove !== null) return false;
  if (e.keywords[KEYWORD_JUNK] === true) return true;
  return Object.keys(e.mailboxIds).some((mb) => state.mailboxes[mb]?.role === "junk");
}

/**
 * The Email/set patch that rescues a message: clear the verdict AND name the
 * destination.
 *
 * ⚠ THE `mailboxIds/` KEY IS NOT REDUNDANT, even though the server alone would
 * supply the inbox move from `keywords/$junk: false`. The offline queue rolls a
 * refused action back BY PATCH KEY (core/actions.ts `revert`: `keywords/*`
 * reverts a keyword, `mailboxIds/*` reverts a move). The rescue shows as an
 * optimistic MOVE, so a patch without a mailbox key would, on refusal, revert a
 * keyword that was never set and leave the move in place — the message gone
 * from Junk, in no list at all, until a reload. Naming the inbox makes the
 * rollback structural rather than a special case.
 *
 * The server resolves the two keys to the same move in either order: setKeyword
 * only supplies MoveToRole "inbox" when no destination has been named, and
 * setMailbox names this one.
 *
 * The `keywords/$junk` removal stays in the patch too: on a provider that DOES tag
 * junk it is what clears is_junk, and a message moved to the inbox but still
 * marked junk would be excluded from the desk.
 */
export function notJunkPatch(inboxId: Id): PatchObject {
  return {
    // `null`, not `false`: a JMAP patch removes a keyword by nulling it (keyword
    // values are only ever `true`), exactly as MailActions' private `keyword`
    // does for every other keyword removal.
    [`keywords/${KEYWORD_JUNK}`]: null,
    [`mailboxIds/${inboxId}`]: true,
  };
}
