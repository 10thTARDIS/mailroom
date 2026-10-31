// SPDX-License-Identifier: AGPL-3.0-only

// WHICH LABELS A MESSAGE WEARS, AND WHICH ONE OF THEM IS ITS AREA (#375
// increment 5).
//
// The reading pane prints two different things that are both, underneath, JMAP
// keywords: the AREA the message is in — one chip, raised, because it answers
// "where on the desk is this" — and every OTHER label it carries, outlined.
// Getting that split right is the whole job of this module, and it is a pure
// function rather than JSX because the interesting cases are all invisible:
//
//   * AN AREA OWNS A LABEL (jmap/types.ts Area.labelId), and placing mail in
//     the area IS applying that label. So the area's own label is ALWAYS on the
//     message when the message is in that area — printing it twice, once as the
//     area chip and again in the outlined row, would show the same fact as two
//     facts. It is excluded from `others` for that reason, and a test holds the
//     line (probe 2 of the increment).
//   * `Email.area` is OPT-IN and body-only (internal/api/area_email.go
//     resolveAreas keys off `properties: ["area"]`). A message fetched for a
//     LIST has no `area` at all, which is `null` here — no chip, rather than a
//     guess from whichever label happens to belong to an area.
//   * The area's provenance is read from `areaProvenance` (#714), the server's
//     answer for the AREA'S OWN LABEL, falling back to `labelProvenance` for
//     that label's keyword: "who put this here" is a question about that one
//     label. The fallback alone never worked for an area's own label, which is
//     local-only (ADR-031) and so has no keyword to look up.

import type { Area, Id, Label, LabelProvenance } from "../../../core/jmap/types";
import type { MessageKey } from "../../i18n";

/** The message shape this needs — deliberately structural, so a test can build
 * one in three lines and `EffectiveEmail` satisfies it without a cast. The
 * keywords are the POST-OVERLAY ones when the caller passes an EffectiveEmail,
 * which is what makes a label applied a moment ago appear immediately. */
export interface LabelledMessage {
  /** The resolved area (opt-in, body-only), or null/absent for Needs sorting
   * — or for a message whose `area` was never asked for. */
  area?: Id | null;
  /** Who applied the resolved area's own label (#714), opt-in like `area`. */
  areaProvenance?: string | null;
  keywords: Record<string, true>;
  labelProvenance?: LabelProvenance[] | null;
}

export interface MessageAreaChip {
  areaId: Id;
  /** The Area record, when this client holds it. Null while areas are still
   * loading — the chip is then omitted rather than drawn with an id for a
   * name. */
  area: Area | null;
  /** The area's OWN label, when this client holds it. */
  label: Label | null;
  /**
   * Who applied the area's own label, straight from `labelProvenance`, or null
   * when the server said nothing about it.
   *
   * A STRING, not the `LabelProvenance["appliedBy"]` union, on purpose: the
   * caller turns it into a sentence through a table with a default clause, so a
   * value this client has never heard of reads as "automatically" instead of
   * failing to typecheck the day the server grows one.
   */
  provenance: string | null;
}

export interface MessageLabels {
  area: MessageAreaChip | null;
  /** Every other label on the message, by name — never including the area's
   * own. */
  others: Label[];
}

/**
 * The area chip and the outlined label chips for one message.
 *
 * `labels` and `areas` are the store's own maps (`state.labels`,
 * `state.areas`). Both are looked up rather than trusted to be complete: a
 * keyword with no Label row is a label this client has not loaded (or a
 * keyword some other client set), and it is skipped rather than rendered as a
 * chip with no name.
 */
export function messageLabels(
  email: LabelledMessage,
  labels: Record<Id, Label>,
  areas: Record<Id, Area>,
): MessageLabels {
  const areaId = email.area ?? null;
  const area = areaId ? areas[areaId] ?? null : null;
  const ownLabel = area ? labels[area.labelId] ?? null : null;

  const chip: MessageAreaChip | null = areaId
    ? {
        areaId,
        area,
        label: ownLabel,
        provenance:
          email.areaProvenance ??
          (ownLabel?.keyword
            ? (email.labelProvenance ?? []).find((p) => p.keyword === ownLabel.keyword)?.appliedBy
            : undefined) ??
          null,
      }
    : null;

  const ownLabelId = area?.labelId ?? null;
  const others = Object.values(labels)
    .filter((l) => l.id !== ownLabelId && !!l.keyword && email.keywords[l.keyword] === true)
    .sort((a, b) => a.name.localeCompare(b.name));

  return { area: chip, others };
}

/** The i18n key for who filed a message into its area, from the area chip's
 * provenance. A table with a DEFAULT: a value this client has never heard of
 * reads as "automatically" rather than as a missing word. The ONE table, used
 * by the reading pane's chip and the sender card alike, so the two cannot
 * describe the same filing differently. */
export function areaProvenanceKey(by: string | null): MessageKey {
  switch (by) {
    case "user":
      return "pane.area.by.user";
    case "rule":
      return "pane.area.by.rule";
    case "desk":
      return "pane.area.by.desk";
    case "learned":
      return "pane.area.by.learned";
    // A move made through an API key (#49): never "automatically", which would
    // credit Mailroom with an agent's decision.
    case "agent":
      return "pane.area.by.agent";
    default:
      return "pane.area.by.auto";
  }
}

/** The colour a chip is drawn in: the label's own, or one of the four palette
 * slots cycling by position (tokens.css --label-1..4). Colour is never the only
 * signal — the name is printed on every chip — so a cycle that repeats past
 * four labels costs nothing. */
export function labelColor(label: Label, index: number): string {
  return label.color ?? `var(--label-${(index % 4) + 1})`;
}
