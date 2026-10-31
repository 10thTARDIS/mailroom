// SPDX-License-Identifier: AGPL-3.0-only

// The words for Settings → Keys (#796) that are chosen from a code: every
// refusal the server documents, the status chips and the sources. One table
// each, so a new code is a type error here rather than a raw string on screen.

import type { KeyEventSource, KeyHistoryLine, RefreshOutcome } from "../../../../core/keyLifecycle";
import { keyDateLong } from "../../../../core/userKeys";
import type { ContactKeySource, KeyActionError, KeyChip, PublishBlock } from "../../../../core/userKeys";
import { t } from "../../../i18n";
import type { MessageKey } from "../../../i18n";

export const KEY_ERROR_TEXT: Record<KeyActionError, MessageKey> = {
  keyManagementUnavailable: "keys.error.keyManagementUnavailable",
  notFound: "keys.error.notFound",
  rateLimited: "keys.error.rateLimited",
  invalidArguments: "keys.error.invalidArguments",
  wildcardIdentity: "keys.error.wildcardIdentity",
  publicKeyRefused: "keys.error.publicKeyRefused",
  invalidKey: "keys.error.invalidKey",
  tooLarge: "keys.error.tooLarge",
  keyRevoked: "keys.error.keyRevoked",
  keyNotBound: "keys.error.keyNotBound",
  wkdUnavailable: "keys.error.wkdUnavailable",
  keyDoesNotNameAddress: "keys.error.keyDoesNotNameAddress",
  oneChangeAtATime: "keys.error.oneChangeAtATime",
  keyIsDefault: "keys.error.keyIsDefault",
  keyIsPublished: "keys.error.keyIsPublished",
  keyRetired: "keys.error.keyRetired",
  wkdNotConfigured: "keys.error.wkdNotConfigured",
  notPublishable: "keys.error.notPublishable",
  retireIsPermanent: "keys.error.retireIsPermanent",
  invalidProperties: "keys.error.invalidProperties",
  unavailable: "keys.error.unavailable",
  failed: "keys.error.failed",
};

/** The sentence for a refusal. Never the code itself. */
export function keyErrorText(error: KeyActionError): string {
  return t(KEY_ERROR_TEXT[error]);
}

export function keyChipText(chip: KeyChip): string {
  switch (chip.kind) {
    case "signsAs":
      return t("keys.chip.signsAs", { address: chip.address });
    case "published":
      return t("keys.chip.published");
    case "expiringSoon":
      return t("keys.chip.expiringSoon");
    case "expired":
      return t("keys.chip.expired");
    case "retired":
      return t("keys.chip.retired");
    case "revoked":
      return t("keys.chip.revoked");
  }
}

export const PUBLISH_BLOCK_TEXT: Record<PublishBlock, MessageKey> = {
  wkdOff: "keys.publish.wkdOff",
  notBound: "keys.publish.notBound",
  notServable: "keys.publish.notServable",
  retired: "keys.publish.retired",
  unknown: "keys.publish.unknown",
};

export const SOURCE_TEXT: Record<ContactKeySource, MessageKey> = {
  imported: "keys.source.imported",
  wkd: "keys.source.wkd",
  autocryptAuthenticated: "keys.source.autocryptAuthenticated",
  autocrypt: "keys.source.autocrypt",
  keyserver: "keys.source.keyserver",
  other: "keys.source.other",
};

const VIA_TEXT: Record<KeyEventSource, MessageKey> = {
  autocrypt: "keys.via.autocrypt",
  wkd: "keys.via.wkd",
  manual: "keys.via.manual",
  keyserver: "keys.via.keyserver",
  user: "keys.via.user",
  other: "keys.via.other",
};

/** One event of a correspondent key's history (#815) as a short sentence:
 * "First seen via Autocrypt", "Renewed via WKD — now expires 3 Nov 2028". */
export function keyEventText(line: KeyHistoryLine): string {
  const via = t(VIA_TEXT[line.source]);
  const tidy = (s: string): string => s.replace(/\s+/g, " ").trim();
  switch (line.kind) {
    case "first_seen":
      return line.source === "manual" ? t("keys.event.imported") : tidy(t("keys.event.firstSeen", { via }));
    case "renewed":
      return line.expiresAt
        ? tidy(t("keys.event.renewedUntil", { via, date: keyDateLong(line.expiresAt) }))
        : tidy(t("keys.event.renewed", { via }));
    case "updated":
      return tidy(t("keys.event.updated", { via }));
    case "revoked_seen":
      return via === "" ? t("keys.event.revokedSeenPlain") : tidy(t("keys.event.revokedSeen", { via }));
    case "pinned":
      return t("keys.event.pinned");
    case "refused":
      return t("keys.event.refused");
    case "removed":
      return t("keys.event.removed");
    case "other":
      return t("keys.event.other");
  }
}

/** What a refresh found (#818), as the sentence the card shows. A refusal of
 * the check itself is a KeyActionError: rateLimited has its own words here. */
export function refreshText(outcome: RefreshOutcome | { kind: "error"; error: KeyActionError }): string {
  switch (outcome.kind) {
    case "renewed":
      return outcome.expiresAt
        ? t("keys.refresh.renewed", { date: keyDateLong(outcome.expiresAt) })
        : t("keys.refresh.renewedNever");
    case "unchanged":
      return t("keys.refresh.unchanged");
    case "differentKey":
      return t("keys.refresh.differentKey");
    case "revoked":
      return t("keys.refresh.revoked");
    case "updated":
      return t("keys.refresh.updated");
    case "notFound":
      return t("keys.refresh.notFound");
    case "unreachable":
      return t("keys.refresh.unreachable");
    case "unknown":
      return t("keys.refresh.unknown");
    case "error":
      return outcome.error === "rateLimited" ? t("keys.refresh.rateLimited") : keyErrorText(outcome.error);
  }
}
