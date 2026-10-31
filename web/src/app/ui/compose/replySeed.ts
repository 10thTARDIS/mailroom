// SPDX-License-Identifier: AGPL-3.0-only

// THE COMPOSER'S INITIAL To/Cc FOR A REPLY OR REPLY-ALL (#646).
//
// Pulled out of Compose.tsx so `replyRecipients` (./recipients.ts) is asked
// exactly ONCE per composer. `to` and `cc` are separate pieces of React state
// with separate lazy initializers, and each used to compute its own half of
// the same decision — which is the "two callers of one question" shape
// CLAUDE.md's measurement rules warn about, not a correctness bug today, but
// one call site away from becoming one the moment either initializer's logic
// drifts from the other's.
//
// A pure function, like the rest of compose's decisions: there is no DOM test
// harness in this project, so the part worth pinning is kept clear of JSX.

import type { Identity } from "../../../core/jmap/types";
import type { Chip } from "./chips";
import { replyRecipients, type ReplyMode, type ReplyParent } from "./recipients";

export interface ReplySeed {
  to: Chip[];
  cc: Chip[];
}

const EMPTY: ReplySeed = { to: [], cc: [] };

/**
 * `to`/`cc` a reply or reply-all composer opens with.
 *
 * `kind` is null for every intent that is not a reply (a blank message, a
 * forward, a resumed draft) — each of those seeds its own fields elsewhere in
 * Compose.tsx, so this returns the empty seed rather than guessing. `parent`
 * is null only while the message a reply is ABOUT has not resolved yet
 * (ComposeRoute's readiness check keeps the composer from mounting before it
 * has, so in practice this composer never sees that combination — the null
 * case exists so the type says what would happen rather than asserting it away).
 */
export function initialReplyFields(
  kind: ReplyMode | null,
  parent: ReplyParent | null,
  identities: readonly Identity[],
): ReplySeed {
  if (!kind || !parent) return EMPTY;
  return replyRecipients(parent, identities, kind);
}
