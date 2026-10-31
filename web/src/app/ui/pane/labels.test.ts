// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import type { Area, Label } from "../../../core/jmap/types";
import { t } from "../../i18n";
import { areaProvenanceKey, labelColor, messageLabels } from "./labels";
import type { LabelledMessage } from "./labels";

function label(id: string, name: string, keyword: string | null, color: string | null = null): Label {
  return { id, name, color, parentId: null, mirrorToImap: true, keyword, tab: null };
}

function area(id: string, name: string, labelId: string): Area {
  return {
    id,
    name,
    position: 0,
    labelId,
    tabs: [],
    labelIds: [],
    autoArchive: true,
    autoArchiveDays: null,
    hiddenFromRail: false,
  };
}

const LABELS: Record<string, Label> = {
  LP: label("LP", "Personal", "MailroomLabel-personal"),
  LW: label("LW", "Work", "MailroomLabel-work"),
  LR: label("LR", "Receipts", "MailroomLabel-receipts", "#123456"),
  LL: label("LL", "Local only", null),
};

const AREAS: Record<string, Area> = { A1: area("A1", "Personal", "LP") };

function msg(over: Partial<LabelledMessage> = {}): LabelledMessage {
  return { area: "A1", keywords: {}, labelProvenance: null, ...over };
}

describe("messageLabels", () => {
  it("names the area from Email.area, through the Area the client holds", () => {
    const got = messageLabels(msg(), LABELS, AREAS);
    expect(got.area?.areaId).toBe("A1");
    expect(got.area?.area?.name).toBe("Personal");
    expect(got.area?.label?.id).toBe("LP");
  });

  it("NEVER lists the area's own label among the others", () => {
    // The load-bearing rule: an area OWNS a label, so a message in the area
    // always carries that keyword. Printing it as an outlined chip as well
    // would show one fact twice — and it is what probe 2 of #375 increment 5
    // deliberately breaks to prove this test can fail.
    const got = messageLabels(
      msg({ keywords: { "MailroomLabel-personal": true, "MailroomLabel-work": true } }),
      LABELS,
      AREAS,
    );
    expect(got.others.map((l) => l.id)).toEqual(["LW"]);
    expect(got.others.map((l) => l.id)).not.toContain("LP");
  });

  it("reads the provenance of the AREA'S OWN label, not of some other one", () => {
    const got = messageLabels(
      msg({
        keywords: { "MailroomLabel-personal": true, "MailroomLabel-work": true },
        labelProvenance: [
          { keyword: "MailroomLabel-work", appliedBy: "user", rule: null },
          { keyword: "MailroomLabel-personal", appliedBy: "rule", rule: { id: "R1", name: "r" } },
        ],
      }),
      LABELS,
      AREAS,
    );
    expect(got.area?.provenance).toBe("rule");
  });

  it("says nothing about provenance when the server said nothing", () => {
    const got = messageLabels(msg({ keywords: { "MailroomLabel-personal": true } }), LABELS, AREAS);
    expect(got.area?.provenance).toBeNull();
  });

  it("has no area chip when `area` was never fetched — it does not guess one", () => {
    // Email.area is opt-in and body-only. A message that only ever came back
    // from a LIST query has no area property at all, and inferring one from
    // whichever label happens to belong to an area would be a claim the server
    // never made.
    const got = messageLabels(
      { keywords: { "MailroomLabel-personal": true } },
      LABELS,
      AREAS,
    );
    expect(got.area).toBeNull();
    // ...and with no area to exclude, the label is an ordinary chip.
    expect(got.others.map((l) => l.id)).toEqual(["LP"]);
  });

  it("has no area chip for Needs sorting (area: null)", () => {
    const got = messageLabels(msg({ area: null }), LABELS, AREAS);
    expect(got.area).toBeNull();
  });

  it("carries the area id even when the Area row has not loaded", () => {
    const got = messageLabels(msg({ area: "A9" }), LABELS, {});
    expect(got.area?.areaId).toBe("A9");
    expect(got.area?.area).toBeNull();
    expect(got.area?.label).toBeNull();
  });

  it("skips a keyword with no Label row rather than drawing a nameless chip", () => {
    const got = messageLabels(
      msg({ area: null, keywords: { "MailroomLabel-work": true, "MailroomLabel-ghost": true } }),
      LABELS,
      AREAS,
    );
    expect(got.others.map((l) => l.id)).toEqual(["LW"]);
  });

  it("ignores a local-only label (no keyword atom can be on the message)", () => {
    const got = messageLabels(msg({ area: null, keywords: {} }), LABELS, AREAS);
    expect(got.others).toEqual([]);
  });

  it("orders the others by name, so the row does not reshuffle between renders", () => {
    const got = messageLabels(
      msg({
        area: null,
        keywords: {
          "MailroomLabel-work": true,
          "MailroomLabel-receipts": true,
          "MailroomLabel-personal": true,
        },
      }),
      LABELS,
      AREAS,
    );
    expect(got.others.map((l) => l.name)).toEqual(["Personal", "Receipts", "Work"]);
  });
});

describe("area provenance (#714)", () => {
  const LOCAL_AREAS: Record<string, Area> = { A2: area("A2", "Updates", "LL") };

  it("reads Email.areaProvenance for a LOCAL-ONLY area label, which has no keyword", () => {
    // The case labelProvenance could never answer: an area's own label is
    // local-only (ADR-031), so there is no keyword to look it up by.
    const got = messageLabels(
      { area: "A2", areaProvenance: "learned", keywords: {}, labelProvenance: null },
      LABELS,
      LOCAL_AREAS,
    );
    expect(got.area?.provenance).toBe("learned");
  });

  it("prefers the server's areaProvenance over a keyword lookup", () => {
    const got = messageLabels(
      msg({
        areaProvenance: "user",
        keywords: { "MailroomLabel-personal": true },
        labelProvenance: [{ keyword: "MailroomLabel-personal", appliedBy: "rule", rule: null }],
      }),
      LABELS,
      AREAS,
    );
    expect(got.area?.provenance).toBe("user");
  });

  it('says "learned from your moves" for a learned placement', () => {
    expect(t(areaProvenanceKey("learned"))).toBe("learned from your moves");
  });

  it("keeps every other producer's words, and a default for one it does not know", () => {
    expect(t(areaProvenanceKey("user"))).toBe("you placed it");
    expect(t(areaProvenanceKey("rule"))).toBe("by rule");
    expect(t(areaProvenanceKey("desk"))).toBe("by the desk's plan");
    expect(t(areaProvenanceKey("agent"))).toBe("filed by an AI tool (API key)");
    expect(t(areaProvenanceKey("something-new"))).toBe("automatically");
    expect(t(areaProvenanceKey(null))).toBe("automatically");
  });
});

describe("labelColor", () => {
  it("uses the label's own colour when it has one", () => {
    expect(labelColor(LABELS["LR"] as Label, 0)).toBe("#123456");
  });

  it("cycles the four palette slots by position otherwise", () => {
    const l = LABELS["LW"] as Label;
    expect(labelColor(l, 0)).toBe("var(--label-1)");
    expect(labelColor(l, 3)).toBe("var(--label-4)");
    expect(labelColor(l, 4)).toBe("var(--label-1)");
  });
});
