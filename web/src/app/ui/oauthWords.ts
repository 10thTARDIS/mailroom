// SPDX-License-Identifier: AGPL-3.0-only

// The words for sign-in providers and results (#52), in one place so the
// settings page, the result message and the banner name things alike.

import { isOAuthProvider } from "../../core/oauth";
import type { OAuthReturn } from "../../core/oauth";
import { t } from "../i18n";
import type { MessageKey } from "../i18n";
import { START_BAD_URL } from "./oauthReturn";

/** "Google", "Microsoft", or "your provider". */
export function providerName(provider: string | null | undefined): string {
  return isOAuthProvider(provider) ? t(`oauth.provider.${provider}` as MessageKey) : t("oauth.provider.unknown");
}

/** The sentence for a sign-in result. */
export function oauthResultText(ret: OAuthReturn): string {
  return t(`oauth.result.${ret.code}` as MessageKey, { provider: providerName(ret.provider) });
}

/** The sentence for a sign-in that could not start. */
export function startErrorText(error: string, provider: string | null | undefined): string {
  return t("oauth.dialog.failed", {
    provider: providerName(provider),
    error: error === START_BAD_URL ? t("oauth.dialog.badUrl") : error,
  });
}
