// SPDX-License-Identifier: AGPL-3.0-only

// The search-options form's query <-> fields mapping (#724). The property that
// matters most is the one a user never sees until it bites: reading a query
// into the form and writing it back must never lose or change any of it.

import { describe, expect, it } from "vitest";

import { ATTACHMENT_VALUES, EMPTY_FORM, WITHIN_VALUES, formToQuery, queryToForm } from "./searchForm";
import type { SearchFormFields } from "./searchForm";
import { hasTopLevelOr, segments } from "./searchQuery";

/** Every combination of a few values per field, including the awkward ones:
 * a value with spaces, a phrase, an OR, an operator the form has no field for,
 * an exclusion with a field in it. */
function* everyForm(): Generator<SearchFormFields> {
  const opts = {
    from: ["", "alice@example.com", "Acme Billing"],
    to: ["", "bob@example.org"],
    subject: ["", "invoice", "end of year"],
    hasWords: ["", "quarterly report", '"exact phrase"', "a OR b", "is:unread label:Work"],
    doesntHave: ["", "spam", '"bad phrase" from:x', "c OR d"],
    within: ["", "1w"] as const,
    larger: ["", "5M"],
    smaller: ["", "100K"],
    area: ["", "Side Projects"],
    attachment: ["", "any", "pdf"] as const,
    includeTrashJunk: [false, true],
  };
  for (const from of opts.from)
    for (const to of opts.to)
      for (const subject of opts.subject)
        for (const hasWords of opts.hasWords)
          for (const doesntHave of opts.doesntHave)
            for (const within of opts.within)
              for (const larger of opts.larger)
                for (const smaller of opts.smaller)
                  for (const area of opts.area)
                    for (const attachment of opts.attachment)
                      for (const includeTrashJunk of opts.includeTrashJunk)
                        yield { from, to, subject, hasWords, doesntHave, within, larger, smaller, area, attachment, includeTrashJunk };
}

/** What a form is expected to read back as: identical, except that "Has the
 * words" holding a top-level OR is bracketed when anything else is written
 * beside it — the bracket is what keeps the OR from swallowing its neighbours,
 * so it has to survive the round trip. */
function expectedBack(f: SearchFormFields): SearchFormFields {
  const others = formToQuery({ ...f, hasWords: "" }) !== "";
  let out = others && hasTopLevelOr(f.hasWords) ? { ...f, hasWords: `(${f.hasWords})` } : f;
  // Excluding a disjunction excludes it as one bracketed part, `-(c OR d)`,
  // which reads back as that part.
  if (hasTopLevelOr(f.doesntHave)) out = { ...out, doesntHave: `(${f.doesntHave})` };
  return out;
}

describe("form -> query -> form, exhaustively", () => {
  it("reads back every combination of fields it wrote", () => {
    let n = 0;
    for (const f of everyForm()) {
      const q = formToQuery(f);
      const back = queryToForm(q);
      if (JSON.stringify(back) !== JSON.stringify(expectedBack(f))) {
        // One clear failure rather than thousands.
        expect({ query: q, back }).toEqual({ query: q, back: expectedBack(f) });
      }
      // And the query is a fixed point: writing what was read writes the same query.
      if (formToQuery(back) !== q) expect(formToQuery(back)).toBe(q);
      n++;
    }
    // ⚠ A count, so a generator that yields nothing cannot pass this vacuously.
    expect(n).toBe(3 * 2 * 3 * 5 * 4 * 2 * 2 * 2 * 2 * 3 * 2);
    // 34,560 round trips: about 1.5 s alone, but over the 5 s default on a loaded
    // CI runner (it timed out at 5,065 ms on PR #832, a change that never touched it).
  }, 30_000);

  it("writes every select value the form offers as an operator the form reads back", () => {
    for (const within of WITHIN_VALUES) {
      expect(queryToForm(formToQuery({ ...EMPTY_FORM, within })).within).toBe(within);
    }
    for (const attachment of ATTACHMENT_VALUES) {
      expect(queryToForm(formToQuery({ ...EMPTY_FORM, attachment })).attachment).toBe(attachment);
    }
  });
});

/** A query's parts as a multiset, operator names case-folded — what must be
 * preserved, in some order, by query -> form -> query. */
function parts(q: string): string[] {
  return segments(q)
    .map((s) => {
      const colon = s.raw.indexOf(":");
      return s.kind === "word" && s.field !== "" ? s.raw.slice(0, colon).toLowerCase() + s.raw.slice(colon) : s.raw;
    })
    .sort();
}

