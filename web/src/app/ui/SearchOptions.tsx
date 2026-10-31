// SPDX-License-Identifier: AGPL-3.0-only

// THE SEARCH-OPTIONS FORM (#724) — an editor for the query string, not a second
// way to search.
//
// It opens with the box's CURRENT query read into its fields (core/searchForm.ts
// queryToForm), and Search writes the fields back into a query string
// (formToQuery) which the box then runs exactly as if it had been typed. Anything
// the fields cannot express stays in "Has the words", so opening the form on a
// query and pressing Search never changes what was asked.
//
// It is IN THE PAGE, a plain <form> under the search box: no window.prompt, no
// confirm, no <dialog> — the iframe and artifact rules forbid those, and a modal
// would also hide the results the user is refining. Accessibility (#447):
//   * every control has a visible <label for> — the label IS the accessible
//     name, so what a screen reader says is what is on screen (WCAG 2.5.3);
//   * the form is named by its heading, and the button that opens it says
//     aria-expanded/aria-controls;
//   * opening moves focus to the first field; Escape or Cancel closes it and
//     puts focus back on the button that opened it (the caller does that);
//   * at phone width it is one column, the full width of the screen.

import { useEffect, useRef, useState } from "react";
import type { JSX } from "react";

import { ATTACHMENT_VALUES, WITHIN_VALUES, formToQuery, queryToForm } from "../../core/searchForm";
import type { Attachment, SearchFormFields, Within } from "../../core/searchForm";
import { t } from "../i18n";

interface Props {
  id: string;
  /** The query in the box when the form opened. Read once, on open. */
  query: string;
  /** Area names, in desk order, for the Area select. */
  areas: readonly string[];
  /** Run the query the form wrote. */
  onSubmit: (query: string) => void;
  /** Close without changing anything. */
  onCancel: () => void;
  /** Phone layout: positioned against the viewport rather than the box. */
  style?: React.CSSProperties;
}

type TextKey = "from" | "to" | "subject" | "hasWords" | "doesntHave" | "larger" | "smaller";

export function SearchOptions({ id, query, areas, onSubmit, onCancel, style }: Props): JSX.Element {
  // Read ONCE: the form edits its own copy; the box is only written on Search.
  const [f, setF] = useState<SearchFormFields>(() => queryToForm(query));
  const first = useRef<HTMLInputElement>(null);
  const titleId = `${id}-title`;

  useEffect(() => {
    first.current?.focus();
  }, []);

  // An area the query names but the desk does not have (renamed, or typed by
  // hand) is still offered — the select must be able to SHOW the value it was
  // given, or opening the form would quietly drop it.
  const areaOptions = f.area !== "" && !areas.includes(f.area) ? [...areas, f.area] : areas;

  const text = (key: TextKey, label: string, opts: { hint?: string; inputMode?: "text" | "email" } = {}) => {
    const fieldId = `${id}-${key}`;
    return (
      <div className="search-options-field">
        <label className="field-label" htmlFor={fieldId}>
          {label}
        </label>
        <input
          id={fieldId}
          ref={key === "from" ? first : undefined}
          className="text-input"
          type="text"
          autoComplete="off"
          spellCheck={false}
          {...(opts.inputMode ? { inputMode: opts.inputMode } : {})}
          {...(opts.hint ? { placeholder: opts.hint } : {})}
          value={f[key]}
          onChange={(e) => {
            const v = e.target.value;
            setF((cur) => ({ ...cur, [key]: v }));
          }}
        />
      </div>
    );
  };

  return (
    <form
      id={id}
      className="search-options"
      aria-labelledby={titleId}
      style={style}
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit(formToQuery(f));
      }}
      onKeyDown={(e) => {
        // Escape closes the form and stops here: the shell's Escape chain would
        // otherwise also act, and one key press should do one thing.
        if (e.key === "Escape") {
          e.preventDefault();
          e.stopPropagation();
          onCancel();
        }
      }}
    >
      <h2 className="search-options-title" id={titleId}>
        {t("search.options.title")}
      </h2>
      <div className="search-options-grid">
        {text("from", t("search.options.from"), { inputMode: "email" })}
        {text("to", t("search.options.to"), { inputMode: "email" })}
        {text("subject", t("search.options.subject"))}
        {text("hasWords", t("search.options.hasWords"))}
        {text("doesntHave", t("search.options.doesntHave"))}
        <div className="search-options-field">
          <label className="field-label" htmlFor={`${id}-within`}>
            {t("search.options.within")}
          </label>
          <select
            id={`${id}-within`}
            className="select"
            value={f.within}
            onChange={(e) => {
              const v = e.target.value as Within | "";
              setF((cur) => ({ ...cur, within: v }));
            }}
          >
            <option value="">{t("search.options.withinAny")}</option>
            {WITHIN_VALUES.map((w) => (
              <option key={w} value={w}>
                {t(`search.options.within.${w}` as Parameters<typeof t>[0])}
              </option>
            ))}
          </select>
        </div>
        {text("larger", t("search.options.larger"), { hint: t("search.options.sizeHint") })}
        {text("smaller", t("search.options.smaller"), { hint: t("search.options.sizeHint") })}
        <div className="search-options-field">
          <label className="field-label" htmlFor={`${id}-area`}>
            {t("search.options.area")}
          </label>
          <select
            id={`${id}-area`}
            className="select"
            value={f.area}
            onChange={(e) => {
              const v = e.target.value;
              setF((cur) => ({ ...cur, area: v }));
            }}
          >
            <option value="">{t("search.options.areaAny")}</option>
            {areaOptions.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </div>
        <div className="search-options-field">
          <label className="field-label" htmlFor={`${id}-attachment`}>
            {t("search.options.attachment")}
          </label>
          <select
            id={`${id}-attachment`}
            className="select"
            value={f.attachment}
            onChange={(e) => {
              const v = e.target.value as Attachment | "";
              setF((cur) => ({ ...cur, attachment: v }));
            }}
          >
            <option value="">{t("search.options.attachment.none")}</option>
            {ATTACHMENT_VALUES.map((a) => (
              <option key={a} value={a}>
                {t(`search.options.attachment.${a}` as Parameters<typeof t>[0])}
              </option>
            ))}
          </select>
        </div>
        <div className="search-options-field search-options-check">
          <input
            id={`${id}-anywhere`}
            type="checkbox"
            checked={f.includeTrashJunk}
            onChange={(e) => {
              const v = e.target.checked;
              setF((cur) => ({ ...cur, includeTrashJunk: v }));
            }}
          />
          <label htmlFor={`${id}-anywhere`}>{t("search.options.anywhere")}</label>
        </div>
      </div>
      <div className="search-options-actions">
        <button type="submit" className="button button-primary">
          {t("search.options.submit")}
        </button>
        <button type="button" className="button-quiet" onClick={onCancel}>
          {t("action.cancel")}
        </button>
      </div>
    </form>
  );
}
