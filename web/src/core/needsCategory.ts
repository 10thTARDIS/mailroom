// SPDX-License-Identifier: AGPL-3.0-only

// Grouping for the needs-a-category queue (#176, epic #156). DOM-free and pure,
// like core/views.ts — this is the part worth being able to reason about without
// a browser.
//
// WHY GROUP AT ALL. The queue's job is not to be cleared; it is to grow the
// labelled corpus, which is the actual bottleneck (a handful of hand-labelled
// messages out of hundreds — nothing about a classifier is measurable at that n). One answer per
// message is linear work for the user and linear evidence for the system, and at
// that exchange rate nobody ever gets to a corpus. One answer for twenty-four
// messages that all arrived at `bank@` is the same single decision and
// twenty-four times the evidence, and it is also — not incidentally — a better
// decision, because the person is answering about a SERVICE they recognise
// rather than about a message they have to read first.
//
// ALIAS FIRST, SENDER DOMAIN SECOND. Under a catchall the alias is the strongest
// key available: it is chosen by the user (they gave that address to exactly one
// service), it survives the sender changing its From, and it is what
// rules.Induce will end up keying the resulting rule on anyway (#177). But it is
// not universal — `brand.example` and `news.example` reach a mailbox with no
// distinguishing alias, and a user without a catchall has no aliases at all. The
// domain fallback is what keeps this a working surface for them instead of a
// flat list with a feature they cannot use.
//
// THE GROUPS ARE A PROPOSAL, NOT A CLAIM. A group says "these arrived the same
// way", which is an observation about envelopes, not a judgement that they
// belong to one category. The user is free to label the group, part of it, or
// nothing — and the induced rule that follows is scored against real mail
// (Covers / Collateral) before it is accepted. Nothing here decides anything.

import type { EffectiveEmail } from "./store";
import type { Id } from "./jmap/types";

/** How a group was keyed — which decides how it describes itself, and how much
 * weight its proposal deserves. */
export type QueueGroupKind = "alias" | "domain";

export interface QueueGroup {
  /** Stable identity for React keys and for "which group did the user act on".
   * Prefixed by kind so an alias `news@example.com` and a domain
   * `news@example.com` (impossible today, but not by construction) cannot
   * collide. */
  key: string;
  kind: QueueGroupKind;
  /** What to show: the alias's local part with its `@` (`bank@`), or the
   * sender's domain (`brand.example`). Short on purpose — the header is a
   * question, not a data dump. */
  display: string;
  /** The messages, most-confident first. */
  ids: Id[];
  /** The best confidence in the group — what the group is ordered by. */
  topConfidence: number;
  /**
   * How many distinct sender domains the group spans — whether this batch is
   * ONE service or a crowd.
   *
   * It is here because the alias is not always a discriminator, and the failure
   * is invisible without it. On a real mailbox the largest alias
   * group is `forward@` with a great many messages from dozens of unrelated senders: an
   * address used to FORWARD mail in, not an address given to one service. A
   * header that said "120 messages arrived at forward@ — where do these go?" and
   * nothing else would be inviting a single label onto a hundred unrelated
   * messages.
   *
   * This is the same distinction #175 landed on for rule collateral: what
   * separates a good binding from a bad one is whether what it sweeps up is
   * HOMOGENEOUS. So the number is shown, and the person decides. It is never
   * used to suppress a group — an operator who genuinely wants one label on a
   * mixed group is allowed to say so.
   */
  senderCount: number;
}

/**
 * The alias a message was delivered to, as a display string, or null.
 *
 * `deliveredTo` is the address the sender actually used — post-#177 that is the
 * per-service alias rather than the mailbox everything collapsed onto. The
 * DOMAIN is dropped from the display because under a catchall it is the same for
 * every group and repeating it would bury the only part that differs.
 *
 * Returns null for an address with no local part, which is not a real alias and
 * would otherwise group unrelated mail under a bare "@".
 */
export function aliasOf(email: { deliveredTo?: string | null }): string | null {
  const addr = (email.deliveredTo ?? "").trim().toLowerCase();
  const at = addr.lastIndexOf("@");
  if (at <= 0) return null;
  return addr.slice(0, at) + "@";
}

/**
 * The sender's domain, or null when the message has no usable From.
 *
 * Deliberately the FULL domain, not a registrable-domain guess: without a public
 * suffix list, "strip to the last two labels" is wrong for `co.uk` and wrong for
 * every brand that segregates bulk onto `e.shop.example` — and the second is exactly
 * the mail this queue is full of (#175). Grouping `e.shop.example` separately from
 * `shop.example` is the honest answer when we cannot tell whether they are one
 * service or two; the user can still label both groups the same way in two
 * keystrokes.
 */
