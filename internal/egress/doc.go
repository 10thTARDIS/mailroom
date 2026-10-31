// SPDX-License-Identifier: AGPL-3.0-only

// Package egress is the ONE guarded outbound HTTP client, and the only door out
// of the server (ADR-026 §3a). It stands to the network as internal/store stands
// to the database: not a helper anyone may bypass, but the only way through.
//
// # Why this exists, and why it is not part of the image proxy
//
// ADR-026 was written to design an SSRF guard for a future image proxy. Drafting
// it turned up the fact that reordered the work: Mailroom ALREADY makes a
// server-side outbound request to a client-supplied URL.
// internal/webpush/send.go POSTs to a push subscription's endpoint, and
// PushSubscription/set accepted any non-empty string as that endpoint — no
// scheme check, no host check, no address check. That is a blind SSRF: the
// response never reaches the caller, but an authenticated user could make the
// server POST to 169.254.169.254, a loopback admin port, or an internal service.
//
// So the guard is not a component of the proxy. It is shared infrastructure with
// two callers on the day it lands, and the second one is a bug fix.
//
// # What it guards
//
//   - PERMITTED SCHEMES COME FROM THE CALLER. This is a parameter rather than a
//     constant because the callers must not get the same policy: webpush permits
//     https only, and the image proxy will permit http as well (ADR-026 decision
//     A — an http image URL is mixed content the browser already blocks, so
//     proxying it is the only way it ever renders). A Policy that names no
//     scheme gets https only, so the unsafe direction is never the default.
//   - RESOLVE, THEN DECIDE, THEN CONNECT TO THE ADDRESS THAT WAS DECIDED ABOUT.
//     The check and the connection use the SAME IP, because validating a
//     hostname and then handing that hostname to http.Client re-resolves it and
//     is the textbook DNS-rebinding hole. Validation therefore happens in the
//     dialer, on the address it is about to dial, and the dial target is an IP
//     literal so nothing can resolve a second time.
//   - EVERY RESOLVED ADDRESS MUST PASS, not merely the one that gets dialed. A
//     host publishing both a public address and 127.0.0.1 is refused outright;
//     otherwise the outcome would depend on which record the resolver happened
//     to return first.
//   - NON-PUBLIC ADDRESSES ARE REFUSED on both families, from one table in
//     address.go — including the IPv4-mapped and IPv4-compatible IPv6 encodings,
//     which are how this check is usually bypassed.
//   - REDIRECTS ARE RE-VALIDATED and capped. The address check needs no special
//     case here: a redirect to another host dials again and therefore resolves
//     and validates again; a redirect to the same host reuses a connection that
//     was already validated.
//   - NO PROXY. The transport sets Proxy: nil rather than inheriting
//     ProxyFromEnvironment, because an HTTP proxy would defeat the address guard
//     completely — the connection would go to the proxy, and the URL's host
//     would never be resolved or checked here at all.
//   - RESPONSES ARE BOUNDED in bytes and in wall-clock time.
//   - A REFUSAL IS NEVER AN EMPTY SUCCESS. Every refusal is a distinct error
//     (ErrSchemeNotPermitted, ErrBlockedAddress, ErrTooManyRedirects,
//     ErrTooLarge) that a caller must handle. This is the shape that has cost
//     this project repeatedly, most recently a security check that reported "the
//     tracker was contacted" because it could not read its own evidence (#332).
//
// # Non-HTTP connections: Dialer
//
// The mail account's own connections — IMAP for sync, IDLE, writeback and
// validation; SMTP for submission — are not HTTP, so they use Dialer rather
// than Client. It is the same guarantee from the same code (checkedAddrs):
// resolve once, refuse unless every address passes, connect to the checked IP
// literal, and leave TLS to the caller with ServerName set to the HOSTNAME.
// Until #625 §3 those dials resolved the hostname themselves, so a host checked
// at validation could rebind to a private address before the next sync pass and
// be handed the account's credentials. Dialer.AllowPrivate is
// MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS; it lifts the policy for account
// connections only, never for Client.
//
// # What it deliberately does not do yet
//
// ADR-026 §3a also lists a sniffed Content-Type allowlist. That is a proxy
// requirement and the proxy does not exist, so building it here now would ship a
// mechanism with no caller — the shape this project keeps paying for. It lands
// with the endpoint that needs it.
//
// Nor does this package strip the caller's headers wholesale. "Send nothing of
// the user's" (ADR-026 §3a) means the READER's identity: cookies (there is no
// cookie jar, so none are ever sent), Referer (stripped, including across
// redirects, where net/http would otherwise add it), and a fixed neutral
// User-Agent. It does not mean a credential the SERVER holds: webpush's VAPID
// Authorization header is Mailroom authenticating itself to a push service, not
// anything about the person reading the mail, and the caller sets it.
package egress
