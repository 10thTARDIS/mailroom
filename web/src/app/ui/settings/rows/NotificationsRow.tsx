// SPDX-License-Identifier: AGPL-3.0-only

// NOTIFICATIONS (#374 increment 2) — this browser's Web Push subscription (#42),
// and since #59 the three choices about what it says: this device's mode, quiet
// hours, and which mail accounts notify (./NotificationControls.tsx).
//
// THE STATE IS READ ONCE PER PAGE, and the hook is separate from the body for the
// reason the Appearance row's is: the state LINE says `push on` and the body's
// button says `Turn off notifications`, and those are two renderings of one fact.
// Two independent reads would show a stale line beside a fresh button for the
// length of a round trip.
//
// NULL IS NOT "OFF". The read is asynchronous — the session has to be connected
// first — and until it resolves this row states NOTHING about push rather than
// guessing the safe-looking answer. "Off" is a claim that a user's notifications
// are not arriving; making it before asking is how a settings page ends up
// inventing an outage (ui/useAccounts.ts makes the same call about "paused").
//
// QUIET HOURS ARE A SETTING NOW (#59). Until then this row said out loud that
// they were missing — the page's rule that an absent feature is stated rather
// than implied by its absence. The sentence went when the control arrived.

import { useEffect, useState } from "react";
import type { JSX } from "react";

import { t } from "../../../i18n";
import type { MessageKey } from "../../../i18n";
import { disablePush, enablePush, readPushStatus } from "../../../push";
import type { PushStatus } from "../../../push";
import { useApp } from "../../../state";
import { AccountPushControl, DeviceModeControl, QuietHoursControl } from "./NotificationControls";

export interface PushApi {
  /** null until the async read resolves — see the header. */
  status: PushStatus | null;
  busy: boolean;
  enable: () => void;
  disable: () => void;
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** The full sentence under the control for each state. */
export function pushDescriptionKey(status: PushStatus): MessageKey {
  switch (status) {
    case "on":
      return "settings.notifications.on";
    case "off":
      return "settings.notifications.off";
    case "blocked":
      return "settings.notifications.blocked";
    case "unavailable":
      return "settings.notifications.unavailable";
    case "unsupported":
    default:
      return "settings.notifications.unsupported";
  }
}

/** The state line's version: three words, not a sentence. `push on` / `push off`
 * / `blocked`, plus the two states that are about the browser or the server
 * rather than about a choice. */
export function pushStateKey(status: PushStatus): MessageKey {
  switch (status) {
    case "on":
      return "settings.state.pushOn";
    case "off":
      return "settings.state.pushOff";
    case "blocked":
      return "settings.state.pushBlocked";
    case "unavailable":
      return "settings.state.pushUnavailable";
    case "unsupported":
    default:
      return "settings.state.pushUnsupported";
  }
}

export function usePushStatus(): PushApi {
  const { client, pushToast } = useApp();
  const [status, setStatus] = useState<PushStatus | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    void readPushStatus(client).then((s) => {
      if (!cancelled) setStatus(s);
    });
    return () => {
      cancelled = true;
    };
  }, [client]);

  const enable = (): void => {
    setBusy(true);
    void (async () => {
      try {
        const s = await enablePush(client);
        setStatus(s);
        if (s === "on") pushToast({ kind: "info", message: t("settings.notifications.enabled") });
        else if (s === "blocked")
          pushToast({ kind: "error", message: t("settings.notifications.blocked") });
      } catch (err) {
        pushToast({
          kind: "error",
          message: t("settings.notifications.error", { error: errorMessage(err) }),
        });
      } finally {
        setBusy(false);
      }
    })();
  };

  const disable = (): void => {
    setBusy(true);
    void (async () => {
      try {
        const s = await disablePush(client);
        setStatus(s);
        pushToast({ kind: "info", message: t("settings.notifications.disabled") });
      } catch (err) {
        pushToast({
          kind: "error",
          message: t("settings.notifications.error", { error: errorMessage(err) }),
        });
      } finally {
        setBusy(false);
      }
    })();
  };

  return { status, busy, enable, disable };
}

export function NotificationsBody({ api }: { api: PushApi }): JSX.Element {
  const { status, busy, enable, disable } = api;
  return (
    <>
      <p className="menu-note" role="status">
        {status === null
          ? t("settings.notifications.working")
          : t(pushDescriptionKey(status))}
      </p>
      {status === "off" ? (
        <button
          type="button"
          className="button button-quiet menu-action"
          disabled={busy}
          onClick={enable}
        >
          {busy ? t("settings.notifications.working") : t("settings.notifications.enable")}
        </button>
      ) : status === "on" ? (
        <button
          type="button"
          className="button button-quiet menu-action"
          disabled={busy}
          onClick={disable}
        >
          {busy ? t("settings.notifications.working") : t("settings.notifications.disable")}
        </button>
      ) : null}
      <DeviceModeControl status={status} />
      <QuietHoursControl />
      <AccountPushControl />
    </>
  );
}
