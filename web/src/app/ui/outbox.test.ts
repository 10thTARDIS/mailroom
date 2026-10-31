// SPDX-License-Identifier: AGPL-3.0-only

// The Outbox's rows (#448).
//
// THE FILTER IS THE LOAD-BEARING PART, and both halves of it are negative
// assertions:
//
//   * a `final` submission has GONE. Listing it would put an Undo button on a
//     message already delivered — and the server accepts `undoStatus: "canceled"`
//     only, and only while the job is pending (internal/api/submission.go), so
//     the button could not do what it says.
//   * a `pending` submission whose `sendAt` has passed is going out right now. Its
//     row would count down through zero and stick there.
//
// Ordering is asserted too, because "newest first" is a claim about ULIDs rather
// than about `sendAt` — see outbox.ts's header.

import { describe, expect, it } from "vitest";

import sidebarSource from "./Sidebar.tsx?raw";
import { outboxCount, outboxMissingEmailIds, outboxRows } from "./outbox";
import type { Email, EmailSubmission, Identity } from "../../core/jmap/types";

const NOW = Date.parse("2026-09-08T12:00:00Z");
const ACC = "01JBQZ3K7WX2N4Y6P8R0S1T2A0";

/** ULIDs in creation order — the ordering claim depends on that, so the fixture
 * has to have it. */
const S1 = "01JBQZ3K7WX2N4Y6P8R0S00001";
const S2 = "01JBQZ3K7WX2N4Y6P8R0S00002";
const S3 = "01JBQZ3K7WX2N4Y6P8R0S00003";

function submission(over: Partial<EmailSubmission> & Pick<EmailSubmission, "id">): EmailSubmission {
  return {
    identityId: "i-house",
    emailId: `e-${over.id}`,
    threadId: null,
    sendAt: new Date(NOW + 20_000).toISOString(),
    undoStatus: "pending",
    ...over,
  };
}

function email(over: Partial<Email> & Pick<Email, "id">): Email {
  return {
    blobId: "B1",
    threadId: "t1",
    mailboxIds: {},
    keywords: { $draft: true },
    size: 100,
    receivedAt: "2026-09-08T11:59:00Z",
    sentAt: null,
    subject: "Hello",
    preview: "",
    hasAttachment: false,
    from: null,
    messageId: null,
    inReplyTo: null,
    references: [],
    deliveredTo: null,
    identityId: null,
    ...over,
  };
}

function identity(over: Partial<Identity> & Pick<Identity, "id" | "email">): Identity {
  return {
    name: "",
    replyTo: null,
    bcc: null,
    textSignature: "",
    htmlSignature: "",
    mayDelete: true,
    accountId: ACC,
    isWildcard: false,
    enabled: true,
    ...over,
  };
}

const identities = { "i-house": identity({ id: "i-house", email: "house@example.com" }) };

