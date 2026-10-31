// SPDX-License-Identifier: AGPL-3.0-only

// Browser-side Web Push orchestration (Forgejo #42, ADR-016).
//
// This lives in src/app — NOT core/ — for the same reason auth.ts does: it is
// the one place that touches browser-only push APIs (ServiceWorkerRegistration,
// PushManager, Notification), which core/ is contractually DOM-free about so a
// React Native / Tauri shell can import core/ unmodified (spec §12). The JMAP
// wire (WebPushSubscription/set|get, the VAPID key getter) stays in the DOM-free
// core/jmap/client.ts; this module drives the browser half and calls into it.
//
// The flow (ADR-016 / THE CONTRACT):
//   1. feature-detect the browser AND read the server's VAPID key from the
//      session capability urn:mailroom:push — either missing ⇒ we can't offer it;
//   2. request Notification permission (denied ⇒ "blocked", the user must undo
//      it in browser settings — we can never re-prompt);
//   3. pushManager.subscribe({ userVisibleOnly: true, applicationServerKey });
//   4. register the resulting endpoint + keys with WebPushSubscription/set create,
//      and remember the server-assigned id locally so we can destroy it later.
// Disable is the mirror: unsubscribe() in the browser, then destroy server-side.

import type { JmapClient } from "../core/jmap/client";
import type { PushMode } from "../core/jmap/types";

/** Where in the browser we stash the SERVER's subscription id (not a secret —
 * the keys never touch storage), so a later "turn off" can destroy the right row. */
const SUB_ID_KEY = "mailroom.push.subscriptionId";

/** Where we remember THIS device's chosen push mode (#747). Turning notifications
 * off destroys the server row, and the mode lived only on that row; keeping the
 * choice here lets the next enable send it with the new registration. Client-side
 * because the mode is per device and the server needs no new state for it. */
const MODE_KEY = "mailroom.push.notifyMode";

const PUSH_MODES: ReadonlyArray<PushMode> = ["all", "above_line", "vip", "above_line_or_vip"];

/** The remembered mode, or undefined when none (or a value we no longer know). */
function rememberedMode(): PushMode | undefined {
  const m = localStorage.getItem(MODE_KEY);
  return PUSH_MODES.find((x) => x === m);
}

/** The clientId used in the WebPushSubscription/set create map. Stable so a
 * re-enable on the same device reuses the same create slot. */
const CREATE_CLIENT_ID = "web";

/**
 * The user-facing state of push on THIS device:
 *   unsupported — this browser lacks service workers / PushManager / Notification;
 *   unavailable — the server does not offer Web Push (no VAPID key in session);
 *   blocked     — the user denied notification permission (only they can undo it);
 *   on          — subscribed here and registered with the server;
 *   off         — supported and permittable, just not subscribed yet.
 */
export type PushStatus = "unsupported" | "unavailable" | "blocked" | "on" | "off";

/** True when this browser has every API the push flow needs. */
export function pushBrowserSupported(): boolean {
  return (
    typeof navigator !== "undefined" &&
    "serviceWorker" in navigator &&
    typeof window !== "undefined" &&
    "PushManager" in window &&
    "Notification" in window
  );
}

/** base64url (unpadded) → Uint8Array, for the VAPID applicationServerKey that
 * pushManager.subscribe wants as raw bytes. Pure; no DOM beyond atob.
 *
 * The backing ArrayBuffer is allocated EXPLICITLY rather than letting
 * `new Uint8Array(length)` conjure one, and the return type is deliberately
 * inferred rather than annotated. From TypeScript 5.7 the typed arrays are
 * generic over their buffer, defaulting to `Uint8Array<ArrayBufferLike>`, while
 * the DOM's BufferSource narrowed to `ArrayBufferView<ArrayBuffer>`. Since
 * ArrayBufferLike admits SharedArrayBuffer, both the old `new Uint8Array(n)`
 * AND a bare `: Uint8Array` annotation resolve to a type `applicationServerKey`
 * no longer accepts. Naming the ArrayBuffer pins the view to a non-shared
 * buffer — which is what this always was — and letting the return type infer
 * carries that through to the caller.
 *
 * Written this way, rather than as `Uint8Array<ArrayBuffer>`, so it stays
 * compatible with TypeScript before 5.7, where the type is not yet generic. */
