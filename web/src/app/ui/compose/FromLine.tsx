// SPDX-License-Identifier: AGPL-3.0-only

// THE FROM LINE (#448): a sentence, and a **Change** button.
//
// It replaced a <select> of every identity beside an editable address box. The
// arithmetic — which address, which reason word, in what order the alternatives
// come — is in ./from.ts, tested; this file is the markup and the one behaviour
// that is not pure: a catch-all keeps the "type the local part" box, because for a
// wildcard identity the address IS the thing being typed.

import { useEffect, useId, useRef, useState } from "react";
import type { JSX } from "react";

import type { Identity } from "../../../core/jmap/types";
import { t } from "../../i18n";
import {
  FROM_REASON_KEY,
  FROM_SENTENCE_KEY,
  fromSentence,
  identityChoices,
  wildcardDomain,
} from "./from";
import type { ParentFacts } from "./from";

export function FromLine({
  identity,
  identities,
  parent,
  localPart,
  onLocalPart,
  onPick,
}: {
  identity: Identity | undefined;
  identities: readonly Identity[];
  parent: ParentFacts | null;
  /** What the user has typed as the address for a CATCH-ALL identity; ignored for
   * a concrete one, which has an address of its own. */
  localPart: string;
  onLocalPart: (v: string) => void;
  onPick: (id: string) => void;
}): JSX.Element {
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState("");
  const listId = useId();
  const filterRef = useRef<HTMLInputElement>(null);

  const sentence = fromSentence(identity, parent, identities, localPart);
  const rows = identityChoices(identities, parent, filter);

  // The filter takes focus when the list opens: the list is "typed and filtered"
  // (the design's words), so it is a search box that happens to have rows under
  // it rather than a list with a search box above it.
  useEffect(() => {
    if (open) filterRef.current?.focus();
  }, [open]);

  const choose = (id: string): void => {
    onPick(id);
    setOpen(false);
    setFilter("");
  };

  return (
    <div className="compose-from">
      <p className="compose-from-line">
        <span className="field-label">{t("compose.from")}</span>
        {sentence ? (
          <>
            <span className="compose-from-address">{sentence.address}</span>
            <span className="compose-from-sep" aria-hidden="true">
              ·
            </span>
            <span className="compose-from-reason">{t(FROM_SENTENCE_KEY[sentence.reason])}</span>
          </>
        ) : (
          // No identity at all. It is a real state — an account whose identities
          // have not loaded — and saying so is better than an empty line that
          // looks like a rendering bug.
          <span className="compose-from-reason">{t("compose.from.none")}</span>
        )}
        <button
          type="button"
          className="link-button compose-from-change"
          aria-expanded={open}
          aria-controls={listId}
          onClick={() => setOpen((v) => !v)}
        >
          {t("compose.from.change")}
        </button>
      </p>

      {/* A CATCH-ALL KEEPS ITS ADDRESS BOX. `*@example.com` means "any local part
          at this domain", so there is no address to state — the user invents one,
          and the sentence above shows what they have invented so far. */}
      {identity?.isWildcard ? (
        <label className="field compose-from-local">
          <span className="field-label">{t("compose.from.localPart")}</span>
          <input
            className="text-input"
            value={localPart}
            placeholder={`name@${wildcardDomain(identity)}`}
            aria-label={t("compose.from.localPart")}
            onChange={(ev) => onLocalPart(ev.target.value)}
          />
        </label>
      ) : null}

      {open ? (
        <div
          className="compose-from-picker"
          id={listId}
          // ESCAPE CLOSES THE PICKER, NOT THE COMPOSER. The shell's Escape chain's
          // first layer is "drop ?compose= from the route", so without stopping the
          // event here an Escape aimed at this list would close the whole composer —
          // the same reason the send-later and template dialogs have a capture-phase
          // guard of their own (ui/Compose.tsx).
          onKeyDown={(ev) => {
            if (ev.key !== "Escape") return;
            ev.stopPropagation();
            setOpen(false);
            setFilter("");
          }}
        >
          <input
            ref={filterRef}
            className="text-input"
            value={filter}
            aria-label={t("compose.from.filter")}
            placeholder={t("compose.from.filter")}
            onChange={(ev) => setFilter(ev.target.value)}
            onKeyDown={(ev) => {
              if (ev.key !== "Enter") return;
              ev.preventDefault();
              const first = rows.find((r) => r.enabled);
              if (first) choose(first.id);
            }}
          />
          {rows.length === 0 ? (
            <p className="hint">{t("compose.from.noMatch")}</p>
          ) : (
            <ul className="compose-from-list" role="listbox" aria-label={t("compose.from.pick")}>
              {rows.map((row) => (
                <li key={row.id}>
                  <button
                    type="button"
                    role="option"
                    aria-selected={row.id === identity?.id}
                    // A DISABLED IDENTITY IS LISTED AND NOT SELECTABLE. It is
                    // listed because "why can I not send from that address?" is
                    // answered by seeing it there with `disabled` on it; it is
                    // not selectable because the server refuses the send
                    // (spec §5 — the From is a security boundary), and offering
                    // it would be offering a choice that fails.
                    disabled={!row.enabled}
                    className={`compose-from-option${row.enabled ? "" : " is-disabled"}`}
                    onClick={() => choose(row.id)}
                  >
                    <span className="compose-from-option-address">{row.address}</span>
                    {row.name.trim() !== "" ? (
                      <span className="compose-from-option-name">{row.name}</span>
                    ) : null}
                    <span className="compose-from-option-reason">
                      {t(FROM_REASON_KEY[row.reason])}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>
      ) : null}
    </div>
  );
}