describe("outboxRows", () => {
  it("lists a pending submission with its recipients, subject and alias", () => {
    const rows = outboxRows(
      { [S1]: submission({ id: S1 }) },
      {
        [`e-${S1}`]: email({
          id: `e-${S1}`,
          subject: "The kitchen",
          to: [{ name: "A friend", email: "friend@example.com" }],
          cc: [{ name: null, email: "cc@example.com" }],
        }),
      },
      identities,
      NOW,
    );
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({
      id: S1,
      // #600: the address is shown too, not just the display name. This line
      // asserted ["A friend", ...] and was encoding the defect.
      recipients: ["A friend <friend@example.com>", "cc@example.com"],
      subject: "The kitchen",
      alias: "house@example.com",
      loaded: true,
      state: { kind: "window", secondsLeft: 20 },
    });
  });

  // ⚠ THE CONTROL FOR THE FILTER. Without it, "lists what is pending" could be
  // "lists everything" and the test above would still pass.
  it("excludes a final submission — it has gone, and there is nothing to undo", () => {
    const rows = outboxRows(
      {
        [S1]: submission({ id: S1 }),
        [S2]: submission({ id: S2, undoStatus: "final" }),
        [S3]: submission({ id: S3, undoStatus: "canceled" }),
      },
      {},
      identities,
      NOW,
    );
    expect(rows.map((r) => r.id)).toEqual([S1]);
  });

  // ⚠ THESE TWO ASSERTED THE OPPOSITE UNTIL #589, and the change is deliberate.
  //
  // Both used to require the row be DROPPED. The reasoning was sound for a view
  // that could only render a countdown: a submission whose instant has passed
  // "would count down through zero", and one with an unreadable time "has
  // nothing to say". Neither objection survives a state that carries no number.
  //
  // What the old assertions cost is the point. They are the same shape as the
  // defect #589 records — a send the user made, held in the store, discarded by
  // this function, reported by nothing — and the lesson there was that hiding a
  // send is never the safe direction. So the scenarios are kept and the
  // expectations inverted, rather than the tests deleted.

  it("shows a pending submission whose sendAt has already passed as sending", () => {
    // It is going out right now, or it failed transiently and the job has it
    // queued for another attempt (compose.SendWorker.requeue puts it back to
    // 'scheduled' keeping its original, now-past, sendAt). Either way the user
    // has mail on its way out and is entitled to see it.
    const rows = outboxRows(
      {
        [S1]: submission({ id: S1, sendAt: new Date(NOW - 1_000).toISOString() }),
        [S2]: submission({ id: S2 }),
      },
      {},
      identities,
      NOW,
    );
    expect(rows.map((r) => r.id)).toEqual([S2, S1]);
    expect(rows.find((r) => r.id === S1)?.state).toEqual({ kind: "sending" });
  });

  it("shows a submission whose sendAt cannot be read as sending, rather than hiding it", () => {
    const rows = outboxRows({ [S1]: submission({ id: S1, sendAt: "not a date" }) }, {}, identities, NOW);
    expect(rows).toHaveLength(1);
    expect(rows[0]!.state).toEqual({ kind: "sending" });
  });

  it("reads a countdown inside the window and a schedule outside it", () => {
    const rows = outboxRows(
      {
        [S1]: submission({ id: S1, sendAt: new Date(NOW + 14_000).toISOString() }),
        [S2]: submission({ id: S2, sendAt: new Date(NOW + 6 * 60_000).toISOString() }),
      },
      {},
      identities,
      NOW,
    );
    const byId = new Map(rows.map((r) => [r.id, r.state]));
    expect(byId.get(S1)).toEqual({ kind: "window", secondsLeft: 14 });
    expect(byId.get(S2)).toEqual({
      kind: "scheduled",
      sendAt: new Date(NOW + 6 * 60_000).toISOString(),
    });
  });

  it("puts the five-minute boundary on the schedule side", () => {
    const rows = outboxRows(
      { [S1]: submission({ id: S1, sendAt: new Date(NOW + 5 * 60_000).toISOString() }) },
      {},
      identities,
      NOW,
    );
    expect(rows[0]?.state.kind).toBe("scheduled");
  });

  it("orders newest first by id, not by sendAt", () => {
    // S1 was created first and is scheduled for next week; S3 was created last
    // and goes out in seconds. Sorting on sendAt would put next week at the top
    // of a list about what is happening now.
    const rows = outboxRows(
      {
        [S1]: submission({ id: S1, sendAt: new Date(NOW + 7 * 86_400_000).toISOString() }),
        [S2]: submission({ id: S2, sendAt: new Date(NOW + 3_600_000).toISOString() }),
        [S3]: submission({ id: S3, sendAt: new Date(NOW + 15_000).toISOString() }),
      },
      {},
      identities,
      NOW,
    );
    expect(rows.map((r) => r.id)).toEqual([S3, S2, S1]);
  });

  it("keeps a row whose draft has not loaded, and says so", () => {
    const rows = outboxRows({ [S1]: submission({ id: S1 }) }, {}, identities, NOW);
    expect(rows[0]).toMatchObject({ loaded: false, recipients: [], subject: null });
  });

  it("reports a null alias for an identity that is not in the store", () => {
    const rows = outboxRows({ [S1]: submission({ id: S1, identityId: "i-gone" }) }, {}, identities, NOW);
    expect(rows[0]?.alias).toBeNull();
  });

  it("reports a null subject for a blank one rather than an empty string", () => {
    const rows = outboxRows(
      { [S1]: submission({ id: S1 }) },
      { [`e-${S1}`]: email({ id: `e-${S1}`, subject: "   " }) },
      identities,
      NOW,
    );
    expect(rows[0]?.subject).toBeNull();
  });
});

