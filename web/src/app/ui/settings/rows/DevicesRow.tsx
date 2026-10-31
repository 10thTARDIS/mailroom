// SPDX-License-Identifier: AGPL-3.0-only

// SIGNED-IN DEVICES (#937, ADR-048 section 6).
//
// The row has the API keys row's states (rows/ApiKeysRow.tsx), for the same
// reason: `urn:mailroom:devices` may be absent, and a server that cannot be
// asked has not said you have no sessions.
//
//   capability absent → "not available on this server"
//   still asking      → no facts
//   asked             → this device first, the others by last activity
//
// Sessions are not JMAP-synced objects: the list is read when the row opens
// and again after every sign-out made here.
//
// THIS DEVICE'S BUTTON IS NOT THE JMAP DESTROY. It goes to the ordinary logout
// route, so the provider logout and the cookie clearing happen exactly as for
// the normal sign-out; the destroy is for the other devices.

import { useCallback, useEffect, useState } from "react";
import type { JSX } from "react";

import type { SignedInDevice } from "../../../../core/jmap/types";
import { keyDateLong } from "../../../../core/userKeys";
import { t, tCount } from "../../../i18n";
import type { StateFact } from "../SettingsRow";
import type { DevicesApi } from "../devices/api";
import { lastActiveText, sortDevices } from "../devices/model";

export type DevicesReading =
  | { kind: "unavailable" }
  | { kind: "asking" }
  | { kind: "loaded"; devices: SignedInDevice[] };

/** Reads the devices once the session has landed (KeysRow's gate), and on reload(). */
export function useDevices(api: DevicesApi, sessionReady: boolean): { reading: DevicesReading; reload: () => void } {
  const [reading, setReading] = useState<DevicesReading>({ kind: "asking" });
  const [generation, setGeneration] = useState(0);
  useEffect(() => {
    if (!sessionReady) return;
    if (api.capability() === null) {
      setReading({ kind: "unavailable" });
      return;
    }
    let cancelled = false;
    void api.list().then((devices) => {
      if (cancelled) return;
      // A failed re-read after a change keeps what is on screen rather than
      // claiming the server went away; only the first read can be unavailable.
      setReading((prev) =>
        devices === null ? (prev.kind === "loaded" ? prev : { kind: "unavailable" }) : { kind: "loaded", devices },
      );
    });
    return () => {
      cancelled = true;
    };
  }, [api, sessionReady, generation]);
  const reload = useCallback(() => setGeneration((g) => g + 1), []);
  return { reading, reload };
}

/** The state line: `3 signed-in devices`. */
export function devicesFacts(reading: DevicesReading): StateFact[] {
  if (reading.kind === "unavailable") return [{ text: t("devices.unavailable") }];
  if (reading.kind === "asking") return [];
  if (reading.devices.length === 0) return [{ text: t("devices.state.none") }];
  return [{ text: tCount("devices.state.count", reading.devices.length) }];
}

export function DevicesBody({
  reading,
  reload,
  api,
  notify,
  now,
}: {
  reading: DevicesReading;
  reload: () => void;
  api: DevicesApi;
  notify: (toast: { kind: "info" | "error"; message: string }) => void;
  /** The clock, injectable for tests. */
  now?: () => number;
}): JSX.Element {
  const [busy, setBusy] = useState(false);

  if (reading.kind === "unavailable") return <p className="field-hint">{t("devices.unavailableLong")}</p>;
  if (reading.kind === "asking") return <p className="field-hint">{t("devices.asking")}</p>;
  if (reading.devices.length === 0) return <p className="field-hint devices-none">{t("devices.none")}</p>;

  const devices = sortDevices(reading.devices);
  const others = devices.filter((d) => !d.isCurrent);
  const clock = (now ?? Date.now)();

  const run = async (ids: string[] | "others", name: string | null): Promise<void> => {
    setBusy(true);
    try {
      const res = await api.signOut(ids);
      if (res === null || (res.destroyed.length === 0 && res.notDestroyed.length > 0)) {
        notify({ kind: "error", message: t("devices.failed") });
        return;
      }
      notify({
        kind: "info",
        message:
          name !== null && res.destroyed.length <= 1
            ? t("devices.done.one", { name })
            : tCount("devices.done.count", res.destroyed.length),
      });
      reload();
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="devices-page">
      <ul className="own-key-list device-list">
        {devices.map((d) => (
          <li key={d.id} className="own-key device-item">
            <p className="own-key-heading">
              {d.description}
              {d.isCurrent ? (
                <>
                  {" "}
                  <span className="device-tag">{t("devices.thisDevice")}</span>
                </>
              ) : null}
            </p>
            <p className="own-key-facts">
              {t("devices.lastActive", { when: lastActiveText(d.lastSeenAt, clock) })}
              {" · "}
              {t("devices.signedIn", { when: keyDateLong(d.createdAt) })}
            </p>
            {d.lastAddr ? <p className="own-key-facts field-hint">{t("devices.from", { addr: d.lastAddr })}</p> : null}
            <div className="own-key-actions">
              {d.isCurrent ? (
                <button type="button" className="button" disabled={busy} onClick={() => api.signOutHere()}>
                  {t("devices.signOutHere")}
                </button>
              ) : (
                <button
                  type="button"
                  className="button"
                  disabled={busy}
                  aria-label={t("devices.signOutFor", { name: d.description })}
                  onClick={() => void run([d.id], d.description)}
                >
                  {t("devices.signOut")}
                </button>
              )}
            </div>
          </li>
        ))}
      </ul>
      {others.length > 0 ? (
        <div className="own-key-actions">
          <button
            type="button"
            className="button"
            disabled={busy}
            onClick={() => {
              if (window.confirm(tCount("devices.confirmOthers", others.length))) void run("others", null);
            }}
          >
            {t("devices.signOutOthers")}
          </button>
        </div>
      ) : null}
      <p className="field-hint">{t("devices.honest")}</p>
      <p className="field-hint">{t("devices.pushNote")}</p>
    </div>
  );
}
