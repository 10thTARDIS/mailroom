// SPDX-License-Identifier: AGPL-3.0-only

// WHICH SAVED SIGNATURE APPLIES (#56) — the identity-matching rules, kept out of the
// components so they can be tested without a DOM.
//
// There are two questions, and they have different answers:
//
//   * WHICH SIGNATURE GOES IN WITHOUT BEING ASKED (`automaticSignature`). At most one,
//     and only if the user marked it default. Preference is exact identity over
//     unbound, because a signature written FOR shop@ beats a general one when sending
//     from shop@ — the whole point of per-identity signatures under spec §5's alias
//     model. Never a signature bound to a DIFFERENT identity: putting the wrong name
//     at the bottom of a message is worse than putting none.
//   * WHICH SIGNATURES ARE OFFERED (`offeredSignatures`). Those bound to this identity
//     plus the unbound ones, bound first, default first within each group, then by
//     name. Same filter, different job: this list is what the picker shows when the
//     automatic one is not what the user wanted.
//
// A TEMPLATE HAS NO IDENTITY at all (the server rejects one —
// compose_snippets_template_has_no_identity), so none of this applies to templates.

import type { ComposeSnippet, Id, Identity } from "./jmap/types";

/** The signature to insert without being asked: the default bound to `identityId`,
 * else the default with no binding, else none. */
export function automaticSignature(
  all: ComposeSnippet[],
  identityId: Id | null | undefined,
): ComposeSnippet | null {
  const defaults = all.filter((s) => s.kind === "signature" && s.isDefault);
  return (
    (identityId ? defaults.find((s) => s.identityId === identityId) : undefined) ??
    defaults.find((s) => s.identityId === null) ??
    null
  );
}

/** The body of the signature to insert without being asked, falling back to the
 * IDENTITY'S OWN signature when no snippet is marked default.
 *
 * That fallback is not a nicety: `htmlSignature`/`textSignature` are RFC 8621 fields on
 * `Identity`, so an account can already have a signature set before this feature existed
 * or through any other JMAP client. Ignoring it would mean Mailroom stopped using a
 * signature the user had configured, which reads as the feature having broken it.
 *
 * The snippet wins when there is one: it is the more specific, more recently expressed
 * choice, and it is the only one that can be bound per identity. */
export function automaticSignatureBody(
  all: ComposeSnippet[],
  identity: Identity | undefined,
): { html: string; text: string } | null {
  const snippet = automaticSignature(all, identity?.id ?? null);
  if (snippet) return { html: snippet.bodyHtml, text: snippet.bodyText };
  const html = identity?.htmlSignature ?? "";
  const text = identity?.textSignature ?? "";
  return html.trim() === "" && text.trim() === "" ? null : { html, text };
}

/** The signatures the picker should offer for `identityId`, in the order to show them. */
export function offeredSignatures(
  all: ComposeSnippet[],
  identityId: Id | null | undefined,
): ComposeSnippet[] {
  const bound = (s: ComposeSnippet): number =>
    identityId != null && s.identityId === identityId ? 0 : 1;
  return all
    .filter((s) => s.kind === "signature")
    .filter((s) => s.identityId === null || s.identityId === identityId)
    .sort(
      (a, b) =>
        bound(a) - bound(b) ||
        Number(b.isDefault) - Number(a.isDefault) ||
        a.name.localeCompare(b.name),
    );
}