describe("outboxMissingEmailIds", () => {
  // ⚠ "e-loaded" CARRIES A RECIPIENT DELIBERATELY (#593). These two tests mean
  // "a draft that IS loaded", and since `loaded` now means "its recipients have
  // been fetched" rather than "an Email object exists", a fixture built from the
  // cheap list properties alone no longer represents one. The fixture is
  // re-armed to say what the test always meant; the assertions are untouched.
  it("names each unloaded draft once", () => {
    const rows = outboxRows(
      {
        [S1]: submission({ id: S1, emailId: "e-shared" }),
        [S2]: submission({ id: S2, emailId: "e-shared" }),
        [S3]: submission({ id: S3, emailId: "e-loaded" }),
      },
      { "e-loaded": email({ id: "e-loaded", to: [{ name: null, email: "them@example.net" }] }) },
      identities,
      NOW,
    );
    expect(outboxMissingEmailIds(rows)).toEqual(["e-shared"]);
  });

  it("asks for nothing when every draft is in the store", () => {
    const rows = outboxRows(
      { [S1]: submission({ id: S1, emailId: "e-loaded" }) },
      { "e-loaded": email({ id: "e-loaded", to: [{ name: null, email: "them@example.net" }] }) },
      identities,
      NOW,
    );
    expect(outboxMissingEmailIds(rows)).toEqual([]);
  });
});

// --- #589: a failed send must be visible, and must leave on its own -----------
//
// The defect these pin: a permanently failed submission was dropped by BOTH
// filters independently (`undoStatus` is `final`, and `sendAt` is necessarily in
// the past once the job has run), so a refused send appeared nowhere and the
// rail badge — computed from these rows — stayed silent. Live evidence: two
// forwards refused with `550 5.7.1 sender address rejected: not allowed`, found
// only in the provider's webmail.

