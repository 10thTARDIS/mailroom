// SPDX-License-Identifier: AGPL-3.0-only

// The words for the Contacts page that are chosen from a code (#816). One
// table, so a new refusal is a type error here rather than a raw string on
// screen.

import type { ContactError, KeptField, SeparateOutcome } from "../../../core/contacts";
import { t } from "../../i18n";
import type { MessageKey } from "../../i18n";

export const CONTACT_ERROR_TEXT: Record<ContactError, MessageKey> = {
  notFound: "contacts.error.notFound",
  userNameTooLong: "contacts.error.userNameTooLong",
  notesTooLong: "contacts.error.notesTooLong",
  vipConflict: "contacts.error.vipConflict",
  invalid: "contacts.error.invalid",
  stateMismatch: "contacts.error.stateMismatch",
  unavailable: "contacts.error.unavailable",
  failed: "contacts.error.failed",
};

/** The sentence for a refusal. Never the code itself. */
export function contactErrorText(error: ContactError): string {
  return t(CONTACT_ERROR_TEXT[error]);
}

const KEPT_FIELD_TEXT: Record<KeptField, MessageKey> = {
  userName: "contacts.separate.kept.userName",
  notes: "contacts.separate.kept.notes",
  isVip: "contacts.separate.kept.isVip",
  isSaved: "contacts.separate.kept.isSaved",
  isBlocked: "contacts.separate.kept.isBlocked",
};

/** "a", "a and b", "a, b and c". */
function listOf(items: string[]): string {
  if (items.length <= 1) return items.join("");
  return items.slice(0, -1).join(", ") + t("contacts.separate.kept.and") + items[items.length - 1];
}

/** What a separate did to the contact it left (#834), in words; null when the
 * server did not say. */
export function separateOutcomeText(outcome: SeparateOutcome): string | null {
  if (!outcome) return null;
  if (outcome.kind === "unchanged") return t("contacts.separate.unchanged");
  const restored = t("contacts.separate.restored");
  if (outcome.kept.length === 0) return restored;
  const fields = listOf(outcome.kept.map((f) => t(KEPT_FIELD_TEXT[f])));
  return restored + " " + t("contacts.separate.kept", { fields });
}
