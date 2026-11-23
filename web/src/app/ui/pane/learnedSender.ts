// SPDX-License-Identifier: AGPL-3.0-only

// What the sender card says about learned sorting (#167), as pure functions so
// every verdict and reason can be tested against a fake LearnedSender/get answer.

import type { LearnedSender, LearnedSenderEvidence } from "../../../core/jmap/types";
import { keyDateLong } from "../../../core/userKeys";
import { t, tCount } from "../../i18n";

/** The key learned sorting would decide this sender by. */
export function decidingEvidence(s: LearnedSender): LearnedSenderEvidence {
  if (s.decidedBy === "domain") return s.domain;
  // address_decides on the domain means the address key is the one that matters.
  if (s.decidedBy === "address" || s.domain.reason === "address_decides") return s.address;
  // Nothing decided: report the first key with a reason, address first.
  if (s.address.reason) return s.address;
  if (s.domain.reason) return s.domain;
  return s.address;
}

/** "Nothing learned", or, when the user asked to forget and nothing has been filed since, say so. */
function nothingLearned(s: LearnedSender): string {
  const at = s.forgottenAt ?? null;
  if (at && s.address.filings === 0 && s.domain.filings === 0) {
    return t("learnedSender.forgotten", { date: keyDateLong(at) });
  }
  return t("learnedSender.none");
}

/** One short plain sentence for the state. `areaName` maps an area id to its name. */
export function learnedSenderSentence(s: LearnedSender, areaName: (id: string) => string | null): string {
  const e = decidingEvidence(s);
  const area = e.areaId ? areaName(e.areaId) : null;
  if (e.acting && area) {
    return t(s.decidedBy === "domain" ? "learnedSender.acting.domain" : "learnedSender.acting.address", {
      area,
      count: e.filings,
    });
  }
  switch (e.reason) {
    case "none":
      return nothingLearned(s);
    case "thin":
      return e.filings > 0 ? tCount("learnedSender.thin", e.filings) : nothingLearned(s);
    case "mixed":
      return t("learnedSender.mixed");
    case "contradicted":
      return t("learnedSender.contradicted");
    case "unauthenticated":
      return t("learnedSender.unauthenticated");
    case "blocked":
      return t("learnedSender.blocked");
    case "gated":
      return area ? t("learnedSender.gated", { area }) : t("learnedSender.gated.plain");
    case "off":
      return t("learnedSender.off");
    default:
      return e.filings === 0 && e.verdict === "none" ? nothingLearned(s) : t("learnedSender.other");
  }
}

/** The scope Forget should use, or null when there is nothing to forget. */
export function forgetScope(s: LearnedSender): "address" | "domain" | null {
  if (s.decidedBy === "domain" && s.domain.filings > 0) return "domain";
  if (s.address.filings > 0) return "address";
  if (s.domain.filings > 0) return "domain";
  return null;
}

/** Trust is offered only where it can matter: refused as unverified, or already on. */
export function showTrust(s: LearnedSender): boolean {
  return s.trusted || s.address.reason === "unauthenticated";
}

/**
 * True when trust cannot help this sender: its address is at one of the user's
 * own identity domains, where mail is always checked. An older server that does
 * not say reads as useful.
 */
export function trustUseless(s: LearnedSender): boolean {
  return s.trustUseful === false;
}