describe("a permanently failed send", () => {
  const failed = submission({
    id: S1,
    undoStatus: "final",
    mailroomState: "failed",
    // In the past, as it must be: the job only runs after sendAt.
    sendAt: new Date(NOW - 60_000).toISOString(),
    deliveryStatus: {
      "*": { smtpReply: '550 "5.7.1 sender address rejected: not allowed"', delivered: "no" },
    },
  });

  it("is listed, with the provider's own words", () => {
    const rows = outboxRows({ [S1]: failed }, {}, {}, NOW);
    expect(rows).toHaveLength(1);
    expect(rows[0]!.state).toEqual({
      kind: "failed",
      reply: '550 "5.7.1 sender address rejected: not allowed"',
    });
  });

  it("is still listed when sendAt is unreadable", () => {
    // The pending states drop an unparseable time because their whole content IS
    // a time. A failure's content is the provider's message, so discarding it
    // over a bad timestamp would reintroduce the silence.
    const rows = outboxRows(
      { [S1]: { ...failed, sendAt: "not a date" } },
      {},
      {},
      NOW,
    );
    expect(rows).toHaveLength(1);
    expect(rows[0]!.state.kind).toBe("failed");
  });

  it("reports no reply rather than inventing one when the server gave none", () => {
    const rows = outboxRows({ [S1]: { ...failed, deliveryStatus: undefined } }, {}, {}, NOW);
    expect(rows[0]!.state).toEqual({ kind: "failed", reply: null });
  });

  it("prefers a named recipient's verdict over the wildcard once a DSN has arrived", () => {
    const rows = outboxRows(
      {
        [S1]: {
          ...failed,
          deliveryStatus: {
            "*": { smtpReply: "250 queued", delivered: "queued" },
            "them@example.net": { smtpReply: "5.1.1 no such user", delivered: "no" },
          },
        },
      },
      {},
      {},
      NOW,
    );
    expect(rows[0]!.state).toEqual({ kind: "failed", reply: "5.1.1 no such user" });
  });

  // ⚠ THE CONTROL. Without this, "a failed send is listed" would also pass for an
  // implementation that listed EVERY submission — including delivered mail, which
  // would put a failure notice on every message the user successfully sent.
  it("does NOT list a send that succeeded", () => {
    const sent = submission({
      id: S2,
      undoStatus: "final",
      mailroomState: "sent",
      sendAt: new Date(NOW - 60_000).toISOString(),
      deliveryStatus: { "*": { smtpReply: "250 2.0.0 Ok", delivered: "yes" } },
    });
    expect(outboxRows({ [S2]: sent }, {}, {}, NOW)).toHaveLength(0);
  });

  // ⚠ THE OTHER CONTROL. A cancelled send did not go and never will, but it is
  // not a failure and must not be reported as one.
  it("does NOT list a cancelled send", () => {
    const cancelled = submission({
      id: S2,
      undoStatus: "canceled",
      mailroomState: "canceled",
      sendAt: new Date(NOW - 60_000).toISOString(),
    });
    expect(outboxRows({ [S2]: cancelled }, {}, {}, NOW)).toHaveLength(0);
  });

  // This is the exit condition, and the reason there is no dismiss button: the
  // operator's requirement was that the banner "go away after the message has
  // been sent", not that each failure be cleared by hand.
  it("stops being listed once the message is re-sent successfully", () => {
    const resent = submission({
      id: S2, // newer ULID, same message
      emailId: failed.emailId,
      undoStatus: "final",
      mailroomState: "sent",
      sendAt: new Date(NOW - 10_000).toISOString(),
    });
    const rows = outboxRows({ [S1]: failed, [S2]: resent }, {}, {}, NOW);
    expect(rows).toHaveLength(0);
  });

  it("is still listed while a re-send is only queued, not yet sent", () => {
    const retry = submission({
      id: S2,
      emailId: failed.emailId,
      undoStatus: "pending",
      sendAt: new Date(NOW + 20_000).toISOString(),
    });
    // The newer submission speaks for the message, and it has not gone: the row
    // is the pending one, not the stale failure.
    const rows = outboxRows({ [S1]: failed, [S2]: retry }, {}, {}, NOW);
    expect(rows).toHaveLength(1);
    expect(rows[0]!.state.kind).toBe("window");
  });
});

describe("a send whose moment has passed", () => {
  // Previously dropped by `sendAt > now`, so a send in flight — or one requeued
  // after a transient failure, which compose.SendWorker.requeue returns to
  // 'scheduled' with its original past sendAt — was invisible too.
  it("reads as sending rather than counting down through zero", () => {
    const inFlight = submission({
      id: S1,
      undoStatus: "pending",
      sendAt: new Date(NOW - 5_000).toISOString(),
    });
    const rows = outboxRows({ [S1]: inFlight }, {}, {}, NOW);
    expect(rows).toHaveLength(1);
    expect(rows[0]!.state).toEqual({ kind: "sending" });
  });

  it("still counts down while the moment is genuinely ahead", () => {
    const soon = submission({ id: S1, sendAt: new Date(NOW + 20_000).toISOString() });
    const rows = outboxRows({ [S1]: soon }, {}, {}, NOW);
    expect(rows[0]!.state).toEqual({ kind: "window", secondsLeft: 20 });
  });
});

// --- #593: a draft already on screen must still name its recipients ----------
//
// `to`/`cc`/`bcc` are body-only server-side and absent from EMAIL_LIST_PROPERTIES,
// so a draft opened in Drafts sits in the store WITHOUT them. `loaded` used to
// mean "an Email object exists", which was true, so the targeted fetch that does
// ask for recipients was skipped and the row printed "no recipients" forever.
//
// Observed live on v1.73.0: a re-sent draft showing its subject correctly beside
// "no recipients" — a genuinely unloaded row has neither, which is what
// identified the cause.