export function senderDomainOf(email: { from?: { email?: string }[] | null }): string | null {
  const addr = (email.from?.[0]?.email ?? "").trim().toLowerCase();
  const at = addr.lastIndexOf("@");
  if (at < 0 || at === addr.length - 1) return null;
  return addr.slice(at + 1);
}

/**
 * Groups the queue: alias where there is one, sender domain otherwise, and
 * ungroupable messages as groups of one.
 *
 * ORDER IS PRESERVED FROM THE INPUT, which arrives most-confident first
 * (store.visibleIds). Groups are ordered by their best member and members keep
 * their incoming order, so the most confident question in the whole queue is
 * still the first thing on screen — grouping rearranges the surroundings of the
 * answer, never which answer is asked for first.
 *
 * EVERY message it is given is grouped. It does NOT filter on having an open
 * suggestion: the queue is "nothing has categorised this", and on a mailbox with
 * no trained model that is the entire queue with not a suggestion in it.
 * Deciding membership here as well as in the query would mean two definitions of
 * the queue, and this one would win by silently rendering nothing.
 */
export function groupQueue(emails: EffectiveEmail[]): QueueGroup[] {
  // PASS 1 — provisionally by alias, so each alias's sender spread can be seen
  // before deciding whether the alias is a useful key at all.
  const aliasOfMsg = new Map<Id, string | null>();
  const domainOfMsg = new Map<Id, string | null>();
  const spread = new Map<string, Map<string, number>>();
  for (const e of emails) {
    const alias = aliasOf(e);
    const domain = senderDomainOf(e);
    aliasOfMsg.set(e.id, alias);
    domainOfMsg.set(e.id, domain);
    if (!alias) continue;
    let byDomain = spread.get(alias);
    if (!byDomain) {
      byDomain = new Map<string, number>();
      spread.set(alias, byDomain);
    }
    const d = domain ?? "";
    byDomain.set(d, (byDomain.get(d) ?? 0) + 1);
  }

  // PASS 2 — build the real groups, keying by domain instead for any alias that
  // turned out not to be a service address.
  const byKey = new Map<string, QueueGroup>();
  const senders = new Map<string, Set<string>>();
  const order: string[] = [];

  for (const e of emails) {
    const alias = aliasOfMsg.get(e.id) ?? null;
    const domain = domainOfMsg.get(e.id) ?? null;
    const useAlias = alias !== null && aliasConcentrates(spread.get(alias));
    const kind: QueueGroupKind = useAlias ? "alias" : "domain";
    const display = useAlias ? alias : domain;
    // No usable key at all: keep it, keyed to itself, so it is still answerable.
    // Falling through to a shared "unknown" bucket would batch genuinely
    // unrelated mail under one label, which is the one outcome this surface must
    // not produce.
    const key = display ? `${kind}:${display}` : `message:${e.id}`;
    // -1 for "no open question", mirroring store.topConfidence: a real
    // confidence is in [0,1], so nothing can collide with it, and groups the
    // system has said nothing about sort after groups it has.
    const confidence = e.topConfidence ?? -1;

    // Tracked per group rather than derived afterwards, so a group's own list of
    // messages and its sender count can never disagree.
    let seenSenders = senders.get(key);
    if (!seenSenders) {
      seenSenders = new Set<string>();
      senders.set(key, seenSenders);
    }
    seenSenders.add(domain ?? "");

    const existing = byKey.get(key);
    if (existing) {
      existing.ids.push(e.id);
      if (confidence > existing.topConfidence) existing.topConfidence = confidence;
      continue;
    }
    byKey.set(key, {
      key,
      kind,
      display: display ?? (e.from?.[0]?.email ?? e.subject ?? ""),
      ids: [e.id],
      topConfidence: confidence,
      senderCount: 0, // filled below, once every member has been seen
    });
    order.push(key);
  }

  const groups = order.map((k) => {
    const g = byKey.get(k) as QueueGroup;
    g.senderCount = senders.get(k)?.size ?? 1;
    return g;
  });
  // Stable: equal-confidence groups keep the order their best member arrived in,
  // so the list does not reshuffle between renders over a tie. (Array.prototype
  // .sort is required to be stable as of ES2019.)
  groups.sort((a, b) => b.topConfidence - a.topConfidence);
  return groups;
}