export function urlBase64ToUint8Array(base64url: string) {
  const padding = "=".repeat((4 - (base64url.length % 4)) % 4);
  const base64 = (base64url + padding).replace(/-/g, "+").replace(/_/g, "/");
  const raw = atob(base64);
  const out = new Uint8Array(new ArrayBuffer(raw.length));
  for (let i = 0; i < raw.length; i += 1) out[i] = raw.charCodeAt(i);
  return out;
}

/** ArrayBuffer → base64url (unpadded), for encoding the PushSubscription's
 * p256dh/auth keys the way the server (and RFC 8291) expect. Null buffer → "". */
function bufferToUrlBase64(buf: ArrayBuffer | null): string {
  if (!buf) return "";
  const bytes = new Uint8Array(buf);
  let binary = "";
  for (let i = 0; i < bytes.length; i += 1) binary += String.fromCharCode(bytes[i] as number);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

/** A short, human hint for the WebPushSubscription/get device list ("Firefox",
 * "Chrome on macOS", …). Best-effort from the UA; never blocks the flow. */
function deviceName(): string {
  if (typeof navigator === "undefined") return "Web";
  const ua = navigator.userAgent || "";
  let browser = "Web";
  if (/Firefox\//.test(ua)) browser = "Firefox";
  else if (/Edg\//.test(ua)) browser = "Edge";
  else if (/Chrome\//.test(ua)) browser = "Chrome";
  else if (/Safari\//.test(ua)) browser = "Safari";
  let os = "";
  if (/Windows/.test(ua)) os = "Windows";
  else if (/Mac OS X/.test(ua)) os = "macOS";
  else if (/Android/.test(ua)) os = "Android";
  else if (/Linux/.test(ua)) os = "Linux";
  return os ? `${browser} on ${os}` : browser;
}

/** Reads the current push state without changing anything. Safe to call on
 * demand (e.g. when the settings menu opens). Requires a connected client so the
 * VAPID capability is readable; before connect it reports "unavailable". */
export async function readPushStatus(client: JmapClient): Promise<PushStatus> {
  if (!pushBrowserSupported()) return "unsupported";
  if (!client.vapidPublicKey()) return "unavailable";
  if (Notification.permission === "denied") return "blocked";
  const reg = await navigator.serviceWorker.getRegistration();
  const sub = reg ? await reg.pushManager.getSubscription() : null;
  return sub ? "on" : "off";
}

/**
 * Turns push ON for this device: permission → subscribe → register server-side.
 * Returns the resulting status. Throws only on an unexpected failure (network,
 * server refusal) — the graceful "can't" outcomes (unsupported / unavailable /
 * blocked) come back as a status, not an exception, so the UI can explain them.
 */
export async function enablePush(client: JmapClient): Promise<PushStatus> {
  if (!pushBrowserSupported()) return "unsupported";
  const vapid = client.vapidPublicKey();
  if (!vapid) return "unavailable";

  const permission = await Notification.requestPermission();
  if (permission !== "granted") {
    return permission === "denied" ? "blocked" : "off";
  }

  const reg = await navigator.serviceWorker.ready;
  // Reuse an existing browser subscription rather than churning a new one.
  let sub = await reg.pushManager.getSubscription();
  if (!sub) {
    sub = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(vapid),
    });
  }

  await registerSubscription(client, sub, true);
  return "on";
}

/**
 * Registers a browser push subscription with the server (WebPushSubscription/set
 * create) and remembers the server's id. Shared by enablePush and the
 * reconcile-on-connect below, which is why the rollback is a parameter: enable
 * made the browser subscription a moment ago, so a server refusal takes it back
 * (no live endpoint that no server row will ever send to); reconcile found it
 * already there, and a refused or failed re-registration must not destroy it.
 */
async function registerSubscription(client: JmapClient, sub: PushSubscription, rollback: boolean): Promise<void> {
  const p256dh = bufferToUrlBase64(sub.getKey("p256dh"));
  const auth = bufferToUrlBase64(sub.getKey("auth"));
  if (!p256dh || !auth) {
    if (rollback) await sub.unsubscribe().catch(() => undefined);
    throw new Error("push: subscription is missing its encryption keys");
  }

  const res = await client.webPushSubscriptionSet({
    accountId: client.accountId,
    create: {
      [CREATE_CLIENT_ID]: {
        endpoint: sub.endpoint,
        keys: { p256dh, auth },
        deviceName: deviceName(),
        ...(rememberedMode() ? { notifyMode: rememberedMode() as PushMode } : {}),
      },
    },
  });

  const created = res.created?.[CREATE_CLIENT_ID];
  if (!created) {
    if (rollback) await sub.unsubscribe().catch(() => undefined);
    const why = res.notCreated?.[CREATE_CLIENT_ID]?.description;
    throw new Error(why ? `push: ${why}` : "push: the server did not register this device");
  }

  localStorage.setItem(SUB_ID_KEY, created.id);
}

let reconciled = false;

/** Test seam: lets a test run the once-per-load reconcile again. */
export function resetPushReconcileForTests(): void {
  reconciled = false;
}

/**
 * PUSH RECONCILIATION ON CONNECT (#937, ADR-048 section 6).
 *
 * Sign-out, a replaced session, a refused renewal and the session sweep all
 * delete the server-side push subscriptions of the session that ended. The
 * Settings toggle is read from the BROWSER alone (readPushStatus), so without
 * this it would keep showing "on" after signing out and back in while nothing
 * arrives: it would have run and changed nothing.
 *
 * So once per page load, after the session is established: if this browser
 * already holds a push subscription and notification permission is already
 * granted, ask the server whether it still lists it, and register it again if
 * not. WebPushSubscription/get never returns endpoints (they are write-only),
 * so "lists it" means the server id remembered at enable time; a browser with
 * no remembered id re-registers, which is harmless because the server upserts
 * on (user, endpoint).
 *
 * NEVER PROMPTS. It does not call Notification.requestPermission(); with
 * permission not granted, or no browser subscription, it does nothing. Failures
 * are silent apart from a console.debug.
 */
export async function reconcilePush(client: JmapClient): Promise<void> {
  if (reconciled) return;
  reconciled = true;
  try {
    if (!pushBrowserSupported()) return;
    if (!client.vapidPublicKey()) return;
    if (Notification.permission !== "granted") return;
    const reg = await navigator.serviceWorker.getRegistration();
    const sub = reg ? await reg.pushManager.getSubscription() : null;
    if (!sub) return;
    const id = localStorage.getItem(SUB_ID_KEY);
    if (id) {
      const res = await client.webPushSubscriptionGet(client.accountId);
      if ((res.list ?? []).some((x) => x.id === id)) return;
    }
    await registerSubscription(client, sub, false);
  } catch (err) {
    console.debug("push: reconcile failed", err);
  }
}

/**
 * THIS DEVICE'S PUSH MODE (#59). The server keeps one mode per registration,
 * and the registration this browser made is the id stored at enable time —
 * so "this device" is that id, read back through WebPushSubscription/get.
 *
 * null means there is no registration to read (push is off here, or was turned
 * on before this browser remembered the id); the caller shows no mode rather
 * than guess one. A server before #59 sends no mode, which is "all": that is
 * what it does.
 */
export async function readDeviceMode(client: JmapClient): Promise<{ id: string; mode: PushMode } | null> {
  const id = localStorage.getItem(SUB_ID_KEY);
  if (!id) return null;
  const res = await client.webPushSubscriptionGet(client.accountId);
  const sub = (res.list ?? []).find((s) => s.id === id);
  if (!sub) return null;
  const mode = sub.notifyMode ?? "all";
  localStorage.setItem(MODE_KEY, mode);
  return { id, mode };
}

/** Sets this device's push mode. Resolves true only when the server said so. */
export async function setDeviceMode(client: JmapClient, id: string, mode: PushMode): Promise<boolean> {
  const res = await client.webPushSubscriptionSet({
    accountId: client.accountId,
    update: { [id]: { notifyMode: mode } },
  });
  const ok = Boolean(res.updated && id in res.updated);
  if (ok) localStorage.setItem(MODE_KEY, mode);
  return ok;
}

/**
 * Turns push OFF for this device: unsubscribe in the browser, then destroy the
 * server row. Best-effort and idempotent — a partial failure still leaves us in
 * a sane "off" state, and both halves tolerate the other already being gone.
 * The device's chosen mode is deliberately KEPT (MODE_KEY), so turning
 * notifications back on restores it (#747).
 */
export async function disablePush(client: JmapClient): Promise<PushStatus> {
  const reg = pushBrowserSupported() ? await navigator.serviceWorker.getRegistration() : null;
  const sub = reg ? await reg.pushManager.getSubscription() : null;
  if (sub) await sub.unsubscribe().catch(() => undefined);

  const id = localStorage.getItem(SUB_ID_KEY);
  if (id) {
    await client
      .webPushSubscriptionSet({ accountId: client.accountId, destroy: [id] })
      .catch(() => undefined);
    localStorage.removeItem(SUB_ID_KEY);
  }
  return "off";
}
