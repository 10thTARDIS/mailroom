// SPDX-License-Identifier: AGPL-3.0-only

// WHICH SERVER READ MAY SPEAK FOR A FIELD THIS DEVICE JUST WROTE (#562).
//
// The shell puts every `web` ClientPreferences record that reaches the store on
// screen (Shell.tsx applyServerPrefs) — that is how a theme chosen on another
// device arrives here. But records reach the store from READS that run
// concurrently with this device's own WRITES: the `/get` beside each `/set`, the
// `/get` a `/changes` poll follows up with (and the push for this device's own
// write is what starts that poll), the reference-data load. A read the server
// answered before a later write committed carries the EARLIER value, and it can
// arrive after that write has landed. Applied, it undoes the user's newer choice
// — which is what the accessibility lane caught as `messageOpens beside: still
// false`, and what a user sees as Settings "not taking" when they change their
// mind quickly.
//
// ⚠ THE ORDERING EVIDENCE IS LOCAL, ON PURPOSE. A JMAP state string is opaque
// (RFC 8620 §1.2): this server's happens to be a number, but a client that
// ordered states by parsing them would be depending on an encoding the protocol
// says it may not. What IS known here, without asking the server anything, is
// the order in which this client sent its requests and heard back:
//
//   A READ SENT AFTER A WRITE'S ANSWER ARRIVED SEES THAT WRITE. The server
//   commits a `/set` before it answers it, and every method call reads in a
//   transaction of its own, so a read that leaves this client after the answer
//   came back cannot have been served before the commit.
//
//   A READ SENT BEFORE THAT MAY OR MAY NOT. It could have been served either
//   side of the commit, and nothing in the record says which. For the fields
//   that write touched, it is not evidence — the device keeps the value the
//   user chose.
//
// A read is still AUTHORITATIVE for every field no unanswered write touches:
// an old read that also carries a theme another device changed delivers the
// theme, and only the field being written is held back.
//
// The one read that is authoritative for a write it was sent WITH is the `/get`
// that rides beside that write's own `/set` — calls in one request run in
// order (RFC 8620 §3.2), so it is served after the set. The caller says so with
// `confirms`, which is how "stores what the SERVER says it holds" keeps holding
// for the write's own answer, including a value the server normalised.
//
// WHAT THIS DELIBERATELY KEEPS:
//   * SERVER WINS AT LOAD. With no write of this device's unanswered, every
//     field of every read is applied, exactly as before.
//   * A REFUSED WRITE LEAVES THE DEVICE ON THE USER'S VALUE. A refusal is an
//     answer: it settles the write like any other, so a read that raced it does
//     not snap the device back, and the first read sent AFTER it is the
//     server's word again — the same "until the next record" the refusal always
//     meant.
//
// The one thing it gives up: a change made on ANOTHER device to the SAME field,
// inside the round trip of this device's write, is held back until the next
// read — it is indistinguishable, from here, from the echo of this device's
// earlier value, and the user's latest click is the better guess.

import type { ClientPreferences } from "./jmap/types";

/** The fields a client writes. Mirrors actions.ts ClientPreferencesPatch; kept
 * here so the ledger does not import the actions module. */
export type PrefsField =
  | "theme"
  | "density"
  | "messageOpens"
  | "paneSplitPct"
  | "textSize"
  | "rowLayout";

export type PrefsPatch = Partial<Pick<ClientPreferences, PrefsField>>;

/** The newest write of one field, and when its answer arrived. */
interface Shadow {
  /** The write it came from. */
  seq: number;
  value: unknown;
  /** Clock value when the write's answer (acceptance, refusal or failure)
   * arrived; null while it is still unanswered. */
  settledAt: number | null;
}

/**
 * The record of this device's ClientPreferences writes that a server read
 * might not reflect yet. Every event takes a tick of one clock — a read being
 * SENT, a write being MADE, a write being ANSWERED — and those ticks are the
 * whole of the ordering evidence.
 *
 * Bounded without any clean-up pass: at most one shadow per family per field,
 * and a newer write replaces the older one's.
 *
 * ⚠ AN ANSWERED SHADOW IS NOT DELETED WHEN A FRESH READ ARRIVES. Reads overlap:
 * the write's own `/get` (or a read sent after its answer) routinely lands
 * BEFORE an older read that was already in flight — that is precisely the
 * accessibility lane's order — and deleting the shadow at the first would hand
 * the second the field. Kept, it costs nothing: the clock only moves forward,
 * so every read sent after the answer passes it by.
 */
export class PrefsLedger {
  private clock = 0;
  private readonly shadows: Record<string, Partial<Record<PrefsField, Shadow>>> = {};

  /** Call immediately before SENDING a request that reads ClientPreferences;
   * hand the result to `reconcile` with that read's answer. */
  beginRead(): number {
    return ++this.clock;
  }

  /**
   * Records a local write the moment the user makes it — before any debounce,
   * because the device has already changed and a read landing in the debounce
   * window is exactly as stale as one landing while the request is out.
   * Returns the write's id, for `settle` and `reconcile`'s `confirms`.
   */
  beginWrite(family: string, patch: PrefsPatch): number {
    const seq = ++this.clock;
    const fam = (this.shadows[family] ??= {});
    for (const [field, value] of Object.entries(patch) as [PrefsField, unknown][]) {
      if (value === undefined) continue;
      fam[field] = { seq, value, settledAt: null };
    }
    return seq;
  }

  /**
   * The server has answered these writes — accepted, refused, or the request
   * failed outright. A shadow a NEWER write has since replaced is left alone:
   * the older write's answer says nothing about the newer one.
   */
  settle(family: string, seqs: readonly number[]): void {
    if (seqs.length === 0) return;
    const at = ++this.clock;
    const fam = this.shadows[family];
    if (!fam) return;
    for (const shadow of Object.values(fam)) {
      if (shadow && shadow.settledAt === null && seqs.includes(shadow.seq)) shadow.settledAt = at;
    }
  }

  /**
   * What a record from a read sent at `readAt` should say, for this device.
   * Fields no unanswered write touches are the server's; a field whose newest
   * write the read cannot be shown to have seen keeps that write's value.
   * `confirms` names writes this read was served AFTER (the `/get` beside the
   * write's own `/set`).
   *
   * Returns the record itself when nothing is held back, so a caller comparing
   * identities sees no change it did not make.
   */
  reconcile(
    record: ClientPreferences,
    readAt: number,
    confirms: readonly number[] = [],
  ): ClientPreferences {
    const fam = this.shadows[record.family];
    if (!fam) return record;
    let out: ClientPreferences | null = null;
    for (const [field, shadow] of Object.entries(fam) as [PrefsField, Shadow | undefined][]) {
      if (!shadow) continue;
      if (confirms.includes(shadow.seq)) {
        // Served after the write: the server's word for this field, including
        // any value it normalised. Remembered as the write's value, so a read
        // still in flight from BEFORE it is held to what the server stored.
        shadow.value = (record as unknown as Record<string, unknown>)[field];
        continue;
      }
      if (shadow.settledAt !== null && shadow.settledAt < readAt) continue;
      out ??= { ...record };
      (out as unknown as Record<string, unknown>)[field] = shadow.value;
    }
    return out ?? record;
  }
}
