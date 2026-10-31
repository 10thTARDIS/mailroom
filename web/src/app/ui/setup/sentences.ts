// SPDX-License-Identifier: AGPL-3.0-only

// THE SENTENCES DAY-ONE SAYS ABOUT SOMEBODY'S MAILBOX (#446).
//
// Every one of them is a claim about thousands of the user's messages made just
// before a bulk write, which is why they are here — pure, at a fixed input, with
// a test — rather than assembled inline in JSX. The desk's own chrome is built
// the same way and for the same reason (ui/desk/format.ts).
//
// THE ONE RULE THAT SHAPES ALL OF IT: A CLAUSE WHOSE NUMBER IS NOT KNOWN IS
// OMITTED, NEVER PRINTED AS ZERO. "0 in the tray" reads exactly like a tray
// somebody counted and found empty, and on a wizard opened straight from a URL —
// where the desk has never been loaded and there is no tray count at all — it
// would be a number nobody measured (#159/#176). So every input that can be
// unknown is `number | null`, and null drops its clause.
//
// ⚠ THE ONE ZERO THAT IS KEPT IS `moves`, and it is kept deliberately: "move 0
// messages" is the reassurance the whole step exists to give. Dropping it would
// leave the sentence silent about the only number that changes anything on the
// provider, which is the number the reader is most anxious about.

import type { FolderCensus, SetupCounts } from "../../../core/jmap/types";
import { t, tCount } from "../../i18n";

/** How clauses are joined, and the desk's own separator. */
const SEP = " · ";

/**
 * A count with thousands separators — "4,612".
 *
 * ⚠ THE GROUPING IS THE CATALOG'S, NOT THE HOST'S. `Intl.NumberFormat` would
 * follow the browser's locale, which sounds better and is wrong here: this app
 * has exactly one catalog (app/i18n.ts) and its sentences are English, so a
 * number grouped as "4.612" inside an English sentence would be a different
 * claim to a German reader, not a translated one. When a second locale lands,
 * this becomes a formatter bound to the catalog's own locale — which is the same
 * decision i18n.ts's `tCount` makes about plural forms, for the same reason.
 */
export function countText(n: number): string {
  const sign = n < 0 ? "-" : "";
  const digits = Math.abs(Math.trunc(n)).toString();
  let out = "";
  for (let i = 0; i < digits.length; i++) {
    if (i > 0 && (digits.length - i) % 3 === 0) out += ",";
    out += digits.charAt(i);
  }
  return sign + out;
}

/** "4,612 messages" / "1 message" — a fragment, so the sentences below can put a
 * grouped count inside a plural without every one of them repeating the pair. */
export function messagesText(n: number): string {
  return tCount("setup.count.messages", n, { n: countText(n) });
}

/** "13 folders" / "1 folder". */
export function foldersText(n: number): string {
  return tCount("setup.count.folders", n, { n: countText(n) });
}

/**
 * WHAT A FOLDER LOOKS LIKE, in one phrase — the "Looks like" column of step 1.
 *
 * ⚠ THE SHAPE WORD IS NEVER ALONE. `shape` is a word for the counts beside it and
 * not an authority (internal/store/setup_census.go says so at length: every
 * boundary behind it is a judgement, not a measurement), so three of the five
 * phrases carry the number they were derived from — `sendersFor90` against
 * `live`, or `distinctSenders` — and the row prints the raw counts in its other
 * columns as well. A user who disagrees with the word can see why it was chosen.
 */
export function shapeSentence(f: FolderCensus): string {
  switch (f.shape) {
    case "oneDomain":
      return t("setup.shape.oneDomain");
    case "fewSenders":
      // The concentration figure, with its denominator: "5 senders cover 190".
      return t("setup.shape.fewSenders", {
        n: countText(f.sendersFor90),
        live: countText(f.live),
      });
    case "bulk":
      return t("setup.shape.bulk", { senders: countText(f.distinctSenders) });
    case "mixed":
      return t("setup.shape.mixed");
    case "empty":
      return t("setup.shape.empty");
  }
}

/**
 * How long a run of this size is expected to take, or null when there is nothing
 * to write.
 *
 * A STATED PLANNING RATE, not a measurement — the server computes it by dividing
 * the write count by a constant (internal/store/setup.go setupWritesPerSecond) —
 * so it is phrased as "about", never as a countdown, and it is DROPPED entirely
 * when the plan writes nothing. "about 0 minutes" would be a duration for work
 * that is not going to happen.
 */
function timeText(estimatedSeconds: number): string | null {
  if (estimatedSeconds <= 0) return null;
  if (estimatedSeconds < 60) return t("setup.would.underMinute");
  return tCount("setup.would.minutes", Math.round(estimatedSeconds / 60));
}

