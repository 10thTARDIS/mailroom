// SPDX-License-Identifier: AGPL-3.0-only

// PICKING A SAVED SIGNATURE OR TEMPLATE IN COMPOSE (#56, spec §5).
//
// Two pickers, one component, because a signature and a template differ only in
// WHERE the text goes: a signature is appended to what you are writing, a template
// replaces an empty body. The store, the object and the management screen are shared
// (see internal/store/compose_snippets.go for why that is one table).
//
// SIGNATURES ARE FILTERED BY THE IDENTITY YOU ARE SENDING AS, which is the whole
// point of per-identity signatures under spec §5's alias model: the sign-off for mail
// from shop@ is not the one for work@, and offering both would put the wrong name at
// the bottom of a message. A signature with no identity is offered for any From.
//
// A TEMPLATE IS ONLY OFFERED BEFORE YOU START WRITING, and refuses afterwards rather
// than asking. A template is the shape of a whole message, so dropping one into text
// someone has already composed produces a mess they cannot undo — compose has no undo —
// so the picker is disabled with the reason on it instead. "Before you start writing"
// means the body is still exactly what compose put there: empty, or nothing but an
// auto-inserted default signature and a reply quote.
//
// A DEFAULT SIGNATURE IS ALREADY IN THE BODY by the time this opens (Compose.tsx's
// openingBodies), so this picker exists for the other ones — a second sign-off, or the
// unbound one on a message from an address that has its own.

import { useMemo } from "react";

import type { ComposeSnippet, Id } from "../../core/jmap/types";
import { offeredSignatures } from "../../core/snippets";
import { t } from "../i18n";

export function SnippetPicker({
  kind,
  snippets,
  identityId,
  bodyUntouched,
  onPick,
  onClose,
}: {
  kind: "signature" | "template";
  snippets: ComposeSnippet[];
  /** The identity being sent as, for filtering signatures. */
  identityId: Id | null;
  /** Whether the body is still what compose opened with — a template will not land on
   * top of text the user wrote. */
  bodyUntouched: boolean;
  onPick: (s: ComposeSnippet) => void;
  onClose: () => void;
}) {
  // The signature filter and ordering live in core/snippets.ts, where they are tested
  // without a DOM — they are the rules that decide whose name goes at the bottom of a
  // message, not presentation. Templates have no identity, so they are simply listed.
  const offered = useMemo(
    () =>
      kind === "template"
        ? snippets.filter((s) => s.kind === "template").sort((a, b) => a.name.localeCompare(b.name))
        : offeredSignatures(snippets, identityId),
    [snippets, kind, identityId],
  );

  const blocked = kind === "template" && !bodyUntouched;

  return (
    <div
      className="sheet sheet-small"
      role="dialog"
      aria-modal="true"
      aria-label={t(kind === "signature" ? "snippets.pickSignature" : "snippets.pickTemplate")}
    >
      <header className="sheet-header">
        <h2>{t(kind === "signature" ? "snippets.pickSignature" : "snippets.pickTemplate")}</h2>
        <button type="button" className="icon-button" aria-label={t("action.close")} onClick={onClose}>
          ✕
        </button>
      </header>
      <div className="sheet-body">
        {blocked ? (
          <p className="field-hint">{t("snippets.templateNeedsEmptyBody")}</p>
        ) : offered.length === 0 ? (
          <p className="field-hint">
            {t(kind === "signature" ? "snippets.noSignatures" : "snippets.noTemplates")}
          </p>
        ) : (
          <ul className="label-list">
            {offered.map((s) => (
              <li key={s.id}>
                <button
                  type="button"
                  className="button button-quiet label-choice"
                  onClick={() => onPick(s)}
                >
                  {s.name}
                  {s.identityId === identityId && s.identityId !== null ? (
                    <span className="remote-allow-kind">{t("snippets.forThisAddress")}</span>
                  ) : null}
                  {s.isDefault ? (
                    <span className="remote-allow-kind">{t("snippets.default")}</span>
                  ) : null}
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
