// SPDX-License-Identifier: AGPL-3.0-only

// Tests for how the suggestion strip renders a confidence, split by producer
// (#242). This is the client half of the rule-sourced-suggestions surface: the
// server can now fill label_suggestions.confidence with either a calibrated T1
// probability ("model") or an evidence rank with no calibration behind it at
// all ("rule"), and the two must render as visibly different things — a
// percentage claims a precision the rule path never earned.
//
// suggestionConfidenceDisplay is tested directly, DOM-free, rather than through
// a rendered SuggestionStrip: this repo's web tests are all pure-function tests
// (no React Testing Library / jsdom is installed), and the function is where
// the load-bearing decision — never a percent sign for a rule suggestion —
// actually lives. SuggestionStrip itself just prints whatever this returns.

import { describe, expect, it } from "vitest";

import { confidencePercent, suggestionConfidenceDisplay } from "./NeedsCategory";
import type { PriorityReason, SuggestedLabel } from "../../core/jmap/types";

function reason(label: string): PriorityReason {
  return { label, weight: 1, polarity: "up" };
}

function modelSuggestion(confidence: number): SuggestedLabel {
  return { labelId: "L", confidence, source: "model", reasons: null };
}

function ruleSuggestion(confidence: number, reasons: PriorityReason[] | null): SuggestedLabel {
  return { labelId: "L", confidence, source: "rule", reasons };
}

describe("suggestionConfidenceDisplay", () => {
  it("renders a model suggestion as a percentage, and nothing else", () => {
    const { percentText, evidenceText } = suggestionConfidenceDisplay(modelSuggestion(0.67));
    expect(percentText).toBe("67%");
    expect(evidenceText).toBeNull();
  });

  it("NEVER renders a percentage for a rule suggestion — the load-bearing rule of #242", () => {
    // A rule's `confidence` is an evidence rank, not a calibrated probability
    // (see SuggestedLabel's doc comment). Whatever evidence the rule carries,
    // and whatever confidence value the server sent alongside it, percentText
    // must stay null: nothing downstream should ever be able to reconstruct a
    // percent sign for a rule-sourced suggestion, including at extreme or
    // suspicious-looking evidence-rank values.
    for (const confidence of [0, 0.01, 0.5, 0.99, 1, 4, 100]) {
      const { percentText } = suggestionConfidenceDisplay(
        ruleSuggestion(confidence, [reason("backed by 4 messages you filed under this label")]),
      );
      expect(percentText).toBeNull();
    }
  });

  it("surfaces the server's evidence phrase — the LAST reasons entry — for a rule suggestion", () => {
    const { evidenceText } = suggestionConfidenceDisplay(
      ruleSuggestion(3, [reason("alias is shop@example.com"), reason("backed by 4 messages you filed under this label")]),
    );
    // The last entry, not the first: the rule inducer appends the evidence
    // summary after the conjuncts that produced it, and taking the first would
    // show a condition instead of the count that justifies the ranking.
    expect(evidenceText).toBe("backed by 4 messages you filed under this label");
  });

  it("falls back to a fixed string, never a blank, when a rule suggestion carries no reasons", () => {
    // The server's own contract concedes this can happen (reasons null or
    // empty predates migration 00019) — it must still never regress to a
    // percentage or to nothing at all.
    for (const reasons of [null, []] as (PriorityReason[] | null)[]) {
      const { percentText, evidenceText } = suggestionConfidenceDisplay(ruleSuggestion(2, reasons));
      expect(percentText).toBeNull();
      expect(evidenceText).toBe("matches a filing rule you have");
    }
  });
});

describe("confidencePercent (unchanged by #242)", () => {
  it("rounds to a whole-number percentage", () => {
    expect(confidencePercent(0.674)).toBe(67);
    expect(confidencePercent(0.005)).toBe(1);
    expect(confidencePercent(1)).toBe(100);
  });
});
