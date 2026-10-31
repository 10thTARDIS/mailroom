# ADR-034: Out of office is one provider-side Sieve script, refused over any other active script, and its replies are outside the outbound ledger

**Status:** Accepted (records a deviation from spec §5)

## Context

Spec §5 asks for "a vacation responder / server-side filters written via
ManageSieve". The same section says every send, the vacation responder
included, "draws from a per-account budget tracked in an `outbound_ledger`".

The read-only report (`mailroom-admin sieve-report`, v1.90.0-rc.1) measured
the provider's capabilities:

- Vacation is supported. `include` and `vacation-seconds` are not.
- MAXSCRIPTSIZE is 49,152 bytes.
- Accounts may carry an inactive script (for example one named "imported") and no
  active script.

The provider runs **one active script**. So a Mailroom script can only run in
place of whatever the user has active, and without `include` there is no way
to run both.

## Decision

1. **One script of Mailroom's own, `mailroom-vacation`.**
   - Mailroom writes, activates and deletes only this script.
   - It never writes, activates, deletes or reads any other script, active or
     not.
2. **Refuse rather than displace.** If any other script is active when the
   user turns the reply on, Mailroom refuses, with a sentence naming that
   script, and saves nothing (decided 2026-10-18).
   - A textual merge with the user's script was rejected: it would edit
     somebody else's filters.
   - The enable sequence is LISTSCRIPTS, CHECKSCRIPT, PUTSCRIPT, LISTSCRIPTS
     again, then SETACTIVE. If a foreign script became active in between,
     Mailroom takes its own script back out and refuses.
3. **Turning it off restores exactly the previous state.** That state was
   "nothing active", because step 2 accepts nothing else. So turning off is
   SETACTIVE "" and then DELETESCRIPT.
   - If the user has since activated their own script, Mailroom leaves it
     active, deletes only its own inactive script, and says so.
4. **Drift is shown, never repaired by force.** Each account's row stores the
   SHA-256 of the exact script Mailroom installed. Settings asks the provider
   about every reply that is on, and reports one of three conditions:
   - another script is now active (`displaced`);
   - Mailroom's script is gone or inactive (`missing`);
   - Mailroom's script was edited outside Mailroom (`modified`).

   No timer ever activates a script.
5. **The window is enforced twice.**
   - The script carries a `currentdate :zone "+0000"` guard on UTC
     `iso8601` instants. Pigeonhole's output for that zone was measured to be
     `YYYY-MM-DDTHH:MM:SSZ`, so string comparison is exact.
   - Durable jobs fire at the start and the end of the window. The end job
     removes the script.
   - If the dispatcher is down, the guard still stops replies at the end.
6. **The replies are outside the outbound ledger. This is the deviation.**
   - The provider sends them, not Mailroom's SMTP submission. Mailroom never
     sees them, so it cannot count them, and the ledger has no honest number
     to record.
   - Settings says plainly that the provider sends these replies, that they
     do not count toward Mailroom's sending quota meter, and that the
     provider may count them toward its own daily limit.
   - Two things bound the exposure:
     - `:days` (at least 1): one reply per sender per period, and the
       provider's own vacation dedup;
     - the provider's built-in refusals: no reply to lists, bulk mail or
       auto-submitted mail, or to mail not addressed to one of the account's
       own addresses (`:addresses`).
7. **Spam.** The script does not reply to mail marked `X-Spam: yes`,
   `X-Spam-Flag: yes` or `X-Spam-Status: Yes…`.
   - Mail that the provider files to Junk *without* such a header cannot be
     distinguished from inside the user's script. This is stated in Settings
     and accepted.
8. **Contacts-only** embeds an `address :is "from"` list:
   - it holds people the user has written to (`outbound_count > 0`) or marked
     VIP, never a blocked contact, most recently seen first;
   - it is capped at 2,000, and at whatever fits MAXSCRIPTSIZE;
   - the number included and the total are stored and shown;
   - a daily job refreshes it while the reply is on.

## Alternatives considered

- **A Mailroom-side responder**, sending replies through the account's SMTP as
  mail is ingested. It would be metered exactly by the ledger, see contacts
  natively, and never touch Sieve.
  - It was not chosen. Spec §5 names ManageSieve, and the standalone-script plan was chosen.
  - It would also stop replying whenever Mailroom is down or behind on sync.
    The provider's responder does not.
  - It remains the route if the ledger gap ever matters in practice.
- **Wrapping the user's script with `include`.** Impossible on this provider.
- **Estimating provider replies into the ledger.** An estimate presented as a
  ledger entry is a measurement nobody made. It was rejected for the reason
  CLAUDE.md gives about unaudited numbers.

## Consequences

- A user with active webmail filters cannot use the reply until they switch
  those filters off. Mailroom says this in so many words.
- The quota meter under-counts by the number of auto-replies. Settings
  states the bound (one per sender per `:days`).
- The `sievelive` CI job checks this behaviour on a real Pigeonhole:
  - the enable and disable sequences;
  - the refusal;
  - the generated scripts' replies (to a contact in the window, and not to
    spam, strangers, or outside the window).
