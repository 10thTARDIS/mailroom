// SPDX-License-Identifier: AGPL-3.0-only

// MANAGING SAVED SEARCHES / SMART FOLDERS (#57).
//
// The search box creates them and the sidebar runs them; this is where they are edited,
// renamed, unpinned and deleted. Without it a saved search would be creatable and
// un-removable, which is the same trap the remembered remote-content grants (#202) were
// built to avoid: a thing the user set up and cannot see the whole of, or take back.
//
// THE QUERY IS EDITABLE HERE, as text. That is the point of storing it as the operator
// string rather than a parsed tree — what you saved is what you read back, and there is no
// renderer that could disagree with a parser about what it means. An invalid query fails
// when it RUNS, exactly as typing the same thing does; this sheet does not pretend to
// validate it, because a second opinion about what a query means is the one thing the
// design avoids.
//
// IT IS A ROW BODY NOW, NOT A SHEET (#374 increment 2) — the whole of the "Saved
// searches" row of the settings page. The dialog furniture (backdrop, ✕, its own
// Escape handler) is gone: the row heading opens it and Escape peels the settings
// route (app/routing.ts). Nothing about what it shows or writes changed.

import { useMemo, useState } from "react";
import type { JSX } from "react";

import type { Id, SavedSearch } from "../../core/jmap/types";
import { useApp, useMailState } from "../state";
import { t } from "../i18n";

interface Draft {
  id?: Id;
  name: string;
  query: string;
  inSidebar: boolean;
  position: number;
}

export function SavedSearchesBody(): JSX.Element {
  const { actions } = useApp();
  const { savedSearches } = useMailState();
  const [draft, setDraft] = useState<Draft | null>(null);
  const [problem, setProblem] = useState<string | null>(null);

  // Same ordering as the sidebar, so the list a user arranges here is the list they
  // see there.
  const list = useMemo(
    () =>
      Object.values(savedSearches).sort(
        (a, b) => a.position - b.position || a.name.localeCompare(b.name),
      ),
    [savedSearches],
  );

  async function save(): Promise<void> {
    if (!draft) return;
    if (draft.name.trim() === "") {
      setProblem(t("savedSearch.nameRequired"));
      return;
    }
    if (draft.query.trim() === "") {
      setProblem(t("savedSearch.queryRequired"));
      return;
    }
    setProblem(null);
    const ok = await actions.saveSavedSearch({
      ...(draft.id ? { id: draft.id } : {}),
      name: draft.name.trim(),
      query: draft.query.trim(),
      inSidebar: draft.inSidebar,
      position: draft.position,
    });
    if (ok) setDraft(null);
  }

  function toDraft(s: SavedSearch): Draft {
    return {
      id: s.id,
      name: s.name,
      query: s.query,
      inSidebar: s.inSidebar,
      position: s.position,
    };
  }

  return (
    <>
      {draft ? (
        <div className="snippet-editor">
          <label className="field">
            <span className="field-label">{t("snippets.name")}</span>
            <input
              className="text-input"
              type="text"
              value={draft.name}
              autoFocus
              onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            />
          </label>

          <label className="field">
            <span className="field-label">{t("savedSearch.query")}</span>
            <input
              className="text-input"
              type="text"
              value={draft.query}
              onChange={(e) => setDraft({ ...draft, query: e.target.value })}
            />
            <span className="field-hint">{t("search.help")}</span>
          </label>

          <label className="field field-inline">
            <input
              type="checkbox"
              checked={draft.inSidebar}
              onChange={(e) => setDraft({ ...draft, inSidebar: e.target.checked })}
            />
            <span className="field-label">{t("search.pinToSidebar")}</span>
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
          <p className="field-hint">{t("savedSearch.explain")}</p>
          {list.length === 0 ? (
            <p className="field-hint">{t("savedSearch.none")}</p>
          ) : (
            <ul className="remote-allow-list">
              {list.map((s) => (
                <li key={s.id} className="remote-allow">
                  <span className="remote-allow-value">{s.name}</span>
                  {/* The QUERY is shown, not hidden behind an edit click: a folder
                      whose contents you cannot explain is one you stop trusting. */}
                  <span className="remote-allow-kind">{s.query}</span>
                  {!s.inSidebar ? (
                    <span className="remote-allow-kind">{t("savedSearch.notPinned")}</span>
                  ) : null}
                  <button
                    type="button"
                    className="button-quiet"
                    onClick={() => setDraft(toDraft(s))}
                  >
                    {t("action.edit")}
                  </button>
                  <button
                    type="button"
                    className="button-quiet"
                    onClick={() => {
                      // Confirmed because there is no undo: the query would have to
                      // be retyped from memory.
                      if (window.confirm(t("savedSearch.deleteConfirm", { name: s.name }))) {
                        void actions.deleteSavedSearch(s.id);
                      }
                    }}
                  >
                    {t("action.delete")}
                  </button>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </>
  );
}
