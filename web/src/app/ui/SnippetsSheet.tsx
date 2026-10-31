// SPDX-License-Identifier: AGPL-3.0-only

// MANAGING SAVED SIGNATURES AND TEMPLATES (#56).
//
// The picker in compose (SnippetPicker.tsx) is the read side; this is where the text
// is actually written. Both lists live in one sheet because they are one object with
// one difference — a signature is appended to what you write, a template replaces an
// empty message — and splitting them into two screens would make the user learn twice.
//
// A SIGNATURE CAN BE BOUND TO AN IDENTITY, which is the point of it under spec §5's
// alias model: the sign-off for mail from shop@ is not the one for work@. The binding
// is the one field here with real consequence, so the picker echoes it back rather than
// leaving the user to remember which signature belongs where. A template has no
// identity — it is text you start from, not a way you sign — and the server rejects a
// bound one (compose_snippets_template_has_no_identity), so the control is not offered.
//
// IT IS PART OF A ROW BODY NOW, NOT A SHEET (#374 increment 2). The settings
// page's "Identities and signatures" row states the precedence of your identities
// and then holds this, because a signature is bound to an identity and the two
// questions are one. The dialog furniture (backdrop, ✕, its own Escape handler)
// is gone; the row heading opens it and Escape peels the settings route
// (app/routing.ts). Nothing about what it shows or writes changed.
//
// EDITING IS A FULL-OBJECT WRITE, deliberately. kind and identityId together decide
// which picker offers a snippet, so a partial update could silently move a signature to
// the wrong address — the #170 class of bug, where a narrow write dropped a field that
// something else depended on.

import { useMemo, useState } from "react";
import type { JSX } from "react";

import type { ComposeSnippet, Id } from "../../core/jmap/types";
import { useApp, useMailState } from "../state";
import { t } from "../i18n";

/** The snippet being edited, or a blank one being created. Kept as its own shape rather
 * than a partial ComposeSnippet so "new" and "editing" are the same code path. */
interface Draft {
  id?: Id;
  kind: "signature" | "template";
  name: string;
  bodyText: string;
  identityId: Id | null;
  isDefault: boolean;
}

function blankDraft(kind: "signature" | "template"): Draft {
  return { kind, name: "", bodyText: "", identityId: null, isDefault: false };
}

function toDraft(s: ComposeSnippet): Draft {
  return {
    id: s.id,
    kind: s.kind,
    name: s.name,
    // The editor is plain text. A snippet saved with HTML from elsewhere keeps its HTML
    // until edited here — see the save path, which only rewrites what it can represent.
    bodyText: s.bodyText,
    identityId: s.identityId,
    isDefault: s.isDefault,
  };
}

