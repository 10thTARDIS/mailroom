// SPDX-License-Identifier: AGPL-3.0-only

// HOW A CORRESPONDENT IS NAMED IN A HEADER LINE (#600).
//
// The thread pane's `To` line rendered `name || email`, so a recipient with a
// display name showed ONLY the name — and there is no title, no popover and no
// expand on that line, so their address was not merely hidden but unreachable.
// It was hit when trying to confirm which address a message had gone to:
// "it just shows To: Dana Vance, with no way to see the email address behind
// that".
//
// TWO REASONS THAT IS WORSE THAN IT SOUNDS ON THIS DEPLOYMENT:
//
//   * the catch-all model is ABOUT which address is which. A client that cannot
//     say where something went cannot be used to check an alias scheme, which is
//     the thing this project exists to run.
//   * a display name is chosen by whoever set it. `Dana Vance <dana@example.org>`
//     and `Dana Vance <someone@elsewhere.example>` render identically when only
//     the name is shown, and on a reply-all that is the difference between
//     answering the thread and answering a stranger. It is the same argument that
//     puts the sender's address in pane/SenderPopover.tsx; it does not stop at
//     From.
//
// WHY INLINE RATHER THAN A POPOVER, which is what the sender gets: SenderPopover
// answers four sender-specific questions (why it ranked here, what authentication
// said, which area it landed in, and what to do about the person). None of that
// exists for a recipient, so reusing it would mean building a second popover to
// carry one string. `Name <address>` is what every other mail client shows in an
// expanded header, works without hover, and works on touch.
//
// A PURE FUNCTION rather than JSX, because there is no jsdom in this repo's web
// tests: logic left in ThreadPane.tsx cannot be covered at all.

/** One address as it should read in a header line.
 *
 * `Name <address>` when a display name exists and differs from the address;
 * the address alone otherwise. Never a name alone — that is the defect. */
export function addressLabel(addr: { name?: string | null; email: string }): string {
  const name = (addr.name ?? "").trim();
  const email = addr.email.trim();
  if (email === "") return name; // nothing better to say; an empty line is worse
  if (name === "" || name.toLowerCase() === email.toLowerCase()) return email;
  return `${name} <${email}>`;
}

/** A header line's worth of addresses, comma-separated. */
export function addressLine(addrs: readonly { name?: string | null; email: string }[]): string {
  return addrs.map(addressLabel).join(", ");
}
