// SPDX-License-Identifier: AGPL-3.0-only

// THE SEARCH BOX, WITH SUGGESTIONS AND RECENT SEARCHES (#57, matrix §7), LIVE
// RESULTS AND A SEARCH-OPTIONS FORM (#724).
//
// Mailroom's search has understood a real operator syntax since v1.0 — `from:`, `label:`,
// `is:unread`, `before:`, phrase and prefix matching. All of it was reachable only by
// knowing it existed: the entire discovery surface was a `title=` tooltip. A syntax nobody
// can see is a syntax nobody uses, so the suggestions ARE the documentation. Type `l` and
// `label:` appears with what it means; type `label:` and your labels are listed.
//
// The ranking and completion rules live in core/searchSuggest.ts, tested without a DOM.
// This file is the keyboard and the ARIA, which is the part that has to be right for the
// feature to be usable at all:
//
//   * A real ARIA 1.2 combobox — the input keeps focus and owns aria-activedescendant, so
//     a screen reader announces the highlighted option while the caret stays where the
//     user is typing. Moving focus into the list instead is the common mistake and it
//     breaks typing.
//   * ArrowDown/Up move the highlight, Enter takes the highlighted option or submits what
//     was typed, Tab COMPLETES without submitting (so operators can be chained), Escape
//     closes the list and leaves the text alone.
//   * NOTHING IS EVER CHOSEN BY DEFAULT. The highlight starts at -1, so Enter submits
//     exactly what the user typed unless they moved to an option deliberately. A list that
//     pre-selects its first row turns a search into a different search on Enter.
//   * The suggestions are for the token the CARET is in (#724), and an address operator's
//     values include the contacts directory (Contact/query), not only loaded senders.
//
// LIVE RESULTS (#724). After SEARCH_DEBOUNCE_MS without a keystroke the box asks the shell
// to show the results for what is typed (`onLive`), with the final word as a prefix. The
// list keeps the previous results until the new ones land and a stale answer can never
// replace a newer one — both enforced in core/sync.ts loadView, where the requests are.
//
// THE OPTIONS FORM (#724) writes the query string; see SearchOptions.tsx.
//
// Recent searches are LOCAL TO THIS BROWSER and clearable from here — see
// searchHistory.ts for why that is a deliberate privacy decision and not a shortcut.

import { useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import { areasInDeskOrder } from "../../core/refile";
import { liveQueryOf, tokenAtCaret } from "../../core/searchQuery";
import { ADDRESS_OPERATORS, sendersOf, suggestSearch } from "../../core/searchSuggest";
import type { Suggestion } from "../../core/searchSuggest";
import { clearSearchHistory, loadSearchHistory } from "../searchHistory";
import { useApp, useMailState } from "../state";
import { t } from "../i18n";
import { SearchOptions } from "./SearchOptions";

/**
 * How long the box waits after the last keystroke before showing results for it (#724).
 *
 * ONE NAMED CONSTANT, because it is the knob the operator turns: too short and every
 * keystroke is a search the server answers and the client throws away; too long and the
 * results feel like they wait for you. The server times every index read in
 * `mailroom_search_duration_seconds` (deploy/README.md) — read its p95 before moving this.
 */
export const SEARCH_DEBOUNCE_MS = 250;

/** How long the contacts lookup waits after a keystroke — RecipientChips' figure. It is
 * shorter than SEARCH_DEBOUNCE_MS on purpose: a suggestion list should keep up with
 * typing, and Contact/query is a cheap indexed read. */
const CONTACT_DEBOUNCE_MS = 160;

/** A default name for a saved search, derived from the query itself.
 *
 * Just the query, trimmed to something a sidebar row can show. Deliberately not clever:
 * a generated name that guesses wrong is worse than an obvious one the user edits, and
 * the field is focused and selected so replacing it costs one keystroke. */
function suggestSearchName(query: string): string {
  const q = query.trim().replace(/\s+/g, " ");
  return q.length <= 28 ? q : q.slice(0, 27) + "…";
}

/** The address operator and its partial value when the caret is in one, else null. */
function addressPartial(value: string, caret: number): string | null {
  const { text } = tokenAtCaret(value, caret);
  const colon = text.indexOf(":");
  if (colon < 0 || !ADDRESS_OPERATORS.has(text.slice(0, colon + 1).toLowerCase())) return null;
  return text.slice(colon + 1).replace(/"/g, "");
}

interface Props {
  value: string;
  onChange: (v: string) => void;
  /** Run the search. The caller decides what a search does to the view. */
  onSubmit: (q: string) => void;
  /** Show results for what is being typed (#724) — called after SEARCH_DEBOUNCE_MS
   * without a keystroke, with the query and whether its final word is a prefix. */
  onLive?: (q: string, prefix: boolean) => void;
  /** Clear the box and leave search. */
  onClear: () => void;
  inputRef?: React.RefObject<HTMLInputElement | null>;
}

export function SearchBox({ value, onChange, onSubmit, onLive, onClear, inputRef }: Props): JSX.Element {
  const state = useMailState();
  const { actions, client, pushToast } = useApp();
  const ownRef = useRef<HTMLInputElement>(null);
  const ref = inputRef ?? ownRef;
  const optionsButton = useRef<HTMLButtonElement>(null);
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const [caret, setCaret] = useState(value.length);
  const [history, setHistory] = useState<string[]>(() => loadSearchHistory());
  const [directory, setDirectory] = useState<Array<{ email: string; name?: string | null }>>([]);
  const [optionsOpen, setOptionsOpen] = useState(false);
  const [optionsStyle, setOptionsStyle] = useState<React.CSSProperties | undefined>(undefined);
  const listId = "search-suggestions";
  const optionsId = "search-options";
  // Saving the current query as a smart folder (#57).
  const [saving, setSaving] = useState(false);
  const [saveName, setSaveName] = useState("");
  const [pinned, setPinned] = useState(true);

  const save = async (): Promise<void> => {
    const name = saveName.trim();
    if (name === "") return;
    // The query saved is the one that is RUNNING, not whatever is in the box: those
    // differ the moment someone edits the text after searching, and saving the edited
    // version would store a search whose results nobody has seen.
    const ok = await actions.saveSavedSearch({
      name,
      query: value.trim(),
      inSidebar: pinned,
      position: 0,
    });
    if (ok) {
      setSaving(false);
      pushToast({ kind: "info", message: t("search.saved", { name }) });
    }
  };

  // The senders of the messages currently loaded. Derived from the list the app already
  // holds, so no request is made per keystroke — see searchSuggest.ts on the trade.
  const contacts = useMemo(
    () =>
      sendersOf(
        state.list.ids
          .map((id) => state.emails[id])
          .filter((e): e is NonNullable<typeof e> => e !== undefined),
      ),
    [state.list.ids, state.emails],
  );
  const areaNames = useMemo(() => areasInDeskOrder(state.areas).map((a) => a.name), [state.areas]);

  // THE CONTACTS DIRECTORY for an address value being typed (#724): one debounced
  // Contact/query per pause, latest answer wins, any failure is "no extra rows" —
  // a suggestion list is a convenience and must never block typing.
  const partial = addressPartial(value, caret);
  useEffect(() => {
    if (partial === null || partial === "") {
      setDirectory([]);
      return;
    }
    let current = true;
    const timer = window.setTimeout(() => {
      client
        .contactAutocomplete(partial)
        .then((cs) => {
          if (!current) return;
          setDirectory(
            cs.flatMap((c) => c.addresses.map((a) => ({ email: a.address, name: c.displayName || null }))),
          );
        })
        .catch(() => {
          if (current) setDirectory([]);
        });
    }, CONTACT_DEBOUNCE_MS);
    return () => {
      current = false;
      window.clearTimeout(timer);
    };
  }, [partial, client]);

  const suggestions = useMemo(
    () =>
      suggestSearch(
        value,
        {
          labels: Object.values(state.labels),
          contacts,
          directory,
          areas: areaNames.map((name) => ({ name })),
          accounts: [],
          history,
        },
        caret,
      ),
    [value, caret, state.labels, contacts, directory, areaNames, history],
  );

  // The highlight is reset whenever the list changes underneath it. Without this, typing
  // one more character can leave the highlight on a row that is now something else, and
  // Enter picks the thing the user did not look at.
  useEffect(() => setActive(-1), [value, caret]);

  // LIVE RESULTS (#724): a pending live search, cancelled by the next keystroke, by
  // Enter (which runs the search itself), by clearing, and by unmounting.
  const liveTimer = useRef<number | undefined>(undefined);
  const cancelLive = (): void => {
    if (liveTimer.current !== undefined) window.clearTimeout(liveTimer.current);
    liveTimer.current = undefined;
  };
  useEffect(() => cancelLive, []);
  const scheduleLive = (text: string): void => {
    cancelLive();
    if (!onLive) return;
    liveTimer.current = window.setTimeout(() => {
      liveTimer.current = undefined;
      const live = liveQueryOf(text);
      if (live) onLive(live.query, live.prefix);
    }, SEARCH_DEBOUNCE_MS);
  };

  const visible = open && !optionsOpen && suggestions.length > 0;

  const run = (q: string): void => {
    const trimmed = q.trim();
    cancelLive();
    if (trimmed === "") return;
    setOpen(false);
    setActive(-1);
    // The caller records the history — it is the one path every search goes through,
    // including the command palette's. This component only READS it, and re-reads on
    // focus, which is why a search made elsewhere shows up here too.
    onSubmit(trimmed);
  };

  const choose = (s: Suggestion, submit: boolean): void => {
    onChange(s.apply);
    setCaret(s.caret);
    if (submit) {
      run(s.apply);
      return;
    }
    // Completing without submitting: keep the list open so the next operator can be
    // chained straight away, and put the caret just after what was inserted.
    setActive(-1);
    scheduleLive(s.apply);
    requestAnimationFrame(() => ref.current?.setSelectionRange(s.caret, s.caret));
  };

  const onKeyDown = (ev: React.KeyboardEvent<HTMLInputElement>): void => {
    if (ev.key === "ArrowDown" && visible) {
      ev.preventDefault();
      setActive((i) => (i + 1) % suggestions.length);
      return;
    }
    if (ev.key === "ArrowUp" && visible) {
      ev.preventDefault();
      setActive((i) => (i <= 0 ? suggestions.length - 1 : i - 1));
      return;
    }
    if (ev.key === "Tab" && visible && active >= 0) {
      // Tab COMPLETES rather than submitting or moving focus: the point of chaining
      // operators is to keep typing. Only when something is highlighted, so Tab still
      // leaves the search box normally the rest of the time.
      ev.preventDefault();
      const s = suggestions[active];
      if (s) choose(s, false);
      return;
    }
    if (ev.key === "Escape") {
      if (visible) {
        // Escape closes the list WITHOUT changing the text, and stops here — the
        // shell's Escape chain would otherwise also act, and one key press should do
        // one thing.
        ev.preventDefault();
        ev.stopPropagation();
        setOpen(false);
        setActive(-1);
      }
      return;
    }
    if (ev.key === "Enter") {
      ev.preventDefault();
      const s = active >= 0 ? suggestions[active] : undefined;
      if (s) choose(s, true);
      else run(value);
    }
  };

  const openOptions = (): void => {
    setOpen(false);
    setSaving(false);
    cancelLive();
    // At phone width the box is too narrow to hang a form from, so the form spans the
    // screen just under it instead (styles.css .search-options); the top is the box's
    // own bottom edge, which only the DOM knows.
    const box = ref.current?.closest(".search");
    const narrow = typeof window.matchMedia === "function" && window.matchMedia("(max-width: 759px)").matches;
    setOptionsStyle(narrow && box ? { top: `${Math.round(box.getBoundingClientRect().bottom + 4)}px` } : undefined);
    setOptionsOpen(true);
  };
  const closeOptions = (): void => {
    setOptionsOpen(false);
    // Focus goes back to the control that opened the form, so a keyboard user is
    // where they were rather than at the top of the page.
    requestAnimationFrame(() => optionsButton.current?.focus());
  };

  return (
    <div className="search" role="search">
      <input
        ref={ref}
        type="search"
        className="search-input"
        value={value}
        aria-label={t("search.label")}
        placeholder={t("search.placeholder")}
        title={t("search.help")}
        role="combobox"
        aria-expanded={visible}
        aria-controls={listId}
        aria-autocomplete="list"
        {...(visible && active >= 0 ? { "aria-activedescendant": `${listId}-${active}` } : {})}
        onChange={(ev) => {
          onChange(ev.target.value);
          setCaret(ev.target.selectionStart ?? ev.target.value.length);
          setOpen(true);
          scheduleLive(ev.target.value);
        }}
        // The caret moves without the text changing (arrow keys, a click): the
        // suggestions follow it to the token it is now in.
        onSelect={(ev) => setCaret(ev.currentTarget.selectionStart ?? ev.currentTarget.value.length)}
        onFocus={() => {
          // Re-read on focus: the history is written by whoever ran the search (see
          // run()), so a search made from the command palette must appear here without
          // this component having been involved.
          setHistory(loadSearchHistory());
          setOpen(true);
        }}
        // A blur that lands on an option must not close the list before the click
        // registers, so the close is deferred one tick. mousedown-preventDefault on the
        // options would also work; this keeps the option a real focusable button.
        onBlur={() => window.setTimeout(() => setOpen(false), 120)}
        onKeyDown={onKeyDown}
      />
      <button
        ref={optionsButton}
        type="button"
        className="icon-button search-aux"
        aria-label={t("search.options")}
        title={t("search.options")}
        aria-expanded={optionsOpen}
        aria-controls={optionsId}
        onClick={() => (optionsOpen ? closeOptions() : openOptions())}
      >
        ▾
      </button>
      {/* SAVE THIS SEARCH (#57), offered where the search is — a query you retype every
          day is a folder you do not have, and the moment to notice that is while you are
          looking at its results. Only for a query that is actually running: offering it
          for half-typed text would save something the user has not seen the results of. */}
      {value.trim() !== "" && !saving ? (
        <button
          type="button"
          className="icon-button search-aux"
          aria-label={t("search.save")}
          title={t("search.save")}
          onClick={() => {
            setOptionsOpen(false);
            setSaveName(suggestSearchName(value));
            setSaving(true);
          }}
        >
          ☆
        </button>
      ) : null}
      {value ? (
        <button
          type="button"
          className="icon-button"
          aria-label={t("search.clear")}
          onClick={() => {
            cancelLive();
            onChange("");
            setOpen(false);
            onClear();
          }}
        >
          ✕
        </button>
      ) : null}

      {optionsOpen ? (
        <SearchOptions
          id={optionsId}
          query={value}
          areas={areaNames}
          {...(optionsStyle ? { style: optionsStyle } : {})}
          onCancel={closeOptions}
          onSubmit={(q) => {
            setOptionsOpen(false);
            onChange(q);
            setCaret(q.length);
            // The form's query runs like a typed one: the same path, the same history.
            run(q);
            requestAnimationFrame(() => ref.current?.focus());
          }}
        />
      ) : null}

      {saving ? (
        <form
          className="search-save"
          onSubmit={(ev) => {
            ev.preventDefault();
            void save();
          }}
        >
          <label className="field-label" htmlFor="search-save-name">
            {t("search.saveAs")}
          </label>
          <input
            id="search-save-name"
            className="text-input"
            value={saveName}
            autoFocus
            onChange={(e) => setSaveName(e.target.value)}
            // Escape closes the naming box without saving, and stops there — the
            // shell's Escape chain would otherwise also act.
            onKeyDown={(e) => {
              if (e.key === "Escape") {
                e.preventDefault();
                e.stopPropagation();
                setSaving(false);
              }
            }}
          />
          <label className="field-inline">
            <input
              type="checkbox"
              checked={pinned}
              onChange={(e) => setPinned(e.target.checked)}
            />
            <span>{t("search.pinToSidebar")}</span>
          </label>
          <button type="submit" className="button button-primary" disabled={saveName.trim() === ""}>
            {t("action.save")}
          </button>
          <button type="button" className="button-quiet" onClick={() => setSaving(false)}>
            {t("action.cancel")}
          </button>
        </form>
      ) : null}

      {visible ? (
        <ul className="search-suggestions" id={listId} role="listbox" aria-label={t("search.suggestions")}>
          {suggestions.map((s, i) => (
            <li key={`${s.kind}:${s.apply}`} role="none">
              <button
                type="button"
                id={`${listId}-${i}`}
                role="option"
                aria-selected={i === active}
                className={`search-option${i === active ? " is-active" : ""}`}
                // The highlight follows the pointer so mouse and keyboard agree about
                // which row is current, rather than showing two different "selected" rows.
                onMouseEnter={() => setActive(i)}
                onClick={() => choose(s, true)}
              >
                <span className="search-option-label">{s.label}</span>
                {s.detail ? <span className="search-option-detail">{s.detail}</span> : null}
                {s.kind === "history" ? (
                  <span className="search-option-kind">{t("search.recent")}</span>
                ) : null}
              </button>
            </li>
          ))}
          {/* Offered only when history is what is being shown: a clear-history control
              beside operator completions would be a control for something not on screen. */}
          {suggestions[0]?.kind === "history" ? (
            <li role="none" className="search-suggestions-foot">
              <button
                type="button"
                className="button-quiet"
                onClick={() => {
                  setHistory(clearSearchHistory());
                  setOpen(false);
                }}
              >
                {t("search.clearHistory")}
              </button>
            </li>
          ) : null}
        </ul>
      ) : null}
    </div>
  );
}
