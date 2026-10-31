// SPDX-License-Identifier: AGPL-3.0-only

// "Create a rule from this message" (#155) — turning the message in front of you
// into a filled-in rule.
//
// This is the affordance the whole rules feature lives or dies on. Almost every
// rule anyone wants starts from a message they are looking at ("stop putting
// THIS in my inbox"), and the reason rule systems go unused is that they ask
// people to reconstruct that message from memory as abstract conditions. So the
// work here is turning one Email into the handful of conditions a person
// actually means, pre-named, ready to confirm.
//
// DOM-free and framework-agnostic (spec §12), like everything in core/.

import type { Email, RuleCondition } from "./jmap/types";

/** One offered starting point: a condition, and how to describe it. */
export interface RuleSuggestion {
  /** Stable key for the UI (also the i18n suffix). */
  kind: "sender" | "domain" | "subject" | "list";
  condition: RuleCondition;
  /** Interpolation params for the `rules.fromMessage.<kind>` string. */
  params: Record<string, string>;
  /** The default name for a rule built from this suggestion. */
  defaultName: string;
}

/** The domain part of an address, lowercased, or "". */
export function domainOf(address: string): string {
  const at = address.lastIndexOf("@");
  if (at < 0 || at === address.length - 1) return "";
  return address.slice(at + 1).toLowerCase();
}

/**
 * The starting points worth offering for this message, best first.
 *
 * "Best first" is deliberate and is the whole judgement of this function:
 *
 *   - A MAILING LIST is offered first when there is one, because List-Id is the
 *     most precise thing a bulk message carries. It identifies the actual list
 *     rather than the domain that happens to send it, which is what keeps a
 *     retailer's newsletter rule from also catching that retailer's receipts.
 *   - THE SENDER next: exact, obvious, and what people mean most of the time.
 *   - THE DOMAIN after that, because it is a blunter instrument — @example.com
 *     catches the receipts too — and the preview is what tells you whether that
 *     matters. It is offered, not defaulted.
 *   - THE SUBJECT last, and only as a `contains`, since an exact subject match
 *     is a rule that fires once and never again.
 *
 * Returns [] for a message with no usable sender, rather than a rule that would
 * match nothing.
 */
export function suggestionsFor(email: Email): RuleSuggestion[] {
  const from = email.from?.[0];
  const address = (from?.email ?? "").trim();
  const display = (from?.name ?? "").trim() || address;
  const out: RuleSuggestion[] = [];

  // Offered only when the message actually names a list. `listId exists` would
  // match EVERY mailing list, which is never what someone means when they pick
  // "from this mailing list" while looking at one specific newsletter.
  const listId = (email.listId ?? "").trim();
  if (listId) {
    out.push({
      kind: "list",
      condition: { field: "listId", op: "contains", value: listId },
      params: { list: listId },
      defaultName: nameFor(listId),
    });
  }

  if (address) {
    out.push({
      kind: "sender",
      condition: { field: "from", op: "equals", value: address },
      params: { from: address },
      defaultName: nameFor(display),
    });
    const domain = domainOf(address);
    if (domain) {
      out.push({
        kind: "domain",
        condition: { field: "fromDomain", op: "equals", value: domain },
        params: { domain },
        defaultName: nameFor(domain),
      });
    }
  }

  const subject = (email.subject ?? "").trim();
  if (subject) {
    out.push({
      kind: "subject",
      // `contains`, never `equals`: an exact subject match is a rule that fires
      // for one message and then never again.
      condition: { field: "subject", op: "contains", value: subject },
      params: { subject },
      defaultName: nameFor(subject),
    });
  }

  return out;
}

/**
 * A default rule name from the thing it keys on, trimmed to something legible.
 *
 * Exported as `trimRuleNamePart` for ruleIdentity.ts (#292), which composes this
 * with the label the user picks in the editor — `Venmo → Promotions` rather than
 * the bare `Venmo` that collides with the rule they already have. The 48-char
 * cap belongs to THIS half of the name, not to the whole thing: it is what keeps
 * a 200-character subject line from eating the label that follows it.
 */
function nameFor(subject: string): string {
  const clean = subject.replace(/\s+/g, " ").trim();
  return clean.length > 48 ? `${clean.slice(0, 47)}…` : clean;
}

export { nameFor as trimRuleNamePart };