/**
 * THE "THIS WOULD …" SENTENCE of step 2 — the last thing a reader sees before
 * the only button in day-one that writes.
 *
 * Null when there is no preview: the step then says the preview could not be
 * measured, which is a different thing from a plan that would do nothing.
 */
export function wouldSentence(counts: SetupCounts | null): string | null {
  if (!counts) return null;
  const clauses: string[] = [
    t("setup.would.labels", { n: countText(counts.labels) }),
    t("setup.would.archived", { n: countText(counts.archived) }),
    // KEPT AT ZERO. See the file header: this is the clause the step exists for.
    t("setup.would.moves", { n: countText(counts.moves) }),
  ];
  // Dropped at zero, unlike moves: with nothing left behind there is no "later"
  // to talk about, and "leave 0 for later" invites the reader to look for the
  // folders it is about.
  if (counts.leave > 0) clauses.push(t("setup.would.leave", { n: countText(counts.leave) }));
  const time = timeText(counts.estimatedSeconds);
  if (time) clauses.push(time);
  return t("setup.would.lead", { clauses: clauses.join(SEP) });
}

/** What the closing sentence of step 4 is made of. Every field is nullable
 * because every one of them is a measurement this client may not have: the desk
 * may never have been loaded (a wizard opened from a pasted URL), and the preview
 * may have failed. */
export interface DeskLineInput {
  /** In the in-tray. Null when the tray has not been counted. */
  tray: number | null;
  /** Above the line at the chosen threshold — from the LAST preview, so it
   * describes the line the four buttons currently show. */
  review: number | null;
  /** Below it, same preview. */
  drawers: number | null;
  /** What day-one filed into All mail: the plan's `archived`, or the run's own
   * count once it has one. */
  allMail: number | null;
  /** Older mail left for later, and the folders it is in. The count without the
   * names would be a number with nowhere to go. */
  leave: number | null;
  leaveFolders: readonly string[];
}

/**
 * "Your desk will open with: …" — the promise step 4 closes on.
 *
 * It is the one sentence in day-one that describes a screen the user has not seen
 * yet, so every number in it has to be one the server actually gave: `review` and
 * `drawers` come from the last `Area/getSummary` preview at the chosen line,
 * `allMail` from the plan the run carried out, `leave` from the preview's own
 * count of what was left. A clause with no number is not present.
 *
 * Null when NOTHING is known — a sentence that is only its own lead-in ("Your
 * desk will open with: .") is worse than no sentence at all.
 */
export function desklineSentence(input: DeskLineInput): string | null {
  const clauses: string[] = [];
  if (input.tray !== null) clauses.push(t("setup.deskline.tray", { n: countText(input.tray) }));
  if (input.review !== null) {
    clauses.push(t("setup.deskline.review", { n: countText(input.review) }));
  }
  if (input.drawers !== null) {
    clauses.push(t("setup.deskline.drawers", { n: countText(input.drawers) }));
  }
  if (input.allMail !== null) {
    clauses.push(t("setup.deskline.allMail", { n: countText(input.allMail) }));
  }
  // The names are part of the clause, not decoration: "55 in Needs sorting" with
  // no folder named leaves the reader unable to act on it. With a count but no
  // names the clause is still worth saying, so the names are optional inside it
  // rather than a condition on it.
  if (input.leave !== null && input.leave > 0) {
    clauses.push(
      input.leaveFolders.length > 0
        ? t("setup.deskline.needsSorting", {
            n: countText(input.leave),
            folders: input.leaveFolders.join(", "),
          })
        : t("setup.deskline.needsSortingUnnamed", { n: countText(input.leave) }),
    );
  }
  if (clauses.length === 0) return null;
  return t("setup.deskline.lead", { clauses: clauses.join(SEP) });
}

/**
 * The first sentence of step 1, in the two shapes a mailbox actually comes in.
 *
 * THE SECOND ONE IS NOT AN EDGE CASE. A login whose provider does no folder
 * sorting at all — a small test instance — has INBOX and nothing else,
 * and the generic sentence would tell them about "1 folder" and then show a table
 * with one row and a decision about feeding folders that do not exist. Saying so
 * outright is the honest answer, and it is what the step hides the table for.
 */
export function censusSentence(folders: readonly FolderCensus[]): string {
  const plain = folders.filter((f) => f.role !== "inbox");
  const messages = folders.reduce((sum, f) => sum + f.live, 0);
  if (plain.length === 0) {
    return t("setup.census.inboxOnly", { messages: messagesText(messages) });
  }
  return t("setup.census.found", {
    messages: messagesText(messages),
    folders: foldersText(folders.length),
  });
}