describe("a draft already in the store from a list view", () => {
  const sub = submission({ id: S1 });

  /** What a list view leaves behind: subject and from, no recipients. */
  const cheap = email({ id: `e-${S1}`, subject: "Already on screen" });

  it("is not treated as loaded, because its recipients were never fetched", () => {
    const rows = outboxRows({ [S1]: sub }, { [`e-${S1}`]: cheap }, identities, NOW);
    expect(rows[0]!.loaded).toBe(false);
    // The subject IS known and is still shown — only the recipients are missing,
    // which is exactly what the live row looked like.
    expect(rows[0]!.subject).toBe("Already on screen");
  });

  it("is asked for, so the row stops saying no recipients", () => {
    const rows = outboxRows({ [S1]: sub }, { [`e-${S1}`]: cheap }, identities, NOW);
    expect(outboxMissingEmailIds(rows)).toEqual([`e-${S1}`]);
  });

  // ⚠ THE NEGATIVE CONTROL THE ISSUE ASKS FOR. A fix that simply re-fetched
  // everything would also make the symptom disappear — while re-requesting every
  // row on every tick, forever. This is what says the fix is targeted.
  it("is NOT asked for again once its recipients are known", () => {
    const full = email({
      id: `e-${S1}`,
      subject: "Already on screen",
      to: [{ name: null, email: "them@example.net" }],
    });
    const rows = outboxRows({ [S1]: sub }, { [`e-${S1}`]: full }, identities, NOW);
    expect(rows[0]!.loaded).toBe(true);
    expect(outboxMissingEmailIds(rows)).toEqual([]);
  });

  // ⚠ THE CASE A NAIVE EMPTINESS TEST BREAKS. A Bcc-only message legitimately has
  // no To and no Cc. Testing `!to?.length` would call it unfetched forever and
  // re-request it on every tick — the symptom fixed, a polling loop introduced.
  it("treats a Bcc-only message as loaded, not as unfetched", () => {
    const bccOnly = email({
      id: `e-${S1}`,
      subject: "Quietly",
      to: null,
      cc: null,
      bcc: [{ name: null, email: "hidden@example.net" }],
    });
    const rows = outboxRows({ [S1]: sub }, { [`e-${S1}`]: bccOnly }, identities, NOW);
    expect(rows[0]!.loaded).toBe(true);
    expect(rows[0]!.recipients).toEqual(["hidden@example.net"]);
    expect(outboxMissingEmailIds(rows)).toEqual([]);
  });

  // A message fetched and carrying genuinely no recipients at all is "loaded"
  // with nothing to name — distinct from "we never asked", and it must not loop.
  it("treats a fetched message with no recipients at all as loaded", () => {
    const none = email({ id: `e-${S1}`, subject: "Nobody", to: null, cc: null, bcc: null });
    const rows = outboxRows({ [S1]: sub }, { [`e-${S1}`]: none }, identities, NOW);
    expect(rows[0]!.loaded).toBe(true);
    expect(rows[0]!.recipients).toEqual([]);
    expect(outboxMissingEmailIds(rows)).toEqual([]);
  });

  it("is still not loaded when the message is absent entirely", () => {
    const rows = outboxRows({ [S1]: sub }, {}, identities, NOW);
    expect(rows[0]!.loaded).toBe(false);
    expect(outboxMissingEmailIds(rows)).toEqual([`e-${S1}`]);
  });
});

