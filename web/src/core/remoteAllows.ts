// SPDX-License-Identifier: AGPL-3.0-only

// DOES THIS SENDER'S REMOTE CONTENT LOAD WITHOUT ASKING? (#202, spec §11.)
//
// Remote images are blocked by default and the affordance that loads them was
// per-message and non-persistent, because "I trusted this one newsletter once"
// must not become "this sender may track me forever". That is right about a
// BLANKET allow; what was missing was anywhere to record a NARROW one, so the only
// answers available were "ask every single time" and "never ask" — and asking
// every time about the same newsletter is how a safe default becomes something a
// user routes around.
//
// This module is the matcher, and it is deliberately pure and DOM-free so it can
// be tested without a browser (the same reason core/needsCategory.ts is). It
// decides ONE thing: whether the reading pane should start with remote content
// already loaded for a given From address.
//
// WHAT IT DOES NOT DO. It does not unblock anything by itself. The SERVER still
// parks every remote URL in a data-attribute unconditionally — sanitize.go never
// branches on a preference, because a sanitizer with a branch stops being the only
// path — so the worst a bug here can do is decide to un-park bytes the server
// already made inert. docs/hostile-content.md §2.3.
//
// FAIL CLOSED. Every uncertain input — no From, a malformed address, an empty
// list, an allow whose kind we do not recognise — resolves to "keep blocking and
// ask". The safe answer is the one that costs a click.

/** A remembered decision. Mirrors the RemoteAllow JMAP object. */
export interface RemoteAllow {
  id: string;
  /** "address" matches one exact From; "domain" matches anyone at it. */
  kind: string;
  value: string;
}

/** The domain half of an address, lowercased — or "" if it does not have one.
 *
 * Uses the LAST `@`, matching internal/store.AddressDomain and the ingest
 * projection, so the client and the server never disagree about what the domain of
 * an address is. A disagreement here would be a grant that appears to be stored
 * and never matches. */
export function addressDomain(address: string): string {
  const at = address.lastIndexOf("@");
  if (at < 0 || at === address.length - 1) return "";
  return address.slice(at + 1).toLowerCase();
}

/** Normalises a From for comparison: trimmed and lowercased.
 *
 * Addresses are case-insensitive in practice and the server stores the value as
 * `citext`, so News@Example.com and news@example.com are one decision. Doing it in
 * one function keeps the client's comparison identical to the database's. */
export function normalizeAddress(address: string | null | undefined): string {
  return (address ?? "").trim().toLowerCase();
}

/** Whether this sender's remote content should load without asking.
 *
 * An exact-address grant matches the address; a domain grant matches any sender at
 * that domain. Nothing else matches anything — in particular there is no wildcard
 * and no "all", and an allow carrying an unrecognised `kind` is ignored rather
 * than guessed at, so a future kind cannot accidentally mean "everything" to an
 * older client. */
export function isRemoteAllowed(
  from: string | null | undefined,
  allows: readonly RemoteAllow[] | null | undefined,
): boolean {
  const addr = normalizeAddress(from);
  if (!addr || !allows || allows.length === 0) return false;
  const domain = addressDomain(addr);
  for (const a of allows) {
    const value = normalizeAddress(a.value);
    if (!value) continue;
    if (a.kind === "address" && value === addr) return true;
    if (a.kind === "domain" && domain !== "" && value === domain) return true;
  }
  return false;
}

/** Which grant, if any, is why this sender is allowed — so the reading pane can
 * SAY why images loaded rather than just loading them.
 *
 * A preview that silently behaves differently for one sender than another is the
 * kind of thing that erodes trust in the default. Returns the first match, exact
 * address before domain, because that is the more specific reason and the one a
 * user is likelier to recognise. */
export function matchingRemoteAllow(
  from: string | null | undefined,
  allows: readonly RemoteAllow[] | null | undefined,
): RemoteAllow | null {
  const addr = normalizeAddress(from);
  if (!addr || !allows) return null;
  const domain = addressDomain(addr);
  const byAddress = allows.find((a) => a.kind === "address" && normalizeAddress(a.value) === addr);
  if (byAddress) return byAddress;
  if (domain === "") return null;
  return allows.find((a) => a.kind === "domain" && normalizeAddress(a.value) === domain) ?? null;
}
