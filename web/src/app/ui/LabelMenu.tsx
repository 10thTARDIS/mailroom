// SPDX-License-Identifier: AGPL-3.0-only

// Apply/remove labels across the current selection (spec §12: bulk "archive /
// label / read / delete in one move").
//
// A MIRRORED label is a JMAP keyword on the Email (spec §13); `Label.keyword`
// is the server-assigned mirror atom. A LOCAL-ONLY label has none — every
// area's own label is one, deliberately (ADR-031) — and is applied by id over
// `labelIds/<id>` instead (#704). Both are selectable; the local one carries a
// hint that it stays in Mailroom, because it will not show up in another mail
// app and nobody should be surprised by that.
//
// KEYBOARD FIRST (#155). Hand-labelling is not a chore to be tolerated here: it
// is both the immediate workflow AND the corpus the learned classifier is
// induced from (#156). A labelling gesture that costs a mouse trip is one people
// stop making after a day, and then there is nothing to learn from. So the menu
// opens with the filter focused, 1–9 apply the first nine labels outright, and
// typing narrows the list — `l` then `1` labels a message without the hands
// leaving the keyboard.

import { useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import { effectiveEmail } from "../../core/store";
import type { Id, Label } from "../../core/jmap/types";
import { useApp, useMailState } from "../state";
import { t } from "../i18n";
import { moveToArea } from "./moveToArea";
import { offerRuleSuggestion } from "./ruleSuggestion";

export function LabelMenu({
  ids,
  onClose,
  onSuggestRule,
  onApplied,
  forceAdd = false,
}: {
  ids: Id[];
  onClose: () => void;
  /** Add-only, never toggle (#176). Set when the menu was opened to ANSWER a
   * question about a group of messages, where "these are Shopping" is the whole
   * gesture and un-labelling is not a meaning the button can have. */
  forceAdd?: boolean;
  /** Called after a label is APPLIED, with that label's id (#168). The shell
   * asks the server whether the labelling now implies a rule. Applying a label
   * is the moment of intent — the point where "and keep doing this" is a natural
   * next thought rather than an interruption. */
  onSuggestRule?: (labelId: Id) => void;
  /** Called whenever a click changes labels (added, removed or moved to an
   * area), so a bulk run can count the batch as done (#769). */
  onApplied?: () => void;
}): JSX.Element {
  const state = useMailState();
  const { actions, sync, store, pushToast, offerUndo } = useApp();
  const [name, setName] = useState("");
  const [filter, setFilter] = useState("");
  const filterRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    filterRef.current?.focus();
  }, []);

  const labels = useMemo(
    () => Object.values(state.labels).sort((a, b) => a.name.localeCompare(b.name)),
    [state.labels],
  );

  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return q ? labels.filter((l) => l.name.toLowerCase().includes(q)) : labels;
  }, [labels, filter]);

  /** A label is "on" when every selected message already carries it: by its
   * atom for a mirrored label (unchanged), by its id for a local-only one —
   * which has no atom, so `labelIds` is the only place it is visible (#704). */
  const isOn = (label: Label): boolean =>
    ids.length > 0 &&
    ids.every((id) => {
      const e = effectiveEmail(state, id);
      if (!e) return false;
      return label.keyword ? e.labelKeywords.includes(label.keyword) : e.labelIds[label.id] === true;
    });

  const toggle = (label: Label): void => {
    if (ids.length === 0) return;
    // `forceAdd` makes this add-only (#176). A batch answer means "these are
    // Shopping"; without it, a group whose messages ALREADY carry the chosen
    // label would read as "on" and the same gesture would strip the label off
    // all of them — the exact opposite of what the button says, silently.
    const adding = forceAdd ? true : !isOn(label);
    onApplied?.();
    // AN AREA'S OWN LABEL IS A MOVE, NOT AN ADD (#728). The earliest area wins, so
    // adding Updates' label to a message that still carries Promotions' left it in
    // Promotions. Adding it here does what Re-file and Move to area do: swap the
    // other areas' labels out, with one Undo. Removing it stays a plain removal.
    const owner = adding ? Object.values(state.areas).find((a) => a.labelId === label.id) : undefined;
    if (owner) {
      void (async () => {
        const deps = { store, sync, actions, offerUndo, pushToast };
        const outcome = await moveToArea(deps, ids, owner.id);
        if (outcome) {
          offerRuleSuggestion(deps, ids, owner.id, outcome.moved);
          onSuggestRule?.(label.id);
        }
      })();
      return;
    }
    void (async () => {
      // AWAITED before signalling, so a caller that goes on to ask the server
      // what this labelling implies is asking about a write that has at least
      // been enqueued. Rule/induce reads persisted labels; signalling first put
      // the question on the wire ahead of the answer it was about.
      await actions.applyLabel(ids, label, adding);
      // Only on ADD. Removing a label is not evidence of a category — it is
      // evidence against one — and proposing a rule off the back of it would
      // suggest automating the thing the user just undid.
      if (adding) onSuggestRule?.(label.id);
    })();
  };

  return (
    <div className="sheet sheet-small" role="dialog" aria-modal="true" aria-label={t("labels.title")}>
      <header className="sheet-header">
        <h2>{t("labels.title")}</h2>
        <button type="button" className="icon-button" aria-label={t("action.close")} onClick={onClose}>
          ✕
        </button>
      </header>

      {labels.length === 0 ? <p className="empty">{t("labels.none")}</p> : null}

      {labels.length > 0 ? (
        <>
          <input
            ref={filterRef}
            className="text-input"
            type="text"
            placeholder={t("labels.filter")}
            aria-label={t("labels.filter")}
            value={filter}
            onChange={(ev) => setFilter(ev.target.value)}
            onKeyDown={(ev) => {
              // Digits act on the FILTERED list, so "wo" then 1 is the first
              // Work-ish label rather than the first label overall — the number
              // always means what is on screen.
              if (/^[1-9]$/.test(ev.key)) {
                const target = shown[Number(ev.key) - 1];
                if (target) {
                  ev.preventDefault();
                  toggle(target);
                }
                return;
              }
              // Enter applies the only remaining match: the natural end of
              // typing a label's name.
              const only = shown.length === 1 ? shown[0] : undefined;
              if (ev.key === "Enter" && only) {
                ev.preventDefault();
                toggle(only);
              }
            }}
          />
          <p className="hint">{t("labels.applyHint")}</p>
        </>
      ) : null}

      <ul className="label-list">
        {shown.map((l, i) => {
          const on = isOn(l);
          return (
            <li key={l.id}>
              <label className="checkbox label-choice">
                <input
                  type="checkbox"
                  checked={on}
                  disabled={ids.length === 0}
                  onChange={() => toggle(l)}
                />
                <span
                  className="label-dot"
                  style={l.color ? { backgroundColor: l.color } : undefined}
                  aria-hidden="true"
                />
                {l.name}
                {i < 9 ? (
                  <span className="label-key" aria-hidden="true">
                    {i + 1}
                  </span>
                ) : null}
                {!l.keyword ? <span className="hint"> ({t("labels.localOnly")})</span> : null}
              </label>
            </li>
          );
        })}
      </ul>

      <form
        className="inline-form"
        onSubmit={(ev) => {
          ev.preventDefault();
          const trimmed = name.trim();
          if (!trimmed) return;
          void actions.createLabel(trimmed).then((created) => {
            // A label created here may come back local-only (no atom) when the
            // server's default says so; it is applied by id then, rather than
            // silently not at all (#704).
            if (created && ids.length > 0) void actions.applyLabel(ids, created, true);
          });
          setName("");
        }}
      >
        <input
          className="text-input"
          placeholder={t("labels.name")}
          aria-label={t("labels.name")}
          value={name}
          onChange={(ev) => setName(ev.target.value)}
        />
        <button type="submit" className="button">
          {t("labels.create")}
        </button>
      </form>
    </div>
  );
}