const CORPUS = [
  "",
  "invoice",
  "from:alice@example.com invoice",
  'from:"Acme Billing" subject:"end of year" newer_than:1w',
  "FROM:alice@example.com Subject:Tax",
  "from:a from:b",
  'subject:"invoice"',
  "subject:invoice*",
  "newer_than:5d",
  "newer_than:1w newer_than:2w",
  "older_than:1y larger:5M smaller:10M",
  "has:attachment has:pdf",
  "has:unsubscribe list:news.example.com",
  "in:anywhere in:trash",
  "in:junk",
  "area:Work area:Home",
  'area:"Side Projects" label:"Big Project"',
  "a OR b from:x",
  "from:x OR from:y has:pdf",
  "(a OR b) from:x",
  "-(a OR b) -from:spam@example.com -newsletter -\"bad phrase\"",
  "((nested (deep))) is:unread",
  "(unclosed group from:x",
  'unbalanced "quote from:x',
  "- lonely-hyphen OR",
  "https://example.com/x <id@example.com>",
  "is:snoozed is:muted is:pinned is:important",
  "filename:report.pdf deliveredto:me@example.com alias:you@example.com",
];

describe("query -> form -> query never drops anything", () => {
  for (const q of CORPUS) {
    it(JSON.stringify(q), () => {
      const out = formToQuery(queryToForm(q));
      expect(parts(out)).toEqual(parts(q));
      // A top-level OR lifts nothing and keeps the order exactly.
      if (hasTopLevelOr(q)) expect(out).toBe(segments(q).map((s) => s.raw).join(" "));
      // Stable after one pass.
      expect(formToQuery(queryToForm(out))).toBe(out);
    });
  }
});

describe("queryToForm: what is and is not lifted", () => {
  it("lifts the simple top-level operators into their fields", () => {
    expect(
      queryToForm(
        'from:alice@example.com to:bob@example.org subject:"end of year" quarterly -spam newer_than:1m larger:5M smaller:1G area:"Side Projects" has:pdf in:anywhere',
      ),
    ).toEqual({
      from: "alice@example.com",
      to: "bob@example.org",
      subject: "end of year",
      hasWords: "quarterly",
      doesntHave: "spam",
      within: "1m",
      larger: "5M",
      smaller: "1G",
      area: "Side Projects",
      attachment: "pdf",
      includeTrashJunk: true,
    });
  });

  it("lifts nothing from a query with a top-level OR", () => {
    const f = queryToForm("from:x invoice OR receipt");
    expect(f).toEqual({ ...EMPTY_FORM, hasWords: "from:x invoice OR receipt" });
  });

  it("keeps a field given twice entirely in the words, in order", () => {
    expect(queryToForm("from:a b from:c")).toMatchObject({ from: "", hasWords: "from:a b from:c" });
    expect(queryToForm("has:pdf has:attachment")).toMatchObject({ attachment: "", hasWords: "has:pdf has:attachment" });
  });

  it("keeps a phrase-quoted single word as typed rather than turning it into a token search", () => {
    expect(queryToForm('subject:"invoice"')).toMatchObject({ subject: "", hasWords: 'subject:"invoice"' });
  });

  it("keeps a date the select cannot show in the words", () => {
    expect(queryToForm("newer_than:5d")).toMatchObject({ within: "", hasWords: "newer_than:5d" });
  });

  it("keeps operators the form has no field for in the words", () => {
    expect(queryToForm("in:trash has:unsubscribe is:unread label:Work")).toMatchObject({
      includeTrashJunk: false,
      attachment: "",
      hasWords: "in:trash has:unsubscribe is:unread label:Work",
    });
  });

  it("does not look inside a group", () => {
    expect(queryToForm("(from:x invoice) to:y")).toMatchObject({ from: "", to: "y", hasWords: "(from:x invoice)" });
  });

  it("puts every negated part in Doesn't have and gives it back its minus", () => {
    const q = '-newsletter -"bad phrase" -from:x -(a OR b)';
    const f = queryToForm(q);
    expect(f.doesntHave).toBe('newsletter "bad phrase" from:x (a OR b)');
    expect(formToQuery(f)).toBe(q);
  });
});

describe("formToQuery", () => {
  it("quotes values that need it and only those", () => {
    expect(formToQuery({ ...EMPTY_FORM, from: "Acme Billing", to: "b@example.org", area: "Work (old)" })).toBe(
      'from:"Acme Billing" to:b@example.org area:"Work (old)"',
    );
  });

  it("brackets an OR in the words when anything else is written beside it", () => {
    expect(formToQuery({ ...EMPTY_FORM, hasWords: "invoice OR receipt", from: "x" })).toBe(
      "from:x (invoice OR receipt)",
    );
    expect(formToQuery({ ...EMPTY_FORM, hasWords: "invoice OR receipt" })).toBe("invoice OR receipt");
  });

  it("excludes an OR as a whole", () => {
    expect(formToQuery({ ...EMPTY_FORM, doesntHave: "a OR b" })).toBe("-(a OR b)");
  });

  it("does not double a minus the user typed into Doesn't have", () => {
    expect(formToQuery({ ...EMPTY_FORM, doesntHave: "-spam ham" })).toBe("-spam -ham");
  });

  it("writes nothing for an empty form", () => {
    expect(formToQuery(EMPTY_FORM)).toBe("");
  });
});
