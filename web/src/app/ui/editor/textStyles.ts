// SPDX-License-Identifier: AGPL-3.0-only

// Colour, highlight, font family and font size (ADR-051 beta 2), as attributes of
// Tiptap's `textStyle` mark. The library's own extensions copy whatever CSS value
// the markup carries into the mark and write it back out, so a pasted
// `background:url(...)` or `color:var(--x)` would travel. These are the same
// extensions with the value gate moved in front, in THREE places so no single
// path is trusted:
//
//   * parsing (paste, seed, insertContent, setContent) reads the written
//     declaration and keeps it only if ./styleValues.ts accepts it;
//   * the commands refuse a value that is not acceptable, so the toolbar (or any
//     other caller) cannot set one either;
//   * rendering re-checks, so a mark built some other way (setMark with raw
//     attributes, JSON content) still writes nothing off-list.
//
// A `<span style>` none of whose values passes is not a mark at all: the text
// stays, unstyled.

import type { Editor } from "@tiptap/core";
import { BackgroundColor, Color, FontFamily, FontSize, TextStyle } from "@tiptap/extension-text-style";
import { normaliseColour, normaliseFontFamily, normaliseFontSize, spanStyle } from "./styleValues";

type Normaliser = (value: unknown) => string | null;

interface Spec {
  /** The CSS property written, and read back. */
  prop: string;
  attr: string;
  normalise: Normaliser;
}

const SPECS: Spec[] = [
  { prop: "color", attr: "color", normalise: normaliseColour },
  { prop: "background-color", attr: "backgroundColor", normalise: normaliseColour },
  { prop: "font-family", attr: "fontFamily", normalise: normaliseFontFamily },
  { prop: "font-size", attr: "fontSize", normalise: normaliseFontSize },
];

/** The attribute definition for a global attribute on `textStyle`. */
function attribute(spec: Spec) {
  return {
    default: null,
    parseHTML: (el: HTMLElement) => {
      const raw = spanStyle(el, spec.prop);
      return raw === null ? null : spec.normalise(raw);
    },
    renderHTML: (attrs: Record<string, unknown>) => {
      const v = spec.normalise(attrs[spec.attr]);
      return v === null ? {} : { style: `${spec.prop}: ${v}` };
    },
  };
}

/** `setX(value)`: false (a no-op) for a value the gate refuses; `unsetX()`. */
function commandsFor(spec: Spec, set: string, unset: string) {
  return {
    [set]:
      (value: unknown) =>
      ({ chain }: { chain: () => ReturnType<Editor["chain"]> }) => {
        const v = spec.normalise(value);
        if (v === null) return false;
        return chain().setMark("textStyle", { [spec.attr]: v }).run();
      },
    [unset]:
      () =>
      ({ chain }: { chain: () => ReturnType<Editor["chain"]> }) =>
        chain().setMark("textStyle", { [spec.attr]: null }).removeEmptyTextStyle().run(),
  };
}

function safe<T extends typeof Color>(base: T, spec: Spec, set: string, unset: string): T {
  return base.extend({
    addGlobalAttributes() {
      return [{ types: this.options.types, attributes: { [spec.attr]: attribute(spec) } }];
    },
    addCommands() {
      return commandsFor(spec, set, unset) as never;
    },
  }) as T;
}

export const SafeColor = safe(Color, SPECS[0]!, "setColor", "unsetColor");
export const SafeBackgroundColor = safe(BackgroundColor as typeof Color, SPECS[1]!, "setBackgroundColor", "unsetBackgroundColor");
export const SafeFontFamily = safe(FontFamily as typeof Color, SPECS[2]!, "setFontFamily", "unsetFontFamily");
export const SafeFontSize = safe(FontSize as typeof Color, SPECS[3]!, "setFontSize", "unsetFontSize");

/** The `textStyle` mark itself: a `<span style>` is a mark only when at least one
 * of its values is one the gate accepts. (The library's rule accepts every span
 * with a style attribute, and would leave an empty `<span>` behind.) Nested spans
 * are merged by the attribute readers above, which look up the span chain, so the
 * library's merge step (which rewrites the pasted DOM's style text) is off. */
export const SafeTextStyle = TextStyle.extend({
  addOptions() {
    return { HTMLAttributes: {}, mergeNestedSpanStyles: false };
  },
  parseHTML() {
    return [
      {
        tag: "span",
        consuming: false,
        getAttrs: (el: HTMLElement | string) => {
          if (typeof el === "string") return false;
          return SPECS.some((s) => {
            const raw = spanStyle(el, s.prop);
            return raw !== null && s.normalise(raw) !== null;
          })
            ? {}
            : false;
        },
      },
    ];
  },
});

export const TEXT_STYLE_EXTENSIONS = [SafeTextStyle, SafeColor, SafeBackgroundColor, SafeFontFamily, SafeFontSize];