// ---------------------------------------------------------------------------
// #602: the RAIL COUNT, which used to carry its own copy of the filter.
//
// The count decides whether the Outbox entry exists at all (Sidebar.tsx gates on
// `> 0`), so a count that disagrees with the list is not a wrong badge — it is a
// view the user cannot open. The old copy filtered `undoStatus === "pending"`,
// which is `final` for a failure, so the entry disappeared exactly when a send
// was refused.
//
// ⚠ THE `sent` CASE IS THE CONTROL. A count that had simply stopped filtering
// would satisfy every assertion about failures below AND make the Outbox
// permanently non-empty, which is the thing "appear and disappear with their
// contents" exists to prevent.
describe("outboxCount — the rail entry's existence", () => {
  it("counts a FAILED submission: the door must open for the one event this view is for", () => {
    const subs = {
      [S1]: submission({ id: S1, undoStatus: "final", mailroomState: "failed" }),
    };
    expect(outboxCount(subs)).toBe(1);
  });

  it("does NOT count a sent one — otherwise the Outbox never disappears", () => {
    const subs = { [S1]: submission({ id: S1, undoStatus: "final", mailroomState: "sent" }) };
    expect(outboxCount(subs)).toBe(0);
  });

  it("does NOT count a canceled one", () => {
    const subs = { [S1]: submission({ id: S1, undoStatus: "canceled" }) };
    expect(outboxCount(subs)).toBe(0);
  });

  it("counts a pending one, as it always did", () => {
    expect(outboxCount({ [S1]: submission({ id: S1 }) })).toBe(1);
  });

  it("counts a re-sent message ONCE, not once per attempt", () => {
    // Same emailId: an old failure answered by a newer send. Only the latest
    // speaks for the message — the old count did not collapse these at all.
    const subs = {
      [S1]: submission({ id: S1, emailId: "e-1", undoStatus: "final", mailroomState: "failed" }),
      [S2]: submission({ id: S2, emailId: "e-1" }),
    };
    expect(outboxCount(subs)).toBe(1);
  });

  it("agrees with the list it gates, which is the whole point", () => {
    const subs = {
      [S1]: submission({ id: S1, emailId: "e-1", undoStatus: "final", mailroomState: "failed" }),
      [S2]: submission({ id: S2, emailId: "e-2" }),
      [S3]: submission({ id: S3, emailId: "e-3", undoStatus: "final", mailroomState: "sent" }),
    };
    expect(outboxCount(subs)).toBe(outboxRows(subs, {}, identities, NOW).length);
  });
});

// ---------------------------------------------------------------------------
// #604: WHICH ADDRESS the row says it was sent as.
//
// On a catch-all the identity's own address is the pattern `*@domain`, so naming
// the row by its identity told the operator `as *@example.net` for a message
// sent as `parcel@example.net` — printed directly above "Change what the server
// objected to", for a `550 … sender address rejected`.
//
// ⚠ THE CONCRETE-IDENTITY CASE IS THE CONTROL: it is what shows this is a
// correction rather than a swap that merely happens to look right on the
// catch-all it was found on.
// #600, the half that was missed. The reading pane was fixed and this row was
// not, so an Outbox row still printed a bare display name — on the one screen
// whose job is telling you what happened to a message you SENT, where "who is
// this going to" is the whole question.
describe("outboxRows — the recipients", () => {
  it("names each recipient WITH their address, not just a display name", () => {
    const subs = { [S1]: submission({ id: S1 }) };
    const emails = {
      [`e-${S1}`]: email({
        id: `e-${S1}`,
        to: [{ name: "Dana Vance", email: "dana@example.org" }],
        cc: [{ name: null, email: "cc@example.com" }],
      }),
    };
    expect(outboxRows(subs, emails, identities, NOW)[0]?.recipients).toEqual([
      "Dana Vance <dana@example.org>",
      "cc@example.com",
    ]);
  });
});

describe("outboxRows — the sender address", () => {
  const wildcard = {
    "i-cat": identity({ id: "i-cat", email: "*@example.net", isWildcard: true }),
    "i-house": identity({ id: "i-house", email: "house@example.com" }),
  };

  it("names the DERIVED ALIAS, not the wildcard pattern", () => {
    const subs = {
      [S1]: submission({
        id: S1,
        identityId: "i-cat",
        envelope: { mailFrom: { email: "parcel@example.net" } },
      }),
    };
    expect(outboxRows(subs, {}, wildcard, NOW)[0]?.alias).toBe("parcel@example.net");
  });

  it("is unchanged for a concrete identity", () => {
    const subs = {
      [S1]: submission({
        id: S1,
        identityId: "i-house",
        envelope: { mailFrom: { email: "house@example.com" } },
      }),
    };
    expect(outboxRows(subs, {}, wildcard, NOW)[0]?.alias).toBe("house@example.com");
  });

  it("falls back to the identity when no envelope came back", () => {
    // Not decoration: reading a property the server did not send would render
    // null and make the "as" line vanish, which reads as a tidy-up rather than
    // a regression.
    const subs = { [S1]: submission({ id: S1, identityId: "i-house" }) };
    expect(outboxRows(subs, {}, wildcard, NOW)[0]?.alias).toBe("house@example.com");
  });

  it("falls back when the envelope carries a blank address", () => {
    const subs = {
      [S1]: submission({ id: S1, identityId: "i-house", envelope: { mailFrom: { email: "  " } } }),
    };
    expect(outboxRows(subs, {}, wildcard, NOW)[0]?.alias).toBe("house@example.com");
  });
});