/**
 * Whether an address behaves like a SERVICE address rather than a forwarder
 * (#186) — i.e. whether the alias is worth grouping on at all.
 *
 * The test: does one sender account for at least half of its mail?
 *
 * An address you gave to one company receives from that company. An address
 * that was your email for a decade and is now forwarded in receives from
 * everyone who ever had it, and the only thing its mail has in common is the
 * plumbing that delivered it — so "24 messages arrived at forward@ — where do
 * these go?" is a question with no answer. Those messages ARE batchable, just by
 * who sent them, and that is the key this falls back to.
 *
 * Measured on a real mailbox, the separation is not close: the forwarded
 * address spans dozens of sender domains with a tiny top share, while
 * every genuine service alias sits well above half over two domains (the
 * service's own domain plus its bulk subdomain). Sub-grouping the forwarded mail
 * by sender turns one unusable blob into a set of batches covering most of it.
 *
 * A MAJORITY, deliberately, rather than a tuned constant. "Most of this
 * address's mail comes from one place" is a statement anyone can check against
 * their own mailbox; 0.35 or 0.6 would be numbers I picked, and this project has
 * already reverted two rounds of tuned weights that turned out to be measuring
 * corrupted data (#175).
 */
const ALIAS_MAJORITY = 0.5;

function aliasConcentrates(byDomain: Map<string, number> | undefined): boolean {
  if (!byDomain || byDomain.size === 0) return true;
  // A single sender always concentrates, whatever the count — including the very
  // common case of one message, where there is nothing yet to disagree with it
  // and the alias is the better guess.
  if (byDomain.size === 1) return true;
  let total = 0;
  let top = 0;
  for (const n of byDomain.values()) {
    total += n;
    if (n > top) top = n;
  }
  return top / total >= ALIAS_MAJORITY;
}

/**
 * The queue's display layout: what order the rows go in, which of them a header
 * is talking about, and where the batches stop.
 *
 * IT PRODUCES THE ORDER RATHER THAN DECORATING ONE, and that is the point. The
 * first version of this lived inside MessageList and reordered rows for display
 * only, which quietly created two different lists: the screen showed groups, and
 * `j`/`k`, shift-range and select-all walked `visibleIds` — so the cursor moved
 * somewhere other than where the eye was. Anything that decides row order has to
 * be the SAME thing the shell navigates, so it lives here, pure and tested, and
 * the shell drives both from it.
 */
export interface QueueLayout {
  /** Display order: batches first, then everything that could not be grouped. */
  ids: Id[];
  /** The group whose header belongs above this row. */
  headerFor: Map<Id, QueueGroup>;
  /** Rows a header is talking about. */
  grouped: Set<Id>;
  /** The last row of each batch — where the group closes. */
  groupLast: Set<Id>;
  /** The first ungrouped row, or null when there is nothing to divide. */
  restStartsAt: Id | null;
}

export function layoutQueue(emails: EffectiveEmail[]): QueueLayout {
  const headerFor = new Map<Id, QueueGroup>();
  const grouped = new Set<Id>();
  const groupLast = new Set<Id>();

  // Batches first, then the one-offs. A batch of twenty-four is twenty-four
  // labelled messages for one decision, and supervision volume is the entire
  // bottleneck this surface exists to relieve — so the work that moves it most
  // is the work at the top. "Most-confident first" still holds WITHIN each
  // section, which is where it means something.
  const batches: QueueGroup[] = [];
  const singles: QueueGroup[] = [];
  for (const g of groupQueue(emails)) (isBatchable(g) ? batches : singles).push(g);

  const ids: Id[] = [];
  const placed = new Set<Id>();
  const push = (id: Id): void => {
    if (placed.has(id)) return;
    placed.add(id);
    ids.push(id);
  };
  for (const g of batches) {
    if (g.ids[0]) headerFor.set(g.ids[0], g);
    for (const id of g.ids) grouped.add(id);
    const last = g.ids[g.ids.length - 1];
    if (last) groupLast.add(last);
    for (const id of g.ids) push(id);
  }
  // The divider only earns its place when there is something above it to divide
  // FROM. A queue that is entirely one-offs needs no heading announcing it.
  const restStartsAt = batches.length > 0 ? (singles[0]?.ids[0] ?? null) : null;
  for (const g of singles) for (const id of g.ids) push(id);

  return { ids, headerFor, grouped, groupLast, restStartsAt };
}

/**
 * Whether a group is worth offering as a BATCH rather than as plain rows.
 *
 * Two is the floor for the same reason `minInduceExamples` is two on the server
 * (internal/api/rule_induce.go): one message shares its alias with itself, so a
 * "batch" of one is just a row with extra furniture. Below this the group header
 * is not drawn at all — which is what makes the first-run character of this
 * surface emerge from the data instead of from a mode switch nobody asked for.
 */
export function isBatchable(group: QueueGroup): boolean {
  return group.ids.length >= 2;
}
