// SPDX-License-Identifier: AGPL-3.0-only

// The paragraph indent as a number and as CSS (ADR-051 beta 2). Kept free of any
// editor import so the plain-text conversion (loaded with the composer, not the
// editor) can read the same attribute the editor writes.

import { declaredValue } from "./styleValues";

/** The deepest paragraph indent: levels 0 to 6, each `margin-left: 2em`. */
export const MAX_INDENT = 6;
const EM_PER_LEVEL = 2;
const PX_PER_LEVEL = 32;

/** Any value as a level: an integer 0 to 6, with anything unusable as 0. */
export function clampIndent(value: unknown): number {
  const n = typeof value === "number" ? value : typeof value === "string" && /^\d{1,4}$/.test(value) ? Number(value) : 0;
  if (!Number.isFinite(n)) return 0;
  return Math.min(MAX_INDENT, Math.max(0, Math.trunc(n)));
}

/** The level a style attribute's `margin-left` stands for. Only em and px are
 * understood; a negative, odd-unit, escaped or prioritised value is no indent. A
 * large positive one is the deepest level. */
export function parseIndent(style: string | null | undefined): number {
  const v = declaredValue(style, "margin-left");
  if (v === null) return 0;
  const m = /^\+?(\d{1,4}(?:\.\d{1,3})?)(em|px)$/i.exec(v);
  if (!m) return 0;
  const per = m[2]!.toLowerCase() === "em" ? EM_PER_LEVEL : PX_PER_LEVEL;
  return clampIndent(Math.round(Number(m[1]) / per));
}

/** The inline style for a level, or null for none. */
export function indentStyle(level: number): string | null {
  const n = clampIndent(level);
  return n > 0 ? `margin-left: ${n * EM_PER_LEVEL}em` : null;
}
