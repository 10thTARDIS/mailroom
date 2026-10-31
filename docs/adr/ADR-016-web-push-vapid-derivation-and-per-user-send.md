# ADR-016: Web Push — VAPID key derived from the master key; sends authorized per-user via RLS

**Status:** Accepted (design for Web Push)

## Context

Web Push (RFC 8030 + VAPID RFC 8292 + payload encryption RFC 8291) needs two
things the rest of the system doesn't have a precedent-free answer for:

1. **A VAPID application keypair.** One P-256 keypair for the whole server: its
   public key is handed to every browser at `pushManager.subscribe` time and is
   baked into every stored subscription; its private key signs every push. If it
   changes, **every existing subscription is invalidated**, so it must be stable
   and survive restarts/redeploys. It is an *application* secret, not per-user,
   and doesn't fit the per-user derived-key model directly.

2. **Sending pushes from a session-less context.** The push sender runs inside
   the sync loop — there is no OIDC session and no `app.current_user_id()`. But
   `app.push_subscriptions` is under `FORCE ROW LEVEL SECURITY` keyed on
   `current_user_id()`, so a naive session-less read sees nothing (fail-closed).

## Decision

### VAPID keypair is HKDF-derived from the master key (extends ADR-013)

The VAPID private scalar is:

```
vapid_scalar = crypto.DeriveKey(masterKey, salt=nil, info="mailroom-vapid-v1")  // 32 bytes
```

— the exact HKDF-SHA256 helper (`internal/crypto/keys.go`) the credentials key,
mail-blob DEK, and blob-path key already use, with **`salt=nil`** because this is
an application-level key (same shape as the blob-path key), not per-user. The
32-byte output is a P-256 private scalar; the public key and an `ecdsa.PrivateKey`
for ES256 JWT signing are computed from it (`crypto/ecdh` / `crypto/ecdsa`).

Consequences:
- **No new secret to store, escrow, or rotate.** The keypair is reconstructible
  from the master key, exactly like every other derived key — losing the master
  key already makes everything unrecoverable, and this adds nothing new to escrow.
- **Stable by construction.** Same master key ⇒ same VAPID keypair across
  restarts and redeploys ⇒ existing subscriptions keep working.
- **Rotation = master-key rotation** (out of scope here), which would invalidate
  subscriptions — acceptable and expected, and no worse than the wrapped-key
  story ADR-013 already defers.

### Sends are authorized per-user through a `UserTx`, not an RLS bypass

The push sender is invoked **per-user from the sync fan-out**, which already
carries the `userID` at every layer (Supervisor → Runner → `syncAccount`), proven
by the `HealthRecorder`. A `PushNotifier` seam (a direct clone of
`WithHealthRecorder`) fires from `syncAccount`'s success tail when
`MessagesIngested > 0`, and its store implementation opens
`Store.UserTx(ctx, userID, …)` to read that user's `push_subscriptions` under
RLS — `current_user_id()` is set to exactly that user.

Consequences:
- **No `SECURITY DEFINER` function and no `BYPASSRLS`** for push. The load-bearing
  "no identity, no rows" rule holds: the sender only ever sees the subscriptions
  of the user whose sync pass it is running in.
- A dead endpoint (HTTP 404/410 from the push service) is marked
  `push_subscriptions.expired_at` in the same per-user tx.

## Scope notes

- **Triage-aware ("important-only") is deferred to its signal.** No per-message
  importance exists before the ML tier; `push_subscriptions.important_only`
  is future-proofing. For now a push fires on any new **inbox** mail.

  ⚠ **Correction, 2026-09-18:** that last sentence was never what the code does.
  `Runner.notifyPush` (`internal/syncrunner/runner.go`) fires once per account,
  per sync pass, whenever that pass's `MessagesIngested > 0` — i.e. any new mail
  ingested into *any* mailbox the account syncs, not only Inbox. Left as a note
  rather than rewritten, per this project's practice for a decision record.

  **Amended, 2026-09-19 (v1.76.0):** the count pushed is now `MessagesIngested`
  minus `MessagesMuted` — new mail that arrived in a muted conversation and was
  archived on arrival does not notify (`pushCount`, same file). A pass that
  brought only muted replies sends no push at all.

  **Amended, 2026-10-17 (v1.89.0): triage-aware push is built, and its
  default deliberately deviates from spec §12.** The spec says "only
  *important* mail pushes by default". Decided:
  push stays **every message until the user picks a mode**, because a
  silent change from "everything" to "only what the model ranks high" on the
  day of an upgrade would look like lost mail. So:

  - `push_subscriptions.notify_mode` (migration 00061) is per device:
    `all` (default), `above_line`, `vip`, `above_line_or_vip`. It replaces
    `important_only`, which is **not** migrated into the mode: every existing
    row held `true` only as the column default, never as a choice.
  - "Above the line" is the desk's own rule (`lineSideSQL` against
    `DeskPreferences.lineThreshold`), plus the `$important` keyword the "Mark
    important" rule action writes — counted for push only (`store.PushCounts`),
    not folded into the desk's split.
  - Quiet hours (per user, IANA zone, wall-clock arithmetic) let only VIP mail
    through, and only while "Allow VIPs during quiet hours" is on.
  - `Account.notifyPush = false` silences one mail account on every device.
  - Mail from a blocked sender is left out of the count, like muted mail.

  Decision logic lives in `internal/notify` (pure, fake-clock tested); the
  sender (`StorePushNotifier`) asks the store for a breakdown only when some
  device's mode or quiet hours need one, so the default costs what it did.
- **RFC 8291 payload encryption (aes128gcm) is hand-rolled on stdlib**
  (`crypto/ecdh` + HKDF + AES-128-GCM), consistent with how the project already
  owns its crypto, and **tested against the RFC 8291 published test vectors** so
  correctness is checked, not assumed. The VAPID ES256 JWT uses `go-jose`
  (already vendored; promoted from indirect to direct require).
- **CSP gains `worker-src 'self'`** for the service worker; the SW registers from
  an external module (never an inline script), preserving `script-src 'self'` and
  the no-inline-script invariant.
