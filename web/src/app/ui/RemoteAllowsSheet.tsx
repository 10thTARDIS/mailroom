// SPDX-License-Identifier: AGPL-3.0-only

// THE SENDERS WHOSE IMAGES YOU ALWAYS LOAD (#202), and the button that stops it.
//
// THIS SURFACE IS NOT OPTIONAL, and it is worth saying why in the file that
// implements it. Remote images are blocked by default, and the affordance that
// loads them used to be per-message and non-persistent on the reasoning that "I
// trusted this one newsletter once" must not become "this sender may track me
// forever". Remembering the decision is what #202 added — and a remembered decision
// that a user cannot SEE or TAKE BACK is precisely the permanent tracking consent
// that reasoning was guarding against, merely moved out of a click and into a
// database. The list is the other half of the feature, not a nicety attached to it.
//
// So the design goals here are narrow: show every grant, say how broad each one is,
// and revoke in one click. There is deliberately nothing else — no editing (a
// changed grant is a different decision), no "allow all", no bulk toggle.
//
// IT IS A ROW BODY NOW, NOT A SHEET (#374 increment 2). This is the whole of the
// "Reading and safety" row of the settings page, so the dialog furniture — the
// backdrop, the ✕, its own Escape handler — is gone: the row's heading is what
// opens it and Escape peels the settings route (app/routing.ts). Nothing about
// WHAT it shows changed. The filename is kept so the history of the surface
// stays in one place.

import { useMemo } from "react";
import type { JSX } from "react";

import { useApp, useMailState } from "../state";
import { t } from "../i18n";

export function RemoteAllowsBody(): JSX.Element {
  const { actions } = useApp();
  const { remoteAllows } = useMailState();

  // Domains first, then addresses, each alphabetically. Domains lead because they
  // are the BROADER grants: if someone is reviewing this list to tighten up, the
  // ones worth reconsidering first should be at the top rather than mixed in.
  const list = useMemo(() => {
    const all = Object.values(remoteAllows);
    return all.sort((a, b) => {
      if (a.kind !== b.kind) return a.kind === "domain" ? -1 : 1;
      return a.value.localeCompare(b.value);
    });
  }, [remoteAllows]);

  return (
    <>
      {/* The list has its own heading inside the row: the row is "Reading and
          safety", and the remembered senders are one of the things it holds. */}
      <h4 className="sheet-section">{t("remoteAllows.title")}</h4>
      <p className="field-hint">{t("remoteAllows.explain")}</p>

      {list.length === 0 ? (
        // The empty state is a GOOD state here, and says so: nothing remembered
        // means every message still asks, which is the safe default rather than a
        // feature that has not been set up.
        <p className="field-hint">{t("remoteAllows.empty")}</p>
      ) : (
        <ul className="remote-allow-list">
          {list.map((a) => (
            <li key={a.id} className="remote-allow">
              <span className="remote-allow-value">
                {a.kind === "domain" ? t("remoteAllows.anyoneAt", { domain: a.value }) : a.value}
              </span>
              {/* The breadth of a domain grant is restated here, not only at the
                  point of granting: this list is where someone comes back weeks
                  later, having forgotten which kind they picked. */}
              {a.kind === "domain" ? (
                <span className="remote-allow-kind">{t("remoteAllows.broad")}</span>
              ) : null}
              <button
                type="button"
                className="button-quiet"
                onClick={() => void actions.revokeRemoteContent(a.id)}
              >
                {t("remoteAllows.revoke")}
              </button>
            </li>
          ))}
        </ul>
      )}
    </>
  );
}