export function SnippetsBody(): JSX.Element {
  const { actions } = useApp();
  const { composeSnippets, identities } = useMailState();
  const [draft, setDraft] = useState<Draft | null>(null);
  const [problem, setProblem] = useState<string | null>(null);

  const all = useMemo(() => Object.values(composeSnippets), [composeSnippets]);
  const signatures = useMemo(
    () => all.filter((s) => s.kind === "signature").sort((a, b) => a.name.localeCompare(b.name)),
    [all],
  );
  const templates = useMemo(
    () => all.filter((s) => s.kind === "template").sort((a, b) => a.name.localeCompare(b.name)),
    [all],
  );

  // Only CONCRETE identities can hold a signature. A catchall wildcard (`*@example.com`)
  // is not an address you send as, so binding to it would mean "any local part at this
  // domain" — a scope the server has no column for and the picker could not honour.
  const bindable = useMemo(
    () => Object.values(identities).filter((i) => !i.email.startsWith("*@")),
    [identities],
  );

  const identityLabel = (id: Id | null): string =>
    id === null ? t("snippets.anyIdentity") : (identities[id]?.email ?? id);

  async function save() {
    if (!draft) return;
    if (draft.name.trim() === "") {
      setProblem(t("snippets.nameRequired"));
      return;
    }
    if (draft.bodyText.trim() === "") {
      setProblem(t("snippets.bodyRequired"));
      return;
    }
    setProblem(null);
    const ok = await actions.saveComposeSnippet({
      ...(draft.id ? { id: draft.id } : {}),
      kind: draft.kind,
      name: draft.name.trim(),
      // A plain-text editor writes the text part and CLEARS the HTML one. Leaving stale
      // HTML behind would mean the rich-mode insert silently used the old text while
      // plain mode used the new — the same body diverging by editor mode.
      bodyHtml: "",
      bodyText: draft.bodyText,
      identityId: draft.kind === "signature" ? draft.identityId : null,
      isDefault: draft.isDefault,
    });
    if (ok) setDraft(null);
  }

  function renderList(list: ComposeSnippet[], kind: "signature" | "template") {
    return (
      <>
        <h3 className="sheet-section">
          {t(kind === "signature" ? "snippets.signatures" : "snippets.templates")}
        </h3>
        {list.length === 0 ? (
          <p className="field-hint">{t("snippets.none")}</p>
        ) : (
          <ul className="remote-allow-list">
            {list.map((s) => (
              <li key={s.id} className="remote-allow">
                <span className="remote-allow-value">{s.name}</span>
                {s.kind === "signature" && s.identityId !== null ? (
                  <span className="remote-allow-kind">{identityLabel(s.identityId)}</span>
                ) : null}
                {s.isDefault ? (
                  <span className="remote-allow-kind">{t("snippets.default")}</span>
                ) : null}
                <button type="button" className="button-quiet" onClick={() => setDraft(toDraft(s))}>
                  {t("action.edit")}
                </button>
                <button
                  type="button"
                  className="button-quiet"
                  onClick={() => {
                    // Confirmed because there is no undo and no trash for a snippet —
                    // the text is gone and has to be retyped.
                    if (window.confirm(t("snippets.deleteConfirm", { name: s.name }))) {
                      void actions.deleteComposeSnippet(s.id);
                    }
                  }}
                >
                  {t("action.delete")}
                </button>
              </li>
            ))}
          </ul>
        )}
        <button
          type="button"
          className="button button-quiet"
          onClick={() => {
            setProblem(null);
            setDraft(blankDraft(kind));
          }}
        >
          {t("snippets.add")}
        </button>
      </>
    );
  }

  return (
    <>
      {draft ? (
        <div className="snippet-editor">
          <label className="field">
            <span className="field-label">{t("snippets.name")}</span>
            <input
              type="text"
              className="text-input"
              value={draft.name}
              autoFocus
              onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            />
          </label>

          <label className="field">
            <span className="field-label">{t("snippets.body")}</span>
            <textarea
              className="textarea"
              rows={8}
              value={draft.bodyText}
              onChange={(e) => setDraft({ ...draft, bodyText: e.target.value })}
            />
          </label>

          {draft.kind === "signature" && bindable.length > 0 ? (
            <label className="field">
              <span className="field-label">{t("snippets.identity")}</span>
              <select
                className="select"
                value={draft.identityId ?? ""}
                onChange={(e) =>
                  setDraft({ ...draft, identityId: e.target.value === "" ? null : e.target.value })
                }
              >
                <option value="">{t("snippets.anyIdentity")}</option>
                {bindable.map((i) => (
                  <option key={i.id} value={i.id}>
                    {i.email}
                  </option>
                ))}
              </select>
            </label>
          ) : null}

          <label className="field field-inline">
            <input
              type="checkbox"
              checked={draft.isDefault}
              onChange={(e) => setDraft({ ...draft, isDefault: e.target.checked })}
            />
            <span className="field-label">{t("snippets.isDefault")}</span>
          </label>

          {problem ? <p className="field-error">{problem}</p> : null}

          <div className="sheet-actions">
            <button type="button" className="button button-primary" onClick={() => void save()}>
              {t("action.save")}
            </button>
            <button type="button" className="button button-quiet" onClick={() => setDraft(null)}>
              {t("action.cancel")}
            </button>
          </div>
        </div>
      ) : (
        <>
          <p className="field-hint">{t("snippets.explain")}</p>
          {renderList(signatures, "signature")}
          {renderList(templates, "template")}
        </>
      )}
    </>
  );
}