// ---------------------------------------------------------------------------
// #602, the part a unit test of `outboxCount` CANNOT reach.
//
// The defect was never that the count function was wrong — it did not exist.
// `Sidebar.tsx` carried its own copy of the rule, and the copy drifted. A test
// of the shared function passes whether or not anybody calls it, which is
// exactly the hole the original bug lived in.
//
// There is no jsdom/RTL in this repo, so the component cannot be rendered and
// asked. The next best thing is to assert over its SOURCE that the duplicate
// rule has not come back — the same technique internal/config/envdoc_test.go
// uses, and for the same reason: an invariant that spans two files is enforced
// or it is forgotten.
describe("the rail count is not a second copy of the rule", () => {
  /**
   * Comments stripped, because the assertion is about CODE and the first version
   * of this test failed on the comment that explains the fix — which quotes the
   * very expression it forbids. A source-level guard that cannot tell code from
   * prose would make every future explanation of this bug a test failure, and
   * the fix for that would be to stop explaining it.
   *
   * Crude on purpose: it would also blank a comment-like run inside a string
   * literal, which is fine for an assertion about an `=== "pending"` comparison.
   */
  const code = sidebarSource.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^[^\n]*\/\/.*$/gm, "");

  it("Sidebar computes the Outbox count with outboxCount", () => {
    expect(code).toMatch(/outboxCount\(/);
  });

  it("Sidebar does not filter submissions by undoStatus itself", () => {
    // The exact shape that shipped, and the shape any re-duplication would take.
    expect(code).not.toMatch(/undoStatus\s*===\s*["']pending["']/);
  });

  it("the stripper leaves real code alone — otherwise both assertions are vacuous", () => {
    // Without this, a stripper that returned "" would satisfy the negative
    // assertion above and make the positive one the only thing holding.
    expect(code).toMatch(/const outbox = useMemo/);
    expect(code).not.toMatch(/used to filter/);
  });
});

// ---------------------------------------------------------------------------
// DISMISSING A FAILED SEND (migration 00059). A refused send had no way to
// leave: destroy is refused, and discarding the draft only soft-trashes it, so
// the row and the rail's count stayed for good. `dismissedAt` is the user's act.
//
// ⚠ THE UNDISMISSED FAILURE IS THE CONTROL: without it, "a dismissed failure is
// not listed" would pass just as well for an Outbox that had stopped listing
// failures at all — which is #589's bug, the one this view exists to prevent.
describe("a dismissed failed send", () => {
  const failed = (over: Partial<EmailSubmission> & Pick<EmailSubmission, "id">): EmailSubmission =>
    submission({ undoStatus: "final", mailroomState: "failed", ...over });

  it("is not listed, and does not count toward the rail", () => {
    const subs = { [S1]: failed({ id: S1, dismissedAt: "2026-09-24T10:00:00Z" }) };
    expect(outboxRows(subs, {}, identities, NOW)).toEqual([]);
    expect(outboxCount(subs)).toBe(0);
  });

  it("CONTROL: an undismissed failure is still listed and counted (#589)", () => {
    const subs = { [S1]: failed({ id: S1, dismissedAt: null }) };
    expect(outboxRows(subs, {}, identities, NOW)).toHaveLength(1);
    expect(outboxCount(subs)).toBe(1);
  });

  it("hides only the dismissed one when several messages failed", () => {
    const subs = {
      [S1]: failed({ id: S1, emailId: "e-1", dismissedAt: "2026-09-24T10:00:00Z" }),
      [S2]: failed({ id: S2, emailId: "e-2" }),
    };
    expect(outboxRows(subs, {}, identities, NOW).map((r) => r.id)).toEqual([S2]);
    expect(outboxCount(subs)).toBe(1);
  });
});
